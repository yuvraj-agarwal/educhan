package app

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// The captcha is a proof-of-work puzzle in ALTCHA's protocol, issued and
// checked here with no third party involved (build plan 2.6). The server hands
// out a signed challenge; the browser runs PBKDF2 over a counter until it finds
// the one whose key starts with the prefix the challenge gives; the post
// carries the answer. Solving costs the browser a moment of arithmetic and
// costs a script that posts in bulk the same moment every time. Nobody but
// educhan sees the request, nothing is stored about who solved it, and no
// cookie is set: the answer travels only in the requests that may spend it.
//
// The challenge is ALTCHA's "deterministic" kind. The server picks the counter
// itself, derives its key, and publishes the first half of that key as the
// prefix to find, together with an HMAC of the whole key. Checking an answer is
// then at most two HMACs, never a key derivation, so a wrong answer costs the
// server no more than a right one, and the work a challenge asks for is set by
// the counter the server chose, not a matter of luck.
//
// The challenge and the answer are in ALTCHA's own format (its "v2" challenge,
// sent as base64 JSON), so its widget or any of its server libraries would
// understand them. educhan uses neither: its server side is this file, and its
// browser side is a few functions in app.js around ALTCHA's PBKDF2 worker,
// vendored under web/lib/altcha/.

const (
	// captchaAlgorithm is the key derivation every challenge asks for. PBKDF2
	// is in every browser's Web Crypto, so the worker needs no WebAssembly,
	// which the CSP does not allow.
	captchaAlgorithm = "PBKDF2/SHA-256"
	// captchaCost is PBKDF2's iteration count for one attempt. Larger is
	// cheaper for the browser per unit of work, since each attempt carries a
	// fixed overhead, and dearer for the server, which derives one key per
	// challenge it issues.
	captchaCost = 2000
	// captchaKeyLength is the derived key's length in bytes.
	captchaKeyLength = 32
	// captchaPrefixLength is how much of the key the challenge gives away. The
	// rest is what an answer has to produce, and the key signature what it is
	// checked against.
	captchaPrefixLength = captchaKeyLength / 2
	// captchaTTL is how long a challenge can be answered. It covers writing a
	// long post after the puzzle started when the reader moved into the box;
	// the client solves a fresh one if its answer is about to run out.
	captchaTTL = 30 * time.Minute
	// defaultCaptchaWork is the work a challenge asks for: the average counter
	// its answer is at, which takes one attempt more than that to reach
	// (captchaCounter). At captchaCost that is about two seconds on a laptop
	// in Chromium, which runs every worker's Web Crypto on one thread, and a
	// few more on a slow phone, spent while the post is being written.
	defaultCaptchaWork = 1500
	// maxCaptchaWork caps the work a challenge asks for, scaled up for a busy
	// address included, and EDUCHAN_CAPTCHA_WORK is held to an eighth of it
	// (captchaMaxScale). A counter is drawn from up to just under twice the
	// work, and the worker tries every counter from zero up to it, so the base
	// puzzle, the one people are given, asks at most 15,000 attempts: about
	// twenty seconds on a laptop and under a minute on a slow phone, inside the
	// two minutes app.js gives a puzzle before saying it could not finish. Only
	// an address asking for challenges far faster than anyone posts is given
	// more than that.
	maxCaptchaWork = 60000
	// maxCaptchaPayload bounds an answer before it is decoded. A real one is
	// about seven hundred bytes.
	maxCaptchaPayload = 4096
)

// Adaptive difficulty: an address that asks for challenges faster than people
// post gets harder ones. The count is kept per client address, in this
// instance's memory, in fixed one-minute windows, so the meter holds an
// address no longer than a one-minute cooldown would.
const (
	// captchaWindow is the period challenges are counted over.
	captchaWindow = time.Minute
	// captchaFreeChallenges is how many challenges one address gets in a
	// window at the base difficulty. It is generous because a campus NAT puts
	// a lecture hall behind one address.
	captchaFreeChallenges = 60
	// captchaMaxScale caps the difficulty at this multiple of the base, which
	// doubles for every further captchaFreeChallenges in the window.
	captchaMaxScale = 8
	// captchaChallengeLimit is how many challenges one address may have in a
	// window at all. Each costs the server a key derivation, about a
	// millisecond of a core, so past this the endpoint answers 429. Every
	// challenge past 180 in a window is already at captchaMaxScale.
	captchaChallengeLimit = 300
)

