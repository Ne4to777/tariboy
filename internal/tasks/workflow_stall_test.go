package tasks

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

// The service clock of the fixtures is fixed at 12:00, so every visit is
// entered then; an iteration from 12:00 to 12:05 spans it.
var (
	stallStart  = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	stallFinish = time.Date(2026, 7, 31, 12, 5, 0, 0, time.UTC)
)

func iterationEnd(agent, key string) IterationEnd {
	return IterationEnd{Agent: agent, IterationID: "it-1", GoalTaskKey: key, StartedAt: stallStart, FinishedAt: stallFinish}
}

func recordEnd(t *testing.T, svc *Service, end IterationEnd) {
	t.Helper()
	if err := svc.RecordIterationEnd(context.Background(), end); err != nil {
		t.Fatal(err)
	}
}

// pauseCommentBody returns the body of the workflow's open pause question.
func pauseCommentBody(t *testing.T, svc *Service, task Task) string {
	t.Helper()
	var bodies []string
	for _, wait := range openWaitRows(t, svc, task) {
		if wait.requesting == workflowActor && wait.expected == task.Customer {
			bodies = append(bodies, wait.body)
		}
	}
	if len(bodies) != 1 {
		t.Fatalf("open workflow waits on the customer = %q", bodies)
	}
	return bodies[0]
}

func intp(n int) *int { return &n }

func TestIdleIterationsPauseAtTheLimit(t *testing.T) {
	svc, _, task := requestFixture(t)
	for i := 1; i <= 2; i++ {
		recordEnd(t, svc, iterationEnd("dev-1", task.Key))
		if got := visitCounters(t, svc, task); got[0] != i {
			t.Fatalf("after %d idle iterations counters = %v", i, got)
		}
		if stored := storedTask(t, svc, task.Key); stored.WorkflowPausedReason != "" {
			t.Fatalf("paused early: %#v", stored)
		}
	}
	recordEnd(t, svc, iterationEnd("dev-1", task.Key))
	stored := storedTask(t, svc, task.Key)
	if stored.WorkflowPausedReason != PauseIdleIterations || stored.Status != StatusWaitCustomer || stored.WorkflowStatus != "develop" {
		t.Fatalf("stored = %#v", stored)
	}
	if body := pauseCommentBody(t, svc, task); !strings.Contains(body, "3 iterations in a row ended without a transition request") {
		t.Fatalf("pause comment = %q", body)
	}
	// A paused task counts nothing more.
	recordEnd(t, svc, iterationEnd("dev-1", task.Key))
	if got := visitCounters(t, svc, task); got[0] != 3 {
		t.Fatalf("counters after the pause = %v", got)
	}
	if n := countEvents(t, svc, task, "workflow.paused"); n != 1 {
		t.Fatalf("pause events = %d", n)
	}
}

func TestIdleIterationsThatDoNotCount(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, svc *Service, task Task) IterationEnd
	}{
		{"no goal", func(t *testing.T, svc *Service, task Task) IterationEnd {
			return iterationEnd("dev-1", "")
		}},
		{"a different goal", func(t *testing.T, svc *Service, task Task) IterationEnd {
			return iterationEnd("dev-1", "DEV-999")
		}},
		{"another agent", func(t *testing.T, svc *Service, task Task) IterationEnd {
			return iterationEnd("dev-2", task.Key)
		}},
		{"the status was entered during the iteration", func(t *testing.T, svc *Service, task Task) IterationEnd {
			end := iterationEnd("dev-1", task.Key)
			end.StartedAt = stallStart.Add(-time.Second)
			return end
		}},
		{"a released holder", func(t *testing.T, svc *Service, task Task) IterationEnd {
			if _, err := svc.db.Exec(`UPDATE task_workflow_holders SET released = 1 WHERE task_id = ?`, task.ID); err != nil {
				t.Fatal(err)
			}
			return iterationEnd("dev-1", task.Key)
		}},
		{"a paused task", func(t *testing.T, svc *Service, task Task) IterationEnd {
			pause(t, svc, task.Key, PauseScriptFailures, "")
			return iterationEnd("dev-1", task.Key)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, task := requestFixture(t)
			recordEnd(t, svc, tc.setup(t, svc, task))
			if got := visitCounters(t, svc, task); got[0] != 0 {
				t.Fatalf("counters = %v; the iteration must not count", got)
			}
		})
	}
	t.Run("a customer status", func(t *testing.T) {
		svc, _, task := requestFixture(t)
		task = enter(t, svc, task.Key, "approval", "ask")
		recordEnd(t, svc, iterationEnd("dev-1", task.Key))
		if got := visitCounters(t, svc, task); got[0] != 0 {
			t.Fatalf("counters = %v", got)
		}
	})
	t.Run("an unknown task or a task without a workflow", func(t *testing.T) {
		svc, actor, _ := requestFixture(t)
		plain, err := svc.CreateTask(context.Background(), actor, CreateTaskInput{Queue: "TASK", Title: "plain", Assignee: "agent:dev-1"})
		if err != nil {
			t.Fatal(err)
		}
		recordEnd(t, svc, iterationEnd("dev-1", "NOPE-1"))
		recordEnd(t, svc, iterationEnd("dev-1", plain.Key))
	})
}

