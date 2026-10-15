package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
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
		for _, k := range passSettings {
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
		cfg.HourlyPosts != 30 || cfg.CheckoutCooldown != defaultPassCheckoutCooldown || cfg.StripeAutomaticTax {
		t.Fatalf("full: %+v, %v", cfg, err)
	}

	bad := map[string]map[string]string{
		"secret without stripe":          {"EDUCHAN_STRIPE_SECRET_KEY": "", "EDUCHAN_STRIPE_WEBHOOK_SECRET": "", "EDUCHAN_STRIPE_PRICE_ID": "", "EDUCHAN_PUBLIC_URL": ""},
		"stripe without secret":          {"EDUCHAN_PASS_SECRET": ""},
		"short secret":                   {"EDUCHAN_PASS_SECRET": "short"},
		"webhook secret shape":           {"EDUCHAN_STRIPE_WEBHOOK_SECRET": "sk_x"},
		"publishable key":                {"EDUCHAN_STRIPE_SECRET_KEY": "pk_live_x"},
		"key shape":                      {"EDUCHAN_STRIPE_SECRET_KEY": "whsec_x"},
		"public url with an empty query": {"EDUCHAN_PUBLIC_URL": "https://educhan.example?"},
		"price shape":                    {"EDUCHAN_STRIPE_PRICE_ID": "prod_x"},
		"http public url":                {"EDUCHAN_PUBLIC_URL": "http://educhan.example"},
		"public url with a path":         {"EDUCHAN_PUBLIC_URL": "https://educhan.example/board"},
		"public url with a query":        {"EDUCHAN_PUBLIC_URL": "https://educhan.example?x=1"},
		"public url with a user":         {"EDUCHAN_PUBLIC_URL": "https://me@educhan.example"},
		"not a url":                      {"EDUCHAN_PUBLIC_URL": "educhan"},
		"zero days":                      {"EDUCHAN_PASS_DAYS": "0"},
		"days past a hundred years":      {"EDUCHAN_PASS_DAYS": "36501"},
		"days past the year 9999":        {"EDUCHAN_PASS_DAYS": "3000000"},
		"sales neither open nor paused":  {"EDUCHAN_PASS_SALES": "closed"},
		"sales off":                      {"EDUCHAN_PASS_SALES": "off"},
		"days not a number":              {"EDUCHAN_PASS_DAYS": "a year"},
		"zero hourly posts":              {"EDUCHAN_PASS_HOURLY_POSTS": "0"},
		"hourly posts negative":          {"EDUCHAN_PASS_HOURLY_POSTS": "-5"},
		"hourly posts not a number":      {"EDUCHAN_PASS_HOURLY_POSTS": "many"},
		"tax not a boolean":              {"EDUCHAN_STRIPE_AUTOMATIC_TAX": "maybe"},
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
		"EDUCHAN_PASS_HOURLY_POSTS": "5", "EDUCHAN_STRIPE_AUTOMATIC_TAX": "1", "EDUCHAN_PASS_COOLDOWN": "0"}
	for k, v := range full {
		if _, ok := env[k]; !ok {
			env[k] = v
		}
	}
	set(t, env)
	if cfg, err := passConfigFromEnv(); err != nil || cfg.Days != 30 || cfg.HourlyPosts != 5 || !cfg.StripeAutomaticTax || cfg.CheckoutCooldown != 0 {
		t.Fatalf("local: %+v, %v", cfg, err)
	}

	// A restricted key is as good as a secret one.
	env["EDUCHAN_STRIPE_SECRET_KEY"] = "rk_live_x"
	set(t, env)
	if _, err := passConfigFromEnv(); err != nil {
		t.Fatalf("restricted key: %v", err)
	}

	// The term may be up to a hundred years, and sales open or paused, in
	// any case.
	for value, paused := range map[string]bool{"": false, "open": false, "OPEN": false, "paused": true, " Paused ": true} {
		env["EDUCHAN_PASS_SALES"] = value
		env["EDUCHAN_PASS_DAYS"] = "36500"
		set(t, env)
		if cfg, err := passConfigFromEnv(); err != nil || cfg.SalesPaused != paused || cfg.Days != 36500 {
			t.Errorf("EDUCHAN_PASS_SALES=%q: %+v, %v", value, cfg, err)
		}
	}

	// Without the secret, every other Pass setting is refused, each naming
	// itself: a deployment that has any of them meant to sell the Pass.
	for name, value := range map[string]string{
		"EDUCHAN_STRIPE_SECRET_KEY":     "sk_live_x",
		"EDUCHAN_STRIPE_WEBHOOK_SECRET": "whsec_x",
		"EDUCHAN_STRIPE_PRICE_ID":       "price_x",
		"EDUCHAN_PUBLIC_URL":            "https://educhan.example",
		"EDUCHAN_STRIPE_AUTOMATIC_TAX":  "true",
		"EDUCHAN_PASS_DAYS":             "30",
		"EDUCHAN_PASS_HOURLY_POSTS":     "0",
		"EDUCHAN_PASS_COOLDOWN":         "5s",
		"EDUCHAN_PASS_SALES":            "paused",
	} {
		set(t, map[string]string{name: value})
		if _, err := passConfigFromEnv(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s without the secret: %v, want a refusal naming it", name, err)
		}
	}
}

