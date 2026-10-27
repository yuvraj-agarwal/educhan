package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/mailer"
)

// Verified Anonymity: a poster proves they hold an address at a real
// institution and may carry a badge for it, while the server stays unable to
// say which address wrote which post — or, without guessing the address, which
// address any record belongs to at all.
//
// Three rules hold that up, and every function in this file is written to keep
// them true:
//
//  1. The address is never stored. It exists in memory for the length of one
//     request, long enough to check its domain and send a code to it. What is
//     kept is HMAC(EDUCHAN_VERIFY_SECRET, normalised address), which exists
//     only to stop one mailbox minting unlimited identities.
//  2. The credential carries no trace of the address. It is a signed blob
//     holding a badge label, an expiry, and a random identifier — nothing that
//     can be reversed or correlated back to the mailbox that earned it.
//  3. Nothing links a credential to a post. The badge is copied onto the post
//     as text, the verification tables reference no post, and using a
//     credential updates no row, so there is no timestamp to line up against.
//     A badged post's poster ID is derived from the random id inside the
//     credential as presented, never from what the database keeps of it, so
//     it ties a credential's posts together within one thread — as any
//     poster's ID does — and can be recomputed only by someone holding the
//     credential itself as well as the salt.
//  4. The credential travels only when it is being used. It is handed to the
//     client once and presented in a header on the posts that ask for a badge,
//     so it is absent from every anonymous post, every page load, and every
//     asset fetch. A cookie would have been attached to all of them, which
//     would have made the badge a stable identifier across a poster's whole
//     session whether or not they ever used it.
//
// README.md states the property in full, including what an operator holding the
// whole database can still work out.

const (
	// credentialHeader carries the credential on the one kind of request that
	// needs it: a post asking for its badge. Nothing else reads it, and nothing
	// attaches it — that is the whole point of it being a header rather than a
	// cookie, and TestCredentialIsReadOnlyOnTheBadgePath holds the server to it.
	credentialHeader = "X-Educhan-Credential"
	// pendingCookieName holds the in-progress verification, so confirming a
	// code does not mean sending the address a second time. It is a cookie
	// because it is scoped to /api/verify and lives for one code's lifetime: it
	// goes to the verification endpoints and nowhere else.
	pendingCookieName = "educhan_verify_pending"
	pendingCookiePath = "/api/verify"

	// verifyCodeDigits is short enough to read out of an email and retype.
	// maxVerifyAttempts is what keeps it safe: five wrong guesses against a
	// one-in-a-million code, and the pending verification is destroyed.
	verifyCodeDigits  = 6
	maxVerifyAttempts = 5

	// credentialVersion prefixes an issued credential so the format can change
	// without a stored one being read as the new shape.
	credentialVersion = "v1"
)

// Machine-readable reasons a badge was refused, returned beside the message so
// a client can drop a credential the server will not honour again instead of
// offering it for another post.
const (
	credentialMissing = "missing"
	credentialExpired = "expired"
	credentialRevoked = "revoked"
)

// Why a request carries no usable badge. The posting path turns these into
// messages a poster can act on, so they distinguish "never verified" from
// "verified, but it has run out".
var (
	errNoCredential      = errors.New("not verified")
	errCredentialExpired = errors.New("verification expired")
	errCredentialRevoked = errors.New("verification superseded")
)

// credential is what a verified poster holds. It is signed by the server and
// opaque to everyone else, though not secret from its holder: the label and the
// expiry are both things they already know. The address is not among them, and
// neither is anything derived from it — id is random, not a hash of a mailbox.
type credential struct {
	Label   string `json:"label"`
	ID      string `json:"id"`
	Expires int64  `json:"exp"`
}

/* Handlers ----------------------------------------------------------------- */

func (a *App) verifyEnabled() bool { return a.cfg.VerifySecret != "" }

// requireVerify gates the endpoints that mint state. Without a secret there is
// nothing to sign a credential with, so the feature stays off, the way
// moderation does without a key.
func (a *App) requireVerify(w http.ResponseWriter) bool {
	if !a.verifyEnabled() {
		writeError(w, http.StatusForbidden, "verification is not configured")
		return false
	}
	return true
}

// handleVerifyStatus reports whether the feature is configured, and nothing
// else. It deliberately does not look at the caller's credential: answering
// "are you verified?" would mean the credential travelling on a request that is
// not posting a badge, which is exactly the habit this design gives up.
//
// The client holds its own credential and can read the label and expiry out of
// it without asking, so there is nothing here for it to learn. A credential the
// server has since revoked is discovered on the next post that uses it.
func (a *App) handleVerifyStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": a.verifyEnabled()})
}

