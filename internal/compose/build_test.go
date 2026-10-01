package compose

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildPublishesDeclaredImagesThroughDaemon(t *testing.T) {
	workdir := t.TempDir()
	ctxDir := filepath.Join(workdir, "analyst")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tariboyfile := "schema_version: 2\n"
	if err := os.WriteFile(filepath.Join(ctxDir, "Tariboyfile.yaml"), []byte(tariboyfile), 0o600); err != nil {
		t.Fatal(err)
	}
	imagesDir := t.TempDir()

	fc := newFake()
	r := NewRunner(fc, imagesDir, workdir, io.Discard)
	f, err := Parse([]byte(goodYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Build(f); err != nil {
		t.Fatalf("build: %v", err)
	}

	if countCalls(fc, "POST /api/images/build") != 1 {
		t.Fatalf("build did not publish through daemon: %v", fc.calls)
	}
	body, ok := bodyFor(fc, "POST /api/images/build").(map[string]any)
	if !ok || body["name"] != "analyst" || body["tag"] != "latest" || body["path"] != ctxDir {
		t.Fatalf("image build body = %#v", body)
	}
	if entries, err := os.ReadDir(imagesDir); err != nil || len(entries) != 0 {
		t.Fatalf("compose wrote shared image store directly: entries=%v err=%v", entries, err)
	}
}
