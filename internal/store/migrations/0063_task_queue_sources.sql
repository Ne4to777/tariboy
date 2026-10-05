-- Runs of workflow source scripts: queue-level scripts that report items, each
-- new item key becoming a task. At most one run of a source of a queue runs.
CREATE TABLE task_queue_source_runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    queue_prefix    TEXT NOT NULL REFERENCES task_queues(prefix) ON DELETE CASCADE,
    source          TEXT NOT NULL,
    script          TEXT NOT NULL,
    workflow_digest TEXT NOT NULL,
    state           TEXT NOT NULL CHECK(state IN ('running','finished','interrupted','cancelled')),
    verdict         TEXT NOT NULL DEFAULT '',
    exit_code       INTEGER,
    message         TEXT NOT NULL DEFAULT '',
    tasks_created   INTEGER NOT NULL DEFAULT 0,
    pid             INTEGER,
    started_at      TEXT NOT NULL,
    finished_at     TEXT NOT NULL DEFAULT '',
    log_path        TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_task_queue_source_runs_one_running
    ON task_queue_source_runs(queue_prefix, source) WHERE state = 'running';
CREATE INDEX idx_task_queue_source_runs_source ON task_queue_source_runs(queue_prefix, source, id);
-- Every item key a source turned into a task. A key outlives its task, so a
-- removed task is never created again.
CREATE TABLE task_queue_source_items (
    queue_prefix TEXT NOT NULL REFERENCES task_queues(prefix) ON DELETE CASCADE,
    source       TEXT NOT NULL,
    item_key     TEXT NOT NULL,
    task_key     TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (queue_prefix, source, item_key)
);
