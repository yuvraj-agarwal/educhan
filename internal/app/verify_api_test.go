package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/yuvraj-agarwal/educhan/internal/mailer"
)

/* Harness ------------------------------------------------------------------- */

// captureSender stands in for a mail provider, keeping what would have been
// sent so a test can read the code back the way a user reads their inbox.
type captureSender struct {
	mu   sync.Mutex
	sent []mailer.Code
}

func (s *captureSender) SendCode(_ context.Context, code mailer.Code) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, code)
	return nil
}

func (s *captureSender) last(t *testing.T) mailer.Code {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		t.Fatal("no verification code was sent")
	}
	return s.sent[len(s.sent)-1]
}

func (s *captureSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// verifyHarness is a running app with verification switched on and its mail
// captured rather than sent.
type verifyHarness struct {
	*harness
	mail *captureSender
}

func verifyServer(t *testing.T, tune func(*Config)) *verifyHarness {
	t.Helper()
	return verifyServerLogging(t, tune, io.Discard)
}

// verifyServerLogging is verifyServer with the application's log written to
// logs as JSON, for tests that assert on what verification writes down.
func verifyServerLogging(t *testing.T, tune func(*Config), logs io.Writer) *verifyHarness {
	t.Helper()
	mail := &captureSender{}
	h := testServerLogging(t, func(cfg *Config) {
		cfg.VerifySecret = "test-verification-secret"
		cfg.VerifyCodeTTL = 15 * time.Minute
		cfg.VerifyCredentialTTL = 180 * 24 * time.Hour
		cfg.Sender = mail
		if tune != nil {
			tune(cfg)
		}
	}, logs)
	return &verifyHarness{harness: h, mail: mail}
}

type verifyResponse struct {
	Enabled    bool   `json:"enabled"`
	Verified   bool   `json:"verified"`
	Status     string `json:"status"`
	Label      string `json:"label"`
	Credential string `json:"credential"`
	ExpiresIn  int    `json:"expires_in"`
	Error      string `json:"error"`
}

// requestCode asks for a code the way the page does once its box is ticked,
// with the 18+ attestation alongside the address.
// TestVerificationRequiresTheAdultAttestation covers the requests without it.
func (h *verifyHarness) requestCode(email string) (int, verifyResponse) {
	h.t.Helper()
	var out verifyResponse
	return h.post("/api/verify/request", map[string]any{"email": email, "adult": true}, &out), out
}

func (h *verifyHarness) confirmCode(code string) (int, verifyResponse) {
	h.t.Helper()
	var out verifyResponse
	return h.post("/api/verify/confirm", map[string]any{"code": code}, &out), out
}

func (h *verifyHarness) status() verifyResponse {
	h.t.Helper()
	var out verifyResponse
	h.get("/api/verify", &out)
	return out
}

// verify runs the whole flow and returns the credential it earned, failing the
// test at the first step that does not go through. Unlike a browser, nothing
// here keeps it: a caller that wants a badge on a post presents it explicitly,
// which is the point of the transport.
func (h *verifyHarness) verify(email string) verifyResponse {
	h.t.Helper()
	if status, out := h.requestCode(email); status != http.StatusAccepted {
		h.t.Fatalf("request code for %s: status %d (%s)", email, status, out.Error)
	}
	status, out := h.confirmCode(h.mail.last(h.t).Code)
	if status != http.StatusOK {
		h.t.Fatalf("confirm code for %s: status %d (%s)", email, status, out.Error)
	}
	if out.Credential == "" {
		h.t.Fatalf("confirming %s returned no credential", email)
	}
	return out
}

// postFrom sends a JSON POST that appears to arrive through one proxy from ip,
// which is how two clients are told apart in a test that shares a loopback.
func (c *client) postFrom(ip, path string, body any, out any) int {
	c.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		c.t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(encoded))
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	return c.send(req, out)
}

// postWithCredential sends a POST presenting a credential in the header the
// server reads it from, which is what a client does when — and only when — the
// poster ticked the badge box.
func (c *client) postWithCredential(credential, path string, body any, out any) int {
	c.t.Helper()
	return c.postWithHeader(credentialHeader, credential, path, body, out)
}

// postWithHeader is postWithCredential's general form, used to present the
// credential to endpoints that have no business reading it.
func (c *client) postWithHeader(header, value, path string, body any, out any) int {
	c.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		c.t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(encoded))
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if value != "" {
		req.Header.Set(header, value)
	}
	return c.send(req, out)
}

// getWithCredential is the same for a read, so a test can check that a handler
// handed the credential simply ignores it.
func (c *client) getWithCredential(credential, path string, out any) int {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	if credential != "" {
		req.Header.Set(credentialHeader, credential)
	}
	return c.send(req, out)
}

