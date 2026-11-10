package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// The educhan Pass, after 4chan's: a term (365 days by default) of posting
// without the captcha, bought for a small fee that helps keep the site running.
//
// It gives exactly one thing. A post that presents a valid Pass skips the
// captcha, up to an hourly budget of posts; past that it solves the captcha
// like any other until the hour ends. It is held to the rules and to every
// cooldown like any other post, and it gets no badge, no name and no mark of
// any kind. 4chan's Pass also lifts IP range and country blocks, which educhan
// does not have, and is tied to one IP address at a time, which would mean
// keeping addresses: here a Pass instead has one hourly budget however many
// people use it, so sharing one shares the budget.
//
// The rules that hold it up, in the spirit of verification's (verify.go):
//
//  1. Nothing about a Pass reaches a post. The post row, the poster ID and the
//     post's request line are the same whether a good Pass was used, one that
//     is no good or none: the line has the method, the path and the status,
//     and no duration, which looking a Pass up would lengthen (requestLogger),
//     and a post whose client goes away during that lookup goes on as one
//     without a Pass (postPass). Using one writes nothing, so there is no "last
//     used" to line up against a post.
//  2. Nothing about the buyer reaches educhan. They pay on Stripe's own page,
//     and what educhan keeps of the payment is an HMAC of its id, which lets a
//     refund or a chargeback take back the time that payment bought, and lets
//     the owner reissue a lost code, and does nothing else.
//  3. The code travels only when it is used: in X-Educhan-Pass on a post, and
//     in the body of the requests that enter it on a device or renew it. A
//     badged post sends the credential instead, so a payment and an
//     institution never meet in one request.
//  4. Buying it takes the same unticked "I am 18 or over" box as the badge,
//     checked and then dropped.

const (
	// passHeader carries the code on a post. Nothing but postPass reads it.
	passHeader = "X-Educhan-Pass"
	// passClaimCookie holds the buyer's claim on the Pass they are paying for,
	// scoped to /api/pass, so it goes to the Pass endpoints and nowhere else.
	passClaimCookie = "educhan_pass_claim"
	passCookiePath  = "/api/pass"
	// passClaimTTL is how long a paid Pass waits to be collected by the
	// browser that bought it, before it is rounded up to the next midnight UTC
	// (passClaimDeadline). A week covers a buyer who closes the tab before
	// Stripe sends them back; past that, the owner can reissue it.
	passClaimTTL = 7 * 24 * time.Hour
	// passPendingPaymentTTL is how long an order whose checkout was completed
	// with a payment still on its way waits for it: longer than a bank debit
	// takes to confirm, the slowest method Stripe Checkout offers.
	passPendingPaymentTTL = 30 * 24 * time.Hour
	// stripeCheckoutTTL is how long a Stripe checkout stays open to be paid:
	// Stripe's default for a session created without expires_at, as
	// createCheckout's are. A slow payment's order is held from when Stripe's
	// report that its checkout completed arrives, which is no later than this
	// after the checkout started unless Stripe delivers the report late
	// (passClaimCookieUntil).
	stripeCheckoutTTL = 24 * time.Hour
	// passRevocationTTL is how long a refunded or disputed payment is
	// remembered after the last full refund or dispute Stripe reports for it, a
	// repeated delivery included, longer than Stripe goes on retrying an event:
	// rounded up to the next midnight UTC, and on until the prune deletes the
	// row, since its lookup (paymentRevoked) takes no bound. A partial refund
	// writes nothing.
	passRevocationTTL = 30 * 24 * time.Hour
	// passExpiredGrace is how long an expired Pass is kept, so it can still be
	// renewed rather than bought again, and so a post with it is told it has
	// expired rather than that it is not a Pass at all. Past it, counted to the
	// instant, the Pass has gone (passCutoff): an expiry is on a midnight UTC
	// (passExpiry), and so is the moment it has gone (passGoneAt).
	passExpiredGrace = 30 * 24 * time.Hour
	// passActivateCooldown spaces out code checks from one address. A code is
	// 100 random bits, so this is courtesy to the database, not a defence.
	passActivateCooldown = 2 * time.Second
	// passPriceTTL is how long the price read from Stripe is kept before it is
	// read again. Stripe never changes a Price's own amount or currency, so a
	// new price is a new Price, set in EDUCHAN_STRIPE_PRICE_ID, and a restart.
	// What a read picks up is the Price archived, or given currency options in
	// other currencies, which Stripe allows on the Price in place, within the
	// hour, or, after a refusal (kept a minute), brought back, rid of those
	// options, or given a tax behavior where it had none, which Stripe allows
	// once.
	passPriceTTL = time.Hour
)

// Machine-readable reasons a Pass was refused, beside the message, so the
// client can forget a code the server will not honour again.
const (
	passInvalid = "invalid"
	passExpired = "expired"
)

/* Codes --------------------------------------------------------------------- */

// passAlphabet is Crockford's base32: digits and capitals without I, L, O and
// U, so a code written down and typed in again survives the usual misreadings.
const passAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// passCodeLen is 20 characters of 5 bits each: 100 random bits, written as
// four groups of five.
const passCodeLen = 20

// formatPassCode groups a canonical code for reading: XXXXX-XXXXX-XXXXX-XXXXX.
func formatPassCode(code string) string {
	var b strings.Builder
	for i := 0; i < len(code); i += 5 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(code[i:min(i+5, len(code))])
	}
	return b.String()
}

// parsePassCode reads a code as a person might type or paste it: any case,
// with or without the dashes, with spaces or other separators between the
// groups (codeSeparators), and with the letters Crockford's alphabet folds
// into digits (O for 0, I and L for 1).
func parsePassCode(raw string) (string, bool) {
	return parseCode(raw, passCodeLen)
}

