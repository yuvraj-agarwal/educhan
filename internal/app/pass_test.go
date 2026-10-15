package app

import (
	"strings"
	"testing"
	"time"
)

func TestPassCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		code, err := newPassCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != passCodeLen || strings.Trim(code, passAlphabet) != "" {
			t.Fatalf("code %q is not %d characters of the alphabet", code, passCodeLen)
		}
		if seen[code] {
			t.Fatalf("code %q drawn twice", code)
		}
		seen[code] = true
		formatted := formatPassCode(code)
		if !passCodeShape(formatted) {
			t.Fatalf("formatted %q", formatted)
		}
		if back, ok := parsePassCode(formatted); !ok || back != code {
			t.Fatalf("parse(format(%q)) = %q, %v", code, back, ok)
		}
	}

	for raw, want := range map[string]string{
		"ABCDE-FGHJK-MNPQR-STVWX":     "ABCDEFGHJKMNPQRSTVWX",
		"abcde fghjk mnpqr stvwx":     "ABCDEFGHJKMNPQRSTVWX",
		" 0o0o0-iIlL1-23456-789AB ":   "00000111112345678 9AB",
		"OOOOOIIIIILLLLL01234":        "00000111111111101234",
		"\tABCDE-FGHJK-MNPQR-STVWX\t": "ABCDEFGHJKMNPQRSTVWX",
	} {
		want = strings.ReplaceAll(want, " ", "")
		if got, ok := parsePassCode(raw); !ok || got != want {
			t.Errorf("parse(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{
		"", "ABCDE", "ABCDE-FGHJK-MNPQR-STVW", "ABCDE-FGHJK-MNPQR-STVWXY",
		"ABCDE-FGHJK-MNPQR-STVWU", // U is not in the alphabet
		"ABCDE-FGHJK-MNPQR-STVW!", "ABCDE-FGHJK-MNPQR-STVWé", "ABCDE_FGHJK_MNPQR_STVWX",
	} {
		if got, ok := parsePassCode(raw); ok {
			t.Errorf("parse(%q) accepted as %q", raw, got)
		}
	}
}

func TestVerifyStripeSignature(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"type":"checkout.session.completed"}`)
	good := signStripe(body, testWebhookSecret, now)
	if err := verifyStripeSignature(good, body, testWebhookSecret, now); err != nil {
		t.Fatalf("good signature: %v", err)
	}
	// While a secret is rolled Stripe signs with both; either one is enough.
	other := signStripe(body, "whsec_old", now)
	rolled := good + "," + other[strings.Index(other, "v1="):]
	if err := verifyStripeSignature(rolled, body, "whsec_old", now); err != nil {
		t.Fatalf("second v1: %v", err)
	}
	for name, c := range map[string]struct {
		header string
		body   []byte
		at     time.Time
	}{
		"empty header":      {"", body, now},
		"no v1":             {good[:strings.Index(good, ",")], body, now},
		"no timestamp":      {good[strings.Index(good, ",")+1:], body, now},
		"changed body":      {good, []byte(`{"type":"charge.refunded"}`), now},
		"old":               {good, body, now.Add(stripeSignatureTolerance + time.Second)},
		"early":             {good, body, now.Add(-stripeSignatureTolerance - time.Second)},
		"v0 only":           {strings.Replace(good, "v1=", "v0=", 1), body, now},
		"wrong secret":      {signStripe(body, "whsec_other", now), body, now},
		"garbage signature": {"t=1700000000,v1=zz", body, now},
	} {
		if err := verifyStripeSignature(c.header, c.body, testWebhookSecret, c.at); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestPassConfigFromEnv(t *testing.T) {
	full := map[string]string{
		"EDUCHAN_PASS_SECRET":           testPassSecret,
		"EDUCHAN_STRIPE_SECRET_KEY":     "sk_live_x",
		"EDUCHAN_STRIPE_WEBHOOK_SECRET": "whsec_x",
		"EDUCHAN_STRIPE_PRICE_ID":       "price_x",
		"EDUCHAN_PUBLIC_URL":            "https://educhan.example/",
	}
	set := func(t *testing.T, env map[string]string) {
		for k := range full {
			t.Setenv(k, "")
		}
		for _, k := range []string{"EDUCHAN_PASS_DAYS", "EDUCHAN_PASS_COOLDOWN", "EDUCHAN_STRIPE_AUTOMATIC_TAX"} {
			t.Setenv(k, "")
		}
		for k, v := range env {
			t.Setenv(k, v)
		}
	}

	set(t, nil)
	cfg, err := passConfigFromEnv()
	if err != nil || cfg.Enabled() {
		t.Fatalf("unset: %+v, %v", cfg, err)
	}

	set(t, full)
	cfg, err = passConfigFromEnv()
	if err != nil || !cfg.Enabled() || cfg.PublicURL != "https://educhan.example" || cfg.Days != 365 ||
		cfg.CheckoutCooldown != defaultPassCheckoutCooldown || cfg.StripeAutomaticTax {
		t.Fatalf("full: %+v, %v", cfg, err)
	}

	bad := map[string]map[string]string{
		"secret without stripe":   {"EDUCHAN_STRIPE_SECRET_KEY": "", "EDUCHAN_STRIPE_WEBHOOK_SECRET": "", "EDUCHAN_STRIPE_PRICE_ID": "", "EDUCHAN_PUBLIC_URL": ""},
		"stripe without secret":   {"EDUCHAN_PASS_SECRET": ""},
		"short secret":            {"EDUCHAN_PASS_SECRET": "short"},
		"webhook secret shape":    {"EDUCHAN_STRIPE_WEBHOOK_SECRET": "sk_x"},
		"price shape":             {"EDUCHAN_STRIPE_PRICE_ID": "prod_x"},
		"http public url":         {"EDUCHAN_PUBLIC_URL": "http://educhan.example"},
		"public url with a path":  {"EDUCHAN_PUBLIC_URL": "https://educhan.example/board"},
		"public url with a query": {"EDUCHAN_PUBLIC_URL": "https://educhan.example?x=1"},
		"public url with a user":  {"EDUCHAN_PUBLIC_URL": "https://me@educhan.example"},
		"not a url":               {"EDUCHAN_PUBLIC_URL": "educhan"},
		"zero days":               {"EDUCHAN_PASS_DAYS": "0"},
		"days not a number":       {"EDUCHAN_PASS_DAYS": "a year"},
		"tax not a boolean":       {"EDUCHAN_STRIPE_AUTOMATIC_TAX": "maybe"},
	}
	for name, change := range bad {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		for k, v := range change {
			env[k] = v
		}
		set(t, env)
		if _, err := passConfigFromEnv(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	env := map[string]string{"EDUCHAN_PUBLIC_URL": "http://localhost:8080", "EDUCHAN_PASS_DAYS": "30",
		"EDUCHAN_STRIPE_AUTOMATIC_TAX": "1", "EDUCHAN_PASS_COOLDOWN": "0"}
	for k, v := range full {
		if _, ok := env[k]; !ok {
			env[k] = v
		}
	}
	set(t, env)
	if cfg, err := passConfigFromEnv(); err != nil || cfg.Days != 30 || !cfg.StripeAutomaticTax || cfg.CheckoutCooldown != 0 {
		t.Fatalf("local: %+v, %v", cfg, err)
	}
}
