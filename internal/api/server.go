// Package api serves the registry over HTTP (unix socket by default).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/auditexport"
	"github.com/alekzonder/tariboy/internal/events"
	"github.com/alekzonder/tariboy/internal/imageportable"
	"github.com/alekzonder/tariboy/internal/imagesnapshot"
	"github.com/alekzonder/tariboy/internal/plugincaps"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/teamportable"
	"github.com/coder/websocket"
)

type Server struct {
	reg             *registry.Registry
	cctx            *registry.Ctx
	http            *http.Server
	webSrv          *http.Server // loopback web listener (nil until ServeWeb)
	serveMu         sync.Mutex
	stopping        bool
	events          events.Source
	plugins         http.Handler
	support         SupportBundleSource
	tasks           tasks.EventSource
	taskActor       func() tasks.Actor
	externalPlugins plugincaps.ExternalResolver
}

func NewServer(reg *registry.Registry, cctx *registry.Ctx) *Server {
	s := &Server{reg: reg, cctx: cctx}
	s.http = &http.Server{Handler: s.Handler()}
	return s
}

// SetEventSource installs the live-events source; when set, the streaming SSE
// route GET /api/agents/{name}/events is registered.
func (s *Server) SetEventSource(src events.Source) { s.events = src }

// SetPluginAPI installs the plugin-facing routes (POST /api/plugin/publish,
// GET /api/plugin/subscriptions, GET /api/plugin/watches), called by plugins
// with their plugin-token over the daemon unix socket.
func (s *Server) SetPluginAPI(h http.Handler) { s.plugins = h }

func (s *Server) SetExternalPlugins(resolver plugincaps.ExternalResolver) {
	s.externalPlugins = resolver
}

func (s *Server) SetTasks(source tasks.EventSource, actor func() tasks.Actor) {
	s.tasks, s.taskActor = source, actor
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, c := range s.reg.Commands() {
		if c.HTTP == nil {
			continue
		}
		cmd := c // capture
		mux.HandleFunc(cmd.HTTP.Method+" "+cmd.HTTP.Path, func(w http.ResponseWriter, r *http.Request) {
			s.dispatch(cmd, w, r)
		})
	}
	if s.events != nil {
		mux.HandleFunc("GET /api/agents/{name}/events", s.serveEvents)
		mux.HandleFunc("GET /api/messages/ws", s.serveMessages)
	}
	mux.HandleFunc("GET /api/agents/{name}/terminal", s.serveTerminal)
	mux.HandleFunc("GET /api/agents/{name}/audit-export", s.serveAuditExport)
	if s.cctx.Scripts != nil {
		mux.HandleFunc("GET /api/agents/{name}/script-runs/{id}/download", s.serveScriptLogDownload)
	}
	mux.HandleFunc("GET /api/images/{ref}/export", s.serveImageExport)
	mux.HandleFunc("POST /api/image-imports", s.serveImageImportPreview)
	mux.HandleFunc("POST /api/image-imports/{id}/apply", s.serveImageImportApply)
	mux.HandleFunc("GET /api/workflow-images/{name}/{tag}/export", s.serveWorkflowImageExport)
	mux.HandleFunc("POST /api/workflow-image-imports", s.serveWorkflowImageImport)
	mux.HandleFunc("GET /api/groups/{name}/export", s.serveTeamExport)
	mux.HandleFunc("POST /api/team-imports", s.serveTeamImportPreview)
	mux.HandleFunc("PUT /api/files/raw", s.serveFileUpload)
	if s.tasks != nil {
		mux.HandleFunc("GET /api/tasks/ws", s.serveTasks)
	}
	if s.plugins != nil {
		mux.Handle("POST /api/plugin/publish", s.plugins)
		mux.Handle("GET /api/plugin/subscriptions", s.plugins)
		mux.Handle("GET /api/plugin/watches", s.plugins)
	}
	if s.support != nil {
		mux.HandleFunc("GET /api/daemon/support-bundle", s.serveSupportBundle)
	}
	mux.HandleFunc("GET /api/help.json", func(w http.ResponseWriter, r *http.Request) {
		WriteOK(w, s.reg.Tree())
	})
	mux.HandleFunc("GET /api/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		WriteOK(w, s.openapi())
	})
	// The daemon serves no UI: it is API + WS only, and the desktop app carries
	// the SPA. Every unmatched path — /api/* miss or not — is a JSON 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		WriteErr(w, http.StatusNotFound, "not_found", "unknown route "+r.Method+" "+r.URL.Path)
	})
	return VersionHeader(s.accessLog(mux))
}

