package tasks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// Verdicts of a finished run. They are the verdict names of
// internal/workflowrun, which this package cannot import: its worker imports
// this package.
const (
	verdictPass    = "pass"    // check: the condition holds
	verdictReject  = "reject"  // check: the condition does not hold
	verdictOutcome = "outcome" // watch: an outcome is ready
	verdictQuiet   = "quiet"   // watch or source: nothing changed
	verdictItems   = "items"   // source: the items it found
	verdictFailure = "failure" // anything else
)

// maxRunMessageBytes bounds the message stored from a run.
const maxRunMessageBytes = 4 << 10

// workflowViewRuns is how many recent runs the workflow view shows.
const workflowViewRuns = 20

// ScriptRun is the durable record of one workflow script run.
type ScriptRun struct {
	ID         int64  `json:"id"`
	TaskKey    string `json:"task_key"`
	Kind       string `json:"kind"`
	Script     string `json:"script"`
	RunAs      string `json:"run_as"`
	State      string `json:"state"`
	Verdict    string `json:"verdict,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Message    string `json:"message,omitempty"`
	CreatedAt  string `json:"created_at"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	LogPath    string `json:"log_path,omitempty"`
	// Holder is the agent a run_as agent check ran as, fixed when the run was
	// created; "" for queue and watch runs.
	Holder string `json:"holder,omitempty"`
	// PID is the process a running run recorded, for the worker's recovery
	// after a restart; it is never marshalled.
	PID *int `json:"-"`
	// RequestID is the transition request a check belongs to, 0 for a watch;
	// the Goal block matches a failed request to its run by it. It is never
	// marshalled.
	RequestID int64 `json:"-"`
}

// RunJob is everything the worker needs to execute one pending run.
type RunJob struct {
	Run             ScriptRun
	Queue           string
	WorkflowName    string
	WorkflowVersion string
	WorkflowDigest  string
	Status          string
	Outcome         string // requested outcome; checks only
	Timeout         time.Duration
	Holder          string // agent name for run_as agent, else ""
	// WorkflowEnv and QueueSecrets reach the process environment only; they are
	// never marshalled.
	WorkflowEnv  map[string]string `json:"-"`
	QueueSecrets map[string]string `json:"-"`
	Snapshot     []byte            // task.json content, no secrets
	Outcomes     []string          // declared outcomes of the status
	Artifacts    []string          // declared artifact names
}

// RunCompletion is the result of a finished run as the worker reports it.
type RunCompletion struct {
	// Verdict is one of pass, reject, outcome, quiet, or failure, the verdict
	// names of internal/workflowrun. CompleteScriptRun refuses any other value.
	Verdict    string
	Outcome    string
	Message    string
	Artifacts  map[string]string
	ExitCode   *int
	LogPath    string
	FinishedAt string
}

// scriptRunRecord is a run with the references the engine follows.
type scriptRunRecord struct {
	ScriptRun
	taskID          int64
	visitID         int64
	requestID       int64 // 0 for a watch run
	checkIndex      int
	cancelRequested bool
}

const scriptRunSelect = `
	SELECT r.id, t.task_key, r.kind, r.script, r.run_as, r.state, r.verdict, r.exit_code, r.message,
	       r.created_at, r.started_at, r.finished_at, r.log_path, r.holder, r.pid,
	       r.task_id, r.visit_id, COALESCE(r.request_id, 0), r.check_index, r.cancel_requested
	FROM task_script_runs r JOIN tasks t ON t.id = r.task_id`

func scanScriptRun(row interface{ Scan(...any) error }) (scriptRunRecord, error) {
	var r scriptRunRecord
	var exit, pid sql.NullInt64
	if err := row.Scan(&r.ID, &r.TaskKey, &r.Kind, &r.Script, &r.RunAs, &r.State, &r.Verdict, &exit, &r.Message,
		&r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.LogPath, &r.Holder, &pid,
		&r.taskID, &r.visitID, &r.requestID, &r.checkIndex, &r.cancelRequested); err != nil {
		return scriptRunRecord{}, err
	}
	if exit.Valid {
		code := int(exit.Int64)
		r.ExitCode = &code
	}
	if pid.Valid {
		p := int(pid.Int64)
		r.PID = &p
	}
	r.RequestID = r.requestID
	return r, nil
}

