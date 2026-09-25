package tasks

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
)

// A transfer moves a task tree from one daemon to another. Only the desktop
// app holds credentials for both daemons, so it drives the two halves below:
// Export reads the tree here, Import writes it there, and the source task is
// cancelled afterwards through the ordinary update surface. Keys survive the
// move because they are random rather than per-queue counters.

// TransferTask is one task inside a bundle. Parent links travel as keys, so a
// bundle carries no identifier that is meaningful only on the source daemon.
type TransferTask struct {
	Key               string            `json:"key"`
	ParentKey         string            `json:"parent_key"`
	Queue             string            `json:"queue"`
	Priority          Priority          `json:"priority"`
	Title             string            `json:"title"`
	Description       string            `json:"description"`
	Status            string            `json:"status"`
	PullRequest       string            `json:"pull_request"`
	Author            string            `json:"author"`
	Customer          string            `json:"customer"`
	Group             string            `json:"group"`
	Assignee          string            `json:"assignee"`
	ManualBlockReason string            `json:"manual_block_reason"`
	CreatedAt         string            `json:"created_at"`
	StartedAt         string            `json:"started_at"`
	UpdatedAt         string            `json:"updated_at"`
	CompletedAt       string            `json:"completed_at"`
	Comments          []TransferComment `json:"comments"`
}

