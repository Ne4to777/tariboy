package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/imagefile"
)

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) }
}

// buildPrompt publishes a schema-v2 image whose only layer is prompt.md.
func buildPrompt(t *testing.T, store *Store, ref Ref, body string) (Manifest, error) {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "prompt.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	src := &imagefile.V2{SchemaVersion: 2, Dir: source, Prompts: []imagefile.PromptEntry{{File: "./prompt.md"}}}
	return BuildV2(src, imagefile.ResolveRoots{}, ref, store, fixedClock(), nil)
}

// TestBuildRefsRejectsReservedTags covers the screen every build entry point
// applies before it reaches the one publication mechanism.
func TestBuildRefsRejectsReservedTags(t *testing.T) {
	if _, _, err := BuildRefs("basic", []string{"latest"}, ""); !errors.Is(err, ErrTagReserved) {
		t.Fatalf("BuildRefs error = %v, want ErrTagReserved", err)
	}
	if _, _, err := BuildRefs("reviewer", []string{"v1", "v1"}, ""); !errors.Is(err, ErrDuplicateTag) {
		t.Fatalf("BuildRefs error = %v, want ErrDuplicateTag", err)
	}
	refs, defaulted, err := BuildRefs("reviewer", nil, "1.2.3")
	if err != nil || !defaulted || len(refs) != 2 || refs[0].Tag != "1.2.3" || refs[1].Tag != "latest" {
		t.Fatalf("BuildRefs = %#v, %v, %v", refs, defaulted, err)
	}
}

func TestBuildConcurrentPublishesRetainEveryGeneration(t *testing.T) {
	previous := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(previous)

	storeDir := t.TempDir()
	ref := Ref{Name: "reviewer", Tag: "latest"}
	if _, err := buildPrompt(t, &Store{Dir: storeDir}, ref, "base"); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 6; round++ {
		const publishes = 12
		start := make(chan struct{})
		results := make(chan struct {
			manifest Manifest
			err      error
		}, publishes)
		for worker := 0; worker < publishes; worker++ {
			body := fmt.Sprintf("round %d worker %d", round, worker)
			go func(body string) {
				<-start
				manifest, err := buildPrompt(t, &Store{Dir: storeDir}, ref, body)
				results <- struct {
					manifest Manifest
					err      error
				}{manifest, err}
			}(body)
		}
		close(start)
		for worker := 0; worker < publishes; worker++ {
			result := <-results
			if result.err != nil {
				t.Fatal(result.err)
			}
			if pinned, err := (&Store{Dir: storeDir}).InspectPinned(ref, result.manifest.Digest); err != nil || pinned.Digest != result.manifest.Digest {
				t.Fatalf("round %d pinned generation %s = %#v, %v", round, result.manifest.Digest, pinned, err)
			}
		}
	}
}
