package tasks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// SourceItem is one item a workflow source script reported. Its key is what
// makes it new: the daemon creates one task per key and source of a queue.
type SourceItem struct {
	Key         string            `json:"key"`
	Title       string            `json:"title"`
	Description string            `json:"description,omitempty"`
	Priority    Priority          `json:"priority,omitempty"`
	Artifacts   map[string]string `json:"artifacts,omitempty"`
}

// SourceJob is one source of a bound queue, with what the worker needs to run
// it. Due says it may start now: no run of it is running and every has passed
// since the last one finished.
type SourceJob struct {
	Queue           string
	Source          string
	Script          string
	WorkflowName    string
	WorkflowVersion string
	WorkflowDigest  string
	Timeout         time.Duration
	Due             bool
	Artifacts       []string // declared artifact names
	// WorkflowEnv and QueueSecrets reach the process environment only; they are
	// never marshalled. QueueSecrets is read for a due job only.
	WorkflowEnv  map[string]string `json:"-"`
	QueueSecrets map[string]string `json:"-"`
}

// Key names the source of a queue for the worker's bookkeeping.
func (j SourceJob) Key() string { return j.Queue + "/" + j.Source }

// SourceCompletion is the result of a finished source run as the worker
// reports it. Verdict is items, quiet, or failure.
type SourceCompletion struct {
	Verdict    string
	Message    string
	Items      []SourceItem
	ExitCode   *int
	LogPath    string
	FinishedAt string
}

// SourceRun is the durable record of one source run.
type SourceRun struct {
	ID           int64  `json:"id"`
	Queue        string `json:"queue"`
	Source       string `json:"source"`
	Script       string `json:"script"`
	State        string `json:"state"`
	Verdict      string `json:"verdict,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	Message      string `json:"message,omitempty"`
	TasksCreated int    `json:"tasks_created"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at,omitempty"`
	LogPath      string `json:"log_path,omitempty"`
	// PID is the process of a running run, for the worker's recovery after a
	// restart; it is never marshalled.
	PID *int `json:"-"`
}

// QueueSource is the operator view of one source of a bound queue.
type QueueSource struct {
	Queue   string `json:"queue"`
	Name    string `json:"name"`
	Script  string `json:"script"`
	Every   string `json:"every"`
	Timeout string `json:"timeout,omitempty"`
	// NextRunAt is when the source runs next; "" while it runs, and the
	// current time when it has never run.
	NextRunAt string `json:"next_run_at,omitempty"`
	// Failures counts the failed and interrupted runs since the last good one.
	Failures int        `json:"failures"`
	LastRun  *SourceRun `json:"last_run,omitempty"`
}

// SourceRunDir is the directory of one source run, which holds its log and
// result file; the worker and the log reader agree on it.
func SourceRunDir(base, queue, source string, id int64) string {
	return filepath.Join(SourceDir(base, queue, source), "runs", strconv.FormatInt(id, 10))
}

// SourceDir is the persistent directory of a source of a queue.
func SourceDir(base, queue, source string) string {
	return filepath.Join(base, "task-queues", queue, "sources", source)
}

const sourceRunSelect = `
	SELECT id, queue_prefix, source, script, state, verdict, exit_code, message, tasks_created,
	       started_at, finished_at, log_path, pid
	FROM task_queue_source_runs`

func scanSourceRun(row interface{ Scan(...any) error }) (SourceRun, error) {
	var r SourceRun
	var exit, pid sql.NullInt64
	if err := row.Scan(&r.ID, &r.Queue, &r.Source, &r.Script, &r.State, &r.Verdict, &exit, &r.Message, &r.TasksCreated,
		&r.StartedAt, &r.FinishedAt, &r.LogPath, &pid); err != nil {
		return SourceRun{}, err
	}
	if exit.Valid {
		code := int(exit.Int64)
		r.ExitCode = &code
	}
	if pid.Valid {
		p := int(pid.Int64)
		r.PID = &p
	}
	return r, nil
}

