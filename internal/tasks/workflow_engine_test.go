package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// engineDefinition is the workflow most engine tests run: two pool statuses, a
// customer approval, a script merge, and two terminal statuses.
func engineDefinition() workflowfile.File {
	pool := func(name string) workflowfile.Owner {
		return workflowfile.Owner{Kind: workflowfile.OwnerPool, Pool: name}
	}
	return workflowfile.File{
		SchemaVersion: 1, Name: "development", WorkflowVersion: "0.1.0", InitialStatus: "develop",
		Statuses: []workflowfile.Status{
			{ID: "develop", Owner: pool("developers"), Transitions: []workflowfile.Transition{
				{On: "ready", To: "review"}, {On: "ask", To: "approval"},
			}},
			{ID: "review", Owner: pool("reviewers"), Transitions: []workflowfile.Transition{
				{On: "approve", To: "approval"}, {On: "changes", To: "develop"},
			}},
			{ID: "approval", Owner: workflowfile.Owner{Kind: workflowfile.OwnerCustomer}, Instructions: "Approve the change.",
				Transitions: []workflowfile.Transition{{On: "approved", To: "merge"}, {On: "rejected", To: "develop"}}},
			{ID: "merge", Owner: workflowfile.Owner{Kind: workflowfile.OwnerScript},
				Transitions: []workflowfile.Transition{{On: "merged", To: "done"}}},
			{ID: "done", Terminal: true},
			{ID: "dropped", Terminal: true, Cancelled: true},
		},
	}
}

