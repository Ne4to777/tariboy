package tasks

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// workflowActor is the principal the engine acts as: it authors assignments,
// the customer-status comment, and its wait.
const workflowActor = "system:workflow"

// currentStatus finds a status of the manifest by ID.
func currentStatus(manifest workflowimage.Manifest, id string) (workflowfile.Status, bool) {
	for _, status := range manifest.Definition.Statuses {
		if status.ID == id {
			return status, true
		}
	}
	return workflowfile.Status{}, false
}

// statusOutcomes lists the outcome names a status declares, in order.
func statusOutcomes(status workflowfile.Status) []string {
	out := make([]string, 0, len(status.Transitions))
	for _, transition := range status.Transitions {
		out = append(out, transition.On)
	}
	return out
}

// workflowManagedErrorFor is workflowManagedError for a workflow task, with the
// current workflow status and its outcomes in Data.
func workflowManagedErrorFor(ctx context.Context, q queryer, task Task) error {
	manifest, err := loadManifestTx(ctx, q, task.WorkflowDigest)
	if err != nil {
		return err
	}
	outcomes := []string{}
	if status, ok := currentStatus(manifest, task.WorkflowStatus); ok {
		outcomes = statusOutcomes(status)
	}
	managed := workflowManagedError().(*Error)
	managed.Data = map[string]any{"status": task.WorkflowStatus, "outcomes": outcomes}
	return managed
}

// enterStatusTx closes the current visit, opens a visit for statusID, sets
// tasks.workflow_status, derives the category and assignee, and records the
// workflow.transitioned event and notification intents. via is the actor.
func (s *Service) enterStatusTx(ctx context.Context, tx *sql.Tx, task *Task, manifest workflowimage.Manifest, statusID, via, outcome, message string) error {
	next, ok := currentStatus(manifest, statusID)
	if !ok {
		return fmt.Errorf("workflow %s:%s has no status %q", manifest.Name, manifest.Version, statusID)
	}
	now := s.now()
	from := task.WorkflowStatus
	if previous, ok := currentStatus(manifest, from); ok && from != "" && !previous.Terminal {
		// Leaving a customer status by any route answers its question; leaving a
		// pool status makes the holder's questions to the customer moot.
		if err := resolveWorkflowWaitsTx(ctx, tx, *task,
			previous.Owner.Kind == workflowfile.OwnerCustomer, previous.Owner.Kind == workflowfile.OwnerPool, now); err != nil {
			return err
		}
	}
	var last int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) FROM task_status_visits WHERE task_id = ?`, task.ID).Scan(&last); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_status_visits SET left_at = ?, outcome = ?, message = ?
		WHERE task_id = ? AND left_at = ''`, now, outcome, message, task.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_status_visits(task_id, sequence, status_id, entered_at, entered_by)
		VALUES (?, ?, ?, ?, ?)`, task.ID, last+1, statusID, now, via); err != nil {
		return err
	}

	previousAssignee, previousCategory := task.Assignee, task.Status
	task.WorkflowStatus = statusID
	task.CompletedAt = ""
	switch {
	case next.Terminal:
		task.Status = StatusDone
		if next.Cancelled {
			task.Status = StatusCancelled
		}
		task.CompletedAt = now
	case next.Owner.Kind == workflowfile.OwnerPool:
		agent := ""
		if !task.Blocked && task.WorkflowPausedReason == "" {
			var err error
			if agent, err = pickHolderTx(ctx, tx, *task, next.Owner.Pool, nil); err != nil {
				return err
			}
		}
		if agent == "" {
			task.Status, task.Assignee = StatusOpen, ""
		} else {
			if err := s.recordHolderTx(ctx, tx, *task, next.Owner.Pool, agent); err != nil {
				return err
			}
			task.Status, task.Assignee = StatusInProgress, agentPrincipal(agent)
		}
	case next.Owner.Kind == workflowfile.OwnerCustomer:
		task.Status = StatusWaitCustomer
	case next.Owner.Kind == workflowfile.OwnerScript:
		task.Status, task.Assignee = StatusWaitCustomer, ""
	default:
		return fmt.Errorf("workflow status %q has no owner", statusID)
	}
	// The tasks_started_at trigger stores the first start; mirror it here.
	if task.Status == StatusInProgress && previousCategory != StatusInProgress && task.StartedAt == "" {
		task.StartedAt = now
	}
	task.Revision++
	task.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?, workflow_status = ?, assignee = ?, completed_at = ?, revision = ?, updated_at = ?
		WHERE id = ?`,
		task.Status, task.WorkflowStatus, task.Assignee, task.CompletedAt, task.Revision, now, task.ID); err != nil {
		return err
	}
	if _, err := appendEventTx(ctx, tx, *task, "workflow.transitioned", Actor{Principal: via}, map[string]any{
		"from": from, "to": statusID, "outcome": outcome, "actor": via, "message": message,
	}, now); err != nil {
		return err
	}
	if !next.Terminal && next.Owner.Kind == workflowfile.OwnerCustomer {
		if err := s.openStatusWaitTx(ctx, tx, *task, next, now); err != nil {
			return err
		}
	}
	if err := recordAssignmentTx(ctx, tx, *task, previousAssignee, now); err != nil {
		return err
	}
	task.WaitingOn = ""
	finishWorkflowFields(task, next.Owner.Kind)
	return nil
}

