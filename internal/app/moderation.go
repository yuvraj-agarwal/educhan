package app

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"

	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// Moderation is authenticated with a single shared secret rather than accounts:
// enough to run a board from a script or an admin curl, and small enough that
// replacing it with real janitor accounts later touches only this file.
//
// The key is presented as either "Authorization: Bearer <key>" or
// "X-Mod-Key: <key>". With no key configured the endpoints stay closed.
const modKeyHeader = "X-Mod-Key"

// authorizeMod reports whether the request carries the moderation key, writing
// the error response itself when it does not.
func (a *App) authorizeMod(w http.ResponseWriter, r *http.Request) bool {
	if a.cfg.ModKey == "" {
		writeError(w, http.StatusForbidden, "moderation is not configured")
		return false
	}
	if subtle.ConstantTimeCompare([]byte(presentedModKey(r)), []byte(a.cfg.ModKey)) != 1 {
		writeError(w, http.StatusForbidden, "invalid moderation key")
		return false
	}
	return true
}

func presentedModKey(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get(modKeyHeader)); key != "" {
		return key
	}
	scheme, key, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if found && strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
		return strings.TrimSpace(key)
	}
	return ""
}

// modTarget resolves the board and thread number a moderation request names.
func (a *App) modTarget(w http.ResponseWriter, r *http.Request) (Board, int64, bool) {
	if !a.authorizeMod(w, r) {
		return Board{}, 0, false
	}
	board, ok := a.board(w, r)
	if !ok {
		return Board{}, 0, false
	}
	no, ok := threadNo(w, r.PathValue("no"))
	if !ok {
		return Board{}, 0, false
	}
	return board, no, true
}

func (a *App) handleSticky(w http.ResponseWriter, r *http.Request)   { a.setSticky(w, r, true) }
func (a *App) handleUnsticky(w http.ResponseWriter, r *http.Request) { a.setSticky(w, r, false) }
func (a *App) handleLock(w http.ResponseWriter, r *http.Request)     { a.setLocked(w, r, true) }
func (a *App) handleUnlock(w http.ResponseWriter, r *http.Request)   { a.setLocked(w, r, false) }

func (a *App) setSticky(w http.ResponseWriter, r *http.Request, sticky bool) {
	board, no, ok := a.modTarget(w, r)
	if !ok {
		return
	}
	if err := a.store.SetThreadFlag(r.Context(), board, no, flagSticky, sticky); err != nil {
		a.fail(w, r, err, "could not update thread")
		return
	}
	a.log.Info("moderation", "action", "sticky", "board", board.Slug, "thread", no, "value", sticky)
	writeJSON(w, http.StatusOK, map[string]any{
		"board": board.Slug, "thread_no": no, "sticky": sticky,
	})
}

func (a *App) setLocked(w http.ResponseWriter, r *http.Request, locked bool) {
	board, no, ok := a.modTarget(w, r)
	if !ok {
		return
	}
	if err := a.store.SetThreadFlag(r.Context(), board, no, flagLocked, locked); err != nil {
		a.fail(w, r, err, "could not update thread")
		return
	}
	a.log.Info("moderation", "action", "lock", "board", board.Slug, "thread", no, "value", locked)
	writeJSON(w, http.StatusOK, map[string]any{
		"board": board.Slug, "thread_no": no, "locked": locked,
	})
}

func (a *App) handleDeleteThread(w http.ResponseWriter, r *http.Request) {
	board, no, ok := a.modTarget(w, r)
	if !ok {
		return
	}
	deleted, err := a.store.DeleteThread(r.Context(), board, no)
	if err != nil {
		a.fail(w, r, err, "could not delete thread")
		return
	}
	a.removeBlobs(r, deleted.Images)
	a.log.Info("moderation", "action", "delete thread", "board", board.Slug, "thread", no,
		"posts", deleted.Posts, "attachments", len(deleted.Images))
	writeJSON(w, http.StatusOK, map[string]any{
		"board":                board.Slug,
		"thread_no":            no,
		"deleted_posts":        deleted.Posts,
		"deleted_attachments":  len(deleted.Images),
		"deleted_thread_count": 1,
	})
}

func (a *App) handleDeletePost(w http.ResponseWriter, r *http.Request) {
	board, no, ok := a.modTarget(w, r)
	if !ok {
		return
	}
	postNo, err := strconv.ParseInt(r.PathValue("post"), 10, 64)
	if err != nil || postNo <= 0 {
		writeError(w, http.StatusBadRequest, "invalid post number")
		return
	}

	deleted, err := a.store.DeletePost(r.Context(), board, no, postNo)
	if err != nil {
		a.fail(w, r, err, "could not delete post")
		return
	}
	a.removeBlobs(r, deleted.Images)
	a.log.Info("moderation", "action", "delete post", "board", board.Slug, "thread", no, "post", postNo,
		"thread_deleted", deleted.ThreadDeleted, "attachments", len(deleted.Images))
	writeJSON(w, http.StatusOK, map[string]any{
		"board":               board.Slug,
		"thread_no":           no,
		"post_no":             postNo,
		"deleted_attachments": len(deleted.Images),
		// Deleting an opening post takes its thread with it, which the client
		// needs to know before it navigates back to the thread.
		"thread_deleted": deleted.ThreadDeleted,
	})
}

// removeBlobs deletes the stored renditions of posts that are already gone from
// the database. A failure here leaves an orphaned file rather than a broken
// post, so it is logged instead of failing the request.
func (a *App) removeBlobs(r *http.Request, images []media.StoredImage) {
	for _, img := range images {
		if err := media.Remove(r.Context(), a.media, img); err != nil {
			a.log.Error("could not remove attachment blobs", "error", err, "path", img.Path)
		}
	}
}
