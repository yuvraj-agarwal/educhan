package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The educhan Pass, after 4chan's: a year of posting without the captcha,
// bought for a small fee that helps keep the site running.
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
//     log are the same whether or not a Pass was used, and using one writes
//     nothing, so there is no "last used" to line up against a post.
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
	// createCheckout's are. A slow payment's order can be held only once its
	// checkout is completed, so no later than this after it started.
	stripeCheckoutTTL = 24 * time.Hour
	// passRevocationTTL is how long a refunded payment is remembered after its
	// order is gone, longer than Stripe goes on retrying an event.
	passRevocationTTL = 30 * 24 * time.Hour
	// passExpiredGrace is how long an expired Pass is kept, so it can still be
	// renewed rather than bought again, and so a post with it is told it has
	// expired rather than that it is not a Pass at all.
	passExpiredGrace = 30 * 24 * time.Hour
	// passActivateCooldown spaces out code checks from one address. A code is
	// 100 random bits, so this is courtesy to the database, not a defence.
	passActivateCooldown = 2 * time.Second
	// passPriceTTL is how long the price read from Stripe is shown before it is
	// read again, so a price changed in Stripe shows up within the hour.
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

// parsePassCode reads a code as a person might type it: any case, with or
// without the dashes, with spaces, and with the letters Crockford's alphabet
// folds into digits (O for 0, I and L for 1).
func parsePassCode(raw string) (string, bool) {
	out := make([]byte, 0, passCodeLen)
	for _, r := range strings.ToUpper(raw) {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			continue
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		if r > 0x7f || strings.IndexByte(passAlphabet, byte(r)) < 0 || len(out) == passCodeLen {
			return "", false
		}
		out = append(out, byte(r))
	}
	if len(out) != passCodeLen {
		return "", false
	}
	return string(out), true
}

// passTokenHash is what the database holds of a code: keyed, so a dump of the
// table cannot be checked against a code without the secret either.
func (a *App) passTokenHash(code string) []byte {
	return hmacSum([]byte(a.cfg.Pass.Secret), "pass-token:"+code)
}

// passNonceCode is the code a collection hands over: derived with the secret
// from a nonce the page draws for it, rather than drawn here, so collecting is
// safe to repeat. The page keeps the nonce until an answer reaches it, and a
// claim or reissue whose answer was lost after the server had committed it (a
// tab closed, a page reloaded, a connection dropped) is asked again with the
// same nonce and handed the same code, where a fresh draw would leave the
// buyer a Pass they can never see. The nonce is 128 random bits that only that
// page holds, the secret is only the server's, and the database keeps neither
// the nonce nor the code, only the code's HMAC, so the code is as hard to
// guess as a random one: 256 is a multiple of 32, so masking a byte of the
// HMAC to five bits picks every character with equal probability. Whoever
// holds the nonce can be handed the code again, as whoever
// holds the code can use it: the page drops the nonce once the code is in its
// hands.
func (a *App) passNonceCode(nonce string) string {
	sum := hmacSum([]byte(a.cfg.Pass.Secret), "pass-nonce-code:"+nonce)
	out := make([]byte, passCodeLen)
	for i := range out {
		out[i] = passAlphabet[sum[i]&31]
	}
	return string(out)
}

// isPassToken reports whether s is a token this server or its page could have
// made: 16 random bytes in unpadded base64url. Claim tokens, reissue links and
// collection nonces all take this form.
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

// passClaimDeadline is when an order started at now stops being collectable
// by its claim: a week on, rounded up to a UTC midnight, so the stored
// deadline dates a checkout to the day and not the microsecond.
func passClaimDeadline(now time.Time) time.Time {
	return dayCeil(now.Add(passClaimTTL))
}

// passClaimCookieUntil is how long a checkout started at now keeps the
// browser's claim cookie: as long as the longest-lived order under it
// (until), and at least as long as the new order could yet be held for a slow
// payment (holdPassOrder), which can happen only once its checkout is
// completed. Only a checkout sets the cookie, so a buyer who leaves before the
// hold and comes back when the bank debit clears must still have it.
func passClaimCookieUntil(now, until time.Time) time.Time {
	return maxTime(until, dayCeil(now.Add(stripeCheckoutTTL+passPendingPaymentTTL)))
}

