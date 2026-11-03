package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/gif"
	"image/png"
	"math"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/media/jpegenc"
)

// Processing stops once its context is done at five places: the decode reads
// through a ctxReader, each format checks once its decode is done, the encode
// writes into a storedBuffer — and the JPEG encoder stops working, not only
// writing, once a write is refused — playGIF checks before each frame, and
// processHeld checks before the thumbnail, or sanitizeGIF before a GIF's,
// which it makes itself. These tests pin each of them on its own, by ending
// the context at the moment that one asks (askedContext) and holding the work
// to stopping right there. None depends on timing: where the context ends is
// fixed by the code's own calls, not by how fast the machine is.

// askedContext is a context that ends — Err returns context.Canceled and
// Done is closed — the moment it is asked, through Err, for the after-th time
// from inside the function named endsAt, or from the start when endsAt is
// empty. It records which function asked each time, so a test can tell
// which check saw it end and whether any work asked again after that.
type askedContext struct {
	endsAt string
	after  int
	// passes, if set, has the context end just after that ask instead, which
	// it answers as if the context had not ended.
	passes bool
	// allocs, if set, has the context note in allocated how many bytes the
	// process had allocated in all (runtime.MemStats.TotalAlloc) as it ends.
	allocs    bool
	allocated uint64

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
			if c.allocs {
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				c.allocated = m.TotalAlloc
			}
			if c.passes {
				c.asks = append(c.asks, contextAsk{fn: fn})
				return nil
			}
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

// The functions that ask a processing context whether it is done. Each
// format's own function (sanitizeJPEG, sanitizePNG, sanitizeGIF,
// sanitizeWebP) asks too, once its decode is done, and sanitizeGIF again
// before the thumbnail it makes of a GIF's first picture.
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

// TestNothingMoreIsDoneOnceTheContextEndsWithTheDecode holds that a context
// that ends once a decode has read all it reads stops the work there, before
// anything is encoded: each format's check after its decode, on its own.
// image/jpeg reads a small progressive file whole before its passes over the
// blocks, and x/image a lossy WebP's partitions before decoding them, so for
// those the decode's reads cannot see a context that ends during its costly
// part; this is the first place that can.
func TestNothingMoreIsDoneOnceTheContextEndsWithTheDecode(t *testing.T) {
	for _, c := range []struct {
		name, asks string
		data       []byte
	}{
		{"JPEG", "sanitizeJPEG", photoJPEG(t)},
		{"PNG", "sanitizePNG", photoPNG(t)},
		{"GIF", "sanitizeGIF", flatGIF(t, 64, 64, 4)},
		{"lossy WebP", "sanitizeWebP", readTestdata(t, "lossy.webp")},
		{"lossless WebP", "sanitizeWebP", readTestdata(t, "sample.webp")},
	} {
		whole, _ := sanitizeLive(t, c.name, c.data).asked(askRead)
		ctx := newAskedContext(c.asks, 1)
		sanitizeUnder(t, c.name, ctx, c.data)
		if n, late := ctx.asked(askRead); n != whole || late != 0 {
			t.Errorf("%s: the decode read %d times (%d after the context ended), want all %d of a whole decode, before it", c.name, n, late, whole)
		}
		if n, _ := ctx.asked(c.asks); n != 1 {
			t.Errorf("%s: %s asked %d times, want once, after the decode", c.name, c.asks, n)
		}
		if n, _ := ctx.asked(askWrite); n != 0 {
			t.Errorf("%s: the encode wrote %d times after the context ended with the decode", c.name, n)
		}
	}
}

// TestEncodingStopsWhenTheContextEndsPartway holds that an encode stops at
// the first write after its context ends, part way through the stored file,
// the decode having finished: storedBuffer's check, on its own. For a JPEG it
// also holds that the encode's work stops, not only its writes: jpegenc
// passes its output on 4 KiB at a time through a buffer that stops passing
// anything on once a write fails, so a count of writes cannot see whether it
// went on transforming and coding the rest of the picture, which on a
// 25-megapixel one is about a second of CPU. A picture whose pixels stop
// after its first rows, though it claims thousands more, can: an encoder that
// read on past the write it was refused would panic.
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

	const w, rows, h = 512, 64, 4096
	noise := rand.New(rand.NewSource(5))
	have := image.NewYCbCr(image.Rect(0, 0, w, rows), image.YCbCrSubsampleRatio420)
	for _, plane := range [][]byte{have.Y, have.Cb, have.Cr} {
		noise.Read(plane)
	}
	var coded bytes.Buffer
	if err := jpegenc.Encode(&coded, have.SubImage(image.Rect(0, 0, w, rows-16)), nil); err != nil || coded.Len() < (at+1)<<12 {
		t.Fatalf("the rows there are code to %d bytes (%v); write %d would not fail among them", coded.Len(), err, at)
	}
	short := *have
	short.Rect = image.Rect(0, 0, w, h)
	ctx := newAskedContext(askWrite, at)
	func() {
		defer func() {
			if v := recover(); v != nil {
				t.Errorf("the JPEG encode read on past the write it was refused: %v", v)
			}
		}()
		err := jpegenc.Encode(newStoredBuffer(ctx, 0, math.MaxInt64), &short, nil)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("jpegenc.Encode = %v, want context.Canceled", err)
		}
	}()
	if n, late := ctx.asked(askWrite); n != at || late != 0 {
		t.Errorf("the JPEG encode wrote %d times (%d after the context ended), want %d", n, late, at)
	}
}

