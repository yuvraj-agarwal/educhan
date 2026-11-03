package app

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// multipartOverhead is the slack allowed on top of the attachment size limit
// for MIME headers and the text fields sharing the request body.
const multipartOverhead = 1 << 20

// clientUploadShare is the most of the upload budget (media.UploadBudget)
// the bodies of one client's posts may hold at once: half of it. A body holds
// room for about what it has sent until an eighth of it has arrived, then for
// all of it, from before most of it is read until its post is answered
// (admitBody), and a post whose body never arrives whole never reaches flood
// control, so without a share one client could hold the whole budget with a
// few connections that send an eighth of large bodies and stop, turn every
// other image post away as busy, and be charged nothing for it but those
// bytes. Half is one upload of the largest size the server may be configured
// to take (maxUploadLimit and its multipartOverhead), and three at the default
// limit: more than one client can post at once, the upload cooldown being per
// client across the server.
const clientUploadShare = media.UploadBudget / 2

// errUploadShare is a post refused because its client's other posts hold
// their share of the upload budget already (clientUploadShare).
var errUploadShare = errors.New("too many images uploading at once from this client")

// uploadShares counts what each client's posts hold of the upload budget,
// keyed on shareKey, so that none holds more than clientUploadShare, and
// which clients have a post waiting for room in it, so that none has more
// than one (reserveUpload). A client's entries go once it holds nothing and
// has nothing waiting.
type uploadShares struct {
	mu      sync.Mutex
	held    map[string]int64
	waiting map[string]bool
}

// take records n more bytes held by client and returns the function that
// gives them back, or reports false, holding nothing, when that would take
// the client past clientUploadShare.
func (s *uploadShares) take(client string, n int64) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[client]+n > clientUploadShare {
		return nil, false
	}
	if s.held == nil {
		s.held = map[string]int64{}
	}
	s.held[client] += n
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.held[client] -= n; s.held[client] <= 0 {
				delete(s.held, client)
			}
		})
	}, true
}

// wait records that one of client's posts is waiting for room in the upload
// budget and returns the function that records it no longer is, or reports
// false, recording nothing, when another of its posts already is.
func (s *uploadShares) wait(client string) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiting[client] {
		return nil, false
	}
	if s.waiting == nil {
		s.waiting = map[string]bool{}
	}
	s.waiting[client] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.waiting, client)
		})
	}, true
}

// shareKey is the client a post's share of the upload budget is counted
// under: its address as flood control counts it (clientIP), except that an
// IPv6 address counts as its /64 network. A host on IPv6 can usually take any
// address in its /64, so counted per address the share would bound nothing:
// a few addresses would hold the whole budget. The people on one /64, a
// campus network's say, have one share between them, as those behind one IPv4
// address do. Flood control is not grouped so: its cooldowns are a wait after
// each post, which a whole network would feel, where a share is room for a
// few uploads at once.
func shareKey(client string) string {
	addr, err := netip.ParseAddr(client)
	if err != nil || !addr.Is6() || addr.Is4In6() {
		return client
	}
	network, err := addr.WithZone("").Prefix(64)
	if err != nil {
		return client
	}
	return network.String()
}

// of reports what client's posts hold, for tests.
func (s *uploadShares) of(client string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held[client]
}

// uploadPaceGrace is how far behind an even pace a multipart post's body may
// fall (pacedBody), and how long, with uploads on, a body has to send its
// first bytes before it is answered upload too slow, having held nothing
// (startBody). App.uploadPaceGrace is this, except in tests.
const uploadPaceGrace = 5 * time.Second

