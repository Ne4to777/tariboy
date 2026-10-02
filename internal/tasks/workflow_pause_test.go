package tasks

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// pause pauses the task through pauseTx in its own transaction, signals the
// way a caller does after commit, and returns the stored task.
func pause(t *testing.T, svc *Service, key, reason, detail string) Task {
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
	if err := svc.pauseTx(ctx, tx, &task, manifest, reason, detail); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	svc.signal()
	return storedTask(t, svc, key)
}

// pauseWait is an open wait of a task and the body of the comment that asked.
type pauseWait struct {
	expected, requesting, body string
	commentID                  int64
}

func openWaitRows(t *testing.T, svc *Service, task Task) []pauseWait {
	t.Helper()
	rows, err := svc.db.Query(`
		SELECT w.expected_principal, w.requesting_principal, w.requesting_comment_id, COALESCE(c.body, '')
		FROM task_waiting_for w LEFT JOIN task_comments c ON c.id = w.requesting_comment_id
		WHERE w.task_id = ? AND w.resolved_at = '' ORDER BY w.id`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []pauseWait
	for rows.Next() {
		var w pauseWait
		if err := rows.Scan(&w.expected, &w.requesting, &w.commentID, &w.body); err != nil {
			t.Fatal(err)
		}
		out = append(out, w)
	}
	return out
}

// visitCounters returns idle_iterations, rejected_requests, and script_failures
// of the open visit.
func visitCounters(t *testing.T, svc *Service, task Task) [3]int {
	t.Helper()
	var c [3]int
	if err := svc.db.QueryRow(`
		SELECT idle_iterations, rejected_requests, script_failures FROM task_status_visits
		WHERE task_id = ? AND left_at = ''`, task.ID).Scan(&c[0], &c[1], &c[2]); err != nil {
		t.Fatal(err)
	}
	return c
}

func setCounters(t *testing.T, svc *Service, task Task) {
	t.Helper()
	if _, err := svc.db.Exec(`
		UPDATE task_status_visits SET idle_iterations = 2, rejected_requests = 4, script_failures = 1
		WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		t.Fatal(err)
	}
}

func holderRow(t *testing.T, svc *Service, task Task, pool string) (string, bool) {
	t.Helper()
	var agent string
	var released bool
	err := svc.db.QueryRow(`SELECT agent, released FROM task_workflow_holders WHERE task_id = ? AND pool = ?`,
		task.ID, pool).Scan(&agent, &released)
	if err != nil {
		return "", false
	}
	return agent, released
}

func countEvents(t *testing.T, svc *Service, task Task, kind string) int {
	t.Helper()
	return countRows(t, svc, `SELECT COUNT(*) FROM task_events WHERE task_id = ? AND kind = ?`, task.ID, kind)
}

func TestPauseStopsTheVisitAndAsksTheCustomer(t *testing.T) {
	svc, actor, task, goals := runFixture(t)
	ctx := context.Background()
	request, run := advanceApprove(t, svc, actor, task)
	if ok, err := svc.ClaimScriptRun(ctx, run.ID, "2026-07-31T12:00:01Z", "/runs/1/run.log"); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	before := goals.Load()
	paused := pause(t, svc, task.Key, PauseRejectedRequests, "ci kept failing")
	if goals.Load() == before {
		t.Fatal("no goal signal after the pause")
	}
	if paused.WorkflowPausedReason != PauseRejectedRequests || paused.Status != StatusWaitCustomer ||
		paused.Category != StatusWaitCustomer || paused.WaitingOn != WaitingOnPause ||
		paused.WorkflowStatus != "review" || paused.Assignee != "agent:reviewer-1" || paused.Revision != task.Revision+1 {
		t.Fatalf("paused = %#v", paused)
	}
	if fields := jsonFields(t, paused); fields["waiting_on"] != "pause" || fields["status"] != "review" ||
		fields["workflow_paused_reason"] != PauseRejectedRequests {
		t.Fatalf("json = %v", fields)
	}
	if state := requestState(t, svc, request.ID); state != "cancelled" {
		t.Fatalf("request = %s", state)
	}
	if state, _, cancel := runState(t, svc, run.ID); state != "running" || !cancel {
		t.Fatalf("running run = %s cancel %v", state, cancel)
	}
	waits := openWaitRows(t, svc, task)
	if len(waits) != 1 || waits[0].expected != task.Customer || waits[0].requesting != workflowActor {
		t.Fatalf("waits = %#v", waits)
	}
	for _, want := range []string{
		"@" + task.Customer, "paused", "review",
		"ttasks workflow resume " + task.Key + " --decision continue",
		"ttasks workflow resume " + task.Key + " --decision release",
		"ttasks cancel " + task.Key,
		"> ci kept failing",
		"A plain reply does not resume the task.",
	} {
		if !strings.Contains(waits[0].body, want) {
			t.Errorf("pause comment lacks %q:\n%s", want, waits[0].body)
		}
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_notification_outbox o JOIN task_events e ON e.sequence = o.event_sequence
		WHERE e.task_id = ? AND o.message_type = 'task.question'`, task.ID); n != 1 {
		t.Fatalf("question notifications = %d", n)
	}
	payload := eventPayload(t, svc, task, "workflow.paused")
	if payload["reason"] != PauseRejectedRequests || payload["status"] != "review" {
		t.Fatalf("paused payload = %v", payload)
	}
	view, err := svc.GetWorkflow(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if view.PausedReason != PauseRejectedRequests || view.WaitingOn != WaitingOnPause {
		t.Fatalf("view = %#v", view)
	}
}

func TestPauseInAScriptStatusStopsTheWatch(t *testing.T) {
	svc, actor, task := watchFixture(t)
	run := scheduleWatch(t, svc, actor, task)
	if _, err := svc.db.Exec(`UPDATE task_status_visits SET next_watch_at = '2026-07-31T12:00:00.000000000Z' WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		t.Fatal(err)
	}
	paused := pause(t, svc, task.Key, PauseScriptFailures, "")
	if paused.WaitingOn != WaitingOnPause || paused.Assignee != "" || paused.WorkflowStatus != "merge" {
		t.Fatalf("paused = %#v", paused)
	}
	if _, _, _, next := openVisit(t, svc, task); next != "" {
		t.Fatalf("next_watch_at = %q", next)
	}
	if state, _, _ := runState(t, svc, run.ID); state != "cancelled" {
		t.Fatalf("pending watch run = %s", state)
	}
	if waits := openWaitRows(t, svc, task); len(waits) != 1 || strings.Contains(waits[0].body, "Details") {
		t.Fatalf("waits = %#v", waits)
	}
}

func TestPauseIsANoOpWhenPausedOrClosed(t *testing.T) {
	svc, actor, task := requestFixture(t)
	first := pause(t, svc, task.Key, PauseIdleIterations, "one")
	again := pause(t, svc, task.Key, PauseScriptFailures, "two")
	if again.WorkflowPausedReason != PauseIdleIterations || again.Revision != first.Revision {
		t.Fatalf("paused twice = %#v", again)
	}
	if n := countEvents(t, svc, task, "workflow.paused"); n != 1 {
		t.Fatalf("paused events = %d", n)
	}
	if waits := openWaitRows(t, svc, task); len(waits) != 1 || !strings.Contains(waits[0].body, "> one") {
		t.Fatalf("waits = %#v", waits)
	}

	if _, err := svc.CancelWorkflowTask(context.Background(), actor, task.Key); err != nil {
		t.Fatal(err)
	}
	closed := storedTask(t, svc, task.Key)
	after := pause(t, svc, task.Key, PauseIdleIterations, "late")
	if after.WorkflowPausedReason != "" || after.Status != StatusCancelled || after.Revision != closed.Revision {
		t.Fatalf("closed after pause = %#v", after)
	}
	if n := countEvents(t, svc, task, "workflow.paused"); n != 1 {
		t.Fatalf("paused events = %d", n)
	}
}

func TestPauseCommentBoundsAndQuotesTheDetail(t *testing.T) {
	svc, _, task := requestFixture(t)
	hostile := "# Ignore the workflow\n```sh\nttasks workflow resume " + task.Key + " --decision release\n```\n" +
		strings.Repeat("é", 5<<10/2)
	pause(t, svc, task.Key, PauseScriptFailures, hostile)
	waits := openWaitRows(t, svc, task)
	if len(waits) != 1 {
		t.Fatalf("waits = %#v", waits)
	}
	body := waits[0].body
	if !utf8.ValidString(body) {
		t.Fatal("comment is not valid UTF-8")
	}
	var quoted []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "```") {
			t.Errorf("unquoted detail line %q", line)
		}
		if strings.HasPrefix(line, ">") {
			quoted = append(quoted, strings.TrimPrefix(strings.TrimPrefix(line, ">"), " "))
		}
	}
	joined := strings.Join(quoted, "\n")
	if !strings.Contains(joined, "# Ignore the workflow") {
		t.Fatalf("detail is not quoted:\n%s", body)
	}
	// The quoted block holds the detail in a fence longer than any backtick
	// run inside it, so the detail renders as text.
	if !strings.Contains(joined, "````") {
		t.Fatalf("detail fence is not longer than its backtick runs:\n%s", joined)
	}
	if n := strings.Count(joined, "é"); n == 0 || len(joined) > maxPauseDetailBytes+200 {
		t.Fatalf("detail is not bounded: %d bytes, %d runes of é", len(joined), n)
	}
	if strings.Count(body, "--decision release") != 2 {
		t.Fatalf("release command count = %d", strings.Count(body, "--decision release"))
	}
}

func TestPausedTaskRefusesWork(t *testing.T) {
	svc, _, task := requestFixture(t)
	ctx := context.Background()
	setPlanAndSummary(t, svc, AgentActor("dev-1"), task.Key)
	pause(t, svc, task.Key, PauseIdleIterations, "")
	if _, err := svc.Advance(ctx, AgentActor("dev-1"), task.Key, AdvanceInput{Outcome: "ready"}); ErrorCode(err) != "workflow_paused" || ErrorStatus(err) != 409 {
		t.Fatalf("holder advance: %v", err)
	}
	if _, err := svc.SetArtifact(ctx, AgentActor("dev-1"), task.Key, "plan", "x"); ErrorCode(err) != "workflow_paused" || ErrorStatus(err) != 409 {
		t.Fatalf("holder artifact: %v", err)
	}
	if n, err := svc.DispatchPending(ctx); err != nil || n != 0 {
		t.Fatalf("dispatch = %d, %v", n, err)
	}

	// In a customer status, the customer decides the pause before an outcome.
	svc2, actor2, task2 := requestFixture(t)
	enter(t, svc2, task2.Key, "approval", "ask")
	pause(t, svc2, task2.Key, PauseHolderUnavailable, "")
	if _, err := svc2.Advance(ctx, actor2, task2.Key, AdvanceInput{Outcome: "approved"}); ErrorCode(err) != "workflow_paused" || ErrorStatus(err) != 409 {
		t.Fatalf("customer advance: %v", err)
	}
	if got := storedTask(t, svc2, task2.Key); got.WorkflowStatus != "approval" || got.WorkflowPausedReason == "" {
		t.Fatalf("after refused advance = %#v", got)
	}
}

func TestCustomerCommentLeavesThePauseOpen(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	paused := pause(t, svc, task.Key, PauseIdleIterations, "")
	waits := openWaitRows(t, svc, task)
	if _, err := svc.AddComment(ctx, actor, task.Key, AddCommentInput{Body: "continue please"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddComment(ctx, actor, task.Key, AddCommentInput{Body: "@agent:dev-1 what is blocking you?"}); err != nil {
		t.Fatal(err)
	}
	got := storedTask(t, svc, task.Key)
	if got.WorkflowPausedReason != PauseIdleIterations || got.Status != StatusWaitCustomer || got.WaitingOn != WaitingOnPause ||
		got.Assignee != paused.Assignee {
		t.Fatalf("after comments = %#v", got)
	}
	after := openWaitRows(t, svc, task)
	found := false
	for _, w := range after {
		if w.expected == task.Customer && w.requesting == workflowActor && w.commentID == waits[0].commentID {
			found = true
		}
	}
	if !found {
		t.Fatalf("pause wait gone: before %#v after %#v", waits, after)
	}
}

func TestCheckThatPassesAfterThePauseChangesNothing(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	ctx := context.Background()
	request, run := advanceApprove(t, svc, actor, task)
	if ok, err := svc.ClaimScriptRun(ctx, run.ID, "2026-07-31T12:00:01Z", "/runs/1/run.log"); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	paused := pause(t, svc, task.Key, PauseRejectedRequests, "")
	complete(t, svc, run.ID, RunCompletion{Verdict: "pass", ExitCode: exitCode(0)})
	got := storedTask(t, svc, task.Key)
	if got.WorkflowStatus != "review" || got.WorkflowPausedReason != PauseRejectedRequests || got.Revision != paused.Revision {
		t.Fatalf("after pass = %#v", got)
	}
	if state := requestState(t, svc, request.ID); state != "cancelled" {
		t.Fatalf("request = %s", state)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_script_runs WHERE task_id = ? AND state IN ('pending', 'running')`, task.ID); n != 0 {
		t.Fatalf("active runs = %d", n)
	}
}

func TestResumeWorkflowRefusals(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	if _, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeContinue); ErrorCode(err) != "workflow_not_paused" || ErrorStatus(err) != 409 {
		t.Fatalf("not paused: %v", err)
	}
	pause(t, svc, task.Key, PauseIdleIterations, "")
	if _, err := svc.ResumeWorkflow(ctx, AgentActor("dev-1"), task.Key, ResumeContinue); ErrorCode(err) != "forbidden" || ErrorStatus(err) != 403 {
		t.Fatalf("agent: %v", err)
	}
	if _, err := svc.ResumeWorkflow(ctx, actor, task.Key, "restart"); ErrorCode(err) != "invalid_decision" || ErrorStatus(err) != 400 {
		t.Fatalf("unknown decision: %v", err)
	}
	if _, err := svc.ResumeWorkflow(ctx, actor, "DEV-999", ResumeContinue); ErrorCode(err) != "not_found" {
		t.Fatalf("missing task: %v", err)
	}
	flexible, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "TASK", Title: "flex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResumeWorkflow(ctx, actor, flexible.Key, ResumeContinue); ErrorCode(err) != "workflow_not_bound" {
		t.Fatalf("flexible: %v", err)
	}
	for _, status := range []string{"approval", "merge"} {
		svc, actor, task := requestFixture(t)
		enter(t, svc, task.Key, status, "")
		pause(t, svc, task.Key, PauseScriptFailures, "")
		if _, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeRelease); ErrorCode(err) != "invalid_decision" || ErrorStatus(err) != 400 {
			t.Fatalf("release in %s: %v", status, err)
		}
		if got := storedTask(t, svc, task.Key); got.WorkflowPausedReason == "" {
			t.Fatalf("refused release unpaused %s", status)
		}
	}
	if got := storedTask(t, svc, task.Key); got.WorkflowPausedReason != PauseIdleIterations {
		t.Fatalf("refusals changed the task: %#v", got)
	}
}

func TestResumeContinueInAPoolStatus(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	setCounters(t, svc, task)
	paused := pause(t, svc, task.Key, PauseIdleIterations, "")
	signals := 0
	svc.SetGoalSignal(func() { signals++ })
	resumed, err := svc.ResumeWorkflow(ctx, actor, task.Key, " continue ")
	if err != nil {
		t.Fatal(err)
	}
	if signals == 0 {
		t.Fatal("no goal signal after resume")
	}
	if resumed.WorkflowPausedReason != "" || resumed.Status != StatusInProgress || resumed.WaitingOn != "" ||
		resumed.Assignee != "agent:dev-1" || resumed.WorkflowStatus != "develop" || resumed.Revision != paused.Revision+1 ||
		resumed.Access != "write" {
		t.Fatalf("resumed = %#v", resumed)
	}
	if c := visitCounters(t, svc, task); c != [3]int{} {
		t.Fatalf("counters = %v", c)
	}
	if waits := openWaitRows(t, svc, task); len(waits) != 0 {
		t.Fatalf("waits = %#v", waits)
	}
	if agent, released := holderRow(t, svc, task, "developers"); agent != "dev-1" || released {
		t.Fatalf("holder = %s released %v", agent, released)
	}
	payload := eventPayload(t, svc, task, "workflow.resumed")
	if payload["decision"] != ResumeContinue || payload["actor"] != "user:customer" || payload["reason"] != PauseIdleIterations {
		t.Fatalf("resumed payload = %v", payload)
	}
	if _, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeContinue); ErrorCode(err) != "workflow_not_paused" {
		t.Fatalf("second resume: %v", err)
	}
}

func TestResumeContinueInAScriptStatusRestartsTheWatch(t *testing.T) {
	svc, actor, task := watchFixture(t)
	ctx := context.Background()
	setCounters(t, svc, task)
	pause(t, svc, task.Key, PauseScriptFailures, "")
	if n, err := svc.ScheduleDueWatches(ctx, svc.clock()); err != nil || n != 0 {
		t.Fatalf("paused schedule = %d, %v", n, err)
	}
	resumed, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeContinue)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != StatusWaitCustomer || resumed.WaitingOn != WaitingOnScript || resumed.Assignee != "" ||
		resumed.WorkflowPausedReason != "" {
		t.Fatalf("resumed = %#v", resumed)
	}
	if _, _, _, next := openVisit(t, svc, task); next != svc.clock().UTC().Format(dispatchedAtLayout) {
		t.Fatalf("next_watch_at = %q", next)
	}
	if c := visitCounters(t, svc, task); c != [3]int{} {
		t.Fatalf("counters = %v", c)
	}
	if waits := openWaitRows(t, svc, task); len(waits) != 0 {
		t.Fatalf("waits = %#v", waits)
	}
	run := scheduleWatch(t, svc, actor, task)
	if run.Kind != "watch" || run.State != "pending" {
		t.Fatalf("run = %#v", run)
	}
}

func TestResumeContinueInACustomerStatusAsksAgain(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	enter(t, svc, task.Key, "approval", "ask")
	pause(t, svc, task.Key, PauseHolderUnavailable, "")
	pauseWait := openWaitRows(t, svc, task)
	if len(pauseWait) != 1 {
		t.Fatalf("waits = %#v", pauseWait)
	}
	resumed, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeContinue)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != StatusWaitCustomer || resumed.WaitingOn != WaitingOnCustomer || resumed.WorkflowPausedReason != "" {
		t.Fatalf("resumed = %#v", resumed)
	}
	waits := openWaitRows(t, svc, task)
	if len(waits) != 1 || waits[0].requesting != workflowActor || waits[0].commentID == pauseWait[0].commentID ||
		!strings.Contains(waits[0].body, `workflow status "approval"`) {
		t.Fatalf("waits = %#v (pause wait %#v)", waits, pauseWait)
	}
	if _, err := svc.Advance(ctx, actor, task.Key, AdvanceInput{Outcome: "rejected"}); err != nil {
		t.Fatalf("customer advance after resume: %v", err)
	}
}

func TestResumeReleaseHandsTheTaskToAnotherMember(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	setCounters(t, svc, task)
	pause(t, svc, task.Key, PauseIdleIterations, "")
	const assignments = `SELECT COUNT(*) FROM task_notification_outbox o JOIN task_events e ON e.sequence = o.event_sequence
		WHERE e.task_id = ? AND o.message_type = 'task.assigned'`
	assigned := countRows(t, svc, assignments, task.ID)
	resumed, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeRelease)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != StatusInProgress || resumed.Assignee != "agent:dev-2" || resumed.WorkflowPausedReason != "" {
		t.Fatalf("resumed = %#v", resumed)
	}
	if agent, released := holderRow(t, svc, task, "developers"); agent != "dev-2" || released {
		t.Fatalf("holder = %s released %v", agent, released)
	}
	if c := visitCounters(t, svc, task); c != [3]int{} {
		t.Fatalf("counters = %v", c)
	}
	if waits := openWaitRows(t, svc, task); len(waits) != 0 {
		t.Fatalf("waits = %#v", waits)
	}
	if payload := eventPayload(t, svc, task, "workflow.resumed"); payload["decision"] != ResumeRelease {
		t.Fatalf("resumed payload = %v", payload)
	}
	if n := countRows(t, svc, assignments, task.ID); n != assigned+1 {
		t.Fatalf("assignment notifications = %d, before %d", n, assigned)
	}
}

// TestResumeReleaseWithoutAnotherMember: reviewer-1 is the only reviewer and
// owns no queue. Released, it does not get the task back from dispatch, keeps
// its read access, and is eligible again after an operator move.
func TestResumeReleaseWithoutAnotherMember(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	ctx := context.Background()
	pause(t, svc, task.Key, PauseHolderUnavailable, "")
	resumed, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeRelease)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != StatusOpen || resumed.Assignee != "" || resumed.WorkflowPausedReason != "" || resumed.WaitingOn != "" {
		t.Fatalf("resumed = %#v", resumed)
	}
	if agent, released := holderRow(t, svc, task, "reviewers"); agent != "reviewer-1" || !released {
		t.Fatalf("holder = %s released %v", agent, released)
	}
	if n, err := svc.DispatchPending(ctx); err != nil || n != 0 {
		t.Fatalf("dispatch = %d, %v", n, err)
	}
	if got := storedTask(t, svc, task.Key); got.Status != StatusOpen || got.Assignee != "" {
		t.Fatalf("after dispatch = %#v", got)
	}
	if view, err := svc.GetWorkflow(ctx, AgentActor("reviewer-1"), task.Key); err != nil || view.Holder != "" {
		t.Fatalf("released holder read = %#v, %v", view, err)
	}
	moved, err := svc.MoveWorkflow(ctx, actor, task.Key, "review", "give it back")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Status != StatusInProgress || moved.Assignee != "agent:reviewer-1" {
		t.Fatalf("moved = %#v", moved)
	}
	if agent, released := holderRow(t, svc, task, "reviewers"); agent != "reviewer-1" || released {
		t.Fatalf("holder after move = %s released %v", agent, released)
	}
}

func TestMoveAndCancelClearThePause(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	pause(t, svc, task.Key, PauseIdleIterations, "")
	moved, err := svc.MoveWorkflow(ctx, actor, task.Key, "review", "hand over")
	if err != nil {
		t.Fatal(err)
	}
	if moved.WorkflowPausedReason != "" || moved.Assignee != "agent:reviewer-1" || moved.Status != StatusInProgress {
		t.Fatalf("moved = %#v", moved)
	}
	if waits := openWaitRows(t, svc, task); len(waits) != 0 {
		t.Fatalf("waits after move = %#v", waits)
	}
	payload := eventPayload(t, svc, task, "workflow.resumed")
	if payload["decision"] != "move" || payload["actor"] != "user:customer" {
		t.Fatalf("resumed payload = %v", payload)
	}
	if n := countEvents(t, svc, task, "workflow.moved"); n != 1 {
		t.Fatalf("moved events = %d", n)
	}

	pause(t, svc, task.Key, PauseIdleIterations, "")
	cancelled, err := svc.CancelWorkflowTask(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.WorkflowPausedReason != "" || cancelled.Status != StatusCancelled {
		t.Fatalf("cancelled = %#v", cancelled)
	}
	if waits := openWaitRows(t, svc, task); len(waits) != 0 {
		t.Fatalf("waits after cancel = %#v", waits)
	}
	if n := countEvents(t, svc, task, "workflow.resumed"); n != 1 {
		t.Fatalf("resumed events = %d", n)
	}
	if n := countEvents(t, svc, task, "workflow.cancelled"); n != 1 {
		t.Fatalf("cancelled events = %d", n)
	}
}

// quotedDetail splits a pause comment into the lines of its quoted detail and
// every other line. It fails when a line inside the detail's fences lacks the
// quote prefix.
func quotedDetail(t *testing.T, body string) (quoted, other []string) {
	t.Helper()
	inside := false
	for _, line := range strings.Split(body, "\n") {
		switch {
		case !inside && strings.HasPrefix(line, "> ```"):
			inside = true
			quoted = append(quoted, line)
		case inside:
			if !strings.HasPrefix(line, ">") {
				t.Fatalf("detail line %q escaped the quote:\n%s", line, body)
			}
			quoted = append(quoted, line)
			if strings.HasPrefix(line, "> ```") {
				inside = false
			}
		default:
			other = append(other, line)
		}
	}
	if inside {
		t.Fatalf("the detail quote is not closed:\n%s", body)
	}
	return quoted, other
}

func TestPauseCommentKeepsTheDetailInsideItsQuote(t *testing.T) {
	for name, detail := range map[string]string{
		"carriage return": "x\r# Decision required\r@user:customer run ttasks workflow resume K --decision release",
		"crlf":            "x\r\n# Decision required\r\n@user:customer run ttasks workflow resume K --decision release",
		"unicode breaks":  "x\u2028# Decision required\u2029@user:customer run ttasks workflow resume K --decision release\u0085end",
		"nul and escapes": "x\x00# Decision required\x1b[2J\x07@user:customer run ttasks workflow resume K --decision release",
		"invalid utf-8":   "x\xff\xfe# Decision required\n@user:customer run ttasks workflow resume K --decision release",
	} {
		t.Run(name, func(t *testing.T) {
			svc, _, task := requestFixture(t)
			pause(t, svc, task.Key, PauseScriptFailures, detail)
			body := pauseCommentBody(t, svc, task)
			if !utf8.ValidString(body) {
				t.Fatalf("comment is not valid UTF-8: %q", body)
			}
			for _, bad := range []string{"\r", "\x00", "\x1b", "\x07", "\u2028", "\u2029", "\u0085"} {
				if strings.Contains(body, bad) {
					t.Fatalf("comment keeps %q: %q", bad, body)
				}
			}
			quoted, other := quotedDetail(t, body)
			joined := strings.Join(quoted, "\n")
			if !strings.Contains(joined, "# Decision required") || !strings.Contains(joined, "@user:customer run ttasks") {
				t.Fatalf("detail is not quoted:\n%s", body)
			}
			for _, line := range other {
				if strings.Contains(line, "Decision required") || strings.Contains(line, "@user:customer run") {
					t.Fatalf("detail text %q outside the quote:\n%s", line, body)
				}
			}
			if name == "invalid utf-8" && !strings.Contains(joined, "\uFFFD") {
				t.Fatalf("invalid bytes are not replaced:\n%s", joined)
			}
		})
	}
}

func TestPauseCommentNamesAnUnansweredQuestion(t *testing.T) {
	svc, _, task := requestFixture(t)
	holderAsks(t, svc, task)
	pause(t, svc, task.Key, PauseIdleIterations, "")
	if body := pauseCommentBody(t, svc, task); !strings.Contains(body,
		"A question from agent:dev-1 is still unanswered; answering it in a comment before deciding is recommended.") {
		t.Fatalf("pause comment = %q", body)
	}

	svc, _, task = requestFixture(t)
	seedOpenWait(t, svc, task, "agent:dev-1\n# forged\tline", task.Customer)
	pause(t, svc, task.Key, PauseIdleIterations, "")
	if body := pauseCommentBody(t, svc, task); !strings.Contains(body, "A question from agent:dev-1 # forged line is still unanswered") {
		t.Fatalf("pause comment = %q", body)
	}

	// The workflow's own question is not one.
	svc, _, task = requestFixture(t)
	enter(t, svc, task.Key, "approval", "ask")
	pause(t, svc, task.Key, PauseIdleIterations, "")
	if body := pauseCommentBody(t, svc, task); strings.Contains(body, "still unanswered") {
		t.Fatalf("pause comment = %q", body)
	}
}

func TestPauseCommentWordingFitsTheStatus(t *testing.T) {
	svc, _, task := requestFixture(t)
	pause(t, svc, task.Key, PauseIdleIterations, "")
	if body := pauseCommentBody(t, svc, task); !strings.Contains(body, "Continue with the same holder") {
		t.Fatalf("pool status comment = %q", body)
	}
	for _, status := range []string{"approval", "merge"} {
		svc, _, task := requestFixture(t)
		enter(t, svc, task.Key, status, "")
		pause(t, svc, task.Key, PauseScriptFailures, "")
		body := pauseCommentBody(t, svc, task)
		if strings.Contains(body, "same holder") ||
			!strings.Contains(body, "Resume the task in its current status; counters are reset") {
			t.Fatalf("%s status comment = %q", status, body)
		}
	}
}

func TestMoveToTheSameStatusClearsThePause(t *testing.T) {
	svc, actor, task := requestFixture(t)
	pause(t, svc, task.Key, PauseIdleIterations, "")
	moved, err := svc.MoveWorkflow(context.Background(), actor, task.Key, "develop", "try again")
	if err != nil {
		t.Fatal(err)
	}
	if moved.WorkflowPausedReason != "" || moved.WorkflowStatus != "develop" || moved.Status != StatusInProgress ||
		moved.Assignee != "agent:dev-1" {
		t.Fatalf("moved = %#v", moved)
	}
	if waits := openWaitRows(t, svc, task); len(waits) != 0 {
		t.Fatalf("waits = %#v", waits)
	}
}

func TestAReleasedAgentStaysEligibleForAnotherTask(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	pause(t, svc, task.Key, PauseIdleIterations, "")
	if resumed, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeRelease); err != nil || resumed.Assignee != "agent:dev-2" {
		t.Fatalf("resumed = %#v, %v", resumed, err)
	}
	other := mustCreateDev(t, svc, actor, "other")
	if other.Assignee != "agent:dev-1" {
		t.Fatalf("other task = %#v; the release concerns one task only", other)
	}
}

func TestAReleasedAgentLosesReadAccessWhenAMoveDropsItsRow(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	ctx := context.Background()
	pause(t, svc, task.Key, PauseHolderUnavailable, "")
	if _, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeRelease); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetWorkflow(ctx, AgentActor("reviewer-1"), task.Key); err != nil {
		t.Fatalf("released holder read before the move: %v", err)
	}
	if _, err := svc.MoveWorkflow(ctx, actor, task.Key, "develop", "back to work"); err != nil {
		t.Fatal(err)
	}
	if _, ok := holderRow(t, svc, task, "reviewers"); ok {
		t.Fatal("the released row survived the move")
	}
	if _, err := svc.GetWorkflow(ctx, AgentActor("reviewer-1"), task.Key); err == nil {
		t.Fatal("the released holder still reads the task after the move dropped its row")
	}
}

// TestASecondReleaseMakesTheFirstReleasedAgentEligibleAgain pins the intended
// behaviour: a pool keeps one holder row per task, so the row of the agent
// that took over replaces the first agent's released row, and a second release
// may hand the task back to the first agent.
func TestASecondReleaseMakesTheFirstReleasedAgentEligibleAgain(t *testing.T) {
	svc, actor, task := requestFixture(t)
	ctx := context.Background()
	pause(t, svc, task.Key, PauseIdleIterations, "")
	if resumed, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeRelease); err != nil || resumed.Assignee != "agent:dev-2" {
		t.Fatalf("first release = %#v, %v", resumed, err)
	}
	pause(t, svc, task.Key, PauseIdleIterations, "")
	resumed, err := svc.ResumeWorkflow(ctx, actor, task.Key, ResumeRelease)
	if err != nil || resumed.Assignee != "agent:dev-1" {
		t.Fatalf("second release = %#v, %v", resumed, err)
	}
	if agent, released := holderRow(t, svc, task, "developers"); agent != "dev-1" || released {
		t.Fatalf("holder = %s released %v", agent, released)
	}
}
