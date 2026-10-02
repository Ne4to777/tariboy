package agentapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/bus"
	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/store"
)

func decode(t *testing.T, body []byte) (bool, map[string]any) {
	t.Helper()
	var env struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if !env.OK {
		return false, env.Error
	}
	return true, env.Result
}

func TestPluginGatingReadsCurrentImageCapabilities(t *testing.T) {
	plugins := []string{}
	server := NewServer(Deps{
		CurrentPlugins: func() []string { return plugins },
		SetStatus:      func(string) (map[string]any, error) { return map[string]any{"ok": true}, nil },
	})
	handler := server.Handler()
	request := func() int {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/status/set", bytes.NewBufferString(`{"message":"working"}`)))
		return rr.Code
	}
	if got := request(); got != http.StatusNotFound {
		t.Fatalf("disabled status=%d", got)
	}
	plugins = []string{"status"}
	if got := request(); got != http.StatusOK {
		t.Fatalf("enabled status=%d", got)
	}
}

func TestExplicitCorePluginsGateTheirCompleteRouteSurface(t *testing.T) {
	plugins := []string{}
	doneCalls := 0
	server := NewServer(Deps{
		CurrentPlugins:   func() []string { return plugins },
		CurrentIteration: func() string { return "iter-1" },
		SetDone:          func(string, bool) error { doneCalls++; return nil },
	})
	handler := server.Handler()
	tests := []struct {
		plugin string
		method string
		path   string
	}{
		{plugin: "whoami", method: http.MethodGet, path: "/tools/whoami"},
		{plugin: "loop", method: http.MethodPost, path: "/tools/loop/done"},
		{plugin: "loop", method: http.MethodPost, path: "/tools/loop/complete"},
		{plugin: "loop", method: http.MethodPost, path: "/tools/loop/control"},
		{plugin: "messages", method: http.MethodPost, path: "/tools/message/send"},
		{plugin: "messages", method: http.MethodGet, path: "/tools/message/ls"},
		{plugin: "messages", method: http.MethodPost, path: "/tools/message/processed"},
		{plugin: "messages", method: http.MethodPost, path: "/tools/message/reply"},
		{plugin: "messages", method: http.MethodGet, path: "/tools/message/dlq"},
		{plugin: "messages", method: http.MethodPost, path: "/tools/message/dlq/requeue"},
		{plugin: "messages", method: http.MethodPost, path: "/tools/request"},
		{plugin: "messages", method: http.MethodPost, path: "/tools/channel/subscribe"},
		{plugin: "messages", method: http.MethodPost, path: "/tools/channel/unsubscribe"},
		{plugin: "messages", method: http.MethodGet, path: "/tools/channel/ls"},
		{plugin: "messages", method: http.MethodGet, path: "/tools/sources"},
	}

	for _, tc := range tests {
		t.Run(tc.plugin+" "+tc.path, func(t *testing.T) {
			beforeDone := doneCalls
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(`{}`)))
			ok, body := decode(t, rr.Body.Bytes())
			if rr.Code != http.StatusNotFound || ok || body["code"] != "plugin_disabled" {
				t.Fatalf("disabled route response=%d %v", rr.Code, body)
			}
			if doneCalls != beforeDone {
				t.Fatalf("disabled route reached SetDone: calls %d -> %d", beforeDone, doneCalls)
			}

			plugins = []string{tc.plugin}
			rr = httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(`{}`)))
			if rr.Code == http.StatusNotFound {
				t.Fatalf("enabled route remained hidden: %s", rr.Body.String())
			}
			if (tc.path == "/tools/loop/done" || tc.path == "/tools/loop/complete") && doneCalls != beforeDone+1 {
				t.Fatalf("enabled loop completion did not reach SetDone: calls %d -> %d", beforeDone, doneCalls)
			}
			plugins = nil
		})
	}
}

func newTestServer(t *testing.T, plugins []string) (*Server, *string, string) {
	t.Helper()
	ctxPath := filepath.Join(t.TempDir(), "CONTEXT.md")
	current := "iter-1"
	var doneSet string
	s := NewServer(Deps{
		Agent: "smoke", Cwd: "/w", ContextPath: ctxPath, Plugins: plugins,
		CurrentIteration: func() string { return current },
		SetDone:          func(id string, _ bool) error { doneSet = id; return nil },
		Status:           func() (map[string]any, error) { return map[string]any{"state": "running"}, nil },
	})
	return s, &doneSet, ctxPath
}

func TestTaskCurrentRouteIsRemoved(t *testing.T) {
	s := NewServer(Deps{Agent: "smoke", ContextPath: filepath.Join(t.TempDir(), "CONTEXT.md"),
		Plugins: []string{"whoami", "loop", "messages", "current-task"}})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/tools/task/current", bytes.NewBufferString(`{"id":"x"}`)))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	ok, e := decode(t, rr.Body.Bytes())
	if ok || e["code"] != "not_found" {
		t.Fatalf("error = %v", e)
	}
}

func TestGoalSetIsCapabilityGated(t *testing.T) {
	called := ""
	server := NewServer(Deps{
		Plugins: []string{"goal"},
		SetGoal: func(key string) (map[string]any, error) {
			called = key
			return map[string]any{"task_id": key, "epic_id": "T-1"}, nil
		},
	})
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/tools/goal/set", bytes.NewBufferString(`{"key":"T-2"}`)))
	ok, result := decode(t, rr.Body.Bytes())
	if rr.Code != http.StatusOK || !ok || called != "T-2" || result["epic_id"] != "T-1" {
		t.Fatalf("enabled response=%d %v called=%q", rr.Code, result, called)
	}

	server.d.Plugins = nil
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/tools/goal/set", bytes.NewBufferString(`{"key":"T-2"}`)))
	if rr.Code != http.StatusNotFound || called != "T-2" {
		t.Fatalf("disabled response=%d called=%q", rr.Code, called)
	}
}

