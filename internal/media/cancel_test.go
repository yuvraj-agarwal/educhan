package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/gif"
	"testing"
	"time"
)

// fullChangeGIF is a GIF of n full-canvas frames that each redraw every pixel
// in a colour other than the one before, so playing it touches every pixel of
// every frame: enough work on one core that a cancellation arriving part way
// through has frames left to play when it does.
func fullChangeGIF(t *testing.T, w, h, n int) []byte {
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

// TestProcessStopsWhenTheContextIsAlreadyDone holds that Process does no work
// and returns the context's error, not a decode or format error, when its
// context is cancelled or past its deadline before it is called, and leaves
// nothing held in the processing budget.
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
}

// TestProcessStopsWhenCancelledWhilePlayingAGIF holds that a cancellation that
// arrives while a long GIF is being played stops it promptly — well before it
// would have finished — with the context's error, and gives the budget back.
// The former attack this answers held the whole budget for the tens of
// seconds the work ran, with nothing to cut it short.
func TestProcessStopsWhenCancelledWhilePlayingAGIF(t *testing.T) {
	upload := fullChangeGIF(t, 2000, 2000, 16)
	if _, err := inspect(upload); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	b := narrowBudget(t, processingBudget)

	// Time an uncancelled run, so the test's bound is relative to this
	// machine rather than a fixed wall-clock guess.
	start := time.Now()
	want, err := Process(context.Background(), upload)
	if err != nil {
		t.Fatalf("uncancelled Process: %v", err)
	}
	full := time.Since(start)
	if n := b.held(); n != 0 {
		t.Fatalf("%d bytes held after an uncancelled Process, want 0", n)
	}
	if full < 50*time.Millisecond {
		t.Skipf("GIF played in %v, too fast to cancel mid-flight reliably", full)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cstart := time.Now()
	go func() {
		_, e := Process(ctx, upload)
		done <- e
	}()
	// Cancel well before the work would finish.
	time.Sleep(full / 10)
	cancel()
	select {
	case e := <-done:
		took := time.Since(cstart)
		if !errors.Is(e, context.Canceled) {
			t.Errorf("cancelled Process = %v, want context.Canceled", e)
		}
		if took > full*3/4 {
			t.Errorf("cancelled Process took %v; the whole run is %v, so it did not stop promptly", took, full)
		}
	case <-time.After(full + 5*time.Second):
		t.Fatal("cancelled Process did not return")
	}
	if n := b.held(); n != 0 {
		t.Errorf("%d bytes held after a cancelled Process, want 0", n)
	}

	// The uncancelled run's bytes are unchanged by the context plumbing: a
	// second uncancelled run matches it.
	again, err := Process(context.Background(), upload)
	if err != nil {
		t.Fatalf("second uncancelled Process: %v", err)
	}
	if !bytes.Equal(again.Data, want.Data) {
		t.Error("an uncancelled Process stored different bytes than another; the context plumbing changed the output")
	}
}
