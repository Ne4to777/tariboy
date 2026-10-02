package tasks

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func taskKeys(tasks []Task) map[string]Task {
	out := make(map[string]Task, len(tasks))
	for _, task := range tasks {
		out[task.Key] = task
	}
	return out
}

func TestAncestorAssignmentExposesAllDescendantsWithoutSiblingBranches(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "TREE", Name: "Tree",
	}); err != nil {
		t.Fatal(err)
	}
	root, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "TREE", Title: "root", Assignee: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := root
	var deepest Task
	for i := 0; i < 5; i++ {
		assignee := ""
		if i == 4 {
			assignee = "bob"
		}
		deepest, err = svc.CreateTask(ctx, customer, CreateTaskInput{
			ParentKey: parent.Key,
			Title:     "level",
			Assignee:  assignee,
		})
		if err != nil {
			t.Fatal(err)
		}
		parent = deepest
	}
	sibling, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "TREE", Title: "unrelated root",
	})
	if err != nil {
		t.Fatal(err)
	}

	alicePage, err := svc.ListTasks(ctx, AgentActor("alice"), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alice := taskKeys(alicePage.Tasks)
	if len(alice) != 6 {
		t.Fatalf("alice sees %d tasks; want assigned root and five descendants", len(alice))
	}
	if _, ok := alice[sibling.Key]; ok {
		t.Fatalf("alice unexpectedly sees sibling branch %s", sibling.Key)
	}
	for key, task := range alice {
		if task.Access != "write" {
			t.Fatalf("alice access to %s = %q; want write", key, task.Access)
		}
	}

	bobPage, err := svc.ListTasks(ctx, AgentActor("bob"), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	bob := taskKeys(bobPage.Tasks)
	if len(bob) != 6 {
		t.Fatalf("bob sees %d tasks; want five context ancestors plus assigned leaf", len(bob))
	}
	if bob[deepest.Key].Access != "write" {
		t.Fatalf("bob leaf access = %q; want write", bob[deepest.Key].Access)
	}
	if bob[root.Key].Access != "context" {
		t.Fatalf("bob root access = %q; want context", bob[root.Key].Access)
	}
	if _, ok := bob[sibling.Key]; ok {
		t.Fatalf("bob unexpectedly sees sibling branch %s", sibling.Key)
	}
}

func TestListTasksLoadsDeepVisibleTreePromptly(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "DEEP", Name: "Deep tree"}); err != nil {
		t.Fatal(err)
	}

	const depth = 2500
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := svc.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var parent any
	for i := 0; i < depth; i++ {
		assignee := ""
		if i == 0 {
			assignee = "agent:alice"
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO tasks(
				task_key, queue_prefix, parent_id, position, priority, title,
				description, status, author, customer, group_name, assignee,
				manual_block_reason, revision, created_at, updated_at, completed_at
			) VALUES (?, 'DEEP', ?, 0, ?, ?, '', 'open', 'user:customer',
				'user:customer', '', ?, '', 1, ?, ?, '')`,
			fmt.Sprintf("DEEP-%d", i+1), parent, PriorityP2, fmt.Sprintf("depth %d", i), assignee, now, now)
		if err != nil {
			t.Fatal(err)
		}
		parent, err = result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	page, err := svc.ListTasks(ctx, AgentActor("alice"), ListFilter{Limit: 500})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 500 || page.NextCursor == "" {
		t.Fatalf("page = %#v, want 500 tasks and a cursor", page)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("deep task list took %s; want under 2s", elapsed)
	}
}

func TestGroupVisibilityTracksCurrentMembership(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "GROUP", Name: "Group",
	}); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "GROUP", Title: "group task", Group: "dev-team",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`
		INSERT INTO agents(name, image_ref, image_digest, "group")
		VALUES ('worker', 'basic:latest', 'digest', 'dev-team')`); err != nil {
		t.Fatal(err)
	}
	page, err := svc.ListTasks(ctx, AgentActor("worker"), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := taskKeys(page.Tasks)[task.Key]; !ok {
		t.Fatalf("group member does not see %s", task.Key)
	}
	if _, err := svc.db.Exec(`UPDATE agents SET "group" = '' WHERE name = 'worker'`); err != nil {
		t.Fatal(err)
	}
	page, err = svc.ListTasks(ctx, AgentActor("worker"), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := taskKeys(page.Tasks)[task.Key]; ok {
		t.Fatalf("former group member still sees %s", task.Key)
	}
}
