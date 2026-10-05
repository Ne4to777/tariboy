package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/tasks"
)

// workflowStub records the calls the workflow routes make. It embeds the
// interface so only the methods under test need bodies.
type workflowStub struct {
	registry.TaskControl
	calls []string
	err   error
}

func (s *workflowStub) CustomerLogin() string { return "customer" }
func (s *workflowStub) record(actor tasks.Actor, format string, args ...any) {
	s.calls = append(s.calls, fmt.Sprintf("%s|%s", actor.Principal, fmt.Sprintf(format, args...)))
}
func (s *workflowStub) Advance(_ context.Context, a tasks.Actor, key string, in tasks.AdvanceInput) (tasks.TransitionRequest, error) {
	s.record(a, "advance %s %q %q from=%q", key, in.Outcome, in.Message, in.From)
	return tasks.TransitionRequest{ID: 7, TaskKey: key, Outcome: in.Outcome, State: "applied"}, s.err
}
func (s *workflowStub) SetArtifact(_ context.Context, a tasks.Actor, key, name, value string) (tasks.Artifact, error) {
	s.record(a, "artifact_set %s %s %q", key, name, value)
	return tasks.Artifact{ID: 1, Name: name, Value: value, Author: a.Principal}, s.err
}
func (s *workflowStub) ListArtifacts(_ context.Context, a tasks.Actor, key string) ([]tasks.Artifact, error) {
	s.record(a, "artifact_ls %s", key)
	return []tasks.Artifact{{Name: "plan", Value: "p"}}, s.err
}
func (s *workflowStub) GetArtifact(_ context.Context, a tasks.Actor, key, name string) (tasks.Artifact, []tasks.Artifact, error) {
	s.record(a, "artifact_show %s %s", key, name)
	current := tasks.Artifact{Name: name, Value: "p"}
	return current, []tasks.Artifact{current}, s.err
}
func (s *workflowStub) GetWorkflow(_ context.Context, a tasks.Actor, key string) (tasks.WorkflowView, error) {
	s.record(a, "workflow_get %s", key)
	return tasks.WorkflowView{Name: "flow", Version: "1.0.0", Status: "develop", Outcomes: []tasks.OutcomeView{}}, s.err
}
func (s *workflowStub) GetTransitionRequest(_ context.Context, a tasks.Actor, key string, id int64) (tasks.TransitionRequest, error) {
	s.record(a, "request_get %s %d", key, id)
	return tasks.TransitionRequest{ID: id, TaskKey: key, State: "pending", WaitSeconds: 90}, s.err
}
func (s *workflowStub) ListScriptRuns(_ context.Context, a tasks.Actor, key string) ([]tasks.ScriptRun, error) {
	s.record(a, "runs %s", key)
	return []tasks.ScriptRun{{ID: 4, TaskKey: key, Kind: "check", State: "finished"}}, s.err
}
func (s *workflowStub) GetScriptRun(_ context.Context, a tasks.Actor, key string, id int64) (tasks.ScriptRun, error) {
	s.record(a, "run_get %s %d", key, id)
	return tasks.ScriptRun{ID: id, TaskKey: key, Kind: "check", State: "finished"}, s.err
}
func (s *workflowStub) ScriptRunLog(_ context.Context, a tasks.Actor, key string, id int64, maxBytes int) (string, bool, error) {
	s.record(a, "run_log %s %d max=%d", key, id, maxBytes)
	return "log text\n", true, s.err
}
func (s *workflowStub) MoveWorkflow(_ context.Context, a tasks.Actor, key, to, reason string) (tasks.Task, error) {
	s.record(a, "workflow_move %s %s %q", key, to, reason)
	return tasks.Task{Key: key, Status: tasks.StatusInProgress, WorkflowDigest: "d", WorkflowStatus: to}, s.err
}
func (s *workflowStub) ResumeWorkflow(_ context.Context, a tasks.Actor, key, decision string) (tasks.Task, error) {
	s.record(a, "workflow_resume %s %s", key, decision)
	return tasks.Task{Key: key, Status: tasks.StatusInProgress, WorkflowDigest: "d", WorkflowStatus: "develop"}, s.err
}
func (s *workflowStub) CancelWorkflowTask(_ context.Context, a tasks.Actor, key string) (tasks.Task, error) {
	s.record(a, "cancel %s", key)
	return tasks.Task{Key: key, Status: tasks.StatusCancelled}, s.err
}
func (s *workflowStub) SetQueueWorkflow(_ context.Context, a tasks.Actor, queue, ref string, revision int64) (tasks.QueueWorkflow, error) {
	s.record(a, "queue_workflow_set %s %s %d", queue, ref, revision)
	return tasks.QueueWorkflow{Queue: queue, Name: "flow", Version: "1.0.0", Revision: revision + 1}, s.err
}
func (s *workflowStub) GetQueueWorkflow(_ context.Context, a tasks.Actor, queue string) (tasks.QueueWorkflow, error) {
	s.record(a, "queue_workflow_get %s", queue)
	return tasks.QueueWorkflow{Queue: queue, Name: "flow", Revision: 3}, s.err
}
func (s *workflowStub) ClearQueueWorkflow(_ context.Context, a tasks.Actor, queue string, revision int64) error {
	s.record(a, "queue_workflow_clear %s %d", queue, revision)
	return s.err
}

