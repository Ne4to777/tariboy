// Package harness adapts an agent's harness config into a runnable (argv, env).
// M3 runs claude and stub for real; codex/opencode are declared but unexercised.
package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// sessionNamespace anchors the UUIDv5 derivation of session ids. It is an
// arbitrary fixed UUID; changing it would remap every iteration to a new
// session id, so treat it as stable.
var sessionNamespace = uuid.MustParse("2f1c8d3e-9b4a-4c6d-8e0f-1a2b3c4d5e6f")

var errExecutableNotFound = errors.New("harness executable not found")

// maxArgStrLen mirrors the kernel's MAX_ARG_STRLEN (32 pages, i.e. 131072 bytes
// on the 4 KiB-page platforms tariboy targets). execve rejects any single
// argv element that long with E2BIG, and /bin/sh reports that as a bare exit
// 126 with no message on stdout, stderr or in the shim log. A prompt therefore
// has to reach the harness on stdin, not in argv: an agent whose context grows
// over time otherwise crosses the limit mid-life and can never start again.
const maxArgStrLen = 32 * 4096

// promptOnStdinCommand runs the harness with the prompt file as its stdin.
// Positionals are $0=tariboy-harness, $1=executable, $2=prompt path, and
// everything after that is the harness's own argv.
const promptOnStdinCommand = `cmd=$1; prompt=$2; shift 2; exec "$cmd" "$@" < "$prompt"`

// errPromptTooLargeForArgv reports a prompt that cannot be handed to a harness
// which only accepts it in argv. See maxArgStrLen.
func errPromptTooLargeForArgv(harness, promptPath string) error {
	info, err := os.Stat(promptPath)
	if err != nil || info.Size() < maxArgStrLen {
		return nil
	}
	return fmt.Errorf("interactive %s cannot take a %d-byte prompt: its TUI requires a terminal on stdin, "+
		"so the prompt must go in argv, and execve caps a single argument at %d bytes; shorten the agent's context",
		harness, info.Size(), maxArgStrLen)
}

// ensureUUID returns a valid UUID string for use as a harness --session-id.
// claude requires --session-id to be a UUID, but tariboy identifies
// iterations with human-readable ids like "daring-sparrow-20260705161437-1".
// A value that already parses as a UUID is passed through unchanged; anything
// else is mapped to a deterministic UUIDv5 so the session id stays stable and
// unique per iteration.
func ensureUUID(sid string) string {
	if sid == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	if _, err := uuid.Parse(sid); err == nil {
		return sid
	}
	return uuid.NewSHA1(sessionNamespace, []byte(sid)).String()
}

type Config struct {
	Model       string
	Effort      string
	Interactive bool
	SessionID   string
	// ProxyURL is the tokenized Tariboy proxy URL for this iteration. Codex
	// needs an explicit model provider override: its ChatGPT login provider does
	// not use OPENAI_BASE_URL for inference traffic.
	ProxyURL string
	// Bare launches the harness with no initial prompt and no tariboy
	// tooling flags (instructions-free session; image manifest bare=true).
	Bare bool
}

type Adapter interface {
	Type() string
	// Executable is the real harness program that must be discoverable in the
	// final per-iteration environment before the shim starts.
	Executable() string
	Command(cwd, promptPath string, cfg Config) (argv []string, env []string, err error)
	SkillBridge(SkillBridgeRequest) (SkillBridge, error)
}

func Get(typ string) (Adapter, error) {
	switch typ {
	case "claude":
		return claude{}, nil
	case "codex":
		return codex{}, nil
	case "opencode":
		return opencode{}, nil
	case "cursor":
		return cursor{}, nil
	case "stub":
		path := os.Getenv("TARIBOY_STUB_HARNESS")
		if path == "" {
			return nil, fmt.Errorf("stub harness requires TARIBOY_STUB_HARNESS to point at the stub script")
		}
		return stub{path: path}, nil
	default:
		return nil, fmt.Errorf("unknown harness %q (want claude|codex|opencode|cursor|stub)", typ)
	}
}

