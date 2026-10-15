package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/media"
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

// Strings the camera fixture carries in its metadata, which the stored file
// must not.
const (
	fixtureCamera  = "educhan-test-camera"
	fixtureDatum   = "WGS-84"
	fixtureComment = "educhan test comment: taken at 51.5N"
)

// cameraJPEGBytes is a w×h JPEG as a phone writes one: an Exif APP1 with the
// camera's make, Orientation 6 (stored on its side), and a GPS IFD with a
// latitude, a datum and a processing method, then a comment. The TIFF inside
// is laid out by hand, little-endian, independently of internal/media's own
// fixtures, so a bug shared between the two cannot hide itself.
func cameraJPEGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	le := binary.LittleEndian
	entry := func(b []byte, tag, typ uint16, count, value uint32) []byte {
		b = le.AppendUint16(b, tag)
		b = le.AppendUint16(b, typ)
		b = le.AppendUint32(b, count)
		return le.AppendUint32(b, value)
	}
	camera := fixtureCamera + "\x00"
	datum := fixtureDatum + "\x00"
	method := "ASCII\x00\x00\x00GPS"

	// Offsets from the start of the TIFF: the header (8), IFD0 with three
	// entries (2+36+4), the make, then the GPS IFD with three entries
	// (2+36+4) and its two long values.
	makeAt := 8 + 42
	gpsAt := makeAt + len(camera)
	datumAt := gpsAt + 42
	methodAt := datumAt + len(datum) + len(datum)%2

	tiff := []byte("II*\x00")
	tiff = le.AppendUint32(tiff, 8)
	tiff = le.AppendUint16(tiff, 3)
	tiff = entry(tiff, 0x010F, 2, uint32(len(camera)), uint32(makeAt)) // Make
	tiff = entry(tiff, 0x0112, 3, 1, 6)                                // Orientation
	tiff = entry(tiff, 0x8825, 4, 1, uint32(gpsAt))                    // GPSInfo
	tiff = le.AppendUint32(tiff, 0)
	tiff = append(tiff, camera...)
	tiff = le.AppendUint16(tiff, 3)
	tiff = entry(tiff, 0x0001, 2, 2, uint32('N'))                        // GPSLatitudeRef
	tiff = entry(tiff, 0x0012, 2, uint32(len(datum)), uint32(datumAt))   // GPSMapDatum
	tiff = entry(tiff, 0x001B, 7, uint32(len(method)), uint32(methodAt)) // GPSProcessingMethod
	tiff = le.AppendUint32(tiff, 0)
	tiff = append(tiff, datum...)
	if len(datum)%2 == 1 {
		tiff = append(tiff, 0)
	}
	tiff = append(tiff, method...)
	if len(tiff) != methodAt+len(method) {
		t.Fatalf("TIFF laid out as %d bytes, want %d", len(tiff), methodAt+len(method))
	}

	segment := func(marker byte, payload []byte) []byte {
		n := len(payload) + 2
		return append([]byte{0xFF, marker, byte(n >> 8), byte(n)}, payload...)
	}
	plain := jpegBytes(t, w, h)
	out := append([]byte(nil), plain[:2]...) // SOI
	out = append(out, segment(0xE1, append([]byte("Exif\x00\x00"), tiff...))...)
	out = append(out, segment(0xFE, []byte(fixtureComment))...)
	return append(out, plain[2:]...)
}

// metadataNeedles are what cameraJPEGBytes carries that a stored file must not.
var metadataNeedles = []string{"Exif", "GPS", fixtureDatum, fixtureCamera, fixtureComment}

// requireNoMetadata fails if data, a file served or stored, carries any of the
// camera fixture's metadata, or any APP or COM segment at all if it is a JPEG.
func requireNoMetadata(t *testing.T, what string, data []byte) {
	t.Helper()
	for _, needle := range metadataNeedles {
		if bytes.Contains(data, []byte(needle)) {
			t.Errorf("%s carries %q", what, needle)
		}
	}
	if !bytes.HasPrefix(data, []byte{0xFF, 0xD8}) {
		return
	}
	for i := 2; i+4 <= len(data) && data[i] == 0xFF && data[i+1] != 0xDA; {
		if m := data[i+1]; (m >= 0xE0 && m <= 0xEF) || m == 0xFE {
			t.Errorf("%s has a JPEG segment with marker %#x", what, m)
		}
		i += 2 + (int(data[i+2])<<8 | int(data[i+3]))
	}
}

