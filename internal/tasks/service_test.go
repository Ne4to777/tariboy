package tasks

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/store"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewService(st.DB, "customer", func() time.Time {
		return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	})
}

func TestNormalizePullRequest(t *testing.T) {
	for _, bad := range []string{"github.com/o/r/pull/1", "ftp://example.test/1", "https://u:p@example.test/1"} {
		if _, err := NormalizePullRequest(bad); ErrorCode(err) != "invalid_pull_request" {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	got, err := NormalizePullRequest(" HTTPS://Example.test/pull/1 ")
	if err != nil || got != "https://example.test/pull/1" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestCreateTaskPullRequestValidatesPersistsAndReplays(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "PULL", Name: "Pull requests"})

	created, err := svc.CreateTask(ctx, actor, CreateTaskInput{
		Queue: "PULL", Title: "ship", PullRequest: " HTTPS://Example.test/org/repo/pull/1 ", IdempotencyKey: "create-pr",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.PullRequest != "https://example.test/org/repo/pull/1" {
		t.Fatalf("created pull request = %q", created.PullRequest)
	}
	detail, err := svc.GetTask(ctx, actor, created.Key)
	if err != nil || detail.Task.PullRequest != created.PullRequest {
		t.Fatalf("persisted task = %#v, %v", detail.Task, err)
	}
	events, err := svc.ListEvents(ctx, actor, created.Key, 0, 20)
	if err != nil || len(events) != 1 || events[0].Payload["pull_request"] != created.PullRequest {
		t.Fatalf("created events = %#v, %v", events, err)
	}

	replayed, err := svc.CreateTask(ctx, actor, CreateTaskInput{
		Queue: "PULL", Title: "ignored", PullRequest: "https://example.test/other", IdempotencyKey: "create-pr",
	})
	if err != nil || replayed.Key != created.Key || replayed.PullRequest != created.PullRequest {
		t.Fatalf("replayed task = %#v, %v", replayed, err)
	}
	if _, err := svc.CreateTask(ctx, actor, CreateTaskInput{
		Queue: "PULL", Title: "invalid", PullRequest: "https://user@example.test/pull/2",
	}); ErrorCode(err) != "invalid_pull_request" {
		t.Fatalf("invalid pull request error = %v", err)
	}
	var taskCount, eventCount int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE queue_prefix = 'PULL'`).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = ?`, created.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 || eventCount != 1 {
		t.Fatalf("tasks/events = %d/%d, want 1/1", taskCount, eventCount)
	}
}

func TestUpdateTaskPullRequestUsesRevisionAndEvents(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "PULL", Name: "Pull requests"})
	task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "PULL", Title: "ship"})
	if err != nil {
		t.Fatal(err)
	}

	pullRequest := " HTTPS://Example.test/org/repo/pull/1 "
	updated, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{
		PullRequest: &pullRequest, Revision: task.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.PullRequest != "https://example.test/org/repo/pull/1" || updated.Revision != task.Revision+1 {
		t.Fatalf("set pull request = %q revision %d", updated.PullRequest, updated.Revision)
	}
	events, err := svc.ListEvents(ctx, actor, task.Key, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Kind != "task.updated" || last.Payload["pull_request"] != updated.PullRequest {
		t.Fatalf("updated event = %#v", last)
	}

	empty := ""
	cleared, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{
		PullRequest: &empty, Revision: updated.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.PullRequest != "" || cleared.Revision != updated.Revision+1 {
		t.Fatalf("cleared pull request = %q revision %d", cleared.PullRequest, cleared.Revision)
	}
	events, err = svc.ListEvents(ctx, actor, task.Key, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	last = events[len(events)-1]
	if last.Kind != "task.updated" || last.Payload["pull_request"] != "" {
		t.Fatalf("cleared event = %#v", last)
	}
	eventCount := len(events)

	invalid := "github.com/org/repo/pull/2"
	if _, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{
		PullRequest: &invalid, Revision: cleared.Revision,
	}); ErrorCode(err) != "invalid_pull_request" {
		t.Fatalf("invalid pull request error = %v", err)
	}
	unchanged, err := svc.GetTask(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Task.PullRequest != "" || unchanged.Task.Revision != cleared.Revision {
		t.Fatalf("task mutated after invalid pull request = %#v", unchanged.Task)
	}
	events, err = svc.ListEvents(ctx, actor, task.Key, 0, 20)
	if err != nil || len(events) != eventCount {
		t.Fatalf("events after invalid pull request = %d, %v; want %d", len(events), err, eventCount)
	}
}

func TestUpdateTaskAcceptsWaitCustomer(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "WAIT", Name: "Waits"})
	task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "WAIT", Title: "choose"})
	if err != nil {
		t.Fatal(err)
	}
	status := StatusWaitCustomer
	updated, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{Status: &status, Revision: task.Revision})
	if err != nil || updated.Status != StatusWaitCustomer || updated.Revision != task.Revision+1 {
		t.Fatalf("wait_customer update = %#v, %v", updated, err)
	}
	active, err := svc.ListTasks(ctx, actor, ListFilter{})
	if err != nil || len(active.Tasks) != 1 || active.Tasks[0].Key != task.Key {
		t.Fatalf("active tasks after wait_customer = %#v, %v", active.Tasks, err)
	}
}

