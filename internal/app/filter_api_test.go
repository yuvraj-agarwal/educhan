package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Integration tests for the content filter's API: the 422 refusals, the 202
// for a held post, and the held-post invariant — a held post appears in no
// public view and changes nothing public until a moderator approves it.

func threadsPath(board string) string { return "/api/boards/" + board + "/threads" }

func repliesPath(board string, no int64) string {
	return fmt.Sprintf("/api/boards/%s/threads/%d/posts", board, no)
}

// rawGet fetches a path and returns its status and body as text.
func (c *client) rawGet(path string) (int, string) {
	c.t.Helper()
	res, err := c.http.Get(c.base + path)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("read %s: %v", path, err)
	}
	return res.StatusCode, string(body)
}

// postJSON sends a JSON post and decodes the answer as a generic object, so a
// test can check exactly which keys came back.
func (c *client) postJSON(path string, body map[string]any) (int, map[string]any) {
	c.t.Helper()
	var out map[string]any
	return c.post(path, body, &out), out
}

// uploadMap is upload with the answer decoded fresh, as postJSON's is.
func (c *client) uploadMap(path string, fields map[string]string, filename string, file []byte) (int, map[string]any) {
	c.t.Helper()
	var out map[string]any
	return c.upload(path, fields, filename, file, &out), out
}

// postWithCredentialMap is postWithCredential with the answer decoded fresh.
func (c *client) postWithCredentialMap(credential, path string, body map[string]any) (int, map[string]any) {
	c.t.Helper()
	var out map[string]any
	return c.postWithCredential(credential, path, body, &out), out
}

func requireKeys(t *testing.T, got map[string]any, keys ...string) {
	t.Helper()
	var have []string
	for k := range got {
		have = append(have, k)
	}
	sort.Strings(have)
	sort.Strings(keys)
	if strings.Join(have, ",") != strings.Join(keys, ",") {
		t.Fatalf("response has keys %v, want exactly %v (%v)", have, keys, got)
	}
}

// requireBlock checks a 422 block refusal against the contract.
func requireBlock(t *testing.T, status int, out map[string]any, rule, field, match string) {
	t.Helper()
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d (%v), want 422", status, out)
	}
	requireKeys(t, out, "error", "outcome", "rule", "field", "match")
	if out["outcome"] != "block" || out["rule"] != rule || out["field"] != field || out["match"] != match {
		t.Fatalf("refusal %v, want a %s block of %q in %s", out, rule, match, field)
	}
	msg, _ := out["error"].(string)
	if msg == "" || !strings.HasSuffix(msg, ".") || strings.Count(msg, ". ") > 0 {
		t.Fatalf("error should be one plain sentence: %q", msg)
	}
}

// requireHeld checks a 202 held acknowledgement: that it was received, the
// board, the thread for a reply, and nothing else — no number, no reason.
func requireHeld(t *testing.T, status int, out map[string]any, board string, threadNo int64) {
	t.Helper()
	if status != http.StatusAccepted {
		t.Fatalf("status %d (%v), want 202", status, out)
	}
	if threadNo == 0 {
		requireKeys(t, out, "held", "board")
	} else {
		requireKeys(t, out, "held", "board", "thread_no")
		if out["thread_no"] != float64(threadNo) {
			t.Fatalf("thread_no %v, want %d", out["thread_no"], threadNo)
		}
	}
	if out["held"] != true || out["board"] != board {
		t.Fatalf("held acknowledgement %v", out)
	}
}

func (h *harness) postCount() int {
	h.t.Helper()
	var n int
	if err := h.app.pool.QueryRow(context.Background(), `SELECT count(*) FROM posts`).Scan(&n); err != nil {
		h.t.Fatalf("count posts: %v", err)
	}
	return n
}

func TestBlockedPostsAreRefusedWithWhatToRemove(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("blk")
	thread := h.createThread("blk", "", "", "clean opening post")
	before := h.postCount()

	status, out := h.postJSON(threadsPath("blk"), map[string]any{"body": "text me on 98765 43210 pls"})
	requireBlock(t, status, out, ruleContact, fieldBody, "98765 43210")
	status, out = h.postJSON(repliesPath("blk", thread.ThreadNo), map[string]any{"body": "add me on snap"})
	requireBlock(t, status, out, ruleContact, fieldBody, "add me on snap")
	status, out = h.postJSON(repliesPath("blk", thread.ThreadNo), map[string]any{"body": "NI: AB 12 34 56 C"})
	requireBlock(t, status, out, ruleIDNumber, fieldBody, "AB 12 34 56 C")

	// The subject and the shown part of the name are read too.
	status, out = h.postJSON(threadsPath("blk"), map[string]any{"subject": "mail me ana.k@gmail.com", "body": "hi"})
	requireBlock(t, status, out, ruleContact, fieldSubject, "ana.k@gmail.com")
	status, out = h.postJSON(repliesPath("blk", thread.ThreadNo), map[string]any{"name": "insta: x_y_z", "body": "hi"})
	requireBlock(t, status, out, ruleContact, fieldName, "insta: x_y_z")

	// The tripcode secret is not: it is never shown, so it is nobody's
	// contact detail, and it is never quoted back.
	var created createdPost
	if status := h.post(repliesPath("blk", thread.ThreadNo),
		map[string]any{"name": "ana#9876543210", "body": "hi"}, &created); status != http.StatusCreated {
		t.Fatalf("a tripcode secret shaped like a phone number: status %d (%s)", status, created.Error)
	}
	detail := fetchThread(t, h, "blk", thread.ThreadNo)
	last := detail.Posts[len(detail.Posts)-1]
	if last.Name != "ana" || last.Tripcode == "" {
		t.Fatalf("the tripcode post: %+v", last)
	}

	if got := h.postCount(); got != before+1 {
		t.Fatalf("posts went from %d to %d; refused posts must store nothing", before, got)
	}
}

