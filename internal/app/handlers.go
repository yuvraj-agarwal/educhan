package app

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxRequestBytes caps a request body well above the largest legal post so a
// malformed or hostile client cannot stream indefinitely.
const maxRequestBytes = 64 << 10

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		a.log.Error("health check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) handleListBoards(w http.ResponseWriter, r *http.Request) {
	boards, err := a.store.Boards(r.Context())
	if err != nil {
		a.fail(w, r, err, "could not list boards")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"boards": boards})
}

func (a *App) handleGetBoard(w http.ResponseWriter, r *http.Request) {
	board, ok := a.board(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, board)
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	board, ok := a.board(w, r)
	if !ok {
		return
	}
	page, err := pageParam(r.URL.Query().Get("page"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	index, err := a.store.Index(r.Context(), board, page)
	if err != nil {
		a.fail(w, r, err, "could not list threads")
		return
	}
	writeJSON(w, http.StatusOK, index)
}

func (a *App) handleCatalog(w http.ResponseWriter, r *http.Request) {
	board, ok := a.board(w, r)
	if !ok {
		return
	}
	catalog, err := a.store.Catalog(r.Context(), board)
	if err != nil {
		a.fail(w, r, err, "could not load catalog")
		return
	}
	writeJSON(w, http.StatusOK, catalog)
}

func (a *App) handleArchive(w http.ResponseWriter, r *http.Request) {
	board, ok := a.board(w, r)
	if !ok {
		return
	}
	archive, err := a.store.Archive(r.Context(), board)
	if err != nil {
		a.fail(w, r, err, "could not load archive")
		return
	}
	writeJSON(w, http.StatusOK, archive)
}

func (a *App) handleGetThread(w http.ResponseWriter, r *http.Request) {
	board, ok := a.board(w, r)
	if !ok {
		return
	}
	no, ok := threadNo(w, r.PathValue("no"))
	if !ok {
		return
	}
	a.writeThread(w, r, board, no)
}

// handleGetThreadByID serves the primary-key thread route, which predates
// per-board thread numbers.
func (a *App) handleGetThreadByID(w http.ResponseWriter, r *http.Request) {
	board, no, ok := a.resolveThreadID(w, r)
	if !ok {
		return
	}
	a.writeThread(w, r, board, no)
}

func (a *App) writeThread(w http.ResponseWriter, r *http.Request, board Board, no int64) {
	detail, err := a.store.Thread(r.Context(), board, no)
	if err != nil {
		a.fail(w, r, err, "could not load thread")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

type createThreadRequest struct {
	Subject string `json:"subject"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	// Badge asks for the poster's verified institution to be shown on the post.
	Badge bool `json:"badge"`
	// Spoiler asks for the attached image to be hidden until it is clicked. A
	// JSON post carries no image, so it is accepted and has nothing to apply to.
	Spoiler bool `json:"spoiler"`
}

func (r *createThreadRequest) submission() submission {
	return submission{Subject: r.Subject, Name: r.Name, Body: r.Body, Badge: r.Badge, Spoiler: r.Spoiler}
}

func (a *App) handleCreateThread(w http.ResponseWriter, r *http.Request) {
	board, ok := a.board(w, r)
	if !ok {
		return
	}

	sub, ok := a.parseSubmission(w, r, &createThreadRequest{})
	if !ok {
		return
	}
	body, ok := postBody(w, sub.Body, sub.Upload != nil)
	if !ok {
		return
	}
	subject, ok := postSubject(w, sub.Subject)
	if !ok {
		return
	}
	// Resolved before flood control, so a post refused for wanting a badge it
	// cannot have does not also cost the poster a cooldown.
	badge, ok := a.postBadge(w, r, sub.Badge)
	if !ok {
		return
	}
	clientKey := clientIP(r, a.cfg.TrustedProxyHops)
	if !a.allow(w, a.postCooldowns(a.threadLimiter, board, clientKey, sub.Upload != nil)) {
		return
	}

	img, err := a.storeUpload(r.Context(), board.Slug, sub.Upload)
	if err != nil {
		a.rejectUpload(w, r, err)
		return
	}

	post, err := a.store.CreateThread(r.Context(), NewThread{
		Board:      board,
		Subject:    subject,
		Identity:   parseIdentity(sub.Name, a.cfg.PosterIDSalt),
		Body:       body,
		PosterID:   a.posterIDFunc(clientKey),
		Badge:      badge,
		Attachment: attachmentInput(sub, img),
	})
	if err != nil {
		a.discardUpload(img)
		a.fail(w, r, err, "could not create thread")
		return
	}
	writeJSON(w, http.StatusCreated, newPostResponse(board, post))
}

type createPostRequest struct {
	Name string `json:"name"`
	Body string `json:"body"`
	// Sage replies without bumping the thread.
	Sage bool `json:"sage"`
	// Badge asks for the poster's verified institution to be shown on the post.
	Badge bool `json:"badge"`
	// Spoiler asks for the attached image to be hidden until it is clicked.
	Spoiler bool `json:"spoiler"`
}

func (r *createPostRequest) submission() submission {
	return submission{Name: r.Name, Body: r.Body, Sage: r.Sage, Badge: r.Badge, Spoiler: r.Spoiler}
}

func (a *App) handleCreatePost(w http.ResponseWriter, r *http.Request) {
	board, ok := a.board(w, r)
	if !ok {
		return
	}
	no, ok := threadNo(w, r.PathValue("no"))
	if !ok {
		return
	}
	a.createPost(w, r, board, no)
}

// handleCreatePostByThreadID serves the primary-key reply route, which predates
// per-board thread numbers.
func (a *App) handleCreatePostByThreadID(w http.ResponseWriter, r *http.Request) {
	board, no, ok := a.resolveThreadID(w, r)
	if !ok {
		return
	}
	a.createPost(w, r, board, no)
}

func (a *App) createPost(w http.ResponseWriter, r *http.Request, board Board, no int64) {
	sub, ok := a.parseSubmission(w, r, &createPostRequest{})
	if !ok {
		return
	}
	body, ok := postBody(w, sub.Body, sub.Upload != nil)
	if !ok {
		return
	}
	badge, ok := a.postBadge(w, r, sub.Badge)
	if !ok {
		return
	}
	clientKey := clientIP(r, a.cfg.TrustedProxyHops)
	if !a.allow(w, a.postCooldowns(a.postLimiter, board, clientKey, sub.Upload != nil)) {
		return
	}

	img, err := a.storeUpload(r.Context(), board.Slug, sub.Upload)
	if err != nil {
		a.rejectUpload(w, r, err)
		return
	}

	post, err := a.store.CreateReply(r.Context(), NewReply{
		Board:      board,
		ThreadNo:   no,
		Identity:   parseIdentity(sub.Name, a.cfg.PosterIDSalt),
		Body:       body,
		Sage:       sub.Sage,
		PosterID:   a.posterIDFunc(clientKey),
		Badge:      badge,
		Attachment: attachmentInput(sub, img),
	})
	if err != nil {
		a.discardUpload(img)
		a.fail(w, r, err, "could not add post")
		return
	}
	writeJSON(w, http.StatusCreated, newPostResponse(board, post))
}

// newPostResponse is the acknowledgement for a created post. It carries the
// board and thread the client should navigate to.
func newPostResponse(board Board, post Post) map[string]any {
	return map[string]any{
		"board":      board.Slug,
		"no":         post.No,
		"thread_no":  post.ThreadNo,
		"created_at": post.CreatedAt,
	}
}

func (a *App) posterIDFunc(clientKey string) func(int64) string {
	return func(threadID int64) string {
		return posterID(a.cfg.PosterIDSalt, clientKey, threadID)
	}
}

// board resolves the {slug} path value, writing the error response itself when
// the board does not exist.
func (a *App) board(w http.ResponseWriter, r *http.Request) (Board, bool) {
	board, err := a.store.Board(r.Context(), r.PathValue("slug"))
	if err != nil {
		a.fail(w, r, err, "could not look up board")
		return Board{}, false
	}
	return board, true
}

func (a *App) resolveThreadID(w http.ResponseWriter, r *http.Request) (Board, int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid thread id")
		return Board{}, 0, false
	}
	board, no, err := a.store.ThreadByID(r.Context(), id)
	if err != nil {
		a.fail(w, r, err, "could not look up thread")
		return Board{}, 0, false
	}
	return board, no, true
}

// postCooldowns is the flood control a submission has to clear: the cooldown
// for the kind of post being made, plus the upload cooldown when it carries an
// image.
//
// Posting is limited per board, so one client can keep a conversation going on
// two boards at once, which is how an imageboard is meant to be read. Uploads
// are limited across the whole server instead, because decoding, thumbnailing,
// and storing an image costs the same wherever the post lands.
func (a *App) postCooldowns(limiter *cooldownLimiter, board Board, clientKey string, hasUpload bool) []cooldown {
	cooldowns := []cooldown{{limiter: limiter, key: board.Slug + "|" + clientKey}}
	if hasUpload {
		cooldowns = append(cooldowns, cooldown{limiter: a.uploadLimiter, key: clientKey})
	}
	return cooldowns
}

// allow applies posting flood control, answering 429 with a Retry-After when
// the caller must wait.
func (a *App) allow(w http.ResponseWriter, cooldowns []cooldown) bool {
	return a.allowAction(w, "posting", cooldowns)
}

// allowAction is allow for anything else that is rate limited; action names
// what was refused, so a 429 from verification does not talk about posting.
func (a *App) allowAction(w http.ResponseWriter, action string, cooldowns []cooldown) bool {
	wait, ok := allowAll(cooldowns...)
	if ok {
		return true
	}
	seconds := int(math.Ceil(wait.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":       action + " too quickly, wait " + strconv.Itoa(seconds) + "s",
		"retry_after": seconds,
	})
	return false
}

// fail maps a store error onto an HTTP response, logging only the unexpected
// ones.
func (a *App) fail(w http.ResponseWriter, r *http.Request, err error, msg string) {
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, errThreadLocked):
		writeError(w, http.StatusForbidden, "thread is locked")
	case errors.Is(err, errThreadArchived):
		writeError(w, http.StatusForbidden, "thread is archived")
	case errors.Is(err, errImageLimit):
		writeError(w, http.StatusForbidden, "image limit reached")
	case errors.Is(err, context.Canceled), errors.Is(r.Context().Err(), context.Canceled):
		// The client went away; there is nobody left to answer.
	default:
		a.log.Error(msg, "error", err, "method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, msg)
	}
}

// postBody applies the shared body rules. A post may omit text only when it
// carries an image.
func postBody(w http.ResponseWriter, raw string, hasAttachment bool) (string, bool) {
	body := normalizeBody(raw)
	switch {
	case body == "" && !hasAttachment:
		writeError(w, http.StatusBadRequest, "body is required")
		return "", false
	case utf8.RuneCountInString(body) > maxBodyLen:
		writeError(w, http.StatusBadRequest, "body must be at most "+strconv.Itoa(maxBodyLen)+" characters")
		return "", false
	case countLines(body) > maxBodyLines:
		writeError(w, http.StatusBadRequest, "body must be at most "+strconv.Itoa(maxBodyLines)+" lines")
		return "", false
	}
	return body, true
}

func postSubject(w http.ResponseWriter, raw string) (string, bool) {
	subject := strings.TrimSpace(collapseControl(strings.ReplaceAll(raw, "\n", " ")))
	if utf8.RuneCountInString(subject) > maxSubjectLen {
		writeError(w, http.StatusBadRequest, "subject must be at most "+strconv.Itoa(maxSubjectLen)+" characters")
		return "", false
	}
	return subject, true
}

func pageParam(raw string) (int, error) {
	if raw == "" {
		return 1, nil
	}
	page, err := strconv.Atoi(raw)
	if err != nil || page < 1 {
		return 0, errors.New("page must be a positive integer")
	}
	return page, nil
}

func threadNo(w http.ResponseWriter, raw string) (int64, bool) {
	no, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || no <= 0 {
		writeError(w, http.StatusBadRequest, "invalid thread number")
		return 0, false
	}
	return no, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mediaType) != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "expected application/json")
			return false
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// The unknown-field error names the offending field, which is worth
		// passing on; other decoder errors can leak input internals.
		if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
			writeError(w, http.StatusBadRequest, "unknown field "+strings.TrimPrefix(msg, "json: unknown field "))
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
