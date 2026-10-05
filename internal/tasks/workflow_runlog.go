package tasks

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

const (
	// defaultRunLogBytes is the log tail returned when the caller names no size.
	defaultRunLogBytes = 64 << 10
	// maxRunLogBytes is the largest log tail a caller may ask for.
	maxRunLogBytes = 1 << 20
)

func runLogUnavailable(id int64) error {
	return domainError(http.StatusNotFound, "not_found", "script run "+strconv.FormatInt(id, 10)+" has no readable log")
}

func invalidMaxBytes() error {
	return domainError(http.StatusBadRequest, "invalid_request", "max_bytes must be a non-negative whole number")
}

// ParseMaxBytes reads the max_bytes argument of a log request as it arrives
// from JSON or a query string: absent, empty, or 0 means the default; anything
// that is not a non-negative whole number is a 400.
func ParseMaxBytes(value any) (int, error) {
	switch v := value.(type) {
	case nil:
		return 0, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return 0, invalidMaxBytes()
		}
		return n, nil
	case float64:
		if v < 0 || v != math.Trunc(v) || v > math.MaxInt32 {
			return 0, invalidMaxBytes()
		}
		return int(v), nil
	case int:
		if v < 0 {
			return 0, invalidMaxBytes()
		}
		return v, nil
	case int64:
		if v < 0 || v > math.MaxInt32 {
			return 0, invalidMaxBytes()
		}
		return int(v), nil
	default:
		return 0, invalidMaxBytes()
	}
}

func runLogInvalid(id int64) error {
	return domainError(http.StatusConflict, "run_log_invalid",
		"the log of script run "+strconv.FormatInt(id, 10)+" is not where the worker writes it")
}

// ScriptRunLog returns the last maxBytes bytes of a run's log, cut to a valid
// UTF-8 boundary, with every queue secret value replaced by "[redacted]". A
// maxBytes of zero or less means 64 KiB; the largest is 1 MiB. truncated reports
// that the log is longer than the text returned. The stored path is trusted
// only when it is exactly <base>/tasks/<KEY>/runs/<id>/run.log for this task
// and run, with no symlink in it.
func (s *Service) ScriptRunLog(ctx context.Context, actor Actor, key string, id int64, maxBytes int) (string, bool, error) {
	if err := validateActor(actor); err != nil {
		return "", false, err
	}
	if maxBytes < 0 {
		return "", false, invalidMaxBytes()
	}
	task, _, err := workflowReadTaskTx(ctx, s.db, actor, key)
	if err != nil {
		return "", false, err
	}
	run, err := scanScriptRun(s.db.QueryRowContext(ctx, scriptRunSelect+` WHERE r.task_id = ? AND r.id = ?`, task.ID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, scriptRunNotFound(id)
	}
	if err != nil {
		return "", false, err
	}
	if err := s.requireRunLogAccess(ctx, actor, task, run.ScriptRun); err != nil {
		return "", false, err
	}
	if run.LogPath == "" {
		return "", false, runLogUnavailable(id)
	}
	if !IsTaskDirKey(task.Key) {
		return "", false, runLogInvalid(id)
	}
	rel := filepath.Join("tasks", task.Key, "runs", strconv.FormatInt(id, 10), "run.log")
	path, err := s.verifiedLogPath(rel, run.ID, run.LogPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, runLogUnavailable(id)
	}
	if err != nil {
		return "", false, err
	}
	return s.redactedLogTail(ctx, task.Queue, path, id, maxBytes)
}

// redactedLogTail reads the last maxBytes bytes of the log at path, cut to a
// valid UTF-8 boundary, with every secret of queue replaced by "[redacted]". A
// maxBytes of zero or less means 64 KiB; the largest is 1 MiB.
func (s *Service) redactedLogTail(ctx context.Context, queue, path string, id int64, maxBytes int) (string, bool, error) {
	if maxBytes <= 0 {
		maxBytes = defaultRunLogBytes
	}
	maxBytes = min(maxBytes, maxRunLogBytes)
	raw, size, err := readLogTail(path, maxBytes+MaxQueueSecretBytes)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, runLogUnavailable(id)
	}
	if err != nil {
		return "", false, runLogInvalid(id)
	}
	secrets, err := s.queueSecrets(ctx, queue)
	if err != nil {
		return "", false, err
	}
	// A window that starts inside the file reads MaxQueueSecretBytes ahead of
	// the asked-for tail, so a secret the start would cut is still matched.
	// Nothing from that lead is returned, however much the redaction shrinks
	// the tail.
	lead := 0
	if size > int64(len(raw)) {
		lead = max(len(raw)-maxBytes, 0)
	}
	text := redactSecretsFrom(string(raw), secretValues(secrets), lead)
	if size > int64(len(raw)) {
		text = string(skipPartialRune([]byte(text)))
	}
	if len(text) > maxBytes {
		text = string(cutTail([]byte(text), maxBytes))
	}
	return strings.ToValidUTF8(text, "�"), size > int64(maxBytes), nil
}

