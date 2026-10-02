// Package maintenance runs the daemon's nightly database upkeep: a consistent
// backup of the whole tariboyd.db, then deletion of data older than the
// retention period, then an optional VACUUM that returns the freed space.
// Cleanup never runs without a fresh backup.
package maintenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
)

// Settings is the operator policy, persisted as JSON in daemon_config.
type Settings struct {
	Enabled             bool   `json:"enabled"`
	Time                string `json:"time"` // HH:MM, daemon host local time
	KeepBackups         int    `json:"keep_backups"`
	RetentionDays       int    `json:"retention_days"` // 0 = never delete
	Compact             bool   `json:"compact"`
	CompactThresholdPct int    `json:"compact_threshold_pct"`
}

// MinRetentionDays keeps every Usage row the calendar-month budget window
// can still count.
const MinRetentionDays = 31

func DefaultSettings() Settings {
	return Settings{Enabled: true, Time: "03:00", KeepBackups: 7, RetentionDays: 90, Compact: true, CompactThresholdPct: 10}
}

func (s Settings) Validate() error {
	if _, err := time.Parse("15:04", s.Time); err != nil || len(s.Time) != 5 {
		return fmt.Errorf("time must be HH:MM, got %q", s.Time)
	}
	if s.KeepBackups < 1 {
		return errors.New("keep_backups must be >= 1")
	}
	if s.RetentionDays < 0 || (s.RetentionDays > 0 && s.RetentionDays < MinRetentionDays) {
		return fmt.Errorf("retention_days must be 0 (keep everything) or >= %d", MinRetentionDays)
	}
	if s.CompactThresholdPct < 0 || s.CompactThresholdPct > 100 {
		return errors.New("compact_threshold_pct must be between 0 and 100")
	}
	return nil
}

// Result describes one run; the latest is persisted in daemon_config.
type Result struct {
	StartedAt  string           `json:"started_at"`
	FinishedAt string           `json:"finished_at"`
	Trigger    string           `json:"trigger"`
	Backup     string           `json:"backup"`
	Deleted    map[string]int64 `json:"deleted"`
	SizeBefore int64            `json:"size_before"`
	SizeAfter  int64            `json:"size_after"`
	Compacted  bool             `json:"compacted"`
	Error      string           `json:"error,omitempty"`
}

var ErrBusy = errors.New("maintenance is already running")

const (
	settingsKey = "maintenance"
	resultKey   = "maintenance_last_run"
	backupStamp = "20060102T150405Z"
	// pollInterval bounds how late a slot is noticed; timers stop during
	// system sleep, so the loop re-reads the wall clock instead of trusting
	// one long timer.
	pollInterval = time.Minute
	// lateGrace is how late a slot may still run; a later wake (a laptop
	// resuming at noon) skips it, as missed runs are not caught up.
	lateGrace = 15 * time.Minute
)

type Service struct {
	// TasksDir is <base-dir>/tasks, where each task keeps its files under its
	// key; the files of a purged task are removed after the purge commits.
	// Empty leaves task files alone.
	TasksDir string

	st        *store.Store
	backupDir string
	now       func() time.Time
	log       *slog.Logger
	mu        sync.Mutex
	wake      chan struct{}
}

func New(st *store.Store, backupDir string, now func() time.Time, log *slog.Logger) *Service {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{st: st, backupDir: backupDir, now: now, log: log, wake: make(chan struct{}, 1)}
}

func (s *Service) Settings() (Settings, error) {
	out := DefaultSettings()
	v, ok, err := s.st.ConfigGet(settingsKey)
	if err != nil || !ok {
		return out, err
	}
	return out, json.Unmarshal([]byte(v), &out)
}

