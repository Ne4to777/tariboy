package tasks

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

// requestDefinition is engineDefinition with the artifacts "plan" and
// "summary", a "ready" transition that requires "plan", and an "approve"
// transition that declares a check.
func requestDefinition() workflowfile.File {
	def := engineDefinition()
	def.Artifacts = []workflowfile.Artifact{{Name: "plan"}, {Name: "summary"}}
	def.Statuses[0].Transitions[0].Requires = []string{"plan", "summary"}
	def.Statuses[1].Transitions[0].Checks = []workflowfile.Check{{Script: "checks/ci.sh"}}
	def.Statuses[2].Instructions = "./statuses/approval.md"
	return def
}

// requestFixture binds DEV to requestDefinition and creates one task held by
// dev-1 in "develop".
func requestFixture(t *testing.T) (*Service, Actor, Task) {
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
	seedEngineImage(t, svc, requestDefinition())
	if _, err := svc.SetQueueWorkflow(context.Background(), actor, "DEV", "development:0.1.0", 0); err != nil {
		t.Fatal(err)
	}
	created := mustCreateDev(t, svc, actor, "requests")
	task, err := taskByKey(svc.db, created.Key)
	if err != nil {
		t.Fatal(err)
	}
	if task.Assignee != "agent:dev-1" || task.WorkflowStatus != "develop" {
		t.Fatalf("fixture task = %#v", task)
	}
	return svc, actor, task
}

