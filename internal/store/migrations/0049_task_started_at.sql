-- 0049_task_started_at.sql -- when a task first moved to in_progress, so the
-- task table measures work rather than time spent waiting in the queue. Set
-- once and never moved: a task paused, reopened or started again keeps it.
-- Every status write path (update, claim, a resolved customer wait and a
-- workflow transition) stamps it through one trigger instead of each statement.
-- The backfill reads the earliest start the event log still holds; a task whose
-- start was never recorded or was purged stays empty.
ALTER TABLE tasks ADD COLUMN started_at TEXT NOT NULL DEFAULT '';
UPDATE tasks SET started_at = COALESCE((
	SELECT MIN(e.created_at) FROM task_events e
	WHERE e.task_id = tasks.id
	  AND (e.kind IN ('task.claimed', 'workflow.transitioned')
	    OR (e.kind = 'task.updated'
	      AND json_extract(CASE WHEN json_valid(e.payload) THEN e.payload END, '$.status') = 'in_progress'))
), '');
CREATE TRIGGER tasks_started_at AFTER UPDATE OF status ON tasks
WHEN NEW.status = 'in_progress' AND NEW.started_at = ''
BEGIN
	UPDATE tasks SET started_at = NEW.updated_at WHERE id = NEW.id;
END;
