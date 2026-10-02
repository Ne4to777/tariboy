package taskgoal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	basestore "github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
)

var goalNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func TestSetSelectsOnlyActiveAssignedGoal(t *testing.T) {
	s := goalStore(t, goalNow)
	seedTask(t, s, "T-1", "agent:worker", "P2", "open", "2026-09-01T00:00:00Z")
	seedTask(t, s, "T-2", "agent:other", "P2", "open", "2026-09-01T00:00:00Z")
	seedTask(t, s, "T-3", "agent:worker", "P2", "done", "2026-09-01T00:00:00Z")

	task, err := s.Set("worker", "T-1", goalNow, func() (func(), error) { return nil, nil })
	if err != nil || task.Key != "T-1" {
		t.Fatalf("Set active assigned goal: task=%#v err=%v", task, err)
	}
	assertStoredGoal(t, s, "T-1")

	for _, key := range []string{"T-2", "T-3", "T-404"} {
		if _, err := s.Set("worker", key, goalNow, func() (func(), error) { return nil, nil }); err == nil {
			t.Errorf("Set(%s) succeeded, want error", key)
		}
		assertStoredGoal(t, s, "T-1")
	}
}

func TestSetDoesNotReplaceExistingGoal(t *testing.T) {
	s := goalStore(t, goalNow)
	seedTask(t, s, "T-1", "agent:worker", "P2", "open", "2026-09-01T00:00:00Z")
	seedTask(t, s, "T-2", "agent:worker", "P2", "open", "2026-09-01T00:00:00Z")
	assertSet := func(key string) error {
		_, err := s.Set("worker", key, goalNow, func() (func(), error) { return nil, nil })
		return err
	}
	if err := assertSet("T-1"); err != nil {
		t.Fatal(err)
	}
	if err := assertSet("T-1"); err != nil {
		t.Fatalf("idempotent set: %v", err)
	}
	if err := assertSet("T-2"); err == nil {
		t.Fatal("replacement set succeeded")
	}
	assertStoredGoal(t, s, "T-1")
}

func TestSetRollsBackWhenLeaseActivationFails(t *testing.T) {
	s := goalStore(t, goalNow)
	seedTask(t, s, "T-1", "agent:worker", "P2", "open", "2026-09-01T00:00:00Z")
	if _, err := s.Set("worker", "T-1", goalNow, func() (func(), error) {
		return nil, context.Canceled
	}); err == nil {
		t.Fatal("Set succeeded")
	}
	assertStoredGoal(t, s, "")
}

func TestReconcileAgentPreemptsStickyGoalForHigherPriority(t *testing.T) {
	s := goalStore(t, goalNow)
	seedTask(t, s, "T-2", "agent:worker", "P2", "in_progress", "2026-09-02T00:00:00Z")

	goal, err := s.ReconcileAgent("worker", goalNow)
	if err != nil || goal.TaskKey != "T-2" || goal.Reason != "selected" || goal.Waiting {
		t.Fatalf("goal=%#v err=%v", goal, err)
	}

	seedTask(t, s, "T-1", "agent:worker", "P1", "open", "2026-09-01T00:00:00Z")
	goal, err = s.ReconcileAgent("worker", goalNow)
	if err != nil || goal.TaskKey != "T-1" {
		t.Fatalf("preempted goal=%#v err=%v", goal, err)
	}
	assertStoredGoal(t, s, "T-1")
}

func TestReconcileAgentKeepsStickyGoalForEqualOrLowerPriority(t *testing.T) {
	s := goalStore(t, goalNow)
	seedTask(t, s, "T-1", "agent:worker", "P1", "open", "2026-09-02T00:00:00Z")
	if goal, err := s.ReconcileAgent("worker", goalNow); err != nil || goal.TaskKey != "T-1" {
		t.Fatalf("initial goal=%#v err=%v", goal, err)
	}

	seedTask(t, s, "T-2", "agent:worker", "P1", "in_progress", "2026-09-01T00:00:00Z")
	goal, err := s.ReconcileAgent("worker", goalNow)
	if err != nil || goal.TaskKey != "T-1" {
		t.Fatalf("sticky goal=%#v err=%v", goal, err)
	}

	seedTask(t, s, "T-3", "agent:worker", "P2", "in_progress", "2026-08-01T00:00:00Z")
	goal, err = s.ReconcileAgent("worker", goalNow)
	if err != nil || goal.TaskKey != "T-1" {
		t.Fatalf("sticky goal with lower priority candidate=%#v err=%v", goal, err)
	}
	assertStoredGoal(t, s, "T-1")
}

