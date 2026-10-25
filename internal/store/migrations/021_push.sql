-- Web Push subscriptions, one per browser that turned notifications on.
-- Device state, like drafts: not history, so not events.
CREATE TABLE IF NOT EXISTS push_subs (
	endpoint   TEXT PRIMARY KEY,
	p256dh     TEXT NOT NULL,
	auth       TEXT NOT NULL,
	agent      TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
