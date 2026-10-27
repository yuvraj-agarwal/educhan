package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

/* Harness ------------------------------------------------------------------- */

// withCaptcha switches the captcha on at a work small enough that a test
// solves a challenge in a few milliseconds.
func withCaptcha(tune func(*Config)) func(*Config) {
	return func(cfg *Config) {
		cfg.CaptchaSecret = testCaptchaSecret
		cfg.CaptchaWork = 4
		if tune != nil {
			tune(cfg)
		}
	}
}

// captchaServer is testServer with the captcha on.
func captchaServer(t *testing.T, tune func(*Config)) *harness {
	t.Helper()
	return testServer(t, withCaptcha(tune))
}

// solveCaptcha asks the server for a challenge and answers it, as app.js does.
func (c *client) solveCaptcha() string {
	c.t.Helper()
	var challenge captchaChallenge
	c.get("/api/captcha", &challenge)
	return solveChallenge(c.t, challenge)
}

// captchaRefusal is a refused request's body: the message, and for a captcha
// refusal the reason.
type captchaRefusal struct {
	Error   string `json:"error"`
	Captcha string `json:"captcha"`
}

// attempt posts body to path, answering the status and a refusal decoded
// into a fresh value, so nothing is left over from an earlier one.
func (c *client) attempt(path string, body any) (int, captchaRefusal) {
	c.t.Helper()
	var refused captchaRefusal
	return c.post(path, body, &refused), refused
}

// expectCaptchaRefusal checks a refusal is the captcha's, for reason.
func expectCaptchaRefusal(t *testing.T, what string, status int, got captchaRefusal, reason string) {
	t.Helper()
	if status != http.StatusForbidden || got.Captcha != reason {
		t.Errorf("%s: status %d, captcha %q (%s), want 403 %q", what, status, got.Captcha, got.Error, reason)
	}
}

// fakeCaptcha stands in for the verifier: it issues placeholder challenges,
// noting the difficulty each was asked at, and answers every check with err.
type fakeCaptcha struct {
	err error

	mu     sync.Mutex
	scales []int
}

func (f *fakeCaptcha) issue(_ time.Time, scale int) (captchaChallenge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scales = append(f.scales, scale)
	return captchaChallenge{Signature: strconv.Itoa(len(f.scales))}, nil
}

func (f *fakeCaptcha) check(payload string, now time.Time) (captchaTicket, error) {
	if f.err != nil {
		return captchaTicket{}, f.err
	}
	key := sha256.Sum256([]byte(payload))
	return captchaTicket{key: key[:], expires: now.Add(captchaTTL)}, nil
}

func (f *fakeCaptcha) issued() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.scales...)
}

/* Tests -------------------------------------------------------------------- */

func TestCaptchaIsOffWithoutASecret(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("general")

	var site Site
	h.get("/api/site", &site)
	if site.Captcha {
		t.Error("/api/site reports the captcha on with no secret configured")
	}
	var refused captchaRefusal
	if status := h.do(http.MethodGet, "/api/captcha", nil, &refused); status != http.StatusNotFound {
		t.Errorf("GET /api/captcha with the captcha off: status %d, want 404", status)
	}
	// Posting needs no answer, and one sent anyway is ignored.
	thread := h.createThread("general", "", "", "no captcha here")
	if status := h.post(fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo),
		map[string]any{"body": "an answer nobody asked for", "captcha": "bm90IGFuIGFuc3dlcg=="}, &refused); status != http.StatusCreated {
		t.Errorf("reply carrying an answer with the captcha off: status %d (%s), want 201", status, refused.Error)
	}
}

func TestPostingNeedsTheCaptcha(t *testing.T) {
	h := captchaServer(t, nil)
	h.addBoard("general")

	var site Site
	h.get("/api/site", &site)
	if !site.Captcha {
		t.Fatal("/api/site does not report the captcha on")
	}

	threads := "/api/boards/general/threads"
	var refused captchaRefusal
	status := h.post(threads, map[string]any{"body": "no answer"}, &refused)
	expectCaptchaRefusal(t, "thread without an answer", status, refused, captchaMissing)
	status = h.post(threads, map[string]any{"body": "made-up answer", "captcha": "bm90IGFuIGFuc3dlcg=="}, &refused)
	expectCaptchaRefusal(t, "thread with a made-up answer", status, refused, captchaInvalid)
	if n := h.countRows("threads"); n != 0 {
		t.Fatalf("%d threads stored from refused posts", n)
	}

	answer := h.solveCaptcha()
	var created createdPost
	if status := h.post(threads, map[string]any{"body": "solved", "captcha": answer}, &created); status != http.StatusCreated {
		t.Fatalf("thread with an answer: status %d (%s)", status, created.Error)
	}
	status = h.post(threads, map[string]any{"body": "the same answer again", "captcha": answer}, &refused)
	expectCaptchaRefusal(t, "thread replaying an answer", status, refused, captchaUsed)

	replies := fmt.Sprintf("/api/boards/general/threads/%d/posts", created.ThreadNo)
	status = h.post(replies, map[string]any{"body": "no answer"}, &refused)
	expectCaptchaRefusal(t, "reply without an answer", status, refused, captchaMissing)
	status = h.post(replies, map[string]any{"body": "the thread's answer", "captcha": answer}, &refused)
	expectCaptchaRefusal(t, "reply replaying the thread's answer", status, refused, captchaUsed)
	answer = h.solveCaptcha()
	if status := h.post(replies, map[string]any{"body": "solved", "captcha": answer}, &created); status != http.StatusCreated {
		t.Fatalf("reply with an answer: status %d (%s)", status, created.Error)
	}

	// A post with an image carries its answer as a form field.
	image := pngBytes(t, 40, 30)
	status = h.upload(replies, map[string]string{"body": "picture"}, "a.png", image, &refused)
	expectCaptchaRefusal(t, "multipart reply without an answer", status, refused, captchaMissing)
	answer = h.solveCaptcha()
	if status := h.upload(replies, map[string]string{"body": "picture", "captcha": answer},
		"a.png", image, &created); status != http.StatusCreated {
		t.Fatalf("multipart reply with an answer: status %d (%s)", status, created.Error)
	}
	status = h.upload(threads, map[string]string{"body": "picture again", "captcha": answer}, "a.png", image, &refused)
	expectCaptchaRefusal(t, "multipart thread replaying an answer", status, refused, captchaUsed)

	if n := h.countRows("posts"); n != 3 {
		t.Errorf("%d posts stored, want the 3 that carried a fresh answer", n)
	}
}

// A post that would be refused for itself is refused for that first, so the
// poster fixes it before solving anything.
func TestPostRefusalsComeBeforeTheCaptcha(t *testing.T) {
	h := captchaServer(t, nil)
	h.addBoard("general")

	if status, refused := h.attempt("/api/boards/general/threads", map[string]any{"body": "  "}); status != http.StatusBadRequest || refused.Captcha != "" {
		t.Errorf("empty thread: status %d, captcha %q, want 400 about the body", status, refused.Captcha)
	}
	if status, _ := h.attempt("/api/boards/nowhere/threads", map[string]any{"body": "hi"}); status != http.StatusNotFound {
		t.Errorf("thread on a missing board: status %d, want 404", status)
	}
	thread := h.createThreadWithCaptcha("general", "open")
	h.exec(`UPDATE threads SET locked = true WHERE no = $1`, thread.ThreadNo)
	status, refused := h.attempt(fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo), map[string]any{"body": "too late"})
	if status != http.StatusForbidden || refused.Captcha != "" || !strings.Contains(refused.Error, "locked") {
		t.Errorf("reply to a locked thread: status %d, captcha %q (%s), want 403 about the lock", status, refused.Captcha, refused.Error)
	}
}

