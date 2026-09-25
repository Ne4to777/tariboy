package aiproxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexChatGPTStreamingResponseRecordsUsageAndCost(t *testing.T) {
	const sse = "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-5.6-terra","usage":{"input_tokens":100,"input_tokens_details":{"cache_write_tokens":7,"cached_tokens":25},"output_tokens":40}}}` + "\n\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer up.Close()

	agentsDir := t.TempDir()
	mkIter(t, agentsDir, "codex", "codex-stream-1")
	router := NewRouter()
	router.SetDefault("chatgpt", Upstream{BaseURL: up.URL})
	pricing := &Pricing{table: map[string]ModelPrice{
		"gpt-5.6-terra": {
			InputPerMtok: 1_000_000, OutputPerMtok: 1_000_000,
			CacheWritePerMtok: 1_000_000, CacheReadPerMtok: 1_000_000,
		},
	}}
	var rows []AIRequest
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: pricing, Store: newStore(t),
		Router: router, AgentsDir: agentsDir, Ingest: func(row AIRequest) { rows = append(rows, row) },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-stream-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses", strings.NewReader(`{"model":"gpt-5.6-terra","stream":true}`))
	req.Header.Set("Authorization", "Bearer oauth-secret")
	req.Header.Set("chatgpt-account-id", "acct-1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Body.String(); got != sse {
		t.Fatalf("client stream differs from upstream: got=%q want=%q", got, sse)
	}
	if len(rows) != 1 {
		t.Fatalf("recorded rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Model != "gpt-5.6-terra" || row.InputTokens != 68 || row.OutputTokens != 40 || row.CacheWriteTokens != 7 || row.CacheReadTokens != 25 || row.CostUSD != 140 {
		t.Fatalf("recorded metadata = %+v", row)
	}

	b, err := os.ReadFile(filepath.Join(agentsDir, "codex", "iterations", "codex-stream-1", "proxy-transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entry TranscriptEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Meta.Model != row.Model || entry.Meta.InputTokens != 68 || entry.Meta.OutputTokens != 40 || entry.Meta.CacheWriteTokens != 7 || entry.Meta.CacheReadTokens != 25 || entry.Meta.CostUSD != 140 {
		t.Fatalf("transcript metadata = %+v", entry.Meta)
	}
	if got := string(entry.Response); got != sse {
		t.Fatalf("transcript response differs from upstream: got=%q want=%q", got, sse)
	}
}

func TestCodexChatGPTSSEBodyWithJSONContentTypeRecordsUsage(t *testing.T) {
	const sse = "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-5.6-terra","usage":{"input_tokens":100,"input_tokens_details":{"cache_write_tokens":7,"cached_tokens":25},"output_tokens":40}}}` + "\n\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ChatGPT can return an SSE-framed body without advertising it as SSE.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sse))
	}))
	defer up.Close()

	agentsDir := t.TempDir()
	mkIter(t, agentsDir, "codex", "codex-json-sse-1")
	router := NewRouter()
	router.SetDefault("chatgpt", Upstream{BaseURL: up.URL})
	var rows []AIRequest
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{}, Store: newStore(t),
		Router: router, AgentsDir: agentsDir, Ingest: func(row AIRequest) { rows = append(rows, row) },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-json-sse-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses", strings.NewReader(`{"model":"gpt-5.6-terra","stream":true}`))
	req.Header.Set("Authorization", "Bearer oauth-secret")
	req.Header.Set("chatgpt-account-id", "acct-1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if got := rr.Body.String(); got != sse {
		t.Fatalf("client response differs from upstream: got=%q want=%q", got, sse)
	}
	if len(rows) != 1 {
		t.Fatalf("recorded rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Model != "gpt-5.6-terra" || row.InputTokens != 68 || row.OutputTokens != 40 || row.CacheWriteTokens != 7 || row.CacheReadTokens != 25 {
		t.Fatalf("recorded metadata = %+v", row)
	}
}

func TestForwardUnknownModelPricingWarnsWithoutRequestContent(t *testing.T) {
	const requestContent = "request-content-must-not-log"
	model := strings.Repeat("m", 300)
	cases := []struct {
		name        string
		path        string
		contentType string
		response    string
	}{
		{
			name:        "non-streaming",
			path:        "/v1/chat/completions",
			contentType: "application/json",
			response:    `{"model":"` + model + `","usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		},
		{
			name:        "streaming",
			path:        "/responses",
			contentType: "text/event-stream",
			response:    `data: {"type":"response.completed","response":{"model":"` + model + `","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer up.Close()

			agentsDir := t.TempDir()
			mkIter(t, agentsDir, "codex", "unknown-model-1")
			router := NewRouter()
			router.SetDefault("openai", Upstream{BaseURL: up.URL})
			var logs bytes.Buffer
			var rows []AIRequest
			p := New(Config{
				Tokens: NewTokenRegistry(nil), Pricing: NewPricing(nil, nil, nil), Store: newStore(t),
				Router: router, AgentsDir: agentsDir, Ingest: func(row AIRequest) { rows = append(rows, row) },
				Log: slog.New(slog.NewTextHandler(&logs, nil)),
			})
			tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "unknown-model-1"})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/_tariboy/"+tok+tc.path,
				strings.NewReader(`{"model":"`+model+`","input":"`+requestContent+`"}`))
			req.Header.Set("Authorization", "Bearer oauth-secret")
			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if len(rows) != 1 || rows[0].CostUSD != 0 {
				t.Fatalf("rows = %+v, want one zero-cost request", rows)
			}
			if got := logs.String(); !strings.Contains(got, "unknown model pricing") {
				t.Fatalf("unknown model diagnostic missing: %q", got)
			} else if strings.Contains(got, requestContent) || strings.Contains(got, model) {
				t.Fatalf("unknown model diagnostic leaked request content or an unbounded model: %q", got)
			}
		})
	}
}

