package tasks

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	// defaultRunLogBytes is the log tail returned when the caller names no size.
	defaultRunLogBytes = 64 << 10
	// maxRunLogBytes is the largest log tail a caller may ask for.
	maxRunLogBytes = 1 << 20
	// minRedactedSecret is the shortest queue secret value replaced in a log;
	// shorter values would blank ordinary words.
	minRedactedSecret = 6
	redactedMarker    = "[redacted]"
)

func runLogUnavailable(id int64) error {
	return domainError(http.StatusNotFound, "not_found", "script run "+strconv.FormatInt(id, 10)+" has no readable log")
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
	if maxBytes <= 0 {
		maxBytes = defaultRunLogBytes
	}
	maxBytes = min(maxBytes, maxRunLogBytes)
	task, _, err := artifactTaskTx(ctx, s.db, actor, key)
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
	if run.LogPath == "" {
		return "", false, runLogUnavailable(id)
	}
	path, err := s.verifiedRunLogPath(task.Key, run.ID, run.LogPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, runLogUnavailable(id)
	}
	if err != nil {
		return "", false, err
	}
	raw, size, err := readLogTail(path, maxBytes+MaxQueueSecretBytes)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, runLogUnavailable(id)
	}
	if err != nil {
		return "", false, runLogInvalid(id)
	}
	secrets, err := s.queueSecrets(ctx, task.Queue)
	if err != nil {
		return "", false, err
	}
	text := redactSecrets(validUTF8Tail(raw, size > int64(len(raw))), secrets)
	if len(text) > maxBytes {
		text = string(cutTail([]byte(text), maxBytes))
	}
	return strings.ToValidUTF8(text, "�"), size > int64(maxBytes), nil
}

// verifiedRunLogPath returns the real path of the log of run id of task key,
// provided logPath names exactly that file under the base directory and no
// component of it is a symlink.
func (s *Service) verifiedRunLogPath(key string, id int64, logPath string) (string, error) {
	if s.runBaseDir == "" || key == "" || !filepath.IsLocal(key) || strings.ContainsAny(key, `/\`) {
		return "", runLogInvalid(id)
	}
	base, err := filepath.Abs(s.runBaseDir)
	if err != nil {
		return "", runLogInvalid(id)
	}
	rel := filepath.Join("tasks", key, "runs", strconv.FormatInt(id, 10), "run.log")
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

// validUTF8Tail drops the bytes of a rune cut by the start of the slice, when
// cut says the slice starts inside the file.
func validUTF8Tail(raw []byte, cut bool) string {
	if cut {
		raw = skipPartialRune(raw)
	}
	return string(raw)
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

// redactSecrets replaces every value of at least minRedactedSecret bytes with
// the redacted marker, longest first so a value that contains another is
// replaced whole.
func redactSecrets(text string, secrets map[string]string) string {
	var values []string
	for _, value := range secrets {
		if len(value) >= minRedactedSecret {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return text
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
	pairs := make([]string, 0, 2*len(values))
	for _, value := range values {
		pairs = append(pairs, value, redactedMarker)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}