// FindExecutable validates executable against the PATH carried by env using
// cwd as the process working directory, without consulting the current process
// environment. Absolute executable paths are validated directly.
func FindExecutable(executable string, env []string, cwd string) (string, error) {
	if filepath.IsAbs(executable) {
		if isExecutable(executable) {
			return executable, nil
		}
		return "", errExecutableNotFound
	}
	if strings.ContainsRune(executable, filepath.Separator) {
		candidate := filepath.Join(cwd, executable)
		if isExecutable(candidate) {
			return candidate, nil
		}
		return "", errExecutableNotFound
	}

	path := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			path = strings.TrimPrefix(entry, "PATH=")
		}
	}
	pathDirs := filepath.SplitList(path)
	if path == "" {
		pathDirs = []string{""}
	}
	for _, dir := range pathDirs {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		candidate := filepath.Join(dir, executable)
		if isExecutable(candidate) {
			return candidate, nil
		}
	}
	return "", errExecutableNotFound
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

type claude struct{}

func (claude) Type() string       { return "claude" }
func (claude) Executable() string { return "claude" }

func (claude) Command(cwd, promptPath string, cfg Config) ([]string, []string, error) {
	sid := ensureUUID(cfg.SessionID)
	if cfg.Bare {
		// No prompt, no allowedTools. The non-login shell preserves the daemon's
		// already-resolved effective environment.
		argv := []string{
			"/bin/sh", "-c",
			`cmd=$1; shift; exec "$cmd" "$@"`,
			"tariboy-harness", "claude",
		}
		if !cfg.Interactive {
			argv = append(argv, "-p", "--output-format", "stream-json", "--verbose")
		}
		argv = append(argv, "--dangerously-skip-permissions", "--session-id", sid)
		if cfg.Model != "" {
			argv = append(argv, "--model", cfg.Model)
		}
		if cfg.Effort != "" {
			argv = append(argv, "--effort", cfg.Effort)
		}
		return argv, nil, nil
	}
	// claude takes its prompt from stdin in both modes: print mode says so
	// outright ("Input must be provided either through stdin or as a prompt
	// argument when using --print"), and the TUI reads stdin as its first
	// message while still drawing on the pane's tty, leaving the input box free
	// for the operator's send-keys. Redirecting keeps prompts of any size off
	// argv, clear of MAX_ARG_STRLEN.
	argv := []string{
		"/bin/sh", "-c",
		promptOnStdinCommand,
		"tariboy-harness", "claude", promptPath,
	}
	// Print mode (-p) streams a machine-readable result for the non-interactive
	// loop, where the shim parses stdout. An interactive agent instead runs its
	// TUI inside tmux (driven by the operator via send-keys), so it must launch
	// WITHOUT -p and the print-only output flags.
	if !cfg.Interactive {
		argv = append(argv, "-p", "--output-format", "stream-json", "--verbose")
	}
	argv = append(argv, "--dangerously-skip-permissions", "--allowedTools", "Bash(i-am-done)", "--session-id", sid)
	if cfg.Model != "" {
		argv = append(argv, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		argv = append(argv, "--effort", cfg.Effort)
	}
	return argv, nil, nil
}

func (claude) SkillBridge(request SkillBridgeRequest) (SkillBridge, error) {
	return claudeSkillBridge(request)
}

type codex struct{}

func (codex) Type() string       { return "codex" }
func (codex) Executable() string { return "codex" }

func (codex) Command(cwd, promptPath string, cfg Config) ([]string, []string, error) {
	// `codex exec` documents stdin as a prompt source ("If not provided as an
	// argument (or if `-` is used), instructions are read from stdin"), which
	// keeps the prompt off argv. It must not also be passed positionally: codex
	// would then append stdin as a separate <stdin> block.
	shellCommand := promptOnStdinCommand
	codexArgs := []string{
		"exec", "-c", "allow_login_shell=false", "--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check", "--cd", cwd,
	}
	if cfg.Interactive {
		// The codex TUI refuses a redirected stdin outright ("Error: stdin is not
		// a terminal"), so unlike every other path its prompt has to stay in argv
		// and stays bound by MAX_ARG_STRLEN. Fail with something diagnosable
		// rather than letting execve return an opaque 126.
		if err := errPromptTooLargeForArgv("codex", promptPath); err != nil {
			return nil, nil, err
		}
		shellCommand = `prompt=$(cat "$2") || exit; cmd=$1; shift 2; exec "$cmd" "$@" "$prompt"`
		codexArgs = []string{
			"-c", "allow_login_shell=false",
			"--dangerously-bypass-approvals-and-sandbox", "--cd", cwd,
		}
	}
	argv := []string{
		"/bin/sh", "-c", shellCommand,
		"tariboy-harness", "codex", promptPath,
	}
	argv = append(argv, codexArgs...)
	if cfg.ProxyURL != "" {
		argv = append(argv,
			"-c", `model_provider="tariboy"`,
			"-c", `model_providers.tariboy.name="Tariboy"`,
			"-c", "model_providers.tariboy.base_url="+strconv.Quote(cfg.ProxyURL),
			"-c", `model_providers.tariboy.requires_openai_auth=true`,
			"-c", `model_providers.tariboy.wire_api="responses"`,
		)
	}
	if cfg.Model != "" {
		argv = append(argv, "--model", cfg.Model)
	}
	return argv, nil, nil
}

func (codex) SkillBridge(request SkillBridgeRequest) (SkillBridge, error) {
	return codexSkillBridge(request)
}

type opencode struct{}

func (opencode) Type() string       { return "opencode" }
func (opencode) Executable() string { return "opencode" }

func (opencode) Command(cwd, promptPath string, cfg Config) ([]string, []string, error) {
	// `opencode run` reads the whole prompt from stdin whenever stdin is not a
	// tty, and concatenates it after any positional message — so the prompt is
	// passed by redirection only, never in argv.
	return []string{
		"/bin/sh", "-c",
		promptOnStdinCommand,
		"tariboy-harness", "opencode", promptPath,
		"run",
	}, nil, nil
}

func (opencode) SkillBridge(request SkillBridgeRequest) (SkillBridge, error) {
	return openCodeSkillBridge(request)
}

type cursor struct{}

func (cursor) Type() string       { return "cursor" }
func (cursor) Executable() string { return "agent" }

func (cursor) Command(cwd, promptPath string, cfg Config) ([]string, []string, error) {
	shellCommand := `cmd=$1; shift; exec "$cmd" "$@"`
	if !cfg.Bare {
		if err := errPromptTooLargeForArgv("cursor", promptPath); err != nil {
			return nil, nil, err
		}
		shellCommand = `prompt=$(cat "$2") || exit; cmd=$1; shift 2; exec "$cmd" "$@" "$prompt"`
	}
	argv := []string{
		"/bin/sh", "-c", shellCommand,
		"tariboy-harness", "agent",
	}
	if !cfg.Bare {
		argv = append(argv, promptPath)
	}
	if !cfg.Interactive {
		argv = append(argv, "-p", "--output-format", "stream-json")
	}
	argv = append(argv, "--force")
	if !cfg.Interactive {
		argv = append(argv, "--trust")
	}
	if cfg.Model != "" {
		argv = append(argv, "--model", cfg.Model)
	}
	return argv, nil, nil
}

func (cursor) SkillBridge(request SkillBridgeRequest) (SkillBridge, error) {
	return cursorSkillBridge(request)
}

type stub struct{ path string }

func (stub) Type() string         { return "stub" }
func (s stub) Executable() string { return s.path }

// Command runs the stub script with the prompt path as its single argument;
// behaviour is driven by STUB_* env vars set by the test/e2e harness.
func (s stub) Command(cwd, promptPath string, cfg Config) ([]string, []string, error) {
	return []string{s.path, promptPath}, nil, nil
}

func (stub) SkillBridge(request SkillBridgeRequest) (SkillBridge, error) {
	return SkillBridge{}, nil
}
