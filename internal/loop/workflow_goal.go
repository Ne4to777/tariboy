package loop

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// WorkflowGoal is everything the Goal block shows for a workflow task.
type WorkflowGoal struct {
	Task         tasks.Task
	View         tasks.WorkflowView
	Instructions string // content of the status instructions file; "" when none
	// InstructionsUnreadable is set when the status names an instructions file
	// that could not be read.
	InstructionsUnreadable bool
}

// WorkflowGoalSource loads the workflow data of a task for the agent that
// works on it.
type WorkflowGoalSource interface {
	WorkflowGoal(ctx context.Context, key, agent string) (WorkflowGoal, error)
}

const maxGoalInstructionsBytes = 64 << 10
const maxGoalValueRunes = 400 // artifact values and script messages are cut to this

// maxGoalDescriptionRunes bounds the task description, which is the one
// untrusted value an agent needs whole; 400 runes would cut most briefs.
const maxGoalDescriptionRunes = 4000

const goalCutMarker = "… (cut)"

// RuntimeGoal renders the Goal block of task for agent. A workflow task is
// rendered from its workflow data; when that cannot be loaded the flexible
// text is used with a one-line notice, so an iteration still starts.
func RuntimeGoal(ctx context.Context, src WorkflowGoalSource, task tasks.Task, agent string, log *slog.Logger) string {
	if task.WorkflowDigest == "" {
		return FormatRuntimeGoal(task)
	}
	notice := "\n\nThe workflow details of this task could not be loaded. Run `ttasks workflow get " + task.Key + "` to see its status and outcomes."
	if src == nil {
		return FormatRuntimeGoal(task) + notice
	}
	goal, err := src.WorkflowGoal(ctx, task.Key, agent)
	if err != nil {
		if log != nil {
			log.Warn("load workflow goal", "agent", agent, "task", task.Key, "err", err)
		}
		return FormatRuntimeGoal(task) + notice
	}
	// The task row the goal selection returned is the one the iteration runs
	// on; the view only adds to it.
	goal.Task = task
	return FormatRuntimeWorkflowGoal(goal)
}

// FormatRuntimeWorkflowGoal renders the Goal block for a workflow task.
func FormatRuntimeWorkflowGoal(goal WorkflowGoal) string {
	task, view := goal.Task, goal.View
	working := goalWorkable(goal)

	head := []string{
		"# Agent Goal",
		fmt.Sprintf("This task follows a workflow. Do only the work of its current status, `%s`. Leave the status only by declaring an outcome with `ttasks advance`. `ttasks status`, `ttasks done`, and `ttasks claim` do not apply to this task. Never merge or close anything on the customer's behalf unless the status instructions below say so. To ask the customer a question, use `ttasks ask %s user:LOGIN \"QUESTION\"` with the customer's login.",
			view.Status, task.Key),
	}
	lines := []string{
		"key: " + task.Key,
		"title: " + oneLine(task.Title),
		"priority: " + string(task.Priority),
		fmt.Sprintf("workflow: %s@%s", view.Name, view.Version),
		"status: " + view.Status,
		"category: " + view.Category,
	}
	if prev, ok := previousVisit(view); ok {
		reached := "reached by: "
		if prev.Outcome != "" {
			reached += fmt.Sprintf("outcome `%s` of `%s`", prev.Outcome, prev.Status)
		} else {
			reached += fmt.Sprintf("a move out of `%s`", prev.Status)
		}
		if by := view.Visits[len(view.Visits)-1].EnteredBy; by != "" {
			reached += ", entered by " + by
		}
		lines = append(lines, reached)
		if prev.Message != "" {
			lines = append(lines, "transition message:", fenced(prev.Message, maxGoalValueRunes))
		}
	}
	if task.Description == "" {
		lines = append(lines, "description: (none)")
	} else {
		lines = append(lines, "description:", fenced(task.Description, maxGoalDescriptionRunes))
	}
	sections := []string{
		strings.Join(head, "\n\n"),
		strings.Join(lines, "\n"),
		"### Status instructions\n\n" + goalInstructions(goal),
	}
	if working {
		sections = append(sections, "### Outcomes\n\n"+goalOutcomes(view))
	}
	sections = append(sections, "### Artifacts\n\n"+goalArtifacts(view))
	if working {
		if text := goalLastRequest(task.Key, view); text != "" {
			sections = append(sections, "### Last transition request\n\n"+text)
		}
		sections = append(sections, "### Commands\n\n"+
			"```\nttasks artifacts set "+task.Key+" NAME [VALUE]\nttasks advance "+task.Key+" --outcome NAME --from "+view.Status+" --message \"TEXT\"\n```\n"+
			"`ttasks artifacts set` reads the value from standard input when you give no VALUE.")
	}
	return strings.TrimRight(strings.Join(sections, "\n\n"), "\n")
}