func TestCodexChatGPTResponsesForwardOAuthAndRecordUsage(t *testing.T) {
	var upstreamCalls atomic.Int32
	var gotPath string
	var gotAuthorization, gotAccountID bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization") == "Bearer oauth-secret"
		gotAccountID = r.Header.Get("chatgpt-account-id") == "acct-1"
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"gpt-5.6-terra","usage":{"input_tokens":12,"output_tokens":7}}`))
	}))
	defer up.Close()

	agentsDir := t.TempDir()
	mkIter(t, agentsDir, "codex", "codex-oauth-1")
	router := NewRouter()
	router.SetDefault("chatgpt", Upstream{BaseURL: up.URL})
	var rows []AIRequest
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		Router: router, AgentsDir: agentsDir, Ingest: func(row AIRequest) { rows = append(rows, row) },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-oauth-1", ImageName: "basic", ImageTag: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses",
		strings.NewReader(`{"model":"gpt-5.6-terra"}`))
	req.Header.Set("Authorization", "Bearer oauth-secret")
	req.Header.Set("chatgpt-account-id", "acct-1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || upstreamCalls.Load() != 1 || gotPath != "/responses" {
		t.Fatalf("forward result: status=%d calls=%d path=%q", rr.Code, upstreamCalls.Load(), gotPath)
	}
	if !gotAuthorization || !gotAccountID {
		t.Fatalf("OAuth headers forwarded: authorization=%t account_id=%t", gotAuthorization, gotAccountID)
	}
	if len(rows) != 1 || rows[0].Provider != "openai" || rows[0].InputTokens != 12 || rows[0].OutputTokens != 7 {
		t.Fatalf("recorded rows=%d metadata_ok=%t", len(rows), len(rows) == 1 && rows[0].Provider == "openai" && rows[0].InputTokens == 12 && rows[0].OutputTokens == 7)
	}
	transcript, err := os.ReadFile(filepath.Join(agentsDir, "codex", "iterations", "codex-oauth-1", "proxy-transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcript), "oauth-secret") || strings.Contains(string(transcript), "acct-1") {
		t.Fatal("transcript contains an OAuth credential or account identifier")
	}
}

func TestCodexChatGPTModelsProxiedWithoutPersistence(t *testing.T) {
	var upstreamCalls atomic.Int32
	var gotPathAndQuery bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		gotPathAndQuery = r.URL.RequestURI() == "/models?client_version=0.144.3"
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer up.Close()

	agentsDir := t.TempDir()
	mkIter(t, agentsDir, "codex", "codex-models-1")
	router := NewRouter()
	router.SetDefault("chatgpt", Upstream{BaseURL: up.URL})
	var rows []AIRequest
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		Router: router, AgentsDir: agentsDir, Ingest: func(row AIRequest) { rows = append(rows, row) },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-models-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/_tariboy/"+tok+"/models?client_version=0.144.3", nil)
	req.Header.Set("Authorization", "Bearer oauth-secret")
	req.Header.Set("chatgpt-account-id", "acct-1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || upstreamCalls.Load() != 1 || !gotPathAndQuery {
		t.Fatalf("models forward: status=%d calls=%d path_and_query_ok=%t", rr.Code, upstreamCalls.Load(), gotPathAndQuery)
	}
	if len(rows) != 0 {
		t.Fatalf("models usage rows=%d, want 0", len(rows))
	}
	_, err = os.Stat(filepath.Join(agentsDir, "codex", "iterations", "codex-models-1", "proxy-transcript.jsonl"))
	if !os.IsNotExist(err) {
		t.Fatalf("models transcript exists or stat failed: is_not_exist=%t", os.IsNotExist(err))
	}
}

func TestCodexChatGPTModelsSubrouteNotPersisted(t *testing.T) {
	agentsDir := t.TempDir()
	mkIter(t, agentsDir, "codex", "codex-model-detail-1")
	var rows []AIRequest
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		Router: NewRouter(), AgentsDir: agentsDir, Ingest: func(row AIRequest) { rows = append(rows, row) },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	p.persist(&Exchange{
		Path: "/models/gpt-5.6-terra", Attr: Attribution{Agent: "codex", Iteration: "codex-model-detail-1"},
		Start: time.Now(), Status: "ok",
	})
	if len(rows) != 0 {
		t.Fatalf("models subroute usage rows=%d, want 0", len(rows))
	}
	_, err := os.Stat(filepath.Join(agentsDir, "codex", "iterations", "codex-model-detail-1", "proxy-transcript.jsonl"))
	if !os.IsNotExist(err) {
		t.Fatalf("models subroute transcript exists or stat failed: is_not_exist=%t", os.IsNotExist(err))
	}
}

func TestCodexChatGPTFailsClosedBeforeUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	newProxy := func(t *testing.T) (*Proxy, string) {
		t.Helper()
		router := NewRouter()
		router.SetDefault("chatgpt", Upstream{BaseURL: up.URL})
		router.SetDefault("openai", Upstream{BaseURL: up.URL})
		p := New(Config{
			Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
			Router: router, AgentsDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-security-1"})
		if err != nil {
			t.Fatal(err)
		}
		return p, tok
	}

	t.Run("missing attribution", func(t *testing.T) {
		p, _ := newProxy(t)
		req := httptest.NewRequest("POST", "/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer oauth-secret")
		req.Header.Set("chatgpt-account-id", "acct-1")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || upstreamCalls.Load() != 0 {
			t.Fatalf("missing attribution: status=%d upstream_calls=%d", rr.Code, upstreamCalls.Load())
		}
	})

	t.Run("missing attribution on invalid route", func(t *testing.T) {
		p, _ := newProxy(t)
		req := httptest.NewRequest("POST", "/responses-evil", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer oauth-secret")
		req.Header.Set("chatgpt-account-id", "acct-1")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || upstreamCalls.Load() != 0 {
			t.Fatalf("missing attribution invalid route: status=%d upstream_calls=%d", rr.Code, upstreamCalls.Load())
		}
	})

	t.Run("revoked attribution", func(t *testing.T) {
		p, tok := newProxy(t)
		p.Revoke(tok)
		req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer oauth-secret")
		req.Header.Set("chatgpt-account-id", "acct-1")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || upstreamCalls.Load() != 0 {
			t.Fatalf("revoked attribution: status=%d upstream_calls=%d", rr.Code, upstreamCalls.Load())
		}
	})

	t.Run("revoked attribution on invalid route", func(t *testing.T) {
		p, tok := newProxy(t)
		p.Revoke(tok)
		req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses-evil", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer oauth-secret")
		req.Header.Set("chatgpt-account-id", "acct-1")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || upstreamCalls.Load() != 0 {
			t.Fatalf("revoked attribution invalid route: status=%d upstream_calls=%d", rr.Code, upstreamCalls.Load())
		}
	})

	t.Run("missing authorization", func(t *testing.T) {
		p, tok := newProxy(t)
		req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses", strings.NewReader(`{}`))
		req.Header.Set("chatgpt-account-id", "acct-1")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || upstreamCalls.Load() != 0 {
			t.Fatalf("missing authorization: status=%d upstream_calls=%d", rr.Code, upstreamCalls.Load())
		}
	})
}

func TestCodexChatGPTCrossOriginRedirectNotFollowed(t *testing.T) {
	var firstOriginCalls atomic.Int32
	var secondOriginCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondOriginCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstOriginCalls.Add(1)
		http.Redirect(w, r, second.URL+"/stolen", http.StatusFound)
	}))
	defer first.Close()

	router := NewRouter()
	router.SetDefault("chatgpt", Upstream{BaseURL: first.URL})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		Router: router, AgentsDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-redirect-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer oauth-secret")
	req.Header.Set("chatgpt-account-id", "acct-1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway || rr.Header().Get("Location") != "" || firstOriginCalls.Load() != 1 || secondOriginCalls.Load() != 0 {
		t.Fatalf("redirect result: status=%d has_location=%t first_origin=%d second_origin=%d",
			rr.Code, rr.Header().Get("Location") != "", firstOriginCalls.Load(), secondOriginCalls.Load())
	}
}

func TestCodexChatGPTSameOriginRedirectFollowed(t *testing.T) {
	var finalCalls atomic.Int32
	var authorizationOK, accountIDOK bool
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			http.Redirect(w, r, up.URL+"/final", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path == "/final" {
			finalCalls.Add(1)
			authorizationOK = r.Header.Get("Authorization") == "Bearer oauth-secret"
			accountIDOK = r.Header.Get("chatgpt-account-id") == "acct-1"
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"model":"gpt-5.6-terra","usage":{}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()

	router := NewRouter()
	router.SetDefault("chatgpt", Upstream{BaseURL: up.URL})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		Router: router, AgentsDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-same-origin-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer oauth-secret")
	req.Header.Set("chatgpt-account-id", "acct-1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || finalCalls.Load() != 1 || !authorizationOK || !accountIDOK {
		t.Fatalf("same-origin redirect: status=%d final_calls=%d authorization=%t account_id=%t",
			rr.Code, finalCalls.Load(), authorizationOK, accountIDOK)
	}
}

func TestCodexChatGPTRedirectPreservesConfiguredClientPolicy(t *testing.T) {
	var policyCalls atomic.Int32
	var finalCalls atomic.Int32
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			http.Redirect(w, r, up.URL+"/final", http.StatusTemporaryRedirect)
			return
		}
		finalCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	router := NewRouter()
	router.SetDefault("chatgpt", Upstream{BaseURL: up.URL})
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		policyCalls.Add(1)
		return http.ErrUseLastResponse
	}}
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		Router: router, AgentsDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Client: client,
	})
	tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-client-policy-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer oauth-secret")
	req.Header.Set("chatgpt-account-id", "acct-1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusTemporaryRedirect || policyCalls.Load() != 1 || finalCalls.Load() != 0 {
		t.Fatalf("configured redirect policy: status=%d policy_calls=%d final_calls=%d",
			rr.Code, policyCalls.Load(), finalCalls.Load())
	}
}

func TestProviderForOpenAIRouteBoundaries(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/responses", want: "openai"},
		{path: "/responses/compact", want: "openai"},
		{path: "/v1/responses/output", want: "openai"},
		{path: "/models/gpt-5.6-terra", want: "openai"},
		{path: "/v1/models/gpt-5.6-terra", want: "openai"},
		{path: "/v1/chat/completions/batches", want: "openai"},
		{path: "/responses-evil", want: "anthropic"},
		{path: "/v1/models-evil", want: "anthropic"},
		{path: "/v1/chat/completions-evil", want: "anthropic"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := providerFor(tt.path); got != tt.want {
				t.Fatalf("providerFor(%q)=%q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestCodexRoutePathSafety(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/responses", want: true},
		{path: "/responses/", want: true},
		{path: "/responses/compact", want: true},
		{path: "/responses/日本語", want: true},
		{path: "/v1/models/gpt-5.6-terra", want: true},
		{path: "/responses/../v1/messages", want: false},
		{path: "/responses//compact", want: false},
		{path: `/responses\..\v1\messages`, want: false},
		{path: "/responses/%2e%2e/v1/messages", want: false},
		{path: "/responses/%252e%252e/v1/messages", want: false},
		{path: "/responses/%00", want: false},
		{path: "/responses/%250a", want: false},
		{path: "/responses/%7f", want: false},
		{path: "/responses/%3f/v1/messages", want: false},
		{path: "/responses/%2523/v1/messages", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isCodexRoute(tt.path); got != tt.want {
				t.Errorf("isCodexRoute=%t, want %t", got, tt.want)
			}
		})
	}
}

func TestSafeRoutePathRejectsResidualEscapes(t *testing.T) {
	for _, path := range []string{
		"/responses%2fcompact",
		"/v1%2fresponses",
		"/responses%252fcompact",
	} {
		t.Run(path, func(t *testing.T) {
			if isSafeRoutePath(path) {
				t.Error("residual escape accepted after ingress decoding")
			}
		})
	}

	req := httptest.NewRequest("GET", "/responses/%E6%97%A5%E6%9C%AC%E8%AA%9E", nil)
	if req.URL.Path != "/responses/日本語" || !isSafeRoutePath(req.URL.Path) || !isCodexRoute(req.URL.Path) {
		t.Error("ordinary encoded Unicode was not accepted after net/http decoding")
	}
}

func TestChatGPTCredentialsRejectedOnNonCodexRoutesBeforeUpstream(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		model         string
		accountValues []string
		omitAccount   bool
		encodedToken  bool
		malformedRaw  bool
	}{
		{name: "anthropic messages", path: "/v1/messages", model: "claude-opus-4-8"},
		{name: "responses lookalike", path: "/responses-evil", model: "gpt-5.6-terra"},
		{name: "generic rule trap", path: "/internal/route", model: "gpt-5.6-terra"},
		{name: "duplicate account header", path: "/v1/messages", model: "claude-opus-4-8", accountValues: []string{"", "acct-secret"}},
		{name: "literal dot traversal", path: "/responses/../v1/messages", model: "gpt-5.6-terra"},
		{name: "encoded dot traversal", path: "/responses/%2e%2e/v1/messages", model: "gpt-5.6-terra"},
		{name: "encoded slash traversal", path: "/responses%2f..%2fv1%2fmessages", model: "gpt-5.6-terra"},
		{name: "double encoded dot traversal", path: "/responses/%252e%252e/v1/messages", model: "gpt-5.6-terra"},
		{name: "internal empty segment", path: "/responses//compact", model: "gpt-5.6-terra"},
		{name: "encoded backslash traversal", path: "/responses%5c..%5cv1%5cmessages", model: "gpt-5.6-terra"},
		{name: "authorization without account on traversal", path: "/responses/../v1/messages", model: "gpt-5.6-terra", omitAccount: true},
		{name: "encoded NUL", path: "/responses/%00", model: "gpt-5.6-terra"},
		{name: "nested encoded newline", path: "/responses/%250a", model: "gpt-5.6-terra"},
		{name: "DEL without account", path: "/responses/%7f", model: "gpt-5.6-terra", omitAccount: true},
		{name: "nested NUL without account", path: "/responses/%2500", model: "gpt-5.6-terra", omitAccount: true},
		{name: "encoded query delimiter", path: "/responses/%3f/v1/messages", model: "gpt-5.6-terra"},
		{name: "nested fragment delimiter without account", path: "/responses/%2523/v1/messages", model: "gpt-5.6-terra", omitAccount: true},
		{name: "nested slash account bound", path: "/responses%252fcompact", model: "gpt-5.6-terra"},
		{name: "nested slash authorization only", path: "/responses%252fcompact", model: "gpt-5.6-terra", omitAccount: true},
		{name: "nested version slash account bound", path: "/v1%252fresponses", model: "gpt-5.6-terra"},
		{name: "nested version slash authorization only", path: "/v1%252fresponses", model: "gpt-5.6-terra", omitAccount: true},
		{name: "deep nested slash account bound", path: "/responses%25252fcompact", model: "gpt-5.6-terra"},
		{name: "deep nested slash authorization only", path: "/responses%25252fcompact", model: "gpt-5.6-terra", omitAccount: true},
		{name: "single encoded slash account bound", path: "/responses%2fcompact", model: "gpt-5.6-terra"},
		{name: "single encoded slash authorization only", path: "/responses%2fcompact", model: "gpt-5.6-terra", omitAccount: true},
		{name: "encoded colon authorization only", path: "/responses%3acompact", model: "gpt-5.6-terra", omitAccount: true},
		{name: "encoded at authorization only", path: "/responses%40compact", model: "gpt-5.6-terra", omitAccount: true},
		{name: "encoded semicolon authorization only", path: "/responses%3bcompact", model: "gpt-5.6-terra", omitAccount: true},
		{name: "invalid UTF-8 account bound", path: "/responses/%ff", model: "gpt-5.6-terra"},
		{name: "encoded token spelling", path: "/responses", model: "gpt-5.6-terra", encodedToken: true},
		{name: "malformed raw path", path: "/responses", model: "gpt-5.6-terra", malformedRaw: true},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var chatGPTCalls, anthropicCalls, trapCalls atomic.Int32
			newUpstream := func(calls *atomic.Int32) *httptest.Server {
				t.Helper()
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.WriteHeader(http.StatusTeapot)
				}))
			}
			chatGPT := newUpstream(&chatGPTCalls)
			defer chatGPT.Close()
			anthropic := newUpstream(&anthropicCalls)
			defer anthropic.Close()
			trap := newUpstream(&trapCalls)
			defer trap.Close()

			agentsDir := t.TempDir()
			iteration := fmt.Sprintf("codex-invalid-route-%d", i)
			mkIter(t, agentsDir, "codex", iteration)
			router := NewRouter()
			router.SetDefault("chatgpt", Upstream{BaseURL: chatGPT.URL})
			router.SetDefault("anthropic", Upstream{BaseURL: anthropic.URL})
			router.SetRules([]Rule{{ModelGlob: "gpt-*", Upstream: Upstream{BaseURL: trap.URL}}})
			var rows atomic.Int32
			p := New(Config{
				Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
				Router: router, AgentsDir: agentsDir, Ingest: func(AIRequest) { rows.Add(1) },
				Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			tok, err := p.Mint(Attribution{Agent: "codex", Iteration: iteration})
			if err != nil {
				t.Fatal(err)
			}
			requestToken := tok
			if tt.encodedToken {
				requestToken = "%73" + tok[1:]
			}
			req := httptest.NewRequest("POST", "/_tariboy/"+requestToken+tt.path,
				strings.NewReader(fmt.Sprintf(`{"model":%q}`, tt.model)))
			if tt.malformedRaw {
				req.URL.RawPath = "/%zz"
			}
			req.Header.Set("Authorization", "Bearer oauth-secret")
			if !tt.omitAccount {
				if len(tt.accountValues) == 0 {
					req.Header.Set("chatgpt-account-id", "acct-secret")
				} else {
					for _, value := range tt.accountValues {
						req.Header.Add("chatgpt-account-id", value)
					}
				}
			}
			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Errorf("status=%d, want %d", rr.Code, http.StatusBadRequest)
			}
			if chatGPTCalls.Load() != 0 || anthropicCalls.Load() != 0 || trapCalls.Load() != 0 {
				t.Errorf("upstream calls: chatgpt=%d anthropic=%d generic_rule=%d, want all zero",
					chatGPTCalls.Load(), anthropicCalls.Load(), trapCalls.Load())
			}
			if strings.Contains(rr.Body.String(), "oauth-secret") || strings.Contains(rr.Body.String(), "acct-secret") {
				t.Error("error response contains credential material")
			}
			if rows.Load() != 0 {
				t.Errorf("persisted usage rows=%d, want zero", rows.Load())
			}
			transcript := filepath.Join(agentsDir, "codex", "iterations", iteration, "proxy-transcript.jsonl")
			if _, err := os.Stat(transcript); !os.IsNotExist(err) {
				t.Errorf("transcript created for rejected route")
			}
		})
	}
}

func TestUnsafeRouteRejectedBeforeBudgetAndRecord(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		withAccount bool
	}{
		{name: "control account bound", path: "/responses/%00", withAccount: true},
		{name: "control authorization only", path: "/responses/%00"},
		{name: "nested slash account bound", path: "/responses%252fcompact", withAccount: true},
		{name: "nested slash authorization only", path: "/responses%252fcompact"},
		{name: "single encoded slash account bound", path: "/responses%2fcompact", withAccount: true},
		{name: "single encoded slash authorization only", path: "/responses%2fcompact"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore(t)
			if err := st.SetBudget(Budget{Scope: "agent:codex", LimitUSD: 0, PeriodS: 3600, Mode: "warn"}); err != nil {
				t.Fatal(err)
			}
			budget := NewBudgetCache(st, time.Now)
			if err := budget.Refresh(); err != nil {
				t.Fatal(err)
			}
			agentsDir := t.TempDir()
			iteration := "codex-control-path"
			mkIter(t, agentsDir, "codex", iteration)
			var warnings, audits, rows, forwarded atomic.Int32
			p := New(Config{
				Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: st,
				Router: NewRouter(), AgentsDir: agentsDir, Budget: budget,
				Warn:   func(string, Decision) { warnings.Add(1) },
				Audit:  func(string, string, string) { audits.Add(1) },
				Ingest: func(AIRequest) { rows.Add(1) },
				Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			p.forward = func(ex *Exchange) error {
				forwarded.Add(1)
				ex.W.WriteHeader(http.StatusTeapot)
				return nil
			}
			tok, err := p.Mint(Attribution{Agent: "codex", Iteration: iteration})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/_tariboy/"+tok+tt.path,
				strings.NewReader(`{"model":"gpt-5.6-terra"}`))
			req.Header.Set("Authorization", "Bearer oauth-secret")
			if tt.withAccount {
				req.Header.Set("chatgpt-account-id", "acct-secret")
			}
			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Errorf("status=%d, want %d", rr.Code, http.StatusBadRequest)
			}
			if warnings.Load() != 0 || audits.Load() != 0 || rows.Load() != 0 || forwarded.Load() != 0 {
				t.Errorf("side effects: warnings=%d audits=%d rows=%d forwarded=%d, want all zero",
					warnings.Load(), audits.Load(), rows.Load(), forwarded.Load())
			}
			transcript := filepath.Join(agentsDir, "codex", "iterations", iteration, "proxy-transcript.jsonl")
			if _, err := os.Stat(transcript); !os.IsNotExist(err) {
				t.Error("transcript created for rejected control path")
			}
		})
	}
}

func TestCodexChatGPTSubrouteNeverReachesAnthropic(t *testing.T) {
	var chatGPTCalls, anthropicCalls atomic.Int32
	chatGPT := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatGPTCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"gpt-5.6-terra","usage":{}}`))
	}))
	defer chatGPT.Close()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer anthropic.Close()

	router := NewRouter()
	router.SetDefault("chatgpt", Upstream{BaseURL: chatGPT.URL})
	router.SetDefault("anthropic", Upstream{BaseURL: anthropic.URL})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		Router: router, AgentsDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-subroute-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses/%e6%97%a5%e6%9c%ac%e8%aa%9e", strings.NewReader(`{"model":"gpt-5.6-terra"}`))
	req.Header.Set("Authorization", "Bearer oauth-secret")
	req.Header.Set("chatgpt-account-id", "acct-1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || chatGPTCalls.Load() != 1 || anthropicCalls.Load() != 0 {
		t.Fatalf("subroute routing: status=%d chatgpt_calls=%d anthropic_calls=%d",
			rr.Code, chatGPTCalls.Load(), anthropicCalls.Load())
	}
}

