package compose

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/tasks"
)

func TestParseWorkflowRefForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want WorkflowRef
	}{
		{"official/development", WorkflowRef{Store: "official", Name: "development"}},
		{"official/development:0.1.0", WorkflowRef{Store: "official", Name: "development", Tag: "0.1.0"}},
		{"development", WorkflowRef{Name: "development"}},
		{"development:0.1.0", WorkflowRef{Name: "development", Tag: "0.1.0"}},
		{"development:latest", WorkflowRef{Name: "development", Tag: "latest"}},
	} {
		got, err := ParseWorkflowRef(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseWorkflowRef(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
}

func TestParseWorkflowRefRejectsMalformedValues(t *testing.T) {
	for _, in := range []string{
		" development", "development ", "a/b/c", "a:b:c", "/development", "official/", "official/:1.0.0",
		":1.0.0", "development:", "Development", "official/Dev", "../x", "official/..", "a b", "official/dev:../x", "dev\n",
	} {
		if got, err := ParseWorkflowRef(in); err == nil {
			t.Errorf("ParseWorkflowRef(%q) = %+v; want an error", in, got)
		}
	}
}

func TestValidateNamesQueueAndMalformedWorkflow(t *testing.T) {
	f := File{Version: 1, TaskQueues: map[string]TaskQueueSpec{"DEV": {Name: "Development", Workflow: "a/b/c"}}}
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "DEV") || !strings.Contains(err.Error(), "a/b/c") {
		t.Fatalf("Validate = %v; want an error naming the queue and the value", err)
	}
}

func TestParseAcceptsQueueWorkflowAndEmptyEqualsAbsent(t *testing.T) {
	f, err := Parse([]byte("version: 1\ntask_queues:\n  DEV: {name: Development, workflow: official/development:0.1.0}\n  OPS: {name: Ops, workflow: \"\"}\n  QA: {name: QA}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if f.TaskQueues["DEV"].Workflow != "official/development:0.1.0" {
		t.Fatalf("workflow = %q", f.TaskQueues["DEV"].Workflow)
	}
	for _, q := range []string{"OPS", "QA"} {
		if _, declared, err := f.TaskQueues[q].WorkflowRef(); declared || err != nil {
			t.Fatalf("%s declared=%v err=%v; want not declared", q, declared, err)
		}
	}
}

// workflowComposeCaller is a fake daemon with workflow images and queue
// bindings. Calls are recorded in order next to the pool and queue calls.
type workflowComposeCaller struct {
	*taskQueueComposeCaller
	built    map[string]map[string]any // selector -> build result
	images   map[string]map[string]any // name:tag -> manifest
	bound    map[string]tasks.QueueWorkflow
	buildErr map[string]error
	bindErr  error
}

func newWorkflowComposeCaller() *workflowComposeCaller {
	return &workflowComposeCaller{
		taskQueueComposeCaller: newTaskQueueComposeCaller(),
		built:                  map[string]map[string]any{},
		images:                 map[string]map[string]any{},
		bound:                  map[string]tasks.QueueWorkflow{},
		buildErr:               map[string]error{},
	}
}

func (f *workflowComposeCaller) publish(selector, name, version, digest string, tags ...string) {
	f.built[selector] = map[string]any{"name": name, "version": version, "digest": digest, "tags": tags, "created": false}
	for _, tag := range tags {
		f.images[name+":"+tag] = map[string]any{"name": name, "version": version, "digest": digest}
	}
}

func (f *workflowComposeCaller) Call(method, route string, body any) (json.RawMessage, error) {
	switch {
	case method == "POST" && route == "/api/workflow-images/build":
		f.record(method, route, body)
		selector := body.(map[string]any)["source"].(string)
		if err := f.buildErr[selector]; err != nil {
			return nil, err
		}
		return mustJSON(f.built[selector]), nil
	case method == "GET" && strings.HasPrefix(route, "/api/workflow-images/"):
		f.record(method, route, body)
		parts := strings.Split(strings.TrimPrefix(route, "/api/workflow-images/"), "/")
		m, ok := f.images[parts[0]+":"+parts[1]]
		if !ok {
			return nil, &client.APIError{Code: "not_found", Msg: "workflow image " + parts[0] + ":" + parts[1] + " not found"}
		}
		return mustJSON(m), nil
	case strings.HasPrefix(route, "/api/task-queues/") && strings.HasSuffix(route, "/workflow") && (method == "GET" || method == "PUT"):
		f.record(method, route, body)
		queue := strings.Split(route, "/")[3]
		if method == "GET" {
			b, ok := f.bound[queue]
			if !ok {
				return nil, &client.APIError{Code: "queue_workflow_not_found", Msg: "queue has no workflow binding"}
			}
			return mustJSON(b), nil
		}
		if f.bindErr != nil {
			return nil, f.bindErr
		}
		ref := body.(map[string]any)["ref"].(string)
		cur := f.bound[queue]
		for _, m := range f.images {
			if m["digest"].(string) == ref[strings.LastIndex(ref, ":")+1:] {
				cur = tasks.QueueWorkflow{Queue: queue, Name: m["name"].(string), Version: m["version"].(string), Digest: m["digest"].(string), Revision: cur.Revision + 1}
			}
		}
		f.bound[queue] = cur
		return mustJSON(cur), nil
	}
	return f.taskQueueComposeCaller.Call(method, route, body)
}

func (f *workflowComposeCaller) record(method, route string, body any) {
	f.calls = append(f.calls, method+" "+route)
	f.bodies = append(f.bodies, body)
}

func workflowComposeFile(workflow string) File {
	f := taskQueueComposeFile()
	q := f.TaskQueues["DEV"]
	q.Workflow = workflow
	f.TaskQueues["DEV"] = q
	return f
}

const digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func callIndex(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

func TestUpBuildsStoreWorkflowAndBindsItsDigestAfterPools(t *testing.T) {
	fc := newWorkflowComposeCaller()
	fc.publish("official/development", "development", "0.1.0", digestA, "0.1.0", "latest")
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, workflowComposeFile("official/development"))
	pool := callIndex(fc.calls, "PATCH /api/task-queues/DEV/pools/developers")
	build := callIndex(fc.calls, "POST /api/workflow-images/build")
	bind := callIndex(fc.calls, "PUT /api/task-queues/DEV/workflow")
	if pool < 0 || build < 0 || bind < 0 || !(pool < build && build < bind) {
		t.Fatalf("order pool=%d build=%d bind=%d in %v", pool, build, bind, fc.calls)
	}
	if got := fc.bound["DEV"]; got.Digest != digestA || got.Name != "development" {
		t.Fatalf("bound = %+v", got)
	}
	if src := fc.bodies[build].(map[string]any)["source"]; src != "official/development" {
		t.Fatalf("build source = %v", src)
	}
}

func TestUpStoreWorkflowWithTagResolvesTheTagAfterTheBuild(t *testing.T) {
	fc := newWorkflowComposeCaller()
	fc.publish("official/development", "development", "0.2.0", digestB, "0.2.0", "latest")
	fc.images["development:0.1.0"] = map[string]any{"name": "development", "version": "0.1.0", "digest": digestA}
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, workflowComposeFile("official/development:0.1.0"))
	if got := fc.bound["DEV"]; got.Digest != digestA {
		t.Fatalf("bound = %+v; want the 0.1.0 digest", got)
	}
	build, resolve := callIndex(fc.calls, "POST /api/workflow-images/build"), callIndex(fc.calls, "GET /api/workflow-images/development/0.1.0")
	if build < 0 || resolve < 0 || build > resolve {
		t.Fatalf("tag resolved before the build: %v", fc.calls)
	}
}

func TestUpStoreWorkflowTagMissingAfterBuildIsAnError(t *testing.T) {
	fc := newWorkflowComposeCaller()
	fc.publish("official/development", "development", "0.2.0", digestB, "0.2.0", "latest")
	r := NewRunner(fc, "", "", io.Discard)
	r.skipBuild = true
	err := r.Up(workflowComposeFile("official/development:9.9.9"))
	if err == nil || !strings.Contains(err.Error(), "development:9.9.9") {
		t.Fatalf("Up = %v; want an error naming the ref", err)
	}
	if callIndex(fc.calls, "PUT /api/task-queues/DEV/workflow") >= 0 {
		t.Fatal("bound despite the missing tag")
	}
}

func TestUpPublishedRefBindsWithoutBuild(t *testing.T) {
	for _, tc := range []struct{ value, image string }{{"development", "development:latest"}, {"development:0.1.0", "development:0.1.0"}} {
		fc := newWorkflowComposeCaller()
		fc.images[tc.image] = map[string]any{"name": "development", "version": "0.1.0", "digest": digestA}
		r := NewRunner(fc, "", "", io.Discard)
		upNoBuild(t, r, workflowComposeFile(tc.value))
		if callIndex(fc.calls, "POST /api/workflow-images/build") >= 0 {
			t.Fatalf("%s: built a workflow without a Store prefix", tc.value)
		}
		if fc.bound["DEV"].Digest != digestA {
			t.Fatalf("%s: bound = %+v", tc.value, fc.bound["DEV"])
		}
	}
}

func TestUpMissingPublishedRefNamesTheRefAndTheFix(t *testing.T) {
	fc := newWorkflowComposeCaller()
	r := NewRunner(fc, "", "", io.Discard)
	r.skipBuild = true
	err := r.Up(workflowComposeFile("development:0.1.0"))
	if err == nil || !strings.Contains(err.Error(), "development:0.1.0") || !strings.Contains(err.Error(), "Store prefix") {
		t.Fatalf("Up = %v", err)
	}
}

func TestUpWithoutWorkflowLeavesBindingUntouched(t *testing.T) {
	fc := newWorkflowComposeCaller()
	fc.bound["DEV"] = tasks.QueueWorkflow{Queue: "DEV", Name: "development", Version: "0.1.0", Digest: digestA, Revision: 1}
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, workflowComposeFile(""))
	for _, c := range fc.calls {
		if strings.Contains(c, "/workflow") {
			t.Fatalf("touched the binding: %s", c)
		}
	}
}

