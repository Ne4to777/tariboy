// Package agentapi serves the per-agent capability socket (spec §8): loop done,
// whoami, context get/set, status. Routes are gated by the agent's plugins.
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/bus"
	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/version"
)

type Deps struct {
	Agent       string
	Cwd         string
	ContextPath string
	Plugins     []string
	// CurrentPlugins overrides the startup snapshot when image activation
	// changes capabilities between iterations.
	CurrentPlugins   func() []string
	CurrentIteration func() string
	SetDone          func(iterationID string, productive bool) error
	Status           func() (map[string]any, error)
	// SetStatus records the agent-authored status message and returns the saved
	// {message, updated}. Gated by the `status` plugin. Nil yields unavailable.
	SetStatus func(message string) (map[string]any, error)
	// SetGoal selects an assigned active Native Task and updates the current
	// iteration's live AI-proxy attribution. Nil yields unavailable.
	SetGoal func(key string) (map[string]any, error)

	// Bus surface (messages is CORE; nil hooks yield bus_unavailable).
	Publish           func(bus.Message) (bus.Message, error)
	Subscribe         func(channel string, matcher bus.Matcher, typeFilter []string) (bus.Subscription, error)
	Unsubscribe       func(id string) error
	ListSubscriptions func() ([]bus.Subscription, error)
	Channels          func() ([]bus.Channel, error)

	// ProvidedChannels returns the provider-declared channels drawn from installed
	// plugin manifests (spec §6.1). The Messages skill merges these into channel
	// list so provider channels are listed and annotated (provider, param keys,
	// help) even before their channel row exists. Nil is treated as "none".
	ProvidedChannels func() ([]ProvidedChannel, error)

	// Phase P messages surface (design §3.3, §4, §5): inbox listing, the
	// explicit processed/reply lifecycle, the request primitive, DLQ requeue,
	// parameterized subscriptions, and the channel-name unsubscribe fallback.
	// All nil hooks yield bus_unavailable.
	Inbox              func(status string, limit int, before string) ([]bus.InboxItem, error)
	MarkProcessed      func(msgID, result string) (bus.InboxItem, error)
	Reply              func(msgID, text string, data map[string]any, typeOverride string) (bus.Message, error)
	Request            func(channel, text, deadline string) (bus.Message, error)
	Requeue            func(msgID string) error
	SubscribeParams    func(channel string, matcher bus.Matcher, typeFilter []string, params map[string]any) (bus.Subscription, error)
	UnsubscribeChannel func(channel string) (int, error)

	// Unanswered is the agent's answering obligation across the chats it takes
	// part in, keyed by chat id (design: replying is a separate act from
	// processing a delivery).
	Unanswered func() (map[string][]bus.Message, error)

	// Schedule/script surface (OPTIONAL plugins: gated).
	AddSchedule        func(kind, spec, channel, template string) (map[string]any, error)
	ListSchedules      func() ([]map[string]any, error)
	CancelSchedule     func(id string) error
	RunScriptOnce      func(script.CreateOnce) (script.Definition, script.Run, error)
	ScheduleScript     func(script.CreateSchedule) (script.Definition, script.Run, error)
	RerunScript        func(scriptID string) (script.Run, error)
	ListScripts        func() ([]script.Definition, error)
	ListScriptRuns     func(scriptID string) ([]script.Run, error)
	GetScriptRun       func(runID string) (script.Run, error)
	LogScriptRun       func(runID string) (string, error)
	CancelScriptTarget func(id string) error
	RemoveScript       func(id string) error

	// Image authoring surface (OPTIONAL image-creator capability: gated). Nil
	// yields unavailable. BuildImage authors + builds a new image from a
	// Tariboyfile at the given path and calls image.Build.
	BuildImage func(name, tag, path string) (map[string]any, error)

	// Group coordination surface. These callbacks are daemon-wired so an agent
	// can coordinate only within its own group through the per-agent tools socket.
	GroupInfo    func() (map[string]any, error)
	GroupStatus  func(member string) (map[string]any, error)
	GroupSend    func(member, typ, text, deadline string) (map[string]any, error)
	GroupObserve func(member string, tail int) (map[string]any, error)
	GroupLoop    func(member, action string) (map[string]any, error)
	LoopControl  func(action string) (map[string]any, error)
	// TaskAction is the native Tasks surface. The daemon binds the caller from
	// this socket's Agent field; request bodies never carry identity.
	TaskAction func(action string, body map[string]any) (any, error)
}

// ProvidedChannel is the daemon-independent view of one plugin provided-channel
// declaration surfaced by the Messages skill's sources command (spec §6.1).
// from plugin manifests; agentapi stays decoupled from the plugins package.
type ProvidedChannel struct {
	Channel  string   `json:"channel"`
	Provider string   `json:"provider"`
	Params   []string `json:"params,omitempty"`
	Help     string   `json:"help,omitempty"`
}

