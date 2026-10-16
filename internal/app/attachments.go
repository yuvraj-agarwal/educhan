package app

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// multipartOverhead is the slack allowed on top of the attachment size limit
// for MIME headers and the text fields sharing the request body.
const multipartOverhead = 1 << 20

// maxFormParts caps the parts of a multipart post. The form has seven at most;
// past this the body is not a post.
const maxFormParts = 100

// uploadFieldName is the multipart field carrying the attachment.
const uploadFieldName = "image"

// uploadsSwitchedOff is the refusal of an image post on a server with uploads
// switched off. Clients match on it, so it does not change.
const uploadsSwitchedOff = "image uploads are switched off on this server"

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

// upload is an image received with a post, before it has been validated. The
// filename the client sent with it is not kept: a camera's or a phone's names a
// date and sometimes a person (IMG_20261013_Priya.jpg), and nothing educhan
// does needs it.
type upload struct {
	Data []byte
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
	Upload  *upload
	// reserved gives back the room taken in the upload budget for reading
	// the body (media.ReserveUpload), for a multipart post with uploads on.
	reserved func()
}

// release gives back what the submission holds of the upload budget. The
// handler defers it as soon as it has the submission: the upload's bytes are
// held until the post is answered.
func (s *submission) release() {
	if s.reserved != nil {
		s.reserved()
		s.reserved = nil
	}
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
//
// A multipart body, with uploads on, is read only once there is room for it
// in the upload budget (media.ReserveUpload), which counts the bodies being
// read and held until their posts are answered: as much as the request says
// it is, or the most it may be. ctx is the post's reserveContext: with no room
// in time the body is read to its end unkept and the post is answered 503
// (busy), before flood control has charged it anything.
//
// A multipart body sets its own connection deadlines, because it may take
// longer than one WriteTimeout from the end of its headers allows: the body
// has uploadReadTimeout to arrive from when there is room to read it, and a
// body slower than that is answered 408 (upload too slow), not the 503 a
// server with no room gives; the write deadline covers that read and the
// answer to it, and processContext resets it for the processing that follows.
// The deadlines are set once the wait for room is over, whatever its outcome,
// so a post that waited has its whole uploadReadTimeout for its body — the
// wait was the server's, and taken out of the body's time it would turn a
// busy server into a slow upload — and a post turned away has that long for
// the rest of its body to be read past and the answer written.
func (a *App) parseSubmission(ctx context.Context, w http.ResponseWriter, r *http.Request, form submissionForm) (*submission, bool) {
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
	bodyLimit := a.cfg.MaxUploadBytes + multipartOverhead
	if !a.cfg.UploadsEnabled {
		bodyLimit = maxRequestBytes
	}
	size := bodyLimit
	if r.ContentLength >= 0 {
		size = min(r.ContentLength, bodyLimit)
	}
	var (
		reserved func()
		err      error
	)
	if a.cfg.UploadsEnabled {
		reserved, err = media.ReserveUpload(ctx, size)
	}
	// Nothing has read the body yet: its time starts now.
	rc := http.NewResponseController(w)
	now := time.Now()
	_ = rc.SetReadDeadline(now.Add(a.uploadReadTimeout))
	_ = rc.SetWriteDeadline(now.Add(a.uploadReadTimeout + answerAllowance))
	if err != nil {
		// The browser is still sending the body. Answered before it has
		// finished, a connection closed under it can reach the poster as a
		// network failure instead of this answer, so the body is read, and
		// nothing of it kept.
		_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, bodyLimit))
		a.rejectUpload(w, r, err)
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
	sub, status, msg := a.readMultipart(r, size)
	if status != 0 {
		if reserved != nil {
			reserved()
		}
		writeError(w, status, msg)
		return nil, false
	}
	sub.reserved = reserved
	return sub, true
}

