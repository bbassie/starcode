CREATE TABLE IF NOT EXISTS queued_prompts (
    id         TEXT PRIMARY KEY,
    thread_id  TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS queued_prompts_thread ON queued_prompts(thread_id, seq);
