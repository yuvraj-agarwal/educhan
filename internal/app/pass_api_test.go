package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	testPassSecret    = "test-pass-secret-of-at-least-32-bytes"
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
	// price is the Price as Stripe keeps it; its "currency_options" are sent
	// only when asked for, as Stripe sends them.
	price map[string]any
	// priceReads counts the times the price was asked for, and priceQueries
	// holds the query each time.
	priceReads   int
	priceQueries []url.Values
	// priceStatus, when set, is the error status the price is read with, and
	// priceMessage Stripe's message for it, as for a Price ID or key Stripe
	// refuses or a read that fails.
	priceStatus  int
	priceMessage string
	// priceDropped, when set, drops the connection the price is asked for
	// on, unanswered, as for Stripe out of reach.
	priceDropped bool
	fail         bool
	// sessionURL, when set, is the page a new session is said to be on.
	sessionURL string
	// opening, when set, runs while a session is being created, with the
	// fake's lock released, to land something in between a checkout's steps.
	opening func()
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
	opening := f.opening
	f.mu.Unlock()
	if opening != nil && r.URL.Path == "/v1/checkout/sessions" {
		opening()
	}
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
		page := "https://checkout.stripe.com/c/pay/" + id
		if f.sessionURL != "" {
			page = f.sessionURL
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "url": page})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/prices/"+testPriceID:
		f.priceReads++
		f.priceQueries = append(f.priceQueries, r.URL.Query())
		if f.priceDropped {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				f.t.Errorf("drop the price's connection: %v", err)
				return
			}
			conn.Close()
			return
		}
		if f.priceStatus != 0 {
			kind := "invalid_request_error"
			if f.priceStatus >= 500 {
				kind = "api_error"
			}
			w.WriteHeader(f.priceStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": kind, "message": f.priceMessage}})
			return
		}
		price := maps.Clone(f.price)
		if !slices.Contains(r.URL.Query()["expand[]"], "currency_options") {
			delete(price, "currency_options")
		}
		_ = json.NewEncoder(w).Encode(price)
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

func (f *fakeStripe) priceReadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.priceReads
}

func testPassConfig(f *fakeStripe) PassConfig {
	return PassConfig{
		Secret:              testPassSecret,
		StripeSecretKey:     "sk_test_key",
		StripeWebhookSecret: testWebhookSecret,
		StripePriceID:       testPriceID,
		PublicURL:           testPublicURL,
		Days:                365,
		HourlyPosts:         30,
		stripeAPI:           f.srv.URL,
		stripeHTTP:          f.srv.Client(),
	}
}

type passHarness struct {
	*harness
	stripe *fakeStripe
	// nonce is the one the page would be sending with its claims, as the page
	// keeps it: drawn when a collection starts, and dropped once an answer
	// says nothing is waiting any more (keepNonce).
	nonce string
}

// collectNonce is the nonce this browser's next claim or reissue sends.
func (p *passHarness) collectNonce() string {
	p.t.Helper()
	if p.nonce == "" {
		var err error
		if p.nonce, err = randomToken(); err != nil {
			p.t.Fatal(err)
		}
	}
	return p.nonce
}

// keepNonce drops the nonce as the page does: once an answer arrives that
// says nothing is waiting under the claim, or refuses the request.
func (p *passHarness) keepNonce(status int, out passResponse) {
	if status >= 300 || !out.Claim {
		p.nonce = ""
	}
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
	Enabled     bool       `json:"enabled"`
	Days        int        `json:"days"`
	HourlyPosts int        `json:"hourly_posts"`
	Price       *passPrice `json:"price"`
	Tax         string     `json:"tax"`
	Paused      bool       `json:"paused"`
	URL         string     `json:"url"`
	Order       string     `json:"order"`
	Status      string     `json:"status"`
	Pass        string     `json:"pass"`
	ExpiresAt   time.Time  `json:"expires_at"`
	Claim       bool       `json:"claim"`
	Code        string     `json:"code"`
	Error       string     `json:"error"`
	RetryAfter  int        `json:"retry_after"`
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
	session, _ := p.checkoutNamed(renew)
	return session
}

// checkoutNamed starts a purchase as checkout does, and returns the name the
// answer gave its order as well, which the page sends back to forget that
// order alone (forgetOrder).
func (p *passHarness) checkoutNamed(renew string) (string, string) {
	p.t.Helper()
	var out passResponse
	body := map[string]any{"adult": true}
	if renew != "" {
		body["renew"] = renew
	}
	if status := p.post("/api/pass/checkout", body, &out); status != http.StatusOK {
		p.t.Fatalf("checkout: status %d (%s)", status, out.Error)
	}
	if !strings.HasPrefix(out.URL, "https://checkout.stripe.com/c/pay/") {
		p.t.Fatalf("checkout url %q", out.URL)
	}
	if out.Order == "" {
		p.t.Fatalf("checkout: %+v, with no name for its order", out)
	}
	return out.URL[strings.LastIndexByte(out.URL, '/')+1:], out.Order
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
	status, out := p.claimWith(p.collectNonce())
	p.keepNonce(status, out)
	return status, out
}

// claimWith collects with the nonce given, as a page asking again after an
// answer it never received would.
func (p *passHarness) claimWith(nonce string) (int, passResponse) {
	p.t.Helper()
	var out passResponse
	status := p.post("/api/pass/claim", map[string]any{"nonce": nonce}, &out)
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

	// Stripe Tax is off, so nothing is said of tax (TestPassTaxFollowsThePrice).
	p := passServer(t, nil)
	p.get("/api/pass", &out)
	if !out.Enabled || out.Days != 365 || out.HourlyPosts != 30 || out.Price == nil || out.Price.Amount != 2000 ||
		out.Price.Currency != "usd" || out.Tax != "" || out.Claim {
		t.Fatalf("status: %+v", out)
	}
}

// TestPassPriceCarriesStripesExponent holds the price GET /api/pass sends to
// Stripe's own amount beside the exponent Stripe keeps it in, from Stripe's
// table and not the browser's, for every currency on Stripe's lists
// (docs.stripe.com/currencies): none for its zero-decimal currencies, but for
// ISK and UGX, which Stripe still writes in hundredths though the browser
// gives them no decimals, so the page shows 3,000 ISK, not 300,000; three for
// its three-decimal ones; and two for the rest, as for USD and HUF. The
// currency is sent in lower case, and looked up in the table so, whatever case
// Stripe writes it in. stripeDecimals holds those lists and nothing else, so
// a row dropped or added there fails here.
func TestPassPriceCarriesStripesExponent(t *testing.T) {
	stripes := map[string]int{
		"bif": 0, "clp": 0, "djf": 0, "gnf": 0, "jpy": 0, "kmf": 0, "krw": 0, "mga": 0,
		"pyg": 0, "rwf": 0, "vnd": 0, "vuv": 0, "xaf": 0, "xof": 0, "xpf": 0,
		"isk": 2, "ugx": 2,
		"bhd": 3, "jod": 3, "kwd": 3, "omr": 3, "tnd": 3,
		"usd": 2, "huf": 2,
	}
	notTwo := maps.Clone(stripes)
	maps.DeleteFunc(notTwo, func(_ string, exponent int) bool { return exponent == 2 })
	if !maps.Equal(stripeDecimals, notTwo) {
		t.Errorf("stripeDecimals is %v; want Stripe's zero- and three-decimal currencies, but ISK and UGX: %v", stripeDecimals, notTwo)
	}
	var currencies []string
	for currency := range stripes {
		currencies = append(currencies, currency)
	}
	slices.Sort(currencies)
	p := passServer(t, nil)
	for _, currency := range currencies {
		for _, written := range []string{currency, strings.ToUpper(currency)} {
			p.stripe.mu.Lock()
			p.stripe.price["currency"], p.stripe.price["unit_amount"] = written, 7500
			p.stripe.mu.Unlock()
			p.app.pass.priceMu.Lock()
			p.app.pass.priceFetchedAt = time.Time{}
			p.app.pass.priceMu.Unlock()
			var status map[string]any
			p.get("/api/pass", &status)
			want := map[string]any{"amount": float64(7500), "currency": currency, "exponent": float64(stripes[currency])}
			if got, _ := status["price"].(map[string]any); !maps.Equal(got, want) {
				t.Errorf("GET /api/pass for %s: price %v, want %v", written, status["price"], want)
			}
		}
	}
}

// TestPassStatusSurvivesStripeTrouble holds GET /api/pass to offering the Pass
// without its price when Stripe can't be asked for it, refuses the request for
// the Price, or answers with a Price the Pass can't be sold at: archived,
// recurring or without a fixed amount, which a zero amount or no currency is
// not either.
func TestPassStatusSurvivesStripeTrouble(t *testing.T) {
	for what, trouble := range map[string]func(f *fakeStripe){
		"Stripe down":       func(f *fakeStripe) { f.fail = true },
		"no such Price":     func(f *fakeStripe) { f.priceStatus = http.StatusNotFound },
		"key refused":       func(f *fakeStripe) { f.priceStatus = http.StatusUnauthorized },
		"archived":          func(f *fakeStripe) { f.price["active"] = false },
		"recurring":         func(f *fakeStripe) { f.price["type"] = "recurring" },
		"without an amount": func(f *fakeStripe) { f.price["unit_amount"] = nil },
		"zero amount":       func(f *fakeStripe) { f.price["unit_amount"] = 0 },
		"no currency":       func(f *fakeStripe) { f.price["currency"] = "" },
	} {
		p := passServer(t, nil)
		p.stripe.mu.Lock()
		trouble(p.stripe)
		p.stripe.mu.Unlock()
		var out passResponse
		p.get("/api/pass", &out)
		if !out.Enabled || out.Price != nil {
			t.Errorf("%s: %+v; want the Pass offered without a price", what, out)
		}
	}
}

// TestPassTaxFollowsThePrice holds what GET /api/pass says of tax to the
// Price's own tax behavior, and checkout to telling Stripe whether to collect
// tax either way, so an account that collects it by default adds none the page
// did not show, and to sending nothing else, with Stripe Tax on as with it off
// (checkSessionSent): nothing that lets the buyer change what they pay is added
// for Stripe Tax. With Stripe Tax on, an inclusive Price has the tax "included"
// and an exclusive one has it "added" at checkout. A Price that says neither,
// or was made before Prices said, is not sold, since Stripe would settle it by
// a default in the account that the page cannot know: the status leaves the
// price out, and checkout refuses before Stripe opens anything or a cooldown
// starts, as it refuses every Price it can't sell
// (TestPassRefusesAPriceItCannotSell), and the log says what to fix once, where
// the Price is read, and nothing of the buyer. Nor is anything sold while the
// Price can't be read. Given a behavior, the Price sells. With Stripe Tax off,
// no tax is collected and nothing is said of it, whatever the Price says.
func TestPassTaxFollowsThePrice(t *testing.T) {
	// absent is a Price without the field; nil is one where Stripe has it null.
	type absent struct{}
	for _, tc := range []struct {
		name     string
		tax      bool
		behavior any
		word     string
	}{
		{"Stripe Tax off, inclusive", false, "inclusive", ""},
		{"Stripe Tax off, exclusive", false, "exclusive", ""},
		{"Stripe Tax off, unspecified", false, "unspecified", ""},
		{"Stripe Tax off, null", false, nil, ""},
		{"Stripe Tax on, inclusive", true, "inclusive", "included"},
		{"Stripe Tax on, exclusive", true, "exclusive", "added"},
		{"Stripe Tax on, unspecified", true, "unspecified", ""},
		{"Stripe Tax on, null", true, nil, ""},
		{"Stripe Tax on, absent", true, absent{}, ""},
	} {
		sold := !tc.tax || tc.word != ""
		t.Run(tc.name, func(t *testing.T) {
			var logs syncBuffer
			p := passServerLogging(t, func(cfg *Config) {
				cfg.Pass.StripeAutomaticTax = tc.tax
				cfg.Pass.CheckoutCooldown = time.Hour
			}, &logs)
			p.stripe.mu.Lock()
			if _, ok := tc.behavior.(absent); ok {
				delete(p.stripe.price, "tax_behavior")
			} else {
				p.stripe.price["tax_behavior"] = tc.behavior
			}
			p.stripe.mu.Unlock()

			var status map[string]any
			p.get("/api/pass", &status)
			_, priced := status["price"]
			tax, said := status["tax"]
			if priced != sold || said != (tc.word != "") || said && tax != tc.word {
				t.Fatalf("GET /api/pass: %v; want the price %t and the tax %q", status, sold, tc.word)
			}
			// The price is its amount, currency and exponent alone: what the
			// Price says of tax goes out as "tax", in the server's words, or
			// not at all.
			if got, _ := status["price"].(map[string]any); sold && !maps.Equal(got, map[string]any{"amount": float64(2000), "currency": "usd", "exponent": float64(2)}) {
				t.Fatalf("GET /api/pass price: %v; want the amount, the currency and its exponent alone", status["price"])
			}

			var out passResponse
			code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out)
			if sold {
				if code != http.StatusOK {
					t.Fatalf("checkout: status %d (%s)", code, out.Error)
				}
				checkSessionSent(t, p.stripe.sessions[0], tc.tax)
				return
			}
			if code != http.StatusServiceUnavailable || out.Error != passPriceUnsold || p.stripe.sessionCount() != 0 {
				t.Fatalf("checkout of a Price that does not say whether tax is in it: status %d (%s), %d Stripe sessions; want 503 and none",
					code, out.Error, p.stripe.sessionCount())
			}
			// What to fix is logged where the Price is read, once while the
			// refusal is kept, and not again for each checkout it refuses.
			if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusServiceUnavailable {
				t.Fatalf("a second checkout of that Price: status %d (%s)", code, out.Error)
			}
			var told int
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				if !strings.Contains(line, "EDUCHAN_STRIPE_AUTOMATIC_TAX") {
					continue
				}
				told++
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatalf("a log line is not JSON: %q", line)
				}
				if len(record) != 4 || record["msg"] != "the Pass price in Stripe needs fixing" || record["error"] != errPassPriceTax.Error() ||
					!strings.Contains(errPassPriceTax.Error(), "EDUCHAN_STRIPE_PRICE_ID") {
					t.Errorf("the log line does not say what to fix, or carries more than its words: %s", line)
				}
			}
			if told != 1 {
				t.Fatalf("%d log lines say what to fix, want 1 for a read and two refused checkouts:\n%s", told, logs.String())
			}

			// Given a behavior, the Price sells once the cached refusal runs
			// out, and the refused checkout started no cooldown.
			p.stripe.mu.Lock()
			p.stripe.price["tax_behavior"] = "inclusive"
			p.stripe.mu.Unlock()
			p.app.pass.priceMu.Lock()
			p.app.pass.priceFetchedAt = time.Time{}
			p.app.pass.priceMu.Unlock()
			p.get("/api/pass", &status)
			if status["tax"] != "included" || status["price"] == nil {
				t.Fatalf("GET /api/pass once the Price is inclusive: %v", status)
			}
			if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusOK {
				t.Fatalf("checkout once the Price is inclusive: status %d (%s)", code, out.Error)
			}
			checkSessionSent(t, p.stripe.sessions[0], true)
		})
	}

	// While Stripe can't be asked for the Price, it may be one the Pass can't
	// be sold at (with Stripe Tax on, one that says neither, as here), so
	// nothing is sold, with Stripe Tax on or off
	// (TestPassRefusesAPriceItCannotSell): checkout answers 502 for as long
	// as the failed read is kept, with nothing opened in Stripe, no cooldown
	// started and nothing logged past the failed read itself.
	t.Run("Stripe Tax on, the Price not read", func(t *testing.T) {
		var logs syncBuffer
		p := passServerLogging(t, func(cfg *Config) {
			cfg.Pass.StripeAutomaticTax = true
			cfg.Pass.CheckoutCooldown = time.Hour
		}, &logs)
		p.stripe.mu.Lock()
		p.stripe.price["tax_behavior"] = "unspecified"
		p.stripe.fail = true
		p.stripe.mu.Unlock()
		var status map[string]any
		if p.get("/api/pass", &status); status["price"] != nil {
			t.Fatalf("GET /api/pass with Stripe down: %v", status)
		}
		p.stripe.mu.Lock()
		p.stripe.fail = false
		p.stripe.mu.Unlock()
		var out passResponse
		if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusBadGateway ||
			out.Error != passStripeUnreachable || p.stripe.sessionCount() != 0 {
			t.Fatalf("checkout while the Price's failed read is kept: status %d (%s), %d Stripe sessions; want 502 and none",
				code, out.Error, p.stripe.sessionCount())
		}
		if n := strings.Count(logs.String(), `"msg":"could not read the Pass price from Stripe"`); n != 1 {
			t.Fatalf("%d log lines for one failed read of the Price, want 1:\n%s", n, logs.String())
		}

		// Read again once the failure runs out, the Price is refused for what
		// it says, and given a behavior it sells.
		refresh := func() {
			p.app.pass.priceMu.Lock()
			p.app.pass.priceFetchedAt = time.Time{}
			p.app.pass.priceMu.Unlock()
		}
		refresh()
		if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusServiceUnavailable || out.Error != passPriceUnsold {
			t.Fatalf("checkout once the Price is read: status %d (%s)", code, out.Error)
		}
		p.stripe.mu.Lock()
		p.stripe.price["tax_behavior"] = "inclusive"
		p.stripe.mu.Unlock()
		refresh()
		if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusOK || p.stripe.sessionCount() != 1 {
			t.Fatalf("checkout once the Price is inclusive: status %d (%s)", code, out.Error)
		}
		checkSessionSent(t, p.stripe.sessions[0], true)
	})
}

// TestPassRefusesAPriceItCannotSell holds a Price that Stripe answers with but
// the Pass can't be sold at, archived, recurring, without a fixed amount (one
// whose buyer chooses what to pay among them), priced in other currencies too
// (TestPassSellsInThePricesOwnCurrencyAlone) or, with Stripe Tax on, silent on
// tax, and a request for the Price Stripe refuses (a 400, 401, 403 or 404),
// to the fault in the setup that it is, with Stripe Tax on or off. Checkout
// refuses it with 503 and words of its own, never the 502 that says to try
// again in a moment, which is for Stripe out of reach or failing (a 5xx, or a
// 429); it opens nothing in Stripe and starts no cooldown, and the log says
// what is wrong once, where the Price is read. The refusal is kept a minute,
// as a failed read is, so the Price fixed sells once that has run out.
func TestPassRefusesAPriceItCannotSell(t *testing.T) {
	refused := func(status int, message string) func(f *fakeStripe) {
		return func(f *fakeStripe) { f.priceStatus, f.priceMessage = status, message }
	}
	const byStripe = "Stripe refused the request for the Price: stripe GET /v1/prices/" + testPriceID + ": status "
	for _, tc := range []struct {
		name   string
		set    func(f *fakeStripe)
		reason string
		// taxOnly is a Price refused only while Stripe Tax is on.
		taxOnly bool
	}{
		{"archived", func(f *fakeStripe) { f.price["active"] = false }, "it is archived", false},
		{"recurring", func(f *fakeStripe) { f.price["type"] = "recurring" }, `it must be one-time, not "recurring"`, false},
		{"without an amount", func(f *fakeStripe) { f.price["unit_amount"] = nil }, "it has no fixed amount", false},
		// "Customer chooses what to pay": Stripe sends the Price with no
		// unit_amount, and the buyer types the amount on Stripe's page.
		{"customer chooses what to pay", func(f *fakeStripe) {
			f.price["unit_amount"] = nil
			f.price["custom_unit_amount"] = map[string]any{"minimum": 100, "maximum": nil, "preset": nil}
		}, "it has no fixed amount", false},
		{"zero amount", func(f *fakeStripe) { f.price["unit_amount"] = 0 }, "it has no fixed amount", false},
		{"no currency", func(f *fakeStripe) { f.price["currency"] = "" }, "it has no fixed amount", false},
		// Checkout would charge a buyer it places in another currency that
		// option's amount, under its own tax behavior, or what they choose
		// to pay. The Price's own currency, which Stripe may list among its
		// options, is no other.
		{"in another currency too", func(f *fakeStripe) {
			f.price["currency_options"] = map[string]any{
				"usd": map[string]any{"unit_amount": 2000, "tax_behavior": "inclusive"},
				"eur": map[string]any{"unit_amount": 1900, "tax_behavior": "exclusive"},
			}
		}, "it must be priced in usd alone, not also in eur", false},
		{"in other currencies too", func(f *fakeStripe) {
			f.price["currency_options"] = map[string]any{
				"jpy": map[string]any{"unit_amount": 3000, "tax_behavior": "inclusive"},
				"gbp": map[string]any{"unit_amount": 1600, "tax_behavior": "inclusive"},
				"eur": map[string]any{"unit_amount": nil, "custom_unit_amount": map[string]any{"minimum": 100}, "tax_behavior": "inclusive"},
			}
		}, "it must be priced in usd alone, not also in eur, gbp, jpy", false},
		{"silent on tax", func(f *fakeStripe) { f.price["tax_behavior"] = "unspecified" }, "with EDUCHAN_STRIPE_AUTOMATIC_TAX on", true},
		{"no such Price", refused(http.StatusNotFound, "No such price: '"+testPriceID+"'"), byStripe + "404: invalid_request_error: No such price", false},
		{"key refused", refused(http.StatusUnauthorized, "Invalid API Key provided: sk_test_***key"), byStripe + "401: invalid_request_error: Invalid API Key", false},
		{"key not allowed", refused(http.StatusForbidden, "The provided key does not have the required permissions"), byStripe + "403: invalid_request_error: The provided key", false},
		{"request refused", refused(http.StatusBadRequest, "Invalid Stripe API version"), byStripe + "400: invalid_request_error: Invalid Stripe API version", false},
	} {
		for _, tax := range []bool{false, true} {
			if tc.taxOnly && !tax {
				// Sold with Stripe Tax off (TestPassTaxFollowsThePrice).
				continue
			}
			t.Run(fmt.Sprintf("Stripe Tax %s, %s", map[bool]string{false: "off", true: "on"}[tax], tc.name), func(t *testing.T) {
				var logs syncBuffer
				p := passServerLogging(t, func(cfg *Config) {
					cfg.Pass.StripeAutomaticTax = tax
					cfg.Pass.CheckoutCooldown = time.Hour
				}, &logs)
				p.stripe.mu.Lock()
				p.stripe.price["tax_behavior"] = "inclusive"
				tc.set(p.stripe)
				p.stripe.mu.Unlock()
				var status passResponse
				if p.get("/api/pass", &status); !status.Enabled || status.Price != nil {
					t.Fatalf("GET /api/pass: %+v; want the Pass offered without a price", status)
				}
				for try := range 2 {
					var out passResponse
					if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusServiceUnavailable ||
						out.Error != passPriceUnsold || p.stripe.sessionCount() != 0 {
						t.Fatalf("checkout %d: status %d (%s), %d Stripe sessions; want 503 %q and none",
							try+1, code, out.Error, p.stripe.sessionCount(), passPriceUnsold)
					}
				}
				var told int
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					var record map[string]any
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatalf("a log line is not JSON: %q", line)
					}
					switch record["msg"] {
					case "the Pass price in Stripe needs fixing":
						told++
						if record["level"] != "ERROR" || !strings.HasPrefix(fmt.Sprint(record["error"]), errPassPriceRefused.Error()+": "+tc.reason) {
							t.Errorf("the log line does not say what is wrong with the Price: %s", line)
						}
					case "could not read the Pass price from Stripe":
						t.Errorf("a Price Stripe answered with is logged as one it could not be asked for: %s", line)
					}
				}
				if told != 1 {
					t.Fatalf("%d log lines say what is wrong with the Price, want 1 for a read and two refused checkouts:\n%s", told, logs.String())
				}

				// Fixed, the Price sells once the refusal has been kept a
				// minute, and the refused checkouts started no cooldown.
				p.stripe.mu.Lock()
				maps.Copy(p.stripe.price, map[string]any{
					"active": true, "type": "one_time", "unit_amount": 2000, "currency": "usd", "tax_behavior": "inclusive",
				})
				delete(p.stripe.price, "custom_unit_amount")
				delete(p.stripe.price, "currency_options")
				p.stripe.priceStatus = 0
				p.stripe.mu.Unlock()
				p.app.pass.priceMu.Lock()
				p.app.pass.priceFetchedAt = time.Now().Add(-61 * time.Second)
				p.app.pass.priceMu.Unlock()
				var out passResponse
				if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusOK || p.stripe.sessionCount() != 1 {
					t.Fatalf("checkout once the Price is fixed: status %d (%s)", code, out.Error)
				}
			})
		}
	}

	// Stripe out of reach, failing, or asking for fewer requests refuses
	// nothing, with Stripe Tax on or off: it is a read that failed, which
	// asking again a minute later can mend. Checkout answers 502 before any
	// cooldown, with nothing opened in Stripe.
	for _, tax := range []bool{false, true} {
		for _, status := range []int{0, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
			what := fmt.Sprintf("status %d", status)
			if status == 0 {
				what = "out of reach"
			}
			t.Run(fmt.Sprintf("Stripe Tax %s, %s", map[bool]string{false: "off", true: "on"}[tax], what), func(t *testing.T) {
				var logs syncBuffer
				p := passServerLogging(t, func(cfg *Config) {
					cfg.Pass.StripeAutomaticTax = tax
					cfg.Pass.CheckoutCooldown = time.Hour
				}, &logs)
				p.stripe.mu.Lock()
				p.stripe.price["tax_behavior"] = "inclusive"
				p.stripe.priceDropped = status == 0
				p.stripe.priceStatus, p.stripe.priceMessage = status, "try later"
				p.stripe.mu.Unlock()
				var out passResponse
				if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusBadGateway ||
					out.Error != passStripeUnreachable || p.stripe.sessionCount() != 0 {
					t.Fatalf("checkout: status %d (%s), %d Stripe sessions; want 502 %q and none",
						code, out.Error, p.stripe.sessionCount(), passStripeUnreachable)
				}
				if n := strings.Count(logs.String(), `"msg":"could not read the Pass price from Stripe"`); n != 1 ||
					strings.Contains(logs.String(), "needs fixing") {
					t.Fatalf("%d log lines for one failed read of the Price, want 1 and no refusal:\n%s", n, logs.String())
				}

				// Stripe back, the Price sells once the failure has been kept
				// a minute, and the failed checkout started no cooldown.
				p.stripe.mu.Lock()
				p.stripe.priceDropped, p.stripe.priceStatus = false, 0
				p.stripe.mu.Unlock()
				p.app.pass.priceMu.Lock()
				p.app.pass.priceFetchedAt = time.Now().Add(-61 * time.Second)
				p.app.pass.priceMu.Unlock()
				if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusOK || p.stripe.sessionCount() != 1 {
					t.Fatalf("checkout once Stripe is back: status %d (%s)", code, out.Error)
				}
			})
		}
	}
}