func TestRefusedPostsCostNoCooldownAndStoreNoFile(t *testing.T) {
	h := testServer(t, func(cfg *Config) {
		cfg.ThreadCooldown = time.Hour
		cfg.PostCooldown = time.Hour
		cfg.UploadCooldown = time.Hour
		cfg.RefusalLimit = 100
	})
	h.addBoard("cool")
	img := pngBytes(t, 16, 16)

	// A blocked thread with an image, then a support refusal: neither starts a
	// cooldown or leaves a file.
	status, out := h.uploadMap(threadsPath("cool"), map[string]string{"body": "wa.me/919876543210"}, "a.png", img)
	requireBlock(t, status, out, ruleContact, fieldBody, "wa.me/919876543210")
	status, out = h.uploadMap(threadsPath("cool"), map[string]string{"body": "i want to die"}, "a.png", img)
	if status != http.StatusUnprocessableEntity || out["outcome"] != "support" {
		t.Fatalf("support refusal: %d %v", status, out)
	}
	if files := filesUnder(t, h.app.cfg.UploadDir); len(files) != 0 {
		t.Fatalf("a refused post left files behind: %v", files)
	}

	// So the clean post straight after goes through, image and all...
	var created createdPost
	if status := h.upload(threadsPath("cool"), map[string]string{"body": "clean"}, "a.png", img, &created); status != http.StatusCreated {
		t.Fatalf("the clean post after the refusals: status %d (%s)", status, created.Error)
	}
	// ...and the one after that is the one flood control stops, which shows
	// the cooldowns were on all along.
	if status, _ := h.postJSON(threadsPath("cool"), map[string]any{"body": "again"}); status != http.StatusTooManyRequests {
		t.Fatalf("second clean thread: status %d, want 429", status)
	}
	// A post in a cooldown is told so before it is read: what it would have
	// been refused for is not checked, quoted back, or counted against the
	// refusal budget.
	refusals := refusalsCounted(h.app.refusals)
	for _, body := range []string{"call 9876543210", "i want to die", "go die"} {
		if status, out := h.postJSON(threadsPath("cool"), map[string]any{"body": body}); status != http.StatusTooManyRequests {
			t.Fatalf("%q in a cooldown: status %d (%v), want 429", body, status, out)
		} else if _, quoted := out["match"]; quoted || out["outcome"] != nil {
			t.Fatalf("%q in a cooldown was read: %v", body, out)
		}
	}
	if got := refusalsCounted(h.app.refusals); got != refusals {
		t.Fatalf("posts turned away by a cooldown counted %d refusals", got-refusals)
	}

	// The same holds for replies.
	status, out = h.uploadMap(repliesPath("cool", created.ThreadNo), map[string]string{"body": "my snap is ana_23"}, "", nil)
	requireBlock(t, status, out, ruleContact, fieldBody, "my snap is ana_23")
	if status, out := h.postJSON(repliesPath("cool", created.ThreadNo), map[string]any{"body": "clean reply"}); status != http.StatusCreated {
		t.Fatalf("the clean reply after the refusal: status %d (%v)", status, out)
	}
	if status, _ := h.postJSON(repliesPath("cool", created.ThreadNo), map[string]any{"body": "again"}); status != http.StatusTooManyRequests {
		t.Fatalf("second clean reply: status %d, want 429", status)
	}
	if status, out := h.postJSON(repliesPath("cool", created.ThreadNo), map[string]any{"body": "call 9876543210"}); status != http.StatusTooManyRequests {
		t.Fatalf("a contact detail in a reply cooldown: status %d (%v), want 429", status, out)
	}
	if got := refusalsCounted(h.app.refusals); got != refusals+1 {
		t.Fatalf("refusals counted went from %d to %d, want the one reply refusal", refusals, got)
	}
}

// refusalsCounted is the number of refusals a budget is holding, across every
// client.
func refusalsCounted(b *refusalBudget) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.seen {
		n += c.n
	}
	return n
}

// A refusal costs no cooldown, so it is counted instead: a client sent back
// more often than a person correcting a post would be is answered 429 without
// its posts being checked, clean or not, until its minute is up. Other clients
// are not affected, and posts that go through count for nothing.
func TestRefusalsAreBudgetedPerClient(t *testing.T) {
	h := testServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.RefusalLimit = 3
	})
	h.addBoard("budget")
	thread := h.createThread("budget", "", "", "op")
	const sender, other = "198.51.100.20", "198.51.100.21"

	refused := []map[string]any{
		{"body": "wa.me/919876543210"},
		{"body": "i want to die"},
		{"body": "my snap is ana_23"},
	}
	for i, body := range refused {
		var out map[string]any
		if status := h.postFrom(sender, threadsPath("budget"), body, &out); status != http.StatusUnprocessableEntity {
			t.Fatalf("refusal %d: status %d (%v), want 422", i+1, status, out)
		}
	}
	posts := h.postCount()
	for _, path := range []string{threadsPath("budget"), repliesPath("budget", thread.ThreadNo)} {
		for _, body := range []map[string]any{{"body": "a clean post"}, {"body": "wa.me/919876543210"}} {
			var out map[string]any
			if status := h.postFrom(sender, path, body, &out); status != http.StatusTooManyRequests {
				t.Fatalf("%s %v past the budget: status %d (%v), want 429", path, body, status, out)
			}
			if secs, _ := out["retry_after"].(float64); secs < 1 || secs > 60 {
				t.Errorf("retry_after = %v, want the rest of the minute", out["retry_after"])
			}
		}
	}
	if got := h.postCount(); got != posts {
		t.Fatalf("posts went from %d to %d while the budget was spent", posts, got)
	}

	// Another client is checked as usual, and posting clean posts spends
	// nothing.
	for i := 0; i < 5; i++ {
		var created createdPost
		if status := h.postFrom(other, repliesPath("budget", thread.ThreadNo),
			map[string]any{"body": fmt.Sprintf("clean %d", i)}, &created); status != http.StatusCreated {
			t.Fatalf("another client's clean reply %d: status %d (%s)", i, status, created.Error)
		}
	}

	// A minute on, the sender is checked again.
	later := time.Now().Add(time.Minute)
	setBudgetClock(h.app.refusals, func() time.Time { return later })
	var created createdPost
	if status := h.postFrom(sender, repliesPath("budget", thread.ThreadNo),
		map[string]any{"body": "back again"}, &created); status != http.StatusCreated {
		t.Fatalf("after the minute: status %d (%s), want 201", status, created.Error)
	}
}

func TestHeldPostsTakeTheCooldown(t *testing.T) {
	h := testServer(t, func(cfg *Config) { cfg.PostCooldown = time.Hour })
	h.addBoard("hc")
	thread := h.createThread("hc", "", "", "op")
	status, out := h.postJSON(repliesPath("hc", thread.ThreadNo), map[string]any{"body": "go die"})
	requireHeld(t, status, out, "hc", thread.ThreadNo)
	if status, _ := h.postJSON(repliesPath("hc", thread.ThreadNo), map[string]any{"body": "fine"}); status != http.StatusTooManyRequests {
		t.Fatalf("a held post should start the cooldown like any other: status %d", status)
	}
}

func TestSupportNeedsSupportSeen(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("sup")
	thread := h.createThread("sup", "", "", "op")

	for _, path := range []string{threadsPath("sup"), repliesPath("sup", thread.ThreadNo)} {
		status, out := h.postJSON(path, map[string]any{"body": "honestly i just want to die"})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d, want 422", path, status)
		}
		requireKeys(t, out, "error", "outcome")
		if out["outcome"] != "support" || out["error"] != supportMessage {
			t.Fatalf("%s: %v", path, out)
		}
		// Sent again having seen it, the post goes through.
		status, out = h.postJSON(path, map[string]any{"body": "honestly i just want to die", "support_seen": true})
		if status != http.StatusCreated {
			t.Fatalf("%s with support_seen: status %d (%v)", path, status, out)
		}
	}

	// Multipart reads it like the other flags.
	status, out := h.uploadMap(repliesPath("sup", thread.ThreadNo), map[string]string{"body": "kms", "support_seen": "on"}, "", nil)
	if status != http.StatusCreated {
		t.Fatalf("multipart support_seen: status %d (%v)", status, out)
	}
	status, out = h.uploadMap(repliesPath("sup", thread.ThreadNo), map[string]string{"body": "kms"}, "", nil)
	if status != http.StatusUnprocessableEntity || out["outcome"] != "support" {
		t.Fatalf("multipart without support_seen: status %d (%v)", status, out)
	}

	// support_seen gets a post past the support screen and nothing else.
	status, out = h.postJSON(repliesPath("sup", thread.ThreadNo),
		map[string]any{"body": "i want to die, 9876543210", "support_seen": true})
	requireBlock(t, status, out, ruleContact, fieldBody, "9876543210")
	status, out = h.postJSON(repliesPath("sup", thread.ThreadNo),
		map[string]any{"body": "i want to die. kys", "support_seen": true})
	requireHeld(t, status, out, "sup", thread.ThreadNo)
}

