package aiproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/agentdir"
)

func testProxy(t *testing.T) *Proxy {
	t.Helper()
	s := newStore(t)
	p := New(Config{
		Tokens:    NewTokenRegistry(nil),
		Pricing:   &Pricing{table: DefaultPricing()},
		Store:     s,
		AgentsDir: t.TempDir(),
		Clock:     func() time.Time { return time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC) },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return p
}

type activityCheckedReader struct {
	t      *testing.T
	called *bool
}

func (r activityCheckedReader) Read([]byte) (int, error) {
	if !*r.called {
		r.t.Error("request body read before activity was recorded")
	}
	return 0, io.EOF
}

func TestAuthRejectsUnknownToken(t *testing.T) {
	p := testProxy(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/sk-tariboy-bogus/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("x-api-key", "sk-tariboy-bogus")
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestAuthAcceptsTokenAndReachesForward(t *testing.T) {
	p := testProxy(t)
	// Replace the forward terminal with a recorder so we can assert attribution.
	var seen Attribution
	p.forward = func(ex *Exchange) error {
		seen = ex.Attr
		ex.Status = "ok"
		ex.W.WriteHeader(200)
		ex.W.Write([]byte(`{}`))
		return nil
	}
	tok, _ := p.Mint(Attribution{Agent: "alice", Iteration: "alice-1", ImageName: "basic", ImageTag: "latest"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("x-api-key", "real-provider-key")
	p.ServeHTTP(rr, req)
	if rr.Code != 200 || seen.Agent != "alice" || seen.Iteration != "alice-1" {
		t.Fatalf("code=%d attr=%+v", rr.Code, seen)
	}
}

func TestAuthenticatedRequestRecordsActivityBeforeForward(t *testing.T) {
	p := testProxy(t)
	called := false
	p.cfg.Activity = func(agent, iteration string, at time.Time) error {
		called = true
		if agent != "alice" || iteration != "alice-1" || !at.Equal(p.cfg.Clock()) {
			t.Fatalf("activity = %q %q %s", agent, iteration, at)
		}
		return nil
	}
	p.forward = func(ex *Exchange) error {
		if !called {
			t.Fatal("forward reached before activity was recorded")
		}
		ex.Status = "ok"
		ex.W.WriteHeader(http.StatusOK)
		return nil
	}
	p.rebuild()
	tok, _ := p.Mint(Attribution{Agent: "alice", Iteration: "alice-1"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/_tariboy/"+tok+"/v1/messages", activityCheckedReader{t: t, called: &called})
	req.Header.Set("x-api-key", "real-provider-key")
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestProviderDetection(t *testing.T) {
	p := testProxy(t)
	var provider string
	p.forward = func(ex *Exchange) error { provider = ex.Provider; ex.W.WriteHeader(200); return nil }
	tok, _ := p.Mint(Attribution{Agent: "a", Iteration: "a-1"})
	for path, want := range map[string]string{
		"/v1/messages":               "anthropic",
		"/v1/chat/completions":       "openai",
		"/v1/responses":              "openai",
		"/agent.v1.AgentService/Run": "cursor",
	} {
		provider = ""
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/_tariboy/"+tok+path, strings.NewReader(`{}`))
		req.Header.Set("x-api-key", "real-provider-key")
		p.ServeHTTP(rr, req)
		if provider != want {
			t.Fatalf("path %s -> provider %q, want %q", path, provider, want)
		}
	}
}

func TestListenBindsLoopbackOnly(t *testing.T) {
	p := testProxy(t)
	addr, err := p.Listen()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("addr = %q, want 127.0.0.1:*", addr)
	}
	if !strings.HasPrefix(p.BaseURL(), "http://127.0.0.1:") {
		t.Fatalf("BaseURL = %q", p.BaseURL())
	}
	_ = p.Shutdown(context.Background())
}

func TestProxyListenAtReusesExactLoopbackAddress(t *testing.T) {
	first := testProxy(t)
	addr, err := first.ListenAt("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- first.Serve() }()
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}

	second := testProxy(t)
	got, err := second.ListenAt(addr)
	if err != nil {
		t.Fatal(err)
	}
	if got != addr {
		t.Fatalf("rebound address = %q, want %q", got, addr)
	}
	go second.Serve()
	if err := second.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProxyListenAtRejectsNonLoopbackAddress(t *testing.T) {
	if _, err := testProxy(t).ListenAt("0.0.0.0:0"); err == nil {
		t.Fatal("non-loopback proxy address was accepted")
	}
}

func TestProxyGroupSnapshotIsCapturedOnceBeforeAsyncIngest(t *testing.T) {
	s := newStore(t)
	if _, err := s.db.Exec(`INSERT INTO groups(name, lead, settings) VALUES ('alpha', '', '{}'), ('beta', '', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO agents(name, image_ref, "group") VALUES ('alice', 'basic:latest', 'alpha')`); err != nil {
		t.Fatal(err)
	}

	agentsDir := t.TempDir()
	mkIter(t, agentsDir, "alice", "alice-1")
	ingester := NewIngester(s, discardLogger())
	lookups := 0
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: s,
		AgentsDir: agentsDir, Ingest: ingester.Enqueue,
		Clock: func() time.Time { return time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC) },
		Log:   discardLogger(),
		GroupSnapshot: func(agent string) (string, string, error) {
			lookups++
			return s.CurrentGroup(agent)
		},
	})
	p.forward = func(ex *Exchange) error {
		ex.Status = "ok"
		ex.W.WriteHeader(http.StatusOK)
		return nil
	}
	tok, err := p.Mint(Attribution{Agent: "alice", Iteration: "alice-1", ImageName: "basic", ImageTag: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	req.Header.Set("x-api-key", "real-provider-key")
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", rr.Code)
	}
	if lookups != 1 {
		t.Fatalf("group snapshot lookups = %d, want exactly 1", lookups)
	}

	// Mutate every live source after enqueue: rename and delete alpha, then move
	// Alice to beta. The buffered row must retain the request-time alpha value.
	if _, err := s.db.Exec(`UPDATE groups SET name='renamed-alpha' WHERE name='alpha'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM groups WHERE name='renamed-alpha'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agents SET "group"='beta' WHERE name='alice'`); err != nil {
		t.Fatal(err)
	}
	if err := ingester.Flush(); err != nil {
		t.Fatal(err)
	}

	var groupID, groupName string
	if err := s.db.QueryRow(`SELECT group_id, group_name FROM ai_requests`).Scan(&groupID, &groupName); err != nil {
		t.Fatal(err)
	}
	if groupID != "alpha" || groupName != "alpha" {
		t.Fatalf("stored snapshot = %q/%q, want alpha/alpha", groupID, groupName)
	}
}

func TestProxyGroupSnapshotLookupFailureRecordsUngroupedWithBoundedWarning(t *testing.T) {
	agentsDir := t.TempDir()
	mkIter(t, agentsDir, "alice", "alice-1")
	var logs bytes.Buffer
	var rows []AIRequest
	lookups := 0
	longError := strings.Repeat("x", 1024) + "unbounded-tail"
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		AgentsDir: agentsDir, Ingest: func(row AIRequest) { rows = append(rows, row) },
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
		GroupSnapshot: func(string) (string, string, error) {
			lookups++
			return "", "", errors.New(longError)
		},
	})
	p.forward = func(ex *Exchange) error {
		ex.Status = "ok"
		ex.W.WriteHeader(http.StatusOK)
		return nil
	}
	tok, err := p.Mint(Attribution{Agent: "alice", Iteration: "alice-1"})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	req.Header.Set("x-api-key", "real-provider-key")
	p.ServeHTTP(rr, req)

	if lookups != 1 {
		t.Fatalf("group snapshot lookups = %d, want exactly 1", lookups)
	}
	if len(rows) != 1 || rows[0].GroupID != "" || rows[0].GroupName != "" {
		t.Fatalf("recorded rows = %+v, want one ungrouped row", rows)
	}
	if got := logs.String(); !strings.Contains(got, "group snapshot") || strings.Contains(got, "unbounded-tail") || len(got) > 700 {
		t.Fatalf("group lookup warning was missing or unbounded: len=%d log=%q", len(got), got)
	}
}

func mkIter(t *testing.T, agentsDir, agent, iteration string) {
	t.Helper()
	if err := agentdir.New(agentsDir, agent).EnsureIteration(iteration); err != nil {
		t.Fatal(err)
	}
}