// adultRequired refuses a code request that does not say its sender is 18 or
// over. It is worded for the person reading it, because the page shows it to
// them, and it says what they can still do: the limit is the badge's, not the
// site's.
const adultRequired = "the badge is for students aged 18 and over; you can still read and post anonymously"

type verifyRequestBody struct {
	Email string `json:"email"`
	// Adult is the sender's own word that they are 18 or over (build plan
	// 2.14). It is checked and then dropped: no column, no log field and no
	// credential claim holds it, so the answer is recorded nowhere.
	Adult bool `json:"adult"`
}

// handleVerifyRequest checks a submitted address against the allowlist and
// emails it a code. The address reaches a database column at no point in this
// handler: what is written is its HMAC, and what is sent is discarded with the
// request.
func (a *App) handleVerifyRequest(w http.ResponseWriter, r *http.Request) {
	if !a.requireVerify(w) {
		return
	}
	var req verifyRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	// The badge is for students aged 18 and over, on their own word. Without
	// it the request goes no further: the address is not parsed, no cooldown
	// starts, nothing is stored and nothing is sent.
	if !req.Adult {
		writeError(w, http.StatusBadRequest, adultRequired)
		return
	}

	addr, err := parseEmail(req.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	label, ok := a.domains.lookup(addr.Domain)
	if !ok {
		writeError(w, http.StatusBadRequest,
			addr.Domain+" is not a recognised institutional domain")
		return
	}

	// Both cooldowns before anything is minted: one address cannot be mailed on
	// a loop, and one client cannot walk a list of addresses.
	emailHash := a.emailHash(addr.Key)
	clientKey := clientIP(r, a.cfg.TrustedProxyHops)
	if !a.allowAction(w, "requesting codes", []cooldown{
		{limiter: a.verifyLimiter, key: "ip|" + clientKey},
		{limiter: a.verifyLimiter, key: "email|" + hex.EncodeToString(emailHash)},
	}) {
		return
	}

	code, err := verificationCode()
	if err != nil {
		a.fail(w, r, err, "could not start verification")
		return
	}
	token, err := randomToken()
	if err != nil {
		a.fail(w, r, err, "could not start verification")
		return
	}

	// Stored before it is sent: a code nobody receives expires quietly, while a
	// code that arrives and does not work reads as the feature being broken.
	if err := a.store.StartVerification(r.Context(), pendingVerification{
		EmailHash: emailHash,
		TokenHash: hashToken(token),
		CodeHash:  a.codeHash(token, code),
		Label:     label,
		ExpiresAt: time.Now().Add(a.cfg.VerifyCodeTTL),
	}); err != nil {
		a.fail(w, r, err, "could not start verification")
		return
	}

	if err := a.sender.SendCode(r.Context(), mailer.Code{
		Address: addr.Delivery,
		Code:    code,
		TTL:     a.cfg.VerifyCodeTTL,
	}); err != nil {
		// Deliberately logged without the address: an undeliverable mailbox is
		// not worth writing down anywhere this feature can see.
		a.log.Error("could not send verification code", "error", err, "institution", label)
		writeError(w, http.StatusBadGateway, "could not send the code, try again in a moment")
		return
	}

	a.setCookie(w, r, pendingCookieName, token, pendingCookiePath, a.cfg.VerifyCodeTTL)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":     "sent",
		"label":      label,
		"expires_in": int(a.cfg.VerifyCodeTTL.Seconds()),
	})
}

type verifyConfirmBody struct {
	Code string `json:"code"`
}

