package workflowrun

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/tasks"
)

// SourceJobs is the part of the task service that runs workflow sources.
type SourceJobs interface {
	QueueSourceJobs(ctx context.Context, now time.Time) ([]tasks.SourceJob, error)
	StartSourceRun(ctx context.Context, job tasks.SourceJob, startedAt string) (int64, error)
	SetSourceRunPID(ctx context.Context, id int64, pid int) error
	CompleteSourceRun(ctx context.Context, id int64, done tasks.SourceCompletion) error
	RunningSourceRuns(ctx context.Context) ([]tasks.SourceRun, error)
	RecoverSourceRuns(ctx context.Context) error
}

// sourceNames are the TARIBOY_* names only a source run receives; with
// protocolNames they are stripped from every layer of its environment.
var sourceNames = []string{"TARIBOY_SOURCE_NAME", "TARIBOY_SOURCE_DIR"}

// unrecordedSource is a finished source run whose completion failed.
type unrecordedSource struct {
	key  string
	done tasks.SourceCompletion
}

// SourceEnv returns the TARIBOY_* entries of one source run, sorted by name.
// A source run has no task, so it gets no task key, status, or snapshot.
func SourceEnv(job tasks.SourceJob, workflowDir, sourceDir, resultFile string) []string {
	return []string{
		"TARIBOY_QUIET_EXIT=" + strconv.Itoa(script.QuietExit),
		"TARIBOY_RESULT_FILE=" + resultFile,
		"TARIBOY_SOURCE_DIR=" + sourceDir,
		"TARIBOY_SOURCE_NAME=" + job.Source,
		"TARIBOY_TASK_QUEUE=" + job.Queue,
		"TARIBOY_WORKFLOW_DIR=" + workflowDir,
		"TARIBOY_WORKFLOW_NAME=" + job.WorkflowName,
		"TARIBOY_WORKFLOW_VERSION=" + job.WorkflowVersion,
	}
}

// passSources starts the due sources while a slot is free, and kills a running
// source the queue's workflow no longer declares.
func (w *worker) passSources(ctx context.Context) {
	if w.s.Sources == nil {
		return
	}
	w.recordUnrecordedSources(ctx)
	jobs, err := w.s.Sources.QueueSourceJobs(ctx, w.clock())
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("list workflow sources", "err", err)
		}
		return
	}
	live := map[string]bool{}
	for _, job := range jobs {
		live[job.Key()] = true
	}
	w.mu.Lock()
	for key, active := range w.sources {
		if !live[key] && !active.cancelRequested.Swap(true) {
			w.log.Info("stop a workflow source the queue no longer binds", "source", key)
			active.cancel()
		}
	}
	w.mu.Unlock()
	for _, job := range jobs {
		if !job.Due || ctx.Err() != nil {
			continue
		}
		w.mu.Lock()
		all, nonCheck := w.loadLocked()
		_, active := w.sources[job.Key()]
		w.mu.Unlock()
		// Like a watch, a source leaves one slot for a check.
		if all >= w.parallel || (w.parallel > 1 && nonCheck >= w.parallel-1) {
			return
		}
		if !active {
			w.startSource(ctx, job)
		}
	}
}

// startSource records a running run of job and executes it in the background.
func (w *worker) startSource(ctx context.Context, job tasks.SourceJob) {
	key := job.Key()
	id, err := w.s.Sources.StartSourceRun(ctx, job, w.clock().UTC().Format(time.RFC3339Nano))
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("start workflow source run", "source", key, "err", err)
		}
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	active := &activeRun{task: key, kind: KindSource, cancel: cancel}
	w.mu.Lock()
	w.sources[key] = active
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer cancel()
		spec, reason := w.prepareSource(job, id)
		if reason != "" {
			reason = tasks.RedactSecrets(reason, secretValues(job.QueueSecrets))
			w.log.Warn("workflow source run failed before it started", "run_id", id, "source", key, "reason", reason)
			w.finishSource(ctx, id, key, tasks.SourceCompletion{
				Verdict: VerdictFailure, Message: reason, FinishedAt: w.clock().UTC().Format(time.RFC3339Nano),
			}, "", false)
			return
		}
		spec.OnStart = func(pid int) {
			if err := w.s.Sources.SetSourceRunPID(ctx, id, pid); err != nil && ctx.Err() == nil {
				w.log.Error("record workflow source pid", "run_id", id, "source", key, "err", err)
			}
		}
		w.log.Info("workflow source started", "run_id", id, "source", key, "script", job.Script)
		res := Execute(runCtx, spec)
		cancel()
		if ctx.Err() != nil {
			w.log.Info("workflow source killed at shutdown; left for recovery", "run_id", id, "source", key)
			w.mu.Lock()
			delete(w.sources, key)
			w.mu.Unlock()
			w.notify()
			return
		}
		w.log.Info("workflow source finished", "run_id", id, "source", key, "verdict", res.Verdict.Kind,
			"items", len(res.Verdict.Items), "exit_code", exitAttr(res.ExitCode), "log", res.LogPath)
		done := redactSourceCompletion(tasks.SourceCompletion{
			Verdict: res.Verdict.Kind, Message: res.Verdict.Message, Items: res.Verdict.Items,
			ExitCode: res.ExitCode, LogPath: res.LogPath, FinishedAt: w.clock().UTC().Format(time.RFC3339Nano),
		}, job.QueueSecrets)
		w.finishSource(ctx, id, key, done, spec.RunDir, active.cancelRequested.Load())
	}()
}

