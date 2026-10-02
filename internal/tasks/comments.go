package tasks

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"strings"
)

var mentionRE = regexp.MustCompile(`@((?:agent|user):[A-Za-z0-9][A-Za-z0-9._-]*)`)

func (s *Service) AddComment(ctx context.Context, actor Actor, key string, in AddCommentInput) (CommentResult, error) {
	if err := validateActor(actor); err != nil {
		return CommentResult{}, err
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return CommentResult{}, domainError(http.StatusBadRequest, "missing_comment", "comment body is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CommentResult{}, err
	}
	defer tx.Rollback()
	if replayed, ok, err := readTaskIdempotency[CommentResult](
		ctx, tx, actor.Principal, "add_comment", in.IdempotencyKey,
	); err != nil {
		return CommentResult{}, err
	} else if ok {
		return replayed, nil
	}
	task, err := taskByKey(tx, strings.TrimSpace(key))
	if err != nil {
		return CommentResult{}, err
	}
	if err := requireRespond(ctx, tx, actor, task); err != nil {
		return CommentResult{}, err
	}
	now := s.now()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO task_comments(task_id, author, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, task.ID, actor.Principal, body, now, now)
	if err != nil {
		return CommentResult{}, err
	}
	commentID, _ := result.LastInsertId()
	comment := Comment{
		ID: commentID, TaskKey: task.Key, Author: actor.Principal, Body: body,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	resolved, err := openWaitsForPrincipal(ctx, tx, task, actor.Principal)
	if err != nil {
		return CommentResult{}, err
	}
	if len(resolved) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE task_waiting_for
			SET resolving_comment_id = ?, resolved_at = ?
			WHERE task_id = ? AND expected_principal = ? AND resolved_at = ''`,
			commentID, now, task.ID, actor.Principal); err != nil {
			return CommentResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE task_notification_state SET read_at = ?
			WHERE customer_principal = ? AND read_at = '' AND notification_id IN (
				SELECT o.notification_id
				FROM task_notification_outbox o
				JOIN task_events e ON e.sequence = o.event_sequence
				WHERE o.message_type = 'task.question' AND e.task_id = ?
			)`, now, userPrincipal(s.customer), task.ID); err != nil {
			return CommentResult{}, err
		}
		for i := range resolved {
			resolved[i].ResolvingCommentID = commentID
			resolved[i].ResolvedAt = now
		}
	}

	mentions := s.parseMentions(body)
	created := make([]WaitingFor, 0, len(mentions))
	for _, principal := range mentions {
		wait, err := upsertWait(ctx, tx, task, principal, actor.Principal, commentID, now)
		if err != nil {
			return CommentResult{}, err
		}
		created = append(created, wait)
	}
	customerWaits, err := openWaitsForPrincipal(ctx, tx, task, task.Customer)
	if err != nil {
		return CommentResult{}, err
	}
	previousStatus := task.Status
	assignedAgentQuestion := task.Assignee == actor.Principal && !actor.IsCustomer &&
		containsPrincipal(created, task.Customer) && task.Status != StatusDone && task.Status != StatusCancelled
	lastCustomerWaitResolved := task.Status == StatusWaitCustomer &&
		containsPrincipal(resolved, task.Customer) && len(customerWaits) == 0
	switch {
	case assignedAgentQuestion:
		task.Status = StatusWaitCustomer
	case lastCustomerWaitResolved:
		task.Status = StatusInProgress
	}
	task.Revision++
	task.UpdatedAt = now
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, revision = revision + 1, updated_at = ? WHERE id = ?`,
		task.Status, now, task.ID); err != nil {
		return CommentResult{}, err
	}
	if task.Status != previousStatus {
		if _, err := appendEventTx(ctx, tx, task, "task.updated", actor,
			map[string]any{"status": task.Status, "pull_request": task.PullRequest, "assignee": task.Assignee, "priority": task.Priority}, now); err != nil {
			return CommentResult{}, err
		}
	}
	sequence, err := appendEventTx(ctx, tx, task, "task.comment_added", actor,
		map[string]any{
			"comment_id": commentID,
			"mentions":   mentions,
			"resolved":   waitPrincipals(resolved),
		}, now)
	if err != nil {
		return CommentResult{}, err
	}
	for _, wait := range created {
		if wait.ExpectedPrincipal == actor.Principal {
			continue
		}
		if err := enqueueNotificationTx(ctx, tx, sequence, wait.ExpectedPrincipal, actor.Principal,
			"task.question", task, actor.Principal+" asked for an answer on "+task.Key, now); err != nil {
			return CommentResult{}, err
		}
	}
	notified := map[string]bool{}
	for _, wait := range resolved {
		if wait.RequestingPrincipal == actor.Principal || notified[wait.RequestingPrincipal] {
			continue
		}
		if err := enqueueNotificationTx(ctx, tx, sequence, wait.RequestingPrincipal, actor.Principal,
			"task.answered", task, actor.Principal+" answered on "+task.Key, now); err != nil {
			return CommentResult{}, err
		}
		notified[wait.RequestingPrincipal] = true
	}
	response := CommentResult{Comment: comment, CreatedWaits: created, ResolvedWaits: resolved}
	if err := writeTaskIdempotency(
		ctx, tx, actor.Principal, "add_comment", in.IdempotencyKey, response, now,
	); err != nil {
		return CommentResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CommentResult{}, err
	}
	s.signal()
	return response, nil
}

func (s *Service) parseMentions(body string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, match := range mentionRE.FindAllStringSubmatch(body, -1) {
		principal := match[1]
		if strings.HasPrefix(principal, "user:") && principal != userPrincipal(s.customer) {
			continue
		}
		if seen[principal] {
			continue
		}
		seen[principal] = true
		out = append(out, principal)
	}
	return out
}

func upsertWait(
	ctx context.Context,
	tx *sql.Tx,
	task Task,
	expected string,
	requester string,
	commentID int64,
	now string,
) (WaitingFor, error) {
	result, err := tx.ExecContext(ctx, `
		UPDATE task_waiting_for
		SET requesting_principal = ?, requesting_comment_id = ?
		WHERE task_id = ? AND expected_principal = ? AND resolved_at = ''`,
		requester, commentID, task.ID, expected)
	if err != nil {
		return WaitingFor{}, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_waiting_for(
				task_id, expected_principal, requesting_principal,
				requesting_comment_id, requested_at
			) VALUES (?, ?, ?, ?, ?)`,
			task.ID, expected, requester, commentID, now); err != nil {
			return WaitingFor{}, err
		}
	}
	var wait WaitingFor
	err = tx.QueryRowContext(ctx, `
		SELECT id, expected_principal, requesting_principal,
		       requesting_comment_id, requested_at
		FROM task_waiting_for
		WHERE task_id = ? AND expected_principal = ? AND resolved_at = ''`,
		task.ID, expected).Scan(
		&wait.ID, &wait.ExpectedPrincipal, &wait.RequestingPrincipal,
		&wait.RequestingCommentID, &wait.RequestedAt)
	wait.TaskKey = task.Key
	return wait, err
}

