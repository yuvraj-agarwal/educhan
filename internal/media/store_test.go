package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanKey(t *testing.T) {
	valid := []string{
		"originals/general/2026/08/abc.jpg",
		"thumbs/general/2026/08/abc.png",
	}
	for _, key := range valid {
		got, err := CleanKey(key)
		if err != nil || got != key {
			t.Errorf("CleanKey(%q) = %q, %v; want unchanged key", key, got, err)
		}
	}

	invalid := []string{
		"",
		"/etc/passwd",
		"../../etc/passwd",
		"originals/../../etc/passwd",
		"originals/./general/a.jpg",
		`originals\general\a.jpg`,
		"originals//general/a.jpg",
		"originals/general/a.jpg\x00",
	}
	for _, key := range invalid {
		if _, err := CleanKey(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("CleanKey(%q) error = %v, want ErrInvalidKey", key, err)
		}
	}
}

func TestNewPath(t *testing.T) {
	at := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	got, err := NewPath("general", at, ".jpg")
	if err != nil {
		t.Fatalf("NewPath: %v", err)
	}
	if !strings.HasPrefix(got, "general/2026/08/") || !strings.HasSuffix(got, ".jpg") {
		t.Fatalf("NewPath = %q, want general/2026/08/<random>.jpg", got)
	}

	other, err := NewPath("general", at, ".jpg")
	if err != nil {
		t.Fatalf("NewPath: %v", err)
	}
	if other == got {
		t.Fatal("NewPath returned the same name twice")
	}

	if _, err := NewPath("../evil", at, ".jpg"); err == nil {
		t.Fatal("NewPath accepted a traversal board name")
	}
	if _, err := NewPath("", at, ".jpg"); err == nil {
		t.Fatal("NewPath accepted an empty board")
	}
}

func TestLocalStoreRoundTrip(t *testing.T) {
	store, err := NewLocalStore(filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	ctx := context.Background()
	key := "originals/general/2026/08/abc.png"
	payload := []byte("pretend this is a png")

	if err := store.Put(ctx, key, "image/png", bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "originals", "general", "2026", "08", "abc.png")); err != nil {
		t.Fatalf("object not on disk: %v", err)
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
		t.Fatalf("Get returned %q, want %q", got, payload)
	}
	if obj.ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png", obj.ContentType)
	}
	if obj.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", obj.Size, len(payload))
	}

	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete of missing key should be a no-op, got %v", err)
	}
}

func TestLocalStoreRejectsEscapingKeys(t *testing.T) {
	root := filepath.Join(t.TempDir(), "uploads")
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	ctx := context.Background()

	if err := store.Put(ctx, "../escaped.png", "image/png", strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Put error = %v, want ErrInvalidKey", err)
	}
	if _, _, err := store.Get(ctx, "originals/../../secret"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Get error = %v, want ErrInvalidKey", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.png")); !os.IsNotExist(err) {
		t.Fatal("a rejected key still wrote outside the storage root")
	}
}

func TestLocalStoreGetDirectoryIsNotFound(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, "originals/general/a.png", "image/png", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, _, err := store.Get(ctx, "originals/general"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(directory) error = %v, want ErrNotFound", err)
	}
}