// The refusals a captcha answer can earn, sent beside the message so the
// client knows to solve a fresh puzzle rather than resend the same answer.
const (
	captchaMissing = "required"
	captchaInvalid = "invalid"
	captchaExpired = "expired"
	captchaUsed    = "used"
)

var (
	errCaptchaInvalid = errors.New("captcha answer does not verify")
	errCaptchaExpired = errors.New("captcha challenge has expired")
)

// captchaParameters is a challenge's signed content, field for field ALTCHA's
// v2 challenge parameters. The fields are declared in key order: the signature
// covers this struct encoded as JSON, which is then the same text as ALTCHA's
// sorted-key encoding of the same values.
type captchaParameters struct {
	Algorithm    string `json:"algorithm"`
	Cost         int    `json:"cost"`
	ExpiresAt    int64  `json:"expiresAt"`
	KeyLength    int    `json:"keyLength"`
	KeyPrefix    string `json:"keyPrefix"`
	KeySignature string `json:"keySignature"`
	Nonce        string `json:"nonce"`
	Salt         string `json:"salt"`
}

// captchaChallenge is what GET /api/captcha answers.
type captchaChallenge struct {
	Parameters captchaParameters `json:"parameters"`
	Signature  string            `json:"signature"`
}

// captchaSolution is the worker's answer. Time is how long it took, which the
// worker reports and nothing reads.
type captchaSolution struct {
	Counter    int64   `json:"counter"`
	DerivedKey string  `json:"derivedKey"`
	Time       float64 `json:"time"`
}

// captchaPayload is the answer as a post carries it: the challenge as issued
// and the solution, as JSON, base64-encoded.
type captchaPayload struct {
	Challenge *captchaChallenge `json:"challenge"`
	Solution  *captchaSolution  `json:"solution"`
}

// captchaTicket is a checked answer, ready to be spent. key identifies the
// challenge, so the answer works once; expires is when the challenge would
// have stopped working anyway, after which nothing needs to remember it.
type captchaTicket struct {
	key     []byte
	expires time.Time
}

// captchaVerifier issues challenges and checks answers. The real one is
// altchaVerifier; tests swap in fakes that pass or fail everything, so a test
// about posting does not have to solve puzzles.
type captchaVerifier interface {
	// issue makes a challenge asking for scale times the base work.
	issue(now time.Time, scale int) (captchaChallenge, error)
	// check verifies an answer, without spending it.
	check(payload string, now time.Time) (captchaTicket, error)
}

// altchaVerifier is the captcha in production: ALTCHA's protocol, keyed by
// EDUCHAN_CAPTCHA_SECRET.
type altchaVerifier struct {
	// signKey signs a challenge's parameters, so an answer can only be to a
	// challenge this server issued, unaltered.
	signKey []byte
	// keySignKey signs a challenge's derived key, so an answer can be checked
	// without deriving it again.
	keySignKey []byte
	// work is the work a challenge at scale 1 asks for: the average counter,
	// one fewer than the average attempts.
	work int
}

func newAltchaVerifier(secret string, work int) *altchaVerifier {
	// Two keys from one secret, each labelled for its use, so neither HMAC can
	// stand in for the other.
	return &altchaVerifier{
		signKey:    hmacSum([]byte(secret), "educhan captcha: challenge"),
		keySignKey: hmacSum([]byte(secret), "educhan captcha: derived key"),
		work:       work,
	}
}

func (v *altchaVerifier) issue(now time.Time, scale int) (captchaChallenge, error) {
	salt := make([]byte, 16)
	nonce := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return captchaChallenge{}, fmt.Errorf("captcha salt: %w", err)
	}
	if _, err := rand.Read(nonce); err != nil {
		return captchaChallenge{}, fmt.Errorf("captcha nonce: %w", err)
	}
	counter, err := captchaCounter(v.work, scale)
	if err != nil {
		return captchaChallenge{}, err
	}
	key := captchaKey(nonce, salt, counter, captchaCost)

	params := captchaParameters{
		Algorithm:    captchaAlgorithm,
		Cost:         captchaCost,
		ExpiresAt:    now.Add(captchaTTL).Unix(),
		KeyLength:    captchaKeyLength,
		KeyPrefix:    hex.EncodeToString(key[:captchaPrefixLength]),
		KeySignature: hex.EncodeToString(hmacBytes(v.keySignKey, key)),
		Nonce:        hex.EncodeToString(nonce),
		Salt:         hex.EncodeToString(salt),
	}
	signature, err := v.sign(params)
	if err != nil {
		return captchaChallenge{}, err
	}
	return captchaChallenge{Parameters: params, Signature: signature}, nil
}

