// Package loop runs one iteration scheduler per agent (spec §5.2). The state
// machine wakes on interval cadence or a manual trigger, runs a single
// iteration to completion, records it, and applies on_timeout/on_error policy.
package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/events"
	"github.com/alekzonder/tariboy/internal/telemetry"
)

type TickResult string

const (
	TickDisabled         TickResult = "disabled"
	TickStarted          TickResult = "started"
	TickRestarted        TickResult = "restarted"
	TickCompletedWaiting TickResult = "completed_waiting"
	TickAwaitingPrevExit TickResult = "awaiting_prev_exit"
	TickRunning          TickResult = "running"
	TickTimeoutStop      TickResult = "timeout_stop"
	TickIdleStop         TickResult = "idle_stop"
	TickTimeoutRestart   TickResult = "timeout_restart"
	TickProcessDied      TickResult = "process_died"
	TickErrorHalt        TickResult = "error_halt"
	TickError            TickResult = "tick_error"
	TickSkipped          TickResult = "skipped"
	TickDetached         TickResult = "detached"
)

// Outcome is what the runner reports for one iteration.
type Outcome struct {
	Status   string // done | no_i_am_done | harness_error | timeout | killed
	ExitCode int
	DoneFlag bool
	// Productive carries the iteration's productive flag (see agent.Iteration).
	// It is read back from the DB so the engine's belt-and-suspenders
	// SetIterationDone does not clobber a `--idle` declaration made via the API.
	Productive bool
	CPUMs      int
	MemPeakKB  int
	// Error is the human-readable reason of a harness_error outcome, recorded
	// on the iteration_finished audit event.
	Error string
}

type IterationRunner interface {
	Run(ctx context.Context, ag agent.Agent, trigger, iterationID, oneShot string) (Outcome, error)
}

type manualReq struct {
	oneShot    string
	launchGate func() (release func(), ok bool)
}

// WakeKind names an external event that must re-evaluate or run the loop.
// WakeConfig/WakeStart/WakeStop only re-read config (the run loop stops its
// timer and reloads); WakeMessage additionally runs a message-triggered
// iteration and is wired in Task 5.
type WakeKind int

const (
	WakeConfig WakeKind = iota
	WakeStart
	WakeStop
	WakeMessage
)

// loopTimer is the stoppable timer seam used by Run. Production wraps
// time.NewTimer; tests inject a fake to count creations and record Stop calls so
// abandoned timers cannot silently leak.
type loopTimer interface {
	C() <-chan time.Time
	Stop()
}

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time { return r.t.C }
func (r *realTimer) Stop()               { r.t.Stop() }

func newRealTimer(d time.Duration) loopTimer { return &realTimer{t: time.NewTimer(d)} }

const messageRetryDelay = time.Second

const (
	messageWakeIdle uint32 = iota
	messageWakeQueued
	messageWakeRunning
	messageWakeRetry
)

type Engine struct {
	ag       agent.Agent
	store    *agent.Store
	runner   IterationRunner
	log      *slog.Logger
	clock    func() time.Time
	newTimer func(d time.Duration) loopTimer

	metrics     *telemetry.Metrics
	usageLookup func(iteration string) (int, int, float64)
	usageFlush  func()

	manualCh chan manualReq
	events   chan WakeKind
	// messageCh is separate from the best-effort config wake queue so a full
	// burst of config/start/stop nudges cannot discard a pending message
	// generation. messageWakeState guarantees at most one signal is queued.
	messageCh chan struct{}
	// messageWakeState coalesces queued publishes and blocked retries while
	// messageWakeDeferred preserves one genuinely new generation that arrives
	// during an already-running message iteration.
	messageWakeState    atomic.Uint32
	messageWakeDeferred atomic.Bool
	peek                func() (bool, error)
	emit                func(events.Event)
	audit               func(typ, source, iterationID string, data map[string]any)
	onClose             func(agent, iterationID string)
	iterationCompleted  func(agent, iterationID string)
	beforeLaunch        func(*agent.Agent) (activatedImage, error)

	mu            sync.Mutex
	current       string // current iteration id ("" when idle)
	currentCancel context.CancelFunc
	lastRun       time.Time // start of the previous iteration
}

