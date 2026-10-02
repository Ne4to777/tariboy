package commands

import (
	"errors"

	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/script"
)

func requireScripts(c *registry.Ctx) (registry.ScriptControl, error) {
	if c.Scripts == nil {
		return nil, api.UserError{Code: "scripts_unavailable", Msg: "script controls are not available"}
	}
	return c.Scripts, nil
}

func scriptLs() registry.Command {
	return registry.Command{Path: "script.ls", Summary: "List an agent's local scripts",
		Args: []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "agent name"}},
		HTTP: &registry.HTTPRoute{Method: "GET", Path: "/api/agents/{name}/scripts"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			sc, err := scriptControlForAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			definitions, err := sc.ListScripts(str(p, "name"))
			if err != nil {
				return nil, scriptUserError(err)
			}
			rows := make([]map[string]any, 0, len(definitions))
			for _, definition := range definitions {
				rows = append(rows, scriptDefinitionView(definition))
			}
			return map[string]any{"scripts": rows, "count": len(rows)}, nil
		},
	}
}

func scriptRunOnce() registry.Command {
	return registry.Command{Path: "script.run", Summary: "Run a local command once",
		Args: scriptCreateArgs(false), HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/scripts/run"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			name := str(p, "name")
			sc, err := scriptControlForAgent(c, name)
			if err != nil {
				return nil, err
			}
			definition, run, err := sc.RunOnce(name, script.CreateOnce{Name: str(p, "script_name"), Description: str(p, "description"), Command: str(p, "command")})
			if err != nil {
				return nil, scriptUserError(err)
			}
			return map[string]any{"script": scriptDefinitionView(definition), "run": scriptRunView(run)}, nil
		},
	}
}

func scriptSchedule() registry.Command {
	args := scriptCreateArgs(true)
	return registry.Command{Path: "script.schedule", Summary: "Schedule a fixed-interval local command",
		Args: args, HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/scripts/schedule"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			name := str(p, "name")
			sc, err := scriptControlForAgent(c, name)
			if err != nil {
				return nil, err
			}
			var quietExit *int
			if _, ok := p["quiet_exit"]; ok {
				value := intOf(p, "quiet_exit", 0)
				quietExit = &value
			}
			definition, run, err := sc.ScheduleScript(name, script.CreateSchedule{Name: str(p, "script_name"), Description: str(p, "description"), Command: str(p, "command"), IntervalSeconds: intOf(p, "interval_seconds", 0), QuietExit: quietExit})
			if err != nil {
				return nil, scriptUserError(err)
			}
			return map[string]any{"script": scriptDefinitionView(definition), "run": scriptRunView(run)}, nil
		},
	}
}

