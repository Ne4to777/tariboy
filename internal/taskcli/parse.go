package taskcli

import (
	"fmt"
	"strings"
)

type request struct {
	action  string
	payload map[string]any
}
type usageError struct{ message string }

func (e usageError) Error() string { return e.message }

var boolFlags = map[string]bool{"claim": true, "to-root": true, "complete-anyway": true}

func taskCommandFlags() map[string]map[string]bool {
	return map[string]map[string]bool{
		"mine": set("queue,status,assignee,text,waiting-for"), "ready": set("queue,limit,idempotency-key,claim"), "show": {},
		"create": set("queue,parent,title,description,pull-request,assignee,group,priority,idempotency-key"),
		"update": set("title,description,status,pull-request,assignee,manual-block-reason,priority,revision"), "assign": set("revision"),
		"comment": set("body,idempotency-key"), "ask": set("idempotency-key"),
		"move": set("parent,before,to-root,revision"), "block": set("by,revision,idempotency-key"), "relate": set("revision,idempotency-key"), "done": set("revision,complete-anyway"),
		"advance": set("outcome,message"), "artifact_set": set("file"), "artifact_ls": {}, "artifact_show": {},
		"workflow_get": {}, "workflow_move": set("to,reason"), "cancel": {},
	}
}

// commandWords maps a two-word command to its action name, which is also the
// agent tools-socket action where one exists.
var commandWords = map[string]map[string]string{
	"artifacts": {"set": "artifact_set", "ls": "artifact_ls", "show": "artifact_show"},
	"workflow":  {"get": "workflow_get", "move": "workflow_move"},
}

// operatorOnlyActions are served only by the host daemon as the customer.
var operatorOnlyActions = map[string]bool{"workflow_move": true, "cancel": true}

// maxArtifactInput is one byte more than the daemon accepts, so an oversize
// value reaches the server and is refused there instead of being cut short.
const maxArtifactInput = 64<<10 + 1