// TestPassSellsInThePricesOwnCurrencyAlone holds checkout to selling only a
// Price priced in its own currency alone, with Stripe Tax on or off. The Price
// is read with its currency options (expand[]=currency_options), which Stripe
// leaves out unless asked for them, so one priced in other currencies too is
// seen and refused (TestPassRefusesAPriceItCannotSell), while one whose options
// hold only its own currency, which Stripe may list, is the Price itself and
// sells, as one with none does. Whether Stripe converts the price into a
// buyer's own currency is left to Adaptive Pricing, the Stripe account's
// setting: no session sends adaptive_pricing (checkSessionSent). Options added
// to the Price in place, keeping its ID, are seen by the first read after the
// hour a good read is kept, and the Price is refused from then, with Stripe Tax
// on or off. The refusal names the other currencies in order, whatever order
// Stripe lists them in.
func TestPassSellsInThePricesOwnCurrencyAlone(t *testing.T) {
	own := map[string]any{"unit_amount": 2000, "tax_behavior": "inclusive", "custom_unit_amount": nil}
	asked := url.Values{"expand[]": {"currency_options"}}
	for _, tax := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			currency string
			// options are the Price's currency options, or none at all if nil.
			options map[string]any
		}{
			{"no currency options", "usd", nil},
			{"no currency options listed", "usd", map[string]any{}},
			{"its own currency alone", "usd", map[string]any{"usd": own}},
			{"its own currency alone, the Price's in capitals", "USD", map[string]any{"usd": own}},
		} {
			t.Run(fmt.Sprintf("Stripe Tax %s, %s", map[bool]string{false: "off", true: "on"}[tax], tc.name), func(t *testing.T) {
				p := passServer(t, func(cfg *Config) { cfg.Pass.StripeAutomaticTax = tax })
				p.stripe.mu.Lock()
				p.stripe.price["tax_behavior"] = "inclusive"
				p.stripe.price["currency"] = tc.currency
				if tc.options != nil {
					p.stripe.price["currency_options"] = tc.options
				}
				p.stripe.mu.Unlock()
				var status map[string]any
				p.get("/api/pass", &status)
				want := map[string]any{"amount": float64(2000), "currency": "usd", "exponent": float64(2)}
				if got, _ := status["price"].(map[string]any); !maps.Equal(got, want) {
					t.Fatalf("GET /api/pass: price %v, want %v", status["price"], want)
				}
				var out passResponse
				if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusOK || p.stripe.sessionCount() != 1 {
					t.Fatalf("checkout: status %d (%s), %d Stripe sessions; want it sold", code, out.Error, p.stripe.sessionCount())
				}
				checkSessionSent(t, p.stripe.sessions[0], tax)
				p.stripe.mu.Lock()
				queries := slices.Clone(p.stripe.priceQueries)
				p.stripe.mu.Unlock()
				if len(queries) != 1 || !maps.EqualFunc(queries[0], asked, slices.Equal[[]string]) {
					t.Fatalf("the Price was read with %v; want it read once, for the page and checkout both, with %v", queries, asked)
				}
			})
		}
	}

	for _, tax := range []bool{false, true} {
		t.Run(fmt.Sprintf("Stripe Tax %s, an option added in place", map[bool]string{false: "off", true: "on"}[tax]), func(t *testing.T) {
			var logs syncBuffer
			p := passServerLogging(t, func(cfg *Config) { cfg.Pass.StripeAutomaticTax = tax }, &logs)
			p.stripe.mu.Lock()
			p.stripe.price["tax_behavior"] = "inclusive"
			p.stripe.mu.Unlock()
			readAgo := func(age time.Duration) {
				p.app.pass.priceMu.Lock()
				p.app.pass.priceFetchedAt = time.Now().Add(-age)
				p.app.pass.priceMu.Unlock()
			}
			shown := func() *passPrice {
				var out passResponse
				p.get("/api/pass", &out)
				return out.Price
			}
			if price := shown(); price == nil {
				t.Fatal("GET /api/pass left out the price of a Price in one currency")
			}
			p.stripe.mu.Lock()
			p.stripe.price["currency_options"] = map[string]any{
				"usd": own,
				"eur": map[string]any{"unit_amount": 1900, "tax_behavior": "exclusive"},
			}
			p.stripe.mu.Unlock()
			readAgo(59 * time.Minute)
			if price := shown(); price == nil {
				t.Fatal("59 minutes after a good read, the price was read again and left out")
			}
			readAgo(61 * time.Minute)
			if price := shown(); price != nil {
				t.Fatalf("61 minutes after a good read, the Price given another currency is still shown: %+v", price)
			}
			var out passResponse
			if code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); code != http.StatusServiceUnavailable ||
				out.Error != passPriceUnsold || p.stripe.sessionCount() != 0 {
				t.Fatalf("checkout of the Price given another currency: status %d (%s), %d Stripe sessions; want 503 and none",
					code, out.Error, p.stripe.sessionCount())
			}
			if n := strings.Count(logs.String(), "not also in eur"); n != 1 {
				t.Fatalf("%d log lines name the other currency, want 1:\n%s", n, logs.String())
			}
		})
	}

	// The refusal names the other currencies in order, whatever order the
	// options come in. Go ranges over the options' map in no set order, and
	// often in the order they were decoded, which Stripe's sorted keys give
	// here, so one read in order proves nothing: the Price is read 50 times,
	// each a fresh map with many currencies, as a missing sort would not
	// survive.
	t.Run("other currencies named in order", func(t *testing.T) {
		f := newFakeStripe(t)
		codes := []string{"aud", "brl", "cad", "chf", "dkk", "eur", "gbp", "jpy", "nok", "sek", "sgd"}
		options := map[string]any{"usd": own}
		for _, code := range codes {
			options[code] = map[string]any{"unit_amount": 2000, "tax_behavior": "inclusive"}
		}
		f.price["currency_options"] = options
		c := newStripeClient(testPassConfig(f))
		want := errPassPriceRefused.Error() + ": it must be priced in usd alone, not also in " + strings.Join(codes, ", ")
		for read := range 50 {
			if _, err := c.price(context.Background()); err == nil || err.Error() != want {
				t.Fatalf("read %d: %v; want %q", read+1, err, want)
			}
		}
	})
}

// TestPassPriceIsKeptForItsTime holds the price read from Stripe to the times
// passPrice keeps it: a failed read a minute, so that one failure stops
// checkout, with Stripe Tax on or off, for no longer than that (this test runs
// with it on), and a good read an hour, so that Stripe is not asked on every
// page view and checkout, and a Price archived meanwhile is refused within the
// hour, as one given other currencies is
// (TestPassSellsInThePricesOwnCurrencyAlone).
func TestPassPriceIsKeptForItsTime(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.Pass.StripeAutomaticTax = true })
	p.stripe.mu.Lock()
	p.stripe.price["tax_behavior"] = "inclusive"
	p.stripe.fail = true
	p.stripe.mu.Unlock()
	readAgo := func(age time.Duration) {
		p.app.pass.priceMu.Lock()
		p.app.pass.priceFetchedAt = time.Now().Add(-age)
		p.app.pass.priceMu.Unlock()
	}
	checkout := func() (int, passResponse) {
		var out passResponse
		code := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out)
		return code, out
	}
	// shown is the price GET /api/pass shows, if it shows one.
	shown := func() *passPrice {
		var out passResponse
		p.get("/api/pass", &out)
		return out.Price
	}
	if price := shown(); price != nil {
		t.Fatalf("GET /api/pass with Stripe down: %+v", price)
	}
	reads := p.stripe.priceReadCount()
	p.stripe.mu.Lock()
	p.stripe.fail = false
	p.stripe.mu.Unlock()

	// Stripe is back, and the failure is kept until a minute has passed.
	readAgo(59 * time.Second)
	if code, out := checkout(); code != http.StatusBadGateway || out.Error != passStripeUnreachable {
		t.Fatalf("checkout 59 seconds after a failed read: status %d (%s); want 502", code, out.Error)
	}
	if price := shown(); price != nil || p.stripe.priceReadCount() != reads {
		t.Fatalf("59 seconds after a failed read the price was read again (%d reads, want %d), or shown: %+v",
			p.stripe.priceReadCount(), reads, price)
	}
	readAgo(61 * time.Second)
	if code, out := checkout(); code != http.StatusOK || p.stripe.priceReadCount() != reads+1 {
		t.Fatalf("checkout 61 seconds after a failed read: status %d (%s), %d reads; want it sold and the price read again",
			code, out.Error, p.stripe.priceReadCount()-reads)
	}

	// A good read is kept an hour: the Price archived meanwhile is still
	// shown and sold, and refused once the hour is out.
	reads = p.stripe.priceReadCount()
	p.stripe.mu.Lock()
	p.stripe.price["active"] = false
	p.stripe.mu.Unlock()
	readAgo(59 * time.Minute)
	if price := shown(); price == nil || p.stripe.priceReadCount() != reads {
		t.Fatalf("59 minutes after a good read the price was read again (%d reads, want %d), or left out",
			p.stripe.priceReadCount(), reads)
	}
	if code, out := checkout(); code != http.StatusOK {
		t.Fatalf("checkout 59 minutes after a good read: status %d (%s)", code, out.Error)
	}
	readAgo(61 * time.Minute)
	if price := shown(); price != nil || p.stripe.priceReadCount() != reads+1 {
		t.Fatalf("61 minutes after a good read the price was not read again (%d reads, want %d), or the archived Price shown: %+v",
			p.stripe.priceReadCount(), reads+1, price)
	}
	if code, out := checkout(); code != http.StatusServiceUnavailable || out.Error != passPriceUnsold {
		t.Fatalf("checkout of the Price archived, read again: status %d (%s); want 503", code, out.Error)
	}
}

// checkSessionSent holds what checkout sent Stripe to open a session (form) to
// the one Pass it sells, with automatic_tax[enabled] as Stripe Tax is set
// (tax), and nothing else, with Stripe Tax on or off. Stripe learns that a Pass
// was bought and nothing about who on educhan bought it (no metadata,
// client_reference_id or customer), and is asked for nothing about tax but
// whether to collect it. Nor is the buyer let change what they pay: no
// allow_promotion_codes, discounts or adjustable_quantity. Nor is
// adaptive_pricing sent: it came in a later API version than stripeAPIVersion,
// under which Stripe may refuse it, and every checkout with it, so Adaptive
// Pricing is left to the Stripe account, where the README says to switch it
// off. So a paid session paid the Price that checkout read, at its own amount
// in its own currency while Adaptive Pricing is off, and the webhook need not
// check the amount.
func checkSessionSent(t *testing.T, form url.Values, tax bool) {
	t.Helper()
	sent := map[string]string{
		"mode":                    "payment",
		"line_items[0][price]":    testPriceID,
		"line_items[0][quantity]": "1",
		"success_url":             testPublicURL + "/pass?paid",
		"cancel_url":              testPublicURL + "/pass?cancelled",
		"submit_type":             "pay",
		"automatic_tax[enabled]":  strconv.FormatBool(tax),
	}
	for key, want := range sent {
		if got := form.Get(key); got != want {
			t.Errorf("Stripe got %s=%q, want %q", key, got, want)
		}
	}
	for key := range form {
		if _, ok := sent[key]; !ok {
			t.Errorf("Stripe was sent %s=%q", key, form.Get(key))
		}
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
	// Stripe Tax is off, and Stripe is told so, since an account can collect
	// tax by default (TestPassTaxFollowsThePrice).
	checkSessionSent(t, p.stripe.sessions[0], false)
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
	wantExpiry := passExpiry(time.Now(), 365)
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
	// A renewal of a code that is no good is refused before the cooldown,
	// so it costs its holder none.
	var refused passResponse
	if status := p.post("/api/pass/checkout", map[string]any{"adult": true, "renew": "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ"}, &refused); status != http.StatusForbidden ||
		refusalState(t, refused) != passInvalid {
		t.Fatalf("renewing an unknown code: status %d, %+v", status, refused)
	}
	p.checkout("")
	var out passResponse
	if status := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); status != http.StatusTooManyRequests {
		t.Fatalf("second checkout inside the cooldown: status %d", status)
	}

	// A session Stripe fails to open is answered 502 after the cooldown has
	// started, so asking again at once is told to wait. The Price was read
	// before Stripe began to fail, and is kept, so it is the session that
	// fails (TestPassRefusesAPriceItCannotSell has Stripe failing as the
	// Price is read).
	q := passServer(t, func(cfg *Config) { cfg.Pass.CheckoutCooldown = time.Minute })
	var status passResponse
	if q.get("/api/pass", &status); status.Price == nil {
		t.Fatalf("GET /api/pass: %+v; want the price read", status)
	}
	q.stripe.mu.Lock()
	q.stripe.fail = true
	q.stripe.mu.Unlock()
	if status := q.post("/api/pass/checkout", map[string]any{"adult": true}, &out); status != http.StatusBadGateway ||
		out.Error != passStripeUnreachable {
		t.Fatalf("checkout with Stripe down: status %d (%s)", status, out.Error)
	}
	if status, _ := q.claim(); status != http.StatusBadRequest {
		t.Fatalf("a failed checkout left a claim behind: status %d", status)
	}
	q.stripe.mu.Lock()
	q.stripe.fail = false
	q.stripe.mu.Unlock()
	if status := q.post("/api/pass/checkout", map[string]any{"adult": true}, &out); status != http.StatusTooManyRequests ||
		out.RetryAfter != 60 {
		t.Fatalf("checkout again at once after Stripe failed to open a session: status %d, %+v; want 429 for 60 seconds", status, out)
	}
}

// TestPassCheckoutCalledOffLogsNothing holds a checkout the page calls off, by
// a forget or by leaving educhan, to what it is: a client gone, which a.fail
// does not log either, and not a fault. Called off while Stripe is opening its
// session, or while the Pass tables are pruned, it logs nothing at WARN or
// ERROR, the request's own line included, and records no order.
func TestPassCheckoutCalledOffLogsNothing(t *testing.T) {
	for _, during := range []string{"Stripe opening the session", "the prune"} {
		t.Run(during, func(t *testing.T) {
			var logs syncBuffer
			p := passServerLogging(t, nil, &logs)
			// held waits until the checkout is held where it is called off.
			var held func()
			switch during {
			case "Stripe opening the session":
				opening, release := make(chan struct{}, 1), make(chan struct{})
				defer close(release)
				p.stripe.mu.Lock()
				p.stripe.opening = func() {
					opening <- struct{}{}
					<-release
				}
				p.stripe.mu.Unlock()
				held = func() {
					select {
					case <-opening:
					case <-time.After(10 * time.Second):
						t.Fatal("the checkout never asked Stripe for a session")
					}
				}
			case "the prune":
				lock := p.holdPassLock()
				defer lock.release()
				held = func() { lock.queued(1, "the checkout's prune") }
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			answer := p.fire(p.request("/api/pass/checkout", map[string]any{"adult": true}, nil).WithContext(ctx))
			held()
			cancel()
			if got := <-answer; !errors.Is(got.err, context.Canceled) {
				t.Fatalf("the checkout called off: status %d, %v; want it cancelled", got.status, got.err)
			}
			// The request's own line is the last the server writes of it.
			var lines []map[string]any
			for deadline := time.Now().Add(10 * time.Second); ; {
				lines = lines[:0]
				done := false
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					var record map[string]any
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatalf("a log line is not JSON: %q", line)
					}
					lines = append(lines, record)
					done = done || record["msg"] == "request" && record["path"] == "/api/pass/checkout"
				}
				if done {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the server never finished the checkout called off:\n%s", logs.String())
				}
				time.Sleep(5 * time.Millisecond)
			}
			for _, record := range lines {
				if record["level"] == "WARN" || record["level"] == "ERROR" {
					t.Errorf("a checkout called off is logged as a fault: %v", record)
				}
			}
			if n := p.count("pass_orders"); n != 0 {
				t.Fatalf("a checkout called off recorded %d orders", n)
			}
		})
	}
}

func TestPassActivationIsPaced(t *testing.T) {
	p := passServer(t, nil)
	p.app.pass.activateLimiter = newCooldownLimiter(passActivateCooldown)
	if status, _ := p.activate("ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ"); status != http.StatusForbidden {
		t.Fatalf("first check: status %d", status)
	}
	// One check per address every 2 seconds, as README says.
	if status, out := p.activate("ZZZZZ-ZZZZZ-ZZZZZ-ZZZZY"); status != http.StatusTooManyRequests || out.RetryAfter != 2 {
		t.Fatalf("second check at once: status %d, %+v; want 429 for 2 seconds", status, out)
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

// TestPassPaidDeliveredAgainBeforeCollection holds a "paid" event that Stripe
// delivers more than once before the Pass is collected, retried after an
// answer that went astray or simply sent twice, to the order it already
// paid: every delivery is answered 200, the log says nothing of the repeat,
// and the claim collects one Pass. The warning that a paid checkout matched
// no open order is for a checkout that matched none, which the owner checks
// against a receipt; a repeat must never raise it.
func TestPassPaidDeliveredAgainBeforeCollection(t *testing.T) {
	var logs syncBuffer
	p := passServerLogging(t, nil, &logs)
	for _, event := range []string{stripeCheckoutCompleted, stripeCheckoutAsyncSuccess} {
		buyer := p.otherBrowser()
		object := map[string]any{"id": buyer.checkout(""), "object": "checkout.session", "mode": "payment",
			"payment_status": "paid", "payment_intent": "pi_delivered_again"}
		if event == stripeCheckoutAsyncSuccess {
			object["payment_intent"] = "pi_delivered_again_slowly"
			object["payment_status"] = "unpaid"
			if status := buyer.webhook(stripeCheckoutCompleted, object); status != http.StatusOK {
				t.Fatalf("the slow payment's checkout completed: status %d", status)
			}
			object["payment_status"] = "paid"
		}
		passes := p.count("passes")
		for i := range 2 {
			if status := buyer.webhook(event, object); status != http.StatusOK {
				t.Fatalf("%s, delivery %d: status %d", event, i+1, status)
			}
		}
		if status, out := buyer.claim(); status != http.StatusOK || out.Status != "issued" || out.Claim {
			t.Fatalf("%s delivered twice, then claimed: status %d, %+v; want one Pass", event, status, out)
		}
		if n := p.count("passes"); n != passes+1 {
			t.Fatalf("%s delivered twice made %d Passes, want 1", event, n-passes)
		}
	}
	if out := logs.String(); strings.Contains(out, "matched no open Pass order") {
		t.Fatalf("a paid event delivered again before its Pass was collected was logged as matching no order:\n%s", out)
	}
}

// TestPassCheckoutGoesOnlyToStripe holds the page a checkout sends the buyer
// to to Stripe's own checkout host. A session URL that comes back as anything
// else is refused before an order or a claim is made.
func TestPassCheckoutGoesOnlyToStripe(t *testing.T) {
	p := passServer(t, nil)
	for _, page := range []string{
		"http://checkout.stripe.com/c/pay/cs_test_x",
		"https://checkout.stripe.com.example.test/c/pay/cs_test_x",
		"https://pay.example.test/c/pay/cs_test_x",
		"https://someone@checkout.stripe.com/c/pay/cs_test_x",
		"//checkout.stripe.com/c/pay/cs_test_x",
		"https://checkout.stripe.com:8443/c/pay/cs_test_x",
	} {
		p.stripe.mu.Lock()
		p.stripe.sessionURL = page
		p.stripe.mu.Unlock()
		var out passResponse
		if status := p.post("/api/pass/checkout", map[string]any{"adult": true}, &out); status != http.StatusBadGateway || out.URL != "" {
			t.Errorf("a session on %s: status %d, %+v", page, status, out)
		}
	}
	if n := p.count("pass_orders"); n != 0 {
		t.Fatalf("refused sessions left %d orders", n)
	}
	if p.reportsClaim() {
		t.Fatal("a refused session left a claim in the browser")
	}
}

// reportsClaim asks GET /api/pass whether this harness's browser has a
// purchase to collect.
func (p *passHarness) reportsClaim() bool {
	p.t.Helper()
	var out passResponse
	p.get("/api/pass", &out)
	return out.Claim
}

// claimCookie reads the claim cookie in this harness's browser.
func (p *passHarness) claimCookie() (string, bool) {
	p.t.Helper()
	u, err := url.Parse(p.base + passCookiePath)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, c := range p.http.Jar.Cookies(u) {
		if c.Name == passClaimCookie {
			return c.Value, true
		}
	}
	return "", false
}

// TestPassCollectsOnlyThroughItsOwnClaim holds every order to the claim its
// own checkout set: a second checkout gives the browser a new claim with only
// its own order under it, and nothing from one checkout is ever added to what
// another buys, whoever paid for it. A payment on an earlier checkout is no
// longer collected by that browser; it is still its buyer's, and a reissue
// code from its receipt makes it a Pass of its own.
func TestPassCollectsOnlyThroughItsOwnClaim(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	if p.reportsClaim() {
		t.Fatal("a browser that bought nothing has a claim")
	}

	first := p.checkout("")
	cookie, _ := p.claimCookie()
	second := p.checkout("")
	if again, _ := p.claimCookie(); again == cookie {
		t.Fatal("a second checkout kept the browser's old claim token")
	}
	var claims, orders int
	if err := p.app.pool.QueryRow(context.Background(), `
		SELECT count(DISTINCT claim_hash), count(*) FROM pass_orders`).Scan(&claims, &orders); err != nil || claims != 2 || orders != 2 {
		t.Fatalf("two checkouts in one browser made %d orders under %d claims (%v), want one under each of 2", orders, claims, err)
	}
	// The claim index is unique, which is what holds a claim to one order:
	// claimPass reads, and deletes, by the claim alone.
	if _, err := p.app.pool.Exec(context.Background(), `
		INSERT INTO pass_orders (session_hash, claim_hash, days, expires_at)
		SELECT '\xee', claim_hash, days, expires_at FROM pass_orders LIMIT 1`); !isUniqueViolation(err) {
		t.Fatalf("a second order under one claim: %v, want a unique violation", err)
	}
	if !p.reportsClaim() {
		t.Fatal("GET /api/pass did not say the browser has a purchase to collect")
	}

	// The second tab is paid: its Pass is issued, and nothing is left to
	// collect here.
	p.paid(second, "pi_second")
	code, issued := p.claim()
	if want := passExpiry(time.Now(), 365); code != http.StatusOK || issued.Status != "issued" || issued.Claim || !issued.ExpiresAt.Equal(want) {
		t.Fatalf("claim of the latest checkout: status %d, %+v; want a Pass to %v", code, issued, want)
	}
	// The first tab, paid later, is not collected here and changes nothing
	// about the Pass the second one made.
	p.paid(first, "pi_first")
	if p.reportsClaim() {
		t.Fatal("the browser is told to collect an earlier checkout's payment")
	}
	if code, out := p.claim(); code != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("claim after the earlier tab was paid: status %d, %+v", code, out)
	}
	if _, ok := p.claimCookie(); !ok {
		t.Fatal("a claim with nothing to collect took the claim cookie out of the browser")
	}
	if got, _ := p.passExpiresAt(issued.Pass); !got.Equal(issued.ExpiresAt) {
		t.Fatalf("an earlier checkout's payment moved the Pass to %v, want %v", got, issued.ExpiresAt)
	}
	// It is still its buyer's: a reissue code from its receipt makes it a
	// Pass of its own, and the first Pass goes on as it was.
	status, _, token := p.reissueCode("mod-key", "pi_first")
	if status != http.StatusOK {
		t.Fatalf("reissue of the earlier tab's payment: status %d", status)
	}
	code, own := p.otherBrowser().redeem(token)
	if code != http.StatusOK || own.Status != "issued" || own.Pass == issued.Pass || !own.ExpiresAt.Equal(passExpiry(time.Now(), 365)) {
		t.Fatalf("redeem of the earlier tab's payment: status %d, %+v", code, own)
	}
	for _, c := range []string{issued.Pass, own.Pass} {
		if status, _ := p.activate(c); status != http.StatusOK {
			t.Fatalf("code %s after both were collected: status %d", c, status)
		}
	}

	// At a shared computer, somebody starts a checkout, keeps its Stripe page
	// and leaves; the next person buys there, and the first pays from
	// elsewhere before the second collects. The buyer's Pass is made from the
	// buyer's payment alone, and the first payment's receipt reissues a Pass
	// of its own, never the buyer's.
	shared := p.otherBrowser()
	left := shared.checkout("")
	bought := shared.checkout("")
	shared.paid(left, "pi_left_open")
	shared.paid(bought, "pi_buyer")
	code, buyer := shared.claim()
	if code != http.StatusOK || buyer.Status != "issued" || !buyer.ExpiresAt.Equal(passExpiry(time.Now(), 365)) {
		t.Fatalf("the buyer's claim: status %d, %+v", code, buyer)
	}
	var origin bool
	var payments int
	if err := p.app.pool.QueryRow(context.Background(), `
		SELECT bool_and(origin = 'bought' AND payment_hash = $2), count(*) FROM pass_payments
		WHERE pass_id = (SELECT id FROM passes WHERE token_hash = $1)`,
		p.app.passTokenHash(strings.ReplaceAll(buyer.Pass, "-", "")), p.app.passPaymentHash("pi_buyer")).Scan(&origin, &payments); err != nil ||
		!origin || payments != 1 {
		t.Fatalf("the buyer's Pass has %d payments, the buyer's its only origin: %v (%v)", payments, origin, err)
	}
	status, _, token = p.reissueCode("mod-key", "pi_left_open")
	if status != http.StatusOK {
		t.Fatalf("reissue of the payment left open: status %d", status)
	}
	if code, out := p.otherBrowser().redeem(token); code != http.StatusOK || out.Status != "issued" || out.Pass == buyer.Pass {
		t.Fatalf("redeem of the payment left open: status %d, %+v", code, out)
	}
	if status, _ := p.activate(buyer.Pass); status != http.StatusOK {
		t.Fatalf("another payment's reissue stopped the buyer's code: status %d", status)
	}
	if status, _, _ := p.reissueCode("mod-key", "pi_buyer"); status != http.StatusOK {
		t.Fatalf("the buyer's own receipt: status %d", status)
	}

	// A bank debit is still clearing when the next person buys at the same
	// browser. Its money renews nobody's Pass: once cleared, its receipt
	// reissues a Pass of its own, and the next person's is as they bought it.
	library := p.otherBrowser()
	slow := map[string]any{"id": library.checkout(""), "object": "checkout.session", "mode": "payment",
		"payment_status": "unpaid", "payment_intent": "pi_debit_clearing"}
	library.webhook(stripeCheckoutCompleted, slow)
	next := library.buy("pi_next_person")
	slow["payment_status"] = "paid"
	library.webhook(stripeCheckoutAsyncSuccess, slow)
	if library.reportsClaim() {
		t.Fatal("the next person is told to collect the cleared debit")
	}
	status, _, token = p.reissueCode("mod-key", "pi_debit_clearing")
	if status != http.StatusOK {
		t.Fatalf("reissue of the cleared debit: status %d", status)
	}
	if code, out := p.otherBrowser().redeem(token); code != http.StatusOK || out.Status != "issued" || out.Pass == next.Pass {
		t.Fatalf("redeem of the cleared debit: status %d, %+v", code, out)
	}
	if got, _ := p.passExpiresAt(next.Pass); !got.Equal(next.ExpiresAt) {
		t.Fatalf("the cleared debit moved the next person's Pass to %v, want %v", got, next.ExpiresAt)
	}

	// A cookie this server could not have made is no claim, and a checkout
	// replaces it.
	u, _ := url.Parse(p.base + passCookiePath)
	p.http.Jar.SetCookies(u, []*http.Cookie{{Name: passClaimCookie, Value: "not-a-claim", Path: passCookiePath}})
	if p.reportsClaim() {
		t.Fatal("a malformed claim cookie was reported as a claim")
	}
	p.checkout("")
	if value, _ := p.claimCookie(); value == "not-a-claim" {
		t.Fatal("a checkout kept a claim cookie this server could not have made")
	}
}

// TestPassOrderDeadlineIsAMidnight holds an order's deadline to the day, so
// the row does not date its checkout to the microsecond.
func TestPassOrderDeadlineIsAMidnight(t *testing.T) {
	p := passServer(t, nil)
	before := time.Now()
	p.checkout("")
	var deadline time.Time
	if err := p.app.pool.QueryRow(context.Background(), `SELECT expires_at FROM pass_orders`).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	if !withinAWeek(deadline, before) {
		t.Fatalf("order deadline %v: want the first UTC midnight a week on", deadline.UTC())
	}
}

// TestPassPaidAfterItsDeadlineIsKept holds a paid order to its payment: paid
// after its deadline, or paid in time and collected late, it is still a Pass,
// and still more to come beside another claim's. Only an order nobody paid for
// lapses, and once it has, neither GET /api/pass nor a claim's answer counts
// it as waiting, though nothing has deleted it yet.
func TestPassPaidAfterItsDeadlineIsKept(t *testing.T) {
	p := passServer(t, nil)
	session := p.checkout("")
	p.exec(`UPDATE pass_orders SET expires_at = $1`, time.Now().Add(-time.Hour))
	p.paid(session, "pi_paid_late")
	if !p.reportsClaim() {
		t.Fatal("GET /api/pass does not tell the page to collect an order paid after its deadline")
	}
	if status, out := p.claim(); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("an order paid after its deadline: status %d, %+v", status, out)
	}

	session = p.checkout("")
	p.paid(session, "pi_collected_late")
	p.exec(`UPDATE pass_orders SET expires_at = $1`, time.Now().Add(-time.Hour))
	if !p.reportsClaim() {
		t.Fatal("GET /api/pass does not tell the page to collect a paid order past its deadline")
	}
	if status, out := p.claim(); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("a paid order collected after its deadline: status %d, %+v", status, out)
	}

	// An unpaid order past its deadline is nothing to collect, even before a
	// prune or a claim deletes it.
	p.checkout("")
	if !p.reportsClaim() {
		t.Fatal("GET /api/pass does not tell the page to collect an order inside its deadline")
	}
	p.exec(`UPDATE pass_orders SET expires_at = $1`, time.Now().Add(-time.Hour))
	if p.reportsClaim() {
		t.Fatal("GET /api/pass tells the page to collect an unpaid order past its deadline")
	}
	if status, out := p.claim(); status != http.StatusBadRequest || out.Claim {
		t.Fatalf("an unpaid order past its deadline: status %d, %+v", status, out)
	}
	if n := p.count("pass_orders"); n != 0 {
		t.Fatalf("a lapsed unpaid order was kept: %d rows", n)
	}

	// Nor is it more to come beside a paid order under another claim cookie,
	// which a claim collects first and stops at, before it reaches the lapsed
	// one to delete it.
	paid := p.otherBrowser()
	paid.paid(paid.checkout(""), "pi_beside_a_lapsed_order")
	paidToken, _ := paid.claimCookie()
	lapsed := p.otherBrowser()
	session = lapsed.checkout("")
	lapsedToken, _ := lapsed.claimCookie()
	p.exec(`UPDATE pass_orders SET expires_at = $1 WHERE session_hash = $2`, time.Now().Add(-time.Hour), p.app.passSessionHash(session))
	status, out := paid.sendingClaimCookies(paidToken, lapsedToken).claim()
	if status != http.StatusOK || out.Status != "issued" || out.Claim {
		t.Fatalf("a paid claim beside a lapsed one: status %d, %+v; want the Pass and nothing more to come", status, out)
	}
	// The claim stopped at the paid order, so the lapsed one is still there,
	// and only its deadline keeps it out of what is waiting.
	if n := p.count("pass_orders"); n != 1 {
		t.Fatalf("%d orders after the paid claim, want the lapsed one still there", n)
	}
	if lapsed.reportsClaim() {
		t.Fatal("GET /api/pass tells the page to collect the lapsed order")
	}

	// A paid order past its deadline under another claim cookie is more to
	// come, though: the answer that collects the first paid order says so, and
	// the next claim, with the same nonce, collects it.
	first := p.otherBrowser()
	first.paid(first.checkout(""), "pi_first_beside_a_late_one")
	firstToken, _ := first.claimCookie()
	late := p.otherBrowser()
	session = late.checkout("")
	late.paid(session, "pi_collected_late_beside")
	lateToken, _ := late.claimCookie()
	p.exec(`UPDATE pass_orders SET expires_at = $1 WHERE session_hash = $2`, time.Now().Add(-time.Hour), p.app.passSessionHash(session))
	both := first.sendingClaimCookies(firstToken, lateToken)
	nonce := both.collectNonce()
	status, one := both.claimWith(nonce)
	if status != http.StatusOK || one.Status != "issued" || !one.Claim {
		t.Fatalf("a paid claim beside one paid and past its deadline: status %d, %+v; want the Pass and more to come", status, one)
	}
	status, other := both.claimWith(nonce)
	if status != http.StatusOK || other.Status != "issued" || other.Claim || other.Pass == one.Pass {
		t.Fatalf("the paid claim past its deadline, asked for next: status %d, %+v; want another Pass and nothing more", status, other)
	}
}

