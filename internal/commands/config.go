package commands

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/fsbrowser"
	"github.com/alekzonder/tariboy/internal/registry"
)

// resolveCwd expands a user-supplied agent cwd to an absolute path before
// agent.ValidateCwd checks it (ValidateCwd itself only accepts an already-
// absolute path and leaves expansion to the caller). Rules:
//   - empty stays empty — the agent falls back to its managed workdir;
//   - an absolute path is honored as-is;
//   - a leading "~"/"~/" and any relative path resolve against the daemon's
//     filesystem root (TARIBOY_FS_ROOT, else the daemon user's $HOME) — the
//     SAME root the UI cwd picker (fsbrowser) browses, so "tmp/" selected there
//     becomes $HOME/tmp.
//
// Existence + is-directory validation stays in ValidateCwd, which runs on the
// resolved path.
func resolveCwd(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", nil
	}
	switch {
	case p == "~":
		p = ""
	case strings.HasPrefix(p, "~/"):
		p = p[2:]
	}
	if p != "" && filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	root, err := fsbrowser.Root()
	if err != nil {
		return "", err
	}
	if p == "" {
		return root, nil
	}
	return filepath.Clean(filepath.Join(root, p)), nil
}

// agentCwd gets or sets the agent working directory. Omitting `value` reads it;
// an empty value clears it (falls back to the workdir). The value must be a
// fully-resolved absolute path to an existing directory ($CWD/$HOME/~ are the
// caller's to expand). A new value applies on the next iteration.
func agentCwd() registry.Command {
	return registry.Command{
		Path:    "agent.cwd",
		Summary: "Get or set the agent working directory (omit value to read, empty to clear); applies next iteration",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "value", Type: registry.String, Help: "new cwd, absolute path (omit to read, empty to clear)"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/cwd"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			if _, present := p["value"]; present {
				v, err := resolveCwd(str(p, "value"))
				if err != nil {
					return nil, api.UserError{Code: "bad_cwd", Msg: err.Error()}
				}
				if err := agent.ValidateCwd(v); err != nil {
					return nil, api.UserError{Code: "bad_cwd", Msg: err.Error()}
				}
				a.Cwd = v
				if err := agentStore(c).Update(a); err != nil {
					return nil, err
				}
			}
			return map[string]any{"name": a.Name, "cwd": a.Cwd}, nil
		},
	}
}

// agentModel gets or sets the agent's model. Omitting `value` reads it. A new
// value is persisted to the agent record and picked up on the next iteration.
func agentModel() registry.Command {
	return registry.Command{
		Path:    "agent.model",
		Summary: "Get or set the agent model (omit value to read); applies next iteration",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "value", Type: registry.String, Help: "new model (omit to read)"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/model"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			if v := str(p, "value"); v != "" {
				a.Model = v
				if err := agentStore(c).Update(a); err != nil {
					return nil, err
				}
			}
			return map[string]any{"name": a.Name, "model": a.Model}, nil
		},
	}
}

// agentEffort gets or sets the agent's effort. Same semantics as agentModel.
func agentEffort() registry.Command {
	return registry.Command{
		Path:    "agent.effort",
		Summary: "Get or set the agent effort (omit value to read); applies next iteration",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "value", Type: registry.String, Help: "new effort (omit to read)"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/effort"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			if v := str(p, "value"); v != "" {
				a.Effort = v
				if err := agentStore(c).Update(a); err != nil {
					return nil, err
				}
			}
			return map[string]any{"name": a.Name, "effort": a.Effort}, nil
		},
	}
}

func agentMessageSetting(path, field string) registry.Command {
	return registry.Command{
		Path:    path,
		Summary: "Set the agent " + strings.ReplaceAll(field, "_", " "),
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "value", Type: registry.Int, Required: true, Help: "positive whole number"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/messages/" + strings.TrimPrefix(path, "agent.messages.")},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			value, err := agentIntParam(p, "value", "bad_"+field, 1)
			if err != nil {
				return nil, err
			}
			if field == "messages_batch" {
				a.MessagesBatch = value
			} else {
				a.MessagesMaxQueue = value
			}
			if err := agentStore(c).Update(a); err != nil {
				return nil, err
			}
			if control, ok := c.Control.(registry.LoopConfigControl); ok {
				control.RefreshLoopConfig(a.Name)
			}
			return map[string]any{"name": a.Name, field: value}, nil
		},
	}
}

// agentAlias sets (POST) the agent's display alias. Omit value to read; empty
// clears it. Persisted via the owned SetAlias setter (not Store.Update).
func agentAlias() registry.Command {
	return registry.Command{
		Path:    "agent.alias.set",
		Summary: "Get or set the agent display alias (omit value to read, empty to clear)",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "value", Type: registry.String, Help: "new alias (omit to read, empty to clear)"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/alias"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			if _, present := p["value"]; present {
				a.Alias = str(p, "value")
				if err := agentStore(c).SetAlias(a.Name, a.Alias); err != nil {
					return nil, err
				}
			}
			return map[string]any{"name": a.Name, "alias": a.Alias}, nil
		},
	}
}