func TestUpdateTaskPullRequestKeepsStatus(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "PRWAIT", Name: "PR waits"})
	task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "PRWAIT", Title: "ship", Assignee: "agent:worker"})
	if err != nil {
		t.Fatal(err)
	}
	url := "https://example.test/org/repo/pull/1"
	updated, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{PullRequest: &url, Revision: task.Revision})
	if err != nil || updated.Status != StatusOpen {
		t.Fatalf("updated = %#v, %v", updated, err)
	}
}

func TestCreateTaskPullRequestKeepsStatus(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "PRCREATE", Name: "PR creates"})
	url := "https://example.test/org/repo/pull/1"
	task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "PRCREATE", Title: "ship", Assignee: "agent:worker", PullRequest: url})
	if err != nil || task.Status != StatusOpen {
		t.Fatalf("created = %#v, %v", task, err)
	}
}

func TestCreateTaskAllocatesPermanentQueueKeyAndInheritsQueue(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")

	queue, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "TEST",
		Name:   "Tests",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: queue.Prefix,
		Title: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: queue.Prefix,
		Title: "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		ParentKey: first.Key,
		Title:     "child",
	})
	if err != nil {
		t.Fatal(err)
	}

	keys := map[string]bool{}
	for _, key := range []string{first.Key, second.Key, child.Key} {
		if !randomKeyPattern.MatchString(key) || !strings.HasPrefix(key, "TEST-") {
			t.Fatalf("key %q is not a TEST random key", key)
		}
		if keys[key] {
			t.Fatalf("duplicate key %q", key)
		}
		keys[key] = true
	}
	if child.ParentKey != first.Key || child.Queue != "TEST" {
		t.Fatalf("child parent/queue = %q/%q; want %q/TEST",
			child.ParentKey, child.Queue, first.Key)
	}
	if child.Customer != "user:customer" {
		t.Fatalf("child customer = %q; want user:customer", child.Customer)
	}
}

func TestCreateTaskInQueueWithoutWorkflowKeepsLegacyBehavior(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "FREE", Name: "Free form"}); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "FREE", Title: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusOpen || task.WorkflowVersionID != 0 || task.WorkflowVersion != "" || task.WorkflowStatus != "" || task.WorkflowRevision != 0 {
		t.Fatalf("legacy task = %#v", task)
	}
	var workflowVersionID, workflowStatus, workflowRevision any
	if err := svc.db.QueryRow(`
		SELECT workflow_version_id, workflow_status, workflow_revision
		FROM tasks WHERE id = ?`, task.ID).Scan(&workflowVersionID, &workflowStatus, &workflowRevision); err != nil {
		t.Fatal(err)
	}
	if workflowVersionID != nil || workflowStatus != nil || workflowRevision != nil {
		t.Fatalf("legacy workflow columns = %#v/%#v/%#v; want NULL", workflowVersionID, workflowStatus, workflowRevision)
	}
	detail, err := svc.GetTask(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.WorkflowVersionID != 0 || detail.Task.WorkflowVersion != "" || detail.Task.WorkflowStatus != "" || detail.Task.WorkflowRevision != 0 {
		t.Fatalf("legacy task read-back workflow = %#v; want zero values", detail.Task)
	}
}