type Server struct {
	d     Deps
	http  *http.Server
	ctxMu sync.Mutex // guards ContextPath (CONTEXT.md) reads/writes
}

func NewServer(d Deps) *Server {
	s := &Server{d: d}
	s.http = &http.Server{Handler: s.Handler()}
	return s
}

func (s *Server) has(plugin string) bool {
	plugins := s.d.Plugins
	if s.d.CurrentPlugins != nil {
		plugins = s.d.CurrentPlugins()
	}
	for _, p := range plugins {
		if p == plugin {
			return true
		}
	}
	return false
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Built-in capability routes are explicit, including the historical core.
	mux.HandleFunc("GET /tools/whoami", s.gated("whoami", func(w http.ResponseWriter, r *http.Request) {
		api.WriteOK(w, map[string]any{
			"agent": s.d.Agent, "cwd": s.d.Cwd, "iteration": s.d.CurrentIteration(),
			// The daemon's own build, so the Whoami skill can print it next to the
			// version of the client that asked (SUPER-224 §4).
			"daemon_version": version.Version,
		})
	}))
	completeLoop := func(w http.ResponseWriter, r *http.Request) {
		id := s.d.CurrentIteration()
		if id == "" {
			api.WriteErr(w, http.StatusConflict, "no_iteration", "no iteration is currently running")
			return
		}
		// `i-am-done --idle` marks the iteration idle (not productive); a plain
		// done leaves it productive. productive = !idle.
		var body struct {
			Idle bool `json:"idle"`
		}
		if err := decodeBody(r, &body); err != nil {
			api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if err := s.d.SetDone(id, !body.Idle); err != nil {
			api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		api.WriteOK(w, map[string]any{"done": true, "iteration": id, "productive": !body.Idle})
	}
	mux.HandleFunc("POST /tools/loop/done", s.gated("loop", completeLoop))
	// `complete` is the socket-native spelling for harnesses that are themselves
	// running under a supervising agent. It has the same authenticated,
	// per-agent semantics as the CLI-facing `loop done` route, without invoking
	// an ambient i-am-done command in the parent process tree.
	mux.HandleFunc("POST /tools/loop/complete", s.gated("loop", completeLoop))
	mux.HandleFunc("POST /tools/loop/control", s.gated("loop", s.loopControl))

	// Messages capability surface.
	mux.HandleFunc("POST /tools/message/send", s.gated("messages", s.messageSend))
	mux.HandleFunc("GET /tools/message/ls", s.gated("messages", s.messageLs))
	mux.HandleFunc("POST /tools/message/processed", s.gated("messages", s.messageProcessed))
	mux.HandleFunc("POST /tools/message/reply", s.gated("messages", s.messageReply))
	mux.HandleFunc("GET /tools/chat/unanswered", s.gated("messages", s.chatUnanswered))
	mux.HandleFunc("GET /tools/message/dlq", s.gated("messages", s.messageDLQ))
	mux.HandleFunc("POST /tools/message/dlq/requeue", s.gated("messages", s.messageDLQRequeue))
	mux.HandleFunc("POST /tools/request", s.gated("messages", s.request))
	mux.HandleFunc("POST /tools/channel/subscribe", s.gated("messages", s.channelSubscribe))
	mux.HandleFunc("POST /tools/channel/unsubscribe", s.gated("messages", s.channelUnsubscribe))
	mux.HandleFunc("GET /tools/channel/ls", s.gated("messages", s.channelLs))
	mux.HandleFunc("GET /tools/sources", s.gated("messages", s.sources))

	// Optional routes: gated by plugin membership.
	mux.HandleFunc("GET /tools/context/get", s.gated("context", s.contextGet))
	mux.HandleFunc("POST /tools/context/set", s.gated("context", s.contextSet))
	mux.HandleFunc("GET /tools/status", s.gated("status", s.status))
	mux.HandleFunc("POST /tools/status/set", s.gated("status", s.statusSet))

	// Schedule/script surface (OPTIONAL plugins: gated).
	mux.HandleFunc("POST /tools/schedule/add", s.gated("schedule", s.scheduleAdd))
	mux.HandleFunc("GET /tools/schedule/ls", s.gated("schedule", s.scheduleLs))
	mux.HandleFunc("POST /tools/schedule/cancel", s.gated("schedule", s.scheduleCancel))
	mux.HandleFunc("POST /tools/script/run", s.gated("scripts", s.scriptRunOnce))
	mux.HandleFunc("POST /tools/script/schedule", s.gated("scripts", s.scriptSchedule))
	mux.HandleFunc("POST /tools/script/rerun", s.gated("scripts", s.scriptRerun))
	mux.HandleFunc("GET /tools/script/ls", s.gated("scripts", s.scriptLs))
	mux.HandleFunc("GET /tools/script/runs/{id}", s.gated("scripts", s.scriptRuns))
	mux.HandleFunc("GET /tools/script/logs/{id}", s.gated("scripts", s.scriptLogs))
	mux.HandleFunc("POST /tools/script/cancel", s.gated("scripts", s.scriptCancel))
	mux.HandleFunc("POST /tools/script/rm", s.gated("scripts", s.scriptRemove))
	mux.HandleFunc("POST /tools/image/build", s.gated("image-creator", s.imageBuild))
	mux.HandleFunc("POST /tools/goal/set", s.gated("goal", s.goalSet))
	mux.HandleFunc("POST /tools/tasks/{action}", s.gated("tasks", s.nativeTaskAction))
	mux.HandleFunc("GET /tools/group/info", s.groupInfo)
	mux.HandleFunc("GET /tools/group/status", s.groupStatus)
	mux.HandleFunc("GET /tools/group/status/{member}", s.groupStatus)
	mux.HandleFunc("POST /tools/group/send", s.groupSend)
	mux.HandleFunc("POST /tools/group/request", s.groupRequest)
	mux.HandleFunc("GET /tools/group/observe/{member}", s.groupObserve)
	mux.HandleFunc("POST /tools/group/loop", s.groupLoop)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		api.WriteErr(w, http.StatusNotFound, "not_found", "unknown agent route "+r.Method+" "+r.URL.Path)
	})
	// One wrapper for the whole agent-facing surface: every response carries the
	// daemon's version, so a shim pinned to an older build can notice the drift.
	return api.VersionHeader(mux)
}

