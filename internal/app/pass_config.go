package app

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// PassConfig configures the educhan Pass. Sales are off unless Secret is set,
// the way verification is off without its secret, and a Secret without the
// Stripe settings and PublicURL beside it is refused at startup rather than at
// the first checkout.
type PassConfig struct {
	// Secret keys the hashes the Pass tables hold of each Pass code and of the
	// processor's session and payment ids, and signs the name a checkout's
	// answer gives its order (passOrderName). A claim token and a reissue code
	// are random, and stored as plain SHA-256 (hashToken). Rotating it cancels
	// every Pass, since no stored code hash matches any more, and orphans the
	// payment records a refund or a reissue would look for.
	Secret string
	// StripeSecretKey is the restricted or secret API key that creates
	// checkout sessions and reads the Price, for the Pass page and before a
	// checkout. A restricted key needs Checkout Sessions write and Prices read.
	StripeSecretKey string
	// StripeWebhookSecret is the signing secret of the webhook endpoint
	// pointed at /api/pass/stripe-webhook ("whsec_…").
	StripeWebhookSecret string
	// StripePriceID is the one-time Price a Pass sells for ("price_…"). Its
	// amount and currency are read from Stripe, which never changes them on a
	// Price, so a new price is a new Price, set here, and a restart.
	StripePriceID string
	// StripeAutomaticTax asks Stripe Tax to work out and collect sales tax,
	// VAT or GST. It needs Stripe Tax switched on in the Stripe account first,
	// and a Price that says whether its amount includes the tax (inclusive)
	// or has it added (exclusive); one that says neither is not sold. Off,
	// checkout tells Stripe to collect no tax.
	StripeAutomaticTax bool
	// PublicURL is the site's own origin, which Stripe sends the buyer back to
	// after paying ("https://educhan.example"). It is configured rather than
	// read from the request's Host, so a forged Host header cannot point the
	// return trip somewhere else.
	PublicURL string
	// Days is the term each order is sold with, stored on the order at
	// checkout (pass_orders.days), so changing this changes what is sold from
	// then on and nothing already paid for. A new Pass's term counts from when
	// it is collected, and a renewal adds its term to the Pass's expiry, or to
	// now for a Pass that has run out.
	Days int
	// HourlyPosts is a Pass's hourly budget: how many posts in one hour,
	// counted in UTC, it lets skip the captcha. Past that, posts with it
	// solve the captcha like any other until the hour ends; none is refused
	// for it.
	HourlyPosts int
	// CheckoutCooldown is the minimum interval between two checkouts started
	// from one client address.
	CheckoutCooldown time.Duration
	// SalesPaused stops selling while honouring what was sold
	// (EDUCHAN_PASS_SALES=paused): checkout refuses purchases and renewals,
	// and everything else goes on, webhooks, collecting, reissuing, codes
	// and posting with a Pass included. Unsetting the secret instead retires
	// the Pass: Passes stop working, and webhooks sent meanwhile are lost.
	SalesPaused bool

	// stripeAPI and stripeHTTP are for tests, which point the client at a fake
	// Stripe. Empty and nil mean the real API and a client with a timeout.
	stripeAPI  string
	stripeHTTP *http.Client
}

// Enabled reports whether Pass sales and Pass checks are switched on.
func (c PassConfig) Enabled() bool { return c.Secret != "" }

const (
	defaultPassDays             = 365
	defaultPassHourlyPosts      = 30
	defaultPassCheckoutCooldown = 30 * time.Second
	// maxPassDays bounds the term at a hundred years, so a Pass's expiry, and
	// an uncollected order's deadline plus its term, stay far inside the dates
	// PostgreSQL and JSON can hold.
	maxPassDays = 36500
)

// The values EDUCHAN_PASS_SALES takes.
const (
	passSalesOpen   = "open"
	passSalesPaused = "paused"
)

