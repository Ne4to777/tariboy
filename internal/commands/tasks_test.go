package commands

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
)

type taskEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func taskRequest(t *testing.T, client *http.Client, method, url string, body any) (int, taskEnvelope) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env taskEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, env
}

func TestPoolAndTriggerOperatorHTTPKeepsTypedRoutesWithoutWorkflowEngine(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := tasks.NewService(st.DB, "customer", time.Now)
	server := api.NewServer(BuildRegistry(), &registry.Ctx{
		Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Tasks: svc,
	})
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	resp, err := httpServer.Client().Get(httpServer.URL + "/api/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var openapi struct {
		Result struct {
			Paths      map[string]map[string]any `json:"paths"`
			Components struct {
				Schemas map[string]map[string]any `json:"schemas"`
			} `json:"components"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&openapi); err != nil {
		t.Fatal(err)
	}
	wantRoutes := map[string]string{
		"/api/task-queues/{queue}/pools/{pool}":           "patch",
		"/api/task-queues/{queue}/pools":                  "get",
		"/api/task-queues/{queue}/workflow-triggers":      "post",
		"/api/task-queues/{queue}/workflow-triggers/{id}": "delete",
	}
	for path, method := range wantRoutes {
		if _, ok := openapi.Result.Paths[path][method]; !ok {
			t.Errorf("OpenAPI missing %s %s", method, path)
		}
	}
	for _, path := range []string{
		"/api/workflows", "/api/workflows/{name}/versions", "/api/workflows/{name}/versions/{version}",
		"/api/workflows/{name}/versions/{version}/validate", "/api/workflows/{name}/versions/{version}/publish",
		"/api/tasks/{key}/work-packets",
		"/api/tasks/{key}/assignments", "/api/tasks/{key}/artifacts/{id}",
		"/api/tasks/{key}/questions", "/api/tasks/{key}/questions/{id}", "/api/tasks/{key}/subscriptions",
		"/api/tasks/{key}/workflow-events",
	} {
		if _, ok := openapi.Result.Paths[path]; ok {
			t.Errorf("OpenAPI still publishes removed route %s", path)
		}
	}
	for _, name := range []string{"WorkflowDefinition", "WorkflowVersion", "QueueWorkflowBinding", "WorkflowExecutionView", "Assignment", "WorkPacket"} {
		if _, ok := openapi.Result.Components.Schemas[name]; ok {
			t.Errorf("OpenAPI still publishes removed schema %s", name)
		}
	}
	existingTaskGet := openapi.Result.Paths["/api/tasks/{key}"]["get"].(map[string]any)
	params := existingTaskGet["parameters"].([]any)
	if len(params) != 1 || params[0].(map[string]any)["name"] != "key" || params[0].(map[string]any)["schema"].(map[string]any)["type"] != "string" || existingTaskGet["responses"] == nil {
		t.Fatalf("existing task OpenAPI contract regressed: %#v", existingTaskGet)
	}
	status, env := taskRequest(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/workflows/legacy/versions", nil)
	if status != http.StatusNotFound {
		t.Fatalf("removed workflow route = %d/%+v; want 404", status, env)
	}

	if _, err := st.DB.Exec(`INSERT INTO agents(name, image_ref, image_digest) VALUES ('dev-a', 'basic:latest', 'digest')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateQueue(t.Context(), tasks.CustomerActor("customer"), tasks.CreateQueueInput{Prefix: "FLOW", Name: "Flow"}); err != nil {
		t.Fatal(err)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodPatch,
		httpServer.URL+"/api/task-queues/FLOW/pools/developers", map[string]any{
			"agents": []string{"dev-a"}, "revision": 0, "idempotency_key": "pool-http",
			"actor": "agent:attacker", "customer": "mallory",
		})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("bind pool = %d/%+v", status, env)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodPatch,
		httpServer.URL+"/api/task-queues/FLOW/pools/developers", map[string]any{
			"agents": []string{"dev-a"}, "revision": 99, "idempotency_key": "pool-stale",
		})
	if status != http.StatusConflict || env.Error == nil || env.Error.Code != "revision_conflict" || env.Error.Details["current"] == nil {
		t.Fatalf("stale pool bind = %d/%+v", status, env)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/tasks", map[string]any{"queue": "FLOW", "title": "flexible"})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("create task = %d/%+v", status, env)
	}
	var created tasks.Task
	if err := json.Unmarshal(env.Result, &created); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(env.Result, []byte(`"workflow_`)) {
		t.Fatalf("flexible task JSON still carries old workflow fields: %s", env.Result)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodPatch,
		httpServer.URL+"/api/tasks/"+created.Key, map[string]any{"status": "done", "revision": created.Revision})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("flexible status update = %d/%+v", status, env)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/task-queues/FLOW/workflow-triggers", map[string]any{"pattern": "incident:*", "action": "create_task"})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("create trigger = %d/%+v", status, env)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/task-queues/FLOW/workflow-triggers", nil)
	if status != http.StatusOK || !env.OK {
		t.Fatalf("list triggers = %d/%+v", status, env)
	}
}

