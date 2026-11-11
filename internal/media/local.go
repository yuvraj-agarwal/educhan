package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LocalStore keeps objects on the local filesystem, rooted at a directory.
type LocalStore struct {
	root string
}

var _ Store = (*LocalStore)(nil)

// NewLocalStore creates the root directory if needed and returns a Store backed
// by it.
func NewLocalStore(root string) (*LocalStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("media: storage root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("media: resolve storage root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("media: create storage root: %w", err)
	}
	return &LocalStore{root: abs}, nil
}

// Root returns the absolute directory objects are stored under.
func (s *LocalStore) Root() string { return s.root }

func (s *LocalStore) path(key string) (string, error) {
	cleaned, err := CleanKey(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, filepath.FromSlash(cleaned)), nil
}

// Put writes the object atomically: content lands in a temporary file that is
// renamed into place, so a reader never observes a partially written image.
// A write whose context has ended by the time it would start, or by the time
// it would be published, is not made, as with Cloud Storage.
func (s *LocalStore) Put(ctx context.Context, key, contentType string, r io.Reader) error {
	full, err := s.path(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("media: create directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".upload-*")
	if err != nil {
		return fmt.Errorf("media: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return fmt.Errorf("media: write object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("media: flush object: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("media: set permissions: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, full); err != nil {
		return fmt.Errorf("media: publish object: %w", err)
	}
	return nil
}

// Get opens an object for reading. The returned reader also seeks, so callers
// can hand it straight to http.ServeContent for range requests.
func (s *LocalStore) Get(ctx context.Context, key string) (io.ReadSeekCloser, Object, error) {
	full, err := s.path(key)
	if err != nil {
		return nil, Object{}, err
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, Object{}, ErrNotFound
		}
		return nil, Object{}, fmt.Errorf("media: open object: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, Object{}, fmt.Errorf("media: stat object: %w", err)
	}
	if info.IsDir() {
		f.Close()
		return nil, Object{}, ErrNotFound
	}
	return f, Object{
		ContentType: ContentTypeForExt(filepath.Ext(full)),
		Size:        info.Size(),
		ModTime:     info.ModTime(),
	}, nil
}

// Delete removes an object, ignoring keys that are already gone.
func (s *LocalStore) Delete(ctx context.Context, key string) error {
	full, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("media: delete object: %w", err)
	}
	return nil
}
