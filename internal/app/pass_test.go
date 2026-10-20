package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

func TestPassCodes(t *testing.T) {
	a := &App{cfg: Config{Pass: PassConfig{Secret: testPassSecret}}}
	other := &App{cfg: Config{Pass: PassConfig{Secret: testPassSecret + "-rotated"}}}
	seen := map[string]bool{}
	for range 200 {
		var tokens [3]string
		for i := range tokens {
			var err error
			if tokens[i], err = randomToken(); err != nil {
				t.Fatal(err)
			}
		}
		token, nonce, another := tokens[0], tokens[1], tokens[2]
		code := a.passClaimCode(token, nonce)
		// The same claim and nonce give the same code, which is what makes
		// asking again safe. Either half alone gives another, and so do a
		// reissue code with the same token and another secret.
		if again := a.passClaimCode(token, nonce); again != code {
			t.Fatalf("claim %q and nonce %q gave %q, then %q", token, nonce, code, again)
		}
		if a.passReissuedCode(token, nonce) != a.passReissuedCode(token, nonce) {
			t.Fatalf("reissue code %q and nonce %q gave two codes", token, nonce)
		}
		for name, c := range map[string]string{
			"another nonce":  a.passClaimCode(token, another),
			"another claim":  a.passClaimCode(another, nonce),
			"the two halves": a.passClaimCode(nonce, token),
			"a reissue code": a.passReissuedCode(token, nonce),
			"another secret": other.passClaimCode(token, nonce),
		} {
			if c == code {
				t.Fatalf("%s gave claim %q and nonce %q's code", name, token, nonce)
			}
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
		// Pasted out of mail or a web page, which can turn the dashes into
		// no-break, narrow or thin spaces, or into other dashes, and carry
		// characters nobody sees.
		"ABCDE\u00a0FGHJK\u202fMNPQR\u2009STVWX":       "ABCDEFGHJKMNPQRSTVWX",
		"ABCDE\u2013FGHJK\u2212MNPQR\u2011STVWX":       "ABCDEFGHJKMNPQRSTVWX",
		"\ufeffABCDE\u200bFGHJK\u00adMNPQR\u2060STVWX": "ABCDEFGHJKMNPQRSTVWX",
		"ABCDE\r\nFGHJK\u3000MNPQR\u2028STVWX\n":       "ABCDEFGHJKMNPQRSTVWX",
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
		"ABCDE.FGHJK.MNPQR.STVWX", "ABCDE/FGHJK/MNPQR/STVWX",
	} {
		if got, ok := parsePassCode(raw); ok {
			t.Errorf("parse(%q) accepted as %q", raw, got)
		}
	}
}

// TestPageAndServerSkipTheSameSeparators holds the Pass page and the server
// to one set of characters that may stand between a code's groups. The page
// tells a reissue code from a Pass code by its characters once the
// separators are left out, and sends it as one or the other; a server that
// skipped fewer would refuse a code the page read, pasted with a no-break
// space say, as "not a reissue code" or "not a Pass code". Every character Go
// counts as a space is one of them, the ones JavaScript's \s matches among
// them.
func TestPageAndServerSkipTheSameSeparators(t *testing.T) {
	found := regexp.MustCompile(`\nconst CODE_SEPARATORS =\s*((?:"(?:[^"\\]|\\.)*"\s*\+?\s*)+);`).FindStringSubmatch(appScript(t))
	if found == nil {
		t.Fatal("app.js has no CODE_SEPARATORS made of string literals")
	}
	var page strings.Builder
	for _, literal := range regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).FindAllString(found[1], -1) {
		s, err := strconv.Unquote(literal)
		if err != nil {
			t.Fatalf("CODE_SEPARATORS: %s: %v", literal, err)
		}
		page.WriteString(s)
	}
	set := func(s string) []rune {
		runes := []rune(s)
		slices.Sort(runes)
		return slices.Compact(runes)
	}
	if got, want := set(page.String()), set(codeSeparators); !slices.Equal(got, want) {
		t.Fatalf("the page skips %+q between a code's groups, the server %+q", string(got), string(want))
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsSpace(r) && !isCodeSeparator(r) {
			t.Errorf("%U is a space, and is not skipped between a code's groups", r)
		}
	}
	for _, r := range "\u00a0\u202f\u2009\ufeff\u2013" {
		if !isCodeSeparator(r) {
			t.Errorf("%U is not skipped between a code's groups", r)
		}
	}
	// None of them is a character of a code.
	for _, r := range codeSeparators {
		if strings.ContainsRune(passAlphabet+"OIL"+strings.ToLower(passAlphabet+"OIL"), r) {
			t.Errorf("%U is a code's character and a separator", r)
		}
	}
}