func TestReconcileAgentSkipsDependencyBlockedCandidate(t *testing.T) {
	s := goalStore(t, goalNow)
	seedTask(t, s, "T-blocker", "agent:other", "P0", "in_progress", "2026-09-01T00:00:00Z")
	seedTask(t, s, "T-blocked", "agent:worker", "P0", "in_progress", "2026-09-01T00:00:00Z")
	seedTask(t, s, "T-ready", "agent:worker", "P1", "in_progress", "2026-09-02T00:00:00Z")
	seedBlockingRelation(t, s, "T-blocker", "T-blocked")

	goal, err := s.ReconcileAgent("worker", goalNow)
	if err != nil || goal.TaskKey != "T-ready" {
		t.Fatalf("goal=%#v err=%v, want unblocked task", goal, err)
	}
}

func TestReconcileAgentReleasesBlockedStickyGoal(t *testing.T) {
	s := goalStore(t, goalNow)
	seedTask(t, s, "T-1", "agent:worker", "P0", "in_progress", "2026-09-01T00:00:00Z")
	seedTask(t, s, "T-2", "agent:worker", "P1", "in_progress", "2026-09-02T00:00:00Z")
	if goal, err := s.ReconcileAgent("worker", goalNow); err != nil || goal.TaskKey != "T-1" {
		t.Fatalf("initial goal=%#v err=%v", goal, err)
	}

	execGoalSQL(t, s, `UPDATE tasks SET manual_block_reason='waiting' WHERE task_key='T-1'`)
	goal, err := s.ReconcileAgent("worker", goalNow)
	if err != nil || goal.TaskKey != "T-2" {
		t.Fatalf("replacement goal=%#v err=%v, want unblocked task", goal, err)
	}
}

func TestReconcileAgentBlockedHigherPriorityDoesNotPreempt(t *testing.T) {
	s := goalStore(t, goalNow)
	seedTask(t, s, "T-current", "agent:worker", "P2", "in_progress", "2026-09-02T00:00:00Z")
	if goal, err := s.ReconcileAgent("worker", goalNow); err != nil || goal.TaskKey != "T-current" {
		t.Fatalf("initial goal=%#v err=%v", goal, err)
	}

	seedTask(t, s, "T-blocker", "agent:other", "P0", "in_progress", "2026-09-01T00:00:00Z")
	seedTask(t, s, "T-blocked", "agent:worker", "P0", "in_progress", "2026-09-01T00:00:00Z")
	seedBlockingRelation(t, s, "T-blocker", "T-blocked")
	goal, err := s.ReconcileAgent("worker", goalNow)
	if err != nil || goal.TaskKey != "T-current" {
		t.Fatalf("goal=%#v err=%v, want sticky unblocked task", goal, err)
	}
}