func querySourceRuns(ctx context.Context, q queryer, where string, args ...any) ([]SourceRun, error) {
	rows, err := q.QueryContext(ctx, sourceRunSelect+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceRun
	for rows.Next() {
		run, err := scanSourceRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func sourceRunNotFound(id int64) error {
	return domainError(http.StatusNotFound, "run_not_found", fmt.Sprintf("source run %d not found", id))
}

// boundSource is one source of a queue's bound workflow.
type boundSource struct {
	queue    string
	manifest workflowimage.Manifest
	source   workflowfile.Source
}

// boundSourcesTx lists the sources of every bound queue, or of one queue when
// queue is set, in queue and manifest order.
func boundSourcesTx(ctx context.Context, q queryer, queue string) ([]boundSource, error) {
	query := `SELECT queue_prefix, workflow_digest FROM task_queue_workflows`
	var args []any
	if queue != "" {
		query += ` WHERE queue_prefix = ?`
		args = append(args, queue)
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY queue_prefix`, args...)
	if err != nil {
		return nil, err
	}
	type binding struct{ queue, digest string }
	var bindings []binding
	for rows.Next() {
		var b binding
		if err := rows.Scan(&b.queue, &b.digest); err != nil {
			rows.Close()
			return nil, err
		}
		bindings = append(bindings, b)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []boundSource
	for _, b := range bindings {
		manifest, err := loadManifestTx(ctx, q, b.digest)
		if err != nil {
			return nil, err
		}
		for _, src := range manifest.Definition.Sources {
			out = append(out, boundSource{queue: b.queue, manifest: manifest, source: src})
		}
	}
	return out, nil
}

// lastSourceRunTx returns the newest run of a source of a queue; found is
// false when it never ran.
func lastSourceRunTx(ctx context.Context, q queryer, queue, source string) (SourceRun, bool, error) {
	runs, err := querySourceRuns(ctx, q, ` WHERE queue_prefix = ? AND source = ? ORDER BY id DESC LIMIT 1`, queue, source)
	if err != nil || len(runs) == 0 {
		return SourceRun{}, false, err
	}
	return runs[0], true, nil
}

// nextSourceRun is when a source with last as its newest run runs next: now
// when it never ran, the zero time while it runs, otherwise every after the
// last run finished.
func nextSourceRun(src workflowfile.Source, last SourceRun, found bool, now time.Time) time.Time {
	if !found {
		return now
	}
	if last.State == "running" {
		return time.Time{}
	}
	finished, err := time.Parse(time.RFC3339Nano, last.FinishedAt)
	if err != nil {
		return now
	}
	return finished.Add(scriptTimeout(src.Every, workflowfile.MinSourceEvery))
}

// QueueSourceJobs lists every source of every bound queue, due or not, so the
// worker also learns which running sources are no longer bound. It reads
// through s.db one query at a time: the database has a single connection.
func (s *Service) QueueSourceJobs(ctx context.Context, now time.Time) ([]SourceJob, error) {
	sources, err := boundSourcesTx(ctx, s.db, "")
	if err != nil {
		return nil, err
	}
	jobs := make([]SourceJob, 0, len(sources))
	for _, b := range sources {
		last, found, err := lastSourceRunTx(ctx, s.db, b.queue, b.source.Name)
		if err != nil {
			return nil, err
		}
		next := nextSourceRun(b.source, last, found, now)
		job := SourceJob{
			Queue: b.queue, Source: b.source.Name, Script: b.source.Script,
			WorkflowName: b.manifest.Name, WorkflowVersion: b.manifest.Version, WorkflowDigest: b.manifest.Digest,
			Timeout:     scriptTimeout(b.source.Timeout, workflowfile.DefaultWatchTimeout),
			Due:         !next.IsZero() && !next.After(now),
			WorkflowEnv: maps.Clone(b.manifest.Definition.Env), Artifacts: []string{},
		}
		if job.WorkflowEnv == nil {
			job.WorkflowEnv = map[string]string{}
		}
		for _, declared := range b.manifest.Definition.Artifacts {
			job.Artifacts = append(job.Artifacts, declared.Name)
		}
		if job.Due {
			if job.QueueSecrets, err = s.queueSecrets(ctx, b.queue); err != nil {
				return nil, err
			}
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// StartSourceRun records a running run of job and returns its id. A source
// that already has a running run is refused by the database.
func (s *Service) StartSourceRun(ctx context.Context, job SourceJob, startedAt string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO task_queue_source_runs(queue_prefix, source, script, workflow_digest, state, started_at)
		VALUES (?, ?, ?, ?, 'running', ?)`, job.Queue, job.Source, job.Script, job.WorkflowDigest, startedAt)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// SetSourceRunPID records the process of a running source run.
func (s *Service) SetSourceRunPID(ctx context.Context, id int64, pid int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE task_queue_source_runs SET pid = ? WHERE id = ? AND state = 'running'`, pid, id)
	return err
}

// RunningSourceRuns lists every running source run with its recorded PID; at
// daemon start the worker uses it to find the scripts a previous daemon left.
func (s *Service) RunningSourceRuns(ctx context.Context) ([]SourceRun, error) {
	return querySourceRuns(ctx, s.db, ` WHERE state = 'running' ORDER BY id`)
}

// RecoverSourceRuns records every running source run as interrupted; the source
// runs next after every, as after any finished run.
func (s *Service) RecoverSourceRuns(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE task_queue_source_runs SET state = 'interrupted', finished_at = ?,
			message = 'the daemon restarted during the run'
		WHERE state = 'running'`, s.now())
	return err
}

// CompleteSourceRun records a finished source run. For items it creates one
// task per key the source has not reported before, all in one transaction: a
// failure creates none, and a crash after the commit repeats none. A run whose
// source the queue's workflow no longer declares is recorded as cancelled and
// creates nothing. Completing a run that is no longer running is a no-op.
func (s *Service) CompleteSourceRun(ctx context.Context, id int64, done SourceCompletion) error {
	switch done.Verdict {
	case verdictItems, verdictQuiet, verdictFailure:
	default:
		return fmt.Errorf("source run %d: unknown verdict %q", id, done.Verdict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	runs, err := querySourceRuns(ctx, tx, ` WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return sourceRunNotFound(id)
	}
	run := runs[0]
	if run.State != "running" {
		return nil
	}
	finished := s.clock()
	if parsed, err := time.Parse(time.RFC3339Nano, done.FinishedAt); err == nil {
		finished = parsed
	}
	finishedAt := finished.UTC().Format(time.RFC3339Nano)
	var exit any
	if done.ExitCode != nil {
		exit = *done.ExitCode
	}
	state, verdict, message, created := "finished", done.Verdict, boundRunMessage(done.Message), 0

	bound, err := boundSourcesTx(ctx, tx, run.Queue)
	if err != nil {
		return err
	}
	var current *boundSource
	for i := range bound {
		if bound[i].source.Name == run.Source {
			current = &bound[i]
		}
	}
	switch {
	case current == nil:
		state = "cancelled"
	case verdict == verdictItems:
		created, err = s.createSourceTasksTx(ctx, tx, *current, run, done.Items)
		var invalid *invalidSourceItem
		if errors.As(err, &invalid) {
			verdict, message = verdictFailure, boundRunMessage(invalid.Error())
		} else if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_queue_source_runs SET state = ?, verdict = ?, exit_code = ?, message = ?, tasks_created = ?,
			finished_at = ?, log_path = ?
		WHERE id = ?`, state, verdict, exit, message, created, finishedAt, done.LogPath, id); err != nil {
		return err
	}
	if state == "finished" {
		switch verdict {
		case verdictQuiet:
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM task_queue_source_runs
				WHERE queue_prefix = ? AND source = ? AND state = 'finished' AND verdict = 'quiet' AND id NOT IN (
					SELECT id FROM task_queue_source_runs
					WHERE queue_prefix = ? AND source = ? AND state = 'finished' AND verdict = 'quiet' ORDER BY id DESC LIMIT ?)`,
				run.Queue, run.Source, run.Queue, run.Source, workflowViewRuns); err != nil {
				return err
			}
		case verdictFailure:
			revision, err := queueSecretRevision(ctx, tx, run.Queue)
			if err != nil {
				return err
			}
			if _, err := appendQueueEventTx(ctx, tx, Queue{Prefix: run.Queue, Revision: revision}, "queue.source_failed",
				Actor{Principal: workflowActor}, map[string]any{
					"run_id": run.ID, "source": run.Source, "script": run.Script, "exit_code": done.ExitCode,
					"message": message, "log_path": done.LogPath,
				}, s.now()); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if created > 0 {
		s.signal()
	}
	return nil
}

// invalidSourceItem is an item the queue's workflow cannot accept; it fails
// the whole run.
type invalidSourceItem struct{ err error }

func (e *invalidSourceItem) Error() string { return e.err.Error() }

// createSourceTasksTx creates the tasks of the items whose key is new, as the
// customer, and records each key. Every item is checked before any task is
// created.
func (s *Service) createSourceTasksTx(ctx context.Context, tx *sql.Tx, b boundSource, run SourceRun, items []SourceItem) (int, error) {
	for i, item := range items {
		if err := validateScriptArtifacts(b.manifest, item.Artifacts); err != nil {
			return 0, &invalidSourceItem{fmt.Errorf("item %d: %w", i, err)}
		}
		if _, err := NormalizePriority(item.Priority); err != nil || strings.TrimSpace(item.Title) == "" {
			return 0, &invalidSourceItem{fmt.Errorf("item %d: the title is empty or the priority is invalid", i)}
		}
	}
	actor := Actor{Principal: userPrincipal(s.customer), IsCustomer: true}
	author := scriptAuthor(run.Script)
	created := 0
	for _, item := range items {
		var known int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM task_queue_source_items WHERE queue_prefix = ? AND source = ? AND item_key = ?`,
			run.Queue, run.Source, item.Key).Scan(&known); err != nil {
			return 0, err
		}
		if known > 0 {
			continue
		}
		description := strings.TrimSpace(item.Description)
		if description != "" {
			description += "\n\n"
		}
		description += "Created by source " + CodeSpan(run.Source) + " for item " + CodeSpan(item.Key) + "."
		task, err := s.createTaskTx(ctx, tx, actor, CreateTaskInput{
			Queue: run.Queue, Title: item.Title, Description: description, Priority: item.Priority,
		})
		if err != nil {
			return 0, err
		}
		now := s.now()
		if _, err := appendEventTx(ctx, tx, task, "workflow.source_item", Actor{Principal: author}, map[string]any{
			"run_id": run.ID, "source": run.Source, "key": item.Key,
		}, now); err != nil {
			return 0, err
		}
		if err := storeScriptArtifactsTx(ctx, tx, task, b.manifest, run.Script, item.Artifacts, now); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_queue_source_items(queue_prefix, source, item_key, task_key, created_at) VALUES (?, ?, ?, ?, ?)`,
			run.Queue, run.Source, item.Key, task.Key, now); err != nil {
			return 0, err
		}
		created++
	}
	return created, nil
}

// ListQueueSources returns the sources of a queue's bound workflow with their
// schedule and newest run. Only the customer may read them.
func (s *Service) ListQueueSources(ctx context.Context, actor Actor, queue string) ([]QueueSource, error) {
	if err := s.requireWorkflowAdmin(actor); err != nil {
		return nil, err
	}
	queue = strings.ToUpper(strings.TrimSpace(queue))
	if err := requireQueueExists(ctx, s.db, queue); err != nil {
		return nil, err
	}
	bound, err := boundSourcesTx(ctx, s.db, queue)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	out := make([]QueueSource, 0, len(bound))
	for _, b := range bound {
		item := QueueSource{Queue: queue, Name: b.source.Name, Script: b.source.Script, Every: b.source.Every, Timeout: b.source.Timeout}
		recent, err := querySourceRuns(ctx, s.db, ` WHERE queue_prefix = ? AND source = ? ORDER BY id DESC LIMIT ?`,
			queue, b.source.Name, workflowViewRuns)
		if err != nil {
			return nil, err
		}
		var last SourceRun
		if len(recent) > 0 {
			last = recent[0]
			item.LastRun = &last
		}
		if next := nextSourceRun(b.source, last, len(recent) > 0, now); !next.IsZero() {
			item.NextRunAt = next.UTC().Format(time.RFC3339Nano)
		}
		for _, run := range recent {
			if run.State == "running" {
				continue
			}
			if run.State != "interrupted" && run.Verdict != verdictFailure {
				break
			}
			item.Failures++
		}
		out = append(out, item)
	}
	return out, nil
}

// QueueSourceRunLog returns the tail of a source run's log, as ScriptRunLog
// does for a task's run. Only the customer may read it.
func (s *Service) QueueSourceRunLog(ctx context.Context, actor Actor, queue string, id int64, maxBytes int) (string, bool, error) {
	if err := s.requireWorkflowAdmin(actor); err != nil {
		return "", false, err
	}
	queue = strings.ToUpper(strings.TrimSpace(queue))
	runs, err := querySourceRuns(ctx, s.db, ` WHERE queue_prefix = ? AND id = ?`, queue, id)
	if err != nil {
		return "", false, err
	}
	if len(runs) == 0 {
		return "", false, sourceRunNotFound(id)
	}
	run := runs[0]
	if run.LogPath == "" || !IsTaskDirKey(queue) || !IsTaskDirKey(run.Source) {
		return "", false, runLogUnavailable(id)
	}
	rel, err := filepath.Rel("/", SourceRunDir("/", queue, run.Source, id))
	if err != nil {
		return "", false, runLogInvalid(id)
	}
	path, err := s.verifiedLogPath(filepath.Join(rel, "run.log"), id, run.LogPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, runLogUnavailable(id)
	}
	if err != nil {
		return "", false, err
	}
	return s.redactedLogTail(ctx, queue, path, id, maxBytes)
}
