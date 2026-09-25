package tasks

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
)

func validStatus(status string) bool {
	switch status {
	case StatusOpen, StatusInProgress, StatusWaitCustomer, StatusDone, StatusCancelled:
		return true
	default:
		return false
	}
}

func descendantCounts(ctx context.Context, q queryer, taskID int64) (total int, active int, err error) {
	err = q.QueryRowContext(ctx, `
		WITH RECURSIVE descendants(id, status) AS (
			SELECT id, status FROM tasks WHERE parent_id = ?
			UNION ALL
			SELECT t.id, t.status
			FROM tasks t JOIN descendants d ON t.parent_id = d.id
		)
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status NOT IN ('done', 'cancelled') THEN 1 ELSE 0 END), 0)
		FROM descendants`, taskID).Scan(&total, &active)
	return total, active, err
}

func (s *Service) UpdateTask(ctx context.Context, actor Actor, key string, in UpdateTaskInput) (Task, error) {
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
	if task.WorkflowVersionID != 0 &&
		(in.Status != nil || in.Assignee != nil || in.ManualBlockReason != nil) {
		return Task{}, workflowManagedError()
	}
	if in.Revision <= 0 || in.Revision != task.Revision {
		return Task{}, &Error{Status: http.StatusConflict, Code: "revision_conflict",
			Msg: "task was changed by another actor", Data: map[string]any{
				"current_revision": task.Revision, "current": task,
			}}
	}
	previousAssignee := task.Assignee
	if in.Title != nil {
		task.Title = strings.TrimSpace(*in.Title)
		if task.Title == "" {
			return Task{}, domainError(http.StatusBadRequest, "missing_title", "task title is required")
		}
	}
	if in.Description != nil {
		task.Description = strings.TrimSpace(*in.Description)
	}
	if in.PullRequest != nil {
		task.PullRequest, err = NormalizePullRequest(*in.PullRequest)
		if err != nil {
			return Task{}, err
		}
	}
	if in.Assignee != nil {
		task.Assignee = normalizeAssignee(*in.Assignee)
	}
	if in.ManualBlockReason != nil {
		task.ManualBlockReason = strings.TrimSpace(*in.ManualBlockReason)
	}
	if in.Priority != nil {
		priority, err := NormalizePriority(*in.Priority)
		if err != nil {
			return Task{}, err
		}
		task.Priority = priority
	}
	if in.Status != nil {
		task.Status = strings.TrimSpace(*in.Status)
		if !validStatus(task.Status) {
			return Task{}, domainError(http.StatusBadRequest, "invalid_status", "invalid task status")
		}
		if task.Status == StatusDone {
			_, count, err := descendantCounts(ctx, tx, task.ID)
			if err != nil {
				return Task{}, err
			}
			if count > 0 {
				return Task{}, &Error{Status: http.StatusConflict, Code: "active_descendants",
					Msg: "task has active descendants", Data: map[string]any{"active_descendants": count}}
			}
		}
	}
	now := s.now()
	task.Revision++
	task.UpdatedAt = now
	if task.Status == StatusDone {
		task.CompletedAt = now
	} else {
		task.CompletedAt = ""
	}
	// The tasks_started_at trigger stores the first start; mirror it in the reply.
	if task.Status == StatusInProgress && task.StartedAt == "" {
		task.StartedAt = now
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET title = ?, description = ?, status = ?, pull_request = ?, assignee = ?, priority = ?,
		    manual_block_reason = ?, revision = ?, updated_at = ?, completed_at = ?
		WHERE id = ?`,
		task.Title, task.Description, task.Status, task.PullRequest, task.Assignee, task.Priority,
		task.ManualBlockReason, task.Revision, now, task.CompletedAt, task.ID); err != nil {
		return Task{}, err
	}
	sequence, err := appendEventTx(ctx, tx, task, "task.updated", actor,
		map[string]any{"status": task.Status, "pull_request": task.PullRequest, "assignee": task.Assignee, "priority": task.Priority}, now)
	if err != nil {
		return Task{}, err
	}
	if task.Assignee != "" && task.Assignee != previousAssignee && task.Assignee != actor.Principal {
		if err := enqueueNotificationTx(ctx, tx, sequence, task.Assignee, actor.Principal,
			"task.assigned", task, "Task assigned: "+task.Key+" "+task.Title, now); err != nil {
			return Task{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	s.signal()
	task.Access = "write"
	return task, nil
}

func (s *Service) CompleteTask(ctx context.Context, actor Actor, key string, in CompleteInput) (Task, error) {
	if !in.CompleteAnyway {
		status := StatusDone
		return s.UpdateTask(ctx, actor, key, UpdateTaskInput{Status: &status, Revision: in.Revision})
	}
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
	if task.WorkflowVersionID != 0 {
		return Task{}, workflowManagedError()
	}
	if in.Revision <= 0 || in.Revision != task.Revision {
		return Task{}, &Error{Status: http.StatusConflict, Code: "revision_conflict",
			Msg: "task was changed by another actor", Data: map[string]any{
				"current_revision": task.Revision, "current": task,
			}}
	}
	now := s.now()
	task.Status, task.CompletedAt, task.UpdatedAt = StatusDone, now, now
	task.Revision++
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = 'done', completed_at = ?, updated_at = ?, revision = ?
		WHERE id = ?`, now, now, task.Revision, task.ID); err != nil {
		return Task{}, err
	}
	if _, err := appendEventTx(ctx, tx, task, "task.completed_anyway", actor,
		map[string]any{"complete_anyway": true}, now); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	s.signal()
	task.Access = "write"
	return task, nil
}

