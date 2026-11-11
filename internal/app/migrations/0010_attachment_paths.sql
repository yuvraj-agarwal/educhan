-- Look an attachment up by the file a media request names.
--
-- A held post's image is in no public view, but its file is stored like any
-- other until the quarantine prefix (build plan 2.2) exists, so the media
-- routes ask, for every file they are asked for, whether its post is held: a
-- held post's file is served only to a request carrying the moderation key.
-- These indexes are what keep that question one lookup rather than a scan of
-- every attachment, on the route every thumbnail is fetched through.
--
-- This is numbered 0010 to follow 0009; 0008 is the strip-metadata branch's.
CREATE INDEX IF NOT EXISTS attachments_original_path_idx ON attachments (original_path);
CREATE INDEX IF NOT EXISTS attachments_thumb_path_idx ON attachments (thumb_path);