// queryScriptRuns reads the runs matching where, which follows scriptRunSelect.
func queryScriptRuns(ctx context.Context, q queryer, where string, args ...any) ([]scriptRunRecord, error) {
	rows, err := q.QueryContext(ctx, scriptRunSelect+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scriptRunRecord
	for rows.Next() {
		run, err := scanScriptRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// Predicates of the run queries the worker and the readers repeat; each
// follows scriptRunSelect. A state list repeats the WHERE clause of the partial
// index idx_task_script_runs_active word for word, which is what lets SQLite
// use that index.
const (
	taskRunsWhere        = ` WHERE r.task_id = ? ORDER BY r.id DESC`
	pendingRunsWhere     = ` WHERE r.state IN ('pending', 'running') AND r.state = 'pending' ORDER BY r.id`
	cancelRequestedWhere = ` WHERE r.state IN ('pending', 'running') AND r.state = 'running' AND r.cancel_requested = 1 ORDER BY r.id`
	runningRunsWhere     = ` WHERE r.state IN ('pending', 'running') AND r.state = 'running' ORDER BY r.id`
)

// scriptRunsTx returns a task's runs, newest first; limit 0 returns all.
func scriptRunsTx(ctx context.Context, q queryer, taskID int64, limit int) ([]ScriptRun, error) {
	where := taskRunsWhere
	args := []any{taskID}
	if limit > 0 {
		where += ` LIMIT ?`
		args = append(args, limit)
	}
	records, err := queryScriptRuns(ctx, q, where, args...)
	if err != nil {
		return nil, err
	}
	runs := make([]ScriptRun, 0, len(records))
	for _, record := range records {
		runs = append(runs, record.ScriptRun)
	}
	return runs, nil
}

// insertScriptRunTx records a pending run. requestID is 0 for a watch run.
func insertScriptRunTx(ctx context.Context, tx *sql.Tx, taskID, visitID, requestID int64, kind, script, runAs, holder string, checkIndex int, now string) error {
	var request any
	if requestID != 0 {
		request = requestID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO task_script_runs(task_id, visit_id, request_id, kind, script, run_as, holder, check_index, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		taskID, visitID, request, kind, script, runAs, holder, checkIndex, now)
	return err
}

// stopVisitScriptsTx stops the scripts of the task's open visit: it cancels
// the pending transition request with why, clears the watch schedule, cancels
// pending runs, and asks the worker to kill a running one. Every route that
// closes a visit calls it first.
func stopVisitScriptsTx(ctx context.Context, tx *sql.Tx, task Task, why, now string) error {
	if err := cancelPendingRequestTx(ctx, tx, task, why, now); err != nil {
		return err
	}
	taskID := task.ID
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_status_visits SET next_watch_at = '' WHERE task_id = ? AND left_at = ''`, taskID); err != nil {
		return err
	}
	const openVisit = `visit_id IN (SELECT id FROM task_status_visits WHERE task_id = ? AND left_at = '')`
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_script_runs SET state = 'cancelled', finished_at = ?
		WHERE task_id = ? AND state = 'pending' AND `+openVisit, now, taskID, taskID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE task_script_runs SET cancel_requested = 1
		WHERE task_id = ? AND state = 'running' AND `+openVisit, taskID, taskID)
	return err
}

// watchTime formats a watch schedule time; the layout is fixed width, so
// next_watch_at compares correctly as a string.
func watchTime(t time.Time) string {
	return t.UTC().Format(dispatchedAtLayout)
}

// scriptTimeout parses a declared timeout, falling back to fallback when it
// is empty or invalid.
func scriptTimeout(declared string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(declared); err == nil && d > 0 {
		return d
	}
	return fallback
}

// watchEvery is the pause after a watch run of status before the next one.
func watchEvery(status workflowfile.Status) time.Duration {
	if status.Watch == nil {
		return workflowfile.MinWatchEvery
	}
	return scriptTimeout(status.Watch.Every, workflowfile.MinWatchEvery)
}

// maxEchoedNameBytes bounds a name taken from script output that goes into a
// message or an event.
const maxEchoedNameBytes = 64

// boundRunMessage cuts message to maxRunMessageBytes on a rune boundary.
func boundRunMessage(message string) string {
	message, _ = CutBytes(message, maxRunMessageBytes)
	return message
}

func scriptAuthor(script string) string { return "script:" + script }

// IsTaskDirKey reports whether a task key may name the task's directory under
// <base-dir>/tasks: a single local path element, and not "." or "..".
func IsTaskDirKey(key string) bool {
	return key != "" && key != "." && key != ".." && filepath.IsLocal(key) && !strings.ContainsAny(key, `/\`)
}

func scriptRunNotFound(id int64) error {
	return domainError(http.StatusNotFound, "run_not_found", fmt.Sprintf("script run %d not found", id))
}

// PendingRunJobs returns every pending run, oldest first, with what the worker
// needs to execute it. A run whose job cannot be built is logged and skipped,
// so it does not hold back the others.
func (s *Service) PendingRunJobs(ctx context.Context) ([]RunJob, error) {
	runs, err := queryScriptRuns(ctx, s.db, pendingRunsWhere)
	if err != nil {
		return nil, err
	}
	jobs := make([]RunJob, 0, len(runs))
	for _, run := range runs {
		job, err := s.runJob(ctx, run)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			s.logger().Error("skip workflow script run whose job cannot be built",
				"run_id", run.ID, "task", run.TaskKey, "err", err)
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// runJob builds the job of one run. It reads through s.db one query at a time:
// the database has a single connection.
func (s *Service) runJob(ctx context.Context, run scriptRunRecord) (RunJob, error) {
	task, err := taskByID(s.db, run.taskID)
	if err != nil {
		return RunJob{}, err
	}
	manifest, err := loadManifestTx(ctx, s.db, task.WorkflowDigest)
	if err != nil {
		return RunJob{}, err
	}
	var statusID string
	if err := s.db.QueryRowContext(ctx, `SELECT status_id FROM task_status_visits WHERE id = ?`, run.visitID).
		Scan(&statusID); err != nil {
		return RunJob{}, err
	}
	status, _ := currentStatus(manifest, statusID)
	job := RunJob{
		Run: run.ScriptRun, Queue: task.Queue,
		WorkflowName: manifest.Name, WorkflowVersion: manifest.Version, WorkflowDigest: manifest.Digest,
		Status: statusID, WorkflowEnv: maps.Clone(manifest.Definition.Env),
		Outcomes: statusOutcomes(status), Artifacts: []string{},
	}
	if job.WorkflowEnv == nil {
		job.WorkflowEnv = map[string]string{}
	}
	for _, declared := range manifest.Definition.Artifacts {
		job.Artifacts = append(job.Artifacts, declared.Name)
	}
	var message string
	switch run.Kind {
	case "check":
		if err := s.db.QueryRowContext(ctx, `SELECT outcome, message FROM task_transition_requests WHERE id = ?`,
			run.requestID).Scan(&job.Outcome, &message); err != nil {
			return RunJob{}, err
		}
		job.Timeout = workflowfile.DefaultCheckTimeout
		if transition, ok := statusTransition(status, job.Outcome); ok && run.checkIndex < len(transition.Checks) {
			job.Timeout = scriptTimeout(transition.Checks[run.checkIndex].Timeout, workflowfile.DefaultCheckTimeout)
		}
		if run.RunAs == workflowfile.RunAsAgent {
			job.Holder = run.Holder
		}
	default:
		job.Timeout = workflowfile.DefaultWatchTimeout
		if status.Watch != nil {
			job.Timeout = scriptTimeout(status.Watch.Timeout, workflowfile.DefaultWatchTimeout)
		}
	}
	if job.Snapshot, err = taskSnapshotJSON(ctx, s.db, task, manifest, run.visitID, job.Outcome, message); err != nil {
		return RunJob{}, err
	}
	if job.QueueSecrets, err = s.queueSecrets(ctx, task.Queue); err != nil {
		return RunJob{}, err
	}
	return job, nil
}

// ClaimScriptRun moves a pending run to running. It returns false when the run
// is no longer pending or its cancellation was requested.
func (s *Service) ClaimScriptRun(ctx context.Context, id int64, startedAt, logPath string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE task_script_runs SET state = 'running', started_at = ?, log_path = ?
		WHERE id = ? AND state = 'pending' AND cancel_requested = 0`, startedAt, logPath, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// SetScriptRunPID records the process of a running run.
func (s *Service) SetScriptRunPID(ctx context.Context, id int64, pid int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE task_script_runs SET pid = ? WHERE id = ? AND state = 'running'`, pid, id)
	return err
}

// CancelRequestedRuns lists the running runs whose cancellation was requested;
// the worker kills them.
func (s *Service) CancelRequestedRuns(ctx context.Context) ([]ScriptRun, error) {
	return s.listScriptRuns(ctx, cancelRequestedWhere)
}

// RunningScriptRuns lists every running run with its recorded PID. At daemon
// start the worker uses it to find the scripts a previous daemon left behind.
func (s *Service) RunningScriptRuns(ctx context.Context) ([]ScriptRun, error) {
	return s.listScriptRuns(ctx, runningRunsWhere)
}

func (s *Service) listScriptRuns(ctx context.Context, where string) ([]ScriptRun, error) {
	records, err := queryScriptRuns(ctx, s.db, where)
	if err != nil {
		return nil, err
	}
	runs := make([]ScriptRun, 0, len(records))
	for _, record := range records {
		runs = append(runs, record.ScriptRun)
	}
	return runs, nil
}

// ListScriptRuns returns the runs of a task the actor may read, newest first.
func (s *Service) ListScriptRuns(ctx context.Context, actor Actor, key string) ([]ScriptRun, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	task, _, err := workflowReadTaskTx(ctx, s.db, actor, key)
	if err != nil {
		return nil, err
	}
	return scriptRunsTx(ctx, s.db, task.ID, 0)
}

// GetScriptRun returns one run of a task the actor may read.
func (s *Service) GetScriptRun(ctx context.Context, actor Actor, key string, id int64) (ScriptRun, error) {
	if err := validateActor(actor); err != nil {
		return ScriptRun{}, err
	}
	task, _, err := workflowReadTaskTx(ctx, s.db, actor, key)
	if err != nil {
		return ScriptRun{}, err
	}
	run, err := scanScriptRun(s.db.QueryRowContext(ctx, scriptRunSelect+` WHERE r.task_id = ? AND r.id = ?`, task.ID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ScriptRun{}, scriptRunNotFound(id)
	}
	if err != nil {
		return ScriptRun{}, err
	}
	return run.ScriptRun, nil
}

// CompleteScriptRun records the result of a run and applies what it means: a
// check moves its transition request on, a watch run reschedules itself or
// applies an outcome. Completing a run that is no longer pending or running is
// a no-op. A run whose cancellation was requested is recorded as cancelled and
// has no further effect.
func (s *Service) CompleteScriptRun(ctx context.Context, id int64, done RunCompletion) error {
	switch done.Verdict {
	case verdictPass, verdictReject, verdictOutcome, verdictQuiet, verdictFailure:
	default:
		return fmt.Errorf("script run %d: unknown verdict %q", id, done.Verdict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	run, err := scanScriptRun(tx.QueryRowContext(ctx, scriptRunSelect+` WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return scriptRunNotFound(id)
	}
	if err != nil {
		return err
	}
	if run.State != "pending" && run.State != "running" {
		return nil
	}
	now := s.now()
	// The stored time is always in the service's format; a time the worker
	// reports that does not parse is replaced by the service clock.
	finished := s.clock()
	if parsed, err := time.Parse(time.RFC3339Nano, done.FinishedAt); err == nil {
		finished = parsed
	}
	finishedAt := finished.UTC().Format(time.RFC3339Nano)
	logPath := done.LogPath
	if logPath == "" {
		logPath = run.LogPath
	}
	message := boundRunMessage(done.Message)
	state := "finished"
	if run.cancelRequested {
		state = "cancelled"
	}
	var exit any
	if done.ExitCode != nil {
		exit = *done.ExitCode
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_script_runs SET state = ?, verdict = ?, exit_code = ?, message = ?, finished_at = ?, log_path = ?
		WHERE id = ?`, state, done.Verdict, exit, message, finishedAt, logPath, id); err != nil {
		return err
	}
	task, err := taskByID(tx, run.taskID)
	if err != nil {
		return err
	}
	if run.Kind == "watch" && done.Verdict == verdictQuiet && state == "finished" {
		// A quiet watch run says nothing happened: it leaves no event, and only
		// the newest quiet runs of the visit are kept. A run cancelled while it
		// went quiet is not one of them.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM task_script_runs
			WHERE visit_id = ? AND kind = 'watch' AND verdict = 'quiet' AND state = 'finished' AND id NOT IN (
				SELECT id FROM task_script_runs
				WHERE visit_id = ? AND kind = 'watch' AND verdict = 'quiet' AND state = 'finished' ORDER BY id DESC LIMIT ?)`,
			run.visitID, run.visitID, workflowViewRuns); err != nil {
			return err
		}
	} else if _, err := appendEventTx(ctx, tx, task, "workflow.script_run", Actor{Principal: workflowActor}, map[string]any{
		"run_id": run.ID, "kind": run.Kind, "script": run.Script, "state": state,
		"verdict": done.Verdict, "exit_code": done.ExitCode,
	}, now); err != nil {
		return err
	}
	if state == "finished" {
		switch run.Kind {
		case "check":
			err = s.finishCheckTx(ctx, tx, &task, run, done.Verdict, done.Artifacts, message, logPath)
		default:
			err = s.finishWatchTx(ctx, tx, &task, run, done, message, logPath, finished)
		}
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.signal()
	return nil
}

// checkRequest is the transition request a check run belongs to.
type checkRequest struct {
	id      int64
	visitID int64
	outcome string
	message string
	actor   string
}

// liveCheckRequestTx returns the request of a check run when the request is
// still pending and its visit is still the task's open visit; ok is false
// otherwise, and the run then changes nothing.
func liveCheckRequestTx(ctx context.Context, tx *sql.Tx, run scriptRunRecord) (checkRequest, bool, error) {
	var request checkRequest
	var state string
	err := tx.QueryRowContext(ctx, `
		SELECT id, visit_id, outcome, message, actor, state FROM task_transition_requests WHERE id = ?`, run.requestID).
		Scan(&request.id, &request.visitID, &request.outcome, &request.message, &request.actor, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return checkRequest{}, false, nil
	}
	if err != nil {
		return checkRequest{}, false, err
	}
	if state != "pending" {
		return checkRequest{}, false, nil
	}
	open, err := visitIsOpenTx(ctx, tx, run.taskID, request.visitID)
	return request, open, err
}

// visitIsOpenTx reports whether visitID is the task's open visit.
func visitIsOpenTx(ctx context.Context, tx *sql.Tx, taskID, visitID int64) (bool, error) {
	var open int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM task_status_visits WHERE task_id = ? AND left_at = ''`, taskID).Scan(&open)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && open == visitID, err
}

// finishCheckTx applies the verdict of a finished check run to its request.
func (s *Service) finishCheckTx(ctx context.Context, tx *sql.Tx, task *Task, run scriptRunRecord, verdict string, artifacts map[string]string, message, logPath string) error {
	request, live, err := liveCheckRequestTx(ctx, tx, run)
	if err != nil || !live {
		return err
	}
	manifest, err := loadManifestTx(ctx, tx, task.WorkflowDigest)
	if err != nil {
		return err
	}
	switch verdict {
	case verdictPass:
		status, _ := currentStatus(manifest, task.WorkflowStatus)
		transition, ok := statusTransition(status, request.outcome)
		if !ok {
			return s.failRequestTx(ctx, tx, *task, &manifest, run, request,
				"status "+task.WorkflowStatus+" no longer declares outcome "+request.outcome, "", false)
		}
		if err := validateScriptArtifacts(manifest, artifacts); err != nil {
			return s.failRequestTx(ctx, tx, *task, &manifest, run, request, err.Error(), logPath, true)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_status_visits SET script_failures = 0 WHERE id = ?`, request.visitID); err != nil {
			return err
		}
		if err := storeScriptArtifactsTx(ctx, tx, *task, manifest, run.Script, artifacts, s.now()); err != nil {
			return err
		}
		if next := run.checkIndex + 1; next < len(transition.Checks) {
			check := transition.Checks[next]
			return insertScriptRunTx(ctx, tx, task.ID, request.visitID, request.id, "check", check.Script,
				checkRunAs(check), checkHolder(check, task.Assignee), next, s.now())
		}
		// The last check passed. Required artifacts were present when the
		// request was made; confirm nothing removed one since.
		present, err := currentArtifactsTx(ctx, tx, task.ID)
		if err != nil {
			return err
		}
		if missing := missingArtifacts(transition, artifactNames(present)); len(missing) > 0 {
			return s.failRequestTx(ctx, tx, *task, &manifest, run, request,
				"outcome "+request.outcome+" requires artifacts with no value: "+strings.Join(missing, ", "), "", false)
		}
		return s.applyTransitionTx(ctx, tx, task, manifest, request.id, transition, request.actor, request.message)
	case verdictReject:
		now := s.now()
		if _, err := tx.ExecContext(ctx, `
			UPDATE task_transition_requests SET state = 'rejected', result_message = ?, finished_at = ? WHERE id = ?`,
			message, now, request.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE task_status_visits SET rejected_requests = rejected_requests + 1, script_failures = 0 WHERE id = ?`,
			request.visitID); err != nil {
			return err
		}
		if _, err := appendEventTx(ctx, tx, *task, "workflow.transition_rejected", Actor{Principal: workflowActor}, map[string]any{
			"request_id": request.id, "status": task.WorkflowStatus, "outcome": request.outcome,
			"script": run.Script, "message": message,
		}, now); err != nil {
			return err
		}
		return s.pauseAtLimitTx(ctx, tx, task, manifest, request.visitID, PauseRejectedRequests, message)
	default:
		// A check never produces outcome or quiet; the protocol turns them into
		// failures, and so does this.
		if message == "" {
			message = "check " + run.Script + " failed"
		}
		return s.failRequestTx(ctx, tx, *task, &manifest, run, request, message, logPath, true)
	}
}

// failRequestTx closes a check's request as failed with message and, when
// set, the log path. scriptFailure counts it toward the visit's
// script_failures and pauses the task at the limit; with no manifest (nil)
// there is no limit to compare, and the failure is only counted.
func (s *Service) failRequestTx(ctx context.Context, tx *sql.Tx, task Task, manifest *workflowimage.Manifest, run scriptRunRecord, request checkRequest, message, logPath string, scriptFailure bool) error {
	now := s.now()
	result := message
	if logPath != "" {
		result += "\nlog: " + logPath
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_transition_requests SET state = 'failed', result_message = ?, finished_at = ? WHERE id = ?`,
		result, now, request.id); err != nil {
		return err
	}
	if scriptFailure {
		if _, err := tx.ExecContext(ctx, `
			UPDATE task_status_visits SET script_failures = script_failures + 1 WHERE id = ?`, request.visitID); err != nil {
			return err
		}
	}
	if _, err := appendEventTx(ctx, tx, task, "workflow.transition_failed", Actor{Principal: workflowActor}, map[string]any{
		"request_id": request.id, "status": task.WorkflowStatus, "outcome": request.outcome,
		"script": run.Script, "message": message, "log_path": logPath,
	}, now); err != nil {
		return err
	}
	if !scriptFailure || manifest == nil {
		return nil
	}
	return s.pauseAtLimitTx(ctx, tx, &task, *manifest, request.visitID, PauseScriptFailures, result)
}

// checkHolder is the agent a check runs as, recorded when the run is created:
// the task's assignee for a run_as agent check, "" for a queue check. The log
// of such a run may hold the agent's own secrets. A run_as agent check of a
// task no agent holds records no holder; the worker fails it.
func checkHolder(check workflowfile.Check, assignee string) string {
	agent, isAgent := strings.CutPrefix(assignee, "agent:")
	if check.RunAs != workflowfile.RunAsAgent || !isAgent {
		return ""
	}
	return agent
}

// checkRunAs is the run mode of a check; queue when it declares none.
func checkRunAs(check workflowfile.Check) string {
	if check.RunAs == workflowfile.RunAsAgent {
		return workflowfile.RunAsAgent
	}
	return workflowfile.RunAsQueue
}

// validateScriptArtifacts confirms every artifact a script returned is
// declared and storable, so a bad result fails the run instead of the
// transaction.
func validateScriptArtifacts(manifest workflowimage.Manifest, artifacts map[string]string) error {
	for _, name := range sortedKeys(artifacts) {
		value := artifacts[name]
		switch {
		case !artifactDeclared(manifest, name):
			// The name comes from the script; only a bounded part of it is echoed.
			echoed, _ := CutBytes(name, maxEchoedNameBytes)
			return fmt.Errorf("the script returned undeclared artifact %q", echoed)
		case value == "" || !utf8.ValidString(value):
			return fmt.Errorf("the script returned artifact %q that is not non-empty UTF-8 text", name)
		case len(value) > maxArtifactBytes:
			return fmt.Errorf("the script returned artifact %q larger than 64 KiB", name)
		}
	}
	return nil
}

// storeScriptArtifactsTx stores the artifacts a script returned, authored by
// the script, in name order.
func storeScriptArtifactsTx(ctx context.Context, tx *sql.Tx, task Task, manifest workflowimage.Manifest, script string, artifacts map[string]string, now string) error {
	for _, name := range sortedKeys(artifacts) {
		if _, err := setArtifactTx(ctx, tx, task, manifest, name, artifacts[name], scriptAuthor(script), now); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// liveWatchTx reports whether a watch run of visitID still speaks for the
// task: the visit is open and the task is neither closed nor paused.
func liveWatchTx(ctx context.Context, tx *sql.Tx, task Task, visitID int64) (bool, error) {
	if task.Status == StatusDone || task.Status == StatusCancelled || task.WorkflowPausedReason != "" {
		return false, nil
	}
	return visitIsOpenTx(ctx, tx, task.ID, visitID)
}

// finishWatchTx applies the verdict of a finished watch run.
func (s *Service) finishWatchTx(ctx context.Context, tx *sql.Tx, task *Task, run scriptRunRecord, done RunCompletion, message, logPath string, finished time.Time) error {
	live, err := liveWatchTx(ctx, tx, *task, run.visitID)
	if err != nil || !live {
		return err
	}
	manifest, err := loadManifestTx(ctx, tx, task.WorkflowDigest)
	if err != nil {
		return err
	}
	status, _ := currentStatus(manifest, task.WorkflowStatus)
	next := watchTime(finished.Add(watchEvery(status)))
	switch done.Verdict {
	case verdictQuiet:
		_, err := tx.ExecContext(ctx, `
			UPDATE task_status_visits SET next_watch_at = ?, script_failures = 0 WHERE id = ?`, next, run.visitID)
		return err
	case verdictOutcome:
		transition, ok := statusTransition(status, done.Outcome)
		if !ok {
			return s.failWatchTx(ctx, tx, *task, manifest, run, done.Verdict,
				"status "+status.ID+" does not declare outcome "+done.Outcome, logPath, next)
		}
		if err := validateScriptArtifacts(manifest, done.Artifacts); err != nil {
			return s.failWatchTx(ctx, tx, *task, manifest, run, done.Verdict, err.Error(), logPath, next)
		}
		present, err := currentArtifactsTx(ctx, tx, task.ID)
		if err != nil {
			return err
		}
		names := artifactNames(present)
		for name := range done.Artifacts {
			names[name] = true
		}
		if missing := missingArtifacts(transition, names); len(missing) > 0 {
			return s.failWatchTx(ctx, tx, *task, manifest, run, done.Verdict,
				"outcome "+done.Outcome+" requires artifacts with no value: "+strings.Join(missing, ", "), logPath, next)
		}
		return s.applyWatchOutcomeTx(ctx, tx, task, manifest, run, transition, done.Artifacts, message)
	default:
		// A watch run never passes or rejects; those are failures too.
		if message == "" {
			message = "watch script " + run.Script + " failed"
		}
		return s.failWatchTx(ctx, tx, *task, manifest, run, done.Verdict, message, logPath, next)
	}
}

// applyWatchOutcomeTx stores the run's artifacts and applies the transition
// through an applied request authored by the script, for the audit trail.
func (s *Service) applyWatchOutcomeTx(ctx context.Context, tx *sql.Tx, task *Task, manifest workflowimage.Manifest, run scriptRunRecord, transition workflowfile.Transition, artifacts map[string]string, message string) error {
	now := s.now()
	author := scriptAuthor(run.Script)
	if err := storeScriptArtifactsTx(ctx, tx, *task, manifest, run.Script, artifacts, now); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO task_transition_requests(task_id, visit_id, outcome, message, actor, state, created_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?)`, task.ID, run.visitID, transition.On, message, author, now)
	if err != nil {
		return err
	}
	requestID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := appendEventTx(ctx, tx, *task, "workflow.transition_requested", Actor{Principal: author}, map[string]any{
		"request_id": requestID, "status": task.WorkflowStatus, "outcome": transition.On, "actor": author, "state": "applied",
	}, now); err != nil {
		return err
	}
	return s.applyTransitionTx(ctx, tx, task, manifest, requestID, transition, author, message)
}

// failWatchTx counts a failed watch run and schedules the next run at next.
// At the limit it pauses the task, which clears that schedule again.
func (s *Service) failWatchTx(ctx context.Context, tx *sql.Tx, task Task, manifest workflowimage.Manifest, run scriptRunRecord, verdict, message, logPath, next string) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_status_visits SET script_failures = script_failures + 1, next_watch_at = ? WHERE id = ?`,
		next, run.visitID); err != nil {
		return err
	}
	if _, err := appendEventTx(ctx, tx, task, "workflow.script_failed", Actor{Principal: workflowActor}, map[string]any{
		"run_id": run.ID, "script": run.Script, "status": task.WorkflowStatus, "verdict": verdict,
		"message": message, "log_path": logPath,
	}, s.now()); err != nil {
		return err
	}
	detail := message
	if logPath != "" {
		detail += "\nlog: " + logPath
	}
	return s.pauseAtLimitTx(ctx, tx, &task, manifest, run.visitID, PauseScriptFailures, detail)
}

// RecoverScriptRuns runs when the worker starts: every running run is recorded
// as interrupted, its check request fails, and its watch runs next after
// every. Pending runs stay pending. A run that cannot be interrupted is logged
// and forced to interrupted, so it does not hold back every other script;
// only a context error fails recovery.
func (s *Service) RecoverScriptRuns(ctx context.Context) error {
	runs, err := queryScriptRuns(ctx, s.db, runningRunsWhere)
	if err != nil {
		return err
	}
	for _, run := range runs {
		err := s.interruptRun(ctx, run.ID)
		if err == nil {
			continue
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("script run %d: %w", run.ID, err)
		}
		s.logger().Error("force workflow script run to interrupted", "run_id", run.ID, "task", run.TaskKey, "err", err)
		if err := s.forceInterruptRun(ctx, run, err); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("script run %d: %w", run.ID, err)
			}
			s.logger().Error("force workflow script run to interrupted failed", "run_id", run.ID, "task", run.TaskKey, "err", err)
		}
	}
	if len(runs) > 0 {
		s.signal()
	}
	return nil
}

// forceInterruptRun records a running run as interrupted when interruptRun
// failed with cause, touching only the run row and, for a check, its pending
// request or, for a watch, the schedule of its open visit.
func (s *Service) forceInterruptRun(ctx context.Context, run scriptRunRecord, cause error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	message := boundRunMessage(fmt.Sprintf("the daemon restarted during %s %s; recording the interruption failed: %v",
		run.Kind, run.Script, cause))
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_script_runs SET state = 'interrupted', finished_at = ?, message = ? WHERE id = ? AND state = 'running'`,
		now, message, run.ID); err != nil {
		return err
	}
	if run.Kind == "check" && run.requestID != 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE task_transition_requests SET state = 'failed', result_message = ?, finished_at = ? WHERE id = ? AND state = 'pending'`,
			message, now, run.requestID); err != nil {
			return err
		}
	}
	// A watch of a visit still open runs again after the shortest every, so
	// the status is not left without its watch.
	if run.Kind == "watch" {
		if _, err := tx.ExecContext(ctx, `UPDATE task_status_visits SET next_watch_at = ? WHERE id = ? AND left_at = ''`,
			watchTime(s.clock().Add(workflowfile.MinWatchEvery)), run.visitID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// interruptRun records one running run as interrupted in its own transaction.
func (s *Service) interruptRun(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	run, err := scanScriptRun(tx.QueryRowContext(ctx, scriptRunSelect+` WHERE r.id = ?`, id))
	if err != nil {
		return err
	}
	if run.State != "running" {
		return nil
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_script_runs SET state = 'interrupted', finished_at = ? WHERE id = ?`, now, id); err != nil {
		return err
	}
	task, err := taskByID(tx, run.taskID)
	if err != nil {
		return err
	}
	if _, err := appendEventTx(ctx, tx, task, "workflow.script_run", Actor{Principal: workflowActor}, map[string]any{
		"run_id": run.ID, "kind": run.Kind, "script": run.Script, "state": "interrupted",
		"verdict": "", "exit_code": nil,
	}, now); err != nil {
		return err
	}
	switch run.Kind {
	case "check":
		request, live, err := liveCheckRequestTx(ctx, tx, run)
		if err != nil {
			return err
		}
		if live {
			// The manifest serves only the limit comparison: without it the
			// request still fails and the failure still counts.
			var limits *workflowimage.Manifest
			manifest, err := loadManifestTx(ctx, tx, task.WorkflowDigest)
			switch {
			case err == nil:
				limits = &manifest
			case ctx.Err() != nil:
				return err
			default:
				s.logger().Warn("fail interrupted check without its manifest", "run_id", run.ID, "task", task.Key, "err", err)
			}
			if err := s.failRequestTx(ctx, tx, task, limits, run, request,
				"the daemon restarted during check "+run.Script+"; the check did not finish", run.LogPath, true); err != nil {
				return err
			}
		}
	default:
		live, err := liveWatchTx(ctx, tx, task, run.visitID)
		if err != nil {
			return err
		}
		if live {
			// The next run follows after every, as after any finished run; with
			// no readable manifest, after the shortest every.
			every := workflowfile.MinWatchEvery
			manifest, err := loadManifestTx(ctx, tx, task.WorkflowDigest)
			switch {
			case err == nil:
				status, _ := currentStatus(manifest, task.WorkflowStatus)
				every = watchEvery(status)
			case ctx.Err() != nil:
				return err
			default:
				s.logger().Warn("reschedule interrupted watch without its manifest", "run_id", run.ID, "task", task.Key, "err", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE task_status_visits SET next_watch_at = ? WHERE id = ?`,
				watchTime(s.clock().Add(every)), run.visitID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// dueWatchVisits selects the open visits whose watch is due at the bound time
// and may run: the task is open and unpaused and has no active run.
const dueWatchVisits = `
	SELECT v.id FROM task_status_visits v JOIN tasks t ON t.id = v.task_id
	WHERE v.left_at = '' AND v.next_watch_at <> '' AND v.next_watch_at <= ?
	  AND t.status NOT IN ('done', 'cancelled') AND t.workflow_paused_reason = ''
	  AND NOT EXISTS (
		SELECT 1 FROM task_script_runs r WHERE r.task_id = t.id AND r.state IN ('pending', 'running'))`

// ScheduleDueWatches creates a pending watch run for every visit whose watch
// is due at now and clears its schedule; completing the run sets it again. It
// returns how many runs it created.
func (s *Service) ScheduleDueWatches(ctx context.Context, now time.Time) (int, error) {
	cutoff := watchTime(now)
	rows, err := s.db.QueryContext(ctx, dueWatchVisits+` ORDER BY v.next_watch_at, v.id`, cutoff)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	scheduled := 0
	var errs []error
	for _, id := range ids {
		created, err := s.scheduleWatch(ctx, id, cutoff)
		if err != nil {
			errs = append(errs, fmt.Errorf("visit %d: %w", id, err))
			continue
		}
		if created {
			scheduled++
		}
	}
	if scheduled > 0 {
		s.signal()
	}
	return scheduled, errors.Join(errs...)
}

// scheduleWatch creates the watch run of one due visit in its own transaction.
func (s *Service) scheduleWatch(ctx context.Context, visitID int64, cutoff string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var still int64
	err = tx.QueryRowContext(ctx, dueWatchVisits+` AND v.id = ?`, cutoff, visitID).Scan(&still)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var taskID int64
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM task_status_visits WHERE id = ?`, visitID).Scan(&taskID); err != nil {
		return false, err
	}
	task, err := taskByID(tx, taskID)
	if err != nil {
		return false, err
	}
	manifest, err := loadManifestTx(ctx, tx, task.WorkflowDigest)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_status_visits SET next_watch_at = '' WHERE id = ?`, visitID); err != nil {
		return false, err
	}
	status, ok := currentStatus(manifest, task.WorkflowStatus)
	created := ok && !status.Terminal && status.Owner.Kind == workflowfile.OwnerScript && status.Watch != nil
	if created {
		if err := insertScriptRunTx(ctx, tx, task.ID, visitID, 0, "watch", status.Watch.Script,
			workflowfile.RunAsQueue, "", 0, s.now()); err != nil {
			return false, err
		}
	}
	return created, tx.Commit()
}
