package media

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBusy is an upload that found no room in a budget, for its bytes
// (ReserveUpload) or for processing them (Process), in the time it had to
// wait. Nothing has been stored for it. It is the server's state, not the
// upload's fault: the same upload may go through a few seconds later.
var ErrBusy = errors.New("media: no room to take the image in time")

// UploadBudget is the memory the uploads themselves may take between them,
// from before most of their bytes are read until their post has been answered
// (ReserveUpload): the request bodies, which processingBudget does not count.
// It holds about eight bodies carrying an image at the default size limit
// (8 MiB), seven as they are read, when the start of each is in memory twice
// (ReserveUploadParts), and more of smaller ones. Without it nothing bounded
// how many were held at once: every request a host lets an instance take at
// once (Cloud Run's containerConcurrency) could be holding one. A body is
// reserved whole, so none may be larger than this: a reservation is capped at
// the budget, and a body past it would be read beyond what was reserved. The
// server refuses to start with an upload size limit that allows one, and with
// any over MaxUploadBytes (cost.go), which is lower.
const UploadBudget int64 = 64 << 20

// processingAllowance is how long before its deadline an upload stops waiting
// for room, in either budget, so that what is left is time to decode, re-encode
// and thumbnail it and to store both renditions. What bounds that work is not
// the upload's size — a few dozen bytes of a progressive JPEG's scan can make
// image/jpeg revisit every block of a 25-megapixel picture — but its shape,
// which Inspect reads from the header: the pixel cap (MaxPixels), the memory
// estimate (processingCost), and, for a JPEG, the scan cap
// (maxJPEGScanRevisits); and a PNG is written at deflate's fastest level
// (storedPNG), whose work follows the length of the rows rather than what they
// hold. So no admitted upload is processed for more than about ten seconds of
// CPU on one core, but one whose image data is made slow to inflate (below).
// The slowest kinds measured, each on one core:
//   - a 16-bit PNG at the most pixels the memory estimate admits of one (~23
//     megapixels as a square of ~4800-4850, the full 25 as a strip of
//     2500×10000): ~4-5 s opaque, and ~6.5-7.5 s with a partial alpha in every
//     pixel and rows filtered by Paeth, the slowest PNG measured, square or
//     strip. Like the WebP figures below, those are on the scale on which the
//     CMYK JPEG takes ~8.5-10 s, as on a busy host (below), rather than the
//     ~5.5-7.5 s its own bullet gives: measured beside that JPEG, in the same
//     runs, the two took about half and three quarters of its time. The alpha
//     costs it more than the filter: every colour channel of every pixel is
//     then reduced to what its alpha shows (reduceHiddenColour, which skips a
//     pixel opaque or clear), the PNG is written at 8 bytes a pixel rather than
//     an opaque one's 6, which takes a fifth to a third longer, and the
//     thumbnail is scaled through the alpha; Paeth, the costliest filter to
//     undo, about doubles the decode, to ~1 s. At 8 bits, one with transparency
//     of about 7 MiB, ~2-3 s. Written at deflate's default level, rows crafted
//     to make it search took a 16-bit one of 1.4 MB ~17 s.
//   - a WebP stored as a PNG: a lossless one of 25 megapixels, which a crafted
//     file of a few dozen bytes can be, as a lossless bitstream can spend no
//     bits on a pixel, ~4-5 s, and ~5.5-6.5 s with its predictor, cross-colour
//     and subtract-green transforms; and a lossy one with transparency of 19
//     megapixels (4362×4362), the largest square the memory estimate admits, or
//     of 21 megapixels in a strip (1300×16178), about the most pixels it
//     admits, which with the CMYK JPEG below is the slowest upload admitted:
//     ~4.5-5.5 s for a photograph half transparent throughout, ~6.5-7.5 s for
//     noise that fills the default size limit (8 MiB), and ~8-9.5 s with an
//     alpha plane crafted as a lossless bitstream that runs every transform. Of
//     that, decoding takes ~1-4 s, converting the picture for image/png
//     (lossyWebPToNRGBA) ~1 s, writing the PNG, of up to ~70 MB, ~2-4 s, and
//     the thumbnail ~1 s. The time follows the colour as well as the alpha:
//     with an alpha opaque but for rounded corners it takes ~3.5-4.5 s over a
//     photograph and ~5-5.5 s over a synthetic one or noise, and with one clear
//     throughout, whose PNG is written in a fraction of a second, ~1.5-2.5 s
//     over a photograph. Those were measured in runs in which the opaque 16-bit
//     PNG took its ~4-5 s and the CMYK JPEG ~8.5-10 s, as on a busy host
//     (below); in runs where the CMYK JPEG took ~6-6.5 s, a clear alpha took
//     ~1.4-1.9 s over a photograph and ~2.5-2.7 s over noise that fills the
//     default size limit, which takes longer to decode.
//   - a JPEG at the scan cap (jpegScanBlockVisits weighs each scan by its
//     work): a progressive one of ~0.8 million blocks, the most the memory
//     estimate admits of one (a 25-megapixel 4:2:2 picture or ~17 megapixels
//     at 4:4:4), decodes in ~3-4 s and Process takes ~4-6 s, whether 64 first
//     passes (~50 million block visits) or ~32 refinement passes fill it. A
//     baseline one is admitted at 25 megapixels whatever its sampling, and its
//     10 scans at the cap decode in ~3.5-4.5 s at 4:4:4 (1.2 million blocks)
//     and ~5-6 s with four components at full resolution (an Adobe CMYK file,
//     1.6 million, the most of any JPEG), whose Process takes ~5.5-7.5 s; a
//     25-megapixel grey or 4:2:0 one decodes in ~2-3 s. A file past the cap is
//     refused from its header, before any cooldown — before the cap one could
//     repeat a cheap scan thousands of times and decode for thousands of
//     seconds.
//   - a GIF of many colours: 25 frames of a 2000×2000 canvas, which the frame
//     cap (maxGIFFramePixels) lets come to 100 million pixels, each drawn from
//     a table of its own of 256 colours, ~4-4.5 s whichever colours they are,
//     and about 5 s when each frame leaves pixels undrawn (~4.9-4.95 s the
//     best of three runs on a quiet core, single runs up to ~5.7 s), for whose
//     colours its rows are gone over a second time (undrawnInTable), however
//     those pixels lie: every other row, every other pixel, half at random or
//     all but two a row. Each colour is found in a frame's table in the same
//     three steps (colourIndex), a tenth to a quarter slower than the hash it
//     replaced over colours the hash spread out, which took ~3.5-4.5 s. Four
//     5000×5000 frames of one table take ~3-3.5 s, and frames a pixel wide,
//     whose rows the cap counts as 64 pixels each (gifRowPixels), a few
//     tenths.
//
// Inflating a PNG's image data follows its size, not its shape: image/png
// inflates the whole stream, and empty deflate blocks, each with codes that
// compress/flate builds tables for afresh, inflate to nothing at about three
// quarters of a second of CPU on one core for every MiB. A picture of one
// pixel padded with them took ~6-7 s at the default size limit (8 MiB) and
// ~20-24 s at MaxUploadBytes (31 MiB), more than processingAllowance, so its
// deadline may come first; the decode reads through a ctxReader, and stopped
// within a few milliseconds of its context ending. An ICC profile's stream is
// inflated no further than iccStreamRead, a fifth of a second of such blocks
// (pngProfileUsable).
//
// A slower core, or other uploads beside it, makes that longer (with the
// host's other cores busy, the CMYK JPEG's decode took about 1.4 times the
// time above, and the opaque 16-bit PNG and the lossless WebP barely more), so
// the deadline still has the last word: Save stores nothing once it has passed,
// and the work stops when the context ends, at the next point that asks
// (Process) — which for a decode that read its whole input before its costly
// passes, as image/jpeg does a small progressive file, is the end of the
// decode. A context without a deadline waits for as long as it lasts.
const processingAllowance = 15 * time.Second

