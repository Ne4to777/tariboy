package workflowrun

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// Defaults of a Supervisor that leaves a field zero.
const (
	DefaultInterval = 2 * time.Second
	DefaultParallel = 4
)

// completeTimeout bounds one attempt to record a finished run. A completion
// that fails is kept and tried again on every pass until it is recorded.
const completeTimeout = 10 * time.Second

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

// procRoot is where the worker reads a process's environment to prove that a
// recorded PID is still a run's script; a test points it elsewhere.
var procRoot = "/proc"

// defaultPath is the PATH of a queue run whose environment names none.
const defaultPath = "PATH=/usr/bin:/bin"

// Jobs is the part of the task service the worker uses.
type Jobs interface {
	PendingRunJobs(ctx context.Context) ([]tasks.RunJob, error)
	ClaimScriptRun(ctx context.Context, id int64, startedAt, logPath string) (bool, error)
	SetScriptRunPID(ctx context.Context, id int64, pid int) error
	CompleteScriptRun(ctx context.Context, id int64, done tasks.RunCompletion) error
	ScheduleDueWatches(ctx context.Context, now time.Time) (int, error)
	CancelRequestedRuns(ctx context.Context) ([]tasks.ScriptRun, error)
	RunningScriptRuns(ctx context.Context) ([]tasks.ScriptRun, error)
	RecoverScriptRuns(ctx context.Context) error
}

// Supervisor is the daemon worker that executes pending workflow script runs.
type Supervisor struct {
	Jobs         Jobs
	Sources      SourceJobs // optional; nil runs no workflow sources
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
	kind            string
	cancel          context.CancelFunc
	cancelRequested atomic.Bool
}

// unrecorded is a finished run whose completion failed; the worker tries to
// record it again on every pass.
type unrecorded struct {
	job       tasks.RunJob
	done      tasks.RunCompletion
	runDir    string
	cancelled bool // the run was cancelled; the engine records it as cancelled
}

// worker is the state of one Run call.
type worker struct {
	s        *Supervisor
	log      *slog.Logger
	clock    func() time.Time
	parallel int
	finished chan struct{}
	wg       sync.WaitGroup

	mu          sync.Mutex
	running     map[int64]*activeRun
	busy        map[string]bool            // tasks with a run in progress or unrecorded
	retry       map[int64]unrecorded       // completions to record again
	sources     map[string]*activeRun      // running source runs by queue/source
	sourceRetry map[int64]unrecordedSource // source completions to record again
	quietDirs   map[string][]string        // task key or "source:QUEUE/NAME" -> directories of its newest quiet runs, oldest first
	recovered   bool                       // RecoverScriptRuns has succeeded
}

// Run executes pending runs until ctx ends. It first terminates the scripts a
// previous daemon left behind and records their runs as interrupted, and starts
// nothing before that succeeded. It wakes on Wake, when a run finishes, and
// every Interval. When ctx ends it kills every running script, waits for them,
// and leaves their records running for the next start; completions not yet
// recorded are dropped with them.
func (s *Supervisor) Run(ctx context.Context) {
	w := &worker{
		s: s, log: s.Log, clock: s.Clock, parallel: s.Parallel,
		finished: make(chan struct{}, 1),
		running:  map[int64]*activeRun{}, busy: map[string]bool{},
		retry: map[int64]unrecorded{}, quietDirs: map[string][]string{},
		sources: map[string]*activeRun{}, sourceRetry: map[int64]unrecordedSource{},
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
		if w.ensureRecovered(ctx) {
			w.pass(ctx)
		}
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

// ensureRecovered reports whether recovery has succeeded, attempting it if
// not: the orphaned scripts of a previous daemon are terminated, then their
// runs are recorded as interrupted. A failure is logged and tried again next
// pass.
func (w *worker) ensureRecovered(ctx context.Context) bool {
	if w.recovered {
		return true
	}
	if err := w.terminateOrphans(ctx); err != nil {
		if ctx.Err() == nil {
			w.log.Error("find orphaned workflow scripts", "err", err)
		}
		return false
	}
	if err := w.s.Jobs.RecoverScriptRuns(ctx); err != nil {
		if ctx.Err() == nil {
			w.log.Error("recover workflow script runs", "err", err)
		}
		return false
	}
	if w.s.Sources != nil {
		if err := w.s.Sources.RecoverSourceRuns(ctx); err != nil {
			if ctx.Err() == nil {
				w.log.Error("recover workflow source runs", "err", err)
			}
			return false
		}
	}
	w.recovered = true
	return true
}

// pass is one loop iteration: the script runs of tasks, then the sources of
// queues.
func (w *worker) pass(ctx context.Context) {
	w.passScripts(ctx)
	w.passSources(ctx)
}

// loadLocked returns how many runs are active and how many of them are not
// checks.
func (w *worker) loadLocked() (all, nonCheck int) {
	for _, active := range w.running {
		if active.kind != KindCheck {
			nonCheck++
		}
	}
	return len(w.running) + len(w.sources), nonCheck + len(w.sources)
}

// passScripts records unrecorded completions, schedules due watches, kills
// cancelled runs, and, while a slot is free, starts pending runs, checks first.
func (w *worker) passScripts(ctx context.Context) {
	w.recordUnrecorded(ctx)
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
		if active != nil && active.cancel != nil && !active.cancelRequested.Swap(true) {
			w.log.Info("cancel workflow script run", "run_id", run.ID, "task", run.TaskKey, "script", run.Script)
			active.cancel()
		}
	}
	w.mu.Lock()
	all, _ := w.loadLocked()
	free := all < w.parallel
	w.mu.Unlock()
	if !free {
		return
	}
	jobs, err := w.s.Jobs.PendingRunJobs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("list pending workflow script runs", "err", err)
		}
		return
	}
	// A check holds up an agent waiting for its request; a watch can wait.
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].Run.Kind == KindCheck && jobs[j].Run.Kind != KindCheck })
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		all, watches := w.loadLocked()
		full := all >= w.parallel
		_, running := w.running[job.Run.ID]
		busy := w.busy[job.Run.TaskKey]
		w.mu.Unlock()
		if full {
			return
		}
		// With more than one slot, one is always left for a check.
		if running || busy || (job.Run.Kind != KindCheck && w.parallel > 1 && watches >= w.parallel-1) {
			continue
		}
		w.start(ctx, job)
	}
}

