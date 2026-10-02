package tasks

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

func seedWorkflowImage(t *testing.T, svc *Service, digest string) workflowimage.Manifest {
	t.Helper()
	m := workflowimage.Manifest{
		SchemaVersion: 1, Name: "dev-flow", Version: "1.2.0", Digest: digest, BuiltAt: "2026-10-02T00:00:00Z",
		Definition: workflowfile.File{
			Name: "dev-flow", WorkflowVersion: "1.2.0", InitialStatus: "build",
			Statuses: []workflowfile.Status{
				{ID: "build", Owner: workflowfile.Owner{Kind: workflowfile.OwnerPool, Pool: "devs"}},
				{ID: "approve", Owner: workflowfile.Owner{Kind: workflowfile.OwnerCustomer}},
				{ID: "verify", Owner: workflowfile.Owner{Kind: workflowfile.OwnerScript}},
				{ID: "shipped", Terminal: true},
			},
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO task_workflow_images(digest, name, version, manifest, built_at) VALUES (?, ?, ?, ?, ?)`,
		digest, m.Name, m.Version, string(raw), m.BuiltAt); err != nil {
		t.Fatal(err)
	}
	return m
}

// seedWorkflowTask creates a flexible task and rewrites its row into a workflow
// task, because nothing in the engine sets workflow_digest yet.
func seedWorkflowTask(t *testing.T, svc *Service, actor Actor, digest, category, status, paused string) Task {
	t.Helper()
	task, err := svc.CreateTask(context.Background(), actor, CreateTaskInput{Queue: "DEV", Title: "wf " + status})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE tasks SET status = ?, workflow_digest = ?, workflow_status = ?, workflow_paused_reason = ? WHERE id = ?`,
		category, digest, status, paused, task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := taskByID(svc.db, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func jsonFields(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTaskMarshalFlexibleTaskReportsStatusAsCategory(t *testing.T) {
	svc, actor := workflowFixture(t)
	task, err := svc.CreateTask(context.Background(), actor, CreateTaskInput{Queue: "DEV", Title: "flex"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := taskByID(svc.db, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOpen || got.Category != StatusOpen || got.WorkflowDigest != "" || got.WaitingOn != "" {
		t.Fatalf("scanned flexible task = %#v", got)
	}
	f := jsonFields(t, got)
	if f["status"] != "open" || f["category"] != "open" {
		t.Fatalf("status/category = %v/%v; want open/open", f["status"], f["category"])
	}
	for _, absent := range []string{"waiting_on", "workflow_digest", "workflow_name", "workflow_version", "workflow_paused_reason"} {
		if _, ok := f[absent]; ok {
			t.Fatalf("flexible task JSON has %s", absent)
		}
	}
}

func TestTaskScanFillsWorkflowFieldsAndWaitingOn(t *testing.T) {
	svc, actor := workflowFixture(t)
	const digest = "d1"
	seedWorkflowImage(t, svc, digest)
	cases := []struct {
		name, category, status, paused string
		wantWaiting                    string
	}{
		{"pool without holder", StatusOpen, "build", "", ""},
		{"pool with holder", StatusInProgress, "build", "", ""},
		{"pool holder question", StatusWaitCustomer, "build", "", WaitingOnCustomer},
		{"customer", StatusWaitCustomer, "approve", "", WaitingOnCustomer},
		{"script", StatusWaitCustomer, "verify", "", WaitingOnScript},
		{"paused", StatusWaitCustomer, "approve", "operator hold", WaitingOnPause},
		{"unknown status", StatusWaitCustomer, "gone", "", ""},
		{"terminal", StatusDone, "shipped", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := seedWorkflowTask(t, svc, actor, digest, tc.category, tc.status, tc.paused)
			if got.Status != tc.category || got.Category != tc.category || got.WorkflowStatus != tc.status {
				t.Fatalf("status/category/workflow = %q/%q/%q", got.Status, got.Category, got.WorkflowStatus)
			}
			if got.WorkflowDigest != digest || got.WorkflowName != "dev-flow" || got.WorkflowVersion != "1.2.0" {
				t.Fatalf("workflow identity = %q/%q/%q", got.WorkflowDigest, got.WorkflowName, got.WorkflowVersion)
			}
			if got.WaitingOn != tc.wantWaiting || got.WorkflowPausedReason != tc.paused {
				t.Fatalf("waiting_on/paused = %q/%q; want %q/%q", got.WaitingOn, got.WorkflowPausedReason, tc.wantWaiting, tc.paused)
			}
			f := jsonFields(t, got)
			if f["status"] != tc.status || f["category"] != tc.category {
				t.Fatalf("JSON status/category = %v/%v; want %s/%s", f["status"], f["category"], tc.status, tc.category)
			}
			if tc.wantWaiting == "" {
				if _, ok := f["waiting_on"]; ok {
					t.Fatal("JSON has waiting_on")
				}
			} else if f["waiting_on"] != tc.wantWaiting {
				t.Fatalf("JSON waiting_on = %v", f["waiting_on"])
			}
			if f["workflow_name"] != "dev-flow" || f["workflow_version"] != "1.2.0" || f["workflow_digest"] != digest {
				t.Fatalf("JSON workflow identity = %v", f)
			}
		})
	}
}

func TestTaskJSONRoundTrip(t *testing.T) {
	svc, actor := workflowFixture(t)
	seedWorkflowImage(t, svc, "d1")
	flexible, err := svc.CreateTask(context.Background(), actor, CreateTaskInput{Queue: "DEV", Title: "flex"})
	if err != nil {
		t.Fatal(err)
	}
	flexible, err = taskByID(svc.db, flexible.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []Task{
		flexible,
		seedWorkflowTask(t, svc, actor, "d1", StatusWaitCustomer, "approve", ""),
		seedWorkflowTask(t, svc, actor, "d1", StatusInProgress, "build", ""),
	} {
		raw, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		var back Task
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		task.ID = 0 // not part of the JSON form
		if !reflect.DeepEqual(back, task) {
			t.Fatalf("round trip\n got %#v\nwant %#v", back, task)
		}
	}
}

func TestLoadManifestTx(t *testing.T) {
	svc, _ := workflowFixture(t)
	want := seedWorkflowImage(t, svc, "d1")
	got, err := loadManifestTx(context.Background(), svc.db, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != want.Name || got.Digest != "d1" || len(got.Definition.Statuses) != 4 {
		t.Fatalf("manifest = %#v", got)
	}
	_, err = loadManifestTx(context.Background(), svc.db, "missing")
	e, ok := err.(*Error)
	if !ok || e.Code != "workflow_not_found" || e.Status != 409 {
		t.Fatalf("missing digest error = %#v; want workflow_not_found 409", err)
	}
}
