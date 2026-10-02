package workflowrun

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fixture is one run's layout under a temporary base directory.
type fixture struct {
	dir, script, runDir, taskDir, resultFile string
}

func newFixture(t *testing.T, body string) fixture {
	t.Helper()
	dir := t.TempDir()
	f := fixture{
		dir:     dir,
		script:  filepath.Join(dir, "image", "check.sh"),
		runDir:  filepath.Join(dir, "tasks", "K-1", "runs", "r1"),
		taskDir: filepath.Join(dir, "tasks", "K-1", "state"),
	}
	f.resultFile = filepath.Join(f.runDir, "result.json")
	if err := os.MkdirAll(filepath.Dir(f.script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) spec(kind string) Spec {
	return Spec{
		RunID:      "r1",
		Kind:       kind,
		ScriptPath: f.script,
		Cwd:        f.taskDir,
		Env: []string{
			"PATH=/usr/bin:/bin",
			"TARIBOY_TASK_FILE=" + filepath.Join(f.runDir, "task.json"),
			"TARIBOY_TASK_DIR=" + f.taskDir,
			"TARIBOY_RESULT_FILE=" + f.resultFile,
			"FIXTURE_DIR=" + f.dir,
		},
		Timeout:  5 * time.Second,
		RunDir:   f.runDir,
		TaskDir:  f.taskDir,
		Snapshot: []byte(`{"key":"K-1"}`),
		Declared: declared,
	}
}

func requireExit(t *testing.T, r Result, code int) {
	t.Helper()
	if r.ExitCode == nil || *r.ExitCode != code {
		t.Fatalf("exit code = %v, want %d (result %+v)", r.ExitCode, code, r)
	}
}

func readLog(t *testing.T, r Result) string {
	t.Helper()
	b, err := os.ReadFile(r.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExecuteCheckPass(t *testing.T) {
	f := newFixture(t, `echo hello; echo oops >&2; cat "$TARIBOY_TASK_FILE" > "$FIXTURE_DIR/seen.json"; exit 0`+"\n")
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictPass {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	requireExit(t, r, 0)
	if r.TimedOut {
		t.Fatal("timed out")
	}
	if r.LogPath != filepath.Join(f.runDir, "run.log") || !filepath.IsAbs(r.LogPath) {
		t.Fatalf("log path = %q", r.LogPath)
	}
	log := readLog(t, r)
	want := "script: " + f.script + "\ncwd: " + f.taskDir + "\n"
	if !strings.HasPrefix(log, want) || !strings.Contains(log, "hello\n") || !strings.Contains(log, "oops\n") {
		t.Fatalf("log = %q", log)
	}
	seen, err := os.ReadFile(filepath.Join(f.dir, "seen.json"))
	if err != nil || string(seen) != `{"key":"K-1"}` {
		t.Fatalf("snapshot seen by the script = %q, %v", seen, err)
	}
	if r.Started.IsZero() || r.Finished.Before(r.Started) {
		t.Fatalf("times %v %v", r.Started, r.Finished)
	}
}

func TestExecuteCheckRejectWithMessage(t *testing.T) {
	f := newFixture(t, `printf '{"message":"tests fail"}' > "$TARIBOY_RESULT_FILE"; exit 112`+"\n")
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictReject || r.Verdict.Message != "tests fail" {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	requireExit(t, r, 112)
}

func TestExecuteWatchOutcomeWithArtifact(t *testing.T) {
	f := newFixture(t, `printf '{"outcome":"merged","artifacts":{"merge_commit":"9f2c"}}' > "$TARIBOY_RESULT_FILE"; exit 0`+"\n")
	r := Execute(context.Background(), f.spec(KindWatch))
	if r.Verdict.Kind != VerdictOutcome || r.Verdict.Outcome != "merged" || r.Verdict.Artifacts["merge_commit"] != "9f2c" {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
}

func TestExecuteWatchQuiet(t *testing.T) {
	f := newFixture(t, "exit 111\n")
	r := Execute(context.Background(), f.spec(KindWatch))
	if r.Verdict.Kind != VerdictQuiet {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	requireExit(t, r, 111)
}

func TestExecuteCrash(t *testing.T) {
	f := newFixture(t, "echo boom >&2; exit 3\n")
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "exit 3") {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	requireExit(t, r, 3)
	if !strings.Contains(readLog(t, r), "boom") {
		t.Fatal("stderr not in the log")
	}
}

func TestExecuteKilledBySignal(t *testing.T) {
	f := newFixture(t, "kill -9 $$\n")
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictFailure || r.ExitCode != nil {
		t.Fatalf("result = %+v", r)
	}
}

func TestExecuteTimeoutKillsGroup(t *testing.T) {
	f := newFixture(t, `sleep 30 & echo $! > "$FIXTURE_DIR/child.pid"; wait`+"\n")
	spec := f.spec(KindCheck)
	spec.Timeout = 300 * time.Millisecond
	start := time.Now()
	r := Execute(context.Background(), spec)
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if !r.TimedOut || r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "timed out") {
		t.Fatalf("result = %+v", r)
	}
	requireGone(t, readPID(t, filepath.Join(f.dir, "child.pid")))
}

func TestExecuteCancelIsNotTimeout(t *testing.T) {
	f := newFixture(t, `echo started > "$FIXTURE_DIR/started"; sleep 30`+"\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spec := f.spec(KindCheck)
	spec.OnStart = func(int) {
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
	}
	r := Execute(ctx, spec)
	if r.TimedOut || r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "cancelled") {
		t.Fatalf("result = %+v", r)
	}
}

// TestExecuteExitBeforeTheTimeoutSignalIsJudgedByItsCode: the deadline passes
// while the script is exiting by itself. The hook holds the timeout branch
// until the script has exited, so the worker must see that and judge the exit
// code instead of signalling a finished process.
func TestExecuteExitBeforeTheTimeoutSignalIsJudgedByItsCode(t *testing.T) {
	f := newFixture(t, "sleep 0.3; exit 0\n")
	spec := f.spec(KindCheck)
	spec.Timeout = 50 * time.Millisecond
	testHookTimeout = func(exited <-chan struct{}) { <-exited }
	t.Cleanup(func() { testHookTimeout = nil })
	r := Execute(context.Background(), spec)
	if r.TimedOut || r.Verdict.Kind != VerdictPass {
		t.Fatalf("result = %+v", r)
	}
	requireExit(t, r, 0)
}

func TestProcessIsNeverSignalledAfterItExited(t *testing.T) {
	cmd := exec.Command("sleep", "0.1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := startProcess(cmd.Process.Pid)
	<-p.exited
	if p.signal(syscall.SIGKILL) {
		t.Fatal("signal reported delivery to an exited process")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the process was signalled: %v", err)
	}
}

func TestExecuteOnStartPanicKillsTheRun(t *testing.T) {
	f := newFixture(t, `sleep 30 & echo $! > "$FIXTURE_DIR/child.pid"; sleep 30`+"\n")
	spec := f.spec(KindCheck)
	childFile := filepath.Join(f.dir, "child.pid")
	spec.OnStart = func(int) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(childFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		panic("bookkeeping exploded")
	}
	start := time.Now()
	r := Execute(context.Background(), spec)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if r.Verdict.Kind != VerdictFailure || r.Verdict.Message != "run bookkeeping failed" {
		t.Fatalf("result = %+v", r)
	}
	requireGone(t, readPID(t, childFile))
}

func TestExecuteCancelledBeforeStart(t *testing.T) {
	f := newFixture(t, `touch "$FIXTURE_DIR/ran"`+"\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := Execute(ctx, f.spec(KindCheck))
	if r.TimedOut || r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "cancelled") {
		t.Fatalf("result = %+v", r)
	}
}

func TestExecuteKillsOrphanedChild(t *testing.T) {
	f := newFixture(t, `sleep 30 & echo $! > "$FIXTURE_DIR/child.pid"; exit 0`+"\n")
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictPass {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	requireGone(t, readPID(t, filepath.Join(f.dir, "child.pid")))
}

func TestExecuteLargeOutputGoesToDisk(t *testing.T) {
	const size = 8 << 20
	f := newFixture(t, "head -c "+strconv.Itoa(size)+" /dev/zero; exit 0\n")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := Execute(context.Background(), f.spec(KindCheck))
	runtime.ReadMemStats(&after)
	if r.Verdict.Kind != VerdictPass {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	info, err := os.Stat(r.LogPath)
	if err != nil || info.Size() < size {
		t.Fatalf("log size = %v, %v", info, err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > size/2 {
		t.Fatalf("Execute allocated %d bytes for an %d byte output", alloc, size)
	}
}

func TestExecuteResultTooLarge(t *testing.T) {
	f := newFixture(t, `head -c 70000 /dev/zero | tr '\0' ' ' > "$TARIBOY_RESULT_FILE"; exit 0`+"\n")
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "larger than") {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
}

func TestExecuteResultSymlinkIsFailure(t *testing.T) {
	f := newFixture(t, `printf '{"message":"x"}' > "$FIXTURE_DIR/real.json"; ln -s "$FIXTURE_DIR/real.json" "$TARIBOY_RESULT_FILE"; exit 0`+"\n")
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "result file") {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
}

func TestExecuteResultDirectoryIsFailure(t *testing.T) {
	f := newFixture(t, `mkdir "$TARIBOY_RESULT_FILE"; exit 0`+"\n")
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "result file") {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
}

func TestExecuteRemovesStaleResult(t *testing.T) {
	f := newFixture(t, "exit 0\n")
	if err := os.MkdirAll(f.runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.resultFile, []byte(`{"outcome":"merged"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Execute(context.Background(), f.spec(KindWatch))
	if r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "without an outcome") {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
}

func TestExecuteMissingScript(t *testing.T) {
	f := newFixture(t, "exit 0\n")
	spec := f.spec(KindCheck)
	spec.ScriptPath = filepath.Join(f.dir, "image", "absent.sh")
	r := Execute(context.Background(), spec)
	if r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "absent.sh") || r.ExitCode != nil {
		t.Fatalf("result = %+v", r)
	}
	if r.LogPath == "" || r.Started.IsZero() || !r.Started.Equal(r.Finished) {
		t.Fatalf("result = %+v", r)
	}
}

func TestExecuteNonExecutableScript(t *testing.T) {
	f := newFixture(t, "exit 0\n")
	if err := os.Chmod(f.script, 0o644); err != nil {
		t.Fatal(err)
	}
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "not executable") {
		t.Fatalf("result = %+v", r)
	}
}

func TestExecuteMissingCwd(t *testing.T) {
	f := newFixture(t, `touch "$FIXTURE_DIR/ran"`+"\n")
	spec := f.spec(KindCheck)
	spec.Cwd = filepath.Join(f.dir, "nowhere")
	called := false
	spec.OnStart = func(int) { called = true }
	r := Execute(context.Background(), spec)
	if r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "nowhere") || called {
		t.Fatalf("result = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the script ran")
	}
}

func TestExecuteExistingLogFails(t *testing.T) {
	f := newFixture(t, `touch "$FIXTURE_DIR/ran"`+"\n")
	if err := os.MkdirAll(f.runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.runDir, "run.log"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictFailure || !strings.Contains(r.Verdict.Message, "run.log") {
		t.Fatalf("result = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the script ran")
	}
}

func TestExecuteModes(t *testing.T) {
	f := newFixture(t, "exit 0\n")
	// A task directory that already exists with wider permissions is narrowed.
	if err := os.MkdirAll(f.taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := Execute(context.Background(), f.spec(KindCheck))
	if r.Verdict.Kind != VerdictPass {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	for path, want := range map[string]os.FileMode{
		f.runDir:                             0o700 | os.ModeDir,
		f.taskDir:                            0o700 | os.ModeDir,
		filepath.Join(f.runDir, "task.json"): 0o600,
		filepath.Join(f.runDir, "run.log"):   0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want {
			t.Errorf("%s mode = %v, want %v", path, info.Mode(), want)
		}
	}
}

func TestExecuteOnStartOnceWithLivePID(t *testing.T) {
	f := newFixture(t, `echo $$ > "$FIXTURE_DIR/self.pid"; sleep 0.2`+"\n")
	spec := f.spec(KindCheck)
	var pids []int
	var alive []bool
	spec.OnStart = func(pid int) {
		pids = append(pids, pid)
		alive = append(alive, syscall.Kill(pid, 0) == nil)
	}
	r := Execute(context.Background(), spec)
	if r.Verdict.Kind != VerdictPass {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	if len(pids) != 1 || !alive[0] {
		t.Fatalf("OnStart calls %v alive %v", pids, alive)
	}
	if got := readPID(t, filepath.Join(f.dir, "self.pid")); got != pids[0] {
		t.Fatalf("OnStart pid %d, script pid %d", pids[0], got)
	}
}

func TestExecuteEnvironmentIsExactlySpecEnv(t *testing.T) {
	t.Setenv("WORKFLOWRUN_LEAK_SENTINEL", "1")
	f := newFixture(t, `env > "$FIXTURE_DIR/env.txt"`+"\n")
	spec := f.spec(KindCheck)
	r := Execute(context.Background(), spec)
	if r.Verdict.Kind != VerdictPass {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	b, err := os.ReadFile(filepath.Join(f.dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		// The shell itself exports these; they do not come from the daemon.
		if strings.HasPrefix(line, "PWD=") || strings.HasPrefix(line, "OLDPWD=") ||
			strings.HasPrefix(line, "SHLVL=") || strings.HasPrefix(line, "_=") {
			continue
		}
		got = append(got, line)
	}
	want := slices.Clone(spec.Env)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("env = %q, want %q", got, want)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// requireGone waits up to two seconds for pid to stop existing.
func requireGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d still exists: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
