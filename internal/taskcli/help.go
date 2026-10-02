package taskcli

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/alekzonder/tariboy/internal/registry"
)

type commandHelp struct {
	summary, usage, help, arguments, examples string
}

var sharedHelp = map[string]commandHelp{
	"mine": {"List tasks visible to the current actor", "mine [flags]",
		"Agent mode lists accessible work; operator mode lists host tasks. Filters narrow the result; --waiting-for selects a principal whose answer is pending.", "", "ttasks mine --queue DEV --status in_progress\nttasks mine --waiting-for agent:worker"},
	"ready": {"List or claim eligible flexible tasks", "ready [flags]",
		"Ready tasks are open, unassigned, and unblocked. Agent mode may atomically claim work with --claim. Operator mode only lists it; its default limit is 50 (maximum 200).", "", "ttasks ready --queue DEV\nttasks ready --claim --idempotency-key claim-1"},
	"show": {"Inspect a task and its conversation", "show KEY",
		"Read task fields, comments, pending answers, relations, and descendant counts, subject to task visibility.", "KEY  Task key, for example DEV-12 (required)", "ttasks show DEV-12"},
	"create": {"Create a root task or child task", "create --title TEXT (--queue QUEUE | --parent KEY) [flags]",
		"Use --queue for a root or --parent for a child. In an agent's non-owned queue, omitting --assignee files an unassigned task: the filed response means it is recorded but no longer visible. An explicit assignee retains task ownership without exposing the queue; --group is forbidden there.", "", "ttasks create --queue DEV --title 'Investigate failure' --assignee worker --priority P1\nttasks create --parent DEV-12 --title 'Add regression test'"},
	"update": {"Change task fields", "update KEY [flags]",
		"Only supplied fields are updated. Empty --pull-request or --manual-block-reason clears that field. Statuses are open, in_progress, wait_customer, done, and cancelled.", "KEY  Task key (required)", "ttasks update DEV-12 --priority P0\nttasks update DEV-12 --pull-request=''"},
	"assign": {"Assign a flexible task to an agent", "assign KEY ASSIGNEE [flags]",
		"Changes task ownership without granting access to the rest of its queue.", "KEY  Task key (required)\nASSIGNEE  Existing agent name (required)", "ttasks assign DEV-12 worker"},
	"comment": {"Add a durable Markdown task comment", "comment KEY TEXT... [flags]\n       ttasks comment KEY --body TEXT [flags]",
		"Pass comment text positionally or with --body, never both. Typed @agent:name and @user:login mentions request an answer; the mentioned principal's next comment resolves its own pending wait.", "KEY  Task key (required)\nTEXT  Markdown comment text (required unless --body is supplied)", "ttasks comment DEV-12 'Found the boundary'\nttasks comment DEV-12 --body 'Tests passed'"},
	"ask": {"Request a durable answer on a task", "ask KEY PRINCIPAL TEXT... [--idempotency-key KEY]",
		"Use a task key and user:login or agent:name (a bare name means agent:name). It creates a principal wait; asking the customer moves assigned work to wait_customer.",
		"KEY  Task key (required)\nPRINCIPAL  user:login or agent:name to answer (required)\nTEXT  Question text (required)",
		"ttasks ask DEV-12 user:login 'Which behavior should win?'"},
	"move": {"Reparent, reorder, or detach a task", "move KEY (--parent KEY | --before KEY | --to-root) [flags]",
		"Moves within the same queue. --before reorders within a priority bucket. --to-root detaches and cannot be combined with --parent or --before; detaching may remove the agent's inherited access.", "KEY  Task to move (required)", "ttasks move DEV-12 --parent DEV-3\nttasks move DEV-12 --to-root"},
	"block": {"Make one task depend on another", "block KEY --by BLOCKER [flags]",
		"Creates a directional blocks relation: BLOCKER must finish before KEY is ready. Cycles are rejected; write access to both tasks is required. --revision refers to the blocker task.", "KEY  Task being blocked (required)", "ttasks block DEV-12 --by DEV-2"},
	"relate": {"Link two related tasks", "relate KEY TARGET [flags]",
		"Adds a symmetric related relation without blocking either task. Requires write access to both endpoints.", "KEY  Source task key (required)\nTARGET  Related task key (required)", "ttasks relate DEV-12 DEV-9"},
	"done": {"Complete a flexible task", "done KEY [flags]",
		"Marks work done. Active descendants cause a conflict unless --complete-anyway is explicit.", "KEY  Task key (required)", "ttasks done DEV-12"},
}

