package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// busyPost is what a post turned away as busy answers with.
type busyPost struct {
	Error      string `json:"error"`
	RetryAfter int    `json:"retry_after"`
}

// checkBusy asserts the 503 and its Retry-After, the same number of seconds
// in the header and the body.
func checkBusy(t *testing.T, c *client, status int, out busyPost) {
	t.Helper()
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d (%s), want 503", status, out.Error)
	}
	want := int(busyRetryAfter / time.Second)
	if got := c.lastHeader.Get("Retry-After"); got != strconv.Itoa(want) {
		t.Errorf("Retry-After = %q, want %d", got, want)
	}
	if out.RetryAfter != want || !strings.Contains(out.Error, strconv.Itoa(want)+"s") {
		t.Errorf("body = %+v, want retry_after %d and the wait in the message", out, want)
	}
}

// storedFiles counts the files under the upload directory.
func storedFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return n
}

// With no room in the upload budget for a post's body in the time it has,
// the post is turned away before its body is kept: 503 with a Retry-After,
// nothing stored, no cooldown started, so the same post goes through as soon
// as there is room. The whole body is read first, so the poster gets the
// answer rather than a connection closed under the upload. Text posts, which
// are JSON, are not held up.
func TestBusyServerTurnsAnUploadAwayBeforeKeepingIt(t *testing.T) {
	h := testServer(t, func(cfg *Config) {
		cfg.ThreadCooldown = time.Minute
		cfg.UploadCooldown = time.Minute
	})
	h.addBoard("general")
	// Shorter than the time a wait leaves for the work, so a wait for room
	// gives up at once.
	h.app.postDeadline = time.Second

	full, err := media.ReserveUpload(context.Background(), math.MaxInt64)
	if err != nil {
		t.Fatalf("fill the upload budget: %v", err)
	}
	var once sync.Once
	free := func() { once.Do(full) }
	t.Cleanup(free)

	// Large enough that a body left unread would not all fit in the
	// connection's buffers.
	image := noisyPNGBytes(t, 1024, 1024)
	var out busyPost
	status := h.upload("/api/boards/general/threads", map[string]string{"body": "busy"}, "photo.png", image, &out)
	checkBusy(t, h.client, status, out)
	if n := h.countRows("posts"); n != 0 {
		t.Errorf("%d posts stored, want none", n)
	}
	if n := storedFiles(t, h.app.cfg.UploadDir); n != 0 {
		t.Errorf("%d files stored, want none", n)
	}

	var text createdPost
	if status := h.post("/api/boards/general/threads", map[string]any{"body": "text only"}, &text); status != http.StatusCreated {
		t.Fatalf("text thread while uploads were busy: status %d (%s)", status, text.Error)
	}

	free()
	h.app.postDeadline = postDeadline
	// The text thread above started the thread cooldown on this board; the
	// image post goes to another, so what is checked is that the turned-away
	// post started nothing of its own: the upload cooldown above all.
	h.addBoard("photos")
	var created createdPost
	if status := h.upload("/api/boards/photos/threads", map[string]string{"body": "again"}, "photo.png", image, &created); status != http.StatusCreated {
		t.Fatalf("the same post once there was room: status %d (%s)", status, created.Error)
	}
}

// A post turned away as busy is the server shedding load as it is meant to,
// not a failure: its request line is a warning, and nothing is logged at
// error level for it.
func TestTurnedAwayPostsAreNotLoggedAsFailures(t *testing.T) {
	var logs syncBuffer
	h := testServerLogging(t, nil, &logs)
	h.addBoard("general")
	h.app.postDeadline = time.Second
	full, err := media.ReserveUpload(context.Background(), math.MaxInt64)
	if err != nil {
		t.Fatalf("fill the upload budget: %v", err)
	}
	defer full()

	var out busyPost
	status := h.upload("/api/boards/general/threads", map[string]string{"body": "busy"}, "photo.png", pngBytes(t, 32, 32), &out)
	checkBusy(t, h.client, status, out)
	lines := logs.String()
	if !strings.Contains(lines, `"level":"WARN","msg":"request","method":"POST","path":"/api/boards/general/threads","status":503`) {
		t.Errorf("no warning for the turned-away post in:\n%s", lines)
	}
	if strings.Contains(lines, `"level":"ERROR"`) {
		t.Errorf("a turned-away post logged an error:\n%s", lines)
	}
}

