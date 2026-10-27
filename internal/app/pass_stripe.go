package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Stripe, spoken to over plain HTTPS with no SDK: one call to start a hosted
// checkout, one to read the Pass's price, and a signature check on the
// webhooks Stripe sends back.
//
// The buyer pays on Stripe's own page, checkout.stripe.com, reached by an
// ordinary top-level navigation. No Stripe script, frame or stylesheet ever
// loads on educhan's origin, so nothing of Stripe's can read the poster key or
// the credential out of localStorage, and the Content-Security-Policy needs no
// exception for it. Google has no checkout of its own for selling on the web
// (Google Pay sits on top of a processor such as Stripe, whose page offers it),
// so this is one place the project uses a non-Google service.

const (
	stripeAPIBase = "https://api.stripe.com"
	// stripeAPIVersion pins the shape of what the API returns. Webhook payloads
	// follow the version the endpoint was created with instead, so the
	// README's "Setting it up" says to create it with this one. Only fields
	// that have been stable across versions are read either way.
	stripeAPIVersion = "2024-06-20"
	// stripeSignatureTolerance is how old a webhook's signed timestamp may be,
	// Stripe's own recommended default. It bounds how long a captured delivery
	// could be replayed.
	stripeSignatureTolerance = 5 * time.Minute
	// maxWebhookBytes bounds a webhook body. Stripe's events are a few
	// kilobytes; this leaves generous room without reading an unbounded body.
	maxWebhookBytes = 256 << 10
)

// stripeClient creates checkout sessions and reads prices.
type stripeClient struct {
	base    string
	key     string
	priceID string
	tax     bool
	http    *http.Client
}

func newStripeClient(cfg PassConfig) *stripeClient {
	c := &stripeClient{
		base:    cfg.stripeAPI,
		key:     cfg.StripeSecretKey,
		priceID: cfg.StripePriceID,
		tax:     cfg.StripeAutomaticTax,
		http:    cfg.stripeHTTP,
	}
	if c.base == "" {
		c.base = stripeAPIBase
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 15 * time.Second}
	}
	return c
}

// checkoutSession is the part of a created session educhan reads: its id, to
// match the webhook that reports it paid, and the page to send the buyer to.
type checkoutSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// createCheckout starts a hosted checkout for one Pass. The buyer comes back
// to successURL or cancelURL; neither carries anything about the purchase,
// because the browser holds its claim in a cookie and the session id would add
// nothing but a value to leak through history or a Referer.
//
// No customer object is created (Stripe's default for one-off payments), and
// no metadata is attached: Stripe learns that somebody bought a Pass, which it
// has to, and nothing about who they are on educhan. With Stripe Tax on,
// Stripe's page also asks for the buyer's billing address to work the tax
// out, and keeps it with the rest of the payment.
func (c *stripeClient) createCheckout(ctx context.Context, successURL, cancelURL string) (checkoutSession, error) {
	form := url.Values{
		"mode":                    {"payment"},
		"line_items[0][price]":    {c.priceID},
		"line_items[0][quantity]": {"1"},
		"success_url":             {successURL},
		"cancel_url":              {cancelURL},
		"submit_type":             {"pay"},
		// Sent either way, so an account set to collect tax by default never
		// adds tax that the Pass page did not show.
		"automatic_tax[enabled]": {strconv.FormatBool(c.tax)},
	}
	var s checkoutSession
	if err := c.call(ctx, http.MethodPost, "/v1/checkout/sessions", form, &s); err != nil {
		return checkoutSession{}, err
	}
	if s.ID == "" || !isStripeCheckoutURL(s.URL) {
		return checkoutSession{}, errors.New("stripe: checkout session without an id or a checkout.stripe.com url")
	}
	return s, nil
}

// stripeCheckoutHost is where Stripe hosts its checkout pages, and the one
// place the Pass page will send a buyer.
const stripeCheckoutHost = "checkout.stripe.com"

// isStripeCheckoutURL reports whether raw is a page on Stripe's checkout
// host, so a session URL that came back as anything else, by a fault or a
// compromised key, never becomes a redirect to another site. Stripe's custom
// checkout domains are not accepted.
func isStripeCheckoutURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == stripeCheckoutHost && u.User == nil
}

// passPrice is what the Pass page shows before anyone clicks: the amount in
// the currency's smallest unit, as Stripe keeps it, and the currency code.
// TaxBehavior is the Price's own word on tax, which GET /api/pass passes on
// as passTaxWords has it.
type passPrice struct {
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	TaxBehavior string `json:"-"`
}

// passTaxWords is what GET /api/pass says of tax while Stripe Tax is on, by
// the Price's tax_behavior: "included" when the amount has the tax in it
// already, and "added" when Stripe adds it at checkout, which can come to
// nothing for some buyers. With Stripe Tax off no tax is collected, and
// nothing is said of it.
var passTaxWords = map[string]string{"inclusive": "included", "exclusive": "added"}