func (e *Engine) SetBeforeLaunch(fn func(*agent.Agent) (activatedImage, error)) { e.beforeLaunch = fn }

func NewEngine(ag agent.Agent, st *agent.Store, runner IterationRunner, log *slog.Logger,
	clock func() time.Time) *Engine {
	return &Engine{
		ag: ag, store: st, runner: runner, log: log, clock: clock, newTimer: newRealTimer,
		manualCh:  make(chan manualReq, 8),
		events:    make(chan WakeKind, 16),
		messageCh: make(chan struct{}, 1),
	}
}

// Trigger requests a manual iteration with an optional one-shot prompt.
func (e *Engine) Trigger(oneShot string) {
	e.queueManual(manualReq{oneShot: oneShot})
}

func (e *Engine) triggerGuarded(oneShot string, gate func() (func(), bool)) {
	e.queueManual(manualReq{oneShot: oneShot, launchGate: gate})
}

func (e *Engine) queueManual(req manualReq) {
	select {
	case e.manualCh <- req:
	default:
		e.log.Warn("manual trigger dropped: queue full", "agent", e.ag.Name)
	}
}

// Wake nudges the run loop to re-evaluate (start/stop/config) or, for
// WakeMessage, to run a message-triggered iteration. Non-blocking: an
// undrained or shutting-down engine simply drops the nudge.
func (e *Engine) Wake(k WakeKind) {
	if k == WakeMessage {
		e.queueMessageWake()
		return
	}
	select {
	case e.events <- k:
	default:
	}
}

func (e *Engine) queueMessageWake() {
	for {
		switch state := e.messageWakeState.Load(); state {
		case messageWakeIdle:
			if !e.messageWakeState.CompareAndSwap(messageWakeIdle, messageWakeQueued) {
				continue
			}
			select {
			case e.messageCh <- struct{}{}:
			default:
				// This is only possible when a signal already represents the
				// pending generation (or after Run has shut down).
				e.messageWakeState.Store(messageWakeIdle)
			}
			return
		case messageWakeQueued, messageWakeRetry:
			// One queued wake or retry already represents the pending burst.
			return
		case messageWakeRunning:
			// A publish during the running iteration is a future generation,
			// not part of the burst that caused the current run. The state
			// recheck closes the race with finish/park: either that transition
			// consumes the flag, or this goroutine queues/drops it under the new
			// state.
			e.messageWakeDeferred.Store(true)
			if e.messageWakeState.Load() != messageWakeRunning &&
				e.messageWakeDeferred.Swap(false) {
				continue
			}
			return
		}
	}
}

// SetMessagePeek installs a non-consuming "are there pending messages?" probe
// (bus.HasPending). Nil means the engine never message-triggers.
func (e *Engine) SetMessagePeek(fn func() (bool, error)) { e.peek = fn }

// SetEmit installs an iteration-lifecycle event sink (SSE hub). Nil disables it.
func (e *Engine) SetEmit(fn func(events.Event)) { e.emit = fn }

// SetAudit installs the durable audit-log sink (per-agent audit.jsonl). Nil
// disables it. Best-effort by contract — the installed function must not fail
// into the loop.
func (e *Engine) SetAudit(fn func(typ, source, iterationID string, data map[string]any)) {
	e.audit = fn
}

func (e *Engine) recordAudit(typ, source, iterationID string, data map[string]any) {
	if e.audit != nil {
		e.audit(typ, source, iterationID, data)
	}
}

// SetOnIterationClose installs a hook invoked exactly once per finished
// iteration (every terminal outcome: done, no_i_am_done, harness_error,
// timeout, killed), after the iteration row's final status is persisted and no
// further writes under that iteration's dir can occur. Wired by the daemon to
// gzip the proxy transcript (spec §9/§12); nil disables it. Best-effort by
// contract of the installed function itself — the engine does not inspect or
// retry errors from it.
func (e *Engine) SetOnIterationClose(fn func(agent, iterationID string)) { e.onClose = fn }

// SetIterationCompleted installs the durable continuation hook used after a
// terminal iteration row is finalized. Nil disables it.
func (e *Engine) SetIterationCompleted(fn func(agent, iterationID string)) {
	e.iterationCompleted = fn
}

