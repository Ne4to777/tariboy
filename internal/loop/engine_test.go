package loop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/events"
	"github.com/alekzonder/tariboy/internal/store"
)

type fakeRunner struct {
	outcomes []Outcome
	calls    int
	seen     []string // iteration ids
}

func (f *fakeRunner) Run(_ context.Context, _ agent.Agent, _, id, _ string) (Outcome, error) {
	o := f.outcomes[min(f.calls, len(f.outcomes)-1)]
	f.seen = append(f.seen, id)
	f.calls++
	return o, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func newEngine(t *testing.T, ag agent.Agent, r IterationRunner) (*Engine, *agent.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	as := agent.NewStore(s)
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	clk := func() time.Time { return time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC) }
	e := NewEngine(ag, as, r, slog.New(slog.NewTextHandler(io.Discard, nil)), clk)
	return e, as
}

// fakeTimer records Stop calls and never fires unless a test writes to ch.
type fakeTimer struct {
	ch      chan time.Time
	stopped int32
}

func (f *fakeTimer) C() <-chan time.Time { return f.ch }
func (f *fakeTimer) Stop()               { atomic.AddInt32(&f.stopped, 1) }

// fakeTimerFactory counts how many timers Run creates and hands out fakeTimers.
type fakeTimerFactory struct {
	mu      sync.Mutex
	created int
	timers  []*fakeTimer
	delays  []time.Duration
}

func (f *fakeTimerFactory) newTimer(delay time.Duration) loopTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{ch: make(chan time.Time, 1)}
	f.created++
	f.timers = append(f.timers, t)
	f.delays = append(f.delays, delay)
	return t
}

func (f *fakeTimerFactory) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

func (f *fakeTimerFactory) lastTimer() *fakeTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.timers) == 0 {
		return nil
	}
	return f.timers[len(f.timers)-1]
}

func (f *fakeTimerFactory) lastDelay() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.delays) == 0 {
		return 0
	}
	return f.delays[len(f.delays)-1]
}

func baseAgent() agent.Agent {
	return agent.Agent{Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub", Enabled: true, LoopEnabled: true, IntervalS: 60, TimeoutS: 30,
		OnTimeout: "restart", OnError: "restart", Plugins: []string{"whoami", "loop", "messages"}}
}

func TestRunOnceDone(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true, CPUMs: 5, MemPeakKB: 9}}}
	e, as := newEngine(t, baseAgent(), r)
	res := e.runOnce(context.Background(), "interval", "")
	if res != TickCompletedWaiting {
		t.Fatalf("res = %q, want completed_waiting", res)
	}
	its, _ := as.ListIterations("smoke")
	if len(its) != 1 || its[0].Status != "done" || !its[0].DoneFlag {
		t.Fatalf("iteration = %+v", its)
	}
	if its[0].CPUMs == nil || *its[0].CPUMs != 5 {
		t.Fatalf("cpu not persisted: %+v", its[0])
	}
}

func TestRunOnceNoIAmDone(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "no_i_am_done", ExitCode: 0}}}
	e, as := newEngine(t, baseAgent(), r)
	if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
		t.Fatalf("res = %q", res)
	}
	its, _ := as.ListIterations("smoke")
	if its[0].Status != "no_i_am_done" {
		t.Fatalf("status = %q", its[0].Status)
	}
}

func TestRunOnceHarnessErrorStopHalts(t *testing.T) {
	ag := baseAgent()
	ag.OnError = "stop"
	r := &fakeRunner{outcomes: []Outcome{{Status: "harness_error", ExitCode: 7}}}
	e, as := newEngine(t, ag, r)
	if res := e.runOnce(context.Background(), "interval", ""); res != TickErrorHalt {
		t.Fatalf("res = %q, want error_halt", res)
	}
	got, _ := as.Get("smoke")
	if got.LoopEnabled {
		t.Fatal("on_error=stop must disable the loop")
	}
}

type detachedRunner struct{}

func (detachedRunner) Run(context.Context, agent.Agent, string, string, string) (Outcome, error) {
	return Outcome{}, ErrIterationDetached
}

func TestEngineDetachedIterationRemainsRunning(t *testing.T) {
	ag := baseAgent()
	ag.OnError = "stop"
	e, as := newEngine(t, ag, detachedRunner{})
	closeCalls := 0
	var terminalEvents int
	e.SetOnIterationClose(func(string, string) { closeCalls++ })
	e.SetEmit(func(event events.Event) {
		if event.Data["phase"] == "finish" {
			terminalEvents++
		}
	})

	if got := e.runOnce(context.Background(), "manual", ""); got != TickDetached {
		t.Fatalf("runOnce = %q, want detached", got)
	}
	iterations, err := as.ListIterations(ag.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(iterations) != 1 || iterations[0].Status != "running" || iterations[0].EndedAt != "" {
		t.Fatalf("detached iterations = %+v, want one unfinished running row", iterations)
	}
	if closeCalls != 0 {
		t.Fatalf("iteration-close calls = %d, want 0", closeCalls)
	}
	if terminalEvents != 0 {
		t.Fatalf("terminal events = %d, want 0", terminalEvents)
	}
	if got, err := as.Get(ag.Name); err != nil || !got.LoopEnabled {
		t.Fatalf("detached iteration applied on_error policy: agent=%+v err=%v", got, err)
	}
}

func TestRunOnceTimeoutStop(t *testing.T) {
	ag := baseAgent()
	ag.OnTimeout = "stop"
	r := &fakeRunner{outcomes: []Outcome{{Status: "timeout"}}}
	e, as := newEngine(t, ag, r)
	if res := e.runOnce(context.Background(), "interval", ""); res != TickTimeoutStop {
		t.Fatalf("res = %q", res)
	}
	got, _ := as.Get("smoke")
	if got.LoopEnabled {
		t.Fatal("on_timeout=stop must disable the loop")
	}
}

func TestRunOnceTimeoutRestart(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "timeout"}}}
	e, _ := newEngine(t, baseAgent(), r)
	if res := e.runOnce(context.Background(), "interval", ""); res != TickTimeoutRestart {
		t.Fatalf("res = %q", res)
	}
}