func (h *harness) createThreadWithCaptcha(board, body string) createdPost {
	h.t.Helper()
	var out createdPost
	if status := h.post("/api/boards/"+board+"/threads",
		map[string]any{"body": body, "captcha": h.solveCaptcha()}, &out); status != http.StatusCreated {
		h.t.Fatalf("create thread: status %d (%s)", status, out.Error)
	}
	return out
}

// The captcha is checked before flood control and spent after it: a post
// refused by the captcha starts no cooldown, and a post refused by a cooldown
// keeps its answer for when it is let through.
func TestCaptchaAndFloodControl(t *testing.T) {
	h := captchaServer(t, func(cfg *Config) { cfg.ThreadCooldown = time.Hour })
	h.addBoard("general", "other")

	var refused captchaRefusal
	status := h.post("/api/boards/general/threads", map[string]any{"body": "no answer"}, &refused)
	expectCaptchaRefusal(t, "thread without an answer", status, refused, captchaMissing)
	h.createThreadWithCaptcha("general", "the refusal cost no cooldown")

	answer := h.solveCaptcha()
	if status := h.post("/api/boards/general/threads", map[string]any{"body": "too soon", "captcha": answer}, &refused); status != http.StatusTooManyRequests {
		t.Fatalf("second thread inside the cooldown: status %d (%s), want 429", status, refused.Error)
	}
	var created createdPost
	if status := h.post("/api/boards/other/threads", map[string]any{"body": "same answer, other board", "captcha": answer}, &created); status != http.StatusCreated {
		t.Errorf("an answer held back by a cooldown, used on another board: status %d (%s), want 201", status, created.Error)
	}
}

// A refusal says whether it took the answer with it. Every refusal before the
// spend, a cooldown included, leaves the answer for the next press and carries
// no header; one after it, for an image the server cannot read, carries the
// header, and the answer is refused as used from then on. So the page keeps an
// answer exactly as long as it is good and solves the next one only when it
// has to.
func TestARefusalSaysWhetherTheAnswerWasSpent(t *testing.T) {
	h := captchaServer(t, func(cfg *Config) { cfg.ThreadCooldown = time.Hour })
	h.addBoard("general", "other", "third")
	spent := func() bool { return h.lastHeader.Get(captchaSpentHeader) == "spent" }

	// The thread also starts the cooldown on its board.
	locked := h.createThreadWithCaptcha("other", "locked soon")
	h.exec(`UPDATE threads SET locked = true WHERE no = $1`, locked.ThreadNo)

	answer := h.solveCaptcha()
	for _, refusal := range []struct {
		what, path string
		body       map[string]any
		status     int
	}{
		{"an empty thread", "/api/boards/general/threads", map[string]any{"body": " "}, http.StatusBadRequest},
		{"a reply to a locked thread", fmt.Sprintf("/api/boards/other/threads/%d/posts", locked.ThreadNo),
			map[string]any{"body": "too late"}, http.StatusForbidden},
		{"a thread inside the cooldown", "/api/boards/other/threads", map[string]any{"body": "too soon"}, http.StatusTooManyRequests},
	} {
		refusal.body["captcha"] = answer
		if status, refused := h.attempt(refusal.path, refusal.body); status != refusal.status || refused.Captcha != "" || spent() {
			t.Errorf("%s: status %d, captcha %q, spent header %q; want %d, the answer kept",
				refusal.what, status, refused.Captcha, h.lastHeader.Get(captchaSpentHeader), refusal.status)
		}
	}

	// A file that looks like an image is let through to flood control and the
	// spend, and refused only when its pixels fail to decode.
	var refused captchaRefusal
	status := h.upload("/api/boards/general/threads", map[string]string{"body": "look", "captcha": answer},
		"cut.png", pngBytes(t, 24, 24)[:33], &refused)
	if status != http.StatusBadRequest || refused.Captcha != "" || !strings.Contains(refused.Error, "image could not be decoded") || !spent() {
		t.Errorf("an image refused after the spend: status %d, captcha %q (%s), spent header %q; want 400 saying the answer was spent",
			status, refused.Captcha, refused.Error, h.lastHeader.Get(captchaSpentHeader))
	}
	// The refused image started general's cooldown, as any post let through
	// flood control does. The spent answer sent there again is told it was
	// used, not to wait: the lookup comes before flood control, so the page
	// solves a fresh puzzle rather than waiting out a cooldown for an answer
	// that can never work.
	status, refused = h.attempt("/api/boards/general/threads", map[string]any{"body": "again", "captcha": answer})
	expectCaptchaRefusal(t, "the spent answer inside a cooldown", status, refused, captchaUsed)
	if h.lastHeader.Get("Retry-After") != "" || spent() {
		t.Errorf("the spent answer inside a cooldown: Retry-After %q, spent header %q; want neither",
			h.lastHeader.Get("Retry-After"), h.lastHeader.Get(captchaSpentHeader))
	}
	// On another board, with no cooldown running, it is refused as used too,
	// and the fresh answer that follows posts there at once.
	status, refused = h.attempt("/api/boards/third/threads", map[string]any{"body": "text alone", "captcha": answer})
	expectCaptchaRefusal(t, "the answer an image refusal spent", status, refused, captchaUsed)
	if spent() {
		t.Error("a refusal as used says the answer was spent by it")
	}

	var created createdPost
	if status := h.post("/api/boards/third/threads", map[string]any{"body": "fresh", "captcha": h.solveCaptcha()}, &created); status != http.StatusCreated || !spent() {
		t.Errorf("a post with a fresh answer: status %d (%s), spent header %q; want 201 saying the answer was spent",
			status, created.Error, h.lastHeader.Get(captchaSpentHeader))
	}
	h.get("/api/site", &Site{})
	if spent() {
		t.Error("a request that carried no answer says one was spent")
	}
}

