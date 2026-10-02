package taskcli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/client"
)

// pollCaller answers an advance with first and each request_get from polls, in
// order; the last poll repeats.
type pollCaller struct {
	first json.RawMessage
	polls []func() (json.RawMessage, error)
	calls []call
}

func (c *pollCaller) Call(method, route string, body any) (json.RawMessage, error) {
	c.calls = append(c.calls, call{method, route, body})
	if strings.HasSuffix(route, "/advance") {
		return c.first, nil
	}
	poll := c.polls[min(len(c.calls)-2, len(c.polls)-1)]
	return poll()
}

func (c *pollCaller) pollCount() int { return len(c.calls) - 1 }

func reply(raw string) func() (json.RawMessage, error) {
	return func() (json.RawMessage, error) { return json.RawMessage(raw), nil }
}

func fail(err error) func() (json.RawMessage, error) {
	return func() (json.RawMessage, error) { return nil, err }
}

// fakeClock replaces the wait clock: sleep advances the clock instead of
// sleeping and returns the context's error once it is done.
func fakeClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	old := waitClock
	t.Cleanup(func() { waitClock = old })
	waitClock.now = func() time.Time { return now }
	waitClock.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		now = now.Add(d)
		return nil
	}
	return &now
}

const (
	pendingRequest = `{"id":7,"task_key":"DEV-1","outcome":"approve","actor":"agent:a","state":"pending","created_at":"t","wait_seconds":60}`
	appliedRequest = `{"id":7,"task_key":"DEV-1","outcome":"approve","actor":"agent:a","state":"applied","created_at":"t","finished_at":"u"}`
)

func advanceArgs(extra ...string) []string {
	return append([]string{"advance", "DEV-1", "--outcome", "approve", "--from", "review"}, extra...)
}

func TestAdvanceWaitsForAPendingRequestAndPrintsApplied(t *testing.T) {
	fakeClock(t)
	c := &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){reply(pendingRequest), reply(appliedRequest)}}
	withCaller(t, c)
	var out, errOut strings.Builder
	if code := Run(context.Background(), advanceArgs(), agentEnv(), &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, &errOut)
	}
	if !strings.Contains(out.String(), "state: applied") || errOut.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", &out, &errOut)
	}
	if len(c.calls) != 3 || c.calls[1].route != "/tools/tasks/request_get" || !sameJSON(c.calls[1].body, map[string]any{"key": "DEV-1", "id": 7}) {
		t.Fatalf("calls = %#v", c.calls)
	}
}

func TestAdvanceWaitReportsARejectionWithTheCommandToRepeat(t *testing.T) {
	fakeClock(t)
	rejected := `{"id":7,"task_key":"DEV-1","outcome":"approve","actor":"a","state":"rejected","result_message":"CI is red\u001b[2J","created_at":"t"}`
	c := &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){reply(rejected)}}
	withCaller(t, c)
	var out, errOut strings.Builder
	if code := Run(context.Background(), advanceArgs(), agentEnv(), &out, &errOut); code != 1 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"rejected:", "CI is red", "hint: repeat with ttasks advance DEV-1 --outcome approve --from review"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, &errOut)
		}
	}
	if strings.Contains(errOut.String(), "\x1b") || out.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", &out, &errOut)
	}
}

func TestAdvanceWaitReportsAFailureWithTheLogHint(t *testing.T) {
	fakeClock(t)
	failed := `{"id":7,"task_key":"DEV-1","outcome":"approve","actor":"a","state":"failed","result_message":"exit status 3\nlog: /base/tasks/DEV-1/runs/12/run.log","created_at":"t"}`
	withCaller(t, &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){reply(failed)}})
	var errOut strings.Builder
	if code := Run(context.Background(), advanceArgs(), agentEnv(), io.Discard, &errOut); code != 1 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"failed:", "could not run", "log: /base/tasks/DEV-1/runs/12/run.log", "hint: ttasks workflow log DEV-1 12"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, &errOut)
		}
	}
	// No run id in the message: no log hint.
	withCaller(t, &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){
		reply(`{"id":7,"outcome":"approve","state":"failed","result_message":"no runtime","created_at":"t"}`)}})
	errOut.Reset()
	if code := Run(context.Background(), advanceArgs(), agentEnv(), io.Discard, &errOut); code != 1 || strings.Contains(errOut.String(), "workflow log") {
		t.Fatalf("code %d stderr %q", code, &errOut)
	}
}

