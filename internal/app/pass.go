package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
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
	// browser that bought it. A week covers a buyer who closes the tab before
	// Stripe sends them back; past that, the owner can reissue it.
	passClaimTTL = 7 * 24 * time.Hour
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

// passExpiry is when a Pass bought or renewed at from runs out: Days later,
// rounded up to a UTC day boundary like a credential's, so the stored expiry
// dates the purchase to the day and not the minute.
func (a *App) passExpiry(from time.Time) time.Time {
	return nextDayBoundary(from.AddDate(0, 0, a.cfg.Pass.Days))
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
// price. Like /api/verify it looks at nothing the caller holds: a browser
// knows its own Pass and when it runs out.
func (a *App) handlePassStatus(w http.ResponseWriter, r *http.Request) {
	if !a.passEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	out := map[string]any{"enabled": true, "days": a.cfg.Pass.Days, "hourly_posts": a.cfg.Pass.HourlyPosts}
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

	var renewID *int64
	if strings.TrimSpace(req.Renew) != "" {
		code, ok := parsePassCode(req.Renew)
		if !ok {
			refusePass(w, passInvalid, "that is not a Pass code")
			return
		}
		id, _, err := a.store.passByToken(r.Context(), a.passTokenHash(code))
		if errors.Is(err, errNotFound) {
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

	claim, err := randomToken()
	if err != nil {
		a.fail(w, r, err, "could not start checkout")
		return
	}
	base := a.cfg.Pass.PublicURL
	session, err := a.pass.stripe.createCheckout(r.Context(), base+"/pass?paid", base+"/pass?cancelled")
	if err != nil {
		a.log.Error("could not create a Stripe checkout", "error", err)
		writeError(w, http.StatusBadGateway, "could not reach the payment page, try again in a moment")
		return
	}

	now := time.Now()
	// Expired orders, revocations and long-expired Passes go here, on the one
	// path that adds rows, so the tables stay small without a job of their own.
	if err := a.store.prunePasses(r.Context(), now); err != nil {
		a.log.Warn("could not prune Pass tables", "error", err)
	}
	if err := a.store.createPassOrder(r.Context(), passOrder{
		ClaimHash:   hashToken(claim),
		SessionHash: a.passSessionHash(session.ID),
		RenewPassID: renewID,
		ExpiresAt:   now.Add(passClaimTTL),
	}); err != nil {
		a.fail(w, r, err, "could not start checkout")
		return
	}

	a.setCookie(w, r, passClaimCookie, claim, passCookiePath, passClaimTTL)
	writeJSON(w, http.StatusOK, map[string]any{"url": session.URL})
}

// handlePassClaim hands a paid-for Pass to the browser that bought it. The
// code is shown here once and never again: only its HMAC is kept, so a buyer
// who loses it asks the owner to reissue it.
//
// Stripe sends the buyer back before or after its webhook arrives, whichever
// is faster, so "not paid yet" is an ordinary answer (202) for the page to
// retry, not an error.
func (a *App) handlePassClaim(w http.ResponseWriter, r *http.Request) {
	if !a.requirePass(w) {
		return
	}
	cookie, err := r.Cookie(passClaimCookie)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusBadRequest, "no purchase in progress in this browser")
		return
	}
	code, err := newPassCode()
	if err != nil {
		a.fail(w, r, err, "could not collect the Pass")
		return
	}
	res, err := a.store.claimPass(r.Context(), hashToken(cookie.Value), passCompletion{
		TokenHash: a.passTokenHash(code),
		Now:       time.Now(),
		Expiry:    a.passExpiry,
	})
	if err != nil {
		a.fail(w, r, err, "could not collect the Pass")
		return
	}
	switch res.State {
	case orderMissing:
		clearCookie(w, passClaimCookie, passCookiePath)
		writeError(w, http.StatusBadRequest, "no purchase in progress in this browser")
	case orderUnpaid:
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
	case orderRenewed:
		clearCookie(w, passClaimCookie, passCookiePath)
		a.log.Info("pass renewed")
		writeJSON(w, http.StatusOK, map[string]any{"status": "renewed", "expires_at": res.ExpiresAt})
	case orderIssued:
		clearCookie(w, passClaimCookie, passCookiePath)
		a.log.Info("pass issued")
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "issued",
			"pass":       formatPassCode(code),
			"expires_at": res.ExpiresAt,
		})
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

// handlePassReissue gives a buyer who lost their code a new one, behind the
// moderation key. It works for a Pass already collected, whose old code stops
// working, and for one paid for and never collected. The owner passes the new
// code on by whatever channel the buyer wrote in from.
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
	code, err := newPassCode()
	if err != nil {
		a.fail(w, r, err, "could not reissue the Pass")
		return
	}
	res, err := a.store.reissuePass(r.Context(), a.passPaymentHash(pi), passCompletion{
		TokenHash: a.passTokenHash(code),
		Now:       time.Now(),
		Expiry:    a.passExpiry,
	})
	if err != nil {
		a.fail(w, r, err, "could not reissue the Pass")
		return
	}
	if res.State != orderIssued {
		writeError(w, http.StatusNotFound, "no Pass was bought with that payment, or it was refunded")
		return
	}
	a.log.Info("pass reissued")
	writeJSON(w, http.StatusOK, map[string]any{"pass": formatPassCode(code), "expires_at": res.ExpiresAt})
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
		// A completed session can still be waiting on a slow payment method;
		// its async_payment_succeeded event comes later and lands here again.
		if s.Mode != "payment" || s.PaymentStatus != "paid" || s.PaymentIntent == nil || *s.PaymentIntent == "" {
			break
		}
		found, err := a.store.markPassOrderPaid(r.Context(), a.passSessionHash(s.ID), a.passPaymentHash(*s.PaymentIntent))
		if err != nil {
			a.fail(w, r, err, "could not record the payment")
			return
		}
		if !found {
			// Another integration's checkout, an order past its week, or a
			// payment already refunded. Nothing here can tell which, and none
			// of them should become a Pass.
			a.log.Warn("a paid Stripe checkout matched no open Pass order")
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
		cancelled, err := a.store.revokePassPayment(r.Context(), a.passPaymentHash(*p.PaymentIntent), time.Now())
		if err != nil {
			a.fail(w, r, err, "could not record the refund")
			return
		}
		if cancelled {
			a.log.Info("pass cancelled by a refund or dispute")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"received": true})
}