var helpGroups = map[string]string{
	"queue":         "Manage task queues, owners, and agent pools (operator-only)",
	"queue.pool":    "Bind existing agents to named queue pools (operator-only)",
	"queue.trigger": "Manage external events that create tasks (operator-only)",
	"notifications": "Read and dismiss customer task notifications (operator-only)",
}

var helpFlags = map[string]string{
	"queue":               "Queue prefix, for example DEV",
	"status":              "Task status: open, in_progress, wait_customer, done, or cancelled",
	"assignee":            "Agent name to assign or filter by",
	"text":                "Search text in tasks",
	"waiting-for":         "Filter pending answers by user:login or agent:name",
	"limit":               "Maximum number of results (operator ready: default 50, maximum 200)",
	"claim":               "Atomically claim eligible flexible work (agent-only; boolean, default false)",
	"parent":              "Parent task key; create inherits its queue",
	"title":               "Task title (required for create)",
	"description":         "Task description as Markdown",
	"pull-request":        "Absolute http(s) pull request URL; an empty update clears it",
	"group":               "Task group; forbidden when filing into a queue the agent does not own",
	"priority":            "P0 Critical, P1 High, P2 Normal (create default), P3 Low",
	"manual-block-reason": "Manual blocker text; an empty update clears it",
	"revision":            "Expected task revision (block: blocker revision); operator mode fetches it if omitted",
	"body":                "Markdown comment text, instead of positional TEXT",
	"idempotency-key":     "Stable key reused for retries of the same intent",
	"before":              "Sibling task key to insert before, in the same priority bucket",
	"to-root":             "Detach from parent; excludes --parent and --before (boolean, default false)",
	"by":                  "Blocking task key (required)",
	"complete-anyway":     "Complete despite active descendants (boolean, default false)",
}

const globalHelp = `Global flags:
  --help, -h   Show local help for a command or group; never execute it
  --help-json  Print the complete command tree as JSON (root only)
  --json       Print command results as JSON
  --version    Print the client build version (root only)`

// Help is resolved before parsing or socket selection in both modes.
func runTextHelp(args []string, stdout, stderr io.Writer) (int, bool) {
	explicit := len(args) == 0 || slices.Contains(args, "--help") || slices.Contains(args, "-h")
	if len(args) > 0 && args[0] == "help" {
		explicit, args = true, args[1:]
	}
	path := strings.Join(args, ".")
	if !explicit {
		if _, group := helpGroups[path]; !group {
			return 0, false
		}
	}
	reg, err := taskOperatorRegistry()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1, true
	}
	entries := taskHelpEntries(reg)
	for n := 0; n <= len(args); n++ {
		path = strings.Join(args[:n], ".")
		if entry, ok := entries[path]; ok {
			printTaskCommandHelp(entry, stdout)
			return 0, true
		}
		if n == len(args) || args[n] == "--help" || args[n] == "-h" || args[n] == "help" {
			if path == "" || helpGroups[path] != "" {
				printTaskGroupHelp(path, entries, stdout)
				return 0, true
			}
			break
		}
	}
	fmt.Fprintf(stderr, "tasks: unknown help command %q\n", strings.Join(args, " "))
	return 2, true
}

func taskHelpEntries(reg *registry.Registry) map[string]commandHelp {
	entries := map[string]commandHelp{}
	for action, entry := range sharedHelp {
		var names []string
		for name := range taskCommandFlags()[action] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			value := " VALUE"
			if boolFlags[name] {
				value = ""
			}
			entry.arguments += fmt.Sprintf("\n--%s%s  %s", name, value, helpFlags[name])
		}
		entries[strings.Join(sharedHelpPath(action), ".")] = entry
	}
	for _, command := range reg.Commands() {
		entry := commandHelp{summary: command.Summary + " (operator-only)", usage: strings.ReplaceAll(command.Path, ".", " "), help: command.Help}
		entry.help += operatorHelp[command.Path] + "\nOperator-only: uses the host daemon as the customer actor. Agent mode cannot execute this command. Arguments may be given with their named flags; required values are shown below."
		for _, arg := range command.Args {
			name := arg.Flag
			if name == "" {
				name = arg.Name
			}
			value, required := " VALUE", ""
			if arg.Type == registry.Bool {
				value = ""
			}
			if arg.Required {
				entry.usage += " --" + name + value
				required = " (required)"
			}
			entry.arguments += fmt.Sprintf("\n--%s%s  %s%s", name, value, arg.Help, required)
		}
		entry.usage += " [flags]"
		entry.examples = "ttasks " + operatorExamples[command.Path]
		entries[command.Path] = entry
	}
	return entries
}

