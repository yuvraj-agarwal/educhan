-- The educhan Pass: a term (a year by default) of skipping the captcha,
-- bought for a fee.
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
--     HMAC(EDUCHAN_PASS_SECRET, payment id), which exists so a refund or a
--     chargeback can take back the time that payment bought, and so the owner
--     can reissue a lost code from the payment on the buyer's receipt.
--   * No created_at. Every timestamp (expires_at in each table, and the day a
--     payment's term ends) is rounded up to a UTC day boundary, so the columns
--     date a purchase, a refund or a reissue only to the day. The rows
--     themselves can be dated more closely: a row's xmin, the id of the
--     transaction that wrote it, comes from the same counter as the posts',
--     which carry their time, and the WAL keeps the same order, so whoever
--     holds the live database files or a physical backup can place a row
--     between two posts. A passes row's xmin dates the latest write to it: its
--     purchase, a renewal collected, a reissue code redeemed, or a refund or
--     dispute that takes days off it. Nothing else marks the row. Only a
--     transaction that writes it locks it (inserting a payment's row, which
--     references it, takes its lock), so its xmax, the last transaction to
--     lock it, holds nothing its xmin does not, and a renewal's checkout, paid
--     or not, leaves it as it was until the renewal is collected. That is why
--     pass_orders.renew_pass_id is not a foreign key. Stripe keeps the exact
--     time of every payment anyway.
--
-- pass_orders is the exception, as verification_pending is for 0004: a row
-- there lives from checkout until the Pass is collected, and is deleted the
-- moment it is. Unpaid, it is deleted at the first prune (a checkout, the
-- daily sweep, or a claim from its own browser) after its deadline, a week
-- after checkout (longer while a slow payment confirms), and until then a late
-- payment still counts; paid and never collected, it is kept for its own term
-- past that deadline, so the owner can still reissue it.

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
    -- takes off only the part of the term still to come, and which terms are
    -- still running or to come decides whether another payment can become
    -- the origin.
    ends_at      TIMESTAMPTZ NOT NULL,
    -- Whether a lost code can be reissued from this payment: 'bought' for the
    -- one that created the Pass, and NULL for a renewal. When the purchase is
    -- refunded or disputed and the Pass goes on, a renewal left becomes its
    -- origin, 'moved', only if it pays for all the time the Pass has left: its
    -- term is the only one still running or to come, a paid renewal not
    -- collected yet counting as one to come. Otherwise the Pass has no origin
    -- and cannot be reissued, until a refund or a dispute of another of its
    -- payments, or of a paid renewal not collected yet, leaves one payment
    -- that does. A 'moved' origin gives no reissue code while a renewal of the
    -- Pass is under way, paid or not yet, and stops being one when the Pass
    -- is renewed again, since it then pays for only part of its time.
    origin       TEXT CHECK (origin IN ('bought', 'moved'))
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
    -- The Pass this purchase renews, when the checkout was sent its code. NULL
    -- buys a new Pass, and so does a renewal whose Pass has gone since, which
    -- names no Pass any more: a Pass's id is never given to another. It is not
    -- a foreign key, whose check would lock the Pass's row and leave the
    -- checkout's transaction id on it (above).
    renew_pass_id BIGINT,
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

-- One-time reissue codes that hand a lost Pass back to its buyer: the owner
-- makes one for a payment (POST /api/pass/reissue) and sends it to the address
-- the buyer wrote in from, and whoever types it on the Pass page is given a new
-- code. The owner never sees a Pass code. A row lives until its reissue code is
-- used, replaced by a newer one, cancelled by a refund or a dispute or by a
-- renewal of a Pass whose right had moved, or past its deadline, rounded up to
-- a UTC midnight like an order's. One for a right that moved is refused, and
-- kept, while a renewal of its Pass is under way.
CREATE TABLE IF NOT EXISTS pass_reissues (
    -- SHA-256 of the reissue code's random characters, which only the
    -- owner's answer, the message to the buyer and the claim that redeems it
    -- carry.
    token_hash   BYTEA PRIMARY KEY,
    -- The payment it reissues: the Pass's origin (the purchase, or the
    -- renewal the right moved to after a refund or a dispute), or a paid
    -- order not collected yet. A newer reissue code for the same payment
    -- replaces this one.
    payment_hash BYTEA NOT NULL UNIQUE,
    expires_at   TIMESTAMPTZ NOT NULL
);
