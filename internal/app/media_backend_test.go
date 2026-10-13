package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yuvraj-agarwal/educhan/internal/media"
	"github.com/yuvraj-agarwal/educhan/internal/media/mediatest"
)

/* Backends ----------------------------------------------------------------- */

// mediaBackend is one blob backend the whole application has to work over.
// internal/media's conformance suite proves the two stores answer alike; these
// tests prove the application does, end to end, through the real routes.
type mediaBackend struct {
	name string
	tune func(t *testing.T, cfg *Config)
}

// mediaBackends is every backend a deployment can select. Adding one here runs
// the whole file against it.
var mediaBackends = []mediaBackend{
	{
		// testServer already points UploadDir at a temporary directory, so the
		// local backend needs no configuration beyond the default.
		name: "local",
		tune: func(t *testing.T, cfg *Config) {},
	},
	{
		name: "gcs",
		tune: func(t *testing.T, cfg *Config) {
			cfg.MediaBackend = MediaBackendGCS
			// A prefix, because a real bucket is rarely educhan's alone and an
			// unprefixed test would not notice the prefix being dropped.
			cfg.GCSBucket = mediatest.StartGCS(t)
			cfg.GCSPrefix = "educhan/media"
		},
	},
}

// backendServer starts an application storing media on the given backend, with
// moderation enabled so a test can delete what it uploaded.
func backendServer(t *testing.T, backend mediaBackend) *harness {
	t.Helper()
	return testServer(t, func(cfg *Config) {
		cfg.ModKey = testModKey
		backend.tune(t, cfg)
	})
}

/* Helpers ------------------------------------------------------------------ */

// blobKeys turns an attachment's URLs back into the storage keys behind them.
// Unlike blobPaths it names objects rather than files, so it reads the same on
// every backend.
func blobKeys(t *testing.T, att *Attachment) (original, thumb string) {
	t.Helper()
	if att == nil {
		t.Fatal("post has no attachment")
	}
	return media.OriginalKey(strings.TrimPrefix(att.URL, originalURLPrefix)),
		media.ThumbKey(strings.TrimPrefix(att.ThumbURL, thumbURLPrefix))
}

// readBlob reads an object straight from a store, below the HTTP layer.
func readBlob(t *testing.T, store media.Store, key string) []byte {
	t.Helper()
	rc, _, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return data
}

func requireBlob(t *testing.T, store media.Store, key string, want bool) {
	t.Helper()
	rc, _, err := store.Get(context.Background(), key)
	if err == nil {
		rc.Close()
	}
	switch {
	case want && err != nil:
		t.Errorf("expected %s to exist: %v", key, err)
	case !want && err == nil:
		t.Errorf("expected %s to be gone", key)
	case !want && !errors.Is(err, media.ErrNotFound):
		t.Errorf("get %s: %v", key, err)
	}
}

/* Tests -------------------------------------------------------------------- */