func (s *Server) goalSet(w http.ResponseWriter, r *http.Request) {
	if s.d.SetGoal == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "Goal selection is unavailable")
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.Key = strings.TrimSpace(body.Key)
	if body.Key == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_key", "key is required")
		return
	}
	result, err := s.d.SetGoal(body.Key)
	if err != nil {
		api.WriteErr(w, http.StatusBadRequest, "goal_failed", err.Error())
		return
	}
	api.WriteOK(w, result)
}

func (s *Server) nativeTaskAction(w http.ResponseWriter, r *http.Request) {
	if s.d.TaskAction == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "native Tasks service is unavailable")
		return
	}
	body := map[string]any{}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	for _, key := range []string{"author", "actor", "customer", "principal_identity", "iteration", "iteration_id"} {
		delete(body, key)
	}
	// `principal` is an action argument for `ask`, never caller identity. Keep it
	// only for that action; scrub it everywhere else.
	action := r.PathValue("action")
	if action != "ask" {
		delete(body, "principal")
	}
	result, err := s.d.TaskAction(action, body)
	if err != nil {
		var domain *tasks.Error
		if errors.As(err, &domain) {
			api.WriteErr(w, domain.Status, domain.Code, domain.Msg)
			return
		}
		api.WriteErr(w, http.StatusBadRequest, "task_failed", err.Error())
		return
	}
	api.WriteOK(w, result)
}

func (s *Server) loopControl(w http.ResponseWriter, r *http.Request) {
	if s.d.LoopControl == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "loop control is not available")
		return
	}
	var body struct{ Action string }
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Action != "start" && body.Action != "stop" {
		api.WriteErr(w, http.StatusBadRequest, "bad_action", "action must be start or stop")
		return
	}
	res, err := s.d.LoopControl(body.Action)
	if err != nil {
		api.WriteErr(w, http.StatusBadRequest, "loop_failed", err.Error())
		return
	}
	api.WriteOK(w, res)
}

func (s *Server) gated(plugin string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.has(plugin) {
			api.WriteErr(w, http.StatusNotFound, "plugin_disabled",
				"command not available: plugin '"+plugin+"' not enabled for this agent")
			return
		}
		h(w, r)
	}
}

func (s *Server) contextGet(w http.ResponseWriter, r *http.Request) {
	s.ctxMu.Lock()
	data, err := os.ReadFile(s.d.ContextPath)
	s.ctxMu.Unlock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	api.WriteOK(w, map[string]any{"text": string(data)})
}

func (s *Server) contextSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
	}
	if err := s.writeContextAtomic([]byte(body.Text)); err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	api.WriteOK(w, map[string]any{"saved": true})
}

