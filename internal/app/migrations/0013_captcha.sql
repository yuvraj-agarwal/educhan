-- Captcha answers already used, so one answer works once (build plan 2.6).
--
-- A captcha challenge is signed, not stored, when it is issued: any instance
-- can check an answer to it without a lookup. Spending one is the only thing
-- that needs shared state, because the instances behind the load balancer
-- would each accept the same answer otherwise. A post or a code request that
-- carries an answer inserts its challenge here, and an insert that finds the
-- row already there is a replay.
--
-- A row is kept only until a minute after its challenge would have expired
-- anyway, which is half an hour after the challenge was issued, and each
-- insert deletes a batch of the rows past that. It holds a hash of the
-- challenge's signature, a value the server made at random, and the
-- challenge's expiry: nothing about who asked for it, solved it or used it,
-- and no reference to the post it let through.
CREATE TABLE IF NOT EXISTS captcha_spent (
    signature_hash BYTEA PRIMARY KEY,
    expires_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS captcha_spent_expires_idx ON captcha_spent (expires_at);
