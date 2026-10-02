package tasks

import (
	"context"
	"strings"
	"testing"
)

func TestMoveRejectsDescendantAndCrossQueueAndReordersRoots(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	for _, q := range []CreateQueueInput{
		{Prefix: "TEST", Name: "Test"},
		{Prefix: "CORE", Name: "Core"},
	} {
		if _, err := svc.CreateQueue(ctx, customer, q); err != nil {
			t.Fatal(err)
		}
	}
	root, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "TEST", Title: "root"})
	child, _ := svc.CreateTask(ctx, customer, CreateTaskInput{ParentKey: root.Key, Title: "child"})
	other, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "TEST", Title: "other"})
	core, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "CORE", Title: "core"})

	if _, err := svc.MoveTask(ctx, customer, root.Key, MoveInput{
		ParentKey: child.Key,
		Revision:  root.Revision,
	}); ErrorCode(err) != "hierarchy_cycle" {
		t.Fatalf("cycle error = %v; want hierarchy_cycle", err)
	}
	if _, err := svc.MoveTask(ctx, customer, other.Key, MoveInput{
		ParentKey: core.Key,
		Revision:  other.Revision,
	}); ErrorCode(err) != "cross_queue_move" {
		t.Fatalf("cross queue error = %v; want cross_queue_move", err)
	}
	moved, err := svc.MoveTask(ctx, customer, other.Key, MoveInput{
		BeforeKey: root.Key,
		Revision:  other.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Position != 0 || moved.Revision != other.Revision+1 {
		t.Fatalf("moved position/revision = %d/%d; want 0/%d",
			moved.Position, moved.Revision, other.Revision+1)
	}
	page, err := svc.ListTasks(ctx, customer, ListFilter{Queue: "TEST"})
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, task := range page.Tasks {
		if task.ParentKey == "" {
			roots = append(roots, task.Key)
		}
	}
	if len(roots) != 2 || roots[0] != other.Key || roots[1] != root.Key {
		t.Fatalf("root order = %v; want [%s %s]", roots, other.Key, root.Key)
	}
}

func TestReadyClaimUsesQueueOwnershipAndIsAtomic(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "WORK", Name: "Work", Owners: []string{"alice"},
	}); err != nil {
		t.Fatal(err)
	}
	first, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "WORK", Title: "first"})
	_, _ = svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "WORK", Title: "second"})

	ready, err := svc.Ready(ctx, AgentActor("alice"), ReadyFilter{Queue: "WORK"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 2 || ready[0].Key != first.Key {
		t.Fatalf("ready = %#v; want first then second", ready)
	}
	claimed, err := svc.ClaimReady(ctx, AgentActor("alice"), ReadyFilter{Queue: "WORK"}, "claim-1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Key != first.Key || claimed.Assignee != "agent:alice" ||
		claimed.Status != StatusInProgress {
		t.Fatalf("claimed = %#v", claimed)
	}
	replayed, err := svc.ClaimReady(ctx, AgentActor("alice"), ReadyFilter{Queue: "WORK"}, "claim-1")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Key != claimed.Key || replayed.Revision != claimed.Revision {
		t.Fatalf("idempotent replay = %#v; want %#v", replayed, claimed)
	}
}

func TestMoveRejectsHiddenParentAndBeforeTargets(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "ACL", Name: "ACL"})
	aliceTask, _ := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "ACL", Title: "alice", Assignee: "alice",
	})
	bobParent, _ := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "ACL", Title: "bob parent", Assignee: "bob",
	})
	bobSibling, _ := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "ACL", Title: "bob sibling", Assignee: "bob",
	})
	contextParent, _ := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "ACL", Title: "context parent", Assignee: "bob",
	})
	aliceChild, _ := svc.CreateTask(ctx, customer, CreateTaskInput{
		ParentKey: contextParent.Key, Title: "alice child", Assignee: "alice",
	})

	if _, err := svc.MoveTask(ctx, AgentActor("alice"), aliceTask.Key, MoveInput{
		ParentKey: bobParent.Key,
		Revision:  aliceTask.Revision,
	}); ErrorCode(err) != "not_found" {
		t.Fatalf("hidden parent move error = %v; want not_found", err)
	}
	if _, err := svc.MoveTask(ctx, AgentActor("alice"), aliceTask.Key, MoveInput{
		BeforeKey: bobSibling.Key,
		Revision:  aliceTask.Revision,
	}); ErrorCode(err) != "not_found" {
		t.Fatalf("hidden before move error = %v; want not_found", err)
	}
	if _, err := svc.MoveTask(ctx, AgentActor("alice"), aliceTask.Key, MoveInput{
		BeforeKey: aliceChild.Key,
		Revision:  aliceTask.Revision,
	}); ErrorCode(err) != "not_found" {
		t.Fatalf("context-only inferred parent error = %v; want not_found", err)
	}
}

