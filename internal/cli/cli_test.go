package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/commands"
	"github.com/alekzonder/tariboy/internal/plugins"
	"github.com/alekzonder/tariboy/internal/registry"
)

type fakeCaller struct {
	method, route string
	body          any
	result        json.RawMessage
	err           error
}

func (f *fakeCaller) Call(method, route string, body any) (json.RawMessage, error) {
	f.method, f.route, f.body = method, route, body
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type captureCaller struct{ route *string }

func (c *captureCaller) Call(method, route string, body any) (json.RawMessage, error) {
	*c.route = route
	return json.RawMessage(`{}`), nil
}

func testReg(t *testing.T) *registry.Registry {
	t.Helper()
	r := registry.New()
	r.Register(registry.Command{
		Path: "daemon.status", Summary: "status",
		HTTP:    &registry.HTTPRoute{Method: "GET", Path: "/api/daemon/status"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) { return nil, nil },
	})
	r.Register(registry.Command{
		Path: "daemon.config.set", Summary: "set config",
		Args: []registry.Arg{
			{Name: "key", Type: registry.String, Required: true},
			{Name: "value", Type: registry.String, Required: true},
		},
		HTTP:    &registry.HTTPRoute{Method: "POST", Path: "/api/daemon/config"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) { return nil, nil },
	})
	return r
}

func testTreeReg(t *testing.T) *registry.Registry {
	t.Helper()
	r := registry.New()
	reg := func(path, summary string, args ...registry.Arg) {
		if err := r.Register(registry.Command{
			Path: path, Summary: summary, Args: args,
			Handler: func(c *registry.Ctx, p registry.Params) (any, error) { return nil, nil },
		}); err != nil {
			t.Fatal(err)
		}
	}
	reg("agent.ps", "List agents")
	reg("agent.stop", "Stop an agent's loop", registry.Arg{Name: "name", Type: registry.String, Required: true})
	reg("agent.status.show", "Show one agent's status", registry.Arg{Name: "name", Type: registry.String, Required: true})
	reg("agent.status.history", "Show status history")
	reg("logs", "Tail daemon logs")
	r.RegisterGroup("agent", "Manage agents")
	r.RegisterGroup("agent.status", "Runtime status")
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	return r
}

func runHelp(t *testing.T, r *registry.Registry, argv ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(context.Background(), r, argv, &fakeCaller{result: json.RawMessage(`{}`)}, nil, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestRootHelpTwoSections(t *testing.T) {
	r := testTreeReg(t)
	out, _, code := runHelp(t, r)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "Command groups:") || !strings.Contains(out, "Commands:") {
		t.Fatalf("root help missing sections:\n%s", out)
	}
	if !strings.Contains(out, "agent") || !strings.Contains(out, "Manage agents") {
		t.Fatalf("root help missing agent group:\n%s", out)
	}
	if !strings.Contains(out, "logs") {
		t.Fatalf("root help missing standalone command:\n%s", out)
	}
	// A leaf under a group must NOT appear at the root.
	if strings.Contains(out, "List agents") {
		t.Fatalf("root help leaked a subcommand:\n%s", out)
	}
}

func TestGroupListing(t *testing.T) {
	r := testTreeReg(t)
	for _, argv := range [][]string{{"agent"}, {"agent", "--help"}, {"agent", "-h"}, {"agent", "help"}} {
		out, _, code := runHelp(t, r, argv...)
		if code != 0 {
			t.Fatalf("%v exit %d", argv, code)
		}
		if !strings.Contains(out, "ps") || !strings.Contains(out, "List agents") {
			t.Fatalf("%v missing leaf child:\n%s", argv, out)
		}
		if !strings.Contains(out, "status") {
			t.Fatalf("%v missing subgroup child:\n%s", argv, out)
		}
	}
}

func TestNestedGroupListing(t *testing.T) {
	r := testTreeReg(t)
	out, _, code := runHelp(t, r, "agent", "status")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "show") || !strings.Contains(out, "history") {
		t.Fatalf("nested group listing wrong:\n%s", out)
	}
}

func TestLeafDetailHelp(t *testing.T) {
	r := testTreeReg(t)
	for _, flag := range []string{"--help", "-h"} {
		out, _, code := runHelp(t, r, "agent", "stop", flag)
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(out, "Usage: tariboy agent stop") {
			t.Fatalf("%s: leaf detail missing usage:\n%s", flag, out)
		}
	}
}

func TestUnknownSubcommandUnderGroup(t *testing.T) {
	r := testTreeReg(t)
	out, errOut, code := runHelp(t, r, "agent", "frobnicate")
	_ = out
	if code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(errOut, "unknown command") {
		t.Fatalf("missing error:\n%s", errOut)
	}
	if !strings.Contains(errOut, "ps") {
		t.Fatalf("group listing not shown on error:\n%s", errOut)
	}
}

func TestRunGETCommand(t *testing.T) {
	f := &fakeCaller{result: json.RawMessage(`{"version":"2.0.0-dev"}`)}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), testReg(t), []string{"daemon", "status"}, f, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	if f.method != "GET" || f.route != "/api/daemon/status" {
		t.Fatalf("called %s %s", f.method, f.route)
	}
	if !strings.Contains(out.String(), "version: 2.0.0-dev") {
		t.Fatalf("human output: %q", out.String())
	}
}

