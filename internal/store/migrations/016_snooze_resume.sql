-- A thread snoozed because its turn hit the plan's usage limit resumes
-- that turn when the snooze ends on time.
ALTER TABLE threads ADD COLUMN snooze_resume INTEGER NOT NULL DEFAULT 0;