func TestReconcileAgentOrdersCandidates(t *testing.T) {
	tests := []struct {
		name  string
		tasks []goalTask
		want  string
	}{
		{
			name: "priority",
			tasks: []goalTask{
				{key: "T-p1", priority: "P1", status: "in_progress", createdAt: "2026-09-01T00:00:00Z"},
				{key: "T-p0", priority: "P0", status: "open", createdAt: "2026-09-02T00:00:00Z"},
			},
			want: "T-p0",
		},
		{
			name: "status",
			tasks: []goalTask{
				{key: "T-open", priority: "P1", status: "open", createdAt: "2026-09-01T00:00:00Z"},
				{key: "T-progress", priority: "P1", status: "in_progress", createdAt: "2026-09-02T00:00:00Z"},
			},
			want: "T-progress",
		},
		{
			name: "created_at",
			tasks: []goalTask{
				{key: "T-new", priority: "P1", status: "open", createdAt: "2026-09-02T00:00:00Z"},
				{key: "T-old", priority: "P1", status: "open", createdAt: "2026-09-01T00:00:00Z"},
			},
			want: "T-old",
		},
		{
			name: "task_key",
			tasks: []goalTask{
				{key: "T-2", priority: "P1", status: "open", createdAt: "2026-09-01T00:00:00Z"},
				{key: "T-1", priority: "P1", status: "open", createdAt: "2026-09-01T00:00:00Z"},
			},
			want: "T-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := goalStore(t, goalNow)
			for _, task := range tt.tasks {
				seedTask(t, s, task.key, "agent:worker", task.priority, task.status, task.createdAt)
			}
			goal, err := s.ReconcileAgent("worker", goalNow)
			if err != nil || goal.TaskKey != tt.want || goal.Reason != "selected" || goal.Revision != 1 {
				t.Fatalf("goal=%#v err=%v, want key %q revision 1", goal, err, tt.want)
			}
		})
	}
}

func TestReconcileAgentIgnoresPullRequest(t *testing.T) {
	t.Run("candidate", func(t *testing.T) {
		base := openGoalStore(t)
		s := NewStore(base)
		seedAgent(t, s, "worker", goalNow.Add(-time.Hour))
		svc := tasks.NewService(base.DB, "customer", func() time.Time { return goalNow })
		actor := tasks.CustomerActor("customer")
		if _, err := svc.CreateQueue(context.Background(), actor, tasks.CreateQueueInput{Prefix: "GOAL", Name: "Tasks"}); err != nil {
			t.Fatal(err)
		}
		task, err := svc.CreateTask(context.Background(), actor, tasks.CreateTaskInput{Queue: "GOAL", Title: "candidate", Assignee: "worker"})
		if err != nil {
			t.Fatal(err)
		}
		status := tasks.StatusInProgress
		pullRequest := "https://example.test/pull/1"
		if _, err := svc.UpdateTask(context.Background(), actor, task.Key, tasks.UpdateTaskInput{Status: &status, PullRequest: &pullRequest, Revision: task.Revision}); err != nil {
			t.Fatal(err)
		}

		goal, err := s.ReconcileAgent("worker", goalNow)
		if err != nil || goal.TaskKey != task.Key {
			t.Fatalf("goal=%#v err=%v, want task with pull request", goal, err)
		}
	})

	t.Run("sticky", func(t *testing.T) {
		s := goalStore(t, goalNow)
		seedTask(t, s, "T-1", "agent:worker", "P1", "in_progress", "2026-09-01T00:00:00Z")
		if goal, err := s.ReconcileAgent("worker", goalNow); err != nil || goal.TaskKey != "T-1" {
			t.Fatalf("initial goal=%#v err=%v", goal, err)
		}

		updateTask(t, s, "T-1", "pull_request", "https://example.test/pull/1")
		goal, err := s.ReconcileAgent("worker", goalNow)
		if err != nil || goal.TaskKey != "T-1" {
			t.Fatalf("goal=%#v err=%v, want sticky task with pull request", goal, err)
		}
	})

	t.Run("higher_priority_candidate", func(t *testing.T) {
		s := goalStore(t, goalNow)
		seedTask(t, s, "T-2", "agent:worker", "P2", "in_progress", "2026-09-02T00:00:00Z")
		if goal, err := s.ReconcileAgent("worker", goalNow); err != nil || goal.TaskKey != "T-2" {
			t.Fatalf("initial goal=%#v err=%v", goal, err)
		}

		seedTask(t, s, "T-1", "agent:worker", "P1", "in_progress", "2026-09-01T00:00:00Z")
		updateTask(t, s, "T-1", "pull_request", "https://example.test/pull/1")
		goal, err := s.ReconcileAgent("worker", goalNow)
		if err != nil || goal.TaskKey != "T-1" {
			t.Fatalf("goal=%#v err=%v, want higher-priority task with pull request", goal, err)
		}
	})
}