func printTaskCommandHelp(entry commandHelp, out io.Writer) {
	fmt.Fprintf(out, "Usage: ttasks %s\n\n%s\n\n%s\n", entry.usage, entry.summary, strings.TrimSpace(entry.help))
	if strings.TrimSpace(entry.arguments) != "" {
		fmt.Fprintf(out, "\nArguments and flags:\n%s\n", strings.TrimSpace(entry.arguments))
	}
	fmt.Fprintf(out, "\nExamples:\n%s\n\n%s\n", entry.examples, globalHelp)
}

func printTaskGroupHelp(path string, entries map[string]commandHelp, out io.Writer) {
	display := strings.ReplaceAll(path, ".", " ")
	if path == "" {
		fmt.Fprintln(out, "Usage: ttasks <command> [args] [flags]\n\nNative Tasks client (tariboy-tasks; ttasks is its alias).\nWith non-empty TARIBOY_TOOLS_SOCKET: identity-bound agent mode, no operator fallback.\nOtherwise: operator mode on the host Unix daemon socket.\nHelp is local and lists both modes; execution still enforces their permissions.")
	} else {
		fmt.Fprintf(out, "Usage: ttasks %s <command> [args] [flags]\n\n%s\n", display, helpGroups[path])
	}
	children := map[string]string{}
	prefix := path
	if prefix != "" {
		prefix += "."
	}
	add := func(key, summary string) {
		if !strings.HasPrefix(key, prefix) {
			return
		}
		name := strings.TrimPrefix(key, prefix)
		if name != "" && !strings.Contains(name, ".") {
			children[name] = summary
		}
	}
	for key, summary := range helpGroups {
		add(key, summary)
	}
	for key, entry := range entries {
		add(key, entry.summary)
	}
	names := make([]string, 0, len(children))
	for name := range children {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintln(out, "\nCommands:")
	for _, name := range names {
		fmt.Fprintf(out, "  %-18s %s\n", name, children[name])
	}
	fmt.Fprintf(out, "\nExamples:\n  ttasks %s --help\n\n%s\n", strings.TrimSpace(display+" "+names[0]), globalHelp)
}

var operatorExamples = map[string]string{
	"queue.list":            "queue list",
	"queue.create":          "queue create --prefix DEV --name Development --owners worker",
	"queue.get":             "queue get DEV",
	"queue.update":          "queue update DEV --name Development --revision 2",
	"queue.pool.set":        "queue pool set DEV developers --agents worker,reviewer --revision 0 --idempotency-key pool-1",
	"queue.pool.list":       "queue pool list DEV",
	"queue.pool.get":        "queue pool get DEV developers",
	"queue.trigger.list":    "queue trigger list DEV",
	"queue.trigger.create":  "queue trigger create DEV --pattern external:incidents --action create_task",
	"queue.trigger.delete":  "queue trigger delete DEV 4",
	"events":                "events DEV-12 --after 42 --limit 20",
	"principals":            "principals",
	"notifications.list":    "notifications list --include-dismissed",
	"notifications.read":    "notifications read 1",
	"notifications.dismiss": "notifications dismiss 1",
}

var operatorHelp = map[string]string{
	"queue.list":            "Lists host queues, including their names, owners, and responsible agents.",
	"queue.create":          "Creates a queue namespace for keys such as DEV-1. Owners are comma-separated existing agent names; the responsible agent handles triage.",
	"queue.get":             "Reads the queue's configuration and current revision before an update.",
	"queue.update":          "Changes only supplied queue fields. Pass the current revision from queue get; stale writes are rejected.",
	"queue.pool.set":        "Replaces pool membership with comma-separated existing agent names. Use revision 0 for a new pool.",
	"queue.pool.list":       "Lists the queue's pools and explicit memberships.",
	"queue.pool.get":        "Reads a pool's membership and current revision before rebinding it.",
	"queue.trigger.list":    "Lists external channel triggers configured to create work in this queue.",
	"queue.trigger.create":  "Creates a trigger for future plugin-produced events. --action must be create_task; internal agent/group/user/system namespaces are rejected. --correlation-key restricts matching to an exact key.",
	"queue.trigger.delete":  "Removes a trigger by numeric id. Existing tasks created by that trigger remain.",
	"events":                "Reads durable task events after the supplied sequence, bounded by --limit. Use the last sequence to resume.",
	"principals":            "Lists principals available for task assignment and typed user:login or agent:name mentions.",
	"notifications.list":    "Lists customer task notifications. Dismissed entries are excluded unless --include-dismissed is supplied.",
	"notifications.read":    "Marks one customer notification read without changing the task or answering its question.",
	"notifications.dismiss": "Dismisses one customer notification; it remains accessible with notifications list --include-dismissed.",
}
