package taskgoal

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	basestore "github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
)

// Goal is the task currently selected for an agent.
type Goal struct {
	Agent    string
	TaskKey  string
	Revision int64
	Reason   string
	Waiting  bool
}

const goalTaskSelect = `
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
       t.revision, t.created_at, t.started_at, t.updated_at, t.completed_at,
       COALESCE((
         SELECT waiting.requested_at
         FROM task_waiting_for waiting
         WHERE waiting.task_id = t.id
           AND waiting.expected_principal = t.customer
           AND waiting.resolved_at = ''
         ORDER BY waiting.requested_at, waiting.id
         LIMIT 1
       ), '')
FROM tasks t
LEFT JOIN tasks p ON p.id = t.parent_id
LEFT JOIN task_workflow_versions w ON w.id = t.workflow_version_id`

type Store struct{ db *sql.DB }

func NewStore(s *basestore.Store) *Store { return &Store{db: s.DB} }

// Set selects an active task assigned to agent as its sticky Goal.
func (s *Store) Set(agent, key string, now time.Time, activate func() (func(), error)) (tasks.Task, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return tasks.Task{}, err
	}
	defer tx.Rollback()

	var enabled bool
	var timeoutS int
	var current string
	if err := tx.QueryRow(`SELECT goal_enabled, goal_wait_customer_timeout_s, current_goal_task_key FROM agents WHERE name=?`, agent).Scan(&enabled, &timeoutS, &current); err != nil {
		return tasks.Task{}, err
	}
	if !enabled {
		return tasks.Task{}, fmt.Errorf("goal is disabled")
	}
	task, waitAt, err := readGoalTask(tx, strings.TrimSpace(key), agent)
	if err != nil {
		return tasks.Task{}, err
	}
	valid, _, err := validGoal(task, waitAt, timeoutS, now)
	if err != nil {
		return tasks.Task{}, err
	}
	if !valid {
		return tasks.Task{}, fmt.Errorf("task %s is not an active goal", task.Key)
	}
	if current != "" && current != task.Key {
		return tasks.Task{}, fmt.Errorf("goal is already set to %s", current)
	}
	changed := current == ""
	if _, err := tx.Exec(`UPDATE agents SET current_goal_task_key=? WHERE name=?`, task.Key, agent); err != nil {
		return tasks.Task{}, err
	}
	undo, err := activate()
	if err != nil {
		return tasks.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		if changed && undo != nil {
			undo()
		}
		return tasks.Task{}, err
	}
	return task, nil
}

// ReconcileAgent validates the agent's sticky goal and selects a replacement
// when necessary. Selection and persistence share one transaction.
func (s *Store) ReconcileAgent(agent string, now time.Time) (Goal, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Goal{}, err
	}
	defer tx.Rollback()

	goal, _, err := reconcileAgent(tx, agent, now)
	if err != nil {
		return Goal{}, err
	}
	if err := tx.Commit(); err != nil {
		return Goal{}, err
	}
	return goal, nil
}

// Current reconciles the sticky selection and returns its authoritative task.
func (s *Store) Current(agent string, now time.Time) (tasks.Task, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return tasks.Task{}, false, err
	}
	defer tx.Rollback()

	goal, task, err := reconcileAgent(tx, agent, now)
	if err != nil {
		return tasks.Task{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, false, err
	}
	return task, goal.TaskKey != "", nil
}

// GoalDeliveryAllowed suppresses another goal wake until the previous one is
// processed or dead-lettered and the agent's configured cooldown has elapsed.
func (s *Store) GoalDeliveryAllowed(agent string, now time.Time) (bool, error) {
	var cooldown int
	var last string
	var pending bool
	err := s.db.QueryRow(`SELECT goal_delivery_cooldown_s, COALESCE(NULLIF(last_goal_delivery_at, ''), (
		SELECT MAX(m.ts) FROM deliveries d JOIN subscriptions s ON s.id=d.subscription_id
		JOIN messages m ON m.id=d.message_id WHERE s.agent=? AND m.type='task.goal'
	), ''),
		EXISTS(SELECT 1 FROM deliveries d JOIN subscriptions s ON s.id=d.subscription_id
		JOIN messages m ON m.id=d.message_id WHERE s.agent=? AND m.type='task.goal'
		AND d.processed_at IS NULL AND d.dlq=0) FROM agents WHERE name=?`, agent, agent, agent).Scan(&cooldown, &last, &pending)
	if err != nil || pending {
		return false, err
	}
	if last == "" {
		return true, nil
	}
	at, err := parseTimestamp(last)
	if err != nil {
		return false, err
	}
	return !now.Before(at.Add(time.Duration(cooldown) * time.Second)), nil
}

