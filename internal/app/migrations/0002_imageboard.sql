-- Turn the skeleton tables into a working imageboard.
--
-- Adds per-board post numbering (the "No. 123" every imageboard displays),
-- anonymous identity fields (tripcode, per-thread poster ID), thread state
-- (sticky/locked plus denormalised counters used by the index and catalog),
-- and the quote graph that powers >>123 links and their backlinks.

-- Board configuration and the per-board post number counter.
ALTER TABLE boards ADD COLUMN IF NOT EXISTS bump_limit   INTEGER NOT NULL DEFAULT 300;
ALTER TABLE boards ADD COLUMN IF NOT EXISTS thread_limit INTEGER NOT NULL DEFAULT 100;
ALTER TABLE boards ADD COLUMN IF NOT EXISTS next_post_no BIGINT  NOT NULL DEFAULT 1;

-- Thread state and counters maintained on write so reads stay cheap.
ALTER TABLE threads ADD COLUMN IF NOT EXISTS no           BIGINT;
ALTER TABLE threads ADD COLUMN IF NOT EXISTS sticky       BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE threads ADD COLUMN IF NOT EXISTS locked       BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE threads ADD COLUMN IF NOT EXISTS reply_count  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN IF NOT EXISTS last_post_at TIMESTAMPTZ;

-- Posts record their board (so numbers can be allocated per board) and the
-- pieces of an anonymous identity.
ALTER TABLE posts RENAME COLUMN author TO name;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS board_id  INTEGER REFERENCES boards(id) ON DELETE CASCADE;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS no        BIGINT;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS tripcode  TEXT NOT NULL DEFAULT '';
ALTER TABLE posts ADD COLUMN IF NOT EXISTS poster_id TEXT NOT NULL DEFAULT '';
ALTER TABLE posts ADD COLUMN IF NOT EXISTS is_op     BOOLEAN NOT NULL DEFAULT FALSE;

-- A thread without posts cannot be numbered and should not exist; the previous
-- schema created the opening post in the same transaction as the thread.
DELETE FROM threads t WHERE NOT EXISTS (SELECT 1 FROM posts p WHERE p.thread_id = t.id);

-- Backfill existing rows.
UPDATE posts p SET board_id = t.board_id
FROM threads t WHERE t.id = p.thread_id AND p.board_id IS NULL;

UPDATE posts p SET is_op = TRUE
FROM (SELECT DISTINCT ON (thread_id) id FROM posts ORDER BY thread_id, id) first
WHERE first.id = p.id;

WITH numbered AS (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY board_id ORDER BY id) AS no FROM posts
)
UPDATE posts p SET no = numbered.no
FROM numbered WHERE numbered.id = p.id AND p.no IS NULL;

UPDATE boards b
SET next_post_no = COALESCE((SELECT MAX(no) + 1 FROM posts p WHERE p.board_id = b.id), 1);

UPDATE threads t SET
    no           = COALESCE(t.no, (SELECT p.no FROM posts p WHERE p.thread_id = t.id AND p.is_op)),
    reply_count  = (SELECT COUNT(*) FROM posts p WHERE p.thread_id = t.id AND NOT p.is_op),
    last_post_at = COALESCE(t.last_post_at, (SELECT MAX(p.created_at) FROM posts p WHERE p.thread_id = t.id));

ALTER TABLE posts   ALTER COLUMN board_id     SET NOT NULL;
ALTER TABLE posts   ALTER COLUMN no           SET NOT NULL;
ALTER TABLE threads ALTER COLUMN no           SET NOT NULL;
ALTER TABLE threads ALTER COLUMN last_post_at SET NOT NULL;
ALTER TABLE threads ALTER COLUMN last_post_at SET DEFAULT now();

CREATE UNIQUE INDEX IF NOT EXISTS posts_board_no_idx   ON posts (board_id, no);
CREATE UNIQUE INDEX IF NOT EXISTS threads_board_no_idx ON threads (board_id, no);
CREATE UNIQUE INDEX IF NOT EXISTS posts_thread_op_idx  ON posts (thread_id) WHERE is_op;
CREATE INDEX IF NOT EXISTS posts_thread_no_idx         ON posts (thread_id, no);
CREATE INDEX IF NOT EXISTS threads_board_order_idx     ON threads (board_id, sticky DESC, bumped_at DESC);

-- Superseded by the two indexes above.
DROP INDEX IF EXISTS threads_board_bumped_idx;
DROP INDEX IF EXISTS posts_thread_created_idx;

-- Quote graph: one row per ">>123" reference that resolves to a real post.
CREATE TABLE IF NOT EXISTS post_quotes (
    from_post_id BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    to_post_id   BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    PRIMARY KEY (from_post_id, to_post_id)
);

CREATE INDEX IF NOT EXISTS post_quotes_to_idx ON post_quotes (to_post_id);