// budget shares out memory among the uploads using it at once, each taking
// its share for as long as it runs. Uploads are served in the order they
// asked: a large one at the head of the queue waits for room and smaller ones
// queue behind it rather than slipping past, so it cannot be starved. It is a
// weighted semaphore; golang.org/x/sync/semaphore is the same idea, but not a
// dependency here.
type budget struct {
	mu      sync.Mutex
	size    int64
	used    int64
	waiters list.List // of *budgetWaiter, in arrival order
}

type budgetWaiter struct {
	n     int64
	ready chan struct{} // closed once the waiter holds its n
}

func newBudget(size int64) *budget { return &budget{size: size} }

// processing and uploads are the budgets Process and ReserveUpload take
// from. They are variables so tests can narrow them; nothing else changes
// them.
var (
	processing = newBudget(processingBudget)
	uploads    = newBudget(UploadBudget)
)

// ReserveUpload waits until n bytes fit in the upload budget, for a request
// body of at most n bytes that is about to be read, and returns the function
// that gives them back once nothing holds the upload any more. It waits as
// acquire does, so a context with a deadline gets ErrBusy once too little of
// it is left to process the upload after it has been read. An n over the
// whole budget waits for all of it.
func ReserveUpload(ctx context.Context, n int64) (release func(), err error) {
	return uploads.acquire(ctx, n)
}

