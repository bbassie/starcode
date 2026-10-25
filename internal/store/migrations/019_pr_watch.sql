-- The watch on a thread's linked pull request: the app's bookkeeping as
-- JSON, empty when the thread is not watching (thread.pr_watch_set).
ALTER TABLE threads ADD COLUMN pr_watch TEXT NOT NULL DEFAULT '';
