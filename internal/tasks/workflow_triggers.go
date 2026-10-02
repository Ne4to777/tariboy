package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var channelSegmentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

type QueueWorkflowTrigger struct {
	ID                    int64  `json:"id"`
	Queue                 string `json:"queue"`
	Pattern               string `json:"pattern"`
	CorrelationKey        string `json:"correlation_key,omitempty"`
	Action                string `json:"action"`
	Enabled               bool   `json:"enabled"`
	CreatedBy             string `json:"created_by"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
	CreatedAfterSequence  int64  `json:"-"`
	ActivationSequenceSet bool   `json:"-"`
}

type CreateQueueWorkflowTriggerInput struct {
	Pattern        string `json:"pattern"`
	CorrelationKey string `json:"correlation_key,omitempty"`
	Action         string `json:"action"`
}

// ApplyWorkflowObservationInput is the daemon-side, post-bus-commit ingress
// contract. EventID must be the immutable bus message id.
type ApplyWorkflowObservationInput struct {
	EventID        string         `json:"event_id"`
	EventAt        string         `json:"event_at,omitempty"`
	Channel        string         `json:"channel"`
	Kind           string         `json:"kind"`
	CorrelationKey string         `json:"correlation_key,omitempty"`
	Payload        map[string]any `json:"payload,omitempty"`
	Sequence       int64          `json:"-"`
}

// ReconcileWorkflowObservations durably replays committed bus rows after the
// last successfully ingested monotonic sequence. Apply is idempotent by message
// id, so a crash between Apply and cursor advancement is harmless.
func (s *Service) ReconcileWorkflowObservations(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	// Repair messages produced by a pre-sequence binary during a rolling update.
	// The statement is one serialized SQLite write and preserves legacy row order.
	if err := repairWorkflowMessageSequences(ctx, s.db); err != nil {
		return 0, err
	}
	var lastSequence int64
	if err := s.db.QueryRowContext(ctx, `SELECT last_message_sequence FROM task_workflow_ingress_state WHERE singleton=1`).Scan(&lastSequence); err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.sequence,m.id,m.channel,m.ts,m.source,m.type,m.subject,m.text,m.data,m.kind,m.correlation_id,m.in_reply_to,m.reply_to FROM task_workflow_message_sequence s JOIN messages m ON m.id=s.message_id WHERE s.sequence>? ORDER BY s.sequence LIMIT ?`, lastSequence, limit)
	if err != nil {
		return 0, err
	}
	type ingressMessage struct {
		sequence                           int64
		id, channel, ts, source, kind, typ string
		correlation, inReplyTo, replyTo    sql.NullString
		subject, text, data                sql.NullString
	}
	messages := []ingressMessage{}
	for rows.Next() {
		var m ingressMessage
		if err := rows.Scan(&m.sequence, &m.id, &m.channel, &m.ts, &m.source, &m.typ, &m.subject, &m.text, &m.data, &m.kind, &m.correlation, &m.inReplyTo, &m.replyTo); err != nil {
			rows.Close()
			return 0, err
		}
		messages = append(messages, m)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(messages) == 0 {
		return 0, nil
	}
	advanced, err := s.advanceWorkflowIngressIfNoTargets(ctx, messages[len(messages)-1].sequence)
	if err != nil {
		return 0, err
	}
	if advanced {
		return len(messages), nil
	}
	processed := 0
	var lastProcessed int64
	for _, m := range messages {
		payload := map[string]any{"source": m.source, "text": m.text.String, "kind": m.kind, "in_reply_to": m.inReplyTo.String, "reply_to": m.replyTo.String}
		if m.subject.Valid && m.subject.String != "" {
			var subject map[string]any
			if err := json.Unmarshal([]byte(m.subject.String), &subject); err != nil {
				return processed, err
			}
			payload["subject"] = subject
		}
		if m.data.Valid && m.data.String != "" {
			var data map[string]any
			if err := json.Unmarshal([]byte(m.data.String), &data); err != nil {
				return processed, err
			}
			payload["data"] = data
		}
		if err := s.ApplyWorkflowObservation(ctx, ApplyWorkflowObservationInput{EventID: m.id, EventAt: m.ts, Channel: m.channel, Kind: m.typ, CorrelationKey: m.correlation.String, Payload: payload, Sequence: m.sequence}); err != nil {
			return processed, err
		}
		lastProcessed = m.sequence
		processed++
	}
	if lastProcessed != 0 {
		if _, err := s.db.ExecContext(ctx, `UPDATE task_workflow_ingress_state SET last_message_sequence=CASE WHEN last_message_sequence<? THEN ? ELSE last_message_sequence END WHERE singleton=1`, lastProcessed, lastProcessed); err != nil {
			return processed, err
		}
	}
	return processed, nil
}