// daysOff is how many whole days to take off a term ending at end so that
// none of it is left after now: the days from now to end, rounded up, so a
// refunded term stops at once rather than at midnight, and none for a term
// that has already ended.
func daysOff(end, now time.Time) int {
	if !end.After(now) {
		return 0
	}
	return termDays(now, end)
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
// for how long one lasts, how many posts an hour it skips the captcha for,
// its price and whether tax is added to it. Like /api/verify it looks up
// nothing about the caller's Pass: a browser knows its own Pass and when it
// runs out. The one thing it reads off the request is the claim cookie, which
// the page cannot see (it is HttpOnly), so the page knows there is a purchase
// to collect. The claim is looked up for that and nothing else: a cookie with
// nothing under it any more is no purchase to collect.
func (a *App) handlePassStatus(w http.ResponseWriter, r *http.Request) {
	if !a.passEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	out := map[string]any{"enabled": true, "days": a.cfg.Pass.Days, "hourly_posts": a.cfg.Pass.HourlyPosts}
	// With Stripe Tax on, the price shown is before tax, and the page says so.
	if a.cfg.Pass.StripeAutomaticTax {
		out["automatic_tax"] = true
	}
	if claim, ok := passClaimToken(r); ok {
		waiting, err := a.store.passClaimWaiting(r.Context(), hashToken(claim), time.Now())
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
		// The price is shown when Stripe could be asked for it. Without it the
		// page still sells: Stripe's own page always shows the price before
		// anyone pays.
		out["price"] = price
	}
	writeJSON(w, http.StatusOK, out)
}

// passPrice returns the cached price, reading it from Stripe when the cache is
// stale. A failure is cached for a minute, so an outage or a misconfigured
// price does not turn every page view into a call to Stripe.
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
	if s.priceErr != nil {
		a.log.Error("could not read the Pass price from Stripe", "error", s.priceErr)
	}
	return s.price, s.priceErr
}

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
// collect the Pass with. The answer is the Stripe page to go to. While sales
// are paused it refuses, for a new Pass and a renewal alike.
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

	now := time.Now()
	// The Pass to renew is read without the Pass lock, and with the same
	// grace limit as the order will be held to, so a code that is no good is
	// refused before it costs its holder a cooldown.
	var renewID *int64
	if strings.TrimSpace(req.Renew) != "" {
		code, ok := parsePassCode(req.Renew)
		if !ok {
			refusePass(w, passInvalid, "that is not a Pass code")
			return
		}
		id, expires, err := a.store.passByToken(r.Context(), a.passTokenHash(code))
		if errors.Is(err, errNotFound) || (err == nil && !now.Before(expires.Add(passExpiredGrace))) {
			refusePass(w, passInvalid, "this Pass is no longer valid, buy a new one instead")
			return
		}
		if err != nil {
			a.fail(w, r, err, "could not start checkout")
			return
		}
		renewID = &id
	}

	// The cooldown comes before anything that takes the Pass lock, so a burst
	// of refused checkouts queues no webhook or claim behind it.
	clientKey := clientIP(r, a.cfg.TrustedProxyHops)
	if !a.allowAction(w, "starting checkouts", []cooldown{
		{limiter: a.pass.checkoutLimiter, key: clientKey},
	}) {
		return
	}

	// Orders past their deadline, refunds past their month and Passes past
	// their grace month go here as well as in the sweep, so a busy server
	// keeps nothing past its time between two sweeps.
	if err := a.store.prunePasses(r.Context(), now); err != nil {
		a.log.Warn("could not prune Pass tables", "error", err)
	}

	// Every checkout gives the browser a new claim, and the orders under the
	// one it held move to it (createPassOrder): a Stripe tab opened earlier
	// and paid later is still collected by this browser, and a claim token
	// copied off this browser, or planted in it, before this checkout covers
	// none of what it buys.
	claim, err := randomToken()
	if err != nil {
		a.fail(w, r, err, "could not start checkout")
		return
	}
	var moveFrom []byte
	if old, ok := passClaimToken(r); ok {
		moveFrom = hashToken(old)
	}
	base := a.cfg.Pass.PublicURL
	session, err := a.pass.stripe.createCheckout(r.Context(), base+"/pass?paid", base+"/pass?cancelled")
	if err != nil {
		a.log.Error("could not create a Stripe checkout", "error", err)
		writeError(w, http.StatusBadGateway, "could not reach the payment page, try again in a moment")
		return
	}

	until, err := a.store.createPassOrder(r.Context(), passOrder{
		ClaimHash:   hashToken(claim),
		MoveFrom:    moveFrom,
		SessionHash: a.passSessionHash(session.ID),
		RenewPassID: renewID,
		Days:        a.cfg.Pass.Days,
		ExpiresAt:   passClaimDeadline(now),
	}, now)
	if errors.Is(err, errPassGone) {
		// Refunded, or past its grace month, between the lookup above and the
		// order. The Stripe session is left to expire unpaid.
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

	// The cookie outlives every order under it, and any hold one of them could
	// yet be given for a slow payment, which only a webhook can give and so
	// cannot extend the cookie itself (passClaimCookieUntil).
	a.setCookie(w, r, passClaimCookie, claim, passCookiePath, time.Until(passClaimCookieUntil(now, until)))
	writeJSON(w, http.StatusOK, map[string]any{"url": session.URL})
}

// passClaimToken reads the claim cookie, when it holds a token this server
// could have made.
func passClaimToken(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(passClaimCookie)
	if err != nil || !isPassToken(cookie.Value) {
		return "", false
	}
	return cookie.Value, true
}

