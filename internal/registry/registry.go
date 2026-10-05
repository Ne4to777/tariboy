// Package registry is the single source of truth for every user-facing
// command: the CLI parser, the daemon HTTP router, --help-json and
// /api/openapi.json are all derived from registered Commands.
package registry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/bus"
	"github.com/alekzonder/tariboy/internal/maintenance"
	"github.com/alekzonder/tariboy/internal/retention"
	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/shim"
	"github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

type ArgType string

const (
	String      ArgType = "string"
	Bool        ArgType = "bool"
	Int         ArgType = "int"
	IntegerList ArgType = "integer-list"
	SecretFile  ArgType = "secret-file"
	JSONObject  ArgType = "json-object"
)

type Arg struct {
	Name       string
	Flag       string
	Short      string // explicit one-letter alias, e.g. "a" for -a
	Type       ArgType
	Default    any
	Required   bool
	Repeatable bool
	Help       string
	// Schema optionally supplies the OpenAPI JSON schema for HTTP-only object or
	// array inputs that do not have a CLI scalar representation.
	Schema map[string]any
}

type HTTPRoute struct {
	Method, Path string
	// MaxBodyBytes bounds JSON decoding when positive; zero preserves the route default.
	MaxBodyBytes int64 `json:"-"`
}

type Params map[string]any

const RequestContextParam = "__tariboy_request_context"

// RequestContext returns the transport context injected by the HTTP router.
// Local/CLI handlers receive Background when no transport context exists.
func RequestContext(p Params) context.Context {
	if ctx, ok := p[RequestContextParam].(context.Context); ok && ctx != nil {
		return ctx
	}
	return context.Background()
}

type Ctx struct {
	Store   *store.Store
	Log     *slog.Logger
	BaseDir string
	Socket  string // daemon unix socket path (for CLI-local composite commands)
	// HTTPAddr is the loopback host:port of the API/WS listener, empty when the
	// daemon runs socket-only. The desktop app reads it from the status payload
	// to build a base URL for a daemon it adopted rather than started.
	HTTPAddr    string
	Version     string
	StartedAt   time.Time
	Control     ServiceControl
	Scripts     ScriptControl
	Bus         *bus.Bus
	Plugins     PluginControl
	Groups      GroupControl
	Operator    string
	Retention   *retention.RetentionAPI
	Maintenance *maintenance.Service
	Tasks       TaskControl
	// WorkflowImages is the workflow image store; the prompt preview reads
	// status instructions from it.
	WorkflowImages *workflowimage.Store
}

