package tasks

import (
	"context"
	"encoding/json"

	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// taskSnapshot is the task.json a workflow script reads. It holds no secret
// and no environment value.
type taskSnapshot struct {
	Key         string             `json:"key"`
	Queue       string             `json:"queue"`
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Priority    Priority           `json:"priority"`
	Customer    string             `json:"customer"`
	Status      string             `json:"status"`
	Category    string             `json:"category"`
	Outcome     string             `json:"outcome"` // the request's; empty for a watch run
	Message     string             `json:"message"` // the request's; empty for a watch run
	Visit       snapshotVisit      `json:"visit"`
	Holders     map[string]string  `json:"holders"` // pool -> agent name
	Artifacts   []snapshotArtifact `json:"artifacts"`
	Workflow    snapshotWorkflow   `json:"workflow"`
}

// snapshotVisit identifies the stay in the status the run belongs to, so a
// watch script can tell a re-entered status from a restart before its outcome
// applied.
type snapshotVisit struct {
	ID        int64  `json:"id"`
	EnteredAt string `json:"entered_at"`
}

type snapshotArtifact struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

type snapshotWorkflow struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

// taskSnapshotJSON builds the snapshot of task for a run of visit visitID.
// outcome and message are the transition request's for a check.
func taskSnapshotJSON(ctx context.Context, q queryer, task Task, manifest workflowimage.Manifest, visitID int64, outcome, message string) ([]byte, error) {
	snapshot := taskSnapshot{
		Key: task.Key, Queue: task.Queue, Title: task.Title, Description: task.Description,
		Priority: task.Priority, Customer: task.Customer, Status: task.WorkflowStatus, Category: task.Category,
		Outcome: outcome, Message: message, Holders: map[string]string{}, Artifacts: []snapshotArtifact{},
		Workflow: snapshotWorkflow{Name: manifest.Name, Version: manifest.Version, Digest: manifest.Digest},
	}
	if err := q.QueryRowContext(ctx, `SELECT id, entered_at FROM task_status_visits WHERE id = ?`, visitID).
		Scan(&snapshot.Visit.ID, &snapshot.Visit.EnteredAt); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT pool, agent FROM task_workflow_holders WHERE task_id = ? AND released = 0`, task.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var pool, agent string
		if err := rows.Scan(&pool, &agent); err != nil {
			rows.Close()
			return nil, err
		}
		snapshot.Holders[pool] = agent
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	artifacts, err := currentArtifactsTx(ctx, q, task.ID)
	if err != nil {
		return nil, err
	}
	for _, artifact := range artifacts {
		snapshot.Artifacts = append(snapshot.Artifacts, snapshotArtifact{
			Name: artifact.Name, Value: artifact.Value, Author: artifact.Author, CreatedAt: artifact.CreatedAt})
	}
	return json.Marshal(snapshot)
}
