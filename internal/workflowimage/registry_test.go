package workflowimage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/workflowfile"
)

const failInsertTrigger = `CREATE TRIGGER fail_insert BEFORE INSERT ON task_workflow_images
WHEN NEW.version = '1.1.0' BEGIN SELECT RAISE(ABORT, 'insert refused'); END`

func TestRegistryRollbackRestoresTags(t *testing.T) {
	r, db := newRegistry(t)
	first, _, err := r.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(failInsertTrigger); err != nil {
		t.Fatal(err)
	}
	src := writeSource(t, "1.1.0")
	put(t, src.Dir, "statuses/work.md", "changed\n", 0o644)
	if _, _, err := r.Publish(src, t0); err == nil {
		t.Fatal("publish succeeded although the insert was refused")
	}
	if d, err := r.Store.Resolve("demo", "latest"); err != nil || d != first.Digest {
		t.Fatalf("latest = %q, %v; want %s", d, err, first.Digest)
	}
	if _, err := r.Store.Resolve("demo", "1.1.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("1.1.0 tag survived: %v", err)
	}
	entries, err := os.ReadDir(r.Store.refsDir("demo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != first.Digest {
		t.Fatalf("refs = %v, want only %s", entries, first.Digest)
	}
}

func TestRegistryRollbackFailureIsReported(t *testing.T) {
	skipRoot(t)
	r, db := newRegistry(t)
	if _, _, err := r.Publish(writeSource(t, "1.0.0"), t0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(failInsertTrigger); err != nil {
		t.Fatal(err)
	}
	tags := r.Store.tagsDir("demo")
	beforeRecord = func() { _ = os.Chmod(tags, 0o500) }
	t.Cleanup(func() {
		beforeRecord = func() {}
		_ = os.Chmod(tags, 0o700)
	})
	src := writeSource(t, "1.1.0")
	put(t, src.Dir, "statuses/work.md", "changed\n", 0o644)
	_, _, err := r.Publish(src, t0)
	if err == nil || !strings.Contains(err.Error(), "insert refused") || !strings.Contains(err.Error(), "roll back") {
		t.Fatalf("err = %v, want the insert error joined with a rollback error", err)
	}
	// The tags could not be restored and still name the new digest, so its
	// content must still be there.
	for _, tag := range []string{"1.1.0", "latest"} {
		d, err := r.Store.Resolve("demo", tag)
		if err != nil {
			t.Fatalf("%s: %v", tag, err)
		}
		if _, err := r.Store.Inspect("demo", d); err != nil {
			t.Fatalf("%s names %s, whose content is gone: %v", tag, d, err)
		}
	}
}

func TestRegistryConcurrentPublish(t *testing.T) {
	r, db := newRegistry(t)
	const rounds = 20
	for i := 0; i < rounds; i++ {
		name := fmt.Sprintf("demo%d", i)
		a, b := renamedSource(t, "1.0.0", name), renamedSource(t, "1.1.0", name)
		put(t, b.Dir, "statuses/work.md", "other\n", 0o644)
		srcs := []*workflowfile.File{a, b}
		versions := []string{"1.0.0", "1.1.0"}
		digests := make([]string, 2)
		var wg sync.WaitGroup
		for j := range srcs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m, _, err := r.Publish(srcs[j], t0)
				if err != nil {
					t.Error(err)
					return
				}
				digests[j] = m.Digest
			}()
		}
		wg.Wait()
		if t.Failed() {
			return
		}
		for j, v := range versions {
			if d, err := r.Store.Resolve(name, v); err != nil || d != digests[j] {
				t.Fatalf("%s tag = %q, %v; want %s", v, d, err, digests[j])
			}
			if _, err := r.Get(digests[j]); err != nil {
				t.Fatalf("row for %s missing: %v", v, err)
			}
		}
		if d, err := r.Store.Resolve(name, "latest"); err != nil || (d != digests[0] && d != digests[1]) {
			t.Fatalf("latest = %q, %v", d, err)
		}
	}
	if n := rowCount(t, db); n != 2*rounds {
		t.Fatalf("rows = %d, want %d", n, 2*rounds)
	}
}

