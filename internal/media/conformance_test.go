package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newStore builds an empty Store for one subtest. Each call must hand back a
// namespace of its own, so subtests cannot see each other's objects.
type newStore func(t *testing.T) Store

// testStoreContract is the behaviour every Store owes its callers, run against
// each backend in turn. Handlers and SQL are written against this interface and
// nothing else, so a backend that passes here is a drop-in for any other: the
// suite is the definition of "invisible above the interface".
//
// Backend-specific properties — where a LocalStore puts bytes on disk, how a
// GCSStore names objects in a bucket — belong in that backend's own test file.
func testStoreContract(t *testing.T, factory newStore) {
	t.Helper()

	t.Run("round trip", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		key := "originals/general/2026/08/abc.png"
		payload := []byte("pretend this is a png")

		if err := store.Put(ctx, key, "image/png", bytes.NewReader(payload)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		rc, obj, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer rc.Close()

		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read object: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("Get returned %q, want %q", got, payload)
		}
		if obj.ContentType != "image/png" {
			t.Errorf("content type = %q, want image/png", obj.ContentType)
		}
		if obj.Size != int64(len(payload)) {
			t.Errorf("size = %d, want %d", obj.Size, len(payload))
		}
		if obj.ModTime.IsZero() {
			t.Error("mod time is zero; http.ServeContent needs it for conditional requests")
		}
	})

	t.Run("content type is derived when the hint is empty", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		key := "originals/general/2026/08/derived.jpg"

		if err := store.Put(ctx, key, "", strings.NewReader("x")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		rc, obj, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer rc.Close()
		if obj.ContentType != FormatJPEG.ContentType {
			t.Errorf("content type = %q, want %q from the extension", obj.ContentType, FormatJPEG.ContentType)
		}
	})

	t.Run("put replaces an existing object", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		key := "originals/general/2026/08/replaced.png"

		if err := store.Put(ctx, key, "image/png", strings.NewReader("first")); err != nil {
			t.Fatalf("first Put: %v", err)
		}
		if err := store.Put(ctx, key, "image/png", strings.NewReader("second")); err != nil {
			t.Fatalf("second Put: %v", err)
		}
		got := mustRead(t, store, key)
		if string(got) != "second" {
			t.Errorf("object = %q, want the second write", got)
		}
	})

	t.Run("missing key is ErrNotFound", func(t *testing.T) {
		store := factory(t)
		if _, _, err := store.Get(context.Background(), "originals/general/2026/08/absent.png"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get error = %v, want ErrNotFound", err)
		}
	})

	t.Run("delete removes the object and is idempotent", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		key := "originals/general/2026/08/doomed.png"

		if err := store.Put(ctx, key, "image/png", strings.NewReader("bytes")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get after delete error = %v, want ErrNotFound", err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("Delete of a missing key should be a no-op, got %v", err)
		}
	})

	// A key naming the directory an object sits in addresses nothing. The
	// filesystem backend has a real directory there and object stores have only
	// a shared name prefix, and both have to answer the same way or
	// /media/original/general would serve something on one backend and 404 on
	// the other.
	t.Run("a path prefix is not an object", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		if err := store.Put(ctx, "originals/general/a.png", "image/png", strings.NewReader("x")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, _, err := store.Get(ctx, "originals/general"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(prefix) error = %v, want ErrNotFound", err)
		}
	})

	t.Run("rejects escaping keys", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		escaping := []string{
			"../escaped.png",
			"originals/../../etc/passwd",
			"/etc/passwd",
			"originals/./general/a.jpg",
			`originals\general\a.jpg`,
			"originals/general/a.jpg\x00",
			"",
		}
		for _, key := range escaping {
			if err := store.Put(ctx, key, "image/png", strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Put(%q) error = %v, want ErrInvalidKey", key, err)
			}
			if _, _, err := store.Get(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Get(%q) error = %v, want ErrInvalidKey", key, err)
			}
			if err := store.Delete(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Delete(%q) error = %v, want ErrInvalidKey", key, err)
			}
		}
	})

	t.Run("save writes both renditions", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		original := encodePNG(t, 640, 480)
		at := time.Date(2026, time.August, 7, 9, 30, 0, 0, time.UTC)

		img, err := Save(ctx, store, "general", original, at)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if !strings.HasPrefix(img.Path, "general/2026/08/") {
			t.Errorf("path = %q, want a general/2026/08/ prefix", img.Path)
		}
		if img.Width != 640 || img.Height != 480 {
			t.Errorf("dimensions = %dx%d, want 640x480", img.Width, img.Height)
		}
		if img.ThumbWidth != ThumbMaxDim {
			t.Errorf("thumb width = %d, want %d", img.ThumbWidth, ThumbMaxDim)
		}

		rc, obj, err := store.Get(ctx, OriginalKey(img.Path))
		if err != nil {
			t.Fatalf("get original: %v", err)
		}
		stored, _ := io.ReadAll(rc)
		rc.Close()
		// What is stored is the upload rewritten without its metadata, the
		// same on every backend: the bytes Save hashed, and the same pixels.
		if sum := sha256.Sum256(stored); hex.EncodeToString(sum[:]) != img.Checksum || int64(len(stored)) != img.Size {
			t.Error("stored original is not the file Save described")
		}
		want, _, err := image.Decode(bytes.NewReader(original))
		if err != nil {
			t.Fatalf("decode upload: %v", err)
		}
		got, _, err := image.Decode(bytes.NewReader(stored))
		if err != nil {
			t.Fatalf("decode stored original: %v", err)
		}
		requireSamePixels(t, want, got)
		if obj.ContentType != img.ContentType {
			t.Errorf("stored content type = %q, want %q", obj.ContentType, img.ContentType)
		}

		rc, _, err = store.Get(ctx, ThumbKey(img.ThumbPath))
		if err != nil {
			t.Fatalf("get thumbnail: %v", err)
		}
		defer rc.Close()
		cfg, _, err := image.DecodeConfig(rc)
		if err != nil {
			t.Fatalf("decode stored thumbnail: %v", err)
		}
		if cfg.Width != img.ThumbWidth || cfg.Height != img.ThumbHeight {
			t.Errorf("stored thumbnail is %dx%d, want %dx%d",
				cfg.Width, cfg.Height, img.ThumbWidth, img.ThumbHeight)
		}
	})

	// Metadata is stripped before Put, so no backend ever holds the upload as
	// it came: the photo's EXIF, GPS position and comments are absent from
	// the object itself, not just from what a handler serves.
	t.Run("save stores no metadata", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		upload := cameraJPEG(t, 64, 32)
		needles := []string{"Exif", needleGPS, needleDatum, needleCamera, needleXMP, needleComment}
		requireCarries(t, upload, needles...)

		img, err := Save(ctx, store, "general", upload, time.Now())
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		requireFree(t, mustRead(t, store, OriginalKey(img.Path)), needles...)
		requireFree(t, mustRead(t, store, ThumbKey(img.ThumbPath)), needles...)
		if img.Width != 64 || img.Height != 32 {
			t.Errorf("dimensions = %dx%d, want the upright 64x32", img.Width, img.Height)
		}
	})

	t.Run("remove deletes both renditions", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		img, err := Save(ctx, store, "general", encodeJPEG(t, 300, 300), time.Now())
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := Remove(ctx, store, img); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		for _, key := range []string{OriginalKey(img.Path), ThumbKey(img.ThumbPath)} {
			if _, _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get(%q) error = %v, want ErrNotFound", key, err)
			}
		}
	})

	// Save writes two objects and has no transaction to roll back, so it undoes
	// the first itself when the second fails. Leaving the original behind would
	// orphan a blob no post ever references.
	t.Run("a failed save removes what it already wrote", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		failing := &failThumbStore{Store: store}

		if _, err := Save(ctx, failing, "general", encodePNG(t, 400, 400), time.Now()); err == nil {
			t.Fatal("Save succeeded although the thumbnail write failed")
		}
		if failing.original == "" {
			t.Fatal("the original was never written, so the test proves nothing")
		}
		if _, _, err := store.Get(ctx, failing.original); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(%q) error = %v, want the original to have been cleaned up", failing.original, err)
		}
	})

	// http.ServeContent drives the reader the way the /media handlers do: it
	// seeks to the end for the size, back to the start, and then over whatever
	// range was asked for. A backend whose reader cannot seek passes every test
	// above and still breaks media serving.
	t.Run("serves through http.ServeContent", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		key := "originals/general/2026/08/served.png"
		payload := encodePNG(t, 200, 200)
		if err := store.Put(ctx, key, "image/png", bytes.NewReader(payload)); err != nil {
			t.Fatalf("Put: %v", err)
		}

		res, body := serve(t, store, key, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
		if !bytes.Equal(body, payload) {
			t.Error("served body differs from the stored object")
		}
		if got := res.Header.Get("Content-Type"); got != "image/png" {
			t.Errorf("content type = %q, want image/png", got)
		}
		if got := res.Header.Get("Content-Length"); got != fmt.Sprint(len(payload)) {
			t.Errorf("content length = %q, want %d", got, len(payload))
		}
	})

	t.Run("serves range requests", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		key := "originals/general/2026/08/ranged.png"
		// Large enough that a range lands well past the first read buffer, so a
		// reader that ignores Seek and streams from zero is caught.
		payload := encodePNG(t, 600, 600)
		if err := store.Put(ctx, key, "image/png", bytes.NewReader(payload)); err != nil {
			t.Fatalf("Put: %v", err)
		}

		ranges := []struct {
			name       string
			header     string
			start, end int // end is exclusive
		}{
			{"leading bytes", "bytes=0-7", 0, 8},
			{"interior window", "bytes=1000-1099", 1000, 1100},
			{"open-ended tail", fmt.Sprintf("bytes=%d-", len(payload)-16), len(payload) - 16, len(payload)},
			{"suffix", "bytes=-32", len(payload) - 32, len(payload)},
		}
		for _, tc := range ranges {
			t.Run(tc.name, func(t *testing.T) {
				res, body := serve(t, store, key, http.Header{"Range": []string{tc.header}})
				if res.StatusCode != http.StatusPartialContent {
					t.Fatalf("status = %d, want 206", res.StatusCode)
				}
				want := payload[tc.start:tc.end]
				if !bytes.Equal(body, want) {
					t.Errorf("range %s returned %d bytes, want the %d bytes at offset %d",
						tc.header, len(body), len(want), tc.start)
				}
				wantRange := fmt.Sprintf("bytes %d-%d/%d", tc.start, tc.end-1, len(payload))
				if got := res.Header.Get("Content-Range"); got != wantRange {
					t.Errorf("content range = %q, want %q", got, wantRange)
				}
			})
		}
	})

	// Seeking is the operation the object backend cannot get for free, and
	// ServeContent is not the only caller: pin the semantics directly.
	t.Run("reader seeks", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		key := "originals/general/2026/08/seeked.bin"
		payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
		if err := store.Put(ctx, key, "image/png", bytes.NewReader(payload)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		rc, _, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer rc.Close()

		if got, err := rc.Seek(0, io.SeekEnd); err != nil || got != int64(len(payload)) {
			t.Fatalf("Seek(0, End) = %d, %v; want %d", got, err, len(payload))
		}
		if got, err := rc.Seek(10, io.SeekStart); err != nil || got != 10 {
			t.Fatalf("Seek(10, Start) = %d, %v; want 10", got, err)
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read after seek: %v", err)
		}
		if string(buf) != "abcde" {
			t.Errorf("read %q at offset 10, want %q", buf, "abcde")
		}
		// Relative seeks compose with reads that have already advanced.
		if got, err := rc.Seek(-5, io.SeekCurrent); err != nil || got != 10 {
			t.Fatalf("Seek(-5, Current) = %d, %v; want 10", got, err)
		}
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read after relative seek: %v", err)
		}
		if string(buf) != "abcde" {
			t.Errorf("re-read %q, want %q", buf, "abcde")
		}
		// Reading from the end yields EOF rather than an error or a hang.
		if _, err := rc.Seek(0, io.SeekEnd); err != nil {
			t.Fatalf("Seek(0, End): %v", err)
		}
		if n, err := rc.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("Read at EOF = %d, %v; want 0, io.EOF", n, err)
		}
	})
}

