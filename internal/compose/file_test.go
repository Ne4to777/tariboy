package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const goodYAML = `
version: 1
images:
  analyst: { context: ./analyst }
groups:
  research-team:
    lead: scout
    budget: { limit_usd: 50, period: 24h, mode: enforce }
agents:
  scout:
    image: analyst:latest
    group: research-team
    harness:
      type: claude
      model: claude-opus-4-8
      effort: high
    env: { REGION: eu }
    plugins: [chat]
    budget: { limit_usd: 10 }
  writer:
    image: analyst:latest
    group: research-team
`

func TestLoadTaskQueuesWithPools(t *testing.T) {
	dir := t.TempDir()
	compose := `version: 1
task_queues:
  DEV:
    name: Development
    pools:
      developers: [dev-ng-developer]
      reviewers: [dev-ng-reviewer]
agents:
  dev-ng-developer: {image: basic:latest}
  dev-ng-reviewer: {image: basic:latest}
`
	path := filepath.Join(dir, "tariboy-compose.yaml")
	if err := os.WriteFile(path, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	f, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := f.TaskQueues["DEV"].Pools["developers"]; len(got) != 1 || got[0] != "dev-ng-developer" {
		t.Fatalf("developers pool = %#v", got)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestParseRejectsRemovedWorkflowKeysByName(t *testing.T) {
	for _, tc := range []struct{ name, yaml, key string }{
		{"top-level workflows", "version: 1\nworkflows:\n  development: {source: ./workflow.yaml}\n", "workflows"},
		{"queue workflow", "version: 1\ntask_queues:\n  DEV: {name: Development, workflow: development}\n", "task_queues.DEV.workflow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), "no longer supported") {
				t.Fatalf("removed key error = %v; want it to name %s", err, tc.key)
			}
		})
	}
}

// Team exports written before the removal always carried an empty workflows
// map; it declares nothing, so those archives stay importable.
func TestParseAcceptsEmptyRemovedWorkflowKeys(t *testing.T) {
	for _, doc := range []string{
		"version: 1\nworkflows: {}\ntask_queues: {}\n",
		"version: 1\nworkflows:\n",
		"version: 1\ntask_queues:\n  DEV: {name: Development, workflow: \"\"}\n",
	} {
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("Parse(%q) = %v; want accepted", doc, err)
		}
	}
	out, err := yaml.Marshal(File{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "workflow") {
		t.Fatalf("marshaled compose file mentions workflows:\n%s", out)
	}
}

func TestTaskQueueValidationRequiresNameAndDeclaredPoolAgents(t *testing.T) {
	f := File{Version: 1, TaskQueues: map[string]TaskQueueSpec{"DEV": {}}}
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("missing name error = %v", err)
	}
	f.TaskQueues["DEV"] = TaskQueueSpec{Name: "Development", Pools: map[string][]string{"developers": {"ghost"}}}
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("undeclared agent error = %v", err)
	}
}