func TestNativeTasksActionIsCapabilityGatedAndScrubsForgedIdentity(t *testing.T) {
	disabled := NewServer(Deps{
		Agent: "alice", Plugins: []string{"whoami", "loop", "messages"},
		TaskAction: func(string, map[string]any) (any, error) {
			t.Fatal("disabled tasks action must not run")
			return nil, nil
		},
	})
	request := httptest.NewRequest("POST", "/tools/tasks/create",
		bytes.NewBufferString(`{"queue":"TEST","title":"work","author":"user:forged"}`))
	recorder := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("disabled status = %d; want 404", recorder.Code)
	}

	var gotAction string
	var gotBody map[string]any
	enabled := NewServer(Deps{
		Agent: "alice", Plugins: []string{"whoami", "loop", "messages", "tasks"},
		TaskAction: func(action string, body map[string]any) (any, error) {
			gotAction, gotBody = action, body
			return map[string]any{"key": "TEST-1", "author": "agent:alice"}, nil
		},
	})
	request = httptest.NewRequest("POST", "/tools/tasks/create",
		bytes.NewBufferString(`{
			"queue":"TEST","title":"work",
			"author":"user:forged","actor":"user:forged","customer":"user:forged","iteration_id":"forged"
		}`))
	recorder = httptest.NewRecorder()
	enabled.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("enabled status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if gotAction != "create" || gotBody["queue"] != "TEST" {
		t.Fatalf("action/body = %q/%v", gotAction, gotBody)
	}
	for _, forbidden := range []string{"author", "actor", "customer", "principal", "iteration_id"} {
		if _, exists := gotBody[forbidden]; exists {
			t.Fatalf("forged identity %q reached task service: %v", forbidden, gotBody)
		}
	}
}