func (e *Engine) emitIteration(id, trigger, status, phase string) {
	if e.emit == nil {
		return
	}
	e.emit(events.Event{Agent: e.ag.Name, Type: "iteration", Time: e.clock().UTC().Format(time.RFC3339),
		Data: map[string]any{"id": id, "trigger": trigger, "status": status, "phase": phase}})
}

func (e *Engine) CurrentIterationID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current
}

// AbortCurrent stops a specific in-flight iteration without stopping its loop.
// It is used when the manager has established that the iteration's shim is no
// longer reachable, so the runner cannot otherwise observe its completion.
func (e *Engine) AbortCurrent(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != id || e.currentCancel == nil {
		return false
	}
	e.currentCancel()
	return true
}

// reload re-reads the agent config so runtime-mutable settings apply on the next
// iteration (spec §5.1).
func (e *Engine) reload() {
	if a, err := e.store.Get(e.ag.Name); err == nil {
		e.ag = a
	}
}

func (e *Engine) Run(ctx context.Context) {
	// A message retry is independent from the interval cadence. It survives
	// trips around the outer loop, but there is never more than one armed timer:
	// publish bursts coalesce while an interactive session owns the harness.
	var messageRetry loopTimer
	var messageRetryC <-chan time.Time
	messageBlocked := false
	abandonMessage := func() {
		e.messageWakeState.Store(messageWakeIdle)
		e.messageWakeDeferred.Store(false)
	}
	finishMessage := func() {
		e.messageWakeState.Store(messageWakeIdle)
		if e.messageWakeDeferred.Swap(false) {
			e.queueMessageWake()
		}
	}
	parkMessage := func() {
		e.messageWakeState.Store(messageWakeRetry)
		e.messageWakeDeferred.Store(false)
	}
	stopMessageRetry := func() {
		if messageRetry != nil {
			messageRetry.Stop()
			messageRetry = nil
			messageRetryC = nil
		}
	}
	armMessageRetry := func() {
		parkMessage()
		if messageRetry != nil {
			return
		}
		messageRetry = e.newTimer(messageRetryDelay)
		messageRetryC = messageRetry.C()
	}
	tryMessage := func() {
		if ctx.Err() != nil {
			messageBlocked = false
			abandonMessage()
			stopMessageRetry()
			return
		}
		e.reload()
		if !e.ag.Enabled || !e.ag.LoopEnabled {
			if messageBlocked {
				parkMessage()
			} else {
				finishMessage()
			}
			stopMessageRetry()
			return
		}
		if !e.hasPending() {
			messageBlocked = false
			finishMessage()
			stopMessageRetry()
			return
		}
		result := e.runOnce(ctx, "message", "")
		if result == TickSkipped && e.hasPending() {
			messageBlocked = true
			armMessageRetry()
			return
		}
		messageBlocked = false
		finishMessage()
		stopMessageRetry()
	}

	for {
		e.reload()
		// Event-only agents (IntervalS <= 0) create no timer at all: a nil
		// channel blocks forever in select, so we never allocate a runtime
		// timer that a manual trigger would then abandon and leak.
		var timer loopTimer
		var timerC <-chan time.Time
		if e.ag.Enabled && e.ag.LoopEnabled && e.ag.IntervalS > 0 {
			timer = e.newTimer(e.nextWait())
			timerC = timer.C()
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			stopMessageRetry()
			abandonMessage()
			return
		case <-e.messageCh:
			if timer != nil {
				timer.Stop()
			}
			// An armed retry already owns this pending condition. Extra
			// publishes only coalesce; they neither create another timer nor
			// spin against the still-live interactive session.
			if messageRetry == nil {
				e.messageWakeState.Store(messageWakeRunning)
				tryMessage()
			} else {
				e.reload()
				if !e.ag.Enabled || !e.ag.LoopEnabled || !e.hasPending() {
					if e.ag.Enabled && e.ag.LoopEnabled {
						messageBlocked = false
						finishMessage()
					} else if !messageBlocked {
						finishMessage()
					}
					stopMessageRetry()
				}
			}
		case <-e.events:
			if timer != nil {
				timer.Stop()
			}
			if messageRetry != nil || messageBlocked {
				// A config/start/stop wake invalidates the old delay. Re-read
				// persisted flags, cancel the old timer, and repark exactly one
				// retry only when the blocked pending condition still applies.
				stopMessageRetry()
				e.reload()
				if e.ag.Enabled && e.ag.LoopEnabled {
					if e.hasPending() {
						armMessageRetry()
					} else {
						messageBlocked = false
						finishMessage()
					}
				} else if messageBlocked {
					parkMessage()
				} else {
					finishMessage()
				}
			}
			// Any wake falls through to the top of the loop, where reload()
			// re-reads config and recreates or omits the timer. This both wakes
			// a parked engine (start/re-enable) and cancels a doomed timer
			// (stop); WakeMessage additionally runs a message iteration above.
		case m := <-e.manualCh:
			if timer != nil {
				timer.Stop()
			}
			e.runOnceGuarded(ctx, "manual", m.oneShot, m.launchGate)
		case <-timerC:
			// The timer fired and drained its channel; no Stop needed.
			// Pause/config can race this select with an already-ready timer.
			// Re-read persisted intent at the last responsible moment so stale
			// state cannot launch one extra billable iteration.
			e.reload()
			if e.ag.Enabled && e.ag.LoopEnabled && e.ag.IntervalS > 0 {
				e.runOnce(ctx, "interval", "")
			}
		case <-messageRetryC:
			if timer != nil {
				timer.Stop()
			}
			// The retry timer fired and its channel is drained, so detach it
			// without Stop before attempting. A still-blocked attempt can then
			// arm the next single retry.
			messageRetry = nil
			messageRetryC = nil
			e.messageWakeState.Store(messageWakeRunning)
			tryMessage()
		}
	}
}

