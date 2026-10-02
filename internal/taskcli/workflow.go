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
	raw, err := io.ReadAll(io.LimitReader(source, maxArtifactInput))
	if err != nil {
		fmt.Fprintf(stderr, "tasks artifacts set: %v\n", err)
		return 2
	}
	if !utf8.Valid(raw) {
		fmt.Fprintf(stderr, "tasks artifacts set: %s is not valid UTF-8 text; artifacts hold text only\n", name)
		return 2
	}
	parsed.payload["value"] = string(raw)
	return 0
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
	fmt.Fprintf(stderr, "hint: ttasks advance %s --outcome <name>\n", key)
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

// printWorkflow renders a WorkflowView as text. It reports false when raw is
// not one, so the caller falls back to the generic dump.
func printWorkflow(raw json.RawMessage, stdout io.Writer) bool {
	var view tasks.WorkflowView
	if json.Unmarshal(raw, &view) != nil || view.Name == "" {
		return false
	}
	field := func(name, value string) { fmt.Fprintln(stdout, strings.TrimRight(name+": "+value, " ")) }
	field("workflow", view.Name+"@"+view.Version)
	field("status", view.Status)
	field("category", view.Category)
	field("waiting_on", view.WaitingOn)
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
	fmt.Fprintln(stdout, "artifacts:")
	artifacts := append([]tasks.Artifact(nil), view.Artifacts...)
	sort.SliceStable(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	for _, artifact := range artifacts {
		fmt.Fprintf(stdout, "  %s  %s  %s\n", artifact.Name, artifact.Author, preview(artifact.Value))
	}
	if last := view.LastRequest; last != nil {
		line := fmt.Sprintf("#%d %s %s by %s", last.ID, last.Outcome, last.State, last.Actor)
		if last.ResultMessage != "" {
			line += ": " + last.ResultMessage
		}
		field("last_request", line)
	}
	return true
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
