package teamportable

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/imagesnapshot"
	"github.com/alekzonder/tariboy/internal/portablearchive"
	storedb "github.com/alekzonder/tariboy/internal/store"
)

func TestImagePreviewUsesStableLowercaseJSONFields(t *testing.T) {
	data, err := json.Marshal(Image{Ref: "img:v1", SourceName: "src", SourceDigest: "digest", OriginalImageDigest: "built"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ref":"img:v1","source_name":"src","source_digest":"digest","original_image_digest":"built"}`
	if string(data) != want {
		t.Fatalf("JSON = %s, want %s", data, want)
	}
}

func TestExportAndPreviewCarryComposeWithoutImageSources(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(source, "Tariboyfile.yaml"), []byte("schema_version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshots := &imagesnapshot.Store{DB: db.DB, Root: filepath.Join(base, "snapshots")}
	if _, err := snapshots.Capture(context.Background(), "demo:v1", "built", "demo", source); err != nil {
		t.Fatal(err)
	}
	service := Service{Snapshots: snapshots, StagingRoot: filepath.Join(base, "imports")}
	composeYAML := "version: 1\ngroups:\n  team:\n    lead: lead\n"
	var archive bytes.Buffer
	if err := service.Export(context.Background(), "team", []byte(composeYAML), []string{"demo:v1"}, &archive); err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(context.Background(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if preview.Team != "team" || !bytes.Contains(preview.ComposeYAML, []byte("groups:")) || len(preview.Images) != 0 {
		t.Fatalf("preview = %+v", preview)
	}
	operation, err := service.Operation(preview.ImportID)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != "pending" || len(operation.Steps) != 0 {
		t.Fatalf("initial operation = %+v", operation)
	}
	stage := filepath.Join(service.StagingRoot, preview.ImportID)
	if err := filepath.Walk(stage, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, path)
		if err != nil {
			return err
		}
		if rel == "images" || filepath.Dir(rel) == "images" {
			t.Fatalf("team archive contains image member %q", rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateFromComposeBuildsArchiveAcceptedByPreview(t *testing.T) {
	root := t.TempDir()
	for _, source := range []string{"manager", "worker"} {
		dir := filepath.Join(root, source)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Tariboyfile.yaml"), []byte("schema_version: 2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	composePath := filepath.Join(root, "tariboy-compose.yaml")
	composeYAML := []byte("version: 1\nimages:\n  manager: {context: ./manager}\n  worker: {context: ./worker}\ngroups:\n  dev: {lead: manager}\nagents:\n  manager: {image: manager:latest, group: dev}\n  worker: {image: worker:latest, group: dev}\n")
	if err := os.WriteFile(composePath, composeYAML, 0o600); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := CreateFromCompose(composePath, &archive); err != nil {
		t.Fatal(err)
	}
	preview, err := (Service{StagingRoot: filepath.Join(root, "imports")}).Preview(context.Background(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if preview.Team != "dev" || len(preview.Images) != 0 || bytes.Contains(preview.ComposeYAML, []byte("context:")) {
		t.Fatalf("preview = %+v", preview)
	}
}

func TestPreviewRejectsLegacySourceBearingTeamArchive(t *testing.T) {
	root := t.TempDir()
	var files []portablearchive.File
	var paths []string
	compose := []byte("version: 1\ngroups:\n  team: {}\n")
	if err := addFile(root, "tariboy-compose.yaml", compose, &files, &paths); err != nil {
		t.Fatal(err)
	}
	if err := addFile(root, "images/source/Tariboyfile.yaml", []byte("schema_version: 2\n"), &files, &paths); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(metadata{Team: "team", Images: []Image{{Ref: "demo:v1", SourceName: "source", SourceDigest: "sha256:legacy"}}})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := portablearchive.Write(&archive, root, portablearchive.Manifest{Format: "tariboy-portable", Version: 1, Kind: "team", Files: files, Metadata: meta}, paths); err != nil {
		t.Fatal(err)
	}
	service := Service{StagingRoot: filepath.Join(t.TempDir(), "imports")}
	if _, err := service.Preview(context.Background(), bytes.NewReader(archive.Bytes()), int64(archive.Len())); err == nil {
		t.Fatal("legacy source-bearing team archive was accepted")
	}
}

func TestPreviewRejectsUnadvertisedImageArchiveMember(t *testing.T) {
	root := t.TempDir()
	var files []portablearchive.File
	var paths []string
	if err := addFile(root, "tariboy-compose.yaml", []byte("version: 1\ngroups:\n  team: {}\n"), &files, &paths); err != nil {
		t.Fatal(err)
	}
	if err := addFile(root, "images/source/Tariboyfile.yaml", []byte("schema_version: 2\n"), &files, &paths); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(metadata{Team: "team", Images: []Image{}})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := portablearchive.Write(&archive, root, portablearchive.Manifest{Format: "tariboy-portable", Version: 1, Kind: "team", Files: files, Metadata: meta}, paths); err != nil {
		t.Fatal(err)
	}
	service := Service{StagingRoot: filepath.Join(t.TempDir(), "imports")}
	if _, err := service.Preview(context.Background(), bytes.NewReader(archive.Bytes()), int64(archive.Len())); err == nil {
		t.Fatal("team archive with unadvertised image member was accepted")
	}
}

func TestLoadRejectsPreviouslyStagedSourceBearingTeamPreview(t *testing.T) {
	service := Service{StagingRoot: filepath.Join(t.TempDir(), "imports")}
	dir := filepath.Join(service.StagingRoot, "legacy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(metadata{Team: "team", Images: []Image{{Ref: "demo:v1", SourceName: "source", SourceDigest: "sha256:legacy"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".team-preview.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tariboy-compose.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Load("legacy"); err == nil {
		t.Fatal("previously staged source-bearing preview was accepted")
	}
}

func TestLoadRejectsPreviouslyStagedExtraImageMember(t *testing.T) {
	service := Service{StagingRoot: filepath.Join(t.TempDir(), "imports")}
	dir := filepath.Join(service.StagingRoot, "legacy")
	if err := os.MkdirAll(filepath.Join(dir, "images", "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(metadata{Team: "team", Images: []Image{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".team-preview.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tariboy-compose.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "source", "Tariboyfile.yaml"), []byte("schema_version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Load("legacy"); err == nil {
		t.Fatal("previously staged preview with extra image member was accepted")
	}
}

func TestCreateFromComposeDoesNotSerializeContextOutsideComposeDirectory(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "tariboy-compose.yaml")
	composeYAML := []byte("version: 1\nimages:\n  manager: {context: ../outside}\ngroups:\n  dev: {lead: manager}\nagents:\n  manager: {image: manager:latest, group: dev}\n")
	if err := os.WriteFile(composePath, composeYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := CreateFromCompose(composePath, &archive); err != nil {
		t.Fatal(err)
	}
	preview, err := (Service{StagingRoot: filepath.Join(root, "imports")}).Preview(context.Background(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(preview.ComposeYAML, []byte("../outside")) || bytes.Contains(preview.ComposeYAML, []byte("context:")) {
		t.Fatalf("portable compose leaked image source path: %s", preview.ComposeYAML)
	}
}

func TestPreviewCreatesDurableOperationThatSurvivesServiceRestart(t *testing.T) {
	base := t.TempDir()
	service := Service{StagingRoot: filepath.Join(base, "imports")}
	// A durable operation is also independently writable for per-item progress.
	operation := Operation{ImportID: "import-1", Team: "team", Status: "running", Steps: []OperationStep{{Kind: "image", Name: "demo:v1", Status: "failed", SourceReady: true}}}
	if err := service.SaveOperation(operation); err != nil {
		t.Fatal(err)
	}
	restarted := Service{StagingRoot: service.StagingRoot}
	got, err := restarted.Operation("import-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" || len(got.Steps) != 1 || got.Steps[0].Status != "failed" || !got.Steps[0].SourceReady {
		t.Fatalf("operation after restart = %+v", got)
	}
}

func TestOperationPersistsImportResolutionsForRetry(t *testing.T) {
	service := Service{StagingRoot: filepath.Join(t.TempDir(), "imports")}
	want := Operation{ImportID: "import-1", Team: "renamed", Status: "failed", ComposeYAML: "version: 1\n", ResolvedRefs: map[string]string{"old:v1": "new:v1"}, UpdateExisting: true}
	if err := service.SaveOperation(want); err != nil {
		t.Fatal(err)
	}
	got, err := service.Operation(want.ImportID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ComposeYAML != want.ComposeYAML || got.ResolvedRefs["old:v1"] != "new:v1" || !got.UpdateExisting {
		t.Fatalf("resolutions after restart = %+v", got)
	}
}

func TestStaleRunningOperationBecomesRetryableAfterRestart(t *testing.T) {
	operation := Operation{Status: "running", Updated: "2026-08-02T00:00:00Z"}
	got := recoverStaleOperation(operation, time.Date(2026, 8, 2, 0, 10, 0, 0, time.UTC), 5*time.Minute)
	if got.Status != "failed" || got.Error == "" {
		t.Fatalf("recovered operation = %+v", got)
	}
}

func TestFreshLeasePreventsActiveImportRecovery(t *testing.T) {
	service := Service{StagingRoot: filepath.Join(t.TempDir(), "imports")}
	operation := Operation{ImportID: "import-1", Team: "team", Status: "running", Updated: "2026-08-02T00:00:00Z"}
	if err := service.SaveOperation(operation); err != nil {
		t.Fatal(err)
	}
	if err := service.TouchLease(operation.ImportID); err != nil {
		t.Fatal(err)
	}
	got, err := service.RecoverOperation(operation.ImportID, time.Now(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("active operation recovered as %+v", got)
	}
}

func TestPreviewYAMLCreatesDurablePendingImport(t *testing.T) {
	service := Service{StagingRoot: filepath.Join(t.TempDir(), "imports")}
	preview, err := service.PreviewYAML("team", []byte("version: 1\ngroups:\n  team: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := service.Load(preview.ImportID)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := service.Operation(preview.ImportID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Team != "team" || operation.Status != "pending" {
		t.Fatalf("preview=%+v operation=%+v", loaded, operation)
	}
}
