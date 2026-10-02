package taskcli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/version"
)

type call struct {
	method, route string
	body          any
}

type scriptedRecorder struct {
	calls   []call
	results []json.RawMessage
	err     error
}

func (r *scriptedRecorder) Call(method, route string, body any) (json.RawMessage, error) {
	if query, ok := body.(map[string]string); ok {
		copy := map[string]string{}
		for key, value := range query {
			copy[key] = value
		}
		body = copy
	}
	r.calls = append(r.calls, call{method, route, body})
	if r.err != nil {
		return nil, r.err
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result, nil
}

type recorder struct {
	calls  []call
	result json.RawMessage
	err    error
}

func (r *recorder) Call(method, route string, body any) (json.RawMessage, error) {
	r.calls = append(r.calls, call{method, route, body})
	return r.result, r.err
}

func mapEnv(values ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i < len(values); i += 2 {
		m[values[i]] = values[i+1]
	}
	return func(key string) string { return m[key] }
}

func operatorEnv(t *testing.T) func(string) string {
	t.Helper()
	return mapEnv("TARIBOY_BASE_DIR", t.TempDir(), "TARIBOY_RUNTIME_DIR", t.TempDir())
}

func TestParseCurrentTasksSurface(t *testing.T) {
	tests := []struct {
		name, action string
		argv         []string
		want         map[string]any
	}{
		{"mine", "mine", []string{"mine", "--queue=DEV"}, map[string]any{"queue": "DEV"}},
		{"ready", "ready", []string{"ready", "--claim"}, map[string]any{"claim": true}},
		{"show", "show", []string{"show", "DEV-1"}, map[string]any{"key": "DEV-1"}},
		{"create", "create", []string{"create", "--queue", "DEV", "--title", "new"}, map[string]any{"queue": "DEV", "title": "new"}},
		{"update", "update", []string{"update", "DEV-1", "--pull-request="}, map[string]any{"key": "DEV-1", "pull_request": ""}},
		{"assign", "assign", []string{"assign", "DEV-1", "worker"}, map[string]any{"key": "DEV-1", "assignee": "worker"}},
		{"comment", "comment", []string{"comment", "DEV-1", "hello"}, map[string]any{"key": "DEV-1", "body": "hello"}},
		{"legacy ask", "ask", []string{"ask", "DEV-1", "user:me", "why"}, map[string]any{"key": "DEV-1", "principal": "user:me", "body": "why"}},
		{"move", "move", []string{"move", "DEV-1", "--to-root"}, map[string]any{"key": "DEV-1", "parent_key": ""}},
		{"block", "block", []string{"block", "DEV-1", "--by", "DEV-2"}, map[string]any{"key": "DEV-1", "blocker_key": "DEV-2"}},
		{"relate", "relate", []string{"relate", "DEV-1", "DEV-2"}, map[string]any{"key": "DEV-1", "target_key": "DEV-2"}},
		{"done", "done", []string{"done", "DEV-1"}, map[string]any{"key": "DEV-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parse(tt.argv)
			if err != nil {
				t.Fatal(err)
			}
			if got.action != tt.action || !sameJSON(got.payload, tt.want) {
				t.Fatalf("parse(%q) = %#v, want action %q payload %#v", tt.argv, got, tt.action, tt.want)
			}
		})
	}
}

func TestRunUsageErrorsDoNotCall(t *testing.T) {
	old := newCaller
	defer func() { newCaller = old }()
	recorded := &recorder{}
	newCaller = func(string) Caller { return recorded }
	for _, argv := range [][]string{{"unknown"}, {"show"}, {"mine", "--unknown"}} {
		if code := Run(context.Background(), argv, mapEnv("TARIBOY_TOOLS_SOCKET", "/agent.sock"), io.Discard, io.Discard); code != 2 {
			t.Fatalf("Run(%q) = %d, want 2", argv, code)
		}
	}
	if len(recorded.calls) != 0 {
		t.Fatalf("calls = %#v, want none", recorded.calls)
	}
}

func TestParseRejectsUnreadArguments(t *testing.T) {
	for _, argv := range [][]string{
		{"create", "--queue", "DEV", "--title"},
		{"ready", "--claim", "stray"},
		{"comment", "DEV-1", "stray", "--body", "body"},
		{"ask", "DEV-1", "user:me", "why", "--question", "why"},
	} {
		if _, err := parse(argv); err == nil {
			t.Fatalf("parse(%q) succeeded, want usage error", argv)
		}
	}
}

func TestOperatorMutationRoutesUseNestedRevision(t *testing.T) {
	tests := []struct {
		argv                  []string
		wantMethod, wantRoute string
		wantBody              map[string]any
	}{
		{[]string{"update", "DEV-1", "--title", "title"}, "PATCH", "/api/tasks/DEV-1", map[string]any{"title": "title", "revision": int64(7)}},
		{[]string{"assign", "DEV-1", "worker"}, "PATCH", "/api/tasks/DEV-1", map[string]any{"assignee": "worker", "revision": int64(7)}},
		{[]string{"move", "DEV-1", "--to-root"}, "POST", "/api/tasks/DEV-1/move", map[string]any{"parent_key": "", "revision": int64(7)}},
		{[]string{"relate", "DEV-1", "DEV-2"}, "POST", "/api/tasks/DEV-1/relations", map[string]any{"target_key": "DEV-2", "type": "related", "revision": int64(7)}},
		{[]string{"done", "DEV-1"}, "POST", "/api/tasks/DEV-1/complete", map[string]any{"revision": int64(7)}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.argv, " "), func(t *testing.T) {
			old := newCaller
			defer func() { newCaller = old }()
			recorded := &scriptedRecorder{results: []json.RawMessage{json.RawMessage(`{"task":{"revision":7}}`), json.RawMessage(`{}`)}}
			newCaller = func(string) Caller { return recorded }
			if code := Run(context.Background(), tt.argv, operatorEnv(t), io.Discard, io.Discard); code != 0 {
				t.Fatalf("code = %d, want 0", code)
			}
			if len(recorded.calls) != 2 || recorded.calls[0].route != "/api/tasks/DEV-1" || recorded.calls[1].method != tt.wantMethod || recorded.calls[1].route != tt.wantRoute || !sameJSON(recorded.calls[1].body, tt.wantBody) {
				t.Fatalf("calls = %#v, want lookup then %s %s %#v", recorded.calls, tt.wantMethod, tt.wantRoute, tt.wantBody)
			}
		})
	}
}

