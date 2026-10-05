package workflowrun

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/script"
)

var sourceDeclared = Declared{Artifacts: []string{"pull_request"}}

func TestClassifySourceItems(t *testing.T) {
	raw := `{"items":[
		{"key":"org/repo#42@3f2a9c1","title":"Review org/repo#42","description":"https://x/42","priority":"P1",
		 "artifacts":{"pull_request":"https://github.com/org/repo/pull/42"}},
		{"key":"b","title":"Second"}]}`
	v := Classify(KindSource, ip(0), false, []byte(raw), nil, sourceDeclared)
	if v.Kind != VerdictItems || len(v.Items) != 2 {
		t.Fatalf("got %+v", v)
	}
	first := v.Items[0]
	if first.Key != "org/repo#42@3f2a9c1" || first.Title != "Review org/repo#42" || first.Description != "https://x/42" ||
		first.Priority != "P1" || first.Artifacts["pull_request"] != "https://github.com/org/repo/pull/42" {
		t.Fatalf("first item = %+v", first)
	}
}

func TestClassifySourceExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		exit   int
		result string
		want   string
	}{
		{"quiet ignores the file", script.QuietExit, "garbage", VerdictQuiet},
		{"0 without a file is no items", 0, "", VerdictItems},
		{"0 with an empty list", 0, `{"items":[]}`, VerdictItems},
		{"reject code", script.RejectExit, "", VerdictFailure},
		{"other code", 2, `{"items":[]}`, VerdictFailure},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw []byte
			if c.result != "" {
				raw = []byte(c.result)
			}
			if v := Classify(KindSource, ip(c.exit), false, raw, nil, sourceDeclared); v.Kind != c.want || len(v.Items) != 0 {
				t.Fatalf("got %+v, want %s", v, c.want)
			}
		})
	}
	if v := Classify(KindSource, nil, true, nil, nil, sourceDeclared); v.Kind != VerdictFailure {
		t.Fatalf("timeout = %+v", v)
	}
}

func TestClassifySourceRejectsInvalidResults(t *testing.T) {
	many := make([]string, MaxSourceItems+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"key":"k%d","title":"t"}`, i)
	}
	cases := map[string]string{
		"not an object":       `[]`,
		"unknown top field":   `{"items":[],"outcome":"x"}`,
		"unknown item field":  `{"items":[{"key":"a","title":"t","assignee":"x"}]}`,
		"missing key":         `{"items":[{"title":"t"}]}`,
		"bad key":             `{"items":[{"key":"a b","title":"t"}]}`,
		"long key":            `{"items":[{"key":"` + strings.Repeat("k", MaxSourceKeyBytes+1) + `","title":"t"}]}`,
		"repeated key":        `{"items":[{"key":"a","title":"t"},{"key":"a","title":"u"}]}`,
		"missing title":       `{"items":[{"key":"a","title":"  "}]}`,
		"multi-line title":    `{"items":[{"key":"a","title":"one\ntwo"}]}`,
		"long title":          `{"items":[{"key":"a","title":"` + strings.Repeat("t", MaxSourceTitleBytes+1) + `"}]}`,
		"long description":    `{"items":[{"key":"a","title":"t","description":"` + strings.Repeat("d", MaxSourceDescriptionBytes+1) + `"}]}`,
		"bad priority":        `{"items":[{"key":"a","title":"t","priority":"urgent"}]}`,
		"undeclared artifact": `{"items":[{"key":"a","title":"t","artifacts":{"plan":"x"}}]}`,
		"empty artifact":      `{"items":[{"key":"a","title":"t","artifacts":{"pull_request":""}}]}`,
		"too many items":      `{"items":[` + strings.Join(many, ",") + `]}`,
		"trailing data":       `{"items":[]} {}`,
		"too large":           `{"items":[],"x":"` + strings.Repeat("x", MaxSourceResultBytes) + `"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			v := Classify(KindSource, ip(0), false, []byte(raw), nil, sourceDeclared)
			if v.Kind != VerdictFailure || v.Message == "" || len(v.Items) != 0 {
				t.Fatalf("got %+v", v)
			}
			if len(v.Message) > MaxMessageBytes {
				t.Fatalf("message is %d bytes", len(v.Message))
			}
		})
	}
}
