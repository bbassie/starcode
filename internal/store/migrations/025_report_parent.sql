-- Children whose reply goes to the parent with the parent's next prompt
-- instead of waking it (domain.ThreadCreated.ReportParent), and whether
-- such a reply is waiting.
ALTER TABLE threads ADD COLUMN report_parent INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN report_pending INTEGER NOT NULL DEFAULT 0;
