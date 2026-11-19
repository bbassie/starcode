-- A watch stopped by hand stays off: the automatic watch on linked pull
-- requests (setting watch_linked_prs) skips the thread until another
-- pull request is linked (domain.ThreadPRWatchStopped Off). Threads whose
-- last word on the current link was a stop by the reader, the agent or
-- the wake limit count as stopped by hand.
ALTER TABLE threads ADD COLUMN pr_watch_off INTEGER NOT NULL DEFAULT 0;
UPDATE threads SET pr_watch_off=1
WHERE pr_number>0 AND pr_watch=''
  AND coalesce((SELECT max(e.seq) FROM events e WHERE e.thread_id=threads.id AND e.type='thread.pr_watch_stopped'
        AND coalesce(json_extract(e.payload, '$.reason'), '') IN ('', 'the agent stopped watching', 'too many comment-only updates in a row')), 0)
    > coalesce((SELECT max(e.seq) FROM events e WHERE e.thread_id=threads.id AND e.type IN ('thread.pr_linked', 'thread.pr_watch_set')), 0);
