package workflowrun

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

const supervisorManifest = `schema_version: 1
name: demo
workflow_version: 1.0.0
initial_status: work
statuses:
  - id: work
    owner: { pool: devs }
    instructions: ./statuses/work.md
    transitions:
      - on: done
        to: finished
        checks:
          - script: ./scripts/pass.sh
  - id: finished
    terminal: true
sources:
  - name: pull-requests
    script: ./scripts/items.sh
    every: 10s
`

// Stub scripts published in the test image. MARK_DIR comes from the baseline
// environment, so a script that ran leaves a file the test can see.
var supervisorScripts = map[string]string{
	"scripts/pass.sh":     "touch \"$MARK_DIR/pass-$TARIBOY_TASK_KEY\"\nexit 0\n",
	"scripts/env.sh":      "pwd > \"$TARIBOY_TASK_DIR/pwd.txt\"\nenv > \"$TARIBOY_TASK_DIR/env.txt\"\nexit 0\n",
	"scripts/sleep.sh":    "sleep 0.4\nexit 0\n",
	"scripts/stubborn.sh": "trap '' TERM\ntouch \"$MARK_DIR/stubborn-started\"\nsleep 30\n",
	"scripts/quiet.sh":    "exit 111\n",
	"scripts/items.sh": "pwd > \"$TARIBOY_SOURCE_DIR/pwd.txt\"\nenv > \"$TARIBOY_SOURCE_DIR/env.txt\"\n" +
		`printf '{"items":[{"key":"k-%s","title":"t %s","description":"%s"}]}' ` +
		`"$TARIBOY_SOURCE_NAME" "$LONG_SECRET" "$TARIBOY_TASK_QUEUE" > "$TARIBOY_RESULT_FILE"` + "\nexit 0\n",
	"scripts/leak.sh": `printf '{"message":"token %s and %s","artifacts":{"note":"v-%s-%s"}}' ` +
		`"$LONG_SECRET" "$SHORT_SECRET" "$LONG_SECRET" "$SHORT_SECRET" > "$TARIBOY_RESULT_FILE"` + "\nexit 0\n",
}

// image publishes the stub workflow into a fresh store.
func supervisorImage(t *testing.T) (*workflowimage.Store, workflowimage.Manifest) {
	t.Helper()
	src := t.TempDir()
	write := func(rel, content string, mode fs.FileMode) {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("Workflowfile.yaml", supervisorManifest, 0o644)
	write("statuses/work.md", "work\n", 0o644)
	for rel, body := range supervisorScripts {
		write(rel, "#!/bin/sh\n"+body, 0o755)
	}
	f, err := workflowfile.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "workflows")
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	store := &workflowimage.Store{Dir: dir}
	m, _, err := store.Publish(f, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return store, m
}

// fakeJobs is an in-memory Jobs. A pending job stays pending until claimed.
type fakeJobs struct {
	mu          sync.Mutex
	jobs        []tasks.RunJob
	claimed     map[int64]string // id -> log path
	refuse      map[int64]bool   // ClaimScriptRun returns false
	pids        map[int64]int
	completions map[int64]tasks.RunCompletion
	cancel      map[int64]bool
	active      map[string]int // running per task, between PID and completion
	maxTask     int            // most runs of one task at once
	maxAll      int            // most runs at once
	activeAll   int
	completed   chan int64
	started     chan int64

	completeFail     map[int64]int           // CompleteScriptRun fails this many more times
	completeNotFound map[int64]bool          // CompleteScriptRun answers run_not_found
	completeBlock    map[int64]chan struct{} // CompleteScriptRun waits for the channel
	attempts         map[int64]int           // CompleteScriptRun calls per run
	pendingCalls     int                     // PendingRunJobs calls
	running          []tasks.ScriptRun       // RunningScriptRuns rows
	recoverFail      int                     // RecoverScriptRuns fails this many more times
	recovered        bool                    // RecoverScriptRuns succeeded
	claimedEarly     bool                    // a run was claimed before recovery succeeded
	activeWatches    int
	maxWatches       int
}

func newFakeJobs(jobs ...tasks.RunJob) *fakeJobs {
	return &fakeJobs{
		jobs: jobs, claimed: map[int64]string{}, refuse: map[int64]bool{}, pids: map[int64]int{},
		completions: map[int64]tasks.RunCompletion{}, cancel: map[int64]bool{}, active: map[string]int{},
		completed: make(chan int64, 64), started: make(chan int64, 64),
		completeFail: map[int64]int{}, completeNotFound: map[int64]bool{}, completeBlock: map[int64]chan struct{}{},
		attempts: map[int64]int{},
	}
}

func (f *fakeJobs) kindOf(id int64) string {
	for _, j := range f.jobs {
		if j.Run.ID == id {
			return j.Run.Kind
		}
	}
	return ""
}

func (f *fakeJobs) RunningScriptRuns(context.Context) ([]tasks.ScriptRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tasks.ScriptRun(nil), f.running...), nil
}

func (f *fakeJobs) RecoverScriptRuns(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recoverFail > 0 {
		f.recoverFail--
		return errors.New("recovery failed")
	}
	f.recovered = true
	return nil
}

func (f *fakeJobs) taskOf(id int64) string {
	for _, j := range f.jobs {
		if j.Run.ID == id {
			return j.Run.TaskKey
		}
	}
	return ""
}