// publicWalk fetches every public view of a board — the board list and the
// board, every index page, the catalog, the archive, every thread any of
// them lists, and the resolution of every post in those threads — and
// returns what they said, concatenated.
func publicWalk(t *testing.T, h *harness, board string) string {
	t.Helper()
	var all strings.Builder
	read := func(path string) string {
		status, body := h.rawGet(path)
		if status != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, status)
		}
		all.WriteString(body)
		all.WriteByte('\n')
		return body
	}
	read("/api/boards")
	read("/api/boards/" + board)

	threads := map[int64]bool{}
	index := fetchIndex(t, h, board, 1)
	for page := 1; page <= index.Pages; page++ {
		read(fmt.Sprintf("%s?page=%d", threadsPath(board), page))
		for _, s := range fetchIndex(t, h, board, page).Threads {
			threads[s.No] = true
		}
	}
	read("/api/boards/" + board + "/catalog")
	for _, no := range catalogThreadNos(t, h, board) {
		threads[no] = true
	}
	read("/api/boards/" + board + "/archive")
	for _, a := range fetchArchive(t, h, board).Threads {
		threads[a.No] = true
	}
	for no := range threads {
		read(fmt.Sprintf("/api/boards/%s/threads/%d", board, no))
		for _, p := range fetchThread(t, h, board, no).Posts {
			read(fmt.Sprintf("/api/boards/%s/posts/%d", board, p.No))
		}
		read(fmt.Sprintf("/api/threads/%d", h.threadID(board, no)))
	}
	return all.String()
}

type heldQueue struct {
	Posts []HeldPost `json:"posts"`
	Total int        `json:"total"`
}

func (h *harness) heldQueue() []HeldPost {
	h.t.Helper()
	var q heldQueue
	if status := h.mod(http.MethodGet, "/api/held", "Authorization", "Bearer "+testModKey, &q); status != http.StatusOK {
		h.t.Fatalf("GET /api/held: status %d", status)
	}
	return q.Posts
}

func (h *harness) approve(board string, threadNo, postNo int64) (int, map[string]any) {
	h.t.Helper()
	var out map[string]any
	path := fmt.Sprintf("/api/boards/%s/threads/%d/posts/%d/approve", board, threadNo, postNo)
	return h.mod(http.MethodPost, path, "Authorization", "Bearer "+testModKey, &out), out
}

// heldNumber finds the number of the held post whose body contains marker.
func heldNumber(t *testing.T, queue []HeldPost, marker string) HeldPost {
	t.Helper()
	for _, p := range queue {
		if strings.Contains(p.Body, marker) {
			return p
		}
	}
	t.Fatalf("no held post contains %q in %+v", marker, queue)
	return HeldPost{}
}

func TestHeldPostsAppearInNoPublicView(t *testing.T) {
	h := modServer(t)
	h.addBoard("hold")
	img := pngBytes(t, 12, 12)

	older := h.createThread("hold", "older", "", "older thread publicmarkerone")
	newer := h.createThread("hold", "newer", "", "newer thread publicmarkertwo")
	quoted := h.reply("hold", older.ThreadNo, "", "a reply to be quoted", false)

	snapshot := func(no int64) Thread { return fetchThread(t, h, "hold", no).Thread }
	olderBefore, newerBefore := snapshot(older.ThreadNo), snapshot(newer.ThreadNo)
	orderBefore := liveThreadNos(t, h, "hold")

	// A held reply with an image, quoting a published post; a held thread
	// with an image; and a held reply with no image.
	status, out := h.uploadMap(repliesPath("hold", older.ThreadNo), map[string]string{
		"body": fmt.Sprintf(">>%d\nhow old are you? dm me heldmarkeralpha", quoted.No),
	}, "held.png", img)
	requireHeld(t, status, out, "hold", older.ThreadNo)
	status, out = h.uploadMap(threadsPath("hold"), map[string]string{
		"subject": "heldsubjectbravo", "body": "I'm going to shoot up the school tomorrow heldmarkerbravo",
	}, "held.png", img)
	requireHeld(t, status, out, "hold", 0)
	status, out = h.postJSON(repliesPath("hold", newer.ThreadNo), map[string]any{"body": "go die heldmarkercharlie"})
	requireHeld(t, status, out, "hold", newer.ThreadNo)

	queue := h.heldQueue()
	if len(queue) != 3 {
		t.Fatalf("held queue has %d posts, want 3", len(queue))
	}
	alpha, bravo, charlie := heldNumber(t, queue, "heldmarkeralpha"), heldNumber(t, queue, "heldmarkerbravo"),
		heldNumber(t, queue, "heldmarkercharlie")

	// A published reply quoting the held ones by number, saged so that the
	// bump order below is the held posts' doing alone.
	quoting := h.reply("hold", newer.ThreadNo, "",
		fmt.Sprintf(">>%d >>%d >>%d publicmarkerthree", alpha.No, bravo.No, charlie.No), true)

	walk := publicWalk(t, h, "hold")
	// Positive control: the walk sees what is published.
	for _, marker := range []string{"publicmarkerone", "publicmarkertwo", "publicmarkerthree"} {
		if !strings.Contains(walk, marker) {
			t.Fatalf("the walk missed published text %q", marker)
		}
	}
	for _, hidden := range []string{"heldmarkeralpha", "heldmarkerbravo", "heldmarkercharlie", "heldsubjectbravo",
		alpha.Attachment.URL, alpha.Attachment.ThumbURL, bravo.Attachment.URL, bravo.Attachment.ThumbURL} {
		if strings.Contains(walk, hidden) {
			t.Errorf("a public view shows %q from a held post", hidden)
		}
	}

	// The held numbers are nowhere a reader can reach.
	for _, no := range []int64{alpha.No, bravo.No, charlie.No} {
		if status, _ := h.rawGet(fmt.Sprintf("/api/boards/hold/posts/%d", no)); status != http.StatusNotFound {
			t.Errorf("resolving held post %d: status %d, want 404", no, status)
		}
	}
	if status := h.threadStatus("hold", bravo.ThreadNo); status != http.StatusNotFound {
		t.Errorf("GET of a held thread: status %d, want 404", status)
	}
	if status, _ := h.rawGet(fmt.Sprintf("/api/threads/%d", h.threadID("hold", bravo.ThreadNo))); status != http.StatusNotFound {
		t.Errorf("GET of a held thread by id: status %d, want 404", status)
	}
	if status, _ := h.postJSON(repliesPath("hold", bravo.ThreadNo), map[string]any{"body": "hello"}); status != http.StatusNotFound {
		t.Errorf("reply to a held thread: status %d, want 404", status)
	}
	if status, _ := h.postJSON(fmt.Sprintf("/api/threads/%d/posts", h.threadID("hold", bravo.ThreadNo)),
		map[string]any{"body": "hello"}); status != http.StatusNotFound {
		t.Errorf("reply to a held thread by id: status %d, want 404", status)
	}
	// CreateReply holds the line on its own, for a reply that got past the
	// handler's early check.
	board, err := h.app.store.Board(context.Background(), "hold")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.store.CreateReply(context.Background(), NewReply{
		Board: board, ThreadNo: bravo.ThreadNo, Identity: identity{Name: anonymousName}, Body: "x",
		PosterID: func(int64) string { return "id" },
	}); !errors.Is(err, errNotFound) {
		t.Errorf("CreateReply to a held thread: %v, want errNotFound", err)
	}
	for _, action := range []string{"sticky", "unsticky", "lock", "unlock"} {
		path := fmt.Sprintf("/api/boards/hold/threads/%d/%s", bravo.ThreadNo, action)
		if status := h.mod(http.MethodPost, path, "Authorization", "Bearer "+testModKey, nil); status != http.StatusNotFound {
			t.Errorf("%s on a held thread: status %d, want 404", action, status)
		}
	}

	// Quotes either side of a held post are not shown.
	for _, p := range fetchThread(t, h, "hold", older.ThreadNo).Posts {
		if p.No == quoted.No && len(p.Backlinks) != 0 {
			t.Errorf("a held reply shows up as a backlink: %+v", p.Backlinks)
		}
	}
	for _, p := range fetchThread(t, h, "hold", newer.ThreadNo).Posts {
		if p.No == quoting.No && len(p.Quotes) != 0 {
			t.Errorf("quotes of held posts are shown: %+v", p.Quotes)
		}
	}

	// Counters, last_post_at and bump order are as if nothing had been sent.
	olderAfter, newerAfter := snapshot(older.ThreadNo), snapshot(newer.ThreadNo)
	if olderAfter.Replies != olderBefore.Replies || olderAfter.Images != olderBefore.Images ||
		!olderAfter.LastPostAt.Equal(olderBefore.LastPostAt) || !olderAfter.BumpedAt.Equal(olderBefore.BumpedAt) {
		t.Errorf("a held reply changed its thread: before %+v, after %+v", olderBefore, olderAfter)
	}
	if newerAfter.Replies != newerBefore.Replies+1 { // the published quoting reply, and not the held one
		t.Errorf("replies on the newer thread: %d, want %d", newerAfter.Replies, newerBefore.Replies+1)
	}
	if fmt.Sprint(liveThreadNos(t, h, "hold")) != fmt.Sprint(orderBefore) {
		t.Errorf("bump order changed: %v, was %v", liveThreadNos(t, h, "hold"), orderBefore)
	}

	// Approving them puts every one of them where it belongs, which is the
	// walk's own positive control for what it looked for.
	for _, p := range []HeldPost{alpha, bravo, charlie} {
		if status, out := h.approve("hold", p.ThreadNo, p.No); status != http.StatusOK {
			t.Fatalf("approve %d: status %d (%v)", p.No, status, out)
		}
	}
	walk = publicWalk(t, h, "hold")
	for _, shown := range []string{"heldmarkeralpha", "heldmarkerbravo", "heldmarkercharlie", "heldsubjectbravo",
		alpha.Attachment.URL, bravo.Attachment.ThumbURL} {
		if !strings.Contains(walk, shown) {
			t.Errorf("approved text %q is not shown", shown)
		}
	}
	for _, p := range fetchThread(t, h, "hold", newer.ThreadNo).Posts {
		if p.No == quoting.No && len(p.Quotes) != 3 {
			t.Errorf("approved posts should bring their quote links: %+v", p.Quotes)
		}
	}
	if len(h.heldQueue()) != 0 {
		t.Errorf("approved posts are still in the queue")
	}
}