// handleVerifyConfirm exchanges a correct code for a credential. Success also
// revokes whatever credential that address held before, which is the store's
// doing: one row per address, and the previous identifier goes with it.
func (a *App) handleVerifyConfirm(w http.ResponseWriter, r *http.Request) {
	if !a.requireVerify(w) {
		return
	}
	var req verifyConfirmBody
	if !decodeJSON(w, r, &req) {
		return
	}
	pending, err := r.Cookie(pendingCookieName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "no verification in progress, request a code first")
		return
	}

	// Codes get read off a screen and retyped, so the spacing people add is
	// not a wrong answer.
	code := strings.Map(func(ru rune) rune {
		if ru == ' ' || ru == '-' {
			return -1
		}
		return ru
	}, strings.TrimSpace(req.Code))
	if code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	id, err := randomToken()
	if err != nil {
		a.fail(w, r, err, "could not complete verification")
		return
	}
	issued := time.Now()
	expires := issued.Add(a.cfg.VerifyCredentialTTL)

	label, err := a.store.ConfirmVerification(r.Context(), confirmVerification{
		TokenHash:        hashToken(pending.Value),
		CodeHash:         a.codeHash(pending.Value, code),
		CredentialIDHash: hashToken(id),
		// Counted from the end of the day the credential is issued in rather
		// than from now, so that the stored row dates the issuance to that
		// day rather than the second, and still outlives the credential it
		// revokes. The row's transaction id dates it more closely until the
		// nightly sweep rewrites the table (Store.RewriteCredentials);
		// credentialDayEnd says why the day ends when it does.
		ExpiresAt: credentialDayEnd(issued).Add(a.cfg.VerifyCredentialTTL),
	})
	switch {
	case errors.Is(err, errNotFound):
		clearCookie(w, pendingCookieName, pendingCookiePath)
		writeError(w, http.StatusBadRequest, "no verification in progress, request a code first")
		return
	case errors.Is(err, errVerifyExpired):
		clearCookie(w, pendingCookieName, pendingCookiePath)
		writeError(w, http.StatusBadRequest, "that code has expired, request a new one")
		return
	case errors.Is(err, errVerifyAttempts):
		clearCookie(w, pendingCookieName, pendingCookiePath)
		writeError(w, http.StatusTooManyRequests, "too many wrong codes, request a new one")
		return
	case errors.Is(err, errVerifyCode):
		writeError(w, http.StatusBadRequest, "that code is not right")
		return
	case err != nil:
		a.fail(w, r, err, "could not complete verification")
		return
	}

	token, err := a.signCredential(credential{Label: label, ID: id, Expires: expires.Unix()})
	if err != nil {
		a.fail(w, r, err, "could not complete verification")
		return
	}
	clearCookie(w, pendingCookieName, pendingCookiePath)
	// That a credential was issued, and nothing about whose or where. The
	// stored row, once the nightly sweep has rewritten it, dates an issuance
	// only to the day; a line naming the institution, stamped to the second,
	// would date that row exactly on any day it was the institution's only
	// one, and line it up with the badged posts that follow. The request
	// log's line for this endpoint already carries the time, so this line
	// carries no more than that does.
	a.log.Info("verification issued")

	// The credential is handed back in the body rather than set as a cookie, so
	// the client decides when it travels. This is the only time the server ever
	// shows it: it is not stored in a form that could be handed out again, so a
	// client that loses it verifies afresh. writeJSON sends no-store, which
	// keeps it out of any cache on the way.
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    true,
		"verified":   true,
		"credential": token,
		"label":      label,
		"expires_at": expires.UTC(),
	})
}

/* Using a credential -------------------------------------------------------- */

// credential reads and checks the credential a request presents: signature,
// then expiry, then the server's own record of it. The label comes back from
// that record rather than from the header, so the server's word is what a badge
// says.
//
// This is the only place in the server that reads credentialHeader, and
// postBadge is the only caller — a credential is examined when it is being
// used, on the request that uses it, and at no other time. There is
// deliberately no cookie fallback: a cookie would arrive on every request to
// the origin, and a credential that arrives everywhere is a session identifier
// whatever it is called.
//
// The lookup writes nothing. A "last used" column here would be a timestamp
// that lines up with a post, which is exactly the correlation this feature
// promises not to keep.
func (a *App) credential(ctx context.Context, r *http.Request) (credential, error) {
	presented := strings.TrimSpace(r.Header.Get(credentialHeader))
	if presented == "" {
		return credential{}, errNoCredential
	}
	cred, ok := a.parseCredential(presented)
	if !ok {
		return credential{}, errNoCredential
	}
	if time.Now().After(time.Unix(cred.Expires, 0)) {
		return credential{}, errCredentialExpired
	}

	label, err := a.store.CredentialLabel(ctx, hashToken(cred.ID))
	if errors.Is(err, errNotFound) {
		// The signature is good but the record is gone: either the address was
		// verified again — which issues a new credential and drops this one —
		// or the row was pruned after expiry.
		return credential{}, errCredentialRevoked
	}
	if err != nil {
		return credential{}, err
	}
	cred.Label = label
	return cred, nil
}

