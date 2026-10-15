package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/gif"
	"image/png"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Processing stops once its context is done at four places: the decode reads
// through a ctxReader, the encode writes into a storedBuffer, playGIF checks
// before each frame, and processHeld checks before the thumbnail. These tests
// pin each of them on its own, by ending the context at the moment that one
// asks (askedContext) and holding the work to stopping right there. None
// depends on timing: where the context ends is fixed by the code's own calls,
// not by how fast the machine is.

// askedContext is a context that ends — Err returns context.Canceled and
// Done is closed — the moment it is asked, through Err, for the after-th time
// from inside the function named endsAt, or from the start when endsAt is
// empty. It records which function asked each time, so a test can tell
// which check saw it end and whether any work asked again after that.
type askedContext struct {
	endsAt string
	after  int

	mu     sync.Mutex
	asks   []contextAsk
	seen   int
	ended  bool
	sawEnd bool
	done   chan struct{}
}

// contextAsk is one call of an askedContext's Err: the function that made
// it, and whether it came after the call that first returned an error.
type contextAsk struct {
	fn   string
	late bool
}

func newAskedContext(endsAt string, after int) *askedContext {
	c := &askedContext{endsAt: endsAt, after: after, done: make(chan struct{})}
	if endsAt == "" {
		c.ended = true
		close(c.done)
	}
	return c
}

func (c *askedContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *askedContext) Value(any) any               { return nil }
func (c *askedContext) Done() <-chan struct{}       { return c.done }

func (c *askedContext) Err() error {
	fn := "?"
	if pc, _, _, ok := runtime.Caller(1); ok {
		fn = strings.TrimPrefix(runtime.FuncForPC(pc).Name(), "github.com/yuvraj-agarwal/educhan/internal/media.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn == c.endsAt && !c.ended {
		if c.seen++; c.seen >= c.after {
			c.ended = true
			close(c.done)
		}
	}
	c.asks = append(c.asks, contextAsk{fn: fn, late: c.sawEnd})
	if c.ended {
		c.sawEnd = true
		return context.Canceled
	}
	return nil
}

// asked is how many times fn asked, and how many of those came after the
// first ask that was told the context had ended.
func (c *askedContext) asked(fn string) (n, late int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.asks {
		if a.fn == fn {
			n++
			if a.late {
				late++
			}
		}
	}
	return n, late
}

// first is the function that asked first.
func (c *askedContext) first() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.asks) == 0 {
		return ""
	}
	return c.asks[0].fn
}

// The functions that ask a processing context whether it is done.
const (
	askRead   = "ctxReader.Read"
	askWrite  = "(*storedBuffer).Write"
	askFrame  = "playGIF"
	askThumb  = "processHeld"
	askBudget = "(*budget).acquire"
)

// photoPNG and photoJPEG are a 320×240 photo-like picture (photo), whose
// decode reads its file in many pieces and whose encode writes many.
func photoPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, photo(320, 240)); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func photoJPEG(t *testing.T) []byte {
	t.Helper()
	return encodeJPEGImage(t, photo(320, 240))
}

