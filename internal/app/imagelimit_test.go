package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"sync"
	"testing"
	"time"
)

// threadCounters is what each view of a thread says about its images.
type threadCounters struct {
	images       int
	atImageLimit bool
}

// imageCounters reads a thread's image counters from the thread view, the
// index, and the catalog, failing unless all three agree.
func imageCounters(t *testing.T, h *harness, board string, no int64) threadCounters {
	t.Helper()
	detail := fetchThread(t, h, board, no)
	got := threadCounters{detail.Images, detail.AtImageLimit}

	var inIndex, inCatalog *threadCounters
	for _, thread := range fetchIndex(t, h, board, 1).Threads {
		if thread.No == no {
			inIndex = &threadCounters{thread.Images, thread.AtImageLimit}
		}
	}
	var catalog Catalog
	h.get("/api/boards/"+board+"/catalog", &catalog)
	for _, thread := range catalog.Threads {
		if thread.No == no {
			inCatalog = &threadCounters{thread.Images, thread.AtImageLimit}
		}
	}
	if inIndex == nil || *inIndex != got {
		t.Errorf("index counters %+v disagree with the thread's %+v", inIndex, got)
	}
	if inCatalog == nil || *inCatalog != got {
		t.Errorf("catalog counters %+v disagree with the thread's %+v", inCatalog, got)
	}
	return got
}

func (c *client) imageReply(board string, threadNo int64, body string) (int, string) {
	c.t.Helper()
	var out createdPost
	path := fmt.Sprintf("/api/boards/%s/threads/%d/posts", board, threadNo)
	status := c.upload(path, map[string]string{"body": body}, "reply.png", pngBytes(c.t, 24, 24), &out)
	return status, out.Error
}

func (c *client) imageThread(board, body string) createdPost {
	c.t.Helper()
	var out createdPost
	if status := c.upload("/api/boards/"+board+"/threads", map[string]string{"body": body},
		"op.png", pngBytes(c.t, 24, 24), &out); status != http.StatusCreated {
		c.t.Fatalf("create thread with an image: status %d (%s)", status, out.Error)
	}
	return out
}

// The cap counts every post with an image, the opening post included, and
// stops images at exactly the limit; text replies carry on, and deleting an
// image frees its slot.
func TestImageLimitBoundary(t *testing.T) {
	h := modServer(t)
	h.addBoard("pics")
	h.exec(`UPDATE boards SET image_limit = 3 WHERE slug = 'pics'`)

	thread := h.imageThread("pics", "opening post with an image")
	if got := imageCounters(t, h, "pics", thread.ThreadNo); got != (threadCounters{1, false}) {
		t.Errorf("after the opening post: %+v, want 1 image, not at the limit", got)
	}

	if status, msg := h.imageReply("pics", thread.ThreadNo, "second image"); status != http.StatusCreated {
		t.Fatalf("second image: status %d (%s)", status, msg)
	}
	h.reply("pics", thread.ThreadNo, "", "text replies do not count", false)
	if got := imageCounters(t, h, "pics", thread.ThreadNo); got != (threadCounters{2, false}) {
		t.Errorf("one below the limit: %+v, want 2 images, not at the limit", got)
	}

	if status, msg := h.imageReply("pics", thread.ThreadNo, "third image takes the last slot"); status != http.StatusCreated {
		t.Fatalf("image at the limit: status %d (%s)", status, msg)
	}
	if got := imageCounters(t, h, "pics", thread.ThreadNo); got != (threadCounters{3, true}) {
		t.Errorf("at the limit: %+v, want 3 images, at the limit", got)
	}
	files := filesUnder(t, h.app.cfg.UploadDir)

	status, msg := h.imageReply("pics", thread.ThreadNo, "one too many")
	if status != http.StatusForbidden || msg != "image limit reached" {
		t.Errorf("image past the limit: status %d (%q), want 403 %q", status, msg, "image limit reached")
	}
	// A refused image leaves nothing behind, whether it was turned away
	// before it was stored or, racing another reply, stored and then refused
	// inside the transaction.
	if after := filesUnder(t, h.app.cfg.UploadDir); len(after) != len(files) {
		t.Errorf("a refused image left blobs behind: %d files before, %d after", len(files), len(after))
	}

	reply := h.reply("pics", thread.ThreadNo, "", "text still goes through", false)
	detail := fetchThread(t, h, "pics", thread.ThreadNo)
	if len(detail.Posts) != 5 || detail.Images != 3 {
		t.Errorf("thread has %d posts and %d images, want 5 and 3", len(detail.Posts), detail.Images)
	}
	if detail.Posts[len(detail.Posts)-1].No != reply.No {
		t.Error("the text reply is not the newest post")
	}

	// Deleting an image frees its slot, and the next image takes it.
	h.modAction(http.MethodDelete,
		fmt.Sprintf("/api/boards/pics/threads/%d/posts/%d", thread.ThreadNo, detail.Posts[1].No))
	if got := imageCounters(t, h, "pics", thread.ThreadNo); got != (threadCounters{2, false}) {
		t.Errorf("after deleting an image: %+v, want 2 images, not at the limit", got)
	}
	if status, msg := h.imageReply("pics", thread.ThreadNo, "into the freed slot"); status != http.StatusCreated {
		t.Errorf("image after a deletion: status %d (%s), want 201", status, msg)
	}
}

