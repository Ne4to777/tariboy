package workflowrun

import (
	"errors"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/script"
)

func ip(n int) *int { return &n }

var declared = Declared{Outcomes: []string{"merged", "closed"}, Artifacts: []string{"merge_commit", "notes"}}

func TestClassifyExitTable(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		exit   int
		result string
		want   string
		out    string
	}{
		{"check 0", KindCheck, 0, "", VerdictPass, ""},
		{"check 0 ignores outcome", KindCheck, 0, `{"outcome":"nope"}`, VerdictPass, ""},
		{"check 111", KindCheck, script.QuietExit, "", VerdictFailure, ""},
		{"check 112", KindCheck, script.RejectExit, "", VerdictReject, ""},
		{"check other", KindCheck, 3, "", VerdictFailure, ""},
		{"watch 0 outcome", KindWatch, 0, `{"outcome":"merged"}`, VerdictOutcome, "merged"},
		{"watch 0 no file", KindWatch, 0, "", VerdictFailure, ""},
		{"watch 0 no outcome", KindWatch, 0, `{"message":"x"}`, VerdictFailure, ""},
		{"watch 0 undeclared", KindWatch, 0, `{"outcome":"lost"}`, VerdictFailure, ""},
		{"watch 111", KindWatch, script.QuietExit, "garbage", VerdictQuiet, ""},
		{"watch 112", KindWatch, script.RejectExit, "", VerdictFailure, ""},
		{"watch other", KindWatch, 1, "", VerdictFailure, ""},
		{"unknown kind", "other", 0, "", VerdictFailure, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw []byte
			if c.result != "" {
				raw = []byte(c.result)
			}
			v := Classify(c.kind, ip(c.exit), false, raw, nil, declared)
			if v.Kind != c.want || v.Outcome != c.out {
				t.Fatalf("got %+v, want kind %s outcome %q", v, c.want, c.out)
			}
			if v.Kind == VerdictFailure && v.Message == "" {
				t.Fatal("failure without a reason")
			}
		})
	}
}

func TestClassifyCheckPassKeepsMessageAndArtifacts(t *testing.T) {
	v := Classify(KindCheck, ip(0), false, []byte(`{"message":"ok","artifacts":{"notes":"n"}}`), nil, declared)
	if v.Kind != VerdictPass || v.Message != "ok" || v.Artifacts["notes"] != "n" {
		t.Fatalf("got %+v", v)
	}
}

func TestClassifyRejectMessage(t *testing.T) {
	v := Classify(KindCheck, ip(112), false, []byte(`{"message":"tests fail","artifacts":{"notes":"n"}}`), nil, declared)
	if v.Kind != VerdictReject || v.Message != "tests fail" || v.Artifacts != nil {
		t.Fatalf("got %+v", v)
	}
	v = Classify(KindCheck, ip(112), false, nil, nil, declared)
	if v.Kind != VerdictReject || v.Message != "the check's condition does not hold" {
		t.Fatalf("got %+v", v)
	}
	v = Classify(KindCheck, ip(112), false, []byte("  \n"), nil, declared)
	if v.Kind != VerdictReject || v.Message == "" {
		t.Fatalf("got %+v", v)
	}
}

func TestClassifyWatchOutcomeArtifacts(t *testing.T) {
	v := Classify(KindWatch, ip(0), false, []byte(`{"outcome":"merged","message":"m","artifacts":{"merge_commit":"9f"}}`), nil, declared)
	if v.Kind != VerdictOutcome || v.Message != "m" || v.Artifacts["merge_commit"] != "9f" {
		t.Fatalf("got %+v", v)
	}
}

func TestClassifyFailureReasons(t *testing.T) {
	big := strings.Repeat("a", MaxArtifactBytes+1)
	cases := []struct {
		name      string
		exit      *int
		timedOut  bool
		raw       string
		resultErr error
		contains  string
	}{
		{"no exit", nil, false, "", nil, "did not exit"},
		{"timeout", ip(0), true, `{}`, nil, "timed out"},
		{"timeout killed", nil, true, "", nil, "timed out"},
		{"result error", ip(0), false, "", errors.New("boom"), "boom"},
		{"malformed", ip(0), false, `{`, nil, "result file"},
		{"not object", ip(0), false, `[1]`, nil, "result file"},
		{"null", ip(0), false, `null`, nil, "result file"},
		{"trailing", ip(0), false, `{} {}`, nil, "result file"},
		{"unknown key", ip(0), false, `{"outcomes":"x"}`, nil, "outcomes"},
		{"oversized file", ip(0), false, strings.Repeat(" ", MaxResultBytes) + `{}`, nil, "too large"},
		{"oversized message", ip(0), false, `{"message":"` + strings.Repeat("m", MaxMessageBytes+1) + `"}`, nil, "message"},
		{"undeclared artifact", ip(0), false, `{"artifacts":{"zzz":"v"}}`, nil, "zzz"},
		{"oversized artifact", ip(0), false, `{"artifacts":{"notes":"` + big + `"}}`, nil, "too large"}, // the file bound and the artifact bound are both 64 KiB
		{"empty artifact", ip(0), false, `{"artifacts":{"notes":""}}`, nil, "notes"},
		{"bad utf8 artifact", ip(0), false, "{\"artifacts\":{\"notes\":\"\xff\"}}", nil, "UTF-8"},
		{"exit code", ip(7), false, `{"message":"why"}`, nil, "exit 7"},
		{"exit code message", ip(7), false, `{"message":"why"}`, nil, "why"},
		{"exit code bad file", ip(7), false, `{`, nil, "exit 7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw []byte
			if c.raw != "" {
				raw = []byte(c.raw)
			}
			v := Classify(KindCheck, c.exit, c.timedOut, raw, c.resultErr, declared)
			if v.Kind != VerdictFailure || !strings.Contains(v.Message, c.contains) {
				t.Fatalf("got %+v, want failure containing %q", v, c.contains)
			}
		})
	}
}

func TestProtocolEnv(t *testing.T) {
	env := ProtocolEnv(EnvValues{TaskKey: "A-1", Queue: "q", WorkflowName: "w", WorkflowVersion: "1", Status: "s",
		WorkflowDir: "/w", TaskFile: "/t.json", TaskDir: "/d", ResultFile: "/r.json"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "TARIBOY_WORKFLOW_OUTCOME") {
		t.Fatal("outcome present when empty")
	}
	for _, want := range []string{"TARIBOY_QUIET_EXIT=111", "TARIBOY_REJECT_EXIT=112", "TARIBOY_TASK_KEY=A-1", "TARIBOY_RESULT_FILE=/r.json"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %v", want, env)
		}
	}
	for i := 1; i < len(env); i++ {
		if env[i-1] > env[i] {
			t.Fatalf("not sorted: %v", env)
		}
	}
	env = ProtocolEnv(EnvValues{Outcome: "merged"})
	if !strings.Contains(strings.Join(env, "\n"), "TARIBOY_WORKFLOW_OUTCOME=merged") {
		t.Fatalf("outcome missing: %v", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "=") {
			t.Fatalf("empty name: %q", e)
		}
	}
}
