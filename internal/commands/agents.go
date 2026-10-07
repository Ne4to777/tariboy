package commands

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/aiproxy"
	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/registry"
)

func agentStore(c *registry.Ctx) *agent.Store { return agent.NewStore(c.Store) }

func agentBudgetView(c *registry.Ctx, name string) (map[string]any, error) {
	b, err := aiproxy.NewStore(c.Store, time.Now).AgentBudgetStatus(name, time.Now())
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"hour_usd": b.HourUSD, "day_usd": b.DayUSD, "week_usd": b.WeekUSD, "month_usd": b.MonthUSD,
		"hour_spent_usd": b.HourSpentUSD, "day_spent_usd": b.DaySpentUSD,
		"week_spent_usd": b.WeekSpentUSD, "month_spent_usd": b.MonthSpentUSD,
		"exhausted": b.Exhausted,
	}, nil
}

func agentView(c *registry.Ctx, a agent.Agent, state string) (map[string]any, error) {
	v := map[string]any{
		"name": a.Name, "image": a.ImageRef, "digest": a.ImageDigest, "state": state,
		"cwd": a.Cwd, "configured_cwd": a.Cwd,
		"harness": a.HarnessType, "model": a.Model, "effort": a.Effort,
		"interactive": a.Interactive, "loop_enabled": a.LoopEnabled, "enabled": a.Enabled, "interval_s": a.IntervalS,
		"timeout_s": a.TimeoutS, "hard_timeout_s": a.HardTimeoutS, "on_timeout": a.OnTimeout,
		"on_error": a.OnError, "user_prompt": a.UserPrompt, "env": a.Env, "plugins": a.Plugins,
		"messages_batch": a.MessagesBatch, "messages_max_queue": a.MessagesMaxQueue,
		"goal_enabled": a.GoalEnabled, "goal_wait_customer_timeout_s": a.GoalWaitCustomerTimeoutS, "goal_delivery_cooldown_s": a.GoalDeliveryCooldownS,
		"current_goal_task_key": a.CurrentGoalTaskKey,
		"group":                 a.Group, "alias": a.Alias, "notes": a.Notes, "color": a.Color,
		"max_idle_iterations": a.MaxIdleIterations,
		"ai_stall_timeout_s":  a.AIStallTimeoutS,
	}
	budget, err := agentBudgetView(c, a.Name)
	if err != nil {
		return nil, err
	}
	v["budget"] = budget
	addErrorReason(v, a, state)
	addHaltReason(v, a)
	return v, nil
}

func addErrorReason(v map[string]any, a agent.Agent, state string) {
	if a.ErrorReason != "" {
		v["error_reason"] = a.ErrorReason
	} else if state == "error" && a.AIStallTimeoutS > 0 {
		v["error_reason"] = fmt.Sprintf("agent stalled - no AI-proxy requests for %d seconds", a.AIStallTimeoutS)
	}
}

func agentImageSet() registry.Command {
	return registry.Command{Path: "agent.image.set", Summary: "Select an image for the agent's next iteration", Args: []registry.Arg{{Name: "name", Type: registry.String, Required: true}, {Name: "image", Type: registry.String, Required: true}}, HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/agents/{name}/image"}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		var result any
		err := image.WithPublicationGate(func() error {
			name := str(p, "name")
			if _, err := getAgent(c, name); err != nil {
				return err
			}
			ref, err := image.ParseRef(str(p, "image"))
			if err != nil {
				return api.UserError{Code: "bad_ref", Msg: err.Error(), Status: http.StatusBadRequest}
			}
			manifest, err := imageStore(c).Inspect(ref)
			if err != nil {
				return api.UserError{Code: "image_not_found", Msg: err.Error(), Status: http.StatusNotFound}
			}
			if err := agentStore(c).SetPendingImage(name, ref.String(), manifest.Digest); err != nil {
				return err
			}
			result, err = agentImageStatusValue(c, name)
			return err
		})
		return result, err
	}}
}

