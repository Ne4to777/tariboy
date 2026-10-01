package stores

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/imagefile"
	storedb "github.com/alekzonder/tariboy/internal/store"
)

func openCatalog(t *testing.T, base string) (*Catalog, *storedb.Store) {
	t.Helper()
	db, err := storedb.Open(filepath.Join(base, "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	return New(db, base), db
}

func writeImage(t *testing.T, root, name, version string) {
	t.Helper()
	dir := filepath.Join(root, "images", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "schema_version: 2\n"
	if version != "" {
		body += "image_version: " + version + "\n"
	}
	body += "plugins: []\nprompts: []\n"
	if err := os.WriteFile(filepath.Join(dir, "Tariboyfile.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func buildStoreImage(t *testing.T, store *image.Store, sourceRoot, sourceName, targetName, tag, version, builtAt string) {
	t.Helper()
	parsed, err := imagefile.ParseV2(filepath.Join(sourceRoot, "images", sourceName))
	if err != nil {
		t.Fatal(err)
	}
	parsed.ImageVersion = version
	when, err := time.Parse(time.RFC3339, builtAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := image.BuildV2(parsed, imagefile.ResolveRoots{}, image.Ref{Name: targetName, Tag: tag}, store, func() time.Time { return when }, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPersistsAndRereadsLocalInventory(t *testing.T) {
	base, source := t.TempDir(), t.TempDir()
	writeImage(t, source, "alpha", "1.2.3")
	broken := filepath.Join(source, "images", "broken")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "Tariboyfile.yaml"), []byte("schema_version: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeImage(t, outside, "outside", "9.9.9")
	if err := os.Symlink(filepath.Join(outside, "images", "outside"), filepath.Join(source, "images", "linked")); err != nil {
		t.Fatal(err)
	}
	fileLinked := filepath.Join(source, "images", "file-linked")
	if err := os.MkdirAll(fileLinked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "images", "outside", "Tariboyfile.yaml"), filepath.Join(fileLinked, "Tariboyfile.yaml")); err != nil {
		t.Fatal(err)
	}

	catalog, db := openCatalog(t, base)
	got, err := catalog.Add(context.Background(), "team", source)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Store{Name: "team", Source: source, Path: source}); got != want {
		t.Fatalf("Add() = %#v, want %#v", got, want)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	catalog, db = openCatalog(t, base)
	defer db.Close()
	if got, err := catalog.List(context.Background()); err != nil || !reflect.DeepEqual(got, []Store{{Name: "team", Source: source, Path: source}}) {
		t.Fatalf("List() = %#v, %v", got, err)
	}

	detail, err := catalog.Detail(context.Background(), "team")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Images) != 4 || detail.Images[0] != (StoreImage{Name: "alpha", Version: "1.2.3", LatestStatus: "missing"}) {
		t.Fatalf("initial inventory = %#v", detail.Images)
	}
	for _, image := range detail.Images[1:] {
		if image.Error == "" {
			t.Fatalf("malformed or linked image was followed: %#v", image)
		}
	}
	writeImage(t, source, "alpha", "2.0.0")
	detail, err = catalog.Detail(context.Background(), "team")
	if err != nil || detail.Images[0].Version != "2.0.0" {
		t.Fatalf("live inventory = %#v, %v", detail.Images, err)
	}

	if err := catalog.Remove(context.Background(), "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, "images", "alpha", "Tariboyfile.yaml")); err != nil {
		t.Fatalf("Remove deleted local source: %v", err)
	}
	if got, err := catalog.List(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("List after Remove = %#v, %v", got, err)
	}
}

func TestCatalogDetailReportsLatestImageVersionAndComparisonStatus(t *testing.T) {
	base, source := t.TempDir(), t.TempDir()
	writeImage(t, source, "alpha", "2.0.0")
	writeImage(t, source, "beta", "")
	writeImage(t, source, "gamma", "3.0.0")
	writeImage(t, source, "delta", "4.0.0")
	writeImage(t, source, "epsilon", "5.0.0")
	built := &image.Store{Dir: filepath.Join(base, "images")}
	buildStoreImage(t, built, source, "alpha", "alpha", "latest", "1.0.0", "2026-09-08T10:00:00Z")
	buildStoreImage(t, built, source, "alpha", "alpha", "2.0.0", "2.0.0", "2026-09-09T10:00:00Z")
	plantBrokenLatest(t, filepath.Join(base, "images"), "alpha", "irrelevant")
	buildStoreImage(t, built, source, "beta", "beta", "latest", "1.0.0", "2026-09-08T10:00:00Z")
	buildStoreImage(t, built, source, "gamma", "gamma", "latest", "", "2026-09-08T10:00:00Z")
	plantBrokenLatest(t, filepath.Join(base, "images"), "epsilon", "latest")

	catalog, db := openCatalog(t, base)
	defer db.Close()
	if _, err := catalog.Add(context.Background(), "team", source); err != nil {
		t.Fatal(err)
	}
	detail, err := catalog.Detail(context.Background(), "team")
	if err != nil {
		t.Fatal(err)
	}
	want := []StoreImage{
		{Name: "alpha", Version: "2.0.0", BuiltVersion: "1.0.0", UpdateNeeded: true, LatestStatus: "built"},
		{Name: "beta", BuiltVersion: "1.0.0", LatestStatus: "built"},
		{Name: "delta", Version: "4.0.0", LatestStatus: "missing"},
		{Name: "epsilon", Version: "5.0.0", LatestStatus: "error"},
		{Name: "gamma", Version: "3.0.0", LatestStatus: "unversioned"},
	}
	got := append([]StoreImage(nil), detail.Images...)
	if len(got) != len(want) || got[3].LatestError == "" {
		t.Fatalf("images = %#v, want %#v with a latest error", detail.Images, want)
	}
	got[3].LatestError = ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("images = %#v, want %#v", got, want)
	}
}

// plantBrokenLatest writes a tag pointing at content that cannot be read.
func plantBrokenLatest(t *testing.T, imagesDir, name, tag string) {
	t.Helper()
	id := strings.Repeat("ab", 32)
	if err := os.MkdirAll(filepath.Join(imagesDir, name, "refs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(imagesDir, name, "tags"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imagesDir, name, "refs", id+".tar.gz"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imagesDir, name, "tags", tag), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogDetailReportsLatestPathInspectionError(t *testing.T) {
	base, source := t.TempDir(), t.TempDir()
	writeImage(t, source, "alpha", "1.0.0")
	if err := os.MkdirAll(filepath.Join(base, "images"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "images", "alpha"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	catalog, db := openCatalog(t, base)
	defer db.Close()
	if _, err := catalog.Add(context.Background(), "team", source); err != nil {
		t.Fatal(err)
	}
	detail, err := catalog.Detail(context.Background(), "team")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Images) != 1 || detail.Images[0].LatestStatus != "error" || detail.Images[0].LatestError == "" {
		t.Fatalf("images = %#v, want latest inspection error", detail.Images)
	}
}

func TestCatalogDetailIgnoresCorruptBuiltArtifactForAnotherImage(t *testing.T) {
	base, source := t.TempDir(), t.TempDir()
	writeImage(t, source, "alpha", "1.0.0")
	plantBrokenLatest(t, filepath.Join(base, "images"), "unrelated", "latest")
	catalog, db := openCatalog(t, base)
	defer db.Close()
	if _, err := catalog.Add(context.Background(), "team", source); err != nil {
		t.Fatal(err)
	}
	detail, err := catalog.Detail(context.Background(), "team")
	if err != nil || len(detail.Images) != 1 || detail.Images[0].Name != "alpha" {
		t.Fatalf("Detail() = %#v, %v", detail, err)
	}
}

func gitRun(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func TestCatalogClonesRefreshesAndSafelyRemovesGitStore(t *testing.T) {
	ctx := context.Background()
	base, seed, remote := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, "-C", seed, "add", "README.md")
	gitRun(t, "-C", seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "one")
	gitRun(t, "clone", "--bare", seed, remote)
	gitRun(t, "-C", seed, "remote", "add", "origin", remote)

	config := filepath.Join(t.TempDir(), "gitconfig")
	remoteURL := "https://store.example/team.git"
	configBody := "[url \"file://" + remote + "\"]\n\tinsteadOf = " + remoteURL + "\n"
	if err := os.WriteFile(config, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	catalog, db := openCatalog(t, base)
	defer db.Close()
	registered, err := catalog.Add(ctx, "team", remoteURL)
	if err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(base, "stores", "team")
	if registered.Path != managed || registered.Source != remoteURL {
		t.Fatalf("registered = %#v", registered)
	}
	if mode, err := os.Stat(managed); err != nil || mode.Mode().Perm() != 0o700 {
		t.Fatalf("managed clone mode = %v, %v", mode, err)
	}

	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, "-C", seed, "add", "README.md")
	gitRun(t, "-C", seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "two")
	gitRun(t, "-C", seed, "push", "origin", "main")
	if _, err := catalog.Refresh(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(managed, "README.md")); err != nil || string(body) != "two\n" {
		t.Fatalf("refreshed README = %q, %v", body, err)
	}

	if err := os.WriteFile(filepath.Join(managed, "README.md"), []byte("local conflict\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managed, "untracked.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("remote conflict\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, "-C", seed, "add", "README.md")
	gitRun(t, "-C", seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "conflict")
	gitRun(t, "-C", seed, "push", "origin", "main")
	if _, err := catalog.Refresh(ctx, "team"); err != nil {
		t.Fatalf("Refresh with local changes = %v, want managed clone reset to upstream", err)
	}
	if body, err := os.ReadFile(filepath.Join(managed, "README.md")); err != nil || string(body) != "remote conflict\n" {
		t.Fatalf("refresh kept local change: %q, %v", body, err)
	}
	if _, err := os.Lstat(filepath.Join(managed, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("refresh kept untracked file: %v", err)
	}

	if err := catalog.Remove(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(managed); !os.IsNotExist(err) {
		t.Fatalf("managed clone survived Remove: %v", err)
	}
}

func TestCatalogRefreshKeepsLocalGitStoreChanges(t *testing.T) {
	ctx := context.Background()
	seed, remote, local := t.TempDir(), filepath.Join(t.TempDir(), "remote.git"), filepath.Join(t.TempDir(), "local")
	gitRun(t, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, "-C", seed, "add", "README.md")
	gitRun(t, "-C", seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "one")
	gitRun(t, "clone", "--bare", seed, remote)
	gitRun(t, "clone", remote, local)
	if err := os.WriteFile(filepath.Join(local, "README.md"), []byte("local edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	catalog, db := openCatalog(t, t.TempDir())
	defer db.Close()
	if _, err := catalog.Add(ctx, "local", local); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Refresh(ctx, "local"); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(local, "README.md")); err != nil || string(body) != "local edit\n" {
		t.Fatalf("refresh changed local Store work: %q, %v", body, err)
	}
}

func TestCatalogRejectsUnsafeSourcesAndDuplicateNames(t *testing.T) {
	catalog, db := openCatalog(t, t.TempDir())
	defer db.Close()
	for _, test := range []struct{ name, source string }{
		{"../escape", t.TempDir()},
		{"relative", "./relative"},
		{"credential", "https://token@store.example/team.git"},
		{"password", "ssh://git:secret@store.example/team.git"},
		{"scheme", "http://store.example/team.git"},
	} {
		if _, err := catalog.Add(context.Background(), test.name, test.source); err == nil {
			t.Fatalf("Add(%q, %q) accepted", test.name, test.source)
		}
	}
	source := t.TempDir()
	if _, err := catalog.Add(context.Background(), "team", source); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Add(context.Background(), "team", source); err == nil {
		t.Fatal("duplicate Store name accepted")
	}
}

func TestCatalogRemoveRefusesLinkedManagedParentAndKeepsRegistration(t *testing.T) {
	base := t.TempDir()
	catalog, db := openCatalog(t, base)
	defer db.Close()
	if _, err := db.DB.Exec(`INSERT INTO image_stores(name,source) VALUES('team','https://store.example/team.git')`); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "team"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outside, "team", "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "stores")); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Remove(context.Background(), "team"); err == nil {
		t.Fatal("Remove followed a linked managed Stores parent")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("Remove deleted outside data: %v", err)
	}
	if list, err := catalog.List(context.Background()); err != nil || len(list) != 1 || list[0].Name != "team" {
		t.Fatalf("registration after refused Remove = %#v, %v", list, err)
	}
}

func TestRestoreBuildLocksKeepsStoreLockFilesUnchanged(t *testing.T) {
	source := t.TempDir()
	writeImage(t, source, "alpha", "1.0.0")
	imageDir := filepath.Join(source, "images", "alpha")
	const original = "{\"original\":true}\n"
	for _, dir := range []string{source, imageDir} {
		if err := os.WriteFile(filepath.Join(dir, "skills-lock.json"), []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tools := t.TempDir()
	script := "#!/bin/sh\nprintf rewritten >skills-lock.json\nexit ${NPX_EXIT-0}\n"
	if err := os.WriteFile(filepath.Join(tools, "npx"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	catalog := &Catalog{}
	for _, exit := range []string{"0", "7"} {
		t.Setenv("NPX_EXIT", exit)
		err := catalog.RestoreBuildLocks(context.Background(), PreparedBuild{Name: "alpha", Path: imageDir})
		if (err != nil) != (exit != "0") {
			t.Fatalf("exit %s: RestoreBuildLocks() = %v", exit, err)
		}
		for _, dir := range []string{source, imageDir} {
			if body, err := os.ReadFile(filepath.Join(dir, "skills-lock.json")); err != nil || string(body) != original {
				t.Fatalf("exit %s: %s lock = %q, %v", exit, dir, body, err)
			}
		}
	}
}