func TestOperatorBlockUsesBlockerRevisionAndDirection(t *testing.T) {
	old := newCaller
	defer func() { newCaller = old }()
	recorded := &scriptedRecorder{results: []json.RawMessage{json.RawMessage(`{"task":{"revision":9}}`), json.RawMessage(`{}`)}}
	newCaller = func(string) Caller { return recorded }
	if code := Run(context.Background(), []string{"block", "DEV-1", "--by", "DEV-2"}, operatorEnv(t), io.Discard, io.Discard); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if len(recorded.calls) != 2 || recorded.calls[0].route != "/api/tasks/DEV-2" || recorded.calls[1].route != "/api/tasks/DEV-2/relations" || !sameJSON(recorded.calls[1].body, map[string]any{"target_key": "DEV-1", "type": "blocks", "revision": int64(9)}) {
		t.Fatalf("calls = %#v", recorded.calls)
	}
}

func TestOperatorAskNormalizesBareAgentPrincipal(t *testing.T) {
	old := newCaller
	defer func() { newCaller = old }()
	recorded := &scriptedRecorder{results: []json.RawMessage{json.RawMessage(`{}`)}}
	newCaller = func(string) Caller { return recorded }
	if code := Run(context.Background(), []string{"ask", "DEV-1", "worker", "please", "review"}, operatorEnv(t), io.Discard, io.Discard); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if len(recorded.calls) != 1 || recorded.calls[0].route != "/api/tasks/DEV-1/comments" || !sameJSON(recorded.calls[0].body, map[string]any{"body": "@agent:worker\n\nplease review", "idempotency_key": nil}) {
		t.Fatalf("calls = %#v", recorded.calls)
	}
}

