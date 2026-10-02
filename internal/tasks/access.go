package tasks

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// visibleTaskIDs returns write-visible tasks plus the read-only ancestor chain
// required to place them in the tree. The recursive CTE has no depth limit.
func visibleTaskIDs(ctx context.Context, q queryer, actor Actor) (map[int64]string, error) {
	out := map[int64]string{}
	if actor.IsCustomer {
		rows, err := q.QueryContext(ctx, `SELECT id FROM tasks`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out[id] = "write"
		}
		return out, rows.Err()
	}
	agent := strings.TrimPrefix(actor.Principal, "agent:")
	rows, err := q.QueryContext(ctx, `
		WITH RECURSIVE writeable(id) AS (
			-- Authorship carries write access inside a tree the agent is already responsible
			-- for, but not to a root: a root it authored in a queue it does not run is a
			-- filed report, and holding on to it would turn filing into self-assignment.
			SELECT id FROM tasks
			WHERE assignee = ? OR (author = ? AND parent_id IS NOT NULL)
			UNION
			SELECT t.id
			FROM tasks t
			WHERE EXISTS (
				SELECT 1
				FROM task_queues q
				LEFT JOIN task_queue_owners qo
				  ON qo.queue_prefix = q.prefix AND qo.agent = ?
				WHERE q.prefix = t.queue_prefix
				  AND (qo.agent IS NOT NULL OR q.responsible_agent = ?)
			)
			UNION
			SELECT t.id
			FROM tasks t
			WHERE t.group_name <> ''
			  AND EXISTS (
				SELECT 1 FROM agents ag
				WHERE ag.name = ? AND ag."group" = t.group_name
			)
			UNION
			SELECT child.id
			FROM tasks child
			JOIN writeable parent ON child.parent_id = parent.id
		),
		respondable(id) AS (
			SELECT DISTINCT t.id
			FROM tasks t
			WHERE EXISTS (
				SELECT 1 FROM task_waiting_for wf
				WHERE wf.task_id = t.id
				  AND wf.expected_principal = ?
				  AND wf.resolved_at = ''
			)
		),
		direct(id, access_rank) AS (
			SELECT id, 2 FROM writeable
			UNION
			SELECT id, 1 FROM respondable
		),
		visible(id, access_rank) AS (
			SELECT id, access_rank FROM direct
			UNION
			SELECT parent.parent_id, 0
			FROM visible
			JOIN tasks parent ON parent.id = visible.id
			WHERE parent.parent_id IS NOT NULL
		)
		SELECT id, MAX(access_rank)
		FROM visible
		GROUP BY id`,
		actor.Principal, actor.Principal, agent, agent, agent, actor.Principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var accessRank int
		if err := rows.Scan(&id, &accessRank); err != nil {
			return nil, err
		}
		switch accessRank {
		case 2:
			out[id] = "write"
		case 1:
			out[id] = "respond"
		default:
			out[id] = "context"
		}
	}
	return out, rows.Err()
}

// agentRunsQueue reports whether the actor owns or triages the queue, which is what separates
// creating work in it from filing a report into it.
func agentRunsQueue(ctx context.Context, q queryer, actor Actor, queue string) (bool, error) {
	if actor.IsCustomer {
		return true, nil
	}
	agent := strings.TrimPrefix(actor.Principal, "agent:")
	var runs bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM task_queues q
			LEFT JOIN task_queue_owners qo
			  ON qo.queue_prefix = q.prefix AND qo.agent = ?
			WHERE q.prefix = ?
			  AND (q.responsible_agent = ? OR qo.agent IS NOT NULL)
		)`, agent, queue, agent).Scan(&runs)
	return runs, err
}

func taskAccess(ctx context.Context, q queryer, actor Actor, taskID int64) (string, error) {
	visible, err := visibleTaskIDs(ctx, q, actor)
	if err != nil {
		return "", err
	}
	return visible[taskID], nil
}

func requireRespond(ctx context.Context, q queryer, actor Actor, task Task) error {
	access, err := taskAccess(ctx, q, actor, task.ID)
	if err != nil {
		return err
	}
	if access != "write" && access != "respond" {
		return notFound(task.Key)
	}
	return nil
}

func requireWrite(ctx context.Context, q queryer, actor Actor, task Task) error {
	access, err := taskAccess(ctx, q, actor, task.ID)
	if err != nil {
		return err
	}
	if access != "write" {
		return notFound(task.Key)
	}
	return nil
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func idsArgs(ids map[int64]string) []any {
	out := make([]any, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	return out
}

func addVisibleClause(query string, ids map[int64]string) (string, []any) {
	args := idsArgs(ids)
	return query + fmt.Sprintf(" AND t.id IN (%s)", placeholders(len(args))), args
}
