package commands

import (
	"net/http"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/tasks"
)

func TestQueueSecretSetRouteStoresTheValueAsGivenAndNeverReturnsIt(t *testing.T) {
	stub := &workflowStub{}
	httpServer := workflowServer(t, stub)
	value := "  s3cr3t-token \n"
	status, env := taskRequest(t, httpServer.Client(), "PUT", httpServer.URL+"/api/task-queues/DEV/secrets/GH_TOKEN",
		map[string]any{"value": value})
	if status != http.StatusOK || !env.OK {
		t.Fatalf("status %d envelope %+v", status, env)
	}
	// One call: the time comes from the transaction that stored the value.
	if len(stub.calls) != 1 || stub.calls[0] != "user:customer|queue_secret_set_info DEV GH_TOKEN "+`"  s3cr3t-token \n"` {
		t.Fatalf("calls = %q", stub.calls)
	}
	for _, want := range []string{`"queue":"DEV"`, `"key":"GH_TOKEN"`, `"updated_at":"2026-10-02T00:00:01Z"`} {
		if !strings.Contains(string(env.Result), want) {
			t.Fatalf("result %s lacks %s", env.Result, want)
		}
	}
	if strings.Contains(string(env.Result), "s3cr3t") {
		t.Fatalf("result returns the value: %s", env.Result)
	}
}

func TestQueueSecretSetRouteBoundsTheBody(t *testing.T) {
	stub := &workflowStub{}
	httpServer := workflowServer(t, stub)
	status, _ := taskRequest(t, httpServer.Client(), "PUT", httpServer.URL+"/api/task-queues/DEV/secrets/GH_TOKEN",
		map[string]any{"value": strings.Repeat("x", 600<<10)})
	if status != http.StatusRequestEntityTooLarge || len(stub.calls) != 0 {
		t.Fatalf("status %d calls %q", status, stub.calls)
	}
}

func TestQueueSecretRoutesPassServiceErrorsThrough(t *testing.T) {
	stub := &workflowStub{err: &tasks.Error{Status: http.StatusConflict, Code: "workflow_secret_missing",
		Msg: "workflow secrets have no value: GH_TOKEN", Data: map[string]any{"secrets": []string{"GH_TOKEN"}}}}
	httpServer := workflowServer(t, stub)
	status, env := taskRequest(t, httpServer.Client(), "DELETE", httpServer.URL+"/api/task-queues/DEV/secrets/GH_TOKEN", nil)
	if status != http.StatusConflict || env.Error == nil || env.Error.Code != "workflow_secret_missing" || env.Error.Details["secrets"] == nil {
		t.Fatalf("status %d envelope %+v", status, env)
	}
}
