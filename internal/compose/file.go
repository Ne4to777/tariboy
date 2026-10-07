// Package compose is the client-side declarative reconciler for
// tariboy-compose.yaml (spec §5): parse+validate, diff desired-vs-actual,
// and converge by driving the existing daemon command surface. It adds no new
// daemon capability.
package compose

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/stores"
	"github.com/alekzonder/tariboy/internal/workflowfile"
)

var taskQueuePrefixRE = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
var poolRouteSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// File is a parsed tariboy-compose.yaml (spec §5.1).
type File struct {
	Version    int                      `yaml:"version"`
	Images     map[string]ImageSpec     `yaml:"images"`
	Groups     map[string]GroupSpec     `yaml:"groups"`
	Agents     map[string]AgentSpec     `yaml:"agents"`
	TaskQueues map[string]TaskQueueSpec `yaml:"task_queues"`
	// RemovedWorkflows captures the top-level workflows: map of the removed
	// task workflow engine so Parse can reject it by name. An empty map is
	// accepted: team exports before the removal always wrote "workflows: {}".
	RemovedWorkflows yaml.Node `yaml:"workflows,omitempty"`
}

type TaskQueueSpec struct {
	Name  string              `yaml:"name"`
	Pools map[string][]string `yaml:"pools"`
	// Workflow is "[STORE/]NAME[:TAG]", the workflow image the queue follows
	// (see ParseWorkflowRef). Empty leaves an existing binding untouched.
	Workflow string `yaml:"workflow,omitempty"`
}

// WorkflowRef is a parsed task_queues.<PREFIX>.workflow value. A Store prefix
// means "build STORE/NAME first"; an empty Tag means the build's own version
// with a Store prefix and "latest" without one.
type WorkflowRef struct {
	Store string
	Name  string
	Tag   string
}

// ParseWorkflowRef parses "[STORE/]NAME[:TAG]". Names follow the Store
// selector and workflow name rules and the tag follows the image tag rule.
func ParseWorkflowRef(s string) (WorkflowRef, error) {
	var ref WorkflowRef
	if s == "" || s != strings.TrimSpace(s) {
		return ref, fmt.Errorf("want [STORE/]NAME[:TAG] without surrounding whitespace")
	}
	rest := s
	if strings.Count(rest, "/") > 1 || strings.Count(rest, ":") > 1 {
		return ref, fmt.Errorf("want [STORE/]NAME[:TAG] with at most one / and one :")
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		ref.Store, rest = rest[:i], rest[i+1:]
		if !stores.ValidName(ref.Store) {
			return WorkflowRef{}, fmt.Errorf("invalid Store name %q", ref.Store)
		}
	}
	ref.Name = rest
	if i := strings.Index(rest, ":"); i >= 0 {
		ref.Name, ref.Tag = rest[:i], rest[i+1:]
		// A tag names a directory of the image store, so "." and ".." are refused.
		if parsed, err := image.ParseRef("x:" + ref.Tag); err != nil || parsed.Tag != ref.Tag || ref.Tag == "." || ref.Tag == ".." {
			return WorkflowRef{}, fmt.Errorf("invalid tag %q", ref.Tag)
		}
	}
	if !workflowfile.ValidName(ref.Name) {
		return WorkflowRef{}, fmt.Errorf("invalid workflow name %q", ref.Name)
	}
	return ref, nil
}

// WorkflowRef returns the parsed workflow of the queue; declared is false
// when the file names none (an empty value and an absent key are the same).
func (q TaskQueueSpec) WorkflowRef() (ref WorkflowRef, declared bool, err error) {
	if q.Workflow == "" {
		return WorkflowRef{}, false, nil
	}
	ref, err = ParseWorkflowRef(q.Workflow)
	return ref, err == nil, err
}

// Selector is the Store selector "STORE/NAME" the build step sends.
func (r WorkflowRef) Selector() string { return r.Store + "/" + r.Name }

// Ref is "NAME:TAG", TAG defaulting to latest, the form the daemon resolves.
func (r WorkflowRef) Ref() string {
	tag := r.Tag
	if tag == "" {
		tag = "latest"
	}
	return r.Name + ":" + tag
}

type ImageSpec struct {
	Context string `yaml:"context"`
}

type GroupSpec struct {
	Lead   string      `yaml:"lead"`
	Budget *BudgetSpec `yaml:"budget"`
}

