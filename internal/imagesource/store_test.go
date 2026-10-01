package imagesource

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/imagefile"
)

var testNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

func testStore(t *testing.T) *Store {
	t.Helper()
	return &Store{
		Root:  filepath.Join(t.TempDir(), "image-sources"),
		Clock: func() time.Time { return testNow },
	}
}

func TestImportTreePublishesValidatedEditableSource(t *testing.T) {
	root := t.TempDir()
	incoming := t.TempDir()
	if err := os.WriteFile(filepath.Join(incoming, "Tariboyfile.yaml"), []byte("schema_version: 2\nprompts: [{file: ./PROMPT.md}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "PROMPT.md"), []byte("imported\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Store{Root: root}
	if _, err := s.ImportTree("portable", incoming); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadFile("portable", "PROMPT.md")
	if err != nil || string(got) != "imported\n" {
		t.Fatalf("prompt = %q, err %v", got, err)
	}
	if _, err := s.ImportTree("portable", incoming); !errors.Is(err, ErrExists) {
		t.Fatalf("second import error = %v, want ErrExists", err)
	}
}

func testCreateRequest(name string) CreateRequest {
	return CreateRequest{
		Name:   name,
		Prompt: "Review the current change.",
	}
}

func TestStoreCreateListGetDelete(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"zeta", "alpha"} {
		if _, err := s.Create(CreateRequest{Name: name, Prompt: name}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Fatalf("sources = %+v", got)
	}
	alpha, err := s.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if alpha.SchemaVersion != 2 || alpha.Name != "alpha" ||
		alpha.CreatedAt != "2026-07-29T12:00:00Z" ||
		alpha.UpdatedAt != alpha.CreatedAt || alpha.LastBuild != nil {
		t.Fatalf("alpha metadata = %+v", alpha)
	}

	if err := s.Delete("alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted source error = %v, want ErrNotFound", err)
	}
}

func TestStoreCreatePreservesSourceProvenance(t *testing.T) {
	s := testStore(t)
	want := Provenance{
		RepositoryID: "production-agent-images",
		GitCommit:    "91ab820",
		LockDigest:   "sha256:lock",
	}
	if _, err := s.Create(CreateRequest{Name: "reviewer", Provenance: want}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance != want {
		t.Fatalf("provenance = %+v, want %+v", got.Provenance, want)
	}
}

func TestStoreCreateGeneratesValidSourceFiles(t *testing.T) {
	s := testStore(t)
	src, err := s.Create(testCreateRequest("reviewer"))
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(s.Root, src.Name)
	im, err := imagefile.ParseV2(dir)
	if err != nil {
		t.Fatalf("generated image source does not parse: %v", err)
	}
	if im.SchemaVersion != 2 || len(im.Prompts) != 1 || im.Prompts[0].File != "./PROMPT.md" {
		t.Fatalf("generated imagefile = %+v", im)
	}
	prompt, err := os.ReadFile(filepath.Join(dir, "PROMPT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(prompt) != "Review the current change." {
		t.Fatalf("PROMPT.md = %q", prompt)
	}
	for _, path := range []string{
		filepath.Join(dir, "Tariboyfile.yaml"),
		filepath.Join(dir, "PROMPT.md"),
		filepath.Join(dir, MetadataFilename),
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, st.Mode().Perm())
		}
	}
}

func TestStoreCreateRejectsInvalidReservedAndDuplicateNames(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create(CreateRequest{Name: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(CreateRequest{Name: "reviewer"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate error = %v, want ErrExists", err)
	}
	for _, name := range []string{"", "../escape", "with/slash", "UPPER", "bare"} {
		if _, err := s.Create(CreateRequest{Name: name}); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q error = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestStoreWriteReadAndListFiles(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create(CreateRequest{Name: "reviewer", Prompt: "initial"}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFile("reviewer", "skills/review/SKILL.md", []byte("# Review\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFile("reviewer", "PROMPT.md", []byte("updated")); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadFile("reviewer", "PROMPT.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "updated" {
		t.Fatalf("PROMPT.md = %q", got)
	}
	files, err := s.ListFiles("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "PROMPT.md,Tariboyfile.yaml,skills/review/SKILL.md" {
		t.Fatalf("files = %v", paths)
	}
}

func TestStoreReadMissingNestedFileReturnsNotFound(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create(CreateRequest{Name: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadFile("reviewer", "missing/file.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read missing nested file error = %v, want ErrNotFound", err)
	}
}

func TestStoreWriteIsAtomicWhenRenameFails(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create(CreateRequest{Name: "reviewer", Prompt: "before"}); err != nil {
		t.Fatal(err)
	}
	s.rename = func(_, _ string) error { return errors.New("injected rename failure") }

	err := s.WriteFile("reviewer", "PROMPT.md", []byte("after"))
	if err == nil || !strings.Contains(err.Error(), "injected rename failure") {
		t.Fatalf("write error = %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(s.Root, "reviewer", "PROMPT.md"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "before" {
		t.Fatalf("failed atomic write changed destination to %q", got)
	}
}

func TestStoreRejectsUnsafeFilePathsAndContents(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create(CreateRequest{Name: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
		body []byte
	}{
		{name: "empty", path: "", body: []byte("x")},
		{name: "absolute", path: "/tmp/escape", body: []byte("x")},
		{name: "parent", path: "../escape", body: []byte("x")},
		{name: "metadata", path: MetadataFilename, body: []byte("x")},
		{name: "invalid utf8", path: "bad.md", body: []byte{0xff}},
		{name: "too large", path: "huge.md", body: []byte(strings.Repeat("x", MaxFileSize+1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.WriteFile("reviewer", tc.path, tc.body); !errors.Is(err, ErrInvalidPath) &&
				!errors.Is(err, ErrInvalidUTF8) && !errors.Is(err, ErrFileTooLarge) {
				t.Fatalf("WriteFile error = %v", err)
			}
		})
	}
}

func TestStoreRejectsSymlinksAndNonRegularFiles(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create(CreateRequest{Name: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Root, "reviewer")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadFile("reviewer", "linked.md"); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("read symlink error = %v, want ErrUnsafeFile", err)
	}
	if err := s.WriteFile("reviewer", "linked.md", []byte("overwrite")); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("write symlink error = %v, want ErrUnsafeFile", err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFile("reviewer", "linked-dir/new.md", []byte("overwrite")); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("write through symlink parent error = %v, want ErrUnsafeFile", err)
	}

	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadFile("reviewer", "pipe"); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("read fifo error = %v, want ErrUnsafeFile", err)
	}
	if _, err := s.ListFiles("reviewer"); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("list unsafe tree error = %v, want ErrUnsafeFile", err)
	}
}

func TestStoreRejectsSymlinkSourceRoot(t *testing.T) {
	s := testStore(t)
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.Root, "reviewer")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("reviewer"); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("get symlink source error = %v, want ErrUnsafeFile", err)
	}
	if err := s.Delete("reviewer"); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("delete symlink source error = %v, want ErrUnsafeFile", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside source target changed: %v", err)
	}
}

func TestStoreDeleteCannotTouchBuiltImages(t *testing.T) {
	base := t.TempDir()
	s := &Store{
		Root:  filepath.Join(base, "image-sources"),
		Clock: func() time.Time { return testNow },
	}
	if _, err := s.Create(CreateRequest{Name: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	built := filepath.Join(base, "images", "reviewer", "latest.tar")
	if err := os.MkdirAll(filepath.Dir(built), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(built, []byte("immutable"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete("reviewer"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(built)
	if err != nil || string(got) != "immutable" {
		t.Fatalf("built image changed after source delete: body=%q err=%v", got, err)
	}
}

func TestStoreRecordBuildUpdatesMetadataOnlyAfterSuccess(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create(CreateRequest{Name: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	buildErr := errors.New("build failed")
	if _, err := s.RecordBuild("reviewer", func(string) (BuildRecord, error) {
		return BuildRecord{}, buildErr
	}, nil); !errors.Is(err, buildErr) {
		t.Fatalf("failed build error = %v", err)
	}
	afterFailure, err := s.Get("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.LastBuild != nil {
		t.Fatalf("failed build updated metadata: %+v", afterFailure.LastBuild)
	}

	want := BuildRecord{
		Ref:     "reviewer:latest",
		Digest:  "sha256:abc",
		BuiltAt: "2026-07-29T12:01:00Z",
	}
	var gotDir string
	got, err := s.RecordBuild("reviewer", func(dir string) (BuildRecord, error) {
		gotDir = dir
		return want, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("build result = %+v, want %+v", got, want)
	}
	if gotDir != filepath.Join(s.Root, "reviewer") {
		t.Fatalf("build dir = %q", gotDir)
	}
	afterSuccess, err := s.Get("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if afterSuccess.LastBuild == nil || *afterSuccess.LastBuild != want {
		t.Fatalf("last build = %+v, want %+v", afterSuccess.LastBuild, want)
	}
}

func TestStoreRecordBuildRollsBackArtifactWhenMetadataPublishFails(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create(CreateRequest{Name: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	s.rename = func(_, _ string) error { return errors.New("injected metadata failure") }
	rolledBack := false

	_, err := s.RecordBuild("reviewer", func(string) (BuildRecord, error) {
		return BuildRecord{
			Ref:     "reviewer:v2",
			Digest:  "sha256:new",
			BuiltAt: "2026-07-29T12:01:00Z",
		}, nil
	}, func() error {
		rolledBack = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "injected metadata failure") {
		t.Fatalf("record build error = %v", err)
	}
	if !rolledBack {
		t.Fatal("metadata failure did not roll back the newly published artifact")
	}
	after, getErr := s.Get("reviewer")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if after.LastBuild != nil {
		t.Fatalf("failed metadata publish changed last_build: %+v", after.LastBuild)
	}
}

func TestStoreRecordBuildKeepsArtifactAfterMetadataRenameCommit(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create(CreateRequest{Name: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	s.syncDir = func(string) error { return errors.New("injected directory sync failure") }
	rolledBack := false
	want := BuildRecord{
		Ref:     "reviewer:v2",
		Digest:  "sha256:new",
		BuiltAt: "2026-07-29T12:01:00Z",
	}

	got, err := s.RecordBuild("reviewer", func(string) (BuildRecord, error) {
		return want, nil
	}, func() error {
		rolledBack = true
		return nil
	})
	if err != nil {
		t.Fatalf("committed metadata returned an error: %v", err)
	}
	if rolledBack {
		t.Fatal("post-rename directory sync failure rolled back the built artifact")
	}
	if got != want {
		t.Fatalf("record = %+v, want %+v", got, want)
	}
	after, err := s.Get("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if after.LastBuild == nil || *after.LastBuild != want {
		t.Fatalf("committed last_build = %+v, want %+v", after.LastBuild, want)
	}
}
