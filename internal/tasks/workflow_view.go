package tasks

import (
	"context"
	"database/sql"
	"strings"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

// OutcomeView is one transition of the current status as the task shows it.
type OutcomeView struct {
	On       string   `json:"on"`
	To       string   `json:"to"`
	Requires []string `json:"requires,omitempty"`
	Missing  []string `json:"missing,omitempty"` // required artifacts with no value
	Checks   []string `json:"checks,omitempty"`  // script paths
}

// WorkflowView is the read model of one workflow task: where it is, who owns
// it, what it may do next, and its history.
type WorkflowView struct {
	Name             string             `json:"name"`
	Version          string             `json:"version"`
	Digest           string             `json:"digest"`
	Status           string             `json:"status"`
	Category         string             `json:"category"`
	WaitingOn        string             `json:"waiting_on,omitempty"`
	PausedReason     string             `json:"paused_reason,omitempty"`     // set while the task waits for the customer's decision
	Owner            string             `json:"owner"`                       // "pool:<name>", "customer", "script", or "" for terminal
	Holder           string             `json:"holder,omitempty"`            // "agent:<name>"
	InstructionsPath string             `json:"instructions_path,omitempty"` // source-relative path inside the image
	Outcomes         []OutcomeView      `json:"outcomes"`
	Artifacts        []Artifact         `json:"artifacts"`
	Visits           []StatusVisit      `json:"visits"`
	LastRequest      *TransitionRequest `json:"last_request,omitempty"`
	Runs             []ScriptRun        `json:"runs"` // the most recent script runs, newest first
}

// GetWorkflow returns the workflow view of a task the actor may read.
func (s *Service) GetWorkflow(ctx context.Context, actor Actor, key string) (WorkflowView, error) {
	if err := validateActor(actor); err != nil {
		return WorkflowView{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowView{}, err
	}
	defer tx.Rollback()
	task, manifest, err := workflowReadTaskTx(ctx, tx, actor, key)
	if err != nil {
		return WorkflowView{}, err
	}
	view := WorkflowView{
		Name: manifest.Name, Version: manifest.Version, Digest: manifest.Digest,
		Status: task.WorkflowStatus, Category: task.Category, WaitingOn: task.WaitingOn, PausedReason: task.WorkflowPausedReason,
		Outcomes: []OutcomeView{},
	}
	if view.Artifacts, err = currentArtifactsTx(ctx, tx, task.ID); err != nil {
		return WorkflowView{}, err
	}
	present := artifactNames(view.Artifacts)
	// A closed task has no owner and no exit: every advance would be refused
	// with workflow_closed.
	closed := task.Status == StatusDone || task.Status == StatusCancelled
	if status, ok := currentStatus(manifest, task.WorkflowStatus); ok && !closed {
		switch {
		case status.Terminal:
		case status.Owner.Kind == workflowfile.OwnerPool:
			view.Owner = "pool:" + status.Owner.Pool
			view.Holder = task.Assignee
		default:
			view.Owner = status.Owner.Kind
		}
		view.InstructionsPath = strings.TrimPrefix(status.Instructions, "./")
		for _, transition := range status.Transitions {
			outcome := OutcomeView{On: transition.On, To: transition.To, Requires: transition.Requires,
				Missing: missingArtifacts(transition, present)}
			for _, check := range transition.Checks {
				outcome.Checks = append(outcome.Checks, check.Script)
			}
			view.Outcomes = append(view.Outcomes, outcome)
		}
	}
	if view.Visits, err = statusVisitsTx(ctx, tx, task.ID); err != nil {
		return WorkflowView{}, err
	}
	if view.Runs, err = scriptRunsTx(ctx, tx, task.ID, workflowViewRuns); err != nil {
		return WorkflowView{}, err
	}
	var last TransitionRequest
	err = tx.QueryRowContext(ctx, `
		SELECT id, outcome, message, actor, state, result_message, created_at, finished_at
		FROM task_transition_requests WHERE task_id = ? ORDER BY id DESC LIMIT 1`, task.ID).Scan(
		&last.ID, &last.Outcome, &last.Message, &last.Actor, &last.State, &last.ResultMessage, &last.CreatedAt, &last.FinishedAt)
	switch {
	case err == nil:
		last.TaskKey = task.Key
		view.LastRequest = &last
	case err != sql.ErrNoRows:
		return WorkflowView{}, err
	}
	return view, nil
}

// statusVisitsTx returns every visit of a task, oldest first.
func statusVisitsTx(ctx context.Context, q queryer, taskID int64) ([]StatusVisit, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, sequence, status_id, entered_at, entered_by, left_at, outcome, message
		FROM task_status_visits WHERE task_id = ? ORDER BY sequence`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	visits := []StatusVisit{}
	for rows.Next() {
		var v StatusVisit
		if err := rows.Scan(&v.ID, &v.Sequence, &v.Status, &v.EnteredAt, &v.EnteredBy, &v.LeftAt, &v.Outcome, &v.Message); err != nil {
			return nil, err
		}
		visits = append(visits, v)
	}
	return visits, rows.Err()
}

// inPoolStatusTx reports whether a workflow task's current status is owned by
// a pool.
func inPoolStatusTx(ctx context.Context, q queryer, task Task) (bool, error) {
	manifest, err := loadManifestTx(ctx, q, task.WorkflowDigest)
	if err != nil {
		return false, err
	}
	status, ok := currentStatus(manifest, task.WorkflowStatus)
	return ok && !status.Terminal && status.Owner.Kind == workflowfile.OwnerPool, nil
}