type AgentSpec struct {
	Image   string            `yaml:"image"`
	Group   string            `yaml:"group"`
	Cwd     string            `yaml:"cwd"`
	Harness *HarnessSpec      `yaml:"harness"`
	Env     map[string]string `yaml:"env"`
	Plugins []string          `yaml:"plugins"`
	Budget  *BudgetSpec       `yaml:"budget"`
	Goal    *GoalSpec         `yaml:"goal"`
	// Timeout is the soft iteration timeout as a Go duration string ("60m",
	// "2h", "90s"); a unit is required. Empty means no override (the daemon
	// default). It maps to the agent's timeout_s and, since the hard timeout
	// derives from it (timeout_s+60s), lifts the shim's 60s hard-kill above the
	// work — the knob that keeps long, subagent-spawning iterations alive.
	Timeout string `yaml:"timeout"`
	// Loop is the optional loop cadence + failure-policy block. See LoopSpec.
	Loop *LoopSpec `yaml:"loop"`
	// Subscribe is the agent's desired bus subscriptions (spec §5.3). Each entry
	// is either a bare channel string or an object {channel, type, matcher,
	// params}; SubscribeSpec.UnmarshalYAML accepts both. Reconciled by
	// convergeSubscriptions on `up`.
	Subscribe []SubscribeSpec `yaml:"subscribe"`
}

// SubscribeSpec is one desired subscription for an agent (spec §5.3). It accepts
// two YAML forms so simple cases stay terse:
//
//	subscribe:
//	  - group:dev:broadcast                                  # string: plain sub
//	  - {channel: ci:events, type: "run.finished",           # object: filtered
//	     matcher: {"data.status": failed}}
//	  - {channel: issue-provider:query, params: {query: "..."}}     # object: parameterized
//
// Channel is the only required field. Type is a comma-separated glob list over
// message type; Matcher is a content filter (dotted paths → globs), both
// evaluated by the bus at publish-time. Params is opaque provider input,
// validated against the channel's provider schema by the daemon at apply time.
type SubscribeSpec struct {
	Channel string            `yaml:"channel"`
	Type    string            `yaml:"type"`
	Matcher map[string]string `yaml:"matcher"`
	Params  map[string]any    `yaml:"params"`
}

// UnmarshalYAML lets a subscribe entry be a bare channel string or the full
// object form. A scalar node decodes straight into Channel; a mapping decodes
// into the struct (via a type alias to avoid recursing back into this method).
func (s *SubscribeSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&s.Channel)
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("subscribe entry must be a channel string or mapping")
	}
	allowed := map[string]bool{"channel": true, "type": true, "matcher": true, "params": true}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !allowed[key] {
			return fmt.Errorf("unknown field %q in subscribe entry", key)
		}
	}
	type raw SubscribeSpec
	var r raw
	if err := node.Decode(&r); err != nil {
		return err
	}
	*s = SubscribeSpec(r)
	return nil
}

// LoopSpec is the per-agent loop cadence + failure policy (mirrors the old
// tariboys.yaml loop block). Every field is optional; an empty field leaves
// the daemon default untouched on converge. Enabled is a *bool so "unset" (nil,
// keep today's default: loop on at create) is distinct from explicit false.
type LoopSpec struct {
	Enabled   *bool  `yaml:"enabled"`
	Interval  string `yaml:"interval"`
	Timeout   string `yaml:"timeout"`
	OnTimeout string `yaml:"on_timeout"`
	OnError   string `yaml:"on_error"`
	// MaxIdleIterations is the idle-autostop threshold: the loop stops after this
	// many consecutive idle (self-declared non-productive) iterations. A *int so
	// "unset" (nil, leave the daemon default) is distinct from an explicit 0,
	// which means the feature is disabled. Negative is rejected by Validate.
	MaxIdleIterations *int `yaml:"max_idle_iterations"`
}

type GoalSpec struct {
	Enabled             *bool  `yaml:"enabled"`
	WaitCustomerTimeout string `yaml:"wait_customer_timeout"`
	DeliveryCooldown    string `yaml:"delivery_cooldown"`
}

func (a AgentSpec) goalWaitCustomerTimeoutSeconds() (int, bool, error) {
	if a.Goal == nil || a.Goal.WaitCustomerTimeout == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(a.Goal.WaitCustomerTimeout)
	if err != nil {
		return 0, true, fmt.Errorf("invalid goal wait_customer_timeout %q: %w", a.Goal.WaitCustomerTimeout, err)
	}
	if d <= 0 || d%time.Second != 0 {
		return 0, true, fmt.Errorf("goal wait_customer_timeout %q must be a positive whole number of seconds", a.Goal.WaitCustomerTimeout)
	}
	return int(d / time.Second), true, nil
}