// TestProcessStopsWhenTheContextIsAlreadyDone holds that Process does no work
// and returns the context's error, not a decode or format error, when its
// context is cancelled or past its deadline before it is called, and leaves
// nothing held in the processing budget. The wait for the budget is what
// refuses it, before sanitize is reached;
// TestSanitizeStopsAtOnceWhenTheContextIsAlreadyDone goes past that.
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

// TestAGIFIsNotThumbnailedOnceTheContextHasEnded holds that sanitizeGIF asks
// after its context twice, once the frames are decoded and again just before
// it thumbnails the first picture, and that a context that has ended by
// either ask stops it there, before the thumbnail and before any frame is
// played: sanitizeGIF's checks, each on its own. A GIF's thumbnail is made in
// sanitizeGIF, before its frames are played, so processHeld's check comes too
// late for it, and the scale is one call that cannot be cut short, at the
// size of the canvas: ~0.6 s of CPU on one of 25 megapixels. It allocates by
// the canvas too, megabytes for this one, which is how the test sees that it
// did not run once the context had ended.
func TestAGIFIsNotThumbnailedOnceTheContextHasEnded(t *testing.T) {
	const asks, size = "sanitizeGIF", 1000
	upload := flatGIF(t, size, size, 2)
	live := sanitizeLive(t, "GIF", upload)
	if n, _ := live.asked(asks); n != 2 {
		t.Fatalf("%s asked %d times in a whole run, want twice: once the frames are decoded and before the thumbnail", asks, n)
	}
	whole, _ := live.asked(askRead)

	// The thumbnail of the first picture, on its own, allocates far more
	// than is allowed after the context ends.
	const allowed = 256 << 10
	g, err := gif.DecodeAll(bytes.NewReader(upload))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var heads []gifFrameHead
	if _, err := walkGIF(upload, math.MaxInt64, func(h gifFrameHead) { heads = append(heads, h) }); err != nil {
		t.Fatalf("walk: %v", err)
	}
	first := newFirstGIFPicture(g.Image[0], heads[0], image.Rect(0, 0, size, size))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := makeThumbnail(first, FormatGIF); err != nil {
		t.Fatalf("thumbnail: %v", err)
	}
	runtime.ReadMemStats(&after)
	if made := after.TotalAlloc - before.TotalAlloc; made < 4*allowed {
		t.Fatalf("the thumbnail allocates %d bytes, too few to tell from the %d allowed", made, allowed)
	}

	for _, c := range []struct {
		when   string
		passes bool // whether the first ask is told the context is live
		asked  int  // how many times sanitizeGIF asks
	}{
		// The first ask, once the frames are decoded, sees it end.
		{"with the decode", false, 1},
		// The first ask is answered live and the context ends just after,
		// while the frames are walked: the ask before the thumbnail sees it.
		{"after the decode", true, 2},
	} {
		ctx := newAskedContext(asks, 1)
		ctx.passes, ctx.allocs = c.passes, true
		sanitizeUnder(t, "GIF ended "+c.when, ctx, upload)
		var end runtime.MemStats
		runtime.ReadMemStats(&end)
		if n, late := ctx.asked(asks); n != c.asked || late != 0 {
			t.Errorf("ended %s: %s asked %d times (%d after the context was seen to end), want %d, and none after", c.when, asks, n, late, c.asked)
		}
		if n, late := ctx.asked(askRead); n != whole || late != 0 {
			t.Errorf("ended %s: the decode read %d times (%d after the context ended), want all %d of a whole decode, before it", c.when, n, late, whole)
		}
		for _, fn := range []string{askFrame, askWrite} {
			if n, _ := ctx.asked(fn); n != 0 {
				t.Errorf("ended %s: %s asked %d times; nothing should have been played", c.when, fn, n)
			}
		}
		if made := end.TotalAlloc - ctx.allocated; made > allowed {
			t.Errorf("ended %s: %d bytes allocated after the context ended, more than the %d allowed: the thumbnail was made", c.when, made, allowed)
		}
	}
}
