package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/agentapi"
	"github.com/alekzonder/tariboy/internal/agentdir"
	"github.com/alekzonder/tariboy/internal/audit"
	"github.com/alekzonder/tariboy/internal/bus"
	"github.com/alekzonder/tariboy/internal/events"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/imagefile"
	"github.com/alekzonder/tariboy/internal/paths"
	"github.com/alekzonder/tariboy/internal/plugincaps"
	"github.com/alekzonder/tariboy/internal/plugins"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/schedule"
	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/shim"
	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/telemetry"
	"golang.org/x/sys/unix"
)

// GroupProvisioner is the subset of groups.Provisioner the manager needs to
// auto-provision an agent's group on `agent.run --group` before its loop starts
// (spec §4.4). nil disables group provisioning.
type GroupProvisioner interface {
	EnsureGroup(name, lead string) error
	Reconcile(name string) error
	SharedDir(group string) string
	Inspect(name string) (map[string]any, error)
}

type ScriptResultNotifier interface {
	Wake()
}

type ManagerConfig struct {
	AgentsDir     string
	RuntimeDir    string
	ShimBin       string
	ImgStore      *image.Store
	Store         *agent.Store
	Log           *slog.Logger
	Clock         func() time.Time
	DoneGrace     time.Duration
	Spawner       Spawner
	RunnerFactory func(agent.Agent) IterationRunner
	ConnectShim   func(string) (*shim.Client, error)
	// BeforeRestartLaunch is a test seam invoked before a post-adoption
	// interactive Restart attempts to acquire its launch gate.
	BeforeRestartLaunch func()
	Bus                 *bus.Bus
	Schedules           *schedule.Store
	Scripts             *script.Store
	ScriptResults       ScriptResultNotifier
	ScriptTimeout       time.Duration
	Emit                func(events.Event)
	Proxy               ProxyBinder
	Groups              GroupProvisioner
	// AuditFor returns the shared per-agent audit recorder. Wired by the daemon to
	// its audit.Registry so recordEvent, the engine lifecycle sink, and the
	// runner's log tailer all share one *audit.Log per agent. Nil disables audit.
	AuditFor func(agent string) Recorder
	// HasTmuxSession / KillTmuxSession are seams for orphan-session reaping on
	// start (interactive agents). Injectable so tests avoid a real tmux; nil uses
	// the real tmux commands.
	HasTmuxSession  func(session string) bool
	KillTmuxSession func(session string) error
	// Metrics records per-iteration telemetry (spec §14); nil is a valid no-op.
	Metrics *telemetry.Metrics
	// UsageLookup is a best-effort per-iteration token/cost accessor (wired by
	// the daemon to aiproxy.Store.IterationUsage) used for OTel span attributes;
	// nil skips the attributes.
	UsageLookup func(iteration string) (int, int, float64)
	// UsageFlush synchronously persists queued proxy usage before terminal span aggregation.
	UsageFlush func()
	// PrepareImageBridge is injectable for activation boundary tests. Nil uses
	// agentdir.PrepareImageBridge.
	PrepareImageBridge func(string, string, []image.ManifestSkill, agentdir.BridgePlan) error
	// OnIterationClose, if set, is invoked exactly once per finished iteration
	// (every terminal outcome) with (agent, iterationID), after the iteration's
	// final status is persisted. The daemon wires this to gzip the AI-proxy
	// transcript (spec §9/§12) without loop importing aiproxy directly.
	OnIterationClose func(agent, iterationID string)
	// GoalSignal coalesces agent configuration changes into a goal scan.
	GoalSignal func()
	// IterationCompleted requests the next goal wake after terminal persistence.
	IterationCompleted func(agent, iterationID string)
	CurrentGoal        func(string, time.Time) (tasks.Task, bool, error)
	SetGoal            func(agent, key string, activate func() (func(), error)) error
	// ProvidedChannels returns provider-declared channels drawn from installed
	// plugin manifests, so `tools sources` can list and annotate provider
	// channels even before their channel row exists (spec §6.1). Wired by the
	// daemon (loop must not import plugins directly); nil yields no annotations.
	ProvidedChannels func() ([]agentapi.ProvidedChannel, error)
	ExternalPlugins  plugincaps.ExternalResolver
	Tasks            registry.TaskControl
	// iterationStore overrides Store for terminal-status finalization only
	// (finalizeIteration). Unexported on purpose: it is a test seam for
	// injecting an UpdateIteration failure, not daemon configuration. Unset
	// means Store.
	iterationStore iterationStore
}

type runtime struct {
	engine    *Engine
	cancel    context.CancelFunc
	apiServer *agentapi.Server
}

var errIterationAdopting = errors.New("agent has an adopted running iteration")

type iterationTarget struct {
	id      string
	sock    string
	runtime *runtime
}

// Reattach liveness tuning. These are package vars (not consts) so tests can
// shorten them; adopt polls result.json every adoptPollInterval and probes the
// shim socket every adoptProbeInterval, giving up after adoptMaxMisses probes.
var (
	adoptPollInterval  = 200 * time.Millisecond
	adoptProbeInterval = time.Second
	adoptProbeTimeout  = time.Second
	adoptMaxMisses     = 3
	shutdownWait       = 10 * time.Second
	scriptPollInterval = 100 * time.Millisecond
	scriptCancelGrace  = 2 * time.Second
)

type Manager struct {
	cfg  ManagerConfig
	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup
	mu   sync.Mutex
	runs map[string]*runtime
	// toolsAPI holds per-agent tools servers that are bound and serving but not
	// (yet) owned by a runtime: the daemon binds them at boot for every agent
	// with an unfinished iteration, whose engine may start much later or never.
	// A name lives in exactly one of toolsAPI and runs — start() moves it — so
	// a socket path always has a single owner and a single listener.
	toolsAPI   map[string]*agentapi.Server
	adopting   map[string]agentdir.LiveIteration
	restarting map[string]bool
	staleKills map[string]string
	rng        *rand.Rand
	// scriptsWake wakes the durable script supervisor after a new record is
	// created. It is deliberately buffered: callers never wait for a process
	// to start in order to receive a script ID.
	scriptsWake   chan struct{}
	scriptRuns    map[string]*exec.Cmd
	scriptCancels map[string]bool
}

// ExtendIterationTimeout makes an extension durable before attempting the
// best-effort shim resync. The runner will retry a pending hard-deadline sync,
// so a transient/unavailable shim must not turn a committed extension into an
// API failure.
func (m *Manager) ExtendIterationTimeout(name, id string) (registry.IterationTimeoutExtension, error) {
	now := m.cfg.Clock()
	it, err := m.cfg.Store.ExtendIterationTimeout(name, id, now)
	if err != nil {
		return registry.IterationTimeoutExtension{}, err
	}
	result := registry.IterationTimeoutExtension{
		TimeoutDeadline: *it.TimeoutDeadline, HardTimeoutDeadline: *it.HardTimeoutDeadline,
		TimeoutExtensions: it.TimeoutExtensions, ShimSync: "pending",
	}
	l := agentdir.New(m.cfg.AgentsDir, name).WithRuntime(m.cfg.RuntimeDir)
	if err := shim.Dial(l.ShimSock()).SetHardDeadline(result.HardTimeoutDeadline); err == nil {
		result.ShimSync = "success"
	}
	data := map[string]any{"timeout_deadline": result.TimeoutDeadline,
		"hard_timeout_deadline": result.HardTimeoutDeadline, "timeout_extensions": result.TimeoutExtensions,
		"shim_sync": result.ShimSync}
	if m.cfg.AuditFor != nil {
		if rec := m.cfg.AuditFor(name); rec != nil {
			rec.Record("iteration_timeout_extended", "system", id, data)
		}
	}
	if m.cfg.Emit != nil {
		m.cfg.Emit(events.Event{Agent: name, Type: "iteration_timeout_extended",
			Time: now.UTC().Format(time.RFC3339Nano), Data: map[string]any{"id": id, "shim_sync": result.ShimSync}})
	}
	return result, nil
}

func NewManager(cfg ManagerConfig) *Manager {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.DoneGrace <= 0 {
		cfg.DoneGrace = defaultDoneGrace
	}
	if cfg.Spawner == nil {
		cfg.Spawner = ExecSpawner{}
	}
	if cfg.RuntimeDir == "" && cfg.AgentsDir != "" {
		cfg.RuntimeDir = defaultRuntimeDir(cfg.AgentsDir)
		_ = os.MkdirAll(cfg.RuntimeDir, 0o700)
	}
	if cfg.HasTmuxSession == nil {
		cfg.HasTmuxSession = tmuxHasSession
	}
	if cfg.KillTmuxSession == nil {
		cfg.KillTmuxSession = tmuxKillSession
	}
	if cfg.ConnectShim == nil {
		cfg.ConnectShim = shim.Connect
	}
	return &Manager{
		cfg: cfg, runs: map[string]*runtime{}, toolsAPI: map[string]*agentapi.Server{},
		adopting:   map[string]agentdir.LiveIteration{},
		restarting: map[string]bool{}, staleKills: map[string]string{},
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
		scriptsWake: make(chan struct{}, 1), scriptRuns: map[string]*exec.Cmd{}, scriptCancels: map[string]bool{},
	}
}

func (m *Manager) signalGoals() {
	if m.cfg.GoalSignal != nil {
		m.cfg.GoalSignal()
	}
}

func (m *Manager) iterationCompleted(agentName, iterationID string) {
	if m.cfg.IterationCompleted != nil {
		m.cfg.IterationCompleted(agentName, iterationID)
	}
}

func defaultRuntimeDir(agentsDir string) string {
	base := agentsDir
	if filepath.Base(agentsDir) == "agents" {
		base = filepath.Dir(agentsDir)
	}
	return paths.New(base).RuntimeDir()
}

// reapOrphanSessions kills any agent-named tmux session that has no live
// iteration to adopt and no running-status iteration in the store. A tmux session
// only ever exists for an interactive incarnation, so a session with no live
// iteration is always an orphan — independent of the agent's CURRENT interactive
// flag (which may have since flipped, as in the manager jam). Such a leftover
// survives retention pruning and daemon restarts and would otherwise block every
// future iteration with a duplicate-session error. adopting names agents whose
// live iteration is being re-adopted.
func (m *Manager) reapOrphanSessions(agents []agent.Agent, adopting map[string][]<-chan struct{}) {
	for _, a := range agents {
		if _, beingAdopted := adopting[a.Name]; beingAdopted {
			continue
		}
		if !m.cfg.HasTmuxSession(a.Name) {
			continue
		}
		if m.hasRunningIteration(a.Name) {
			continue
		}
		if err := m.cfg.KillTmuxSession(a.Name); err != nil {
			m.cfg.Log.Warn("reap orphan tmux session", "agent", a.Name, "err", err)
			continue
		}
		m.cfg.Log.Info("reaped orphan tmux session", "agent", a.Name)
		if m.cfg.AuditFor != nil {
			if rec := m.cfg.AuditFor(a.Name); rec != nil {
				rec.Record("session_reaped", "system", "", map[string]any{"reason": "orphan_no_running_iteration"})
			}
		}
	}
}