func TestATransitionRequestDuringTheIterationResetsTheIdleCount(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	if _, err := svc.db.Exec(`UPDATE task_status_visits SET idle_iterations = 2 WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		t.Fatal(err)
	}
	_, run := advanceApprove(t, svc, actor, task)
	if got := visitCounters(t, svc, task); got[0] != 0 {
		t.Fatalf("counters after the request = %v; a request resets the idle count", got)
	}
	complete(t, svc, run.ID, RunCompletion{Verdict: "reject", Message: "CI is red", ExitCode: exitCode(112)})
	// The rejected request was made during the iteration: it is not idle.
	recordEnd(t, svc, iterationEnd("reviewer-1", task.Key))
	if got := visitCounters(t, svc, task); got[0] != 0 {
		t.Fatalf("counters = %v", got)
	}
	// The next iteration makes no request.
	next := iterationEnd("reviewer-1", task.Key)
	next.StartedAt, next.FinishedAt = stallFinish.Add(time.Second), stallFinish.Add(time.Minute)
	recordEnd(t, svc, next)
	if got := visitCounters(t, svc, task); got[0] != 1 {
		t.Fatalf("counters = %v", got)
	}
}

func TestIdleIterationLimits(t *testing.T) {
	t.Run("a status limit overrides the workflow limit", func(t *testing.T) {
		svc, _, task := requestFixture(t)
		def := requestDefinition()
		def.Limits = &workflowfile.Limits{IdleIterations: intp(5)}
		def.Statuses[0].Limits = &workflowfile.Limits{IdleIterations: intp(1)}
		rewriteDefinition(t, svc, task, def)
		recordEnd(t, svc, iterationEnd("dev-1", task.Key))
		if stored := storedTask(t, svc, task.Key); stored.WorkflowPausedReason != PauseIdleIterations {
			t.Fatalf("stored = %#v", stored)
		}
	})
	t.Run("a limit of zero never pauses", func(t *testing.T) {
		svc, _, task := requestFixture(t)
		def := requestDefinition()
		def.Limits = &workflowfile.Limits{IdleIterations: intp(0)}
		rewriteDefinition(t, svc, task, def)
		for i := 0; i < 5; i++ {
			recordEnd(t, svc, iterationEnd("dev-1", task.Key))
		}
		if stored := storedTask(t, svc, task.Key); stored.WorkflowPausedReason != "" {
			t.Fatalf("stored = %#v", stored)
		}
	})
}

func TestRejectedRequestsPauseAtTheLimit(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	if _, err := svc.db.Exec(`UPDATE task_status_visits SET rejected_requests = 4 WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		t.Fatal(err)
	}
	_, run := advanceApprove(t, svc, actor, task)
	complete(t, svc, run.ID, RunCompletion{Verdict: "reject", Message: "CI is red", ExitCode: exitCode(112)})
	stored := storedTask(t, svc, task.Key)
	if stored.WorkflowPausedReason != PauseRejectedRequests || stored.WorkflowStatus != "review" || stored.Assignee != "agent:reviewer-1" ||
		stored.Status != StatusWaitCustomer {
		t.Fatalf("stored = %#v", stored)
	}
	if n := countEvents(t, svc, task, "workflow.paused"); n != 1 {
		t.Fatalf("pause events = %d", n)
	}
	if body := pauseCommentBody(t, svc, task); !strings.Contains(body, "> CI is red") {
		t.Fatalf("pause comment = %q", body)
	}
	if got := visitCounters(t, svc, task); got[1] != 5 {
		t.Fatalf("counters = %v", got)
	}
}

