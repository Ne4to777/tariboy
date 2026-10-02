package tasks

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// maxArtifactBytes is the largest artifact value the daemon stores.
const maxArtifactBytes = 64 << 10

// SetArtifact stores value as the new current value of the declared artifact
// name. The customer may set an artifact on any open workflow task; an agent
// only while it holds the task in a pool-owned status.
func (s *Service) SetArtifact(ctx context.Context, actor Actor, key, name, value string) (Artifact, error) {
	if err := validateActor(actor); err != nil {
		return Artifact{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Artifact{}, err
	}
	defer tx.Rollback()
	task, manifest, err := artifactTaskTx(ctx, tx, actor, key)
	if err != nil {
		return Artifact{}, err
	}
	if task.Status == StatusDone || task.Status == StatusCancelled {
		return Artifact{}, domainError(http.StatusConflict, "workflow_closed", "task "+task.Key+" is closed")
	}
	if !actor.IsCustomer {
		if task.WorkflowPausedReason != "" {
			return Artifact{}, domainError(http.StatusConflict, "workflow_paused",
				"task "+task.Key+" is paused and waits for the customer: "+task.WorkflowPausedReason)
		}
		holds, err := holdsPoolStatusTx(ctx, tx, task, manifest, actor)
		if err != nil {
			return Artifact{}, err
		}
		if !holds {
			return Artifact{}, domainError(http.StatusForbidden, "not_holder",
				"only the holder of a pool status or the customer may set artifacts on "+task.Key)
		}
	}
	artifact, err := setArtifactTx(ctx, tx, task, manifest, name, value, actor.Principal, s.now())
	if err != nil {
		return Artifact{}, err
	}
	if err := tx.Commit(); err != nil {
		return Artifact{}, err
	}
	s.signal()
	return artifact, nil
}

// ListArtifacts returns the current value of every artifact set on the task,
// sorted by name.
func (s *Service) ListArtifacts(ctx context.Context, actor Actor, key string) ([]Artifact, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	task, _, err := workflowReadTaskTx(ctx, s.db, actor, key)
	if err != nil {
		return nil, err
	}
	return currentArtifactsTx(ctx, s.db, task.ID)
}

// currentArtifactsTx returns the current value of every artifact set on a
// task, sorted by name.
func currentArtifactsTx(ctx context.Context, q queryer, taskID int64) ([]Artifact, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT a.id, a.name, a.value, a.author, a.created_at
		FROM task_artifacts a
		WHERE a.task_id = ? AND a.id = (
			SELECT MAX(b.id) FROM task_artifacts b WHERE b.task_id = a.task_id AND b.name = a.name)
		ORDER BY a.name`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.Name, &a.Value, &a.Author, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetArtifact returns the current value of a declared artifact and its full
// history, newest first; the current value is the first history entry.
func (s *Service) GetArtifact(ctx context.Context, actor Actor, key, name string) (Artifact, []Artifact, error) {
	if err := validateActor(actor); err != nil {
		return Artifact{}, nil, err
	}
	task, manifest, err := workflowReadTaskTx(ctx, s.db, actor, key)
	if err != nil {
		return Artifact{}, nil, err
	}
	if !artifactDeclared(manifest, name) {
		return Artifact{}, nil, unknownArtifactError(manifest, name)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, value, author, created_at FROM task_artifacts
		WHERE task_id = ? AND name = ? ORDER BY id DESC`, task.ID, name)
	if err != nil {
		return Artifact{}, nil, err
	}
	defer rows.Close()
	history := []Artifact{}
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.Name, &a.Value, &a.Author, &a.CreatedAt); err != nil {
			return Artifact{}, nil, err
		}
		history = append(history, a)
	}
	if err := rows.Err(); err != nil {
		return Artifact{}, nil, err
	}
	if len(history) == 0 {
		return Artifact{}, nil, domainError(http.StatusNotFound, "artifact_not_found",
			"artifact "+name+" has no value on "+task.Key)
	}
	return history[0], history, nil
}

// taskQueryer reads tasks in or out of a transaction.
type taskQueryer interface {
	queryer
	QueryRow(query string, args ...any) *sql.Row
}

// artifactTaskTx loads the task and its manifest for an actor that may see
// the task through taskAccess; the actions that change a workflow task start
// here and then check their own rights. A flexible task has no artifacts.
func artifactTaskTx(ctx context.Context, q taskQueryer, actor Actor, key string) (Task, workflowimage.Manifest, error) {
	return workflowTaskTx(ctx, q, actor, key, taskAccess)
}