func TestAdvanceWaitReportsACancelledRequest(t *testing.T) {
	fakeClock(t)
	cancelled := `{"id":7,"task_key":"DEV-1","outcome":"approve","actor":"a","state":"cancelled","result_message":"moved by user:me","created_at":"t"}`
	withCaller(t, &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){reply(cancelled)}})
	var errOut strings.Builder
	if code := Run(context.Background(), advanceArgs(), agentEnv(), io.Discard, &errOut); code != 1 ||
		!strings.Contains(errOut.String(), "cancelled:") || !strings.Contains(errOut.String(), "moved by user:me") {
		t.Fatalf("code %d stderr %q", code, &errOut)
	}
}

func TestAdvanceWaitTimesOutAfterWaitSeconds(t *testing.T) {
	now := fakeClock(t)
	start := *now
	c := &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){reply(pendingRequest)}}
	withCaller(t, c)
	var errOut strings.Builder
	if code := Run(context.Background(), advanceArgs(), agentEnv(), io.Discard, &errOut); code != 1 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"still pending", "60 seconds", "ttasks workflow get DEV-1"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, &errOut)
		}
	}
	if elapsed := now.Sub(start); elapsed != 60*time.Second || c.pollCount() != 60 {
		t.Fatalf("waited %v with %d polls; want 60s and one poll a second", elapsed, c.pollCount())
	}
}

func TestAdvanceWaitFallsBackToNinetySecondsWithoutWaitSeconds(t *testing.T) {
	now := fakeClock(t)
	start := *now
	bare := `{"id":7,"task_key":"DEV-1","outcome":"approve","state":"pending","created_at":"t"}`
	withCaller(t, &pollCaller{first: json.RawMessage(bare), polls: []func() (json.RawMessage, error){reply(bare)}})
	if code := Run(context.Background(), advanceArgs(), agentEnv(), io.Discard, io.Discard); code != 1 || now.Sub(start) != 90*time.Second {
		t.Fatalf("code %d waited %v", code, now.Sub(start))
	}
}

func TestAdvanceNoWaitPrintsThePendingRequest(t *testing.T) {
	fakeClock(t)
	c := &pollCaller{first: json.RawMessage(pendingRequest)}
	withCaller(t, c)
	var out strings.Builder
	if code := Run(context.Background(), advanceArgs("--no-wait"), agentEnv(), &out, io.Discard); code != 0 {
		t.Fatalf("code %d", code)
	}
	if len(c.calls) != 1 || !strings.Contains(out.String(), "state: pending") {
		t.Fatalf("calls %#v stdout %q", c.calls, &out)
	}
	if body, _ := c.calls[0].body.(map[string]any); body["no-wait"] != nil || body["no_wait"] != nil {
		t.Fatalf("the daemon was sent a client option: %#v", body)
	}
}

func TestAdvanceAppliedWithoutChecksDoesNotPoll(t *testing.T) {
	fakeClock(t)
	c := &pollCaller{first: json.RawMessage(appliedRequest)}
	withCaller(t, c)
	if code := Run(context.Background(), advanceArgs(), agentEnv(), io.Discard, io.Discard); code != 0 || len(c.calls) != 1 {
		t.Fatalf("code %d calls %#v", code, c.calls)
	}
}

func TestAdvanceWaitStopsWhenTheContextIsCancelled(t *testing.T) {
	fakeClock(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){reply(pendingRequest)}}
	withCaller(t, c)
	var errOut strings.Builder
	if code := Run(ctx, advanceArgs(), agentEnv(), io.Discard, &errOut); code != 1 || c.pollCount() != 0 ||
		!strings.Contains(errOut.String(), "interrupted") || !strings.Contains(errOut.String(), "workflow get DEV-1") {
		t.Fatalf("code %d polls %d stderr %q", code, c.pollCount(), &errOut)
	}
}

func TestAdvanceWaitRetriesATransientTransportError(t *testing.T) {
	fakeClock(t)
	c := &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){
		fail(errors.New("connection reset")), reply(appliedRequest)}}
	withCaller(t, c)
	if code := Run(context.Background(), advanceArgs(), agentEnv(), io.Discard, io.Discard); code != 0 || c.pollCount() != 2 {
		t.Fatalf("code %d polls %d", code, c.pollCount())
	}
}

func TestAdvanceWaitReportsTheLastTransportErrorAtTheDeadline(t *testing.T) {
	fakeClock(t)
	c := &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){fail(errors.New("connection reset"))}}
	withCaller(t, c)
	var errOut strings.Builder
	if code := Run(context.Background(), advanceArgs(), agentEnv(), io.Discard, &errOut); code != 2 || !strings.Contains(errOut.String(), "not reachable") || c.pollCount() != 60 {
		t.Fatalf("code %d polls %d stderr %q", code, c.pollCount(), &errOut)
	}
	// Operator mode names the transport error itself.
	c = &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){fail(errors.New("connection reset"))}}
	withCaller(t, c)
	errOut.Reset()
	if code := Run(context.Background(), advanceArgs(), operatorEnv(t), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "connection reset") {
		t.Fatalf("operator code %d stderr %q", code, &errOut)
	}
}