// TestAttachmentLifecycleAcrossBackends is the whole attachment story — upload,
// thumbnail, serve, range, delete — over each backend in turn. It is what makes
// EDUCHAN_MEDIA_BACKEND a configuration choice rather than a code path with its
// own behaviour.
func TestAttachmentLifecycleAcrossBackends(t *testing.T) {
	for _, backend := range mediaBackends {
		t.Run(backend.name, func(t *testing.T) {
			c := backendServer(t, backend)
			c.addBoard("general")
			original := pngBytes(t, 800, 400)

			var created createdPost
			if status := c.upload("/api/boards/general/threads",
				map[string]string{"subject": "stored elsewhere", "body": "look at this"},
				"holiday photo.png", original, &created); status != http.StatusCreated {
				t.Fatalf("create thread: status %d (%s)", status, created.Error)
			}

			att := fetchThread(t, c, "general", created.ThreadNo).Posts[0].Attachment
			if att == nil {
				t.Fatal("opening post has no attachment")
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
			// The URL is the backend-independent part of the contract: the same
			// path whichever store answers it.
			if !strings.HasPrefix(att.URL, "/media/original/general/") {
				t.Errorf("url = %q, want a /media/original/general/ prefix", att.URL)
			}

			// What is stored is the upload rewritten without its metadata:
			// the same picture, and the same bytes on every backend.
			originalKey, _ := blobKeys(t, att)
			stored := readBlob(t, c.app.media, originalKey)

			t.Run("serves the original", func(t *testing.T) {
				res, payload := c.fetch(att.URL, nil)
				if res.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", res.StatusCode)
				}
				if !bytes.Equal(payload, stored) {
					t.Error("served original differs from the stored file")
				}
				requireSamePicture(t, original, payload)
				if got := res.Header.Get("Content-Type"); got != "image/png" {
					t.Errorf("content type = %q, want image/png", got)
				}
				if got := res.Header.Get("Cache-Control"); got != mediaCacheControl {
					t.Errorf("cache control = %q, want %q", got, mediaCacheControl)
				}
			})

			t.Run("serves the thumbnail", func(t *testing.T) {
				res, payload := c.fetch(att.ThumbURL, nil)
				if res.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", res.StatusCode)
				}
				cfg, format, err := image.DecodeConfig(bytes.NewReader(payload))
				if err != nil {
					t.Fatalf("decode served thumbnail: %v", err)
				}
				if format != "png" {
					t.Errorf("thumbnail format = %q, want png", format)
				}
				if cfg.Width != att.ThumbWidth || cfg.Height != att.ThumbHeight {
					t.Errorf("served thumbnail is %dx%d, want %dx%d",
						cfg.Width, cfg.Height, att.ThumbWidth, att.ThumbHeight)
				}
			})

			t.Run("serves range requests", func(t *testing.T) {
				// An object backend has to seek to answer these; a reader that
				// only streams forward would serve the wrong bytes here and
				// nowhere else.
				for _, tc := range []struct {
					header     string
					start, end int
				}{
					{"bytes=0-7", 0, 8},
					{"bytes=500-999", 500, 1000},
					{fmt.Sprintf("bytes=%d-", len(stored)-16), len(stored) - 16, len(stored)},
				} {
					res, payload := c.fetch(att.URL, http.Header{"Range": []string{tc.header}})
					if res.StatusCode != http.StatusPartialContent {
						t.Fatalf("%s: status = %d, want 206", tc.header, res.StatusCode)
					}
					if !bytes.Equal(payload, stored[tc.start:tc.end]) {
						t.Errorf("%s served the wrong %d bytes", tc.header, len(payload))
					}
				}
			})

			t.Run("deleting the thread removes both renditions", func(t *testing.T) {
				originalKey, thumbKey := blobKeys(t, att)
				requireBlob(t, c.app.media, originalKey, true)
				requireBlob(t, c.app.media, thumbKey, true)

				c.modAction(http.MethodDelete, fmt.Sprintf("/api/boards/general/threads/%d", created.ThreadNo))

				requireBlob(t, c.app.media, originalKey, false)
				requireBlob(t, c.app.media, thumbKey, false)
				if res, _ := c.fetch(att.URL, nil); res.StatusCode != http.StatusNotFound {
					t.Errorf("status after delete = %d, want 404", res.StatusCode)
				}
			})
		})
	}
}

