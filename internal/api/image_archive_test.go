package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/imagefile"
	"github.com/alekzonder/tariboy/internal/imagesnapshot"
	"github.com/alekzonder/tariboy/internal/registry"
	storedb "github.com/alekzonder/tariboy/internal/store"
)

func TestImageArchiveExportAndUploadPreview(t *testing.T) {
	base := t.TempDir()
	db, err := storedb.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "Tariboyfile.yaml"), []byte("schema_version: 2\nplugins: []\nskills: []\nprompts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := imagefile.ParseV2(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := image.BuildV2(parsed, imagefile.ResolveRoots{}, image.Ref{Name: "demo", Tag: "v1"}, &image.Store{Dir: filepath.Join(base, "images")}, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	snapshots := imagesnapshot.Store{DB: db.DB, Root: filepath.Join(base, "image-source-snapshots")}
	if _, err := snapshots.Capture(context.Background(), "demo:v1", "image-digest", "demo", source); err != nil {
		t.Fatal(err)
	}
	cctx := &registry.Ctx{Store: db, BaseDir: base, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := NewServer(registry.New(), cctx).Handler()

	exportReq := httptest.NewRequest(http.MethodGet, "/api/images/demo:v1/export", nil)
	exportRec := httptest.NewRecorder()
	h.ServeHTTP(exportRec, exportReq)
	if exportRec.Code != http.StatusOK || exportRec.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("export status=%d type=%q body=%s", exportRec.Code, exportRec.Header().Get("Content-Type"), exportRec.Body.String())
	}

	previewReq := httptest.NewRequest(http.MethodPost, "/api/image-imports", exportRec.Body)
	previewReq.ContentLength = int64(exportRec.Body.Len())
	previewRec := httptest.NewRecorder()
	h.ServeHTTP(previewRec, previewReq)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			ImportID string `json:"import_id"`
			Ref      string `json:"ref"`
			Digest   string `json:"digest"`
		} `json:"result"`
	}
	if err := json.NewDecoder(previewRec.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || env.Result.ImportID == "" || env.Result.Ref != "demo:v1" || env.Result.Digest == "" {
		t.Fatalf("preview = %+v", env)
	}
}

func TestTeamArchiveExportAndUploadPreview(t *testing.T) {
	base := t.TempDir()
	db, err := storedb.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := agent.NewStore(db).Create(agent.Agent{Name: "lead", ImageRef: "demo:v1", Group: "team"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "Tariboyfile.yaml"), []byte("schema_version: 2\nplugins: []\nskills: []\nprompts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshots := imagesnapshot.Store{DB: db.DB, Root: filepath.Join(base, "image-source-snapshots")}
	if _, err := snapshots.Capture(context.Background(), "demo:v1", "built", "demo", source); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := reg.Register(registry.Command{Path: "team.compose", Summary: "test compose", Handler: func(*registry.Ctx, registry.Params) (any, error) { return map[string]any{"yaml": "version: 1\n"}, nil }}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(registry.Command{Path: "team.import.archive.plan", Summary: "test plan", Handler: func(*registry.Ctx, registry.Params) (any, error) {
		return map[string]any{"import_id": "planned", "team": "team", "yaml": "version: 1\n", "images": []map[string]any{{"ref": "demo:v1"}}}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	cctx := &registry.Ctx{Store: db, BaseDir: base, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := NewServer(reg, cctx).Handler()
	exportReq := httptest.NewRequest(http.MethodGet, "/api/groups/team/export", nil)
	exportRec := httptest.NewRecorder()
	h.ServeHTTP(exportRec, exportReq)
	if exportRec.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", exportRec.Code, exportRec.Body.String())
	}
	previewReq := httptest.NewRequest(http.MethodPost, "/api/team-imports", exportRec.Body)
	previewReq.ContentLength = int64(exportRec.Body.Len())
	previewRec := httptest.NewRecorder()
	h.ServeHTTP(previewRec, previewReq)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			ImportID string `json:"import_id"`
			Team     string `json:"team"`
			YAML     string `json:"yaml"`
			Images   []struct{ Ref string }
		} `json:"result"`
	}
	if err := json.NewDecoder(previewRec.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || env.Result.ImportID == "" || env.Result.Team != "team" || len(env.Result.Images) != 1 || env.Result.Images[0].Ref != "demo:v1" {
		t.Fatalf("preview=%+v", env)
	}
	badRegistry := registry.New()
	if err := badRegistry.Register(registry.Command{Path: "team.compose", Summary: "invalid compose", Handler: func(*registry.Ctx, registry.Params) (any, error) { return map[string]any{"yaml": "version: 2\n"}, nil }}); err != nil {
		t.Fatal(err)
	}
	if err := badRegistry.Register(registry.Command{Path: "team.import.archive.plan", Summary: "reject plan", Handler: func(*registry.Ctx, registry.Params) (any, error) {
		return nil, errors.New("unsupported compose version")
	}}); err != nil {
		t.Fatal(err)
	}
	badServer := NewServer(badRegistry, cctx).Handler()
	badExport := httptest.NewRecorder()
	badServer.ServeHTTP(badExport, httptest.NewRequest(http.MethodGet, "/api/groups/team/export", nil))
	badPreviewReq := httptest.NewRequest(http.MethodPost, "/api/team-imports", badExport.Body)
	badPreviewReq.ContentLength = int64(badExport.Body.Len())
	badPreview := httptest.NewRecorder()
	badServer.ServeHTTP(badPreview, badPreviewReq)
	if badPreview.Code != http.StatusBadRequest || !strings.Contains(badPreview.Body.String(), "bad_compose") {
		t.Fatalf("invalid compose preview status=%d body=%s", badPreview.Code, badPreview.Body.String())
	}
}