// badgeGrant is what postBadge hands a posting handler: the label to show on
// the post, and the random id of the credential that earned it. The zero value
// is an unbadged post.
type badgeGrant struct {
	Label string
	// CredentialID is the id inside the credential the request presented and
	// postBadge verified. It leaves postBadge for one purpose, deriving the
	// post's poster ID in posterSourceFor, and must never be logged, stored, or
	// put in a response: it is the credential's identity, and posts carry none.
	// TestCredentialIDOnlyFeedsThePosterID holds it to that.
	CredentialID string
}

// postBadge resolves the badge a submission asked for. It answers the zero
// badgeGrant for a post made without one, which is every post by default.
//
// A request for a badge that cannot be honoured is refused rather than quietly
// posted bare: the poster asked to be seen as verified, and finding out
// afterwards that they were not is worse than being told now.
func (a *App) postBadge(w http.ResponseWriter, r *http.Request, want bool) (badgeGrant, bool) {
	if !want {
		return badgeGrant{}, true
	}
	if !a.verifyEnabled() {
		writeError(w, http.StatusForbidden, "verification is not configured")
		return badgeGrant{}, false
	}

	cred, err := a.credential(r.Context(), r)
	switch {
	case err == nil:
		return badgeGrant{Label: cred.Label, CredentialID: cred.ID}, true
	case errors.Is(err, errNoCredential):
		refuseBadge(w, credentialMissing,
			"verify an institutional address before posting with a badge")
	case errors.Is(err, errCredentialExpired):
		refuseBadge(w, credentialExpired,
			"your verification has expired, verify again to post with a badge")
	case errors.Is(err, errCredentialRevoked):
		refuseBadge(w, credentialRevoked,
			"this verification is no longer valid, verify again to post with a badge")
	default:
		a.fail(w, r, err, "could not read verification")
	}
	return badgeGrant{}, false
}

// refuseBadge answers a post that asked for a badge it cannot have. The reason
// is machine-readable beside the message, so a client can discard a credential
// the server will not honour again rather than offering it for the next post.
func refuseBadge(w http.ResponseWriter, state, msg string) {
	writeJSON(w, http.StatusForbidden, map[string]any{"error": msg, "credential": state})
}

/* Credential format --------------------------------------------------------- */

// signCredential renders a credential as "v1.<claims>.<mac>", the claims being
// base64url JSON. The encoding is not encryption and is not meant to be: the
// holder may read their own badge and expiry, and the MAC is what stops them
// writing a new one.
func (a *App) signCredential(c credential) (string, error) {
	claims, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode credential: %w", err)
	}
	signed := credentialVersion + "." + base64.RawURLEncoding.EncodeToString(claims)
	return signed + "." + base64.RawURLEncoding.EncodeToString(a.credentialMAC(signed)), nil
}

