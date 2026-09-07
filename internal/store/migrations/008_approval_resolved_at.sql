-- When an approval was answered, so the time a turn spent waiting on one
-- can be left out of its "worked for" count. Empty while pending, and for
-- approvals answered before this column existed.
ALTER TABLE approvals ADD COLUMN resolved_at TEXT NOT NULL DEFAULT '';