func (s *Server) serveFileUpload(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if r.ContentLength > MaxFileUploadBytes {
		WriteErr(w, http.StatusRequestEntityTooLarge, "too_large", "file exceeds 1 GiB")
		return
	}
	result, err := SaveUploadedFile(s.cctx.BaseDir, r.URL.Query().Get("name"), r.Body, MaxFileUploadBytes)
	if err != nil {
		var userErr UserError
		if errors.As(err, &userErr) {
			status := userErr.Status
			if status == 0 {
				status = http.StatusBadRequest
			}
			WriteErr(w, status, userErr.Code, userErr.Msg)
			return
		}
		s.cctx.Log.Error("file upload failed", "err", err)
		WriteErr(w, http.StatusInternalServerError, "internal", "internal error, see daemon log")
		return
	}
	WriteOK(w, result)
}

func (s *Server) serveScriptLogDownload(w http.ResponseWriter, r *http.Request) {
	agentName, runID := r.PathValue("name"), r.PathValue("id")
	if !agent.ValidName(agentName) || runID == "" {
		WriteErr(w, http.StatusBadRequest, "bad_request", "invalid agent or script run")
		return
	}
	file, filename, err := s.cctx.Scripts.OpenScriptLog(agentName, runID)
	if err != nil {
		WriteErr(w, http.StatusNotFound, "not_found", "script run log not found")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeAttachmentPart(filename)+`"`)
	_, _ = io.Copy(w, file)
}

func (s *Server) serveAuditExport(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("name")
	iteration := r.URL.Query().Get("iteration")
	if !agent.ValidName(agentName) || iteration != "" && !validAuditIterationID(iteration) {
		WriteErr(w, http.StatusBadRequest, "bad_request", "invalid agent or iteration")
		return
	}
	markdown := r.URL.Query().Get("format") == "markdown"
	agentsDir := filepath.Join(s.cctx.BaseDir, "agents")
	scope := iteration
	if scope == "" {
		scope = "all"
	}
	if markdown {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	} else {
		filename := safeAttachmentPart(agentName) + "-" + safeAttachmentPart(scope) + "-audit.zip"
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	}
	stream := &responseWriteTracker{ResponseWriter: w}
	var err error
	if markdown {
		err = auditexport.WriteMarkdown(r.Context(), stream, agentsDir, agentName, iteration)
	} else {
		err = auditexport.WriteZIP(r.Context(), stream, agentsDir, agentName, iteration)
	}
	if err != nil {
		if !stream.started {
			w.Header().Del("Content-Disposition")
			WriteErr(w, http.StatusInternalServerError, "audit_export_failed", "cannot build audit export")
		} else {
			s.cctx.Log.Error("audit export stream failed")
		}
	}
}

type responseWriteTracker struct {
	http.ResponseWriter
	started bool
}

func (w *responseWriteTracker) Write(body []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(body)
}

func (w *responseWriteTracker) WriteHeader(status int) {
	w.started = true
	w.ResponseWriter.WriteHeader(status)
}

func validAuditIterationID(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 255 &&
		!strings.ContainsAny(value, "/\\\x00")
}

func safeAttachmentPart(value string) string {
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "audit"
	}
	return out.String()
}

func (s *Server) teamPortableService() teamportable.Service {
	return teamportable.Service{Snapshots: &imagesnapshot.Store{DB: s.cctx.Store.DB, Root: filepath.Join(s.cctx.BaseDir, "image-source-snapshots")}, StagingRoot: filepath.Join(s.cctx.BaseDir, "team-imports")}
}

