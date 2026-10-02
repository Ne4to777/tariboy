package tasks

import (
	"context"
	"database/sql"
	"errors"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/workflowfile"
)

// pickHolderTx chooses the agent for a task entering or waiting in a status
// owned by pool. The task's previous holder for the pool wins while it is still
// a member, whatever it is doing, unless the customer released it: a released
// holder is neither sticky nor eligible for this task and pool until another
// agent replaces its row or an operator move clears it. Otherwise the first
// eligible member wins:
// enabled, loop enabled, Goal enabled, not halted, with no current Goal, with
// no other assigned task that Goal could select, and not in busy; ties go to the member dispatched least recently in this queue,
// then to pool order. It returns "" when nobody is eligible.
func pickHolderTx(ctx context.Context, tx *sql.Tx, task Task, pool string, busy map[string]bool) (string, error) {
	var holder string
	err := tx.QueryRowContext(ctx, `
		SELECT h.agent FROM task_workflow_holders h
		JOIN task_agent_pools p ON p.queue_prefix = ? AND p.name = h.pool
		JOIN task_agent_pool_members m ON m.pool_id = p.id AND m.agent = h.agent
		WHERE h.task_id = ? AND h.pool = ? AND h.released = 0`, task.Queue, task.ID, pool).Scan(&holder)
	if err == nil {
		return holder, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	// The halt test mirrors agent.HaltReason: an error reason, or a status
	// message that starts with the idle-stop prefix. The assigned-work test is
	// the one taskgoal's reconcileAgent selects a Goal by, read in this
	// transaction: current_goal_task_key is filled only after the reconciler
	// runs, so without it one free agent would collect every new task.
	rows, err := tx.QueryContext(ctx, `
		SELECT m.agent
		FROM task_agent_pools p
		JOIN task_agent_pool_members m ON m.pool_id = p.id
		JOIN agents a ON a.name = m.agent
		WHERE p.queue_prefix = ? AND p.name = ?
		  AND a.enabled = 1 AND a.loop_enabled = 1 AND a.goal_enabled = 1
		  AND a.error_reason = '' AND substr(a.status_message, 1, length(?)) <> ?
		  AND a.current_goal_task_key = ''
		  AND NOT EXISTS (
			SELECT 1 FROM task_workflow_holders r
			WHERE r.task_id = ? AND r.pool = p.name AND r.agent = m.agent AND r.released = 1
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM tasks t
			WHERE t.assignee = 'agent:' || m.agent AND t.id <> ?
			  AND t.status IN ('in_progress', 'open')
			  AND t.manual_block_reason = ''
			  AND NOT EXISTS (
				SELECT 1 FROM task_relations r JOIN tasks b ON b.id = r.source_id
				WHERE r.target_id = t.id AND r.type = 'blocks'
				  AND b.status NOT IN ('done', 'cancelled')
			  )
		  )
		ORDER BY COALESCE((
			SELECT MAX(h.dispatched_at) FROM task_workflow_holders h
			JOIN tasks ht ON ht.id = h.task_id
			WHERE h.agent = m.agent AND ht.queue_prefix = p.queue_prefix
		), ''), m.position`,
		task.Queue, pool, agent.IdleStopPrefix, agent.IdleStopPrefix, task.ID, task.ID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		if !busy[name] {
			return name, nil
		}
	}
	return "", rows.Err()
}

// dispatchedAtLayout is fixed width, so dispatch times order correctly as
// strings; RFC3339Nano trims trailing zeros and misorders times within a second.
const dispatchedAtLayout = "2006-01-02T15:04:05.000000000Z"

// recordHolderTx records name as the task's holder for pool, dispatched now,
// replacing a released holder. It is the only writer of
// task_workflow_holders.dispatched_at.
func (s *Service) recordHolderTx(ctx context.Context, tx *sql.Tx, task Task, pool, name string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO task_workflow_holders(task_id, pool, agent, dispatched_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(task_id, pool) DO UPDATE SET agent = excluded.agent, dispatched_at = excluded.dispatched_at, released = 0`,
		task.ID, pool, name, s.clock().UTC().Format(dispatchedAtLayout))
	return err
}

// DispatchPending assigns holders to workflow tasks waiting in a pool status
// without one. It returns how many tasks it assigned.
func (s *Service) DispatchPending(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id FROM tasks t
		WHERE t.workflow_digest IS NOT NULL AND t.status = 'open' AND t.workflow_paused_reason = ''
		  AND t.manual_block_reason = '' AND NOT EXISTS (
			SELECT 1 FROM task_relations r
			JOIN tasks blocker ON blocker.id = r.source_id
			WHERE r.target_id = t.id AND r.type = 'blocks'
			  AND blocker.status NOT IN ('done', 'cancelled')
		  )
		ORDER BY t.priority, t.created_at, t.id`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	// An agent assigned in this pass has work now and is not eligible again.
	busy := map[string]bool{}
	assigned := 0
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		name, err := s.dispatchTask(ctx, id, busy)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if name != "" {
			busy[name] = true
			assigned++
		}
	}
	if assigned > 0 {
		s.signal()
	}
	return assigned, errors.Join(errs...)
}

// dispatchTask assigns one waiting workflow task in its own transaction and
// returns the agent it assigned, or "" when the task needs nobody or nobody is
// eligible.
func (s *Service) dispatchTask(ctx context.Context, id int64, busy map[string]bool) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	task, err := taskByID(tx, id)
	if err != nil {
		return "", err
	}
	if task.WorkflowDigest == "" || task.Status != StatusOpen || task.WorkflowPausedReason != "" || task.Blocked {
		return "", nil
	}
	manifest, err := loadManifestTx(ctx, tx, task.WorkflowDigest)
	if err != nil {
		return "", err
	}
	status, ok := currentStatus(manifest, task.WorkflowStatus)
	if !ok || status.Terminal || status.Owner.Kind != workflowfile.OwnerPool {
		return "", nil
	}
	name, err := pickHolderTx(ctx, tx, task, status.Owner.Pool, busy)
	if err != nil || name == "" {
		return "", err
	}
	now := s.now()
	if err := s.recordHolderTx(ctx, tx, task, status.Owner.Pool, name); err != nil {
		return "", err
	}
	previousAssignee, previousRevision := task.Assignee, task.Revision
	task.Status, task.Assignee = StatusInProgress, agentPrincipal(name)
	if task.StartedAt == "" {
		task.StartedAt = now
	}
	task.Revision++
	task.UpdatedAt = now
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = ?, assignee = ?, revision = ?, updated_at = ?
		WHERE id = ? AND revision = ?`,
		task.Status, task.Assignee, task.Revision, now, task.ID, previousRevision)
	if err != nil {
		return "", err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return "", err
	}
	if err := recordAssignmentTx(ctx, tx, task, previousAssignee, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return name, nil
}