// captchaCounter draws the counter a challenge's answer is at: uniformly from
// one to just under twice the work asked for, so it averages the work and is
// never the free counter zero. The worker counts up from zero, so the answer
// takes the counter plus one attempts: the work plus one on average, and at
// most twice the work. Every counter in the range is as likely as any other,
// so counting up from the bottom is the best anyone can do: a script that
// knows the range saves nothing over a browser, as it would if the range
// started above zero and the browser did not.
func captchaCounter(work, scale int) (uint32, error) {
	mean := min(max(int64(work), 1)*int64(max(scale, 1)), maxCaptchaWork)
	n, err := rand.Int(rand.Reader, big.NewInt(2*mean-1))
	if err != nil {
		return 0, fmt.Errorf("captcha counter: %w", err)
	}
	return uint32(1 + n.Int64()), nil
}

// captchaKey is the key an answer has to reproduce: PBKDF2 over the nonce with
// the counter appended as four big-endian bytes, which is how ALTCHA's worker
// builds the password it tries.
func captchaKey(nonce, salt []byte, counter uint32, cost int) []byte {
	password := binary.BigEndian.AppendUint32(append([]byte(nil), nonce...), counter)
	return pbkdf2.Key(password, salt, cost, captchaKeyLength, sha256.New)
}

// sign is the challenge signature: an HMAC over the parameters as JSON.
func (v *altchaVerifier) sign(params captchaParameters) (string, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("captcha parameters: %w", err)
	}
	return hex.EncodeToString(hmacBytes(v.signKey, raw)), nil
}

// check verifies an answer: that it is to a challenge this server signed,
// unaltered, that the challenge has not run out, and that the key it brings is
// the one the challenge was made from. It says nothing about whether the
// answer was used before; spending it is the store's job.
//
// Every refusal is errCaptchaInvalid except a genuine challenge that ran out,
// which is errCaptchaExpired. The shape is checked before anything is hashed,
// and the signature before the expiry, so "expired" is only ever said about a
// challenge this server issued.
func (v *altchaVerifier) check(payload string, now time.Time) (captchaTicket, error) {
	p, err := decodeCaptchaPayload(payload)
	if err != nil {
		return captchaTicket{}, err
	}
	params := p.Challenge.Parameters
	if !wellFormedCaptcha(params) {
		return captchaTicket{}, errCaptchaInvalid
	}
	signature, err := hex.DecodeString(p.Challenge.Signature)
	if err != nil || len(signature) != sha256.Size {
		return captchaTicket{}, errCaptchaInvalid
	}
	want, err := v.sign(params)
	if err != nil {
		return captchaTicket{}, err
	}
	if !hmac.Equal([]byte(want), []byte(p.Challenge.Signature)) {
		return captchaTicket{}, errCaptchaInvalid
	}

	expires := time.Unix(params.ExpiresAt, 0)
	if !now.Before(expires) {
		return captchaTicket{}, errCaptchaExpired
	}

	key, err := hex.DecodeString(p.Solution.DerivedKey)
	if err != nil || len(key) != captchaKeyLength || !isLowerHex(p.Solution.DerivedKey) {
		return captchaTicket{}, errCaptchaInvalid
	}
	// The key signature is the server's own, under the challenge signature, so
	// a key that matches it is the key the challenge was made from, and its
	// first half is the signed prefix: there is nothing else to compare.
	keySignature, _ := hex.DecodeString(params.KeySignature) // well formed, checked above
	if !hmac.Equal(hmacBytes(v.keySignKey, key), keySignature) {
		return captchaTicket{}, errCaptchaInvalid
	}

	// The challenge, not the answer, is what is spent: there is exactly one
	// right key per challenge, so the two come to the same thing, and the
	// signature is the part of it the server made.
	spent := sha256.Sum256(signature)
	return captchaTicket{key: spent[:], expires: expires}, nil
}