// idleOutcome is a done iteration self-declared idle (productive=false).
var idleOutcome = Outcome{Status: "done", DoneFlag: true, Productive: false}

// TestIdleAutostopDisabled: MaxIdleIterations==0 (default) never idle-stops,
// no matter how many idle iterations pile up.
func TestIdleAutostopDisabled(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{idleOutcome}}
	e, as := newEngine(t, baseAgent(), r) // MaxIdleIterations defaults 0
	for i := 0; i < 5; i++ {
		if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
			t.Fatalf("iter %d: res = %q, want completed_waiting", i, res)
		}
	}
	got, _ := as.Get("smoke")
	if !got.LoopEnabled {
		t.Fatal("MaxIdleIterations=0 must never disable the loop")
	}
}

// TestIdleAutostopFiresAtThreshold: with MaxIdleIterations=3, the loop keeps
// waiting for the first two idle iterations and cleanly stops exactly at the
// third — state=stopped (loop off, NO error_reason) with an idle_limit reason.
func TestIdleAutostopFiresAtThreshold(t *testing.T) {
	ag := baseAgent()
	ag.MaxIdleIterations = 3
	r := &fakeRunner{outcomes: []Outcome{idleOutcome}}
	e, as := newEngine(t, ag, r)

	if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
		t.Fatalf("iter 1: res = %q, want completed_waiting", res)
	}
	if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
		t.Fatalf("iter 2: res = %q, want completed_waiting", res)
	}
	if res := e.runOnce(context.Background(), "interval", ""); res != TickIdleStop {
		t.Fatalf("iter 3: res = %q, want idle_stop", res)
	}

	got, _ := as.Get("smoke")
	if got.LoopEnabled {
		t.Fatal("idle-stop must disable the loop")
	}
	if got.Enabled {
		t.Fatal("idle-stop must clear master enabled")
	}
	if got.ErrorReason != "" {
		t.Fatalf("idle-stop must be a clean stop, error_reason=%q", got.ErrorReason)
	}
	if !strings.Contains(got.StatusMessage, "idle_limit") {
		t.Fatalf("StatusMessage = %q, want it to contain idle_limit", got.StatusMessage)
	}
}

// TestIdleAutostopRestartGrantsFreshBudget: after an idle auto-stop at threshold
// N, a Start/restart (Store.StartResetIdle) must grant a fresh N-iteration budget
// — N-1 idle iterations must NOT re-stop, and the Nth stops with a reason whose
// count equals N (never > N), proving the streak is scoped past the restart
// boundary rather than re-counting the pre-restart idle history.
func TestIdleAutostopRestartGrantsFreshBudget(t *testing.T) {
	ag := baseAgent()
	ag.MaxIdleIterations = 3
	r := &fakeRunner{outcomes: []Outcome{idleOutcome}}
	e, as := newEngine(t, ag, r)

	// First idle streak: stop exactly at the 3rd.
	for i := 1; i <= 2; i++ {
		if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
			t.Fatalf("pre-restart iter %d: res = %q, want completed_waiting", i, res)
		}
	}
	if res := e.runOnce(context.Background(), "interval", ""); res != TickIdleStop {
		t.Fatalf("pre-restart iter 3: res = %q, want idle_stop", res)
	}

	// Restart: grant a fresh idle budget (what Manager.start does on Start).
	if err := as.StartResetIdle("smoke"); err != nil {
		t.Fatal(err)
	}

	// The first N-1 idle iterations after the restart must NOT re-stop.
	for i := 1; i <= 2; i++ {
		if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
			t.Fatalf("post-restart iter %d: res = %q, want completed_waiting (fresh budget)", i, res)
		}
	}
	// The Nth idle iteration after the restart stops again.
	if res := e.runOnce(context.Background(), "interval", ""); res != TickIdleStop {
		t.Fatalf("post-restart iter 3: res = %q, want idle_stop", res)
	}
	got, _ := as.Get("smoke")
	if got.StatusMessage != "idle_limit (3 idle iterations)" {
		t.Fatalf("StatusMessage = %q, want exactly \"idle_limit (3 idle iterations)\" (count == N, never > N)", got.StatusMessage)
	}
}

// TestIdleAutostopProductiveResets: a productive iteration in the middle resets
// the streak, so the count must reach the threshold again before stopping.
func TestIdleAutostopProductiveResets(t *testing.T) {
	ag := baseAgent()
	ag.MaxIdleIterations = 2
	// idle, idle-reset-by-productive, then idle, idle => stop on the 2nd idle run.
	r := &fakeRunner{outcomes: []Outcome{
		idleOutcome,
		{Status: "done", DoneFlag: true, Productive: true}, // productive: resets
		idleOutcome,
		idleOutcome,
	}}
	e, as := newEngine(t, ag, r)

	if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
		t.Fatalf("iter 1 (idle): res = %q", res)
	}
	if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
		t.Fatalf("iter 2 (productive): res = %q", res)
	}
	if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
		t.Fatalf("iter 3 (idle, streak=1): res = %q", res)
	}
	if res := e.runOnce(context.Background(), "interval", ""); res != TickIdleStop {
		t.Fatalf("iter 4 (idle, streak=2): res = %q, want idle_stop", res)
	}
	if got, _ := as.Get("smoke"); got.LoopEnabled {
		t.Fatal("streak should have reached threshold and stopped")
	}
}