func TestRunJSONFlag(t *testing.T) {
	f := &fakeCaller{result: json.RawMessage(`{"a":1}`)}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), testReg(t), []string{"--json", "daemon", "status"}, f, nil, &out, &errOut)
	if code != 0 || strings.TrimSpace(out.String()) != `{"a":1}` {
		t.Fatalf("exit=%d out=%q", code, out.String())
	}
}

func TestRunCreatesTaskQueue(t *testing.T) {
	caller := &fakeCaller{result: json.RawMessage(`{"prefix":"TEST"}`)}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), commands.BuildRegistry(), []string{
		"tasks", "queue", "create", "--prefix", "TEST", "--name", "Test queue",
		"--description", "CLI-created", "--owners", "alice,bob", "--responsible-agent", "worker",
	}, caller, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	if caller.method != "POST" || caller.route != "/api/task-queues" {
		t.Fatalf("called %s %s", caller.method, caller.route)
	}
	body := caller.body.(registry.Params)
	if body["prefix"] != "TEST" || body["name"] != "Test queue" || body["description"] != "CLI-created" || body["owners"] != "alice,bob" || body["responsible_agent"] != "worker" {
		t.Fatalf("body=%#v", body)
	}
	help, _, helpCode := runHelp(t, commands.BuildRegistry(), "tasks", "queue", "create", "--help")
	if helpCode != 0 || !strings.Contains(help, "--responsible-agent") {
		t.Fatalf("help exit=%d out=%s", helpCode, help)
	}
}

// TestRunValidateExitsNonZeroWhenInvalid pins that a validate command exits 1
// on a `valid: false` result, after printing the whole result, so a script
// can gate on it; the HTTP result itself is not an error.
func TestRunValidateExitsNonZeroWhenInvalid(t *testing.T) {
	src := t.TempDir()
	commandArgs := map[string][]string{
		"workflow validate": {"workflow", "validate", "--path", src},
		"image validate":    {"image", "validate", "--name", "developer", "--path", src},
	}
	invalid := `{"valid":false,"errors":[{"code":"transition_target_unknown","path":"statuses[0].transitions[0].to","message":"transition target \"nowhere\" names no status"}]}`
	for name, argv := range commandArgs {
		for _, mode := range []string{"text", "json"} {
			t.Run(name+" "+mode, func(t *testing.T) {
				args := append([]string{}, argv...)
				if mode == "json" {
					args = append(args, "--json")
				}
				var out, errOut bytes.Buffer
				code := Run(context.Background(), commands.BuildRegistry(), args, &fakeCaller{result: json.RawMessage(invalid)}, nil, &out, &errOut)
				if code != 1 || !strings.Contains(out.String(), "transition_target_unknown") {
					t.Fatalf("invalid: exit=%d out=%s err=%s", code, out.String(), errOut.String())
				}
				out.Reset()
				code = Run(context.Background(), commands.BuildRegistry(), args, &fakeCaller{result: json.RawMessage(`{"valid":true,"errors":[]}`)}, nil, &out, &errOut)
				if code != 0 || !strings.Contains(out.String(), "true") {
					t.Fatalf("valid: exit=%d out=%s err=%s", code, out.String(), errOut.String())
				}
			})
		}
	}
	// Another command with a `valid` field is not a gate.
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), testReg(t), []string{"daemon", "status"}, &fakeCaller{result: json.RawMessage(`{"valid":false}`)}, nil, &out, &errOut); code != 0 {
		t.Fatalf("daemon status exit=%d", code)
	}
}

