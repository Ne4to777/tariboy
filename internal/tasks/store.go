package tasks

import (
	"database/sql"
)

const taskSelect = `
SELECT t.id, t.task_key, t.queue_prefix, COALESCE(p.task_key, ''),
       t.position, t.priority, t.title, t.description, t.status, t.pull_request, t.author, t.customer,
       t.group_name, t.assignee, t.manual_block_reason,
       EXISTS (
         SELECT 1
         FROM task_relations r
         JOIN tasks blocker ON blocker.id = r.source_id
         WHERE r.target_id = t.id AND r.type = 'blocks'
           AND blocker.status NOT IN ('done', 'cancelled')
       ) OR t.manual_block_reason <> '',
	   COALESCE(t.workflow_version_id, 0),
	   COALESCE(w.name || '@' || w.version, ''),
	   COALESCE(t.workflow_status, ''), COALESCE(t.workflow_revision, 0),
       t.revision, t.created_at, t.started_at, t.updated_at, t.completed_at
FROM tasks t
LEFT JOIN tasks p ON p.id = t.parent_id
LEFT JOIN task_workflow_versions w ON w.id = t.workflow_version_id`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (Task, error) {
	var t Task
	var blocked bool
	err := row.Scan(
		&t.ID, &t.Key, &t.Queue, &t.ParentKey,
		&t.Position, &t.Priority, &t.Title, &t.Description, &t.Status, &t.PullRequest, &t.Author, &t.Customer,
		&t.Group, &t.Assignee, &t.ManualBlockReason, &blocked,
		&t.WorkflowVersionID, &t.WorkflowVersion, &t.WorkflowStatus, &t.WorkflowRevision,
		&t.Revision, &t.CreatedAt, &t.StartedAt, &t.UpdatedAt, &t.CompletedAt,
	)
	t.Blocked = blocked
	return t, err
}

// taskByKey is the single key lookup for the whole service, so key spelling is
// normalized here rather than at every caller, and a key retired by the random
// key migration keeps resolving through its alias.
func taskByKey(q interface {
	QueryRow(query string, args ...any) *sql.Row
}, key string) (Task, error) {
	normalized := NormalizeKey(key)
	t, err := scanTask(q.QueryRow(taskSelect+`
		WHERE t.task_key = ?
		   OR t.id = (SELECT task_id FROM task_key_aliases WHERE old_key = ?)`,
		normalized, normalized))
	if err == sql.ErrNoRows {
		return Task{}, notFound(key)
	}
	return t, err
}

func taskByID(q interface {
	QueryRow(query string, args ...any) *sql.Row
}, id int64) (Task, error) {
	t, err := scanTask(q.QueryRow(taskSelect+` WHERE t.id = ?`, id))
	if err == sql.ErrNoRows {
		return Task{}, notFound("")
	}
	return t, err
}
