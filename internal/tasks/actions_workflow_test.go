package tasks

import (
	"context"
	"testing"
)

func TestAgentWorkflowActionsUseTheSocketIdentityNotTheBody(t *testing.T) {
	svc, _, task := requestFixture(t)
	ctx := context.Background()
	holder, other := AgentActor("dev-1"), AgentActor("dev-2")
	// dev-2 may read the task (queue owner) but does not hold it, so the
	// refusal below is not_holder rather than mere invisibility.
	queue, err := svc.GetQueue(ctx, CustomerActor("customer"), "DEV")
	if err != nil {
		t.Fatal(err)
	}
	owners := append(append([]string{}, queue.Owners...), "dev-2")
	if _, err := svc.UpdateQueue(ctx, CustomerActor("customer"), "DEV", UpdateQueueInput{Owners: &owners, Revision: queue.Revision}); err != nil {
		t.Fatal(err)
	}
	forged := map[string]any{"key": task.Key, "name": "plan", "value": "p", "actor": "agent:dev-1",
		"author": "agent:dev-1", "principal": "agent:dev-1", "customer": "customer"}

	// A caller that is not the holder cannot pass itself off as the holder.
	if _, err := svc.AgentAction(ctx, other, "artifact_set", forged); ErrorCode(err) != "not_holder" {
		t.Fatalf("forged artifact_set error = %v; want not_holder", err)
	}
	if _, err := svc.AgentAction(ctx, other, "advance", map[string]any{
		"key": task.Key, "outcome": "ready", "actor": "agent:dev-1"}); ErrorCode(err) != "not_holder" {
		t.Fatalf("forged advance error = %v; want not_holder", err)
	}

	got, err := svc.AgentAction(ctx, holder, "artifact_set", forged)
	if err != nil {
		t.Fatal(err)
	}
	artifact, ok := got.(Artifact)
	if !ok || artifact.Name != "plan" || artifact.Value != "p" || artifact.Author != "agent:dev-1" {
		t.Fatalf("artifact_set = %#v", got)
	}
}

func TestAgentWorkflowActionsReadAndAdvance(t *testing.T) {
	svc, _, task := requestFixture(t)
	ctx := context.Background()
	holder := AgentActor("dev-1")
	// The value is stored as given: no trimming of the trailing newline.
	if _, err := svc.AgentAction(ctx, holder, "artifact_set", map[string]any{"key": task.Key, "name": "plan", "value": "step 1\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AgentAction(ctx, holder, "artifact_set", map[string]any{"key": task.Key, "name": "summary", "value": "s"}); err != nil {
		t.Fatal(err)
	}
	listed, err := svc.AgentAction(ctx, holder, "artifact_ls", map[string]any{"key": task.Key})
	if err != nil {
		t.Fatal(err)
	}
	list := listed.(map[string]any)
	if list["count"] != 2 || len(list["artifacts"].([]Artifact)) != 2 {
		t.Fatalf("artifact_ls = %#v", listed)
	}
	shown, err := svc.AgentAction(ctx, holder, "artifact_show", map[string]any{"key": task.Key, "name": "plan"})
	if err != nil {
		t.Fatal(err)
	}
	show := shown.(map[string]any)
	if show["artifact"].(Artifact).Value != "step 1\n" || len(show["history"].([]Artifact)) != 1 {
		t.Fatalf("artifact_show = %#v", shown)
	}
	view, err := svc.AgentAction(ctx, holder, "workflow_get", map[string]any{"key": task.Key})
	if err != nil {
		t.Fatal(err)
	}
	if v := view.(WorkflowView); v.Status != "develop" || v.Holder != "agent:dev-1" {
		t.Fatalf("workflow_get = %#v", view)
	}
	if _, err := svc.AgentAction(ctx, holder, "advance", map[string]any{"key": task.Key, "outcome": "ready", "from": "review"}); ErrorCode(err) != "status_changed" {
		t.Fatalf("advance with a stale from: %v", err)
	}
	advanced, err := svc.AgentAction(ctx, holder, "advance", map[string]any{"key": task.Key, "outcome": "ready", "message": "done", "from": "develop"})
	if err != nil {
		t.Fatal(err)
	}
	if r := advanced.(TransitionRequest); r.State != "applied" || r.Outcome != "ready" || r.Message != "done" {
		t.Fatalf("advance = %#v", advanced)
	}
}

func TestAgentActionRefusesOperatorOnlyWorkflowActions(t *testing.T) {
	svc, _, task := requestFixture(t)
	for _, action := range []string{"workflow_move", "workflow_resume", "cancel", "queue_workflow_set", "queue_workflow_get", "queue_workflow_clear"} {
		_, err := svc.AgentAction(context.Background(), AgentActor("dev-1"), action,
			map[string]any{"key": task.Key, "to": "review", "reason": "x"})
		if ErrorCode(err) != "invalid_action" {
			t.Errorf("%s error = %v; want invalid_action", action, err)
		}
	}
}
