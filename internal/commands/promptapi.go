package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/alekzonder/tariboy/internal/agentdir"
	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/loop"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/taskgoal"
)

func contextGet() registry.Command {
	return registry.Command{
		Path:    "context.get",
		Summary: "Read the agent's CONTEXT.md",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "agent name"}},
		HTTP:    &registry.HTTPRoute{Method: "GET", Path: "/api/agents/{name}/context"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			l := agentdir.New(agentsDir(c), a.Name)
			data, err := os.ReadFile(l.ContextPath())
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			return map[string]any{"name": a.Name, "context": string(data)}, nil
		},
	}
}

func contextSet() registry.Command {
	return registry.Command{
		Path:    "context.set",
		Summary: "Overwrite the agent's CONTEXT.md",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "agent name"},
			{Name: "text", Type: registry.String, Help: "new CONTEXT.md contents"},
		},
		HTTP: &registry.HTTPRoute{Method: "POST", Path: "/api/agents/{name}/context"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			l := agentdir.New(agentsDir(c), a.Name)
			if err := writeFileAtomic(l.ContextPath(), []byte(str(p, "text"))); err != nil {
				return nil, err
			}
			return map[string]any{"name": a.Name, "saved": true}, nil
		},
	}
}

// writeFileAtomic mirrors agentapi's writeContextAtomic: write to a temp file
// in the same directory, then rename over the destination so readers never
// see a partial write.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".context-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func promptGet() registry.Command {
	return registry.Command{
		Path:    "prompt.get",
		Summary: "Preview the agent's assembled next-iteration prompt",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "agent name"}},
		HTTP:    &registry.HTTPRoute{Method: "GET", Path: "/api/agents/{name}/prompt"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			a, err := getAgent(c, str(p, "name"))
			if err != nil {
				return nil, err
			}
			l := agentdir.New(agentsDir(c), a.Name)
			man, err := readManifest(filepath.Join(l.ImageDir(), "manifest.json"))
			if err != nil {
				return nil, api.UserError{Code: "not_provisioned", Msg: "agent " + a.Name + " has no provisioned image: " + err.Error()}
			}
			contextText, err := os.ReadFile(l.ContextPath())
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}

			cwd := a.Cwd
			if cwd == "" {
				cwd = l.Workdir()
			}
			var prompt string
			var layers any = man.Layers
			goal := ""
			if a.ImageRef != image.BareRef.String() {
				if task, ok, err := taskgoal.NewStore(c.Store).Current(a.Name, time.Now().UTC()); err != nil {
					return nil, err
				} else if ok {
					goal = loop.RuntimeGoal(registry.RequestContext(p), workflowGoals(c), task, a.Name, c.Log)
				}
			}
			if a.ImageRef == image.BareRef.String() {
				layers = []image.TemplateEntry{}
			} else if man.SchemaVersion == 2 {
				data, err := os.ReadFile(filepath.Join(l.ImageDir(), "prompt", "template.json"))
				if err != nil {
					return nil, api.UserError{Code: "not_provisioned", Msg: err.Error()}
				}
				var template image.PromptTemplate
				if err := json.Unmarshal(data, &template); err != nil {
					return nil, err
				}
				prompt, err = loop.RenderPromptTemplate(template, l.ImageDir(), loop.RuntimePromptValues{
					Identity: loop.FormatRuntimeIdentity(a.Name, a.ImageRef, man.ImageVersion, a.ImageDigest, cwd, ""), Goal: goal, Context: string(contextText),
					Messages: "[runtime: messages]", UserPrompt: a.UserPrompt, OneShot: "[runtime: one-shot]",
				})
				if err != nil {
					return nil, err
				}
				layers = template.Entries
			} else {
				imagePrompt, err := os.ReadFile(filepath.Join(l.ImageDir(), "PROMPT.md"))
				if err != nil {
					return nil, api.UserError{Code: "not_provisioned", Msg: err.Error()}
				}
				tail, _ := os.ReadFile(filepath.Join(l.ImageDir(), "PROMPT_TAIL.md"))
				prompt = loop.AssemblePrompt(loop.PromptParts{Agent: a.Name, Cwd: cwd, ImagePrompt: string(imagePrompt), Context: string(contextText), UserPrompt: a.UserPrompt, Goal: goal, Messages: "[runtime: messages]", OneShot: "[runtime: one-shot]", Tail: string(tail)})
			}

			out := map[string]any{"name": a.Name, "prompt": prompt, "layers": layers}
			return out, nil
		},
	}
}

func readManifest(path string) (image.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return image.Manifest{}, err
	}
	var man image.Manifest
	if err := json.Unmarshal(data, &man); err != nil {
		return image.Manifest{}, err
	}
	return man, nil
}

// workflowGoals is the source of workflow data for the prompt preview; nil
// when the daemon has no Tasks service, which renders the flexible text.
func workflowGoals(c *registry.Ctx) loop.WorkflowGoalSource {
	if c.Tasks == nil {
		return nil
	}
	return loop.TaskWorkflowGoals{Tasks: c.Tasks, Images: c.WorkflowImages, Log: c.Log}
}
