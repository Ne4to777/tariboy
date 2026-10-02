package compose

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/tasks"
)

type taskQueueComposeCaller struct {
	*fakeCaller
	queues map[string]tasks.Queue
	pools  map[string]map[string]tasks.AgentPool
}

func newTaskQueueComposeCaller() *taskQueueComposeCaller {
	return &taskQueueComposeCaller{fakeCaller: newFake(), queues: map[string]tasks.Queue{}, pools: map[string]map[string]tasks.AgentPool{}}
}

func (f *taskQueueComposeCaller) Call(method, route string, body any) (json.RawMessage, error) {
	f.calls = append(f.calls, method+" "+route)
	f.bodies = append(f.bodies, body)
	switch {
	case method == "GET" && route == "/api/task-queues":
		items := []tasks.Queue{}
		for _, item := range f.queues {
			items = append(items, item)
		}
		return mustJSON(map[string]any{"queues": items, "count": len(items)}), nil
	case method == "POST" && route == "/api/task-queues":
		b := body.(map[string]any)
		prefix := b["prefix"].(string)
		q := tasks.Queue{Prefix: prefix, Name: b["name"].(string), Revision: 1}
		f.queues[prefix] = q
		return mustJSON(q), nil
	case method == "PATCH" && strings.HasPrefix(route, "/api/task-queues/") && !strings.Contains(route, "/pools/"):
		prefix := strings.TrimPrefix(route, "/api/task-queues/")
		b := body.(map[string]any)
		q := f.queues[prefix]
		q.Name = b["name"].(string)
		q.Revision++
		f.queues[prefix] = q
		return mustJSON(q), nil
	case method == "GET" && strings.HasSuffix(route, "/pools"):
		queue := strings.Split(route, "/")[3]
		items := []tasks.AgentPool{}
		for _, item := range f.pools[queue] {
			items = append(items, item)
		}
		return mustJSON(map[string]any{"items": items, "count": len(items)}), nil
	case method == "PATCH" && strings.Contains(route, "/pools/"):
		parts := strings.Split(route, "/")
		queue, pool := parts[3], parts[5]
		b := body.(map[string]any)
		if f.pools[queue] == nil {
			f.pools[queue] = map[string]tasks.AgentPool{}
		}
		item := f.pools[queue][pool]
		item.Queue, item.Name = queue, pool
		item.Revision++
		item.Agents = append([]string(nil), b["agents"].([]string)...)
		f.pools[queue][pool] = item
		return mustJSON(item), nil
	default:
		// Undo the recording above because fakeCaller records its own calls.
		f.calls = f.calls[:len(f.calls)-1]
		f.bodies = f.bodies[:len(f.bodies)-1]
		return f.fakeCaller.Call(method, route, body)
	}
}

// fakeCaller records requests and serves canned agent/group/budget state, so
// the reconciler can be exercised without a daemon. It is stateful: POSTed
// groups and budgets are recorded and returned on later GETs, so a second Up
// sees the state the first Up created (the daemon's create-or-update / set
// semantics modelled faithfully).
type fakeCaller struct {
	calls   []string
	bodies  []any // parallel to calls: the raw body/query passed to Call for calls[i]
	agents  map[string]map[string]any
	groups  map[string]map[string]any
	budgets map[string]map[string]any // keyed by scope, in budget.ls shape
	// subs models the daemon's subscription rows keyed by agent name. The POST
	// handler dedups by the same identity the real daemon uses (channel + the
	// filter/params triple), so a re-applied compose file creates no duplicate.
	subs    map[string][]subRow
	logsFor func(name string) json.RawMessage
}

// subRow is a recorded subscription in the fake daemon: the channel plus the
// canonical form of its filter/provider inputs used as the dedup identity.
type subRow struct {
	channel string
	key     string // canonical (type|matcher|params) fingerprint
}

func newFake() *fakeCaller {
	return &fakeCaller{
		agents:  map[string]map[string]any{},
		groups:  map[string]map[string]any{},
		budgets: map[string]map[string]any{},
		subs:    map[string][]subRow{},
	}
}

