package portablearchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type tarEntry struct {
	name     string
	typeflag byte
	body     []byte
	link     string
}

func makeArchive(t *testing.T, manifest Manifest, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	all := append([]tarEntry{{name: "manifest.json", typeflag: tar.TypeReg, body: manifestBytes}}, entries...)
	for _, entry := range all {
		h := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Size: int64(len(entry.body)), Mode: 0o600, Linkname: entry.link}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fileManifest(path string, body []byte) Manifest {
	sum := sha256.Sum256(body)
	return Manifest{Format: "tariboy-portable", Version: 1, Kind: "test", Files: []File{{Path: path, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}}}
}

func TestStageRoundTripValidatesManifest(t *testing.T) {
	body := []byte("schema_version: 2\n")
	archive := makeArchive(t, fileManifest("source/Tariboyfile.yaml", body), []tarEntry{{name: "source/Tariboyfile.yaml", typeflag: tar.TypeReg, body: body}})
	destination := filepath.Join(t.TempDir(), "staged")
	manifest, err := Stage(bytes.NewReader(archive), int64(len(archive)), destination, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Kind != "test" {
		t.Fatalf("kind = %q", manifest.Kind)
	}
	got, err := os.ReadFile(filepath.Join(destination, "source", "Tariboyfile.yaml"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("staged body = %q, err %v", got, err)
	}
}

func TestStageRejectsUnsafeOrInconsistentEntries(t *testing.T) {
	body := []byte("x")
	cases := []struct {
		name     string
		manifest Manifest
		entries  []tarEntry
	}{
		{name: "parent traversal", manifest: fileManifest("safe", body), entries: []tarEntry{{name: "../escape", typeflag: tar.TypeReg, body: body}}},
		{name: "absolute", manifest: fileManifest("safe", body), entries: []tarEntry{{name: "/escape", typeflag: tar.TypeReg, body: body}}},
		{name: "symlink", manifest: fileManifest("safe", body), entries: []tarEntry{{name: "safe", typeflag: tar.TypeSymlink, link: "outside"}}},
		{name: "hardlink", manifest: fileManifest("safe", body), entries: []tarEntry{{name: "safe", typeflag: tar.TypeLink, link: "outside"}}},
		{name: "fifo", manifest: fileManifest("safe", body), entries: []tarEntry{{name: "safe", typeflag: tar.TypeFifo}}},
		{name: "duplicate", manifest: fileManifest("safe", body), entries: []tarEntry{{name: "safe", typeflag: tar.TypeReg, body: body}, {name: "safe", typeflag: tar.TypeReg, body: body}}},
		{name: "digest mismatch", manifest: fileManifest("safe", []byte("different")), entries: []tarEntry{{name: "safe", typeflag: tar.TypeReg, body: body}}},
		{name: "unmanifested", manifest: Manifest{Format: "tariboy-portable", Version: 1, Kind: "test"}, entries: []tarEntry{{name: "safe", typeflag: tar.TypeReg, body: body}}},
		{name: "missing", manifest: fileManifest("missing", body), entries: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := makeArchive(t, tc.manifest, tc.entries)
			parent := t.TempDir()
			destination := filepath.Join(parent, "staged")
			if _, err := Stage(bytes.NewReader(archive), int64(len(archive)), destination, DefaultLimits()); err == nil {
				t.Fatal("Stage accepted unsafe archive")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("destination published after failure: %v", err)
			}
		})
	}
}

func TestStageEnforcesExpandedAndFileCountLimits(t *testing.T) {
	body := []byte("12345")
	manifest := fileManifest("one", body)
	archive := makeArchive(t, manifest, []tarEntry{{name: "one", typeflag: tar.TypeReg, body: body}})
	for _, limits := range []Limits{
		{MaxCompressedBytes: int64(len(archive)) + 1, MaxExpandedBytes: 4, MaxFiles: 2, MaxPathBytes: 100},
		{MaxCompressedBytes: int64(len(archive)) + 1, MaxExpandedBytes: 100, MaxFiles: 0, MaxPathBytes: 100},
	} {
		if _, err := Stage(bytes.NewReader(archive), int64(len(archive)), filepath.Join(t.TempDir(), "staged"), limits); err == nil {
			t.Fatal("Stage ignored limits")
		}
	}
}
