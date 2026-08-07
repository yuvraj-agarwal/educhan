// Package handler holds the HTTP handlers for the educhan API.
package handler

import (
	"encoding/json"
	"net/http"
)

// Health reports that the service is up. It is deliberately dependency-free so
// that a struggling database cannot cause an orchestrator to restart instances
// that are otherwise serving fine.
func Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "educhan",
	})
}

// Home serves the placeholder landing page.
func Home(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("educhan — Anonymous imageboard for higher education\n"))
}