// errPassPriceTax is price's refusal, while Stripe Tax is on, of a Price that
// does not say whether its amount includes tax. Stripe would settle that by a
// default in the account that the Pass page cannot know, so the page could
// show a price the buyer does not pay. passPrice logs it each time it reads
// the Price, at most once a minute while it lasts, and nothing else does, so
// it says what to fix.
var errPassPriceTax = errors.New("stripe: with EDUCHAN_STRIPE_AUTOMATIC_TAX on, the Pass price must have a tax behavior, inclusive or exclusive: set one on the Price, or make a new Price with one and set EDUCHAN_STRIPE_PRICE_ID to it")

// price reads the Pass's Price. A recurring Price is refused, because the
// Pass is bought by hand for each term, as 4chan's is, and never renews
// itself. So, while Stripe Tax is on, is a Price that does not say whether tax
// is in its amount (errPassPriceTax).
func (c *stripeClient) price(ctx context.Context) (passPrice, error) {
	var p struct {
		Active      bool   `json:"active"`
		Type        string `json:"type"`
		UnitAmount  *int64 `json:"unit_amount"`
		Currency    string `json:"currency"`
		TaxBehavior string `json:"tax_behavior"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/prices/"+url.PathEscape(c.priceID), nil, &p); err != nil {
		return passPrice{}, err
	}
	switch {
	case !p.Active:
		return passPrice{}, errors.New("stripe: the Pass price is archived")
	case p.Type != "one_time":
		return passPrice{}, fmt.Errorf("stripe: the Pass price must be one-time, not %q", p.Type)
	case p.UnitAmount == nil || *p.UnitAmount <= 0 || p.Currency == "":
		return passPrice{}, errors.New("stripe: the Pass price has no fixed amount")
	case c.tax && passTaxWords[p.TaxBehavior] == "":
		return passPrice{}, errPassPriceTax
	}
	return passPrice{Amount: *p.UnitAmount, Currency: strings.ToLower(p.Currency), TaxBehavior: p.TaxBehavior}, nil
}

// call makes one API request. Errors carry Stripe's message and type but
// never the request, which holds the secret key in its header.
func (c *stripeClient) call(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("stripe: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Stripe-Version", stripeAPIVersion)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		// A retry by the HTTP client after a lost response must not open a
		// second session; a fresh key per call is enough for that.
		key, err := randomToken()
		if err != nil {
			return err
		}
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("stripe %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe %s %s: %w", method, path, err)
	}
	if res.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return fmt.Errorf("stripe %s %s: status %d: %s: %s", method, path, res.StatusCode, e.Error.Type, e.Error.Message)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("stripe %s %s: decode: %w", method, path, err)
	}
	return nil
}

/* Webhooks ------------------------------------------------------------------ */

var errStripeSignature = errors.New("stripe webhook signature does not verify")

// verifyStripeSignature checks a webhook's Stripe-Signature header: a
// timestamp t and one or more v1 signatures, each HMAC-SHA256 over
// "<t>.<body>" with the endpoint's signing secret. Any one v1 matching is
// enough, because Stripe signs with both secrets while one is being rolled.
// The timestamp must be recent, so a captured delivery cannot be replayed
// later.
func verifyStripeSignature(header string, body []byte, secret string, now time.Time) error {
	var ts string
	var sigs [][]byte
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			if sig, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, sig)
			}
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return errStripeSignature
	}
	if age := now.Sub(time.Unix(unix, 0)); age > stripeSignatureTolerance || age < -stripeSignatureTolerance {
		return errStripeSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, sig := range sigs {
		if hmac.Equal(sig, want) {
			return nil
		}
	}
	return errStripeSignature
}

// stripeEvent is the envelope of a webhook. Only the type and the few fields
// below are ever read out of the object; the rest, which includes the payer's
// email, name and card details, is decoded into nothing and never logged.
type stripeEvent struct {
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// stripeSessionObject is a checkout session as a webhook carries it.
type stripeSessionObject struct {
	ID            string  `json:"id"`
	Mode          string  `json:"mode"`
	PaymentStatus string  `json:"payment_status"`
	PaymentIntent *string `json:"payment_intent"`
}

// stripePaymentObject is a charge or a dispute: both name the payment intent
// they belong to, which is the one id educhan keys a Pass's payments on.
type stripePaymentObject struct {
	PaymentIntent *string `json:"payment_intent"`
	// Refunded is a charge's: true once the whole amount has been refunded.
	Refunded bool `json:"refunded"`
}

// The events the webhook endpoint subscribes to. Anything else is
// acknowledged and ignored, so subscribing to more does no harm.
const (
	stripeCheckoutCompleted    = "checkout.session.completed"
	stripeCheckoutAsyncSuccess = "checkout.session.async_payment_succeeded"
	stripeCheckoutAsyncFailure = "checkout.session.async_payment_failed"
	stripeCheckoutExpired      = "checkout.session.expired"
	stripeChargeRefunded       = "charge.refunded"
	stripeDisputeCreated       = "charge.dispute.created"
)

func readWebhookBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	return buf.Bytes(), err
}
