package loop

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
)

type attributionTaskReader struct {
	tasks map[string]tasks.Task
	err   map[string]error
	seen  []tasks.Actor
}

type attributionProxy struct {
	key, task, epic string
	updated         int
	changed         bool
}

func (p *attributionProxy) UpdateTask(key, task, epic string) int {
	p.key, p.task, p.epic = key, task, epic
	return p.updated
}

func (p *attributionProxy) SetTaskIfEmpty(key, task, epic string) (int, bool) {
	p.key, p.task, p.epic = key, task, epic
	return p.updated, p.changed
}

func (r *attributionTaskReader) GetTask(_ context.Context, actor tasks.Actor, key string) (tasks.TaskDetail, error) {
	r.seen = append(r.seen, actor)
	if err := r.err[key]; err != nil {
		return tasks.TaskDetail{}, err
	}
	task, ok := r.tasks[key]
	if !ok {
		return tasks.TaskDetail{}, errors.New("task not found")
	}
	return tasks.TaskDetail{Task: task}, nil
}

func TestResolveNativeTaskAttributionWalksToRootAsAgent(t *testing.T) {
	reader := &attributionTaskReader{tasks: map[string]tasks.Task{
		"SUPER-1": {Key: "SUPER-1"},
		"SUPER-2": {Key: "SUPER-2", ParentKey: "SUPER-1"},
		"SUPER-3": {Key: "SUPER-3", ParentKey: "SUPER-2"},
	}}

	tests := []struct {
		name     string
		key      string
		wantTask string
		wantEpic string
	}{
		{name: "root", key: "SUPER-1", wantTask: "SUPER-1", wantEpic: "SUPER-1"},
		{name: "child", key: "SUPER-2", wantTask: "SUPER-2", wantEpic: "SUPER-1"},
		{name: "grandchild", key: "SUPER-3", wantTask: "SUPER-3", wantEpic: "SUPER-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader.seen = nil
			taskID, epicID, err := resolveNativeTaskAttribution(context.Background(), reader, "lively-quokka", tt.key)
			if err != nil {
				t.Fatal(err)
			}
			if taskID != tt.wantTask || epicID != tt.wantEpic {
				t.Fatalf("attribution = %q/%q, want %q/%q", taskID, epicID, tt.wantTask, tt.wantEpic)
			}
			for _, actor := range reader.seen {
				if actor.Principal != "agent:lively-quokka" || actor.IsCustomer {
					t.Fatalf("lookup actor = %#v, want agent:lively-quokka", actor)
				}
			}
		})
	}
}

func TestResolveNativeTaskAttributionPropagatesLookupError(t *testing.T) {
	want := errors.New("native task unavailable")
	reader := &attributionTaskReader{err: map[string]error{"SUPER-9": want}}
	_, _, err := resolveNativeTaskAttribution(context.Background(), reader, "worker", "SUPER-9")
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestResolveNativeTaskAttributionRejectsParentCycle(t *testing.T) {
	reader := &attributionTaskReader{tasks: map[string]tasks.Task{
		"SUPER-1": {Key: "SUPER-1", ParentKey: "SUPER-2"},
		"SUPER-2": {Key: "SUPER-2", ParentKey: "SUPER-1"},
	}}
	_, _, err := resolveNativeTaskAttribution(context.Background(), reader, "worker", "SUPER-1")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error = %v, want parent cycle error", err)
	}
}

