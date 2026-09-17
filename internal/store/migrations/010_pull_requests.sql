-- The pull request a thread produced or works on (thread.pr_linked), and
-- what GitHub last said about each linked PR. pr_state is reader state
-- like seen and drafts: the background poll writes it, no event does, and
-- it only has to be right enough to colour a chip until the next poll.
ALTER TABLE threads ADD COLUMN pr_repo TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN pr_number INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN pr_url TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS pr_state (
    repo       TEXT NOT NULL,
    number     INTEGER NOT NULL,
    title      TEXT NOT NULL DEFAULT '',
    url        TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT '',
    review     TEXT NOT NULL DEFAULT '',
    checks     TEXT NOT NULL DEFAULT '',
    draft      INTEGER NOT NULL DEFAULT 0,
    head       TEXT NOT NULL DEFAULT '',
    checked_at TEXT NOT NULL,
    PRIMARY KEY (repo, number)
);