// passSettings names every Pass setting, the secret first. Without the secret
// none of the others may be set: a deployment that has some of them meant to
// sell the Pass and would otherwise start with it quietly off.
var passSettings = []string{
	"EDUCHAN_PASS_SECRET",
	"EDUCHAN_STRIPE_SECRET_KEY", "EDUCHAN_STRIPE_WEBHOOK_SECRET", "EDUCHAN_STRIPE_PRICE_ID",
	"EDUCHAN_STRIPE_AUTOMATIC_TAX", "EDUCHAN_PUBLIC_URL",
	"EDUCHAN_PASS_DAYS", "EDUCHAN_PASS_HOURLY_POSTS", "EDUCHAN_PASS_COOLDOWN", "EDUCHAN_PASS_SALES",
}

// passConfigFromEnv reads the Pass settings. Validation is left to
// validatePassConfig, which New runs too, so a Config built in code is held to
// the same rules as one read from the environment; the one rule only the
// environment can break, a setting given without the secret, is checked here
// first, before the defaults hide which ones were given.
func passConfigFromEnv() (PassConfig, error) {
	cfg := PassConfig{Secret: strings.TrimSpace(os.Getenv("EDUCHAN_PASS_SECRET"))}
	if !cfg.Enabled() {
		var set []string
		for _, name := range passSettings[1:] {
			if strings.TrimSpace(os.Getenv(name)) != "" {
				set = append(set, name)
			}
		}
		if len(set) > 0 {
			return PassConfig{}, fmt.Errorf("%s set but EDUCHAN_PASS_SECRET is not; set it to sell the Pass, or unset them",
				strings.Join(set, ", "))
		}
		return cfg, nil
	}
	cfg.StripeSecretKey = strings.TrimSpace(os.Getenv("EDUCHAN_STRIPE_SECRET_KEY"))
	cfg.StripeWebhookSecret = strings.TrimSpace(os.Getenv("EDUCHAN_STRIPE_WEBHOOK_SECRET"))
	cfg.StripePriceID = strings.TrimSpace(os.Getenv("EDUCHAN_STRIPE_PRICE_ID"))
	cfg.PublicURL = strings.TrimRight(strings.TrimSpace(os.Getenv("EDUCHAN_PUBLIC_URL")), "/")
	cfg.Days = defaultPassDays
	cfg.HourlyPosts = defaultPassHourlyPosts
	var err error
	if cfg.StripeAutomaticTax, err = envBool("EDUCHAN_STRIPE_AUTOMATIC_TAX"); err != nil {
		return PassConfig{}, err
	}
	if raw := strings.TrimSpace(os.Getenv("EDUCHAN_PASS_DAYS")); raw != "" {
		if cfg.Days, err = strconv.Atoi(raw); err != nil {
			return PassConfig{}, fmt.Errorf("EDUCHAN_PASS_DAYS: %w", err)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("EDUCHAN_PASS_HOURLY_POSTS")); raw != "" {
		if cfg.HourlyPosts, err = strconv.Atoi(raw); err != nil {
			return PassConfig{}, fmt.Errorf("EDUCHAN_PASS_HOURLY_POSTS: %w", err)
		}
	}
	if cfg.CheckoutCooldown, err = envDuration("EDUCHAN_PASS_COOLDOWN", defaultPassCheckoutCooldown); err != nil {
		return PassConfig{}, err
	}
	switch sales := strings.ToLower(strings.TrimSpace(os.Getenv("EDUCHAN_PASS_SALES"))); sales {
	case "", passSalesOpen:
	case passSalesPaused:
		cfg.SalesPaused = true
	default:
		return PassConfig{}, fmt.Errorf("EDUCHAN_PASS_SALES: want %q or %q, got %q", passSalesOpen, passSalesPaused, sales)
	}
	return cfg, validatePassConfig(cfg)
}

// validatePassConfig refuses a half-configured Pass. Every Stripe setting is
// needed for a sale to complete: without the webhook secret a buyer pays and
// never gets a Pass, and without the price there is nothing to buy. Setting
// any Pass setting without the secret is just as much a mistake, so it is
// refused too rather than read as "off".
func validatePassConfig(c PassConfig) error {
	stripe := map[string]string{
		"EDUCHAN_STRIPE_SECRET_KEY":     c.StripeSecretKey,
		"EDUCHAN_STRIPE_WEBHOOK_SECRET": c.StripeWebhookSecret,
		"EDUCHAN_STRIPE_PRICE_ID":       c.StripePriceID,
		"EDUCHAN_PUBLIC_URL":            c.PublicURL,
	}
	var missing []string
	for _, name := range []string{
		"EDUCHAN_STRIPE_SECRET_KEY", "EDUCHAN_STRIPE_WEBHOOK_SECRET", "EDUCHAN_STRIPE_PRICE_ID", "EDUCHAN_PUBLIC_URL",
	} {
		if stripe[name] == "" {
			missing = append(missing, name)
		}
	}
	if !c.Enabled() {
		// What a Config built in code has set; from the environment, every
		// field is empty here (passConfigFromEnv).
		set := map[string]bool{
			"EDUCHAN_STRIPE_SECRET_KEY":     c.StripeSecretKey != "",
			"EDUCHAN_STRIPE_WEBHOOK_SECRET": c.StripeWebhookSecret != "",
			"EDUCHAN_STRIPE_PRICE_ID":       c.StripePriceID != "",
			"EDUCHAN_STRIPE_AUTOMATIC_TAX":  c.StripeAutomaticTax,
			"EDUCHAN_PUBLIC_URL":            c.PublicURL != "",
			"EDUCHAN_PASS_DAYS":             c.Days != 0,
			"EDUCHAN_PASS_HOURLY_POSTS":     c.HourlyPosts != 0,
			"EDUCHAN_PASS_COOLDOWN":         c.CheckoutCooldown != 0,
			"EDUCHAN_PASS_SALES":            c.SalesPaused,
		}
		var names []string
		for _, name := range passSettings[1:] {
			if set[name] {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			return fmt.Errorf("%s set but EDUCHAN_PASS_SECRET is not; set it to sell the Pass, or unset them",
				strings.Join(names, ", "))
		}
		return nil
	}
	if len(missing) > 0 {
		return fmt.Errorf("EDUCHAN_PASS_SECRET is set but %s is not", strings.Join(missing, ", "))
	}
	// Counted in bytes, as the README says, since an HMAC's key is bytes.
	if len(c.Secret) < 32 {
		return errors.New("EDUCHAN_PASS_SECRET must be at least 32 bytes")
	}
	if !strings.HasPrefix(c.StripeSecretKey, "sk_") && !strings.HasPrefix(c.StripeSecretKey, "rk_") {
		return errors.New(`EDUCHAN_STRIPE_SECRET_KEY: want a secret or restricted API key, "sk_…" or "rk_…"`)
	}
	if !strings.HasPrefix(c.StripeWebhookSecret, "whsec_") {
		return errors.New(`EDUCHAN_STRIPE_WEBHOOK_SECRET: want the endpoint's signing secret, "whsec_…"`)
	}
	if !strings.HasPrefix(c.StripePriceID, "price_") {
		return errors.New(`EDUCHAN_STRIPE_PRICE_ID: want a Price id, "price_…"`)
	}
	if err := checkPublicURL(c.PublicURL); err != nil {
		return err
	}
	if c.Days < 1 || c.Days > maxPassDays {
		return fmt.Errorf("EDUCHAN_PASS_DAYS must be from 1 to %d", maxPassDays)
	}
	if c.HourlyPosts < 1 {
		return errors.New("EDUCHAN_PASS_HOURLY_POSTS must be at least 1")
	}
	if c.CheckoutCooldown < 0 {
		return errors.New("EDUCHAN_PASS_COOLDOWN must not be negative")
	}
	return nil
}

// checkPublicURL wants a bare origin: Stripe appends nothing to it, and the
// return paths are added here, so a trailing slash would make them "//pass".
// The environment's value has its slash trimmed already; one set in code is
// refused. Plain http is accepted only for a local checkout, where Stripe's
// test mode allows it.
func checkPublicURL(raw string) error {
	u, err := url.Parse(raw)
	// An empty fragment parses to nothing, so the raw value is checked too.
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("EDUCHAN_PUBLIC_URL: want the site's origin with no path or trailing slash, like https://educhan.example, got %q", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if host := u.Hostname(); host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
	}
	return fmt.Errorf("EDUCHAN_PUBLIC_URL: must be https (plain http only for localhost), got %q", raw)
}
