package tasks

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

// runDefinition is requestDefinition with two checks on review's "approve"
// (the second runs as the agent with a 90s timeout), a watch on the script
// status "merge", the artifact "merge_commit", and workflow env.
func runDefinition() workflowfile.File {
	def := requestDefinition()
	def.Env = map[string]string{"REPO": "org/repo"}
	def.Artifacts = append(def.Artifacts, workflowfile.Artifact{Name: "merge_commit"})
	def.Statuses[1].Transitions[0].Checks = []workflowfile.Check{
		{Script: "checks/ci.sh"},
		{Script: "checks/branch.sh", RunAs: workflowfile.RunAsAgent, Timeout: "90s"},
	}
	def.Statuses[3].Watch = &workflowfile.Watch{Script: "watch/merge.sh", Every: "5m", Timeout: "2m"}
	return def
}

// runFixture binds DEV to runDefinition and returns a task held by
// reviewer-1 in "review", with a counter of goal signals.
func runFixture(t *testing.T) (*Service, Actor, Task, *atomic.Int32) {
	t.Helper()
	svc, actor := workflowFixture(t)
	ctx := context.Background()
	if err := svc.EnsureDefaultQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE agents SET enabled = 1, loop_enabled = 1, goal_enabled = 1`); err != nil {
		t.Fatal(err)
	}
	mustRebindPool(t, svc, actor, "developers", []string{"dev-1", "dev-2"}, 0)
	mustRebindPool(t, svc, actor, "reviewers", []string{"reviewer-1"}, 0)
	seedEngineImage(t, svc, runDefinition())
	if _, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "development:0.1.0", 0); err != nil {
		t.Fatal(err)
	}
	created := mustCreateDev(t, svc, actor, "runs")
	task := enter(t, svc, created.Key, "review", "ready")
	if task.Assignee != "agent:reviewer-1" {
		t.Fatalf("fixture task = %#v", task)
	}
	var goals atomic.Int32
	svc.SetGoalSignal(func() { goals.Add(1) })
	return svc, actor, task, &goals
}

func listRuns(t *testing.T, svc *Service, actor Actor, task Task) []ScriptRun {
	t.Helper()
	runs, err := svc.ListScriptRuns(context.Background(), actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// openVisit returns the open visit's status, rejected_requests,
// script_failures, and next_watch_at.
func openVisit(t *testing.T, svc *Service, task Task) (string, int, int, string) {
	t.Helper()
	var status, next string
	var rejected, failures int
	if err := svc.db.QueryRow(`
		SELECT status_id, rejected_requests, script_failures, next_watch_at
		FROM task_status_visits WHERE task_id = ? AND left_at = ''`, task.ID).Scan(&status, &rejected, &failures, &next); err != nil {
		t.Fatal(err)
	}
	return status, rejected, failures, next
}

func runState(t *testing.T, svc *Service, id int64) (string, string, bool) {
	t.Helper()
	var state, verdict string
	var cancel bool
	if err := svc.db.QueryRow(`SELECT state, verdict, cancel_requested FROM task_script_runs WHERE id = ?`, id).
		Scan(&state, &verdict, &cancel); err != nil {
		t.Fatal(err)
	}
	return state, verdict, cancel
}

func requestRow(t *testing.T, svc *Service, id int64) (string, string) {
	t.Helper()
	var state, result string
	if err := svc.db.QueryRow(`SELECT state, result_message FROM task_transition_requests WHERE id = ?`, id).
		Scan(&state, &result); err != nil {
		t.Fatal(err)
	}
	return state, result
}

func storedTask(t *testing.T, svc *Service, key string) Task {
	t.Helper()
	task, err := taskByKey(svc.db, key)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func complete(t *testing.T, svc *Service, id int64, done RunCompletion) {
	t.Helper()
	if err := svc.CompleteScriptRun(context.Background(), id, done); err != nil {
		t.Fatal(err)
	}
}

func exitCode(n int) *int { return &n }

// advanceApprove requests "approve" as reviewer-1 and returns the request and
// the first check's run.
func advanceApprove(t *testing.T, svc *Service, actor Actor, task Task) (TransitionRequest, ScriptRun) {
	t.Helper()
	request, err := svc.Advance(context.Background(), AgentActor("reviewer-1"), task.Key,
		AdvanceInput{Outcome: "approve", Message: "looks good"})
	if err != nil {
		t.Fatal(err)
	}
	runs := listRuns(t, svc, actor, task)
	if len(runs) == 0 {
		t.Fatal("no run")
	}
	return request, runs[0]
}

func TestAdvanceWithChecksRecordsAPendingRequestAndTheFirstRun(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	ctx := context.Background()
	request, run := advanceApprove(t, svc, actor, task)
	if request.ID == 0 || request.State != "pending" || request.FinishedAt != "" || request.Outcome != "approve" ||
		request.Message != "looks good" || request.Actor != "agent:reviewer-1" {
		t.Fatalf("request = %#v", request)
	}
	if state := requestState(t, svc, request.ID); state != "pending" {
		t.Fatalf("stored request state = %q", state)
	}
	runs := listRuns(t, svc, actor, task)
	if len(runs) != 1 {
		t.Fatalf("runs = %#v", runs)
	}
	if run.TaskKey != task.Key || run.Kind != "check" || run.Script != "checks/ci.sh" || run.RunAs != "queue" ||
		run.State != "pending" || run.CreatedAt == "" || run.Verdict != "" || run.ExitCode != nil {
		t.Fatalf("run = %#v", run)
	}
	var index int
	var requestID int64
	if err := svc.db.QueryRow(`SELECT check_index, request_id FROM task_script_runs WHERE id = ?`, run.ID).
		Scan(&index, &requestID); err != nil {
		t.Fatal(err)
	}
	if index != 0 || requestID != request.ID {
		t.Fatalf("check_index %d request_id %d", index, requestID)
	}
	if got := storedTask(t, svc, task.Key); got.WorkflowStatus != "review" {
		t.Fatalf("status = %q", got.WorkflowStatus)
	}
	requested := eventPayload(t, svc, task, "workflow.transition_requested")
	if requested["state"] != "pending" || requested["request_id"] != float64(request.ID) {
		t.Fatalf("requested = %v", requested)
	}
	if _, err := svc.Advance(ctx, AgentActor("reviewer-1"), task.Key, AdvanceInput{Outcome: "approve"}); ErrorCode(err) != "transition_pending" {
		t.Fatalf("second advance: %v", err)
	}
}

func TestCompleteScriptRunPassRunsTheNextCheckThenApplies(t *testing.T) {
	svc, actor, task, goals := runFixture(t)
	ctx := context.Background()
	request, first := advanceApprove(t, svc, actor, task)
	if _, err := svc.db.Exec(`UPDATE task_status_visits SET script_failures = 2 WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := svc.ClaimScriptRun(ctx, first.ID, "2026-07-31T12:00:01Z", "/runs/1/run.log"); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	complete(t, svc, first.ID, RunCompletion{Verdict: "pass", Message: "ci green", ExitCode: exitCode(0),
		Artifacts: map[string]string{"summary": "ci ok"}, FinishedAt: "2026-07-31T12:00:02Z"})

	state, verdict, _ := runState(t, svc, first.ID)
	if state != "finished" || verdict != "pass" {
		t.Fatalf("first run = %s %s", state, verdict)
	}
	if got := requestState(t, svc, request.ID); got != "pending" {
		t.Fatalf("request after first check = %q", got)
	}
	if _, _, failures, _ := openVisit(t, svc, task); failures != 0 {
		t.Fatalf("script_failures after a pass = %d", failures)
	}
	runs := listRuns(t, svc, actor, task)
	if len(runs) != 2 || runs[0].Script != "checks/branch.sh" || runs[0].RunAs != "agent" || runs[0].State != "pending" {
		t.Fatalf("runs = %#v", runs)
	}
	if runs[1].Message != "ci green" || runs[1].ExitCode == nil || *runs[1].ExitCode != 0 ||
		runs[1].LogPath != "/runs/1/run.log" || runs[1].StartedAt == "" || runs[1].FinishedAt != "2026-07-31T12:00:02Z" {
		t.Fatalf("finished run = %#v", runs[1])
	}
	scriptRun := eventPayload(t, svc, task, "workflow.script_run")
	if scriptRun["run_id"] != float64(first.ID) || scriptRun["kind"] != "check" || scriptRun["script"] != "checks/ci.sh" ||
		scriptRun["verdict"] != "pass" || scriptRun["exit_code"] != float64(0) {
		t.Fatalf("script_run = %v", scriptRun)
	}
	if _, ok := scriptRun["artifacts"]; ok {
		t.Fatalf("script_run carries artifacts: %v", scriptRun)
	}

	complete(t, svc, runs[0].ID, RunCompletion{Verdict: "pass", ExitCode: exitCode(0),
		Artifacts: map[string]string{"plan": "branch ok"}})
	if got := requestState(t, svc, request.ID); got != "applied" {
		t.Fatalf("request after last check = %q", got)
	}
	stored := storedTask(t, svc, task.Key)
	if stored.WorkflowStatus != "approval" || stored.Status != StatusWaitCustomer {
		t.Fatalf("stored = %#v", stored)
	}
	var outcome, message, enteredBy string
	if err := svc.db.QueryRow(`SELECT outcome, message FROM task_status_visits WHERE task_id = ? AND status_id = 'review' AND left_at <> ''`,
		task.ID).Scan(&outcome, &message); err != nil {
		t.Fatal(err)
	}
	if err := svc.db.QueryRow(`SELECT entered_by FROM task_status_visits WHERE task_id = ? AND left_at = ''`, task.ID).Scan(&enteredBy); err != nil {
		t.Fatal(err)
	}
	if outcome != "approve" || message != "looks good" || enteredBy != "agent:reviewer-1" {
		t.Fatalf("visit = %q %q %q", outcome, message, enteredBy)
	}
	artifacts, err := svc.ListArtifacts(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	authors := map[string]string{}
	for _, artifact := range artifacts {
		authors[artifact.Name] = artifact.Author + "=" + artifact.Value
	}
	if authors["summary"] != "script:checks/ci.sh=ci ok" || authors["plan"] != "script:checks/branch.sh=branch ok" {
		t.Fatalf("artifacts = %v", authors)
	}
	if goals.Load() == 0 {
		t.Fatal("no goal signal after the transition applied")
	}
}

func TestCompleteScriptRunPassAfterARequiredArtifactVanishedFailsTheRequest(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	if _, err := svc.db.Exec(`INSERT INTO task_artifacts(task_id, name, value, author, created_at) VALUES (?, 'plan', 'p', 'user:customer', 'now')`, task.ID); err != nil {
		t.Fatal(err)
	}
	def := runDefinition()
	def.Statuses[1].Transitions[0].Requires = []string{"plan"}
	rewriteDefinition(t, svc, task, def)
	request, first := advanceApprove(t, svc, actor, task)
	complete(t, svc, first.ID, RunCompletion{Verdict: "pass"})
	if _, err := svc.db.Exec(`DELETE FROM task_artifacts WHERE task_id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	second := listRuns(t, svc, actor, task)[0]
	complete(t, svc, second.ID, RunCompletion{Verdict: "pass"})
	state, result := requestRow(t, svc, request.ID)
	if state != "failed" || !strings.Contains(result, "plan") {
		t.Fatalf("request = %s %q", state, result)
	}
	if got := storedTask(t, svc, task.Key); got.WorkflowStatus != "review" {
		t.Fatalf("status = %q", got.WorkflowStatus)
	}
}

// rewriteDefinition replaces the definition in the task's pinned manifest.
func rewriteDefinition(t *testing.T, svc *Service, task Task, def workflowfile.File) {
	t.Helper()
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE task_workflow_images SET manifest = json_set(manifest, '$.definition', json(?)) WHERE digest = ?`,
		string(raw), task.WorkflowDigest); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteScriptRunRejectClosesTheRequest(t *testing.T) {
	svc, actor, task, goals := runFixture(t)
	request, first := advanceApprove(t, svc, actor, task)
	if _, err := svc.db.Exec(`UPDATE task_status_visits SET script_failures = 2 WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		t.Fatal(err)
	}
	before := goals.Load()
	complete(t, svc, first.ID, RunCompletion{Verdict: "reject", Message: "CI is red", ExitCode: exitCode(112)})
	state, result := requestRow(t, svc, request.ID)
	if state != "rejected" || result != "CI is red" {
		t.Fatalf("request = %s %q", state, result)
	}
	status, rejected, failures, _ := openVisit(t, svc, task)
	if status != "review" || rejected != 1 || failures != 0 {
		t.Fatalf("visit = %s rejected %d failures %d", status, rejected, failures)
	}
	if got := storedTask(t, svc, task.Key); got.WorkflowStatus != "review" || got.Assignee != "agent:reviewer-1" {
		t.Fatalf("task = %#v", got)
	}
	if goals.Load() == before {
		t.Fatal("the holder got no goal signal")
	}
	rejectedEvent := eventPayload(t, svc, task, "workflow.transition_rejected")
	if rejectedEvent["request_id"] != float64(request.ID) || rejectedEvent["message"] != "CI is red" {
		t.Fatalf("event = %v", rejectedEvent)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_script_runs WHERE task_id = ?`, task.ID); n != 1 {
		t.Fatalf("runs = %d; a rejection runs no further check", n)
	}
	// The agent may ask again.
	if _, err := svc.Advance(context.Background(), AgentActor("reviewer-1"), task.Key, AdvanceInput{Outcome: "approve"}); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteScriptRunFailureClosesTheRequestWithTheLog(t *testing.T) {
	for _, verdict := range []string{"failure", "outcome", "quiet"} {
		t.Run(verdict, func(t *testing.T) {
			svc, actor, task, goals := runFixture(t)
			request, first := advanceApprove(t, svc, actor, task)
			before := goals.Load()
			complete(t, svc, first.ID, RunCompletion{Verdict: verdict, Message: "exit status 3", ExitCode: exitCode(3),
				LogPath: "/base/tasks/DEV/runs/1/run.log"})
			state, result := requestRow(t, svc, request.ID)
			if state != "failed" || !strings.Contains(result, "exit status 3") || !strings.Contains(result, "/base/tasks/DEV/runs/1/run.log") {
				t.Fatalf("request = %s %q", state, result)
			}
			if _, rejected, failures, _ := openVisit(t, svc, task); rejected != 0 || failures != 1 {
				t.Fatalf("rejected %d failures %d", rejected, failures)
			}
			if goals.Load() == before {
				t.Fatal("no goal signal")
			}
			failed := eventPayload(t, svc, task, "workflow.transition_failed")
			if failed["request_id"] != float64(request.ID) || failed["log_path"] != "/base/tasks/DEV/runs/1/run.log" {
				t.Fatalf("event = %v", failed)
			}
		})
	}
}

func TestCompleteScriptRunRefusesAnUnknownVerdict(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	_, first := advanceApprove(t, svc, actor, task)
	if err := svc.CompleteScriptRun(context.Background(), first.ID, RunCompletion{Verdict: "maybe"}); err == nil {
		t.Fatal("an unknown verdict was accepted")
	}
	if state, _, _ := runState(t, svc, first.ID); state != "pending" {
		t.Fatalf("run = %s", state)
	}
}

func TestCompleteScriptRunTwiceIsANoOp(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	request, first := advanceApprove(t, svc, actor, task)
	complete(t, svc, first.ID, RunCompletion{Verdict: "reject", Message: "no"})
	events := countRows(t, svc, `SELECT COUNT(*) FROM task_events WHERE task_id = ?`, task.ID)
	complete(t, svc, first.ID, RunCompletion{Verdict: "pass"})
	if after := countRows(t, svc, `SELECT COUNT(*) FROM task_events WHERE task_id = ?`, task.ID); after != events {
		t.Fatalf("events %d -> %d", events, after)
	}
	if _, verdict, _ := runState(t, svc, first.ID); verdict != "reject" {
		t.Fatalf("verdict = %q", verdict)
	}
	if state, _ := requestRow(t, svc, request.ID); state != "rejected" {
		t.Fatalf("request = %s", state)
	}
	if _, rejected, _, _ := openVisit(t, svc, task); rejected != 1 {
		t.Fatalf("rejected = %d", rejected)
	}
}

func TestCompleteScriptRunBoundsTheMessage(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	request, first := advanceApprove(t, svc, actor, task)
	long := strings.Repeat("я", 3000) // 6000 bytes
	complete(t, svc, first.ID, RunCompletion{Verdict: "reject", Message: long})
	runs := listRuns(t, svc, actor, task)
	if len(runs[0].Message) > 4096 || !utf8.ValidString(runs[0].Message) || !strings.HasPrefix(long, runs[0].Message) {
		t.Fatalf("message = %d bytes, valid %v", len(runs[0].Message), utf8.ValidString(runs[0].Message))
	}
	if _, result := requestRow(t, svc, request.ID); len(result) > 4096 {
		t.Fatalf("result_message = %d bytes", len(result))
	}
}

func TestClaimScriptRun(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	ctx := context.Background()
	_, first := advanceApprove(t, svc, actor, task)
	if ok, err := svc.ClaimScriptRun(ctx, first.ID, "2026-07-31T12:00:01Z", "/log"); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if ok, err := svc.ClaimScriptRun(ctx, first.ID, "2026-07-31T12:00:01Z", "/log"); err != nil || ok {
		t.Fatalf("second claim = %v, %v", ok, err)
	}
	if err := svc.SetScriptRunPID(ctx, first.ID, 4242); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := svc.db.QueryRow(`SELECT pid FROM task_script_runs WHERE id = ?`, first.ID).Scan(&pid); err != nil || pid != 4242 {
		t.Fatalf("pid = %d, %v", pid, err)
	}
	got, err := svc.GetScriptRun(ctx, actor, task.Key, first.ID)
	if err != nil || got.State != "running" || got.StartedAt != "2026-07-31T12:00:01Z" || got.LogPath != "/log" {
		t.Fatalf("run = %#v, %v", got, err)
	}

	// A pending run whose cancellation was requested is never claimed.
	if _, err := svc.db.Exec(`UPDATE task_script_runs SET state = 'pending', cancel_requested = 1 WHERE id = ?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := svc.ClaimScriptRun(ctx, first.ID, "now", "/log"); err != nil || ok {
		t.Fatalf("claim of a cancelled run = %v, %v", ok, err)
	}
}

func TestLeavingTheStatusStopsItsRuns(t *testing.T) {
	t.Run("a move cancels a pending check and its request", func(t *testing.T) {
		svc, actor, task, _ := runFixture(t)
		request, first := advanceApprove(t, svc, actor, task)
		if _, err := svc.MoveWorkflow(context.Background(), actor, task.Key, "develop", "back"); err != nil {
			t.Fatal(err)
		}
		if state, _, _ := runState(t, svc, first.ID); state != "cancelled" {
			t.Fatalf("run = %s", state)
		}
		if state := requestState(t, svc, request.ID); state != "cancelled" {
			t.Fatalf("request = %s", state)
		}
		// The worker may still report it; nothing changes.
		complete(t, svc, first.ID, RunCompletion{Verdict: "pass"})
		if state, _, _ := runState(t, svc, first.ID); state != "cancelled" {
			t.Fatalf("run after completion = %s", state)
		}
	})
	t.Run("a move requests cancellation of a running check", func(t *testing.T) {
		svc, actor, task, _ := runFixture(t)
		ctx := context.Background()
		request, first := advanceApprove(t, svc, actor, task)
		if ok, err := svc.ClaimScriptRun(ctx, first.ID, "t", "/log"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if _, err := svc.MoveWorkflow(ctx, actor, task.Key, "develop", "back"); err != nil {
			t.Fatal(err)
		}
		state, _, cancel := runState(t, svc, first.ID)
		if state != "running" || !cancel {
			t.Fatalf("run = %s cancel %v", state, cancel)
		}
		toKill, err := svc.CancelRequestedRuns(ctx)
		if err != nil || len(toKill) != 1 || toKill[0].ID != first.ID {
			t.Fatalf("cancel requested = %#v, %v", toKill, err)
		}
		status := storedTask(t, svc, task.Key).WorkflowStatus
		complete(t, svc, first.ID, RunCompletion{Verdict: "pass", Artifacts: map[string]string{"plan": "x"}})
		if state, _, _ := runState(t, svc, first.ID); state != "cancelled" {
			t.Fatalf("run = %s", state)
		}
		if state := requestState(t, svc, request.ID); state != "cancelled" {
			t.Fatalf("request = %s", state)
		}
		if got := storedTask(t, svc, task.Key).WorkflowStatus; got != status {
			t.Fatalf("status %s -> %s", status, got)
		}
		if n := countRows(t, svc, `SELECT COUNT(*) FROM task_artifacts WHERE task_id = ?`, task.ID); n != 0 {
			t.Fatalf("artifacts = %d", n)
		}
	})
	t.Run("a run that finishes after its visit was left changes only its row", func(t *testing.T) {
		svc, actor, task, _ := runFixture(t)
		ctx := context.Background()
		request, first := advanceApprove(t, svc, actor, task)
		if ok, err := svc.ClaimScriptRun(ctx, first.ID, "t", "/log"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		// Leave the status behind the engine's back, without a cancellation.
		enter(t, svc, task.Key, "develop", "changes")
		if _, err := svc.db.Exec(`UPDATE task_script_runs SET cancel_requested = 0 WHERE id = ?`, first.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.db.Exec(`UPDATE task_transition_requests SET state = 'pending' WHERE id = ?`, request.ID); err != nil {
			t.Fatal(err)
		}
		complete(t, svc, first.ID, RunCompletion{Verdict: "failure", Message: "late"})
		if state, verdict, _ := runState(t, svc, first.ID); state != "finished" || verdict != "failure" {
			t.Fatalf("run = %s %s", state, verdict)
		}
		if state := requestState(t, svc, request.ID); state != "pending" {
			t.Fatalf("request = %s", state)
		}
		if status, _, failures, _ := openVisit(t, svc, task); status != "develop" || failures != 0 {
			t.Fatalf("visit = %s failures %d", status, failures)
		}
	})
}

// watchFixture moves the run fixture's task into the script status "merge".
func watchFixture(t *testing.T) (*Service, Actor, Task) {
	t.Helper()
	svc, actor, task, _ := runFixture(t)
	task = enter(t, svc, task.Key, "merge", "approved")
	return svc, actor, task
}

// scheduleWatch makes the open visit due and schedules its run.
func scheduleWatch(t *testing.T, svc *Service, actor Actor, task Task) ScriptRun {
	t.Helper()
	n, err := svc.ScheduleDueWatches(context.Background(), svc.clock())
	if err != nil || n != 1 {
		t.Fatalf("scheduled = %d, %v", n, err)
	}
	return listRuns(t, svc, actor, task)[0]
}

func TestEnteringAScriptStatusSchedulesItsWatch(t *testing.T) {
	svc, actor, task := watchFixture(t)
	ctx := context.Background()
	now := svc.clock().UTC()
	_, _, _, next := openVisit(t, svc, task)
	if next != now.Format(dispatchedAtLayout) {
		t.Fatalf("next_watch_at = %q", next)
	}
	if n, err := svc.ScheduleDueWatches(ctx, now.Add(-time.Second)); err != nil || n != 0 {
		t.Fatalf("early = %d, %v", n, err)
	}
	run := scheduleWatch(t, svc, actor, task)
	if run.Kind != "watch" || run.Script != "watch/merge.sh" || run.RunAs != "queue" || run.State != "pending" {
		t.Fatalf("run = %#v", run)
	}
	if _, _, _, next := openVisit(t, svc, task); next != "" {
		t.Fatalf("next_watch_at after scheduling = %q", next)
	}
	if n, err := svc.ScheduleDueWatches(ctx, now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("again = %d, %v", n, err)
	}
	// Even a due visit gets no second run while one is active.
	if _, err := svc.db.Exec(`UPDATE task_status_visits SET next_watch_at = ? WHERE task_id = ? AND left_at = ''`,
		now.Format(dispatchedAtLayout), task.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.ScheduleDueWatches(ctx, now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("while active = %d, %v", n, err)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_script_runs WHERE task_id = ?`, task.ID); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	// A pool status has no watch.
	if _, _, _, next := func() (string, int, int, string) {
		enter(t, svc, task.Key, "develop", "")
		return openVisit(t, svc, task)
	}(); next != "" {
		t.Fatalf("pool status next_watch_at = %q", next)
	}
}

func TestScheduleDueWatchesSkipsPausedAndClosedTasks(t *testing.T) {
	svc, _, task := watchFixture(t)
	ctx := context.Background()
	if _, err := svc.db.Exec(`UPDATE tasks SET workflow_paused_reason = 'hold' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.ScheduleDueWatches(ctx, svc.clock()); err != nil || n != 0 {
		t.Fatalf("paused = %d, %v", n, err)
	}
	if _, err := svc.db.Exec(`UPDATE tasks SET workflow_paused_reason = '', status = 'cancelled' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.ScheduleDueWatches(ctx, svc.clock()); err != nil || n != 0 {
		t.Fatalf("closed = %d, %v", n, err)
	}
}

func TestWatchQuietReschedulesAfterEvery(t *testing.T) {
	svc, actor, task := watchFixture(t)
	run := scheduleWatch(t, svc, actor, task)
	if _, err := svc.db.Exec(`UPDATE task_status_visits SET script_failures = 2 WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		t.Fatal(err)
	}
	finished := time.Date(2026, 7, 31, 12, 3, 0, 0, time.UTC)
	complete(t, svc, run.ID, RunCompletion{Verdict: "quiet", ExitCode: exitCode(111), FinishedAt: finished.Format(time.RFC3339Nano)})
	status, _, failures, next := openVisit(t, svc, task)
	if status != "merge" || failures != 0 || next != finished.Add(5*time.Minute).Format(dispatchedAtLayout) {
		t.Fatalf("visit = %s failures %d next %q", status, failures, next)
	}
	if n, err := svc.ScheduleDueWatches(context.Background(), finished.Add(5*time.Minute-time.Nanosecond)); err != nil || n != 0 {
		t.Fatalf("before every = %d, %v", n, err)
	}
	if n, err := svc.ScheduleDueWatches(context.Background(), finished.Add(5*time.Minute)); err != nil || n != 1 {
		t.Fatalf("after every = %d, %v", n, err)
	}
}

func TestWatchFailureCountsAndReschedules(t *testing.T) {
	for _, verdict := range []string{"failure", "pass", "reject"} {
		t.Run(verdict, func(t *testing.T) {
			svc, actor, task := watchFixture(t)
			run := scheduleWatch(t, svc, actor, task)
			finished := time.Date(2026, 7, 31, 12, 3, 0, 0, time.UTC)
			complete(t, svc, run.ID, RunCompletion{Verdict: verdict, Message: "boom", ExitCode: exitCode(1),
				LogPath: "/log", FinishedAt: finished.Format(time.RFC3339Nano)})
			status, _, failures, next := openVisit(t, svc, task)
			if status != "merge" || failures != 1 || next != finished.Add(5*time.Minute).Format(dispatchedAtLayout) {
				t.Fatalf("visit = %s failures %d next %q", status, failures, next)
			}
			failed := eventPayload(t, svc, task, "workflow.script_failed")
			if failed["run_id"] != float64(run.ID) || failed["script"] != "watch/merge.sh" || failed["log_path"] != "/log" {
				t.Fatalf("event = %v", failed)
			}
		})
	}
}

func TestWatchOutcomeAppliesTheTransition(t *testing.T) {
	svc, actor, task := watchFixture(t)
	run := scheduleWatch(t, svc, actor, task)
	complete(t, svc, run.ID, RunCompletion{Verdict: "outcome", Outcome: "merged", Message: "merged as 9f2c1e7",
		ExitCode: exitCode(0), Artifacts: map[string]string{"merge_commit": "9f2c1e7"}})
	stored := storedTask(t, svc, task.Key)
	if stored.WorkflowStatus != "done" || stored.Status != StatusDone {
		t.Fatalf("stored = %#v", stored)
	}
	var outcome, message string
	if err := svc.db.QueryRow(`SELECT outcome, message FROM task_status_visits WHERE task_id = ? AND status_id = 'merge'`,
		task.ID).Scan(&outcome, &message); err != nil {
		t.Fatal(err)
	}
	if outcome != "merged" || message != "merged as 9f2c1e7" {
		t.Fatalf("visit = %q %q", outcome, message)
	}
	var enteredBy string
	if err := svc.db.QueryRow(`SELECT entered_by FROM task_status_visits WHERE task_id = ? AND left_at = ''`, task.ID).Scan(&enteredBy); err != nil {
		t.Fatal(err)
	}
	if enteredBy != "script:watch/merge.sh" {
		t.Fatalf("entered_by = %q", enteredBy)
	}
	var reqActor, reqState, reqOutcome string
	if err := svc.db.QueryRow(`SELECT actor, state, outcome FROM task_transition_requests WHERE task_id = ? ORDER BY id DESC LIMIT 1`,
		task.ID).Scan(&reqActor, &reqState, &reqOutcome); err != nil {
		t.Fatal(err)
	}
	if reqActor != "script:watch/merge.sh" || reqState != "applied" || reqOutcome != "merged" {
		t.Fatalf("request = %s %s %s", reqActor, reqState, reqOutcome)
	}
	_, history, err := svc.GetArtifact(context.Background(), actor, task.Key, "merge_commit")
	if err != nil || len(history) != 1 || history[0].Author != "script:watch/merge.sh" || history[0].Value != "9f2c1e7" {
		t.Fatalf("artifact = %#v, %v", history, err)
	}
}

func TestWatchUndeclaredOutcomeIsAFailure(t *testing.T) {
	svc, actor, task := watchFixture(t)
	run := scheduleWatch(t, svc, actor, task)
	complete(t, svc, run.ID, RunCompletion{Verdict: "outcome", Outcome: "exploded", ExitCode: exitCode(0)})
	if status, _, failures, next := openVisit(t, svc, task); status != "merge" || failures != 1 || next == "" {
		t.Fatalf("visit = %s failures %d next %q", status, failures, next)
	}
}

func TestLeavingAScriptStatusStopsTheWatch(t *testing.T) {
	t.Run("move cancels a pending watch run", func(t *testing.T) {
		svc, actor, task := watchFixture(t)
		run := scheduleWatch(t, svc, actor, task)
		if _, err := svc.db.Exec(`UPDATE task_status_visits SET next_watch_at = 'x' WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.MoveWorkflow(context.Background(), actor, task.Key, "develop", "manual"); err != nil {
			t.Fatal(err)
		}
		if state, _, _ := runState(t, svc, run.ID); state != "cancelled" {
			t.Fatalf("run = %s", state)
		}
		if n := countRows(t, svc, `SELECT COUNT(*) FROM task_status_visits WHERE task_id = ? AND next_watch_at <> ''`, task.ID); n != 0 {
			t.Fatalf("visits still scheduled = %d", n)
		}
	})
	t.Run("cancel requests cancellation of a running watch run", func(t *testing.T) {
		svc, actor, task := watchFixture(t)
		ctx := context.Background()
		run := scheduleWatch(t, svc, actor, task)
		if ok, err := svc.ClaimScriptRun(ctx, run.ID, "t", "/log"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if _, err := svc.db.Exec(`UPDATE task_status_visits SET next_watch_at = 'x' WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.CancelWorkflowTask(ctx, actor, task.Key); err != nil {
			t.Fatal(err)
		}
		if state, _, cancel := runState(t, svc, run.ID); state != "running" || !cancel {
			t.Fatalf("run = %s cancel %v", state, cancel)
		}
		if n := countRows(t, svc, `SELECT COUNT(*) FROM task_status_visits WHERE task_id = ? AND next_watch_at <> ''`, task.ID); n != 0 {
			t.Fatalf("visits still scheduled = %d", n)
		}
		complete(t, svc, run.ID, RunCompletion{Verdict: "outcome", Outcome: "merged"})
		if state, _, _ := runState(t, svc, run.ID); state != "cancelled" {
			t.Fatalf("run = %s", state)
		}
		if got := storedTask(t, svc, task.Key); got.Status != StatusCancelled || got.WorkflowStatus != "merge" {
			t.Fatalf("task = %#v", got)
		}
	})
}

func TestAdvanceWithChecksWaitsForAStoppingRun(t *testing.T) {
	svc, actor, task := watchFixture(t)
	ctx := context.Background()
	run := scheduleWatch(t, svc, actor, task)
	if ok, err := svc.ClaimScriptRun(ctx, run.ID, "t", "/log"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// The operator returns the task to review while the watch run still dies.
	if _, err := svc.MoveWorkflow(ctx, actor, task.Key, "review", "redo"); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, svc, `SELECT COUNT(*) FROM task_transition_requests WHERE task_id = ?`, task.ID)
	_, err := svc.Advance(ctx, AgentActor("reviewer-1"), task.Key, AdvanceInput{Outcome: "approve"})
	if ErrorCode(err) != "script_running" || ErrorStatus(err) != 409 {
		t.Fatalf("advance while a run stops: %v", err)
	}
	if after := countRows(t, svc, `SELECT COUNT(*) FROM task_transition_requests WHERE task_id = ?`, task.ID); after != before {
		t.Fatalf("requests %d -> %d", before, after)
	}
	complete(t, svc, run.ID, RunCompletion{Verdict: "failure"})
	if _, err := svc.Advance(ctx, AgentActor("reviewer-1"), task.Key, AdvanceInput{Outcome: "approve"}); err != nil {
		t.Fatalf("advance after the run stopped: %v", err)
	}
}

func TestRecoverScriptRuns(t *testing.T) {
	t.Run("an interrupted check fails its request", func(t *testing.T) {
		svc, actor, task, _ := runFixture(t)
		ctx := context.Background()
		request, first := advanceApprove(t, svc, actor, task)
		if ok, err := svc.ClaimScriptRun(ctx, first.ID, "t", "/log"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := svc.RecoverScriptRuns(ctx); err != nil {
			t.Fatal(err)
		}
		if state, _, _ := runState(t, svc, first.ID); state != "interrupted" {
			t.Fatalf("run = %s", state)
		}
		state, result := requestRow(t, svc, request.ID)
		if state != "failed" || !strings.Contains(result, "restarted") {
			t.Fatalf("request = %s %q", state, result)
		}
		if _, _, failures, _ := openVisit(t, svc, task); failures != 1 {
			t.Fatalf("failures = %d", failures)
		}
		// Completing it later is a no-op.
		complete(t, svc, first.ID, RunCompletion{Verdict: "pass"})
		if state, _, _ := runState(t, svc, first.ID); state != "interrupted" {
			t.Fatalf("run = %s", state)
		}
	})
	t.Run("an interrupted watch run reschedules and a pending run stays", func(t *testing.T) {
		svc, actor, task := watchFixture(t)
		ctx := context.Background()
		run := scheduleWatch(t, svc, actor, task)
		if ok, err := svc.ClaimScriptRun(ctx, run.ID, "t", "/log"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := svc.RecoverScriptRuns(ctx); err != nil {
			t.Fatal(err)
		}
		if state, _, _ := runState(t, svc, run.ID); state != "interrupted" {
			t.Fatalf("run = %s", state)
		}
		// The next run follows after every, as after any run: an interrupted
		// run may have been killed by the restart it caused.
		every := svc.clock().Add(5 * time.Minute)
		if _, _, _, next := openVisit(t, svc, task); next != every.UTC().Format(dispatchedAtLayout) {
			t.Fatalf("next_watch_at = %q", next)
		}
		if n, err := svc.ScheduleDueWatches(ctx, every); err != nil || n != 1 {
			t.Fatalf("scheduled = %d, %v", n, err)
		}
		pending := listRuns(t, svc, actor, task)[0]
		if err := svc.RecoverScriptRuns(ctx); err != nil {
			t.Fatal(err)
		}
		if state, _, _ := runState(t, svc, pending.ID); state != "pending" {
			t.Fatalf("pending run = %s", state)
		}
	})
}

func TestRunningScriptRunsCarryTheirPID(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	ctx := context.Background()
	_, first := advanceApprove(t, svc, actor, task)
	if runs, err := svc.RunningScriptRuns(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("running before the claim = %#v, %v", runs, err)
	}
	if ok, err := svc.ClaimScriptRun(ctx, first.ID, "t", "/log"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	runs, err := svc.RunningScriptRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].ID != first.ID || runs[0].PID != nil {
		t.Fatalf("running without a pid = %#v, %v", runs, err)
	}
	if err := svc.SetScriptRunPID(ctx, first.ID, 4242); err != nil {
		t.Fatal(err)
	}
	runs, err = svc.RunningScriptRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].PID == nil || *runs[0].PID != 4242 || runs[0].TaskKey != task.Key {
		t.Fatalf("running = %#v, %v", runs, err)
	}
	// The pid is the worker's business; a reader never sees it.
	raw, err := json.Marshal(runs[0])
	if err != nil || strings.Contains(string(raw), "4242") {
		t.Fatalf("run json = %s, %v", raw, err)
	}
}

func TestPendingRunJobs(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	ctx := context.Background()
	if err := svc.SetQueueSecret(ctx, actor, "DEV", "GH_TOKEN", "s3cr3t-value"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetArtifact(ctx, actor, task.Key, "plan", "the plan"); err != nil {
		t.Fatal(err)
	}
	_, first := advanceApprove(t, svc, actor, task)
	jobs, err := svc.PendingRunJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %#v, %v", jobs, err)
	}
	job := jobs[0]
	if job.Run.ID != first.ID || job.Queue != "DEV" || job.WorkflowName != "development" || job.WorkflowVersion != "0.1.0" ||
		job.WorkflowDigest != task.WorkflowDigest || job.Status != "review" || job.Outcome != "approve" ||
		job.Timeout != workflowfile.DefaultCheckTimeout || job.Holder != "" {
		t.Fatalf("job = %#v", job)
	}
	if !reflect.DeepEqual(job.WorkflowEnv, map[string]string{"REPO": "org/repo"}) ||
		!reflect.DeepEqual(job.QueueSecrets, map[string]string{"GH_TOKEN": "s3cr3t-value"}) ||
		!reflect.DeepEqual(job.Outcomes, []string{"approve", "changes"}) ||
		!reflect.DeepEqual(job.Artifacts, []string{"plan", "summary", "merge_commit"}) {
		t.Fatalf("job = %#v", job)
	}
	if strings.Contains(string(job.Snapshot), "s3cr3t-value") || strings.Contains(string(job.Snapshot), "org/repo") {
		t.Fatalf("snapshot leaks a secret or env value: %s", job.Snapshot)
	}
	var snapshot struct {
		Key, Queue, Title, Description, Customer, Status, Category, Outcome, Message string
		Priority                                                                     any
		Holders                                                                      map[string]string
		Artifacts                                                                    []struct {
			Name, Value, Author string
			CreatedAt           string `json:"created_at"`
		}
		Workflow struct{ Name, Version, Digest string }
	}
	if err := json.Unmarshal(job.Snapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Key != task.Key || snapshot.Queue != "DEV" || snapshot.Title != "runs" || snapshot.Customer != task.Customer ||
		snapshot.Status != "review" || snapshot.Category != StatusInProgress || snapshot.Outcome != "approve" ||
		snapshot.Message != "looks good" || snapshot.Priority == nil ||
		!reflect.DeepEqual(snapshot.Holders, map[string]string{"developers": "dev-1", "reviewers": "reviewer-1"}) ||
		len(snapshot.Artifacts) != 1 || snapshot.Artifacts[0].Name != "plan" || snapshot.Artifacts[0].Value != "the plan" ||
		snapshot.Artifacts[0].Author != "user:customer" || snapshot.Artifacts[0].CreatedAt == "" ||
		snapshot.Workflow.Name != "development" || snapshot.Workflow.Version != "0.1.0" || snapshot.Workflow.Digest != task.WorkflowDigest {
		t.Fatalf("snapshot = %s", job.Snapshot)
	}

	// The second check runs as the agent with its own timeout.
	complete(t, svc, first.ID, RunCompletion{Verdict: "pass"})
	jobs, err = svc.PendingRunJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Holder != "reviewer-1" || jobs[0].Timeout != 90*time.Second || jobs[0].Run.RunAs != "agent" {
		t.Fatalf("jobs = %#v, %v", jobs, err)
	}

	// A watch job has no outcome, the watch timeout, and no holder.
	complete(t, svc, jobs[0].Run.ID, RunCompletion{Verdict: "reject"})
	task = enter(t, svc, task.Key, "merge", "approved")
	scheduleWatch(t, svc, actor, task)
	jobs, err = svc.PendingRunJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %#v, %v", jobs, err)
	}
	if jobs[0].Status != "merge" || jobs[0].Outcome != "" || jobs[0].Timeout != 2*time.Minute || jobs[0].Holder != "" ||
		!reflect.DeepEqual(jobs[0].Outcomes, []string{"merged"}) {
		t.Fatalf("watch job = %#v", jobs[0])
	}
	if err := json.Unmarshal(jobs[0].Snapshot, &snapshot); err != nil || snapshot.Outcome != "" || snapshot.Message != "" {
		t.Fatalf("watch snapshot = %s, %v", jobs[0].Snapshot, err)
	}
}

func TestWatchSnapshotCarriesTheOpenVisit(t *testing.T) {
	svc, actor, task := watchFixture(t)
	ctx := context.Background()
	visitOf := func() (int64, string) {
		t.Helper()
		jobs, err := svc.PendingRunJobs(ctx)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %#v, %v", jobs, err)
		}
		var snapshot struct {
			Visit struct {
				ID        int64  `json:"id"`
				EnteredAt string `json:"entered_at"`
			} `json:"visit"`
		}
		if err := json.Unmarshal(jobs[0].Snapshot, &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot.Visit.ID, snapshot.Visit.EnteredAt
	}
	openID := func() (int64, string) {
		t.Helper()
		var id int64
		var entered string
		if err := svc.db.QueryRow(`SELECT id, entered_at FROM task_status_visits WHERE task_id = ? AND left_at = ''`, task.ID).
			Scan(&id, &entered); err != nil {
			t.Fatal(err)
		}
		return id, entered
	}
	run := scheduleWatch(t, svc, actor, task)
	firstID, firstEntered := visitOf()
	if wantID, wantEntered := openID(); firstID != wantID || firstEntered != wantEntered || firstID == 0 {
		t.Fatalf("visit = %d %q, want %d %q", firstID, firstEntered, wantID, wantEntered)
	}
	complete(t, svc, run.ID, RunCompletion{Verdict: "quiet"})
	enter(t, svc, task.Key, "develop", "")
	enter(t, svc, task.Key, "merge", "approved")
	scheduleWatch(t, svc, actor, task)
	secondID, _ := visitOf()
	if wantID, _ := openID(); secondID != wantID || secondID == firstID {
		t.Fatalf("visit after re-entry = %d, first %d, open %d", secondID, firstID, wantID)
	}
}

func TestScriptRunsAreReadable(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	ctx := context.Background()
	_, first := advanceApprove(t, svc, actor, task)
	complete(t, svc, first.ID, RunCompletion{Verdict: "pass"})
	runs := listRuns(t, svc, actor, task)
	if len(runs) != 2 || runs[0].ID <= runs[1].ID {
		t.Fatalf("runs not newest first: %#v", runs)
	}
	if _, err := svc.ListScriptRuns(ctx, AgentActor("stranger"), task.Key); ErrorCode(err) != "not_found" {
		t.Fatalf("stranger list: %v", err)
	}
	if _, err := svc.GetScriptRun(ctx, AgentActor("stranger"), task.Key, first.ID); ErrorCode(err) != "not_found" {
		t.Fatalf("stranger get: %v", err)
	}
	if _, err := svc.GetScriptRun(ctx, actor, task.Key, 999); ErrorStatus(err) != 404 {
		t.Fatalf("missing run: %v", err)
	}
	view, err := svc.GetWorkflow(ctx, actor, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(view.Runs, runs) {
		t.Fatalf("view runs = %#v, want %#v", view.Runs, runs)
	}
	raw, err := json.Marshal(view)
	if err != nil || !strings.Contains(string(raw), `"runs":[`) {
		t.Fatalf("view json = %s, %v", raw, err)
	}
}

func TestIsTaskDirKey(t *testing.T) {
	for key, want := range map[string]bool{
		"DEV-1": true, "a.b": true,
		"": false, ".": false, "..": false, "a/b": false, `a\b`: false, "/abs": false, "../x": false,
	} {
		if got := IsTaskDirKey(key); got != want {
			t.Errorf("IsTaskDirKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestRunJobNeverMarshalsSecretsOrEnv(t *testing.T) {
	raw, err := json.Marshal(RunJob{
		WorkflowEnv:  map[string]string{"REPO": "env-value-1"},
		QueueSecrets: map[string]string{"GH_TOKEN": "secret-value-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "env-value-1") || strings.Contains(string(raw), "secret-value-1") {
		t.Fatalf("job json = %s", raw)
	}
}

func TestQuietWatchRunsLeaveNoEventAndOnlyTheNewestAreKept(t *testing.T) {
	svc, actor, task := watchFixture(t)
	ctx := context.Background()
	at := svc.clock()
	runOnce := func(verdict string) ScriptRun {
		t.Helper()
		if n, err := svc.ScheduleDueWatches(ctx, at); err != nil || n != 1 {
			t.Fatalf("scheduled = %d, %v", n, err)
		}
		run := listRuns(t, svc, actor, task)[0]
		complete(t, svc, run.ID, RunCompletion{Verdict: verdict, ExitCode: exitCode(111), FinishedAt: at.Format(time.RFC3339Nano)})
		at = at.Add(5 * time.Minute)
		return run
	}
	failed := runOnce("failure")
	var quiet []ScriptRun
	for range workflowViewRuns + 3 {
		quiet = append(quiet, runOnce("quiet"))
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_events WHERE task_id = ? AND kind = 'workflow.script_run'`, task.ID); n != 1 {
		t.Fatalf("script_run events = %d; only the failure has one", n)
	}
	if n := countRows(t, svc, `SELECT COUNT(*) FROM task_script_runs WHERE task_id = ? AND verdict = 'quiet'`, task.ID); n != workflowViewRuns {
		t.Fatalf("quiet runs kept = %d", n)
	}
	for _, gone := range quiet[:3] {
		if _, err := svc.GetScriptRun(ctx, actor, task.Key, gone.ID); ErrorCode(err) != "run_not_found" {
			t.Fatalf("old quiet run %d: %v", gone.ID, err)
		}
	}
	if _, err := svc.GetScriptRun(ctx, actor, task.Key, failed.ID); err != nil {
		t.Fatalf("the failed run was pruned: %v", err)
	}
}

func TestPendingRunJobsSkipsARunWhoseJobCannotBeBuilt(t *testing.T) {
	svc, actor, task := watchFixture(t)
	ctx := context.Background()
	other := enter(t, svc, mustCreateDev(t, svc, actor, "other").Key, "merge", "approved")
	if n, err := svc.ScheduleDueWatches(ctx, svc.clock()); err != nil || n != 2 {
		t.Fatalf("scheduled = %d, %v", n, err)
	}
	broken := listRuns(t, svc, actor, task)[0].ID
	if _, err := svc.db.Exec(`UPDATE tasks SET workflow_digest = 'missing' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	svc.SetLogger(slog.New(slog.NewTextHandler(&logged, nil)))
	jobs, err := svc.PendingRunJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Run.TaskKey != other.Key {
		t.Fatalf("jobs = %#v, %v", jobs, err)
	}
	if !strings.Contains(logged.String(), "run_id="+strconv.FormatInt(broken, 10)) || !strings.Contains(logged.String(), "err=") {
		t.Fatalf("log = %q", logged.String())
	}
}

func TestCompleteScriptRunNormalizesFinishedAt(t *testing.T) {
	for given, want := range map[string]string{
		"2026-07-31T14:00:02+02:00": "2026-07-31T12:00:02Z",
		"not a time":                "2026-07-31T12:00:00Z",
		"":                          "2026-07-31T12:00:00Z",
	} {
		t.Run(given, func(t *testing.T) {
			svc, actor, task, _ := runFixture(t)
			_, first := advanceApprove(t, svc, actor, task)
			complete(t, svc, first.ID, RunCompletion{Verdict: "reject", FinishedAt: given})
			got, err := svc.GetScriptRun(context.Background(), actor, task.Key, first.ID)
			if err != nil || got.FinishedAt != want {
				t.Fatalf("finished_at = %q, %v; want %q", got.FinishedAt, err, want)
			}
		})
	}
}

func TestUndeclaredArtifactNameIsBounded(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	request, first := advanceApprove(t, svc, actor, task)
	name := strings.Repeat("ж", 100) // 200 bytes
	complete(t, svc, first.ID, RunCompletion{Verdict: "pass", Artifacts: map[string]string{name: "x"}})
	state, result := requestRow(t, svc, request.ID)
	cut := strings.Repeat("ж", 32) // 64 bytes
	if state != "failed" || !strings.Contains(result, cut) || strings.Contains(result, cut+"ж") || !utf8.ValidString(result) {
		t.Fatalf("request = %s %q", state, result)
	}
	failed := eventPayload(t, svc, task, "workflow.transition_failed")
	if message, _ := failed["message"].(string); !strings.Contains(message, cut) || strings.Contains(message, cut+"ж") {
		t.Fatalf("event message = %q", message)
	}
}

func TestAgentCheckWithoutAnAgentAssignee(t *testing.T) {
	svc, customer, task, queueRun, _ := logFixture(t)
	ctx := context.Background()
	// The customer took the task over before the agent check was created.
	if _, err := svc.db.Exec(`UPDATE tasks SET assignee = 'user:customer' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	complete(t, svc, queueRun.ID, RunCompletion{Verdict: "pass"})
	agentRun := listRuns(t, svc, customer, task)[0]
	if agentRun.RunAs != "agent" || agentRun.Holder != "" || agentRun.State != "pending" {
		t.Fatalf("agent run = %#v", agentRun)
	}
	jobs, err := svc.PendingRunJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Run.ID != agentRun.ID || jobs[0].Holder != "" {
		t.Fatalf("jobs = %#v, %v", jobs, err)
	}
	dir := filepath.Join(svc.runBaseDir, "tasks", task.Key, "runs", strconv.FormatInt(agentRun.ID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(dir, "run.log")
	if err := os.WriteFile(agentPath, []byte("agent log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setLogPath(t, svc, agentRun.ID, agentPath)
	if _, _, err := svc.ScriptRunLog(ctx, customer, task.Key, agentRun.ID, 0); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if _, _, err := svc.ScriptRunLog(ctx, AgentActor("reviewer-1"), task.Key, agentRun.ID, 0); ErrorCode(err) != "forbidden" {
		t.Fatalf("pool holder: %v", err)
	}
}

// TestHoldersKeepReadAccessToTheirTask: reviewer-1, a pool member that owns no
// queue, advances review into the script status "merge" through two checks.
// The assignee is cleared, yet its holder row lets it read the task's workflow
// data; it gains no write, and an agent with no holder row still sees nothing.
func TestHoldersKeepReadAccessToTheirTask(t *testing.T) {
	svc, customer, task, _ := runFixture(t)
	ctx := context.Background()
	def := runDefinition()
	def.Statuses[1].Transitions[0].To = "merge"
	rewriteDefinition(t, svc, task, def)
	request, first := advanceApprove(t, svc, customer, task)
	complete(t, svc, first.ID, RunCompletion{Verdict: "pass", Artifacts: map[string]string{"summary": "ci ok"}})
	second := listRuns(t, svc, customer, task)[0]
	complete(t, svc, second.ID, RunCompletion{Verdict: "pass"})
	if stored := storedTask(t, svc, task.Key); stored.WorkflowStatus != "merge" || stored.Assignee != "" {
		t.Fatalf("stored = %#v", stored)
	}

	reviewer := AgentActor("reviewer-1")
	if got, err := svc.GetTransitionRequest(ctx, reviewer, task.Key, request.ID); err != nil || got.State != "applied" {
		t.Fatalf("GetTransitionRequest = %#v, %v", got, err)
	}
	if view, err := svc.GetWorkflow(ctx, reviewer, task.Key); err != nil || view.Status != "merge" {
		t.Fatalf("GetWorkflow = %#v, %v", view, err)
	}
	if artifacts, err := svc.ListArtifacts(ctx, reviewer, task.Key); err != nil || len(artifacts) != 1 {
		t.Fatalf("ListArtifacts = %#v, %v", artifacts, err)
	}
	if _, _, err := svc.GetArtifact(ctx, reviewer, task.Key, "summary"); err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if runs, err := svc.ListScriptRuns(ctx, reviewer, task.Key); err != nil || len(runs) != 2 {
		t.Fatalf("ListScriptRuns = %#v, %v", runs, err)
	}
	if _, err := svc.GetScriptRun(ctx, reviewer, task.Key, first.ID); err != nil {
		t.Fatalf("GetScriptRun: %v", err)
	}
	if detail, err := svc.GetTask(ctx, reviewer, task.Key); err != nil || detail.Task.Access == "write" {
		t.Fatalf("GetTask = %q, %v", detail.Task.Access, err)
	}

	// Reading is all a holder row gives.
	if _, err := svc.Advance(ctx, reviewer, task.Key, AdvanceInput{Outcome: "merged"}); err == nil {
		t.Fatal("a holder advanced a script status")
	}
	if _, err := svc.SetArtifact(ctx, reviewer, task.Key, "merge_commit", "abc"); err == nil {
		t.Fatal("a holder set an artifact")
	}
	if _, err := svc.AddComment(ctx, reviewer, task.Key, AddCommentInput{Body: "hi"}); err == nil {
		t.Fatal("a holder commented")
	}
	status := StatusDone
	if _, err := svc.UpdateTask(ctx, reviewer, task.Key, UpdateTaskInput{Status: &status, Revision: storedTask(t, svc, task.Key).Revision}); err == nil {
		t.Fatal("a holder changed the status")
	}

	// dev-2 is a pool member with no holder row and no other access.
	stranger := AgentActor("dev-2")
	if _, err := svc.GetWorkflow(ctx, stranger, task.Key); ErrorCode(err) != "not_found" {
		t.Fatalf("stranger GetWorkflow: %v", err)
	}
	if _, err := svc.GetTransitionRequest(ctx, stranger, task.Key, request.ID); ErrorCode(err) != "not_found" {
		t.Fatalf("stranger GetTransitionRequest: %v", err)
	}
	if _, err := svc.GetTask(ctx, stranger, task.Key); ErrorCode(err) != "not_found" {
		t.Fatalf("stranger GetTask: %v", err)
	}
}
