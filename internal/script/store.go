package script

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	basestore "github.com/alekzonder/tariboy/internal/store"
)

const seqWidth = 9

type dbtx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

type Store struct {
	db    *sql.DB
	clock func() time.Time
}

func NewStore(s *basestore.Store, clock func() time.Time) *Store {
	if clock == nil {
		clock = time.Now
	}
	return &Store{db: s.DB, clock: clock}
}

func (s *Store) CreateOnce(agent string, in CreateOnce) (Definition, Run, error) {
	if err := validateCommon(in.Name, in.Description, in.Command); err != nil {
		return Definition{}, Run{}, err
	}
	return s.create(agent, in.Name, in.Description, in.Command, ModeOnce, 0)
}

func (s *Store) CreateSchedule(agent string, in CreateSchedule) (Definition, Run, error) {
	if err := validateCommon(in.Name, in.Description, in.Command); err != nil {
		return Definition{}, Run{}, err
	}
	if in.IntervalSeconds <= 0 {
		return Definition{}, Run{}, errors.New("recurring interval must be positive")
	}
	command := in.Command
	if in.QuietExit != nil {
		if *in.QuietExit < 0 || *in.QuietExit > 255 {
			return Definition{}, Run{}, errors.New("quiet exit must be between 0 and 255")
		}
		if *in.QuietExit != QuietExit {
			command = LegacyQuietCommand(command, *in.QuietExit)
		}
	}
	return s.create(agent, in.Name, in.Description, command, ModeEvery, in.IntervalSeconds)
}

func validateCommon(name, description, command string) error {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(description) == "" || strings.TrimSpace(command) == "" {
		return errors.New("name, description, and command are required")
	}
	return nil
}

func (s *Store) create(agent, name, description, command, mode string, interval int) (Definition, Run, error) {
	now := s.clock().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return Definition{}, Run{}, err
	}
	defer tx.Rollback()
	scriptID, err := nextID(tx, "scripts", "scr-"+agent+"-", agent, now)
	if err != nil {
		return Definition{}, Run{}, err
	}
	runID, err := nextID(tx, "script_runs", "srun-"+agent+"-", agent, now)
	if err != nil {
		return Definition{}, Run{}, err
	}
	definition := Definition{ID: scriptID, Agent: agent, Name: name, Description: description, Command: command, Mode: mode, IntervalSeconds: interval, State: StateActive, CreatedAt: now}
	run := Run{ID: runID, ScriptID: scriptID, Agent: agent, Status: RunPending, CreatedAt: now}
	if _, err := tx.Exec(`INSERT INTO scripts(id,agent,name,description,command,mode,interval_seconds,state,created_at,next_run_at) VALUES(?,?,?,?,?,?,?,?,?,NULL)`,
		definition.ID, definition.Agent, definition.Name, definition.Description, definition.Command, definition.Mode, nullInterval(interval), definition.State, definition.CreatedAt); err != nil {
		return Definition{}, Run{}, err
	}
	if err := insertRun(tx, run); err != nil {
		return Definition{}, Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Definition{}, Run{}, err
	}
	definition.LatestRun = &run
	return definition, run, nil
}