// agentAliasGet is the GET half of the alias route (same path, GET method).
func agentAliasGet() registry.Command {
	return registry.Command{
		Path:    "agent.alias.get",
		Summary: "Read the agent display alias",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "agent name"}},
		HTTP:    &registry.HTTPRoute{Method: "GET", Path: "/api/agents/{name}/alias"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"name": a.Name, "alias": a.Alias}, nil
		},
	}
}

// agentNotes sets (POST) the agent's free-form notes. Omit value to read; empty
// clears it. Persisted via the owned SetNotes setter (not Store.Update).
func agentNotes() registry.Command {
	return registry.Command{
		Path:    "agent.notes.set",
		Summary: "Get or set the agent notes (omit value to read, empty to clear)",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "value", Type: registry.String, Help: "new notes (omit to read, empty to clear)"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/notes"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			if _, present := p["value"]; present {
				a.Notes = str(p, "value")
				if err := agentStore(c).SetNotes(a.Name, a.Notes); err != nil {
					return nil, err
				}
			}
			return map[string]any{"name": a.Name, "notes": a.Notes}, nil
		},
	}
}

// agentNotesGet is the GET half of the notes route (same path, GET method).
func agentNotesGet() registry.Command {
	return registry.Command{
		Path:    "agent.notes.get",
		Summary: "Read the agent notes",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "agent name"}},
		HTTP:    &registry.HTTPRoute{Method: "GET", Path: "/api/agents/{name}/notes"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"name": a.Name, "notes": a.Notes}, nil
		},
	}
}

// hexColorRe matches a canonical 6-digit hex color with a leading '#', e.g.
// "#4f8cff" (case-insensitive). This is the single source of truth for a valid
// color and matches the frontend's isValidHex (ui/src/lib/utils.ts) exactly;
// native only ever emits 6-digit hex, so 3-digit shorthand is intentionally
// rejected to keep backend and UI validation in lockstep.
var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// isValidHex reports whether s is a valid #rrggbb hex color.
func isValidHex(s string) bool { return hexColorRe.MatchString(s) }

// agentColor sets (POST) the agent's accent color. Omit value to read; empty
// clears it. A non-empty value must be a valid #rrggbb hex string.
// Persisted via the owned SetColor setter (not Store.Update).
func agentColor() registry.Command {
	return registry.Command{
		Path:    "agent.color.set",
		Summary: "Get or set the agent accent color (omit value to read, empty to clear)",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "value", Type: registry.String, Help: "hex color #rrggbb (omit to read, empty to clear)"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/color"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			if _, present := p["value"]; present {
				color := str(p, "value")
				if color != "" && !isValidHex(color) {
					return nil, api.UserError{Code: "bad_value", Msg: "value must be a hex color like #4f8cff"}
				}
				a.Color = color
				if err := agentStore(c).SetColor(a.Name, a.Color); err != nil {
					return nil, err
				}
			}
			return map[string]any{"name": a.Name, "color": a.Color}, nil
		},
	}
}

// agentColorGet is the GET half of the color route (same path, GET method).
func agentColorGet() registry.Command {
	return registry.Command{
		Path:    "agent.color.get",
		Summary: "Read the agent accent color",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "agent name"}},
		HTTP:    &registry.HTTPRoute{Method: "GET", Path: "/api/agents/{name}/color"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"name": a.Name, "color": a.Color}, nil
		},
	}
}

// agentInteractive gets or sets interactive (tmux TUI) mode. This changes the
// harness launch mode, so it only takes effect on the next (re)launch — callers
// restart the agent to apply it.
func agentInteractive() registry.Command {
	return registry.Command{
		Path:    "agent.interactive",
		Summary: "Get or set interactive mode (bool); restart the agent to apply",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "value", Type: registry.Bool, Help: "true/false (omit to read)"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/interactive"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			if v, ok := p["value"]; ok {
				b, ok := v.(bool)
				if !ok {
					return nil, api.UserError{Code: "bad_value", Msg: "value must be a boolean"}
				}
				a.Interactive = b
				if err := agentStore(c).Update(a); err != nil {
					return nil, err
				}
			}
			return map[string]any{"name": a.Name, "interactive": a.Interactive}, nil
		},
	}
}

// validHarnessTypes are the harness types internal/harness.Get() knows about.
var validHarnessTypes = map[string]bool{
	"claude":   true,
	"codex":    true,
	"opencode": true,
	"cursor":   true,
	"stub":     true,
}

// agentHarness gets or sets the agent's harness type. Omitting `value` reads it.
// A non-empty value is validated against the known harness types and persisted;
// like interactive it changes the launch mode, so callers restart the agent to
// apply it. This handler does not restart the agent.
func agentHarness() registry.Command {
	return registry.Command{
		Path:    "agent.harness",
		Summary: "Get or set the agent harness type (omit value to read); restart the agent to apply",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "value", Type: registry.String, Help: "new harness type claude|codex|opencode|cursor|stub (omit to read)"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/harness"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			if v := str(p, "value"); v != "" {
				if !validHarnessTypes[v] {
					return nil, api.UserError{Code: "bad_value", Msg: "value must be one of claude|codex|opencode|cursor|stub"}
				}
				a.HarnessType = v
				if err := agentStore(c).Update(a); err != nil {
					return nil, err
				}
			}
			return map[string]any{"name": a.Name, "harness": a.HarnessType}, nil
		},
	}
}
