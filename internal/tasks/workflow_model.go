package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// StatusVisit is one stay of a task in a workflow status.
type StatusVisit struct {
	ID        int64  `json:"id"`
	Sequence  int64  `json:"sequence"`
	Status    string `json:"status"`
	EnteredAt string `json:"entered_at"`
	EnteredBy string `json:"entered_by"`
	LeftAt    string `json:"left_at,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	Message   string `json:"message,omitempty"`
}

// TransitionRequest is a request to leave a status with an outcome.
type TransitionRequest struct {
	ID            int64  `json:"id"`
	TaskKey       string `json:"task_key"`
	Outcome       string `json:"outcome"`
	Message       string `json:"message,omitempty"`
	Actor         string `json:"actor"`
	State         string `json:"state"`
	ResultMessage string `json:"result_message,omitempty"`
	CreatedAt     string `json:"created_at"`
	FinishedAt    string `json:"finished_at,omitempty"`
	// WaitSeconds is how long a caller may wait for a pending request: the
	// timeouts of the transition's checks plus a margin. It is computed from
	// the pinned manifest, never stored, and set only while pending.
	WaitSeconds int `json:"wait_seconds,omitempty"`
}

// Artifact is one stored version of a named task artifact.
type Artifact struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Value     string `json:"value"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

// QueueWorkflow is the workflow image a queue is bound to.
type QueueWorkflow struct {
	Queue     string `json:"queue"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Digest    string `json:"digest"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at"`
}

// TaskWorkflowColumns follow the stored task columns in every task select and
// need TaskWorkflowJoin. The last column is the owner kind of the current
// status, read from the image manifest only for an unpaused workflow task in
// wait_customer, so a list query never opens a manifest it does not need.
const TaskWorkflowColumns = `
       COALESCE(t.workflow_digest, ''), COALESCE(t.workflow_status, ''), t.workflow_paused_reason,
       COALESCE(wi.name, ''), COALESCE(wi.version, ''),
       CASE WHEN t.status = 'wait_customer' AND t.workflow_paused_reason = '' THEN COALESCE((
         SELECT json_extract(s.value, '$.owner.kind')
         FROM json_each(wi.manifest, '$.definition.statuses') s
         WHERE json_extract(s.value, '$.id') = t.workflow_status
       ), '') ELSE '' END`

// TaskWorkflowJoin joins the image of a workflow task.
const TaskWorkflowJoin = `
LEFT JOIN task_workflow_images wi ON wi.digest = t.workflow_digest`

// WorkflowScanTargets returns the scan destinations for TaskWorkflowColumns and
// a function to call after the scan, which derives Category and WaitingOn. It
// lets another package keep its own task select in step with scanTask.
func WorkflowScanTargets(t *Task) ([]any, func()) {
	var owner string
	dest := []any{&t.WorkflowDigest, &t.WorkflowStatus, &t.WorkflowPausedReason, &t.WorkflowName, &t.WorkflowVersion, &owner}
	return dest, func() { finishWorkflowFields(t, owner) }
}

func finishWorkflowFields(t *Task, ownerKind string) {
	t.Category = t.Status
	if t.WorkflowDigest == "" || t.Status != StatusWaitCustomer {
		return
	}
	switch {
	case t.WorkflowPausedReason != "":
		t.WaitingOn = WaitingOnPause
	case ownerKind == workflowfile.OwnerCustomer, ownerKind == workflowfile.OwnerPool:
		// In a pool status the holder's question to the customer is the wait.
		t.WaitingOn = WaitingOnCustomer
	case ownerKind == workflowfile.OwnerScript:
		t.WaitingOn = WaitingOnScript
	}
}

// loadManifestTx reads a published manifest by digest from task_workflow_images.
func loadManifestTx(ctx context.Context, q queryer, digest string) (workflowimage.Manifest, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT manifest FROM task_workflow_images WHERE digest = ?`, digest).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return workflowimage.Manifest{}, domainError(http.StatusConflict, "workflow_not_found", "workflow image "+digest+" is not published")
	}
	if err != nil {
		return workflowimage.Manifest{}, err
	}
	var m workflowimage.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return workflowimage.Manifest{}, err
	}
	return m, nil
}
