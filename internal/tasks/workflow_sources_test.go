package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

const sourceScript = "./scripts/prs.sh"

// sourceDefinition is engineDefinition with the artifact pull_request and the
// source pull-requests.
func sourceDefinition() workflowfile.File {
	def := engineDefinition()
	def.Artifacts = []workflowfile.Artifact{{Name: "pull_request"}}
	def.Sources = []workflowfile.Source{{Name: "pull-requests", Script: sourceScript, Every: "2m", Timeout: "30s"}}
	return def
}

// sourceFixture binds DEV to sourceDefinition with eligible pools.
func sourceFixture(t *testing.T) (*Service, Actor) {
	t.Helper()
	svc, actor := workflowFixture(t)
	ctx := context.Background()
	if err := svc.EnsureDefaultQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE agents SET enabled = 1, loop_enabled = 1, goal_enabled = 1`); err != nil {
		t.Fatal(err)
	}
	mustRebindPool(t, svc, actor, "developers", []string{"dev-1", "dev-2"}, 0)
	mustRebindPool(t, svc, actor, "reviewers", []string{"reviewer-1"}, 0)
	seedEngineImage(t, svc, sourceDefinition())
	if _, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "development:0.1.0", 0); err != nil {
		t.Fatal(err)
	}
	return svc, actor
}

func sourceJobs(t *testing.T, svc *Service, now time.Time) []SourceJob {
	t.Helper()
	jobs, err := svc.QueueSourceJobs(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func startSource(t *testing.T, svc *Service, job SourceJob, at time.Time) int64 {
	t.Helper()
	id, err := svc.StartSourceRun(context.Background(), job, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func completeSource(t *testing.T, svc *Service, id int64, done SourceCompletion) {
	t.Helper()
	if err := svc.CompleteSourceRun(context.Background(), id, done); err != nil {
		t.Fatal(err)
	}
}

func itemsDone(at time.Time, items ...SourceItem) SourceCompletion {
	return SourceCompletion{Verdict: verdictItems, Items: items, FinishedAt: at.UTC().Format(time.RFC3339Nano)}
}

func devTasks(t *testing.T, svc *Service) []Task {
	t.Helper()
	rows, err := svc.db.Query(`SELECT task_key FROM tasks WHERE queue_prefix = 'DEV' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	out := make([]Task, 0, len(keys))
	for _, key := range keys {
		out = append(out, storedTask(t, svc, key))
	}
	return out
}

func sourceRunRow(t *testing.T, svc *Service, id int64) (state, verdict, message string, created int) {
	t.Helper()
	if err := svc.db.QueryRow(`SELECT state, verdict, message, tasks_created FROM task_queue_source_runs WHERE id = ?`, id).
		Scan(&state, &verdict, &message, &created); err != nil {
		t.Fatal(err)
	}
	return
}