// writeContextAtomic writes data to ContextPath atomically: it creates a
// temp file in the same directory, writes and closes it, then renames it
// over the destination. This avoids concurrent readers ever observing a
// partially-written or corrupted CONTEXT.md, and the mutex serializes
// concurrent writers so the final content is always exactly one full write.
func (s *Server) writeContextAtomic(data []byte) error {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()

	dir := filepath.Dir(s.d.ContextPath)
	tmp, err := os.CreateTemp(dir, ".context-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.d.ContextPath)
}

func (s *Server) messageSend(w http.ResponseWriter, r *http.Request) {
	if s.d.Publish == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	var body struct {
		Channel string         `json:"channel"`
		Type    string         `json:"type"`
		Subject map[string]any `json:"subject"`
		Text    string         `json:"text"`
		Data    map[string]any `json:"data"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Channel == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_channel", "channel is required")
		return
	}
	msg, err := s.d.Publish(bus.Message{
		Channel: body.Channel, Type: body.Type, Subject: body.Subject, Text: body.Text, Data: body.Data,
		Source: "agent:" + s.d.Agent, ProducedByAgent: s.d.Agent, ProducedInIteration: s.d.CurrentIteration(),
	})
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "publish_failed", err.Error())
		return
	}
	api.WriteOK(w, map[string]any{"id": msg.ID, "channel": msg.Channel, "sent": true})
}

func (s *Server) channelSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.d.Subscribe == nil && s.d.SubscribeParams == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	var body struct {
		Channel string            `json:"channel"`
		Matcher map[string]string `json:"matcher"`
		Type    string            `json:"type"` // comma list of type globs
		Params  map[string]any    `json:"params"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Channel == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_channel", "channel is required")
		return
	}
	// Parameterized subscriptions describe provider work (§5.1); they need the
	// params-aware hook. A plain sub prefers SubscribeParams (which folds to
	// Subscribe on empty params) and falls back to Subscribe when unwired.
	var sub bus.Subscription
	var err error
	switch {
	case len(body.Params) > 0:
		if s.d.SubscribeParams == nil {
			api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "parameterized subscriptions are not available")
			return
		}
		sub, err = s.d.SubscribeParams(body.Channel, bus.Matcher(body.Matcher), splitCSV(body.Type), body.Params)
	case s.d.SubscribeParams != nil:
		sub, err = s.d.SubscribeParams(body.Channel, bus.Matcher(body.Matcher), splitCSV(body.Type), nil)
	default:
		sub, err = s.d.Subscribe(body.Channel, bus.Matcher(body.Matcher), splitCSV(body.Type))
	}
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "subscribe_failed", err.Error())
		return
	}
	res := map[string]any{"id": sub.ID, "channel": sub.Channel}
	if sub.Watch != "" {
		res["watch"] = sub.Watch
	}
	if len(sub.Params) > 0 {
		res["params"] = sub.Params
	}
	api.WriteOK(w, res)
}

// channelUnsubscribe removes by subscription id (the stable form), and — when
// no such id exists — falls back to treating the argument as a channel name,
// dropping all of the agent's own subscriptions on that channel (§5.3).
func (s *Server) channelUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.d.Unsubscribe == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.ID == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_id", "subscription id (or channel name) is required")
		return
	}
	err := s.d.Unsubscribe(body.ID)
	if err == nil {
		api.WriteOK(w, map[string]any{"unsubscribed": body.ID})
		return
	}
	// Unknown id → try the channel-name fallback before reporting not-found.
	if errors.Is(err, bus.ErrNotFound) && s.d.UnsubscribeChannel != nil {
		if n, cerr := s.d.UnsubscribeChannel(body.ID); cerr == nil {
			api.WriteOK(w, map[string]any{"unsubscribed": body.ID, "removed": n})
			return
		}
	}
	api.WriteErr(w, http.StatusNotFound, "not_found", err.Error())
}

func (s *Server) channelLs(w http.ResponseWriter, r *http.Request) {
	if s.d.ListSubscriptions == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	subs, err := s.d.ListSubscriptions()
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	rows := make([]map[string]any, 0, len(subs))
	for _, sub := range subs {
		row := map[string]any{"id": sub.ID, "channel": sub.Channel,
			"matcher": sub.Matcher, "type_filter": sub.TypeFilter}
		if sub.Watch != "" {
			row["watch"] = sub.Watch
		}
		if len(sub.Params) > 0 {
			row["params"] = sub.Params
		}
		if sub.Locked {
			row["locked"] = true
		}
		rows = append(rows, row)
	}
	api.WriteOK(w, map[string]any{"subscriptions": rows, "count": len(rows)})
}