// readMultipart reads a multipart post's text fields and its image, from a
// body of at most size bytes. Everything else in it is read past and not
// kept, the filename the image came with included. On failure it returns the
// status and the message to answer with.
//
// It reads the body itself rather than through ParseMultipartForm, which
// would keep a file over its memory limit in a temporary file (memory again,
// on a host whose disk is memory, as Cloud Run's is) and then hand it over to
// be copied whole: the image is read once, into a buffer the size of what is
// left of the body, so what the upload budget reserved for it is what it
// takes. The text fields may come to what a whole JSON post may be
// (maxRequestBytes) between them; the first of each name is the one read, as
// FormValue would.
func (a *App) readMultipart(r *http.Request, size int64) (*submission, int, string) {
	refuse := func(err error) (*submission, int, string) {
		switch {
		case isReadTimeout(err):
			// The body did not arrive inside uploadReadTimeout: the client's
			// doing, not the server's load, so 408 rather than the 503 busy.
			return nil, http.StatusRequestTimeout, "upload too slow"
		case tooLarge(err) && !a.cfg.UploadsEnabled:
			return nil, http.StatusForbidden, uploadsSwitchedOff
		case tooLarge(err), errors.Is(err, errPartTooLarge):
			return nil, http.StatusRequestEntityTooLarge, "attachment too large"
		}
		return nil, http.StatusBadRequest, "invalid form data"
	}

	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		return refuse(err)
	}
	body := &countingReader{r: r.Body}
	mr := multipart.NewReader(body, params["boundary"])

	fields := map[string]string{}
	var (
		text    int64 // bytes of text field kept so far
		image   []byte
		imaged  bool // an image part has been read
		hasFile bool // it had bytes in it, with uploads off
	)
	for parts := 0; ; parts++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return refuse(err)
		}
		if parts == maxFormParts {
			return refuse(nil)
		}
		name := part.FormName()
		switch {
		case name == uploadFieldName && part.FileName() != "" && !imaged:
			imaged = true
			if !a.cfg.UploadsEnabled {
				// Nothing of it is kept: whether it has any bytes is all
				// there is to know.
				n, err := io.ReadFull(part, make([]byte, 1))
				if err != nil && err != io.EOF {
					return refuse(err)
				}
				hasFile = n > 0
				continue
			}
			// What is left of the body is the most the image can be, give
			// or take the little the reader has read ahead of it.
			left := size - body.n + 8<<10
			if image, err = readAtMost(part, left, a.cfg.MaxUploadBytes); err != nil {
				return refuse(err)
			}
		case part.FileName() == "" && isPostField(name):
			if _, seen := fields[name]; seen {
				continue
			}
			value, err := readAtMost(part, 512, maxRequestBytes-text)
			if errors.Is(err, errPartTooLarge) {
				// More text than a whole JSON post may carry: refused as
				// one would be.
				return nil, http.StatusBadRequest, "invalid form data"
			}
			if err != nil {
				return refuse(err)
			}
			text += int64(len(value))
			fields[name] = string(value)
		}
	}

	// A file small enough to fit under the switched-off body limit above has
	// been read with the form; it is refused here, before anything is
	// decoded. Only a part with bytes in it counts: an untouched file input
	// still sends an empty one, and that post carries no image.
	if hasFile {
		return nil, http.StatusForbidden, uploadsSwitchedOff
	}
	sub := &submission{
		Subject: fields["subject"],
		Name:    fields["name"],
		Body:    fields["body"],
		Sage:    isTruthy(fields["sage"]),
		Badge:   isTruthy(fields["badge"]),
		Spoiler: isTruthy(fields["spoiler"]),
	}
	if len(image) == 0 {
		return sub, 0, ""
	}
	// What the file says about itself, its format and its decoded size, is
	// cheap to read, so a file that is not an accepted image, one of too many
	// pixels or too costly to process, or an animated WebP, which nothing
	// here decodes, is refused here, before flood control starts a cooldown
	// for it. Decoding, the cost the upload cooldown is there for, waits until
	// after: an image whose pixels then fail to decode has started its
	// cooldowns.
	if _, err := media.Inspect(image); err != nil {
		return nil, http.StatusBadRequest, err.Error()
	}
	sub.Upload = &upload{Data: image}
	return sub, 0, ""
}

