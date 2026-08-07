-- educhan schema. Applied idempotently on every startup so a fresh Postgres
-- instance becomes usable without a separate migration step.

CREATE TABLE IF NOT EXISTS boards (
    id          SERIAL PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS threads (
    id         BIGSERIAL PRIMARY KEY,
    board_id   INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
    subject    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    bumped_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS threads_board_bumped_idx ON threads (board_id, bumped_at DESC);

CREATE TABLE IF NOT EXISTS posts (
    id         BIGSERIAL PRIMARY KEY,
    thread_id  BIGINT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    author     TEXT NOT NULL DEFAULT 'Anonymous',
    body       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS posts_thread_created_idx ON posts (thread_id, created_at);

-- Seed a few default boards that match the communities described in the README.
INSERT INTO boards (slug, name, description) VALUES
    ('placements', 'Placements', 'Interviews, offers, and career talk'),
    ('research',   'Research',   'Papers, labs, and academic life'),
    ('general',    'General',    'Everything else about campus life')
ON CONFLICT (slug) DO NOTHING;
