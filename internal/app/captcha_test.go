package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// testCaptchaSecret is long enough for checkCaptchaSecret.
const testCaptchaSecret = "test-captcha-secret-0123456789abcdef"

// solveChallenge does what the browser's worker does: PBKDF2 over each counter
// from zero until a key starts with the challenge's prefix. It answers the
// payload a post carries, in the format app.js builds.
func solveChallenge(t testing.TB, c captchaChallenge) string {
	t.Helper()
	nonce, err := hex.DecodeString(c.Parameters.Nonce)
	if err != nil {
		t.Fatalf("challenge nonce: %v", err)
	}
	salt, err := hex.DecodeString(c.Parameters.Salt)
	if err != nil {
		t.Fatalf("challenge salt: %v", err)
	}
	for counter := uint32(0); counter < 1<<20; counter++ {
		key := captchaKey(nonce, salt, counter, c.Parameters.Cost)
		if strings.HasPrefix(hex.EncodeToString(key), c.Parameters.KeyPrefix) {
			return encodeCaptchaPayload(t, captchaPayload{
				Challenge: &c,
				Solution:  &captchaSolution{Counter: int64(counter), DerivedKey: hex.EncodeToString(key), Time: 12.5},
			})
		}
	}
	t.Fatal("no counter solves the challenge")
	return ""
}

// encodeCaptchaPayload is the client's btoa(JSON.stringify(payload)).
func encodeCaptchaPayload(t testing.TB, p any) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal captcha payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// decodeTestPayload undoes encodeCaptchaPayload, for a test that alters an
// answer before sending it.
func decodeTestPayload(t testing.TB, payload string) captchaPayload {
	t.Helper()
	p, err := decodeCaptchaPayload(payload)
	if err != nil {
		t.Fatalf("decode captcha payload: %v", err)
	}
	return p
}

// The worker vendored under web/lib/altcha/ produced this solution, run in
// Node on these inputs: counter 36 is the first whose key starts with 00. If
// the server's key derivation and the worker's ever disagree, no browser could
// answer a challenge, and this is the test that says so.
func TestCaptchaKeyMatchesTheWorker(t *testing.T) {
	nonce, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	salt, _ := hex.DecodeString("f0e1d2c3b4a5968778695a4b3c2d1e0f")
	const want = "00e1b959cc4436a876deb274a6abac47805e33d701c2b72265c469ccb010ac43"

	if got := hex.EncodeToString(captchaKey(nonce, salt, 36, 2000)); got != want {
		t.Fatalf("key for counter 36 = %s, want the worker's %s", got, want)
	}
	for counter := uint32(0); counter < 36; counter++ {
		if key := hex.EncodeToString(captchaKey(nonce, salt, counter, 2000)); strings.HasPrefix(key, "00") {
			t.Errorf("counter %d gives %s, but the worker reported 36 as the first key starting 00", counter, key)
		}
	}
}

func TestCaptchaRoundTrip(t *testing.T) {
	v := newAltchaVerifier(testCaptchaSecret, 8)
	now := time.Now()
	c, err := v.issue(now, 1)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	p := c.Parameters
	if p.Algorithm != captchaAlgorithm || p.Cost != captchaCost || p.KeyLength != captchaKeyLength {
		t.Errorf("parameters = %+v, want %s at cost %d for %d bytes", p, captchaAlgorithm, captchaCost, captchaKeyLength)
	}
	if want := now.Add(captchaTTL).Unix(); p.ExpiresAt != want {
		t.Errorf("expiresAt = %d, want %d", p.ExpiresAt, want)
	}
	if !wellFormedCaptcha(p) {
		t.Errorf("issued parameters are not well formed: %+v", p)
	}
	if !isHexOfLength(c.Signature, sha256.Size) {
		t.Errorf("signature = %q, want 64 lowercase hex digits", c.Signature)
	}

	payload := solveChallenge(t, c)
	if counter := decodeTestPayload(t, payload).Solution.Counter; counter < 1 || counter > 15 {
		t.Errorf("solved at counter %d, want 1 to 15 for a work of 8", counter)
	}
	ticket, err := v.check(payload, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("check a right answer: %v", err)
	}
	signature, _ := hex.DecodeString(c.Signature)
	if want := sha256.Sum256(signature); !bytes.Equal(ticket.key, want[:]) {
		t.Errorf("ticket key = %x, want the hash of the signature %x", ticket.key, want)
	}
	if !ticket.expires.Equal(time.Unix(p.ExpiresAt, 0)) {
		t.Errorf("ticket expires %s, want the challenge's expiry %s", ticket.expires, time.Unix(p.ExpiresAt, 0))
	}

	// Two challenges never share a salt, a nonce or a signature, so they never
	// share a spent row either.
	other, err := v.issue(now, 1)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if other.Parameters.Salt == p.Salt || other.Parameters.Nonce == p.Nonce || other.Signature == c.Signature {
		t.Error("two challenges came out with the same salt, nonce or signature")
	}
}