// requireSamePicture fails unless two encoded images decode to the same
// pixels. The PNG fixtures are lossless, so their stored form, rewritten
// without metadata, must still be the picture that was uploaded.
func requireSamePicture(t *testing.T, want, got []byte) {
	t.Helper()
	a, _, err := image.Decode(bytes.NewReader(want))
	if err != nil {
		t.Fatalf("decode uploaded image: %v", err)
	}
	b, _, err := image.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("decode served image: %v", err)
	}
	if a.Bounds() != b.Bounds() {
		t.Fatalf("served image is %v, want %v", b.Bounds(), a.Bounds())
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			if color.NRGBAModel.Convert(a.At(x, y)) != color.NRGBAModel.Convert(b.At(x, y)) {
				t.Fatalf("served image differs from the upload at (%d,%d)", x, y)
			}
		}
	}
}

/* Multipart client --------------------------------------------------------- */

// upload posts a multipart submission, attaching a file when filename is set.
// It returns the status and decodes the response into out.
func (c *client) upload(path string, fields map[string]string, filename string, file []byte, out any) int {
	c.t.Helper()
	return c.uploadWithHeaders(nil, path, fields, filename, file, out)
}

// uploadWithHeaders is upload with extra request headers, for the poster key
// and the credential a browser sends alongside a multipart post.
func (c *client) uploadWithHeaders(header http.Header, path string, fields map[string]string, filename string, file []byte, out any) int {
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
	for k, values := range header {
		req.Header[k] = values
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
	c.addBoard("general")
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
	if att.Name != "image.png" {
		t.Errorf("name = %q, want image.png: the uploaded filename is never kept", att.Name)
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
	if !strings.HasPrefix(att.URL, "/media/original/general/") {
		t.Errorf("url = %q, want a /media/original/general/ prefix", att.URL)
	}
	// The stored filename must be random rather than derived from the upload.
	if strings.Contains(att.URL, "holiday") {
		t.Errorf("url = %q, want a generated filename", att.URL)
	}

	// The original is served as stored: rewritten without its metadata, so
	// not the uploaded bytes, but the same picture, and the size the
	// attachment reports is the stored file's.
	res, payload := c.fetch(att.URL, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get original: status %d", res.StatusCode)
	}
	requireSamePicture(t, original, payload)
	if att.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d, the stored file's", att.Size, len(payload))
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

// A camera's or a phone's filename can name its owner, so whatever the file
// was called, the image is shown as "image" and the extension of the format it
// was stored in, and the name sent is written nowhere: not in the database
// and not in the log, for an upload refused as for one stored.
func TestUploadedFilenameIsNotKept(t *testing.T) {
	var logs syncBuffer
	c := testServerLogging(t, func(cfg *Config) { cfg.MaxUploadBytes = 1 << 20 }, &logs)
	c.addBoard("general")
	const marker = "body-marker-for-the-filename-scan"

	thread := c.createThread("general", "names", "", marker)
	path := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)
	uploads := []struct {
		filename string
		file     []byte
		want     string
	}{
		{"IMG_20261013_Priya.jpg", jpegBytes(t, 64, 48), "image.jpg"},
		// The extension sent is not the format: a PNG called .jpg is a PNG.
		{"Priya at the fest.jpg", pngBytes(t, 64, 48), "image.png"},
		{`C:\Users\Priya\Desktop\..\..\Priya.webp`, jpegBytes(t, 64, 48), "image.jpg"},
		{"Priya ☕ selfie.png", pngBytes(t, 64, 48), "image.png"},
		{strings.Repeat("Priya", 100) + ".gif", pngBytes(t, 64, 48), "image.png"},
	}
	for _, u := range uploads {
		var out createdPost
		if status := c.upload(path, map[string]string{"body": ""}, u.filename, u.file, &out); status != http.StatusCreated {
			t.Fatalf("reply with %q: status %d (%s)", u.filename, status, out.Error)
		}
	}
	// Each way an upload is refused once its part has been read: not an
	// image, an image that does not decode, and a file over the size limit.
	refused := []struct {
		filename string
		file     []byte
		status   int
	}{
		{"Priya's notes.png", []byte("these are not the pixels you are looking for"), http.StatusBadRequest},
		{"Priya_broken.jpg", jpegBytes(t, 64, 48)[:300], http.StatusBadRequest},
		{"Priya_huge.png", make([]byte, 3<<20), http.StatusRequestEntityTooLarge},
	}
	for _, u := range refused {
		var out createdPost
		if status := c.upload(path, map[string]string{"body": ""}, u.filename, u.file, &out); status != u.status {
			t.Fatalf("reply with %q: status %d (%s), want %d", u.filename, status, out.Error, u.status)
		}
	}

	posts := fetchThread(t, c, "general", thread.ThreadNo).Posts
	if len(posts) != len(uploads)+1 {
		t.Fatalf("thread has %d posts, want %d", len(posts), len(uploads)+1)
	}
	for i, u := range uploads {
		att := posts[i+1].Attachment
		if att == nil {
			t.Fatalf("reply %d has no attachment", i+1)
		}
		if att.Name != u.want {
			t.Errorf("uploaded as %q: name = %q, want %q", u.filename, att.Name, u.want)
		}
		if strings.Contains(att.URL, "Priya") || strings.Contains(att.ThumbURL, "Priya") {
			t.Errorf("uploaded as %q: URLs %s and %s carry the filename", u.filename, att.URL, att.ThumbURL)
		}
	}

	// The scans have to be able to find what is there before their silence
	// about the filename means anything.
	if n := c.rowsContaining("posts", []byte(marker)); n != 1 {
		t.Fatalf("the scan found the body marker in %d posts, want 1; it cannot be trusted", n)
	}
	if n := c.rowsContaining("attachments", []byte("image.png")); n != 3 {
		t.Fatalf("the scan found image.png in %d attachments, want 3; it cannot be trusted", n)
	}
	for _, table := range c.tables() {
		if n := c.rowsContaining(table, []byte("Priya")); n != 0 {
			t.Errorf("%d rows of %s contain the uploaded filename", n, table)
		}
	}
	out := logs.String()
	for _, status := range []int{http.StatusCreated, http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		line := fmt.Sprintf(`"path":%q,"status":%d`, path, status)
		if !strings.Contains(out, line) {
			t.Fatalf("the log has no request line with %s; it cannot be trusted:\n%s", line, out)
		}
	}
	if strings.Contains(out, "Priya") {
		t.Errorf("the log contains the uploaded filename:\n%s", out)
	}
}

func TestReplyWithImageAndNoBody(t *testing.T) {
	c := testServer(t, nil)
	c.addBoard("general")
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
	c.addBoard("general")
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

// pngClaiming is a one-pixel PNG whose header claims to be w×h. IHDR follows
// the 8-byte signature: its length, its type, the width and the height, and
// after the rest of its data a CRC over the type and the data.
func pngClaiming(t *testing.T, w, h uint32) []byte {
	t.Helper()
	data := pngBytes(t, 1, 1)
	binary.BigEndian.PutUint32(data[16:20], w)
	binary.BigEndian.PutUint32(data[20:24], h)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

// pngDeepClaiming is pngClaiming with sixteen bits a channel, which image/png
// decodes to eight bytes a pixel, and interlaced, which it decodes a pass at a
// time beside the whole.
func pngDeepClaiming(t *testing.T, w, h uint32) []byte {
	t.Helper()
	data := pngClaiming(t, w, h)
	data[24], data[28] = 16, 1
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

// A file whose header shows it cannot be used, because it is not an accepted
// image, has too many pixels, or is encoded in a way that would take more
// memory to decode than the server has for it, is refused before flood
// control, so the poster
// can send the text on its own at once. An image whose header is sound but
// whose pixels do not decode, or that would be stored many times larger than
// it came in, is found only by decoding and encoding it, after the cooldowns
// have started, which are the exceptions the README names.
func TestImagesRefusedByTheirHeaderStartNoCooldown(t *testing.T) {
	c := testServer(t, func(cfg *Config) {
		cfg.TrustedProxyHops = 1
		cfg.ThreadCooldown = time.Minute
		cfg.PostCooldown = time.Minute
		cfg.UploadCooldown = time.Minute
	})
	c.addBoard("general")
	threads := "/api/boards/general/threads"
	unusable := []struct {
		name, filename string
		data           []byte
		want           string
	}{
		{"not an image", "notes.png", []byte("#!/bin/sh\necho hello\n"), "unsupported image format"},
		{"too many pixels", "huge.png", pngClaiming(t, 10000, 10000), "image resolution is too large"},
		{"too costly to decode", "deep.png", pngDeepClaiming(t, 6000, 4000), "image resolution is too large"},
	}

	const poster = "198.51.100.21"
	from := http.Header{"X-Forwarded-For": {poster}}
	for _, tc := range unusable {
		var out createdPost
		if status := c.uploadWithHeaders(from, threads, map[string]string{"body": "look"}, tc.filename, tc.data, &out); status != http.StatusBadRequest || !strings.Contains(out.Error, tc.want) {
			t.Errorf("thread with %s: %d %q, want 400 %q", tc.name, status, out.Error, tc.want)
		}
	}
	var thread createdPost
	if status := c.postFrom(poster, threads, map[string]any{"body": "look, no image"}, &thread); status != http.StatusCreated {
		t.Fatalf("text thread after the refusals: %d %q, want 201: a refused image started the thread cooldown", status, thread.Error)
	}

	replies := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)
	for _, tc := range unusable {
		var out createdPost
		if status := c.uploadWithHeaders(from, replies, map[string]string{"body": "look"}, tc.filename, tc.data, &out); status != http.StatusBadRequest || !strings.Contains(out.Error, tc.want) {
			t.Errorf("reply with %s: %d %q, want 400 %q", tc.name, status, out.Error, tc.want)
		}
	}
	var reply createdPost
	if status := c.postFrom(poster, replies, map[string]any{"body": "and no image"}, &reply); status != http.StatusCreated {
		t.Errorf("text reply after the refusals: %d %q, want 201: a refused image started the reply cooldown", status, reply.Error)
	}

	// The header of a PNG and none of its pixels.
	const another = "198.51.100.22"
	var out createdPost
	if status := c.uploadWithHeaders(http.Header{"X-Forwarded-For": {another}}, replies, map[string]string{"body": "look"},
		"cut.png", pngBytes(t, 24, 24)[:33], &out); status != http.StatusBadRequest || !strings.Contains(out.Error, "image could not be decoded") {
		t.Fatalf("reply with pixels missing: %d %q, want 400 %q", status, out.Error, "image could not be decoded")
	}
	if status := c.postFrom(another, replies, map[string]any{"body": "and no image"}, &out); status != http.StatusTooManyRequests {
		t.Errorf("text reply after an image that failed to decode: %d, want 429: decoding comes after flood control", status)
	}

	// A file that would be stored many times larger than it came in, which
	// only encoding it can find, is refused for that, not for its resolution.
	repeat, err := os.ReadFile(filepath.Join("..", "media", "testdata", "repeat.webp"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	const third = "198.51.100.23"
	if status := c.uploadWithHeaders(http.Header{"X-Forwarded-For": {third}}, replies, map[string]string{"body": "look"},
		"repeat.webp", repeat, &out); status != http.StatusBadRequest || out.Error != media.ErrStoredTooLarge.Error() {
		t.Fatalf("reply with a file that outgrows its upload: %d %q, want 400 %q", status, out.Error, media.ErrStoredTooLarge.Error())
	}
	if status := c.postFrom(third, replies, map[string]any{"body": "and no image"}, &out); status != http.StatusTooManyRequests {
		t.Errorf("text reply after an image too large once re-encoded: %d, want 429: encoding comes after flood control", status)
	}
}

// TestUploadsSwitchedOff covers the default deployment: a post carrying an
// image is refused before anything is stored, and text posts are unaffected,
// including multipart ones whose file input was left empty.
func TestUploadsSwitchedOff(t *testing.T) {
	c := testServer(t, func(cfg *Config) { cfg.UploadsEnabled = false })
	c.addBoard("general")
	image := pngBytes(t, 64, 64)

	var refused createdPost
	if status := c.upload("/api/boards/general/threads", map[string]string{"body": "pic"},
		"pic.png", image, &refused); status != http.StatusForbidden {
		t.Fatalf("thread with image: status %d, want 403", status)
	}
	if refused.Error != "image uploads are switched off on this server" {
		t.Errorf("error = %q, want the exact switched-off refusal clients match on", refused.Error)
	}

	var thread createdPost
	if status := c.upload("/api/boards/general/threads", map[string]string{"body": "text only"},
		"", nil, &thread); status != http.StatusCreated {
		t.Fatalf("multipart thread without image: status %d (%s)", status, thread.Error)
	}
	var reply createdPost
	if status := c.upload(fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo),
		map[string]string{"body": "empty file input"}, "", nil, &reply); status != http.StatusCreated {
		t.Fatalf("multipart reply without image: status %d (%s)", status, reply.Error)
	}
	if status := c.upload(fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo),
		map[string]string{"body": "empty part"}, "blank.png", nil, &reply); status != http.StatusCreated {
		t.Fatalf("reply with an empty file part: status %d (%s)", status, reply.Error)
	}
	if status := c.upload(fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo),
		map[string]string{"body": "pic"}, "pic.png", image, &refused); status != http.StatusForbidden {
		t.Fatalf("reply with image: status %d, want 403", status)
	}

	var count int
	if err := c.app.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM attachments`).Scan(&count); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if count != 0 {
		t.Errorf("stored %d attachments, want 0", count)
	}
	entries, err := os.ReadDir(c.app.cfg.UploadDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read upload dir: %v", err)
	}
	for _, e := range entries {
		if files, _ := os.ReadDir(filepath.Join(c.app.cfg.UploadDir, e.Name())); len(files) > 0 {
			t.Errorf("upload dir %s has %d files, want none", e.Name(), len(files))
		}
	}
}

// With uploads switched off an image post is refused for that reason however
// large its file: a file over the size limit is a 403 that names the switch,
// not a 413 that sends the poster off to find a smaller one, and the server
// stops reading once the body is past what text alone could be. A refusal
// stores nothing and starts no cooldown, so the text can go straight away on
// its own, and the longest legal post still fits under the smaller limit.
func TestUploadsSwitchedOffRefusesALargeFileUnread(t *testing.T) {
	const limit = 1 << 20
	// Past what even a server taking images reads before it gives up.
	big := make([]byte, limit+multipartOverhead+1)
	if _, err := rand.Read(big); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	small := pngBytes(t, 64, 64)

	// The control: a server taking images refuses this file for its size.
	on := testServer(t, func(cfg *Config) { cfg.MaxUploadBytes = limit })
	on.addBoard("general")
	var tooBig createdPost
	if status := on.upload("/api/boards/general/threads", map[string]string{"body": "big"},
		"big.png", big, &tooBig); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("uploads on: status %d (%s), want 413", status, tooBig.Error)
	}

	c := testServer(t, func(cfg *Config) {
		cfg.UploadsEnabled = false
		cfg.MaxUploadBytes = limit
		cfg.ThreadCooldown = time.Minute
		cfg.PostCooldown = time.Minute
		cfg.UploadCooldown = time.Minute
	})
	c.addBoard("general")
	refuse := func(what, path string, file []byte) {
		t.Helper()
		var out createdPost
		status := c.upload(path, map[string]string{"body": what}, "pic.png", file, &out)
		if status != http.StatusForbidden || out.Error != uploadsSwitchedOff {
			t.Errorf("%s: status %d %q, want 403 %q", what, status, out.Error, uploadsSwitchedOff)
		}
	}

	refuse("thread with a small image", "/api/boards/general/threads", small)
	refuse("thread with an image over the size limit", "/api/boards/general/threads", big)
	// The thread cooldown is a minute, so this goes through only if neither
	// refusal started it.
	var thread createdPost
	if status := c.upload("/api/boards/general/threads", map[string]string{"body": "the text alone"},
		"", nil, &thread); status != http.StatusCreated {
		t.Fatalf("text-only thread after the refusals: status %d (%s)", status, thread.Error)
	}

	replies := fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo)
	refuse("reply with a small image", replies, small)
	refuse("reply with an image over the size limit", replies, big)
	// The longest post the rules allow, written in four-byte characters, with
	// the empty file part a browser sends for an untouched input.
	line := strings.Repeat("𝑥", 79)
	longest := map[string]string{
		"name": strings.Repeat("𝑥", maxNameLen),
		"body": strings.TrimSuffix(strings.Repeat(line+"\r\n", maxBodyLines), "\r\n"),
	}
	var reply createdPost
	if status := c.upload(replies, longest, "blank.png", nil, &reply); status != http.StatusCreated {
		t.Fatalf("the longest legal reply after the refusals: status %d (%s)", status, reply.Error)
	}
	if got := fetchThread(t, c, "general", thread.ThreadNo).Posts[1].Body; got != strings.ReplaceAll(longest["body"], "\r\n", "\n") {
		t.Errorf("the longest legal reply came back as %d characters, want it whole", len([]rune(got)))
	}

	c.app.uploadLimiter.mu.Lock()
	uploadKeys := len(c.app.uploadLimiter.seen)
	c.app.uploadLimiter.mu.Unlock()
	if uploadKeys != 0 {
		t.Errorf("the upload cooldown holds %d clients, want none: nothing was uploaded", uploadKeys)
	}
	var count int
	if err := c.app.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM attachments`).Scan(&count); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if count != 0 {
		t.Errorf("stored %d attachments, want 0", count)
	}
	if files := filesUnder(t, c.app.cfg.UploadDir); len(files) != 0 {
		t.Errorf("upload directory holds %v, want nothing", files)
	}

	// How much of the file the server read: no more than a text post may be.
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("body", "counted")
	part, err := form.CreateFormFile(uploadFieldName, "big.png")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write(big); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	sent := &countingReader{r: &body}
	req := httptest.NewRequest(http.MethodPost, "/api/boards/general/threads", sent)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	c.app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("counted request: status %d (%s), want 403", rec.Code, rec.Body)
	}
	if sent.n > maxRequestBytes+1 {
		t.Errorf("the server read %d bytes of a %d-byte image post, want at most %d", sent.n, len(big), maxRequestBytes+1)
	}
}

// GET /api/site tells a client what the posting form can offer before anyone
// fills one in: whether images are taken at all, and the size limit behind the
// 413. It is configuration, so it carries those two fields and nothing about
// the caller.
func TestSiteReportsTheUploadSettings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		uploads bool
		limit   int64
	}{
		{"uploads on", true, 3 << 20},
		{"uploads off", false, defaultMaxUploadBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testServer(t, func(cfg *Config) {
				cfg.UploadsEnabled = tc.uploads
				cfg.MaxUploadBytes = tc.limit
			})

			res, payload := c.fetch("/api/site", nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status %d (%s), want 200", res.StatusCode, payload)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(payload, &fields); err != nil {
				t.Fatalf("decode %s: %v", payload, err)
			}
			var keys []string
			for key := range fields {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if got := strings.Join(keys, ","); got != "max_upload_bytes,uploads" {
				t.Errorf("fields = %s, want exactly max_upload_bytes,uploads", got)
			}
			var site Site
			if err := json.Unmarshal(payload, &site); err != nil {
				t.Fatalf("decode %s: %v", payload, err)
			}
			if site.Uploads != tc.uploads {
				t.Errorf("uploads = %v, want %v", site.Uploads, tc.uploads)
			}
			if site.MaxUploadBytes != tc.limit {
				t.Errorf("max_upload_bytes = %d, want the configured %d", site.MaxUploadBytes, tc.limit)
			}
			if cookies := res.Header.Values("Set-Cookie"); len(cookies) != 0 {
				t.Errorf("GET /api/site set cookies %v, want none", cookies)
			}

			// Whatever a request carries, the answer is the same bytes.
			_, again := c.fetch("/api/site", http.Header{
				"X-Forwarded-For":      {"198.51.100.23"},
				"X-Educhan-Poster-Key": {"AAAAAAAAAAAAAAAAAAAAAA"},
				"Cookie":               {"educhan_verify=whatever"},
			})
			if !bytes.Equal(payload, again) {
				t.Errorf("answered differently to a request carrying headers:\n without: %s\n with:    %s", payload, again)
			}
		})
	}

	// The limit reported is the limit enforced: one byte over it is a 413.
	c := testServer(t, func(cfg *Config) { cfg.MaxUploadBytes = 1 << 20 })
	c.addBoard("general")
	var site Site
	c.get("/api/site", &site)
	big := noisyPNGBytes(t, 800, 800)
	if int64(len(big)) <= site.MaxUploadBytes {
		t.Fatalf("fixture is %d bytes, expected it to exceed the reported %d", len(big), site.MaxUploadBytes)
	}
	var out createdPost
	if status := c.upload("/api/boards/general/threads", map[string]string{"body": "big"},
		"big.png", big, &out); status != http.StatusRequestEntityTooLarge {
		t.Errorf("an image over the reported limit: status %d (%s), want 413", status, out.Error)
	}
}