func TestRunResolvesImageSourcePathAgainstClientCWD(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "work")
	source := filepath.Join(root, "source")
	for _, dir := range []string{workdir, source} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(workdir)

	for _, verb := range []string{"build", "validate"} {
		t.Run(verb, func(t *testing.T) {
			caller := &fakeCaller{result: json.RawMessage(`{}`)}
			var out, errOut bytes.Buffer
			code := Run(context.Background(), commands.BuildRegistry(), []string{
				"image", verb, "--name", "developer", "--path", "../source",
			}, caller, nil, &out, &errOut)
			if code != 0 {
				t.Fatalf("exit=%d err=%s", code, errOut.String())
			}
			body := caller.body.(registry.Params)
			if got := body["path"]; got != source {
				t.Fatalf("path = %q, want %q", got, source)
			}
		})
	}
}

func TestRunImageBuildRepeatableTags(t *testing.T) {
	source := t.TempDir()
	caller := &fakeCaller{result: json.RawMessage(`{}`)}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), commands.BuildRegistry(), []string{
		"image", "build", "--name", "reviewer", "--path", source,
		"--tag", "latest", "--tag", "v2",
	}, caller, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	body := caller.body.(registry.Params)
	if got, want := body["tag"], []string{"latest", "v2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tag = %#v, want %#v", got, want)
	}
}

func TestRunImageBuildStoreSelectorIsPositional(t *testing.T) {
	caller := &fakeCaller{result: json.RawMessage(`{}`)}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), commands.BuildRegistry(), []string{
		"image", "build", "team/reviewer", "--tag", "v2",
	}, caller, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	body := caller.body.(registry.Params)
	if body["source"] != "team/reviewer" || !reflect.DeepEqual(body["tag"], []string{"v2"}) {
		t.Fatalf("body = %#v", body)
	}
}

func TestRunPositionalAndFlagArgs(t *testing.T) {
	f := &fakeCaller{result: json.RawMessage(`{}`)}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), testReg(t), []string{"daemon", "config", "set", "log_level", "debug"}, f, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	body := f.body.(registry.Params)
	if body["key"] != "log_level" || body["value"] != "debug" {
		t.Fatalf("body=%v", body)
	}
	// flags form
	code = Run(context.Background(), testReg(t), []string{"daemon", "config", "set", "--key", "k", "--value", "v"}, f, nil, &out, &errOut)
	if code != 0 || f.body.(registry.Params)["key"] != "k" {
		t.Fatalf("flag form failed: %v", f.body)
	}
}

func TestMergePluginCommandsRunsActionAndReadsSecretFile(t *testing.T) {
	r := registry.New()
	caller := &fakeCaller{result: json.RawMessage(`{"configured":true}`)}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("123:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contributions := []plugins.Contribution{{
		Name: "telegram", Description: "Telegram integration",
		Commands: []plugins.OperatorCommand{{
			Path: "configure", Summary: "Configure Telegram", Action: "configure",
			Args: []plugins.OperatorArg{
				{Name: "token", Flag: "token-file", Type: "secret-file"},
				{Name: "allowed_uids", Flag: "allowed-uids", Type: "integer-list", Required: true},
			},
		}},
	}}
	if err := MergePluginCommands(r, contributions, caller, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), r, []string{
		"telegram", "configure", "--token-file", tokenFile, "--allowed-uids", "22,11,22",
	}, caller, &registry.Ctx{}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	if caller.method != "POST" || caller.route != "/api/plugins/telegram/action" {
		t.Fatalf("called %s %s", caller.method, caller.route)
	}
	body := caller.body.(map[string]any)
	if body["action"] != "configure" {
		t.Fatalf("body = %#v", body)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(body["data"].(string)), &data); err != nil {
		t.Fatal(err)
	}
	if data["token"] != "123:secret" || strings.Contains(body["data"].(string), tokenFile) {
		t.Fatalf("secret action data = %#v", data)
	}
	if got := data["allowed_uids"].([]any); len(got) != 3 || got[0] != float64(22) {
		t.Fatalf("allowed_uids = %#v", data["allowed_uids"])
	}
	help, _, helpCode := runHelp(t, r, "telegram")
	if helpCode != 0 || !strings.Contains(help, "configure") {
		t.Fatalf("help exit=%d out=%s", helpCode, help)
	}
}

