-- How full the model's context window is, as of the last reading the
-- agent sent. context_window is 0 when the agent named no limit; the
-- reader then falls back to the model catalog.
ALTER TABLE threads ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0;
