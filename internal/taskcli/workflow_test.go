package taskcli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/client"
)

func agentEnv() func(string) string { return mapEnv("TARIBOY_TOOLS_SOCKET", "/agent.sock") }

func withCaller(t *testing.T, c Caller) {
	t.Helper()
	old := newCaller
	t.Cleanup(func() { newCaller = old })
	newCaller = func(string) Caller { return c }
}

func withStdin(t *testing.T, input string) {
	t.Helper()
	old := stdin
	t.Cleanup(func() { stdin = old })
	stdin = strings.NewReader(input)
}

func TestAgentWorkflowCommandsUseToolsActions(t *testing.T) {
	tests := []struct {
		argv   []string
		action string
		body   map[string]any
	}{
		{[]string{"advance", "DEV-1", "--outcome", "ready", "--message", "PR up"}, "advance", map[string]any{"key": "DEV-1", "outcome": "ready", "message": "PR up"}},
		{[]string{"advance", "DEV-1", "--outcome", "ready"}, "advance", map[string]any{"key": "DEV-1", "outcome": "ready"}},
		{[]string{"advance", "DEV-1", "--outcome", "ready", "--from", "develop"}, "advance", map[string]any{"key": "DEV-1", "outcome": "ready", "from": "develop"}},
		{[]string{"artifacts", "set", "DEV-1", "plan", "step 1"}, "artifact_set", map[string]any{"key": "DEV-1", "name": "plan", "value": "step 1"}},
		{[]string{"artifacts", "ls", "DEV-1"}, "artifact_ls", map[string]any{"key": "DEV-1"}},
		{[]string{"artifacts", "show", "DEV-1", "plan"}, "artifact_show", map[string]any{"key": "DEV-1", "name": "plan"}},
		{[]string{"workflow", "get", "DEV-1"}, "workflow_get", map[string]any{"key": "DEV-1"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.argv, " "), func(t *testing.T) {
			r := &recorder{result: json.RawMessage(`{}`)}
			withCaller(t, r)
			var errOut strings.Builder
			if code := Run(context.Background(), tt.argv, agentEnv(), io.Discard, &errOut); code != 0 {
				t.Fatalf("code %d: %s", code, &errOut)
			}
			if len(r.calls) != 1 || r.calls[0].method != "POST" || r.calls[0].route != "/tools/tasks/"+tt.action || !sameJSON(r.calls[0].body, tt.body) {
				t.Fatalf("calls = %#v; want POST /tools/tasks/%s %#v", r.calls, tt.action, tt.body)
			}
		})
	}
}

func TestOperatorWorkflowCommandsUseRestRoutes(t *testing.T) {
	tests := []struct {
		argv          []string
		method, route string
		body          any
	}{
		{[]string{"advance", "DEV-1", "--outcome", "approve", "--message", "ok"}, "POST", "/api/tasks/DEV-1/advance", map[string]any{"outcome": "approve", "message": "ok"}},
		{[]string{"advance", "DEV-1", "--outcome", "approve", "--from", "approval"}, "POST", "/api/tasks/DEV-1/advance", map[string]any{"outcome": "approve", "from": "approval"}},
		{[]string{"artifacts", "set", "DEV-1", "plan", "v"}, "PUT", "/api/tasks/DEV-1/artifacts/plan", map[string]any{"value": "v"}},
		{[]string{"artifacts", "ls", "DEV-1"}, "GET", "/api/tasks/DEV-1/artifacts", map[string]string{}},
		{[]string{"artifacts", "show", "DEV-1", "plan"}, "GET", "/api/tasks/DEV-1/artifacts/plan", map[string]string{}},
		{[]string{"workflow", "get", "DEV-1"}, "GET", "/api/tasks/DEV-1/workflow", map[string]string{}},
		{[]string{"workflow", "move", "DEV-1", "--to", "develop", "--reason", "regression"}, "POST", "/api/tasks/DEV-1/workflow/move", map[string]any{"to": "develop", "reason": "regression"}},
		{[]string{"cancel", "DEV-1"}, "POST", "/api/tasks/DEV-1/cancel", map[string]any{}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.argv, " "), func(t *testing.T) {
			r := &recorder{result: json.RawMessage(`{}`)}
			withCaller(t, r)
			var errOut strings.Builder
			if code := Run(context.Background(), tt.argv, operatorEnv(t), io.Discard, &errOut); code != 0 {
				t.Fatalf("code %d: %s", code, &errOut)
			}
			if len(r.calls) != 1 || r.calls[0].method != tt.method || r.calls[0].route != tt.route || !sameJSON(r.calls[0].body, tt.body) {
				t.Fatalf("calls = %#v; want %s %s %#v", r.calls, tt.method, tt.route, tt.body)
			}
		})
	}
}