func eventPayload(t *testing.T, svc *Service, task Task, kind string) map[string]any {
	t.Helper()
	var raw string
	if err := svc.db.QueryRow(`SELECT payload FROM task_events WHERE task_id = ? AND kind = ? ORDER BY sequence DESC LIMIT 1`,
		task.ID, kind).Scan(&raw); err != nil {
		t.Fatalf("event %s: %v", kind, err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func insertPendingRequest(t *testing.T, svc *Service, task Task) int64 {
	t.Helper()
	result, err := svc.db.Exec(`
		INSERT INTO task_transition_requests(task_id, visit_id, outcome, actor, state, created_at)
		SELECT ?, id, 'ready', 'agent:dev-1', 'pending', '2026-10-02T00:00:00Z'
		FROM task_status_visits WHERE task_id = ? AND left_at = ''`, task.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func requestState(t *testing.T, svc *Service, id int64) string {
	t.Helper()
	var state string
	if err := svc.db.QueryRow(`SELECT state FROM task_transition_requests WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func setPlanAndSummary(t *testing.T, svc *Service, actor Actor, key string) {
	t.Helper()
	for _, name := range []string{"plan", "summary"} {
		if _, err := svc.SetArtifact(context.Background(), actor, key, name, name+" text"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdvanceAppliesATransitionWithoutChecks(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	holder := AgentActor("dev-1")
	setPlanAndSummary(t, svc, holder, task.Key)
	got, err := svc.Advance(ctx, holder, task.Key, AdvanceInput{Outcome: "ready", Message: "PR opened"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 || got.TaskKey != task.Key || got.Outcome != "ready" || got.Message != "PR opened" ||
		got.Actor != "agent:dev-1" || got.State != "applied" || got.CreatedAt == "" || got.FinishedAt == "" {
		t.Fatalf("request = %#v", got)
	}
	stored, err := taskByKey(svc.db, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorkflowStatus != "review" || stored.Status != StatusInProgress || stored.Assignee != "agent:reviewer-1" {
		t.Fatalf("stored = %#v", stored)
	}
	var outcome, message, leftAt string
	if err := svc.db.QueryRow(`SELECT outcome, message, left_at FROM task_status_visits WHERE task_id = ? AND sequence = 1`,
		task.ID).Scan(&outcome, &message, &leftAt); err != nil {
		t.Fatal(err)
	}
	if outcome != "ready" || message != "PR opened" || leftAt == "" {
		t.Fatalf("closed visit = %q %q %q", outcome, message, leftAt)
	}
	var status, enteredBy string
	if err := svc.db.QueryRow(`SELECT status_id, entered_by FROM task_status_visits WHERE task_id = ? AND left_at = ''`,
		task.ID).Scan(&status, &enteredBy); err != nil {
		t.Fatal(err)
	}
	if status != "review" || enteredBy != "agent:dev-1" {
		t.Fatalf("open visit = %q %q", status, enteredBy)
	}
	requested := eventPayload(t, svc, task, "workflow.transition_requested")
	if requested["outcome"] != "ready" || requested["actor"] != "agent:dev-1" || requested["state"] != "applied" ||
		requested["request_id"] != float64(got.ID) {
		t.Fatalf("requested payload = %v", requested)
	}
	transitioned := eventPayload(t, svc, task, "workflow.transitioned")
	if transitioned["from"] != "develop" || transitioned["to"] != "review" || transitioned["outcome"] != "ready" {
		t.Fatalf("transitioned payload = %v", transitioned)
	}
	// Nothing outside the new visit resets: artifacts and the developers'
	// holder stay.
	if list, err := svc.ListArtifacts(ctx, actor, task.Key); err != nil || len(list) != 2 {
		t.Fatalf("artifacts = %v, %v", list, err)
	}
	if got := holderOf(t, svc, task, "developers"); got != "dev-1" {
		t.Fatalf("developers holder = %q", got)
	}
}

func TestAdvanceByTheCustomerResolvesTheCustomerWait(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	waiting := enter(t, svc, task.Key, "approval", "ask")
	if waiting.WaitingOn != WaitingOnCustomer {
		t.Fatalf("waiting = %#v", waiting)
	}
	if _, err := svc.Advance(ctx, AgentActor("dev-1"), task.Key, AdvanceInput{Outcome: "approved"}); ErrorCode(err) != "not_holder" {
		t.Fatalf("agent in a customer status: %v", err)
	}
	if _, err := svc.Advance(ctx, actor, task.Key, AdvanceInput{Outcome: "rejected"}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_waiting_for WHERE task_id = ? AND resolved_at = ''`, task.ID); n != 0 {
		t.Fatalf("open waits = %d", n)
	}
	stored, _ := taskByKey(svc.db, task.Key)
	if stored.WorkflowStatus != "develop" || stored.Status != StatusInProgress || stored.Assignee != "agent:dev-1" {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestAdvanceRefusals(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	holder := AgentActor("dev-1")

	if _, err := svc.Advance(ctx, holder, "DEV-nope", AdvanceInput{Outcome: "ready"}); ErrorCode(err) != "not_found" {
		t.Fatalf("unknown task: %v", err)
	}
	flexible, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "TASK", Title: "flex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Advance(ctx, actor, flexible.Key, AdvanceInput{Outcome: "ready"}); ErrorCode(err) != "workflow_not_bound" {
		t.Fatalf("flexible: %v", err)
	}
	if _, err := svc.Advance(ctx, holder, task.Key, AdvanceInput{Outcome: "ready", Message: strings.Repeat("x", 4097)}); ErrorCode(err) != "invalid_message" {
		t.Fatalf("long message: %v", err)
	}
	// The customer does not hold a pool status.
	if _, err := svc.Advance(ctx, actor, task.Key, AdvanceInput{Outcome: "ready"}); ErrorCode(err) != "not_holder" || ErrorStatus(err) != 403 {
		t.Fatalf("customer in a pool status: %v", err)
	}
	_, err = svc.Advance(ctx, holder, task.Key, AdvanceInput{Outcome: "merged"})
	if ErrorCode(err) != "outcome_unknown" || ErrorStatus(err) != 400 ||
		!reflect.DeepEqual(err.(*Error).Data["outcomes"], []string{"ready", "ask"}) {
		t.Fatalf("unknown outcome: %#v", err)
	}
	if _, err := svc.SetArtifact(ctx, holder, task.Key, "plan", "x"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Advance(ctx, holder, task.Key, AdvanceInput{Outcome: "ready"})
	if ErrorCode(err) != "artifact_missing" || ErrorStatus(err) != 409 ||
		!reflect.DeepEqual(err.(*Error).Data["missing"], []string{"summary"}) {
		t.Fatalf("missing artifact: %#v", err)
	}
	if _, err := svc.SetArtifact(ctx, holder, task.Key, "summary", "x"); err != nil {
		t.Fatal(err)
	}
	pending := insertPendingRequest(t, svc, task)
	if _, err := svc.Advance(ctx, holder, task.Key, AdvanceInput{Outcome: "ready"}); ErrorCode(err) != "transition_pending" {
		t.Fatalf("pending: %v", err)
	}
	if _, err := svc.db.Exec(`DELETE FROM task_transition_requests WHERE id = ?`, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE tasks SET workflow_paused_reason = 'idle' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Advance(ctx, holder, task.Key, AdvanceInput{Outcome: "ready"}); ErrorCode(err) != "workflow_paused" {
		t.Fatalf("paused: %v", err)
	}
	if _, err := svc.db.Exec(`UPDATE tasks SET workflow_paused_reason = '' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}

	// A transition with checks is refused and writes nothing.
	enter(t, svc, task.Key, "review", "ready")
	before := countRows(t, svc, `SELECT COUNT(*) FROM task_events WHERE task_id = ?`, task.ID)
	reviewer := AgentActor("reviewer-1")
	if _, err := svc.Advance(ctx, reviewer, task.Key, AdvanceInput{Outcome: "approve"}); ErrorCode(err) != "checks_unavailable" || ErrorStatus(err) != 409 {
		t.Fatalf("checks: %v", err)
	}
	if after := countRows(t, svc, `SELECT COUNT(*) FROM task_events WHERE task_id = ?`, task.ID); after != before {
		t.Fatalf("events %d -> %d", before, after)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_transition_requests WHERE task_id = ?`, task.ID); n != 0 {
		t.Fatalf("requests = %d", n)
	}

	// Nobody advances a script status.
	enter(t, svc, task.Key, "merge", "approved")
	if _, err := svc.Advance(ctx, actor, task.Key, AdvanceInput{Outcome: "merged"}); ErrorCode(err) != "not_holder" {
		t.Fatalf("customer in a script status: %v", err)
	}
	enter(t, svc, task.Key, "done", "merged")
	if _, err := svc.Advance(ctx, actor, task.Key, AdvanceInput{Outcome: "merged"}); ErrorCode(err) != "workflow_closed" {
		t.Fatalf("closed: %v", err)
	}
}

func TestAdvanceRacingRequestsApplyOnce(t *testing.T) {
	svc, actor, task := requestFixture(t)
	enter(t, svc, task.Key, "approval", "ask")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Advance(context.Background(), actor, task.Key, AdvanceInput{Outcome: "approved"})
		}(i)
	}
	wg.Wait()
	applied := 0
	for _, err := range errs {
		switch {
		case err == nil:
			applied++
		case ErrorStatus(err) >= 400 && ErrorStatus(err) < 500:
			// The loser sees the winner's status: the transaction serializes
			// on the single connection, so the task has left "approval".
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if applied != 1 {
		t.Fatalf("applied = %d, errs = %v", applied, errs)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_transition_requests WHERE task_id = ? AND state = 'applied'`, task.ID); n != 1 {
		t.Fatalf("applied requests = %d", n)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_status_visits WHERE task_id = ? AND status_id = 'merge'`, task.ID); n != 1 {
		t.Fatalf("merge visits = %d", n)
	}
}

func TestMoveWorkflow(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	if _, err := svc.MoveWorkflow(ctx, AgentActor("dev-1"), task.Key, "approval", "skip"); ErrorCode(err) != "forbidden" || ErrorStatus(err) != 403 {
		t.Fatalf("agent: %v", err)
	}
	if _, err := svc.MoveWorkflow(ctx, actor, task.Key, "approval", "  "); ErrorCode(err) != "reason_required" {
		t.Fatalf("empty reason: %v", err)
	}
	if _, err := svc.MoveWorkflow(ctx, actor, task.Key, "nowhere", "skip"); ErrorCode(err) != "status_unknown" {
		t.Fatalf("unknown status: %v", err)
	}
	flexible, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "TASK", Title: "flex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MoveWorkflow(ctx, actor, flexible.Key, "approval", "skip"); ErrorCode(err) != "workflow_not_bound" {
		t.Fatalf("flexible: %v", err)
	}

	pending := insertPendingRequest(t, svc, task)
	moved, err := svc.MoveWorkflow(ctx, actor, task.Key, "approval", " skip review ")
	if err != nil {
		t.Fatal(err)
	}
	if moved.WorkflowStatus != "approval" || moved.Category != StatusWaitCustomer || moved.WaitingOn != WaitingOnCustomer {
		t.Fatalf("moved = %#v", moved)
	}
	if state := requestState(t, svc, pending); state != "cancelled" {
		t.Fatalf("pending request = %s", state)
	}
	payload := eventPayload(t, svc, task, "workflow.moved")
	if payload["from"] != "develop" || payload["to"] != "approval" || payload["actor"] != "user:customer" || payload["reason"] != "skip review" {
		t.Fatalf("moved payload = %v", payload)
	}
	var outcome, message string
	if err := svc.db.QueryRow(`SELECT outcome, message FROM task_status_visits WHERE task_id = ? AND sequence = 1`,
		task.ID).Scan(&outcome, &message); err != nil {
		t.Fatal(err)
	}
	if outcome != "" || message != "skip review" {
		t.Fatalf("closed visit = %q %q", outcome, message)
	}

	// Out of a terminal status: the task reopens.
	if _, err := svc.MoveWorkflow(ctx, actor, task.Key, "dropped", "abandon"); err != nil {
		t.Fatal(err)
	}
	closed, _ := taskByKey(svc.db, task.Key)
	if closed.Status != StatusCancelled || closed.CompletedAt == "" {
		t.Fatalf("closed = %#v", closed)
	}
	reopened, err := svc.MoveWorkflow(ctx, actor, task.Key, "develop", "try again")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.WorkflowStatus != "develop" || reopened.Status != StatusInProgress || reopened.CompletedAt != "" ||
		reopened.Assignee != "agent:dev-1" {
		t.Fatalf("reopened = %#v", reopened)
	}

	// A paused task is unpaused by the move.
	if _, err := svc.db.Exec(`UPDATE tasks SET workflow_paused_reason = 'idle' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	unpaused, err := svc.MoveWorkflow(ctx, actor, task.Key, "review", "hand over")
	if err != nil {
		t.Fatal(err)
	}
	if unpaused.WorkflowPausedReason != "" || unpaused.Assignee != "agent:reviewer-1" || unpaused.Status != StatusInProgress {
		t.Fatalf("unpaused = %#v", unpaused)
	}
	stored, _ := taskByKey(svc.db, task.Key)
	if stored.WorkflowPausedReason != "" || stored.Revision != unpaused.Revision {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestCancelWorkflowTask(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	if _, err := svc.CancelWorkflowTask(ctx, AgentActor("dev-1"), task.Key); ErrorCode(err) != "forbidden" {
		t.Fatalf("agent: %v", err)
	}
	flexible, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "TASK", Title: "flex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelWorkflowTask(ctx, actor, flexible.Key); ErrorCode(err) != "workflow_not_bound" {
		t.Fatalf("flexible: %v", err)
	}
	enter(t, svc, task.Key, "approval", "ask")
	pending := insertPendingRequest(t, svc, task)
	if _, err := svc.db.Exec(`UPDATE tasks SET workflow_paused_reason = 'idle' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := svc.CancelWorkflowTask(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != StatusCancelled || cancelled.Category != StatusCancelled || cancelled.CompletedAt == "" ||
		cancelled.WorkflowStatus != "approval" || cancelled.WorkflowPausedReason != "" || cancelled.WaitingOn != "" {
		t.Fatalf("cancelled = %#v", cancelled)
	}
	stored, _ := taskByKey(svc.db, task.Key)
	if stored.Status != StatusCancelled || stored.WorkflowStatus != "approval" || stored.CompletedAt == "" ||
		stored.WorkflowPausedReason != "" || stored.Revision != cancelled.Revision {
		t.Fatalf("stored = %#v", stored)
	}
	if state := requestState(t, svc, pending); state != "cancelled" {
		t.Fatalf("pending request = %s", state)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_waiting_for WHERE task_id = ? AND resolved_at = ''`, task.ID); n != 0 {
		t.Fatalf("open waits = %d", n)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_status_visits WHERE task_id = ? AND left_at = ''`, task.ID); n != 0 {
		t.Fatalf("open visits = %d", n)
	}
	var message string
	if err := svc.db.QueryRow(`SELECT message FROM task_status_visits WHERE task_id = ? ORDER BY sequence DESC LIMIT 1`,
		task.ID).Scan(&message); err != nil {
		t.Fatal(err)
	}
	if message != "cancelled" {
		t.Fatalf("visit message = %q", message)
	}
	payload := eventPayload(t, svc, task, "workflow.cancelled")
	if payload["status"] != "approval" || payload["actor"] != "user:customer" {
		t.Fatalf("cancelled payload = %v", payload)
	}
	if _, err := svc.CancelWorkflowTask(ctx, actor, task.Key); ErrorCode(err) != "workflow_closed" {
		t.Fatalf("again: %v", err)
	}
}

func TestAdvanceByAgentsThatDoNotOwnTheStatus(t *testing.T) {
	svc, _, task := requestFixture(t)
	ctx := context.Background()
	unchanged := func(status string) {
		t.Helper()
		stored, err := taskByKey(svc.db, task.Key)
		if err != nil {
			t.Fatal(err)
		}
		if stored.WorkflowStatus != status || stored.Revision != task.Revision {
			t.Fatalf("task moved: %q revision %d -> %d", stored.WorkflowStatus, task.Revision, stored.Revision)
		}
	}
	// Agents with no read access to the task do not learn it exists: another
	// member of the holder's pool and an agent of an unrelated pool alike get
	// not_found.
	for _, name := range []string{"dev-2", "reviewer-1"} {
		if _, err := svc.Advance(ctx, AgentActor(name), task.Key, AdvanceInput{Outcome: "ask"}); ErrorCode(err) != "not_found" || ErrorStatus(err) != 404 {
			t.Fatalf("%s without access: %v", name, err)
		}
	}
	unchanged("develop")
	// A pool member that may read the task (here as a queue owner) is still not
	// the holder.
	if _, err := svc.db.Exec(`INSERT INTO task_queue_owners(queue_prefix, agent) VALUES ('DEV', 'dev-2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Advance(ctx, AgentActor("dev-2"), task.Key, AdvanceInput{Outcome: "ask"}); ErrorCode(err) != "not_holder" || ErrorStatus(err) != 403 {
		t.Fatalf("pool member with access: %v", err)
	}
	unchanged("develop")
	// No agent advances a script status.
	task = enter(t, svc, task.Key, "merge", "approved")
	if _, err := svc.Advance(ctx, AgentActor("dev-2"), task.Key, AdvanceInput{Outcome: "merged"}); ErrorCode(err) != "not_holder" || ErrorStatus(err) != 403 {
		t.Fatalf("agent in a script status: %v", err)
	}
	unchanged("merge")
}

func TestAdvanceInAStatusTheManifestDoesNotDeclare(t *testing.T) {
	svc, actor, task := requestFixture(t)
	if _, err := svc.db.Exec(`UPDATE tasks SET workflow_status = 'gone' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Advance(context.Background(), actor, task.Key, AdvanceInput{Outcome: "ready"}); ErrorCode(err) != "status_unknown" || ErrorStatus(err) != 409 {
		t.Fatalf("unknown status: %v", err)
	}
}

func TestDispatchPendingSkipsACancelledWorkflowTask(t *testing.T) {
	svc, actor, _ := requestFixture(t)
	ctx := context.Background()
	setAgent(t, svc, "dev-1", "enabled = 0")
	setAgent(t, svc, "dev-2", "enabled = 0")
	waiting := mustCreateDev(t, svc, actor, "nobody free")
	if waiting.Status != StatusOpen || waiting.Assignee != "" {
		t.Fatalf("waiting = %#v", waiting)
	}
	if _, err := svc.CancelWorkflowTask(ctx, actor, waiting.Key); err != nil {
		t.Fatal(err)
	}
	setAgent(t, svc, "dev-1", "enabled = 1")
	setAgent(t, svc, "dev-2", "enabled = 1")
	if n, err := svc.DispatchPending(ctx); err != nil || n != 0 {
		t.Fatalf("dispatched = %d, %v", n, err)
	}
	stored, _ := taskByKey(svc.db, waiting.Key)
	if stored.Status != StatusCancelled || stored.Assignee != "" {
		t.Fatalf("stored = %#v", stored)
	}
}

// openWaitsBy counts the task's open waits that requester asked of expected.
func openWaitsBy(t *testing.T, svc *Service, task Task, requester, expected string) int {
	t.Helper()
	return countRows(t, svc, `SELECT COUNT(*) FROM task_waiting_for
		WHERE task_id = ? AND requesting_principal = ? AND expected_principal = ? AND resolved_at = ''`,
		task.ID, requester, expected)
}

// holderAsks has the holder dev-1 ask the customer and dev-2 on the task.
func holderAsks(t *testing.T, svc *Service, task Task) {
	t.Helper()
	for _, body := range []string{"@user:customer which API?", "@agent:dev-2 seen this before?"} {
		if _, err := svc.AddComment(context.Background(), AgentActor("dev-1"), task.Key, AddCommentInput{Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	if openWaitsBy(t, svc, task, "agent:dev-1", "user:customer") != 1 || openWaitsBy(t, svc, task, "agent:dev-1", "agent:dev-2") != 1 {
		t.Fatal("the holder's questions are not open")
	}
	if got, _ := taskByKey(svc.db, task.Key); got.Status != StatusWaitCustomer || got.WaitingOn != WaitingOnCustomer {
		t.Fatalf("after the question = %#v", got)
	}
}

func TestLeavingAPoolStatusResolvesTheHoldersQuestionsToTheCustomer(t *testing.T) {
	ctx := context.Background()
	t.Run("advance", func(t *testing.T) {
		svc, _, task := requestFixture(t)
		holderAsks(t, svc, task)
		setPlanAndSummary(t, svc, AgentActor("dev-1"), task.Key)
		if _, err := svc.Advance(ctx, AgentActor("dev-1"), task.Key, AdvanceInput{Outcome: "ready"}); err != nil {
			t.Fatal(err)
		}
		if openWaitsBy(t, svc, task, "agent:dev-1", "user:customer") != 0 || openWaitsBy(t, svc, task, "agent:dev-1", "agent:dev-2") != 1 {
			t.Fatal("waits after the advance")
		}
		got, _ := taskByKey(svc.db, task.Key)
		if got.WorkflowStatus != "review" || got.Category != StatusInProgress || got.WaitingOn != "" {
			t.Fatalf("after the advance = %#v", got)
		}
	})
	t.Run("move", func(t *testing.T) {
		svc, actor, task := requestFixture(t)
		holderAsks(t, svc, task)
		if _, err := svc.MoveWorkflow(ctx, actor, task.Key, "approval", "decide now"); err != nil {
			t.Fatal(err)
		}
		if openWaitsBy(t, svc, task, "agent:dev-1", "user:customer") != 0 || openWaitsBy(t, svc, task, "agent:dev-1", "agent:dev-2") != 1 ||
			openWaitsBy(t, svc, task, workflowActor, "user:customer") != 1 {
			t.Fatal("waits after the move")
		}
		got, _ := taskByKey(svc.db, task.Key)
		if got.WorkflowStatus != "approval" || got.Category != StatusWaitCustomer || got.WaitingOn != WaitingOnCustomer {
			t.Fatalf("after the move = %#v", got)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		svc, actor, task := requestFixture(t)
		holderAsks(t, svc, task)
		if _, err := svc.CancelWorkflowTask(ctx, actor, task.Key); err != nil {
			t.Fatal(err)
		}
		if openWaitsBy(t, svc, task, "agent:dev-1", "user:customer") != 0 || openWaitsBy(t, svc, task, "agent:dev-1", "agent:dev-2") != 1 {
			t.Fatal("waits after the cancel")
		}
		got, _ := taskByKey(svc.db, task.Key)
		if got.Category != StatusCancelled || got.WaitingOn != "" {
			t.Fatalf("after the cancel = %#v", got)
		}
	})
	t.Run("flexible task", func(t *testing.T) {
		svc, actor, _ := requestFixture(t)
		flexible, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "TASK", Title: "flex", Assignee: "agent:dev-1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AddComment(ctx, AgentActor("dev-1"), flexible.Key, AddCommentInput{Body: "@user:customer which API?"}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.MoveWorkflow(ctx, actor, flexible.Key, "review", "x"); ErrorCode(err) != "workflow_not_bound" {
			t.Fatalf("move: %v", err)
		}
		if _, err := svc.CancelWorkflowTask(ctx, actor, flexible.Key); ErrorCode(err) != "workflow_not_bound" {
			t.Fatalf("cancel: %v", err)
		}
		if openWaitsBy(t, svc, flexible, "agent:dev-1", "user:customer") != 1 {
			t.Fatal("the flexible task's question was resolved")
		}
	})
}

// applyTransitionTx is what a later script runner calls for a pending request:
// the request becomes applied and the task enters the target status.
func TestApplyTransitionAppliesAPendingRequest(t *testing.T) {
	ctx := context.Background()
	svc, _, task := requestFixture(t)
	id := insertPendingRequest(t, svc, task)
	tx, err := svc.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	manifest, err := loadManifestTx(ctx, tx, task.WorkflowDigest)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := currentStatus(manifest, "develop")
	transition, _ := statusTransition(status, "ready")
	if err := svc.applyTransitionTx(ctx, tx, &task, manifest, id, transition, "agent:dev-1", "checks passed"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var state, finishedAt string
	if err := svc.db.QueryRow(`SELECT state, finished_at FROM task_transition_requests WHERE id = ?`, id).Scan(&state, &finishedAt); err != nil {
		t.Fatal(err)
	}
	if state != "applied" || finishedAt == "" {
		t.Fatalf("request = %q finished %q", state, finishedAt)
	}
	stored, err := taskByKey(svc.db, task.Key)
	if err != nil || stored.WorkflowStatus != "review" || stored.Assignee != "agent:reviewer-1" {
		t.Fatalf("stored = %#v, %v", stored, err)
	}
	transitioned := eventPayload(t, svc, task, "workflow.transitioned")
	if transitioned["outcome"] != "ready" || transitioned["message"] != "checks passed" || transitioned["actor"] != "agent:dev-1" {
		t.Fatalf("transitioned payload = %v", transitioned)
	}
}

// seedOpenWait records an open wait requester asked of expected on task.
func seedOpenWait(t *testing.T, svc *Service, task Task, requester, expected string) {
	t.Helper()
	result, err := svc.db.Exec(`INSERT INTO task_comments(task_id, author, body, created_at, updated_at) VALUES (?, ?, 'q', 'n', 'n')`,
		task.ID, requester)
	if err != nil {
		t.Fatal(err)
	}
	commentID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`
		INSERT INTO task_waiting_for(task_id, expected_principal, requesting_principal, requesting_comment_id, requested_at, resolved_at)
		VALUES (?, ?, ?, ?, 'n', '')`, task.ID, expected, requester, commentID); err != nil {
		t.Fatal(err)
	}
}

// A system:workflow wait (a later pause opens one in any status) is resolved
// whenever its status is left, whatever owns that status, and on cancel.
func TestLeavingAnyStatusResolvesTheWorkflowsOwnWait(t *testing.T) {
	ctx := context.Background()
	t.Run("move out of a pool status", func(t *testing.T) {
		svc, actor, task := requestFixture(t)
		seedOpenWait(t, svc, task, workflowActor, "user:customer")
		if _, err := svc.MoveWorkflow(ctx, actor, task.Key, "review", "hand over"); err != nil {
			t.Fatal(err)
		}
		if n := openWaitsBy(t, svc, task, workflowActor, "user:customer"); n != 0 {
			t.Fatalf("open workflow waits after the move = %d", n)
		}
	})
	t.Run("move out of a script status", func(t *testing.T) {
		svc, actor, task := requestFixture(t)
		enter(t, svc, task.Key, "merge", "")
		seedOpenWait(t, svc, task, workflowActor, "user:customer")
		if _, err := svc.MoveWorkflow(ctx, actor, task.Key, "develop", "redo"); err != nil {
			t.Fatal(err)
		}
		if n := openWaitsBy(t, svc, task, workflowActor, "user:customer"); n != 0 {
			t.Fatalf("open workflow waits after the move = %d", n)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		svc, actor, task := requestFixture(t)
		seedOpenWait(t, svc, task, workflowActor, "user:customer")
		if _, err := svc.CancelWorkflowTask(ctx, actor, task.Key); err != nil {
			t.Fatal(err)
		}
		if n := openWaitsBy(t, svc, task, workflowActor, "user:customer"); n != 0 {
			t.Fatalf("open workflow waits after the cancel = %d", n)
		}
	})
}

// Leaving a pool status resolves the holder's questions only; another agent's
// question to the customer on the same task stays open. (A task has at most one
// open wait per expected principal, so the holder's own question is covered by
// TestLeavingAPoolStatusResolvesTheHoldersQuestionsToTheCustomer.)
func TestLeavingAPoolStatusKeepsOtherAgentsQuestions(t *testing.T) {
	ctx := context.Background()
	t.Run("advance", func(t *testing.T) {
		svc, _, task := requestFixture(t)
		seedOpenWait(t, svc, task, "agent:dev-2", "user:customer")
		setPlanAndSummary(t, svc, AgentActor("dev-1"), task.Key)
		if _, err := svc.Advance(ctx, AgentActor("dev-1"), task.Key, AdvanceInput{Outcome: "ready"}); err != nil {
			t.Fatal(err)
		}
		if openWaitsBy(t, svc, task, "agent:dev-2", "user:customer") != 1 {
			t.Fatal("another agent's question was resolved by the advance")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		svc, actor, task := requestFixture(t)
		seedOpenWait(t, svc, task, "agent:dev-2", "user:customer")
		if _, err := svc.CancelWorkflowTask(ctx, actor, task.Key); err != nil {
			t.Fatal(err)
		}
		if openWaitsBy(t, svc, task, "agent:dev-2", "user:customer") != 1 {
			t.Fatal("another agent's question was resolved by the cancel")
		}
	})
}

func TestAdvanceFromGuardsTheStatusTheCallerSaw(t *testing.T) {
	ctx := context.Background()
	holder := AgentActor("dev-1")
	t.Run("mismatch is refused and writes nothing", func(t *testing.T) {
		svc, _, task := requestFixture(t)
		before := countRows(t, svc, `SELECT COUNT(*) FROM task_events WHERE task_id = ?`, task.ID)
		_, err := svc.Advance(ctx, holder, task.Key, AdvanceInput{Outcome: "ask", From: "review"})
		if ErrorCode(err) != "status_changed" || ErrorStatus(err) != 409 || err.(*Error).Data["status"] != "develop" {
			t.Fatalf("stale from: %#v", err)
		}
		if after := countRows(t, svc, `SELECT COUNT(*) FROM task_events WHERE task_id = ?`, task.ID); after != before {
			t.Fatalf("events %d -> %d", before, after)
		}
		if n := countRows(t, svc, `SELECT COUNT(*) FROM task_transition_requests WHERE task_id = ?`, task.ID); n != 0 {
			t.Fatalf("requests = %d", n)
		}
	})
	t.Run("mismatch is checked before ownership", func(t *testing.T) {
		svc, actor, task := requestFixture(t)
		if _, err := svc.Advance(ctx, actor, task.Key, AdvanceInput{Outcome: "ask", From: "approval"}); ErrorCode(err) != "status_changed" {
			t.Fatalf("customer with a stale from: %v", err)
		}
	})
	for name, from := range map[string]string{"matching from": "develop", "empty from": ""} {
		t.Run(name+" applies", func(t *testing.T) {
			svc, _, task := requestFixture(t)
			if _, err := svc.Advance(ctx, holder, task.Key, AdvanceInput{Outcome: "ask", From: from}); err != nil {
				t.Fatal(err)
			}
			stored, err := taskByKey(svc.db, task.Key)
			if err != nil || stored.WorkflowStatus != "approval" {
				t.Fatalf("stored = %#v, %v", stored, err)
			}
		})
	}
}

// A retried advance whose first attempt applied is refused when the new status
// is owned by the same caller and declares the same outcome.
func TestAdvanceRetryWithFromAppliesOnce(t *testing.T) {
	ctx := context.Background()
	svc, actor := workflowFixture(t)
	customer := workflowfile.Owner{Kind: workflowfile.OwnerCustomer}
	seedEngineImage(t, svc, workflowfile.File{
		SchemaVersion: 1, Name: "signoff", WorkflowVersion: "0.1.0", InitialStatus: "first",
		Statuses: []workflowfile.Status{
			{ID: "first", Owner: customer, Transitions: []workflowfile.Transition{{On: "ok", To: "second"}}},
			{ID: "second", Owner: customer, Transitions: []workflowfile.Transition{{On: "ok", To: "done"}}},
			{ID: "done", Terminal: true},
		},
	})
	if _, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "signoff:0.1.0", 0); err != nil {
		t.Fatal(err)
	}
	task := mustCreateDev(t, svc, actor, "retry")
	if _, err := svc.Advance(ctx, actor, task.Key, AdvanceInput{Outcome: "ok", From: "first"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Advance(ctx, actor, task.Key, AdvanceInput{Outcome: "ok", From: "first"})
	if ErrorCode(err) != "status_changed" || err.(*Error).Data["status"] != "second" {
		t.Fatalf("retry: %#v", err)
	}
	stored, err := taskByKey(svc.db, task.Key)
	if err != nil || stored.WorkflowStatus != "second" {
		t.Fatalf("stored = %#v, %v", stored, err)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_transition_requests WHERE task_id = ?`, task.ID); n != 1 {
		t.Fatalf("requests = %d", n)
	}
}