func openWaitsForPrincipal(ctx context.Context, q queryer, task Task, principal string) ([]WaitingFor, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, expected_principal, requesting_principal,
		       requesting_comment_id, requested_at
		FROM task_waiting_for
		WHERE task_id = ? AND expected_principal = ? AND resolved_at = ''
		ORDER BY id`, task.ID, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WaitingFor{}
	for rows.Next() {
		var wait WaitingFor
		if err := rows.Scan(&wait.ID, &wait.ExpectedPrincipal, &wait.RequestingPrincipal,
			&wait.RequestingCommentID, &wait.RequestedAt); err != nil {
			return nil, err
		}
		wait.TaskKey = task.Key
		out = append(out, wait)
	}
	return out, rows.Err()
}

func listOpenWaits(ctx context.Context, q queryer, task Task) ([]WaitingFor, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, expected_principal, requesting_principal,
		       requesting_comment_id, requested_at
		FROM task_waiting_for
		WHERE task_id = ? AND resolved_at = ''
		ORDER BY id`, task.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WaitingFor{}
	for rows.Next() {
		var wait WaitingFor
		if err := rows.Scan(&wait.ID, &wait.ExpectedPrincipal, &wait.RequestingPrincipal,
			&wait.RequestingCommentID, &wait.RequestedAt); err != nil {
			return nil, err
		}
		wait.TaskKey = task.Key
		out = append(out, wait)
	}
	return out, rows.Err()
}

func listComments(ctx context.Context, q queryer, task Task) ([]Comment, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, author, body, revision, created_at, updated_at
		FROM task_comments WHERE task_id = ? ORDER BY id`, task.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var comment Comment
		if err := rows.Scan(&comment.ID, &comment.Author, &comment.Body,
			&comment.Revision, &comment.CreatedAt, &comment.UpdatedAt); err != nil {
			return nil, err
		}
		comment.TaskKey = task.Key
		out = append(out, comment)
	}
	return out, rows.Err()
}

func waitPrincipals(waits []WaitingFor) []string {
	out := make([]string, 0, len(waits))
	for _, wait := range waits {
		out = append(out, wait.ExpectedPrincipal)
	}
	return out
}

func containsPrincipal(waits []WaitingFor, principal string) bool {
	for _, wait := range waits {
		if wait.ExpectedPrincipal == principal {
			return true
		}
	}
	return false
}