func TestAgentModeRefusesOperatorOnlyWorkflowCommandsLocally(t *testing.T) {
	r := &recorder{result: json.RawMessage(`{}`)}
	withCaller(t, r)
	for _, argv := range [][]string{
		{"workflow", "move", "DEV-1", "--to", "develop", "--reason", "x"},
		{"cancel", "DEV-1"},
		{"queue", "workflow", "get", "DEV"},
		{"queue", "workflow", "set", "DEV", "development:latest"},
		{"queue", "workflow", "clear", "DEV", "--revision", "1"},
	} {
		var errOut strings.Builder
		if code := Run(context.Background(), argv, agentEnv(), io.Discard, &errOut); code != 2 || !strings.Contains(errOut.String(), "unknown command") {
			t.Errorf("%q = %d %q; want 2 and an unknown-command refusal", argv, code, &errOut)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("calls = %#v; want none", r.calls)
	}
}

func TestWorkflowCommandUsageErrors(t *testing.T) {
	r := &recorder{}
	withCaller(t, r)
	for _, argv := range [][]string{
		{"advance", "DEV-1"},
		{"advance", "--outcome", "ready"},
		{"artifacts", "frobnicate", "DEV-1"},
		{"artifacts", "set", "DEV-1"},
		{"artifacts", "set", "DEV-1", "plan", "v", "extra"},
		{"artifacts", "set", "DEV-1", "plan", "v", "--file", "x"},
		{"artifacts", "show", "DEV-1"},
		{"workflow", "get"},
		{"workflow", "move", "DEV-1", "--to", "develop"},
		{"workflow", "move", "DEV-1", "--reason", "x"},
		{"cancel"},
	} {
		if code := Run(context.Background(), argv, agentEnv(), io.Discard, io.Discard); code != 2 {
			t.Errorf("Run(%q) = %d; want 2", argv, code)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("calls = %#v; want none", r.calls)
	}
}

func TestArtifactValueComesFromStdinOrFileAsGiven(t *testing.T) {
	r := &recorder{result: json.RawMessage(`{}`)}
	withCaller(t, r)
	withStdin(t, "from stdin\n")
	if code := Run(context.Background(), []string{"artifacts", "set", "DEV-1", "plan"}, agentEnv(), io.Discard, io.Discard); code != 0 {
		t.Fatalf("stdin code %d", code)
	}
	if got := r.calls[0].body.(map[string]any)["value"]; got != "from stdin\n" {
		t.Fatalf("stdin value = %q; want it stored as given", got)
	}
	path := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(path, []byte("from file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(context.Background(), []string{"artifacts", "set", "DEV-1", "plan", "--file", path}, agentEnv(), io.Discard, io.Discard); code != 0 {
		t.Fatalf("file code %d", code)
	}
	body := r.calls[1].body.(map[string]any)
	if body["value"] != "from file\n" || body["file"] != nil {
		t.Fatalf("file body = %#v", body)
	}
	var errOut strings.Builder
	if code := Run(context.Background(), []string{"artifacts", "set", "DEV-1", "plan", "--file", path + ".missing"}, agentEnv(), io.Discard, &errOut); code != 2 || len(r.calls) != 2 {
		t.Fatalf("missing file = %d, calls %d, stderr %q", code, len(r.calls), &errOut)
	}
}

func TestArtifactInputReadsOneByteMoreThanTheLimit(t *testing.T) {
	r := &recorder{result: json.RawMessage(`{}`)}
	withCaller(t, r)
	withStdin(t, strings.Repeat("x", 200<<10))
	if code := Run(context.Background(), []string{"artifacts", "set", "DEV-1", "plan"}, agentEnv(), io.Discard, io.Discard); code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := len(r.calls[0].body.(map[string]any)["value"].(string)); got != 64<<10+1 {
		t.Fatalf("sent %d bytes; want 64 KiB + 1 so the server reports artifact_too_large", got)
	}
}

func TestArtifactSetFailsFastOnATerminalAndOnInvalidText(t *testing.T) {
	r := &recorder{result: json.RawMessage(`{}`)}
	withCaller(t, r)
	old := isTerminal
	t.Cleanup(func() { isTerminal = old })

	isTerminal = func(io.Reader) bool { return true }
	withStdin(t, "never read")
	var errOut strings.Builder
	if code := Run(context.Background(), []string{"artifacts", "set", "DEV-1", "plan"}, agentEnv(), io.Discard, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "pipe the value on stdin") || len(r.calls) != 0 {
		t.Fatalf("terminal stdin = %d %q calls %d", code, &errOut, len(r.calls))
	}
	// A terminal does not matter when VALUE or --file supplies the value.
	if code := Run(context.Background(), []string{"artifacts", "set", "DEV-1", "plan", "v"}, agentEnv(), io.Discard, io.Discard); code != 0 {
		t.Fatalf("VALUE with a terminal stdin = %d", code)
	}
	isTerminal = func(io.Reader) bool { return false }

	withStdin(t, "ok \xff\xfe binary")
	errOut.Reset()
	if code := Run(context.Background(), []string{"artifacts", "set", "DEV-1", "plan"}, agentEnv(), io.Discard, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "stdin is not valid UTF-8") || len(r.calls) != 1 {
		t.Fatalf("binary stdin = %d %q calls %d", code, &errOut, len(r.calls))
	}
	path := filepath.Join(t.TempDir(), "blob.bin")
	if err := os.WriteFile(path, []byte{0x89, 'P', 'N', 'G', 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if code := Run(context.Background(), []string{"artifacts", "set", "DEV-1", "plan", "--file", path}, agentEnv(), io.Discard, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), path+" is not valid UTF-8") || len(r.calls) != 1 {
		t.Fatalf("binary file = %d %q calls %d", code, &errOut, len(r.calls))
	}
}

func TestOperatorRoutesEscapeKeyAndArtifactName(t *testing.T) {
	r := &recorder{result: json.RawMessage(`{}`)}
	withCaller(t, r)
	for _, argv := range [][]string{
		{"artifacts", "set", "DEV-1", "a/b?c", "v"},
		{"artifacts", "show", "DEV-1", "a/b?c"},
	} {
		if code := Run(context.Background(), argv, operatorEnv(t), io.Discard, io.Discard); code != 0 {
			t.Fatalf("%q code %d", argv, code)
		}
	}
	for _, call := range r.calls {
		if call.route != "/api/tasks/DEV-1/artifacts/a%2Fb%3Fc" {
			t.Errorf("route = %q; want the name escaped", call.route)
		}
	}
	r.calls = nil
	if code := Run(context.Background(), []string{"cancel", "DEV/../x?y"}, operatorEnv(t), io.Discard, io.Discard); code != 0 ||
		r.calls[0].route != "/api/tasks/DEV%2F..%2Fx%3Fy/cancel" {
		t.Fatalf("cancel calls = %#v", r.calls)
	}
}

func TestExtraArgumentErrorsNameTheRealCommand(t *testing.T) {
	for argv, want := range map[string]string{
		"artifacts set DEV-1 plan v extra":        "tasks artifacts set: unexpected argument: extra",
		"artifacts ls DEV-1 extra":                "tasks artifacts ls: unexpected argument: extra",
		"workflow get DEV-1 extra":                "tasks workflow get: unexpected argument: extra",
		"workflow move DEV-1 x --to a --reason b": "tasks workflow move: unexpected argument: x",
		"artifacts show DEV-1":                    "tasks artifacts show: artifact name is required",
	} {
		_, err := parse(strings.Fields(argv))
		if err == nil || err.Error() != want {
			t.Errorf("parse(%q) error = %v; want %q", argv, err, want)
		}
	}
}

func TestTerminalHostileTextIsNeutralised(t *testing.T) {
	if got := preview("a\x1b[31mred\x1b[0m\x07\x00b"); strings.ContainsAny(got, "\x1b\x07\x00") || got != "a [31mred [0m b" {
		t.Fatalf("preview = %q", got)
	}
	withCaller(t, &recorder{err: &client.APIError{Code: "workflow_managed", Msg: "m",
		Details: map[string]any{"status": "s", "outcomes": []any{"ok", "bad\x1b[2Jname"}}}})
	var errOut strings.Builder
	Run(context.Background(), []string{"done", "DEV-1", "--revision", "1"}, agentEnv(), io.Discard, &errOut)
	if strings.Contains(errOut.String(), "\x1b") || !strings.Contains(errOut.String(), `"bad\x1b[2Jname"`) ||
		!strings.Contains(errOut.String(), "--outcome <name>") {
		t.Fatalf("hint = %q", &errOut)
	}
}

func TestAdvanceFailureExitsNonZero(t *testing.T) {
	withCaller(t, &recorder{err: &client.APIError{Code: "outcome_unknown", Msg: "no such outcome"}})
	var errOut strings.Builder
	if code := Run(context.Background(), []string{"advance", "DEV-1", "--outcome", "nope"}, agentEnv(), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "outcome_unknown") {
		t.Fatalf("code %d stderr %q", code, &errOut)
	}
}

const sampleView = `{"name":"development","version":"1.2.0","digest":"d1","status":"develop","category":"in_progress",
 "owner":"pool:developers","holder":"agent:dev-1",
 "outcomes":[{"on":"ready","to":"review","requires":["plan","summary"],"missing":["summary"],"checks":["checks/ci.sh"]},{"on":"abandon","to":"done"}],
 "artifacts":[{"id":2,"name":"plan","value":"line one\nline two","author":"agent:dev-1","created_at":"t"}],
 "visits":[],"last_request":{"id":7,"task_key":"DEV-1","outcome":"ready","actor":"agent:dev-1","state":"applied","created_at":"t"}}`

func TestWorkflowGetTextAndJSON(t *testing.T) {
	withCaller(t, &recorder{result: json.RawMessage(sampleView)})
	var out strings.Builder
	if code := Run(context.Background(), []string{"workflow", "get", "DEV-1"}, agentEnv(), &out, io.Discard); code != 0 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{
		"workflow: development@1.2.0", "status: develop", "category: in_progress", "waiting_on:", "owner: pool:developers", "holder: agent:dev-1",
		"ready -> review  requires: plan, summary  missing: summary  checks: checks/ci.sh", "abandon -> done",
		"plan  agent:dev-1  line one line two", "last_request: #7 ready applied by agent:dev-1",
		"next: ttasks advance DEV-1 --from develop --outcome <name>",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, &out)
		}
	}
	out.Reset()
	if code := Run(context.Background(), []string{"workflow", "get", "DEV-1", "--json"}, agentEnv(), &out, io.Discard); code != 0 || !strings.Contains(out.String(), `"digest":"d1"`) {
		t.Fatalf("json code %d: %s", code, &out)
	}
}

func TestArtifactPreviewIsOneLineCappedAt120Runes(t *testing.T) {
	long := strings.Repeat("é", 300) + "\nsecond"
	got := preview(long)
	if strings.Contains(got, "\n") || len([]rune(got)) != 120 || !strings.HasSuffix(got, "…") {
		t.Fatalf("preview = %q (%d runes)", got, len([]rune(got)))
	}
	if preview("a\n\nb  c") != "a b c" {
		t.Fatalf("short preview = %q", preview("a\n\nb  c"))
	}
}

func TestShowPrintsStatusCategoryWaitingOnAndPointsAtWorkflowGet(t *testing.T) {
	withCaller(t, &recorder{result: json.RawMessage(
		`{"task":{"key":"DEV-1","status":"approve","category":"wait_customer","waiting_on":"customer","workflow_digest":"d1"}}`)})
	var out strings.Builder
	if code := Run(context.Background(), []string{"show", "DEV-1"}, agentEnv(), &out, io.Discard); code != 0 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"status: approve", "category: wait_customer", "waiting_on: customer", "ttasks workflow get DEV-1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show lacks %q:\n%s", want, &out)
		}
	}
	withCaller(t, &recorder{result: json.RawMessage(`{"task":{"key":"DEV-2","status":"open","category":"open"}}`)})
	out.Reset()
	if code := Run(context.Background(), []string{"show", "DEV-2"}, agentEnv(), &out, io.Discard); code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out.String(), "status: open") || !strings.Contains(out.String(), "category: open") || strings.Contains(out.String(), "workflow get") {
		t.Errorf("flexible show:\n%s", &out)
	}
}

