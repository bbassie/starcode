-- A prompt the thread gets when its snooze runs out on time, and the
-- thread whose agent spawned this one through the starcode tools.
ALTER TABLE threads ADD COLUMN snooze_prompt TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN parent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN wake_parent INTEGER NOT NULL DEFAULT 0;