// seedEngineImage records def as a published manifest and installs a resolver
// that resolves its name to the digest.
func seedEngineImage(t *testing.T, svc *Service, def workflowfile.File) string {
	t.Helper()
	rawDef, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rawDef)
	digest := hex.EncodeToString(sum[:])
	raw, err := json.Marshal(workflowimage.Manifest{
		SchemaVersion: 1, Name: def.Name, Version: def.WorkflowVersion, Digest: digest,
		BuiltAt: "2026-10-02T00:00:00Z", Definition: def,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO task_workflow_images(digest, name, version, manifest, built_at) VALUES (?, ?, ?, ?, ?)`,
		digest, def.Name, def.WorkflowVersion, string(raw), "2026-10-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	svc.SetWorkflowResolver(func(string) (string, error) { return digest, nil })
	return digest
}

// engineFixture binds DEV to engineDefinition with developers [dev-1, dev-2]
// and reviewers [reviewer-1]; every agent starts eligible.
func engineFixture(t *testing.T) (*Service, Actor, string) {
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
	digest := seedEngineImage(t, svc, engineDefinition())
	if _, err := svc.SetQueueWorkflow(context.Background(), actor, "DEV", "development:0.1.0", 0); err != nil {
		t.Fatal(err)
	}
	return svc, actor, digest
}

func mustCreateDev(t *testing.T, svc *Service, actor Actor, title string) Task {
	t.Helper()
	task, err := svc.CreateTask(context.Background(), actor, CreateTaskInput{Queue: "DEV", Title: title})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// enter moves a task into statusID through enterStatusTx in its own
// transaction and returns the stored task afterwards.
func enter(t *testing.T, svc *Service, key, statusID, outcome string) Task {
	t.Helper()
	ctx := context.Background()
	tx, err := svc.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	task, err := taskByKey(tx, key)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadManifestTx(ctx, tx, task.WorkflowDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.enterStatusTx(ctx, tx, &task, manifest, statusID, "user:customer", outcome, "msg "+outcome); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stored, err := taskByKey(svc.db, key)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func setAgent(t *testing.T, svc *Service, name, assignments string) {
	t.Helper()
	if _, err := svc.db.Exec(`UPDATE agents SET `+assignments+` WHERE name = ?`, name); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, svc *Service, query string, args ...any) int {
	t.Helper()
	var n int
	if err := svc.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func holderOf(t *testing.T, svc *Service, task Task, pool string) string {
	t.Helper()
	var agent string
	err := svc.db.QueryRow(`SELECT agent FROM task_workflow_holders WHERE task_id = ? AND pool = ?`, task.ID, pool).Scan(&agent)
	if err != nil {
		return ""
	}
	return agent
}

func TestCreateTaskInBoundQueuePinsDigestAndEntersInitialStatus(t *testing.T) {
	svc, actor, digest := engineFixture(t)
	signals := 0
	svc.SetGoalSignal(func() { signals++ })
	created := mustCreateDev(t, svc, actor, "Build it")
	if created.WorkflowDigest != digest || created.WorkflowStatus != "develop" || created.Status != StatusInProgress ||
		created.Category != StatusInProgress || created.Assignee != "agent:dev-1" || created.WorkflowName != "development" ||
		created.WorkflowVersion != "0.1.0" || created.Revision != 2 || created.StartedAt == "" {
		t.Fatalf("created = %#v", created)
	}
	if signals == 0 {
		t.Fatal("goal signal did not fire after the assignment")
	}
	stored, err := taskByKey(svc.db, created.Key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorkflowDigest != digest || stored.WorkflowStatus != "develop" || stored.Status != StatusInProgress ||
		stored.Assignee != "agent:dev-1" || stored.Revision != created.Revision {
		t.Fatalf("stored = %#v", stored)
	}
	if got := holderOf(t, svc, stored, "developers"); got != "dev-1" {
		t.Fatalf("holder = %q", got)
	}
	var sequence int
	var status, enteredBy, leftAt string
	if err := svc.db.QueryRow(`SELECT sequence, status_id, entered_by, left_at FROM task_status_visits WHERE task_id = ?`,
		stored.ID).Scan(&sequence, &status, &enteredBy, &leftAt); err != nil {
		t.Fatal(err)
	}
	if sequence != 1 || status != "develop" || enteredBy != "user:customer" || leftAt != "" {
		t.Fatalf("visit = %d %q %q %q", sequence, status, enteredBy, leftAt)
	}
	var raw string
	if err := svc.db.QueryRow(`SELECT payload FROM task_events WHERE task_id = ? AND kind = 'workflow.transitioned'`, stored.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["from"] != "" || payload["to"] != "develop" || payload["outcome"] != "" || payload["actor"] != "user:customer" {
		t.Fatalf("transition payload = %v", payload)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_notification_outbox o JOIN task_events e ON e.sequence = o.event_sequence
		WHERE e.task_id = ? AND o.message_type = 'task.assigned' AND o.channel LIKE '%dev-1%'`, stored.ID); n != 1 {
		t.Fatalf("assignment notifications = %d", n)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_events WHERE task_id = ? AND kind = 'task.updated' AND actor = 'system:workflow'`, stored.ID); n != 1 {
		t.Fatalf("assignment events = %d", n)
	}
	// The create response carries the workflow status as status.
	encoded, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"status":"develop"`) || !strings.Contains(string(encoded), `"category":"in_progress"`) {
		t.Fatalf("encoded = %s", encoded)
	}

	// A child pins by its own queue's binding and enters the initial status too.
	child, err := svc.CreateTask(context.Background(), actor, CreateTaskInput{ParentKey: created.Key, Title: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if child.WorkflowDigest != digest || child.WorkflowStatus != "develop" {
		t.Fatalf("child = %#v", child)
	}

	// A task in an unbound queue stays flexible.
	flexible, err := svc.CreateTask(context.Background(), actor, CreateTaskInput{Queue: "TASK", Title: "plain", Assignee: "dev-1"})
	if err != nil {
		t.Fatal(err)
	}
	if flexible.WorkflowDigest != "" || flexible.Status != StatusOpen || flexible.Assignee != "agent:dev-1" || flexible.Revision != 1 {
		t.Fatalf("flexible = %#v", flexible)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_status_visits WHERE task_id = (SELECT id FROM tasks WHERE task_key = ?)`, flexible.Key); n != 0 {
		t.Fatalf("flexible visits = %d", n)
	}
}

func TestCreateTaskInBoundQueueRejectsAssignee(t *testing.T) {
	svc, actor, _ := engineFixture(t)
	_, err := svc.CreateTask(context.Background(), actor, CreateTaskInput{Queue: "DEV", Title: "x", Assignee: "dev-2"})
	if ErrorCode(err) != "workflow_managed" {
		t.Fatalf("assignee error = %v", err)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM tasks WHERE queue_prefix = 'DEV'`); n != 0 {
		t.Fatalf("tasks = %d", n)
	}
}

func TestQueueTriggerTaskPinsTheBinding(t *testing.T) {
	svc, actor, digest := engineFixture(t)
	if _, err := svc.CreateQueueWorkflowTrigger(context.Background(), actor, "DEV",
		CreateQueueWorkflowTriggerInput{Pattern: "metrics:api", Action: "create_task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO channels(name,kind,created_at) VALUES('metrics:api','chat',?) ON CONFLICT DO NOTHING`, svc.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO messages(id,channel,ts,source,type,subject,text,data,kind) VALUES('durable-1','metrics:api',?,'plugin:metrics','metric.alert','{}','API alert','{}','event')`, "2099-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.ReconcileWorkflowObservations(context.Background(), 100); err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v", n, err)
	}
	var pinned, status string
	if err := svc.db.QueryRow(`SELECT workflow_digest, workflow_status FROM tasks WHERE queue_prefix = 'DEV'`).Scan(&pinned, &status); err != nil {
		t.Fatal(err)
	}
	if pinned != digest || status != "develop" {
		t.Fatalf("trigger task pinned %q in %q", pinned, status)
	}
}

func TestEnterPoolStatusReassignsPreviousHolderEvenWithAnotherGoal(t *testing.T) {
	svc, actor, _ := engineFixture(t)
	task := mustCreateDev(t, svc, actor, "t")
	if task.Assignee != "agent:dev-1" {
		t.Fatalf("initial assignee = %q", task.Assignee)
	}
	reviewed := enter(t, svc, task.Key, "review", "ready")
	if reviewed.Assignee != "agent:reviewer-1" || reviewed.Status != StatusInProgress {
		t.Fatalf("review = %#v", reviewed)
	}
	setAgent(t, svc, "dev-1", `current_goal_task_key = 'DEV-other'`)
	back := enter(t, svc, task.Key, "develop", "changes")
	if back.Assignee != "agent:dev-1" || back.Status != StatusInProgress {
		t.Fatalf("back to develop = %#v; want the previous holder", back)
	}
	// A holder that left the pool is not reassigned.
	enter(t, svc, task.Key, "review", "ready")
	developers, err := svc.GetAgentPool(context.Background(), actor, "DEV", "developers")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RebindAgentPool(context.Background(), actor, "DEV", "developers", []string{"dev-2"}, developers.Revision, "drop-dev-1"); err != nil {
		t.Fatal(err)
	}
	again := enter(t, svc, task.Key, "develop", "changes")
	if again.Assignee != "agent:dev-2" || holderOf(t, svc, again, "developers") != "dev-2" {
		t.Fatalf("after leaving the pool = %#v", again)
	}
}

func TestEnterPoolStatusSkipsIneligibleMembers(t *testing.T) {
	for name, assignments := range map[string]string{
		"disabled":      `enabled = 0`,
		"loop disabled": `loop_enabled = 0`,
		"goal disabled": `goal_enabled = 0`,
		"error halt":    `error_reason = 'harness crashed'`,
		"idle stop":     `status_message = 'idle_limit: 3 idle iterations'`,
		"current goal":  `current_goal_task_key = 'TASK-x'`,
	} {
		t.Run(name, func(t *testing.T) {
			svc, actor, _ := engineFixture(t)
			setAgent(t, svc, "dev-1", assignments)
			task := mustCreateDev(t, svc, actor, "t")
			if task.Assignee != "agent:dev-2" {
				t.Fatalf("assignee = %q; want dev-2", task.Assignee)
			}
		})
	}
	t.Run("status line is not a halt", func(t *testing.T) {
		svc, actor, _ := engineFixture(t)
		setAgent(t, svc, "dev-1", `status_message = 'working on idle_limit docs'`)
		if task := mustCreateDev(t, svc, actor, "t"); task.Assignee != "agent:dev-1" {
			t.Fatalf("assignee = %q; want dev-1", task.Assignee)
		}
	})
}

func TestEnterPoolStatusBreaksTiesByLastDispatchThenPoolOrder(t *testing.T) {
	svc, actor, _ := engineFixture(t)
	first := mustCreateDev(t, svc, actor, "first")
	if first.Assignee != "agent:dev-1" {
		t.Fatalf("first = %q; want pool order", first.Assignee)
	}
	// dev-1 now has a dispatch time and dev-2 has none, so dev-2 comes first.
	second := mustCreateDev(t, svc, actor, "second")
	if second.Assignee != "agent:dev-2" {
		t.Fatalf("second = %q; want the never-dispatched member", second.Assignee)
	}
	// dev-2 was dispatched more recently than dev-1.
	if _, err := svc.db.Exec(`UPDATE task_workflow_holders SET dispatched_at = '2026-01-01T00:00:00Z' WHERE agent = 'dev-1'`); err != nil {
		t.Fatal(err)
	}
	third := mustCreateDev(t, svc, actor, "third")
	if third.Assignee != "agent:dev-1" {
		t.Fatalf("third = %q; want the least recently dispatched member", third.Assignee)
	}
}

func TestEnterCustomerStatusOpensOneSystemWait(t *testing.T) {
	svc, actor, _ := engineFixture(t)
	task := mustCreateDev(t, svc, actor, "t")
	waiting := enter(t, svc, task.Key, "approval", "ask")
	if waiting.Status != StatusWaitCustomer || waiting.WaitingOn != WaitingOnCustomer || waiting.Assignee != "agent:dev-1" ||
		waiting.WorkflowStatus != "approval" {
		t.Fatalf("customer status = %#v", waiting)
	}
	waits, err := listOpenWaits(context.Background(), svc.db, waiting)
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0].ExpectedPrincipal != "user:customer" || waits[0].RequestingPrincipal != "system:workflow" {
		t.Fatalf("waits = %#v", waits)
	}
	var author, body string
	if err := svc.db.QueryRow(`SELECT author, body FROM task_comments WHERE id = ?`, waits[0].RequestingCommentID).Scan(&author, &body); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"approval", "approved", "rejected"} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment %q lacks %q", body, want)
		}
	}
	if author != "system:workflow" {
		t.Fatalf("comment author = %q", author)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_notification_outbox o JOIN task_events e ON e.sequence = o.event_sequence
		WHERE e.task_id = ? AND o.message_type = 'task.question' AND o.channel = 'user:customer'`, task.ID); n != 1 {
		t.Fatalf("question notifications = %d", n)
	}

	// A plain customer comment does not leave the status or resolve its wait.
	if _, err := svc.AddComment(context.Background(), actor, task.Key, AddCommentInput{Body: "looking"}); err != nil {
		t.Fatal(err)
	}
	stored, err := taskByKey(svc.db, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusWaitCustomer || countRows(t, svc, `SELECT COUNT(*) FROM task_waiting_for WHERE task_id = ? AND resolved_at = ''`, task.ID) != 1 {
		t.Fatalf("after a plain comment = %#v", stored)
	}

	// Leaving resolves the wait; re-entering opens a new one.
	back := enter(t, svc, task.Key, "develop", "rejected")
	if back.Status != StatusInProgress || countRows(t, svc, `SELECT COUNT(*) FROM task_waiting_for WHERE task_id = ? AND resolved_at = ''`, task.ID) != 0 {
		t.Fatalf("after leaving = %#v", back)
	}
	enter(t, svc, task.Key, "approval", "ask")
	if open, total := countRows(t, svc, `SELECT COUNT(*) FROM task_waiting_for WHERE task_id = ? AND resolved_at = ''`, task.ID),
		countRows(t, svc, `SELECT COUNT(*) FROM task_waiting_for WHERE task_id = ?`, task.ID); open != 1 || total != 2 {
		t.Fatalf("waits open/total = %d/%d", open, total)
	}
}

func TestEnterScriptStatusClearsAssigneeWithoutWait(t *testing.T) {
	svc, actor, _ := engineFixture(t)
	task := mustCreateDev(t, svc, actor, "t")
	merged := enter(t, svc, task.Key, "merge", "approved")
	if merged.Status != StatusWaitCustomer || merged.WaitingOn != WaitingOnScript || merged.Assignee != "" {
		t.Fatalf("script status = %#v", merged)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_waiting_for WHERE task_id = ?`, task.ID); n != 0 {
		t.Fatalf("waits = %d", n)
	}
}

