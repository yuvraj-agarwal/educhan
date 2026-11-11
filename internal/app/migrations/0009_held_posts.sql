-- Held posts (build plan 2.9, 2.11 and 2.12).
--
-- The content filter (filter.go) keeps some posts back for a moderator instead
-- of publishing them: a threat with a place or a time, self-harm encouragement
-- or method, and an age or location question paired with a private-contact
-- cue. A held post is stored like any other so that a moderator can read it and
-- approve or delete it, and until then it is in no public view and changes
-- nothing a reader can see: no thread, index, catalog or archive shows it, it
-- cannot be quoted or resolved, and its thread's counters and bump order are
-- as if it had not been sent.
--
-- held_reason is the rule's name and nothing more: not what matched, and not
-- the text, which is already in body. Both columns are set together and
-- cleared together, when a moderator approves the post.
--
-- This is numbered 0009 although 0008 is not here: the strip-metadata branch
-- adds 0008_attachment_names.sql, and the two must not share a version.
ALTER TABLE posts
    ADD COLUMN IF NOT EXISTS held_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS held_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE posts
    ADD CONSTRAINT posts_held_reason_known
        CHECK (held_reason IN ('', 'threat', 'grooming', 'self_harm')),
    ADD CONSTRAINT posts_held_together
        CHECK ((held_at IS NULL) = (held_reason = ''));

-- The moderator queue reads held posts oldest first.
CREATE INDEX IF NOT EXISTS posts_held_queue_idx ON posts (held_at, id) WHERE held_at IS NOT NULL;

-- A thread whose opening post is held is held as a whole, and threads.held_at
-- mirrors the opening post's: set when the thread is created held, cleared when
-- its opening post is approved, and never set on any other thread. The mirror
-- is what keeps every listing a query of threads alone — the index and its
-- count, the catalog, pruning and the thread limit all filter on it as they
-- filter on archived_at — and lets one partial index cover exactly the
-- threads a reader can see, which a condition on the opening post could not.
ALTER TABLE threads ADD COLUMN IF NOT EXISTS held_at TIMESTAMPTZ;

-- A held thread is never archived, and so never swept: it is not on the board
-- to be pushed off it, and deleting it would decide its fate before anyone had
-- read it. The sweeper skips threads with a held reply for the same reason.
ALTER TABLE threads
    ADD CONSTRAINT threads_held_not_archived CHECK (held_at IS NULL OR archived_at IS NULL);

-- The ordering index covers the public set: live and not held. It replaces
-- 0005's, whose condition no public query uses on its own any more.
CREATE INDEX IF NOT EXISTS threads_board_public_order_idx
    ON threads (board_id, sticky DESC, bumped_at DESC) WHERE archived_at IS NULL AND held_at IS NULL;
DROP INDEX IF EXISTS threads_board_live_order_idx;