func (s *Server) serveTeamExport(w http.ResponseWriter, r *http.Request) {
	command, ok := s.reg.Get("team.compose")
	if !ok {
		WriteErr(w, http.StatusNotImplemented, "unavailable", "team export unavailable")
		return
	}
	value, err := command.Handler(s.cctx, registry.Params{"name": r.PathValue("name")})
	if err != nil {
		WriteErr(w, http.StatusBadRequest, "team_export_failed", err.Error())
		return
	}
	result, _ := value.(map[string]any)
	yamlText, _ := result["yaml"].(string)
	members, err := agent.NewStore(s.cctx.Store).ListByGroup(r.PathValue("name"))
	if err != nil {
		WriteErr(w, http.StatusInternalServerError, "internal", "cannot list team")
		return
	}
	refs := make([]string, 0, len(members))
	for _, member := range members {
		refs = append(refs, member.ImageRef)
	}
	temp, err := os.CreateTemp(s.cctx.BaseDir, ".team-export-*.tar.gz")
	if err != nil {
		WriteErr(w, http.StatusInternalServerError, "internal", "cannot stage export")
		return
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := s.teamPortableService().Export(r.Context(), r.PathValue("name"), []byte(yamlText), refs, temp); err != nil {
		temp.Close()
		WriteErr(w, http.StatusConflict, "team_not_exportable", err.Error())
		return
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		temp.Close()
		WriteErr(w, http.StatusInternalServerError, "internal", "cannot read export")
		return
	}
	defer temp.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="tariboy-team.tar.gz"`)
	_, _ = io.Copy(w, temp)
}

func (s *Server) serveTeamImportPreview(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 256 << 20
	if r.ContentLength < 0 || r.ContentLength > maxUpload {
		WriteErr(w, http.StatusRequestEntityTooLarge, "archive_too_large", "team archive size is missing or exceeds 256 MiB")
		return
	}
	defer r.Body.Close()
	preview, err := s.teamPortableService().Preview(r.Context(), http.MaxBytesReader(w, r.Body, maxUpload), r.ContentLength)
	if err != nil {
		WriteErr(w, http.StatusBadRequest, "bad_archive", err.Error())
		return
	}
	planner, ok := s.reg.Get("team.import.archive.plan")
	if !ok {
		_ = os.RemoveAll(preview.StagedDir)
		WriteErr(w, http.StatusInternalServerError, "internal", "team import planner unavailable")
		return
	}
	plan, err := planner.Handler(s.cctx, registry.Params{"id": preview.ImportID})
	if err != nil {
		_ = os.RemoveAll(preview.StagedDir)
		WriteErr(w, http.StatusBadRequest, "bad_compose", err.Error())
		return
	}
	WriteOK(w, plan)
}

func (s *Server) imagePortableService() imageportable.Service {
	return imageportable.Service{
		Snapshots: &imagesnapshot.Store{
			DB: s.cctx.Store.DB, Root: filepath.Join(s.cctx.BaseDir, "image-source-snapshots"),
		},
		StagingRoot:     filepath.Join(s.cctx.BaseDir, "image-imports"),
		BaseDir:         s.cctx.BaseDir,
		ExternalPlugins: s.externalPlugins,
	}
}

func (s *Server) serveImageExport(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	temp, err := os.CreateTemp(s.cctx.BaseDir, ".image-export-*.tar.gz")
	if err != nil {
		WriteErr(w, http.StatusInternalServerError, "internal", "cannot stage export")
		return
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := s.imagePortableService().Export(r.Context(), ref, temp); err != nil {
		temp.Close()
		WriteErr(w, http.StatusNotFound, "image_not_exportable", err.Error())
		return
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		temp.Close()
		WriteErr(w, http.StatusInternalServerError, "internal", "cannot read export")
		return
	}
	defer temp.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="tariboy-image.tar.gz"`)
	_, _ = io.Copy(w, temp)
}

func (s *Server) serveImageImportApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ref string `json:"ref"`
	}
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		WriteErr(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON")
		return
	}
	result, err := s.imagePortableService().Apply(r.Context(), r.PathValue("id"), body.Ref)
	if err != nil {
		WriteErr(w, http.StatusConflict, "image_import_failed", err.Error())
		return
	}
	WriteOK(w, result)
}