// A badged post skips the captcha; the badge has already cost a verified
// student more than a puzzle would. Holding a credential is not enough: only a
// post that asks for the badge, and gets it, goes without an answer.
func TestBadgedPostsSkipTheCaptcha(t *testing.T) {
	h := verifyServer(t, withCaptcha(nil))
	h.addBoard("general")

	var started verifyResponse
	if status := h.post("/api/verify/request", map[string]any{
		"email": "ana@iitb.ac.in", "adult": true, "captcha": h.solveCaptcha(),
	}, &started); status != http.StatusAccepted {
		t.Fatalf("request code: status %d (%s)", status, started.Error)
	}
	status, confirmed := h.confirmCode(h.mail.last(t).Code)
	if status != http.StatusOK {
		t.Fatalf("confirm code: status %d (%s)", status, confirmed.Error)
	}

	var created createdPost
	if status := h.postWithCredential(confirmed.Credential, "/api/boards/general/threads",
		map[string]any{"body": "badged, no puzzle", "badge": true}, &created); status != http.StatusCreated {
		t.Fatalf("badged thread without an answer: status %d (%s)", status, created.Error)
	}
	replies := fmt.Sprintf("/api/boards/general/threads/%d/posts", created.ThreadNo)
	if status := h.postWithCredential(confirmed.Credential, replies,
		map[string]any{"body": "badged reply", "badge": true}, &created); status != http.StatusCreated {
		t.Fatalf("badged reply without an answer: status %d (%s)", status, created.Error)
	}

	var refused captchaRefusal
	status = h.postWithCredential(confirmed.Credential, replies, map[string]any{"body": "anonymous"}, &refused)
	expectCaptchaRefusal(t, "unbadged reply from a verified client", status, refused, captchaMissing)

	// Asking for a badge without one is refused as a badge problem, not a
	// captcha one, so the page sends the poster to verify rather than to solve.
	status, refused = h.attempt(replies, map[string]any{"body": "badge please", "badge": true})
	if status != http.StatusForbidden || refused.Captcha != "" {
		t.Errorf("badge without a credential: status %d, captcha %q, want a 403 about the badge", status, refused.Captcha)
	}
}

// A credential changes nothing about the captcha off the badge path: the site
// settings read the same with one or without, and a challenge is issued the
// same way, counted against the same address, so presenting a credential
// neither skips the count nor starts a fresh one.
func TestCredentialHasNoEffectOnTheCaptcha(t *testing.T) {
	fake := &fakeCaptcha{}
	h := verifyServer(t, func(cfg *Config) { cfg.captcha = fake })
	now := time.Unix(1_800_000_000, 0).Add(15 * time.Second)
	meterAt(h.app.captchaMeter, &now)

	var started verifyResponse
	if status := h.post("/api/verify/request", map[string]any{
		"email": "ana@iitb.ac.in", "adult": true, "captcha": "any answer the fake takes",
	}, &started); status != http.StatusAccepted {
		t.Fatalf("request code: status %d (%s)", status, started.Error)
	}
	status, confirmed := h.confirmCode(h.mail.last(t).Code)
	if status != http.StatusOK {
		t.Fatalf("confirm code: status %d (%s)", status, confirmed.Error)
	}

	var bare, presented json.RawMessage
	h.get("/api/site", &bare)
	if status := h.getWithCredential(confirmed.Credential, "/api/site", &presented); status != http.StatusOK || !bytes.Equal(bare, presented) {
		t.Errorf("GET /api/site with a credential: status %d, %s; without: %s", status, presented, bare)
	}

	for i := range 2 * captchaFreeChallenges {
		var challenge captchaChallenge
		var status int
		if i%2 == 0 {
			status = h.do(http.MethodGet, "/api/captcha", nil, &challenge)
		} else {
			status = h.getWithCredential(confirmed.Credential, "/api/captcha", &challenge)
		}
		if status != http.StatusOK {
			t.Fatalf("challenge %d: status %d", i+1, status)
		}
	}
	// Every other request carried the credential, so the requests alternate,
	// and the count runs on through all of them.
	scales := fake.issued()
	if len(scales) != 2*captchaFreeChallenges {
		t.Fatalf("%d challenges issued, want %d", len(scales), 2*captchaFreeChallenges)
	}
	for i, scale := range scales {
		if want := captchaScale(i + 1); scale != want {
			t.Fatalf("challenge %d issued at scale %d, want %d: a credential changed the count", i+1, scale, want)
		}
	}
}

func TestCodeRequestsNeedTheCaptcha(t *testing.T) {
	h := verifyServer(t, withCaptcha(nil))

	// The attestation comes first: without it nothing else is looked at.
	if status, refused := h.attempt("/api/verify/request", map[string]any{"email": "ana@iitb.ac.in"}); status != http.StatusBadRequest || refused.Captcha != "" {
		t.Errorf("request without the attestation: status %d, captcha %q, want 400 about the attestation", status, refused.Captcha)
	}
	status, refused := h.attempt("/api/verify/request", map[string]any{"email": "ana@iitb.ac.in", "adult": true})
	expectCaptchaRefusal(t, "request without an answer", status, refused, captchaMissing)

	// An address that is refused leaves the answer unspent.
	answer := h.solveCaptcha()
	if status, refused := h.attempt("/api/verify/request", map[string]any{
		"email": "ana@gmail.com", "adult": true, "captcha": answer,
	}); status != http.StatusBadRequest || refused.Captcha != "" {
		t.Errorf("request for a refused domain: status %d (%s), want 400", status, refused.Error)
	}
	var started verifyResponse
	if status := h.post("/api/verify/request", map[string]any{
		"email": "ana@iitb.ac.in", "adult": true, "captcha": answer,
	}, &started); status != http.StatusAccepted {
		t.Fatalf("request with the same answer: status %d (%s), want 202", status, started.Error)
	}
	status = h.post("/api/verify/request", map[string]any{
		"email": "ben@iitb.ac.in", "adult": true, "captcha": answer,
	}, &refused)
	expectCaptchaRefusal(t, "request replaying an answer", status, refused, captchaUsed)
	if n := h.mail.count(); n != 1 {
		t.Errorf("%d codes sent, want only the one request that carried a fresh answer", n)
	}
}

// burstSize is how many requests a burst sends at once.
const burstSize = 8

// roomForABurst gives the server's pool a connection for each request of a
// burst held at its spend (spendingBurst), one for the hold, and one to watch
// the requests arrive with.
func roomForABurst(cfg *Config) {
	cfg.DBMaxConns = burstSize + 2
}

// solveCaptchas answers n challenges, one for each request of a burst.
func (c *client) solveCaptchas(n int) []string {
	c.t.Helper()
	answers := make([]string, n)
	for i := range answers {
		answers[i] = c.solveCaptcha()
	}
	return answers
}

// jsonRequest builds post's request without sending it, for a burst.
func (c *client) jsonRequest(path string, body any) *http.Request {
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
	return req
}

// burstOutcome is how one request of a burst was answered: the answer it
// carried, its status, and whether the response said the answer was spent.
type burstOutcome struct {
	answer string
	status int
	spent  bool
}

// burst sends one request for each answer, all at the same moment, each built
// beforehand by request, and says how each was answered. meanwhile, unless it
// is nil, runs once they have gone, while they are on their way.
func (h *harness) burst(answers []string, request func(i int, answer string) *http.Request, meanwhile func()) []burstOutcome {
	h.t.Helper()
	requests := make([]*http.Request, len(answers))
	for i, answer := range answers {
		requests[i] = request(i, answer)
	}
	outcomes := make([]burstOutcome, len(answers))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, req := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := h.http.Do(req)
			if err != nil {
				return
			}
			res.Body.Close()
			outcomes[i] = burstOutcome{answers[i], res.StatusCode, res.Header.Get(captchaSpentHeader) == "spent"}
		}()
	}
	close(start)
	if meanwhile != nil {
		meanwhile()
	}
	wg.Wait()
	return outcomes
}