// TestPassConfigInCodeIsValidated holds a Config built in code, which New
// validates, to the environment's rules: a public URL with a trailing slash,
// which the environment's reading trims, is refused rather than turned into
// "https://educhan.example//pass".
func TestPassConfigInCodeIsValidated(t *testing.T) {
	good := PassConfig{
		Secret:              testPassSecret,
		StripeSecretKey:     "sk_test_key",
		StripeWebhookSecret: testWebhookSecret,
		StripePriceID:       testPriceID,
		PublicURL:           testPublicURL,
		Days:                365,
		HourlyPosts:         30,
	}
	if err := validatePassConfig(good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	for name, c := range map[string]struct {
		change func(*PassConfig)
		names  string
	}{
		"trailing slash":      {func(c *PassConfig) { c.PublicURL = testPublicURL + "/" }, "EDUCHAN_PUBLIC_URL"},
		"publishable key":     {func(c *PassConfig) { c.StripeSecretKey = "pk_test_key" }, "EDUCHAN_STRIPE_SECRET_KEY"},
		"a century and a day": {func(c *PassConfig) { c.Days = maxPassDays + 1 }, "EDUCHAN_PASS_DAYS"},
		"paused without the secret": {func(c *PassConfig) { *c = PassConfig{SalesPaused: true} },
			"EDUCHAN_PASS_SALES"},
		"a term without the secret": {func(c *PassConfig) { *c = PassConfig{Days: 30, StripeAutomaticTax: true} },
			"EDUCHAN_STRIPE_AUTOMATIC_TAX, EDUCHAN_PASS_DAYS"},
	} {
		cfg := good
		c.change(&cfg)
		// New refuses it before it connects to anything.
		_, err := New(context.Background(), Config{Pass: cfg}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil || !strings.Contains(err.Error(), c.names) {
			t.Errorf("%s: New answered %v, want a refusal naming %s", name, err, c.names)
		}
	}
}

func TestPassBudget(t *testing.T) {
	b := newPassBudget(3)
	hour := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)

	// Counted within one clock hour, wherever in it the posts fall.
	for i, at := range []time.Duration{0, 20 * time.Minute, 59*time.Minute + 59*time.Second} {
		if !b.left("a", hour.Add(at)) {
			t.Fatalf("post %d: no budget left after %d posts", i+1, i)
		}
		b.spend("a", hour.Add(at))
	}
	if b.left("a", hour.Add(59*time.Minute)) {
		t.Fatal("budget left after three posts of three")
	}
	// Spending past the budget is counted and refuses nothing.
	b.spend("a", hour.Add(59*time.Minute))
	if b.hours["a"].count != 4 {
		t.Fatalf("count %d, want 4", b.hours["a"].count)
	}
	// Each Pass has its own.
	if !b.left("b", hour) {
		t.Fatal("another Pass shares the first one's budget")
	}

	// The next hour starts afresh, and looking at a past hour's entry drops it.
	next := hour.Add(time.Hour)
	if !b.left("a", next) {
		t.Fatal("no budget left in the next hour")
	}
	if _, kept := b.hours["a"]; kept {
		t.Fatal("a past hour's count was kept after it was looked at")
	}
	b.spend("a", next)
	if e := b.hours["a"]; e.hour != clockHour(next) || e.count != 1 {
		t.Fatalf("next hour's entry %+v, want one post in hour %d", e, clockHour(next))
	}

	// The sweep forgets every hour but the current one.
	b.spend("c", hour)
	b.mu.Lock()
	b.sweep(clockHour(next))
	b.mu.Unlock()
	if _, kept := b.hours["c"]; kept {
		t.Fatal("the sweep kept a past hour's count")
	}
	if _, kept := b.hours["a"]; !kept {
		t.Fatal("the sweep dropped the current hour's count")
	}
	// A nil budget, as a sweep might hold, sweeps nothing and does not panic.
	var none *passBudget
	none.sweepExpired()

	// What is kept about a Pass is an hour and a count, and nothing that says
	// when within the hour it posted.
	var got []string
	for _, f := range reflect.VisibleFields(reflect.TypeOf(passHour{})) {
		got = append(got, f.Name+" "+f.Type.String())
	}
	if strings.Join(got, ", ") != "hour int64, count int" {
		t.Errorf("passHour holds %v, want only the hour and the count", got)
	}
}

// TestPassBudgetIsSafeForConcurrentPosts spends one budget from many
// goroutines at once; run with -race, it also checks the locking.
func TestPassBudgetIsSafeForConcurrentPosts(t *testing.T) {
	b := newPassBudget(1000)
	now := time.Now()
	const posters, posts = 16, 50
	var wg sync.WaitGroup
	for i := range posters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range posts {
				b.left("shared", now)
				b.spend("shared", now)
				b.spend(fmt.Sprintf("own-%d", i), now)
				b.sweepExpired()
			}
		}(i)
	}
	wg.Wait()
	if n := b.hours["shared"].count; n != posters*posts {
		t.Fatalf("shared count %d, want %d", n, posters*posts)
	}
	for i := range posters {
		if n := b.hours[fmt.Sprintf("own-%d", i)].count; n != posts {
			t.Fatalf("own-%d count %d, want %d", i, n, posts)
		}
	}
}