func (s *workflowStub) SetQueueSecret(_ context.Context, a tasks.Actor, queue, key, value string) error {
	s.record(a, "queue_secret_set %s %s %q", queue, key, value)
	return s.err
}
func (s *workflowStub) SetQueueSecretInfo(_ context.Context, a tasks.Actor, queue, key, value string) (tasks.QueueSecretInfo, error) {
	s.record(a, "queue_secret_set_info %s %s %q", queue, key, value)
	return tasks.QueueSecretInfo{Key: key, UpdatedAt: "2026-10-02T00:00:01Z"}, s.err
}
func (s *workflowStub) ListQueueSecrets(_ context.Context, a tasks.Actor, queue string) ([]tasks.QueueSecretInfo, error) {
	s.record(a, "queue_secret_ls %s", queue)
	return []tasks.QueueSecretInfo{{Key: "GH_TOKEN", UpdatedAt: "2026-10-02T00:00:00Z"}}, s.err
}
func (s *workflowStub) RemoveQueueSecret(_ context.Context, a tasks.Actor, queue, key string) error {
	s.record(a, "queue_secret_rm %s %s", queue, key)
	return s.err
}

func (s *workflowStub) ListQueueSources(_ context.Context, a tasks.Actor, queue string) ([]tasks.QueueSource, error) {
	s.record(a, "queue_source_ls %s", queue)
	return []tasks.QueueSource{{Queue: queue, Name: "pull-requests", Script: "./scripts/prs.sh", Every: "2m", Failures: 1}}, s.err
}
func (s *workflowStub) QueueSourceRunLog(_ context.Context, a tasks.Actor, queue string, id int64, maxBytes int) (string, bool, error) {
	s.record(a, "queue_source_log %s %d max=%d", queue, id, maxBytes)
	return "source log\n", false, s.err
}