func TestCreateQueueRejectsInvalidPrefixAndNonCustomer(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if _, err := svc.CreateQueue(ctx, CustomerActor("customer"),
		CreateQueueInput{Prefix: "bad-prefix", Name: "Bad"}); ErrorCode(err) != "invalid_queue_prefix" {
		t.Fatalf("invalid prefix error = %v; want invalid_queue_prefix", err)
	}
	if _, err := svc.CreateQueue(ctx, AgentActor("alice"),
		CreateQueueInput{Prefix: "TEAM", Name: "Team"}); ErrorCode(err) != "forbidden" {
		t.Fatalf("agent create queue error = %v; want forbidden", err)
	}
}

func TestUpdateQueueRollsBackOwnersWhenRevisionGuardUpdatesNoRow(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	queue, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "RACE", Name: "Race", Owners: []string{"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`
		CREATE TRIGGER suppress_queue_update
		BEFORE UPDATE ON task_queues
		BEGIN
			SELECT RAISE(IGNORE);
		END`); err != nil {
		t.Fatal(err)
	}

	replacement := []string{"bob"}
	if _, err := svc.UpdateQueue(ctx, customer, queue.Prefix, UpdateQueueInput{
		Owners: &replacement, Revision: queue.Revision,
	}); ErrorCode(err) != "revision_conflict" {
		t.Fatalf("zero-row guarded update error = %v; want revision_conflict", err)
	}

	got, err := svc.GetQueue(ctx, customer, queue.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Owners) != 1 || got.Owners[0] != "alice" {
		t.Fatalf("owners after rejected update = %#v; want alice", got.Owners)
	}
}

func TestQueueMutationsAppendEventsAndNudgeHub(t *testing.T) {
	svc := newTestService(t)
	hub := NewHub(svc)
	svc.SetHub(hub)
	wake, cancel := hub.Subscribe()
	defer cancel()
	ctx := context.Background()
	customer := CustomerActor("customer")

	queue, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "LIVE", Name: "Live", Owners: []string{"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("queue create did not nudge hub")
	}
	renamed := "Live renamed"
	if _, err := svc.UpdateQueue(ctx, customer, queue.Prefix, UpdateQueueInput{
		Name: &renamed, Revision: queue.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("queue update did not nudge hub")
	}
	events, err := svc.ListEvents(ctx, customer, "", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "task.queue_created" ||
		events[1].Kind != "task.queue_updated" {
		t.Fatalf("queue events = %#v", events)
	}
	agentEvents, err := svc.ListEvents(ctx, AgentActor("alice"), "", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(agentEvents) != 2 {
		t.Fatalf("queue owner events = %#v; want both queue events", agentEvents)
	}
}

func TestCreateTaskIdempotencyReplaysOriginalWithoutAllocatingAnotherKey(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "IDEM", Name: "Idempotency",
	}); err != nil {
		t.Fatal(err)
	}

	first, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "IDEM", Title: "original", IdempotencyKey: "create-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "IDEM", Title: "different retry payload", IdempotencyKey: "create-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Key != first.Key || replayed.Title != first.Title {
		t.Fatalf("replayed = %#v, first = %#v", replayed, first)
	}
	var tasks, events int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE queue_prefix = 'IDEM'`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM task_events WHERE kind = 'task.created'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || events != 1 {
		t.Fatalf("tasks/events = %d/%d, want 1/1", tasks, events)
	}
}

func TestListTasksStatusViewsPreserveVisibilityAndFlattenExcludedParents(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "VIEW", Name: "Views"}); err != nil {
		t.Fatal(err)
	}
	parent, err := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "VIEW", Title: "completed parent", Assignee: "alice", Priority: PriorityP1})
	if err != nil {
		t.Fatal(err)
	}
	child, err := svc.CreateTask(ctx, customer, CreateTaskInput{ParentKey: parent.Key, Title: "active child"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "VIEW", Title: "first active root", Assignee: "alice", Priority: PriorityP0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "VIEW", Title: "private task", Assignee: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteTask(ctx, customer, parent.Key, CompleteInput{Revision: parent.Revision, CompleteAnyway: true}); err != nil {
		t.Fatal(err)
	}

	keys := func(page TaskPage) []string {
		out := make([]string, len(page.Tasks))
		for i, task := range page.Tasks {
			out[i] = task.Key
		}
		return out
	}
	assertView := func(actor Actor, filter ListFilter, want ...string) {
		t.Helper()
		page, err := svc.ListTasks(ctx, actor, filter)
		if err != nil {
			t.Fatal(err)
		}
		got := keys(page)
		if len(got) != len(want) {
			t.Fatalf("%+v keys = %v, want %v", filter, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%+v keys = %v, want %v", filter, got, want)
			}
		}
	}

	// Active never includes the completed parent as rendering context: its visible
	// child is a deterministic root-level orphan in this filtered response.
	assertView(AgentActor("alice"), ListFilter{}, first.Key, child.Key)
	assertView(AgentActor("alice"), ListFilter{StatusView: "closed"}, parent.Key)
	assertView(AgentActor("alice"), ListFilter{StatusView: "all"}, first.Key, parent.Key, child.Key)
	assertView(customer, ListFilter{ScopeAgent: "alice"}, first.Key, child.Key)
	if _, err := svc.ListTasks(ctx, AgentActor("alice"), ListFilter{ScopeAgent: "bob"}); ErrorCode(err) != "forbidden" {
		t.Fatalf("expanded agent scope error = %v; want forbidden", err)
	}
}

func TestListTasksExplicitStatusIsAuthoritativeWithoutStatusView(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "COMPAT", Name: "Compatibility"}); err != nil {
		t.Fatal(err)
	}
	open, err := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "COMPAT", Title: "open"})
	if err != nil {
		t.Fatal(err)
	}
	done, err := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "COMPAT", Title: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteTask(ctx, customer, done.Key, CompleteInput{Revision: done.Revision}); err != nil {
		t.Fatal(err)
	}

	keys := func(filter ListFilter) []string {
		t.Helper()
		page, err := svc.ListTasks(ctx, customer, filter)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(page.Tasks))
		for i, task := range page.Tasks {
			out[i] = task.Key
		}
		return out
	}
	assertKeys := func(filter ListFilter, want ...string) {
		t.Helper()
		got := keys(filter)
		if len(got) != len(want) {
			t.Fatalf("%+v keys = %v, want %v", filter, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%+v keys = %v, want %v", filter, got, want)
			}
		}
	}

	// Removing the status-only path from ListTasks would make this regression
	// fail: legacy callers that supply status but no status_view must keep their
	// explicit status semantics.
	assertKeys(ListFilter{Status: "done"}, done.Key)
	assertKeys(ListFilter{}, open.Key)
	assertKeys(ListFilter{Status: "done", StatusView: "closed"}, done.Key)
	assertKeys(ListFilter{Status: "done", StatusView: "active"})
}

