package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// multipartOverhead is the slack allowed on top of the attachment size limit
// for MIME headers and the text fields sharing the request body.
const multipartOverhead = 1 << 20

// uploadFieldName is the multipart field carrying the attachment.
const uploadFieldName = "image"

// uploadsSwitchedOff is the refusal of an image post on a server with uploads
// switched off. Clients match on it, so it does not change.
const uploadsSwitchedOff = "image uploads are switched off on this server"

// maxOriginalNameLen bounds the client-supplied filename kept for display.
const maxOriginalNameLen = 128

// mediaCacheControl is safe to set aggressively: stored names are random and
// their contents never change.
const mediaCacheControl = "public, max-age=31536000, immutable"

// URL prefixes the two renditions are served under.
const (
	originalURLPrefix = "/media/original/"
	thumbURLPrefix    = "/media/thumb/"
)

func originalURL(path string) string { return originalURLPrefix + path }
func thumbURL(path string) string    { return thumbURLPrefix + path }

// upload is an image received with a post, before it has been validated.
type upload struct {
	Filename string
	Data     []byte
}

// submission is the shared payload of "create thread" and "reply", parsed from
// either JSON or multipart/form-data.
type submission struct {
	Subject string
	Name    string
	Body    string
	Sage    bool
	// Badge asks for the poster's verified institution to be shown on the post.
	Badge bool
	// Spoiler asks for the image to be covered until it is clicked. It is a
	// display preference: the thumbnail is made either way, and a submission
	// without an image ignores it.
	Spoiler bool
	// SupportSeen says the poster has been shown the support screen for this
	// post and is sending it again; see checkContent.
	SupportSeen bool
	Upload      *upload
}

// submissionForm is a JSON request body that can be reduced to a submission.
// The two shapes differ so that each endpoint keeps rejecting fields that do
// not belong to it.
type submissionForm interface {
	submission() submission
}

// parseSubmission reads a post submission in whichever encoding the client
// used: multipart/form-data when it carries an image, JSON otherwise. It writes
// the error response itself and reports success.
func (a *App) parseSubmission(w http.ResponseWriter, r *http.Request, form submissionForm) (*submission, bool) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if !decodeJSON(w, r, form) {
			return nil, false
		}
		sub := form.submission()
		return &sub, true
	}

	// With uploads switched off a post can carry nothing but its text fields,
	// so its body is held to what a JSON post may be (maxRequestBytes, well
	// above the longest legal post) instead of an image's worth. A body past
	// that is carrying a file, and is refused for the reason that will not
	// change, uploads switched off, once that much has been read: the rest of
	// the file is never read, and no size limit answers first.
	bodyLimit, inMemory := a.cfg.MaxUploadBytes+multipartOverhead, int64(multipartOverhead)
	if !a.cfg.UploadsEnabled {
		bodyLimit, inMemory = maxRequestBytes, maxRequestBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
	if err := r.ParseMultipartForm(inMemory); err != nil {
		switch {
		case tooLarge(err) && !a.cfg.UploadsEnabled:
			writeError(w, http.StatusForbidden, uploadsSwitchedOff)
		case tooLarge(err):
			writeError(w, http.StatusRequestEntityTooLarge, "attachment too large")
		default:
			writeError(w, http.StatusBadRequest, "invalid form data")
		}
		return nil, false
	}
	defer r.MultipartForm.RemoveAll()

	sub := &submission{
		Subject: r.FormValue("subject"),
		Name:    r.FormValue("name"),
		Body:    r.FormValue("body"),
		Sage:    isTruthy(r.FormValue("sage")),
		Badge:   isTruthy(r.FormValue("badge")),
		Spoiler: isTruthy(r.FormValue("spoiler")),

		SupportSeen: isTruthy(r.FormValue("support_seen")),
	}

	file, header, err := r.FormFile(uploadFieldName)
	if errors.Is(err, http.ErrMissingFile) {
		return sub, true
	}
	if err != nil {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "attachment too large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "invalid attachment")
		return nil, false
	}
	defer file.Close()

	// A file small enough to fit under the switched-off body limit above has
	// been read with the form; it is refused here, before it is copied out or
	// decoded. Only a part with bytes in it counts: an untouched file input
	// still sends an empty one, and that post carries no image.
	if !a.cfg.UploadsEnabled && header.Size > 0 {
		writeError(w, http.StatusForbidden, uploadsSwitchedOff)
		return nil, false
	}

	data, err := io.ReadAll(io.LimitReader(file, a.cfg.MaxUploadBytes+1))
	if err != nil {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "attachment too large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "could not read attachment")
		return nil, false
	}
	if int64(len(data)) > a.cfg.MaxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "attachment too large")
		return nil, false
	}
	// An untouched file input still yields an empty part in some browsers.
	if len(data) == 0 {
		return sub, true
	}
	// What the file says about itself, its format and its decoded size, is
	// cheap to read, so a file that is not an accepted image, or one too large
	// to decode, is refused here, before flood control starts a cooldown for
	// it. Decoding, the cost the upload cooldown is there for, waits until
	// after: an image whose pixels then fail to decode has started its
	// cooldowns.
	if _, err := media.Inspect(data); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	sub.Upload = &upload{Filename: sanitizeFilename(header.Filename), Data: data}
	return sub, true
}

