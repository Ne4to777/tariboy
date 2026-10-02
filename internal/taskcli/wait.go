package taskcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/tasks"
)

const (
	// pollInterval is the pause between two polls of a pending request.
	pollInterval = time.Second
	// defaultWait bounds the wait when the daemon names no wait_seconds.
	defaultWait = 90 * time.Second
)

// waitClock is the clock and the sleep of the wait loop. Tests replace it so
// they do not sleep.
var waitClock = struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}{
	now: time.Now,
	sleep: func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	},
}

// waitForAdvance finishes an advance whose request may still be pending: it
// polls the request until it leaves "pending", for at most the daemon's
// wait_seconds, then prints the result. poll fetches the request by id and
// fail reports a failed call as the command's error. The exit code is 0 for an
// applied request and 1 for a rejected, failed, or cancelled one or a wait that
// ran out.
func waitForAdvance(ctx context.Context, parsed request, raw json.RawMessage, poll func(id int64) (json.RawMessage, error), fail func(error) int, jsonOut bool, stdout, stderr io.Writer) int {
	var current tasks.TransitionRequest
	if err := json.Unmarshal(raw, &current); err != nil || current.State != "pending" {
		return printResult(parsed, raw, jsonOut, stdout, stderr)
	}
	key, _ := parsed.payload["key"].(string)
	wait := defaultWait
	if current.WaitSeconds > 0 {
		wait = time.Duration(current.WaitSeconds) * time.Second
	}
	deadline := waitClock.now().Add(wait)
	var lastErr error
	for current.State == "pending" {
		if err := waitClock.sleep(ctx, pollInterval); err != nil {
			if jsonOut {
				fmt.Fprintln(stdout, string(raw))
			}
			fmt.Fprintf(stderr, "tasks advance: interrupted; request %d of %s may still be pending\n", current.ID, key)
			fmt.Fprintf(stderr, "hint: ttasks workflow get %s shows its state\n", key)
			return 1
		}
		polled, err := poll(current.ID)
		var apiErr *client.APIError
		switch {
		case errors.As(err, &apiErr):
			return fail(err)
		case err != nil:
			lastErr = err // a transport error may pass; retry until the deadline
		default:
			var next tasks.TransitionRequest
			if json.Unmarshal(polled, &next) != nil {
				lastErr = unreadableReply{id: current.ID}
				break
			}
			lastErr, raw, current = nil, polled, next
		}
		if current.State == "pending" && !waitClock.now().Before(deadline) {
			var unreadable unreadableReply
			if errors.As(lastErr, &unreadable) {
				fmt.Fprintf(stderr, "request %d of %s: the daemon's reply could not be read\n", unreadable.id, key)
				fmt.Fprintf(stderr, "hint: ttasks workflow get %s shows its state\n", key)
				return 1
			}
			if lastErr != nil {
				return fail(lastErr)
			}
			if jsonOut {
				fmt.Fprintln(stdout, string(raw))
			}
			fmt.Fprintf(stderr, "still pending: request %d of %s did not finish within %d seconds\n", current.ID, key, int(wait/time.Second))
			fmt.Fprintf(stderr, "hint: ttasks workflow get %s shows its state\n", key)
			return 1
		}
	}
	return printFinishedRequest(parsed, current, raw, jsonOut, stdout, stderr)
}

// unreadableReply is a poll answer that is not a transition request.
type unreadableReply struct{ id int64 }

func (e unreadableReply) Error() string { return "the daemon's reply could not be read" }

// runLogPath finds the run id in the "log: <path>" line of a failed request:
// the worker keeps a run's log at .../runs/<id>/run.log.
var runLogPath = regexp.MustCompile(`/runs/(\d+)/run\.log\s*$`)