// TaskControl is the daemon-owned native Tasks surface consumed by typed HTTP
// commands and identity-bound agent adapters.
type TaskControl interface {
	CustomerLogin() string
	CreateQueue(context.Context, tasks.Actor, tasks.CreateQueueInput) (tasks.Queue, error)
	ListQueues(context.Context, tasks.Actor) ([]tasks.Queue, error)
	GetQueue(context.Context, tasks.Actor, string) (tasks.Queue, error)
	UpdateQueue(context.Context, tasks.Actor, string, tasks.UpdateQueueInput) (tasks.Queue, error)
	CreateTask(context.Context, tasks.Actor, tasks.CreateTaskInput) (tasks.Task, error)
	GetTask(context.Context, tasks.Actor, string) (tasks.TaskDetail, error)
	ListTasks(context.Context, tasks.Actor, tasks.ListFilter) (tasks.TaskPage, error)
	UpdateTask(context.Context, tasks.Actor, string, tasks.UpdateTaskInput) (tasks.Task, error)
	MoveTask(context.Context, tasks.Actor, string, tasks.MoveInput) (tasks.Task, error)
	CompleteTask(context.Context, tasks.Actor, string, tasks.CompleteInput) (tasks.Task, error)
	ClaimTask(context.Context, tasks.Actor, string, int64) (tasks.Task, error)
	ExportTask(context.Context, tasks.Actor, string) (tasks.TransferBundle, error)
	ImportTask(context.Context, tasks.Actor, tasks.TransferBundle) (tasks.Task, error)
	Ready(context.Context, tasks.Actor, tasks.ReadyFilter) ([]tasks.Task, error)
	ClaimReady(context.Context, tasks.Actor, tasks.ReadyFilter, string) (tasks.Task, error)
	AddComment(context.Context, tasks.Actor, string, tasks.AddCommentInput) (tasks.CommentResult, error)
	AddRelation(context.Context, tasks.Actor, string, tasks.RelationInput) (tasks.Relation, error)
	DeleteRelation(context.Context, tasks.Actor, string, tasks.DeleteRelationInput) error
	ListEvents(context.Context, tasks.Actor, string, int64, int) ([]tasks.Event, error)
	ListNotifications(context.Context, tasks.Actor, bool) ([]tasks.Notification, error)
	MarkNotification(context.Context, tasks.Actor, string, string) (tasks.Notification, error)
	Principals(context.Context, tasks.Actor) (tasks.PrincipalInfo, error)
	AgentAction(context.Context, tasks.Actor, string, map[string]any) (any, error)
	RebindAgentPool(context.Context, tasks.Actor, string, string, []string, int64, string) (tasks.AgentPool, error)
	GetAgentPool(context.Context, tasks.Actor, string, string) (tasks.AgentPool, error)
	ListAgentPools(context.Context, tasks.Actor, string) ([]tasks.AgentPool, error)
	CreateQueueWorkflowTrigger(context.Context, tasks.Actor, string, tasks.CreateQueueWorkflowTriggerInput) (tasks.QueueWorkflowTrigger, error)
	ListQueueWorkflowTriggers(context.Context, tasks.Actor, string) ([]tasks.QueueWorkflowTrigger, error)
	DeleteQueueWorkflowTrigger(context.Context, tasks.Actor, string, int64) error
	Advance(context.Context, tasks.Actor, string, tasks.AdvanceInput) (tasks.TransitionRequest, error)
	SetArtifact(context.Context, tasks.Actor, string, string, string) (tasks.Artifact, error)
	ListArtifacts(context.Context, tasks.Actor, string) ([]tasks.Artifact, error)
	GetArtifact(context.Context, tasks.Actor, string, string) (tasks.Artifact, []tasks.Artifact, error)
	GetWorkflow(context.Context, tasks.Actor, string) (tasks.WorkflowView, error)
	GetTransitionRequest(context.Context, tasks.Actor, string, int64) (tasks.TransitionRequest, error)
	ListScriptRuns(context.Context, tasks.Actor, string) ([]tasks.ScriptRun, error)
	GetScriptRun(context.Context, tasks.Actor, string, int64) (tasks.ScriptRun, error)
	ScriptRunLog(context.Context, tasks.Actor, string, int64, int) (string, bool, error)
	MoveWorkflow(context.Context, tasks.Actor, string, string, string) (tasks.Task, error)
	CancelWorkflowTask(context.Context, tasks.Actor, string) (tasks.Task, error)
	ResumeWorkflow(context.Context, tasks.Actor, string, string) (tasks.Task, error)
	SetQueueWorkflow(context.Context, tasks.Actor, string, string, int64) (tasks.QueueWorkflow, error)
	GetQueueWorkflow(context.Context, tasks.Actor, string) (tasks.QueueWorkflow, error)
	ClearQueueWorkflow(context.Context, tasks.Actor, string, int64) error
	SetQueueSecret(context.Context, tasks.Actor, string, string, string) error
	SetQueueSecretInfo(context.Context, tasks.Actor, string, string, string) (tasks.QueueSecretInfo, error)
	ListQueueSecrets(context.Context, tasks.Actor, string) ([]tasks.QueueSecretInfo, error)
	RemoveQueueSecret(context.Context, tasks.Actor, string, string) error
	ListQueueSources(context.Context, tasks.Actor, string) ([]tasks.QueueSource, error)
	QueueSourceRunLog(context.Context, tasks.Actor, string, int64, int) (string, bool, error)
}