// storeUpload validates and persists an attachment. Blobs are written before
// the database row exists; callers must call discardUpload if the surrounding
// transaction fails.
func (a *App) storeUpload(ctx context.Context, board string, up *upload) (*media.StoredImage, error) {
	if up == nil {
		return nil, nil
	}
	img, err := media.Save(ctx, a.media, board, up.Data, time.Now())
	if err != nil {
		return nil, err
	}
	return &img, nil
}

// discardUpload removes blobs whose post never made it into the database.
func (a *App) discardUpload(img *media.StoredImage) {
	if img == nil {
		return
	}
	_ = media.Remove(context.Background(), a.media, *img)
}

// rejectUpload maps an upload failure onto a response. Validation problems are
// the client's fault; anything else is ours.
func (a *App) rejectUpload(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, media.ErrUnsupportedFormat),
		errors.Is(err, media.ErrCorruptImage),
		errors.Is(err, media.ErrImageTooLarge):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		a.fail(w, r, err, "could not store attachment")
	}
}

// attachmentInput pairs the stored blob metadata with what the submission said
// about it: the original filename and whether it is a spoiler.
func attachmentInput(sub *submission, img *media.StoredImage) *newAttachment {
	if img == nil {
		return nil
	}
	name := ""
	if sub.Upload != nil {
		name = sub.Upload.Filename
	}
	return &newAttachment{OriginalName: name, Image: *img, Spoiler: sub.Spoiler}
}

// isTruthy reads the checkbox and flag encodings a multipart form may use for a
// boolean field.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func (a *App) handleMediaOriginal(w http.ResponseWriter, r *http.Request) {
	a.serveMedia(w, r, media.OriginalPrefix)
}

func (a *App) handleMediaThumb(w http.ResponseWriter, r *http.Request) {
	a.serveMedia(w, r, media.ThumbPrefix)
}

func (a *App) serveMedia(w http.ResponseWriter, r *http.Request, prefix string) {
	key, err := media.CleanKey(prefix + "/" + r.PathValue("path"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	// A held post's image is for the moderators until one approves it, so it
	// is answered as if it did not exist unless the moderation key comes with
	// the request, and nothing on either answer may be kept by a cache: the
	// same address serves the public once the post is approved. An image in
	// an expired thread is answered the same way, as its thread is, and so is
	// a file whose rows are gone (MediaPublic).
	cacheControl := mediaCacheControl
	public, err := a.store.MediaPublic(r.Context(), prefix == media.ThumbPrefix, r.PathValue("path"))
	if err != nil {
		a.fail(w, r, err, "could not read attachment")
		return
	}
	if !public {
		cacheControl = "no-store"
		if !a.isModerator(r) {
			w.Header().Set("Cache-Control", cacheControl)
			writeError(w, http.StatusNotFound, "not found")
			return
		}
	}

	rc, obj, err := a.media.Get(r.Context(), key)
	if err != nil {
		if errors.Is(err, media.ErrNotFound) || errors.Is(err, media.ErrInvalidKey) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not read attachment")
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, "", obj.ModTime, rc)
}

// sanitizeFilename keeps a display-only version of the uploaded filename: the
// base name, trimmed, stripped of control characters and of the bidirectional
// controls (which could make "gnp.exe" read as "exe.png"; see collapseControl),
// and valid UTF-8 so it can be stored in a text column. Truncation happens on
// rune boundaries.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	name = strings.ToValidUTF8(name, "")
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || bidiControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	if utf8.RuneCountInString(name) > maxOriginalNameLen {
		ext := filepath.Ext(name)
		if utf8.RuneCountInString(ext) > 16 {
			ext = ""
		}
		name = truncateRunes(name, maxOriginalNameLen-utf8.RuneCountInString(ext)) + ext
	}
	return name
}

func tooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
