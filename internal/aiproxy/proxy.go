package aiproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Exchange carries one request through the middleware chain.
type Exchange struct {
	R              *http.Request
	W              http.ResponseWriter
	Provider       string // anthropic | openai
	Path           string
	EscapedPath    string
	EscapedPathOK  bool
	Token          string
	Attr           Attribution
	ReqBody        []byte
	RespBody       []byte
	Streamed       bool
	Usage          Usage
	Model          string
	CostUSD        float64
	UpstreamStatus int
	Status         string // ok | budget_block | auth_error | upstream_error
	Start          time.Time
	LatencyMs      int
}

type Handler func(*Exchange) error
type Middleware func(Handler) Handler

// Warn is invoked when a warn-mode budget is exceeded (set by the daemon to
// publish a bus message + event). Audit records an agent-scoped audit event
// (set by the daemon to recordEvent with the real agent). Emit pushes a proxy
// SSE event. All are optional.
type Config struct {
	Tokens    *TokenRegistry
	Pricing   *Pricing
	Store     *Store
	Router    *Router // set in Task 6
	AgentsDir string
	Clock     func() time.Time
	Log       *slog.Logger
	Rand      io.Reader // request-id randomness (nil = crypto/rand)
	// GroupSnapshot resolves membership once while finalizing request metadata.
	// An error is diagnostic-only; the request remains recorded as ungrouped.
	GroupSnapshot func(agent string) (id, name string, err error)
	Activity      func(agent, iteration string, at time.Time) error

	Ingest func(AIRequest)                         // Task 7
	Emit   func(agent string, data map[string]any) // Task 9
	Warn   func(agent string, d Decision)          // Task 8/9
	Audit  func(agent, kind, dataJSON string)      // Task 9
	Budget *BudgetCache                            // Task 8
	Client *http.Client                            // Task 6 (nil = default)
}

type Proxy struct {
	cfg     Config
	ln      net.Listener
	server  *http.Server
	chain   Handler
	forward Handler // terminal; replaced with the real forward in Task 6
}

func New(cfg Config) *Proxy {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	p := &Proxy{cfg: cfg}
	p.forward = p.doForward
	p.rebuild()
	return p
}

// rebuild assembles the middleware chain (outer -> inner). Later tasks extend
// the middleware slice; the terminal is always p.forward (indirected so tests
// can swap it).
func (p *Proxy) rebuild() {
	terminal := func(ex *Exchange) error { return p.forward(ex) }
	mws := p.middlewares()
	h := terminal
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	p.chain = h
}

// middlewares returns the ordered chain (outer first). Authentication precedes
// the route guard so invalid attribution keeps its normal 401;
// record remains outside budget so its decisions are persisted.
func (p *Proxy) middlewares() []Middleware {
	mws := []Middleware{p.auth, p.upstreamRouteGuard, p.record}
	if p.cfg.Budget != nil {
		mws = append(mws, p.budget)
	}
	return mws
}

// upstreamRouteGuard rejects ambiguous path spellings for every provider and
// keeps account-bound OAuth credentials confined to the Codex endpoints served
// by the ChatGPT upstream. It runs outside record so a rejected request cannot
// create a transcript, usage row, audit, or event.
func (p *Proxy) upstreamRouteGuard(next Handler) Handler {
	return func(ex *Exchange) error {
		routeSafe := ex.EscapedPathOK && isSafeEscapedRoutePath(ex.Path, ex.EscapedPath)
		if routeSafe && (!isChatGPTRequest(ex.R.Header) || isCodexRoute(ex.Path)) {
			return next(ex)
		}
		ex.Status = "auth_error"
		ex.W.Header().Set("Content-Type", "application/json")
		ex.W.WriteHeader(http.StatusBadRequest)
		ex.W.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"request path is not allowed"}}`))
		return nil
	}
}

// budget checks the cached aggregate. block over-limit short-circuits with a
// 429 (the iteration ends with a budget error); warn over-limit signals once
// and continues.
func (p *Proxy) budget(next Handler) Handler {
	return func(ex *Exchange) error {
		d := p.cfg.Budget.Check(ex.Attr.Agent)
		if !d.Over {
			return next(ex)
		}
		if d.Mode == "block" {
			ex.Status = "budget_block"
			if p.cfg.Audit != nil {
				p.cfg.Audit(ex.Attr.Agent, "budget_block",
					`{"scope":"`+d.Scope+`"}`)
			}
			ex.W.Header().Set("Content-Type", "application/json")
			ex.W.WriteHeader(http.StatusTooManyRequests)
			message := "budget exceeded"
			if len(d.Exhausted) > 0 {
				message += ": " + strings.Join(d.Exhausted, ", ")
			}
			ex.W.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"` + message + `"}}`))
			return nil
		}
		// warn: emit once, keep serving.
		if p.cfg.Warn != nil {
			p.cfg.Warn(ex.Attr.Agent, d)
		}
		return next(ex)
	}
}