func TestTasksHTTPDerivesCustomerScopesListsAndRejectsStaleRevision(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	taskService := tasks.NewService(st.DB, "customer", func() time.Time {
		return time.Date(2026, 7, 31, 14, 0, 0, 0, time.UTC)
	})
	server := api.NewServer(BuildRegistry(), &registry.Ctx{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tasks: taskService,
	})
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	status, env := taskRequest(t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/task-queues", map[string]any{
			"prefix": "API", "name": "API",
		})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("create queue status/env = %d/%+v", status, env)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/tasks", map[string]any{
			"queue": "API", "title": "Alice task", "assignee": "alice",
		})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("create task status/env = %d/%+v", status, env)
	}
	var created tasks.Task
	if err := json.Unmarshal(env.Result, &created); err != nil {
		t.Fatal(err)
	}
	if created.Author != "user:customer" || !strings.HasPrefix(created.Key, "API-") {
		t.Fatalf("created = %#v", created)
	}
	_, _ = taskService.CreateTask(t.Context(), tasks.CustomerActor("customer"),
		tasks.CreateTaskInput{Queue: "API", Title: "Hidden from Alice"})

	status, env = taskRequest(t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/tasks?scope_agent=alice", nil)
	if status != http.StatusOK || !env.OK {
		t.Fatalf("list status/env = %d/%+v", status, env)
	}
	var page tasks.TaskPage
	if err := json.Unmarshal(env.Result, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].Key != created.Key {
		t.Fatalf("agent-scoped page = %#v", page)
	}

	status, env = taskRequest(t, httpServer.Client(), http.MethodPatch,
		httpServer.URL+"/api/tasks/"+created.Key, map[string]any{
			"title": "stale overwrite", "revision": 999,
		})
	if status != http.StatusConflict || env.Error == nil ||
		env.Error.Code != "revision_conflict" {
		t.Fatalf("stale update status/env = %d/%+v", status, env)
	}
	if env.Error.Details["current_revision"] != float64(created.Revision) {
		t.Fatalf("conflict details = %#v; want current revision %d",
			env.Error.Details, created.Revision)
	}

	status, env = taskRequest(t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/tasks", map[string]any{
			"queue": "API", "title": "Relation target",
		})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("create target status/env = %d/%+v", status, env)
	}
	var target tasks.Task
	if err := json.Unmarshal(env.Result, &target); err != nil {
		t.Fatal(err)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/tasks/"+created.Key+"/relations", map[string]any{
			"target_key": target.Key, "type": "related", "revision": created.Revision,
			"idempotency_key": "http-rel-1",
		})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("add relation status/env = %d/%+v", status, env)
	}
	var relation tasks.Relation
	if err := json.Unmarshal(env.Result, &relation); err != nil {
		t.Fatal(err)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodDelete,
		httpServer.URL+"/api/tasks/"+created.Key+"/relations?relation_id="+
			strconv.FormatInt(relation.ID, 10)+"&revision="+
			strconv.FormatInt(created.Revision+1, 10)+"&idempotency_key=http-del-1", nil)
	if status != http.StatusOK || !env.OK {
		t.Fatalf("delete relation status/env = %d/%+v", status, env)
	}
}