// start claims one job and executes it in the background. A job that cannot
// be prepared is claimed with no log path and completed in the background as a
// failure without running anything.
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

	runCtx, cancel := context.WithCancel(ctx)
	active := &activeRun{task: job.Run.TaskKey, kind: job.Run.Kind, cancel: cancel}
	w.mu.Lock()
	w.running[id] = active
	w.busy[active.task] = true
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer cancel()
		if reason == "" {
			for _, dir := range []string{spec.TaskDir, spec.RunDir} {
				if err := ownerDir(dir); err != nil {
					reason = fmt.Sprintf("cannot create directory %s: %v", dir, err)
					break
				}
			}
		}
		if reason != "" {
			reason = tasks.RedactSecrets(reason, secretValues(job.QueueSecrets))
			w.log.Warn("workflow script run failed before it started", "run_id", id, "task", job.Run.TaskKey,
				"script", job.Run.Script, "reason", reason)
			w.finish(ctx, id, job, tasks.RunCompletion{
				Verdict: VerdictFailure, Message: reason,
				FinishedAt: w.clock().UTC().Format(time.RFC3339Nano),
			}, "", false)
			return
		}
		spec.OnStart = func(pid int) {
			if err := w.s.Jobs.SetScriptRunPID(ctx, id, pid); err != nil && ctx.Err() == nil {
				w.log.Error("record workflow script pid", "run_id", id, "task", job.Run.TaskKey, "err", err)
			}
		}
		w.log.Info("workflow script started", "run_id", id, "task", job.Run.TaskKey, "kind", job.Run.Kind,
			"script", job.Run.Script, "cwd", spec.Cwd)
		res := Execute(runCtx, spec)
		cancel()
		attrs := []any{"run_id", id, "task", job.Run.TaskKey, "script", job.Run.Script,
			"verdict", res.Verdict.Kind, "exit_code", exitAttr(res.ExitCode),
			"duration", res.Finished.Sub(res.Started).Round(time.Millisecond)}
		if ctx.Err() != nil {
			w.log.Info("workflow script killed at shutdown; left for recovery", attrs...)
			w.forget(id, active.task)
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
		w.finish(ctx, id, job, done, spec.RunDir, active.cancelRequested.Load())
	}()
}

// finish records a finished run and frees its slot. When the completion fails
// the run is kept for the next pass and its task stays busy, so the next run of
// the task cannot start before the engine has seen this one. cancelled says
// the run was cancelled while it ran.
func (w *worker) finish(ctx context.Context, id int64, job tasks.RunJob, done tasks.RunCompletion, runDir string, cancelled bool) {
	err := w.complete(ctx, id, done)
	w.mu.Lock()
	delete(w.running, id)
	if err != nil && ctx.Err() == nil {
		w.retry[id] = unrecorded{job: job, done: done, runDir: runDir, cancelled: cancelled}
		w.mu.Unlock()
		w.log.Error("record workflow script run; trying again on the next pass", "run_id", id,
			"task", job.Run.TaskKey, "err", err)
		return
	}
	delete(w.busy, job.Run.TaskKey)
	stale := ""
	if err == nil {
		stale = w.rememberQuietLocked(job, done, runDir, cancelled)
	}
	w.mu.Unlock()
	w.removeRunDir(stale)
	w.notify()
}