func (f *fakeCaller) Call(method, route string, body any) (json.RawMessage, error) {
	f.calls = append(f.calls, method+" "+route)
	f.bodies = append(f.bodies, body)
	switch {
	case method == "GET" && route == "/api/agents":
		rows := []map[string]any{}
		for _, a := range f.agents {
			rows = append(rows, a)
		}
		return mustJSON(map[string]any{"agents": rows, "count": len(rows)}), nil
	case method == "GET" && route == "/api/groups":
		rows := []map[string]any{}
		for _, g := range f.groups {
			rows = append(rows, g)
		}
		return mustJSON(map[string]any{"groups": rows, "count": len(rows)}), nil
	case method == "GET" && route == "/api/budgets":
		rows := []map[string]any{}
		for _, b := range f.budgets {
			rows = append(rows, b)
		}
		return mustJSON(map[string]any{"budgets": rows, "count": len(rows)}), nil
	case method == "POST" && route == "/api/groups":
		m := body.(map[string]any)
		name := m["name"].(string)
		f.groups[name] = map[string]any{"name": name, "lead": m["lead"]}
		return mustJSON(map[string]any{"name": name}), nil
	case method == "POST" && route == "/api/agents":
		m := body.(map[string]any)
		name := m["name"].(string)
		enabled := true
		if v, ok := m["loop"].(bool); ok {
			enabled = v
		}
		f.agents[name] = map[string]any{"name": name, "state": "running",
			"group": m["group"], "image": m["image"], "cwd": m["cwd"], "loop_enabled": enabled}
		return mustJSON(map[string]any{"name": name}), nil
	case method == "POST" && strings.HasSuffix(route, "/subscriptions"):
		name := route[len("/api/agents/") : len(route)-len("/subscriptions")]
		m := body.(map[string]any)
		channel := m["channel"].(string)
		// Canonical dedup identity: the daemon is idempotent on channel + the
		// filter/params triple (params folds into the watch, watch into the
		// matcher key). Model that so a re-applied file creates no duplicate.
		key := string(mustJSON([]any{m["type"], m["matcher"], m["params"]}))
		row := subRow{channel: channel, key: key}
		for _, existing := range f.subs[name] {
			if existing == row {
				return mustJSON(map[string]any{"channel": channel, "id": "sub-existing"}), nil
			}
		}
		f.subs[name] = append(f.subs[name], row)
		return mustJSON(map[string]any{"channel": channel, "id": fmt.Sprintf("sub-%d", len(f.subs[name]))}), nil
	case method == "POST" && len(route) > 5 && route[len(route)-4:] == "/cwd":
		name := route[len("/api/agents/") : len(route)-len("/cwd")]
		m := body.(map[string]any)
		if a := f.agents[name]; a != nil {
			a["cwd"] = m["value"]
		}
		return mustJSON(map[string]any{"name": name, "cwd": m["value"]}), nil
	case method == "POST" && strings.HasSuffix(route, "/loop/timeout"):
		name := route[len("/api/agents/") : len(route)-len("/loop/timeout")]
		m := body.(map[string]any)
		if a := f.agents[name]; a != nil {
			a["timeout_s"] = m["value"]
		}
		return mustJSON(map[string]any{"name": name, "timeout_s": m["value"]}), nil
	case method == "POST" && strings.HasSuffix(route, "/loop/interval"):
		name := route[len("/api/agents/") : len(route)-len("/loop/interval")]
		if a := f.agents[name]; a != nil {
			a["interval_s"] = body.(map[string]any)["value"]
		}
		return mustJSON(map[string]any{"name": name}), nil
	case method == "POST" && strings.HasSuffix(route, "/loop/on-timeout"):
		name := route[len("/api/agents/") : len(route)-len("/loop/on-timeout")]
		if a := f.agents[name]; a != nil {
			a["on_timeout"] = body.(map[string]any)["value"]
		}
		return mustJSON(map[string]any{"name": name}), nil
	case method == "POST" && strings.HasSuffix(route, "/loop/on-error"):
		name := route[len("/api/agents/") : len(route)-len("/loop/on-error")]
		if a := f.agents[name]; a != nil {
			a["on_error"] = body.(map[string]any)["value"]
		}
		return mustJSON(map[string]any{"name": name}), nil
	case method == "POST" && strings.HasSuffix(route, "/loop/max-idle"):
		name := route[len("/api/agents/") : len(route)-len("/loop/max-idle")]
		if a := f.agents[name]; a != nil {
			a["max_idle_iterations"] = body.(map[string]any)["value"]
		}
		return mustJSON(map[string]any{"name": name}), nil
	case method == "POST" && strings.HasSuffix(route, "/goal-enabled"):
		name := route[len("/api/agents/") : len(route)-len("/goal-enabled")]
		if a := f.agents[name]; a != nil {
			a["goal_enabled"] = body.(map[string]any)["enabled"]
			if a["goal_enabled"] == false {
				a["current_goal_task_key"] = ""
			}
		}
		return mustJSON(map[string]any{"name": name}), nil
	case method == "POST" && strings.HasSuffix(route, "/goal-wait-customer-timeout"):
		name := route[len("/api/agents/") : len(route)-len("/goal-wait-customer-timeout")]
		if a := f.agents[name]; a != nil {
			a["goal_wait_customer_timeout_s"] = body.(map[string]any)["seconds"]
		}
		return mustJSON(map[string]any{"name": name}), nil
	case method == "POST" && strings.HasSuffix(route, "/goal-delivery-cooldown"):
		name := route[len("/api/agents/") : len(route)-len("/goal-delivery-cooldown")]
		if a := f.agents[name]; a != nil {
			a["goal_delivery_cooldown_s"] = body.(map[string]any)["seconds"]
		}
		return mustJSON(map[string]any{"name": name}), nil
	case method == "POST" && (strings.HasSuffix(route, "/loop/enable") || strings.HasSuffix(route, "/loop/disable")):
		on := strings.HasSuffix(route, "/loop/enable")
		suffix := "/loop/disable"
		if on {
			suffix = "/loop/enable"
		}
		name := route[len("/api/agents/") : len(route)-len(suffix)]
		if a := f.agents[name]; a != nil {
			a["loop_enabled"] = on
		}
		return mustJSON(map[string]any{"name": name}), nil
	case method == "POST" && route == "/api/budgets":
		// Model the daemon: parse the string limit-usd/period into the stored
		// budget.ls shape (limit_usd float, period_s int, mode string).
		m := body.(map[string]any)
		scope := m["scope"].(string)
		limit, _ := strconv.ParseFloat(m["limit-usd"].(string), 64)
		periodS := 86400
		if pr, ok := m["period"].(string); ok && pr != "" {
			if d, err := time.ParseDuration(pr); err == nil {
				periodS = int(d.Seconds())
			}
		}
		f.budgets[scope] = map[string]any{"scope": scope, "limit_usd": limit,
			"period_s": periodS, "mode": m["mode"]}
		return mustJSON(map[string]any{"scope": scope}), nil
	case method == "GET" && len(route) > len("/api/agents/") && route[len(route)-5:] == "/logs":
		name := route[len("/api/agents/") : len(route)-len("/logs")]
		if f.logsFor != nil {
			return f.logsFor(name), nil
		}
		return mustJSON(map[string]any{"lines": []string{}, "count": 0}), nil
	case method == "POST" && strings.HasSuffix(route, "/reprovision"):
		name := route[len("/api/agents/") : len(route)-len("/reprovision")]
		if a := f.agents[name]; a != nil {
			// Reprovision re-unpacks the (possibly new) image and restarts the loop.
			if m, ok := body.(map[string]any); ok {
				if img, ok := m["image"].(string); ok && img != "" {
					a["image"] = img
				}
			}
			a["state"] = "running"
			a["loop_enabled"] = true
		}
		return mustJSON(map[string]any{"name": name, "state": "running"}), nil
	case method == "POST" && len(route) > 6 && route[len(route)-5:] == "/exec":
		return mustJSON(map[string]any{"status": "queued"}), nil
	case method == "DELETE":
		return mustJSON(map[string]any{"removed": true}), nil
	default:
		return mustJSON(map[string]any{"ok": true}), nil
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func countCalls(f *fakeCaller, want string) int {
	n := 0
	for _, c := range f.calls {
		if c == want {
			n++
		}
	}
	return n
}

// bodyFor returns the body/query recorded for the last call matching
// "METHOD route", or nil if no such call was recorded.
func bodyFor(f *fakeCaller, want string) any {
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i] == want {
			return f.bodies[i]
		}
	}
	return nil
}