func TestEnterTerminalStatusClosesTheTaskDespiteActiveChildren(t *testing.T) {
	svc, actor, _ := engineFixture(t)
	task := mustCreateDev(t, svc, actor, "t")
	if _, err := svc.CreateTask(context.Background(), actor, CreateTaskInput{ParentKey: task.Key, Title: "child"}); err != nil {
		t.Fatal(err)
	}
	done := enter(t, svc, task.Key, "done", "merged")
	if done.Status != StatusDone || done.CompletedAt == "" || done.Assignee != "agent:dev-1" {
		t.Fatalf("done = %#v", done)
	}
	other := mustCreateDev(t, svc, actor, "other")
	dropped := enter(t, svc, other.Key, "dropped", "")
	if dropped.Status != StatusCancelled || dropped.CompletedAt == "" {
		t.Fatalf("dropped = %#v", dropped)
	}
}

func TestEnterStatusRecordsVisitsAndTransitionEvents(t *testing.T) {
	svc, actor, _ := engineFixture(t)
	task := mustCreateDev(t, svc, actor, "t")
	enter(t, svc, task.Key, "review", "ready")
	final := enter(t, svc, task.Key, "develop", "changes")
	if final.Revision != task.Revision+2 {
		t.Fatalf("revision = %d; want %d", final.Revision, task.Revision+2)
	}
	rows, err := svc.db.Query(`SELECT sequence, status_id, left_at <> '', outcome, message FROM task_status_visits WHERE task_id = ? ORDER BY sequence`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	type visit struct {
		seq              int
		status           string
		left             bool
		outcome, message string
	}
	var got []visit
	for rows.Next() {
		var v visit
		if err := rows.Scan(&v.seq, &v.status, &v.left, &v.outcome, &v.message); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	rows.Close()
	want := []visit{
		{1, "develop", true, "ready", "msg ready"},
		{2, "review", true, "changes", "msg changes"},
		{3, "develop", false, "", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("visits = %#v", got)
	}
	events, err := svc.db.Query(`SELECT payload FROM task_events WHERE task_id = ? AND kind = 'workflow.transitioned' ORDER BY sequence`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var transitions []string
	for events.Next() {
		var raw string
		if err := events.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		transitions = append(transitions, p["from"].(string)+">"+p["to"].(string)+":"+p["outcome"].(string)+":"+p["actor"].(string))
	}
	events.Close()
	if !reflect.DeepEqual(transitions, []string{
		">develop::user:customer", "develop>review:ready:user:customer", "review>develop:changes:user:customer",
	}) {
		t.Fatalf("transitions = %v", transitions)
	}
}

func TestWorkflowTaskRefusesFlexibleLifecycleWrites(t *testing.T) {
	ctx := context.Background()
	svc, actor, _ := engineFixture(t)
	task := mustCreateDev(t, svc, actor, "t")
	status, assignee := StatusDone, "agent:dev-2"
	for name, call := range map[string]func() error{
		"update status": func() error {
			_, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{Status: &status, Revision: task.Revision})
			return err
		},
		"update assignee": func() error {
			_, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{Assignee: &assignee, Revision: task.Revision})
			return err
		},
		"complete": func() error {
			_, err := svc.CompleteTask(ctx, actor, task.Key, CompleteInput{Revision: task.Revision})
			return err
		},
		"complete anyway": func() error {
			_, err := svc.CompleteTask(ctx, actor, task.Key, CompleteInput{Revision: task.Revision, CompleteAnyway: true})
			return err
		},
		"claim": func() error {
			_, err := svc.ClaimTask(ctx, AgentActor("dev-1"), task.Key, task.Revision)
			return err
		},
	} {
		err := call()
		var domain *Error
		if !errors.As(err, &domain) || domain.Code != "workflow_managed" || domain.Status != 409 ||
			domain.Data["status"] != "develop" || !reflect.DeepEqual(domain.Data["outcomes"], []string{"ready", "ask"}) {
			t.Fatalf("%s error = %#v", name, err)
		}
	}
	title, description, pr, block := "New title", "New description", "https://example.test/pr/1", "waiting on infra"
	priority := PriorityP1
	updated, err := svc.UpdateTask(ctx, actor, task.Key, UpdateTaskInput{
		Title: &title, Description: &description, PullRequest: &pr, Priority: &priority, ManualBlockReason: &block,
		Revision: task.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != title || updated.Description != description || updated.PullRequest != pr || updated.Priority != PriorityP1 ||
		updated.ManualBlockReason != block || updated.Status != StatusInProgress || updated.Assignee != "agent:dev-1" {
		t.Fatalf("updated = %#v", updated)
	}
	stored, err := taskByKey(svc.db, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorkflowStatus != "develop" || stored.Status != StatusInProgress {
		t.Fatalf("stored after update = %#v", stored)
	}
}

func TestReadyListExcludesWorkflowTasks(t *testing.T) {
	ctx := context.Background()
	svc, actor, _ := engineFixture(t)
	setAgent(t, svc, "dev-1", `enabled = 0`)
	setAgent(t, svc, "dev-2", `enabled = 0`)
	waiting := mustCreateDev(t, svc, actor, "waiting")
	if waiting.Status != StatusOpen || waiting.Assignee != "" {
		t.Fatalf("waiting = %#v", waiting)
	}
	plain, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "TASK", Title: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := svc.Ready(ctx, actor, ReadyFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].Key != plain.Key {
		t.Fatalf("ready = %#v", ready)
	}
	if _, err := svc.UpdateQueue(ctx, actor, "DEV", UpdateQueueInput{Owners: &[]string{"dev-1"}, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClaimReady(ctx, AgentActor("dev-1"), ReadyFilter{Queue: "DEV"}, ""); ErrorCode(err) != "no_ready_task" {
		t.Fatalf("claim ready = %v", err)
	}
}

func TestTransferRefusesWorkflowTasksAndBoundQueues(t *testing.T) {
	ctx := context.Background()
	svc, actor, _ := engineFixture(t)
	task := mustCreateDev(t, svc, actor, "t")
	if _, err := svc.ExportTask(ctx, actor, task.Key); ErrorCode(err) != "workflow_managed" {
		t.Fatalf("export error = %v", err)
	}
	plain, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "TASK", Title: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := svc.ExportTask(ctx, actor, plain.Key)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Queue = "DEV"
	bundle.Tasks[0].Key = "DEV-zz99"
	bundle.RootKey = "DEV-zz99"
	if _, err := svc.ImportTask(ctx, actor, bundle); ErrorCode(err) != "workflow_managed" {
		t.Fatalf("import error = %v", err)
	}
}

func TestCommentsKeepTheWorkflowQuestionAndHolderQuestionsWork(t *testing.T) {
	ctx := context.Background()
	svc, actor, _ := engineFixture(t)
	task := mustCreateDev(t, svc, actor, "t")
	// The holder's question in a pool status moves the category as on a flexible task.
	if _, err := svc.AddComment(ctx, AgentActor("dev-1"), task.Key, AddCommentInput{Body: "@user:customer which API?"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := taskByKey(svc.db, task.Key); got.Status != StatusWaitCustomer || got.WorkflowStatus != "develop" {
		t.Fatalf("after the holder's question = %#v", got)
	}
	if _, err := svc.AddComment(ctx, actor, task.Key, AddCommentInput{Body: "the v2 one"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := taskByKey(svc.db, task.Key); got.Status != StatusInProgress {
		t.Fatalf("after the answer = %#v", got)
	}
	// In a customer status, a mention does not take over the workflow's wait and
	// a reply does not answer it.
	enter(t, svc, task.Key, "approval", "ask")
	if _, err := svc.AddComment(ctx, AgentActor("dev-1"), task.Key, AddCommentInput{Body: "@user:customer please approve"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddComment(ctx, actor, task.Key, AddCommentInput{Body: "soon"}); err != nil {
		t.Fatal(err)
	}
	got, _ := taskByKey(svc.db, task.Key)
	waits, err := listOpenWaits(ctx, svc.db, got)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusWaitCustomer || len(waits) != 1 || waits[0].RequestingPrincipal != workflowActor {
		t.Fatalf("customer status after comments = %#v, waits %#v", got, waits)
	}
}
