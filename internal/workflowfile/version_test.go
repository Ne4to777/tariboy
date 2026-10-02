package workflowfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeVersionSource(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DefaultFilename), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestVersionUpdate(t *testing.T) {
	for _, tc := range []struct{ part, want string }{{"major", "2.0.0"}, {"minor", "1.3.0"}, {"patch", "1.2.4"}} {
		t.Run(tc.part, func(t *testing.T) {
			dir := writeVersionSource(t, "# workflow\nschema_version: 1\nname: dev\nworkflow_version: '1.2.3-rc.1+build.7' # release\ninitial_status: todo\n")
			path := filepath.Join(dir, DefaultFilename)
			got, err := UpdateVersion(path, tc.part)
			if err != nil || got != tc.want {
				t.Fatalf("update = %q, %v", got, err)
			}
			got, err = GetVersion(dir)
			if err != nil || got != tc.want {
				t.Fatalf("get = %q, %v", got, err)
			}
			body, _ := os.ReadFile(path)
			for _, keep := range []string{"# workflow", "# release", "initial_status: todo"} {
				if !strings.Contains(string(body), keep) {
					t.Fatalf("lost %q: %s", keep, body)
				}
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("permissions changed: %v, %v", info, err)
			}
		})
	}
}

func TestVersionErrorsLeaveSourceUnchanged(t *testing.T) {
	for _, body := range []string{
		"schema_version: 1\n",
		"workflow_version: garbage\n",
		"workflow_version: 1.2.3\nworkflow_version: 2.3.4\n",
		"workflow_version: 1.2.3\n---\nother: document\n",
		"image_version: 1.2.3\n",
	} {
		dir := writeVersionSource(t, body)
		_, err := UpdateVersion(dir, "patch")
		if err == nil {
			t.Errorf("accepted %q", body)
		} else if strings.Contains(err.Error(), "image_version") || strings.Contains(err.Error(), "Tariboyfile") {
			t.Errorf("error names the image file: %v", err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, DefaultFilename))
		if string(got) != body {
			t.Fatalf("changed invalid source: %s", got)
		}
	}
	dir := writeVersionSource(t, "workflow_version: 1.2.3\n")
	if _, err := UpdateVersion(dir, "bogus"); err == nil {
		t.Fatal("accepted bogus part")
	}
}
