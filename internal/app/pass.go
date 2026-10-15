package app

import (
	"context"
	"crypto/rand"
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
//     refund or a chargeback cancel the Pass and does nothing else.
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

// newPassCode draws a code. 256 is a multiple of 32, so masking a random byte
// to five bits picks every character with equal probability.
func newPassCode() (string, error) {
	buf := make([]byte, passCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate pass code: %w", err)
	}
	out := make([]byte, passCodeLen)
	for i, b := range buf {
		out[i] = passAlphabet[b&31]
	}
	return string(out), nil
}

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
// to a midnight is counted in, so taking the term off again for a refund
// leaves the Pass where it would have been without that payment, or earlier,
// never later: a refunded purchase stops at once rather than at midnight.
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

// passTermDays is the term a Pass is sold for. A Config built without the Pass
// switched on has none, and the sweeper still prunes with the default.
func (a *App) passTermDays() int {
	if a.cfg.Pass.Days >= 1 {
		return a.cfg.Pass.Days
	}
	return defaultPassDays
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

// handlePassStatus tells the Pass page whether the Pass is on sale, for how
// long one lasts, how many posts an hour it skips the captcha for, and its
// price. Like /api/verify it looks up nothing about the caller: a browser
// knows its own Pass and when it runs out. The one thing it reads off the
// request is whether the browser holds a claim cookie, which the page cannot
// see (it is HttpOnly), so the page knows there is a purchase to collect. The
// claim itself is not looked up.
func (a *App) handlePassStatus(w http.ResponseWriter, r *http.Request) {
	if !a.passEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	out := map[string]any{"enabled": true, "days": a.cfg.Pass.Days, "hourly_posts": a.cfg.Pass.HourlyPosts}
	if _, ok := passClaimToken(r); ok {
		out["claim"] = true
	}
	// The price is shown when Stripe could be asked for it. Without it the page
	// still sells: Stripe's own page always shows the price before anyone pays.
	if price, err := a.passPrice(r.Context()); err == nil {
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
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
// collect the Pass with. The answer is the Stripe page to go to.
func (a *App) handlePassCheckout(w http.ResponseWriter, r *http.Request) {
	if !a.requirePass(w) {
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
	// Orders past their deadline, refunds past their month and Passes past
	// their grace month go first, so a Pass too old to renew is gone before it
	// is looked up. The sweeper prunes too, so a quiet server keeps nothing
	// past its time either.
	if err := a.store.prunePasses(r.Context(), now, a.passTermDays()); err != nil {
		a.log.Warn("could not prune Pass tables", "error", err)
	}

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

	clientKey := clientIP(r, a.cfg.TrustedProxyHops)
	if !a.allowAction(w, "starting checkouts", []cooldown{
		{limiter: a.pass.checkoutLimiter, key: clientKey},
	}) {
		return
	}

	// A browser already holding a claim keeps it, and the new order joins the
	// ones under it: a Stripe tab opened earlier and paid later is collected
	// with this one, rather than orphaned by a new cookie.
	claim, ok := passClaimToken(r)
	if !ok {
		var err error
		if claim, err = randomToken(); err != nil {
			a.fail(w, r, err, "could not start checkout")
			return
		}
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
		SessionHash: a.passSessionHash(session.ID),
		RenewPassID: renewID,
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

	// The cookie lasts as long as the longest-lived order under it.
	a.setCookie(w, r, passClaimCookie, claim, passCookiePath, time.Until(until))
	writeJSON(w, http.StatusOK, map[string]any{"url": session.URL})
}

// passClaimToken reads the claim cookie, when it holds a token this server
// could have made: 16 random bytes in unpadded base64url.
func passClaimToken(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(passClaimCookie)
	if err != nil {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(raw) != 16 {
		return "", false
	}
	return cookie.Value, true
}

// handlePassClaim hands what a browser paid for to that browser: every paid
// order under its claim. A code is shown here once and never again: only its
// HMAC is kept, so a buyer who loses it asks the owner to reissue it.
//
// Stripe sends the buyer back before or after its webhook arrives, whichever
// is faster, so "not paid yet" is an ordinary answer (202) for the page to
// retry, not an error. The answer says whether anything is still waiting
// under the claim ("claim"), and the cookie is cleared once nothing is.
func (a *App) handlePassClaim(w http.ResponseWriter, r *http.Request) {
	if !a.requirePass(w) {
		return
	}
	var req passClaimBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reissue != "" {
		a.redeemPassReissue(w, r, req.Reissue)
		return
	}
	claim, ok := passClaimToken(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "no purchase in progress in this browser")
		return
	}
	code, err := newPassCode()
	if err != nil {
		a.fail(w, r, err, "could not collect the Pass")
		return
	}
	res, err := a.store.claimPass(r.Context(), hashToken(claim), passCompletion{
		TokenHash: a.passTokenHash(code),
		Now:       time.Now(),
		Days:      a.cfg.Pass.Days,
	})
	if err != nil {
		a.fail(w, r, err, "could not collect the Pass")
		return
	}
	// The cookie goes once nothing is left under it, and otherwise lasts as
	// long as the order that waits longest, which a slow payment can extend
	// (holdPassOrder).
	waiting := res.Waiting > 0
	if waiting {
		a.setCookie(w, r, passClaimCookie, claim, passCookiePath, time.Until(res.WaitUntil))
	} else {
		clearCookie(w, passClaimCookie, passCookiePath)
	}
	if res.Renewed {
		a.log.Info("pass renewed")
	}
	switch {
	case res.Issued:
		a.log.Info("pass issued")
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "issued",
			"pass":       formatPassCode(code),
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
	// Reissue is the token of a reissue link (handlePassReissue), which the
	// Pass page reads from its address's fragment. Without it, the claim is
	// the browser's cookie.
	Reissue string `json:"reissue"`
}

// redeemPassReissue answers a claim that carries a reissue link's token, in
// the claim's own words: issued, with the new code shown once, or renewed,
// for an uncollected renewal. A link works once.
func (a *App) redeemPassReissue(w http.ResponseWriter, r *http.Request, token string) {
	if raw, err := base64.RawURLEncoding.DecodeString(token); err != nil || len(raw) != 16 {
		refusePass(w, passInvalid, "this reissue link is not valid; ask for a new one")
		return
	}
	code, err := newPassCode()
	if err != nil {
		a.fail(w, r, err, "could not collect the Pass")
		return
	}
	res, err := a.store.redeemPassReissue(r.Context(), hashToken(token), passCompletion{
		TokenHash: a.passTokenHash(code),
		Now:       time.Now(),
		Days:      a.cfg.Pass.Days,
	})
	if err != nil {
		a.fail(w, r, err, "could not collect the Pass")
		return
	}
	_, claim := passClaimToken(r)
	switch {
	case res.Issued:
		a.log.Info("pass reissued")
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
	if !a.requirePass(w) {
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
	expires, ok := a.checkPass(w, r, code)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pass": formatPassCode(code), "expires_at": expires})
}

// checkPass looks a code up and refuses it, writing the response, when it is
// unknown or expired. It returns the expiry of a Pass that is good.
func (a *App) checkPass(w http.ResponseWriter, r *http.Request, code string) (time.Time, bool) {
	_, expires, err := a.store.passByToken(r.Context(), a.passTokenHash(code))
	switch {
	case errors.Is(err, errNotFound):
		refusePass(w, passInvalid, "this Pass is not valid; it may have been refunded or replaced")
		return time.Time{}, false
	case err != nil:
		a.fail(w, r, err, "could not read the Pass")
		return time.Time{}, false
	case !time.Now().Before(expires):
		refusePass(w, passExpired, "this Pass has expired; renew it on the Pass page")
		return time.Time{}, false
	}
	return expires, true
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
}

// postPass resolves the Pass a post presents. A post made without one gets
// the zero passGrant.
//
// A badged post never reads the header: it is the credential's request, and a
// payment and an institution are not to meet in one. With sales switched off a
// presented Pass is ignored rather than refused, so a deployment that stops
// selling does not stop its holders posting.
//
// A Pass that is presented and no good is refused rather than ignored, the way
// a badge is: the poster believes they hold one, and the page needs to hear
// that they do not, so it can forget the code and say why.
//
// A good Pass changes nothing here: no cooldown is added or lifted, and a post
// that is refused after this, by flood control or anything else, costs its
// budget nothing.
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
		refusePass(w, passInvalid, "this Pass is not valid; it may have been refunded or replaced")
		return passGrant{}, false
	}
	if _, ok := a.checkPass(w, r, code); !ok {
		return passGrant{}, false
	}
	return passGrant{Valid: true, BudgetKey: a.passBudgetKey(code)}, true
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

// passBudget counts the posts each Pass has made in the current clock hour, so
// that it skips the captcha for at most HourlyPosts of them. Sharing a Pass
// shares its budget: everyone using one code draws on the same count.
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
// tables gives them nothing to post with. Only the payment that made a Pass,
// or a paid order not collected yet, can be reissued: somebody who renewed
// another person's Pass, and then asks for a reissue with their own payment,
// would otherwise be handed that person's Pass.
func (a *App) handlePassReissue(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeMod(w, r) {
		return
	}
	if !a.requirePass(w) {
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
		writeError(w, http.StatusConflict, "that payment renewed a Pass; reissue it with the payment that bought the Pass")
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
			a.log.Info("pass term taken off by a refund or dispute")
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
	ClaimHash   []byte
	SessionHash []byte
	RenewPassID *int64
	ExpiresAt   time.Time
}

// errPassGone is a renewal whose Pass was refunded or pruned between the
// checkout's lookup and its order.
var errPassGone = errors.New("the Pass to renew is gone")

// createPassOrder records a checkout's order. A renewal's Pass is looked up
// again under the lock, with the same grace limit as the checkout's own
// lookup, so a refund or a prune that landed in between, while Stripe was
// being asked for a session, answers errPassGone rather than a foreign-key
// failure. It returns the latest deadline of any order under the claim, which
// is how long the browser's claim cookie needs to last.
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
		_, err := tx.Exec(ctx, `
			INSERT INTO pass_orders (claim_hash, session_hash, renew_pass_id, expires_at)
			VALUES ($1, $2, $3, $4)`, o.ClaimHash, o.SessionHash, o.RenewPassID, o.ExpiresAt)
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
// since Stripe may deliver one event more than once, and it refuses a payment
// already refunded, since Stripe may deliver a refund before a retried "paid".
// An order past its claim deadline is still marked: the money has been taken,
// and a paid order waits to be collected or reissued (prunePasses).
func (s *Store) markPassOrderPaid(ctx context.Context, sessionHash, paymentHash []byte) (bool, error) {
	var found bool
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		revoked, err := paymentRevoked(ctx, tx, paymentHash)
		if err != nil || revoked {
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

// passCompletion is what turning a paid order into a Pass needs from the
// caller: the hash of a freshly drawn code, the time, and the term a payment
// buys.
type passCompletion struct {
	TokenHash []byte
	Now       time.Time
	Days      int
}

// claimResult is what collecting a claim came to.
type claimResult struct {
	// Issued is set when a new Pass was made, under the code whose hash the
	// passCompletion carried. IssuedExpiresAt is when it runs out, after any
	// later payment under the same claim renewed it.
	Issued          bool
	IssuedExpiresAt time.Time
	// Renewed is set when a Pass the browser already held was extended, and
	// RenewedExpiresAt is its new expiry.
	Renewed          bool
	RenewedExpiresAt time.Time
	// Waiting counts the orders still unpaid under the claim, and WaitUntil
	// is the latest of their deadlines.
	Waiting   int
	WaitUntil time.Time
}

// claimPass collects every paid order under a claim, oldest deadline first.
// An order that renews a Pass renews it. The first that does not creates a
// Pass with the drawn code, and any later one under the same claim, paid now
// or later, renews that new Pass, so a buyer who paid twice gets a longer Pass
// and never loses a payment. A renewal whose Pass is gone counts as one that
// does not renew.
//
// An unpaid order past its deadline is deleted, as prunePasses would. A paid
// order is collected whatever its deadline: it was paid for.
func (s *Store) claimPass(ctx context.Context, claimHash []byte, c passCompletion) (claimResult, error) {
	var res claimResult
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		res = claimResult{}
		rows, err := tx.Query(ctx, `
			SELECT session_hash, payment_hash, renew_pass_id, expires_at FROM pass_orders
			WHERE claim_hash = $1
			ORDER BY expires_at, session_hash`, claimHash)
		if err != nil {
			return err
		}
		type order struct {
			session, payment []byte
			renewID          *int64
			expires          time.Time
		}
		var orders []order
		for rows.Next() {
			var o order
			if err := rows.Scan(&o.session, &o.payment, &o.renewID, &o.expires); err != nil {
				return err
			}
			orders = append(orders, o)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		var created int64
		for _, o := range orders {
			if o.payment == nil && c.Now.Before(o.expires) {
				res.Waiting++
				res.WaitUntil = maxTime(res.WaitUntil, o.expires)
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
				expires, found, err := renewPass(ctx, tx, *o.renewID, o.payment, c)
				if err != nil {
					return err
				}
				if found {
					if *o.renewID == created {
						res.IssuedExpiresAt = expires
					} else {
						res.Renewed, res.RenewedExpiresAt = true, expires
					}
					continue
				}
			}
			if created != 0 {
				if res.IssuedExpiresAt, _, err = renewPass(ctx, tx, created, o.payment, c); err != nil {
					return err
				}
				continue
			}
			if created, res.IssuedExpiresAt, err = issuePass(ctx, tx, o.payment, c); err != nil {
				return err
			}
			res.Issued = true
		}
		if created == 0 || res.Waiting == 0 {
			return nil
		}
		// What is still unpaid under the claim, paid later, renews the Pass
		// just made rather than making a second one with a code this browser
		// would be shown in place of the first.
		_, err = tx.Exec(ctx, `
			UPDATE pass_orders SET renew_pass_id = $2
			WHERE claim_hash = $1 AND renew_pass_id IS NULL`, claimHash, created)
		return err
	})
	return res, err
}

// issuePass makes a new Pass for a payment, under the code whose hash c
// carries, for a full term from now, and links the payment to it as its
// origin.
func issuePass(ctx context.Context, tx pgx.Tx, payment []byte, c passCompletion) (int64, time.Time, error) {
	expires := passExpiry(c.Now, c.Days)
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO passes (token_hash, expires_at) VALUES ($1, $2) RETURNING id`,
		c.TokenHash, expires).Scan(&id); err != nil {
		return 0, time.Time{}, err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO pass_payments (payment_hash, pass_id, days, origin) VALUES ($1, $2, $3, true)`,
		payment, id, termDays(c.Now, expires))
	return id, expires, err
}

// renewPass extends a Pass by a full term from its expiry, or from now if it
// has run out, and links the payment to it with the term it bought
// (termDays), which is what a refund of it takes off again. found is false
// when there is no such Pass.
func renewPass(ctx context.Context, tx pgx.Tx, passID int64, payment []byte, c passCompletion) (time.Time, bool, error) {
	var current time.Time
	err := tx.QueryRow(ctx, `SELECT expires_at FROM passes WHERE id = $1`, passID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	base := maxTime(current, c.Now)
	expires := passExpiry(base, c.Days)
	if _, err := tx.Exec(ctx, `UPDATE passes SET expires_at = $2 WHERE id = $1`, passID, expires); err != nil {
		return time.Time{}, false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO pass_payments (payment_hash, pass_id, days, origin) VALUES ($1, $2, $3, false)`,
		payment, passID, termDays(base, expires))
	return expires, true, err
}

// What asking for a reissue link came to.
type reissueState int

const (
	reissueMissing reissueState = iota // no Pass or paid order for the payment, or it was refunded
	reissueRenewal                     // the payment renewed a Pass; only the one that bought it reissues it
	reissueReady                       // a link was made
)

// createPassReissue records a reissue link for a payment: the payment that
// made a Pass, or a paid order nobody collected. A newer link for the same
// payment replaces the one before it.
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
// makes a Pass under the drawn code. Otherwise the Pass the payment made is
// given the drawn code, which is what stops the old one working. A link that
// is unknown, used, past its deadline or for a refunded payment gives nothing.
func (s *Store) redeemPassReissue(ctx context.Context, tokenHash []byte, c passCompletion) (claimResult, error) {
	var res claimResult
	err := s.passTx(ctx, func(tx pgx.Tx) error {
		res = claimResult{}
		var payment []byte
		err := tx.QueryRow(ctx, `
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
		err = tx.QueryRow(ctx, `
			DELETE FROM pass_orders WHERE payment_hash = $1 RETURNING renew_pass_id`, payment).Scan(&renewID)
		switch {
		case err == nil:
			if renewID != nil {
				expires, found, err := renewPass(ctx, tx, *renewID, payment, c)
				if err != nil || found {
					res.Renewed, res.RenewedExpiresAt = found, expires
					return err
				}
			}
			_, res.IssuedExpiresAt, err = issuePass(ctx, tx, payment, c)
			res.Issued = err == nil
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
		res.Issued = err == nil
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
	revokedTerm                      // the payment's term, from a Pass that goes on
	revokedPass                      // the Pass, whose last term it was
)

// revokePassPayment undoes what a refunded or disputed payment bought: the
// order still waiting to be collected, or the term it added to a Pass. A Pass
// left with nothing of its time to come, because that was its only term or
// the terms after it were refunded too, is deleted. A refund of one renewal
// therefore costs the holder that renewal and nothing else, and somebody who
// renews another person's Pass and disputes the payment takes back only what
// they gave.
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
		var passID int64
		var days int
		err = tx.QueryRow(ctx, `
			DELETE FROM pass_payments WHERE payment_hash = $1 RETURNING pass_id, days`,
			paymentHash).Scan(&passID, &days)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			var expires time.Time
			if err := tx.QueryRow(ctx, `SELECT expires_at FROM passes WHERE id = $1`, passID).Scan(&expires); err != nil {
				return err
			}
			expires = expires.UTC().AddDate(0, 0, -days)
			if expires.After(now) {
				res = revokedTerm
				_, err = tx.Exec(ctx, `UPDATE passes SET expires_at = $2 WHERE id = $1`, passID, expires)
			} else {
				res = revokedPass
				_, err = tx.Exec(ctx, `DELETE FROM passes WHERE id = $1`, passID)
			}
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO pass_revocations (payment_hash, expires_at) VALUES ($1, $2)
			ON CONFLICT (payment_hash) DO UPDATE SET expires_at = EXCLUDED.expires_at`,
			paymentHash, now.Add(passRevocationTTL))
		return err
	})
	return res, err
}

// prunePasses deletes what has outlived its use: unpaid orders past their
// deadline; paid orders nobody collected, once a Pass bought with one at its
// deadline would have run out too (deadline plus a term of days); reissue
// links past their deadline; remembered refunds past their month; and Passes
// a month past expiry, which can no longer be renewed and are bought anew
// instead. Checkout runs it, and so does
// the daily sweep, so a quiet server keeps nothing past its time either.
func (s *Store) prunePasses(ctx context.Context, now time.Time, days int) error {
	return s.passTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			WITH o AS (
			       DELETE FROM pass_orders
			       WHERE expires_at <= CASE WHEN payment_hash IS NULL THEN $1::timestamptz ELSE $3::timestamptz END),
			     r AS (DELETE FROM pass_revocations WHERE expires_at <= $1),
			     i AS (DELETE FROM pass_reissues WHERE expires_at <= $1)
			DELETE FROM passes WHERE expires_at <= $2`,
			now, now.Add(-passExpiredGrace), now.UTC().AddDate(0, 0, -days))
		return err
	})
}
