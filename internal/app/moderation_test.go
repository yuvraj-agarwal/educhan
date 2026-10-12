package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuvraj-agarwal/educhan/internal/media"
)

const testModKey = "s3cret-mod-key"

// modServer starts an application with moderation enabled.
func modServer(t *testing.T) *harness {
	t.Helper()
	return testServer(t, func(cfg *Config) { cfg.ModKey = testModKey })
}

// mod sends a moderation request authenticated with the given credentials. An
// empty auth header is sent as no credentials at all.
func (c *client) mod(method, path, header, value string, out any) int {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, nil)
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	if header != "" {
		req.Header.Set(header, value)
	}
	return c.send(req, out)
}

// modAction performs an authenticated moderation call and fails the test unless
// it succeeds.
func (c *client) modAction(method, path string) map[string]any {
	c.t.Helper()
	var out map[string]any
	if status := c.mod(method, path, "Authorization", "Bearer "+testModKey, &out); status != http.StatusOK {
		c.t.Fatalf("%s %s: status %d (%v)", method, path, status, out["error"])
	}
	return out
}

// blobPaths turns an attachment's URLs back into the files backing them.
func blobPaths(t *testing.T, root string, att *Attachment) (string, string) {
	t.Helper()
	if att == nil {
		t.Fatal("post has no attachment")
	}
	original := strings.TrimPrefix(att.URL, originalURLPrefix)
	thumb := strings.TrimPrefix(att.ThumbURL, thumbURLPrefix)
	return filepath.Join(root, media.OriginalPrefix, filepath.FromSlash(original)),
		filepath.Join(root, media.ThumbPrefix, filepath.FromSlash(thumb))
}

func requireFile(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	switch {
	case want && err != nil:
		t.Errorf("expected %s to exist: %v", path, err)
	case !want && err == nil:
		t.Errorf("expected %s to be gone", path)
	case !want && !os.IsNotExist(err):
		t.Errorf("stat %s: %v", path, err)
	}
}

/* Authentication ----------------------------------------------------------- */

func TestModerationRequiresTheKey(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")
	thread := c.createThread("general", "guarded", "", "cannot be pinned by anyone")
	path := fmt.Sprintf("/api/boards/general/threads/%d/sticky", thread.ThreadNo)

	cases := []struct {
		name           string
		header, value  string
		wantStatus     int
		wantErrorMatch string
	}{
		{name: "no credentials", wantStatus: http.StatusForbidden, wantErrorMatch: "invalid moderation key"},
		{name: "wrong bearer token", header: "Authorization", value: "Bearer nope",
			wantStatus: http.StatusForbidden, wantErrorMatch: "invalid moderation key"},
		{name: "wrong key header", header: modKeyHeader, value: "nope",
			wantStatus: http.StatusForbidden, wantErrorMatch: "invalid moderation key"},
		{name: "key without the bearer scheme", header: "Authorization", value: testModKey,
			wantStatus: http.StatusForbidden, wantErrorMatch: "invalid moderation key"},
		{name: "bearer token", header: "Authorization", value: "Bearer " + testModKey,
			wantStatus: http.StatusOK},
		{name: "mod key header", header: modKeyHeader, value: testModKey,
			wantStatus: http.StatusOK},
		{name: "lowercase bearer scheme", header: "Authorization", value: "bearer " + testModKey,
			wantStatus: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out map[string]any
			status := c.mod(http.MethodPost, path, tc.header, tc.value, &out)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%v)", status, tc.wantStatus, out["error"])
			}
			if tc.wantErrorMatch != "" && !strings.Contains(fmt.Sprint(out["error"]), tc.wantErrorMatch) {
				t.Errorf("error = %v, want it to mention %q", out["error"], tc.wantErrorMatch)
			}
		})
	}
}