// A held post's image is in no public view, and its file is not served to
// the public either: its address answers 404 to anyone without the moderation
// key, the moderator gets the file, and neither answer may be cached, because
// the same address serves everyone once the post is approved.
func TestHeldImagesAreServedOnlyToModerators(t *testing.T) {
	h := modServer(t)
	h.addBoard("himg")
	img := pngBytes(t, 12, 12)

	var published createdPost
	if status := h.upload(threadsPath("himg"), map[string]string{"body": "a published image"}, "a.png", img, &published); status != http.StatusCreated {
		t.Fatalf("published thread: status %d (%s)", status, published.Error)
	}
	status, out := h.uploadMap(repliesPath("himg", published.ThreadNo), map[string]string{"body": "go die heldimage"}, "b.png", img)
	requireHeld(t, status, out, "himg", published.ThreadNo)
	held := heldNumber(t, h.heldQueue(), "heldimage")
	if held.Attachment == nil {
		t.Fatal("the held reply has no attachment in the queue")
	}
	public := fetchThread(t, h, "himg", published.ThreadNo).Posts[0].Attachment
	if public == nil {
		t.Fatal("the published thread has no attachment")
	}

	get := func(url string, header ...string) (int, string) {
		t.Helper()
		var status int
		if len(header) == 0 {
			status = h.mod(http.MethodGet, url, "", "", nil)
		} else {
			status = h.mod(http.MethodGet, url, header[0], header[1], nil)
		}
		return status, h.lastHeader.Get("Cache-Control")
	}
	// Positive control: a published post's files are served to anyone, to be
	// kept for good.
	for _, url := range []string{public.URL, public.ThumbURL} {
		if status, cache := get(url); status != http.StatusOK || cache != mediaCacheControl {
			t.Fatalf("published %s: status %d, Cache-Control %q", url, status, cache)
		}
	}
	for _, url := range []string{held.Attachment.URL, held.Attachment.ThumbURL} {
		if status, cache := get(url); status != http.StatusNotFound || cache != "no-store" {
			t.Errorf("held %s without the key: status %d, Cache-Control %q; want 404, no-store", url, status, cache)
		}
		if status, cache := get(url, "X-Mod-Key", "not-the-key"); status != http.StatusNotFound || cache != "no-store" {
			t.Errorf("held %s with a wrong key: status %d, Cache-Control %q; want 404, no-store", url, status, cache)
		}
		if status, cache := get(url, "X-Mod-Key", testModKey); status != http.StatusOK || cache != "no-store" {
			t.Errorf("held %s with the key: status %d, Cache-Control %q; want 200, no-store", url, status, cache)
		}
		if status, cache := get(url, "Authorization", "Bearer "+testModKey); status != http.StatusOK || cache != "no-store" {
			t.Errorf("held %s with the bearer key: status %d, Cache-Control %q; want 200, no-store", url, status, cache)
		}
	}

	if status, out := h.approve("himg", published.ThreadNo, held.No); status != http.StatusOK {
		t.Fatalf("approve: %d %v", status, out)
	}
	for _, url := range []string{held.Attachment.URL, held.Attachment.ThumbURL} {
		if status, cache := get(url); status != http.StatusOK || cache != mediaCacheControl {
			t.Errorf("approved %s: status %d, Cache-Control %q; want 200 for anyone", url, status, cache)
		}
	}
}

// With moderation switched off (no EDUCHAN_MOD_KEY) a held image is served to
// nobody: an empty key, or an empty bearer token, must not match the empty
// key the server has.
func TestHeldImagesWithModerationOffAreServedToNobody(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("nomod")
	thread := h.createThread("nomod", "", "", "op")
	status, out := h.uploadMap(repliesPath("nomod", thread.ThreadNo), map[string]string{"body": "go die"}, "h.png", pngBytes(t, 12, 12))
	requireHeld(t, status, out, "nomod", thread.ThreadNo)
	var row struct{ original, thumb string }
	if err := h.app.pool.QueryRow(context.Background(), `
		SELECT a.original_path, a.thumb_path FROM attachments a JOIN posts p ON p.id = a.post_id
		WHERE p.held_at IS NOT NULL`).Scan(&row.original, &row.thumb); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"/media/original/" + row.original, "/media/thumb/" + row.thumb} {
		for _, header := range [][2]string{
			{"", ""}, {"X-Mod-Key", ""}, {"Authorization", "Bearer "}, {"Authorization", "Bearer"}, {"Authorization", ""},
		} {
			status := h.mod(http.MethodGet, url, header[0], header[1], nil)
			if cache := h.lastHeader.Get("Cache-Control"); status != http.StatusNotFound || cache != "no-store" {
				t.Errorf("%s with %q: %q: status %d, Cache-Control %q; want 404, no-store", url, header[0], header[1], status, cache)
			}
		}
	}
}