// record runs outermost: after the chain unwinds it writes the transcript
// (bodies) and enqueues the metadata row, so blocked/errored requests are
// recorded too. Task 9 adds the SSE emit here.
func (p *Proxy) record(next Handler) Handler {
	return func(ex *Exchange) error {
		err := next(ex)
		p.persist(ex)
		return err
	}
}

func (p *Proxy) persist(ex *Exchange) {
	if pathAtOrBelow(ex.Path, "/models") || pathAtOrBelow(ex.Path, "/v1/models") {
		p.cfg.Log.Debug("proxy models request",
			"agent", ex.Attr.Agent, "iteration", ex.Attr.Iteration, "status", ex.Status)
		return
	}
	if ex.Attr.Agent == "" || ex.Attr.Iteration == "" {
		return // auth failed before attribution; nothing to attribute
	}
	id := NewRequestID(p.cfg.Rand)
	now := p.cfg.Clock()
	row, groupErr := ex.toRequest(id, now, int(now.Sub(ex.Start).Milliseconds()), p.cfg.GroupSnapshot)
	if groupErr != nil {
		p.cfg.Log.Warn("group snapshot lookup failed",
			"agent", boundedGroupDiagnostic(ex.Attr.Agent),
			"err", boundedGroupDiagnostic(groupErr.Error()))
	}
	entry := TranscriptEntry{Meta: row, Request: ex.ReqBody}
	if len(ex.RespBody) > 0 {
		entry.Response = ex.RespBody
	}
	if err := AppendTranscript(p.cfg.AgentsDir, entry); err != nil {
		p.cfg.Log.Warn("append proxy transcript", "iteration", ex.Attr.Iteration, "err", err)
	}
	if p.cfg.Ingest != nil {
		p.cfg.Ingest(row)
	}
	// Task 9 emits the proxy SSE event here.
	p.emitProxyEvent(ex, row)
}

func (ex *Exchange) toRequest(
	id string,
	now time.Time,
	latencyMs int,
	groupSnapshot func(agent string) (id, name string, err error),
) (AIRequest, error) {
	row := AIRequest{
		ID: id, TS: now.UTC().Format(time.RFC3339Nano), Agent: ex.Attr.Agent, Iteration: ex.Attr.Iteration,
		ImageName: ex.Attr.ImageName, ImageTag: ex.Attr.ImageTag, ImageDigest: ex.Attr.ImageDigest,
		Provider: ex.Provider, Model: ex.Model,
		InputTokens: ex.Usage.InputTokens, OutputTokens: ex.Usage.OutputTokens,
		CacheWriteTokens: ex.Usage.CacheWriteTokens, CacheReadTokens: ex.Usage.CacheReadTokens,
		CostUSD: ex.CostUSD, LatencyMs: latencyMs, Status: ex.Status, UpstreamStatus: ex.UpstreamStatus,
		TaskID: ex.Attr.TaskID, EpicID: ex.Attr.EpicID,
	}
	if groupSnapshot == nil {
		return row, nil
	}
	groupID, groupName, err := groupSnapshot(row.Agent)
	if err != nil {
		return row, err
	}
	row.GroupID, row.GroupName = groupID, groupName
	return row, nil
}

const maxGroupDiagnosticBytes = 256

func boundedGroupDiagnostic(value string) string {
	if len(value) <= maxGroupDiagnosticBytes {
		return value
	}
	return value[:maxGroupDiagnosticBytes]
}