func TestModerationIsClosedWithoutAConfiguredKey(t *testing.T) {
	c := testServer(t, nil)
	c.addBoard("general")
	thread := c.createThread("general", "unmoderated", "", "no key configured")

	for _, call := range []struct{ method, path string }{
		{http.MethodPost, fmt.Sprintf("/api/boards/general/threads/%d/sticky", thread.ThreadNo)},
		{http.MethodPost, fmt.Sprintf("/api/boards/general/threads/%d/lock", thread.ThreadNo)},
		{http.MethodDelete, fmt.Sprintf("/api/boards/general/threads/%d", thread.ThreadNo)},
		{http.MethodDelete, fmt.Sprintf("/api/boards/general/threads/%d/posts/%d", thread.ThreadNo, thread.No)},
	} {
		var out map[string]any
		status := c.mod(call.method, call.path, "Authorization", "Bearer "+testModKey, &out)
		if status != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", call.method, call.path, status)
		}
		if got := fmt.Sprint(out["error"]); !strings.Contains(got, "not configured") {
			t.Errorf("%s %s: error = %q, want it to say moderation is not configured", call.method, call.path, got)
		}
	}

	// The thread is untouched.
	if detail := fetchThread(t, c, "general", thread.ThreadNo); detail.Sticky || detail.Locked {
		t.Errorf("thread state changed: sticky=%v locked=%v", detail.Sticky, detail.Locked)
	}
}

/* Sticky and lock ---------------------------------------------------------- */

func TestStickyAndUnsticky(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")

	pinned := c.createThread("general", "read me first", "", "board rules")
	newer := c.createThread("general", "newer", "", "posted later")

	if index := fetchIndex(t, c, "general", 1); index.Threads[0].No != newer.ThreadNo {
		t.Fatalf("index should start with the newest thread, got No. %d", index.Threads[0].No)
	}

	out := c.modAction(http.MethodPost, fmt.Sprintf("/api/boards/general/threads/%d/sticky", pinned.ThreadNo))
	if out["sticky"] != true {
		t.Errorf("response = %v, want sticky true", out)
	}

	index := fetchIndex(t, c, "general", 1)
	if index.Threads[0].No != pinned.ThreadNo || !index.Threads[0].Sticky {
		t.Errorf("sticky thread should lead the index, got %+v", index.Threads[0].Thread)
	}
	if detail := fetchThread(t, c, "general", pinned.ThreadNo); !detail.Sticky {
		t.Error("thread should report itself as sticky")
	}

	// A sticky thread survives pruning, so unsticky has to be reversible.
	out = c.modAction(http.MethodPost, fmt.Sprintf("/api/boards/general/threads/%d/unsticky", pinned.ThreadNo))
	if out["sticky"] != false {
		t.Errorf("response = %v, want sticky false", out)
	}
	if index := fetchIndex(t, c, "general", 1); index.Threads[0].No != newer.ThreadNo {
		t.Errorf("after unsticky the newest thread should lead, got No. %d", index.Threads[0].No)
	}
}

func TestLockAndUnlock(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")
	thread := c.createThread("general", "heated", "", "this will get out of hand")
	replyPath := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)

	out := c.modAction(http.MethodPost, fmt.Sprintf("/api/boards/general/threads/%d/lock", thread.ThreadNo))
	if out["locked"] != true {
		t.Errorf("response = %v, want locked true", out)
	}

	var rejected map[string]any
	if status := c.post(replyPath, map[string]any{"body": "let me in"}, &rejected); status != http.StatusForbidden {
		t.Fatalf("reply to a locked thread: status = %d, want 403", status)
	}
	if detail := fetchThread(t, c, "general", thread.ThreadNo); !detail.Locked || detail.Replies != 0 {
		t.Errorf("thread = locked %v with %d replies, want locked with none", detail.Locked, detail.Replies)
	}

	out = c.modAction(http.MethodPost, fmt.Sprintf("/api/boards/general/threads/%d/unlock", thread.ThreadNo))
	if out["locked"] != false {
		t.Errorf("response = %v, want locked false", out)
	}
	c.reply("general", thread.ThreadNo, "", "thanks", false)
	if detail := fetchThread(t, c, "general", thread.ThreadNo); detail.Locked || detail.Replies != 1 {
		t.Errorf("thread = locked %v with %d replies, want unlocked with one", detail.Locked, detail.Replies)
	}
}