func (a AgentSpec) goalDeliveryCooldownSeconds() (int, bool, error) {
	if a.Goal == nil || a.Goal.DeliveryCooldown == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(a.Goal.DeliveryCooldown)
	if err != nil {
		return 0, true, fmt.Errorf("invalid goal delivery_cooldown %q: %w", a.Goal.DeliveryCooldown, err)
	}
	if d <= 0 || d%time.Second != 0 {
		return 0, true, fmt.Errorf("goal delivery_cooldown %q must be a positive whole number of seconds", a.Goal.DeliveryCooldown)
	}
	return int(d / time.Second), true, nil
}

// parseDurationSeconds parses a Go duration string to whole seconds. Empty
// yields 0 (unset). A unit is required; a negative duration is rejected.
func parseDurationSeconds(what, s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q (want a duration like 60m/2h/90s): %w", what, s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("negative %s %q", what, s)
	}
	return int(d.Seconds()), nil
}

// TimeoutSeconds parses the agent's flat Timeout duration string to whole
// seconds. Empty yields 0 (unset).
func (a AgentSpec) TimeoutSeconds() (int, error) {
	return parseDurationSeconds("timeout", a.Timeout)
}

// effectiveTimeout returns the timeout duration string to converge: loop.timeout
// when the block sets it, else the flat Timeout field (back-compat).
func (a AgentSpec) effectiveTimeout() string {
	if a.Loop != nil && a.Loop.Timeout != "" {
		return a.Loop.Timeout
	}
	return a.Timeout
}

// effectiveTimeoutSeconds parses effectiveTimeout to whole seconds.
func (a AgentSpec) effectiveTimeoutSeconds() (int, error) {
	return parseDurationSeconds("timeout", a.effectiveTimeout())
}

// intervalSeconds returns (seconds, set, err). set is false when no loop
// interval is configured, so the reconciler knows to leave the daemon alone.
func (a AgentSpec) intervalSeconds() (int, bool, error) {
	if a.Loop == nil || a.Loop.Interval == "" {
		return 0, false, nil
	}
	s, err := parseDurationSeconds("interval", a.Loop.Interval)
	return s, true, err
}

// validPolicy reports whether an on_timeout/on_error value is allowed (empty =
// unset). Mirrors the daemon's allowed set in internal/commands/loop.go.
func validPolicy(v string) bool { return v == "" || v == "restart" || v == "stop" }

// HarnessSpec is a per-agent harness config (spec §5.1). It is applied as a
// launch-time override on POST /api/agents; the image (built from a
// Tariboyfile, which no longer carries harness) supplies no harness of its
// own, so this block owns the agent's harness/model/effort/interactive.
type HarnessSpec struct {
	Type        string `yaml:"type"`
	Model       string `yaml:"model"`
	Effort      string `yaml:"effort"`
	Interactive bool   `yaml:"interactive"`
}

// Validate checks a per-agent harness block: type is required and must name a
// supported harness (mirrors internal/imagefile's set).
func (h HarnessSpec) Validate() error {
	if h.Type == "" {
		return fmt.Errorf("harness.type is required")
	}
	switch h.Type {
	case "claude", "codex", "opencode", "cursor":
		return nil
	default:
		return fmt.Errorf("harness.type %q is not supported (want claude|codex|opencode|cursor)", h.Type)
	}
}

type BudgetSpec struct {
	LimitUSD float64 `yaml:"limit_usd"`
	Period   string  `yaml:"period"`
	Mode     string  `yaml:"mode"`
}

// NormalizedMode maps the compose-facing mode onto the stored budget mode
// (warn|block). "enforce" is the spec's word for "block" (reject); "" defaults
// to warn.
func (b BudgetSpec) NormalizedMode() (string, error) {
	switch b.Mode {
	case "", "warn":
		return "warn", nil
	case "block", "enforce":
		return "block", nil
	default:
		return "", fmt.Errorf("invalid budget mode %q (want warn|block|enforce)", b.Mode)
	}
}

