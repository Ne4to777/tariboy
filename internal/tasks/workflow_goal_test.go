package tasks_test

import (
	"context"
	"testing"
	"time"

	basestore "github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/taskgoal"
	"github.com/alekzonder/tariboy/internal/tasks"
)

// A holder that hands its task to a customer status is released by the goal
// reconciler at once, so dispatch can give it other work.
func TestHolderIsFreeOnceItsTaskWaitsInACustomerStatus(t *testing.T) {
	ctx := context.Background()
	svc, actor, task := tasks.RequestFixture(t)
	db := tasks.ServiceDB(svc)
	goals := taskgoal.NewStore(&basestore.Store{DB: db})
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	if goal, err := goals.ReconcileAgent("dev-1", now); err != nil || goal.TaskKey != task.Key {
		t.Fatalf("initial goal = %#v, %v", goal, err)
	}
	if _, err := db.Exec(`UPDATE agents SET enabled = 0 WHERE name = 'dev-2'`); err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateTask(ctx, actor, tasks.CreateTaskInput{Queue: "DEV", Title: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != tasks.StatusOpen || other.Assignee != "" {
		t.Fatalf("other = %#v; want open and unassigned while dev-1 is busy", other)
	}

	if _, err := svc.Advance(ctx, tasks.AgentActor("dev-1"), task.Key, tasks.AdvanceInput{Outcome: "ask"}); err != nil {
		t.Fatal(err)
	}
	goal, err := goals.ReconcileAgent("dev-1", now)
	if err != nil || goal.TaskKey != "" || goal.Waiting {
		t.Fatalf("goal after customer handoff = %#v, %v; want released", goal, err)
	}
	var current string
	if err := db.QueryRow(`SELECT current_goal_task_key FROM agents WHERE name = 'dev-1'`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != "" {
		t.Fatalf("current_goal_task_key = %q; want cleared", current)
	}
	if n, err := svc.DispatchPending(ctx); err != nil || n != 1 {
		t.Fatalf("dispatch = %d, %v; want 1", n, err)
	}
	stored, err := svc.GetTask(ctx, actor, other.Key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Task.Assignee != "agent:dev-1" || stored.Task.Status != tasks.StatusInProgress {
		t.Fatalf("other after dispatch = %#v; want held by dev-1", stored.Task)
	}
}