// pacedBody holds a multipart post's body to an even pace through its read
// window (uploadReadTimeout), so that a body which is not arriving gives its
// room in the upload budget back within uploadPaceGrace rather than at the end
// of the window. Before each read it moves the connection's read deadline to
// the time by which what has arrived would have arrived at the pace that
// brings the whole body (size, what is reserved for it once it is under way)
// in at the end of the window, plus the grace, and never past the window's
// end: a connection that sends nothing more is cut off after the grace, and
// one that trickles soon after, while a body on course to arrive inside the
// window, as every body the window admits must be on average, has the grace
// to fall behind by on the way. A body cut off is answered 400 (upload too
// slow, uploadTooSlowStatus), as one past the window is. A wait for room in
// the upload budget while the body is read (bodyRoom.take) is the server's,
// not the body's, and moves the window on by as long (pause).
//
// The grace is the whole of uploadPaceGrace from the first byte. A body sent
// over a long round trip arrives as TCP's slow start sends it, in bursts a
// round trip apart that double from about 15 KB, and in its first seconds
// falls well behind the pace of a large body before it catches up: a grace
// that began at a second and grew only as the body arrived refused an 8 MB
// image sent over a round trip of 0.65 s or more, a satellite link's or a busy
// mobile network's, within two seconds, where the link would have brought it
// in inside the window. What a body that stops holds is what it has sent
// (admitBody), so the grace need not be short to keep the budget from bodies
// that stop.
//
// Every body that holds room in the upload budget has a length, so its pace
// is its own: with uploads on, a multipart post without a Content-Length is
// refused before anything is reserved for it (parseSubmission). A body
// without one is read only with uploads off, holding no room, and is paced as
// the most it may be, a JSON post's worth (maxRequestBytes).
type pacedBody struct {
	io.ReadCloser
	setDeadline   func(time.Time) error
	start         time.Time
	window, grace time.Duration
	size, n       int64
}

func (p *pacedBody) Read(b []byte) (int, error) {
	end := p.end()
	due := end
	if p.size > 0 {
		due = p.start.Add(p.grace + time.Duration(float64(p.window)*float64(p.n)/float64(p.size)))
	}
	if due.After(end) {
		due = end
	}
	_ = p.setDeadline(due)
	k, err := p.ReadCloser.Read(b)
	p.n += int64(k)
	return k, err
}

// pause moves the body's window on by d, the time a wait for room took.
func (p *pacedBody) pause(d time.Duration) { p.start = p.start.Add(d) }

// end is when the body's window ends.
func (p *pacedBody) end() time.Time { return p.start.Add(p.window) }

// uploadHeadBytes is the most of a multipart post's body read before any
// room is taken for it (startBody): its first read, into a buffer the size of
// a connection's own. It is also the first piece of room the body takes, for
// that buffer (admitBody).
const uploadHeadBytes = 4 << 10

// uploadAdmitPart is how much of a multipart post's body, one byte in this
// many, has to arrive before room is taken for the whole of it (admitBytes).
const uploadAdmitPart = 8

// admitBytes is how much of a multipart post's body of size bytes admitBody
// reads, taking room a piece at a time, before it takes room for the whole
// body: an eighth of it (uploadAdmitPart), and at least its first piece
// (uploadHeadBytes), or all of a body smaller than that.
func admitBytes(size int64) int64 {
	return min(size, max(size/uploadAdmitPart, uploadHeadBytes))
}

// bufferedBody is a multipart post's body whose start has been read into
// memory (startBody, admitBody): the pieces it was read into, each let go
// once it has been read, then what that reading ended with, if it ended with
// an error, and otherwise the rest of the body.
type bufferedBody struct {
	chunks [][]byte
	err    error
	io.ReadCloser
}

func (s *bufferedBody) Read(b []byte) (int, error) {
	for len(s.chunks) > 0 {
		c := s.chunks[0]
		if len(c) == 0 {
			s.chunks[0] = nil
			s.chunks = s.chunks[1:]
			continue
		}
		n := copy(b, c)
		s.chunks[0] = c[n:]
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	return s.ReadCloser.Read(b)
}

// startBody waits up to uploadPaceGrace for the first bytes of a multipart
// post's body, with uploads on, before anything is taken for it: its client's
// share of the upload budget, room in the budget, or a place in the queue for
// room (parseSubmission). It reports false, having answered the post upload
// too slow (uploadTooSlowStatus), when none came in that time, and the
// connection is closed, the body unread. Otherwise it returns the body to
// read on from: what that first read brought, into a buffer of
// uploadHeadBytes, a connection's own buffer's worth, which is all a post
// holds before it holds room, and then what the read ended with, if it ended
// with an error — a body that ended or failed before its first byte has
// nothing to take room for, and the read of the form reports why — and
// otherwise the rest of the body.
func (a *App) startBody(w http.ResponseWriter, r *http.Request) (*bufferedBody, bool) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(a.uploadPaceGrace))
	head := make([]byte, uploadHeadBytes)
	var (
		n   int
		err error
	)
	for n == 0 && err == nil {
		n, err = r.Body.Read(head)
	}
	if n == 0 && isReadTimeout(err) {
		w.Header().Set("Connection", "close")
		writeError(w, uploadTooSlowStatus, uploadTooSlow)
		return nil, false
	}
	return &bufferedBody{chunks: [][]byte{head[:n]}, err: err, ReadCloser: r.Body}, true
}

