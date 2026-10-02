package tasks

import (
	"context"
	"reflect"
	"testing"
)

func TestGetWorkflowOnAFlexibleTask(t *testing.T) {
	svc, actor, _ := requestFixture(t)
	flexible, err := svc.CreateTask(context.Background(), actor, CreateTaskInput{Queue: "TASK", Title: "flex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetWorkflow(context.Background(), actor, flexible.Key); ErrorCode(err) != "workflow_not_bound" {
		t.Fatalf("flexible: %v", err)
	}
	if _, err := svc.GetWorkflow(context.Background(), actor, "DEV-nope"); ErrorCode(err) != "not_found" {
		t.Fatalf("unknown: %v", err)
	}
}

func TestGetWorkflowInAPoolStatus(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	holder := AgentActor("dev-1")
	view, err := svc.GetWorkflow(ctx, holder, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if view.Name != "development" || view.Version != "0.1.0" || view.Digest != task.WorkflowDigest ||
		view.Status != "develop" || view.Category != StatusInProgress || view.WaitingOn != "" ||
		view.Owner != "pool:developers" || view.Holder != "agent:dev-1" || view.InstructionsPath != "" ||
		view.LastRequest != nil || len(view.Artifacts) != 0 || len(view.Visits) != 1 || view.Visits[0].Status != "develop" {
		t.Fatalf("view = %#v", view)
	}
	want := []OutcomeView{
		{On: "ready", To: "review", Requires: []string{"plan", "summary"}, Missing: []string{"plan", "summary"}},
		{On: "ask", To: "approval"},
	}
	if !reflect.DeepEqual(view.Outcomes, want) {
		t.Fatalf("outcomes = %#v", view.Outcomes)
	}
	if _, err := svc.SetArtifact(ctx, holder, task.Key, "summary", "s"); err != nil {
		t.Fatal(err)
	}
	view, err = svc.GetWorkflow(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(view.Outcomes[0].Missing, []string{"plan"}) || len(view.Artifacts) != 1 || view.Artifacts[0].Name != "summary" {
		t.Fatalf("after summary = %#v", view)
	}

	// The holder's question to the customer waits on the customer.
	if _, err := svc.AddComment(ctx, holder, task.Key, AddCommentInput{Body: "@user:customer which API?"}); err != nil {
		t.Fatal(err)
	}
	view, err = svc.GetWorkflow(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if view.Category != StatusWaitCustomer || view.WaitingOn != WaitingOnCustomer || view.Holder != "agent:dev-1" {
		t.Fatalf("after question = %#v", view)
	}
}

func TestGetWorkflowAfterTransitions(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	holder := AgentActor("dev-1")
	setPlanAndSummary(t, svc, holder, task.Key)
	request, err := svc.Advance(ctx, holder, task.Key, AdvanceInput{Outcome: "ready", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	view, err := svc.GetWorkflow(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if view.Owner != "pool:reviewers" || view.Holder != "agent:reviewer-1" || view.LastRequest == nil ||
		*view.LastRequest != request || len(view.Visits) != 2 || view.Visits[0].Outcome != "ready" ||
		view.Visits[1].Status != "review" || len(view.Artifacts) != 2 || view.Artifacts[0].Name != "plan" {
		t.Fatalf("view = %#v", view)
	}
	if !reflect.DeepEqual(view.Outcomes[0].Checks, []string{"checks/ci.sh"}) {
		t.Fatalf("outcomes = %#v", view.Outcomes)
	}

	enter(t, svc, task.Key, "approval", "approve")
	view, err = svc.GetWorkflow(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if view.Owner != "customer" || view.Holder != "" || view.Category != StatusWaitCustomer ||
		view.WaitingOn != WaitingOnCustomer || view.InstructionsPath != "statuses/approval.md" {
		t.Fatalf("customer view = %#v", view)
	}

	enter(t, svc, task.Key, "merge", "approved")
	view, err = svc.GetWorkflow(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if view.Owner != "script" || view.Holder != "" || view.WaitingOn != WaitingOnScript {
		t.Fatalf("script view = %#v", view)
	}

	enter(t, svc, task.Key, "done", "merged")
	view, err = svc.GetWorkflow(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if view.Owner != "" || view.Holder != "" || view.Category != StatusDone || view.Outcomes == nil || len(view.Outcomes) != 0 {
		t.Fatalf("terminal view = %#v", view)
	}
}
