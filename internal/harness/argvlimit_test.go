package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A prompt comfortably past MAX_ARG_STRLEN. An agent whose CONTEXT.md grows
// over time crosses the limit mid-life and then cannot start at all.
const oversizedPromptLen = 200_000

func oversizedPrompt(t *testing.T) (path, body string) {
	t.Helper()
	body = strings.Repeat("tariboy ", oversizedPromptLen/11)
	return writePrompt(t, body), body
}

// Every adapter that can take its prompt off argv must do so: execve rejects a
// single argv element of MAX_ARG_STRLEN or more with E2BIG, and /bin/sh reports
// that as a bare exit 126 with no diagnostic anywhere.
func TestPromptNeverInlinedIntoArgv(t *testing.T) {
	promptPath, _ := oversizedPrompt(t)
	cases := []struct {
		name string
		a    Adapter
		cfg  Config
	}{
		{name: "claude print", a: claude{}},
		{name: "claude interactive", a: claude{}, cfg: Config{Interactive: true}},
		{name: "codex exec", a: codex{}},
		{name: "opencode", a: opencode{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, _, err := tc.a.Command("/w", promptPath, tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			for i, arg := range argv {
				if len(arg) >= maxArgStrLen {
					t.Fatalf("argv[%d] is %d bytes, at or over the %d-byte MAX_ARG_STRLEN: execve would fail with E2BIG",
						i, len(arg), maxArgStrLen)
				}
			}
		})
	}
}

// The unit assertion above only proves the prompt is not in argv. This one
// proves the generated argv actually runs: it puts a stub named after the
// harness executable on PATH and checks the stub received the whole prompt.
func TestOversizedPromptReachesHarnessStdin(t *testing.T) {
	promptPath, body := oversizedPrompt(t)
	cases := []struct {
		name string
		a    Adapter
		cfg  Config
	}{
		{name: "claude print", a: claude{}},
		{name: "claude interactive", a: claude{}, cfg: Config{Interactive: true}},
		{name: "codex exec", a: codex{}},
		{name: "opencode", a: opencode{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			gotPath := filepath.Join(t.TempDir(), "got")
			stub := "#!/bin/sh\ncat > " + gotPath + "\n"
			if err := os.WriteFile(filepath.Join(binDir, tc.a.Executable()), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}

			argv, _, err := tc.a.Command("/w", promptPath, tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			// Prepend rather than replace: the wrapper itself needs a usable PATH.
			cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("running generated argv failed: %v (output %q)", err, out)
			}

			got, err := os.ReadFile(gotPath)
			if err != nil {
				t.Fatalf("harness received no prompt on stdin: %v", err)
			}
			if strings.TrimRight(string(got), "\n") != body {
				t.Fatalf("harness stdin = %d bytes, want the %d-byte prompt", len(got), len(body))
			}
		})
	}
}

// The codex TUI rejects a non-tty stdin outright ("Error: stdin is not a
// terminal"), so its prompt has to stay in argv and stays bound by
// MAX_ARG_STRLEN. Report that as a diagnosable error rather than letting
// execve fail with an opaque exit 126.
func TestCursorRejectsOversizedPrompt(t *testing.T) {
	promptPath, _ := oversizedPrompt(t)
	for _, cfg := range []Config{{}, {Interactive: true}} {
		_, _, err := cursor{}.Command("/w", promptPath, cfg)
		if err == nil {
			t.Fatal("oversized prompt must be rejected when it has to travel in argv")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "cursor") {
			t.Fatalf("error %q does not mention cursor", err)
		}
	}
	_, _, err := cursor{}.Command("/w", writePrompt(t, "DO THE WORK"), Config{})
	if err != nil {
		t.Fatalf("prompt within the limit must be accepted: %v", err)
	}
}

func TestCodexInteractiveRejectsOversizedPrompt(t *testing.T) {
	promptPath, _ := oversizedPrompt(t)
	_, _, err := codex{}.Command("/w", promptPath, Config{Interactive: true})
	if err == nil {
		t.Fatal("oversized prompt must be rejected for the interactive codex TUI")
	}
	for _, want := range []string{"codex", "interactive"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	// A prompt that fits must still be accepted.
	_, _, err = codex{}.Command("/w", writePrompt(t, "DO THE WORK"), Config{Interactive: true})
	if err != nil {
		t.Fatalf("prompt within the limit must be accepted: %v", err)
	}
}