// TestIdleAutostopTimeoutDoesNotCount: an abnormal iteration in the middle of an
// idle run must NOT count toward the streak (it defaults productive=1), so it
// resets the count rather than pushing it over the threshold.
func TestIdleAutostopTimeoutDoesNotCount(t *testing.T) {
	ag := baseAgent()
	ag.MaxIdleIterations = 3
	// idle, idle, timeout (restart), idle: streak after these is 1, below 3.
	r := &fakeRunner{outcomes: []Outcome{
		idleOutcome,
		idleOutcome,
		{Status: "timeout"}, // restart, productive defaults 1 => breaks streak
		idleOutcome,
	}}
	e, as := newEngine(t, ag, r)

	if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
		t.Fatalf("iter 1 (idle): res = %q", res)
	}
	if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
		t.Fatalf("iter 2 (idle): res = %q", res)
	}
	if res := e.runOnce(context.Background(), "interval", ""); res != TickTimeoutRestart {
		t.Fatalf("iter 3 (timeout): res = %q, want timeout_restart", res)
	}
	if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
		t.Fatalf("iter 4 (idle after break, streak=1): res = %q, want completed_waiting", res)
	}
	if got, _ := as.Get("smoke"); !got.LoopEnabled {
		t.Fatal("timeout in the middle must not push the streak over threshold")
	}
}

// TestRunOnceOnIterationCloseDone verifies the close hook fires exactly once,
// with the right (agent, iterationID), for a normal "done" outcome.
func TestRunOnceOnIterationCloseDone(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, _ := newEngine(t, baseAgent(), r)

	var calls []string
	e.SetOnIterationClose(func(agent, iterationID string) {
		calls = append(calls, agent+"/"+iterationID)
	})

	if res := e.runOnce(context.Background(), "interval", ""); res != TickCompletedWaiting {
		t.Fatalf("res = %q", res)
	}
	if len(calls) != 1 {
		t.Fatalf("onClose called %d times, want 1: %v", len(calls), calls)
	}
	if want := "smoke/" + r.seen[0]; calls[0] != want {
		t.Fatalf("onClose args = %q, want %q", calls[0], want)
	}
}

func TestRunOnceGoalIterationCompletedAfterFinalStatus(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, as := newEngine(t, baseAgent(), r)

	var calls []string
	e.SetIterationCompleted(func(agentName, iterationID string) {
		it, err := as.GetIteration(agentName, iterationID)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, agentName+"/"+iterationID+"/"+it.Status)
	})

	if got := e.runOnce(context.Background(), "interval", ""); got != TickCompletedWaiting {
		t.Fatalf("runOnce = %q", got)
	}
	if want := "smoke/" + r.seen[0] + "/done"; len(calls) != 1 || calls[0] != want {
		t.Fatalf("completion calls = %v, want [%s]", calls, want)
	}
}

func TestRunOnceRejectedTerminalStatusHasNoOutcomeSideEffects(t *testing.T) {
	raw, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	as := agent.NewStore(raw)
	ag := baseAgent()
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.DB.Exec(`CREATE TRIGGER reject_terminal_iteration_update
		BEFORE UPDATE OF status ON iterations
		WHEN OLD.status = 'running' AND NEW.status != 'running'
		BEGIN
			SELECT RAISE(FAIL, 'terminal write rejected');
		END`); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{outcomes: []Outcome{{Status: "harness_error", DoneFlag: true}}}
	clk := func() time.Time { return time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC) }
	ag.OnError = "stop"
	e := NewEngine(ag, as, r, slog.New(slog.NewTextHandler(io.Discard, nil)), clk)
	var phases, audits []string
	e.SetEmit(func(event events.Event) { phases = append(phases, event.Data["phase"].(string)) })
	e.SetAudit(func(typ, _, _ string, _ map[string]any) { audits = append(audits, typ) })
	closes, completions := 0, 0
	e.SetOnIterationClose(func(string, string) { closes++ })
	e.SetIterationCompleted(func(string, string) { completions++ })

	if got := e.runOnce(context.Background(), "manual", ""); got != TickSkipped {
		t.Fatalf("runOnce = %q, want skipped", got)
	}
	if len(phases) != 1 || phases[0] != "start" || len(audits) != 1 || audits[0] != "iteration_started" || closes != 1 || completions != 0 {
		t.Fatalf("phases/audits/closes/completions = %v/%v/%d/%d", phases, audits, closes, completions)
	}
	it, err := as.GetIteration(ag.Name, r.seen[0])
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "running" || it.DoneFlag {
		t.Fatalf("iteration after rejected terminal status = %#v", it)
	}
	stored, err := as.Get(ag.Name)
	if err != nil || !stored.LoopEnabled || stored.ErrorReason != "" {
		t.Fatalf("agent after rejected terminal status = %#v, %v", stored, err)
	}
}

