package app

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

const maxBodyLen = 8192

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := a.pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) handleListBoards(w http.ResponseWriter, r *http.Request) {
	boards, err := a.listBoards(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list boards")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"boards": boards})
}

func (a *App) handleListThreads(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	boardID, ok, err := a.boardID(r.Context(), slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "board not found")
		return
	}
	threads, err := a.listThreads(r.Context(), boardID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list threads")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"board": slug, "threads": threads})
}

type createThreadRequest struct {
	Subject string `json:"subject"`
	Author  string `json:"author"`
	Body    string `json:"body"`
}

func (a *App) handleCreateThread(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	boardID, ok, err := a.boardID(r.Context(), slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "board not found")
		return
	}

	var req createThreadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}
	if len(body) > maxBodyLen {
		writeError(w, http.StatusBadRequest, "body too long")
		return
	}

	id, err := a.createThread(r.Context(), boardID, strings.TrimSpace(req.Subject), authorOrAnon(req.Author), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create thread")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

type createPostRequest struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

func (a *App) handleGetThread(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid thread id")
		return
	}
	detail, err := a.getThread(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load thread")
		return
	}
	if detail == nil {
		writeError(w, http.StatusNotFound, "thread not found")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (a *App) handleCreatePost(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid thread id")
		return
	}
	var req createPostRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}
	if len(body) > maxBodyLen {
		writeError(w, http.StatusBadRequest, "body too long")
		return
	}

	ok, err := a.addPost(r.Context(), id, authorOrAnon(req.Author), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not add post")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "thread not found")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

func authorOrAnon(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Anonymous"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyLen+1024))
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
