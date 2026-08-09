package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// CacheControl is the caching policy stored objects carry. Stored names are
// random and their contents never change, so it can be set aggressively. It
// mirrors mediaCacheControl in internal/app/attachments.go, which is what
// actually sets the response header while media streams through the process;
// writing it onto the object too means a signed URL or a CDN reading straight
// from the bucket later would cache identically. TestCacheControlMatchesStore
// in that package pins the two together.
const CacheControl = "public, max-age=31536000, immutable"

// GCSStore keeps objects in a Google Cloud Storage bucket, optionally under a
// key prefix.
//
// The bucket is never made public: Get streams through the process so that the
// same /media/… routes, headers, and range handling apply whichever backend is
// configured. Signed URLs would remove that hop and are a later optimisation,
// not a change of contract.
type GCSStore struct {
	client *storage.Client
	bucket string
	// prefix is prepended to every key, without a leading or trailing slash.
	// Empty means objects live at the root of the bucket.
	prefix string
}

var _ Store = (*GCSStore)(nil)

// NewGCSStore connects to Cloud Storage and returns a Store backed by bucket.
//
// Credentials come from Application Default Credentials: the attached service
// account on Cloud Run, or GOOGLE_APPLICATION_CREDENTIALS elsewhere. No key
// material belongs in the repository or the image. Setting STORAGE_EMULATOR_HOST
// points the client at a local emulator and switches authentication off, which
// is how the tests run without any cloud access.
//
// The caller owns the returned store and must Close it.
func NewGCSStore(ctx context.Context, bucket, prefix string, opts ...option.ClientOption) (*GCSStore, error) {
	bucket = strings.TrimSpace(bucket)
	if bucket == "" {
		return nil, fmt.Errorf("media: storage bucket is required")
	}
	cleanPrefix, err := cleanPrefix(prefix)
	if err != nil {
		return nil, err
	}
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("media: connect to cloud storage: %w", err)
	}
	if err := reachable(ctx, client, bucket, cleanPrefix); err != nil {
		client.Close()
		return nil, err
	}
	return &GCSStore{client: client, bucket: bucket, prefix: cleanPrefix}, nil
}

// reachable confirms the bucket answers before the server starts serving.
//
// Without it a mistyped bucket or a service account that was never granted
// access fails as a 404 on every attachment — the same answer a genuinely
// missing image gives, because the API returns 404 for both and the client
// cannot tell them apart. That is a silent outage. Listing one object needs
// storage.objects.list, which roles/storage.objectAdmin already grants, so this
// costs no broader permission than writing does.
func reachable(ctx context.Context, client *storage.Client, bucket, prefix string) error {
	it := client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	it.PageInfo().MaxSize = 1
	if _, err := it.Next(); err != nil && !errors.Is(err, iterator.Done) {
		return fmt.Errorf("media: cloud storage bucket %q is not usable: %w", bucket, err)
	}
	return nil
}

// Bucket returns the bucket objects are stored in.
func (s *GCSStore) Bucket() string { return s.bucket }

// Prefix returns the key prefix objects are stored under, without slashes.
func (s *GCSStore) Prefix() string { return s.prefix }

// Close releases the underlying client.
func (s *GCSStore) Close() error {
	if s.client == nil {
		return nil
	}
	return s.client.Close()
}

// cleanPrefix normalises an optional object prefix. It is held to the same
// traversal rules as a key so that a misconfigured prefix cannot widen the
// namespace the store addresses.
func cleanPrefix(prefix string) (string, error) {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return "", nil
	}
	if _, err := CleanKey(prefix); err != nil {
		return "", fmt.Errorf("media: invalid object prefix %q: %w", prefix, err)
	}
	return prefix, nil
}

// object resolves a storage key to an object handle, applying the same key
// validation as the filesystem backend. Cloud Storage would happily accept a
// name containing ".." — the namespace is flat, so nothing escapes — but keys
// stay portable between backends only if both enforce the same rules.
func (s *GCSStore) object(key string) (*storage.ObjectHandle, error) {
	cleaned, err := CleanKey(key)
	if err != nil {
		return nil, err
	}
	name := cleaned
	if s.prefix != "" {
		name = s.prefix + "/" + cleaned
	}
	return s.client.Bucket(s.bucket).Object(name), nil
}