// parseCode reads n characters of passAlphabet as parsePassCode does. The
// Pass page folds the same letters before it sends a code, as CODE_FOLDS in
// app.js, so it sends the code as it is read here;
// TestPageAndServerFoldTheSameLetters holds the two together.
func parseCode(raw string, n int) (string, bool) {
	out := make([]byte, 0, n)
	for _, r := range strings.ToUpper(raw) {
		switch {
		case isCodeSeparator(r):
			continue
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		if r > 0x7f || strings.IndexByte(passAlphabet, byte(r)) < 0 || len(out) == n {
			return "", false
		}
		out = append(out, byte(r))
	}
	if len(out) != n {
		return "", false
	}
	return string(out), true
}

// codeSeparators are what may stand between a code's groups, which parseCode
// skips: the hyphen, and the dashes and minus sign a word processor or a mail
// client may turn it into; white space and line breaks, every Unicode space
// among them, the no-break and narrow ones a paste out of mail or a web page
// brings included; and the soft hyphen, zero-width space, word joiner and
// byte-order mark, which a paste can carry unseen. The Pass page skips exactly
// these, as CODE_SEPARATORS in app.js, so a code it reads as one kind is read
// as that kind here; TestPageAndServerSkipTheSameSeparators holds the two
// together.
const codeSeparators = "-\u2010\u2011\u2012\u2013\u2014\u2015\u2212" +
	" \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006" +
	"\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000" +
	"\u00ad\u200b\u2060\ufeff"

func isCodeSeparator(r rune) bool { return strings.ContainsRune(codeSeparators, r) }

// A reissue code is what the owner sends a buyer who lost their code
// (handlePassReissue), to type or paste on the Pass page. It is the word
// REISSUE, which no Pass code can hold (U is not in the alphabet), so the page
// and a person can tell the two apart, and then reissueCodeLen characters of
// the alphabet: 140 random bits, more than the 128 of a claim token, in seven
// groups of four to read out. It is never in an address, so no browser's
// history, address bar or Referer can keep it.
const (
	reissueCodePrefix = "REISSUE"
	reissueCodeLen    = 28
)

// newReissueCode draws a reissue code's characters: 256 is a multiple of 32,
// so masking a random byte to five bits picks every one with equal
// probability.
func newReissueCode() (string, error) {
	raw := make([]byte, reissueCodeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i, b := range raw {
		raw[i] = passAlphabet[b&31]
	}
	return string(raw), nil
}

// formatReissueCode writes a reissue code to send: REISSUE-XXXX-…-XXXX.
func formatReissueCode(code string) string {
	var b strings.Builder
	b.WriteString(reissueCodePrefix)
	for i := 0; i < len(code); i += 4 {
		b.WriteByte('-')
		b.WriteString(code[i:min(i+4, len(code))])
	}
	return b.String()
}

// parseReissueCode reads a reissue code as a person might type or paste it,
// as parsePassCode reads a Pass code, with or without the word in front. What
// it returns is the characters after the word, which is what the server
// hashes and derives the new Pass code from.
func parseReissueCode(raw string) (string, bool) {
	bare := strings.Map(func(r rune) rune {
		if isCodeSeparator(r) {
			return -1
		}
		return r
	}, strings.ToUpper(raw))
	return parseCode(strings.TrimPrefix(bare, reissueCodePrefix), reissueCodeLen)
}

// passTokenHash is what the database holds of a code: keyed, so a dump of the
// table cannot be checked against a code without the secret either.
func (a *App) passTokenHash(code string) []byte {
	return hmacSum([]byte(a.cfg.Pass.Secret), "pass-token:"+code)
}

// passClaimCode is the code a claim hands over, and passReissuedCode the code
// a reissue code does. Each is derived with the secret from the token the
// request has to hold, the claim cookie or the reissue code, and a nonce the
// page draws for the collection, rather than drawn here, so collecting is
// safe to repeat. The page keeps a claim's nonce until an answer says nothing
// more is waiting, and a reissue's until an answer says it issued or renewed
// a Pass (never on a refusal) or Remove the Pass or Forget this browser drops
// it. So a claim or reissue whose answer was lost after the server had
// committed it (a tab closed, a page reloaded, a connection dropped) is asked
// again with the same cookie or reissue code and the same nonce, and handed
// the same code, where a fresh draw would leave the buyer a Pass they can
// never see. A renewal that renews its Pass hands over no code, and is not
// answered again: the order or reissue code is spent, so the retry is
// refused, and the page checks the code it holds to learn the new date.
//
// Neither half is worth anything alone. A nonce copied off a browser, planted
// in one or left behind in one gives nothing with any other cookie or reissue
// code, and a checkout gives the browser a new cookie, so a nonce planted
// before somebody buys there reaches nothing they buy. Each order is collected
// through its own claim, once, and a reissue code works once, so no later
// request can make a Pass under a code handed over before: one refunded away,
// or given a new code by a reissue, is under no code a retry can ask for, and
// does not come back.
//
// The claim token and the nonce are 128 random bits each and a reissue code
// 140, the secret is only the server's, and the database keeps none of them
// and not the code, only the code's HMAC (passTokenHash), so the code is as
// hard to guess as a random one: 256 is a multiple of 32, so masking a byte of
// the HMAC to five bits picks every character with equal probability. A claim
// token and a nonce are base64url and a reissue code is written in
// passAlphabet, none of which has a colon, so the two halves cannot be shifted
// into each other.
func (a *App) passClaimCode(claim, nonce string) string {
	return a.passCollectedCode("pass-claim-code:" + claim + ":" + nonce)
}

func (a *App) passReissuedCode(reissue, nonce string) string {
	return a.passCollectedCode("pass-reissue-code:" + reissue + ":" + nonce)
}

func (a *App) passCollectedCode(input string) string {
	sum := hmacSum([]byte(a.cfg.Pass.Secret), input)
	out := make([]byte, passCodeLen)
	for i := range out {
		out[i] = passAlphabet[sum[i]&31]
	}
	return string(out)
}

// isPassToken reports whether s is a token this server or its page could have
// made: 16 random bytes in unpadded base64url. Claim tokens and collection
// nonces both take this form.
func isPassToken(s string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(raw) == 16
}

// passBudgetKey is what a Pass's hourly budget is counted against. It is a
// different HMAC from the stored one, so the budget in memory holds nothing
// that matches a database row.
func (a *App) passBudgetKey(code string) string {
	return "pass|" + hex.EncodeToString(hmacSum([]byte(a.cfg.Pass.Secret), "pass-budget:"+code))
}

func (a *App) passSessionHash(id string) []byte {
	return hmacSum([]byte(a.cfg.Pass.Secret), "pass-session:"+id)
}

func (a *App) passPaymentHash(id string) []byte {
	return hmacSum([]byte(a.cfg.Pass.Secret), "pass-payment:"+id)
}

// passExpiry is when a Pass runs out once days are added to it from base: now
// for a new Pass, and for a renewal the later of now and its current expiry.
// It is rounded up to a UTC midnight like a credential's expiry, so the stored
// expiry dates a purchase to the day and not the minute. An expiry already on
// a midnight stays there, so renewing a Pass that has not run out adds exactly
// the days.
func passExpiry(base time.Time, days int) time.Time {
	return dayCeil(base.UTC().AddDate(0, 0, days))
}

// dayCeil rounds t up to the next UTC midnight, and leaves a UTC midnight as
// it is.
func dayCeil(t time.Time) time.Time {
	utc := t.UTC()
	midnight := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	if midnight.Equal(utc) {
		return midnight
	}
	return midnight.AddDate(0, 0, 1)
}

// termDays is the term a payment bought, as recorded beside it: how far it
// moved the Pass's expiry past base, in whole days rounded up. The rounding
// to a midnight is counted in, so taking the whole term off again for a
// refund leaves the Pass where it would have been without that payment, or
// earlier, never later. A refund takes off at most this (revokeTerm).
func termDays(base, expiry time.Time) int {
	const day = 24 * time.Hour
	d := expiry.Sub(base)
	n := d / day
	if d%day > 0 {
		n++
	}
	return int(n)
}

// passClaimDeadline is the deadline of an order started at now: a week on,
// rounded up to a UTC midnight, so the stored deadline dates a checkout to the
// day and not the microsecond. From then on an unpaid order is no longer waited
// for, and goes when something deletes it, the prune (prunePasses) or a claim
// (claimOne), say, though a payment Stripe reports for it before then still
// counts until its own term has passed since (passOrderPayable); a paid one is
// kept for that term past it (passOrderEnd).
func passClaimDeadline(now time.Time) time.Time {
	return dayCeil(now.Add(passClaimTTL))
}

// passClaimCookieUntil is how long a checkout started at now keeps the
// browser's claim cookie: past its order's deadline, and 30 days past Stripe's
// 24-hour checkout, as long as the order is held for a slow payment
// (holdPassOrder) when Stripe reports the checkout completed as it happens.
// Only a checkout sets the cookie, so a buyer who leaves before the hold and
// comes back when the bank debit clears must still have it. The hold counts
// from when that report arrives, so one Stripe delivers late, on a retry, can
// hold the order past the cookie, by at most as long as it was late rounded
// up to whole days, and its buyer then collects it with a reissue code.
func passClaimCookieUntil(now time.Time) time.Time {
	return dayCeil(now.Add(stripeCheckoutTTL + passPendingPaymentTTL))
}

// daysOff is how many whole days to take off a term ending at end so that
// none of it is left after now: the days from now to end, rounded up, so a
// refunded term with no other after it stops at once rather than at
// midnight, and none for a term that has already ended.
func daysOff(end, now time.Time) int {
	if !end.After(now) {
		return 0
	}
	return termDays(now, end)
}

// wholeDaysLeft is how many whole days of a term ending at end are left after
// now, rounded down: taken off a refunded term with another after it, they
// leave the refunded term to stop at the next midnight and the next term to
// start there, so it comes forward whole (revokeTerm).
func wholeDaysLeft(end, now time.Time) int {
	if !end.After(now) {
		return 0
	}
	return int(end.Sub(now) / (24 * time.Hour))
}

/* Handlers ------------------------------------------------------------------ */

// passState is the part of App the Pass needs beyond Config: the Stripe client,
// the limiters, the hourly budget, and the cached price.
type passState struct {
	stripe          *stripeClient
	checkoutLimiter *cooldownLimiter
	activateLimiter *cooldownLimiter
	budget          *passBudget
	priceMu         sync.Mutex
	price           passPrice
	priceErr        error
	priceFetchedAt  time.Time
}

func newPassState(cfg PassConfig) *passState {
	return &passState{
		stripe:          newStripeClient(cfg),
		checkoutLimiter: newCooldownLimiter(cfg.CheckoutCooldown),
		activateLimiter: newCooldownLimiter(passActivateCooldown),
		budget:          newPassBudget(cfg.HourlyPosts),
	}
}

func (a *App) passEnabled() bool { return a.cfg.Pass.Enabled() }

func (a *App) requirePass(w http.ResponseWriter) bool {
	if !a.passEnabled() {
		writeError(w, http.StatusForbidden, "the Pass is not on sale")
		return false
	}
	return true
}

// passRequest refuses a request to a Pass endpoint that another site could
// have made the browser send. Its body must be declared JSON, which no form
// can send and no other origin's script can send without asking first (a
// CORS preflight this server never answers), and a browser that says the
// request came from another site is refused outright. The claim cookie is
// SameSite=Lax already; this also keeps a page on a sibling subdomain, which
// Lax counts as the same site, from spending a browser's claim. decodeJSON,
// which every other endpoint uses, lets a missing Content-Type through; the
// Pass endpoints do not.
func passRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeError(w, http.StatusForbidden, "requests from other sites are refused")
		return false
	}
	mediaType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if !strings.EqualFold(strings.TrimSpace(mediaType), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	return true
}

// passNotOnSale is checkout's refusal while sales are paused
// (EDUCHAN_PASS_SALES=paused). The page knows it by these words.
const passNotOnSale = "the Pass is not on sale right now"

// handlePassStatus tells the Pass page whether the Pass is on sale or paused,
// for how long one lasts, how many posts an hour it skips the captcha for, its
// price and, with Stripe Tax on, whether tax is in the price or added to it at
// checkout. Like /api/verify it looks up nothing about the caller's Pass: a
// browser knows its own Pass and when it runs out. The one thing it reads off
// the request is the claim cookie, which the page cannot see (it is HttpOnly),
// so the page knows there is a purchase to collect. The claim is looked up for
// that and nothing else: a cookie is a purchase to collect while an order
// under it can still bring a Pass, an unpaid one past its deadline included,
// which the page's claim then deletes (passClaimWaiting), so one with none is
// no purchase to collect, now or later. Every claim cookie the request carries
// is looked under (passClaimTokens).
func (a *App) handlePassStatus(w http.ResponseWriter, r *http.Request) {
	if !a.passEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	out := map[string]any{"enabled": true, "days": a.cfg.Pass.Days, "hourly_posts": a.cfg.Pass.HourlyPosts}
	if claims := passClaimTokens(r); len(claims) > 0 {
		waiting, err := a.store.passClaimWaiting(r.Context(), claimHashes(claims), time.Now())
		if err != nil {
			a.fail(w, r, err, "could not look up the purchase in progress")
			return
		}
		if waiting {
			out["claim"] = true
		}
	}
	if a.cfg.Pass.SalesPaused {
		// Nothing is sold, so Stripe is not asked for a price.
		out["paused"] = true
	} else if price, err := a.passPrice(r.Context()); err == nil {
		// The price is shown when Stripe could be asked for it and answered
		// with a Price the Pass can be sold at (errPassPriceRefused). Without
		// it the page still offers the Pass, but checkout sells nothing, with
		// Stripe Tax on or off: it refuses every Price it can't sell and a
		// request for the Price Stripe refuses (passPriceUnsold), and any
		// checkout while Stripe is out of reach or failing. With Stripe Tax on,
		// the Price says whether the tax is in it or added to it, and the page
		// says which (passTaxWords); one that says neither is a Price the Pass
		// can't be sold at.
		out["price"] = price
		if a.cfg.Pass.StripeAutomaticTax {
			out["tax"] = passTaxWords[price.TaxBehavior]
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// passPrice returns the cached price, reading it from Stripe when the cache is
// stale. A failure is cached for a minute, so an outage or a misconfigured
// price does not turn every page view into a call to Stripe, and a good read
// for an hour (passPriceTTL); TestPassPriceIsKeptForItsTime holds both.
func (a *App) passPrice(ctx context.Context) (passPrice, error) {
	s := a.pass
	s.priceMu.Lock()
	defer s.priceMu.Unlock()
	ttl := passPriceTTL
	if s.priceErr != nil {
		ttl = time.Minute
	}
	if !s.priceFetchedAt.IsZero() && time.Since(s.priceFetchedAt) < ttl {
		return s.price, s.priceErr
	}
	// The answer is cached for every later request, so it must not depend on
	// this one: a client that goes away mid-fetch would otherwise leave
	// "context canceled" in the cache for a minute.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.price, s.priceErr = s.stripe.price(ctx)
	s.priceFetchedAt = time.Now()
	switch {
	case errors.Is(s.priceErr, errPassPriceRefused):
		a.log.Error("the Pass price in Stripe needs fixing", "error", s.priceErr)
	case s.priceErr != nil:
		a.log.Error("could not read the Pass price from Stripe", "error", s.priceErr)
	}
	return s.price, s.priceErr
}

// passPriceUnsold is checkout's refusal, with Stripe Tax on or off, when
// Stripe answered with a Price the Pass can't be sold at, or refused the
// request for the Price (errPassPriceRefused). It is the owner's to fix, and
// trying again will not, so it does not say to: the log says what to fix.
const passPriceUnsold = "the Pass can't be bought right now"

// passStripeUnreachable answers a checkout Stripe could not be asked about:
// Stripe was out of reach or failing when asked for the Price, or the session
// could not be opened.
const passStripeUnreachable = "could not reach the payment page, try again in a moment"

// passAdultRequired refuses a checkout that does not say its buyer is 18 or
// over, in the badge's words, because it is the same rule.
const passAdultRequired = "the Pass is sold only to people aged 18 and over; you can still read and post without one"

type passCheckoutBody struct {
	// Adult is the buyer's own word that they are 18 or over. It is checked
	// and dropped: no column, log field or Stripe parameter holds it.
	Adult bool `json:"adult"`
	// Renew is the code of a Pass to extend rather than a new one to buy. The
	// browser sends it only when its holder chose to renew.
	Renew string `json:"renew"`
}

// handlePassCheckout starts a purchase: it opens a Stripe checkout, records an
// order the webhook can mark paid, and gives the browser the claim it will
// collect the Pass with. The answer is the Stripe page to go to, and the
// order's name (passOrderName). While sales are paused it refuses, for a new
// Pass and a renewal alike.
func (a *App) handlePassCheckout(w http.ResponseWriter, r *http.Request) {
	if !a.requirePass(w) || !passRequest(w, r) {
		return
	}
	if a.cfg.Pass.SalesPaused {
		writeError(w, http.StatusForbidden, passNotOnSale)
		return
	}
	var req passCheckoutBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.Adult {
		writeError(w, http.StatusBadRequest, passAdultRequired)
		return
	}
	// The Pass is sold only at a Price read from Stripe and found to be one it
	// can be sold at, with Stripe Tax on or off: one-time, not archived, with a
	// fixed amount, so not one whose buyer chooses what to pay, in its own
	// currency alone, and, while Stripe Tax is on, saying whether tax is in it,
	// since the page could not otherwise say what the buyer pays. The session
	// sells one of it, sending nothing that lets the buyer change what they
	// pay, at its own amount in its own currency unless the Stripe account has
	// Adaptive Pricing on (createCheckout), so the webhook need not check what
	// was paid. A Price the Pass can't be sold at, and a request for the Price
	// Stripe refuses (a key or Price ID it does not take, say), are refused as
	// the fault in the setup that they are (503), and every checkout while
	// Stripe is out of reach or failing as just that (502). passPrice keeps
	// either a minute, and logged why as it read the Price, so a refusal logs
	// nothing more. Nothing is opened in Stripe, and no cooldown starts for a
	// fault that is not the buyer's.
	switch _, err := a.passPrice(r.Context()); {
	case errors.Is(err, errPassPriceRefused):
		writeError(w, http.StatusServiceUnavailable, passPriceUnsold)
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, passStripeUnreachable)
		return
	}

	now := time.Now()
	// The Pass to renew is read without the Pass lock, as gone once it is 30
	// days past its expiry (passCutoff), as the order will be held to, so a
	// code that is no good is refused before it costs its holder a cooldown.
	// The order reads it again by the same code, under the lock, by the clock
	// once Stripe has answered (createPassOrder).
	var renewToken []byte
	if strings.TrimSpace(req.Renew) != "" {
		code, ok := parsePassCode(req.Renew)
		if !ok {
			refusePass(w, passInvalid, "that is not a Pass code")
			return
		}
		renewToken = a.passTokenHash(code)
		_, _, err := a.store.passByToken(r.Context(), renewToken, now)
		if errors.Is(err, errNotFound) {
			refusePass(w, passInvalid, "this Pass is no longer valid, buy a new one instead")
			return
		}
		if err != nil {
			a.fail(w, r, err, "could not start checkout")
			return
		}
	}

	// The cooldown comes before anything that takes the Pass lock, so a burst
	// of refused checkouts queues no webhook or claim behind it.
	clientKey := clientIP(r, a.cfg.TrustedProxyHops)
	if !a.allowAction(w, "starting checkouts", []cooldown{
		{limiter: a.pass.checkoutLimiter, key: clientKey},
	}) {
		return
	}

	// Orders no longer kept (passOrderKept), refunds past their 30 days and
	// Passes past their 30 days of grace go here as well as in the sweep, so a
	// busy server keeps nothing past its time between two sweeps. A Pass past
	// its grace counts as gone before then (passCutoff), so this only frees
	// rows.
	//
	// A client gone meanwhile, a checkout the page called off by a forget or
	// by leaving, is no fault, so it is not logged, here or below, as a.fail
	// logs nothing for it.
	if err := a.store.prunePasses(r.Context(), now); err != nil && r.Context().Err() == nil {
		a.log.Warn("could not prune Pass tables", "error", err)
	}

	// Every checkout gives the browser a new claim, with this order under it
	// and no other. An order is collected only through the claim its own
	// checkout set, so a claim token copied off this browser, or planted in
	// it, before this checkout covers nothing it buys, and nothing under the
	// claim the browser held before, somebody else's checkout included, is
	// added to it. A payment on that earlier checkout that has not been
	// collected is no longer collected here: it is still its buyer's, and the
	// owner reissues it from the payment on the receipt, as for any order
	// nobody collected.
	claim, err := randomToken()
	if err != nil {
		a.fail(w, r, err, "could not start checkout")
		return
	}
	base := a.cfg.Pass.PublicURL
	session, err := a.pass.stripe.createCheckout(r.Context(), base+"/pass?paid", base+"/pass?cancelled")
	if err != nil && r.Context().Err() != nil {
		// Nobody is left to answer, and Stripe's session, if it opened one, is
		// left to expire unpaid.
		return
	}
	if err != nil {
		a.log.Error("could not create a Stripe checkout", "error", err)
		writeError(w, http.StatusBadGateway, passStripeUnreachable)
		return
	}

	// The order's deadline counts from the checkout's start, as the cookie
	// does, but the Pass it renews is held to its 30 days past its expiry by
	// the clock read now, after the Stripe call, not by the one read before the
	// lookup above, so one that ran past those 30 days while Stripe was opening
	// the session is refused like one refunded meanwhile.
	err = a.store.createPassOrder(r.Context(), passOrder{
		ClaimHash:      hashToken(claim),
		SessionHash:    a.passSessionHash(session.ID),
		RenewTokenHash: renewToken,
		Days:           a.cfg.Pass.Days,
		ExpiresAt:      passClaimDeadline(now),
	}, time.Now())
	if errors.Is(err, errPassGone) {
		// Refunded, past its 30 days of grace, or given another code by a
		// reissue between the lookup above and the order. The Stripe session is
		// left to expire unpaid.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "that Pass can't be renewed any more; buy a new one",
			"pass":  passInvalid,
		})
		return
	}
	if err != nil {
		a.fail(w, r, err, "could not start checkout")
		return
	}

	// The cookie outlives the order's deadline, and the hold it could yet be
	// given for a slow payment when Stripe reports the checkout completed as it
	// happens. Only a webhook gives the hold, and it cannot reach the cookie
	// (passClaimCookieUntil).
	a.setCookie(w, r, passClaimCookie, claim, passCookiePath, time.Until(passClaimCookieUntil(now)))
	writeJSON(w, http.StatusOK, map[string]any{"url": session.URL, "order": a.passOrderName(claim)})
}

// passOrderTagSize is how many bytes of an HMAC an order's name carries.
const passOrderTagSize = 16

// passOrderName is how a checkout's answer names its order to the page: by
// what the database holds of the claim the order is under, signed with the
// secret (passOrderTag). It is for a page that does not send the browser on
// to the Stripe page the answer names, which nobody can pay once it is thrown
// away, to drop that claim and no other ({"forget": true, "order"},
// forgetPassOrder): the page does so for a checkout answered after Remove the
// Pass, Forget this browser or Stop collecting it here was pressed in another
// tab, or once the Pass the browser holds has changed, or while the reader is
// on another page. It collects nothing, since collecting takes the claim
// itself, which the page never sees (the cookie is HttpOnly). It is signed
// because the hash alone is what anyone who can read pass_orders holds, from
// a backup say, and a name made from it would drop every waiting order there,
// paid ones included, for their buyers to ask the owner for reissue codes.
func (a *App) passOrderName(claim string) string {
	hash := hashToken(claim)
	return base64.RawURLEncoding.EncodeToString(append(hash, a.passOrderTag(hash)...))
}

// passOrderTag is the signature an order's name carries beside its claim's
// hash, which only the server, holding the secret, can make.
func (a *App) passOrderTag(claimHash []byte) []byte {
	return hmacSum([]byte(a.cfg.Pass.Secret), "pass-order:"+hex.EncodeToString(claimHash))[:passOrderTagSize]
}

// passClaimCookieLimit is how many claim cookies a request is read for. A
// browser sends this server's own and any that a page on a sibling subdomain
// set for the whole site at a path reaching the Pass endpoints, a handful at
// most; more is no browser's, and reading no more keeps one request from
// queueing a lookup for each of thousands behind the Pass lock.
const passClaimCookieLimit = 16

// passClaimTokens reads every claim cookie the request carries that holds a
// token this server could have made, each once, in the order the browser sent
// them. There can be more than one: a page on a sibling subdomain can set a
// cookie of the same name for the whole site, at a longer path that the
// browser sends ahead of this server's own, and reading only the first would
// let it hide the buyer's claim from collecting and from forgetting. So none
// stands in for another: the status and the claim look under each, and
// forgetting drops each.
func passClaimTokens(r *http.Request) []string {
	var tokens []string
	for _, c := range r.Cookies() {
		if len(tokens) == passClaimCookieLimit {
			break
		}
		if c.Name == passClaimCookie && isPassToken(c.Value) && !slices.Contains(tokens, c.Value) {
			tokens = append(tokens, c.Value)
		}
	}
	return tokens
}

// claimHashes is what the database holds of each claim token.
func claimHashes(tokens []string) [][]byte {
	hashes := make([][]byte, len(tokens))
	for i, t := range tokens {
		hashes[i] = hashToken(t)
	}
	return hashes
}

// handlePassClaim hands what a browser paid for to that browser: the order
// under its claim, once it is paid. A code is shown here once: only its HMAC
// is kept, so a buyer who loses it asks the owner to reissue it. The code is
// derived from the claim and the nonce the page sends (passClaimCode), so
// asking again with both, after an answer that never arrived, is answered
// with the same code; the page drops the nonce once an answer says nothing
// more is waiting.
//
// Stripe sends the buyer back before or after its webhook arrives, whichever
// is faster, so "not paid yet" is an ordinary answer (202) for the page to
// retry, not an error. The answer says whether anything is still waiting
// under the claim ("claim"). A claim never sets or clears the cookie: only a
// checkout sets it, for long enough (passClaimCookieUntil), and only
// forgetting clears it, since an answer for a cookie the browser has since
// replaced, in another tab, would otherwise clear the new one.
//
// A browser holding more than one claim cookie (passClaimTokens) has each
// collected in turn, one order an answer (claimPass), and is told to go on
// while anything waits under any of them.
func (a *App) handlePassClaim(w http.ResponseWriter, r *http.Request) {
	if !a.requirePass(w) || !passRequest(w, r) {
		return
	}
	var req passClaimBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Forget {
		if req.Order != "" {
			a.forgetPassOrder(w, r, req.Order)
			return
		}
		a.forgetPassClaim(w, r)
		return
	}
	if req.Order != "" {
		writeError(w, http.StatusBadRequest, `order: goes only with "forget": true`)
		return
	}
	if !isPassToken(req.Nonce) {
		writeError(w, http.StatusBadRequest, "nonce: want the 16 random bytes the page draws for each collection, in base64url")
		return
	}
	if req.Reissue != "" {
		a.redeemPassReissue(w, r, req.Reissue, req.Nonce)
		return
	}
	claims := passClaimTokens(r)
	if len(claims) == 0 {
		writeError(w, http.StatusBadRequest, "no purchase in progress in this browser")
		return
	}
	codes := make([]string, len(claims))
	claimants := make([]passClaimant, len(claims))
	for i, claim := range claims {
		codes[i] = a.passClaimCode(claim, req.Nonce)
		claimants[i] = passClaimant{ClaimHash: hashToken(claim), TokenHash: a.passTokenHash(codes[i])}
	}
	res, err := a.store.claimPass(r.Context(), claimants, time.Now())
	if err != nil {
		a.fail(w, r, err, "could not collect the Pass")
		return
	}
	switch {
	case res.Issued:
		if res.Made {
			a.log.Info("pass issued")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "issued",
			"pass":       formatPassCode(codes[res.Which]),
			"expires_at": res.IssuedExpiresAt,
			"claim":      res.More,
		})
	case res.Renewed:
		a.log.Info("pass renewed")
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "renewed",
			"expires_at": res.RenewedExpiresAt,
			"claim":      res.More,
		})
	case res.Waiting:
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending", "claim": true})
	default:
		writeError(w, http.StatusBadRequest, "no purchase in progress in this browser")
	}
}