// TestRunOnceOnIterationCloseHarnessErrorAndTimeout verifies the close hook
// fires exactly once for terminal non-done outcomes too: harness_error (via a
// runner error, the runErr early-return path in runOnce) and timeout (via
// applyPolicy's normal path).
func TestRunOnceOnIterationCloseHarnessErrorAndTimeout(t *testing.T) {
	t.Run("harness_error via runner error", func(t *testing.T) {
		r := &erroringRunner{}
		e, as := newEngine(t, baseAgent(), r)

		var calls []string
		e.SetOnIterationClose(func(agent, iterationID string) {
			calls = append(calls, agent+"/"+iterationID)
		})

		if res := e.runOnce(context.Background(), "interval", ""); res != TickError {
			t.Fatalf("res = %q, want tick_error", res)
		}
		its, _ := as.ListIterations("smoke")
		if len(its) != 1 || its[0].Status != "harness_error" {
			t.Fatalf("iteration = %+v", its)
		}
		if len(calls) != 1 {
			t.Fatalf("onClose called %d times, want 1: %v", len(calls), calls)
		}
		if want := "smoke/" + its[0].ID; calls[0] != want {
			t.Fatalf("onClose args = %q, want %q", calls[0], want)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		r := &fakeRunner{outcomes: []Outcome{{Status: "timeout"}}}
		e, _ := newEngine(t, baseAgent(), r)

		var calls []string
		e.SetOnIterationClose(func(agent, iterationID string) {
			calls = append(calls, agent+"/"+iterationID)
		})

		if res := e.runOnce(context.Background(), "interval", ""); res != TickTimeoutRestart {
			t.Fatalf("res = %q, want timeout_restart", res)
		}
		if len(calls) != 1 {
			t.Fatalf("onClose called %d times, want 1: %v", len(calls), calls)
		}
		if want := "smoke/" + r.seen[0]; calls[0] != want {
			t.Fatalf("onClose args = %q, want %q", calls[0], want)
		}
	})
}

// erroringRunner always returns a runner-level error (the runErr early-return
// path in runOnce, distinct from an Outcome{Status: "harness_error"}).
type erroringRunner struct{}

func (erroringRunner) Run(_ context.Context, _ agent.Agent, _, _, _ string) (Outcome, error) {
	return Outcome{}, io.ErrUnexpectedEOF
}

func TestRunLoopManualTrigger(t *testing.T) {
	ag := baseAgent()
	ag.LoopEnabled = false // pure manual
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, as := newEngine(t, ag, r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	e.Trigger("one-shot prompt")
	// Wait until the manual iteration is recorded.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if its, _ := as.ListIterations("smoke"); len(its) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	its, _ := as.ListIterations("smoke")
	if len(its) != 1 || its[0].Trigger != "manual" {
		t.Fatalf("manual iteration = %+v", its)
	}
}

// TestRunEventOnlyCreatesNoTimers verifies that an event-only agent
// (IntervalS <= 0) processes manual triggers without ever allocating a timer,
// which is what used to leak on every manual wakeup.
func TestRunEventOnlyCreatesNoTimers(t *testing.T) {
	ag := baseAgent()
	ag.IntervalS = 0 // event-only; loop stays enabled but has no interval cadence
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, as := newEngine(t, ag, r)
	f := &fakeTimerFactory{}
	e.newTimer = f.newTimer

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	for i := 0; i < 3; i++ {
		e.Trigger("")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if its, _ := as.ListIterations("smoke"); len(its) == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	its, _ := as.ListIterations("smoke")
	if len(its) != 3 {
		t.Fatalf("want 3 iterations, got %d", len(its))
	}
	if c := f.createdCount(); c != 0 {
		t.Fatalf("event-only agent created %d timers, want 0", c)
	}
}

// TestRunStopsTimerOnManualTrigger verifies that a timer created for an
// interval cycle is Stop()ed when a manual trigger wins the select, so the
// unfired timer is not abandoned.
func TestRunStopsTimerOnManualTrigger(t *testing.T) {
	ag := baseAgent()
	ag.IntervalS = 3600 // long interval so the timer never fires on its own
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, as := newEngine(t, ag, r)
	f := &fakeTimerFactory{}
	e.newTimer = f.newTimer

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	// Wait until Run has created the first interval timer before triggering.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.createdCount() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	first := f.lastTimer()
	if first == nil {
		t.Fatal("Run did not create an interval timer")
	}

	e.Trigger("")
	for time.Now().Before(deadline) {
		if its, _ := as.ListIterations("smoke"); len(its) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := atomic.LoadInt32(&first.stopped); got == 0 {
		t.Fatal("timer abandoned on manual trigger was not Stop()ed")
	}
	its, _ := as.ListIterations("smoke")
	if len(its) != 1 || its[0].Trigger != "manual" {
		t.Fatalf("manual iteration = %+v", its)
	}
}

func TestWakeStartsParkedEngine(t *testing.T) {
	// A disabled interval agent parks (no timer, blocked on select). A later
	// enable + Wake(WakeStart) must run an iteration.
	ag := baseAgent()
	ag.LoopEnabled = false
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, as := newEngine(t, ag, r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	// Nothing should run while parked.
	time.Sleep(50 * time.Millisecond)
	if its, _ := as.ListIterations("smoke"); len(its) != 0 {
		t.Fatalf("parked engine ran %d iterations", len(its))
	}
	// Enable in the store, then wake.
	got, _ := as.Get("smoke")
	got.LoopEnabled = true
	got.IntervalS = 60
	if err := as.Update(got); err != nil {
		t.Fatal(err)
	}
	e.Wake(WakeStart)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if its, _ := as.ListIterations("smoke"); len(its) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if its, _ := as.ListIterations("smoke"); len(its) < 1 {
		t.Fatal("Wake(WakeStart) did not wake the parked engine")
	}
}

func TestIntervalTimerReloadsPersistedPauseBeforeRunning(t *testing.T) {
	ag := baseAgent()
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, as := newEngine(t, ag, r)
	factory := &fakeTimerFactory{}
	e.newTimer = factory.newTimer

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	waitForTimerCount(t, factory, 1)

	stored, err := as.Get(ag.Name)
	if err != nil {
		t.Fatal(err)
	}
	stored.LoopEnabled = false
	if err := as.Update(stored); err != nil {
		t.Fatal(err)
	}
	factory.lastTimer().ch <- time.Now()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if iterations, err := as.ListIterations(ag.Name); err != nil {
		t.Fatal(err)
	} else if len(iterations) != 0 {
		t.Fatalf("paused timer started iterations: %+v", iterations)
	}
}

func TestWakeMessageRunsIterationWhenPending(t *testing.T) {
	ag := baseAgent()
	ag.LoopEnabled = true
	ag.IntervalS = 0 // event-only
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, as := newEngine(t, ag, r)

	pending := true
	e.SetMessagePeek(func() (bool, error) { return pending, nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	e.Wake(WakeMessage)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if its, _ := as.ListIterations("smoke"); len(its) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	its, _ := as.ListIterations("smoke")
	if len(its) != 1 || its[0].Trigger != "message" {
		t.Fatalf("message-triggered iteration = %+v", its)
	}
}

func TestWakeMessageNoopWhenEmpty(t *testing.T) {
	ag := baseAgent()
	ag.IntervalS = 0
	r := &fakeRunner{outcomes: []Outcome{{Status: "done"}}}
	e, as := newEngine(t, ag, r)
	e.SetMessagePeek(func() (bool, error) { return false, nil }) // nothing pending

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	e.Wake(WakeMessage)
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done
	if its, _ := as.ListIterations("smoke"); len(its) != 0 {
		t.Fatalf("no-pending wake ran an iteration: %+v", its)
	}
}

type collisionRunner struct {
	fakeRunner
	blocked atomic.Bool
	checked chan struct{}
}

func newCollisionRunner() *collisionRunner {
	r := &collisionRunner{
		fakeRunner: fakeRunner{
			outcomes: []Outcome{{Status: "done", DoneFlag: true}},
		},
		checked: make(chan struct{}, 1),
	}
	r.blocked.Store(true)
	return r
}

func (r *collisionRunner) SessionBlocked(agent.Agent) bool {
	blocked := r.blocked.Load()
	if blocked {
		select {
		case r.checked <- struct{}{}:
		default:
		}
	}
	return blocked
}

func waitForTimerCount(t *testing.T, factory *fakeTimerFactory, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if factory.createdCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("created timers = %d, want at least %d", factory.createdCount(), want)
}

func TestWakeMessageRetriesAfterInteractiveSessionCloses(t *testing.T) {
	ag := baseAgent()
	ag.Interactive = true
	ag.IntervalS = 0
	r := newCollisionRunner()
	e, as := newEngine(t, ag, r)
	e.SetMessagePeek(func() (bool, error) { return true, nil })
	factory := &fakeTimerFactory{}
	e.newTimer = factory.newTimer

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	e.Wake(WakeMessage)
	waitForTimerCount(t, factory, 1)
	if got := factory.lastDelay(); got != time.Second {
		t.Fatalf("message retry delay = %v, want 1s", got)
	}
	if its, _ := as.ListIterations("smoke"); len(its) != 0 {
		t.Fatalf("session-blocked wake created iteration rows: %+v", its)
	}

	r.blocked.Store(false)
	factory.lastTimer().ch <- time.Now()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if its, _ := as.ListIterations("smoke"); len(its) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	its, _ := as.ListIterations("smoke")
	if len(its) != 1 || its[0].Trigger != "message" {
		t.Fatalf("message retry iterations = %+v, want one message iteration", its)
	}
}

func TestWakeMessageCoalescesBeforeDrain(t *testing.T) {
	e, _ := newEngine(t, baseAgent(), &fakeRunner{
		outcomes: []Outcome{{Status: "done", DoneFlag: true}},
	})
	for range 9 {
		e.Wake(WakeMessage)
	}
	if got := len(e.messageCh); got != 1 {
		t.Fatalf("queued message wakes = %d, want one coalesced generation", got)
	}
}

func TestRunOnceDoesNotStartAfterContextCancellation(t *testing.T) {
	e, as := newEngine(t, baseAgent(), &fakeRunner{
		outcomes: []Outcome{{Status: "done", DoneFlag: true}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := e.runOnce(ctx, "message", ""); result != TickDisabled {
		t.Fatalf("cancelled run result = %q, want disabled", result)
	}
	if its, _ := as.ListIterations("smoke"); len(its) != 0 {
		t.Fatalf("cancelled run created iterations: %+v", its)
	}
}

type pausingRunner struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (r *pausingRunner) Run(
	_ context.Context, _ agent.Agent, _, _, _ string,
) (Outcome, error) {
	r.calls.Add(1)
	r.started <- struct{}{}
	<-r.release
	return Outcome{Status: "done", DoneFlag: true}, nil
}

func TestWakeMessageDuringRunQueuesOneFutureGeneration(t *testing.T) {
	ag := baseAgent()
	ag.IntervalS = 0
	r := &pausingRunner{
		started: make(chan struct{}, 2),
		release: make(chan struct{}, 2),
	}
	e, _ := newEngine(t, ag, r)
	e.SetMessagePeek(func() (bool, error) { return true, nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	e.Wake(WakeMessage)
	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first message iteration did not start")
	}
	for range 5 {
		e.Wake(WakeMessage)
	}
	r.release <- struct{}{}
	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("publish during message iteration was lost")
	}
	if got := r.calls.Load(); got != 2 {
		t.Fatalf("runner calls = %d, want two coalesced generations", got)
	}
	r.release <- struct{}{}
	cancel()
	<-done
}

func TestWakeMessageDuringRunSurvivesFullEventQueue(t *testing.T) {
	ag := baseAgent()
	ag.IntervalS = 0
	r := &pausingRunner{
		started: make(chan struct{}, 2),
		release: make(chan struct{}, 2),
	}
	e, _ := newEngine(t, ag, r)
	e.SetMessagePeek(func() (bool, error) { return true, nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	e.Wake(WakeMessage)
	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first message iteration did not start")
	}

	for range cap(e.events) {
		e.Wake(WakeConfig)
	}
	if got := len(e.events); got != cap(e.events) {
		t.Fatalf("event queue length = %d, want full capacity %d", got, cap(e.events))
	}
	e.Wake(WakeMessage)
	r.release <- struct{}{}

	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("deferred message generation was lost behind a full event queue")
	}
	if got := r.calls.Load(); got != 2 {
		t.Fatalf("runner calls = %d, want two message generations", got)
	}
	r.release <- struct{}{}
	cancel()
	<-done
}

func TestWakeMessageRetryCoalescesPublishesAndReparksOnConfig(t *testing.T) {
	ag := baseAgent()
	ag.Interactive = true
	ag.IntervalS = 0
	r := newCollisionRunner()
	e, as := newEngine(t, ag, r)
	e.SetMessagePeek(func() (bool, error) { return true, nil })
	factory := &fakeTimerFactory{}
	e.newTimer = factory.newTimer

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	e.Wake(WakeMessage)
	waitForTimerCount(t, factory, 1)
	first := factory.lastTimer()
	for range 8 {
		e.Wake(WakeMessage)
	}
	time.Sleep(50 * time.Millisecond)
	if got := factory.createdCount(); got != 1 {
		t.Fatalf("coalesced publishes created %d retry timers, want 1", got)
	}

	e.Wake(WakeConfig)
	waitForTimerCount(t, factory, 2)
	if atomic.LoadInt32(&first.stopped) == 0 {
		t.Fatal("config wake did not stop the old retry timer")
	}
	if its, _ := as.ListIterations("smoke"); len(its) != 0 {
		t.Fatalf("blocked retry created iteration rows: %+v", its)
	}

	cancel()
	<-done
	if atomic.LoadInt32(&factory.lastTimer().stopped) == 0 {
		t.Fatal("context cancellation did not stop the parked retry timer")
	}
}

func TestWakeMessageRetryCancelsWhenStoppedOrPendingClears(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		ag := baseAgent()
		ag.Interactive = true
		ag.IntervalS = 0
		r := newCollisionRunner()
		e, as := newEngine(t, ag, r)
		e.SetMessagePeek(func() (bool, error) { return true, nil })
		factory := &fakeTimerFactory{}
		e.newTimer = factory.newTimer

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { e.Run(ctx); close(done) }()
		e.Wake(WakeMessage)
		waitForTimerCount(t, factory, 1)
		retry := factory.lastTimer()

		stored, _ := as.Get("smoke")
		stored.LoopEnabled = false
		if err := as.Update(stored); err != nil {
			t.Fatal(err)
		}
		e.Wake(WakeStop)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) && atomic.LoadInt32(&retry.stopped) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		if atomic.LoadInt32(&retry.stopped) == 0 {
			t.Fatal("stop wake left message retry armed")
		}
		if got := factory.createdCount(); got != 1 {
			t.Fatalf("stop wake created %d timers, want no replacement", got)
		}
		cancel()
		<-done
	})

	t.Run("pending cleared", func(t *testing.T) {
		ag := baseAgent()
		ag.Interactive = true
		ag.IntervalS = 0
		r := newCollisionRunner()
		e, as := newEngine(t, ag, r)
		var pending atomic.Bool
		pending.Store(true)
		e.SetMessagePeek(func() (bool, error) { return pending.Load(), nil })
		factory := &fakeTimerFactory{}
		e.newTimer = factory.newTimer

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { e.Run(ctx); close(done) }()
		e.Wake(WakeMessage)
		waitForTimerCount(t, factory, 1)

		pending.Store(false)
		r.blocked.Store(false)
		factory.lastTimer().ch <- time.Now()
		time.Sleep(50 * time.Millisecond)
		cancel()
		<-done
		if r.calls != 0 {
			t.Fatalf("cleared pending state launched runner %d times", r.calls)
		}
		if its, _ := as.ListIterations("smoke"); len(its) != 0 {
			t.Fatalf("cleared pending state created iterations: %+v", its)
		}
		if got := factory.createdCount(); got != 1 {
			t.Fatalf("cleared pending state rearmed retry: timers=%d", got)
		}
	})
}

func TestRunOnceEmitsRootSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(tracenoop.NewTracerProvider()) })

	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true, CPUMs: 5, MemPeakKB: 9}}}
	e, _ := newEngine(t, baseAgent(), r)
	e.runOnce(context.Background(), "manual", "")

	spans := sr.Ended()
	var root bool
	for _, s := range spans {
		if s.Name() == "iteration" {
			root = true
			var haveAgent, haveOutcome bool
			for _, kv := range s.Attributes() {
				if string(kv.Key) == "agent" {
					haveAgent = true
				}
				if string(kv.Key) == "outcome" {
					haveOutcome = true
				}
			}
			if !haveAgent || !haveOutcome {
				t.Fatalf("root span missing attrs: agent=%v outcome=%v", haveAgent, haveOutcome)
			}
		}
	}
	if !root {
		t.Fatalf("no root 'iteration' span among %d spans", len(spans))
	}
}

func TestRunOnceFlushesUsageBeforeTerminalSpanLookup(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(tracenoop.NewTracerProvider()) })

	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, _ := newEngine(t, baseAgent(), r)
	flushed := false
	e.usageFlush = func() { flushed = true }
	e.usageLookup = func(string) (int, int, float64) {
		if !flushed {
			t.Fatal("usage lookup ran before terminal flush")
		}
		return 123, 45, 0.67
	}

	e.runOnce(context.Background(), "manual", "")

	for _, s := range sr.Ended() {
		if s.Name() != "iteration" {
			continue
		}
		got := map[string]float64{}
		for _, kv := range s.Attributes() {
			switch string(kv.Key) {
			case "tokens_in", "tokens_out":
				got[string(kv.Key)] = float64(kv.Value.AsInt64())
			case "cost_usd":
				got[string(kv.Key)] = kv.Value.AsFloat64()
			}
		}
		if got["tokens_in"] != 123 || got["tokens_out"] != 45 || got["cost_usd"] != 0.67 {
			t.Fatalf("terminal usage attributes = %v", got)
		}
		return
	}
	t.Fatal("iteration span not found")
}

