package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/agentdir"
	"github.com/alekzonder/tariboy/internal/bus"
	"github.com/alekzonder/tariboy/internal/harness"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/shim"
	"github.com/alekzonder/tariboy/internal/tasks"
)

// PromptParts are the ordered sections of an iteration prompt (spec §5.3).
type PromptParts struct {
	Agent           string
	Cwd             string
	ImagePrompt     string
	Context         string
	Messages        string
	AwaitingReplies string
	UserPrompt      string
	OneShot         string
	Goal            string
	Tail            string
}

func AssemblePrompt(p PromptParts) string {
	header := fmt.Sprintf("# You are agent %s\ncwd: %s", p.Agent, p.Cwd)
	work := FormatTaskProcessingOrder(RuntimePromptValues{OneShot: p.OneShot, Messages: p.Messages, AwaitingReplies: p.AwaitingReplies, Goal: p.Goal})
	sections := []string{header, p.ImagePrompt, p.Context, work, p.UserPrompt, p.Tail}
	var kept []string
	for _, s := range sections {
		if strings.TrimSpace(s) != "" {
			kept = append(kept, strings.TrimRight(s, "\n"))
		}
	}
	return strings.Join(kept, "\n\n") + "\n"
}

// messageProcessedInstruction is the standing block appended to every rendered
// message batch. Deliveries are no longer auto-acked on iteration success (spec
// §3.2): an agent MUST explicitly process each message by its id, mirroring the
// old system. A message stays pending (and is re-rendered next iteration) until
// it is processed — Pending's attempts++/DLQ-after-5 and the loop-hot HasPending
// behaviour then give re-prompt-until-processed semantics for free.
const messageProcessedInstruction = `When you have handled a message you MUST run:
    scripts/messages.sh message processed <id> "<what you did / result>"
Both arguments are mandatory. Handling can be: doing the work, filing a task
(name it in the result), or replying (a reply auto-processes the message).`

// FormatMessages renders a pending batch into the prompt's Messages section in a
// stable, greppable layout (spec §3.2, §5.3). Each message is rendered WITH its
// id and per-message state (kind + threading fields), followed by the standing
// processing instruction.
func FormatMessages(msgs []bus.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Messages\nIncoming messages for this iteration (newest last). Each is listed with its id:\n")
	for _, m := range msgs {
		fmt.Fprintf(&b, "\n- id %s [%s] %s: %s", m.ID, m.Type, m.Source, m.Text)
		if m.Kind != "" && m.Kind != "event" {
			fmt.Fprintf(&b, "\n  kind: %s", m.Kind)
		}
		if m.InReplyTo != "" {
			fmt.Fprintf(&b, "\n  in_reply_to: %s", m.InReplyTo)
		}
		if m.CorrelationID != "" {
			fmt.Fprintf(&b, "\n  correlation_id: %s", m.CorrelationID)
		}
		if m.ReplyTo != "" {
			fmt.Fprintf(&b, "\n  reply_to: %s", m.ReplyTo)
		}
		if m.Deadline != "" {
			fmt.Fprintf(&b, "\n  deadline: %s", m.Deadline)
		}
		if len(m.Subject) > 0 {
			if j, err := json.Marshal(m.Subject); err == nil {
				fmt.Fprintf(&b, "\n  subject: %s", string(j))
			}
		}
		if len(m.Data) > 0 {
			if j, err := json.Marshal(m.Data); err == nil {
				fmt.Fprintf(&b, "\n  data: %s", string(j))
			}
		}
	}
	b.WriteString("\n\n")
	b.WriteString(messageProcessedInstruction)
	return b.String()
}

// FormatAwaitingReplies renders the agent's outstanding requests — kind=request
// messages with no reply yet — into the prompt's "# Awaiting replies" section
// (spec §3.2, §7). Derived from bus.PendingRequests; empty ⇒ "" so the section is
// skipped. now anchors each request's age.
func FormatAwaitingReplies(msgs []bus.Message, now time.Time) string {
	if len(msgs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Awaiting replies\nRequests you sent that have no reply yet:\n")
	for _, m := range msgs {
		fmt.Fprintf(&b, "\n- id %s  channel %s", m.ID, m.Channel)
		if age, ok := messageAge(m.TS, now); ok {
			fmt.Fprintf(&b, "  age %s", age)
		}
		if m.Deadline != "" {
			fmt.Fprintf(&b, "  deadline %s", m.Deadline)
		}
	}
	return b.String()
}

// messageAge reports how long ago ts (RFC3339Nano) was relative to now, truncated
// to whole seconds. ok=false when ts is unparseable, so callers omit the age
// rather than render a bogus one.
func messageAge(ts string, now time.Time) (time.Duration, bool) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0, false
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return d.Truncate(time.Second), true
}