func TestListTasksUsesStableCursorPagination(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "PAGE", Name: "Pages"})
	for _, title := range []string{"one", "two", "three"} {
		if _, err := svc.CreateTask(ctx, customer, CreateTaskInput{
			Queue: "PAGE", Title: title,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := svc.ListTasks(ctx, customer, ListFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Tasks) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %#v; want 2 tasks and cursor", first)
	}
	second, err := svc.ListTasks(ctx, customer, ListFilter{
		Limit: 2, AfterKey: first.NextCursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Tasks) != 1 || second.NextCursor != "" ||
		second.Tasks[0].Key == first.Tasks[0].Key ||
		second.Tasks[0].Key == first.Tasks[1].Key {
		t.Fatalf("second page = %#v; want final distinct task", second)
	}
}

func TestCreateTaskDefaultsAndValidatesPriority(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "PRIO", Name: "Priorities"})

	normal, err := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "PRIO", Title: "normal"})
	if err != nil {
		t.Fatal(err)
	}
	if normal.Priority != PriorityP2 {
		t.Fatalf("default priority = %q; want P2", normal.Priority)
	}
	critical, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "PRIO", Title: "critical", Priority: PriorityP0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if critical.Priority != PriorityP0 {
		t.Fatalf("explicit priority = %q; want P0", critical.Priority)
	}
	if _, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "PRIO", Title: "invalid", Priority: Priority("urgent"),
	}); ErrorCode(err) != "invalid_priority" {
		t.Fatalf("invalid create error = %v; want invalid_priority", err)
	}
}

