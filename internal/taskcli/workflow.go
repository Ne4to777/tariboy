package taskcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/tasks"
)

// previewRunes caps the one-line artifact preview of "workflow get".
const previewRunes = 120

// isTerminal reports whether r is an interactive terminal; a read from one
// would block until the user types. Tests replace it.
var isTerminal = func(r io.Reader) bool {
	file, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// resolveArtifactValue fills the value of "artifacts set" from --file or from
// stdin when no VALUE argument was given. The value is stored as read, trailing
// newline included. At most maxArtifactInput bytes are read, so an oversize
// value is refused by the daemon rather than cut short here.
func resolveArtifactValue(parsed *request, stderr io.Writer) int {
	if parsed.action != "artifact_set" {
		return 0
	}
	if _, given := parsed.payload["value"]; given {
		return 0
	}
	var source io.Reader = stdin
	name := "stdin"
	path, _ := parsed.payload["file"].(string)
	if path == "" && isTerminal(stdin) {
		fmt.Fprintln(stderr, "tasks artifacts set: pass VALUE, --file PATH, or pipe the value on stdin")
		return 2
	}
	if path != "" {
		name = path
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(stderr, "tasks artifacts set: %v\n", err)
			return 2
		}
		defer file.Close()
		source = file
	}
	delete(parsed.payload, "file")
	value, ok := readText(source, name, "tasks artifacts set", "artifacts", stderr)
	if !ok {
		return 2
	}
	parsed.payload["value"] = value
	return 0
}

// readText reads at most maxArtifactInput bytes of UTF-8 text from source, so
// an oversize value is refused by the daemon rather than cut short here. It
// prints a usage error naming command and reports false when the read fails or
// the text is not UTF-8. noun names what holds only text.
func readText(source io.Reader, name, command, noun string, stderr io.Writer) (string, bool) {
	raw, err := io.ReadAll(io.LimitReader(source, maxArtifactInput))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return "", false
	}
	if !utf8.Valid(raw) {
		fmt.Fprintf(stderr, "%s: %s is not valid UTF-8 text; %s hold text only\n", command, name, noun)
		return "", false
	}
	return string(raw), true
}

// secretSetPositionals is "queue secret set QUEUE KEY".
const secretSetPositionals = 2

// resolveQueueSecretValue completes "queue secret set QUEUE KEY" when no
// --value is given by reading the value from stdin and adding it to args, which
// stay in this process. One trailing newline is stripped, because a token piped
// in almost always carries one. A value is never accepted as a positional
// argument, so it cannot land in a shell history by accident. Other commands
// pass through unchanged.
func resolveQueueSecretValue(args []string, stderr io.Writer) ([]string, int) {
	if len(args) < 3 || args[0] != "queue" || args[1] != "secret" || args[2] != "set" {
		return args, 0
	}
	positionals := 0
	for i := 3; i < len(args); i++ {
		switch {
		case args[i] == "--value" || strings.HasPrefix(args[i], "--value="):
			return args, 0
		case args[i] == "--json":
		case strings.HasPrefix(args[i], "-"):
		default:
			positionals++
		}
	}
	if positionals > secretSetPositionals {
		fmt.Fprintln(stderr, "tasks queue secret set: pass the value with --value or on stdin, not as an argument")
		return nil, 2
	}
	if positionals < secretSetPositionals {
		return args, 0 // the command reports the missing QUEUE or KEY
	}
	if isTerminal(stdin) {
		fmt.Fprintln(stderr, "tasks queue secret set: pass --value VALUE or pipe the value on stdin")
		return nil, 2
	}
	value, ok := readText(stdin, "stdin", "tasks queue secret set", "secrets", stderr)
	if !ok {
		return nil, 2
	}
	if strings.HasSuffix(value, "\r\n") {
		value = strings.TrimSuffix(value, "\r\n")
	} else {
		value = strings.TrimSuffix(value, "\n")
	}
	if value == "" {
		fmt.Fprintln(stderr, "tasks queue secret set: the value is empty; pass --value VALUE or pipe it on stdin")
		return nil, 2
	}
	return append(append([]string{}, args...), "--value", value), 0
}

// printWorkflowManagedHint explains a workflow_managed refusal: the workflow
// status, the outcomes it offers, and the exact advance command.
func printWorkflowManagedHint(apiErr *client.APIError, key string, stderr io.Writer) {
	if apiErr.Code != "workflow_managed" {
		return
	}
	if key == "" {
		key = "KEY"
	}
	var outcomes []string
	if list, ok := apiErr.Details["outcomes"].([]any); ok {
		for _, item := range list {
			if name, ok := item.(string); ok {
				outcomes = append(outcomes, fmt.Sprintf("%q", name))
			}
		}
	}
	status, _ := apiErr.Details["status"].(string)
	fmt.Fprintf(stderr, "hint: %s is in workflow status %q; its workflow decides what happens next.\n", key, status)
	if len(outcomes) > 0 {
		fmt.Fprintf(stderr, "hint: available outcomes: %s\n", strings.Join(outcomes, ", "))
	} else {
		fmt.Fprintln(stderr, "hint: this status offers no outcomes")
	}
	fmt.Fprintf(stderr, "hint: %s\n", advanceCommand(key, status))
}