func TestUpSecondRunMakesNoBindCallAndReportsUnchanged(t *testing.T) {
	fc := newWorkflowComposeCaller()
	fc.publish("official/development", "development", "0.1.0", digestA, "0.1.0", "latest")
	var out strings.Builder
	r := NewRunner(fc, "", "", &out)
	f := workflowComposeFile("official/development")
	upNoBuild(t, r, f)
	before, outBefore := len(fc.calls), out.Len()
	upNoBuild(t, r, f)
	if n := countCallsSince(fc.calls, before, "PUT /api/task-queues/DEV/workflow"); n != 0 {
		t.Fatalf("second run bound %d times", n)
	}
	if got := out.String()[outBefore:]; !strings.Contains(got, "workflow development:0.1.0 unchanged") {
		t.Fatalf("second run output:\n%s", got)
	}
}

func TestUpRebindsWhenAnotherDigestIsBound(t *testing.T) {
	fc := newWorkflowComposeCaller()
	fc.publish("official/development", "development", "0.2.0", digestB, "0.2.0", "latest")
	fc.bound["DEV"] = tasks.QueueWorkflow{Queue: "DEV", Name: "development", Version: "0.1.0", Digest: digestA, Revision: 3}
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, workflowComposeFile("official/development"))
	bind := callIndex(fc.calls, "PUT /api/task-queues/DEV/workflow")
	if bind < 0 {
		t.Fatal("no bind call")
	}
	body := fc.bodies[bind].(map[string]any)
	if body["revision"] != int64(3) || fc.bound["DEV"].Digest != digestB {
		t.Fatalf("body=%v bound=%+v", body, fc.bound["DEV"])
	}
}

