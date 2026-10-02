package tasks

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
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
