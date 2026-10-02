package tasks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// IterationEnd describes one finished iteration of an agent.
type IterationEnd struct {
	Agent       string
	IterationID string
	GoalTaskKey string // the Goal the iteration ran with; "" when none
	StartedAt   time.Time
	FinishedAt  time.Time
}

// pauseAtLimitTx pauses the task with reason when the visit's counter for
// that reason reached its limit in the visit's status. A limit of 0 or less
// never pauses. detail is untrusted text for the pause comment.
func (s *Service) pauseAtLimitTx(ctx context.Context, tx *sql.Tx, task *Task, manifest workflowimage.Manifest, visitID int64, reason, detail string) error {
	var statusID string
	var idle, rejected, failures int
	if err := tx.QueryRowContext(ctx, `
		SELECT status_id, idle_iterations, rejected_requests, script_failures FROM task_status_visits WHERE id = ?`,
		visitID).Scan(&statusID, &idle, &rejected, &failures); err != nil {
		return err
	}
	limits := manifest.Definition.StatusLimits(statusID)
	var count, limit int
	switch reason {
	case PauseIdleIterations:
		count, limit = idle, limits.IdleIterations
	case PauseRejectedRequests:
		count, limit = rejected, limits.RejectedRequests
	case PauseScriptFailures:
		count, limit = failures, limits.ScriptFailures
	default:
		return fmt.Errorf("pause reason %q has no counter", reason)
	}
	if limit <= 0 || count < limit {
		return nil
	}
	return s.pauseTx(ctx, tx, task, manifest, reason, detail)
}

// RecordIterationEnd counts the iteration as idle for its Goal task when the
// agent is the holder of a pool status it already held when the iteration
// started, the status did not change, and the agent made no transition
// request during the iteration. It pauses the task at the limit.
func (s *Service) RecordIterationEnd(ctx context.Context, end IterationEnd) error {
	key := strings.TrimSpace(end.GoalTaskKey)
	if key == "" || strings.TrimSpace(end.Agent) == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := taskByKey(tx, key)
	if ErrorCode(err) == "not_found" {
		return nil
	}
	if err != nil {
		return err
	}
	if task.WorkflowDigest == "" || task.Status == StatusDone || task.Status == StatusCancelled ||
		task.WorkflowPausedReason != "" || task.Assignee != agentPrincipal(end.Agent) {
		return nil
	}
	manifest, err := loadManifestTx(ctx, tx, task.WorkflowDigest)
	if err != nil {
		return err
	}
	status, ok := currentStatus(manifest, task.WorkflowStatus)
	if !ok || status.Terminal || status.Owner.Kind != workflowfile.OwnerPool {
		return nil
	}
	var holds bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM task_workflow_holders WHERE task_id = ? AND pool = ? AND agent = ? AND released = 0)`,
		task.ID, status.Owner.Pool, strings.TrimPrefix(task.Assignee, "agent:")).Scan(&holds); err != nil {
		return err
	}
	if !holds {
		return nil
	}
	var visitID int64
	var enteredAt string
	var idle int
	if err := tx.QueryRowContext(ctx, `
		SELECT id, entered_at, idle_iterations FROM task_status_visits WHERE task_id = ? AND left_at = ''`,
		task.ID).Scan(&visitID, &enteredAt, &idle); err != nil {
		return err
	}
	entered, err := time.Parse(time.RFC3339Nano, enteredAt)
	if err != nil {
		return fmt.Errorf("visit %d entered_at: %w", visitID, err)
	}
	if entered.After(end.StartedAt) {
		return nil
	}
	requested, err := requestedDuringTx(ctx, tx, visitID, end.StartedAt, end.FinishedAt)
	if err != nil || requested {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_status_visits SET idle_iterations = idle_iterations + 1 WHERE id = ?`, visitID); err != nil {
		return err
	}
	idle++
	if err := s.pauseAtLimitTx(ctx, tx, &task, manifest, visitID, PauseIdleIterations,
		fmt.Sprintf("%d iterations in a row ended without a transition request.", idle)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if task.WorkflowPausedReason != "" {
		s.signal()
	}
	return nil
}

