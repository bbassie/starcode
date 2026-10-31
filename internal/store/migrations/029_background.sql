-- Work an agent runs beside its turn (domain.ThreadBackgroundStarted):
-- a row per task that has not ended. lost marks the tasks a restart or a
-- closed agent process cut off, which the thread's next prompt names
-- (domain.ThreadBackgroundReported clears them).
CREATE TABLE IF NOT EXISTS background (
    thread_id  TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    task_id    TEXT NOT NULL,
    kind       TEXT NOT NULL,
    label      TEXT NOT NULL DEFAULT '',
    lost       INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    PRIMARY KEY (thread_id, task_id)
);