// TestPageAndServerFoldTheSameLetters holds the Pass page and the server to one
// reading of the letters a code's alphabet leaves out. The server reads O as 0
// and I or L as 1 (parseCode), and the page sends a code typed with them
// written as those digits (canonicalCode, by CODE_FOLDS), which is how the
// server writes a code. Every character is put through both: the server's
// reading of it as a code's character, and the page's, in capitals as
// codeCharacters puts it and then folded by its table. Where the server reads
// the character as one of a code's, the two agree; where the server refuses it,
// the page folds nothing, so it sends no code the server would not have read
// from what was typed.
func TestPageAndServerFoldTheSameLetters(t *testing.T) {
	found := regexp.MustCompile(`\nconst CODE_FOLDS = \{([^}]*)\};\n`).FindStringSubmatch(appScript(t))
	if found == nil {
		t.Fatal("app.js has no CODE_FOLDS table")
	}
	folds := map[rune]rune{}
	entry := regexp.MustCompile(`^\s*([A-Z]): "([0-9A-Z])"\s*$`)
	for _, e := range strings.Split(found[1], ",") {
		m := entry.FindStringSubmatch(e)
		if m == nil {
			t.Fatalf("CODE_FOLDS: %q is not a capital and the character it is read as", e)
		}
		folds[rune(m[1][0])] = rune(m[2][0])
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if isCodeSeparator(r) {
			continue // skipped by both (TestPageAndServerSkipTheSameSeparators)
		}
		page, folded := folds[unicode.ToUpper(r)]
		if !folded {
			page = unicode.ToUpper(r)
		}
		server, ok := parseCode(string(r), 1)
		switch {
		case ok && rune(server[0]) != page:
			t.Errorf("the server reads %+q as %q, the page as %+q", r, server[0], page)
		case !ok && folded:
			t.Errorf("the page reads %+q as %q, a character the server refuses", r, page)
		}
	}
}