func (s *Store) MarkGoalDelivered(agent string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE agents SET last_goal_delivery_at=? WHERE name=?`, now.UTC().Format(time.RFC3339Nano), agent)
	return err
}

func reconcileAgent(tx *sql.Tx, agent string, now time.Time) (Goal, tasks.Task, error) {
	var enabled, loopEnabled, goalEnabled bool
	var timeoutS int
	var current string
	if err := tx.QueryRow(`SELECT enabled, loop_enabled, goal_enabled,
		goal_wait_customer_timeout_s, current_goal_task_key
		FROM agents WHERE name=?`, agent).Scan(&enabled, &loopEnabled, &goalEnabled, &timeoutS, &current); err != nil {
		return Goal{}, tasks.Task{}, err
	}
	if !goalEnabled {
		if _, err := tx.Exec(`UPDATE agents SET current_goal_task_key='' WHERE name=?`, agent); err != nil {
			return Goal{}, tasks.Task{}, err
		}
		return Goal{}, tasks.Task{}, nil
	}
	if !enabled || !loopEnabled {
		return Goal{}, tasks.Task{}, nil
	}

	if current != "" {
		task, waitAt, err := readGoalTask(tx, current, agent)
		if err != nil && err != sql.ErrNoRows {
			return Goal{}, tasks.Task{}, err
		}
		if err == nil {
			valid, waiting, err := validGoal(task, waitAt, timeoutS, now)
			if err != nil {
				return Goal{}, tasks.Task{}, err
			}
			var higherPriority bool
			if valid {
				err = tx.QueryRow(`SELECT EXISTS(
					SELECT 1 FROM tasks t
					WHERE t.assignee='agent:' || ?
					  AND t.status IN ('in_progress','open') AND t.priority < ?
					  AND t.manual_block_reason = '' AND NOT EXISTS (
						SELECT 1 FROM task_relations r
						JOIN tasks blocker ON blocker.id = r.source_id
						WHERE r.target_id = t.id AND r.type = 'blocks'
						  AND blocker.status NOT IN ('done', 'cancelled')
					  )
				)`, agent, task.Priority).Scan(&higherPriority)
			}
			if err != nil {
				return Goal{}, tasks.Task{}, err
			}
			if valid && !higherPriority {
				return Goal{Agent: agent, TaskKey: task.Key, Revision: task.Revision, Reason: "selected", Waiting: waiting}, task, nil
			}
		}
	}

	selected := ""
	var revision int64
	err := tx.QueryRow(`
			SELECT t.task_key, t.revision
			FROM tasks t
			WHERE t.assignee='agent:' || ?
			  AND t.status IN ('in_progress','open')
			  AND t.manual_block_reason = '' AND NOT EXISTS (
				SELECT 1 FROM task_relations r
				JOIN tasks blocker ON blocker.id = r.source_id
				WHERE r.target_id = t.id AND r.type = 'blocks'
				  AND blocker.status NOT IN ('done', 'cancelled')
			  )
			ORDER BY CASE t.priority WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2 ELSE 3 END,
			         CASE t.status WHEN 'in_progress' THEN 0 ELSE 1 END,
			         t.created_at, t.task_key
			LIMIT 1`, agent).Scan(&selected, &revision)
	if err != nil && err != sql.ErrNoRows {
		return Goal{}, tasks.Task{}, err
	}

	if _, err := tx.Exec(`UPDATE agents SET current_goal_task_key=? WHERE name=?`, selected, agent); err != nil {
		return Goal{}, tasks.Task{}, err
	}
	if selected == "" {
		return Goal{}, tasks.Task{}, nil
	}

	task, _, err := readGoalTask(tx, selected, agent)
	if err != nil {
		return Goal{}, tasks.Task{}, err
	}
	return Goal{Agent: agent, TaskKey: task.Key, Revision: task.Revision, Reason: "selected"}, task, nil
}

func readGoalTask(tx *sql.Tx, key, agent string) (tasks.Task, string, error) {
	var task tasks.Task
	var blocked bool
	var waitAt string
	// A key reaches this path from an agent, so it is normalized, and a key
	// retired by the random key migration resolves through its alias — an
	// agent's stored context can still name the number it was given.
	key = tasks.NormalizeKey(key)
	err := tx.QueryRow(goalTaskSelect+`
		WHERE (t.task_key=? OR t.id=(SELECT task_id FROM task_key_aliases WHERE old_key=?))
		  AND t.assignee='agent:' || ?`, key, key, agent).Scan(
		&task.ID, &task.Key, &task.Queue, &task.ParentKey,
		&task.Position, &task.Priority, &task.Title, &task.Description, &task.Status, &task.PullRequest, &task.Author, &task.Customer,
		&task.Group, &task.Assignee, &task.ManualBlockReason, &blocked,
		&task.WorkflowVersionID, &task.WorkflowVersion, &task.WorkflowStatus, &task.WorkflowRevision,
		&task.Revision, &task.CreatedAt, &task.StartedAt, &task.UpdatedAt, &task.CompletedAt, &waitAt,
	)
	task.Blocked = blocked
	return task, waitAt, err
}

func validGoal(task tasks.Task, waitAt string, timeoutS int, now time.Time) (bool, bool, error) {
	if task.Blocked {
		return false, false, nil
	}
	switch task.Status {
	case "open", "in_progress":
		return true, false, nil
	case "wait_customer":
		if waitAt == "" {
			return false, false, nil
		}
		requestedAt, err := parseTimestamp(waitAt)
		if err != nil {
			return false, false, fmt.Errorf("parse customer wait for %s: %w", task.Key, err)
		}
		waiting := now.Before(requestedAt.Add(time.Duration(timeoutS) * time.Second))
		return waiting, waiting, nil
	default:
		return false, false, nil
	}
}

func parseTimestamp(raw string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, raw)
}