func TestReconcileAgentReleasesInvalidStickyGoal(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Store)
		want   string
	}{
		{name: "done", mutate: func(t *testing.T, s *Store) { updateTask(t, s, "T-1", "status", "done") }, want: "T-2"},
		{name: "cancelled", mutate: func(t *testing.T, s *Store) { updateTask(t, s, "T-1", "status", "cancelled") }, want: "T-2"},
		{name: "deleted", mutate: func(t *testing.T, s *Store) { execGoalSQL(t, s, `DELETE FROM tasks WHERE task_key='T-1'`) }, want: "T-2"},
		{name: "reassigned", mutate: func(t *testing.T, s *Store) { updateTask(t, s, "T-1", "assignee", "agent:other") }, want: "T-2"},
		{name: "goal_disabled", mutate: func(t *testing.T, s *Store) {
			execGoalSQL(t, s, `UPDATE agents SET goal_enabled=0 WHERE name='worker'`)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := goalStore(t, goalNow)
			seedTask(t, s, "T-1", "agent:worker", "P0", "in_progress", "2026-09-01T00:00:00Z")
			seedTask(t, s, "T-2", "agent:worker", "P1", "in_progress", "2026-09-02T00:00:00Z")
			if goal, err := s.ReconcileAgent("worker", goalNow); err != nil || goal.TaskKey != "T-1" {
				t.Fatalf("initial goal=%#v err=%v", goal, err)
			}

			tt.mutate(t, s)
			goal, err := s.ReconcileAgent("worker", goalNow)
			if err != nil || goal.TaskKey != tt.want {
				t.Fatalf("goal=%#v err=%v, want key %q", goal, err, tt.want)
			}
			assertStoredGoal(t, s, tt.want)
		})
	}
}

func TestReconcileAgentPreservesStickyGoalWhileAgentCannotRun(t *testing.T) {
	tests := []struct {
		name     string
		disable  string
		reenable string
	}{
		{name: "agent_disabled", disable: `UPDATE agents SET enabled=0 WHERE name='worker'`, reenable: `UPDATE agents SET enabled=1 WHERE name='worker'`},
		{name: "loop_disabled", disable: `UPDATE agents SET loop_enabled=0 WHERE name='worker'`, reenable: `UPDATE agents SET loop_enabled=1 WHERE name='worker'`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := goalStore(t, goalNow)
			seedTask(t, s, "T-1", "agent:worker", "P0", "in_progress", "2026-09-01T00:00:00Z")
			seedTask(t, s, "T-2", "agent:worker", "P1", "in_progress", "2026-09-02T00:00:00Z")
			if goal, err := s.ReconcileAgent("worker", goalNow); err != nil || goal.TaskKey != "T-1" {
				t.Fatalf("initial goal=%#v err=%v", goal, err)
			}

			execGoalSQL(t, s, tt.disable)
			goal, err := s.ReconcileAgent("worker", goalNow)
			if err != nil || goal.TaskKey != "" {
				t.Fatalf("disabled goal=%#v err=%v", goal, err)
			}
			assertStoredGoal(t, s, "T-1")

			updateTask(t, s, "T-1", "status", "done")
			execGoalSQL(t, s, tt.reenable)
			goal, err = s.ReconcileAgent("worker", goalNow)
			if err != nil || goal.TaskKey != "T-2" {
				t.Fatalf("re-enabled goal=%#v err=%v", goal, err)
			}
			assertStoredGoal(t, s, "T-2")
		})
	}
}

