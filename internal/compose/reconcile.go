package compose

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/tasks"
)

// Caller is the daemon HTTP client surface the reconciler drives (satisfied by
// *client.Client).
type Caller interface {
	Call(method, route string, body any) (json.RawMessage, error)
}

// Runner converges a compose File against a live daemon.
type Runner struct {
	call      Caller
	workdir   string // compose-file dir, to resolve relative image contexts
	out       io.Writer
	skipBuild bool // test seam: skip the image build
}

func NewRunner(call Caller, _ string, workdir string, out io.Writer) *Runner {
	return &Runner{call: call, workdir: workdir, out: out}
}

func (r *Runner) logf(format string, a ...any) {
	if r.out != nil {
		fmt.Fprintf(r.out, format+"\n", a...)
	}
}

// Up reconciles desired (the file) → actual (the daemon): build, ensure groups,
// ensure agents, apply budgets (spec §5.2). Idempotent.
func (r *Runner) Up(f File) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if err := r.Build(f); err != nil {
		return err
	}
	// Ensure groups first so lead/inbox wiring exists before members join.
	// Diff-first: skip the create-or-update POST when the group already exists
	// with the desired lead, so an already-converged file makes no group calls.
	liveG, err := r.liveGroups()
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(f.Groups) {
		g := f.Groups[name]
		if cur, ok := liveG[name]; ok && fmt.Sprintf("%v", cur["lead"]) == g.Lead {
			continue // already converged (present + lead matches)
		}
		r.logf("ensuring group %s (lead %s)", name, g.Lead)
		if _, err := r.call.Call("POST", "/api/groups",
			map[string]any{"name": name, "lead": g.Lead}); err != nil {
			return fmt.Errorf("ensure group %s: %w", name, err)
		}
	}
	live, err := r.liveAgents()
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(f.Agents) {
		a := f.Agents[name]
		cur, exists := live[name]
		if !exists {
			r.logf("creating agent %s", name)
			enabled := true
			if a.Loop != nil && a.Loop.Enabled != nil {
				enabled = *a.Loop.Enabled
			}
			body := map[string]any{"image": a.Image, "name": name, "loop": enabled}
			if a.Group != "" {
				body["group"] = a.Group
			}
			if a.Harness != nil {
				if a.Harness.Type != "" {
					body["harness"] = a.Harness.Type
				}
				if a.Harness.Model != "" {
					body["model"] = a.Harness.Model
				}
				if a.Harness.Effort != "" {
					body["effort"] = a.Harness.Effort
				}
				if a.Harness.Interactive {
					body["interactive"] = true
				}
			}
			if len(a.Env) > 0 {
				body["env"] = kvList(a.Env)
			}
			if len(a.Plugins) > 0 {
				body["plugins"] = joinComma(a.Plugins)
			}
			if cwd := effectiveCwd(a.Cwd, r.workdir); cwd != "" {
				body["cwd"] = cwd
			}
			if a.Goal != nil {
				if a.Goal.Enabled != nil {
					body["goal_enabled"] = *a.Goal.Enabled
				}
				if seconds, set, err := a.goalWaitCustomerTimeoutSeconds(); err != nil {
					return fmt.Errorf("agent %s goal: %w", name, err)
				} else if set {
					body["goal_wait_customer_timeout_s"] = seconds
				}
				if seconds, set, err := a.goalDeliveryCooldownSeconds(); err != nil {
					return fmt.Errorf("agent %s goal: %w", name, err)
				} else if set {
					body["goal_delivery_cooldown_s"] = seconds
				}
			}
			if _, err := r.call.Call("POST", "/api/agents", body); err != nil {
				return fmt.Errorf("create agent %s: %w", name, err)
			}
			// The create endpoint takes no timeout/interval/policy, so set them
			// out of band.
			if err := r.convergeTimeout(name, a, nil); err != nil {
				return err
			}
			if err := r.convergeLoop(name, a, nil); err != nil {
				return err
			}
			if err := r.convergeSubscriptions(name, a); err != nil {
				return err
			}
			if enabled {
				if _, err := r.call.Call("POST", "/api/agents/"+name+"/start", nil); err != nil {
					return fmt.Errorf("start agent %s: %w", name, err)
				}
			}
			continue
		}
		// A stopped agent that still has its DB row — e.g. after a data-preserving
		// `down` — must be re-provisioned in place before the rest of the converge
		// runs: re-unpack the file's (possibly new) image over the retained
		// history and restart its loop. This is what makes `down` (preserve) then
		// `up` swap the image while keeping CONTEXT.md / iterations / audit.jsonl.
		if fmt.Sprintf("%v", cur["state"]) == "stopped" {
			r.logf("reprovisioning stopped agent %s (image %s)", name, a.Image)
			if _, err := r.call.Call("POST", "/api/agents/"+name+"/reprovision",
				map[string]any{"image": a.Image}); err != nil {
				return fmt.Errorf("reprovision agent %s: %w", name, err)
			}
			// Reprovision blanket-forces LoopEnabled=true and restarts the loop
			// (loop/manager.go) to honour the standalone /reprovision restart
			// contract. That leaves `cur` — fetched before reprovision — stale, so
			// the converge steps below would diff the file against a pre-reprovision
			// row and miss the forced enable: a preserved agent whose file says
			// loop.enabled:false would stay enabled after `up`, breaking
			// down(preserve)+up symmetry with a first-ever create. Re-fetch the
			// post-reprovision row so convergeLoop sees loop_enabled=true and, for a
			// file that wants it off, drives it back off. No-loop-block and
			// enabled:true files stay enabled, matching create.
			fresh, err := r.liveAgents()
			if err != nil {
				return fmt.Errorf("re-fetch reprovisioned agent %s: %w", name, err)
			}
			if row, ok := fresh[name]; ok {
				cur = row
			}
		}
		// Drift: (re)assign the group if it changed.
		if a.Group != "" && fmt.Sprintf("%v", cur["group"]) != a.Group {
			r.logf("assigning agent %s to group %s", name, a.Group)
			if _, err := r.call.Call("POST", "/api/groups/"+a.Group+"/assign",
				map[string]any{"agent": name}); err != nil {
				return fmt.Errorf("assign agent %s: %w", name, err)
			}
		}
		// Drift: converge cwd toward the file's value, defaulting to the compose
		// file's directory when none is specified so the agent runs on the project.
		if want := effectiveCwd(a.Cwd, r.workdir); want != "" {
			if fmt.Sprintf("%v", cur["cwd"]) != want {
				r.logf("setting cwd for agent %s -> %s", name, want)
				if _, err := r.call.Call("POST", "/api/agents/"+name+"/cwd",
					map[string]any{"value": want}); err != nil {
					return fmt.Errorf("set cwd %s: %w", name, err)
				}
			}
		}
		// Drift: converge the soft timeout toward the file's value.
		if err := r.convergeTimeout(name, a, cur); err != nil {
			return err
		}
		// Drift: converge loop cadence/policy (interval, on_timeout, on_error,
		// enabled) toward the file's value.
		if err := r.convergeLoop(name, a, cur); err != nil {
			return err
		}
		if err := r.convergeGoal(name, a, cur); err != nil {
			return err
		}
		// Drift: converge the agent's declared subscriptions toward the file.
		if err := r.convergeSubscriptions(name, a); err != nil {
			return err
		}
	}
	if err := r.applyBudgets(f); err != nil {
		return err
	}
	return r.convergeTaskQueues(f)
}