func reserveWorkflowIngressWriter(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE task_workflow_ingress_state SET singleton=singleton WHERE singleton=1`)
	return err
}

type workflowSequenceExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func repairWorkflowMessageSequences(ctx context.Context, execer workflowSequenceExecer) error {
	_, err := execer.ExecContext(ctx, `INSERT OR IGNORE INTO task_workflow_message_sequence(message_id) SELECT m.id FROM messages m LEFT JOIN task_workflow_message_sequence s ON s.message_id=m.id WHERE s.message_id IS NULL ORDER BY m.rowid`)
	return err
}

func workflowIngressMaxSequence(ctx context.Context, tx *sql.Tx) (int64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM task_workflow_message_sequence`).Scan(&sequence)
	return sequence, err
}

// advanceWorkflowIngressIfNoTargets makes the zero-target check and cursor
// advance one serialized SQLite write. Trigger creation on another daemon must
// land wholly before the count or wholly after the cursor move.
func (s *Service) advanceWorkflowIngressIfNoTargets(ctx context.Context, last int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// BEGIN is deferred in SQLite. Take the writer reservation before reading
	// target state so another connection cannot commit a target in the gap.
	if err := reserveWorkflowIngressWriter(ctx, tx); err != nil {
		return false, err
	}
	var targets int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_queue_workflow_triggers WHERE enabled=1`).Scan(&targets); err != nil {
		return false, err
	}
	if s.workflowIngressAfterTargetCount != nil {
		s.workflowIngressAfterTargetCount()
	}
	if targets != 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_workflow_ingress_state SET last_message_sequence=CASE WHEN last_message_sequence<? THEN ? ELSE last_message_sequence END WHERE singleton=1`, last, last); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) applyQueueWorkflowTriggers(ctx context.Context, in ApplyWorkflowObservationInput) error {
	source, _ := in.Payload["source"].(string)
	if !strings.HasPrefix(source, "plugin:") {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,queue_prefix,pattern,correlation_key,action,created_by,created_at,created_after_sequence,activation_sequence_set FROM task_queue_workflow_triggers WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return err
	}
	type trigger struct {
		id, createdAfterSequence                                  int64
		activationSequenceSet                                     bool
		queue, pattern, correlation, action, createdBy, createdAt string
	}
	triggers := []trigger{}
	for rows.Next() {
		var trigger trigger
		if err := rows.Scan(&trigger.id, &trigger.queue, &trigger.pattern, &trigger.correlation, &trigger.action, &trigger.createdBy, &trigger.createdAt, &trigger.createdAfterSequence, &trigger.activationSequenceSet); err != nil {
			rows.Close()
			return err
		}
		triggers = append(triggers, trigger)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, trigger := range triggers {
		if !workflowTargetEligible(in.Sequence, trigger.createdAfterSequence, trigger.activationSequenceSet, in.EventAt, trigger.createdAt) {
			continue
		}
		if !channelPatternMatches(trigger.pattern, in.Channel) || (trigger.correlation != "" && trigger.correlation != in.CorrelationKey) {
			continue
		}
		if trigger.action != "create_task" {
			return domainError(http.StatusConflict, "invalid_trigger_action", "persisted workflow trigger action is unsupported")
		}
		title, _ := in.Payload["text"].(string)
		title = strings.TrimSpace(title)
		if title == "" {
			title = "External event: " + in.Kind
		}
		description, err := json.Marshal(map[string]any{"event_id": in.EventID, "channel": in.Channel, "correlation_key": in.CorrelationKey, "payload": in.Payload})
		if err != nil {
			return err
		}
		actor := Actor{Principal: trigger.createdBy, IsCustomer: true}
		if _, err := s.CreateTask(ctx, actor, CreateTaskInput{Queue: trigger.queue, Title: title, Description: string(description), IdempotencyKey: "workflow-trigger:" + strconv.FormatInt(trigger.id, 10) + ":" + in.EventID}); err != nil {
			return err
		}
	}
	return nil
}

func workflowTargetEligible(eventSequence, createdAfterSequence int64, activationSequenceSet bool, eventAt, createdAt string) bool {
	if eventSequence > 0 && activationSequenceSet {
		return eventSequence > createdAfterSequence
	}
	return timestampNotBefore(eventAt, createdAt)
}

func (s *Service) CreateQueueWorkflowTrigger(ctx context.Context, actor Actor, queue string, in CreateQueueWorkflowTriggerInput) (QueueWorkflowTrigger, error) {
	if err := requireOperator(actor); err != nil {
		return QueueWorkflowTrigger{}, err
	}
	queue = strings.ToUpper(strings.TrimSpace(queue))
	in.Pattern, in.CorrelationKey, in.Action = strings.TrimSpace(in.Pattern), strings.TrimSpace(in.CorrelationKey), strings.TrimSpace(in.Action)
	if !validRuntimeChannelPattern(in.Pattern) {
		return QueueWorkflowTrigger{}, domainError(http.StatusBadRequest, "invalid_channel_pattern", "trigger channel pattern is invalid")
	}
	switch strings.SplitN(in.Pattern, ":", 2)[0] {
	case "agent", "group", "user", "system":
		return QueueWorkflowTrigger{}, domainError(http.StatusBadRequest, "invalid_channel_pattern", "task-creating triggers require an external channel namespace")
	}
	if in.Action == "" {
		return QueueWorkflowTrigger{}, domainError(http.StatusBadRequest, "invalid_trigger_action", "trigger action is required")
	}
	if in.Action != "create_task" {
		return QueueWorkflowTrigger{}, domainError(http.StatusBadRequest, "invalid_trigger_action", "trigger action must be create_task")
	}
	if _, err := s.GetQueue(ctx, actor, queue); err != nil {
		return QueueWorkflowTrigger{}, err
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QueueWorkflowTrigger{}, err
	}
	defer tx.Rollback()
	if err := reserveWorkflowIngressWriter(ctx, tx); err != nil {
		return QueueWorkflowTrigger{}, err
	}
	if s.workflowActivationAfterWriterReservation != nil {
		s.workflowActivationAfterWriterReservation()
	}
	if err := repairWorkflowMessageSequences(ctx, tx); err != nil {
		return QueueWorkflowTrigger{}, err
	}
	createdAfterSequence, err := workflowIngressMaxSequence(ctx, tx)
	if err != nil {
		return QueueWorkflowTrigger{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO task_queue_workflow_triggers(queue_prefix, pattern, correlation_key, action, enabled, created_by, created_at, updated_at, created_after_sequence, activation_sequence_set) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, 1)`, queue, in.Pattern, in.CorrelationKey, in.Action, actor.Principal, now, now, createdAfterSequence)
	if err != nil {
		return QueueWorkflowTrigger{}, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return QueueWorkflowTrigger{}, err
	}
	s.workflowIngressEnabled.Store(true)
	return QueueWorkflowTrigger{ID: id, Queue: queue, Pattern: in.Pattern, CorrelationKey: in.CorrelationKey, Action: in.Action, Enabled: true, CreatedBy: actor.Principal, CreatedAt: now, UpdatedAt: now, CreatedAfterSequence: createdAfterSequence, ActivationSequenceSet: true}, nil
}

func (s *Service) ListQueueWorkflowTriggers(ctx context.Context, actor Actor, queue string) ([]QueueWorkflowTrigger, error) {
	if err := requireOperator(actor); err != nil {
		return nil, err
	}
	queue = strings.ToUpper(strings.TrimSpace(queue))
	rows, err := s.db.QueryContext(ctx, `SELECT id, pattern, correlation_key, action, enabled, created_by, created_at, updated_at, created_after_sequence, activation_sequence_set FROM task_queue_workflow_triggers WHERE queue_prefix=? ORDER BY id`, queue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueueWorkflowTrigger{}
	for rows.Next() {
		var x QueueWorkflowTrigger
		x.Queue = queue
		if err := rows.Scan(&x.ID, &x.Pattern, &x.CorrelationKey, &x.Action, &x.Enabled, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt, &x.CreatedAfterSequence, &x.ActivationSequenceSet); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Service) DeleteQueueWorkflowTrigger(ctx context.Context, actor Actor, queue string, id int64) error {
	if err := requireOperator(actor); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM task_queue_workflow_triggers WHERE id=? AND queue_prefix=?`, id, strings.ToUpper(strings.TrimSpace(queue)))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return domainError(http.StatusNotFound, "workflow_trigger_not_found", "workflow trigger not found")
	}
	s.refreshWorkflowIngressEnabled(ctx)
	return nil
}