func TestMergePluginCommandsRejectsCoreNamespaceCollision(t *testing.T) {
	r := testTreeReg(t)
	err := MergePluginCommands(r, []plugins.Contribution{{
		Name:     "agent",
		Commands: []plugins.OperatorCommand{{Path: "configure", Summary: "bad", Action: "configure"}},
	}}, &fakeCaller{}, strings.NewReader(""))
	if err == nil {
		t.Fatal("plugin namespace collision should be rejected")
	}
}

func TestLoadPluginCommandsDiscoversRegistry(t *testing.T) {
	caller := &fakeCaller{result: json.RawMessage(`{
  "plugins":[{"name":"telegram","description":"Telegram integration",
    "operator_commands":[{"path":"status","summary":"Show status","action":"status"}]}]
}`)}
	r := registry.New()
	if err := LoadPluginCommands(r, caller, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if caller.method != "GET" || caller.route != "/api/plugin-contributions" {
		t.Fatalf("discovery called %s %s", caller.method, caller.route)
	}
	out, _, code := runHelp(t, r, "telegram")
	if code != 0 || !strings.Contains(out, "status") {
		t.Fatalf("help exit=%d out=%s", code, out)
	}
}

func TestMergePluginCommandsRejectsPermissiveSecretFile(t *testing.T) {
	r := registry.New()
	caller := &fakeCaller{result: json.RawMessage(`{}`)}
	if err := MergePluginCommands(r, []plugins.Contribution{{
		Name: "telegram",
		Commands: []plugins.OperatorCommand{{
			Path: "configure", Summary: "Configure", Action: "configure",
			Args: []plugins.OperatorArg{{Name: "token", Flag: "token-file", Type: "secret-file", Required: true}},
		}},
	}}, caller, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), r, []string{"telegram", "configure", "--token-file", path}, caller, &registry.Ctx{}, &out, &errOut)
	if code == 0 || !strings.Contains(errOut.String(), "owner-only") {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	if caller.method != "" {
		t.Fatalf("plugin called despite unsafe secret file: %s %s", caller.method, caller.route)
	}
}

func TestRunMissingRequiredArg(t *testing.T) {
	f := &fakeCaller{result: json.RawMessage(`{}`)}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), testReg(t), []string{"daemon", "config", "set", "onlykey"}, f, nil, &out, &errOut)
	if code == 0 || !strings.Contains(errOut.String(), "value") {
		t.Fatalf("exit=%d err=%q", code, errOut.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), testReg(t), []string{"bogus"}, &fakeCaller{}, nil, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit=%d", code)
	}
}

func TestRunCLIHiddenNotResolvable(t *testing.T) {
	var gotBody any
	r := registry.New()
	r.Register(registry.Command{
		Path: "secret.store", Summary: "store", CLIHidden: true,
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true},
			{Name: "key", Type: registry.String, Required: true},
			{Name: "value", Type: registry.String, Required: true},
		},
		HTTP:    &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/secrets"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) { gotBody = p; return nil, nil },
	})
	f := &fakeCaller{result: json.RawMessage(`{}`)}
	var out, errOut bytes.Buffer
	// The value must never be reachable as a CLI positional argv path.
	code := Run(context.Background(), r, []string{"secret", "store", "smoke", "TOKEN", "s3cr3t"}, f, nil, &out, &errOut)
	if code != 2 {
		t.Fatalf("hidden command resolved: exit=%d", code)
	}
	if gotBody != nil || f.route != "" {
		t.Fatalf("hidden command was invoked: body=%v route=%q", gotBody, f.route)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("err=%q", errOut.String())
	}
	// And it must not be advertised in root help.
	out.Reset()
	Run(context.Background(), r, []string{"--help"}, f, nil, &out, &errOut)
	if strings.Contains(out.String(), "secret store") {
		t.Fatalf("hidden command listed in help: %q", out.String())
	}
}

