CREATE TABLE IF NOT EXISTS session_rules (
    thread_id  TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (thread_id, key)
);

-- Reader scratch state, outside the event log: what is typed in a
-- composer, and when each thread was last looked at. Both apply to every
-- browser, which is why they are not in localStorage.
CREATE TABLE IF NOT EXISTS drafts (
    key        TEXT PRIMARY KEY,
    body       TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS seen (
    thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    seen_at   TEXT NOT NULL
);
-- Existing threads start out seen, so an upgrade does not mark the whole
-- list unread.
INSERT OR IGNORE INTO seen(thread_id, seen_at) SELECT id, updated_at FROM threads;
