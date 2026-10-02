package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/agentdir"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

func TestPromptPreviewShowsTheWorkflowGoalBlock(t *testing.T) {
	c, as, _ := ctxWithStore(t)
	if err := as.Create(agent.Agent{Name: "v2", Enabled: true, LoopEnabled: true, GoalEnabled: true, OnTimeout: "restart", OnError: "restart"}); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for rel, body := range map[string]string{
		"Workflowfile.yaml": "schema_version: 1\nname: demo\nworkflow_version: 0.1.0\ninitial_status: work\nstatuses:\n  - id: work\n    owner: { pool: devs }\n    instructions: ./statuses/work.md\n    transitions:\n      - on: done\n        to: finished\n  - id: finished\n    terminal: true\n",
		"statuses/work.md":  "Do the work.\n",
	} {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file, err := workflowfile.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	images := &workflowimage.Store{Dir: filepath.Join(t.TempDir(), "workflows")}
	t.Cleanup(func() {
		_ = filepath.WalkDir(images.Dir, func(p string, _ os.DirEntry, err error) error {
			if err == nil {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	if _, _, err := (&workflowimage.Registry{Store: images, DB: c.Store.DB}).Publish(file, time.Now()); err != nil {
		t.Fatal(err)
	}
	svc := tasks.NewService(c.Store.DB, "customer", time.Now)
	svc.SetWorkflowResolver(func(string) (string, error) { return images.Resolve("demo", "latest") })
	ctx, customer := context.Background(), tasks.CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, tasks.CreateQueueInput{Prefix: "DEV", Name: "Development"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RebindAgentPool(ctx, customer, "DEV", "devs", []string{"v2"}, 0, "pool"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetQueueWorkflow(ctx, customer, "DEV", "demo:latest", 0); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, customer, tasks.CreateTaskInput{Queue: "DEV", Title: "Preview", Description: "body"})
	if err != nil {
		t.Fatal(err)
	}
	c.Tasks, c.WorkflowImages = svc, images

	l := agentdir.New(agentsDir(c), "v2")
	if err := os.MkdirAll(filepath.Join(l.ImageDir(), "prompt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.ImageDir(), "manifest.json"), []byte(`{"schema_version":2,"name":"x","tag":"latest"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := []image.TemplateEntry{{Kind: "runtime", Runtime: "goal"}}
	templateSHA, err := image.PromptTemplateHash(entries)
	if err != nil {
		t.Fatal(err)
	}
	template, err := json.Marshal(image.PromptTemplate{SchemaVersion: 2, Entries: entries, SHA256: templateSHA})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.ImageDir(), "prompt", "template.json"), template, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := h(t, "prompt.get")(c, registry.Params{"name": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	prompt := res.(map[string]any)["prompt"].(string)
	for _, want := range []string{"## Goal", "This task follows a workflow.", "key: " + task.Key + "\n", "### Status instructions\n\nDo the work.\n", "ttasks advance " + task.Key + " --outcome NAME --from work"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q in %s", want, prompt)
		}
	}
}