func (f *fakeJobs) PendingRunJobs(context.Context) ([]tasks.RunJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingCalls++
	var out []tasks.RunJob
	for _, j := range f.jobs {
		if _, ok := f.claimed[j.Run.ID]; !ok {
			out = append(out, j)
		}
	}
	return out, nil
}

func (f *fakeJobs) ClaimScriptRun(_ context.Context, id int64, startedAt, logPath string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse[id] {
		return false, nil
	}
	if _, err := time.Parse(time.RFC3339Nano, startedAt); err != nil {
		return false, err
	}
	if _, ok := f.claimed[id]; ok {
		return false, nil
	}
	if !f.recovered {
		f.claimedEarly = true
	}
	f.claimed[id] = logPath
	return true, nil
}

func (f *fakeJobs) SetScriptRunPID(_ context.Context, id int64, pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pids[id] = pid
	task := f.taskOf(id)
	f.active[task]++
	f.activeAll++
	f.maxTask = max(f.maxTask, f.active[task])
	f.maxAll = max(f.maxAll, f.activeAll)
	if f.kindOf(id) == KindWatch {
		f.activeWatches++
		f.maxWatches = max(f.maxWatches, f.activeWatches)
	}
	f.started <- id
	return nil
}

func (f *fakeJobs) CompleteScriptRun(_ context.Context, id int64, done tasks.RunCompletion) error {
	f.mu.Lock()
	block := f.completeBlock[id]
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[id]++
	if f.completeNotFound[id] {
		return &tasks.Error{Status: 404, Code: "run_not_found", Msg: "script run not found"}
	}
	if f.completeFail[id] > 0 {
		f.completeFail[id]--
		return errors.New("database is locked")
	}
	if _, ok := f.pids[id]; ok {
		f.active[f.taskOf(id)]--
		f.activeAll--
		if f.kindOf(id) == KindWatch {
			f.activeWatches--
		}
	}
	f.completions[id] = done
	delete(f.cancel, id)
	f.completed <- id
	return nil
}

func (f *fakeJobs) ScheduleDueWatches(context.Context, time.Time) (int, error) { return 0, nil }

func (f *fakeJobs) CancelRequestedRuns(context.Context) ([]tasks.ScriptRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tasks.ScriptRun
	for id := range f.cancel {
		out = append(out, tasks.ScriptRun{ID: id, TaskKey: f.taskOf(id), State: "running"})
	}
	return out, nil
}

func (f *fakeJobs) claimedPath(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claimed[id]
}

func (f *fakeJobs) completion(id int64) (tasks.RunCompletion, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.completions[id]
	return c, ok
}

// harness is one supervisor under test.
type harness struct {
	t       *testing.T
	jobs    *fakeJobs
	sup     *Supervisor
	base    string
	marks   string
	image   workflowimage.Manifest
	wake    chan struct{}
	cancel  context.CancelFunc
	stopped chan struct{}
}

func newHarness(t *testing.T, jobs ...tasks.RunJob) *harness {
	t.Helper()
	store, m := supervisorImage(t)
	h := &harness{t: t, base: t.TempDir(), marks: t.TempDir(), image: m, wake: make(chan struct{}, 1)}
	for i := range jobs {
		if jobs[i].WorkflowName == "" {
			jobs[i].WorkflowName, jobs[i].WorkflowVersion, jobs[i].WorkflowDigest = m.Name, m.Version, m.Digest
		}
	}
	h.jobs = newFakeJobs(jobs...)
	h.sup = &Supervisor{
		Jobs: h.jobs, Images: store, BaseDir: h.base,
		BaseEnv: func() []string {
			return []string{"PATH=/usr/bin:/bin", "MARK_DIR=" + h.marks, "BASE_ONLY=base", "SHARED=base",
				"TARIBOY_TOOLS_SOCKET=/run/tools.sock", "TARIBOY_DAEMON_SOCKET=/run/daemon.sock",
				"TARIBOY_PLUGIN_SOCKET=/run/plugin.sock", "TARIBOY_PLUGIN_TOKEN=plugintoken"}
		},
		AgentRuntime: func(string) (string, []string, error) {
			return "", nil, errors.New("no agent runtime in this test")
		},
		Wake: h.wake, Interval: 20 * time.Millisecond, Parallel: 4,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return h
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.stopped = cancel, make(chan struct{})
	go func() {
		defer close(h.stopped)
		h.sup.Run(ctx)
	}()
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.stopped:
	case <-time.After(10 * time.Second):
		h.t.Fatal("supervisor did not stop")
	}
}

func (h *harness) nudge() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// awaitCompletion waits for the completion of run id.
func (h *harness) awaitCompletion(id int64, within time.Duration) tasks.RunCompletion {
	h.t.Helper()
	deadline := time.After(within)
	for {
		if c, ok := h.jobs.completion(id); ok {
			return c
		}
		select {
		case <-h.jobs.completed:
		case <-deadline:
			h.t.Fatalf("run %d was not completed within %v", id, within)
		}
	}
}

