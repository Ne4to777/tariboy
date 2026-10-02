package tasks

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// Pause reasons, stored in tasks.workflow_paused_reason, and the decisions
// that resolve a pause.
const (
	PauseIdleIterations    = "idle_iterations"
	PauseRejectedRequests  = "rejected_requests"
	PauseScriptFailures    = "script_failures"
	PauseHolderUnavailable = "holder_unavailable"

	ResumeContinue = "continue"
	ResumeRelease  = "release"
)

// maxPauseDetailBytes bounds the untrusted detail a pause comment quotes.
const maxPauseDetailBytes = 1 << 10

// pauseReasonText says what a pause reason means, for the customer.
func pauseReasonText(reason string) string {
	switch reason {
	case PauseIdleIterations:
		return "the holder finished too many iterations in a row in this status without requesting a transition"
	case PauseRejectedRequests:
		return "checks rejected too many transition requests in a row in this status"
	case PauseScriptFailures:
		return "scripts failed too many times in a row in this status"
	case PauseHolderUnavailable:
		return "the holder cannot work on the task (disabled, halted, out of the pool, or deleted) and the grace period ran out"
	default:
		return reason
	}
}

// pauseTx pauses a workflow task. It is a no-op when the task is already
// paused or closed. The task keeps its status and assignee; its scripts and
// pending request stop, its category becomes wait_customer, and the customer
// is asked for a decision through a wait authored by the workflow. The caller
// commits and then signals.
func (s *Service) pauseTx(ctx context.Context, tx *sql.Tx, task *Task, manifest workflowimage.Manifest, reason, detail string) error {
	if task.WorkflowPausedReason != "" || task.Status == StatusDone || task.Status == StatusCancelled {
		return nil
	}
	now := s.now()
	if err := stopVisitScriptsTx(ctx, tx, *task, "the task was paused: "+reason, now); err != nil {
		return err
	}
	task.WorkflowPausedReason, task.Status = reason, StatusWaitCustomer
	task.Revision++
	task.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = ?, workflow_paused_reason = ?, revision = ?, updated_at = ? WHERE id = ?`,
		task.Status, reason, task.Revision, now, task.ID); err != nil {
		return err
	}
	if _, err := appendEventTx(ctx, tx, *task, "workflow.paused", Actor{Principal: workflowActor}, map[string]any{
		"status": task.WorkflowStatus, "reason": reason,
	}, now); err != nil {
		return err
	}
	if err := s.askCustomerTx(ctx, tx, *task, pauseComment(*task, reason, detail), now); err != nil {
		return err
	}
	owner := ""
	if status, ok := currentStatus(manifest, task.WorkflowStatus); ok && !status.Terminal {
		owner = status.Owner.Kind
	}
	task.WaitingOn = ""
	finishWorkflowFields(task, owner)
	return nil
}

// pauseComment is the workflow's question to the customer on a paused task.
// detail is untrusted: it is bounded and quoted as text.
func pauseComment(task Task, reason, detail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "@%s The workflow paused this task in status %q and waits for your decision.\n\n", task.Customer, task.WorkflowStatus)
	fmt.Fprintf(&b, "Reason (`%s`): %s.\n", reason, pauseReasonText(reason))
	if detail = strings.TrimSpace(cutRunes(detail, maxPauseDetailBytes)); detail != "" {
		b.WriteString("\nDetails:\n\n")
		b.WriteString(quoteBlock(detail))
	}
	fmt.Fprintf(&b, "\nDecide with one of:\n\n"+
		"- Continue with the same holder: `ttasks workflow resume %[1]s --decision continue`\n"+
		"- Release the holder and dispatch the task again (pool statuses only): `ttasks workflow resume %[1]s --decision release`\n"+
		"- Cancel the task: `ttasks cancel %[1]s`\n\n"+
		"A plain reply does not resume the task.", task.Key)
	return b.String()
}

// quoteBlock renders text as a Markdown block quote holding a fenced block
// whose fence is longer than any backtick run in text, so headings, fences,
// and mentions inside it render as plain text.
func quoteBlock(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	var b strings.Builder
	b.WriteString("> " + fence + "text\n")
	for _, line := range strings.Split(text, "\n") {
		b.WriteString(strings.TrimRight("> "+line, " ") + "\n")
	}
	b.WriteString("> " + fence + "\n")
	return b.String()
}

// ResumeWorkflow resolves a pause. Customer only. continue restores the
// category of the current status with the same holder; release, only in a
// pool status, marks the holder released and dispatches the task again to
// another member. Both reset the visit's counters and answer the pause wait.
func (s *Service) ResumeWorkflow(ctx context.Context, actor Actor, key, decision string) (Task, error) {
	if err := requireWorkflowOperator(actor); err != nil {
		return Task{}, err
	}
	decision = strings.TrimSpace(decision)
	if decision != ResumeContinue && decision != ResumeRelease {
		return Task{}, domainError(http.StatusBadRequest, "invalid_decision",
			"a pause is resolved with --decision continue or --decision release")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	task, manifest, err := artifactTaskTx(ctx, tx, actor, key)
	if err != nil {
		return Task{}, err
	}
	if task.WorkflowPausedReason == "" {
		return Task{}, domainError(http.StatusConflict, "workflow_not_paused", "task "+task.Key+" is not paused")
	}
	status, ok := currentStatus(manifest, task.WorkflowStatus)
	if !ok || status.Terminal {
		return Task{}, domainError(http.StatusConflict, "status_unknown",
			"task "+task.Key+" is in status "+task.WorkflowStatus+", which its workflow does not declare")
	}
	pool := status.Owner.Kind == workflowfile.OwnerPool
	if decision == ResumeRelease && !pool {
		return Task{}, domainError(http.StatusBadRequest, "invalid_decision",
			"release applies only to a status owned by an agent pool; status "+status.ID+" is not")
	}
	now := s.now()
	if err := guardRevisionTx(ctx, tx, task); err != nil {
		return Task{}, err
	}
	if err := resolveWorkflowWaitsTx(ctx, tx, task, "", now); err != nil {
		return Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_status_visits SET idle_iterations = 0, rejected_requests = 0, script_failures = 0
		WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		return Task{}, err
	}
	reason := task.WorkflowPausedReason
	previousAssignee, previousCategory := task.Assignee, task.Status
	task.WorkflowPausedReason = ""
	switch status.Owner.Kind {
	case workflowfile.OwnerPool:
		if decision == ResumeRelease {
			if _, err := tx.ExecContext(ctx, `
				UPDATE task_workflow_holders SET released = 1 WHERE task_id = ? AND pool = ?`,
				task.ID, status.Owner.Pool); err != nil {
				return Task{}, err
			}
		}
		// The previous holder is sticky unless released; otherwise this is the
		// dispatch of entering the status.
		agent := ""
		if !task.Blocked {
			if agent, err = pickHolderTx(ctx, tx, task, status.Owner.Pool, nil); err != nil {
				return Task{}, err
			}
		}
		if agent == "" {
			task.Status, task.Assignee = StatusOpen, ""
		} else {
			if err := s.recordHolderTx(ctx, tx, task, status.Owner.Pool, agent); err != nil {
				return Task{}, err
			}
			task.Status, task.Assignee = StatusInProgress, agentPrincipal(agent)
		}
	case workflowfile.OwnerCustomer:
		task.Status = StatusWaitCustomer
	case workflowfile.OwnerScript:
		task.Status, task.Assignee = StatusWaitCustomer, ""
		if status.Watch != nil {
			if _, err := tx.ExecContext(ctx, `
				UPDATE task_status_visits SET next_watch_at = ? WHERE task_id = ? AND left_at = ''`,
				watchTime(s.clock()), task.ID); err != nil {
				return Task{}, err
			}
		}
	default:
		return Task{}, fmt.Errorf("workflow status %q has no owner", status.ID)
	}
	if task.Status == StatusInProgress && previousCategory != StatusInProgress && task.StartedAt == "" {
		task.StartedAt = now
	}
	task.Revision++
	task.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = ?, assignee = ?, workflow_paused_reason = '', revision = ?, updated_at = ?
		WHERE id = ?`, task.Status, task.Assignee, task.Revision, now, task.ID); err != nil {
		return Task{}, err
	}
	if _, err := appendEventTx(ctx, tx, task, "workflow.resumed", actor, map[string]any{
		"status": task.WorkflowStatus, "decision": decision, "actor": actor.Principal, "reason": reason,
	}, now); err != nil {
		return Task{}, err
	}
	if status.Owner.Kind == workflowfile.OwnerCustomer {
		if err := s.openStatusWaitTx(ctx, tx, task, status, now); err != nil {
			return Task{}, err
		}
	}
	if err := recordAssignmentTx(ctx, tx, task, previousAssignee, now); err != nil {
		return Task{}, err
	}
	resumed, err := taskByID(tx, task.ID)
	if err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	s.signal()
	resumed.Access = "write"
	return resumed, nil
}