func (m *Manager) hasRunningIteration(name string) bool {
	its, err := m.cfg.Store.ListIterations(name)
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

func (m *Manager) runnerFor(ag agent.Agent) IterationRunner {
	if m.cfg.RunnerFactory != nil {
		return m.cfg.RunnerFactory(ag)
	}
	return NewShimRunner(RunnerConfig{
		AgentsDir: m.cfg.AgentsDir, RuntimeDir: m.cfg.RuntimeDir, ShimBin: m.cfg.ShimBin,
		ImgStore: m.cfg.ImgStore, Store: m.cfg.Store, Spawner: m.cfg.Spawner, Clock: m.cfg.Clock,
		DoneGrace: m.cfg.DoneGrace, Logger: m.cfg.Log, Bus: m.cfg.Bus, Proxy: m.cfg.Proxy, AuditFor: m.cfg.AuditFor,
		CurrentGoal: m.cfg.CurrentGoal, Tasks: m.cfg.Tasks,
	})
}

// refreshShims rewrites each agent's bin shims against the running daemon's
// skill scripts. Disabled agents are included — they may be enabled later and
// must find a working client then. One unwritable agent dir is logged and
// skipped: daemon startup is not a place to die over a single directory.
//
// Each agent validates its own capability-owned direct scripts before changing
// its shims, so a missing script skips that agent without blocking the fleet.
func (m *Manager) refreshShims(agents []agent.Agent) map[string]bool {
	failed := make(map[string]bool)
	for _, a := range agents {
		l := agentdir.New(m.cfg.AgentsDir, a.Name)
		if err := agentdir.WriteShims(l, a); err != nil {
			failed[a.Name] = true
			m.cfg.Log.Error("refresh agent shims", "agent", a.Name, "err", err)
		}
	}
	return failed
}

// StartAll reattaches live iterations and starts every persisted-running agent.
// An agent that owns a live iteration has its engine start deferred until the
// adoption completes, so we never launch a second concurrent shim for it.
func (m *Manager) StartAll(ctx context.Context) error {
	cctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.ctx = cctx
	m.stop = cancel
	m.mu.Unlock()
	agents, err := m.cfg.Store.List()
	if err != nil {
		return err
	}
	// Repoint every agent's bin shims at this daemon's client before anything
	// can run: they were written once at provision time with an absolute path
	// to the then-current release, so an upgraded daemon would otherwise keep
	// serving agents frozen (and flag-incompatible) client scripts. This
	// stays above the script supervisor: its first scan runs before the first
	// tick, so a script already due at daemon start would otherwise inherit a
	// PATH pointing at the stale shims.
	shimRefreshFailed := m.refreshShims(agents)

	if m.cfg.Scripts != nil {
		if err := m.cfg.Scripts.RecoverRunning(); err != nil {
			return fmt.Errorf("recover scripts: %w", err)
		}
		m.startScriptSupervisor(cctx)
	}

	live, err := agentdir.ListLive(m.cfg.AgentsDir, m.cfg.RuntimeDir)
	if err != nil {
		return err
	}
	// Map each agent to the adoption(s) that must finish before its engine may
	// start. A single agent normally owns at most one live iteration.
	adopting := map[string][]<-chan struct{}{}
	for _, li := range live {
		adopting[li.Agent] = append(adopting[li.Agent], m.adopt(cctx, li))
	}
	// An unfinished iteration keeps running across the restart, and its harness
	// reaches the daemon ONLY through the per-agent tools socket — including
	// i-am-done. Binding it here, after adoption has registered the live
	// iteration and before anything waits on that adoption, is what breaks the
	// deadlock: the engine's start is deferred until the iteration ends, the
	// iteration cannot end without i-am-done, and i-am-done needs this socket.
	// A disabled agent (iteration launched by hand) never gets an engine at all,
	// so for it this is the only bind there will ever be.
	m.bindLiveToolsSockets(agents, adopting)
	m.reapOrphanSessions(agents, adopting)
	for _, a := range agents {
		if shimRefreshFailed[a.Name] {
			continue
		}
		if dones, ok := adopting[a.Name]; ok {
			m.startAfter(cctx, a.Name, dones)
			continue
		}
		if !a.Enabled {
			continue
		}
		if err := m.start(a); err != nil {
			m.cfg.Log.Error("reattach start", "agent", a.Name, "err", err)
		}
	}
	return nil
}

// bindLiveToolsSockets binds the tools socket of every agent that owns a live
// iteration, independently of whether its engine will start. A failure is
// logged and not fatal: an enabled agent's start() binds the same path and
// fails loudly there, while for a disabled one there is nothing else to fail.
func (m *Manager) bindLiveToolsSockets(agents []agent.Agent, adopting map[string][]<-chan struct{}) {
	for _, a := range agents {
		if _, live := adopting[a.Name]; !live {
			continue
		}
		l := agentdir.New(m.cfg.AgentsDir, a.Name).WithRuntime(m.cfg.RuntimeDir)
		m.mu.Lock()
		_, err := m.bindToolsSocketLocked(a, l)
		m.mu.Unlock()
		if err != nil {
			m.cfg.Log.Error("bind agent tools socket for live iteration",
				"agent", a.Name, "sock", l.Sock(), "err", err)
		}
	}
}

// bindToolsSocketLocked returns the one server that owns the agent's tools
// socket, binding and serving it on first call. Re-binding a path that is
// already served would unlink the live socket file and leave its listener
// serving an orphan (agentapi.Listen removes the path before binding), so every
// caller goes through here instead of calling Listen itself. m.mu must be held.
func (m *Manager) bindToolsSocketLocked(ag agent.Agent, l agentdir.Layout) (*agentapi.Server, error) {
	if rt, ok := m.runs[ag.Name]; ok && rt.apiServer != nil {
		return rt.apiServer, nil
	}
	if srv, ok := m.toolsAPI[ag.Name]; ok {
		return srv, nil
	}
	srv := m.newToolsAPIServer(ag, l)
	ln, err := srv.Listen(l.Sock())
	if err != nil {
		return nil, err
	}
	m.toolsAPI[ag.Name] = srv
	go func() {
		if err := srv.ServeListener(ln); err != nil {
			m.cfg.Log.Error("tools socket serve", "agent", ag.Name, "err", err)
		}
	}()
	return srv, nil
}

// currentIterationID resolves the iteration a tools call belongs to. The engine
// is authoritative while it runs one; otherwise the live iteration being
// adopted answers, so a harness that outlived a daemon restart is still
// recognized as the caller of its own iteration and can close it.
func (m *Manager) currentIterationID(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt := m.runs[name]; rt != nil && rt.engine != nil {
		if id := rt.engine.CurrentIterationID(); id != "" {
			return id
		}
	}
	if live, ok := m.adopting[name]; ok {
		return live.ID
	}
	return ""
}

// startAfter starts an agent's engine only once every pending adoption for it
// has finished (or the manager is shutting down), serializing reattach so a
// freshly started engine does not race the adopted iteration's shim.
func (m *Manager) startAfter(ctx context.Context, name string, dones []<-chan struct{}) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for _, d := range dones {
			select {
			case <-d:
			case <-ctx.Done():
				return
			}
		}
		ag, err := m.cfg.Store.Get(name)
		if err != nil {
			m.cfg.Log.Error("reattach load agent", "agent", name, "err", err)
			return
		}
		if !ag.Enabled {
			return
		}
		if err := m.start(ag); err != nil {
			m.cfg.Log.Error("reattach start", "agent", name, "err", err)
			return
		}
		m.mu.Lock()
		restart := m.restarting[name]
		rt := m.runs[name]
		m.mu.Unlock()
		if restart && ag.Interactive && rt != nil {
			rt.engine.triggerGuarded("", func() (func(), bool) {
				if m.cfg.BeforeRestartLaunch != nil {
					m.cfg.BeforeRestartLaunch()
				}
				return m.acquireRestartLaunch(name)
			})
		}
	}()
}

// acquireRestartLaunch linearizes a post-adoption interactive launch with
// Stop. The caller holds the returned release function until the engine has
// created the iteration and is ready to enter its runner.
func (m *Manager) acquireRestartLaunch(name string) (func(), bool) {
	m.mu.Lock()
	if !m.restarting[name] {
		m.mu.Unlock()
		return nil, false
	}
	ag, err := m.cfg.Store.Get(name)
	if err != nil || !ag.Enabled {
		delete(m.restarting, name)
		m.mu.Unlock()
		if err != nil {
			m.cfg.Log.Error("reattach restart load agent", "agent", name, "err", err)
		}
		return nil, false
	}
	delete(m.restarting, name)
	return m.mu.Unlock, true
}

// adopt waits for a live iteration's result.json and records the outcome. If the
// shim socket stops answering Status probes and no result.json ever appears (a
// SIGKILLed shim leaves a stale shim.sock), the iteration is classified
// terminally as harness_error and the stale socket is removed. The returned
// channel is closed once adoption finishes.
func (m *Manager) adopt(ctx context.Context, li agentdir.LiveIteration) <-chan struct{} {
	done := make(chan struct{})
	m.mu.Lock()
	m.adopting[li.Agent] = li
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			if current, ok := m.adopting[li.Agent]; ok && current.ID == li.ID {
				delete(m.adopting, li.Agent)
			}
			m.mu.Unlock()
			close(done)
		}()
		l := agentdir.New(m.cfg.AgentsDir, li.Agent).WithRuntime(m.cfg.RuntimeDir)
		tk := time.NewTicker(adoptPollInterval)
		defer tk.Stop()
		misses := 0
		lastProbe := time.Time{}
		doneGrace := doneGraceTracker{}
		kill := cooperativeKill{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
			if res, ok := readResult(li.ResultPath); ok {
				m.recordAdopted(l, li, res)
				return
			}
			it, iterationErr := m.cfg.Store.GetIteration(li.Agent, li.ID)
			if iterationErr == nil {
				if _, err := enforceSoftTimeout(m.cfg.Store, it, m.cfg.Clock(), func() error {
					return kill.send(func() error { return shim.Dial(li.ShimSock).Kill() })
				}); err != nil {
					m.cfg.Log.Debug("enforce adopted shim soft deadline", "agent", li.Agent, "id", li.ID, "err", err)
				}
				doneGrace.observe(it.DoneFlag, m.cfg.Clock(), m.cfg.DoneGrace, func() error {
					return kill.send(func() error { return shim.Dial(li.ShimSock).Kill() })
				})
			}
			if time.Since(lastProbe) < adoptProbeInterval {
				continue
			}
			lastProbe = time.Now()
			c := shim.Dial(li.ShimSock)
			c.Timeout = adoptProbeTimeout
			// The DB is authoritative across a daemon restart. Replay the absolute
			// hard deadline before probing so an extension committed before an RPC
			// failure is eventually enforced by the surviving shim.
			if iterationErr == nil && it.HardTimeoutDeadline != nil {
				if err := c.SetHardDeadline(*it.HardTimeoutDeadline); err != nil {
					m.cfg.Log.Debug("resync adopted shim deadline", "agent", li.Agent, "id", li.ID, "err", err)
				}
			}
			if _, err := c.Status(); err != nil {
				misses++
				if misses >= adoptMaxMisses {
					// One last check: a result may have landed between the poll
					// above and the failing probe.
					if res, ok := readResult(li.ResultPath); ok {
						m.recordAdopted(l, li, res)
						return
					}
					m.recordStaleAdoption(l, li)
					return
				}
				continue
			}
			misses = 0
		}
	}()
	return done
}

// iterationStore is the narrow slice of *agent.Store that terminal-status
// finalization needs. It exists so a failing terminal CAS can be injected
// (ManagerConfig.iterationStore); every other call site keeps using the
// concrete store.
type iterationStore interface {
	GetIteration(agentName, id string) (agent.Iteration, error)
	FinalizeRunningIteration(it agent.Iteration) (bool, error)
}

// iterations returns the store used for terminal-status finalization.
func (m *Manager) iterations() iterationStore {
	if m.cfg.iterationStore != nil {
		return m.cfg.iterationStore
	}
	return m.cfg.Store
}

// errIterationLookup marks a finalizeIteration failure that happened while
// reading the iteration, before anything on disk or in the store was touched.
var errIterationLookup = errors.New("read iteration")

