package imagesnapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/imagefile"
	"github.com/alekzonder/tariboy/internal/imagesource"
	storedb "github.com/alekzonder/tariboy/internal/store"
)

func TestCaptureStoresImmutableContentAndReusesDigest(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(source, "PROMPT.md"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".tariboy-source.json"), []byte(`{"internal":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := Store{DB: db.DB, Root: filepath.Join(base, "snapshots")}
	first, err := s.Capture(context.Background(), "demo:v1", "sha256:image-one", "demo", source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Capture(context.Background(), "demo:v2", "sha256:image-two", "demo", source)
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceDigest == "" || second.SourceDigest != first.SourceDigest {
		t.Fatalf("source digests = %q and %q, want one non-empty digest", first.SourceDigest, second.SourceDigest)
	}
	if first.RelativeDir != second.RelativeDir {
		t.Fatalf("relative dirs = %q and %q, want content-addressed reuse", first.RelativeDir, second.RelativeDir)
	}

	root, err := s.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(filepath.Join(root, "PROMPT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(prompt) != "first\n" {
		t.Fatalf("snapshot prompt = %q", prompt)
	}
	if _, err := os.Stat(filepath.Join(root, ".tariboy-source.json")); !os.IsNotExist(err) {
		t.Fatalf("internal source metadata entered snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "PROMPT.md"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := s.Capture(context.Background(), "demo:v3", "sha256:image-three", "demo", source)
	if err != nil {
		t.Fatal(err)
	}
	if third.SourceDigest == first.SourceDigest {
		t.Fatal("changed source reused the old digest")
	}
}

func TestCaptureRejectsUnsafeEntriesWithoutPublishing(t *testing.T) {
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
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}

	s := Store{DB: db.DB, Root: filepath.Join(base, "snapshots")}
	if _, err := s.Capture(context.Background(), "demo:v1", "sha256:image", "demo", source); err == nil {
		t.Fatal("Capture accepted a symlink")
	}
	if _, ok, err := s.Lookup(context.Background(), "demo:v1"); err != nil || ok {
		t.Fatalf("Lookup after rejected capture = ok %v, err %v", ok, err)
	}
}

func TestFreezeKeepsOneSourceGeneration(t *testing.T) {
	root := t.TempDir()
	source := t.TempDir()
	path := filepath.Join(source, "prompt.md")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	frozen, err := store.Freeze(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, frozen.RelativeDir, "prompt.md"))
	if err != nil || string(data) != "first" {
		t.Fatalf("frozen prompt = %q, %v", data, err)
	}
}

func TestFreezePinsSiblingSkillForBuild(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "images", "reviewer")
	skill := filepath.Join(base, "skills", "review")
	for _, dir := range []string{source, skill} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("---\nname: review\ndescription: Review changes.\n---\nOriginal\n")
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "finish.md"), []byte("finish\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "Tariboyfile.yaml"), []byte("schema_version: 2\nplugins: []\nskills:\n  - dir: ../../skills/review\nprompts:\n  - file: ../../skills/review/finish.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An unrelated unsafe entry must not enter the snapshot's copy scope.
	if err := os.Symlink("missing", filepath.Join(base, "unrelated")); err != nil {
		t.Fatal(err)
	}
	parsed, err := imagefile.ParseV2(source)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := Store{Root: t.TempDir()}
	frozen, err := snapshots.Freeze(source, parsed.Skills...)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	dir, err := snapshots.OpenFrozen(frozen)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = imagefile.ParseV2(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &image.Store{Dir: t.TempDir()}
	ref := image.Ref{Name: "reviewer", Tag: "v1"}
	if _, err := image.BuildV2(parsed, imagefile.ResolveRoots{SourceSkills: frozen.SourceSkills}, ref, store, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadFile(ref, "skills/review/SKILL.md")
	if err != nil || string(got) != string(body) {
		t.Fatalf("frozen skill = %q, %v", got, err)
	}
	got, err = store.ReadFile(ref, "prompt/layers/000-finish.md")
	if err != nil || string(got) != "finish\n" {
		t.Fatalf("frozen skill prompt = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(frozen.SourceSkills["../../skills/review"], "check.sh"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("frozen script mode = %v, %v", info, err)
	}
	parsed.Skills[0].Dir = "../unfrozen/review"
	if _, err := image.ValidateV2(parsed, imagefile.ResolveRoots{SourceSkills: frozen.SourceSkills}, nil); err == nil {
		t.Fatal("accepted a source skill missing from the frozen declaration set")
	}
}

func TestFreezeCanonicalizesSourceSkillPathsUnderSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	actualRoot := filepath.Join(base, "snapshots")
	if err := os.Mkdir(actualRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	selectedRoot := filepath.Join(base, "selected-snapshots")
	if err := os.Symlink(actualRoot, selectedRoot); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(base, "source")
	skill := filepath.Join(source, "skills", "review")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("review\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	frozen, err := (Store{Root: selectedRoot}).Freeze(source, imagefile.SkillEntry{Dir: "./skills/review"})
	if err != nil {
		t.Fatal(err)
	}
	got := frozen.SourceSkills["./skills/review"]
	canonicalRoot, err := filepath.EvalSymlinks(actualRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalRoot, frozen.RelativeDir, "skills", "review")
	if got != want {
		t.Fatalf("frozen source skill path = %q, want canonical path %q", got, want)
	}
}

func TestFreezePreservesOwnerExecutableMode(t *testing.T) {
	root := t.TempDir()
	source := t.TempDir()
	path := filepath.Join(source, "run.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	frozen, err := (Store{Root: root}).Freeze(source)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, frozen.RelativeDir, "run.sh"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("frozen mode = %v, %v; want 0700", info.Mode(), err)
	}
}

func TestFreezeDisambiguatesFileBoundaries(t *testing.T) {
	root := t.TempDir()
	first := t.TempDir()
	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "a"), []byte("x\x00b\x00600\x00y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "a"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "b"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	one, err := store.Freeze(first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := store.Freeze(second)
	if err != nil {
		t.Fatal(err)
	}
	if one.SourceDigest == two.SourceDigest {
		t.Fatalf("distinct trees shared digest %s", one.SourceDigest)
	}
}

func TestCaptureStoresAndLooksUpGitProvenanceByImageDigest(t *testing.T) {
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
	want := imagesource.Provenance{
		RepositoryID: "production-agent-images",
		GitCommit:    "91ab820",
		LockDigest:   "sha256:lock",
	}

	s := Store{DB: db.DB, Root: filepath.Join(base, "snapshots")}
	if _, err := s.CaptureWithProvenance(
		context.Background(), "reviewer:v7", "sha256:image", "reviewer", source, want,
	); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LookupDigest(context.Background(), "sha256:image")
	if err != nil || !ok {
		t.Fatalf("LookupDigest = ok %v, err %v", ok, err)
	}
	if got.RepositoryID != want.RepositoryID || got.GitCommit != want.GitCommit || got.LockDigest != want.LockDigest {
		t.Fatalf("snapshot provenance = %+v, want %+v", got, want)
	}
}

func TestLookupDigestRejectsConflictingGitProvenance(t *testing.T) {
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
	s := Store{DB: db.DB, Root: filepath.Join(base, "snapshots")}
	for _, item := range []struct {
		ref        string
		provenance imagesource.Provenance
	}{
		{ref: "reviewer:v7", provenance: imagesource.Provenance{RepositoryID: "images-a", GitCommit: "91ab820"}},
		{ref: "reviewer:v8", provenance: imagesource.Provenance{RepositoryID: "images-b", GitCommit: "22cc991"}},
	} {
		if _, err := s.CaptureWithProvenance(context.Background(), item.ref, "sha256:same-image", "reviewer", source, item.provenance); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := s.LookupDigest(context.Background(), "sha256:same-image"); err == nil {
		t.Fatal("LookupDigest accepted conflicting Git provenance")
	}
}
