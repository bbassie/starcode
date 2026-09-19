-- Full-text index over prompts and replies for the palette search, so a
-- keystroke does not scan every transcript body. The store fills it when
-- an item is finished (see indexItem in store.go), not through triggers:
-- an assistant body grows one token at a time and a trigger would index
-- it again for every token. The rowid is the item's seq, which makes a
-- row cheap to find for a delete and lets the search sort by rowid.
CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(
    item_id UNINDEXED,
    thread_id UNINDEXED,
    body,
    tokenize='unicode61 remove_diacritics 2'
);
-- Emptied first so the backfill can run twice on a database that lost its
-- schema_migrations rows.
DELETE FROM items_fts;
INSERT INTO items_fts(rowid, item_id, thread_id, body)
    SELECT seq, id, thread_id, body FROM items
    WHERE kind IN ('user','assistant') AND body != '';