// finalizeIteration commits a terminal status for one running iteration and
// drops its shim.sock as a single all-or-nothing step. It reports whether the
// terminal status was committed and whether the row was already terminal
// before cleanup began.
//
// Two facts pull the order in opposite directions. The iteration's status is
// what every observer polls, so the socket must be gone *before* the terminal
// status becomes visible — otherwise a finished iteration still exposes a
// shim.sock nobody serves. But agentdir.ListLive recognises an iteration to
// re-classify only by a present shim.sock, so a *failed* status write must
// leave that marker on disk — otherwise the row stays "running" forever,
// hasRunningIteration stays true, and the orphaned tmux session is never
// reaped. No ordering of two independent operations satisfies both; removing
// first and restoring the marker when the write fails satisfies both, and is
// why all three call sites go through here instead of ordering the pair
// themselves.
//
// The removal stays inside the "still running" guard: the shim socket is
// per-agent, not per-iteration, so once this iteration is terminal the file at
// that path may already belong to the agent's next iteration.
func (m *Manager) finalizeIteration(
	l agentdir.Layout,
	agentName, id string,
	apply func(*agent.Iteration),
) (committed, terminalBeforeCleanup bool, err error) {
	it, err := m.iterations().GetIteration(agentName, id)
	if err != nil {
		return false, false, fmt.Errorf("%w: %v", errIterationLookup, err)
	}
	if it.Status != "running" {
		return false, true, nil
	}
	apply(&it)
	_ = os.Remove(l.ShimSock())
	committed, err = m.iterations().FinalizeRunningIteration(it)
	if err != nil {
		// Roll the marker back so adoption can retry this iteration. A plain
		// file is enough: ListLive only stats the path, adoption's probe of a
		// non-socket fails exactly like a dead shim, and every shim removes a
		// leftover before binding (internal/shim/shim.go).
		if f, cerr := os.OpenFile(l.ShimSock(), os.O_CREATE|os.O_WRONLY, 0o600); cerr == nil {
			_ = f.Close()
		} else {
			m.cfg.Log.Error("restore shim.sock marker after failed terminal status",
				"agent", agentName, "id", id, "err", cerr)
		}
		return false, false, err
	}
	return committed, false, nil
}

// terminalHarnessError shapes an iteration into the harness_error outcome used
// when a shim disappears without ever writing a result.
func (m *Manager) terminalHarnessError(it *agent.Iteration) {
	ec := -1
	it.Status = "harness_error"
	it.EndedAt = m.cfg.Clock().Format(time.RFC3339)
	it.ExitCode = &ec
}

// recordAdopted persists a completed adopted iteration from its result.json.
func (m *Manager) recordAdopted(l agentdir.Layout, li agentdir.LiveIteration, res shim.IterationResult) {
	status := ""
	committed, _, err := m.finalizeIteration(l, li.Agent, li.ID, func(it *agent.Iteration) {
		ec, cpu, mem := res.ExitCode, res.CPUMs, res.MemPeakKB
		it.Status = Classify(res.ExitCode, it.DoneFlag, it.TimeoutTriggeredAt != nil, res.TerminationReason == "hard_timeout")
		it.EndedAt = m.cfg.Clock().Format(time.RFC3339)
		it.ExitCode, it.CPUMs, it.MemPeakKB = &ec, &cpu, &mem
		status = it.Status
	})
	if err != nil && !errors.Is(err, errIterationLookup) {
		m.cfg.Log.Error("record adopted iteration: status not committed, shim.sock kept for re-adoption",
			"agent", li.Agent, "id", li.ID, "status", status, "err", err)
	}
	if !committed {
		return
	}
	if m.cfg.Proxy != nil {
		m.cfg.Proxy.RevokeIteration(li.ID)
	}
	m.iterationCompleted(li.Agent, li.ID)
	m.cfg.Log.Info("adopted live iteration", "agent", li.Agent, "id", li.ID, "status", status)
}

// recordStaleAdoption terminally classifies an iteration whose shim vanished
// without ever writing result.json (harness_error, exit_code=-1) and removes the
// stale shim.sock so reattach cannot spin on it forever.
func (m *Manager) recordStaleAdoption(l agentdir.Layout, li agentdir.LiveIteration) {
	committed, terminalBeforeCleanup, err := m.finalizeIteration(l, li.Agent, li.ID, m.terminalHarnessError)
	switch {
	case err == nil && terminalBeforeCleanup:
		// Already terminal, yet ListLive still offered this iteration — so the
		// socket outlived a committed status. Drop it (as this function always
		// did) or reattach spins on it forever. Safe here specifically because
		// adoption gates the agent's next iteration: no newer shim can own the
		// per-agent socket path while we are adopting.
		_ = os.Remove(l.ShimSock())
	case err != nil && !errors.Is(err, errIterationLookup):
		m.cfg.Log.Error("record stale adoption: status not committed, shim.sock kept for re-adoption",
			"agent", li.Agent, "id", li.ID, "err", err)
	}
	if committed && m.cfg.Proxy != nil {
		m.cfg.Proxy.RevokeIteration(li.ID)
	}
	if committed {
		m.iterationCompleted(li.Agent, li.ID)
	}
	m.cfg.Log.Warn("adopted iteration abandoned: shim not responding", "agent", li.Agent, "id", li.ID)
}

func (m *Manager) Run(spec registry.RunSpec) (string, error) {
	var name string
	err := image.WithPublicationGate(func() error {
		var err error
		name, err = m.run(spec)
		return err
	})
	return name, err
}

func (m *Manager) run(spec registry.RunSpec) (string, error) {
	ref, err := image.ParseRef(spec.ImageRef)
	if err != nil {
		return "", err
	}
	archive, err := m.cfg.ImgStore.ArchiveBytes(ref)
	if err != nil {
		return "", fmt.Errorf("image %s: %w", ref.String(), err)
	}
	man, err := image.ValidatePortableArchiveContract(archive, ref, m.imagePluginResolver())
	if err != nil {
		return "", fmt.Errorf("image %s: %w", ref.String(), err)
	}
	name := spec.Name
	if name == "" {
		name = agent.GenerateName(m.rng)
	}
	// Reject a user-supplied --name that could escape AgentsDir before it reaches
	// agentdir.New/Provision (MkdirAll) or Store.Create. Generated names always
	// pass, so this only refuses a malicious/malformed operator name such as
	// "../../evil". This is the M6 plugin path-traversal fix applied to agents.
	if !agent.ValidName(name) {
		return "", fmt.Errorf("invalid agent name %q: must match ^[a-z0-9][a-z0-9_-]*$", name)
	}
	if ok, _ := m.cfg.Store.Exists(name); ok {
		return "", fmt.Errorf("agent %q already exists", name)
	}

	// Schema v2 is exact: plugins are image content, never an implicit core
	// union or a runtime override. Schema v1 keeps its historical precedence.
	requested := spec.Plugins
	if man.SchemaVersion == 2 || len(requested) == 0 {
		requested = nil
		for _, p := range man.Plugins {
			requested = append(requested, p.Name)
		}
	}
	pluginsDir := filepath.Join(filepath.Dir(m.cfg.ImgStore.Dir), "plugins")
	resolver := plugins.ResolveInstalled(pluginsDir)
	var resolvedPlugins []string
	if man.SchemaVersion == 2 {
		resolver = m.imagePluginResolver()
		resolvedPlugins, err = plugincaps.ValidateExplicit(requested, resolver)
	} else {
		resolvedPlugins, err = plugincaps.ResolveWithExternal(requested, resolver)
	}
	if err != nil {
		return "", err
	}

	harnessType, model, effort, env := pick(spec.Harness, "", "claude"), spec.Model, spec.Effort, spec.Env
	if man.SchemaVersion == 1 {
		harnessType = pick(spec.Harness, man.Harness.Type, "claude")
		model = pick(spec.Model, man.Harness.Model, "")
		effort = pick(spec.Effort, man.Harness.Effort, "")
		env = mergeEnv(man.Env, spec.Env)
	}
	if env == nil {
		env = map[string]string{}
	}
	onTimeout := pick(spec.OnTimeout, "restart")
	onError := pick(spec.OnError, "restart")
	messagesBatch := spec.MessagesBatch
	if messagesBatch == 0 {
		messagesBatch = 10
	}
	messagesMaxQueue := spec.MessagesMaxQueue
	if messagesMaxQueue == 0 {
		messagesMaxQueue = 100
	}
	goalEnabled := true
	if spec.GoalEnabled != nil {
		goalEnabled = *spec.GoalEnabled
	}
	goalWaitCustomerTimeoutS := spec.GoalWaitCustomerTimeoutS
	if goalWaitCustomerTimeoutS == 0 {
		goalWaitCustomerTimeoutS = 300
	}
	goalDeliveryCooldownS := spec.GoalDeliveryCooldownS
	if goalDeliveryCooldownS == 0 {
		goalDeliveryCooldownS = 60
	}
	ag := agent.Agent{
		Name: name, ImageRef: ref.String(), ImageDigest: man.Digest,
		Cwd: spec.Cwd, HarnessType: harnessType,
		Model: model, Effort: effort,
		Interactive: spec.Interactive, LoopEnabled: spec.Loop, Enabled: false,
		IntervalS: spec.IntervalS, TimeoutS: spec.TimeoutS, HardTimeoutS: spec.HardTimeoutS,
		OnTimeout: onTimeout, OnError: onError, MaxIdleIterations: spec.MaxIdleIterations,
		AIStallTimeoutS: spec.AIStallTimeoutS,
		UserPrompt:      spec.UserPrompt,
		Env:             env, Plugins: resolvedPlugins,
		MessagesBatch: messagesBatch, MessagesMaxQueue: messagesMaxQueue,
		GoalEnabled: goalEnabled, GoalWaitCustomerTimeoutS: goalWaitCustomerTimeoutS, GoalDeliveryCooldownS: goalDeliveryCooldownS,
		Alias: spec.Alias, Notes: spec.Notes, Color: spec.Color,
	}
	if ref == image.BareRef {
		ag.Interactive = true
		ag.LoopEnabled = false
	}
	// Group membership validates the name and ensures the group entity and shared
	// directory exist before provisioning. Subscriptions are wired by Reconcile
	// after Create.
	if spec.Group != "" {
		if m.cfg.Groups == nil {
			return "", fmt.Errorf("group provisioning is not available")
		}
		if !agent.ValidName(spec.Group) {
			return "", fmt.Errorf("invalid group name %q: must match ^[a-z0-9][a-z0-9_-]*$", spec.Group)
		}
		if err := m.cfg.Groups.EnsureGroup(spec.Group, ""); err != nil {
			return "", err
		}
		ag.Group = spec.Group
	}
	l := agentdir.New(m.cfg.AgentsDir, name)
	if err := agentdir.Provision(l, ag, m.cfg.ImgStore, ref); err != nil {
		return "", err
	}
	if err := m.cfg.Store.Create(ag); err != nil {
		return "", err
	}
	if err := m.ensureOwnInbox(ag.Name); err != nil {
		return "", err
	}
	if spec.Group != "" {
		if err := m.cfg.Groups.Reconcile(spec.Group); err != nil {
			return "", err
		}
	}
	if ag.Enabled {
		if err := m.start(ag); err != nil {
			return "", err
		}
	}
	return name, nil
}

func (m *Manager) ensureOwnInbox(agentName string) error {
	if m.cfg.Bus == nil {
		return fmt.Errorf("message bus is not available")
	}
	_, err := m.cfg.Bus.Subscribe(agentName, bus.InboxChannel(agentName), bus.Matcher{}, nil)
	if err != nil {
		return fmt.Errorf("subscribe agent %q to own inbox: %w", agentName, err)
	}
	// A new agent gets its chats here, not only at the next daemon start, so the
	// customer can open a conversation with it immediately.
	if err := m.cfg.Bus.EnsureAgentChats(agentName); err != nil {
		return fmt.Errorf("provision chats for agent %q: %w", agentName, err)
	}
	return nil
}

