package workflowimage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

// beforeRecord runs after the content is published and before its row is
// inserted. Tests replace it to inject failures between the two writes.
var beforeRecord = func() {}

// Registry publishes workflow images through the Store and records each
// manifest in SQLite, so a manifest can be read by digest inside a database
// transaction.
type Registry struct {
	Store *Store
	DB    *sql.DB
}

// insert records m. An existing row for the digest is left as it is.
func (r *Registry) insert(m Manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = r.DB.Exec(
		`INSERT OR IGNORE INTO task_workflow_images (digest, name, version, manifest, built_at) VALUES (?, ?, ?, ?, ?)`,
		m.Digest, m.Name, m.Version, string(data), m.BuiltAt)
	return err
}

// Publish publishes to disk and records the manifest. On a database failure
// it removes content and tags this call introduced. The whole sequence runs
// under the package lock, so no other publication or removal can interleave
// with the rollback.
func (r *Registry) Publish(src *workflowfile.File, now time.Time) (Manifest, bool, error) {
	mu.Lock()
	defer mu.Unlock()

	// Tag state before publishing, so a rollback can put it back. When the
	// names are invalid, Store.Publish reports it.
	var before []tagState
	if src != nil && checkName("workflow name", src.Name) == nil && checkName("workflow version", src.WorkflowVersion) == nil {
		for _, tag := range []string{src.WorkflowVersion, latestTag} {
			d, err := r.Store.readTag(src.Name, tag)
			switch {
			case err == nil:
				before = append(before, tagState{tag: tag, digest: d, had: true})
			case errors.Is(err, ErrNotFound):
				before = append(before, tagState{tag: tag})
			default:
				return Manifest{}, false, err
			}
		}
	}
	m, created, err := r.Store.publishLocked(src, now)
	if err != nil {
		return Manifest{}, false, err
	}
	beforeRecord()
	if err := r.insert(m); err != nil {
		err = fmt.Errorf("record workflow image %s %s: %w", m.Name, m.Version, err)
		if created {
			err = errors.Join(err, r.rollback(m, before))
		}
		return Manifest{}, false, err
	}
	return m, created, nil
}

// rollback undoes a publication that created content: tags return to their
// earlier state and the new content is deleted. The caller holds mu.
func (r *Registry) rollback(m Manifest, before []tagState) error {
	tagErr := r.Store.restoreTags(m.Name, before)
	rmErr := removeAll(r.Store.ContentDir(m.Name, m.Digest))
	if err := errors.Join(tagErr, rmErr); err != nil {
		return fmt.Errorf("roll back workflow image %s %s: %w", m.Name, m.Version, err)
	}
	return nil
}

// Get returns the recorded manifest for a digest. It reads only SQLite.
func (r *Registry) Get(digest string) (Manifest, error) {
	var data string
	err := r.DB.QueryRow(`SELECT manifest FROM task_workflow_images WHERE digest = ?`, digest).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Manifest{}, fmt.Errorf("%w: digest %s", ErrNotFound, digest)
	}
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return Manifest{}, fmt.Errorf("read recorded manifest %s: %w", digest, err)
	}
	return m, nil
}

// Remove removes a tag; when the content goes, it removes the row too.
func (r *Registry) Remove(name, tag string) error {
	if err := checkName("name", name); err != nil {
		return err
	}
	if err := checkName("tag", tag); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	digest, contentRemoved, err := r.Store.removeTagLocked(name, tag)
	if err != nil {
		return err
	}
	if contentRemoved {
		if _, err := r.DB.Exec(`DELETE FROM task_workflow_images WHERE digest = ?`, digest); err != nil {
			return fmt.Errorf("delete workflow image record %s: %w", digest, err)
		}
	}
	return nil
}

// Reconcile inserts rows for stored content that has none. Called at daemon
// start so a crash between the two writes heals. It never deletes rows.
func (r *Registry) Reconcile() error {
	listed, err := r.Store.List()
	if err != nil {
		return err
	}
	for _, l := range listed {
		if err := r.insert(l.Manifest); err != nil {
			return fmt.Errorf("record workflow image %s %s: %w", l.Name, l.Version, err)
		}
	}
	return nil
}