// Every way an answer can be wrong gets the same refusal, so a script learns
// nothing from it about which part to fix.
func TestCaptchaRefusesWrongAnswers(t *testing.T) {
	v := newAltchaVerifier(testCaptchaSecret, 8)
	now := time.Now()
	c, err := v.issue(now, 1)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	good := solveChallenge(t, c)
	if _, err := v.check(good, now); err != nil {
		t.Fatalf("check a right answer: %v", err)
	}

	// altered re-encodes the right answer after changing one thing in it.
	altered := func(change func(p *captchaPayload)) string {
		p := decodeTestPayload(t, good)
		challenge, solution := *p.Challenge, *p.Solution
		p.Challenge, p.Solution = &challenge, &solution
		change(&p)
		return encodeCaptchaPayload(t, p)
	}
	otherServer, err := newAltchaVerifier(strings.Repeat("x", 40), 8).issue(now, 1)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	otherChallenge, err := v.issue(now, 1)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(good)
	// padded is the right answer's JSON after the fewest leading spaces, which
	// JSON ignores, that leave its length off a multiple of three, so that its
	// standard base64 ends in padding and the unpadded case below is the right
	// answer refused for its encoding alone. (There is no such case for
	// URL-safe base64: it differs from the standard kind only in + and /,
	// which ASCII text never encodes to unless it holds >, ? or ~, so a real
	// answer reads the same in both.)
	var padded []byte
	for n := 0; len(padded)%3 == 0; n++ {
		padded = append(bytes.Repeat([]byte(" "), n), raw...)
	}
	withTail := func(tail string) string {
		return base64.StdEncoding.EncodeToString(append(append([]byte{}, raw...), tail...))
	}

	// The same answers as the cases below, in the encoding and length the
	// server reads, are accepted: what those cases change is nothing else.
	for name, payload := range map[string]string{
		"leading spaces":      base64.StdEncoding.EncodeToString(padded),
		"trailing whitespace": withTail(" \n"),
	} {
		if _, err := v.check(payload, now); err != nil {
			t.Fatalf("check the right answer, %s: %v", name, err)
		}
	}

	cases := map[string]string{
		"empty":           "",
		"not base64":      "not base64!",
		"unpadded base64": base64.RawStdEncoding.EncodeToString(padded),
		"not JSON":        base64.StdEncoding.EncodeToString([]byte("challenge=1")),
		"trailing data":   withTail(` {}`),
		"trailing brace":  withTail(`}`),
		// Refused only by its length: whitespace after the right answer is
		// otherwise accepted, as above.
		"too long":      withTail(strings.Repeat(" ", maxCaptchaPayload)),
		"unknown field": base64.StdEncoding.EncodeToString(append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"test":true}`)...)),
		// The widget's test mode, which answers without doing any work.
		"widget test mode": encodeCaptchaPayload(t, map[string]any{"challenge": nil, "solution": nil, "test": true}),
		"no challenge":     encodeCaptchaPayload(t, map[string]any{"solution": decodeTestPayload(t, good).Solution}),
		"no solution":      encodeCaptchaPayload(t, map[string]any{"challenge": c}),
		"null parts":       encodeCaptchaPayload(t, map[string]any{"challenge": nil, "solution": nil}),

		"other algorithm": altered(func(p *captchaPayload) { p.Challenge.Parameters.Algorithm = "SHA-256" }),
		"cheaper cost":    altered(func(p *captchaPayload) { p.Challenge.Parameters.Cost = 1 }),
		"zero cost":       altered(func(p *captchaPayload) { p.Challenge.Parameters.Cost = 0 }),
		"later expiry":    altered(func(p *captchaPayload) { p.Challenge.Parameters.ExpiresAt += 3600 }),
		"no expiry":       altered(func(p *captchaPayload) { p.Challenge.Parameters.ExpiresAt = 0 }),
		"shorter key":     altered(func(p *captchaPayload) { p.Challenge.Parameters.KeyLength = 16 }),
		"shorter prefix":  altered(func(p *captchaPayload) { p.Challenge.Parameters.KeyPrefix = p.Challenge.Parameters.KeyPrefix[:2] }),
		"uppercase prefix": altered(func(p *captchaPayload) {
			p.Challenge.Parameters.KeyPrefix = strings.ToUpper(p.Challenge.Parameters.KeyPrefix)
		}),
		"other nonce":     altered(func(p *captchaPayload) { p.Challenge.Parameters.Nonce = otherChallenge.Parameters.Nonce }),
		"other salt":      altered(func(p *captchaPayload) { p.Challenge.Parameters.Salt = otherChallenge.Parameters.Salt }),
		"other key sig":   altered(func(p *captchaPayload) { p.Challenge.Parameters.KeySignature = otherChallenge.Parameters.KeySignature }),
		"uppercase sig":   altered(func(p *captchaPayload) { p.Challenge.Signature = strings.ToUpper(p.Challenge.Signature) }),
		"short sig":       altered(func(p *captchaPayload) { p.Challenge.Signature = p.Challenge.Signature[:32] }),
		"no sig":          altered(func(p *captchaPayload) { p.Challenge.Signature = "" }),
		"other server":    altered(func(p *captchaPayload) { p.Challenge = &otherServer }),
		"other challenge": altered(func(p *captchaPayload) { p.Challenge = &otherChallenge }),
		"wrong key":       altered(func(p *captchaPayload) { p.Solution.DerivedKey = flipLastHexDigit(p.Solution.DerivedKey) }),
		"uppercase key":   altered(func(p *captchaPayload) { p.Solution.DerivedKey = strings.ToUpper(p.Solution.DerivedKey) }),
		"prefix only":     altered(func(p *captchaPayload) { p.Solution.DerivedKey = p.Challenge.Parameters.KeyPrefix }),
		"no key":          altered(func(p *captchaPayload) { p.Solution.DerivedKey = "" }),
		"key with prefix": altered(func(p *captchaPayload) {
			p.Solution.DerivedKey = p.Challenge.Parameters.KeyPrefix + strings.Repeat("0", 32)
		}),
		"key not hex":       altered(func(p *captchaPayload) { p.Solution.DerivedKey = strings.Repeat("zz", captchaKeyLength) }),
		"key one byte long": altered(func(p *captchaPayload) { p.Solution.DerivedKey += "00" }),
	}
	if len(cases["too long"]) <= maxCaptchaPayload {
		t.Fatalf("the too-long case is only %d bytes", len(cases["too long"]))
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.check(payload, now); !errors.Is(err, errCaptchaInvalid) {
				t.Errorf("check = %v, want errCaptchaInvalid", err)
			}
		})
	}
}

func flipLastHexDigit(s string) string {
	last := s[len(s)-1]
	if last == '0' {
		return s[:len(s)-1] + "1"
	}
	return s[:len(s)-1] + "0"
}

// The counter in a solution is the worker's to report and nothing reads it: the
// key is the proof, and only the challenge's own key matches its signature.
func TestCaptchaReadsOnlyTheKey(t *testing.T) {
	v := newAltchaVerifier(testCaptchaSecret, 8)
	now := time.Now()
	c, err := v.issue(now, 1)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	p := decodeTestPayload(t, solveChallenge(t, c))
	p.Solution.Counter, p.Solution.Time = 999999, 0
	if _, err := v.check(encodeCaptchaPayload(t, p), now); err != nil {
		t.Errorf("check with another counter: %v, want the right key to be enough", err)
	}
}

func TestCaptchaExpires(t *testing.T) {
	v := newAltchaVerifier(testCaptchaSecret, 8)
	now := time.Now()
	c, err := v.issue(now, 1)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	payload := solveChallenge(t, c)
	expires := time.Unix(c.Parameters.ExpiresAt, 0)

	if _, err := v.check(payload, expires.Add(-time.Second)); err != nil {
		t.Errorf("a second before expiry: %v, want it accepted", err)
	}
	for _, at := range []time.Time{expires, expires.Add(time.Hour)} {
		if _, err := v.check(payload, at); !errors.Is(err, errCaptchaExpired) {
			t.Errorf("at %s: %v, want errCaptchaExpired", at.Sub(expires), err)
		}
	}

	// "Expired" is said only of a challenge this server signed: one with its
	// expiry rewritten, or signed elsewhere, is invalid whenever it arrives.
	p := decodeTestPayload(t, payload)
	p.Challenge.Parameters.ExpiresAt -= 3600
	if _, err := v.check(encodeCaptchaPayload(t, p), now); !errors.Is(err, errCaptchaInvalid) {
		t.Errorf("rewritten expiry: %v, want errCaptchaInvalid", err)
	}
	if _, err := newAltchaVerifier(strings.Repeat("y", 40), 8).check(payload, expires.Add(time.Hour)); !errors.Is(err, errCaptchaInvalid) {
		t.Errorf("another server's expired challenge: %v, want errCaptchaInvalid", err)
	}
}

// ALTCHA signs a challenge's parameters as JSON with the keys sorted. The
// struct's fields are declared in that order so that json.Marshal writes the
// same text; a field added out of order would break every widget and library
// that checks the signature the ALTCHA way.
func TestCaptchaParametersEncodeWithSortedKeys(t *testing.T) {
	c, err := newAltchaVerifier(testCaptchaSecret, 8).issue(time.Now(), 1)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	direct, err := json.Marshal(c.Parameters)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(direct))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sorted, err := json.Marshal(fields) // encoding/json writes map keys sorted
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(direct, sorted) {
		t.Errorf("parameters encode as\n %s\nnot in sorted key order\n %s", direct, sorted)
	}
	want := []string{"algorithm", "cost", "expiresAt", "keyLength", "keyPrefix", "keySignature", "nonce", "salt"}
	if got := sortedKeys(fields); !slices.Equal(got, want) {
		t.Errorf("parameter keys = %v, want ALTCHA's %v", got, want)
	}
}

// The two keys derived from the secret are different, so a key signature can
// never pass for a challenge signature or the other way round.
func TestCaptchaKeysAreSeparate(t *testing.T) {
	v := newAltchaVerifier(testCaptchaSecret, 8)
	if bytes.Equal(v.signKey, v.keySignKey) {
		t.Fatal("the challenge key and the derived-key key are the same")
	}
	w := newAltchaVerifier(testCaptchaSecret+"!", 8)
	if bytes.Equal(v.signKey, w.signKey) || bytes.Equal(v.keySignKey, w.keySignKey) {
		t.Error("different secrets gave the same keys")
	}
}

func TestCaptchaCounterRange(t *testing.T) {
	cases := []struct {
		work, scale int
		low, high   uint32
	}{
		{work: 1500, scale: 1, low: 1, high: 2999},
		{work: 1500, scale: 4, low: 1, high: 11999},
		{work: 1500, scale: 0, low: 1, high: 2999}, // a scale below one is one
		{work: 1, scale: 1, low: 1, high: 1},       // never the free counter zero
		{work: 0, scale: 1, low: 1, high: 1},       // work below one is one
		{work: 3, scale: 1, low: 1, high: 5},
		{work: maxCaptchaWork, scale: captchaMaxScale, low: 1, high: 2*maxCaptchaWork - 1},
	}
	for _, tc := range cases {
		seen := map[uint32]bool{}
		var sum float64
		const draws = 2000
		for range draws {
			n, err := captchaCounter(tc.work, tc.scale)
			if err != nil {
				t.Fatalf("captchaCounter: %v", err)
			}
			if n < tc.low || n > tc.high {
				t.Fatalf("captchaCounter(%d, %d) = %d, want %d to %d", tc.work, tc.scale, n, tc.low, tc.high)
			}
			seen[n] = true
			sum += float64(n)
		}
		if tc.high-tc.low < 10 && (!seen[tc.low] || !seen[tc.high]) {
			t.Errorf("captchaCounter(%d, %d) never drew both ends of %d to %d", tc.work, tc.scale, tc.low, tc.high)
		}
		// Uniform from the bottom of the range, so the average is the work
		// asked for: within five standard errors of it.
		mean := float64(tc.low+tc.high) / 2
		spread := float64(tc.high-tc.low) / math.Sqrt(12) / math.Sqrt(draws)
		if got := sum / draws; math.Abs(got-mean) > 5*spread+0.5 {
			t.Errorf("captchaCounter(%d, %d) averaged %.0f, want about %.0f", tc.work, tc.scale, got, mean)
		}
	}
}

func TestCaptchaScale(t *testing.T) {
	for n, want := range map[int]int{
		1: 1, 60: 1, 61: 2, 120: 2, 121: 4, 180: 4, 181: 8, 240: 8, 241: 8, captchaChallengeLimit: 8,
	} {
		if got := captchaScale(n); got != want {
			t.Errorf("captchaScale(%d) = %d, want %d", n, got, want)
		}
	}
}

// meterAt points a meter's clock at *now, read under the meter's lock.
func meterAt(m *captchaMeter, now *time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = func() time.Time { return *now }
}

func TestCaptchaMeter(t *testing.T) {
	m := newCaptchaMeter()
	now := time.Unix(1_800_000_000, 0).Add(10 * time.Second) // ten seconds into a window
	meterAt(m, &now)

	for n := 1; n <= captchaChallengeLimit; n++ {
		scale, _, ok := m.take("203.0.113.7")
		if !ok {
			t.Fatalf("challenge %d refused, want up to %d", n, captchaChallengeLimit)
		}
		if want := captchaScale(n); scale != want {
			t.Fatalf("challenge %d at scale %d, want %d", n, scale, want)
		}
	}
	if _, wait, ok := m.take("203.0.113.7"); ok || wait != 50*time.Second {
		t.Errorf("challenge past the limit: ok %t, wait %s, want refused for the 50s left in the window", ok, wait)
	}
	if scale, _, ok := m.take("198.51.100.2"); !ok || scale != 1 {
		t.Errorf("another address: scale %d, ok %t, want its own count at the base", scale, ok)
	}

	// The next window starts the count again.
	now = now.Add(50 * time.Second)
	if scale, _, ok := m.take("203.0.113.7"); !ok || scale != 1 {
		t.Errorf("next window: scale %d, ok %t, want a fresh count", scale, ok)
	}

	// The sweep forgets every address whose window has ended.
	now = now.Add(captchaWindow)
	m.take("192.0.2.1")
	m.sweepExpired()
	m.mu.Lock()
	kept := sortedKeys(m.counts)
	m.mu.Unlock()
	if !slices.Equal(kept, []string{"192.0.2.1"}) {
		t.Errorf("after the sweep the meter holds %v, want only the address counted this window", kept)
	}

	var none *captchaMeter
	none.sweepExpired() // a nil meter has nothing to sweep
}

// A meter that fills up drops the earlier windows' addresses itself, as the
// cooldown limiters do, without waiting for the sweep.
func TestCaptchaMeterSweepsWhenFull(t *testing.T) {
	m := newCaptchaMeter()
	now := time.Unix(1_800_000_000, 0)
	meterAt(m, &now)
	for i := range cooldownSweepThreshold {
		m.take(fmt.Sprintf("198.51.100.%d:%d", i%256, i/256))
	}
	// Full of this window's own addresses, the meter is scanned once and not
	// again until the window changes.
	m.take("one more")
	window := now.UnixNano() / int64(captchaWindow)
	m.mu.Lock()
	if m.swept != window || len(m.counts) != cooldownSweepThreshold+1 {
		t.Errorf("after filling up: swept window %d, %d addresses; want %d, %d", m.swept, len(m.counts), window, cooldownSweepThreshold+1)
	}
	// So an entry from an earlier window, which a scan would drop, outlives a
	// further challenge in this one.
	m.counts["from the window before"] = captchaCount{window: window - 1, n: 1}
	m.mu.Unlock()
	m.take("and another")
	m.mu.Lock()
	if _, kept := m.counts["from the window before"]; !kept {
		t.Error("a challenge after the window's sweep scanned the full meter again")
	}
	m.mu.Unlock()

	// The next window's first challenge scans it, and drops that entry with
	// the rest.
	now = now.Add(captchaWindow)
	m.take("new")
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, kept := m.counts["from the window before"]; kept || len(m.counts) != 1 {
		t.Errorf("meter holds %d addresses after filling up, want only this window's one", len(m.counts))
	}
}

// An IPv6 client is counted by its /64, so moving to another address in the
// block it was given does not start a fresh count.
func TestCaptchaMeterKey(t *testing.T) {
	for ip, want := range map[string]string{
		"203.0.113.7":                    "203.0.113.7",
		"2001:db8:1:2:3:4:5:6":           "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff:ffff:ff": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":                "2001:db8:1:3::/64",
		"fe80::1%eth0":                   "fe80::/64",
		"::ffff:203.0.113.7":             "::ffff:203.0.113.7",
		"not an address":                 "not an address",
	} {
		if got := captchaMeterKey(ip); got != want {
			t.Errorf("captchaMeterKey(%q) = %q, want %q", ip, got, want)
		}
	}
}

func TestCaptchaConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv("EDUCHAN_CAPTCHA_SECRET", "")
		t.Setenv("EDUCHAN_CAPTCHA_WORK", "")
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.CaptchaSecret != "" || cfg.CaptchaWork != defaultCaptchaWork {
			t.Errorf("secret %q, work %d, want the captcha off at work %d", cfg.CaptchaSecret, cfg.CaptchaWork, defaultCaptchaWork)
		}
		if newCaptcha(cfg) != nil {
			t.Error("the captcha is on without a secret")
		}
	})
	t.Run("set", func(t *testing.T) {
		t.Setenv("EDUCHAN_CAPTCHA_SECRET", " "+testCaptchaSecret+"\n")
		t.Setenv("EDUCHAN_CAPTCHA_WORK", " 3000 ")
		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv: %v", err)
		}
		if cfg.CaptchaSecret != testCaptchaSecret || cfg.CaptchaWork != 3000 {
			t.Errorf("secret %q, work %d, want the trimmed secret at work 3000", cfg.CaptchaSecret, cfg.CaptchaWork)
		}
		v, ok := newCaptcha(cfg).(*altchaVerifier)
		if !ok || v.work != 3000 {
			t.Errorf("verifier = %#v, want ALTCHA's at work 3000", newCaptcha(cfg))
		}
	})
	for _, raw := range []string{"0", "-5", "lots", "1.5", "7501"} {
		t.Run("work "+raw, func(t *testing.T) {
			t.Setenv("EDUCHAN_CAPTCHA_WORK", raw)
			if _, err := ConfigFromEnv(); err == nil {
				t.Errorf("expected an error for EDUCHAN_CAPTCHA_WORK=%q", raw)
			}
		})
	}
	t.Run("largest work", func(t *testing.T) {
		t.Setenv("EDUCHAN_CAPTCHA_WORK", "7500")
		if _, err := ConfigFromEnv(); err != nil {
			t.Errorf("EDUCHAN_CAPTCHA_WORK at its largest: %v", err)
		}
	})
	t.Run("work zero in code", func(t *testing.T) {
		v, ok := newCaptcha(Config{CaptchaSecret: testCaptchaSecret}).(*altchaVerifier)
		if !ok || v.work != defaultCaptchaWork {
			t.Errorf("verifier = %#v, want ALTCHA's at the default work", v)
		}
	})
}

// A secret short enough to guess would let a script sign its own challenges,
// so the server refuses to start with one rather than run a captcha that only
// looks like it works.
func TestCaptchaRefusesAShortSecret(t *testing.T) {
	short := strings.Repeat("s", minCaptchaSecretLen-1)
	if err := checkCaptchaSecret(Config{CaptchaSecret: short}); err == nil {
		t.Error("a secret one character short was accepted")
	}
	if err := checkCaptchaSecret(Config{CaptchaSecret: short + "s"}); err != nil {
		t.Errorf("a secret of exactly %d characters: %v", minCaptchaSecretLen, err)
	}
	if err := checkCaptchaSecret(Config{}); err != nil {
		t.Errorf("no secret (captcha off): %v", err)
	}
	_, err := New(context.Background(), Config{CaptchaSecret: short}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "EDUCHAN_CAPTCHA_SECRET") {
		t.Errorf("New with a short secret: %v, want it refused naming EDUCHAN_CAPTCHA_SECRET", err)
	}
}

// sortedKeys lists a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