// emitProxyEvent pushes a metadata-only proxy event (model/tokens/cost/latency/
// status). Never carries request/response bodies (spec §9, §13).
func (p *Proxy) emitProxyEvent(ex *Exchange, row AIRequest) {
	if p.cfg.Emit == nil {
		return
	}
	p.cfg.Emit(row.Agent, map[string]any{
		"request_id":         row.ID,
		"iteration_id":       row.Iteration,
		"model":              row.Model,
		"provider":           row.Provider,
		"input_tokens":       row.InputTokens,
		"output_tokens":      row.OutputTokens,
		"cache_write_tokens": row.CacheWriteTokens,
		"cache_read_tokens":  row.CacheReadTokens,
		"cost_usd":           row.CostUSD,
		"latency_ms":         row.LatencyMs,
		"status":             row.Status,
	})
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ex := &Exchange{R: r, W: w, Path: r.URL.Path, Start: p.cfg.Clock()}
	p.resolvePathToken(ex)
	p.recordActivity(ex)
	ex.Provider = providerFor(r.URL.Path)
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	_ = r.Body.Close()
	if err != nil {
		ex.Status = "upstream_error"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"failed to read request body"}}`))
		ex.LatencyMs = int(p.cfg.Clock().Sub(ex.Start).Milliseconds())
		return
	}
	ex.ReqBody = body
	_ = p.chain(ex)
	ex.LatencyMs = int(p.cfg.Clock().Sub(ex.Start).Milliseconds())
}

func (p *Proxy) recordActivity(ex *Exchange) {
	if p.cfg.Activity == nil || ex.Attr.Agent == "" || ex.Attr.Iteration == "" {
		return
	}
	if err := p.cfg.Activity(ex.Attr.Agent, ex.Attr.Iteration, ex.Start); err != nil {
		p.cfg.Log.Warn("record proxy activity", "agent", ex.Attr.Agent, "iteration", ex.Attr.Iteration, "err", err)
	}
}

func (p *Proxy) resolvePathToken(ex *Exchange) {
	escapedPath, escapedOK := validatedEscapedPath(ex.R.URL)
	ex.EscapedPath = escapedPath
	ex.EscapedPathOK = escapedOK

	const prefix = "/_tariboy/"
	if !strings.HasPrefix(ex.R.URL.Path, prefix) {
		return
	}
	rest := strings.TrimPrefix(ex.R.URL.Path, prefix)
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return
	}
	rawTok := rest[:i]
	tok, err := url.PathUnescape(rawTok)
	if err != nil {
		return
	}
	attr, ok := p.cfg.Tokens.Resolve(tok)
	if !ok {
		return
	}
	ex.Token = tok
	ex.Attr = attr
	ex.R.URL.Path = "/" + rest[i+1:]
	ex.Path = ex.R.URL.Path
	ex.EscapedPath, ex.EscapedPathOK = tokenPathSuffix(escapedPath, tok, ex.Path)
	ex.EscapedPathOK = ex.EscapedPathOK && escapedOK
	ex.R.URL.RawPath = ""
}

func validatedEscapedPath(u *url.URL) (string, bool) {
	escaped := u.RawPath
	if escaped == "" {
		escaped = u.EscapedPath()
	}
	decoded, err := url.PathUnescape(escaped)
	return escaped, err == nil && decoded == u.Path
}

func tokenPathSuffix(escapedPath, token, decodedSuffix string) (string, bool) {
	const prefix = "/_tariboy/"
	if !strings.HasPrefix(escapedPath, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(escapedPath, prefix)
	i := strings.IndexByte(rest, '/')
	if i < 0 || rest[:i] != token {
		return "", false
	}
	suffix := "/" + rest[i+1:]
	decoded, err := url.PathUnescape(suffix)
	return suffix, err == nil && decoded == decodedSuffix
}

func providerFor(path string) string {
	if !isSafeRoutePath(path) {
		return "anthropic"
	}
	switch {
	case pathAtOrBelow(path, "/models"),
		pathAtOrBelow(path, "/responses"),
		pathAtOrBelow(path, "/v1/models"),
		pathAtOrBelow(path, "/v1/responses"),
		pathAtOrBelow(path, "/v1/chat/completions"):
		return "openai"
	default:
		return "anthropic"
	}
}

func isCodexRoute(path string) bool {
	return isSafeRoutePath(path) && (pathAtOrBelow(path, "/models") ||
		pathAtOrBelow(path, "/responses") ||
		pathAtOrBelow(path, "/v1/models") ||
		pathAtOrBelow(path, "/v1/responses"))
}

// isSafeRoutePath rejects path spellings that an HTTP client, upstream, or
// intermediary could normalize into a route outside the one selected here.
// net/http has already decoded URL.Path on ingress, so any remaining valid
// escape would be decoded only while constructing the upstream request and
// could make route selection disagree with forwarding. It validates only; it
// never rewrites the path sent by the caller.
func isSafeRoutePath(path string) bool {
	if path == "" || path[0] != '/' || hasUnsafeRouteBytes(path) {
		return false
	}
	unescaped, err := url.PathUnescape(path)
	if err != nil || unescaped != path {
		return false
	}

	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == "." || segment == ".." {
			return false
		}
		if segment == "" && i != 0 && i != len(segments)-1 {
			return false
		}
	}
	return true
}

func isSafeEscapedRoutePath(path, escapedPath string) bool {
	if !isSafeRoutePath(path) || !utf8.ValidString(path) {
		return false
	}
	decoded, err := url.PathUnescape(escapedPath)
	if err != nil || decoded != path {
		return false
	}
	for i := 0; i < len(escapedPath); i++ {
		if escapedPath[i] != '%' {
			continue
		}
		if i+2 >= len(escapedPath) {
			return false
		}
		b, ok := decodeHexByte(escapedPath[i+1], escapedPath[i+2])
		if !ok || b < 0x80 {
			return false
		}
		i += 2
	}
	return true
}

func decodeHexByte(hi, lo byte) (byte, bool) {
	h, ok := hexNibble(hi)
	if !ok {
		return 0, false
	}
	l, ok := hexNibble(lo)
	if !ok {
		return 0, false
	}
	return h<<4 | l, true
}

func hexNibble(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

func hasUnsafeRouteBytes(path string) bool {
	for i := 0; i < len(path); i++ {
		b := path[i]
		if b < 0x20 || b == 0x7f || b == '\\' || b == '?' || b == '#' {
			return true
		}
	}
	return false
}

func pathAtOrBelow(path, base string) bool {
	return path == base || strings.HasPrefix(path, base+"/")
}

// auth requires per-iteration attribution from the tokenized proxy URL. Provider
// auth headers are forwarded to the upstream unchanged by doForward.
func (p *Proxy) auth(next Handler) Handler {
	return func(ex *Exchange) error {
		if ex.Attr.Agent != "" && ex.Attr.Iteration != "" {
			return next(ex)
		}
		ex.Status = "auth_error"
		ex.W.Header().Set("Content-Type", "application/json")
		ex.W.WriteHeader(http.StatusUnauthorized)
		ex.W.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"missing or invalid tariboy attribution token"}}`))
		return nil
	}
}