/* Storage ------------------------------------------------------------------- */

type passOrder struct {
	ClaimHash   []byte
	SessionHash []byte
	RenewPassID *int64
	ExpiresAt   time.Time
}

func (s *Store) createPassOrder(ctx context.Context, o passOrder) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pass_orders (claim_hash, session_hash, renew_pass_id, expires_at)
		VALUES ($1, $2, $3, $4)`, o.ClaimHash, o.SessionHash, o.RenewPassID, o.ExpiresAt)
	return err
}

// markPassOrderPaid records the payment behind an order. It is idempotent,
// since Stripe may deliver one event more than once, and it refuses a payment
// already refunded, since Stripe may deliver a refund before a retried "paid".
func (s *Store) markPassOrderPaid(ctx context.Context, sessionHash, paymentHash []byte) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE pass_orders SET payment_hash = $2
		WHERE session_hash = $1
		  AND expires_at > now()
		  AND (payment_hash IS NULL OR payment_hash = $2)
		  AND NOT EXISTS (SELECT 1 FROM pass_revocations WHERE payment_hash = $2)`,
		sessionHash, paymentHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
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

// What became of an order on collection.
type orderState int

const (
	orderMissing orderState = iota // no such order, or it is past its week
	orderUnpaid                    // Stripe has not reported it paid yet
	orderIssued                    // a Pass code was issued: new, or replacing one
	orderRenewed                   // the existing Pass was extended
)

type orderResult struct {
	State     orderState
	ExpiresAt time.Time
}

// passCompletion is what turning a paid order into a Pass needs from the
// caller: the hash of a freshly drawn code, the time, and the expiry rule.
type passCompletion struct {
	TokenHash []byte
	Now       time.Time
	Expiry    func(from time.Time) time.Time
}

// claimPass collects the order a claim belongs to.
func (s *Store) claimPass(ctx context.Context, claimHash []byte, c passCompletion) (orderResult, error) {
	var res orderResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var payment []byte
		var renewID *int64
		var expires time.Time
		err := tx.QueryRow(ctx, `
			SELECT payment_hash, renew_pass_id, expires_at FROM pass_orders
			WHERE claim_hash = $1 FOR UPDATE`, claimHash).Scan(&payment, &renewID, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			res.State = orderMissing
			return nil
		}
		if err != nil {
			return err
		}
		if !c.Now.Before(expires) {
			res.State = orderMissing
			_, err := tx.Exec(ctx, `DELETE FROM pass_orders WHERE claim_hash = $1`, claimHash)
			return err
		}
		if payment == nil {
			res.State = orderUnpaid
			return nil
		}
		if _, err := tx.Exec(ctx, `DELETE FROM pass_orders WHERE claim_hash = $1`, claimHash); err != nil {
			return err
		}
		res, err = completePassOrder(ctx, tx, payment, renewID, c, false)
		return err
	})
	return res, err
}

