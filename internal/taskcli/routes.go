package taskcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/alekzonder/tariboy/internal/cli"
	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/commands"
	"github.com/alekzonder/tariboy/internal/paths"
	"github.com/alekzonder/tariboy/internal/registry"
)

func runOperator(ctx context.Context, parsed request, caller Caller, jsonOut bool, stdout, stderr io.Writer) int {
	method, route := "", ""
	body := parsed.payload
	key, _ := body["key"].(string)
	switch parsed.action {
	case "mine":
		method, route = "GET", "/api/tasks"
	case "ready":
		if claim, _ := body["claim"].(bool); claim {
			fmt.Fprintln(stderr, "tasks ready --claim requires agent mode")
			return 2
		}
		return runReady(parsed, caller, jsonOut, stdout, stderr)
	case "show":
		method, route, body = "GET", "/api/tasks/"+key, nil
	case "create":
		method, route = "POST", "/api/tasks"
	case "update", "assign":
		if _, ok := body["revision"]; !ok {
			revision, code := revisionFor(caller, key, stderr)
			if code != 0 {
				return code
			}
			body["revision"] = revision
		}
		method, route = "PATCH", "/api/tasks/"+key
		delete(body, "key")
	case "comment":
		method, route = "POST", "/api/tasks/"+key+"/comments"
		delete(body, "key")
	case "ask":
		method, route = "POST", "/api/tasks/"+key+"/comments"
		principal := strings.TrimSpace(fmt.Sprint(body["principal"]))
		if !strings.Contains(principal, ":") {
			principal = "agent:" + principal
		}
		body = map[string]any{"body": "@" + principal + "\n\n" + fmt.Sprint(body["body"]), "idempotency_key": body["idempotency_key"]}
	case "move":
		if _, ok := body["revision"]; !ok {
			revision, code := revisionFor(caller, key, stderr)
			if code != 0 {
				return code
			}
			body["revision"] = revision
		}
		method, route = "POST", "/api/tasks/"+key+"/move"
		delete(body, "key")
	case "block":
		blocker, _ := body["blocker_key"].(string)
		if _, ok := body["revision"]; !ok {
			revision, code := revisionFor(caller, blocker, stderr)
			if code != 0 {
				return code
			}
			body["revision"] = revision
		}
		method, route = "POST", "/api/tasks/"+blocker+"/relations"
		body["target_key"], body["type"] = key, "blocks"
		delete(body, "key")
		delete(body, "blocker_key")
	case "relate":
		if _, ok := body["revision"]; !ok {
			revision, code := revisionFor(caller, key, stderr)
			if code != 0 {
				return code
			}
			body["revision"] = revision
		}
		body["type"] = "related"
		method, route = "POST", "/api/tasks/"+key+"/relations"
		delete(body, "key")
	case "done":
		if _, ok := body["revision"]; !ok {
			revision, code := revisionFor(caller, key, stderr)
			if code != 0 {
				return code
			}
			body["revision"] = revision
		}
		method, route = "POST", "/api/tasks/"+key+"/complete"
		delete(body, "key")
	case "advance":
		method, route = "POST", "/api/tasks/"+key+"/advance"
		delete(body, "key")
	case "artifact_set":
		name, _ := body["name"].(string)
		method, route = "PUT", "/api/tasks/"+key+"/artifacts/"+name
		body = map[string]any{"value": body["value"]}
	case "artifact_ls":
		method, route, body = "GET", "/api/tasks/"+key+"/artifacts", nil
	case "artifact_show":
		name, _ := body["name"].(string)
		method, route, body = "GET", "/api/tasks/"+key+"/artifacts/"+name, nil
	case "workflow_get":
		method, route, body = "GET", "/api/tasks/"+key+"/workflow", nil
	case "workflow_move":
		method, route = "POST", "/api/tasks/"+key+"/workflow/move"
		delete(body, "key")
	case "cancel":
		method, route = "POST", "/api/tasks/"+key+"/cancel"
		delete(body, "key")
	default:
		fmt.Fprintf(stderr, "tasks: unsupported operator command %s\n", parsed.action)
		return 2
	}
	var requestBody any = body
	if method == "GET" {
		requestBody = query(body)
	}
	raw, err := caller.Call(method, route, requestBody)
	if err != nil {
		return operatorErrorFor(err, key, stderr)
	}
	return printResult(parsed, raw, jsonOut, stdout, stderr)
}

func query(body map[string]any) map[string]string {
	out := map[string]string{}
	for key, value := range body {
		out[key] = fmt.Sprint(value)
	}
	return out
}

