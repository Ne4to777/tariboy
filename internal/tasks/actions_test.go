package tasks

import (
	"context"
	"testing"
)

func TestAgentActionBindsAuthorAndEnforcesQueueAccess(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "OWN", Name: "Owned", Owners: []string{"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.AgentAction(ctx, AgentActor("alice"), "create", map[string]any{
		"queue": "OWN", "title": "agent work", "author": "user:forged",
	})
	if err != nil {
		t.Fatal(err)
	}
	created, ok := result.(Task)
	if !ok {
		t.Fatalf("result type = %T; want Task", result)
	}
	if created.Author != "agent:alice" {
		t.Fatalf("author = %q; want agent:alice", created.Author)
	}
	owned, err := svc.AgentAction(ctx, AgentActor("alice"), "create", map[string]any{
		"queue": "OWN", "title": "owner keeps control", "assignee": "alice", "group": "team",
	})
	if err != nil {
		t.Fatalf("owner create with assignee and group: %v", err)
	}
	if task := owned.(Task); task.Assignee != "agent:alice" || task.Group != "team" {
		t.Fatalf("owner create assignee/group = %q/%q; want agent:alice/team", task.Assignee, task.Group)
	}
}

func TestAgentAskSeparatesMentionFromMarkdownBody(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.CreateQueue(ctx, CustomerActor("customer"), CreateQueueInput{
		Prefix: "ASK", Name: "Questions", Owners: []string{"alice"},
	}); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, AgentActor("alice"), CreateTaskInput{Queue: "ASK", Title: "Question"})
	if err != nil {
		t.Fatal(err)
	}

	result, err := svc.AgentAction(ctx, AgentActor("alice"), "ask", map[string]any{
		"key": task.Key, "principal": "user:customer", "body": "## Release workflow failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	comment := result.(CommentResult)
	if got := comment.Comment.Body; got != "@user:customer\n\n## Release workflow failed" {
		t.Fatalf("comment body = %q", got)
	}
	if len(comment.CreatedWaits) != 1 || comment.CreatedWaits[0].ExpectedPrincipal != "user:customer" {
		t.Fatalf("created waits = %#v", comment.CreatedWaits)
	}
}

// A non-owner agent may file a report into any existing queue, but filing is not a way to
// give itself work or a window into the queue: the report is unassigned, ungrouped, and
// invisible to its author afterwards. Triage belongs to whoever owns the queue.
func TestAgentActionFilesUnassignedReportWithoutQueueVisibility(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "OWN", Name: "Owned", Owners: []string{"alice"},
	}); err != nil {
		t.Fatal(err)
	}
	unrelated, err := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "OWN", Title: "customer work"})
	if err != nil {
		t.Fatal(err)
	}

	bob := AgentActor("bob")
	result, err := svc.AgentAction(ctx, bob, "create", map[string]any{
		"queue": "OWN", "title": "lint is red on main", "description": "pre-existing defect",
	})
	if err != nil {
		t.Fatalf("non-owner root create: %v", err)
	}
	report, ok := result.(Task)
	if !ok {
		t.Fatalf("result type = %T; want Task", result)
	}
	if report.Author != "agent:bob" {
		t.Fatalf("author = %q; want agent:bob", report.Author)
	}
	if report.Assignee != "" || report.Group != "" {
		t.Fatalf("report assignee/group = %q/%q; want both empty", report.Assignee, report.Group)
	}
	if report.Access != "" {
		t.Fatalf("report access = %q; want empty, the author keeps no write access", report.Access)
	}
	if !report.Filed {
		t.Fatal("report Filed = false; want true so the CLI can explain the task is gone from view")
	}

	if _, err := svc.GetTask(ctx, bob, report.Key); ErrorCode(err) != "not_found" {
		t.Fatalf("author reading own report: %v; want not_found", err)
	}
	page, err := svc.ListTasks(ctx, bob, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range page.Tasks {
		if task.Key == report.Key {
			t.Fatal("filed report is still listed for its author")
		}
		if task.Key == unrelated.Key {
			t.Fatal("unrelated queue task leaked to a non-owner agent")
		}
	}
	if _, err := svc.GetTask(ctx, customer, report.Key); err != nil {
		t.Fatalf("customer reading the report: %v", err)
	}

	if _, err := svc.AgentAction(ctx, bob, "create", map[string]any{
		"queue": "OWN", "title": "group scoped", "group": "team",
	}); ErrorCode(err) != "report_cannot_assign" {
		t.Fatalf("group-scoped report error = %v; want report_cannot_assign", err)
	}

	queue, err := svc.GetQueue(ctx, customer, "OWN")
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Owners) != 1 || queue.Owners[0] != "alice" {
		t.Fatalf("queue owners = %v; want [alice]", queue.Owners)
	}
}

