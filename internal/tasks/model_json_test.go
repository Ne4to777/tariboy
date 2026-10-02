package tasks

import (
	"encoding/json"
	"testing"
)

func TestTaskUnmarshalDecidesFromThePayloadNotTheReceiver(t *testing.T) {
	workflow := `{"key":"DEV-1","status":"develop","category":"in_progress","workflow_digest":"d1",` +
		`"workflow_name":"flow","workflow_version":"1.0.0","workflow_paused_reason":"x","waiting_on":"pause"}`
	flexible := `{"key":"DEV-2","status":"open","category":"open"}`

	var task Task
	if err := json.Unmarshal([]byte(workflow), &task); err != nil {
		t.Fatal(err)
	}
	if task.WorkflowStatus != "develop" || task.Status != StatusInProgress || task.WorkflowDigest != "d1" {
		t.Fatalf("workflow decode = %#v", task)
	}
	// Reuse the value for a flexible task: nothing of the workflow may survive.
	if err := json.Unmarshal([]byte(flexible), &task); err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusOpen || task.Category != StatusOpen || task.WorkflowStatus != "" ||
		task.WorkflowDigest != "" || task.WorkflowName != "" || task.WorkflowVersion != "" ||
		task.WorkflowPausedReason != "" || task.WaitingOn != "" {
		t.Fatalf("flexible decode into a reused task = %#v", task)
	}
	// And back: a workflow payload decoded into a flexible receiver.
	if err := json.Unmarshal([]byte(workflow), &task); err != nil {
		t.Fatal(err)
	}
	if task.WorkflowStatus != "develop" || task.Status != StatusInProgress {
		t.Fatalf("workflow decode into a reused task = %#v", task)
	}
}

func TestTaskUnmarshalWithoutStatusKeepsStatus(t *testing.T) {
	task := Task{Status: StatusInProgress, Category: StatusInProgress}
	if err := json.Unmarshal([]byte(`{"key":"DEV-3","title":"t"}`), &task); err != nil {
		t.Fatal(err)
	}
	if task.Key != "DEV-3" || task.Status != StatusInProgress || task.Category != StatusInProgress {
		t.Fatalf("decode without status = %#v", task)
	}
}