// TestRunOnceNoSpanWhenTelemetryOff verifies iteration behavior is unchanged
// when telemetry is off (noop tracer): the iteration still completes and is
// persisted with the correct outcome, and no spans are recorded.
func TestRunOnceNoSpanWhenTelemetryOff(t *testing.T) {
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	sr := tracetest.NewSpanRecorder()
	t.Cleanup(func() { otel.SetTracerProvider(tracenoop.NewTracerProvider()) })

	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true, CPUMs: 5, MemPeakKB: 9}}}
	e, as := newEngine(t, baseAgent(), r)
	if res := e.runOnce(context.Background(), "manual", ""); res != TickCompletedWaiting {
		t.Fatalf("res = %q, want completed_waiting", res)
	}
	its, _ := as.ListIterations("smoke")
	if len(its) != 1 || its[0].Status != "done" || !its[0].DoneFlag {
		t.Fatalf("iteration = %+v", its)
	}
	if n := len(sr.Ended()); n != 0 {
		t.Fatalf("telemetry off recorded %d spans, want 0", n)
	}
}

func TestWakeIsNonBlocking(t *testing.T) {
	// Wake must never block even if no one is draining the channel.
	e, _ := newEngine(t, baseAgent(), &fakeRunner{outcomes: []Outcome{{Status: "done"}}})
	for i := 0; i < 100; i++ {
		e.Wake(WakeStop) // buffered + default: cannot deadlock
	}
}