func TestUpReportsPublishedVersionConflictFromTheBuild(t *testing.T) {
	fc := newWorkflowComposeCaller()
	fc.buildErr["official/development"] = &client.APIError{Code: "workflow_version_published", Msg: "version 0.1.0 is published with other content"}
	r := NewRunner(fc, "", "", io.Discard)
	r.skipBuild = true
	err := r.Up(workflowComposeFile("official/development"))
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "workflow_version_published" {
		t.Fatalf("Up = %v; want workflow_version_published to pass through", err)
	}
}

func TestUpSecretMissingNamesSecretsAndTheCommandWithoutAValue(t *testing.T) {
	fc := newWorkflowComposeCaller()
	fc.publish("official/development", "development", "0.1.0", digestA, "0.1.0", "latest")
	fc.bindErr = &client.APIError{Code: "workflow_secret_missing", Msg: "workflow secrets have no value: GH_TOKEN, NPM_TOKEN", Details: map[string]any{"secrets": []any{"GH_TOKEN", "NPM_TOKEN"}}}
	r := NewRunner(fc, "", "", io.Discard)
	r.skipBuild = true
	err := r.Up(workflowComposeFile("official/development"))
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "workflow_secret_missing" {
		t.Fatalf("Up = %v; want the daemon error intact", err)
	}
	for _, want := range []string{"GH_TOKEN", "NPM_TOKEN", "ttasks queue secret set DEV GH_TOKEN", "standard input"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err.Error(), want)
		}
	}
}