func (p *Proxy) Mint(a Attribution) (string, error) { return p.cfg.Tokens.Mint(a) }
func (p *Proxy) Revoke(token string)                { p.cfg.Tokens.Revoke(token) }

// --- loop.ProxyBinder adapter ---

func (p *Proxy) ProxyBaseURL() string { return p.BaseURL() }

func (p *Proxy) ProxyBaseURLForToken(token string) string {
	return p.BaseURL() + "/_tariboy/" + url.PathEscape(token)
}

func (p *Proxy) MintToken(agent, iteration, imageName, imageTag, imageDigest string) (string, error) {
	return p.Mint(Attribution{Agent: agent, Iteration: iteration,
		ImageName: imageName, ImageTag: imageTag, ImageDigest: imageDigest})
}

func (p *Proxy) RevokeToken(token string) { p.Revoke(token) }

func (p *Proxy) RevokeIteration(iteration string) {
	p.cfg.Tokens.RevokeIteration(iteration)
}

// UpdateTask stamps native task/root attribution onto the live token(s) matching
// key (a token string or iteration id) and returns how many were updated. Empty
// taskID/epicID clear the tags. See TokenRegistry.UpdateTask.
func (p *Proxy) UpdateTask(key, taskID, epicID string) int {
	return p.cfg.Tokens.UpdateTask(key, taskID, epicID)
}

func (p *Proxy) SetTaskIfEmpty(key, taskID, epicID string) (int, bool) {
	return p.cfg.Tokens.SetTaskIfEmpty(key, taskID, epicID)
}