// blockingRunner is a fakeRunner that reports its agent's tmux session is alive.
type blockingRunner struct{ fakeRunner }

func (b *blockingRunner) SessionBlocked(agent.Agent) bool { return true }

func TestRunOnceManualFailsOnSessionConflict(t *testing.T) {
	r := &blockingRunner{}
	ag := baseAgent()
	ag.Interactive = true
	e, as := newEngine(t, ag, r)
	var events []string
	e.SetAudit(func(typ, _, _ string, data map[string]any) {
		events = append(events, typ+":"+asStr(data["reason"]))
	})
	res := e.runOnce(context.Background(), "manual", "")
	if res != TickError {
		t.Fatalf("res = %q, want tick_error", res)
	}
	if r.calls != 0 {
		t.Fatalf("runner.Run called %d times, want 0", r.calls)
	}
	its, _ := as.ListIterations("smoke")
	if len(its) != 1 || its[0].Status != "failed" {
		t.Fatalf("iteration = %+v, want one failed row", its)
	}
	if len(events) != 1 || events[0] != "iteration_failed:tmux_session_exists" {
		t.Fatalf("audit = %v", events)
	}
}

func TestRunOnceManualSessionConflictSignalsGoalAfterFailedRow(t *testing.T) {
	r := &blockingRunner{}
	ag := baseAgent()
	ag.Interactive = true
	e, as := newEngine(t, ag, r)
	var calls []string
	e.SetIterationCompleted(func(agentName, iterationID string) {
		it, err := as.GetIteration(agentName, iterationID)
		if err != nil {
			calls = append(calls, "lookup error: "+err.Error())
			return
		}
		calls = append(calls, agentName+"/"+iterationID+"/"+it.Status)
	})

	if got := e.runOnce(context.Background(), "manual", ""); got != TickError {
		t.Fatalf("runOnce = %q, want tick_error", got)
	}
	its, err := as.ListIterations(ag.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 1 {
		t.Fatalf("iterations = %+v, want one failed row", its)
	}
	want := ag.Name + "/" + its[0].ID + "/failed"
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("completion calls = %v, want [%s]", calls, want)
	}
}