// BuildEnv merges base env with the per-iteration injections (spec §10). Agent
// env and secrets override base; TARIBOY_* and the agent bin PATH are set
// last. When proxyEnabled is set the harness is pointed at the local AI proxy,
// while provider credentials remain the harness' responsibility and are
// forwarded by the proxy as-is.
func BuildEnv(base []string, agentBin, agentName, iterationID, toolsSock string, proxyEnabled bool, proxyURL, proxyToken string, env, secrets map[string]string) []string {
	m := map[string]string{}
	for _, kv := range base {
		if i := strings.Index(kv, "="); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range env {
		m[k] = v
	}
	for k, v := range secrets {
		m[k] = v
	}
	m["TARIBOY_AGENT"] = agentName
	m["TARIBOY_ITERATION"] = iterationID
	m["TARIBOY_TOOLS_SOCKET"] = toolsSock
	m["SHELL"] = "/bin/sh"
	if proxyURL != "" && proxyToken != "" {
		// Route the harness through the AI proxy. Attribution lives in proxyURL;
		// provider auth env remains unchanged.
		m["ANTHROPIC_BASE_URL"] = proxyURL
		m["OPENAI_BASE_URL"] = proxyURL + "/v1"
	}
	basePath := m["PATH"]
	if basePath == "" {
		basePath = "/usr/bin:/bin"
	}
	if agentBin != "" {
		m["PATH"] = agentBin + ":" + basePath
	} else {
		m["PATH"] = basePath
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(m))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

const agentShellPrelude = `set -e
readonly -a tariboy_harness_argv=("${@:3}")
for script in "$1" "$2"; do
  if [[ -f "$script" ]]; then
    set -e
    # Keep source outside a conditional so errexit also applies inside it.
    source "$script"
    status=$?
    if [[ "$status" != 0 ]]; then exit "$status"; fi
  fi
done
exec "${tariboy_harness_argv[@]}"`

func agentShellCommand(bash, baseDir string, layout agentdir.Layout, harnessArgv []string) []string {
	argv := []string{bash, "-c", agentShellPrelude, "tariboy-agent-shell", filepath.Join(baseDir, "global-agent-shell.sh"), layout.ShellScriptPath()}
	return append(argv, harnessArgv...)
}

func mergeSkillLaunchEnv(base, overlay []string) ([]string, error) {
	m := make(map[string]string, len(base)+len(overlay))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	for _, kv := range overlay {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid image skill environment entry %q", kv)
		}
		key := kv[:i]
		switch key {
		case "HOME", "CODEX_HOME", "XDG_CONFIG_HOME":
			return nil, fmt.Errorf("image skill bridge may not override %s", key)
		}
		m[key] = kv[i+1:]
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+m[key])
	}
	return out, nil
}

// Classify maps the shim result to an iteration status (spec §5).
func Classify(exitCode int, done, softTimeout, hardTimeout bool) string {
	if softTimeout || hardTimeout {
		return "timeout"
	}
	if done {
		return "done"
	}
	if exitCode != 0 {
		return "harness_error"
	}
	return "no_i_am_done"
}

// Spawner starts the shim process; the seam lets tests avoid real processes.
type Spawner interface {
	Start(argv, env []string, dir string) error
}

// ManagedSpawner additionally returns a process-level termination function.
// ShimRunner uses it when Stop cancels an iteration while detached shim launch
// is in progress. The production function first asks the shim process to run
// its signal cleanup, then escalates against the shim process group.
type ManagedSpawner interface {
	StartManaged(argv, env []string, dir string) (terminate func() error, err error)
}

// ProxyBinder is the slice of the AI proxy the runner needs. Defined here (plain
// strings, no aiproxy import) so loop stays free of an import cycle; satisfied by
// *aiproxy.Proxy via thin wrapper methods (Task 12).
type ProxyBinder interface {
	ProxyBaseURL() string
	MintToken(agent, iteration, imageName, imageTag, imageDigest string) (string, error)
	RevokeToken(token string)
	RevokeIteration(iteration string)
	// UpdateTask stamps native task/root attribution onto the live token(s) for
	// key (a token string or iteration id); empty task/root clear it. Returns
	// the number of tokens updated. Iteration preparation uses it before launch.
	UpdateTask(key, taskID, epicID string) int
}

// ExecSpawner launches the shim detached with os/exec.
type ExecSpawner struct{}

func (ExecSpawner) Start(argv, env []string, dir string) error {
	_, err := (ExecSpawner{}).StartManaged(argv, env, dir)
	return err
}

func (ExecSpawner) StartManaged(argv, env []string, dir string) (func() error, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// The shim is launched detached: the runner observes the iteration through
	// result.json and the shim RPC, never through this process handle. Nothing
	// else waits on the child, so without an explicit reaper every finished
	// iteration leaves a zombie behind for the daemon's whole uptime. Waiting in
	// a goroutine reaps the shim without blocking the launch path, and the shim
	// still outlives the daemon on shutdown (it has its own process group).
	pgid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		_ = cmd.Wait()
	}()
	// A process group outlives its leader — its pgid stays reserved until the last
	// member exits. Before this reaper the shim lingered as a zombie and pinned
	// the pgid forever; now the group usually empties when the shim is reaped, and
	// an empty pgid is free for the kernel to hand out again. Signalling then
	// could land on somebody else's group, so skip the signal once the reaper has
	// run and report the os.ErrProcessDone an ESRCH kill already produces.
	//
	// This narrows the window rather than closing it: the shim can still be reaped
	// between the check and the kill. It also gives up on a process that outlives
	// the shim inside its group — the harness is not one (the shim starts it with
	// Setsid) and terminateCanceledLaunch tries the shim RPC and tmux before ever
	// reaching here, so losing that rare straggler beats signalling a stranger.
	kill := func(sig syscall.Signal) error {
		select {
		case <-reaped:
			return os.ErrProcessDone
		default:
		}
		return syscall.Kill(-pgid, sig)
	}
	return func() error {
		err := kill(syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		killErr := kill(syscall.SIGKILL)
		if errors.Is(killErr, syscall.ESRCH) || errors.Is(killErr, os.ErrProcessDone) {
			return nil
		}
		return killErr
	}, nil
}