// inboxRows renders inbox items for the CLI: immutable message fields plus the
// per-agent delivery state. Optional threading/state fields are omitted when
// empty so the common (plain, pending) row stays terse.
func inboxRows(items []bus.InboxItem) []map[string]any {
	rows := make([]map[string]any, 0, len(items))
	for _, it := range items {
		row := map[string]any{
			"id": it.ID, "channel": it.Channel, "ts": it.TS, "source": it.Source,
			"type": it.Type, "text": it.Text, "attempts": it.Attempts, "dlq": it.DLQ,
		}
		if it.Kind != "" {
			row["kind"] = it.Kind
		}
		if len(it.Subject) > 0 {
			row["subject"] = it.Subject
		}
		if it.CorrelationID != "" {
			row["correlation_id"] = it.CorrelationID
		}
		if it.InReplyTo != "" {
			row["in_reply_to"] = it.InReplyTo
		}
		if it.ReplyTo != "" {
			row["reply_to"] = it.ReplyTo
		}
		if it.Deadline != "" {
			row["deadline"] = it.Deadline
		}
		if it.ProcessedAt != "" {
			row["processed_at"] = it.ProcessedAt
			row["result"] = it.Result
		}
		rows = append(rows, row)
	}
	return rows
}

// chatUnanswered lists, per chat, the messages this agent has not replied to
// yet. A delivery marked processed is NOT an answer: the conversation is
// answered only by a reply published into it.
func (s *Server) chatUnanswered(w http.ResponseWriter, r *http.Request) {
	if s.d.Unanswered == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	queues, err := s.d.Unanswered()
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	chats := make([]map[string]any, 0, len(queues))
	total := 0
	for chatID, msgs := range queues {
		rows := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			rows = append(rows, map[string]any{"id": m.ID, "channel": m.Channel, "ts": m.TS,
				"from": bus.MessageFrom(m), "type": m.Type, "text": m.Text})
		}
		chats = append(chats, map[string]any{"chat": chatID, "messages": rows, "count": len(rows)})
		total += len(rows)
	}
	sort.Slice(chats, func(i, j int) bool {
		return chats[i]["chat"].(string) < chats[j]["chat"].(string)
	})
	api.WriteOK(w, map[string]any{"chats": chats, "count": total})
}

// messageLs lists the agent's own inbox: pending by default, or the whole
// archive (pending + processed + dlq) with ?all=true (§7).
func (s *Server) messageLs(w http.ResponseWriter, r *http.Request) {
	if s.d.Inbox == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	status := "pending"
	if all, _ := strconv.ParseBool(r.URL.Query().Get("all")); all {
		status = "all"
	}
	items, err := s.d.Inbox(status, 0, "")
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	rows := inboxRows(items)
	api.WriteOK(w, map[string]any{"messages": rows, "count": len(rows)})
}

// messageProcessed is the explicit ack: a mandatory result is recorded and the
// message drains from the pending queue (§3.3). Empty result is rejected.
func (s *Server) messageProcessed(w http.ResponseWriter, r *http.Request) {
	if s.d.MarkProcessed == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	var body struct {
		ID     string `json:"id"`
		Result string `json:"result"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.ID == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_id", "message id is required")
		return
	}
	if strings.TrimSpace(body.Result) == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_result",
			`a result is required: scripts/messages.sh message processed <id> "<result>"`)
		return
	}
	item, err := s.d.MarkProcessed(body.ID, body.Result)
	if err != nil {
		if errors.Is(err, bus.ErrNotFound) {
			api.WriteErr(w, http.StatusNotFound, "not_found", "no such message in your inbox: "+body.ID)
			return
		}
		api.WriteErr(w, http.StatusInternalServerError, "processed_failed", err.Error())
		return
	}
	api.WriteOK(w, map[string]any{"id": item.ID, "processed": true, "result": item.Result})
}

// messageReply publishes a threaded reply to a message, which auto-processes the
// original for this agent (§4.1).
func (s *Server) messageReply(w http.ResponseWriter, r *http.Request) {
	if s.d.Reply == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	var body struct {
		ID   string         `json:"id"`
		Text string         `json:"text"`
		Data map[string]any `json:"data"`
		Type string         `json:"type"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.ID == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_id", "message id is required")
		return
	}
	reply, err := s.d.Reply(body.ID, body.Text, body.Data, body.Type)
	if err != nil {
		if errors.Is(err, bus.ErrNotFound) {
			api.WriteErr(w, http.StatusNotFound, "not_found", "no such message: "+body.ID)
			return
		}
		api.WriteErr(w, http.StatusInternalServerError, "reply_failed", err.Error())
		return
	}
	api.WriteOK(w, map[string]any{"id": reply.ID, "channel": reply.Channel,
		"in_reply_to": reply.InReplyTo, "correlation_id": reply.CorrelationID, "replied": true})
}

