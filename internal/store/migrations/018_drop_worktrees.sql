-- starcode no longer makes worktrees: threads work in the project's
-- checkout. The columns and the cleanup settings go with the feature.
DELETE FROM settings WHERE key IN ('wt_clean_days', 'wt_clean_merged');
ALTER TABLE threads DROP COLUMN worktree;
ALTER TABLE threads DROP COLUMN worktree_branch;
ALTER TABLE projects DROP COLUMN worktrees;
ALTER TABLE projects DROP COLUMN cleanup;