// bodyRoom is what a multipart post's body holds of the upload budget, and
// of its client's share of it, taken a piece at a time (admitBody) through
// reserveUpload under ctx, the post's reserveContext. A wait for a piece is
// the server's, and moves the body's window on by as long (pacedBody.pause).
type bodyRoom struct {
	a     *App
	ctx   context.Context
	r     *http.Request
	pace  *pacedBody
	held  int64    // what it holds of the client's share
	gives []func() // each gives back a piece
	copy  func()   // gives back the room for the body's start a second time
}

// reserve takes shared bytes of the client's share and n of the upload
// budget, as reserveUpload does, and moves the body's window on by as long as
// that took.
func (h *bodyRoom) reserve(shared, n int64) (func(), error) {
	asked := time.Now()
	give, err := h.a.reserveUpload(h.ctx, h.r, shared, n)
	h.pace.pause(time.Since(asked))
	return give, err
}

// take takes n bytes more of room for the body.
func (h *bodyRoom) take(n int64) error {
	if n <= 0 {
		return nil
	}
	give, err := h.reserve(n, n)
	if err != nil {
		return err
	}
	h.gives = append(h.gives, give)
	h.held += n
	return nil
}

// takeCopy takes n bytes of the upload budget, and none of the client's
// share, for the start of the body that admitBody read into memory, which
// the read of the form copies into the image's buffer (readMultipart): until
// that read is over, the start is in memory twice, and the image's buffer
// is what the rest of the body's room is for.
func (h *bodyRoom) takeCopy(n int64) error {
	give, err := h.reserve(0, n)
	if err != nil {
		return err
	}
	h.copy = give
	return nil
}

// releaseCopy gives back the room takeCopy took, once the form has been read.
func (h *bodyRoom) releaseCopy() {
	if h.copy != nil {
		h.copy()
		h.copy = nil
	}
}

// release gives back all the room the body holds.
func (h *bodyRoom) release() {
	h.releaseCopy()
	for _, give := range h.gives {
		give()
	}
	h.gives, h.held = nil, 0
}

// admitBody reads the start of a multipart post's body, with uploads on,
// into memory, taking room for it in the upload budget as it comes (room),
// until the body is under way, admitBytes of it, an eighth, having arrived;
// then it takes room for the rest, the body's declared size (size) in all,
// which the read of the form fills (readMultipart, whose buffer for the image
// is the size of what is left of the body). It takes room a piece at a time,
// each before it reads into it: first for the buffer the body's first read
// began (startBody), uploadHeadBytes, and then each piece as large as all
// before it, so a body that stops holds at most twice what it has sent.
//
// Room taken for the whole of a body that has sent only its first bytes cost
// the sender nothing to hold: answered upload too slow once its grace ran
// out, before flood control, it came straight back. A few hundred addresses,
// as one IPv6 /48 gives, each with its one post in the queue for room
// (reserveUpload), kept the budget full and the queue long with a few
// hundred bytes a post, and an honest image post waited ten seconds behind
// them. Now holding a body's declared size costs an eighth of it sent, and
// holding less costs half of it.
//
// It reports, as away, the error a take of room failed with
// (reserveUpload), the body read no further, to be read past and the post
// turned away (turnAway); or, as failed, the error the body's read failed
// with before the body was under way, a body that fell behind its pace among
// them. Either way the room taken is the caller's to give back. A body that
// sent nothing before it ended or failed takes no room, and the read of the
// form reports why.
func (a *App) admitBody(b *bufferedBody, room *bodyRoom, size int64) (away, failed error) {
	head := b.chunks[0]
	if len(head) == 0 {
		return nil, nil
	}
	want := admitBytes(size)
	first := max(int64(len(head)), min(want, int64(cap(head))))
	if err := room.take(first); err != nil {
		return err, nil
	}
	b.chunks[0] = head[:len(head):first]
	for have := int64(len(head)); have < want && b.err == nil; {
		last := &b.chunks[len(b.chunks)-1]
		if len(*last) == cap(*last) {
			piece := min(want-have, have)
			if err := room.take(piece); err != nil {
				return err, nil
			}
			b.chunks = append(b.chunks, make([]byte, 0, piece))
			last = &b.chunks[len(b.chunks)-1]
		}
		k, err := b.ReadCloser.Read((*last)[len(*last):cap(*last)])
		*last = (*last)[:len(*last)+k]
		have += int64(k)
		b.err = err
	}
	if b.err != nil && b.err != io.EOF {
		return nil, b.err
	}
	buffered := room.held
	if err := room.take(size - room.held); err != nil {
		return err, nil
	}
	if err := room.takeCopy(buffered); err != nil {
		return err, nil
	}
	return nil, nil
}

