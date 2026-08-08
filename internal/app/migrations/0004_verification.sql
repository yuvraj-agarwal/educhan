-- Institutional verification ("Verified Anonymity").
--
-- The privacy property these two tables exist to hold up is that a badge can
-- never be traced back to an address or to a post. Three deliberate omissions
-- carry it:
--
--   * No email column. The uniqueness key is HMAC(EDUCHAN_VERIFY_SECRET,
--     normalised address); the address itself is never written anywhere.
--   * No reference to posts or threads. There is no foreign key, and no column
--     either table could be joined on, so a badge on a post says only what it
--     displays: somebody at that institution wrote it.
--   * No created_at, and no "last used" of any kind. An issuance timestamp
--     correlates with the posts made just after it, and a use timestamp
--     correlates with a specific post, so neither is recorded. expires_at is
--     the one timestamp kept — pruning needs it — and it is rounded up to a UTC
--     day boundary so it dates an issuance only to the day.
--
-- The pending table is the exception that proves the rule: a row there does
-- carry a fifteen-minute expiry, and is deleted the moment the code is used or
-- runs out. It never becomes part of the durable record.

CREATE TABLE IF NOT EXISTS verification_pending (
    -- One address may have one code in flight; a new request replaces it.
    email_hash BYTEA PRIMARY KEY,
    -- Handed to the browser in a short-lived cookie so the address never has to
    -- be sent a second time. Stored hashed, like the code.
    token_hash BYTEA NOT NULL UNIQUE,
    code_hash  BYTEA NOT NULL,
    -- The badge this address has earned, resolved from the domain allowlist at
    -- request time so confirming needs no second lookup.
    label      TEXT NOT NULL,
    -- Wrong guesses, capped in the application so a short code stays safe.
    attempts   INTEGER NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS verification_pending_expires_idx ON verification_pending (expires_at);

CREATE TABLE IF NOT EXISTS verification_credentials (
    -- One active credential per address: re-verifying overwrites this row, and
    -- the previous credential_id_hash goes with it, which is what revokes it.
    email_hash         BYTEA PRIMARY KEY,
    -- SHA-256 of the random identifier inside the issued credential. Hashed so
    -- a database dump alone yields no usable credential.
    credential_id_hash BYTEA NOT NULL UNIQUE,
    label              TEXT NOT NULL,
    expires_at         TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS verification_credentials_expires_idx ON verification_credentials (expires_at);

-- The badge a post was made with, copied from the credential at posting time.
-- It is a label, not a reference: many posters share one, and it points at no
-- row in either table above.
ALTER TABLE posts ADD COLUMN IF NOT EXISTS badge TEXT NOT NULL DEFAULT '';