func TestTasksHTTPFiltersStatusViewsBeforeBuildingTheResponse(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := tasks.NewService(st.DB, "customer", time.Now)
	server := api.NewServer(BuildRegistry(), &registry.Ctx{
		Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Tasks: svc,
	})
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	customer := tasks.CustomerActor("customer")
	if _, err := svc.CreateQueue(t.Context(), customer, tasks.CreateQueueInput{Prefix: "VIEW", Name: "Views"}); err != nil {
		t.Fatal(err)
	}
	open, _ := svc.CreateTask(t.Context(), customer, tasks.CreateTaskInput{Queue: "VIEW", Title: "open"})
	inProgress, _ := svc.CreateTask(t.Context(), customer, tasks.CreateTaskInput{Queue: "VIEW", Title: "in progress"})
	done, _ := svc.CreateTask(t.Context(), customer, tasks.CreateTaskInput{Queue: "VIEW", Title: "done"})
	cancelled, _ := svc.CreateTask(t.Context(), customer, tasks.CreateTaskInput{Queue: "VIEW", Title: "cancelled"})
	for _, update := range []struct {
		task   tasks.Task
		status string
	}{{inProgress, "in_progress"}, {done, "done"}, {cancelled, "cancelled"}} {
		status := update.status
		if _, err := svc.UpdateTask(t.Context(), customer, update.task.Key, tasks.UpdateTaskInput{Status: &status, Revision: update.task.Revision}); err != nil {
			t.Fatal(err)
		}
	}

	assertView := func(path string, want ...string) int {
		t.Helper()
		status, env := taskRequest(t, httpServer.Client(), http.MethodGet, httpServer.URL+path, nil)
		if status != http.StatusOK || !env.OK {
			t.Fatalf("GET %s = %d/%+v", path, status, env)
		}
		var page tasks.TaskPage
		if err := json.Unmarshal(env.Result, &page); err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(page.Tasks))
		for _, task := range page.Tasks {
			got = append(got, task.Status)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("GET %s statuses = %v, want %v", path, got, want)
		}
		return len(env.Result)
	}
	activeBytes := assertView("/api/tasks?queue=VIEW", "open", "in_progress")
	assertView("/api/tasks?queue=VIEW&status_view=closed", "done")
	allBytes := assertView("/api/tasks?queue=VIEW&status_view=all", "open", "in_progress", "done", "cancelled")
	if activeBytes >= allBytes {
		t.Fatalf("active response is %d bytes, all response is %d bytes; excluded rows must not be shipped", activeBytes, allBytes)
	}
	status, env := taskRequest(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/tasks?status_view=everything", nil)
	if status != http.StatusBadRequest || env.Error == nil || env.Error.Code != "invalid_status_view" {
		t.Fatalf("invalid status view = %d/%+v", status, env)
	}
	_ = open
}

func TestTasksHTTPExposesAndValidatesPriority(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	taskService := tasks.NewService(st.DB, "customer", time.Now)
	server := api.NewServer(BuildRegistry(), &registry.Ctx{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tasks: taskService,
	})
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	status, env := taskRequest(t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/task-queues", map[string]any{"prefix": "PRI", "name": "Priority"})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("create queue status/env = %d/%+v", status, env)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/tasks", map[string]any{
			"queue": "PRI", "title": "urgent", "priority": "P1",
		})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("create task status/env = %d/%+v", status, env)
	}
	var created tasks.Task
	if err := json.Unmarshal(env.Result, &created); err != nil {
		t.Fatal(err)
	}
	if created.Priority != tasks.PriorityP1 {
		t.Fatalf("created priority = %q, want P1", created.Priority)
	}

	status, env = taskRequest(t, httpServer.Client(), http.MethodPatch,
		httpServer.URL+"/api/tasks/"+created.Key, map[string]any{
			"priority": "P0", "revision": created.Revision,
		})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("update task status/env = %d/%+v", status, env)
	}
	var updated tasks.Task
	if err := json.Unmarshal(env.Result, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Priority != tasks.PriorityP0 {
		t.Fatalf("updated priority = %q, want P0", updated.Priority)
	}

	status, env = taskRequest(t, httpServer.Client(), http.MethodGet,
		httpServer.URL+"/api/tasks", nil)
	if status != http.StatusOK || !env.OK {
		t.Fatalf("list tasks status/env = %d/%+v", status, env)
	}
	var page tasks.TaskPage
	if err := json.Unmarshal(env.Result, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].Priority != tasks.PriorityP0 {
		t.Fatalf("listed tasks = %#v", page.Tasks)
	}

	events, err := taskService.ListEvents(t.Context(), tasks.CustomerActor("customer"), created.Key, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[len(events)-1].Payload["priority"] != "P0" {
		t.Fatalf("events = %#v, want updated priority payload", events)
	}

	status, env = taskRequest(t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/tasks", map[string]any{
			"queue": "PRI", "title": "invalid", "priority": "urgent",
		})
	if status != http.StatusBadRequest || env.Error == nil || env.Error.Code != "invalid_priority" {
		t.Fatalf("invalid create status/env = %d/%+v", status, env)
	}
}