func TestRunOnceLoopSkipsOnSessionConflict(t *testing.T) {
	r := &blockingRunner{}
	ag := baseAgent()
	ag.Interactive = true
	e, as := newEngine(t, ag, r)
	var events []string
	e.SetAudit(func(typ, _, _ string, data map[string]any) {
		events = append(events, typ+":"+asStr(data["reason"]))
	})
	res := e.runOnce(context.Background(), "interval", "")
	if res != TickSkipped {
		t.Fatalf("res = %q, want skipped", res)
	}
	its, _ := as.ListIterations("smoke")
	if len(its) != 0 {
		t.Fatalf("loop skip must create no iteration, got %+v", its)
	}
	if len(events) != 1 || events[0] != "iteration_skipped:session_alive" {
		t.Fatalf("audit = %v", events)
	}
}

// reapableRunner reports a live session but self-heals it as an orphan, so the
// engine should reap-and-launch instead of failing the iteration.
type reapableRunner struct {
	fakeRunner
	reaps int
}

func (r *reapableRunner) SessionBlocked(agent.Agent) bool  { return true }
func (r *reapableRunner) ReapOrphanBlock(agent.Agent) bool { r.reaps++; return true }

func TestRunOnceReapsOrphanThenLaunches(t *testing.T) {
	r := &reapableRunner{fakeRunner: fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}}
	ag := baseAgent()
	ag.Interactive = true
	e, as := newEngine(t, ag, r)
	var events []string
	e.SetAudit(func(typ, _, _ string, data map[string]any) {
		events = append(events, typ+":"+asStr(data["reason"]))
	})
	res := e.runOnce(context.Background(), "manual", "")
	if r.reaps != 1 {
		t.Fatalf("ReapOrphanBlock called %d times, want 1", r.reaps)
	}
	if r.calls != 1 {
		t.Fatalf("runner.Run called %d times, want 1 (launch proceeded after reap)", r.calls)
	}
	if res == TickError {
		t.Fatalf("res = %q, want a launched tick, not tick_error", res)
	}
	its, _ := as.ListIterations("smoke")
	if len(its) != 1 || its[0].Status == "failed" {
		t.Fatalf("iteration = %+v, want one launched (non-failed) row", its)
	}
	if len(events) == 0 || events[0] != "session_reaped:orphan_before_launch" {
		t.Fatalf("audit = %v, want session_reaped first", events)
	}
}

