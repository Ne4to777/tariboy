-- Indexes for the workflow script worker and the run readers: a task's runs
-- newest first, the runs of a visit and of a request, the active runs, and
-- the open visits with a scheduled watch. The partial WHERE clauses are
-- repeated word for word by the queries that use them.
CREATE INDEX idx_task_script_runs_task ON task_script_runs(task_id, id);
CREATE INDEX idx_task_script_runs_visit ON task_script_runs(visit_id);
CREATE INDEX idx_task_script_runs_request ON task_script_runs(request_id);
CREATE INDEX idx_task_script_runs_active
    ON task_script_runs(state) WHERE state IN ('pending', 'running');
CREATE INDEX idx_task_status_visits_next_watch
    ON task_status_visits(next_watch_at) WHERE left_at = '' AND next_watch_at <> '';