func TestJSONSubmissionsCarryNoAttachment(t *testing.T) {
	c := testServer(t, nil)
	c.addBoard("general")

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
	c.addBoard("general")
	original := pngBytes(t, 300, 300)

	var created createdPost
	if status := c.upload("/api/boards/general/threads",
		map[string]string{"body": "ranged"}, "range.png", original, &created); status != http.StatusCreated {
		t.Fatalf("create thread: status %d (%s)", status, created.Error)
	}
	att := fetchThread(t, c, "general", created.ThreadNo).Posts[0].Attachment

	_, whole := c.fetch(att.URL, nil)
	res, payload := c.fetch(att.URL, http.Header{"Range": []string{"bytes=0-7"}})
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", res.StatusCode)
	}
	if !bytes.Equal(payload, whole[:8]) {
		t.Errorf("range body = %v, want the first 8 bytes of the stored original", payload)
	}
}

/* Spoilers ----------------------------------------------------------------- */

// A spoilered image is stored and thumbnailed like any other; the flag rides
// along on the attachment in every view, for the client to cover it with.
func TestSpoilerRoundTripsThroughMultipart(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("spoil")

	var thread createdPost
	if status := h.upload("/api/boards/spoil/threads",
		map[string]string{"body": "ending of the film", "spoiler": "on"},
		"ending.png", pngBytes(t, 300, 200), &thread); status != http.StatusCreated {
		t.Fatalf("create thread: status %d (%s)", status, thread.Error)
	}

	// The three encodings the contract names, plus values that are not a yes.
	values := []struct {
		value string
		want  bool
	}{
		{"1", true}, {"true", true}, {"on", true},
		{"", false}, {"0", false}, {"false", false}, {"off", false},
	}
	path := fmt.Sprintf("/api/boards/spoil/threads/%d/posts", thread.ThreadNo)
	for _, v := range values {
		var out createdPost
		if status := h.upload(path, map[string]string{"body": "spoiler=" + v.value, "spoiler": v.value},
			"reply.png", pngBytes(t, 40, 40), &out); status != http.StatusCreated {
			t.Fatalf("reply with spoiler=%q: status %d (%s)", v.value, status, out.Error)
		}
	}
	// And a reply that does not send the field at all.
	if status, msg := h.imageReply("spoil", thread.ThreadNo, "no spoiler field"); status != http.StatusCreated {
		t.Fatalf("reply without a spoiler field: status %d (%s)", status, msg)
	}

	detail := fetchThread(t, h, "spoil", thread.ThreadNo)
	if len(detail.Posts) != len(values)+2 {
		t.Fatalf("thread has %d posts, want %d", len(detail.Posts), len(values)+2)
	}
	op := detail.Posts[0].Attachment
	if op == nil || !op.Spoiler {
		t.Fatalf("opening post attachment = %+v, want a spoiler", op)
	}
	for i, v := range values {
		att := detail.Posts[i+1].Attachment
		if att == nil || att.Spoiler != v.want {
			t.Errorf("spoiler=%q stored as %+v, want spoiler %v", v.value, att, v.want)
		}
	}
	if att := detail.Posts[len(detail.Posts)-1].Attachment; att == nil || att.Spoiler {
		t.Errorf("reply without the field stored as %+v, want no spoiler", att)
	}

	// The thumbnail is a real one: hiding it is the client's job, not the
	// server's.
	if op.ThumbWidth == 0 || op.ThumbHeight == 0 {
		t.Errorf("spoilered image has thumbnail dimensions %dx%d", op.ThumbWidth, op.ThumbHeight)
	}
	if res, _ := h.fetch(op.ThumbURL, nil); res.StatusCode != http.StatusOK {
		t.Errorf("spoilered thumbnail: status %d, want 200", res.StatusCode)
	}

	// The index and the catalog carry the flag on the opening post too.
	index := fetchIndex(t, h, "spoil", 1)
	if att := index.Threads[0].OP.Attachment; att == nil || !att.Spoiler {
		t.Errorf("index opening post attachment = %+v, want a spoiler", att)
	}
	if last := index.Threads[0].LastReplies; len(last) == 0 || last[len(last)-1].Attachment == nil ||
		last[len(last)-1].Attachment.Spoiler {
		t.Errorf("index last reply should carry an unspoilered image, got %+v", last)
	}
	var catalog Catalog
	h.get("/api/boards/spoil/catalog", &catalog)
	if att := catalog.Threads[0].Attachment; att == nil || !att.Spoiler {
		t.Errorf("catalog attachment = %+v, want a spoiler", att)
	}

	// Present as false, not omitted, so a client never has to guess.
	var raw struct {
		Posts []struct {
			Attachment map[string]json.RawMessage `json:"attachment"`
		} `json:"posts"`
	}
	h.get(fmt.Sprintf("/api/boards/spoil/threads/%d", thread.ThreadNo), &raw)
	if got := string(raw.Posts[len(raw.Posts)-1].Attachment["spoiler"]); got != "false" {
		t.Errorf("unspoilered attachment spoiler = %q, want false", got)
	}
}

