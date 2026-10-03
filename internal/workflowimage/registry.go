package workflowimage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// insert records m. An existing row for the digest is left as it is; a row
// holding the same name and version under another digest is an error.
func (r *Registry) insert(m Manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = r.DB.Exec(
		`INSERT INTO task_workflow_images (digest, name, version, manifest, built_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(digest) DO NOTHING`,
		m.Digest, m.Name, m.Version, string(data), m.BuiltAt)
	return err
}

// Publish publishes to disk and records the manifest. On a database failure
// it removes content and tags this call introduced. The whole sequence runs
// under the package lock, so no other publication or removal can interleave
// with the rollback.
func (r *Registry) Publish(src *workflowfile.File, now time.Time) (Manifest, bool, error) {
	return r.publish(src, "", now)
}

// publish is Publish that, with a non-empty want, refuses content whose
// digest differs.
func (r *Registry) publish(src *workflowfile.File, want string, now time.Time) (Manifest, bool, error) {
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
	m, created, err := r.Store.publishLocked(src, want, now)
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
// earlier state, and only then is the new content deleted. When the tags
// cannot be restored the content stays, so no tag names missing content. The
// caller holds mu.
func (r *Registry) rollback(m Manifest, before []tagState) error {
	if err := r.Store.restoreTags(m.Name, before); err != nil {
		return fmt.Errorf("roll back workflow image %s %s: restore tags, keeping content: %w", m.Name, m.Version, err)
	}
	if err := removeAll(r.Store.ContentDir(m.Name, m.Digest)); err != nil {
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

// Remove removes a tag and returns the digest it named. When it was the last
// tag of that digest, the content and its row go too, and contentRemoved is
// true. The row is deleted in a transaction that commits only after the disk
// step succeeded, so a failure never leaves a row without content or content
// without a row; a failed commit after the disk step is healed by Reconcile.
func (r *Registry) Remove(name, tag string) (digest string, contentRemoved bool, err error) {
	if err := checkName("name", name); err != nil {
		return "", false, err
	}
	if err := checkName("tag", tag); err != nil {
		return "", false, err
	}
	mu.Lock()
	defer mu.Unlock()
	tx, err := r.DB.Begin()
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	digest, err = r.Store.readTag(name, tag)
	if err != nil {
		return "", false, err
	}
	tags, err := r.Store.Tags(name, digest)
	if err != nil {
		return "", false, err
	}
	last := true
	for _, other := range tags {
		if other != tag {
			last = false
		}
	}
	if last {
		if err := requireUnused(tx, digest); err != nil {
			return "", false, err
		}
		if _, err := tx.Exec(`DELETE FROM task_workflow_images WHERE digest = ?`, digest); err != nil {
			return "", false, fmt.Errorf("delete workflow image record %s: %w", digest, err)
		}
	}
	if _, contentRemoved, err = r.Store.removeTagLocked(name, tag); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return digest, contentRemoved, fmt.Errorf("delete workflow image record %s: %w", digest, err)
	}
	return digest, contentRemoved, nil
}

// rowQueryer is what imageUse needs from a *sql.DB or a *sql.Tx.
type rowQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

// imageUse counts what holds a digest: the queues bound to it, and the open
// and the closed tasks pinned to it. A table that does not exist in this
// database means nothing uses the image through it.
func imageUse(q rowQueryer, digest string) (queues, open, closed int, err error) {
	present := func(table string) (bool, error) {
		var n int
		err := q.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
		return n > 0, err
	}
	if ok, err := present("task_queue_workflows"); err != nil {
		return 0, 0, 0, err
	} else if ok {
		if err := q.QueryRow(`SELECT COUNT(*) FROM task_queue_workflows WHERE workflow_digest = ?`, digest).Scan(&queues); err != nil {
			return 0, 0, 0, fmt.Errorf("check workflow image use %s: %w", digest, err)
		}
	}
	if ok, err := present("tasks"); err != nil {
		return 0, 0, 0, err
	} else if ok {
		if err := q.QueryRow(`
			SELECT COALESCE(SUM(status NOT IN ('done', 'cancelled')), 0), COALESCE(SUM(status IN ('done', 'cancelled')), 0)
			FROM tasks WHERE workflow_digest = ?`, digest).Scan(&open, &closed); err != nil {
			return 0, 0, 0, fmt.Errorf("check workflow image use %s: %w", digest, err)
		}
	}
	return queues, open, closed, nil
}

// requireUnused returns ErrInUse when a queue is bound to the digest or any
// task, open or closed, is pinned to it: a closed task still reads its
// workflow and artifacts through the image. Task retention removes closed task
// trees, which frees the image later.
func requireUnused(tx *sql.Tx, digest string) error {
	queues, open, closed, err := imageUse(tx, digest)
	switch {
	case err != nil:
		return err
	case queues > 0:
		return fmt.Errorf("%w: %s bound to it", ErrInUse, plural(queues, "queue is", "queues are"))
	case open > 0:
		return fmt.Errorf("%w: %s pinned to it", ErrInUse, plural(open, "open task is", "open tasks are"))
	case closed > 0:
		return fmt.Errorf("%w: only %s pinned to it; task retention frees it once they are removed",
			ErrInUse, plural(closed, "closed task is", "closed tasks are"))
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Reconcile makes the rows match the stored content. It deletes rows whose
// content directory no longer exists and inserts rows for tagged content that
// has none. Called at daemon start so a crash between a disk write and its
// row heals. It never deletes a row whose content exists.
func (r *Registry) Reconcile() error {
	mu.Lock()
	defer mu.Unlock()
	if err := r.pruneOrphans(); err != nil {
		return err
	}
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

// pruneOrphans deletes rows whose content directory is missing. A row whose
// name or digest is not a valid path component is left alone, as is one whose
// directory cannot be checked for any reason other than absence, and one whose
// digest a queue is bound to or any task is pinned to.
func (r *Registry) pruneOrphans() error {
	rows, err := r.DB.Query(`SELECT digest, name FROM task_workflow_images`)
	if err != nil {
		return err
	}
	var orphans []string
	for rows.Next() {
		var digest, name string
		if err := rows.Scan(&digest, &name); err != nil {
			rows.Close()
			return err
		}
		dir := r.Store.ContentDir(name, digest)
		if dir == "" {
			continue
		}
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			orphans = append(orphans, digest)
		} else if err != nil {
			rows.Close()
			return fmt.Errorf("check workflow image content %s %s: %w", name, digest, err)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, digest := range orphans {
		queues, open, closed, err := imageUse(r.DB, digest)
		if err != nil {
			return err
		}
		if queues+open+closed > 0 {
			continue
		}
		if _, err := r.DB.Exec(`DELETE FROM task_workflow_images WHERE digest = ?`, digest); err != nil {
			return fmt.Errorf("delete workflow image record %s: %w", digest, err)
		}
	}
	return nil
}