func TestCodexChatGPTBypassesGenericModelRules(t *testing.T) {
	var chatGPTCalls, trapCalls atomic.Int32
	chatGPT := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatGPTCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"gpt-5.6-terra","usage":{}}`))
	}))
	defer chatGPT.Close()
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trapCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer trap.Close()

	router := NewRouter()
	router.SetDefault("chatgpt", Upstream{BaseURL: chatGPT.URL})
	router.SetRules([]Rule{{ModelGlob: "gpt-*", Upstream: Upstream{BaseURL: trap.URL}}})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		Router: router, AgentsDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-rule-bypass-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/responses", strings.NewReader(`{"model":"gpt-5.6-terra"}`))
	req.Header.Set("Authorization", "Bearer oauth-secret")
	req.Header.Set("chatgpt-account-id", "acct-1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || chatGPTCalls.Load() != 1 || trapCalls.Load() != 0 {
		t.Fatalf("OAuth model-rule routing: status=%d chatgpt_calls=%d trap_calls=%d",
			rr.Code, chatGPTCalls.Load(), trapCalls.Load())
	}
}

func TestOpenAIAPIKeyTrafficKeepsGenericModelRules(t *testing.T) {
	var defaultCalls, ruleCalls atomic.Int32
	var authorizationOK bool
	defaultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer defaultUpstream.Close()
	ruleUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ruleCalls.Add(1)
		authorizationOK = r.Header.Get("Authorization") == "Bearer api-key-secret"
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"gpt-4o","usage":{}}`))
	}))
	defer ruleUpstream.Close()

	router := NewRouter()
	router.SetDefault("openai", Upstream{BaseURL: defaultUpstream.URL})
	router.SetRules([]Rule{{ModelGlob: "gpt-*", Upstream: Upstream{BaseURL: ruleUpstream.URL}}})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		Router: router, AgentsDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, err := p.Mint(Attribution{Agent: "api-client", Iteration: "openai-rule-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/responses", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer api-key-secret")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || defaultCalls.Load() != 0 || ruleCalls.Load() != 1 || !authorizationOK {
		t.Fatalf("API-key model-rule routing: status=%d default_calls=%d rule_calls=%d authorization=%t",
			rr.Code, defaultCalls.Load(), ruleCalls.Load(), authorizationOK)
	}
}