// isPostField reports whether name is one of the text fields a post may
// carry in a multipart body.
func isPostField(name string) bool {
	switch name {
	case "subject", "name", "body", "sage", "badge", "spoiler":
		return true
	}
	return false
}

// errPartTooLarge is a part longer than readAtMost's limit.
var errPartTooLarge = errors.New("part too large")

// readAtMost reads r to its end into a buffer made for hint bytes, which
// grows only if r turns out longer, and fails with errPartTooLarge once
// more than limit bytes have come.
func readAtMost(r io.Reader, hint, limit int64) ([]byte, error) {
	// One more than the most that is kept, so a part that fits is read to
	// its end without the buffer growing to find out.
	buf := make([]byte, 0, max(min(hint, limit)+1, 512))
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if int64(len(buf)) > limit {
			return nil, errPartTooLarge
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
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
// the client's fault; no room in time (tooBusy) is the server's state and
// answers 503; anything else is ours.
func (a *App) rejectUpload(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, media.ErrUnsupportedFormat),
		errors.Is(err, media.ErrCorruptImage),
		errors.Is(err, media.ErrImageTooLarge),
		errors.Is(err, media.ErrImageTooCostly),
		errors.Is(err, media.ErrStoredTooLarge):
		writeError(w, http.StatusBadRequest, err.Error())
	case tooBusy(err) && r.Context().Err() == nil:
		answerBusy(w)
	default:
		a.fail(w, r, err, "could not store attachment")
	}
}

// tooBusy reports whether err is a post's upload that could not be taken in
// the time the post had (postDeadline): no room in a budget in time
// (media.ErrBusy), or the processing deadline itself running out. A slow body
// is not this — it ends in a read-deadline error (isReadTimeout), answered 408
// — because the processing deadline starts only after the body is read
// (processContext).
func tooBusy(err error) bool {
	return errors.Is(err, media.ErrBusy) || errors.Is(err, context.DeadlineExceeded)
}

// isReadTimeout reports whether err is a request body that stopped arriving
// until its read deadline (uploadReadTimeout) ran out. The net poller surfaces
// that as os.ErrDeadlineExceeded, wrapped on its way up through the multipart
// reader and http.MaxBytesReader; a net.Error that reports a timeout is the
// same thing by another route.
func isReadTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// busyRetryAfter is how long a post turned away as busy is told to wait. The
// uploads ahead of it have had their time by then, or been turned away too.
const busyRetryAfter = 10 * time.Second

// answerBusy answers a post the server could not take in time: 503 with a
// Retry-After, like flood control's 429 with what to wait. Nothing was stored
// for it and no cooldown is left charged for it.
func answerBusy(w http.ResponseWriter) {
	seconds := int(busyRetryAfter / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error":       "too many images are being posted, try again in " + strconv.Itoa(seconds) + "s",
		"retry_after": seconds,
	})
}

// attachmentInput pairs the stored blob metadata with what the submission said
// about it, whether it is a spoiler, and the name it is shown under.
func attachmentInput(sub *submission, img *media.StoredImage) *newAttachment {
	if img == nil {
		return nil
	}
	return &newAttachment{Name: attachmentName(img.Path), Image: *img, Spoiler: sub.Spoiler}
}

// attachmentName is the name every image is shown under: "image" and the
// extension of the format it was stored in, whatever the poster's file was
// called. 0008_attachment_names.sql gave the rows from before the same name,
// from the same path, and renameAttachments gives it on every start to any an
// older release has stored since; both have to agree with this.
func attachmentName(storedPath string) string {
	return "image" + path.Ext(storedPath)
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

func tooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