// spendingBurst is burst for requests that each spend an answer, every one of
// them held at its spend until all of them are there. Nothing starts a
// cooldown before its spend, so the whole burst passes flood control's first
// check, and only the second stands between it and every request going ahead.
// Unheld, one request could start its cooldowns before the others reached the
// first check, which would then refuse them all and leave the second untried.
func (h *harness) spendingBurst(answers []string, request func(i int, answer string) *http.Request) []burstOutcome {
	h.t.Helper()
	if conns, need := h.app.pool.Config().MaxConns, int32(len(answers))+2; conns < need {
		h.t.Fatalf("the pool has %d connections, and a burst of %d held at its spends needs %d (roomForABurst)",
			conns, len(answers), need)
	}
	pid, release := h.holdEverySpend()
	return h.burst(answers, request, func() {
		h.waitForSpends(pid, len(answers))
		release()
	})
}

// holdEverySpend locks the table answers are spent in, in a transaction it
// leaves open, so that every request that reaches its spend waits there until
// release ends the transaction. Checking an answer only reads the table, which
// the lock allows. pid is the open transaction's backend, which the waiting
// requests wait on.
func (h *harness) holdEverySpend() (pid uint32, release func()) {
	h.t.Helper()
	ctx := context.Background()
	tx, err := h.app.pool.Begin(ctx)
	if err != nil {
		h.t.Fatalf("begin: %v", err)
	}
	h.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		h.t.Fatalf("backend pid: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE captcha_spent IN SHARE MODE`); err != nil {
		h.t.Fatalf("hold the spends: %v", err)
	}
	return pid, func() {
		if err := tx.Rollback(ctx); err != nil {
			h.t.Fatalf("release the spends: %v", err)
		}
	}
}