type passClaimBody struct {
	// Nonce is what the page draws, 16 random bytes in base64url, for each
	// collection, and sends with every claim until an answer says nothing more
	// is waiting, and with a reissue code until an answer says it issued or
	// renewed a Pass (passClaimCode, passReissuedCode).
	Nonce string `json:"nonce"`
	// Reissue is a reissue code (handlePassReissue), which the reader types
	// or pastes on the Pass page. Without it, the claim is the browser's
	// cookie.
	Reissue string `json:"reissue"`
	// Forget drops the browser's claim instead (forgetPassClaim).
	Forget bool `json:"forget"`
	// Order, beside Forget, names the one claim to drop in place of the
	// browser's: the one whose order a checkout's answer named
	// (passOrderName, forgetPassOrder).
	Order string `json:"order"`
}

// forgetPassClaim drops the browser's claim, for a shared computer: the order
// under it moves under a hash of nothing anybody holds, and the cookie is
// cleared, so whoever uses the browser next collects nothing of it, even with
// a copy of the cookie taken before. Paid, or paid later, the order is the
// buyer's still: the owner reissues it from the payment on the buyer's
// receipt, as for any paid order nobody collected. Every claim cookie the
// request carries is dropped (passClaimTokens), and the one this server set
// is cleared; one set from a sibling subdomain is out of its reach, but has
// nothing under it any more.
func (a *App) forgetPassClaim(w http.ResponseWriter, r *http.Request) {
	if claims := passClaimTokens(r); len(claims) > 0 {
		if err := a.store.dropPassClaims(r.Context(), claimHashes(claims)); err != nil {
			a.fail(w, r, err, "could not forget the purchase in progress")
			return
		}
	}
	clearCookie(w, passClaimCookie, passCookiePath)
	writeJSON(w, http.StatusOK, map[string]any{"status": "forgotten", "claim": false})
}

// forgetPassOrder drops one checkout's claim, the one whose order its answer
// named (passOrderName), as forgetPassClaim drops each of the browser's: for a
// checkout the page does not follow to Stripe, which nobody can then pay, and
// that no forget sent from its tab drops. That is one answered after Remove the
// Pass, Forget this browser or Stop collecting it here was pressed in another
// tab, whose claim would otherwise outlive that forget, or one answered once
// the Pass the browser holds has changed, or while the reader is on another
// page, whose claim the page would otherwise go on collecting for nothing until
// Stripe expires it. A name without the server's signature, one made from a
// copy of pass_orders say, is refused. It reads no cookie and clears none. A
// checkout started since is somebody's purchase, and its cookie can arrive
// while this request is on its way, so clearing the cookie here could clear
// that one; the cookie the dropped checkout set may stay in the browser, with
// nothing under it, until another checkout replaces it, Remove the Pass, Forget
// this browser or Stop collecting it here clears it (forgetPassClaim), or it
// runs out. The answer says nothing of what else waits, since no cookie was
// read.
func (a *App) forgetPassOrder(w http.ResponseWriter, r *http.Request, name string) {
	raw, err := base64.RawURLEncoding.DecodeString(name)
	if err != nil || len(raw) != sha256.Size+passOrderTagSize ||
		!hmac.Equal(raw[sha256.Size:], a.passOrderTag(raw[:sha256.Size])) {
		writeError(w, http.StatusBadRequest, "order: want the name a checkout's answer gave its order")
		return
	}
	claimHash := raw[:sha256.Size]
	if err := a.store.dropPassClaims(r.Context(), [][]byte{claimHash}); err != nil {
		a.fail(w, r, err, "could not forget the purchase in progress")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "forgotten"})
}

