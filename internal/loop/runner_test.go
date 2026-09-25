package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/agentdir"
	"github.com/alekzonder/tariboy/internal/bus"
	"github.com/alekzonder/tariboy/internal/harness"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/shim"
	"github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
)

func TestAssemblePromptOrder(t *testing.T) {
	got := AssemblePrompt(PromptParts{
		Agent: "smoke", Cwd: "/w",
		ImagePrompt: "SYSTEM+BODY", Context: "CTX", UserPrompt: "STANDING",
		OneShot: "ONESHOT", Tail: "i-am-done",
	})
	// Header first, tail last, in the documented order.
	idx := func(s string) int { return strings.Index(got, s) }
	order := []string{"# You are agent smoke", "SYSTEM+BODY", "CTX", "ONESHOT", "STANDING", "i-am-done"}
	for i := 1; i < len(order); i++ {
		if idx(order[i-1]) < 0 || idx(order[i]) < 0 || idx(order[i-1]) >= idx(order[i]) {
			t.Fatalf("section %q not before %q in:\n%s", order[i-1], order[i], got)
		}
	}
	if !strings.Contains(got, "cwd: /w") {
		t.Fatalf("header missing cwd: %s", got)
	}
}

func TestAssemblePromptSkipsBlanks(t *testing.T) {
	got := AssemblePrompt(PromptParts{Agent: "a", Cwd: "/w", ImagePrompt: "BODY", Tail: "TAIL"})
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("blank sections not collapsed: %q", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "TAIL") {
		t.Fatalf("tail must be last: %q", got)
	}
}

