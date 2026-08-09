package media

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/yuvraj-agarwal/educhan/internal/media/mediatest"
)

// gcsStore returns a GCSStore against a freshly started emulator.
func gcsStore(t *testing.T, prefix string) *GCSStore {
	t.Helper()
	store, err := NewGCSStore(context.Background(), mediatest.StartGCS(t), prefix)
	if err != nil {
		t.Fatalf("NewGCSStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

// TestGCSStoreContract holds the Cloud Storage backend to the same contract as
// the filesystem one. It is the test that makes swapping backends a
// configuration change rather than a leap of faith.
func TestGCSStoreContract(t *testing.T) {
	testStoreContract(t, func(t *testing.T) Store { return gcsStore(t, "") })
}

// TestGCSStoreContractWithPrefix runs the whole contract again through a
// prefix, because the prefix is applied on every key and a mistake there would
// show up as objects landing in the wrong place rather than as a failure.
func TestGCSStoreContractWithPrefix(t *testing.T) {
	testStoreContract(t, func(t *testing.T) Store { return gcsStore(t, "media/v1") })
}

func TestGCSStorePrefixNamesObjects(t *testing.T) {
	ctx := context.Background()
	store := gcsStore(t, "/media/v1/")

	if store.Prefix() != "media/v1" {
		t.Errorf("prefix = %q, want the trimmed %q", store.Prefix(), "media/v1")
	}
	if store.Bucket() != mediatest.Bucket {
		t.Errorf("bucket = %q, want %q", store.Bucket(), mediatest.Bucket)
	}

	key := "originals/general/2026/08/prefixed.png"
	if err := store.Put(ctx, key, "image/png", strings.NewReader("bytes")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// The object name is what a bucket listing, a lifecycle rule, or a signed
	// URL would use, so assert on it directly rather than through the store.
	client, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatalf("storage client: %v", err)
	}
	defer client.Close()

	want := "media/v1/" + key
	if _, err := client.Bucket(mediatest.Bucket).Object(want).Attrs(ctx); err != nil {
		t.Fatalf("object %q: %v", want, err)
	}
	// And nothing was written at the unprefixed name.
	if _, err := client.Bucket(mediatest.Bucket).Object(key).Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("unprefixed object %q error = %v, want ErrObjectNotExist", key, err)
	}
}

func TestGCSStoreWritesObjectMetadata(t *testing.T) {
	ctx := context.Background()
	store := gcsStore(t, "")
	key := "originals/general/2026/08/meta.jpg"

	if err := store.Put(ctx, key, FormatJPEG.ContentType, strings.NewReader("bytes")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	client, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatalf("storage client: %v", err)
	}
	defer client.Close()

	attrs, err := client.Bucket(mediatest.Bucket).Object(key).Attrs(ctx)
	if err != nil {
		t.Fatalf("attrs: %v", err)
	}
	if attrs.ContentType != FormatJPEG.ContentType {
		t.Errorf("stored content type = %q, want %q", attrs.ContentType, FormatJPEG.ContentType)
	}

	// Cache-Control is read off the download response rather than off Attrs:
	// the emulator's JSON metadata omits the field, and the response is the
	// surface that matters anyway — it is what a signed URL or a CDN reading
	// straight from the bucket would see, and the reason the object carries the
	// policy at all when the handler already sets the header itself.
	reader, err := client.Bucket(mediatest.Bucket).Object(key).NewReader(ctx)
	if err != nil {
		t.Fatalf("open object: %v", err)
	}
	defer reader.Close()
	if reader.Attrs.CacheControl != CacheControl {
		t.Errorf("served cache control = %q, want %q", reader.Attrs.CacheControl, CacheControl)
	}
}

// A prefix is configuration, and configuration that could widen the namespace
// the store addresses is refused where it is read rather than where it bites.
func TestNewGCSStoreValidatesConfiguration(t *testing.T) {
	mediatest.StartGCS(t)
	ctx := context.Background()

	if _, err := NewGCSStore(ctx, "  ", ""); err == nil {
		t.Error("NewGCSStore accepted an empty bucket")
	}
	for _, prefix := range []string{"../escape", "media/../../etc", `media\v1`, "media/./v1"} {
		store, err := NewGCSStore(ctx, mediatest.Bucket, prefix)
		if err == nil {
			store.Close()
			t.Errorf("NewGCSStore accepted the prefix %q", prefix)
		} else if !errors.Is(err, ErrInvalidKey) {
			t.Errorf("NewGCSStore(%q) error = %v, want ErrInvalidKey", prefix, err)
		}
	}
}

// A GCS read is a range request per Read-after-Seek, and a partial download
// that stopped early would corrupt a large image without erroring. Round-trip
// something bigger than the client's buffers.
func TestGCSStoreRoundTripsALargeObject(t *testing.T) {
	ctx := context.Background()
	store := gcsStore(t, "")
	key := "originals/general/2026/08/large.png"

	payload := make([]byte, 3<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := store.Put(ctx, key, "image/png", bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := mustRead(t, store, key); !bytes.Equal(got, payload) {
		t.Errorf("read back %d bytes, want %d identical ones", len(got), len(payload))
	}
}

// An unusable bucket has to stop the server starting. The API answers 404 for
// "no such bucket" and for "no such object" alike, and the client cannot tell
// them apart, so a bucket that is missing or ungranted would otherwise surface
// as every attachment 404ing — an outage that looks like an empty board.
func TestNewGCSStoreRejectsAnUnusableBucket(t *testing.T) {
	mediatest.StartGCS(t)

	store, err := NewGCSStore(context.Background(), "educhan-no-such-bucket", "")
	if err == nil {
		store.Close()
		t.Fatal("NewGCSStore accepted a bucket that does not exist")
	}
	if !strings.Contains(err.Error(), "educhan-no-such-bucket") {
		t.Errorf("error = %v, want it to name the bucket", err)
	}
}
