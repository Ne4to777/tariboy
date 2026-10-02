package workflowfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// developmentManifest is the development example of the design spec
// (docs/superpowers/specs/2026-10-02-workflow-images-design.md, section
// "Workflow image source"), copied exactly.
const developmentManifest = `schema_version: 1
name: development
workflow_version: 0.1.0
initial_status: plan

requires_secrets: [GH_TOKEN]
env:
  PR_POLL_SECONDS: "60"

limits:
  idle_iterations: 3
  rejected_requests: 5
  script_failures: 3
  unavailable_grace: 5m

artifacts:
  - name: plan
    description: The implementation plan, as Markdown.
  - name: pull_request
    description: URL of the pull request for this task.
  - name: merge_commit
    description: Merge commit recorded by the monitor.

statuses:
  - id: plan
    owner: { pool: developers }
    instructions: ./statuses/plan.md
    transitions:
      - on: planned
        to: approval
        requires: [plan]

  - id: approval
    owner: customer
    transitions:
      - { on: approved, to: implement }
      - { on: changes_requested, to: plan }

  - id: implement
    owner: { pool: developers }
    instructions: ./statuses/implement.md
    limits: { idle_iterations: 6 }
    transitions:
      - on: ready
        to: review
        requires: [pull_request]
        checks:
          - script: ./scripts/pr-open.sh
            timeout: 60s

  - id: review
    owner: script
    watch:
      script: ./scripts/pr-monitor.py
      every: 60s
      timeout: 60s
    transitions:
      - { on: merged, to: complete }
      - { on: changes_requested, to: implement }

  - id: complete
    owner: { pool: developers }
    instructions: ./statuses/complete.md
    transitions:
      - on: cleaned
        to: done
        checks:
          - script: ./scripts/merged-on-base.sh
            run_as: agent

  - id: done
    terminal: true
`

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DefaultFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseDevelopmentExample(t *testing.T) {
	dir := writeManifest(t, developmentManifest)
	f, err := Parse(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.SchemaVersion != 1 || f.Name != "development" || f.WorkflowVersion != "0.1.0" || f.InitialStatus != "plan" {
		t.Fatalf("root fields: %+v", f)
	}
	if len(f.RequiresSecrets) != 1 || f.RequiresSecrets[0] != "GH_TOKEN" || f.Env["PR_POLL_SECONDS"] != "60" {
		t.Fatalf("secrets/env: %+v %+v", f.RequiresSecrets, f.Env)
	}
	if f.Limits == nil || *f.Limits.IdleIterations != 3 || *f.Limits.RejectedRequests != 5 ||
		*f.Limits.ScriptFailures != 3 || f.Limits.UnavailableGrace != "5m" {
		t.Fatalf("limits: %+v", f.Limits)
	}
	if len(f.Artifacts) != 3 || f.Artifacts[1].Name != "pull_request" || f.Artifacts[0].Description == "" {
		t.Fatalf("artifacts: %+v", f.Artifacts)
	}
	if len(f.Statuses) != 6 {
		t.Fatalf("statuses: %d", len(f.Statuses))
	}
	plan, approval, impl, review, complete, done := f.Statuses[0], f.Statuses[1], f.Statuses[2], f.Statuses[3], f.Statuses[4], f.Statuses[5]
	if plan.Owner != (Owner{Kind: OwnerPool, Pool: "developers"}) || plan.Instructions != "./statuses/plan.md" {
		t.Fatalf("plan: %+v", plan)
	}
	if tr := plan.Transitions[0]; tr.On != "planned" || tr.To != "approval" || len(tr.Requires) != 1 || tr.Requires[0] != "plan" {
		t.Fatalf("plan transition: %+v", tr)
	}
	if approval.Owner != (Owner{Kind: OwnerCustomer}) || len(approval.Transitions) != 2 || approval.Transitions[1].On != "changes_requested" {
		t.Fatalf("approval: %+v", approval)
	}
	if impl.Limits == nil || *impl.Limits.IdleIterations != 6 || impl.Limits.RejectedRequests != nil {
		t.Fatalf("implement limits: %+v", impl.Limits)
	}
	if c := impl.Transitions[0].Checks; len(c) != 1 || c[0].Script != "./scripts/pr-open.sh" || c[0].Timeout != "60s" {
		t.Fatalf("implement checks: %+v", c)
	}
	if review.Owner != (Owner{Kind: OwnerScript}) || review.Watch == nil ||
		review.Watch.Script != "./scripts/pr-monitor.py" || review.Watch.Every != "60s" || review.Watch.Timeout != "60s" {
		t.Fatalf("review: %+v", review)
	}
	if c := complete.Transitions[0].Checks; len(c) != 1 || c[0].RunAs != RunAsAgent {
		t.Fatalf("complete checks: %+v", c)
	}
	if done.ID != "done" || !done.Terminal || done.Owner.Kind != "" || done.Cancelled {
		t.Fatalf("done: %+v", done)
	}
}

func TestParseAcceptsFileAndDirectory(t *testing.T) {
	dir := writeManifest(t, developmentManifest)
	for _, path := range []string{dir, filepath.Join(dir, DefaultFilename)} {
		f, err := Parse(path)
		if err != nil {
			t.Fatalf("Parse(%s): %v", path, err)
		}
		if f.Dir != dir {
			t.Fatalf("Parse(%s): Dir = %q, want %q", path, f.Dir, dir)
		}
	}
}

func TestParseRejectsOtherManifestFileName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alt.yaml")
	if err := os.WriteFile(path, []byte(developmentManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Parse(path)
	if err == nil || !strings.Contains(err.Error(), "must be named Workflowfile.yaml") {
		t.Fatalf("err = %v", err)
	}
}

// TestDevelopmentExampleValidates parses and validates the development example
// of the design spec, section "Workflow image source", with every file it
// names present.
func TestDevelopmentExampleValidates(t *testing.T) {
	dir := writeManifest(t, developmentManifest)
	for rel, mode := range map[string]os.FileMode{
		"statuses/plan.md":          0o644,
		"statuses/implement.md":     0o644,
		"statuses/complete.md":      0o644,
		"scripts/pr-open.sh":        0o755,
		"scripts/pr-monitor.py":     0o755,
		"scripts/merged-on-base.sh": 0o755,
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Parse(dir)
	if err != nil {
		t.Fatal(err)
	}
	if errs := Validate(f); len(errs) != 0 {
		t.Fatalf("errors = %+v", errs)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	cases := map[string]struct{ body, field string }{
		"root":       {"schema_version: 1\nbogus_root: x\nstatuses: []\n", "bogus_root"},
		"status":     {"schema_version: 1\nstatuses:\n  - id: a\n    bogus_status: x\n", "bogus_status"},
		"transition": {"schema_version: 1\nstatuses:\n  - id: a\n    transitions:\n      - on: x\n        bogus_tr: y\n", "bogus_tr"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(writeManifest(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("err = %v, want mention of %q", err, tc.field)
			}
		})
	}
}

func TestParseRejectsSecondDocument(t *testing.T) {
	_, err := Parse(writeManifest(t, "schema_version: 1\nname: a\n---\nname: b\n"))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRejectsBadOwner(t *testing.T) {
	for name, owner := range map[string]string{
		"unknown scalar": "nobody",
		"extra key":      "{ pool: a, extra: b }",
		"unknown key":    "{ team: a }",
		"sequence":       "[a]",
		"empty pool":     "{ pool: \"\" }",
		"null pool":      "{ pool: null }",
		"tilde pool":     "{ pool: ~ }",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(writeManifest(t, "schema_version: 1\nstatuses:\n  - id: a\n    owner: "+owner+"\n"))
			if err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestOwnerRoundTrip(t *testing.T) {
	type holder struct {
		Owner Owner `yaml:"owner"`
	}
	for _, want := range []Owner{
		{Kind: OwnerCustomer},
		{Kind: OwnerScript},
		{Kind: OwnerPool, Pool: "developers"},
	} {
		data, err := yaml.Marshal(holder{want})
		if err != nil {
			t.Fatal(err)
		}
		var got holder
		if err := yaml.Unmarshal(data, &got); err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if got.Owner != want {
			t.Fatalf("round trip %q: got %+v, want %+v", data, got.Owner, want)
		}
	}
}

func TestOwnerAbsentLeavesKindEmpty(t *testing.T) {
	f, err := Parse(writeManifest(t, "schema_version: 1\nstatuses:\n  - id: done\n    terminal: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Statuses[0].Owner != (Owner{}) {
		t.Fatalf("owner = %+v", f.Statuses[0].Owner)
	}
}

func TestTransitionOnKeyIsString(t *testing.T) {
	f, err := Parse(writeManifest(t, "schema_version: 1\nstatuses:\n  - id: a\n    owner: customer\n    transitions:\n      - on: go\n        to: b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statuses[0].Transitions[0].On; got != "go" {
		t.Fatalf("On = %q", got)
	}
}