func nullInterval(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nextID(x dbtx, table, prefix, agent, at string) (string, error) {
	timestamp := at
	if parsed, err := time.Parse(time.RFC3339, at); err == nil {
		timestamp = parsed.UTC().Format("20060102150405")
	}
	fullPrefix := prefix + timestamp + "-"
	query := fmt.Sprintf(`SELECT COALESCE(MAX(CAST(substr(id, ?) AS INTEGER)), 0) FROM %s WHERE agent=? AND substr(id,1,?)=?`, table)
	var maxSeq int64
	if err := x.QueryRow(query, len(fullPrefix)+1, agent, len(fullPrefix), fullPrefix).Scan(&maxSeq); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%0*d", fullPrefix, seqWidth, maxSeq+1), nil
}

func insertRun(x dbtx, run Run) error {
	_, err := x.Exec(`INSERT INTO script_runs(id,script_id,agent,status,cancel_requested,pid,exit_code,created_at,started_at,finished_at,log_path) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.ScriptID, run.Agent, run.Status, run.CancelRequested, run.PID, run.ExitCode, run.CreatedAt, nullable(run.StartedAt), nullable(run.FinishedAt), run.LogPath)
	return err
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const selectDefinition = `SELECT id,agent,name,description,command,mode,COALESCE(interval_seconds,0),state,created_at,COALESCE(next_run_at,'') FROM scripts`
const selectRun = `SELECT id,script_id,agent,status,cancel_requested,pid,exit_code,created_at,COALESCE(started_at,''),COALESCE(finished_at,''),log_path FROM script_runs`

func scanDefinition(row interface{ Scan(...any) error }) (Definition, error) {
	var definition Definition
	err := row.Scan(&definition.ID, &definition.Agent, &definition.Name, &definition.Description, &definition.Command, &definition.Mode, &definition.IntervalSeconds, &definition.State, &definition.CreatedAt, &definition.NextRunAt)
	return definition, err
}

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var run Run
	err := row.Scan(&run.ID, &run.ScriptID, &run.Agent, &run.Status, &run.CancelRequested, &run.PID, &run.ExitCode, &run.CreatedAt, &run.StartedAt, &run.FinishedAt, &run.LogPath)
	return run, err
}

func (s *Store) GetDefinition(agent, id string) (Definition, error) {
	definition, err := scanDefinition(s.db.QueryRow(selectDefinition+` WHERE agent=? AND id=?`, agent, id))
	if err == sql.ErrNoRows {
		return Definition{}, ErrNotFound
	}
	if err != nil {
		return Definition{}, err
	}
	latest, err := scanRun(s.db.QueryRow(selectRun+` WHERE agent=? AND script_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, agent, id))
	if err != nil && err != sql.ErrNoRows {
		return Definition{}, err
	}
	if err == nil {
		definition.LatestRun = &latest
	}
	return definition, nil
}

func (s *Store) GetRun(agent, id string) (Run, error) {
	run, err := scanRun(s.db.QueryRow(selectRun+` WHERE agent=? AND id=?`, agent, id))
	if err == sql.ErrNoRows {
		return Run{}, ErrNotFound
	}
	return run, err
}