func TestAnAppliedTransitionResetsTheCounters(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	if _, err := svc.db.Exec(`UPDATE task_status_visits SET idle_iterations = 2, rejected_requests = 4, script_failures = 2
		WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		t.Fatal(err)
	}
	_, first := advanceApprove(t, svc, actor, task)
	complete(t, svc, first.ID, RunCompletion{Verdict: "pass", ExitCode: exitCode(0)})
	second := listRuns(t, svc, actor, task)[0]
	complete(t, svc, second.ID, RunCompletion{Verdict: "pass", ExitCode: exitCode(0)})
	stored := storedTask(t, svc, task.Key)
	if stored.WorkflowStatus != "approval" || stored.WorkflowPausedReason != "" {
		t.Fatalf("stored = %#v", stored)
	}
	if got := visitCounters(t, svc, stored); got != [3]int{} {
		t.Fatalf("counters of the new visit = %v", got)
	}
}

func TestScriptFailuresPauseAtTheLimit(t *testing.T) {
	setFailures := func(t *testing.T, svc *Service, task Task, n int) {
		t.Helper()
		if _, err := svc.db.Exec(`UPDATE task_status_visits SET script_failures = ? WHERE task_id = ? AND left_at = ''`, n, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	// requirePaused checks the pause every limit leaves: the customer's
	// category and exactly one pause event.
	requirePaused := func(t *testing.T, svc *Service, task Task, status string) {
		t.Helper()
		stored := storedTask(t, svc, task.Key)
		if stored.WorkflowPausedReason != PauseScriptFailures || stored.WorkflowStatus != status || stored.Status != StatusWaitCustomer {
			t.Fatalf("stored = %#v", stored)
		}
		if n := countEvents(t, svc, task, "workflow.paused"); n != 1 {
			t.Fatalf("pause events = %d", n)
		}
	}
	t.Run("a check failure", func(t *testing.T) {
		svc, actor, task, _ := runFixture(t)
		setFailures(t, svc, task, 2)
		_, run := advanceApprove(t, svc, actor, task)
		complete(t, svc, run.ID, RunCompletion{Verdict: "failure", Message: "exit status 3", ExitCode: exitCode(3),
			LogPath: "/base/tasks/DEV/runs/1/run.log"})
		requirePaused(t, svc, task, "review")
		body := pauseCommentBody(t, svc, task)
		if !strings.Contains(body, "exit status 3") || !strings.Contains(body, "/base/tasks/DEV/runs/1/run.log") {
			t.Fatalf("pause comment = %q", body)
		}
	})
	t.Run("an interrupted check", func(t *testing.T) {
		svc, actor, task, _ := runFixture(t)
		setFailures(t, svc, task, 2)
		_, run := advanceApprove(t, svc, actor, task)
		if ok, err := svc.ClaimScriptRun(context.Background(), run.ID, "2026-07-31T12:00:01Z", "/runs/1/run.log"); err != nil || !ok {
			t.Fatalf("claim = %v, %v", ok, err)
		}
		if err := svc.RecoverScriptRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
		requirePaused(t, svc, task, "review")
	})
	t.Run("a watch failure stops the watch", func(t *testing.T) {
		svc, actor, task := watchFixture(t)
		setFailures(t, svc, task, 2)
		run := scheduleWatch(t, svc, actor, task)
		complete(t, svc, run.ID, RunCompletion{Verdict: "failure", Message: "boom", ExitCode: exitCode(1), LogPath: "/log"})
		requirePaused(t, svc, task, "merge")
		if _, _, failures, next := openVisit(t, svc, task); failures != 3 || next != "" {
			t.Fatalf("failures %d next %q; a paused watch is not rescheduled", failures, next)
		}
		if body := pauseCommentBody(t, svc, task); !strings.Contains(body, "boom") || !strings.Contains(body, "/log") {
			t.Fatalf("pause comment = %q", body)
		}
	})
	t.Run("below the limit", func(t *testing.T) {
		svc, actor, task, _ := runFixture(t)
		setFailures(t, svc, task, 1)
		_, run := advanceApprove(t, svc, actor, task)
		complete(t, svc, run.ID, RunCompletion{Verdict: "failure", Message: "exit status 3", ExitCode: exitCode(3)})
		if stored := storedTask(t, svc, task.Key); stored.WorkflowPausedReason != "" {
			t.Fatalf("stored = %#v", stored)
		}
	})
}

func TestEveryVerdictButAFailureResetsScriptFailures(t *testing.T) {
	for _, verdict := range []string{"pass", "reject"} {
		t.Run("check "+verdict, func(t *testing.T) {
			svc, actor, task, _ := runFixture(t)
			if _, err := svc.db.Exec(`UPDATE task_status_visits SET script_failures = 2 WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
				t.Fatal(err)
			}
			_, run := advanceApprove(t, svc, actor, task)
			complete(t, svc, run.ID, RunCompletion{Verdict: verdict, ExitCode: exitCode(0)})
			if _, _, failures, _ := openVisit(t, svc, task); failures != 0 {
				t.Fatalf("failures = %d", failures)
			}
		})
	}
	t.Run("watch quiet", func(t *testing.T) {
		svc, actor, task := watchFixture(t)
		if _, err := svc.db.Exec(`UPDATE task_status_visits SET script_failures = 2 WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
			t.Fatal(err)
		}
		run := scheduleWatch(t, svc, actor, task)
		complete(t, svc, run.ID, RunCompletion{Verdict: "quiet", ExitCode: exitCode(111)})
		if _, _, failures, _ := openVisit(t, svc, task); failures != 0 {
			t.Fatalf("failures = %d", failures)
		}
	})
}

// unavailableSince returns the holder row's unavailable_since.
func unavailableSince(t *testing.T, svc *Service, task Task, pool string) string {
	t.Helper()
	var since string
	if err := svc.db.QueryRow(`SELECT unavailable_since FROM task_workflow_holders WHERE task_id = ? AND pool = ?`,
		task.ID, pool).Scan(&since); err != nil {
		t.Fatal(err)
	}
	return since
}

func checkHolders(t *testing.T, svc *Service, now time.Time) int {
	t.Helper()
	n, err := svc.CheckHolders(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCheckHoldersPausesAfterTheGrace(t *testing.T) {
	svc, _, task := requestFixture(t)
	if n := checkHolders(t, svc, stallStart); n != 0 || unavailableSince(t, svc, task, "developers") != "" {
		t.Fatalf("a working holder: paused %d since %q", n, unavailableSince(t, svc, task, "developers"))
	}
	setAgent(t, svc, "dev-1", `enabled = 0`)
	if n := checkHolders(t, svc, stallStart); n != 0 {
		t.Fatalf("paused %d on the first observation", n)
	}
	if since := unavailableSince(t, svc, task, "developers"); since == "" {
		t.Fatal("unavailable_since was not recorded")
	}
	if n := checkHolders(t, svc, stallStart.Add(5*time.Minute-time.Second)); n != 0 {
		t.Fatalf("paused %d before the grace", n)
	}
	if stored := storedTask(t, svc, task.Key); stored.WorkflowPausedReason != "" {
		t.Fatalf("stored = %#v", stored)
	}
	if n := checkHolders(t, svc, stallStart.Add(5*time.Minute)); n != 1 {
		t.Fatalf("paused %d at the grace", n)
	}
	stored := storedTask(t, svc, task.Key)
	if stored.WorkflowPausedReason != PauseHolderUnavailable || stored.Assignee != "agent:dev-1" || stored.WorkflowStatus != "develop" {
		t.Fatalf("stored = %#v", stored)
	}
	if body := pauseCommentBody(t, svc, task); !strings.Contains(body, "agent dev-1 is disabled") {
		t.Fatalf("pause comment = %q", body)
	}
	// A paused task is not checked again.
	if n := checkHolders(t, svc, stallStart.Add(time.Hour)); n != 0 {
		t.Fatalf("paused %d again", n)
	}
}

func TestCheckHoldersClearsTheMarkWhenTheHolderCanWorkAgain(t *testing.T) {
	svc, _, task := requestFixture(t)
	setAgent(t, svc, "dev-1", `enabled = 0`)
	checkHolders(t, svc, stallStart)
	setAgent(t, svc, "dev-1", `enabled = 1`)
	checkHolders(t, svc, stallStart.Add(time.Minute))
	if since := unavailableSince(t, svc, task, "developers"); since != "" {
		t.Fatalf("unavailable_since = %q", since)
	}
	// A new outage starts its own grace.
	setAgent(t, svc, "dev-1", `enabled = 0`)
	if n := checkHolders(t, svc, stallStart.Add(6*time.Minute)); n != 0 {
		t.Fatalf("paused %d on a new outage", n)
	}
}

func TestCheckHoldersWithAZeroGracePausesAtOnce(t *testing.T) {
	svc, _, task := requestFixture(t)
	def := requestDefinition()
	def.Statuses[0].Limits = &workflowfile.Limits{UnavailableGrace: "0s"}
	rewriteDefinition(t, svc, task, def)
	setAgent(t, svc, "dev-1", `loop_enabled = 0`)
	if n := checkHolders(t, svc, stallStart); n != 1 {
		t.Fatalf("paused %d", n)
	}
	if stored := storedTask(t, svc, task.Key); stored.WorkflowPausedReason != PauseHolderUnavailable {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestCheckHoldersCauses(t *testing.T) {
	cases := []struct {
		name, want string
		setup      func(t *testing.T, svc *Service)
	}{
		{"deleted", "agent dev-1 no longer exists", func(t *testing.T, svc *Service) {
			// Pool membership restricts deleting an agent.
			if _, err := svc.db.Exec(`DELETE FROM task_agent_pool_members WHERE agent = 'dev-1'`); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.db.Exec(`DELETE FROM agents WHERE name = 'dev-1'`); err != nil {
				t.Fatal(err)
			}
		}},
		{"disabled", "agent dev-1 is disabled", func(t *testing.T, svc *Service) { setAgent(t, svc, "dev-1", `enabled = 0`) }},
		{"loop disabled", "the loop of agent dev-1 is disabled", func(t *testing.T, svc *Service) { setAgent(t, svc, "dev-1", `loop_enabled = 0`) }},
		{"halted on error", "agent dev-1 is halted", func(t *testing.T, svc *Service) { setAgent(t, svc, "dev-1", `error_reason = 'boom'`) }},
		{"halted idle", "agent dev-1 is halted", func(t *testing.T, svc *Service) {
			setAgent(t, svc, "dev-1", `status_message = 'idle_limit (3 idle iterations)'`)
		}},
		{"left the pool", "agent dev-1 is no longer a member of pool developers", func(t *testing.T, svc *Service) {
			if _, err := svc.db.Exec(`DELETE FROM task_agent_pool_members WHERE agent = 'dev-1'`); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, task := requestFixture(t)
			def := requestDefinition()
			def.Limits = &workflowfile.Limits{UnavailableGrace: "0s"}
			rewriteDefinition(t, svc, task, def)
			tc.setup(t, svc)
			if n := checkHolders(t, svc, stallStart); n != 1 {
				t.Fatalf("paused %d", n)
			}
			if body := pauseCommentBody(t, svc, task); !strings.Contains(body, tc.want) {
				t.Fatalf("pause comment = %q; want %q", body, tc.want)
			}
		})
	}
	for _, healthy := range []string{`goal_enabled = 0`, `current_goal_task_key = 'DEV-999'`, `status_message = 'working on it'`} {
		t.Run("can work: "+healthy, func(t *testing.T) {
			svc, _, task := requestFixture(t)
			def := requestDefinition()
			def.Limits = &workflowfile.Limits{UnavailableGrace: "0s"}
			rewriteDefinition(t, svc, task, def)
			setAgent(t, svc, "dev-1", healthy)
			if n := checkHolders(t, svc, stallStart); n != 0 {
				t.Fatalf("paused %d", n)
			}
		})
	}
}

func TestCheckHoldersIgnoresTasksOutsideAnActivePoolStatus(t *testing.T) {
	setup := func(t *testing.T) (*Service, Task) {
		t.Helper()
		svc, _, task := requestFixture(t)
		def := requestDefinition()
		def.Limits = &workflowfile.Limits{UnavailableGrace: "0s"}
		rewriteDefinition(t, svc, task, def)
		setAgent(t, svc, "dev-1", `enabled = 0`)
		return svc, task
	}
	t.Run("paused", func(t *testing.T) {
		svc, task := setup(t)
		pause(t, svc, task.Key, PauseIdleIterations, "")
		if n := checkHolders(t, svc, stallStart); n != 0 {
			t.Fatalf("paused %d", n)
		}
		if stored := storedTask(t, svc, task.Key); stored.WorkflowPausedReason != PauseIdleIterations {
			t.Fatalf("stored = %#v", stored)
		}
	})
	t.Run("closed", func(t *testing.T) {
		svc, task := setup(t)
		enter(t, svc, task.Key, "done", "merged")
		if n := checkHolders(t, svc, stallStart); n != 0 {
			t.Fatalf("paused %d", n)
		}
	})
	t.Run("customer status", func(t *testing.T) {
		svc, task := setup(t)
		enter(t, svc, task.Key, "approval", "ask")
		if n := checkHolders(t, svc, stallStart); n != 0 {
			t.Fatalf("paused %d", n)
		}
	})
}

func TestANewDispatchClearsUnavailableSince(t *testing.T) {
	svc, _, task := requestFixture(t)
	setAgent(t, svc, "dev-1", `enabled = 0`)
	checkHolders(t, svc, stallStart)
	if unavailableSince(t, svc, task, "developers") == "" {
		t.Fatal("not marked")
	}
	// Entering the status again dispatches the holder again.
	task = enter(t, svc, task.Key, "develop", "again")
	if since := unavailableSince(t, svc, task, "developers"); since != "" {
		t.Fatalf("unavailable_since carried over: %q", since)
	}
}

// at returns the stall test time minutes after 12:00.
func at(minutes int) time.Time { return stallStart.Add(time.Duration(minutes) * time.Minute) }

func iterationBetween(agent, key string, from, to int) IterationEnd {
	end := iterationEnd(agent, key)
	end.StartedAt, end.FinishedAt = at(from), at(to)
	return end
}

func TestAPendingCheckKeepsIterationsFromCountingIdle(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	request, _ := advanceApprove(t, svc, actor, task)
	// The check stays pending across several iterations.
	for i := 0; i < 4; i++ {
		recordEnd(t, svc, iterationBetween("reviewer-1", task.Key, 1+2*i, 2+2*i))
	}
	if got := visitCounters(t, svc, task); got[0] != 0 {
		t.Fatalf("counters = %v; a pending check is not idleness", got)
	}
	// The request is cancelled during the next iteration: still not idle.
	if _, err := svc.db.Exec(`UPDATE task_transition_requests SET state = 'cancelled', finished_at = ? WHERE id = ?`,
		at(11).Format(time.RFC3339Nano), request.ID); err != nil {
		t.Fatal(err)
	}
	recordEnd(t, svc, iterationBetween("reviewer-1", task.Key, 10, 12))
	if got := visitCounters(t, svc, task); got[0] != 0 {
		t.Fatalf("counters = %v; a request cancelled during the iteration is not idleness", got)
	}
	// An iteration reported after the request finished, which ended while it
	// was still pending, does not count either.
	if _, err := svc.db.Exec(`UPDATE task_transition_requests SET state = 'rejected', finished_at = ? WHERE id = ?`,
		at(30).Format(time.RFC3339Nano), request.ID); err != nil {
		t.Fatal(err)
	}
	recordEnd(t, svc, iterationBetween("reviewer-1", task.Key, 20, 25))
	if got := visitCounters(t, svc, task); got[0] != 0 {
		t.Fatalf("counters = %v; the request was pending when the iteration ended", got)
	}
	recordEnd(t, svc, iterationBetween("reviewer-1", task.Key, 31, 35))
	if got := visitCounters(t, svc, task); got[0] != 1 {
		t.Fatalf("counters = %v; the next plain iteration counts", got)
	}
}

func TestTheHoldersQuestionKeepsIterationsFromCountingIdle(t *testing.T) {
	svc, _, task := requestFixture(t)
	ctx := context.Background()
	setClock := func(minutes int) { svc.clock = func() time.Time { return at(minutes) } }
	// The holder asks during the first iteration; the iteration ends while
	// the question is open.
	setClock(2)
	if _, err := svc.AddComment(ctx, AgentActor("dev-1"), task.Key, AddCommentInput{Body: "@" + task.Customer + " which database?"}); err != nil {
		t.Fatal(err)
	}
	if stored := storedTask(t, svc, task.Key); stored.Status != StatusWaitCustomer {
		t.Fatalf("stored = %#v", stored)
	}
	recordEnd(t, svc, iterationBetween("dev-1", task.Key, 0, 5))
	// An iteration that only waits for the answer counts nothing.
	recordEnd(t, svc, iterationBetween("dev-1", task.Key, 6, 10))
	if got := visitCounters(t, svc, task); got[0] != 0 {
		t.Fatalf("counters = %v; a question is not idleness", got)
	}
	// The answer clears what counted before the question.
	if _, err := svc.db.Exec(`UPDATE task_status_visits SET idle_iterations = 2 WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		t.Fatal(err)
	}
	setClock(12)
	if _, err := svc.AddComment(ctx, CustomerActor("customer"), task.Key, AddCommentInput{Body: "Postgres."}); err != nil {
		t.Fatal(err)
	}
	if got := visitCounters(t, svc, task); got[0] != 0 {
		t.Fatalf("counters after the answer = %v", got)
	}
	if stored := storedTask(t, svc, task.Key); stored.Status != StatusInProgress {
		t.Fatalf("stored = %#v", stored)
	}
	recordEnd(t, svc, iterationBetween("dev-1", task.Key, 13, 18))
	if got := visitCounters(t, svc, task); got[0] != 1 {
		t.Fatalf("counters = %v; the next plain iteration counts", got)
	}
}

func TestAQuestionAnsweredWithinTheIterationIsNotIdle(t *testing.T) {
	svc, _, task := requestFixture(t)
	ctx := context.Background()
	svc.clock = func() time.Time { return at(2) }
	if _, err := svc.AddComment(ctx, AgentActor("dev-1"), task.Key, AddCommentInput{Body: "@" + task.Customer + " which database?"}); err != nil {
		t.Fatal(err)
	}
	svc.clock = func() time.Time { return at(3) }
	if _, err := svc.AddComment(ctx, CustomerActor("customer"), task.Key, AddCommentInput{Body: "Postgres."}); err != nil {
		t.Fatal(err)
	}
	recordEnd(t, svc, iterationBetween("dev-1", task.Key, 1, 5))
	if got := visitCounters(t, svc, task); got[0] != 0 {
		t.Fatalf("counters = %v; the holder asked during the iteration", got)
	}
}

// resumedAt returns the open visit's resumed_at.
func resumedAt(t *testing.T, svc *Service, task Task) string {
	t.Helper()
	var resumed string
	if err := svc.db.QueryRow(`SELECT resumed_at FROM task_status_visits WHERE task_id = ? AND left_at = ''`, task.ID).Scan(&resumed); err != nil {
		t.Fatal(err)
	}
	return resumed
}

func TestAnIterationThatStartedBeforeAResumeIsNotIdle(t *testing.T) {
	svc, actor, task := requestFixture(t)
	pause(t, svc, task.Key, PauseIdleIterations, "")
	svc.clock = func() time.Time { return at(10) }
	if _, err := svc.ResumeWorkflow(context.Background(), actor, task.Key, ResumeContinue); err != nil {
		t.Fatal(err)
	}
	if got := resumedAt(t, svc, task); got != at(10).Format(time.RFC3339Nano) {
		t.Fatalf("resumed_at = %q", got)
	}
	recordEnd(t, svc, iterationBetween("dev-1", task.Key, 5, 15))
	if got := visitCounters(t, svc, task); got[0] != 0 {
		t.Fatalf("counters = %v; the iteration started before the resume", got)
	}
	recordEnd(t, svc, iterationBetween("dev-1", task.Key, 16, 20))
	if got := visitCounters(t, svc, task); got[0] != 1 {
		t.Fatalf("counters = %v", got)
	}
}

func TestResumeReleaseRecordsResumedAt(t *testing.T) {
	svc, actor, task := requestFixture(t)
	pause(t, svc, task.Key, PauseIdleIterations, "")
	svc.clock = func() time.Time { return at(10) }
	if _, err := svc.ResumeWorkflow(context.Background(), actor, task.Key, ResumeRelease); err != nil {
		t.Fatal(err)
	}
	if got := resumedAt(t, svc, task); got != at(10).Format(time.RFC3339Nano) {
		t.Fatalf("resumed_at = %q", got)
	}
}

func TestUnavailableHolderTasksSelectsOnlyCandidates(t *testing.T) {
	svc, actor, task := requestFixture(t)
	other := mustCreateDev(t, svc, actor, "other")
	ctx := context.Background()
	if ids, err := svc.unavailableHolderTasks(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("ids = %v, %v; every holder can work", ids, err)
	}
	// The other task is held by dev-2, who can work.
	if _, err := svc.db.Exec(`UPDATE tasks SET assignee = 'agent:dev-2' WHERE task_key = ?`, other.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE task_workflow_holders SET agent = 'dev-2' WHERE task_id = (SELECT id FROM tasks WHERE task_key = ?)`, other.Key); err != nil {
		t.Fatal(err)
	}
	setAgent(t, svc, "dev-1", `enabled = 0`)
	if ids, err := svc.unavailableHolderTasks(ctx); err != nil || len(ids) != 1 || ids[0] != task.ID {
		t.Fatalf("ids = %v, %v; want only %d", ids, err, task.ID)
	}
	// A marked row stays a candidate after its agent can work again, so the
	// check clears the mark.
	checkHolders(t, svc, stallStart)
	setAgent(t, svc, "dev-1", `enabled = 1`)
	if ids, err := svc.unavailableHolderTasks(ctx); err != nil || len(ids) != 1 || ids[0] != task.ID {
		t.Fatalf("ids = %v, %v; want the marked %d", ids, err, task.ID)
	}
	checkHolders(t, svc, stallStart.Add(time.Minute))
	if ids, err := svc.unavailableHolderTasks(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("ids = %v, %v; the mark was cleared", ids, err)
	}
}

func TestCheckHoldersLogsAnUnloadableManifestOnce(t *testing.T) {
	svc, _, task := requestFixture(t)
	setAgent(t, svc, "dev-1", `enabled = 0`)
	var logged strings.Builder
	svc.SetLogger(slog.New(slog.NewTextHandler(&logged, nil)))
	setDigest := func(digest string) {
		t.Helper()
		if _, err := svc.db.Exec(`UPDATE tasks SET workflow_digest = ? WHERE id = ?`, digest, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	setDigest("missing")
	for i := 0; i < 3; i++ {
		checkHolders(t, svc, stallStart)
	}
	if n := strings.Count(logged.String(), "check workflow holder"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, logged.String())
	}
	// A load that succeeds again clears the record, so a new failure is logged.
	setDigest(task.WorkflowDigest)
	checkHolders(t, svc, stallStart)
	setDigest("missing")
	checkHolders(t, svc, stallStart)
	if n := strings.Count(logged.String(), "check workflow holder"); n != 2 {
		t.Fatalf("logged %d times:\n%s", n, logged.String())
	}
}

func TestAReleasedHolderCannotSetArtifacts(t *testing.T) {
	svc, _, task := requestFixture(t)
	if _, err := svc.db.Exec(`UPDATE task_workflow_holders SET released = 1 WHERE task_id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetArtifact(context.Background(), AgentActor("dev-1"), task.Key, "plan", "p"); ErrorCode(err) != "not_holder" {
		t.Fatalf("err = %v; a released holder does not hold the status", err)
	}
}

func TestAWatchOutcomeResetsScriptFailures(t *testing.T) {
	svc, actor, task := watchFixture(t)
	if _, err := svc.db.Exec(`UPDATE task_status_visits SET script_failures = 2 WHERE task_id = ? AND left_at = ''`, task.ID); err != nil {
		t.Fatal(err)
	}
	run := scheduleWatch(t, svc, actor, task)
	complete(t, svc, run.ID, RunCompletion{Verdict: "outcome", Outcome: "merged", ExitCode: exitCode(0)})
	stored := storedTask(t, svc, task.Key)
	if stored.WorkflowStatus != "done" {
		t.Fatalf("stored = %#v", stored)
	}
	if got := visitCounters(t, svc, stored); got[2] != 0 {
		t.Fatalf("counters = %v", got)
	}
}
