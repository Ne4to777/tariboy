package loop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/tasks"
)

func TestManagerReportsTheIterationEndBeforeTheGoalWake(t *testing.T) {
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	finish := start.Add(5 * time.Minute)
	var calls []string
	var ends []tasks.IterationEnd
	m := NewManager(ManagerConfig{
		Clock: func() time.Time { return finish },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		RecordIterationEnd: func(_ context.Context, end tasks.IterationEnd) error {
			calls = append(calls, "end")
			ends = append(ends, end)
			return errors.New("logged, not fatal")
		},
		IterationCompleted: func(string, string) { calls = append(calls, "completed") },
	})
	runner := m.runnerFor(agent.Agent{}).(*ShimRunner)
	runner.cfg.GoalRead("alice", "alice-1", "DEV-1", start)

	m.iterationCompleted("alice", "alice-1")
	want := tasks.IterationEnd{Agent: "alice", IterationID: "alice-1", GoalTaskKey: "DEV-1", StartedAt: start, FinishedAt: finish}
	if strings.Join(calls, ",") != "end,completed" || len(ends) != 1 || ends[0] != want {
		t.Fatalf("calls = %v ends = %#v; want the end %#v before the goal wake", calls, ends, want)
	}
	// The Goal is reported once: another completion of the same iteration, or
	// of an iteration the runner did not prepare, ran with no known Goal.
	m.iterationCompleted("alice", "alice-1")
	runner.cfg.GoalRead("alice", "alice-2", "DEV-1", start)
	m.iterationCompleted("alice", "alice-3")
	if len(ends) != 3 || ends[1].GoalTaskKey != "" || ends[2].GoalTaskKey != "" || ends[2].IterationID != "alice-3" {
		t.Fatalf("ends = %#v", ends)
	}
}

func TestManagerReportsAGoalSelectedDuringTheIteration(t *testing.T) {
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	var ends []tasks.IterationEnd
	m := NewManager(ManagerConfig{
		Clock: func() time.Time { return start.Add(time.Minute) },
		RecordIterationEnd: func(_ context.Context, end tasks.IterationEnd) error {
			ends = append(ends, end)
			return nil
		},
	})
	m.noteIterationGoal("alice", "alice-1", "", start)
	m.noteSelectedGoal("alice", "alice-1", "DEV-2")
	// A Goal already attached is not replaced.
	m.noteSelectedGoal("alice", "alice-1", "DEV-3")
	m.iterationCompleted("alice", "alice-1")
	if len(ends) != 1 || ends[0].GoalTaskKey != "DEV-2" || !ends[0].StartedAt.Equal(start) {
		t.Fatalf("ends = %#v", ends)
	}
}