// request publishes a request primitive to a channel, optionally arming a
// deadline timeout that fires into the requester's inbox (§4.2).
func (s *Server) request(w http.ResponseWriter, r *http.Request) {
	if s.d.Request == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	var body struct {
		Channel  string `json:"channel"`
		Text     string `json:"text"`
		Deadline string `json:"deadline"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Channel == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_channel", "channel is required")
		return
	}
	req, err := s.d.Request(body.Channel, body.Text, body.Deadline)
	if errors.Is(err, bus.ErrDeadlineUnsupported) {
		api.WriteErr(w, http.StatusBadRequest, "deadline_unsupported", err.Error())
		return
	}
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "request_failed", err.Error())
		return
	}
	api.WriteOK(w, map[string]any{"id": req.ID, "channel": req.Channel,
		"correlation_id": req.CorrelationID, "requested": true})
}

// messageDLQ lists the agent's dead-lettered messages (§3.3).
func (s *Server) messageDLQ(w http.ResponseWriter, r *http.Request) {
	if s.d.Inbox == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	items, err := s.d.Inbox("dlq", 0, "")
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	rows := inboxRows(items)
	api.WriteOK(w, map[string]any{"messages": rows, "count": len(rows)})
}

// messageDLQRequeue clears the DLQ flag and resets attempts, returning a
// message to the pending queue (§3.3).
func (s *Server) messageDLQRequeue(w http.ResponseWriter, r *http.Request) {
	if s.d.Requeue == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.ID == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_id", "message id is required")
		return
	}
	if err := s.d.Requeue(body.ID); err != nil {
		if errors.Is(err, bus.ErrNotFound) {
			api.WriteErr(w, http.StatusNotFound, "not_found", "no such message in your inbox: "+body.ID)
			return
		}
		api.WriteErr(w, http.StatusInternalServerError, "requeue_failed", err.Error())
		return
	}
	api.WriteOK(w, map[string]any{"id": body.ID, "requeued": true})
}

func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	if s.d.Channels == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "bus_unavailable", "channel bus is not available")
		return
	}
	chans, err := s.d.Channels()
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Merge existing channel rows with provider declarations from plugin
	// manifests (spec §6.1). Provider channels are annotated (provider, param
	// keys, help) and listed even before their channel row exists.
	byName := make(map[string]map[string]any, len(chans))
	rows := make([]map[string]any, 0, len(chans))
	for _, c := range chans {
		row := map[string]any{"name": c.Name, "kind": c.Kind}
		byName[c.Name] = row
		rows = append(rows, row)
	}
	if s.d.ProvidedChannels != nil {
		provided, err := s.d.ProvidedChannels()
		if err != nil {
			api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		for _, p := range provided {
			row, ok := byName[p.Channel]
			if !ok {
				row = map[string]any{"name": p.Channel, "kind": bus.ChannelKind(p.Channel)}
				byName[p.Channel] = row
				rows = append(rows, row)
			}
			row["provider"] = p.Provider
			if len(p.Params) > 0 {
				row["params"] = p.Params
			}
			if p.Help != "" {
				row["help"] = p.Help
			}
		}
	}
	// Sort unconditionally so channel order is deterministic regardless of
	// whether the provider dep is wired (review finding #5).
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["name"].(string) < rows[j]["name"].(string)
	})
	api.WriteOK(w, map[string]any{"channels": rows, "count": len(rows)})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	out, err := s.d.Status()
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	api.WriteOK(w, out)
}

func (s *Server) statusSet(w http.ResponseWriter, r *http.Request) {
	if s.d.SetStatus == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "status set is not available")
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
	}
	out, err := s.d.SetStatus(body.Message)
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	api.WriteOK(w, out)
}

func (s *Server) scheduleAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind, Spec, Channel string
		Message             json.RawMessage `json:"message"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	tpl := "{}"
	if len(body.Message) > 0 {
		tpl = string(body.Message)
	}
	res, err := s.d.AddSchedule(body.Kind, body.Spec, body.Channel, tpl)
	if err != nil {
		api.WriteErr(w, http.StatusBadRequest, "schedule_failed", err.Error())
		return
	}
	api.WriteOK(w, res)
}

func (s *Server) scheduleLs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.d.ListSchedules()
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	api.WriteOK(w, map[string]any{"schedules": rows, "count": len(rows)})
}

func (s *Server) scheduleCancel(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID string }
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if err := s.d.CancelSchedule(body.ID); err != nil {
		api.WriteErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	api.WriteOK(w, map[string]any{"cancelled": body.ID})
}