// hasVolumesTrue reports whether the body is the map[string]string shape
// Runner.Down sends for a group delete, with volumes set to "true".
func hasVolumesTrue(body any) bool {
	m, ok := body.(map[string]string)
	if !ok {
		return false
	}
	return m["volumes"] == "true"
}

// hasPurgeTrue reports whether the body is the map[string]string shape
// Runner.Down sends for an agent delete, with purge set to "true".
func hasPurgeTrue(body any) bool {
	m, ok := body.(map[string]string)
	if !ok {
		return false
	}
	return m["purge"] == "true"
}

// upNoBuild is the reconciler with the local image-build step disabled, so the
// test needs no filesystem image context.
func upNoBuild(t *testing.T, r *Runner, f File) {
	t.Helper()
	r.skipBuild = true
	if err := r.Up(f); err != nil {
		t.Fatalf("up: %v", err)
	}
}

func taskQueueComposeFile() File {
	return File{Version: 1,
		Agents:     map[string]AgentSpec{"dev": {Image: "basic:latest"}},
		TaskQueues: map[string]TaskQueueSpec{"DEV": {Name: "Development", Pools: map[string][]string{"developers": {"dev"}}}},
	}
}

func TestReconcileTaskQueueIsIdempotentAndRebindsPoolDrift(t *testing.T) {
	f := taskQueueComposeFile()
	fc := newTaskQueueComposeCaller()
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, f)
	if got := fc.queues["DEV"].Name; got != "Development" {
		t.Fatalf("queue name = %q", got)
	}
	if got := fc.pools["DEV"]["developers"].Agents; !slices.Equal(got, []string{"dev"}) {
		t.Fatalf("pool agents = %v", got)
	}
	before := len(fc.calls)
	upNoBuild(t, r, f)
	for _, call := range fc.calls[before:] {
		if strings.HasPrefix(call, "POST /api/task-queues") || strings.HasPrefix(call, "PATCH /api/task-queues") || strings.HasPrefix(call, "PUT /api/task-queues") {
			t.Fatalf("second reconcile mutated task state: %s", call)
		}
	}

	fc.pools["DEV"]["developers"] = tasks.AgentPool{Queue: "DEV", Name: "developers", Agents: []string{"other"}, Revision: 2}
	before = len(fc.calls)
	upNoBuild(t, r, f)
	if count := countCallsSince(fc.calls, before, "PATCH /api/task-queues/DEV/pools/developers"); count != 1 {
		t.Fatalf("pool rebind calls = %d", count)
	}
}

func TestStatusReportsTaskQueueNameAndPoolDrift(t *testing.T) {
	f := taskQueueComposeFile()
	fc := newTaskQueueComposeCaller()
	fc.agents["dev"] = map[string]any{"name": "dev", "state": "running", "image": "basic:latest"}
	fc.queues["DEV"] = tasks.Queue{Prefix: "DEV", Name: "Old name", Revision: 1}
	fc.pools["DEV"] = map[string]tasks.AgentPool{"developers": {Queue: "DEV", Name: "developers", Agents: []string{"other"}, Revision: 1}}
	var out strings.Builder
	r := NewRunner(fc, "", "", &out)
	if err := r.Status(f); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "name drift") || !strings.Contains(got, "pool developers drift") {
		t.Fatalf("status output:\n%s", got)
	}
}

func TestStatusReportsMissingTaskQueueWithoutFailing(t *testing.T) {
	f := taskQueueComposeFile()
	fc := newTaskQueueComposeCaller()
	fc.agents["dev"] = map[string]any{"name": "dev", "state": "running", "image": "basic:latest"}
	var out strings.Builder
	r := NewRunner(fc, "", "", &out)
	if err := r.Status(f); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "task queue DEV") || !strings.Contains(got, "MISSING") {
		t.Fatalf("status output:\n%s", got)
	}
}

func countCallsSince(calls []string, start int, want string) int {
	n := 0
	for _, call := range calls[start:] {
		if call == want {
			n++
		}
	}
	return n
}

func TestComposeIdempotencyHashesStructuredInputsWithoutDelimiterCollisions(t *testing.T) {
	left := composeIdempotency("pool", "a:b", "c")
	right := composeIdempotency("pool", "a", "b:c")
	if left == right {
		t.Fatalf("structured inputs collided: %q", left)
	}
	if left != composeIdempotency("pool", "a:b", "c") {
		t.Fatal("idempotency hash is not deterministic")
	}
}

// TestUpForwardsHarnessToCreate pins that a per-agent harness block is applied
// as launch-time overrides on the create body — including interactive, which
// the create endpoint accepts and compose must not silently drop.
func TestUpForwardsHarnessToCreate(t *testing.T) {
	const y = `
version: 1
agents:
  a:
    image: img:latest
    harness:
      type: claude
      model: claude-opus-4-8
      effort: high
      interactive: true
`
	fc := newFake()
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(y))
	upNoBuild(t, r, f)
	body, ok := bodyFor(fc, "POST /api/agents").(map[string]any)
	if !ok {
		t.Fatalf("create body not a map: %#v", bodyFor(fc, "POST /api/agents"))
	}
	if body["harness"] != "claude" || body["model"] != "claude-opus-4-8" || body["effort"] != "high" {
		t.Fatalf("harness/model/effort not forwarded: %#v", body)
	}
	if body["interactive"] != true {
		t.Fatalf("interactive not forwarded: %#v", body)
	}
}