// claimSetCookie collects in this harness's browser and returns the claim
// cookie the answer set.
func (p *passHarness) claimSetCookie() (int, *http.Cookie) {
	p.t.Helper()
	res, err := p.http.Do(p.claimRequest())
	if err != nil {
		p.t.Fatal(err)
	}
	defer res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == passClaimCookie {
			return res.StatusCode, c
		}
	}
	return res.StatusCode, nil
}

// TestPassSlowPaymentIsWaitedFor holds a checkout completed with a payment
// that takes days to clear, a bank debit say, until it does: neither the
// order nor the browser's claim lapses at the week a card payment gets. The
// claim cookie is set for that long at checkout, since only a webhook holds
// the order and a buyer may not come back until the debit has cleared. A
// report that the checkout completed unpaid, arriving after the payment did,
// holds nothing.
func TestPassSlowPaymentIsWaitedFor(t *testing.T) {
	p := passServer(t, nil)
	session, cookie := p.checkoutSetCookie("")
	if cookie == nil || time.Duration(cookie.MaxAge)*time.Second < stripeCheckoutTTL+passPendingPaymentTTL-time.Minute {
		t.Fatalf("the checkout's claim cookie %v lapses before a slow payment's hold could", cookie)
	}
	object := map[string]any{"id": session, "object": "checkout.session", "mode": "payment",
		"payment_status": "unpaid", "payment_intent": "pi_debit"}
	if status := p.webhook(stripeCheckoutCompleted, object); status != http.StatusOK {
		t.Fatalf("completed webhook: status %d", status)
	}
	var held time.Time
	if err := p.app.pool.QueryRow(context.Background(), `SELECT expires_at FROM pass_orders`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if !held.Equal(dayCeil(held)) || held.Before(time.Now().Add(passPendingPaymentTTL-time.Minute)) {
		t.Fatalf("a checkout waiting on its payment is held until %v, want a midnight %v on", held.UTC(), passPendingPaymentTTL)
	}
	// The cookie set at checkout outlives the hold, and asking after the
	// purchase neither sets nor clears it.
	if time.Now().Add(time.Duration(cookie.MaxAge) * time.Second).Before(held) {
		t.Fatalf("the claim cookie lapses before the order it holds (%v)", held.UTC())
	}
	if status, set := p.claimSetCookie(); status != http.StatusAccepted || set != nil {
		t.Fatalf("claim while the payment clears: status %d, cookie %v", status, set)
	}
	// Only forgetting the claim clears it: a claim told to wait, or refused
	// for its body, leaves it in the browser to collect with.
	var refused passResponse
	if status := p.post("/api/pass/claim", map[string]any{}, &refused); status != http.StatusBadRequest {
		t.Fatalf("claim with no nonce: status %d", status)
	}
	if value, ok := p.claimCookie(); !ok || value != cookie.Value {
		t.Fatal("a claim answered 202 or 400 took the claim cookie out of the browser")
	}
	// A week and a day on, the debit has not cleared yet and the order stays.
	if err := p.app.store.prunePasses(context.Background(), time.Now().AddDate(0, 0, 8)); err != nil {
		t.Fatal(err)
	}
	if n := p.count("pass_orders"); n != 1 {
		t.Fatalf("an order waiting on a slow payment was pruned at its week: %d rows", n)
	}
	object["payment_status"] = "paid"
	if status := p.webhook(stripeCheckoutAsyncSuccess, object); status != http.StatusOK {
		t.Fatalf("async webhook: status %d", status)
	}
	if status, out := p.claim(); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("claim after the debit cleared: status %d, %+v", status, out)
	}

	// A debit that fails can never be paid, and its order goes at once.
	session = p.checkout("")
	object = map[string]any{"id": session, "object": "checkout.session", "mode": "payment",
		"payment_status": "unpaid", "payment_intent": "pi_bounced"}
	p.webhook(stripeCheckoutCompleted, object)
	if status := p.webhook(stripeCheckoutAsyncFailure, object); status != http.StatusOK {
		t.Fatalf("async failure webhook: status %d", status)
	}
	if status, out := p.claim(); status != http.StatusBadRequest || out.Claim {
		t.Fatalf("claim after the debit failed: status %d, %+v", status, out)
	}

	// Stripe does not order its deliveries: a debit's success can arrive
	// before a retried report that its checkout completed unpaid. The late
	// report holds nothing, so the paid order keeps the deadline it had, and
	// goes its own term past that, as any paid order nobody collected does.
	session = p.checkout("")
	object = map[string]any{"id": session, "object": "checkout.session", "mode": "payment",
		"payment_status": "paid", "payment_intent": "pi_debit_reported_late"}
	if status := p.webhook(stripeCheckoutAsyncSuccess, object); status != http.StatusOK {
		t.Fatalf("async webhook ahead of the completed one: status %d", status)
	}
	order := func() (time.Time, int) {
		t.Helper()
		var deadline time.Time
		var days int
		if err := p.app.pool.QueryRow(context.Background(), `
			SELECT expires_at, days FROM pass_orders WHERE session_hash = $1 AND payment_hash IS NOT NULL`,
			p.app.passSessionHash(session)).Scan(&deadline, &days); err != nil {
			t.Fatalf("the paid order: %v", err)
		}
		return deadline, days
	}
	deadline, days := order()
	object["payment_status"] = "unpaid"
	if status := p.webhook(stripeCheckoutCompleted, object); status != http.StatusOK {
		t.Fatalf("completed webhook after the payment: status %d", status)
	}
	if after, _ := order(); !after.Equal(deadline) {
		t.Fatalf("a late report of the checkout completed unpaid moved the paid order's deadline from %v to %v",
			deadline.UTC(), after.UTC())
	}
	if err := p.app.store.prunePasses(context.Background(), deadline.Add(time.Duration(days)*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var kept bool
	if err := p.app.pool.QueryRow(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM pass_orders WHERE session_hash = $1)`, p.app.passSessionHash(session)).Scan(&kept); err != nil || kept {
		t.Fatalf("the paid order outlived its term past its deadline (%v)", err)
	}
}

// TestPassExpiredCheckoutIsDropped holds an order to its Stripe session: one
// Stripe closed unpaid is dropped, so the browser stops waiting for it, and
// no other order goes with it.
func TestPassExpiredCheckoutIsDropped(t *testing.T) {
	p := passServer(t, nil)
	expired := func(session string) {
		t.Helper()
		if status := p.webhook(stripeCheckoutExpired, map[string]any{"id": session, "object": "checkout.session",
			"mode": "payment", "status": "expired", "payment_status": "unpaid"}); status != http.StatusOK {
			t.Fatalf("expired webhook: status %d", status)
		}
	}
	abandoned := p.checkout("")
	open := p.checkout("")
	expired(abandoned)
	if n := p.count("pass_orders"); n != 1 {
		t.Fatalf("%d orders left after one of two checkouts expired, want 1", n)
	}
	if status, out := p.claim(); status != http.StatusAccepted || !out.Claim {
		t.Fatalf("claim with one checkout expired and one open: status %d, %+v", status, out)
	}
	expired(open)
	if status, out := p.claim(); status != http.StatusBadRequest || out.Claim {
		t.Fatalf("claim with every checkout expired: status %d, %+v", status, out)
	}
	if p.reportsClaim() {
		t.Fatal("the claim outlived its expired checkouts")
	}

	// An expiry never takes a paid order with it.
	paid := p.checkout("")
	p.paid(paid, "pi_paid_then_expired")
	expired(paid)
	if status, out := p.claim(); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("a paid order after an expiry event: status %d, %+v", status, out)
	}
}

// TestPassRenewalOfAPassThatIsGone answers a renewal of a Pass that cannot be
// renewed any more as such, where it once failed the order's foreign key with
// a 500 after Stripe had opened a checkout.
func TestPassRenewalOfAPassThatIsGone(t *testing.T) {
	p := passServer(t, nil)

	// Past its grace month (30 days): refused before Stripe is asked for
	// anything.
	lapsed := p.buy("pi_lapsed_long_ago")
	p.exec(`UPDATE passes SET expires_at = $1`, time.Now().Add(-30*24*time.Hour-time.Hour))
	var out passResponse
	if status := p.post("/api/pass/checkout", map[string]any{"adult": true, "renew": lapsed.Pass}, &out); status != http.StatusForbidden ||
		refusalState(t, out) != passInvalid {
		t.Fatalf("renewing a Pass past its grace month: status %d, %+v", status, out)
	}
	if n := p.stripe.sessionCount(); n != 1 {
		t.Fatalf("Stripe opened %d sessions, want only the purchase's", n)
	}

	// Refunded while Stripe was opening the renewal's checkout.
	live := p.buy("pi_refunded_meanwhile")
	before, _ := p.claimCookie()
	var revokeErr error
	p.stripe.mu.Lock()
	p.stripe.opening = func() {
		_, revokeErr = p.app.store.revokePassPayment(context.Background(), p.app.passPaymentHash("pi_refunded_meanwhile"), time.Now())
	}
	p.stripe.mu.Unlock()
	status := p.post("/api/pass/checkout", map[string]any{"adult": true, "renew": live.Pass}, &out)
	if revokeErr != nil {
		t.Fatal(revokeErr)
	}
	if status != http.StatusConflict || out.Pass != passInvalid || !strings.Contains(out.Error, "buy a new one") {
		t.Fatalf("renewal of a Pass refunded mid-checkout: status %d, %+v", status, out)
	}
	if n := p.count("pass_orders"); n != 0 {
		t.Fatalf("a renewal of a refunded Pass left %d orders", n)
	}
	if after, _ := p.claimCookie(); after != before || p.reportsClaim() {
		t.Fatal("a refused renewal gave the browser a claim")
	}

	// Run past its grace month while Stripe was opening the checkout, but not
	// pruned yet: the order is refused all the same.
	ageing := p.buy("pi_aged_meanwhile")
	p.stripe.mu.Lock()
	p.stripe.opening = func() {
		_, revokeErr = p.app.pool.Exec(context.Background(), `UPDATE passes SET expires_at = $1 WHERE token_hash = $2`,
			time.Now().Add(-30*24*time.Hour-time.Minute), p.app.passTokenHash(strings.ReplaceAll(ageing.Pass, "-", "")))
	}
	p.stripe.mu.Unlock()
	status = p.post("/api/pass/checkout", map[string]any{"adult": true, "renew": ageing.Pass}, &out)
	if revokeErr != nil {
		t.Fatal(revokeErr)
	}
	if status != http.StatusConflict || out.Pass != passInvalid {
		t.Fatalf("renewal of a Pass that ran past its grace month mid-checkout: status %d, %+v", status, out)
	}
	if n := p.count("pass_orders"); n != 0 {
		t.Fatalf("a renewal of a lapsed Pass left %d orders", n)
	}

	// The foreign key itself is read as the same refusal.
	_, err := p.app.pool.Exec(context.Background(), `
		INSERT INTO pass_orders (claim_hash, session_hash, renew_pass_id, days, expires_at)
		VALUES ('\x00', '\x00', 999999999, 365, now())`)
	if !isForeignKeyViolation(err) {
		t.Fatalf("an order for a missing Pass: %v, want a foreign key violation", err)
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

	// Refunded after paying and before collecting: nothing to collect. The
	// order goes with the refund, so the browser is told at once that
	// nothing waits, and the refunded payment's hash is not kept beside a
	// claim until a claim from that browser, which may never come.
	session := p.checkout("")
	p.paid(session, "pi_uncollected")
	p.webhook(stripeChargeRefunded, map[string]any{"object": "charge", "payment_intent": "pi_uncollected", "refunded": true})
	if p.reportsClaim() {
		t.Fatal("the browser is still told to collect a refunded order")
	}
	if n := p.count("pass_orders"); n != 0 {
		t.Fatalf("the refunded order was kept: %d rows", n)
	}
	if status, _ := p.claim(); status != http.StatusBadRequest {
		t.Fatalf("a refunded order could still be collected: status %d", status)
	}

	// The refund arrives ahead of a retried "paid": the late event must not
	// turn the refunded purchase into a Pass, and the order, which can never
	// be paid now, goes, so the browser stops waiting for it.
	session = p.checkout("")
	p.webhook(stripeChargeRefunded, map[string]any{"object": "charge", "payment_intent": "pi_early", "refunded": true})
	p.paid(session, "pi_early")
	if p.reportsClaim() {
		t.Fatal("the browser is still told to wait for a refunded payment")
	}
	if status, out := p.claim(); status != http.StatusBadRequest || out.Claim {
		t.Fatalf("a late paid event for a refunded payment: status %d, %+v", status, out)
	}
	if n := p.count("pass_orders"); n != 0 {
		t.Fatalf("the refunded payment's order was kept: %d rows", n)
	}
}

func TestPassRenewal(t *testing.T) {
	p := passServer(t, nil)
	issued := p.buy("pi_first")
	// Bring the expiry close, so the renewal is seen to add to it.
	soon := dayCeil(time.Now().AddDate(0, 0, 10))
	p.exec(`UPDATE passes SET expires_at = $1`, soon)

	var out passResponse
	if status := p.post("/api/pass/checkout", map[string]any{"adult": true, "renew": "AAAAA-AAAAA-AAAAA-AAAAA"}, &out); status != http.StatusForbidden {
		t.Fatalf("renewing an unknown code: status %d", status)
	}
	// A renewal is a sale like any other: without the 18+ box it is refused
	// before Stripe is asked for anything.
	sessions := p.stripe.sessionCount()
	for _, body := range []map[string]any{{"renew": issued.Pass}, {"adult": false, "renew": issued.Pass}} {
		if status := p.post("/api/pass/checkout", body, &out); status != http.StatusBadRequest || out.Error != passAdultRequired {
			t.Fatalf("renewing without the 18+ box: status %d, %+v", status, out)
		}
	}
	if n := p.stripe.sessionCount() - sessions; n != 0 {
		t.Fatalf("Stripe opened %d sessions for a renewal without the 18+ box", n)
	}

	p.paid(p.checkout(issued.Pass), "pi_renewal")
	status, renewed := p.claim()
	if status != http.StatusOK || renewed.Status != "renewed" || renewed.Pass != "" {
		t.Fatalf("renewal claim: status %d, %+v", status, renewed)
	}
	// A Pass that has not run out gains exactly the term: its expiry is on a
	// midnight already, so there is nothing to round.
	if want := soon.AddDate(0, 0, 365); !renewed.ExpiresAt.Equal(want) {
		t.Fatalf("renewed to %v, want %v", renewed.ExpiresAt, want)
	}
	if status, active := p.activate(issued.Pass); status != http.StatusOK || !active.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatalf("the renewed code: status %d, %+v", status, active)
	}

	// An expired Pass can still be renewed within its grace month, from now.
	// Checked as typed, in lower case and without its dashes, it is refused
	// with the code as the server writes it and the day it ran out, so the
	// page can keep it to renew.
	ranOut := dayCeil(time.Now().Add(-72 * time.Hour))
	p.exec(`UPDATE passes SET expires_at = $1`, ranOut)
	typed := strings.ToLower(strings.ReplaceAll(issued.Pass, "-", ""))
	if status, out := p.activate(typed); status != http.StatusForbidden || refusalState(t, out) != passExpired ||
		out.Code != issued.Pass || !out.ExpiresAt.Equal(ranOut) {
		t.Fatalf("an expired Pass: status %d, %+v", status, out)
	}
	p.paid(p.checkout(issued.Pass), "pi_late_renewal")
	if status, out := p.claim(); status != http.StatusOK ||
		!out.ExpiresAt.Equal(dayCeil(time.Now().AddDate(0, 0, 365))) {
		t.Fatalf("renewing an expired Pass: status %d, %+v", status, out)
	}
	if status, _ := p.activate(issued.Pass); status != http.StatusOK {
		t.Fatalf("the code after renewing an expired Pass: status %d", status)
	}
}

// passExpiresAt reads a Pass's stored expiry by its code.
func (p *passHarness) passExpiresAt(code string) (time.Time, bool) {
	p.t.Helper()
	canonical, _ := parsePassCode(code)
	_, expires, err := p.app.store.passByToken(context.Background(), p.app.passTokenHash(canonical))
	if err != nil {
		return time.Time{}, false
	}
	return expires, true
}

func (p *passHarness) refund(paymentIntent string) {
	p.t.Helper()
	if status := p.webhook(stripeChargeRefunded, map[string]any{
		"object": "charge", "payment_intent": paymentIntent, "refunded": true,
	}); status != http.StatusOK {
		p.t.Fatalf("refund webhook: status %d", status)
	}
}

func (p *passHarness) renew(code, paymentIntent string) passResponse {
	p.t.Helper()
	p.paid(p.checkout(code), paymentIntent)
	status, out := p.claim()
	if status != http.StatusOK || out.Status != "renewed" {
		p.t.Fatalf("renewal claim: status %d, %+v", status, out)
	}
	return out
}

// TestPassRefundTakesOffOneTerm holds a refund or a dispute to the payment it
// is for: it takes that payment's term off the Pass, and a Pass is cancelled
// only when nothing of its time is left.
func TestPassRefundTakesOffOneTerm(t *testing.T) {
	p := passServer(t, nil)
	issued := p.buy("pi_bought")
	bought, _ := p.passExpiresAt(issued.Pass)
	p.renew(issued.Pass, "pi_renewed")
	p.renew(issued.Pass, "pi_renewed_again")
	if got, _ := p.passExpiresAt(issued.Pass); !got.Equal(bought.AddDate(0, 0, 730)) {
		t.Fatalf("two renewals took the Pass to %v, want %v", got, bought.AddDate(0, 0, 730))
	}

	// Refunding the middle renewal takes its year off and leaves the rest.
	p.refund("pi_renewed")
	if got, ok := p.passExpiresAt(issued.Pass); !ok || !got.Equal(bought.AddDate(0, 0, 365)) {
		t.Fatalf("after refunding one renewal the Pass runs to %v (kept: %v), want %v", got, ok, bought.AddDate(0, 0, 365))
	}
	// Stripe delivers events more than once; a repeat takes nothing more.
	p.refund("pi_renewed")
	if got, _ := p.passExpiresAt(issued.Pass); !got.Equal(bought.AddDate(0, 0, 365)) {
		t.Fatalf("a repeated refund took more off: %v", got)
	}

	// Refunding the purchase itself leaves the renewal paid after it.
	p.refund("pi_bought")
	if status, _ := p.activate(issued.Pass); status != http.StatusOK {
		t.Fatalf("refunding the purchase wiped the renewal paid after it: status %d", status)
	}
	// The last term refunded, nothing is left, and the Pass goes with it.
	p.refund("pi_renewed_again")
	if status, out := p.activate(issued.Pass); status != http.StatusForbidden || refusalState(t, out) != passInvalid {
		t.Fatalf("a Pass with every term refunded still works: status %d, %+v", status, out)
	}
	var left int
	if err := p.app.pool.QueryRow(context.Background(), `SELECT count(*) FROM pass_payments`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("payments of a cancelled Pass left behind: %d (%v)", left, err)
	}

	// Somebody who renews another person's Pass and then disputes it takes
	// back only what they gave.
	victim := p.buy("pi_victim")
	before, _ := p.passExpiresAt(victim.Pass)
	p.renew(victim.Pass, "pi_gift")
	p.webhook(stripeDisputeCreated, map[string]any{"object": "dispute", "payment_intent": "pi_gift"})
	if got, ok := p.passExpiresAt(victim.Pass); !ok || !got.Equal(before) {
		t.Fatalf("a disputed renewal left the Pass at %v (kept: %v), want %v as before it", got, ok, before)
	}
	if status, _ := p.activate(victim.Pass); status != http.StatusOK {
		t.Fatalf("a disputed renewal cost the holder their Pass: status %d", status)
	}

	// A renewal of a Pass that had run out, refunded, leaves nothing over: the
	// part of a day the expiry was rounded up by goes with it.
	lapsed := p.buy("pi_lapsed")
	p.exec(`UPDATE passes SET expires_at = $1 WHERE token_hash = $2`,
		dayCeil(time.Now()).AddDate(0, 0, -3), p.app.passTokenHash(strings.ReplaceAll(lapsed.Pass, "-", "")))
	p.renew(lapsed.Pass, "pi_revival")
	p.refund("pi_revival")
	if status, _ := p.activate(lapsed.Pass); status != http.StatusForbidden {
		t.Fatalf("a refunded renewal of a lapsed Pass left it working: status %d", status)
	}
}

// reissueCode asks for a reissue code as the owner, and returns the status,
// the answer and the code, as the owner would send it on.
func (p *passHarness) reissueCode(key, paymentIntent string) (int, passResponse, string) {
	p.t.Helper()
	var out passResponse
	status := p.postWithHeaders(http.Header{modKeyHeader: {key}}, "/api/pass/reissue",
		map[string]any{"payment_intent": paymentIntent}, &out)
	if status != http.StatusOK {
		return status, out, ""
	}
	return status, out, out.Code
}

// otherBrowser is a browser with no cookies of this harness's, as a buyer
// typing a reissue code somewhere else would be.
func (p *passHarness) otherBrowser() *passHarness {
	p.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		p.t.Fatal(err)
	}
	h := &harness{client: &client{t: p.t, base: p.base, http: &http.Client{Jar: jar}}, app: p.app}
	return &passHarness{harness: h, stripe: p.stripe}
}

// redeem types a reissue code on the Pass page, which sends it in the claim's
// body.
func (p *passHarness) redeem(token string) (int, passResponse) {
	p.t.Helper()
	status, out := p.redeemWith(token, p.collectNonce())
	p.keepNonce(status, out)
	return status, out
}

func (p *passHarness) redeemWith(token, nonce string) (int, passResponse) {
	p.t.Helper()
	var out passResponse
	status := p.post("/api/pass/claim", map[string]any{"reissue": token, "nonce": nonce}, &out)
	return status, out
}

// TestPassReissue holds a reissue to a one-time code the owner passes on and
// never sees a Pass code through, for the payment that bought a Pass or a paid
// order nobody collected. The reissue code is in no address: it is answered
// to the owner as a code to send on, and typed on the Pass page, in any case
// and with or without its dashes and the word in front.
func TestPassReissue(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	issued := p.buy("pi_lost")

	if status, _, _ := p.reissueCode("wrong", "pi_lost"); status != http.StatusForbidden {
		t.Fatalf("reissue without the moderation key: status %d", status)
	}
	if status, _, _ := p.reissueCode("mod-key", "pi_unknown"); status != http.StatusNotFound {
		t.Fatalf("reissue of an unknown payment: status %d", status)
	}
	asked := time.Now()
	status, made, code := p.reissueCode("mod-key", "pi_lost")
	if status != http.StatusOK || made.Pass != "" || made.URL != "" {
		t.Fatalf("reissue: status %d, %+v", status, made)
	}
	if !reissueCodeShape(code) {
		t.Fatalf("reissue code %q is not REISSUE and seven groups of four", code)
	}
	// It lasts a week, to the next midnight UTC, like an order's deadline.
	if !withinAWeek(made.ExpiresAt, asked) {
		t.Fatalf("reissue code good until %v, want the first UTC midnight a week on", made.ExpiresAt.UTC())
	}
	// Making the reissue code changes nothing: the Pass code works until the
	// reissue code is used.
	if status, _ := p.activate(issued.Pass); status != http.StatusOK {
		t.Fatalf("making a reissue code stopped the Pass code: status %d", status)
	}

	// Typed anywhere, as a person might type it, the reissue code gives a new
	// code for the same Pass, once.
	buyer := p.otherBrowser()
	typed := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(code, reissueCodePrefix+"-"), "-", " "))
	status, fresh := buyer.redeem(typed)
	if status != http.StatusOK || fresh.Status != "issued" || !passCodeShape(fresh.Pass) || fresh.Pass == issued.Pass ||
		!fresh.ExpiresAt.Equal(issued.ExpiresAt) || fresh.Claim {
		t.Fatalf("redeeming the reissue code: status %d, %+v", status, fresh)
	}
	if status, _ := p.activate(issued.Pass); status != http.StatusForbidden {
		t.Fatalf("the replaced code still works: status %d", status)
	}
	if status, _ := p.activate(fresh.Pass); status != http.StatusOK {
		t.Fatalf("the reissued code: status %d", status)
	}
	if status, out := buyer.redeem(code); status != http.StatusForbidden || refusalState(t, out) != passInvalid || out.Error != passReissueSpent {
		t.Fatalf("a reissue code used twice: status %d, %+v", status, out)
	}
	unknown, err := newReissueCode()
	if err != nil {
		t.Fatal(err)
	}
	if status, out := buyer.redeem(formatReissueCode(unknown)); status != http.StatusForbidden || refusalState(t, out) != passInvalid ||
		out.Error != passReissueSpent {
		t.Fatalf("a well-formed reissue code nobody was sent: status %d, %+v", status, out)
	}
	for _, malformed := range []string{"not a code", issued.Pass, fresh.Pass, code + "A"} {
		if status, out := buyer.redeem(malformed); status != http.StatusForbidden || refusalState(t, out) != passInvalid {
			t.Fatalf("a malformed reissue code %q: status %d, %+v", malformed, status, out)
		}
	}

	// A newer reissue code replaces the one before it, and a reissue code
	// lapses at its deadline.
	_, _, older := p.reissueCode("mod-key", "pi_lost")
	_, _, newer := p.reissueCode("mod-key", "pi_lost")
	if status, _ := buyer.redeem(older); status != http.StatusForbidden {
		t.Fatalf("a replaced reissue code still works: status %d", status)
	}
	p.exec(`UPDATE pass_reissues SET expires_at = now() - interval '1 second'`)
	if status, _ := buyer.redeem(newer); status != http.StatusForbidden {
		t.Fatalf("a reissue code past its deadline still works: status %d", status)
	}

	// Paid for and never collected: the reissue code collects it, and the
	// browser's claim has nothing left. Typed in a browser with a purchase of
	// its own in progress, its answer says so, and the page goes on to
	// collect that.
	p.paid(p.checkout(""), "pi_never_collected")
	_, _, code = p.reissueCode("mod-key", "pi_never_collected")
	shopper := p.otherBrowser()
	shopper.checkout("")
	if status, rescued := shopper.redeem(code); status != http.StatusOK || rescued.Status != "issued" ||
		!passCodeShape(rescued.Pass) || !rescued.Claim {
		t.Fatalf("redeeming a reissue code for an uncollected Pass: status %d, %+v", status, rescued)
	}
	if status, _ := p.claim(); status != http.StatusBadRequest {
		t.Fatalf("an order collected through a reissue code could be collected again: status %d", status)
	}

	// A refund or a dispute cancels a reissue code not yet used: its row goes
	// with the payment, rather than waiting out its deadline, and the code
	// gives nothing. A refunded or disputed payment gets none.
	for _, revoke := range []struct {
		name, event string
		object      map[string]any
	}{
		{"refund", stripeChargeRefunded, map[string]any{"object": "charge", "refunded": true}},
		{"dispute", stripeDisputeCreated, map[string]any{"object": "dispute"}},
	} {
		pi := "pi_" + revoke.name + "_after_reissue"
		revoked := p.buy(pi)
		_, _, code = p.reissueCode("mod-key", pi)
		payment := p.app.passPaymentHash(pi)
		if n := p.rowsContaining("pass_reissues", payment); n != 1 {
			t.Fatalf("the reissue code made before the %s is in %d pass_reissues rows, want 1", revoke.name, n)
		}
		revoke.object["payment_intent"] = pi
		if status := p.webhook(revoke.event, revoke.object); status != http.StatusOK {
			t.Fatalf("%s webhook: status %d", revoke.name, status)
		}
		if n := p.rowsContaining("pass_reissues", payment); n != 0 {
			t.Errorf("the %s left the payment's reissue code in %d pass_reissues rows", revoke.name, n)
		}
		if status, _ := buyer.redeem(code); status != http.StatusForbidden {
			t.Fatalf("a reissue code for a payment after a %s still works: status %d", revoke.name, status)
		}
		if status, _, _ := p.reissueCode("mod-key", pi); status != http.StatusNotFound {
			t.Fatalf("a reissue code for a payment after a %s: status %d", revoke.name, status)
		}
		if status, _ := p.activate(revoked.Pass); status != http.StatusForbidden {
			t.Fatalf("a Pass works after a %s: status %d", revoke.name, status)
		}
	}
}

// withinAWeek reports whether deadline is the first UTC midnight a week after
// a moment between since and now: the week README promises an order and a
// reissue code, written out here rather than read from passClaimTTL.
func withinAWeek(deadline, since time.Time) bool {
	week := 7 * 24 * time.Hour
	return !deadline.Before(dayCeil(since.Add(week))) && !deadline.After(dayCeil(time.Now().Add(week)))
}

// reissueCodeShape reports whether code is written as the owner is given it:
// REISSUE-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX.
func reissueCodeShape(code string) bool {
	groups := strings.Split(code, "-")
	if len(groups) != 8 || groups[0] != reissueCodePrefix {
		return false
	}
	for _, g := range groups[1:] {
		if len(g) != 4 || strings.Trim(g, passAlphabet) != "" {
			return false
		}
	}
	return true
}

// TestPassReissueCannotTakeOverAPass holds a reissue to the payment that
// bought the Pass. Somebody who sees a code once, pays to renew it and asks
// for a reissue with their own payment gets no reissue code, and the holder
// keeps the Pass; one whose renewal was never collected gets only the
// renewal.
func TestPassReissueCannotTakeOverAPass(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	victim := p.buy("pi_victim")

	intruder := p.otherBrowser()
	intruder.renew(victim.Pass, "pi_intruder")
	if status, out, token := p.reissueCode("mod-key", "pi_intruder"); status != http.StatusConflict || token != "" ||
		!strings.Contains(out.Error, "payment that bought the Pass") {
		t.Fatalf("reissue with a renewal's payment: status %d, %+v", status, out)
	}
	if status, _ := p.activate(victim.Pass); status != http.StatusOK {
		t.Fatalf("the holder lost their Pass to a renewal: status %d", status)
	}

	// A reissue code made while a renewal was uncollected, used after it was
	// collected as a renewal, gives nothing either.
	intruder.paid(intruder.checkout(victim.Pass), "pi_collected_after_reissue")
	_, _, early := p.reissueCode("mod-key", "pi_collected_after_reissue")
	if status, out := intruder.claim(); status != http.StatusOK || out.Status != "renewed" {
		t.Fatalf("collect the renewal: status %d, %+v", status, out)
	}
	if status, out := intruder.redeem(early); status != http.StatusForbidden || out.Pass != passInvalid {
		t.Fatalf("a reissue code for a renewal collected since: status %d, %+v", status, out)
	}
	if status, _ := p.activate(victim.Pass); status != http.StatusOK {
		t.Fatalf("the holder lost their Pass to a renewal's early reissue code: status %d", status)
	}

	// Renewed and never collected: the reissue code collects the renewal,
	// which renews the Pass and shows no code.
	before, _ := p.passExpiresAt(victim.Pass)
	intruder.paid(intruder.checkout(victim.Pass), "pi_uncollected_renewal")
	status, _, token := p.reissueCode("mod-key", "pi_uncollected_renewal")
	if status != http.StatusOK {
		t.Fatalf("reissue of an uncollected renewal: status %d", status)
	}
	status, out := intruder.redeem(token)
	if status != http.StatusOK || out.Status != "renewed" || out.Pass != "" || !out.ExpiresAt.Equal(before.AddDate(0, 0, 365)) {
		t.Fatalf("redeeming an uncollected renewal: status %d, %+v", status, out)
	}
	if status, _ := p.activate(victim.Pass); status != http.StatusOK {
		t.Fatalf("the holder lost their Pass to an uncollected renewal's reissue code: status %d", status)
	}
	// Collected now, that payment is a renewal like any other.
	if status, _, _ := p.reissueCode("mod-key", "pi_uncollected_renewal"); status != http.StatusConflict {
		t.Fatalf("reissue of a collected renewal: status %d", status)
	}
}

// TestPassSharesOneHourlyBudget holds the Pass to its one perk: it adds no
// cooldown and lifts none, everyone using one code draws on one hourly
// budget, and only a post that is saved is counted against it.
func TestPassSharesOneHourlyBudget(t *testing.T) {
	p := passServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.ThreadCooldown = time.Minute
		cfg.PostCooldown = time.Minute
		cfg.Pass.HourlyPosts = 2
	})
	p.addBoard("pass-board")
	issued := p.buy("pi_shared")
	code, _ := parsePassCode(issued.Pass)
	key := p.app.passBudgetKey(code)
	spent := func() int {
		p.app.pass.budget.mu.Lock()
		defer p.app.pass.budget.mu.Unlock()
		return p.app.pass.budget.hours[key].count
	}

	var thread createdPost
	if status := p.postWithHeaders(http.Header{passHeader: {issued.Pass}, "X-Forwarded-For": {"10.0.0.1"}},
		"/api/boards/pass-board/threads", map[string]any{"body": "a thread with a pass"}, &thread); status != http.StatusCreated {
		t.Fatalf("first post with the Pass: status %d (%s)", status, thread.Error)
	}
	// Somebody else, elsewhere, with the same Pass: no cooldown is shared, so
	// a post tells nobody when the Pass last posted. The count is shared.
	var reply createdPost
	if status := p.postWithHeaders(http.Header{passHeader: {issued.Pass}, "X-Forwarded-For": {"10.0.0.2"}},
		fmt.Sprintf("/api/boards/pass-board/threads/%d/posts", thread.ThreadNo), map[string]any{"body": "a reply with a pass"}, &reply); status != http.StatusCreated {
		t.Fatalf("a shared Pass was held to the other poster's cooldown: status %d (%s)", status, reply.Error)
	}
	if n := spent(); n != 2 || p.app.pass.budget.left(key, time.Now()) {
		t.Fatalf("two posts with one Pass spent %d of its budget, left %v", n, p.app.pass.budget.left(key, time.Now()))
	}
	// The budget spent, a post with the Pass is still taken: it is a meter for
	// the captcha, never a refusal.
	if status, out := p.postWithPass("pass-board", issued.Pass, "10.0.0.3", nil); status != http.StatusCreated {
		t.Fatalf("a post past the Pass's budget: status %d (%s)", status, out.Error)
	}
	// A Pass never lifts an address's own cooldown, and a post refused by it
	// costs the Pass nothing.
	before := spent()
	if status, _ := p.postWithPass("pass-board", issued.Pass, "10.0.0.1", nil); status != http.StatusTooManyRequests {
		t.Fatalf("a Pass lifted the address's cooldown: status %d", status)
	}
	// Nor does a post that fails: a reply to a thread that is not there.
	var out passResponse
	if status := p.postWithHeaders(http.Header{passHeader: {issued.Pass}, "X-Forwarded-For": {"10.0.0.4"}},
		"/api/boards/pass-board/threads/999999/posts", map[string]any{"body": "nowhere"}, &out); status != http.StatusNotFound {
		t.Fatalf("a reply to a missing thread with a Pass: status %d (%s)", status, out.Error)
	}
	if n := spent(); n != before {
		t.Fatalf("refused and failed posts spent the budget: %d, was %d", n, before)
	}

	// The count is for one clock hour and is forgotten when the hour ends: the
	// flood-control sweep drops it.
	p.app.pass.budget.mu.Lock()
	p.app.pass.budget.hours[key] = passHour{hour: clockHour(time.Now()) - 1, count: 99}
	p.app.pass.budget.mu.Unlock()
	p.app.cooldownSweep.run(time.Now())
	p.app.pass.budget.mu.Lock()
	_, kept := p.app.pass.budget.hours[key]
	p.app.pass.budget.mu.Unlock()
	if kept {
		t.Fatal("the sweep kept a Pass's count from an hour that has ended")
	}
}

// TestPassNoGoodOnPostsIsNoPass holds a post that presents a Pass that is no
// good, refunded, replaced or run out, to what a post without one gets: no
// status of its own for the log to record beside the thread's path, and the
// Pass's state in the answer's body, which no log records, for the page.
func TestPassNoGoodOnPostsIsNoPass(t *testing.T) {
	var logs syncBuffer
	p := passServerLogging(t, nil, &logs)
	p.addBoard("pass-board")
	issued := p.buy("pi_post")

	if status, out := p.postWithPass("pass-board", issued.Pass, "", nil); status != http.StatusCreated || out.Pass != "" {
		t.Fatalf("posting with a Pass: status %d, %+v", status, out)
	}
	for name, code := range map[string]string{
		"malformed": "not a pass",
		"unknown":   "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ",
	} {
		status, out := p.postWithPass("pass-board", code, "", nil)
		if status != http.StatusCreated || out.Pass != passInvalid {
			t.Errorf("%s Pass: status %d, %+v", name, status, out)
		}
	}
	p.exec(`UPDATE passes SET expires_at = now() - interval '1 hour'`)
	if status, out := p.postWithPass("pass-board", issued.Pass, "", nil); status != http.StatusCreated || out.Pass != passExpired {
		t.Fatalf("expired Pass: status %d, %+v", status, out)
	}

	// Refunded, its holder's next reply leaves the same line a reply without
	// a Pass does.
	thread := p.createThread("pass-board", "", "", "a thread")
	refunded := p.buy("pi_post_refunded")
	p.refund("pi_post_refunded")
	path := fmt.Sprintf("/api/boards/pass-board/threads/%d/posts", thread.ThreadNo)
	var with, without passResponse
	if status := p.postWithHeaders(http.Header{passHeader: {refunded.Pass}}, path, map[string]any{"body": "with"}, &with); status != http.StatusCreated || with.Pass != passInvalid {
		t.Fatalf("a reply with a refunded Pass: status %d, %+v", status, with)
	}
	if status := p.postWithHeaders(nil, path, map[string]any{"body": "without"}, &without); status != http.StatusCreated || without.Pass != "" {
		t.Fatalf("a reply without a Pass: status %d, %+v", status, without)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, path) {
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			delete(entry, "time")
			delete(entry, "duration")
			delete(entry, "duration_ms")
			again, _ := json.Marshal(entry)
			lines = append(lines, string(again))
		}
	}
	if len(lines) != 2 || lines[0] != lines[1] {
		t.Fatalf("the replies with and without a refunded Pass were logged differently:\n%s", strings.Join(lines, "\n"))
	}

	// A badged post is the credential's request: the Pass header is not read,
	// so a bad one does not stand in its way, and a good one is not looked up
	// or counted against its hour.
	mail := &captureSender{}
	v := passServer(t, func(cfg *Config) {
		cfg.VerifySecret = "test-verification-secret"
		cfg.VerifyCodeTTL = defaultVerifyCodeTTL
		cfg.VerifyCredentialTTL = defaultVerifyCredentialTTL
		cfg.Sender = mail
	})
	v.addBoard("pass-board")
	credential := (&verifyHarness{harness: v.harness, mail: mail}).verify("ana@iitb.ac.in").Credential
	held := v.buy("pi_beside_a_badge")
	heldCode, _ := parsePassCode(held.Pass)
	for name, code := range map[string]string{"a bad": "not a pass", "a good": held.Pass} {
		var badged passResponse
		if status := v.postWithHeaders(http.Header{credentialHeader: {credential}, passHeader: {code}},
			"/api/boards/pass-board/threads", map[string]any{"body": "badged", "badge": true}, &badged); status != http.StatusCreated || badged.Pass != "" {
			t.Fatalf("badged post with %s Pass header: status %d, %+v", name, status, badged)
		}
	}
	v.app.pass.budget.mu.Lock()
	_, counted := v.app.pass.budget.hours[v.app.passBudgetKey(heldCode)]
	v.app.pass.budget.mu.Unlock()
	if counted {
		t.Fatal("a badged post counted the Pass beside it against its hour")
	}

	// A deployment that has retired the Pass ignores Passes rather than
	// refusing posts, or answering for them.
	off := testServer(t, nil)
	off.addBoard("pass-board")
	var created passResponse
	if status := off.postWithHeaders(http.Header{passHeader: {"not a pass"}},
		"/api/boards/pass-board/threads", map[string]any{"body": "hi"}, &created); status != http.StatusCreated || created.Pass != "" {
		t.Fatalf("posting with a Pass header where the Pass is off: status %d, %+v", status, created)
	}
}

// TestPassLookupFailureNamesNoThread holds a failed Pass lookup on a post to
// a log line without the post's path, which names the board and the thread:
// beside the time, it would say a Pass holder posted there.
func TestPassLookupFailureNamesNoThread(t *testing.T) {
	var logs syncBuffer
	p := passServerLogging(t, nil, &logs)
	p.addBoard("telltale")
	thread := p.createThread("telltale", "", "", "a thread")
	p.exec(`ALTER TABLE passes RENAME TO passes_unreadable`)
	status, _ := p.postWithHeadersStatus(http.Header{passHeader: {"ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ"}},
		fmt.Sprintf("/api/boards/telltale/threads/%d/posts", thread.ThreadNo), map[string]any{"body": "with a pass"})
	if status != http.StatusInternalServerError {
		t.Fatalf("a post whose Pass could not be read: status %d", status)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if entry["msg"] != "could not read the Pass" {
			continue
		}
		found = true
		if _, ok := entry["path"]; ok || strings.Contains(line, "telltale") {
			t.Errorf("the Pass lookup failure names the thread: %s", line)
		}
	}
	if !found {
		t.Fatalf("no Pass lookup failure in the log:\n%s", logs.String())
	}
}

// postWithHeadersStatus is postWithHeaders for a test that wants the status
// and the error only.
func (p *passHarness) postWithHeadersStatus(header http.Header, path string, body any) (int, string) {
	p.t.Helper()
	var out passResponse
	status := p.postWithHeaders(header, path, body, &out)
	return status, out.Error
}

// TestPassEndpointsTakeOnlyJSONFromThisSite holds the Pass's POST endpoints
// to requests another site cannot make a browser send: a body declared as
// JSON, and no Sec-Fetch-Site saying the request came from another site.
func TestPassEndpointsTakeOnlyJSONFromThisSite(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	issued := p.buy("pi_guarded")
	send := func(path, contentType, site, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, p.base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		req.Header.Set(modKeyHeader, "mod-key")
		var out passResponse
		status := p.send(req, &out)
		return status, out.Error
	}
	sessions := p.stripe.sessionCount()
	for path, body := range map[string]string{
		"/api/pass/checkout": `{"adult":true}`,
		"/api/pass/claim":    `{}`,
		"/api/pass/activate": `{"pass":"` + issued.Pass + `"}`,
		"/api/pass/reissue":  `{"payment_intent":"pi_guarded"}`,
	} {
		for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
			if status, _ := send(path, contentType, "", body); status != http.StatusUnsupportedMediaType {
				t.Errorf("%s with Content-Type %q: status %d, want 415", path, contentType, status)
			}
		}
		if status, msg := send(path, "application/json", "cross-site", body); status != http.StatusForbidden ||
			!strings.Contains(msg, "other sites") {
			t.Errorf("%s from another site: status %d (%s), want 403", path, status, msg)
		}
		for _, site := range []string{"", "same-origin", "none"} {
			status, msg := send(path, "application/json; charset=utf-8", site, body)
			if status == http.StatusUnsupportedMediaType || strings.Contains(msg, "other sites") {
				t.Errorf("%s with Sec-Fetch-Site %q: status %d (%s)", path, site, status, msg)
			}
		}
	}
	// Three checkouts went through, one per same-site try, and the refused
	// ones never reached Stripe.
	if n := p.stripe.sessionCount() - sessions; n != 3 {
		t.Errorf("Stripe opened %d sessions, want 3", n)
	}
}

// TestPassPriceSurvivesACancelledRequest holds the cached price to Stripe's
// answer, not to the request that happened to ask: a client that went away
// mid-fetch would otherwise blank the price for everyone for a minute.
func TestPassPriceSurvivesACancelledRequest(t *testing.T) {
	p := passServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	price, err := p.app.passPrice(ctx)
	if err != nil || price.Amount != 2000 {
		t.Fatalf("price for a cancelled request: %+v, %v", price, err)
	}
	var out passResponse
	if p.get("/api/pass", &out); out.Price == nil {
		t.Fatal("a cancelled request left the price out of the cache")
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
// a code used on every posting path (a thread and a reply, each as JSON and
// with an image, and a reply by thread id) is found in no table but its own,
// nowhere in the log, and neither are the buyer's details from the webhook,
// nor the nonce a code was collected with.
func TestPassIsNeverStoredOrLogged(t *testing.T) {
	var logs syncBuffer
	p := passServerLogging(t, func(cfg *Config) { cfg.ModKey = "mod-key" }, &logs)
	p.addBoard("scan")
	p.paid(p.checkout(""), "pi_scan_payment")
	claimToken, _ := p.claimCookie()
	nonce := p.collectNonce()
	_, issued := p.claim()
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
	if status := p.uploadWithHeaders(header, "/api/boards/scan/threads",
		map[string]string{"body": "image thread with a pass"}, "pass.png", pngBytes(t, 16, 16), &created); status != http.StatusCreated {
		t.Fatalf("image thread with a Pass: status %d (%s)", status, created.Error)
	}
	if status := p.postWithHeaders(header, fmt.Sprintf("/api/threads/%d/posts", p.threadID("scan", thread)),
		map[string]any{"body": "reply by thread id with a pass"}, &created); status != http.StatusCreated {
		t.Fatalf("reply by thread id with a Pass: status %d (%s)", status, created.Error)
	}
	if status, out := p.postWithPass("scan", "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ", "", nil); status != http.StatusCreated || out.Pass != passInvalid {
		t.Fatalf("unknown Pass: status %d, %+v", status, out)
	}
	// The code is lost and reissued through a reissue code, and the new one
	// posts too.
	_, _, reissueCode := p.reissueCode("mod-key", "pi_scan_payment")
	canonicalReissue, _ := parseReissueCode(reissueCode)
	elsewhere := p.otherBrowser()
	reissueNonce := elsewhere.collectNonce()
	status, reissued := elsewhere.redeem(reissueCode)
	if status != http.StatusOK || reissued.Status != "issued" {
		t.Fatalf("redeem the reissue code: status %d, %+v", status, reissued)
	}
	if status, out := p.postWithPass("scan", reissued.Pass, "", nil); status != http.StatusCreated {
		t.Fatalf("thread with the reissued Pass: status %d (%s)", status, out.Error)
	}
	newCode, _ := parsePassCode(reissued.Pass)
	// Another is asked for and left unused, so the scan has one in
	// pass_reissues to look for.
	_, _, unspent := p.reissueCode("mod-key", "pi_scan_payment")
	canonicalUnspent, ok := parseReissueCode(unspent)
	if !ok {
		t.Fatalf("the second reissue code %q does not parse", unspent)
	}
	// A checkout is forgotten by its order's name, as the page forgets one
	// that answered after Remove the Pass, Forget this browser or Stop: the
	// name goes in a body, once with a signature that is not the server's,
	// which is refused, and once as it was given. While the order waits, its
	// claim's hash is in pass_orders, which is what the name is made from;
	// once forgotten, it is nowhere.
	orderer := p.otherBrowser()
	orderSession, orderName := orderer.checkoutNamed("")
	orderToken, _ := orderer.claimCookie()
	orderBytes, err := base64.RawURLEncoding.DecodeString(orderName)
	if err != nil || len(orderBytes) != sha256.Size+passOrderTagSize {
		t.Fatalf("the order's name %q is not a claim hash and a signature: %v", orderName, err)
	}
	orderHash := orderBytes[:sha256.Size]
	if n := p.rowsContaining("pass_orders", orderHash); n != 1 {
		t.Fatalf("the waiting order's claim hash is in %d pass_orders rows, want 1; the scan cannot be trusted", n)
	}
	forgedName := base64.RawURLEncoding.EncodeToString(append(slices.Clone(orderHash), make([]byte, passOrderTagSize)...))
	if status, _ := orderer.forgetOrder(forgedName); status != http.StatusBadRequest {
		t.Fatalf("forget by a name the server did not sign: status %d", status)
	}
	if status, _ := orderer.forgetOrder(orderName); status != http.StatusOK {
		t.Fatalf("forget by the order's name: status %d", status)
	}

	stored := p.app.passTokenHash(newCode)
	// Positive controls: the scan finds the post body in posts, and the stored
	// hash in passes, so a scan that finds nothing has looked.
	if n := p.rowsContaining("posts", []byte(marker)); n != 1 {
		t.Fatalf("the scan found the marker in %d posts, want 1; it cannot be trusted", n)
	}
	if n := p.rowsContaining("passes", stored); n != 1 {
		t.Fatalf("the scan found the stored hash in %d passes rows, want 1; it misses bytea columns", n)
	}
	// The reissue code waiting to be used is kept as the SHA-256 hash of its
	// characters, and only as that.
	if n := p.rowsContaining("pass_reissues", hashToken(canonicalUnspent)); n != 1 {
		t.Fatalf("the unused reissue code's hash is in %d pass_reissues rows, want 1", n)
	}
	// The hourly budget is kept in memory under a hash of the code that
	// matches nothing in the database: not the stored hash, and not any
	// column's bytes.
	budgetKey := p.app.passBudgetKey(code)
	budgetHex := strings.TrimPrefix(budgetKey, "pass|")
	budgetBytes, err := hex.DecodeString(budgetHex)
	if err != nil || len(budgetBytes) != sha256.Size {
		t.Fatalf("the budget key %q is not a prefixed hex HMAC: %v", budgetKey, err)
	}
	for _, c := range []string{code, newCode} {
		if strings.TrimPrefix(p.app.passBudgetKey(c), "pass|") == hex.EncodeToString(p.app.passTokenHash(c)) {
			t.Fatal("the budget key is the hash the passes table keeps")
		}
	}
	for name, needle := range map[string][]byte{
		"the code":                    []byte(issued.Pass),
		"the canonical code":          []byte(code),
		"the replaced code's hash":    p.app.passTokenHash(code),
		"the reissued code":           []byte(reissued.Pass),
		"the canonical new code":      []byte(newCode),
		"the reissue code":            []byte(reissueCode),
		"its characters":              []byte(canonicalReissue),
		"the unused reissue code":     []byte(unspent),
		"the unused one's characters": []byte(canonicalUnspent),
		"the claim token":             []byte(claimToken),
		"the claim's nonce":           []byte(nonce),
		"the reissue's nonce":         []byte(reissueNonce),
		"the stored hash":             stored,
		"the budget key":              []byte(budgetKey),
		"the budget key's bytes":      budgetBytes,
		"the unknown code":            []byte("ZZZZZZZZZZZZZZZZZZZZ"),
		"SHA-256 of the code":         sha(code),
		"the payment intent":          []byte("pi_scan_payment"),
		"the buyer's email":           []byte("buyer@example.com"),
		"the buyer's name":            []byte("Bea Buyer"),
		"the checkout session id":     []byte("cs_test_1"),
		"the order's name":            []byte(orderName),
		"the name's bytes":            orderBytes,
		"the forgotten claim's hash":  orderHash,
		"the forgotten claim":         []byte(orderToken),
		"the forged name":             []byte(forgedName),
		"the forgotten session id":    []byte(orderSession),
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
	if !strings.Contains(out, `"path":"/api/pass/stripe-webhook"`) || !strings.Contains(out, `"path":"/api/boards/scan/threads"`) ||
		!strings.Contains(out, `"path":"/api/pass/claim"`) {
		t.Fatalf("expected the webhook, the posts and the claims in the log, got:\n%s", out)
	}
	for name, needle := range map[string]string{
		"the code":                    issued.Pass,
		"the canonical code":          code,
		"the reissued code":           reissued.Pass,
		"the new code":                newCode,
		"the reissue code":            reissueCode,
		"its characters":              canonicalReissue,
		"the unused reissue code":     unspent,
		"the unused one's characters": canonicalUnspent,
		"the claim token":             claimToken,
		"the claim's nonce":           nonce,
		"the reissue nonce":           reissueNonce,
		"the stored hash":             hex.EncodeToString(stored),
		"the budget key":              budgetKey,
		"its hex":                     budgetHex,
		"the payment intent":          "pi_scan_payment",
		"the buyer's email":           "buyer@example.com",
		"the buyer's name":            "Bea Buyer",
		"the session id":              "cs_test_1",
		"the order's name":            orderName,
		"the name's hex":              hex.EncodeToString(orderBytes),
		"the name in standard base64": base64.StdEncoding.EncodeToString(orderBytes),
		"the forgotten claim's hash":  hex.EncodeToString(orderHash),
		"the forgotten claim":         orderToken,
		"the forged name":             forgedName,
		"the forgotten session id":    orderSession,
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
		"pass_payments":    {"days", "ends_at", "origin", "pass_id", "payment_hash"},
		"pass_orders":      {"claim_hash", "days", "expires_at", "payment_hash", "renew_pass_id", "session_hash"},
		"pass_revocations": {"expires_at", "payment_hash"},
		"pass_reissues":    {"expires_at", "payment_hash", "token_hash"},
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

// TestSweepPrunesThePassTables holds the Pass tables to their stated
// lifetimes on a server where nobody starts a checkout: the daily sweep
// prunes them too, whether or not the Pass is on sale.
func TestSweepPrunesThePassTables(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("on sale %v", on), func(t *testing.T) {
			var h *harness
			if on {
				h = passServer(t, nil).harness
			} else {
				h = testServer(t, nil)
			}
			now := time.Now()
			days := func(n int) time.Time { return now.AddDate(0, 0, n) }
			h.exec(`INSERT INTO passes (id, token_hash, expires_at) VALUES
				(1, '\x01', $1), (2, '\x02', $2)`, days(-31), days(-1))
			// Each paid order is kept for its own term past its deadline, and
			// no longer: a year's or a 30-day one whose deadline is an hour
			// less than its term ago stays, and one an hour more goes; a
			// two-year one stays 400 days on. The sweeper is run without
			// EDUCHAN_PASS_DAYS (the server that is not on sale here has
			// none), and needs none.
			termAgo := func(n int, more time.Duration) time.Time {
				return now.Add(-time.Duration(n)*24*time.Hour - more)
			}
			h.exec(`INSERT INTO pass_orders (session_hash, claim_hash, payment_hash, days, expires_at) VALUES
				('\xa1', '\xc1', NULL, 365, $1),
				('\xa2', '\xc2', NULL, 365, $2),
				('\xa3', '\xc3', '\xb3', 365, $3),
				('\xa4', '\xc4', '\xb4', 365, $4),
				('\xa5', '\xc5', '\xb5', 730, $5),
				('\xa6', '\xc6', '\xb6', 30, $6),
				('\xa7', '\xc7', '\xb9', 365, $7),
				('\xa8', '\xc8', '\xba', 30, $8)`,
				now.Add(-time.Hour), days(1), days(-10), termAgo(365, time.Hour), days(-400), termAgo(30, time.Hour),
				termAgo(365, -time.Hour), termAgo(30, -time.Hour))
			h.exec(`INSERT INTO pass_revocations (payment_hash, expires_at) VALUES
				('\xb7', $1), ('\xb8', $2)`, now.Add(-time.Hour), days(1))
			h.exec(`INSERT INTO pass_reissues (token_hash, payment_hash, expires_at) VALUES
				('\xd7', '\xb7', $1), ('\xd8', '\xb8', $2)`, now.Add(-time.Hour), days(1))
			if _, err := h.app.Sweep(context.Background()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			for table, want := range map[string]string{
				// A Pass a month past expiry goes; one within its grace month stays.
				"SELECT string_agg(id::text, ',' ORDER BY id) FROM passes": "2",
				// Unpaid past its deadline goes; paid stays a term past it.
				"SELECT string_agg(encode(session_hash, 'hex'), ',' ORDER BY session_hash) FROM pass_orders": "a2,a3,a5,a7,a8",
				"SELECT string_agg(encode(payment_hash, 'hex'), ',') FROM pass_revocations":                  "b8",
				// A reissue code goes at its deadline.
				"SELECT string_agg(encode(token_hash, 'hex'), ',') FROM pass_reissues": "d8",
			} {
				var got string
				if err := h.app.pool.QueryRow(context.Background(), table).Scan(&got); err != nil {
					t.Fatalf("%s: %v", table, err)
				}
				if got != want {
					t.Errorf("%s after the sweep: %q, want %q", table, got, want)
				}
			}
		})
	}
}

// TestPassCheckoutPrunesThePassTables holds a checkout to pruning what the
// sweep would, so a busy server keeps nothing past its time between two
// sweeps, as the README promises.
func TestPassCheckoutPrunesThePassTables(t *testing.T) {
	p := passServer(t, nil)
	now := time.Now()
	p.exec(`INSERT INTO passes (id, token_hash, expires_at) VALUES
		(1, '\x01', $1), (2, '\x02', $2)`, now.AddDate(0, 0, -31), now.AddDate(0, 0, -1))
	p.exec(`INSERT INTO pass_orders (session_hash, claim_hash, payment_hash, days, expires_at) VALUES
		('\xa1', '\xc1', NULL, 30, $1), ('\xa2', '\xc2', NULL, 30, $2)`, now.Add(-time.Hour), now.AddDate(0, 0, 1))
	p.exec(`INSERT INTO pass_revocations (payment_hash, expires_at) VALUES
		('\xb7', $1), ('\xb8', $2)`, now.Add(-time.Hour), now.AddDate(0, 0, 1))
	p.exec(`INSERT INTO pass_reissues (token_hash, payment_hash, expires_at) VALUES
		('\xd7', '\xb7', $1), ('\xd8', '\xb8', $2)`, now.Add(-time.Hour), now.AddDate(0, 0, 1))
	p.checkout("")
	for query, want := range map[string]string{
		"SELECT string_agg(id::text, ',' ORDER BY id) FROM passes":                                            "2",
		"SELECT string_agg(encode(session_hash, 'hex'), ',') FROM pass_orders WHERE length(session_hash) = 1": "a2",
		"SELECT string_agg(encode(payment_hash, 'hex'), ',') FROM pass_revocations":                           "b8",
		"SELECT string_agg(encode(token_hash, 'hex'), ',') FROM pass_reissues":                                "d8",
	} {
		var got string
		if err := p.app.pool.QueryRow(context.Background(), query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got != want {
			t.Errorf("%s after a checkout: %q, want %q", query, got, want)
		}
	}
}

/* Races --------------------------------------------------------------------- */

// The Pass's writes are applied one at a time under one advisory lock
// (passLockID). These tests take that lock in a transaction of their own,
// fire the racing requests while it is held, and wait until each is queued
// behind it, which fails if any of them does not take the lock first. Released,
// the requests take the lock in the order they asked for it, so each race is
// run both ways round and its outcome checked for each.

type racedResponse struct {
	status int
	body   passResponse
	err    error
}

// fire sends req on a goroutine of its own and delivers the answer on the
// returned channel. Nothing there may fail the test, which only the test's
// own goroutine can do, so the request is built beforehand.
func (p *passHarness) fire(req *http.Request) <-chan racedResponse {
	done := make(chan racedResponse, 1)
	go func() {
		res, err := p.http.Do(req)
		if err != nil {
			done <- racedResponse{err: err}
			return
		}
		defer res.Body.Close()
		var out passResponse
		_ = json.NewDecoder(res.Body).Decode(&out)
		done <- racedResponse{status: res.StatusCode, body: out}
	}()
	return done
}

func (p *passHarness) request(path string, body any, header http.Header) *http.Request {
	p.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		p.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, p.base+path, bytes.NewReader(encoded))
	if err != nil {
		p.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header[k] = v
	}
	return req
}

func (p *passHarness) claimRequest() *http.Request {
	return p.request("/api/pass/claim", map[string]any{"nonce": p.collectNonce()}, nil)
}

func (p *passHarness) webhookRequest(eventType string, object map[string]any) *http.Request {
	p.t.Helper()
	body, err := json.Marshal(map[string]any{
		"id": "evt_test", "object": "event", "type": eventType,
		"data": map[string]any{"object": object},
	})
	if err != nil {
		p.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, p.base+"/api/pass/stripe-webhook", bytes.NewReader(body))
	if err != nil {
		p.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", signStripe(body, testWebhookSecret, time.Now()))
	return req
}

func (p *passHarness) paidRequest(session, paymentIntent string) *http.Request {
	return p.webhookRequest(stripeCheckoutCompleted, map[string]any{
		"id": session, "object": "checkout.session", "mode": "payment",
		"payment_status": "paid", "payment_intent": paymentIntent,
	})
}

func (p *passHarness) refundRequest(paymentIntent string) *http.Request {
	return p.webhookRequest(stripeChargeRefunded, map[string]any{
		"object": "charge", "payment_intent": paymentIntent, "refunded": true,
	})
}

// passLock is the Pass lock held by the test.
type passLock struct {
	p    *passHarness
	conn *pgx.Conn
	tx   pgx.Tx
	pid  int
}

func (p *passHarness) holdPassLock() *passLock {
	p.t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, p.app.cfg.DatabaseURL)
	if err != nil {
		p.t.Fatalf("connect: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		p.t.Fatalf("begin: %v", err)
	}
	l := &passLock{p: p, conn: conn, tx: tx}
	p.t.Cleanup(l.release)
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&l.pid); err != nil {
		p.t.Fatalf("backend pid: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext(current_schema()))`, passLockID); err != nil {
		p.t.Fatalf("take the Pass lock: %v", err)
	}
	return l
}

// queued waits until n requests are waiting on the lock: the one holding it,
// or another waiting ahead of them.
func (l *passLock) queued(n int, what string) {
	l.p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		// Asked on the holder's own connection: the requests waiting have taken
		// connections from the server's pool, which may have none left.
		if err := l.tx.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
			  AND classid = $1::int::oid AND objid = hashtext(current_schema())::oid`,
			passLockID).Scan(&waiting); err != nil {
			l.p.t.Fatalf("look for requests waiting on the Pass lock: %v", err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			l.p.t.Fatalf("%s never waited on the Pass lock (%d of %d waiting)", what, waiting, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (l *passLock) release() {
	if l.conn == nil {
		return
	}
	ctx := context.Background()
	_ = l.tx.Rollback(ctx)
	_ = l.conn.Close(ctx)
	l.conn = nil
}

// race fires two requests in turn behind the Pass lock, so they run in that
// order, and returns their answers.
func (p *passHarness) race(first, second *http.Request, names [2]string) (racedResponse, racedResponse) {
	p.t.Helper()
	lock := p.holdPassLock()
	a := p.fire(first)
	lock.queued(1, names[0])
	b := p.fire(second)
	lock.queued(2, names[1])
	lock.release()
	ra, rb := <-a, <-b
	for i, r := range []racedResponse{ra, rb} {
		if r.err != nil || r.status >= 500 {
			p.t.Fatalf("%s: status %d, %v (%s)", names[i], r.status, r.err, r.body.Error)
		}
	}
	return ra, rb
}

func (p *passHarness) count(table string) int {
	p.t.Helper()
	var n int
	if err := p.app.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		p.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// A refund racing the claim of what it refunds: whichever goes first, the
// refunded payment ends up buying nothing.
func TestPassRefundRacingAClaim(t *testing.T) {
	for _, claimFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("claim first %v", claimFirst), func(t *testing.T) {
			p := passServer(t, nil)
			p.paid(p.checkout(""), "pi_race")
			claim, refund := p.claimRequest(), p.refundRequest("pi_race")
			var claimed racedResponse
			if claimFirst {
				claimed, _ = p.race(claim, refund, [2]string{"the claim", "the refund"})
				if claimed.status != http.StatusOK || claimed.body.Status != "issued" {
					t.Fatalf("claim ahead of the refund: status %d, %+v", claimed.status, claimed.body)
				}
				if status, _ := p.activate(claimed.body.Pass); status != http.StatusForbidden {
					t.Fatalf("the refunded Pass still works: status %d", status)
				}
			} else {
				_, claimed = p.race(refund, claim, [2]string{"the refund", "the claim"})
				if claimed.status != http.StatusBadRequest {
					t.Fatalf("claim behind the refund: status %d, %+v", claimed.status, claimed.body)
				}
			}
			for _, table := range []string{"passes", "pass_payments", "pass_orders"} {
				if n := p.count(table); n != 0 {
					t.Errorf("%d rows left in %s after a refund", n, table)
				}
			}
		})
	}
}

// A refund racing the "paid" event of the same payment: whichever goes
// first, no Pass comes of it.
func TestPassPaymentRacingARefund(t *testing.T) {
	for _, paidFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("paid first %v", paidFirst), func(t *testing.T) {
			p := passServer(t, nil)
			session := p.checkout("")
			paid, refund := p.paidRequest(session, "pi_race"), p.refundRequest("pi_race")
			if paidFirst {
				p.race(paid, refund, [2]string{"the paid event", "the refund"})
			} else {
				p.race(refund, paid, [2]string{"the refund", "the paid event"})
			}
			status, out := p.claim()
			if status == http.StatusOK || p.count("passes") != 0 {
				t.Fatalf("a refunded payment was collected: status %d, %+v", status, out)
			}
			if p.count("pass_revocations") != 1 {
				t.Fatal("the refund was not remembered")
			}
		})
	}
}

// Collecting a renewal racing a refund of the Pass it renews: it once
// deadlocked, and whichever goes first now, each payment keeps its own term.
func TestPassRenewalRacingARefund(t *testing.T) {
	for _, claimFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("claim first %v", claimFirst), func(t *testing.T) {
			p := passServer(t, nil)
			issued := p.buy("pi_origin")
			bought, _ := p.passExpiresAt(issued.Pass)
			p.paid(p.checkout(issued.Pass), "pi_renewal")
			claim, refund := p.claimRequest(), p.refundRequest("pi_origin")
			if claimFirst {
				claimed, _ := p.race(claim, refund, [2]string{"the renewal's claim", "the refund"})
				if claimed.status != http.StatusOK || claimed.body.Status != "renewed" {
					t.Fatalf("renewal ahead of the refund: status %d, %+v", claimed.status, claimed.body)
				}
				// The renewal's year stands; the purchase's is gone.
				got, ok := p.passExpiresAt(issued.Pass)
				if !ok || !got.After(time.Now()) || !got.Before(bought.AddDate(0, 0, 365)) {
					t.Fatalf("after the refund the Pass runs to %v (kept: %v); want the renewal's year only", got, ok)
				}
			} else {
				_, claimed := p.race(refund, claim, [2]string{"the refund", "the renewal's claim"})
				// The Pass it renewed is gone, so the renewal is a Pass of its own.
				if claimed.status != http.StatusOK || claimed.body.Status != "issued" {
					t.Fatalf("renewal behind the refund: status %d, %+v", claimed.status, claimed.body)
				}
				if status, _ := p.activate(issued.Pass); status != http.StatusForbidden {
					t.Fatalf("the refunded Pass still works: status %d", status)
				}
				if status, _ := p.activate(claimed.body.Pass); status != http.StatusOK {
					t.Fatalf("the renewal's own Pass: status %d", status)
				}
			}
		})
	}
}

// Every request that writes a Pass table, and the sweep, queues for the Pass
// lock.
func TestEveryPassWriteTakesThePassLock(t *testing.T) {
	p := passServer(t, func(cfg *Config) {
		cfg.ModKey = "mod-key"
		// Each request waiting on the lock holds a connection.
		cfg.DBMaxConns = 16
	})
	p.buy("pi_reissue")
	_, _, token := p.reissueCode("mod-key", "pi_reissue")
	session, order := p.checkoutNamed("")
	unpaid := map[string]any{"id": session, "object": "checkout.session", "mode": "payment", "payment_status": "unpaid"}
	request := func(req *http.Request) func() <-chan racedResponse {
		return func() <-chan racedResponse { return p.fire(req) }
	}
	writes := []struct {
		name  string
		start func() <-chan racedResponse
	}{
		{"a checkout", request(p.request("/api/pass/checkout", map[string]any{"adult": true}, nil))},
		{"a claim", request(p.claimRequest())},
		{"a slow payment", request(p.webhookRequest(stripeCheckoutCompleted, unpaid))},
		{"an expired checkout", request(p.webhookRequest(stripeCheckoutExpired, unpaid))},
		{"a paid event", request(p.paidRequest(session, "pi_paid"))},
		{"a refund", request(p.refundRequest("pi_refund"))},
		{"a dispute", request(p.webhookRequest(stripeDisputeCreated, map[string]any{"payment_intent": "pi_dispute"}))},
		{"a reissue", request(p.request("/api/pass/reissue", map[string]any{"payment_intent": "pi_reissue"},
			http.Header{modKeyHeader: {"mod-key"}}))},
		{"a reissue code redeemed", request(p.request("/api/pass/claim", map[string]any{"reissue": token, "nonce": p.collectNonce()}, nil))},
		{"a claim forgotten", request(p.request("/api/pass/claim", map[string]any{"forget": true}, nil))},
		{"a claim forgotten by its order", request(p.request("/api/pass/claim", map[string]any{"forget": true, "order": order}, nil))},
		{"the sweep", func() <-chan racedResponse {
			done := make(chan racedResponse, 1)
			go func() {
				_, err := p.app.Sweep(context.Background())
				done <- racedResponse{status: http.StatusOK, err: err}
			}()
			return done
		}},
	}
	lock := p.holdPassLock()
	var answers []<-chan racedResponse
	for i, w := range writes {
		answers = append(answers, w.start())
		lock.queued(i+1, w.name)
	}
	lock.release()
	for i, a := range answers {
		if r := <-a; r.err != nil || r.status >= 500 {
			t.Errorf("%s: status %d, %v", writes[i].name, r.status, r.err)
		}
	}
}

/* Round two ---------------------------------------------------------------- */

// checkoutSetCookie starts a checkout in this harness's browser and returns
// the Stripe session id and the claim cookie the answer set.
func (p *passHarness) checkoutSetCookie(renew string) (string, *http.Cookie) {
	p.t.Helper()
	body := map[string]any{"adult": true}
	if renew != "" {
		body["renew"] = renew
	}
	res, err := p.http.Do(p.request("/api/pass/checkout", body, nil))
	if err != nil {
		p.t.Fatal(err)
	}
	defer res.Body.Close()
	var out passResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != http.StatusOK {
		p.t.Fatalf("checkout: status %d, %+v (%v)", res.StatusCode, out, err)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == passClaimCookie {
			cookie = c
		}
	}
	return out.URL[strings.LastIndexByte(out.URL, '/')+1:], cookie
}

// setClaimCookie puts a claim token in this harness's browser, as somebody
// copying one off another browser, or planting one, would.
func (p *passHarness) setClaimCookie(value string) {
	p.t.Helper()
	u, err := url.Parse(p.base + passCookiePath)
	if err != nil {
		p.t.Fatal(err)
	}
	p.http.Jar.SetCookies(u, []*http.Cookie{{Name: passClaimCookie, Value: value, Path: passCookiePath}})
}

// forget drops this browser's claim, as Forget this browser and Remove the
// Pass do.
func (p *passHarness) forget() (int, passResponse) {
	p.t.Helper()
	var out passResponse
	status := p.post("/api/pass/claim", map[string]any{"forget": true}, &out)
	return status, out
}

// twin is a second server on this one's database with its Pass settings
// changed, as the same deployment restarted with new settings would be. It
// shares this browser: the same cookies go to both.
func (p *passHarness) twin(tune func(*PassConfig)) *passHarness {
	p.t.Helper()
	cfg := p.app.cfg
	tune(&cfg.Pass)
	app, err := New(context.Background(), cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		p.t.Fatalf("start the twin: %v", err)
	}
	p.t.Cleanup(app.Close)
	srv := httptest.NewServer(app.Routes())
	p.t.Cleanup(srv.Close)
	app.pass.activateLimiter = newCooldownLimiter(0)
	h := &harness{client: &client{t: p.t, base: srv.URL, http: p.http}, app: app}
	return &passHarness{harness: h, stripe: p.stripe}
}

// TestPassClaimCanBeAskedAgain holds collecting to being safe to repeat: a
// page whose answer never arrived asks again with the cookie and the nonce it
// kept and is given the same code, where it was once told there was nothing
// to collect and the paid code was lost for good. Only both together are: the
// cookie copied without the nonce, or the nonce copied, planted or left
// behind and sent with any other cookie or reissue code, gets nothing, and a
// code refunded away or replaced by a reissue does not come back.
func TestPassClaimCanBeAskedAgain(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	p.paid(p.checkout(""), "pi_lost_answer")
	cookie, _ := p.claimCookie()
	nonce := p.collectNonce()
	status, first := p.claimWith(nonce)
	if status != http.StatusOK || first.Status != "issued" || first.Claim {
		t.Fatalf("claim: status %d, %+v", status, first)
	}
	// The answer never reached the page, which asks again.
	status, again := p.claimWith(nonce)
	if status != http.StatusOK || again.Status != "issued" || again.Pass != first.Pass ||
		!again.ExpiresAt.Equal(first.ExpiresAt) || again.Claim {
		t.Fatalf("the same claim asked again: status %d, %+v; want %s again", status, again, first.Pass)
	}
	if n := p.count("passes"); n != 1 {
		t.Fatalf("asking again made %d Passes, want 1", n)
	}
	other, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if status, out := p.claimWith(other); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("a claim with another nonce: status %d, %+v", status, out)
	}
	var out passResponse
	if status := p.post("/api/pass/claim", map[string]any{}, &out); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("a claim with no nonce: status %d, %+v", status, out)
	}
	thief := p.otherBrowser()
	thief.setClaimCookie(cookie)
	if status, out := thief.claim(); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("the claim cookie copied without the nonce: status %d, %+v", status, out)
	}
	madeUp, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	nonceThief := p.otherBrowser()
	if status, out := nonceThief.claimWith(nonce); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("the nonce copied without the cookie: status %d, %+v", status, out)
	}
	nonceThief.setClaimCookie(madeUp)
	if status, out := nonceThief.claimWith(nonce); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("the nonce copied with a made-up cookie: status %d, %+v", status, out)
	}

	// A nonce planted in a shared browser by somebody who started a checkout
	// there: the next person is told to wait, buys and collects with it, and
	// the planter, with the nonce and the cookie of their own checkout or one
	// made up, gets nothing of what was bought.
	shared := p.otherBrowser()
	shared.checkout("")
	plantersCookie, _ := shared.claimCookie()
	planted := shared.collectNonce()
	if status, out := shared.claim(); status != http.StatusAccepted || !out.Claim {
		t.Fatalf("the next person's first claim: status %d, %+v", status, out)
	}
	shared.paid(shared.checkout(""), "pi_shared")
	if status, out := shared.claimWith(planted); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("the next person's purchase: status %d, %+v", status, out)
	}
	planter := p.otherBrowser()
	for name, c := range map[string]string{"their own checkout's": plantersCookie, "a made-up": madeUp} {
		planter.setClaimCookie(c)
		if status, out := planter.claimWith(planted); status == http.StatusOK || out.Pass != "" {
			t.Fatalf("the planted nonce with %s cookie: status %d, %+v", name, status, out)
		}
	}

	// A reissue code spent by a request whose answer was lost is answered
	// again the same way, and only to that nonce.
	_, _, token := p.reissueCode("mod-key", "pi_lost_answer")
	buyer := p.otherBrowser()
	reissueNonce := buyer.collectNonce()
	// Without a nonce, or with one the page could not have drawn, the reissue
	// code is refused before it is looked at, and is not spent.
	for name, body := range map[string]map[string]any{
		"no nonce":          {"reissue": token},
		"a malformed nonce": {"reissue": token, "nonce": "not-a-nonce"},
	} {
		var out passResponse
		if status := buyer.post("/api/pass/claim", body, &out); status != http.StatusBadRequest || out.Pass != "" {
			t.Fatalf("a reissue code with %s: status %d, %+v", name, status, out)
		}
	}
	status, fresh := buyer.redeemWith(token, reissueNonce)
	if status != http.StatusOK || fresh.Status != "issued" || fresh.Pass == first.Pass {
		t.Fatalf("redeem: status %d, %+v", status, fresh)
	}
	status, refetched := buyer.redeemWith(token, reissueNonce)
	if status != http.StatusOK || refetched.Status != "issued" || refetched.Pass != fresh.Pass || !refetched.ExpiresAt.Equal(fresh.ExpiresAt) {
		t.Fatalf("the same redeem asked again: status %d, %+v; want %s again", status, refetched, fresh.Pass)
	}
	// Pasted the other way the second time, in lower case, without the word
	// and with spaces for dashes, it is the same reissue code, and gives the
	// same Pass code.
	retyped := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(token, reissueCodePrefix+"-"), "-", " "))
	if status, out := buyer.redeemWith(retyped, reissueNonce); status != http.StatusOK || out.Status != "issued" || out.Pass != fresh.Pass {
		t.Fatalf("the same redeem asked again, typed differently: status %d, %+v; want %s again", status, out, fresh.Pass)
	}
	if status, out := buyer.redeemWith(token, other); status != http.StatusForbidden || out.Pass != passInvalid {
		t.Fatalf("the spent reissue code with another nonce: status %d, %+v", status, out)
	}
	madeUpReissue, err := newReissueCode()
	if err != nil {
		t.Fatal(err)
	}
	if status, out := buyer.redeemWith(madeUpReissue, reissueNonce); status != http.StatusForbidden || out.Pass != passInvalid {
		t.Fatalf("the reissue's nonce with a made-up reissue code: status %d, %+v", status, out)
	}
	if status, _ := p.activate(first.Pass); status != http.StatusForbidden {
		t.Fatalf("the replaced code still works: status %d", status)
	}
	if status, _ := p.activate(fresh.Pass); status != http.StatusOK {
		t.Fatalf("the reissued code: status %d", status)
	}
	// The claim that collected the replaced code, asked again, does not bring
	// it back.
	if status, out := p.claimWith(nonce); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("the claim asked again after a reissue: status %d, %+v", status, out)
	}

	// A nonce left behind in a browser by a reissue code whose answer was
	// lost, sent with another reissue code, gets that one's own code and
	// leaves the first alone.
	p.otherBrowser().buy("pi_second_lost")
	_, _, second := p.reissueCode("mod-key", "pi_second_lost")
	status, leftover := buyer.redeemWith(second, reissueNonce)
	if status != http.StatusOK || leftover.Status != "issued" || leftover.Pass == fresh.Pass {
		t.Fatalf("another reissue code with the nonce left behind: status %d, %+v", status, leftover)
	}
	for _, code := range []string{fresh.Pass, leftover.Pass} {
		if status, _ := p.activate(code); status != http.StatusOK {
			t.Fatalf("code %s after a second reissue code with the same nonce: status %d", code, status)
		}
	}

	// A claim asked again after its Pass was refunded away gets nothing.
	refunded := p.otherBrowser()
	refunded.paid(refunded.checkout(""), "pi_refunded_away")
	refundedNonce := refunded.collectNonce()
	if status, out := refunded.claimWith(refundedNonce); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("claim before the refund: status %d, %+v", status, out)
	}
	refunded.refund("pi_refunded_away")
	if status, out := refunded.claimWith(refundedNonce); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("the claim asked again after a refund: status %d, %+v", status, out)
	}
}

// TestPassCheckoutGivesANewClaim holds every checkout to a claim token only
// that browser has: a token copied off a shared browser, or planted in it,
// before somebody buys there collects nothing of what they buy.
func TestPassCheckoutGivesANewClaim(t *testing.T) {
	p := passServer(t, nil)
	// The claim cookie goes back only to the Pass endpoints, never to a post
	// or anything else, is out of the page's scripts' reach, is not sent on
	// another site's form, and lasts past Stripe's day and the 30 days a slow
	// payment is held: 31 days, rounded up to a UTC midnight.
	before := time.Now()
	_, cookie := p.checkoutSetCookie("")
	after := time.Now()
	if cookie == nil {
		t.Fatal("a checkout set no claim cookie")
	}
	if cookie.Path != "/api/pass" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("the claim cookie: path %q, HttpOnly %v, SameSite %v; want /api/pass, HttpOnly and Lax",
			cookie.Path, cookie.HttpOnly, cookie.SameSite)
	}
	// The cookie runs out at the midnight that ends those 31 days, to within
	// the second its max age is counted in, for a checkout started at any
	// moment of the request.
	lasts := time.Duration(cookie.MaxAge) * time.Second
	ends := func(start time.Time) bool {
		until := dayCeil(start.Add(31 * 24 * time.Hour))
		return lasts <= until.Sub(before) && lasts >= until.Sub(after)-time.Second
	}
	if !ends(before) && !ends(after) {
		t.Fatalf("the claim cookie's max age is %v, want what is left of 31 days rounded up to a UTC midnight", lasts)
	}
	// Those are 31 days exactly, not just to the midnight they round to: a
	// checkout started at a midnight keeps the cookie to the midnight 31 days
	// on, and one started a moment after to the midnight after that.
	midnight := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	for start, want := range map[time.Time]time.Time{
		midnight:                      midnight.AddDate(0, 0, 31),
		midnight.Add(time.Nanosecond): midnight.AddDate(0, 0, 32),
	} {
		if got := passClaimCookieUntil(start); !got.Equal(want) {
			t.Errorf("a checkout started at %v keeps its claim cookie until %v, want %v", start, got, want)
		}
	}

	// Somebody starts a checkout on a shared computer, copies its claim
	// cookie out of the browser and leaves without paying.
	p.checkout("")
	copied, _ := p.claimCookie()
	thief := p.otherBrowser()
	thief.setClaimCookie(copied)
	// The next person at the computer buys a Pass.
	p.paid(p.checkout(""), "pi_student")
	if now, _ := p.claimCookie(); now == copied {
		t.Fatal("the checkout kept the claim token that was copied before it")
	}
	// The copied token still reaches the checkout it was copied from, which
	// nobody paid, and nothing else.
	if status, out := thief.claim(); status != http.StatusAccepted || out.Pass != "" {
		t.Fatalf("the copied token: status %d, %+v; want the abandoned checkout still waiting", status, out)
	}
	status, bought := p.claim()
	if status != http.StatusOK || bought.Status != "issued" || bought.Claim {
		t.Fatalf("the buyer's claim: status %d, %+v", status, bought)
	}
	if status, out := thief.claim(); status != http.StatusAccepted || out.Pass != "" {
		t.Fatalf("the copied token after the buyer collected: status %d, %+v", status, out)
	}

	// A token chosen elsewhere and planted in a browser covers nothing that
	// browser buys after it.
	victim := p.otherBrowser()
	planted, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	victim.setClaimCookie(planted)
	victim.paid(victim.checkout(""), "pi_victim")
	attacker := p.otherBrowser()
	attacker.setClaimCookie(planted)
	if status, out := attacker.claim(); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("a planted token collected the victim's Pass: status %d, %+v", status, out)
	}
	if status, out := victim.claim(); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("the victim's claim: status %d, %+v", status, out)
	}
}

// TestPassForgetDropsTheClaim holds Forget this browser, Remove the Pass and
// Stop collecting it here to forgetting a purchase still being collected:
// whoever uses the browser next collects nothing of it, even with a copy of
// the claim cookie taken before, and the buyer gets it from their receipt
// through a reissue.
func TestPassForgetDropsTheClaim(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	session := p.checkout("")
	slow := map[string]any{"id": session, "object": "checkout.session", "mode": "payment",
		"payment_status": "unpaid", "payment_intent": "pi_forgotten"}
	p.webhook(stripeCheckoutCompleted, slow)
	if !p.reportsClaim() {
		t.Fatal("no purchase to collect before forgetting")
	}
	// The page has asked once, and been told to wait.
	nonce := p.collectNonce()
	if status, out := p.claimWith(nonce); status != http.StatusAccepted || !out.Claim {
		t.Fatalf("claim before forgetting: status %d, %+v", status, out)
	}
	copied, _ := p.claimCookie()
	if status, out := p.forget(); status != http.StatusOK || out.Claim {
		t.Fatalf("forget: status %d, %+v", status, out)
	}
	if _, ok := p.claimCookie(); ok {
		t.Fatal("forgetting left the claim cookie in the browser")
	}
	// The bank debit clears after the buyer has left.
	slow["payment_status"] = "paid"
	p.webhook(stripeCheckoutAsyncSuccess, slow)
	if p.reportsClaim() {
		t.Fatal("the next person at the browser is told there is a purchase to collect")
	}
	if status, out := p.claim(); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("the next person collected the forgotten purchase: status %d, %+v", status, out)
	}
	fresh, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	replay := p.otherBrowser()
	replay.setClaimCookie(copied)
	for name, n := range map[string]string{"the nonce it had": nonce, "a fresh nonce": fresh} {
		if status, out := replay.claimWith(n); status != http.StatusBadRequest || out.Pass != "" {
			t.Fatalf("the claim cookie copied before forgetting, with %s: status %d, %+v", name, status, out)
		}
	}
	if n := p.count("passes"); n != 0 {
		t.Fatalf("a forgotten purchase was collected: %d Passes", n)
	}
	// The buyer writes in with their receipt.
	_, _, token := p.reissueCode("mod-key", "pi_forgotten")
	if status, out := p.otherBrowser().redeem(token); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("reissue of the forgotten purchase: status %d, %+v", status, out)
	}
	// Forgetting with nothing to forget is no error.
	if status, _ := p.forget(); status != http.StatusOK {
		t.Fatalf("forget with no claim: status %d", status)
	}
}

// forgetOrder drops the one claim whose order a checkout's answer named, as
// the page does for a checkout that answered after a forget, and returns the
// claim cookie the answer set, if it set one.
func (p *passHarness) forgetOrder(name string) (int, *http.Cookie) {
	p.t.Helper()
	status, set, _ := p.forgetOrderAnswered(name)
	return status, set
}

// forgetOrderAnswered is forgetOrder with the whole answer, every key in it.
func (p *passHarness) forgetOrderAnswered(name string) (int, *http.Cookie, map[string]any) {
	p.t.Helper()
	res, err := p.http.Do(p.request("/api/pass/claim", map[string]any{"forget": true, "order": name}, nil))
	if err != nil {
		p.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		p.t.Fatal(err)
	}
	if res.StatusCode == http.StatusOK && out["status"] != "forgotten" {
		p.t.Fatalf("forget by order: %v", out)
	}
	for _, c := range res.Cookies() {
		if c.Name == passClaimCookie {
			return res.StatusCode, c, out
		}
	}
	return res.StatusCode, nil, out
}

// TestPassForgetByOrderDropsThatClaimAlone holds the forget the page sends for
// a checkout that answered after Remove the Pass, Forget this browser or Stop
// collecting it here, naming the order the answer gave it, to that checkout's
// claim and no other. The claim collects nothing, even with a copy of its
// cookie and once its checkout is paid after all, and the buyer gets that
// through a reissue. A checkout started after the forget is somebody's
// purchase, and its cookie can be in the browser by the time the forget of the
// earlier one arrives, so that forget reads no cookie and clears none, and
// its answer, {"status":"forgotten"}, says nothing of what else waits. Only
// the server makes a name: one made from what pass_orders holds drops nothing.
func TestPassForgetByOrderDropsThatClaimAlone(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })

	// The checkout answered after the browser's own forget did, so its
	// cookie is the browser's when the forget naming it goes.
	session, name := p.checkoutNamed("")
	token, _ := p.claimCookie()
	if name == token {
		t.Fatal("a checkout's answer named its order by the claim itself, which collects it")
	}
	if p.sendingClaimCookies(name).reportsClaim() {
		t.Fatal("the order's name, sent as a claim cookie, has something to collect")
	}
	// The answer is the status and nothing more: no cookie was read, so it
	// says nothing of what else waits ("claim").
	if status, set, out := p.forgetOrderAnswered(name); status != http.StatusOK || set != nil ||
		!maps.Equal(out, map[string]any{"status": "forgotten"}) {
		t.Fatalf(`forget by order: status %d, setting %v, answer %v; want 200, no cookie set or cleared, and {"status":"forgotten"} alone`,
			status, set, out)
	}
	if left, ok := p.claimCookie(); !ok || left != token {
		t.Fatal("forgetting by order took the cookie out of the browser")
	}
	if p.reportsClaim() {
		t.Fatal("GET /api/pass still tells the next person at the browser there is a purchase to collect")
	}
	// Paid after all, which nothing on the page led to, it is the buyer's
	// still, by a reissue code from the receipt, and no copy of the claim
	// collects it.
	p.paid(session, "pi_forgotten_by_order")
	if p.reportsClaim() {
		t.Fatal("the forgotten checkout, paid, is a purchase to collect")
	}
	if status, out := p.claim(); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("the browser collected the forgotten checkout: status %d, %+v", status, out)
	}
	replay := p.otherBrowser()
	replay.setClaimCookie(token)
	if status, out := replay.claim(); status != http.StatusBadRequest || out.Pass != "" {
		t.Fatalf("a copy of the forgotten claim: status %d, %+v", status, out)
	}
	_, _, code := p.reissueCode("mod-key", "pi_forgotten_by_order")
	if status, out := p.otherBrowser().redeem(code); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("reissue of the checkout forgotten by order: status %d, %+v", status, out)
	}

	// A checkout started after the forget answered before the forgotten one's
	// forget arrived: its cookie is the browser's, and it is still collected.
	q := p.otherBrowser()
	forgotten, forgottenName := q.checkoutNamed("")
	forgottenToken, _ := q.claimCookie()
	after := q.checkout("")
	kept, _ := q.claimCookie()
	if status, set := q.forgetOrder(forgottenName); status != http.StatusOK || set != nil {
		t.Fatalf("forget by order beside a later checkout: status %d, setting %v", status, set)
	}
	if left, ok := q.claimCookie(); !ok || left != kept {
		t.Fatal("forgetting the earlier checkout by order took the later one's cookie out of the browser")
	}
	if !q.reportsClaim() {
		t.Fatal("forgetting the earlier checkout by order dropped the later one's claim")
	}
	q.paid(forgotten, "pi_forgotten_beside")
	q.paid(after, "pi_started_after_the_forget")
	if status, out := q.claim(); status != http.StatusOK || out.Status != "issued" || out.Claim {
		t.Fatalf("the checkout started after the forget: status %d, %+v; want its Pass and nothing more", status, out)
	}
	replay = p.otherBrowser()
	replay.setClaimCookie(forgottenToken)
	if replay.reportsClaim() {
		t.Fatal("a copy of the claim forgotten by order has something to collect")
	}

	// A name that is not one is refused, and so is a name beside anything but
	// a forget, from a browser with a checkout waiting, which is left as it
	// was; a name with nothing under it is nothing to forget, and no error.
	waiting := p.otherBrowser()
	waitingSession, waitingName := waiting.checkoutNamed("")
	waitingToken, _ := waiting.claimCookie()
	for what, body := range map[string]map[string]any{
		"a name cut short":         {"forget": true, "order": waitingName[:20]},
		"a name not in base64url":  {"forget": true, "order": strings.Repeat("!", len(waitingName))},
		"a claim token for a name": {"forget": true, "order": waitingToken},
		"a name with no forget":    {"order": waitingName, "nonce": waiting.collectNonce()},
	} {
		var out passResponse
		if status := waiting.post("/api/pass/claim", body, &out); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, %+v; want 400", what, status, out)
		}
	}
	if !waiting.reportsClaim() {
		t.Fatal("a refused forget by order dropped the browser's claim")
	}
	// Nor does a name made from what the database holds of a claim, which
	// anyone who can read pass_orders has, drop anything, bare or with a
	// signature that is not the server's for it, one made under another
	// secret included: only the server makes a name, so a copy of the table
	// takes no waiting order from its buyer.
	rows, err := p.app.pool.Query(context.Background(), `SELECT claim_hash FROM pass_orders`)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatal(err)
	}
	signed, _ := base64.RawURLEncoding.DecodeString(waitingName)
	waitingHash := hashToken(waitingToken)
	if len(signed) != sha256.Size+passOrderTagSize || !bytes.Equal(signed[:sha256.Size], waitingHash) ||
		!slices.ContainsFunc(stored, func(h []byte) bool { return bytes.Equal(h, waitingHash) }) {
		t.Fatalf("the waiting checkout's name %q is not its stored claim hash and a signature", waitingName)
	}
	stranger := p.otherBrowser()
	rotated := p.twin(func(c *PassConfig) { c.Secret = testPassSecret + "-rotated" })
	for _, hash := range stored {
		for what, forged := range map[string][]byte{
			"a stored claim hash":                              hash,
			"a stored claim hash, signed with zeros":           append(slices.Clone(hash), make([]byte, passOrderTagSize)...),
			"a stored claim hash, with another's tag":          append(slices.Clone(hash), p.app.passOrderTag(hashToken("another claim"))...),
			"a stored claim hash, tagged with its hash":        append(slices.Clone(hash), hash[:passOrderTagSize]...),
			"a stored claim hash, signed under another secret": append(slices.Clone(hash), rotated.app.passOrderTag(hash)...),
		} {
			if status, _ := stranger.forgetOrder(base64.RawURLEncoding.EncodeToString(forged)); status != http.StatusBadRequest {
				t.Errorf("%s as an order's name: status %d; want 400", what, status)
			}
		}
	}
	if !waiting.reportsClaim() {
		t.Fatal("a name made from pass_orders dropped a waiting order")
	}
	waiting.paid(waitingSession, "pi_named_from_the_table")
	if status, out := waiting.claim(); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("the buyer whose order was named from the table: status %d, %+v; want their Pass", status, out)
	}
	if status, _ := p.otherBrowser().forgetOrder(p.app.passOrderName("nothing-under-this")); status != http.StatusOK {
		t.Fatalf("forget by a name with nothing under it: status %d", status)
	}
}

// TestPassHashesAreKeyedWithTheSecret holds every hash the Pass tables keep
// of a code or a Stripe id, the name a checkout's answer gives its order, and
// the key a Pass's hourly budget is counted under to EDUCHAN_PASS_SECRET. The
// same deployment started with another secret finds none of them, as the
// README says of rotating it (every Pass cancelled, every payment record
// orphaned): a code bought before is no Pass and its payment is not one to
// reissue, and with them a paid report of a checkout opened before matches no
// order, and an order's name is not the server's. A hash keyed with anything
// but the secret would let whoever holds a copy of the tables check a code
// against them or make an order's name; the server that bought them still
// finds every one.
func TestPassHashesAreKeyedWithTheSecret(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	issued := p.buy("pi_before_the_rotation")
	buyer := p.otherBrowser()
	session, name := buyer.checkoutNamed("")
	q := p.twin(func(c *PassConfig) { c.Secret = testPassSecret + "-rotated" })

	if status, out := q.activate(issued.Pass); status != http.StatusForbidden || refusalState(t, out) != passInvalid {
		t.Fatalf("a code bought under the old secret, checked under the new: status %d, %+v; want 403 invalid", status, out)
	}
	if status, _, _ := q.reissueCode("mod-key", "pi_before_the_rotation"); status != http.StatusNotFound {
		t.Fatalf("a reissue under the new secret of a payment made under the old: status %d; want 404", status)
	}
	q.paid(session, "pi_paid_under_the_new_secret")
	if status, out := buyer.claim(); status != http.StatusAccepted {
		t.Fatalf("a checkout opened under the old secret, reported paid under the new: status %d, %+v; want it still waiting", status, out)
	}
	if status, _ := q.otherBrowser().forgetOrder(name); status != http.StatusBadRequest {
		t.Fatalf("forget under the new secret by a name given under the old: status %d; want 400", status)
	}
	code, _ := parsePassCode(issued.Pass)
	if p.app.passBudgetKey(code) == q.app.passBudgetKey(code) {
		t.Fatal("one Pass's hourly budget is counted under the same key whatever the secret")
	}

	// Under the secret they were made with, every one is still found.
	if status, out := p.activate(issued.Pass); status != http.StatusOK || out.Pass != issued.Pass {
		t.Fatalf("the code under its own secret: status %d, %+v", status, out)
	}
	if status, _, _ := p.reissueCode("mod-key", "pi_before_the_rotation"); status != http.StatusOK {
		t.Fatalf("a reissue under the payment's own secret: status %d", status)
	}
	p.paid(session, "pi_paid_under_its_own_secret")
	if status, out := buyer.claim(); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("the checkout reported paid under its own secret: status %d, %+v", status, out)
	}
	another := p.otherBrowser()
	_, anotherName := another.checkoutNamed("")
	if status, _ := p.otherBrowser().forgetOrder(anotherName); status != http.StatusOK || another.reportsClaim() {
		t.Fatalf("forget by an order's name under its own secret: status %d, or the order still waits", status)
	}
}

// roundTripper is an http.RoundTripper made of a function.
type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// sendingClaimCookies is a browser that sends these claim tokens, in this
// order, on every request, as one holding a claim cookie planted from a
// sibling subdomain beside the server's own does: one Cookie header with all
// of them, which says nothing of whose each is.
func (p *passHarness) sendingClaimCookies(tokens ...string) *passHarness {
	p.t.Helper()
	pairs := make([]string, len(tokens))
	for i, token := range tokens {
		pairs[i] = passClaimCookie + "=" + token
	}
	cookies := strings.Join(pairs, "; ")
	transport := roundTripper(func(req *http.Request) (*http.Response, error) {
		req = req.Clone(req.Context())
		req.Header.Set("Cookie", cookies)
		return http.DefaultTransport.RoundTrip(req)
	})
	h := &harness{client: &client{t: p.t, base: p.base, http: &http.Client{Transport: transport}}, app: p.app}
	return &passHarness{harness: h, stripe: p.stripe}
}

// TestPassReadsEveryClaimCookie holds the Pass endpoints to every claim cookie
// a browser sends. A page on a sibling subdomain can set one of the same name
// for the whole site, at a path the browser sends ahead of the server's own,
// with a checkout of its own under it or nothing: whichever comes first, it
// neither hides the buyer's purchase from the status and the claim, nor keeps
// the buyer's claim from being forgotten. Two paid orders are collected one an
// answer, and a claim asked again after a lost answer gives the Pass made last.
func TestPassReadsEveryClaimCookie(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	empty, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		order func(planted, own string) []string
	}{
		{"planted first", func(planted, own string) []string { return []string{planted, empty, own} }},
		{"the buyer's first", func(planted, own string) []string { return []string{own, empty, planted} }},
	} {
		// What its planter can put under a token: a checkout left unpaid.
		planter := p.otherBrowser()
		planter.checkout("")
		planted, _ := planter.claimCookie()

		buyer := p.otherBrowser()
		buyer.paid(buyer.checkout(""), "pi_beside_"+strings.ReplaceAll(c.name, " ", "_"))
		own, _ := buyer.claimCookie()
		both := buyer.sendingClaimCookies(c.order(planted, own)...)
		if !both.reportsClaim() {
			t.Fatalf("%s: GET /api/pass says there is nothing to collect", c.name)
		}
		nonce := both.collectNonce()
		status, issued := both.claimWith(nonce)
		if status != http.StatusOK || issued.Status != "issued" || !issued.Claim {
			t.Fatalf("%s: the buyer's claim: status %d, %+v; want the Pass, and the planted checkout still waiting", c.name, status, issued)
		}
		if status, again := both.claimWith(nonce); status != http.StatusOK || again.Status != "issued" || again.Pass != issued.Pass {
			t.Fatalf("%s: the buyer's claim asked again: status %d, %+v; want %s again", c.name, status, again, issued.Pass)
		}
		if status, _ := p.activate(issued.Pass); status != http.StatusOK {
			t.Fatalf("%s: the buyer's code: status %d", c.name, status)
		}

		// Forgotten with the planted cookie beside it, the buyer's paid
		// order is dropped, and so is whatever was planted: neither a copy of
		// the buyer's cookie nor the planter's own collects anything after.
		left := p.otherBrowser()
		left.paid(left.checkout(""), "pi_left_"+strings.ReplaceAll(c.name, " ", "_"))
		kept, _ := left.claimCookie()
		if status, out := left.sendingClaimCookies(c.order(planted, kept)...).forget(); status != http.StatusOK || out.Claim {
			t.Fatalf("%s: forget: status %d, %+v", c.name, status, out)
		}
		if left.reportsClaim() || planter.reportsClaim() {
			t.Fatalf("%s: a claim is still waiting after the browser was forgotten: the buyer's %v, the planted %v",
				c.name, left.reportsClaim(), planter.reportsClaim())
		}
		if status, out := left.claim(); status != http.StatusBadRequest || out.Pass != "" {
			t.Fatalf("%s: a copy of the buyer's cookie after forgetting: status %d, %+v", c.name, status, out)
		}
	}

	// Two claims with paid orders are collected one an answer, under the same
	// nonce. Asked again after the second answer was lost, the claim is
	// answered with the Pass made last, not the one handed over before it.
	first := p.otherBrowser()
	first.paid(first.checkout(""), "pi_first_of_two")
	firstToken, _ := first.claimCookie()
	second := p.otherBrowser()
	second.paid(second.checkout(""), "pi_second_of_two")
	secondToken, _ := second.claimCookie()
	two := first.sendingClaimCookies(firstToken, secondToken)
	nonce := two.collectNonce()
	status, one := two.claimWith(nonce)
	if status != http.StatusOK || one.Status != "issued" || !one.Claim {
		t.Fatalf("the first of two paid claims: status %d, %+v; want a Pass and another to come", status, one)
	}
	status, other := two.claimWith(nonce)
	if status != http.StatusOK || other.Status != "issued" || other.Claim || other.Pass == one.Pass {
		t.Fatalf("the second of two paid claims: status %d, %+v; want another Pass and nothing more", status, other)
	}
	if status, again := two.claimWith(nonce); status != http.StatusOK || again.Pass != other.Pass {
		t.Fatalf("asked again after the second answer: status %d, %+v; want %s, the Pass made last", status, again, other.Pass)
	}

	// A token with nothing under it is no claim, however many there are; and
	// past the 16th, cookies are not read at all (passClaimCookieLimit), so one
	// request queues no more lookups than that.
	buyer := p.otherBrowser()
	buyer.paid(buyer.checkout(""), "pi_past_the_limit")
	own, _ := buyer.claimCookie()
	if buyer.sendingClaimCookies(empty).reportsClaim() {
		t.Fatal("a token with nothing under it is reported as a claim")
	}
	// A reissue code typed in the browser tells it the purchase is there.
	p.otherBrowser().buy("pi_lost_beside")
	_, _, code := p.reissueCode("mod-key", "pi_lost_beside")
	if status, out := buyer.sendingClaimCookies(empty, own).redeem(code); status != http.StatusOK || out.Status != "issued" || !out.Claim {
		t.Fatalf("a reissue code typed beside a planted cookie: status %d, %+v; want the purchase still to collect", status, out)
	}
	padding := make([]string, 0, 16)
	for range 15 {
		token, err := randomToken()
		if err != nil {
			t.Fatal(err)
		}
		padding = append(padding, token)
	}
	if !buyer.sendingClaimCookies(append(padding, own)...).reportsClaim() {
		t.Fatalf("the buyer's claim behind %d others is not read", len(padding))
	}
	padding = append(padding, empty)
	if buyer.sendingClaimCookies(append(padding, own)...).reportsClaim() {
		t.Fatalf("a claim behind %d others was read, past the 16th", len(padding))
	}
}

// TestPassRefundTakesOffOnlyWhatIsLeft holds a refund to the part of its
// payment's term still to come: a purchase refunded after its term has run
// takes nothing off the renewal paid after it, where it once took the
// renewal's time and deleted the Pass.
func TestPassRefundTakesOffOnlyWhatIsLeft(t *testing.T) {
	// A month's Pass runs out and is renewed in its grace month; then the
	// purchase is disputed.
	p := passServer(t, func(cfg *Config) { cfg.Pass.Days = 30 })
	issued := p.buy("pi_month")
	lapsed := dayCeil(time.Now()).AddDate(0, 0, -10)
	p.exec(`UPDATE passes SET expires_at = $1`, lapsed)
	p.exec(`UPDATE pass_payments SET ends_at = $1`, lapsed)
	renewed := p.renew(issued.Pass, "pi_next_month")
	p.webhook(stripeDisputeCreated, map[string]any{"object": "dispute", "payment_intent": "pi_month"})
	if got, ok := p.passExpiresAt(issued.Pass); !ok || !got.Equal(renewed.ExpiresAt) {
		t.Fatalf("disputing a purchase whose month had run left the Pass at %v (kept: %v), want %v", got, ok, renewed.ExpiresAt)
	}
	// Refunding the renewal, all of it still to come, ends the Pass.
	p.refund("pi_next_month")
	if status, out := p.activate(issued.Pass); status != http.StatusForbidden || refusalState(t, out) != passInvalid {
		t.Fatalf("a Pass with its only time to come refunded: status %d, %+v", status, out)
	}

	// A year's Pass, renewed at once: the purchase refunded after its year
	// takes nothing off, and the renewal refunded part way through its year
	// takes off what is left of it, to the day.
	q := passServer(t, nil)
	bought := q.buy("pi_year")
	again := q.renew(bought.Pass, "pi_second_year")
	later := time.Now().AddDate(0, 0, 370)
	if res, err := q.app.store.revokePassPayment(context.Background(), q.app.passPaymentHash("pi_year"), later); err != nil || res != revokedTerm {
		t.Fatalf("refund of the purchase after its year: %v, %v", res, err)
	}
	if got, ok := q.passExpiresAt(bought.Pass); !ok || !got.Equal(again.ExpiresAt) {
		t.Fatalf("refunding a purchase whose year had run left the Pass at %v (kept: %v), want %v", got, ok, again.ExpiresAt)
	}
	if res, err := q.app.store.revokePassPayment(context.Background(), q.app.passPaymentHash("pi_second_year"), later); err != nil || res != revokedPass {
		t.Fatalf("refund of the renewal part way through: %v, %v", res, err)
	}

	// Bought and renewed twice, a year each. The purchase refunded with 265
	// and a half days of it left takes the 265 whole days off, so it stops at
	// the next midnight and the renewals come forward whole: the first now
	// runs a year from that midnight, to 100 days past the purchase's end, not
	// 365, rather than losing the half day the refund would otherwise round
	// off it. Refunded ten days later, that renewal takes off the 355 whole
	// days left of it, and the last renewal keeps its whole year, from the
	// midnight after that refund.
	r := passServer(t, nil)
	first := r.buy("pi_three_terms")
	end := first.ExpiresAt
	r.renew(first.Pass, "pi_three_terms_second")
	r.renew(first.Pass, "pi_three_terms_third")
	endsAt := func(paymentIntent string) time.Time {
		t.Helper()
		var ends time.Time
		if err := r.app.pool.QueryRow(context.Background(), `SELECT ends_at FROM pass_payments WHERE payment_hash = $1`,
			r.app.passPaymentHash(paymentIntent)).Scan(&ends); err != nil {
			t.Fatalf("the term of %s: %v", paymentIntent, err)
		}
		return ends
	}
	refundedAt := end.Add(-(265*24 + 12) * time.Hour)
	if res, err := r.app.store.revokePassPayment(context.Background(), r.app.passPaymentHash("pi_three_terms"), refundedAt); err != nil || res != revokedTerm {
		t.Fatalf("refund of the purchase: %v, %v", res, err)
	}
	if got, ok := r.passExpiresAt(first.Pass); !ok || !got.Equal(end.AddDate(0, 0, 465)) {
		t.Fatalf("after refunding the purchase the Pass runs to %v (kept: %v), want %v", got, ok, end.AddDate(0, 0, 465))
	}
	if got, want := endsAt("pi_three_terms_second"), dayCeil(refundedAt).AddDate(0, 0, 365); !got.Equal(want) || !want.Equal(end.AddDate(0, 0, 100)) {
		t.Fatalf("after refunding the purchase the next renewal ends %v, want a whole year from the midnight after the refund, %v", got, want)
	}
	secondRefund := refundedAt.AddDate(0, 0, 10)
	if res, err := r.app.store.revokePassPayment(context.Background(), r.app.passPaymentHash("pi_three_terms_second"), secondRefund); err != nil || res != revokedTerm {
		t.Fatalf("refund of the first renewal: %v, %v", res, err)
	}
	if got, ok := r.passExpiresAt(first.Pass); !ok || !got.Equal(end.AddDate(0, 0, 110)) {
		t.Fatalf("after refunding the first renewal the Pass runs to %v (kept: %v), want %v", got, ok, end.AddDate(0, 0, 110))
	}
	if got, want := endsAt("pi_three_terms_third"), dayCeil(secondRefund).AddDate(0, 0, 365); !got.Equal(want) || !want.Equal(end.AddDate(0, 0, 110)) {
		t.Fatalf("the last renewal's term ends %v, want a whole year from the midnight after the refund, %v", got, want)
	}
	// Refunded in its turn an hour later, the last renewal leaves only the
	// rest of the day the first renewal stops at, which no payment left pays
	// for, and the Pass is cancelled at once, as one with no time left is.
	if res, err := r.app.store.revokePassPayment(context.Background(), r.app.passPaymentHash("pi_three_terms_third"), secondRefund.Add(time.Hour)); err != nil || res != revokedPass {
		t.Fatalf("refund of the last renewal the same day: %v, %v; want the Pass cancelled", res, err)
	}
	if _, ok := r.passExpiresAt(first.Pass); ok {
		t.Fatal("a Pass none of whose payments has time to come is kept for the rest of the day")
	}
}

// TestPassReissueAfterTheOriginIsRefunded holds the right to reissue a Pass,
// which hands over the whole of it, to a payment that paid for all the time
// it has left. With the purchase refunded, and a renewal by somebody who saw
// the code once and one by the holder both still to come, neither payment
// reissues the Pass, and the holder keeps it; the one renewal left with time
// to come takes the purchase's place, whether the others were refunded or had
// run their terms, until the Pass is renewed again. While the purchase is the
// origin, no renewal becomes a second one.
func TestPassReissueAfterTheOriginIsRefunded(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	origins := func(code string) int {
		t.Helper()
		canonical, _ := parsePassCode(code)
		var n int
		if err := p.app.pool.QueryRow(context.Background(), `
			SELECT count(*) FILTER (WHERE origin IS NOT NULL) FROM pass_payments
			WHERE pass_id = (SELECT id FROM passes WHERE token_hash = $1)`, p.app.passTokenHash(canonical)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	held := p.buy("pi_holder_bought")
	p.otherBrowser().renew(held.Pass, "pi_onlooker")
	p.renew(held.Pass, "pi_holder_renewed")
	p.refund("pi_holder_bought")
	if status, _ := p.activate(held.Pass); status != http.StatusOK {
		t.Fatalf("refunding the purchase ended a Pass with renewals: status %d", status)
	}
	if status, _, _ := p.reissueCode("mod-key", "pi_holder_bought"); status != http.StatusNotFound {
		t.Fatalf("reissue with the refunded purchase: status %d", status)
	}
	for _, pi := range []string{"pi_onlooker", "pi_holder_renewed"} {
		if status, out, code := p.reissueCode("mod-key", pi); status != http.StatusConflict || out.Error != passReissueNoOrigin || code != "" {
			t.Fatalf("reissue with %s, one of two renewals to come: status %d, %+v", pi, status, out)
		}
	}
	if n := origins(held.Pass); n != 0 {
		t.Fatalf("a Pass with two renewals to come has %d origins after its purchase was refunded, want none", n)
	}
	if status, _ := p.activate(held.Pass); status != http.StatusOK {
		t.Fatalf("the holder lost their Pass: status %d", status)
	}

	// The onlooker's renewal is refunded in its turn: the holder's is then
	// the one payment left with time to come, and reissues the Pass.
	p.refund("pi_onlooker")
	if n := origins(held.Pass); n != 1 {
		t.Fatalf("one renewal left with time to come: %d origins, want 1", n)
	}
	status, _, code := p.reissueCode("mod-key", "pi_holder_renewed")
	if status != http.StatusOK {
		t.Fatalf("reissue with the one renewal left: status %d", status)
	}
	status, fresh := p.otherBrowser().redeem(code)
	if status != http.StatusOK || fresh.Status != "issued" || fresh.Pass == held.Pass {
		t.Fatalf("redeem: status %d, %+v", status, fresh)
	}
	if status, _ := p.activate(held.Pass); status != http.StatusForbidden {
		t.Fatalf("the replaced code still works: status %d", status)
	}

	// One renewal and the purchase: refunded, the purchase leaves the renewal
	// paying for all the time there is, and it takes the purchase's place.
	single := p.buy("pi_single")
	p.renew(single.Pass, "pi_single_renewal")
	p.refund("pi_single")
	if status, _, _ := p.reissueCode("mod-key", "pi_single_renewal"); status != http.StatusOK {
		t.Fatalf("reissue with the only renewal: status %d", status)
	}

	// The renewal the right moved to loses it once the Pass is renewed again,
	// by its holder say: it no longer pays for all the time the Pass has left,
	// and reissuing the Pass to its payer would hand over the year renewed
	// after it. A reissue code made for it before goes with the right.
	kept := p.buy("pi_kept")
	onlooker := p.otherBrowser()
	onlooker.renew(kept.Pass, "pi_kept_onlooker")
	p.refund("pi_kept")
	if n := origins(kept.Pass); n != 1 {
		t.Fatalf("the onlooker's renewal left alone with time to come: %d origins, want 1", n)
	}
	status, _, early := p.reissueCode("mod-key", "pi_kept_onlooker")
	if status != http.StatusOK {
		t.Fatalf("reissue with the renewal the right moved to: status %d", status)
	}
	onlookersPayment := p.app.passPaymentHash("pi_kept_onlooker")
	if n := p.rowsContaining("pass_reissues", onlookersPayment); n != 1 {
		t.Fatalf("the reissue code made is in %d pass_reissues rows, want 1", n)
	}
	renewedAfter := p.renew(kept.Pass, "pi_kept_renewed_after")
	if n := origins(kept.Pass); n != 0 {
		t.Fatalf("a Pass renewed after its right to reissue moved has %d origins, want none", n)
	}
	if n := p.rowsContaining("pass_reissues", onlookersPayment); n != 0 {
		t.Fatalf("the renewal's reissue code is kept after it lost the right: %d rows", n)
	}
	for _, pi := range []string{"pi_kept_onlooker", "pi_kept_renewed_after"} {
		if status, out, code := p.reissueCode("mod-key", pi); status != http.StatusConflict || out.Error != passReissueNoOrigin || code != "" {
			t.Fatalf("reissue with %s after the holder renewed: status %d, %+v", pi, status, out)
		}
	}
	if status, out := onlooker.redeem(early); status != http.StatusForbidden || out.Error != passReissueSpent || out.Pass != passInvalid {
		t.Fatalf("a reissue code made before the holder renewed, used after: status %d, %+v", status, out)
	}
	if status, out := p.activate(kept.Pass); status != http.StatusOK || !out.ExpiresAt.Equal(renewedAfter.ExpiresAt) {
		t.Fatalf("the holder lost their Pass: status %d, %+v", status, out)
	}

	// While the purchase is the origin, a refund that leaves one renewal with
	// time to come makes it no second origin. With the purchase's own year
	// run, and a renewal by somebody who saw the code running, the holder's
	// later renewal refunded leaves the purchase the Pass's one origin.
	guarded := p.buy("pi_guarded")
	p.otherBrowser().renew(guarded.Pass, "pi_guarded_onlooker")
	p.renew(guarded.Pass, "pi_guarded_holder")
	guardedCode, _ := parsePassCode(guarded.Pass)
	guardedHash := p.app.passTokenHash(guardedCode)
	p.exec(`UPDATE passes SET expires_at = expires_at - make_interval(hours => 24 * 366) WHERE token_hash = $1`, guardedHash)
	p.exec(`UPDATE pass_payments SET ends_at = ends_at - make_interval(hours => 24 * 366)
		WHERE pass_id = (SELECT id FROM passes WHERE token_hash = $1)`, guardedHash)
	p.refund("pi_guarded_holder")
	if status, out, _ := p.reissueCode("mod-key", "pi_guarded_onlooker"); status != http.StatusConflict || out.Error != passReissueRenewal {
		t.Fatalf("reissue with a renewal left alone with time to come beside the purchase: status %d, %+v", status, out)
	}
	if n := origins(guarded.Pass); n != 1 {
		t.Fatalf("the purchase and a renewal left alone with time to come: %d origins, want the purchase alone", n)
	}
	if status, _, _ := p.reissueCode("mod-key", "pi_guarded"); status != http.StatusOK {
		t.Fatalf("reissue with the purchase whose year has run: status %d", status)
	}

	// A renewal whose term has run does not stand in the way: with the
	// purchase's and the first renewal's terms ended, the purchase refunded
	// leaves the last renewal paying for all the time left.
	ran := p.buy("pi_ran")
	p.renew(ran.Pass, "pi_ran_renewal")
	p.renew(ran.Pass, "pi_running_renewal")
	for pi, ends := range map[string]time.Time{
		"pi_ran":             dayCeil(time.Now()).AddDate(0, 0, -20),
		"pi_ran_renewal":     dayCeil(time.Now()).AddDate(0, 0, -10),
		"pi_running_renewal": dayCeil(time.Now()).AddDate(0, 0, 355),
	} {
		p.exec(`UPDATE pass_payments SET ends_at = $1 WHERE payment_hash = $2`, ends, p.app.passPaymentHash(pi))
	}
	canonical, _ := parsePassCode(ran.Pass)
	p.exec(`UPDATE passes SET expires_at = $1 WHERE token_hash = $2`, dayCeil(time.Now()).AddDate(0, 0, 355), p.app.passTokenHash(canonical))
	p.refund("pi_ran")
	if status, out, _ := p.reissueCode("mod-key", "pi_ran_renewal"); status != http.StatusConflict || out.Error != passReissueRenewal {
		t.Fatalf("reissue with a renewal whose term has run: status %d, %+v", status, out)
	}
	if status, _, _ := p.reissueCode("mod-key", "pi_running_renewal"); status != http.StatusOK {
		t.Fatalf("reissue with the one renewal left running: status %d", status)
	}
}

// originOf reads a payment's right to reissue its Pass: "bought", "moved",
// none ("") or, for a payment on no Pass, "no payment".
func (p *passHarness) originOf(paymentIntent string) string {
	p.t.Helper()
	var origin *string
	err := p.app.pool.QueryRow(context.Background(), `SELECT origin FROM pass_payments WHERE payment_hash = $1`,
		p.app.passPaymentHash(paymentIntent)).Scan(&origin)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "no payment"
	case err != nil:
		p.t.Fatal(err)
	case origin == nil:
		return ""
	}
	return *origin
}

// TestPassReissueWaitsForARenewalUnderWay holds the right to reissue a Pass
// that moved to a renewal, when the purchase was refunded, to the time that
// renewal paid for. A renewal of the Pass under way is somebody's payment,
// which would land on the Pass handed over: the holder's, paid and not
// collected yet, opened and not paid yet, or paid between the reissue code's
// making and its use. A paid renewal waiting to be collected keeps the right
// from moving at all, and is counted again if it is refunded; while any
// renewal is under way, the right that did move gives no reissue code, and one
// made before is refused without being spent, to work again if the renewal
// comes to nothing. An unpaid one counts until Stripe closes it or it is
// pruned past its deadline, since until then it can still be paid. Collected,
// the renewal stays on the holder's code.
func TestPassReissueWaitsForARenewalUnderWay(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	refusedNoOrigin := func(paymentIntent, when string) {
		t.Helper()
		if status, out, code := p.reissueCode("mod-key", paymentIntent); status != http.StatusConflict ||
			out.Error != passReissueNoOrigin || code != "" {
			t.Fatalf("reissue with %s %s: status %d, %+v", paymentIntent, when, status, out)
		}
	}

	// The purchase keeps its right while a renewal is under way: its payer
	// bought the Pass, and the renewal lands on the code it has then.
	bought := p.buy("pi_bought")
	p.paid(p.checkout(bought.Pass), "pi_bought_renewal")
	status, _, token := p.reissueCode("mod-key", "pi_bought")
	if status != http.StatusOK {
		t.Fatalf("reissue with the purchase while a renewal of it waits to be collected: status %d", status)
	}
	status, reissued := p.otherBrowser().redeem(token)
	if status != http.StatusOK || reissued.Status != "issued" {
		t.Fatalf("redeem with the purchase while a renewal of it waits to be collected: status %d, %+v", status, reissued)
	}
	if status, out := p.claim(); status != http.StatusOK || out.Status != "renewed" {
		t.Fatalf("the renewal collected after the reissue: status %d, %+v", status, out)
	}

	// Paid and not collected when the purchase is refunded: the holder's
	// renewal is a term to come beside the onlooker's, so neither pays for
	// all the Pass's time and the right moves to neither. Collected, the
	// renewal is on the holder's code, and the Pass still cannot be
	// reissued.
	held := p.buy("pi_paid_bought")
	p.otherBrowser().renew(held.Pass, "pi_paid_onlooker")
	p.paid(p.checkout(held.Pass), "pi_paid_renewal")
	p.refund("pi_paid_bought")
	if got := p.originOf("pi_paid_onlooker"); got != "" {
		t.Fatalf("the onlooker's renewal beside the holder's paid one: origin %q, want none", got)
	}
	refusedNoOrigin("pi_paid_onlooker", "while the holder's paid renewal waits to be collected")
	status, renewed := p.claim()
	if status != http.StatusOK || renewed.Status != "renewed" {
		t.Fatalf("the holder collects the renewal: status %d, %+v", status, renewed)
	}
	if status, out := p.activate(held.Pass); status != http.StatusOK || !out.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatalf("the holder's code after collecting the renewal: status %d, %+v; want good until %v", status, out, renewed.ExpiresAt)
	}
	refusedNoOrigin("pi_paid_onlooker", "after the holder collected the renewal")

	// Refunded before it is collected, the holder's renewal leaves the
	// onlooker's the one payment paying for all the time there is, and the
	// right moves to it then.
	refunded := p.buy("pi_refunded_bought")
	p.otherBrowser().renew(refunded.Pass, "pi_refunded_onlooker")
	p.paid(p.checkout(refunded.Pass), "pi_refunded_renewal")
	p.refund("pi_refunded_bought")
	if got := p.originOf("pi_refunded_onlooker"); got != "" {
		t.Fatalf("the onlooker's renewal beside the holder's paid one: origin %q, want none", got)
	}
	p.refund("pi_refunded_renewal")
	if got := p.originOf("pi_refunded_onlooker"); got != "moved" {
		t.Fatalf("the onlooker's renewal once the holder's uncollected one was refunded: origin %q, want moved", got)
	}
	if status, _, _ := p.reissueCode("mod-key", "pi_refunded_onlooker"); status != http.StatusOK {
		t.Fatalf("reissue with the one renewal left: status %d", status)
	}

	// Opened and not paid yet when the purchase is refunded: the right moves
	// to the onlooker's renewal, but gives no reissue code while the
	// checkout is open, or once it is paid; collected, the renewal takes the
	// right away and stays on the holder's code.
	opened := p.buy("pi_opened_bought")
	p.otherBrowser().renew(opened.Pass, "pi_opened_onlooker")
	session := p.checkout(opened.Pass)
	p.refund("pi_opened_bought")
	if got := p.originOf("pi_opened_onlooker"); got != "moved" {
		t.Fatalf("the onlooker's renewal beside an unpaid checkout: origin %q, want moved", got)
	}
	refusedNoOrigin("pi_opened_onlooker", "while the holder's renewal checkout is open")
	p.paid(session, "pi_opened_renewal")
	refusedNoOrigin("pi_opened_onlooker", "while the holder's renewal is paid and not collected")
	status, renewed = p.claim()
	if status != http.StatusOK || renewed.Status != "renewed" {
		t.Fatalf("the holder collects the renewal: status %d, %+v", status, renewed)
	}
	if status, out := p.activate(opened.Pass); status != http.StatusOK || !out.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatalf("the holder's code after collecting the renewal: status %d, %+v; want good until %v", status, out, renewed.ExpiresAt)
	}
	refusedNoOrigin("pi_opened_onlooker", "after the holder collected the renewal")
	// A checkout Stripe closes unpaid is nothing under way.
	lapsed := p.buy("pi_lapsed_bought")
	p.otherBrowser().renew(lapsed.Pass, "pi_lapsed_onlooker")
	session = p.checkout(lapsed.Pass)
	p.refund("pi_lapsed_bought")
	refusedNoOrigin("pi_lapsed_onlooker", "while the holder's renewal checkout is open")
	p.webhook(stripeCheckoutExpired, map[string]any{"id": session, "object": "checkout.session", "mode": "payment", "payment_status": "unpaid"})
	if status, _, _ := p.reissueCode("mod-key", "pi_lapsed_onlooker"); status != http.StatusOK {
		t.Fatalf("reissue with the one renewal left once the holder's checkout closed unpaid: status %d", status)
	}
	// An unpaid order past its deadline is under way until it is pruned,
	// since Stripe can still report it paid until then. Pruned, it is gone,
	// and a payment for it after all renews nothing.
	late := p.buy("pi_late_bought")
	latecomer := p.otherBrowser()
	latecomer.renew(late.Pass, "pi_late_onlooker")
	session = p.checkout(late.Pass)
	p.refund("pi_late_bought")
	if _, err := p.app.pool.Exec(context.Background(), `
		UPDATE pass_orders SET expires_at = now() - interval '1 day'
		WHERE renew_pass_id = (SELECT pass_id FROM pass_payments WHERE payment_hash = $1)`,
		p.app.passPaymentHash("pi_late_onlooker")); err != nil {
		t.Fatal(err)
	}
	refusedNoOrigin("pi_late_onlooker", "while the holder's unpaid checkout is past its deadline and not pruned yet")
	if err := p.app.store.prunePasses(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	status, _, lateCode := p.reissueCode("mod-key", "pi_late_onlooker")
	if status != http.StatusOK {
		t.Fatalf("reissue with the one renewal left once the holder's unpaid checkout was pruned: status %d", status)
	}
	status, taken := latecomer.redeem(lateCode)
	if status != http.StatusOK || taken.Status != "issued" {
		t.Fatalf("redeem once the holder's unpaid checkout was pruned: status %d, %+v", status, taken)
	}
	p.paid(session, "pi_late_renewal")
	if status, out := p.claim(); status == http.StatusOK {
		t.Fatalf("the pruned renewal, paid after all, was collected: %+v", out)
	}
	if status, out := p.activate(taken.Pass); status != http.StatusOK || !out.ExpiresAt.Equal(taken.ExpiresAt) {
		t.Fatalf("the reissued code once the pruned renewal was paid: status %d, %+v; want good until %v", status, out, taken.ExpiresAt)
	}

	// Made with nothing under way, and used after the holder paid to renew:
	// the reissue code is refused, not spent, and the holder keeps the Pass.
	// Collected, the renewal stays on the holder's code and cancels the
	// reissue code.
	between := p.buy("pi_between_bought")
	onlooker := p.otherBrowser()
	onlooker.renew(between.Pass, "pi_between_onlooker")
	p.refund("pi_between_bought")
	status, _, code := p.reissueCode("mod-key", "pi_between_onlooker")
	if status != http.StatusOK {
		t.Fatalf("reissue with the renewal the right moved to: status %d", status)
	}
	p.paid(p.checkout(between.Pass), "pi_between_renewal")
	if status, out := onlooker.redeem(code); status != http.StatusConflict || out.Error != passReissueRenewing || out.Pass != "" {
		t.Fatalf("a reissue code used while the holder's paid renewal waits to be collected: status %d, %+v", status, out)
	}
	if n := p.rowsContaining("pass_reissues", p.app.passPaymentHash("pi_between_onlooker")); n != 1 {
		t.Fatalf("the refused reissue code is in %d pass_reissues rows, want 1: it was spent", n)
	}
	status, renewed = p.claim()
	if status != http.StatusOK || renewed.Status != "renewed" {
		t.Fatalf("the holder collects the renewal: status %d, %+v", status, renewed)
	}
	if status, out := p.activate(between.Pass); status != http.StatusOK || !out.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatalf("the holder's code after collecting the renewal: status %d, %+v; want good until %v", status, out, renewed.ExpiresAt)
	}
	if status, out := onlooker.redeem(code); status != http.StatusForbidden || out.Error != passReissueSpent {
		t.Fatalf("the reissue code after the holder collected the renewal: status %d, %+v", status, out)
	}
	// Paid, and refunded before it is collected, the renewal comes to
	// nothing, and the reissue code works again.
	again := p.buy("pi_again_bought")
	taker := p.otherBrowser()
	taker.renew(again.Pass, "pi_again_onlooker")
	p.refund("pi_again_bought")
	_, _, code = p.reissueCode("mod-key", "pi_again_onlooker")
	session = p.checkout(again.Pass)
	if status, out := taker.redeem(code); status != http.StatusConflict || out.Error != passReissueRenewing {
		t.Fatalf("a reissue code used while the holder's renewal checkout is open: status %d, %+v", status, out)
	}
	p.paid(session, "pi_again_renewal")
	if status, out := taker.redeem(code); status != http.StatusConflict || out.Error != passReissueRenewing {
		t.Fatalf("a reissue code used while the holder's paid renewal waits to be collected: status %d, %+v", status, out)
	}
	p.refund("pi_again_renewal")
	status, fresh := taker.redeem(code)
	if status != http.StatusOK || fresh.Status != "issued" || fresh.Pass == again.Pass {
		t.Fatalf("the reissue code once the holder's renewal was refunded: status %d, %+v", status, fresh)
	}
	if status, _ := p.activate(fresh.Pass); status != http.StatusOK {
		t.Fatalf("the reissued code: status %d", status)
	}
}

// TestPassRenewalCheckoutRacingAReissue holds a renewal checkout to the code
// it was started with. The holder's checkout reads the Pass without the lock
// and then waits on Stripe for a session; a reissue code for the right that
// moved to the onlooker's renewal, used meanwhile, finds no order under way
// and gives the Pass another code. The order, looked up again by the holder's
// code, is refused as a renewal of a Pass that is gone, so the year the
// holder would pay for never lands on the Pass handed to the onlooker.
func TestPassRenewalCheckoutRacingAReissue(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	held := p.buy("pi_raced_bought")
	onlooker := p.otherBrowser()
	onlooker.renew(held.Pass, "pi_raced_onlooker")
	p.refund("pi_raced_bought")
	if got := p.originOf("pi_raced_onlooker"); got != "moved" {
		t.Fatalf("the onlooker's renewal once the purchase was refunded: origin %q, want moved", got)
	}
	_, _, code := p.reissueCode("mod-key", "pi_raced_onlooker")
	if code == "" {
		t.Fatal("no reissue code for the renewal the right moved to")
	}
	before, _ := p.claimCookie()

	var redeemed passResponse
	redeemStatus := 0
	p.stripe.mu.Lock()
	p.stripe.opening = func() {
		if redeemStatus == 0 {
			redeemStatus, redeemed = onlooker.redeem(code)
		}
	}
	p.stripe.mu.Unlock()
	var out passResponse
	status := p.post("/api/pass/checkout", map[string]any{"adult": true, "renew": held.Pass}, &out)
	p.stripe.mu.Lock()
	p.stripe.opening = nil
	p.stripe.mu.Unlock()

	if redeemStatus != http.StatusOK || redeemed.Status != "issued" {
		t.Fatalf("the reissue code used while the holder's checkout was opening: status %d, %+v", redeemStatus, redeemed)
	}
	if status != http.StatusConflict || out.Pass != passInvalid || !strings.Contains(out.Error, "buy a new one") {
		t.Fatalf("the holder's renewal checkout once the code was reissued: status %d, %+v", status, out)
	}
	if n := p.count("pass_orders"); n != 0 {
		t.Fatalf("the refused renewal left %d orders", n)
	}
	if after, _ := p.claimCookie(); after != before || p.reportsClaim() {
		t.Fatal("the refused renewal gave the browser a claim")
	}
	if status, got := p.activate(redeemed.Pass); status != http.StatusOK || !got.ExpiresAt.Equal(redeemed.ExpiresAt) {
		t.Fatalf("the reissued code: status %d, %+v; want good until %v", status, got, redeemed.ExpiresAt)
	}
}

// TestPassOrderKeepsItsTerm holds each order to the term it was sold with:
// EDUCHAN_PASS_DAYS changed after a checkout changes nothing it bought,
// whether it is collected as a Pass or a renewal, or reissued uncollected.
func TestPassOrderKeepsItsTerm(t *testing.T) {
	p := passServer(t, func(cfg *Config) {
		cfg.ModKey = "mod-key"
		cfg.Pass.Days = 30
	})
	bought := p.checkout("")
	uncollected := p.otherBrowser().checkout("")
	// The operator sells two years from now on, and restarts.
	q := p.twin(func(c *PassConfig) { c.Days = 730 })
	var status passResponse
	if q.get("/api/pass", &status); status.Days != 730 {
		t.Fatalf("the restarted server sells %d days", status.Days)
	}
	q.paid(bought, "pi_month")
	code, issued := q.claim()
	if want := passExpiry(time.Now(), 30); code != http.StatusOK || issued.Status != "issued" || !issued.ExpiresAt.Equal(want) {
		t.Fatalf("a month's order collected after the change: status %d, %+v; want it to run to %v", code, issued, want)
	}
	q.paid(p.checkout(issued.Pass), "pi_month_renewal")
	if code, renewed := q.claim(); code != http.StatusOK || renewed.Status != "renewed" ||
		!renewed.ExpiresAt.Equal(issued.ExpiresAt.AddDate(0, 0, 30)) {
		t.Fatalf("a month's renewal collected after the change: status %d, %+v", code, renewed)
	}
	q.paid(uncollected, "pi_uncollected_month")
	_, _, token := q.reissueCode("mod-key", "pi_uncollected_month")
	if code, out := q.otherBrowser().redeem(token); code != http.StatusOK || out.Status != "issued" ||
		!out.ExpiresAt.Equal(passExpiry(time.Now(), 30)) {
		t.Fatalf("a month's order reissued after the change: status %d, %+v", code, out)
	}
}

// TestPassSalesPaused holds EDUCHAN_PASS_SALES=paused to stopping sales and
// nothing else: checkout refuses a purchase and a renewal alike, and the page
// is told; webhooks are verified and recorded, a paid order is collected, a
// Pass still posts and checks, and reissue codes and forgetting still work.
// Open, the default, sells.
func TestPassSalesPaused(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	p.addBoard("paused-board")
	var open passResponse
	if p.get("/api/pass", &open); !open.Enabled || open.Paused || open.Price == nil {
		t.Fatalf("open: %+v", open)
	}
	held := p.buy("pi_before_pause")
	pending := p.checkout("")

	q := p.twin(func(c *PassConfig) { c.SalesPaused = true })
	prices := q.stripe.priceReadCount()
	var paused passResponse
	if q.get("/api/pass", &paused); !paused.Enabled || !paused.Paused || paused.Price != nil || !paused.Claim {
		t.Fatalf("paused: %+v", paused)
	}
	sessions := q.stripe.sessionCount()
	for name, body := range map[string]map[string]any{
		"a purchase": {"adult": true},
		"a renewal":  {"adult": true, "renew": held.Pass},
		// Refused before the body is read: a body that would be refused for
		// itself is told sales are paused, the reason that will not change
		// by asking again.
		"no box":           {},
		"the box unticked": {"adult": false},
		"an unknown field": {"adult": true, "email": "buyer@example.com"},
	} {
		var out passResponse
		if status := q.post("/api/pass/checkout", body, &out); status != http.StatusForbidden || out.Error != passNotOnSale {
			t.Errorf("%s while paused: status %d, %+v", name, status, out)
		}
	}
	malformed, err := http.NewRequest(http.MethodPost, q.base+"/api/pass/checkout", strings.NewReader(`{"adult":`))
	if err != nil {
		t.Fatal(err)
	}
	malformed.Header.Set("Content-Type", "application/json")
	var out passResponse
	if status := q.send(malformed, &out); status != http.StatusForbidden || out.Error != passNotOnSale {
		t.Errorf("a malformed body while paused: status %d, %+v", status, out)
	}
	if n := q.stripe.sessionCount() - sessions; n != 0 {
		t.Fatalf("Stripe opened %d sessions while sales were paused", n)
	}
	if n := q.stripe.priceReadCount() - prices; n != 0 {
		t.Fatalf("the price was asked for %d times while sales were paused", n)
	}

	// Webhooks are still checked, and still recorded.
	forged, _ := json.Marshal(map[string]any{"type": stripeCheckoutCompleted, "data": map[string]any{"object": map[string]any{
		"id": pending, "mode": "payment", "payment_status": "paid", "payment_intent": "pi_forged"}}})
	if status := q.rawWebhook(forged, signStripe(forged, "whsec_other", time.Now())); status != http.StatusBadRequest {
		t.Fatalf("a forged webhook while paused: status %d", status)
	}
	q.paid(pending, "pi_during_pause")
	status, collected := q.claim()
	if status != http.StatusOK || collected.Status != "issued" {
		t.Fatalf("collecting a purchase paid while paused: status %d, %+v", status, collected)
	}
	for _, code := range []string{held.Pass, collected.Pass} {
		if status, _ := q.activate(code); status != http.StatusOK {
			t.Errorf("checking a code while paused: status %d", status)
		}
		if status, out := q.postWithPass("paused-board", code, "", nil); status != http.StatusCreated || out.Pass != "" {
			t.Errorf("posting with a Pass while paused: status %d, %+v", status, out)
		}
	}
	_, _, token := q.reissueCode("mod-key", "pi_before_pause")
	if status, out := q.otherBrowser().redeem(token); status != http.StatusOK || out.Status != "issued" {
		t.Fatalf("a reissue while paused: status %d, %+v", status, out)
	}
	q.refund("pi_during_pause")
	if status, _ := q.activate(collected.Pass); status != http.StatusForbidden {
		t.Fatalf("a refund while paused was not recorded: status %d", status)
	}
	if status, _ := q.forget(); status != http.StatusOK {
		t.Fatalf("forget while paused: status %d", status)
	}
}

// TestPassRefusedCheckoutTakesNoLock holds a checkout refused by its cooldown
// to an answer before the Pass lock, so a burst of them queues no webhook or
// claim behind it.
func TestPassRefusedCheckoutTakesNoLock(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.Pass.CheckoutCooldown = time.Hour })
	p.checkout("")
	lock := p.holdPassLock()
	defer lock.release()
	answer := p.fire(p.request("/api/pass/checkout", map[string]any{"adult": true}, nil))
	select {
	case r := <-answer:
		if r.err != nil || r.status != http.StatusTooManyRequests {
			t.Fatalf("a checkout inside the cooldown: status %d, %v", r.status, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a checkout refused by its cooldown waited on the Pass lock")
	}
}

// TestPassOrderTakesTheLock holds the write a checkout makes after Stripe
// answers, its order, to the Pass lock: taken while Stripe is opening the
// session, the lock holds the order back.
func TestPassOrderTakesTheLock(t *testing.T) {
	p := passServer(t, nil)
	p.checkout("")
	reached, proceed := make(chan struct{}), make(chan struct{})
	p.stripe.mu.Lock()
	p.stripe.opening = func() {
		close(reached)
		<-proceed
	}
	p.stripe.mu.Unlock()
	answer := p.fire(p.request("/api/pass/checkout", map[string]any{"adult": true}, nil))
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the checkout never asked Stripe for a session")
	}
	lock := p.holdPassLock()
	close(proceed)
	lock.queued(1, "the checkout's order")
	lock.release()
	if r := <-answer; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("checkout: status %d, %v (%s)", r.status, r.err, r.body.Error)
	}
	var claims, orders int
	if err := p.app.pool.QueryRow(context.Background(), `
		SELECT count(DISTINCT claim_hash), count(*) FROM pass_orders`).Scan(&claims, &orders); err != nil || claims != 2 || orders != 2 {
		t.Fatalf("%d orders under %d claims (%v), want one under each of 2", orders, claims, err)
	}
}

// TestPassRevocationIsDayRounded holds the record of a refund to the day,
// like every other date in the Pass tables.
func TestPassRevocationIsDayRounded(t *testing.T) {
	p := passServer(t, nil)
	p.buy("pi_rounded")
	p.refund("pi_rounded")
	p.refund("pi_not_ours")
	rows, err := p.app.pool.Query(context.Background(), `SELECT expires_at FROM pass_revocations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		n++
		month := 30 * 24 * time.Hour
		if !at.Equal(dayCeil(at)) || at.Before(time.Now().Add(month)) || at.After(dayCeil(time.Now().Add(month))) {
			t.Errorf("a refund is remembered until %v, want the first UTC midnight 30 days on", at.UTC())
		}
	}
	if n != 2 {
		t.Fatalf("%d refunds remembered, want 2", n)
	}
}

// TestPassRefundCancelsAPassInItsGraceMonth holds a refund or dispute of any
// payment on a Pass that has run out, and can still be renewed for its grace
// month, to cancelling it outright, as the README says: nothing of its time is
// left to take off, so the code the buyer was paid back for cannot be renewed.
func TestPassRefundCancelsAPassInItsGraceMonth(t *testing.T) {
	for _, c := range []struct{ name, event, payment string }{
		{"refund of the renewal", stripeChargeRefunded, "pi_grace_renewal"},
		{"dispute of the purchase", stripeDisputeCreated, "pi_grace_bought"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := passServer(t, nil)
			issued := p.buy("pi_grace_bought")
			p.renew(issued.Pass, "pi_grace_renewal")
			// Run out five days ago, every term with it.
			expires, _ := p.passExpiresAt(issued.Pass)
			shift := time.Until(expires) + 5*24*time.Hour
			p.exec(`UPDATE passes SET expires_at = expires_at - make_interval(secs => $1)`, shift.Seconds())
			p.exec(`UPDATE pass_payments SET ends_at = ends_at - make_interval(secs => $1)`, shift.Seconds())
			if status, out := p.activate(issued.Pass); status != http.StatusForbidden || refusalState(t, out) != passExpired {
				t.Fatalf("the Pass before the refund: status %d, %+v; want it run out and still on file", status, out)
			}
			object := map[string]any{"object": "charge", "payment_intent": c.payment, "refunded": true}
			if c.event == stripeDisputeCreated {
				object = map[string]any{"object": "dispute", "payment_intent": c.payment}
			}
			if status := p.webhook(c.event, object); status != http.StatusOK {
				t.Fatalf("%s: status %d", c.event, status)
			}
			if n := p.count("passes"); n != 0 {
				t.Fatalf("%s left %d Passes in their grace month", c.name, n)
			}
			if status, out := p.activate(issued.Pass); status != http.StatusForbidden || refusalState(t, out) != passInvalid {
				t.Fatalf("the Pass after the refund: status %d, %+v", status, out)
			}
			var renewal passResponse
			if status := p.post("/api/pass/checkout", map[string]any{"adult": true, "renew": issued.Pass}, &renewal); status != http.StatusForbidden {
				t.Fatalf("renewing a Pass cancelled in its grace month: status %d", status)
			}
		})
	}
}