func (s *Server) serveImageImportPreview(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 64 << 20
	if r.ContentLength < 0 || r.ContentLength > maxUpload {
		WriteErr(w, http.StatusRequestEntityTooLarge, "archive_too_large", "image archive size is missing or exceeds 64 MiB")
		return
	}
	preview, err := s.imagePortableService().Preview(r.Context(), http.MaxBytesReader(w, r.Body, maxUpload), r.ContentLength)
	if err != nil {
		WriteErr(w, http.StatusBadRequest, "bad_archive", err.Error())
		return
	}
	WriteOK(w, preview)
}

func (s *Server) serveTasks(w http.ResponseWriter, r *http.Request) {
	// The tokenless loopback listener relies on the browser Origin boundary.
	// Authenticated federated sockets may legitimately come from another host,
	// while non-browser clients commonly omit Origin entirely.
	if !requestAuthenticated(r) {
		if origin := r.Header.Get("Origin"); origin != "" && !isAllowedWebOrigin(origin) {
			WriteErr(w, http.StatusForbidden, "forbidden_origin", "websocket origin is not allowed")
			return
		}
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ctx := ws.CloseRead(r.Context())
	actor := tasks.CustomerActor("")
	if s.taskActor != nil {
		actor = s.taskActor()
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	wake, cancel := s.tasks.Subscribe()
	defer cancel()

	send := func() error {
		for {
			hints, reset, err := s.tasks.Replay(ctx, actor, after, 200)
			if err != nil {
				return err
			}
			if reset {
				if err := ws.Write(ctx, websocket.MessageText, mustJSON(tasks.EventHint{
					Type: "reset", Sequence: after,
				})); err != nil {
					return err
				}
			}
			for _, hint := range hints {
				if err := ws.Write(ctx, websocket.MessageText, mustJSON(hint)); err != nil {
					return err
				}
				after = hint.Sequence
			}
			if len(hints) < 200 {
				return nil
			}
		}
	}
	if err := send(); err != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-wake:
			if !ok || send() != nil {
				return
			}
		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := ws.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func mustJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func wildcardNames(pattern string) []string {
	var out []string
	for _, seg := range strings.Split(pattern, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			// A trailing "{name...}" wildcard is exposed by ServeMux under the
			// bare name ("name"), so strip the "..." before the PathValue lookup.
			out = append(out, strings.TrimSuffix(seg[1:len(seg)-1], "..."))
		}
	}
	return out
}

func (s *Server) dispatch(cmd registry.Command, w http.ResponseWriter, r *http.Request) {
	p := registry.Params{}
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		for k, vs := range r.URL.Query() {
			if len(vs) > 0 {
				p[k] = vs[0]
			}
		}
	} else if r.Body != nil {
		defer r.Body.Close()
		if cmd.HTTP.MaxBodyBytes > 0 {
			if r.ContentLength > cmd.HTTP.MaxBodyBytes {
				WriteErr(w, http.StatusRequestEntityTooLarge, "too_large", "request body exceeds the upload limit")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, cmd.HTTP.MaxBodyBytes)
		}
		// An empty body (io.EOF) means "no params"; any other decode error is a bad request.
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil && !errors.Is(err, io.EOF) {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				WriteErr(w, http.StatusRequestEntityTooLarge, "too_large", "request body exceeds the upload limit")
				return
			}
			WriteErr(w, http.StatusBadRequest, "bad_json", "request body is not a JSON object: "+err.Error())
			return
		}
	}
	for _, name := range wildcardNames(cmd.HTTP.Path) {
		if v := r.PathValue(name); v != "" {
			p[name] = v
		}
	}
	p[registry.RequestContextParam] = r.Context()
	result, err := cmd.Handler(s.cctx, p)
	if err != nil {
		var ue UserError
		if errors.As(err, &ue) {
			status := ue.Status
			if status == 0 {
				status = http.StatusBadRequest
			}
			WriteErrData(w, status, ue.Code, ue.Msg, ue.Data)
			return
		}
		s.cctx.Log.Error("handler failed", "command", cmd.Path, "err", err)
		WriteErr(w, http.StatusInternalServerError, "internal", "internal error, see daemon log")
		return
	}
	WriteOK(w, result)
}