func (e *Engine) hasPending() bool {
	if e.peek == nil {
		return false
	}
	ok, err := e.peek()
	if err != nil {
		e.log.Error("message peek", "agent", e.ag.Name, "err", err)
		return false
	}
	return ok
}

// nextWait returns the delay until the next interval firing, measured from the
// previous iteration's start.
func (e *Engine) nextWait() time.Duration {
	if e.ag.IntervalS <= 0 {
		return time.Duration(1 << 62) // effectively "never" for event-only agents
	}
	e.mu.Lock()
	last := e.lastRun
	e.mu.Unlock()
	if last.IsZero() {
		return 0
	}
	elapsed := e.clock().Sub(last)
	remaining := time.Duration(e.ag.IntervalS)*time.Second - elapsed
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (e *Engine) runOnce(ctx context.Context, trigger, oneShot string) TickResult {
	return e.runOnceGuarded(ctx, trigger, oneShot, nil)
}

func (e *Engine) runOnceGuarded(
	ctx context.Context,
	trigger, oneShot string,
	launchGate func() (release func(), ok bool),
) TickResult {
	var release func()
	gateAcquired := false
	releaseLaunch := func() {
		if release != nil {
			release()
			release = nil
		}
	}
	acquireLaunch := func() bool {
		if launchGate == nil || gateAcquired {
			return true
		}
		var ok bool
		release, ok = launchGate()
		gateAcquired = ok
		return ok
	}
	defer releaseLaunch()
	if ctx.Err() != nil {
		return TickDisabled
	}
	// Launch guard (spec: tmux session collision). An interactive agent whose tmux
	// session is already alive must not spawn a second iteration on top: a
	// loop-triggered tick is skipped, a manual one is failed with a visible reason.
	if b, ok := e.runner.(interface {
		SessionBlocked(agent.Agent) bool
	}); ok && b.SessionBlocked(e.ag) {
		// Self-heal an orphaned session before failing. A tmux session left by a
		// crashed daemon / failed adoption has no running iteration to own it and
		// would otherwise block every future iteration forever (on_error=restart
		// never sees this — it is a pre-launch guard, not an iteration outcome).
		// Reap it and fall through to launch; a session still owned by a live
		// iteration is a real conflict and is left untouched.
		reaped := false
		if rp, ok := e.runner.(interface {
			ReapOrphanBlock(agent.Agent) bool
		}); ok {
			reaped = rp.ReapOrphanBlock(e.ag)
		}
		if !reaped {
			if trigger == "manual" {
				if !acquireLaunch() {
					return TickDisabled
				}
				now := e.clock()
				id, err := e.store.NextIterationID(e.ag.Name, now)
				if err != nil {
					e.log.Error("next iteration id", "agent", e.ag.Name, "err", err)
					return TickError
				}
				ts := now.Format(time.RFC3339)
				it := agent.Iteration{
					ID: id, Agent: e.ag.Name, Trigger: trigger, Status: "failed",
					StartedAt: ts, EndedAt: ts,
				}
				if err := e.store.CreateIteration(it); err != nil {
					e.log.Error("create iteration", "agent", e.ag.Name, "err", err)
					return TickError
				}
				releaseLaunch()
				e.log.Warn("manual iteration failed: tmux session already alive",
					"agent", e.ag.Name, "id", id)
				e.recordAudit("iteration_failed", "system", id,
					map[string]any{"reason": "tmux_session_exists"})
				e.emitIteration(id, trigger, "failed", "finish")
				if e.iterationCompleted != nil {
					e.iterationCompleted(e.ag.Name, id)
				}
				return TickError
			}
			e.log.Info("loop iteration skipped: tmux session still alive", "agent", e.ag.Name)
			e.recordAudit("iteration_skipped", "system", "",
				map[string]any{"reason": "session_alive"})
			return TickSkipped
		}
		e.log.Info("reaped orphan tmux session before launch", "agent", e.ag.Name)
		e.recordAudit("session_reaped", "system", "",
			map[string]any{"reason": "orphan_before_launch"})
	}

	if !acquireLaunch() {
		return TickDisabled
	}
	activated := activatedImage{}
	if e.beforeLaunch != nil {
		var err error
		activated, err = e.beforeLaunch(&e.ag)
		if err != nil {
			// Pre-launch failures (unverifiable manifest, missing image skills,
			// unusable harness) produce no iteration row, so the daemon log was
			// their only trace and the loop repeated the same failure every
			// tick. Halt with the cause in error_reason, which the agent status
			// surfaces as halt_reason; Start/Restart clears it.
			e.log.Error("prepare image activation", "agent", e.ag.Name, "err", err)
			e.disableLoop("image activation failed: " + err.Error())
			e.recordAudit("iteration_failed", "system", "", map[string]any{
				"reason": "image_activation_failed", "error": err.Error(),
			})
			return TickErrorHalt
		}
	}
	now := e.clock()
	id, err := e.store.NextIterationID(e.ag.Name, now)
	if err != nil {
		e.log.Error("next iteration id", "agent", e.ag.Name, "err", err)
		return TickError
	}
	it := agent.Iteration{
		ID: id, Agent: e.ag.Name, Trigger: trigger, Status: "running",
		StartedAt: now.Format(time.RFC3339), ImageRef: e.ag.ImageRef,
		ImageVersion: activated.ImageVersion, ImageDigest: e.ag.ImageDigest, PromptTemplateSHA256: activated.PromptTemplateSHA256,
	}
	if err := e.store.CreateIteration(it); err != nil {
		e.log.Error("create iteration", "agent", e.ag.Name, "err", err)
		return TickError
	}
	// Root iteration span (spec §14). OTel-off installs a no-op tracer, so span
	// start/end and attribute-setting are free and never alter iteration
	// behavior. span.End() is deferred so it fires on every exit path
	// (done/no_i_am_done/harness_error/timeout/killed); ctx carries the span so
	// the runner's child spans nest under it.
	runCtx, cancel := context.WithCancel(ctx)
	runCtx = context.WithValue(runCtx, activatedImageContextKey{}, activated)
	e.mu.Lock()
	e.current = id
	e.lastRun = now
	e.currentCancel = cancel
	e.mu.Unlock()
	defer cancel()
	releaseLaunch()
	e.log.Info("iteration started", "agent", e.ag.Name, "id", id, "trigger", trigger)
	e.emitIteration(id, trigger, "running", "start")
	e.recordAudit("iteration_started", "system", id, map[string]any{
		"trigger": trigger, "image_ref": e.ag.ImageRef,
		"image_version": activated.ImageVersion, "image_digest": e.ag.ImageDigest,
	})
	tr := otel.Tracer("tariboy/loop")
	spanStart := e.clock()
	runCtx, span := tr.Start(runCtx, "iteration", oteltrace.WithAttributes(
		attribute.String("agent", e.ag.Name),
		attribute.String("iteration_id", id),
		attribute.String("trigger", trigger),
		attribute.String("image_ref", e.ag.ImageRef),
	))
	defer span.End()

	// Fires after every closed runner below (harness_error included), but not
	// after a restart handoff: the adopted shim still owns that iteration. Goal
	// completion is signaled separately by the component that successfully
	// commits the terminal row.
	detached := false
	defer func() {
		if detached {
			return
		}
		if e.onClose != nil {
			e.onClose(e.ag.Name, id)
		}
	}()

	outcome, runErr := e.runner.Run(runCtx, e.ag, trigger, id, oneShot)

	e.mu.Lock()
	e.current = ""
	e.currentCancel = nil
	e.mu.Unlock()

	end := e.clock().Format(time.RFC3339)
	if errors.Is(runErr, ErrIterationDetached) {
		detached = true
		e.log.Info("iteration detached for daemon handoff", "agent", e.ag.Name, "id", id)
		return TickDetached
	}
	if runErr != nil {
		e.log.Error("iteration runner failed", "agent", e.ag.Name, "id", id, "err", runErr)
		// A manager-side stale-shim recovery may have finalized this row while
		// the runner was unwinding its cancelled context. Do not erase that
		// durable terminal detail (notably its synthetic exit code).
		if current, err := e.store.GetIteration(e.ag.Name, id); err == nil {
			current.Status = "harness_error"
			current.EndedAt = end
			if committed, err := e.store.FinalizeRunningIteration(current); err != nil {
				e.log.Error("update iteration", "agent", e.ag.Name, "id", id, "err", err)
			} else if committed {
				e.emitIteration(id, trigger, "harness_error", "finish")
				e.recordAudit("iteration_finished", "system", id, map[string]any{"status": "harness_error", "error": runErr.Error()})
				if e.iterationCompleted != nil {
					e.iterationCompleted(e.ag.Name, id)
				}
			}
		}
		e.finishSpan(span, spanStart, id, "harness_error", 0, 0)
		return TickError
	}

	ec := outcome.ExitCode
	cpu := outcome.CPUMs
	mem := outcome.MemPeakKB
	it.Status = outcome.Status
	it.EndedAt = end
	it.DoneFlag = outcome.DoneFlag
	it.ExitCode = &ec
	it.CPUMs = &cpu
	it.MemPeakKB = &mem
	terminalPersisted, err := e.store.FinalizeRunningIteration(it)
	if err != nil {
		e.log.Error("update iteration", "agent", e.ag.Name, "id", id, "err", err)
		return TickSkipped
	}
	if !terminalPersisted {
		return TickSkipped
	}
	if outcome.DoneFlag {
		// done_flag is owned exclusively by SetIterationDone (spec §5.2).
		// outcome.Productive was read from the DB, so an `--idle` declaration made
		// via the API during the iteration is preserved rather than overwritten.
		if err := e.store.SetIterationDone(id, outcome.Productive); err != nil {
			e.log.Error("set iteration done", "agent", e.ag.Name, "id", id, "err", err)
		}
	}
	e.log.Info("iteration finished", "agent", e.ag.Name, "id", id, "status", outcome.Status)
	e.emitIteration(id, trigger, outcome.Status, "finish")
	finished := map[string]any{"status": outcome.Status}
	if outcome.Error != "" {
		finished["error"] = outcome.Error
	}
	e.recordAudit("iteration_finished", "system", id, finished)

	e.finishSpan(span, spanStart, id, outcome.Status, outcome.CPUMs, outcome.MemPeakKB)
	result := e.applyPolicy(outcome)
	if e.iterationCompleted != nil {
		e.iterationCompleted(e.ag.Name, id)
	}
	return result
}

// finishSpan sets the terminal attributes/status on the iteration span and
// records the iteration metric (Task 2). Tokens/cost are best-effort span
// attributes (spec §14): usageLookup reflects what the async proxy ingester has
// persisted at classify time, which may lag the last request — the accurate
// accounting view remains usage/budget (the DB). Every step here is nil-safe
// and cannot fail the iteration: a no-op tracer yields no-op spans, and a nil
// usageLookup/metrics is skipped.
func (e *Engine) finishSpan(span oteltrace.Span, start time.Time, id, outcome string, cpu, mem int) {
	dur := float64(e.clock().Sub(start).Milliseconds())
	span.SetAttributes(
		attribute.String("outcome", outcome),
		attribute.Int("cpu_ms", cpu),
		attribute.Int("mem_peak_kb", mem),
	)
	if e.usageLookup != nil {
		if e.usageFlush != nil {
			e.usageFlush()
		}
		in, out, cost := e.usageLookup(id)
		span.SetAttributes(
			attribute.Int("tokens_in", in),
			attribute.Int("tokens_out", out),
			attribute.Float64("cost_usd", cost),
		)
	}
	if outcome == "harness_error" || outcome == "timeout" {
		span.SetStatus(codes.Error, outcome)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	e.metrics.RecordIteration(context.Background(), outcome, dur)
}

func (e *Engine) applyPolicy(o Outcome) TickResult {
	switch o.Status {
	case "done", "no_i_am_done":
		if res, stopped := e.maybeIdleStop(); stopped {
			return res
		}
		return TickCompletedWaiting
	case "killed":
		return TickProcessDied
	case "timeout":
		if e.ag.OnTimeout == "stop" {
			e.disableLoop("timeout")
			return TickTimeoutStop
		}
		return TickTimeoutRestart
	case "harness_error":
		if e.ag.OnError == "stop" {
			e.disableLoop("harness_error")
			return TickErrorHalt
		}
		return TickRestarted
	default:
		return TickCompletedWaiting
	}
}

// disableLoop halts the loop when a stop policy (on_timeout=stop / on_error=stop)
// fires. SetError records the halt reason and clears loop_enabled in one write,
// so the agent surfaces as state=error (derived) rather than a silent stopped.
// The engine goroutine itself stays alive, blocked on a manual trigger.
func (e *Engine) disableLoop(reason string) {
	e.ag.LoopEnabled = false
	if err := e.store.SetError(e.ag.Name, "halted: "+reason); err != nil {
		e.log.Error("disable loop", "agent", e.ag.Name, "err", err)
	}
}

// maybeIdleStop applies the idle-autostop policy after a completed iteration
// (Task 2). When MaxIdleIterations > 0 and the agent's consecutive idle streak
// (self-declared via `i-am-done --idle`) has reached the threshold, it cleanly
// stops the loop and returns (TickIdleStop, true). MaxIdleIterations == 0 means
// the feature is disabled and it never stops. Any query error is logged and
// treated as "do not stop" so a transient DB hiccup cannot silently halt a loop.
func (e *Engine) maybeIdleStop() (TickResult, bool) {
	if e.ag.MaxIdleIterations <= 0 {
		return "", false
	}
	streak, err := e.store.IdleStreak(e.ag.Name)
	if err != nil {
		e.log.Error("idle streak query", "agent", e.ag.Name, "err", err)
		return "", false
	}
	if streak < e.ag.MaxIdleIterations {
		return "", false
	}
	// The streak is scoped to iterations since the last Start (IdleStreak +
	// StartResetIdle), so at the trip it equals the threshold. Clamp defensively
	// so the reported reason can never exceed MaxIdleIterations.
	count := streak
	if count > e.ag.MaxIdleIterations {
		count = e.ag.MaxIdleIterations
	}
	e.idleStop(fmt.Sprintf(agent.IdleStopPrefix+" (%d idle iterations)", count))
	return TickIdleStop, true
}

// idleStop performs the clean idle halt: it clears loop_enabled and the master
// enabled switch and records the reason in status_message via SetIdleStopped
// (NOT SetError), so the agent surfaces as derived state=stopped with a
// visible, non-error reason. The engine goroutine stays alive, blocked until a
// manual trigger or a Start re-enables it.
func (e *Engine) idleStop(reason string) {
	e.ag.LoopEnabled = false
	e.ag.Enabled = false
	updated := e.clock().UTC().Format(time.RFC3339)
	if err := e.store.SetIdleStopped(e.ag.Name, reason, updated); err != nil {
		e.log.Error("idle stop", "agent", e.ag.Name, "err", err)
	}
	e.log.Info("loop idle-stopped", "agent", e.ag.Name, "reason", reason)
	e.recordAudit("loop_idle_stopped", "system", "", map[string]any{"reason": reason})
}
