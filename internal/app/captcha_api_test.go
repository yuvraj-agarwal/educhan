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
	// flood control does, so the answer is tried on another board. It is
	// refused as used before flood control, so the fresh answer that follows
	// posts there at once.
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

	from := func(ip string) (int, http.Header) {
		req, err := http.NewRequest(http.MethodGet, h.base+"/api/captcha", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("X-Forwarded-For", ip)
		var out map[string]any
		status := h.send(req, &out)
		return status, h.lastHeader
	}
	for n := 1; n <= captchaChallengeLimit; n++ {
		if status, _ := from("203.0.113.9"); status != http.StatusOK {
			t.Fatalf("challenge %d: status %d, want 200 up to %d", n, status, captchaChallengeLimit)
		}
	}
	status, header := from("203.0.113.9")
	if status != http.StatusTooManyRequests || header.Get("Retry-After") != "45" {
		t.Errorf("challenge past the limit: status %d, Retry-After %q, want 429 after the 45s left", status, header.Get("Retry-After"))
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

// Spent answers are kept until a minute after their challenge would have
// expired, and pruned by the next spend after that.
func TestSpentCaptchasArePruned(t *testing.T) {
	h := testServer(t, nil)
	ctx := context.Background()
	now := time.Now()
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
	spend("long gone", now.Add(-2*time.Minute), true)
	spend("just gone", now.Add(-30*time.Second), true)
	spend("live", now.Add(10*time.Minute), true)
	if n := h.countRows("captcha_spent"); n != 2 {
		t.Errorf("%d spent answers kept, want 2: the one past its margin pruned", n)
	}
	spend("live", now.Add(10*time.Minute), false)
	spend("just gone", now.Add(-30*time.Second), false)
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
// it before it is handled.
func TestCaptchaMeterForgetsAddresses(t *testing.T) {
	t.Run("on a quiet server", func(t *testing.T) {
		h := captchaServer(t, func(cfg *Config) {
			cfg.TrustedProxyHops = 1
			cfg.cooldownSweepEvery = time.Millisecond
		})
		const address = "198.51.100.9"
		req, _ := http.NewRequest(http.MethodGet, h.base+"/api/captcha", nil)
		req.Header.Set("X-Forwarded-For", address)
		if status := h.send(req, nil); status != http.StatusOK {
			t.Fatalf("challenge: status %d", status)
		}
		if got := meterHolds(h.app.captchaMeter); len(got) != 1 || got[0] != address {
			t.Fatalf("the meter holds %v, want the address", got)
		}
		later := time.Now().Add(captchaWindow)
		meterAt(h.app.captchaMeter, &later)
		waitUntil(t, "the meter forgets the address", func() bool { return len(meterHolds(h.app.captchaMeter)) == 0 })
	})
	t.Run("before the next request", func(t *testing.T) {
		const every = 20 * time.Millisecond
		h := captchaServer(t, func(cfg *Config) { cfg.cooldownSweepEvery = every })
		h.solveCaptcha()
		h.app.stopCooldownSweep()
		later := time.Now().Add(captchaWindow)
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
