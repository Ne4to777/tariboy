ALTER TABLE tasks ADD COLUMN workflow_digest TEXT;
ALTER TABLE tasks ADD COLUMN workflow_paused_reason TEXT NOT NULL DEFAULT '';
-- tasks.workflow_status (existing, nullable) holds the current status ID.
CREATE INDEX idx_tasks_workflow_digest ON tasks(workflow_digest, workflow_status);

CREATE TABLE task_queue_workflows (
    queue_prefix     TEXT PRIMARY KEY REFERENCES task_queues(prefix) ON DELETE CASCADE,
    workflow_digest  TEXT NOT NULL REFERENCES task_workflow_images(digest) ON DELETE RESTRICT,
    revision         INTEGER NOT NULL DEFAULT 1,
    updated_at       TEXT NOT NULL
);

CREATE TABLE task_workflow_holders (
    task_id       INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    pool          TEXT NOT NULL,
    agent         TEXT NOT NULL,
    dispatched_at TEXT NOT NULL,
    PRIMARY KEY (task_id, pool)
);

CREATE TABLE task_status_visits (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id           INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    sequence          INTEGER NOT NULL,
    status_id         TEXT NOT NULL,
    entered_at        TEXT NOT NULL,
    entered_by        TEXT NOT NULL,
    left_at           TEXT NOT NULL DEFAULT '',
    outcome           TEXT NOT NULL DEFAULT '',
    message           TEXT NOT NULL DEFAULT '',
    idle_iterations   INTEGER NOT NULL DEFAULT 0,
    rejected_requests INTEGER NOT NULL DEFAULT 0,
    script_failures   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (task_id, sequence)
);

CREATE TABLE task_transition_requests (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id        INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    visit_id       INTEGER NOT NULL REFERENCES task_status_visits(id) ON DELETE CASCADE,
    outcome        TEXT NOT NULL,
    message        TEXT NOT NULL DEFAULT '',
    actor          TEXT NOT NULL,
    state          TEXT NOT NULL CHECK(state IN ('pending','applied','rejected','failed','cancelled')),
    result_message TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    finished_at    TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_task_transition_requests_one_pending
    ON task_transition_requests(task_id) WHERE state = 'pending';

CREATE TABLE task_artifacts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    value      TEXT NOT NULL,
    author     TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_task_artifacts_current ON task_artifacts(task_id, name, id DESC);