func TestUpCreatesAndConvergesGoalSettings(t *testing.T) {
	off := false
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"worker": {Image: "basic:latest", Goal: &GoalSpec{Enabled: &off, WaitCustomerTimeout: "2m", DeliveryCooldown: "1m"}},
	}}
	fc := newFake()
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, f)
	create := bodyFor(fc, "POST /api/agents").(map[string]any)
	if create["goal_enabled"] != false || create["goal_wait_customer_timeout_s"] != 120 || create["goal_delivery_cooldown_s"] != 60 {
		t.Fatalf("create body = %#v", create)
	}

	fc.calls, fc.bodies = nil, nil
	on := true
	f.Agents["worker"] = AgentSpec{Image: "basic:latest", Goal: &GoalSpec{Enabled: &on, WaitCustomerTimeout: "3m", DeliveryCooldown: "2m"}}
	upNoBuild(t, r, f)
	if got := bodyFor(fc, "POST /api/agents/worker/goal-enabled"); !reflect.DeepEqual(got, map[string]any{"enabled": true}) {
		t.Fatalf("goal-enabled body = %#v", got)
	}
	if got := bodyFor(fc, "POST /api/agents/worker/goal-wait-customer-timeout"); !reflect.DeepEqual(got, map[string]any{"seconds": 180}) {
		t.Fatalf("goal timeout body = %#v", got)
	}
	if got := bodyFor(fc, "POST /api/agents/worker/goal-delivery-cooldown"); !reflect.DeepEqual(got, map[string]any{"seconds": 120}) {
		t.Fatalf("goal cooldown body = %#v", got)
	}
}

func TestUpOmittedGoalSettingsPreserveCurrentValues(t *testing.T) {
	fc := newFake()
	fc.agents["worker"] = map[string]any{
		"name": "worker", "state": "running", "image": "basic:latest", "group": "", "cwd": "",
		"goal_enabled": false, "goal_wait_customer_timeout_s": 17, "current_goal_task_key": "TARI-43",
	}
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, File{Version: 1, Agents: map[string]AgentSpec{"worker": {Image: "basic:latest"}}})
	for _, call := range fc.calls {
		if strings.Contains(call, "/goal-") {
			t.Fatalf("omitted Goal field mutated daemon state: %v", fc.calls)
		}
	}
	if fc.agents["worker"]["goal_enabled"] != false || fc.agents["worker"]["goal_wait_customer_timeout_s"] != 17 || fc.agents["worker"]["current_goal_task_key"] != "TARI-43" {
		t.Fatalf("Goal state changed: %#v", fc.agents["worker"])
	}
}

func TestUpCreatesAndIsIdempotent(t *testing.T) {
	fc := newFake()
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(goodYAML))
	// Reference an already-built image so no local build is attempted.
	upNoBuild(t, r, f)
	if len(fc.agents) != 2 || fc.groups["research-team"] == nil {
		t.Fatalf("first up did not create everything: agents=%v groups=%v", fc.agents, fc.groups)
	}
	if countCalls(fc, "POST /api/agents") != 2 {
		t.Fatalf("expected 2 agent creates, got %d", countCalls(fc, "POST /api/agents"))
	}
	// Second up on an already-converged file: ZERO mutating daemon calls.
	// The reconcile is truly idempotent — no agent creates, and (post diff-first
	// fix) no group or budget POSTs either.
	fc.calls = nil
	upNoBuild(t, r, f)
	if n := countCalls(fc, "POST /api/agents"); n != 0 {
		t.Fatalf("re-up created %d agents, want 0", n)
	}
	if n := countCalls(fc, "POST /api/groups"); n != 0 {
		t.Fatalf("re-up issued %d group POSTs, want 0 (already converged)", n)
	}
	if n := countCalls(fc, "POST /api/budgets"); n != 0 {
		t.Fatalf("re-up issued %d budget POSTs, want 0 (already converged)", n)
	}
}

// TestUpConvergesGroupLeadDrift proves the group skip is conditional: when a
// group already exists but its live lead differs from desired, the reconciler
// must issue exactly one POST /api/groups to converge the lead.
func TestUpConvergesGroupLeadDrift(t *testing.T) {
	fc := newFake()
	// Pre-seed the group with a stale lead so only the lead needs converging.
	fc.groups["research-team"] = map[string]any{"name": "research-team", "lead": "someone-else"}
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(goodYAML))
	upNoBuild(t, r, f)
	if n := countCalls(fc, "POST /api/groups"); n != 1 {
		t.Fatalf("lead drift caused %d group POSTs, want exactly 1", n)
	}
	if got := fmt.Sprintf("%v", fc.groups["research-team"]["lead"]); got != "scout" {
		t.Fatalf("lead did not converge: lead=%q, want scout", got)
	}
}

// TestUpConvergesBudgetDrift proves the budget skip is conditional: when a
// budget with the same scope exists but a field (here limit) differs, the
// reconciler must issue exactly one POST /api/budgets for that scope.
func TestUpConvergesBudgetDrift(t *testing.T) {
	fc := newFake()
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(goodYAML))
	// Converge once so all state exists.
	upNoBuild(t, r, f)
	// Drift the group budget's limit (desired 50) and re-up.
	fc.budgets["group:research-team"]["limit_usd"] = 999.0
	fc.calls = nil
	upNoBuild(t, r, f)
	if n := countCalls(fc, "POST /api/budgets"); n != 1 {
		t.Fatalf("limit drift caused %d budget POSTs, want exactly 1", n)
	}
	if got := fc.budgets["group:research-team"]["limit_usd"].(float64); got != 50 {
		t.Fatalf("budget limit did not converge: limit_usd=%v, want 50", got)
	}
}

