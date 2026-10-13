-- Archive instead of prune, the per-thread image cap, image spoilers, and the
-- board settings that drive them.
--
-- A thread pushed past its board's thread_limit used to be deleted on the spot.
-- It is now archived: archived_at is set, the thread leaves the index and the
-- catalog, refuses replies, and still loads by its number until the sweeper
-- (`educhan -sweep`) deletes it, media included, archive_days after that.

-- How long an archived thread is kept before the sweeper deletes it. Zero is a
-- real value — archived threads go at the next sweep — not "forever".
ALTER TABLE boards ADD COLUMN IF NOT EXISTS archive_days INTEGER NOT NULL DEFAULT 7
    CONSTRAINT boards_archive_days_check CHECK (archive_days >= 0);

-- The most posts with an image one thread may hold, the opening post included.
-- It is read literally, like bump_limit: zero means replies can never carry an
-- image, not that images are uncapped. There is no "unlimited" value, for the
-- same reason the API refuses one for the other limits.
ALTER TABLE boards ADD COLUMN IF NOT EXISTS image_limit INTEGER NOT NULL DEFAULT 150
    CONSTRAINT boards_image_limit_check CHECK (image_limit >= 0);

-- Free text shown on the board's rules page; empty means the site-wide rules
-- are all there is.
ALTER TABLE boards ADD COLUMN IF NOT EXISTS rules TEXT NOT NULL DEFAULT '';

-- NULL while the thread is live.
ALTER TABLE threads ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;

-- Posts in the thread that carry an attachment, maintained on write like
-- reply_count so the index and the catalog can show it without aggregating.
-- CreateReply recounts it from the posts themselves, under the thread lock,
-- whenever a reply brings an image, so the cap is never enforced against a
-- counter that has drifted.
ALTER TABLE threads ADD COLUMN IF NOT EXISTS image_count INTEGER NOT NULL DEFAULT 0;

UPDATE threads t SET image_count = (
    SELECT count(*) FROM posts p JOIN attachments a ON a.post_id = p.id WHERE p.thread_id = t.id
);

-- The poster asked for the image to be hidden behind a spoiler. The thumbnail
-- is still made; covering it is the client's job.
ALTER TABLE attachments ADD COLUMN IF NOT EXISTS spoiler BOOLEAN NOT NULL DEFAULT FALSE;

-- The index, the catalog, and pruning only ever look at live threads, so the
-- ordering index covers exactly those and archived rows add nothing to it.
CREATE INDEX IF NOT EXISTS threads_board_live_order_idx
    ON threads (board_id, sticky DESC, bumped_at DESC) WHERE archived_at IS NULL;
DROP INDEX IF EXISTS threads_board_order_idx;

-- The archive listing, newest archived first, and the sweeper's search for
-- archived threads that have outlived their board's archive_days.
CREATE INDEX IF NOT EXISTS threads_board_archived_idx
    ON threads (board_id, archived_at DESC) WHERE archived_at IS NOT NULL;