// stalledStore holds every write until its context ends, as a store too slow
// for the post's deadline would.
type stalledStore struct {
	media.Store
}

func (stalledStore) Put(ctx context.Context, _, _ string, _ io.Reader) error {
	<-ctx.Done()
	return ctx.Err()
}

// A post whose image cannot be stored before its deadline is not made: 503,
// nothing stored, and the cooldowns it started given back, so it can be sent
// again at once. Without the deadline it would wait on, be made after the
// server could no longer answer it, and leave its poster to send it twice.
func TestPostPastItsDeadlineIsNotMadeAndNotCharged(t *testing.T) {
	h := testServer(t, func(cfg *Config) {
		cfg.ThreadCooldown = time.Minute
		cfg.PostCooldown = time.Minute
		cfg.UploadCooldown = time.Minute
	})
	// The opening post starts the thread cooldown on its board, so the new
	// threads go to another.
	h.addBoard("general", "photos")
	thread := h.createThread("general", "", "", "op")
	h.app.postDeadline = 500 * time.Millisecond
	real := h.app.media
	h.app.media = stalledStore{real}

	image := pngBytes(t, 64, 64)
	start := time.Now()
	var out busyPost
	status := h.upload("/api/boards/photos/threads", map[string]string{"body": "late"}, "photo.png", image, &out)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("answered after %v; want soon after the deadline", took)
	}
	checkBusy(t, h.client, status, out)
	status = h.upload(fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.No), map[string]string{"body": "late"}, "photo.png", image, &out)
	checkBusy(t, h.client, status, out)

	if n := h.countRows("posts"); n != 1 {
		t.Errorf("%d posts stored, want only the opening post made before", n)
	}
	if n := h.countRows("attachments"); n != 0 {
		t.Errorf("%d attachments stored, want none", n)
	}

	// Given back: the same posts go through at once with a store that keeps
	// up, under cooldowns of a minute.
	h.app.media = real
	h.app.postDeadline = postDeadline
	var created createdPost
	if status := h.upload("/api/boards/photos/threads", map[string]string{"body": "again"}, "photo.png", image, &created); status != http.StatusCreated {
		t.Fatalf("the thread again: status %d (%s), want it made", status, created.Error)
	}
	// The thread just made started the upload cooldown, which a reply with
	// an image also needs; a text reply shows the reply cooldown was given
	// back.
	if status := h.post(fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.No), map[string]any{"body": "again"}, &created); status != http.StatusCreated {
		t.Fatalf("the reply again: status %d (%s), want it made", status, created.Error)
	}
}

// formPart is one part of a multipart body: a field when filename is empty,
// otherwise a file part.
type formPart struct{ name, filename, value string }

