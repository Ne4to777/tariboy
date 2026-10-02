package tasks

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"

	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// maxTransitionMessageBytes is the largest message an advance may carry.
const maxTransitionMessageBytes = 4 << 10

// AdvanceInput declares an outcome for a task's current workflow status.
type AdvanceInput struct {
	Outcome string `json:"outcome"`
	Message string `json:"message"`
}

// Advance declares an outcome for the current status. A pool status accepts
// it from the holder; a customer status from the customer. A transition
// without checks applies in this call. A transition with checks is refused
// with checks_unavailable until the scripts plan lands.
func (s *Service) Advance(ctx context.Context, actor Actor, key string, in AdvanceInput) (TransitionRequest, error) {
	if err := validateActor(actor); err != nil {
		return TransitionRequest{}, err
	}
	in.Outcome = strings.TrimSpace(in.Outcome)
	if len(in.Message) > maxTransitionMessageBytes {
		return TransitionRequest{}, domainError(http.StatusBadRequest, "invalid_message",
			"a transition message is limited to 4 KiB")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TransitionRequest{}, err
	}
	defer tx.Rollback()
	task, manifest, err := artifactTaskTx(ctx, tx, actor, key)
	if err != nil {
		return TransitionRequest{}, err
	}
	if err := requireOpenWorkflow(task); err != nil {
		return TransitionRequest{}, err
	}
	status, ok := currentStatus(manifest, task.WorkflowStatus)
	if !ok {
		return TransitionRequest{}, domainError(http.StatusConflict, "status_unknown",
			"task "+task.Key+" is in status "+task.WorkflowStatus+", which its workflow does not declare")
	}
	owns, err := ownsStatusTx(ctx, tx, task, manifest, status, actor)
	if err != nil {
		return TransitionRequest{}, err
	}
	if !owns {
		return TransitionRequest{}, domainError(http.StatusForbidden, "not_holder",
			actor.Principal+" does not own status "+status.ID+" of "+task.Key)
	}
	transition, ok := statusTransition(status, in.Outcome)
	if !ok {
		unknown := domainError(http.StatusBadRequest, "outcome_unknown",
			"status "+status.ID+" does not declare outcome "+in.Outcome).(*Error)
		unknown.Data = map[string]any{"outcomes": statusOutcomes(status)}
		return TransitionRequest{}, unknown
	}
	artifacts, err := currentArtifactsTx(ctx, tx, task.ID)
	if err != nil {
		return TransitionRequest{}, err
	}
	if missing := missingArtifacts(transition, artifactNames(artifacts)); len(missing) > 0 {
		refused := domainError(http.StatusConflict, "artifact_missing",
			"outcome "+in.Outcome+" requires artifacts with no value: "+strings.Join(missing, ", ")).(*Error)
		refused.Data = map[string]any{"missing": missing}
		return TransitionRequest{}, refused
	}
	var pending bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM task_transition_requests WHERE task_id = ? AND state = 'pending')`,
		task.ID).Scan(&pending); err != nil {
		return TransitionRequest{}, err
	}
	if pending {
		return TransitionRequest{}, domainError(http.StatusConflict, "transition_pending",
			"another transition request of "+task.Key+" is still running")
	}
	// Checks run scripts, which a later plan adds. Until then a transition that
	// declares checks is refused here, before anything is written.
	if len(transition.Checks) > 0 {
		return TransitionRequest{}, domainError(http.StatusConflict, "checks_unavailable",
			"outcome "+in.Outcome+" declares checks, and script checks are not available in this build")
	}

	now := s.now()
	if err := guardRevisionTx(ctx, tx, task); err != nil {
		return TransitionRequest{}, err
	}
	var visitID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM task_status_visits WHERE task_id = ? AND left_at = ''`,
		task.ID).Scan(&visitID); err != nil {
		return TransitionRequest{}, err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO task_transition_requests(task_id, visit_id, outcome, message, actor, state, created_at, finished_at)
		VALUES (?, ?, ?, ?, ?, 'applied', ?, ?)`,
		task.ID, visitID, in.Outcome, in.Message, actor.Principal, now, now)
	if err != nil {
		return TransitionRequest{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return TransitionRequest{}, err
	}
	request := TransitionRequest{ID: id, TaskKey: task.Key, Outcome: in.Outcome, Message: in.Message,
		Actor: actor.Principal, State: "applied", CreatedAt: now, FinishedAt: now}
	if _, err := appendEventTx(ctx, tx, task, "workflow.transition_requested", actor, map[string]any{
		"request_id": id, "status": status.ID, "outcome": in.Outcome, "actor": actor.Principal, "state": request.State,
	}, now); err != nil {
		return TransitionRequest{}, err
	}
	if err := s.enterStatusTx(ctx, tx, &task, manifest, transition.To, actor.Principal, in.Outcome, in.Message); err != nil {
		return TransitionRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return TransitionRequest{}, err
	}
	s.signal()
	return request, nil
}

// MoveWorkflow moves a task to any status without checks. Customer only.
func (s *Service) MoveWorkflow(ctx context.Context, actor Actor, key, to, reason string) (Task, error) {
	if err := requireWorkflowOperator(actor); err != nil {
		return Task{}, err
	}
	to, reason = strings.TrimSpace(to), strings.TrimSpace(reason)
	if reason == "" {
		return Task{}, domainError(http.StatusBadRequest, "reason_required", "a workflow move needs a reason")
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
	if _, ok := currentStatus(manifest, to); !ok {
		ids := make([]string, 0, len(manifest.Definition.Statuses))
		for _, status := range manifest.Definition.Statuses {
			ids = append(ids, status.ID)
		}
		unknown := domainError(http.StatusBadRequest, "status_unknown",
			"workflow "+manifest.Name+" does not declare status "+to).(*Error)
		unknown.Data = map[string]any{"statuses": ids}
		return Task{}, unknown
	}
	now := s.now()
	if err := guardRevisionTx(ctx, tx, task); err != nil {
		return Task{}, err
	}
	if err := cancelPendingRequestTx(ctx, tx, task, "moved by "+actor.Principal, now); err != nil {
		return Task{}, err
	}
	// The move is the customer's decision on a paused task; enterStatusTx
	// dispatches only an unpaused one.
	if task.WorkflowPausedReason != "" {
		task.WorkflowPausedReason = ""
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET workflow_paused_reason = '' WHERE id = ?`, task.ID); err != nil {
			return Task{}, err
		}
	}
	from := task.WorkflowStatus
	if err := s.enterStatusTx(ctx, tx, &task, manifest, to, actor.Principal, "", reason); err != nil {
		return Task{}, err
	}
	if _, err := appendEventTx(ctx, tx, task, "workflow.moved", actor, map[string]any{
		"from": from, "to": to, "actor": actor.Principal, "reason": reason,
	}, now); err != nil {
		return Task{}, err
	}
	moved, err := taskByID(tx, task.ID)
	if err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	s.signal()
	moved.Access = "write"
	return moved, nil
}