// TestPassLostRenewalAnswer holds what the page relies on when the answer to
// a renewal never arrives. A renewal hands over no code, so nothing is kept
// to answer it again from: asked again with the cookie and the nonce, the
// claim answers 400 as for a browser with nothing to collect, and a reissue
// code that collected a renewal answers 403 like any spent one. The renewal is
// on the Pass all the same, and checking the code held shows its new date,
// which is how the page tells the reader.
func TestPassLostRenewalAnswer(t *testing.T) {
	p := passServer(t, func(cfg *Config) { cfg.ModKey = "mod-key" })
	issued := p.buy("pi_lost_bought")
	before, _ := p.passExpiresAt(issued.Pass)

	p.paid(p.checkout(issued.Pass), "pi_lost_renewal")
	nonce := p.collectNonce()
	status, lost := p.claimWith(nonce)
	if status != http.StatusOK || lost.Status != "renewed" || !lost.ExpiresAt.After(before) {
		t.Fatalf("the renewal's claim: status %d, %+v", status, lost)
	}
	if status, again := p.claimWith(nonce); status != http.StatusBadRequest || again.Status != "" || again.Pass != "" || again.Claim {
		t.Fatalf("the renewal's claim asked again: status %d, %+v; want 400 and nothing to collect", status, again)
	}
	if status, out := p.activate(issued.Pass); status != http.StatusOK || !out.ExpiresAt.Equal(lost.ExpiresAt) {
		t.Fatalf("the held code after a lost renewal answer: status %d, %+v; want good until %v", status, out, lost.ExpiresAt)
	}

	// A renewal paid in one browser and collected in another with a reissue
	// code.
	p.paid(p.checkout(issued.Pass), "pi_lost_reissued_renewal")
	_, _, code := p.reissueCode("mod-key", "pi_lost_reissued_renewal")
	elsewhere := p.otherBrowser()
	reissueNonce := elsewhere.collectNonce()
	status, renewed := elsewhere.redeemWith(code, reissueNonce)
	if status != http.StatusOK || renewed.Status != "renewed" || !renewed.ExpiresAt.After(lost.ExpiresAt) {
		t.Fatalf("a reissue code for a renewal: status %d, %+v", status, renewed)
	}
	if status, again := elsewhere.redeemWith(code, reissueNonce); status != http.StatusForbidden || refusalState(t, again) != passInvalid ||
		again.Error != passReissueSpent {
		t.Fatalf("the reissue code asked again: status %d, %+v; want it refused as spent", status, again)
	}
	if status, out := p.activate(issued.Pass); status != http.StatusOK || !out.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatalf("the held code after a lost reissue answer: status %d, %+v; want good until %v", status, out, renewed.ExpiresAt)
	}
}

