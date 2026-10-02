package tasks

import (
	"context"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

// artifactFixture binds DEV to a workflow declaring the artifacts "plan" and
// "summary" and creates one task, held by a developer.
func artifactFixture(t *testing.T) (*Service, Actor, Task) {
	t.Helper()
	svc, actor := workflowFixture(t)
	if err := svc.EnsureDefaultQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE agents SET enabled = 1, loop_enabled = 1, goal_enabled = 1`); err != nil {
		t.Fatal(err)
	}
	mustRebindPool(t, svc, actor, "developers", []string{"dev-1", "dev-2"}, 0)
	mustRebindPool(t, svc, actor, "reviewers", []string{"reviewer-1"}, 0)
	def := engineDefinition()
	def.Artifacts = []workflowfile.Artifact{{Name: "plan"}, {Name: "summary"}}
	seedEngineImage(t, svc, def)
	if _, err := svc.SetQueueWorkflow(context.Background(), actor, "DEV", "development:0.1.0", 0); err != nil {
		t.Fatal(err)
	}
	task := mustCreateDev(t, svc, actor, "artifacts")
	stored, err := taskByKey(svc.db, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Assignee == "" {
		t.Fatal("task has no holder")
	}
	return svc, actor, stored
}

func TestSetArtifactKeepsHistoryAndRecordsEventWithoutValue(t *testing.T) {
	svc, actor, task := artifactFixture(t)
	ctx := context.Background()
	holder := AgentActor(strings.TrimPrefix(task.Assignee, "agent:"))
	first, err := svc.SetArtifact(ctx, holder, task.Key, "plan", "first")
	if err != nil {
		t.Fatal(err)
	}
	if first.Author != task.Assignee || first.Name != "plan" || first.Value != "first" || first.CreatedAt == "" {
		t.Fatalf("first = %+v", first)
	}
	if _, err := svc.SetArtifact(ctx, actor, task.Key, "plan", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetArtifact(ctx, actor, task.Key, "summary", "done"); err != nil {
		t.Fatal(err)
	}
	list, err := svc.ListArtifacts(ctx, holder, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "plan" || list[0].Value != "second" || list[1].Name != "summary" {
		t.Fatalf("list = %+v", list)
	}
	current, history, err := svc.GetArtifact(ctx, actor, task.Key, "plan")
	if err != nil {
		t.Fatal(err)
	}
	if current.Value != "second" || len(history) != 2 || history[0].Value != "second" || history[1].Value != "first" {
		t.Fatalf("current = %+v history = %+v", current, history)
	}
	var payload string
	if err := svc.db.QueryRow(`SELECT payload FROM task_events WHERE task_id = ? AND kind = 'artifact.set' ORDER BY sequence LIMIT 1`, task.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"name":"plan"`) || !strings.Contains(payload, `"bytes":5`) ||
		!strings.Contains(payload, `"author":"agent:`) || strings.Contains(payload, "first") {
		t.Fatalf("payload = %s", payload)
	}
	stored, err := taskByKey(svc.db, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != task.Revision {
		t.Fatalf("revision %d -> %d", task.Revision, stored.Revision)
	}
}

func TestSetArtifactValidation(t *testing.T) {
	svc, actor, task := artifactFixture(t)
	ctx := context.Background()
	cases := []struct {
		name, value, code string
	}{
		{"nope", "x", "artifact_unknown"},
		{"plan", "", "invalid_artifact"},
		{"plan", "a\xffb", "invalid_artifact"},
		{"plan", strings.Repeat("x", maxArtifactBytes+1), "artifact_too_large"},
	}
	for _, c := range cases {
		if _, err := svc.SetArtifact(ctx, actor, task.Key, c.name, c.value); ErrorCode(err) != c.code {
			t.Errorf("%q: err = %v, want %s", c.name, err, c.code)
		}
	}
	if _, err := svc.SetArtifact(ctx, actor, task.Key, "plan", strings.Repeat("x", maxArtifactBytes)); err != nil {
		t.Fatalf("max size: %v", err)
	}
	if _, _, err := svc.GetArtifact(ctx, actor, task.Key, "nope"); ErrorCode(err) != "artifact_unknown" {
		t.Fatalf("get undeclared: %v", err)
	}
	if _, _, err := svc.GetArtifact(ctx, actor, task.Key, "summary"); ErrorCode(err) != "artifact_not_found" {
		t.Fatalf("get unset: %v", err)
	}
}

func TestSetArtifactAuthorization(t *testing.T) {
	svc, actor, task := artifactFixture(t)
	ctx := context.Background()
	other := "dev-1"
	if task.Assignee == "agent:dev-1" {
		other = "dev-2"
	}
	if _, err := svc.SetArtifact(ctx, AgentActor(other), task.Key, "plan", "x"); ErrorCode(err) != "not_holder" && ErrorCode(err) != "not_found" {
		t.Fatalf("non-holder: %v", err)
	}
	// In a customer status the agent still holds the task as assignee, but the
	// status is not pool-owned.
	enter(t, svc, task.Key, "approval", "ask")
	holder := AgentActor(strings.TrimPrefix(task.Assignee, "agent:"))
	if _, err := svc.SetArtifact(ctx, holder, task.Key, "plan", "x"); ErrorCode(err) != "not_holder" {
		t.Fatalf("agent in customer status: %v", err)
	}
	if _, err := svc.SetArtifact(ctx, actor, task.Key, "plan", "x"); err != nil {
		t.Fatalf("customer in customer status: %v", err)
	}
	enter(t, svc, task.Key, "done", "x")
	if _, err := svc.SetArtifact(ctx, actor, task.Key, "plan", "y"); ErrorCode(err) != "workflow_closed" {
		t.Fatalf("closed: %v", err)
	}
	if list, err := svc.ListArtifacts(ctx, actor, task.Key); err != nil || len(list) != 1 {
		t.Fatalf("list closed = %v, %v", list, err)
	}
}

func TestArtifactsOnFlexibleTask(t *testing.T) {
	svc, actor := workflowFixture(t)
	ctx := context.Background()
	task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "DEV", Title: "flex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetArtifact(ctx, actor, task.Key, "plan", "x"); ErrorCode(err) != "workflow_not_bound" {
		t.Fatalf("set: %v", err)
	}
	if _, err := svc.ListArtifacts(ctx, actor, task.Key); ErrorCode(err) != "workflow_not_bound" {
		t.Fatalf("list: %v", err)
	}
	if _, _, err := svc.GetArtifact(ctx, actor, task.Key, "plan"); ErrorCode(err) != "workflow_not_bound" {
		t.Fatalf("get: %v", err)
	}
}