func TestPassExpiryArithmetic(t *testing.T) {
	midnight := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	afternoon := midnight.Add(15 * time.Hour)

	if got := dayCeil(midnight); !got.Equal(midnight) {
		t.Errorf("dayCeil(midnight) = %v, want it unchanged", got)
	}
	if got := dayCeil(afternoon); !got.Equal(midnight.AddDate(0, 0, 1)) {
		t.Errorf("dayCeil(afternoon) = %v, want the next midnight", got)
	}
	if got := dayCeil(midnight.Add(time.Nanosecond)); !got.Equal(midnight.AddDate(0, 0, 1)) {
		t.Errorf("dayCeil(just past midnight) = %v, want the next midnight", got)
	}

	// Renewing a Pass that has not run out adds exactly the term.
	if got := passExpiry(midnight, 365); !got.Equal(midnight.AddDate(0, 0, 365)) {
		t.Errorf("a renewal from %v gives %v, want exactly 365 days on", midnight, got)
	}
	// Buying one rounds up to the midnight after the term.
	if got := passExpiry(afternoon, 365); !got.Equal(midnight.AddDate(0, 0, 366)) {
		t.Errorf("a purchase at %v runs out %v, want the midnight after 365 days", afternoon, got)
	}
	// In UTC whatever zone the time arrives in: pgx and time.Now hand back
	// local times, and a local day is not always 24 hours.
	evening := time.Date(2026, 2, 28, 23, 30, 0, 0, time.FixedZone("UTC-5", -5*3600))
	if got := passExpiry(evening, 1); !got.Equal(midnight.AddDate(0, 0, 2)) || got.Location() != time.UTC {
		t.Errorf("passExpiry(%v, 1) = %v, want %v in UTC", evening, got, midnight.AddDate(0, 0, 2))
	}

	// A term is the whole days a payment moved the expiry, rounded up, so a
	// refund taking it off lands at or before where the Pass would have been.
	for _, c := range []struct {
		base, expiry time.Time
		want         int
	}{
		{midnight, midnight.AddDate(0, 0, 365), 365},
		{afternoon, passExpiry(afternoon, 365), 366},
		{midnight, midnight.AddDate(0, 0, 365).Add(time.Nanosecond), 366},
	} {
		if got := termDays(c.base, c.expiry); got != c.want {
			t.Errorf("termDays(%v, %v) = %d, want %d", c.base, c.expiry, got, c.want)
		}
		if back := c.expiry.AddDate(0, 0, -termDays(c.base, c.expiry)); back.After(c.base) {
			t.Errorf("taking the term off %v lands at %v, after the base %v", c.expiry, back, c.base)
		}
	}
}
