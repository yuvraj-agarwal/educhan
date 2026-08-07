package app

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web/index.html
var embeddedWeb embed.FS

// webFS returns the embedded static web assets rooted at the web/ directory.
func webFS() fs.FS {
	sub, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		// The embed path is a compile-time constant, so this cannot fail in a
		// correctly built binary.
		panic(err)
	}
	return sub
}

// withCommonHeaders adds headers useful for a small JSON+HTML service.
func withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
