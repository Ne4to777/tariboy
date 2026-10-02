package workflowrun

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// Defaults of a Supervisor that leaves a field zero.
const (
	DefaultInterval = 2 * time.Second
	DefaultParallel = 4
)

// Completion retries: a run whose completion cannot be recorded stays running
// and is recovered as interrupted at the next daemon start.
const (
	completeAttempts   = 5
	completeRetryDelay = 500 * time.Millisecond
	completeTimeout    = 10 * time.Second
)

// strippedEnv names the variables that reach the agent tools socket or the
// daemon API. A run never receives them, whatever its baseline holds.
var strippedEnv = []string{
	"TARIBOY_TOOLS_SOCKET",
	"TARIBOY_DAEMON_SOCKET",
	"TARIBOY_PLUGIN_SOCKET",
	"TARIBOY_PLUGIN_TOKEN",
}

// protocolNames are the TARIBOY_* names the protocol owns. No other layer may
// set one, including TARIBOY_WORKFLOW_OUTCOME on a run that has no outcome.
var protocolNames = func() []string {
	var names []string
	for _, kv := range ProtocolEnv(EnvValues{Outcome: "-"}) {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	return names
}()

// Jobs is the part of the task service the worker uses.
type Jobs interface {
	PendingRunJobs(ctx context.Context) ([]tasks.RunJob, error)
	ClaimScriptRun(ctx context.Context, id int64, startedAt, logPath string) (bool, error)
	SetScriptRunPID(ctx context.Context, id int64, pid int) error
	CompleteScriptRun(ctx context.Context, id int64, done tasks.RunCompletion) error
	ScheduleDueWatches(ctx context.Context, now time.Time) (int, error)
	CancelRequestedRuns(ctx context.Context) ([]tasks.ScriptRun, error)
}

// Supervisor is the daemon worker that executes pending workflow script runs.
type Supervisor struct {
	Jobs         Jobs
	Images       *workflowimage.Store
	BaseDir      string
	BaseEnv      func() []string // daemon baseline
	AgentRuntime func(agent string) (cwd string, env []string, err error)
	Clock        func() time.Time
	Wake         <-chan struct{} // optional nudge
	Interval     time.Duration   // poll fallback, default 2s
	Parallel     int             // concurrent runs, default 4
	Log          *slog.Logger
}

// activeRun is a run this worker is executing.
type activeRun struct {
	task            string
	cancel          context.CancelFunc
	cancelRequested atomic.Bool
}

// worker is the state of one Run call.
type worker struct {
	s        *Supervisor
	log      *slog.Logger
	clock    func() time.Time
	parallel int
	finished chan struct{}
	wg       sync.WaitGroup

	mu      sync.Mutex
	running map[int64]*activeRun
	busy    map[string]bool // tasks with a run in progress
}

// Run executes pending runs until ctx ends. It wakes on Wake, when a run
// finishes, and every Interval. When ctx ends it kills every running script,
// waits for them, and leaves their records running for RecoverScriptRuns.
func (s *Supervisor) Run(ctx context.Context) {
	w := &worker{
		s: s, log: s.Log, clock: s.Clock, parallel: s.Parallel,
		finished: make(chan struct{}, 1),
		running:  map[int64]*activeRun{}, busy: map[string]bool{},
	}
	if w.log == nil {
		w.log = slog.Default()
	}
	if w.clock == nil {
		w.clock = time.Now
	}
	if w.parallel <= 0 {
		w.parallel = DefaultParallel
	}
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	// Every run context derives from ctx, so its end kills every script.
	defer w.wg.Wait()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	wake := s.Wake
	for {
		w.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case _, ok := <-wake:
			if !ok {
				wake = nil
			}
		case <-w.finished:
		case <-ticker.C:
		}
	}
}

// pass is one loop iteration: schedule due watches, kill cancelled runs, and
// start pending runs.
func (w *worker) pass(ctx context.Context) {
	if _, err := w.s.Jobs.ScheduleDueWatches(ctx, w.clock()); err != nil && ctx.Err() == nil {
		w.log.Error("schedule workflow watch runs", "err", err)
	}
	cancels, err := w.s.Jobs.CancelRequestedRuns(ctx)
	if err != nil && ctx.Err() == nil {
		w.log.Error("list cancelled workflow script runs", "err", err)
	}
	for _, run := range cancels {
		w.mu.Lock()
		active := w.running[run.ID]
		w.mu.Unlock()
		if active != nil && !active.cancelRequested.Swap(true) {
			w.log.Info("cancel workflow script run", "run_id", run.ID, "task", run.TaskKey, "script", run.Script)
			active.cancel()
		}
	}
	jobs, err := w.s.Jobs.PendingRunJobs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("list pending workflow script runs", "err", err)
		}
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		full := len(w.running) >= w.parallel
		_, running := w.running[job.Run.ID]
		busy := w.busy[job.Run.TaskKey]
		w.mu.Unlock()
		if full {
			return
		}
		if running || busy {
			continue
		}
		w.start(ctx, job)
	}
}

