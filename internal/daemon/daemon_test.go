package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/agentdir"
	"github.com/alekzonder/tariboy/internal/audit"
	"github.com/alekzonder/tariboy/internal/bus"
	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/paths"
	"github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/telemetry"
)

func TestTasksRuntimeAliasRefreshAndOwnership(t *testing.T) {
	runtimeDir := t.TempDir()
	for _, value := range []string{"old", "new"} {
		bundle := t.TempDir()
		payload := filepath.Join(bundle, "tariboy-tasks")
		if err := os.WriteFile(payload, []byte("#!/bin/sh\nprintf '"+value+"\\n'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		binDir, err := installTasksAlias(bundle, runtimeDir)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(filepath.Join(binDir, "ttasks")).Output()
		if err != nil || strings.TrimSpace(string(out)) != value {
			t.Fatalf("alias output = %q, err = %v", out, err)
		}
	}
	for _, kind := range []string{"file", "foreign-link", "directory"} {
		t.Run(kind, func(t *testing.T) {
			bundle, runtimeDir := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(bundle, "tariboy-tasks"), []byte("#!/bin/sh\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			binDir := filepath.Join(runtimeDir, "bin")
			if err := os.Mkdir(binDir, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(binDir, "ttasks")
			switch kind {
			case "file":
				if err := os.WriteFile(link, []byte("user-owned"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign-link":
				if err := os.Symlink("/bin/sh", link); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(link, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := installTasksAlias(bundle, runtimeDir); err == nil {
				t.Fatal("occupied runtime command was replaced")
			}
			if kind == "foreign-link" {
				if target, err := os.Readlink(link); err != nil || target != "/bin/sh" {
					t.Fatalf("foreign target = %q, err = %v", target, err)
				}
			}
		})
	}
}

type fakeObservationReconciler struct{ calls chan struct{} }

func (f *fakeObservationReconciler) ReconcileWorkflowObservations(context.Context, int) (int, error) {
	select {
	case f.calls <- struct{}{}:
	default:
	}
	return 1, nil
}

func TestWorkflowObservationReconcilerRunsAtStartupPeriodicallyAndStops(t *testing.T) {
	fake := &fakeObservationReconciler{calls: make(chan struct{}, 4)}
	signal := newWorkflowIngressSignal()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runWorkflowObservationReconciler(ctx, fake, signal.C(), 5*time.Millisecond, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
		close(done)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-fake.calls:
		case <-time.After(time.Second):
			t.Fatalf("reconcile call %d did not arrive", i+1)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observation reconciler did not stop")
	}
}

func TestWorkflowIngressSignalProcessesPromptlyAndCoalescesWithoutBlockingPublish(t *testing.T) {
	fake := &fakeObservationReconciler{calls: make(chan struct{}, 8)}
	signal := newWorkflowIngressSignal()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runWorkflowObservationReconciler(ctx, fake, signal.C(), time.Hour, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
		close(done)
	}()
	select {
	case <-fake.calls:
	case <-time.After(time.Second):
		t.Fatal("startup reconcile missing")
	}
	for range 1000 {
		signal.Signal()
	}
	select {
	case <-fake.calls:
	case <-time.After(time.Second):
		t.Fatal("signaled reconcile was not prompt")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("signaled reconciler leaked on shutdown")
	}
}

type fakeDispatcher struct {
	calls chan struct{}
	err   error
}

func (f *fakeDispatcher) DispatchPending(context.Context) (int, error) {
	select {
	case f.calls <- struct{}{}:
	default:
	}
	return 0, f.err
}

func TestWorkflowDispatcherRunsAtStartupOnSignalAndPeriodicallyAndStops(t *testing.T) {
	var logs bytes.Buffer
	fake := &fakeDispatcher{calls: make(chan struct{}, 8), err: context.DeadlineExceeded}
	signal := newWorkflowIngressSignal()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runWorkflowDispatcher(ctx, fake, signal.C(), time.Hour, slog.New(slog.NewTextHandler(&logs, nil)))
		close(done)
	}()
	wait := func(what string) {
		t.Helper()
		select {
		case <-fake.calls:
		case <-time.After(time.Second):
			t.Fatalf("%s dispatch did not arrive", what)
		}
	}
	wait("startup")
	signal.Signal()
	wait("signaled")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not stop")
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "workflow dispatch") {
		t.Fatalf("dispatch error was not logged at warn: %s", logs.String())
	}

	periodic := &fakeDispatcher{calls: make(chan struct{}, 8)}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go runWorkflowDispatcher(ctx, periodic, make(chan struct{}), 5*time.Millisecond, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	for i := 0; i < 3; i++ {
		select {
		case <-periodic.calls:
		case <-time.After(time.Second):
			t.Fatalf("periodic dispatch %d did not arrive", i+1)
		}
	}
}

func TestOrdinaryPublishDoesNotSynchronouslyWriteWorkflowStateOrChangeLegacyDelivery(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "publish.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	agents := agent.NewStore(st)
	if err := agents.Create(agent.Agent{Name: "legacy"}); err != nil {
		t.Fatal(err)
	}
	b := bus.New(st, time.Now)
	if _, err := b.Subscribe("legacy", "chat:legacy", nil, nil); err != nil {
		t.Fatal(err)
	}
	signal := newWorkflowIngressSignal()
	taskService := tasks.NewService(st.DB, "customer", time.Now)
	b.SetPublishHook(func(bus.Message, []string) {
		if taskService.WorkflowIngressEnabled() {
			signal.Signal()
		}
	})
	first, err := b.Publish(bus.Message{Channel: "chat:legacy", Source: "operator", Type: "note", Text: "unchanged legacy payload"})
	if err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	go func() {
		_, err := b.Publish(bus.Message{Channel: "chat:legacy", Source: "operator", Type: "note", Text: "second"})
		returned <- err
	}()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("publish blocked on full workflow signal")
	}
	if _, err := taskService.ReconcileWorkflowObservations(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signal.C():
		t.Fatal("ordinary publish signaled workflow ingress without targets")
	default:
	}
	var created int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("ordinary publish created %d tasks", created)
	}
	pending, err := b.Pending("legacy", 10)
	if err != nil || len(pending) != 2 || pending[0].ID != first.ID || pending[0].Text != "unchanged legacy payload" {
		t.Fatalf("legacy pending = %#v, %v", pending, err)
	}
}

func TestTaskMutationSignalsGoalReconciler(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	service := tasks.NewService(st.DB, "customer", time.Now)
	actor := tasks.CustomerActor("customer")
	if _, err := service.CreateQueue(context.Background(), actor, tasks.CreateQueueInput{Prefix: "GOAL", Name: "Goals"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	service.SetGoalSignal(func() { calls++ })

	if _, err := service.CreateTask(context.Background(), actor, tasks.CreateTaskInput{
		Queue: "GOAL", Title: "Wake worker", Assignee: "agent:worker",
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("goal signals = %d, want 1", calls)
	}
}

// Removing daemon-start reconciliation must make this fail: the legacy agent
// would still have no durable delivery lane after restart.
func TestReconcileAgentInboxesRepairsExistingAgentsIdempotently(t *testing.T) {
	base := t.TempDir()
	st, err := store.Open(filepath.Join(base, "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	agents := agent.NewStore(st)
	for _, name := range []string{"legacy", "already"} {
		if err := agents.Create(agent.Agent{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	channelBus := bus.New(st, time.Now)
	if _, err := channelBus.Subscribe("already", bus.InboxChannel("already"), bus.Matcher{}, nil); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := reconcileAgentInboxes(agents, channelBus); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"legacy", "already"} {
		subs, err := channelBus.ListSubscriptions(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(subs) != 1 || subs[0].Channel != bus.InboxChannel(name) || len(subs[0].Matcher) != 0 || len(subs[0].TypeFilter) != 0 {
			t.Fatalf("subscriptions for %s = %+v, want one unfiltered own inbox", name, subs)
		}
	}
}

func TestRecordEventAgentScopedGoesToAuditFile(t *testing.T) {
	agentsDir := t.TempDir()
	reg := audit.NewRegistry(
		func(a string) string { return agentdir.New(agentsDir, a).AuditLog() },
		func() time.Time { return time.Unix(0, 0).UTC() },
	)
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	recordEvent(log, nil, reg, nil, "manager", "shim", `{"line":"boom"}`)

	evs, err := audit.ReadEvents(agentdir.New(agentsDir, "manager").AuditLog(), 0, 0)
	if err != nil || len(evs) != 1 {
		t.Fatalf("read = %d evs err=%v", len(evs), err)
	}
	if evs[0].Type != "shim" || evs[0].Data["line"] != "boom" {
		t.Fatalf("event = %+v", evs[0])
	}
}

// TestTelemetryNoopSetupShutdown pins the daemon's default observability path:
// with no OTLP endpoint, telemetry.Setup installs no-op providers (Enabled ==
// false) and Shutdown is a clean no-op. This is the path exercised by every
// daemon boot in these tests (no OTEL_EXPORTER_OTLP_ENDPOINT is set), so a
// non-nil logger is passed as Run does — Setup may log.Warn with no nil guard.
func TestTelemetryNoopSetupShutdown(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	tel, err := telemetry.Setup(context.Background(),
		telemetry.Config{Endpoint: "", ServiceName: "tariboy"}, log)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if tel.Enabled() {
		t.Fatal("providers must be disabled with no endpoint")
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown no-op must be clean: %v", err)
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	base := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, daemonTestOptions(Options{BaseDir: base, Listen: "unix", LogLevel: "error"})) }()

	sock := paths.New(base).Socket()
	c := client.New(sock)
	var raw json.RawMessage
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		raw, err = c.Call("GET", "/api/daemon/status", nil)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("daemon never came up: %v", err)
	}
	var st map[string]any
	json.Unmarshal(raw, &st)
	if st["base_dir"] != base {
		t.Fatalf("status base_dir = %v", st["base_dir"])
	}
	images := &image.Store{Dir: paths.New(base).ImagesDir()}
	if !images.Exists(image.BareRef) || images.Exists(image.Ref{Name: "basic", Tag: "latest"}) {
		t.Fatalf("fresh daemon images: bare=%v basic=%v", images.Exists(image.BareRef), images.Exists(image.Ref{Name: "basic", Tag: "latest"}))
	}
	if _, err := os.Stat(filepath.Join(base, "store", "versions")); !os.IsNotExist(err) {
		t.Fatalf("fresh daemon created versioned Store: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

func TestRunTaskGoalInitialScanUsesPublishHookAndStops(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TARIBOY_RUNTIME_DIR", t.TempDir())
	stubHarness := filepath.Join(t.TempDir(), "stub-harness")
	if err := os.WriteFile(stubHarness, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TARIBOY_STUB_HARNESS", stubHarness)
	imagesDir := filepath.Join(base, "images")
	buildBasicImage(t, imagesDir)
	manifest, err := (&image.Store{Dir: imagesDir}).Inspect(image.Ref{Name: "basic", Tag: "latest"})
	if err != nil {
		t.Fatal(err)
	}

	seed, err := store.Open(filepath.Join(base, "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	worker := agent.Agent{
		Name: "worker", ImageRef: "basic:latest", ImageDigest: manifest.Digest, HarnessType: "stub", Cwd: t.TempDir(),
		Enabled: true, LoopEnabled: true, GoalEnabled: true, Plugins: []string{"context"},
	}
	if err := agent.NewStore(seed).Create(worker); err != nil {
		t.Fatal(err)
	}
	if err := agentdir.Provision(
		agentdir.New(filepath.Join(base, "agents"), worker.Name), worker,
		&image.Store{Dir: imagesDir}, image.Ref{Name: "basic", Tag: "latest"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.DB.Exec(`INSERT INTO task_queues(prefix,name,created_at,updated_at)
		VALUES ('GOAL','Goals','2020-01-01T00:00:00Z','2020-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.DB.Exec(`INSERT INTO tasks(
		task_key,queue_prefix,title,status,author,customer,assignee,created_at,updated_at)
		VALUES ('GOAL-9xk2','GOAL','Goal','open','user:customer','user:customer','agent:worker',
		'2020-01-01T00:00:00Z','2020-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	spawner := &captureSpawner{fired: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, daemonTestOptions(Options{
			BaseDir: base, Listen: "unix", HTTPAddr: "", LogLevel: "error", Spawner: spawner,
		}))
	}()

	select {
	case <-spawner.fired:
		// interval_s=0 leaves the ordinary bus publish hook as the only route
		// that can wake this agent and reach the injected spawner.
	case err := <-done:
		cancel()
		t.Fatalf("daemon returned before reminder wake: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("daemon did not stop after reminder wake timeout")
		}
		t.Fatal("task goal initial scan did not publish through the wake hook")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not await task reminder shutdown")
	}

	reopened, err := store.Open(filepath.Join(base, "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var channel, typ, source, data, idempotencyKey string
	if err := reopened.DB.QueryRow(`SELECT channel,type,source,data,idempotency_key
		FROM messages WHERE idempotency_key='task-goal:worker:GOAL-9xk2:1:'`).Scan(&channel, &typ, &source, &data, &idempotencyKey); err != nil {
		t.Fatal(err)
	}
	if channel != "chat:service:worker" || typ != "task.goal" || source != "tasks" || idempotencyKey != "task-goal:worker:GOAL-9xk2:1:" {
		t.Fatalf("goal = channel:%q type:%q source:%q idempotency:%q", channel, typ, source, idempotencyKey)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["reason"] != "selected" || payload["task_key"] != "GOAL-9xk2" {
		t.Fatalf("payload = %#v", payload)
	}
	var deliveries int
	if err := reopened.DB.QueryRow(`SELECT COUNT(*) FROM deliveries d
		JOIN messages m ON m.id=d.message_id WHERE m.type='task.goal'`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Fatalf("durable deliveries = %d, want one unprocessed Goal delivery", deliveries)
	}
}

func TestFreshDaemonStartsWithNoInstalledPlugins(t *testing.T) {
	base := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, daemonTestOptions(Options{BaseDir: base, Listen: "unix", LogLevel: "error"}))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not shut down")
		}
	})

	c := client.New(paths.New(base).Socket())
	deadline := time.Now().Add(15 * time.Second)
	var raw json.RawMessage
	var err error
	for time.Now().Before(deadline) {
		raw, err = c.Call("GET", "/api/plugins", nil)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("daemon never came up: %v", err)
	}
	var listing struct {
		Plugins []map[string]any `json:"plugins"`
		Count   int              `json:"count"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Count != 0 || len(listing.Plugins) != 0 {
		t.Fatalf("fresh plugin list = %+v", listing)
	}
}

func TestRunRejectsPublicTCPWithoutAuth(t *testing.T) {
	err := Run(context.Background(), daemonTestOptions(Options{BaseDir: t.TempDir(), Listen: "tcp:0.0.0.0:0", LogLevel: "error"}))
	if err == nil {
		t.Fatal("public tcp without auth must fail")
	}
}

func TestRecordEventLogsOnError(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close() // subsequent AddEvent must fail (database is closed)
	var buf bytes.Buffer
	recordEvent(slog.New(slog.NewTextHandler(&buf, nil)), s, nil, nil, "", "daemon_start", "{}")
	if !strings.Contains(buf.String(), "failed to record daemon event") ||
		!strings.Contains(buf.String(), "kind=daemon_start") {
		t.Fatalf("warn not logged: %q", buf.String())
	}
}

func TestSecondDaemonRefusesWhenSocketLive(t *testing.T) {
	base := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, daemonTestOptions(Options{BaseDir: base, Listen: "unix", LogLevel: "error"})) }()

	p := paths.New(base)
	ready := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("unix", p.Socket(), 200*time.Millisecond); err == nil {
			c.Close()
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("first daemon never became ready")
	}
	if _, err := os.Stat(p.PidFile()); err != nil {
		t.Fatalf("pidfile missing while running: %v", err)
	}

	err := Run(ctx, daemonTestOptions(Options{BaseDir: base, Listen: "unix", LogLevel: "error"}))
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second Run err = %v, want 'already running'", err)
	}

	cancel()
	<-done
	if _, err := os.Stat(p.PidFile()); !os.IsNotExist(err) {
		t.Fatalf("pidfile should be removed after shutdown, stat err = %v", err)
	}
}
