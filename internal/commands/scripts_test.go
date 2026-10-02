package commands

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/store"
)

type fakeScriptControl struct {
	definitions map[string]script.Definition
	runs        map[string][]script.Run
}

func (f *fakeScriptControl) RunOnce(owner string, in script.CreateOnce) (script.Definition, script.Run, error) {
	definition := script.Definition{ID: "scr-1", Agent: owner, Name: in.Name, Description: in.Description, Command: in.Command, Mode: script.ModeOnce, State: script.StateActive}
	run := script.Run{ID: "srun-1", ScriptID: definition.ID, Agent: owner, Status: script.RunPending}
	definition.LatestRun = &run
	f.definitions[definition.ID], f.runs[definition.ID] = definition, []script.Run{run}
	return definition, run, nil
}

func (f *fakeScriptControl) ScheduleScript(owner string, in script.CreateSchedule) (script.Definition, script.Run, error) {
	if in.IntervalSeconds <= 0 {
		return script.Definition{}, script.Run{}, errors.New("recurring interval must be positive")
	}
	definition := script.Definition{ID: "scr-2", Agent: owner, Name: in.Name, Description: in.Description, Command: in.Command, Mode: script.ModeEvery, IntervalSeconds: in.IntervalSeconds, State: script.StateActive}
	run := script.Run{ID: "srun-2", ScriptID: definition.ID, Agent: owner, Status: script.RunPending}
	f.definitions[definition.ID], f.runs[definition.ID] = definition, []script.Run{run}
	return definition, run, nil
}

func (f *fakeScriptControl) RerunScript(owner, id string) (script.Run, error) {
	if _, ok := f.definitions[id]; !ok {
		return script.Run{}, script.ErrNotFound
	}
	run := script.Run{ID: "srun-rerun", ScriptID: id, Agent: owner, Status: script.RunPending}
	f.runs[id] = append([]script.Run{run}, f.runs[id]...)
	return run, nil
}
func (f *fakeScriptControl) ListScripts(owner string) ([]script.Definition, error) {
	out := make([]script.Definition, 0, len(f.definitions))
	for _, definition := range f.definitions {
		if definition.Agent == owner {
			out = append(out, definition)
		}
	}
	return out, nil
}
func (f *fakeScriptControl) ListScriptRuns(owner, id string) ([]script.Run, error) {
	if _, ok := f.definitions[id]; !ok {
		return nil, script.ErrNotFound
	}
	return f.runs[id], nil
}
func (f *fakeScriptControl) GetScriptRun(owner, id string) (script.Run, error) {
	for _, runs := range f.runs {
		for _, run := range runs {
			if run.Agent == owner && run.ID == id {
				return run, nil
			}
		}
	}
	return script.Run{}, script.ErrNotFound
}
func (f *fakeScriptControl) LogScriptRun(owner, id string) (string, error) {
	if _, err := f.GetScriptRun(owner, id); err != nil {
		return "", err
	}
	return "log", nil
}
func (f *fakeScriptControl) OpenScriptLog(owner, id string) (io.ReadCloser, string, error) {
	if _, err := f.GetScriptRun(owner, id); err != nil {
		return nil, "", err
	}
	return io.NopCloser(bytes.NewReader([]byte("log"))), "run.log", nil
}
func (f *fakeScriptControl) CancelScriptTarget(owner, id string) error { return nil }
func (f *fakeScriptControl) RemoveScript(owner, id string) error {
	definition, ok := f.definitions[id]
	if !ok || definition.Agent != owner {
		return script.ErrNotFound
	}
	if definition.State == script.StateActive {
		return script.ErrActive
	}
	delete(f.definitions, id)
	return nil
}

func TestScriptCommandsExposeDefinitionsAndRuns(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "commands.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := agent.NewStore(db).Create(agent.Agent{Name: "alice", ImageRef: "basic:latest"}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeScriptControl{definitions: map[string]script.Definition{}, runs: map[string][]script.Run{}}
	ctx := &registry.Ctx{Store: db, Scripts: fake}
	reg := BuildRegistry()
	for _, path := range []string{"script.ls", "script.run", "script.schedule", "script.rerun", "script.runs", "script.run-get", "script.logs", "script.cancel", "script.rm"} {
		if _, ok := reg.Get(path); !ok {
			t.Fatalf("missing command %s", path)
		}
	}
	runCommand, _ := reg.Get("script.run")
	result, err := runCommand.Handler(ctx, registry.Params{"name": "alice", "script_name": "check", "description": "check repo", "command": "make check"})
	if err != nil {
		t.Fatal(err)
	}
	created := result.(map[string]any)
	if created["script"].(map[string]any)["mode"] != script.ModeOnce || created["run"].(map[string]any)["status"] != script.RunPending {
		t.Fatalf("created=%#v", created)
	}
	runsCommand, _ := reg.Get("script.runs")
	result, err = runsCommand.Handler(ctx, registry.Params{"name": "alice", "id": "scr-1"})
	if err != nil || result.(map[string]any)["count"] != 1 {
		t.Fatalf("runs=%#v err=%v", result, err)
	}
}