// newToolsAPIServer builds the per-agent tools server. It deliberately depends
// on the manager and the agent name only, never on a loop engine: the same
// server is bound for an agent whose engine has not started (and may never
// start) while its iteration is still running, and the current iteration is
// resolved per request via currentIterationID.
func (m *Manager) newToolsAPIServer(ag agent.Agent, l agentdir.Layout) *agentapi.Server {
	agName := ag.Name
	return agentapi.NewServer(agentapi.Deps{
		Agent: ag.Name, Cwd: cwdOf(ag, l), ContextPath: l.ContextPath(), Plugins: ag.Plugins,
		CurrentPlugins: func() []string {
			current, err := m.cfg.Store.Get(agName)
			if err != nil {
				return nil
			}
			return current.Plugins
		},
		CurrentIteration: func() string { return m.currentIterationID(agName) },
		// SetDone reconfirms the caller's iteration is still the current one
		// before flipping done_flag. The harness reads the id via CurrentIteration
		// (or TARIBOY_ITERATION) and POSTs loop/done during that same iteration;
		// if the iteration has already rolled over, the stale id is rejected rather
		// than marking a finished iteration done. The residual window between the
		// reconfirm and the DB write is negligible: the engine only advances
		// current after the runner returns, long after the harness process exits.
		SetDone: func(id string, productive bool) error {
			if m.currentIterationID(agName) != id {
				return fmt.Errorf("iteration %q is no longer running", id)
			}
			return m.cfg.Store.SetIterationDone(id, productive)
		},
		Status: func() (map[string]any, error) {
			state, err := m.LiveState(ag.Name)
			if err != nil {
				return nil, err
			}
			a, err := m.cfg.Store.Get(ag.Name)
			if err != nil {
				return nil, err
			}
			return map[string]any{"state": state, "loop_enabled": a.LoopEnabled,
				"current_iteration": m.currentIterationID(agName),
				"message":           a.StatusMessage, "updated": a.StatusUpdated}, nil
		},
		SetStatus: func(message string) (map[string]any, error) {
			updated := m.cfg.Clock().Format(time.RFC3339)
			if err := m.cfg.Store.SetStatus(ag.Name, message, updated); err != nil {
				return nil, err
			}
			// Mirror the write into the audit log so `iteration logs`/audit shows
			// the full status timeline (v1 recorded type="status" per set).
			if m.cfg.AuditFor != nil {
				if rec := m.cfg.AuditFor(agName); rec != nil {
					rec.Record("status", "agent", m.currentIterationID(agName), map[string]any{"message": message})
				}
			}
			return map[string]any{"message": message, "updated": updated}, nil
		},
		SetGoal: func(key string) (map[string]any, error) {
			iteration := m.currentIterationID(agName)
			if iteration == "" {
				return nil, fmt.Errorf("no iteration is currently running")
			}
			proxy, ok := m.cfg.Proxy.(taskAttributionProxy)
			if !ok {
				return nil, fmt.Errorf("goal selection is unavailable")
			}
			return setGoal(context.Background(), m.cfg.Tasks, m.cfg.SetGoal, proxy, iteration, agName, key)
		},
		Publish: func(msg bus.Message) (bus.Message, error) {
			if m.cfg.Bus == nil {
				return bus.Message{}, fmt.Errorf("bus not configured")
			}
			return m.cfg.Bus.Publish(msg)
		},
		Subscribe: func(channel string, matcher bus.Matcher, tf []string) (bus.Subscription, error) {
			if m.cfg.Bus == nil {
				return bus.Subscription{}, fmt.Errorf("bus not configured")
			}
			return m.cfg.Bus.Subscribe(agName, channel, matcher, tf)
		},
		Unsubscribe: func(id string) error {
			if m.cfg.Bus == nil {
				return fmt.Errorf("bus not configured")
			}
			return m.cfg.Bus.Unsubscribe(agName, id)
		},
		ListSubscriptions: func() ([]bus.Subscription, error) {
			if m.cfg.Bus == nil {
				return nil, nil
			}
			return m.cfg.Bus.ListSubscriptions(agName)
		},
		Channels: func() ([]bus.Channel, error) {
			if m.cfg.Bus == nil {
				return nil, nil
			}
			return m.cfg.Bus.Channels()
		},
		ProvidedChannels: m.cfg.ProvidedChannels,
		TaskAction: func(action string, body map[string]any) (any, error) {
			if m.cfg.Tasks == nil {
				return nil, fmt.Errorf("native Tasks service is unavailable")
			}
			body["iteration_id"] = m.currentIterationID(agName)
			return m.cfg.Tasks.AgentAction(context.Background(), tasks.AgentActor(agName), action, body)
		},
		WorkflowPermissions: func() (tasks.ActiveWorkflowPermissionSet, error) {
			if m.cfg.Tasks == nil {
				return tasks.ActiveWorkflowPermissionSet{}, nil
			}
			return m.cfg.Tasks.ActiveWorkflowPermissions(context.Background(), agName, m.currentIterationID(agName))
		},
		Inbox: func(status string, limit int, before string) ([]bus.InboxItem, error) {
			if m.cfg.Bus == nil {
				return nil, nil
			}
			return m.cfg.Bus.Inbox(agName, status, limit, before)
		},
		MarkProcessed: func(msgID, result string) (bus.InboxItem, error) {
			if m.cfg.Bus == nil {
				return bus.InboxItem{}, fmt.Errorf("bus not configured")
			}
			return m.cfg.Bus.MarkProcessed(agName, msgID, result)
		},
		Reply: func(msgID, text string, data map[string]any, typeOverride string) (bus.Message, error) {
			if m.cfg.Bus == nil {
				return bus.Message{}, fmt.Errorf("bus not configured")
			}
			return m.cfg.Bus.Reply(agName, msgID, text, data, typeOverride)
		},
		Request: func(channel, text, deadline string) (bus.Message, error) {
			if m.cfg.Bus == nil {
				return bus.Message{}, fmt.Errorf("bus not configured")
			}
			return m.cfg.Bus.Request(agName, channel, text, deadline)
		},
		Requeue: func(msgID string) error {
			if m.cfg.Bus == nil {
				return fmt.Errorf("bus not configured")
			}
			return m.cfg.Bus.Requeue(agName, msgID)
		},
		SubscribeParams: func(channel string, matcher bus.Matcher, tf []string, params map[string]any) (bus.Subscription, error) {
			if m.cfg.Bus == nil {
				return bus.Subscription{}, fmt.Errorf("bus not configured")
			}
			return m.cfg.Bus.SubscribeParams(agName, channel, matcher, tf, params)
		},
		UnsubscribeChannel: func(channel string) (int, error) {
			if m.cfg.Bus == nil {
				return 0, fmt.Errorf("bus not configured")
			}
			return m.cfg.Bus.UnsubscribeChannel(agName, channel)
		},
		Unanswered: func() (map[string][]bus.Message, error) {
			if m.cfg.Bus == nil {
				return nil, fmt.Errorf("bus not configured")
			}
			return m.cfg.Bus.UnansweredForPrincipal("agent:" + agName)
		},
		AddSchedule: func(kind, spec, channel, tpl string) (map[string]any, error) {
			if m.cfg.Schedules == nil {
				return nil, fmt.Errorf("schedule store not configured")
			}
			if channel == "" {
				// A schedule the agent set for itself is service traffic; an
				// explicit channel still wins, so an existing schedule and a
				// deliberate chat target both keep working.
				channel = bus.ChatChannelFor(bus.ChatIDService(agName))
			}
			sch, err := m.cfg.Schedules.Add(schedule.Schedule{
				Agent: agName, Kind: kind, Spec: spec, Channel: channel, MessageTemplate: tpl})
			if err != nil {
				return nil, err
			}
			return map[string]any{"id": sch.ID, "kind": sch.Kind, "channel": sch.Channel,
				"next_fire_at": sch.NextFireAt}, nil
		},
		ListSchedules: func() ([]map[string]any, error) {
			if m.cfg.Schedules == nil {
				return nil, nil
			}
			list, err := m.cfg.Schedules.List(agName)
			if err != nil {
				return nil, err
			}
			rows := make([]map[string]any, 0, len(list))
			for _, s := range list {
				rows = append(rows, map[string]any{"id": s.ID, "kind": s.Kind, "spec": s.Spec,
					"channel": s.Channel, "next_fire_at": s.NextFireAt, "enabled": s.Enabled})
			}
			return rows, nil
		},
		CancelSchedule: func(id string) error {
			if m.cfg.Schedules == nil {
				return fmt.Errorf("schedule store not configured")
			}
			return m.cfg.Schedules.Cancel(agName, id)
		},
		RunScriptOnce: func(in script.CreateOnce) (script.Definition, script.Run, error) { return m.RunOnce(agName, in) },
		ScheduleScript: func(in script.CreateSchedule) (script.Definition, script.Run, error) {
			return m.ScheduleScript(agName, in)
		},
		RerunScript:        func(id string) (script.Run, error) { return m.RerunScript(agName, id) },
		ListScripts:        func() ([]script.Definition, error) { return m.ListScripts(agName) },
		ListScriptRuns:     func(id string) ([]script.Run, error) { return m.ListScriptRuns(agName, id) },
		GetScriptRun:       func(id string) (script.Run, error) { return m.GetScriptRun(agName, id) },
		LogScriptRun:       func(id string) (string, error) { return m.LogScriptRun(agName, id) },
		CancelScriptTarget: func(id string) error { return m.CancelScriptTarget(agName, id) },
		RemoveScript:       func(id string) error { return m.RemoveScript(agName, id) },
		BuildImage: func(name, tag, path string) (map[string]any, error) {
			return buildImageForAgent(m.cfg.ImgStore, l.Workdir(), name, tag, path, m.imagePluginResolver())
		},
		LoopControl: func(action string) (map[string]any, error) {
			updated, err := m.toolsLoopControl(agName, action)
			if err != nil {
				return nil, err
			}
			return map[string]any{"agent": agName, "action": action,
				"enabled": updated.Enabled, "loop_enabled": updated.LoopEnabled}, nil
		},
		GroupInfo: func() (map[string]any, error) {
			return m.groupToolsInfo(agName)
		},
		GroupStatus: func(member string) (map[string]any, error) {
			return m.groupToolsStatus(agName, member)
		},
		GroupSend: func(member, typ, text, deadline string) (map[string]any, error) {
			return m.groupToolsSend(agName, m.currentIterationID(agName), member, typ, text, deadline)
		},
		GroupObserve: func(member string, tail int) (map[string]any, error) {
			return m.groupToolsObserve(agName, member, tail)
		},
		GroupLoop: func(member, action string) (map[string]any, error) {
			return m.groupToolsLoop(agName, member, action)
		},
	})
}