// A phone photo keeps its GPS position, its camera and its comment in its
// metadata, and anyone who opened the full image could read them; its
// filename can name its owner. None of it reaches either backend: what is
// stored, and so what /media/original serves, is the picture rewritten upright
// with no metadata at all, the checksum recorded for it is of those bytes, and
// its name is image.jpg.
func TestUploadedPhotoKeepsNoMetadataOnEveryBackend(t *testing.T) {
	for _, backend := range mediaBackends {
		t.Run(backend.name, func(t *testing.T) {
			c := backendServer(t, backend)
			c.addBoard("general")
			upload := cameraJPEGBytes(t, 400, 300)
			for _, needle := range metadataNeedles {
				if !bytes.Contains(upload, []byte(needle)) {
					t.Fatalf("the fixture does not carry %q, so the test would prove nothing", needle)
				}
			}

			var created createdPost
			if status := c.upload("/api/boards/general/threads", map[string]string{"body": "from my phone"},
				"IMG_20261013_Priya.jpg", upload, &created); status != http.StatusCreated {
				t.Fatalf("create thread: status %d (%s)", status, created.Error)
			}
			att := fetchThread(t, c, "general", created.ThreadNo).Posts[0].Attachment
			if att == nil {
				t.Fatal("opening post has no attachment")
			}
			// The phone's filename names its owner; it is not kept either.
			if att.Name != "image.jpg" {
				t.Errorf("name = %q, want image.jpg", att.Name)
			}

			res, served := c.fetch(att.URL, nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("get original: status %d", res.StatusCode)
			}
			requireNoMetadata(t, "served original", served)
			_, thumb := c.fetch(att.ThumbURL, nil)
			requireNoMetadata(t, "served thumbnail", thumb)

			originalKey, thumbKey := blobKeys(t, att)
			if stored := readBlob(t, c.app.media, originalKey); !bytes.Equal(stored, served) {
				t.Error("the stored original differs from the one served")
			}
			requireNoMetadata(t, "stored thumbnail", readBlob(t, c.app.media, thumbKey))

			// Stored on its side with Orientation 6, kept upright.
			cfg, format, err := image.DecodeConfig(bytes.NewReader(served))
			if err != nil || format != "jpeg" {
				t.Fatalf("served original decodes as %q: %v", format, err)
			}
			if cfg.Width != 300 || cfg.Height != 400 || att.Width != 300 || att.Height != 400 {
				t.Errorf("served %dx%d, reported %dx%d; want the upright 300x400",
					cfg.Width, cfg.Height, att.Width, att.Height)
			}
			if att.ThumbWidth != 187 || att.ThumbHeight != 250 {
				t.Errorf("thumb = %dx%d, want the upright 187x250", att.ThumbWidth, att.ThumbHeight)
			}

			if att.Size != int64(len(served)) {
				t.Errorf("size = %d, want %d, the stored file's", att.Size, len(served))
			}
			var checksum string
			if err := c.app.pool.QueryRow(context.Background(),
				`SELECT checksum FROM attachments`).Scan(&checksum); err != nil {
				t.Fatalf("read checksum: %v", err)
			}
			if sum := sha256.Sum256(served); checksum != hex.EncodeToString(sum[:]) {
				t.Errorf("checksum = %s, want the SHA-256 of the stored file", checksum)
			}
		})
	}
}

// webpCarrying is a WebP fixture from internal/media's testdata with the
// whole of hidden appended inside its picture chunk (its first one, VP8 or
// VP8L), where a decoder never looks: the upload still shows the same
// picture.
func webpCarrying(t *testing.T, fixture string, hidden []byte) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "media", "testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if len(data) < 20 || string(data[12:15]) != "VP8" {
		t.Fatalf("%s is not a simple-format WebP", fixture)
	}
	n := int(binary.LittleEndian.Uint32(data[16:20]))
	payload := append(append([]byte(nil), data[20:20+n]...), hidden...)
	out := append([]byte("RIFF\x00\x00\x00\x00WEBP"), data[12:16]...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(payload)))
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out
}