func parse(argv []string) (request, error) {
	if len(argv) == 0 {
		return request{}, usageError{"tasks: a command is required"}
	}
	action, rest := argv[0], argv[1:]
	if words, grouped := commandWords[action]; grouped {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "--") {
			return request{}, usageError{fmt.Sprintf("tasks %s: a command is required", action)}
		}
		sub, ok := words[rest[0]]
		if !ok {
			return request{}, usageError{fmt.Sprintf("tasks: unknown command %q", action+" "+rest[0])}
		}
		action, rest = sub, rest[1:]
	}
	allowed := taskCommandFlags()
	valid, ok := allowed[action]
	if !ok {
		return request{}, usageError{fmt.Sprintf("tasks: unknown command %q", action)}
	}
	flags, pos, err := parseFlags(rest, valid)
	if err != nil {
		return request{}, err
	}
	p := map[string]any{}
	copyFlag := func(name string, present bool) {
		if v, ok := flags[name]; ok && (present || v != "") {
			p[strings.ReplaceAll(name, "-", "_")] = v
		}
	}
	require := func(index int, label string) (string, error) {
		if len(pos) <= index || strings.TrimSpace(pos[index]) == "" {
			return "", usageError{fmt.Sprintf("tasks %s: %s is required", strings.ReplaceAll(action, "_", " "), label)}
		}
		return pos[index], nil
	}
	switch action {
	case "mine":
		for name := range valid {
			copyFlag(name, false)
		}
	case "ready":
		for _, name := range []string{"queue", "limit", "idempotency-key"} {
			copyFlag(name, false)
		}
		if flags["claim"] == "true" {
			p["claim"] = true
		}
	case "show":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		p["key"] = v
	case "create":
		for name := range valid {
			copyFlag(name, false)
		}
		if v, ok := p["parent"].(string); ok {
			delete(p, "parent")
			p["parent_key"] = v
		}
		if _, ok := p["title"]; !ok {
			return request{}, usageError{"tasks create: --title is required"}
		}
		if _, q := p["queue"]; !q {
			if _, parent := p["parent_key"]; !parent {
				return request{}, usageError{"tasks create: --queue or --parent is required"}
			}
		}
	case "update":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		p["key"] = v
		for name := range valid {
			copyFlag(name, name == "manual-block-reason" || name == "pull-request")
		}
	case "assign":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		a, e := require(1, "assignee")
		if e != nil {
			return request{}, e
		}
		p["key"], p["assignee"] = v, a
		copyFlag("revision", false)
	case "comment":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		body := strings.TrimSpace(flags["body"])
		if _, set := flags["body"]; set && len(pos) > 1 {
			return request{}, usageError{fmt.Sprintf("tasks comment: unexpected argument: %s", pos[1])}
		}
		if body == "" {
			body = strings.TrimSpace(strings.Join(pos[1:], " "))
		}
		if body == "" {
			return request{}, usageError{"tasks comment: comment text is required"}
		}
		p["key"], p["body"] = v, body
		copyFlag("idempotency-key", false)
	case "ask":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		principal, e := require(1, "principal")
		if e != nil {
			return request{}, e
		}
		if len(pos) < 3 {
			return request{}, usageError{"tasks ask: principal and question are required"}
		}
		p["key"], p["principal"], p["body"] = v, principal, strings.Join(pos[2:], " ")
		copyFlag("idempotency-key", false)
	case "move":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		p["key"] = v
		if v, ok := flags["parent"]; ok && v != "" {
			p["parent_key"] = v
		}
		if v, ok := flags["before"]; ok && v != "" {
			p["before_key"] = v
		}
		copyFlag("revision", false)
		if flags["to-root"] == "true" {
			if _, parent := p["parent_key"]; parent {
				return request{}, usageError{"tasks move: --to-root cannot be combined with --parent or --before"}
			}
			if _, before := p["before_key"]; before {
				return request{}, usageError{"tasks move: --to-root cannot be combined with --parent or --before"}
			}
			p["parent_key"] = ""
		} else {
			if _, parent := p["parent_key"]; !parent {
				if _, before := p["before_key"]; !before {
					return request{}, usageError{"tasks move: pass --parent, --before, or --to-root to detach"}
				}
			}
		}
	case "block":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		by := flags["by"]
		if by == "" {
			return request{}, usageError{"tasks block: --by is required"}
		}
		p["key"], p["blocker_key"] = v, by
		copyFlag("revision", false)
		copyFlag("idempotency-key", false)
	case "relate":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		target, e := require(1, "related task key")
		if e != nil {
			return request{}, e
		}
		p["key"], p["target_key"] = v, target
		copyFlag("revision", false)
		copyFlag("idempotency-key", false)
	case "done":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		p["key"] = v
		copyFlag("revision", false)
		if flags["complete-anyway"] == "true" {
			p["complete_anyway"] = true
		}
	case "advance":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		p["key"] = v
		if strings.TrimSpace(flags["outcome"]) == "" {
			return request{}, usageError{"tasks advance: --outcome is required"}
		}
		p["outcome"] = strings.TrimSpace(flags["outcome"])
		copyFlag("message", true)
	case "artifact_set":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		name, e := require(1, "artifact name")
		if e != nil {
			return request{}, e
		}
		p["key"], p["name"] = v, name
		if len(pos) > 2 {
			if _, file := flags["file"]; file {
				return request{}, usageError{"tasks artifacts set: pass VALUE or --file, not both"}
			}
			p["value"] = pos[2]
		}
		copyFlag("file", false)
	case "artifact_ls", "workflow_get", "cancel":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		p["key"] = v
	case "artifact_show":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		name, e := require(1, "artifact name")
		if e != nil {
			return request{}, e
		}
		p["key"], p["name"] = v, name
	case "workflow_move":
		v, e := require(0, "task key")
		if e != nil {
			return request{}, e
		}
		p["key"] = v
		for _, name := range []string{"to", "reason"} {
			if strings.TrimSpace(flags[name]) == "" {
				return request{}, usageError{fmt.Sprintf("tasks workflow move: --%s is required", name)}
			}
			p[name] = strings.TrimSpace(flags[name])
		}
	}
	if err := noExtra(action, pos); err != nil {
		return request{}, err
	}
	return request{action, p}, nil
}

func set(csv string) map[string]bool {
	out := map[string]bool{}
	for _, name := range strings.Split(csv, ",") {
		if name != "" {
			out[name] = true
		}
	}
	return out
}
func csv(value string) []string {
	out := []string{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
func parseFlags(args []string, allowed map[string]bool) (map[string]string, []string, error) {
	flags := map[string]string{}
	pos := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			pos = append(pos, arg)
			continue
		}
		name, value, has := strings.Cut(arg[2:], "=")
		if !allowed[name] {
			return nil, nil, usageError{fmt.Sprintf("unknown flag --%s", name)}
		}
		if !has {
			if boolFlags[name] {
				value = "true"
			} else {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
					return nil, nil, usageError{fmt.Sprintf("flag --%s needs a value", name)}
				}
				i++
				value = args[i]
			}
		} else if boolFlags[name] && value != "true" && value != "false" {
			return nil, nil, usageError{fmt.Sprintf("flag --%s must be true or false", name)}
		}
		flags[name] = value
	}
	return flags, pos, nil
}
func noExtra(action string, pos []string) error {
	limits := map[string]int{"mine": 0, "ready": 0, "show": 1, "create": 0, "update": 1, "assign": 2, "comment": -1, "ask": -1, "move": 1, "block": 1, "relate": 2, "done": 1,
		"advance": 1, "artifact_set": 3, "artifact_ls": 1, "artifact_show": 2, "workflow_get": 1, "workflow_move": 1, "cancel": 1}
	if limit, ok := limits[action]; ok && limit >= 0 && len(pos) > limit {
		return usageError{fmt.Sprintf("tasks %s: unexpected argument: %s", strings.ReplaceAll(action, "_", " "), pos[limit])}
	}
	return nil
}