func (m *Manager) start(ag agent.Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if live, ok := m.adopting[ag.Name]; ok {
		return fmt.Errorf("%w %q for agent %q", errIterationAdopting, live.ID, ag.Name)
	}
	if _, ok := m.runs[ag.Name]; ok {
		// The runtime is still live (e.g. Start after Stop, where the engine
		// goroutine survived). Clear any stale halt reason, grant a fresh idle
		// budget (reset the idle-stop boundary + clear stale idle status), and
		// wake the parked engine so a re-enable is picked up now, not on some
		// future timer.
		if err := m.cfg.Store.ClearError(ag.Name); err != nil {
			return err
		}
		if err := m.cfg.Store.StartResetIdle(ag.Name); err != nil {
			return err
		}
		m.runs[ag.Name].engine.Wake(WakeStart)
		return nil
	}
	if err := m.cfg.Store.ClearError(ag.Name); err != nil {
		return err
	}
	if err := m.cfg.Store.StartResetIdle(ag.Name); err != nil {
		return err
	}
	agName := ag.Name
	l := agentdir.New(m.cfg.AgentsDir, ag.Name).WithRuntime(m.cfg.RuntimeDir)
	engine := NewEngine(ag, m.cfg.Store, m.runnerFor(ag), m.cfg.Log, m.cfg.Clock)
	if m.cfg.ImgStore != nil {
		engine.SetBeforeLaunch(m.activatePendingImage)
	}
	engine.metrics = m.cfg.Metrics
	engine.usageLookup = m.cfg.UsageLookup
	engine.usageFlush = m.cfg.UsageFlush
	if m.cfg.Bus != nil {
		engine.SetMessagePeek(func() (bool, error) { return m.cfg.Bus.HasPending(agName) })
	}
	if m.cfg.Emit != nil {
		engine.SetEmit(m.cfg.Emit)
	}
	if m.cfg.AuditFor != nil {
		engine.SetAudit(func(typ, source, iterationID string, data map[string]any) {
			if rec := m.cfg.AuditFor(agName); rec != nil {
				rec.Record(typ, source, iterationID, data)
			}
		})
	}
	engine.SetOnIterationClose(func(agentName, iterationID string) {
		// A stale kill is idempotent only while the recovered iteration is
		// unwinding. Once that iteration has closed, retaining its marker would
		// incorrectly make future Kill calls against an idle or stopped agent
		// succeed forever.
		m.mu.Lock()
		if m.staleKills[agentName] == iterationID {
			delete(m.staleKills, agentName)
		}
		m.mu.Unlock()
		if m.cfg.OnIterationClose != nil {
			m.cfg.OnIterationClose(agentName, iterationID)
		}
	})
	engine.SetIterationCompleted(m.iterationCompleted)

	// Bind the per-agent tools socket SYNCHRONOUSLY before launching the loop.
	// The harness reaches the daemon (loop done, context, messages, ...) only
	// through this socket; if the bind fails and we started the loop anyway, the
	// harness would run with no control plane and every iteration would silently
	// end in no_i_am_done. So a bind failure fails the start loudly and rolls the
	// agent back out of "running" instead of degrading in the dark. An early bind
	// from boot reconciliation is taken over here rather than replaced.
	apiSrv, err := m.bindToolsSocketLocked(ag, l)
	if err != nil {
		m.cfg.Log.Error("bind agent tools socket", "agent", ag.Name, "sock", l.Sock(), "err", err)
		if serr := m.cfg.Store.SetError(ag.Name, "bind tools socket failed"); serr != nil {
			m.cfg.Log.Error("rollback agent state", "agent", ag.Name, "err", serr)
		}
		return fmt.Errorf("bind agent tools socket %s: %w", l.Sock(), err)
	}

	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	ectx, cancel := context.WithCancel(base)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		engine.Run(ectx)
	}()

	m.runs[ag.Name] = &runtime{engine: engine, cancel: cancel, apiServer: apiSrv}
	// The runtime now owns the socket; drop the boot-time registration so
	// teardown closes exactly one server for this agent.
	delete(m.toolsAPI, ag.Name)
	if m.cfg.Bus != nil {
		// Nudge the freshly launched engine so a message-capable agent (peek
		// installed above) that already has unacked pending deliveries drains
		// them now instead of waiting on some unrelated future publish. This is
		// the common entry point for a fresh Run, Start, and every reattach path
		// (StartAll's direct start and startAfter's post-adoption start), so one
		// call here covers all of them. The engine's WakeMessage handler is a
		// no-op unless LoopEnabled && hasPending(), so this is harmless for
		// interval-only or currently-empty agents.
		engine.Wake(WakeMessage)
	}
	return nil
}

// Start is the master enable: it turns the whole agent on. It records
// Enabled=true, registers/wakes the loop engine, and — for an interactive agent
// — launches an iteration so the tmux/PTY session comes up (an interactive agent
// has no autonomous tick to spin one, so it would otherwise park at idle with no
// session to attach to).
func (m *Manager) Start(name string) error {
	ag, err := m.cfg.Store.Get(name)
	if err != nil {
		return err
	}
	ag.Enabled = true
	if err := m.cfg.Store.Update(ag); err != nil {
		return err
	}
	m.signalGoals()
	if err := m.start(ag); err != nil {
		if errors.Is(err, errIterationAdopting) {
			return nil
		}
		return err
	}
	if ag.Interactive {
		if _, err := m.Exec(name, ""); err != nil {
			return err
		}
	}
	return nil
}

// Stop is the master disable: it turns the whole agent off. It records
// Enabled=false FIRST (so a killed iteration is not relaunched — the engine's
// reload sees the master switch off and parks), wakes the engine to repark, then
// kills any in-flight iteration / interactive session. Kill is best-effort: an
// enabled-but-idle agent has nothing to kill.
func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	ag, err := m.cfg.Store.Get(name)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	ag.Enabled = false
	if err := m.cfg.Store.Update(ag); err != nil {
		m.mu.Unlock()
		return err
	}
	rt := m.runs[name]
	delete(m.restarting, name)
	if rt != nil {
		rt.engine.Wake(WakeStop)
	}
	m.mu.Unlock()
	m.signalGoals()
	_ = m.Kill(name)
	return nil
}

// SetLoopEnabled persists the independent Autopilot flag and immediately wakes
// any live runtime. Start/Stop remain responsible for the master Enabled flag.
func (m *Manager) SetLoopEnabled(name string, enabled bool) error {
	ag, err := m.cfg.Store.Get(name)
	if err != nil {
		return err
	}
	ag.LoopEnabled = enabled
	if err := m.cfg.Store.Update(ag); err != nil {
		return err
	}
	m.RefreshLoopConfig(name)
	return nil
}

// RefreshLoopConfig asks a live engine to discard its timer and reload the
// persisted settings. A stopped or not-yet-started agent has nothing to wake.
func (m *Manager) RefreshLoopConfig(name string) {
	m.signalGoals()
	m.mu.Lock()
	rt := m.runs[name]
	m.mu.Unlock()
	if rt != nil {
		rt.engine.Wake(WakeConfig)
	}
}

func (m *Manager) Restart(name string) error {
	if err := m.Stop(name); err != nil {
		return err
	}
	ag, err := m.cfg.Store.Get(name)
	if err != nil {
		return err
	}
	ag.Enabled = true
	if err := m.cfg.Store.Update(ag); err != nil {
		return err
	}
	m.signalGoals()
	m.mu.Lock()
	if _, ok := m.adopting[name]; ok {
		if ag.Interactive {
			m.restarting[name] = true
		}
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if err := m.start(ag); err != nil {
		return err
	}
	if ag.Interactive {
		_, err = m.Exec(name, "")
	}
	return err
}

// WakeAgents wakes the named running engines. Used by the bus publish hook, the
// scheduler and the scripts result path to nudge affected agents.
func (m *Manager) WakeAgents(agents []string, kind WakeKind) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range agents {
		if rt := m.runs[name]; rt != nil {
			rt.engine.Wake(kind)
		}
	}
}

func (m *Manager) Kill(name string) error {
	client, target, err := m.currentShimClient(name)
	if err != nil {
		if target.runtime != nil {
			l := agentdir.New(m.cfg.AgentsDir, name).WithRuntime(m.cfg.RuntimeDir)
			return m.recoverStaleKill(name, target.id, target.runtime, l, err)
		}
		m.mu.Lock()
		staleID := m.staleKills[name]
		m.mu.Unlock()
		if staleID != "" {
			return nil
		}
		return err
	}
	l := agentdir.New(m.cfg.AgentsDir, name).WithRuntime(m.cfg.RuntimeDir)
	if err := client.Kill(); err != nil {
		if target.runtime == nil {
			return err
		}
		var rpcErr *shim.RPCError
		if errors.As(err, &rpcErr) {
			return err
		}
		return m.recoverStaleKill(name, target.id, target.runtime, l, err)
	}
	return nil
}

// recoverStaleKill finalizes an iteration whose shim cannot be contacted. It
// cancels only that iteration, so its normal loop remains able to launch again.
func (m *Manager) recoverStaleKill(name, id string, rt *runtime, l agentdir.Layout, rpcErr error) error {
	// Same finalizer as the adoption paths, so a failing store leaves the same
	// state everywhere: status still "running" and the shim.sock marker on disk
	// for a later adoption to retry. Before, this path alone kept the socket by
	// never reaching the removal, while the adoption paths dropped it.
	committed, terminalBeforeCleanup, err := m.finalizeIteration(l, name, id, m.terminalHarnessError)
	switch {
	case errors.Is(err, errIterationLookup):
		return fmt.Errorf("kill shim for %q: %w", name, rpcErr)
	case err != nil:
		return fmt.Errorf("recover stale shim for %q: %w", name, err)
	case terminalBeforeCleanup:
		// Already terminal: the socket must not outlive a committed status.
		_ = os.Remove(l.ShimSock())
	}
	if committed {
		m.iterationCompleted(name, id)
	}
	m.mu.Lock()
	if m.runs[name] == rt && rt.engine.CurrentIterationID() == id {
		m.staleKills[name] = id
		rt.engine.AbortCurrent(id)
	}
	m.mu.Unlock()
	m.cfg.Log.Warn("kill recovered unreachable shim", "agent", name, "id", id, "err", rpcErr)
	return nil
}

func (m *Manager) Exec(name, prompt string) (string, error) {
	m.mu.Lock()
	rt := m.runs[name]
	m.mu.Unlock()
	if rt == nil {
		ag, err := m.cfg.Store.Get(name)
		if err != nil {
			return "", err
		}
		if err := m.start(ag); err != nil {
			return "", err
		}
		m.mu.Lock()
		rt = m.runs[name]
		m.mu.Unlock()
		if rt == nil {
			return "", fmt.Errorf("agent %q is not running after start", name)
		}
	}
	rt.engine.Trigger(prompt)
	return "queued", nil
}

// RerunScript queues an immediate attempt for an eligible stored definition.
func (m *Manager) RerunScript(agentName, scriptID string) (script.Run, error) {
	if m.cfg.Scripts == nil {
		return script.Run{}, fmt.Errorf("script store not configured")
	}
	run, err := m.cfg.Scripts.Rerun(agentName, scriptID)
	if err == nil {
		m.wakeScripts()
	}
	return run, err
}

// RunOnce creates one local command definition and one immediate asynchronous run.
func (m *Manager) RunOnce(agentName string, in script.CreateOnce) (script.Definition, script.Run, error) {
	if m.cfg.Scripts == nil {
		return script.Definition{}, script.Run{}, fmt.Errorf("script store not configured")
	}
	if _, err := m.cfg.Store.Get(agentName); err != nil {
		return script.Definition{}, script.Run{}, err
	}
	definition, run, err := m.cfg.Scripts.CreateOnce(agentName, in)
	if err == nil {
		m.wakeScripts()
	}
	return definition, run, err
}

// ScheduleScript creates a fixed-delay recurring local command and its first
// immediate run. Later runs are queued only after the previous run finishes.
func (m *Manager) ScheduleScript(agentName string, in script.CreateSchedule) (script.Definition, script.Run, error) {
	if m.cfg.Scripts == nil {
		return script.Definition{}, script.Run{}, fmt.Errorf("script store not configured")
	}
	if _, err := m.cfg.Store.Get(agentName); err != nil {
		return script.Definition{}, script.Run{}, err
	}
	definition, run, err := m.cfg.Scripts.CreateSchedule(agentName, in)
	if err == nil {
		m.wakeScripts()
	}
	return definition, run, err
}

func (m *Manager) ListScripts(agentName string) ([]script.Definition, error) {
	if m.cfg.Scripts == nil {
		return nil, fmt.Errorf("script store not configured")
	}
	return m.cfg.Scripts.ListDefinitions(agentName)
}

func (m *Manager) ListScriptRuns(agentName, scriptID string) ([]script.Run, error) {
	if m.cfg.Scripts == nil {
		return nil, fmt.Errorf("script store not configured")
	}
	return m.cfg.Scripts.ListRuns(agentName, scriptID)
}

func (m *Manager) GetScriptRun(agentName, runID string) (script.Run, error) {
	if m.cfg.Scripts == nil {
		return script.Run{}, fmt.Errorf("script store not configured")
	}
	return m.cfg.Scripts.GetRun(agentName, runID)
}