// The JSON endpoints reject unknown fields, so spoiler has to be a known one
// on both. A JSON post carries no image, so the flag has nothing to apply to.
func TestSpoilerIsAcceptedInJSON(t *testing.T) {
	h := testServer(t, nil)
	h.addBoard("spoil")

	var thread createdPost
	if status := h.post("/api/boards/spoil/threads",
		map[string]any{"body": "text only", "spoiler": true}, &thread); status != http.StatusCreated {
		t.Fatalf("thread with spoiler: status %d (%s)", status, thread.Error)
	}
	var reply createdPost
	if status := h.post(fmt.Sprintf("/api/boards/spoil/threads/%d/posts", thread.ThreadNo),
		map[string]any{"body": "also text", "spoiler": true}, &reply); status != http.StatusCreated {
		t.Fatalf("reply with spoiler: status %d (%s)", status, reply.Error)
	}
	for _, p := range fetchThread(t, h, "spoil", thread.ThreadNo).Posts {
		if p.Attachment != nil {
			t.Errorf("post No. %d has an attachment from a JSON post", p.No)
		}
	}

	var out map[string]any
	if status := h.post("/api/boards/spoil/threads",
		map[string]any{"body": "x", "spoiler": "yes please"}, &out); status != http.StatusBadRequest {
		t.Errorf("spoiler as a string: status %d, want 400", status)
	}
}