/* Deletion ----------------------------------------------------------------- */

func TestDeletePostRemovesItAndItsAttachment(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")
	thread := c.createThread("general", "with a bad reply", "", "opening post")
	c.reply("general", thread.ThreadNo, "", "an innocent reply", false)

	var reply createdPost
	replyPath := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)
	if status := c.upload(replyPath, map[string]string{"body": "look at this"},
		"evidence.png", pngBytes(t, 120, 90), &reply); status != http.StatusCreated {
		t.Fatalf("reply with an image: status %d (%s)", status, reply.Error)
	}

	detail := fetchThread(t, c, "general", thread.ThreadNo)
	if len(detail.Posts) != 3 || detail.Replies != 2 {
		t.Fatalf("thread has %d posts and %d replies, want 3 and 2", len(detail.Posts), detail.Replies)
	}
	original, thumb := blobPaths(t, c.app.cfg.UploadDir, detail.Posts[2].Attachment)
	requireFile(t, original, true)
	requireFile(t, thumb, true)

	out := c.modAction(http.MethodDelete,
		fmt.Sprintf("/api/boards/general/threads/%d/posts/%d", thread.ThreadNo, reply.No))
	if out["thread_deleted"] != false {
		t.Errorf("response = %v, want thread_deleted false", out)
	}
	if out["deleted_attachments"] != float64(1) {
		t.Errorf("response = %v, want one deleted attachment", out)
	}

	// The post, its attachment row, and both blobs are gone; the rest stays.
	detail = fetchThread(t, c, "general", thread.ThreadNo)
	if len(detail.Posts) != 2 {
		t.Fatalf("thread has %d posts, want 2", len(detail.Posts))
	}
	for _, p := range detail.Posts {
		if p.No == reply.No {
			t.Errorf("post No. %d is still in the thread", reply.No)
		}
	}
	if detail.Replies != 1 {
		t.Errorf("replies = %d, want 1: the counter must follow the deletion", detail.Replies)
	}
	requireFile(t, original, false)
	requireFile(t, thumb, false)

	var attachments int
	if err := c.app.pool.QueryRow(context.Background(), `SELECT count(*) FROM attachments`).Scan(&attachments); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attachments != 0 {
		t.Errorf("%d attachment rows survived, want 0", attachments)
	}
}

func TestDeleteOpeningPostDeletesTheThread(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")
	thread := c.createThread("general", "spam", "", "the whole thread has to go")
	c.reply("general", thread.ThreadNo, "", "a reply", false)

	out := c.modAction(http.MethodDelete,
		fmt.Sprintf("/api/boards/general/threads/%d/posts/%d", thread.ThreadNo, thread.No))
	if out["thread_deleted"] != true {
		t.Errorf("response = %v, want thread_deleted true", out)
	}

	var status404 map[string]any
	path := fmt.Sprintf("/api/boards/general/threads/%d", thread.ThreadNo)
	if status := c.mod(http.MethodGet, path, "", "", &status404); status != http.StatusNotFound {
		t.Errorf("GET %s after deleting the OP: status = %d, want 404", path, status)
	}
	if index := fetchIndex(t, c, "general", 1); len(index.Threads) != 0 {
		t.Errorf("index still lists %d threads", len(index.Threads))
	}
}