func (s *Service) AddRelation(ctx context.Context, actor Actor, key string, in RelationInput) (Relation, error) {
	if err := validateActor(actor); err != nil {
		return Relation{}, err
	}
	in.Type = strings.TrimSpace(in.Type)
	if in.Type != "blocks" && in.Type != "related" {
		return Relation{}, domainError(http.StatusBadRequest, "invalid_relation", "relation type must be blocks or related")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Relation{}, err
	}
	defer tx.Rollback()
	if replayed, ok, err := readTaskIdempotency[Relation](
		ctx, tx, actor.Principal, "add_relation", in.IdempotencyKey,
	); err != nil {
		return Relation{}, err
	} else if ok {
		return replayed, nil
	}
	source, err := taskByKey(tx, strings.TrimSpace(key))
	if err != nil {
		return Relation{}, err
	}
	target, err := taskByKey(tx, strings.TrimSpace(in.TargetKey))
	if err != nil {
		return Relation{}, err
	}
	if source.ID == target.ID {
		return Relation{}, domainError(http.StatusBadRequest, "self_relation", "a task cannot relate to itself")
	}
	if err := requireWrite(ctx, tx, actor, source); err != nil {
		return Relation{}, err
	}
	mutationTask := source
	if in.Revision <= 0 || in.Revision != mutationTask.Revision {
		return Relation{}, &Error{Status: http.StatusConflict, Code: "revision_conflict",
			Msg: "task was changed by another actor", Data: map[string]any{
				"current_revision": mutationTask.Revision, "current": mutationTask,
			}}
	}
	if err := requireWrite(ctx, tx, actor, target); err != nil {
		return Relation{}, err
	}
	counterpart := target
	if in.Type == "related" && source.ID > target.ID {
		source, target = target, source
	}
	if in.Type == "blocks" {
		var cycle bool
		if err := tx.QueryRowContext(ctx, `
			WITH RECURSIVE path(id) AS (
				SELECT ?
				UNION
				SELECT r.target_id
				FROM task_relations r JOIN path p ON r.source_id = p.id
				WHERE r.type = 'blocks'
			)
			SELECT EXISTS(SELECT 1 FROM path WHERE id = ?)`,
			target.ID, source.ID).Scan(&cycle); err != nil {
			return Relation{}, err
		}
		if cycle {
			return Relation{}, domainError(http.StatusConflict, "blocking_cycle",
				"blocking relation would create a cycle")
		}
	}
	now := s.now()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO task_relations(source_id, target_id, type, created_by, created_at)
		VALUES (?, ?, ?, ?, ?)`, source.ID, target.ID, in.Type, actor.Principal, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Relation{}, domainError(http.StatusConflict, "relation_exists", "relation already exists")
		}
		return Relation{}, err
	}
	id, _ := result.LastInsertId()
	mutationTask.Revision++
	mutationTask.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET revision = ?, updated_at = ?
		WHERE id = ? AND revision = ?`,
		mutationTask.Revision, now, mutationTask.ID, in.Revision); err != nil {
		return Relation{}, err
	}
	eventPayload := map[string]any{"relation_id": id, "type": in.Type}
	if _, err := appendEventTx(ctx, tx, mutationTask, "task.relation_added", actor,
		eventPayload, now); err != nil {
		return Relation{}, err
	}
	if _, err := appendEventTx(ctx, tx, counterpart, "task.relation_added", actor,
		eventPayload, now); err != nil {
		return Relation{}, err
	}
	relation := Relation{ID: id,
		SourceKey: source.Key, SourceTitle: source.Title, SourceStatus: string(source.Status),
		TargetKey: target.Key, TargetTitle: target.Title, TargetStatus: string(target.Status),
		Type: in.Type, CreatedBy: actor.Principal, CreatedAt: now}
	if err := writeTaskIdempotency(
		ctx, tx, actor.Principal, "add_relation", in.IdempotencyKey, relation, now,
	); err != nil {
		return Relation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Relation{}, err
	}
	s.signal()
	return relation, nil
}

