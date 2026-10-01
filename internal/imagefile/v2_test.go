package imagefile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeV2Source(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DefaultFilename), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseV2PreservesExactOrder(t *testing.T) {
	dir := writeV2Source(t, `schema_version: 2
plugins: [{name: whoami}, {name: context}]
skills:
  - {dir: ./skills/review}
  - {dir: $PLUGINS/company/1.2.0/skills/release}
prompts:
  - {file: ./a.md}
  - {runtime: identity}
  - {runtime: workdir}
  - {file: ./b.md}
`)
	got, err := ParseV2(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []PromptEntry{{File: "./a.md"}, {Runtime: "identity"}, {Runtime: "workdir"}, {File: "./b.md"}}
	if len(got.Prompts) != len(want) {
		t.Fatalf("prompts = %#v", got.Prompts)
	}
	for i := range want {
		if got.Prompts[i] != want[i] {
			t.Fatalf("prompts[%d] = %#v, want %#v", i, got.Prompts[i], want[i])
		}
	}
	wantSkills := []SkillEntry{{Dir: "./skills/review"}, {Dir: "$PLUGINS/company/1.2.0/skills/release"}}
	if !reflect.DeepEqual(got.Skills, wantSkills) {
		t.Fatalf("skills = %#v, want %#v", got.Skills, wantSkills)
	}
}

func TestParseV2AcceptsOnlySkillDirectoryObjects(t *testing.T) {
	dir := writeV2Source(t, `schema_version: 2
plugins: []
skills:
  - dir: ./skills/review
  - dir: $PLUGINS/company/1.2.0/skills/release
prompts: []
`)
	got, err := ParseV2(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []SkillEntry{{Dir: "./skills/review"}, {Dir: "$PLUGINS/company/1.2.0/skills/release"}}
	if !reflect.DeepEqual(got.Skills, want) {
		t.Fatalf("skills = %#v, want %#v", got.Skills, want)
	}
}

func TestParseV2RejectsInvalidSkillEntries(t *testing.T) {
	for _, body := range []string{
		"skills: [./skills/review]",
		"skills: [{}]",
		"skills: [{dir: ''}]",
		"skills: [{dir: ./skills/review, name: review}]",
	} {
		_, err := ParseV2(writeV2Source(t, "schema_version: 2\nplugins: []\nprompts: []\n"+body+"\n"))
		if err == nil {
			t.Fatalf("accepted invalid source:\n%s", body)
		}
	}
}

func TestParseV2RejectsRemovedAndAmbiguousFields(t *testing.T) {
	tests := map[string]string{
		"from": "from: basic:latest", "harness": "harness: {type: codex}",
		"env": "env: {A: B}", "policy": "policy: {}", "secrets": "requires_secrets: [TOKEN]",
		"evals": "evals: []", "unknown": "mystery: true",
	}
	for name, field := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseV2(writeV2Source(t, "schema_version: 2\nplugins: []\nprompts: []\n"+field+"\n"))
			if err == nil {
				t.Fatalf("accepted %s", field)
			}
		})
	}
	bad := []string{
		"plugins: [{name: x}, {name: x}]\nprompts: []",
		"plugins: []\nprompts: [{file: ./a, runtime: context}]",
		"plugins: []\nprompts: [{}]",
		"plugins: []\nprompts: [{runtime: foreign}]",
		"plugins: []\nprompts: [{runtime: context}, {runtime: context}]",
		"plugins: []\nprompts: [{runtime: workdir}, {runtime: workdir}]",
		"plugins: [{name: x, version: 1}]\nprompts: []",
		"plugins: []\nprompts: []\n---\nschema_version: 2\nplugins: []\nprompts: []",
	}
	for _, body := range bad {
		if _, err := ParseV2(writeV2Source(t, "schema_version: 2\n"+body+"\n")); err == nil {
			t.Fatalf("accepted invalid source:\n%s", body)
		}
	}
}

func TestParseV2RejectsV1WithMigrationMessage(t *testing.T) {
	_, err := ParseV2(writeV2Source(t, "schema_version: 1\nharness: {type: claude}\n"))
	if err == nil || err.Error() != SchemaV1MigrationMessage {
		t.Fatalf("ParseV2 schema 1 error = %v, want %q", err, SchemaV1MigrationMessage)
	}
}

func TestParseV2GoalRuntime(t *testing.T) {
	parsed, err := ParseV2(writeV2Source(t, "schema_version: 2\nplugins: []\nprompts: [{runtime: goal}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []PromptEntry{{Runtime: "goal"}}; !reflect.DeepEqual(parsed.Prompts, want) {
		t.Fatalf("prompts = %#v, want %#v", parsed.Prompts, want)
	}

	_, err = ParseV2(writeV2Source(t, "schema_version: 2\nplugins: []\nprompts: [{runtime: goal}, {runtime: goal}]\n"))
	if err == nil || !strings.Contains(err.Error(), `duplicate runtime placeholder "goal"`) {
		t.Fatalf("duplicate goal error = %v", err)
	}
}