// workflowReadTaskTx is artifactTaskTx for the reads of a workflow task, which
// also admit an agent recorded as a holder of the task (readAccess).
func workflowReadTaskTx(ctx context.Context, q taskQueryer, actor Actor, key string) (Task, workflowimage.Manifest, error) {
	return workflowTaskTx(ctx, q, actor, key, readAccess)
}

func workflowTaskTx(ctx context.Context, q taskQueryer, actor Actor, key string,
	accessOf func(context.Context, queryer, Actor, int64) (string, error)) (Task, workflowimage.Manifest, error) {
	key = strings.TrimSpace(key)
	task, err := taskByKey(q, key)
	if err != nil {
		return Task{}, workflowimage.Manifest{}, err
	}
	access, err := accessOf(ctx, q, actor, task.ID)
	if err != nil {
		return Task{}, workflowimage.Manifest{}, err
	}
	if access == "" {
		return Task{}, workflowimage.Manifest{}, notFound(key)
	}
	if task.WorkflowDigest == "" {
		return Task{}, workflowimage.Manifest{}, domainError(http.StatusConflict, "workflow_not_bound",
			"task "+task.Key+" has no workflow")
	}
	manifest, err := loadManifestTx(ctx, q, task.WorkflowDigest)
	if err != nil {
		return Task{}, workflowimage.Manifest{}, err
	}
	return task, manifest, nil
}

// holdsPoolStatusTx reports whether the actor is the task's assignee and the
// recorded holder for the pool that owns the task's current status.
func holdsPoolStatusTx(ctx context.Context, q queryer, task Task, manifest workflowimage.Manifest, actor Actor) (bool, error) {
	status, ok := currentStatus(manifest, task.WorkflowStatus)
	if !ok || status.Owner.Kind != workflowfile.OwnerPool || task.Assignee != actor.Principal {
		return false, nil
	}
	var recorded string
	err := q.QueryRowContext(ctx, `SELECT agent FROM task_workflow_holders WHERE task_id = ? AND pool = ?`,
		task.ID, status.Owner.Pool).Scan(&recorded)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return agentPrincipal(recorded) == actor.Principal, nil
}

func artifactDeclared(manifest workflowimage.Manifest, name string) bool {
	for _, declared := range manifest.Definition.Artifacts {
		if declared.Name == name {
			return true
		}
	}
	return false
}

func unknownArtifactError(manifest workflowimage.Manifest, name string) error {
	names := make([]string, 0, len(manifest.Definition.Artifacts))
	for _, declared := range manifest.Definition.Artifacts {
		names = append(names, declared.Name)
	}
	err := domainError(http.StatusBadRequest, "artifact_unknown",
		"workflow "+manifest.Name+" does not declare artifact "+name).(*Error)
	err.Data = map[string]any{"artifacts": names}
	return err
}

// setArtifactTx validates and stores a new current value of a declared
// artifact and records the artifact.set event, which carries the size and
// never the value. author is the principal recorded on the value. It does not
// check who may write; the callers do.
func setArtifactTx(ctx context.Context, tx *sql.Tx, task Task, manifest workflowimage.Manifest, name, value, author, now string) (Artifact, error) {
	if !artifactDeclared(manifest, name) {
		return Artifact{}, unknownArtifactError(manifest, name)
	}
	if value == "" || !utf8.ValidString(value) {
		return Artifact{}, domainError(http.StatusBadRequest, "invalid_artifact",
			"an artifact value must be non-empty UTF-8 text")
	}
	if len(value) > maxArtifactBytes {
		return Artifact{}, domainError(http.StatusRequestEntityTooLarge, "artifact_too_large",
			"an artifact value is limited to 64 KiB")
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO task_artifacts(task_id, name, value, author, created_at) VALUES (?, ?, ?, ?, ?)`,
		task.ID, name, value, author, now)
	if err != nil {
		return Artifact{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Artifact{}, err
	}
	if _, err := appendEventTx(ctx, tx, task, "artifact.set", Actor{Principal: author},
		map[string]any{"name": name, "author": author, "bytes": len(value)}, now); err != nil {
		return Artifact{}, err
	}
	return Artifact{ID: id, Name: name, Value: value, Author: author, CreatedAt: now}, nil
}
