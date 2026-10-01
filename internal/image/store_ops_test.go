package image

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/imagefile"
)

// archivePathFor resolves the file a ref currently points at.
func archivePathFor(t *testing.T, st *Store, ref Ref) string {
	t.Helper()
	path, err := st.archiveFor(ref)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// plantRawRef writes opaque bytes as the content of ref, bypassing validation.
func plantRawRef(t *testing.T, st *Store, ref Ref, body string) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	id := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(st.refsDir(ref.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.contentPath(ref.Name, id), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTag(ref, id); err != nil {
		t.Fatal(err)
	}
}

func seed(t *testing.T, st *Store, name string) {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "b.md"), []byte("BODY "+name), 0o600); err != nil {
		t.Fatal(err)
	}
	im := &imagefile.V2{
		SchemaVersion: 2,
		Prompts:       []imagefile.PromptEntry{{File: "./b.md"}},
		Dir:           src,
	}
	if _, err := BuildV2(im, imagefile.ResolveRoots{}, Ref{Name: name, Tag: "latest"}, st, fixedClock(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestStoreListAndRender(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	if l, err := st.List(); err != nil || len(l) != 0 {
		t.Fatalf("empty list: %v %v", l, err)
	}
	seed(t, st, "bbb")
	seed(t, st, "aaa")
	list, err := st.List()
	if err != nil || len(list) != 2 || list[0].Name != "aaa" || list[1].Name != "bbb" {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	prompt, err := st.RenderPrompt(Ref{Name: "aaa", Tag: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "BODY aaa"; prompt != want {
		t.Fatalf("render = %q, want %q", prompt, want)
	}
}

func TestStoreRemoveAndUnpack(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	seed(t, st, "app")
	dest := t.TempDir()
	if err := st.Unpack(Ref{Name: "app", Tag: "latest"}, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "manifest.json")); err != nil {
		t.Fatalf("unpack missing manifest: %v", err)
	}
	if err := st.Remove(Ref{Name: "app", Tag: "latest"}); err != nil {
		t.Fatal(err)
	}
	if st.Exists(Ref{Name: "app", Tag: "latest"}) {
		t.Fatal("image still present after Remove")
	}
	if err := st.Remove(Ref{Name: "app", Tag: "latest"}); err == nil {
		t.Fatal("removing absent image should error")
	}
}

func TestRetagArchiveReusesPayload(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	source := Ref{Name: "reviewer", Tag: "latest"}
	seed(t, store, source.Name)
	archive, err := store.ArchiveBytes(source)
	if err != nil {
		t.Fatal(err)
	}
	target := Ref{Name: "reviewer", Tag: "v2"}
	manifest, err := store.RetagArchive(source, target, archive)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != target.Name || manifest.Tag != target.Tag || manifest.Digest == "" {
		t.Fatalf("retagged manifest = %#v", manifest)
	}
	published, err := store.Inspect(target)
	if err != nil || manifest.Digest != published.Digest {
		t.Fatalf("returned manifest digest = %q, published = %#v, %v", manifest.Digest, published, err)
	}
	if got, err := store.RenderPrompt(target); err != nil || !strings.Contains(got, "BODY reviewer") {
		t.Fatalf("retagged payload = %q, %v", got, err)
	}
}

// TestBuildMovesAnExistingRef is the store-level contract: rebuilding a ref
// that already exists moves it and keeps the prior generation reachable by its
// own id, so pinned agents are unaffected.
func TestBuildMovesAnExistingRef(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	ref := Ref{Name: "app", Tag: "latest"}
	seed(t, st, ref.Name)

	before, err := st.Inspect(ref)
	if err != nil {
		t.Fatal(err)
	}

	rebuilt, err := buildPrompt(t, st, ref, "CHANGED BODY")
	if err != nil {
		t.Fatalf("rebuilding an existing ref: %v", err)
	}
	if rebuilt.Digest == before.Digest {
		t.Fatal("rebuilt content kept the previous ref id")
	}
	after, err := st.Inspect(ref)
	if err != nil {
		t.Fatal(err)
	}
	if after.Digest != rebuilt.Digest {
		t.Fatalf("tag points at %s, want %s", after.Digest, rebuilt.Digest)
	}
	if pinned, err := st.InspectPinned(ref, before.Digest); err != nil || pinned.Digest != before.Digest {
		t.Fatalf("previous generation = %#v, %v", pinned, err)
	}
}

func TestReservedDefaultRefsCannotBeRemoved(t *testing.T) {
	for _, ref := range []Ref{{Name: "bare", Tag: "latest"}, {Name: "basic", Tag: "latest"}} {
		st := &Store{Dir: t.TempDir()}
		plantRawRef(t, st, ref, "managed")
		if err := st.Remove(ref); !errors.Is(err, ErrReserved) {
			t.Fatalf("Remove(%s) error = %v, want ErrReserved", ref, err)
		}
		if !st.Exists(ref) {
			t.Fatalf("reserved image %s was removed", ref)
		}
	}

	ordinary := Ref{Name: "basic", Tag: "custom"}
	st := &Store{Dir: t.TempDir()}
	plantRawRef(t, st, ordinary, "ordinary")
	if err := st.Remove(ordinary); err != nil {
		t.Fatalf("Remove(%s): %v", ordinary, err)
	}
}

func TestStoreListFiles(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	if _, err := st.ListFiles(Ref{Name: "nope", Tag: "latest"}); err == nil {
		t.Fatal("ListFiles on absent image should error")
	}
	seed(t, st, "app")
	entries, err := st.ListFiles(Ref{Name: "app", Tag: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileEntry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	for _, want := range []string{"manifest.json", "prompt/template.json", "prompt/layers/000-b.md"} {
		e, ok := byPath[want]
		if !ok {
			t.Fatalf("ListFiles missing %q; got %+v", want, entries)
		}
		if e.IsDir {
			t.Fatalf("%q reported as dir", want)
		}
		if e.Size <= 0 {
			t.Fatalf("%q has non-positive size %d", want, e.Size)
		}
	}
	// entries come back sorted by path
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Path > entries[i].Path {
			t.Fatalf("entries not sorted: %+v", entries)
		}
	}
}

func TestStoreReadFile(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	if _, err := st.ReadFile(Ref{Name: "nope", Tag: "latest"}, "prompt/layers/000-b.md"); err == nil {
		t.Fatal("ReadFile on absent image should error")
	}
	seed(t, st, "app")
	ref := Ref{Name: "app", Tag: "latest"}

	body, err := st.ReadFile(ref, "prompt/layers/000-b.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "BODY app" {
		t.Fatalf("prompt layer = %q", body)
	}

	// missing in-archive file → error
	if _, err := st.ReadFile(ref, "does-not-exist.md"); err == nil {
		t.Fatal("ReadFile of absent member should error")
	}

	// path traversal is rejected before any lookup
	for _, bad := range []string{"../etc/passwd", "skills/../../secret", "/etc/passwd", ".."} {
		if _, err := st.ReadFile(ref, bad); err == nil {
			t.Fatalf("ReadFile(%q) should be rejected", bad)
		}
	}
}

// tarEntryNames lists every member name present in the archive.
func tarEntryNames(t *testing.T, archive string) map[string]bool {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = true
	}
	return out
}