// goalWorkable reports whether the agent may work on the task now: it is in a
// pool status and not paused.
func goalWorkable(goal WorkflowGoal) bool {
	return goal.Task.WorkflowPausedReason == "" && strings.HasPrefix(goal.View.Owner, "pool:") &&
		goal.View.Category == tasks.StatusInProgress
}

func goalInstructions(goal WorkflowGoal) string {
	view := goal.View
	switch {
	case goal.Task.WorkflowPausedReason != "":
		return fmt.Sprintf("This task is paused and waits for the customer's decision (reason: `%s`). Do not work on it.", goal.Task.WorkflowPausedReason)
	case view.Owner == "script":
		return "A script watches this task in this status. Do not work on it."
	case view.Owner == "customer" || view.Category == tasks.StatusWaitCustomer:
		return "This task waits for the customer in this status. Do not work on it."
	case !strings.HasPrefix(view.Owner, "pool:"):
		return "This task is closed and has no open status. Do not work on it."
	case goal.InstructionsUnreadable:
		return "The status instructions could not be read."
	case view.InstructionsPath == "" || strings.TrimSpace(goal.Instructions) == "":
		return "This status has no instructions."
	}
	text, cut := cutBytes(goal.Instructions, maxGoalInstructionsBytes)
	text = strings.TrimRight(text, "\n")
	if cut {
		text += fmt.Sprintf("\n\n[The status instructions were cut here: the file is longer than %d bytes.]", maxGoalInstructionsBytes)
	}
	return text
}

