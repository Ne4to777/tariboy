package workflowrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// DefaultTimeout bounds a run whose Spec names no timeout.
const DefaultTimeout = 60 * time.Second

// killGrace is how long a process group has between SIGTERM and SIGKILL.
const killGrace = 2 * time.Second

// Spec describes one run of one workflow script.
type Spec struct {
	RunID      string
	Kind       string
	ScriptPath string // absolute path inside the unpacked image
	Cwd        string
	Env        []string // complete environment, protocol entries included
	Timeout    time.Duration
	RunDir     string // <base>/tasks/<KEY>/runs/<run-id>, or the run directory of a source
	TaskDir    string // <base>/tasks/<KEY>/state, or the state directory of a source
	Snapshot   []byte // written to RunDir/task.json
	Declared   Declared
	OnStart    func(pid int) // called once the process has a PID
}

// Result is what one run produced.
type Result struct {
	Verdict  Verdict
	ExitCode *int
	TimedOut bool
	LogPath  string
	Started  time.Time
	Finished time.Time
}

// Execute prepares the directories and task.json, runs the script in its own
// process group with combined output in RunDir/run.log, enforces the timeout,
// reads RunDir/result.json, and classifies. Cancelling ctx kills the group.
func Execute(ctx context.Context, spec Spec) Result {
	var res Result
	// fail records a run that ended before or instead of a normal exit. The
	// reason also goes to the log when one is open, so a reader sees it there.
	var logFile *os.File
	fail := func(format string, args ...any) Result {
		res.Verdict = failure(format, args...)
		now := time.Now()
		if res.Started.IsZero() {
			res.Started = now
		}
		res.Finished = now
		if logFile != nil {
			_, _ = fmt.Fprintf(logFile, "error: %s\n", res.Verdict.Message)
			_ = logFile.Close()
		}
		return res
	}

	for _, dir := range []string{spec.RunDir, spec.TaskDir} {
		if err := ownerDir(dir); err != nil {
			return fail("cannot create directory %s: %v", dir, err)
		}
	}
	logPath, err := filepath.Abs(filepath.Join(spec.RunDir, "run.log"))
	if err != nil {
		return fail("cannot resolve the run log path: %v", err)
	}
	logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		logFile = nil
		return fail("cannot create the run log %s: %v", logPath, err)
	}
	res.LogPath = logPath
	if _, err := fmt.Fprintf(logFile, "script: %s\ncwd: %s\n", spec.ScriptPath, spec.Cwd); err != nil {
		return fail("cannot write the run log %s: %v", logPath, err)
	}

	// A source run has no task, so it has no snapshot.
	if spec.Kind != KindSource {
		taskFile := filepath.Join(spec.RunDir, "task.json")
		if err := writeOwnerFile(taskFile, spec.Snapshot); err != nil {
			return fail("cannot write %s: %v", taskFile, err)
		}
	}
	resultPath := filepath.Join(spec.RunDir, "result.json")
	if err := os.Remove(resultPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail("cannot remove the stale result file %s: %v", resultPath, err)
	}

	if info, err := os.Stat(spec.Cwd); err != nil {
		return fail("the working directory %s is not usable: %v", spec.Cwd, err)
	} else if !info.IsDir() {
		return fail("the working directory %s is not a directory", spec.Cwd)
	}
	if !filepath.IsAbs(spec.ScriptPath) {
		return fail("the script path %s is not absolute", spec.ScriptPath)
	}
	if info, err := os.Stat(spec.ScriptPath); err != nil {
		return fail("the script %s is not usable: %v", spec.ScriptPath, err)
	} else if !info.Mode().IsRegular() {
		return fail("the script %s is not a regular file", spec.ScriptPath)
	} else if info.Mode().Perm()&0o111 == 0 {
		return fail("the script %s is not executable", spec.ScriptPath)
	}
	if ctx.Err() != nil {
		return fail("the run was cancelled before it started")
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancelRun := context.WithTimeout(ctx, timeout)
	defer cancelRun()

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return fail("cannot open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()

	// The worker signals the process group itself, never through os/exec's
	// context handling, which may signal after the process was reaped.
	cmd := exec.Command(spec.ScriptPath)
	cmd.Dir, cmd.Env = spec.Cwd, spec.Env
	if cmd.Env == nil {
		// A nil Env means "inherit" to os/exec; a run gets exactly spec.Env.
		cmd.Env = []string{}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	res.Started = time.Now()
	if err := cmd.Start(); err != nil {
		return fail("cannot start the script %s: %v", spec.ScriptPath, err)
	}
	pid := cmd.Process.Pid
	proc := startProcess(pid)
	// reap kills what the script left in its group while the exited leader,
	// not yet reaped, still holds the group id, and then reaps the leader.
	reap := func() {
		<-proc.exited
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			_, _ = fmt.Fprintf(logFile, "error: cannot kill the process group %d: %v\n", pid, err)
		}
		_ = cmd.Wait()
		res.Finished = time.Now()
	}
	if !callOnStart(spec.OnStart, pid) {
		proc.signal(syscall.SIGKILL)
		reap()
		_, _ = fmt.Fprintln(logFile, "error: run bookkeeping failed")
		_ = logFile.Close()
		res.Verdict = failure("run bookkeeping failed")
		return res
	}
	killed := false
	select {
	case <-proc.exited:
	case <-runCtx.Done():
		if testHookTimeout != nil {
			testHookTimeout(proc.exited)
		}
		// A script that has exited by itself is judged by its exit code, even
		// when the deadline passed at the same instant.
		if killed = proc.signal(syscall.SIGTERM); killed {
			select {
			case <-proc.exited:
			case <-time.After(killGrace):
				proc.signal(syscall.SIGKILL)
			}
		}
	}
	reap()
	_ = logFile.Close()

	if st := cmd.ProcessState; st != nil && st.Exited() {
		code := st.ExitCode()
		res.ExitCode = &code
	}
	if killed && ctx.Err() != nil {
		res.Verdict = failure("the run was cancelled")
		return res
	}
	res.TimedOut = killed && errors.Is(runCtx.Err(), context.DeadlineExceeded)
	var raw []byte
	var resultErr error
	if !res.TimedOut {
		raw, resultErr = readResultFile(resultPath)
	}
	res.Verdict = Classify(spec.Kind, res.ExitCode, res.TimedOut, raw, resultErr, spec.Declared)
	return res
}

// testHookTimeout, when set by a test, runs when the timeout fires, before the
// worker decides whether to signal; it receives the channel closed on exit.
var testHookTimeout func(exited <-chan struct{})

// process tracks whether a started child has exited, without reaping it, so
// its process group is signalled only while its id cannot have been reused.
type process struct {
	pid    int
	exited chan struct{} // closed once the child has exited

	mu   sync.Mutex
	done bool
}

// startProcess watches pid until it exits. The child stays a zombie, holding
// its pid and group id, until the caller reaps it after exited is closed.
func startProcess(pid int) *process {
	p := &process{pid: pid, exited: make(chan struct{})}
	go func() {
		waitExit(pid)
		p.mu.Lock()
		p.done = true
		p.mu.Unlock()
		close(p.exited)
	}()
	return p
}

// signal sends sig to the process group while the leader has not exited; it
// reports whether it did.
func (p *process) signal(sig syscall.Signal) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return false
	}
	_ = syscall.Kill(-p.pid, sig)
	return true
}

// callOnStart runs onStart and reports false when it panicked.
func callOnStart(onStart func(int), pid int) (ok bool) {
	if onStart == nil {
		return true
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	onStart(pid)
	return true
}

// ownerDir creates dir, and narrows it to 0700 if it already existed.
func ownerDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// writeOwnerFile writes data to path with mode 0600, never through a symlink.
func writeOwnerFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readResultFile reads the result a script wrote. A missing file is no result;
// a symlink, a non-regular file, or one larger than MaxResultBytes is an error.
func readResultFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("result file is not a regular file (%v)", info.Mode().Type())
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxResultBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxResultBytes {
		return nil, fmt.Errorf("result file is larger than %d bytes", MaxResultBytes)
	}
	return raw, nil
}