func TestAgentActionAssignsCrossQueueRootWithoutQueueVisibility(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "OWN", Name: "Owned", Owners: []string{"alice"},
	}); err != nil {
		t.Fatal(err)
	}
	unrelated, err := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "OWN", Title: "customer work"})
	if err != nil {
		t.Fatal(err)
	}

	bob := AgentActor("bob")
	result, err := svc.AgentAction(ctx, bob, "create", map[string]any{
		"queue": "OWN", "title": "review the change", "assignee": "bob",
	})
	if err != nil {
		t.Fatalf("cross-queue assigned create: %v", err)
	}
	assigned := result.(Task)
	if assigned.Assignee != "agent:bob" || assigned.Access != "write" || assigned.Filed {
		t.Fatalf("assigned task assignee/access/filed = %q/%q/%v; want agent:bob/write/false",
			assigned.Assignee, assigned.Access, assigned.Filed)
	}
	if _, err := svc.GetTask(ctx, bob, assigned.Key); err != nil {
		t.Fatalf("assignee reading assigned root: %v", err)
	}
	if _, err := svc.GetTask(ctx, bob, unrelated.Key); ErrorCode(err) != "not_found" {
		t.Fatalf("assignee reading unrelated queue task: %v; want not_found", err)
	}
}

func TestAgentActionCrossQueueAssignmentReportsCreatorAccessAccurately(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "OWN", Name: "Owned", Owners: []string{"alice"},
	}); err != nil {
		t.Fatal(err)
	}
	unrelated, err := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "OWN", Title: "customer work"})
	if err != nil {
		t.Fatal(err)
	}

	bob := AgentActor("bob")
	body := map[string]any{
		"queue": "OWN", "title": "review the change", "assignee": "carol",
		"idempotency_key": "assign-carol",
	}
	result, err := svc.AgentAction(ctx, bob, "create", body)
	if err != nil {
		t.Fatalf("cross-queue assigned create: %v", err)
	}
	assigned := result.(Task)
	if assigned.Assignee != "agent:carol" || assigned.Access != "" || assigned.Filed {
		t.Fatalf("assigned task assignee/access/filed = %q/%q/%v; want agent:carol/empty/false",
			assigned.Assignee, assigned.Access, assigned.Filed)
	}
	if _, err := svc.GetTask(ctx, bob, assigned.Key); ErrorCode(err) != "not_found" {
		t.Fatalf("creator reading task assigned to another agent: %v; want not_found", err)
	}
	if _, err := svc.GetTask(ctx, AgentActor("carol"), assigned.Key); err != nil {
		t.Fatalf("assignee reading assigned root: %v", err)
	}
	if _, err := svc.GetTask(ctx, AgentActor("carol"), unrelated.Key); ErrorCode(err) != "not_found" {
		t.Fatalf("assignee reading unrelated queue task: %v; want not_found", err)
	}

	replayed, err := svc.AgentAction(ctx, bob, "create", body)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if got := replayed.(Task); got.Key != assigned.Key || got.Access != "" {
		t.Fatalf("idempotent replay key/access = %q/%q; want %q/empty", got.Key, got.Access, assigned.Key)
	}
}