// waitForSpends waits until n requests are waiting on the hold whose backend
// is pid.
func (h *harness) waitForSpends(pid uint32, n int) {
	h.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := h.app.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid))`,
			int64(pid)).Scan(&waiting); err != nil {
			h.t.Fatalf("count the requests at their spends: %v", err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%d of %d requests reached their spends", waiting, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// expectOneThrough checks that one request of a spending burst went ahead,
// answered ok, and that flood control's second check refused every other with
// a 429 saying its answer was spent. again sends a refused answer once more
// where no cooldown holds it back, and it is refused as used.
func expectOneThrough(t *testing.T, what string, outcomes []burstOutcome, ok int, again func(i int, answer string) (int, captchaRefusal)) {
	t.Helper()
	through := 0
	for i, o := range outcomes {
		switch {
		case o.status == ok:
			through++
		case o.status == http.StatusTooManyRequests && o.spent:
			status, refused := again(i, o.answer)
			expectCaptchaRefusal(t, fmt.Sprintf("%s, racer %d's answer, which its 429 said was spent, sent again", what, i),
				status, refused, captchaUsed)
		default:
			t.Errorf("%s, racer %d: status %d, spent header %v; want %d, or 429 saying the answer was spent",
				what, i, o.status, o.spent, ok)
		}
	}
	if through != 1 {
		t.Errorf("%s: %d of %d went ahead, want 1", what, through, len(outcomes))
	}
}

// A burst of posts from one address, each with an answer of its own, can all
// pass flood control's first check before any of them spends, which the test
// makes sure of (spendingBurst); the second check comes again as the
// cooldowns start, so exactly one posts. The others were refused there, have
// spent their answers, and say so.
func TestABurstFromOneAddressPostsOnce(t *testing.T) {
	h := captchaServer(t, func(cfg *Config) {
		cfg.ThreadCooldown = time.Hour
		roomForABurst(cfg)
	})
	h.addBoard("general")

	outcomes := h.spendingBurst(h.solveCaptchas(burstSize), func(_ int, answer string) *http.Request {
		return h.jsonRequest("/api/boards/general/threads", map[string]any{"body": "burst", "captcha": answer})
	})
	expectOneThrough(t, "racing threads", outcomes, http.StatusCreated, func(i int, answer string) (int, captchaRefusal) {
		// On a board of its own, whose thread cooldown nothing has started.
		board := fmt.Sprint("spare", i)
		h.addBoard(board)
		return h.attempt("/api/boards/"+board+"/threads", map[string]any{"body": "again", "captcha": answer})
	})
}

// The same for replies: a burst of them from one address to one thread, each
// with an answer of its own, posts once.
func TestABurstOfRepliesFromOneAddressPostsOnce(t *testing.T) {
	h := captchaServer(t, func(cfg *Config) {
		cfg.PostCooldown = time.Hour
		roomForABurst(cfg)
	})
	h.addBoard("general")
	thread := h.createThreadWithCaptcha("general", "open")
	path := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)

	outcomes := h.spendingBurst(h.solveCaptchas(burstSize), func(_ int, answer string) *http.Request {
		return h.jsonRequest(path, map[string]any{"body": "burst", "captcha": answer})
	})
	expectOneThrough(t, "racing replies", outcomes, http.StatusCreated, func(_ int, answer string) (int, captchaRefusal) {
		// As a new thread, which the reply cooldown does not hold back.
		return h.attempt("/api/boards/general/threads", map[string]any{"body": "again", "captcha": answer})
	})
}

// The upload cooldown is one per address across every board, so a burst of
// posts with images from one address, new threads on one board and replies on
// another, posts once, though neither board's own cooldown is running.
func TestABurstOfImagesFromOneAddressPostsOnce(t *testing.T) {
	h := captchaServer(t, func(cfg *Config) {
		cfg.UploadCooldown = time.Hour
		roomForABurst(cfg)
	})
	h.addBoard("general", "other")
	thread := h.createThreadWithCaptcha("other", "open")
	replies := fmt.Sprintf("/api/boards/other/threads/%d/posts", thread.ThreadNo)
	image := pngBytes(t, 24, 24)

	outcomes := h.spendingBurst(h.solveCaptchas(burstSize), func(i int, answer string) *http.Request {
		path := "/api/boards/general/threads"
		if i%2 == 1 {
			path = replies
		}
		return h.uploadRequest(path, map[string]string{"body": "look", "captcha": answer}, "look.png", image)
	})
	expectOneThrough(t, "racing posts with images", outcomes, http.StatusCreated, func(_ int, answer string) (int, captchaRefusal) {
		// Without the image, which nothing holds back.
		return h.attempt("/api/boards/general/threads", map[string]any{"body": "again", "captcha": answer})
	})
}

// A burst of code requests from one client, each for another email address and
// with an answer of its own, sends one code: the client's cooldown is checked
// again as the cooldowns start, as a post's are.
func TestABurstOfCodeRequestsFromOneClientSendsOneCode(t *testing.T) {
	h := verifyServer(t, withCaptcha(func(cfg *Config) {
		cfg.VerifyCooldown = time.Hour
		cfg.TrustedProxyHops = 1
		roomForABurst(cfg)
	}))

	outcomes := h.spendingBurst(h.solveCaptchas(burstSize), func(i int, answer string) *http.Request {
		req := h.jsonRequest("/api/verify/request", map[string]any{
			"email": fmt.Sprintf("student%d@iitb.ac.in", i), "adult": true, "captcha": answer,
		})
		req.Header.Set("X-Forwarded-For", "203.0.113.1")
		return req
	})
	if n := h.mail.count(); n != 1 {
		t.Errorf("%d codes sent by the burst, want 1", n)
	}
	expectOneThrough(t, "racing code requests", outcomes, http.StatusAccepted, func(i int, answer string) (int, captchaRefusal) {
		// From another client, for another email address, so no cooldown the
		// burst started holds it back.
		var refused captchaRefusal
		status := h.postFrom(fmt.Sprintf("198.51.100.%d", i+1), "/api/verify/request", map[string]any{
			"email": fmt.Sprintf("again%d@iitb.ac.in", i), "adult": true, "captcha": answer,
		}, &refused)
		return status, refused
	})
}

// A burst of code requests for one email address, each from another client and
// with an answer of its own, sends one code too: the address's cooldown is
// checked again as the cooldowns start, as the client's is.
func TestABurstOfCodeRequestsForOneAddressSendsOneCode(t *testing.T) {
	h := verifyServer(t, withCaptcha(func(cfg *Config) {
		cfg.VerifyCooldown = time.Hour
		cfg.TrustedProxyHops = 1
		roomForABurst(cfg)
	}))

	outcomes := h.spendingBurst(h.solveCaptchas(burstSize), func(i int, answer string) *http.Request {
		req := h.jsonRequest("/api/verify/request", map[string]any{
			"email": "ana@iitb.ac.in", "adult": true, "captcha": answer,
		})
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i+1))
		return req
	})
	if n := h.mail.count(); n != 1 {
		t.Errorf("%d codes sent by the burst, want 1", n)
	}
	expectOneThrough(t, "racing code requests for one address", outcomes, http.StatusAccepted, func(i int, answer string) (int, captchaRefusal) {
		// From a client of its own, for another email address, so no cooldown
		// the burst started holds it back.
		var refused captchaRefusal
		status := h.postFrom(fmt.Sprintf("198.51.100.%d", i+1), "/api/verify/request", map[string]any{
			"email": fmt.Sprintf("again%d@iitb.ac.in", i), "adult": true, "captcha": answer,
		}, &refused)
		return status, refused
	})
}

// A burst of badged posts posts once too. They spend no answer, so nothing
// lies between flood control's two halves, and the first half alone holds the
// burst to one: this shows a badged burst posts once, not that the second
// half holds, which the unbadged bursts above pin.
func TestABadgedBurstFromOneAddressPostsOnce(t *testing.T) {
	h := verifyServer(t, withCaptcha(func(cfg *Config) { cfg.PostCooldown = time.Hour }))
	h.addBoard("general")
	var started verifyResponse
	if status := h.post("/api/verify/request", map[string]any{
		"email": "ana@iitb.ac.in", "adult": true, "captcha": h.solveCaptcha(),
	}, &started); status != http.StatusAccepted {
		t.Fatalf("request code: status %d (%s)", status, started.Error)
	}
	status, confirmed := h.confirmCode(h.mail.last(t).Code)
	if status != http.StatusOK {
		t.Fatalf("confirm code: status %d (%s)", status, confirmed.Error)
	}
	credential := confirmed.Credential
	thread := h.createThreadWithCaptcha("general", "open")
	path := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)

	outcomes := h.burst(make([]string, burstSize), func(_ int, _ string) *http.Request {
		req := h.jsonRequest(path, map[string]any{"body": "badged burst", "badge": true})
		req.Header.Set(credentialHeader, credential)
		return req
	}, nil)
	counts := map[int]int{}
	for _, o := range outcomes {
		counts[o.status]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusTooManyRequests] != burstSize-1 {
		t.Errorf("statuses %v, want one 201 and %d 429s", counts, burstSize-1)
	}
}

// A reply is held to the same order as a thread: a captcha refusal starts no
// cooldown, a reply flood control refuses keeps its answer, and the answer is
// spent before the image is processed, so a refusal for an image that fails
// to decode says the answer went with it.
func TestReplyCaptchaAndFloodControl(t *testing.T) {
	h := captchaServer(t, func(cfg *Config) {
		cfg.PostCooldown = time.Hour
		cfg.UploadCooldown = time.Hour
	})
	h.addBoard("general", "other", "third")
	spent := func() bool { return h.lastHeader.Get(captchaSpentHeader) == "spent" }
	replies := func(board string) string {
		thread := h.createThreadWithCaptcha(board, "open")
		return fmt.Sprintf("/api/boards/%s/threads/%d/posts", board, thread.ThreadNo)
	}
	general, other, third := replies("general"), replies("other"), replies("third")

	status, refused := h.attempt(general, map[string]any{"body": "no answer"})
	expectCaptchaRefusal(t, "reply without an answer", status, refused, captchaMissing)
	var created createdPost
	if status := h.post(general, map[string]any{"body": "answered", "captcha": h.solveCaptcha()}, &created); status != http.StatusCreated {
		t.Fatalf("reply with an answer after a captcha refusal: status %d (%s), want 201: the refusal started a cooldown",
			status, created.Error)
	}

	answer := h.solveCaptcha()
	if status, refused := h.attempt(general, map[string]any{"body": "too soon", "captcha": answer}); status != http.StatusTooManyRequests || spent() {
		t.Fatalf("second reply inside the cooldown: status %d (%s), spent header %q; want 429 with the answer kept",
			status, refused.Error, h.lastHeader.Get(captchaSpentHeader))
	}
	if status := h.post(other, map[string]any{"body": "same answer, other board", "captcha": answer}, &created); status != http.StatusCreated {
		t.Errorf("an answer held back by the reply cooldown, used on another board: status %d (%s), want 201",
			status, created.Error)
	}

	status = h.upload(third, map[string]string{"body": "look", "captcha": h.solveCaptcha()},
		"cut.png", pngBytes(t, 24, 24)[:33], &refused)
	if status != http.StatusBadRequest || !strings.Contains(refused.Error, "image could not be decoded") || !spent() {
		t.Errorf("a reply whose image fails to decode: status %d (%s), spent header %q; want 400 saying the answer was spent",
			status, refused.Error, h.lastHeader.Get(captchaSpentHeader))
	}
}

// A code request is held to the same order, with the answer checked straight
// after the 18+ box: before the address is even parsed, before flood control,
// and spent only once both cooldowns let the request through.
func TestCodeRequestCaptchaAndFloodControl(t *testing.T) {
	h := verifyServer(t, withCaptcha(func(cfg *Config) {
		cfg.VerifyCooldown = time.Hour
		cfg.TrustedProxyHops = 1
	}))
	request := func(ip, email, answer string) (int, captchaRefusal) {
		body := map[string]any{"email": email, "adult": true}
		if answer != "" {
			body["captcha"] = answer
		}
		var refused captchaRefusal
		return h.postFrom(ip, "/api/verify/request", body, &refused), refused
	}

	status, refused := request("203.0.113.1", "not an address", "")
	expectCaptchaRefusal(t, "an unparseable address with no answer", status, refused, captchaMissing)
	status, refused = request("203.0.113.1", "ana@iitb.ac.in", "")
	expectCaptchaRefusal(t, "a request with no answer", status, refused, captchaMissing)
	if status, refused := request("203.0.113.1", "ana@iitb.ac.in", h.solveCaptcha()); status != http.StatusAccepted {
		t.Fatalf("the same request with an answer: status %d (%s), want 202: the captcha refusal started a cooldown",
			status, refused.Error)
	}

	answer := h.solveCaptcha()
	if status, refused := request("203.0.113.1", "ben@iitb.ac.in", answer); status != http.StatusTooManyRequests {
		t.Fatalf("a second request from the address: status %d (%s), want 429", status, refused.Error)
	}
	if status, refused := request("203.0.113.2", "ana@iitb.ac.in", answer); status != http.StatusTooManyRequests {
		t.Fatalf("a second request for the email address: status %d (%s), want 429", status, refused.Error)
	}
	if status, refused := request("203.0.113.2", "ben@iitb.ac.in", answer); status != http.StatusAccepted {
		t.Errorf("the answer both cooldowns held back, from another address for another email: status %d (%s), want 202",
			status, refused.Error)
	}
	if n := h.mail.count(); n != 2 {
		t.Errorf("%d codes sent, want 2", n)
	}

	// That answer is spent now, and both of the second address's cooldowns
	// are running. Sent again, it is told it was used, not to wait.
	status, refused = request("203.0.113.2", "ana@iitb.ac.in", answer)
	expectCaptchaRefusal(t, "a spent answer inside both cooldowns", status, refused, captchaUsed)
}

// Every instance behind the load balancer spends answers in the same table, so
// an answer spent on one is refused on another.
func TestASpentAnswerIsSpentOnEveryInstance(t *testing.T) {
	first := captchaServer(t, nil)
	first.addBoard("general")

	second, err := New(context.Background(), first.app.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("start a second instance: %v", err)
	}
	t.Cleanup(second.Close)
	srv := httptest.NewServer(second.Routes())
	t.Cleanup(srv.Close)
	other := &client{t: t, base: srv.URL, http: srv.Client()}

	answer := other.solveCaptcha() // issued by one instance, checked by the other
	var created createdPost
	if status := first.post("/api/boards/general/threads", map[string]any{"body": "first", "captcha": answer}, &created); status != http.StatusCreated {
		t.Fatalf("answer from the second instance, spent on the first: status %d (%s)", status, created.Error)
	}
	var refused captchaRefusal
	status := other.post("/api/boards/general/threads", map[string]any{"body": "again", "captcha": answer}, &refused)
	expectCaptchaRefusal(t, "the same answer on the second instance", status, refused, captchaUsed)
}

// Requests racing with one answer: exactly one of them posts.
func TestOneAnswerPostsOnceUnderConcurrency(t *testing.T) {
	h := captchaServer(t, nil)
	h.addBoard("general")
	answer := h.solveCaptcha()

	const racers = 8
	body, _ := json.Marshal(map[string]any{"body": "racing", "captcha": answer})
	statuses := make(chan int, racers)
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := http.Post(h.base+"/api/boards/general/threads", "application/json", bytes.NewReader(body))
			if err != nil {
				statuses <- 0
				return
			}
			res.Body.Close()
			statuses <- res.StatusCode
		}()
	}
	wg.Wait()
	close(statuses)

	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusForbidden] != racers-1 {
		t.Errorf("statuses %v, want one 201 and %d 403s", counts, racers-1)
	}
	if n := h.countRows("threads"); n != 1 {
		t.Errorf("%d threads stored, want 1", n)
	}
}

// holdSpend spends answer in a transaction it leaves open, as a request a
// moment ahead with the same answer would. A request checked meanwhile finds
// the answer unspent, gets through flood control's check, and then waits at
// its own spend until release commits the transaction, when it is refused as
// used. pid is the open transaction's backend, which that request waits on.
func (h *harness) holdSpend(answer string) (pid uint32, release func()) {
	h.t.Helper()
	ctx := context.Background()
	ticket, err := h.app.captcha.check(answer, time.Now())
	if err != nil {
		h.t.Fatalf("check the answer: %v", err)
	}
	tx, err := h.app.pool.Begin(ctx)
	if err != nil {
		h.t.Fatalf("begin: %v", err)
	}
	h.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		h.t.Fatalf("backend pid: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO captcha_spent (signature_hash, expires_at) VALUES ($1, $2)`,
		ticket.key, spentCaptchaExpiry(ticket.expires)); err != nil {
		h.t.Fatalf("spend the answer: %v", err)
	}
	return pid, func() {
		if err := tx.Commit(ctx); err != nil {
			h.t.Fatalf("commit the spend: %v", err)
		}
	}
}