func workflowServer(t *testing.T, stub *workflowStub) *httptest.Server {
	t.Helper()
	server := api.NewServer(BuildRegistry(), &registry.Ctx{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Tasks: stub,
	})
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

func TestWorkflowRoutesCallTheServiceAsTheCustomer(t *testing.T) {
	cases := []struct {
		name, method, path string
		body               any
		call               string
		result             string // a substring of the JSON result
	}{
		{"advance", "POST", "/api/tasks/DEV-1/advance", map[string]any{"outcome": "ready", "message": "PR up"},
			`advance DEV-1 "ready" "PR up" from=""`, `"state":"applied"`},
		{"advance from", "POST", "/api/tasks/DEV-1/advance", map[string]any{"outcome": "ready", "from": "develop"},
			`advance DEV-1 "ready" "" from="develop"`, `"state":"applied"`},
		{"artifact set keeps the value as given", "PUT", "/api/tasks/DEV-1/artifacts/plan", map[string]any{"value": "step 1\n"},
			`artifact_set DEV-1 plan "step 1\n"`, `"name":"plan"`},
		{"artifact ls", "GET", "/api/tasks/DEV-1/artifacts", nil, `artifact_ls DEV-1`, `"count":1`},
		{"artifact show", "GET", "/api/tasks/DEV-1/artifacts/plan", nil, `artifact_show DEV-1 plan`, `"history":[`},
		{"workflow get", "GET", "/api/tasks/DEV-1/workflow", nil, `workflow_get DEV-1`, `"status":"develop"`},
		{"request get", "GET", "/api/tasks/DEV-1/workflow/requests/7", nil, `request_get DEV-1 7`, `"wait_seconds":90`},
		{"runs list", "GET", "/api/tasks/DEV-1/workflow/runs", nil, `runs DEV-1`, `"runs":[{"id":4`},
		{"run get", "GET", "/api/tasks/DEV-1/workflow/runs/4", nil, `run_get DEV-1 4`, `"kind":"check"`},
		{"run log", "GET", "/api/tasks/DEV-1/workflow/runs/4/log", nil, `run_log DEV-1 4 max=0`,
			`"run_id":4,"text":"log text\n","truncated":true`},
		{"run log max_bytes", "GET", "/api/tasks/DEV-1/workflow/runs/4/log?max_bytes=100", nil, `run_log DEV-1 4 max=100`, `"truncated":true`},
		{"workflow move", "POST", "/api/tasks/DEV-1/workflow/move", map[string]any{"to": "review", "reason": "unstick"},
			`workflow_move DEV-1 review "unstick"`, `"key":"DEV-1"`},
		{"workflow resume", "POST", "/api/tasks/DEV-1/workflow/resume", map[string]any{"decision": "release"},
			`workflow_resume DEV-1 release`, `"category":"in_progress"`},
		{"cancel", "POST", "/api/tasks/DEV-1/cancel", nil, `cancel DEV-1`, `"category":"cancelled"`},
		{"queue workflow set", "PUT", "/api/task-queues/DEV/workflow", map[string]any{"ref": "flow:1.0.0", "revision": 2},
			`queue_workflow_set DEV flow:1.0.0 2`, `"revision":3`},
		{"queue workflow get", "GET", "/api/task-queues/DEV/workflow", nil, `queue_workflow_get DEV`, `"queue":"DEV"`},
		{"queue workflow clear", "DELETE", "/api/task-queues/DEV/workflow?revision=3", nil, `queue_workflow_clear DEV 3`,
			`"cleared":true`},
		{"queue secret ls", "GET", "/api/task-queues/DEV/secrets", nil, `queue_secret_ls DEV`,
			`"secrets":[{"key":"GH_TOKEN","updated_at":"2026-10-02T00:00:00Z"}]`},
		{"queue source ls", "GET", "/api/task-queues/DEV/sources", nil, `queue_source_ls DEV`,
			`"sources":[{"queue":"DEV","name":"pull-requests"`},
		{"queue source log", "GET", "/api/task-queues/DEV/source-runs/5/log?max_bytes=10", nil, `queue_source_log DEV 5 max=10`,
			`"run_id":5,"text":"source log\n","truncated":false`},
		{"queue secret rm", "DELETE", "/api/task-queues/DEV/secrets/GH_TOKEN", nil, `queue_secret_rm DEV GH_TOKEN`,
			`"removed":true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &workflowStub{}
			httpServer := workflowServer(t, stub)
			status, env := taskRequest(t, httpServer.Client(), tc.method, httpServer.URL+tc.path, tc.body)
			if status != http.StatusOK || !env.OK {
				t.Fatalf("status %d envelope %+v", status, env)
			}
			if len(stub.calls) != 1 || stub.calls[0] != "user:customer|"+tc.call {
				t.Fatalf("calls = %q; want %q", stub.calls, "user:customer|"+tc.call)
			}
			if !strings.Contains(string(env.Result), tc.result) {
				t.Fatalf("result %s lacks %s", env.Result, tc.result)
			}
		})
	}
}

func TestRunLogRouteValidatesMaxBytes(t *testing.T) {
	for query, wantStatus := range map[string]int{"?max_bytes=-1": 400, "?max_bytes=abc": 400, "?max_bytes=1.5": 400, "?max_bytes=0": 200, "": 200, "?max_bytes=": 200} {
		stub := &workflowStub{}
		httpServer := workflowServer(t, stub)
		status, env := taskRequest(t, httpServer.Client(), "GET", httpServer.URL+"/api/tasks/DEV-1/workflow/runs/4/log"+query, nil)
		if status != wantStatus {
			t.Errorf("%q: status %d envelope %+v", query, status, env)
		}
		if wantStatus == 400 && (env.Error == nil || env.Error.Code != "invalid_request" || len(stub.calls) != 0) {
			t.Errorf("%q: envelope %+v calls %v", query, env, stub.calls)
		}
	}
}

func TestWorkflowRoutesPassServiceErrorsThrough(t *testing.T) {
	stub := &workflowStub{err: &tasks.Error{Status: http.StatusConflict, Code: "workflow_managed",
		Msg: "task lifecycle is managed by its workflow", Data: map[string]any{"status": "develop", "outcomes": []string{"ready"}}}}
	httpServer := workflowServer(t, stub)
	status, env := taskRequest(t, httpServer.Client(), "POST", httpServer.URL+"/api/tasks/DEV-1/advance", map[string]any{"outcome": "ready"})
	if status != http.StatusConflict || env.Error == nil || env.Error.Code != "workflow_managed" || env.Error.Details["status"] != "develop" {
		t.Fatalf("status %d envelope %+v", status, env)
	}
	stub.err = &tasks.Error{Status: http.StatusBadRequest, Code: "invalid_decision", Msg: "unknown decision"}
	status, env = taskRequest(t, httpServer.Client(), "POST", httpServer.URL+"/api/tasks/DEV-1/workflow/resume", map[string]any{"decision": "restart"})
	if status != http.StatusBadRequest || env.Error == nil || env.Error.Code != "invalid_decision" {
		t.Fatalf("status %d envelope %+v", status, env)
	}
	stub.err = &tasks.Error{Status: http.StatusNotFound, Code: "queue_workflow_not_found", Msg: "queue has no workflow binding"}
	status, env = taskRequest(t, httpServer.Client(), "GET", httpServer.URL+"/api/task-queues/DEV/workflow", nil)
	if status != http.StatusNotFound || env.Error == nil || env.Error.Code != "queue_workflow_not_found" {
		t.Fatalf("status %d envelope %+v", status, env)
	}
}

func TestWorkflowOpenAPIDescribesRoutesAndSchemas(t *testing.T) {
	httpServer := workflowServer(t, &workflowStub{})
	status, env := taskRequest(t, httpServer.Client(), "GET", httpServer.URL+"/api/openapi.json", nil)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	var doc struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(env.Result, &doc); err != nil {
		t.Fatal(err)
	}
	for path, method := range map[string]string{
		"/api/tasks/{key}/advance":          "post",
		"/api/tasks/{key}/artifacts/{name}": "put",
		"/api/tasks/{key}/artifacts":        "get",
		"/api/tasks/{key}/workflow":         "get",
		"/api/tasks/{key}/workflow/move":    "post",
		"/api/tasks/{key}/workflow/resume":  "post",
		"/api/tasks/{key}/workflow/runs":    "get",

		"/api/tasks/{key}/workflow/runs/{id}":     "get",
		"/api/tasks/{key}/workflow/runs/{id}/log": "get",
		"/api/tasks/{key}/workflow/requests/{id}": "get",
		"/api/tasks/{key}/cancel":                 "post",
		"/api/task-queues/{queue}/workflow":       "delete",
	} {
		if _, ok := doc.Paths[path][method]; !ok {
			t.Errorf("openapi lacks %s %s", method, path)
		}
	}
	for _, name := range []string{"WorkflowView", "OutcomeView", "TransitionRequest", "Artifact", "StatusVisit", "QueueWorkflow", "ScriptRun"} {
		if doc.Components.Schemas[name] == nil {
			t.Errorf("openapi lacks schema %s", name)
		}
	}
	viewProps, _ := doc.Components.Schemas["WorkflowView"]["properties"].(map[string]any)
	for _, name := range []string{"paused_reason", "declared_artifacts", "statuses"} {
		if viewProps[name] == nil {
			t.Errorf("WorkflowView schema lacks %s", name)
		}
	}
	for _, name := range []string{"DeclaredArtifact", "StatusView"} {
		if doc.Components.Schemas[name] == nil {
			t.Errorf("openapi lacks schema %s", name)
		}
	}
	props, _ := doc.Components.Schemas["Task"]["properties"].(map[string]any)
	for _, name := range []string{"category", "waiting_on", "workflow_digest", "workflow_name", "workflow_version", "workflow_paused_reason"} {
		if props[name] == nil {
			t.Errorf("Task schema lacks %s", name)
		}
	}
	var advance map[string]any
	if err := json.Unmarshal(doc.Paths["/api/tasks/{key}/advance"]["post"], &advance); err != nil {
		t.Fatal(err)
	}
	body := advance["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	bodyProps, _ := body["properties"].(map[string]any)
	for _, name := range []string{"outcome", "message", "from"} {
		if bodyProps[name] == nil {
			t.Errorf("advance request schema lacks %s", name)
		}
	}
}