func TestApprovingAReplyRecountsWithoutBumping(t *testing.T) {
	h := modServer(t)
	h.addBoard("apr")
	older := h.createThread("apr", "", "", "older")
	newer := h.createThread("apr", "", "", "newer")
	before := fetchThread(t, h, "apr", older.ThreadNo).Thread

	status, out := h.uploadMap(repliesPath("apr", older.ThreadNo), map[string]string{"body": "go die"}, "a.png", pngBytes(t, 8, 8))
	requireHeld(t, status, out, "apr", older.ThreadNo)
	held := h.heldQueue()[0]
	if held.OP || held.Reason != holdSelfHarm || held.ThreadNo != older.ThreadNo || held.Attachment == nil {
		t.Fatalf("queue entry: %+v", held)
	}

	time.Sleep(10 * time.Millisecond)
	if status, out := h.approve("apr", older.ThreadNo, held.No); status != http.StatusOK || out["approved"] != true {
		t.Fatalf("approve: %d %v", status, out)
	}
	after := fetchThread(t, h, "apr", older.ThreadNo)
	if after.Replies != before.Replies+1 || after.Images != before.Images+1 {
		t.Errorf("counters after approval: replies %d images %d, want %d and %d",
			after.Replies, after.Images, before.Replies+1, before.Images+1)
	}
	if !after.BumpedAt.Equal(before.BumpedAt) {
		t.Errorf("approving a reply bumped the thread: %v -> %v", before.BumpedAt, after.BumpedAt)
	}
	if last := after.Posts[len(after.Posts)-1]; last.No != held.No || !after.LastPostAt.Equal(last.CreatedAt) {
		t.Errorf("last_post_at %v, want the approved reply's %v", after.LastPostAt, last.CreatedAt)
	}
	if got := liveThreadNos(t, h, "apr"); fmt.Sprint(got) != fmt.Sprint([]int64{newer.ThreadNo, older.ThreadNo}) {
		t.Errorf("bump order after approval: %v", got)
	}

	// Approving it again, or approving any published post, is a conflict.
	if status, out := h.approve("apr", older.ThreadNo, held.No); status != http.StatusConflict || out["error"] != "post is not held" {
		t.Errorf("approving twice: %d %v", status, out)
	}
	if status, _ := h.approve("apr", older.ThreadNo, older.No); status != http.StatusConflict {
		t.Errorf("approving a published opening post: status %d, want 409", status)
	}
	if status, _ := h.approve("apr", older.ThreadNo, 9999); status != http.StatusNotFound {
		t.Errorf("approving a missing post: status %d, want 404", status)
	}
	if status, _ := h.approve("apr", newer.ThreadNo, held.No); status != http.StatusNotFound {
		t.Errorf("approving a post through the wrong thread: status %d, want 404", status)
	}
}

// A held post counts for nothing a reader can see: not the index's page
// count, not a thread's image cap, and not the counters a deletion or an
// approval recounts.
func TestHeldPostsLeaveCountsAndCapsAlone(t *testing.T) {
	h := modServer(t)
	h.addBoard("cnt", "cap")

	// A board of exactly one page of published threads stays one page with a
	// held thread on it.
	for i := 0; i < indexPageSize; i++ {
		h.createThread("cnt", "", "", fmt.Sprintf("thread %d", i))
	}
	status, out := h.postJSON(threadsPath("cnt"), map[string]any{"body": "go die"})
	requireHeld(t, status, out, "cnt", 0)
	if index := fetchIndex(t, h, "cnt", 1); index.Pages != 1 || len(index.Threads) != indexPageSize {
		t.Fatalf("index with a held thread: %d pages, %d threads; want 1 and %d", index.Pages, len(index.Threads), indexPageSize)
	}

	// A held image reply takes no slot under the cap...
	h.exec(`UPDATE boards SET image_limit = 2 WHERE slug = 'cap'`)
	thread := h.imageThread("cap", "opening post with an image")
	img := pngBytes(t, 12, 12)
	status, out = h.uploadMap(repliesPath("cap", thread.ThreadNo), map[string]string{"body": "go die heldimage"}, "h.png", img)
	requireHeld(t, status, out, "cap", thread.ThreadNo)
	held := heldNumber(t, h.heldQueue(), "heldimage")
	var published createdPost
	if status := h.upload(repliesPath("cap", thread.ThreadNo), map[string]string{"body": "a published image"}, "p.png", img, &published); status != http.StatusCreated {
		t.Fatalf("a published image after a held one at image_limit 2: status %d (%s), want 201", status, published.Error)
	}
	// ...and the cap still holds at the published count.
	if status, msg := h.imageReply("cap", thread.ThreadNo, "one too many"); status != http.StatusForbidden || msg != "image limit reached" {
		t.Fatalf("a third published image: status %d (%s), want 403 image limit reached", status, msg)
	}
	counts := func() (int, int) {
		th := fetchThread(t, h, "cap", thread.ThreadNo).Thread
		return th.Replies, th.Images
	}
	if replies, images := counts(); replies != 1 || images != 2 {
		t.Fatalf("counts with a held image reply: %d replies, %d images; want 1 and 2", replies, images)
	}

	// Deleting the published image recounts from the published posts only,
	// so the held one is not counted in its place.
	h.modAction(http.MethodDelete, fmt.Sprintf("/api/boards/cap/threads/%d/posts/%d", thread.ThreadNo, published.No))
	if replies, images := counts(); replies != 0 || images != 1 {
		t.Fatalf("counts after deleting the published image: %d replies, %d images; want 0 and 1", replies, images)
	}
	// Approving the held one counts it.
	if status, out := h.approve("cap", thread.ThreadNo, held.No); status != http.StatusOK {
		t.Fatalf("approve: %d %v", status, out)
	}
	if replies, images := counts(); replies != 1 || images != 2 {
		t.Fatalf("counts after approval: %d replies, %d images; want 1 and 2", replies, images)
	}
}