// parseCredential checks a credential's signature and decodes its claims. It
// deliberately stops there: expiry and revocation belong to the caller, which
// has the context to look one up and the vocabulary to explain the other.
func (a *App) parseCredential(token string) (credential, bool) {
	cut := strings.LastIndexByte(token, '.')
	if cut < 0 {
		return credential{}, false
	}
	signed, sig := token[:cut], token[cut+1:]
	claims, ok := strings.CutPrefix(signed, credentialVersion+".")
	if !ok {
		return credential{}, false
	}

	mac, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(mac, a.credentialMAC(signed)) {
		return credential{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(claims)
	if err != nil {
		return credential{}, false
	}
	var c credential
	if err := json.Unmarshal(payload, &c); err != nil || c.ID == "" || c.Label == "" {
		return credential{}, false
	}
	return c, true
}

func (a *App) credentialMAC(value string) []byte {
	return hmacSum([]byte(a.cfg.VerifySecret), "credential:"+value)
}

// emailHash is the uniqueness key: the only thing derived from an address that
// educhan keeps. It is keyed by the server secret, so a stolen database cannot
// be tested against a list of addresses without it, and it is stored in a table
// that references nothing.
func (a *App) emailHash(key string) []byte {
	return hmacSum([]byte(a.cfg.VerifySecret), "email:"+key)
}

// codeHash binds a pending code to the pending verification it belongs to, so
// the stored hash of "123456" differs between two verifications in flight.
func (a *App) codeHash(token, code string) []byte {
	return hmacSum([]byte(a.cfg.VerifySecret), "code:"+token+":"+code)
}

// hashToken reduces a random identifier to what the database holds. The
// identifiers are 128-bit and random, so a plain digest is enough: there is no
// dictionary of them to iterate.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// verificationCode returns the digits emailed to an address, drawn uniformly
// so every code is as likely as any other.
func verificationCode() (string, error) {
	space := new(big.Int).Exp(big.NewInt(10), big.NewInt(verifyCodeDigits), nil)
	n, err := rand.Int(rand.Reader, space)
	if err != nil {
		return "", fmt.Errorf("generate verification code: %w", err)
	}
	return fmt.Sprintf("%0*d", verifyCodeDigits, n), nil
}

// credentialDayStart is when, in UTC, a credential day begins: the day a
// credential row dates its issuance to (credentialDayEnd).
const credentialDayStart = 5 * time.Hour

// credentialDayEnd returns the end of the credential day t falls in, a day
// that runs from credentialDayStart (05:00 UTC) to the same time the next
// day. A credential row's stored expiry is the end of the day it was issued
// in plus the credential's lifetime, rather than the exact expiry, so that it
// tells what day an address was verified and not what minute — the resolution
// that would let an issuance be lined up against a post.
//
// That is half of what keeps the credential's own row to the day. The other
// half is the nightly sweep, which rewrites every credential row so that the
// transaction id PostgreSQL keeps on each is the sweep's
// (Store.RewriteCredentials), and the two have to agree on when a day ends.
// The sweep gives its id to every row issued before it and to none issued
// after, so it splits the day it runs in. Were that day to begin at midnight,
// a row of it carrying the sweep's id would have been issued between midnight
// and the sweep at 03:17 UTC, and the expiry and the id together would date it
// to those three hours, until the next sweep and in that morning's backups.
// So a day ends after the sweep instead, and after the time the Job's one
// retry may take as well, which the deploy guide keeps before its backup
// window opens at 05:00 UTC (deploy/README.md sections 2.2 and 2.9). Then
// the sweep's id is on every credential of an earlier day, where it says
// nothing the expiry does not, and on those of the day it runs in that were
// issued in the 22 hours before it; one issued between the sweep and 05:00
// carries its own id, which dates it anyway until the next sweep.
// TestCredentialDaysEndAfterTheSweep reads the schedule, and the Job's retries
// and task timeout, from the deploy guide and first-deploy.sh, and wants the
// day the sweep runs in to end half an hour or more after the latest the
// sweep and its retry can still be running, and two hours at most after the
// scheduled time: a sweep scheduled at 03:00 UTC or later, whose retry cannot
// run past 04:30. With one retry of a fifteen-minute task, that is a sweep
// scheduled from 03:00 to 04:00. A change to the schedule, the retries or the
// timeout that leaves that window means moving credentialDayStart with it,
// not only a move past 05:00. A sweep at 04:40 that succeeds at once falls in
// the same day as one at 03:17, and splits it no more; but its retry can run
// until 05:10, and a retry that rewrites after 05:00 splits the next day, as
// a sweep at 05:30 always does, which is why the test rejects it. And the day
// of a sweep at 02:30 ends two and a half hours after it. A sweep run after
// 05:00 — by hand, or late — still splits the day it runs in at the time it
// runs, and the README says what that leaves.
//
// It is not all an operator can see. The code request and the confirmation
// each took a transaction id from the counter the posts take theirs from, and
// the gaps they leave among the posts' ids still place the issuance between
// two posts as closely as the row's own id did, which can be to the minute:
// the README's "What that protects, exactly" and
// TestIssuanceLeavesGapsAmongThePostsTransactionIDs.
func credentialDayEnd(t time.Time) time.Time {
	shifted := t.UTC().Add(-credentialDayStart)
	day := time.Date(shifted.Year(), shifted.Month(), shifted.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, 1).Add(credentialDayStart)
}

/* Cookies ------------------------------------------------------------------- */

// The pending token is the only cookie this feature sets. The credential is
// not one: see credentialHeader.

func (a *App) setCookie(w http.ResponseWriter, r *http.Request, name, value, path string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:  name,
		Value: value,
		Path:  path,
		// Lax keeps another site from driving a verification in progress on the
		// holder's behalf; the API has no other CSRF defence. Posting needs no
		// such protection now that the credential is a header — a cross-site
		// form submission carries no header a browser would add for it.
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   a.requestIsHTTPS(r),
		MaxAge:   int(ttl.Seconds()),
	})
}

func clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Path:     path,
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// requestIsHTTPS reports whether the browser reached the server over TLS, so a
// deployment that terminates TLS at a proxy still marks its cookies Secure.
// X-Forwarded-Proto is read on the same terms as X-Forwarded-For: only when the
// operator has said a proxy is there.
func (a *App) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if a.cfg.TrustedProxyHops <= 0 {
		return false
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}
