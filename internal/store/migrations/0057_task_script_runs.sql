-- Durable records of workflow script runs: checks gating a transition request
-- and watch scripts owning a status. At most one run of a task is active.
CREATE TABLE task_script_runs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id          INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    visit_id         INTEGER NOT NULL REFERENCES task_status_visits(id) ON DELETE CASCADE,
    request_id       INTEGER REFERENCES task_transition_requests(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK(kind IN ('check','watch')),
    script           TEXT NOT NULL,
    run_as           TEXT NOT NULL CHECK(run_as IN ('queue','agent')),
    check_index      INTEGER NOT NULL DEFAULT 0,
    state            TEXT NOT NULL CHECK(state IN ('pending','running','finished','interrupted','cancelled')),
    verdict          TEXT NOT NULL DEFAULT '',
    exit_code        INTEGER,
    message          TEXT NOT NULL DEFAULT '',
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    pid              INTEGER,
    created_at       TEXT NOT NULL,
    started_at       TEXT NOT NULL DEFAULT '',
    finished_at      TEXT NOT NULL DEFAULT '',
    log_path         TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_task_script_runs_one_active
    ON task_script_runs(task_id) WHERE state IN ('pending','running');
-- When the open visit of a script status runs its watch next, in a fixed-width
-- UTC layout; '' when nothing is scheduled.
ALTER TABLE task_status_visits ADD COLUMN next_watch_at TEXT NOT NULL DEFAULT '';
