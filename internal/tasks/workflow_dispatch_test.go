package tasks

import (
	"context"
	"testing"
)

func TestDispatchPendingAssignsOnceAMemberBecomesEligible(t *testing.T) {
	ctx := context.Background()
	svc, actor, _ := engineFixture(t)
	setAgent(t, svc, "dev-1", `enabled = 0`)
	setAgent(t, svc, "dev-2", `current_goal_task_key = 'TASK-busy'`)
	task := mustCreateDev(t, svc, actor, "t")
	if task.Status != StatusOpen || task.Assignee != "" || task.WorkflowStatus != "develop" {
		t.Fatalf("created = %#v; want open and unassigned", task)
	}
	if holderOf(t, svc, task, "developers") != "" {
		t.Fatal("a holder was recorded without an assignment")
	}
	if n, err := svc.DispatchPending(ctx); err != nil || n != 0 {
		t.Fatalf("dispatch with nobody eligible = %d, %v", n, err)
	}
	signals := 0
	svc.SetGoalSignal(func() { signals++ })
	setAgent(t, svc, "dev-2", `current_goal_task_key = ''`)
	if n, err := svc.DispatchPending(ctx); err != nil || n != 1 {
		t.Fatalf("dispatch = %d, %v; want 1", n, err)
	}
	if signals == 0 {
		t.Fatal("goal signal did not fire after dispatch")
	}
	stored, err := taskByKey(svc.db, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusInProgress || stored.Assignee != "agent:dev-2" || stored.WorkflowStatus != "develop" ||
		stored.Revision != task.Revision+1 {
		t.Fatalf("dispatched = %#v", stored)
	}
	if holderOf(t, svc, stored, "developers") != "dev-2" {
		t.Fatal("holder not recorded")
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_notification_outbox o JOIN task_events e ON e.sequence = o.event_sequence
		WHERE e.task_id = ? AND o.message_type = 'task.assigned' AND e.actor = 'system:workflow'`, task.ID); n != 1 {
		t.Fatalf("assignment notifications = %d", n)
	}
	if n, err := svc.DispatchPending(ctx); err != nil || n != 0 {
		t.Fatalf("second dispatch = %d, %v", n, err)
	}
}

func TestDispatchPendingOrdersByPriorityAndUsesEachAgentOnce(t *testing.T) {
	ctx := context.Background()
	svc, actor, _ := engineFixture(t)
	setAgent(t, svc, "dev-1", `enabled = 0`)
	setAgent(t, svc, "dev-2", `enabled = 0`)
	older := mustCreateDev(t, svc, actor, "older")
	urgent, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "DEV", Title: "urgent", Priority: PriorityP0})
	if err != nil {
		t.Fatal(err)
	}
	setAgent(t, svc, "dev-1", `enabled = 1`)
	if n, err := svc.DispatchPending(ctx); err != nil || n != 1 {
		t.Fatalf("dispatch = %d, %v; want 1", n, err)
	}
	gotUrgent, _ := taskByKey(svc.db, urgent.Key)
	gotOlder, _ := taskByKey(svc.db, older.Key)
	if gotUrgent.Assignee != "agent:dev-1" || gotOlder.Assignee != "" || gotOlder.Status != StatusOpen {
		t.Fatalf("urgent = %q, older = %q/%s", gotUrgent.Assignee, gotOlder.Assignee, gotOlder.Status)
	}
}

func TestDispatchPendingSkipsBlockedTasks(t *testing.T) {
	ctx := context.Background()
	svc, actor, _ := engineFixture(t)
	setAgent(t, svc, "dev-1", `enabled = 0`)
	setAgent(t, svc, "dev-2", `enabled = 0`)
	task := mustCreateDev(t, svc, actor, "t")
	block := "waiting on infra"
	blocked, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{ManualBlockReason: &block, Revision: task.Revision})
	if err != nil {
		t.Fatal(err)
	}
	setAgent(t, svc, "dev-1", `enabled = 1`)
	if n, err := svc.DispatchPending(ctx); err != nil || n != 0 {
		t.Fatalf("dispatch of a blocked task = %d, %v", n, err)
	}
	stored, _ := taskByKey(svc.db, task.Key)
	if stored.WorkflowStatus != "develop" || stored.Status != StatusOpen {
		t.Fatalf("blocked task = %#v", stored)
	}
	clear := ""
	if _, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{ManualBlockReason: &clear, Revision: blocked.Revision}); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.DispatchPending(ctx); err != nil || n != 1 {
		t.Fatalf("dispatch after unblocking = %d, %v", n, err)
	}
}

func TestDispatchPendingSkipsPausedTasks(t *testing.T) {
	ctx := context.Background()
	svc, actor, _ := engineFixture(t)
	setAgent(t, svc, "dev-1", `enabled = 0`)
	setAgent(t, svc, "dev-2", `enabled = 0`)
	task := mustCreateDev(t, svc, actor, "t")
	if _, err := svc.db.Exec(`UPDATE tasks SET workflow_paused_reason = 'idle_iterations' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	setAgent(t, svc, "dev-1", `enabled = 1`)
	if n, err := svc.DispatchPending(ctx); err != nil || n != 0 {
		t.Fatalf("dispatch of a paused task = %d, %v", n, err)
	}
}
