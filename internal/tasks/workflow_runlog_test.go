package tasks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

func TestPendingRequestCarriesWaitSeconds(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	ctx := context.Background()
	// runDefinition: ci.sh has no timeout (60s), branch.sh 90s, plus 30s.
	want := 60 + 90 + 30
	request, first := advanceApprove(t, svc, actor, task)
	if request.WaitSeconds != want {
		t.Fatalf("Advance wait_seconds = %d; want %d", request.WaitSeconds, want)
	}
	got, err := svc.GetTransitionRequest(ctx, AgentActor("reviewer-1"), task.Key, request.ID)
	if err != nil || got.WaitSeconds != want || got.State != "pending" || got.ID != request.ID {
		t.Fatalf("GetTransitionRequest = %#v, %v", got, err)
	}
	complete(t, svc, first.ID, RunCompletion{Verdict: "reject", Message: "no"})
	got, err = svc.GetTransitionRequest(ctx, actor, task.Key, request.ID)
	if err != nil || got.State != "rejected" || got.ResultMessage != "no" || got.WaitSeconds != 0 {
		t.Fatalf("finished request = %#v, %v", got, err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "wait_seconds") {
		t.Fatalf("finished request json = %s", raw)
	}
}

func TestWaitSecondsSumsExplicitTimeouts(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	def := runDefinition()
	def.Statuses[1].Transitions[0].Checks = []workflowfile.Check{
		{Script: "checks/ci.sh", Timeout: "10s"},
		{Script: "checks/branch.sh", Timeout: "2m"},
	}
	rewriteDefinition(t, svc, task, def)
	request, _ := advanceApprove(t, svc, actor, task)
	if want := 10 + 120 + 30; request.WaitSeconds != want {
		t.Fatalf("wait_seconds = %d; want %d", request.WaitSeconds, want)
	}
}

func TestAnAppliedRequestHasNoWaitSeconds(t *testing.T) {
	svc, _, task := requestFixture(t)
	for _, name := range []string{"plan", "summary"} {
		if _, err := svc.SetArtifact(context.Background(), AgentActor("dev-1"), task.Key, name, "x"); err != nil {
			t.Fatal(err)
		}
	}
	request, err := svc.Advance(context.Background(), AgentActor("dev-1"), task.Key, AdvanceInput{Outcome: "ready"})
	if err != nil || request.State != "applied" || request.WaitSeconds != 0 {
		t.Fatalf("request = %#v, %v", request, err)
	}
}

func TestGetTransitionRequestIsScopedToTheTask(t *testing.T) {
	svc, actor, task, _ := runFixture(t)
	ctx := context.Background()
	request, _ := advanceApprove(t, svc, actor, task)
	if _, err := svc.GetTransitionRequest(ctx, AgentActor("stranger"), task.Key, request.ID); ErrorCode(err) != "not_found" {
		t.Fatalf("stranger: %v", err)
	}
	other := mustCreateDev(t, svc, actor, "other")
	if _, err := svc.GetTransitionRequest(ctx, actor, other.Key, request.ID); ErrorCode(err) != "not_found" {
		t.Fatalf("request of another task: %v", err)
	}
	if _, err := svc.GetTransitionRequest(ctx, actor, task.Key, request.ID+100); ErrorCode(err) != "not_found" {
		t.Fatalf("missing request: %v", err)
	}
}

// logFixture is a task with one run whose log lives at the path the worker
// would use under a temporary base directory.
func logFixture(t *testing.T) (*Service, Actor, Task, ScriptRun, string) {
	t.Helper()
	svc, actor, task, _ := runFixture(t)
	base := t.TempDir()
	svc.SetRunBaseDir(base)
	_, run := advanceApprove(t, svc, actor, task)
	dir := filepath.Join(base, "tasks", task.Key, "runs", strconv.FormatInt(run.ID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run.log")
	setLogPath(t, svc, run.ID, path)
	return svc, actor, task, run, path
}

func setLogPath(t *testing.T, svc *Service, id int64, path string) {
	t.Helper()
	if _, err := svc.db.Exec(`UPDATE task_script_runs SET log_path = ? WHERE id = ?`, path, id); err != nil {
		t.Fatal(err)
	}
}

func TestScriptRunLogReadsTheLog(t *testing.T) {
	svc, actor, task, run, path := logFixture(t)
	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, truncated, err := svc.ScriptRunLog(context.Background(), AgentActor("reviewer-1"), task.Key, run.ID, 0)
	if err != nil || text != "hello\nworld\n" || truncated {
		t.Fatalf("log = %q, %v, %v", text, truncated, err)
	}
	text, truncated, err = svc.ScriptRunLog(context.Background(), actor, task.Key, run.ID, 6)
	if err != nil || text != "world\n" || !truncated {
		t.Fatalf("tail = %q, %v, %v", text, truncated, err)
	}
	if _, _, err := svc.ScriptRunLog(context.Background(), AgentActor("stranger"), task.Key, run.ID, 0); ErrorCode(err) != "not_found" {
		t.Fatalf("stranger: %v", err)
	}
}

func TestScriptRunLogRefusesUnsafePaths(t *testing.T) {
	svc, actor, task, run, path := logFixture(t)
	ctx := context.Background()
	base := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not a log"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A path outside the task's run directory, even a real file.
	setLogPath(t, svc, run.ID, outside)
	if _, _, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 0); err == nil || ErrorStatus(err) != 409 {
		t.Fatalf("outside path: %v", err)
	}
	// The run directory of another run of the same task.
	otherDir := filepath.Join(base, "tasks", task.Key, "runs", "999")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "run.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	setLogPath(t, svc, run.ID, filepath.Join(otherDir, "run.log"))
	if _, _, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 0); ErrorStatus(err) != 409 {
		t.Fatalf("other run's log: %v", err)
	}
	// A path that climbs out and back in is cleaned to the expected one.
	setLogPath(t, svc, run.ID, filepath.Join(filepath.Dir(path), "..", strconv.FormatInt(run.ID, 10), "run.log"))
	if err := os.WriteFile(path, []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if text, _, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 0); err != nil || text != "fine" {
		t.Fatalf("cleaned path: %q, %v", text, err)
	}
	// A symlinked run.log.
	setLogPath(t, svc, run.ID, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if text, _, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 0); ErrorStatus(err) != 409 || strings.Contains(text, "not a log") {
		t.Fatalf("symlink: %q, %v", text, err)
	}
	// A symlinked run directory.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	realDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(realDir, "run.log"), []byte("elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Dir(path)
	if err := os.RemoveAll(runDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, runDir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 0); ErrorStatus(err) != 409 {
		t.Fatalf("symlinked directory: %v", err)
	}
}

func TestScriptRunLogMissingAndUnsetAreNotFound(t *testing.T) {
	svc, actor, task, run, _ := logFixture(t)
	ctx := context.Background()
	if _, _, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 0); ErrorCode(err) != "not_found" {
		t.Fatalf("missing file: %v", err)
	}
	setLogPath(t, svc, run.ID, "")
	if _, _, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 0); ErrorCode(err) != "not_found" {
		t.Fatalf("no log path: %v", err)
	}
	if _, _, err := svc.ScriptRunLog(ctx, actor, task.Key, 9999, 0); ErrorStatus(err) != 404 {
		t.Fatalf("missing run: %v", err)
	}
}

