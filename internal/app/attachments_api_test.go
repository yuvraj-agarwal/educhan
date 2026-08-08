package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

/* Fixtures ----------------------------------------------------------------- */

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// noisyPNGBytes produces a PNG that barely compresses, so its encoded size is
// predictably large.
func noisyPNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if _, err := rand.Read(img.Pix); err != nil {
		t.Fatalf("random pixels: %v", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(y), G: 80, B: uint8(x), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

/* Multipart client --------------------------------------------------------- */

// upload posts a multipart submission, attaching a file when filename is set.
// It returns the status and decodes the response into out.
func (c *client) upload(path string, fields map[string]string, filename string, file []byte, out any) int {
	c.t.Helper()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := form.WriteField(k, v); err != nil {
			c.t.Fatalf("write field %s: %v", k, err)
		}
	}
	if filename != "" {
		part, err := form.CreateFormFile(uploadFieldName, filename)
		if err != nil {
			c.t.Fatalf("create file part: %v", err)
		}
		if _, err := part.Write(file); err != nil {
			c.t.Fatalf("write file part: %v", err)
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

// fetch performs a GET and returns the raw response, which media tests need for
// their headers and bodies.
func (c *client) fetch(path string, header http.Header) (*http.Response, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	for k, values := range header {
		req.Header[k] = values
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return res, payload
}

/* Tests -------------------------------------------------------------------- */

func TestCreateThreadWithImage(t *testing.T) {
	c := testServer(t, nil)
	original := pngBytes(t, 800, 400)

	var created createdPost
	if status := c.upload("/api/boards/general/threads",
		map[string]string{"subject": "with image", "name": "uploader", "body": "look at this"},
		"holiday photo.png", original, &created); status != http.StatusCreated {
		t.Fatalf("create thread: status %d (%s)", status, created.Error)
	}

	detail := fetchThread(t, c, "general", created.ThreadNo)
	if len(detail.Posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(detail.Posts))
	}
	att := detail.Posts[0].Attachment
	if att == nil {
		t.Fatal("opening post has no attachment")
	}
	if att.Name != "holiday photo.png" {
		t.Errorf("name = %q, want the uploaded filename", att.Name)
	}
	if att.ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png", att.ContentType)
	}
	if att.Width != 800 || att.Height != 400 {
		t.Errorf("dimensions = %dx%d, want 800x400", att.Width, att.Height)
	}
	if att.ThumbWidth != 250 || att.ThumbHeight != 125 {
		t.Errorf("thumb = %dx%d, want 250x125", att.ThumbWidth, att.ThumbHeight)
	}
	if att.Size != int64(len(original)) {
		t.Errorf("size = %d, want %d", att.Size, len(original))
	}
	if !strings.HasPrefix(att.URL, "/media/original/general/") {
		t.Errorf("url = %q, want a /media/original/general/ prefix", att.URL)
	}
	// The stored filename must be random rather than derived from the upload.
	if strings.Contains(att.URL, "holiday") {
		t.Errorf("url = %q, want a generated filename", att.URL)
	}

	res, payload := c.fetch(att.URL, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get original: status %d", res.StatusCode)
	}
	if !bytes.Equal(payload, original) {
		t.Error("served original differs from the uploaded bytes")
	}
	if got := res.Header.Get("Cache-Control"); got != mediaCacheControl {
		t.Errorf("cache control = %q, want %q", got, mediaCacheControl)
	}

	res, payload = c.fetch(att.ThumbURL, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get thumbnail: status %d", res.StatusCode)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode served thumbnail: %v", err)
	}
	if format != "png" {
		t.Errorf("thumbnail format = %q, want png", format)
	}
	if cfg.Width != att.ThumbWidth || cfg.Height != att.ThumbHeight {
		t.Errorf("served thumbnail is %dx%d, want %dx%d", cfg.Width, cfg.Height, att.ThumbWidth, att.ThumbHeight)
	}

	// The index and catalog carry the opening post's image as a thumbnail.
	index := fetchIndex(t, c, "general", 1)
	if len(index.Threads) != 1 {
		t.Fatalf("got %d threads in the index, want 1", len(index.Threads))
	}
	if index.Threads[0].OP.Attachment == nil || index.Threads[0].OP.Attachment.ThumbURL != att.ThumbURL {
		t.Errorf("index op attachment = %+v, want the uploaded image", index.Threads[0].OP.Attachment)
	}

	var catalog Catalog
	c.get("/api/boards/general/catalog", &catalog)
	if len(catalog.Threads) != 1 || catalog.Threads[0].Attachment == nil {
		t.Fatalf("catalog tile has no attachment: %+v", catalog.Threads)
	}
	if catalog.Threads[0].Attachment.ThumbURL != att.ThumbURL {
		t.Errorf("catalog thumb = %q, want %q", catalog.Threads[0].Attachment.ThumbURL, att.ThumbURL)
	}
}

func TestReplyWithImageAndNoBody(t *testing.T) {
	c := testServer(t, nil)
	thread := c.createThread("general", "text only op", "", "no image here")

	var created createdPost
	path := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)
	if status := c.upload(path, map[string]string{"name": "replier", "body": "  "},
		"reply.jpg", jpegBytes(t, 400, 900), &created); status != http.StatusCreated {
		t.Fatalf("reply: status %d (%s)", status, created.Error)
	}

	detail := fetchThread(t, c, "general", thread.ThreadNo)
	if len(detail.Posts) != 2 {
		t.Fatalf("got %d posts, want 2", len(detail.Posts))
	}
	if detail.Posts[0].Attachment != nil {
		t.Error("text-only opening post should have no attachment")
	}
	att := detail.Posts[1].Attachment
	if att == nil {
		t.Fatal("reply has no attachment")
	}
	if att.ContentType != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", att.ContentType)
	}
	// Portrait images are bounded on their height instead.
	if att.ThumbHeight != 250 || att.ThumbWidth != 111 {
		t.Errorf("thumb = %dx%d, want 111x250", att.ThumbWidth, att.ThumbHeight)
	}
	if detail.Posts[1].Body != "" {
		t.Errorf("body = %q, want empty", detail.Posts[1].Body)
	}

	// The index shows the reply's image on the last-replies tail.
	index := fetchIndex(t, c, "general", 1)
	if len(index.Threads) != 1 || len(index.Threads[0].LastReplies) != 1 {
		t.Fatalf("index does not show the reply: %+v", index.Threads)
	}
	if index.Threads[0].LastReplies[0].Attachment == nil {
		t.Error("the reply's image is missing from the index")
	}
}