// maxFormParts caps the parts of a multipart post. The form has seven at most;
// past this the body is not a post.
const maxFormParts = 100

// uploadFieldName is the multipart field carrying the attachment.
const uploadFieldName = "image"

// uploadsSwitchedOff is the refusal of an image post on a server with uploads
// switched off. Clients match on it, so it does not change.
const uploadsSwitchedOff = "image uploads are switched off on this server"

// lengthRequired is the refusal of a multipart post sent without a
// Content-Length while uploads are on (refuseUnsized).
const lengthRequired = "a post with a file needs a Content-Length"

// uploadTooSlow is the refusal of a multipart post whose body fell behind its
// pace (pacedBody) or did not arrive within uploadReadTimeout, whether it was
// being read or read past: answered uploadTooSlowStatus, with no Retry-After,
// since waiting does not make a connection faster. Clients match on it, so it
// does not change.
const uploadTooSlow = "upload too slow"

// uploadTooSlowStatus is what a body that fell behind is answered with: 400,
// not 408 Request Timeout. Chromium takes a 408 that arrives on a reused
// HTTP/1.1 keep-alive connection for an idle connection the server closed: it
// sends the request again on another connection, the whole body from the
// start, without telling the page, until a 408 comes on a fresh connection.
// A poster too slow to send the image once then sent it once for each idle
// connection and once more, waited that many times as long to be told it took
// too long, and held room in the upload budget and the client's share for each
// sending. No header stops that; only the status does. app.js knows this
// refusal by its words (UPLOAD_TOO_SLOW), as it knows the other refusals it
// has its own notice for.
const uploadTooSlowStatus = http.StatusBadRequest