func TestDeleteThreadRemovesPostsAndAttachments(t *testing.T) {
	c := modServer(t)
	c.addBoard("general")

	var thread createdPost
	if status := c.upload("/api/boards/general/threads",
		map[string]string{"subject": "raid", "body": "opening post"},
		"op.png", pngBytes(t, 200, 100), &thread); status != http.StatusCreated {
		t.Fatalf("create thread: status %d (%s)", status, thread.Error)
	}
	var reply createdPost
	replyPath := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)
	if status := c.upload(replyPath, map[string]string{"body": "and a reply"},
		"reply.png", pngBytes(t, 90, 140), &reply); status != http.StatusCreated {
		t.Fatalf("reply: status %d (%s)", status, reply.Error)
	}
	// A second thread proves deletion is scoped to the one being removed.
	survivor := c.createThread("general", "unrelated", "", "leave me alone")

	detail := fetchThread(t, c, "general", thread.ThreadNo)
	opOriginal, opThumb := blobPaths(t, c.app.cfg.UploadDir, detail.Posts[0].Attachment)
	replyOriginal, replyThumb := blobPaths(t, c.app.cfg.UploadDir, detail.Posts[1].Attachment)

	out := c.modAction(http.MethodDelete, fmt.Sprintf("/api/boards/general/threads/%d", thread.ThreadNo))
	if out["deleted_posts"] != float64(2) || out["deleted_attachments"] != float64(2) {
		t.Errorf("response = %v, want two deleted posts and two attachments", out)
	}

	for _, path := range []string{opOriginal, opThumb, replyOriginal, replyThumb} {
		requireFile(t, path, false)
	}

	var posts, attachments, threads int
	ctx := context.Background()
	if err := c.app.pool.QueryRow(ctx, `SELECT count(*) FROM posts`).Scan(&posts); err != nil {
		t.Fatalf("count posts: %v", err)
	}
	if err := c.app.pool.QueryRow(ctx, `SELECT count(*) FROM attachments`).Scan(&attachments); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if err := c.app.pool.QueryRow(ctx, `SELECT count(*) FROM threads`).Scan(&threads); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if posts != 1 || attachments != 0 || threads != 1 {
		t.Errorf("after deletion: %d posts, %d attachments, %d threads; want 1, 0, 1",
			posts, attachments, threads)
	}

	index := fetchIndex(t, c, "general", 1)
	if len(index.Threads) != 1 || index.Threads[0].No != survivor.ThreadNo {
		t.Errorf("index = %+v, want only the unrelated thread", index.Threads)
	}
}

func TestModerationNotFoundResponses(t *testing.T) {
	c := modServer(t)
	c.addBoard("general", "research")
	thread := c.createThread("general", "present", "", "opening post")

	cases := map[string]struct {
		method, path string
		wantStatus   int
	}{
		"unknown board": {http.MethodPost, "/api/boards/nope/threads/1/sticky", http.StatusNotFound},
		"unknown thread": {http.MethodPost,
			fmt.Sprintf("/api/boards/general/threads/%d/lock", thread.ThreadNo+999), http.StatusNotFound},
		"unknown thread deletion": {http.MethodDelete,
			fmt.Sprintf("/api/boards/general/threads/%d", thread.ThreadNo+999), http.StatusNotFound},
		"unknown post": {http.MethodDelete,
			fmt.Sprintf("/api/boards/general/threads/%d/posts/%d", thread.ThreadNo, thread.No+999),
			http.StatusNotFound},
		"post in another thread": {http.MethodDelete,
			fmt.Sprintf("/api/boards/research/threads/%d/posts/%d", thread.ThreadNo, thread.No),
			http.StatusNotFound},
		"malformed thread number": {http.MethodPost, "/api/boards/general/threads/abc/sticky",
			http.StatusBadRequest},
		"malformed post number": {http.MethodDelete,
			fmt.Sprintf("/api/boards/general/threads/%d/posts/abc", thread.ThreadNo), http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out map[string]any
			status := c.mod(tc.method, tc.path, modKeyHeader, testModKey, &out)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%v)", status, tc.wantStatus, out["error"])
			}
			if out["error"] == nil {
				t.Error("error responses must carry an error message")
			}
		})
	}
}