// ApplyWorkflowObservation turns one committed bus message into tasks for every
// matching queue trigger. Trigger task creation is idempotent per trigger and
// message id, so replaying a message creates nothing new.
func (s *Service) ApplyWorkflowObservation(ctx context.Context, in ApplyWorkflowObservationInput) error {
	in.EventID, in.EventAt, in.Channel, in.Kind, in.CorrelationKey = strings.TrimSpace(in.EventID), strings.TrimSpace(in.EventAt), strings.TrimSpace(in.Channel), strings.TrimSpace(in.Kind), strings.TrimSpace(in.CorrelationKey)
	if in.EventID == "" || in.Channel == "" {
		return domainError(http.StatusBadRequest, "invalid_observation", "event id and channel are required")
	}
	if in.EventAt == "" {
		in.EventAt = s.now()
	}
	if _, err := time.Parse(time.RFC3339Nano, in.EventAt); err != nil {
		return domainError(http.StatusBadRequest, "invalid_observation", "event_at must be RFC3339")
	}
	return s.applyQueueWorkflowTriggers(ctx, in)
}

func validRuntimeChannelPattern(pattern string) bool {
	parts := strings.Split(strings.TrimSpace(pattern), ":")
	if len(parts) < 2 {
		return false
	}
	for i, p := range parts {
		if p == "*" {
			if len(parts) != 2 || i != 1 {
				return false
			}
			continue
		}
		if !channelSegmentRE.MatchString(p) {
			return false
		}
	}
	return true
}

func channelPatternMatches(pattern, channel string) bool {
	if !validRuntimeChannelPattern(pattern) || !validRuntimeChannelPattern(channel) || strings.Contains(channel, "*") {
		return false
	}
	if strings.HasSuffix(pattern, ":*") {
		return strings.HasPrefix(channel, strings.TrimSuffix(pattern, "*")) && len(strings.Split(channel, ":")) == 2
	}
	return pattern == channel
}

func timestampNotBefore(eventAt, createdAt string) bool {
	eventTime, eventErr := time.Parse(time.RFC3339Nano, eventAt)
	createdTime, createdErr := time.Parse(time.RFC3339Nano, createdAt)
	return eventErr == nil && createdErr == nil && !eventTime.Before(createdTime)
}

func requireOperator(actor Actor) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	if !actor.IsCustomer {
		return domainError(http.StatusForbidden, "forbidden", "operator access required")
	}
	return nil
}
