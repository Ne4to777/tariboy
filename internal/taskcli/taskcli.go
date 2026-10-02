// Package taskcli provides the shared agent and operator Tasks command client.
package taskcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/paths"
	"github.com/alekzonder/tariboy/internal/version"
)

type Caller interface {
	Call(method, route string, body any) (json.RawMessage, error)
}

var newCaller = func(socket string) Caller { return client.New(socket) }

// stdin is where "artifacts set" reads a value that is not given as an argument.
var stdin io.Reader = os.Stdin

// Run dispatches task commands to the identity-bound agent socket when one is
// present, otherwise to the operator daemon socket.
func Run(ctx context.Context, argv []string, getenv func(string) string, stdout, stderr io.Writer) int {
	jsonOut := false
	args := make([]string, 0, len(argv))
	for _, arg := range argv {
		if arg == "--json" {
			jsonOut = true
		} else {
			args = append(args, arg)
		}
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(stdout, version.Version)
		return 0
	}
	if len(args) == 1 && args[0] == "--help-json" {
		return runHelpJSON(ctx, stdout, stderr)
	}
	if code, handled := runTextHelp(args, stdout, stderr); handled {
		return code
	}
	parsed, err := parse(args)
	if err != nil {
		if isOperatorCommand(args) && getenv("TARIBOY_TOOLS_SOCKET") == "" {
			if jsonOut {
				args = append(args, "--json")
			}
			return runOperatorCommand(ctx, args, getenv, stdout, stderr)
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	if toolsSocket := getenv("TARIBOY_TOOLS_SOCKET"); toolsSocket != "" {
		if operatorOnlyActions[parsed.action] {
			fmt.Fprintf(stderr, "tasks: unknown command %q\n", strings.ReplaceAll(parsed.action, "_", " "))
			return 2
		}
		if code := resolveArtifactValue(&parsed, stderr); code != 0 {
			return code
		}
		return runAgent(parsed, newCaller(toolsSocket), jsonOut, stdout, stderr)
	}
	resolved, err := paths.Resolve(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "tariboy-tasks: %v\n", err)
		return 2
	}
	if code := resolveArtifactValue(&parsed, stderr); code != 0 {
		return code
	}
	return runOperator(ctx, parsed, newCaller(resolved.Socket()), jsonOut, stdout, stderr)
}

func runAgent(parsed request, caller Caller, jsonOut bool, stdout, stderr io.Writer) int {
	raw, err := caller.Call("POST", "/tools/tasks/"+parsed.action, parsed.payload)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			fmt.Fprintf(stderr, "error (%s): %s\n", apiErr.Code, apiErr.Msg)
			key, _ := parsed.payload["key"].(string)
			printWorkflowManagedHint(apiErr, key, stderr)
			return 1
		}
		fmt.Fprintln(stderr, "tools: agent socket is not reachable")
		return 2
	}
	return printResult(parsed, raw, jsonOut, stdout, stderr)
}

func printResult(parsed request, raw json.RawMessage, jsonOut bool, stdout, stderr io.Writer) int {
	if jsonOut {
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	switch parsed.action {
	case "workflow_get":
		if printWorkflow(raw, stdout) {
			return 0
		}
	case "show":
		printShowHeader(raw, stdout)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if object, ok := value.(map[string]any); ok {
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(stdout, "%s: %s\n", key, formatValue(object[key]))
		}
		if parsed.action == "create" && object["filed"] == true {
			fmt.Fprintf(stdout, "\n%v is filed into queue %v: no assignee, and no longer visible to you.\nWhoever runs that queue triages it. It is recorded — do not file it again.\n", object["key"], object["queue"])
		}
		return 0
	}
	if text, ok := value.(string); ok {
		fmt.Fprintln(stdout, text)
		return 0
	}
	fmt.Fprintln(stdout, string(raw))
	return 0
}

func formatValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "<nil>"
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case []any:
		values := make([]string, len(typed))
		for i, v := range typed {
			values[i] = formatValue(v)
		}
		return "[" + strings.Join(values, " ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = key + ":" + formatValue(typed[key])
		}
		return "map[" + strings.Join(parts, " ") + "]"
	default:
		return fmt.Sprint(typed)
	}
}