// ErrSessionAlive is returned by Run when an interactive agent already has a live
// tmux session — launching a second iteration on top would fail with a duplicate
// session and silently produce no harness. The loop maps it to a failed manual
// iteration (and skips loop-triggered ticks) instead.
var ErrSessionAlive = errors.New("tmux_session_exists")

// ErrIterationDetached means the daemon-side observer stopped while the
// shim-owned harness continues. The durable iteration remains running for the
// next daemon to adopt; this is not a terminal harness error.
var ErrIterationDetached = errors.New("iteration_detached")

type RunnerConfig struct {
	AgentsDir    string
	RuntimeDir   string
	ShimBin      string
	ImgStore     *image.Store
	Store        *agent.Store
	Spawner      Spawner
	Clock        func() time.Time
	PollInterval time.Duration
	DoneGrace    time.Duration
	Logger       *slog.Logger
	Bus          *bus.Bus
	Proxy        ProxyBinder
	CurrentGoal  func(string, time.Time) (tasks.Task, bool, error)
	Tasks        nativeTaskReader
	// HasTmuxSession reports whether an interactive agent's tmux session is already
	// alive. Injectable so tests avoid a real tmux. Defaults to tmuxHasSession.
	HasTmuxSession func(session string) bool
	// KillTmuxSession tears down an orphaned tmux session (one no running
	// iteration owns). Injectable so tests avoid a real tmux. Defaults to
	// tmuxKillSession.
	KillTmuxSession func(session string) error
	// AuditFor returns the shared per-agent audit recorder (one *audit.Log
	// instance per agent, also used by the daemon and engine so the seq counter
	// stays consistent). Nil disables audit tailing.
	AuditFor func(agent string) Recorder
}

const defaultDoneGrace = 2 * time.Second

type doneGraceTracker struct {
	seen     bool
	seenAt   time.Time
	killSent bool
}

type cooperativeKill struct {
	sent bool
}

func (k *cooperativeKill) send(kill func() error) error {
	if k.sent {
		return nil
	}
	k.sent = true
	return kill()
}

func (d *doneGraceTracker) observe(done bool, now time.Time, grace time.Duration, kill func() error) {
	if !done || d.killSent {
		return
	}
	if !d.seen {
		d.seen = true
		d.seenAt = now
		return
	}
	if now.Before(d.seenAt.Add(grace)) {
		return
	}
	d.killSent = true
	_ = kill()
}

// tmuxHasSession is the production HasTmuxSession: `tmux has-session -t <name>`
// exits 0 iff the session exists.
func tmuxHasSession(session string) bool {
	return exec.Command("tmux", "has-session", "-t", session).Run() == nil
}

// tmuxKillSession is the production KillTmuxSession.
func tmuxKillSession(session string) error {
	return shim.KillTmuxSession(session)
}

// SessionBlocked reports whether launching an iteration for ag would collide with
// an already-live tmux session (interactive agents only).
func (r *ShimRunner) SessionBlocked(ag agent.Agent) bool {
	return ag.Interactive && r.cfg.HasTmuxSession != nil && r.cfg.HasTmuxSession(ag.Name)
}

// ReapOrphanBlock self-heals a launch blocked solely by an ORPHANED tmux session:
// one that is alive but owned by no running iteration (left behind by a crashed
// daemon or a failed adoption). It kills such a session and returns true so the
// engine can proceed with the launch instead of failing every future iteration.
// It returns false when the agent is not session-blocked, or when a genuinely
// running iteration still owns the session — a live interactive harness must
// never be killed from under the user. Mirrors Manager.reapOrphanSessions, but at
// launch time rather than only on daemon restart.
func (r *ShimRunner) ReapOrphanBlock(ag agent.Agent) bool {
	if !r.SessionBlocked(ag) {
		return false
	}
	if r.hasRunningIteration(ag.Name) {
		return false
	}
	kill := r.cfg.KillTmuxSession
	if kill == nil {
		kill = tmuxKillSession // engine may hold a runner built without the default
	}
	if err := kill(ag.Name); err != nil {
		r.cfg.Logger.Warn("reap orphan tmux session before launch", "agent", ag.Name, "err", err)
		return false
	}
	r.cfg.Logger.Info("reaped orphan tmux session before launch", "agent", ag.Name)
	return true
}

// hasRunningIteration reports whether the store holds a still-running iteration
// for name (the owner of a live tmux session).
func (r *ShimRunner) hasRunningIteration(name string) bool {
	if r.cfg.Store == nil {
		return false
	}
	its, err := r.cfg.Store.ListIterations(name)
	if err != nil {
		return false
	}
	for _, it := range its {
		if it.Status == "running" {
			return true
		}
	}
	return false
}

type ShimRunner struct{ cfg RunnerConfig }

