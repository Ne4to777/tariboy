package workflowrun

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/alekzonder/tariboy/internal/tasks"
)

// orphanPoll is how often the worker checks whether a terminated orphan's
// process group is gone.
const orphanPoll = 50 * time.Millisecond

// terminateOrphans terminates the scripts a previous daemon left running: for
// every running run with a recorded PID whose process is alive and provably
// that run's script, its process group gets SIGTERM, the kill grace, and
// SIGKILL. Proof is the run's own result file path in the process's
// environment, read from /proc; without /proc nothing is signalled and each
// such run is logged.
func (w *worker) terminateOrphans(ctx context.Context) error {
	runs, err := w.s.Jobs.RunningScriptRuns(ctx)
	if err != nil {
		return err
	}
	base, err := filepath.Abs(w.s.BaseDir)
	if err != nil {
		return err
	}
	_, procErr := os.Stat(filepath.Join(procRoot, "self"))
	for _, run := range runs {
		if run.PID == nil || *run.PID <= 0 || !tasks.IsTaskDirKey(run.TaskKey) {
			continue
		}
		pid := *run.PID
		if procErr != nil {
			w.log.Warn("cannot prove a recorded workflow script process is the run's script without /proc; it is not signalled",
				"run_id", run.ID, "task", run.TaskKey, "pid", pid)
			continue
		}
		resultFile := filepath.Join(base, "tasks", run.TaskKey, "runs", strconv.FormatInt(run.ID, 10), "result.json")
		if !isRunScript(pid, resultFile) {
			continue
		}
		w.log.Warn("terminate a workflow script left running by a previous daemon",
			"run_id", run.ID, "task", run.TaskKey, "pid", pid)
		terminateOrphan(pid, resultFile)
	}
	return nil
}

// isRunScript reports whether process pid is alive and has
// TARIBOY_RESULT_FILE=resultFile in its environment, which only the run's
// script and the children it started carry.
func isRunScript(pid int, resultFile string) bool {
	environ, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return false
	}
	want := []byte("TARIBOY_RESULT_FILE=" + resultFile)
	for _, entry := range bytes.Split(environ, []byte{0}) {
		if bytes.Equal(entry, want) {
			return true
		}
	}
	return false
}

// terminateOrphan stops the process group of a proven script: SIGTERM, up to
// killGrace for the group to end, then SIGKILL. Before SIGKILL it proves the
// group is still the script's: either the leader still carries the run's
// environment, or the leader is gone while the group lives on, which keeps its
// id from being reused.
func terminateOrphan(pid int, resultFile string) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(killGrace)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(orphanPoll)
	}
	if errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
		return
	}
	leaderGone := errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	if leaderGone || isRunScript(pid, resultFile) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