// Put writes the object. Cloud Storage publishes an upload only when the write
// is finalised, so — as with the filesystem backend's rename — a reader never
// observes a partially written image. contentType is persisted as the object's
// own content type; an empty hint is derived from the key's extension so that
// both backends answer Get identically.
func (s *GCSStore) Put(ctx context.Context, key, contentType string, r io.Reader) error {
	obj, err := s.object(key)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = ContentTypeForExt(path.Ext(key))
	}

	// A cancelled context is how an in-flight Writer is abandoned; without it a
	// failed copy would still be finalised by Close.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w := obj.NewWriter(ctx)
	w.ContentType = contentType
	w.CacheControl = CacheControl
	if _, err := io.Copy(w, r); err != nil {
		cancel()
		return fmt.Errorf("media: write object: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("media: publish object: %w", err)
	}
	return nil
}

// Get opens an object for reading. The returned reader seeks, so callers can
// hand it straight to http.ServeContent for range requests; bytes are fetched
// from Cloud Storage lazily, so a seek costs nothing and a range request
// downloads only the range that is served.
func (s *GCSStore) Get(ctx context.Context, key string) (io.ReadSeekCloser, Object, error) {
	obj, err := s.object(key)
	if err != nil {
		return nil, Object{}, err
	}
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, Object{}, ErrNotFound
		}
		return nil, Object{}, fmt.Errorf("media: stat object: %w", err)
	}

	contentType := attrs.ContentType
	if contentType == "" {
		contentType = ContentTypeForExt(path.Ext(key))
	}
	return &gcsReader{
			ctx: ctx,
			// Pinning the generation keeps every range read on the rendition
			// that was stat'ed, rather than racing a concurrent overwrite.
			obj:  obj.Generation(attrs.Generation),
			size: attrs.Size,
		}, Object{
			ContentType: contentType,
			Size:        attrs.Size,
			ModTime:     attrs.Updated,
		}, nil
}

// Delete removes an object, ignoring keys that are already gone.
func (s *GCSStore) Delete(ctx context.Context, key string) error {
	obj, err := s.object(key)
	if err != nil {
		return err
	}
	if err := obj.Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("media: delete object: %w", err)
	}
	return nil
}

// gcsReader adapts a Cloud Storage object to io.ReadSeekCloser.
//
// Seeking only moves an offset; the range read is opened on the first Read
// after it and torn down when the offset moves again. http.ServeContent seeks
// to the end for the size, back to the start, and then to the start of the
// range it serves before reading a byte — so a range request costs exactly one
// download, of exactly that range, and the size probe costs none.
type gcsReader struct {
	ctx  context.Context
	obj  *storage.ObjectHandle
	size int64
	pos  int64
	r    *storage.Reader
}

var _ io.ReadSeekCloser = (*gcsReader)(nil)

func (g *gcsReader) Read(p []byte) (int, error) {
	if g.pos >= g.size {
		return 0, io.EOF
	}
	if g.r == nil {
		r, err := g.obj.NewRangeReader(g.ctx, g.pos, -1)
		if err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				return 0, ErrNotFound
			}
			return 0, fmt.Errorf("media: read object: %w", err)
		}
		g.r = r
	}
	n, err := g.r.Read(p)
	g.pos += int64(n)
	if err == io.EOF {
		g.closeReader()
		// The object may have more to give only if the range reader stopped
		// short, which it does not; report EOF once the offset is at the end.
		if g.pos < g.size {
			return n, io.ErrUnexpectedEOF
		}
		return n, io.EOF
	}
	if err != nil {
		g.closeReader()
		return n, fmt.Errorf("media: read object: %w", err)
	}
	return n, nil
}

func (g *gcsReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = g.pos + offset
	case io.SeekEnd:
		abs = g.size + offset
	default:
		return 0, fmt.Errorf("media: seek: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("media: seek: negative position %d", abs)
	}
	if abs != g.pos {
		// The open range read starts at the old offset and is of no use now.
		g.closeReader()
		g.pos = abs
	}
	return abs, nil
}

func (g *gcsReader) Close() error {
	g.closeReader()
	return nil
}

func (g *gcsReader) closeReader() {
	if g.r != nil {
		g.r.Close()
		g.r = nil
	}
}