// advanceCommand is the advance command line for key in status, with --from so
// a retry cannot apply twice. A status that is not a plain identifier is left
// out rather than printed to the terminal.
func advanceCommand(key, status string) string {
	if status == "" || strings.IndexFunc(status, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) >= 0 {
		return "ttasks advance " + key + " --outcome <name>"
	}
	return "ttasks advance " + key + " --from " + status + " --outcome <name>"
}

// printShowHeader prints the status, category, and waiting_on of the shown task
// ahead of the generic dump, and points a workflow task at "workflow get".
func printShowHeader(raw json.RawMessage, stdout io.Writer) {
	var detail struct {
		Task map[string]any `json:"task"`
	}
	if json.Unmarshal(raw, &detail) != nil || detail.Task == nil {
		return
	}
	line := func(name string) {
		value, _ := detail.Task[name].(string)
		fmt.Fprintln(stdout, strings.TrimRight(name+": "+value, " "))
	}
	line("status")
	line("category")
	line("waiting_on")
	if digest, _ := detail.Task["workflow_digest"].(string); digest != "" {
		key, _ := detail.Task["key"].(string)
		fmt.Fprintf(stdout, "workflow: see ttasks workflow get %s\n", key)
	}
}

// printWorkflow renders the WorkflowView of task key as text. It reports false
// when raw is not one, so the caller falls back to the generic dump.
func printWorkflow(raw json.RawMessage, key string, stdout io.Writer) bool {
	var view tasks.WorkflowView
	if json.Unmarshal(raw, &view) != nil || view.Name == "" {
		return false
	}
	field := func(name, value string) { fmt.Fprintln(stdout, strings.TrimRight(name+": "+value, " ")) }
	field("workflow", view.Name+"@"+view.Version)
	field("status", view.Status)
	field("category", view.Category)
	field("waiting_on", view.WaitingOn)
	if view.PausedReason != "" {
		field("paused", view.PausedReason)
	}
	field("owner", view.Owner)
	field("holder", view.Holder)
	fmt.Fprintln(stdout, "outcomes:")
	for _, outcome := range view.Outcomes {
		line := fmt.Sprintf("  %s -> %s", outcome.On, outcome.To)
		if len(outcome.Requires) > 0 {
			line += "  requires: " + strings.Join(outcome.Requires, ", ")
		}
		if len(outcome.Missing) > 0 {
			line += "  missing: " + strings.Join(outcome.Missing, ", ")
		}
		if len(outcome.Checks) > 0 {
			line += "  checks: " + strings.Join(outcome.Checks, ", ")
		}
		fmt.Fprintln(stdout, line)
	}
	if len(view.Outcomes) > 0 && key != "" {
		field("next", advanceCommand(key, view.Status))
	}
	fmt.Fprintln(stdout, "artifacts:")
	artifacts := append([]tasks.Artifact(nil), view.Artifacts...)
	sort.SliceStable(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	for _, artifact := range artifacts {
		fmt.Fprintf(stdout, "  %s  %s  %s\n", artifact.Name, artifact.Author, preview(artifact.Value))
	}
	printReachedBy(view, stdout)
	if last := view.LastRequest; last != nil {
		line := fmt.Sprintf("#%d %s %s by %s", last.ID, last.Outcome, last.State, last.Actor)
		if last.ResultMessage != "" {
			line += ": " + last.ResultMessage
		}
		field("last_request", line)
	}
	return true
}

// printReachedBy prints the outcome and message that led to the current status,
// the message whole and fenced as data, as the Goal block cuts it.
func printReachedBy(view tasks.WorkflowView, stdout io.Writer) {
	if len(view.Visits) < 2 {
		return
	}
	prev, current := view.Visits[len(view.Visits)-2], view.Visits[len(view.Visits)-1]
	line := "Reached by: "
	if prev.Outcome != "" {
		line += fmt.Sprintf("outcome %s of %s", terminalSafe(prev.Outcome), terminalSafe(prev.Status))
	} else {
		line += "a move out of " + terminalSafe(prev.Status)
	}
	if current.EnteredBy != "" {
		line += ", entered by " + terminalSafe(current.EnteredBy)
	}
	fmt.Fprintln(stdout, line)
	if prev.Message == "" {
		return
	}
	text := terminalText(prev.Message)
	fence := tasks.FenceFor(text, 3)
	fmt.Fprintf(stdout, "message:\n%s\n%s\n%s\n", fence, strings.TrimRight(text, "\n"), fence)
}

// terminalSafe folds a value onto one line without control characters.
func terminalSafe(value string) string {
	return strings.Join(strings.Fields(terminalText(value)), " ")
}

// terminalText drops control characters other than newline and tab, which
// would act on the operator's terminal.
func terminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.Is(unicode.Cc, r) {
			return -1
		}
		return r
	}, value)
}

// preview folds a value onto one line and caps it at previewRunes runes.
func preview(value string) string {
	// Control characters, ESC included, would act on the operator's terminal.
	cleaned := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cc, r) {
			return ' '
		}
		return r
	}, value)
	folded := strings.Join(strings.Fields(cleaned), " ")
	runes := []rune(folded)
	if len(runes) <= previewRunes {
		return folded
	}
	return string(runes[:previewRunes-1]) + "…"
}
