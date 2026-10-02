-- Per-queue secrets for workflow scripts. Values are plaintext, like agent
-- secrets, and are never returned by a read route.
CREATE TABLE task_queue_secrets (
    queue_prefix TEXT NOT NULL REFERENCES task_queues(prefix) ON DELETE CASCADE,
    key          TEXT NOT NULL,
    value        TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (queue_prefix, key)
);