func TestResolveNativeTaskAttributionUsesRealServiceAuthorization(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	service := tasks.NewService(state.DB, "customer", func() time.Time {
		return time.Date(2026, 8, 3, 6, 0, 0, 0, time.UTC)
	})
	ctx := context.Background()
	customer := tasks.CustomerActor("customer")
	if _, err := service.CreateQueue(ctx, customer, tasks.CreateQueueInput{Prefix: "SUPER", Name: "Super"}); err != nil {
		t.Fatal(err)
	}
	root, err := service.CreateTask(ctx, customer, tasks.CreateTaskInput{Queue: "SUPER", Title: "root", Assignee: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := service.CreateTask(ctx, customer, tasks.CreateTaskInput{ParentKey: root.Key, Title: "child"})
	if err != nil {
		t.Fatal(err)
	}

	taskID, epicID, err := resolveNativeTaskAttribution(ctx, service, "worker", child.Key)
	if err != nil {
		t.Fatal(err)
	}
	if taskID != child.Key || epicID != root.Key {
		t.Fatalf("attribution=%q/%q, want %q/%q", taskID, epicID, child.Key, root.Key)
	}
	if _, _, err := resolveNativeTaskAttribution(ctx, service, "outsider", child.Key); tasks.ErrorCode(err) != "not_found" {
		t.Fatalf("outsider error=%v code=%q, want not_found", err, tasks.ErrorCode(err))
	}
}

func TestSetGoalUpdatesSelectionAndLiveProxyAttribution(t *testing.T) {
	reader := &attributionTaskReader{tasks: map[string]tasks.Task{
		"T-1": {Key: "T-1"},
		"T-2": {Key: "T-2", ParentKey: "T-1"},
	}}
	proxy := &attributionProxy{updated: 1, changed: true}
	selected := ""
	result, err := setGoal(context.Background(), reader, func(agent, key string, activate func() (func(), error)) error {
		if agent != "worker" {
			t.Fatalf("agent=%q", agent)
		}
		if _, err := activate(); err != nil {
			return err
		}
		selected = key
		return nil
	}, proxy, "iter-1", "worker", "T-2")
	if err != nil {
		t.Fatal(err)
	}
	if selected != "T-2" || proxy.key != "iter-1" || proxy.task != "T-2" || proxy.epic != "T-1" {
		t.Fatalf("selected=%q proxy=%+v", selected, proxy)
	}
	if result["task_id"] != "T-2" || result["epic_id"] != "T-1" || result["updated"] != 1 {
		t.Fatalf("result=%v", result)
	}
}

// TestSetGoalRefusesATaskReadOnlyAsAFormerHolder: a workflow holder row lets
// an agent read its former task, never select it. A task the agent reads as
// the ancestor of its own work resolves as it always did.
func TestSetGoalRefusesATaskReadOnlyAsAFormerHolder(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	service := tasks.NewService(state.DB, "customer", func() time.Time {
		return time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	})
	ctx := context.Background()
	customer := tasks.CustomerActor("customer")
	if _, err := service.CreateQueue(ctx, customer, tasks.CreateQueueInput{Prefix: "SUPER", Name: "Super"}); err != nil {
		t.Fatal(err)
	}
	root, err := service.CreateTask(ctx, customer, tasks.CreateTaskInput{Queue: "SUPER", Title: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateTask(ctx, customer, tasks.CreateTaskInput{ParentKey: root.Key, Title: "child", Assignee: "worker"}); err != nil {
		t.Fatal(err)
	}
	former, err := service.CreateTask(ctx, customer, tasks.CreateTaskInput{Queue: "SUPER", Title: "former"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.DB.Exec(`INSERT INTO task_workflow_holders(task_id, pool, agent, dispatched_at) VALUES (?, 'devs', 'worker', 'now')`,
		former.ID); err != nil {
		t.Fatal(err)
	}
	if detail, err := service.GetTask(ctx, tasks.AgentActor("worker"), former.Key); err != nil || detail.Task.Access != "context" {
		t.Fatalf("former holder read = %q, %v", detail.Task.Access, err)
	}

	var selected []string
	selectGoal := func(_, key string, _ func() (func(), error)) error {
		selected = append(selected, key)
		return nil
	}
	proxy := &attributionProxy{updated: 1, changed: true}
	if _, err := setGoal(ctx, service, selectGoal, proxy, "iter-1", "worker", former.Key); err == nil {
		t.Fatal("a former holder selected its task as the goal")
	}
	if _, err := setGoal(ctx, service, selectGoal, proxy, "iter-1", "worker", root.Key); err != nil {
		t.Fatalf("ancestor context reader: %v", err)
	}
	if len(selected) != 1 || selected[0] != root.Key {
		t.Fatalf("selected = %v; want only %s", selected, root.Key)
	}
}

func TestSetGoalDoesNotPersistWhenLiveLeaseCannotBeUpdated(t *testing.T) {
	reader := &attributionTaskReader{tasks: map[string]tasks.Task{"T-1": {Key: "T-1"}}}
	proxy := &attributionProxy{}
	selected := false
	_, err := setGoal(context.Background(), reader, func(_, _ string, activate func() (func(), error)) error {
		if _, err := activate(); err != nil {
			return err
		}
		selected = true
		return nil
	}, proxy, "iter-1", "worker", "T-1")
	if err == nil || selected {
		t.Fatalf("err=%v selected=%t", err, selected)
	}
}