func TestAdvanceWaitStopsAtOnceOnARefusal(t *testing.T) {
	fakeClock(t)
	c := &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){
		fail(&client.APIError{Code: "not_found", Msg: "no such request"})}}
	withCaller(t, c)
	var errOut strings.Builder
	if code := Run(context.Background(), advanceArgs(), agentEnv(), io.Discard, &errOut); code != 1 || c.pollCount() != 1 ||
		!strings.Contains(errOut.String(), "error (not_found)") {
		t.Fatalf("code %d polls %d stderr %q", code, c.pollCount(), &errOut)
	}
}

func TestAdvanceWaitInOperatorModeUsesTheRestRoute(t *testing.T) {
	fakeClock(t)
	c := &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){reply(appliedRequest)}}
	withCaller(t, c)
	if code := Run(context.Background(), advanceArgs(), operatorEnv(t), io.Discard, io.Discard); code != 0 {
		t.Fatalf("code %d", code)
	}
	if len(c.calls) != 2 || c.calls[1].method != "GET" || c.calls[1].route != "/api/tasks/DEV-1/workflow/requests/7" {
		t.Fatalf("calls = %#v", c.calls)
	}
}

func TestAdvanceWaitJSONPrintsTheFinalRequestAndKeepsTheExitCode(t *testing.T) {
	fakeClock(t)
	rejected := `{"id":7,"task_key":"DEV-1","outcome":"approve","actor":"a","state":"rejected","result_message":"no","created_at":"t"}`
	withCaller(t, &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){reply(rejected)}})
	var out, errOut strings.Builder
	if code := Run(context.Background(), advanceArgs("--json"), agentEnv(), &out, &errOut); code != 1 {
		t.Fatalf("code %d", code)
	}
	if strings.TrimSpace(out.String()) != rejected || errOut.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", &out, &errOut)
	}
	withCaller(t, &pollCaller{first: json.RawMessage(pendingRequest), polls: []func() (json.RawMessage, error){reply(appliedRequest)}})
	out.Reset()
	if code := Run(context.Background(), advanceArgs("--json"), agentEnv(), &out, io.Discard); code != 0 || strings.TrimSpace(out.String()) != appliedRequest {
		t.Fatalf("applied code %d stdout %q", code, &out)
	}
}

func TestParseWorkflowRunsAndLog(t *testing.T) {
	for argv, want := range map[string]request{
		"workflow runs DEV-1":                        {action: "workflow_runs", payload: map[string]any{"key": "DEV-1"}},
		"workflow log DEV-1 4":                       {action: "workflow_run_log", payload: map[string]any{"key": "DEV-1", "id": int64(4)}},
		"workflow log DEV-1 4 --max-bytes 2048":      {action: "workflow_run_log", payload: map[string]any{"key": "DEV-1", "id": int64(4), "max_bytes": 2048}},
		"advance DEV-1 --outcome ok --no-wait":       {action: "advance", payload: map[string]any{"key": "DEV-1", "outcome": "ok"}, noWait: true},
		"advance DEV-1 --outcome ok --no-wait=false": {action: "advance", payload: map[string]any{"key": "DEV-1", "outcome": "ok"}},
	} {
		got, err := parse(strings.Fields(argv))
		if err != nil || got.action != want.action || got.noWait != want.noWait || !sameJSON(got.payload, want.payload) {
			t.Errorf("parse(%q) = %#v, %v; want %#v", argv, got, err, want)
		}
	}
	for argv, want := range map[string]string{
		"workflow runs":                       "tasks workflow runs: task key is required",
		"workflow runs DEV-1 extra":           "tasks workflow runs: unexpected argument: extra",
		"workflow log DEV-1":                  "tasks workflow log: run id is required",
		"workflow log DEV-1 abc":              `tasks workflow log: run id must be a positive number, got "abc"`,
		"workflow log DEV-1 0":                `tasks workflow log: run id must be a positive number, got "0"`,
		"workflow log DEV-1 4 5":              "tasks workflow log: unexpected argument: 5",
		"workflow log DEV-1 4 --max-bytes 0":  `tasks workflow log: --max-bytes must be a positive number, got "0"`,
		"workflow log DEV-1 4 --max-bytes xx": `tasks workflow log: --max-bytes must be a positive number, got "xx"`,
	} {
		_, err := parse(strings.Fields(argv))
		if err == nil || err.Error() != want {
			t.Errorf("parse(%q) error = %v; want %q", argv, err, want)
		}
	}
}