// TestPassReissueSurvivesAFailedClaimRead holds a reissue that has gone
// through to answering so, when the read of whether the browser also has a
// purchase to collect fails afterwards: the code is changed by then, and a 500
// would tell the reader it had not been. The answer says there is nothing to
// collect, the failure is logged without the code, and the page finds the
// purchase from GET /api/pass on its next visit.
func TestPassReissueSurvivesAFailedClaimRead(t *testing.T) {
	var logs syncBuffer
	p := passServerLogging(t, func(cfg *Config) { cfg.ModKey = "mod-key" }, &logs)
	issued := p.buy("pi_read_fails")
	_, _, code := p.reissueCode("mod-key", "pi_read_fails")
	// The browser that types the code has a purchase of its own waiting.
	p.checkout("")
	if !p.reportsClaim() {
		t.Fatal("the browser has no purchase waiting; the test proves nothing")
	}
	// Hide the one column the waiting read needs and the reissue does not.
	// A rename rather than a revoked privilege, which a superuser, as CI's
	// database user is, would not feel.
	p.exec(`ALTER TABLE pass_orders RENAME COLUMN claim_hash TO claim_hash_unreadable`)
	status, reissued := p.redeem(code)
	p.exec(`ALTER TABLE pass_orders RENAME COLUMN claim_hash_unreadable TO claim_hash`)
	if status != http.StatusOK || reissued.Status != "issued" || !passCodeShape(reissued.Pass) || reissued.Claim {
		t.Fatalf("a reissue whose claim read failed: status %d, %+v; want the new code and no claim", status, reissued)
	}
	if status, out := p.activate(issued.Pass); status != http.StatusForbidden || refusalState(t, out) != passInvalid {
		t.Fatalf("the replaced code: status %d, %+v", status, out)
	}
	if status, _ := p.activate(reissued.Pass); status != http.StatusOK {
		t.Fatalf("the reissued code: status %d", status)
	}
	if !p.reportsClaim() {
		t.Fatal("the purchase waiting in the browser was lost")
	}
	out := logs.String()
	if !strings.Contains(out, `"msg":"could not look up the purchase in progress"`) {
		t.Fatalf("the failed read is not in the log:\n%s", out)
	}
	canonical, _ := parsePassCode(reissued.Pass)
	for _, needle := range []string{reissued.Pass, canonical, code} {
		if strings.Contains(out, needle) {
			t.Errorf("the log contains %q:\n%s", needle, out)
		}
	}
}

