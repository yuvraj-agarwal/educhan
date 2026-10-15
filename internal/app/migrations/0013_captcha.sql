-- Captcha answers already used, so one answer works once (build plan 2.6).
--
-- A captcha challenge is signed, not stored, when it is issued: any instance
-- can check an answer to it without a lookup. Spending one is the only thing
-- that needs shared state, because the instances behind the load balancer
-- would each accept the same answer otherwise. A post or a code request that
-- carries an answer inserts its challenge here, and an insert that finds the
-- row already there is a replay.
--
-- A row holds a hash of the challenge's signature, a value the server made at
-- random, and the challenge's expiry rounded up to the next UTC midnight, as a
-- verification credential's is (0004), so the only date a row stores is the
-- day. Nothing about who asked for the challenge, solved it or used it, and no
-- reference to the post it let through. Each insert deletes a batch of the rows
-- past their expiry, and the daily sweep (educhan -sweep) the rest, so a row is
-- gone a day or so after its puzzle was solved.
CREATE TABLE IF NOT EXISTS captcha_spent (
    signature_hash BYTEA PRIMARY KEY,
    expires_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS captcha_spent_expires_idx ON captcha_spent (expires_at);