func TestUpAppliesBudgets(t *testing.T) {
	fc := newFake()
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(goodYAML))
	upNoBuild(t, r, f)
	// group + per-agent budgets => 2 budget.set calls.
	if n := countCalls(fc, "POST /api/budgets"); n != 2 {
		t.Fatalf("budget.set calls = %d, want 2 (%v)", n, fc.calls)
	}
}

// TestUpReportsDriftWithoutMutating pins the locked M8 decision: agent field
// drift is GROUP-only. When an existing agent's group already matches desired
// but a NON-group field (here model) drifts, the reconciler must NOT mutate it
// (there is no agent.update in M8) — no create, no re-assign. Surfacing such
// drift to the operator is the job of `compose status` via liveAgents, not Up.
func TestUpReportsDriftWithoutMutating(t *testing.T) {
	fc := newFake()
	fc.agents["scout"] = map[string]any{"name": "scout", "state": "running", "group": "research-team", "image": "analyst:latest", "model": "claude-sonnet"}
	fc.agents["writer"] = map[string]any{"name": "writer", "state": "running", "group": "research-team", "image": "analyst:latest"}
	fc.groups["research-team"] = map[string]any{"name": "research-team", "lead": "scout"}
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(goodYAML))
	upNoBuild(t, r, f)
	if n := countCalls(fc, "POST /api/agents"); n != 0 {
		t.Fatalf("drift caused %d agent creates, want 0 (no agent.update in M8)", n)
	}
	if n := countCalls(fc, "POST /api/groups/research-team/assign"); n != 0 {
		t.Fatalf("group already converged but reconciler issued %d assign calls", n)
	}
	if got := fmt.Sprintf("%v", fc.agents["scout"]["model"]); got != "claude-sonnet" {
		t.Fatalf("reconciler mutated a non-group field: model=%q", got)
	}
}

var _ = fmt.Sprintf

// TestUpAppliesAndConvergesCwd covers the three cwd paths: create sends the
// resolved cwd; an existing agent whose live cwd drifts gets a converge POST;
// an already-converged agent gets none.
func TestUpAppliesAndConvergesCwd(t *testing.T) {
	dir := t.TempDir()
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"a": {Image: "img:latest", Cwd: "$CWD"},
	}}

	// create: body carries the resolved cwd (= compose dir).
	fc := newFake()
	r := NewRunner(fc, "", dir, io.Discard)
	upNoBuild(t, r, f)
	body, _ := bodyFor(fc, "POST /api/agents").(map[string]any)
	if body == nil || fmt.Sprintf("%v", body["cwd"]) != dir {
		t.Fatalf("create body cwd = %v, want %s", body, dir)
	}

	// converge: existing agent with drifting cwd -> exactly one POST .../cwd.
	fc2 := newFake()
	fc2.agents["a"] = map[string]any{"name": "a", "image": "img:latest", "cwd": "/old"}
	r2 := NewRunner(fc2, "", dir, io.Discard)
	upNoBuild(t, r2, f)
	if n := countCalls(fc2, "POST /api/agents/a/cwd"); n != 1 {
		t.Fatalf("cwd convergence POSTs = %d, want 1", n)
	}
	if got := fmt.Sprintf("%v", fc2.agents["a"]["cwd"]); got != dir {
		t.Fatalf("cwd did not converge: %q want %s", got, dir)
	}

	// converged: no cwd call when live already matches.
	fc3 := newFake()
	fc3.agents["a"] = map[string]any{"name": "a", "image": "img:latest", "cwd": dir}
	r3 := NewRunner(fc3, "", dir, io.Discard)
	upNoBuild(t, r3, f)
	if n := countCalls(fc3, "POST /api/agents/a/cwd"); n != 0 {
		t.Fatalf("converged cwd caused %d POSTs, want 0", n)
	}
}

// TestUpAppliesAndConvergesTimeout covers the three timeout paths: create sends
// the parsed seconds via /loop/timeout; an existing agent whose live timeout_s
// drifts gets a converge POST; an already-converged agent gets none. An agent
// that omits timeout gets no timeout call at all.
func TestUpAppliesAndConvergesTimeout(t *testing.T) {
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"a": {Image: "img:latest", Timeout: "60m"},
	}}

	// create: a fresh agent gets the parsed seconds (60m = 3600s).
	fc := newFake()
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, f)
	body, _ := bodyFor(fc, "POST /api/agents/a/loop/timeout").(map[string]any)
	if body == nil || fmt.Sprintf("%v", body["value"]) != "3600" {
		t.Fatalf("create timeout value = %v, want 3600", body)
	}

	// converge: existing agent with drifting timeout_s -> exactly one POST.
	fc2 := newFake()
	fc2.agents["a"] = map[string]any{"name": "a", "image": "img:latest", "timeout_s": float64(10)}
	r2 := NewRunner(fc2, "", "", io.Discard)
	upNoBuild(t, r2, f)
	if n := countCalls(fc2, "POST /api/agents/a/loop/timeout"); n != 1 {
		t.Fatalf("timeout convergence POSTs = %d, want 1", n)
	}
	if got := fmt.Sprintf("%v", fc2.agents["a"]["timeout_s"]); got != "3600" {
		t.Fatalf("timeout did not converge: %q want 3600", got)
	}

	// converged: no timeout call when live already matches.
	fc3 := newFake()
	fc3.agents["a"] = map[string]any{"name": "a", "image": "img:latest", "timeout_s": float64(3600)}
	r3 := NewRunner(fc3, "", "", io.Discard)
	upNoBuild(t, r3, f)
	if n := countCalls(fc3, "POST /api/agents/a/loop/timeout"); n != 0 {
		t.Fatalf("converged timeout caused %d POSTs, want 0", n)
	}

	// omitted: an agent without timeout makes no timeout call.
	fOmit := File{Version: 1, Agents: map[string]AgentSpec{"b": {Image: "img:latest"}}}
	fc4 := newFake()
	r4 := NewRunner(fc4, "", "", io.Discard)
	upNoBuild(t, r4, fOmit)
	if n := countCalls(fc4, "POST /api/agents/b/loop/timeout"); n != 0 {
		t.Fatalf("omitted timeout caused %d POSTs, want 0", n)
	}
}