func TestSourceJobsScheduleOneRunAtATime(t *testing.T) {
	svc, _ := sourceFixture(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	jobs := sourceJobs(t, svc, now)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	job := jobs[0]
	if !job.Due || job.Queue != "DEV" || job.Source != "pull-requests" || job.Script != sourceScript ||
		job.Timeout != 30*time.Second || job.WorkflowName != "development" || len(job.Artifacts) != 1 {
		t.Fatalf("job = %+v", job)
	}
	id := startSource(t, svc, job, now)
	if jobs := sourceJobs(t, svc, now.Add(time.Hour)); len(jobs) != 1 || jobs[0].Due {
		t.Fatalf("a running source is due again: %+v", jobs)
	}
	if _, err := svc.StartSourceRun(context.Background(), job, now.Format(time.RFC3339Nano)); err == nil {
		t.Fatal("a second run of a running source started")
	}
	completeSource(t, svc, id, SourceCompletion{Verdict: verdictQuiet, FinishedAt: now.Add(time.Second).Format(time.RFC3339Nano)})
	if jobs := sourceJobs(t, svc, now.Add(2*time.Minute)); jobs[0].Due {
		t.Fatal("due before every has passed since the run finished")
	}
	if jobs := sourceJobs(t, svc, now.Add(2*time.Minute+time.Second)); !jobs[0].Due {
		t.Fatal("not due after every")
	}
}

func TestSourceItemsCreateOneTaskPerKey(t *testing.T) {
	svc, _ := sourceFixture(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	job := sourceJobs(t, svc, now)[0]
	id := startSource(t, svc, job, now)
	completeSource(t, svc, id, itemsDone(now,
		SourceItem{Key: "org/repo#42@abc", Title: "Review org/repo#42", Description: "Look at it.", Priority: PriorityP1,
			Artifacts: map[string]string{"pull_request": "https://github.com/org/repo/pull/42"}},
		SourceItem{Key: "org/repo#43@def", Title: "Review org/repo#43"},
	))
	created := devTasks(t, svc)
	if len(created) != 2 {
		t.Fatalf("tasks = %+v", created)
	}
	first := created[0]
	if first.Title != "Review org/repo#42" || first.Priority != PriorityP1 || first.WorkflowStatus != "develop" ||
		!strings.HasPrefix(first.Assignee, "agent:dev-") || first.Author != "user:customer" {
		t.Fatalf("first task = %+v", first)
	}
	if !strings.Contains(first.Description, "Look at it.") || !strings.Contains(first.Description, "pull-requests") ||
		!strings.Contains(first.Description, "org/repo#42@abc") {
		t.Fatalf("description = %q", first.Description)
	}
	artifacts, err := currentArtifactsTx(context.Background(), svc.db, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Value != "https://github.com/org/repo/pull/42" || artifacts[0].Author != "script:"+sourceScript {
		t.Fatalf("artifacts = %+v", artifacts)
	}
	if state, verdict, _, n := sourceRunRow(t, svc, id); state != "finished" || verdict != verdictItems || n != 2 {
		t.Fatalf("run = %s %s %d", state, verdict, n)
	}

	// A repeated key creates nothing, even after its task is gone.
	for _, stmt := range []string{`PRAGMA foreign_keys = OFF`, `DELETE FROM tasks WHERE id = ?`, `PRAGMA foreign_keys = ON`} {
		var args []any
		if strings.HasPrefix(stmt, "DELETE") {
			args = append(args, first.ID)
		}
		if _, err := svc.db.Exec(stmt, args...); err != nil {
			t.Fatal(err)
		}
	}
	later := now.Add(5 * time.Minute)
	id = startSource(t, svc, sourceJobs(t, svc, later)[0], later)
	completeSource(t, svc, id, itemsDone(later,
		SourceItem{Key: "org/repo#42@abc", Title: "Review org/repo#42"},
		SourceItem{Key: "org/repo#43@def", Title: "Review org/repo#43"},
		SourceItem{Key: "org/repo#44@123", Title: "Review org/repo#44"},
	))
	if got := devTasks(t, svc); len(got) != 2 || got[1].Title != "Review org/repo#44" {
		t.Fatalf("tasks after the repeat = %+v", got)
	}
	if _, _, _, n := sourceRunRow(t, svc, id); n != 1 {
		t.Fatalf("tasks_created = %d", n)
	}
}

func TestSourceRunWithAnInvalidItemCreatesNothing(t *testing.T) {
	svc, _ := sourceFixture(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	id := startSource(t, svc, sourceJobs(t, svc, now)[0], now)
	completeSource(t, svc, id, itemsDone(now,
		SourceItem{Key: "a", Title: "fine"},
		SourceItem{Key: "b", Title: "bad", Artifacts: map[string]string{"plan": "x"}},
	))
	if got := devTasks(t, svc); len(got) != 0 {
		t.Fatalf("tasks = %+v", got)
	}
	if state, verdict, message, _ := sourceRunRow(t, svc, id); state != "finished" || verdict != verdictFailure || !strings.Contains(message, "plan") {
		t.Fatalf("run = %s %s %q", state, verdict, message)
	}
	var events int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM task_events WHERE kind = 'queue.source_failed' AND queue_prefix = 'DEV'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("queue.source_failed events = %d", events)
	}
	// The key of the rejected run is still new.
	later := now.Add(time.Hour)
	id = startSource(t, svc, sourceJobs(t, svc, later)[0], later)
	completeSource(t, svc, id, itemsDone(later, SourceItem{Key: "a", Title: "fine"}))
	if got := devTasks(t, svc); len(got) != 1 {
		t.Fatalf("tasks = %+v", got)
	}
}

func TestSourceStopsWhenTheQueueIsUnbound(t *testing.T) {
	svc, actor := sourceFixture(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	id := startSource(t, svc, sourceJobs(t, svc, now)[0], now)
	binding, err := svc.GetQueueWorkflow(context.Background(), actor, "DEV")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ClearQueueWorkflow(context.Background(), actor, "DEV", binding.Revision); err != nil {
		t.Fatal(err)
	}
	if jobs := sourceJobs(t, svc, now.Add(time.Hour)); len(jobs) != 0 {
		t.Fatalf("jobs of an unbound queue = %+v", jobs)
	}
	completeSource(t, svc, id, itemsDone(now, SourceItem{Key: "a", Title: "late"}))
	if got := devTasks(t, svc); len(got) != 0 {
		t.Fatalf("tasks = %+v", got)
	}
	if state, _, _, _ := sourceRunRow(t, svc, id); state != "cancelled" {
		t.Fatalf("state = %s", state)
	}
}

func TestRecoverSourceRunsInterruptsRunningRuns(t *testing.T) {
	svc, _ := sourceFixture(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	id := startSource(t, svc, sourceJobs(t, svc, now)[0], now)
	if err := svc.SetSourceRunPID(context.Background(), id, 4242); err != nil {
		t.Fatal(err)
	}
	running, err := svc.RunningSourceRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].PID == nil || *running[0].PID != 4242 || running[0].Queue != "DEV" || running[0].Source != "pull-requests" {
		t.Fatalf("running = %+v", running)
	}
	if err := svc.RecoverSourceRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, _, _, _ := sourceRunRow(t, svc, id); state != "interrupted" {
		t.Fatalf("state = %s", state)
	}
	// Completing an interrupted run changes nothing.
	completeSource(t, svc, id, itemsDone(now, SourceItem{Key: "a", Title: "late"}))
	if got := devTasks(t, svc); len(got) != 0 {
		t.Fatalf("tasks = %+v", got)
	}
}

func TestSourceKeepsOnlyTheNewestQuietRuns(t *testing.T) {
	svc, _ := sourceFixture(t)
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < workflowViewRuns+5; i++ {
		id := startSource(t, svc, sourceJobs(t, svc, at)[0], at)
		completeSource(t, svc, id, SourceCompletion{Verdict: verdictQuiet, FinishedAt: at.Format(time.RFC3339Nano)})
		at = at.Add(time.Hour)
	}
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM task_queue_source_runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != workflowViewRuns {
		t.Fatalf("kept %d quiet runs", n)
	}
}

func TestListQueueSourcesAndLog(t *testing.T) {
	svc, actor := sourceFixture(t)
	base := t.TempDir()
	svc.SetRunBaseDir(base)
	svc.db.Exec(`INSERT INTO task_queue_secrets(queue_prefix, key, value, updated_at) VALUES ('DEV', 'GH_TOKEN', 'secret-value-1', '')`)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	job := sourceJobs(t, svc, now)[0]
	for i := 0; i < 2; i++ {
		at := now.Add(time.Duration(i) * time.Hour)
		id := startSource(t, svc, job, at)
		logPath := SourceRunDir(base, "DEV", "pull-requests", id) + "/run.log"
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, []byte("token secret-value-1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		completeSource(t, svc, id, SourceCompletion{Verdict: verdictFailure, Message: "boom " + strconv.Itoa(i),
			LogPath: logPath, FinishedAt: at.Format(time.RFC3339Nano)})
	}
	if _, err := svc.ListQueueSources(context.Background(), AgentActor("dev-1"), "DEV"); ErrorCode(err) != "forbidden" {
		t.Fatalf("agent list error = %v", err)
	}
	sources, err := svc.ListQueueSources(context.Background(), actor, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %+v", sources)
	}
	src := sources[0]
	if src.Name != "pull-requests" || src.Script != sourceScript || src.Every != "2m" || src.Failures != 2 ||
		src.LastRun == nil || src.LastRun.Message != "boom 1" || src.NextRunAt != now.Add(time.Hour+2*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("source = %+v (last %+v)", src, src.LastRun)
	}
	if _, _, err := svc.QueueSourceRunLog(context.Background(), AgentActor("dev-1"), "DEV", src.LastRun.ID, 0); ErrorCode(err) != "forbidden" {
		t.Fatalf("agent log error = %v", err)
	}
	text, _, err := svc.QueueSourceRunLog(context.Background(), actor, "DEV", src.LastRun.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "secret-value-1") || !strings.Contains(text, "[redacted]") {
		t.Fatalf("log = %q", text)
	}
	if _, _, err := svc.QueueSourceRunLog(context.Background(), actor, "DEV", 999, 0); ErrorCode(err) != "run_not_found" {
		t.Fatalf("missing run error = %v", err)
	}
}