func TestScriptRunLogClampsTheTail(t *testing.T) {
	svc, actor, task, run, path := logFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 2<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	text, truncated, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 10<<20)
	if err != nil || len(text) != 1<<20 || !truncated {
		t.Fatalf("clamped to max: len %d truncated %v err %v", len(text), truncated, err)
	}
	text, truncated, err = svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 0)
	if err != nil || len(text) != 64<<10 || !truncated {
		t.Fatalf("default: len %d truncated %v err %v", len(text), truncated, err)
	}
}

func TestScriptRunLogReturnsValidUTF8(t *testing.T) {
	svc, actor, task, run, path := logFixture(t)
	ctx := context.Background()
	// "é-ok" is 5 bytes, then an invalid byte, then "end".
	if err := os.WriteFile(path, []byte("é-ok\xffend"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, truncated, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 0)
	if err != nil || truncated || text != "é-ok�end" {
		t.Fatalf("whole = %q, %v, %v", text, truncated, err)
	}
	// The last 8 bytes start inside the two-byte rune; the cut moves forward
	// to a rune boundary.
	text, truncated, err = svc.ScriptRunLog(ctx, actor, task.Key, run.ID, 8)
	if err != nil || !truncated || text != "-ok�end" {
		t.Fatalf("cut = %q, %v, %v", text, truncated, err)
	}
	if !utf8.ValidString(text) {
		t.Fatalf("invalid UTF-8: %q", text)
	}
}

func TestScriptRunLogRedactsQueueSecrets(t *testing.T) {
	svc, actor, task, run, path := logFixture(t)
	ctx := context.Background()
	for key, value := range map[string]string{"GH_TOKEN": "s3cr3t-value", "SHORT": "abc", "PREFIX": "s3cr3t"} {
		if err := svc.SetQueueSecret(ctx, actor, "DEV", key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("token=s3cr3t-value abc done\nagain s3cr3t-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, _, err := svc.ScriptRunLog(ctx, AgentActor("reviewer-1"), task.Key, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "s3cr3t") {
		t.Fatalf("secret leaked: %q", text)
	}
	if text != "token=[redacted] abc done\nagain [redacted]\n" {
		t.Fatalf("text = %q; short values stay, long ones are replaced", text)
	}
}

// TestScriptRunLogNeverReturnsTheLead: the tail window reads
// MaxQueueSecretBytes more than asked for so a secret cut by the window start
// is still matched. A redaction that shrinks the asked-for part must not pull
// that lead into the result.
func TestScriptRunLogNeverReturnsTheLead(t *testing.T) {
	svc, actor, task, run, path := logFixture(t)
	ctx := context.Background()
	secret := strings.Repeat("S", 50)
	if err := svc.SetQueueSecret(ctx, actor, "DEV", "GH_TOKEN", secret); err != nil {
		t.Fatal(err)
	}
	tail := secret + strings.Repeat("b", 50)
	content := strings.Repeat("x", 70000) + "LEAKED-LEAD" + tail
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	text, truncated, err := svc.ScriptRunLog(ctx, actor, task.Key, run.ID, len(tail))
	if err != nil || !truncated {
		t.Fatalf("log = %q, %v, %v", text, truncated, err)
	}
	if text != "[redacted]"+strings.Repeat("b", 50) {
		t.Fatalf("text = %q", text)
	}
}

func TestAgentActionsForRequestsAndRuns(t *testing.T) {
	svc, actor, task, run, path := logFixture(t)
	ctx := context.Background()
	holder, stranger := AgentActor("reviewer-1"), AgentActor("stranger")
	if err := os.WriteFile(path, []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requestID int64
	if err := svc.db.QueryRow(`SELECT id FROM task_transition_requests WHERE task_id = ?`, task.ID).Scan(&requestID); err != nil {
		t.Fatal(err)
	}

	got, err := svc.AgentAction(ctx, holder, "request_get", map[string]any{"key": task.Key, "id": float64(requestID)})
	if err != nil || got.(TransitionRequest).ID != requestID {
		t.Fatalf("request_get = %#v, %v", got, err)
	}
	got, err = svc.AgentAction(ctx, holder, "workflow_runs", map[string]any{"key": task.Key})
	list, _ := got.(map[string]any)
	if err != nil || list["count"] != 1 || list["runs"].([]ScriptRun)[0].ID != run.ID {
		t.Fatalf("workflow_runs = %#v, %v", got, err)
	}
	got, err = svc.AgentAction(ctx, holder, "workflow_run_log", map[string]any{"key": task.Key, "id": float64(run.ID), "max_bytes": float64(3)})
	logged, _ := got.(map[string]any)
	if err != nil || logged["run_id"] != run.ID || logged["text"] != "ne\n" || logged["truncated"] != true {
		t.Fatalf("workflow_run_log = %#v, %v", got, err)
	}

	for action, body := range map[string]map[string]any{
		"request_get":      {"key": task.Key, "id": float64(requestID)},
		"workflow_runs":    {"key": task.Key},
		"workflow_run_log": {"key": task.Key, "id": float64(run.ID)},
	} {
		if _, err := svc.AgentAction(ctx, stranger, action, body); ErrorCode(err) != "not_found" {
			t.Errorf("%s as a stranger: %v", action, err)
		}
	}
	if _, err := svc.AgentAction(ctx, holder, "request_get", map[string]any{"key": task.Key}); ErrorStatus(err) != 400 {
		t.Errorf("request_get without id: %v", err)
	}
	_ = actor
}
