package tasks

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// AgentPool is a named, ordered set of agents scoped to one queue.
type AgentPool struct {
	ID        int64    `json:"id"`
	Queue     string   `json:"queue"`
	Name      string   `json:"name"`
	Agents    []string `json:"agents"`
	Revision  int64    `json:"revision"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

func (s *Service) requireWorkflowAdmin(actor Actor) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	if !actor.IsCustomer || actor.Principal != userPrincipal(s.customer) {
		return domainError(http.StatusForbidden, "forbidden", "only the daemon customer can administer workflows")
	}
	return nil
}

func (s *Service) RebindAgentPool(
	ctx context.Context,
	actor Actor,
	queue string,
	poolName string,
	agents []string,
	revision int64,
	idempotencyKey string,
) (AgentPool, error) {
	if err := s.requireWorkflowAdmin(actor); err != nil {
		return AgentPool{}, err
	}
	queue = strings.ToUpper(strings.TrimSpace(queue))
	poolName = strings.TrimSpace(poolName)
	if poolName == "" {
		return AgentPool{}, domainError(http.StatusBadRequest, "missing_pool", "agent pool name is required")
	}
	agents = normalizePoolAgents(agents)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentPool{}, err
	}
	defer tx.Rollback()
	if replayed, ok, err := readTaskIdempotency[AgentPool](
		ctx, tx, actor.Principal, "rebind_agent_pool", idempotencyKey,
	); err != nil {
		return AgentPool{}, err
	} else if ok {
		return replayed, nil
	}
	if err := requireQueueExists(ctx, tx, queue); err != nil {
		return AgentPool{}, err
	}
	if err := requireAgentsExist(ctx, tx, agents); err != nil {
		return AgentPool{}, err
	}
	if len(agents) == 0 {
		if err := requirePoolUnusedByBinding(ctx, tx, queue, poolName); err != nil {
			return AgentPool{}, err
		}
	}
	current, found, err := agentPoolByName(ctx, tx, queue, poolName)
	if err != nil {
		return AgentPool{}, err
	}
	now := s.now()
	var pool AgentPool
	if found {
		if revision <= 0 || revision != current.Revision {
			return AgentPool{}, poolRevisionConflict(current)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE task_agent_pools SET revision = revision + 1, updated_at = ?
			WHERE id = ? AND revision = ?`, now, current.ID, revision)
		if err != nil {
			return AgentPool{}, err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return AgentPool{}, err
		} else if affected != 1 {
			fresh, _, loadErr := agentPoolByName(ctx, tx, queue, poolName)
			if loadErr != nil {
				return AgentPool{}, loadErr
			}
			return AgentPool{}, poolRevisionConflict(fresh)
		}
		pool = current
		pool.Revision++
		pool.UpdatedAt = now
		pool.Agents = agents
	} else {
		if revision != 0 {
			return AgentPool{}, poolRevisionConflict(AgentPool{Queue: queue, Name: poolName})
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO task_agent_pools(queue_prefix, name, revision, created_at, updated_at)
			VALUES (?, ?, 1, ?, ?)`, queue, poolName, now, now)
		if err != nil {
			return AgentPool{}, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return AgentPool{}, err
		}
		pool = AgentPool{
			ID: id, Queue: queue, Name: poolName, Agents: agents,
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_agent_pool_members WHERE pool_id = ?`, pool.ID); err != nil {
		return AgentPool{}, err
	}
	for position, agent := range agents {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_agent_pool_members(pool_id, agent, position)
			VALUES (?, ?, ?)`, pool.ID, agent, position); err != nil {
			return AgentPool{}, err
		}
	}
	if _, err := appendQueueEventTx(ctx, tx, Queue{Prefix: queue, Revision: pool.Revision},
		"task.agent_pool_rebound", actor, map[string]any{
			"pool": pool.Name, "agents": pool.Agents,
		}, now); err != nil {
		return AgentPool{}, err
	}
	if err := writeTaskIdempotency(
		ctx, tx, actor.Principal, "rebind_agent_pool", idempotencyKey, pool, now,
	); err != nil {
		return AgentPool{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentPool{}, err
	}
	s.signal()
	return pool, nil
}

func (s *Service) GetAgentPool(ctx context.Context, actor Actor, queue, pool string) (AgentPool, error) {
	if err := s.requireWorkflowAdmin(actor); err != nil {
		return AgentPool{}, err
	}
	item, found, err := agentPoolByName(ctx, s.db, strings.ToUpper(strings.TrimSpace(queue)), strings.TrimSpace(pool))
	if err != nil {
		return AgentPool{}, err
	}
	if !found {
		return AgentPool{}, domainError(http.StatusNotFound, "workflow_pool_not_found", "workflow agent pool not found")
	}
	return item, nil
}

func (s *Service) ListAgentPools(ctx context.Context, actor Actor, queue string) ([]AgentPool, error) {
	if err := s.requireWorkflowAdmin(actor); err != nil {
		return nil, err
	}
	queue = strings.ToUpper(strings.TrimSpace(queue))
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM task_agent_pools WHERE queue_prefix=? ORDER BY name`, queue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	items := make([]AgentPool, 0, len(names))
	for _, name := range names {
		item, _, err := agentPoolByName(ctx, s.db, queue, name)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// WorkflowResolver turns "name:tag" or a digest into a published digest.
type WorkflowResolver func(ref string) (digest string, err error)

// SetWorkflowResolver installs the resolver SetQueueWorkflow uses to turn a
// workflow ref into a digest. Without one, binding is unavailable.
func (s *Service) SetWorkflowResolver(resolve WorkflowResolver) { s.workflowResolver = resolve }

func queueWorkflowRevisionConflict(current int64) error {
	return &Error{
		Status: http.StatusConflict, Code: "revision_conflict",
		Msg:  "queue workflow binding was changed by another actor",
		Data: map[string]any{"current_revision": current},
	}
}

func errQueueWorkflowNotFound() error {
	return domainError(http.StatusNotFound, "queue_workflow_not_found", "queue has no workflow binding")
}

// queueWorkflowTx reads the binding of a queue; found is false when unbound.
func queueWorkflowTx(ctx context.Context, q queryer, queue string) (QueueWorkflow, bool, error) {
	var b QueueWorkflow
	err := q.QueryRowContext(ctx, `
		SELECT qw.queue_prefix, qw.workflow_digest, qw.revision, qw.updated_at, wi.name, wi.version
		FROM task_queue_workflows qw
		JOIN task_workflow_images wi ON wi.digest = qw.workflow_digest
		WHERE qw.queue_prefix = ?`, queue).Scan(&b.Queue, &b.Digest, &b.Revision, &b.UpdatedAt, &b.Name, &b.Version)
	if err == sql.ErrNoRows {
		return QueueWorkflow{}, false, nil
	}
	if err != nil {
		return QueueWorkflow{}, false, err
	}
	return b, true, nil
}

// emptyPools returns the sorted names among pools that are missing from the
// queue or have no member.
func emptyPools(ctx context.Context, q queryer, queue string, pools []string) ([]string, error) {
	missing := []string{}
	for _, name := range pools {
		var members int
		err := q.QueryRowContext(ctx, `
			SELECT COUNT(m.agent) FROM task_agent_pools p
			LEFT JOIN task_agent_pool_members m ON m.pool_id = p.id
			WHERE p.queue_prefix = ? AND p.name = ?
			GROUP BY p.id`, queue, name).Scan(&members)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if members == 0 {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// requirePoolUnusedByBinding rejects emptying a pool the queue's bound
// workflow uses.
func requirePoolUnusedByBinding(ctx context.Context, q queryer, queue, pool string) error {
	binding, bound, err := queueWorkflowTx(ctx, q, queue)
	if err != nil || !bound {
		return err
	}
	manifest, err := loadManifestTx(ctx, q, binding.Digest)
	if err != nil {
		return err
	}
	for _, used := range manifest.Definition.Pools() {
		if used == pool {
			return poolsEmptyError([]string{pool})
		}
	}
	return nil
}

func poolsEmptyError(pools []string) error {
	return &Error{
		Status: http.StatusConflict, Code: "workflow_pool_empty",
		Msg:  "workflow pools are missing or have no agents: " + strings.Join(pools, ", "),
		Data: map[string]any{"pools": pools},
	}
}

func (s *Service) resolveWorkflowRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", domainError(http.StatusBadRequest, "missing_workflow", "workflow reference is required")
	}
	if s.workflowResolver == nil {
		return "", domainError(http.StatusServiceUnavailable, "workflow_unavailable", "workflow images are unavailable")
	}
	digest, err := s.workflowResolver(ref)
	switch {
	case err == nil:
		return digest, nil
	case errors.Is(err, workflowimage.ErrNotFound):
		return "", domainError(http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, workflowimage.ErrInvalid):
		return "", domainError(http.StatusBadRequest, "invalid_workflow_ref", err.Error())
	default:
		return "", err
	}
}

// SetQueueWorkflow binds a queue to the workflow image ref names. revision is
// the current binding revision, 0 for a first binding. Binding the digest that
// is already bound changes nothing and keeps the revision.
func (s *Service) SetQueueWorkflow(ctx context.Context, actor Actor, queue, ref string, revision int64) (QueueWorkflow, error) {
	if err := s.requireWorkflowAdmin(actor); err != nil {
		return QueueWorkflow{}, err
	}
	queue = strings.ToUpper(strings.TrimSpace(queue))
	digest, err := s.resolveWorkflowRef(ref)
	if err != nil {
		return QueueWorkflow{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QueueWorkflow{}, err
	}
	defer tx.Rollback()
	if err := requireQueueExists(ctx, tx, queue); err != nil {
		return QueueWorkflow{}, err
	}
	current, bound, err := queueWorkflowTx(ctx, tx, queue)
	if err != nil {
		return QueueWorkflow{}, err
	}
	if revision != current.Revision {
		return QueueWorkflow{}, queueWorkflowRevisionConflict(current.Revision)
	}
	manifest, err := loadManifestTx(ctx, tx, digest)
	if err != nil {
		if ErrorCode(err) == "workflow_not_found" {
			return QueueWorkflow{}, domainError(http.StatusNotFound, "not_found", "workflow image "+digest+" is not published")
		}
		return QueueWorkflow{}, err
	}
	if bound && current.Digest == digest {
		return current, nil
	}
	if missing, err := emptyPools(ctx, tx, queue, manifest.Definition.Pools()); err != nil {
		return QueueWorkflow{}, err
	} else if len(missing) > 0 {
		return QueueWorkflow{}, poolsEmptyError(missing)
	}
	now := s.now()
	next := QueueWorkflow{
		Queue: queue, Name: manifest.Name, Version: manifest.Version,
		Digest: digest, Revision: current.Revision + 1, UpdatedAt: now,
	}
	if bound {
		result, err := tx.ExecContext(ctx, `
			UPDATE task_queue_workflows SET workflow_digest = ?, revision = ?, updated_at = ?
			WHERE queue_prefix = ? AND revision = ?`, digest, next.Revision, now, queue, current.Revision)
		if err != nil {
			return QueueWorkflow{}, err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return QueueWorkflow{}, err
		} else if affected != 1 {
			return QueueWorkflow{}, queueWorkflowRevisionConflict(current.Revision)
		}
	} else if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_queue_workflows(queue_prefix, workflow_digest, revision, updated_at)
		VALUES (?, ?, ?, ?)`, queue, digest, next.Revision, now); err != nil {
		return QueueWorkflow{}, err
	}
	if _, err := appendQueueEventTx(ctx, tx, Queue{Prefix: queue, Revision: next.Revision},
		"queue.workflow_bound", actor, map[string]any{
			"workflow": manifest.Name, "version": manifest.Version, "digest": digest,
		}, now); err != nil {
		return QueueWorkflow{}, err
	}
	if err := tx.Commit(); err != nil {
		return QueueWorkflow{}, err
	}
	s.signal()
	return next, nil
}

// ClearQueueWorkflow removes the binding of a queue. Tasks already created
// keep the digest they are pinned to.
func (s *Service) ClearQueueWorkflow(ctx context.Context, actor Actor, queue string, revision int64) error {
	if err := s.requireWorkflowAdmin(actor); err != nil {
		return err
	}
	queue = strings.ToUpper(strings.TrimSpace(queue))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireQueueExists(ctx, tx, queue); err != nil {
		return err
	}
	current, bound, err := queueWorkflowTx(ctx, tx, queue)
	if err != nil {
		return err
	}
	if !bound {
		return errQueueWorkflowNotFound()
	}
	if revision != current.Revision {
		return queueWorkflowRevisionConflict(current.Revision)
	}
	result, err := tx.ExecContext(ctx, `
		DELETE FROM task_queue_workflows WHERE queue_prefix = ? AND revision = ?`, queue, revision)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return queueWorkflowRevisionConflict(current.Revision)
	}
	if _, err := appendQueueEventTx(ctx, tx, Queue{Prefix: queue, Revision: current.Revision + 1},
		"queue.workflow_cleared", actor, map[string]any{
			"workflow": current.Name, "version": current.Version, "digest": current.Digest,
		}, s.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.signal()
	return nil
}

// GetQueueWorkflow returns the workflow image a queue is bound to.
func (s *Service) GetQueueWorkflow(ctx context.Context, actor Actor, queue string) (QueueWorkflow, error) {
	if err := s.requireWorkflowAdmin(actor); err != nil {
		return QueueWorkflow{}, err
	}
	binding, bound, err := queueWorkflowTx(ctx, s.db, strings.ToUpper(strings.TrimSpace(queue)))
	if err != nil {
		return QueueWorkflow{}, err
	}
	if !bound {
		return QueueWorkflow{}, errQueueWorkflowNotFound()
	}
	return binding, nil
}

func requireQueueExists(ctx context.Context, q queryer, queue string) error {
	var exists int
	if err := q.QueryRowContext(ctx, `SELECT 1 FROM task_queues WHERE prefix = ?`, queue).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return domainError(http.StatusNotFound, "queue_not_found", "queue not found")
		}
		return err
	}
	return nil
}

func requireAgentsExist(ctx context.Context, q queryer, agents []string) error {
	for _, agent := range agents {
		var exists int
		if err := q.QueryRowContext(ctx, `SELECT 1 FROM agents WHERE name = ?`, agent).Scan(&exists); err != nil {
			if err == sql.ErrNoRows {
				return &Error{
					Status: http.StatusBadRequest, Code: "agent_not_found",
					Msg: "agent pool member does not exist", Data: map[string]any{"agent": agent},
				}
			}
			return err
		}
	}
	return nil
}

func agentPoolByName(ctx context.Context, q queryer, queue string, name string) (AgentPool, bool, error) {
	var pool AgentPool
	err := q.QueryRowContext(ctx, `
		SELECT id, queue_prefix, name, revision, created_at, updated_at
		FROM task_agent_pools WHERE queue_prefix = ? AND name = ?`, queue, name).Scan(
		&pool.ID, &pool.Queue, &pool.Name, &pool.Revision, &pool.CreatedAt, &pool.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return AgentPool{}, false, nil
	}
	if err != nil {
		return AgentPool{}, false, err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT agent FROM task_agent_pool_members
		WHERE pool_id = ? ORDER BY position`, pool.ID)
	if err != nil {
		return AgentPool{}, false, err
	}
	defer rows.Close()
	pool.Agents = []string{}
	for rows.Next() {
		var agent string
		if err := rows.Scan(&agent); err != nil {
			return AgentPool{}, false, err
		}
		pool.Agents = append(pool.Agents, agent)
	}
	if err := rows.Err(); err != nil {
		return AgentPool{}, false, err
	}
	return pool, true, nil
}

func normalizePoolAgents(agents []string) []string {
	return normalizeOwners(agents)
}

func poolRevisionConflict(current AgentPool) error {
	return &Error{
		Status: http.StatusConflict, Code: "revision_conflict",
		Msg:  "agent pool was changed by another actor",
		Data: map[string]any{"current_revision": current.Revision, "current": current},
	}
}