func (s *Server) scriptRunOnce(w http.ResponseWriter, r *http.Request) {
	if s.d.RunScriptOnce == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "scripts are not available")
		return
	}
	var body script.CreateOnce
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Description == "" {
		body.Description = body.Name
	}
	definition, run, err := s.d.RunScriptOnce(body)
	if err != nil {
		writeScriptError(w, err)
		return
	}
	api.WriteOK(w, map[string]any{"script": scriptView(definition), "run": scriptRunView(run)})
}

func (s *Server) scriptSchedule(w http.ResponseWriter, r *http.Request) {
	if s.d.ScheduleScript == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "scripts are not available")
		return
	}
	var body script.CreateSchedule
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Description == "" {
		body.Description = body.Name
	}
	definition, run, err := s.d.ScheduleScript(body)
	if err != nil {
		writeScriptError(w, err)
		return
	}
	api.WriteOK(w, map[string]any{"script": scriptView(definition), "run": scriptRunView(run)})
}

func (s *Server) scriptLs(w http.ResponseWriter, r *http.Request) {
	if s.d.ListScripts == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "scripts are not available")
		return
	}
	records, err := s.d.ListScripts()
	if err != nil {
		api.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	rows := make([]map[string]any, 0, len(records))
	for _, definition := range records {
		rows = append(rows, scriptView(definition))
	}
	api.WriteOK(w, map[string]any{"scripts": rows, "count": len(rows)})
}

func (s *Server) scriptRerun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if s.d.RerunScript == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "scripts are not available")
		return
	}
	run, err := s.d.RerunScript(body.ID)
	if err != nil {
		writeScriptError(w, err)
		return
	}
	api.WriteOK(w, scriptRunView(run))
}

func (s *Server) scriptRuns(w http.ResponseWriter, r *http.Request) {
	if s.d.ListScriptRuns == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "scripts are not available")
		return
	}
	runs, err := s.d.ListScriptRuns(r.PathValue("id"))
	if err != nil {
		writeScriptError(w, err)
		return
	}
	rows := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, scriptRunView(run))
	}
	api.WriteOK(w, map[string]any{"runs": rows, "count": len(rows)})
}

func (s *Server) scriptLogs(w http.ResponseWriter, r *http.Request) {
	if s.d.GetScriptRun == nil || s.d.LogScriptRun == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "scripts are not available")
		return
	}
	run, err := s.d.GetScriptRun(r.PathValue("id"))
	if err != nil {
		writeScriptError(w, err)
		return
	}
	logText, err := s.d.LogScriptRun(run.ID)
	if err != nil {
		writeScriptError(w, err)
		return
	}
	api.WriteOK(w, map[string]any{"run": scriptRunView(run), "log": logText})
}

func (s *Server) scriptCancel(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID string }
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if s.d.CancelScriptTarget == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "scripts are not available")
		return
	}
	if err := s.d.CancelScriptTarget(body.ID); err != nil {
		writeScriptError(w, err)
		return
	}
	api.WriteOK(w, map[string]any{"id": body.ID, "cancelled": true})
}

func (s *Server) scriptRemove(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID string }
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if s.d.RemoveScript == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "scripts are not available")
		return
	}
	if err := s.d.RemoveScript(body.ID); err != nil {
		if errors.Is(err, script.ErrActive) {
			api.WriteErr(w, http.StatusConflict, "script_active", "cannot remove an active script; cancel it first")
			return
		}
		api.WriteErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	api.WriteOK(w, map[string]any{"id": body.ID, "removed": true})
}

func scriptView(definition script.Definition) map[string]any {
	row := map[string]any{"id": definition.ID, "agent": definition.Agent, "name": definition.Name, "description": definition.Description,
		"command": definition.Command, "mode": definition.Mode, "interval_seconds": definition.IntervalSeconds, "state": definition.State,
		"created_at": definition.CreatedAt, "next_run_at": definition.NextRunAt}
	if definition.LatestRun != nil {
		row["latest_run"] = scriptRunView(*definition.LatestRun)
	}
	return row
}

func scriptRunView(run script.Run) map[string]any {
	row := map[string]any{"id": run.ID, "script_id": run.ScriptID, "agent": run.Agent, "status": run.Status,
		"created_at": run.CreatedAt, "started_at": run.StartedAt, "finished_at": run.FinishedAt, "log_path": run.LogPath}
	if run.PID != nil {
		row["pid"] = *run.PID
	}
	if run.ExitCode != nil {
		row["exit_code"] = *run.ExitCode
	}
	return row
}