// renamedSource is writeSource with the workflow name rewritten in
// Workflowfile.yaml, so the name is part of the stored bytes and the digest.
func renamedSource(t *testing.T, version, name string) *workflowfile.File {
	t.Helper()
	src := writeSource(t, version)
	body := strings.Replace(strings.ReplaceAll(testManifest, "%VERSION%", version), "name: demo", "name: "+name, 1)
	put(t, src.Dir, "Workflowfile.yaml", body, 0o644)
	f, err := workflowfile.Parse(src.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func newRegistry(t *testing.T) (*Registry, *sql.DB) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Registry{Store: newStore(t), DB: st.DB}, st.DB
}

func rowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_workflow_images`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRegistryPublishRecordsRow(t *testing.T) {
	r, db := newRegistry(t)
	m, created, err := r.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil || !created {
		t.Fatalf("publish: created=%v err=%v", created, err)
	}
	var name, version, manifest, builtAt string
	err = db.QueryRow(`SELECT name, version, manifest, built_at FROM task_workflow_images WHERE digest = ?`, m.Digest).
		Scan(&name, &version, &manifest, &builtAt)
	if err != nil {
		t.Fatal(err)
	}
	if name != "demo" || version != "1.0.0" || builtAt != m.BuiltAt {
		t.Fatalf("row = %q %q %q", name, version, builtAt)
	}
	var got Manifest
	if err := json.Unmarshal([]byte(manifest), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("manifest round trip differs:\n got %+v\nwant %+v", got, m)
	}
	viaGet, err := r.Get(m.Digest)
	if err != nil || !reflect.DeepEqual(viaGet, m) {
		t.Fatalf("Get = %+v, %v", viaGet, err)
	}
	inspected, err := r.Store.Inspect("demo", m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(inspected)
	if manifest != string(want) {
		t.Fatalf("stored JSON differs from Inspect JSON")
	}
}

func TestRegistryRepublishDoesNotDuplicate(t *testing.T) {
	r, db := newRegistry(t)
	f := writeSource(t, "1.0.0")
	if _, _, err := r.Publish(f, t0); err != nil {
		t.Fatal(err)
	}
	if _, created, err := r.Publish(f, t0.Add(1)); err != nil || created {
		t.Fatalf("republish: created=%v err=%v", created, err)
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestRegistryGetUnknown(t *testing.T) {
	r, _ := newRegistry(t)
	if _, err := r.Get("deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRegistryDatabaseFailureLeavesNothing(t *testing.T) {
	r, _ := newRegistry(t)
	if err := r.DB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Publish(writeSource(t, "1.0.0"), t0); err == nil {
		t.Fatal("publish succeeded with a closed database")
	}
	noRefs(t, r.Store)
	if _, err := r.Store.Resolve("demo", "1.0.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tag survived: %v", err)
	}
	if _, err := r.Store.Resolve("demo", "latest"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("latest survived: %v", err)
	}
}

func TestRegistryDatabaseFailureKeepsExistingContent(t *testing.T) {
	r, _ := newRegistry(t)
	m := publish(t, r.Store, writeSource(t, "1.0.0")) // content without a row
	if err := r.DB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Publish(writeSource(t, "1.0.0"), t0); err == nil {
		t.Fatal("publish succeeded with a closed database")
	}
	if _, err := r.Store.Inspect("demo", m.Digest); err != nil {
		t.Fatalf("existing content was removed: %v", err)
	}
}

func TestRegistryRemoveDeletesRowOnlyWithContent(t *testing.T) {
	r, db := newRegistry(t)
	m, _, err := r.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	d, removed, err := r.Remove("demo", "latest")
	if err != nil || removed || d != m.Digest {
		t.Fatalf("Remove latest = %q, %v, %v", d, removed, err)
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("rows after removing latest = %d, want 1", n)
	}
	d, removed, err = r.Remove("demo", "1.0.0")
	if err != nil || !removed || d != m.Digest {
		t.Fatalf("Remove 1.0.0 = %q, %v, %v", d, removed, err)
	}
	if n := rowCount(t, db); n != 0 {
		t.Fatalf("rows after removing content = %d, want 0", n)
	}
	if _, err := r.Get(m.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after remove: %v", err)
	}
	if _, _, err := r.Remove("demo", "1.0.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove unknown tag: %v", err)
	}
}

// A failed row delete must leave the disk untouched, so no row outlives its
// content and no content loses its row.
func TestRegistryRemoveRowFailureKeepsContent(t *testing.T) {
	r, db := newRegistry(t)
	m, _, err := r.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Remove("demo", "latest"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_delete BEFORE DELETE ON task_workflow_images BEGIN SELECT RAISE(ABORT, 'delete refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Remove("demo", "1.0.0"); err == nil || !strings.Contains(err.Error(), "delete refused") {
		t.Fatalf("Remove = %v, want the delete error", err)
	}
	if d, err := r.Store.Resolve("demo", "1.0.0"); err != nil || d != m.Digest {
		t.Fatalf("1.0.0 tag = %q, %v", d, err)
	}
	if _, err := r.Store.Inspect("demo", m.Digest); err != nil {
		t.Fatalf("content: %v", err)
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// A failed disk removal rolls the row delete back.
func TestRegistryRemoveDiskFailureKeepsRow(t *testing.T) {
	skipRoot(t)
	r, db := newRegistry(t)
	m, _, err := r.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Remove("demo", "latest"); err != nil {
		t.Fatal(err)
	}
	tags := r.Store.tagsDir("demo")
	if err := os.Chmod(tags, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tags, 0o700) })
	if _, removed, err := r.Remove("demo", "1.0.0"); err == nil || removed {
		t.Fatalf("Remove = %v, %v; want a disk error", removed, err)
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if _, err := r.Get(m.Digest); err != nil {
		t.Fatalf("row: %v", err)
	}
}

func TestRegistryReconcilePrunesRowsWithoutContent(t *testing.T) {
	r, db := newRegistry(t)
	gone, _, err := r.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	kept, _, err := r.Publish(writeSource(t, "2.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	// Content and its tag deleted by hand, as a crash between the disk step
	// and the row delete would leave them.
	if err := os.Remove(r.Store.tagPath("demo", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := removeAll(r.Store.ContentDir("demo", gone.Digest)); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(gone.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan row survived: %v", err)
	}
	if _, err := r.Get(kept.Digest); err != nil {
		t.Fatalf("row with content was pruned: %v", err)
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// Reconcile keeps the row of content that no tag names any more.
func TestRegistryReconcileKeepsUntaggedContentRow(t *testing.T) {
	r, db := newRegistry(t)
	m, _, err := r.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"1.0.0", "latest"} {
		if err := os.Remove(r.Store.tagPath("demo", tag)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(m.Digest); err != nil {
		t.Fatalf("row was pruned although its content exists: %v", err)
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestRegistryVersionImmutableAfterTagRemoval(t *testing.T) {
	r, db := newRegistry(t)
	first, _, err := r.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Remove("demo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	changed := writeSource(t, "1.0.0")
	put(t, changed.Dir, "scripts/check.sh", "#!/bin/sh\nexit 1\n", 0o755)
	if _, _, err := r.Publish(changed, t0); !errors.Is(err, ErrVersionPublished) {
		t.Fatalf("publish changed 1.0.0 = %v, want ErrVersionPublished", err)
	}
	if d, err := r.Store.Resolve("demo", "latest"); err != nil || d != first.Digest {
		t.Fatalf("latest = %q, %v; want %s", d, err, first.Digest)
	}
	if _, err := r.Store.Resolve("demo", "1.0.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("1.0.0 tag = %v, want ErrNotFound", err)
	}
	b, err := os.ReadFile(filepath.Join(r.Store.ContentDir("demo", first.Digest), "scripts", "check.sh"))
	if err != nil || string(b) != "#!/bin/sh\nexit 0\n" {
		t.Fatalf("stored check.sh = %q, %v", b, err)
	}
	if entries, _ := os.ReadDir(r.Store.refsDir("demo")); len(entries) != 1 {
		t.Fatalf("refs = %v, want only %s", entries, first.Digest)
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if _, err := r.Get(first.Digest); err != nil {
		t.Fatalf("first row: %v", err)
	}

	// Once the last tag is gone, the content and its row are gone, and the
	// version may be published again with other content.
	if _, removed, err := r.Remove("demo", "latest"); err != nil || !removed {
		t.Fatalf("Remove latest = %v, %v", removed, err)
	}
	if n := rowCount(t, db); n != 0 {
		t.Fatalf("rows after removing latest = %d, want 0", n)
	}
	m, created, err := r.Publish(changed, t0)
	if err != nil || !created || m.Digest == first.Digest || m.Version != "1.0.0" {
		t.Fatalf("republish = %+v, %v, %v", m, created, err)
	}
}

func TestRegistryUniqueVersionIsNotIgnored(t *testing.T) {
	r, db := newRegistry(t)
	other := strings.Repeat("0", 64)
	if _, err := db.Exec(`INSERT INTO task_workflow_images (digest, name, version, manifest, built_at) VALUES (?, 'demo', '1.0.0', '{}', 'x')`, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Publish(writeSource(t, "1.0.0"), t0); err == nil {
		t.Fatal("publish succeeded although a row already holds demo 1.0.0")
	}
	noRefs(t, r.Store)
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestRegistryReconcileRestoresRow(t *testing.T) {
	r, db := newRegistry(t)
	m, _, err := r.Publish(writeSource(t, "1.0.0"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM task_workflow_images`); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(m.Digest)
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("Get after reconcile = %+v, %v", got, err)
	}
	if err := r.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}