// Parse decodes a tariboy-compose.yaml document. It does not validate the
// result; call Validate separately.
func Parse(b []byte) (File, error) {
	var f File
	decoder := yaml.NewDecoder(strings.NewReader(string(b)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&f); err != nil {
		return File{}, fmt.Errorf("parse compose file: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return File{}, fmt.Errorf("parse compose file: multiple YAML documents are not supported")
		}
		return File{}, fmt.Errorf("parse compose file: %w", err)
	}
	if declaresValue(f.RemovedWorkflows) {
		return File{}, fmt.Errorf("parse compose file: the top-level workflows key is no longer supported: the task workflow engine was removed")
	}
	return f, nil
}

// declaresValue reports whether a captured YAML value says anything: an absent
// key, null, an empty string, and an empty map or list do not.
func declaresValue(n yaml.Node) bool {
	switch n.Kind {
	case 0:
		return false
	case yaml.ScalarNode:
		return n.Tag != "!!null" && n.Value != ""
	case yaml.MappingNode, yaml.SequenceNode:
		return len(n.Content) != 0
	default:
		return true
	}
}

// Validate enforces the schema rules (spec §5.1): version, name charset (the
// shared path-traversal guard), lead-is-a-member, group references, image
// presence, and budget mode.
func (f File) Validate() error {
	if f.Version != 1 {
		return fmt.Errorf("unsupported compose version %d (want 1)", f.Version)
	}
	for name := range f.Images {
		if !agent.ValidName(name) {
			return fmt.Errorf("invalid image name %q", name)
		}
	}
	for name, g := range f.Groups {
		if !agent.ValidName(name) {
			return fmt.Errorf("invalid group name %q", name)
		}
		if g.Lead != "" {
			a, ok := f.Agents[g.Lead]
			if !ok || a.Group != name {
				return fmt.Errorf("group %q lead %q is not a member of that group", name, g.Lead)
			}
		}
		if g.Budget != nil {
			if _, err := g.Budget.NormalizedMode(); err != nil {
				return fmt.Errorf("group %q budget: %w", name, err)
			}
		}
	}
	for name, a := range f.Agents {
		if !agent.ValidName(name) {
			return fmt.Errorf("invalid agent name %q", name)
		}
		if a.Image == "" {
			return fmt.Errorf("agent %q has no image", name)
		}
		if a.Group != "" {
			if !agent.ValidName(a.Group) {
				return fmt.Errorf("agent %q references invalid group name %q", name, a.Group)
			}
			if _, ok := f.Groups[a.Group]; !ok {
				return fmt.Errorf("agent %q references undeclared group %q", name, a.Group)
			}
		}
		if a.Budget != nil {
			if _, err := a.Budget.NormalizedMode(); err != nil {
				return fmt.Errorf("agent %q budget: %w", name, err)
			}
		}
		if a.Harness != nil {
			if err := a.Harness.Validate(); err != nil {
				return fmt.Errorf("agent %q %w", name, err)
			}
		}
		if _, err := a.effectiveTimeoutSeconds(); err != nil {
			return fmt.Errorf("agent %q %w", name, err)
		}
		if _, _, err := a.goalWaitCustomerTimeoutSeconds(); err != nil {
			return fmt.Errorf("agent %q %w", name, err)
		}
		if _, _, err := a.goalDeliveryCooldownSeconds(); err != nil {
			return fmt.Errorf("agent %q %w", name, err)
		}
		if a.Loop != nil {
			if _, _, err := a.intervalSeconds(); err != nil {
				return fmt.Errorf("agent %q loop %w", name, err)
			}
			if !validPolicy(a.Loop.OnTimeout) {
				return fmt.Errorf("agent %q loop on_timeout %q must be restart|stop", name, a.Loop.OnTimeout)
			}
			if !validPolicy(a.Loop.OnError) {
				return fmt.Errorf("agent %q loop on_error %q must be restart|stop", name, a.Loop.OnError)
			}
			if a.Loop.MaxIdleIterations != nil && *a.Loop.MaxIdleIterations < 0 {
				return fmt.Errorf("agent %q loop max_idle_iterations %d must be >= 0", name, *a.Loop.MaxIdleIterations)
			}
		}
		for i, sub := range a.Subscribe {
			if sub.Channel == "" {
				return fmt.Errorf("agent %q subscribe[%d] has no channel", name, i)
			}
		}
	}
	for prefix, queue := range f.TaskQueues {
		if !taskQueuePrefixRE.MatchString(prefix) {
			return fmt.Errorf("invalid task queue prefix %q", prefix)
		}
		if strings.TrimSpace(queue.Name) == "" {
			return fmt.Errorf("task queue %q name is required", prefix)
		}
		if _, _, err := queue.WorkflowRef(); err != nil {
			return fmt.Errorf("task queue %q workflow %q: %w", prefix, queue.Workflow, err)
		}
		for pool, members := range queue.Pools {
			if !poolRouteSegmentRE.MatchString(pool) {
				return fmt.Errorf("task queue %q has unsafe pool route name %q", prefix, pool)
			}
			for _, name := range normalizeAgentNames(members) {
				if _, exists := f.Agents[name]; !exists {
					return fmt.Errorf("task queue %q pool %q references undeclared agent %q", prefix, pool, name)
				}
			}
		}
	}
	return nil
}

func normalizeAgentNames(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