// A WebP is stored and served as a PNG or a JPEG made from its pixels, never
// as the file that came in: a lossy one, here carrying a whole camera JPEG
// inside its bitstream chunk, comes back as a JPEG with none of it.
func TestUploadedWebPIsServedAsAFileMadeFromItsPixels(t *testing.T) {
	for _, backend := range mediaBackends {
		t.Run(backend.name, func(t *testing.T) {
			c := backendServer(t, backend)
			c.addBoard("general")
			for _, tc := range []struct {
				fixture, name, contentType string
			}{
				{"lossy.webp", "image.jpg", "image/jpeg"},
				{"sample.webp", "image.png", "image/png"},
			} {
				upload := webpCarrying(t, tc.fixture, cameraJPEGBytes(t, 32, 24))
				for _, needle := range metadataNeedles {
					if !bytes.Contains(upload, []byte(needle)) {
						t.Fatalf("the fixture does not carry %q, so the test would prove nothing", needle)
					}
				}
				var created createdPost
				if status := c.upload("/api/boards/general/threads", map[string]string{"body": "webp"},
					"cat.webp", upload, &created); status != http.StatusCreated {
					t.Fatalf("%s: create thread: status %d (%s)", tc.fixture, status, created.Error)
				}
				att := fetchThread(t, c, "general", created.ThreadNo).Posts[0].Attachment
				if att == nil {
					t.Fatalf("%s: opening post has no attachment", tc.fixture)
				}
				if att.Name != tc.name {
					t.Errorf("%s: name = %q, want %s", tc.fixture, att.Name, tc.name)
				}
				res, served := c.fetch(att.URL, nil)
				if res.StatusCode != http.StatusOK {
					t.Fatalf("%s: get original: status %d", tc.fixture, res.StatusCode)
				}
				if ct := res.Header.Get("Content-Type"); ct != tc.contentType {
					t.Errorf("%s: served as %q, want %s", tc.fixture, ct, tc.contentType)
				}
				if bytes.HasPrefix(served, []byte("RIFF")) {
					t.Errorf("%s: the served original is still a WebP", tc.fixture)
				}
				requireNoMetadata(t, tc.fixture+" served original", served)
				_, thumb := c.fetch(att.ThumbURL, nil)
				requireNoMetadata(t, tc.fixture+" served thumbnail", thumb)
				originalKey, _ := blobKeys(t, att)
				requireNoMetadata(t, tc.fixture+" stored original", readBlob(t, c.app.media, originalKey))
			}
		})
	}
}