/* Tests -------------------------------------------------------------------- */

func TestVerificationHappyPath(t *testing.T) {
	h := verifyServer(t, nil)

	if before := h.status(); !before.Enabled {
		t.Fatalf("before verifying: %+v, want the feature reported as enabled", before)
	}

	status, out := h.requestCode("Ana@iitb.ac.in")
	if status != http.StatusAccepted {
		t.Fatalf("request code: status %d (%s)", status, out.Error)
	}
	if out.Label != "IIT Bombay" {
		t.Errorf("label = %q, want IIT Bombay", out.Label)
	}
	if out.ExpiresIn != int((15 * time.Minute).Seconds()) {
		t.Errorf("expires_in = %d, want 900", out.ExpiresIn)
	}

	sent := h.mail.last(t)
	if sent.Address != "ana@iitb.ac.in" {
		t.Errorf("code went to %q, want the submitted address", sent.Address)
	}
	if len(sent.Code) != verifyCodeDigits {
		t.Errorf("code %q is not %d digits", sent.Code, verifyCodeDigits)
	}

	status, out = h.confirmCode(sent.Code)
	if status != http.StatusOK {
		t.Fatalf("confirm: status %d (%s)", status, out.Error)
	}
	if !out.Verified || out.Label != "IIT Bombay" {
		t.Errorf("confirm returned %+v, want verified as IIT Bombay", out)
	}

	// The credential comes back in the body, and the client decides when it
	// travels. It must not also be a cookie: a cookie would be attached to
	// every later request, badge or not.
	if out.Credential == "" {
		t.Fatal("confirming should return the credential in the response")
	}
	if _, ok := h.app.parseCredential(out.Credential); !ok {
		t.Errorf("the returned credential %q does not verify", out.Credential)
	}
	for _, raw := range h.lastHeader.Values("Set-Cookie") {
		name, _, _ := strings.Cut(raw, "=")
		if name != pendingCookieName {
			t.Errorf("confirming set an unexpected cookie %q; the credential is not a cookie", raw)
		}
	}
	// A response carrying a credential must not be cached on the way.
	if got := h.lastHeader.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	// The status endpoint answers whether the feature exists and nothing about
	// the caller, so presenting the credential to it changes nothing.
	var after verifyResponse
	if code := h.getWithCredential(out.Credential, "/api/verify", &after); code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if !after.Enabled || after.Verified || after.Label != "" {
		t.Errorf("status = %+v, want only the enabled flag", after)
	}

	// The pending verification is spent, not lying around.
	if n := h.countRows("verification_pending"); n != 0 {
		t.Errorf("%d pending rows survived a successful verification, want 0", n)
	}
	if n := h.countRows("verification_credentials"); n != 1 {
		t.Errorf("%d credential rows, want exactly 1", n)
	}
}

func TestVerificationRejectsAWrongCode(t *testing.T) {
	h := verifyServer(t, nil)

	if status, out := h.requestCode("ana@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("request code: status %d (%s)", status, out.Error)
	}
	sent := h.mail.last(t)

	wrong := "000000"
	if wrong == sent.Code {
		wrong = "111111"
	}
	status, out := h.confirmCode(wrong)
	if status != http.StatusBadRequest {
		t.Fatalf("wrong code: status %d, want 400", status)
	}
	if !strings.Contains(out.Error, "not right") {
		t.Errorf("error = %q, want it to say the code is wrong", out.Error)
	}
	if out.Credential != "" {
		t.Error("a wrong code must not hand out a credential")
	}
	if n := h.countRows("verification_credentials"); n != 0 {
		t.Errorf("a wrong code issued %d credentials, want 0", n)
	}

	// The verification survives a typo: the right code still works.
	if status, out := h.confirmCode(sent.Code); status != http.StatusOK {
		t.Fatalf("correct code after a typo: status %d (%s)", status, out.Error)
	}
}

// A short code is only safe while guesses are counted, so the count is what
// this asserts: five wrong answers and the verification is destroyed.
func TestVerificationCapsWrongGuesses(t *testing.T) {
	h := verifyServer(t, nil)

	if status, _ := h.requestCode("ana@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatal("request code failed")
	}
	sent := h.mail.last(t)

	for i := 0; i < maxVerifyAttempts; i++ {
		wrong := fmt.Sprintf("%0*d", verifyCodeDigits, i)
		if wrong == sent.Code {
			continue
		}
		if status, _ := h.confirmCode(wrong); status != http.StatusBadRequest {
			t.Fatalf("guess %d: status %d, want 400", i, status)
		}
	}

	// Even the right code is refused now, and the pending row is gone with it.
	status, out := h.confirmCode(sent.Code)
	if status != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong guesses: status %d, want 429", maxVerifyAttempts, status)
	}
	if !strings.Contains(out.Error, "too many") {
		t.Errorf("error = %q, want it to name the attempt limit", out.Error)
	}
	if n := h.countRows("verification_pending"); n != 0 {
		t.Errorf("%d pending rows survived the attempt cap, want 0", n)
	}
}