func NewShimRunner(cfg RunnerConfig) *ShimRunner {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	if cfg.DoneGrace <= 0 {
		cfg.DoneGrace = defaultDoneGrace
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HasTmuxSession == nil {
		cfg.HasTmuxSession = tmuxHasSession
	}
	if cfg.KillTmuxSession == nil {
		cfg.KillTmuxSession = tmuxKillSession
	}
	return &ShimRunner{cfg: cfg}
}

func (r *ShimRunner) Run(ctx context.Context, ag agent.Agent, trigger, iterationID, oneShot string) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	// Backstop guard: never spawn a shim on top of a live tmux session. The engine
	// normally intercepts this before creating the iteration (SessionBlocked), so
	// reaching here means a direct Run — fail loudly rather than silently no-op.
	if r.SessionBlocked(ag) {
		return Outcome{}, ErrSessionAlive
	}
	l := agentdir.New(r.cfg.AgentsDir, ag.Name).WithRuntime(r.cfg.RuntimeDir)
	if err := l.EnsureIteration(iterationID); err != nil {
		return Outcome{}, err
	}

	// Child spans nest under the iteration span carried by ctx (spec §14).
	// prepare -> shim.spawn -> harness are sequential phases, not nested, so each
	// ends before the next starts. OTel-off ⇒ no-op spans (free).
	tr := otel.Tracer("tariboy/loop")

	prep, err := r.prepare(ctx, tr, ag, l, iterationID, oneShot)
	if err != nil {
		return Outcome{}, err
	}
	// The proxy token (if any) was minted inside prepare and must outlive it.
	// A normal terminal exit revokes it; a daemon-side detach deliberately
	// leaves it active for the replacement daemon and adopted shim.
	detached := false
	if r.cfg.Proxy != nil && prep.proxyToken != "" {
		defer func() {
			if !detached {
				r.cfg.Proxy.RevokeToken(prep.proxyToken)
			}
		}()
	}

	var rec Recorder
	if r.cfg.AuditFor != nil {
		rec = r.cfg.AuditFor(ag.Name)
	}

	r.cfg.Logger.Info("launching harness",
		"agent", ag.Name, "iteration", iterationID, "harness", ag.HarnessType,
		"cwd", prep.cwd, "argv", prep.maskedArgv)
	if rec != nil {
		rec.Record("launching_harness", "system", iterationID,
			map[string]any{
				"harness":     ag.HarnessType,
				"interactive": ag.Interactive,
				"argv":        prep.maskedArgv,
				"env":         prep.maskedEnv,
			})
	}
	_, spawn := tr.Start(ctx, "shim.spawn")
	if err := ctx.Err(); err != nil {
		spawn.End()
		return Outcome{}, err
	}
	// Snapshot both deadlines from one clock sample directly before the shim is
	// launched. Preparation time must not consume the configured timeout.
	now := r.cfg.Clock()
	if err := r.cfg.Store.InitializeIterationTimeout(iterationID, ag.TimeoutS, hardTimeout(ag), now); err != nil {
		spawn.End()
		return Outcome{}, fmt.Errorf("initialize iteration timeout: %w", err)
	}
	// The shim starts its watchdog before the runner's first polling
	// synchronization. Pass the exact persisted deadline at launch so that
	// initial enforcement is anchored to the pre-spawn snapshot, rather than to
	// the shim process's later wall-clock read.
	it, err := r.cfg.Store.GetIteration(ag.Name, iterationID)
	if err != nil {
		spawn.End()
		return Outcome{}, fmt.Errorf("read initialized iteration timeout: %w", err)
	}
	if it.HardTimeoutDeadline != nil {
		separator := slices.Index(prep.shimArgv, "--")
		if separator < 0 {
			spawn.End()
			return Outcome{}, fmt.Errorf("shim argv missing harness separator")
		}
		prep.shimArgv = slices.Insert(prep.shimArgv, separator, "--hard-deadline", *it.HardTimeoutDeadline)
		prep.maskedArgv = slices.Insert(prep.maskedArgv, separator, "--hard-deadline", *it.HardTimeoutDeadline)
	}
	if err := ctx.Err(); err != nil {
		spawn.End()
		return Outcome{}, err
	}
	var terminate func() error
	if managed, ok := r.cfg.Spawner.(ManagedSpawner); ok {
		terminate, err = managed.StartManaged(prep.shimArgv, prep.env, prep.cwd)
	} else {
		err = r.cfg.Spawner.Start(prep.shimArgv, prep.env, prep.cwd)
	}
	spawn.End()
	if err != nil {
		r.cfg.Logger.Error("spawn shim failed", "agent", ag.Name, "iteration", iterationID, "err", err)
		if rec != nil {
			rec.Record("shim_error", "system", iterationID, map[string]any{"error": err.Error()})
		}
		return Outcome{}, fmt.Errorf("spawn shim: %w", err)
	}
	if ctx.Err() != nil {
		if it, loadErr := r.cfg.Store.GetIteration(ag.Name, iterationID); loadErr == nil && it.Status != "running" {
			r.terminateCanceledLaunch(ag, l, iterationID, terminate)
			return Outcome{}, ctx.Err()
		}
	}
	r.cfg.Logger.Info("harness spawned", "agent", ag.Name, "iteration", iterationID)
	if rec != nil {
		rec.Record("harness_spawned", "system", iterationID, nil)
		// Tee logs into the audit log for the lifetime of the harness; the final
		// drain on Stop captures trailing output. Interactive agents tee only
		// shim.log (their harness output is a tmux TUI capture, not audit-worthy).
		tailer := StartTailer(rec, iterationID, l.LogsDir(iterationID), r.cfg.PollInterval, ag.Interactive)
		defer tailer.Stop()
	}

	hctx, harnessSpan := tr.Start(ctx, "harness")
	outcome, err := r.await(hctx, ag, l, iterationID)
	harnessSpan.End()
	if errors.Is(err, ErrIterationDetached) {
		detached = true
	}
	if err == nil && outcome.Status == "harness_error" {
		outcome.Error = harnessExitReason(outcome.ExitCode, l.HarnessStderr(iterationID), ag.Interactive)
	}
	// No auto-ack of the message batch here (spec §3.2). Deliveries are drained
	// only by an explicit `tools message processed` (bus.MarkProcessed); anything
	// unprocessed re-renders next iteration and eventually dead-letters.
	return outcome, err
}