// TransferComment keeps a comment's author and time, so the imported history
// reads as what happened rather than as a bulk write by the importer.
type TransferComment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// TransferRelation is a relation whose two endpoints are both in the bundle.
// A relation pointing outside the moved tree stays on the source daemon.
type TransferRelation struct {
	SourceKey string `json:"source_key"`
	TargetKey string `json:"target_key"`
	Type      string `json:"type"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// TransferBundle is the whole payload of one transfer: the root task, its
// descendants in parent-before-child order, and the relations inside the tree.
type TransferBundle struct {
	RootKey   string             `json:"root_key"`
	Queue     string             `json:"queue"`
	Tasks     []TransferTask     `json:"tasks"`
	Relations []TransferRelation `json:"relations"`
}

const transferSubtree = `
WITH RECURSIVE subtree(id) AS (
	SELECT id FROM tasks WHERE id = ?
	UNION ALL
	SELECT t.id FROM tasks t JOIN subtree s ON t.parent_id = s.id
)
SELECT t.id, t.task_key, COALESCE(p.task_key, ''), t.queue_prefix, t.priority, t.title,
       t.description, t.status, t.pull_request, t.author, t.customer, t.group_name,
       t.assignee, t.manual_block_reason, t.workflow_version_id,
       t.created_at, t.started_at, t.updated_at, t.completed_at
FROM tasks t
JOIN subtree s ON s.id = t.id
LEFT JOIN tasks p ON p.id = t.parent_id
ORDER BY t.id`

// ExportTask reads a task and its whole subtree as a transferable bundle.
func (s *Service) ExportTask(ctx context.Context, actor Actor, key string) (TransferBundle, error) {
	if err := validateActor(actor); err != nil {
		return TransferBundle{}, err
	}
	task, err := taskByKey(s.db, key)
	if err != nil {
		return TransferBundle{}, err
	}
	if err := requireWrite(ctx, s.db, actor, task); err != nil {
		return TransferBundle{}, err
	}
	rows, err := s.db.QueryContext(ctx, transferSubtree, task.ID)
	if err != nil {
		return TransferBundle{}, err
	}
	defer rows.Close()
	bundle := TransferBundle{RootKey: task.Key, Queue: task.Queue, Tasks: []TransferTask{}, Relations: []TransferRelation{}}
	ids := []int64{}
	byID := map[int64]int{}
	for rows.Next() {
		var id int64
		var workflowVersion sql.NullInt64
		var item TransferTask
		if err := rows.Scan(&id, &item.Key, &item.ParentKey, &item.Queue, &item.Priority, &item.Title,
			&item.Description, &item.Status, &item.PullRequest, &item.Author, &item.Customer,
			&item.Group, &item.Assignee, &item.ManualBlockReason, &workflowVersion,
			&item.CreatedAt, &item.StartedAt, &item.UpdatedAt, &item.CompletedAt); err != nil {
			return TransferBundle{}, err
		}
		// Workflow execution state (assignments, leases, requirement history) is
		// daemon-local runtime, so a managed task cannot be moved without
		// silently dropping the execution it is in the middle of.
		if workflowVersion.Valid && workflowVersion.Int64 != 0 {
			return TransferBundle{}, domainError(http.StatusBadRequest, "transfer_managed_task",
				"a task in a managed queue cannot be transferred")
		}
		if item.Key == task.Key {
			item.ParentKey = ""
		}
		item.Comments = []TransferComment{}
		byID[id] = len(bundle.Tasks)
		ids = append(ids, id)
		bundle.Tasks = append(bundle.Tasks, item)
	}
	if err := rows.Err(); err != nil {
		return TransferBundle{}, err
	}
	for _, id := range ids {
		comments, err := transferComments(ctx, s.db, id)
		if err != nil {
			return TransferBundle{}, err
		}
		bundle.Tasks[byID[id]].Comments = comments
	}
	bundle.Relations, err = transferRelations(ctx, s.db, task.ID)
	if err != nil {
		return TransferBundle{}, err
	}
	return bundle, nil
}

func transferComments(ctx context.Context, db *sql.DB, taskID int64) ([]TransferComment, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT author, body, created_at FROM task_comments WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TransferComment{}
	for rows.Next() {
		var comment TransferComment
		if err := rows.Scan(&comment.Author, &comment.Body, &comment.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, comment)
	}
	return out, rows.Err()
}

func transferRelations(ctx context.Context, db *sql.DB, rootID int64) ([]TransferRelation, error) {
	rows, err := db.QueryContext(ctx, `
		WITH RECURSIVE subtree(id) AS (
			SELECT id FROM tasks WHERE id = ?
			UNION ALL
			SELECT t.id FROM tasks t JOIN subtree s ON t.parent_id = s.id
		)
		SELECT source.task_key, target.task_key, r.type, r.created_by, r.created_at
		FROM task_relations r
		JOIN tasks source ON source.id = r.source_id
		JOIN tasks target ON target.id = r.target_id
		WHERE r.source_id IN (SELECT id FROM subtree)
		  AND r.target_id IN (SELECT id FROM subtree)
		ORDER BY r.id`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TransferRelation{}
	for rows.Next() {
		var relation TransferRelation
		if err := rows.Scan(&relation.SourceKey, &relation.TargetKey, &relation.Type,
			&relation.CreatedBy, &relation.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, relation)
	}
	return out, rows.Err()
}

// ImportTask writes an exported bundle into this daemon under its original
// keys. It refuses rather than renaming anything: a missing queue or a key that
// is already in use here means the transfer was aimed at the wrong daemon.
func (s *Service) ImportTask(ctx context.Context, actor Actor, bundle TransferBundle) (Task, error) {
	if err := validateActor(actor); err != nil {
		return Task{}, err
	}
	if !actor.IsCustomer || actor.Principal != userPrincipal(s.customer) {
		return Task{}, domainError(http.StatusForbidden, "forbidden",
			"only the daemon customer can import a transferred task")
	}
	if len(bundle.Tasks) == 0 {
		return Task{}, domainError(http.StatusBadRequest, "transfer_empty", "the bundle carries no task")
	}
	queue := strings.ToUpper(strings.TrimSpace(bundle.Queue))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_queues WHERE prefix = ?`, queue).Scan(&exists); err != nil {
		return Task{}, err
	}
	if exists == 0 {
		return Task{}, domainError(http.StatusNotFound, "queue_not_found",
			"this daemon has no queue "+queue)
	}
	if _, managed, err := activeWorkflowForQueue(ctx, tx, queue); err != nil {
		return Task{}, err
	} else if managed {
		return Task{}, domainError(http.StatusBadRequest, "transfer_managed_queue",
			"queue "+queue+" runs a workflow and cannot receive a transferred task")
	}
	now := s.now()
	ids := map[string]int64{}
	var root Task
	for _, item := range bundle.Tasks {
		key := NormalizeKey(item.Key)
		var taken int
		if err := tx.QueryRowContext(ctx, `
			SELECT (SELECT COUNT(*) FROM tasks WHERE task_key = ?)
			     + (SELECT COUNT(*) FROM task_key_aliases WHERE old_key = ?)`,
			key, key).Scan(&taken); err != nil {
			return Task{}, err
		}
		if taken > 0 {
			return Task{}, domainError(http.StatusConflict, "task_key_taken",
				"key "+key+" already exists on this daemon")
		}
		var parentID any
		if parent := NormalizeKey(item.ParentKey); parent != "" {
			id, ok := ids[parent]
			if !ok {
				return Task{}, domainError(http.StatusBadRequest, "transfer_orphan",
					"task "+key+" names a parent that is not in the bundle")
			}
			parentID = id
		}
		priority, err := NormalizePriority(item.Priority)
		if err != nil {
			return Task{}, err
		}
		var position int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(position), -1) + 1 FROM tasks
			WHERE queue_prefix = ? AND parent_id IS ? AND priority = ?`,
			queue, parentID, priority).Scan(&position); err != nil {
			return Task{}, err
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO tasks(
				task_key, queue_prefix, parent_id, position, priority, title, description, status,
				pull_request, author, customer, group_name, assignee, manual_block_reason,
				created_at, started_at, updated_at, completed_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			key, queue, parentID, position, priority, item.Title, item.Description, item.Status,
			item.PullRequest, item.Author, item.Customer, item.Group, item.Assignee,
			item.ManualBlockReason, item.CreatedAt, item.StartedAt, item.UpdatedAt, item.CompletedAt)
		if err != nil {
			return Task{}, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return Task{}, err
		}
		ids[key] = id
		for _, comment := range item.Comments {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO task_comments(task_id, author, body, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?)`,
				id, comment.Author, comment.Body, comment.CreatedAt, comment.CreatedAt); err != nil {
				return Task{}, err
			}
		}
		imported, err := taskByID(tx, id)
		if err != nil {
			return Task{}, err
		}
		if _, err := appendEventTx(ctx, tx, imported, "task.imported", actor, map[string]any{
			"task_key": key, "queue": queue, "source_root_key": bundle.RootKey,
		}, now); err != nil {
			return Task{}, err
		}
		if imported.ParentKey == "" {
			root = imported
		}
	}
	for _, relation := range bundle.Relations {
		source, target := ids[NormalizeKey(relation.SourceKey)], ids[NormalizeKey(relation.TargetKey)]
		if source == 0 || target == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_relations(source_id, target_id, type, created_by, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			source, target, relation.Type, relation.CreatedBy, relation.CreatedAt); err != nil {
			return Task{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	s.signal()
	return root, nil
}
