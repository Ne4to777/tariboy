package workflowimage

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func exportArchive(t *testing.T, r *Registry, name, tag string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := r.Store.Export(&buf, name, tag); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func importArchive(r *Registry, archive []byte, staging string) (Manifest, bool, error) {
	return r.Import(bytes.NewReader(archive), int64(len(archive)), staging, t0)
}

func TestExportImportRoundTrip(t *testing.T) {
	src, _ := newRegistry(t)
	want, _, err := src.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	archive := exportArchive(t, src, "demo", "1.0.0")

	dst, _ := newRegistry(t)
	staging := filepath.Join(t.TempDir(), "imports")
	got, created, err := importArchive(dst, archive, staging)
	if err != nil {
		t.Fatal(err)
	}
	if !created || got.Digest != want.Digest || got.Version != "1.0.0" {
		t.Fatalf("import = %s %s created=%v; want %s created", got.Version, got.Digest, created, want.Digest)
	}
	if !reflect.DeepEqual(got.Files, want.Files) {
		t.Fatalf("files = %+v; want %+v", got.Files, want.Files)
	}
	tags, err := dst.Store.Tags("demo", want.Digest)
	if err != nil || !reflect.DeepEqual(tags, []string{"1.0.0", "latest"}) {
		t.Fatalf("tags = %v, %v", tags, err)
	}
	if _, err := dst.Get(want.Digest); err != nil {
		t.Fatalf("row missing: %v", err)
	}
	if entries, err := os.ReadDir(staging); err != nil || len(entries) != 0 {
		t.Fatalf("staging left behind: %v, %v", entries, err)
	}

	if _, created, err := importArchive(dst, archive, staging); err != nil || created {
		t.Fatalf("second import created=%v err=%v; want existing", created, err)
	}
}

func TestExportByDigestAndUnknown(t *testing.T) {
	r, _ := newRegistry(t)
	m, _, err := r.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	exportArchive(t, r, "demo", m.Digest)
	if _, err := r.Store.Export(&bytes.Buffer{}, "demo", "9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
}

func TestImportRefusesDigestMismatch(t *testing.T) {
	src, _ := newRegistry(t)
	m, _, err := src.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	meta := archiveMetadata{Name: m.Name, Version: m.Version, Digest: "0000000000000000000000000000000000000000000000000000000000000000", Executable: []string{"scripts/check.sh"}}
	if err := writeArchive(&buf, src.Store.ContentDir(m.Name, m.Digest), m.Files, meta); err != nil {
		t.Fatal(err)
	}
	dst, db := newRegistry(t)
	if _, _, err := importArchive(dst, buf.Bytes(), filepath.Join(t.TempDir(), "imports")); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v; want ErrDigestMismatch", err)
	}
	noRefs(t, dst.Store)
	if n := rowCount(t, db); n != 0 {
		t.Fatalf("rows = %d", n)
	}
}

func TestImportLosingExecutableBitIsMismatch(t *testing.T) {
	src, _ := newRegistry(t)
	m, _, err := src.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	meta := archiveMetadata{Name: m.Name, Version: m.Version, Digest: m.Digest}
	if err := writeArchive(&buf, src.Store.ContentDir(m.Name, m.Digest), m.Files, meta); err != nil {
		t.Fatal(err)
	}
	dst, _ := newRegistry(t)
	_, _, err = importArchive(dst, buf.Bytes(), filepath.Join(t.TempDir(), "imports"))
	if err == nil {
		t.Fatal("import accepted an archive without the script's executable bit")
	}
	noRefs(t, dst.Store)
}

func TestImportRefusesPublishedVersionWithOtherContent(t *testing.T) {
	src, _ := newRegistry(t)
	if _, _, err := src.Publish(writeSource(t, "1.0.0"), t0); err != nil {
		t.Fatal(err)
	}
	archive := exportArchive(t, src, "demo", "1.0.0")
	dst, _ := newRegistry(t)
	other := writeSource(t, "1.0.0")
	put(t, other.Dir, "statuses/work.md", "different\n", 0o644)
	if _, _, err := dst.Publish(other, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := importArchive(dst, archive, filepath.Join(t.TempDir(), "imports")); !errors.Is(err, ErrVersionPublished) {
		t.Fatalf("err = %v; want ErrVersionPublished", err)
	}
}

func TestImportRefusesOtherArchives(t *testing.T) {
	dst, _ := newRegistry(t)
	staging := filepath.Join(t.TempDir(), "imports")
	if _, _, err := importArchive(dst, []byte("not a gzip"), staging); !errors.Is(err, ErrBadArchive) {
		t.Fatalf("err = %v; want ErrBadArchive", err)
	}
	// A valid portable archive of another kind.
	src, _ := newRegistry(t)
	m, _, err := src.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	raw, _ := json.Marshal(archiveMetadata{Name: m.Name, Version: m.Version, Digest: m.Digest})
	if err := writePortable(&buf, src.Store.ContentDir(m.Name, m.Digest), m.Files, "image-artifact", raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := importArchive(dst, buf.Bytes(), staging); !errors.Is(err, ErrBadArchive) {
		t.Fatalf("err = %v; want ErrBadArchive", err)
	}
	if entries, err := os.ReadDir(staging); err != nil || len(entries) != 0 {
		t.Fatalf("staging left behind: %v, %v", entries, err)
	}
	noRefs(t, dst.Store)
}