// Listen binds a random loopback port and records the chosen address.
func (p *Proxy) Listen() (string, error) {
	return p.ListenAt("")
}

// ListenAt binds addr, which must resolve to a loopback IP. An empty address
// allocates a random IPv4 loopback port.
func (p *Proxy) ListenAt(addr string) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid AI proxy listen address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("AI proxy listen address must be loopback: %q", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	p.ln = ln
	p.server = &http.Server{Handler: p}
	return ln.Addr().String(), nil
}

func (p *Proxy) Serve() error {
	err := p.server.Serve(p.ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (p *Proxy) Shutdown(ctx context.Context) error {
	if p.server == nil {
		return nil
	}
	return p.server.Shutdown(ctx)
}

// CloseListener releases a listener that was bound but never served, for
// example when persisting its restart handoff address fails.
func (p *Proxy) CloseListener() error {
	if p.ln == nil {
		return nil
	}
	return p.ln.Close()
}

func (p *Proxy) Addr() string {
	if p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

func (p *Proxy) BaseURL() string { return "http://" + p.Addr() }

// doForward proxies to the resolved upstream and parses usage/model/cost.
// Provider credentials are forwarded as the harness sent them.
func (p *Proxy) doForward(ex *Exchange) error {
	if ex.Provider == "openai" && ex.R.Header.Get("Authorization") == "" {
		ex.Status = "auth_error"
		ex.W.Header().Set("Content-Type", "application/json")
		ex.W.WriteHeader(http.StatusUnauthorized)
		ex.W.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"missing upstream authorization"}}`))
		return nil
	}

	up := p.upstreamFor(ex)
	target, err := url.Parse(strings.TrimRight(up.BaseURL, "/") + ex.Path)
	if err != nil {
		ex.Status = "upstream_error"
		ex.W.WriteHeader(http.StatusBadGateway)
		return nil
	}
	target.RawQuery = ex.R.URL.RawQuery

	outReq, err := http.NewRequestWithContext(ex.R.Context(), ex.R.Method, target.String(), bytes.NewReader(ex.ReqBody))
	if err != nil {
		ex.Status = "upstream_error"
		ex.W.WriteHeader(http.StatusBadGateway)
		return nil
	}
	copyHeaders(outReq.Header, ex.R.Header)
	outReq.Host = target.Host
	// Drop the client's Accept-Encoding so Go's transport negotiates gzip and
	// decompresses it; usage parsing and transcripts need the plain body.
	outReq.Header.Del("Accept-Encoding")
	if requestUsesAttributionToken(ex.R.Header, ex.Token) {
		// Codex authenticates to the custom provider with the iteration token.
		// That token grants attribution only and must never escape to the real
		// upstream; replace it with the daemon-owned provider credential.
		key := os.Getenv(up.KeyEnv)
		if key == "" {
			ex.Status = "upstream_error"
			ex.W.Header().Set("Content-Type", "application/json")
			ex.W.WriteHeader(http.StatusBadGateway)
			ex.W.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream credential unavailable"}}`))
			return nil
		}
		outReq.Header.Del("x-api-key")
		outReq.Header.Set("Authorization", "Bearer "+key)
	}

	client := sameOriginRedirectClient(p.cfg.Client)
	resp, err := client.Do(outReq)
	if err != nil {
		ex.Status = "upstream_error"
		ex.W.Header().Set("Content-Type", "application/json")
		ex.W.WriteHeader(http.StatusBadGateway)
		ex.W.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream unreachable"}}`))
		return nil
	}
	defer resp.Body.Close()
	ex.UpstreamStatus = resp.StatusCode

	copyHeaders(ex.W.Header(), resp.Header)
	ex.W.WriteHeader(resp.StatusCode)

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		ex.Streamed = true
		p.streamThrough(ex, resp.Body)
	} else {
		body, rerr := io.ReadAll(resp.Body)
		// Pass through whatever bytes we received (byte-identical); a read error
		// mid-body means a truncated upstream response, which we surface as an
		// upstream error while still forwarding the partial body.
		ex.RespBody = body
		ex.W.Write(body)
		if rerr != nil {
			ex.Status = "upstream_error"
		} else {
			p.parseUsage(ex, body)
		}
	}
	if resp.StatusCode >= 400 {
		ex.Status = "upstream_error"
	} else if ex.Status == "" {
		ex.Status = "ok"
	}
	return nil
}