// start claims one job and executes it in the background. A job that cannot
// be prepared is claimed and completed as a failure without running anything.
func (w *worker) start(ctx context.Context, job tasks.RunJob) {
	id := job.Run.ID
	spec, reason := w.prepare(job)
	logPath := ""
	if reason == "" {
		logPath = filepath.Join(spec.RunDir, "run.log")
	}
	claimed, err := w.s.Jobs.ClaimScriptRun(ctx, id, w.clock().UTC().Format(time.RFC3339Nano), logPath)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("claim workflow script run", "run_id", id, "task", job.Run.TaskKey, "err", err)
		}
		return
	}
	if !claimed {
		return
	}
	if reason == "" {
		for _, dir := range []string{spec.TaskDir, spec.RunDir} {
			if err := ownerDir(dir); err != nil {
				reason = fmt.Sprintf("cannot create directory %s: %v", dir, err)
				break
			}
		}
	}
	if reason != "" {
		reason = redactSecrets(reason, job.QueueSecrets)
		w.log.Warn("workflow script run failed before it started", "run_id", id, "task", job.Run.TaskKey,
			"script", job.Run.Script, "reason", reason)
		w.complete(ctx, job, tasks.RunCompletion{
			Verdict: VerdictFailure, Message: reason,
			FinishedAt: w.clock().UTC().Format(time.RFC3339Nano),
		})
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	active := &activeRun{task: job.Run.TaskKey, cancel: cancel}
	w.mu.Lock()
	w.running[id] = active
	w.busy[active.task] = true
	w.mu.Unlock()
	spec.OnStart = func(pid int) {
		if err := w.s.Jobs.SetScriptRunPID(ctx, id, pid); err != nil && ctx.Err() == nil {
			w.log.Error("record workflow script pid", "run_id", id, "task", job.Run.TaskKey, "err", err)
		}
	}
	w.log.Info("workflow script started", "run_id", id, "task", job.Run.TaskKey, "kind", job.Run.Kind,
		"script", job.Run.Script, "cwd", spec.Cwd)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer w.release(id, active)
		res := Execute(runCtx, spec)
		cancel()
		attrs := []any{"run_id", id, "task", job.Run.TaskKey, "script", job.Run.Script,
			"verdict", res.Verdict.Kind, "exit_code", exitAttr(res.ExitCode),
			"duration", res.Finished.Sub(res.Started).Round(time.Millisecond)}
		if ctx.Err() != nil {
			w.log.Info("workflow script killed at shutdown; left for recovery", attrs...)
			return
		}
		if active.cancelRequested.Load() {
			w.log.Info("workflow script cancelled", attrs...)
		} else {
			w.log.Info("workflow script finished", append(attrs, "log", res.LogPath)...)
		}
		done := redactCompletion(tasks.RunCompletion{
			Verdict: res.Verdict.Kind, Outcome: res.Verdict.Outcome, Message: res.Verdict.Message,
			Artifacts: res.Verdict.Artifacts, ExitCode: res.ExitCode, LogPath: res.LogPath,
			FinishedAt: w.clock().UTC().Format(time.RFC3339Nano),
		}, job.QueueSecrets)
		w.log.Debug("workflow script message", "run_id", id, "message", done.Message)
		w.complete(ctx, job, done)
	}()
}

// release forgets a finished run, after its completion is recorded, so the
// next run of the task cannot start before the engine has seen this one.
func (w *worker) release(id int64, active *activeRun) {
	w.mu.Lock()
	delete(w.running, id)
	delete(w.busy, active.task)
	w.mu.Unlock()
	select {
	case w.finished <- struct{}{}:
	default:
	}
}

// complete records a finished run, retrying a few times. A completion that
// cannot be recorded leaves the run running for RecoverScriptRuns.
func (w *worker) complete(ctx context.Context, job tasks.RunJob, done tasks.RunCompletion) {
	id := job.Run.ID
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
		err := w.s.Jobs.CompleteScriptRun(cctx, id, done)
		cancel()
		if err == nil {
			return
		}
		w.log.Error("record workflow script run", "run_id", id, "task", job.Run.TaskKey,
			"attempt", attempt, "err", err)
		if attempt == completeAttempts {
			w.log.Error("workflow script run stays running until the next daemon start",
				"run_id", id, "task", job.Run.TaskKey)
			return
		}
		select {
		case <-ctx.Done():
			w.log.Error("workflow script run stays running until the next daemon start",
				"run_id", id, "task", job.Run.TaskKey)
			return
		case <-time.After(completeRetryDelay):
		}
	}
}