func TestAgentActionAppliesPriority(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "PRIO", Name: "Priorities", Owners: []string{"alice"}}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.AgentAction(ctx, AgentActor("alice"), "create", map[string]any{
		"queue": "PRIO", "title": "urgent", "priority": "P1",
	})
	if err != nil {
		t.Fatal(err)
	}
	created := result.(Task)
	if created.Priority != PriorityP1 {
		t.Fatalf("created priority = %q; want P1", created.Priority)
	}

	result, err = svc.AgentAction(ctx, AgentActor("alice"), "create", map[string]any{
		"queue": "PRIO", "title": "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	defaulted := result.(Task)
	if defaulted.Priority != PriorityP2 {
		t.Fatalf("defaulted priority = %q; want P2", defaulted.Priority)
	}

	result, err = svc.AgentAction(ctx, AgentActor("alice"), "update", map[string]any{
		"key": created.Key, "priority": "P0",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated := result.(Task)
	if updated.Priority != PriorityP0 {
		t.Fatalf("updated priority = %q; want P0", updated.Priority)
	}

	result, err = svc.AgentAction(ctx, AgentActor("alice"), "update", map[string]any{
		"key": updated.Key, "status": StatusInProgress,
	})
	if err != nil {
		t.Fatal(err)
	}
	unchanged := result.(Task)
	if unchanged.Priority != PriorityP0 {
		t.Fatalf("status-only update priority = %q; want P0", unchanged.Priority)
	}

	if _, err := svc.AgentAction(ctx, AgentActor("alice"), "update", map[string]any{
		"key": unchanged.Key, "priority": "urgent",
	}); ErrorCode(err) != "invalid_priority" {
		t.Fatalf("invalid priority error = %v; want invalid_priority", err)
	}
}

func TestAgentActionCreateAndUpdatePreservePullRequestAndWaitCustomer(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := AgentActor("alice")
	if _, err := svc.CreateQueue(ctx, CustomerActor("customer"), CreateQueueInput{
		Prefix: "PR", Name: "Pull requests", Owners: []string{"alice"},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := svc.AgentAction(ctx, actor, "create", map[string]any{
		"queue": "PR", "title": "Expose PR", "pull_request": " HTTPS://Example.test/o/r/pull/6 ",
	})
	if err != nil {
		t.Fatal(err)
	}
	created := result.(Task)
	if created.PullRequest != "https://example.test/o/r/pull/6" {
		t.Fatalf("created task = %#v", created)
	}
	result, err = svc.AgentAction(ctx, actor, "update", map[string]any{
		"key": created.Key, "pull_request": "https://github.com/o/r/pull/7", "status": StatusWaitCustomer,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated := result.(Task)
	if updated.PullRequest != "https://github.com/o/r/pull/7" || updated.Status != StatusWaitCustomer {
		t.Fatalf("updated task = %#v", updated)
	}
}

func TestAgentActionUpdateClearsExplicitManualBlockReason(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "BLOCK", Name: "Blocks", Owners: []string{"alice"}}); err != nil {
		t.Fatal(err)
	}
	created, err := svc.CreateTask(ctx, AgentActor("alice"), CreateTaskInput{Queue: "BLOCK", Title: "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.AgentAction(ctx, AgentActor("alice"), "update", map[string]any{
		"key": created.Key, "manual_block_reason": "waiting for review",
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked := result.(Task)
	detail, err := svc.GetTask(ctx, AgentActor("alice"), blocked.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Task.Blocked {
		t.Fatal("updated task is not blocked")
	}
	result, err = svc.AgentAction(ctx, AgentActor("alice"), "update", map[string]any{"key": blocked.Key})
	if err != nil {
		t.Fatal(err)
	}
	unchanged := result.(Task)
	if unchanged.ManualBlockReason != "waiting for review" {
		t.Fatalf("omitted reason changed task = %#v", unchanged)
	}

	result, err = svc.AgentAction(ctx, AgentActor("alice"), "update", map[string]any{
		"key": unchanged.Key, "manual_block_reason": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated := result.(Task)
	detail, err = svc.GetTask(ctx, AgentActor("alice"), updated.Key)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ManualBlockReason != "" || detail.Task.Blocked {
		t.Fatalf("updated task = %#v; detail = %#v; want cleared, unblocked task", updated, detail.Task)
	}
}
