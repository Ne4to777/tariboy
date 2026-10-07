package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func writePrompt(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "PROMPT.md")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeAdapter(t *testing.T) {
	a, err := Get("claude")
	if err != nil {
		t.Fatal(err)
	}
	if a.Type() != "claude" {
		t.Fatalf("type = %q", a.Type())
	}
	prompt := writePrompt(t, "DO THE WORK")
	// A non-UUID session id (like an iteration id) must be mapped to a valid
	// UUID: claude rejects --session-id values that are not UUIDs.
	wantSID := ensureUUID("sess-1")
	if _, err := uuid.Parse(wantSID); err != nil {
		t.Fatalf("ensureUUID must yield a valid UUID, got %q", wantSID)
	}
	argv, _, err := a.Command("/w", prompt, Config{SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"/bin/sh", "tariboy-harness", "claude", prompt, "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--allowedTools", "Bash(i-am-done)", "--session-id", wantSID} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %q: %v", want, argv)
		}
	}
	if strings.Contains(joined, "DO THE WORK") {
		t.Fatalf("prompt body must be passed via file, not argv: %v", argv)
	}
	// no model/effort configured: no --model or --effort flags at all
	wantNoModelEffort := []string{
		"/bin/sh", "-c", promptOnStdinCommand,
		"tariboy-harness", "claude", prompt, "-p", "--output-format", "stream-json", "--verbose",
		"--dangerously-skip-permissions", "--allowedTools", "Bash(i-am-done)", "--session-id", wantSID,
	}
	if len(argv) != len(wantNoModelEffort) {
		t.Fatalf("argv (no model/effort) = %v, want %v", argv, wantNoModelEffort)
	}
	for i, want := range wantNoModelEffort {
		if argv[i] != want {
			t.Fatalf("argv[%d] = %q, want %q (full argv %v)", i, argv[i], want, argv)
		}
	}

	// An interactive agent runs in a tmux TUI driven by the operator, so it must
	// launch WITHOUT print mode: no -p / --output-format stream-json / --verbose.
	// Everything else (skip-permissions, allowedTools, session-id) is unchanged.
	iargv, _, err := a.Command("/w", prompt, Config{Interactive: true, SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	wantInteractive := []string{
		"/bin/sh", "-c", promptOnStdinCommand,
		"tariboy-harness", "claude", prompt,
		"--dangerously-skip-permissions", "--allowedTools", "Bash(i-am-done)", "--session-id", wantSID,
	}
	if len(iargv) != len(wantInteractive) {
		t.Fatalf("interactive argv = %v, want %v", iargv, wantInteractive)
	}
	for i, want := range wantInteractive {
		if iargv[i] != want {
			t.Fatalf("interactive argv[%d] = %q, want %q (full argv %v)", i, iargv[i], want, iargv)
		}
	}
}

func TestClaudeAdapterModelEffort(t *testing.T) {
	a, err := Get("claude")
	if err != nil {
		t.Fatal(err)
	}
	prompt := writePrompt(t, "DO THE WORK")
	argv, _, err := a.Command("/w", prompt, Config{Model: "sonnet", Effort: "high", SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/bin/sh", "-c", promptOnStdinCommand,
		"tariboy-harness", "claude", prompt, "-p", "--output-format", "stream-json", "--verbose",
		"--dangerously-skip-permissions", "--allowedTools", "Bash(i-am-done)", "--session-id", ensureUUID("sess-1"),
		"--model", "sonnet", "--effort", "high",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i, w := range want {
		if argv[i] != w {
			t.Fatalf("argv[%d] = %q, want %q (full argv %v)", i, argv[i], w, argv)
		}
	}
	if strings.Contains(strings.Join(argv, " "), "DO THE WORK") {
		t.Fatalf("prompt body must be passed via file, not argv: %v", argv)
	}
}

func TestCodexAdapter(t *testing.T) {
	a, err := Get("codex")
	if err != nil {
		t.Fatal(err)
	}
	if a.Type() != "codex" {
		t.Fatalf("type = %q", a.Type())
	}
	prompt := writePrompt(t, "DO THE WORK")
	argv, _, err := a.Command("/w", prompt, Config{Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/bin/sh", "-c", promptOnStdinCommand,
		"tariboy-harness", "codex", prompt,
		"exec", "-c", "allow_login_shell=false", "--json", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check", "--cd", "/w",
		"--model", "gpt-5",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i, w := range want {
		if argv[i] != w {
			t.Fatalf("argv[%d] = %q, want %q (full argv %v)", i, argv[i], w, argv)
		}
	}
	if strings.Contains(strings.Join(argv, " "), "DO THE WORK") {
		t.Fatalf("prompt body must be passed via file, not argv: %v", argv)
	}
}

func TestCodexAdapterInteractiveUsesTUI(t *testing.T) {
	a, err := Get("codex")
	if err != nil {
		t.Fatal(err)
	}
	prompt := writePrompt(t, "DO THE WORK")
	argv, _, err := a.Command("/w", prompt, Config{
		Interactive: true,
		Model:       "gpt-5",
		ProxyURL:    "http://127.0.0.1:5555/_tariboy/sk-tariboy-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT promptOnStdinCommand: the codex TUI rejects a redirected
	// stdin, so this is the one path that still passes the prompt in argv.
	want := []string{
		"/bin/sh", "-c", `prompt=$(cat "$2") || exit; cmd=$1; shift 2; exec "$cmd" "$@" "$prompt"`,
		"tariboy-harness", "codex", prompt,
		"-c", "allow_login_shell=false",
		"--dangerously-bypass-approvals-and-sandbox", "--cd", "/w",
		"-c", `model_provider="tariboy"`,
		"-c", `model_providers.tariboy.name="Tariboy"`,
		"-c", `model_providers.tariboy.base_url="http://127.0.0.1:5555/_tariboy/sk-tariboy-token"`,
		"-c", `model_providers.tariboy.requires_openai_auth=true`,
		"-c", `model_providers.tariboy.wire_api="responses"`,
		"--model", "gpt-5",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("interactive argv = %v, want %v", argv, want)
	}
	if strings.Contains(strings.Join(argv, " "), "DO THE WORK") {
		t.Fatalf("prompt body must be passed via file, not argv: %v", argv)
	}
}

func TestCodexAdapterProxyProviderUsesOpenAIAuth(t *testing.T) {
	a, err := Get("codex")
	if err != nil {
		t.Fatal(err)
	}
	argv, _, err := a.Command("/w", writePrompt(t, "DO THE WORK"), Config{
		ProxyURL: "http://127.0.0.1:5555/_tariboy/sk-tariboy-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\n")
	for _, want := range []string{
		`model_provider="tariboy"`,
		`model_providers.tariboy.name="Tariboy"`,
		`model_providers.tariboy.base_url="http://127.0.0.1:5555/_tariboy/sk-tariboy-token"`,
		`model_providers.tariboy.requires_openai_auth=true`,
		`model_providers.tariboy.wire_api="responses"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("proxy argv missing %q: %v", want, argv)
		}
	}
	for _, forbidden := range []string{"env_key", "OPENAI_API_KEY"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("proxy argv contains forbidden %q: %v", forbidden, argv)
		}
	}
}

func TestEnsureUUID(t *testing.T) {
	// A real iteration id is not a UUID and must be mapped to one,
	// deterministically (same input -> same output).
	iter := "daring-sparrow-20260705161437-1"
	got := ensureUUID(iter)
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("ensureUUID(%q) = %q, not a valid UUID: %v", iter, got, err)
	}
	if got == iter {
		t.Fatalf("ensureUUID must convert non-UUID input, got %q", got)
	}
	if again := ensureUUID(iter); again != got {
		t.Fatalf("ensureUUID not deterministic: %q != %q", got, again)
	}

	// Distinct iterations map to distinct session ids.
	if ensureUUID("daring-sparrow-20260705161437-2") == got {
		t.Fatal("different iteration ids must map to different UUIDs")
	}

	// A value that is already a UUID passes through unchanged.
	valid := "123e4567-e89b-12d3-a456-426614174000"
	if ensureUUID(valid) != valid {
		t.Fatalf("valid UUID must pass through unchanged, got %q", ensureUUID(valid))
	}

	// Empty falls back to the nil UUID.
	if ensureUUID("") != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("empty session id = %q", ensureUUID(""))
	}
}

func TestStubAdapter(t *testing.T) {
	t.Setenv("TARIBOY_STUB_HARNESS", "/tmp/stub-harness.sh")
	a, err := Get("stub")
	if err != nil {
		t.Fatal(err)
	}
	prompt := writePrompt(t, "x")
	argv, _, err := a.Command("/w", prompt, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "/tmp/stub-harness.sh" || argv[1] != prompt {
		t.Fatalf("stub argv = %v", argv)
	}
}

func TestAdapterExecutable(t *testing.T) {
	stubPath := filepath.Join(t.TempDir(), "stub-harness")
	t.Setenv("TARIBOY_STUB_HARNESS", stubPath)
	cases := map[string]string{
		"claude":   "claude",
		"codex":    "codex",
		"opencode": "opencode",
		"cursor":   "agent",
		"stub":     stubPath,
	}
	for typ, want := range cases {
		a, err := Get(typ)
		if err != nil {
			t.Fatalf("Get(%q): %v", typ, err)
		}
		if got := a.Executable(); got != want {
			t.Fatalf("%s executable = %q, want %q", typ, got, want)
		}
	}
}

func TestHarnessWrappersPreserveEffectivePath(t *testing.T) {
	prompt := writePrompt(t, "DO THE WORK")
	cases := []struct {
		name string
		a    Adapter
		cfg  Config
	}{
		{name: "claude", a: claude{}},
		{name: "claude bare", a: claude{}, cfg: Config{Bare: true}},
		{name: "codex", a: codex{}},
		{name: "cursor", a: cursor{}},
		{name: "cursor bare", a: cursor{}, cfg: Config{Bare: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, _, err := tc.a.Command("/w", prompt, tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(argv) < 2 || argv[0] != "/bin/sh" || argv[1] != "-c" {
				t.Fatalf("wrapper argv = %v, want /bin/sh -c", argv)
			}
		})
	}
}

func TestFindExecutableUsesEffectivePath(t *testing.T) {
	suppliedBin := t.TempDir()
	ambientBin := t.TempDir()
	writeExecutable(t, filepath.Join(suppliedBin, "only-supplied"), 0o755)
	writeExecutable(t, filepath.Join(ambientBin, "only-ambient"), 0o755)
	t.Setenv("PATH", ambientBin)

	got, err := FindExecutable("only-supplied", []string{"HOME=/isolated", "PATH=" + suppliedBin}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(suppliedBin, "only-supplied"); got != want {
		t.Fatalf("FindExecutable path = %q, want %q", got, want)
	}
	if _, err := FindExecutable("only-ambient", []string{"PATH=" + suppliedBin}, t.TempDir()); err == nil {
		t.Fatal("FindExecutable used ambient PATH instead of supplied environment")
	}
}

func TestFindExecutableRejectsInvalidCandidates(t *testing.T) {
	binDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(binDir, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "not-executable"), 0o644)

	for _, name := range []string{"directory", "not-executable"} {
		t.Run(name, func(t *testing.T) {
			if _, err := FindExecutable(name, []string{"PATH=" + binDir}, t.TempDir()); err == nil {
				t.Fatalf("FindExecutable accepted %s", name)
			}
		})
	}
}

func TestFindExecutableValidatesAbsoluteExecutable(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "stub-harness")
	writeExecutable(t, executable, 0o755)
	got, err := FindExecutable(executable, []string{"PATH=" + t.TempDir()}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != executable {
		t.Fatalf("FindExecutable path = %q, want %q", got, executable)
	}
}

func TestFindExecutableUsesIterationCwd(t *testing.T) {
	cwd := t.TempDir()
	relativeBin := filepath.Join(cwd, "relative-bin")
	if err := os.Mkdir(relativeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(relativeBin, "relative-component"), 0o755)
	writeExecutable(t, filepath.Join(cwd, "empty-component"), 0o755)
	slashExecutable := filepath.Join("tools", "slash-executable")
	if err := os.Mkdir(filepath.Join(cwd, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(cwd, slashExecutable), 0o755)

	tests := []struct {
		name       string
		executable string
		path       string
		want       string
	}{
		{
			name:       "relative PATH component",
			executable: "relative-component",
			path:       "relative-bin",
			want:       filepath.Join(cwd, "relative-bin", "relative-component"),
		},
		{
			name:       "empty PATH component",
			executable: "empty-component",
			path:       "",
			want:       filepath.Join(cwd, "empty-component"),
		},
		{
			name:       "slash-containing relative executable",
			executable: slashExecutable,
			path:       t.TempDir(),
			want:       filepath.Join(cwd, slashExecutable),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FindExecutable(tc.executable, []string{"PATH=" + tc.path}, cwd)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("FindExecutable() = %q, want %q", got, tc.want)
			}
		})
	}
}

func writeExecutable(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestCursorAdapter(t *testing.T) {
	a, err := Get("cursor")
	if err != nil {
		t.Fatal(err)
	}
	if a.Type() != "cursor" || a.Executable() != "agent" {
		t.Fatalf("cursor adapter = %s %s", a.Type(), a.Executable())
	}
	prompt := writePrompt(t, "DO THE WORK")
	argv, env, err := a.Command("/w", prompt, Config{Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	if env != nil {
		t.Fatalf("env = %v", env)
	}
	want := []string{
		"/bin/sh", "-c", `prompt=$(cat "$2") || exit; cmd=$1; shift 2; exec "$cmd" "$@" "$prompt"`,
		"tariboy-harness", "agent", prompt,
		"-p", "--output-format", "stream-json", "--force", "--trust",
		"--model", "gpt-5",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	if strings.Contains(strings.Join(argv, " "), "DO THE WORK") {
		t.Fatalf("prompt body must be passed via file, not argv: %v", argv)
	}

	iargv, _, err := a.Command("/w", prompt, Config{Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	wantInteractive := []string{
		"/bin/sh", "-c", `prompt=$(cat "$2") || exit; cmd=$1; shift 2; exec "$cmd" "$@" "$prompt"`,
		"tariboy-harness", "agent", prompt,
		"--force",
	}
	if !reflect.DeepEqual(iargv, wantInteractive) {
		t.Fatalf("interactive argv = %v, want %v", iargv, wantInteractive)
	}

	bargv, _, err := a.Command("/w", prompt, Config{Bare: true})
	if err != nil {
		t.Fatal(err)
	}
	wantBare := []string{
		"/bin/sh", "-c", `cmd=$1; shift; exec "$cmd" "$@"`,
		"tariboy-harness", "agent",
		"-p", "--output-format", "stream-json", "--force", "--trust",
	}
	if !reflect.DeepEqual(bargv, wantBare) {
		t.Fatalf("bare argv = %v, want %v", bargv, wantBare)
	}
}

func TestGetUnknown(t *testing.T) {
	if _, err := Get("bard"); err == nil {
		t.Fatal("unknown harness accepted")
	}
}

func TestStubRequiresEnv(t *testing.T) {
	t.Setenv("TARIBOY_STUB_HARNESS", "")
	if _, err := Get("stub"); err == nil {
		t.Fatal("stub without TARIBOY_STUB_HARNESS should error")
	}
}

func TestClaudeBareCommand(t *testing.T) {
	argv, _, err := claude{}.Command("/w", "/p/PROMPT.md",
		Config{Interactive: true, SessionID: "11111111-2222-3333-4444-555555555555", Bare: true, Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/bin/sh", "-c", `cmd=$1; shift; exec "$cmd" "$@"`, "tariboy-harness", "claude",
		"--dangerously-skip-permissions",
		"--session-id", "11111111-2222-3333-4444-555555555555", "--model", "opus",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	joined := strings.Join(argv, " ")
	for _, forbidden := range []string{"allowedTools", "i-am-done", "PROMPT.md", "cat \""} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("bare argv contains %q: %v", forbidden, argv)
		}
	}
	// Preserve the daemon's effective environment rather than starting another
	// login shell that may replace PATH.
	if argv[0] != "/bin/sh" || argv[1] != "-c" {
		t.Fatalf("bare argv does not preserve the effective environment: %v", argv)
	}
}
