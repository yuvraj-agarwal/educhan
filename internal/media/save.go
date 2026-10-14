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
	// Checksum is the hex-encoded SHA-256 of the stored original, the rewritten
	// file rather than the upload, so it is the same however many times the
	// same picture comes in with different metadata on it. Size is that file's
	// length.
	Checksum string
}

// Save validates data, rewrites it without its metadata, derives a thumbnail,
// and writes both renditions. The upload's own bytes are never written. On a
// partial failure it removes whatever it already wrote so the store is not left
// holding orphans.
//
// It waits for room to process the upload as Process does, and holds it until
// both renditions are stored, because until then it holds them (processingCost
// counts them). ctx bounds the writes as well as the wait: once it has ended
// nothing more is written, a write it cuts off fails the save, and what was
// already written is removed all the same.
func Save(ctx context.Context, s Store, board string, data []byte, now time.Time) (StoredImage, error) {
	processed, release, err := processHeld(ctx, data)
	if err != nil {
		return StoredImage{}, err
	}
	defer release()
	// Processing takes what it takes; a deadline it ran past stops the save
	// here, on every backend, rather than in whichever write notices.
	if err := ctx.Err(); err != nil {
		return StoredImage{}, err
	}

	origPath, err := NewPath(board, now, processed.Format.Ext)
	if err != nil {
		return StoredImage{}, err
	}
	thumbPath := swapExt(origPath, processed.Thumb.Format.Ext)

	if err := s.Put(ctx, OriginalKey(origPath), processed.Format.ContentType, bytes.NewReader(processed.Data)); err != nil {
		return StoredImage{}, err
	}
	if err := s.Put(ctx, ThumbKey(thumbPath), processed.Thumb.Format.ContentType, bytes.NewReader(processed.Thumb.Data)); err != nil {
		// The context may be what failed the write; the original is removed
		// whether or not it is still running, or nothing would remove it.
		_ = s.Delete(context.WithoutCancel(ctx), OriginalKey(origPath))
		return StoredImage{}, err
	}

	sum := sha256.Sum256(processed.Data)
	return StoredImage{
		Path:             origPath,
		ThumbPath:        thumbPath,
		ContentType:      processed.Format.ContentType,
		ThumbContentType: processed.Thumb.Format.ContentType,
		Size:             int64(len(processed.Data)),
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
