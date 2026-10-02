package workflowimage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alekzonder/tariboy/internal/store"
)

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
	if err := r.Remove("demo", "latest"); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("rows after removing latest = %d, want 1", n)
	}
	if err := r.Remove("demo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, db); n != 0 {
		t.Fatalf("rows after removing content = %d, want 0", n)
	}
	if _, err := r.Get(m.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after remove: %v", err)
	}
	if err := r.Remove("demo", "1.0.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove unknown tag: %v", err)
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