// handlePassClaim hands what a browser paid for to that browser: every paid
// order under its claim. A code is shown here once: only its HMAC is kept, so
// a buyer who loses it asks the owner to reissue it. The code is derived from
// the nonce the page sends (passNonceCode), so asking again with the same
// nonce, after an answer that never arrived, is answered with the same code;
// the page drops the nonce once it has the code.
//
// Stripe sends the buyer back before or after its webhook arrives, whichever
// is faster, so "not paid yet" is an ordinary answer (202) for the page to
// retry, not an error. The answer says whether anything is still waiting
// under the claim ("claim"). A claim never sets or clears the cookie: only a
// checkout sets it, for long enough (passClaimCookieUntil), and only
// forgetting clears it, since an answer for a cookie the browser has since
// replaced, in another tab, would otherwise clear the new one.
func (a *App) handlePassClaim(w http.ResponseWriter, r *http.Request) {
	if !a.requirePass(w) || !passRequest(w, r) {
		return
	}
	var req passClaimBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Forget {
		a.forgetPassClaim(w, r)
		return
	}
	if !isPassToken(req.Nonce) {
		writeError(w, http.StatusBadRequest, "nonce: want the 16 random bytes the page draws for each collection, in base64url")
		return
	}
	completion := passCompletion{TokenHash: a.passTokenHash(a.passNonceCode(req.Nonce)), Now: time.Now()}
	if req.Reissue != "" {
		a.redeemPassReissue(w, r, req.Reissue, req.Nonce, completion)
		return
	}
	claim, ok := passClaimToken(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "no purchase in progress in this browser")
		return
	}
	res, err := a.store.claimPass(r.Context(), hashToken(claim), completion)
	if err != nil {
		a.fail(w, r, err, "could not collect the Pass")
		return
	}
	waiting := res.Waiting > 0
	if res.Renewed {
		a.log.Info("pass renewed")
	}
	switch {
	case res.Issued:
		if res.Made {
			a.log.Info("pass issued")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "issued",
			"pass":       formatPassCode(a.passNonceCode(req.Nonce)),
			"expires_at": res.IssuedExpiresAt,
			"claim":      waiting,
		})
	case res.Renewed:
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "renewed",
			"expires_at": res.RenewedExpiresAt,
			"claim":      waiting,
		})
	case waiting:
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending", "claim": true})
	default:
		writeError(w, http.StatusBadRequest, "no purchase in progress in this browser")
	}
}

type passClaimBody struct {
	// Nonce is what the page draws, 16 random bytes in base64url, for each
	// collection, and sends with every claim and reissue until one hands it a
	// code (passNonceCode).
	Nonce string `json:"nonce"`
	// Reissue is the token of a reissue link (handlePassReissue), which the
	// Pass page reads from its address's fragment. Without it, the claim is
	// the browser's cookie.
	Reissue string `json:"reissue"`
	// Forget drops the browser's claim instead (forgetPassClaim).
	Forget bool `json:"forget"`
}

// forgetPassClaim drops the browser's claim, for a shared computer: the
// orders under it move under a hash of nothing anybody holds, and the cookie
// is cleared, so whoever uses the browser next collects none of them. A paid
// order among them, or one paid later, is the buyer's still: the owner
// reissues it from the payment on the buyer's receipt, as for any paid order
// nobody collected.
func (a *App) forgetPassClaim(w http.ResponseWriter, r *http.Request) {
	if claim, ok := passClaimToken(r); ok {
		if err := a.store.dropPassClaim(r.Context(), hashToken(claim)); err != nil {
			a.fail(w, r, err, "could not forget the purchase in progress")
			return
		}
	}
	clearCookie(w, passClaimCookie, passCookiePath)
	writeJSON(w, http.StatusOK, map[string]any{"status": "forgotten", "claim": false})
}