/* Helpers ------------------------------------------------------------------ */

// serve runs one request through http.ServeContent backed by store, the way
// App.serveMedia does, and returns the response.
func serve(t *testing.T, store Store, key string, header http.Header) (*http.Response, []byte) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/media/"+key, nil)
	for k, values := range header {
		req.Header[k] = values
	}
	rec := httptest.NewRecorder()

	rc, obj, err := store.Get(req.Context(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer rc.Close()
	rec.Header().Set("Content-Type", obj.ContentType)
	http.ServeContent(rec, req, "", obj.ModTime, rc)

	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return res, body
}

func mustRead(t *testing.T, store Store, key string) []byte {
	t.Helper()
	rc, _, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return got
}

// failThumbStore writes originals to the real backend and refuses thumbnails,
// which is the partial failure Save has to undo. It records the original's key
// so the test can check that backend rather than trusting the wrapper.
type failThumbStore struct {
	Store
	original string
}

func (f *failThumbStore) Put(ctx context.Context, key, contentType string, r io.Reader) error {
	if strings.HasPrefix(key, ThumbPrefix+"/") {
		return errors.New("media: thumbnail write failed on purpose")
	}
	if err := f.Store.Put(ctx, key, contentType, r); err != nil {
		return err
	}
	f.original = key
	return nil
}