type agentImageProjection struct {
	agent.ImageAssignment
	ImageVersion string `json:"image_version,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// The caller holds the publication gate, including when set returns its preview.
func agentImageStatusValue(c *registry.Ctx, name string) (map[string]any, error) {
	a, err := getAgent(c, name)
	if err != nil {
		return nil, err
	}
	pending, err := agentStore(c).PendingImage(name)
	if err != nil {
		return nil, err
	}
	images := imageStore(c)
	inspect := func(refText, digest, reason string) agentImageProjection {
		projection := agentImageProjection{ImageAssignment: agent.ImageAssignment{Ref: refText, Digest: digest}, Reason: reason}
		ref, err := image.ParseRef(refText)
		var manifest image.Manifest
		if err == nil {
			if reason == "ref_moved" {
				manifest, err = images.Inspect(ref)
			} else {
				manifest, err = images.InspectPinned(ref, digest)
			}
		}
		if err != nil {
			projection.Error = err.Error()
		} else {
			projection.Digest = manifest.Digest
			projection.ImageVersion = manifest.ImageVersion
		}
		return projection
	}
	current := inspect(a.ImageRef, a.ImageDigest, "")
	next := current
	next.Reason = "current"
	if pending.Ref != "" {
		next = inspect(pending.Ref, pending.Digest, "pending")
	} else if _, err := image.ParseRef(a.ImageRef); err == nil {
		// Any ref can be rebuilt, so the active ref's current content is what
		// the next iteration would adopt.
		next = inspect(a.ImageRef, "", "ref_moved")
	}
	return map[string]any{"name": name, "current": current, "pending": pending, "next": next}, nil
}

func agentImageStatus() registry.Command {
	return registry.Command{Path: "agent.image.status", Summary: "Show current, pending, and next agent image", Args: []registry.Arg{{Name: "name", Type: registry.String, Required: true}}, HTTP: &registry.HTTPRoute{Method: http.MethodGet, Path: "/api/agents/{name}/image"}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		var result any
		err := image.WithPublicationGate(func() error {
			var err error
			result, err = agentImageStatusValue(c, str(p, "name"))
			return err
		})
		return result, err
	}}
}

func agentImageCancel() registry.Command {
	return registry.Command{Path: "agent.image.cancel", Summary: "Cancel a pending agent image", Args: []registry.Arg{{Name: "name", Type: registry.String, Required: true}}, HTTP: &registry.HTTPRoute{Method: http.MethodDelete, Path: "/api/agents/{name}/image"}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		var result any
		err := image.WithPublicationGate(func() error {
			name := str(p, "name")
			if err := agentStore(c).ClearPendingImage(name); err != nil {
				return err
			}
			var err error
			result, err = agentImageStatusValue(c, name)
			return err
		})
		return result, err
	}}
}

// addHaltReason emits the derived halt keys on a reader's output, and only when
// there is a halt: with no reason NEITHER key appears, matching the
// emit-only-when-non-empty style of error_reason above. Every reader goes
// through here so the idle-prefix test lives in exactly one place.
func addHaltReason(v map[string]any, a agent.Agent) {
	if kind, reason := a.HaltReason(); kind != "" {
		v["halt_kind"] = kind
		v["halt_reason"] = reason
	}
}

func parseKV(list string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(list, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if i := strings.Index(pair, "="); i >= 0 {
			out[pair[:i]] = pair[i+1:]
		}
	}
	return out
}

func parseList(list string) []string {
	var out []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseAgentEnv(value any) (map[string]string, error) {
	switch value := value.(type) {
	case nil:
		return map[string]string{}, nil
	case string:
		return parseKV(value), nil
	case map[string]string:
		return value, nil
	case map[string]any:
		out := make(map[string]string, len(value))
		for key, raw := range value {
			text, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("env value for %q must be a string", key)
			}
			out[key] = text
		}
		return out, nil
	default:
		return nil, fmt.Errorf("env must be a K=V string or string-valued object")
	}
}

func parseAgentPlugins(value any) ([]string, error) {
	if text, ok := value.(string); ok {
		return parseList(text), nil
	}
	plugins, ok := stringSlice(value)
	if !ok {
		return nil, fmt.Errorf("plugins must be a comma list or string array")
	}
	return plugins, nil
}

func parseAgentIntParam(p registry.Params, key string) (int, bool, error) {
	raw, present := p[key]
	if !present {
		return 0, false, nil
	}
	switch value := raw.(type) {
	case int:
		return value, true, nil
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
			return 0, true, fmt.Errorf("%s must be a whole number", key)
		}
		parsed, err := strconv.ParseInt(strconv.FormatFloat(value, 'f', -1, 64), 10, 0)
		if err != nil {
			return 0, true, fmt.Errorf("%s is outside the supported integer range", key)
		}
		return int(parsed), true, nil
	case string:
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, true, fmt.Errorf("%s must be a whole number", key)
		}
		return parsed, true, nil
	default:
		return 0, true, fmt.Errorf("%s must be a whole number", key)
	}
}

func agentIntParam(p registry.Params, key, code string, minimum int) (int, error) {
	value, present, err := parseAgentIntParam(p, key)
	if err != nil || present && value < minimum {
		if err == nil {
			err = fmt.Errorf("%s must be at least %d", key, minimum)
		}
		return 0, api.UserError{Code: code, Msg: err.Error()}
	}
	return value, nil
}

var agentColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func parseAgentTimeout(value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return 0, fmt.Errorf("invalid timeout %q (want 60m, 2h, or 90s)", value)
	}
	return int(duration.Seconds()), nil
}

func agentRun() registry.Command {
	return registry.Command{
		Path:    "agent.run",
		Summary: "Create an agent from an image (image:tag); starts stopped — enable with 'agent start'",
		Args: []registry.Arg{
			{Name: "image", Type: registry.String, Required: true, Help: "image ref name:tag"},
			{Name: "name", Flag: "name", Type: registry.String, Help: "agent name (default: generated)"},
			{Name: "cwd", Flag: "cwd", Type: registry.String, Help: "working directory"},
			{Name: "harness", Flag: "harness", Short: "a", Type: registry.String, Help: "harness: claude|codex|opencode|cursor|stub"},
			{Name: "model", Flag: "model", Short: "m", Type: registry.String, Help: "model"},
			{Name: "effort", Flag: "effort", Short: "e", Type: registry.String, Help: "effort"},
			{Name: "interactive", Flag: "interactive", Short: "i", Type: registry.Bool, Help: "interactive (tmux) mode"},
			{Name: "env", Flag: "env", Type: registry.String, Help: "comma-separated K=V env pairs", Schema: map[string]any{"oneOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			}}},
			{Name: "plugins", Flag: "plugins", Type: registry.String, Help: "comma-separated plugin override", Schema: map[string]any{"oneOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			}}},
			{Name: "group", Flag: "group", Short: "g", Type: registry.String, Help: "join this group"},
			{Name: "loop", Flag: "loop", Type: registry.Bool, Default: true, Help: "set loop intent (nested; agent still starts stopped — default true)"},
			{Name: "timeout", Flag: "timeout", Type: registry.String, Help: "soft iteration timeout duration (for example 60m or 2h)"},
			{Name: "interval_s", Type: registry.Int, Help: "loop interval in whole seconds"},
			{Name: "timeout_s", Type: registry.Int, Help: "soft iteration timeout in whole seconds"},
			{Name: "hard_timeout_s", Type: registry.Int, Help: "hard iteration timeout in whole seconds"},
			{Name: "on_timeout", Type: registry.String, Help: "timeout policy: restart or stop"},
			{Name: "on_error", Type: registry.String, Help: "error policy: restart or stop"},
			{Name: "max_idle_iterations", Type: registry.Int, Help: "maximum consecutive idle iterations; 0 disables"},
			{Name: "ai_stall_timeout_s", Type: registry.Int, Default: 1800, Help: "AI-proxy inactivity error threshold in whole seconds (default 1800)"},
			{Name: "user_prompt", Type: registry.String, Help: "standing user prompt"},
			{Name: "messages_batch", Type: registry.Int, Help: "maximum messages delivered per iteration"},
			{Name: "messages_max_queue", Type: registry.Int, Help: "maximum queued messages"},
			{Name: "goal_enabled", Flag: "goal-enabled", Type: registry.Bool, Default: true, Help: "enable Native Task goal selection (default true)"},
			{Name: "goal_wait_customer_timeout_s", Flag: "goal-wait-customer-timeout-s", Type: registry.Int, Default: 300, Help: "customer-wait grace period in whole seconds (default 300)"},
			{Name: "goal_delivery_cooldown_s", Flag: "goal-delivery-cooldown-s", Type: registry.Int, Default: 60, Help: "minimum seconds between Goal deliveries (default 60)"},
			{Name: "alias", Type: registry.String, Help: "display alias"},
			{Name: "notes", Type: registry.String, Help: "operator notes"},
			{Name: "color", Type: registry.String, Help: "agent accent color as #rrggbb"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			loop := true
			if v, ok := p["loop"].(bool); ok {
				loop = v
			}
			interactive, _ := p["interactive"].(bool)
			goalEnabled := true
			if value, present := p["goal_enabled"]; present {
				var ok bool
				goalEnabled, ok = value.(bool)
				if !ok {
					return nil, api.UserError{Code: "bad_goal_enabled", Msg: "goal_enabled must be a boolean"}
				}
			}
			goalWaitCustomerTimeoutS := 300
			if _, present := p["goal_wait_customer_timeout_s"]; present {
				var err error
				goalWaitCustomerTimeoutS, err = agentIntParam(p, "goal_wait_customer_timeout_s", "bad_goal_wait_customer_timeout", 1)
				if err != nil {
					return nil, err
				}
			}
			goalDeliveryCooldownS := 60
			if _, present := p["goal_delivery_cooldown_s"]; present {
				var err error
				goalDeliveryCooldownS, err = agentIntParam(p, "goal_delivery_cooldown_s", "bad_goal_delivery_cooldown", 1)
				if err != nil {
					return nil, err
				}
			}
			if _, oldTimeout := p["timeout"]; oldTimeout {
				if _, exactTimeout := p["timeout_s"]; exactTimeout {
					return nil, api.UserError{Code: "ambiguous_timeout", Msg: "timeout and timeout_s cannot be supplied together"}
				}
			}
			env, envErr := parseAgentEnv(p["env"])
			if envErr != nil {
				return nil, api.UserError{Code: "bad_env", Msg: envErr.Error()}
			}
			pluginList, pluginsErr := parseAgentPlugins(p["plugins"])
			if pluginsErr != nil {
				return nil, api.UserError{Code: "bad_plugins", Msg: pluginsErr.Error()}
			}
			timeoutS, timeoutErr := parseAgentTimeout(str(p, "timeout"))
			if timeoutErr != nil {
				return nil, api.UserError{Code: "bad_timeout", Msg: timeoutErr.Error()}
			}
			intervalS, err := agentIntParam(p, "interval_s", "bad_interval", 0)
			if err != nil {
				return nil, err
			}
			if _, present := p["timeout_s"]; present {
				timeoutS, err = agentIntParam(p, "timeout_s", "bad_timeout", 0)
				if err != nil {
					return nil, err
				}
			}
			hardTimeoutS, err := agentIntParam(p, "hard_timeout_s", "bad_hard_timeout", 0)
			if err != nil {
				return nil, err
			}
			maxIdleIterations, err := agentIntParam(p, "max_idle_iterations", "bad_max_idle", 0)
			if err != nil {
				return nil, err
			}
			aiStallTimeoutS := 1800
			if _, present := p["ai_stall_timeout_s"]; present {
				aiStallTimeoutS, err = agentIntParam(p, "ai_stall_timeout_s", "bad_ai_stall_timeout", 1)
				if err != nil {
					return nil, err
				}
			}
			messagesBatch, err := agentIntParam(p, "messages_batch", "bad_messages_batch", 1)
			if err != nil {
				return nil, err
			}
			messagesMaxQueue, err := agentIntParam(p, "messages_max_queue", "bad_messages_max_queue", 1)
			if err != nil {
				return nil, err
			}
			if _, present := p["messages_batch"]; !present {
				messagesBatch = 10
			}
			if _, present := p["messages_max_queue"]; !present {
				messagesMaxQueue = 100
			}
			onTimeout := str(p, "on_timeout")
			if _, present := p["on_timeout"]; present && onTimeout != "restart" && onTimeout != "stop" {
				return nil, api.UserError{Code: "bad_on_timeout", Msg: "on_timeout must be restart or stop"}
			}
			onError := str(p, "on_error")
			if _, present := p["on_error"]; present && onError != "restart" && onError != "stop" {
				return nil, api.UserError{Code: "bad_on_error", Msg: "on_error must be restart or stop"}
			}
			color := str(p, "color")
			if color != "" && !agentColorPattern.MatchString(color) {
				return nil, api.UserError{Code: "bad_color", Msg: "color must be empty or a six-digit #rrggbb value"}
			}
			spec := registry.RunSpec{
				ImageRef: str(p, "image"), Name: str(p, "name"), Cwd: str(p, "cwd"),
				Harness: str(p, "harness"), Model: str(p, "model"), Effort: str(p, "effort"),
				Interactive: interactive, Env: env, Plugins: pluginList, Loop: loop,
				IntervalS: intervalS, TimeoutS: timeoutS, HardTimeoutS: hardTimeoutS,
				OnTimeout: onTimeout, OnError: onError,
				MaxIdleIterations:        maxIdleIterations,
				AIStallTimeoutS:          aiStallTimeoutS,
				UserPrompt:               str(p, "user_prompt"),
				MessagesBatch:            messagesBatch,
				MessagesMaxQueue:         messagesMaxQueue,
				GoalEnabled:              &goalEnabled,
				GoalWaitCustomerTimeoutS: goalWaitCustomerTimeoutS,
				GoalDeliveryCooldownS:    goalDeliveryCooldownS,
				Group:                    str(p, "group"), Alias: str(p, "alias"), Notes: str(p, "notes"),
				Color: strings.ToLower(color),
			}
			if spec.ImageRef == "" {
				return nil, api.UserError{Code: "missing_image", Msg: "image ref is required"}
			}
			// Expand a relative/~ cwd against the daemon's fs root ($HOME) before
			// validating — the UI cwd picker offers paths relative to that root, so
			// "tmp/" must become $HOME/tmp rather than be rejected as non-absolute.
			resolvedCwd, err := resolveCwd(spec.Cwd)
			if err != nil {
				return nil, api.UserError{Code: "bad_cwd", Msg: err.Error()}
			}
			spec.Cwd = resolvedCwd
			if err := agent.ValidateCwd(spec.Cwd); err != nil {
				return nil, api.UserError{Code: "bad_cwd", Msg: err.Error()}
			}
			name, err := c.Control.Run(spec)
			if err != nil {
				return nil, api.UserError{Code: "run_failed", Msg: err.Error()}
			}
			state, _ := c.Control.LiveState(name)
			return map[string]any{"name": name, "state": state,
				"goal_enabled": goalEnabled, "goal_wait_customer_timeout_s": goalWaitCustomerTimeoutS, "goal_delivery_cooldown_s": goalDeliveryCooldownS}, nil
		},
	}
}

func agentPs() registry.Command {
	return registry.Command{
		Path:    "agent.ps",
		Summary: "List agents",
		HTTP:    &registry.HTTPRoute{Method: "GET", Path: "/api/agents"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			list, err := agentStore(c).List()
			if err != nil {
				return nil, err
			}
			rows := make([]map[string]any, 0, len(list))
			for _, a := range list {
				state, _ := c.Control.LiveState(a.Name)
				row := map[string]any{
					"name": a.Name, "image": a.ImageRef, "state": state,
					"alias":   a.Alias,
					"harness": a.HarnessType, "loop_enabled": a.LoopEnabled, "enabled": a.Enabled, "group": a.Group,
					"color": a.Color, "cwd": a.Cwd, "timeout_s": a.TimeoutS,
					"interval_s": a.IntervalS, "on_timeout": a.OnTimeout, "on_error": a.OnError,
					"max_idle_iterations": a.MaxIdleIterations,
					"ai_stall_timeout_s":  a.AIStallTimeoutS,
					"interactive":         a.Interactive,
					"goal_enabled":        a.GoalEnabled, "goal_wait_customer_timeout_s": a.GoalWaitCustomerTimeoutS, "goal_delivery_cooldown_s": a.GoalDeliveryCooldownS,
					"current_goal_task_key": a.CurrentGoalTaskKey,
				}
				if budget, err := agentBudgetView(c, a.Name); err != nil {
					return nil, err
				} else {
					row["budget"] = budget
				}
				addErrorReason(row, a, state)
				addHaltReason(row, a)
				rows = append(rows, row)
			}
			return map[string]any{"agents": rows, "count": len(rows)}, nil
		},
	}
}

func agentGoalSettingsView(a agent.Agent) map[string]any {
	return map[string]any{"name": a.Name, "goal_enabled": a.GoalEnabled,
		"goal_wait_customer_timeout_s": a.GoalWaitCustomerTimeoutS, "goal_delivery_cooldown_s": a.GoalDeliveryCooldownS}
}

func refreshAgentGoal(c *registry.Ctx, name string) {
	if control, ok := c.Control.(registry.LoopConfigControl); ok {
		control.RefreshLoopConfig(name)
	}
}

func agentGoalEnabled() registry.Command {
	return registry.Command{
		Path: "agent.goal-enabled", Summary: "Enable or disable Native Task goal selection",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "enabled", Flag: "enabled", Type: registry.Bool, Required: true, Help: "true or false"},
		},
		HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/agents/{name}/goal-enabled"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			enabled, ok := p["enabled"].(bool)
			if !ok {
				return nil, api.UserError{Code: "bad_goal_enabled", Msg: "enabled must be a boolean"}
			}
			a.GoalEnabled = enabled
			if err := agentStore(c).Update(a); err != nil {
				return nil, err
			}
			refreshAgentGoal(c, a.Name)
			return agentGoalSettingsView(a), nil
		},
	}
}

func agentGoalWaitCustomerTimeout() registry.Command {
	return registry.Command{
		Path: "agent.goal-wait-customer-timeout", Summary: "Set the Goal customer-wait grace period",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "seconds", Flag: "seconds", Type: registry.Int, Required: true, Help: "positive whole seconds"},
		},
		HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/agents/{name}/goal-wait-customer-timeout"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			if _, present := p["seconds"]; !present {
				return nil, api.UserError{Code: "bad_goal_wait_customer_timeout", Msg: "seconds must be at least 1"}
			}
			seconds, err := agentIntParam(p, "seconds", "bad_goal_wait_customer_timeout", 1)
			if err != nil {
				return nil, err
			}
			a.GoalWaitCustomerTimeoutS = seconds
			if err := agentStore(c).Update(a); err != nil {
				return nil, err
			}
			refreshAgentGoal(c, a.Name)
			return agentGoalSettingsView(a), nil
		},
	}
}

func agentGoalDeliveryCooldown() registry.Command {
	return registry.Command{Path: "agent.goal-delivery-cooldown", Summary: "Set the Goal delivery cooldown", Args: []registry.Arg{{Name: "name", Type: registry.String, Required: true}, {Name: "seconds", Type: registry.Int, Required: true}}, HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/agents/{name}/goal-delivery-cooldown"}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		a, err := getAgent(c, str(p, "name"))
		if err != nil {
			return nil, err
		}
		seconds, err := agentIntParam(p, "seconds", "bad_goal_delivery_cooldown", 1)
		if err != nil {
			return nil, err
		}
		a.GoalDeliveryCooldownS = seconds
		if err := agentStore(c).Update(a); err != nil {
			return nil, err
		}
		refreshAgentGoal(c, a.Name)
		return agentGoalSettingsView(a), nil
	}}
}

func agentStatus() registry.Command {
	return registry.Command{
		Path:    "agent.status.show",
		Summary: "Show one agent's runtime status",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "agent name"}},
		HTTP:    &registry.HTTPRoute{Method: "GET", Path: "/api/agents/{name}/status"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			b, err := requireBus(c)
			if err != nil {
				return nil, err
			}
			messagesPending, err := b.PendingCount(a.Name)
			if err != nil {
				return nil, err
			}
			state, _ := c.Control.LiveState(a.Name)
			its, _ := agentStore(c).ListIterations(a.Name)
			last, lastID := "", ""
			if n := len(its); n > 0 {
				last = its[n-1].Status
				lastID = its[n-1].ID
			}
			out := map[string]any{"name": a.Name, "state": state, "loop_enabled": a.LoopEnabled, "enabled": a.Enabled,
				"iterations": len(its), "last_iteration": last, "last_iteration_id": lastID,
				"status_message": a.StatusMessage, "status_updated": a.StatusUpdated,
				"messages_pending": messagesPending, "messages_max_queue": a.MessagesMaxQueue,
				"messages_queue_full": messagesPending >= a.MessagesMaxQueue,
				"server_now":          time.Now().UTC().Format(time.RFC3339Nano)}
			if budget, err := agentBudgetView(c, a.Name); err != nil {
				return nil, err
			} else {
				out["budget"] = budget
			}
			addErrorReason(out, a, state)
			addHaltReason(out, a)
			for i := len(its) - 1; i >= 0; i-- {
				it := its[i]
				if it.Status != "running" {
					continue
				}
				active := map[string]any{"id": it.ID, "started_at": it.StartedAt,
					"timeout_extensions": it.TimeoutExtensions}
				if it.TimeoutPeriodS != nil {
					active["timeout_period_s"] = *it.TimeoutPeriodS
				}
				if it.TimeoutDeadline != nil {
					active["timeout_deadline"] = *it.TimeoutDeadline
				}
				if it.HardTimeoutDeadline != nil {
					active["hard_timeout_deadline"] = *it.HardTimeoutDeadline
				}
				if it.TimeoutDeadline != nil || it.HardTimeoutDeadline != nil {
					active["effective_deadline"] = effectiveDeadline(it.TimeoutDeadline, it.HardTimeoutDeadline)
				}
				out["active_iteration"] = active
				break
			}
			return out, nil
		},
	}
}

func effectiveDeadline(soft, hard *string) string {
	if soft == nil {
		return *hard
	}
	if hard == nil {
		return *soft
	}
	st, serr := time.Parse(time.RFC3339Nano, *soft)
	ht, herr := time.Parse(time.RFC3339Nano, *hard)
	if serr == nil && herr == nil && ht.Before(st) {
		return *hard
	}
	return *soft
}

func agentInspect() registry.Command {
	return registry.Command{
		Path:    "agent.inspect",
		Summary: "Show one agent's full config",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "agent name"}},
		HTTP:    &registry.HTTPRoute{Method: "GET", Path: "/api/agents/{name}"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			state, _ := c.Control.LiveState(a.Name)
			v, err := agentView(c, a, state)
			if err != nil {
				return nil, err
			}
			// Report the EFFECTIVE cwd (the workdir fallback when Cwd is unset),
			// matching cwdOf/agentCwdFor, so the UI never shows an empty value.
			if cwd, err := agentCwdFor(c, a.Name); err == nil {
				v["cwd"] = cwd
			}
			return v, nil
		},
	}
}

func agentBudgetGet() registry.Command {
	return registry.Command{Path: "agent.budget.get", Summary: "Show an agent's calendar USD budget", Args: []registry.Arg{{Name: "name", Type: registry.String, Required: true}}, HTTP: &registry.HTTPRoute{Method: http.MethodGet, Path: "/api/agents/{name}/budget"}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		name := str(p, "name")
		if _, err := getAgent(c, name); err != nil {
			return nil, err
		}
		return agentBudgetView(c, name)
	}}
}

func agentBudgetSet() registry.Command {
	return registry.Command{Path: "agent.budget.set", Summary: "Set an agent's calendar USD budget", Args: []registry.Arg{
		{Name: "name", Type: registry.String, Required: true},
		{Name: "hour_usd", Type: registry.String, Required: true}, {Name: "day_usd", Type: registry.String, Required: true},
		{Name: "week_usd", Type: registry.String, Required: true}, {Name: "month_usd", Type: registry.String, Required: true},
	}, HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/agents/{name}/budget"}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		name := str(p, "name")
		if _, err := getAgent(c, name); err != nil {
			return nil, err
		}
		values := []string{str(p, "hour_usd"), str(p, "day_usd"), str(p, "week_usd"), str(p, "month_usd")}
		parsed := [4]float64{}
		for i, value := range values {
			var err error
			parsed[i], err = parseFloat(value)
			if err != nil {
				return nil, api.UserError{Code: "bad_budget", Msg: "budget limits must be finite non-negative USD values"}
			}
		}
		if err := aiproxy.NewStore(c.Store, nil).SetAgentBudget(name, aiproxy.AgentBudget{HourUSD: parsed[0], DayUSD: parsed[1], WeekUSD: parsed[2], MonthUSD: parsed[3]}); err != nil {
			return nil, api.UserError{Code: "bad_budget", Msg: err.Error()}
		}
		return agentBudgetView(c, name)
	}}
}

func agentLifecycle(path, summary, verb string) registry.Command {
	return registry.Command{
		Path:    path,
		Summary: summary,
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "agent name"}},
		HTTP:    &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/" + verb},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			name := str(p, "name")
			var err error
			switch verb {
			case "stop":
				err = c.Control.Stop(name)
			case "start":
				err = c.Control.Start(name)
			case "restart":
				err = c.Control.Restart(name)
			case "kill":
				err = c.Control.Kill(name)
			}
			if err != nil {
				return nil, api.UserError{Code: verb + "_failed", Msg: err.Error()}
			}
			return map[string]any{"name": name, "action": verb}, nil
		},
	}
}

func agentRm() registry.Command {
	return registry.Command{
		Path:    "agent.rm",
		Summary: "Remove an agent (stop it first, or --force)",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "force", Flag: "force", Short: "f", Type: registry.Bool, Help: "kill and remove even if running"},
			{Name: "purge", Flag: "purge", Type: registry.Bool, Help: "also delete durable data (iterations, context, audit) and leaked rows; default preserves data"},
		},
		HTTP: &registry.HTTPRoute{Method: "DELETE", Path: "/api/agents/{name}"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			force := toBool(p["force"])
			purge := toBool(p["purge"])
			if err := c.Control.Remove(str(p, "name"), force, purge); err != nil {
				return nil, api.UserError{Code: "rm_failed", Msg: err.Error()}
			}
			return map[string]any{"removed": str(p, "name"), "purged": purge}, nil
		},
	}
}

func agentReprovision() registry.Command {
	return registry.Command{
		Path:    "agent.reprovision",
		Summary: "Re-unpack an agent's image tree in place and restart its loop, keeping its data",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "image", Flag: "image", Type: registry.String, Help: "image ref to (re)unpack; empty keeps the current image"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/reprovision"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			name := str(p, "name")
			if err := c.Control.Reprovision(name, str(p, "image")); err != nil {
				return nil, api.UserError{Code: "reprovision_failed", Msg: err.Error()}
			}
			state, _ := c.Control.LiveState(name)
			return map[string]any{"name": name, "state": state}, nil
		},
	}
}

func getAgent(c *registry.Ctx, name string) (agent.Agent, error) {
	a, err := agentStore(c).Get(name)
	if err != nil {
		return agent.Agent{}, api.UserError{Code: "not_found", Msg: "agent " + name + " not found"}
	}
	return a, nil
}

func str(p registry.Params, key string) string {
	s, _ := p[key].(string)
	return s
}

// toBool accepts the CLI's typed bool and the daemon's query-string "true".
func toBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	default:
		return false
	}
}