func TestWorkflowManagedRefusalHintsTheAdvanceCommand(t *testing.T) {
	managed := &client.APIError{Code: "workflow_managed", Msg: "task lifecycle is managed by its workflow",
		Details: map[string]any{"status": "develop", "outcomes": []any{"ready", "abandon"}}}
	for _, tt := range []struct {
		name string
		env  func(string) string
		argv []string
		key  string
	}{
		{"agent done", agentEnv(), []string{"done", "DEV-1", "--revision", "3"}, "DEV-1"},
		{"agent update", agentEnv(), []string{"update", "DEV-1", "--status", "done", "--revision", "3"}, "DEV-1"},
		{"agent ready claim", agentEnv(), []string{"ready", "--claim"}, "KEY"},
		{"operator done", operatorEnv(t), []string{"done", "DEV-1", "--revision", "3"}, "DEV-1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withCaller(t, &recorder{err: managed})
			var errOut strings.Builder
			if code := Run(context.Background(), tt.argv, tt.env, io.Discard, &errOut); code != 1 {
				t.Fatalf("code %d", code)
			}
			for _, want := range []string{"workflow_managed", `status "develop"`, `"ready", "abandon"`, "ttasks advance " + tt.key + " --from develop --outcome <name>"} {
				if !strings.Contains(errOut.String(), want) {
					t.Errorf("stderr lacks %q:\n%s", want, &errOut)
				}
			}
		})
	}
	// Other errors carry no hint.
	withCaller(t, &recorder{err: &client.APIError{Code: "forbidden", Msg: "no"}})
	var errOut strings.Builder
	Run(context.Background(), []string{"done", "DEV-1", "--revision", "3"}, agentEnv(), io.Discard, &errOut)
	if strings.Contains(errOut.String(), "hint") {
		t.Errorf("unexpected hint: %s", &errOut)
	}
}