// reissuePass gives the Pass a payment bought a new code: the Pass itself
// when it was collected, or the order when it was paid for and never was.
func (s *Store) reissuePass(ctx context.Context, paymentHash []byte, c passCompletion) (orderResult, error) {
	var res orderResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var renewID *int64
		err := tx.QueryRow(ctx, `
			DELETE FROM pass_orders WHERE payment_hash = $1
			RETURNING renew_pass_id`, paymentHash).Scan(&renewID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		uncollected := err == nil
		if !uncollected {
			var known bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM pass_payments WHERE payment_hash = $1)`, paymentHash).Scan(&known); err != nil {
				return err
			}
			if !known {
				res.State = orderMissing
				return nil
			}
		}
		res, err = completePassOrder(ctx, tx, paymentHash, renewID, c, true)
		return err
	})
	return res, err
}

// completePassOrder turns a paid order into a Pass, inside the caller's
// transaction:
//
//   - a payment already linked to a Pass (reissued before it was collected)
//     gives that Pass the new code;
//   - a renewal extends the Pass it renews by a full term from its expiry, or
//     from now if it has run out, and links the payment to it;
//   - anything else, including a renewal of a Pass refunded in the meantime,
//     is a new Pass.
//
// A renewal keeps its code unless rotate is set, which reissuing does: a buyer
// asking for a reissue has lost the code, so they get a new one either way.
func completePassOrder(ctx context.Context, tx pgx.Tx, payment []byte, renewID *int64, c passCompletion, rotate bool) (orderResult, error) {
	var res orderResult
	var linked int64
	err := tx.QueryRow(ctx, `SELECT pass_id FROM pass_payments WHERE payment_hash = $1`, payment).Scan(&linked)
	switch {
	case err == nil:
		res.State = orderIssued
		err = tx.QueryRow(ctx, `
			UPDATE passes SET token_hash = $2 WHERE id = $1 RETURNING expires_at`,
			linked, c.TokenHash).Scan(&res.ExpiresAt)
		return res, err
	case !errors.Is(err, pgx.ErrNoRows):
		return res, err
	}

	if renewID != nil {
		var current time.Time
		err := tx.QueryRow(ctx, `SELECT expires_at FROM passes WHERE id = $1 FOR UPDATE`, *renewID).Scan(&current)
		switch {
		case err == nil:
			res.ExpiresAt = c.Expiry(maxTime(current, c.Now))
			res.State = orderRenewed
			if _, err := tx.Exec(ctx, `UPDATE passes SET expires_at = $2 WHERE id = $1`, *renewID, res.ExpiresAt); err != nil {
				return res, err
			}
			if rotate {
				res.State = orderIssued
				if _, err := tx.Exec(ctx, `UPDATE passes SET token_hash = $2 WHERE id = $1`, *renewID, c.TokenHash); err != nil {
					return res, err
				}
			}
			_, err = tx.Exec(ctx, `INSERT INTO pass_payments (payment_hash, pass_id) VALUES ($1, $2)`, payment, *renewID)
			return res, err
		case !errors.Is(err, pgx.ErrNoRows):
			return res, err
		}
	}

	res.State = orderIssued
	res.ExpiresAt = c.Expiry(c.Now)
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO passes (token_hash, expires_at) VALUES ($1, $2) RETURNING id`,
		c.TokenHash, res.ExpiresAt).Scan(&id); err != nil {
		return res, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO pass_payments (payment_hash, pass_id) VALUES ($1, $2)`, payment, id)
	return res, err
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// revokePassPayment cancels whatever a refunded or disputed payment bought:
// the Pass it paid for or renewed, with every other payment on it, or the
// order still waiting to be collected. The payment is remembered for a month
// either way, so a late "paid" event for it cannot reopen what was cancelled.
// It reports whether anything was cancelled.
func (s *Store) revokePassPayment(ctx context.Context, paymentHash []byte, now time.Time) (bool, error) {
	var cancelled int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM passes WHERE id IN (SELECT pass_id FROM pass_payments WHERE payment_hash = $1)`, paymentHash)
		if err != nil {
			return err
		}
		cancelled += tag.RowsAffected()
		tag, err = tx.Exec(ctx, `DELETE FROM pass_orders WHERE payment_hash = $1`, paymentHash)
		if err != nil {
			return err
		}
		cancelled += tag.RowsAffected()
		_, err = tx.Exec(ctx, `
			INSERT INTO pass_revocations (payment_hash, expires_at) VALUES ($1, $2)
			ON CONFLICT (payment_hash) DO UPDATE SET expires_at = EXCLUDED.expires_at`,
			paymentHash, now.Add(passRevocationTTL))
		return err
	})
	return cancelled > 0, err
}

// prunePasses deletes what has outlived its use: orders past their week,
// remembered refunds past their month, and Passes a month past expiry, which
// can no longer be renewed and are bought anew instead.
func (s *Store) prunePasses(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		WITH o AS (DELETE FROM pass_orders WHERE expires_at <= $1),
		     r AS (DELETE FROM pass_revocations WHERE expires_at <= $1)
		DELETE FROM passes WHERE expires_at <= $2`, now, now.Add(-passExpiredGrace))
	return err
}
