package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// Save's storage behaviour — both renditions written, both removed, and the
// original cleaned up after a partial failure — is backend-independent and
// lives in testStoreContract, which runs it against every backend. What stays
// here is the metadata Save derives, which no backend can affect.

func TestSaveDerivesImageMetadata(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	original := encodePNG(t, 640, 480)
	at := time.Date(2026, time.August, 7, 9, 30, 0, 0, time.UTC)

	img, err := Save(context.Background(), store, "general", original, at)
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
	// The size and the checksum describe the file that was stored, which is
	// the upload rewritten without its metadata rather than the upload.
	stored := mustRead(t, store, OriginalKey(img.Path))
	if img.Size != int64(len(stored)) {
		t.Errorf("size = %d, want %d, the stored file's", img.Size, len(stored))
	}
	sum := sha256.Sum256(stored)
	if img.Checksum != hex.EncodeToString(sum[:]) {
		t.Errorf("checksum = %q, want the SHA-256 of the stored file", img.Checksum)
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