func TestRunOnceRecordsLifecycleAudit(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, _ := newEngine(t, baseAgent(), r)
	var types []string
	e.SetAudit(func(typ, _, _ string, _ map[string]any) { types = append(types, typ) })
	e.runOnce(context.Background(), "interval", "")
	if len(types) != 2 || types[0] != "iteration_started" || types[1] != "iteration_finished" {
		t.Fatalf("lifecycle audit = %v", types)
	}
}

func asStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestEngineSkipsIterationWhenDisabled verifies the interval gate requires
// the master Enabled flag, not just LoopEnabled: a disabled agent must not
// fire any interval iteration even when LoopEnabled=true and IntervalS>0.
// Mirrors TestWakeStartsParkedEngine's "parked engine" shape (real timer,
// real sleep, assert ListIterations stays empty), but parks via Enabled=false
// instead of LoopEnabled=false.
func TestEngineSkipsIterationWhenDisabled(t *testing.T) {
	ag := baseAgent()
	ag.Enabled = false
	ag.LoopEnabled = true
	ag.IntervalS = 1 // short enough to fire well within the test's wait window
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	e, as := newEngine(t, ag, r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	// Wait comfortably past the 1s interval; a disabled agent must never
	// launch an interval iteration, no matter how long we wait here.
	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	its, _ := as.ListIterations("smoke")
	if len(its) != 0 {
		t.Fatalf("disabled agent ran %d iteration(s), want 0: %+v", len(its), its)
	}
	if r.calls != 0 {
		t.Fatalf("runner.Run called %d times for disabled agent, want 0", r.calls)
	}
}

func TestDisableLoopSetsError(t *testing.T) {
	ag := agent.Agent{Name: "eh", ImageRef: "i:latest", OnError: "stop", LoopEnabled: true}
	e, st := newEngine(t, ag, &fakeRunner{outcomes: []Outcome{{Status: "harness_error"}}})
	e.applyPolicy(Outcome{Status: "harness_error"})
	a, err := st.Get("eh")
	if err != nil {
		t.Fatal(err)
	}
	if a.ErrorReason == "" || a.LoopEnabled {
		t.Fatalf("after error-stop halt: reason=%q loop=%v, want set/false", a.ErrorReason, a.LoopEnabled)
	}
}

// TestRunOnceBeforeLaunchErrorSurfacesReason pins the operator-visible outcome
// of a pre-launch failure (unverifiable image manifest, missing image skills,
// unusable harness): the loop halts with the real cause in error_reason and one
// iteration_failed audit event, instead of failing silently every tick.
func TestRunOnceBeforeLaunchErrorSurfacesReason(t *testing.T) {
	e, as := newEngine(t, baseAgent(), &fakeRunner{})
	e.SetBeforeLaunch(func(*agent.Agent) (activatedImage, error) {
		return activatedImage{}, errors.New("image manifest verify: digest mismatch")
	})
	var events []string
	e.SetAudit(func(typ, _, _ string, data map[string]any) {
		events = append(events, typ+":"+asStr(data["reason"])+":"+asStr(data["error"]))
	})

	if res := e.runOnce(context.Background(), "interval", ""); res != TickErrorHalt {
		t.Fatalf("res = %q, want error_halt", res)
	}
	got, err := as.Get("smoke")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.ErrorReason, "image manifest verify: digest mismatch") {
		t.Fatalf("error_reason = %q, want the activation error text", got.ErrorReason)
	}
	if got.LoopEnabled {
		t.Fatal("a pre-launch failure must halt the loop instead of repeating every tick")
	}
	want := "iteration_failed:image_activation_failed:image manifest verify: digest mismatch"
	if len(events) != 1 || events[0] != want {
		t.Fatalf("audit = %v, want [%s]", events, want)
	}
	if its, _ := as.ListIterations("smoke"); len(its) != 0 {
		t.Fatalf("iterations = %+v, want none (the iteration never started)", its)
	}
}

func TestRunOnceRecordsHarnessErrorReason(t *testing.T) {
	for name, r := range map[string]IterationRunner{
		"runner error": erroringRunner{},
		"harness exit": &fakeRunner{outcomes: []Outcome{{Status: "harness_error", ExitCode: 3, Error: "harness exited with code 3"}}},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := newEngine(t, baseAgent(), r)
			var finished map[string]any
			e.SetAudit(func(typ, _, _ string, data map[string]any) {
				if typ == "iteration_finished" {
					finished = data
				}
			})
			e.runOnce(context.Background(), "manual", "")
			if finished == nil || finished["status"] != "harness_error" || asStr(finished["error"]) == "" {
				t.Fatalf("iteration_finished = %v, want harness_error with a reason", finished)
			}
		})
	}
}