func TestWorkflowCommandsAreDocumentedInHelp(t *testing.T) {
	for _, tt := range []struct {
		path  []string
		wants []string
	}{
		{[]string{"advance"}, []string{"--outcome", "--from", "--message", "KEY", "status_changed"}},
		{[]string{"artifacts", "set"}, []string{"--file", "stdin", "NAME"}},
		{[]string{"artifacts", "ls"}, []string{"KEY"}},
		{[]string{"artifacts", "show"}, []string{"NAME"}},
		{[]string{"workflow", "get"}, []string{"KEY"}},
		{[]string{"workflow", "move"}, []string{"--to", "--reason", "operator-only"}},
		{[]string{"cancel"}, []string{"operator-only"}},
		{[]string{"queue", "workflow", "set"}, []string{"operator-only", "--ref", "--revision"}},
		{[]string{"queue", "workflow", "get"}, []string{"operator-only"}},
		{[]string{"queue", "workflow", "clear"}, []string{"operator-only", "--revision"}},
		{[]string{"artifacts"}, []string{"set", "ls", "show"}},
		{[]string{"workflow"}, []string{"get", "move"}},
	} {
		var out strings.Builder
		if code := Run(context.Background(), append(append([]string{}, tt.path...), "--help"), mapEnv(), &out, io.Discard); code != 0 {
			t.Errorf("%v: code %d", tt.path, code)
			continue
		}
		for _, want := range tt.wants {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v help lacks %q", tt.path, want)
			}
		}
	}
	var out strings.Builder
	Run(context.Background(), []string{"--help-json"}, mapEnv(), &out, io.Discard)
	var tree map[string]any
	if err := json.Unmarshal([]byte(out.String()), &tree); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{
		{"advance"}, {"artifacts", "set"}, {"artifacts", "ls"}, {"artifacts", "show"}, {"workflow", "get"}, {"workflow", "move"}, {"cancel"},
		{"queue", "workflow", "set"}, {"queue", "workflow", "get"}, {"queue", "workflow", "clear"},
	} {
		node := tree
		for _, part := range path {
			next, ok := node[part].(map[string]any)
			if !ok {
				t.Fatalf("--help-json lacks %v", path)
			}
			node = next
		}
		if node["summary"] == nil || node["examples"] == nil {
			t.Errorf("--help-json %v lacks summary or examples: %#v", path, node)
		}
	}
}

func TestOperatorReadyIgnoresWorkflowTasks(t *testing.T) {
	withCaller(t, &scriptedRecorder{results: []json.RawMessage{json.RawMessage(
		`{"tasks":[{"key":"DEV-1","status":"develop","category":"open","workflow_digest":"d1"},` +
			`{"key":"DEV-2","status":"open","category":"open"},` +
			`{"key":"DEV-3","status":"approve","category":"wait_customer","workflow_digest":"d1"}]}`)}})
	var out strings.Builder
	if code := Run(context.Background(), []string{"ready", "--json"}, operatorEnv(t), &out, io.Discard); code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out.String(), "DEV-2") || strings.Contains(out.String(), "DEV-1") || strings.Contains(out.String(), "DEV-3") {
		t.Fatalf("ready = %s; want only the flexible open task", &out)
	}
}