// terminateCanceledLaunch first asks the actual shim to stop cooperatively, so
// its Kill implementation owns the harness process group or tmux session. The
// socket can appear just after detached process creation, so connection is
// retried for a bounded interval. Only when no shim can be reached do we fall
// back to an explicit tmux kill plus the managed outer process-group handle.
func (r *ShimRunner) terminateCanceledLaunch(
	ag agent.Agent,
	l agentdir.Layout,
	iterationID string,
	terminate func() error,
) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		dialTimeout := minDuration(remaining, 100*time.Millisecond)
		client, err := shim.ConnectTimeout(l.ShimSock(), dialTimeout)
		if err == nil {
			if err := client.Kill(); err == nil {
				break
			}
		}
		time.Sleep(minDuration(remaining, 20*time.Millisecond))
	}
	if ag.Interactive && r.cfg.KillTmuxSession != nil {
		if err := r.cfg.KillTmuxSession(ag.Name); err != nil {
			r.cfg.Logger.Warn("terminate canceled tmux launch",
				"agent", ag.Name, "iteration", iterationID, "err", err)
		}
	}
	if terminate != nil {
		if err := terminate(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			r.cfg.Logger.Warn("terminate canceled shim launch",
				"agent", ag.Name, "iteration", iterationID, "err", err)
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// preparedRun carries everything the prepare phase computed for shim.spawn.
type preparedRun struct {
	shimArgv   []string
	env        []string
	cwd        string
	proxyToken string // non-empty only when a proxy token was minted
	// maskedArgv/maskedEnv mirror shimArgv/env with secret values redacted for
	// logs and the launching_harness audit event — never for the actual spawn.
	maskedArgv []string
	maskedEnv  []string
}

type activatedImageContextKey struct{}

// prepare runs the prepare phase (prompt assembly, harness command build,
// proxy token mint) in its own scope so the "prepare" span is ended on every
// exit — success or any of its early-return errors — via defer, without
// staying open across the later shim.spawn/harness phases. Failed spans are
// marked with an error status so prepare-phase failures are traceable.
func (r *ShimRunner) prepare(ctx context.Context, tr oteltrace.Tracer, ag agent.Agent, l agentdir.Layout, iterationID, oneShot string) (preparedRun, error) {
	_, prep := tr.Start(ctx, "prepare")
	defer prep.End()

	fail := func(err error) (preparedRun, error) {
		prep.RecordError(err)
		prep.SetStatus(codes.Error, err.Error())
		return preparedRun{}, err
	}

	// A bare image gets an instructions-free session:
	// no assembled prompt, no message drain, no bin shims on PATH (spec
	// 2026-07-22-terminals-simple-ui §1.3).
	bare := ag.ImageRef == image.BareRef.String()
	iteration, err := r.cfg.Store.GetIteration(ag.Name, iterationID)
	if err != nil {
		return fail(fmt.Errorf("read trusted iteration image identity: %w", err))
	}
	if iteration.ImageRef != "" && iteration.ImageRef != ag.ImageRef {
		return fail(fmt.Errorf("iteration image ref does not match active agent image"))
	}
	if iteration.ImageDigest != "" && iteration.ImageDigest != ag.ImageDigest {
		return fail(fmt.Errorf("iteration image digest does not match active agent image"))
	}
	imageSchemaVersion := 1
	if iteration.PromptTemplateSHA256 != "" {
		imageSchemaVersion = 2
	}
	currentGoal, hasCurrentGoal := tasks.Task{}, false
	if r.cfg.CurrentGoal != nil && !bare {
		currentGoal, hasCurrentGoal, err = r.cfg.CurrentGoal(ag.Name, r.cfg.Clock().UTC())
		if err != nil {
			return fail(fmt.Errorf("read current agent goal: %w", err))
		}
	}

	// Assemble and write the prompt.
	prompt := ""
	var batch []bus.Message
	if !bare {
		contextText := ""
		if data, err := os.ReadFile(l.ContextPath()); err == nil {
			contextText = string(data)
		}

		// Drain a batch of pending messages (spec §5.3). Rendering increments each
		// delivery's attempts; the batch is no longer acked on success — the agent
		// drains it explicitly via `tools message processed` (spec §3.2).
		var awaiting []bus.Message
		if r.cfg.Bus != nil {
			n := ag.MessagesBatch
			if n <= 0 {
				n = 10
			}
			if msgs, err := r.cfg.Bus.Pending(ag.Name, n); err != nil {
				r.cfg.Logger.Warn("pending messages", "agent", ag.Name, "err", err)
			} else {
				batch = msgs
			}
			// Derived "# Awaiting replies" section: the agent's own outstanding
			// requests with no reply yet (spec §3.2, §4.2).
			if reqs, err := r.cfg.Bus.PendingRequests(ag.Name); err != nil {
				r.cfg.Logger.Warn("pending requests", "agent", ag.Name, "err", err)
			} else {
				awaiting = reqs
			}
		}

		goal := ""
		if hasCurrentGoal {
			goal = FormatRuntimeGoal(currentGoal)
		}
		if imageSchemaVersion == 2 {
			template, err := ReadPromptTemplate(l.ImageDir(), iteration.PromptTemplateSHA256)
			if err != nil {
				return fail(err)
			}
			workdir, err := FormatRuntimeWorkdir(l.Workdir())
			if err != nil {
				return fail(err)
			}
			prompt, err = RenderPromptTemplate(template, l.ImageDir(), RuntimePromptValues{
				Identity: FormatRuntimeIdentity(ag.Name, ag.ImageRef, iteration.ImageVersion, ag.ImageDigest, agentCwd(ag, l), iterationID),
				Goal:     goal,
				Workdir:  workdir,
				Context:  contextText, Messages: FormatMessages(batch), AwaitingReplies: FormatAwaitingReplies(awaiting, r.cfg.Clock()), UserPrompt: ag.UserPrompt, OneShot: oneShot,
			})
			if err != nil {
				return fail(err)
			}
		} else {
			imagePrompt, err := os.ReadFile(filepath.Join(l.ImageDir(), "PROMPT.md"))
			if err != nil {
				return fail(fmt.Errorf("read image prompt: %w", err))
			}
			tail, _ := os.ReadFile(filepath.Join(l.ImageDir(), "PROMPT_TAIL.md"))
			prompt = AssemblePrompt(PromptParts{
				Agent: ag.Name, Cwd: agentCwd(ag, l), ImagePrompt: string(imagePrompt),
				Context: contextText, Messages: FormatMessages(batch),
				AwaitingReplies: FormatAwaitingReplies(awaiting, r.cfg.Clock()),
				UserPrompt:      ag.UserPrompt, OneShot: oneShot, Goal: goal, Tail: string(tail),
			})
		}
	}
	activated, _ := ctx.Value(activatedImageContextKey{}).(activatedImage)
	if activated.Skills.PromptPrefix != "" {
		prompt = activated.Skills.PromptPrefix + prompt
	}
	if err := os.WriteFile(l.PromptPath(iterationID), []byte(prompt), 0o600); err != nil {
		return fail(err)
	}
	// Preserve stable evidence references without leaking prompt or message
	// bodies into the audit timeline.
	if r.cfg.AuditFor != nil {
		if rec := r.cfg.AuditFor(ag.Name); rec != nil {
			sum := sha256.Sum256([]byte(prompt))
			deliveryIDs := make([]string, 0, len(batch))
			for _, msg := range batch {
				deliveryIDs = append(deliveryIDs, msg.ID)
			}
			rec.Record("iteration_prepared", "system", iterationID, map[string]any{
				"prompt_sha256": hex.EncodeToString(sum[:]),
				"delivery_ids":  deliveryIDs,
			})
		}
	}
	// Record the prompt path on the iteration row. This is best-effort: a
	// failure here must not abort the run, but it should not be silently
	// swallowed either. prompt_path is owned exclusively by
	// SetIterationPromptPath so a later UpdateIteration call cannot clobber it.
	if err := r.cfg.Store.SetIterationPromptPath(iterationID, l.PromptPath(iterationID)); err != nil {
		r.cfg.Logger.Warn("record prompt path", "agent", ag.Name, "id", iterationID, "err", err)
	}

	// Build the harness command.
	adapter, err := harness.Get(ag.HarnessType)
	if err != nil {
		return fail(err)
	}
	cwd := agentCwd(ag, l)
	if ag.Cwd != "" {
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			return fail(fmt.Errorf("agent cwd %s does not exist", cwd))
		}
	}

	// Schema-v1 images retain their legacy CWD-relative skill layout. Schema-v2
	// skills are attached through the activation-time harness bridge instead.
	if sub := harness.LegacySkillsSubdir(adapter.Type()); imageSchemaVersion != 2 && sub != "" {
		if err := agentdir.MaterializeSkills(l.ImageDir(), cwd, sub); err != nil {
			return fail(fmt.Errorf("materialize skills: %w", err))
		}
	}

	// Injected env (spec §10).
	secrets, err := r.cfg.Store.SecretMap(ag.Name)
	if err != nil {
		return fail(err)
	}

	// Mint a per-iteration proxy token (spec §9). Revoked when the iteration
	// ends, on every exit path.
	//
	// Fail-closed: when a proxy is configured the harness MUST route through it.
	// If we cannot obtain a token or the proxy URL is unavailable, running the
	// iteration would let the harness reach the real upstream API directly with
	// the operator's key — so we abort instead. The abort surfaces as an error
	// which the engine records as a failed (harness_error) iteration.
	proxyEnabled := r.cfg.Proxy != nil
	proxyURL, proxyToken := "", ""
	if proxyEnabled {
		taskID, epicID := "", ""
		if hasCurrentGoal {
			taskID, epicID = currentGoal.Key, currentGoal.Key
			if currentGoal.ParentKey != "" {
				taskID, epicID, err = resolveNativeTaskAttribution(ctx, r.cfg.Tasks, ag.Name, currentGoal.Key)
				if err != nil {
					return fail(fmt.Errorf("resolve current agent goal attribution: %w", err))
				}
			}
		}
		name, tag := imageNameTag(ag.ImageRef)
		tok, mErr := r.cfg.Proxy.MintToken(ag.Name, iterationID, name, tag, ag.ImageDigest)
		if mErr != nil {
			// Nothing was minted, so there is no token to revoke.
			return fail(fmt.Errorf("proxy token unavailable, refusing to run without proxy: %w", mErr))
		}
		if taskID != "" {
			if r.cfg.Proxy.UpdateTask(tok, taskID, epicID) != 1 {
				r.cfg.Proxy.RevokeToken(tok)
				return fail(fmt.Errorf("attribute proxy token to current agent goal"))
			}
		}
		// A token exists from here on. If we abort below (ProxyBaseURL empty) it
		// must still be revoked even though prepare is about to return an error;
		// on the success path the caller takes ownership and defers the revoke
		// for the rest of the iteration instead.
		proxyURL = r.cfg.Proxy.ProxyBaseURL()
		if u, ok := r.cfg.Proxy.(interface{ ProxyBaseURLForToken(string) string }); ok {
			proxyURL = u.ProxyBaseURLForToken(tok)
		}
		if proxyURL == "" {
			r.cfg.Proxy.RevokeToken(tok)
			return fail(fmt.Errorf("proxy base URL unavailable, refusing to run without proxy"))
		}
		proxyToken = tok
	}

	hargv, henv, err := adapter.Command(cwd, l.PromptPath(iterationID),
		harness.Config{Model: ag.Model, Effort: ag.Effort, Interactive: ag.Interactive, SessionID: iterationID, ProxyURL: proxyURL, Bare: bare})
	if err != nil {
		if proxyToken != "" {
			r.cfg.Proxy.RevokeToken(proxyToken)
		}
		return fail(err)
	}
	hargv = append(hargv, activated.Skills.Args...)

	base := append(os.Environ(), henv...)
	binDir := l.BinDir()
	if bare {
		binDir = ""
	}
	env := BuildEnv(base, binDir, ag.Name, iterationID, l.Sock(), proxyEnabled, proxyURL, proxyToken, ag.Env, secrets)
	env, err = mergeSkillLaunchEnv(env, activated.Skills.Env)
	if err != nil {
		if proxyToken != "" {
			r.cfg.Proxy.RevokeToken(proxyToken)
		}
		return fail(err)
	}
	if _, err := harness.FindExecutable(adapter.Executable(), env, cwd); err != nil {
		if proxyToken != "" {
			r.cfg.Proxy.RevokeToken(proxyToken)
		}
		return fail(fmt.Errorf("harness executable %q not found", adapter.Type()))
	}
	if !bare {
		python3, err := harness.FindExecutable("python3", env, cwd)
		if err != nil {
			if proxyToken != "" {
				r.cfg.Proxy.RevokeToken(proxyToken)
			}
			return fail(errors.New("python3 is required in the iteration environment for agent tool scripts"))
		}
		env, err = mergeSkillLaunchEnv(env, []string{"TARIBOY_PYTHON3=" + python3})
		if err != nil {
			if proxyToken != "" {
				r.cfg.Proxy.RevokeToken(proxyToken)
			}
			return fail(err)
		}
	}
	bash, err := harness.FindExecutable("bash", env, cwd)
	if err != nil {
		if proxyToken != "" {
			r.cfg.Proxy.RevokeToken(proxyToken)
		}
		return fail(errors.New("bash is required in the iteration environment for agent shell scripts"))
	}
	hargv = agentShellCommand(bash, filepath.Dir(r.cfg.AgentsDir), l, hargv)

	// Compose the shim command.
	tmuxSession := ""
	if ag.Interactive {
		tmuxSession = ag.Name
	}
	shimArgv := []string{
		r.cfg.ShimBin,
		"--iteration-dir", l.IterationDir(iterationID),
		"--agent", ag.Name,
		"--iteration-id", iterationID,
		"--hard-timeout-s", fmt.Sprintf("%d", hardTimeout(ag)),
		"--tmux-session", tmuxSession,
		"--shim-sock", l.ShimSock(),
		"--",
	}
	shimArgv = append(shimArgv, hargv...)

	// Redact secret material for the audit record only. secrets carries the
	// per-agent secret key/values; proxyToken has no env key of its own (it is
	// embedded in the *_BASE_URL values) so it is passed as an extra value.
	maskedArgv, maskedEnv := maskLaunch(shimArgv, env, secrets, proxyToken)

	return preparedRun{
		shimArgv:   shimArgv,
		env:        env,
		cwd:        cwd,
		proxyToken: proxyToken,
		maskedArgv: maskedArgv,
		maskedEnv:  maskedEnv,
	}, nil
}

// await polls result.json (and the shim RPC) until the iteration finishes,
// enforcing the soft timeout via the shim kill RPC.
func (r *ShimRunner) await(ctx context.Context, ag agent.Agent, l agentdir.Layout, iterationID string) (Outcome, error) {
	start := r.cfg.Clock()
	_ = start // retained as a clock sample for backward-compatible test seams.
	softHit := false
	lastHardDeadline := ""
	doneGrace := doneGraceTracker{}
	kill := cooperativeKill{}
	tk := time.NewTicker(r.cfg.PollInterval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			if res, ok := readResult(l.ResultPath(iterationID)); ok {
				it, err := r.cfg.Store.GetIteration(ag.Name, iterationID)
				done, productive := r.doneAndProductive(ag.Name, iterationID)
				soft := err == nil && it.TimeoutTriggeredAt != nil
				return Outcome{
					Status:     Classify(res.ExitCode, done, soft, res.TerminationReason == "hard_timeout"),
					ExitCode:   res.ExitCode,
					DoneFlag:   done,
					Productive: productive,
					CPUMs:      res.CPUMs,
					MemPeakKB:  res.MemPeakKB,
				}, nil
			}
			if it, err := r.cfg.Store.GetIteration(ag.Name, iterationID); err == nil && it.Status == "running" {
				return Outcome{}, ErrIterationDetached
			}
			return Outcome{}, ctx.Err()
		case <-tk.C:
		}
		if res, ok := readResult(l.ResultPath(iterationID)); ok {
			it, err := r.cfg.Store.GetIteration(ag.Name, iterationID)
			done, productive := r.doneAndProductive(ag.Name, iterationID)
			soft := softHit || (err == nil && it.TimeoutTriggeredAt != nil)
			return Outcome{
				Status:     Classify(res.ExitCode, done, soft, res.TerminationReason == "hard_timeout"),
				ExitCode:   res.ExitCode,
				DoneFlag:   done,
				Productive: productive,
				CPUMs:      res.CPUMs,
				MemPeakKB:  res.MemPeakKB,
			}, nil
		}
		it, err := r.cfg.Store.GetIteration(ag.Name, iterationID)
		if err == nil {
			if it.HardTimeoutDeadline != nil && *it.HardTimeoutDeadline != lastHardDeadline {
				if err := shim.Dial(l.ShimSock()).SetHardDeadline(*it.HardTimeoutDeadline); err == nil {
					lastHardDeadline = *it.HardTimeoutDeadline
				}
			}
			if triggered, err := enforceSoftTimeout(r.cfg.Store, it, r.cfg.Clock(), func() error {
				return kill.send(func() error { return shim.Dial(l.ShimSock()).Kill() })
			}); err == nil && triggered {
				softHit = true
			}
		}
		doneGrace.observe(r.doneFlag(ag.Name, iterationID), r.cfg.Clock(), r.cfg.DoneGrace, func() error {
			return kill.send(func() error { return shim.Dial(l.ShimSock()).Kill() })
		})
	}
}