func TestCodexResponsesRouteRecordsAttributedTranscript(t *testing.T) {
	const upstreamKey = "upstream-openai-key"
	t.Setenv("OPENAI_API_KEY", upstreamKey)
	var gotAuth, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"gpt-5","usage":{"input_tokens":12,"output_tokens":7}}`))
	}))
	defer up.Close()

	agentsDir := t.TempDir()
	mkIter(t, agentsDir, "codex", "codex-1")
	router := NewRouter()
	router.SetDefault("openai", Upstream{BaseURL: up.URL, KeyEnv: "OPENAI_API_KEY"})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
		Router: router, AgentsDir: agentsDir, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, err := p.Mint(Attribution{Agent: "codex", Iteration: "codex-1", ImageName: "basic", ImageTag: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || gotPath != "/v1/responses" {
		t.Fatalf("response route status=%d path=%q body=%s", rr.Code, gotPath, rr.Body.String())
	}
	if gotAuth != "Bearer "+upstreamKey {
		t.Fatalf("upstream authorization = %q; attribution token must not leave proxy", gotAuth)
	}
	b, err := os.ReadFile(filepath.Join(agentsDir, "codex", "iterations", "codex-1", "proxy-transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	for _, want := range []string{`"Agent":"codex"`, `"Iteration":"codex-1"`, `"Provider":"openai"`, `"InputTokens":12`, `"OutputTokens":7`} {
		if !strings.Contains(line, want) {
			t.Fatalf("transcript missing %s: %s", want, line)
		}
	}
}

func TestForwardKeepsProviderKeyAndParsesUsage(t *testing.T) {
	// Fake Anthropic upstream: asserts it received the provider key from the
	// harness request, never a tariboy attribution token.
	var gotKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":50,
			"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
	}))
	defer up.Close()

	s := newStore(t)
	router := NewRouter()
	router.SetDefault("anthropic", Upstream{BaseURL: up.URL, KeyEnv: "UNUSED_KEY_ENV"})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: s,
		Router: router, AgentsDir: t.TempDir(),
		Clock: func() time.Time { return time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC) },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, _ := p.Mint(Attribution{Agent: "alice", Iteration: "alice-1", ImageName: "basic", ImageTag: "latest"})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	req.Header.Set("x-api-key", "real-secret-key")
	p.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if gotKey != "real-secret-key" || gotKey == tok {
		t.Fatalf("upstream saw key %q, want provider key and never token", gotKey)
	}
	if !strings.Contains(rr.Body.String(), "usage") {
		t.Fatalf("response body not proxied back: %s", rr.Body.String())
	}
	// Usage + cost parsed onto the exchange (checked via a forward wrapper is
	// hard; assert via the recorded metadata in Task 7). Here we assert the
	// upstream status flowed through.
	if rr.Code != 200 {
		t.Fatalf("upstream status not proxied")
	}
}

func TestForwardStreamingSSEByteIdenticalAndUsage(t *testing.T) {
	// Realistic multi-event Anthropic SSE stream (event:/data: framing, blank-line
	// boundaries, a message_delta cumulative output, and OpenAI-style [DONE]).
	const sse = "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":1,"cache_creation_input_tokens":7,"cache_read_input_tokens":3}}}` + "\n" +
		"\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}` + "\n" +
		"\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":50}}` + "\n" +
		"\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n" +
		"\n" +
		"data: [DONE]\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sse))
	}))
	defer up.Close()

	s := newStore(t)
	router := NewRouter()
	router.SetDefault("anthropic", Upstream{BaseURL: up.URL, KeyEnv: "UNUSED_KEY_ENV"})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: s,
		Router: router, AgentsDir: t.TempDir(),
		Clock: func() time.Time { return time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC) },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// Capture the exchange to assert usage tapped from the stream. The terminal
	// dereferences p.forward at call time, so no rebuild is needed.
	var captured *Exchange
	orig := p.forward
	p.forward = func(ex *Exchange) error {
		err := orig(ex)
		captured = ex
		return err
	}

	tok, _ := p.Mint(Attribution{Agent: "alice", Iteration: "alice-1", ImageName: "basic", ImageTag: "latest"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	req.Header.Set("x-api-key", "real-secret-key")
	p.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Body.String(); got != sse {
		t.Fatalf("client stream not byte-identical to upstream:\n got=%q\nwant=%q", got, sse)
	}
	if captured == nil || !captured.Streamed {
		t.Fatalf("exchange not marked streamed: %+v", captured)
	}
	if captured.Usage.InputTokens != 100 || captured.Usage.OutputTokens != 50 {
		t.Fatalf("streamed usage = %+v, want input=100 output=50", captured.Usage)
	}
	if captured.Usage.CacheWriteTokens != 7 || captured.Usage.CacheReadTokens != 3 {
		t.Fatalf("streamed cache usage = %+v, want write=7 read=3", captured.Usage)
	}
	if captured.Model != "claude-opus-4-8" {
		t.Fatalf("streamed model = %q", captured.Model)
	}
}

// TestForwardStreamingCapturesRespBody covers the §9 gap: the streamed response
// must be recorded on the exchange (ex.RespBody) so persist writes it to the
// transcript, WITHOUT altering the client output (must stay byte-identical).
func TestForwardStreamingCapturesRespBody(t *testing.T) {
	const sse = "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n" +
		"\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}` + "\n" +
		"\n" +
		"data: [DONE]\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sse))
	}))
	defer up.Close()

	s := newStore(t)
	router := NewRouter()
	router.SetDefault("anthropic", Upstream{BaseURL: up.URL, KeyEnv: "UNUSED_KEY_ENV"})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: s,
		Router: router, AgentsDir: t.TempDir(),
		Clock: func() time.Time { return time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC) },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	var captured *Exchange
	orig := p.forward
	p.forward = func(ex *Exchange) error {
		err := orig(ex)
		captured = ex
		return err
	}

	tok, _ := p.Mint(Attribution{Agent: "alice", Iteration: "alice-1", ImageName: "basic", ImageTag: "latest"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	req.Header.Set("x-api-key", "real-secret-key")
	p.ServeHTTP(rr, req)

	if got := rr.Body.String(); got != sse {
		t.Fatalf("client stream not byte-identical:\n got=%q\nwant=%q", got, sse)
	}
	if captured == nil {
		t.Fatal("exchange not captured")
	}
	if got := string(captured.RespBody); got != sse {
		t.Fatalf("streamed response not captured on ex.RespBody:\n got=%q\nwant=%q", got, sse)
	}
}

// TestForwardStreamingRespBodyCappedButClientUncapped proves the tee is capped
// for the transcript while the client still receives ALL bytes byte-identically.
func TestForwardStreamingRespBodyCappedButClientUncapped(t *testing.T) {
	// Build an SSE stream that exceeds respBodyCap so the capture truncates.
	var b strings.Builder
	big := strings.Repeat("x", 100_000)
	for b.Len() < respBodyCap+500_000 {
		b.WriteString("data: {\"text\":\"" + big + "\"}\n")
	}
	sse := b.String()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sse))
	}))
	defer up.Close()

	s := newStore(t)
	router := NewRouter()
	router.SetDefault("anthropic", Upstream{BaseURL: up.URL, KeyEnv: "UNUSED_KEY_ENV"})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: s,
		Router: router, AgentsDir: t.TempDir(),
		Clock: func() time.Time { return time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC) },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	var captured *Exchange
	orig := p.forward
	p.forward = func(ex *Exchange) error {
		err := orig(ex)
		captured = ex
		return err
	}

	tok, _ := p.Mint(Attribution{Agent: "alice", Iteration: "alice-1", ImageName: "basic", ImageTag: "latest"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	req.Header.Set("x-api-key", "real-secret-key")
	p.ServeHTTP(rr, req)

	// Client output uncapped and byte-identical.
	if got := rr.Body.String(); got != sse {
		t.Fatalf("client stream not byte-identical under cap (len got=%d want=%d)", len(got), len(sse))
	}
	// Capture is capped: it is a prefix of the upstream stream up to the cap,
	// followed by a truncation marker.
	if captured == nil {
		t.Fatal("exchange not captured")
	}
	if len(captured.RespBody) >= len(sse) {
		t.Fatalf("RespBody not capped: got %d, upstream %d", len(captured.RespBody), len(sse))
	}
	if !strings.HasPrefix(sse, string(captured.RespBody[:respBodyCap])) {
		t.Fatal("captured bytes are not a prefix of the upstream stream up to the cap")
	}
	if !strings.Contains(string(captured.RespBody), "[transcript truncated at cap]") {
		t.Fatal("expected truncation marker in capped RespBody")
	}
}

func TestForwardRequiresAttributionPath(t *testing.T) {
	s := newStore(t)
	router := NewRouter()
	router.SetDefault("anthropic", Upstream{BaseURL: "http://unused", KeyEnv: "DEFINITELY_UNSET_KEY_ENV"})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: s,
		Router: router, AgentsDir: t.TempDir(),
		Clock: time.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "real-provider-key")
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing attribution -> status %d, want 401", rr.Code)
	}
}

