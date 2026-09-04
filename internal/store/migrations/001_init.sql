CREATE TABLE IF NOT EXISTS events (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id  TEXT,
    type       TEXT NOT NULL,
    payload    TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_thread ON events(thread_id, seq);

CREATE TABLE IF NOT EXISTS projects (
    id         TEXT PRIMARY KEY,
    path       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS threads (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title               TEXT NOT NULL,
    agent               TEXT NOT NULL,
    model               TEXT NOT NULL DEFAULT '',
    external_session_id TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'idle',
    status_detail       TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS threads_project ON threads(project_id, updated_at);

CREATE TABLE IF NOT EXISTS items (
    id         TEXT PRIMARY KEY,
    thread_id  TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    kind       TEXT NOT NULL,
    tool_name  TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL DEFAULT '',
    output     TEXT NOT NULL DEFAULT '',
    meta       TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS items_thread ON items(thread_id, seq);

CREATE TABLE IF NOT EXISTS approvals (
    id          TEXT NOT NULL,
    thread_id   TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    item_id     TEXT NOT NULL DEFAULT '',
    tool_name   TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    input       TEXT NOT NULL DEFAULT '{}',
    decision    TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    PRIMARY KEY (thread_id, id)
);