func TestOperatorReadyFiltersAcrossPagesAndClaimRequiresAgent(t *testing.T) {
	old := newCaller
	defer func() { newCaller = old }()
	recorded := &scriptedRecorder{results: []json.RawMessage{
		json.RawMessage(`{"tasks":[{"key":"DEV-1","status":"open","assignee":"worker"},{"key":"DEV-2","status":"open","blocked":true}],"next_cursor":"DEV-3","sequence":1}`),
		json.RawMessage(`{"tasks":[{"key":"DEV-4","status":"open","revision":4}],"sequence":2}`),
	}}
	newCaller = func(string) Caller { return recorded }
	var out, errOut strings.Builder
	env := operatorEnv(t)
	if code := Run(context.Background(), []string{"ready", "--json"}, env, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != `[{"key":"DEV-4","status":"open","revision":4}]` {
		t.Fatalf("ready = %d stdout %q stderr %q", code, out.String(), errOut.String())
	}
	if len(recorded.calls) != 2 || !sameJSON(recorded.calls[0].body, map[string]string{"status": "open", "blocked": "false", "limit": "500"}) || !sameJSON(recorded.calls[1].body, map[string]string{"status": "open", "blocked": "false", "limit": "500", "after": "DEV-3"}) {
		t.Fatalf("ready calls = %#v", recorded.calls)
	}
	recorded.calls = nil
	if code := Run(context.Background(), []string{"ready", "--claim"}, env, io.Discard, &errOut); code != 2 || len(recorded.calls) != 0 || !strings.Contains(errOut.String(), "requires agent mode") {
		t.Fatalf("claim = %d calls %#v stderr %q", code, recorded.calls, errOut.String())
	}
}

func TestAgentAPIErrorsAndOperatorAdministrationJSON(t *testing.T) {
	old := newCaller
	defer func() { newCaller = old }()
	agent := &scriptedRecorder{err: &client.APIError{Code: "forbidden", Msg: "nope"}}
	newCaller = func(string) Caller { return agent }
	var errOut strings.Builder
	if code := Run(context.Background(), []string{"mine"}, mapEnv("TARIBOY_TOOLS_SOCKET", "/agent.sock"), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "error (forbidden): nope") {
		t.Fatalf("agent error = %d %q", code, errOut.String())
	}
	admin := &scriptedRecorder{results: []json.RawMessage{json.RawMessage(`{"queues":[]}`)}}
	newCaller = func(string) Caller { return admin }
	var out strings.Builder
	if code := Run(context.Background(), []string{"queue", "list", "--json"}, operatorEnv(t), &out, io.Discard); code != 0 || strings.TrimSpace(out.String()) != `{"queues":[]}` {
		t.Fatalf("admin json = %d %q", code, out.String())
	}
}

func TestRunJSONAndVersion(t *testing.T) {
	old := newCaller
	defer func() { newCaller = old }()
	recorded := &recorder{result: json.RawMessage(`{"ok":true}`)}
	newCaller = func(string) Caller { return recorded }
	var out strings.Builder
	if code := Run(context.Background(), []string{"--version"}, mapEnv(), &out, io.Discard); code != 0 || strings.TrimSpace(out.String()) != version.Version {
		t.Fatalf("version = %d %q, want 0 %q", code, out.String(), version.Version)
	}
	out.Reset()
	if code := Run(context.Background(), []string{"mine", "--json"}, mapEnv("TARIBOY_TOOLS_SOCKET", "/agent.sock"), &out, io.Discard); code != 0 || strings.TrimSpace(out.String()) != `{"ok":true}` {
		t.Fatalf("json = %d %q", code, out.String())
	}
}

func TestOperatorAdministrationArguments(t *testing.T) {
	tests := []struct {
		args          []string
		method, route string
		body          any
	}{
		{[]string{"queue", "get", "OPS"}, "GET", "/api/task-queues/OPS", map[string]string{}},
		{[]string{"queue", "update", "OPS", "--name", "Operations", "--revision", "2"}, "PATCH", "/api/task-queues/OPS", map[string]any{"name": "Operations", "revision": 2}},
		{[]string{"notifications", "read", "1"}, "POST", "/api/task-notifications/1/read", map[string]any{}},
		{[]string{"notifications", "dismiss", "1"}, "POST", "/api/task-notifications/1/dismiss", map[string]any{}},
		{[]string{"notifications", "list", "--include-dismissed"}, "GET", "/api/task-notifications", map[string]string{"include_dismissed": "true"}},
		{[]string{"queue", "pool", "get", "OPS", "reviewers"}, "GET", "/api/task-queues/OPS/pools/reviewers", map[string]string{}},
		{[]string{"queue", "pool", "set", "OPS", "reviewers", "--agents", "worker", "--revision", "0", "--idempotency-key", "pool-1"}, "PATCH", "/api/task-queues/OPS/pools/reviewers", map[string]any{"agents": "worker", "revision": 0, "idempotency_key": "pool-1"}},
		{[]string{"queue", "trigger", "create", "OPS", "--pattern", "external:incidents", "--action", "create_task"}, "POST", "/api/task-queues/OPS/workflow-triggers", map[string]any{"pattern": "external:incidents", "action": "create_task"}},
		{[]string{"queue", "trigger", "delete", "OPS", "4"}, "DELETE", "/api/task-queues/OPS/workflow-triggers/4", map[string]string{}},
		{[]string{"events", "OPS-1", "--after", "7", "--limit", "10"}, "GET", "/api/tasks/OPS-1/events", map[string]string{"after": "7", "limit": "10"}},
	}
	old := newCaller
	defer func() { newCaller = old }()
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			r := &recorder{result: json.RawMessage(`{}`)}
			newCaller = func(string) Caller { return r }
			var errOut strings.Builder
			if code := Run(context.Background(), tt.args, operatorEnv(t), io.Discard, &errOut); code != 0 {
				t.Fatalf("code = %d, stderr = %s", code, errOut.String())
			}
			if len(r.calls) != 1 || r.calls[0].method != tt.method || r.calls[0].route != tt.route || !sameJSON(r.calls[0].body, tt.body) {
				t.Fatalf("calls = %#v, want %s %s %#v", r.calls, tt.method, tt.route, tt.body)
			}
		})
	}
}

