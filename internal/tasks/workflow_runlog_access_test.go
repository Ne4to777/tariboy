package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// accessFixture is logFixture with both checks of "approve" run: the first a
// queue run, the second a run as agent reviewer-1. Both have a log. dev-2 is a
// queue owner, as are dev-1 and reviewer-1, so each can read the task whether or not it holds it.
func accessFixture(t *testing.T) (svc *Service, customer Actor, task Task, queueRun, agentRun ScriptRun) {
	t.Helper()
	svc, customer, task, queueRun, path := logFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(path, []byte("queue log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	queue, err := svc.GetQueue(ctx, customer, "DEV")
	if err != nil {
		t.Fatal(err)
	}
	owners := append(append([]string{}, queue.Owners...), "dev-1", "dev-2", "reviewer-1")
	if _, err := svc.UpdateQueue(ctx, customer, "DEV", UpdateQueueInput{Owners: &owners, Revision: queue.Revision}); err != nil {
		t.Fatal(err)
	}
	complete(t, svc, queueRun.ID, RunCompletion{Verdict: "pass"})
	runs := listRuns(t, svc, customer, task)
	agentRun = runs[0]
	if agentRun.ID == queueRun.ID || agentRun.RunAs != "agent" {
		t.Fatalf("second run = %#v", agentRun)
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
	return svc, customer, task, queueRun, agentRun
}

func TestRunsRecordTheirHolder(t *testing.T) {
	svc, customer, task, queueRun, agentRun := accessFixture(t)
	if queueRun.Holder != "" || agentRun.Holder != "reviewer-1" {
		t.Fatalf("holders = %q, %q", queueRun.Holder, agentRun.Holder)
	}
	// The holder is fixed when the run is created: a later assignee change
	// does not move it, and the worker's job takes it from the row.
	if _, err := svc.db.Exec(`UPDATE tasks SET assignee = 'agent:dev-2' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err := svc.PendingRunJobs(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].Run.ID != agentRun.ID || jobs[0].Holder != "reviewer-1" {
		t.Fatalf("jobs = %#v, %v", jobs, err)
	}
	got, err := svc.GetScriptRun(context.Background(), customer, task.Key, agentRun.ID)
	if err != nil || got.Holder != "reviewer-1" {
		t.Fatalf("GetScriptRun = %#v, %v", got, err)
	}
}

func TestRunLogAccess(t *testing.T) {
	svc, customer, task, queueRun, agentRun := accessFixture(t)
	ctx := context.Background()
	read := func(actor Actor, run ScriptRun) error {
		_, _, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 0)
		return err
	}
	dev1, dev2, reviewer := AgentActor("dev-1"), AgentActor("dev-2"), AgentActor("reviewer-1")

	// The customer reads every log.
	for _, run := range []ScriptRun{queueRun, agentRun} {
		if err := read(customer, run); err != nil {
			t.Errorf("customer, run %d: %v", run.ID, err)
		}
	}
	// A queue owner that holds nothing is refused both, yet may list the runs.
	for _, run := range []ScriptRun{queueRun, agentRun} {
		if err := read(dev2, run); ErrorCode(err) != "forbidden" || ErrorStatus(err) != 403 {
			t.Errorf("non-holder, run %d: %v", run.ID, err)
		}
	}
	if _, err := svc.ListScriptRuns(ctx, dev2, task.Key); err != nil {
		t.Errorf("non-holder list: %v", err)
	}
	if _, err := svc.GetScriptRun(ctx, dev2, task.Key, agentRun.ID); err != nil {
		t.Errorf("non-holder get: %v", err)
	}
	// The holder of another pool reads the queue-mode log, not the agent one.
	if err := read(dev1, queueRun); err != nil {
		t.Errorf("other pool's holder, queue log: %v", err)
	}
	if err := read(dev1, agentRun); ErrorCode(err) != "forbidden" {
		t.Errorf("other pool's holder, agent log: %v", err)
	}
	// The agent the run ran as reads its log.
	if err := read(reviewer, agentRun); err != nil {
		t.Errorf("run's agent: %v", err)
	}
	// A stranger still cannot see the task.
	if err := read(AgentActor("stranger"), queueRun); ErrorCode(err) != "not_found" {
		t.Errorf("stranger: %v", err)
	}
	// After the task was released to another agent, the recorded agent keeps
	// its log, and the new assignee, a holder now, reads queue-mode logs only.
	if _, err := svc.db.Exec(`UPDATE tasks SET assignee = 'agent:dev-2' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE task_workflow_holders SET agent = 'dev-2' WHERE task_id = ? AND pool = 'reviewers'`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := read(reviewer, agentRun); err != nil {
		t.Errorf("released agent, its own log: %v", err)
	}
	if err := read(dev2, queueRun); err != nil {
		t.Errorf("new holder, queue log: %v", err)
	}
	if err := read(dev2, agentRun); ErrorCode(err) != "forbidden" {
		t.Errorf("new holder, previous agent's log: %v", err)
	}
}

func TestScriptRunLogRefusesWithoutABaseDirectory(t *testing.T) {
	svc, customer, task, _ := runFixture(t)
	_, run := advanceApprove(t, svc, customer, task)
	setLogPath(t, svc, run.ID, "/base/tasks/"+task.Key+"/runs/"+strconv.FormatInt(run.ID, 10)+"/run.log")
	if _, _, err := svc.ScriptRunLog(context.Background(), customer, task.Key, run.ID, 0); ErrorStatus(err) != 409 {
		t.Fatalf("no base directory: %v", err)
	}
}

func TestScriptRunLogRefusesAFifoWithoutBlocking(t *testing.T) {
	svc, customer, task, run, path := logFixture(t)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := svc.ScriptRunLog(context.Background(), customer, task.Key, run.ID, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if ErrorStatus(err) != 409 {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO log blocked")
	}
}

func TestScriptRunLogMatchesTheWorkersPathForAnyBaseSpelling(t *testing.T) {
	svc, customer, task, run, path := logFixture(t)
	if err := os.WriteFile(path, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The worker stores filepath.Abs(BaseDir)/tasks/...; Abs cleans a trailing
	// slash or a "." segment, and so does the reader.
	for _, spelling := range []string{svc.runBaseDir + "/", svc.runBaseDir + "/./"} {
		svc.SetRunBaseDir(spelling)
		if text, _, err := svc.ScriptRunLog(context.Background(), customer, task.Key, run.ID, 0); err != nil || text != "ok\n" {
			t.Errorf("base %q: %q, %v", spelling, text, err)
		}
	}
}

func TestScriptRunLogMaxBytesIsValidated(t *testing.T) {
	svc, customer, task, run, path := logFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(path, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ScriptRunLog(ctx, customer, task.Key, run.ID, -1); ErrorStatus(err) != 400 || ErrorCode(err) != "invalid_request" {
		t.Fatalf("negative: %v", err)
	}
	for _, value := range []any{float64(-1), "abc", "-5", 1.5, true} {
		if _, err := ParseMaxBytes(value); ErrorStatus(err) != 400 {
			t.Errorf("ParseMaxBytes(%#v): %v", value, err)
		}
	}
	for value, want := range map[any]int{nil: 0, "": 0, "0": 0, "12": 12, float64(7): 7, 3: 3} {
		if got, err := ParseMaxBytes(value); err != nil || got != want {
			t.Errorf("ParseMaxBytes(%#v) = %d, %v", value, got, err)
		}
	}
	for _, bad := range []any{float64(-1), "abc"} {
		_, err := svc.AgentAction(ctx, AgentActor("reviewer-1"), "workflow_run_log", map[string]any{"key": task.Key, "id": float64(run.ID), "max_bytes": bad})
		if ErrorStatus(err) != 400 {
			t.Errorf("action max_bytes %#v: %v", bad, err)
		}
	}
}
