-- Image attachments. One image per post, classic imageboard style, enforced by
-- the unique constraint on post_id.
--
-- Paths are variant-relative ("general/2026/08/<random>.jpg"): the blob store
-- prefixes them with originals/ or thumbs/, so rows stay valid if the backing
-- store moves from the local filesystem to object storage.

CREATE TABLE IF NOT EXISTS attachments (
    id                 BIGSERIAL PRIMARY KEY,
    post_id            BIGINT NOT NULL UNIQUE REFERENCES posts(id) ON DELETE CASCADE,
    original_name      TEXT NOT NULL DEFAULT '',
    original_path      TEXT NOT NULL,
    thumb_path         TEXT NOT NULL,
    content_type       TEXT NOT NULL,
    thumb_content_type TEXT NOT NULL,
    byte_size          BIGINT NOT NULL,
    width              INTEGER NOT NULL,
    height             INTEGER NOT NULL,
    thumb_width        INTEGER NOT NULL,
    thumb_height       INTEGER NOT NULL,
    checksum           TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS attachments_checksum_idx ON attachments (checksum);