// TestReissueCodes holds a reissue code to a form a person can read out and
// type back, which the page and the server can tell from a Pass code: the word
// REISSUE and 28 characters of the alphabet, at least the 128 random bits a
// link's token had, read back in any case, with or without the dashes and the
// word.
func TestReissueCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		code, err := newReissueCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != reissueCodeLen || reissueCodeLen*5 < 128 || strings.Trim(code, passAlphabet) != "" {
			t.Fatalf("reissue code %q is not %d characters of the alphabet", code, reissueCodeLen)
		}
		if seen[code] {
			t.Fatalf("reissue code %q drawn twice", code)
		}
		seen[code] = true
		written := formatReissueCode(code)
		if !reissueCodeShape(written) {
			t.Fatalf("formatted %q", written)
		}
		bare := strings.ReplaceAll(strings.TrimPrefix(written, reissueCodePrefix+"-"), "-", "")
		for _, typed := range []string{
			written,
			strings.ToLower(written),
			strings.ReplaceAll(written, "-", " "),
			"\t" + bare + " ",
			bare,
			reissueCodePrefix + bare,
			"reissue " + strings.ToLower(bare),
			strings.ReplaceAll(written, "-", "\u00a0"),
			strings.ReplaceAll(written, "-", "\u202f"),
			strings.ReplaceAll(written, "-", "\u2013"),
			strings.ReplaceAll(written, "-", "\r\n"),
			"\u200b" + strings.ReplaceAll(written, "-", "\u2009") + "\ufeff",
		} {
			if back, ok := parseReissueCode(typed); !ok || back != code {
				t.Fatalf("parse(%q) = %q, %v; want %q", typed, back, ok, code)
			}
		}
		// Neither kind of code is ever read as the other.
		if _, ok := parsePassCode(written); ok {
			t.Fatalf("reissue code %q reads as a Pass code", written)
		}
		if _, ok := parsePassCode(bare); ok {
			t.Fatalf("reissue code %q without the word reads as a Pass code", bare)
		}
	}
	for raw, want := range map[string]string{
		"REISSUE-0O0O-1IiL-2345-6789-ABCD-EFGH-JKMN": "0000111123456789ABCDEFGHJKMN",
	} {
		if got, ok := parseReissueCode(raw); !ok || got != want {
			t.Errorf("parse(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{
		"", "REISSUE", "REISSUE-ABCD",
		"ABCDE-FGHJK-MNPQR-STVWX",                     // a Pass code
		"REISSUE-ABCD-EFGH-JKMN-PQRS-TVWX-YZ01-234",   // 27 characters
		"REISSUE-ABCD-EFGH-JKMN-PQRS-TVWX-YZ01-23456", // 29
		"REISSUE-ABCD-EFGH-JKMN-PQRS-TVWX-YZ01-234U",  // U is not in the alphabet
		"REISSUED-ABCD-EFGH-JKMN-PQRS-TVWX-YZ01-2345", // another word
		"REISSUE:ABCD-EFGH-JKMN-PQRS-TVWX-YZ01-2345",  // a colon
		"REISSUE_ABCD_EFGH_JKMN_PQRS_TVWX_YZ01_2345",  // underscores
		"REISSUE-ABCD-EFGH-JKMN-PQRS-TVWX-YZ01-234é",  // past ASCII
		"AAAAAAAAAAAAAAAAAAAAAA",                      // a link's token
	} {
		if got, ok := parseReissueCode(raw); ok {
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
		cfg.HourlyPosts != 30 || cfg.CheckoutCooldown != 30*time.Second || cfg.StripeAutomaticTax {
		t.Fatalf("full: %+v, %v; want, among the rest, the documented 30-second checkout cooldown", cfg, err)
	}

	bad := map[string]map[string]string{
		"secret without stripe":          {"EDUCHAN_STRIPE_SECRET_KEY": "", "EDUCHAN_STRIPE_WEBHOOK_SECRET": "", "EDUCHAN_STRIPE_PRICE_ID": "", "EDUCHAN_PUBLIC_URL": ""},
		"stripe without secret":          {"EDUCHAN_PASS_SECRET": ""},
		"short secret":                   {"EDUCHAN_PASS_SECRET": "short"},
		"secret of 31 characters":        {"EDUCHAN_PASS_SECRET": strings.Repeat("x", 31)},
		"webhook secret shape":           {"EDUCHAN_STRIPE_WEBHOOK_SECRET": "sk_x"},
		"publishable key":                {"EDUCHAN_STRIPE_SECRET_KEY": "pk_live_x"},
		"key shape":                      {"EDUCHAN_STRIPE_SECRET_KEY": "whsec_x"},
		"public url with an empty query": {"EDUCHAN_PUBLIC_URL": "https://educhan.example?"},
		"price shape":                    {"EDUCHAN_STRIPE_PRICE_ID": "prod_x"},
		"http public url":                {"EDUCHAN_PUBLIC_URL": "http://educhan.example"},
		"public url with a path":         {"EDUCHAN_PUBLIC_URL": "https://educhan.example/board"},
		"public url with a query":        {"EDUCHAN_PUBLIC_URL": "https://educhan.example?x=1"},
		"public url with a user":         {"EDUCHAN_PUBLIC_URL": "https://me@educhan.example"},
		"public url with an empty hash":  {"EDUCHAN_PUBLIC_URL": "https://educhan.example#"},
		"public url with a fragment":     {"EDUCHAN_PUBLIC_URL": "https://educhan.example#x"},
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

	// The secret is at least 32 characters, as the README and .env.example
	// say, and 32 is enough.
	env["EDUCHAN_PASS_SECRET"] = strings.Repeat("x", 32)
	set(t, env)
	if cfg, err := passConfigFromEnv(); err != nil || cfg.Secret != env["EDUCHAN_PASS_SECRET"] {
		t.Fatalf("a secret of 32 characters: %+v, %v", cfg, err)
	}
	env["EDUCHAN_PASS_SECRET"] = testPassSecret

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
		"trailing hash":       {func(c *PassConfig) { c.PublicURL = testPublicURL + "#" }, "EDUCHAN_PUBLIC_URL"},
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

	// A refund takes off the days of a term still to come, rounded up so
	// none of it is left, and nothing of a term that has run.
	for _, c := range []struct {
		end, now time.Time
		want     int
	}{
		{midnight, afternoon, 0},
		{midnight, midnight, 0},
		{midnight.AddDate(0, 0, 10), midnight, 10},
		{midnight.AddDate(0, 0, 10), afternoon, 10},
		{midnight.AddDate(0, 0, 10), midnight.Add(time.Nanosecond), 10},
	} {
		if got := daysOff(c.end, c.now); got != c.want {
			t.Errorf("daysOff(%v, %v) = %d, want %d", c.end, c.now, got, c.want)
		}
		if c.want > 0 && c.end.AddDate(0, 0, -daysOff(c.end, c.now)).After(c.now) {
			t.Errorf("taking %d days off a term ending %v leaves some of it after %v", c.want, c.end, c.now)
		}
	}
}
