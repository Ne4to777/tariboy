CREATE INDEX idx_task_transition_requests_visit ON task_transition_requests(visit_id, created_at);
ALTER TABLE task_status_visits ADD COLUMN resumed_at TEXT NOT NULL DEFAULT '';