func TestLegacyIterationKeepsDirectMessaging(t *testing.T) {
	called := false
	s := NewServer(Deps{Agent: "alice", Plugins: []string{"messages"}, CurrentIteration: func() string { return "legacy" },
		Publish: func(message bus.Message) (bus.Message, error) { called = true; message.ID = "m1"; return message, nil },
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/tools/message/send", bytes.NewBufferString(`{"channel":"chat:ops","text":"x"}`)))
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("legacy send status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestWhoamiAndLoopDone(t *testing.T) {
	s, doneSet, _ := newTestServer(t, []string{"whoami", "loop", "messages"})
	h := s.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/whoami", nil))
	ok, res := decode(t, rr.Body.Bytes())
	if !ok || res["agent"] != "smoke" || res["cwd"] != "/w" || res["iteration"] != "iter-1" {
		t.Fatalf("whoami = %v", res)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/loop/done", nil))
	if ok, _ := decode(t, rr.Body.Bytes()); !ok || *doneSet != "iter-1" {
		t.Fatalf("loop done not recorded: %q", *doneSet)
	}

	*doneSet = ""
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/loop/complete", nil))
	if ok, _ := decode(t, rr.Body.Bytes()); !ok || *doneSet != "iter-1" {
		t.Fatalf("loop complete not recorded: %q", *doneSet)
	}
}

func TestContextGatedByPlugin(t *testing.T) {
	// context plugin absent -> 404 plugin_disabled
	s, _, _ := newTestServer(t, []string{"whoami", "loop", "messages"})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/tools/context/get", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	ok, e := decode(t, rr.Body.Bytes())
	if ok || e["code"] != "plugin_disabled" {
		t.Fatalf("error = %v", e)
	}
}

func TestContextSetGet(t *testing.T) {
	s, _, ctxPath := newTestServer(t, []string{"whoami", "loop", "messages", "context"})
	h := s.Handler()

	body, _ := json.Marshal(map[string]string{"text": "remember this"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/context/set", bytes.NewReader(body)))
	if ok, _ := decode(t, rr.Body.Bytes()); !ok {
		t.Fatalf("context set failed: %s", rr.Body.String())
	}
	if data, _ := os.ReadFile(ctxPath); string(data) != "remember this" {
		t.Fatalf("CONTEXT.md = %q", data)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/context/get", nil))
	ok, res := decode(t, rr.Body.Bytes())
	if !ok || res["text"] != "remember this" {
		t.Fatalf("context get = %v", res)
	}
}

func TestContextSetConcurrent(t *testing.T) {
	s, _, ctxPath := newTestServer(t, []string{"whoami", "loop", "messages", "context"})
	h := s.Handler()

	payloadA := strings.Repeat("A", 2048)
	payloadB := strings.Repeat("B", 2048)

	doSet := func(text string) {
		body, _ := json.Marshal(map[string]string{"text": text})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/context/set", bytes.NewReader(body)))
		if ok, _ := decode(t, rr.Body.Bytes()); !ok {
			t.Errorf("context set failed: %s", rr.Body.String())
		}
	}
	doGet := func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/context/get", nil))
		ok, res := decode(t, rr.Body.Bytes())
		if !ok {
			t.Errorf("context get failed: %s", rr.Body.String())
			return
		}
		text, _ := res["text"].(string)
		if text != "" && text != payloadA && text != payloadB {
			t.Errorf("context get returned corrupted/partial content: len=%d", len(text))
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				doSet(payloadA)
			case 1:
				doSet(payloadB)
			case 2:
				doGet()
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(ctxPath)
	if err != nil {
		t.Fatalf("read CONTEXT.md: %v", err)
	}
	got := string(data)
	if got != payloadA && got != payloadB {
		t.Fatalf("final CONTEXT.md is neither full payload (corrupted/truncated): len=%d", len(got))
	}
}

func TestStatusRoute(t *testing.T) {
	s, _, _ := newTestServer(t, []string{"whoami", "loop", "messages", "status"})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/tools/status", nil))
	ok, res := decode(t, rr.Body.Bytes())
	if !ok || res["state"] != "running" {
		t.Fatalf("status = %v", res)
	}
}

func TestStatusSetRoute(t *testing.T) {
	var gotMsg string
	newSrv := func(plugins []string, withHook bool) *Server {
		d := Deps{
			Agent: "smoke", Plugins: plugins,
			CurrentIteration: func() string { return "iter-1" },
			SetDone:          func(string, bool) error { return nil },
			Status:           func() (map[string]any, error) { return map[string]any{"state": "running"}, nil },
		}
		if withHook {
			d.SetStatus = func(msg string) (map[string]any, error) {
				gotMsg = msg
				return map[string]any{"message": msg, "updated": "2026-07-09T10:00:00Z"}, nil
			}
		}
		return NewServer(d)
	}
	post := func(s *Server) (bool, map[string]any) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/tools/status/set", strings.NewReader(`{"message":"cloning repo"}`))
		s.Handler().ServeHTTP(rr, req)
		return decode(t, rr.Body.Bytes())
	}

	// happy path: status plugin + hook present.
	ok, res := post(newSrv([]string{"whoami", "loop", "messages", "status"}, true))
	if !ok || gotMsg != "cloning repo" || res["message"] != "cloning repo" {
		t.Fatalf("status set = %v gotMsg=%q", res, gotMsg)
	}
	// gated: status plugin absent -> plugin_disabled.
	if ok, e := post(newSrv([]string{"whoami", "loop", "messages"}, true)); ok || e["code"] != "plugin_disabled" {
		t.Fatalf("expected plugin_disabled, got ok=%v e=%v", ok, e)
	}
	// unavailable: hook nil -> unavailable.
	if ok, e := post(newSrv([]string{"whoami", "loop", "messages", "status"}, false)); ok || e["code"] != "unavailable" {
		t.Fatalf("expected unavailable, got ok=%v e=%v", ok, e)
	}
}

func TestMessageSendAndChannels(t *testing.T) {
	var published bus.Message
	subs := []bus.Subscription{}
	s := NewServer(Deps{
		Agent: "smoke", Cwd: "/w", Plugins: []string{"whoami", "loop", "messages"},
		CurrentIteration: func() string { return "iter-1" },
		SetDone:          func(string, bool) error { return nil },
		Publish: func(m bus.Message) (bus.Message, error) {
			m.ID = "m1"
			published = m
			return m, nil
		},
		Subscribe: func(channel string, matcher bus.Matcher, tf []string) (bus.Subscription, error) {
			sub := bus.Subscription{ID: "sub-1", Agent: "smoke", Channel: channel, Matcher: matcher, TypeFilter: tf}
			subs = append(subs, sub)
			return sub, nil
		},
		ListSubscriptions: func() ([]bus.Subscription, error) { return subs, nil },
		Channels:          func() ([]bus.Channel, error) { return []bus.Channel{{Name: "chat:room", Kind: "chat"}}, nil },
	})
	h := s.Handler()

	// send
	body, _ := json.Marshal(map[string]any{"channel": "chat:room", "type": "note", "text": "hi",
		"subject": map[string]any{"env": "prod"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/message/send", bytes.NewReader(body)))
	if ok, _ := decode(t, rr.Body.Bytes()); !ok {
		t.Fatalf("send failed: %s", rr.Body.String())
	}
	if published.Channel != "chat:room" || published.Text != "hi" ||
		published.ProducedByAgent != "smoke" || published.ProducedInIteration != "iter-1" ||
		published.Source != "agent:smoke" {
		t.Fatalf("attribution/publish wrong: %+v", published)
	}

	// subscribe
	sbody, _ := json.Marshal(map[string]any{"channel": "chat:room", "matcher": map[string]string{"type": "note"}, "type": "note.*"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/channel/subscribe", bytes.NewReader(sbody)))
	if ok, res := decode(t, rr.Body.Bytes()); !ok || res["id"] != "sub-1" {
		t.Fatalf("subscribe = %v", res)
	}

	// channel ls
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/channel/ls", nil))
	if ok, res := decode(t, rr.Body.Bytes()); !ok || res["count"].(float64) != 1 {
		t.Fatalf("channel ls = %v", res)
	}

	// sources
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/sources", nil))
	if ok, res := decode(t, rr.Body.Bytes()); !ok || res["count"].(float64) != 1 {
		t.Fatalf("sources = %v", res)
	}
}

// TestSourcesMergesProviderAnnotations verifies the sources handler merges the
// live channel list with provider declarations (spec §6.1): an existing channel
// row that is also provided gets annotated in place, and a provider channel with
// no row yet is listed anyway. Rows come back sorted by name.
func TestSourcesMergesProviderAnnotations(t *testing.T) {
	s := NewServer(Deps{
		Agent: "smoke", Cwd: "/w", Plugins: []string{"messages"},
		CurrentIteration: func() string { return "iter-1" },
		SetDone:          func(string, bool) error { return nil },
		Channels: func() ([]bus.Channel, error) {
			// issue-provider:query already has a row; issue-provider:ticket does not yet.
			return []bus.Channel{
				{Name: "chat:room", Kind: "chat"},
				{Name: "issue-provider:query", Kind: "chat"},
			}, nil
		},
		ProvidedChannels: func() ([]ProvidedChannel, error) {
			return []ProvidedChannel{
				{Channel: "issue-provider:query", Provider: "issue-provider", Params: []string{"query"}, Help: "run a query"},
				{Channel: "issue-provider:ticket", Provider: "issue-provider", Params: []string{"ticket"}, Help: "watch a ticket"},
			}, nil
		},
	})
	h := s.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/sources", nil))
	ok, res := decode(t, rr.Body.Bytes())
	if !ok {
		t.Fatalf("sources failed: %s", rr.Body.String())
	}
	if res["count"].(float64) != 3 {
		t.Fatalf("count = %v, want 3 (chat:room + 2 issue-provider channels)", res["count"])
	}
	rows := res["channels"].([]any)
	byName := map[string]map[string]any{}
	var names []string
	for _, r := range rows {
		row := r.(map[string]any)
		name := row["name"].(string)
		byName[name] = row
		names = append(names, name)
	}
	// Sorted: chat:room, issue-provider:query, issue-provider:ticket.
	if len(names) != 3 || names[0] != "chat:room" || names[1] != "issue-provider:query" || names[2] != "issue-provider:ticket" {
		t.Fatalf("names not sorted as expected: %v", names)
	}
	// chat:room is a plain channel — no provider annotation.
	if _, has := byName["chat:room"]["provider"]; has {
		t.Fatalf("chat:room should not be annotated: %v", byName["chat:room"])
	}
	// issue-provider:query existing row annotated in place.
	q := byName["issue-provider:query"]
	if q["provider"] != "issue-provider" || q["help"] != "run a query" {
		t.Fatalf("issue-provider:query not annotated: %v", q)
	}
	if params, _ := q["params"].([]any); len(params) != 1 || params[0] != "query" {
		t.Fatalf("issue-provider:query params = %v", q["params"])
	}
	// issue-provider:ticket listed even without a channel row.
	tk := byName["issue-provider:ticket"]
	if tk["provider"] != "issue-provider" || tk["kind"] == nil {
		t.Fatalf("issue-provider:ticket not listed/annotated: %v", tk)
	}
}

// TestSourcesSortedWithoutProvider verifies channel rows are sorted by name even
// when the ProvidedChannels dep is not wired (review finding #5): the sort used
// to live inside the `ProvidedChannels != nil` branch, so ordering was whatever
// the bus returned when no provider was present.
func TestSourcesSortedWithoutProvider(t *testing.T) {
	s := NewServer(Deps{
		Agent: "smoke", Cwd: "/w", Plugins: []string{"messages"},
		CurrentIteration: func() string { return "iter-1" },
		SetDone:          func(string, bool) error { return nil },
		Channels: func() ([]bus.Channel, error) {
			// Deliberately unsorted; no ProvidedChannels dep.
			return []bus.Channel{
				{Name: "zeta:room", Kind: "chat"},
				{Name: "alpha:room", Kind: "chat"},
				{Name: "mid:room", Kind: "chat"},
			}, nil
		},
	})
	h := s.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/sources", nil))
	ok, res := decode(t, rr.Body.Bytes())
	if !ok {
		t.Fatalf("sources failed: %s", rr.Body.String())
	}
	rows := res["channels"].([]any)
	var names []string
	for _, r := range rows {
		names = append(names, r.(map[string]any)["name"].(string))
	}
	if len(names) != 3 || names[0] != "alpha:room" || names[1] != "mid:room" || names[2] != "zeta:room" {
		t.Fatalf("names not sorted without provider dep: %v", names)
	}
}

// TestChannelUnsubscribeCrossAgentRejected mirrors exactly how Manager wires
// agentapi.Deps.Unsubscribe (internal/loop/manager.go): the calling agent is
// bound into the closure from Deps.Agent, never taken from the request body.
// This guards against agent B unsubscribing agent A's subscription, even
// though subscription ids embed the owner's name (sub-<agent>-<ts>) and are
// therefore guessable.
func TestChannelUnsubscribeCrossAgentRejected(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	bs := bus.New(s, time.Now)

	newAgentServer := func(agentName string) *Server {
		return NewServer(Deps{
			Agent: agentName, Cwd: "/w", Plugins: []string{"whoami", "loop", "messages"},
			CurrentIteration: func() string { return "iter-1" },
			SetDone:          func(string, bool) error { return nil },
			Subscribe: func(channel string, matcher bus.Matcher, tf []string) (bus.Subscription, error) {
				return bs.Subscribe(agentName, channel, matcher, tf)
			},
			// Bound exactly like Manager's closure: agentName comes from the
			// server's own Deps.Agent, never from the request body.
			Unsubscribe:       func(id string) error { return bs.Unsubscribe(agentName, id) },
			ListSubscriptions: func() ([]bus.Subscription, error) { return bs.ListSubscriptions(agentName) },
		})
	}
	aliceSrv := newAgentServer("alice")
	bobSrv := newAgentServer("bob")

	// alice subscribes to her own inbox over her route.
	sbody, _ := json.Marshal(map[string]any{"channel": bus.InboxChannel("alice")})
	rr := httptest.NewRecorder()
	aliceSrv.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/tools/channel/subscribe", bytes.NewReader(sbody)))
	ok, res := decode(t, rr.Body.Bytes())
	if !ok {
		t.Fatalf("alice subscribe failed: %s", rr.Body.String())
	}
	subID, _ := res["id"].(string)
	if subID == "" {
		t.Fatalf("alice subscribe: no id in %v", res)
	}

	// bob calls unsubscribe on HIS OWN route/server with alice's (guessed) id.
	ubody, _ := json.Marshal(map[string]any{"id": subID})
	rr = httptest.NewRecorder()
	bobSrv.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/tools/channel/unsubscribe", bytes.NewReader(ubody)))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-agent unsubscribe status = %d, want 404", rr.Code)
	}
	if ok, _ := decode(t, rr.Body.Bytes()); ok {
		t.Fatal("bob was able to remove alice's subscription")
	}

	// alice's subscription must still be listed (via alice's own route).
	rr = httptest.NewRecorder()
	aliceSrv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/tools/channel/ls", nil))
	ok, res = decode(t, rr.Body.Bytes())
	if !ok || res["count"].(float64) != 1 {
		t.Fatalf("alice's subscription missing after bob's failed unsubscribe: %v", res)
	}

	// alice unsubscribing her own id, over her own route, succeeds.
	rr = httptest.NewRecorder()
	aliceSrv.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/tools/channel/unsubscribe", bytes.NewReader(ubody)))
	if ok, _ := decode(t, rr.Body.Bytes()); !ok {
		t.Fatalf("alice could not unsubscribe her own subscription: %s", rr.Body.String())
	}
}

func TestGroupToolsInfoStatusMessagingLoopAndObserve(t *testing.T) {
	var published bus.Message
	var loopStarted, loopStopped string
	s := NewServer(Deps{
		Agent: "manager", Cwd: "/w", Plugins: []string{"whoami", "loop", "messages", "status"},
		CurrentIteration: func() string { return "iter-manager" },
		SetDone:          func(string, bool) error { return nil },
		Status:           func() (map[string]any, error) { return map[string]any{"state": "running"}, nil },
		GroupInfo: func() (map[string]any, error) {
			return map[string]any{
				"agent": "manager", "group": "dev-team", "lead": "manager", "role": "lead",
				"members": []string{"manager", "worker"}, "broadcast": "group:dev-team:broadcast",
				"inbox": "group:dev-team:inbox", "shared_dir": "/groups/dev-team/shared",
			}, nil
		},
		GroupStatus: func(member string) (map[string]any, error) {
			rows := []map[string]any{
				{"name": "manager", "role": "lead", "state": "running", "loop_enabled": true, "status_message": "coordinating"},
				{"name": "worker", "role": "member", "state": "idle", "loop_enabled": false, "status_message": "waiting"},
			}
			if member != "" {
				if member != "worker" {
					return nil, errors.New("agent is not in your group")
				}
				return map[string]any{"member": rows[1]}, nil
			}
			return map[string]any{"members": rows, "count": len(rows)}, nil
		},
		GroupSend: func(member, typ, text, deadline string) (map[string]any, error) {
			published = bus.Message{ID: "m1", Channel: bus.InboxChannel(member), Type: typ, Text: text,
				Data: map[string]any{"deadline": deadline}}
			return map[string]any{"sent": true, "id": "m1", "target": member}, nil
		},
		GroupObserve: func(member string, tail int) (map[string]any, error) {
			return map[string]any{"events": []map[string]any{
				{"seq": float64(2), "kind": "status", "source": "agent", "data": `{"message":"done"}`},
			}, "count": 1, "tail": tail}, nil
		},
		GroupLoop: func(member, action string) (map[string]any, error) {
			if action == "start" {
				loopStarted = member
			} else {
				loopStopped = member
			}
			return map[string]any{"member": member, "action": action}, nil
		},
		LoopControl: func(action string) (map[string]any, error) {
			return map[string]any{"agent": "manager", "action": action}, nil
		},
	})
	h := s.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/group/info", nil))
	ok, res := decode(t, rr.Body.Bytes())
	if !ok || res["group"] != "dev-team" || res["role"] != "lead" {
		t.Fatalf("group info = %v", res)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/group/status", nil))
	ok, res = decode(t, rr.Body.Bytes())
	if !ok || res["count"].(float64) != 2 {
		t.Fatalf("group status = %v", res)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/group/status/worker", nil))
	ok, res = decode(t, rr.Body.Bytes())
	if !ok || res["member"].(map[string]any)["name"] != "worker" {
		t.Fatalf("group status worker = %v", res)
	}

	body, _ := json.Marshal(map[string]any{"member": "worker", "text": "please check", "deadline": "5m"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/group/request", bytes.NewReader(body)))
	ok, res = decode(t, rr.Body.Bytes())
	if !ok || !res["sent"].(bool) || published.Channel != "agent:worker:inbox" ||
		published.Type != "group.request" || published.Text != "please check" || published.Data["deadline"] != "5m" {
		t.Fatalf("group request res=%v published=%+v", res, published)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/tools/group/observe/worker?tail=7", nil))
	ok, res = decode(t, rr.Body.Bytes())
	if !ok || res["tail"].(float64) != 7 || res["count"].(float64) != 1 {
		t.Fatalf("group observe = %v", res)
	}

	body, _ = json.Marshal(map[string]any{"member": "worker", "action": "start"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/group/loop", bytes.NewReader(body)))
	if ok, _ := decode(t, rr.Body.Bytes()); !ok || loopStarted != "worker" {
		t.Fatalf("group loop start failed: %s started=%q", rr.Body.String(), loopStarted)
	}
	body, _ = json.Marshal(map[string]any{"member": "worker", "action": "stop"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/group/loop", bytes.NewReader(body)))
	if ok, _ := decode(t, rr.Body.Bytes()); !ok || loopStopped != "worker" {
		t.Fatalf("group loop stop failed: %s stopped=%q", rr.Body.String(), loopStopped)
	}

	body, _ = json.Marshal(map[string]any{"action": "start"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/loop/control", bytes.NewReader(body)))
	ok, res = decode(t, rr.Body.Bytes())
	if !ok || res["action"] != "start" {
		t.Fatalf("loop control = %v", res)
	}
}

func TestGroupToolsUnavailable(t *testing.T) {
	s := NewServer(Deps{
		Agent: "solo", Plugins: []string{"whoami", "loop", "messages"},
		CurrentIteration: func() string { return "iter-1" },
		SetDone:          func(string, bool) error { return nil },
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/tools/group/info", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	ok, e := decode(t, rr.Body.Bytes())
	if ok || !strings.Contains(e["message"].(string), "group tools are not available") {
		t.Fatalf("error = %v", e)
	}
}

func TestScheduleAndScriptGated(t *testing.T) {
	// schedule/script absent from plugins -> 404 plugin_disabled.
	s := NewServer(Deps{Agent: "smoke", Plugins: []string{"whoami", "loop", "messages"},
		CurrentIteration: func() string { return "i" }, SetDone: func(string, bool) error { return nil }})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/tools/schedule/ls", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("schedule ls status = %d, want 404", rr.Code)
	}
}

func TestScheduleAddAndScriptRun(t *testing.T) {
	var ran string
	s := NewServer(Deps{
		Agent: "smoke", Plugins: []string{"whoami", "loop", "messages", "schedule", "scripts"},
		CurrentIteration: func() string { return "i" }, SetDone: func(string, bool) error { return nil },
		AddSchedule: func(kind, spec, channel, tpl string) (map[string]any, error) {
			return map[string]any{"id": "sch-1", "kind": kind, "channel": channel}, nil
		},
		ListSchedules: func() ([]map[string]any, error) { return []map[string]any{{"id": "sch-1"}}, nil },
		RunScriptOnce: func(in script.CreateOnce) (script.Definition, script.Run, error) {
			ran = in.Name
			return script.Definition{ID: "scr-1", Name: in.Name, Mode: script.ModeOnce, State: script.StateActive}, script.Run{ID: "srun-1", ScriptID: "scr-1", Status: script.RunPending}, nil
		},
		ListScripts: func() ([]script.Definition, error) { return []script.Definition{{ID: "scr-0", Name: "greet"}}, nil },
	})
	h := s.Handler()

	body, _ := json.Marshal(map[string]any{"kind": "cron", "spec": "*/5 * * * *"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/schedule/add", bytes.NewReader(body)))
	if ok, res := decode(t, rr.Body.Bytes()); !ok || res["id"] != "sch-1" {
		t.Fatalf("schedule add = %v", res)
	}

	rbody, _ := json.Marshal(map[string]any{"name": "greet", "description": "greet", "command": "echo hi"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/script/run", bytes.NewReader(rbody)))
	if ok, _ := decode(t, rr.Body.Bytes()); !ok || ran != "greet" {
		t.Fatalf("script run not invoked: ran=%q", ran)
	}
}

func TestScriptToolsCreateAndCancel(t *testing.T) {
	var created script.CreateOnce
	var cancelled string
	s := NewServer(Deps{
		Agent: "smoke", Plugins: []string{"scripts"},
		RunScriptOnce: func(in script.CreateOnce) (script.Definition, script.Run, error) {
			created = in
			return script.Definition{ID: "scr-1", Agent: "smoke", Name: in.Name, Description: in.Description, Command: in.Command, Mode: script.ModeOnce, State: script.StateActive}, script.Run{ID: "srun-1", ScriptID: "scr-1", Agent: "smoke", Status: script.RunPending}, nil
		},
		CancelScriptTarget: func(id string) error { cancelled = id; return nil },
	})
	h := s.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/script/run", strings.NewReader(`{"name":"ci","description":"check","command":"true"}`)))
	ok, res := decode(t, rr.Body.Bytes())
	if !ok || rr.Code != http.StatusOK || res["script"].(map[string]any)["id"] != "scr-1" || created.Command != "true" {
		t.Fatalf("add status=%d result=%v create=%#v", rr.Code, res, created)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/tools/script/cancel", strings.NewReader(`{"id":"scr-1"}`)))
	if ok, _ := decode(t, rr.Body.Bytes()); !ok || rr.Code != http.StatusOK || cancelled != "scr-1" {
		t.Fatalf("cancel status=%d id=%q", rr.Code, cancelled)
	}
}

func TestScriptToolsRejectActiveRemoval(t *testing.T) {
	s := NewServer(Deps{
		Agent: "smoke", Plugins: []string{"scripts"},
		RemoveScript: func(string) error { return script.ErrActive },
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/tools/script/rm", strings.NewReader(`{"id":"scr-1"}`)))
	if ok, res := decode(t, rr.Body.Bytes()); ok || rr.Code != http.StatusConflict || res["code"] != "script_active" {
		t.Fatalf("remove status=%d ok=%v result=%v", rr.Code, ok, res)
	}
}

func TestImageBuildGatedAndDispatched(t *testing.T) {
	var gotTag, gotPath string
	deps := Deps{
		Agent: "creator", Cwd: "/w",
		Plugins: []string{"whoami", "loop", "messages", "image-creator"},
		BuildImage: func(name, tag, path string) (map[string]any, error) {
			gotTag, gotPath = name+":"+tag, path
			return map[string]any{"name": "authored", "tag": "latest", "digest": "deadbeef", "layers": 3}, nil
		},
	}
	s := NewServer(deps)

	// Positive: an image-creator agent can build.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tools/image/build",
		strings.NewReader(`{"name":"authored","path":"authored"}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("build status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// An empty tag reaches the builder, which then publishes the source
	// image_version plus latest, exactly as the operator CLI does.
	if gotTag != "authored:" || gotPath != "authored" {
		t.Fatalf("BuildImage called with (%q,%q)", gotTag, gotPath)
	}
	if !strings.Contains(rec.Body.String(), "deadbeef") {
		t.Fatalf("digest not returned: %s", rec.Body.String())
	}

	// Negative (privilege boundary): a non-creator agent is refused.
	deps2 := Deps{
		Agent: "plain", Cwd: "/w",
		Plugins:    []string{"whoami", "loop", "messages"},
		BuildImage: func(name, tag, path string) (map[string]any, error) { t.Fatal("must not be called"); return nil, nil },
	}
	s2 := NewServer(deps2)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/tools/image/build",
		strings.NewReader(`{"name":"evil","path":"x"}`))
	s2.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != 404 {
		t.Fatalf("non-creator build status = %d, want 404 (plugin_disabled)", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "plugin_disabled") {
		t.Fatalf("expected plugin_disabled, got %s", rec2.Body.String())
	}
}

// newBusServer wires a bus-backed agentapi Server for one agent exactly as
// Manager does (agent name bound into the closures, never from the request
// body). It returns the server, the live bus, and the agent name so tests can
// drive the Phase P messages surface end-to-end against real storage.
func newBusServer(t *testing.T, agent string) (*Server, *bus.Bus) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p4.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bs := bus.New(st, time.Now)
	s := NewServer(Deps{
		Agent: agent, Cwd: "/w", Plugins: []string{"whoami", "loop", "messages"},
		CurrentIteration: func() string { return "iter-1" },
		SetDone:          func(string, bool) error { return nil },
		Publish:          func(m bus.Message) (bus.Message, error) { return bs.Publish(m) },
		Subscribe: func(channel string, matcher bus.Matcher, tf []string) (bus.Subscription, error) {
			return bs.Subscribe(agent, channel, matcher, tf)
		},
		Unsubscribe:       func(id string) error { return bs.Unsubscribe(agent, id) },
		ListSubscriptions: func() ([]bus.Subscription, error) { return bs.ListSubscriptions(agent) },
		Channels:          func() ([]bus.Channel, error) { return bs.Channels() },
		Inbox: func(status string, limit int, before string) ([]bus.InboxItem, error) {
			return bs.Inbox(agent, status, limit, before)
		},
		MarkProcessed: func(msgID, result string) (bus.InboxItem, error) {
			return bs.MarkProcessed(agent, msgID, result)
		},
		Reply: func(msgID, text string, data map[string]any, typ string) (bus.Message, error) {
			return bs.Reply(agent, msgID, text, data, typ)
		},
		Request: func(channel, text, deadline string) (bus.Message, error) {
			return bs.Request(agent, channel, text, deadline)
		},
		Requeue: func(msgID string) error { return bs.Requeue(agent, msgID) },
		SubscribeParams: func(channel string, matcher bus.Matcher, tf []string, params map[string]any) (bus.Subscription, error) {
			return bs.SubscribeParams(agent, channel, matcher, tf, params)
		},
		UnsubscribeChannel: func(channel string) (int, error) { return bs.UnsubscribeChannel(agent, channel) },
	})
	return s, bs
}

// call is a small helper: run one request against the handler and decode.
func call(t *testing.T, s *Server, method, route, body string) (bool, map[string]any, int) {
	t.Helper()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(method, route, rdr))
	ok, res := decode(t, rr.Body.Bytes())
	return ok, res, rr.Code
}

// deliverToInbox subscribes the agent to its own inbox and publishes one message
// there, so a real delivery row exists for the Phase P lifecycle to act on.
func deliverToInbox(t *testing.T, s *Server, bs *bus.Bus, agent, text string) string {
	t.Helper()
	if _, err := bs.Subscribe(agent, bus.InboxChannel(agent), nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := bs.Publish(bus.Message{Channel: bus.InboxChannel(agent), Type: "note", Text: text, Source: "agent:other"})
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func TestMessageLsAndProcessed(t *testing.T) {
	s, bs := newBusServer(t, "alice")
	id := deliverToInbox(t, s, bs, "alice", "hello")

	// Pending inbox shows the message.
	ok, res, _ := call(t, s, "GET", "/tools/message/ls", "")
	if !ok || res["count"].(float64) != 1 {
		t.Fatalf("ls pending = %v", res)
	}
	msgs := res["messages"].([]any)
	if msgs[0].(map[string]any)["id"] != id {
		t.Fatalf("ls id = %v, want %s", msgs[0], id)
	}

	// Empty result is rejected with a clear error.
	ok, e, code := call(t, s, "POST", "/tools/message/processed", `{"id":"`+id+`","result":"  "}`)
	if ok || code != http.StatusBadRequest || e["code"] != "missing_result" {
		t.Fatalf("empty result not rejected: code=%d err=%v", code, e)
	}

	// Missing id.
	ok, e, code = call(t, s, "POST", "/tools/message/processed", `{"result":"done"}`)
	if ok || code != http.StatusBadRequest || e["code"] != "missing_id" {
		t.Fatalf("missing id not rejected: code=%d err=%v", code, e)
	}

	// Unknown id → 404.
	ok, e, code = call(t, s, "POST", "/tools/message/processed", `{"id":"nope","result":"done"}`)
	if ok || code != http.StatusNotFound {
		t.Fatalf("unknown id: code=%d err=%v", code, e)
	}

	// Process it with a real result.
	ok, res, _ = call(t, s, "POST", "/tools/message/processed", `{"id":"`+id+`","result":"read it"}`)
	if !ok || res["processed"] != true || res["result"] != "read it" {
		t.Fatalf("processed = %v", res)
	}

	// Pending inbox is now empty; --all still shows the (processed) message.
	if _, res, _ := call(t, s, "GET", "/tools/message/ls", ""); res["count"].(float64) != 0 {
		t.Fatalf("pending should be empty after processed: %v", res)
	}
	if _, res, _ := call(t, s, "GET", "/tools/message/ls?all=true", ""); res["count"].(float64) != 1 {
		t.Fatalf("--all should still show processed message: %v", res)
	}
}

func TestMessageReplyAutoProcesses(t *testing.T) {
	s, bs := newBusServer(t, "alice")
	id := deliverToInbox(t, s, bs, "alice", "please answer")

	ok, res, _ := call(t, s, "POST", "/tools/message/reply", `{"id":"`+id+`","text":"here you go"}`)
	if !ok || res["replied"] != true || res["in_reply_to"] != id {
		t.Fatalf("reply = %v", res)
	}
	// The original is auto-processed (drains from pending), the reply lands on the
	// source agent's inbox (not alice's), so alice's pending is empty.
	if _, res, _ := call(t, s, "GET", "/tools/message/ls", ""); res["count"].(float64) != 0 {
		t.Fatalf("reply should auto-process original: pending=%v", res)
	}
}

// TestMessageReplyToChatChannelNoSelfDelivery is the end-to-end proof for the
// dev-t-gmb.1 fix against the exact customer scenario, one layer above the bus
// unit test: it drives the real Messages skill reply path an agent runs during
// an iteration (POST /tools/message/reply on the per-agent agentapi server,
// wired to a live bus exactly as loop/manager.go wires it).
//
// Scenario: alice is subscribed to a chat channel; a plugin delivers an inbound
// chat message onto that channel; alice replies. The reply is routed back to the
// same chat channel (replyTarget: plugin source, no reply_to). A co-subscribed
// sink must still receive the reply so it can forward it out — but the reply must
// NOT re-enter alice's own queue.
//
// Before the fix this test FAILS: with self-delivery unsuppressed the reply
// lands a fresh pending delivery on alice's own chat-channel subscription, so her
// pending count is 1 (the echoed reply) instead of 0. Confirmed both directions:
// passes on the fixed bus, fails when the author-skip guard in Bus.Publish is
// removed.
func TestMessageReplyToChatChannelNoSelfDelivery(t *testing.T) {
	s, bs := newBusServer(t, "alice")
	chat := bus.ChatChannel("room")

	// alice (the replying agent) and sink (a plugin sink that forwards replies
	// out) both subscribe to the chat channel.
	if _, err := bs.Subscribe("alice", chat, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := bs.Subscribe("sink", chat, nil, nil); err != nil {
		t.Fatal(err)
	}

	// A plugin-sourced inbound chat message (no agent author) fans out to both.
	inbound, err := bs.Publish(bus.Message{Channel: chat, Type: "chat.message",
		Text: "hello", Source: "plugin:messenger"})
	if err != nil {
		t.Fatal(err)
	}
	// alice sees the inbound message in her pending queue.
	if _, res, _ := call(t, s, "GET", "/tools/message/ls", ""); res["count"].(float64) != 1 {
		t.Fatalf("alice should have the inbound chat message pending: %v", res)
	}

	// alice replies as herself through the real agent tool.
	ok, res, _ := call(t, s, "POST", "/tools/message/reply",
		`{"id":"`+inbound.ID+`","text":"answer"}`)
	if !ok || res["replied"] != true || res["in_reply_to"] != inbound.ID {
		t.Fatalf("reply = %v", res)
	}
	if res["channel"] != chat {
		t.Fatalf("reply should land on the chat channel, got %v", res["channel"])
	}
	replyID := res["id"].(string)

	// The reply IS delivered to the co-subscribed sink (it must forward it out).
	sinkPend, err := bs.Inbox("sink", "pending", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasInbox(sinkPend, replyID) {
		t.Fatalf("sink missed the reply it must forward out: %+v", sinkPend)
	}

	// The reply is NOT echoed back into alice's own queue. Her inbound delivery
	// was auto-processed by the reply and the reply itself was suppressed, so her
	// pending queue is empty. Pre-fix, the echoed reply would sit here (count 1).
	if _, res, _ := call(t, s, "GET", "/tools/message/ls", ""); res["count"].(float64) != 0 {
		t.Fatalf("reply echoed back into alice's own queue: pending=%v", res)
	}
	alicePend, err := bs.Inbox("alice", "pending", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if hasInbox(alicePend, replyID) {
		t.Fatalf("alice's queue contains her own reply %s: %+v", replyID, alicePend)
	}
}

// hasInbox reports whether any inbox item carries the given message id.
func hasInbox(items []bus.InboxItem, id string) bool {
	for _, it := range items {
		if it.Message.ID == id {
			return true
		}
	}
	return false
}

func TestRequestPrimitive(t *testing.T) {
	s, _ := newBusServer(t, "alice")

	ok, e, code := call(t, s, "POST", "/tools/request", `{"text":"hi"}`)
	if ok || code != http.StatusBadRequest || e["code"] != "missing_channel" {
		t.Fatalf("missing channel: code=%d err=%v", code, e)
	}

	ok, res, _ := call(t, s, "POST", "/tools/request", `{"channel":"svc:q","text":"do X"}`)
	if !ok || res["requested"] != true || res["correlation_id"] == "" {
		t.Fatalf("request = %v", res)
	}
	if res["id"] != res["correlation_id"] {
		t.Fatalf("request correlation should equal its own id: %v", res)
	}
}

func TestDLQListAndRequeue(t *testing.T) {
	s, bs := newBusServer(t, "alice")
	id := deliverToInbox(t, s, bs, "alice", "doomed")

	// Drive attempts past maxAttempts (5) so the delivery dead-letters.
	for i := 0; i < 6; i++ {
		if _, err := bs.Pending("alice", 100); err != nil {
			t.Fatal(err)
		}
	}
	ok, res, _ := call(t, s, "GET", "/tools/message/dlq", "")
	if !ok || res["count"].(float64) != 1 {
		t.Fatalf("dlq list = %v", res)
	}

	// Requeue returns it to the pending queue.
	ok, res, _ = call(t, s, "POST", "/tools/message/dlq/requeue", `{"id":"`+id+`"}`)
	if !ok || res["requeued"] != true {
		t.Fatalf("requeue = %v", res)
	}
	if _, res, _ := call(t, s, "GET", "/tools/message/dlq", ""); res["count"].(float64) != 0 {
		t.Fatalf("dlq should be empty after requeue: %v", res)
	}
	if _, res, _ := call(t, s, "GET", "/tools/message/ls", ""); res["count"].(float64) != 1 {
		t.Fatalf("requeued message should be pending again: %v", res)
	}

	// Requeue of an unknown id → 404.
	if ok, _, code := call(t, s, "POST", "/tools/message/dlq/requeue", `{"id":"nope"}`); ok || code != http.StatusNotFound {
		t.Fatalf("requeue unknown id: code=%d", code)
	}
}

func TestSubscribeParamsAndUnsubscribeByChannel(t *testing.T) {
	s, _ := newBusServer(t, "alice")

	// Parameterized subscription: watch fingerprint is returned and echoed by ls.
	body := `{"channel":"issue-provider:query","params":{"query":"Status: Open"}}`
	ok, res, _ := call(t, s, "POST", "/tools/channel/subscribe", body)
	if !ok || res["watch"] == nil || res["watch"] == "" {
		t.Fatalf("params subscribe should return watch: %v", res)
	}
	watch := res["watch"]

	ok, res, _ = call(t, s, "GET", "/tools/channel/ls", "")
	if !ok || res["count"].(float64) != 1 {
		t.Fatalf("ls = %v", res)
	}
	sub := res["subscriptions"].([]any)[0].(map[string]any)
	if sub["watch"] != watch || sub["params"] == nil {
		t.Fatalf("ls row missing params/watch: %v", sub)
	}

	// Unsubscribe by channel name (fallback) drops the sub.
	ok, res, _ = call(t, s, "POST", "/tools/channel/unsubscribe", `{"id":"issue-provider:query"}`)
	if !ok || res["removed"].(float64) != 1 {
		t.Fatalf("channel-name unsubscribe = %v", res)
	}
	if _, res, _ := call(t, s, "GET", "/tools/channel/ls", ""); res["count"].(float64) != 0 {
		t.Fatalf("subscription should be gone: %v", res)
	}

	// Unknown id/channel → 404.
	if ok, _, code := call(t, s, "POST", "/tools/channel/unsubscribe", `{"id":"nope"}`); ok || code != http.StatusNotFound {
		t.Fatalf("unknown unsubscribe: code=%d", code)
	}
}

// TestPhasePBusUnavailable checks every Phase P route degrades to bus_unavailable
// (503) when its hook is nil, matching the existing messages-surface contract.
func TestPhasePBusUnavailable(t *testing.T) {
	s := NewServer(Deps{
		Agent: "alice", Cwd: "/w", Plugins: []string{"whoami", "loop", "messages"},
		CurrentIteration: func() string { return "iter-1" },
		SetDone:          func(string, bool) error { return nil },
	})
	cases := []struct{ method, route, body string }{
		{"GET", "/tools/message/ls", ""},
		{"POST", "/tools/message/processed", `{"id":"x","result":"y"}`},
		{"POST", "/tools/message/reply", `{"id":"x","text":"y"}`},
		{"GET", "/tools/message/dlq", ""},
		{"POST", "/tools/message/dlq/requeue", `{"id":"x"}`},
		{"POST", "/tools/request", `{"channel":"c","text":"t"}`},
	}
	for _, c := range cases {
		ok, e, code := call(t, s, c.method, c.route, c.body)
		if ok || code != http.StatusServiceUnavailable || e["code"] != "bus_unavailable" {
			t.Fatalf("%s %s: code=%d err=%v, want 503 bus_unavailable", c.method, c.route, code, e)
		}
	}
}

func TestChatRoutesAreNotRegistered(t *testing.T) {
	s := NewServer(Deps{})
	for _, route := range []struct {
		method string
		path   string
	}{
		{"POST", "/tools/chat/create"},
		{"POST", "/tools/chat/bind"},
		{"GET", "/tools/chat/ls"},
	} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(route.method, route.path, nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404", route.method, route.path, rr.Code)
		}
	}
}
