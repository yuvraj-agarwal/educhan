package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPassSecret    = "test-pass-secret-at-least-32-characters"
	testWebhookSecret = "whsec_test_signing_secret"
	testPriceID       = "price_test_pass"
	testPublicURL     = "https://educhan.test"
)

// fakeStripe stands in for api.stripe.com: it opens checkout sessions, answers
// for one price, and remembers what it was sent.
type fakeStripe struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	sessions []url.Values
	headers  []http.Header
	price    map[string]any
	fail     bool
}

func newFakeStripe(t *testing.T) *fakeStripe {
	t.Helper()
	f := &fakeStripe{t: t, price: map[string]any{
		"id": testPriceID, "active": true, "type": "one_time", "unit_amount": 2000, "currency": "usd",
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStripe) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sk_test_key" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.fail {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"type":"api_error","message":"down"}}`)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.sessions = append(f.sessions, r.PostForm)
		f.headers = append(f.headers, r.Header.Clone())
		id := "cs_test_" + strconv.Itoa(len(f.sessions))
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "url": "https://checkout.stripe.test/c/pay/" + id})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/prices/"+testPriceID:
		_ = json.NewEncoder(w).Encode(f.price)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"no such route"}}`)
	}
}

func (f *fakeStripe) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

func testPassConfig(f *fakeStripe) PassConfig {
	return PassConfig{
		Secret:              testPassSecret,
		StripeSecretKey:     "sk_test_key",
		StripeWebhookSecret: testWebhookSecret,
		StripePriceID:       testPriceID,
		PublicURL:           testPublicURL,
		Days:                365,
		stripeAPI:           f.srv.URL,
		stripeHTTP:          f.srv.Client(),
	}
}

type passHarness struct {
	*harness
	stripe *fakeStripe
}

func passServer(t *testing.T, tune func(*Config)) *passHarness {
	t.Helper()
	return passServerLogging(t, tune, io.Discard)
}

func passServerLogging(t *testing.T, tune func(*Config), logs io.Writer) *passHarness {
	t.Helper()
	f := newFakeStripe(t)
	h := testServerLogging(t, func(cfg *Config) {
		cfg.Pass = testPassConfig(f)
		if tune != nil {
			tune(cfg)
		}
	}, logs)
	// Tests check codes back to back; TestPassActivationIsPaced keeps the pace.
	h.app.pass.activateLimiter = newCooldownLimiter(0)
	return &passHarness{harness: h, stripe: f}
}

type passResponse struct {
	Enabled   bool       `json:"enabled"`
	Days      int        `json:"days"`
	Price     *passPrice `json:"price"`
	URL       string     `json:"url"`
	Status    string     `json:"status"`
	Pass      string     `json:"pass"`
	ExpiresAt time.Time  `json:"expires_at"`
	Error     string     `json:"error"`
}

// refusalState reads the machine-readable refusal, which shares the "pass" key
// with an issued code.
func refusalState(t *testing.T, out passResponse) string {
	t.Helper()
	if out.Error == "" {
		t.Fatalf("expected a refusal, got %+v", out)
	}
	return out.Pass
}

// checkout starts a purchase in this harness's browser (its cookie jar) and
// returns the Stripe session id the fake handed out.
func (p *passHarness) checkout(renew string) string {
	p.t.Helper()
	var out passResponse
	body := map[string]any{"adult": true}
	if renew != "" {
		body["renew"] = renew
	}
	if status := p.post("/api/pass/checkout", body, &out); status != http.StatusOK {
		p.t.Fatalf("checkout: status %d (%s)", status, out.Error)
	}
	if !strings.HasPrefix(out.URL, "https://checkout.stripe.test/") {
		p.t.Fatalf("checkout url %q", out.URL)
	}
	return out.URL[strings.LastIndexByte(out.URL, '/')+1:]
}

// webhook delivers a signed event, as Stripe does.
func (p *passHarness) webhook(eventType string, object map[string]any) int {
	p.t.Helper()
	body, err := json.Marshal(map[string]any{
		"id": "evt_test", "object": "event", "type": eventType,
		"data": map[string]any{"object": object},
	})
	if err != nil {
		p.t.Fatal(err)
	}
	return p.rawWebhook(body, signStripe(body, testWebhookSecret, time.Now()))
}

func (p *passHarness) rawWebhook(body []byte, signature string) int {
	p.t.Helper()
	req, err := http.NewRequest(http.MethodPost, p.base+"/api/pass/stripe-webhook", strings.NewReader(string(body)))
	if err != nil {
		p.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("Stripe-Signature", signature)
	}
	return p.send(req, nil)
}

func signStripe(body []byte, secret string, at time.Time) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (p *passHarness) paid(session, paymentIntent string) {
	p.t.Helper()
	if status := p.webhook(stripeCheckoutCompleted, map[string]any{
		"id": session, "object": "checkout.session", "mode": "payment",
		"payment_status": "paid", "payment_intent": paymentIntent,
		"customer_details": map[string]any{"email": "buyer@example.com", "name": "Bea Buyer"},
	}); status != http.StatusOK {
		p.t.Fatalf("paid webhook: status %d", status)
	}
}

func (p *passHarness) claim() (int, passResponse) {
	p.t.Helper()
	var out passResponse
	status := p.post("/api/pass/claim", map[string]any{}, &out)
	return status, out
}

// buy runs a whole purchase and returns the issued code.
func (p *passHarness) buy(paymentIntent string) passResponse {
	p.t.Helper()
	p.paid(p.checkout(""), paymentIntent)
	status, out := p.claim()
	if status != http.StatusOK || out.Status != "issued" {
		p.t.Fatalf("claim: status %d, %+v", status, out)
	}
	return out
}

func (p *passHarness) activate(code string) (int, passResponse) {
	p.t.Helper()
	var out passResponse
	status := p.post("/api/pass/activate", map[string]any{"pass": code}, &out)
	return status, out
}

// postWithPass opens a thread on board carrying the Pass and, optionally, the
// forwarded address a request appears to come from.
func (p *passHarness) postWithPass(board, code, from string, extra map[string]any) (int, passResponse) {
	p.t.Helper()
	header := http.Header{}
	if code != "" {
		header.Set(passHeader, code)
	}
	if from != "" {
		header.Set("X-Forwarded-For", from)
	}
	body := map[string]any{"body": "posted with a pass"}
	for k, v := range extra {
		body[k] = v
	}
	var out passResponse
	status := p.postWithHeaders(header, "/api/boards/"+board+"/threads", body, &out)
	return status, out
}

func TestPassStatus(t *testing.T) {
	off := testServer(t, nil)
	var out passResponse
	off.get("/api/pass", &out)
	if out.Enabled {
		t.Fatal("the Pass is on sale with no configuration")
	}

	p := passServer(t, nil)
	p.get("/api/pass", &out)
	if !out.Enabled || out.Days != 365 || out.Price == nil || out.Price.Amount != 2000 || out.Price.Currency != "usd" {
		t.Fatalf("status: %+v", out)
	}
}

func TestPassStatusSurvivesStripeTrouble(t *testing.T) {
	p := passServer(t, nil)
	p.stripe.mu.Lock()
	p.stripe.price["type"] = "recurring"
	p.stripe.mu.Unlock()
	var out passResponse
	p.get("/api/pass", &out)
	if !out.Enabled || out.Price != nil {
		t.Fatalf("a recurring price must be refused but the page still offered: %+v", out)
	}
}

func TestPassPurchase(t *testing.T) {
	p := passServer(t, nil)

	// No box ticked: refused before Stripe is asked for anything.
	var out passResponse
	if status := p.post("/api/pass/checkout", map[string]any{}, &out); status != http.StatusBadRequest ||
		!strings.Contains(out.Error, "18") {
		t.Fatalf("checkout without the 18+ box: status %d (%s)", status, out.Error)
	}
	if n := p.stripe.sessionCount(); n != 0 {
		t.Fatalf("Stripe saw %d sessions for a refused checkout", n)
	}
	if status, _ := p.claim(); status != http.StatusBadRequest {
		t.Fatalf("claim with no purchase: status %d", status)
	}

	session := p.checkout("")
	form := p.stripe.sessions[0]
	for key, want := range map[string]string{
		"mode":                    "payment",
		"line_items[0][price]":    testPriceID,
		"line_items[0][quantity]": "1",
		"success_url":             testPublicURL + "/pass?paid",
		"cancel_url":              testPublicURL + "/pass?cancelled",
	} {
		if got := form.Get(key); got != want {
			t.Errorf("Stripe got %s=%q, want %q", key, got, want)
		}
	}
	// Stripe learns that a Pass was bought and nothing about who on educhan
	// bought it.
	for key := range form {
		if strings.HasPrefix(key, "metadata") || strings.HasPrefix(key, "client_reference_id") ||
			strings.HasPrefix(key, "customer") || strings.HasPrefix(key, "automatic_tax") {
			t.Errorf("Stripe was sent %s", key)
		}
	}
	if p.stripe.headers[0].Get("Idempotency-Key") == "" || p.stripe.headers[0].Get("Stripe-Version") != stripeAPIVersion {
		t.Errorf("checkout headers: %v", p.stripe.headers[0])
	}

	// Back from Stripe before the webhook: try again shortly.
	if status, out := p.claim(); status != http.StatusAccepted || out.Status != "pending" {
		t.Fatalf("claim before payment: status %d, %+v", status, out)
	}

	// A webhook that is not Stripe's changes nothing.
	forged, _ := json.Marshal(map[string]any{"type": stripeCheckoutCompleted, "data": map[string]any{"object": map[string]any{
		"id": session, "mode": "payment", "payment_status": "paid", "payment_intent": "pi_forged"}}})
	for name, sig := range map[string]string{
		"unsigned":        "",
		"wrong secret":    signStripe(forged, "whsec_other", time.Now()),
		"stale":           signStripe(forged, testWebhookSecret, time.Now().Add(-10*time.Minute)),
		"from the future": signStripe(forged, testWebhookSecret, time.Now().Add(10*time.Minute)),
	} {
		if status := p.rawWebhook(forged, sig); status != http.StatusBadRequest {
			t.Errorf("%s webhook: status %d, want 400", name, status)
		}
	}
	if status, _ := p.claim(); status != http.StatusAccepted {
		t.Fatalf("a forged webhook paid for the order: status %d", status)
	}

	p.paid(session, "pi_test_1")
	p.paid(session, "pi_test_1") // Stripe may deliver twice.
	status, issued := p.claim()
	if status != http.StatusOK || issued.Status != "issued" {
		t.Fatalf("claim: status %d, %+v", status, issued)
	}
	if !passCodeShape(issued.Pass) {
		t.Fatalf("issued code %q is not XXXXX-XXXXX-XXXXX-XXXXX", issued.Pass)
	}
	wantExpiry := nextDayBoundary(time.Now().AddDate(0, 0, 365))
	if !issued.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("expires %v, want %v (a year, rounded up to a day)", issued.ExpiresAt, wantExpiry)
	}

	// The code is shown once; the claim is spent.
	if status, _ := p.claim(); status != http.StatusBadRequest {
		t.Fatalf("second claim: status %d", status)
	}

	status, active := p.activate(strings.ToLower(strings.ReplaceAll(issued.Pass, "-", " ")))
	if status != http.StatusOK || active.Pass != issued.Pass || !active.ExpiresAt.Equal(issued.ExpiresAt) {
		t.Fatalf("activate: status %d, %+v", status, active)
	}
}

func passCodeShape(code string) bool {
	groups := strings.Split(code, "-")
	if len(groups) != 4 {
		return false
	}
	for _, g := range groups {
		if len(g) != 5 || strings.Trim(g, passAlphabet) != "" {
			return false
		}
	}
	return true
}

func TestPassCheckoutCooldownAndStripeFailure(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.Pass.CheckoutCooldown = time.Minute })
	p.checkout("")
	var out passResponse
	if status := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); status != http.StatusTooManyRequests {
		t.Fatalf("second checkout inside the cooldown: status %d", status)
	}

	q := passServer(t, nil)
	q.stripe.mu.Lock()
	q.stripe.fail = true
	q.stripe.mu.Unlock()
	if status := q.post("/api/pass/checkout", map[string]any{"adult": true}, &out); status != http.StatusBadGateway {
		t.Fatalf("checkout with Stripe down: status %d", status)
	}
	if status, _ := q.claim(); status != http.StatusBadRequest {
		t.Fatalf("a failed checkout left a claim behind: status %d", status)
	}
}

func TestPassActivationIsPaced(t *testing.T) {
	p := passServer(t, nil)
	p.app.pass.activateLimiter = newCooldownLimiter(passActivateCooldown)
	if status, _ := p.activate("ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ"); status != http.StatusForbidden {
		t.Fatalf("first check: status %d", status)
	}
	if status, _ := p.activate("ZZZZZ-ZZZZZ-ZZZZZ-ZZZZY"); status != http.StatusTooManyRequests {
		t.Fatalf("second check at once: status %d", status)
	}
}

func TestPassAsyncPayment(t *testing.T) {
	p := passServer(t, nil)
	session := p.checkout("")
	object := map[string]any{"id": session, "mode": "payment", "payment_status": "unpaid", "payment_intent": "pi_slow"}
	if status := p.webhook(stripeCheckoutCompleted, object); status != http.StatusOK {
		t.Fatalf("completed webhook: status %d", status)
	}
	if status, _ := p.claim(); status != http.StatusAccepted {
		t.Fatalf("an unpaid session issued a Pass: status %d", status)
	}
	object["payment_status"] = "paid"
	if status := p.webhook(stripeCheckoutAsyncSuccess, object); status != http.StatusOK {
		t.Fatalf("async webhook: status %d", status)
	}
	if status, out := p.claim(); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("claim after the async payment: status %d, %+v", status, out)
	}
}

func TestPassRefundsAndDisputes(t *testing.T) {
	p := passServer(t, nil)

	refunded := p.buy("pi_refund")
	// A partial refund leaves the Pass alone.
	p.webhook(stripeChargeRefunded, map[string]any{"object": "charge", "payment_intent": "pi_refund", "refunded": false})
	if status, _ := p.activate(refunded.Pass); status != http.StatusOK {
		t.Fatalf("a partial refund cancelled the Pass: status %d", status)
	}
	p.webhook(stripeChargeRefunded, map[string]any{"object": "charge", "payment_intent": "pi_refund", "refunded": true})
	if status, out := p.activate(refunded.Pass); status != http.StatusForbidden || refusalState(t, out) != passInvalid {
		t.Fatalf("a refunded Pass still works: status %d, %+v", status, out)
	}

	disputed := p.buy("pi_dispute")
	p.webhook(stripeDisputeCreated, map[string]any{"object": "dispute", "payment_intent": "pi_dispute"})
	if status, _ := p.activate(disputed.Pass); status != http.StatusForbidden {
		t.Fatalf("a disputed Pass still works: status %d", status)
	}

	// Refunded after paying and before collecting: nothing to collect.
	session := p.checkout("")
	p.paid(session, "pi_uncollected")
	p.webhook(stripeChargeRefunded, map[string]any{"object": "charge", "payment_intent": "pi_uncollected", "refunded": true})
	if status, _ := p.claim(); status != http.StatusBadRequest {
		t.Fatalf("a refunded order could still be collected: status %d", status)
	}

	// The refund arrives ahead of a retried "paid": the late event must not
	// turn the refunded purchase into a Pass.
	session = p.checkout("")
	p.webhook(stripeChargeRefunded, map[string]any{"object": "charge", "payment_intent": "pi_early", "refunded": true})
	p.paid(session, "pi_early")
	if status, _ := p.claim(); status != http.StatusAccepted {
		t.Fatalf("a late paid event revived a refunded payment: status %d", status)
	}
}

func TestPassRenewal(t *testing.T) {
	p := passServer(t, nil)
	issued := p.buy("pi_first")
	// Bring the expiry close, so the renewal is seen to add to it.
	soon := nextDayBoundary(time.Now().AddDate(0, 0, 10))
	p.exec(`UPDATE passes SET expires_at = $1`, soon)

	var out passResponse
	if status := p.post("/api/pass/checkout", map[string]any{"adult": true, "renew": "AAAAA-AAAAA-AAAAA-AAAAA"}, &out); status != http.StatusForbidden {
		t.Fatalf("renewing an unknown code: status %d", status)
	}

	p.paid(p.checkout(issued.Pass), "pi_renewal")
	status, renewed := p.claim()
	if status != http.StatusOK || renewed.Status != "renewed" || renewed.Pass != "" {
		t.Fatalf("renewal claim: status %d, %+v", status, renewed)
	}
	if want := nextDayBoundary(soon.AddDate(0, 0, 365)); !renewed.ExpiresAt.Equal(want) {
		t.Fatalf("renewed to %v, want %v", renewed.ExpiresAt, want)
	}
	if status, active := p.activate(issued.Pass); status != http.StatusOK || !active.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatalf("the renewed code: status %d, %+v", status, active)
	}

	// An expired Pass can still be renewed within its grace month, from now.
	p.exec(`UPDATE passes SET expires_at = $1`, time.Now().Add(-48*time.Hour))
	if status, out := p.activate(issued.Pass); status != http.StatusForbidden || refusalState(t, out) != passExpired {
		t.Fatalf("an expired Pass: status %d, %+v", status, out)
	}
	p.paid(p.checkout(issued.Pass), "pi_late_renewal")
	if status, out := p.claim(); status != http.StatusOK ||
		!out.ExpiresAt.Equal(nextDayBoundary(time.Now().AddDate(0, 0, 365))) {
		t.Fatalf("renewing an expired Pass: status %d, %+v", status, out)
	}

	// Refunding a renewal cancels the Pass it renewed.
	p.webhook(stripeChargeRefunded, map[string]any{"payment_intent": "pi_renewal", "refunded": true})
	if status, _ := p.activate(issued.Pass); status != http.StatusForbidden {
		t.Fatalf("refunding a renewal left the Pass working: status %d", status)
	}
	var left int
	if err := p.app.pool.QueryRow(context.Background(), `SELECT count(*) FROM pass_payments`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("payments of a cancelled Pass left behind: %d (%v)", left, err)
	}
}

func TestPassReissue(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	issued := p.buy("pi_lost")

	reissue := func(key, pi string) (int, passResponse) {
		var out passResponse
		status := p.postWithHeaders(http.Header{modKeyHeader: {key}}, "/api/pass/reissue",
			map[string]any{"payment_intent": pi}, &out)
		return status, out
	}
	if status, _ := reissue("wrong", "pi_lost"); status != http.StatusForbidden {
		t.Fatalf("reissue without the moderation key: status %d", status)
	}
	if status, _ := reissue("mod-key", "pi_unknown"); status != http.StatusNotFound {
		t.Fatalf("reissue of an unknown payment: status %d", status)
	}
	status, fresh := reissue("mod-key", "pi_lost")
	if status != http.StatusOK || !passCodeShape(fresh.Pass) || fresh.Pass == issued.Pass || !fresh.ExpiresAt.Equal(issued.ExpiresAt) {
		t.Fatalf("reissue: status %d, %+v", status, fresh)
	}
	if status, _ := p.activate(issued.Pass); status != http.StatusForbidden {
		t.Fatalf("the replaced code still works: status %d", status)
	}
	if status, _ := p.activate(fresh.Pass); status != http.StatusOK {
		t.Fatalf("the reissued code: status %d", status)
	}

	// Paid for and never collected: reissue issues it, and the claim is gone.
	p.paid(p.checkout(""), "pi_never_collected")
	status, rescued := reissue("mod-key", "pi_never_collected")
	if status != http.StatusOK || !passCodeShape(rescued.Pass) {
		t.Fatalf("reissue of an uncollected Pass: status %d, %+v", status, rescued)
	}
	if status, _ := p.claim(); status != http.StatusBadRequest {
		t.Fatalf("an order reissued by the owner could still be collected: status %d", status)
	}
}

func TestPassSharesOneCooldown(t *testing.T) {
	p := passServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.ThreadCooldown = time.Minute
	})
	p.addBoard("pass-board")
	issued := p.buy("pi_shared")

	if status, out := p.postWithPass("pass-board", issued.Pass, "10.0.0.1", nil); status != http.StatusCreated {
		t.Fatalf("first post with the Pass: status %d (%s)", status, out.Error)
	}
	// Somebody else, elsewhere, with the same Pass: one Pass, one cooldown.
	if status, _ := p.postWithPass("pass-board", issued.Pass, "10.0.0.2", nil); status != http.StatusTooManyRequests {
		t.Fatalf("a shared Pass escaped its cooldown: status %d", status)
	}
	// The same person without the Pass is limited only by their address.
	if status, out := p.postWithPass("pass-board", "", "10.0.0.2", nil); status != http.StatusCreated {
		t.Fatalf("posting without a Pass: status %d (%s)", status, out.Error)
	}
	// And a Pass never lifts an address's own cooldown.
	other := p.buy("pi_other")
	if status, _ := p.postWithPass("pass-board", other.Pass, "10.0.0.1", nil); status != http.StatusTooManyRequests {
		t.Fatalf("a Pass lifted the address's cooldown: status %d", status)
	}
}

func TestPassRefusedOnPosts(t *testing.T) {
	p := passServer(t, nil)
	p.addBoard("pass-board")
	issued := p.buy("pi_post")

	if status, out := p.postWithPass("pass-board", issued.Pass, "", nil); status != http.StatusCreated {
		t.Fatalf("posting with a Pass: status %d (%s)", status, out.Error)
	}
	for name, code := range map[string]string{
		"malformed": "not a pass",
		"unknown":   "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ",
	} {
		status, out := p.postWithPass("pass-board", code, "", nil)
		if status != http.StatusForbidden || refusalState(t, out) != passInvalid {
			t.Errorf("%s Pass: status %d, %+v", name, status, out)
		}
	}
	p.exec(`UPDATE passes SET expires_at = now() - interval '1 hour'`)
	if status, out := p.postWithPass("pass-board", issued.Pass, "", nil); status != http.StatusForbidden || refusalState(t, out) != passExpired {
		t.Fatalf("expired Pass: status %d, %+v", status, out)
	}

	// A badged post is the credential's request: the Pass header is not read,
	// so even a bad one does not stand in its way.
	mail := &captureSender{}
	v := passServer(t, func(cfg *Config) {
		cfg.VerifySecret = "test-verification-secret"
		cfg.VerifyCodeTTL = defaultVerifyCodeTTL
		cfg.VerifyCredentialTTL = defaultVerifyCredentialTTL
		cfg.Sender = mail
	})
	v.addBoard("pass-board")
	credential := (&verifyHarness{harness: v.harness, mail: mail}).verify("ana@iitb.ac.in").Credential
	var badged createdPost
	if status := v.postWithHeaders(http.Header{credentialHeader: {credential}, passHeader: {"not a pass"}},
		"/api/boards/pass-board/threads", map[string]any{"body": "badged", "badge": true}, &badged); status != http.StatusCreated {
		t.Fatalf("badged post with a Pass header: status %d (%s)", status, badged.Error)
	}

	// A deployment that stops selling ignores Passes rather than refusing posts.
	off := testServer(t, nil)
	off.addBoard("pass-board")
	var created createdPost
	if status := off.postWithHeaders(http.Header{passHeader: {"not a pass"}},
		"/api/boards/pass-board/threads", map[string]any{"body": "hi"}, &created); status != http.StatusCreated {
		t.Fatalf("posting with a Pass header where the Pass is off: status %d (%s)", status, created.Error)
	}
}

func TestPassDisabled(t *testing.T) {
	h := testServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	var out passResponse
	for _, path := range []string{"/api/pass/checkout", "/api/pass/claim", "/api/pass/activate"} {
		if status := h.post(path, map[string]any{}, &out); status != http.StatusForbidden {
			t.Errorf("%s with the Pass off: status %d", path, status)
		}
	}
	if status := h.postWithHeaders(http.Header{modKeyHeader: {"mod-key"}}, "/api/pass/reissue",
		map[string]any{"payment_intent": "pi_x"}, &out); status != http.StatusForbidden {
		t.Errorf("reissue with the Pass off: status %d", status)
	}
	if status := h.post("/api/pass/stripe-webhook", map[string]any{}, &out); status != http.StatusNotFound {
		t.Errorf("webhook with the Pass off: status %d", status)
	}
}

// TestPassIsNeverStoredOrLogged holds the Pass to the poster key's standard:
// a code used on every posting path is found in no table but its own, nowhere
// in the log, and neither are the buyer's details from the webhook.
func TestPassIsNeverStoredOrLogged(t *testing.T) {
	var logs syncBuffer
	p := passServerLogging(t, func(cfg *Config) { cfg.ModKey = "mod-key" }, &logs)
	p.addBoard("scan")
	issued := p.buy("pi_scan_payment")
	code, _ := parsePassCode(issued.Pass)
	const marker = "body-marker-for-the-pass-scan"

	b := browser{harness: p.harness, key: newPosterKey(t)}
	header := b.headers(false)
	header.Set(passHeader, issued.Pass)
	var created createdPost
	if status := p.postWithHeaders(header, "/api/boards/scan/threads", map[string]any{"body": marker}, &created); status != http.StatusCreated {
		t.Fatalf("thread with a Pass: status %d (%s)", status, created.Error)
	}
	thread := created.ThreadNo
	if status := p.postWithHeaders(header, fmt.Sprintf("/api/boards/scan/threads/%d/posts", thread),
		map[string]any{"body": "reply with a pass"}, &created); status != http.StatusCreated {
		t.Fatalf("reply with a Pass: status %d (%s)", status, created.Error)
	}
	if status := p.uploadWithHeaders(header, fmt.Sprintf("/api/boards/scan/threads/%d/posts", thread),
		map[string]string{"body": "image reply with a pass"}, "pass.png", pngBytes(t, 16, 16), &created); status != http.StatusCreated {
		t.Fatalf("image reply with a Pass: status %d (%s)", status, created.Error)
	}
	if status, _ := p.postWithPass("scan", "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ", "", nil); status != http.StatusForbidden {
		t.Fatalf("unknown Pass: status %d", status)
	}

	stored := p.app.passTokenHash(code)
	// Positive controls: the scan finds the post body in posts, and the stored
	// hash in passes, so a scan that finds nothing has looked.
	if n := p.rowsContaining("posts", []byte(marker)); n != 1 {
		t.Fatalf("the scan found the marker in %d posts, want 1; it cannot be trusted", n)
	}
	if n := p.rowsContaining("passes", stored); n != 1 {
		t.Fatalf("the scan found the stored hash in %d passes rows, want 1; it misses bytea columns", n)
	}
	cooldownKey := p.app.passCooldownKey(code)
	for name, needle := range map[string][]byte{
		"the code":                []byte(issued.Pass),
		"the canonical code":      []byte(code),
		"the stored hash":         stored,
		"the cooldown key":        []byte(cooldownKey),
		"the unknown code":        []byte("ZZZZZZZZZZZZZZZZZZZZ"),
		"SHA-256 of the code":     sha(code),
		"the payment intent":      []byte("pi_scan_payment"),
		"the buyer's email":       []byte("buyer@example.com"),
		"the buyer's name":        []byte("Bea Buyer"),
		"the checkout session id": []byte("cs_test_1"),
	} {
		for _, table := range p.tables() {
			if table == "passes" && name == "the stored hash" {
				continue
			}
			if n := p.rowsContaining(table, needle); n != 0 {
				t.Errorf("%d rows of %s contain %s", n, table, name)
			}
		}
	}

	out := logs.String()
	if !strings.Contains(out, `"path":"/api/pass/stripe-webhook"`) || !strings.Contains(out, `"path":"/api/boards/scan/threads"`) {
		t.Fatalf("expected the webhook and the posts in the log, got:\n%s", out)
	}
	for name, needle := range map[string]string{
		"the code":           issued.Pass,
		"the canonical code": code,
		"the stored hash":    hex.EncodeToString(stored),
		"the cooldown key":   cooldownKey,
		"the payment intent": "pi_scan_payment",
		"the buyer's email":  "buyer@example.com",
		"the buyer's name":   "Bea Buyer",
		"the session id":     "cs_test_1",
	} {
		if strings.Contains(out, needle) {
			t.Errorf("the log contains %s:\n%s", name, out)
		}
	}
}

func sha(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// TestPassTablesCannotBeJoinedToPosts pins the Pass tables the way the
// verification tables are pinned: every column is a decision, none of them
// can be joined to a post, and their only references are to each other.
func TestPassTablesCannotBeJoinedToPosts(t *testing.T) {
	h := testServer(t, nil)

	want := map[string][]string{
		"passes":           {"expires_at", "id", "token_hash"},
		"pass_payments":    {"pass_id", "payment_hash"},
		"pass_orders":      {"claim_hash", "expires_at", "payment_hash", "renew_pass_id", "session_hash"},
		"pass_revocations": {"expires_at", "payment_hash"},
	}
	for table, columns := range want {
		if got := h.columns(table); strings.Join(got, ",") != strings.Join(columns, ",") {
			t.Errorf("%s has columns %v, want exactly %v", table, got, columns)
		}
		rows, err := h.app.pool.Query(context.Background(), `
			SELECT confrelid::regclass::text FROM pg_constraint
			WHERE conrelid = $1::regclass AND contype = 'f'`, table)
		if err != nil {
			t.Fatalf("foreign keys of %s: %v", table, err)
		}
		for rows.Next() {
			var target string
			if err := rows.Scan(&target); err != nil {
				t.Fatal(err)
			}
			if target != "passes" {
				t.Errorf("%s references %s; the Pass tables reference only each other", table, target)
			}
		}
		rows.Close()
	}

	for _, column := range h.columns("posts") {
		if strings.Contains(column, "pass") || strings.Contains(column, "payment") {
			t.Errorf("posts.%s links a post to a Pass", column)
		}
	}
}
