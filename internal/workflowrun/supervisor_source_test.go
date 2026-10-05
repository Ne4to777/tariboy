package workflowrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/tasks"
)

// fakeSources is an in-memory SourceJobs. A due job runs once: starting it
// clears its Due flag.
type fakeSources struct {
	mu        sync.Mutex
	jobs      []tasks.SourceJob
	nextID    int64
	pids      map[int64]int
	done      map[int64]tasks.SourceCompletion
	completed chan int64
	started   chan int64
	recovered bool
	early     bool // a run started before RecoverSourceRuns
}

func newFakeSources(jobs ...tasks.SourceJob) *fakeSources {
	return &fakeSources{jobs: jobs, pids: map[int64]int{}, done: map[int64]tasks.SourceCompletion{},
		completed: make(chan int64, 16), started: make(chan int64, 16)}
}

func (f *fakeSources) setJobs(jobs ...tasks.SourceJob) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = jobs
}

func (f *fakeSources) QueueSourceJobs(context.Context, time.Time) ([]tasks.SourceJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tasks.SourceJob(nil), f.jobs...), nil
}

func (f *fakeSources) StartSourceRun(_ context.Context, job tasks.SourceJob, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.early = f.early || !f.recovered
	for i := range f.jobs {
		if f.jobs[i].Key() == job.Key() {
			f.jobs[i].Due = false
		}
	}
	f.nextID++
	return f.nextID, nil
}

func (f *fakeSources) SetSourceRunPID(_ context.Context, id int64, pid int) error {
	f.mu.Lock()
	f.pids[id] = pid
	f.mu.Unlock()
	f.started <- id
	return nil
}

func (f *fakeSources) CompleteSourceRun(_ context.Context, id int64, done tasks.SourceCompletion) error {
	f.mu.Lock()
	f.done[id] = done
	f.mu.Unlock()
	f.completed <- id
	return nil
}

func (f *fakeSources) RunningSourceRuns(context.Context) ([]tasks.SourceRun, error) { return nil, nil }

func (f *fakeSources) RecoverSourceRuns(context.Context) error {
	f.mu.Lock()
	f.recovered = true
	f.mu.Unlock()
	return nil
}

func (f *fakeSources) await(t *testing.T, id int64) tasks.SourceCompletion {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case got := <-f.completed:
			if got == id {
				f.mu.Lock()
				defer f.mu.Unlock()
				return f.done[id]
			}
		case <-deadline:
			t.Fatalf("source run %d did not complete", id)
		}
	}
}

func sourceJob(h *harness, script string) tasks.SourceJob {
	return tasks.SourceJob{
		Queue: "DEV", Source: "pull-requests", Script: script,
		WorkflowName: h.image.Name, WorkflowVersion: h.image.Version, WorkflowDigest: h.image.Digest,
		Timeout: 10 * time.Second, Due: true, Artifacts: []string{},
		WorkflowEnv:  map[string]string{"REPO": "org/repo"},
		QueueSecrets: map[string]string{"LONG_SECRET": "long-secret-value"},
	}
}

func TestSupervisorRunsASourceAndReportsItsItems(t *testing.T) {
	h := newHarness(t)
	sources := newFakeSources()
	sources.setJobs(sourceJob(h, "scripts/items.sh"))
	h.sup.Sources = sources
	h.start()
	done := sources.await(t, 1)
	if done.Verdict != VerdictItems || len(done.Items) != 1 {
		t.Fatalf("completion = %+v", done)
	}
	item := done.Items[0]
	if item.Key != "k-pull-requests" || item.Title != "t [redacted]" || item.Description != "DEV" {
		t.Fatalf("item = %+v", item)
	}
	if sources.early {
		t.Fatal("a source started before its runs were recovered")
	}
	dir := filepath.Join(h.base, "task-queues", "DEV", "sources", "pull-requests")
	if done.LogPath != filepath.Join(dir, "runs", "1", "run.log") {
		t.Fatalf("log path = %q", done.LogPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs", "1", "task.json")); !os.IsNotExist(err) {
		t.Fatalf("a source run has a task snapshot: %v", err)
	}
	pwd, err := os.ReadFile(filepath.Join(dir, "state", "pwd.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(pwd)) != filepath.Join(dir, "state") {
		t.Fatalf("cwd = %q", pwd)
	}
	env := envFile(t, filepath.Join(dir, "state", "env.txt"))
	for name, want := range map[string]string{
		"TARIBOY_TASK_QUEUE": "DEV", "TARIBOY_SOURCE_NAME": "pull-requests", "TARIBOY_SOURCE_DIR": filepath.Join(dir, "state"),
		"TARIBOY_RESULT_FILE": filepath.Join(dir, "runs", "1", "result.json"), "TARIBOY_WORKFLOW_NAME": h.image.Name,
		"TARIBOY_QUIET_EXIT": "111", "REPO": "org/repo", "LONG_SECRET": "long-secret-value", "BASE_ONLY": "base",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	for _, name := range []string{"TARIBOY_TASK_KEY", "TARIBOY_TASK_FILE", "TARIBOY_WORKFLOW_STATUS"} {
		if _, ok := env[name]; ok {
			t.Errorf("a source run received %s", name)
		}
	}
	noToolsSocket(t, env)
}

func TestSupervisorKillsASourceTheQueueNoLongerBinds(t *testing.T) {
	h := newHarness(t)
	sources := newFakeSources()
	sources.setJobs(sourceJob(h, "scripts/stubborn.sh"))
	h.sup.Sources = sources
	h.start()
	select {
	case <-sources.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the source did not start")
	}
	sources.setJobs()
	h.nudge()
	if done := sources.await(t, 1); done.Verdict != VerdictFailure || done.Message != "the run was cancelled" {
		t.Fatalf("completion = %+v", done)
	}
}

func TestRedactSourceCompletionFailsAKeyWithASecret(t *testing.T) {
	secrets := map[string]string{"TOKEN": "secret-token"}
	done := redactSourceCompletion(tasks.SourceCompletion{Verdict: VerdictItems, Items: []tasks.SourceItem{
		{Key: "ok", Title: "a secret-token", Description: "secret-token", Artifacts: map[string]string{"x": "secret-token"}},
	}}, secrets)
	item := done.Items[0]
	if done.Verdict != VerdictItems || item.Title != "a [redacted]" || item.Description != "[redacted]" || item.Artifacts["x"] != "[redacted]" {
		t.Fatalf("redacted = %+v", done)
	}
	done = redactSourceCompletion(tasks.SourceCompletion{Verdict: VerdictItems, Items: []tasks.SourceItem{
		{Key: "k-secret-token", Title: "t"},
	}}, secrets)
	if done.Verdict != VerdictFailure || len(done.Items) != 0 || strings.Contains(done.Message, "secret-token") {
		t.Fatalf("completion = %+v", done)
	}
}