func TestRunAPIError(t *testing.T) {
	f := &fakeCaller{err: &client.APIError{Code: "nope", Msg: "denied"}}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), testReg(t), []string{"daemon", "status"}, f, nil, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "nope") {
		t.Fatalf("exit=%d err=%q", code, errOut.String())
	}
}

func TestHelpJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), testReg(t), []string{"--help-json"}, &fakeCaller{}, nil, &out, &errOut)
	if code != 0 {
		t.Fatal("help-json failed")
	}
	var tree map[string]any
	if err := json.Unmarshal(out.Bytes(), &tree); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if tree["daemon"] == nil {
		t.Fatalf("tree: %v", tree)
	}
}

func TestRunSubstitutesPathValue(t *testing.T) {
	var gotRoute string
	fc := &captureCaller{route: &gotRoute}
	r := registry.New()
	r.Register(registry.Command{
		Path: "agent.status", Summary: "s",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true}},
		HTTP:    &registry.HTTPRoute{Method: "GET", Path: "/api/agents/{name}/status"},
		Handler: func(*registry.Ctx, registry.Params) (any, error) { return nil, nil },
	})
	var out, errOut bytes.Buffer
	Run(context.Background(), r, []string{"agent", "status", "smoke"}, fc, nil, &out, &errOut)
	if gotRoute != "/api/agents/smoke/status" {
		t.Fatalf("route = %q", gotRoute)
	}
}

func TestParseArgsShortAlias(t *testing.T) {
	cmd := cmdWith(registry.Arg{Name: "harness", Flag: "harness", Short: "a", Type: registry.String})
	p, err := parseArgs(cmd, []string{"-a", "codex"})
	if err != nil || p["harness"] != "codex" {
		t.Fatalf("short alias: p=%v err=%v", p, err)
	}
}