// TestUpDefaultsCwdToComposeDir: an agent that omits cwd still runs in the
// compose file's directory (the project), both on create and on convergence.
func TestUpDefaultsCwdToComposeDir(t *testing.T) {
	dir := t.TempDir()
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"a": {Image: "img:latest"}, // no Cwd
	}}

	// create: body carries the default cwd (= compose dir).
	fc := newFake()
	r := NewRunner(fc, "", dir, io.Discard)
	upNoBuild(t, r, f)
	body, _ := bodyFor(fc, "POST /api/agents").(map[string]any)
	if body == nil || fmt.Sprintf("%v", body["cwd"]) != dir {
		t.Fatalf("create body cwd = %v, want %s", body, dir)
	}

	// converge: existing agent whose live cwd differs gets a converge POST.
	fc2 := newFake()
	fc2.agents["a"] = map[string]any{"name": "a", "image": "img:latest", "cwd": "/old"}
	r2 := NewRunner(fc2, "", dir, io.Discard)
	upNoBuild(t, r2, f)
	if n := countCalls(fc2, "POST /api/agents/a/cwd"); n != 1 {
		t.Fatalf("default-cwd convergence POSTs = %d, want 1", n)
	}
	if got := fmt.Sprintf("%v", fc2.agents["a"]["cwd"]); got != dir {
		t.Fatalf("cwd did not converge to compose dir: %q want %s", got, dir)
	}
}

func TestUpConvergesLoopOnCreate(t *testing.T) {
	fc := newFake()
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"a": {Image: "i:latest", Loop: &LoopSpec{Interval: "5m", Timeout: "60m", OnTimeout: "restart", OnError: "stop"}},
	}}
	r := &Runner{call: fc, out: io.Discard, skipBuild: true}
	if err := r.Up(f); err != nil {
		t.Fatal(err)
	}
	if b, _ := bodyFor(fc, "POST /api/agents/a/loop/interval").(map[string]any); b == nil || fmt.Sprintf("%v", b["value"]) != "300" {
		t.Errorf("interval not set to 300: %v", b)
	}
	if b, _ := bodyFor(fc, "POST /api/agents/a/loop/on-timeout").(map[string]any); b == nil || b["value"] != "restart" {
		t.Errorf("on-timeout not set to restart: %v", b)
	}
	if b, _ := bodyFor(fc, "POST /api/agents/a/loop/on-error").(map[string]any); b == nil || b["value"] != "stop" {
		t.Errorf("on-error not set to stop: %v", b)
	}
	if b, _ := bodyFor(fc, "POST /api/agents/a/loop/timeout").(map[string]any); b == nil || fmt.Sprintf("%v", b["value"]) != "3600" {
		t.Errorf("timeout not set to 3600 (loop.timeout wins): %v", b)
	}
}

func TestUpCreatesWithLoopDisabled(t *testing.T) {
	fc := newFake()
	off := false
	f := File{Version: 1, Agents: map[string]AgentSpec{"a": {Image: "i:latest", Loop: &LoopSpec{Enabled: &off}}}}
	r := &Runner{call: fc, out: io.Discard, skipBuild: true}
	if err := r.Up(f); err != nil {
		t.Fatal(err)
	}
	b := bodyFor(fc, "POST /api/agents").(map[string]any)
	if b["loop"] != false {
		t.Errorf("create body loop = %v, want false", b["loop"])
	}
}

// TestUpStartsFreshCreateWhenEnabled is the C1 regression guard: since agents
// are now created disabled by default, a fresh `compose up` (agent not yet
// live) must explicitly POST /api/agents/{name}/start when the spec's loop is
// enabled — otherwise every newly created agent would sit stopped forever.
func TestUpStartsFreshCreateWhenEnabled(t *testing.T) {
	fc := newFake()
	f := File{Version: 1, Agents: map[string]AgentSpec{"a": {Image: "i:latest"}}}
	r := &Runner{call: fc, out: io.Discard, skipBuild: true}
	if err := r.Up(f); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(fc, "POST /api/agents/a/start"); n != 1 {
		t.Fatalf("fresh enabled create issued %d start calls, want exactly 1 (%v)", n, fc.calls)
	}
}

// TestUpDoesNotStartFreshCreateWhenLoopDisabled is the C1 counterpart: a fresh
// create whose spec sets loop.enabled:false must stay stopped — no start call.
func TestUpDoesNotStartFreshCreateWhenLoopDisabled(t *testing.T) {
	fc := newFake()
	off := false
	f := File{Version: 1, Agents: map[string]AgentSpec{"a": {Image: "i:latest", Loop: &LoopSpec{Enabled: &off}}}}
	r := &Runner{call: fc, out: io.Discard, skipBuild: true}
	if err := r.Up(f); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(fc, "POST /api/agents/a/start"); n != 0 {
		t.Fatalf("loop.enabled:false create issued %d start calls, want 0 (%v)", n, fc.calls)
	}
}