// redeemPassReissue answers a claim that carries a reissue link's token, in
// the claim's own words: issued, with the new code shown once, or renewed,
// for an uncollected renewal. A link works once; asked again with the nonce
// that spent it, it is answered with the same code.
func (a *App) redeemPassReissue(w http.ResponseWriter, r *http.Request, token, nonce string, c passCompletion) {
	if !isPassToken(token) {
		refusePass(w, passInvalid, "this reissue link is not valid; ask for a new one")
		return
	}
	res, err := a.store.redeemPassReissue(r.Context(), hashToken(token), c)
	if err != nil {
		a.fail(w, r, err, "could not collect the Pass")
		return
	}
	// Whether this browser also has a purchase to collect, so the page goes
	// on to collect it.
	claim := false
	if cookie, ok := passClaimToken(r); ok {
		if claim, err = a.store.passClaimWaiting(r.Context(), hashToken(cookie), c.Now); err != nil {
			a.fail(w, r, err, "could not collect the Pass")
			return
		}
	}
	switch {
	case res.Issued:
		if res.Made {
			a.log.Info("pass reissued")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "issued",
			"pass":       formatPassCode(a.passNonceCode(nonce)),
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
		refusePass(w, passInvalid, "this reissue link has been used, has expired or was cancelled by a refund; ask for a new one")
	}
}

type passActivateBody struct {
	Pass string `json:"pass"`
}

// handlePassActivate checks a code typed in on a device, so the page can say
// whether it works and until when before the first post depends on it. It
// writes nothing: the browser keeps the code, as it keeps the credential.
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
	_, expires, err := a.store.passByToken(r.Context(), a.passTokenHash(code))
	switch {
	case errors.Is(err, errNotFound):
		refusePass(w, passInvalid, "this Pass is not valid; it may have run out more than a month ago, or been refunded or replaced")
		return
	case err != nil:
		a.fail(w, r, err, "could not read the Pass")
		return
	case !time.Now().Before(expires):
		refusePass(w, passExpired, "this Pass has run out; you can renew it on the Pass page")
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
// payment and an institution are not to meet in one. With sales switched off a
// presented Pass is ignored, so a deployment that stops selling does not stop
// its holders posting.
//
// A Pass that is presented and no good, because it was refunded, replaced or
// has run out, is treated as no Pass at all rather than refused. A refusal of
// its own would put a status in the request log, on the path that names the
// thread, that only a Pass can cause; that line, then the same thread's 201
// once the poster posts again, would tie the Pass's holder, whom the refund
// names to the operator in Stripe, to the post. Treated as absent, the post
// is held to the captcha like any other, and whatever it is refused for is
// what any post without a Pass can be refused for. The page hears that the
// Pass is no good from the answer's body (passGrant.Refused), which no log
// records.
//
// A good Pass changes nothing here: no cooldown is added or lifted, and a post
// that is refused after this, by flood control or anything else, costs its
// budget nothing. Only a failure to read the Pass is an error, and it is
// answered without logging the path, which names the thread.
func (a *App) postPass(w http.ResponseWriter, r *http.Request, badged bool) (passGrant, bool) {
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
	_, expires, err := a.store.passByToken(r.Context(), a.passTokenHash(code))
	switch {
	case errors.Is(err, errNotFound):
		return passGrant{Refused: passInvalid}, true
	case err != nil:
		if r.Context().Err() == nil {
			a.log.Error("could not read the Pass", "error", err)
		}
		writeError(w, http.StatusInternalServerError, "could not read the Pass")
		return passGrant{}, false
	case !time.Now().Before(expires):
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
// quarter past, in a zone that differs from UTC by part of an hour, India's
// for one; the README and the page say so. Sharing a Pass shares its budget:
// everyone using one code draws on the same count.
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

// passReissueRenewal answers a reissue asked for with a renewal's payment.
const passReissueRenewal = "that payment renewed a Pass; reissue it with the payment that bought the Pass, " +
	"or, if that one was refunded, with the earliest renewal that was not"

type passReissueBody struct {
	// PaymentIntent is Stripe's id for the payment ("pi_…"), which the owner
	// finds in the Stripe dashboard from the buyer's receipt.
	PaymentIntent string `json:"payment_intent"`
}

// handlePassReissue gives a buyer who lost their code a way back, behind the
// moderation key: a link the owner sends to the address the buyer wrote in
// from, which works once, until a deadline like an order's. Whoever opens it
// on the Pass page is given a new code for the Pass, and the old one stops
// working; for a purchase paid and never collected, it collects it.
//
// The owner never sees a code, so reading Stripe's records beside the Pass
// tables gives them nothing to post with. Only a Pass's origin, the payment
// that made it, or a paid order not collected yet, can be reissued: somebody
// who renewed another person's Pass, and then asks for a reissue with their
// own payment, would otherwise be handed that person's Pass. When the origin
// is refunded and the Pass goes on, the earliest payment left becomes its
// origin (revokePassPayment), so a Pass that is still paid for can always be
// reissued, by whoever paid for the time it has left.
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
	token, err := randomToken()
	if err != nil {
		a.fail(w, r, err, "could not make a reissue link")
		return
	}
	deadline := passClaimDeadline(time.Now())
	state, err := a.store.createPassReissue(r.Context(), a.passPaymentHash(pi), hashToken(token), deadline)
	if err != nil {
		a.fail(w, r, err, "could not make a reissue link")
		return
	}
	switch state {
	case reissueMissing:
		writeError(w, http.StatusNotFound, "no Pass was bought with that payment, or it was refunded")
		return
	case reissueRenewal:
		writeError(w, http.StatusConflict, passReissueRenewal)
		return
	}
	a.log.Info("pass reissue link made")
	// The token travels in the fragment, which a browser sends to no server
	// and puts in no Referer.
	writeJSON(w, http.StatusOK, map[string]any{
		"url":        a.cfg.Pass.PublicURL + "/pass#reissue=" + token,
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
		// that long, and its async_payment_succeeded event comes later and
		// lands here again.
		if ev.Type == stripeCheckoutCompleted && s.PaymentStatus == "unpaid" {
			until := dayCeil(time.Now().Add(passPendingPaymentTTL))
			if err := a.store.holdPassOrder(r.Context(), a.passSessionHash(s.ID), until); err != nil {
				a.fail(w, r, err, "could not record the checkout")
				return
			}
			break
		}
		if s.PaymentStatus != "paid" || s.PaymentIntent == nil || *s.PaymentIntent == "" {
			break
		}
		found, err := a.store.markPassOrderPaid(r.Context(), a.passSessionHash(s.ID), a.passPaymentHash(*s.PaymentIntent))
		if err != nil {
			a.fail(w, r, err, "could not record the payment")
			return
		}
		if !found {
			// Another integration's checkout, an unpaid order pruned after its
			// deadline, or a payment already refunded. Nothing here can tell
			// which, and none of them should become a Pass.
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
		// A partial refund is a goodwill gesture, not a cancellation; only a
		// full refund or a dispute ends the Pass.
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
// dropping it, collecting it, making and redeeming a reissue link, revoking a
// payment and pruning. They are therefore applied one at a time, in the order
// they took the lock, and each sees everything the ones before it committed.
// Pass purchases are rare, so the queue costs nothing, and it rules out every
// interleaving of a payment, a refund and a collection: no refund can miss a
// Pass being collected beside it, no "paid" can slip past a refund recorded at
// the same moment, and no two of them can deadlock over rows taken in
// different orders. Reading a Pass to post with
// it (passByToken) takes no lock, and no locked transaction makes a network
// call, so the lock is never held for longer than a few statements.
//
// The second key is the schema, so the Pass tables of two schemas in one
// database (a staging copy beside production, or the tests' schemas) are not
// queued behind each other.
const passLockID int32 = 0x50617373 // "Pass"

// passTx runs fn in a transaction holding the Pass lock.
func (s *Store) passTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext(current_schema()))`, passLockID); err != nil {
			return fmt.Errorf("take the Pass lock: %w", err)
		}
		return fn(tx)
	})
}

type passOrder struct {
	ClaimHash []byte
	// MoveFrom is the hash of the claim the browser held before this
	// checkout, if it held one: the orders under it move under ClaimHash.
	MoveFrom    []byte
	SessionHash []byte
	RenewPassID *int64
	// Days is the term the order buys, EDUCHAN_PASS_DAYS as it is now.
	Days      int
	ExpiresAt time.Time
}

// errPassGone is a renewal whose Pass was refunded or pruned between the
// checkout's lookup and its order.
var errPassGone = errors.New("the Pass to renew is gone")

// createPassOrder records a checkout's order under the browser's new claim,
// and moves the orders under the claim it held before to the new one. A
// renewal's Pass is looked up again under the lock, with the same grace limit
// as the checkout's own lookup, so a refund or a prune that landed in between,
// while Stripe was being asked for a session, answers errPassGone, and moves
// nothing, rather than failing on the foreign key. It returns the latest
// deadline of any order under the claim, which the browser's claim cookie
// must outlast.
func (s *Store) createPassOrder(ctx context.Context, o passOrder, now time.Time) (time.Time, error) {
	var until time.Time
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		if o.RenewPassID != nil {
			var expires time.Time
			err := tx.QueryRow(ctx, `SELECT expires_at FROM passes WHERE id = $1`, *o.RenewPassID).Scan(&expires)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !now.Before(expires.Add(passExpiredGrace))) {
				return errPassGone
			}
			if err != nil {
				return err
			}
		}
		if o.MoveFrom != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE pass_orders SET claim_hash = $2 WHERE claim_hash = $1`, o.MoveFrom, o.ClaimHash); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO pass_orders (claim_hash, session_hash, renew_pass_id, days, expires_at)
			VALUES ($1, $2, $3, $4, $5)`, o.ClaimHash, o.SessionHash, o.RenewPassID, o.Days, o.ExpiresAt)
		if isForeignKeyViolation(err) {
			return errPassGone
		}
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT max(expires_at) FROM pass_orders WHERE claim_hash = $1`, o.ClaimHash).Scan(&until)
	})
	return until, err
}

// foreignKeyViolation is the PostgreSQL SQLSTATE for a reference to a row
// that does not exist.
const foreignKeyViolation = "23503"

// isForeignKeyViolation reports whether err is a PostgreSQL foreign key
// failure.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation
}