// ScriptControl is the narrow manager-owned script lifecycle surface used by
// dashboard commands. Keeping it separate from ServiceControl avoids making
// every lifecycle implementation aware of durable script execution.
type ScriptControl interface {
	RunOnce(agent string, in script.CreateOnce) (script.Definition, script.Run, error)
	ScheduleScript(agent string, in script.CreateSchedule) (script.Definition, script.Run, error)
	RerunScript(agent, scriptID string) (script.Run, error)
	ListScripts(agent string) ([]script.Definition, error)
	ListScriptRuns(agent, scriptID string) ([]script.Run, error)
	GetScriptRun(agent, runID string) (script.Run, error)
	LogScriptRun(agent, runID string) (string, error)
	OpenScriptLog(agent, runID string) (file io.ReadCloser, filename string, err error)
	CancelScriptTarget(agent, id string) error
	RemoveScript(agent, scriptID string) error
}

// RunSpec is the create+start request for an agent service.
type RunSpec struct {
	ImageRef                 string
	Name                     string
	Cwd                      string
	Harness                  string
	Model                    string
	Effort                   string
	Interactive              bool
	Env                      map[string]string
	Plugins                  []string
	Loop                     bool
	IntervalS                int
	TimeoutS                 int
	HardTimeoutS             int
	OnTimeout                string
	OnError                  string
	MaxIdleIterations        int
	AIStallTimeoutS          int
	UserPrompt               string
	MessagesBatch            int
	MessagesMaxQueue         int
	GoalEnabled              *bool
	GoalWaitCustomerTimeoutS int
	GoalDeliveryCooldownS    int
	Group                    string
	Alias                    string
	Notes                    string
	Color                    string
}

// ServiceControl is the daemon-side agent lifecycle, implemented by loop.Manager
// and consumed by the agent.* command handlers.
type ServiceControl interface {
	Run(spec RunSpec) (string, error)
	Start(name string) error
	Stop(name string) error
	Restart(name string) error
	Kill(name string) error
	// Remove tears an agent down. force bypasses the running-guard (orthogonal to
	// purge). When purge is false the agent's durable data is preserved — the
	// agents DB row (left stopped), CONTEXT.md, iterations (dir + rows),
	// audit.jsonl is kept while only the rebuildable image/bin/workdir
	// tree is dropped, so a later up re-provisions in place. When purge is true it
	// is a full hard delete that also cleans the agent-keyed rows the plain path
	// leaves behind.
	Remove(name string, force, purge bool) error
	// Reprovision re-unpacks image into an existing agent's tree (image/bin/workdir)
	// and restarts its loop WITHOUT touching CONTEXT.md, iterations or audit.jsonl.
	// It is the up-side counterpart of a preserving Remove: after a data-preserving
	// down, up calls this to bring the agent back on the (possibly new) image while
	// keeping its history. Passing an image different from the stored one performs
	// the in-place image swap.
	Reprovision(name, image string) error
	Exec(name, prompt string) (string, error)
	LiveState(name string) (string, error)
	Screen(name string) (string, error)
	SendKeys(name, keys string) error
	SendKeysItems(name string, items []shim.KeyItem) error
	// Attach opens a live PTY stream to the agent's running interactive shim,
	// sizing the pane to cols x rows. The returned conn carries raw terminal
	// bytes both ways; it errors when no interactive iteration is running.
	Attach(name string, cols, rows int) (net.Conn, error)
	// Resize adjusts the running interactive shim's PTY dimensions.
	Resize(name string, cols, rows int) error
	// ExtendIterationTimeout advances a live iteration's persisted timeout and
	// asks its shim to adopt the new hard deadline. A sync failure is reported in
	// the result but does not undo the durable extension.
	ExtendIterationTimeout(name, id string) (IterationTimeoutExtension, error)
}

// IterationTimeoutExtension is the canonical result of extending a running
// iteration. ShimSync is either "success" or "pending".
type IterationTimeoutExtension struct {
	TimeoutDeadline     string
	HardTimeoutDeadline string
	TimeoutExtensions   int
	ShimSync            string
}