func scriptCreateArgs(schedule bool) []registry.Arg {
	args := []registry.Arg{
		{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
		{Name: "script_name", Flag: "script-name", Type: registry.String, Required: true, Help: "script name"},
		{Name: "description", Type: registry.String, Required: true, Help: "description"},
		{Name: "command", Type: registry.String, Required: true, Help: "shell command"},
	}
	if schedule {
		args = append(args,
			registry.Arg{Name: "interval_seconds", Flag: "interval-seconds", Type: registry.Int, Required: true, Help: "delay after each completion"},
			registry.Arg{Name: "quiet_exit", Flag: "quiet-exit", Type: registry.Int, Help: "deprecated: the quiet exit code is always 111; another value wraps the command so that code is reported as 111"})
	}
	return args
}

func scriptRerun() registry.Command {
	return scriptRunAction("script.rerun", "Run a stored script now, resuming a stopped schedule", "POST", "/api/agents/{name}/scripts/{id}/rerun", func(sc registry.ScriptControl, name, id string) (any, error) {
		run, err := sc.RerunScript(name, id)
		return scriptRunView(run), err
	})
}

func scriptRuns() registry.Command {
	return scriptRunAction("script.runs", "List a script's runs", "GET", "/api/agents/{name}/scripts/{id}/runs", func(sc registry.ScriptControl, name, id string) (any, error) {
		runs, err := sc.ListScriptRuns(name, id)
		if err != nil {
			return nil, err
		}
		rows := make([]map[string]any, 0, len(runs))
		for _, run := range runs {
			rows = append(rows, scriptRunView(run))
		}
		return map[string]any{"runs": rows, "count": len(rows)}, nil
	})
}

func scriptRunDetail() registry.Command {
	return scriptRunAction("script.run-get", "Get one script run", "GET", "/api/agents/{name}/script-runs/{id}", func(sc registry.ScriptControl, name, id string) (any, error) {
		run, err := sc.GetScriptRun(name, id)
		return scriptRunView(run), err
	})
}

func scriptLogs() registry.Command {
	return scriptRunAction("script.logs", "Read a bounded script run log tail", "GET", "/api/agents/{name}/script-runs/{id}/logs", func(sc registry.ScriptControl, name, id string) (any, error) {
		run, err := sc.GetScriptRun(name, id)
		if err != nil {
			return nil, err
		}
		logText, err := sc.LogScriptRun(name, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"run": scriptRunView(run), "log": logText}, nil
	})
}

func scriptCancel() registry.Command {
	return scriptAction("script.cancel", "Cancel a script or one run", "POST", "/api/agents/{name}/script-targets/{id}/cancel", "cancelled", func(sc registry.ScriptControl, name, id string) error { return sc.CancelScriptTarget(name, id) })
}

func scriptRemove() registry.Command {
	return scriptAction("script.rm", "Remove an inactive script", "DELETE", "/api/agents/{name}/scripts/{id}", "removed", func(sc registry.ScriptControl, name, id string) error { return sc.RemoveScript(name, id) })
}

func scriptControlForAgent(c *registry.Ctx, name string) (registry.ScriptControl, error) {
	if _, err := getAgent(c, name); err != nil {
		return nil, err
	}
	return requireScripts(c)
}

func scriptRunAction(path, summary, method, route string, action func(registry.ScriptControl, string, string) (any, error)) registry.Command {
	return registry.Command{Path: path, Summary: summary,
		Args: []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "agent name"}, {Name: "id", Type: registry.String, Required: true, Help: "script or run id"}},
		HTTP: &registry.HTTPRoute{Method: method, Path: route},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			sc, err := scriptControlForAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			result, err := action(sc, str(p, "name"), str(p, "id"))
			if err != nil {
				return nil, scriptUserError(err)
			}
			return result, nil
		},
	}
}

func scriptAction(path, summary, method, route, result string, action func(registry.ScriptControl, string, string) error) registry.Command {
	return scriptRunAction(path, summary, method, route, func(sc registry.ScriptControl, name, id string) (any, error) {
		if err := action(sc, name, id); err != nil {
			return nil, err
		}
		return map[string]any{"id": id, result: true}, nil
	})
}

func scriptUserError(err error) error {
	switch {
	case errors.Is(err, script.ErrNotFound):
		return api.UserError{Code: "not_found", Msg: "script or run not found"}
	case errors.Is(err, script.ErrActive):
		return api.UserError{Code: "script_active", Msg: err.Error()}
	case errors.Is(err, script.ErrMode):
		return api.UserError{Code: "script_mode", Msg: err.Error()}
	case errors.Is(err, script.ErrConflict):
		return api.UserError{Code: "script_conflict", Msg: err.Error()}
	default:
		return api.UserError{Code: "invalid_script", Msg: err.Error()}
	}
}

func scriptDefinitionView(definition script.Definition) map[string]any {
	row := map[string]any{"id": definition.ID, "agent": definition.Agent, "name": definition.Name, "description": definition.Description,
		"command": definition.Command, "mode": definition.Mode, "interval_seconds": definition.IntervalSeconds, "state": definition.State,
		"created_at": definition.CreatedAt, "next_run_at": definition.NextRunAt}
	if definition.LatestRun != nil {
		row["latest_run"] = scriptRunView(*definition.LatestRun)
	}
	return row
}

func scriptRunView(run script.Run) map[string]any {
	row := map[string]any{"id": run.ID, "script_id": run.ScriptID, "agent": run.Agent, "status": run.Status,
		"cancel_requested": run.CancelRequested, "created_at": run.CreatedAt, "started_at": run.StartedAt, "finished_at": run.FinishedAt, "log_path": run.LogPath}
	if run.PID != nil {
		row["pid"] = *run.PID
	}
	if run.ExitCode != nil {
		row["exit_code"] = *run.ExitCode
	}
	return row
}