func (s *Service) SetSettings(v Settings) error {
	if err := v.Validate(); err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	if err := s.st.ConfigSet(settingsKey, string(b)); err != nil {
		return err
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *Service) LastResult() (*Result, error) {
	v, ok, err := s.st.ConfigGet(resultKey)
	if err != nil || !ok {
		return nil, err
	}
	var r Result
	return &r, json.Unmarshal([]byte(v), &r)
}

// Run performs backup -> cleanup -> compaction now. The returned error is the
// first failure; the persisted Result records it as well.
func (s *Service) Run() (Result, error) { return s.run("manual") }

func (s *Service) run(trigger string) (Result, error) {
	if !s.mu.TryLock() {
		return Result{}, ErrBusy
	}
	defer s.mu.Unlock()
	res := Result{StartedAt: s.now().UTC().Format(time.RFC3339), Trigger: trigger, Deleted: map[string]int64{}}
	err := s.runLocked(&res)
	if err != nil {
		res.Error = err.Error()
		s.log.Warn("maintenance run", "err", err)
	}
	res.FinishedAt = s.now().UTC().Format(time.RFC3339)
	if b, jerr := json.Marshal(res); jerr == nil {
		if cerr := s.st.ConfigSet(resultKey, string(b)); cerr != nil && err == nil {
			err = cerr
		}
	}
	return res, err
}

func (s *Service) runLocked(res *Result) error {
	set, err := s.Settings()
	if err != nil {
		return err
	}
	if res.SizeBefore, err = s.size(); err != nil {
		return err
	}
	res.SizeAfter = res.SizeBefore
	if res.Backup, err = s.backup(set.KeepBackups); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	var errs []error
	if set.RetentionDays > 0 {
		cutoff := s.now().UTC().AddDate(0, 0, -set.RetentionDays).Format(time.RFC3339Nano)
		errs = append(errs, s.cleanup(cutoff, res.Deleted))
	}
	if set.Compact {
		res.Compacted, err = s.compact(set.CompactThresholdPct)
		errs = append(errs, err)
	}
	if res.SizeAfter, err = s.size(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// size is the logical database size: page_count * page_size.
func (s *Service) size() (int64, error) {
	var pages, pageSize int64
	if err := s.st.DB.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	err := s.st.DB.QueryRow(`PRAGMA page_size`).Scan(&pageSize)
	return pages * pageSize, err
}

// backup writes a consistent copy with VACUUM INTO while the daemon keeps
// running, then keeps only the newest keep backups.
func (s *Service) backup(keep int) (string, error) {
	if err := os.MkdirAll(s.backupDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(s.backupDir, 0o700); err != nil {
		return "", err
	}
	name := "tariboyd-" + s.now().UTC().Format(backupStamp) + ".db"
	final := filepath.Join(s.backupDir, name)
	tmp := final + ".partial"
	// Leftovers of interrupted runs are never valid backups.
	if stale, _ := filepath.Glob(filepath.Join(s.backupDir, "*.partial")); len(stale) > 0 {
		for _, f := range stale {
			_ = os.Remove(f)
		}
	}
	if _, err := s.st.DB.Exec(`VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(s.backupDir, "tariboyd-*.db"))
	if err != nil {
		return final, err
	}
	// Rotate only files this service named, never an operator's own copy.
	var all []string
	for _, f := range matches {
		stamp := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "tariboyd-"), ".db")
		if _, err := time.Parse(backupStamp, stamp); err == nil {
			all = append(all, f)
		}
	}
	sort.Strings(all) // UTC timestamps sort chronologically
	for _, old := range all[:max(0, len(all)-keep)] {
		if err := os.Remove(old); err != nil {
			return final, err
		}
	}
	return final, nil
}

// older compares variable-width RFC3339 text through SQLite's time parser.
func older(col string) string {
	return `strftime('%Y-%m-%dT%H:%M:%f',` + col + `) < strftime('%Y-%m-%dT%H:%M:%f',?)`
}

// cleanup deletes each category in its own transaction, so one failure rolls
// back only that category.
func (s *Service) cleanup(cutoff string, deleted map[string]int64) error {
	var errs []error
	step := func(name string, fn func(tx *sql.Tx) (int64, error)) bool {
		tx, err := s.st.DB.Begin()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return false
		}
		n, err := fn(tx)
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return false
		}
		deleted[name] = n
		return true
	}
	simple := func(q string) func(tx *sql.Tx) (int64, error) {
		return func(tx *sql.Tx) (int64, error) {
			r, err := tx.Exec(q, cutoff)
			if err != nil {
				return 0, err
			}
			return r.RowsAffected()
		}
	}
	var purged []string
	if step("tasks", func(tx *sql.Tx) (n int64, err error) {
		n, purged, err = purgeTasks(tx, cutoff)
		return n, err
	}) {
		errs = append(errs, s.removeTaskFiles(purged))
	}
	step("ai_requests", func(tx *sql.Tx) (int64, error) {
		// A rolling budget sums spend over its own period, which may be
		// longer than the retention period; keep what it can still count.
		aiCutoff := cutoff
		var longest int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(period_s),0) FROM budgets`).Scan(&longest); err != nil {
			return 0, err
		}
		if c := s.now().UTC().Add(-time.Duration(longest) * time.Second).Format(time.RFC3339Nano); longest > 0 && c < aiCutoff {
			aiCutoff = c
		}
		r, err := tx.Exec(`DELETE FROM ai_requests WHERE `+older("ts"), aiCutoff)
		if err != nil {
			return 0, err
		}
		return r.RowsAffected()
	})
	step("messages", func(tx *sql.Tx) (int64, error) { return purgeMessages(tx, cutoff) })
	step("events", simple(`DELETE FROM events WHERE `+older("ts")))
	step("task_idempotency", simple(`DELETE FROM task_idempotency WHERE `+older("created_at")))
	return errors.Join(errs...)
}

// purgeTasks removes whole task trees whose every node is done or cancelled
// before cutoff, together with the rows that reference them, and returns the
// keys of the removed tasks. A tree related to a task that is not being
// removed is kept.
func purgeTasks(tx *sql.Tx, cutoff string) (int64, []string, error) {
	stmts := []string{
		`CREATE TEMP TABLE IF NOT EXISTS purge_tasks(id INTEGER PRIMARY KEY, root INTEGER NOT NULL)`,
		`DELETE FROM purge_tasks`,
		`INSERT INTO purge_tasks(id, root)
		 WITH RECURSIVE tree(id, root) AS (
		   SELECT id, id FROM tasks WHERE parent_id IS NULL
		   UNION ALL SELECT t.id, tree.root FROM tasks t JOIN tree ON t.parent_id = tree.id)
		 SELECT tree.id, tree.root FROM tree WHERE tree.root NOT IN (
		   SELECT tree.root FROM tree JOIN tasks t ON t.id = tree.id
		   WHERE t.status NOT IN ('done','cancelled') OR t.completed_at = '' OR NOT ` + older("t.completed_at") + `)`,
	}
	for _, q := range stmts {
		args := []any{}
		if strings.Contains(q, "?") {
			args = append(args, cutoff)
		}
		if _, err := tx.Exec(q, args...); err != nil {
			return 0, nil, err
		}
	}
	// Drop trees linked to a kept task until stable: dropping one tree can
	// strand another that was only linked to it.
	for {
		r, err := tx.Exec(`DELETE FROM purge_tasks WHERE root IN (
			SELECT p.root FROM task_relations r
			JOIN purge_tasks p ON p.id IN (r.source_id, r.target_id)
			WHERE r.source_id NOT IN (SELECT id FROM purge_tasks) OR r.target_id NOT IN (SELECT id FROM purge_tasks))`)
		if err != nil {
			return 0, nil, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			break
		}
	}
	rows, err := tx.Query(`SELECT t.task_key FROM tasks t JOIN purge_tasks p ON p.id = t.id ORDER BY t.id`)
	if err != nil {
		return 0, nil, err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return 0, nil, err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, nil, err
	}
	const in = ` IN (SELECT id FROM purge_tasks)`
	for _, q := range []string{
		`DELETE FROM task_waiting_for WHERE task_id` + in,
		`DELETE FROM task_comments WHERE task_id` + in,
		`DELETE FROM task_notification_state WHERE notification_id IN (SELECT o.notification_id FROM task_notification_outbox o
			JOIN task_events e ON e.sequence = o.event_sequence WHERE e.task_id` + in + `)`,
		`DELETE FROM task_notification_outbox WHERE event_sequence IN (SELECT sequence FROM task_events WHERE task_id` + in + `)`,
		`DELETE FROM task_events WHERE task_id` + in,
		`DELETE FROM task_relations WHERE source_id` + in + ` OR target_id` + in,
	} {
		if _, err := tx.Exec(q); err != nil {
			return 0, nil, err
		}
	}
	// parent_id is ON DELETE RESTRICT, which SQLite checks per row, so
	// delete leaves first, one tree level per statement.
	var total int64
	for {
		r, err := tx.Exec(`DELETE FROM tasks WHERE id` + in + ` AND NOT EXISTS (SELECT 1 FROM tasks c WHERE c.parent_id = tasks.id)`)
		if err != nil {
			return 0, nil, err
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			return total, keys, nil
		}
		total += n
	}
}

// removeTaskFiles removes the directories of purged tasks. A key that is not a
// single local path element is refused, never joined into a path.
func (s *Service) removeTaskFiles(keys []string) error {
	if s.TasksDir == "" {
		return nil
	}
	var errs []error
	for _, key := range keys {
		if !tasks.IsTaskDirKey(key) {
			s.log.Warn("maintenance keeps the files of a purged task whose key is not a directory name", "task", key)
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.TasksDir, key)); err != nil {
			errs = append(errs, fmt.Errorf("task files of %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// purgeMessages removes old messages whose every delivery is acknowledged or
// dead-lettered, with those deliveries. Unacknowledged work and messages
// workflow ingress has not consumed yet are never deleted.
func purgeMessages(tx *sql.Tx, cutoff string) (int64, error) {
	victims := `(SELECT id FROM messages m WHERE ` + older("m.ts") + ` AND NOT EXISTS (
		SELECT 1 FROM deliveries d WHERE d.message_id = m.id AND d.acked_at IS NULL AND d.dlq = 0)
		AND NOT EXISTS (SELECT 1 FROM task_workflow_message_sequence q
		  JOIN task_workflow_ingress_state i ON i.singleton = 1
		  WHERE q.message_id = m.id AND q.sequence > i.last_message_sequence))`
	if _, err := tx.Exec(`DELETE FROM deliveries WHERE message_id IN `+victims, cutoff); err != nil {
		return 0, err
	}
	// Deliveries of the victims are gone, so re-selecting by age alone plus the
	// same guard deletes exactly the same messages.
	r, err := tx.Exec(`DELETE FROM messages WHERE id IN `+victims, cutoff)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// compact runs VACUUM when at least thresholdPct of pages are free, then
// truncates the WAL. The daemon's single connection waits meanwhile.
func (s *Service) compact(thresholdPct int) (bool, error) {
	var free, pages int64
	if err := s.st.DB.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		return false, err
	}
	if err := s.st.DB.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return false, err
	}
	if pages == 0 || free == 0 || free*100 < int64(thresholdPct)*pages {
		return false, nil
	}
	if _, err := s.st.DB.Exec(`VACUUM`); err != nil {
		return false, fmt.Errorf("vacuum: %w", err)
	}
	if _, err := s.st.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return true, fmt.Errorf("wal checkpoint: %w", err)
	}
	return true, nil
}

// nextRun is the first HH:MM strictly after now, in now's location.
func nextRun(now time.Time, hhmm string) time.Time {
	t, _ := time.Parse("15:04", hhmm)
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// Loop runs the scheduled maintenance until ctx ends. It polls the wall
// clock, so a slot is honored across system sleep, and a settings change
// re-plans the next slot. A slot noticed more than lateGrace late is skipped.
func (s *Service) Loop(ctx context.Context, after func(time.Duration) <-chan time.Time) {
	if after == nil {
		after = time.After
	}
	var planned time.Time
	for {
		set, err := s.Settings()
		if err != nil {
			s.log.Warn("maintenance settings", "err", err)
			set = DefaultSettings()
		}
		if planned.IsZero() {
			planned = nextRun(s.now(), set.Time)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			planned = time.Time{}
			continue
		case <-after(pollInterval):
		}
		now := s.now()
		if now.Before(planned) {
			continue
		}
		if set.Enabled && now.Sub(planned) <= lateGrace {
			_, _ = s.run("scheduled")
		}
		planned = nextRun(maxTime(now, planned), set.Time)
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
