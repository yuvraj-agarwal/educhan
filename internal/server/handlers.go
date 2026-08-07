package server

import (
	"encoding/json"
	"net/http"
)

// Version is stamped at build time with
// -ldflags "-X github.com/yuvraj-agarwal/educhan/internal/server.Version=<ref>".
var Version = "dev"

const indexPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>educhan</title>
</head>
<body>
<h1>educhan</h1>
<p>Anonymous imageboard for tertiary education.</p>
</body>
</html>
`

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(indexPage))
}

// handleLive reports that the process is up. It must stay dependency-free so a
// struggling database cannot cause the orchestrator to restart healthy pods.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": Version,
	})
}

// handleReady reports that the process can serve traffic. Checks for backing
// services belong here as they are introduced.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ready",
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError)
		return
	}
	payload = append(payload, '\n')

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Health and API responses are per-request state, never cacheable.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// writeError writes a JSON error body without risking a recursive failure, so
// it is safe to call from the panic recovery path.
func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + http.StatusText(status) + `"}` + "\n"))
}