func TestUpdateTaskPriorityUsesRevisionGuard(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "EDIT", Name: "Editing"})
	task, err := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "EDIT", Title: "task"})
	if err != nil {
		t.Fatal(err)
	}

	high := PriorityP1
	updated, err := svc.UpdateTask(ctx, customer, task.Key, UpdateTaskInput{
		Priority: &high, Revision: task.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Priority != PriorityP1 {
		t.Fatalf("updated priority = %q; want P1", updated.Priority)
	}
	invalid := Priority("urgent")
	if _, err := svc.UpdateTask(ctx, customer, task.Key, UpdateTaskInput{
		Priority: &invalid, Revision: updated.Revision,
	}); ErrorCode(err) != "invalid_priority" {
		t.Fatalf("invalid update error = %v; want invalid_priority", err)
	}
	low := PriorityP3
	if _, err := svc.UpdateTask(ctx, customer, task.Key, UpdateTaskInput{
		Priority: &low, Revision: task.Revision,
	}); ErrorCode(err) != "revision_conflict" {
		t.Fatalf("stale update error = %v; want revision_conflict", err)
	}
	got, err := svc.GetTask(ctx, customer, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Task.Priority != PriorityP1 {
		t.Fatalf("priority after rejected updates = %q; want P1", got.Task.Priority)
	}
}

func TestListTasksFilterByTextMatchesTaskKey(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "KEYS", Name: "Keys"}); err != nil {
		t.Fatal(err)
	}
	target, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "KEYS", Title: "unrelated title", Description: "unrelated description",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "KEYS", Title: "another task", Description: "another description",
	}); err != nil {
		t.Fatal(err)
	}

	keys := func(text string) []string {
		t.Helper()
		page, err := svc.ListTasks(ctx, customer, ListFilter{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(page.Tasks))
		for i, task := range page.Tasks {
			out[i] = task.Key
		}
		return out
	}
	assertOnly := func(text string, want string) {
		t.Helper()
		got := keys(text)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("keys(%q) = %v, want [%s]", text, got, want)
		}
	}

	assertOnly(target.Key, target.Key)
	assertOnly(strings.ToLower(target.Key), target.Key)
	bareNumber := strings.TrimPrefix(target.Key, "KEYS-")
	assertOnly(bareNumber, target.Key)

	if got := keys("no-such-match-anywhere"); len(got) != 0 {
		t.Fatalf("keys(no-such-match-anywhere) = %v, want none", got)
	}
}