// redeemPassReissue answers a claim that carries a reissue code, in the
// claim's own words: issued, with the new code shown once, or renewed, for
// an uncollected renewal of a Pass that is still there; one whose Pass has
// gone makes a new Pass and is issued. A reissue code works once; asked
// again with the nonce that spent it, it is answered with the same code if
// it gave one (passReissuedCode), and one that renewed a Pass, which hands
// over no code, is refused like any spent reissue code. One for a right that
// moved, used while its Pass is being renewed, is refused for that and not
// spent (claimResult.Renewing).
func (a *App) redeemPassReissue(w http.ResponseWriter, r *http.Request, typed, nonce string) {
	reissue, ok := parseReissueCode(typed)
	if !ok {
		refusePass(w, passInvalid, "that is not a reissue code; it is REISSUE and 28 letters and digits")
		return
	}
	code := a.passReissuedCode(reissue, nonce)
	c := passCompletion{TokenHash: a.passTokenHash(code), Now: time.Now()}
	res, err := a.store.redeemPassReissue(r.Context(), hashToken(reissue), c)
	if err != nil {
		a.fail(w, r, err, "could not collect the Pass")
		return
	}
	if res.Renewing {
		writeError(w, http.StatusConflict, passReissueRenewing)
		return
	}
	// Whether this browser also has a purchase to collect, so the page goes
	// on to collect it, as GET /api/pass would say (passClaimWaiting, an
	// unpaid order past its deadline included, which that collecting
	// deletes), by the clock as it is read, not the one the reissue read
	// before it waited for the lock. The reissue is committed by now, so
	// failing to read this does not fail the answer, which would tell the
	// reader the code did not work after it had: the page also asks GET
	// /api/pass on every visit.
	claim := false
	if claims := passClaimTokens(r); len(claims) > 0 {
		waiting, err := a.store.passClaimWaiting(r.Context(), claimHashes(claims), time.Now())
		if err != nil && r.Context().Err() == nil {
			a.log.Error("could not look up the purchase in progress", "error", err)
		}
		claim = err == nil && waiting
	}
	switch {
	case res.Issued:
		if res.Made {
			a.log.Info("pass reissued")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "issued",
			"pass":       formatPassCode(code),
			"expires_at": res.IssuedExpiresAt,
			"claim":      claim,
		})
	case res.Renewed:
		a.log.Info("pass renewed")
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "renewed",
			"expires_at": res.RenewedExpiresAt,
			"claim":      claim,
		})
	default:
		refusePass(w, passInvalid, passReissueSpent)
	}
}

type passActivateBody struct {
	Pass string `json:"pass"`
}

// handlePassActivate checks a code typed in on a device, so the page can say
// whether it works and until when before the first post depends on it. A code
// that has run out is refused as expired, with its date, until 30 days past its
// expiry, and as no longer valid from then on (passCutoff). It writes nothing:
// the browser keeps the code, as it keeps the credential.
func (a *App) handlePassActivate(w http.ResponseWriter, r *http.Request) {
	if !a.requirePass(w) || !passRequest(w, r) {
		return
	}
	var req passActivateBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if !a.allowAction(w, "checking Pass codes", []cooldown{
		{limiter: a.pass.activateLimiter, key: clientIP(r, a.cfg.TrustedProxyHops)},
	}) {
		return
	}
	code, ok := parsePassCode(req.Pass)
	if !ok {
		refusePass(w, passInvalid, "that is not a Pass code; it is 20 letters and digits")
		return
	}
	now := time.Now()
	_, expires, err := a.store.passByToken(r.Context(), a.passTokenHash(code), now)
	switch {
	case errors.Is(err, errNotFound):
		refusePass(w, passInvalid, "this Pass is not valid; it may have run out more than 30 days ago, or been refunded or replaced")
		return
	case err != nil:
		a.fail(w, r, err, "could not read the Pass")
		return
	case !now.Before(expires):
		// The code goes back as the server writes it, with its date, so the
		// page can keep a Pass typed in after it ran out, to renew it.
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":      "this Pass has run out; you can renew it on the Pass page",
			"pass":       passExpired,
			"code":       formatPassCode(code),
			"expires_at": expires,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pass": formatPassCode(code), "expires_at": expires})
}

func refusePass(w http.ResponseWriter, state, msg string) {
	writeJSON(w, http.StatusForbidden, map[string]any{"error": msg, "pass": state})
}

// passGrant is what a post's Pass comes to: whether it presented a valid,
// unexpired one, and the key that Pass's hourly budget is counted against.
// The captcha lets a post with a valid Pass through while the budget has
// posts left (budget.left), and a post counts against it only once it is
// saved (spendPassBudget).
type passGrant struct {
	Valid     bool
	BudgetKey string
	// Refused is passInvalid or passExpired for a Pass that was presented and
	// is no good. The post goes on as one made without a Pass, and its answer
	// carries this (passPostResponse), so the page can forget the code, or
	// offer to renew it.
	Refused string
}

// postPass resolves the Pass a post presents. A post made without one gets
// the zero passGrant.
//
// A badged post never reads the header: it is the credential's request, and a
// payment and an institution are not to meet in one. With the Pass switched
// off (no secret, which the README calls retiring it) a presented Pass is
// ignored, so the post goes on as one without a Pass; paused sales honour it
// as usual.
//
// A Pass that is presented and no good, because it was refunded, replaced or
// has run out, is treated as no Pass at all rather than refused. A refusal of
// its own would put a status in the request log, on the path that names the
// thread, that only a Pass can cause; that line, then the same thread's 201
// once the poster posts again, would tie the Pass's holder, whom the refund
// names to the operator in Stripe, to the post. Treated as absent, the post is
// held to the captcha like any other, and whatever it is refused for is what
// any post without a Pass can be refused for. Nor does the line show the lookup
// below, which a code that parses costs and no Pass does not: a posting route's
// line carries no duration (untimed), and this takes it off too (markUntimed),
// so a post that gets this far leaves none however its route was registered.
// Nor does a client that goes away during the lookup: the post goes on as one
// without a Pass, so it is stopped where any post whose client has gone is, by
// its next read or by flood control, say, and leaves the line such a post
// leaves. The page hears that the Pass is no good from the answer's body
// (passGrant.Refused), which no log records: expired until 30 days past its
// expiry, so the page can offer to renew it, and invalid from then on, when it
// has gone (passCutoff).
//
// A good Pass changes nothing here: no cooldown is added or lifted, and a post
// that is refused after this, by flood control or anything else, costs its
// budget nothing. Only a failure to read the Pass while its client is still
// there is an error, and it is answered without logging the path, which names
// the thread.
func (a *App) postPass(w http.ResponseWriter, r *http.Request, badged bool) (passGrant, bool) {
	markUntimed(w)
	if badged || !a.passEnabled() {
		return passGrant{}, true
	}
	presented := strings.TrimSpace(r.Header.Get(passHeader))
	if presented == "" {
		return passGrant{}, true
	}
	code, ok := parsePassCode(presented)
	if !ok {
		return passGrant{Refused: passInvalid}, true
	}
	now := time.Now()
	_, expires, err := a.store.passByToken(r.Context(), a.passTokenHash(code), now)
	switch {
	case errors.Is(err, errNotFound):
		return passGrant{Refused: passInvalid}, true
	case err != nil && r.Context().Err() != nil:
		// The client went away. A 500 here would be a status only a code that
		// parses can cause; the post goes on as one without a Pass instead.
		return passGrant{}, true
	case err != nil:
		a.log.Error("could not read the Pass", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the Pass")
		return passGrant{}, false
	case !now.Before(expires):
		return passGrant{Refused: passExpired}, true
	}
	return passGrant{Valid: true, BudgetKey: a.passBudgetKey(code)}, true
}

// passPostResponse is a created post's answer, with the state of the Pass it
// presented when that Pass was no good.
func passPostResponse(board Board, post Post, g passGrant) map[string]any {
	out := newPostResponse(board, post)
	if g.Refused != "" {
		out["pass"] = g.Refused
	}
	return out
}

// spendPassBudget counts a saved post against the hourly budget of the Pass
// it was made with, if any. It is called once the post is in the database, so
// a post that was refused or failed costs nothing.
func (a *App) spendPassBudget(g passGrant) {
	if g.Valid {
		a.pass.budget.spend(g.BudgetKey, time.Now())
	}
}

/* Hourly budget ------------------------------------------------------------- */

// passBudget counts the posts each Pass has made in the current hour, counted
// in UTC (clockHour), so that it skips the captcha for at most HourlyPosts of
// them. The hour turns on the hour in UTC, which is half past the hour, or a
// quarter to, in a zone that differs from UTC by part of an hour, India's for
// one; the README and the page give India's. Sharing a Pass shares its
// budget: everyone using one code draws on the same count.
//
// It is kept in memory, per process like flood control, keyed on
// passBudgetKey. What it holds about a Pass is which clock hour its count is
// for and the count: no timestamp, so nothing in memory says when a Pass
// posted, to line up against a post. An entry for an hour that has ended is
// dropped when it is next looked at and by the flood-control sweep
// (cooldownSweep), which runs once a minute, so a Pass's count is forgotten
// within a minute of its hour ending while the process has CPU.
//
// Running out never refuses a post: the Pass just stops skipping the captcha
// until the next hour. left and spend are separate calls, so two posts saved
// at once can both be let through on the last post left; the budget is a
// meter, not a lock, and one post over costs nothing worth a lock.
type passBudget struct {
	size int

	mu    sync.Mutex
	hours map[string]passHour
}

// passHour is everything passBudget keeps about a Pass.
type passHour struct {
	// hour is the clock hour the count is for, in hours since the Unix epoch.
	hour  int64
	count int
}

func newPassBudget(size int) *passBudget {
	return &passBudget{size: size, hours: map[string]passHour{}}
}

// clockHour is the UTC clock hour t falls in, counted from the Unix epoch.
func clockHour(t time.Time) int64 {
	return t.Unix() / 3600
}

// left reports whether key has posts left in now's hour.
func (b *passBudget) left(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.hours[key]
	if ok && e.hour != clockHour(now) {
		delete(b.hours, key)
		ok = false
	}
	return !ok || e.count < b.size
}

// spend counts one saved post against key in now's hour.
func (b *passBudget) spend(key string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hour := clockHour(now)
	if len(b.hours) >= cooldownSweepThreshold {
		b.sweep(hour)
	}
	e := b.hours[key]
	if e.hour != hour {
		e = passHour{hour: hour}
	}
	e.count++
	b.hours[key] = e
}

// sweep drops every entry for an hour other than hour. Callers must hold the
// mutex.
func (b *passBudget) sweep(hour int64) {
	for key, e := range b.hours {
		if e.hour != hour {
			delete(b.hours, key)
		}
	}
}

// sweepExpired drops the entries of every hour that has ended, for
// cooldownSweep.
func (b *passBudget) sweepExpired() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep(clockHour(time.Now()))
}

// passReissueMissing answers a reissue asked for with a payment that has
// nothing left to reissue: it bought nothing, it was refunded or disputed, or
// what it bought has gone, such as a Pass 30 days past its expiry (passCutoff),
// one ended by a refund or a dispute of another of its payments, or a paid
// order nobody collected for its own term past its deadline (passOrderKept).
// passReissueRenewal answers one asked for with a renewal's payment, and
// passReissueNoOrigin one for a Pass that no payment can reissue now: none left
// on it pays for all its time, or the one the right moved to is waiting on a
// renewal under way. passReissueSpent refuses a reissue code that gives
// nothing, which the Pass page knows by these words (PASS_REISSUE_SPENT), and
// passReissueRenewing one that cannot be used while a renewal of its Pass is
// under way and is not spent by being refused.
const (
	passReissueMissing = "no Pass or paid order is left from that payment: it bought nothing, it was refunded or disputed, " +
		"or what it bought has gone, such as a Pass 30 days past its expiry, one ended by a refund or dispute of another of its payments, " +
		"or a paid order nobody collected for its own term past its deadline"
	passReissueRenewal = "that payment renewed a Pass; reissue it with the payment that bought the Pass, " +
		"or, if that one was refunded or disputed, with the one renewal that then paid for all the time it had left"
	passReissueNoOrigin = "this Pass can't be reissued now: the payment that bought it was refunded or disputed, " +
		"and no one payment left on it pays for all the time it has left, counting a renewal still being paid for or collected"
	passReissueSpent    = "this reissue code has been used, has run out or was cancelled"
	passReissueRenewing = "this reissue code can't be used while its Pass is being renewed: it works again, until its deadline, " +
		"if that renewal goes unpaid, or is refunded or disputed before it is collected, and is cancelled once the renewal is collected"
)

type passReissueBody struct {
	// PaymentIntent is Stripe's id for the payment ("pi_…"), which the owner
	// finds in the Stripe dashboard from the buyer's receipt.
	PaymentIntent string `json:"payment_intent"`
}

// handlePassReissue gives a buyer who lost their code a way back, behind the
// moderation key: a reissue code the owner sends to the address the buyer
// wrote in from, which works once, until a deadline like an order's, or
// sooner when what it is for goes sooner (createPassReissue), which the answer
// gives. Whoever types it on the Pass page is given a new code for the Pass,
// and the old one stops working. For a purchase or a renewal paid and never
// collected, it collects it instead: a renewal's reissue code renews the Pass
// it was for and hands over no code, or makes a new Pass and hands over its
// code if that one has gone.
//
// The owner never sees a Pass code, so reading Stripe's records beside the
// Pass tables gives them nothing to post with. Only a Pass's origin, the
// payment that made it, or a paid order not collected yet, can be reissued:
// somebody who renewed another person's Pass, and then asks for a reissue
// with their own payment, would otherwise be handed that person's Pass. A
// reissue hands over the whole Pass, so when the origin is refunded or
// disputed and the Pass goes on, the right moves to another payment only
// while that one pays for all the time the Pass has left, a paid renewal not
// collected yet counted (moveOrigin, renewPass); otherwise the Pass cannot be
// reissued, since the server cannot tell one payer from another. Nor can it
// while a renewal of it is under way, paid or not yet, when the right has
// moved (createPassReissue, redeemPassReissue): the renewal is somebody's
// payment, and would land on the reissued Pass.
func (a *App) handlePassReissue(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeMod(w, r) {
		return
	}
	if !a.requirePass(w) || !passRequest(w, r) {
		return
	}
	var req passReissueBody
	if !decodeJSON(w, r, &req) {
		return
	}
	pi := strings.TrimSpace(req.PaymentIntent)
	if !strings.HasPrefix(pi, "pi_") {
		writeError(w, http.StatusBadRequest, `payment_intent: want Stripe's payment id, "pi_…"`)
		return
	}
	code, err := newReissueCode()
	if err != nil {
		a.fail(w, r, err, "could not make a reissue code")
		return
	}
	now := time.Now()
	state, deadline, err := a.store.createPassReissue(r.Context(), a.passPaymentHash(pi), hashToken(code), passClaimDeadline(now), now)
	if err != nil {
		a.fail(w, r, err, "could not make a reissue code")
		return
	}
	switch state {
	case reissueMissing:
		writeError(w, http.StatusNotFound, passReissueMissing)
		return
	case reissueRenewal:
		writeError(w, http.StatusConflict, passReissueRenewal)
		return
	case reissueNoOrigin:
		writeError(w, http.StatusConflict, passReissueNoOrigin)
		return
	}
	a.log.Info("pass reissue code made")
	// The code goes to the owner to send on, and is in no address: a link
	// would leave it in the history of whatever browser opened it.
	writeJSON(w, http.StatusOK, map[string]any{
		"code":       formatReissueCode(code),
		"expires_at": deadline,
	})
}

// handleStripeWebhook takes Stripe's word that a checkout was paid, or that a
// payment was refunded or disputed. The body is read only for the fields
// below and never logged: it carries the payer's email and name.
//
// A 2xx tells Stripe the event is handled; anything else makes it retry for
// days, which is what a database error wants and a forged request does not.
func (a *App) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if !a.passEnabled() {
		writeError(w, http.StatusNotFound, "unknown endpoint")
		return
	}
	body, err := readWebhookBody(w, r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	if err := verifyStripeSignature(r.Header.Get("Stripe-Signature"), body, a.cfg.Pass.StripeWebhookSecret, time.Now()); err != nil {
		writeError(w, http.StatusBadRequest, "signature does not verify")
		return
	}
	var ev stripeEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeError(w, http.StatusBadRequest, "invalid event")
		return
	}

	switch ev.Type {
	case stripeCheckoutCompleted, stripeCheckoutAsyncSuccess:
		var s stripeSessionObject
		if err := json.Unmarshal(ev.Data.Object, &s); err != nil || s.ID == "" {
			writeError(w, http.StatusBadRequest, "invalid checkout session")
			return
		}
		if s.Mode != "payment" {
			break
		}
		// A completed session can still be waiting on a slow payment method, a
		// bank debit say, which can take a couple of weeks. Its order is held
		// for 30 days (passPendingPaymentTTL), longer than that, unless it has
		// gone already (holdPassOrder), and its async_payment_succeeded event
		// comes later and lands here again.
		if ev.Type == stripeCheckoutCompleted && s.PaymentStatus == "unpaid" {
			now := time.Now()
			until := dayCeil(now.Add(passPendingPaymentTTL))
			if err := a.store.holdPassOrder(r.Context(), a.passSessionHash(s.ID), until, now); err != nil {
				a.fail(w, r, err, "could not record the checkout")
				return
			}
			break
		}
		if s.PaymentStatus != "paid" || s.PaymentIntent == nil || *s.PaymentIntent == "" {
			break
		}
		outcome, err := a.store.markPassOrderPaid(r.Context(), a.passSessionHash(s.ID), a.passPaymentHash(*s.PaymentIntent), time.Now())
		if err != nil {
			a.fail(w, r, err, "could not record the payment")
			return
		}
		if outcome == paidUnmatched {
			// No order still there to be paid or still kept, and no Pass still
			// there, has this payment: a checkout of something else sold on the
			// same Stripe account, say, an unpaid order paid after all once
			// something deleted it past its deadline (the prune, or a claim
			// from its own browser, claimOne), or once its own term had passed
			// since that deadline, deleted yet or not (markPassOrderPaid), or a
			// delivery repeated after the order or Pass it bought has gone (an
			// order its own term past its deadline, a Pass 30 days past its
			// expiry or ended by a refund or dispute of another of its
			// payments). Nothing here can tell which, and none should become a
			// Pass; the owner checks the payment against the receipt before
			// refunding it. A repeated delivery while its order or the Pass it
			// bought is still there, or one for a payment refunded or disputed
			// first, is no news, and is not logged.
			a.log.Warn("a paid Stripe checkout matched no open Pass order")
		}
	case stripeCheckoutExpired, stripeCheckoutAsyncFailure:
		// A checkout left unpaid until Stripe closed it, or whose slow payment
		// failed, can never be paid, so its order goes now rather than at its
		// deadline, and the browser that started it stops waiting for it.
		var s stripeSessionObject
		if err := json.Unmarshal(ev.Data.Object, &s); err != nil || s.ID == "" {
			writeError(w, http.StatusBadRequest, "invalid checkout session")
			return
		}
		if err := a.store.dropUnpaidPassOrder(r.Context(), a.passSessionHash(s.ID)); err != nil {
			a.fail(w, r, err, "could not record the expired checkout")
			return
		}
	case stripeChargeRefunded, stripeDisputeCreated:
		var p stripePaymentObject
		if err := json.Unmarshal(ev.Data.Object, &p); err != nil {
			writeError(w, http.StatusBadRequest, "invalid payment")
			return
		}
		// A partial refund is a goodwill gesture and changes nothing; a full
		// refund or a dispute takes back what that payment bought. Any
		// charge.dispute.created does, an inquiry included, since no dispute's
		// status is read, and nothing handles how a dispute closes, so winning
		// one does not bring the term back (the README says so).
		if p.PaymentIntent == nil || *p.PaymentIntent == "" || (ev.Type == stripeChargeRefunded && !p.Refunded) {
			break
		}
		revoked, err := a.store.revokePassPayment(r.Context(), a.passPaymentHash(*p.PaymentIntent), time.Now())
		if err != nil {
			a.fail(w, r, err, "could not record the refund")
			return
		}
		switch revoked {
		case revokedOrder:
			a.log.Info("pass order cancelled by a refund or dispute")
		case revokedTerm:
			a.log.Info("pass payment taken back by a refund or dispute; the Pass goes on")
		case revokedPass:
			a.log.Info("pass cancelled by a refund or dispute")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"received": true})
}

