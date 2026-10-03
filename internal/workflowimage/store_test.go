package workflowimage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

const testManifest = `schema_version: 1
name: demo
workflow_version: %VERSION%
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

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// writeSource writes a valid source tree and returns its parsed manifest.
func writeSource(t *testing.T, version string) *workflowfile.File {
	t.Helper()
	dir := t.TempDir()
	put(t, dir, "Workflowfile.yaml", strings.ReplaceAll(testManifest, "%VERSION%", version), 0o644)
	put(t, dir, "statuses/work.md", "do the work\n", 0o644)
	put(t, dir, "scripts/check.sh", "#!/bin/sh\nexit 0\n", 0o755)
	put(t, dir, "scripts/helper.sh", "echo helper\n", 0o644)
	f, err := workflowfile.Parse(dir)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func put(t *testing.T, dir, rel, content string, mode fs.FileMode) {
	t.Helper()
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

// newStore returns a store whose read-only tree is made writable again before
// the temporary directory is removed.
func newStore(t *testing.T) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "workflows")
	t.Cleanup(func() { makeWritable(dir) })
	return &Store{Dir: dir}
}

func publish(t *testing.T, s *Store, f *workflowfile.File) Manifest {
	t.Helper()
	m, _, err := s.Publish(f, t0)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func skipRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure injection does not work as root")
	}
}

func TestPublishAndInspect(t *testing.T) {
	s := newStore(t)
	m, isNew, err := s.Publish(writeSource(t, "0.1.0"), t0)
	if err != nil || !isNew {
		t.Fatalf("Publish = %v, %v", isNew, err)
	}
	if len(m.Digest) != 64 || m.Name != "demo" || m.Version != "0.1.0" || m.SchemaVersion != 1 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.BuiltAt != "2026-10-02T12:00:00Z" {
		t.Fatalf("BuiltAt = %q", m.BuiltAt)
	}
	for _, tag := range []string{"0.1.0", "latest", m.Digest} {
		got, err := s.Inspect("demo", tag)
		if err != nil {
			t.Fatalf("Inspect(%s): %v", tag, err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Fatalf("Inspect(%s) = %+v, want %+v", tag, got, m)
		}
	}
	if d, err := s.Resolve("demo", ""); err != nil || d != m.Digest {
		t.Fatalf("Resolve empty = %q, %v", d, err)
	}
	list, err := s.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if list[0].Tag != "0.1.0" || list[1].Tag != "latest" || list[0].Digest != m.Digest {
		t.Fatalf("List = %+v", list)
	}
	tags, err := s.Tags("demo", m.Digest)
	if err != nil || !reflect.DeepEqual(tags, []string{"0.1.0", "latest"}) {
		t.Fatalf("Tags = %v, %v", tags, err)
	}
	if _, err := s.Inspect("demo", "9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect unknown = %v", err)
	}
	if _, err := s.Inspect("other", "latest"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect unknown name = %v", err)
	}
}

func TestStoredTreeModesAndContent(t *testing.T) {
	s := newStore(t)
	m := publish(t, s, writeSource(t, "0.1.0"))
	root := s.ContentDir("demo", m.Digest)
	want := map[string]fs.FileMode{
		"Workflowfile.yaml": 0o400,
		"statuses/work.md":  0o400,
		"scripts/check.sh":  0o500,
		"scripts/helper.sh": 0o400,
		"manifest.json":     0o400,
		"statuses":          fs.ModeDir | 0o500,
		"scripts":           fs.ModeDir | 0o500,
		".":                 fs.ModeDir | 0o500,
	}
	got := map[string]fs.FileMode{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		got[filepath.ToSlash(rel)] = info.Mode()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modes = %v\nwant %v", got, want)
	}
	if len(m.Files) != 4 {
		t.Fatalf("Files = %+v", m.Files)
	}
	for _, e := range m.Files {
		if e.Path == "manifest.json" || e.SHA256 == "" || e.Size == 0 {
			t.Fatalf("bad entry %+v", e)
		}
		if e.Executable != (e.Path == "scripts/check.sh") {
			t.Fatalf("executable of %s = %v", e.Path, e.Executable)
		}
	}
	p, err := s.FilePath("demo", m.Digest, "scripts/check.sh")
	if err != nil || p != filepath.Join(root, "scripts", "check.sh") {
		t.Fatalf("FilePath = %q, %v", p, err)
	}
}

func TestRepublishIsNoOp(t *testing.T) {
	s := newStore(t)
	src := writeSource(t, "0.1.0")
	first := publish(t, s, src)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src.Dir, "scripts", "helper.sh"), old, old); err != nil {
		t.Fatal(err)
	}
	m, isNew, err := s.Publish(src, t0.Add(time.Hour))
	if err != nil || isNew {
		t.Fatalf("Publish = %v, %v", isNew, err)
	}
	if m.Digest != first.Digest || m.BuiltAt != first.BuiltAt {
		t.Fatalf("second = %+v, first = %+v", m, first)
	}
}

func TestChangedContentSameVersion(t *testing.T) {
	s := newStore(t)
	src := writeSource(t, "0.1.0")
	first := publish(t, s, src)
	put(t, src.Dir, "scripts/helper.sh", "echo changed\n", 0o644)
	_, _, err := s.Publish(src, t0)
	if !errors.Is(err, ErrVersionPublished) {
		t.Fatalf("Publish = %v", err)
	}
	for _, tag := range []string{"0.1.0", "latest"} {
		d, err := s.Resolve("demo", tag)
		if err != nil || d != first.Digest {
			t.Fatalf("Resolve(%s) = %q, %v", tag, d, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(s.ContentDir("demo", first.Digest), "scripts", "helper.sh"))
	if err != nil || string(b) != "echo helper\n" {
		t.Fatalf("stored helper = %q, %v", b, err)
	}
	if refs, _ := os.ReadDir(filepath.Join(s.Dir, "demo", "refs")); len(refs) != 1 {
		t.Fatalf("refs = %v", refs)
	}
}

func TestNewVersionMovesLatest(t *testing.T) {
	s := newStore(t)
	a := publish(t, s, writeSource(t, "0.1.0"))
	b := publish(t, s, writeSource(t, "0.2.0"))
	if a.Digest == b.Digest {
		t.Fatal("digests equal")
	}
	for tag, want := range map[string]string{"0.1.0": a.Digest, "0.2.0": b.Digest, "latest": b.Digest} {
		if d, err := s.Resolve("demo", tag); err != nil || d != want {
			t.Fatalf("Resolve(%s) = %q, %v", tag, d, err)
		}
	}
	if tags, _ := s.Tags("demo", a.Digest); !reflect.DeepEqual(tags, []string{"0.1.0"}) {
		t.Fatalf("Tags(a) = %v", tags)
	}
	list, _ := s.List()
	if len(list) != 3 {
		t.Fatalf("List = %+v", list)
	}
}

func TestExecutableBitChangesDigest(t *testing.T) {
	s1, s2 := newStore(t), newStore(t)
	src := writeSource(t, "0.1.0")
	a := publish(t, s1, src)
	if err := os.Chmod(filepath.Join(src.Dir, "scripts", "helper.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := publish(t, s2, src)
	if a.Digest == b.Digest {
		t.Fatal("executable bit did not change the digest")
	}
}

const goldenManifest = `schema_version: 1
name: golden
workflow_version: 1.0.0
initial_status: work
statuses:
  - id: work
    owner: customer
    transitions:
      - { on: done, to: finished }
  - id: finished
    terminal: true