// A zero limit is read literally: replies can never carry an image. The
// opening post is not held to it, because the cap limits a conversation.
func TestImageLimitZeroKeepsImagesOutOfReplies(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("words")
	h.exec(`UPDATE boards SET image_limit = 0 WHERE slug = 'words'`)

	text := h.createThread("words", "", "", "a text thread")
	if got := imageCounters(t, h, "words", text.ThreadNo); got != (threadCounters{0, true}) {
		t.Errorf("text thread: %+v, want no images and at the limit", got)
	}
	if status, _ := h.imageReply("words", text.ThreadNo, "an image"); status != http.StatusForbidden {
		t.Errorf("image reply: status %d, want 403", status)
	}
	h.reply("words", text.ThreadNo, "", "words only", false)

	pictured := h.imageThread("words", "an opening image is still allowed")
	if got := imageCounters(t, h, "words", pictured.ThreadNo); got != (threadCounters{1, true}) {
		t.Errorf("thread with an opening image: %+v, want 1 image and at the limit", got)
	}
}

// The cap answers to the posts, not to the counter the listings show. A
// counter that has drifted must neither let an extra image through nor keep a
// free slot shut, and the next image puts it right.
func TestImageLimitCountsThePostsNotTheCounter(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("drift")
	h.exec(`UPDATE boards SET image_limit = 2 WHERE slug = 'drift'`)
	setCounter := func(no int64, n int) {
		h.exec(`UPDATE threads SET image_count = $1 WHERE no = $2
			AND board_id = (SELECT id FROM boards WHERE slug = 'drift')`, n, no)
	}

	full := h.imageThread("drift", "opening image")
	if status, msg := h.imageReply("drift", full.ThreadNo, "second image"); status != http.StatusCreated {
		t.Fatalf("second image: status %d (%s)", status, msg)
	}
	setCounter(full.ThreadNo, 0)
	if status, _ := h.imageReply("drift", full.ThreadNo, "would be third"); status != http.StatusForbidden {
		t.Errorf("image to a full thread whose counter reads 0: status %d, want 403", status)
	}

	roomy := h.imageThread("drift", "opening image")
	setCounter(roomy.ThreadNo, 2)
	if status, msg := h.imageReply("drift", roomy.ThreadNo, "second image"); status != http.StatusCreated {
		t.Errorf("image to a thread with room whose counter reads 2: status %d (%s), want 201", status, msg)
	}
	if got := fetchThread(t, h, "drift", roomy.ThreadNo).Images; got != 2 {
		t.Errorf("counter after the next image = %d, want it recounted to 2", got)
	}
}