/* Storage ------------------------------------------------------------------- */

// passLockID is the advisory lock every transaction that writes a Pass table
// takes before anything else: creating an order, marking it paid, holding or
// dropping it, collecting it, making and redeeming a reissue code, revoking a
// payment and pruning. They are therefore applied one at a time, in the order
// they took the lock, and each sees everything the ones before it committed.
// Pass purchases are rare, so the queue costs nothing, and it rules out every
// interleaving of a payment, a refund and a collection: no refund can miss a
// Pass being collected beside it, no "paid" can slip past a refund recorded at
// the same moment, and no two of them can deadlock over rows taken in
// different orders. Reading a Pass to post with it (passByToken) takes no
// lock, and no locked transaction makes a network call, so the lock is never
// held for longer than a few statements.
//
// The second key is the schema, so the Pass tables of two schemas in one
// database (a staging copy beside production, or the tests' schemas) are not
// queued behind each other.
const passLockID int32 = 0x50617373 // "Pass"

// passTx runs fn in a transaction holding the Pass lock. A function that runs
// it and holds a lookup to a bound of now (passCutoff, passOrderKept) is given
// a now its caller reads just before calling it, so the bound is the moment
// the request reached the store, earlier than the lookup by no more than the
// wait for the lock. The prune, which only frees rows past such bounds, can be
// given an earlier one, as checkout's is.
func (s *Store) passTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext(current_schema()))`, passLockID); err != nil {
			return fmt.Errorf("take the Pass lock: %w", err)
		}
		return fn(tx)
	})
}

type passOrder struct {
	ClaimHash   []byte
	SessionHash []byte
	// RenewTokenHash is the hash of the code of the Pass to renew, for a
	// renewal.
	RenewTokenHash []byte
	// Days is the term the order buys, EDUCHAN_PASS_DAYS as it is now.
	Days      int
	ExpiresAt time.Time
}

// errPassGone is a renewal whose Pass was refunded, went 30 days past its
// expiry (passCutoff) or was given another code between the checkout's lookup
// and its order.
var errPassGone = errors.New("the Pass to renew is gone")

// createPassOrder records a checkout's order under the claim the checkout gives
// the browser, which no other order is under (pass_orders_claim_idx is unique).
// A renewal's Pass is looked up again under the lock, by the code the checkout
// was given and as gone 30 days past its expiry, as its own lookup was
// (passCutoff), but at now, which the checkout reads once Stripe has answered,
// so a refund, those 30 days running out, or a reissue that gave the Pass
// another code, landing in between while Stripe was being asked for a session,
// answers errPassGone rather than renewing a Pass whose code the browser no
// longer holds. A Pass reissued from a right that moved to a renewal
// (moveOrigin) would otherwise be handed over with this renewal still to land
// on it, past the check that stops a reissue while one is under way
// (renewalUnderWay).
//
// The order names the Pass it renews with no foreign key, whose check would
// lock the Pass's row and leave this transaction's id on it (0012). What the
// key's ON DELETE SET NULL did is done where a Pass is deleted (revokeTerm,
// prunePasses): that transaction clears the Pass from every order naming it,
// and it takes the lock too, so it comes either before the lookup above, which
// then finds no Pass, or after the order is written, which it then clears. No
// order is left naming a deleted Pass, and collecting one cleared makes a new
// Pass (collectPassOrder), as does collecting one whose Pass has since gone 30
// days past its expiry, before the prune clears it (renewPass).
func (s *Store) createPassOrder(ctx context.Context, o passOrder, now time.Time) error {
	return s.passTx(ctx, func(tx pgx.Tx) error {
		var renewID *int64
		if o.RenewTokenHash != nil {
			id, _, err := passWithToken(ctx, tx, o.RenewTokenHash, now)
			if err != nil {
				return err
			}
			if id == 0 {
				return errPassGone
			}
			renewID = &id
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO pass_orders (claim_hash, session_hash, renew_pass_id, days, expires_at)
			VALUES ($1, $2, $3, $4, $5)`, o.ClaimHash, o.SessionHash, renewID, o.Days, o.ExpiresAt)
		return err
	})
}

// What a "paid" event came to.
type paidOutcome int

const (
	paidUnmatched paidOutcome = iota // no open order has the session, and no Pass the payment
	paidRecorded                     // the order is marked paid, now or by an earlier delivery
	paidCollected                    // the payment is on a Pass that has not gone: a delivery after it was collected
	paidRevoked                      // the payment was refunded or disputed first
)