// requestedDuringTx reports whether anyone made a transition request of the
// visit from start to finish, both included. created_at is RFC3339Nano, which
// does not order as a string, so the times are compared parsed.
func requestedDuringTx(ctx context.Context, tx *sql.Tx, visitID int64, start, finish time.Time) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT created_at FROM task_transition_requests WHERE visit_id = ?`, visitID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var createdAt string
		if err := rows.Scan(&createdAt); err != nil {
			return false, err
		}
		created, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return false, fmt.Errorf("transition request of visit %d created_at: %w", visitID, err)
		}
		if !created.Before(start) && !created.After(finish) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// holderUnavailableText says why agent cannot work on pool work, for the
// pause comment.
func holderUnavailableText(name, pool, cause string) string {
	switch cause {
	case causeDeleted:
		return "agent " + name + " no longer exists"
	case causeDisabled:
		return "agent " + name + " is disabled"
	case causeLoopDisabled:
		return "the loop of agent " + name + " is disabled"
	case causeHalted:
		return "agent " + name + " is halted"
	case causeLeftPool:
		return "agent " + name + " is no longer a member of pool " + pool
	default:
		return "agent " + name + " cannot work (" + cause + ")"
	}
}

// CheckHolders records since when each holder of an active pool status cannot
// work and pauses tasks whose holder has been unavailable for the grace.
func (s *Service) CheckHolders(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM tasks
		WHERE workflow_digest IS NOT NULL AND status NOT IN ('done', 'cancelled')
		  AND workflow_paused_reason = '' AND assignee LIKE 'agent:%'
		ORDER BY id`)
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
	paused := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		ok, err := s.checkHolder(ctx, id, now)
		if err != nil {
			if ctx.Err() == nil {
				s.logger().Warn("check workflow holder", "task_id", id, "err", err)
			}
			continue
		}
		if ok {
			paused++
		}
	}
	if paused > 0 {
		s.signal()
	}
	return paused, ctx.Err()
}

// checkHolder checks the holder of one task in its own transaction and
// reports whether it paused the task.
func (s *Service) checkHolder(ctx context.Context, id int64, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	task, err := taskByID(tx, id)
	if err != nil {
		return false, err
	}
	name, isAgent := strings.CutPrefix(task.Assignee, "agent:")
	if task.WorkflowDigest == "" || task.Status == StatusDone || task.Status == StatusCancelled ||
		task.WorkflowPausedReason != "" || !isAgent {
		return false, nil
	}
	manifest, err := loadManifestTx(ctx, tx, task.WorkflowDigest)
	if err != nil {
		return false, err
	}
	status, ok := currentStatus(manifest, task.WorkflowStatus)
	if !ok || status.Terminal || status.Owner.Kind != workflowfile.OwnerPool {
		return false, nil
	}
	var since, cause string
	err = tx.QueryRowContext(ctx, `
		SELECT h.unavailable_since, `+holderUnavailableCauseSQL+`
		FROM task_workflow_holders h
		LEFT JOIN agents a ON a.name = h.agent
		LEFT JOIN task_agent_pools p ON p.queue_prefix = ? AND p.name = h.pool
		LEFT JOIN task_agent_pool_members m ON m.pool_id = p.id AND m.agent = h.agent
		WHERE h.task_id = ? AND h.pool = ? AND h.agent = ? AND h.released = 0`,
		agent.IdleStopPrefix, agent.IdleStopPrefix, task.Queue, task.ID, status.Owner.Pool, name).Scan(&since, &cause)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	const setSince = `UPDATE task_workflow_holders SET unavailable_since = ? WHERE task_id = ? AND pool = ?`
	if cause == "" {
		if since == "" {
			return false, nil
		}
		if _, err := tx.ExecContext(ctx, setSince, "", task.ID, status.Owner.Pool); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	start := now
	if since == "" {
		since = watchTime(now)
		if _, err := tx.ExecContext(ctx, setSince, since, task.ID, status.Owner.Pool); err != nil {
			return false, err
		}
	} else if start, err = time.Parse(time.RFC3339Nano, since); err != nil {
		return false, fmt.Errorf("holder unavailable_since: %w", err)
	}
	if now.Sub(start) >= manifest.Definition.StatusLimits(status.ID).UnavailableGrace {
		detail := holderUnavailableText(name, status.Owner.Pool, cause) + " since " + since + "."
		if err := s.pauseTx(ctx, tx, &task, manifest, PauseHolderUnavailable, detail); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return task.WorkflowPausedReason != "", nil
}
