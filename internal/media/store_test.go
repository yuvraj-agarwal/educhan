package media

import (
	"context"
	"errors"
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

// TestLocalStoreContract runs the shared Store contract against the filesystem
// backend. TestGCSStoreContract runs the same suite against Cloud Storage.
func TestLocalStoreContract(t *testing.T) {
	testStoreContract(t, func(t *testing.T) Store {
		store, err := NewLocalStore(filepath.Join(t.TempDir(), "uploads"))
		if err != nil {
			t.Fatalf("NewLocalStore: %v", err)
		}
		return store
	})
}

// The properties below are the filesystem backend's own: where the bytes land,
// and that a rejected key cannot reach the disk outside the storage root.

func TestLocalStoreWritesUnderTheRoot(t *testing.T) {
	store, err := NewLocalStore(filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	key := "originals/general/2026/08/abc.png"
	if err := store.Put(context.Background(), key, "image/png", strings.NewReader("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "originals", "general", "2026", "08", "abc.png")); err != nil {
		t.Fatalf("object not on disk: %v", err)
	}
}

func TestLocalStoreEscapingKeyWritesNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "uploads")
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	if err := store.Put(context.Background(), "../escaped.png", "image/png", strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Put error = %v, want ErrInvalidKey", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.png")); !os.IsNotExist(err) {
		t.Fatal("a rejected key still wrote outside the storage root")
	}
}
