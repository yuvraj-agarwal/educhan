package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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

			t.Run("serves the original", func(t *testing.T) {
				res, payload := c.fetch(att.URL, nil)
				if res.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", res.StatusCode)
				}
				if !bytes.Equal(payload, original) {
					t.Error("served original differs from the uploaded bytes")
				}
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
					{fmt.Sprintf("bytes=%d-", len(original)-16), len(original) - 16, len(original)},
				} {
					res, payload := c.fetch(att.URL, http.Header{"Range": []string{tc.header}})
					if res.StatusCode != http.StatusPartialContent {
						t.Fatalf("%s: status = %d, want 206", tc.header, res.StatusCode)
					}
					if !bytes.Equal(payload, original[tc.start:tc.end]) {
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

// Blobs are written before the row that references them exists, so a
// transaction that refuses the post leaves them orphaned unless the handler
// cleans up. A locked thread is the refusal that is easiest to arrange and is
// exactly the shape of the ones that matter.
func TestFailedPostRemovesItsBlobs(t *testing.T) {
	for _, backend := range mediaBackends {
		t.Run(backend.name, func(t *testing.T) {
			c := backendServer(t, backend)

			// Watch every write, so the assertion covers whatever the upload
			// actually stored rather than the keys the test guessed at.
			blobs := &recordingStore{Store: c.app.media}
			c.app.media = blobs

			thread := c.createThread("general", "closed", "", "no more replies")
			c.modAction(http.MethodPost, fmt.Sprintf("/api/boards/general/threads/%d/lock", thread.ThreadNo))

			var out createdPost
			status := c.upload(fmt.Sprintf("/api/boards/general/threads/%d/posts", thread.ThreadNo),
				map[string]string{"body": "sneaking in"}, "late.png", pngBytes(t, 320, 240), &out)
			if status != http.StatusForbidden {
				t.Fatalf("reply to a locked thread: status = %d, want 403 (%s)", status, out.Error)
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
			if report.Threads != 1 || report.Attachments != 1 {
				t.Errorf("sweep report = %+v, want one thread with one attachment", report)
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