// LoopConfigControl is the live-runtime half of persisted Autopilot settings.
// Command handlers use it when a daemon manager is present so parked engines
// re-read configuration immediately and armed timers are canceled promptly.
type LoopConfigControl interface {
	SetLoopEnabled(name string, enabled bool) error
	RefreshLoopConfig(name string)
}

// PluginControl is the daemon-side external-plugin lifecycle, implemented by
// plugins.Host and consumed by the plugin.* command handlers.
type PluginControl interface {
	Install(sourcePath string) (map[string]any, error)
	Remove(name string) error
	// Restart cycles one named plugin's process in place (stop + wait-drain +
	// start from the stored record) without touching any other plugin or the
	// daemon. Consumed by the plugin.restart command handler.
	Restart(name string) error
	List() ([]map[string]any, error)
	Inspect(name string) (map[string]any, error)
	Logs(name string, tail int) ([]string, error)
	PluginRoutes(name string) (map[string]any, error)
	PluginAction(name string, body map[string]any) (map[string]any, error)
	ApplyActionSubscriptions(name string, response map[string]any) error
}

// GroupControl is the daemon-side group lifecycle, implemented by
// groups.Provisioner and consumed by the group.* command handlers.
type GroupControl interface {
	Create(name, lead string) (map[string]any, error)
	List() ([]map[string]any, error)
	Inspect(name string) (map[string]any, error)
	Remove(name string, volumes bool) error
	Assign(agent, group string) error
	Rename(oldName, newName string) error
	ChangeLead(name, lead string) error
}

type HandlerFunc func(c *Ctx, p Params) (any, error)

// FollowFunc is a CLI-local streaming/polling composite. cli.Run invokes it —
// with the daemon socket path — when a command's follow flag is set, instead of
// the normal remote/local dispatch. It prints to out and returns when ctx is
// cancelled (Ctrl-C) or the stream ends.
type FollowFunc func(ctx context.Context, sock string, p Params, out io.Writer) error

type Command struct {
	Path    string
	Summary string
	Help    string
	Args    []Arg
	// ResultSchema constrains the value stored under the standard result
	// envelope in generated OpenAPI. Nil keeps the backward-compatible free
	// result value for commands that have not declared a schema yet.
	ResultSchema map[string]any
	// Schemas contributes reusable OpenAPI component schemas referenced by this
	// command's request or result metadata.
	Schemas map[string]map[string]any
	HTTP    *HTTPRoute
	Handler HandlerFunc
	// FollowFlag names a Bool arg that switches this (otherwise remote) command
	// into follow mode; when that arg is true, cli.Run runs Follow (a CLI-local
	// composite over Ctx.Socket) instead of the HTTP dispatch. Used by
	// `logs -f` (SSE stream) and `channel tail -f` (poll for new messages).
	FollowFlag string
	Follow     FollowFunc
	// CLIHidden keeps a command off the CLI surface entirely: it is not
	// resolvable as an argv path and is omitted from CLI help. The daemon HTTP
	// route (and openapi) is unaffected. Used for endpoints that are called only
	// by CLI-local composites (e.g. secret.store, whose value must never land in
	// argv/ps/shell history — see secret.set).
	CLIHidden bool
}

type Registry struct {
	byPath  map[string]Command
	byRoute map[string]string // "GET /api/x" -> path
	byGroup map[string]string // dotted group path -> summary
}

func New() *Registry {
	return &Registry{
		byPath:  map[string]Command{},
		byRoute: map[string]string{},
		byGroup: map[string]string{},
	}
}

func (r *Registry) Register(c Command) error {
	if c.Path == "" {
		return fmt.Errorf("command path must not be empty")
	}
	if c.Summary == "" {
		return fmt.Errorf("command %s: summary required", c.Path)
	}
	if c.Handler == nil {
		return fmt.Errorf("command %s: handler required", c.Path)
	}
	if _, dup := r.byPath[c.Path]; dup {
		return fmt.Errorf("duplicate command path %s", c.Path)
	}
	if c.HTTP != nil {
		key := c.HTTP.Method + " " + c.HTTP.Path
		if owner, dup := r.byRoute[key]; dup {
			return fmt.Errorf("route %s already owned by %s", key, owner)
		}
		r.byRoute[key] = c.Path
	}
	r.byPath[c.Path] = c
	return nil
}