// markPassOrderPaid records the payment behind an order. It is idempotent,
// since Stripe may deliver one event more than once: before the order is
// collected, while it is kept (passOrderKept), and after, when the payment is
// found on its Pass instead, while that Pass has not gone (passCutoff); once
// either has gone, a delivery again finds nothing, as once it is pruned. A
// payment already refunded or disputed is refused, since Stripe may deliver a
// refund or a dispute before a retried "paid", and its order, which can never
// be paid now, is deleted, so the browser that started it stops waiting for it.
// An unpaid order past its claim deadline is still marked until something
// deletes it, the next prune or a claim from its own browser (claimOne), say:
// the money has been taken, and a paid order waits to be collected or reissued
// for its own term past that deadline (prunePasses). Once that term has passed
// too, the order would be gone for every purpose the moment it was marked
// (passOrderKept), so it is not marked, pruned yet or not (passOrderPayable):
// the event finds nothing, as once it is pruned, and the owner is warned the
// same way.
func (s *Store) markPassOrderPaid(ctx context.Context, sessionHash, paymentHash []byte, now time.Time) (paidOutcome, error) {
	var outcome paidOutcome
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		outcome = paidUnmatched
		revoked, err := paymentRevoked(ctx, tx, paymentHash)
		if err != nil {
			return err
		}
		if revoked {
			outcome = paidRevoked
			_, err := tx.Exec(ctx, `
				DELETE FROM pass_orders WHERE session_hash = $1 AND payment_hash IS NULL`, sessionHash)
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE pass_orders SET payment_hash = $2
			WHERE session_hash = $1 AND (payment_hash IS NULL OR payment_hash = $2)
			  AND `+passOrderPayable("", "$3"),
			sessionHash, paymentHash, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			outcome = paidRecorded
			return nil
		}
		var collected bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pass_payments p JOIN passes s ON s.id = p.pass_id
			               WHERE p.payment_hash = $1 AND s.expires_at > $2)`,
			paymentHash, passCutoff(now)).Scan(&collected); err != nil {
			return err
		}
		if collected {
			outcome = paidCollected
		}
		return nil
	})
	return outcome, err
}

// holdPassOrder keeps an unpaid order until at least until. A checkout
// completed with a payment that takes days to confirm, a bank debit say, would
// otherwise go past its deadline, at the next prune or a claim from its own
// browser, before the money arrived. An order that has gone at now, its own
// term past its deadline (passOrderPayable), is not held, pruned yet or not, as
// once it is pruned: no payment Stripe reports for it counts any more, and
// holding it would make one count again.
func (s *Store) holdPassOrder(ctx context.Context, sessionHash []byte, until, now time.Time) error {
	return s.passTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE pass_orders SET expires_at = $2
			WHERE session_hash = $1 AND payment_hash IS NULL AND expires_at < $2
			  AND `+passOrderPayable("", "$3"),
			sessionHash, until, now)
		return err
	})
}

// dropUnpaidPassOrder deletes an order whose checkout can no longer be paid:
// Stripe closed it unpaid, or its payment failed.
func (s *Store) dropUnpaidPassOrder(ctx context.Context, sessionHash []byte) error {
	return s.passTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM pass_orders WHERE session_hash = $1 AND payment_hash IS NULL`, sessionHash)
		return err
	})
}

// passClaimWaiting reports whether any of a browser's claims has anything to
// collect: an order that can still bring a Pass, since a payment Stripe reports
// for it would still count (passOrderPayable). That is a paid order while it is
// kept, until its own term past its deadline, and an unpaid one until that term
// has passed too, whatever its deadline: inside its deadline a claim is told to
// wait, and past it something deletes the order, a claim (claimOne) or the
// prune (prunePasses), say, so a payment reported after that finds nothing. So
// an answer that nothing is waiting is final: no order under the claims can be
// paid any more, so nothing can come under them later. It reads without the
// lock, as passByToken does.
func (s *Store) passClaimWaiting(ctx context.Context, claimHashes [][]byte, now time.Time) (bool, error) {
	return claimsWaiting(ctx, s.pool, claimHashes, now)
}

// claimsWaiting is passClaimWaiting's read, through q: the pool, or the
// transaction of a claim, which answers with it whether anything is left to
// collect (claimPass), so the two hold to one bound.
func claimsWaiting(ctx context.Context, q rowReader, claimHashes [][]byte, now time.Time) (bool, error) {
	var waiting bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pass_orders WHERE claim_hash = ANY($1) AND `+passOrderPayable("", "$2")+`)`,
		claimHashes, now).Scan(&waiting)
	return waiting, err
}

// dropPassClaims moves the order under each of a browser's claims under a
// random hash of its own, which no token hashes to, so the claims collect
// nothing, and no claim reaches the orders again. They are otherwise left as
// they were. Paid, or paid later, each waits to be reissued from its payment
// like any order nobody collected, and goes when a reissue code redeems it,
// when its payment is refunded or disputed while it is kept, or at the first
// prune (prunePasses) once its own term past its deadline has passed. Unpaid,
// it goes when Stripe reports its checkout expired or its payment failed
// (dropUnpaidPassOrder), or reports it paid with a payment already refunded or
// disputed (markPassOrderPaid), and otherwise at the first prune past its
// deadline, a checkout or the daily sweep: a claim, which would delete it then
// too (claimOne), no longer finds it.
func (s *Store) dropPassClaims(ctx context.Context, claimHashes [][]byte) error {
	return s.passTx(ctx, func(tx pgx.Tx) error {
		for _, claimHash := range claimHashes {
			nobody := make([]byte, sha256.Size)
			if _, err := rand.Read(nobody); err != nil {
				return fmt.Errorf("drop the claim: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE pass_orders SET claim_hash = $2 WHERE claim_hash = $1`, claimHash, nobody); err != nil {
				return err
			}
		}
		return nil
	})
}

// paymentRevoked reports whether a payment was refunded or disputed.
func paymentRevoked(ctx context.Context, tx pgx.Tx, paymentHash []byte) (bool, error) {
	var revoked bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pass_revocations WHERE payment_hash = $1)`, paymentHash).Scan(&revoked)
	return revoked, err
}

// passCutoff is the expiry on or before which a Pass has gone at now: one 30
// days past its expiry (passExpiredGrace), to the instant, can no longer be
// renewed, and counts as gone for every purpose from then on, exactly as if
// prunePasses, which deletes it by the same bound, had run already, so the
// prune only frees its rows. Every lookup of a Pass, by its code
// (passWithToken), by its id or through one of its payments, holds it to
// expires_at > passCutoff(now), but for moveOrigin's (below), and anything new
// that looks one up must too; what a transaction then reads or writes of the
// Pass it found, under the lock, follows from that lookup (revokeTerm after
// revokePassPayment's, for example). So checking it, posting with it, renewing
// it, collecting a renewal of it, reissuing it and refunding one of its
// payments, for example, each find no Pass, as they would once it was pruned.
// An order names the Pass it renews until that Pass is deleted (revokeTerm,
// prunePasses), and moveOrigin, which revokePassPayment calls on that Pass when
// it takes such an order back, reads it by its id without the bound, which
// moves nothing for one past those 30 days, since none of its terms still runs.
func passCutoff(now time.Time) time.Time { return now.Add(-passExpiredGrace) }

// passGoneAt is the moment a Pass that runs out at expires has gone: the first
// now whose passCutoff is not before expires.
func passGoneAt(expires time.Time) time.Time { return expires.Add(passExpiredGrace) }

// passOrderEnd is, in SQL, when the pass_orders row that alias names ("o." in
// a join, or "" for the table's own columns) stops being kept: an unpaid order
// at its deadline, and a paid one, which waits to be collected or reissued, its
// own term (days) after that (passPaidOrderEnd).
func passOrderEnd(alias string) string {
	return "CASE WHEN " + alias + "payment_hash IS NULL THEN " + alias + "expires_at " +
		"ELSE " + passPaidOrderEnd(alias) + " END"
}

// passPaidOrderEnd is, in SQL, when that order stops being kept once it is
// paid, whether it is yet or not: its own term (days) after its deadline, by
// when a Pass bought with it at its deadline would have run out. The term
// counts in whole 24-hour days, so a UTC midnight stays one whatever the
// session's time zone.
func passPaidOrderEnd(alias string) string {
	return alias + "expires_at + make_interval(hours => 24 * " + alias + "days)"
}

// passOrderPayable is, in SQL, whether a payment Stripe reports for that order
// at the time in the parameter now names still counts: until the end of the
// keep it would have once paid (passPaidOrderEnd), whatever its deadline, and
// so, for an order paid already, exactly while it is kept (passOrderKept). An
// unpaid order past its deadline is no longer waited for, and goes when
// something deletes it first: the next prune (prunePasses) or a claim from its
// own browser (claimOne), say, which the Pass page sends on a visit while the
// order counts as something to collect (passClaimWaiting); a payment Stripe
// reports for it after that finds nothing, and the owner is warned. An unpaid
// order past this bound has gone for every purpose, pruned yet or not, as a
// paid one past its keep has, so the prune only frees its row: marking it paid
// (markPassOrderPaid), holding it for a slow payment (holdPassOrder), counting
// it as a renewal under way (renewalUnderWay) and counting it as something for
// its browser to collect (passClaimWaiting, claimPass) each hold it to this,
// one bound, so none of them takes an order the others have let go, and
// anything new that decides what to do with an unpaid order past its deadline
// must too.
func passOrderPayable(alias, now string) string {
	return passPaidOrderEnd(alias) + " > " + now
}

// passOrderKept is, in SQL, whether that order is still kept at the time in the
// parameter now names ("$2", say). A paid order no longer kept has gone for
// every purpose, exactly as if prunePasses, which deletes it by the same bound,
// had run already, so the prune only frees its row. Every read of a paid order
// that decides what to do holds it to this, or to passOrderPayable, the same
// bound for a paid order, and anything new that reads one must too; a write
// that only moves its claim (dropPassClaims) or clears the Pass it names
// (revokeTerm, prunePasses), and so decides nothing for it, need not. So
// collecting it, by its claim (claimOne) or by a reissue code
// (redeemPassReissue), making a reissue code for it (createPassReissue), Stripe
// reporting it paid again (markPassOrderPaid), a refund of it
// (revokePassPayment), and counting it as something for a browser to collect
// (passClaimWaiting, claimPass) or as a renewal waiting to land on its Pass
// (moveOrigin, renewalUnderWay), for example, each find no order, as once it is
// pruned. An unpaid order is no longer waited for from its deadline on, but a
// payment Stripe reports for it still counts until it is deleted, by the prune
// or by a claim from its own browser (claimOne), say, which the Pass page sends
// while the order counts as something to collect, and never once its own term
// has passed since that deadline (passOrderPayable): so marking it paid
// (markPassOrderPaid), holding it for a slow payment (holdPassOrder), counting
// it as a renewal under way (renewalUnderWay) and counting it as something for
// its browser to collect (passClaimWaiting, claimPass), for example, take an
// unpaid order past its deadline until then and none after, as once it is
// pruned. Deleting one, as dropUnpaidPassOrder does once Stripe closes its
// checkout unpaid, decides nothing the prune would not, and takes one of any
// age.
func passOrderKept(alias, now string) string {
	return passOrderEnd(alias) + " > " + now
}

// passByToken returns the Pass a code hash belongs to at now, run out or not,
// or errNotFound once it has gone (passCutoff). It reads without the lock.
func (s *Store) passByToken(ctx context.Context, tokenHash []byte, now time.Time) (int64, time.Time, error) {
	id, expires, err := passWithToken(ctx, s.pool, tokenHash, now)
	if err == nil && id == 0 {
		err = errNotFound
	}
	return id, expires, err
}

// passCompletion is what handing a Pass over needs from the caller: the hash
// of the code the request's claim or reissue code and its nonce give
// (passClaimCode, passReissuedCode), and the time. The term each payment buys
// is its order's own.
type passCompletion struct {
	TokenHash []byte
	Now       time.Time
}

// claimResult is what collecting a claim, or redeeming a reissue code, came
// to.
type claimResult struct {
	// Issued is set when a Pass is under the code whose hash the
	// passCompletion carried: made now, or by an earlier request with the
	// same claim or reissue code and nonce whose answer never arrived (Made
	// tells which). IssuedExpiresAt is when it runs out.
	Issued          bool
	Made            bool
	IssuedExpiresAt time.Time
	// Renewed is set when the order renewed the Pass it was for, and
	// RenewedExpiresAt is that Pass's new expiry. The Pass is the one whose
	// code the checkout was sent, which the browser collecting it may no
	// longer hold.
	Renewed          bool
	RenewedExpiresAt time.Time
	// Waiting is set while a claim's order is unpaid and inside its
	// deadline, and nothing else was collected.
	Waiting bool
	// Renewing is set when a reissue code is for a right that moved to a
	// renewal (moveOrigin) and its Pass is being renewed: the reissue code is
	// left as it was, to work again if the renewal comes to nothing.
	Renewing bool
	// Which is the claim, of those claimPass was given, that the answer is
	// about, and More whether anything is left to collect under any of them.
	Which int
	More  bool
	// pass is the Pass a claim asked again found under its code (claimOne),
	// which claimPass orders several by.
	pass int64
}

// rowReader is what passWithToken and claimsWaiting read with: the pool, or a
// transaction.
type rowReader interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// passWithToken returns the Pass under a code hash at now and its expiry, run
// out or not, or 0 once it has gone (passCutoff).
func passWithToken(ctx context.Context, q rowReader, tokenHash []byte, now time.Time) (int64, time.Time, error) {
	var id int64
	var expires time.Time
	err := q.QueryRow(ctx, `SELECT id, expires_at FROM passes WHERE token_hash = $1 AND expires_at > $2`,
		tokenHash, passCutoff(now)).Scan(&id, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	return id, expires, err
}

// passClaimant is one claim a request carries: the hash of its token, and the
// hash of the code that token and the request's nonce give (passClaimCode).
type passClaimant struct {
	ClaimHash []byte
	TokenHash []byte
}

// claimPass collects the order under a browser's claims, one order an answer.
// Every checkout makes a claim of its own, so there is at most one order
// under each, and nothing from another checkout is ever folded into what it
// buys. The claims are looked under in the order given, and the first with a
// paid order is collected; failing that, a claim asked again is answered with
// the Pass an earlier ask made, whose answer was lost, the one made last of
// several; failing that, the answer is to wait while any order is unpaid.
// More says whether anything is left under any of them, so the page asks
// again for the next, by the bound GET /api/pass answers by (claimsWaiting):
// an unpaid order past its deadline under a claim the loop stopped before
// counts, and the next ask deletes it, so an answer that nothing is left is
// final.
func (s *Store) claimPass(ctx context.Context, claims []passClaimant, now time.Time) (claimResult, error) {
	var res claimResult
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		res = claimResult{}
		var again claimResult
		waiting := false
		for i, c := range claims {
			got, err := claimOne(ctx, tx, c.ClaimHash, passCompletion{TokenHash: c.TokenHash, Now: now})
			if err != nil {
				return err
			}
			got.Which = i
			if got.Made || got.Renewed {
				res = got
				break
			}
			// Of several claims with a Pass under this nonce, the one made
			// last is answered, as the one whose answer can have been lost
			// last.
			if got.Issued && got.pass > again.pass {
				again = got
			}
			waiting = waiting || got.Waiting
		}
		if !res.Made && !res.Renewed {
			res = again
			res.Waiting = !again.Issued && waiting
		}
		hashes := make([][]byte, len(claims))
		for i, c := range claims {
			hashes[i] = c.ClaimHash
		}
		var err error
		res.More, err = claimsWaiting(ctx, tx, hashes, now)
		return err
	})
	return res, err
}

// claimOne collects the order under one claim. Once it is paid, a renewal
// renews the Pass it was for, or makes a new one if that Pass is gone, and
// anything else makes a new Pass, under the code whose hash c carries, for the
// order's own term. The order is deleted as it is collected, so a claim asked
// again finds none, and is answered with the Pass under that code if there is
// one: made by a claim with the same cookie and nonce, whose answer was lost. A
// Pass refunded away since, given a new code by a reissue or gone 30 days past
// its expiry (passCutoff) is under no such code any more, and nothing comes
// back for it.
//
// An order no longer kept (passOrderKept) is deleted, as prunePasses would,
// and nothing comes of it, nor of a payment Stripe reports for it afterwards,
// which finds no order: an unpaid one past its deadline, and a paid one its
// own term past that. A paid order is collected past its deadline until then:
// it was paid for.
func claimOne(ctx context.Context, tx pgx.Tx, claimHash []byte, c passCompletion) (claimResult, error) {
	var payment []byte
	var renewID *int64
	var days int
	var kept bool
	err := tx.QueryRow(ctx, `
		SELECT payment_hash, renew_pass_id, days, `+passOrderKept("", "$2")+` FROM pass_orders
		WHERE claim_hash = $1`, claimHash, c.Now).Scan(&payment, &renewID, &days, &kept)
	if errors.Is(err, pgx.ErrNoRows) {
		id, expires, err := passWithToken(ctx, tx, c.TokenHash, c.Now)
		return claimResult{Issued: id != 0, IssuedExpiresAt: expires, pass: id}, err
	}
	if err != nil {
		return claimResult{}, err
	}
	if payment == nil && kept {
		return claimResult{Waiting: true}, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM pass_orders WHERE claim_hash = $1`, claimHash); err != nil {
		return claimResult{}, err
	}
	if payment == nil || !kept {
		return claimResult{}, nil
	}
	// Revoking deletes the order, so this never holds while the lock does its
	// job; it is checked anyway, as it is everywhere a payment turns into a
	// Pass.
	if revoked, err := paymentRevoked(ctx, tx, payment); err != nil || revoked {
		return claimResult{}, err
	}
	return collectPassOrder(ctx, tx, payment, renewID, days, c)
}