// CancelScriptTarget records cancellation intent for a running attempt before
// terminating its process group. The run remains active until Wait confirms
// process exit, preserving the per-definition no-overlap invariant.
func (m *Manager) CancelScriptTarget(agentName, id string) error {
	if m.cfg.Scripts == nil {
		return fmt.Errorf("script store not configured")
	}
	at := m.cfg.Clock().UTC().Format(time.RFC3339)
	if run, err := m.cfg.Scripts.GetRun(agentName, id); err == nil {
		if run.Status == script.RunRunning {
			run, err = m.cfg.Scripts.RequestRunCancellation(agentName, id)
			if err != nil {
				return err
			}
			key := run.Agent + "/" + run.ID
			tracked := m.markScriptCancellation(key)
			if run.PID != nil {
				if tracked {
					m.terminateScriptProcess(key, *run.PID)
				} else {
					_ = syscall.Kill(-*run.PID, syscall.SIGTERM)
				}
			}
			return nil
		}
		if err := m.cfg.Scripts.CancelRun(agentName, id, at); err != nil {
			return err
		}
		if m.cfg.ScriptResults != nil {
			m.cfg.ScriptResults.Wake()
		}
		return nil
	} else if !errors.Is(err, script.ErrNotFound) {
		return err
	}

	definition, err := m.cfg.Scripts.GetDefinition(agentName, id)
	if err != nil {
		return err
	}
	var running *script.Run
	if definition.LatestRun != nil && definition.LatestRun.Status == script.RunRunning {
		running = definition.LatestRun
	}
	if err := m.cfg.Scripts.CancelDefinition(agentName, id, at); err != nil {
		return err
	}
	if running != nil {
		key := running.Agent + "/" + running.ID
		tracked := m.markScriptCancellation(key)
		if running.PID != nil {
			if tracked {
				m.terminateScriptProcess(key, *running.PID)
			} else {
				_ = syscall.Kill(-*running.PID, syscall.SIGTERM)
			}
		}
	}
	if m.cfg.ScriptResults != nil {
		m.cfg.ScriptResults.Wake()
	}
	return nil
}

func (m *Manager) markScriptCancellation(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, tracked := m.scriptRuns[key]; !tracked {
		return false
	}
	m.scriptCancels[key] = true
	return true
}

func (m *Manager) terminateScriptProcess(key string, pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.AfterFunc(scriptCancelGrace, func() {
		m.mu.Lock()
		cancelPending := m.scriptCancels[key]
		_, stillRunning := m.scriptRuns[key]
		m.mu.Unlock()
		if cancelPending && stillRunning {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
}

func (m *Manager) RemoveScript(agentName, id string) error {
	if m.cfg.Scripts == nil {
		return fmt.Errorf("script store not configured")
	}
	definition, err := m.cfg.Scripts.GetDefinition(agentName, id)
	if err != nil {
		return err
	}
	if definition.State == script.StateActive {
		return fmt.Errorf("cannot remove active script %q; cancel it first: %w", id, script.ErrActive)
	}
	runs, err := m.cfg.Scripts.ListRuns(agentName, id)
	if err != nil {
		return err
	}
	logPaths := make([]string, 0, len(runs))
	for _, run := range runs {
		if run.Status == script.RunPending || run.Status == script.RunRunning {
			return fmt.Errorf("cannot remove script %q with an active run: %w", id, script.ErrActive)
		}
		if run.LogPath == "" {
			continue
		}
		_, path, err := m.scriptLogLocation(agentName, run.LogPath)
		if err != nil {
			return err
		}
		logPaths = append(logPaths, path)
	}
	type movedLog struct{ from, to string }
	moved := make([]movedLog, 0, len(logPaths))
	rollback := func() {
		for i := len(moved) - 1; i >= 0; i-- {
			if err := os.Rename(moved[i].to, moved[i].from); err != nil && !os.IsNotExist(err) {
				m.cfg.Log.Error("restore script log after failed removal", "from", moved[i].to, "to", moved[i].from, "err", err)
			}
		}
	}
	for index, path := range logPaths {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			rollback()
			return err
		}
		quarantine := fmt.Sprintf("%s.deleting-%d-%d", path, m.cfg.Clock().UnixNano(), index)
		if _, err := os.Lstat(quarantine); err == nil {
			rollback()
			return fmt.Errorf("script log quarantine path already exists: %s", quarantine)
		} else if !os.IsNotExist(err) {
			rollback()
			return err
		}
		if err := os.Rename(path, quarantine); err != nil {
			rollback()
			return err
		}
		moved = append(moved, movedLog{from: path, to: quarantine})
	}
	if err := m.cfg.Scripts.RemoveDefinition(agentName, id); err != nil {
		rollback()
		return err
	}
	for _, log := range moved {
		if err := os.Remove(log.to); err != nil && !os.IsNotExist(err) {
			m.cfg.Log.Error("remove quarantined script log", "path", log.to, "err", err)
		}
	}
	return nil
}

// LogScript returns a bounded tail after confirming the record belongs to the
// requested agent. Missing logs are represented by an empty string.
func (m *Manager) LogScriptRun(agentName, id string) (string, error) {
	file, _, err := m.openScriptLog(agentName, id)
	if errors.Is(err, script.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	const maxLogBytes = 64 * 1024
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > maxLogBytes {
		if _, err := file.Seek(info.Size()-maxLogBytes, io.SeekStart); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(io.LimitReader(file, maxLogBytes))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (m *Manager) OpenScriptLog(agentName, runID string) (io.ReadCloser, string, error) {
	return m.openScriptLog(agentName, runID)
}

func (m *Manager) openScriptLog(agentName, runID string) (*os.File, string, error) {
	run, err := m.GetScriptRun(agentName, runID)
	if err != nil {
		return nil, "", err
	}
	if run.LogPath == "" {
		return nil, "", script.ErrNotFound
	}
	root, path, err := m.scriptLogLocation(agentName, run.LogPath)
	if err != nil {
		return nil, "", err
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", script.ErrNotFound
		}
		return nil, "", err
	}
	defer unix.Close(rootFD)
	filename := filepath.Base(path)
	fd, err := unix.Openat(rootFD, filename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", script.ErrNotFound
		}
		return nil, "", err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, "", errors.New("open script log")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, "", err
	}
	stat, _ := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || stat == nil || stat.Nlink != 1 {
		file.Close()
		return nil, "", errors.New("script log is not a regular owner-only file")
	}
	return file, filename, nil
}

func (m *Manager) scriptLogLocation(agentName, logPath string) (string, string, error) {
	root, err := filepath.Abs(filepath.Join(agentdir.New(m.cfg.AgentsDir, agentName).Root, "scripts"))
	if err != nil {
		return "", "", err
	}
	path, err := filepath.Abs(logPath)
	if err != nil {
		return "", "", err
	}
	if filepath.Dir(path) != root {
		return "", "", fmt.Errorf("script log path is outside the agent scripts directory")
	}
	return root, path, nil
}

func (m *Manager) wakeScripts() {
	select {
	case m.scriptsWake <- struct{}{}:
	default:
	}
}

func (m *Manager) startScriptSupervisor(ctx context.Context) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(scriptPollInterval)
		defer ticker.Stop()
		for {
			m.superviseScripts(ctx)
			select {
			case <-ctx.Done():
				return
			case <-m.scriptsWake:
			case <-ticker.C:
			}
		}
	}()
}

func (m *Manager) superviseScripts(ctx context.Context) {
	agents, err := m.cfg.Store.List()
	if err != nil {
		m.cfg.Log.Error("list scripts agents", "err", err)
		return
	}
	now := m.cfg.Clock().UTC().Format(time.RFC3339)
	due, err := m.cfg.Scripts.DueDefinitions(now)
	if err != nil {
		m.cfg.Log.Error("list due scripts", "err", err)
		return
	}
	for _, definition := range due {
		if _, err := m.cfg.Scripts.ScheduleNext(definition.Agent, definition.ID); err != nil && !errors.Is(err, script.ErrConflict) {
			m.cfg.Log.Error("schedule next script run", "agent", definition.Agent, "script", definition.ID, "err", err)
		}
	}
	pending, err := m.cfg.Scripts.PendingRuns()
	if err != nil {
		m.cfg.Log.Error("list pending script runs", "err", err)
		return
	}
	agentsByName := make(map[string]agent.Agent, len(agents))
	for _, ag := range agents {
		agentsByName[ag.Name] = ag
	}
	for _, run := range pending {
		ag, ok := agentsByName[run.Agent]
		if !ok {
			continue
		}
		m.startScript(ctx, ag, run)
	}
}

func (m *Manager) startScript(ctx context.Context, ag agent.Agent, r script.Run) {
	key := r.Agent + "/" + r.ID
	m.mu.Lock()
	if _, running := m.scriptRuns[key]; running {
		m.mu.Unlock()
		return
	}
	// A placeholder makes concurrent scan passes unable to start this record
	// twice before its shell has a PID.
	m.scriptRuns[key] = nil
	m.mu.Unlock()
	l := agentdir.New(m.cfg.AgentsDir, ag.Name).WithRuntime(m.cfg.RuntimeDir)
	definition, err := m.cfg.Scripts.GetDefinition(r.Agent, r.ScriptID)
	if err != nil {
		m.finishScript(key, r, -1, fmt.Errorf("load script definition: %w", err), "")
		return
	}
	logPath, err := filepath.Abs(filepath.Join(l.Root, "scripts", r.ID+".log"))
	if err != nil {
		m.finishScript(key, r, -1, fmt.Errorf("resolve script log path: %w", err), "")
		return
	}
	claimed, err := m.cfg.Scripts.ClaimRun(r.Agent, r.ID, m.cfg.Clock().UTC().Format(time.RFC3339), logPath)
	if err != nil {
		m.finishScript(key, r, -1, err, logPath)
		return
	}
	if !claimed {
		m.mu.Lock()
		delete(m.scriptRuns, key)
		m.mu.Unlock()
		return
	}

	secrets, _ := m.cfg.Store.SecretMap(ag.Name)
	env := BuildEnv(os.Environ(), l.BinDir(), ag.Name, "", l.Sock(), false, "", "", ag.Env, secrets)
	runContext := ctx
	cancelRunContext := func() {}
	if m.cfg.ScriptTimeout > 0 {
		runContext, cancelRunContext = context.WithTimeout(ctx, m.cfg.ScriptTimeout)
	}
	cmd := exec.CommandContext(runContext, "sh", "-c", definition.Command)
	cmd.Dir, cmd.Env = cwdOf(ag, l), env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		cancelRunContext()
		m.finishScript(key, r, -1, err, logPath)
		return
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cancelRunContext()
		m.finishScript(key, r, -1, err, logPath)
		return
	}
	if _, err := fmt.Fprintf(logFile, "cwd: %s\n", cmd.Dir); err != nil {
		cancelRunContext()
		_ = logFile.Close()
		m.finishScript(key, r, -1, err, logPath)
		return
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		cancelRunContext()
		_ = logFile.Close()
		m.finishScript(key, r, -1, err, logPath)
		return
	}
	pidSet, err := m.cfg.Scripts.SetRunPID(r.Agent, r.ID, cmd.Process.Pid)
	if err != nil || !pidSet {
		cancelRunContext()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = logFile.Close()
		if err == nil {
			err = errors.New("script claim was canceled before process startup completed")
		}
		m.finishScript(key, r, -1, err, logPath)
		return
	}
	m.mu.Lock()
	m.scriptRuns[key] = cmd
	cancelRequested := m.scriptCancels[key]
	m.mu.Unlock()
	if cancelRequested {
		m.terminateScriptProcess(key, cmd.Process.Pid)
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancelRunContext()
		err := cmd.Wait()
		_ = logFile.Close()
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = -1
			}
		}
		if runContext.Err() != nil {
			code, err = -1, runContext.Err()
		}
		m.finishScript(key, r, code, err, logPath)
	}()
}

