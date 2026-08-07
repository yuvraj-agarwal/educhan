package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"io"
	"strings"
	"testing"
	"time"
)

func TestSaveWritesBothRenditions(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
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
	if img.Size != int64(len(original)) {
		t.Errorf("size = %d, want %d", img.Size, len(original))
	}
	sum := sha256.Sum256(original)
	if img.Checksum != hex.EncodeToString(sum[:]) {
		t.Errorf("checksum = %q, want the SHA-256 of the upload", img.Checksum)
	}

	rc, obj, err := store.Get(ctx, OriginalKey(img.Path))
	if err != nil {
		t.Fatalf("get original: %v", err)
	}
	stored, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(stored, original) {
		t.Error("stored original differs from the uploaded bytes")
	}
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
}

func TestSaveRejectsNonImage(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	if _, err := Save(context.Background(), store, "general", []byte("not an image"), time.Now()); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("Save error = %v, want ErrUnsupportedFormat", err)
	}
}

func TestRemoveDeletesBothRenditions(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
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
}

func TestSwapExt(t *testing.T) {
	cases := map[string]string{
		"general/2026/08/abc.webp": "general/2026/08/abc.png",
		"general/2026/08/abc":      "general/2026/08/abc.png",
		"a.b/2026/08/abc.jpg":      "a.b/2026/08/abc.png",
	}
	for in, want := range cases {
		if got := swapExt(in, ".png"); got != want {
			t.Errorf("swapExt(%q) = %q, want %q", in, got, want)
		}
	}
}