// A reply the thread refuses anyway — an image to a thread at its cap, or
// anything to a thread that is locked, archived or gone — is turned away
// before flood control charges for it and before its upload is stored. The
// client tells a poster refused at the cap to send the text on its own, and
// that has to go through at once rather than meet a 429.
func TestRefusedRepliesCostNoCooldown(t *testing.T) {
	h := testServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.PostCooldown = time.Minute
		cfg.UploadCooldown = time.Minute
	})
	h.addBoard("full", "roomy", "closed")
	h.exec(`UPDATE boards SET image_limit = 1 WHERE slug = 'full'`)

	// Somebody else opens the threads, so the cooldowns that brings are theirs.
	const opener, poster = "198.51.100.1", "198.51.100.2"
	asPoster := http.Header{"X-Forwarded-For": {poster}}
	var full, roomy createdPost
	if status := h.uploadWithHeaders(http.Header{"X-Forwarded-For": {opener}}, "/api/boards/full/threads",
		map[string]string{"body": "the only image"}, "op.png", pngBytes(t, 24, 24), &full); status != http.StatusCreated {
		t.Fatalf("open the full thread: status %d (%s)", status, full.Error)
	}
	if status := h.postFrom(opener, "/api/boards/roomy/threads",
		map[string]any{"body": "room for images"}, &roomy); status != http.StatusCreated {
		t.Fatalf("open the roomy thread: status %d (%s)", status, roomy.Error)
	}

	files := filesUnder(t, h.app.cfg.UploadDir)
	fullPath := fmt.Sprintf("/api/boards/full/threads/%d/posts", full.ThreadNo)
	var out createdPost
	status := h.uploadWithHeaders(asPoster, fullPath, map[string]string{"body": "one image too many"},
		"late.png", pngBytes(t, 24, 24), &out)
	if status != http.StatusForbidden || out.Error != "image limit reached" {
		t.Fatalf("image past the cap: status %d %q, want 403 %q", status, out.Error, "image limit reached")
	}
	if after := filesUnder(t, h.app.cfg.UploadDir); len(after) != len(files) {
		t.Errorf("the refused image was stored: %d files before, %d after", len(files), len(after))
	}
	// The text on its own, straight away: the reply cooldown was not started.
	if status := h.postFrom(poster, fullPath, map[string]any{"body": "the text on its own"}, &out); status != http.StatusCreated {
		t.Errorf("text reply right after the refusal: status %d (%s), want 201", status, out.Error)
	}
	// An image to a thread with room, straight away: nor was the upload one.
	roomyPath := fmt.Sprintf("/api/boards/roomy/threads/%d/posts", roomy.ThreadNo)
	if status := h.uploadWithHeaders(asPoster, roomyPath, map[string]string{"body": "an image with room"},
		"room.png", pngBytes(t, 24, 24), &out); status != http.StatusCreated {
		t.Errorf("image reply elsewhere right after the refusal: status %d (%s), want 201", status, out.Error)
	}
	// The cooldowns are on, so the two posts above prove something.
	if status := h.uploadWithHeaders(asPoster, roomyPath, map[string]string{"body": "and another"},
		"again.png", pngBytes(t, 24, 24), &out); status != http.StatusTooManyRequests {
		t.Errorf("a second image reply at once: status %d, want 429", status)
	}

	// A locked, an archived and a missing thread refuse for free as well: the
	// reply after all three is the poster's first to be charged.
	locked := h.createThread("closed", "", "", "to be locked")
	archived := h.createThread("closed", "", "", "to be archived")
	open := h.createThread("closed", "", "", "still open")
	h.exec(`UPDATE threads SET locked = TRUE WHERE no = $1
		AND board_id = (SELECT id FROM boards WHERE slug = 'closed')`, locked.ThreadNo)
	h.exec(`UPDATE threads SET archived_at = now() WHERE no = $1
		AND board_id = (SELECT id FROM boards WHERE slug = 'closed')`, archived.ThreadNo)
	for _, tc := range []struct {
		no     int64
		status int
		error  string
	}{
		{locked.ThreadNo, http.StatusForbidden, "thread is locked"},
		{archived.ThreadNo, http.StatusForbidden, "thread is archived"},
		{open.ThreadNo + 100, http.StatusNotFound, "not found"},
	} {
		var refused createdPost
		path := fmt.Sprintf("/api/boards/closed/threads/%d/posts", tc.no)
		if status := h.postFrom(poster, path, map[string]any{"body": "refused"}, &refused); status != tc.status || refused.Error != tc.error {
			t.Errorf("reply to No. %d: status %d %q, want %d %q", tc.no, status, refused.Error, tc.status, tc.error)
		}
	}
	if status := h.postFrom(poster, fmt.Sprintf("/api/boards/closed/threads/%d/posts", open.ThreadNo),
		map[string]any{"body": "this one lands"}, &out); status != http.StatusCreated {
		t.Errorf("reply after three refusals: status %d (%s), want 201", status, out.Error)
	}
}

