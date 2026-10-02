package loop

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/agentapi"
	"github.com/alekzonder/tariboy/internal/agentdir"
	"github.com/alekzonder/tariboy/internal/audit"
	"github.com/alekzonder/tariboy/internal/bus"
	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/events"
	"github.com/alekzonder/tariboy/internal/groups"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/imagefile"
	"github.com/alekzonder/tariboy/internal/plugincaps"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/schedule"
	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/scriptnotify"
	"github.com/alekzonder/tariboy/internal/shim"
	"github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
)

func TestManagerPassesCurrentGoalToRunner(t *testing.T) {
	want := tasks.Task{Key: "TARI-43"}
	m := &Manager{cfg: ManagerConfig{
		CurrentGoal: func(agentName string, _ time.Time) (tasks.Task, bool, error) {
			if agentName != "alice" {
				t.Fatalf("agent = %q", agentName)
			}
			return want, true, nil
		},
	}}
	runner := m.runnerFor(agent.Agent{}).(*ShimRunner)
	got, ok, err := runner.cfg.CurrentGoal("alice", time.Time{})
	if err != nil || !ok || got != want {
		t.Fatalf("CurrentGoal = %#v, %v, %v", got, ok, err)
	}
}

func TestShutdownDoesNotHoldManagerLockWhileDrainingHTTP(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := agentapi.NewServer(agentapi.Deps{
		Agent: "worker", Plugins: []string{"status"},
		Status: func() (map[string]any, error) {
			close(entered)
			<-release
			return map[string]any{"state": "idle"}, nil
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeListener(ln) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/tools/status")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	m := &Manager{
		cfg:      ManagerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		runs:     map[string]*runtime{},
		toolsAPI: map[string]*agentapi.Server{"worker": srv},
	}
	shutdownDone := make(chan struct{})
	go func() {
		m.Shutdown()
		close(shutdownDone)
	}()
	time.Sleep(20 * time.Millisecond)
	activeDone := make(chan struct{})
	go func() {
		_ = m.ActiveAgents()
		close(activeDone)
	}()
	select {
	case <-activeDone:
	case <-time.After(200 * time.Millisecond):
		close(release)
		t.Fatal("manager lock remained held while HTTP shutdown drained")
	}
	close(release)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("manager shutdown did not finish")
	}
}

func startScriptTestSupervisor(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx, m.stop = ctx, cancel
	m.startScriptSupervisor(ctx)
	t.Cleanup(m.Shutdown)
}

func awaitScriptRun(t *testing.T, st *script.Store, agentName, id string, want func(script.Run) bool) script.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, err := st.GetRun(agentName, id)
		if err == nil && want(r) {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, err := st.GetRun(agentName, id)
	t.Fatalf("script %s did not reach expected state: %#v %v", id, r, err)
	return script.Run{}
}

func TestScriptSupervisorKeepsOutputInLogAndPublishesPath(t *testing.T) {
	m, as, agentsDir, raw := newManager(t, &fakeRunner{})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeAgentsDir, err := filepath.Rel(cwd, agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.AgentsDir = relativeAgentsDir
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts, m.cfg.Bus = st, bus.New(raw, time.Now)
	workdir := t.TempDir()
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: workdir}); err != nil {
		t.Fatal(err)
	}
	startScriptTestSupervisor(t, m)
	_, r, err := m.RunOnce("worker", script.CreateOnce{
		Name: "ok", Description: "test",
		Command: `awk 'BEGIN { for (i = 0; i < 70000; i++) printf "o" }'; awk 'BEGIN { for (i = 0; i < 70000; i++) printf "e" }' >&2`,
	})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitScriptRun(t, st, "worker", r.ID, func(r script.Run) bool { return r.Status == script.RunSucceeded })
	got, err := os.ReadFile(r.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	header := "cwd: " + workdir + "\n"
	if !strings.HasPrefix(string(got), header) {
		t.Fatalf("log does not start with resolved CWD: %q", got[:min(len(got), len(header))])
	}
	if len(got) != len(header)+140000 || got[len(header)] != 'o' || got[len(got)-1] != 'e' {
		t.Fatalf("log size=%d first=%q last=%q", len(got), got[0], got[len(got)-1])
	}
	publisher := scriptnotify.New(raw.DB, m.cfg.Bus, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := publisher.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	messages, err := m.cfg.Bus.MessagesSince(bus.ChatChannelFor(bus.ChatIDService("worker")), "", 10)
	if err != nil || len(messages) != 1 || messages[0].Type != "script.result" {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	msg := messages[0]
	if len(msg.Text) >= 1024 || !strings.Contains(msg.Text, r.LogPath) {
		t.Fatalf("message text does not contain a concise log reference: len=%d contains_path=%t", len(msg.Text), strings.Contains(msg.Text, r.LogPath))
	}
	if _, ok := msg.Data["output"]; ok {
		t.Fatalf("message embeds script output: data keys=%v", msg.Data)
	}
	if msg.Data["log_path"] != r.LogPath {
		t.Fatalf("message log_path=%v, want %q", msg.Data["log_path"], r.LogPath)
	}
	if len(msg.Data) != 7 || msg.Data["run_id"] != r.ID || msg.Data["name"] != "ok" || msg.Data["exit_code"] != float64(0) {
		t.Fatalf("message data=%#v, want script/run/name/status/mode/exit_code/log_path", msg.Data)
	}
	if !filepath.IsAbs(r.LogPath) {
		t.Fatalf("log_path=%q, want absolute path", r.LogPath)
	}
	if !strings.HasPrefix(r.LogPath, filepath.Join(agentsDir, "worker", "scripts")) {
		t.Fatalf("log path %q is outside agent record directory", r.LogPath)
	}
}

func TestScriptSupervisorDeprecatedQuietExitTwoStaysQuiet(t *testing.T) {
	m, as, _, raw := newManager(t, &fakeRunner{})
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts, m.cfg.Bus = st, bus.New(raw, time.Now)
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	startScriptTestSupervisor(t, m)
	quietExit := 2
	definition, r, err := m.ScheduleScript("worker", script.CreateSchedule{Name: "quiet", Description: "test", Command: "exit 2", IntervalSeconds: 60, QuietExit: &quietExit})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitScriptRun(t, st, "worker", r.ID, func(r script.Run) bool { return r.Status == script.RunFailed })
	if r.ExitCode == nil || *r.ExitCode != script.QuietExit {
		t.Fatalf("run=%#v, want the legacy exit 2 reported as %d", r, script.QuietExit)
	}
	definition, err = st.GetDefinition("worker", definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if definition.NextRunAt == "" {
		t.Fatal("recurring exit 2 did not schedule a next run")
	}
	var outboxCount int
	if err := raw.DB.QueryRow(`SELECT COUNT(*) FROM script_result_outbox WHERE run_id=?`, r.ID).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("quiet outbox count=%d err=%v", outboxCount, err)
	}
	messages, err := m.cfg.Bus.MessagesSince(bus.ChatChannelFor(bus.ChatIDService("worker")), "", 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("quiet result messages=%#v err=%v", messages, err)
	}
}

func TestScriptSupervisorExportsTheQuietExitCode(t *testing.T) {
	m, as, _, raw := newManager(t, &fakeRunner{})
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts, m.cfg.Bus = st, bus.New(raw, time.Now)
	// The agent's own value must not replace the protocol constant.
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: t.TempDir(), Env: map[string]string{"TARIBOY_QUIET_EXIT": "5"}}); err != nil {
		t.Fatal(err)
	}
	startScriptTestSupervisor(t, m)
	definition, r, err := m.ScheduleScript("worker", script.CreateSchedule{Name: "quiet", Description: "test", Command: `exit "$TARIBOY_QUIET_EXIT"`, IntervalSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitScriptRun(t, st, "worker", r.ID, func(r script.Run) bool { return r.Status == script.RunFailed })
	if r.ExitCode == nil || *r.ExitCode != script.QuietExit {
		t.Fatalf("run=%#v, want exit %d from $TARIBOY_QUIET_EXIT", r, script.QuietExit)
	}
	definition, err = st.GetDefinition("worker", definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if definition.State != script.StateActive || definition.NextRunAt == "" {
		t.Fatalf("quiet run stopped its schedule: %#v", definition)
	}
	var outboxCount int
	if err := raw.DB.QueryRow(`SELECT COUNT(*) FROM script_result_outbox WHERE run_id=?`, r.ID).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("quiet outbox count=%d err=%v", outboxCount, err)
	}
}

func TestScriptSupervisorMakeExitTwoCreatesFailureResult(t *testing.T) {
	m, as, _, raw := newManager(t, &fakeRunner{})
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts, m.cfg.Bus = st, bus.New(raw, time.Now)
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "Makefile"), []byte("all:\n\t@exit 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: workdir}); err != nil {
		t.Fatal(err)
	}
	startScriptTestSupervisor(t, m)
	_, run, err := m.RunOnce("worker", script.CreateOnce{Name: "make-check", Description: "regression", Command: "make"})
	if err != nil {
		t.Fatal(err)
	}
	run = awaitScriptRun(t, st, "worker", run.ID, func(run script.Run) bool { return run.Status == script.RunFailed })
	if run.ExitCode == nil || *run.ExitCode != 2 {
		t.Fatalf("run=%#v, want failed exit 2", run)
	}
	publisher := scriptnotify.New(raw.DB, m.cfg.Bus, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := publisher.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	messages, err := m.cfg.Bus.MessagesSince(bus.ChatChannelFor(bus.ChatIDService("worker")), "", 10)
	if err != nil || len(messages) != 1 || messages[0].Type != "script.result" || messages[0].Data["exit_code"] != float64(2) {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
}

func TestFinishScriptDoesNotPublishResultWithoutLogPath(t *testing.T) {
	m, as, _, raw := newManager(t, &fakeRunner{})
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts, m.cfg.Bus = st, bus.New(raw, time.Now)
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	_, r, err := st.CreateOnce("worker", script.CreateOnce{Name: "broken", Description: "test", Command: "true"})
	if err != nil {
		t.Fatal(err)
	}

	m.finishScript(r.ID, r, -1, errors.New("log path unavailable"), "")

	messages, err := m.cfg.Bus.MessagesSince(bus.ChatChannelFor(bus.ChatIDService("worker")), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("published result without log path: %#v", messages)
	}
}

func TestScriptSupervisorRecoveryAndCancel(t *testing.T) {
	m, as, _, raw := newManager(t, &fakeRunner{})
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts = st
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	staleDefinition, stale, err := st.CreateSchedule("worker", script.CreateSchedule{Name: "stale", Description: "test", Command: "true", IntervalSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := st.ClaimRun("worker", stale.ID, time.Now().UTC().Format(time.RFC3339), "/tmp/stale.log"); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if set, err := st.SetRunPID("worker", stale.ID, 1234); err != nil || !set {
		t.Fatalf("pid set=%v err=%v", set, err)
	}
	if err := st.RecoverRunning(); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetRun("worker", stale.ID); got.Status != script.RunInterrupted {
		t.Fatalf("recovered run=%#v", got)
	}
	if got, _ := st.GetDefinition("worker", staleDefinition.ID); got.State != script.StateCompleted || got.NextRunAt != "" {
		t.Fatalf("recovered definition=%#v", got)
	}
	startScriptTestSupervisor(t, m)
	_, r, err := m.RunOnce("worker", script.CreateOnce{Name: "long", Description: "test", Command: "sleep 30 & wait"})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitScriptRun(t, st, "worker", r.ID, func(r script.Run) bool { return r.Status == script.RunRunning && r.PID != nil })
	if err := m.CancelScriptTarget("worker", r.ID); err != nil {
		t.Fatal(err)
	}
	r = awaitScriptRun(t, st, "worker", r.ID, func(r script.Run) bool { return r.Status == script.RunCancelled })
	if r.LogPath == "" {
		t.Fatal("cancelled run lost its log path")
	}
}

func TestCancelRecurringRunStopsItsDefinitionAfterProcessExit(t *testing.T) {
	oldGrace := scriptCancelGrace
	scriptCancelGrace = 200 * time.Millisecond
	t.Cleanup(func() { scriptCancelGrace = oldGrace })

	m, as, _, raw := newManager(t, &fakeRunner{})
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts = st
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	startScriptTestSupervisor(t, m)
	definition, run, err := m.ScheduleScript("worker", script.CreateSchedule{
		Name: "stubborn", Description: "ignore term",
		Command: "trap '' TERM; echo started; while :; do sleep 1; done", IntervalSeconds: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	run = awaitScriptRun(t, st, "worker", run.ID, func(run script.Run) bool {
		return run.Status == script.RunRunning && run.PID != nil
	})
	if err := m.CancelScriptTarget("worker", run.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.CancelScriptTarget("worker", run.ID); err != nil {
		t.Fatalf("repeated running cancellation: %v", err)
	}
	current, err := st.GetRun("worker", run.ID)
	if err != nil || current.Status != script.RunRunning {
		t.Fatalf("run became terminal before process exit: %#v err=%v", current, err)
	}
	currentDefinition, err := st.GetDefinition("worker", definition.ID)
	if err != nil || currentDefinition.NextRunAt != "" {
		t.Fatalf("successor scheduled before process exit: %#v err=%v", currentDefinition, err)
	}
	awaitScriptRun(t, st, "worker", run.ID, func(run script.Run) bool { return run.Status == script.RunCancelled })
	currentDefinition, err = st.GetDefinition("worker", definition.ID)
	if err != nil || currentDefinition.State != script.StateCompleted || currentDefinition.NextRunAt != "" {
		t.Fatalf("cancelled run kept its schedule: %#v err=%v", currentDefinition, err)
	}
}

func TestScriptLogRejectsSymlink(t *testing.T) {
	m, as, agentsDir, raw := newManager(t, &fakeRunner{})
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts = st
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	_, run, err := st.CreateOnce("worker", script.CreateOnce{Name: "done", Description: "done", Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(agentdir.New(agentsDir, "worker").Root, "scripts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, run.ID+".log")
	if err := os.Symlink(outside, logPath); err != nil {
		t.Fatal(err)
	}
	if claimed, err := st.ClaimRun("worker", run.ID, time.Now().UTC().Format(time.RFC3339), logPath); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	exit := 0
	if _, err := st.CompleteRun("worker", run.ID, script.Completion{Status: script.RunSucceeded, ExitCode: &exit, FinishedAt: time.Now().UTC().Format(time.RFC3339), LogPath: logPath}); err != nil {
		t.Fatal(err)
	}
	if log, err := m.LogScriptRun("worker", run.ID); err == nil {
		t.Fatalf("symlink log was followed: %q", log)
	}
}

func TestRemoveScriptRejectsActiveRecordsAndRemovesTerminalRecords(t *testing.T) {
	m, as, _, raw := newManager(t, &fakeRunner{})
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts = st
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	active, _, err := st.CreateOnce("worker", script.CreateOnce{Name: "active", Description: "test", Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveScript("worker", active.ID); !errors.Is(err, script.ErrActive) {
		t.Fatalf("RemoveScript(active) error = %v, want ErrActive", err)
	}
	if _, err := st.GetDefinition("worker", active.ID); err != nil {
		t.Fatalf("active record was removed: %v", err)
	}

	terminal, terminalRun, err := st.CreateOnce("worker", script.CreateOnce{Name: "terminal", Description: "test", Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(agentdir.New(m.cfg.AgentsDir, "worker").Root, "scripts", terminalRun.ID+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("finished"), 0o600); err != nil {
		t.Fatal(err)
	}
	if claimed, err := st.ClaimRun("worker", terminalRun.ID, time.Now().UTC().Format(time.RFC3339), logPath); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	exit := 0
	if _, err := st.CompleteRun("worker", terminalRun.ID, script.Completion{Status: script.RunSucceeded, ExitCode: &exit, FinishedAt: time.Now().UTC().Format(time.RFC3339), LogPath: logPath}); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveScript("worker", terminal.ID); err != nil {
		t.Fatalf("RemoveScript(terminal): %v", err)
	}
	if _, err := st.GetDefinition("worker", terminal.ID); !errors.Is(err, script.ErrNotFound) {
		t.Fatalf("terminal record still exists: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("terminal log was not removed: %v", err)
	}
}

func TestStartScriptDoesNotLaunchCanceledPendingRecord(t *testing.T) {
	m, as, _, raw := newManager(t, &fakeRunner{})
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts = st
	workdir := t.TempDir()
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: workdir}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workdir, "started")
	_, r, err := st.CreateOnce("worker", script.CreateOnce{Name: "race", Description: "test", Command: "touch " + marker})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CancelRun("worker", r.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	m.startScript(context.Background(), agent.Agent{Name: "worker", Cwd: workdir}, r)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("canceled pending record started command: stat err=%v", err)
	}
	got, err := st.GetRun("worker", r.ID)
	if err != nil || got.Status != script.RunCancelled {
		t.Fatalf("record after canceled start attempt = %#v, %v", got, err)
	}
}

func buildBasic(t *testing.T, st *image.Store) {
	buildTestImage(t, st, "basic", "BODY")
}

func buildTestImage(t *testing.T, st *image.Store, name, body string) {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "task.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	skills := testSkillsDir(t)
	if _, err := image.BuildV2(&imagefile.V2{
		SchemaVersion: 2, Dir: src,
		Plugins: []imagefile.V2Plugin{{Name: "whoami"}, {Name: "loop"}, {Name: "messages"}, {Name: "context"}},
		Prompts: []imagefile.PromptEntry{{File: "./task.md"}},
		Skills: []imagefile.SkillEntry{
			{Dir: filepath.Join(skills, "whoami")},
			{Dir: filepath.Join(skills, "loop")},
			{Dir: filepath.Join(skills, "messages")},
			{Dir: filepath.Join(skills, "context")},
			{Dir: filepath.Join(skills, "tasks")},
		},
	}, imagefile.ResolveRoots{}, image.Ref{Name: name, Tag: "latest"}, st, time.Now, nil); err != nil {
		t.Fatal(err)
	}
}

func buildContractInvalidImage(t *testing.T, st *image.Store, name string) {
	t.Helper()
	ref := image.Ref{Name: name, Tag: "latest"}
	if _, err := image.BuildV2(&imagefile.V2{SchemaVersion: 2, Dir: t.TempDir()}, imagefile.ResolveRoots{}, ref, st, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	archive, err := st.ArchiveBytes(ref)
	if err != nil {
		t.Fatal(err)
	}
	in, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	tr := tar.NewReader(in)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		copyHeader := *header
		if header.Name == "manifest.json" {
			var manifest map[string]any
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest["plugins"] = []map[string]string{{"name": "loop"}}
			body, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			copyHeader.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&copyHeader); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InstallArchive(ref, out.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func pinBasicImage(t *testing.T, m *Manager, ag *agent.Agent) {
	t.Helper()
	manifest, err := m.cfg.ImgStore.Inspect(image.Ref{Name: "basic", Tag: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	ag.ImageDigest = manifest.Digest
}

// TestStartFailsLoudlyWhenToolsSocketUnbindable locks in the Defect B fix: a
// per-agent tools-socket bind failure must fail the start with a real error and
// leave the agent in "error", not silently launch the loop without a control
// plane (which previously made every iteration end in no_i_am_done forever).
func TestStartFailsLoudlyWhenToolsSocketUnbindable(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, _, _ := newManager(t, r)
	// Sockets would land in a directory that does not exist, so net.Listen fails.
	m.cfg.RuntimeDir = filepath.Join(t.TempDir(), "does", "not", "exist")
	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "smoke",
		Harness: "stub", Plugins: []string{"context"}, Loop: true}); err != nil {
		t.Fatalf("run: unexpected error creating disabled agent: %v", err)
	}
	// Run() creates the agent disabled (Task 4); the master switch (LiveState
	// gate, Task 2) wins over a stale error_reason, so enable it here to
	// exercise the "error" LiveState this test locks in (flipping Enabled on
	// Start is Task 5's job, not this test's concern).
	got, err := as.Get("smoke")
	if err != nil {
		t.Fatal(err)
	}
	got.Enabled = true
	if err := as.Update(got); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("smoke"); err == nil {
		t.Fatal("expected a loud error when the tools socket cannot bind")
	}
	if got, _ := as.Get("smoke"); got.ErrorReason == "" {
		t.Fatalf("agent error_reason = %q, want non-empty", got.ErrorReason)
	}
	if st, _ := m.LiveState("smoke"); st != "error" {
		t.Fatalf("LiveState = %q, want error", st)
	}
}

func newManager(t *testing.T, r IterationRunner) (*Manager, *agent.Store, string, *store.Store) {
	t.Helper()
	t.Setenv("TARIBOY_STUB_HARNESS", "/usr/bin/true")
	base := t.TempDir()
	s, err := store.Open(filepath.Join(base, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	as := agent.NewStore(s)
	imgStore := &image.Store{Dir: filepath.Join(base, "images")}
	buildBasic(t, imgStore)
	m := NewManager(ManagerConfig{
		AgentsDir: filepath.Join(base, "agents"),
		ShimBin:   "/opt/tariboy-shim", ImgStore: imgStore, Store: as,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: time.Now,
		Bus:           bus.New(s, time.Now),
		RunnerFactory: func(agent.Agent) IterationRunner { return r },
	})
	return m, as, filepath.Join(base, "agents"), s
}

func testSkillsDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"whoami", "loop", "messages", "context", "tasks"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: Test " + name + " capability.\n---\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		"loop/scripts/loop.sh",
		"tasks/scripts/tasks.sh",
	} {
		path = filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/usr/bin/env python3\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRefreshLoopConfigSignalsGoalReconciler(t *testing.T) {
	m, _, _, _ := newManager(t, &fakeRunner{})
	calls := 0
	m.cfg.GoalSignal = func() { calls++ }

	m.RefreshLoopConfig("worker")

	if calls != 1 {
		t.Fatalf("goal signals = %d, want 1", calls)
	}
}

func TestStartSignalsGoalReconcilerWhileIterationIsAdopting(t *testing.T) {
	m, as, _, _ := newManager(t, &fakeRunner{})
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest"}); err != nil {
		t.Fatal(err)
	}
	m.adopting["worker"] = agentdir.LiveIteration{Agent: "worker", ID: "iter-7"}
	calls := 0
	m.cfg.GoalSignal = func() { calls++ }

	if err := m.Start("worker"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("goal signals = %d, want 1", calls)
	}
}

func TestSetLoopEnabledSignalsGoalReconcilerAfterPersistence(t *testing.T) {
	m, as, _, _ := newManager(t, &fakeRunner{})
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest"}); err != nil {
		t.Fatal(err)
	}
	var enabled []bool
	m.cfg.GoalSignal = func() {
		ag, err := as.Get("worker")
		if err == nil {
			enabled = append(enabled, ag.LoopEnabled)
		}
	}

	if err := m.SetLoopEnabled("worker", true); err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 1 || !enabled[0] {
		t.Fatalf("loop-enabled states at goal signals = %v, want [true]", enabled)
	}
}

func TestRestartSignalsGoalReconcilerAfterReenableDuringAdoption(t *testing.T) {
	m, as, _, _ := newManager(t, &fakeRunner{})
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m.adopting["worker"] = agentdir.LiveIteration{Agent: "worker", ID: "iter-7"}
	var enabled []bool
	m.cfg.GoalSignal = func() {
		ag, err := as.Get("worker")
		if err == nil {
			enabled = append(enabled, ag.Enabled)
		}
	}

	if err := m.Restart("worker"); err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 2 || enabled[0] || !enabled[1] {
		t.Fatalf("enabled states at goal signals = %v, want [false true]", enabled)
	}
}

// Removing the own-inbox subscription from Manager.Run must make this fail:
// a standalone agent would persist successfully but task publication would
// create no delivery for it.
func TestRunSubscribesStandaloneAgentToOwnInbox(t *testing.T) {
	m, _, _, raw := newManager(t, &fakeRunner{})
	bs := bus.New(raw, time.Now)
	m.cfg.Bus = bs

	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "solo", Loop: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bs.Publish(bus.Message{
		Channel: bus.InboxChannel(name), Type: "task.assigned", Text: "wake",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := bs.Pending(name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "task.assigned" {
		t.Fatalf("pending = %+v, want one task.assigned delivery", got)
	}
}

func TestRunDoesNotInjectRetiredCapabilityEnvironment(t *testing.T) {
	m, as, _, _ := newManager(t, &fakeRunner{})
	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "solo", Loop: false}); err != nil {
		t.Fatal(err)
	}
	ag, err := as.Get("solo")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ag.Env["BE"+"ADS_DIR"]; ok {
		t.Fatalf("retired environment variable was injected: %v", ag.Env)
	}
}

// Catches Manager.Run accepting a complete create request but silently
// rebuilding a mostly-default Agent before the one-row persistence boundary.
func TestRunPersistsCompleteConfiguration(t *testing.T) {
	m, store, _, _ := newManager(t, &fakeRunner{})
	cwd := t.TempDir()
	spec := registry.RunSpec{
		ImageRef: "basic:latest", Name: "clone", Cwd: cwd,
		Harness: "codex", Model: "gpt-5", Effort: "high",
		Interactive: true, Loop: false,
		IntervalS: 12, TimeoutS: 34, HardTimeoutS: 56,
		OnTimeout: "stop", OnError: "restart", MaxIdleIterations: 7, AIStallTimeoutS: 420,
		UserPrompt: "standing prompt", Env: map[string]string{"CSV": "a,b"},
		Plugins: []string{"context"}, MessagesBatch: 8, MessagesMaxQueue: 900,
		Alias: "Clone", Notes: "all fields", Color: "#123abc",
	}
	enabled := false
	spec.GoalEnabled = &enabled
	spec.GoalWaitCustomerTimeoutS = 120
	_, err := m.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("clone")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cwd != cwd || got.HarnessType != "codex" || got.Model != "gpt-5" ||
		got.Effort != "high" || !got.Interactive || got.LoopEnabled || got.Enabled ||
		got.IntervalS != 12 || got.TimeoutS != 34 || got.HardTimeoutS != 56 ||
		got.OnTimeout != "stop" || got.OnError != "restart" || got.MaxIdleIterations != 7 || got.AIStallTimeoutS != 420 ||
		got.UserPrompt != "standing prompt" || got.Env["CSV"] != "a,b" ||
		strings.Join(got.Plugins, ",") != "whoami,loop,messages,context" ||
		got.MessagesBatch != 8 || got.MessagesMaxQueue != 900 ||
		got.Alias != "Clone" || got.Notes != "all fields" || got.Color != "#123abc" ||
		got.GoalEnabled || got.GoalWaitCustomerTimeoutS != 120 {
		t.Fatalf("persisted agent = %#v", got)
	}
}

type killBlockingRunner struct {
	started chan string
	// release, when non-nil, holds the runner inside Run after its context is
	// cancelled until the test closes it. That keeps the cancelled iteration
	// open, so a test can exercise the stale-kill recovery path without racing
	// the engine goroutine that unwinds it. Most users of this runner leave it
	// unset, and receiving from a nil channel blocks forever, so Run must skip
	// the receive entirely when it is nil.
	release           chan struct{}
	returnAfterCancel *Outcome
}

func (r *killBlockingRunner) Run(ctx context.Context, _ agent.Agent, _ string, id string, _ string) (Outcome, error) {
	r.started <- id
	<-ctx.Done()
	if r.release != nil {
		<-r.release
	}
	// ShimRunner can still find and classify result.json after cancellation.
	// Model that normal return so stale-kill recovery races the real terminal
	// result path instead of only the simpler ctx.Err path.
	if r.returnAfterCancel != nil {
		return *r.returnAfterCancel, nil
	}
	return Outcome{}, ctx.Err()
}

func awaitCurrentIteration(t *testing.T, m *Manager, name string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		rt := m.runs[name]
		m.mu.Unlock()
		if rt != nil {
			if id := rt.engine.CurrentIterationID(); id != "" {
				return id
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agent %q did not start an iteration", name)
	return ""
}

type killRecordingShim struct{ killed chan struct{} }

func (s killRecordingShim) Status() shim.StatusResult          { return shim.StatusResult{Running: true} }
func (s killRecordingShim) Kill() error                        { close(s.killed); return nil }
func (s killRecordingShim) Screen() (string, error)            { return "", nil }
func (s killRecordingShim) SendKeys(shim.SendKeysParams) error { return nil }
func (s killRecordingShim) Report() shim.ReportResult          { return shim.ReportResult{} }

func TestManagerKillForwardsToLiveShim(t *testing.T) {
	r := &killBlockingRunner{started: make(chan string, 1)}
	m, _, agentsDir, _ := newManager(t, r)
	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "smoke", Harness: "stub", Plugins: []string{"context"}, Loop: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	if _, err := m.Exec("smoke", ""); err != nil {
		t.Fatal(err)
	}
	awaitCurrentIteration(t, m, "smoke")
	l := agentdir.New(agentsDir, "smoke").WithRuntime(m.cfg.RuntimeDir)
	ln, err := net.Listen("unix", l.ShimSock())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	killed := make(chan struct{})
	go func() { _ = shim.Serve(ln, killRecordingShim{killed: killed}) }()
	if err := m.Kill("smoke"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("live shim did not receive kill RPC")
	}
}

// attachRecordingShim is a fake shim that supports the StreamHandler
// extension (Attach/Resize) alongside the base Handler methods, so
// Manager.Attach/Manager.Resize can be exercised end-to-end over a real
// unix socket.
type attachRecordingShim struct {
	resized chan shim.ResizeParams
	killed  chan struct{}
	sent    chan string
}

func (s attachRecordingShim) Status() shim.StatusResult { return shim.StatusResult{Running: true} }
func (s attachRecordingShim) Screen() (string, error)   { return "", nil }
func (s attachRecordingShim) SendKeys(p shim.SendKeysParams) error {
	if s.sent != nil {
		select {
		case s.sent <- p.Keys:
		default:
		}
	}
	return nil
}
func (s attachRecordingShim) Report() shim.ReportResult { return shim.ReportResult{} }

func (s attachRecordingShim) Kill() error {
	if s.killed != nil {
		select {
		case s.killed <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s attachRecordingShim) Attach(conn net.Conn, _ shim.AttachParams) error {
	defer conn.Close()
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		return err
	}
	_, err := conn.Write(buf)
	return err
}

func (s attachRecordingShim) Resize(p shim.ResizeParams) error {
	s.resized <- p
	return nil
}

func startPendingAdoption(
	t *testing.T,
	m *Manager,
	as *agent.Store,
	agentsDir string,
	handler attachRecordingShim,
) string {
	t.Helper()
	ag := agent.Agent{
		Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub",
		Enabled: true, LoopEnabled: true, Interactive: true,
		Plugins: []string{"context"},
	}
	pinBasicImage(t, m, &ag)
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, ag.Name).WithRuntime(m.cfg.RuntimeDir)
	id := "smoke-20260731100000-1"
	if err := l.EnsureIteration(id); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", l.ShimSock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = shim.Serve(ln, handler) }()
	if err := as.CreateIteration(agent.Iteration{
		ID: id, Agent: ag.Name, Trigger: "manual", Status: "running",
		StartedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	return id
}

func TestManagerAttachForwardsToAdoptedShim(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{outcomes: []Outcome{{Status: "done"}}})
	startPendingAdoption(t, m, as, agentsDir, attachRecordingShim{})

	conn, err := m.Attach("smoke", 80, 24)
	if err != nil {
		t.Fatalf("Attach adopted shim: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("write adopted shim: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read adopted shim: %v", err)
	}
	if string(buf) != "x" {
		t.Fatalf("adopted attach round-trip = %q, want x", buf)
	}
}

func TestManagerKillForwardsToAdoptedShim(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{outcomes: []Outcome{{Status: "done"}}})
	killed := make(chan struct{}, 1)
	startPendingAdoption(t, m, as, agentsDir, attachRecordingShim{killed: killed})

	if err := m.Kill("smoke"); err != nil {
		t.Fatalf("Kill adopted shim: %v", err)
	}
	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("adopted shim did not receive Kill")
	}
}

func TestManagerStartDuringAdoptionDoesNotLaunchDuplicate(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done"}}}
	m, as, agentsDir, _ := newManager(t, r)
	startPendingAdoption(t, m, as, agentsDir, attachRecordingShim{})

	if err := m.Start("smoke"); err != nil {
		t.Fatalf("Start adopted agent: %v", err)
	}
	m.mu.Lock()
	_, started := m.runs["smoke"]
	m.mu.Unlock()
	if started {
		t.Fatal("Start created a normal runtime while adoption was pending")
	}
	iterations, err := as.ListIterations("smoke")
	if err != nil {
		t.Fatal(err)
	}
	if len(iterations) != 1 {
		t.Fatalf("iterations after Start during adoption = %d, want original only", len(iterations))
	}
}

func TestManagerExecRejectsDuringAdoption(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{outcomes: []Outcome{{Status: "done"}}})
	startPendingAdoption(t, m, as, agentsDir, attachRecordingShim{})

	if _, err := m.Exec("smoke", "duplicate"); err == nil || !strings.Contains(err.Error(), "adopted running iteration") {
		t.Fatalf("Exec during adoption error = %v, want adopted running iteration", err)
	}
	m.mu.Lock()
	_, started := m.runs["smoke"]
	m.mu.Unlock()
	if started {
		t.Fatal("Exec created a normal runtime while adoption was pending")
	}
}

func TestManagerStopDuringAdoptionDoesNotRestartEngine(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{outcomes: []Outcome{{Status: "done"}}})
	killed := make(chan struct{}, 1)
	id := startPendingAdoption(t, m, as, agentsDir, attachRecordingShim{killed: killed})

	if err := m.Stop("smoke"); err != nil {
		t.Fatalf("Stop adopted agent: %v", err)
	}
	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("Stop did not kill the adopted shim")
	}
	l := agentdir.New(agentsDir, "smoke").WithRuntime(m.cfg.RuntimeDir)
	if err := os.WriteFile(l.ResultPath(id), []byte(`{"exit_code":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reconciled := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(reconciled)
	}()
	select {
	case <-reconciled:
	case <-time.After(2 * time.Second):
		t.Fatal("post-adoption reconciliation did not finish")
	}
	m.mu.Lock()
	_, started := m.runs["smoke"]
	m.mu.Unlock()
	if started {
		t.Fatal("post-adoption reconciliation restarted a stopped agent")
	}
	ag, err := as.Get("smoke")
	if err != nil {
		t.Fatal(err)
	}
	if ag.Enabled {
		t.Fatal("Stop did not persist Enabled=false")
	}
}

func TestManagerRestartDuringAdoptionLaunchesInteractiveIteration(t *testing.T) {
	r := &killBlockingRunner{started: make(chan string, 1)}
	m, as, agentsDir, _ := newManager(t, r)
	killed := make(chan struct{}, 1)
	id := startPendingAdoption(t, m, as, agentsDir, attachRecordingShim{killed: killed})
	ag, err := as.Get("smoke")
	if err != nil {
		t.Fatal(err)
	}
	ag.LoopEnabled = false
	if err := as.Update(ag); err != nil {
		t.Fatal(err)
	}

	if err := m.Restart("smoke"); err != nil {
		t.Fatalf("Restart adopted agent: %v", err)
	}
	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("Restart did not kill the adopted shim")
	}
	l := agentdir.New(agentsDir, "smoke").WithRuntime(m.cfg.RuntimeDir)
	if err := os.WriteFile(l.ResultPath(id), []byte(`{"exit_code":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Restart did not launch a replacement interactive iteration after adoption")
	}
}

func TestManagerStopCancelsConsumedAdoptionRestart(t *testing.T) {
	r := &killBlockingRunner{started: make(chan string, 1)}
	m, as, agentsDir, _ := newManager(t, r)
	killed := make(chan struct{}, 1)
	id := startPendingAdoption(t, m, as, agentsDir, attachRecordingShim{killed: killed})
	ag, err := as.Get("smoke")
	if err != nil {
		t.Fatal(err)
	}
	ag.LoopEnabled = false
	if err := as.Update(ag); err != nil {
		t.Fatal(err)
	}
	launching := make(chan struct{})
	proceed := make(chan struct{})
	m.cfg.BeforeRestartLaunch = func() {
		close(launching)
		<-proceed
	}

	if err := m.Restart("smoke"); err != nil {
		t.Fatalf("Restart adopted agent: %v", err)
	}
	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("Restart did not kill the adopted shim")
	}
	l := agentdir.New(agentsDir, "smoke").WithRuntime(m.cfg.RuntimeDir)
	if err := os.WriteFile(l.ResultPath(id), []byte(`{"exit_code":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-launching:
	case <-time.After(2 * time.Second):
		t.Fatal("post-adoption Restart did not reach the launch gate")
	}
	if err := m.Stop("smoke"); err != nil {
		t.Fatalf("Stop after consumed Restart: %v", err)
	}
	close(proceed)
	select {
	case started := <-r.started:
		t.Fatalf("replacement iteration %q started after Stop completed", started)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestManagerAdoptedControlCannotCrossSocketReuse(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{outcomes: []Outcome{{Status: "done"}}})
	id := startPendingAdoption(t, m, as, agentsDir, attachRecordingShim{})
	connecting := make(chan struct{})
	proceed := make(chan struct{})
	m.cfg.ConnectShim = func(sock string) (*shim.Client, error) {
		close(connecting)
		<-proceed
		return shim.Connect(sock)
	}

	sendDone := make(chan error, 1)
	go func() { sendDone <- m.SendKeys("smoke", "race") }()
	select {
	case <-connecting:
	case <-time.After(time.Second):
		t.Fatal("SendKeys did not reach shim connection")
	}
	l := agentdir.New(agentsDir, "smoke").WithRuntime(m.cfg.RuntimeDir)
	if err := os.WriteFile(l.ResultPath(id), []byte(`{"exit_code":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(l.ShimSock()); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("adoption did not remove the old shim socket")
		}
		time.Sleep(10 * time.Millisecond)
	}

	handoffUnlocked := m.mu.TryLock()
	if handoffUnlocked {
		m.mu.Unlock()
	}
	close(proceed)
	sendErr := <-sendDone

	replacementKeys := make(chan string, 1)
	replacementListener, err := net.Listen("unix", l.ShimSock())
	if err != nil {
		t.Fatalf("bind replacement shim: %v", err)
	}
	defer replacementListener.Close()
	go func() {
		_ = shim.Serve(replacementListener, attachRecordingShim{sent: replacementKeys})
	}()
	if handoffUnlocked {
		t.Fatal("handoff lock was released before adopted control finished connecting")
	}
	if sendErr == nil {
		t.Fatal("SendKeys unexpectedly reached a replacement shim")
	}
	select {
	case keys := <-replacementKeys:
		t.Fatalf("replacement shim received keys %q", keys)
	default:
	}
}

func TestManagerAttachAndResizeForwardToLiveShim(t *testing.T) {
	r := &killBlockingRunner{started: make(chan string, 1)}
	m, _, agentsDir, _ := newManager(t, r)
	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "smoke", Harness: "stub", Plugins: []string{"context"}, Loop: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	if _, err := m.Exec("smoke", ""); err != nil {
		t.Fatal(err)
	}
	awaitCurrentIteration(t, m, "smoke")
	l := agentdir.New(agentsDir, "smoke").WithRuntime(m.cfg.RuntimeDir)
	ln, err := net.Listen("unix", l.ShimSock())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fake := attachRecordingShim{resized: make(chan shim.ResizeParams, 1)}
	go func() { _ = shim.Serve(ln, fake) }()

	conn, err := m.Attach("smoke", 80, 24)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("write to attach conn: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read from attach conn: %v", err)
	}
	if buf[0] != 'x' {
		t.Fatalf("attach conn round-trip = %q, want %q", buf, "x")
	}

	if err := m.Resize("smoke", 100, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	select {
	case p := <-fake.resized:
		if p.Cols != 100 || p.Rows != 40 {
			t.Fatalf("Resize params = %+v, want {100 40}", p)
		}
	case <-time.After(time.Second):
		t.Fatal("live shim did not receive resize RPC")
	}
}

func TestManagerKillRecoveryWinsCancelledRunnerNormalOutcome(t *testing.T) {
	r := &killBlockingRunner{
		started: make(chan string, 2), release: make(chan struct{}),
		returnAfterCancel: &Outcome{Status: "timeout", DoneFlag: true, ExitCode: 0, CPUMs: 17, MemPeakKB: 23},
	}
	var releaseOnce sync.Once
	releaseRunner := func() { releaseOnce.Do(func() { close(r.release) }) }
	m, as, agentsDir, _ := newManager(t, r)
	var finishEvents, closes atomic.Int32
	m.cfg.Emit = func(event events.Event) {
		if event.Type == "iteration" && event.Data["phase"] == "finish" {
			finishEvents.Add(1)
		}
	}
	recorder := &captureRecorder{}
	m.cfg.AuditFor = func(string) Recorder { return recorder }
	m.cfg.OnIterationClose = func(string, string) { closes.Add(1) }
	completed := make(chan string, 2)
	m.cfg.IterationCompleted = func(agentName, iterationID string) {
		it, err := as.GetIteration(agentName, iterationID)
		if err != nil {
			completed <- "lookup error: " + err.Error()
			return
		}
		completed <- agentName + "/" + iterationID + "/" + it.Status
	}
	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "smoke", Harness: "stub", Plugins: []string{"context"}, Loop: true, OnTimeout: "stop"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	// Registered after m.Shutdown so it runs before it: t.Cleanup is LIFO. A
	// t.Fatal between the two Kill calls below would otherwise leave the runner
	// parked and make Shutdown burn its full wait before giving up.
	t.Cleanup(releaseRunner)
	if err := as.Create(agent.Agent{Name: "other", ImageRef: "basic:latest"}); err != nil {
		t.Fatal(err)
	}
	if err := as.CreateIteration(agent.Iteration{ID: "other-live", Agent: "other", Status: "running", StartedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Exec("smoke", ""); err != nil {
		t.Fatal(err)
	}
	id := awaitCurrentIteration(t, m, "smoke")
	select {
	case got := <-r.started:
		if got != id {
			t.Fatalf("runner started %q, want %q", got, id)
		}
	case <-time.After(time.Second):
		t.Fatal("initial iteration did not reach runner")
	}
	l := agentdir.New(agentsDir, "smoke").WithRuntime(m.cfg.RuntimeDir)
	if err := os.WriteFile(l.ShimSock(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Kill("smoke"); err != nil {
		t.Fatalf("recover stale kill: %v", err)
	}
	select {
	case got := <-completed:
		if want := "smoke/" + id + "/harness_error"; got != want {
			t.Fatalf("completion = %q, want %q", got, want)
		}
	default:
		t.Fatal("stale-kill terminal commit did not signal goal completion")
	}
	if err := m.Kill("smoke"); err != nil {
		t.Fatalf("repeated stale kill: %v", err)
	}
	select {
	case got := <-completed:
		t.Fatalf("repeated stale kill emitted duplicate completion %q", got)
	default:
	}
	// Both Kill calls above have observed the recovery path; let the cancelled
	// iteration finish unwinding so the polls below can see it close.
	releaseRunner()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		it, _ := as.GetIteration("smoke", id)
		if it.Status != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	it, _ := as.GetIteration("smoke", id)
	if it.Status != "harness_error" || it.ExitCode == nil || *it.ExitCode != -1 {
		t.Fatalf("recovered iteration = %+v, want terminal harness_error with exit -1", it)
	}
	// The repeated Kill above is idempotent while the recovered iteration is
	// unwinding. Once it has closed, the stale-kill marker must no longer mask
	// the normal no-running-iteration error.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		rt := m.runs["smoke"]
		current := ""
		if rt != nil {
			current = rt.engine.CurrentIterationID()
		}
		marker := m.staleKills["smoke"]
		m.mu.Unlock()
		// Both conditions are needed. The engine clears its current iteration
		// inside the run body, but drops the stale-kill marker from a deferred
		// close hook that only fires when that body returns - with a log write
		// and a store read in between. Waiting on the current iteration alone
		// releases this poll exactly as that window opens, and the Kill below
		// would then be masked by the still-live marker.
		if current == "" && marker == "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := m.Kill("smoke"); err == nil || !strings.Contains(err.Error(), "has no running iteration") {
		t.Fatalf("Kill after recovered iteration unwound = %v, want no-running-iteration error", err)
	}
	select {
	case got := <-completed:
		t.Fatalf("recovered iteration unwind emitted duplicate completion %q", got)
	default:
	}
	finishedAudits := 0
	for _, event := range recorder.snapshot() {
		if event.typ == "iteration_finished" {
			finishedAudits++
		}
	}
	stored, err := as.Get("smoke")
	if err != nil || finishEvents.Load() != 0 || finishedAudits != 0 || closes.Load() != 1 || !stored.LoopEnabled || stored.ErrorReason != "" {
		t.Fatalf("losing engine side effects: finish_events=%d finish_audits=%d closes=%d agent=%#v err=%v",
			finishEvents.Load(), finishedAudits, closes.Load(), stored, err)
	}
	if err := m.Stop("smoke"); err != nil {
		t.Fatal(err)
	}
	if err := m.Kill("smoke"); err == nil || !strings.Contains(err.Error(), "has no running iteration") {
		t.Fatalf("Kill after stop = %v, want no-running-iteration error", err)
	}
	if _, err := os.Stat(l.ShimSock()); !os.IsNotExist(err) {
		t.Fatalf("stale shim socket was not removed: %v", err)
	}
	if other, _ := as.GetIteration("other", "other-live"); other.Status != "running" {
		t.Fatalf("unrelated iteration changed: %+v", other)
	}
	if _, err := m.Exec("smoke", "next"); err != nil {
		t.Fatalf("subsequent iteration did not launch: %v", err)
	}
	select {
	case next := <-r.started:
		if next == id {
			t.Fatal("expected a new iteration after recovery")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subsequent iteration did not reach runner")
	}
	// The engine launches this next iteration only after every defer from the
	// recovered one has run, making this an exact (non-timing-based) duplicate
	// check for the stale-kill/unwind handoff.
	select {
	case got := <-completed:
		t.Fatalf("recovered iteration emitted duplicate completion before next launch: %q", got)
	default:
	}
	it, err = as.GetIteration("smoke", id)
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "harness_error" || it.ExitCode == nil || *it.ExitCode != -1 || it.DoneFlag || it.CPUMs != nil || it.MemPeakKB != nil {
		t.Fatalf("cancelled runner overwrote stale-kill winner: %+v", it)
	}
}

func TestManagerRunProvisionsAndStops(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, agentsDir, _ := newManager(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "smoke",
		Harness: "stub", Plugins: []string{"context"}, Loop: true})
	if err != nil || name != "smoke" {
		t.Fatalf("run: name=%q err=%v", name, err)
	}
	_ = ctx
	// dir provisioned
	if _, err := os.Stat(filepath.Join(agentsDir, "smoke", "image", "prompt", "template.json")); err != nil {
		t.Fatalf("agent dir not provisioned: %v", err)
	}
	got, _ := as.Get("smoke")
	if !got.LoopEnabled {
		t.Fatalf("loop intent not recorded: %+v", got)
	}
	if got.Enabled {
		t.Fatalf("freshly created agent should be disabled: %+v", got)
	}
	if st, _ := m.LiveState("smoke"); st != "stopped" {
		t.Fatalf("after run LiveState = %q, want stopped (created disabled)", st)
	}
	if err := m.Stop("smoke"); err != nil {
		t.Fatal(err)
	}
	got, _ = as.Get("smoke")
	if got.Enabled {
		t.Fatalf("after stop enabled still true: %+v", got)
	}
	if st, _ := m.LiveState("smoke"); st != "stopped" {
		t.Fatalf("after stop LiveState = %q, want stopped", st)
	}
	if err := m.Remove("smoke", false, true); err != nil {
		t.Fatal(err)
	}
	if ok, _ := as.Exists("smoke"); ok {
		t.Fatal("agent not removed")
	}
	m.Shutdown()
}

func TestRunRejectsContractInvalidImageBeforeProvisioning(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{})
	buildContractInvalidImage(t, m.cfg.ImgStore, "invalid")

	if _, err := m.Run(registry.RunSpec{ImageRef: "invalid:latest", Name: "worker"}); err == nil || !strings.Contains(err.Error(), `plugin "loop" requires packaged skill "loop"`) {
		t.Fatalf("Run error = %v", err)
	}
	if ok, _ := as.Exists("worker"); ok {
		t.Fatal("contract-invalid image created an agent row")
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "worker")); !os.IsNotExist(err) {
		t.Fatalf("contract-invalid image published an agent tree: %v", err)
	}
}

func TestRunV2UsesEnabledExternalPluginResolver(t *testing.T) {
	m, _, _, _ := newManager(t, &fakeRunner{})
	pluginDir := filepath.Join(filepath.Dir(m.cfg.ImgStore.Dir), "plugins", "widget")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"widget","version":"1.0.0","protocol_version":1,"types":["tool"],"exec":"run.sh","channels":{"publish":[],"subscribe":[]}}`
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "run.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ref := image.Ref{Name: "external-v2", Tag: "latest"}
	_, err := image.BuildV2(&imagefile.V2{
		SchemaVersion: 2,
		Plugins:       []imagefile.V2Plugin{{Name: "widget"}},
	}, imagefile.ResolveRoots{}, ref, m.cfg.ImgStore, time.Now, func(string) (plugincaps.ResolvedPlugin, error) {
		return plugincaps.ResolvedPlugin{Installed: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.ExternalPlugins = func(string) (plugincaps.ResolvedPlugin, error) {
		return plugincaps.ResolvedPlugin{}, nil // installed on disk, but disabled/inactive
	}

	if _, err := m.Run(registry.RunSpec{ImageRef: ref.String(), Name: "worker"}); err == nil || !strings.Contains(err.Error(), `unknown plugin "widget"`) {
		t.Fatalf("Run accepted disabled external plugin: %v", err)
	}
}

func TestRunV2DoesNotReadLegacyExternalPluginPrompt(t *testing.T) {
	m, as, _, _ := newManager(t, &fakeRunner{})
	pluginDir := filepath.Join(filepath.Dir(m.cfg.ImgStore.Dir), "plugins", "widget")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"widget","version":"1.0.0","protocol_version":1,"types":["tool"],"exec":"run.sh","prompt":"missing.md","channels":{"publish":[],"subscribe":[]}}`
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "run.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ref := image.Ref{Name: "metadata-v2", Tag: "latest"}
	_, err := image.BuildV2(&imagefile.V2{
		SchemaVersion: 2,
		Plugins:       []imagefile.V2Plugin{{Name: "widget"}},
	}, imagefile.ResolveRoots{}, ref, m.cfg.ImgStore, time.Now, func(string) (plugincaps.ResolvedPlugin, error) {
		return plugincaps.ResolvedPlugin{Installed: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Run(registry.RunSpec{ImageRef: ref.String(), Name: "worker"}); err != nil {
		t.Fatalf("Run read schema-v1 plugin prompt metadata: %v", err)
	}
	if got, err := as.Get("worker"); err != nil || len(got.Plugins) != 1 || got.Plugins[0] != "widget" {
		t.Fatalf("created agent plugins=%v err=%v", got.Plugins, err)
	}
}

func TestStartEnablesStopDisables(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, _, _ := newManager(t, r)
	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "smoke",
		Harness: "stub", Plugins: []string{"context"}, Loop: false}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("smoke"); err != nil {
		t.Fatal(err)
	}
	got, _ := as.Get("smoke")
	if !got.Enabled {
		t.Fatalf("after Start Enabled=false, want true")
	}
	if got.LoopEnabled {
		t.Fatalf("after direct Start LoopEnabled=true, want unchanged false")
	}
	if st, _ := m.LiveState("smoke"); st == "stopped" {
		t.Fatalf("after Start state=stopped, want idle/running")
	}
	if err := m.SetLoopEnabled("smoke", true); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop("smoke"); err != nil {
		t.Fatal(err)
	}
	got, _ = as.Get("smoke")
	if got.Enabled {
		t.Fatalf("after Stop Enabled=true, want false")
	}
	if !got.LoopEnabled {
		t.Fatalf("after direct Stop LoopEnabled=false, want unchanged true")
	}
	if st, _ := m.LiveState("smoke"); st != "stopped" {
		t.Fatalf("after Stop state=%q, want stopped", st)
	}
	m.Shutdown()
}

func TestToolsLoopControlPersistsBothFlagsAndReturnsStoredState(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, agentsDir, _ := newManager(t, r)
	defer m.Shutdown()
	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "self",
		Harness: "stub", Plugins: []string{"context"}, Loop: false}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("self"); err != nil {
		t.Fatal(err)
	}
	c := client.New(agentdir.New(agentsDir, "self").WithRuntime(m.cfg.RuntimeDir).Sock())

	raw, err := c.Call("POST", "/tools/loop/control", map[string]any{"action": "start"})
	if err != nil {
		t.Fatalf("tools loop start: %v", err)
	}
	var start map[string]any
	if err := json.Unmarshal(raw, &start); err != nil {
		t.Fatal(err)
	}
	if got, _ := as.Get("self"); !got.Enabled || !got.LoopEnabled {
		t.Fatalf("after tools loop start = %+v, want both flags true", got)
	}
	if start["enabled"] != true || start["loop_enabled"] != true {
		t.Fatalf("tools loop start payload = %v, want persisted flags true", start)
	}

	raw, err = c.Call("POST", "/tools/loop/control", map[string]any{"action": "stop"})
	if err != nil {
		t.Fatalf("tools loop stop: %v", err)
	}
	var stop map[string]any
	if err := json.Unmarshal(raw, &stop); err != nil {
		t.Fatal(err)
	}
	if got, _ := as.Get("self"); got.Enabled || got.LoopEnabled {
		t.Fatalf("after tools loop stop = %+v, want both flags false", got)
	}
	if stop["enabled"] != false || stop["loop_enabled"] != false {
		t.Fatalf("tools loop stop payload = %v, want persisted flags false", stop)
	}
}

func TestSetLoopEnabledWakesParkedRuntime(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, _, _ := newManager(t, r)
	t.Cleanup(m.Shutdown)
	if _, err := m.Run(registry.RunSpec{
		ImageRef: "basic:latest",
		Name:     "smoke",
		Harness:  "stub",
		Plugins:  []string{"context"},
		Loop:     false,
	}); err != nil {
		t.Fatal(err)
	}
	ag, err := as.Get("smoke")
	if err != nil {
		t.Fatal(err)
	}
	ag.IntervalS = 1
	if err := as.Update(ag); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("smoke"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetLoopEnabled("smoke", true); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if iterations, _ := as.ListIterations("smoke"); len(iterations) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("enabling Autopilot did not wake the parked runtime")
}

func TestRunRejectsTraversalName(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, agentsDir, _ := newManager(t, r)
	defer m.Shutdown()

	// A traversing --name must be refused before any dir is provisioned or the
	// agent is persisted.
	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "../../evil"}); err == nil {
		t.Fatal("Run with traversing name succeeded, want error")
	}
	// No directory escaped AgentsDir: the join target sits outside agentsDir.
	escape := filepath.Join(agentsDir, "../../evil")
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatalf("escape dir %q exists (stat err=%v)", escape, err)
	}
	// AgentsDir's parent is untouched and nothing was persisted.
	if list, err := as.List(); err != nil || len(list) != 0 {
		t.Fatalf("after rejected Run: list=%v err=%v, want empty", list, err)
	}

	// Other malformed names are all rejected. (An empty --name is not here: Run
	// treats it as "auto-generate", which the ValidName table covers instead.)
	for _, bad := range []string{"..", "a/b", "/etc", "UP"} {
		if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: bad}); err == nil {
			t.Fatalf("Run(name=%q) succeeded, want error", bad)
		}
	}
	if list, _ := as.List(); len(list) != 0 {
		t.Fatalf("agents persisted despite rejected names: %v", list)
	}

	// A valid explicit name works.
	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "my-agent"})
	if err != nil || name != "my-agent" {
		t.Fatalf("Run(my-agent): name=%q err=%v", name, err)
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "my-agent", "image", "prompt", "template.json")); err != nil {
		t.Fatalf("valid agent not provisioned: %v", err)
	}
	// A generated name (empty --name) also works.
	gen, err := m.Run(registry.RunSpec{ImageRef: "basic:latest"})
	if err != nil || !agent.ValidName(gen) {
		t.Fatalf("Run(generated): name=%q err=%v", gen, err)
	}
}

func TestReattachAdoptsLiveIteration(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done"}}}
	m, as, agentsDir, _ := newManager(t, r)
	// Persist a running agent with a running iteration and a result.json waiting.
	ag := agent.Agent{Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub", LoopEnabled: false, Plugins: []string{"context"}}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, "smoke").WithRuntime(m.cfg.RuntimeDir)
	id := "smoke-20260706100000-1"
	l.EnsureIteration(id)
	os.WriteFile(l.ShimSock(), []byte{}, 0o600)
	as.CreateIteration(agent.Iteration{ID: id, Agent: "smoke", Trigger: "interval",
		Status: "running", StartedAt: time.Now().Format(time.RFC3339)})
	os.WriteFile(l.ResultPath(id), []byte(`{"exit_code":0,"ended_at":"t","cpu_ms":3,"mem_peak_kb":4}`), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	// Wait for the adopt-poller to record the outcome.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if it, err := as.GetIteration("smoke", id); err == nil && it.Status != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	it, _ := as.GetIteration("smoke", id)
	if it.Status == "running" {
		t.Fatalf("live iteration not adopted: %+v", it)
	}
	m.Shutdown()
}

// TestReattachOnlyStartsEnabledAgents proves the boot reconcile gate keys on
// the master Enabled flag, not LoopEnabled: an agent with LoopEnabled=true but
// Enabled=false must NOT come up on daemon boot.
func TestReattachOnlyStartsEnabledAgents(t *testing.T) {
	r := &fakeRunner{}
	m, as, _, _ := newManager(t, r)
	if err := as.Create(agent.Agent{Name: "on", ImageRef: "basic:latest", HarnessType: "stub", LoopEnabled: true, Enabled: true, IntervalS: 0}); err != nil {
		t.Fatal(err)
	}
	if err := as.Create(agent.Agent{Name: "off", ImageRef: "basic:latest", HarnessType: "stub", LoopEnabled: true, Enabled: false, IntervalS: 0}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.LiveState("off"); st != "stopped" {
		t.Fatalf("disabled agent came up on boot: state=%q", st)
	}
	m.mu.Lock()
	_, started := m.runs["off"]
	m.mu.Unlock()
	if started {
		t.Fatalf("disabled agent's engine was started on boot")
	}
	m.Shutdown()
}

func TestRecordAdoptedPreservesPersistedTimeoutAndHardWatchdogReason(t *testing.T) {
	r := &fakeRunner{}
	m, as, agentsDir, _ := newManager(t, r)
	ag := agent.Agent{Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub", Plugins: []string{"context"}}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	id := "smoke-adopted-hard"
	if err := as.CreateIteration(agent.Iteration{ID: id, Agent: ag.Name, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := as.InitializeIterationTimeout(id, 30, 90, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, ag.Name).WithRuntime(m.cfg.RuntimeDir)
	if err := l.EnsureIteration(id); err != nil {
		t.Fatal(err)
	}
	m.recordAdopted(l, agentdir.LiveIteration{Agent: ag.Name, ID: id}, shim.IterationResult{ExitCode: 143, TerminationReason: "hard_timeout"})
	it, err := as.GetIteration(ag.Name, id)
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "timeout" || it.TimeoutDeadline == nil || it.TimeoutExtensions != 0 {
		t.Fatalf("adopted hard timeout lost persisted state: %+v", it)
	}
}

func TestRecordAdoptedSignalsGoalCompletionAfterFinalStatus(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{})
	ag := agent.Agent{Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub"}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	id := "smoke-adopted-goal"
	if err := as.CreateIteration(agent.Iteration{ID: id, Agent: ag.Name, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	var got string
	m.cfg.IterationCompleted = func(agentName, iterationID string) {
		it, err := as.GetIteration(agentName, iterationID)
		if err != nil {
			t.Fatal(err)
		}
		got = agentName + "/" + iterationID + "/" + it.Status
	}
	l := agentdir.New(agentsDir, ag.Name).WithRuntime(m.cfg.RuntimeDir)
	if err := l.EnsureIteration(id); err != nil {
		t.Fatal(err)
	}

	m.recordAdopted(l, agentdir.LiveIteration{Agent: ag.Name, ID: id}, shim.IterationResult{ExitCode: 0})

	if got != "smoke/smoke-adopted-goal/no_i_am_done" {
		t.Fatalf("completion = %q", got)
	}
}

type adoptionProxy struct {
	revokedIterations []string
}

func (*adoptionProxy) ProxyBaseURL() string { return "http://127.0.0.1:1" }
func (*adoptionProxy) MintToken(string, string, string, string, string) (string, error) {
	return "token", nil
}
func (*adoptionProxy) RevokeToken(string) {}
func (p *adoptionProxy) RevokeIteration(iteration string) {
	p.revokedIterations = append(p.revokedIterations, iteration)
}
func (*adoptionProxy) UpdateTask(string, string, string) int { return 0 }

func TestAdoptionRevokesIterationProxyLease(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%v", stale), func(t *testing.T) {
			m, as, agentsDir, _ := newManager(t, &fakeRunner{})
			proxy := &adoptionProxy{}
			m.cfg.Proxy = proxy
			ag := agent.Agent{Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub"}
			if err := as.Create(ag); err != nil {
				t.Fatal(err)
			}
			id := "smoke-adopted"
			if err := as.CreateIteration(agent.Iteration{
				ID: id, Agent: ag.Name, Trigger: "manual", Status: "running",
			}); err != nil {
				t.Fatal(err)
			}
			l := agentdir.New(agentsDir, ag.Name).WithRuntime(m.cfg.RuntimeDir)
			if err := l.EnsureIteration(id); err != nil {
				t.Fatal(err)
			}
			live := agentdir.LiveIteration{Agent: ag.Name, ID: id}
			if stale {
				m.recordStaleAdoption(l, live)
			} else {
				m.recordAdopted(l, live, shim.IterationResult{ExitCode: 0})
			}
			if len(proxy.revokedIterations) != 1 || proxy.revokedIterations[0] != id {
				t.Fatalf("revoked iterations = %v, want [%s]", proxy.revokedIterations, id)
			}
		})
	}
}

// stubShim is a live shim RPC server used by reattach tests so Status probes
// succeed and the adoption never times out as stale.
type stubShim struct{}

func (stubShim) Status() shim.StatusResult          { return shim.StatusResult{Running: true, PID: 1} }
func (stubShim) Kill() error                        { return nil }
func (stubShim) Screen() (string, error)            { return "", nil }
func (stubShim) SendKeys(shim.SendKeysParams) error { return nil }
func (stubShim) Report() shim.ReportResult          { return shim.ReportResult{} }

type timeoutAdoptionShim struct {
	kills    atomic.Int32
	statuses atomic.Int32
	onKill   func()
}

func (s *timeoutAdoptionShim) Status() shim.StatusResult {
	s.statuses.Add(1)
	return shim.StatusResult{Running: true, PID: 1}
}
func (s *timeoutAdoptionShim) Kill() error {
	if s.kills.Add(1) == 1 && s.onKill != nil {
		s.onKill()
	}
	return nil
}
func (*timeoutAdoptionShim) Screen() (string, error)            { return "", nil }
func (*timeoutAdoptionShim) SendKeys(shim.SendKeysParams) error { return nil }
func (*timeoutAdoptionShim) Report() shim.ReportResult          { return shim.ReportResult{} }
func (*timeoutAdoptionShim) SetHardDeadline(string) error       { return nil }

func TestAdoptionEnforcesExtendedPersistedSoftDeadlineOnce(t *testing.T) {
	defer swapAdoptTiming(5*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond)()

	m, as, agentsDir, _ := newManager(t, &fakeRunner{})
	base := time.Now().UTC()
	var clockNanos atomic.Int64
	clockNanos.Store(base.Add(29 * time.Second).UnixNano())
	m.cfg.Clock = func() time.Time { return time.Unix(0, clockNanos.Load()).UTC() }
	ag := agent.Agent{Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub"}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	id := "smoke-adopted-soft-timeout"
	if err := as.CreateIteration(agent.Iteration{ID: id, Agent: ag.Name, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := as.InitializeIterationTimeout(id, 30, 90, base); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, ag.Name).WithRuntime(m.cfg.RuntimeDir)
	if err := l.EnsureIteration(id); err != nil {
		t.Fatal(err)
	}
	handler := &timeoutAdoptionShim{onKill: func() {
		_ = os.WriteFile(l.ResultPath(id), []byte(`{"exit_code":143}`), 0o600)
	}}
	ln, err := net.Listen("unix", l.ShimSock())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = shim.Serve(ln, handler) }()
	t.Cleanup(func() { _ = ln.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()

	if _, err := m.ExtendIterationTimeout(ag.Name, id); err != nil {
		t.Fatalf("extend adopted timeout: %v", err)
	}
	clockNanos.Store(base.Add(31 * time.Second).UnixNano())
	probeDeadline := time.Now().Add(time.Second)
	for time.Now().Before(probeDeadline) && handler.statuses.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := handler.statuses.Load(); got < 2 {
		t.Fatalf("adoption did not probe live shim after extension: statuses=%d", got)
	}
	if got := handler.kills.Load(); got != 0 {
		t.Fatalf("adoption enforced superseded deadline: kills=%d", got)
	}

	clockNanos.Store(base.Add(61 * time.Second).UnixNano())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		it, _ := as.GetIteration(ag.Name, id)
		if it.Status == "timeout" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	it, err := as.GetIteration(ag.Name, id)
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "timeout" || it.TimeoutTriggeredAt == nil {
		t.Fatalf("adopted soft deadline not durably enforced: %+v", it)
	}
	if got := handler.kills.Load(); got != 1 {
		t.Fatalf("soft timeout kills=%d, want exactly 1", got)
	}
}

func serveStubShim(t *testing.T, sock string) {
	t.Helper()
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen shim sock: %v", err)
	}
	go func() { _ = shim.Serve(l, stubShim{}) }()
	t.Cleanup(func() {
		_ = l.Close()
		_ = os.Remove(sock)
	})
}

// Finding 1: Start after Stop must restore state=running even though the engine
// runtime already exists, and a fresh manager's StartAll must then pick it up.
func TestStartAfterStopRestoresRunningState(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "no_i_am_done"}}}
	m, as, agentsDir, _ := newManager(t, r)

	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "smoke",
		Harness: "stub", Plugins: []string{"context"}, Loop: true})
	if err != nil || name != "smoke" {
		t.Fatalf("run: name=%q err=%v", name, err)
	}
	if err := m.Stop("smoke"); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.LiveState("smoke"); st != "stopped" {
		t.Fatalf("after stop LiveState = %q, want stopped", st)
	}
	// Runtime still exists (Stop does not tear it down); Start must re-enable.
	if err := m.Start("smoke"); err != nil {
		t.Fatal(err)
	}
	got, _ := as.Get("smoke")
	if !got.Enabled {
		t.Fatalf("after restart not enabled: %+v", got)
	}
	if st, _ := m.LiveState("smoke"); st != "idle" {
		t.Fatalf("after restart LiveState = %q, want idle", st)
	}
	m.Shutdown()

	// A fresh manager over the same store/dir reattaches the running agent.
	m2 := NewManager(ManagerConfig{
		AgentsDir: agentsDir, ShimBin: "/opt/tariboy-shim",
		Store: as, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: time.Now,
		RunnerFactory: func(agent.Agent) IterationRunner { return r },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m2.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := as.ListIterations("smoke")
	if _, err := m2.Exec("smoke", ""); err != nil {
		t.Fatalf("exec on reattached agent: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if its, _ := as.ListIterations("smoke"); len(its) > len(before) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if its, _ := as.ListIterations("smoke"); len(its) <= len(before) {
		t.Fatal("fresh manager did not pick up the running agent (no iteration ran)")
	}
	m2.Shutdown()
}

// Finding 2: with a live adopted iteration, the engine must not launch a second
// iteration until the adoption finishes (result.json appears and is classified).
func TestReattachDelaysEngineUntilAdopted(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, agentsDir, _ := newManager(t, r)

	ag := agent.Agent{Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub", Enabled: true, LoopEnabled: true, IntervalS: 60,
		OnTimeout: "restart", OnError: "restart", Plugins: []string{"context"}}
	pinBasicImage(t, m, &ag)
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, "smoke").WithRuntime(m.cfg.RuntimeDir)
	id := "smoke-20260706100000-1"
	l.EnsureIteration(id)
	serveStubShim(t, l.ShimSock()) // live shim: probes succeed, no stale timeout
	as.CreateIteration(agent.Iteration{ID: id, Agent: "smoke", Trigger: "interval",
		Status: "running", StartedAt: time.Now().Format(time.RFC3339)})
	// Deliberately no result.json yet.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}

	// While the adoption is pending, the engine must NOT run: only the adopted
	// iteration row exists and it stays running.
	time.Sleep(400 * time.Millisecond)
	if its, _ := as.ListIterations("smoke"); len(its) != 1 {
		t.Fatalf("engine started before adoption finished: %d iterations", len(its))
	}
	if it, _ := as.GetIteration("smoke", id); it.Status != "running" {
		t.Fatalf("adopted iteration prematurely classified: %+v", it)
	}

	// Now the shim finishes: result.json lands, adoption classifies it, and the
	// engine is allowed to start and run a fresh iteration.
	os.WriteFile(l.ResultPath(id), []byte(`{"exit_code":0,"cpu_ms":1,"mem_peak_kb":2}`), 0o600)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		its, _ := as.ListIterations("smoke")
		adopted, _ := as.GetIteration("smoke", id)
		if adopted.Status != "running" && len(its) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	adopted, _ := as.GetIteration("smoke", id)
	if adopted.Status == "running" {
		t.Fatalf("adopted iteration never classified: %+v", adopted)
	}
	if its, _ := as.ListIterations("smoke"); len(its) < 2 {
		t.Fatalf("engine did not start after adoption: %d iterations", len(its))
	}
	m.Shutdown()
}

// Finding 3: a stale shim.sock (SIGKILLed shim, no result.json) must not spin
// forever; adoption classifies it terminally as harness_error and removes the
// dead socket.
func TestReattachStaleShimClassifiesHarnessError(t *testing.T) {
	// Shorten probe cadence so the dead socket is declared stale quickly.
	defer swapAdoptTiming(10*time.Millisecond, 20*time.Millisecond, 50*time.Millisecond)()

	r := &fakeRunner{outcomes: []Outcome{{Status: "done"}}}
	m, as, agentsDir, _ := newManager(t, r)

	ag := agent.Agent{Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub", LoopEnabled: false, Plugins: []string{"context"}}
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, "smoke").WithRuntime(m.cfg.RuntimeDir)
	id := "smoke-20260706100000-1"
	l.EnsureIteration(id)
	// A regular file at shim.sock: Dial/Status fails (not a live socket).
	os.WriteFile(l.ShimSock(), []byte{}, 0o600)
	as.CreateIteration(agent.Iteration{ID: id, Agent: "smoke", Trigger: "interval",
		Status: "running", StartedAt: time.Now().Format(time.RFC3339)})
	// No result.json will ever appear.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if it, _ := as.GetIteration("smoke", id); it.Status != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	it, _ := as.GetIteration("smoke", id)
	if it.Status != "harness_error" {
		t.Fatalf("stale shim not classified: status=%q", it.Status)
	}
	if it.ExitCode == nil || *it.ExitCode != -1 {
		t.Fatalf("stale shim exit_code = %v, want -1", it.ExitCode)
	}
	if _, err := os.Stat(l.ShimSock()); !os.IsNotExist(err) {
		t.Fatalf("stale shim.sock not removed: %v", err)
	}
	m.Shutdown()
}

func TestManagerWakesOnPublish(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, _, st := newManager(t, r)
	// Attach a bus and wire the hook exactly as the daemon does.
	bs := bus.New(st, time.Now)
	m.cfg.Bus = bs
	bs.SetPublishHook(func(_ bus.Message, agents []string) { m.WakeAgents(agents, WakeMessage) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "smoke",
		Harness: "stub", Plugins: []string{"context"}, Loop: true}); err != nil {
		t.Fatal(err)
	}
	// Run() creates the agent disabled (Task 4); enable it in the store so
	// Start() launches an engine that actually reacts to wakes (Enabled
	// flip on Start is Task 5's job, not this test's concern).
	got, err := as.Get("smoke")
	if err != nil {
		t.Fatal(err)
	}
	got.Enabled = true
	if err := as.Update(got); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("smoke"); err != nil {
		t.Fatal(err)
	}
	// smoke subscribes to its own inbox and receives a message -> a message
	// iteration must fire.
	if _, err := bs.Subscribe("smoke", bus.InboxChannel("smoke"), bus.Matcher{}, nil); err != nil {
		t.Fatal(err)
	}
	before := iterationCount(t, as, "smoke")
	if _, err := bs.Publish(bus.Message{Channel: bus.InboxChannel("smoke"), Type: "ping", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if iterationCount(t, as, "smoke") > before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if iterationCount(t, as, "smoke") <= before {
		t.Fatal("publish did not wake the agent")
	}
	m.Shutdown()
}

func TestManagerRetriesPendingAfterInteractiveOwnerCloses(t *testing.T) {
	r := newCollisionRunner()
	m, as, agentsDir, st := newManager(t, r)
	bs := bus.New(st, time.Now)
	m.cfg.Bus = bs
	bs.SetPublishHook(func(_ bus.Message, agents []string) {
		m.WakeAgents(agents, WakeMessage)
	})

	ag := agent.Agent{
		Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub",
		Enabled: true, LoopEnabled: true, Interactive: true, IntervalS: 0,
		OnTimeout: "restart", OnError: "restart", Plugins: []string{"context"},
	}
	pinBasicImage(t, m, &ag)
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	if err := agentdir.New(agentsDir, "smoke").EnsureIteration("bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := bs.Subscribe("smoke", bus.InboxChannel("smoke"), bus.Matcher{}, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)

	if _, err := bs.Publish(bus.Message{
		Channel: bus.InboxChannel("smoke"), Type: "ping", Text: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("engine never observed the live interactive owner")
	}
	if its, _ := as.ListIterations("smoke"); len(its) != 0 {
		t.Fatalf("live interactive owner created iteration rows: %+v", its)
	}

	r.blocked.Store(false)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if iterationCount(t, as, "smoke") == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	its, _ := as.ListIterations("smoke")
	if len(its) != 1 || its[0].Trigger != "message" {
		t.Fatalf("post-collision iterations = %+v, want one message iteration", its)
	}
}

func TestManagerWiresGroupTools(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, agentsDir, st := newManager(t, r)
	bs := bus.New(st, time.Now)
	prov := groups.NewProvisioner(groups.ProvisionerConfig{
		Groups: groups.NewStore(st, time.Now), Agents: as, Bus: bs,
		GroupsDir: filepath.Join(filepath.Dir(agentsDir), "groups"), Clock: time.Now,
	})
	m.cfg.Bus = bs
	m.cfg.Groups = prov
	// Wire the request-deadline seam to a schedule store exactly as the daemon
	// does, so a group request with --deadline arms a one-shot timeout and a
	// reply cancels it (spec §4.2).
	schedStore := schedule.NewStore(st, time.Now)
	bs.SetDeadlineHooks(
		func(tx *sql.Tx, agentName, correlationID, deadline string) error {
			dur, err := time.ParseDuration(deadline)
			if err != nil {
				return err
			}
			_, err = schedStore.AddTx(tx, schedule.Schedule{
				Agent: agentName, Kind: "oneshot",
				Spec:            time.Now().UTC().Add(dur).Format(time.RFC3339),
				Channel:         bus.InboxChannel(agentName),
				MessageTemplate: `{"type":"timeout"}`,
				CorrelationID:   correlationID,
			})
			return err
		},
		func(tx *sql.Tx, correlationID string) error {
			return schedStore.CancelByCorrelationTx(tx, correlationID)
		},
	)
	m.cfg.AuditFor = func(a string) Recorder { return audit.Open(agentdir.New(agentsDir, a).AuditLog(), time.Now) }
	if err := prov.EnsureGroup("dev-team", "manager"); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "manager",
		Harness: "stub", Plugins: []string{"status"}, Loop: false, Group: "dev-team"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "worker",
		Harness: "stub", Plugins: []string{"status"}, Loop: false, Group: "dev-team"}); err != nil {
		t.Fatal(err)
	}
	// Run() no longer auto-starts (Task 4); this test only needs the tools
	// socket/apiServer up, not an active loop, so a plain Start suffices.
	if err := m.Start("manager"); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("worker"); err != nil {
		t.Fatal(err)
	}
	if err := as.SetStatus("worker", "waiting for issue", "2026-07-10T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	audit.Open(agentdir.New(agentsDir, "worker").AuditLog(), time.Now).
		Record("status", "agent", "", map[string]any{"message": "waiting for issue"})

	c := client.New(agentdir.New(agentsDir, "manager").WithRuntime(m.cfg.RuntimeDir).Sock())
	raw, err := c.Call("GET", "/tools/group/info", nil)
	if err != nil {
		t.Fatalf("group info: %v", err)
	}
	var info map[string]any
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	if info["group"] != "dev-team" || info["lead"] != "manager" || info["role"] != "lead" {
		t.Fatalf("group info = %v", info)
	}

	raw, err = c.Call("GET", "/tools/group/status/worker", nil)
	if err != nil {
		t.Fatalf("group status worker: %v", err)
	}
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	member := status["member"].(map[string]any)
	if member["name"] != "worker" || member["status_message"] != "waiting for issue" {
		t.Fatalf("member status = %v", member)
	}

	raw, err = c.Call("POST", "/tools/group/request", map[string]any{"member": "worker", "text": "what is blocking you?", "deadline": "5m"})
	if err != nil {
		t.Fatalf("group request: %v", err)
	}
	var reqRes map[string]any
	if err := json.Unmarshal(raw, &reqRes); err != nil {
		t.Fatal(err)
	}
	// The request rides the Request primitive: kind=request onto the member's
	// group direct channel, correlation id echoed back (spec §4.2).
	if reqRes["kind"] != "request" || reqRes["channel"] != bus.GroupDirect("dev-team", "worker") {
		t.Fatalf("group request result = %v", reqRes)
	}
	corr, _ := reqRes["correlation_id"].(string)
	if corr == "" {
		t.Fatalf("group request has no correlation id: %v", reqRes)
	}
	// The member sees the request pending, threaded as a kind=request with
	// reply_to pointing back at the manager's inbox.
	msgs, err := bs.Pending("worker", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Kind != "request" || msgs[0].Text != "what is blocking you?" ||
		msgs[0].ReplyTo != bus.InboxChannel("manager") {
		t.Fatalf("worker pending = %+v", msgs)
	}
	// The manager's request is outstanding (# Awaiting replies) and its deadline
	// armed a one-shot timeout schedule tagged with the correlation id.
	if pend, _ := bs.PendingRequests("manager"); len(pend) != 1 || pend[0].CorrelationID != corr {
		t.Fatalf("manager pending requests = %+v", pend)
	}
	if scheds, _ := schedStore.List("manager"); len(scheds) != 1 {
		t.Fatalf("expected one armed deadline schedule, got %+v", scheds)
	}
	// A reply from the member retires the pending request and cancels the timeout.
	if _, err := bs.Reply("worker", msgs[0].ID, "still setting up", nil, ""); err != nil {
		t.Fatalf("worker reply: %v", err)
	}
	if pend, _ := bs.PendingRequests("manager"); len(pend) != 0 {
		t.Fatalf("reply should retire pending: %+v", pend)
	}
	if scheds, _ := schedStore.List("manager"); len(scheds) != 0 {
		t.Fatalf("reply should cancel the timeout schedule: %+v", scheds)
	}

	raw, err = c.Call("GET", "/tools/group/observe/worker?tail=1", nil)
	if err != nil {
		t.Fatalf("group observe: %v", err)
	}
	var observed map[string]any
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatal(err)
	}
	if observed["count"].(float64) != 1 {
		t.Fatalf("observed = %v", observed)
	}

	raw, err = c.Call("POST", "/tools/group/loop", map[string]any{"member": "worker", "action": "start"})
	if err != nil {
		t.Fatalf("group loop start: %v", err)
	}
	var groupStart map[string]any
	if err := json.Unmarshal(raw, &groupStart); err != nil {
		t.Fatal(err)
	}
	if got, _ := as.Get("worker"); !got.Enabled || !got.LoopEnabled {
		t.Fatalf("worker after group start = %+v, want both flags true", got)
	}
	if groupStart["enabled"] != true || groupStart["loop_enabled"] != true {
		t.Fatalf("group loop start payload = %v, want persisted flags true", groupStart)
	}
	raw, err = c.Call("POST", "/tools/group/loop", map[string]any{"member": "worker", "action": "stop"})
	if err != nil {
		t.Fatalf("group loop stop: %v", err)
	}
	var groupStop map[string]any
	if err := json.Unmarshal(raw, &groupStop); err != nil {
		t.Fatal(err)
	}
	if got, _ := as.Get("worker"); got.Enabled || got.LoopEnabled {
		t.Fatalf("worker after group stop = %+v, want both flags false", got)
	}
	if groupStop["enabled"] != false || groupStop["loop_enabled"] != false {
		t.Fatalf("group loop stop payload = %v, want persisted flags false", groupStop)
	}
	if _, err := c.Call("POST", "/tools/group/loop", map[string]any{"member": "outsider", "action": "start"}); err == nil {
		t.Fatal("group loop allowed outsider")
	}

	m.Shutdown()
}

// TestReattachWakesEventOnlyAgentWithPending guards against the M4 review
// finding: start() launched an engine but never sent WakeMessage, so an
// event-only agent (IntervalS<=0, message-triggered) that already has an
// unacked pending delivery at reattach time just blocks in select forever —
// stranded on a quiet channel until some unrelated future publish wakes it.
func TestReattachWakesEventOnlyAgentWithPending(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, agentsDir, st := newManager(t, r)
	bs := bus.New(st, time.Now)
	m.cfg.Bus = bs

	// Event-only agent: no interval timer, message-triggered only. Persisted as
	// already running (as it would be across a daemon restart), but its engine
	// has not started yet.
	ag := agent.Agent{Name: "smoke", ImageRef: "basic:latest", HarnessType: "stub", Enabled: true, LoopEnabled: true, IntervalS: 0,
		OnTimeout: "restart", OnError: "restart", Plugins: []string{"context"}}
	pinBasicImage(t, m, &ag)
	if err := as.Create(ag); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, "smoke")
	if err := l.EnsureIteration("bootstrap"); err != nil {
		t.Fatal(err)
	}

	// Subscribe and publish BEFORE the engine starts, so a pending delivery
	// already exists at reattach time.
	if _, err := bs.Subscribe("smoke", bus.InboxChannel("smoke"), bus.Matcher{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := bs.Publish(bus.Message{Channel: bus.InboxChannel("smoke"), Type: "ping", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if pending, err := bs.HasPending("smoke"); err != nil || !pending {
		t.Fatalf("setup: expected pending delivery, pending=%v err=%v", pending, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}

	// Without any further publish, the reattached engine must wake itself and
	// drain the pending delivery via a message-triggered iteration.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if iterationCount(t, as, "smoke") > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	its, _ := as.ListIterations("smoke")
	if len(its) == 0 {
		t.Fatal("event-only agent with pending delivery never ran after reattach")
	}
	if its[0].Trigger != "message" {
		t.Fatalf("iteration trigger = %q, want %q", its[0].Trigger, "message")
	}
	m.Shutdown()
}

func iterationCount(t *testing.T, as *agent.Store, name string) int {
	t.Helper()
	its, _ := as.ListIterations(name)
	return len(its)
}

// swapAdoptTiming shortens the reattach probe cadence for a test and returns a
// restore func.
func swapAdoptTiming(poll, probe, timeout time.Duration) func() {
	op, opr, ot := adoptPollInterval, adoptProbeInterval, adoptProbeTimeout
	adoptPollInterval, adoptProbeInterval, adoptProbeTimeout = poll, probe, timeout
	return func() { adoptPollInterval, adoptProbeInterval, adoptProbeTimeout = op, opr, ot }
}

func TestBuildImageForAgentResolvesRelativePathFromWorkdir(t *testing.T) {
	base := t.TempDir()
	imgStore := &image.Store{Dir: filepath.Join(base, "images")}
	workdir := filepath.Join(base, "workdir")

	// A valid Tariboyfile inside the workdir builds.
	authored := filepath.Join(workdir, "authored")
	if err := os.MkdirAll(authored, 0o755); err != nil {
		t.Fatal(err)
	}
	sf := "schema_version: 2\n"
	if err := os.WriteFile(filepath.Join(authored, "Tariboyfile.yaml"), []byte(sf), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := buildImageForAgent(imgStore, workdir, "authored", "latest", "authored")
	if err != nil {
		t.Fatalf("build inside workdir: %v", err)
	}
	if res["name"] != "authored" {
		t.Fatalf("res = %v", res)
	}
	if !imgStore.Exists(image.Ref{Name: "authored", Tag: "latest"}) {
		t.Fatal("authored image not stored")
	}

	// A bad ref is rejected.
	if _, err := buildImageForAgent(imgStore, workdir, "Bad Ref", "latest", "authored"); err == nil {
		t.Fatal("expected an error for a bad ref, got nil")
	}
}

func TestBuildImageForAgentAssemblesExtends(t *testing.T) {
	base := t.TempDir()
	imgStore := &image.Store{Dir: filepath.Join(base, "images")}
	workdir := filepath.Join(base, "workdir")
	for rel, body := range map[string]string{
		"parent/Tariboyfile.yaml":     "schema_version: 2\nplugins: []\nskills: [{dir: ./skills/demo}]\nprompts: [{file: ./role.md}]\n",
		"parent/role.md":              "parent role",
		"parent/skills/demo/SKILL.md": "---\nname: demo\ndescription: demo\n---\nbody\n",
		"child/Tariboyfile.yaml":      "schema_version: 2\nextends: [../parent]\nplugins: []\nprompts: [{file: ./role.md}]\n",
		"child/role.md":               "child role",
	} {
		path := filepath.Join(workdir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := buildImageForAgent(imgStore, workdir, "child", "latest", "child"); err != nil {
		t.Fatal(err)
	}
	manifest, err := imgStore.Inspect(image.Ref{Name: "child", Tag: "latest"})
	if err != nil || len(manifest.Skills) != 1 {
		t.Fatalf("manifest skills = %#v, %v", manifest.Skills, err)
	}
}

func TestAgentImageBuildSourcePathResolution(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{})
	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "creator", Harness: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := as.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	ag.Cwd = t.TempDir()
	ag.Plugins = append(ag.Plugins, "image-creator")
	if err := as.Update(ag); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, name)
	source := filepath.Join(l.Workdir(), "images", "authored")
	skill := filepath.Join(l.Workdir(), "skills", "review")
	for _, dir := range []string{source, skill} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: review\ndescription: Review changes.\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "Tariboyfile.yaml"), []byte("schema_version: 2\nskills:\n  - dir: ../../skills/review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	externalSkill := filepath.Join(ag.Cwd, "review")
	if err := os.Mkdir(externalSkill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(externalSkill, "SKILL.md"), []byte("---\nname: review\ndescription: Review changes.\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ag.Cwd, "Tariboyfile.yaml"), []byte("schema_version: 2\nskills:\n  - dir: ./review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ag.Cwd, filepath.Join(l.Workdir(), "outside")); err != nil {
		t.Fatal(err)
	}
	parentPath, err := filepath.Rel(l.Workdir(), ag.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	handler := m.newToolsAPIServer(ag, l).Handler()
	for _, tc := range []struct {
		name, path string
	}{
		{"absolute", source},
		{"relative", "images/authored"},
		{"external-cwd", ag.Cwd},
		{"parent", parentPath},
		{"symlink", "outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"name": tc.name, "path": tc.path})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/tools/image/build", bytes.NewReader(body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("build status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if !m.cfg.ImgStore.Exists(image.Ref{Name: tc.name, Tag: "latest"}) {
				t.Fatal("image was not published")
			}
		})
	}
}

func TestAgentAuthoredImageRejectsDisabledExternalPlugin(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{})
	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "creator", Harness: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := as.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	ag.Plugins = append(ag.Plugins, "image-creator")
	if err := as.Update(ag); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, name)
	source := filepath.Join(l.Workdir(), "authored")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "Tariboyfile.yaml"), []byte("schema_version: 2\nplugins:\n  - name: widget\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pluginDir := filepath.Join(filepath.Dir(m.cfg.ImgStore.Dir), "plugins", "widget")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"widget","version":"1.0.0","protocol_version":1,"types":["tool"],"exec":"run.sh","channels":{"publish":[],"subscribe":[]}}`
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "run.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	m.cfg.ExternalPlugins = func(string) (plugincaps.ResolvedPlugin, error) { return plugincaps.ResolvedPlugin{}, nil }

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/tools/image/build", strings.NewReader(`{"name":"authored","path":"authored"}`))
	m.newToolsAPIServer(ag, l).Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `unknown plugin \"widget\"`) {
		t.Fatalf("agent image build status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBuildImageForAgentWaitsForPublicationGate(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "authored")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "Tariboyfile.yaml"), []byte("schema_version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &image.Store{Dir: filepath.Join(t.TempDir(), "images")}
	entered, release, locked := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		locked <- image.WithPublicationGate(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	done := make(chan error, 1)
	go func() {
		_, err := buildImageForAgent(store, workdir, "authored", "latest", "authored")
		done <- err
	}()
	select {
	case err := <-done:
		close(release)
		<-locked
		t.Fatalf("agent-authored publisher ignored publication gate: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The ordinary image builder still rejects symlinks inside packaged skills.
func TestBuildImageForAgentRejectsSkillSymlink(t *testing.T) {
	base := t.TempDir()
	imgStore := &image.Store{Dir: filepath.Join(base, "images")}
	workdir := filepath.Join(base, "workdir")
	authored := filepath.Join(workdir, "authored")

	// A secret host file OUTSIDE the workdir whose content an escape would leak.
	secret := filepath.Join(base, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "id_rsa"), []byte("TOP-SECRET-KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A packaged skill containing a symlink to a file outside its tree.
	inSkill := filepath.Join(authored, "myskill")
	if err := os.MkdirAll(inSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inSkill, "SKILL.md"), []byte("---\nname: myskill\ndescription: Test skill.\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(secret, "id_rsa"), filepath.Join(inSkill, "leak")); err != nil {
		t.Fatal(err)
	}

	writeSF := func(skill string) {
		sf := "schema_version: 2\nskills:\n  - dir: " + skill + "\n"
		if err := os.WriteFile(filepath.Join(authored, "Tariboyfile.yaml"), []byte(sf), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The ordinary builder rejects symlinks inside packaged skill directories.
	writeSF("./myskill")
	if _, err := buildImageForAgent(imgStore, workdir, "leak-inner", "latest", "authored"); err == nil {
		t.Fatal("expected rejection of a symlink inside a packaged skill, got nil")
	}
	if imgStore.Exists(image.Ref{Name: "leak-inner", Tag: "latest"}) {
		t.Fatal("leak-inner image must NOT have been built from a skill dir with an escaping inner symlink")
	}

	// Sanity: the outside secret content is NOT packed anywhere in the store.
	packed, _ := filepath.Glob(filepath.Join(imgStore.Dir, "leak-inner", "refs", "*.tar.gz"))
	for _, path := range packed {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "TOP-SECRET-KEY") {
			t.Fatal("outside secret content was packed into the built image")
		}
	}

	// A legitimate skill dir (no escaping symlink) still builds: drop the leak.
	if err := os.Remove(filepath.Join(inSkill, "leak")); err != nil {
		t.Fatal(err)
	}
	writeSF("./myskill")
	if _, err := buildImageForAgent(imgStore, workdir, "good-inner", "latest", "authored"); err != nil {
		t.Fatalf("legitimate in-workdir skill (no escaping symlink) must still build: %v", err)
	}
	if !imgStore.Exists(image.Ref{Name: "good-inner", Tag: "latest"}) {
		t.Fatal("good-inner image with a clean in-workdir skill was not stored")
	}
}

func TestReapOrphanSessions(t *testing.T) {
	base := t.TempDir()
	s, err := store.Open(filepath.Join(base, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	as := agent.NewStore(s)
	// live orphan session (no running iteration) -> reaped.
	_ = as.Create(agent.Agent{Name: "orphan", ImageRef: "basic:latest", Interactive: true})
	// iteration still running -> NOT reaped.
	_ = as.Create(agent.Agent{Name: "busy", ImageRef: "basic:latest", Interactive: true})
	_ = as.CreateIteration(agent.Iteration{ID: "busy-1", Agent: "busy", Status: "running", StartedAt: "t"})
	// a session for a currently-non-interactive agent is still an orphan (the flag
	// may have flipped since the session was created) -> reaped, flag-independent.
	_ = as.Create(agent.Agent{Name: "plain", ImageRef: "basic:latest", Interactive: false})

	var killed []string
	m := NewManager(ManagerConfig{
		AgentsDir: filepath.Join(base, "agents"), Store: as,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: time.Now,
		HasTmuxSession:  func(string) bool { return true },
		KillTmuxSession: func(sess string) error { killed = append(killed, sess); return nil },
	})
	agents, _ := as.List()
	m.reapOrphanSessions(agents, map[string][]<-chan struct{}{})

	if len(killed) != 2 || killed[0] != "orphan" || killed[1] != "plain" {
		t.Fatalf("killed = %v, want [orphan plain]", killed)
	}
}

func TestLiveState(t *testing.T) {
	r := &fakeRunner{}
	m, as, _, _ := newManager(t, r)
	a := agent.Agent{Name: "ls", ImageRef: "basic:latest", Enabled: true, LoopEnabled: true,
		HarnessType: "stub", Plugins: []string{"context"}}
	if err := as.Create(a); err != nil {
		t.Fatal(err)
	}

	assertLiveState(t, m, "ls", "idle") // loop_enabled, no engine

	a2, _ := as.Get("ls")
	a2.LoopEnabled = false
	if err := as.Update(a2); err != nil {
		t.Fatal(err)
	}
	// Master switch (Enabled) is still true; loop_enabled off alone reports
	// idle, not stopped — that's what TestLiveStateGatesOnEnabled locks in.
	assertLiveState(t, m, "ls", "idle") // loop off, master still enabled

	if err := as.SetError("ls", "boom"); err != nil {
		t.Fatal(err)
	}
	assertLiveState(t, m, "ls", "error") // error_reason wins
}

func TestLiveStateReportsAndClearsAIStall(t *testing.T) {
	r := &fakeRunner{}
	m, as, _, _ := newManager(t, r)
	now := time.Date(2026, 9, 10, 2, 10, 0, 0, time.UTC)
	m.cfg.Clock = func() time.Time { return now }
	a := agent.Agent{Name: "worker", ImageRef: "basic:latest", Enabled: true, LoopEnabled: true,
		HarnessType: "stub", AIStallTimeoutS: 300}
	if err := as.Create(a); err != nil {
		t.Fatal(err)
	}
	if err := as.CreateIteration(agent.Iteration{
		ID: "worker-1", Agent: "worker", Status: "running",
		StartedAt: now.Add(-6 * time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	m.adopting["worker"] = agentdir.LiveIteration{Agent: "worker", ID: "worker-1"}
	assertLiveState(t, m, "worker", "error")

	if err := as.RecordAIRequest("worker", "worker-1", now); err != nil {
		t.Fatal(err)
	}
	assertLiveState(t, m, "worker", "running")
	stored, _ := as.Get("worker")
	if !stored.LoopEnabled {
		t.Fatal("informational AI stall disabled the loop")
	}
}

func assertLiveState(t *testing.T, m *Manager, name, want string) {
	t.Helper()
	got, err := m.LiveState(name)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("LiveState(%s)=%q, want %q", name, got, want)
	}
}

func TestLiveStateGatesOnEnabled(t *testing.T) {
	r := &fakeRunner{}
	m, as, _, _ := newManager(t, r)
	// Enabled=false with loop_enabled=true must still be "stopped".
	if err := as.Create(agent.Agent{Name: "a", ImageRef: "basic:latest", HarnessType: "stub", LoopEnabled: true, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.LiveState("a"); st != "stopped" {
		t.Fatalf("disabled agent LiveState = %q, want stopped", st)
	}
	// Enabled=true, no live iteration → idle (even with loop off).
	if err := as.Create(agent.Agent{Name: "b", ImageRef: "basic:latest", HarnessType: "stub", LoopEnabled: false, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.LiveState("b"); st != "idle" {
		t.Fatalf("enabled idle agent LiveState = %q, want idle", st)
	}
}

func TestReapOrphanSessionIgnoresInteractiveFlag(t *testing.T) {
	r := &fakeRunner{}
	m, as, _, _ := newManager(t, r)
	killed := map[string]bool{}
	m.cfg.HasTmuxSession = func(s string) bool { return s == "manager" }
	m.cfg.KillTmuxSession = func(s string) error { killed[s] = true; return nil }
	if err := as.Create(agent.Agent{Name: "manager", ImageRef: "basic:latest", Interactive: false,
		HarnessType: "stub", Plugins: []string{"context"}}); err != nil {
		t.Fatal(err)
	}
	m.reapOrphanSessions([]agent.Agent{{Name: "manager", Interactive: false}}, map[string][]<-chan struct{}{})
	if !killed["manager"] {
		t.Fatal("orphan session for a non-interactive-flagged agent was not reaped")
	}
}

// mustWrite writes content to path, creating parent dirs, failing the test on error.
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedLeakedRows inserts one row per agent-keyed side table PurgeAgentData must
// clean — the rows a plain Store.Delete leaks today. deliveries hangs off the
// subscription so we can prove the dependent rows go too.
func seedLeakedRows(t *testing.T, s *store.Store, name string) {
	t.Helper()
	exec := func(q string, args ...any) {
		if _, err := s.DB.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO subscriptions(id, agent, channel) VALUES(?,?,?)`, "sub-1", name, "ch")
	exec(`INSERT INTO deliveries(subscription_id, message_id) VALUES(?,?)`, "sub-1", "msg-1")
	exec(`INSERT INTO schedules(id, agent, kind, spec, channel, next_fire_at) VALUES(?,?,?,?,?,?)`,
		"sch-1", name, "oneshot", "2026-07-13T00:00:00Z", "ch", "2026-07-13T00:00:00Z")
	exec(`INSERT INTO scripts(id, agent, name, description, command, mode, state, created_at) VALUES(?,?,?,?,?,?,?,?)`,
		"scr-1", name, "s", "test", "echo hi", "once", "completed", "2026-07-13T00:00:00Z")
	exec(`INSERT INTO script_result_outbox(idempotency_key,script_id,run_id,agent,payload,next_attempt_at) VALUES(?,?,?,?,?,?)`,
		"script-result:srun-1", "scr-1", "srun-1", name, `{"script_id":"scr-1","run_id":"srun-1"}`, "2026-07-13T00:00:00Z")
	exec(`INSERT INTO ai_requests(id, ts, agent) VALUES(?,?,?)`, "air-1", "2026-07-13T00:00:00Z", name)
	exec(`INSERT INTO budgets(scope) VALUES(?)`, "agent:"+name)
	exec(`INSERT INTO retention_policies(agent) VALUES(?)`, name)
}

// leakedRowCount totals the seeded side-table rows still attributed to the agent.
func leakedRowCount(t *testing.T, s *store.Store, name string) int {
	t.Helper()
	total := 0
	count := func(q string, arg any) {
		var n int
		if err := s.DB.QueryRow(q, arg).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		total += n
	}
	count(`SELECT COUNT(*) FROM subscriptions WHERE agent=?`, name)
	count(`SELECT COUNT(*) FROM deliveries WHERE subscription_id IN (SELECT id FROM subscriptions WHERE agent=?)`, name)
	count(`SELECT COUNT(*) FROM schedules WHERE agent=?`, name)
	count(`SELECT COUNT(*) FROM scripts WHERE agent=?`, name)
	count(`SELECT COUNT(*) FROM script_result_outbox WHERE agent=?`, name)
	count(`SELECT COUNT(*) FROM ai_requests WHERE agent=?`, name)
	count(`SELECT COUNT(*) FROM budgets WHERE scope=?`, "agent:"+name)
	count(`SELECT COUNT(*) FROM retention_policies WHERE agent=?`, name)
	return total
}

// TestRemovePreserveThenPurge locks in option B: a preserving Remove keeps the
// agent row (stopped) and every durable artifact while dropping only the
// rebuildable tree; a purging Remove hard-deletes everything AND the leaked
// agent-keyed side-table rows.
func TestRemovePreserveThenPurge(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, agentsDir, s := newManager(t, r)
	defer m.Shutdown()

	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "keep",
		Harness: "stub", Plugins: []string{"context"}, Loop: true})
	if err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, name)

	// Durable artifacts an agent accrues over its life, plus a DB iteration row.
	mustWrite(t, l.ContextPath(), "remember me")
	mustWrite(t, l.AuditLog(), "{\"type\":\"status\"}\n")
	if err := os.MkdirAll(l.IterationDir("iter-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := as.CreateIteration(agent.Iteration{ID: "iter-1", Agent: name,
		Trigger: "manual", Status: "done", StartedAt: "2026-07-13T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	seedLeakedRows(t, s, name)
	wantPreservedRows := leakedRowCount(t, s, name)

	// --- Preserve (plain down): keep data, drop only the rebuildable tree. ---
	if err := m.Remove(name, true, false); err != nil {
		t.Fatalf("preserve remove: %v", err)
	}
	if ok, _ := as.Exists(name); !ok {
		t.Fatal("preserve remove dropped the agents row")
	}
	if got, _ := as.Get(name); got.LoopEnabled {
		t.Fatal("preserve remove left loop enabled; want stopped")
	}
	if st, _ := m.LiveState(name); st != "stopped" {
		t.Fatalf("after preserve remove LiveState = %q, want stopped", st)
	}
	for _, p := range []string{l.ContextPath(), l.AuditLog(), l.IterationDir("iter-1")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("preserve remove dropped %s: %v", p, err)
		}
	}
	if its, _ := as.ListIterations(name); len(its) != 1 {
		t.Fatalf("preserve remove dropped iteration rows: got %d, want 1", len(its))
	}
	if n := leakedRowCount(t, s, name); n != wantPreservedRows {
		t.Fatalf("preserve remove must keep side-table rows: got %d, want %d", n, wantPreservedRows)
	}
	for _, d := range []string{l.ImageDir(), l.BinDir(), l.Workdir()} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("preserve remove kept rebuildable dir %s (err=%v)", d, err)
		}
	}

	// --- Purge (down --volumes): hard-delete everything + leaked rows. ---
	if err := m.Remove(name, true, true); err != nil {
		t.Fatalf("purge remove: %v", err)
	}
	if ok, _ := as.Exists(name); ok {
		t.Fatal("purge remove kept the agents row")
	}
	if its, _ := as.ListIterations(name); len(its) != 0 {
		t.Fatalf("purge remove kept iteration rows: %d", len(its))
	}
	if _, err := os.Stat(l.Root); !os.IsNotExist(err) {
		t.Fatalf("purge remove kept the durable tree (err=%v)", err)
	}
	if n := leakedRowCount(t, s, name); n != 0 {
		t.Fatalf("purge remove left %d leaked side-table rows", n)
	}
}

// TestReprovisionKeepsDataAndSwapsImage proves the up-side counterpart: after a
// preserving down, Reprovision re-unpacks the (new) image over the retained
// CONTEXT.md / iterations / audit and restarts the loop.
func TestReprovisionKeepsDataAndSwapsImage(t *testing.T) {
	r := &fakeRunner{outcomes: []Outcome{{Status: "done", DoneFlag: true}}}
	m, as, agentsDir, _ := newManager(t, r)
	defer m.Shutdown()

	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "keep",
		Harness: "stub", Plugins: []string{"context"}, Loop: true})
	if err != nil {
		t.Fatal(err)
	}
	// Run() creates the agent disabled (Task 4); enable it in the store so the
	// preserving Remove + Reprovision round trip below brings a running loop
	// back up (Enabled is not itself touched by Remove/Reprovision — that is
	// Task 6's job, not this test's concern).
	got0, err := as.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	got0.Enabled = true
	if err := as.Update(got0); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, name)
	mustWrite(t, l.ContextPath(), "remember me")
	mustWrite(t, l.AuditLog(), "{\"type\":\"status\"}\n")
	mustWrite(t, filepath.Join(l.ImageDir(), "old-marker"), "stale")

	// A second image version to swap onto.
	buildBasic2(t, m.cfg.ImgStore)

	if err := m.Remove(name, true, false); err != nil {
		t.Fatalf("preserve remove: %v", err)
	}
	if _, err := os.Stat(l.ImageDir()); !os.IsNotExist(err) {
		t.Fatalf("preserve remove kept the image tree (err=%v)", err)
	}
	var goalStates []string
	m.cfg.GoalSignal = func() {
		ag, err := as.Get(name)
		if err == nil {
			goalStates = append(goalStates, fmt.Sprintf("enabled=%v loop=%v", ag.Enabled, ag.LoopEnabled))
		}
	}

	if err := m.Reprovision(name, "basic2:latest"); err != nil {
		t.Fatalf("reprovision: %v", err)
	}
	if len(goalStates) != 1 || goalStates[0] != "enabled=true loop=true" {
		t.Fatalf("states at reprovision goal signals = %v, want [enabled=true loop=true]", goalStates)
	}
	// Tree re-unpacked, shims rewritten.
	if _, err := os.Stat(filepath.Join(l.ImageDir(), "prompt", "template.json")); err != nil {
		t.Fatalf("reprovision did not re-unpack the image: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.BinDir(), "tools")); !os.IsNotExist(err) {
		t.Fatalf("reprovision restored the removed central tools shim: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.ImageDir(), "old-marker")); !os.IsNotExist(err) {
		t.Fatalf("reprovision retained stale image content: %v", err)
	}
	// New image recorded on the row.
	got, _ := as.Get(name)
	if got.ImageRef != "basic2:latest" {
		t.Fatalf("reprovision image_ref = %q, want basic2:latest", got.ImageRef)
	}
	// History kept.
	if data, err := os.ReadFile(l.ContextPath()); err != nil || string(data) != "remember me" {
		t.Fatalf("reprovision clobbered CONTEXT.md: %q err=%v", data, err)
	}
	if _, err := os.Stat(l.AuditLog()); err != nil {
		t.Fatalf("reprovision dropped audit.jsonl: %v", err)
	}
	// Loop back up.
	if st, _ := m.LiveState(name); st != "idle" {
		t.Fatalf("after reprovision LiveState = %q, want idle (loop re-enabled)", st)
	}
}

func TestReprovisionContractFailurePreservesIdentityAndTree(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{})
	defer m.Shutdown()
	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "keep", Harness: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := as.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, name)
	marker := filepath.Join(l.ImageDir(), "old-marker")
	if err := os.WriteFile(marker, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	buildContractInvalidImage(t, m.cfg.ImgStore, "invalid")

	if err := m.Reprovision(name, "invalid:latest"); err == nil || !strings.Contains(err.Error(), `plugin "loop" requires packaged skill "loop"`) {
		t.Fatalf("Reprovision error = %v", err)
	}
	after, err := as.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	if after.ImageRef != before.ImageRef || after.ImageDigest != before.ImageDigest {
		t.Fatalf("identity changed from %s@%s to %s@%s", before.ImageRef, before.ImageDigest, after.ImageRef, after.ImageDigest)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "old" {
		t.Fatalf("old image tree was not preserved: body=%q err=%v", body, err)
	}
}

func TestReprovisionReplacesRatherThanOverlaysImageTree(t *testing.T) {
	m, _, agentsDir, _ := newManager(t, &fakeRunner{})
	defer m.Shutdown()
	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "replace", Harness: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, name)
	marker := filepath.Join(l.ImageDir(), "old-marker")
	if err := os.WriteFile(marker, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	buildBasic2(t, m.cfg.ImgStore)

	if err := m.Reprovision(name, "basic2:latest"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("reprovision retained stale image content: %v", err)
	}
	shim, err := os.ReadFile(filepath.Join(l.BinDir(), "i-am-done"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shim), filepath.Join(l.ImageDir(), "skills", "loop")) || strings.Contains(string(shim), ".reprovision-") {
		t.Fatalf("reprovisioned shim points outside active image: %s", shim)
	}
}

func TestReprovisionBridgeFailurePreservesIdentityAndTree(t *testing.T) {
	m, as, agentsDir, _ := newManager(t, &fakeRunner{})
	defer m.Shutdown()
	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "bridge", Harness: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := as.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	before.HarnessType = "codex"
	if err := as.Update(before); err != nil {
		t.Fatal(err)
	}
	l := agentdir.New(agentsDir, name)
	marker := filepath.Join(l.ImageDir(), "old-marker")
	if err := os.WriteFile(marker, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	shimBefore, err := os.ReadFile(filepath.Join(l.BinDir(), "i-am-done"))
	if err != nil {
		t.Fatal(err)
	}
	buildBasic2(t, m.cfg.ImgStore)
	m.cfg.PrepareImageBridge = func(string, string, []image.ManifestSkill, agentdir.BridgePlan) error {
		return errors.New("bridge rejected")
	}

	if err := m.Reprovision(name, "basic2:latest"); err == nil || !strings.Contains(err.Error(), "bridge rejected") {
		t.Fatalf("Reprovision error = %v", err)
	}
	after, err := as.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	if after.ImageRef != before.ImageRef || after.ImageDigest != before.ImageDigest {
		t.Fatalf("identity changed from %s@%s to %s@%s", before.ImageRef, before.ImageDigest, after.ImageRef, after.ImageDigest)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "old" {
		t.Fatalf("old image tree was not preserved: body=%q err=%v", body, err)
	}
	if shimAfter, err := os.ReadFile(filepath.Join(l.BinDir(), "i-am-done")); err != nil || string(shimAfter) != string(shimBefore) {
		t.Fatalf("old shim changed: body=%q err=%v", shimAfter, err)
	}
}

// buildBasic2 builds a second image "basic2:latest" for image-swap tests.
func buildBasic2(t *testing.T, st *image.Store) {
	buildTestImage(t, st, "basic2", "BODY2")
}

// A schedule the agent arms for itself carries no channel, and its wake is
// service traffic: it belongs in the agent's service chat, not in the inbox.
func TestAgentScheduleWithoutChannelWakesTheServiceChat(t *testing.T) {
	m, as, agentsDir, st := newManager(t, &fakeRunner{})
	m.cfg.Schedules = schedule.NewStore(st, time.Now)
	name, err := m.Run(registry.RunSpec{ImageRef: "basic:latest", Name: "worker", Harness: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := as.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	ag.Plugins = append(ag.Plugins, "schedule")
	if err := as.Update(ag); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/tools/schedule/add",
		strings.NewReader(`{"kind":"cron","spec":"*/5 * * * *"}`))
	m.newToolsAPIServer(ag, agentdir.New(agentsDir, name)).Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("schedule add status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	want := bus.ChatChannelFor(bus.ChatIDService(name))
	if !strings.Contains(recorder.Body.String(), want) {
		t.Fatalf("schedule channel is not the service chat %q: %s", want, recorder.Body.String())
	}
}