func (r *Runner) convergeGoal(name string, a AgentSpec, cur map[string]any) error {
	if a.Goal == nil {
		return nil
	}
	if a.Goal.Enabled != nil {
		want := *a.Goal.Enabled
		if fmt.Sprintf("%v", cur["goal_enabled"]) != fmt.Sprintf("%v", want) {
			if _, err := r.call.Call("POST", "/api/agents/"+name+"/goal-enabled", map[string]any{"enabled": want}); err != nil {
				return fmt.Errorf("set goal enabled %s: %w", name, err)
			}
		}
	}
	seconds, set, err := a.goalWaitCustomerTimeoutSeconds()
	if err != nil {
		return fmt.Errorf("agent %s goal: %w", name, err)
	}
	if set && fmt.Sprintf("%v", cur["goal_wait_customer_timeout_s"]) != fmt.Sprintf("%d", seconds) {
		if _, err := r.call.Call("POST", "/api/agents/"+name+"/goal-wait-customer-timeout", map[string]any{"seconds": seconds}); err != nil {
			return fmt.Errorf("set goal wait customer timeout %s: %w", name, err)
		}
	}
	seconds, set, err = a.goalDeliveryCooldownSeconds()
	if err != nil {
		return fmt.Errorf("agent %s goal: %w", name, err)
	}
	if set && fmt.Sprintf("%v", cur["goal_delivery_cooldown_s"]) != fmt.Sprintf("%d", seconds) {
		if _, err := r.call.Call("POST", "/api/agents/"+name+"/goal-delivery-cooldown", map[string]any{"seconds": seconds}); err != nil {
			return fmt.Errorf("set goal delivery cooldown %s: %w", name, err)
		}
	}
	return nil
}

