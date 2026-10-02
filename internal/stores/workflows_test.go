package stores

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeWorkflow(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, "workflows", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Workflowfile.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDetailListsWorkflowSources(t *testing.T) {
	base, source := t.TempDir(), t.TempDir()
	catalog, db := openCatalog(t, base)
	defer db.Close()
	writeWorkflow(t, source, "a", "schema_version: 1\nname: a\nworkflow_version: 1.2.3\n")
	writeWorkflow(t, source, "b", "schema_version: 1\nbogus: true\n")
	if _, err := catalog.Add(context.Background(), "team", source); err != nil {
		t.Fatal(err)
	}
	detail, err := catalog.Detail(context.Background(), "team")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Workflows) != 2 {
		t.Fatalf("workflows = %#v", detail.Workflows)
	}
	if got := detail.Workflows[0]; got.Name != "a" || got.Version != "1.2.3" || got.Error != "" {
		t.Fatalf("a = %#v", got)
	}
	if got := detail.Workflows[1]; got.Name != "b" || got.Version != "" || got.Error == "" {
		t.Fatalf("b = %#v", got)
	}
}

func TestDetailWithoutWorkflowsDirectoryHasEmptyList(t *testing.T) {
	base, source := t.TempDir(), t.TempDir()
	catalog, db := openCatalog(t, base)
	defer db.Close()
	if _, err := catalog.Add(context.Background(), "team", source); err != nil {
		t.Fatal(err)
	}
	detail, err := catalog.Detail(context.Background(), "team")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Workflows == nil || len(detail.Workflows) != 0 {
		t.Fatalf("workflows = %#v", detail.Workflows)
	}
	raw, _ := json.Marshal(detail)
	if !strings.Contains(string(raw), `"workflows":[]`) {
		t.Fatalf("json = %s", raw)
	}
}

func TestPrepareWorkflowBuild(t *testing.T) {
	base, source := t.TempDir(), t.TempDir()
	catalog, db := openCatalog(t, base)
	defer db.Close()
	dir := writeWorkflow(t, source, "a", "schema_version: 1\n")
	if _, err := catalog.Add(context.Background(), "team", source); err != nil {
		t.Fatal(err)
	}
	prepared, release, err := catalog.PrepareWorkflowBuild("team/a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if want := (PreparedBuild{Name: "a", Path: dir}); !reflect.DeepEqual(prepared, want) {
		t.Fatalf("prepared = %#v, want %#v", prepared, want)
	}
	for _, bad := range []string{"x/../y", "team", "team/a/b", "team/missing"} {
		if _, _, err := catalog.PrepareWorkflowBuild(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	if _, _, err := catalog.PrepareWorkflowBuild("x/../y"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	// The failed calls must have released the lock.
	if _, err := catalog.Detail(context.Background(), "team"); err != nil {
		t.Fatal(err)
	}
}