func TestHeldThreadsTakeNoPlaceOnTheBoardUntilApproved(t *testing.T) {
	h := modServer(t)
	h.addBoard("lim")
	h.exec(`UPDATE boards SET thread_limit = 2 WHERE slug = 'lim'`)

	first := h.createThread("lim", "", "", "first")
	second := h.createThread("lim", "", "", "second")
	status, out := h.postJSON(threadsPath("lim"), map[string]any{"subject": "how old are you", "body": "dm me"})
	requireHeld(t, status, out, "lim", 0)
	held := h.heldQueue()[0]
	if !held.OP || held.Reason != holdGrooming || held.Subject != "how old are you" {
		t.Fatalf("queue entry: %+v", held)
	}

	// The held thread archived nothing and does not count: a third published
	// thread archives exactly one.
	if archive := fetchArchive(t, h, "lim"); len(archive.Threads) != 0 {
		t.Fatalf("a held thread archived %+v", archive.Threads)
	}
	third := h.createThread("lim", "", "", "third")
	if got := liveThreadNos(t, h, "lim"); fmt.Sprint(got) != fmt.Sprint([]int64{third.ThreadNo, second.ThreadNo}) {
		t.Fatalf("live threads %v, want third and second", got)
	}
	if h.archivedAt("lim", held.ThreadNo) != nil {
		t.Fatal("a held thread was archived")
	}

	// Approved, it goes on the board as new and pushes the oldest off.
	if status, out := h.approve("lim", held.ThreadNo, held.No); status != http.StatusOK {
		t.Fatalf("approve: %d %v", status, out)
	}
	if got := liveThreadNos(t, h, "lim"); fmt.Sprint(got) != fmt.Sprint([]int64{held.ThreadNo, third.ThreadNo}) {
		t.Fatalf("live threads after approval %v, want the approved one first", got)
	}
	archived := []int64{}
	for _, a := range fetchArchive(t, h, "lim").Threads {
		archived = append(archived, a.No)
	}
	sort.Slice(archived, func(i, j int) bool { return archived[i] < archived[j] })
	if fmt.Sprint(archived) != fmt.Sprint([]int64{first.ThreadNo, second.ThreadNo}) {
		t.Fatalf("archive %v, want first and second", archived)
	}
	if detail := fetchThread(t, h, "lim", held.ThreadNo); detail.Subject != "how old are you" || len(detail.Posts) != 1 {
		t.Fatalf("approved thread: %+v", detail)
	}

	// The schema holds the line as well: a held thread cannot be archived.
	if _, err := h.app.pool.Exec(context.Background(),
		`UPDATE threads SET held_at = now(), archived_at = now() WHERE no = $1`, first.ThreadNo); err == nil {
		t.Error("a thread was both held and archived")
	}
}

func TestHeldQueueListsOldestFirstWithWhatAReaderWouldSee(t *testing.T) {
	h := modServer(t)
	h.addBoard("q1", "q2")
	thread := h.createThread("q1", "", "", "op")

	posts := []struct {
		path   string
		body   map[string]any
		reason string
	}{
		{threadsPath("q2"), map[string]any{"subject": "warning", "name": "ana#secret", "body": "i will bring a gun to school"}, holdThreat},
		{repliesPath("q1", thread.ThreadNo), map[string]any{"body": "asl? we should talk privately"}, holdGrooming},
		{repliesPath("q1", thread.ThreadNo), map[string]any{"body": "thinspo"}, holdSelfHarm},
	}
	for _, p := range posts {
		if status, out := h.postJSON(p.path, p.body); status != http.StatusAccepted {
			t.Fatalf("%v: status %d (%v)", p.body, status, out)
		}
	}
	queue := h.heldQueue()
	if len(queue) != len(posts) {
		t.Fatalf("queue has %d posts, want %d", len(queue), len(posts))
	}
	for i, p := range posts {
		got := queue[i]
		if got.Reason != p.reason || got.Body != p.body["body"] {
			t.Errorf("queue[%d] = %+v, want the %s post", i, got, p.reason)
		}
	}
	first := queue[0]
	if first.Board != "q2" || !first.OP || first.Subject != "warning" || first.Name != "ana" ||
		first.Tripcode == "" || first.ThreadNo != first.No || first.CreatedAt.IsZero() {
		t.Errorf("held thread entry: %+v", first)
	}
	if queue[1].Board != "q1" || queue[1].OP || queue[1].Subject != "" || queue[1].ThreadNo != thread.ThreadNo {
		t.Errorf("held reply entry: %+v", queue[1])
	}

	// The raw shape, field by field.
	var raw struct {
		Posts []map[string]any `json:"posts"`
		Total *int             `json:"total"`
	}
	h.mod(http.MethodGet, "/api/held", "X-Mod-Key", testModKey, &raw)
	requireKeys(t, raw.Posts[0], "board", "thread_no", "no", "is_op", "held_reason", "created_at", "subject",
		"name", "tripcode", "poster_id", "body", "badge", "attachment")
	if raw.Total == nil || *raw.Total != len(posts) {
		t.Errorf("total = %v, want %d", raw.Total, len(posts))
	}

	// The poster ID is the one the post shows once approved, so a moderator
	// can tell which of a thread's posters sent it.
	reply := queue[1]
	if reply.PosterID == "" {
		t.Fatalf("held reply has no poster ID: %+v", reply)
	}
	if status, out := h.approve("q1", reply.ThreadNo, reply.No); status != http.StatusOK {
		t.Fatalf("approve: %d %v", status, out)
	}
	for _, p := range fetchThread(t, h, "q1", reply.ThreadNo).Posts {
		if p.No == reply.No && p.PosterID != reply.PosterID {
			t.Fatalf("approved reply shows poster ID %q, the queue said %q", p.PosterID, reply.PosterID)
		}
	}
}

