package commands

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

const workflowTestManifest = `schema_version: 1
name: demo
workflow_version: VERSION
initial_status: work
statuses:
  - id: work
    owner: { pool: devs }
    instructions: ./statuses/work.md
    transitions:
      - on: done
        to: finished
        checks:
          - script: ./scripts/check.sh
  - id: finished
    terminal: true
`

func writeWorkflowSource(t *testing.T, dir, version string) string {
	t.Helper()
	put := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	put("Workflowfile.yaml", strings.ReplaceAll(workflowTestManifest, "VERSION", version), 0o644)
	put("statuses/work.md", "do the work\n", 0o644)
	put("scripts/check.sh", "#!/bin/sh\nexit 0\n", 0o755)
	return dir
}

// workflowCtx returns a command context whose stored workflow trees are made
// writable again before cleanup.
func workflowCtx(t *testing.T) *registry.Ctx {
	t.Helper()
	c := localCtx(t)
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(c.BaseDir, "workflows"), func(p string, d os.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	return c
}

func userError(t *testing.T, err error, code string, status int) api.UserError {
	t.Helper()
	var ue api.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want UserError %s", err, code)
	}
	if ue.Code != code || (ue.Status != status && !(status == http.StatusBadRequest && ue.Status == 0)) {
		t.Fatalf("err = %#v, want %s/%d", ue, code, status)
	}
	return ue
}

func TestWorkflowImageCommandsRegisteredWithRoutes(t *testing.T) {
	want := map[string]registry.HTTPRoute{
		"workflow.validate": {Method: http.MethodPost, Path: "/api/workflow-images/validate"},
		"workflow.build":    {Method: http.MethodPost, Path: "/api/workflow-images/build"},
		"workflow.ls":       {Method: http.MethodGet, Path: "/api/workflow-images"},
		"workflow.inspect":  {Method: http.MethodGet, Path: "/api/workflow-images/{name}/{tag}"},
		"workflow.rm":       {Method: http.MethodDelete, Path: "/api/workflow-images/{name}/{tag}"},
	}
	reg := BuildRegistry()
	for path, route := range want {
		command, ok := reg.Get(path)
		if !ok {
			t.Fatalf("missing %s", path)
		}
		if command.HTTP == nil || *command.HTTP != route {
			t.Fatalf("%s route = %#v, want %#v", path, command.HTTP, route)
		}
	}
	if _, ok := reg.Group("workflow"); !ok {
		t.Fatal("missing workflow group")
	}
}