// resolveWorkflowWaitsTx resolves open waits of a workflow task without an
// answering comment: with systemWait the workflow's own question, and with
// holderQuestions every question an agent asked the task's customer. Waits
// requested by anyone else, or expecting another principal, stay open.
func resolveWorkflowWaitsTx(ctx context.Context, tx *sql.Tx, task Task, systemWait, holderQuestions bool, now string) error {
	if !systemWait && !holderQuestions {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE task_waiting_for SET resolved_at = ?
		WHERE task_id = ? AND resolved_at = '' AND (
			(? AND requesting_principal = ?) OR
			(? AND expected_principal = ? AND requesting_principal LIKE 'agent:%'))`,
		now, task.ID, systemWait, workflowActor, holderQuestions, task.Customer)
	return err
}

// openStatusWaitTx asks the customer for an outcome the way a comment with a
// mention does: a comment by the workflow, an open wait, and a question
// notification.
func (s *Service) openStatusWaitTx(ctx context.Context, tx *sql.Tx, task Task, status workflowfile.Status, now string) error {
	body := fmt.Sprintf("@%s The task is waiting for you in workflow status %q.\nOutcomes: %s.",
		task.Customer, status.ID, strings.Join(statusOutcomes(status), ", "))
	if instructions := strings.TrimSpace(status.Instructions); instructions != "" {
		body += "\n\n" + instructions
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO task_comments(task_id, author, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, task.ID, workflowActor, body, now, now)
	if err != nil {
		return err
	}
	commentID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := upsertWait(ctx, tx, task, task.Customer, workflowActor, commentID, now); err != nil {
		return err
	}
	sequence, err := appendEventTx(ctx, tx, task, "task.comment_added", Actor{Principal: workflowActor},
		map[string]any{"comment_id": commentID, "mentions": []string{task.Customer}, "resolved": []string{}}, now)
	if err != nil {
		return err
	}
	return enqueueNotificationTx(ctx, tx, sequence, task.Customer, workflowActor,
		"task.question", task, workflowActor+" asked for an answer on "+task.Key, now)
}

// recordAssignmentTx writes what a manual assignment writes, by the workflow:
// the task.updated event and the assignment notification.
func recordAssignmentTx(ctx context.Context, tx *sql.Tx, task Task, previousAssignee, now string) error {
	if task.Assignee == "" || task.Assignee == previousAssignee {
		return nil
	}
	sequence, err := appendEventTx(ctx, tx, task, "task.updated", Actor{Principal: workflowActor},
		map[string]any{"status": task.Status, "pull_request": task.PullRequest, "assignee": task.Assignee, "priority": task.Priority}, now)
	if err != nil {
		return err
	}
	return enqueueNotificationTx(ctx, tx, sequence, task.Assignee, workflowActor,
		"task.assigned", task, "Task assigned: "+task.Key+" "+task.Title, now)
}
