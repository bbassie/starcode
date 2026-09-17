-- Instance-wide preferences the reader sets on the settings page, kept
-- as plain key/value pairs like drafts and seen marks: they are not
-- history, and a change should not become an event.
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