// awaitRecovered waits for RecoverScriptRuns to succeed.
func (h *harness) awaitRecovered() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.jobs.mu.Lock()
		recovered := h.jobs.recovered
		h.jobs.mu.Unlock()
		if recovered {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatal("recovery did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) awaitStart(id int64) {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		h.jobs.mu.Lock()
		_, ok := h.jobs.pids[id]
		h.jobs.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-h.jobs.started:
		case <-deadline:
			h.t.Fatalf("run %d did not start", id)
		}
	}
}

func checkJob(id int64, key, script string) tasks.RunJob {
	return tasks.RunJob{
		Run:    tasks.ScriptRun{ID: id, TaskKey: key, Kind: KindCheck, Script: script, RunAs: workflowfile.RunAsQueue, State: "pending"},
		Queue:  "DEV",
		Status: "work", Outcome: "done", Timeout: 10 * time.Second,
		WorkflowEnv: map[string]string{}, QueueSecrets: map[string]string{},
		Snapshot: []byte(`{"key":"` + key + `"}`), Outcomes: []string{"done"}, Artifacts: []string{},
	}
}

// envFile reads the environment a stub script dumped, failing on a duplicate.
func envFile(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if _, dup := out[name]; dup {
			t.Fatalf("variable %s appears twice in the run environment", name)
		}
		out[name] = value
	}
	return out
}

func noToolsSocket(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range script.DaemonAccessEnv {
		if _, ok := env[name]; ok {
			t.Fatalf("%s reached the script", name)
		}
	}
	if _, ok := env["TARIBOY_TOOLS_SOCKET"]; ok {
		t.Fatal("TARIBOY_TOOLS_SOCKET reached the script")
	}
}

func TestSupervisorRunsACheckAndCompletesIt(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/pass.sh"))
	h.start()
	done := h.awaitCompletion(1, 5*time.Second)
	if done.Verdict != VerdictPass || done.ExitCode == nil || *done.ExitCode != 0 {
		t.Fatalf("completion = %+v, want a pass with exit 0", done)
	}
	want := filepath.Join(h.base, "tasks", "DEV-1", "runs", "1", "run.log")
	if done.LogPath != want || h.jobs.claimedPath(1) != want {
		t.Fatalf("log path completed %q claimed %q, want %q", done.LogPath, h.jobs.claimedPath(1), want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339Nano, done.FinishedAt); err != nil {
		t.Fatalf("finished at %q: %v", done.FinishedAt, err)
	}
	if _, err := os.Stat(filepath.Join(h.marks, "pass-DEV-1")); err != nil {
		t.Fatalf("the script did not run: %v", err)
	}
	for _, dir := range []string{"tasks/DEV-1/state", "tasks/DEV-1/runs/1"} {
		info, err := os.Stat(filepath.Join(h.base, dir))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v, want mode 0700", dir, info, err)
		}
	}
}

func TestSupervisorQueueModeEnvironmentAndCwd(t *testing.T) {
	job := checkJob(1, "DEV-1", "scripts/env.sh")
	job.WorkflowEnv = map[string]string{"SHARED": "workflow", "WF_ONLY": "wf", "TARIBOY_TASK_KEY": "hijack",
		"TARIBOY_TOOLS_SOCKET": "/wf/tools.sock"}
	job.QueueSecrets = map[string]string{"SHARED": "queue-secret", "Q_SECRET": "qs", "TARIBOY_WORKFLOW_STATUS": "hijack"}
	h := newHarness(t, job)
	h.sup.AgentRuntime = func(string) (string, []string, error) {
		t.Error("a queue run asked for an agent runtime")
		return "", nil, errors.New("unexpected")
	}
	h.start()
	if done := h.awaitCompletion(1, 5*time.Second); done.Verdict != VerdictPass {
		t.Fatalf("completion = %+v", done)
	}
	state := filepath.Join(h.base, "tasks", "DEV-1", "state")
	pwd, _ := os.ReadFile(filepath.Join(state, "pwd.txt"))
	if strings.TrimSpace(string(pwd)) != state {
		t.Fatalf("cwd = %q, want the task state directory %q", pwd, state)
	}
	env := envFile(t, filepath.Join(state, "env.txt"))
	for name, want := range map[string]string{
		"BASE_ONLY": "base", "WF_ONLY": "wf", "Q_SECRET": "qs", "SHARED": "queue-secret",
		"TARIBOY_TASK_KEY": "DEV-1", "TARIBOY_TASK_QUEUE": "DEV", "TARIBOY_WORKFLOW_STATUS": "work",
		"TARIBOY_WORKFLOW_OUTCOME": "done", "TARIBOY_TASK_DIR": state,
		"TARIBOY_WORKFLOW_DIR": h.sup.Images.ContentDir(h.image.Name, h.image.Digest),
		"TARIBOY_RESULT_FILE":  filepath.Join(h.base, "tasks", "DEV-1", "runs", "1", "result.json"),
		"TARIBOY_QUIET_EXIT":   "111", "TARIBOY_REJECT_EXIT": "112",
	} {
		if env[name] != want {
			t.Fatalf("%s = %q, want %q", name, env[name], want)
		}
	}
	noToolsSocket(t, env)
}