// markPassOrderPaid records the payment behind an order. It is idempotent,
// since Stripe may deliver one event more than once. A payment already
// refunded is refused, since Stripe may deliver a refund before a retried
// "paid", and its order, which can never be paid now, is deleted, so the
// browser that started it stops waiting for it. An order past its claim
// deadline is still marked while it is kept, until the next prune: the money
// has been taken, and a paid order waits to be collected or reissued
// (prunePasses).
func (s *Store) markPassOrderPaid(ctx context.Context, sessionHash, paymentHash []byte) (bool, error) {
	var found bool
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		found = false
		revoked, err := paymentRevoked(ctx, tx, paymentHash)
		if err != nil {
			return err
		}
		if revoked {
			_, err := tx.Exec(ctx, `
				DELETE FROM pass_orders WHERE session_hash = $1 AND payment_hash IS NULL`, sessionHash)
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE pass_orders SET payment_hash = $2
			WHERE session_hash = $1
			  AND (payment_hash IS NULL OR payment_hash = $2)`,
			sessionHash, paymentHash)
		found = tag.RowsAffected() == 1
		return err
	})
	return found, err
}

// holdPassOrder keeps an unpaid order until at least until. A checkout
// completed with a payment that takes days to confirm, a bank debit say,
// would otherwise be pruned at its deadline before the money arrived.
func (s *Store) holdPassOrder(ctx context.Context, sessionHash []byte, until time.Time) error {
	return s.passTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE pass_orders SET expires_at = $2
			WHERE session_hash = $1 AND payment_hash IS NULL AND expires_at < $2`,
			sessionHash, until)
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

// passClaimWaiting reports whether a claim has anything to collect: a paid
// order, or an unpaid one still inside its deadline. It reads without the
// lock, as passByToken does.
func (s *Store) passClaimWaiting(ctx context.Context, claimHash []byte, now time.Time) (bool, error) {
	var waiting bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pass_orders
		               WHERE claim_hash = $1 AND (payment_hash IS NOT NULL OR expires_at > $2))`,
		claimHash, now).Scan(&waiting)
	return waiting, err
}

// dropPassClaim moves the orders under a claim under a random hash, which no
// token hashes to, so the claim collects none of them. They are otherwise
// left as they were: paid, or paid later, an order waits to be reissued from
// its payment like any order nobody collected, and unpaid it goes at its
// deadline.
func (s *Store) dropPassClaim(ctx context.Context, claimHash []byte) error {
	nobody := make([]byte, sha256.Size)
	if _, err := rand.Read(nobody); err != nil {
		return fmt.Errorf("drop the claim: %w", err)
	}
	return s.passTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE pass_orders SET claim_hash = $2 WHERE claim_hash = $1`, claimHash, nobody)
		return err
	})
}