// convergeTaskQueues drives only the typed native Tasks REST commands, whose
// daemon handlers are backed by registry.TaskControl. Compose never reaches
// into task persistence directly.
func (r *Runner) convergeTaskQueues(f File) error {
	if len(f.TaskQueues) == 0 {
		return nil
	}
	queues, err := r.listTaskQueues()
	if err != nil {
		return err
	}
	for _, prefix := range sortedKeys(f.TaskQueues) {
		want := f.TaskQueues[prefix]
		queue, exists := queues[prefix]
		if !exists {
			r.logf("creating task queue %s", prefix)
			raw, err := r.call.Call("POST", "/api/task-queues", map[string]any{"prefix": prefix, "name": want.Name})
			if err != nil {
				return fmt.Errorf("create task queue %s: %w", prefix, err)
			}
			if err := json.Unmarshal(raw, &queue); err != nil {
				return err
			}
		} else if queue.Name != want.Name {
			r.logf("updating task queue %s", prefix)
			raw, err := r.call.Call("PATCH", "/api/task-queues/"+prefix, map[string]any{"name": want.Name, "revision": queue.Revision})
			if err != nil {
				return fmt.Errorf("update task queue %s: %w", prefix, err)
			}
			if err := json.Unmarshal(raw, &queue); err != nil {
				return err
			}
		}
		pools, err := r.listTaskPools(prefix)
		if err != nil {
			return err
		}
		for _, poolName := range sortedKeys(want.Pools) {
			agents := normalizeAgentNames(want.Pools[poolName])
			current, found := pools[poolName]
			if found && reflect.DeepEqual(current.Agents, agents) {
				continue
			}
			revision := int64(0)
			if found {
				revision = current.Revision
			}
			r.logf("binding task queue %s pool %s", prefix, poolName)
			if _, err := r.call.Call("PATCH", "/api/task-queues/"+prefix+"/pools/"+poolName, map[string]any{"agents": agents, "revision": revision, "idempotency_key": composeIdempotency("pool", prefix, poolName, revision, agents)}); err != nil {
				return fmt.Errorf("bind pool %s/%s: %w", prefix, poolName, err)
			}
		}
	}
	return nil
}

