package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// StoredImage is the metadata describing an upload that has been written to a
// Store. It is what callers persist alongside a post.
type StoredImage struct {
	Path             string // variant-relative path of the original
	ThumbPath        string // variant-relative path of the thumbnail
	ContentType      string
	ThumbContentType string
	Size             int64
	Width            int
	Height           int
	ThumbWidth       int
	ThumbHeight      int
	Checksum         string // hex-encoded SHA-256 of the original bytes
}

// Save validates data, derives a thumbnail, and writes both renditions. On a
// partial failure it removes whatever it already wrote so the store is not left
// holding orphans.
func Save(ctx context.Context, s Store, board string, data []byte, now time.Time) (StoredImage, error) {
	processed, err := Process(data)
	if err != nil {
		return StoredImage{}, err
	}

	origPath, err := NewPath(board, now, processed.Format.Ext)
	if err != nil {
		return StoredImage{}, err
	}
	thumbPath := swapExt(origPath, processed.Thumb.Format.Ext)

	if err := s.Put(ctx, OriginalKey(origPath), processed.Format.ContentType, bytes.NewReader(data)); err != nil {
		return StoredImage{}, err
	}
	if err := s.Put(ctx, ThumbKey(thumbPath), processed.Thumb.Format.ContentType, bytes.NewReader(processed.Thumb.Data)); err != nil {
		_ = s.Delete(ctx, OriginalKey(origPath))
		return StoredImage{}, err
	}

	sum := sha256.Sum256(data)
	return StoredImage{
		Path:             origPath,
		ThumbPath:        thumbPath,
		ContentType:      processed.Format.ContentType,
		ThumbContentType: processed.Thumb.Format.ContentType,
		Size:             int64(len(data)),
		Width:            processed.Width,
		Height:           processed.Height,
		ThumbWidth:       processed.Thumb.Width,
		ThumbHeight:      processed.Thumb.Height,
		Checksum:         hex.EncodeToString(sum[:]),
	}, nil
}

// Remove deletes both renditions of a stored image.
func Remove(ctx context.Context, s Store, img StoredImage) error {
	var firstErr error
	for _, key := range []string{OriginalKey(img.Path), ThumbKey(img.ThumbPath)} {
		if err := s.Delete(ctx, key); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("media: remove %s: %w", key, err)
		}
	}
	return firstErr
}

func swapExt(p, ext string) string {
	if i := strings.LastIndex(p, "."); i > strings.LastIndex(p, "/") {
		return p[:i] + ext
	}
	return p + ext
}