// prepare builds the Spec of a job without touching the file system. A
// non-empty reason says why the job cannot run; it names no environment value.
func (w *worker) prepare(job tasks.RunJob) (Spec, string) {
	run := job.Run
	key := run.TaskKey
	if key == "" || !filepath.IsLocal(key) || strings.ContainsAny(key, `/\`) {
		return Spec{}, fmt.Sprintf("task key %q is not a valid directory name", key)
	}
	base, err := filepath.Abs(w.s.BaseDir)
	if err != nil {
		return Spec{}, fmt.Sprintf("cannot resolve the base directory: %v", err)
	}
	if w.s.Images == nil {
		return Spec{}, "no workflow image store is configured"
	}
	scriptPath, err := w.s.Images.FilePath(job.WorkflowName, job.WorkflowDigest, run.Script)
	if err != nil {
		return Spec{}, fmt.Sprintf("script %s of workflow %s %s is not usable: %v",
			run.Script, job.WorkflowName, job.WorkflowVersion, err)
	}
	taskRoot := filepath.Join(base, "tasks", key)
	stateDir := filepath.Join(taskRoot, "state")
	runDir := filepath.Join(taskRoot, "runs", strconv.FormatInt(run.ID, 10))

	var cwd string
	var baseline []string
	// A watch run is always a queue run, whatever its record says.
	if run.Kind == KindCheck && run.RunAs == workflowfile.RunAsAgent {
		if job.Holder == "" {
			return Spec{}, fmt.Sprintf("check %s runs as the agent, but the task has no agent holder", run.Script)
		}
		if w.s.AgentRuntime == nil {
			return Spec{}, fmt.Sprintf("check %s runs as agent %s, but no agent runtime is configured", run.Script, job.Holder)
		}
		cwd, baseline, err = w.s.AgentRuntime(job.Holder)
		if err != nil {
			return Spec{}, fmt.Sprintf("cannot prepare the runtime of agent %s for check %s: %v", job.Holder, run.Script, err)
		}
	} else {
		cwd = stateDir
		if w.s.BaseEnv != nil {
			baseline = w.s.BaseEnv()
		}
	}

	outcome := ""
	if run.Kind == KindCheck {
		outcome = job.Outcome
	}
	protocol := ProtocolEnv(EnvValues{
		TaskKey: key, Queue: job.Queue, WorkflowName: job.WorkflowName, WorkflowVersion: job.WorkflowVersion,
		Status: job.Status, Outcome: outcome,
		WorkflowDir: w.s.Images.ContentDir(job.WorkflowName, job.WorkflowDigest),
		TaskFile:    filepath.Join(runDir, "task.json"), TaskDir: stateDir,
		ResultFile: filepath.Join(runDir, "result.json"),
	})
	env := mergeEnv(
		withoutNames(baseline, protocolNames),
		withoutNames(mapEnv(job.WorkflowEnv), protocolNames),
		withoutNames(mapEnv(job.QueueSecrets), protocolNames),
		protocol,
	)
	return Spec{
		RunID: strconv.FormatInt(run.ID, 10), Kind: run.Kind, ScriptPath: scriptPath, Cwd: cwd, Env: env,
		Timeout: job.Timeout, RunDir: runDir, TaskDir: stateDir, Snapshot: job.Snapshot,
		Declared: Declared{Outcomes: job.Outcomes, Artifacts: job.Artifacts},
	}, ""
}

// mergeEnv joins environment lists so that a later value replaces an earlier
// one of the same name, keeping the order of first appearance, and drops the
// variables in strippedEnv.
func mergeEnv(lists ...[]string) []string {
	values := map[string]string{}
	var order []string
	for _, list := range lists {
		for _, kv := range list {
			name, value, ok := strings.Cut(kv, "=")
			if !ok || name == "" {
				continue
			}
			if _, seen := values[name]; !seen {
				order = append(order, name)
			}
			values[name] = value
		}
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		if !contains(strippedEnv, name) {
			out = append(out, name+"="+values[name])
		}
	}
	return out
}

// mapEnv turns a variable map into entries sorted by name.
func mapEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for name, value := range m {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out
}

// withoutNames drops the entries whose name is in names.
func withoutNames(env, names []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !contains(names, name) {
			out = append(out, kv)
		}
	}
	return out
}

// Redaction of queue secret values from what a script returns. A value shorter
// than minRedactBytes is too short to match safely and is left alone.
const (
	redactedText   = "[redacted]"
	minRedactBytes = 6
)

// redactCompletion replaces queue secret values in the message and in every
// artifact value. The outcome is matched against declared names and is left
// alone; the run log is owner-only and is not rewritten.
func redactCompletion(done tasks.RunCompletion, secrets map[string]string) tasks.RunCompletion {
	done.Message = redactSecrets(done.Message, secrets)
	if done.Artifacts != nil {
		artifacts := make(map[string]string, len(done.Artifacts))
		for name, value := range done.Artifacts {
			artifacts[name] = redactSecrets(value, secrets)
		}
		done.Artifacts = artifacts
	}
	return done
}

// redactSecrets replaces every secret value of at least minRedactBytes in s,
// longest first, so a value that contains another is replaced whole.
func redactSecrets(s string, secrets map[string]string) string {
	if s == "" {
		return s
	}
	values := make([]string, 0, len(secrets))
	for _, value := range secrets {
		if len(value) >= minRedactBytes {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		s = strings.ReplaceAll(s, value, redactedText)
	}
	return s
}

func exitAttr(code *int) any {
	if code == nil {
		return nil
	}
	return *code
}