func TestVerificationRejectsAnExpiredCode(t *testing.T) {
	h := verifyServer(t, nil)

	if status, _ := h.requestCode("ana@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatal("request code failed")
	}
	sent := h.mail.last(t)
	h.exec(`UPDATE verification_pending SET expires_at = now() - interval '1 minute'`)

	status, out := h.confirmCode(sent.Code)
	if status != http.StatusBadRequest {
		t.Fatalf("expired code: status %d, want 400", status)
	}
	if !strings.Contains(out.Error, "expired") {
		t.Errorf("error = %q, want it to say the code expired", out.Error)
	}
	if out.Credential != "" {
		t.Error("an expired code must not hand out a credential")
	}
	if n := h.countRows("verification_credentials"); n != 0 {
		t.Errorf("an expired code issued %d credentials, want 0", n)
	}
	if n := h.countRows("verification_pending"); n != 0 {
		t.Errorf("%d expired pending rows survived, want 0", n)
	}
}

func TestVerificationRejectsNonAcademicDomains(t *testing.T) {
	h := verifyServer(t, nil)

	cases := []string{
		"ana@gmail.com",
		"ana@iitb.ac.in.example.com",
		// A namespace is not an institution.
		"ana@ac.in",
		"not-an-address",
	}
	for _, email := range cases {
		t.Run(email, func(t *testing.T) {
			status, out := h.requestCode(email)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			if out.Error == "" {
				t.Error("expected an error explaining the refusal")
			}
		})
	}
	if h.mail.count() != 0 {
		t.Errorf("%d codes were sent to unrecognised domains, want 0", h.mail.count())
	}
	if n := h.countRows("verification_pending"); n != 0 {
		t.Errorf("a refused domain wrote %d rows, want 0", n)
	}
}

// The badge is for students aged 18 and over, on their own word (build plan
// 2.14). A code request that does not say so is turned away before anything
// else happens to it: no code is mailed, no row is written, and neither
// cooldown starts, so asking again properly is not held up by having asked
// without it.
func TestVerificationRequiresTheAdultAttestation(t *testing.T) {
	h := verifyServer(t, func(cfg *Config) {
		cfg.VerifyCooldown = defaultVerifyCooldown
		cfg.TrustedProxyHops = 1
	})
	const client, email = "203.0.113.9", "ana@iitb.ac.in"

	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"missing", map[string]any{"email": email}, adultRequired},
		{"false", map[string]any{"email": email, "adult": false}, adultRequired},
		{"null", map[string]any{"email": email, "adult": nil}, adultRequired},
		// Not a boolean at all, which the decoder refuses before the handler
		// gets to look.
		{"a string", map[string]any{"email": email, "adult": "yes"}, "invalid JSON body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out verifyResponse
			if status := h.postFrom(client, "/api/verify/request", tc.body, &out); status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			if out.Error != tc.want {
				t.Errorf("error = %q, want %q", out.Error, tc.want)
			}
		})
	}
	if n := h.mail.count(); n != 0 {
		t.Errorf("%d codes were sent without the attestation, want 0", n)
	}
	if n := h.countRows("verification_pending"); n != 0 {
		t.Errorf("requests without the attestation wrote %d pending rows, want 0", n)
	}

	// The same client and the same address, with the cooldown on: had any
	// refusal above started either cooldown, this would be a 429.
	var out verifyResponse
	if status := h.postFrom(client, "/api/verify/request",
		map[string]any{"email": email, "adult": true}, &out); status != http.StatusAccepted {
		t.Fatalf("with the attestation: status %d (%s), want 202", status, out.Error)
	}
	if n := h.mail.count(); n != 1 {
		t.Fatalf("%d codes sent, want 1", n)
	}
	// And the cooldown really is on, so the request above proves something.
	if status := h.postFrom(client, "/api/verify/request",
		map[string]any{"email": email, "adult": true}, &out); status != http.StatusTooManyRequests {
		t.Errorf("asking again at once: status %d, want 429", status)
	}

	// The rest of the flow is untouched by the attestation.
	status, confirmed := h.confirmCode(h.mail.last(t).Code)
	if status != http.StatusOK || confirmed.Credential == "" {
		t.Fatalf("confirm: status %d (%s), want a credential", status, confirmed.Error)
	}
}