// loseSpendRace sends a request carrying answer while holdSpend is spending
// it, lets the spend commit once the request is waiting on it, and returns how
// the request was answered. send runs on a client of its own.
func (h *harness) loseSpendRace(answer string, send func(c *client, answer string) (int, captchaRefusal)) (int, captchaRefusal, http.Header) {
	h.t.Helper()
	pid, release := h.holdSpend(answer)

	racer := &client{t: h.t, base: h.base, http: h.http}
	var (
		status  int
		refused captchaRefusal
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		status, refused = send(racer, answer)
	}()

	for deadline := time.Now().Add(10 * time.Second); ; {
		select {
		case <-done:
			h.t.Fatalf("the request was answered %d (%s) before it reached its spend", status, refused.Error)
		default:
		}
		var waiting bool
		if err := h.app.pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid)))`,
			int64(pid)).Scan(&waiting); err != nil {
			h.t.Fatalf("look for the waiting request: %v", err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatal("the request never waited on the open spend")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		h.t.Fatal("the request was not answered once the spend committed")
	}
	return status, refused, racer.lastHeader
}

// Two requests carrying one answer can both find it unspent; the second to
// spend it is refused as used, and that refusal starts no cooldown, for a post
// or a code request alike. Otherwise one answer sent to many boards at once, or
// naming many email addresses, would hold every one of them back for a whole
// cooldown at the cost of a single puzzle. Each case then shows the cooldown
// the loser did not start is real: a fresh answer goes straight through, and
// starts it.
func TestALostSpendStartsNoCooldown(t *testing.T) {
	race := func(t *testing.T, h *harness, what string, send func(c *client, answer string) (int, captchaRefusal), ok int) {
		t.Helper()
		status, refused, header := h.loseSpendRace(h.solveCaptcha(), send)
		expectCaptchaRefusal(t, what+" that lost the spend", status, refused, captchaUsed)
		if header.Get(captchaSpentHeader) != "" {
			t.Errorf("%s that lost the spend: the refusal says the answer was spent by it", what)
		}
		if status, refused := send(h.client, h.solveCaptcha()); status != ok {
			t.Fatalf("%s with a fresh answer: status %d (%s), want %d: the lost spend started a cooldown",
				what, status, refused.Error, ok)
		}
		if status, refused := send(h.client, h.solveCaptcha()); status != http.StatusTooManyRequests {
			t.Errorf("%s again: status %d (%s), want 429 from the cooldown the fresh answer started",
				what, status, refused.Error)
		}
	}

	t.Run("thread", func(t *testing.T) {
		h := captchaServer(t, func(cfg *Config) {
			cfg.ThreadCooldown = time.Hour
			cfg.UploadCooldown = time.Hour
		})
		h.addBoard("general")
		image := pngBytes(t, 24, 24)
		// An image thread, so the upload cooldown is in play as well.
		race(t, h, "an image thread", func(c *client, answer string) (int, captchaRefusal) {
			var refused captchaRefusal
			status := c.upload("/api/boards/general/threads", map[string]string{"body": "look", "captcha": answer},
				"look.png", image, &refused)
			return status, refused
		}, http.StatusCreated)
	})

	t.Run("reply", func(t *testing.T) {
		h := captchaServer(t, func(cfg *Config) { cfg.PostCooldown = time.Hour })
		h.addBoard("general")
		thread := h.createThreadWithCaptcha("general", "open")
		path := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)
		race(t, h, "a reply", func(c *client, answer string) (int, captchaRefusal) {
			return c.attempt(path, map[string]any{"body": "me too", "captcha": answer})
		}, http.StatusCreated)
	})

	t.Run("code request", func(t *testing.T) {
		h := verifyServer(t, withCaptcha(func(cfg *Config) { cfg.VerifyCooldown = time.Hour }))
		race(t, h.harness, "a code request", func(c *client, answer string) (int, captchaRefusal) {
			return c.attempt("/api/verify/request", map[string]any{"email": "ana@iitb.ac.in", "adult": true, "captcha": answer})
		}, http.StatusAccepted)
		if n := h.mail.count(); n != 1 {
			t.Errorf("%d codes sent, want the one for the fresh answer", n)
		}
	})
}

func TestCaptchaRefusalReasons(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{errCaptchaExpired, captchaExpired},
		{errCaptchaInvalid, captchaInvalid},
		{errors.New("anything else"), captchaInvalid},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			h := testServer(t, func(cfg *Config) { cfg.captcha = &fakeCaptcha{err: tc.err} })
			h.addBoard("general")
			var refused captchaRefusal
			status := h.post("/api/boards/general/threads", map[string]any{"body": "hi", "captcha": "eyJ9"}, &refused)
			expectCaptchaRefusal(t, "thread", status, refused, tc.reason)
		})
	}
}

// An address asking for challenges faster than people post gets harder ones,
// and past captchaChallengeLimit in a minute it gets none.
func TestCaptchaChallengesPerAddress(t *testing.T) {
	fake := &fakeCaptcha{}
	h := testServer(t, func(cfg *Config) {
		cfg.captcha = fake
		cfg.TrustedProxyHops = 1
	})
	now := time.Unix(1_800_000_000, 0).Add(15 * time.Second)
	meterAt(h.app.captchaMeter, &now)

	var out map[string]any
	from := func(ip string) (int, http.Header) {
		req, err := http.NewRequest(http.MethodGet, h.base+"/api/captcha", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("X-Forwarded-For", ip)
		out = nil
		status := h.send(req, &out)
		return status, h.lastHeader
	}
	for n := 1; n <= captchaChallengeLimit; n++ {
		if status, _ := from("203.0.113.9"); status != http.StatusOK {
			t.Fatalf("challenge %d: status %d, want 200 up to %d", n, status, captchaChallengeLimit)
		}
	}
	status, header := from("203.0.113.9")
	if status != http.StatusTooManyRequests || header.Get("Retry-After") != "45" || out["retry_after"] != 45.0 {
		t.Errorf("challenge past the limit: status %d, Retry-After %q, retry_after %v, want 429 after the 45s left",
			status, header.Get("Retry-After"), out["retry_after"])
	}
	if status, _ := from("198.51.100.4"); status != http.StatusOK {
		t.Errorf("another address: status %d, want 200", status)
	}

	scales := fake.issued()
	if len(scales) != captchaChallengeLimit+1 {
		t.Fatalf("%d challenges issued, want %d", len(scales), captchaChallengeLimit+1)
	}
	for i, scale := range scales[:captchaChallengeLimit] {
		if want := captchaScale(i + 1); scale != want {
			t.Fatalf("challenge %d issued at scale %d, want %d", i+1, scale, want)
		}
	}
	if last := scales[captchaChallengeLimit]; last != 1 {
		t.Errorf("another address's first challenge at scale %d, want 1", last)
	}

	// An IPv6 client is counted by its /64, the block one connection is
	// usually given: a second address in it carries on the first's count, and
	// an address in the next block starts its own.
	for range captchaFreeChallenges {
		from("2001:db8:1:2::1")
	}
	from("2001:db8:1:2::2")
	from("2001:db8:1:3::1")
	scales = fake.issued()[captchaChallengeLimit+1:]
	if got, want := scales[captchaFreeChallenges], captchaScale(captchaFreeChallenges+1); got != want {
		t.Errorf("a second address in the /64 issued at scale %d, want %d: the block's count carried on", got, want)
	}
	if got := scales[captchaFreeChallenges+1]; got != 1 {
		t.Errorf("an address in the next /64 issued at scale %d, want 1", got)
	}
}

// A challenge is fresh on every request and personal to nobody: no cookie, and
// nothing a cache may keep.
func TestCaptchaChallengesAreNotCached(t *testing.T) {
	h := captchaServer(t, nil)
	res, body := h.fetch("/api/captcha", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d (%s)", res.StatusCode, body)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if cookies := res.Header.Values("Set-Cookie"); len(cookies) != 0 {
		t.Errorf("GET /api/captcha set cookies %v, want none", cookies)
	}
	var challenge captchaChallenge
	if err := json.Unmarshal(body, &challenge); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if !wellFormedCaptcha(challenge.Parameters) {
		t.Errorf("challenge %s is not one the server would accept", body)
	}
}

// A spent answer is stored with its expiry rounded up to the next UTC
// midnight, so the only date the table stores is the day. Rows past
// that are deleted a batch at a time by later spends and all at once by the
// daily sweep, which on a quiet server is the one that clears them.
func TestSpentCaptchasArePruned(t *testing.T) {
	h := testServer(t, nil)
	ctx := context.Background()
	// Half a minute into a UTC day, so yesterday's rows are inside the margin.
	now := time.Date(2026, 10, 15, 0, 0, 30, 0, time.UTC)
	key := func(s string) []byte { k := sha256.Sum256([]byte(s)); return k[:] }

	spend := func(name string, expires time.Time, want bool) {
		t.Helper()
		fresh, err := h.app.store.SpendCaptcha(ctx, key(name), expires, now)
		if err != nil {
			t.Fatalf("spend %s: %v", name, err)
		}
		if fresh != want {
			t.Errorf("spend %s: fresh %t, want %t", name, fresh, want)
		}
	}
	// More rows from the day before yesterday than one spend prunes.
	for i := range captchaPruneBatch + 5 {
		h.exec(`INSERT INTO captcha_spent (signature_hash, expires_at) VALUES ($1, $2)`,
			key(fmt.Sprint("old ", i)), now.Add(-24*time.Hour).Truncate(24*time.Hour))
	}
	spend("yesterday", now.Add(-12*time.Hour), true)
	if n := h.countRows("captcha_spent"); n != 5+1 {
		t.Errorf("%d spent answers kept after one spend, want 6: %d of the old ones pruned, and the new one", n, captchaPruneBatch)
	}
	sweep := func(asOf time.Time, deleted, left int) {
		t.Helper()
		report, err := h.app.sweep(ctx, asOf, sweepBatchSize)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if report.SpentCaptchas != deleted || h.countRows("captcha_spent") != left {
			t.Errorf("sweep at %s deleted %d spent answers and left %d, want %d deleted and %d left",
				asOf, report.SpentCaptchas, h.countRows("captcha_spent"), deleted, left)
		}
	}
	// The rest of the old ones go; yesterday's is kept for the margin.
	sweep(now, 5, 1)

	spend("live", now.Add(10*time.Minute), true)
	rows, err := h.app.pool.Query(ctx, `SELECT expires_at FROM captcha_spent`)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range stored {
		if at.UTC() != at.UTC().Truncate(24*time.Hour) {
			t.Errorf("a spent answer is stored expiring at %s, not at a UTC midnight", at.UTC())
		}
	}

	// Yesterday's answer stays spent until its day is over and a minute more.
	spend("yesterday", now.Add(-12*time.Hour), false)
	sweep(now.Add(2*time.Minute), 1, 1)
	spend("live", now.Add(10*time.Minute), false)
}

// A spend prunes only the rows no other spend is already deleting (SKIP
// LOCKED), so posts arriving together never queue behind each other's pruning.
func TestASpendDoesNotWaitOnAnotherSpendsPruning(t *testing.T) {
	h := testServer(t, nil)
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	key := func(s string) []byte { k := sha256.Sum256([]byte(s)); return k[:] }
	for _, name := range []string{"held", "free"} {
		h.exec(`INSERT INTO captcha_spent (signature_hash, expires_at) VALUES ($1, $2)`,
			key(name), now.Truncate(24*time.Hour).Add(-24*time.Hour))
	}

	// Another spend, part way through its pruning, holds a row it is deleting.
	tx, err := h.app.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `SELECT 1 FROM captcha_spent WHERE signature_hash = $1 FOR UPDATE`, key("held")); err != nil {
		t.Fatalf("hold the row: %v", err)
	}

	waiting, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if fresh, err := h.app.store.SpendCaptcha(waiting, key("new"), now.Add(time.Hour), now); err != nil || !fresh {
		t.Fatalf("a spend while another holds a row it is pruning: fresh %t, error %v; want it to go ahead at once", fresh, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var held, free bool
	if err := h.app.pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM captcha_spent WHERE signature_hash = $1),
		EXISTS (SELECT 1 FROM captcha_spent WHERE signature_hash = $2)`, key("held"), key("free")).Scan(&held, &free); err != nil {
		t.Fatal(err)
	}
	if !held || free {
		t.Errorf("after the spend: held row kept %t, free row kept %t; want the held one left to its holder and the free one pruned", held, free)
	}
}