// TestUpReprovisionLoopStateMatchesCreate is the gaa.7 regression guard: a
// stopped agent re-provisioned by `up` must end in the SAME loop-enabled state
// a first-ever create would produce for the same file. Reprovision blanket-
// forces loop_enabled=true (to honour the standalone /reprovision restart
// contract), so unless compose Up re-reads the post-reprovision row the converge
// step would diff the file against the stale pre-reprovision row (loop_enabled
// =false, as a preserving `down` leaves it) and, for loop.enabled:false, see no
// drift and leave the loop wrongly running. Each case starts from a stopped,
// loop-disabled row and asserts the final live loop-enabled state after `up`
// equals what create produces (no block -> enabled by create default; enabled
// :false -> disabled; enabled:true -> enabled).
func TestUpReprovisionLoopStateMatchesCreate(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		loop *LoopSpec
		want bool // final loop_enabled after up, == first-ever create for this file
	}{
		{"no-loop-block", nil, true},                       // create defaults loop:true
		{"enabled-false", &LoopSpec{Enabled: &off}, false}, // the bug this fixes
		{"enabled-true", &LoopSpec{Enabled: &on}, true},    // image-swap-restart guard
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFake()
			fc.agents["a"] = map[string]any{"name": "a", "state": "stopped",
				"image": "i:latest", "group": "", "cwd": "", "loop_enabled": false}
			f := File{Version: 1, Agents: map[string]AgentSpec{
				"a": {Image: "i:latest", Loop: tc.loop},
			}}
			r := &Runner{call: fc, out: io.Discard, skipBuild: true}
			if err := r.Up(f); err != nil {
				t.Fatal(err)
			}
			if countCalls(fc, "POST /api/agents/a/reprovision") != 1 {
				t.Fatalf("agent was not reprovisioned exactly once: %v", fc.calls)
			}
			if countCalls(fc, "POST /api/agents") != 0 {
				t.Fatalf("stopped agent was recreated instead of reprovisioned: %v", fc.calls)
			}
			if got := fc.agents["a"]["loop_enabled"]; got != tc.want {
				t.Fatalf("final loop_enabled = %v, want %v (calls: %v)", got, tc.want, fc.calls)
			}
		})
	}
}

func TestUpConvergedLoopMakesNoCalls(t *testing.T) {
	fc := newFake()
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"a": {Image: "i:latest", Loop: &LoopSpec{Interval: "5m", OnTimeout: "restart"}},
	}}
	r := &Runner{call: fc, out: io.Discard, skipBuild: true}
	if err := r.Up(f); err != nil { // create + set
		t.Fatal(err)
	}
	fc2 := &fakeCaller{agents: fc.agents, groups: fc.groups, budgets: fc.budgets} // reuse converged state
	r2 := &Runner{call: fc2, out: io.Discard, skipBuild: true}
	if err := r2.Up(f); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/loop/interval", "/loop/on-timeout", "/loop/on-error"} {
		n := 0
		for _, c := range fc2.calls {
			if strings.HasSuffix(c, suffix) {
				n++
			}
		}
		if n != 0 {
			t.Errorf("converged file re-POSTed %s (%d times)", suffix, n)
		}
	}
}

func TestStatusReportsIntervalDrift(t *testing.T) {
	fc := newFake()
	fc.agents["a"] = map[string]any{"name": "a", "state": "running", "image": "i:latest",
		"group": "", "cwd": "", "interval_s": float64(120)}
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"a": {Image: "i:latest", Loop: &LoopSpec{Interval: "5m"}},
	}}
	var buf strings.Builder
	r := &Runner{call: fc, out: &buf, skipBuild: true}
	if err := r.Status(f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "interval drift") {
		t.Errorf("expected interval drift, got:\n%s", buf.String())
	}
}

// TestUpConvergesMaxIdle: loop.max_idle_iterations converges onto the agent both
// as a create-time endpoint call and as the persisted value.
func TestUpConvergesMaxIdle(t *testing.T) {
	fc := newFake()
	three := 3
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"a": {Image: "i:latest", Loop: &LoopSpec{MaxIdleIterations: &three}},
	}}
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, f)
	if b, _ := bodyFor(fc, "POST /api/agents/a/loop/max-idle").(map[string]any); b == nil || fmt.Sprintf("%v", b["value"]) != "3" {
		t.Errorf("max-idle not set to 3: %v", b)
	}
	if got := fmt.Sprintf("%v", fc.agents["a"]["max_idle_iterations"]); got != "3" {
		t.Errorf("agent max_idle_iterations did not converge to 3: %q", got)
	}
}

// TestUpOmittedMaxIdleMakesNoCall: leaving max_idle_iterations unset never POSTs
// the loop/max-idle endpoint, so the daemon default (0, disabled) is untouched.
func TestUpOmittedMaxIdleMakesNoCall(t *testing.T) {
	fc := newFake()
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"a": {Image: "i:latest", Loop: &LoopSpec{Interval: "5m"}},
	}}
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, f)
	if n := countCalls(fc, "POST /api/agents/a/loop/max-idle"); n != 0 {
		t.Errorf("omitted max-idle caused %d POSTs, want 0", n)
	}
}

// TestUpReconvergesMaxIdleDrift: an existing agent whose live max_idle_iterations
// differs from the file gets exactly one converge POST; a converged value makes
// none.
func TestUpReconvergesMaxIdleDrift(t *testing.T) {
	five := 5
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"a": {Image: "i:latest", Loop: &LoopSpec{MaxIdleIterations: &five}},
	}}

	// drift: live value 1 != want 5 -> one converge POST, value re-converged.
	fc := newFake()
	fc.agents["a"] = map[string]any{"name": "a", "image": "i:latest", "max_idle_iterations": float64(1)}
	r := NewRunner(fc, "", "", io.Discard)
	upNoBuild(t, r, f)
	if n := countCalls(fc, "POST /api/agents/a/loop/max-idle"); n != 1 {
		t.Fatalf("max-idle drift POSTs = %d, want 1", n)
	}
	if got := fmt.Sprintf("%v", fc.agents["a"]["max_idle_iterations"]); got != "5" {
		t.Fatalf("max_idle_iterations did not re-converge to 5: %q", got)
	}

	// converged: live already matches -> no POST.
	fc2 := newFake()
	fc2.agents["a"] = map[string]any{"name": "a", "image": "i:latest", "max_idle_iterations": float64(5)}
	r2 := NewRunner(fc2, "", "", io.Discard)
	upNoBuild(t, r2, f)
	if n := countCalls(fc2, "POST /api/agents/a/loop/max-idle"); n != 0 {
		t.Errorf("converged max-idle caused %d POSTs, want 0", n)
	}
}