// multipartPost sends the parts in order as a multipart post.
func (c *client) multipartPost(path string, parts []formPart, out any) int {
	c.t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, p := range parts {
		var w io.Writer
		var err error
		if p.filename == "" {
			w, err = form.CreateFormField(p.name)
		} else {
			w, err = form.CreateFormFile(p.name, p.filename)
		}
		if err != nil {
			c.t.Fatalf("part %s: %v", p.name, err)
		}
		if _, err := io.WriteString(w, p.value); err != nil {
			c.t.Fatalf("part %s: %v", p.name, err)
		}
	}
	if err := form.Close(); err != nil {
		c.t.Fatalf("close form: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, &body)
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	return c.send(req, out)
}

// A multipart post is read as ParseMultipartForm and FormValue read it: the
// first value of each field, an empty file input as no image, a value named
// "image" as no image, and anything else passed over. What changed is what
// it may carry: text fields come to what a JSON post may at most, and the
// parts to a number no form comes near.
func TestMultipartPostsAreReadAsTheFormSendsThem(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("general")
	image := string(pngBytes(t, 32, 32))

	var created createdPost
	status := h.multipartPost("/api/boards/general/threads", []formPart{
		{name: "body", value: "first"},
		{name: "image", value: "not a file"},
		{name: "other", filename: "other.png", value: image},
		{name: "image", filename: "", value: ""},
		{name: "body", value: "second"},
		{name: "subject", value: "kept"},
	}, &created)
	if status != http.StatusCreated {
		t.Fatalf("status %d (%s)", status, created.Error)
	}
	var thread ThreadDetail
	h.get(fmt.Sprintf("/api/boards/general/threads/%d", created.No), &thread)
	if op := thread.Posts[0]; op.Body != "first" || thread.Subject != "kept" || op.Attachment != nil {
		t.Errorf("stored body %q, subject %q, attachment %v; want the first body, the subject, no image", op.Body, thread.Subject, op.Attachment)
	}
	if n := storedFiles(t, h.app.cfg.UploadDir); n != 0 {
		t.Errorf("%d files stored for parts that are not the image", n)
	}

	// The first image part is the image; a second is passed over.
	status = h.multipartPost("/api/boards/general/threads", []formPart{
		{name: "image", filename: "a.png", value: image},
		{name: "image", filename: "b.png", value: "not an image"},
		{name: "spoiler", value: "on"},
	}, &created)
	if status != http.StatusCreated {
		t.Fatalf("two image parts: status %d (%s)", status, created.Error)
	}

	var refused createdPost
	long := strings.Repeat("x", maxRequestBytes/2+1)
	if status := h.multipartPost("/api/boards/general/threads", []formPart{
		{name: "subject", value: long}, {name: "body", value: long},
	}, &refused); status != http.StatusBadRequest || refused.Error != "invalid form data" {
		t.Errorf("text past a JSON post's limit: status %d (%s), want 400 invalid form data", status, refused.Error)
	}
	many := make([]formPart, maxFormParts+1)
	for i := range many {
		many[i] = formPart{name: "x" + strconv.Itoa(i), value: "y"}
	}
	if status := h.multipartPost("/api/boards/general/threads", many, &refused); status != http.StatusBadRequest {
		t.Errorf("%d parts: status %d (%s), want 400", len(many), status, refused.Error)
	}
}

// An upload is read once, into a buffer of its size: nothing goes to a
// temporary file to be copied back, which on Cloud Run, whose disk is
// memory, would be the upload held twice. Measured over a reply its thread
// refuses after reading it, so nothing is decoded.
func TestAnUploadIsReadOnce(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("general")
	thread := h.createThread("general", "", "", "op")
	h.exec(`UPDATE threads SET locked = true`)

	image := noisyPNGBytes(t, 1024, 1024)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile(uploadFieldName, "photo.png")
	part.Write(image)
	form.Close()
	path := fmt.Sprintf("%s/api/boards/general/threads/%d/posts", h.base, thread.No)
	send := func() {
		req, err := http.NewRequest(http.MethodPost, path, bytes.NewReader(body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", form.FormDataContentType())
		var out createdPost
		if status := h.send(req, &out); status != http.StatusForbidden {
			t.Fatalf("reply to a locked thread: status %d (%s), want 403", status, out.Error)
		}
	}
	send() // connections, prepared statements

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	send()
	runtime.ReadMemStats(&after)
	if got, most := after.TotalAlloc-before.TotalAlloc, uint64(len(image))*3/2; got > most {
		t.Errorf("reading a %d-byte upload allocated %d bytes; want under %d", len(image), got, most)
	}
}

// postDeadline leaves the commit and the answer time inside the server's
// write timeout, which runs from the same moment.
func TestPostDeadlineLeavesTimeToAnswer(t *testing.T) {
	if WriteTimeout-postDeadline < 5*time.Second {
		t.Errorf("postDeadline %v leaves %v of WriteTimeout %v for the commit and the answer; want 5s at least",
			postDeadline, WriteTimeout-postDeadline, WriteTimeout)
	}
}