// The captcha leaves nothing behind that could tie a post to who wrote it: no
// table holds an answer or its challenge, the spent-answer table holds only a
// hash, and no log line carries any of it.
func TestCaptchaLeavesNoTrace(t *testing.T) {
	var logs bytes.Buffer
	h := testServerLogging(t, withCaptcha(nil), &logs)
	h.addBoard("general")

	var challenge captchaChallenge
	h.get("/api/captcha", &challenge)
	answer := solveChallenge(t, challenge)
	derived := decodeTestPayload(t, answer).Solution.DerivedKey
	created := h.createThreadWithCaptcha("general", "first")
	var created2 createdPost
	if status := h.post(fmt.Sprintf("/api/boards/general/threads/%d/posts", created.ThreadNo),
		map[string]any{"body": "traceless", "captcha": answer}, &created2); status != http.StatusCreated {
		t.Fatalf("reply: status %d (%s)", status, created2.Error)
	}
	h.post("/api/boards/general/threads", map[string]any{"body": "replay", "captcha": answer}, &captchaRefusal{})

	signature, _ := hex.DecodeString(challenge.Signature)
	nonce, _ := hex.DecodeString(challenge.Parameters.Nonce)
	key, _ := hex.DecodeString(derived)
	needles := map[string][]byte{
		"answer":    []byte(answer),
		"signature": signature,
		"nonce":     nonce,
		"key":       key,
	}
	for _, table := range h.tables() {
		for name, needle := range needles {
			if n := h.rowsContaining(table, needle); n != 0 {
				t.Errorf("%s holds the %s in %d rows", table, name, n)
			}
		}
	}
	spent := sha256.Sum256(signature)
	if n := h.rowsContaining("captcha_spent", spent[:]); n != 1 {
		t.Errorf("captcha_spent holds the signature's hash in %d rows, want 1", n)
	}
	if n := h.foreignKeys("captcha_spent"); n != 0 {
		t.Errorf("captcha_spent has %d foreign keys, want none", n)
	}

	for name, needle := range map[string]string{
		"answer":    answer,
		"signature": challenge.Signature,
		"nonce":     challenge.Parameters.Nonce,
		"key":       derived,
	} {
		if strings.Contains(logs.String(), needle) {
			t.Errorf("the log holds the %s", name)
		}
	}
}

