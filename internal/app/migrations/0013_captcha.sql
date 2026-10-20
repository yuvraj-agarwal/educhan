-- Captcha answers already used, so one answer works once (build plan 2.6).
--
-- A captcha challenge is signed, not stored, when it is issued: any instance
-- can check its signature without a lookup. Whether it was spent is the one
-- shared fact, because the instances behind the load balancer would each
-- accept the same answer otherwise. A post or a code request looks its
-- challenge up here before flood control, so a replay is turned away before it
-- costs anything, and inserts it once flood control's first check lets it
-- through; of two requests racing with one answer, the insert lets exactly
-- one through.
--
-- A row holds a hash of the challenge's signature, a value the server made at
-- random, and the challenge's expiry rounded up to the next UTC midnight, as a
-- verification credential's is (0004), so the only date a row stores is the
-- day. Nothing about who asked for the challenge, solved it or used it, and no
-- reference to the post it let through. Each insert deletes a batch of the rows
-- past their expiry, and the daily sweep (educhan -sweep) the rest, so a row is
-- gone a day or so after its puzzle was solved. Until then the row's xmin and
-- ctid lie beside those of the post or code request it let through, written
-- just after it, which pairs the two for whoever reads the database and dates
-- the row by the post; that says only that the post carried an answer.
CREATE TABLE IF NOT EXISTS captcha_spent (
    signature_hash BYTEA PRIMARY KEY,
    expires_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS captcha_spent_expires_idx ON captcha_spent (expires_at);