// TestStatusReportsMaxIdleDrift: status surfaces a max_idle_iterations mismatch
// as drift without mutating the agent.
func TestStatusReportsMaxIdleDrift(t *testing.T) {
	fc := newFake()
	fc.agents["a"] = map[string]any{"name": "a", "state": "running", "image": "i:latest",
		"group": "", "cwd": "", "max_idle_iterations": float64(1)}
	three := 3
	f := File{Version: 1, Agents: map[string]AgentSpec{
		"a": {Image: "i:latest", Loop: &LoopSpec{MaxIdleIterations: &three}},
	}}
	var buf strings.Builder
	r := NewRunner(fc, "", "", &buf)
	if err := r.Status(f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "max_idle_iterations drift") {
		t.Errorf("expected max_idle_iterations drift, got:\n%s", buf.String())
	}
}

// TestSubscribeParsesBothForms proves a subscribe: list accepts both the bare
// string form (channel only) and the object form {channel, type, matcher,
// params} in one document.
func TestSubscribeParsesBothForms(t *testing.T) {
	const y = `
version: 1
agents:
  a:
    image: img:latest
    subscribe:
      - group:dev:broadcast
      - channel: ci:events
        type: "run.finished"
        matcher:
          data.status: failed
      - channel: issue-provider:query
        params:
          query: "Queue: PROJ AND Status: Open"
`
	f, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	subs := f.Agents["a"].Subscribe
	if len(subs) != 3 {
		t.Fatalf("want 3 subscribe entries, got %d: %#v", len(subs), subs)
	}
	// String form -> channel only, no filters/params.
	if subs[0].Channel != "group:dev:broadcast" || subs[0].Type != "" ||
		len(subs[0].Matcher) != 0 || len(subs[0].Params) != 0 {
		t.Fatalf("string form parsed wrong: %#v", subs[0])
	}
	// Filtered object form.
	if subs[1].Channel != "ci:events" || subs[1].Type != "run.finished" ||
		subs[1].Matcher["data.status"] != "failed" || len(subs[1].Params) != 0 {
		t.Fatalf("filtered object form parsed wrong: %#v", subs[1])
	}
	// Parameterized object form.
	if subs[2].Channel != "issue-provider:query" ||
		subs[2].Params["query"] != "Queue: PROJ AND Status: Open" {
		t.Fatalf("parameterized object form parsed wrong: %#v", subs[2])
	}
}

// TestValidateRejectsSubscribeWithoutChannel pins that an object-form entry with
// no channel is a schema error, not a silent no-op sub.
func TestValidateRejectsSubscribeWithoutChannel(t *testing.T) {
	const y = `
version: 1
agents:
  a:
    image: img:latest
    subscribe:
      - type: "run.finished"
`
	f, _ := Parse([]byte(y))
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "no channel") {
		t.Fatalf("want a 'no channel' validation error, got %v", err)
	}
}

// TestUpSubscribesBothForms proves Up() POSTs one subscription per declared
// entry to /api/agents/{name}/subscriptions, forwarding type/matcher/params
// unchanged so the daemon can filter and validate provider params at apply time.
func TestUpSubscribesBothForms(t *testing.T) {
	const y = `
version: 1
agents:
  a:
    image: img:latest
    subscribe:
      - group:dev:broadcast
      - channel: ci:events
        type: "run.finished"
        matcher:
          data.status: failed
      - channel: issue-provider:query
        params:
          query: "Q1"
`
	fc := newFake()
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(y))
	upNoBuild(t, r, f)

	if n := countCalls(fc, "POST /api/agents/a/subscriptions"); n != 3 {
		t.Fatalf("want 3 subscribe POSTs, got %d", n)
	}
	if len(fc.subs["a"]) != 3 {
		t.Fatalf("fake daemon holds %d subs, want 3: %#v", len(fc.subs["a"]), fc.subs["a"])
	}
	// The parameterized entry's body must carry params verbatim.
	var paramBody map[string]any
	for i, c := range fc.calls {
		if c != "POST /api/agents/a/subscriptions" {
			continue
		}
		if m, ok := fc.bodies[i].(map[string]any); ok && m["channel"] == "issue-provider:query" {
			paramBody = m
		}
	}
	if paramBody == nil {
		t.Fatal("no subscribe POST for issue-provider:query recorded")
	}
	params, ok := paramBody["params"].(map[string]any)
	if !ok || params["query"] != "Q1" {
		t.Fatalf("params not forwarded on parameterized sub: %#v", paramBody)
	}
	// The plain entry carries neither matcher nor params.
	var plainBody map[string]any
	for i, c := range fc.calls {
		if c != "POST /api/agents/a/subscriptions" {
			continue
		}
		if m, ok := fc.bodies[i].(map[string]any); ok && m["channel"] == "group:dev:broadcast" {
			plainBody = m
		}
	}
	if plainBody == nil {
		t.Fatal("no subscribe POST for group:dev:broadcast recorded")
	}
	if _, has := plainBody["matcher"]; has {
		t.Fatalf("plain sub should not carry matcher: %#v", plainBody)
	}
	if _, has := plainBody["params"]; has {
		t.Fatalf("plain sub should not carry params: %#v", plainBody)
	}
}

// TestUpSubscribeIdempotent proves re-applying a converged file creates no
// duplicate subscription: the daemon dedups on channel + filter/params, so a
// second Up leaves the fake daemon holding exactly the desired set (same params
// -> same watch -> same row).
func TestUpSubscribeIdempotent(t *testing.T) {
	const y = `
version: 1
agents:
  a:
    image: img:latest
    subscribe:
      - group:dev:broadcast
      - channel: issue-provider:query
        params:
          query: "Q1"
`
	fc := newFake()
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(y))
	upNoBuild(t, r, f)
	upNoBuild(t, r, f)
	if len(fc.subs["a"]) != 2 {
		t.Fatalf("after re-apply the daemon holds %d subs, want 2 (no duplicates): %#v",
			len(fc.subs["a"]), fc.subs["a"])
	}
}