// enforceSoftTimeout gives the durable marker ownership of the kill. This
// keeps concurrent observers (including adoption after a daemon restart) from
// sending duplicate termination requests while still honoring the latest
// persisted deadline written by an extension.
func enforceSoftTimeout(st *agent.Store, it agent.Iteration, now time.Time, kill func() error) (bool, error) {
	if it.TimeoutTriggeredAt != nil {
		return true, nil
	}
	if it.TimeoutDeadline == nil {
		return false, nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, *it.TimeoutDeadline)
	if err != nil {
		return false, err
	}
	if now.Before(deadline) {
		return false, nil
	}
	marked, err := st.MarkIterationTimeoutTriggered(it.Agent, it.ID, *it.TimeoutDeadline, now.UTC().Format(time.RFC3339Nano))
	if err != nil || !marked {
		return false, err
	}
	_ = kill()
	return true, nil
}

func (r *ShimRunner) doneFlag(agentName, id string) bool {
	it, err := r.cfg.Store.GetIteration(agentName, id)
	return err == nil && it.DoneFlag
}

// doneAndProductive reads both flags in one shot for the final Outcome. On a read
// error productive defaults to true, matching the column's DEFAULT 1 and the rule
// that only an explicit `--idle` declaration makes an iteration non-productive.
func (r *ShimRunner) doneAndProductive(agentName, id string) (done, productive bool) {
	it, err := r.cfg.Store.GetIteration(agentName, id)
	if err != nil {
		return false, true
	}
	return it.DoneFlag, it.Productive
}