func TestListTasksOrdersEverySiblingSetByPriorityThenPosition(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "ORDER", Name: "Ordering"})
	low, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "ORDER", Title: "low", Priority: PriorityP3})
	critical, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "ORDER", Title: "critical", Priority: PriorityP0})
	high, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "ORDER", Title: "high", Priority: PriorityP1})
	normal, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "ORDER", Title: "normal"})
	childLow, err := svc.CreateTask(ctx, customer, CreateTaskInput{ParentKey: critical.Key, Title: "child low", Priority: PriorityP3})
	if err != nil {
		t.Fatal(err)
	}
	childHigh, err := svc.CreateTask(ctx, customer, CreateTaskInput{ParentKey: critical.Key, Title: "child high", Priority: PriorityP1})
	if err != nil {
		t.Fatal(err)
	}
	childHigh2, err := svc.CreateTask(ctx, customer, CreateTaskInput{ParentKey: critical.Key, Title: "child high 2", Priority: PriorityP1})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.db.Exec(`UPDATE tasks SET position = 0 WHERE task_key IN (?, ?, ?, ?, ?, ?, ?)`,
		low.Key, critical.Key, high.Key, normal.Key, childLow.Key, childHigh.Key, childHigh2.Key); err != nil {
		t.Fatal(err)
	}
	page, err := svc.ListTasks(ctx, customer, ListFilter{Queue: "ORDER"})
	if err != nil {
		t.Fatal(err)
	}
	var roots, children []string
	for _, task := range page.Tasks {
		if task.ParentKey == "" {
			roots = append(roots, task.Key)
		} else if task.ParentKey == critical.Key {
			children = append(children, task.Key)
		}
	}
	wantRoots := []string{critical.Key, high.Key, normal.Key, low.Key}
	// Both P1 children were forced to position 0, so the canonical order falls
	// through to the task key, which is random rather than creation-ordered.
	firstHigh, secondHigh := childHigh.Key, childHigh2.Key
	if secondHigh < firstHigh {
		firstHigh, secondHigh = secondHigh, firstHigh
	}
	wantChildren := []string{firstHigh, secondHigh, childLow.Key}
	if strings.Join(roots, ",") != strings.Join(wantRoots, ",") {
		t.Fatalf("root order = %v; want %v", roots, wantRoots)
	}
	if strings.Join(children, ",") != strings.Join(wantChildren, ",") {
		t.Fatalf("child order = %v; want %v", children, wantChildren)
	}
}

func TestMoveStaysWithinPriorityBucket(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "MOVE", Name: "Moving"})
	high, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "MOVE", Title: "high", Priority: PriorityP1})
	normalA, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "MOVE", Title: "normal a"})
	normalB, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "MOVE", Title: "normal b"})
	low, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "MOVE", Title: "low", Priority: PriorityP3})

	if _, err := svc.MoveTask(ctx, customer, normalB.Key, MoveInput{
		BeforeKey: high.Key, Revision: normalB.Revision,
	}); ErrorCode(err) != "priority_bucket_mismatch" {
		t.Fatalf("cross-priority move error = %v; want priority_bucket_mismatch", err)
	}
	moved, err := svc.MoveTask(ctx, customer, normalB.Key, MoveInput{
		BeforeKey: normalA.Key, Revision: normalB.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Priority != PriorityP2 || moved.Position != 0 {
		t.Fatalf("moved priority/position = %s/%d; want P2/0", moved.Priority, moved.Position)
	}
	for key, want := range map[string]int64{high.Key: 0, normalA.Key: 1, low.Key: 0} {
		var got int64
		if err := svc.db.QueryRow(`SELECT position FROM tasks WHERE task_key = ?`, key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("position of %s = %d; want %d", key, got, want)
		}
	}
}