func TestTaskCreateAndUpdateAcceptPullRequestAndWaitCustomer(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := tasks.NewService(st.DB, "customer", time.Now)
	server := api.NewServer(BuildRegistry(), &registry.Ctx{
		Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Tasks: svc,
	})
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	actor := tasks.CustomerActor("customer")
	if _, err := svc.CreateQueue(t.Context(), actor, tasks.CreateQueueInput{Prefix: "PR", Name: "Pull requests"}); err != nil {
		t.Fatal(err)
	}
	status, env := taskRequest(t, httpServer.Client(), http.MethodPost,
		httpServer.URL+"/api/tasks", map[string]any{
			"queue": "PR", "title": "Expose PR", "pull_request": " HTTPS://Example.test/o/r/pull/6 ",
		})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("create task = %d/%+v", status, env)
	}
	var created tasks.Task
	if err := json.Unmarshal(env.Result, &created); err != nil {
		t.Fatal(err)
	}
	if created.PullRequest != "https://example.test/o/r/pull/6" {
		t.Fatalf("created task = %#v", created)
	}
	status, env = taskRequest(t, httpServer.Client(), http.MethodPatch,
		httpServer.URL+"/api/tasks/"+created.Key, map[string]any{
			"pull_request": "https://github.com/o/r/pull/7",
			"status":       tasks.StatusWaitCustomer,
			"revision":     created.Revision,
		})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("update task = %d/%+v", status, env)
	}
	var updated tasks.Task
	if err := json.Unmarshal(env.Result, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.PullRequest != "https://github.com/o/r/pull/7" || updated.Status != tasks.StatusWaitCustomer {
		t.Fatalf("updated task = %#v", updated)
	}
}

func TestTaskOpenAPIExposesCreateAndUpdatePullRequestAndWaitCustomer(t *testing.T) {
	server := api.NewServer(BuildRegistry(), &registry.Ctx{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	resp, err := httpServer.Client().Get(httpServer.URL + "/api/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var document struct {
		Result struct {
			Paths      map[string]map[string]any `json:"paths"`
			Components struct {
				Schemas map[string]map[string]any `json:"schemas"`
			} `json:"components"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	properties := document.Result.Components.Schemas["Task"]["properties"].(map[string]any)
	if properties["pull_request"] == nil {
		t.Fatalf("Task schema properties = %#v", properties)
	}
	create := document.Result.Paths["/api/tasks"]["post"].(map[string]any)
	createBody := create["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	createProperties := createBody["properties"].(map[string]any)
	if createProperties["pull_request"] == nil {
		t.Fatalf("task create properties = %#v", createProperties)
	}
	update := document.Result.Paths["/api/tasks/{key}"]["patch"].(map[string]any)
	body := update["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	updateProperties := body["properties"].(map[string]any)
	if updateProperties["pull_request"] == nil {
		t.Fatalf("task update properties = %#v", updateProperties)
	}
	statusSchema := updateProperties["status"].(map[string]any)
	values := statusSchema["enum"].([]any)
	for _, value := range values {
		if value == tasks.StatusWaitCustomer {
			return
		}
	}
	t.Fatalf("task update status values = %#v", values)
}