// CancelWorkflowTask closes a workflow task as cancelled. Customer only.
func (s *Service) CancelWorkflowTask(ctx context.Context, actor Actor, key string) (Task, error) {
	if err := requireWorkflowOperator(actor); err != nil {
		return Task{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	task, _, err := artifactTaskTx(ctx, tx, actor, key)
	if err != nil {
		return Task{}, err
	}
	if task.Status == StatusDone || task.Status == StatusCancelled {
		return Task{}, domainError(http.StatusConflict, "workflow_closed", "task "+task.Key+" is closed")
	}
	now := s.now()
	if err := guardRevisionTx(ctx, tx, task); err != nil {
		return Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_status_visits SET left_at = ?, outcome = '', message = 'cancelled'
		WHERE task_id = ? AND left_at = ''`, now, task.ID); err != nil {
		return Task{}, err
	}
	if err := cancelPendingRequestTx(ctx, tx, task, "task cancelled by "+actor.Principal, now); err != nil {
		return Task{}, err
	}
	if err := resolveWorkflowWaitsTx(ctx, tx, task, true, true, now); err != nil {
		return Task{}, err
	}
	// The workflow status stays as the record of where the task stopped.
	task.Status, task.CompletedAt, task.WorkflowPausedReason = StatusCancelled, now, ""
	task.Revision++
	task.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = ?, completed_at = ?, workflow_paused_reason = '', revision = ?, updated_at = ?
		WHERE id = ?`, task.Status, now, task.Revision, now, task.ID); err != nil {
		return Task{}, err
	}
	if _, err := appendEventTx(ctx, tx, task, "workflow.cancelled", actor, map[string]any{
		"status": task.WorkflowStatus, "actor": actor.Principal,
	}, now); err != nil {
		return Task{}, err
	}
	cancelled, err := taskByID(tx, task.ID)
	if err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	s.signal()
	cancelled.Access = "write"
	return cancelled, nil
}

// requireWorkflowOperator admits the customer and the operator, who act as the
// customer; agents cannot override a workflow.
func requireWorkflowOperator(actor Actor) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	if !actor.IsCustomer {
		return domainError(http.StatusForbidden, "forbidden", "only the customer can override a workflow")
	}
	return nil
}

// requireOpenWorkflow refuses a closed or paused workflow task.
func requireOpenWorkflow(task Task) error {
	if task.Status == StatusDone || task.Status == StatusCancelled {
		return domainError(http.StatusConflict, "workflow_closed", "task "+task.Key+" is closed")
	}
	if task.WorkflowPausedReason != "" {
		return domainError(http.StatusConflict, "workflow_paused",
			"task "+task.Key+" is paused and waits for the customer: "+task.WorkflowPausedReason)
	}
	return nil
}

// ownsStatusTx reports whether the actor owns the task's current status: the
// holder of a pool status, the customer of a customer status, and nobody for a
// script or terminal status.
func ownsStatusTx(ctx context.Context, q queryer, task Task, manifest workflowimage.Manifest, status workflowfile.Status, actor Actor) (bool, error) {
	if status.Terminal {
		return false, nil
	}
	switch status.Owner.Kind {
	case workflowfile.OwnerPool:
		return holdsPoolStatusTx(ctx, q, task, manifest, actor)
	case workflowfile.OwnerCustomer:
		return actor.IsCustomer, nil
	default:
		return false, nil
	}
}

func statusTransition(status workflowfile.Status, outcome string) (workflowfile.Transition, bool) {
	for _, transition := range status.Transitions {
		if transition.On == outcome {
			return transition, true
		}
	}
	return workflowfile.Transition{}, false
}

// missingArtifacts lists the artifacts a transition requires that have no
// value, sorted by name.
func missingArtifacts(transition workflowfile.Transition, present map[string]bool) []string {
	var missing []string
	for _, name := range transition.Requires {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// artifactNames is the set of names in artifacts.
func artifactNames(artifacts []Artifact) map[string]bool {
	names := make(map[string]bool, len(artifacts))
	for _, artifact := range artifacts {
		names[artifact.Name] = true
	}
	return names
}

// guardRevisionTx confirms the task row still has the revision the caller
// read. Where the task was read in the same transaction it cannot fail; it is
// defence for a caller that reads the task outside the transaction.
func guardRevisionTx(ctx context.Context, tx *sql.Tx, task Task) error {
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET revision = revision WHERE id = ? AND revision = ?`,
		task.ID, task.Revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domainError(http.StatusConflict, "revision_conflict", "task was changed by another actor")
	}
	return nil
}

// cancelPendingRequestTx closes the task's pending transition request, if any,
// as cancelled.
func cancelPendingRequestTx(ctx context.Context, tx *sql.Tx, task Task, why, now string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE task_transition_requests SET state = 'cancelled', result_message = ?, finished_at = ?
		WHERE task_id = ? AND state = 'pending'`, why, now, task.ID)
	return err
}
