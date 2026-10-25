-- The queue's order: the event seq at first, rewritten when the reader
-- moves a queued prompt (prompt.reordered).
ALTER TABLE queued_prompts ADD COLUMN pos INTEGER NOT NULL DEFAULT 0;
UPDATE queued_prompts SET pos = seq;
