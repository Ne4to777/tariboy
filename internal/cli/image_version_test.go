package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alekzonder/tariboy/internal/commands"
	"github.com/alekzonder/tariboy/internal/registry"
)

func TestImageVersionLocalCLI(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "Tariboyfile.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 2\nimage_version: 1.2.3\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"get"}, "1.2.3\n"},
		{[]string{"update", "patch", "--path", path}, "1.2.4\n"},
		{[]string{"update", "minor", "--path", dir}, "1.3.0\n"},
		{[]string{"update", "major"}, "2.0.0\n"},
	} {
		var out, errOut bytes.Buffer
		code := Run(context.Background(), commands.BuildRegistry(), append([]string{"image", "version"}, tc.args...), nil, &registry.Ctx{}, &out, &errOut)
		if code != 0 || out.String() != tc.want {
			t.Fatalf("%v: code %d, out %q, err %s", tc.args, code, out.String(), &errOut)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("permissions changed: %v, %v", info, err)
	}
}

func TestWorkflowVersionLocalCLI(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "Workflowfile.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\nworkflow_version: 1.2.3\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"get"}, "1.2.3\n"},
		{[]string{"update", "patch", "--path", path}, "1.2.4\n"},
		{[]string{"update", "minor", "--path", dir}, "1.3.0\n"},
		{[]string{"update", "major"}, "2.0.0\n"},
	} {
		var out, errOut bytes.Buffer
		code := Run(context.Background(), commands.BuildRegistry(), append([]string{"workflow", "version"}, tc.args...), nil, &registry.Ctx{}, &out, &errOut)
		if code != 0 || out.String() != tc.want {
			t.Fatalf("%v: code %d, out %q, err %s", tc.args, code, out.String(), &errOut)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("permissions changed: %v, %v", info, err)
	}
}

func TestImageBuildLeavesDefaultTagToDaemon(t *testing.T) {
	call := &fakeCaller{result: []byte(`{}`)}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), commands.BuildRegistry(), []string{"image", "build", "--name", "test", "--path", "."}, call, nil, &out, &errOut)
	if code != 0 {
		t.Fatal(&errOut)
	}
	if tag := call.body.(registry.Params)["tag"]; tag != nil {
		t.Fatalf("CLI supplied default tag: %#v", tag)
	}
}