func TestParseArgsBoolSpaceValue(t *testing.T) {
	cmd := cmdWith(
		registry.Arg{Name: "archive", Flag: "archive", Type: registry.Bool},
		registry.Arg{Name: "keep-days", Flag: "keep-days", Type: registry.Int},
	)
	p, err := parseArgs(cmd, []string{"--archive", "true"})
	if err != nil || p["archive"] != true {
		t.Fatalf("--archive true: p=%v err=%v", p, err)
	}
	p, err = parseArgs(cmd, []string{"--archive", "false"})
	if err != nil || p["archive"] != false {
		t.Fatalf("--archive false: p=%v err=%v", p, err)
	}
	p, err = parseArgs(cmd, []string{"--archive"})
	if err != nil || p["archive"] != true {
		t.Fatalf("bare --archive: p=%v err=%v", p, err)
	}
	p, err = parseArgs(cmd, []string{"--archive=false"})
	if err != nil || p["archive"] != false {
		t.Fatalf("--archive=false: p=%v err=%v", p, err)
	}
	// A bare bool flag followed by a non-bool token (a positional/int flag's
	// value here) must NOT consume that token.
	p, err = parseArgs(cmd, []string{"--archive", "--keep-days", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if p["archive"] != true {
		t.Fatalf("archive = %v, want true", p["archive"])
	}
	if p["keep-days"] != 3 {
		t.Fatalf("keep-days = %v, want 3 (wrongly consumed by --archive?)", p["keep-days"])
	}
}

func cmdWith(args ...registry.Arg) registry.Command {
	return registry.Command{
		Path: "x", Summary: "s", Args: args,
		Handler: func(*registry.Ctx, registry.Params) (any, error) { return nil, nil },
	}
}

func TestParseArgsBoolStrict(t *testing.T) {
	cmd := cmdWith(registry.Arg{Name: "v", Flag: "v", Type: registry.Bool})
	if _, err := parseArgs(cmd, []string{"--v=maybe"}); err == nil {
		t.Fatal("non-boolean value accepted")
	}
	for in, want := range map[string]bool{"--v=true": true, "--v=1": true, "--v=false": false, "--v=0": false} {
		p, err := parseArgs(cmd, []string{in})
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if p["v"] != want {
			t.Fatalf("%s => %v, want %v", in, p["v"], want)
		}
	}
}

func TestParseArgsExplicitEmptyValue(t *testing.T) {
	// `--key=` sets an empty string and must NOT swallow the next token.
	cmd := cmdWith(
		registry.Arg{Name: "key", Flag: "key", Type: registry.String},
		registry.Arg{Name: "pos", Type: registry.String},
	)
	p, err := parseArgs(cmd, []string{"--key=", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if p["key"] != "" {
		t.Fatalf("key = %q, want empty", p["key"])
	}
	if p["pos"] != "hello" {
		t.Fatalf("pos = %q, want hello (next token wrongly consumed)", p["pos"])
	}
}

func TestParseArgsShortFlag(t *testing.T) {
	cmd := cmdWith(registry.Arg{Name: "tag", Flag: "tag", Type: registry.String, Required: true})
	p, err := parseArgs(cmd, []string{"-t", "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if p["tag"] != "v1" {
		t.Fatalf("tag = %q, want v1", p["tag"])
	}
}

func TestRunLocalCommand(t *testing.T) {
	r := registry.New()
	r.Register(registry.Command{
		Path: "img.ls", Summary: "list",
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			return map[string]any{"base": c.BaseDir}, nil
		},
	})
	var out, errOut bytes.Buffer
	code := Run(context.Background(), r, []string{"img", "ls"}, &fakeCaller{}, &registry.Ctx{BaseDir: "/b"}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "base: /b") {
		t.Fatalf("exit=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestRunLocalNoContext(t *testing.T) {
	r := registry.New()
	r.Register(registry.Command{
		Path: "img.ls", Summary: "list",
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) { return nil, nil },
	})
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), r, []string{"img", "ls"}, &fakeCaller{}, nil, &out, &errOut); code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
}

func TestRunLocalUserError(t *testing.T) {
	r := registry.New()
	r.Register(registry.Command{
		Path: "img.rm", Summary: "rm",
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			return nil, api.UserError{Code: "not_found", Msg: "no image"}
		},
	})
	var out, errOut bytes.Buffer
	code := Run(context.Background(), r, []string{"img", "rm"}, &fakeCaller{}, &registry.Ctx{}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "not_found") {
		t.Fatalf("exit=%d err=%q", code, errOut.String())
	}
}

func TestRunLocalStringResult(t *testing.T) {
	r := registry.New()
	r.Register(registry.Command{
		Path: "img.prompt", Summary: "prompt",
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) { return "line1\nline2", nil },
	})
	var out, errOut bytes.Buffer
	code := Run(context.Background(), r, []string{"img", "prompt"}, &fakeCaller{}, &registry.Ctx{}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "line1\nline2") {
		t.Fatalf("exit=%d out=%q", code, out.String())
	}
}

type errLocalCaller struct{ err error }

func (c *errLocalCaller) Call(string, string, any) (json.RawMessage, error) { return nil, c.err }

func TestPrintHumanNestedMap(t *testing.T) {
	var buf bytes.Buffer
	printHuman(json.RawMessage(`{"agent":"bot","pruned":["a","b"],"policy":{"keep_iterations":3}}`), &buf)
	out := buf.String()
	// Scalar stays key: value; nested value is JSON, not Go's map[...] form.
	if !strings.Contains(out, "agent: bot") {
		t.Fatalf("scalar missing: %q", out)
	}
	if strings.Contains(out, "map[") {
		t.Fatalf("nested value rendered as Go map: %q", out)
	}
	if !strings.Contains(out, `"keep_iterations": 3`) && !strings.Contains(out, `keep_iterations`) {
		t.Fatalf("nested map not rendered as JSON: %q", out)
	}
}

func TestLocalHandlerMapsAPIError(t *testing.T) {
	// A CLI-local composite whose handler returns a *client.APIError must print
	// the standard "error (code): msg" line and exit 1.
	r := registry.New()
	r.Register(registry.Command{
		Path: "cp", Summary: "s",
		Args: []registry.Arg{{Name: "src", Type: registry.String, Required: true}},
		Handler: func(*registry.Ctx, registry.Params) (any, error) {
			return nil, &client.APIError{Code: "not_found", Msg: "file not found"}
		},
	})
	var out, errOut bytes.Buffer
	code := Run(context.Background(), r, []string{"cp", "x"}, &errLocalCaller{}, &registry.Ctx{}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "error (not_found): file not found") {
		t.Fatalf("code=%d err=%q", code, errOut.String())
	}
}