// Authorship still carries a whole subtree: an agent running a customer root keeps its
// children even though a root it files itself would be invisible. Detaching a child hands
// it out of that subtree, and the counts on the root say how big the subtree has grown.
func TestSubtreeAccessSurvivesAuthorshipNarrowingAndDetach(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "TREE", Name: "Tree"}); err != nil {
		t.Fatal(err)
	}
	root, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "TREE", Title: "customer request", Assignee: "mgr",
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr := AgentActor("mgr")
	child, err := svc.CreateTask(ctx, mgr, CreateTaskInput{ParentKey: root.Key, Title: "decomposed piece"})
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := svc.CreateTask(ctx, mgr, CreateTaskInput{ParentKey: child.Key, Title: "deeper piece"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{root.Key, child.Key, grandchild.Key} {
		detail, err := svc.GetTask(ctx, mgr, key)
		if err != nil {
			t.Fatalf("manager reading %s: %v", key, err)
		}
		if detail.Task.Access != "write" {
			t.Fatalf("access on %s = %q; want write", key, detail.Task.Access)
		}
	}

	detail, err := svc.GetTask(ctx, mgr, root.Key)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Descendants != 2 || detail.ActiveDescendants != 2 {
		t.Fatalf("root counts = %d/%d; want 2/2", detail.Descendants, detail.ActiveDescendants)
	}

	// Detaching a leaf takes it out of the manager's reach entirely: it is a root now, and a
	// root the manager neither owns nor is assigned is not its business any more.
	if _, err := svc.MoveTask(ctx, mgr, grandchild.Key, MoveInput{Revision: grandchild.Revision}); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if _, err := svc.GetTask(ctx, mgr, grandchild.Key); ErrorCode(err) != "not_found" {
		t.Fatalf("manager reading a detached leaf: %v; want not_found", err)
	}
	if _, err := svc.GetTask(ctx, customer, grandchild.Key); err != nil {
		t.Fatalf("customer reading a detached leaf: %v", err)
	}
	detail, err = svc.GetTask(ctx, mgr, root.Key)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Descendants != 1 || detail.ActiveDescendants != 1 {
		t.Fatalf("root counts after detach = %d/%d; want 1/1", detail.Descendants, detail.ActiveDescendants)
	}
	childDetail, err := svc.GetTask(ctx, mgr, child.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteTask(ctx, mgr, child.Key, CompleteInput{Revision: childDetail.Task.Revision}); err != nil {
		t.Fatalf("completing the remaining child: %v", err)
	}
	detail, err = svc.GetTask(ctx, mgr, root.Key)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Descendants != 1 || detail.ActiveDescendants != 0 {
		t.Fatalf("root counts after closing the child = %d/%d; want 1/0", detail.Descendants, detail.ActiveDescendants)
	}
	if _, err := svc.CompleteTask(ctx, mgr, root.Key, CompleteInput{Revision: detail.Task.Revision}); err != nil {
		t.Fatalf("completing the root once its tree is empty: %v", err)
	}
}

func TestEnsureDefaultQueueCreatesTaskQueueOnce(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")

	for range 2 {
		if err := svc.EnsureDefaultQueue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := svc.GetQueue(ctx, customer, "TASK")
	if err != nil {
		t.Fatal(err)
	}
	if queue.Name != "Tasks" || len(queue.Owners) != 0 || queue.ResponsibleAgent != "" {
		t.Fatalf("default queue = %#v", queue)
	}
	events, err := svc.ListEvents(ctx, customer, "", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "task.queue_created" || events[0].Actor != "system:tariboyd" {
		t.Fatalf("events = %#v; want one task.queue_created by system:tariboyd", events)
	}
}

func TestEnsureDefaultQueueKeepsExistingTaskQueue(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{
		Prefix: "TASK", Name: "Custom", Owners: []string{"alice"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsureDefaultQueue(ctx); err != nil {
		t.Fatal(err)
	}
	queue, err := svc.GetQueue(ctx, customer, "TASK")
	if err != nil {
		t.Fatal(err)
	}
	if queue.Name != "Custom" || len(queue.Owners) != 1 || queue.Revision != 1 {
		t.Fatalf("existing queue changed: %#v", queue)
	}
}

func TestStartedAtRecordsTheFirstMoveToInProgress(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	clock := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc.clock = func() time.Time { return clock }
	_, _ = svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "START", Name: "Starts", Owners: []string{"alice"}})
	task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "START", Title: "work"})
	if err != nil || task.StartedAt != "" {
		t.Fatalf("created = %#v, %v", task, err)
	}
	set := func(status string) Task {
		t.Helper()
		clock = clock.Add(time.Hour)
		detail, err := svc.GetTask(ctx, actor, task.Key)
		if err != nil {
			t.Fatal(err)
		}
		updated, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{Status: &status, Revision: detail.Task.Revision})
		if err != nil {
			t.Fatal(err)
		}
		return updated
	}
	first := set(StatusInProgress)
	if want := "2026-07-31T13:00:00Z"; first.StartedAt != want {
		t.Fatalf("started_at = %q; want %q", first.StartedAt, want)
	}
	set(StatusOpen)
	set(StatusInProgress)
	set(StatusDone)
	detail, err := svc.GetTask(ctx, actor, task.Key)
	if err != nil || detail.Task.StartedAt != first.StartedAt {
		t.Fatalf("started_at = %q, %v; want the first start %q", detail.Task.StartedAt, err, first.StartedAt)
	}

	claimable, _ := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "START", Title: "claim me"})
	claimed, err := svc.ClaimReady(ctx, AgentActor("alice"), ReadyFilter{Queue: "START"}, "claim-start")
	if err != nil || claimed.Key != claimable.Key {
		t.Fatalf("claimed = %#v, %v", claimed, err)
	}
	detail, err = svc.GetTask(ctx, actor, claimable.Key)
	if err != nil || detail.Task.StartedAt != svc.now() {
		t.Fatalf("claimed started_at = %q, %v", detail.Task.StartedAt, err)
	}
}