func TestSupervisorAgentModeEnvironmentAndCwd(t *testing.T) {
	job := checkJob(1, "DEV-1", "scripts/env.sh")
	job.Run.RunAs = workflowfile.RunAsAgent
	job.Holder = "worker"
	job.WorkflowEnv = map[string]string{"WF_ONLY": "wf", "AGENT_WF": "workflow", "TARIBOY_TASK_KEY": "hijack"}
	job.QueueSecrets = map[string]string{"TOKEN": "queue-token"}
	h := newHarness(t, job)
	agentCwd := t.TempDir()
	var asked string
	h.sup.AgentRuntime = func(agent string) (string, []string, error) {
		asked = agent
		return agentCwd, []string{"PATH=/agent/bin:/usr/bin:/bin", "MARK_DIR=" + h.marks, "TOKEN=agent-token",
			"AGENT_ONLY=a", "AGENT_WF=agent", "TARIBOY_TOOLS_SOCKET=/agent/tools.sock"}, nil
	}
	h.start()
	if done := h.awaitCompletion(1, 5*time.Second); done.Verdict != VerdictPass {
		t.Fatalf("completion = %+v", done)
	}
	if asked != "worker" {
		t.Fatalf("agent runtime asked for %q, want worker", asked)
	}
	state := filepath.Join(h.base, "tasks", "DEV-1", "state")
	pwd, _ := os.ReadFile(filepath.Join(state, "pwd.txt"))
	if strings.TrimSpace(string(pwd)) != agentCwd {
		t.Fatalf("cwd = %q, want the agent cwd %q", pwd, agentCwd)
	}
	env := envFile(t, filepath.Join(state, "env.txt"))
	for name, want := range map[string]string{
		"PATH": "/agent/bin:/usr/bin:/bin", "AGENT_ONLY": "a", "AGENT_WF": "workflow", "WF_ONLY": "wf",
		"TOKEN": "queue-token", "TARIBOY_TASK_KEY": "DEV-1", "TARIBOY_TASK_DIR": state,
	} {
		if env[name] != want {
			t.Fatalf("%s = %q, want %q", name, env[name], want)
		}
	}
	if _, ok := env["BASE_ONLY"]; ok {
		t.Fatal("agent mode took the daemon baseline a second time instead of the agent runtime")
	}
	noToolsSocket(t, env)
}

func TestSupervisorWatchRunIsAlwaysQueueMode(t *testing.T) {
	job := checkJob(1, "DEV-1", "scripts/quiet.sh")
	job.Run.Kind, job.Run.RunAs, job.Holder, job.Outcome = KindWatch, workflowfile.RunAsAgent, "worker", ""
	h := newHarness(t, job)
	h.sup.AgentRuntime = func(string) (string, []string, error) {
		t.Error("a watch run asked for an agent runtime")
		return "", nil, errors.New("unexpected")
	}
	h.start()
	if done := h.awaitCompletion(1, 5*time.Second); done.Verdict != VerdictQuiet {
		t.Fatalf("completion = %+v, want quiet", done)
	}
}

func TestSupervisorPreExecutionFailures(t *testing.T) {
	const secret = "s3cr3t-value-never-shown"
	unknownHolder := checkJob(1, "DEV-1", "scripts/pass.sh")
	unknownHolder.Run.RunAs = workflowfile.RunAsAgent
	runtimeErr := checkJob(2, "DEV-2", "scripts/pass.sh")
	runtimeErr.Run.RunAs, runtimeErr.Holder = workflowfile.RunAsAgent, "broken"
	escape := checkJob(3, "DEV-3", "../../../../bin/true")
	missing := checkJob(4, "DEV-4", "scripts/pass.sh")
	missing.WorkflowName, missing.WorkflowDigest = "demo", strings.Repeat("ab", 32)
	absent := checkJob(5, "DEV-5", "scripts/absent.sh")
	jobs := []tasks.RunJob{unknownHolder, runtimeErr, escape, missing, absent}
	for i := range jobs {
		jobs[i].QueueSecrets = map[string]string{"TOKEN": secret}
		jobs[i].WorkflowEnv = map[string]string{"WF": secret}
	}
	h := newHarness(t, jobs...)
	h.sup.AgentRuntime = func(agent string) (string, []string, error) {
		return "", nil, errors.New("agent " + agent + " is not available")
	}
	h.start()
	for _, tc := range []struct {
		id   int64
		want string
	}{{1, "holder"}, {2, "broken"}, {3, "../../../../bin/true"}, {4, "scripts/pass.sh"}, {5, "scripts/absent.sh"}} {
		done := h.awaitCompletion(tc.id, 5*time.Second)
		if done.Verdict != VerdictFailure || !strings.Contains(done.Message, tc.want) {
			t.Fatalf("run %d completion = %+v, want a failure naming %q", tc.id, done, tc.want)
		}
		if strings.Contains(done.Message, secret) {
			t.Fatalf("run %d failure message contains a secret value: %q", tc.id, done.Message)
		}
		h.jobs.mu.Lock()
		_, claimed := h.jobs.claimed[tc.id]
		_, ran := h.jobs.pids[tc.id]
		h.jobs.mu.Unlock()
		if !claimed {
			t.Fatalf("run %d was completed without a claim", tc.id)
		}
		if ran {
			t.Fatalf("run %d started a process", tc.id)
		}
	}
	if _, err := os.Stat(filepath.Join(h.marks, "pass-DEV-1")); !os.IsNotExist(err) {
		t.Fatalf("a failed run executed its script: %v", err)
	}
}