// TestPassLogsAreBare holds the webhook to never logging its payload, on
// every event it takes and on a delivery it refuses, and every Pass log line
// to its message alone: no date, session, payment or code, nothing that would
// tie a line to a buyer, a Pass or a thread. It also holds the warning about a
// paid checkout that matched no order to the case it is about: it is not
// logged for a repeated delivery after the Pass was collected, or for a
// payment refunded before its paid event.
func TestPassLogsAreBare(t *testing.T) {
	var logs syncBuffer
	p := passServerLogging(t, func(cfg *Config) { cfg.ModKey = "mod-key" }, &logs)
	// What starting the server logged (its migrations) is not the Pass's.
	started := len(logs.String())
	buyer := map[string]any{"email": "buyer@example.com", "name": "Bea Buyer", "phone": "+44 7700 900123",
		"address": map[string]any{"line1": "1 Card Street", "postal_code": "ZZ1 1ZZ"}}
	session := func(id, payment, status string) map[string]any {
		return map[string]any{"id": id, "object": "checkout.session", "mode": "payment", "payment_status": status,
			"payment_intent": payment, "customer_details": buyer, "customer_email": "buyer@example.com"}
	}
	charge := func(payment string) map[string]any {
		return map[string]any{"object": "charge", "payment_intent": payment, "refunded": true,
			"billing_details": buyer, "receipt_email": "buyer@example.com"}
	}
	deliver := func(event string, object map[string]any) {
		t.Helper()
		if status := p.webhook(event, object); status != http.StatusOK {
			t.Fatalf("%s: status %d", event, status)
		}
	}

	// A slow payment held, then paid, collected, asked again with the same
	// nonce as after a lost answer, which logs no second issue, and delivered
	// again.
	slow := p.checkout("")
	deliver(stripeCheckoutCompleted, session(slow, "pi_logs_slow", "unpaid"))
	deliver(stripeCheckoutAsyncSuccess, session(slow, "pi_logs_slow", "paid"))
	nonce := p.collectNonce()
	_, issued := p.claimWith(nonce)
	if issued.Status != "issued" {
		t.Fatalf("collecting the slow payment: %+v", issued)
	}
	status, again := p.claimWith(nonce)
	if again.Status != "issued" || again.Pass != issued.Pass {
		t.Fatalf("collecting the slow payment asked again: %+v", again)
	}
	p.keepNonce(status, again)
	deliver(stripeCheckoutCompleted, session(slow, "pi_logs_slow", "paid"))
	// A slow payment that failed, and a checkout left until Stripe closed it.
	failed := p.checkout("")
	deliver(stripeCheckoutCompleted, session(failed, "pi_logs_failed", "unpaid"))
	deliver(stripeCheckoutAsyncFailure, session(failed, "pi_logs_failed", "unpaid"))
	deliver(stripeCheckoutExpired, session(p.checkout(""), "", "unpaid"))
	// A payment refunded before its paid event arrived.
	early := p.checkout("")
	deliver(stripeChargeRefunded, charge("pi_logs_early"))
	deliver(stripeCheckoutCompleted, session(early, "pi_logs_early", "paid"))
	// A paid checkout nothing here started.
	deliver(stripeCheckoutCompleted, session("cs_live_elsewhere", "pi_logs_elsewhere", "paid"))
	// A renewal, a reissue, an order refunded before it was collected, a
	// refunded renewal and a disputed purchase.
	p.renew(issued.Pass, "pi_logs_renewal")
	_, _, reissueCode := p.reissueCode("mod-key", "pi_logs_slow")
	elsewhere := p.otherBrowser()
	nonce = elsewhere.collectNonce()
	_, reissued := elsewhere.redeemWith(reissueCode, nonce)
	if reissued.Status != "issued" {
		t.Fatalf("redeeming the reissue code: %+v", reissued)
	}
	if _, again := elsewhere.redeemWith(reissueCode, nonce); again.Status != "issued" || again.Pass != reissued.Pass {
		t.Fatalf("redeeming the reissue code asked again: %+v", again)
	}
	p.paid(p.otherBrowser().checkout(""), "pi_logs_uncollected")
	deliver(stripeChargeRefunded, charge("pi_logs_uncollected"))
	deliver(stripeChargeRefunded, charge("pi_logs_renewal"))
	deliver(stripeDisputeCreated, map[string]any{"object": "dispute", "payment_intent": "pi_logs_slow",
		"evidence": map[string]any{"customer_email_address": "buyer@example.com", "customer_name": "Bea Buyer"}})
	// A delivery whose signature does not verify.
	forged, err := json.Marshal(map[string]any{"id": "evt_forged", "object": "event", "type": stripeCheckoutCompleted,
		"data": map[string]any{"object": session("cs_forged", "pi_logs_forged", "paid")}})
	if err != nil {
		t.Fatal(err)
	}
	if status := p.rawWebhook(forged, signStripe(forged, "whsec_somebody_else", time.Now())); status != http.StatusBadRequest {
		t.Fatalf("a forged delivery: status %d", status)
	}

	out := logs.String()[started:]
	messages := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %q", line)
		}
		msg, _ := record["msg"].(string)
		messages[msg]++
		want := []string{"level", "msg", "time"}
		if msg == "request" {
			want = []string{"duration_ms", "level", "method", "msg", "path", "status", "time"}
		}
		keys := make([]string, 0, len(record))
		for k := range record {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, want) {
			t.Errorf("a log record carries %v, want %v: %s", keys, want, line)
		}
	}
	for msg, want := range map[string]int{
		"pass issued":            1,
		"pass renewed":           1,
		"pass reissue code made": 1,
		"pass reissued":          1,
		"a paid Stripe checkout matched no open Pass order":                1,
		"pass order cancelled by a refund or dispute":                      1,
		"pass payment taken back by a refund or dispute; the Pass goes on": 1,
		"pass cancelled by a refund or dispute":                            1,
	} {
		if messages[msg] != want {
			t.Errorf("%q logged %d times, want %d", msg, messages[msg], want)
		}
	}

	canonical, _ := parsePassCode(issued.Pass)
	needles := []string{"buyer@example.com", "Bea Buyer", "7700 900123", "1 Card Street", "ZZ1 1ZZ",
		"cs_test_", "cs_live_elsewhere", "cs_forged", "evt_", issued.Pass, canonical, reissued.Pass, reissueCode}
	for _, payment := range []string{"slow", "failed", "early", "elsewhere", "renewal", "uncollected", "forged"} {
		needles = append(needles, "pi_logs_"+payment)
	}
	for _, needle := range needles {
		if strings.Contains(out, needle) {
			t.Errorf("the log contains %q:\n%s", needle, out)
		}
	}
}

// TestPassWarnsWithoutTheModerationKey holds a server that sells the Pass with
// no moderation key to saying, as it starts serving, that a lost code cannot
// be reissued: the reissue is behind that key, and an owner who never
// moderates by key would otherwise find out from a buyer.
func TestPassWarnsWithoutTheModerationKey(t *testing.T) {
	var logs syncBuffer
	p := passServerLogging(t, func(cfg *Config) { cfg.ModKey = "" }, &logs)
	p.app.WarnServingSettings()
	if out := logs.String(); !strings.Contains(out, "EDUCHAN_MOD_KEY is unset") || !strings.Contains(out, "no lost Pass code can be reissued") {
		t.Fatalf("serving the Pass without a moderation key says nothing about reissuing:\n%s", out)
	}
	if status, _, _ := p.reissueCode("", "pi_anything"); status != http.StatusForbidden {
		t.Fatalf("a reissue with no moderation key configured: status %d", status)
	}
}
