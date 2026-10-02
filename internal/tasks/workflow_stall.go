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
	// In a pool status an unpaused task waits on its customer only while the
	// holder's own question is open: waiting for the answer is not idleness.
	if task.WorkflowDigest == "" || task.Status == StatusDone || task.Status == StatusCancelled ||
		task.Status == StatusWaitCustomer || task.WorkflowPausedReason != "" || task.Assignee != agentPrincipal(end.Agent) {
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
	var enteredAt, resumedAt string
	var idle int
	if err := tx.QueryRowContext(ctx, `
		SELECT id, entered_at, resumed_at, idle_iterations FROM task_status_visits WHERE task_id = ? AND left_at = ''`,
		task.ID).Scan(&visitID, &enteredAt, &resumedAt, &idle); err != nil {
		return err
	}
	entered, err := time.Parse(time.RFC3339Nano, enteredAt)
	if err != nil {
		return fmt.Errorf("visit %d entered_at: %w", visitID, err)
	}
	if entered.After(end.StartedAt) {
		return nil
	}
	// An iteration that started while the task was paused belongs to the
	// pause, not to the resumed visit.
	if resumedAt != "" {
		resumed, err := time.Parse(time.RFC3339Nano, resumedAt)
		if err != nil {
			return fmt.Errorf("visit %d resumed_at: %w", visitID, err)
		}
		if end.StartedAt.Before(resumed) {
			return nil
		}
	}
	requested, err := requestedDuringTx(ctx, tx, visitID, end.StartedAt, end.FinishedAt)
	if err != nil || requested {
		return err
	}
	asked, err := askedCustomerDuringTx(ctx, tx, task, end.StartedAt, end.FinishedAt)
	if err != nil || asked {
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

// The service stores times as RFC3339Nano in UTC, which does not order as a
// string within one second. secondOf and secondAfter are the whole-second
// prefixes SQL narrows rows with: every time in t's second or later sorts at
// or after secondOf(t), and every time up to t's second sorts before
// secondAfter(t). The exact comparison is then made on parsed times.
func secondOf(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05") }

func secondAfter(t time.Time) string {
	return t.UTC().Truncate(time.Second).Add(time.Second).Format("2006-01-02T15:04:05")
}

// requestedDuringTx reports whether a transition request of the visit kept
// the iteration from start to finish (both included) busy: a request was made
// during it, was still pending at its finish, or was cancelled or applied
// during it.
func requestedDuringTx(ctx context.Context, tx *sql.Tx, visitID int64, start, finish time.Time) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT created_at, state, finished_at FROM task_transition_requests
		WHERE visit_id = ? AND created_at < ? AND (created_at >= ? OR state = 'pending' OR finished_at >= ?)`,
		visitID, secondAfter(finish), secondOf(start), secondOf(start))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var createdAt, state, finishedAt string
		if err := rows.Scan(&createdAt, &state, &finishedAt); err != nil {
			return false, err
		}
		created, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return false, fmt.Errorf("transition request of visit %d created_at: %w", visitID, err)
		}
		if created.After(finish) {
			continue
		}
		if !created.Before(start) || state == "pending" {
			return true, nil
		}
		finished, err := time.Parse(time.RFC3339Nano, finishedAt)
		if err != nil {
			return false, fmt.Errorf("transition request of visit %d finished_at: %w", visitID, err)
		}
		if finished.After(finish) || ((state == "cancelled" || state == "applied") && !finished.Before(start)) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// askedCustomerDuringTx reports whether the task's assignee asked the
// customer a question from start to finish, both included.
func askedCustomerDuringTx(ctx context.Context, tx *sql.Tx, task Task, start, finish time.Time) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT requested_at FROM task_waiting_for
		WHERE expected_principal = ? AND task_id = ? AND requesting_principal = ? AND requested_at >= ? AND requested_at < ?`,
		task.Customer, task.ID, task.Assignee, secondOf(start), secondAfter(finish))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var requestedAt string
		if err := rows.Scan(&requestedAt); err != nil {
			return false, err
		}
		requested, err := time.Parse(time.RFC3339Nano, requestedAt)
		if err != nil {
			return false, fmt.Errorf("wait of task %s requested_at: %w", task.Key, err)
		}
		if !requested.Before(start) && !requested.After(finish) {
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
// Only the tasks unavailableHolderTasks selects get a transaction of their own.
func (s *Service) CheckHolders(ctx context.Context, now time.Time) (int, error) {
	ids, err := s.unavailableHolderTasks(ctx)
	if err != nil {
		return 0, err
	}
	paused := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		ok, err := s.checkHolder(ctx, id, now)
		if err != nil {
			if ctx.Err() == nil && s.firstHolderFailure(id, err) {
				s.logger().Warn("check workflow holder", "task_id", id, "err", err)
			}
			continue
		}
		s.forgetHolderFailure(id)
		if ok {
			paused++
		}
	}
	if paused > 0 {
		s.signal()
	}
	return paused, ctx.Err()
}

// unavailableHolderTasks selects, in one query, the open unpaused workflow
// tasks whose agent assignee has an unreleased holder row that either cannot
// work by holderUnavailableCauseSQL or is marked unavailable_since: the only
// tasks checkHolder may change. A task whose holder can work and carries no
// mark is left alone.
func (s *Service) unavailableHolderTasks(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT t.id FROM tasks t
		JOIN task_workflow_holders h ON h.task_id = t.id AND t.assignee = 'agent:' || h.agent AND h.released = 0
		LEFT JOIN agents a ON a.name = h.agent
		LEFT JOIN task_agent_pools p ON p.queue_prefix = t.queue_prefix AND p.name = h.pool
		LEFT JOIN task_agent_pool_members m ON m.pool_id = p.id AND m.agent = h.agent
		WHERE t.workflow_digest IS NOT NULL AND t.status NOT IN ('done', 'cancelled')
		  AND t.workflow_paused_reason = '' AND t.assignee LIKE 'agent:%'
		  AND (h.unavailable_since <> '' OR (`+holderUnavailableCauseSQL+`) <> '')
		ORDER BY t.id`, agent.IdleStopPrefix, agent.IdleStopPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// manifestLoadError is a holder check that could not load the task's
// workflow manifest; it repeats on every pass until the image is fixed.
type manifestLoadError struct{ err error }

func (e manifestLoadError) Error() string { return "load workflow manifest: " + e.err.Error() }
func (e manifestLoadError) Unwrap() error { return e.err }

// firstHolderFailure reports whether err of the holder check of task id
// should be logged: a manifest that cannot be loaded is logged once per task
// and error text, every other error every time.
func (s *Service) firstHolderFailure(id int64, err error) bool {
	var load manifestLoadError
	if !errors.As(err, &load) {
		return true
	}
	s.holderFailuresMu.Lock()
	defer s.holderFailuresMu.Unlock()
	if s.holderFailures == nil {
		s.holderFailures = map[int64]string{}
	}
	if s.holderFailures[id] == err.Error() {
		return false
	}
	s.holderFailures[id] = err.Error()
	return true
}

// forgetHolderFailure clears the logged manifest failure of task id after a
// check that loaded its manifest.
func (s *Service) forgetHolderFailure(id int64) {
	s.holderFailuresMu.Lock()
	defer s.holderFailuresMu.Unlock()
	delete(s.holderFailures, id)
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
		if ctx.Err() != nil {
			return false, err
		}
		return false, manifestLoadError{err}
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