func (m *Manager) finishScript(key string, r script.Run, code int, runErr error, logPath string) {
	m.mu.Lock()
	cancelRequested := m.scriptCancels[key]
	delete(m.scriptRuns, key)
	delete(m.scriptCancels, key)
	m.mu.Unlock()
	current, err := m.cfg.Scripts.GetRun(r.Agent, r.ID)
	if err != nil {
		return
	}
	if cancelRequested || current.CancelRequested {
		if err := m.cfg.Scripts.CancelRun(r.Agent, r.ID, m.cfg.Clock().UTC().Format(time.RFC3339)); err != nil && !errors.Is(err, script.ErrConflict) {
			m.cfg.Log.Error("cancel script run", "agent", r.Agent, "run", r.ID, "err", err)
			return
		}
		if m.cfg.ScriptResults != nil {
			m.cfg.ScriptResults.Wake()
		}
		return
	}
	// Cancellation wins over a late Wait result, so a killed recurring script
	// cannot resurrect itself as waiting.
	if current.Status == script.RunCancelled {
		return
	}
	if runErr != nil && code == -1 {
		m.cfg.Log.Error("script start/wait", "agent", r.Agent, "script", r.ID, "err", runErr)
	}
	status := script.RunFailed
	var exitCode *int
	if errors.Is(runErr, context.DeadlineExceeded) {
		status = script.RunTimedOut
	} else if errors.Is(runErr, context.Canceled) {
		status = script.RunInterrupted
	} else if code >= 0 {
		exitCode = &code
		if code == 0 {
			status = script.RunSucceeded
		}
	}
	if _, err := m.cfg.Scripts.CompleteRun(r.Agent, r.ID, script.Completion{Status: status, ExitCode: exitCode, FinishedAt: m.cfg.Clock().UTC().Format(time.RFC3339), LogPath: logPath}); err != nil {
		if !errors.Is(err, script.ErrConflict) {
			m.cfg.Log.Error("complete script run", "agent", r.Agent, "run", r.ID, "err", err)
		}
		return
	}
	if m.cfg.ScriptResults != nil {
		m.cfg.ScriptResults.Wake()
	}
}

// Remove tears an agent down. force only bypasses the running-guard; it is
// orthogonal to purge. With purge=false (the data-preserving default for a plain
// compose down) the agents DB row is kept and left stopped, together with every
// durable artifact — CONTEXT.md, iterations (dir + rows), and audit.jsonl
// — while only the rebuildable image/bin/workdir tree is dropped, so a later
// Reprovision re-unpacks a (possibly new) image in place over the retained
// history. With purge=true it is a full hard delete that also cleans the
// agent-keyed side-table rows Store.Delete leaves behind (see PurgeAgentData).
func (m *Manager) Remove(name string, force, purge bool) error {
	ag, err := m.cfg.Store.Get(name)
	if err != nil {
		return err
	}
	m.mu.Lock()
	rt := m.runs[name]
	// Refuse to nuke an agent whose loop is enabled (intent to run) or that has an
	// iteration in flight right now — unless forced. This is the reality-derived
	// equivalent of the old state=="running" guard.
	iterating := rt != nil && rt.engine.CurrentIterationID() != ""
	if (ag.Enabled || iterating) && !force {
		m.mu.Unlock()
		return fmt.Errorf("agent %q is running; stop it first or use --force", name)
	}
	servers := map[*agentapi.Server]struct{}{}
	if rt != nil {
		rt.cancel()
		servers[rt.apiServer] = struct{}{}
		delete(m.runs, name)
	}
	if srv, ok := m.toolsAPI[name]; ok {
		servers[srv] = struct{}{}
		delete(m.toolsAPI, name)
	}
	m.mu.Unlock()
	shutdownAgentAPIs(servers)

	l := agentdir.New(m.cfg.AgentsDir, name)
	rl := l.WithRuntime(m.cfg.RuntimeDir)
	// Runtime sockets are pure runtime, never data — drop them in either path.
	_ = os.Remove(rl.Sock())
	_ = os.Remove(rl.ShimSock())

	if !purge {
		// Preserve: leave the agent stopped (loop disabled) and every durable
		// artifact intact; remove only the rebuildable tree.
		ag.Enabled = false
		ag.LoopEnabled = false
		if err := m.cfg.Store.Update(ag); err != nil {
			return err
		}
		for _, d := range []string{l.ImageDir(), l.BinDir(), l.Workdir()} {
			if err := os.RemoveAll(d); err != nil {
				return err
			}
		}
		return nil
	}

	// Purge: hard-delete the core rows (iterations/secrets/agents), the leaked
	// agent-keyed side-table rows, and the whole durable tree.
	if err := m.cfg.Store.Delete(name); err != nil {
		return err
	}
	if err := m.cfg.Store.PurgeAgentData(name); err != nil {
		return err
	}
	return os.RemoveAll(l.Root)
}

// Reprovision re-unpacks image into an existing agent's rebuildable tree
// (image/bin/workdir) and restarts its loop, WITHOUT touching CONTEXT.md,
// iterations or audit.jsonl. It is the up-side counterpart of a preserving
// Remove: after a data-preserving down, up calls this to bring the agent back on
// the (possibly new) image while keeping its history. When imageRef differs from
// the stored one this performs an in-place image swap (the row's image_ref/digest
// are updated to the new image). An empty imageRef keeps the agent's current
// image.
func (m *Manager) Reprovision(name, imageRef string) error {
	return image.WithPublicationGate(func() error { return m.reprovision(name, imageRef) })
}

func (m *Manager) reprovision(name, imageRef string) error {
	ag, err := m.cfg.Store.Get(name)
	if err != nil {
		return err
	}
	if imageRef == "" {
		imageRef = ag.ImageRef
	}
	ref, err := image.ParseRef(imageRef)
	if err != nil {
		return err
	}
	archive, err := m.cfg.ImgStore.ArchiveBytes(ref)
	if err != nil {
		return fmt.Errorf("image %s: %w", ref.String(), err)
	}
	man, err := image.ValidatePortableArchiveContract(archive, ref, m.imagePluginResolver())
	if err != nil {
		return fmt.Errorf("image %s: %w", ref.String(), err)
	}
	candidate := ag
	candidate.Enabled = true
	candidate.LoopEnabled = true
	candidate.ImageRef = ref.String()
	candidate.ImageDigest = man.Digest
	if man.SchemaVersion == 2 {
		candidate.Plugins = make([]string, 0, len(man.Plugins))
		for _, plugin := range man.Plugins {
			candidate.Plugins = append(candidate.Plugins, plugin.Name)
		}
	}
	l := agentdir.New(m.cfg.AgentsDir, name)
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		return err
	}
	stageRoot, err := os.MkdirTemp(l.Root, ".reprovision-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageRoot)
	stage := agentdir.Layout{Root: stageRoot, Name: name}
	if err := agentdir.Provision(stage, candidate, m.cfg.ImgStore, ref); err != nil {
		return err
	}
	if _, err := m.prepareImageSkillBridge(candidate, man, stage.ImageDir()); err != nil {
		return err
	}
	imageBackup, binBackup := filepath.Join(l.Root, ".image-backup"), filepath.Join(l.Root, ".bin-backup")
	_ = os.RemoveAll(imageBackup)
	_ = os.RemoveAll(binBackup)
	hadImage, hadBin := false, false
	if err := os.Rename(l.ImageDir(), imageBackup); err == nil {
		hadImage = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(l.BinDir(), binBackup); err == nil {
		hadBin = true
	} else if !os.IsNotExist(err) {
		if hadImage {
			_ = os.Rename(imageBackup, l.ImageDir())
		}
		return err
	}
	rollback := func() {
		_ = os.RemoveAll(l.ImageDir())
		_ = os.RemoveAll(l.BinDir())
		if hadImage {
			_ = os.Rename(imageBackup, l.ImageDir())
		}
		if hadBin {
			_ = os.Rename(binBackup, l.BinDir())
		}
	}
	if err := os.Rename(stage.ImageDir(), l.ImageDir()); err != nil {
		rollback()
		return err
	}
	if err := os.MkdirAll(l.BinDir(), 0o700); err != nil {
		rollback()
		return err
	}
	if err := agentdir.WriteShims(l, candidate); err != nil {
		rollback()
		return err
	}
	for _, dir := range []string{l.Workdir(), l.IterationsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			rollback()
			return err
		}
	}
	if err := m.cfg.Store.SetImageIdentity(ag.Name, candidate.ImageRef, candidate.ImageDigest); err != nil {
		rollback()
		return err
	}
	if err := m.cfg.Store.Update(candidate); err != nil {
		_ = m.cfg.Store.SetImageIdentity(ag.Name, ag.ImageRef, ag.ImageDigest)
		rollback()
		return err
	}
	_ = os.RemoveAll(imageBackup)
	_ = os.RemoveAll(binBackup)
	m.signalGoals()
	return m.start(candidate)
}

func (m *Manager) imagePluginResolver() plugincaps.ExternalResolver {
	if m.cfg.ExternalPlugins != nil {
		return m.cfg.ExternalPlugins
	}
	return plugins.ResolveInstalledMetadata(filepath.Join(filepath.Dir(m.cfg.ImgStore.Dir), "plugins"))
}

func (m *Manager) Screen(name string) (string, error) {
	client, _, err := m.currentShimClient(name)
	if err != nil {
		return "", err
	}
	return client.Screen()
}

func (m *Manager) SendKeys(name, keys string) error {
	client, _, err := m.currentShimClient(name)
	if err != nil {
		return err
	}
	return client.SendKeys(keys)
}

func (m *Manager) SendKeysItems(name string, items []shim.KeyItem) error {
	client, _, err := m.currentShimClient(name)
	if err != nil {
		return err
	}
	return client.SendKeysItems(items)
}

func (m *Manager) Attach(name string, cols, rows int) (net.Conn, error) {
	client, _, err := m.currentShimClient(name)
	if err != nil {
		return nil, err
	}
	return client.Attach(cols, rows)
}

func (m *Manager) Resize(name string, cols, rows int) error {
	client, _, err := m.currentShimClient(name)
	if err != nil {
		return err
	}
	return client.Resize(cols, rows)
}

// currentShimClient resolves the current iteration and connects to its shim
// while holding the manager lock. Adoption cleanup uses the same lock before it
// releases startAfter, so a stable per-agent socket path cannot be rebound to a
// replacement iteration between resolution and connection.
func (m *Manager) currentShimClient(name string) (*shim.Client, iterationTarget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, err := m.currentIterationTargetLocked(name)
	if err != nil {
		return nil, iterationTarget{}, err
	}
	client, err := m.cfg.ConnectShim(target.sock)
	if err != nil {
		return nil, target, err
	}
	return client, target, nil
}

func (m *Manager) currentIterationTargetLocked(name string) (iterationTarget, error) {
	rt := m.runs[name]
	if rt != nil {
		if id := rt.engine.CurrentIterationID(); id != "" {
			return iterationTarget{
				id: id,
				sock: agentdir.New(m.cfg.AgentsDir, name).
					WithRuntime(m.cfg.RuntimeDir).ShimSock(),
				runtime: rt,
			}, nil
		}
	}
	if live, ok := m.adopting[name]; ok {
		return iterationTarget{id: live.ID, sock: live.ShimSock}, nil
	}
	if rt == nil {
		return iterationTarget{}, fmt.Errorf("agent %q is not running", name)
	}
	return iterationTarget{}, fmt.Errorf("agent %q has no running iteration", name)
}

// LiveState derives an agent's reported lifecycle state from reality — never
// from a persisted column. The master !enabled switch wins over everything
// else, reporting "stopped" regardless of a stale error_reason or
// loop_enabled intent; then error_reason wins; then an actually-executing
// iteration (a live engine with a current id, or a live iteration on disk
// during the post-crash adopt window) reports "running" unless its AI-proxy
// activity is stale; otherwise an
// enabled agent with nothing live is "idle".
func (m *Manager) LiveState(name string) (string, error) {
	a, err := m.cfg.Store.Get(name)
	if err != nil {
		return "", err
	}
	// Master switch wins: a disabled agent is "stopped" regardless of a stale
	// error_reason or loop_enabled intent.
	if !a.Enabled {
		return "stopped", nil
	}
	if a.ErrorReason != "" {
		return "error", nil
	}
	m.mu.Lock()
	iterationID := ""
	if rt := m.runs[name]; rt != nil {
		iterationID = rt.engine.CurrentIterationID()
	}
	if iterationID == "" {
		iterationID = m.adopting[name].ID
	}
	m.mu.Unlock()
	if iterationID != "" {
		it, err := m.cfg.Store.GetIteration(name, iterationID)
		if err == nil {
			last := it.StartedAt
			if it.LastAIRequestAt != "" {
				last = it.LastAIRequestAt
			}
			if at, err := time.Parse(time.RFC3339Nano, last); err == nil &&
				m.cfg.Clock().Sub(at) >= time.Duration(a.AIStallTimeoutS)*time.Second {
				return "error", nil
			}
		}
		return "running", nil
	}
	if m.hasLiveIterationOnDisk(name) {
		return "running", nil
	}
	// Enabled but nothing executing (loop on-waiting or loop off) → idle.
	return "idle", nil
}

