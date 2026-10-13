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
	Badge  bool
	Upload *upload
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

	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxUploadBytes+multipartOverhead)
	if err := r.ParseMultipartForm(multipartOverhead); err != nil {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "attachment too large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "invalid form data")
		return nil, false
	}
	defer r.MultipartForm.RemoveAll()

	sub := &submission{
		Subject: r.FormValue("subject"),
		Name:    r.FormValue("name"),
		Body:    r.FormValue("body"),
		Sage:    isTruthy(r.FormValue("sage")),
		Badge:   isTruthy(r.FormValue("badge")),
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

	// Checked before the bytes are read, and only for a part that has some:
	// an untouched file input still sends an empty one, and that post carries
	// no image.
	if !a.cfg.UploadsEnabled && header.Size > 0 {
		writeError(w, http.StatusForbidden, "image uploads are switched off on this server")
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

// attachmentInput pairs the stored blob metadata with the original filename.
func attachmentInput(up *upload, img *media.StoredImage) *newAttachment {
	if img == nil {
		return nil
	}
	name := ""
	if up != nil {
		name = up.Filename
	}
	return &newAttachment{OriginalName: name, Image: *img}
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
	w.Header().Set("Cache-Control", mediaCacheControl)
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, "", obj.ModTime, rc)
}

// sanitizeFilename keeps a display-only version of the uploaded filename: the
// base name, trimmed, stripped of control characters, and valid UTF-8 so it can
// be stored in a text column. Truncation happens on rune boundaries.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	name = strings.ToValidUTF8(name, "")
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
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