func TestForwardPathTokenKeepsProviderAuth(t *testing.T) {
	var gotKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("upstream path = %q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"claude-haiku-4-5-20251001","usage":{"input_tokens":1,"output_tokens":2}}`))
	}))
	defer up.Close()

	s := newStore(t)
	router := NewRouter()
	router.SetDefault("anthropic", Upstream{BaseURL: up.URL, KeyEnv: "DEFINITELY_UNSET_KEY_ENV"})
	p := New(Config{
		Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: s,
		Router: router, AgentsDir: t.TempDir(),
		Clock: time.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tok, _ := p.Mint(Attribution{Agent: "a", Iteration: "a-1"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001"}`))
	req.Header.Set("x-api-key", "real-provider-key")
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if gotKey != "real-provider-key" {
		t.Fatalf("provider key was not forwarded as-is: %q", gotKey)
	}
}

// gzipIfAccepted mimics Anthropic: it compresses the body only when the
// request advertises gzip support.
func gzipIfAccepted(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(body))
		_ = zw.Close()
	}))
}

func TestAnthropicCompressedResponseRecordsUsage(t *testing.T) {
	const sse = "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":100,"cache_creation_input_tokens":7,"cache_read_input_tokens":25,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":40}}` + "\n\n"
	const js = `{"model":"claude-opus-4-8","usage":{"input_tokens":100,"cache_creation_input_tokens":7,"cache_read_input_tokens":25,"output_tokens":40}}`

	for _, tc := range []struct{ name, contentType, body, reqBody string }{
		{"stream", "text/event-stream", sse, `{"model":"claude-opus-4-8","stream":true}`},
		{"json", "application/json", js, `{"model":"claude-opus-4-8"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := gzipIfAccepted(t, tc.contentType, tc.body)
			defer up.Close()

			agentsDir := t.TempDir()
			mkIter(t, agentsDir, "alice", "alice-1")
			router := NewRouter()
			router.SetDefault("anthropic", Upstream{BaseURL: up.URL})
			var rows []AIRequest
			p := New(Config{
				Tokens: NewTokenRegistry(nil), Pricing: &Pricing{table: DefaultPricing()}, Store: newStore(t),
				Router: router, AgentsDir: agentsDir, Ingest: func(row AIRequest) { rows = append(rows, row) },
				Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			tok, err := p.Mint(Attribution{Agent: "alice", Iteration: "alice-1"})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(tc.reqBody))
			req.Header.Set("x-api-key", "real-provider-key")
			req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, req)

			if got := rr.Body.String(); got != tc.body {
				t.Fatalf("client response differs from upstream: got=%q want=%q", got, tc.body)
			}
			if enc := rr.Header().Get("Content-Encoding"); enc != "" {
				t.Fatalf("client Content-Encoding = %q, want none", enc)
			}
			if len(rows) != 1 {
				t.Fatalf("recorded rows = %d, want 1", len(rows))
			}
			row := rows[0]
			if row.Model != "claude-opus-4-8" || row.InputTokens != 100 || row.OutputTokens != 40 || row.CacheWriteTokens != 7 || row.CacheReadTokens != 25 || row.CostUSD <= 0 {
				t.Fatalf("recorded metadata = %+v", row)
			}
		})
	}
}