// A threat names a place or a time, which may be today, while every other held
// post is harmless where it is. So threats lead the queue however many older
// holds are waiting, and the total says how many are held beyond the page.
func TestHeldQueuePutsThreatsFirstAndCountsEverything(t *testing.T) {
	h := modServer(t)
	h.addBoard("urgent")
	thread := h.createThread("urgent", "", "", "op")

	bodies := []struct{ body, reason string }{
		{"thinspo", holdSelfHarm},
		{"asl? we should talk privately", holdGrooming},
		{"pro ana tips", holdSelfHarm},
		{"i will bring a gun to school tomorrow", holdThreat},
		{"we should talk privately, how old are you?", holdGrooming},
		{"i'm going to stab the principal at school", holdThreat},
	}
	for _, b := range bodies {
		if status, out := h.postJSON(repliesPath("urgent", thread.ThreadNo), map[string]any{"body": b.body}); status != http.StatusAccepted {
			t.Fatalf("%q: status %d (%v)", b.body, status, out)
		}
	}
	want := []int{3, 5, 0, 1, 2, 4} // the threats in the order they came, then the rest
	queue := h.heldQueue()
	if len(queue) != len(want) {
		t.Fatalf("queue has %d posts, want %d", len(queue), len(want))
	}
	for i, j := range want {
		if queue[i].Body != bodies[j].body || queue[i].Reason != bodies[j].reason {
			t.Errorf("queue[%d] = %q (%s), want %q (%s)", i, queue[i].Body, queue[i].Reason, bodies[j].body, bodies[j].reason)
		}
	}

	// A page shorter than the queue still leads with the threats, and still
	// counts the whole queue.
	page, total, err := h.app.store.HeldPosts(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Body != bodies[3].body || total != len(bodies) {
		t.Errorf("a page of one: %d posts, first %q, total %d; want the first threat and a total of %d",
			len(page), page[0].Body, total, len(bodies))
	}
}

func TestHeldQueueAndApprovalNeedTheKey(t *testing.T) {
	h := modServer(t)
	h.addBoard("key")
	thread := h.createThread("key", "", "", "op")
	if status, _ := h.postJSON(repliesPath("key", thread.ThreadNo), map[string]any{"body": "kys"}); status != http.StatusAccepted {
		t.Fatalf("held reply: status %d", status)
	}
	no := h.heldQueue()[0].No
	approve := fmt.Sprintf("/api/boards/key/threads/%d/posts/%d/approve", thread.ThreadNo, no)

	for _, c := range []struct{ header, value string }{{"", ""}, {"Authorization", "Bearer wrong"}, {"X-Mod-Key", "wrong"}} {
		if status := h.mod(http.MethodGet, "/api/held", c.header, c.value, nil); status != http.StatusForbidden {
			t.Errorf("GET /api/held with %q: status %d, want 403", c.value, status)
		}
		if status := h.mod(http.MethodPost, approve, c.header, c.value, nil); status != http.StatusForbidden {
			t.Errorf("approve with %q: status %d, want 403", c.value, status)
		}
	}
	if len(h.heldQueue()) != 1 {
		t.Fatal("a refused approval approved the post")
	}

	closed := testServer(t, nil)
	if status := closed.mod(http.MethodGet, "/api/held", "Authorization", "Bearer "+testModKey, nil); status != http.StatusForbidden {
		t.Errorf("GET /api/held with moderation off: status %d, want 403", status)
	}
}

func TestDeletingHeldPostsRejectsThem(t *testing.T) {
	h := modServer(t)
	h.addBoard("rej")
	thread := h.createThread("rej", "", "", "op")
	h.reply("rej", thread.ThreadNo, "", "published reply", false)
	before := fetchThread(t, h, "rej", thread.ThreadNo).Thread
	img := pngBytes(t, 8, 8)

	h.uploadMap(repliesPath("rej", thread.ThreadNo), map[string]string{"body": "kys"}, "a.png", img)
	h.uploadMap(threadsPath("rej"), map[string]string{"body": "i will shoot up the school"}, "a.png", img)
	queue := h.heldQueue()
	reply, held := heldNumber(t, queue, "kys"), heldNumber(t, queue, "shoot")
	if files := filesUnder(t, h.app.cfg.UploadDir); len(files) != 4 {
		t.Fatalf("held images should be stored, two files each: %v", files)
	}

	h.modAction(http.MethodDelete, fmt.Sprintf("/api/boards/rej/threads/%d/posts/%d", thread.ThreadNo, reply.No))
	h.modAction(http.MethodDelete, fmt.Sprintf("/api/boards/rej/threads/%d", held.ThreadNo))
	if len(h.heldQueue()) != 0 {
		t.Fatal("deleted held posts are still queued")
	}
	if files := filesUnder(t, h.app.cfg.UploadDir); len(files) != 0 {
		t.Fatalf("deleting held posts left files: %v", files)
	}
	// Deleting a held reply leaves the counters it never touched alone.
	after := fetchThread(t, h, "rej", thread.ThreadNo).Thread
	if after.Replies != before.Replies || after.Images != before.Images || !after.LastPostAt.Equal(before.LastPostAt) {
		t.Errorf("deleting a held reply changed its thread: %+v -> %+v", before, after)
	}
}

// A thread with a held reply is not deleted by the sweep while the reply is
// unread, but to a reader it goes when it would have gone: the sweep that
// deletes the threads archived alongside it takes it out of every public view,
// image and links included, so how long it lasts says nothing about a held
// post. The moderators still have the reply, and once it is decided the next
// sweep deletes the thread.
func TestHeldReplyKeepsItsThreadFromTheSweep(t *testing.T) {
	h := modServer(t)
	h.addBoard("swp")
	h.exec(`UPDATE boards SET archive_days = 1 WHERE slug = 'swp'`)
	img := pngBytes(t, 12, 12)
	var thread createdPost
	if status := h.upload(threadsPath("swp"), map[string]string{"body": "op with an image"}, "a.png", img, &thread); status != http.StatusCreated {
		t.Fatalf("thread: status %d (%s)", status, thread.Error)
	}
	plain := h.createThread("swp", "", "", "archived alongside it")
	if status, _ := h.postJSON(repliesPath("swp", thread.ThreadNo), map[string]any{"body": "go die"}); status != http.StatusAccepted {
		t.Fatalf("held reply: status %d", status)
	}
	held := h.heldQueue()[0]
	opImage := fetchThread(t, h, "swp", thread.ThreadNo).Posts[0].Attachment
	// A thread elsewhere quotes the opening post, so a backlink would show it.
	other := h.createThread("swp", "", "", "elsewhere")
	quoting := h.reply("swp", other.ThreadNo, "", fmt.Sprintf(">>%d", thread.No), false)
	h.archiveThread("swp", thread.ThreadNo, 30*24*time.Hour)
	h.archiveThread("swp", plain.ThreadNo, 30*24*time.Hour)

	// Before the sweep both are in the archive and the quote links, as
	// positive controls.
	if got := archivedNos(fetchArchive(t, h, "swp")); !slices.Contains(got, thread.ThreadNo) || !slices.Contains(got, plain.ThreadNo) {
		t.Fatalf("archive before the sweep: %v", got)
	}
	quoteLinks := func() bool {
		for _, p := range fetchThread(t, h, "swp", other.ThreadNo).Posts {
			if p.No == quoting.No {
				return len(p.Quotes) != 0
			}
		}
		t.Fatal("the quoting reply is missing")
		return false
	}
	if !quoteLinks() {
		t.Fatal("the quote did not link before the sweep")
	}

	report, err := h.app.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Threads != 1 {
		t.Fatalf("the sweep deleted %d threads, want the plain one", report.Threads)
	}
	for _, no := range []int64{thread.ThreadNo, plain.ThreadNo} {
		if status := h.threadStatus("swp", no); status != http.StatusNotFound {
			t.Fatalf("thread %d after the sweep: status %d, want 404", no, status)
		}
	}
	if got := archivedNos(fetchArchive(t, h, "swp")); len(got) != 0 {
		t.Fatalf("archive after the sweep: %v, want empty", got)
	}
	var where map[string]any
	if status := h.do(http.MethodGet, fmt.Sprintf("/api/boards/swp/posts/%d", thread.No), nil, &where); status != http.StatusNotFound {
		t.Fatalf("resolving the expired thread's post: status %d (%v), want 404", status, where)
	}
	if status, out := h.postJSON(repliesPath("swp", thread.ThreadNo), map[string]any{"body": "hello?"}); status != http.StatusNotFound {
		t.Fatalf("replying to the expired thread: status %d (%v), want 404", status, out)
	}
	if quoteLinks() {
		t.Fatal("the quote of the expired thread still links")
	}
	for _, url := range []string{opImage.URL, opImage.ThumbURL} {
		if status := h.mod(http.MethodGet, url, "", "", nil); status != http.StatusNotFound ||
			h.lastHeader.Get("Cache-Control") != "no-store" {
			t.Fatalf("the expired thread's %s: status %d, Cache-Control %q; want 404, no-store",
				url, status, h.lastHeader.Get("Cache-Control"))
		}
		if status := h.mod(http.MethodGet, url, "X-Mod-Key", testModKey, nil); status != http.StatusOK {
			t.Fatalf("the expired thread's %s for a moderator: status %d", url, status)
		}
	}
	if queue := h.heldQueue(); len(queue) != 1 || queue[0].No != held.No {
		t.Fatalf("the held reply should still be queued: %+v", queue)
	}

	// Once a moderator has decided, the thread goes with the next sweep.
	if status, out := h.approve("swp", thread.ThreadNo, held.No); status != http.StatusOK {
		t.Fatalf("approve: %d %v", status, out)
	}
	if status := h.threadStatus("swp", thread.ThreadNo); status != http.StatusNotFound {
		t.Fatalf("approving the reply brought the expired thread back: status %d", status)
	}
	report, err = h.app.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Threads != 1 || report.Attachments != 1 {
		t.Fatalf("after approval the sweep should take the thread and its image: %+v", report)
	}
}

func archivedNos(a Archive) []int64 {
	nos := []int64{}
	for _, t := range a.Threads {
		nos = append(nos, t.No)
	}
	return nos
}

// The direction controls are taken out of everything a post shows before the
// content rules read it, so what a reader sees is in the order the rules read:
// an override cannot turn a reversed address the rules passed into a working
// one on screen.
func TestBidiControlsAreNotStored(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("bidi")
	var created createdPost
	if status := h.post(threadsPath("bidi"), map[string]any{
		"subject": "\u202eti daer", "name": "\u2067ana\u2069", "body": "\u202emoc.liamg@ana\u202c and \u200fmore",
	}, &created); status != http.StatusCreated {
		t.Fatalf("status %d (%s)", status, created.Error)
	}
	detail := fetchThread(t, h, "bidi", created.ThreadNo)
	op := detail.Posts[0]
	for field, got := range map[string]string{"subject": detail.Thread.Subject, "name": op.Name, "body": op.Body} {
		if strings.IndexFunc(got, bidiControl) >= 0 {
			t.Errorf("%s stored with a direction control: %q", field, got)
		}
	}
	if detail.Thread.Subject != "ti daer" || op.Name != "ana" || op.Body != "moc.liamg@ana and more" {
		t.Fatalf("stored %q / %q / %q", detail.Thread.Subject, op.Name, op.Body)
	}
}

func TestBadgedPostsAreFilteredAlike(t *testing.T) {
	h := verifyServer(t, func(cfg *Config) { cfg.ModKey = testModKey })
	h.addBoard("bdg")
	cred := h.verify("ana@iitb.ac.in").Credential

	status, out := h.postWithCredentialMap(cred, threadsPath("bdg"), map[string]any{"body": "dm me on insta", "badge": true})
	requireBlock(t, status, out, ruleContact, fieldBody, "dm me on insta")
	status, out = h.postWithCredentialMap(cred, threadsPath("bdg"), map[string]any{"body": "how old are you? dm me", "badge": true})
	requireHeld(t, status, out, "bdg", 0)
	if queue := h.heldQueue(); len(queue) != 1 || queue[0].Badge != "IIT Bombay" {
		t.Fatalf("a held badged post: %+v", queue)
	}
}

func TestTheFilterLogsNothingItRead(t *testing.T) {
	var logs syncBuffer
	h := testServerLogging(t, func(cfg *Config) { cfg.ModKey = testModKey }, &logs)
	h.addBoard("log")
	thread := h.createThread("log", "", "", "op")
	startup := len(logs.String())

	posts := []map[string]any{
		{"body": "call 9876543210"},
		{"name": "snap: zebra_quokka", "body": "hi"},
		{"body": "i want to die quietmarkerone"},
		{"body": "how old are you? dm me quietmarkertwo"},
		{"body": "go die quietmarkerthree"},
		{"body": "a clean reply"},
	}
	for _, p := range posts {
		h.postJSON(repliesPath("log", thread.ThreadNo), p)
	}
	h.postJSON(threadsPath("log"), map[string]any{"subject": "ana@gmail.com", "body": "hi"})
	for _, p := range h.heldQueue() {
		h.approve("log", p.ThreadNo, p.No)
	}

	text := logs.String()
	// Positive control: the requests were logged.
	if !strings.Contains(text, fmt.Sprintf("/api/boards/log/threads/%d/posts", thread.ThreadNo)) ||
		!strings.Contains(text, `"action":"approve"`) {
		t.Fatalf("the log is missing the requests it should have: %s", text)
	}
	for _, secret := range []string{"9876543210", "zebra_quokka", "quietmarker", "ana@gmail.com", "die",
		"how old", "self_harm", "grooming", "contact", "support"} {
		if strings.Contains(text, secret) {
			t.Errorf("the log contains %q", secret)
		}
	}

	// What the log does say, exactly: one line per request, with its method,
	// path, status and duration, so a 422 or a 202 on a posting route shows
	// that a post there was refused or held, and nothing more; and the
	// moderator's approval. The documentation promises exactly this much.
	allowed := map[string][]string{
		"request":    {"duration_ms", "level", "method", "msg", "path", "status", "time"},
		"moderation": {"action", "board", "level", "msg", "op", "post", "thread", "time"},
	}
	statuses := map[float64]bool{}
	for _, line := range strings.Split(strings.TrimSpace(text[startup:]), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		msg, _ := record["msg"].(string)
		want, ok := allowed[msg]
		if !ok {
			t.Errorf("unexpected log record %v", record)
			continue
		}
		requireKeys(t, record, want...)
		if msg == "request" && strings.HasPrefix(record["path"].(string), "/api/boards/log/") {
			statuses[record["status"].(float64)] = true
		}
		if msg == "moderation" && record["action"] != "approve" {
			t.Errorf("unexpected moderation record %v", record)
		}
	}
	for _, status := range []float64{http.StatusCreated, http.StatusUnprocessableEntity, http.StatusAccepted} {
		if !statuses[status] {
			t.Errorf("no posting request logged with status %v: %s", status, text)
		}
	}
}

// Approving takes the board's row before the thread's, as posting does. The
// test plays a new thread's part by hand — it takes the board's row, waits
// until the approval is blocked behind it, then archives the thread the held
// reply is in — so the two meet in the order that would deadlock if the
// approval took the thread first.
func TestApprovalRacingTheArchivingThreadDoesNotDeadlock(t *testing.T) {
	h := modServer(t)
	h.addBoard("arace")
	thread := h.createThread("arace", "", "", "about to be archived")
	if status, _ := h.postJSON(repliesPath("arace", thread.ThreadNo), map[string]any{"body": "go die"}); status != http.StatusAccepted {
		t.Fatalf("held reply: status %d", status)
	}
	held := h.heldQueue()[0]

	ctx := context.Background()
	newThread, err := pgx.Connect(ctx, h.app.cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer newThread.Close(ctx)
	tx, err := newThread.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE boards SET next_post_no = next_post_no + 1 WHERE slug = 'arace'`); err != nil {
		t.Fatalf("take the board's row: %v", err)
	}

	approved := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/boards/arace/threads/%d/posts/%d/approve",
			h.base, thread.ThreadNo, held.No), nil)
		if err != nil {
			approved <- 0
			return
		}
		req.Header.Set("Authorization", "Bearer "+testModKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			approved <- 0
			return
		}
		res.Body.Close()
		approved <- res.StatusCode
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := h.app.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&waiting); err != nil {
			t.Fatalf("look for the blocked approval: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the approval never waited on the board's row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := tx.Exec(ctx, `UPDATE threads SET archived_at = now()
		WHERE no = $1 AND board_id = (SELECT id FROM boards WHERE slug = 'arace')`, thread.ThreadNo); err != nil {
		t.Fatalf("archive the thread: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the new thread's part: %v", err)
	}
	if status := <-approved; status != http.StatusOK {
		t.Fatalf("approval: status %d, want 200", status)
	}
	// The reply is published in the thread as it now stands, archived.
	detail := fetchThread(t, h, "arace", thread.ThreadNo)
	if detail.ArchivedAt == nil || len(detail.Posts) != 2 || detail.Replies != 1 {
		t.Fatalf("thread after approval: %+v", detail)
	}
}
