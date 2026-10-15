-- Archived threads kept past their time for a held reply.
--
-- The sweeper deletes an archived thread once its board's archive_days have
-- run out, but not one with a held reply: that would decide the held post's
-- fate before a moderator had read it (0009). Kept public, though, such a
-- thread would outlive every other thread archived with it, and the one thing
-- that could keep it there is a held post, so a reader watching the archive
-- would learn that one existed. expired_at is set by the sweep that would have
-- deleted the thread, at the moment it would have, and from then on the thread
-- is in no public view, as if it had been deleted with the rest; it stays in
-- the database for the moderator queue, and the first sweep after its last
-- held post is approved or deleted removes it.
ALTER TABLE threads ADD COLUMN IF NOT EXISTS expired_at TIMESTAMPTZ;

-- Only an archived thread outlives its archive period.
ALTER TABLE threads
    ADD CONSTRAINT threads_expired_archived CHECK (expired_at IS NULL OR archived_at IS NOT NULL);
