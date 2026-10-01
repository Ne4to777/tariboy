-- The daemon rewrites numeric task keys left from before random keys (0044)
-- once and records that here. Random suffixes can be all digits too, so the
-- numeric pattern alone cannot tell a legacy key from a new one after that
-- first rewrite; without this marker every daemon start renamed such tasks.
CREATE TABLE task_legacy_keys_migrated (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    migrated_at TEXT NOT NULL
);