func TestSupervisorRunsDifferentTasksConcurrentlyUpToParallel(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/sleep.sh"), checkJob(2, "DEV-2", "scripts/sleep.sh"),
		checkJob(3, "DEV-3", "scripts/sleep.sh"))
	h.sup.Parallel = 2
	h.start()
	for id := int64(1); id <= 3; id++ {
		if done := h.awaitCompletion(id, 10*time.Second); done.Verdict != VerdictPass {
			t.Fatalf("run %d completion = %+v", id, done)
		}
	}
	h.jobs.mu.Lock()
	defer h.jobs.mu.Unlock()
	if h.jobs.maxAll != 2 {
		t.Fatalf("at most %d runs at once, want exactly Parallel=2", h.jobs.maxAll)
	}
}

func TestSupervisorNeverOverlapsRunsOfOneTask(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/sleep.sh"), checkJob(2, "DEV-1", "scripts/sleep.sh"))
	h.start()
	h.awaitCompletion(1, 10*time.Second)
	h.awaitCompletion(2, 10*time.Second)
	h.jobs.mu.Lock()
	defer h.jobs.mu.Unlock()
	if h.jobs.maxTask != 1 {
		t.Fatalf("%d runs of one task at once, want 1", h.jobs.maxTask)
	}
}

func TestSupervisorClaimRefusedRunsNothing(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/pass.sh"), checkJob(2, "DEV-2", "scripts/pass.sh"))
	h.jobs.refuse[1] = true
	h.start()
	h.awaitCompletion(2, 5*time.Second)
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(h.marks, "pass-DEV-1")); !os.IsNotExist(err) {
		t.Fatalf("a refused claim executed its script: %v", err)
	}
	if _, ok := h.jobs.completion(1); ok {
		t.Fatal("a refused claim was completed")
	}
	if _, err := os.Stat(filepath.Join(h.base, "tasks", "DEV-1", "runs", "1", "run.log")); !os.IsNotExist(err) {
		t.Fatalf("a refused claim created a run log: %v", err)
	}
}

func TestSupervisorCancelRequestKillsAStubbornScript(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/stubborn.sh"))
	h.start()
	h.awaitStart(1)
	waitFile(t, filepath.Join(h.marks, "stubborn-started"))
	h.jobs.mu.Lock()
	pid := h.jobs.pids[1]
	h.jobs.cancel[1] = true
	h.jobs.mu.Unlock()
	asked := time.Now()
	h.nudge()
	done := h.awaitCompletion(1, killGrace+5*time.Second)
	if elapsed := time.Since(asked); elapsed > killGrace+3*time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
	if done.Verdict != VerdictFailure {
		t.Fatalf("completion = %+v, want a failure recorded for the cancelled run", done)
	}
	groupGone(t, pid)
}

func TestSupervisorShutdownLeavesRunsUncompleted(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/stubborn.sh"))
	h.start()
	h.awaitStart(1)
	waitFile(t, filepath.Join(h.marks, "stubborn-started"))
	h.jobs.mu.Lock()
	pid := h.jobs.pids[1]
	h.jobs.mu.Unlock()
	h.stop()
	if _, ok := h.jobs.completion(1); ok {
		t.Fatal("a run killed by shutdown was completed")
	}
	groupGone(t, pid)
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", path)
}

func groupGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process group %d is still alive", pid)
}

func TestSupervisorRedactsQueueSecretsFromWhatAScriptReturns(t *testing.T) {
	job := checkJob(1, "DEV-1", "scripts/leak.sh")
	job.Artifacts = []string{"note"}
	job.QueueSecrets = map[string]string{"LONG_SECRET": "queue-secret-123", "SHORT_SECRET": "abc"}
	h := newHarness(t, job)
	h.start()
	done := h.awaitCompletion(1, 5*time.Second)
	if done.Verdict != VerdictPass {
		t.Fatalf("completion = %+v, want a pass", done)
	}
	if done.Message != "token [redacted] and abc" {
		t.Fatalf("message = %q, want the long secret redacted and the short one kept", done.Message)
	}
	if got := done.Artifacts["note"]; got != "v-[redacted]-abc" {
		t.Fatalf("artifact note = %q, want the long secret redacted and the short one kept", got)
	}
}

func watchJob(id int64, key, script string) tasks.RunJob {
	job := checkJob(id, key, script)
	job.Run.Kind, job.Outcome = KindWatch, ""
	return job
}

func TestSupervisorRetriesAFailedCompletionAndKeepsTheTaskBusy(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/pass.sh"), checkJob(2, "DEV-1", "scripts/pass.sh"))
	h.jobs.completeFail[1] = 3
	h.start()
	if done := h.awaitCompletion(1, 5*time.Second); done.Verdict != VerdictPass {
		t.Fatalf("completion = %+v", done)
	}
	h.awaitCompletion(2, 5*time.Second)
	h.jobs.mu.Lock()
	defer h.jobs.mu.Unlock()
	if h.jobs.attempts[1] != 4 {
		t.Fatalf("completion attempts = %d, want 4", h.jobs.attempts[1])
	}
	// Run 2 of the same task was claimed only after run 1 was recorded.
	if h.jobs.attempts[2] != 1 || h.jobs.maxTask != 1 {
		t.Fatalf("attempts %v, max runs of one task %d", h.jobs.attempts, h.jobs.maxTask)
	}
}