// requireRunLogAccess admits the customer, and an agent only when it is the
// agent a run_as agent run ran as, or, for a queue or watch run, a holder of
// the task. A run as the agent has that agent's own secrets in its
// environment, which the queue-secret redaction does not cover; one recorded
// without an agent is the customer's alone.
func (s *Service) requireRunLogAccess(ctx context.Context, actor Actor, task Task, run ScriptRun) error {
	if actor.IsCustomer {
		return nil
	}
	allowed := false
	switch {
	case run.Holder != "":
		allowed = actor.Principal == agentPrincipal(run.Holder)
	case run.RunAs == workflowfile.RunAsAgent:
	default:
		rows, err := s.db.QueryContext(ctx, `SELECT agent FROM task_workflow_holders WHERE task_id = ?`, task.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var agent string
			if err := rows.Scan(&agent); err != nil {
				return err
			}
			allowed = allowed || actor.Principal == agentPrincipal(agent)
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if !allowed {
		return domainError(http.StatusForbidden, "forbidden",
			"the log of script run "+strconv.FormatInt(run.ID, 10)+" is available to the task's holder and the customer")
	}
	return nil
}

// verifiedLogPath returns the real path of the log of run id, provided logPath
// names exactly rel under the base directory and no component of it is a
// symlink.
func (s *Service) verifiedLogPath(rel string, id int64, logPath string) (string, error) {
	if s.runBaseDir == "" {
		return "", runLogInvalid(id)
	}
	base, err := filepath.Abs(s.runBaseDir)
	if err != nil {
		return "", runLogInvalid(id)
	}
	if filepath.Clean(logPath) != filepath.Join(base, rel) {
		return "", runLogInvalid(id)
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", runLogInvalid(id)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(base, rel))
	if err != nil {
		return "", err
	}
	if resolved != filepath.Join(realBase, rel) {
		return "", runLogInvalid(id)
	}
	return resolved, nil
}

// readLogTail reads at most the last window bytes of the regular file at path
// and returns them with the file's size. It does not follow a symlink.
func readLogTail(path string, window int) ([]byte, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("run log is not a regular file")
	}
	size := info.Size()
	offset := max(size-int64(window), 0)
	raw, err := io.ReadAll(io.NewSectionReader(f, offset, size-offset))
	return raw, size, err
}

// skipPartialRune drops leading continuation bytes.
func skipPartialRune(raw []byte) []byte {
	for len(raw) > 0 && !utf8.RuneStart(raw[0]) {
		raw = raw[1:]
	}
	return raw
}

// cutTail returns the last n bytes of raw, starting on a rune boundary.
func cutTail(raw []byte, n int) []byte {
	if len(raw) <= n {
		return raw
	}
	return skipPartialRune(raw[len(raw)-n:])
}