func isChatGPTRequest(h http.Header) bool {
	for _, accountID := range h.Values("chatgpt-account-id") {
		if accountID != "" {
			return true
		}
	}
	return false
}

func (p *Proxy) upstreamFor(ex *Exchange) Upstream {
	provider := ex.Provider
	if provider == "openai" && isChatGPTRequest(ex.R.Header) {
		return p.cfg.Router.ResolveDefault("chatgpt")
	}
	return p.cfg.Router.Resolve(provider, modelFromBody(ex.ReqBody))
}

var errCrossOriginRedirect = errors.New("cross-origin redirect blocked")

func sameOriginRedirectClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	c := *base
	previous := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 &&
			(req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host) {
			return errCrossOriginRedirect
		}
		if previous != nil {
			return previous(req, via)
		}
		return nil
	}
	return &c
}

func requestUsesAttributionToken(h http.Header, token string) bool {
	if token == "" {
		return false
	}
	return h.Get("x-api-key") == token || h.Get("Authorization") == "Bearer "+token
}

// respBodyCap bounds the copy of a streamed response body kept for the
// transcript. The client stream is never capped — only this tapped copy is.
const respBodyCap = 8 << 20 // 8 MiB

// streamThrough copies an SSE body to the client byte-for-byte (preserving the
// exact event:/data:/blank-line framing) while tapping data: payloads into the
// accumulator. ReadBytes keeps the delimiter, so the concatenation of every
// chunk written equals the upstream stream exactly (spec §9 streaming usage).
//
// It also TEEs the raw upstream bytes into a capped buffer so persist can record
// the streamed response (spec §9 gap). The tee is a copy: the client output is
// unchanged and stays byte-identical to the upstream stream even after the cap
// is hit — only the captured copy stops growing (a truncation marker is added).
func (p *Proxy) streamThrough(ex *Exchange, body io.Reader) {
	acc := &SSEAccumulator{}
	flusher, _ := ex.W.(http.Flusher)
	br := bufio.NewReaderSize(body, 64*1024)
	var capture bytes.Buffer
	truncated := false
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			// Pass through unmodified so the client stream is byte-identical.
			ex.W.Write(line)
			if flusher != nil {
				flusher.Flush()
			}
			// Tee a capped copy for the transcript (never touches client output).
			if room := respBodyCap - capture.Len(); room > 0 {
				if len(line) <= room {
					capture.Write(line)
				} else {
					capture.Write(line[:room])
					truncated = true
				}
			} else {
				truncated = true
			}
			// Tap usage from `data:` lines (space optional; ignore [DONE]).
			trimmed := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[len("data:"):])
				if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
					acc.FeedData(payload)
				}
			}
		}
		if err != nil {
			break
		}
	}
	if truncated {
		capture.WriteString("\n[transcript truncated at cap]\n")
	}
	ex.RespBody = capture.Bytes()
	ex.Usage, ex.Model = acc.Result()
	p.setUsageCost(ex)
}

func (p *Proxy) parseUsage(ex *Exchange, body []byte) {
	var (
		u     Usage
		model string
		ok    bool
	)
	if ex.Provider == "openai" {
		u, model, ok = ParseOpenAIUsage(body)
	} else {
		u, model, ok = ParseAnthropicUsage(body)
	}
	if !ok {
		u, model, ok = ParseSSEUsage(body)
	}
	if ok {
		ex.Usage, ex.Model = u, model
		if ex.Model == "" {
			ex.Model = modelFromBody(ex.ReqBody)
		}
		p.setUsageCost(ex)
	}
}

func (p *Proxy) setUsageCost(ex *Exchange) {
	cost, known := p.cfg.Pricing.Price(ex.Model, ex.Usage)
	ex.CostUSD = cost
	if !known {
		p.cfg.Log.Warn("unknown model pricing", "model", boundedModel(ex.Model))
	}
}

const maxModelDiagnosticBytes = 256

func boundedModel(model string) string {
	if len(model) <= maxModelDiagnosticBytes {
		return model
	}
	return model[:maxModelDiagnosticBytes]
}

func modelFromBody(body []byte) string {
	var r struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Model
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		// Skip hop-by-hop headers.
		switch k {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Host", "Content-Length":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