func (r *Registry) Get(path string) (Command, bool) {
	c, ok := r.byPath[path]
	return c, ok
}

func (r *Registry) Commands() []Command {
	out := make([]Command, 0, len(r.byPath))
	for _, c := range r.byPath {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

type Group struct{ Path, Summary string }

// RegisterGroup describes an intermediate command-tree node (agent, agent.status).
func (r *Registry) RegisterGroup(path, summary string) error {
	if path == "" {
		return fmt.Errorf("group path must not be empty")
	}
	if summary == "" {
		return fmt.Errorf("group %s: summary required", path)
	}
	if _, dup := r.byGroup[path]; dup {
		return fmt.Errorf("duplicate group path %s", path)
	}
	if _, clash := r.byPath[path]; clash {
		return fmt.Errorf("group %s collides with command of the same path", path)
	}
	r.byGroup[path] = summary
	return nil
}

func (r *Registry) Group(path string) (string, bool) {
	s, ok := r.byGroup[path]
	return s, ok
}

func (r *Registry) Groups() []Group {
	out := make([]Group, 0, len(r.byGroup))
	for p, s := range r.byGroup {
		out = append(out, Group{Path: p, Summary: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Validate enforces the group model: every ancestor prefix of a multi-segment
// command OR group path must be a registered group, and no command path may
// equal a group path. Run once after all Register/RegisterGroup calls.
func (r *Registry) Validate() error {
	for path := range r.byPath {
		if _, isGroup := r.byGroup[path]; isGroup {
			return fmt.Errorf("command %s has the same path as a group", path)
		}
		parts := strings.Split(path, ".")
		for i := 1; i < len(parts); i++ {
			prefix := strings.Join(parts[:i], ".")
			if _, ok := r.byGroup[prefix]; !ok {
				return fmt.Errorf("command %s: parent group %q has no registered summary", path, prefix)
			}
		}
	}
	// A group path is itself a tree node, so its ancestors must exist too:
	// registering "a.b" with no "a" group leaves "tariboy a" unknown and
	// "a.b" unreachable. Hold groups to the same ancestry rule as commands.
	for path := range r.byGroup {
		parts := strings.Split(path, ".")
		for i := 1; i < len(parts); i++ {
			prefix := strings.Join(parts[:i], ".")
			if _, ok := r.byGroup[prefix]; !ok {
				return fmt.Errorf("group %s: parent group %q has no registered summary", path, prefix)
			}
		}
	}
	return nil
}

// Tree renders {"daemon": {"status": {"summary": ..., "args": [...]}}}.
func (r *Registry) Tree() map[string]any {
	root := map[string]any{}
	for _, c := range r.Commands() {
		// CLIHidden commands are off the CLI surface (not in -h), so keep them
		// out of --help-json too — otherwise the two disagree on what's exposed.
		if c.CLIHidden {
			continue
		}
		node := root
		parts := strings.Split(c.Path, ".")
		for i, p := range parts[:len(parts)-1] {
			child, ok := node[p].(map[string]any)
			if !ok {
				child = map[string]any{}
				node[p] = child
			}
			if s, ok := r.byGroup[strings.Join(parts[:i+1], ".")]; ok {
				child["summary"] = s
			}
			node = child
		}
		leaf := map[string]any{"summary": c.Summary}
		if c.Help != "" {
			leaf["help"] = c.Help
		}
		if c.HTTP != nil {
			leaf["http"] = map[string]string{"method": c.HTTP.Method, "path": c.HTTP.Path}
		}
		args := []map[string]any{}
		for _, a := range c.Args {
			args = append(args, map[string]any{
				"name": a.Name, "flag": a.Flag, "short": a.Short, "type": string(a.Type),
				"required": a.Required, "repeatable": a.Repeatable, "default": a.Default, "help": a.Help,
			})
		}
		leaf["args"] = args
		node[parts[len(parts)-1]] = leaf
	}
	return root
}