// TryReserveUpload takes n bytes of the upload budget if they are free now and
// no upload waits for room ahead of it, as ReserveUpload would without
// waiting, and returns the function that gives them back; otherwise it takes
// nothing and reports false. It never waits.
func TryReserveUpload(n int64) (release func(), ok bool) {
	return uploads.tryAcquire(n)
}

// ReserveUploadParts is ReserveUpload for room that is given back in parts:
// it waits once, at one place in the queue, until all of the parts fit
// together, and returns a function for each part that gives that part back.
// Room an upload goes on to hold more of is taken so in one ask, rather than
// in one ask after another, each waiting its turn while the upload holds what
// the one before took: uploads that each held room and waited for more could
// hold all of the budget between them, none of them able to go on until one
// gave up. Over the whole budget, the parts are taken in order as far as it
// goes. An upload that waits while it holds room, as a request body taken a
// piece at a time does, is the caller's to keep from that: internal/app lets
// the uploads waiting hold no more than leaves room for the largest ask once
// the uploads not waiting have given theirs back (waitingRoom).
func ReserveUploadParts(ctx context.Context, parts ...int64) (releases []func(), err error) {
	return uploads.acquireParts(ctx, parts)
}

// TryReserveUploadParts takes the parts together, as ReserveUploadParts does,
// if they are free now and no upload waits for room ahead of them, and
// returns a function for each part that gives it back; otherwise it takes
// nothing and reports false. It never waits.
func TryReserveUploadParts(parts ...int64) (releases []func(), ok bool) {
	return uploads.tryAcquireParts(parts)
}

// acquireProcessing waits until cost fits in the processing budget and returns
// the function that gives it back, as acquire does. A cost over the whole
// budget is refused by Inspect before this is reached; here it is capped at
// the budget, so it waits for the budget to be empty rather than forever.
func acquireProcessing(ctx context.Context, cost int64) (func(), error) {
	return processing.acquire(ctx, cost)
}

// acquire takes n from the budget and returns the function that gives it
// back. With no room it waits in line: until it is served, until the context
// ends (the context's error), or, for a context with a deadline, until only
// processingAllowance of it is left (ErrBusy). Room that is there when it
// asks is taken whatever the time: only waiting is cut short.
func (b *budget) acquire(ctx context.Context, n int64) (func(), error) {
	releases, err := b.acquireParts(ctx, []int64{n})
	if err != nil {
		return nil, err
	}
	return releases[0], nil
}