// refuseUnsized answers a multipart post that came without a Content-Length,
// with uploads on: 411, before anything is reserved for its body or read of
// it (parseSubmission). Its body is not read past either, because only the
// client decides when it ends: the connection is closed once the answer is
// written, and the read deadline set to now, so the server does not wait on
// the rest of the body to close it.
func refuseUnsized(w http.ResponseWriter) {
	w.Header().Set("Connection", "close")
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	writeError(w, http.StatusLengthRequired, lengthRequired)
}

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
// A multipart body, with uploads on, holds room in the upload budget
// (media.ReserveUpload), which counts the bodies being read and held until
// their posts are answered, for what it has sent until it is under way, and
// then for as much as the request says it is (admitBody): its first bytes
// come before any (startBody), a body that sends none within uploadPaceGrace
// answered upload too slow having held nothing, neither room nor a place in
// the queue for room; room is then taken a piece at a time, each before it is
// read into, until an eighth of the body has arrived; and then room for the
// whole body is taken before the rest of it is read. So connections that
// declare a body and send little of it hold little, and keep the budget from
// no one. ctx is the post's reserveContext: with no room in time the body is
// read to its end unkept and the post is answered 503 (busy), before flood
// control has charged it anything. The room comes out of the client's share
// of the budget first (reserveUpload): a post whose client's other posts hold
// that already, or one of which already waits for room, is read to its end
// unkept and answered 429 at once, without waiting, and also charged nothing.
//
// Such a body has to say how long it is: one without a Content-Length
// (chunked) is answered 411 before anything is reserved for it or read of it
// (refuseUnsized), and charged nothing. Its length known only at its end, it
// would have had room reserved for the most a body may be, and could only be
// held to the pace of that, which cut a small body sent slowly off, or to
// none, which let a trickle of a few bytes a second hold that room for the
// whole read window: three addresses' shares turned every other image post
// away 503. Browsers send a Content-Length with every form.
//
// A multipart body sets its own connection deadlines, because it may take
// longer than one WriteTimeout from the end of its headers allows: the body has
// uploadReadTimeout to arrive from its first bytes, at an even pace with
// uploadPaceGrace to spare (pacedBody), and a body slower than that is
// answered 400 (upload too slow, uploadTooSlowStatus), not the 503 a server
// with no room gives; the write deadline covers that read and the answer to
// it, and processContext resets it for the processing that follows. A wait for
// room moves both deadlines on by as long, whatever its outcome, so a post that
// waited has its whole uploadReadTimeout for its body — the wait was the
// server's, and taken out of the body's time it would turn a busy server into
// a slow upload — and a post turned away has the rest of it for the rest of
// its body to be read past, at the same pace, and the answer written: upload
// too slow if it fell behind on the way, as with room, and otherwise why it
// was turned away (turnAway).
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
	} else if r.ContentLength < 0 {
		refuseUnsized(w)
		return nil, false
	}
	size := bodyLimit
	if r.ContentLength >= 0 {
		size = min(r.ContentLength, bodyLimit)
	}
	var started *bufferedBody
	if a.cfg.UploadsEnabled {
		var ok bool
		if started, ok = a.startBody(w, r); !ok {
			return nil, false
		}
	}
	// No more than the body's first read has been made: its time starts now.
	rc := http.NewResponseController(w)
	now := time.Now()
	_ = rc.SetReadDeadline(now.Add(a.uploadReadTimeout))
	_ = rc.SetWriteDeadline(now.Add(a.uploadReadTimeout + answerAllowance))
	pace := &pacedBody{ReadCloser: r.Body, setDeadline: rc.SetReadDeadline, start: now,
		window: a.uploadReadTimeout, grace: a.uploadPaceGrace, size: size}
	r.Body = pace
	room := &bodyRoom{a: a, ctx: ctx, r: r, pace: pace}
	if started != nil {
		pace.n = int64(len(started.chunks[0]))
		started.ReadCloser = pace
		r.Body = started
		away, failed := a.admitBody(started, room, size)
		_ = rc.SetWriteDeadline(pace.end().Add(answerAllowance))
		switch {
		case failed != nil:
			room.release()
			if isReadTimeout(failed) {
				writeError(w, uploadTooSlowStatus, uploadTooSlow)
			} else {
				writeError(w, http.StatusBadRequest, "invalid form data")
			}
			return nil, false
		case away != nil:
			// The browser is still sending the body. Answered before it has
			// finished, a connection closed under it can reach the poster as
			// a network failure instead of this answer, so the body is read,
			// and nothing of it kept.
			room.release()
			started.chunks = nil
			_, drained := io.Copy(io.Discard, http.MaxBytesReader(w, pace, bodyLimit-pace.n))
			a.turnAway(w, r, away, drained)
			return nil, false
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
	sub, status, msg := a.readMultipart(r, size)
	if status != 0 {
		room.release()
		writeError(w, status, msg)
		return nil, false
	}
	// The body's start has been copied into the image's buffer, or read past.
	room.releaseCopy()
	sub.reserved = room.release
	return sub, true
}

// reserveUpload takes room for a multipart post's body: shared bytes of its
// client's share first (clientUploadShare, the client as shareKey counts it),
// refused at once with errUploadShare when the client's other posts and what
// this one holds already hold it, and then n bytes of the upload budget
// itself, n being shared but for the room a body's start takes a second time
// (bodyRoom.takeCopy): what is free now (media.TryReserveUpload), or else what
// it waits for under ctx (media.ReserveUpload). One post of a client waits for room at a time:
// while one does, the client's next post that finds no room is refused at
// once with errUploadShare too. The queue for room is in the order posts
// asked, so without that a few clients, each within its share, could put
// their bodies' worth of posts in it ahead of everyone else's, each to hold its
// room for a while once it got it, and keep other image posts waiting; with
// it, each such client has one post in the queue. The function it returns
// gives back what it took.
func (a *App) reserveUpload(ctx context.Context, r *http.Request, shared, n int64) (func(), error) {
	client := shareKey(clientIP(r, a.cfg.TrustedProxyHops))
	share, ok := a.uploadShares.take(client, shared)
	if !ok {
		return nil, errUploadShare
	}
	reserved, ok := media.TryReserveUpload(n)
	if !ok {
		waited, ok := a.uploadShares.wait(client)
		if !ok {
			share()
			return nil, errUploadShare
		}
		var err error
		reserved, err = media.ReserveUpload(ctx, n)
		waited()
		if err != nil {
			share()
			return nil, err
		}
	}
	return func() {
		reserved()
		share()
	}, nil
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
// takes, with the start of the body that admitBody read into memory, which
// has room of its own until it has been copied (bodyRoom.takeCopy). The text fields may come to what a whole JSON post may be
// (maxRequestBytes) between them; the first of each name is the one read, as
// FormValue would.
func (a *App) readMultipart(r *http.Request, size int64) (*submission, int, string) {
	body := &countingReader{r: r.Body}
	refuse := func(err error) (*submission, int, string) {
		// A failed read of the body is the answer, whatever the form's
		// reader made of it: a part's header line that the read deadline
		// or the body limit cut short inside its name reaches it as a
		// malformed header, because bufio's ReadLine hands back what it has
		// of a line and drops the error that cut it.
		if body.err != nil {
			err = body.err
		}
		switch {
		case isReadTimeout(err):
			// The body did not arrive inside uploadReadTimeout, or fell
			// behind its pace: the client's doing, not the server's load, so
			// upload too slow rather than the 503 busy.
			return nil, uploadTooSlowStatus, uploadTooSlow
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

// countingReader counts the bytes read through it, and keeps the first error
// other than io.EOF that a read through it returned.
type countingReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && err != io.EOF && c.err == nil {
		c.err = err
	}
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

// turnAway answers a multipart post that reserveUpload turned away with err,
// once its body has been read past, the read having ended with drained. A body
// that fell behind its pace on the way is answered upload too slow
// (uploadTooSlowStatus), whatever the post was turned away for, as it would
// have been with room to read it: the connection is what the poster has to
// change. Any other is answered for why it was turned away. The answer is
// decided from those two errors alone, never from the request's context:
// net/http cancels that for any failed read of the body, a read deadline's
// included, so a busy refusal mapped through fail took the slow body for a
// client gone and answered nothing, which net/http sent as 200 with an empty
// body, and a client read as its post made.
func (a *App) turnAway(w http.ResponseWriter, r *http.Request, err, drained error) {
	switch {
	case isReadTimeout(drained):
		writeError(w, uploadTooSlowStatus, uploadTooSlow)
	case errors.Is(err, errUploadShare):
		// The client's own doing, like flood control: 429, with as long to
		// wait as a busy server asks, and no cooldown started.
		tooQuickly(w, "uploading images", busyRetryAfter)
	case tooBusy(err):
		answerBusy(w)
	default:
		// The wait for room ended with the request's context, the client having
		// gone away while its post waited.
		a.fail(w, r, err, "could not store attachment")
	}
}

// tooBusy reports whether err is a post's upload that could not be taken in
// the time the post had (postDeadline): no room in a budget in time
// (media.ErrBusy), or the processing deadline itself running out. A slow body
// is not this — it ends in a read-deadline error (isReadTimeout), answered
// upload too slow — because the processing deadline starts only after the
// body is read (processContext).
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