// paymentRevoked reports whether a payment was refunded or disputed.
func paymentRevoked(ctx context.Context, tx pgx.Tx, paymentHash []byte) (bool, error) {
	var revoked bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pass_revocations WHERE payment_hash = $1)`, paymentHash).Scan(&revoked)
	return revoked, err
}

// passByToken returns the Pass a code hash belongs to, expired or not.
func (s *Store) passByToken(ctx context.Context, tokenHash []byte) (int64, time.Time, error) {
	var id int64
	var expires time.Time
	err := s.pool.QueryRow(ctx, `SELECT id, expires_at FROM passes WHERE token_hash = $1`, tokenHash).Scan(&id, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, errNotFound
	}
	return id, expires, err
}

// passCompletion is what handing a Pass over needs from the caller: the hash
// of the code the page's nonce gives (passNonceCode), and the time. The term
// each payment buys is its order's own.
type passCompletion struct {
	TokenHash []byte
	Now       time.Time
}

// claimResult is what collecting a claim, or redeeming a reissue link, came
// to.
type claimResult struct {
	// Issued is set when a Pass is under the code whose hash the
	// passCompletion carried: made now, or by an earlier request with the
	// same nonce whose answer never arrived (Made tells which).
	// IssuedExpiresAt is when it runs out, after any later payment under the
	// same claim renewed it.
	Issued          bool
	Made            bool
	IssuedExpiresAt time.Time
	// Renewed is set when a Pass the browser already held was extended, and
	// RenewedExpiresAt is its new expiry.
	Renewed          bool
	RenewedExpiresAt time.Time
	// Waiting counts the orders still unpaid under the claim.
	Waiting int
}

// passWithToken returns the Pass under a code hash and its expiry, or 0.
func passWithToken(ctx context.Context, tx pgx.Tx, tokenHash []byte) (int64, time.Time, error) {
	var id int64
	var expires time.Time
	err := tx.QueryRow(ctx, `SELECT id, expires_at FROM passes WHERE token_hash = $1`, tokenHash).Scan(&id, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	return id, expires, err
}

// claimPass collects every paid order under a claim, oldest deadline first.
// An order that renews a Pass renews it. The first that does not creates a
// Pass under the code the nonce gives, and any later one under the same
// claim, paid now or later, renews that new Pass, so a buyer who paid twice
// gets a longer Pass and never loses a payment. A renewal whose Pass is gone
// counts as one that does not renew. A Pass already under that code was made
// by an earlier claim with the same nonce, whose answer was lost: it stands
// in for the new one, and is answered again whether or not anything new was
// paid. Each payment adds its own order's term.
//
// An unpaid order past its deadline is deleted, as prunePasses would. A paid
// order is collected whatever its deadline: it was paid for.
func (s *Store) claimPass(ctx context.Context, claimHash []byte, c passCompletion) (claimResult, error) {
	var res claimResult
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		res = claimResult{}
		created, _, err := passWithToken(ctx, tx, c.TokenHash)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT session_hash, payment_hash, renew_pass_id, days, expires_at FROM pass_orders
			WHERE claim_hash = $1
			ORDER BY expires_at, session_hash`, claimHash)
		if err != nil {
			return err
		}
		type order struct {
			session, payment []byte
			renewID          *int64
			days             int
			expires          time.Time
		}
		var orders []order
		for rows.Next() {
			var o order
			if err := rows.Scan(&o.session, &o.payment, &o.renewID, &o.days, &o.expires); err != nil {
				return err
			}
			orders = append(orders, o)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for _, o := range orders {
			if o.payment == nil && c.Now.Before(o.expires) {
				res.Waiting++
				continue
			}
			if _, err := tx.Exec(ctx, `DELETE FROM pass_orders WHERE session_hash = $1`, o.session); err != nil {
				return err
			}
			if o.payment == nil {
				continue
			}
			// Revoking deletes the order, so this never holds while the lock
			// does its job; it is checked anyway, as it is everywhere a payment
			// turns into a Pass.
			if revoked, err := paymentRevoked(ctx, tx, o.payment); err != nil {
				return err
			} else if revoked {
				continue
			}
			if o.renewID != nil {
				expires, found, err := renewPass(ctx, tx, *o.renewID, o.payment, o.days, c.Now)
				if err != nil {
					return err
				}
				if found {
					if *o.renewID != created {
						res.Renewed, res.RenewedExpiresAt = true, expires
					}
					continue
				}
			}
			if created != 0 {
				if _, _, err := renewPass(ctx, tx, created, o.payment, o.days, c.Now); err != nil {
					return err
				}
				continue
			}
			if created, _, err = issuePass(ctx, tx, o.payment, c.TokenHash, o.days, c.Now); err != nil {
				return err
			}
			res.Made = true
		}
		if created == 0 {
			return nil
		}
		res.Issued = true
		if err := tx.QueryRow(ctx, `SELECT expires_at FROM passes WHERE id = $1`, created).Scan(&res.IssuedExpiresAt); err != nil {
			return err
		}
		if res.Waiting == 0 {
			return nil
		}
		// What is still unpaid under the claim, paid later, renews this Pass
		// rather than making a second one with a code this browser would be
		// shown in place of the first.
		_, err = tx.Exec(ctx, `
			UPDATE pass_orders SET renew_pass_id = $2
			WHERE claim_hash = $1 AND renew_pass_id IS NULL`, claimHash, created)
		return err
	})
	return res, err
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
		INSERT INTO pass_payments (payment_hash, pass_id, days, ends_at, origin) VALUES ($1, $2, $3, $4, true)`,
		payment, id, termDays(now, expires), expires)
	return id, expires, err
}

// renewPass extends a Pass by a term of days from its expiry, or from now if
// it has run out, and links the payment to it with the term it bought
// (termDays) and the day that term ends, which is what a refund of it reads.
// found is false when there is no such Pass.
func renewPass(ctx context.Context, tx pgx.Tx, passID int64, payment []byte, days int, now time.Time) (time.Time, bool, error) {
	var current time.Time
	err := tx.QueryRow(ctx, `SELECT expires_at FROM passes WHERE id = $1`, passID).Scan(&current)
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
	_, err = tx.Exec(ctx, `
		INSERT INTO pass_payments (payment_hash, pass_id, days, ends_at, origin) VALUES ($1, $2, $3, $4, false)`,
		payment, passID, termDays(base, expires), expires)
	return expires, true, err
}

// What asking for a reissue link came to.
type reissueState int

const (
	reissueMissing reissueState = iota // no Pass or paid order for the payment, or it was refunded
	reissueRenewal                     // the payment renewed a Pass; only its origin reissues it
	reissueReady                       // a link was made
)

// createPassReissue records a reissue link for a payment: a Pass's origin, or
// a paid order nobody collected. A newer link for the same payment replaces
// the one before it.
func (s *Store) createPassReissue(ctx context.Context, paymentHash, tokenHash []byte, deadline time.Time) (reissueState, error) {
	var state reissueState
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		state = reissueMissing
		if revoked, err := paymentRevoked(ctx, tx, paymentHash); err != nil || revoked {
			return err
		}
		var uncollected bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pass_orders WHERE payment_hash = $1)`, paymentHash).Scan(&uncollected); err != nil {
			return err
		}
		if !uncollected {
			var origin bool
			err := tx.QueryRow(ctx, `SELECT origin FROM pass_payments WHERE payment_hash = $1`, paymentHash).Scan(&origin)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return nil
			case err != nil:
				return err
			case !origin:
				state = reissueRenewal
				return nil
			}
		}
		state = reissueReady
		_, err := tx.Exec(ctx, `
			INSERT INTO pass_reissues (token_hash, payment_hash, expires_at) VALUES ($1, $2, $3)
			ON CONFLICT (payment_hash) DO UPDATE SET token_hash = EXCLUDED.token_hash, expires_at = EXCLUDED.expires_at`,
			tokenHash, paymentHash, deadline)
		return err
	})
	return state, err
}