// The handler's early refusal is a read without a lock, there for the poster's
// sake. What holds when replies race is CreateReply's own check under the
// board and thread locks, so called directly, with no handler in front of it,
// it still refuses every case on its own.
func TestCreateReplyRefusesOnItsOwn(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("direct")
	h.exec(`UPDATE boards SET image_limit = 1 WHERE slug = 'direct'`)
	full := h.imageThread("direct", "the only image")
	locked := h.createThread("direct", "", "", "to be locked")
	archived := h.createThread("direct", "", "", "to be archived")
	h.exec(`UPDATE threads SET locked = TRUE WHERE no = $1
		AND board_id = (SELECT id FROM boards WHERE slug = 'direct')`, locked.ThreadNo)
	h.exec(`UPDATE threads SET archived_at = now() WHERE no = $1
		AND board_id = (SELECT id FROM boards WHERE slug = 'direct')`, archived.ThreadNo)

	ctx := context.Background()
	board, err := h.app.store.Board(ctx, "direct")
	if err != nil {
		t.Fatalf("look up the board: %v", err)
	}
	reply := func(no int64, image bool) error {
		in := NewReply{Board: board, ThreadNo: no, Body: "straight to the store",
			PosterID: func(int64) string { return "direct" }}
		if image {
			// Refused before the insert, so it needs no stored file behind it.
			in.Attachment = &newAttachment{}
		}
		_, err := h.app.store.CreateReply(ctx, in)
		return err
	}
	for _, tc := range []struct {
		what  string
		no    int64
		image bool
		want  error
	}{
		{"an image to the full thread", full.ThreadNo, true, errImageLimit},
		{"text to the locked thread", locked.ThreadNo, false, errThreadLocked},
		{"text to the archived thread", archived.ThreadNo, false, errThreadArchived},
		{"text to no thread", archived.ThreadNo + 100, false, errNotFound},
	} {
		if err := reply(tc.no, tc.image); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.what, err, tc.want)
		}
	}
	if err := reply(full.ThreadNo, false); err != nil {
		t.Errorf("text to the full thread: %v, want it posted", err)
	}
}

// Replies racing for the last slot serialise on the board row each takes for
// its post number: exactly one gets it, however many arrive at once.
func TestImageLimitHoldsUnderConcurrentReplies(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("race")
	h.exec(`UPDATE boards SET image_limit = 2 WHERE slug = 'race'`)
	thread := h.imageThread("race", "one slot left")

	// The requests are built up front so the goroutines do nothing but send:
	// the harness's helpers fail the test, which only the test's own
	// goroutine may do.
	const racers = 6
	path := fmt.Sprintf("%s/api/boards/race/threads/%d/posts", h.base, thread.ThreadNo)
	bodies := make([][]byte, racers)
	contentTypes := make([]string, racers)
	for i := range bodies {
		var buf bytes.Buffer
		form := multipart.NewWriter(&buf)
		_ = form.WriteField("body", fmt.Sprintf("racer %d", i))
		part, err := form.CreateFormFile(uploadFieldName, "racer.png")
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := part.Write(pngBytes(t, 24, 24)); err != nil {
			t.Fatalf("write file part: %v", err)
		}
		if err := form.Close(); err != nil {
			t.Fatalf("close form: %v", err)
		}
		// Each form has a boundary of its own, so each body keeps its type.
		bodies[i], contentTypes[i] = buf.Bytes(), form.FormDataContentType()
	}

	statuses := make([]int, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := http.Post(path, contentTypes[i], bytes.NewReader(bodies[i]))
			if err != nil {
				errs[i] = err
				return
			}
			res.Body.Close()
			statuses[i] = res.StatusCode
		}(i)
	}
	wg.Wait()

	created, refused := 0, 0
	for i, status := range statuses {
		switch {
		case errs[i] != nil:
			t.Errorf("racer %d: %v", i, errs[i])
		case status == http.StatusCreated:
			created++
		case status == http.StatusForbidden:
			refused++
		default:
			t.Errorf("racer %d: status %d", i, status)
		}
	}
	if created != 1 || refused != racers-1 {
		t.Errorf("%d racers got the last slot and %d were refused, want 1 and %d", created, refused, racers-1)
	}
	if got := fetchThread(t, h, "race", thread.ThreadNo).Images; got != 2 {
		t.Errorf("thread holds %d images, want the limit of 2", got)
	}
	// Two images, two renditions each: the refused ones left nothing behind.
	if files := filesUnder(t, h.app.cfg.UploadDir); len(files) != 4 {
		t.Errorf("upload directory holds %d files, want 4: %v", len(files), files)
	}
}
