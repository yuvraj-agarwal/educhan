-- The educhan Pass: a year of skipping the captcha, bought for a fee.
--
-- The privacy property these tables hold up is that a Pass can never be traced
-- to a post, and a payment can be traced to a Pass only by someone holding the
-- server's secret and the payment processor's records together. The same
-- deliberate omissions as verification (0004) carry it:
--
--   * No reference to posts or threads, and nothing a post carries points
--     here. Using a Pass writes nothing, so there is no "last used" to line up
--     against a post either.
--   * No email, name, card or address. The processor keeps those; educhan keeps
--     HMAC(EDUCHAN_PASS_SECRET, payment id), which exists only so a refund or a
--     chargeback can take back the time that payment bought.
--   * No created_at. Every timestamp (expires_at in each table, and the day a
--     payment's term ends) is rounded up to a UTC day boundary, so it dates a
--     purchase to the day and not the minute.
--
-- pass_orders is the exception, as verification_pending is for 0004: a row
-- there lives from checkout until the Pass is collected, and is deleted the
-- moment it is. Unpaid, it is deleted at its deadline, a week after checkout
-- (longer while a slow payment confirms); paid and never collected, it is
-- kept for its own term past that, so the owner can still reissue it.

CREATE TABLE IF NOT EXISTS passes (
    id         BIGSERIAL PRIMARY KEY,
    -- HMAC(EDUCHAN_PASS_SECRET, code). The code is shown to its buyer once and
    -- never stored, so a database dump alone yields no usable Pass, nor any way
    -- to check a code against it. Reissuing a lost Pass replaces this hash,
    -- which is what makes the old code stop working.
    token_hash BYTEA NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS passes_expires_idx ON passes (expires_at);

-- The payments behind a Pass: the first purchase and every renewal. Refunding
-- or disputing one takes the part of its term still to come off the Pass's
-- expiry and deletes its row; a Pass left with no time to come goes too, and
-- these rows with it.
CREATE TABLE IF NOT EXISTS pass_payments (
    payment_hash BYTEA PRIMARY KEY,
    pass_id      BIGINT NOT NULL REFERENCES passes (id) ON DELETE CASCADE,
    -- The term this payment bought: how many days it moved the expiry on,
    -- rounded up. A refund takes off no more than this.
    days         INTEGER NOT NULL,
    -- The day that term ends: the Pass's expiry just after the payment was
    -- applied, moved back with it when an earlier term is refunded. A refund
    -- takes off only the part of the term still to come, and the earliest
    -- term left is the one that becomes the origin.
    ends_at      TIMESTAMPTZ NOT NULL,
    -- True for the payment that created the Pass, false for a renewal. When
    -- the origin is refunded and the Pass goes on, the earliest payment left
    -- becomes its origin, so the Pass can always be reissued.
    origin       BOOLEAN NOT NULL
);

CREATE INDEX IF NOT EXISTS pass_payments_pass_idx ON pass_payments (pass_id);

CREATE TABLE IF NOT EXISTS pass_orders (
    -- HMAC of the processor's checkout session id, which is how the payment
    -- webhook finds the order. Keyed, so the table alone names no session.
    session_hash  BYTEA PRIMARY KEY,
    -- The buyer's browser holds the claim token in a cookie scoped to
    -- /api/pass; this is its hash. Collecting the Pass needs it, so a Pass is
    -- handed to the browser that paid for it and to nobody else. Every
    -- checkout gives the browser a new token with only its own order under
    -- it, so a claim collects one order, and a token copied before a checkout
    -- collects nothing that checkout buys.
    claim_hash    BYTEA NOT NULL,
    -- Set when the processor reports the payment complete. NULL means the
    -- buyer has not paid yet, or the webhook has not arrived.
    payment_hash  BYTEA UNIQUE,
    -- The Pass this purchase renews, when the buyer renewed one from a browser
    -- holding it. NULL buys a new Pass.
    renew_pass_id BIGINT REFERENCES passes (id) ON DELETE SET NULL,
    -- The term the order buys, in days: EDUCHAN_PASS_DAYS when the checkout
    -- started. Collecting, renewing and pruning the order all use this, so a
    -- change to the setting never changes what was already sold.
    days          INTEGER NOT NULL,
    -- The claim deadline, rounded up to a UTC midnight.
    expires_at    TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS pass_orders_claim_idx ON pass_orders (claim_hash);
CREATE INDEX IF NOT EXISTS pass_orders_expires_idx ON pass_orders (expires_at);

-- Payments refunded or disputed: the processor can deliver a refund ahead of
-- a retried "paid" event, and this is what stops that late event from turning
-- a refunded purchase into a Pass. Kept for a month, longer than the processor
-- retries an event, rounded up to a UTC midnight.
CREATE TABLE IF NOT EXISTS pass_revocations (
    payment_hash BYTEA PRIMARY KEY,
    expires_at   TIMESTAMPTZ NOT NULL
);

-- One-time links that hand a lost Pass back to its buyer: the owner makes one
-- for a payment (POST /api/pass/reissue) and sends it to the address the buyer
-- wrote in from, and whoever opens it is given a new code. The owner never sees
-- a code. A row lives until its link is used, refunded or past its deadline,
-- rounded up to a UTC midnight like an order's.
CREATE TABLE IF NOT EXISTS pass_reissues (
    -- SHA-256 of the link's random token, which is only ever in the link.
    token_hash   BYTEA PRIMARY KEY,
    -- The payment it reissues: the one that made a Pass, or a paid order not
    -- collected yet. A newer link for the same payment replaces this one.
    payment_hash BYTEA NOT NULL UNIQUE,
    expires_at   TIMESTAMPTZ NOT NULL
);