func TestLegacyComposeWithoutTaskSectionsRemainsValid(t *testing.T) {
	f, err := Parse([]byte("version: 1\nagents:\n  dev: {image: basic:latest}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("legacy validate: %v", err)
	}
}

func TestParseRejectsUnknownComposeField(t *testing.T) {
	_, err := Parse([]byte("version: 1\nworkflwos: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "workflwos") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestParseRejectsUnknownSubscribeObjectField(t *testing.T) {
	_, err := Parse([]byte(`version: 1
agents:
  dev:
    image: basic:latest
    subscribe:
      - channel: logs:api
        macher: {service: api}
`))
	if err == nil || !strings.Contains(err.Error(), "macher") {
		t.Fatalf("unknown subscribe field error = %v", err)
	}
}

func TestValidateRejectsUnsafePoolRouteSegments(t *testing.T) {
	for _, bad := range []string{"bad/name", "bad?name", "bad#name", "bad:name", "разработка"} {
		f := File{Version: 1, Agents: map[string]AgentSpec{"dev": {Image: "basic:latest"}}, TaskQueues: map[string]TaskQueueSpec{"DEV": {Name: "Development", Pools: map[string][]string{bad: {"dev"}, "developers": {"dev"}}}}}
		if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "pool") {
			t.Errorf("pool name %q error = %v", bad, err)
		}
	}
}

func TestParseAndValidateGood(t *testing.T) {
	f, err := Parse([]byte(goodYAML))
	if err != nil {
		t.Fatal(err)
	}
	if f.Version != 1 || f.Groups["research-team"].Lead != "scout" {
		t.Fatalf("file = %+v", f)
	}
	if f.Agents["scout"].Env["REGION"] != "eu" || f.Agents["scout"].Plugins[0] != "chat" {
		t.Fatalf("scout = %+v", f.Agents["scout"])
	}
	if h := f.Agents["scout"].Harness; h == nil || h.Type != "claude" || h.Model != "claude-opus-4-8" || h.Effort != "high" {
		t.Fatalf("scout harness = %+v", f.Agents["scout"].Harness)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	mode, err := f.Groups["research-team"].Budget.NormalizedMode()
	if err != nil || mode != "block" {
		t.Fatalf("enforce should normalize to block: %q err=%v", mode, err)
	}
}

func TestValidateRejectsHarnessWithoutType(t *testing.T) {
	src := "version: 1\nagents:\n  a:\n    image: img:latest\n    harness:\n      model: x\n"
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(); err == nil {
		t.Fatal("expected error for harness without type")
	}
}

func TestValidateRejectsUnknownHarnessType(t *testing.T) {
	src := "version: 1\nagents:\n  a:\n    image: img:latest\n    harness:\n      type: bard\n"
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(); err == nil {
		t.Fatal("expected error for unknown harness type")
	}
}

func TestParseCwd(t *testing.T) {
	const y = `
version: 1
agents:
  a:
    image: img:latest
    cwd: $CWD/sub
`
	f, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if f.Agents["a"].Cwd != "$CWD/sub" {
		t.Fatalf("cwd = %q", f.Agents["a"].Cwd)
	}
}

func TestParseTimeout(t *testing.T) {
	const y = `
version: 1
agents:
  a:
    image: img:latest
    timeout: 90m
`
	f, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if f.Agents["a"].Timeout != "90m" {
		t.Fatalf("timeout = %q", f.Agents["a"].Timeout)
	}
	secs, err := f.Agents["a"].TimeoutSeconds()
	if err != nil || secs != 5400 {
		t.Fatalf("TimeoutSeconds = %d err=%v, want 5400", secs, err)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid timeout rejected: %v", err)
	}
	// Empty timeout is unset (0), and validates.
	var empty AgentSpec
	if secs, err := empty.TimeoutSeconds(); err != nil || secs != 0 {
		t.Fatalf("empty TimeoutSeconds = %d err=%v, want 0", secs, err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*File){
		"bad version":      func(f *File) { f.Version = 2 },
		"bad timeout unit": func(f *File) { a := f.Agents["writer"]; a.Timeout = "60"; f.Agents["writer"] = a },
		"negative timeout": func(f *File) { a := f.Agents["writer"]; a.Timeout = "-5m"; f.Agents["writer"] = a },
		"bad group name":   func(f *File) { f.Groups["Bad Name!"] = f.Groups["research-team"]; delete(f.Groups, "research-team") },
		"lead not member":  func(f *File) { g := f.Groups["research-team"]; g.Lead = "ghost"; f.Groups["research-team"] = g },
		"unknown group":    func(f *File) { a := f.Agents["writer"]; a.Group = "nope"; f.Agents["writer"] = a },
		"bad agent name":   func(f *File) { f.Agents["../evil"] = f.Agents["writer"]; delete(f.Agents, "writer") },
		"agent no image":   func(f *File) { a := f.Agents["writer"]; a.Image = ""; f.Agents["writer"] = a },
		"bad budget mode": func(f *File) {
			g := f.Groups["research-team"]
			g.Budget = &BudgetSpec{LimitUSD: 1, Mode: "wat"}
			f.Groups["research-team"] = g
		},
	}
	for name, mutate := range cases {
		f, _ := Parse([]byte(goodYAML))
		mutate(&f)
		if err := f.Validate(); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func TestLoopBlockParses(t *testing.T) {
	f, err := Parse([]byte(`
version: 1
agents:
  m:
    image: img:latest
    loop:
      enabled: false
      interval: 5m
      timeout: 60m
      on_timeout: restart
      on_error: stop
`))
	if err != nil {
		t.Fatal(err)
	}
	a := f.Agents["m"]
	if a.Loop == nil {
		t.Fatal("loop block not parsed")
	}
	if a.Loop.Enabled == nil || *a.Loop.Enabled != false {
		t.Errorf("enabled = %v, want explicit false", a.Loop.Enabled)
	}
	if a.Loop.Interval != "5m" || a.Loop.OnTimeout != "restart" || a.Loop.OnError != "stop" {
		t.Errorf("bad loop fields: %+v", a.Loop)
	}
	if s, _, _ := a.intervalSeconds(); s != 300 {
		t.Errorf("intervalSeconds = %d, want 300", s)
	}
}

func TestGoalBlockParsesPositiveWholeSecondDuration(t *testing.T) {
	f, err := Parse([]byte(`
version: 1
agents:
  worker:
    image: basic:latest
    goal:
      enabled: false
      wait_customer_timeout: 5m
      delivery_cooldown: 1m
`))
	if err != nil {
		t.Fatal(err)
	}
	goal := f.Agents["worker"].Goal
	if goal == nil || goal.Enabled == nil || *goal.Enabled || goal.WaitCustomerTimeout != "5m" || goal.DeliveryCooldown != "1m" {
		t.Fatalf("Goal = %#v", goal)
	}
	seconds, set, err := f.Agents["worker"].goalWaitCustomerTimeoutSeconds()
	if err != nil || !set || seconds != 300 {
		t.Fatalf("goal timeout = %d set=%t err=%v", seconds, set, err)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid Goal rejected: %v", err)
	}
	cooldown, set, err := f.Agents["worker"].goalDeliveryCooldownSeconds()
	if err != nil || !set || cooldown != 60 {
		t.Fatalf("goal cooldown = %d set=%t err=%v", cooldown, set, err)
	}
}

func TestGoalDurationRejectsZeroFractionalAndInvalidValues(t *testing.T) {
	for _, value := range []string{"0s", "1500ms", "5", "-1s"} {
		t.Run(value, func(t *testing.T) {
			f, err := Parse([]byte("version: 1\nagents:\n  worker:\n    image: basic:latest\n    goal:\n      wait_customer_timeout: " + value + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Validate(); err == nil {
				t.Fatalf("Goal duration %q was accepted", value)
			}
		})
	}
	for _, value := range []string{"0s", "1500ms", "5", "-1s"} {
		t.Run("cooldown-"+value, func(t *testing.T) {
			f, err := Parse([]byte("version: 1\nagents:\n  worker:\n    image: basic:latest\n    goal:\n      delivery_cooldown: " + value + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Validate(); err == nil {
				t.Fatalf("Goal cooldown %q was accepted", value)
			}
		})
	}
}

func TestEffectiveTimeoutPrecedence(t *testing.T) {
	loopWins := AgentSpec{Timeout: "10m", Loop: &LoopSpec{Timeout: "60m"}}
	if got := loopWins.effectiveTimeout(); got != "60m" {
		t.Errorf("loop.timeout should win, got %q", got)
	}
	flatFallback := AgentSpec{Timeout: "10m"}
	if got := flatFallback.effectiveTimeout(); got != "10m" {
		t.Errorf("flat timeout fallback, got %q", got)
	}
	blockNoTimeout := AgentSpec{Timeout: "10m", Loop: &LoopSpec{Interval: "5m"}}
	if got := blockNoTimeout.effectiveTimeout(); got != "10m" {
		t.Errorf("block without timeout falls back to flat, got %q", got)
	}
}

func TestLoopValidationRejectsBadValues(t *testing.T) {
	bad := []string{
		"version: 1\nagents:\n  m:\n    image: i:latest\n    loop:\n      interval: 5\n",             // no unit
		"version: 1\nagents:\n  m:\n    image: i:latest\n    loop:\n      on_timeout: reboot\n",      // bad policy
		"version: 1\nagents:\n  m:\n    image: i:latest\n    loop:\n      timeout: -1m\n",            // negative
		"version: 1\nagents:\n  m:\n    image: i:latest\n    loop:\n      max_idle_iterations: -1\n", // negative idle
	}
	for _, src := range bad {
		f, _ := Parse([]byte(src))
		if err := f.Validate(); err == nil {
			t.Errorf("expected validation error for:\n%s", src)
		}
	}
}
