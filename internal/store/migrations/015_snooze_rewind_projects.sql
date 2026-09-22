-- settled_at is when a thread was settled (archived), for the order of
-- the Settled section; held_at when it was last brought back by hand,
-- which restarts the idle settle clock without counting as activity.
-- Settling used to stamp updated_at for both, which moved the thread and
-- marked it unread; an undo could not put it back where it was.
ALTER TABLE threads ADD COLUMN settled_at TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN held_at TEXT NOT NULL DEFAULT '';
UPDATE threads SET settled_at = updated_at WHERE archived = 1;
-- A snoozed thread waits on the Snoozed shelf until this time.
ALTER TABLE threads ADD COLUMN snoozed_until TEXT NOT NULL DEFAULT '';
-- anchor is where the agent's conversation can be forked to keep every
-- turn so far (the last turn's agent.TurnCompleted Anchor); fork_at is
-- set by a rewind and read by the next session start, which forks the
-- conversation there.
ALTER TABLE threads ADD COLUMN anchor TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN fork_at TEXT NOT NULL DEFAULT '';

-- A project's defaults for new threads (agent empty: none) and its
-- worktree cleanup override ('' inherits, 'off').
ALTER TABLE projects ADD COLUMN agent TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN effort TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN mode TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN cleanup TEXT NOT NULL DEFAULT '';

-- Prompts put aside with the stash key, reader state like drafts.
CREATE TABLE IF NOT EXISTS stash (
    id         TEXT PRIMARY KEY,
    body       TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