func revisionFor(caller Caller, key string, stderr io.Writer) (int64, int) {
	raw, err := caller.Call("GET", "/api/tasks/"+key, nil)
	if err != nil {
		return 0, operatorError(err, stderr)
	}
	var value struct {
		Task struct {
			Revision int64 `json:"revision"`
		} `json:"task"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		fmt.Fprintln(stderr, err)
		return 0, 1
	}
	return value.Task.Revision, 0
}

func runReady(parsed request, caller Caller, jsonOut bool, stdout, stderr io.Writer) int {
	limit := 50
	if text, _ := parsed.payload["limit"].(string); text != "" {
		if n, err := strconv.Atoi(text); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	readyQuery := map[string]string{"status": "open", "blocked": "false", "limit": "500"}
	if queue, _ := parsed.payload["queue"].(string); queue != "" {
		readyQuery["queue"] = queue
	}
	ready := []json.RawMessage{}
	for {
		raw, err := caller.Call("GET", "/api/tasks", readyQuery)
		if err != nil {
			return operatorError(err, stderr)
		}
		var page struct {
			Tasks      []json.RawMessage `json:"tasks"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		for _, rawTask := range page.Tasks {
			var task struct {
				Category          string `json:"category"`
				WorkflowDigest    string `json:"workflow_digest"`
				Assignee          string `json:"assignee"`
				ManualBlockReason string `json:"manual_block_reason"`
				Blocked           bool   `json:"blocked"`
			}
			if err := json.Unmarshal(rawTask, &task); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			// "status" is the workflow status for a workflow task; the category is
			// the lifecycle state. The daemon assigns workflow work, so a task with a
			// workflow is never ready to claim.
			if task.Category == "open" && task.WorkflowDigest == "" && task.Assignee == "" && task.ManualBlockReason == "" && !task.Blocked {
				ready = append(ready, rawTask)
				if len(ready) == limit {
					result, _ := json.Marshal(ready)
					return printResult(parsed, result, jsonOut, stdout, stderr)
				}
			}
		}
		if page.NextCursor == "" {
			break
		}
		readyQuery["after"] = page.NextCursor
	}
	result, _ := json.Marshal(ready)
	return printResult(parsed, result, jsonOut, stdout, stderr)
}

func firstTask(raw json.RawMessage) (string, int64, error) {
	var value struct {
		Tasks []struct {
			Key      string `json:"key"`
			Revision int64  `json:"revision"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", 0, err
	}
	if len(value.Tasks) == 0 {
		return "", 0, fmt.Errorf("tasks ready: no eligible task to claim")
	}
	return value.Tasks[0].Key, value.Tasks[0].Revision, nil
}

func operatorError(err error, stderr io.Writer) int { return operatorErrorFor(err, "", stderr) }

// operatorErrorFor is operatorError for a command on task key, so a
// workflow_managed refusal can name the exact advance command.
func operatorErrorFor(err error, key string, stderr io.Writer) int {
	if client.IsDaemonDown(err) {
		fmt.Fprintln(stderr, "tariboyd is not running (start it with: tariboyd)")
		return 2
	}
	fmt.Fprintf(stderr, "error: %v\n", err)
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		printWorkflowManagedHint(apiErr, key, stderr)
	}
	return 1
}

func isOperatorCommand(args []string) bool {
	return len(args) > 0 && operatorRoots[args[0]]
}

var operatorRoots = map[string]bool{"queue": true, "events": true, "principals": true, "notifications": true}

func runOperatorCommand(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	resolved, err := paths.Resolve(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "tariboy-tasks: %v\n", err)
		return 2
	}
	reg, err := taskOperatorRegistry()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return cli.Run(ctx, reg, args, newCaller(resolved.Socket()), nil, stdout, stderr)
}

func runHelpJSON(ctx context.Context, stdout, stderr io.Writer) int {
	reg, err := taskOperatorRegistry()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_ = ctx
	tree := reg.Tree()
	entries := taskHelpEntries(reg)
	for _, command := range reg.Commands() {
		node := tree
		for _, part := range strings.Split(command.Path, ".") {
			node = node[part].(map[string]any)
		}
		entry := entries[command.Path]
		node["summary"], node["help"], node["usage"], node["examples"] = entry.summary, entry.help, entry.usage, strings.Split(entry.examples, "\n")
	}
	for action, flags := range taskCommandFlags() {
		path := sharedHelpPath(action)
		if len(path) == 0 {
			continue
		}
		insertSharedHelp(tree, path, flags, sharedHelp[action])
	}
	encoded, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, string(encoded))
	return 0
}

func sharedHelpPath(action string) []string {
	for group, words := range commandWords {
		for word, name := range words {
			if name == action {
				return []string{group, word}
			}
		}
	}
	return []string{action}
}

func insertSharedHelp(tree map[string]any, path []string, flags map[string]bool, help commandHelp) {
	node := tree
	for i, segment := range path[:len(path)-1] {
		child, ok := node[segment].(map[string]any)
		if !ok {
			child = map[string]any{"summary": helpGroups[strings.Join(path[:i+1], ".")]}
			node[segment] = child
		}
		node = child
	}
	values := make([]string, 0, len(flags))
	descriptions := map[string]string{}
	for flag := range flags {
		values = append(values, flag)
		descriptions[flag] = helpFlags[flag]
	}
	sort.Strings(values)
	node[path[len(path)-1]] = map[string]any{
		"summary": help.summary, "flags": values, "flag_help": descriptions,
		"help": help.help, "usage": help.usage, "arguments": help.arguments, "examples": strings.Split(help.examples, "\n"),
	}
}

func taskOperatorRegistry() (*registry.Registry, error) {
	reg := registry.New()
	groups := map[string]bool{}
	for _, command := range commands.TaskOperatorCommands() {
		command.Path = strings.TrimPrefix(command.Path, "tasks.")
		if !operatorRoots[strings.Split(command.Path, ".")[0]] {
			continue
		}
		command.CLIHidden = false
		if err := reg.Register(command); err != nil {
			return nil, err
		}
		parts := strings.Split(command.Path, ".")
		for i := 1; i < len(parts); i++ {
			groups[strings.Join(parts[:i], ".")] = true
		}
	}
	for group := range groups {
		if err := reg.RegisterGroup(group, helpGroups[group]); err != nil {
			return nil, err
		}
	}
	return reg, nil
}