func TestUploadRejections(t *testing.T) {
	c := testServer(t, func(cfg *Config) { cfg.MaxUploadBytes = 1 << 20 })
	target := "/api/boards/general/threads"

	t.Run("non-image payload", func(t *testing.T) {
		var out createdPost
		status := c.upload(target, map[string]string{"body": "sneaky"},
			"payload.png", []byte("#!/bin/sh\nrm -rf /\n"), &out)
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", status, out.Error)
		}
		if !strings.Contains(out.Error, "unsupported image format") {
			t.Errorf("error = %q, want an unsupported-format error", out.Error)
		}
	})

	t.Run("oversize attachment", func(t *testing.T) {
		big := noisyPNGBytes(t, 800, 800)
		if int64(len(big)) <= c.app.cfg.MaxUploadBytes {
			t.Fatalf("fixture is %d bytes, expected it to exceed the %d byte limit",
				len(big), c.app.cfg.MaxUploadBytes)
		}
		var out createdPost
		if status := c.upload(target, map[string]string{"body": "big"}, "big.png", big, &out); status != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 (%s)", status, out.Error)
		}
	})

	t.Run("no body and no image", func(t *testing.T) {
		var out createdPost
		if status := c.upload(target, map[string]string{"body": "   "}, "", nil, &out); status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", status)
		}
		if !strings.Contains(out.Error, "body is required") {
			t.Errorf("error = %q, want a body-required error", out.Error)
		}
	})

	t.Run("nothing is stored for rejected uploads", func(t *testing.T) {
		var count int
		if err := c.app.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM attachments`).Scan(&count); err != nil {
			t.Fatalf("count attachments: %v", err)
		}
		if count != 0 {
			t.Errorf("stored %d attachments, want 0", count)
		}
	})
}

func TestJSONSubmissionsCarryNoAttachment(t *testing.T) {
	c := testServer(t, nil)

	thread := c.createThread("general", "json", "curl", "still text-only")
	c.reply("general", thread.ThreadNo, "curl", "a reply", false)

	var raw map[string]any
	c.get(fmt.Sprintf("/api/boards/general/threads/%d", thread.ThreadNo), &raw)
	detail := fetchThread(t, c, "general", thread.ThreadNo)
	for _, p := range detail.Posts {
		if p.Attachment != nil {
			t.Errorf("post No. %d unexpectedly has an attachment", p.No)
		}
	}
	// Attachments are omitted entirely rather than serialised as null.
	posts, _ := raw["posts"].([]any)
	for _, p := range posts {
		if _, ok := p.(map[string]any)["attachment"]; ok {
			t.Error("a post without an image should omit the attachment field")
		}
	}
}

func TestMediaServingEdgeCases(t *testing.T) {
	c := testServer(t, nil)

	cases := map[string]string{
		"missing object":     "/media/original/general/2026/08/does-not-exist.png",
		"missing thumbnail":  "/media/thumb/general/2026/08/does-not-exist.png",
		"escaped traversal":  "/media/original/general/%2e%2e/%2e%2e/%2e%2e/etc/passwd",
		"encoded null byte":  "/media/thumb/general/%00.png",
		"directory not file": "/media/original/general",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			res, payload := c.fetch(path, nil)
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (%s)", res.StatusCode, payload)
			}
			if strings.Contains(string(payload), "root:") {
				t.Fatal("response leaked file contents from outside the media root")
			}
		})
	}
}

func TestMediaSupportsRangeRequests(t *testing.T) {
	c := testServer(t, nil)
	original := pngBytes(t, 300, 300)

	var created createdPost
	if status := c.upload("/api/boards/general/threads",
		map[string]string{"body": "ranged"}, "range.png", original, &created); status != http.StatusCreated {
		t.Fatalf("create thread: status %d (%s)", status, created.Error)
	}
	att := fetchThread(t, c, "general", created.ThreadNo).Posts[0].Attachment

	res, payload := c.fetch(att.URL, http.Header{"Range": []string{"bytes=0-7"}})
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", res.StatusCode)
	}
	if !bytes.Equal(payload, original[:8]) {
		t.Errorf("range body = %v, want the first 8 bytes of the original", payload)
	}
}
