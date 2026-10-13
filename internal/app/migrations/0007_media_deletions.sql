-- Files the sweeper has taken the rows away from but not yet removed from the
-- media store.
--
-- The sweeper deletes an expired thread's rows and records the storage keys of
-- its attachments here in the same transaction; then it removes each file and
-- forgets its key. A file the store would not delete, or did not hold, keeps
-- its key, so the run reports failure and the next sweep tries again. Before
-- this table, the rows that named a file went first, so a file that could not
-- be removed — a storage outage, a sweeper pointed at the wrong bucket — stayed
-- public at its URL with nothing left to say it existed.
--
-- A key is a storage path: the board, the month and a random name. Nothing in
-- it, and nothing in this table, says who posted the file or when it was
-- removed.
CREATE TABLE IF NOT EXISTS media_deletions (
    id  BIGSERIAL PRIMARY KEY,
    key TEXT NOT NULL
);
