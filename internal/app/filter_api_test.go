package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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

	// The same holds for replies.
	status, out = h.uploadMap(repliesPath("cool", created.ThreadNo), map[string]string{"body": "my snap is ana_23"}, "", nil)
	requireBlock(t, status, out, ruleContact, fieldBody, "my snap is ana_23")
	if status, out := h.postJSON(repliesPath("cool", created.ThreadNo), map[string]any{"body": "clean reply"}); status != http.StatusCreated {
		t.Fatalf("the clean reply after the refusal: status %d (%v)", status, out)
	}
	if status, _ := h.postJSON(repliesPath("cool", created.ThreadNo), map[string]any{"body": "again"}); status != http.StatusTooManyRequests {
		t.Fatalf("second clean reply: status %d, want 429", status)
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
	var raw map[string][]map[string]any
	h.mod(http.MethodGet, "/api/held", "X-Mod-Key", testModKey, &raw)
	requireKeys(t, raw["posts"][0], "board", "thread_no", "no", "is_op", "held_reason", "created_at", "subject",
		"name", "tripcode", "body", "badge", "attachment")
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

func TestHeldReplyKeepsItsThreadFromTheSweep(t *testing.T) {
	h := modServer(t)
	h.addBoard("swp")
	h.exec(`UPDATE boards SET archive_days = 1 WHERE slug = 'swp'`)
	thread := h.createThread("swp", "", "", "op")
	if status, _ := h.postJSON(repliesPath("swp", thread.ThreadNo), map[string]any{"body": "go die"}); status != http.StatusAccepted {
		t.Fatalf("held reply: status %d", status)
	}
	held := h.heldQueue()[0]
	h.archiveThread("swp", thread.ThreadNo, 30*24*time.Hour)

	if _, err := h.app.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := h.threadStatus("swp", thread.ThreadNo); status != http.StatusOK {
		t.Fatalf("the sweep took a thread with a held reply: status %d", status)
	}
	// Once a moderator has decided, the thread goes with the next sweep.
	if status, out := h.approve("swp", thread.ThreadNo, held.No); status != http.StatusOK {
		t.Fatalf("approve: %d %v", status, out)
	}
	if _, err := h.app.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := h.threadStatus("swp", thread.ThreadNo); status != http.StatusNotFound {
		t.Fatalf("after approval the sweep should take the thread: status %d", status)
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

	posts := []map[string]any{
		{"body": "call 9876543210"},
		{"name": "snap: zebra_quokka", "body": "hi"},
		{"body": "i want to die quietmarkerone"},
		{"body": "how old are you? dm me quietmarkertwo"},
		{"body": "go die quietmarkerthree"},
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