func TestStatusShowsBoundDriftedPendingAndUndeclaredWorkflows(t *testing.T) {
	status := func(t *testing.T, fc *workflowComposeCaller, workflow string) string {
		t.Helper()
		fc.agents["dev"] = map[string]any{"name": "dev", "state": "running", "image": "basic:latest"}
		fc.queues["DEV"] = tasks.Queue{Prefix: "DEV", Name: "Development", Revision: 1}
		fc.pools["DEV"] = map[string]tasks.AgentPool{"developers": {Queue: "DEV", Name: "developers", Agents: []string{"dev"}, Revision: 1}}
		var out strings.Builder
		if err := NewRunner(fc, "", "", &out).Status(workflowComposeFile(workflow)); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	bound := tasks.QueueWorkflow{Queue: "DEV", Name: "development", Version: "0.1.0", Digest: digestA, Revision: 1}

	fc := newWorkflowComposeCaller()
	fc.images["development:latest"] = map[string]any{"name": "development", "version": "0.1.0", "digest": digestA}
	fc.bound["DEV"] = bound
	if got := status(t, fc, "official/development"); !strings.Contains(got, "workflow development:0.1.0 aaaaaaaaaaaa ok") || !strings.Contains(got, "drift: 0") {
		t.Fatalf("bound status:\n%s", got)
	}

	fc = newWorkflowComposeCaller()
	fc.images["development:latest"] = map[string]any{"name": "development", "version": "0.2.0", "digest": digestB}
	fc.bound["DEV"] = bound
	if got := status(t, fc, "official/development"); !strings.Contains(got, "workflow drift: have=development:0.1.0 aaaaaaaaaaaa want=development:latest bbbbbbbbbbbb") || !strings.Contains(got, "drift: 1") {
		t.Fatalf("drift status:\n%s", got)
	}

	fc = newWorkflowComposeCaller()
	fc.images["development:latest"] = map[string]any{"name": "development", "version": "0.1.0", "digest": digestA}
	if got := status(t, fc, "official/development"); !strings.Contains(got, "workflow MISSING (want=development:latest)") || !strings.Contains(got, "drift: 1") {
		t.Fatalf("unbound status:\n%s", got)
	}

	fc = newWorkflowComposeCaller()
	if got := status(t, fc, "official/development"); !strings.Contains(got, "workflow MISSING (want=development:latest, not built)") {
		t.Fatalf("unbuilt status:\n%s", got)
	}

	fc = newWorkflowComposeCaller()
	fc.bound["DEV"] = bound
	if got := status(t, fc, ""); !strings.Contains(got, "workflow development:0.1.0 aaaaaaaaaaaa (not declared)") || !strings.Contains(got, "drift: 0") {
		t.Fatalf("undeclared status:\n%s", got)
	}
}
