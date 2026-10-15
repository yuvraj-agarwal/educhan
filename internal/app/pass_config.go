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
// Stripe settings beside it is refused at startup rather than at the first
// checkout.
type PassConfig struct {
	// Secret keys every hash the Pass tables hold: of each Pass code, and of
	// the processor's session and payment ids. Rotating it cancels every Pass,
	// since no stored code hash matches any more, and orphans the payment
	// records a refund would look for.
	Secret string
	// StripeSecretKey is the restricted or secret API key checkout sessions are
	// created with.
	StripeSecretKey string
	// StripeWebhookSecret is the signing secret of the webhook endpoint
	// pointed at /api/pass/stripe-webhook ("whsec_…").
	StripeWebhookSecret string
	// StripePriceID is the one-time Price a Pass sells for ("price_…"). The
	// amount and currency live in Stripe, so changing the price needs no
	// deploy.
	StripePriceID string
	// StripeAutomaticTax asks Stripe Tax to work out and collect sales tax,
	// VAT or GST. It needs Stripe Tax switched on in the Stripe account first.
	StripeAutomaticTax bool
	// PublicURL is the site's own origin, which Stripe sends the buyer back to
	// after paying ("https://educhan.example"). It is configured rather than
	// read from the request's Host, so a forged Host header cannot point the
	// return trip somewhere else.
	PublicURL string
	// Days is how long a Pass lasts from the day it is bought.
	Days int
	// CheckoutCooldown is the minimum interval between two checkouts started
	// from one client address.
	CheckoutCooldown time.Duration

	// stripeAPI and stripeHTTP are for tests, which point the client at a fake
	// Stripe. Empty and nil mean the real API and a client with a timeout.
	stripeAPI  string
	stripeHTTP *http.Client
}

// Enabled reports whether Pass sales and Pass checks are switched on.
func (c PassConfig) Enabled() bool { return c.Secret != "" }

const (
	defaultPassDays             = 365
	defaultPassCheckoutCooldown = 30 * time.Second
)

// passConfigFromEnv reads the Pass settings. Validation is left to
// validatePassConfig, which New runs too, so a Config built in code is held to
// the same rules as one read from the environment.
func passConfigFromEnv() (PassConfig, error) {
	cfg := PassConfig{
		Secret:              strings.TrimSpace(os.Getenv("EDUCHAN_PASS_SECRET")),
		StripeSecretKey:     strings.TrimSpace(os.Getenv("EDUCHAN_STRIPE_SECRET_KEY")),
		StripeWebhookSecret: strings.TrimSpace(os.Getenv("EDUCHAN_STRIPE_WEBHOOK_SECRET")),
		StripePriceID:       strings.TrimSpace(os.Getenv("EDUCHAN_STRIPE_PRICE_ID")),
		PublicURL:           strings.TrimRight(strings.TrimSpace(os.Getenv("EDUCHAN_PUBLIC_URL")), "/"),
		Days:                defaultPassDays,
	}
	var err error
	if cfg.StripeAutomaticTax, err = envBool("EDUCHAN_STRIPE_AUTOMATIC_TAX"); err != nil {
		return PassConfig{}, err
	}
	if raw := strings.TrimSpace(os.Getenv("EDUCHAN_PASS_DAYS")); raw != "" {
		if cfg.Days, err = strconv.Atoi(raw); err != nil {
			return PassConfig{}, fmt.Errorf("EDUCHAN_PASS_DAYS: %w", err)
		}
	}
	if cfg.CheckoutCooldown, err = envDuration("EDUCHAN_PASS_COOLDOWN", defaultPassCheckoutCooldown); err != nil {
		return PassConfig{}, err
	}
	return cfg, validatePassConfig(cfg)
}

// validatePassConfig refuses a half-configured Pass. Every Stripe setting is
// needed for a sale to complete: without the webhook secret a buyer pays and
// never gets a Pass, and without the price there is nothing to buy. Setting
// some of them without the secret is just as much a mistake, so it is refused
// too rather than read as "off".
func validatePassConfig(c PassConfig) error {
	stripe := map[string]string{
		"EDUCHAN_STRIPE_SECRET_KEY":     c.StripeSecretKey,
		"EDUCHAN_STRIPE_WEBHOOK_SECRET": c.StripeWebhookSecret,
		"EDUCHAN_STRIPE_PRICE_ID":       c.StripePriceID,
		"EDUCHAN_PUBLIC_URL":            c.PublicURL,
	}
	var missing, set []string
	for _, name := range []string{
		"EDUCHAN_STRIPE_SECRET_KEY", "EDUCHAN_STRIPE_WEBHOOK_SECRET", "EDUCHAN_STRIPE_PRICE_ID", "EDUCHAN_PUBLIC_URL",
	} {
		if stripe[name] == "" {
			missing = append(missing, name)
		} else {
			set = append(set, name)
		}
	}
	if !c.Enabled() {
		if len(set) > 0 {
			return fmt.Errorf("%s set but EDUCHAN_PASS_SECRET is not; set it to sell the Pass, or unset them",
				strings.Join(set, ", "))
		}
		return nil
	}
	if len(missing) > 0 {
		return fmt.Errorf("EDUCHAN_PASS_SECRET is set but %s is not", strings.Join(missing, ", "))
	}
	if len(c.Secret) < 32 {
		return errors.New("EDUCHAN_PASS_SECRET must be at least 32 characters")
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
	if c.Days < 1 {
		return errors.New("EDUCHAN_PASS_DAYS must be at least 1")
	}
	if c.CheckoutCooldown < 0 {
		return errors.New("EDUCHAN_PASS_COOLDOWN must not be negative")
	}
	return nil
}

// checkPublicURL wants a bare origin: Stripe appends nothing to it, and the
// return paths are added here. Plain http is accepted only for a local
// checkout, where Stripe's test mode allows it.
func checkPublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("EDUCHAN_PUBLIC_URL: want the site's origin, like https://educhan.example, got %q", raw)
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