// hasLiveIterationOnDisk reports whether a shim socket exists for the agent's
// current iteration — the signal that an iteration is mid-flight during the
// reattach/adopt window before the engine is registered in runs.
func (m *Manager) hasLiveIterationOnDisk(name string) bool {
	sock := agentdir.New(m.cfg.AgentsDir, name).WithRuntime(m.cfg.RuntimeDir).ShimSock()
	_, err := os.Stat(sock)
	return err == nil
}

// ActiveAgents is the number of running per-agent loop engines — the
// active-agents gauge for telemetry (spec §14).
func (m *Manager) ActiveAgents() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.runs)
}

// Shutdown cancels every engine and reattach goroutine and waits for them to
// quiesce (bounded by shutdownWait) so daemon teardown does not race the store
// Close that follows it.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	servers := map[*agentapi.Server]struct{}{}
	if m.stop != nil {
		m.stop() // unblock adopt/startAfter goroutines
	}
	for name, rt := range m.runs {
		rt.cancel()
		servers[rt.apiServer] = struct{}{}
		delete(m.runs, name)
	}
	// Agents that only ever got the boot-time bind (engine never started) own
	// their tools server here, not in runs.
	for name, srv := range m.toolsAPI {
		servers[srv] = struct{}{}
		delete(m.toolsAPI, name)
	}
	m.mu.Unlock()
	shutdownAgentAPIs(servers)

	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownWait):
		m.cfg.Log.Warn("shutdown: goroutines did not quiesce in time", "timeout", shutdownWait)
	}
}

func shutdownAgentAPIs(servers map[*agentapi.Server]struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	for srv := range servers {
		if srv != nil {
			_ = srv.Shutdown(ctx)
		}
	}
}

func (m *Manager) callerGroup(caller string) (agent.Agent, string, string, error) {
	ag, err := m.cfg.Store.Get(caller)
	if err != nil {
		return agent.Agent{}, "", "", err
	}
	if ag.Group == "" {
		return agent.Agent{}, "", "", fmt.Errorf("agent %q is not assigned to a group", caller)
	}
	if m.cfg.Groups == nil {
		return agent.Agent{}, "", "", fmt.Errorf("group control is not configured")
	}
	info, err := m.cfg.Groups.Inspect(ag.Group)
	if err != nil {
		return agent.Agent{}, "", "", err
	}
	lead, _ := info["lead"].(string)
	return ag, ag.Group, lead, nil
}

func (m *Manager) groupMember(caller, member string) (agent.Agent, string, string, error) {
	_, group, lead, err := m.callerGroup(caller)
	if err != nil {
		return agent.Agent{}, "", "", err
	}
	if member == "" {
		return agent.Agent{}, "", "", fmt.Errorf("member is required")
	}
	members, err := m.cfg.Store.ListByGroup(group)
	if err != nil {
		return agent.Agent{}, "", "", err
	}
	for _, a := range members {
		if a.Name == member {
			return a, group, lead, nil
		}
	}
	return agent.Agent{}, "", "", fmt.Errorf("agent %q is not in your group", member)
}

func (m *Manager) groupToolsInfo(caller string) (map[string]any, error) {
	ag, group, lead, err := m.callerGroup(caller)
	if err != nil {
		return nil, err
	}
	info, err := m.cfg.Groups.Inspect(group)
	if err != nil {
		return nil, err
	}
	info["agent"] = ag.Name
	info["group"] = group
	info["role"] = roleOf(ag.Name, lead)
	return info, nil
}

func (m *Manager) groupToolsStatus(caller, member string) (map[string]any, error) {
	_, group, lead, err := m.callerGroup(caller)
	if err != nil {
		return nil, err
	}
	members, err := m.cfg.Store.ListByGroup(group)
	if err != nil {
		return nil, err
	}
	if member != "" {
		for _, a := range members {
			if a.Name == member {
				return map[string]any{"member": m.groupStatusRow(a, lead)}, nil
			}
		}
		return nil, fmt.Errorf("agent %q is not in your group", member)
	}
	rows := make([]map[string]any, 0, len(members))
	for _, a := range members {
		rows = append(rows, m.groupStatusRow(a, lead))
	}
	return map[string]any{"members": rows, "count": len(rows)}, nil
}

func (m *Manager) groupStatusRow(a agent.Agent, lead string) map[string]any {
	state, _ := m.LiveState(a.Name)
	row := map[string]any{
		"name": a.Name, "role": roleOf(a.Name, lead), "state": state,
		"loop_enabled": a.LoopEnabled, "status_message": a.StatusMessage,
		"status_updated": a.StatusUpdated,
	}
	m.mu.Lock()
	if rt := m.runs[a.Name]; rt != nil {
		row["current_iteration"] = rt.engine.CurrentIterationID()
	}
	m.mu.Unlock()
	return row
}

func roleOf(agentName, lead string) string {
	if agentName == lead {
		return "lead"
	}
	return "member"
}

func (m *Manager) groupToolsSend(caller, iterationID, member, typ, text, deadline string) (map[string]any, error) {
	if m.cfg.Bus == nil {
		return nil, fmt.Errorf("bus not configured")
	}
	_, group, _, err := m.groupMember(caller, member)
	if err != nil {
		return nil, err
	}
	// group request is the request primitive (spec §4.2): publish kind=request to
	// the member's group:<g>:direct:<member> channel with a fresh correlation id,
	// reply_to=caller inbox, and — with a deadline — an armed one-shot timeout.
	// The member's reply retires the caller's pending (# Awaiting replies) and
	// cancels the timeout. No bespoke request bookkeeping: this replaces the old
	// path that hand-stuffed data["deadline"] onto a plain publish and dropped
	// kind/correlation_id.
	if typ == "group.request" {
		req, err := m.cfg.Bus.Request(caller, bus.GroupDirect(group, member), text, deadline)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sent": true, "id": req.ID, "target": member, "channel": req.Channel,
			"type": req.Type, "kind": req.Kind, "correlation_id": req.CorrelationID}, nil
	}
	msg, err := m.cfg.Bus.Publish(bus.Message{
		Channel: bus.InboxChannel(member), Type: typ, Text: text, Source: "agent:" + caller,
		ProducedByAgent: caller, ProducedInIteration: iterationID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"sent": true, "id": msg.ID, "target": member, "channel": msg.Channel, "type": msg.Type}, nil
}

func (m *Manager) groupToolsObserve(caller, member string, tail int) (map[string]any, error) {
	if _, _, _, err := m.groupMember(caller, member); err != nil {
		return nil, err
	}
	evs, err := audit.ReadEvents(agentdir.New(m.cfg.AgentsDir, member).AuditLog(), tail, 0)
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(evs))
	for _, ev := range evs {
		data := "{}"
		if ev.Data != nil {
			if b, err := json.Marshal(ev.Data); err == nil {
				data = string(b)
			}
		}
		rows = append(rows, map[string]any{"seq": ev.Seq, "kind": ev.Type, "source": ev.Source,
			"iteration_id": ev.IterationID, "data": data, "at": ev.TS})
	}
	return map[string]any{"member": member, "events": rows, "count": len(rows), "tail": tail}, nil
}

func (m *Manager) groupToolsLoop(caller, member, action string) (map[string]any, error) {
	if _, _, _, err := m.groupMember(caller, member); err != nil {
		return nil, err
	}
	updated, err := m.toolsLoopControl(member, action)
	if err != nil {
		return nil, err
	}
	return map[string]any{"member": member, "action": action,
		"enabled": updated.Enabled, "loop_enabled": updated.LoopEnabled}, nil
}

// toolsLoopControl owns both the master and Autopilot flags for the agent-facing
// loop controls. It intentionally leaves Start and Stop independent for the
// operator-facing lifecycle commands.
func (m *Manager) toolsLoopControl(name, action string) (agent.Agent, error) {
	switch action {
	case "start":
		ag, err := m.cfg.Store.Get(name)
		if err != nil {
			return agent.Agent{}, err
		}
		ag.LoopEnabled = true
		if err := m.cfg.Store.Update(ag); err != nil {
			return agent.Agent{}, err
		}
		if err := m.Start(name); err != nil {
			return agent.Agent{}, err
		}
	case "stop":
		if err := m.Stop(name); err != nil {
			return agent.Agent{}, err
		}
		ag, err := m.cfg.Store.Get(name)
		if err != nil {
			return agent.Agent{}, err
		}
		ag.LoopEnabled = false
		if err := m.cfg.Store.Update(ag); err != nil {
			return agent.Agent{}, err
		}
	default:
		return agent.Agent{}, fmt.Errorf("unknown loop action %q", action)
	}
	return m.cfg.Store.Get(name)
}

// buildImageForAgent authors + builds a new image from a Tariboyfile. Relative
// paths resolve against the managed workdir; absolute paths are used as given.
// It calls image.BuildV2 against the daemon's shared image store through the
// one publication mechanism the operator CLI uses, so an existing ref — latest
// included — is moved onto the new content instead of being refused.
func buildImageForAgent(imgStore *image.Store, workdir, name, tag, path string, externalPlugins ...plugincaps.ExternalResolver) (map[string]any, error) {
	var result map[string]any
	err := image.WithPublicationGate(func() error {
		var err error
		result, err = buildImageForAgentLocked(imgStore, workdir, name, tag, path, externalPlugins...)
		return err
	})
	return result, err
}

func buildImageForAgentLocked(imgStore *image.Store, workdir, name, tag, path string, externalPlugins ...plugincaps.ExternalResolver) (map[string]any, error) {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workdir, abs)
	}
	abs, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return nil, err
	}
	parsed, err := imagefile.ParseV2(abs)
	if err != nil {
		return nil, err
	}
	// An empty tag publishes the declared image_version and latest, exactly as
	// the operator CLI does.
	var requested []string
	if tag != "" {
		requested = []string{tag}
	}
	refs, _, err := image.BuildRefs(name, requested, parsed.ImageVersion)
	if err != nil {
		return nil, err
	}
	baseDir := filepath.Dir(imgStore.Dir)
	layout := paths.Paths{Base: baseDir}
	pluginsDir := layout.PluginsDir()
	if len(parsed.Extends) > 0 {
		assembled, cleanup, err := imagefile.Assemble(abs, filepath.Join(baseDir, "image-build"), imagefile.ResolveRoots{Plugins: pluginsDir}, nil)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		if parsed, err = imagefile.ParseV2(assembled); err != nil {
			return nil, err
		}
	}
	resolver := plugins.ResolveInstalledMetadata(pluginsDir)
	if len(externalPlugins) > 0 && externalPlugins[0] != nil {
		resolver = externalPlugins[0]
	}
	if err := imgStore.Migrate(); err != nil {
		return nil, err
	}
	man, err := image.BuildV2(parsed, imagefile.ResolveRoots{Plugins: pluginsDir}, refs[0], imgStore, time.Now, resolver)
	if err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref != refs[0] {
			if err := imgStore.SetTag(ref, man.Digest); err != nil {
				return nil, err
			}
		}
		tags = append(tags, ref.Tag)
	}
	return map[string]any{"name": man.Name, "tag": refs[0].Tag, "tags": tags, "digest": man.Digest, "layers": len(man.Layers)}, nil
}

func cwdOf(ag agent.Agent, l agentdir.Layout) string {
	if ag.Cwd != "" {
		return ag.Cwd
	}
	return l.Workdir()
}

func pick(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func mergeEnv(base, over map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// ensure the interface is satisfied at compile time
var _ registry.ServiceControl = (*Manager)(nil)
