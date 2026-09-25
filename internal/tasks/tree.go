package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

func (s *Service) MoveTask(ctx context.Context, actor Actor, key string, in MoveInput) (Task, error) {
	if err := validateActor(actor); err != nil {
		return Task{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	task, err := taskByKey(tx, strings.TrimSpace(key))
	if err != nil {
		return Task{}, err
	}
	if err := requireWrite(ctx, tx, actor, task); err != nil {
		return Task{}, err
	}
	if in.Revision <= 0 || in.Revision != task.Revision {
		return Task{}, &Error{Status: http.StatusConflict, Code: "revision_conflict",
			Msg: "task was changed by another actor", Data: map[string]any{
				"current_revision": task.Revision, "current": task,
			}}
	}

	var targetParentID any
	var targetParentKey string
	var targetParent Task
	setTargetParent := func(parent Task) error {
		if err := requireWrite(ctx, tx, actor, parent); err != nil {
			return err
		}
		if parent.ID == task.ID {
			return domainError(http.StatusConflict, "hierarchy_cycle", "a task cannot parent itself")
		}
		if parent.Queue != task.Queue {
			return domainError(http.StatusConflict, "cross_queue_move", "tasks cannot move across queues")
		}
		if parent.Customer != task.Customer || parent.Group != task.Group {
			return domainError(http.StatusConflict, "scope_change", "move would change inherited task scope")
		}
		var descendant bool
		if err := tx.QueryRowContext(ctx, `
			WITH RECURSIVE descendants(id) AS (
				SELECT id FROM tasks WHERE parent_id = ?
				UNION ALL
				SELECT t.id FROM tasks t JOIN descendants d ON t.parent_id = d.id
			)
			SELECT EXISTS(SELECT 1 FROM descendants WHERE id = ?)`,
			task.ID, parent.ID).Scan(&descendant); err != nil {
			return err
		}
		if descendant {
			return domainError(http.StatusConflict, "hierarchy_cycle",
				"a task cannot move beneath its own descendant")
		}
		targetParentID, targetParentKey, targetParent = parent.ID, parent.Key, parent
		return nil
	}
	if strings.TrimSpace(in.ParentKey) != "" {
		targetParent, err = taskByKey(tx, strings.TrimSpace(in.ParentKey))
		if err != nil {
			return Task{}, err
		}
		if err := setTargetParent(targetParent); err != nil {
			return Task{}, err
		}
	}

	var beforeID int64
	if strings.TrimSpace(in.BeforeKey) != "" {
		before, err := taskByKey(tx, strings.TrimSpace(in.BeforeKey))
		if err != nil {
			return Task{}, err
		}
		if err := requireWrite(ctx, tx, actor, before); err != nil {
			return Task{}, err
		}
		if before.ID == task.ID {
			return Task{}, domainError(http.StatusConflict, "invalid_before",
				"a task cannot be ordered before itself")
		}
		if before.Queue != task.Queue {
			return Task{}, domainError(http.StatusConflict, "cross_queue_move", "tasks cannot move across queues")
		}
		if before.Priority != task.Priority {
			return Task{}, domainError(http.StatusConflict, "priority_bucket_mismatch",
				"before task must have the same priority")
		}
		if in.ParentKey == "" {
			if before.ParentKey != "" {
				parent, err := taskByKey(tx, before.ParentKey)
				if err != nil {
					return Task{}, err
				}
				if err := setTargetParent(parent); err != nil {
					return Task{}, err
				}
			}
		} else if before.ParentKey != targetParentKey {
			return Task{}, domainError(http.StatusConflict, "invalid_before",
				"before task must be a child of the target parent")
		}
		beforeID = before.ID
	}

	sourceParentID := any(nil)
	if task.ParentKey != "" {
		parent, err := taskByKey(tx, task.ParentKey)
		if err != nil {
			return Task{}, err
		}
		sourceParentID = parent.ID
	}
	targetIDs, err := siblingIDs(ctx, tx, task.Queue, targetParentID, task.Priority, task.ID)
	if err != nil {
		return Task{}, err
	}
	insertAt := len(targetIDs)
	if beforeID != 0 {
		for i, id := range targetIDs {
			if id == beforeID {
				insertAt = i
				break
			}
		}
	}
	targetIDs = append(targetIDs, 0)
	copy(targetIDs[insertAt+1:], targetIDs[insertAt:])
	targetIDs[insertAt] = task.ID

	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET parent_id = ?, position = 1000000000 WHERE id = ?`,
		targetParentID, task.ID); err != nil {
		return Task{}, err
	}
	if !sameNullableID(sourceParentID, targetParentID) {
		sourceIDs, err := siblingIDs(ctx, tx, task.Queue, sourceParentID, task.Priority, task.ID)
		if err != nil {
			return Task{}, err
		}
		if err := applySiblingOrder(ctx, tx, task.Queue, sourceParentID, task.Priority, sourceIDs); err != nil {
			return Task{}, err
		}
	}
	if err := applySiblingOrder(ctx, tx, task.Queue, targetParentID, task.Priority, targetIDs); err != nil {
		return Task{}, err
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET parent_id = ?, position = ?, revision = revision + 1, updated_at = ?
		WHERE id = ?`, targetParentID, insertAt, now, task.ID); err != nil {
		return Task{}, err
	}
	payload, _ := json.Marshal(map[string]any{
		"parent_key": targetParentKey, "before_key": strings.TrimSpace(in.BeforeKey), "position": insertAt,
	})
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_events(event_id, task_id, queue_prefix, kind, actor, task_revision, payload, created_at)
		VALUES (?, ?, ?, 'task.moved', ?, ?, ?, ?)`,
		newID("te"), task.ID, task.Queue, actor.Principal, task.Revision+1, string(payload), now); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	s.signal()
	moved, err := taskByKey(s.db, task.Key)
	if err == nil {
		moved.Access = "write"
	}
	return moved, err
}

func sameNullableID(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.(int64) == b.(int64)
}

func siblingIDs(ctx context.Context, tx *sql.Tx, queue string, parentID any, priority Priority, exclude int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM tasks
		WHERE queue_prefix = ? AND parent_id IS ? AND priority = ? AND id <> ?
		ORDER BY position, task_key`, queue, parentID, priority, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func applySiblingOrder(ctx context.Context, tx *sql.Tx, queue string, parentID any, priority Priority, ids []int64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET position = position + 1000000
		WHERE queue_prefix = ? AND parent_id IS ? AND priority = ?`, queue, parentID, priority); err != nil {
		return err
	}
	for position, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET position = ? WHERE id = ?`, position, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Ready(ctx context.Context, actor Actor, filter ReadyFilter) ([]Task, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return readyWith(ctx, s.db, actor, filter)
}

func readyWith(ctx context.Context, q queryer, actor Actor, filter ReadyFilter) ([]Task, error) {
	visible, err := visibleTaskIDs(ctx, q, actor)
	if err != nil {
		return nil, err
	}
	writeIDs := map[int64]string{}
	for id, access := range visible {
		if access == "write" {
			writeIDs[id] = access
		}
	}
	if len(writeIDs) == 0 {
		return []Task{}, nil
	}
	query, args := addVisibleClause(taskSelect+`
		WHERE t.status = 'open' AND t.assignee = '' AND t.manual_block_reason = ''
		  AND t.workflow_version_id IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM task_relations r
			JOIN tasks blocker ON blocker.id = r.source_id
			WHERE r.target_id = t.id AND r.type = 'blocks'
			  AND blocker.status NOT IN ('done', 'cancelled')
		  )`, writeIDs)
	if filter.Queue != "" {
		query += ` AND t.queue_prefix = ?`
		args = append(args, strings.ToUpper(strings.TrimSpace(filter.Queue)))
	}
	query += ` ORDER BY t.queue_prefix, t.priority, t.position, t.task_key`
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query += ` LIMIT ?`
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		task.Access = "write"
		out = append(out, task)
	}
	return out, rows.Err()
}

func (s *Service) ClaimReady(ctx context.Context, actor Actor, filter ReadyFilter, idempotencyKey string) (Task, error) {
	if err := validateActor(actor); err != nil {
		return Task{}, err
	}
	if actor.IsCustomer || !strings.HasPrefix(actor.Principal, "agent:") {
		return Task{}, domainError(http.StatusForbidden, "forbidden", "only an agent can claim ready work")
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	if idempotencyKey != "" {
		var raw string
		err := tx.QueryRowContext(ctx, `
			SELECT response FROM task_idempotency
			WHERE actor = ? AND action = 'claim_ready' AND idempotency_key = ?`,
			actor.Principal, idempotencyKey).Scan(&raw)
		if err == nil {
			var replay Task
			if json.Unmarshal([]byte(raw), &replay) != nil {
				return Task{}, domainError(http.StatusInternalServerError, "invalid_replay", "stored claim result is invalid")
			}
			return replay, nil
		}
		if err != sql.ErrNoRows {
			return Task{}, err
		}
	}
	ready, err := readyWith(ctx, tx, actor, ReadyFilter{Queue: filter.Queue, Limit: 1})
	if err != nil {
		return Task{}, err
	}
	if len(ready) == 0 {
		return Task{}, domainError(http.StatusNotFound, "no_ready_task", "no ready task is available")
	}
	task := ready[0]
	now := s.now()
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET assignee = ?, status = 'in_progress', revision = revision + 1, updated_at = ?
		WHERE id = ? AND status = 'open' AND assignee = ''
		  AND workflow_version_id IS NULL`,
		actor.Principal, now, task.ID)
	if err != nil {
		return Task{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return Task{}, domainError(http.StatusConflict, "claim_conflict", "task was claimed by another actor")
	}
	task.Assignee = actor.Principal
	task.Status = StatusInProgress
	if task.StartedAt == "" {
		task.StartedAt = now
	}
	task.Revision++
	task.UpdatedAt = now
	task.Access = "write"
	payload, _ := json.Marshal(map[string]any{"assignee": actor.Principal})
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_events(event_id, task_id, queue_prefix, kind, actor, task_revision, payload, created_at)
		VALUES (?, ?, ?, 'task.claimed', ?, ?, ?, ?)`,
		newID("te"), task.ID, task.Queue, actor.Principal, task.Revision, string(payload), now); err != nil {
		return Task{}, err
	}
	if idempotencyKey != "" {
		raw, _ := json.Marshal(task)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_idempotency(actor, action, idempotency_key, response, created_at)
			VALUES (?, 'claim_ready', ?, ?, ?)`,
			actor.Principal, idempotencyKey, string(raw), now); err != nil {
			return Task{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	s.signal()
	return task, nil
}

// Keep map iteration from influencing SQL argument order in future extensions.
func sortedIDs(ids map[int64]string) []int64 {
	out := make([]int64, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
