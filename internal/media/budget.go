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
// from before their bytes are read until their post has been answered
// (ReserveUpload): the request bodies, which processingBudget does not count.
// It holds about eight bodies carrying an image at the default size limit
// (8 MiB), and more of smaller ones. Without it nothing bounded how many were
// held at once: every request a host lets an instance take at once (Cloud
// Run's containerConcurrency) could be holding one. A body is reserved whole,
// so none may be larger than this: a reservation is capped at the budget, and
// a body past it would be read beyond what was reserved. The server refuses
// to start with an upload size limit that allows one, and with any over
// MaxUploadBytes (cost.go), which is lower.
const UploadBudget int64 = 64 << 20

// processingAllowance is how long before its deadline an upload stops waiting
// for room, in either budget, so that what is left is time to decode, re-encode
// and thumbnail it and to store both renditions. What bounds that work is not
// the upload's size — a few dozen bytes of a progressive JPEG's scan can make
// image/jpeg revisit every block of a 25-megapixel picture — but its shape,
// which Inspect reads from the header: the pixel cap (MaxPixels), the memory
// estimate (processingCost), and, for a JPEG, the scan cap
// (maxJPEGScanRevisits), so that no admitted upload decodes for more than a few
// seconds of CPU on one core. The slowest kinds measured, each on one core:
//   - a 25-megapixel PNG with transparency of about 7 MiB: ~4 s.
//   - a JPEG at the scan cap: the most blocks the memory estimate admits are
//     ~0.8 million, a 25-megapixel 4:2:2 picture or ~17 megapixels at 4:4:4,
//     and at the cap its scans decode in ~3-4 s and Process takes ~4-6 s,
//     whether they are 64 progressive first passes (~50 million block
//     visits), ~32 refinement passes or 10 baseline scans (jpegScanBlockVisits
//     weighs each by its work); a 25-megapixel grey or 4:2:0 one decodes in
//     ~2-3 s. A file past the cap is refused from its header, before any
//     cooldown — before the cap one could repeat a cheap scan thousands of
//     times and decode for thousands of seconds.
//   - a GIF of many colours: 25 frames of a 2000×2000 canvas each drawn from a
//     table of its own and each leaving a pixel to clear, 4.5 MB, which the
//     frame cap (maxGIFFramePixels) lets come to 100 million pixels, ~4 s.
//     Four 5000×5000 frames of one table take ~2.5 s, and frames a pixel wide,
//     whose rows the cap counts as 64 pixels each (gifRowPixels), a few tenths.
//
// A slower core, or other uploads beside it, makes that longer, so the
// deadline still has the last word: Save stores nothing once it has passed,
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n = min(max(n, 1), b.size)
	release := func() { b.release(n) }

	b.mu.Lock()
	if b.waiters.Len() == 0 && b.used+n <= b.size {
		b.used += n
		b.mu.Unlock()
		return release, nil
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
		return release, nil
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
