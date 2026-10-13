package app

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var embeddedWeb embed.FS

// webFS returns the embedded static assets rooted at the web/ directory.
func webFS() fs.FS {
	sub, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		// The embed path is a compile-time constant, so this cannot fail in a
		// correctly built binary.
		panic(err)
	}
	return sub
}

// webCacheControl is what every embedded asset goes out with. None of their
// URLs carries a version — /app.js is /app.js in every release — so a browser
// may keep a copy but must ask before using it; the ETag makes that question
// a 304 with no body whenever the file has not changed. Marking them
// immutable would strand a returning reader on an old app.js after a deploy.
const webCacheControl = "no-cache"

// webAsset is one embedded file, read once at startup with everything a
// response needs: its ETag, from a hash of its bytes because an embedded file
// has no modification time, and for text a gzipped copy with its own ETag,
// since the two are different bodies.
type webAsset struct {
	name        string
	contentType string
	body        []byte
	etag        string
	gzipped     []byte // nil when the file is not worth compressing
	gzipETag    string
}

// compressibleTypes are the extensions worth gzipping. Fonts and images are
// compressed already.
var compressibleTypes = map[string]bool{
	".html": true, ".js": true, ".css": true, ".svg": true,
	".json": true, ".txt": true, ".md": true,
}

// webAssets reads and compresses the embedded files once per process, however
// many handlers are built — a test server builds one each.
var webAssets = sync.OnceValues(func() (map[string]*webAsset, error) {
	return loadWebAssets(webFS())
})

func loadWebAssets(assets fs.FS) (map[string]*webAsset, error) {
	loaded := map[string]*webAsset{}
	err := fs.WalkDir(assets, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(assets, name)
		if err != nil {
			return err
		}
		ext := path.Ext(name)
		asset := &webAsset{
			name:        name,
			contentType: mime.TypeByExtension(ext),
			body:        body,
			etag:        contentETag(body, ""),
		}
		switch {
		case ext == ".md":
			// Not in Go's own table, and the system's may not have it.
			asset.contentType = "text/markdown; charset=utf-8"
		case asset.contentType == "":
			// A library's LICENSE, which has no extension.
			asset.contentType = http.DetectContentType(body)
		}
		if compressibleTypes[ext] {
			var buf bytes.Buffer
			zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			if err != nil {
				return err
			}
			if _, err := zw.Write(body); err != nil {
				return err
			}
			if err := zw.Close(); err != nil {
				return err
			}
			if buf.Len() < len(body) {
				asset.gzipped = buf.Bytes()
				asset.gzipETag = contentETag(body, "-gzip")
			}
		}
		loaded[name] = asset
		return nil
	})
	return loaded, err
}

// contentETag is a strong validator for body: a prefix of its SHA-256, with a
// suffix for each encoding of the same file.
func contentETag(body []byte, suffix string) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + suffix + `"`
}

// webHandler serves the embedded assets and falls back to the single-page app
// for client-side routes such as /math/, /math/thread/12, or /math/post/12.
// That fallback is also what serves an institution board's /u/iitb/ routes:
// the UI maps them to the board stored as u-iitb, so the server needs no alias
// for them.
func (a *App) webHandler() http.Handler {
	assets, err := webAssets()
	if err != nil {
		// The files are compiled in, so this cannot fail in a correctly
		// built binary.
		panic(err)
	}
	shell := assets["index.html"]

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if asset, ok := assets[name]; ok {
			serveAsset(w, r, asset)
			return
		}
		if shell == nil {
			writeError(w, http.StatusInternalServerError, "ui unavailable")
			return
		}
		serveAsset(w, r, shell)
	})
}

// serveAsset writes asset, gzipped when the client takes it, and leaves
// conditional and range requests to http.ServeContent, which answers a
// matching If-None-Match with 304.
func serveAsset(w http.ResponseWriter, r *http.Request, asset *webAsset) {
	h := w.Header()
	h.Set("Content-Type", asset.contentType)
	h.Set("Cache-Control", webCacheControl)
	body, etag := asset.body, asset.etag
	if asset.gzipped != nil {
		// The body depends on Accept-Encoding, so a shared cache must key on
		// it whichever body this response carries.
		h.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Values("Accept-Encoding")) {
			body, etag = asset.gzipped, asset.gzipETag
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("ETag", etag)
	http.ServeContent(w, r, asset.name, time.Time{}, bytes.NewReader(body))
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip: named, or
// covered by "*", with a quality above zero.
func acceptsGzip(values []string) bool {
	gzipQ, starQ := -1.0, -1.0
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			q := 1.0
			for _, param := range strings.Split(params, ";") {
				key, val, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
					parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
					if err != nil {
						parsed = 0
					}
					q = parsed
				}
			}
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "gzip", "x-gzip":
				gzipQ = q
			case "*":
				starQ = q
			}
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return starQ > 0
}