func TestWorkflowValidateReportsResultWithoutWriting(t *testing.T) {
	c := workflowCtx(t)
	src := writeWorkflowSource(t, t.TempDir(), "1.0.0")
	got, err := cmdHandler(t, "workflow.validate")(c, registry.Params{"path": src})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"valid": true, "name": "demo", "version": "1.0.0",
		"pools": []any{"devs"}, "files": []any{"scripts/check.sh", "statuses/work.md"}, "errors": []any{},
	}
	if obj := jsonObject(t, got); !reflect.DeepEqual(obj, want) {
		t.Fatalf("validate = %#v, want %#v", obj, want)
	}
	if _, err := os.Stat(filepath.Join(c.BaseDir, "workflows")); !os.IsNotExist(err) {
		t.Fatalf("validate wrote the workflows directory: %v", err)
	}

	if err := os.Remove(filepath.Join(src, "statuses", "work.md")); err != nil {
		t.Fatal(err)
	}
	got, err = cmdHandler(t, "workflow.validate")(c, registry.Params{"path": filepath.Join(src, "Workflowfile.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	obj := jsonObject(t, got)
	if obj["valid"] != false || len(obj["errors"].([]any)) == 0 {
		t.Fatalf("validate = %#v", obj)
	}
}

// TestWorkflowValidateReportsWhatBuildRefuses pins that validate runs the
// same validation as build: every source build refuses, validate reports.
func TestWorkflowValidateReportsWhatBuildRefuses(t *testing.T) {
	cases := map[string]func(t *testing.T, src string){
		"transition target names no status": func(t *testing.T, src string) {
			m := strings.Replace(strings.ReplaceAll(workflowTestManifest, "VERSION", "1.0.0"), "to: finished", "to: nowhere", 1)
			if err := os.WriteFile(filepath.Join(src, "Workflowfile.yaml"), []byte(m), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"symlink in the source": func(t *testing.T, src string) {
			if err := os.Symlink("work.md", filepath.Join(src, "statuses", "link.md")); err != nil {
				t.Fatal(err)
			}
		},
		"reserved manifest name": func(t *testing.T, src string) {
			if err := os.WriteFile(filepath.Join(src, "manifest.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, breakSource := range cases {
		t.Run(name, func(t *testing.T) {
			c := workflowCtx(t)
			src := writeWorkflowSource(t, t.TempDir(), "1.0.0")
			breakSource(t, src)
			_, buildErr := cmdHandler(t, "workflow.build")(c, registry.Params{"path": src})
			userError(t, buildErr, "workflow_invalid", http.StatusBadRequest)
			got, err := cmdHandler(t, "workflow.validate")(c, registry.Params{"path": src})
			if err != nil {
				t.Fatal(err)
			}
			obj := jsonObject(t, got)
			errs, _ := obj["errors"].([]any)
			if obj["valid"] != false || len(errs) == 0 {
				t.Fatalf("validate = %#v; build refused with %v", obj, buildErr)
			}
		})
	}
}

func TestWorkflowValidateUnparsableManifestIsWorkflowInvalid(t *testing.T) {
	c := workflowCtx(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Workflowfile.yaml"), []byte("bogus: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := cmdHandler(t, "workflow.validate")(c, registry.Params{"path": dir})
	userError(t, err, "workflow_invalid", http.StatusBadRequest)
}

func TestWorkflowSourceSelection(t *testing.T) {
	c := workflowCtx(t)
	for _, path := range []string{"workflow.validate", "workflow.build"} {
		_, err := cmdHandler(t, path)(c, registry.Params{"source": "team/demo", "path": "/x"})
		userError(t, err, "bad_source", http.StatusBadRequest)
		_, err = cmdHandler(t, path)(c, registry.Params{})
		userError(t, err, "missing_path", http.StatusBadRequest)
	}
}

func TestWorkflowBuildFromStoreSelector(t *testing.T) {
	c := workflowCtx(t)
	root := t.TempDir()
	writeWorkflowSource(t, filepath.Join(root, "workflows", "demo"), "2.0.0")
	if _, err := cmdHandler(t, "store.add")(c, registry.Params{"name": "team", "source": root}); err != nil {
		t.Fatal(err)
	}
	got, err := cmdHandler(t, "workflow.build")(c, registry.Params{"source": "team/demo"})
	if err != nil {
		t.Fatal(err)
	}
	if obj := jsonObject(t, got); obj["name"] != "demo" || obj["version"] != "2.0.0" || obj["created"] != true {
		t.Fatalf("build = %#v", obj)
	}
	_, err = cmdHandler(t, "workflow.build")(c, registry.Params{"source": "team/missing"})
	if err == nil {
		t.Fatal("missing Store workflow was built")
	}
}

func TestWorkflowBuildLsInspectRm(t *testing.T) {
	c := workflowCtx(t)
	src := writeWorkflowSource(t, t.TempDir(), "1.0.0")

	built, err := cmdHandler(t, "workflow.build")(c, registry.Params{"path": src})
	if err != nil {
		t.Fatal(err)
	}
	obj := jsonObject(t, built)
	digest, _ := obj["digest"].(string)
	if len(digest) != 64 || obj["name"] != "demo" || obj["version"] != "1.0.0" || obj["created"] != true ||
		!reflect.DeepEqual(obj["tags"], []any{"1.0.0", "latest"}) {
		t.Fatalf("build = %#v", obj)
	}

	again, err := cmdHandler(t, "workflow.build")(c, registry.Params{"path": src})
	if err != nil {
		t.Fatal(err)
	}
	if obj := jsonObject(t, again); obj["created"] != false || obj["digest"] != digest {
		t.Fatalf("rebuild = %#v", obj)
	}

	listed, err := cmdHandler(t, "workflow.ls")(c, registry.Params{})
	if err != nil {
		t.Fatal(err)
	}
	lobj := jsonObject(t, listed)
	rows := lobj["workflows"].([]any)
	if lobj["count"] != float64(2) || len(rows) != 2 {
		t.Fatalf("ls = %#v", lobj)
	}
	first := rows[0].(map[string]any)
	if first["name"] != "demo" || first["tag"] != "1.0.0" || first["version"] != "1.0.0" || first["digest"] != digest || first["built_at"] == "" {
		t.Fatalf("row = %#v", first)
	}
	if rows[1].(map[string]any)["tag"] != "latest" {
		t.Fatalf("rows = %#v", rows)
	}

	for _, tag := range []string{"1.0.0", digest, ""} {
		got, err := cmdHandler(t, "workflow.inspect")(c, registry.Params{"name": "demo", "tag": tag})
		if err != nil {
			t.Fatalf("inspect %q: %v", tag, err)
		}
		if m, ok := got.(workflowimage.Manifest); !ok || m.Digest != digest {
			t.Fatalf("inspect %q = %#v", tag, got)
		}
	}
	_, err = cmdHandler(t, "workflow.inspect")(c, registry.Params{"name": "demo", "tag": "9.9.9"})
	userError(t, err, "not_found", http.StatusNotFound)

	removed, err := cmdHandler(t, "workflow.rm")(c, registry.Params{"name": "demo", "tag": "latest"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"name": "demo", "tag": "latest", "removed": true, "content_removed": false}
	if got := jsonObject(t, removed); !reflect.DeepEqual(got, want) {
		t.Fatalf("rm = %#v, want %#v", got, want)
	}
	removed, err = cmdHandler(t, "workflow.rm")(c, registry.Params{"name": "demo", "tag": "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if got := jsonObject(t, removed); got["content_removed"] != true {
		t.Fatalf("rm = %#v", got)
	}
	_, err = cmdHandler(t, "workflow.rm")(c, registry.Params{"name": "demo", "tag": "1.0.0"})
	userError(t, err, "not_found", http.StatusNotFound)
	listed, _ = cmdHandler(t, "workflow.ls")(c, registry.Params{})
	if lobj := jsonObject(t, listed); lobj["count"] != float64(0) || len(lobj["workflows"].([]any)) != 0 {
		t.Fatalf("ls = %#v", lobj)
	}
}

func TestWorkflowBuildErrors(t *testing.T) {
	c := workflowCtx(t)
	src := writeWorkflowSource(t, t.TempDir(), "1.0.0")
	if _, err := cmdHandler(t, "workflow.build")(c, registry.Params{"path": src}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "statuses", "work.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := cmdHandler(t, "workflow.build")(c, registry.Params{"path": src})
	userError(t, err, "workflow_version_published", http.StatusConflict)

	if err := os.Remove(filepath.Join(src, "scripts", "check.sh")); err != nil {
		t.Fatal(err)
	}
	_, err = cmdHandler(t, "workflow.build")(c, registry.Params{"path": src})
	ue := userError(t, err, "workflow_invalid", http.StatusBadRequest)
	if errs, ok := ue.Data["errors"].([]workflowfile.ValidationError); !ok || len(errs) == 0 {
		t.Fatalf("details = %#v", ue.Data)
	}
}