// flatGIF is a GIF of n full-canvas frames that each redraw every pixel in a
// colour other than the one before, so each frame shows a new picture.
func flatGIF(t *testing.T, w, h, n int) []byte {
	t.Helper()
	g := &gif.GIF{LoopCount: 0, Config: image.Config{ColorModel: playPalette, Width: w, Height: h}}
	for k := 0; k < n; k++ {
		px := uint8(redPx)
		if k%2 == 1 {
			px = greenPx
		}
		m := image.NewPaletted(image.Rect(0, 0, w, h), playPalette)
		for i := range m.Pix {
			m.Pix[i] = px
		}
		g.Image = append(g.Image, m)
		g.Delay = append(g.Delay, 5)
		g.Disposal = append(g.Disposal, keepFrame)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// sanitizeLive runs sanitize under an askedContext that never ends and
// returns it, to count how often each check asks in a whole run. A live
// context changes nothing, so it must store what sanitize under
// context.Background stores.
func sanitizeLive(t *testing.T, name string, data []byte) *askedContext {
	t.Helper()
	in, err := inspect(data)
	if err != nil {
		t.Fatalf("%s: inspect: %v", name, err)
	}
	want, err := sanitize(context.Background(), in, data)
	if err != nil {
		t.Fatalf("%s: sanitize: %v", name, err)
	}
	live := newAskedContext("never", 1)
	got, err := sanitize(live, in, data)
	if err != nil {
		t.Fatalf("%s: sanitize under a live context: %v", name, err)
	}
	if !bytes.Equal(got.data, want.data) {
		t.Fatalf("%s: a live context changed the stored bytes", name)
	}
	return live
}

// sanitizeUnder runs sanitize on data under ctx, which must stop it with
// context.Canceled.
func sanitizeUnder(t *testing.T, name string, ctx context.Context, data []byte) {
	t.Helper()
	in, err := inspect(data)
	if err != nil {
		t.Fatalf("%s: inspect: %v", name, err)
	}
	if _, err := sanitize(ctx, in, data); !errors.Is(err, context.Canceled) {
		t.Errorf("%s: sanitize = %v, want context.Canceled", name, err)
	}
}

// TestSanitizeStopsAtOnceWhenTheContextIsAlreadyDone holds that sanitize,
// given a context that is already done, stops at its first read — the
// decode's, through ctxReader — and returns the context's error, not
// ErrCorruptImage, for every format, without encoding anything. Process
// refuses such a context while it waits for the budget, before sanitize
// (TestProcessStopsWhenTheContextIsAlreadyDone), so this calls sanitize
// itself.
func TestSanitizeStopsAtOnceWhenTheContextIsAlreadyDone(t *testing.T) {
	uploads := map[string][]byte{
		"JPEG": photoJPEG(t),
		"PNG":  photoPNG(t),
		"GIF":  flatGIF(t, 64, 64, 4),
		"WebP": readTestdata(t, "sample.webp"),
	}
	for name, data := range uploads {
		ctx := newAskedContext("", 0)
		sanitizeUnder(t, name, ctx, data)
		if first := ctx.first(); first != askRead {
			t.Errorf("%s: %s asked first, want the decode's read (%s)", name, first, askRead)
		}
		if n, _ := ctx.asked(askRead); n != 1 {
			t.Errorf("%s: the decode read %d times through a done context, want 1", name, n)
		}
		if n, _ := ctx.asked(askWrite); n != 0 {
			t.Errorf("%s: the encode wrote %d times, though the decode should have stopped", name, n)
		}
	}
}

// TestDecodingStopsWhenTheContextEndsPartway holds that a decode stops at
// the first read after its context ends, part way through the file, for a
// PNG and a JPEG whose decode reads in many pieces, and that nothing is
// encoded: ctxReader's check, on its own.
func TestDecodingStopsWhenTheContextEndsPartway(t *testing.T) {
	const at = 3
	for name, data := range map[string][]byte{"PNG": photoPNG(t), "JPEG": photoJPEG(t)} {
		whole, _ := sanitizeLive(t, name, data).asked(askRead)
		if whole <= at {
			t.Fatalf("%s: a whole decode reads %d times; the fixture must read more than %d for the context to end part way", name, whole, at)
		}
		ctx := newAskedContext(askRead, at)
		sanitizeUnder(t, name, ctx, data)
		if n, late := ctx.asked(askRead); n != at || late != 0 {
			t.Errorf("%s: read %d times (%d after the context ended), want %d of the %d a whole decode makes, and none after", name, n, late, at, whole)
		}
		if n, _ := ctx.asked(askWrite); n != 0 {
			t.Errorf("%s: the encode wrote %d times after the decode was cut short", name, n)
		}
	}
}

// TestEncodingStopsWhenTheContextEndsPartway holds that an encode stops at
// the first write after its context ends, part way through the stored file,
// the decode having finished: storedBuffer's check, on its own.
func TestEncodingStopsWhenTheContextEndsPartway(t *testing.T) {
	const at = 2
	for name, data := range map[string][]byte{"PNG": photoPNG(t), "JPEG": photoJPEG(t)} {
		whole, _ := sanitizeLive(t, name, data).asked(askWrite)
		if whole <= at {
			t.Fatalf("%s: a whole encode writes %d times; the fixture must write more than %d for the context to end part way", name, whole, at)
		}
		ctx := newAskedContext(askWrite, at)
		sanitizeUnder(t, name, ctx, data)
		if n, late := ctx.asked(askWrite); n != at || late != 0 {
			t.Errorf("%s: wrote %d times (%d after the context ended), want %d of the %d a whole encode makes, and none after", name, n, late, at, whole)
		}
	}
}

// TestProcessStopsWhenTheContextIsAlreadyDone holds that Process does no work
// and returns the context's error, not a decode or format error, when its
// context is cancelled or past its deadline before it is called, and leaves
// nothing held in the processing budget. The wait for the budget is what
// refuses it, before sanitize is reached; TestSanitizeStopsAtOnceWhenTheContextIsAlreadyDone
// goes past that.
//
// Unlike the other cancel tests, this one guards behaviour that predates the
// in-flight cancellation work: the budget wait ((*budget).acquire) has always
// returned the context's error before sanitize runs, so it passes without the
// ctxReader, storedBuffer and playGIF checks. It is an honest regression guard
// for the budget wait, not evidence of those checks — each of those is pinned
// by its own test above, which fails when its own check is removed.
func TestProcessStopsWhenTheContextIsAlreadyDone(t *testing.T) {
	upload := encodePNG(t, 64, 64)
	b := narrowBudget(t, processingBudget)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Process(cancelled, upload); !errors.Is(err, context.Canceled) {
		t.Errorf("Process(cancelled) = %v, want context.Canceled", err)
	}
	if n := b.held(); n != 0 {
		t.Errorf("%d bytes held after a cancelled Process, want 0", n)
	}

	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	if _, err := Process(expired, upload); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Process(expired) = %v, want context.DeadlineExceeded", err)
	}
	if n := b.held(); n != 0 {
		t.Errorf("%d bytes held after an expired Process, want 0", n)
	}

	done := newAskedContext("", 0)
	if _, err := Process(done, upload); !errors.Is(err, context.Canceled) {
		t.Errorf("Process(done) = %v, want context.Canceled", err)
	}
	if n := len(done.asks); n != 1 || done.first() != askBudget {
		t.Errorf("Process(done) asked %d times, first %s; want once, by the budget's wait (%s)", n, done.first(), askBudget)
	}
}

// TestProcessStopsWhenCancelledWhilePlayingAGIF holds that a context that
// ends while a GIF is being played stops it before the next frame, with the
// context's error, and gives the budget back: playGIF's check, on its own.
// The context ends as the third of eight frames is about to be played, so the
// rest are never played. The former attack this answers held the whole
// budget for the tens of seconds a costly play ran, with nothing to cut it
// short.
func TestProcessStopsWhenCancelledWhilePlayingAGIF(t *testing.T) {
	const frames, at = 8, 3
	upload := flatGIF(t, 64, 64, frames)
	if n, _ := sanitizeLive(t, "GIF", upload).asked(askFrame); n != frames {
		t.Fatalf("playGIF asked %d times playing %d frames, want once a frame", n, frames)
	}
	b := narrowBudget(t, processingBudget)

	ctx := newAskedContext(askFrame, at)
	if _, err := Process(ctx, upload); !errors.Is(err, context.Canceled) {
		t.Errorf("Process = %v, want context.Canceled", err)
	}
	if n, late := ctx.asked(askFrame); n != at || late != 0 {
		t.Errorf("playGIF asked %d times (%d after the context ended), want %d of %d, and none after: it played on", n, late, at, frames)
	}
	if n, late := ctx.asked(askWrite); late != 0 {
		t.Errorf("%d of %d writes came after the context ended", late, n)
	}
	if n := b.held(); n != 0 {
		t.Errorf("%d bytes held after a cancelled Process, want 0", n)
	}
}

// TestProcessSkipsTheThumbnailOnceTheContextHasEnded holds that a context
// that ends once the picture has been rewritten, before it is thumbnailed,
// stops Process there with the context's error rather than scaling a
// thumbnail nothing is waiting for, and gives the budget back: processHeld's
// check, on its own.
func TestProcessSkipsTheThumbnailOnceTheContextHasEnded(t *testing.T) {
	upload := photoPNG(t)
	b := narrowBudget(t, processingBudget)
	ctx := newAskedContext(askThumb, 1)
	if _, err := Process(ctx, upload); !errors.Is(err, context.Canceled) {
		t.Errorf("Process = %v, want context.Canceled", err)
	}
	if n, _ := ctx.asked(askThumb); n != 1 {
		t.Errorf("processHeld asked %d times, want 1", n)
	}
	if n := b.held(); n != 0 {
		t.Errorf("%d bytes held after a cancelled Process, want 0", n)
	}
}
