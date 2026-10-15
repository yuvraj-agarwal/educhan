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
--     chargeback can cancel the Pass it paid for.
--   * No created_at. expires_at is the one timestamp in every table, rounded
--     up to a UTC day boundary, so it dates a purchase to the day and not the
--     minute.
--
-- pass_orders is the exception, as verification_pending is for 0004: a row
-- there lives from checkout until the Pass is collected, and is deleted the
-- moment it is. Unpaid, it is deleted at its deadline, a week after checkout
-- (longer while a slow payment confirms); paid and never collected, it is
-- kept for a term past that, so the owner can still reissue it.

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
-- or disputing one takes its term off the Pass's expiry and deletes its row;
-- a Pass left with no time to come goes too, and these rows with it.
CREATE TABLE IF NOT EXISTS pass_payments (
    payment_hash BYTEA PRIMARY KEY,
    pass_id      BIGINT NOT NULL REFERENCES passes (id) ON DELETE CASCADE,
    -- The term this payment bought: how many days it moved the expiry on,
    -- rounded up. A refund takes exactly this off again.
    days         INTEGER NOT NULL,
    -- True for the payment that created the Pass, false for a renewal.
    origin       BOOLEAN NOT NULL
);

CREATE INDEX IF NOT EXISTS pass_payments_pass_idx ON pass_payments (pass_id);

CREATE TABLE IF NOT EXISTS pass_orders (
    -- HMAC of the processor's checkout session id, which is how the payment
    -- webhook finds the order. Keyed, so the table alone names no session.
    session_hash  BYTEA PRIMARY KEY,
    -- The buyer's browser holds the claim token in a cookie scoped to
    -- /api/pass; this is its hash. Collecting the Pass needs it, so a Pass is
    -- handed to the browser that paid for it and to nobody else. A browser
    -- that starts a second checkout keeps its token, so one claim can have
    -- several orders, all collected together.
    claim_hash    BYTEA NOT NULL,
    -- Set when the processor reports the payment complete. NULL means the
    -- buyer has not paid yet, or the webhook has not arrived.
    payment_hash  BYTEA UNIQUE,
    -- The Pass this purchase renews, when the buyer renewed one from a browser
    -- holding it. NULL buys a new Pass.
    renew_pass_id BIGINT REFERENCES passes (id) ON DELETE SET NULL,
    -- The claim deadline, rounded up to a UTC midnight.
    expires_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS pass_orders_claim_idx ON pass_orders (claim_hash);
CREATE INDEX IF NOT EXISTS pass_orders_expires_idx ON pass_orders (expires_at);

-- Payments refunded or disputed: the processor can deliver a refund ahead of
-- a retried "paid" event, and this is what stops that late event from turning
-- a refunded purchase into a Pass. Kept for a month, longer than the processor
-- retries an event.
CREATE TABLE IF NOT EXISTS pass_revocations (
    payment_hash BYTEA PRIMARY KEY,
    expires_at   TIMESTAMPTZ NOT NULL
);