func TestSupervisorKeepsATaskBusyWhileItsCompletionFails(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/pass.sh"), checkJob(2, "DEV-1", "scripts/pass.sh"))
	h.jobs.completeFail[1] = 1 << 30
	h.start()
	waitFile(t, filepath.Join(h.marks, "pass-DEV-1"))
	time.Sleep(300 * time.Millisecond)
	h.jobs.mu.Lock()
	attempts := h.jobs.attempts[1]
	_, claimedTwo := h.jobs.claimed[2]
	h.jobs.mu.Unlock()
	// The completion is retried on every pass, and the task stays busy.
	if attempts < 3 || claimedTwo {
		t.Fatalf("attempts %d, second run claimed %v", attempts, claimedTwo)
	}
}

func TestSupervisorCountsRunNotFoundAsCompleted(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/pass.sh"), checkJob(2, "DEV-1", "scripts/pass.sh"))
	h.jobs.completeNotFound[1] = true
	h.start()
	h.awaitCompletion(2, 5*time.Second)
	h.jobs.mu.Lock()
	defer h.jobs.mu.Unlock()
	if h.jobs.attempts[1] != 1 {
		t.Fatalf("attempts = %d; run_not_found is final", h.jobs.attempts[1])
	}
}

func TestSupervisorCompletesAPreStartFailureInTheBackground(t *testing.T) {
	broken := checkJob(1, "DEV-1", "scripts/absent.sh")
	h := newHarness(t, broken, checkJob(2, "DEV-2", "scripts/pass.sh"))
	release := make(chan struct{})
	h.jobs.completeBlock[1] = release
	h.start()
	// The pass is not held by the blocked completion of run 1.
	h.awaitCompletion(2, 5*time.Second)
	if path := h.jobs.claimedPath(1); path != "" {
		t.Fatalf("pre-start failure claimed with log path %q", path)
	}
	close(release)
	if done := h.awaitCompletion(1, 5*time.Second); done.Verdict != VerdictFailure {
		t.Fatalf("completion = %+v", done)
	}
}

func TestSupervisorRecoversBeforeStartingAnyRun(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/pass.sh"))
	h.jobs.recoverFail = 2
	h.start()
	h.awaitCompletion(1, 5*time.Second)
	h.jobs.mu.Lock()
	defer h.jobs.mu.Unlock()
	if !h.jobs.recovered || h.jobs.claimedEarly {
		t.Fatalf("recovered %v, claimed before recovery %v", h.jobs.recovered, h.jobs.claimedEarly)
	}
}

// orphan starts a real child in its own process group, as a script the
// previous daemon left behind, with env added to its environment.
func orphan(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	})
	return cmd
}

func TestSupervisorTerminatesOnlyProvenOrphans(t *testing.T) {
	h := newHarness(t)
	resultFile := filepath.Join(h.base, "tasks", "DEV-7", "runs", "7", "result.json")
	ours := orphan(t, "TARIBOY_RESULT_FILE="+resultFile)
	stranger := orphan(t, "TARIBOY_RESULT_FILE="+filepath.Join(h.base, "tasks", "DEV-8", "runs", "9", "result.json"))
	oursPID, strangerPID := ours.Process.Pid, stranger.Process.Pid
	h.jobs.running = []tasks.ScriptRun{
		{ID: 7, TaskKey: "DEV-7", Kind: KindCheck, State: "running", PID: &oursPID},
		{ID: 8, TaskKey: "DEV-8", Kind: KindWatch, State: "running", PID: &strangerPID},
	}
	h.start()
	groupGone(t, oursPID)
	h.awaitRecovered()
	if err := syscall.Kill(strangerPID, 0); err != nil {
		t.Fatalf("a process that is not the run's script was signalled: %v", err)
	}
}

func TestSupervisorSignalsNothingWithoutProc(t *testing.T) {
	h := newHarness(t)
	var logged strings.Builder
	var logMu sync.Mutex
	h.sup.Log = slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logged.Write(p)
	}), nil))
	procRoot = filepath.Join(t.TempDir(), "no-proc")
	t.Cleanup(func() { procRoot = "/proc" })
	resultFile := filepath.Join(h.base, "tasks", "DEV-7", "runs", "7", "result.json")
	ours := orphan(t, "TARIBOY_RESULT_FILE="+resultFile)
	pid := ours.Process.Pid
	h.jobs.running = []tasks.ScriptRun{{ID: 7, TaskKey: "DEV-7", Kind: KindCheck, State: "running", PID: &pid}}
	h.start()
	h.awaitRecovered()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the orphan was signalled without proof: %v", err)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(logged.String(), "run_id=7") || !strings.Contains(logged.String(), "pid="+strconv.Itoa(pid)) {
		t.Fatalf("log = %q", logged.String())
	}
}