// redeemPassReissue spends a reissue link. A paid order nobody collected is
// collected, as claimPass would: a renewal renews its Pass, and anything else
// makes a Pass under the code the nonce gives, for the order's own term.
// Otherwise the Pass the payment is the origin of is given that code, which
// is what stops the old one working. A link that is unknown, used, past its
// deadline or for a refunded payment gives nothing, unless a Pass is under
// that code already: the link was spent by an earlier request with the same
// nonce, whose answer never arrived, and it is answered again.
func (s *Store) redeemPassReissue(ctx context.Context, tokenHash []byte, c passCompletion) (claimResult, error) {
	var res claimResult
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		res = claimResult{}
		// Checked first, so a link still unspent is never asked to give a
		// second Pass the code the first one has.
		id, expires, err := passWithToken(ctx, tx, c.TokenHash)
		if err != nil || id != 0 {
			res.Issued, res.IssuedExpiresAt = id != 0, expires
			return err
		}
		var payment []byte
		err = tx.QueryRow(ctx, `
			DELETE FROM pass_reissues WHERE token_hash = $1 AND expires_at > $2
			RETURNING payment_hash`, tokenHash, c.Now).Scan(&payment)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// Revoking deletes the link, so this never holds while the lock does
		// its job; it is checked anyway, as it is everywhere a payment turns
		// into a Pass.
		if revoked, err := paymentRevoked(ctx, tx, payment); err != nil || revoked {
			return err
		}
		var renewID *int64
		var days int
		err = tx.QueryRow(ctx, `
			DELETE FROM pass_orders WHERE payment_hash = $1 RETURNING renew_pass_id, days`, payment).Scan(&renewID, &days)
		switch {
		case err == nil:
			if renewID != nil {
				expires, found, err := renewPass(ctx, tx, *renewID, payment, days, c.Now)
				if err != nil || found {
					res.Renewed, res.RenewedExpiresAt = found, expires
					return err
				}
			}
			_, res.IssuedExpiresAt, err = issuePass(ctx, tx, payment, c.TokenHash, days, c.Now)
			res.Issued, res.Made = err == nil, err == nil
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		err = tx.QueryRow(ctx, `
			UPDATE passes SET token_hash = $2
			WHERE id = (SELECT pass_id FROM pass_payments WHERE payment_hash = $1 AND origin)
			RETURNING expires_at`, payment, c.TokenHash).Scan(&res.IssuedExpiresAt)
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
// The payment is remembered for a month either way, so a late "paid" event
// for it cannot reopen what was undone.
func (s *Store) revokePassPayment(ctx context.Context, paymentHash []byte, now time.Time) (revocation, error) {
	var res revocation
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		res = revokedNothing
		tag, err := tx.Exec(ctx, `DELETE FROM pass_orders WHERE payment_hash = $1`, paymentHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			res = revokedOrder
		}
		if _, err := tx.Exec(ctx, `DELETE FROM pass_reissues WHERE payment_hash = $1`, paymentHash); err != nil {
			return err
		}
		var p revokedPayment
		err = tx.QueryRow(ctx, `
			DELETE FROM pass_payments WHERE payment_hash = $1 RETURNING pass_id, days, ends_at, origin`,
			paymentHash).Scan(&p.passID, &p.days, &p.ends, &p.origin)
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
	origin bool
}

// revokeTerm takes what is left of a revoked payment's term off its Pass:
// the days from now to the day that term ends, rounded up so it stops at once
// rather than at midnight, and never more than the term. A term already run
// takes off nothing. The terms paid after it move back by as much, since the
// Pass now reaches them sooner. A Pass left with no time to come is deleted,
// and its other payments with it. One that goes on, having lost its origin,
// makes the earliest payment left its origin, so it can still be reissued
// (createPassReissue): a renewal is bought only by somebody holding the code,
// through checkout, or by the browser that bought the Pass, through its
// claim, so this gives nobody a way in who did not have one.
func revokeTerm(ctx context.Context, tx pgx.Tx, p revokedPayment, now time.Time) (revocation, error) {
	var expires time.Time
	if err := tx.QueryRow(ctx, `SELECT expires_at FROM passes WHERE id = $1`, p.passID).Scan(&expires); err != nil {
		return revokedNothing, err
	}
	off := min(p.days, daysOff(p.ends, now))
	expires = expires.UTC().AddDate(0, 0, -off)
	if !expires.After(now) {
		_, err := tx.Exec(ctx, `DELETE FROM passes WHERE id = $1`, p.passID)
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
	if p.origin {
		if _, err := tx.Exec(ctx, `
			UPDATE pass_payments SET origin = true
			WHERE payment_hash = (SELECT payment_hash FROM pass_payments WHERE pass_id = $1
			                      ORDER BY ends_at, payment_hash LIMIT 1)`, p.passID); err != nil {
			return revokedNothing, err
		}
	}
	return revokedTerm, nil
}

// prunePasses deletes what has outlived its use: unpaid orders past their
// deadline; paid orders nobody collected, once a Pass bought with one at its
// deadline would have run out too (the deadline plus the order's own term);
// reissue links past their deadline; remembered refunds past their month; and
// Passes a month past expiry, which can no longer be renewed and are bought
// anew instead. Checkout runs it, and so does the daily sweep, which needs no
// Pass setting for it: every order carries its own term.
func (s *Store) prunePasses(ctx context.Context, now time.Time) error {
	return s.passTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			WITH o AS (
			       DELETE FROM pass_orders
			       WHERE CASE WHEN payment_hash IS NULL THEN expires_at
			                  ELSE expires_at + make_interval(hours => 24 * days) END <= $1),
			     r AS (DELETE FROM pass_revocations WHERE expires_at <= $1),
			     i AS (DELETE FROM pass_reissues WHERE expires_at <= $1)
			DELETE FROM passes WHERE expires_at <= $2`,
			now, now.Add(-passExpiredGrace))
		return err
	})
}
