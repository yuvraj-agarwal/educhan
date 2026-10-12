package app

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
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

// webHandler serves the embedded assets and falls back to the single-page app
// for client-side routes such as /math/, /math/thread/12, or /math/post/12.
// That fallback is also what serves an institution board's /u/iitb/ routes:
// the UI maps them to the board stored as u-iitb, so the server needs no alias
// for them.
func (a *App) webHandler() http.Handler {
	assets := webFS()
	files := http.FileServerFS(assets)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "." {
			if info, err := fs.Stat(assets, name); err == nil && !info.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w, r, assets)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	page, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ui unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell is tiny and every deploy may change it, so revalidate always.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(page)
}