// decodeCaptchaPayload undoes the client's btoa(JSON.stringify(…)). It refuses
// an answer too long to be real, other fields, anything after the one value,
// and the widget's own test-mode answer, whose challenge and solution are
// null. It is not stricter than encoding/json, which matches keys in any case
// and keeps the last of a repeated one, and need not be: what is checked and
// spent is the signature, which only the server's own HMAC matches, written
// one way.
func decodeCaptchaPayload(payload string) (captchaPayload, error) {
	if len(payload) > maxCaptchaPayload {
		return captchaPayload{}, errCaptchaInvalid
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return captchaPayload{}, errCaptchaInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p captchaPayload
	if err := dec.Decode(&p); err != nil {
		return captchaPayload{}, errCaptchaInvalid
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return captchaPayload{}, errCaptchaInvalid
	}
	if p.Challenge == nil || p.Solution == nil {
		return captchaPayload{}, errCaptchaInvalid
	}
	return p, nil
}

// wellFormedCaptcha reports whether parameters have the shape this server
// issues, before anything is hashed: the algorithm and key length it uses, and
// lowercase hex of the right length everywhere hex belongs, so anything not
// shaped like a challenge this server issued is turned away before any HMAC
// is computed. One spent key per challenge rests on the signature, not on
// this (see decodeCaptchaPayload). The cost and expiry need no check of their
// own: the signature covers them.
func wellFormedCaptcha(p captchaParameters) bool {
	return p.Algorithm == captchaAlgorithm &&
		p.KeyLength == captchaKeyLength &&
		isHexOfLength(p.KeyPrefix, captchaPrefixLength) &&
		isHexOfLength(p.KeySignature, sha256.Size) &&
		isHexOfLength(p.Nonce, 16) &&
		isHexOfLength(p.Salt, 16)
}

func isHexOfLength(s string, bytes int) bool {
	return len(s) == 2*bytes && isLowerHex(s)
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func hmacBytes(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

/* Adaptive difficulty -------------------------------------------------------- */

// captchaMeter counts the challenges each client address has asked for in the
// current window. It holds an address, a count, and the minute the count
// started, nothing finer, and an entry from an earlier window is dropped by
// the cooldown sweep that forgets the flood-control entries, within
// cooldownSweepInterval of its window ending.
type captchaMeter struct {
	window time.Duration
	now    func() time.Time

	mu     sync.Mutex
	counts map[string]captchaCount
	// swept is the window take last swept in, so a map that is large with
	// the current window's own addresses is scanned once per window rather
	// than on every challenge.
	swept int64
}

type captchaCount struct {
	window int64
	n      int
}

func newCaptchaMeter() *captchaMeter {
	return &captchaMeter{window: captchaWindow, now: time.Now, counts: map[string]captchaCount{}}
}

// take counts one more challenge for key. It answers the difficulty to issue
// it at, or, past captchaChallengeLimit, how long until the window ends.
func (m *captchaMeter) take(key string) (scale int, retryAfter time.Duration, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	window := now.UnixNano() / int64(m.window)
	if len(m.counts) >= cooldownSweepThreshold && m.swept != window {
		m.sweep(window)
		m.swept = window
	}
	c := m.counts[key]
	if c.window != window {
		c = captchaCount{window: window}
	}
	if c.n >= captchaChallengeLimit {
		return 0, time.Duration((window+1)*int64(m.window) - now.UnixNano()), false
	}
	c.n++
	m.counts[key] = c
	return captchaScale(c.n), 0, true
}

// captchaMeterKey is the address a challenge is counted against: the client
// address, except that an IPv6 address counts as its /64, the block one
// subscriber is usually given whole, so a client cannot start a fresh count by
// moving to the next address in its own block. An address that does not parse
// is counted as it is.
func captchaMeterKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is6() || addr.Is4In6() {
		return ip
	}
	prefix, err := addr.WithZone("").Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.String()
}

// captchaScale is the difficulty for the nth challenge an address has asked
// for in a window: the base for the first captchaFreeChallenges, then double
// for each further captchaFreeChallenges, up to captchaMaxScale.
func captchaScale(n int) int {
	scale := 1
	for over := (n - 1) / captchaFreeChallenges; over > 0 && scale < captchaMaxScale; over-- {
		scale *= 2
	}
	return scale
}

// sweep drops counts from earlier windows. Callers must hold the mutex.
func (m *captchaMeter) sweep(window int64) {
	for key, c := range m.counts {
		if c.window != window {
			delete(m.counts, key)
		}
	}
}

// sweepExpired drops every count from a window that has ended, for the
// cooldown sweep.
func (m *captchaMeter) sweepExpired() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweep(m.now().UnixNano() / int64(m.window))
}

/* HTTP ------------------------------------------------------------------------ */

func (a *App) captchaEnabled() bool { return a.captcha != nil }

// handleCaptcha issues a challenge. The client asks for one only on something a
// reader does in a posting form or the code request form, moving into a box to
// write in most often, never when a page is drawn.
func (a *App) handleCaptcha(w http.ResponseWriter, r *http.Request) {
	if !a.captchaEnabled() {
		writeError(w, http.StatusNotFound, "the captcha is not switched on here")
		return
	}
	scale, wait, ok := a.captchaMeter.take(captchaMeterKey(clientIP(r, a.cfg.TrustedProxyHops)))
	if !ok {
		seconds := int(math.Ceil(wait.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":       "too many captchas, wait " + strconv.Itoa(seconds) + "s",
			"retry_after": seconds,
		})
		return
	}
	challenge, err := a.captcha.issue(time.Now(), scale)
	if err != nil {
		a.fail(w, r, err, "could not make a captcha")
		return
	}
	writeJSON(w, http.StatusOK, challenge)
}

// checkCaptcha holds a request to the captcha, when it is on and the request is
// not exempt, and answers the ticket to spend once flood control's first check
// has let the request through. A nil ticket with ok true means there is nothing
// to spend.
//
// It refuses every answer it can before flood control: a missing, malformed,
// wrong or expired one, and one already spent, so a captcha refusal costs no
// cooldown. Looking up a spent one here, rather than leaving it to the spend,
// tells a replay "used" at once, even while a cooldown runs, so the page
// solves a fresh puzzle instead of waiting out a cooldown for an answer that
// can never work. It writes nothing, so a request refused afterwards, by flood
// control or anything else before the spend, leaves its answer for the retry.
func (a *App) checkCaptcha(w http.ResponseWriter, r *http.Request, payload string, exempt bool) (*captchaTicket, bool) {
	if !a.captchaEnabled() || exempt {
		return nil, true
	}
	if payload == "" {
		refuseCaptcha(w, captchaMissing, "solve the captcha first")
		return nil, false
	}
	ticket, err := a.captcha.check(payload, time.Now())
	switch {
	case errors.Is(err, errCaptchaExpired):
		refuseCaptcha(w, captchaExpired, "the captcha ran out, solve a new one")
		return nil, false
	case err != nil:
		refuseCaptcha(w, captchaInvalid, "the captcha answer is not valid, solve a new one")
		return nil, false
	}
	spent, err := a.store.CaptchaSpent(r.Context(), ticket.key)
	if err != nil {
		a.fail(w, r, err, "could not check the captcha")
		return nil, false
	}
	if spent {
		refuseCaptchaUsed(w)
		return nil, false
	}
	return &ticket, true
}

// captchaSpentHeader is set, to "spent", on every response to a request whose
// answer has been spent, a refusal written later included: a cooldown another
// request from the same address started meanwhile, an image that cannot be
// read, or a thread that filled up while the post was on its way.
// The client keeps its answer through a refusal without it and solves the
// next one as soon as a refusal with it arrives.
const captchaSpentHeader = "Educhan-Captcha"

// spendCaptcha marks a checked answer used, refusing a second use of it. It
// comes once flood control's first check has found nothing to wait out and
// before the request's cooldowns start, so a request refused here starts no
// cooldown, and nothing costly or lasting has been done for it yet. A request
// flood control then refuses on its second check, because another from the
// same address started a cooldown in between, has spent its answer, and the
// header says so.
func (a *App) spendCaptcha(w http.ResponseWriter, r *http.Request, ticket *captchaTicket) bool {
	if ticket == nil {
		return true
	}
	fresh, err := a.store.SpendCaptcha(r.Context(), ticket.key, ticket.expires, time.Now())
	if err != nil {
		a.fail(w, r, err, "could not check the captcha")
		return false
	}
	// Another request spent the same answer between this one's check and
	// now: a replay racing its original, the one refusal as used that comes
	// after flood control's check. Its cooldowns have not started, so the
	// replay costs nobody a wait.
	if !fresh {
		refuseCaptchaUsed(w)
		return false
	}
	w.Header().Set(captchaSpentHeader, "spent")
	return true
}

// refuseCaptcha answers a request the captcha holds back, with the reason
// beside the message for the client to act on.
func refuseCaptcha(w http.ResponseWriter, reason, msg string) {
	writeJSON(w, http.StatusForbidden, map[string]any{"error": msg, "captcha": reason})
}

func refuseCaptchaUsed(w http.ResponseWriter) {
	refuseCaptcha(w, captchaUsed, "that captcha answer was already used, solve a new one")
}