func TestBuildEnv(t *testing.T) {
	env := BuildEnv([]string{"PATH=/usr/bin", "HOME=/h"}, "/a/bin", "smoke", "iter-1", "/a/agent.sock",
		false, "", "", map[string]string{"APP_ENV": "prod"}, map[string]string{"TOKEN": "s3cr3t"})
	m := map[string]string{}
	for _, kv := range env {
		if i := strings.Index(kv, "="); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	if m["TARIBOY_AGENT"] != "smoke" || m["TARIBOY_ITERATION"] != "iter-1" ||
		m["TARIBOY_TOOLS_SOCKET"] != "/a/agent.sock" {
		t.Fatalf("tariboy vars: %v", m)
	}
	if !strings.HasPrefix(m["PATH"], "/a/bin:") {
		t.Fatalf("agent bin not prepended to PATH: %q", m["PATH"])
	}
	if m["SHELL"] != "/bin/sh" {
		t.Fatalf("SHELL = %q, want /bin/sh", m["SHELL"])
	}
	if m["APP_ENV"] != "prod" || m["TOKEN"] != "s3cr3t" {
		t.Fatalf("env/secrets not injected: %v", m)
	}
}

func TestAgentShellCommandSourcesGlobalThenAgent(t *testing.T) {
	base := t.TempDir()
	layout := agentdir.New(filepath.Join(base, "agents"), "a1")
	if err := os.MkdirAll(layout.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "global-agent-shell.sh"), []byte("export ORDER=global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.ShellScriptPath(), []byte("export ORDER=$ORDER-agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := agentShellCommand("/bin/bash", base, layout, []string{"/bin/bash", "-c", `printf %s "$ORDER"`})
	output, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil || string(output) != "global-agent" {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestAgentShellCommandStopsOnSourceFailure(t *testing.T) {
	for _, location := range []string{"global", "agent"} {
		for _, script := range []string{"set +e; return 7\n", "false\nprintf SCRIPT_CONTINUED\n"} {
			t.Run(location+"/"+script, func(t *testing.T) {
				base := t.TempDir()
				layout := agentdir.New(filepath.Join(base, "agents"), "a1")
				if err := os.MkdirAll(layout.Root, 0o700); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(base, "global-agent-shell.sh")
				if location == "agent" {
					file = layout.ShellScriptPath()
				}
				if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
					t.Fatal(err)
				}
				argv := agentShellCommand("/bin/bash", base, layout, []string{"/bin/bash", "-c", "printf HARNESS_STARTED"})
				output, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
				var exit *exec.ExitError
				want := 1
				if strings.Contains(script, "return 7") {
					want = 7
				}
				if !errors.As(err, &exit) || exit.ExitCode() != want || len(output) != 0 {
					t.Fatalf("output=%q err=%v, want no output and exit %d", output, err, want)
				}
			})
		}
	}
}

func TestAgentShellCommandPreservesArgvAfterSet(t *testing.T) {
	base := t.TempDir()
	layout := agentdir.New(filepath.Join(base, "agents"), "a1")
	if err := os.MkdirAll(layout.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{filepath.Join(base, "global-agent-shell.sh"), layout.ShellScriptPath()} {
		if err := os.WriteFile(file, []byte("set -- one two /usr/bin/printf WRONG_COMMAND\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	argv := agentShellCommand("/bin/bash", base, layout, []string{"/bin/bash", "-c", `printf '<%s>' "$@"`, "harness", "two words", "", "*", "line\nbreak"})
	output, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil || string(output) != "<two words><><*><line\nbreak>" {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		exit   int
		done   bool
		softTO bool
		want   string
	}{
		{0, true, false, "done"},
		{143, true, false, "done"},
		{0, false, false, "no_i_am_done"},
		{7, false, false, "harness_error"},
		{143, false, true, "timeout"},
		{0, true, true, "timeout"}, // soft timeout wins even on a clean-looking exit
	}
	for _, c := range cases {
		if got := Classify(c.exit, c.done, c.softTO, false); got != c.want {
			t.Fatalf("Classify(%d,%v,%v) = %q, want %q", c.exit, c.done, c.softTO, got, c.want)
		}
	}
}

func TestClassifyHardWatchdogAsTimeout(t *testing.T) {
	if got := Classify(143, false, false, true); got != "timeout" {
		t.Fatalf("hard watchdog result classified %q, want timeout", got)
	}
}

func TestAssemblePromptMessagesOrder(t *testing.T) {
	got := AssemblePrompt(PromptParts{
		Agent: "smoke", Cwd: "/w",
		ImagePrompt: "BODY", Context: "CTX", Messages: "MSGS", UserPrompt: "STANDING", Tail: "TAIL",
	})
	idx := func(s string) int { return strings.Index(got, s) }
	order := []string{"BODY", "CTX", "MSGS", "STANDING", "TAIL"}
	for i := 1; i < len(order); i++ {
		if idx(order[i-1]) < 0 || idx(order[i]) < 0 || idx(order[i-1]) >= idx(order[i]) {
			t.Fatalf("section %q not before %q in:\n%s", order[i-1], order[i], got)
		}
	}
}

func TestFormatMessages(t *testing.T) {
	out := FormatMessages([]bus.Message{
		{ID: "m1", Type: "deploy.requested", Source: "agent:alice", Text: "ship it",
			Subject: map[string]any{"env": "prod"}},
		{ID: "m2", Type: "note", Source: "operator", Text: "fyi"},
	})
	// Each message is rendered with its id, and the standing processing
	// instruction is appended (spec §3.2): both are required now that the runner
	// no longer auto-acks the batch.
	for _, want := range []string{
		"# Messages", "deploy.requested", "agent:alice", "ship it", "env", "note", "fyi",
		"m1", "m2", `scripts/messages.sh message processed <id> "<what you did / result>"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("FormatMessages missing %q:\n%s", want, out)
		}
	}
}

func TestFormatMessagesRendersThreadingState(t *testing.T) {
	out := FormatMessages([]bus.Message{
		{ID: "r1", Type: "answer", Source: "agent:bob", Text: "done",
			Kind: "reply", InReplyTo: "req-9", CorrelationID: "corr-9"},
	})
	for _, want := range []string{"kind: reply", "in_reply_to: req-9", "correlation_id: corr-9"} {
		if !strings.Contains(out, want) {
			t.Fatalf("FormatMessages missing per-message state %q:\n%s", want, out)
		}
	}
}

func TestFormatMessagesEmpty(t *testing.T) {
	if got := FormatMessages(nil); got != "" {
		t.Fatalf("empty batch should render nothing, got %q", got)
	}
}

func TestFormatAwaitingReplies(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 30, 0, time.UTC)
	sent := now.Add(-90 * time.Second).Format(time.RFC3339Nano)
	out := FormatAwaitingReplies([]bus.Message{
		{ID: "req-1", Channel: "ops:deploys", TS: sent, Deadline: "2026-07-12T12:05:00Z"},
	}, now)
	for _, want := range []string{"# Awaiting replies", "req-1", "ops:deploys", "age 1m30s", "deadline 2026-07-12T12:05:00Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("FormatAwaitingReplies missing %q:\n%s", want, out)
		}
	}
	if got := FormatAwaitingReplies(nil, now); got != "" {
		t.Fatalf("no pending requests should render nothing, got %q", got)
	}
}

func TestBuildEnvInjectsProxy(t *testing.T) {
	out := BuildEnv([]string{"PATH=/x", "ANTHROPIC_API_KEY=real-secret"}, "/bin", "alice", "alice-1",
		"/sock", true, "http://127.0.0.1:5555", "sk-tariboy-tok", nil, nil)
	m := map[string]string{}
	for _, kv := range out {
		if i := strings.Index(kv, "="); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	if m["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:5555" {
		t.Fatalf("anthropic proxy env not set: %v", m)
	}
	if m["OPENAI_BASE_URL"] != "http://127.0.0.1:5555/v1" {
		t.Fatalf("openai proxy env not set: %v", m)
	}
	if m["ANTHROPIC_API_KEY"] != "real-secret" {
		t.Fatalf("real ANTHROPIC_API_KEY not preserved for harness auth: %v", m)
	}
	if _, ok := m["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Fatalf("proxy token must not replace provider auth token: %v", m)
	}
	if _, ok := m["OPENAI_API_KEY"]; ok {
		t.Fatalf("proxy token must not replace OPENAI_API_KEY: %v", m)
	}
}

func TestBuildEnvNoProxyKeepsEnv(t *testing.T) {
	out := BuildEnv([]string{"PATH=/x", "ANTHROPIC_API_KEY=real"}, "/bin", "a", "a-1", "/s", false, "", "", nil, nil)
	m := map[string]string{}
	for _, kv := range out {
		if i := strings.Index(kv, "="); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	// With no proxy configured, env is untouched (M4 behaviour).
	if m["ANTHROPIC_API_KEY"] != "real" || m["ANTHROPIC_BASE_URL"] != "" {
		t.Fatalf("no-proxy env changed: %v", m)
	}
}

func TestBuildEnvKeepsProviderKeysWhenProxyEnabledWithoutToken(t *testing.T) {
	out := BuildEnv([]string{"PATH=/x", "ANTHROPIC_API_KEY=REALKEY", "OPENAI_API_KEY=REALKEY2"},
		"/bin", "a", "a-1", "/s", true, "", "", nil, nil)
	m := map[string]string{}
	for _, kv := range out {
		if i := strings.Index(kv, "="); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	if m["ANTHROPIC_API_KEY"] != "REALKEY" {
		t.Fatalf("ANTHROPIC_API_KEY not preserved: %v", m)
	}
	if m["OPENAI_API_KEY"] != "REALKEY2" {
		t.Fatalf("OPENAI_API_KEY not preserved: %v", m)
	}
}

type doneGraceShimHandler struct {
	resultPath string
	kills      atomic.Int32
}

func (h *doneGraceShimHandler) Status() shim.StatusResult {
	return shim.StatusResult{Running: true}
}

func (h *doneGraceShimHandler) Kill() error {
	h.kills.Add(1)
	return os.WriteFile(h.resultPath, []byte(`{"exit_code":0}`), 0o600)
}

func (h *doneGraceShimHandler) Screen() (string, error) { return "", nil }

func (h *doneGraceShimHandler) SendKeys(shim.SendKeysParams) error { return nil }

func (h *doneGraceShimHandler) Report() shim.ReportResult {
	return shim.ReportResult{Finished: false}
}

func TestAwaitKillsAfterDoneGraceForAllModes(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(fmt.Sprintf("interactive=%v", interactive), func(t *testing.T) {
			base := t.TempDir()
			s, err := store.Open(filepath.Join(base, "x.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			as := agent.NewStore(s)

			ag := agent.Agent{Name: "alice", ImageRef: "basic:latest", Interactive: interactive}
			if err := as.Create(ag); err != nil {
				t.Fatal(err)
			}

			agentsDir := filepath.Join(base, "agents")
			runtimeDir, err := os.MkdirTemp("/tmp", "tariboy-loop-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
			l := agentdir.New(agentsDir, ag.Name).WithRuntime(runtimeDir)
			iterID := "alice-1"
			if err := l.EnsureIteration(iterID); err != nil {
				t.Fatal(err)
			}
			if err := as.CreateIteration(agent.Iteration{ID: iterID, Agent: ag.Name, Trigger: "manual", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if err := as.SetIterationDone(iterID, true); err != nil {
				t.Fatal(err)
			}

			ln, err := net.Listen("unix", l.ShimSock())
			if err != nil {
				t.Fatal(err)
			}
			handler := &doneGraceShimHandler{resultPath: l.ResultPath(iterID)}
			serveDone := make(chan struct{})
			go func() {
				_ = shim.Serve(ln, handler)
				close(serveDone)
			}()
			t.Cleanup(func() {
				_ = ln.Close()
				<-serveDone
			})

			r := NewShimRunner(RunnerConfig{
				AgentsDir: agentsDir, RuntimeDir: runtimeDir, Store: as,
				PollInterval: 5 * time.Millisecond, DoneGrace: 20 * time.Millisecond,
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			})

			start := time.Now()
			out, err := r.await(context.Background(), ag, l, iterID)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "done" || !out.DoneFlag {
				t.Fatalf("outcome = %+v, want done with done flag", out)
			}
			if handler.kills.Load() != 1 {
				t.Fatalf("kill calls = %d, want 1", handler.kills.Load())
			}
			if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
				t.Fatalf("kill happened before done grace elapsed: %v", elapsed)
			}
		})
	}
}

func TestPrepareUsesAgentNameAsTmuxSession(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TARIBOY_STUB_HARNESS", "/usr/bin/true")
	s, err := store.Open(filepath.Join(base, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	as := agent.NewStore(s)

	ag := agent.Agent{
		Name: "alice", ImageRef: "basic:latest", HarnessType: "stub", Interactive: true, Plugins: []string{"context"},
	}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	iterID := "alice-20260705213000-1"
	if err := as.CreateIteration(agent.Iteration{ID: iterID, Agent: ag.Name, Trigger: "manual", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	agentsDir := filepath.Join(base, "agents")
	l := agentdir.New(agentsDir, ag.Name)
	if err := l.EnsureIteration(iterID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.ImageDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.ImageDir(), "PROMPT.md"), []byte("BODY"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewShimRunner(RunnerConfig{
		AgentsDir: agentsDir, Store: as, ShimBin: "/opt/tariboy-shim",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	prep, err := r.prepare(context.Background(), otel.Tracer("test"), ag, l, iterID, "")
	if err != nil {
		t.Fatal(err)
	}
	for i, arg := range prep.shimArgv {
		if arg == "--tmux-session" {
			if i+1 >= len(prep.shimArgv) {
				t.Fatalf("--tmux-session missing value: %v", prep.shimArgv)
			}
			if prep.shimArgv[i+1] != ag.Name {
				t.Fatalf("tmux session = %q, want %q (argv %v)", prep.shimArgv[i+1], ag.Name, prep.shimArgv)
			}
			return
		}
	}
	t.Fatalf("--tmux-session missing from argv: %v", prep.shimArgv)
}

// failMintBinder simulates a proxy whose token mint fails, exercising the
// fail-closed path.
type failMintBinder struct {
	base    string
	revoked []string
}

func (b *failMintBinder) ProxyBaseURL() string { return b.base }
func (b *failMintBinder) MintToken(agent, iter, name, tag, digest string) (string, error) {
	return "", fmt.Errorf("mint boom")
}
func (b *failMintBinder) RevokeToken(token string)              { b.revoked = append(b.revoked, token) }
func (b *failMintBinder) RevokeIteration(string)                {}
func (b *failMintBinder) UpdateTask(key, task, epic string) int { return 0 }

// countingSpawner records how many times the harness would have been started.
type countingSpawner struct{ starts int }

func (s *countingSpawner) Start(argv, env []string, dir string) error { s.starts++; return nil }

// TestRunAbortsWhenMintFails asserts the fail-closed behaviour: when a proxy is
// configured but the token mint fails, the iteration must NOT spawn the harness
// (which would otherwise hit the real API directly with the operator's key) and
// must surface a failed status with no dangling token.
func TestRunAbortsWhenMintFails(t *testing.T) {
	binder := &failMintBinder{base: "http://127.0.0.1:5555"}
	r, ag, _, _ := newRunnerForProxyTest(t, binder)
	cs := &countingSpawner{}
	r.cfg.Spawner = cs

	_, err := r.Run(context.Background(), ag, "manual", "alice-1", "")
	if err == nil {
		t.Fatal("expected an error when proxy token mint fails, got nil")
	}
	if cs.starts != 0 {
		t.Fatalf("harness spawned despite mint failure: starts=%d", cs.starts)
	}
	if len(binder.revoked) != 0 {
		t.Fatalf("nothing was minted, so nothing should be revoked: %v", binder.revoked)
	}
}

// TestPrepareSpanEndedWithErrorStatusOnFailure is the telemetry-completeness
// regression test: a prepare-phase failure (here, the proxy token mint erroring
// out) must still end the "prepare" span — not leave it dangling — and mark it
// with an error status/recorded error, while shim.spawn/harness must never run.
func TestPrepareSpanEndedWithErrorStatusOnFailure(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(tracenoop.NewTracerProvider()) })

	binder := &failMintBinder{base: "http://127.0.0.1:5555"}
	r, ag, _, _ := newRunnerForProxyTest(t, binder)
	cs := &countingSpawner{}
	r.cfg.Spawner = cs

	_, err := r.Run(context.Background(), ag, "manual", "alice-1", "")
	if err == nil {
		t.Fatal("expected an error when proxy token mint fails, got nil")
	}
	if cs.starts != 0 {
		t.Fatalf("harness spawned despite prepare failure: starts=%d", cs.starts)
	}

	var prepFound, spawnFound, harnessFound bool
	var prepSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		switch s.Name() {
		case "prepare":
			prepFound = true
			prepSpan = s
		case "shim.spawn":
			spawnFound = true
		case "harness":
			harnessFound = true
		}
	}
	if !prepFound {
		t.Fatalf("prepare span was not ended/recorded on prepare-phase failure")
	}
	if prepSpan.Status().Code != codes.Error {
		t.Fatalf("prepare span status = %v, want codes.Error", prepSpan.Status().Code)
	}
	if len(prepSpan.Events()) == 0 {
		t.Fatalf("prepare span has no recorded error event")
	}
	if spawnFound || harnessFound {
		t.Fatalf("shim.spawn/harness must not run after a prepare failure: spawn=%v harness=%v", spawnFound, harnessFound)
	}
}

type fakeBinder struct {
	minted            []string
	revoked           []string
	revokedIterations []string
	updates           []struct{ key, task, epic string }
	failUpdate        bool
	base              string
}

func (b *fakeBinder) ProxyBaseURL() string { return b.base }
func (b *fakeBinder) ProxyBaseURLForToken(token string) string {
	return b.base + "/_tariboy/" + token
}
func (b *fakeBinder) MintToken(agent, iter, name, tag, digest string) (string, error) {
	tok := "tok-" + iter
	b.minted = append(b.minted, tok)
	return tok, nil
}
func (b *fakeBinder) RevokeToken(token string) { b.revoked = append(b.revoked, token) }
func (b *fakeBinder) RevokeIteration(iteration string) {
	b.revokedIterations = append(b.revokedIterations, iteration)
}
func (b *fakeBinder) UpdateTask(key, task, epic string) int {
	b.updates = append(b.updates, struct{ key, task, epic string }{key, task, epic})
	if b.failUpdate {
		return 0
	}
	return 1
}

// fakeProxySpawner writes an exit-0 result.json so ShimRunner.await returns
// promptly without spawning a real process.
type fakeProxySpawner struct {
	resultPath string
	argv       []string
	env        []string
}

func (s *fakeProxySpawner) Start(argv, env []string, dir string) error {
	s.argv = append([]string(nil), argv...)
	s.env = append([]string(nil), env...)
	return os.WriteFile(s.resultPath, []byte(`{"exit_code":0}`), 0o600)
}

func newRunnerForProxyTest(t *testing.T, binder ProxyBinder) (*ShimRunner, agent.Agent, agentdir.Layout, *agent.Store) {
	t.Helper()
	base := t.TempDir()
	// The stub harness only needs a path here; the fake spawner writes result.json
	// directly, so the script is never executed.
	t.Setenv("TARIBOY_STUB_HARNESS", "/usr/bin/true")
	s, err := store.Open(filepath.Join(base, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	as := agent.NewStore(s)

	ag := agent.Agent{Name: "alice", ImageRef: "basic:latest", ImageDigest: "sha256:abc",
		HarnessType: "stub", Plugins: []string{"context"}}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	if err := as.CreateIteration(agent.Iteration{ID: "alice-1", Agent: ag.Name, Trigger: "manual", Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}

	agentsDir := filepath.Join(base, "agents")
	l := agentdir.New(agentsDir, ag.Name)
	if err := os.MkdirAll(l.ImageDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.ImageDir(), "PROMPT.md"), []byte("BODY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.ImageDir(), "PROMPT_TAIL.md"), []byte("TAIL"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewShimRunner(RunnerConfig{
		AgentsDir: agentsDir, ShimBin: "/opt/tariboy-shim",
		Store: as, Spawner: &fakeProxySpawner{resultPath: l.ResultPath("alice-1")},
		Clock: time.Now, PollInterval: 5 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Proxy: binder,
	})
	return r, ag, l, as
}

func TestRunMintsAndRevokesToken(t *testing.T) {
	// Reuse the existing ShimRunner test harness (a stub spawner + fake result).
	// The key assertion: after a run, exactly one token was minted and then revoked.
	binder := &fakeBinder{base: "http://127.0.0.1:5555"}
	r, ag, l, _ := newRunnerForProxyTest(t, binder)
	_ = l
	if _, err := r.Run(context.Background(), ag, "manual", "alice-1", ""); err != nil {
		t.Fatal(err)
	}
	if len(binder.minted) != 1 || len(binder.revoked) != 1 || binder.minted[0] != binder.revoked[0] {
		t.Fatalf("mint/revoke mismatch: minted=%v revoked=%v", binder.minted, binder.revoked)
	}
}

func TestRunAttributesTokenToCurrentGoal(t *testing.T) {
	binder := &fakeBinder{base: "http://127.0.0.1:5555"}
	r, ag, _, _ := newRunnerForProxyTest(t, binder)
	goalReads := 0
	r.cfg.CurrentGoal = func(string, time.Time) (tasks.Task, bool, error) {
		goalReads++
		return tasks.Task{Key: "SUPER-3", ParentKey: "SUPER-1"}, true, nil
	}
	r.cfg.Tasks = &attributionTaskReader{tasks: map[string]tasks.Task{
		"SUPER-1": {Key: "SUPER-1"},
		"SUPER-3": {Key: "SUPER-3", ParentKey: "SUPER-1"},
	}}

	if _, err := r.Run(context.Background(), ag, "manual", "alice-1", ""); err != nil {
		t.Fatal(err)
	}
	if goalReads != 1 {
		t.Fatalf("CurrentGoal reads = %d, want 1", goalReads)
	}
	if len(binder.updates) != 1 {
		t.Fatalf("proxy attribution updates = %v, want one", binder.updates)
	}
	if got := binder.updates[0]; got.key != binder.minted[0] || got.task != "SUPER-3" || got.epic != "SUPER-1" {
		t.Fatalf("proxy attribution = %+v, want token/SUPER-3/SUPER-1", got)
	}
}

func TestRunFailsWhenGoalAttributionCannotBePersisted(t *testing.T) {
	binder := &fakeBinder{base: "http://127.0.0.1:5555", failUpdate: true}
	r, ag, _, _ := newRunnerForProxyTest(t, binder)
	r.cfg.CurrentGoal = func(string, time.Time) (tasks.Task, bool, error) {
		return tasks.Task{Key: "SUPER-1"}, true, nil
	}

	_, err := r.Run(context.Background(), ag, "manual", "alice-1", "")
	if err == nil || !strings.Contains(err.Error(), "attribute proxy token") {
		t.Fatalf("error = %v, want attribution failure", err)
	}
	if len(binder.revoked) != 1 || binder.revoked[0] != binder.minted[0] {
		t.Fatalf("minted=%v revoked=%v", binder.minted, binder.revoked)
	}
}

type cancelSpawner struct{ cancel context.CancelFunc }

func (s cancelSpawner) Start(_, _ []string, _ string) error {
	s.cancel()
	return nil
}

func TestShimRunnerCancellationDetachesLiveIteration(t *testing.T) {
	binder := &fakeBinder{base: "http://127.0.0.1:5555"}
	r, ag, _, as := newRunnerForProxyTest(t, binder)
	ctx, cancel := context.WithCancel(context.Background())
	r.cfg.Spawner = cancelSpawner{cancel: cancel}

	_, err := r.Run(ctx, ag, "manual", "alice-1", "")
	if !errors.Is(err, ErrIterationDetached) {
		t.Fatalf("Run error = %v, want ErrIterationDetached", err)
	}
	if got := len(binder.revoked); got != 0 {
		t.Fatalf("detached runner revoked %d tokens, want 0", got)
	}
	it, err := as.GetIteration(ag.Name, "alice-1")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "running" {
		t.Fatalf("detached iteration status = %q, want running", it.Status)
	}
}

type managedCancelSpawner struct {
	entered         chan struct{}
	proceed         chan struct{}
	outerTerminated chan struct{}
	childKilled     chan struct{}
	listener        net.Listener
}

func (s *managedCancelSpawner) Start(_, _ []string, _ string) error {
	close(s.entered)
	<-s.proceed
	return nil
}

func (s *managedCancelSpawner) StartManaged(argv, _ []string, _ string) (func() error, error) {
	close(s.entered)
	<-s.proceed
	sock := ""
	for i, arg := range argv {
		if arg == "--shim-sock" && i+1 < len(argv) {
			sock = argv[i+1]
			break
		}
	}
	if sock == "" {
		return nil, errors.New("missing --shim-sock")
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	s.listener = ln
	go func() {
		_ = shim.Serve(ln, managedChildShim{
			childKilled: s.childKilled,
		})
	}()
	return func() error {
		close(s.outerTerminated)
		return nil
	}, nil
}

type managedChildShim struct {
	childKilled chan struct{}
}

func (s managedChildShim) Status() shim.StatusResult { return shim.StatusResult{Running: true} }
func (s managedChildShim) Kill() error {
	close(s.childKilled)
	return nil
}
func (s managedChildShim) Screen() (string, error)            { return "", nil }
func (s managedChildShim) SendKeys(shim.SendKeysParams) error { return nil }
func (s managedChildShim) Report() shim.ReportResult          { return shim.ReportResult{} }

func TestShimRunnerTerminatesSpawnThatRacesStoppedIteration(t *testing.T) {
	binder := &fakeBinder{base: "http://127.0.0.1:5555"}
	r, ag, _, as := newRunnerForProxyTest(t, binder)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spawner := &managedCancelSpawner{
		entered: make(chan struct{}), proceed: make(chan struct{}),
		outerTerminated: make(chan struct{}), childKilled: make(chan struct{}),
	}
	r.cfg.Spawner = spawner
	t.Cleanup(func() {
		if spawner.listener != nil {
			spawner.listener.Close()
		}
	})

	runDone := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, ag, "manual", "alice-1", "")
		runDone <- err
	}()
	select {
	case <-spawner.entered:
	case <-time.After(time.Second):
		t.Fatal("runner did not reach shim spawn")
	}
	it, err := as.GetIteration(ag.Name, "alice-1")
	if err != nil {
		t.Fatal(err)
	}
	it.Status = "harness_error"
	it.EndedAt = time.Now().UTC().Format(time.RFC3339)
	if err := as.UpdateIteration(it); err != nil {
		t.Fatal(err)
	}
	cancel()
	close(spawner.proceed)

	select {
	case <-spawner.outerTerminated:
	case <-time.After(time.Second):
		t.Fatal("spawned shim was not terminated after Stop won the launch race")
	}
	select {
	case <-spawner.childKilled:
	case <-time.After(time.Second):
		t.Fatal("spawned shim child survived Stop winning the launch race")
	}
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
}

func TestRunRedactsCodexProxyAndInheritedOpenAIKeyFromLaunchRecords(t *testing.T) {
	const providerKey = "real-provider-key-for-test"
	t.Setenv("OPENAI_API_KEY", providerKey)
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	binder := &fakeBinder{base: "http://127.0.0.1:5555"}
	r, ag, _, _ := newRunnerForProxyTest(t, binder)
	ag.HarnessType = "codex"
	spawner := r.cfg.Spawner.(*fakeProxySpawner)
	recorder := &captureRecorder{}
	r.cfg.AuditFor = func(string) Recorder { return recorder }
	var logs bytes.Buffer
	r.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	if _, err := r.Run(context.Background(), ag, "manual", "alice-1", ""); err != nil {
		t.Fatal(err)
	}

	proxyToken := binder.minted[0]
	rawArgv := strings.Join(spawner.argv, "\n")
	if !strings.Contains(rawArgv, binder.ProxyBaseURLForToken(proxyToken)) {
		t.Error("spawned argv does not retain the tokenized proxy URL")
	}
	spawnedKeyPreserved := false
	for _, entry := range spawner.env {
		if entry == "OPENAI_API_KEY="+providerKey {
			spawnedKeyPreserved = true
			break
		}
	}
	if !spawnedKeyPreserved {
		t.Error("spawned env does not retain the inherited provider key")
	}

	logText := logs.String()
	if strings.Contains(logText, proxyToken) || strings.Contains(logText, providerKey) {
		t.Error("launch log contains credential material")
	}

	var launchData map[string]any
	for _, event := range recorder.snapshot() {
		if event.typ == "launching_harness" {
			launchData = event.data
			break
		}
	}
	if launchData == nil {
		t.Fatal("launching_harness audit event not recorded")
	}
	maskedArgv, argvOK := launchData["argv"].([]string)
	maskedEnv, envOK := launchData["env"].([]string)
	if !argvOK || !envOK {
		t.Fatal("launching_harness audit event has invalid argv/env fields")
	}
	maskedLaunch := strings.Join(append(append([]string{}, maskedArgv...), maskedEnv...), "\n")
	if strings.Contains(maskedLaunch, proxyToken) || strings.Contains(maskedLaunch, providerKey) {
		t.Error("launching_harness audit event contains credential material")
	}
	maskedProviderKey := false
	for _, entry := range maskedEnv {
		if entry == "OPENAI_API_KEY="+secretPlaceholder {
			maskedProviderKey = true
			break
		}
	}
	if !maskedProviderKey {
		t.Error("launching_harness audit env does not mask the provider key")
	}
}

// TestRunHappyPathRecordsAllPhaseSpans confirms the happy path is unchanged by
// the prepare-span fix: prepare, shim.spawn, and harness are each started and
// ended exactly once, and prepare is not left with an error status.
func TestRunHappyPathRecordsAllPhaseSpans(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(tracenoop.NewTracerProvider()) })

	binder := &fakeBinder{base: "http://127.0.0.1:5555"}
	r, ag, _, _ := newRunnerForProxyTest(t, binder)
	if _, err := r.Run(context.Background(), ag, "manual", "alice-1", ""); err != nil {
		t.Fatal(err)
	}

	counts := map[string]int{}
	var prepSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		counts[s.Name()]++
		if s.Name() == "prepare" {
			prepSpan = s
		}
	}
	for _, name := range []string{"prepare", "shim.spawn", "harness"} {
		if counts[name] != 1 {
			t.Fatalf("span %q recorded %d times, want 1 (all: %v)", name, counts[name], counts)
		}
	}
	if prepSpan.Status().Code == codes.Error {
		t.Fatalf("happy-path prepare span marked as error")
	}
}

func TestRunSnapshotsTimeoutFromOnePreSpawnClockSample(t *testing.T) {
	binder := &fakeBinder{base: "http://127.0.0.1:5555"}
	r, ag, _, as := newRunnerForProxyTest(t, binder)
	spawner := r.cfg.Spawner.(*fakeProxySpawner)
	wantHarnessArgv := []string{"/usr/bin/true", filepath.Join(r.cfg.AgentsDir, "alice", "iterations", "alice-1", "PROMPT.md")}
	ag.TimeoutS, ag.HardTimeoutS = 30, 90
	t0 := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(7 * time.Second)
	t2 := t1.Add(11 * time.Second)
	calls := 0
	r.cfg.Clock = func() time.Time {
		calls++
		if calls == 1 { // prompt rendering's clock read
			return t0
		}
		if calls == 2 { // the sole snapshot immediately before shim spawn
			return t1
		}
		return t2 // await's independent clock read
	}
	if _, err := r.Run(context.Background(), ag, "manual", "alice-1", ""); err != nil {
		t.Fatal(err)
	}
	got, err := as.GetIteration("alice", "alice-1")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || got.TimeoutDeadline == nil || got.HardTimeoutDeadline == nil ||
		*got.TimeoutDeadline != t1.Add(30*time.Second).Format(time.RFC3339Nano) ||
		*got.HardTimeoutDeadline != t1.Add(90*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("clock calls=%d snapshot=%+v, want deadlines based on sole pre-spawn sample %s", calls, got, t1)
	}
	separator := slices.Index(spawner.argv, "--")
	if separator < 0 {
		t.Fatalf("shim argv missing harness separator: %v", spawner.argv)
	}
	harnessArgv := spawner.argv[separator+1:]
	if len(harnessArgv) < len(wantHarnessArgv) || !slices.Equal(harnessArgv[len(harnessArgv)-len(wantHarnessArgv):], wantHarnessArgv) {
		t.Fatalf("harness argv = %v, want unchanged suffix %v", harnessArgv, wantHarnessArgv)
	}
	for i, arg := range spawner.argv[:separator] {
		if arg == "--hard-deadline" {
			if i+1 >= len(spawner.argv) || spawner.argv[i+1] != *got.HardTimeoutDeadline {
				t.Fatalf("shim hard deadline = %q, want persisted deadline %q (argv %v)", spawner.argv[i+1], *got.HardTimeoutDeadline, spawner.argv)
			}
			return
		}
	}
	t.Fatalf("shim argv missing persisted --hard-deadline: %v", spawner.argv)
}

// TestPrepareMaterializesSkills is the M15 skill-layout discriminant: when an
// agent's image carries a packed skill (image/skills/<name>/SKILL.md), running
// an iteration must lay it into the harness-native dir (<cwd>/.claude/skills)
// so the harness sees it. Modeled on TestPrepareUsesAgentNameAsTmuxSession.
func TestRunnerSchemaV1StillMaterializesPackedSkillsUnderCWD(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TARIBOY_STUB_HARNESS", "/usr/bin/true")
	s, err := store.Open(filepath.Join(base, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	as := agent.NewStore(s)

	ag := agent.Agent{
		Name: "alice", ImageRef: "img:latest", HarnessType: "stub", Plugins: []string{"context"},
	}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	iterID := "alice-20260706-1"
	if err := as.CreateIteration(agent.Iteration{ID: iterID, Agent: ag.Name, Trigger: "manual", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	agentsDir := filepath.Join(base, "agents")
	l := agentdir.New(agentsDir, ag.Name)
	if err := l.EnsureIteration(iterID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.Workdir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.ImageDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.ImageDir(), "PROMPT.md"), []byte("BODY"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(l.ImageDir(), "skills", "greeter")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Greeter"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewShimRunner(RunnerConfig{
		AgentsDir: agentsDir, Store: as, ShimBin: "/opt/tariboy-shim",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, err := r.prepare(context.Background(), otel.Tracer("test"), ag, l, iterID, ""); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	laid := filepath.Join(l.Workdir(), ".claude", "skills", "greeter", "SKILL.md")
	if _, err := os.Stat(laid); err != nil {
		t.Fatalf("skill not materialized at %s: %v", laid, err)
	}
}

func TestRunnerSchemaV2AttachesImageSkillBridgeWithoutChangingCWDOrHome(t *testing.T) {
	tests := []struct {
		name        string
		harnessType string
		interactive bool
	}{
		{name: "claude", harnessType: "claude"},
		{name: "codex-batch", harnessType: "codex"},
		{name: "codex-interactive", harnessType: "codex", interactive: true},
		{name: "opencode", harnessType: "opencode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			db, err := store.Open(filepath.Join(base, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			as := agent.NewStore(db)
			binDir := filepath.Join(base, "harness-bin")
			if err := os.MkdirAll(binDir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeHarnessExecutable(t, filepath.Join(binDir, tc.harnessType))
			writeHarnessExecutable(t, filepath.Join(binDir, "python3"))
			writeHarnessExecutable(t, filepath.Join(binDir, "bash"))
			home := filepath.Join(base, "home")
			cwd := filepath.Join(home, "project")
			if err := os.MkdirAll(cwd, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			digest := strings.Repeat("a", 64)
			ag := agent.Agent{
				Name: "worker", ImageRef: "reviewer:latest", ImageDigest: digest,
				HarnessType: tc.harnessType, Cwd: cwd, Interactive: tc.interactive, Env: map[string]string{"PATH": binDir},
			}
			if err := as.Create(ag); err != nil {
				t.Fatal(err)
			}
			iterID := "worker-1"
			entries := []image.TemplateEntry{{Kind: "runtime", Runtime: "identity"}}
			templateSHA, err := image.PromptTemplateHash(entries)
			if err != nil {
				t.Fatal(err)
			}
			if err := as.CreateIteration(agent.Iteration{
				ID: iterID, Agent: ag.Name, Trigger: "manual", Status: "running",
				ImageRef: ag.ImageRef, ImageDigest: digest, PromptTemplateSHA256: templateSHA,
			}); err != nil {
				t.Fatal(err)
			}
			agentsDir := filepath.Join(base, "agents")
			l := agentdir.New(agentsDir, ag.Name)
			if err := l.EnsureIteration(iterID); err != nil {
				t.Fatal(err)
			}
			promptDir := filepath.Join(l.ImageDir(), "prompt")
			if err := os.MkdirAll(promptDir, 0o700); err != nil {
				t.Fatal(err)
			}
			templateBody, err := json.Marshal(image.PromptTemplate{SchemaVersion: 2, Entries: entries, SHA256: templateSHA})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(promptDir, "template.json"), templateBody, 0o600); err != nil {
				t.Fatal(err)
			}
			packedSkill := filepath.Join(l.ImageDir(), "skills", "review")
			if err := os.MkdirAll(packedSkill, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(packedSkill, "SKILL.md"), []byte("packed"), 0o600); err != nil {
				t.Fatal(err)
			}
			adapter, err := harness.Get(tc.harnessType)
			if err != nil {
				t.Fatal(err)
			}
			bridgeDir := filepath.Join(l.ImageBridgesDir(), digest, harness.SkillAdapterContractVersion, tc.harnessType)
			bridge, err := adapter.SkillBridge(harness.SkillBridgeRequest{
				ImageName: "reviewer", ImageDigest: digest, BridgeDir: bridgeDir,
				Skills: []harness.SkillDescriptor{{Name: "review", Description: "Review changes.", TreeSHA256: strings.Repeat("b", 64)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			r := NewShimRunner(RunnerConfig{
				AgentsDir: agentsDir, Store: as, ShimBin: "/opt/tariboy-shim",
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			ctx := context.WithValue(context.Background(), activatedImageContextKey{}, activatedImage{Skills: bridge.Launch})
			prepared, err := r.prepare(ctx, otel.Tracer("test"), ag, l, iterID, "")
			if err != nil {
				t.Fatal(err)
			}
			separator := slices.Index(prepared.shimArgv, "--")
			if separator < 0 {
				t.Fatalf("missing harness separator: %v", prepared.shimArgv)
			}
			harnessArgv := prepared.shimArgv[separator+1:]
			if len(bridge.Launch.Args) > 0 && !slices.Equal(harnessArgv[len(harnessArgv)-len(bridge.Launch.Args):], bridge.Launch.Args) {
				t.Fatalf("harness args %v do not end in bridge args %v", harnessArgv, bridge.Launch.Args)
			}
			if !slices.Contains(harnessArgv, l.PromptPath(iterID)) {
				t.Fatalf("harness args do not name shared prompt path: %v", harnessArgv)
			}
			promptBody, err := os.ReadFile(l.PromptPath(iterID))
			if err != nil {
				t.Fatal(err)
			}
			if tc.harnessType == "codex" {
				if bridge.Launch.PromptPrefix == "" || !strings.HasPrefix(string(promptBody), bridge.Launch.PromptPrefix) {
					t.Fatalf("%s prompt did not start with image skill catalog:\n%s", tc.name, promptBody)
				}
				if !strings.Contains(string(promptBody)[len(bridge.Launch.PromptPrefix):], "# You are agent worker") {
					t.Fatalf("%s prompt lost rendered image template:\n%s", tc.name, promptBody)
				}
				if strings.Contains(strings.Join(harnessArgv, "\n"), "marketplaces.") {
					t.Fatalf("%s retained marketplace overrides: %v", tc.name, harnessArgv)
				}
			} else if bridge.Launch.PromptPrefix != "" || strings.Contains(string(promptBody), "## Image skills") {
				t.Fatalf("%s received a Codex prompt catalog: launch=%#v prompt=%q", tc.name, bridge.Launch, promptBody)
			}
			if got := environmentValue(prepared.env, "HOME"); got != home {
				t.Fatalf("HOME = %q, want %q", got, home)
			}
			for _, entry := range bridge.Launch.Env {
				key, value, _ := strings.Cut(entry, "=")
				if got := environmentValue(prepared.env, key); got != value {
					t.Fatalf("%s = %q, want %q", key, got, value)
				}
			}
			for _, root := range []string{cwd, home} {
				for _, name := range []string{".claude", ".agents", ".codex", ".opencode"} {
					if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
						t.Fatalf("schema-v2 launch created %s below %s: %v", name, root, err)
					}
				}
			}
			if prepared.cwd != cwd {
				t.Fatalf("launch cwd = %q, want %q", prepared.cwd, cwd)
			}
		})
	}
}

func TestRunnerSchemaV2AppendsGoalGuidanceWithoutGoalTemplateEntry(t *testing.T) {
	base := t.TempDir()
	db, err := store.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	as := agent.NewStore(db)
	externalCwd := t.TempDir()
	ag := agent.Agent{
		Name: "alice", ImageRef: "img:latest", ImageDigest: "digest",
		HarnessType: "stub", Cwd: externalCwd, Plugins: []string{"goal"},
	}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	entries := []image.TemplateEntry{
		{Kind: "runtime", Runtime: "identity"},
		{Kind: "runtime", Runtime: "workdir"},
	}
	templateSHA, err := image.PromptTemplateHash(entries)
	if err != nil {
		t.Fatal(err)
	}
	iterID := "alice-1"
	if err := as.CreateIteration(agent.Iteration{
		ID: iterID, Agent: ag.Name, Trigger: "manual", Status: "running",
		ImageRef: ag.ImageRef, ImageDigest: ag.ImageDigest, PromptTemplateSHA256: templateSHA,
	}); err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(base, "agents")
	l := agentdir.New(agentsDir, ag.Name)
	if err := l.EnsureIteration(iterID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.Workdir(), 0o700); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(l.ImageDir(), "prompt")
	if err := os.MkdirAll(promptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	templateBody, err := json.Marshal(image.PromptTemplate{SchemaVersion: 2, Entries: entries, SHA256: templateSHA})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "template.json"), templateBody, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TARIBOY_STUB_HARNESS", "/usr/bin/true")
	r := NewShimRunner(RunnerConfig{
		AgentsDir: agentsDir, Store: as, ShimBin: "/opt/tariboy-shim",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		CurrentGoal: func(string, time.Time) (tasks.Task, bool, error) {
			return tasks.Task{}, false, nil
		},
	})
	if _, err := r.prepare(context.Background(), otel.Tracer("test"), ag, l, iterID, ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(l.PromptPath(iterID))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(body)
	for _, want := range []string{"cwd: " + externalCwd, "workdir: " + l.Workdir(), "## Goal", "No goal selected for this iteration.", "wait for the customer answer"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "workdir: "+externalCwd) {
		t.Fatalf("prompt used effective CWD as managed workdir:\n%s", prompt)
	}
}

func TestRunnerSchemaV2RendersAuthoritativeGoal(t *testing.T) {
	base := t.TempDir()
	db, err := store.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	as := agent.NewStore(db)
	ag := agent.Agent{Name: "alice", ImageRef: "img:latest", ImageDigest: "digest", HarnessType: "stub"}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	entries := []image.TemplateEntry{}
	templateSHA, err := image.PromptTemplateHash(entries)
	if err != nil {
		t.Fatal(err)
	}
	iterID := "alice-1"
	if err := as.CreateIteration(agent.Iteration{
		ID: iterID, Agent: ag.Name, Trigger: "manual", Status: "running",
		ImageRef: ag.ImageRef, ImageDigest: ag.ImageDigest, PromptTemplateSHA256: templateSHA,
	}); err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(base, "agents")
	l := agentdir.New(agentsDir, ag.Name)
	if err := l.EnsureIteration(iterID); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(l.ImageDir(), "prompt")
	if err := os.MkdirAll(promptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	templateBody, err := json.Marshal(image.PromptTemplate{SchemaVersion: 2, Entries: entries, SHA256: templateSHA})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "template.json"), templateBody, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TARIBOY_STUB_HARNESS", "/usr/bin/true")
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	r := NewShimRunner(RunnerConfig{
		AgentsDir: agentsDir, Store: as, ShimBin: "/opt/tariboy-shim", Clock: func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		CurrentGoal: func(agentName string, at time.Time) (tasks.Task, bool, error) {
			if agentName != "alice" || !at.Equal(now) {
				t.Fatalf("CurrentGoal(%q, %s)", agentName, at)
			}
			return tasks.Task{
				Key: "TARI-43", Title: "Render goal", Priority: tasks.PriorityP1,
				Status: tasks.StatusInProgress, Description: "line one\nline two",
			}, true, nil
		},
	})
	if _, err := r.prepare(context.Background(), otel.Tracer("test"), ag, l, iterID, ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(l.PromptPath(iterID))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Task Processing Order", "## Goal", "key: TARI-43\ntitle: Render goal\npriority: P1\nstatus: in_progress\ndescription: line one\nline two", "do not merge it yourself"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
}

func TestMergeSkillLaunchEnvRejectsHomeOverrides(t *testing.T) {
	for _, key := range []string{"HOME", "CODEX_HOME", "XDG_CONFIG_HOME"} {
		if _, err := mergeSkillLaunchEnv([]string{"HOME=/safe"}, []string{key + "=/bridge"}); err == nil {
			t.Fatalf("accepted %s override", key)
		}
	}
}

func TestRunGuardManualFailsWhenSessionAlive(t *testing.T) {
	sp := &countingSpawner{}
	r := NewShimRunner(RunnerConfig{
		AgentsDir:      t.TempDir(),
		Spawner:        sp,
		HasTmuxSession: func(string) bool { return true },
	})
	ag := agent.Agent{Name: "manager", Interactive: true}
	_, err := r.Run(context.Background(), ag, "manual", "manager-x-1", "")
	if !errors.Is(err, ErrSessionAlive) {
		t.Fatalf("err = %v, want ErrSessionAlive", err)
	}
	if sp.starts != 0 {
		t.Fatalf("spawned %d shims, want 0", sp.starts)
	}
}

func TestReapOrphanBlock(t *testing.T) {
	newRunner := func(t *testing.T, running bool, killed *int) (*ShimRunner, agent.Agent) {
		t.Helper()
		s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		as := agent.NewStore(s)
		ag := agent.Agent{Name: "manager", ImageRef: "i:latest", Interactive: true}
		if err := as.Create(ag); err != nil {
			t.Fatal(err)
		}
		if running {
			if err := as.CreateIteration(agent.Iteration{ID: "manager-x-1", Agent: "manager", Trigger: "manual", Status: "running"}); err != nil {
				t.Fatal(err)
			}
		}
		r := NewShimRunner(RunnerConfig{
			AgentsDir: t.TempDir(), Store: as,
			HasTmuxSession:  func(string) bool { return true },
			KillTmuxSession: func(string) error { *killed++; return nil },
		})
		return r, ag
	}

	t.Run("orphan session (no running iteration) is reaped", func(t *testing.T) {
		killed := 0
		r, ag := newRunner(t, false, &killed)
		if !r.ReapOrphanBlock(ag) {
			t.Fatal("want reaped=true for an orphan session")
		}
		if killed != 1 {
			t.Fatalf("kills = %d, want 1", killed)
		}
	})

	t.Run("session owned by a running iteration is never reaped", func(t *testing.T) {
		killed := 0
		r, ag := newRunner(t, true, &killed)
		if r.ReapOrphanBlock(ag) {
			t.Fatal("want reaped=false while a running iteration owns the session")
		}
		if killed != 0 {
			t.Fatalf("kills = %d, want 0 (never kill a live session)", killed)
		}
	})

	t.Run("non-interactive agent is never reaped", func(t *testing.T) {
		killed := 0
		r, ag := newRunner(t, false, &killed)
		ag.Interactive = false
		if r.ReapOrphanBlock(ag) {
			t.Fatal("non-interactive agent must not be reaped")
		}
		if killed != 0 {
			t.Fatalf("kills = %d, want 0", killed)
		}
	})
}

func TestSessionBlockedOnlyInteractive(t *testing.T) {
	r := NewShimRunner(RunnerConfig{HasTmuxSession: func(string) bool { return true }})
	if r.SessionBlocked(agent.Agent{Name: "a", Interactive: false}) {
		t.Fatal("non-interactive agent must never be session-blocked")
	}
	if !r.SessionBlocked(agent.Agent{Name: "a", Interactive: true}) {
		t.Fatal("interactive agent with live session must be blocked")
	}
}

// newPromptProbe builds a runner wired to a real Bus over an isolated store,
// with the image PROMPT.md and an iteration row in place, so tests can call
// prepare()/Run() and inspect the assembled prompt file for the message-batch
// behaviour of dev-t-25j.3.
func newPromptProbe(t *testing.T) (*ShimRunner, agent.Agent, agentdir.Layout, *bus.Bus, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("TARIBOY_STUB_HARNESS", "/usr/bin/true")
	s, err := store.Open(filepath.Join(base, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	as := agent.NewStore(s)
	bs := bus.New(s, time.Now)

	ag := agent.Agent{Name: "alice", ImageRef: "img:latest", HarnessType: "stub", Plugins: []string{"context"}}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	iterID := "alice-1"
	if err := as.CreateIteration(agent.Iteration{ID: iterID, Agent: ag.Name, Trigger: "manual", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	agentsDir := filepath.Join(base, "agents")
	l := agentdir.New(agentsDir, ag.Name)
	if err := l.EnsureIteration(iterID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.ImageDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.ImageDir(), "PROMPT.md"), []byte("BODY"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewShimRunner(RunnerConfig{
		AgentsDir: agentsDir, Store: as, ShimBin: "/opt/tariboy-shim",
		Bus: bs, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return r, ag, l, bs, iterID
}

// prepAndReadPrompt runs prepare and returns the assembled prompt written to disk.
func prepAndReadPrompt(t *testing.T, r *ShimRunner, ag agent.Agent, l agentdir.Layout, iterID string) string {
	t.Helper()
	if _, err := r.prepare(context.Background(), otel.Tracer("test"), ag, l, iterID, ""); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	data, err := os.ReadFile(l.PromptPath(iterID))
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	return string(data)
}

// TestRunDoesNotAckBatch is the core regression for dev-t-25j.3: a successful
// iteration must NOT ack the drained message batch. The delivery stays pending so
// it re-renders next iteration until the agent processes it explicitly.
func TestRunDoesNotAckBatch(t *testing.T) {
	r, ag, l, bs, iterID := newPromptProbe(t)
	if _, err := bs.Subscribe(ag.Name, bus.InboxChannel(ag.Name), bus.Matcher{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := bs.Publish(bus.Message{Channel: bus.InboxChannel(ag.Name), Type: "note", Text: "hi"}); err != nil {
		t.Fatal(err)
	}

	r.cfg.Spawner = &fakeProxySpawner{resultPath: l.ResultPath(iterID)}
	out, err := r.Run(context.Background(), ag, "manual", iterID, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no_i_am_done" {
		t.Fatalf("status = %q, want no_i_am_done", out.Status)
	}

	pending, err := bs.HasPending(ag.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("message was acked on iteration success; it must stay pending until explicitly processed")
	}
}

// TestPrepareReRendersUntilProcessed asserts a message keeps rendering into the
// prompt across iterations until MarkProcessed drains it.
func TestPrepareReRendersUntilProcessed(t *testing.T) {
	r, ag, l, bs, iterID := newPromptProbe(t)
	if _, err := bs.Subscribe(ag.Name, bus.InboxChannel(ag.Name), bus.Matcher{}, nil); err != nil {
		t.Fatal(err)
	}
	m, err := bs.Publish(bus.Message{Channel: bus.InboxChannel(ag.Name), Type: "note", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}

	// Two successive prepares both render the message id — it is not auto-drained.
	for i := 0; i < 2; i++ {
		if got := prepAndReadPrompt(t, r, ag, l, iterID); !strings.Contains(got, m.ID) {
			t.Fatalf("iteration %d: message id %q not re-rendered:\n%s", i, m.ID, got)
		}
	}

	// Once processed, it drops out of the batch and leaves the explicit empty state.
	if _, err := bs.MarkProcessed(ag.Name, m.ID, "handled"); err != nil {
		t.Fatal(err)
	}
	got := prepAndReadPrompt(t, r, ag, l, iterID)
	if strings.Contains(got, m.ID) {
		t.Fatalf("processed message still rendered:\n%s", got)
	}
	if !strings.Contains(got, "## Messages\n\nNo incoming messages for this iteration.") {
		t.Fatalf("Messages section should report the empty batch:\n%s", got)
	}
}

// TestPrepareDropsMessageAfterDLQ verifies the existing attempts++/DLQ-after-5
// path still fires through the runner: an unprocessed message re-renders until it
// is dead-lettered, after which it no longer appears in the prompt.
func TestPrepareDropsMessageAfterDLQ(t *testing.T) {
	r, ag, l, bs, iterID := newPromptProbe(t)
	if _, err := bs.Subscribe(ag.Name, bus.InboxChannel(ag.Name), bus.Matcher{}, nil); err != nil {
		t.Fatal(err)
	}
	m, err := bs.Publish(bus.Message{Channel: bus.InboxChannel(ag.Name), Type: "note", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}

	// maxAttempts is 5; the delivery is returned on renders 1..6 and dead-lettered
	// on the 6th. Drain enough times to trip the DLQ.
	for i := 0; i < 6; i++ {
		if got := prepAndReadPrompt(t, r, ag, l, iterID); !strings.Contains(got, m.ID) {
			t.Fatalf("render %d: message id %q missing before DLQ:\n%s", i+1, m.ID, got)
		}
	}
	// After dead-lettering, the message is gone from the pending batch.
	if got := prepAndReadPrompt(t, r, ag, l, iterID); strings.Contains(got, m.ID) {
		t.Fatalf("dead-lettered message still rendered:\n%s", got)
	}
	if pending, err := bs.HasPending(ag.Name); err != nil || pending {
		t.Fatalf("HasPending = %v (err %v), want false after DLQ", pending, err)
	}
}

// TestPrepareRendersAwaitingReplies asserts an outstanding request produces the
// derived "# Awaiting replies" prompt section.
func TestPrepareRendersAwaitingReplies(t *testing.T) {
	r, ag, l, bs, iterID := newPromptProbe(t)
	req, err := bs.Request(ag.Name, "ops:deploys", "status?", "")
	if err != nil {
		t.Fatal(err)
	}
	got := prepAndReadPrompt(t, r, ag, l, iterID)
	if !strings.Contains(got, "# Awaiting replies") {
		t.Fatalf("awaiting-replies section missing:\n%s", got)
	}
	if !strings.Contains(got, req.ID) {
		t.Fatalf("outstanding request id %q missing:\n%s", req.ID, got)
	}
}

func TestBuildEnvEmptyAgentBinSkipsPathPrepend(t *testing.T) {
	env := BuildEnv([]string{"PATH=/usr/bin:/bin"}, "", "a", "i1", "/tmp/s.sock", false, "", "", nil, nil)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			if kv != "PATH=/usr/bin:/bin" {
				t.Fatalf("PATH = %q, want untouched /usr/bin:/bin", kv)
			}
			return
		}
	}
	t.Fatal("PATH missing from env")
}

type harnessPreflightSpawner struct {
	resultPath string
	starts     int
	env        []string
}

func (s *harnessPreflightSpawner) Start(_ []string, env []string, _ string) error {
	s.starts++
	s.env = append([]string(nil), env...)
	return os.WriteFile(s.resultPath, []byte(`{"exit_code":0}`), 0o600)
}

func newHarnessPreflightRunner(t *testing.T, bare bool, agentEnv map[string]string) (*ShimRunner, agent.Agent, agentdir.Layout, *harnessPreflightSpawner) {
	t.Helper()
	base := t.TempDir()
	s, err := store.Open(filepath.Join(base, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	as := agent.NewStore(s)

	ag := agent.Agent{
		Name: "path-agent", ImageRef: "image:latest", HarnessType: "claude",
		Env: agentEnv,
	}
	if bare {
		ag.ImageRef = image.BareRef.String()
	}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	iterID := "path-agent-1"
	if err := as.CreateIteration(agent.Iteration{
		ID: iterID, Agent: ag.Name, Trigger: "manual", Status: "running",
	}); err != nil {
		t.Fatal(err)
	}

	agentsDir := filepath.Join(base, "agents")
	l := agentdir.New(agentsDir, ag.Name)
	if err := l.EnsureIteration(iterID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.ImageDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if !bare {
		if err := os.WriteFile(filepath.Join(l.ImageDir(), "PROMPT.md"), []byte("BODY"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(filepath.Join(l.ImageDir(), "manifest.json"), []byte(`{"schema_version":1,"name":"bare","tag":"latest","bare":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	spawner := &harnessPreflightSpawner{resultPath: l.ResultPath(iterID)}
	r := NewShimRunner(RunnerConfig{
		AgentsDir: agentsDir, Store: as, ShimBin: "/opt/tariboy-shim",
		Spawner: spawner, PollInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return r, ag, l, spawner
}

func TestHarnessPreflightUsesAgentEffectivePath(t *testing.T) {
	baseline := t.TempDir()
	t.Setenv("PATH", baseline)
	agentBin := t.TempDir()
	writeHarnessExecutable(t, filepath.Join(agentBin, "claude"))
	writeHarnessExecutable(t, filepath.Join(agentBin, "python3"))
	writeHarnessExecutable(t, filepath.Join(agentBin, "bash"))
	r, ag, l, spawner := newHarnessPreflightRunner(t, false, map[string]string{"PATH": agentBin})

	if _, err := r.Run(context.Background(), ag, "manual", "path-agent-1", ""); err != nil {
		t.Fatal(err)
	}
	if spawner.starts != 1 {
		t.Fatalf("shim starts = %d, want 1", spawner.starts)
	}
	if got := environmentValue(spawner.env, "PATH"); got != l.BinDir()+":"+agentBin {
		t.Fatalf("effective PATH = %q, want %q", got, l.BinDir()+":"+agentBin)
	}
	if got := environmentValue(spawner.env, "TARIBOY_PYTHON3"); got != filepath.Join(agentBin, "python3") {
		t.Fatalf("TARIBOY_PYTHON3 = %q, want resolved absolute interpreter", got)
	}
}

func TestPythonPreflightRejectsAgentPathWithHarnessButNoPython(t *testing.T) {
	daemonPath := t.TempDir()
	writeHarnessExecutable(t, filepath.Join(daemonPath, "python3"))
	t.Setenv("PATH", daemonPath)
	agentPath := t.TempDir()
	writeHarnessExecutable(t, filepath.Join(agentPath, "claude"))
	r, ag, _, spawner := newHarnessPreflightRunner(t, false, map[string]string{"PATH": agentPath})

	_, err := r.Run(context.Background(), ag, "manual", "path-agent-1", "")
	if err == nil || !strings.Contains(err.Error(), "python3") {
		t.Fatalf("iteration accepted an effective PATH without Python: %v", err)
	}
	if spawner.starts != 0 {
		t.Fatalf("shim starts = %d, want 0", spawner.starts)
	}
}

func TestHarnessPreflightResolvesRelativePathFromIterationCwd(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cwd := t.TempDir()
	relativeBin := filepath.Join(cwd, "relative-bin")
	if err := os.Mkdir(relativeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeHarnessExecutable(t, filepath.Join(relativeBin, "claude"))
	writeHarnessExecutable(t, filepath.Join(relativeBin, "python3"))
	writeHarnessExecutable(t, filepath.Join(relativeBin, "bash"))
	r, ag, _, spawner := newHarnessPreflightRunner(t, false, map[string]string{"PATH": "relative-bin"})
	ag.Cwd = cwd

	if _, err := r.Run(context.Background(), ag, "manual", "path-agent-1", ""); err != nil {
		t.Fatal(err)
	}
	if spawner.starts != 1 {
		t.Fatalf("shim starts = %d, want 1", spawner.starts)
	}
}

func TestHarnessPreflightRejectsMissingExecutableBeforeSpawn(t *testing.T) {
	missingPath := t.TempDir()
	t.Setenv("PATH", missingPath)
	r, ag, _, spawner := newHarnessPreflightRunner(t, false, nil)

	_, err := r.Run(context.Background(), ag, "manual", "path-agent-1", "")
	if err == nil {
		t.Fatal("missing harness executable accepted")
	}
	if got, want := err.Error(), `harness executable "claude" not found`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if spawner.starts != 0 {
		t.Fatalf("shim starts = %d, want 0", spawner.starts)
	}
}

func TestHarnessPreflightStubErrorDoesNotExposeConfiguredPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	secretPath := filepath.Join(t.TempDir(), "secret-token-value", "stub-harness")
	t.Setenv("TARIBOY_STUB_HARNESS", secretPath)
	r, ag, _, spawner := newHarnessPreflightRunner(t, false, nil)
	ag.HarnessType = "stub"

	_, err := r.Run(context.Background(), ag, "manual", "path-agent-1", "")
	if err == nil {
		t.Fatal("missing stub harness accepted")
	}
	if got, want := err.Error(), `harness executable "stub" not found`; got != want {
		t.Fatalf("error = %q, want sanitized %q", got, want)
	}
	if strings.Contains(err.Error(), secretPath) || strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("error exposes configured stub path: %q", err)
	}
	if spawner.starts != 0 {
		t.Fatalf("shim starts = %d, want 0", spawner.starts)
	}
}

func TestHarnessPreflightFailureRevokesProxyTokenBeforeSpawn(t *testing.T) {
	binder := &fakeBinder{base: "http://127.0.0.1:5555"}
	r, ag, _, _ := newRunnerForProxyTest(t, binder)
	t.Setenv("TARIBOY_STUB_HARNESS", filepath.Join(t.TempDir(), "missing-stub"))
	spawner := &countingSpawner{}
	r.cfg.Spawner = spawner

	_, err := r.Run(context.Background(), ag, "manual", "alice-1", "")
	if err == nil {
		t.Fatal("missing harness executable accepted")
	}
	if len(binder.minted) != 1 {
		t.Fatalf("minted tokens = %v, want exactly one", binder.minted)
	}
	if len(binder.revoked) != 1 || binder.revoked[0] != binder.minted[0] {
		t.Fatalf("revoked tokens = %v, want exactly minted token %q", binder.revoked, binder.minted[0])
	}
	if spawner.starts != 0 {
		t.Fatalf("shim starts = %d, want 0", spawner.starts)
	}
}

func TestHarnessPreflightSearchesAgentBin(t *testing.T) {
	baseline := t.TempDir()
	writeHarnessExecutable(t, filepath.Join(baseline, "python3"))
	writeHarnessExecutable(t, filepath.Join(baseline, "bash"))
	t.Setenv("PATH", baseline)
	r, ag, l, spawner := newHarnessPreflightRunner(t, false, nil)
	if err := os.MkdirAll(l.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	writeHarnessExecutable(t, filepath.Join(l.BinDir(), "claude"))

	if _, err := r.Run(context.Background(), ag, "manual", "path-agent-1", ""); err != nil {
		t.Fatal(err)
	}
	if got := environmentValue(spawner.env, "PATH"); got != l.BinDir()+":"+baseline {
		t.Fatalf("effective PATH = %q, want %q", got, l.BinDir()+":"+baseline)
	}
}

func TestHarnessPreflightBareUsesDaemonEffectivePath(t *testing.T) {
	baseline := t.TempDir()
	writeHarnessExecutable(t, filepath.Join(baseline, "claude"))
	writeHarnessExecutable(t, filepath.Join(baseline, "bash"))
	t.Setenv("PATH", baseline)
	r, ag, l, spawner := newHarnessPreflightRunner(t, true, nil)

	if _, err := r.Run(context.Background(), ag, "manual", "path-agent-1", ""); err != nil {
		t.Fatal(err)
	}
	if got := environmentValue(spawner.env, "PATH"); got != baseline {
		t.Fatalf("effective PATH = %q, want daemon baseline %q", got, baseline)
	}
	if strings.Contains(environmentValue(spawner.env, "PATH"), l.BinDir()) {
		t.Fatalf("bare effective PATH contains agent bin %q", l.BinDir())
	}
}

func writeHarnessExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func environmentValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func TestPrepareBareImage(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TARIBOY_STUB_HARNESS", "/usr/bin/true")
	s, err := store.Open(filepath.Join(base, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	as := agent.NewStore(s)

	ag := agent.Agent{Name: "bob", ImageRef: "bare:latest", HarnessType: "stub", Interactive: true}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	iterID := "bob-20260722120000-1"
	if err := as.CreateIteration(agent.Iteration{ID: iterID, Agent: ag.Name, Trigger: "manual", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(base, "agents")
	l := agentdir.New(agentsDir, ag.Name)
	if err := l.EnsureIteration(iterID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.ImageDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// Bare manifest; deliberately NO PROMPT.md — bare prepare must not need it.
	if err := os.WriteFile(filepath.Join(l.ImageDir(), "manifest.json"), []byte(`{"schema_version":1,"name":"bare","tag":"latest","bare":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewShimRunner(RunnerConfig{
		AgentsDir: agentsDir, Store: as, ShimBin: "/opt/tariboy-shim",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	prep, err := r.prepare(context.Background(), otel.Tracer("test"), ag, l, iterID, "")
	if err != nil {
		t.Fatal(err)
	}
	// Prompt file exists and is empty.
	b, err := os.ReadFile(l.PromptPath(iterID))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Fatalf("bare prompt file not empty: %q", b)
	}
	// PATH must not contain the agent bin dir.
	for _, kv := range prep.env {
		if strings.HasPrefix(kv, "PATH=") && strings.Contains(kv, l.BinDir()) {
			t.Fatalf("bare PATH contains agent bin: %s", kv)
		}
	}
	// Tariboy env attribution is still present.
	found := false
	for _, kv := range prep.env {
		if kv == "TARIBOY_AGENT=bob" {
			found = true
		}
	}
	if !found {
		t.Fatalf("TARIBOY_AGENT missing from env: %v", prep.env)
	}
}

func TestEnforceSoftTimeoutDoesNotKillAfterConcurrentExtension(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	as := agent.NewStore(db)
	if err := as.Create(agent.Agent{Name: "smoke", ImageRef: "basic:latest"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	if err := as.CreateIteration(agent.Iteration{ID: "live", Agent: "smoke", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := as.InitializeIterationTimeout("live", 30, 90, now); err != nil {
		t.Fatal(err)
	}
	stale, err := as.GetIteration("smoke", "live")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := as.ExtendIterationTimeout("smoke", "live", now.Add(29*time.Second)); err != nil {
		t.Fatal(err)
	}
	var kills atomic.Int32
	triggered, err := enforceSoftTimeout(as, stale, now.Add(31*time.Second), func() error {
		kills.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if triggered || kills.Load() != 0 {
		t.Fatalf("stale deadline triggered=%v kills=%d after committed extension", triggered, kills.Load())
	}
	current, err := as.GetIteration("smoke", "live")
	if err != nil {
		t.Fatal(err)
	}
	if current.TimeoutTriggeredAt != nil {
		t.Fatalf("stale deadline persisted marker after extension: %+v", current)
	}
}

type failingHarnessSpawner struct{ l agentdir.Layout }

func (s failingHarnessSpawner) Start(_, _ []string, _ string) error {
	if err := os.MkdirAll(s.l.LogsDir("alice-1"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.l.HarnessStderr("alice-1"), []byte("booting\nfatal: model not found\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(s.l.ResultPath("alice-1"), []byte(`{"exit_code":3}`), 0o600)
}

func TestRunReportsHarnessExitReason(t *testing.T) {
	r, ag, l, _ := newRunnerForProxyTest(t, &fakeBinder{base: "http://127.0.0.1:5555"})
	r.cfg.Spawner = failingHarnessSpawner{l: l}
	out, err := r.Run(context.Background(), ag, "manual", "alice-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "harness_error" || !strings.Contains(out.Error, "harness exited with code 3") ||
		!strings.Contains(out.Error, "fatal: model not found") {
		t.Fatalf("outcome = %+v, want exit code and stderr tail in Error", out)
	}
}

func TestRunRejectsMissingAgentCwd(t *testing.T) {
	r, ag, _, _ := newRunnerForProxyTest(t, &fakeBinder{base: "http://127.0.0.1:5555"})
	cs := &countingSpawner{}
	r.cfg.Spawner = cs
	ag.Cwd = filepath.Join(t.TempDir(), "missing")
	_, err := r.Run(context.Background(), ag, "manual", "alice-1", "")
	if err == nil || !strings.Contains(err.Error(), "agent cwd "+ag.Cwd+" does not exist") || cs.starts != 0 {
		t.Fatalf("err = %v starts = %d, want missing cwd rejected before spawn", err, cs.starts)
	}
}