func (s *Store) ListDefinitions(agent string) ([]Definition, error) {
	rows, err := s.db.Query(selectDefinition+` WHERE agent=? ORDER BY created_at DESC,id DESC`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var definitions []Definition
	for rows.Next() {
		definition, err := scanDefinition(rows)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range definitions {
		latest, err := scanRun(s.db.QueryRow(selectRun+` WHERE agent=? AND script_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, agent, definitions[i].ID))
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if err == nil {
			definitions[i].LatestRun = &latest
		}
	}
	return definitions, nil
}

func (s *Store) ListRuns(agent, scriptID string) ([]Run, error) {
	if _, err := s.GetDefinition(agent, scriptID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(selectRun+` WHERE agent=? AND script_id=? ORDER BY created_at DESC,id DESC`, agent, scriptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) ClaimRun(agent, runID, startedAt, logPath string) (bool, error) {
	result, err := s.db.Exec(`UPDATE script_runs SET status=?,started_at=?,log_path=? WHERE agent=? AND id=? AND status=?`, RunRunning, startedAt, logPath, agent, runID, RunPending)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) SetRunPID(agent, runID string, pid int) (bool, error) {
	result, err := s.db.Exec(`UPDATE script_runs SET pid=? WHERE agent=? AND id=? AND status=?`, pid, agent, runID, RunRunning)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) RequestRunCancellation(agent, runID string) (Run, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRow(selectRun+` WHERE agent=? AND id=?`, agent, runID))
	if err == sql.ErrNoRows {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	if run.Status == RunCancelled || run.CancelRequested {
		return run, nil
	}
	if run.Status != RunRunning {
		return Run{}, ErrConflict
	}
	if _, err := tx.Exec(`UPDATE script_runs SET cancel_requested=1 WHERE agent=? AND id=? AND status=?`, agent, runID, RunRunning); err != nil {
		return Run{}, err
	}
	run.CancelRequested = true
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) CompleteRun(agent, runID string, completion Completion) (Run, error) {
	if !terminalStatus(completion.Status) {
		return Run{}, fmt.Errorf("invalid terminal run status %q", completion.Status)
	}
	if completion.FinishedAt == "" {
		completion.FinishedAt = s.clock().UTC().Format(time.RFC3339)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRow(selectRun+` WHERE agent=? AND id=?`, agent, runID))
	if err == sql.ErrNoRows {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	if run.Status != RunPending && run.Status != RunRunning {
		return Run{}, ErrConflict
	}
	definition, err := scanDefinition(tx.QueryRow(selectDefinition+` WHERE agent=? AND id=?`, agent, run.ScriptID))
	if err != nil {
		return Run{}, err
	}
	result, err := tx.Exec(`UPDATE script_runs SET status=?,cancel_requested=0,pid=NULL,exit_code=?,finished_at=?,log_path=? WHERE agent=? AND id=? AND status IN (?,?)`,
		completion.Status, completion.ExitCode, completion.FinishedAt, completion.LogPath, agent, runID, RunPending, RunRunning)
	if err != nil {
		return Run{}, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return Run{}, err
		}
		return Run{}, ErrConflict
	}
	// QuietExit is the only quiet result, and only for a recurring script: a
	// one-shot run always reports. A recurring script keeps its schedule only
	// while it stays quiet. Any published result stops it, so the agent is
	// notified once and resumes it deliberately with Rerun.
	quiet := definition.Mode == ModeEvery && completion.ExitCode != nil && *completion.ExitCode == QuietExit
	state := StateCompleted
	var nextRun any
	if definition.Mode == ModeEvery && definition.State == StateActive && quiet {
		state = StateActive
		finished, err := time.Parse(time.RFC3339, completion.FinishedAt)
		if err != nil {
			return Run{}, err
		}
		nextRun = finished.Add(time.Duration(definition.IntervalSeconds) * time.Second).UTC().Format(time.RFC3339)
	} else if definition.State == StateCancelled {
		state = StateCancelled
	}
	if _, err := tx.Exec(`UPDATE scripts SET state=?,next_run_at=? WHERE agent=? AND id=?`, state, nextRun, agent, definition.ID); err != nil {
		return Run{}, err
	}
	run.Status = completion.Status
	run.CancelRequested = false
	run.PID = nil
	run.ExitCode = completion.ExitCode
	run.FinishedAt = completion.FinishedAt
	run.LogPath = completion.LogPath
	if !quiet {
		payload, err := json.Marshal(ResultPayload{ScriptID: definition.ID, RunID: run.ID, Name: definition.Name, Mode: definition.Mode, Status: run.Status, ExitCode: run.ExitCode, LogPath: run.LogPath})
		if err != nil {
			return Run{}, err
		}
		key := "script-result:" + run.ID
		if _, err := tx.Exec(`INSERT INTO script_result_outbox(idempotency_key,script_id,run_id,agent,payload,next_attempt_at) VALUES(?,?,?,?,?,?)`, key, definition.ID, run.ID, agent, string(payload), completion.FinishedAt); err != nil {
			return Run{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func terminalStatus(status string) bool {
	switch status {
	case RunSucceeded, RunFailed, RunCancelled, RunTimedOut, RunInterrupted:
		return true
	default:
		return false
	}
}

func (s *Store) Rerun(agent, scriptID string) (Run, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	definition, err := scanDefinition(tx.QueryRow(selectDefinition+` WHERE agent=? AND id=?`, agent, scriptID))
	if err == sql.ErrNoRows {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	if definition.Mode != ModeOnce && definition.Mode != ModeEvery {
		return Run{}, ErrMode
	}
	if definition.Mode == ModeOnce && definition.State == StateActive {
		return Run{}, ErrActive
	}
	if definition.State == StateCancelled {
		return Run{}, ErrConflict
	}
	run, err := s.newPendingRun(tx, definition)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Run{}, ErrConflict
		}
		return Run{}, err
	}
	if _, err := tx.Exec(`UPDATE scripts SET state=?,next_run_at=NULL WHERE agent=? AND id=?`, StateActive, agent, scriptID); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) ScheduleNext(agent, scriptID string) (Run, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	definition, err := scanDefinition(tx.QueryRow(selectDefinition+` WHERE agent=? AND id=?`, agent, scriptID))
	if err == sql.ErrNoRows {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	if definition.Mode != ModeEvery {
		return Run{}, ErrMode
	}
	if definition.State != StateActive {
		return Run{}, ErrConflict
	}
	run, err := s.newPendingRun(tx, definition)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Run{}, ErrConflict
		}
		return Run{}, err
	}
	if _, err := tx.Exec(`UPDATE scripts SET next_run_at=NULL WHERE agent=? AND id=?`, agent, scriptID); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) newPendingRun(tx *sql.Tx, definition Definition) (Run, error) {
	now := s.clock().UTC().Format(time.RFC3339)
	runID, err := nextID(tx, "script_runs", "srun-"+definition.Agent+"-", definition.Agent, now)
	if err != nil {
		return Run{}, err
	}
	run := Run{ID: runID, ScriptID: definition.ID, Agent: definition.Agent, Status: RunPending, CreatedAt: now}
	if err := insertRun(tx, run); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) DueDefinitions(at string) ([]Definition, error) {
	rows, err := s.db.Query(selectDefinition+` WHERE mode=? AND state=? AND next_run_at IS NOT NULL AND julianday(next_run_at)<=julianday(?) ORDER BY next_run_at,id`, ModeEvery, StateActive, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var definitions []Definition
	for rows.Next() {
		definition, err := scanDefinition(rows)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, rows.Err()
}

func (s *Store) PendingRuns() ([]Run, error) {
	rows, err := s.db.Query(selectRun+` WHERE status=? ORDER BY created_at,id`, RunPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) CancelDefinition(agent, scriptID, at string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	definition, err := scanDefinition(tx.QueryRow(selectDefinition+` WHERE agent=? AND id=?`, agent, scriptID))
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if definition.State == StateCancelled {
		return nil
	}
	result, err := tx.Exec(`UPDATE scripts SET state=?,next_run_at=NULL WHERE agent=? AND id=? AND state<>?`, StateCancelled, agent, scriptID, StateCancelled)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return err
		}
		return ErrNotFound
	}
	rows, err := tx.Query(selectRun+` WHERE agent=? AND script_id=? AND status=?`, agent, scriptID, RunPending)
	if err != nil {
		return err
	}
	var active []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return err
		}
		active = append(active, run)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE script_runs SET status=?,pid=NULL,finished_at=? WHERE agent=? AND script_id=? AND status=?`, RunCancelled, at, agent, scriptID, RunPending); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE script_runs SET cancel_requested=1 WHERE agent=? AND script_id=? AND status=?`, agent, scriptID, RunRunning); err != nil {
		return err
	}
	for _, run := range active {
		run.Status, run.PID, run.FinishedAt = RunCancelled, nil, at
		if err := enqueueResult(tx, definition, run, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CancelRun(agent, runID, at string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRow(selectRun+` WHERE agent=? AND id=?`, agent, runID))
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if run.Status == RunCancelled {
		return nil
	}
	if run.Status != RunPending && run.Status != RunRunning {
		return ErrConflict
	}
	definition, err := scanDefinition(tx.QueryRow(selectDefinition+` WHERE agent=? AND id=?`, agent, run.ScriptID))
	if err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE script_runs SET status=?,cancel_requested=0,pid=NULL,finished_at=? WHERE agent=? AND id=? AND status IN (?,?)`, RunCancelled, at, agent, runID, RunPending, RunRunning)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	// The cancelled run publishes a result, which stops a recurring definition
	// exactly as a completed run does. A cancelled definition is already
	// durable and prevents future attempts.
	if definition.State != StateCancelled {
		if _, err := tx.Exec(`UPDATE scripts SET state=?,next_run_at=NULL WHERE agent=? AND id=?`, StateCompleted, agent, definition.ID); err != nil {
			return err
		}
	}
	run.Status, run.CancelRequested, run.PID, run.FinishedAt = RunCancelled, false, nil, at
	if err := enqueueResult(tx, definition, run, at); err != nil {
		return err
	}
	return tx.Commit()
}

func enqueueResult(tx *sql.Tx, definition Definition, run Run, at string) error {
	payload, err := json.Marshal(ResultPayload{ScriptID: definition.ID, RunID: run.ID, Name: definition.Name, Mode: definition.Mode, Status: run.Status, ExitCode: run.ExitCode, LogPath: run.LogPath})
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO script_result_outbox(idempotency_key,script_id,run_id,agent,payload,next_attempt_at) VALUES(?,?,?,?,?,?)`, "script-result:"+run.ID, definition.ID, run.ID, run.Agent, string(payload), at)
	return err
}

func (s *Store) RemoveDefinition(agent, scriptID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	definition, err := scanDefinition(tx.QueryRow(selectDefinition+` WHERE agent=? AND id=?`, agent, scriptID))
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if definition.State == StateActive {
		return ErrActive
	}
	var activeRuns int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM script_runs WHERE agent=? AND script_id=? AND status IN (?,?)`, agent, scriptID, RunPending, RunRunning).Scan(&activeRuns); err != nil {
		return err
	}
	if activeRuns != 0 {
		return ErrActive
	}
	result, err := tx.Exec(`DELETE FROM scripts WHERE agent=? AND id=?`, agent, scriptID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) RecoverRunning() error {
	now := s.clock().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(selectRun+` WHERE status=? ORDER BY id`, RunRunning)
	if err != nil {
		return err
	}
	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return err
		}
		runs = append(runs, run)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, run := range runs {
		definition, err := scanDefinition(tx.QueryRow(selectDefinition+` WHERE agent=? AND id=?`, run.Agent, run.ScriptID))
		if err != nil {
			return err
		}
		terminal := RunInterrupted
		if run.CancelRequested {
			terminal = RunCancelled
		}
		if _, err := tx.Exec(`UPDATE script_runs SET status=?,cancel_requested=0,pid=NULL,finished_at=? WHERE id=? AND status=?`, terminal, now, run.ID, RunRunning); err != nil {
			return err
		}
		// Recovery publishes a result for the interrupted attempt, so a
		// recurring definition stops here too and waits for Rerun.
		state := StateCompleted
		if definition.State == StateCancelled {
			state = StateCancelled
		}
		if _, err := tx.Exec(`UPDATE scripts SET state=?,next_run_at=NULL WHERE id=?`, state, definition.ID); err != nil {
			return err
		}
		payload, err := json.Marshal(ResultPayload{ScriptID: definition.ID, RunID: run.ID, Name: definition.Name, Mode: definition.Mode, Status: terminal, LogPath: run.LogPath})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO script_result_outbox(idempotency_key,script_id,run_id,agent,payload,next_attempt_at) VALUES(?,?,?,?,?,?)`, "script-result:"+run.ID, definition.ID, run.ID, run.Agent, string(payload), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