// acquireParts takes the parts from the budget together, as acquire takes
// their sum, and returns a function for each that gives it back (shares).
func (b *budget) acquireParts(ctx context.Context, parts []int64) ([]func(), error) {
	if err := ctx.Err(); err != nil || len(parts) == 0 {
		return nil, err
	}
	n := b.whole(parts)

	b.mu.Lock()
	if b.waiters.Len() == 0 && b.used+n <= b.size {
		b.used += n
		b.mu.Unlock()
		return b.shares(n, parts), nil
	}
	var giveUp <-chan time.Time
	if deadline, ok := ctx.Deadline(); ok {
		wait := time.Until(deadline) - processingAllowance
		if wait <= 0 {
			b.mu.Unlock()
			return nil, ErrBusy
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		giveUp = timer.C
	}
	w := &budgetWaiter{n: n, ready: make(chan struct{})}
	elem := b.waiters.PushBack(w)
	b.mu.Unlock()

	var err error
	select {
	case <-w.ready:
		return b.shares(n, parts), nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-giveUp:
		err = ErrBusy
	}
	b.mu.Lock()
	select {
	case <-w.ready:
		// Granted as the wait ended: give it straight back.
		b.mu.Unlock()
		b.release(n)
	default:
		head := b.waiters.Front() == elem
		b.waiters.Remove(elem)
		if head {
			// Whoever was queued behind may fit now.
			b.grant()
		}
		b.mu.Unlock()
	}
	return nil, err
}

// tryAcquire takes n from the budget, as acquire does when there is room and
// no queue, and returns the function that gives it back; or, with no room or
// a queue, takes nothing and reports false.
func (b *budget) tryAcquire(n int64) (func(), bool) {
	releases, ok := b.tryAcquireParts([]int64{n})
	if !ok {
		return nil, false
	}
	return releases[0], true
}

// tryAcquireParts takes the parts together, as tryAcquire takes their sum.
func (b *budget) tryAcquireParts(parts []int64) ([]func(), bool) {
	if len(parts) == 0 {
		return nil, true
	}
	n := b.whole(parts)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waiters.Len() != 0 || b.used+n > b.size {
		return nil, false
	}
	b.used += n
	return b.shares(n, parts), true
}

// whole is what the budget takes for parts: their sum, but at least a byte
// and at most the whole budget.
func (b *budget) whole(parts []int64) int64 {
	var n int64
	for _, p := range parts {
		n = min(n+min(max(p, 0), b.size), b.size)
	}
	return max(n, 1)
}

// shares returns a function for each of parts that gives back its share of n,
// what the budget took for them (whole): each part its own, in order, as far
// as n goes, and the first also the byte n holds when the parts come to none,
// so that a part of none holds nothing whether it is given back or not.
func (b *budget) shares(n int64, parts []int64) []func() {
	releases := make([]func(), len(parts))
	k := make([]int64, len(parts))
	left := n
	for i, p := range parts {
		k[i] = min(max(p, 0), left)
		left -= k[i]
	}
	k[0] += left
	for i := range parts {
		share := k[i]
		releases[i] = func() { b.release(share) }
	}
	return releases
}

func (b *budget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	if b.used < 0 {
		b.mu.Unlock()
		panic("media: budget released more than it held")
	}
	b.grant()
	b.mu.Unlock()
}

// grant hands out room to the waiters at the head of the queue, in order, for
// as long as the next one fits. b.mu is held.
func (b *budget) grant() {
	for {
		front := b.waiters.Front()
		if front == nil {
			return
		}
		w := front.Value.(*budgetWaiter)
		if b.used+w.n > b.size {
			return
		}
		b.used += w.n
		b.waiters.Remove(front)
		close(w.ready)
	}
}

// held reports what the budget has handed out, for tests.
func (b *budget) held() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