func (s *Service) DeleteRelation(ctx context.Context, actor Actor, key string, in DeleteRelationInput) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, ok, err := readTaskIdempotency[bool](
		ctx, tx, actor.Principal, "delete_relation", in.IdempotencyKey,
	); err != nil {
		return err
	} else if ok {
		return nil
	}
	task, err := taskByKey(tx, strings.TrimSpace(key))
	if err != nil {
		return err
	}
	if err := requireWrite(ctx, tx, actor, task); err != nil {
		return err
	}
	if in.Revision <= 0 || in.Revision != task.Revision {
		return &Error{Status: http.StatusConflict, Code: "revision_conflict",
			Msg: "task was changed by another actor", Data: map[string]any{
				"current_revision": task.Revision, "current": task,
			}}
	}
	var sourceID, targetID int64
	var relationType string
	if err := tx.QueryRowContext(ctx, `
		SELECT source_id, target_id, type FROM task_relations
		WHERE id = ? AND (source_id = ? OR target_id = ?)`,
		in.RelationID, task.ID, task.ID).Scan(&sourceID, &targetID, &relationType); err != nil {
		if err == sql.ErrNoRows {
			return domainError(http.StatusNotFound, "relation_not_found", "relation not found")
		}
		return err
	}
	source, err := taskByID(tx, sourceID)
	if err != nil {
		return err
	}
	target, err := taskByID(tx, targetID)
	if err != nil {
		return err
	}
	if err := requireWrite(ctx, tx, actor, source); err != nil {
		return err
	}
	if err := requireWrite(ctx, tx, actor, target); err != nil {
		return err
	}
	counterpart := source
	if source.ID == task.ID {
		counterpart = target
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_relations WHERE id = ?`, in.RelationID); err != nil {
		return err
	}
	task.Revision++
	now := s.now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET revision = revision + 1, updated_at = ? WHERE id = ?`,
		now, task.ID); err != nil {
		return err
	}
	eventPayload := map[string]any{"relation_id": in.RelationID, "type": relationType}
	if _, err := appendEventTx(ctx, tx, task, "task.relation_removed", actor,
		eventPayload, now); err != nil {
		return err
	}
	if _, err := appendEventTx(ctx, tx, counterpart, "task.relation_removed", actor,
		eventPayload, now); err != nil {
		return err
	}
	if err := writeTaskIdempotency(
		ctx, tx, actor.Principal, "delete_relation", in.IdempotencyKey, true, now,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.signal()
	return nil
}

func (s *Service) ClaimTask(ctx context.Context, actor Actor, key string, revision int64) (Task, error) {
	if actor.IsCustomer || !strings.HasPrefix(actor.Principal, "agent:") {
		return Task{}, domainError(http.StatusForbidden, "forbidden", "only an agent can claim a task")
	}
	detail, err := s.GetTask(ctx, actor, key)
	if err != nil {
		return Task{}, err
	}
	if detail.Task.WorkflowVersionID != 0 {
		return Task{}, workflowManagedError()
	}
	if detail.Task.Blocked {
		return Task{}, domainError(http.StatusConflict, "task_blocked", "blocked task cannot be claimed")
	}
	if detail.Task.Assignee != "" && detail.Task.Assignee != actor.Principal {
		return Task{}, domainError(http.StatusConflict, "already_assigned", "task is assigned to another principal")
	}
	assignee, status := actor.Principal, StatusInProgress
	return s.UpdateTask(ctx, actor, key, UpdateTaskInput{
		Assignee: &assignee,
		Status:   &status,
		Revision: revision,
	})
}

func workflowManagedError() error {
	return domainError(http.StatusConflict, "workflow_managed",
		"task lifecycle is managed by its workflow")
}

func listRelations(ctx context.Context, q queryer, actor Actor, task Task) ([]Relation, error) {
	visible, err := visibleTaskIDs(ctx, q, actor)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT r.id, r.source_id, source.task_key, source.title, source.status,
		       r.target_id, target.task_key, target.title, target.status,
		       r.type, r.created_by, r.created_at
		FROM task_relations r
		JOIN tasks source ON source.id = r.source_id
		JOIN tasks target ON target.id = r.target_id
		WHERE r.source_id = ? OR r.target_id = ?
		ORDER BY r.id`, task.ID, task.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Relation{}
	for rows.Next() {
		var relation Relation
		var sourceID, targetID int64
		if err := rows.Scan(&relation.ID,
			&sourceID, &relation.SourceKey, &relation.SourceTitle, &relation.SourceStatus,
			&targetID, &relation.TargetKey, &relation.TargetTitle, &relation.TargetStatus,
			&relation.Type, &relation.CreatedBy, &relation.CreatedAt); err != nil {
			return nil, err
		}
		otherID := sourceID
		if sourceID == task.ID {
			otherID = targetID
		}
		if visible[otherID] == "" {
			continue
		}
		out = append(out, relation)
	}
	return out, rows.Err()
}