func writeScriptError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, script.ErrNotFound):
		api.WriteErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, script.ErrActive):
		api.WriteErr(w, http.StatusConflict, "script_active", err.Error())
	case errors.Is(err, script.ErrMode):
		api.WriteErr(w, http.StatusConflict, "script_mode", err.Error())
	case errors.Is(err, script.ErrConflict):
		api.WriteErr(w, http.StatusConflict, "script_conflict", err.Error())
	default:
		api.WriteErr(w, http.StatusBadRequest, "invalid_script", err.Error())
	}
}

func (s *Server) imageBuild(w http.ResponseWriter, r *http.Request) {
	if s.d.BuildImage == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "image build is not available")
		return
	}
	var body struct{ Name, Tag, Path string }
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Name == "" || body.Path == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_args", "name and path are required")
		return
	}
	// An empty tag is passed through: the builder then publishes the source
	// image_version plus latest, exactly as the operator CLI does.
	res, err := s.d.BuildImage(body.Name, body.Tag, body.Path)
	if err != nil {
		api.WriteErr(w, http.StatusBadRequest, "build_failed", err.Error())
		return
	}
	api.WriteOK(w, res)
}

func (s *Server) groupInfo(w http.ResponseWriter, r *http.Request) {
	if s.d.GroupInfo == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "group tools are not available")
		return
	}
	res, err := s.d.GroupInfo()
	if err != nil {
		api.WriteErr(w, http.StatusBadRequest, "group_failed", err.Error())
		return
	}
	api.WriteOK(w, res)
}

func (s *Server) groupStatus(w http.ResponseWriter, r *http.Request) {
	if s.d.GroupStatus == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "group tools are not available")
		return
	}
	res, err := s.d.GroupStatus(r.PathValue("member"))
	if err != nil {
		api.WriteErr(w, http.StatusBadRequest, "group_failed", err.Error())
		return
	}
	api.WriteOK(w, res)
}

func (s *Server) groupSend(w http.ResponseWriter, r *http.Request) {
	s.groupSendTyped(w, r, "group.message")
}

func (s *Server) groupRequest(w http.ResponseWriter, r *http.Request) {
	s.groupSendTyped(w, r, "group.request")
}

func (s *Server) groupSendTyped(w http.ResponseWriter, r *http.Request, typ string) {
	if s.d.GroupSend == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "group tools are not available")
		return
	}
	var body struct {
		Member   string `json:"member"`
		Text     string `json:"text"`
		Deadline string `json:"deadline"`
	}
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Member == "" || body.Text == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_args", "member and text are required")
		return
	}
	res, err := s.d.GroupSend(body.Member, typ, body.Text, body.Deadline)
	if err != nil {
		api.WriteErr(w, http.StatusBadRequest, "group_failed", err.Error())
		return
	}
	api.WriteOK(w, res)
}

func (s *Server) groupObserve(w http.ResponseWriter, r *http.Request) {
	if s.d.GroupObserve == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "group tools are not available")
		return
	}
	tail := 80
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			api.WriteErr(w, http.StatusBadRequest, "bad_tail", "tail must be a positive integer")
			return
		}
		tail = n
	}
	res, err := s.d.GroupObserve(r.PathValue("member"), tail)
	if err != nil {
		api.WriteErr(w, http.StatusBadRequest, "group_failed", err.Error())
		return
	}
	api.WriteOK(w, res)
}

func (s *Server) groupLoop(w http.ResponseWriter, r *http.Request) {
	if s.d.GroupLoop == nil {
		api.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "group tools are not available")
		return
	}
	var body struct{ Member, Action string }
	if err := decodeBody(r, &body); err != nil {
		api.WriteErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Member == "" {
		api.WriteErr(w, http.StatusBadRequest, "missing_member", "member is required")
		return
	}
	if body.Action != "start" && body.Action != "stop" {
		api.WriteErr(w, http.StatusBadRequest, "bad_action", "action must be start or stop")
		return
	}
	res, err := s.d.GroupLoop(body.Member, body.Action)
	if err != nil {
		api.WriteErr(w, http.StatusBadRequest, "group_failed", err.Error())
		return
	}
	api.WriteOK(w, res)
}

// Listen binds the unix socket. Split out from Serve so callers can bind
// synchronously (surfacing a bind failure as an error) before serving in a
// goroutine, instead of losing the error inside a fire-and-forget Serve.
func (s *Server) Listen(sock string) (net.Listener, error) {
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// ServeListener serves HTTP on an already-bound listener until Shutdown.
func (s *Server) ServeListener(ln net.Listener) error {
	err := s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Serve binds and serves in one call (Listen + ServeListener).
func (s *Server) Serve(sock string) error {
	ln, err := s.Listen(sock)
	if err != nil {
		return err
	}
	return s.ServeListener(ln)
}

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func decodeBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