// The attestation is checked at the door and dropped there. Nothing educhan
// keeps (a row, a log line, the credential) records the answer, so there is
// none to leak, to hand over, or to line up against a post
// (design-recommendations 3.3, 4.4).
func TestAdultAttestationIsNeverKept(t *testing.T) {
	var logs syncBuffer
	h := verifyServerLogging(t, nil, &logs)

	// Both outcomes, so either has had the chance to leave something behind,
	// and a verification left in flight so the pending table holds a row.
	var refused verifyResponse
	if status := h.post("/api/verify/request",
		map[string]any{"email": "bo@iitb.ac.in", "adult": false}, &refused); status != http.StatusBadRequest {
		t.Fatalf("without the attestation: status %d, want 400", status)
	}
	issued := h.verify("ana@iitb.ac.in")
	if status, out := h.requestCode("cy@iitb.ac.in"); status != http.StatusAccepted {
		t.Fatalf("second request: status %d (%s)", status, out.Error)
	}

	// The credential carries the three claims it always has, and no fourth.
	parts := strings.Split(issued.Credential, ".")
	if len(parts) != 3 {
		t.Fatalf("credential %q is not version.claims.mac", issued.Credential)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode the claims: %v", err)
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("decode the claims: %v", err)
	}
	var names []string
	for name := range claims {
		names = append(names, name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "exp,id,label" {
		t.Errorf("credential claims = %s, want exactly exp,id,label", got)
	}

	// The log, once it is shown to hold the handlers' own lines.
	written := logs.String()
	if !strings.Contains(written, `"msg":"verification issued"`) {
		t.Fatalf("the log has no verification lines, so it cannot show what is missing:\n%s", written)
	}
	if strings.Contains(strings.ToLower(written), "adult") {
		t.Errorf("the log mentions the attestation:\n%s", written)
	}

	// The database. A column for it would hold a boolean, which no text scan
	// can find, so the column names are what is checked here; every table's
	// columns are also pinned exactly by TestEveryTableHasItsColumnsPinned and
	// the two tests beside it.
	ageShaped := regexp.MustCompile(`adult|birth|(^|_)age($|_)`)
	for _, table := range h.tables() {
		for _, column := range h.columns(table) {
			if ageShaped.MatchString(column) {
				t.Errorf("%s.%s looks like a record of age", table, column)
			}
		}
	}
	if n := h.rowsContaining("verification_pending", []byte("IIT Bombay")); n == 0 {
		t.Fatal("the scan found no label in verification_pending, so it cannot be trusted")
	}
	for _, table := range h.tables() {
		if n := h.rowsContaining(table, []byte("adult")); n != 0 {
			t.Errorf("%d rows of %s mention the attestation", n, table)
		}
	}
}

// The stored row dates an issuance only to the day. The log notes that a
// credential was issued, as the request log notes the request, but never for
// which institution: a label stamped to the second would date the row exactly
// on a day it was its institution's only one.
func TestIssuanceIsLoggedWithoutTheInstitution(t *testing.T) {
	var logs syncBuffer
	h := verifyServerLogging(t, nil, &logs)
	issued := h.verify("ana@iitb.ac.in")
	if issued.Label != "IIT Bombay" {
		t.Fatalf("label = %q, want IIT Bombay, or the scan below proves nothing", issued.Label)
	}

	written := logs.String()
	if !strings.Contains(written, `"msg":"verification issued"`) {
		t.Fatalf("the issuance is not in the log at all:\n%s", written)
	}
	for _, trace := range []string{"IIT Bombay", "iitb", "institution", "label"} {
		if strings.Contains(written, trace) {
			t.Errorf("the log carries %q:\n%s", trace, written)
		}
	}
}

// One address holds one badge. Verifying again is how a badge is renewed or
// moved to another browser, and it takes the previous credential with it.
func TestReverificationRevokesThePreviousCredential(t *testing.T) {
	h := verifyServer(t, nil)
	h.addBoard("general")

	first := h.verify("ana@iitb.ac.in").Credential

	// Same mailbox, written the way somebody would on a second device.
	second := h.verify("Ana+phone@iitb.ac.in").Credential

	if first == second {
		t.Fatal("re-verifying should issue a different credential")
	}
	if n := h.countRows("verification_credentials"); n != 1 {
		t.Errorf("%d credential rows for one address, want 1", n)
	}

	thread := h.createThread("general", "badges", "", "opening post")
	path := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)

	var out createdPost
	if status := h.postWithCredential(first, path,
		map[string]any{"body": "with the old credential", "badge": true}, &out); status != http.StatusForbidden {
		t.Fatalf("old credential: status %d, want 403", status)
	}
	if !strings.Contains(out.Error, "no longer valid") {
		t.Errorf("error = %q, want it to say the credential is superseded", out.Error)
	}
	if out.Credential != credentialRevoked {
		t.Errorf("refusal reason = %q, want %q so a client can drop what it holds",
			out.Credential, credentialRevoked)
	}

	if status := h.postWithCredential(second, path,
		map[string]any{"body": "with the new credential", "badge": true}, &out); status != http.StatusCreated {
		t.Fatalf("new credential: status %d (%s), want 201", status, out.Error)
	}
}

// An expired credential is refused on its own claims, before the database is
// consulted, and says so in a way that tells the holder what to do about it.
func TestExpiredCredentialStopsBadging(t *testing.T) {
	h := verifyServer(t, func(cfg *Config) { cfg.VerifyCredentialTTL = 40 * time.Millisecond })
	h.addBoard("general")

	credential := h.verify("ana@iitb.ac.in").Credential
	time.Sleep(60 * time.Millisecond)

	var out createdPost
	status := h.postWithCredential(credential, "/api/boards/general/threads",
		map[string]any{"body": "too late", "badge": true}, &out)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(out.Error, "expired") {
		t.Errorf("error = %q, want it to say the verification expired", out.Error)
	}
	if out.Credential != credentialExpired {
		t.Errorf("refusal reason = %q, want %q", out.Credential, credentialExpired)
	}
}

// Code requests are limited on both keys at once: one client cannot walk a list
// of addresses, and one address cannot be mailed from a list of clients.
func TestVerificationRateLimits(t *testing.T) {
	h := verifyServer(t, func(cfg *Config) {
		cfg.VerifyCooldown = time.Minute
		cfg.TrustedProxyHops = 1
	})

	var out verifyResponse
	if status := h.postFrom("203.0.113.1", "/api/verify/request",
		map[string]any{"email": "ana@iitb.ac.in", "adult": true}, &out); status != http.StatusAccepted {
		t.Fatalf("first request: status %d (%s)", status, out.Error)
	}

	// Same client, a different address: refused on the client's cooldown.
	if status := h.postFrom("203.0.113.1", "/api/verify/request",
		map[string]any{"email": "bo@iitb.ac.in", "adult": true}, &out); status != http.StatusTooManyRequests {
		t.Errorf("second address from the same client: status %d, want 429", status)
	}
	if got := h.lastHeader.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
	if !strings.Contains(out.Error, "requesting codes") {
		t.Errorf("error = %q, want it to name what was refused", out.Error)
	}

	// Different client, the same address: refused on the address's cooldown, so
	// a mailbox cannot be flooded from a pool of hosts.
	if status := h.postFrom("203.0.113.2", "/api/verify/request",
		map[string]any{"email": "ana@iitb.ac.in", "adult": true}, &out); status != http.StatusTooManyRequests {
		t.Errorf("same address from another client: status %d, want 429", status)
	}

	// Different client, different address: unaffected.
	if status := h.postFrom("203.0.113.2", "/api/verify/request",
		map[string]any{"email": "cy@iitb.ac.in", "adult": true}, &out); status != http.StatusAccepted {
		t.Errorf("unrelated request: status %d (%s), want 202", status, out.Error)
	}
	if h.mail.count() != 2 {
		t.Errorf("%d codes sent, want 2: a refused request must not send one", h.mail.count())
	}
}

func TestPostingWithAndWithoutABadge(t *testing.T) {
	h := verifyServer(t, nil)
	h.addBoard("general")

	// Without a credential, asking for a badge is refused rather than quietly
	// dropped: being told now beats finding out from the posted thread.
	var refused createdPost
	if status := h.post("/api/boards/general/threads",
		map[string]any{"body": "not verified", "badge": true}, &refused); status != http.StatusForbidden {
		t.Fatalf("badge without a credential: status %d, want 403", status)
	}
	if !strings.Contains(refused.Error, "verify") {
		t.Errorf("error = %q, want it to point at verification", refused.Error)
	}
	if refused.Credential != credentialMissing {
		t.Errorf("refusal reason = %q, want %q", refused.Credential, credentialMissing)
	}

	issued := h.verify("ana@iitb.ac.in")
	if issued.Label != "IIT Bombay" {
		t.Fatalf("verified as %q, want IIT Bombay", issued.Label)
	}

	var created createdPost
	if status := h.postWithCredential(issued.Credential, "/api/boards/general/threads",
		map[string]any{"subject": "badges", "body": "posting with a badge", "badge": true},
		&created); status != http.StatusCreated {
		t.Fatalf("badged thread: status %d (%s)", status, created.Error)
	}
	// The same poster, holding the same credential, posting without it: no
	// badge asked for means no badge sent, which is the whole design.
	if status := h.post(fmt.Sprintf("/api/boards/general/threads/%d/posts", created.ThreadNo),
		map[string]any{"body": "posting anonymously"}, &createdPost{}); status != http.StatusCreated {
		t.Fatalf("unbadged reply: status %d", status)
	}
	if status := h.postWithCredential(issued.Credential,
		fmt.Sprintf("/api/boards/general/threads/%d/posts", created.ThreadNo),
		map[string]any{"body": "and with it again", "badge": true}, &createdPost{}); status != http.StatusCreated {
		t.Fatalf("badged reply: status %d", status)
	}

	detail := fetchThread(t, h.harness, "general", created.ThreadNo)
	if len(detail.Posts) != 3 {
		t.Fatalf("thread has %d posts, want 3", len(detail.Posts))
	}
	op, plain, badged := detail.Posts[0], detail.Posts[1], detail.Posts[2]
	if op.Badge != "IIT Bombay" {
		t.Errorf("op badge = %q, want IIT Bombay", op.Badge)
	}
	if plain.Badge != "" {
		t.Errorf("unbadged reply carries %q, want no badge: the checkbox is per post", plain.Badge)
	}
	if badged.Badge != "IIT Bombay" {
		t.Errorf("badged reply = %q, want IIT Bombay", badged.Badge)
	}
	// The unbadged reply came from the same client, and must not share the
	// badged posts' ID: if it did, the ID would tie it to the institution. The
	// two badged posts share one, derived from the credential, as any poster's
	// posts in one thread do.
	if op.PosterID == plain.PosterID {
		t.Errorf("a badged and an unbadged post from one client share the poster ID %q", op.PosterID)
	}
	if op.PosterID != badged.PosterID {
		t.Errorf("one credential in one thread got poster IDs %q and %q, want one", op.PosterID, badged.PosterID)
	}

	// The index builds its posts from a different query, so it needs asserting
	// separately or a column added in one place goes missing in the other.
	index := fetchIndex(t, h.harness, "general", 1)
	if len(index.Threads) != 1 {
		t.Fatalf("index has %d threads, want 1", len(index.Threads))
	}
	summary := index.Threads[0]
	if summary.OP.Badge != "IIT Bombay" {
		t.Errorf("index op badge = %q, want IIT Bombay", summary.OP.Badge)
	}
	var badgesInReplies []string
	for _, reply := range summary.LastReplies {
		badgesInReplies = append(badgesInReplies, reply.Badge)
	}
	if len(badgesInReplies) != 2 || badgesInReplies[0] != "" || badgesInReplies[1] != "IIT Bombay" {
		t.Errorf("index reply badges = %q, want [\"\", \"IIT Bombay\"]", badgesInReplies)
	}
}

func TestVerificationIsOffWithoutASecret(t *testing.T) {
	c := testServer(t, nil)
	c.addBoard("general")

	var status verifyResponse
	c.get("/api/verify", &status)
	if status.Enabled {
		t.Errorf("status = %+v, want the feature reported as disabled", status)
	}

	var out verifyResponse
	if code := c.post("/api/verify/request", map[string]any{"email": "ana@iitb.ac.in", "adult": true}, &out); code != http.StatusForbidden {
		t.Errorf("request without a secret: status %d, want 403", code)
	}
	if code := c.post("/api/verify/confirm", map[string]any{"code": "123456"}, &out); code != http.StatusForbidden {
		t.Errorf("confirm without a secret: status %d, want 403", code)
	}

	var created createdPost
	if code := c.post("/api/boards/general/threads",
		map[string]any{"body": "badge me", "badge": true}, &created); code != http.StatusForbidden {
		t.Errorf("badged post without a secret: status %d, want 403", code)
	}
	// Ordinary posting is untouched by the feature being off.
	c.createThread("general", "", "", "still works")
}

// The privacy claim in the README, as an assertion: run the whole feature, then
// look for the address in every column of every table.
func TestNoEmailAddressReachesTheDatabase(t *testing.T) {
	h := verifyServer(t, nil)
	h.addBoard("general")

	// iitb.ac.in maps to a named institution, so even the domain has no reason
	// to appear. For a domain matched by suffix alone the badge *is* the
	// domain, which is the intended behaviour and why the local part and the
	// whole address are the parts this test treats as absolute.
	const (
		local  = "no-address-in-the-database"
		domain = "iitb.ac.in"
		email  = local + "@" + domain
	)

	issued := h.verify(email)

	// A second verification left in flight, so the pending table is populated
	// too rather than only the credential table.
	if status, out := h.requestCode("pending-" + email); status != http.StatusAccepted {
		t.Fatalf("second request: status %d (%s)", status, out.Error)
	}

	var created createdPost
	if status := h.postWithCredential(issued.Credential, "/api/boards/general/threads",
		map[string]any{"subject": "badged", "body": "posting with a badge", "badge": true},
		&created); status != http.StatusCreated {
		t.Fatalf("badged thread: status %d (%s)", status, created.Error)
	}
	h.reply("general", created.ThreadNo, "", "and an ordinary reply", false)

	// Positive controls first: a scan that finds nothing is only worth
	// something once it has been shown to find what is genuinely there. The
	// badge exercises the text columns and the stored key the bytea ones,
	// which render as \x… and would otherwise slip past a plain search.
	storedKey := h.app.emailHash(mustParseEmail(t, email).Key)
	if n := h.rowsContaining("posts", []byte("IIT Bombay")); n == 0 {
		t.Fatal("the scan found no badge in posts, so it cannot be trusted to find an address")
	}
	if n := h.rowsContaining("verification_credentials", storedKey); n == 0 {
		t.Fatal("the scan missed a bytea column, so it cannot be trusted to find an address in one")
	}

	for _, needle := range []string{email, local, domain} {
		for _, table := range h.tables() {
			if n := h.rowsContaining(table, []byte(needle)); n != 0 {
				t.Errorf("%d rows of %s contain %q; the address must never be written", n, table, needle)
			}
		}
	}

	// What is stored instead: the keyed hash, and nothing that reverses.
	var stored []byte
	if err := h.app.pool.QueryRow(context.Background(),
		`SELECT email_hash FROM verification_credentials`).Scan(&stored); err != nil {
		t.Fatalf("read the credential row: %v", err)
	}
	if !bytes.Equal(stored, storedKey) {
		t.Error("the stored key should be HMAC(secret, normalised address)")
	}
}

// The tables are arranged so that holding all of them still does not say who
// posted what. A foreign key, or a column that could be joined on, would undo
// that quietly — so the shape itself is asserted.
func TestVerificationTablesCannotBeJoinedToPosts(t *testing.T) {
	h := verifyServer(t, nil)

	want := map[string][]string{
		"verification_pending":     {"attempts", "code_hash", "email_hash", "expires_at", "label", "token_hash"},
		"verification_credentials": {"credential_id_hash", "email_hash", "expires_at", "label"},
	}
	for table, columns := range want {
		got := h.columns(table)
		if strings.Join(got, ",") != strings.Join(columns, ",") {
			t.Errorf("%s has columns %v, want exactly %v", table, got, columns)
		}
		if n := h.foreignKeys(table); n != 0 {
			t.Errorf("%s has %d foreign keys, want none", table, n)
		}
	}

	// And nothing points the other way either: a post carries a badge as text,
	// never a reference to the credential it came from.
	for _, column := range h.columns("posts") {
		if strings.Contains(column, "verif") || strings.Contains(column, "credential") {
			t.Errorf("posts.%s links a post to a verification record", column)
		}
	}
}

// TestPostsTableColumnsArePinned holds the posts table to the columns it has
// today, so every future column is a decision somebody made on purpose rather
// than a migration that slipped through. A post is the record every reader can
// see the public half of, and anything stored beside it is stored about every
// poster. In particular nothing derived from a poster key, a credential, an
// address, or an email belongs here unless a change says why and a test pins
// what it can be used for; the ban handles planned for later are exactly such
// a column, and adding one should mean editing this list in the same change.
func TestPostsTableColumnsArePinned(t *testing.T) {
	h := testServer(t, nil)

	want := []string{
		"badge",      // the institution label, as text; never a reference to a credential
		"board_id",   // which board's numbering the post belongs to
		"body",       // the post text
		"created_at", // shown on the post
		// held_at and held_reason: when the content filter kept the post back
		// for a moderator, and which rule's name — never what matched, which
		// the post's own body already holds. Both are about the post, not the
		// poster, and are cleared when the post is approved.
		"held_at",
		"held_reason",
		"id",        // internal primary key
		"is_op",     // the opening post of its thread
		"name",      // display name, "Anonymous" by default
		"no",        // the per-board number posters quote
		"poster_id", // the per-thread ID: a keyed hash, never the input it came from
		"thread_id", // the thread the post is in
		"tripcode",  // the salted tripcode, if the poster used one; never the secret
	}
	if got := h.columns("posts"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("posts has columns %v, want exactly %v\n"+
			"a new column on posts is stored about every poster: add it here deliberately, "+
			"with what it holds and why it cannot identify anyone", got, want)
	}
}

// The other tables are pinned the same way, because a thread is one row per
// opening post and an attachment one row per image post: a column on either is
// stored about a poster just as much as one on posts. A hash of a poster key
// on attachments, or a poster's source on threads "for moderation", would be
// a handle on one browser across every thread, and nothing else would notice
// it arriving. Every table in the schema is listed here or in one of the two
// tests above, so a new table is a decision too.
func TestEveryTableHasItsColumnsPinned(t *testing.T) {
	h := testServer(t, nil)

	want := map[string][]string{
		"threads": {
			"archived_at",  // when it left the board; NULL while live
			"board_id",     // the board it is on
			"bumped_at",    // the bump order every listing uses
			"created_at",   // shown with the opening post
			"expired_at",   // its archive period ran out while a held reply kept it from the sweep
			"held_at",      // mirrors the opening post's held_at: the whole thread waits for a moderator
			"id",           // internal primary key
			"image_count",  // denormalised count of posts with an image
			"last_post_at", // shown in listings
			"locked",       // moderator flag
			"no",           // the opening post's number, the thread's address
			"reply_count",  // denormalised count of replies
			"sticky",       // moderator flag
			"subject",      // the opening post's subject
		},
		"attachments": {
			"byte_size",          // shown on the post
			"checksum",           // SHA-256 of the stored file, rewritten without metadata; nothing about the poster
			"content_type",       // of the original
			"created_at",         // when the file was stored, as the post was
			"height",             // of the original
			"id",                 // internal primary key
			"original_name",      // "image" and the stored format's extension (attachmentName); never the poster's filename
			"original_path",      // storage path: board, month, random name
			"post_id",            // the post it belongs to
			"spoiler",            // the poster asked for it hidden
			"thumb_content_type", // of the thumbnail
			"thumb_height",       // of the thumbnail
			"thumb_path",         // storage path of the thumbnail
			"thumb_width",        // of the thumbnail
			"width",              // of the original
		},
		"post_quotes":       {"from_post_id", "to_post_id"},    // a reply and what it quoted, both public in its text
		"media_deletions":   {"id", "key"},                     // storage keys the sweeper has still to remove
		"schema_migrations": {"applied_at", "name", "version"}, // which migrations ran
		// Board settings, nothing about posters.
		"boards": {"archive_days", "bump_limit", "created_at", "description", "id",
			"image_limit", "name", "next_post_no", "rules", "slug", "sort_order", "thread_limit"},
	}
	for table, columns := range want {
		if got := h.columns(table); strings.Join(got, ",") != strings.Join(columns, ",") {
			t.Errorf("%s has columns %v, want exactly %v\n"+
				"add a column here deliberately, with what it holds and why it cannot identify anyone", table, got, columns)
		}
	}

	// Pinned by TestPostsTableColumnsArePinned and
	// TestVerificationTablesCannotBeJoinedToPosts.
	for _, table := range []string{"posts", "verification_pending", "verification_credentials"} {
		want[table] = nil
	}
	for _, table := range h.tables() {
		if _, ok := want[table]; !ok {
			t.Errorf("table %s has no pinned column list; add one here", table)
		}
	}
}

/* Schema helpers ------------------------------------------------------------ */

func (h *harness) countRows(table string) int {
	h.t.Helper()
	var n int
	query := "SELECT count(*) FROM " + pgx.Identifier{table}.Sanitize()
	if err := h.app.pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		h.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// tables lists the base tables in the schema this test owns.
func (h *harness) tables() []string {
	h.t.Helper()
	rows, err := h.app.pool.Query(context.Background(), `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		h.t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			h.t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("list tables: %v", err)
	}
	if len(tables) == 0 {
		h.t.Fatal("no tables found; the scan would pass vacuously")
	}
	return tables
}

func (h *harness) columns(table string) []string {
	h.t.Helper()
	rows, err := h.app.pool.Query(context.Background(), `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1
		ORDER BY column_name`, table)
	if err != nil {
		h.t.Fatalf("list columns of %s: %v", table, err)
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			h.t.Fatalf("scan column name: %v", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("list columns of %s: %v", table, err)
	}
	return columns
}

func (h *harness) foreignKeys(table string) int {
	h.t.Helper()
	var n int
	if err := h.app.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.table_constraints
		WHERE table_schema = current_schema() AND table_name = $1
		  AND constraint_type = 'FOREIGN KEY'`, table).Scan(&n); err != nil {
		h.t.Fatalf("list foreign keys of %s: %v", table, err)
	}
	return n
}

// rowsContaining counts rows of a table holding needle anywhere in them.
// Casting the whole row to text covers every column in one pass; the hex form
// covers the bytea columns, which render as \x… rather than as their contents.
func (h *harness) rowsContaining(table string, needle []byte) int {
	h.t.Helper()

	patterns := []string{"%" + hex.EncodeToString(needle) + "%"}
	if utf8.Valid(needle) {
		patterns = append(patterns, "%"+string(needle)+"%")
	}

	var n int
	query := fmt.Sprintf(`SELECT count(*) FROM %s AS row WHERE row::text ILIKE ANY($1)`,
		pgx.Identifier{table}.Sanitize())
	if err := h.app.pool.QueryRow(context.Background(), query, patterns).Scan(&n); err != nil {
		h.t.Fatalf("scan %s for %q: %v", table, needle, err)
	}
	return n
}
