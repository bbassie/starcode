-- Prompts sent later, once or on a cron (schedule.set, schedule.ran).
CREATE TABLE IF NOT EXISTS schedules (
	id          TEXT PRIMARY KEY,
	thread_id   TEXT NOT NULL DEFAULT '',
	project_id  TEXT NOT NULL DEFAULT '',
	prompt      TEXT NOT NULL,
	title       TEXT NOT NULL DEFAULT '',
	next_at     TEXT NOT NULL,
	cron        TEXT NOT NULL DEFAULT '',
	tz          TEXT NOT NULL DEFAULT '',
	agent       TEXT NOT NULL DEFAULT '',
	model       TEXT NOT NULL DEFAULT '',
	effort      TEXT NOT NULL DEFAULT '',
	mode        TEXT NOT NULL DEFAULT '',
	paused      INTEGER NOT NULL DEFAULT 0,
	by_thread   TEXT NOT NULL DEFAULT '',
	last_at     TEXT NOT NULL DEFAULT '',
	last_thread TEXT NOT NULL DEFAULT '',
	last_error  TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS schedules_thread ON schedules(thread_id);