// printFinishedRequest prints a request that left "pending". Text goes to
// stdout for an applied request and to stderr, as an error, otherwise; with
// --json the request object is printed on stdout in every case.
func printFinishedRequest(parsed request, request tasks.TransitionRequest, raw json.RawMessage, jsonOut bool, stdout, stderr io.Writer) int {
	key, _ := parsed.payload["key"].(string)
	if request.State == "applied" {
		return printResult(parsed, raw, jsonOut, stdout, stderr)
	}
	if jsonOut {
		fmt.Fprintln(stdout, string(raw))
	}
	out := stderr
	if jsonOut {
		out = io.Discard
	}
	message := cleanText(request.ResultMessage)
	switch request.State {
	case "rejected":
		fmt.Fprintf(out, "rejected: %s did not advance by %q; a check says the condition does not hold\n", key, request.Outcome)
		if message != "" {
			fmt.Fprintln(out, message)
		}
		command := "ttasks advance " + key + " --outcome " + request.Outcome
		if from, _ := parsed.payload["from"].(string); from != "" {
			command += " --from " + from
		}
		fmt.Fprintf(out, "hint: repeat with %s\n", command)
	case "failed":
		fmt.Fprintf(out, "failed: a check of %q could not run for %s\n", request.Outcome, key)
		if message != "" {
			fmt.Fprintln(out, message)
		}
		if match := runLogPath.FindStringSubmatch(request.ResultMessage); match != nil {
			fmt.Fprintf(out, "hint: ttasks workflow log %s %s\n", key, match[1])
		}
	case "cancelled":
		fmt.Fprintf(out, "cancelled: request %d of %s was cancelled; an operator moved or cancelled the task\n", request.ID, key)
		if message != "" {
			fmt.Fprintln(out, message)
		}
	default:
		fmt.Fprintf(out, "request %d of %s ended in state %q\n", request.ID, key, request.State)
	}
	return 1
}

// cleanText keeps the text of a script message or log but replaces control
// characters other than newline and tab: an escape sequence would act on the
// reader's terminal.
func cleanText(text string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.Is(unicode.Cc, r) {
			return '�'
		}
		return r
	}, strings.TrimRight(text, "\n"))
}

// printRuns renders the runs of "workflow runs" as a table. It reports false
// when raw is not a run list, so the caller falls back to the generic dump.
func printRuns(raw json.RawMessage, stdout io.Writer) bool {
	var list struct {
		Runs []tasks.ScriptRun `json:"runs"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return false
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tKIND\tSCRIPT\tSTATE\tVERDICT\tEXIT\tSTARTED\tDURATION")
	for _, run := range list.Runs {
		exit := "-"
		if run.ExitCode != nil {
			exit = fmt.Sprint(*run.ExitCode)
		}
		fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", run.ID, run.Kind, preview(run.Script), run.State,
			dash(run.Verdict), exit, dash(run.StartedAt), runDuration(run))
	}
	table.Flush()
	return true
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// runDuration is how long a finished run took; "-" for one that has not both
// started and finished.
func runDuration(run tasks.ScriptRun) string {
	started, err := time.Parse(time.RFC3339Nano, run.StartedAt)
	if err != nil {
		return "-"
	}
	finished, err := time.Parse(time.RFC3339Nano, run.FinishedAt)
	if err != nil || finished.Before(started) {
		return "-"
	}
	return finished.Sub(started).Round(time.Millisecond).String()
}

// printRunLog prints the log text of "workflow log"; when the daemon cut the
// log it first says so on stderr. It reports false when raw is not a log.
func printRunLog(raw json.RawMessage, stdout, stderr io.Writer) bool {
	var log struct {
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	}
	if json.Unmarshal(raw, &log) != nil {
		return false
	}
	if log.Truncated {
		fmt.Fprintf(stderr, "(truncated to the last %d bytes)\n", len(log.Text))
	}
	fmt.Fprint(stdout, cleanText(log.Text))
	if log.Text != "" {
		fmt.Fprintln(stdout)
	}
	return true
}
