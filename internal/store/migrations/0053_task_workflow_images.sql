CREATE TABLE task_workflow_images (
    digest   TEXT PRIMARY KEY,
    name     TEXT NOT NULL,
    version  TEXT NOT NULL,
    manifest TEXT NOT NULL,
    built_at TEXT NOT NULL
);

CREATE INDEX idx_task_workflow_images_name ON task_workflow_images(name, version);