func composeIdempotency(parts ...any) string {
	encoded, err := json.Marshal(parts)
	if err != nil {
		panic(fmt.Sprintf("compose idempotency inputs: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("compose:%x", sum[:])
}

func (r *Runner) listTaskQueues() (map[string]tasks.Queue, error) {
	raw, err := r.call.Call("GET", "/api/task-queues", map[string]string{})
	if err != nil {
		return nil, err
	}
	var env struct {
		Queues []tasks.Queue `json:"queues"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	out := map[string]tasks.Queue{}
	for _, queue := range env.Queues {
		out[queue.Prefix] = queue
	}
	return out, nil
}
func (r *Runner) listTaskPools(queue string) (map[string]tasks.AgentPool, error) {
	raw, err := r.call.Call("GET", "/api/task-queues/"+queue+"/pools", map[string]string{})
	if err != nil {
		return nil, err
	}
	var env struct {
		Items []tasks.AgentPool `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	out := map[string]tasks.AgentPool{}
	for _, pool := range env.Items {
		out[pool.Name] = pool
	}
	return out, nil
}

// convergeSubscriptions ensures every subscription the file declares for an
// agent exists on the daemon (spec §5.3). It POSTs each desired entry to
// /api/agents/{name}/subscriptions with {channel, type?, matcher?, params?};
// the daemon is idempotent on (agent, channel, matcher) — and, for a
// parameterized entry, the watch fingerprint of (channel, params) is folded
// into the matcher key — so re-applying a converged file creates no duplicate
// subscription (same params → same watch → same row). Params are validated
// against the channel's provider schema by the daemon at this apply time; a bad
// params object fails the POST loudly rather than silently.
//
// Unlike the diff-first converge steps above, this re-POSTs each entry on every
// `up`: the operator subscriptions GET collapses to distinct channels and drops
// matcher/params, so it cannot tell a filtered/parameterized sub from a plain
// one — there is nothing lossless to diff against. Re-POSTing is cheap and, by
// the idempotency above, harmless. Subscriptions are additive only: an entry
// removed from the file is not torn down here (mirrors the file's other
// converge steps, which never delete).
func (r *Runner) convergeSubscriptions(name string, a AgentSpec) error {
	for _, sub := range a.Subscribe {
		body := map[string]any{"channel": sub.Channel}
		if sub.Type != "" {
			body["type"] = sub.Type
		}
		if len(sub.Matcher) > 0 {
			body["matcher"] = sub.Matcher
		}
		if len(sub.Params) > 0 {
			body["params"] = sub.Params
		}
		r.logf("subscribing %s -> %s", name, sub.Channel)
		if _, err := r.call.Call("POST", "/api/agents/"+name+"/subscriptions", body); err != nil {
			return fmt.Errorf("subscribe agent %s to %s: %w", name, sub.Channel, err)
		}
	}
	return nil
}

// convergeTimeout sets the agent's soft timeout (timeout_s) to the file's value
// via the loop-setting endpoint the create call can't carry. cur is the live
// agent row (nil on create); when present and already matching, it's a no-op so
// an already-converged file makes no call. An empty effective timeout (neither
// loop.timeout nor flat timeout set) leaves the daemon's value untouched.
func (r *Runner) convergeTimeout(name string, a AgentSpec, cur map[string]any) error {
	if a.effectiveTimeout() == "" {
		return nil
	}
	secs, err := a.effectiveTimeoutSeconds() // validated in Up; defensive re-check
	if err != nil {
		return fmt.Errorf("agent %s %w", name, err)
	}
	if cur != nil && fmt.Sprintf("%v", cur["timeout_s"]) == fmt.Sprintf("%d", secs) {
		return nil // already converged
	}
	r.logf("setting timeout for agent %s -> %ds", name, secs)
	if _, err := r.call.Call("POST", "/api/agents/"+name+"/loop/timeout",
		map[string]any{"value": secs}); err != nil {
		return fmt.Errorf("set timeout %s: %w", name, err)
	}
	return nil
}

// convergeLoop converges the agent's loop cadence/policy toward the file. Each
// field is diff-first against the live list-view row (cur, nil on create), so a
// converged file makes no loop calls. Enabled is only converged on drift when
// the file set it explicitly (*bool non-nil); the create path handles the
// initial value via the create body. The soft timeout is handled separately by
// convergeTimeout (loop.timeout precedence lives there).
func (r *Runner) convergeLoop(name string, a AgentSpec, cur map[string]any) error {
	if a.Loop == nil {
		return nil
	}
	if secs, set, err := a.intervalSeconds(); err != nil {
		return fmt.Errorf("agent %s loop %w", name, err)
	} else if set {
		if cur == nil || fmt.Sprintf("%v", cur["interval_s"]) != fmt.Sprintf("%d", secs) {
			r.logf("setting interval for agent %s -> %ds", name, secs)
			if _, err := r.call.Call("POST", "/api/agents/"+name+"/loop/interval",
				map[string]any{"value": secs}); err != nil {
				return fmt.Errorf("set interval %s: %w", name, err)
			}
		}
	}
	if err := r.convergeLoopStr(name, "on-timeout", "on_timeout", a.Loop.OnTimeout, cur); err != nil {
		return err
	}
	if err := r.convergeLoopStr(name, "on-error", "on_error", a.Loop.OnError, cur); err != nil {
		return err
	}
	if a.Loop.MaxIdleIterations != nil {
		want := *a.Loop.MaxIdleIterations
		if cur == nil || fmt.Sprintf("%v", cur["max_idle_iterations"]) != fmt.Sprintf("%d", want) {
			r.logf("setting max-idle for agent %s -> %d", name, want)
			if _, err := r.call.Call("POST", "/api/agents/"+name+"/loop/max-idle",
				map[string]any{"value": want}); err != nil {
				return fmt.Errorf("set max-idle %s: %w", name, err)
			}
		}
	}
	if a.Loop.Enabled != nil && cur != nil {
		want := *a.Loop.Enabled
		if fmt.Sprintf("%v", cur["loop_enabled"]) != fmt.Sprintf("%v", want) {
			verb := "disable"
			if want {
				verb = "enable"
			}
			r.logf("turning loop %s for agent %s", verb, name)
			if _, err := r.call.Call("POST", "/api/agents/"+name+"/loop/"+verb, map[string]any{}); err != nil {
				return fmt.Errorf("set loop enabled %s: %w", name, err)
			}
		}
	}
	return nil
}

// convergeLoopStr converges one string loop policy (on-timeout/on-error).
// route is the URL suffix, field is the live list-view key. Empty want is a
// no-op; a matching live value is a no-op (diff-first).
func (r *Runner) convergeLoopStr(name, route, field, want string, cur map[string]any) error {
	if want == "" {
		return nil
	}
	if cur != nil && fmt.Sprintf("%v", cur[field]) == want {
		return nil
	}
	r.logf("setting %s for agent %s -> %s", field, name, want)
	if _, err := r.call.Call("POST", "/api/agents/"+name+"/loop/"+route,
		map[string]any{"value": want}); err != nil {
		return fmt.Errorf("set %s %s: %w", field, name, err)
	}
	return nil
}

// Down stops+removes the file's agents and removes its groups (spec §5.3). The
// shared dir is kept unless volumes is set. Scoped strictly to the names in the
// file; unrelated agents/groups are untouched.
func (r *Runner) Down(f File, volumes bool) error {
	for _, name := range sortedKeys(f.Agents) {
		r.logf("removing agent %s (purge=%v)", name, volumes)
		q := map[string]string{"force": "true"}
		// Mirror the group --volumes semantics onto agents: a plain down preserves
		// each agent's data (row + CONTEXT.md + iterations + audit), while
		// down --volumes purges it. Without volumes, down keeps everything so a
		// later up re-provisions in place over the retained history.
		if volumes {
			q["purge"] = "true"
		}
		if _, err := r.call.Call("DELETE", "/api/agents/"+name, q); err != nil {
			return fmt.Errorf("remove agent %s: %w", name, err)
		}
	}
	for _, name := range sortedKeys(f.Groups) {
		r.logf("removing group %s (volumes=%v)", name, volumes)
		q := map[string]string{}
		if volumes {
			q["volumes"] = "true"
		}
		if _, err := r.call.Call("DELETE", "/api/groups/"+name, q); err != nil {
			return fmt.Errorf("remove group %s: %w", name, err)
		}
	}
	return nil
}

// Build publishes every declared image through the daemon. No-op when the file
// declares no images or the test seam is set.
func (r *Runner) Build(f File) error {
	if r.skipBuild {
		return nil
	}
	for _, name := range sortedKeys(f.Images) {
		spec := f.Images[name]
		ctx := spec.Context
		if !filepath.IsAbs(ctx) {
			ctx = filepath.Join(r.workdir, ctx)
		}
		r.logf("building image %s:latest", name)
		if _, err := r.call.Call("POST", "/api/images/build", map[string]any{"name": name, "tag": "latest", "path": ctx}); err != nil {
			return fmt.Errorf("build image %s: %w", name, err)
		}
	}
	return nil
}

func (r *Runner) applyBudgets(f File) error {
	// Diff-first: read live budgets once so an already-converged file makes no
	// budget.set calls.
	liveB, err := r.liveBudgets()
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(f.Groups) {
		g := f.Groups[name]
		if g.Budget == nil {
			continue
		}
		if err := r.setBudget("group:"+name, *g.Budget, liveB); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(f.Agents) {
		a := f.Agents[name]
		if a.Budget == nil {
			continue
		}
		if err := r.setBudget("agent:"+name, *a.Budget, liveB); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) setBudget(scope string, b BudgetSpec, live map[string]map[string]any) error {
	mode, err := b.NormalizedMode()
	if err != nil {
		return err
	}
	// Skip the set when a budget with this scope already matches desired
	// (limit, normalized mode, and period all equal).
	if cur, ok := live[scope]; ok && budgetMatches(cur, b, mode) {
		return nil
	}
	body := map[string]any{"scope": scope, "limit-usd": fmt.Sprintf("%g", b.LimitUSD), "mode": mode}
	if b.Period != "" {
		body["period"] = b.Period
	}
	r.logf("setting budget %s", scope)
	if _, err := r.call.Call("POST", "/api/budgets", body); err != nil {
		return fmt.Errorf("set budget %s: %w", scope, err)
	}
	return nil
}

// budgetMatches reports whether a live budget row (budget.ls shape: limit_usd,
// period_s, mode) already equals the desired spec with normalized mode.
func budgetMatches(cur map[string]any, b BudgetSpec, mode string) bool {
	if fmt.Sprintf("%v", cur["mode"]) != mode {
		return false
	}
	if toFloat(cur["limit_usd"]) != b.LimitUSD {
		return false
	}
	return toInt(cur["period_s"]) == desiredPeriodSeconds(b.Period)
}

// desiredPeriodSeconds mirrors the daemon's budget.set default: an empty period
// means 24h (86400s); anything else is a Go duration.
func desiredPeriodSeconds(period string) int {
	if period == "" {
		return 86400
	}
	d, err := time.ParseDuration(period)
	if err != nil {
		return -1 // never matches a live row; forces a converge POST
	}
	return int(d.Seconds())
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// liveAgents returns name -> view for every agent the daemon knows.
func (r *Runner) liveAgents() (map[string]map[string]any, error) {
	raw, err := r.call.Call("GET", "/api/agents", map[string]string{})
	if err != nil {
		return nil, err
	}
	var env struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, a := range env.Agents {
		if n, ok := a["name"].(string); ok {
			out[n] = a
		}
	}
	return out, nil
}

// liveGroups returns name -> view for every group the daemon knows.
func (r *Runner) liveGroups() (map[string]map[string]any, error) {
	raw, err := r.call.Call("GET", "/api/groups", map[string]string{})
	if err != nil {
		return nil, err
	}
	var env struct {
		Groups []map[string]any `json:"groups"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, g := range env.Groups {
		if n, ok := g["name"].(string); ok {
			out[n] = g
		}
	}
	return out, nil
}

// liveBudgets returns scope -> view for every budget the daemon knows
// (budget.ls shape: scope/limit_usd/period_s/mode).
func (r *Runner) liveBudgets() (map[string]map[string]any, error) {
	raw, err := r.call.Call("GET", "/api/budgets", map[string]string{})
	if err != nil {
		return nil, err
	}
	var env struct {
		Budgets []map[string]any `json:"budgets"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, b := range env.Budgets {
		if s, ok := b["scope"].(string); ok {
			out[s] = b
		}
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func kvList(m map[string]string) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, k+"="+m[k])
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

// memberNames returns the file's agent names, or — when args is non-empty — the
// named subset. A name that is neither a file agent nor a file group is an
// error. A group name expands to that group's file members.
func (r *Runner) memberNames(f File, args []string) ([]string, error) {
	if len(args) == 0 {
		return sortedKeys(f.Agents), nil
	}
	var out []string
	for _, name := range args {
		switch {
		case fileHasAgent(f, name):
			out = append(out, name)
		case fileHasGroup(f, name):
			for _, an := range sortedKeys(f.Agents) {
				if f.Agents[an].Group == name {
					out = append(out, an)
				}
			}
		default:
			return nil, fmt.Errorf("%q is not an agent or group in this compose file", name)
		}
	}
	return out, nil
}

func fileHasAgent(f File, name string) bool { _, ok := f.Agents[name]; return ok }
func fileHasGroup(f File, name string) bool { _, ok := f.Groups[name]; return ok }

// Lifecycle applies an agent lifecycle verb to the selected members.
func (r *Runner) Lifecycle(f File, verb string, args []string) error {
	names, err := r.memberNames(f, args)
	if err != nil {
		return err
	}
	for _, name := range names {
		r.logf("%s %s", verb, name)
		if _, err := r.call.Call("POST", "/api/agents/"+name+"/"+verb, map[string]any{}); err != nil {
			return fmt.Errorf("%s %s: %w", verb, name, err)
		}
	}
	return nil
}

// Ps prints the file's members and their live state (scoped to the file).
func (r *Runner) Ps(f File) error {
	live, err := r.liveAgents()
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(f.Agents) {
		state := "absent"
		group := f.Agents[name].Group
		if a, ok := live[name]; ok {
			state = fmt.Sprintf("%v", a["state"])
		}
		r.logf("%-20s %-12s group=%s", name, state, group)
	}
	return nil
}

// Status reports desired-vs-actual drift (spec §5, M8 status semantics):
//   - agents: missing agents, group-membership drift, and non-group field
//     drift. Group membership converges on `up`, so live group mismatch here
//     means the file changed since the last `up`. Non-group fields ("image"
//     and "harness" are exposed by agent.ps's live view; model/env/plugins
//     are not part of the list view and would need a per-agent inspect call)
//     have no agent.update route, so they are only ever reported, never
//     mutated.
//   - groups: missing groups and lead drift (lead converges on `up`).
//   - budgets: missing/diverged budgets for every group and agent scope that
//     declares one (budgets converge on `up`).
//
// Coverage is partial (model/env/plugins drift cannot be detected — see
// above), so the printed output always ends with a one-line disclaimer; a
// bare "ok"/"drift: N" must never be read as a full convergence guarantee.
//
// This is read-only: it never issues a mutating call.
func (r *Runner) Status(f File) error {
	live, err := r.liveAgents()
	if err != nil {
		return err
	}
	liveG, err := r.liveGroups()
	if err != nil {
		return err
	}
	liveB, err := r.liveBudgets()
	if err != nil {
		return err
	}
	drift := 0
	for _, name := range sortedKeys(f.Agents) {
		r.statusAgent(name, f.Agents[name], live, &drift)
	}
	for _, name := range sortedKeys(f.Groups) {
		r.statusGroup(name, f.Groups[name], liveG, &drift)
	}
	for _, name := range sortedKeys(f.Groups) {
		if b := f.Groups[name].Budget; b != nil {
			r.statusBudget("group:"+name, *b, liveB, &drift)
		}
	}
	for _, name := range sortedKeys(f.Agents) {
		if b := f.Agents[name].Budget; b != nil {
			r.statusBudget("agent:"+name, *b, liveB, &drift)
		}
	}
	liveQueues := map[string]tasks.Queue{}
	if len(f.TaskQueues) != 0 {
		liveQueues, err = r.listTaskQueues()
		if err != nil {
			return fmt.Errorf("status task queues: %w", err)
		}
	}
	for _, prefix := range sortedKeys(f.TaskQueues) {
		want := f.TaskQueues[prefix]
		liveQueue, queueExists := liveQueues[prefix]
		if !queueExists {
			r.logf("task queue %-10s MISSING (want name=%s)", prefix, want.Name)
			drift++
			continue
		}
		if liveQueue.Name != want.Name {
			r.logf("task queue %-10s name drift: have=%s want=%s", prefix, liveQueue.Name, want.Name)
			drift++
		}
		pools, err := r.listTaskPools(prefix)
		if err != nil {
			return fmt.Errorf("status task queue %s pools: %w", prefix, err)
		}
		for _, poolName := range sortedKeys(want.Pools) {
			desired := normalizeAgentNames(want.Pools[poolName])
			pool, exists := pools[poolName]
			if !exists {
				r.logf("task queue %-10s pool %s MISSING (want=%v)", prefix, poolName, desired)
				drift++
			} else if !reflect.DeepEqual(pool.Agents, desired) {
				r.logf("task queue %-10s pool %s drift: have=%v want=%v", prefix, poolName, pool.Agents, desired)
				drift++
			} else {
				r.logf("task queue %-10s pool %s ok", prefix, poolName)
			}
		}
	}
	r.logf("drift: %d", drift)
	r.logf("note: model/env/plugins drift not checked (not exposed by agent.ps)")
	return nil
}

func (r *Runner) statusAgent(name string, want AgentSpec, live map[string]map[string]any, drift *int) {
	a, ok := live[name]
	if !ok {
		r.logf("agent %-14s MISSING (desired image=%s group=%s)", name, want.Image, want.Group)
		*drift++
		return
	}
	var issues []string
	if haveGroup := fmt.Sprintf("%v", a["group"]); want.Group != "" && haveGroup != want.Group {
		issues = append(issues, fmt.Sprintf("group drift: have=%s want=%s", haveGroup, want.Group))
	}
	// image is a non-group field agent.ps's live view exposes; report (never
	// mutate) since there is no agent.update in M8.
	if img, present := a["image"]; present && want.Image != "" {
		if haveImage := fmt.Sprintf("%v", img); haveImage != want.Image {
			issues = append(issues, fmt.Sprintf("image drift: have=%s want=%s", haveImage, want.Image))
		}
	}
	// harness is likewise exposed by agent.ps's live view; report (never
	// mutate) since there is no agent.update in M8.
	if h, present := a["harness"]; present && want.Harness != nil && want.Harness.Type != "" {
		if haveHarness := fmt.Sprintf("%v", h); haveHarness != want.Harness.Type {
			issues = append(issues, fmt.Sprintf("harness drift: have=%s want=%s", haveHarness, want.Harness.Type))
		}
	}
	// cwd is exposed by agent.ps's live view and DOES converge on `up`; report
	// drift here against the effective cwd (file value, or the compose dir by
	// default).
	if wantCwd := effectiveCwd(want.Cwd, r.workdir); wantCwd != "" {
		if haveCwd := fmt.Sprintf("%v", a["cwd"]); haveCwd != wantCwd {
			issues = append(issues, fmt.Sprintf("cwd drift: have=%s want=%s", haveCwd, wantCwd))
		}
	}
	// loop cadence/policy are exposed by agent.ps's live view and converge on
	// `up`; report drift here for each field the file sets.
	if secs, set, err := want.intervalSeconds(); err == nil && set {
		if have := fmt.Sprintf("%v", a["interval_s"]); have != fmt.Sprintf("%d", secs) {
			issues = append(issues, fmt.Sprintf("interval drift: have=%ss want=%ds", have, secs))
		}
	}
	if want.Loop != nil && want.Loop.OnTimeout != "" {
		if have := fmt.Sprintf("%v", a["on_timeout"]); have != want.Loop.OnTimeout {
			issues = append(issues, fmt.Sprintf("on_timeout drift: have=%s want=%s", have, want.Loop.OnTimeout))
		}
	}
	if want.Loop != nil && want.Loop.OnError != "" {
		if have := fmt.Sprintf("%v", a["on_error"]); have != want.Loop.OnError {
			issues = append(issues, fmt.Sprintf("on_error drift: have=%s want=%s", have, want.Loop.OnError))
		}
	}
	if want.Loop != nil && want.Loop.MaxIdleIterations != nil {
		if have := fmt.Sprintf("%v", a["max_idle_iterations"]); have != fmt.Sprintf("%d", *want.Loop.MaxIdleIterations) {
			issues = append(issues, fmt.Sprintf("max_idle_iterations drift: have=%s want=%d", have, *want.Loop.MaxIdleIterations))
		}
	}
	if len(issues) > 0 {
		r.logf("agent %-14s DRIFT %s (state=%v)", name, strings.Join(issues, "; "), a["state"])
		*drift++
		return
	}
	r.logf("agent %-14s ok (state=%v group=%s)", name, a["state"], a["group"])
}

func (r *Runner) statusGroup(name string, want GroupSpec, liveG map[string]map[string]any, drift *int) {
	g, ok := liveG[name]
	if !ok {
		r.logf("group %-14s MISSING (desired lead=%s)", name, want.Lead)
		*drift++
		return
	}
	haveLead := fmt.Sprintf("%v", g["lead"])
	if want.Lead != "" && haveLead != want.Lead {
		r.logf("group %-14s lead drift: have=%s want=%s", name, haveLead, want.Lead)
		*drift++
		return
	}
	r.logf("group %-14s ok (lead=%s)", name, haveLead)
}

func (r *Runner) statusBudget(scope string, b BudgetSpec, liveB map[string]map[string]any, drift *int) {
	mode, err := b.NormalizedMode()
	if err != nil {
		r.logf("budget %-14s invalid mode: %v", scope, err)
		*drift++
		return
	}
	cur, ok := liveB[scope]
	if !ok {
		r.logf("budget %-14s MISSING (desired limit=%g mode=%s)", scope, b.LimitUSD, mode)
		*drift++
		return
	}
	if !budgetMatches(cur, b, mode) {
		r.logf("budget %-14s drift: have limit=%v mode=%v", scope, cur["limit_usd"], cur["mode"])
		*drift++
		return
	}
	r.logf("budget %-14s ok", scope)
}

// Exec runs a manual iteration in a named member, with an optional one-shot
// prompt (the remaining args joined by spaces). The target must be one of the
// file's own agents — exec never reaches an agent outside the compose file.
func (r *Runner) Exec(f File, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("exec needs a member name")
	}
	name := args[0]
	if !fileHasAgent(f, name) {
		return fmt.Errorf("%q is not an agent in this compose file", name)
	}
	body := map[string]any{}
	if len(args) > 1 {
		prompt := args[1]
		for _, w := range args[2:] {
			prompt += " " + w
		}
		body["prompt"] = prompt
	}
	r.logf("exec %s", name)
	_, err := r.call.Call("POST", "/api/agents/"+name+"/exec", body)
	return err
}

// Logs prints each selected member's recent events (scoped to the file, or the
// named member/group subset), one line per event. This maps onto the same
// GET /api/agents/{name}/logs route the top-level `logs` command uses (see
// internal/commands/logs.go): its handler returns {"events": [...],"count":N}
// with each event shaped {kind,data,at} — NOT a "lines" envelope. `tail` is
// sent as a query hint for forward-compat; the current handler ignores it and
// always returns its own fixed-size window, so this cannot be used to page
// through history. Follow (-f) and --source filtering are CLI-local features
// of the top-level `logs` command and are out of scope for `compose logs`,
// which only ever does one read pass.
func (r *Runner) Logs(f File, args []string, tail int) error {
	names, err := r.memberNames(f, args)
	if err != nil {
		return err
	}
	for _, name := range names {
		raw, err := r.call.Call("GET", "/api/agents/"+name+"/logs",
			map[string]string{"tail": fmt.Sprintf("%d", tail)})
		if err != nil {
			return fmt.Errorf("logs %s: %w", name, err)
		}
		var env struct {
			Events []struct {
				Kind string `json:"kind"`
				Data string `json:"data"`
				At   string `json:"at"`
			} `json:"events"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return err
		}
		for _, ev := range env.Events {
			r.logf("%s | %s [%s] %s", name, ev.At, ev.Kind, ev.Data)
		}
	}
	return nil
}

// Rm removes stopped members (spec §5.3). Running members are skipped (use
// down, or stop first) — this mirrors docker-compose rm, and is strictly
// scoped to the file's own agents (never touches an unrelated daemon agent).
func (r *Runner) Rm(f File, args []string) error {
	live, err := r.liveAgents()
	if err != nil {
		return err
	}
	names, err := r.memberNames(f, args)
	if err != nil {
		return err
	}
	for _, name := range names {
		a, ok := live[name]
		if !ok {
			continue
		}
		if fmt.Sprintf("%v", a["state"]) == "running" {
			r.logf("skip %s (running; stop it first)", name)
			continue
		}
		r.logf("removing %s", name)
		if _, err := r.call.Call("DELETE", "/api/agents/"+name, map[string]string{}); err != nil {
			return fmt.Errorf("rm %s: %w", name, err)
		}
	}
	return nil
}
