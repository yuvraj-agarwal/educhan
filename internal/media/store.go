// Package media handles image uploads: validating them, deriving thumbnails,
// and persisting both variants through a small blob-storage abstraction.
//
// Business logic only ever talks to Store, so the local filesystem backend used
// today can be replaced by an object store (Google Cloud Storage is the planned
// target) without touching handlers or SQL.
package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// Variant prefixes. A stored image is addressed by a variant-relative path such
// as "general/2026/08/6f1c….jpg"; the prefix selects the original or the
// thumbnail rendition of that same path.
const (
	OriginalPrefix = "originals"
	ThumbPrefix    = "thumbs"
)

// ErrNotFound is returned by Store.Get for keys that do not exist.
var ErrNotFound = errors.New("media: object not found")

// ErrInvalidKey is returned for keys that escape the storage namespace.
var ErrInvalidKey = errors.New("media: invalid object key")

// Object carries the metadata a Store knows about a stored blob.
type Object struct {
	ContentType string
	Size        int64
	ModTime     time.Time
}

// Store is the blob storage contract. Keys are slash-separated, relative, and
// never contain "." or ".." segments.
type Store interface {
	// Put writes r under key, replacing any existing object. contentType is a
	// hint backends may persist; backends that cannot store it may ignore it.
	Put(ctx context.Context, key, contentType string, r io.Reader) error
	// Get opens key for reading. It returns ErrNotFound if the key is absent.
	Get(ctx context.Context, key string) (io.ReadSeekCloser, Object, error)
	// Delete removes key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// OriginalKey returns the storage key of the full-size rendition of p.
func OriginalKey(p string) string { return OriginalPrefix + "/" + p }

// ThumbKey returns the storage key of the thumbnail rendition of p.
func ThumbKey(p string) string { return ThumbPrefix + "/" + p }

// CleanKey validates a storage key and returns it in canonical form. Keys must
// be relative and free of traversal segments so that a filesystem backend can
// map them onto paths safely.
func CleanKey(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || strings.ContainsRune(key, 0) {
		return "", ErrInvalidKey
	}
	cleaned := path.Clean(key)
	if cleaned != key || cleaned == "." || strings.HasPrefix(cleaned, "..") {
		return "", ErrInvalidKey
	}
	return cleaned, nil
}

// NewPath builds the variant-relative path for a freshly uploaded image:
// "{board}/{YYYY}/{MM}/{random}{ext}". The random component means stored names
// never derive from user input and never collide.
func NewPath(board string, at time.Time, ext string) (string, error) {
	board = strings.Trim(board, "/")
	if board == "" {
		return "", fmt.Errorf("media: board is required")
	}
	if _, err := CleanKey(board); err != nil {
		return "", err
	}
	name, err := RandomName(ext)
	if err != nil {
		return "", err
	}
	at = at.UTC()
	return fmt.Sprintf("%s/%04d/%02d/%s", board, at.Year(), int(at.Month()), name), nil
}

// RandomName returns an unguessable base filename with the given extension.
func RandomName(ext string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("media: generate name: %w", err)
	}
	return hex.EncodeToString(buf) + ext, nil
}