// finishSource records a finished source run and frees its slot; a completion
// that fails is kept and tried again on the next pass.
func (w *worker) finishSource(ctx context.Context, id int64, key string, done tasks.SourceCompletion, runDir string, cancelled bool) {
	err := w.completeSource(ctx, id, done)
	w.mu.Lock()
	delete(w.sources, key)
	if err != nil && ctx.Err() == nil {
		w.sourceRetry[id] = unrecordedSource{key: key, done: done}
		w.mu.Unlock()
		w.log.Error("record workflow source run; trying again on the next pass", "run_id", id, "source", key, "err", err)
		return
	}
	stale := ""
	if err == nil && done.Verdict == VerdictQuiet && !cancelled && runDir != "" {
		stale = w.ringLocked("source:"+key, runDir)
	}
	w.mu.Unlock()
	w.removeRunDir(stale)
	w.notify()
}

// recordUnrecordedSources tries the kept source completions once more and
// stops at the first failure.
func (w *worker) recordUnrecordedSources(ctx context.Context) {
	w.mu.Lock()
	pending := make(map[int64]unrecordedSource, len(w.sourceRetry))
	for id, entry := range w.sourceRetry {
		pending[id] = entry
	}
	w.mu.Unlock()
	for id, entry := range pending {
		if err := w.completeSource(ctx, id, entry.done); err != nil {
			w.log.Error("record workflow source run", "run_id", id, "source", entry.key, "err", err)
			return
		}
		w.mu.Lock()
		delete(w.sourceRetry, id)
		w.mu.Unlock()
	}
}

// completeSource makes one attempt to record a finished source run. A run the
// service no longer has counts as recorded.
func (w *worker) completeSource(ctx context.Context, id int64, done tasks.SourceCompletion) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
	defer cancel()
	err := w.s.Sources.CompleteSourceRun(cctx, id, done)
	if tasks.ErrorCode(err) == "run_not_found" {
		return nil
	}
	return err
}

// prepareSource builds the Spec of a source run without touching the file
// system. A non-empty reason says why it cannot run; it names no environment
// value.
func (w *worker) prepareSource(job tasks.SourceJob, id int64) (Spec, string) {
	if !tasks.IsTaskDirKey(job.Queue) || !tasks.IsTaskDirKey(job.Source) {
		return Spec{}, fmt.Sprintf("queue %q or source %q is not a valid directory name", job.Queue, job.Source)
	}
	base, err := filepath.Abs(w.s.BaseDir)
	if err != nil {
		return Spec{}, fmt.Sprintf("cannot resolve the base directory: %v", err)
	}
	if w.s.Images == nil {
		return Spec{}, "no workflow image store is configured"
	}
	scriptPath, err := w.s.Images.FilePath(job.WorkflowName, job.WorkflowDigest, job.Script)
	if err != nil {
		return Spec{}, fmt.Sprintf("script %s of workflow %s %s is not usable: %v",
			job.Script, job.WorkflowName, job.WorkflowVersion, err)
	}
	stateDir := filepath.Join(tasks.SourceDir(base, job.Queue, job.Source), "state")
	runDir := tasks.SourceRunDir(base, job.Queue, job.Source, id)
	var baseline []string
	if w.s.BaseEnv != nil {
		baseline = w.s.BaseEnv()
	}
	reserved := append(slices.Clone(protocolNames), sourceNames...)
	env := mergeEnv(
		withoutNames(baseline, reserved),
		withoutNames(mapEnv(job.WorkflowEnv), reserved),
		withoutNames(mapEnv(job.QueueSecrets), reserved),
		SourceEnv(job, w.s.Images.ContentDir(job.WorkflowName, job.WorkflowDigest), stateDir, filepath.Join(runDir, "result.json")),
	)
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		env = append(env, defaultPath)
	}
	return Spec{
		RunID: strconv.FormatInt(id, 10), Kind: KindSource, ScriptPath: scriptPath, Cwd: stateDir, Env: env,
		Timeout: job.Timeout, RunDir: runDir, TaskDir: stateDir,
		Declared: Declared{Artifacts: job.Artifacts},
	}, ""
}

// redactSourceCompletion replaces queue secret values in the message and in
// every item's title, description, and artifact values. An item key holding a
// secret fails the run: a key is stored and shown as it is.
func redactSourceCompletion(done tasks.SourceCompletion, secrets map[string]string) tasks.SourceCompletion {
	values := secretValues(secrets)
	done.Message = tasks.RedactSecrets(done.Message, values)
	items := make([]tasks.SourceItem, 0, len(done.Items))
	for i, item := range done.Items {
		if tasks.RedactSecrets(item.Key, values) != item.Key {
			done.Verdict, done.Items = VerdictFailure, nil
			done.Message = fmt.Sprintf("item %d: the key holds a queue secret", i)
			return done
		}
		item.Title = tasks.RedactSecrets(item.Title, values)
		item.Description = tasks.RedactSecrets(item.Description, values)
		if item.Artifacts != nil {
			artifacts := make(map[string]string, len(item.Artifacts))
			for name, value := range item.Artifacts {
				artifacts[name] = tasks.RedactSecrets(value, values)
			}
			item.Artifacts = artifacts
		}
		items = append(items, item)
	}
	if done.Items != nil {
		done.Items = items
	}
	return done
}