func goalOutcomes(view tasks.WorkflowView) string {
	if len(view.Outcomes) == 0 {
		return "This status has no outcome."
	}
	out := make([]string, 0, len(view.Outcomes))
	for _, o := range view.Outcomes {
		line := fmt.Sprintf("- `%s` -> `%s`", o.On, o.To)
		if len(o.Requires) > 0 {
			line += "; requires: " + strings.Join(o.Requires, ", ")
		}
		if len(o.Missing) > 0 {
			line += "; missing: " + strings.Join(o.Missing, ", ")
		}
		if len(o.Checks) > 0 {
			line += "; checks: " + strings.Join(o.Checks, ", ")
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func goalArtifacts(view tasks.WorkflowView) string {
	if len(view.Artifacts) == 0 {
		return "No artifact has been set."
	}
	out := make([]string, 0, len(view.Artifacts))
	for _, a := range view.Artifacts {
		out = append(out, fmt.Sprintf("- `%s` by %s:\n%s", a.Name, a.Author, fenced(a.Value, maxGoalValueRunes)))
	}
	return strings.Join(out, "\n\n")
}

// goalLastRequest describes the last transition request of the current visit
// when it was rejected or failed; any other state needs no word.
func goalLastRequest(key string, view tasks.WorkflowView) string {
	req := view.LastRequest
	if req == nil || (req.State != "rejected" && req.State != "failed") || !inCurrentVisit(view, req.CreatedAt) {
		return ""
	}
	message := req.ResultMessage
	if message == "" {
		message = "(no message)"
	}
	if req.State == "rejected" {
		return fmt.Sprintf("Your request for outcome `%s` was rejected. A check script found that the condition does not hold; the message below says what to fix.\n%s",
			req.Outcome, fenced(message, maxGoalValueRunes))
	}
	text := fmt.Sprintf("Your request for outcome `%s` failed: a check could not run. Repeating the request unchanged will not help.", req.Outcome)
	for _, run := range view.Runs { // newest first
		if run.Kind == "check" {
			text += fmt.Sprintf(" Read the log with `ttasks workflow log %s %d` and fix the cause first.", key, run.ID)
			break
		}
	}
	return text + "\n" + fenced(message, maxGoalValueRunes)
}

// previousVisit returns the visit before the current one.
func previousVisit(view tasks.WorkflowView) (tasks.StatusVisit, bool) {
	if len(view.Visits) < 2 {
		return tasks.StatusVisit{}, false
	}
	return view.Visits[len(view.Visits)-2], true
}

func inCurrentVisit(view tasks.WorkflowView, createdAt string) bool {
	if len(view.Visits) == 0 {
		return false
	}
	entered := view.Visits[len(view.Visits)-1].EnteredAt
	a, errA := time.Parse(time.RFC3339Nano, createdAt)
	b, errB := time.Parse(time.RFC3339Nano, entered)
	if errA != nil || errB != nil {
		return createdAt >= entered
	}
	return !a.Before(b)
}

// oneLine collapses a value into a single line cut to maxGoalValueRunes.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	text, _ := cutRunes(s, maxGoalValueRunes)
	return text
}

// fenced renders an untrusted value as data: inside a code fence longer than
// any run of backticks in the value, cut to limit runes.
func fenced(value string, limit int) string {
	text, cut := cutRunes(value, limit)
	if cut {
		text += goalCutMarker
	}
	fence := strings.Repeat("`", max(3, longestBacktickRun(text)+1))
	return fence + "\n" + strings.TrimRight(text, "\n") + "\n" + fence
}

func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

func cutRunes(s string, limit int) (string, bool) {
	if utf8.RuneCountInString(s) <= limit {
		return s, false
	}
	n := 0
	for i := range s {
		if n == limit {
			return s[:i], true
		}
		n++
	}
	return s, false
}

// cutBytes cuts s to at most limit bytes at a UTF-8 boundary.
func cutBytes(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// TaskWorkflowGoals is the WorkflowGoalSource backed by the Tasks service and
// the workflow image store.
type TaskWorkflowGoals struct {
	Tasks  workflowViewReader
	Images *workflowimage.Store
	Log    *slog.Logger
}

type workflowViewReader interface {
	GetWorkflow(context.Context, tasks.Actor, string) (tasks.WorkflowView, error)
}

// WorkflowGoal reads the task as agent sees it and its status instructions.
// An instructions file that cannot be read is logged and flagged, not fatal.
func (g TaskWorkflowGoals) WorkflowGoal(ctx context.Context, key, agent string) (WorkflowGoal, error) {
	view, err := g.Tasks.GetWorkflow(ctx, tasks.AgentActor(agent), key)
	if err != nil {
		return WorkflowGoal{}, err
	}
	goal := WorkflowGoal{View: view}
	if view.InstructionsPath == "" {
		return goal, nil
	}
	if g.Images == nil {
		goal.InstructionsUnreadable = true
		return goal, nil
	}
	text, err := g.readInstructions(view)
	if err != nil {
		if g.Log != nil {
			g.Log.Warn("read workflow status instructions", "task", key, "status", view.Status, "path", view.InstructionsPath, "err", err)
		}
		goal.InstructionsUnreadable = true
		return goal, nil
	}
	goal.Instructions = text
	return goal, nil
}

// readInstructions reads one byte more than the limit, so the formatter can
// tell a cut file from one that fits.
func (g TaskWorkflowGoals) readInstructions(view tasks.WorkflowView) (string, error) {
	path, err := g.Images.FilePath(view.Name, view.Digest, view.InstructionsPath)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxGoalInstructionsBytes+1))
	return string(data), err
}
