package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/paths"
	"github.com/alekzonder/tariboy/internal/registry"
	storedb "github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

const workflowArchiveManifest = `schema_version: 1
name: demo
workflow_version: 1.0.0
initial_status: work
statuses:
  - id: work
    owner: { pool: devs }
    transitions:
      - on: done
        to: finished
        checks:
          - script: ./check.sh
  - id: finished
    terminal: true
`

func workflowArchiveServer(t *testing.T) (http.Handler, *workflowimage.Registry, string) {
	t.Helper()
	base := t.TempDir()
	db, err := storedb.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	dir := paths.Paths{Base: base}.WorkflowsDir()
	// The stored tree is read-only; make it removable for TempDir cleanup.
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	reg := &workflowimage.Registry{Store: &workflowimage.Store{Dir: dir}, DB: db.DB}
	cctx := &registry.Ctx{Store: db, BaseDir: base, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return NewServer(registry.New(), cctx).Handler(), reg, base
}

func decodeEnvelope(t *testing.T, body io.Reader, result any) (ok bool, code string) {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.OK && result != nil {
		if err := json.Unmarshal(env.Result, result); err != nil {
			t.Fatal(err)
		}
	}
	return env.OK, env.Error.Code
}

func TestWorkflowArchiveExportAndImport(t *testing.T) {
	srcHandler, srcReg, _ := workflowArchiveServer(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "Workflowfile.yaml"), []byte(workflowArchiveManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := workflowfile.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	built, _, err := srcReg.Publish(f, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	exportRec := httptest.NewRecorder()
	srcHandler.ServeHTTP(exportRec, httptest.NewRequest(http.MethodGet, "/api/workflow-images/demo/1.0.0/export", nil))
	if exportRec.Code != http.StatusOK || exportRec.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("export status=%d type=%q body=%s", exportRec.Code, exportRec.Header().Get("Content-Type"), exportRec.Body.String())
	}
	archive := exportRec.Body.Bytes()

	dstHandler, dstReg, dstBase := workflowArchiveServer(t)
	upload := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/workflow-image-imports", bytes.NewReader(archive))
		rec := httptest.NewRecorder()
		dstHandler.ServeHTTP(rec, req)
		return rec
	}
	var result struct {
		Name    string   `json:"name"`
		Version string   `json:"version"`
		Digest  string   `json:"digest"`
		Tags    []string `json:"tags"`
		Created bool     `json:"created"`
	}
	rec := upload()
	if ok, code := decodeEnvelope(t, rec.Body, &result); !ok || rec.Code != http.StatusOK {
		t.Fatalf("import status=%d code=%s", rec.Code, code)
	}
	if result.Name != "demo" || result.Version != "1.0.0" || result.Digest != built.Digest || !result.Created || len(result.Tags) != 2 {
		t.Fatalf("import = %+v; want digest %s", result, built.Digest)
	}
	if _, err := dstReg.Get(built.Digest); err != nil {
		t.Fatalf("imported row missing: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dstBase, "workflow-image-imports")); len(entries) != 0 {
		t.Fatalf("staging left behind: %v", entries)
	}
	rec = upload()
	if ok, _ := decodeEnvelope(t, rec.Body, &result); !ok || result.Created {
		t.Fatalf("second import = %+v", result)
	}
}

func TestWorkflowArchiveErrors(t *testing.T) {
	h, _, _ := workflowArchiveServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/workflow-images/demo/latest/export", nil))
	if _, code := decodeEnvelope(t, rec.Body, nil); rec.Code != http.StatusNotFound || code != "not_found" {
		t.Fatalf("unknown export status=%d code=%s", rec.Code, code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/workflow-image-imports", bytes.NewReader([]byte("x")))
	req.ContentLength = -1
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if _, code := decodeEnvelope(t, rec.Body, nil); rec.Code != http.StatusRequestEntityTooLarge || code != "archive_too_large" {
		t.Fatalf("unsized import status=%d code=%s", rec.Code, code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/workflow-image-imports", bytes.NewReader([]byte("not a gzip"))))
	if _, code := decodeEnvelope(t, rec.Body, nil); rec.Code != http.StatusBadRequest || code != "bad_archive" {
		t.Fatalf("garbage import status=%d code=%s", rec.Code, code)
	}
}