func readResult(path string) (shim.IterationResult, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return shim.IterationResult{}, false
	}
	var res shim.IterationResult
	if jsonUnmarshal(data, &res) != nil {
		return shim.IterationResult{}, false
	}
	return res, true
}

// harnessExitReason describes a non-zero harness exit for the audit log: the
// exit code plus the tail of harness stderr. Interactive agents get only the
// code, because their stderr file is a tmux TUI capture.
func harnessExitReason(exitCode int, stderrPath string, interactive bool) string {
	reason := fmt.Sprintf("harness exited with code %d", exitCode)
	if interactive {
		return reason
	}
	b, err := os.ReadFile(stderrPath)
	if err != nil {
		return reason
	}
	const maxTail = 2048
	if len(b) > maxTail {
		b = b[len(b)-maxTail:]
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	if tail := strings.TrimSpace(strings.Join(lines, "\n")); tail != "" {
		reason += ": " + tail
	}
	return reason
}

func agentCwd(ag agent.Agent, l agentdir.Layout) string {
	if ag.Cwd != "" {
		return ag.Cwd
	}
	return l.Workdir()
}

func hardTimeout(ag agent.Agent) int {
	if ag.HardTimeoutS > 0 {
		return ag.HardTimeoutS
	}
	if ag.TimeoutS > 0 {
		return ag.TimeoutS + 60
	}
	return 0 // shim applies its own 60s default
}

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// imageNameTag splits an image ref "name:tag" for attribution; a parse failure
// degrades to empty fields rather than blocking the iteration.
func imageNameTag(ref string) (string, string) {
	r, err := image.ParseRef(ref)
	if err != nil {
		return "", ""
	}
	return r.Name, r.Tag
}