func TestRecordUnrecordedStopsAtTheFirstFailureOfAPass(t *testing.T) {
	h := newHarness(t)
	w := &worker{s: h.sup, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		busy: map[string]bool{}, retry: map[int64]unrecorded{}, quietDirs: map[string][]string{}}
	for id := int64(1); id <= 3; id++ {
		h.jobs.completeFail[id] = 1
		w.retry[id] = unrecorded{job: checkJob(id, "DEV-"+strconv.FormatInt(id, 10), "scripts/pass.sh"),
			done: tasks.RunCompletion{Verdict: VerdictPass}}
	}
	attempts := func() int {
		h.jobs.mu.Lock()
		defer h.jobs.mu.Unlock()
		total := 0
		for _, n := range h.jobs.attempts {
			total += n
		}
		return total
	}
	w.recordUnrecorded(context.Background())
	if n := attempts(); n != 1 {
		t.Fatalf("attempts in a failing pass = %d; the pass stops at the first failure", n)
	}
	// Each later pass records what it can and stops at the next failure.
	for pass := 0; pass < 6 && len(w.retry) > 0; pass++ {
		w.recordUnrecorded(context.Background())
	}
	if len(w.retry) != 0 {
		t.Fatalf("kept completions left = %d", len(w.retry))
	}
}

func TestProtectedOrphanPIDs(t *testing.T) {
	sameGroup := exec.Command("sleep", "30")
	if err := sameGroup.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sameGroup.Process.Kill()
		_ = sameGroup.Wait()
	})
	own := orphan(t)
	for pid, want := range map[int]bool{
		1: true, os.Getpid(): true, syscall.Getpgrp(): true, sameGroup.Process.Pid: true,
		own.Process.Pid: false,
	} {
		if got := protectedOrphanPID(pid); got != want {
			t.Errorf("protectedOrphanPID(%d) = %v, want %v", pid, got, want)
		}
	}
}

