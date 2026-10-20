package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
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
// The 503 is written once the deadline has passed, inside the write deadline
// processContext set answerAllowance beyond it, so this is also what holds
// that allowance: without it the client gets no answer at all.
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

// throttledReader hands out its data a chunk at a time with a pause between, so
// a request body streams slowly as from a poor connection.
type throttledReader struct {
	data  []byte
	pos   int
	chunk int
	pause time.Duration
}

func (r *throttledReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	time.Sleep(r.pause)
	n := copy(p[:min(len(p), r.chunk)], r.data[r.pos:])
	r.pos += n
	return n, nil
}

// multipartImage builds a one-image, one-field multipart body and the
// Content-Type header that describes it.
func multipartImage(t *testing.T, body string, image []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	if err := form.WriteField("body", body); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile(uploadFieldName, "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(image); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), form.FormDataContentType()
}

// A post whose body trickles in over longer than the time its image has to be
// processed is still made on an idle server: the processing deadline starts
// after the body is read (processContext), so a slow connection no longer
// spends the image's budget and is no longer mistaken for a busy server and
// turned away 503. A body slower than the read timeout is the client's doing
// and is answered 408, with nothing stored either way; that answer is written
// once the read timeout has passed, inside the write deadline parseSubmission
// set answerAllowance beyond it, so the 408 is lost without the allowance.
func TestSlowUploadBodyIsNotMistakenForABusyServer(t *testing.T) {
	t.Run("a body slower than the processing deadline still posts", func(t *testing.T) {
		h := testServer(t, nil)
		h.addBoard("general")
		h.app.postDeadline = 1500 * time.Millisecond // less than the body takes
		h.app.uploadReadTimeout = 20 * time.Second

		body, contentType := multipartImage(t, "slow but whole", pngBytes(t, 32, 32))
		req, err := http.NewRequest(http.MethodPost, h.client.base+"/api/boards/general/threads",
			&throttledReader{data: body, chunk: len(body)/10 + 1, pause: 250 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		req.ContentLength = int64(len(body))

		start := time.Now()
		var out createdPost
		status := h.client.send(req, &out)
		if took := time.Since(start); took < h.app.postDeadline {
			t.Fatalf("body arrived in %v, not past the %v processing deadline; the test proves nothing", took, h.app.postDeadline)
		}
		if status != http.StatusCreated {
			t.Fatalf("slow upload: status %d (%s), want 201", status, out.Error)
		}
		if n := h.countRows("posts"); n != 1 {
			t.Errorf("%d posts stored, want the slow upload made", n)
		}
	})

	// The wait for room in the upload budget is the server's: a post that
	// waited still has the whole read timeout for its body, counted from when
	// there was room, and is not answered 408 for the server's own load.
	t.Run("a body that waited for room has the whole read timeout", func(t *testing.T) {
		h := testServer(t, nil)
		h.addBoard("general")
		const (
			held    = 2 * time.Second // the budget is full this long, well inside the wait postDeadline allows
			timeout = 3 * time.Second // the body's read timeout
			sending = 2400 * time.Millisecond
		)
		h.app.uploadReadTimeout = timeout

		full, err := media.ReserveUpload(context.Background(), math.MaxInt64)
		if err != nil {
			t.Fatalf("fill the upload budget: %v", err)
		}
		var once sync.Once
		free := func() { once.Do(full) }
		t.Cleanup(free)
		release := time.AfterFunc(held, free)
		defer release.Stop()

		// Expect: 100-continue keeps the body back until the server starts
		// reading it, as a slow connection whose buffers have filled does,
		// so the body takes its time from when there was room.
		body, contentType := multipartImage(t, "waited, then slow", pngBytes(t, 32, 32))
		const chunks = 12
		req, err := http.NewRequest(http.MethodPost, h.client.base+"/api/boards/general/threads",
			&throttledReader{data: body, chunk: len(body)/chunks + 1, pause: sending / chunks})
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Expect", "100-continue")
		req.ContentLength = int64(len(body))
		patient := &http.Client{Transport: &http.Transport{ExpectContinueTimeout: time.Minute}}
		t.Cleanup(patient.CloseIdleConnections)

		start := time.Now()
		resp, err := patient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		answer, _ := io.ReadAll(resp.Body)
		took := time.Since(start)
		if took < timeout {
			t.Fatalf("answered after %v, inside the %v read timeout from the start; the test proves nothing", took, timeout)
		}
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("a post that waited %v for room, then sent its body in %v: status %d (%s) after %v, want 201",
				held, sending, resp.StatusCode, answer, took)
		}
		if n := h.countRows("posts"); n != 1 {
			t.Errorf("%d posts stored, want the post made", n)
		}
	})

	t.Run("a body slower than the read timeout is answered 408", func(t *testing.T) {
		h := testServer(t, nil)
		h.addBoard("general")
		h.app.uploadReadTimeout = 400 * time.Millisecond

		status, bodyText := stalledUpload(t, h.client.base)
		if status != http.StatusRequestTimeout {
			t.Fatalf("stalled upload: status %d (%s), want 408", status, bodyText)
		}
		if !strings.Contains(bodyText, "slow") {
			t.Errorf("408 body = %q, want it to name the slow upload", bodyText)
		}
		if n := h.countRows("posts"); n != 0 {
			t.Errorf("%d posts stored for a stalled upload, want none", n)
		}
	})
}

// stalledUpload opens a raw connection, sends an image post's headers and the
// first bytes of its body, then stalls without sending the rest, and reads the
// status and body the server answers. It is a raw socket, not net/http's
// client, so the test reads the server's early answer itself rather than
// racing the client's own request-body write loop against it.
func stalledUpload(t *testing.T, base string) (int, string) {
	t.Helper()
	addr := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	preamble := "--BOUND\r\n" +
		"Content-Disposition: form-data; name=\"image\"; filename=\"a.png\"\r\n" +
		"Content-Type: image/png\r\n\r\n\x89PNG"
	req := "POST /api/boards/general/threads HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Content-Type: multipart/form-data; boundary=BOUND\r\n" +
		"Content-Length: 100000\r\n" + // the rest never comes
		"Connection: close\r\n\r\n" + preamble
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// idleBody is a raw connection that has sent an image post's headers,
// declaring a body it has not sent yet. One from idleUpload has also been
// told by the server to go on (Expect: 100-continue), which the server says
// only once its handler starts reading the body: past the wait for room,
// holding it or refused it.
type idleBody struct {
	conn    net.Conn
	read    *bufio.Reader
	chunked bool // declared no length (rawUpload)
	closes  bool // the answer said the connection closes after it (answer)
}

// rawUpload opens an idleBody for an image thread on /general/ from client
// (sent as X-Forwarded-For; none when empty), declaring size bytes of body;
// or, when size is negative, declaring no length, its body to be sent chunked
// (idleBody.trickle), so that the server learns its length only at its end.
// With expect, it asks to be told to go on before it sends the body. The
// test's cleanup closes it.
func rawUpload(t *testing.T, base, client string, size int64, expect bool) *idleBody {
	t.Helper()
	addr := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	length := "Content-Length: " + strconv.FormatInt(size, 10) + "\r\n"
	if size < 0 {
		length = "Transfer-Encoding: chunked\r\n"
	}
	req := "POST /api/boards/general/threads HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Content-Type: multipart/form-data; boundary=BOUND\r\n" +
		length +
		"Connection: close\r\n"
	if expect {
		req += "Expect: 100-continue\r\n"
	}
	if client != "" {
		req += "X-Forwarded-For: " + client + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return &idleBody{conn: conn, read: bufio.NewReader(conn), chunked: size < 0}
}

// idleUpload is a rawUpload that asks to be told to go on, once it has been.
func idleUpload(t *testing.T, base, client string, size int64) *idleBody {
	t.Helper()
	b := rawUpload(t, base, client, size, true)
	_ = b.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := b.read.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 100 ") {
		t.Fatalf("waiting to send the body: %q, %v; want 100 Continue", line, err)
	}
	if blank, err := b.read.ReadString('\n'); err != nil || blank != "\r\n" {
		t.Fatalf("after 100 Continue: %q, %v", blank, err)
	}
	return b
}

// answer reads the server's final answer to the idle body, within wait, and
// notes in closes whether it said the connection closes after it.
func (b *idleBody) answer(t *testing.T, wait time.Duration) (int, string) {
	t.Helper()
	_ = b.conn.SetReadDeadline(time.Now().Add(wait))
	resp, err := http.ReadResponse(b.read, nil)
	if err != nil {
		t.Fatalf("read the answer to an idle body: %v", err)
	}
	defer resp.Body.Close()
	b.closes = resp.Close
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// uploadFrom posts a small image thread on /general/ from client, sent as
// X-Forwarded-For.
func uploadFrom(t *testing.T, h *harness, client string, out any) int {
	t.Helper()
	body, contentType := multipartImage(t, "from "+client, pngBytes(t, 32, 32))
	req, err := http.NewRequest(http.MethodPost, h.base+"/api/boards/general/threads", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Forwarded-For", client)
	return h.client.send(req, out)
}

// drainShares waits, at the test's end, once its connections are closed, for
// the clients' posts to give back what they held of the upload budget, which
// every test in the package shares.
func drainShares(t *testing.T, h *harness, clients ...string) {
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for _, c := range clients {
			for h.app.uploadShares.of(c) != 0 {
				if time.Now().After(deadline) {
					t.Errorf("%s still holds %d bytes of the upload budget", c, h.app.uploadShares.of(c))
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
}

// A body holds its room in the upload budget from before it is read until its
// post is answered, and a post whose body never arrives never reaches flood
// control. So eight connections declaring 8 MiB bodies and sending nothing
// held the whole budget for the read timeout, turned every other image post
// away 503, and cost their client nothing. Now one client's posts, an IPv6
// client's counted by its /64 (shareKey), hold at most clientUploadShare, half
// the budget, its next post refused 429 at once; and a body that falls behind
// an even pace through its read window by more than uploadPaceGrace is cut
// off and answered 408, giving its room back, whether it sends nothing or a
// trickle.
func TestIdleUploadsCannotHoldTheUploadBudget(t *testing.T) {
	const declared = 8 << 20 // the default attachment limit, declared and never sent
	conns := int(media.UploadBudget / declared)

	t.Run("one client holds no more than its share", func(t *testing.T) {
		h := testServer(t, func(cfg *Config) { cfg.TrustedProxyHops = 1 })
		h.addBoard("general")
		h.app.uploadPaceGrace = time.Minute // so only the share is at work
		h.app.uploadReadTimeout = time.Minute
		h.app.postDeadline = 16 * time.Second // a wait for room gives up after a second
		const holder, other = "203.0.113.1", "198.51.100.2"
		drainShares(t, h, holder, other)
		for i := 0; i < conns; i++ {
			idleUpload(t, h.base, holder, declared)
		}
		if got := h.app.uploadShares.of(holder); got != clientUploadShare {
			t.Errorf("the idle client holds %d bytes of the upload budget, want its share, %d", got, clientUploadShare)
		}

		var created createdPost
		if status := uploadFrom(t, h, other, &created); status != http.StatusCreated {
			t.Fatalf("another client's image post: status %d (%s), want 201", status, created.Error)
		}
		var refused busyPost
		status := uploadFrom(t, h, holder, &refused)
		want := int(busyRetryAfter / time.Second)
		if status != http.StatusTooManyRequests || refused.RetryAfter != want ||
			h.client.lastHeader.Get("Retry-After") != strconv.Itoa(want) {
			t.Errorf("the idle client's next image post: status %d, %+v, Retry-After %q; want 429 with %ds to wait",
				status, refused, h.client.lastHeader.Get("Retry-After"), want)
		}
		if n := h.countRows("posts"); n != 1 {
			t.Errorf("%d posts stored, want the other client's", n)
		}
	})

	// A host on IPv6 can usually take any address in its /64: counted per
	// address, a few of them would hold the whole budget.
	t.Run("the addresses of one IPv6 network hold one share", func(t *testing.T) {
		h := testServer(t, func(cfg *Config) { cfg.TrustedProxyHops = 1 })
		h.addBoard("general")
		h.app.uploadPaceGrace = time.Minute // so only the share is at work
		h.app.uploadReadTimeout = time.Minute
		h.app.postDeadline = 16 * time.Second // a wait for room gives up after a second
		const network, other = "2001:db8:0:1::", "2001:db8:0:2::1"
		drainShares(t, h, shareKey(network+"1"), shareKey(other))
		for i := 1; i <= conns; i++ {
			idleUpload(t, h.base, network+strconv.Itoa(i), declared)
		}
		if got := h.app.uploadShares.of(shareKey(network + "1")); got != clientUploadShare {
			t.Errorf("the idle network holds %d bytes of the upload budget, want its share, %d", got, clientUploadShare)
		}

		var created createdPost
		if status := uploadFrom(t, h, other, &created); status != http.StatusCreated {
			t.Fatalf("another network's image post: status %d (%s), want 201", status, created.Error)
		}
		var refused busyPost
		if status := uploadFrom(t, h, network+"99", &refused); status != http.StatusTooManyRequests {
			t.Errorf("another address of the idle network: status %d (%s), want 429", status, refused.Error)
		}
	})

	t.Run("a body that does not arrive gives its room back", func(t *testing.T) {
		h := testServer(t, func(cfg *Config) { cfg.TrustedProxyHops = 1 })
		h.addBoard("general")
		h.app.uploadPaceGrace = 300 * time.Millisecond
		h.app.uploadReadTimeout = time.Minute
		h.app.postDeadline = 17 * time.Second // a wait for room gives up after two seconds
		// Two clients, each within its share, between them the whole budget.
		holders := []string{"203.0.113.1", "203.0.113.2"}
		drainShares(t, h, append(holders, "198.51.100.2")...)
		var idle []*idleBody
		for i := 0; i < conns; i++ {
			idle = append(idle, idleUpload(t, h.base, holders[i%2], declared))
		}

		var created createdPost
		if status := uploadFrom(t, h, "198.51.100.2", &created); status != http.StatusCreated {
			t.Fatalf("another client's image post: status %d (%s), want 201", status, created.Error)
		}
		for i, b := range idle {
			if status, text := b.answer(t, 5*time.Second); status != http.StatusRequestTimeout {
				t.Errorf("idle body %d: status %d (%s), want 408", i, status, text)
			}
		}
	})

	t.Run("a body that trickles is cut off", func(t *testing.T) {
		h := testServer(t, nil)
		h.addBoard("general")
		h.app.uploadPaceGrace = 300 * time.Millisecond
		h.app.uploadReadTimeout = time.Minute
		drainShares(t, h, "127.0.0.1")
		b := idleUpload(t, h.base, "", declared)
		// Twenty bytes a second, where the window asks for 8 MiB a minute.
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					if _, err := b.conn.Write([]byte("x")); err != nil {
						return
					}
				}
			}
		}()
		start := time.Now()
		status, text := b.answer(t, 10*time.Second)
		if status != http.StatusRequestTimeout {
			t.Fatalf("trickling body: status %d (%s), want 408", status, text)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("trickling body answered after %v, want soon after the %v grace, not at the %v window's end",
				took, h.app.uploadPaceGrace, h.app.uploadReadTimeout)
		}
	})
}

// trickle sends body in pieces of at most size bytes with pause after each,
// as chunks if the body was declared no length, leaving the body open, and
// reports whether all of it went. It stops early once the server has answered
// or the connection refuses a chunk, so that the test can read an early
// answer, which sending on past it could lose to the connection's reset.
func (b *idleBody) trickle(body []byte, size int, pause time.Duration) bool {
	for len(body) > 0 {
		_ = b.conn.SetReadDeadline(time.Now().Add(time.Millisecond))
		if _, err := b.read.Peek(1); err == nil {
			return false // answered
		}
		n := min(size, len(body))
		piece := string(body[:n])
		if b.chunked {
			piece = fmt.Sprintf("%x\r\n%s\r\n", n, piece)
		}
		if _, err := io.WriteString(b.conn, piece); err != nil {
			return false
		}
		body = body[n:]
		time.Sleep(pause)
	}
	return true
}

// end sends the last chunk, ending a body declared no length.
func (b *idleBody) end() {
	_, _ = io.WriteString(b.conn, "0\r\n\r\n")
}

// boundForm is an image post's multipart body under the boundary rawUpload
// declares (BOUND), led by a part of filler bytes that the server reads past.
func boundForm(t *testing.T, filler int, body string, image []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	if err := form.SetBoundary("BOUND"); err != nil {
		t.Fatal(err)
	}
	if filler > 0 {
		part, err := form.CreateFormField("filler")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(bytes.Repeat([]byte("x"), filler)); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.WriteField("body", body); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile(uploadFieldName, "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(image); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A post's body holds its room in the upload budget from before it is read,
// and a body sent without a Content-Length (chunked) would have held room for
// the most a body may be, its length known only at its end. Held to the pace
// of that, a small one sent slowly was cut off; held to none, one trickling a
// few bytes a second kept its room for the whole read window, so seven such
// bodies from three addresses, each within its share, held the whole budget
// at almost no cost, and every other image post was turned away 503. With
// uploads on, such a post is now refused 411 on its headers, before anything
// is reserved for it or read of it, and charged nothing: every body that
// holds room has a length and is held to its own pace.
func TestPostsWithoutALengthAreRefused(t *testing.T) {
	t.Run("at once, holding nothing and charged nothing", func(t *testing.T) {
		h := testServer(t, func(cfg *Config) {
			cfg.ThreadCooldown = time.Minute
			cfg.UploadCooldown = time.Minute
		})
		h.addBoard("general")
		drainShares(t, h, "127.0.0.1")

		// Nothing of the body is sent: the answer comes on the headers.
		b := rawUpload(t, h.base, "", -1, false)
		status, text := b.answer(t, 5*time.Second)
		if status != http.StatusLengthRequired || !strings.Contains(text, lengthRequired) {
			t.Fatalf("an image post declaring no length: status %d (%s), want 411 %q", status, text, lengthRequired)
		}
		if !b.closes {
			t.Error("the 411 answer keeps the connection open, want it closed: the body is not read past")
		}
		if got := h.app.uploadShares.of("127.0.0.1"); got != 0 {
			t.Errorf("the refused post's client holds %d bytes of the upload budget, want none", got)
		}

		// No cooldown was started: the same post with a length posts at once.
		var created createdPost
		if status := uploadFrom(t, h, "127.0.0.1", &created); status != http.StatusCreated {
			t.Fatalf("the same client's image post with a length: status %d (%s), want 201", status, created.Error)
		}
		if n := h.countRows("posts"); n != 1 {
			t.Errorf("%d posts stored, want only the one with a length", n)
		}
	})

	t.Run("trickles from three addresses hold no room", func(t *testing.T) {
		h := testServer(t, func(cfg *Config) { cfg.TrustedProxyHops = 1 })
		h.addBoard("general")
		h.app.uploadPaceGrace = 2 * time.Second
		h.app.uploadReadTimeout = 6 * time.Second
		h.app.postDeadline = 17 * time.Second // a wait for room gives up after two seconds
		holders := []string{"203.0.113.1", "203.0.113.1", "203.0.113.1",
			"203.0.113.2", "203.0.113.2", "203.0.113.2", "203.0.113.3"}
		const other = "198.51.100.9"
		drainShares(t, h, "203.0.113.1", "203.0.113.2", "203.0.113.3", other)
		stop := make(chan struct{})
		var trickling sync.WaitGroup
		defer func() {
			close(stop)
			trickling.Wait()
		}()
		var bodies []*idleBody
		for _, client := range holders {
			b := rawUpload(t, h.base, client, -1, false)
			bodies = append(bodies, b)
			// A byte every 100 ms, as a chunk of its own, until the
			// connection refuses one: each body well within its pace's
			// grace from one byte to the next.
			trickling.Add(1)
			go func() {
				defer trickling.Done()
				tick := time.NewTicker(100 * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-stop:
						return
					case <-tick.C:
						if _, err := io.WriteString(b.conn, "1\r\nx\r\n"); err != nil {
							return
						}
					}
				}
			}()
		}
		// Long enough for the seven to be answered, or, were they read, to
		// have taken their room.
		time.Sleep(300 * time.Millisecond)

		// An image post of more than the megabyte the seven would leave.
		body := boundForm(t, 2<<20, "from another client", pngBytes(t, 32, 32))
		req, err := http.NewRequest(http.MethodPost, h.base+"/api/boards/general/threads", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "multipart/form-data; boundary=BOUND")
		req.Header.Set("X-Forwarded-For", other)
		var created createdPost
		if status := h.client.send(req, &created); status != http.StatusCreated {
			t.Errorf("another client's %d-byte image post: status %d (%s), want 201", len(body), status, created.Error)
		}

		for i, b := range bodies {
			if status, text := b.answer(t, 2*time.Second); status != http.StatusLengthRequired {
				t.Errorf("body %d from %s, declaring no length: status %d (%s), want 411", i, holders[i], status, text)
			}
		}
		for _, client := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
			if got := h.app.uploadShares.of(client); got != 0 {
				t.Errorf("%s holds %d bytes of the upload budget, want none", client, got)
			}
		}
	})
}

// pacedBody holds a body to an even pace through its read window, with
// uploadPaceGrace to fall behind it by, not to a rate from one read to the
// next: a body ahead of its pace may pause, as an upload does when its
// connection drops for a moment, and still post. 2 MiB of a body of a little
// over 8 MiB is a quarter of it at once, about fifteen seconds ahead of the
// pace that brings it all in in a minute, and a pause of three times the
// grace leaves it well ahead.
func TestABodyAheadOfItsPaceMayPause(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("general")
	h.app.uploadPaceGrace = time.Second
	h.app.uploadReadTimeout = time.Minute
	drainShares(t, h, "127.0.0.1")
	const first, filler = 2 << 20, 8 << 20
	body := boundForm(t, filler, "fast, then a pause", pngBytes(t, 32, 32))
	pause := 3 * h.app.uploadPaceGrace

	b := idleUpload(t, h.base, "", int64(len(body)))
	if !b.trickle(body[:first], 64<<10, 0) {
		t.Fatal("the body's first 2 MiB were not taken")
	}
	time.Sleep(pause)
	if !b.trickle(body[first:], 64<<10, 0) {
		t.Fatal("the rest of the body was not taken")
	}
	if status, text := b.answer(t, 30*time.Second); status != http.StatusCreated {
		t.Fatalf("an image post that paused %v, ahead of its pace: status %d (%s), want 201", pause, status, text)
	}
	if n := h.countRows("posts"); n != 1 {
		t.Errorf("%d posts stored, want the paused upload made", n)
	}
}

// A part's header line cut short reached the form's reader as a malformed
// header, because bufio's ReadLine, which reads header lines, hands back what
// it has of a line and drops the error that cut it: so a body that stopped
// inside a header's name was answered 400 (invalid form data), not 408 for
// stopping, and one the body limit cut there 400, not what the limit answers.
// When the body's own read failed, that failure is the answer.
func TestABodyCutOffInsideAPartHeaderIsAnsweredForTheCut(t *testing.T) {
	// Where the image part's first header line is cut: inside its name, so
	// that what arrived of the line has no colon.
	cutIn := func(body []byte) int {
		i := bytes.Index(body, []byte("Content-Disposition: form-data; name=\"image\""))
		if i < 0 {
			t.Fatal("the image part's header is not in the body")
		}
		return i + len("Content-Disp")
	}
	for _, sized := range []bool{true, false} {
		// A body without a length is read only with uploads off: with them
		// on, its post is refused on its headers (refuseUnsized).
		name := "a chunked body that stops there, with uploads off, is answered 408"
		if sized {
			name = "a body with a length that stops there is answered 408"
		}
		t.Run(name, func(t *testing.T) {
			h := testServer(t, func(cfg *Config) { cfg.UploadsEnabled = sized })
			h.addBoard("general")
			// Most of a short body with a length has arrived by the cut, far
			// ahead of its pace: a short window has it cut off soon all the
			// same.
			h.app.uploadPaceGrace = 300 * time.Millisecond
			h.app.uploadReadTimeout = 2 * time.Second
			drainShares(t, h, "127.0.0.1")
			body := boundForm(t, 0, "cut in a header", pngBytes(t, 32, 32))
			size := int64(-1)
			if sized {
				size = int64(len(body))
			}

			b := idleUpload(t, h.base, "", size)
			if !b.trickle(body[:cutIn(body)], len(body), 0) {
				t.Fatal("the start of the body was not taken")
			}
			if status, text := b.answer(t, 20*time.Second); status != http.StatusRequestTimeout {
				t.Fatalf("a body that stopped inside a part's header: status %d (%s), want 408", status, text)
			}
		})
	}

	t.Run("one the limit cuts there with uploads off is answered as past the limit", func(t *testing.T) {
		h := testServer(t, func(cfg *Config) { cfg.UploadsEnabled = false })
		h.addBoard("general")
		// A text field long enough that the switched-off limit, a JSON
		// post's worth, falls inside the image part's header after it.
		probe := boundForm(t, 0, "", []byte("x"))
		text := strings.Repeat("a", maxRequestBytes-cutIn(probe))
		body := boundForm(t, 0, text, []byte("x"))
		if got := cutIn(body); got != maxRequestBytes {
			t.Fatalf("the limit cuts the body at %d, inside the header at %d; the test proves nothing", maxRequestBytes, got)
		}

		b := idleUpload(t, h.base, "", int64(len(body)))
		b.trickle(body, len(body), 0)
		status, text := b.answer(t, 20*time.Second)
		if status != http.StatusForbidden || !strings.Contains(text, uploadsSwitchedOff) {
			t.Fatalf("a body the switched-off limit cut inside a part's header: status %d (%s), want 403 %q",
				status, text, uploadsSwitchedOff)
		}
	})
}
