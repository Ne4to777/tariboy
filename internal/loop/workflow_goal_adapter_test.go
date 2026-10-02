package loop

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

const adapterWorkflow = `schema_version: 1
name: demo
workflow_version: 0.1.0
initial_status: work
artifacts:
  - name: plan
statuses:
  - id: work
    owner: { pool: devs }
    instructions: ./statuses/work.md
    transitions:
      - on: done
        to: finished
        requires: [plan]
  - id: finished
    terminal: true
`

// workflowGoalFixture publishes a one-status workflow into a real image store,
// binds queue DEV to it, and returns a task held by agent dev-1.
func workflowGoalFixture(t *testing.T) (*tasks.Service, *workflowimage.Store, tasks.Task) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := tasks.NewService(st.DB, "customer", func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) })
	if _, err := st.DB.Exec(`INSERT INTO agents(name, image_ref, image_digest, enabled, loop_enabled, goal_enabled) VALUES ('dev-1', 'basic:latest', 'sha', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for rel, content := range map[string]string{"Workflowfile.yaml": adapterWorkflow, "statuses/work.md": "Do the work.\n"} {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file, err := workflowfile.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	images := &workflowimage.Store{Dir: filepath.Join(t.TempDir(), "workflows")}
	t.Cleanup(func() { makeTreeWritable(images.Dir) })
	if _, _, err := (&workflowimage.Registry{Store: images, DB: st.DB}).Publish(file, time.Now()); err != nil {
		t.Fatal(err)
	}
	svc.SetWorkflowResolver(func(ref string) (string, error) { return images.Resolve("demo", "latest") })
	ctx, customer := context.Background(), tasks.CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, tasks.CreateQueueInput{Prefix: "DEV", Name: "Development"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RebindAgentPool(ctx, customer, "DEV", "devs", []string{"dev-1"}, 0, "pool"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetQueueWorkflow(ctx, customer, "DEV", "demo:latest", 0); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, customer, tasks.CreateTaskInput{Queue: "DEV", Title: "Adapter", Description: "body"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, images, task
}

// makeTreeWritable undoes the read-only mode of a stored image tree so the
// test can remove a file and the temporary directory can be cleaned up.
func makeTreeWritable(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
}

func TestTaskWorkflowGoalsReadsTheViewAndTheInstructions(t *testing.T) {
	svc, images, task := workflowGoalFixture(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := TaskWorkflowGoals{Tasks: svc, Images: images, Log: log}
	ctx := context.Background()

	got := RuntimeGoal(ctx, src, task, "dev-1", log)
	for _, want := range []string{
		"workflow: demo@0.1.0\nstatus: work\ncategory: in_progress\n",
		"### Status instructions\n\nDo the work.\n",
		"- `done` -> `finished`; requires: plan; missing: plan\n",
		"ttasks advance " + task.Key + " --outcome NAME --from work --message \"TEXT\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}

	// The instructions file disappears: the block says so and the rest stays.
	view, err := svc.GetWorkflow(ctx, tasks.AgentActor("dev-1"), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	path, err := images.FilePath(view.Name, view.Digest, view.InstructionsPath)
	if err != nil {
		t.Fatal(err)
	}
	makeTreeWritable(images.Dir)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got = RuntimeGoal(ctx, src, task, "dev-1", log)
	if !strings.Contains(got, "### Status instructions\n\nThe status instructions could not be read.\n") || !strings.Contains(got, "### Commands") {
		t.Fatalf("missing file:\n%s", got)
	}

	// The holder's own question reads as its own.
	if _, err := svc.AddComment(ctx, tasks.AgentActor("dev-1"), task.Key, tasks.AddCommentInput{Body: "@user:customer which API?"}); err != nil {
		t.Fatal(err)
	}
	if got := RuntimeGoal(ctx, src, task, "dev-1", log); !strings.Contains(got, "\n\nYour question to the customer is open.") {
		t.Fatalf("holder question:\n%s", got)
	}
}
