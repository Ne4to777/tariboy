-- Remove the unused versioned task workflow engine. Managed tasks become
-- flexible tasks that keep their current status; one workflow.removed event
-- per task records the workflow status it had.
--
-- tasks.workflow_version_id carries a foreign key, so SQLite cannot drop it
-- without rebuilding tasks. It, workflow_status, workflow_revision and the
-- task_workflow_versions table stay in place: the columns always NULL, the
-- table empty. Queue triggers, the bus ingress cursor and message sequence,
-- and agent pools are kept; none of them references a dropped table.

INSERT INTO task_events(event_id, task_id, queue_prefix, kind, actor, task_revision, payload, created_at)
SELECT 'te-' || lower(hex(randomblob(12))), t.id, t.queue_prefix, 'workflow.removed',
       'system:migration', t.revision,
       json_object('workflow_status', t.workflow_status,
                   'workflow', w.name, 'workflow_version', w.version),
       strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
FROM tasks t
LEFT JOIN task_workflow_versions w ON w.id = t.workflow_version_id
WHERE t.workflow_version_id IS NOT NULL
ORDER BY t.id;

UPDATE tasks
SET workflow_version_id = NULL, workflow_status = NULL, workflow_revision = NULL
WHERE workflow_version_id IS NOT NULL;

DROP TABLE task_observations;
DROP TABLE task_workflow_subscriptions;
DROP TABLE task_workflow_holds;
DROP TABLE task_workflow_questions;
DROP TABLE task_artifacts;
DROP TABLE task_workflow_outbox;
DROP TABLE task_assignments;
DROP TABLE task_requirement_executions;
DROP TABLE task_status_executions;
DROP TABLE task_queue_workflows;

DELETE FROM task_workflow_versions;
