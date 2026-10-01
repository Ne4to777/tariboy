package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/tasks"
)

type archiveImportCaller struct {
	*fakeCaller
	uploaded []byte
}

func (c *archiveImportCaller) Upload(_ string, body io.Reader, _ int64) (json.RawMessage, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	c.uploaded = data
	return json.RawMessage(`{"import_id":"import-1","team":"dev"}`), nil
}

func TestComposeArchiveWritesPortableTeamArchive(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"manager", "worker"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "Tariboyfile.yaml"), []byte("schema_version: 2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	composePath := filepath.Join(dir, "tariboy-compose.yaml")
	if err := os.WriteFile(composePath, []byte("version: 1\nimages:\n  manager: {context: ./manager}\n  worker: {context: ./worker}\ngroups:\n  dev: {lead: manager}\nagents:\n  manager: {image: manager:latest, group: dev}\n  worker: {image: worker:latest, group: dev}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "dev.tar.gz")
	var stderr bytes.Buffer
	if code := Main(context.Background(), newFake(), "", []string{"archive", "-f", composePath, "--output", output}, io.Discard, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if info, err := os.Stat(output); err != nil || info.Size() == 0 {
		t.Fatalf("archive stat=%v err=%v", info, err)
	}
}

func TestComposeImportYesUploadsThenAppliesPreview(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "team.tar.gz")
	if err := os.WriteFile(archivePath, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	caller := &archiveImportCaller{fakeCaller: newFake()}
	var stderr bytes.Buffer
	if code := Main(context.Background(), caller, "", []string{"import", "--archive", archivePath, "--yes"}, io.Discard, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if string(caller.uploaded) != "archive" {
		t.Fatalf("uploaded = %q", caller.uploaded)
	}
	if countCalls(caller.fakeCaller, "POST /api/team-imports/import-1/apply") != 1 {
		t.Fatalf("calls = %v", caller.calls)
	}
}

func TestComposeStatusPrintsWorkflowAndPoolDrift(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workflow.yaml"), []byte(`name: development
version: 1
initial_status: work
statuses:
  - id: work
    requirements:
      - {id: implement, pool: developers, dispatch: claim_one, outcomes: [completed]}
    transitions: [{to: done}]
  - {id: done, terminal: true, requirements: [], transitions: []}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(dir, "tariboy-compose.yaml")
	if err := os.WriteFile(composePath, []byte(`version: 1
workflows:
  development: {source: ./workflow.yaml}
task_queues:
  DEV:
    name: Development
    workflow: development
    pools: {developers: [dev]}
agents:
  dev: {image: basic:latest}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	fc := newWorkflowComposeCaller()
	fc.agents["dev"] = map[string]any{"name": "dev", "state": "running", "image": "basic:latest", "cwd": dir}
	fc.queues["DEV"] = tasks.Queue{Prefix: "DEV", Name: "Development", Revision: 1}
	fc.bindings["DEV"] = tasks.QueueWorkflowBinding{Queue: "DEV", WorkflowName: "development", WorkflowVersion: 2, Revision: 1}
	fc.pools["DEV"] = map[string]tasks.AgentPool{"developers": {Queue: "DEV", Name: "developers", Agents: []string{"other"}, Revision: 1}}
	var out strings.Builder
	var errOut strings.Builder
	if code := Main(context.Background(), fc, "", []string{"status", "-f", composePath}, &out, &errOut); code != 0 {
		t.Fatalf("Main code=%d stderr=%s", code, errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "workflow drift") || !strings.Contains(got, "pool developers drift") {
		t.Fatalf("status output:\n%s", got)
	}
}

func TestLifecycleCommandsDoNotLoadMissingWorkflowSource(t *testing.T) {
	for _, verb := range []string{"build", "down", "stop", "kill", "rm", "logs"} {
		t.Run(verb, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "tariboy-compose.yaml")
			if err := os.WriteFile(path, []byte(`version: 1
workflows:
  development: {source: ./missing-workflow.yaml}
agents:
  dev: {image: basic:latest}
`), 0o600); err != nil {
				t.Fatal(err)
			}
			fc := newFake()
			fc.agents["dev"] = map[string]any{"name": "dev", "state": "running", "image": "basic:latest"}
			var errOut strings.Builder
			args := []string{verb, "-f", path}
			if verb == "rm" {
				args = append(args, "dev")
			}
			if code := Main(context.Background(), fc, "", args, io.Discard, &errOut); code != 0 {
				t.Fatalf("%s code=%d stderr=%s", verb, code, errOut.String())
			}
		})
	}
}

func TestDownRemovesAgentsAndGroups(t *testing.T) {
	fc := newFake()
	fc.agents["scout"] = map[string]any{"name": "scout", "state": "running", "group": "research-team"}
	fc.agents["writer"] = map[string]any{"name": "writer", "state": "running", "group": "research-team"}
	fc.groups["research-team"] = map[string]any{"name": "research-team"}
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(goodYAML))
	if err := r.Down(f, true); err != nil {
		t.Fatal(err)
	}
	// Both agents force-deleted, group deleted with volumes.
	if countCalls(fc, "DELETE /api/agents/scout") != 1 || countCalls(fc, "DELETE /api/agents/writer") != 1 {
		t.Fatalf("agents not removed: %v", fc.calls)
	}
	// volumes mirrors onto agents as purge=true (full data delete).
	if body := bodyFor(fc, "DELETE /api/agents/scout"); !hasPurgeTrue(body) {
		t.Fatalf("expected purge=true in agent delete body when Down(f, true), got %#v", body)
	}
	if countCalls(fc, "DELETE /api/groups/research-team") != 1 {
		t.Fatalf("group not removed: %v", fc.calls)
	}
	if body := bodyFor(fc, "DELETE /api/groups/research-team"); !hasVolumesTrue(body) {
		t.Fatalf("expected volumes=true in group delete body when Down(f, true), got %#v", body)
	}
	for _, c := range fc.calls {
		if strings.Contains(c, "image") {
			t.Fatalf("Down touched an image route: %v", fc.calls)
		}
	}
}

// TestDownWithoutVolumesKeepsData proves the non-destructive default path:
// Down(f, false) must remove agents (force) and the group, but must NOT tell
// the daemon to remove group volumes. This is the discriminating counterpart
// to TestDownRemovesAgentsAndGroups: it fails if Down ever sends volumes=true
// unconditionally.
func TestDownWithoutVolumesKeepsData(t *testing.T) {
	fc := newFake()
	fc.agents["scout"] = map[string]any{"name": "scout", "state": "running", "group": "research-team"}
	fc.agents["writer"] = map[string]any{"name": "writer", "state": "running", "group": "research-team"}
	fc.groups["research-team"] = map[string]any{"name": "research-team"}
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(goodYAML))
	if err := r.Down(f, false); err != nil {
		t.Fatal(err)
	}
	// Both agents still force-deleted, group still removed — just without volumes.
	if countCalls(fc, "DELETE /api/agents/scout") != 1 || countCalls(fc, "DELETE /api/agents/writer") != 1 {
		t.Fatalf("agents not removed: %v", fc.calls)
	}
	// Data-preserving default: agents deleted WITHOUT purge, so their history stays.
	if body := bodyFor(fc, "DELETE /api/agents/scout"); hasPurgeTrue(body) {
		t.Fatalf("expected purge NOT true when Down(f, false), got %#v", body)
	}
	if countCalls(fc, "DELETE /api/groups/research-team") != 1 {
		t.Fatalf("group not removed: %v", fc.calls)
	}
	if body := bodyFor(fc, "DELETE /api/groups/research-team"); hasVolumesTrue(body) {
		t.Fatalf("expected volumes NOT true when Down(f, false), got %#v", body)
	}
	for _, c := range fc.calls {
		if strings.Contains(c, "image") {
			t.Fatalf("Down touched an image route: %v", fc.calls)
		}
	}
}

func writeComposeFile(t *testing.T, dir string) string {
	t.Helper()
	path := dir + "/tariboy-compose.yaml"
	if err := os.WriteFile(path, []byte(goodYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestComposeUpAutostartsDaemon(t *testing.T) {
	called := 0
	orig := ensureDaemonUp
	ensureDaemonUp = func(ctx context.Context, out io.Writer) error { called++; return nil }
	defer func() { ensureDaemonUp = orig }()

	path := writeComposeFile(t, t.TempDir())
	fc := newFake()
	// The empty fake makes r.Up fail afterward; the assertion under test is that
	// the auto-start seam ran (once) before the file was applied.
	_ = Main(context.Background(), fc, t.TempDir(), []string{"up", "-f", path}, io.Discard, io.Discard)
	if called != 1 {
		t.Fatalf("EnsureUp called %d times, want 1", called)
	}
}

func TestComposeUpNoStartSkipsAutostart(t *testing.T) {
	called := 0
	orig := ensureDaemonUp
	ensureDaemonUp = func(ctx context.Context, out io.Writer) error { called++; return nil }
	defer func() { ensureDaemonUp = orig }()

	path := writeComposeFile(t, t.TempDir())
	fc := newFake()
	_ = Main(context.Background(), fc, t.TempDir(), []string{"up", "--no-start", "-f", path}, io.Discard, io.Discard)
	if called != 0 {
		t.Fatalf("EnsureUp called %d times with --no-start, want 0", called)
	}
}

// TestUpReprovisionsStoppedAgent proves the up-side of option B: an agent that
// still has its DB row but is stopped (after a data-preserving down) is
// re-provisioned in place with the file's image, not recreated — so its
// CONTEXT.md / iterations / audit survive an image swap.
func TestUpReprovisionsStoppedAgent(t *testing.T) {
	fc := newFake()
	fc.groups["research-team"] = map[string]any{"name": "research-team", "lead": "scout"}
	fc.agents["scout"] = map[string]any{"name": "scout", "state": "stopped",
		"group": "research-team", "image": "analyst:latest", "loop_enabled": false}
	fc.agents["writer"] = map[string]any{"name": "writer", "state": "stopped",
		"group": "research-team", "image": "analyst:latest", "loop_enabled": false}
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(goodYAML))
	upNoBuild(t, r, f)

	if countCalls(fc, "POST /api/agents/scout/reprovision") != 1 {
		t.Fatalf("stopped agent scout was not reprovisioned: %v", fc.calls)
	}
	if body := bodyFor(fc, "POST /api/agents/scout/reprovision"); body == nil ||
		body.(map[string]any)["image"] != "analyst:latest" {
		t.Fatalf("reprovision image wrong: %#v", body)
	}
	// It must NOT be recreated — the row already exists.
	if countCalls(fc, "POST /api/agents") != 0 {
		t.Fatalf("stopped agent was recreated instead of reprovisioned: %v", fc.calls)
	}
	// Final loop state must match a first-ever create of the same file. goodYAML
	// declares no loop block for scout/writer, and create defaults loop:true, so
	// after reprovision-by-up both must be loop-enabled (not left disabled from
	// the stopped row, nor forced/converged to anything else).
	for _, name := range []string{"scout", "writer"} {
		if got := fc.agents[name]["loop_enabled"]; got != true {
			t.Fatalf("agent %s loop_enabled = %v after up, want true (== create default)", name, got)
		}
	}
}

// TestUpDoesNotReprovisionRunningAgent is the discriminating counterpart: a
// converged, running agent must not be reprovisioned on a plain `up`.
func TestUpDoesNotReprovisionRunningAgent(t *testing.T) {
	fc := newFake()
	fc.groups["research-team"] = map[string]any{"name": "research-team", "lead": "scout"}
	fc.agents["scout"] = map[string]any{"name": "scout", "state": "running",
		"group": "research-team", "image": "analyst:latest", "loop_enabled": true}
	fc.agents["writer"] = map[string]any{"name": "writer", "state": "running",
		"group": "research-team", "image": "analyst:latest", "loop_enabled": true}
	r := NewRunner(fc, "", "", io.Discard)
	f, _ := Parse([]byte(goodYAML))
	upNoBuild(t, r, f)

	if n := countCalls(fc, "POST /api/agents/scout/reprovision"); n != 0 {
		t.Fatalf("running agent scout was reprovisioned %d times: %v", n, fc.calls)
	}
}