// serveEvents streams live agent events as SSE. It is hand-written (not routed
// through the registry envelope, which buffers a JSON body), flushes per event,
// and returns when the client disconnects (r.Context().Done()). The subscriber
// is always unregistered via cancel so no hub goroutine or channel leaks.
func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteErr(w, http.StatusInternalServerError, "no_stream", "streaming unsupported")
		return
	}
	agent := r.PathValue("name")
	var types []string
	if t := r.URL.Query().Get("types"); t != "" {
		types = strings.Split(t, ",")
	}
	ch, cancel := s.events.Subscribe(agent, types)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := w.Write([]byte("event: " + e.Type + "\ndata: " + string(data) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// queryInt returns the integer value of query parameter key, or def when it is
// absent or not a valid integer.
func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// serveTerminal bridges a browser websocket to the agent's live interactive
// shim PTY. Binary frames carry raw terminal bytes in both directions; a text
// frame {"cols":C,"rows":R} triggers a PTY resize. When no interactive
// iteration is running, Attach fails and the socket is closed with code 4404 so
// the frontend can distinguish "gone" from an ordinary close.
func (s *Server) serveTerminal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cols, rows := queryInt(r, "cols", 80), queryInt(r, "rows", 24)

	conn, err := s.cctx.Control.Attach(name, cols, rows)
	if err != nil {
		// No interactive iteration: accept then close with a distinct code so
		// the client can tell "no session" from other close reasons.
		//
		// InsecureSkipVerify disables coder/websocket's default same-origin
		// check. Auth here is the bearer token (Authorization header or
		// ?token= query param, checked before this handler runs), not the
		// browser Origin, and CORS already answers every request with
		// Access-Control-Allow-Origin: * (see the CORS middleware below), so
		// origin pinning adds no real protection — it only breaks the
		// federated /terminals page, where a browser on daemon A opens a
		// terminal websocket to daemon B and Origin never equals Host.
		if c, e := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true}); e == nil {
			c.Close(4404, "no interactive session")
		}
		return
	}
	defer conn.Close()

	// See the InsecureSkipVerify comment above: token auth + wildcard CORS
	// make origin pinning both redundant and harmful for cross-host use.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	// Terminal input (paste, control sequences) must not be capped by the
	// library's default 32 KiB read limit; -1 disables the limit entirely.
	ws.SetReadLimit(-1)
	ctx := r.Context()

	// PTY bytes → browser (binary frames).
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := conn.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				ws.Close(websocket.StatusNormalClosure, "eof")
				return
			}
		}
	}()

	// browser → PTY: binary = raw bytes, text = resize control.
	for {
		typ, data, rerr := ws.Read(ctx)
		if rerr != nil {
			return
		}
		if typ == websocket.MessageText {
			var m struct{ Cols, Rows int }
			if json.Unmarshal(data, &m) == nil && m.Cols > 0 && m.Rows > 0 {
				_ = s.cctx.Control.Resize(name, m.Cols, m.Rows)
			}
			continue
		}
		if _, werr := conn.Write(data); werr != nil {
			return
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

// Unwrap exposes the underlying ResponseWriter so http.ResponseController (used
// by coder/websocket's Accept to hijack the connection) can reach the real
// Hijacker through the accessLog statusWriter wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so the SSE handler, which runs inside
// the accessLog wrapper, can stream events instead of being buffered.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.cctx.Log.Info("http", "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "dur_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) openapi() map[string]any {
	paths := map[string]any{}
	components := map[string]any{}
	for _, c := range s.reg.Commands() {
		if c.HTTP == nil {
			continue
		}
		for name, schema := range c.Schemas {
			components[name] = schema
		}
		entry, _ := paths[c.HTTP.Path].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		operation := map[string]any{
			"summary":     c.Summary,
			"operationId": c.Path,
			"responses": map[string]any{
				"200":     map[string]any{"description": "Success", "content": map[string]any{"application/json": map[string]any{"schema": resultEnvelopeSchema(c.ResultSchema)}}},
				"default": map[string]any{"description": "Typed error", "content": map[string]any{"application/json": map[string]any{"schema": errorEnvelopeSchema()}}},
			},
		}
		parameters := []any{}
		argsByName := map[string]registry.Arg{}
		for _, arg := range c.Args {
			argsByName[arg.Name] = arg
		}
		pathNames := map[string]bool{}
		for _, name := range wildcardNames(c.HTTP.Path) {
			pathNames[name] = true
			schema := map[string]any{"type": "string"}
			if arg, ok := argsByName[name]; ok {
				schema = openAPIArgSchema(arg)
			}
			parameters = append(parameters, map[string]any{"name": name, "in": "path", "required": true, "schema": schema})
		}
		if c.HTTP.Method == http.MethodGet || c.HTTP.Method == http.MethodDelete {
			for _, arg := range c.Args {
				if !pathNames[arg.Name] {
					parameters = append(parameters, map[string]any{"name": arg.Name, "in": "query", "required": arg.Required, "schema": openAPIArgSchema(arg)})
				}
			}
		}
		if len(parameters) != 0 {
			operation["parameters"] = parameters
		}
		if c.HTTP.Method != http.MethodGet && c.HTTP.Method != http.MethodDelete {
			properties := map[string]any{}
			required := []string{}
			for _, arg := range c.Args {
				if pathNames[arg.Name] {
					continue
				}
				properties[arg.Name] = openAPIArgSchema(arg)
				if arg.Required {
					required = append(required, arg.Name)
				}
			}
			schema := map[string]any{"type": "object", "properties": properties}
			if len(required) != 0 {
				schema["required"] = required
			}
			operation["requestBody"] = map[string]any{"required": len(required) != 0, "content": map[string]any{"application/json": map[string]any{"schema": schema}}}
		}
		entry[toLowerMethod(c.HTTP.Method)] = operation
		paths[c.HTTP.Path] = entry
	}
	return map[string]any{
		"openapi":    "3.0.3",
		"info":       map[string]any{"title": "tariboyd", "version": s.cctx.Version},
		"paths":      paths,
		"components": map[string]any{"schemas": components},
	}
}

func openAPIArgSchema(arg registry.Arg) map[string]any {
	if arg.Schema != nil {
		return arg.Schema
	}
	typ := "string"
	if arg.Type == registry.Bool {
		typ = "boolean"
	} else if arg.Type == registry.Int {
		typ = "integer"
	}
	if arg.Repeatable {
		return map[string]any{"type": "array", "items": map[string]any{"type": typ}, "description": arg.Help}
	}
	return map[string]any{"type": typ, "description": arg.Help}
}

func resultEnvelopeSchema(result map[string]any) map[string]any {
	if result == nil {
		result = map[string]any{}
	}
	return map[string]any{"type": "object", "required": []string{"ok", "result"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}, "result": result}}
}
func errorEnvelopeSchema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"ok", "error"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}, "error": map[string]any{"type": "object", "required": []string{"code", "message"}, "properties": map[string]any{"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "details": map[string]any{"type": "object", "description": "May include current_revision and current state for revision_conflict"}}}}}
}

func toLowerMethod(m string) string {
	b := []byte(m)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func (s *Server) ServeUnix(sock string) error {
	// Rebuild the handler so a SetEventSource that ran after NewServer (which
	// cached a handler built without the SSE route) takes effect.
	handler := s.Handler()
	_ = os.Remove(sock) // stale socket from a previous run
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	s.serveMu.Lock()
	if s.stopping {
		s.serveMu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.http.Handler = handler
	s.serveMu.Unlock()
	err = s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// corsMiddleware adds permissive-but-scoped CORS headers so a browser SPA served
// from one daemon's origin can call THIS daemon cross-origin, and answers a
// preflight OPTIONS with 204 BEFORE auth (a preflight carries no Authorization
// header, so it must short-circuit ahead of AuthMiddleware). It is applied ONLY
// by ServeTCP — the loopback web listener (ServeWeb) and the unix socket
// (ServeUnix) never get CORS, so their private posture is unchanged. `*` does not
// weaken the boundary: the TCP listener still requires a valid bearer
// (AuthMiddleware), and no cookies are used (bearer header, not credentials mode),
// so a wildcard origin is legal and safe.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ServeTCP(addr, authToken string) error {
	handler := corsMiddleware(AuthMiddleware(authToken, s.Handler()))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.serveMu.Lock()
	if s.stopping {
		s.serveMu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.http.Handler = handler
	s.serveMu.Unlock()
	err = s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// isLoopbackWebHost reports whether the Host header names a loopback host the
// HTTP API listener may answer on, regardless of port. DNS-rebinding is
// defeated by the hostname check alone (an attacker's evil.com never resolves
// to "localhost" or a loopback IP in the header), so the local port a browser,
// SSH tunnel, or port-forward exposes is irrelevant and any port is accepted.
func isLoopbackWebHost(hostHeader string) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	} else {
		// No port present: strip IPv6 brackets if any ("[::1]" -> "::1").
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// webHostMiddleware rejects any request whose Host header is not a loopback
// host. This is defense-in-depth against DNS-rebinding on the unauthenticated
// loopback HTTP API listener: a browser pointed at http://evil.com:PORT
// (evil.com resolving to 127.0.0.1) sends Host: evil.com:PORT and is refused
// with 421, while 127.0.0.1 / localhost / ::1 on any port are allowed. It is
// NOT authentication — the loopback trust model is unchanged.
func webHostMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackWebHost(r.Host) {
			WriteErr(w, http.StatusMisdirectedRequest, "bad_host",
				"host "+r.Host+" is not an allowed loopback host for the HTTP API listener")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackOrigin reports whether an Origin header names a loopback host over
// plain http on any port ("http://localhost:9993", "http://127.0.0.1:9990",
// "http://[::1]:9990"). Anything else — a remote site, https, the "null" origin
// of a sandboxed/file:// document — is not loopback.
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return false
	}
	return isLoopbackWebHost(u.Host)
}

// desktopOrigin is the origin of the bundled SPA inside the macOS Tauri app.
// Tauri v2 serves the frontend from its own custom protocol, and the webview
// sends exactly this string. It is matched literally — no prefix or suffix
// matching — so no other host can borrow it.
const desktopOrigin = "tauri://localhost"

// isAllowedWebOrigin reports whether the unauthenticated loopback web listener
// may echo an Origin back. Two callers are legitimate: another daemon's SPA on
// some localhost port (the port-forward case) and the desktop app.
func isAllowedWebOrigin(origin string) bool {
	return origin == desktopOrigin || isLoopbackOrigin(origin)
}

// webCORSMiddleware lets a browser SPA served by one daemon's web listener call
// ANOTHER daemon's web listener, which is the normal shape once several daemons
// are port-forwarded to different localhost ports (UI on :9990 fetching :9993).
// It also lets the bundled SPA inside the macOS Tauri desktop app (origin
// tauri://localhost) call the loopback listener directly, since EventSource
// cannot be routed around CORS the way fetch can. Unlike corsMiddleware
// (ServeTCP) it never answers `*`: the web listener has no bearer auth, so it
// echoes back ONLY these explicitly allowed origins. A page on evil.com sends
// Origin: http://evil.com, gets no ACAO, and the browser blocks the read — the
// loopback trust model is unchanged, and this composes with the Host-header
// check that already defeats DNS-rebinding. Preflight OPTIONS is answered 204
// here so it never reaches a route that would 404/405 it.
func webCORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && isAllowedWebOrigin(origin)
		if allowed {
			// Vary: Origin — the response body is origin-independent but this
			// header is not, so caches must key on it.
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeWeb binds a loopback TCP listener serving the same Handler() (JSON API +
// websockets, no UI) with NO bearer auth, guarded by a Host-header allowlist
// (defense-in-depth against DNS-rebinding) and loopback-only CORS. The caller
// (daemon.Run) validates addr is loopback before calling this.
func (s *Server) ServeWeb(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// Host check outermost: a request with a non-loopback Host is refused 421
	// before CORS ever labels it, preflight included.
	webSrv := &http.Server{Handler: webHostMiddleware(webCORSMiddleware(s.Handler()))}
	s.serveMu.Lock()
	if s.stopping {
		s.serveMu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.webSrv = webSrv
	s.serveMu.Unlock()
	err = webSrv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.serveMu.Lock()
	s.stopping = true
	webSrv := s.webSrv
	s.serveMu.Unlock()
	if webSrv != nil {
		_ = webSrv.Shutdown(ctx)
	}
	return s.http.Shutdown(ctx)
}