func TestReconcileAgentAppliesCustomerWaitBoundary(t *testing.T) {
	tests := []struct {
		name     string
		waitAge  time.Duration
		seedWait bool
		answered bool
		wantKey  string
		wantWait bool
	}{
		{name: "299_seconds", waitAge: 299 * time.Second, seedWait: true, wantKey: "T-1", wantWait: true},
		{name: "300_seconds", waitAge: 300 * time.Second, seedWait: true},
		{name: "wait_customer_without_unresolved_wait"},
		{name: "answered_wait_returns_to_in_progress", waitAge: time.Hour, seedWait: true, answered: true, wantKey: "T-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := goalStore(t, goalNow)
			seedTask(t, s, "T-1", "agent:worker", "P1", "in_progress", "2026-09-01T00:00:00Z")
			if goal, err := s.ReconcileAgent("worker", goalNow); err != nil || goal.TaskKey != "T-1" {
				t.Fatalf("initial goal=%#v err=%v", goal, err)
			}

			updateTask(t, s, "T-1", "status", "wait_customer")
			if tt.seedWait {
				seedCustomerWait(t, s, "T-1", goalNow.Add(-tt.waitAge), tt.answered)
			}
			if tt.answered {
				updateTask(t, s, "T-1", "status", "in_progress")
			}

			goal, err := s.ReconcileAgent("worker", goalNow)
			if err != nil || goal.TaskKey != tt.wantKey || goal.Waiting != tt.wantWait {
				t.Fatalf("goal=%#v err=%v, want key %q waiting %t", goal, err, tt.wantKey, tt.wantWait)
			}
			assertStoredGoal(t, s, tt.wantKey)
		})
	}
}

func TestReconcileAgentReleasesTaskWaitingOnWorkflowCustomerStatus(t *testing.T) {
	tests := []struct {
		name      string
		workflow  bool
		requester string
		wantKey   string
	}{
		{name: "workflow_status_wait_releases", workflow: true, requester: "system:workflow"},
		{name: "workflow_holder_question_keeps_grace", workflow: true, requester: "agent:worker", wantKey: "T-1"},
		{name: "flexible_question_keeps_grace", requester: "agent:worker", wantKey: "T-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := goalStore(t, goalNow)
			seedTask(t, s, "T-1", "agent:worker", "P1", "in_progress", "2026-09-01T00:00:00Z")
			if tt.workflow {
				execGoalSQL(t, s, `INSERT INTO task_workflow_images(digest,name,version,manifest,built_at) VALUES ('d1','flow','1.0.0','{}','now')`)
				execGoalSQL(t, s, `UPDATE tasks SET workflow_digest='d1', workflow_status='build' WHERE task_key='T-1'`)
			}
			if goal, err := s.ReconcileAgent("worker", goalNow); err != nil || goal.TaskKey != "T-1" {
				t.Fatalf("initial goal=%#v err=%v", goal, err)
			}
			updateTask(t, s, "T-1", "status", "wait_customer")
			seedCustomerWaitBy(t, s, "T-1", tt.requester, goalNow.Add(-time.Second), false)

			goal, err := s.ReconcileAgent("worker", goalNow)
			if err != nil || goal.TaskKey != tt.wantKey || goal.Waiting != (tt.wantKey != "") {
				t.Fatalf("goal=%#v err=%v, want key %q", goal, err, tt.wantKey)
			}
			assertStoredGoal(t, s, tt.wantKey)
		})
	}
}