// recordUnrecorded tries the kept completions once more. It stops at the first
// failure: the store is likely still failing, and each attempt may wait its
// full timeout; the next pass tries again.
func (w *worker) recordUnrecorded(ctx context.Context) {
	w.mu.Lock()
	pending := make(map[int64]unrecorded, len(w.retry))
	for id, entry := range w.retry {
		pending[id] = entry
	}
	w.mu.Unlock()
	for id, entry := range pending {
		if ctx.Err() != nil {
			return
		}
		if err := w.complete(ctx, id, entry.done); err != nil {
			w.log.Error("record workflow script run", "run_id", id, "task", entry.job.Run.TaskKey, "err", err)
			return
		}
		w.mu.Lock()
		delete(w.retry, id)
		delete(w.busy, entry.job.Run.TaskKey)
		stale := w.rememberQuietLocked(entry.job, entry.done, entry.runDir, entry.cancelled)
		w.mu.Unlock()
		w.removeRunDir(stale)
	}
}

// complete makes one attempt to record a finished run. A run the engine no
// longer has counts as recorded.
func (w *worker) complete(ctx context.Context, id int64, done tasks.RunCompletion) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
	defer cancel()
	err := w.s.Jobs.CompleteScriptRun(cctx, id, done)
	if tasks.ErrorCode(err) == "run_not_found" {
		return nil
	}
	return err
}

// keptQuietRunDirs is how many quiet watch runs of a task keep their files,
// matching the quiet run rows the engine keeps (tasks.workflowViewRuns).
const keptQuietRunDirs = 20

// rememberQuietLocked keeps the files of only the newest quiet watch runs of a
// task: it adds runDir of a quiet run to the task's ring and returns the
// directory the ring evicted, now stale, or "". A run cancelled while it ran
// is recorded as cancelled, not as quiet, so it is never remembered and its
// files stay. Other runs keep their files, and so do directories left from
// before a restart: nightly retention removes the task directory.
func (w *worker) rememberQuietLocked(job tasks.RunJob, done tasks.RunCompletion, runDir string, cancelled bool) string {
	if job.Run.Kind != KindWatch || done.Verdict != VerdictQuiet || runDir == "" || cancelled {
		return ""
	}
	return w.ringLocked(job.Run.TaskKey, runDir)
}

// ringLocked adds runDir to the ring of quiet run directories under key and
// returns the directory the ring evicted, or "".
func (w *worker) ringLocked(key, runDir string) string {
	ring := w.quietDirs[key]
	if slices.Contains(ring, runDir) {
		return ""
	}
	ring = append(ring, runDir)
	stale := ""
	if len(ring) > keptQuietRunDirs {
		stale, ring = ring[0], slices.Clone(ring[1:])
	}
	w.quietDirs[key] = ring
	return stale
}

func (w *worker) removeRunDir(dir string) {
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		w.log.Warn("remove the files of a quiet workflow watch run", "dir", dir, "err", err)
	}
}

// forget frees the slot of a run left for recovery at shutdown.
func (w *worker) forget(id int64, task string) {
	w.mu.Lock()
	delete(w.running, id)
	delete(w.busy, task)
	w.mu.Unlock()
	w.notify()
}

func (w *worker) notify() {
	select {
	case w.finished <- struct{}{}:
	default:
	}
}

// prepare builds the Spec of a job without touching the file system. A
// non-empty reason says why the job cannot run; it names no environment value.
func (w *worker) prepare(job tasks.RunJob) (Spec, string) {
	run := job.Run
	key := run.TaskKey
	if !tasks.IsTaskDirKey(key) {
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
	agentMode := run.Kind == KindCheck && run.RunAs == workflowfile.RunAsAgent
	if agentMode {
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
	// A queue run whose layers name no PATH still finds the system tools.
	if !agentMode && !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		env = append(env, defaultPath)
	}
	return Spec{
		RunID: strconv.FormatInt(run.ID, 10), Kind: run.Kind, ScriptPath: scriptPath, Cwd: cwd, Env: env,
		Timeout: job.Timeout, RunDir: runDir, TaskDir: stateDir, Snapshot: job.Snapshot,
		Declared: Declared{Outcomes: job.Outcomes, Artifacts: job.Artifacts},
	}, ""
}

// mergeEnv joins environment lists so that a later value replaces an earlier
// one of the same name, keeping the order of first appearance, and drops the
// variables in script.DaemonAccessEnv: a run never receives them, whatever its
// baseline holds.
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
		if !contains(script.DaemonAccessEnv, name) {
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

// redactCompletion replaces queue secret values in the message and in every
// artifact value with tasks.RedactSecrets. The outcome is matched against
// declared names and is left alone; the run log is owner-only and is not
// rewritten.
func redactCompletion(done tasks.RunCompletion, secrets map[string]string) tasks.RunCompletion {
	values := secretValues(secrets)
	done.Message = tasks.RedactSecrets(done.Message, values)
	if done.Artifacts != nil {
		artifacts := make(map[string]string, len(done.Artifacts))
		for name, value := range done.Artifacts {
			artifacts[name] = tasks.RedactSecrets(value, values)
		}
		done.Artifacts = artifacts
	}
	return done
}

// secretValues lists the values of a secret map.
func secretValues(secrets map[string]string) []string {
	values := make([]string, 0, len(secrets))
	for _, value := range secrets {
		values = append(values, value)
	}
	return values
}

func exitAttr(code *int) any {
	if code == nil {
		return nil
	}
	return *code
}