// meterHolds lists the addresses the captcha meter holds.
func meterHolds(m *captchaMeter) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedKeys(m.counts)
}

// The captcha meter holds client addresses, so it is swept with the cooldowns:
// an address that asked for a challenge is gone within a minute of its window
// ending, with nothing else sent, and an idle instance's next request clears
// it before it is handled. The meter's clock is pinned fifteen seconds into a
// window, so a sweep cannot find the window over before the test looks.
func TestCaptchaMeterForgetsAddresses(t *testing.T) {
	t.Run("on a quiet server", func(t *testing.T) {
		h := captchaServer(t, func(cfg *Config) {
			cfg.TrustedProxyHops = 1
			cfg.cooldownSweepEvery = time.Millisecond
		})
		now := time.Unix(1_800_000_000, 0).Add(15 * time.Second)
		meterAt(h.app.captchaMeter, &now)
		const address = "198.51.100.9"
		req, _ := http.NewRequest(http.MethodGet, h.base+"/api/captcha", nil)
		req.Header.Set("X-Forwarded-For", address)
		if status := h.send(req, nil); status != http.StatusOK {
			t.Fatalf("challenge: status %d", status)
		}
		if got := meterHolds(h.app.captchaMeter); len(got) != 1 || got[0] != address {
			t.Fatalf("the meter holds %v, want the address", got)
		}
		later := now.Add(captchaWindow)
		meterAt(h.app.captchaMeter, &later)
		waitUntil(t, "the meter forgets the address", func() bool { return len(meterHolds(h.app.captchaMeter)) == 0 })
	})
	t.Run("before the next request", func(t *testing.T) {
		const every = 20 * time.Millisecond
		h := captchaServer(t, func(cfg *Config) { cfg.cooldownSweepEvery = every })
		now := time.Unix(1_800_000_000, 0).Add(15 * time.Second)
		meterAt(h.app.captchaMeter, &now)
		h.solveCaptcha()
		h.app.stopCooldownSweep()
		later := now.Add(captchaWindow)
		meterAt(h.app.captchaMeter, &later)
		time.Sleep(every)
		if got := meterHolds(h.app.captchaMeter); len(got) != 1 {
			t.Fatalf("while idle the meter holds %v, want the address still there for a request to sweep", got)
		}
		if status := h.do(http.MethodGet, "/healthz", nil, nil); status != http.StatusOK {
			t.Fatalf("healthz: status %d", status)
		}
		if got := meterHolds(h.app.captchaMeter); len(got) != 0 {
			t.Errorf("after the next request the meter holds %v, want nothing", got)
		}
	})
}