func TestRepeatedCustomerQuestionKeepsOldestWaitGraceBoundary(t *testing.T) {
	base := openGoalStore(t)
	goals := NewStore(base)
	askedAt := goalNow.Add(-300 * time.Second)
	now := askedAt
	seedAgent(t, goals, "worker", askedAt.Add(-time.Hour))
	svc := tasks.NewService(base.DB, "customer", func() time.Time { return now })
	actor := tasks.CustomerActor("customer")
	if _, err := svc.CreateQueue(context.Background(), actor, tasks.CreateQueueInput{Prefix: "GOAL", Name: "Tasks"}); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(context.Background(), actor, tasks.CreateTaskInput{Queue: "GOAL", Title: "decision", Assignee: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	status := tasks.StatusInProgress
	task, err = svc.UpdateTask(context.Background(), actor, task.Key, tasks.UpdateTaskInput{Status: &status, Revision: task.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if goal, err := goals.ReconcileAgent("worker", now); err != nil || goal.TaskKey != task.Key {
		t.Fatalf("initial goal = %#v, %v", goal, err)
	}
	first, err := svc.AddComment(context.Background(), tasks.AgentActor("worker"), task.Key, tasks.AddCommentInput{Body: "@user:customer first question"})
	if err != nil || len(first.CreatedWaits) != 1 {
		t.Fatalf("first question = %#v, %v", first, err)
	}

	now = askedAt.Add(299 * time.Second)
	second, err := svc.AddComment(context.Background(), tasks.AgentActor("worker"), task.Key, tasks.AddCommentInput{Body: "@user:customer follow-up"})
	if err != nil || len(second.CreatedWaits) != 1 {
		t.Fatalf("second question = %#v, %v", second, err)
	}
	wait := second.CreatedWaits[0]
	if wait.ID != first.CreatedWaits[0].ID || wait.RequestedAt != askedAt.Format(time.RFC3339Nano) || wait.RequestingCommentID == first.CreatedWaits[0].RequestingCommentID {
		t.Fatalf("updated wait = %#v, first = %#v", wait, first.CreatedWaits[0])
	}

	goal, err := goals.ReconcileAgent("worker", askedAt.Add(300*time.Second))
	if err != nil || goal.TaskKey != "" || goal.Waiting {
		t.Fatalf("goal at original grace boundary = %#v, %v", goal, err)
	}
}

func TestCurrentReturnsAuthoritativeSelectedTask(t *testing.T) {
	s := goalStore(t, goalNow)
	seedTask(t, s, "T-1", "agent:worker", "P0", "in_progress", "2026-09-01T00:00:00Z")
	seedTask(t, s, "T-2", "agent:worker", "P1", "open", "2026-09-02T00:00:00Z")

	task, ok, err := s.Current("worker", goalNow)
	if err != nil || !ok {
		t.Fatalf("Current initial: task=%#v ok=%t err=%v", task, ok, err)
	}
	if task.Key != "T-1" || task.Title != "title T-1" || task.Description != "description T-1" || task.Priority != "P0" || task.Status != "in_progress" || task.Revision != 1 {
		t.Fatalf("Current initial task=%#v", task)
	}

	updateTask(t, s, "T-1", "status", "done")
	task, ok, err = s.Current("worker", goalNow)
	if err != nil || !ok || task.Key != "T-2" {
		t.Fatalf("Current after release: task=%#v ok=%t err=%v", task, ok, err)
	}
	assertStoredGoal(t, s, "T-2")

	execGoalSQL(t, s, `UPDATE agents SET loop_enabled=0 WHERE name='worker'`)
	task, ok, err = s.Current("worker", goalNow)
	if err != nil || ok || task.Key != "" {
		t.Fatalf("Current disabled: task=%#v ok=%t err=%v", task, ok, err)
	}
	assertStoredGoal(t, s, "T-2")

	execGoalSQL(t, s, `UPDATE agents SET loop_enabled=1 WHERE name='worker'`)
	task, ok, err = s.Current("worker", goalNow)
	if err != nil || !ok || task.Key != "T-2" {
		t.Fatalf("Current re-enabled: task=%#v ok=%t err=%v", task, ok, err)
	}
}

func TestSetSelectsWorkflowTaskByStoredCategory(t *testing.T) {
	s := goalStore(t, goalNow)
	seedTask(t, s, "T-1", "agent:worker", "P2", "in_progress", "2026-09-01T00:00:00Z")
	execGoalSQL(t, s, `INSERT INTO task_workflow_images(digest,name,version,manifest,built_at) VALUES ('d1','flow','1.0.0','{}','now')`)
	execGoalSQL(t, s, `UPDATE tasks SET workflow_digest='d1', workflow_status='build' WHERE task_key='T-1'`)

	task, err := s.Set("worker", "T-1", goalNow, func() (func(), error) { return nil, nil })
	if err != nil || task.Key != "T-1" {
		t.Fatalf("Set workflow task: task=%#v err=%v", task, err)
	}
	if task.Status != "in_progress" || task.Category != "in_progress" || task.WorkflowStatus != "build" || task.WorkflowName != "flow" || task.WorkflowVersion != "1.0.0" {
		t.Fatalf("workflow task fields = %#v", task)
	}
}

type goalTask struct {
	key, priority, status, createdAt string
}

func goalStore(t *testing.T, now time.Time) *Store {
	t.Helper()
	base, err := basestore.Open(filepath.Join(t.TempDir(), "goals.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	s := NewStore(base)
	seedAgent(t, s, "worker", now.Add(-time.Hour))
	return s
}

func seedAgent(t *testing.T, s *Store, name string, createdAt time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO agents(name,image_ref,created_at,enabled,loop_enabled,goal_enabled,goal_wait_customer_timeout_s) VALUES (?,?,?,?,?,?,?)`,
		name, "basic:latest", createdAt.Format(time.RFC3339Nano), true, true, true, 300); err != nil {
		t.Fatalf("insert agent %s: %v", name, err)
	}
}

func seedTask(t *testing.T, s *Store, key, assignee, priority, status, createdAt string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO task_queues(prefix,name,created_at,updated_at) VALUES ('T','Tasks',?,?)`, createdAt, createdAt); err != nil {
		t.Fatalf("insert queue: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO tasks(task_key,queue_prefix,priority,title,description,status,author,customer,assignee,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		key, "T", priority, "title "+key, "description "+key, status, "user:customer", "user:customer", assignee, createdAt, createdAt); err != nil {
		t.Fatalf("insert task %s: %v", key, err)
	}
}

func seedCustomerWait(t *testing.T, s *Store, key string, requestedAt time.Time, answered bool) {
	t.Helper()
	seedCustomerWaitBy(t, s, key, "agent:worker", requestedAt, answered)
}

// seedCustomerWaitBy seeds a question to the customer asked by requester.
func seedCustomerWaitBy(t *testing.T, s *Store, key, requester string, requestedAt time.Time, answered bool) {
	t.Helper()
	var taskID int64
	if err := s.db.QueryRow(`SELECT id FROM tasks WHERE task_key=?`, key).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	result, err := s.db.Exec(`INSERT INTO task_comments(task_id,author,body,created_at,updated_at) VALUES (?,?,?,?,?)`,
		taskID, requester, "question", requestedAt.Format(time.RFC3339Nano), requestedAt.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	commentID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	resolvedAt := ""
	var resolvingCommentID any
	if answered {
		resolvedAt = goalNow.Format(time.RFC3339Nano)
		resolvingCommentID = commentID
	}
	if _, err := s.db.Exec(`INSERT INTO task_waiting_for(task_id,expected_principal,requesting_principal,requesting_comment_id,requested_at,resolving_comment_id,resolved_at) VALUES (?,?,?,?,?,?,?)`,
		taskID, "user:customer", requester, commentID, requestedAt.Format(time.RFC3339Nano), resolvingCommentID, resolvedAt); err != nil {
		t.Fatal(err)
	}
}

func seedBlockingRelation(t *testing.T, s *Store, source, target string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO task_relations(source_id,target_id,type,created_by,created_at)
		SELECT source.id,target.id,'blocks','user:customer',? FROM tasks source,tasks target
		WHERE source.task_key=? AND target.task_key=?`, goalNow.Format(time.RFC3339Nano), source, target); err != nil {
		t.Fatal(err)
	}
}

func updateTask(t *testing.T, s *Store, key, field, value string) {
	t.Helper()
	queries := map[string]string{
		"status":       `UPDATE tasks SET status=? WHERE task_key=?`,
		"pull_request": `UPDATE tasks SET pull_request=? WHERE task_key=?`,
		"assignee":     `UPDATE tasks SET assignee=? WHERE task_key=?`,
	}
	query, ok := queries[field]
	if !ok {
		t.Fatalf("unsupported task field %q", field)
	}
	if _, err := s.db.Exec(query, value, key); err != nil {
		t.Fatal(err)
	}
}

func assertStoredGoal(t *testing.T, s *Store, want string) {
	t.Helper()
	var got string
	if err := s.db.QueryRow(`SELECT current_goal_task_key FROM agents WHERE name='worker'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("stored goal = %q, want %q", got, want)
	}
}

func execGoalSQL(t *testing.T, s *Store, query string) {
	t.Helper()
	if _, err := s.db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func openGoalStore(t *testing.T) *basestore.Store {
	t.Helper()
	base, err := basestore.Open(filepath.Join(t.TempDir(), "goals.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	return base
}
