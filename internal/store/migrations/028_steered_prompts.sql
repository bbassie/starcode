-- A queued prompt handed to the running turn (domain.PromptSteered),
-- which the agent reads at its next step. It leaves the queue when the
-- agent takes it.
ALTER TABLE queued_prompts ADD COLUMN steered INTEGER NOT NULL DEFAULT 0;