// collectPassOrder turns a paid order its caller has deleted into what it
// bought: a renewal renews the Pass it was for, and anything else, or a
// renewal whose Pass is gone, makes a new Pass under the code whose hash c
// carries. Either way the payment adds the order's own term.
func collectPassOrder(ctx context.Context, tx pgx.Tx, payment []byte, renewID *int64, days int, c passCompletion) (claimResult, error) {
	if renewID != nil {
		expires, found, err := renewPass(ctx, tx, *renewID, payment, days, c.Now)
		if err != nil || found {
			return claimResult{Renewed: found, RenewedExpiresAt: expires}, err
		}
	}
	_, expires, err := issuePass(ctx, tx, payment, c.TokenHash, days, c.Now)
	if err != nil {
		return claimResult{}, err
	}
	return claimResult{Issued: true, Made: true, IssuedExpiresAt: expires}, nil
}

// issuePass makes a new Pass for a payment, under the code whose hash it is
// given, for a term of days from now, and links the payment to it as its
// origin.
func issuePass(ctx context.Context, tx pgx.Tx, payment, tokenHash []byte, days int, now time.Time) (int64, time.Time, error) {
	expires := passExpiry(now, days)
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO passes (token_hash, expires_at) VALUES ($1, $2) RETURNING id`,
		tokenHash, expires).Scan(&id); err != nil {
		return 0, time.Time{}, err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO pass_payments (payment_hash, pass_id, days, ends_at, origin) VALUES ($1, $2, $3, $4, 'bought')`,
		payment, id, termDays(now, expires), expires)
	return id, expires, err
}