`

const goldenScript = "#!/bin/sh\nexit 0\n"

// goldenDigest is the digest of the two-file source below, computed outside
// Go with sha256sum over the documented byte stream. It must not change when
// Go types change.
const goldenDigest = "aa1d44e5beebbefd7f87507641fe2bf2835a6b4c1c8b7d2de6f820f39e57eef9"

func TestDigestGolden(t *testing.T) {
	line := func(path, exec, content string) string {
		sum := sha256.Sum256([]byte(content))
		return path + "\x00" + exec + "\x00" + hex.EncodeToString(sum[:]) + "\n"
	}
	stream := line("Workflowfile.yaml", "0", goldenManifest) + line("scripts/run.sh", "1", goldenScript)
	if sum := sha256.Sum256([]byte(stream)); hex.EncodeToString(sum[:]) != goldenDigest {
		t.Fatalf("documented stream hashes to %x, want %s", sum, goldenDigest)
	}

	dir := t.TempDir()
	put(t, dir, "Workflowfile.yaml", goldenManifest, 0o644)
	put(t, dir, "scripts/run.sh", goldenScript, 0o755)
	src, err := workflowfile.Parse(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := publish(t, newStore(t), src)
	if m.Digest != goldenDigest {
		t.Fatalf("digest = %s, want %s", m.Digest, goldenDigest)
	}
}

func TestCheckScannedFiles(t *testing.T) {
	src := &workflowfile.File{Statuses: []workflowfile.Status{
		{ID: "work", Owner: workflowfile.Owner{Kind: workflowfile.OwnerPool, Pool: "devs"}, Instructions: "./statuses/work.md",
			Transitions: []workflowfile.Transition{{On: "done", To: "watch", Checks: []workflowfile.Check{{Script: "./scripts/check.sh"}}}}},
		{ID: "watch", Owner: workflowfile.Owner{Kind: workflowfile.OwnerScript},
			Watch:       &workflowfile.Watch{Script: "./scripts/watch.sh", Every: "1m"},
			Transitions: []workflowfile.Transition{{On: "done", To: "finished"}}},
		{ID: "finished", Terminal: true},
	}}
	full := func() []FileEntry {
		return []FileEntry{
			{Path: "Workflowfile.yaml"},
			{Path: "scripts/check.sh", Executable: true},
			{Path: "scripts/watch.sh", Executable: true},
			{Path: "statuses/work.md"},
		}
	}
	if err := checkScanned(src, full()); err != nil {
		t.Fatalf("complete entries: %v", err)
	}
	cases := map[string]struct {
		mutate func([]FileEntry) []FileEntry
		path   string
	}{
		"missing instructions": {func(e []FileEntry) []FileEntry { return e[:3] }, "statuses/work.md"},
		"missing check script": {func(e []FileEntry) []FileEntry { return append(e[:1], e[2:]...) }, "scripts/check.sh"},
		"check not executable": {func(e []FileEntry) []FileEntry { e[1].Executable = false; return e }, "scripts/check.sh"},
		"watch not executable": {func(e []FileEntry) []FileEntry { e[2].Executable = false; return e }, "scripts/watch.sh"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := checkScanned(src, tc.mutate(full()))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("err = %v, want ErrInvalid naming %s", err, tc.path)
			}
		})
	}
}

func TestInvalidManifest(t *testing.T) {
	s := newStore(t)
	src := writeSource(t, "0.1.0")
	if err := os.Remove(filepath.Join(src.Dir, "statuses", "work.md")); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Publish(src, t0)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Publish = %v", err)
	}
	var inv *InvalidError
	if !errors.As(err, &inv) || len(inv.Errors) == 0 || inv.Errors[0].Code != "file_missing" {
		t.Fatalf("InvalidError = %+v", inv)
	}
	noRefs(t, s)
}

func noRefs(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.Dir, "demo", "refs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refs not empty: %v", entries)
	}
}

func TestSourceRejections(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"symlink": func(t *testing.T, dir string) {
			if err := os.Symlink("helper.sh", filepath.Join(dir, "scripts", "link.sh")); err != nil {
				t.Fatal(err)
			}
		},
		"large file": func(t *testing.T, dir string) {
			put(t, dir, "big.bin", strings.Repeat("a", maxFileSize+1), 0o644)
		},
		"too many files": func(t *testing.T, dir string) {
			for i := 0; i < maxFiles; i++ {
				put(t, dir, fmt.Sprintf("many/f%03d.txt", i), "x", 0o644)
			}
		},
		"total too large": func(t *testing.T, dir string) {
			chunk := strings.Repeat("a", maxFileSize)
			for i := 0; i < 9; i++ {
				put(t, dir, fmt.Sprintf("chunk%d", i), chunk, 0o644)
			}
		},
		"manifest.json at root": func(t *testing.T, dir string) { put(t, dir, "manifest.json", "{}", 0o644) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			src := writeSource(t, "0.1.0")
			mutate(t, src.Dir)
			_, _, err := s.Publish(src, t0)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Publish = %v", err)
			}
			noRefs(t, s)
		})
	}
}

func TestGitDirSkipped(t *testing.T) {
	s := newStore(t)
	src := writeSource(t, "0.1.0")
	put(t, src.Dir, ".git/HEAD", "ref\n", 0o644)
	if err := os.Symlink("HEAD", filepath.Join(src.Dir, ".git", "link")); err != nil {
		t.Fatal(err)
	}
	put(t, src.Dir, ".hidden", "h\n", 0o644)
	m := publish(t, s, src)
	var paths []string
	for _, e := range m.Files {
		paths = append(paths, e.Path)
	}
	want := []string{".hidden", "Workflowfile.yaml", "scripts/check.sh", "scripts/helper.sh", "statuses/work.md"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v", paths)
	}
}

func TestRemoveTag(t *testing.T) {
	s := newStore(t)
	m := publish(t, s, writeSource(t, "0.1.0"))
	d, removed, err := s.RemoveTag("demo", "latest")
	if err != nil || removed || d != m.Digest {
		t.Fatalf("RemoveTag latest = %q, %v, %v", d, removed, err)
	}
	if _, err := s.Inspect("demo", "0.1.0"); err != nil {
		t.Fatal(err)
	}
	d, removed, err = s.RemoveTag("demo", "0.1.0")
	if err != nil || !removed || d != m.Digest {
		t.Fatalf("RemoveTag version = %q, %v, %v", d, removed, err)
	}
	if _, err := os.Stat(s.ContentDir("demo", m.Digest)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("content still present: %v", err)
	}
	if _, _, err := s.RemoveTag("demo", "0.1.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RemoveTag unknown = %v", err)
	}
	if _, _, err := s.RemoveTag("demo", "../x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("RemoveTag bad = %v", err)
	}
	// Republishing after removal builds the content again.
	if _, isNew, err := s.Publish(writeSource(t, "0.1.0"), t0); err != nil || !isNew {
		t.Fatalf("republish = %v, %v", isNew, err)
	}
}

func TestFilePathRejectsEscapes(t *testing.T) {
	s := newStore(t)
	m := publish(t, s, writeSource(t, "0.1.0"))
	for _, rel := range []string{"../x", "/etc/passwd", "a/../../x", "", ".."} {
		if _, err := s.FilePath("demo", m.Digest, rel); !errors.Is(err, ErrInvalid) {
			t.Errorf("FilePath(%q) = %v", rel, err)
		}
	}
	if _, err := s.FilePath("demo", m.Digest, "nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file = %v", err)
	}
	if _, err := s.FilePath("../demo", m.Digest, "Workflowfile.yaml"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad name = %v", err)
	}
}

func TestPublishRestoresTagsOnFailure(t *testing.T) {
	skipRoot(t)
	s := newStore(t)
	a := publish(t, s, writeSource(t, "0.1.0"))
	tags := filepath.Join(s.Dir, "demo", "tags")
	if err := os.Chmod(tags, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tags, 0o700) })
	if _, _, err := s.Publish(writeSource(t, "0.2.0"), t0); err == nil {
		t.Fatal("Publish succeeded with read-only tags")
	}
	if d, err := s.Resolve("demo", "latest"); err != nil || d != a.Digest {
		t.Fatalf("latest = %q, %v", d, err)
	}
	if _, err := s.Resolve("demo", "0.2.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("0.2.0 = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.Dir, "demo", "refs"))
	if len(entries) != 1 {
		t.Fatalf("refs = %v", entries)
	}
}

func TestReadRegularRefusesSymlinkAndOversize(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "real.txt", "secret", 0o644)
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRegular(filepath.Join(dir, "link.txt"), 100); err == nil {
		t.Fatal("symlink was read")
	}
	put(t, dir, "big.txt", strings.Repeat("a", 50), 0o755)
	if _, _, err := readRegular(filepath.Join(dir, "big.txt"), 10); err == nil {
		t.Fatal("oversized file was read")
	}
	b, mode, err := readRegular(filepath.Join(dir, "big.txt"), 50)
	if err != nil || len(b) != 50 || mode.Perm()&0o111 == 0 {
		t.Fatalf("readRegular = %d bytes, %v, %v", len(b), mode, err)
	}
}

func TestPublishRollsBackFirstTagWhenSecondFails(t *testing.T) {
	s := newStore(t)
	a := publish(t, s, writeSource(t, "0.1.0"))
	// A directory named latest makes the second tag move fail.
	latest := filepath.Join(s.Dir, "demo", "tags", "latest")
	if err := os.Remove(latest); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(latest, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Publish(writeSource(t, "0.2.0"), t0); err == nil {
		t.Fatal("Publish succeeded")
	}
	if _, err := s.Resolve("demo", "0.2.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("0.2.0 tag survived: %v", err)
	}
	if d, err := s.Resolve("demo", "0.1.0"); err != nil || d != a.Digest {
		t.Fatalf("0.1.0 = %q, %v", d, err)
	}
	// The tags were restored, so the new content is gone.
	if entries, _ := os.ReadDir(filepath.Join(s.Dir, "demo", "refs")); len(entries) != 1 || entries[0].Name() != a.Digest {
		t.Fatalf("refs = %v, want only %s", entries, a.Digest)
	}
}

func TestPublishKeepsContentWhenTagRestoreFails(t *testing.T) {
	skipRoot(t)
	s := newStore(t)
	a := publish(t, s, writeSource(t, "0.1.0"))
	tags := filepath.Join(s.Dir, "demo", "tags")
	// The version tag is written; then the tags directory turns read-only, so
	// both the latest write and the restore of the version tag fail.
	beforeTagWrite = func(tag string) {
		if tag == "latest" {
			_ = os.Chmod(tags, 0o500)
		}
	}
	t.Cleanup(func() {
		beforeTagWrite = func(string) {}
		_ = os.Chmod(tags, 0o700)
	})
	_, _, err := s.Publish(writeSource(t, "0.2.0"), t0)
	if err == nil || !strings.Contains(err.Error(), "restore tags") {
		t.Fatalf("Publish = %v, want a tag restore error", err)
	}
	d, err := s.Resolve("demo", "0.2.0")
	if err != nil || d == a.Digest {
		t.Fatalf("0.2.0 = %q, %v; want the unrestored new digest", d, err)
	}
	if _, err := s.Inspect("demo", d); err != nil {
		t.Fatalf("0.2.0 names %s, whose content is gone: %v", d, err)
	}
}

func TestRestoreTagsReportsFailure(t *testing.T) {
	skipRoot(t)
	s := newStore(t)
	publish(t, s, writeSource(t, "0.1.0"))
	tags := filepath.Join(s.Dir, "demo", "tags")
	if err := os.Chmod(tags, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tags, 0o700) })
	if err := s.restoreTags("demo", []tagState{{tag: "latest"}}); err == nil {
		t.Fatal("restore failure was swallowed")
	}
}

func TestNameWithDoubleDot(t *testing.T) {
	s := newStore(t)
	src := writeSource(t, "0.1.0")
	data, err := os.ReadFile(filepath.Join(src.Dir, "Workflowfile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	put(t, src.Dir, "Workflowfile.yaml", strings.Replace(string(data), "name: demo", "name: foo..bar", 1), 0o644)
	src, err = workflowfile.Parse(src.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Publish(src, t0); err != nil {
		t.Fatalf("Publish foo..bar: %v", err)
	}
	for _, c := range [][2]string{{"..", "latest"}, {"a/b", "latest"}, {"x", ".."}} {
		if _, err := s.Resolve(c[0], c[1]); !errors.Is(err, ErrInvalid) {
			t.Errorf("Resolve(%q, %q) = %v", c[0], c[1], err)
		}
	}
}

func TestStoreNameErrorsNameTheManifestField(t *testing.T) {
	got := storeNameErrors(&workflowfile.File{Name: "a/b", WorkflowVersion: ".."})
	if len(got) != 2 || got[0].Code != "name_invalid" || got[0].Path != "name" ||
		got[1].Code != "version_invalid" || got[1].Path != "workflow_version" {
		t.Fatalf("errors = %#v", got)
	}
	if errs := storeNameErrors(&workflowfile.File{Name: "demo", WorkflowVersion: "1.0.0"}); len(errs) != 0 {
		t.Fatalf("valid names: %#v", errs)
	}
}