func TestHelpJSONCoversRunnableTaskRootsWithoutSocket(t *testing.T) {
	old := newCaller
	defer func() { newCaller = old }()
	newCaller = func(string) Caller { t.Fatal("help must not construct a caller"); return nil }
	var out strings.Builder
	if code := Run(context.Background(), []string{"--help-json"}, mapEnv(), &out, io.Discard); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	var tree map[string]any
	if err := json.Unmarshal([]byte(out.String()), &tree); err != nil {
		t.Fatalf("help = %q, tree = %#v, err = %v", out.String(), tree, err)
	}
	for _, root := range []string{"mine", "ready", "show", "assign", "ask", "queue", "events", "principals", "notifications"} {
		if tree[root] == nil {
			t.Fatalf("help tree missing runnable root %q: %#v", root, tree)
		}
	}
	for _, root := range []string{"list", "get", "work", "artifacts", "observe", "questions", "answer", "workflows", "workflow"} {
		if tree[root] != nil {
			t.Fatalf("help tree advertises inaccessible root %q: %#v", root, tree)
		}
	}
}

func TestAgentModeNeverFallsBack(t *testing.T) {
	old := newCaller
	defer func() { newCaller = old }()
	var sockets []string
	newCaller = func(socket string) Caller {
		sockets = append(sockets, socket)
		return &scriptedRecorder{err: io.EOF}
	}
	env := mapEnv("TARIBOY_TOOLS_SOCKET", "/missing/agent.sock", "TARIBOY_RUNTIME_DIR", t.TempDir())
	code := Run(context.Background(), []string{"mine"}, env, io.Discard, io.Discard)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if len(sockets) != 1 || sockets[0] != "/missing/agent.sock" {
		t.Fatalf("sockets = %q, want only agent socket", sockets)
	}
}

func TestWhitespaceSocketRemainsAgentMode(t *testing.T) {
	old := newCaller
	defer func() { newCaller = old }()
	var sockets []string
	newCaller = func(socket string) Caller {
		sockets = append(sockets, socket)
		return &recorder{err: io.EOF}
	}
	env := mapEnv("TARIBOY_TOOLS_SOCKET", " \t ", "TARIBOY_BASE_DIR", t.TempDir(), "TARIBOY_RUNTIME_DIR", t.TempDir())
	if code := Run(context.Background(), []string{"mine"}, env, io.Discard, io.Discard); code != 2 || len(sockets) != 1 || sockets[0] != " \t " {
		t.Fatalf("mine code = %d, sockets = %q; want only raw agent socket", code, sockets)
	}
	sockets = nil
	if code := Run(context.Background(), []string{"queue", "list"}, env, io.Discard, io.Discard); code != 2 || len(sockets) != 0 {
		t.Fatalf("admin code = %d, sockets = %q; want local refusal", code, sockets)
	}
}

func TestOnlyToolsSocketSelectsAgentMode(t *testing.T) {
	old := newCaller
	defer func() { newCaller = old }()
	var sockets []string
	newCaller = func(socket string) Caller {
		sockets = append(sockets, socket)
		return &recorder{result: json.RawMessage(`{}`)}
	}
	env := mapEnv("TARIBOY_BASE_DIR", t.TempDir(), "TARIBOY_RUNTIME_DIR", t.TempDir())
	if code := Run(context.Background(), []string{"mine"}, env, io.Discard, io.Discard); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if len(sockets) != 1 || !strings.HasSuffix(sockets[0], "tariboyd.sock") {
		t.Fatalf("sockets = %q, want operator socket", sockets)
	}
}

func sameJSON(got, want any) bool {
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	return string(g) == string(w)
}