// Blobs are written before the row that references them exists, so a
// transaction that refuses the post leaves them orphaned unless the handler
// cleans up. A reply to a thread that is already locked is turned away before
// its upload is stored, so the test locks the thread while the reply waits for
// the board's row, after the upload: the race a moderator's lock really runs,
// played in the same order every time.
func TestFailedPostRemovesItsBlobs(t *testing.T) {
	for _, backend := range mediaBackends {
		t.Run(backend.name, func(t *testing.T) {
			c := backendServer(t, backend)
			c.addBoard("general")

			// Watch every write, so the assertion covers whatever the upload
			// actually stored rather than the keys the test guessed at.
			blobs := &recordingStore{Store: c.app.media}
			c.app.media = blobs

			// Already locked: refused up front, and nothing is written.
			locked := c.createThread("general", "closed", "", "no more replies")
			c.modAction(http.MethodPost, fmt.Sprintf("/api/boards/general/threads/%d/lock", locked.ThreadNo))
			var out createdPost
			status := c.upload(fmt.Sprintf("/api/boards/general/threads/%d/posts", locked.ThreadNo),
				map[string]string{"body": "sneaking in"}, "late.png", pngBytes(t, 320, 240), &out)
			if status != http.StatusForbidden || out.Error != "thread is locked" {
				t.Fatalf("reply to a locked thread: status = %d (%s), want 403 thread is locked", status, out.Error)
			}
			if written := blobs.written(); len(written) != 0 {
				t.Fatalf("a reply refused up front wrote %v, want nothing", written)
			}

			// Locked while the reply waits: stored, refused, and cleaned up.
			thread := c.createThread("general", "open", "", "about to be locked")
			ctx := context.Background()
			other, err := pgx.Connect(ctx, c.app.cfg.DatabaseURL)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer other.Close(ctx)
			tx, err := other.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatalf("backend pid: %v", err)
			}
			// The board's row, as a new thread or another reply holds it.
			if _, err := tx.Exec(ctx, `UPDATE boards SET next_post_no = next_post_no + 1 WHERE slug = 'general'`); err != nil {
				t.Fatalf("take the board's row: %v", err)
			}

			// Built here so the goroutine does nothing but send: only the
			// test's own goroutine may fail the test.
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			_ = form.WriteField("body", "racing the lock")
			part, err := form.CreateFormFile(uploadFieldName, "racing.png")
			if err != nil {
				t.Fatalf("create file part: %v", err)
			}
			if _, err := part.Write(pngBytes(t, 320, 240)); err != nil {
				t.Fatalf("write file part: %v", err)
			}
			if err := form.Close(); err != nil {
				t.Fatalf("close form: %v", err)
			}
			type outcome struct {
				status int
				body   createdPost
				err    error
			}
			replied := make(chan outcome, 1)
			go func() {
				res, err := http.Post(fmt.Sprintf("%s/api/boards/general/threads/%d/posts", c.base, thread.ThreadNo),
					form.FormDataContentType(), &body)
				if err != nil {
					replied <- outcome{err: err}
					return
				}
				defer res.Body.Close()
				var got createdPost
				err = json.NewDecoder(res.Body).Decode(&got)
				replied <- outcome{status: res.StatusCode, body: got, err: err}
			}()

			deadline := time.Now().Add(10 * time.Second)
			for {
				var waiting int
				if err := c.app.pool.QueryRow(ctx,
					`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&waiting); err != nil {
					t.Fatalf("look for the blocked reply: %v", err)
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the reply never waited on the board's row")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := tx.Exec(ctx, `UPDATE threads SET locked = TRUE WHERE no = $1
				AND board_id = (SELECT id FROM boards WHERE slug = 'general')`, thread.ThreadNo); err != nil {
				t.Fatalf("lock the thread: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}

			got := <-replied
			if got.err != nil {
				t.Fatalf("reply: %v", got.err)
			}
			if got.status != http.StatusForbidden || got.body.Error != "thread is locked" {
				t.Fatalf("reply locked out mid-flight: status = %d (%s), want 403 thread is locked", got.status, got.body.Error)
			}
			written := blobs.written()
			if len(written) != 2 {
				t.Fatalf("the upload wrote %d objects (%v), want the original and its thumbnail — "+
					"without them the test proves nothing", len(written), written)
			}
			for _, key := range written {
				requireBlob(t, blobs.Store, key, false)
			}
		})
	}
}

// The sweeper removes media through the same interface on every backend; on
// Cloud Run it is the GCS one, and a sweep that dropped the rows but not the
// objects would keep images past their archive period.
func TestSweepRemovesBlobsOnEveryBackend(t *testing.T) {
	for _, backend := range mediaBackends {
		t.Run(backend.name, func(t *testing.T) {
			c := backendServer(t, backend)
			c.addBoard("swept")
			c.exec(`UPDATE boards SET archive_days = 0 WHERE slug = 'swept'`)

			thread := c.imageThread("swept", "archived with an image")
			c.archiveThread("swept", thread.ThreadNo, time.Minute)
			originalKey, thumbKey := blobKeys(t, fetchThread(t, c, "swept", thread.ThreadNo).Posts[0].Attachment)
			requireBlob(t, c.app.media, originalKey, true)
			requireBlob(t, c.app.media, thumbKey, true)

			report, err := c.app.Sweep(context.Background())
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if report.Threads != 1 || report.Attachments != 1 || report.BlobsRemoved != 2 {
				t.Errorf("sweep report = %+v, want one thread with one attachment, both its files removed", report)
			}
			requireBlob(t, c.app.media, originalKey, false)
			requireBlob(t, c.app.media, thumbKey, false)
		})
	}
}

// The GCS backend stamps its objects with a caching policy so that a signed URL
// or a CDN reading straight from the bucket later would answer as the process
// does today. Two constants in two packages drift; this is what stops them.
func TestMediaCacheControlMatchesTheStore(t *testing.T) {
	if mediaCacheControl != media.CacheControl {
		t.Errorf("handler cache control %q and stored cache control %q have drifted apart",
			mediaCacheControl, media.CacheControl)
	}
}

/* recordingStore ----------------------------------------------------------- */

// recordingStore notes the key of every object successfully written through it,
// so a test can assert on what an upload really stored.
type recordingStore struct {
	media.Store
	mu   sync.Mutex
	keys []string
}

func (r *recordingStore) Put(ctx context.Context, key, contentType string, src io.Reader) error {
	if err := r.Store.Put(ctx, key, contentType, src); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, key)
	return nil
}

func (r *recordingStore) written() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}