// renewPass extends a Pass by a term of days from its expiry, or from now if
// it has run out, and links the payment to it with the term it bought
// (termDays) and the day that term ends, which is what a refund of it reads.
// found is false when there is no such Pass, or it has gone (passCutoff).
func renewPass(ctx context.Context, tx pgx.Tx, passID int64, payment []byte, days int, now time.Time) (time.Time, bool, error) {
	var current time.Time
	err := tx.QueryRow(ctx, `SELECT expires_at FROM passes WHERE id = $1 AND expires_at > $2`,
		passID, passCutoff(now)).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	base := maxTime(current, now)
	expires := passExpiry(base, days)
	if _, err := tx.Exec(ctx, `UPDATE passes SET expires_at = $2 WHERE id = $1`, passID, expires); err != nil {
		return time.Time{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pass_payments (payment_hash, pass_id, days, ends_at) VALUES ($1, $2, $3, $4)`,
		payment, passID, termDays(base, expires), expires); err != nil {
		return time.Time{}, false, err
	}
	// A renewal that the right to reissue the Pass moved to when its purchase
	// was refunded or disputed (moveOrigin) loses it now: it no longer pays
	// for all the time the Pass has left. Its unused reissue codes go with the
	// right, a code refused while this renewal was under way included.
	_, err = tx.Exec(ctx, `
		WITH moved AS (
		       UPDATE pass_payments SET origin = NULL WHERE pass_id = $1 AND origin = 'moved'
		       RETURNING payment_hash)
		DELETE FROM pass_reissues WHERE payment_hash IN (SELECT payment_hash FROM moved)`, passID)
	return expires, true, err
}

// What asking for a reissue code came to.
type reissueState int

const (
	reissueMissing  reissueState = iota // no paid order for the payment and no Pass left from it, or it was refunded or disputed
	reissueRenewal                      // the payment renewed a Pass; only its origin reissues it
	reissueNoOrigin                     // the payment is on a Pass with no origin now (moveOrigin, renewPass, renewalUnderWay)
	reissueReady                        // a reissue code was made
)

// createPassReissue records a reissue code for a payment: a Pass's origin, or
// a paid order nobody collected. A Pass that has gone (passCutoff) is no
// payment's, nor is a paid order no longer kept (passOrderKept). A newer
// reissue code for the same payment replaces the one before it. A right that
// moved to a renewal (moveOrigin) gives no reissue code while the Pass is
// being renewed (renewalUnderWay).
//
// The reissue code is good until deadline, the caller's week, or sooner when
// what it is for goes sooner as that stands now: for an order, the end of its
// keep (passOrderEnd), and for a Pass, the moment it has gone, 30 days past its
// expiry as it is now (passGoneAt), which a renewal of the Pass afterwards does
// not move for the code. A Pass that goes sooner still, ended by a refund or
// dispute of another of its payments, leaves the code giving nothing until its
// deadline (redeemPassReissue). The deadline the code is given is returned, to
// answer.
func (s *Store) createPassReissue(ctx context.Context, paymentHash, tokenHash []byte, deadline, now time.Time) (reissueState, time.Time, error) {
	var state reissueState
	var until time.Time
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		state, until = reissueMissing, deadline
		if revoked, err := paymentRevoked(ctx, tx, paymentHash); err != nil || revoked {
			return err
		}
		var keptUntil time.Time
		err := tx.QueryRow(ctx, `
			SELECT `+passOrderEnd("")+` FROM pass_orders WHERE payment_hash = $1 AND `+passOrderKept("", "$2"),
			paymentHash, now).Scan(&keptUntil)
		switch {
		case err == nil:
			until = minTime(until, keptUntil)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		default:
			var origin, orphaned bool
			var expires time.Time
			err := tx.QueryRow(ctx, `
				SELECT p.origin IS NOT NULL,
				       NOT EXISTS (SELECT 1 FROM pass_payments o WHERE o.pass_id = p.pass_id AND o.origin IS NOT NULL),
				       s.expires_at
				FROM pass_payments p JOIN passes s ON s.id = p.pass_id
				WHERE p.payment_hash = $1 AND s.expires_at > $2`,
				paymentHash, passCutoff(now)).Scan(&origin, &orphaned, &expires)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return nil
			case err != nil:
				return err
			case orphaned:
				state = reissueNoOrigin
				return nil
			case !origin:
				state = reissueRenewal
				return nil
			}
			renewing, err := renewalUnderWay(ctx, tx, paymentHash, now)
			if err != nil {
				return err
			}
			if renewing {
				state = reissueNoOrigin
				return nil
			}
			until = minTime(until, passGoneAt(expires))
		}
		state = reissueReady
		_, err = tx.Exec(ctx, `
			INSERT INTO pass_reissues (token_hash, payment_hash, expires_at) VALUES ($1, $2, $3)
			ON CONFLICT (payment_hash) DO UPDATE SET token_hash = EXCLUDED.token_hash, expires_at = EXCLUDED.expires_at`,
			tokenHash, paymentHash, until)
		return err
	})
	return state, until, err
}

// renewalUnderWay reports whether a payment holds the right to reissue its Pass
// that moved to it when the purchase was refunded or disputed (moveOrigin),
// while any order renews that Pass, paid or not yet. A reissue then would hand
// the payer the Pass with that renewal still to land on it, and somebody else's
// time with it, so it waits: an unpaid order counts while a payment Stripe
// reports for it would still count (passOrderPayable, the bound
// markPassOrderPaid holds it to), so until it goes, when Stripe reports its
// checkout expired or its payment failed, or reports it paid with a payment
// already refunded or disputed, or past its deadline when the prune
// (prunePasses) or a claim from its own browser (claimOne) deletes it, and
// never once its own term has passed since that deadline, pruned yet or not,
// when nothing can pay it any more; a paid one, once it is no longer kept
// (passOrderKept); a refunded or disputed one goes at once; and a collected one
// takes the right away (renewPass). A Pass that has gone (passCutoff) has no
// renewal under way, as once the prune has cleared it from its orders.
func renewalUnderWay(ctx context.Context, tx pgx.Tx, paymentHash []byte, now time.Time) (bool, error) {
	var renewing bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pass_payments p JOIN passes s ON s.id = p.pass_id
		                                             JOIN pass_orders o ON o.renew_pass_id = p.pass_id
		               WHERE p.payment_hash = $1 AND p.origin = 'moved' AND s.expires_at > $2
		                 AND (o.payment_hash IS NULL AND `+passOrderPayable("o.", "$3")+`
		                      OR o.payment_hash IS NOT NULL AND `+passOrderKept("o.", "$3")+`))`,
		paymentHash, passCutoff(now), now).Scan(&renewing)
	return renewing, err
}

// redeemPassReissue spends a reissue code. A paid order nobody collected is
// collected, as claimPass would: a renewal renews its Pass, and anything else,
// or a renewal whose Pass is gone, makes a Pass under the code the reissue code
// and the nonce give, for the order's own term. Otherwise the Pass the payment
// is the origin of is given that code, which is what stops the old one working.
// A reissue code that is unknown, used, past its deadline or for a refunded
// payment gives nothing, unless a Pass is under that code already: the reissue
// code was spent by an earlier request with the same nonce, whose answer never
// arrived, and it is answered again. A Pass refunded away since, given a new
// code by another reissue or gone 30 days past its expiry (passCutoff) is under
// no such code, and nothing comes back for it; nor does a renewal that renewed
// its Pass, which made none under that code. A reissue code made for a Pass's
// origin and used once that Pass has gone is spent on nothing, as once the Pass
// is pruned. One made for a paid order runs out when the order stops being kept
// (createPassReissue), and the order is collected here only while it is kept
// (passOrderKept) all the same.
//
// A reissue code for a right that moved to a renewal (moveOrigin) is not used
// while the Pass is being renewed, paid or not yet (renewalUnderWay): the
// renewal would land on the Pass handed over. It is refused and left as it
// was (Renewing), so it works again, until its deadline, if the renewal goes
// unpaid, or is refunded or disputed before it is collected; collected, the
// renewal takes the right away and cancels it (renewPass).
func (s *Store) redeemPassReissue(ctx context.Context, tokenHash []byte, c passCompletion) (claimResult, error) {
	var res claimResult
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		res = claimResult{}
		// Checked first, so a reissue code spent by a request whose answer
		// was lost is answered again rather than refused.
		id, expires, err := passWithToken(ctx, tx, c.TokenHash, c.Now)
		if err != nil || id != 0 {
			res.Issued, res.IssuedExpiresAt = id != 0, expires
			return err
		}
		var payment []byte
		err = tx.QueryRow(ctx, `
			SELECT payment_hash FROM pass_reissues WHERE token_hash = $1 AND expires_at > $2`,
			tokenHash, c.Now).Scan(&payment)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if res.Renewing, err = renewalUnderWay(ctx, tx, payment, c.Now); err != nil || res.Renewing {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM pass_reissues WHERE token_hash = $1`, tokenHash); err != nil {
			return err
		}
		// Revoking deletes the reissue code, so this never holds while the
		// lock does its job; it is checked anyway, as it is everywhere a
		// payment turns into a Pass.
		if revoked, err := paymentRevoked(ctx, tx, payment); err != nil || revoked {
			return err
		}
		var renewID *int64
		var days int
		err = tx.QueryRow(ctx, `
			DELETE FROM pass_orders WHERE payment_hash = $1 AND `+passOrderKept("", "$2")+`
			RETURNING renew_pass_id, days`, payment, c.Now).Scan(&renewID, &days)
		switch {
		case err == nil:
			res, err = collectPassOrder(ctx, tx, payment, renewID, days, c)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		err = tx.QueryRow(ctx, `
			UPDATE passes SET token_hash = $2
			WHERE id = (SELECT pass_id FROM pass_payments WHERE payment_hash = $1 AND origin IS NOT NULL)
			  AND expires_at > $3
			RETURNING expires_at`, payment, c.TokenHash, passCutoff(c.Now)).Scan(&res.IssuedExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		res.Issued, res.Made = err == nil, err == nil
		return err
	})
	return res, err
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// What revoking a payment undid.
type revocation int

const (
	revokedNothing revocation = iota // nothing of the payment's was left to undo
	revokedOrder                     // an order not yet collected
	revokedTerm                      // the payment, from a Pass that goes on
	revokedPass                      // the Pass, which had no time to come without it
)

// revokePassPayment undoes what a refunded or disputed payment bought: the
// order still waiting to be collected, or the part of its term still to come
// (revokeTerm). A refund of one renewal therefore costs the holder that
// renewal and nothing else; a refund of a purchase whose term has run takes
// nothing off the renewals paid after it; and somebody who renews another
// person's Pass and disputes the payment takes back only what they gave.
//
// The payment is remembered either way, for 30 days rounded up to the next
// midnight UTC and on until the prune deletes its row (passRevocationTTL), so a
// late "paid" event for it cannot reopen what was undone.
func (s *Store) revokePassPayment(ctx context.Context, paymentHash []byte, now time.Time) (revocation, error) {
	var res revocation
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		res = revokedNothing
		// An order no longer kept (passOrderKept) is taken back as none, as
		// once it is pruned, and its row is left to that prune.
		var renewID *int64
		err := tx.QueryRow(ctx, `
			DELETE FROM pass_orders WHERE payment_hash = $1 AND `+passOrderKept("", "$2")+`
			RETURNING renew_pass_id`, paymentHash, now).Scan(&renewID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			res = revokedOrder
			// A paid renewal not collected yet counts as a term to come of the
			// Pass it renews, so taking it back is applied as a payment of that
			// Pass would be. An order whose Pass was deleted since names none
			// (revokeTerm, prunePasses), and nothing moves; nor does anything
			// for one whose Pass has gone 30 days past its expiry, before the
			// prune clears it, since none of that Pass's terms still runs
			// (moveOrigin).
			if renewID != nil {
				if err := moveOrigin(ctx, tx, *renewID, now); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM pass_reissues WHERE payment_hash = $1`, paymentHash); err != nil {
			return err
		}
		// A payment of a Pass that has gone (passCutoff) is on no Pass, as once
		// the prune has deleted it, so nothing of it is taken back, and its row
		// is left to that prune.
		var p revokedPayment
		err = tx.QueryRow(ctx, `
			DELETE FROM pass_payments p USING passes s
			WHERE p.payment_hash = $1 AND s.id = p.pass_id AND s.expires_at > $2
			RETURNING p.pass_id, p.days, p.ends_at`,
			paymentHash, passCutoff(now)).Scan(&p.passID, &p.days, &p.ends)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			if res, err = revokeTerm(ctx, tx, p, now); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO pass_revocations (payment_hash, expires_at) VALUES ($1, $2)
			ON CONFLICT (payment_hash) DO UPDATE SET expires_at = EXCLUDED.expires_at`,
			paymentHash, dayCeil(now.Add(passRevocationTTL)))
		return err
	})
	return res, err
}

// revokedPayment is the row of a payment being revoked.
type revokedPayment struct {
	passID int64
	days   int
	ends   time.Time
}

// revokeTerm takes what is left of a revoked payment's term off its Pass, in
// whole days, and never more than the term; a term already run takes off
// nothing. The terms paid after it move back by as much, since the Pass now
// reaches them sooner. Expiries stay on midnights, so what is left is rounded
// to whole days one way or the other, and the rounding never costs another
// payment: with no term after it, the days are rounded up, so the refunded
// term stops at once rather than at midnight, and with one, rounded down, so
// it stops at the next midnight and the terms after it come forward whole. A
// Pass left with no paid time to come is deleted, and its other payments with
// it: one whose only time left is the rest of a day a refunded term stopped
// at goes as one with none would. The same statement clears it from every
// order that renews it, paid or not, writing to pass_orders alone, so none
// names the deleted Pass (0012), and collecting one makes a new Pass.
//
// A Pass that goes on without its origin, the refunded payment having been
// it, could no longer be reissued; moveOrigin decides whether another payment
// takes its place.
func revokeTerm(ctx context.Context, tx pgx.Tx, p revokedPayment, now time.Time) (revocation, error) {
	// later is whether a term was paid after this one, and paid whether any
	// payment left has time to come; the terms after this one stay after now
	// however far they move back, so asking before they do gives the same.
	var expires time.Time
	var later, paid bool
	if err := tx.QueryRow(ctx, `
		SELECT expires_at,
		       EXISTS (SELECT 1 FROM pass_payments WHERE pass_id = $1 AND ends_at > $2),
		       EXISTS (SELECT 1 FROM pass_payments WHERE pass_id = $1 AND ends_at > $3)
		FROM passes WHERE id = $1`, p.passID, p.ends, now).Scan(&expires, &later, &paid); err != nil {
		return revokedNothing, err
	}
	off := daysOff(p.ends, now)
	if later {
		off = wholeDaysLeft(p.ends, now)
	}
	off = min(p.days, off)
	expires = expires.UTC().AddDate(0, 0, -off)
	if !expires.After(now) || !paid {
		_, err := tx.Exec(ctx, `
			WITH gone AS (DELETE FROM passes WHERE id = $1 RETURNING id)
			UPDATE pass_orders SET renew_pass_id = NULL WHERE renew_pass_id IN (SELECT id FROM gone)`, p.passID)
		return revokedPass, err
	}
	if off > 0 {
		if _, err := tx.Exec(ctx, `UPDATE passes SET expires_at = $2 WHERE id = $1`, p.passID, expires); err != nil {
			return revokedNothing, err
		}
		// In whole 24-hour days, so a UTC midnight stays one whatever the
		// session's time zone.
		if _, err := tx.Exec(ctx, `
			UPDATE pass_payments SET ends_at = ends_at - make_interval(hours => 24 * $3)
			WHERE pass_id = $1 AND ends_at > $2`, p.passID, p.ends, off); err != nil {
			return revokedNothing, err
		}
	}
	if err := moveOrigin(ctx, tx, p.passID, now); err != nil {
		return revokedNothing, err
	}
	return revokedTerm, nil
}

// moveOrigin gives a Pass with no origin left one, when a payment can be it.
// Reissuing hands over the whole Pass, and the server cannot tell one payer
// from another, so the right moves to a payment left on it only while that
// payment pays for all the time the Pass has left: from when it is the one
// payment whose term is still running or to come, every other's having ended
// and no paid renewal of the Pass waiting to be collected, until the Pass is
// renewed again (renewPass). Otherwise the Pass has no origin, and no payment
// of it can be reissued (createPassReissue), so somebody who renewed a code
// they saw once is never handed time its holder paid for after them; and
// while a renewal of it is under way, paid or not yet, the one it moved to
// cannot be reissued either (renewalUnderWay). The rule is applied whenever a
// payment of such a Pass is revoked, or a paid renewal of it not collected
// yet, which can leave one such payment; a term that simply runs out, or an
// order that lapses, changes nothing. A Pass that has an origin keeps it.
func moveOrigin(ctx context.Context, tx pgx.Tx, passID int64, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE pass_payments SET origin = 'moved'
		WHERE pass_id = $1 AND ends_at > $2
		  AND NOT EXISTS (SELECT 1 FROM pass_payments WHERE pass_id = $1 AND origin IS NOT NULL)
		  AND NOT EXISTS (SELECT 1 FROM pass_orders o
		                  WHERE o.renew_pass_id = $1 AND o.payment_hash IS NOT NULL AND `+passOrderKept("o.", "$2")+`)
		  AND (SELECT count(*) FROM pass_payments WHERE pass_id = $1 AND ends_at > $2) = 1`,
		passID, now)
	return err
}

// prunePasses deletes what has outlived its use: orders no longer kept
// (passOrderKept), unpaid ones past their deadline and paid ones nobody
// collected once a Pass bought with one at its deadline would have run out too
// (the deadline plus the order's own term); reissue codes past their deadline;
// remembered refunds past their 30 days, rounded up to a midnight UTC
// (passRevocationTTL); and Passes 30 days past expiry, which can no longer be
// renewed and are bought anew instead. A paid order and a Pass count as gone
// from that moment (passOrderKept, passCutoff), as does an unpaid order once
// its own term has passed since its deadline (passOrderPayable), so this only
// frees their rows. A Pass that goes is cleared from every order that renews
// it, as revokeTerm clears one, so no order kept names a deleted Pass, and
// collecting one makes a new Pass. Checkout runs it, and so does the daily
// sweep, which needs no Pass setting for it: every order carries its own term.
func (s *Store) prunePasses(ctx context.Context, now time.Time) error {
	return s.passTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			WITH r AS (DELETE FROM pass_revocations WHERE expires_at <= $1),
			     i AS (DELETE FROM pass_reissues WHERE expires_at <= $1)
			DELETE FROM pass_orders WHERE NOT (`+passOrderKept("", "$1")+`)`, now)
		if err != nil {
			return err
		}
		// A statement of its own, after the orders', since one statement that
		// both deleted an order and cleared it would leave Postgres to pick
		// which of the two happens.
		_, err = tx.Exec(ctx, `
			WITH gone AS (DELETE FROM passes WHERE expires_at <= $1 RETURNING id)
			UPDATE pass_orders SET renew_pass_id = NULL WHERE renew_pass_id IN (SELECT id FROM gone)`,
			passCutoff(now))
		return err
	})
}