func TestWorkflowRunsAndLogUseToolsActionsAndRestRoutes(t *testing.T) {
	r := &recorder{result: json.RawMessage(`{"runs":[],"count":0}`)}
	withCaller(t, r)
	Run(context.Background(), []string{"workflow", "runs", "DEV-1"}, agentEnv(), io.Discard, io.Discard)
	Run(context.Background(), []string{"workflow", "log", "DEV-1", "4", "--max-bytes", "99"}, agentEnv(), io.Discard, io.Discard)
	Run(context.Background(), []string{"workflow", "runs", "DEV-1"}, operatorEnv(t), io.Discard, io.Discard)
	Run(context.Background(), []string{"workflow", "log", "DEV-1", "4", "--max-bytes", "99"}, operatorEnv(t), io.Discard, io.Discard)
	Run(context.Background(), []string{"workflow", "log", "DEV-1", "4"}, operatorEnv(t), io.Discard, io.Discard)
	want := []call{
		{"POST", "/tools/tasks/workflow_runs", map[string]any{"key": "DEV-1"}},
		{"POST", "/tools/tasks/workflow_run_log", map[string]any{"key": "DEV-1", "id": 4, "max_bytes": 99}},
		{"GET", "/api/tasks/DEV-1/workflow/runs", map[string]string{}},
		{"GET", "/api/tasks/DEV-1/workflow/runs/4/log", map[string]string{"max_bytes": "99"}},
		{"GET", "/api/tasks/DEV-1/workflow/runs/4/log", map[string]string{}},
	}
	if len(r.calls) != len(want) {
		t.Fatalf("calls = %#v", r.calls)
	}
	for i, w := range want {
		if r.calls[i].method != w.method || r.calls[i].route != w.route || !sameJSON(r.calls[i].body, w.body) {
			t.Errorf("call %d = %#v; want %#v", i, r.calls[i], w)
		}
	}
}

func TestWorkflowRunsPrintsATableNewestFirst(t *testing.T) {
	withCaller(t, &recorder{result: json.RawMessage(`{"count":2,"runs":[
	 {"id":9,"task_key":"DEV-1","kind":"check","script":"checks/ci.sh","run_as":"queue","state":"finished","verdict":"reject","exit_code":112,
	  "created_at":"2026-10-02T10:00:00Z","started_at":"2026-10-02T10:00:01Z","finished_at":"2026-10-02T10:00:03.5Z"},
	 {"id":8,"task_key":"DEV-1","kind":"watch","script":"watch/merge.sh","run_as":"queue","state":"running","created_at":"2026-10-02T09:00:00Z","started_at":"2026-10-02T09:00:01Z"}]}`)})
	var out strings.Builder
	if code := Run(context.Background(), []string{"workflow", "runs", "DEV-1"}, agentEnv(), &out, io.Discard); code != 0 {
		t.Fatalf("code %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("table:\n%s", &out)
	}
	for i, wants := range [][]string{
		{"ID", "KIND", "SCRIPT", "STATE", "VERDICT", "EXIT", "STARTED", "DURATION"},
		{"9", "check", "checks/ci.sh", "finished", "reject", "112", "2026-10-02T10:00:01Z", "2.5s"},
		{"8", "watch", "watch/merge.sh", "running", "-", "-", "2026-10-02T09:00:01Z", "-"},
	} {
		if fields := strings.Fields(lines[i]); strings.Join(fields, " ") != strings.Join(wants, " ") {
			t.Errorf("line %d = %q; want fields %v", i, lines[i], wants)
		}
	}
	// --json prints the daemon's answer as is.
	out.Reset()
	if code := Run(context.Background(), []string{"workflow", "runs", "DEV-1", "--json"}, agentEnv(), &out, io.Discard); code != 0 || !strings.Contains(out.String(), `"count":2`) {
		t.Fatalf("json code %d: %s", code, &out)
	}
}

func TestWorkflowLogPrintsTheTextAndSaysWhenTruncated(t *testing.T) {
	withCaller(t, &recorder{result: json.RawMessage(`{"run_id":4,"text":"line 1\nline 2\u001b[2J\n","truncated":true}`)})
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"workflow", "log", "DEV-1", "4"}, agentEnv(), &out, &errOut); code != 0 {
		t.Fatalf("code %d", code)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.HasPrefix(out.String(), "line 1\nline 2") || !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("stdout %q", &out)
	}
	if !strings.HasPrefix(errOut.String(), "(truncated to the last ") || !strings.HasSuffix(errOut.String(), " bytes)\n") {
		t.Fatalf("stderr %q", &errOut)
	}
	withCaller(t, &recorder{result: json.RawMessage(`{"run_id":4,"text":"all\n","truncated":false}`)})
	out.Reset()
	errOut.Reset()
	Run(context.Background(), []string{"workflow", "log", "DEV-1", "4"}, agentEnv(), &out, &errOut)
	if out.String() != "all\n" || errOut.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", &out, &errOut)
	}
}