func TestSupervisorNeverSignalsInitOrItself(t *testing.T) {
	h := newHarness(t)
	var logged strings.Builder
	var logMu sync.Mutex
	h.sup.Log = slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logged.Write(p)
	}), nil))
	// Without /proc the identity check would only log; the guard comes first.
	procRoot = filepath.Join(t.TempDir(), "no-proc")
	t.Cleanup(func() { procRoot = "/proc" })
	initPID, self := 1, os.Getpid()
	h.jobs.running = []tasks.ScriptRun{
		{ID: 7, TaskKey: "DEV-7", Kind: KindCheck, State: "running", PID: &initPID},
		{ID: 8, TaskKey: "DEV-8", Kind: KindWatch, State: "running", PID: &self},
	}
	h.start()
	h.awaitRecovered()
	logMu.Lock()
	defer logMu.Unlock()
	for _, want := range []string{"run_id=7", "run_id=8", "never signals"} {
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("log lacks %q: %q", want, logged.String())
		}
	}
	if strings.Contains(logged.String(), "cannot prove") {
		t.Fatalf("a protected pid reached the identity check: %q", logged.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestSupervisorStartsChecksBeforeWatches(t *testing.T) {
	h := newHarness(t, watchJob(1, "DEV-1", "scripts/quiet.sh"), checkJob(2, "DEV-2", "scripts/sleep.sh"))
	h.sup.Parallel = 1
	h.start()
	h.awaitCompletion(1, 10*time.Second)
	h.jobs.mu.Lock()
	defer h.jobs.mu.Unlock()
	if _, ok := h.jobs.completions[2]; !ok {
		t.Fatal("the watch run finished before the check that was pending with it")
	}
}

func TestSupervisorKeepsOneSlotFromWatches(t *testing.T) {
	h := newHarness(t, watchJob(1, "DEV-1", "scripts/sleep.sh"), watchJob(2, "DEV-2", "scripts/sleep.sh"),
		watchJob(3, "DEV-3", "scripts/sleep.sh"))
	h.sup.Parallel = 2
	h.start()
	for id := int64(1); id <= 3; id++ {
		h.awaitCompletion(id, 10*time.Second)
	}
	h.jobs.mu.Lock()
	defer h.jobs.mu.Unlock()
	if h.jobs.maxWatches != 1 {
		t.Fatalf("%d watch runs at once with Parallel=2, want 1", h.jobs.maxWatches)
	}
}

func TestSupervisorAsksForJobsOnlyWithAFreeSlot(t *testing.T) {
	h := newHarness(t, checkJob(1, "DEV-1", "scripts/stubborn.sh"), checkJob(2, "DEV-2", "scripts/pass.sh"))
	h.sup.Parallel = 1
	h.start()
	h.awaitStart(1)
	h.jobs.mu.Lock()
	before := h.jobs.pendingCalls
	h.jobs.mu.Unlock()
	for range 5 {
		h.nudge()
		time.Sleep(40 * time.Millisecond)
	}
	h.jobs.mu.Lock()
	after := h.jobs.pendingCalls
	h.jobs.mu.Unlock()
	if after != before {
		t.Fatalf("PendingRunJobs called %d times while every slot was taken", after-before)
	}
}

func TestSupervisorKeepsTheNewestQuietRunDirectories(t *testing.T) {
	jobs := make([]tasks.RunJob, 0, keptQuietRunDirs+2)
	for id := int64(1); id <= keptQuietRunDirs+1; id++ {
		jobs = append(jobs, watchJob(id, "DEV-1", "scripts/quiet.sh"))
	}
	jobs = append(jobs, watchJob(keptQuietRunDirs+2, "DEV-1", "scripts/sleep.sh"))
	h := newHarness(t, jobs...)
	h.start()
	runDir := func(id int) string { return filepath.Join(h.base, "tasks", "DEV-1", "runs", strconv.Itoa(id)) }
	exists := func(id int) bool {
		_, err := os.Stat(runDir(id))
		return err == nil
	}
	h.awaitCompletion(keptQuietRunDirs+2, 30*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for exists(1) {
		if time.Now().After(deadline) {
			t.Fatal("the quiet run evicted from the ring kept its directory")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for id := 2; id <= keptQuietRunDirs+2; id++ {
		if !exists(id) {
			t.Fatalf("run %d lost its directory; the newest %d quiet runs and every other run keep their files", id, keptQuietRunDirs)
		}
	}
}

func TestRememberQuietKeepsARingPerTaskAndSkipsCancelledRuns(t *testing.T) {
	w := &worker{quietDirs: map[string][]string{}}
	quiet := tasks.RunCompletion{Verdict: VerdictQuiet}
	dir := func(task string, i int) string { return "/runs/" + task + "/" + strconv.Itoa(i) }
	for i := 1; i <= keptQuietRunDirs; i++ {
		if stale := w.rememberQuietLocked(watchJob(int64(i), "DEV-1", "q"), quiet, dir("DEV-1", i), false); stale != "" {
			t.Fatalf("run %d evicted %q before the ring was full", i, stale)
		}
	}
	// Another task has a ring of its own.
	if stale := w.rememberQuietLocked(watchJob(100, "DEV-2", "q"), quiet, dir("DEV-2", 1), false); stale != "" {
		t.Fatalf("another task evicted %q", stale)
	}
	// A run whose completion was cancelled is never remembered, so it is
	// never deleted.
	if stale := w.rememberQuietLocked(watchJob(50, "DEV-1", "q"), quiet, dir("DEV-1", 50), true); stale != "" {
		t.Fatalf("a cancelled run evicted %q", stale)
	}
	if stale := w.rememberQuietLocked(watchJob(51, "DEV-1", "q"), quiet, dir("DEV-1", 51), false); stale != dir("DEV-1", 1) {
		t.Fatalf("stale = %q; the oldest quiet run leaves the ring", stale)
	}
	if stale := w.rememberQuietLocked(watchJob(52, "DEV-1", "q"), quiet, dir("DEV-1", 52), false); stale != dir("DEV-1", 2) {
		t.Fatalf("stale = %q; the cancelled run took no place in the ring", stale)
	}
	// Remembering the same directory again evicts nothing.
	if stale := w.rememberQuietLocked(watchJob(52, "DEV-1", "q"), quiet, dir("DEV-1", 52), false); stale != "" {
		t.Fatalf("stale = %q", stale)
	}
	// Other runs keep their files and do not enter the ring.
	if stale := w.rememberQuietLocked(checkJob(53, "DEV-1", "c"), tasks.RunCompletion{Verdict: VerdictPass}, dir("DEV-1", 53), false); stale != "" {
		t.Fatalf("stale = %q", stale)
	}
}

func TestPrepareGivesAQueueRunADefaultPath(t *testing.T) {
	h := newHarness(t)
	w := &worker{s: h.sup}
	job := checkJob(1, "DEV-1", "scripts/env.sh")
	job.WorkflowName, job.WorkflowVersion, job.WorkflowDigest = h.image.Name, h.image.Version, h.image.Digest
	h.sup.BaseEnv = func() []string { return []string{"MARK_DIR=" + h.marks} }
	spec, reason := w.prepare(job)
	if reason != "" || !slices.Contains(spec.Env, "PATH=/usr/bin:/bin") {
		t.Fatalf("env = %v, reason %q", spec.Env, reason)
	}
	job.WorkflowEnv = map[string]string{"PATH": "/opt/bin"}
	spec, reason = w.prepare(job)
	if reason != "" || !slices.Contains(spec.Env, "PATH=/opt/bin") || slices.Contains(spec.Env, "PATH=/usr/bin:/bin") {
		t.Fatalf("env with a workflow PATH = %v, reason %q", spec.Env, reason)
	}
}

func TestPrepareRefusesATaskKeyThatIsNotOneDirectory(t *testing.T) {
	h := newHarness(t)
	w := &worker{s: h.sup}
	for _, key := range []string{"", ".", "..", "a/b", "../x"} {
		job := checkJob(1, key, "scripts/pass.sh")
		job.WorkflowName, job.WorkflowVersion, job.WorkflowDigest = h.image.Name, h.image.Version, h.image.Digest
		if _, reason := w.prepare(job); reason == "" {
			t.Errorf("key %q was accepted", key)
		}
	}
}

func TestRedactCompletionLeavesNoPartialSecret(t *testing.T) {
	secrets := map[string]string{"A": "abcdefgh", "B": "efghijkl"}
	done := redactCompletion(tasks.RunCompletion{
		Message: "x abcdefghijkl y", Artifacts: map[string]string{"note": "abcdefghijkl"},
	}, secrets)
	if done.Message != "x [redacted] y" || done.Artifacts["note"] != "[redacted]" {
		t.Fatalf("completion = %+v", done)
	}
}

func TestMergeEnvLaterReplacesEarlierAndStrips(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=1", "TARIBOY_TOOLS_SOCKET=x"}, []string{"B=2", "C=3"}, []string{"A=4"})
	want := []string{"A=4", "B=2", "C=3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mergeEnv = %v, want %v", got, want)
	}
}
