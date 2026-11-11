package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yuvraj-agarwal/educhan/internal/media"
)

// An image is shown under "image" and its stored format's extension. The name
// comes from the path the server chose, so nothing a poster sent can reach it.
func TestAttachmentName(t *testing.T) {
	cases := map[string]string{
		"general/2026/10/6f1c0e.jpg":   "image.jpg",
		"general/2026/10/6f1c0e.png":   "image.png",
		"u-iitb/2026/10/6f1c0e.webp":   "image.webp",
		"a.b/2026/10/6f1c0e.gif":       "image.gif",
		"general/2026/10/no-extension": "image",
	}
	for stored, want := range cases {
		if got := attachmentName(stored); got != want {
			t.Errorf("attachmentName(%q) = %q, want %q", stored, got, want)
		}
	}
}

// The upload shares are keyed on client addresses (clientIP), so they hold
// one only while that client's image posts hold room: an entry goes the
// moment the last of them gives its share back, with no sweep to wait for,
// and a share given back twice gives back nothing more.
func TestUploadSharesForgetAClientOnceItHoldsNothing(t *testing.T) {
	var s uploadShares
	const client = "203.0.113.7"
	first, ok := s.take(client, 3<<20)
	if !ok {
		t.Fatal("the first share was refused")
	}
	second, ok := s.take(client, 2<<20)
	if !ok {
		t.Fatal("the second share was refused")
	}
	if _, ok := s.take(client, clientUploadShare); ok {
		t.Fatal("a share past clientUploadShare was taken")
	}
	first()
	first()
	if got := s.of(client); got != 2<<20 {
		t.Errorf("after one of two shares was given back (twice), the client holds %d bytes, want %d", got, 2<<20)
	}
	second()
	if n := len(s.held); n != 0 {
		t.Errorf("the shares hold %d clients once every share is given back, want none: %v", n, s.held)
	}
}

// What the posts waiting for room hold between them is capped (waitingRoom),
// so it has to go back down as each stops waiting: kept, it would only grow,
// until every post that holds room and finds none free was turned away busy at
// once for the life of the process. A client waits once at a time, and is free
// to wait again once it no longer does; the function that says so says it
// once, however often it is called.
func TestUploadSharesGiveBackWhatTheWaitingHeld(t *testing.T) {
	var s uploadShares
	const (
		client = "203.0.113.7"
		other  = "203.0.113.8"
		limit  = 10 << 20
	)
	waitHeld := func() int64 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.waitHeld
	}

	first, err := s.wait(client, limit, limit)
	if err != nil {
		t.Fatalf("a wait holding the whole waiting room was refused: %v", err)
	}
	if _, err := s.wait(client, 0, limit); !errors.Is(err, errUploadShare) {
		t.Errorf("a client's second wait at once: %v, want %v", err, errUploadShare)
	}
	if _, err := s.wait(other, 1, limit); !errors.Is(err, media.ErrBusy) {
		t.Errorf("a wait holding a byte past the waiting room: %v, want %v", err, media.ErrBusy)
	}
	nothing, err := s.wait(other, 0, limit)
	if err != nil {
		t.Fatalf("a wait holding nothing, beside one holding the whole waiting room, was refused: %v", err)
	}
	nothing()
	if got := waitHeld(); got != limit {
		t.Errorf("the posts waiting hold %d bytes once one holding nothing stops, want %d", got, limit)
	}

	first()
	first()
	if got := waitHeld(); got != 0 {
		t.Errorf("the posts waiting hold %d bytes once the only one holding any has stopped (said twice), want 0", got)
	}
	if n := len(s.waiting); n != 0 {
		t.Errorf("%d clients wait once none does, want none: %v", n, s.waiting)
	}

	// The client, and the room, are free again: the same wait is taken, as
	// often as the room it held was given back.
	for i := 0; i < 3; i++ {
		again, err := s.wait(client, limit, limit)
		if err != nil {
			t.Fatalf("wait %d, holding what the first held, was refused: %v", i+2, err)
		}
		again()
	}
	if got := waitHeld(); got != 0 {
		t.Errorf("the posts waiting hold %d bytes once each has stopped, want 0", got)
	}
}

// A post that holds room and waits for more does not always get it: its
// request ends while it waits (the poster went away), or its deadline is too
// near to leave time to process the image once read, and it is refused busy
// without waiting (media.ErrBusy). Either way it stops being one of the posts
// waiting, and gives back what it held of the waiting room and its share, as
// one that got its room does (waitForUpload). On a busy server many waits end
// so: kept, what they held would add up until every post that holds room and
// finds none free was turned away busy at once, for the life of the process,
// and a client whose wait was kept could never wait again. Each post here
// holds the whole waiting room, so the next could not wait at all had one
// before it kept any.
func TestUploadWaitsThatGiveUpGiveBackWhatTheyHeld(t *testing.T) {
	a := &App{cfg: Config{MaxUploadBytes: defaultMaxUploadBytes}}
	r := httptest.NewRequest(http.MethodPost, "/api/boards/general/threads", nil)
	r.RemoteAddr = "203.0.113.7:40000"
	held := a.waitingRoom()
	const n = 1 << 20
	state := func() (waitHeld int64, waiting, shares int) {
		a.uploadShares.mu.Lock()
		defer a.uploadShares.mu.Unlock()
		return a.uploadShares.waitHeld, len(a.uploadShares.waiting), len(a.uploadShares.held)
	}

	awaitBudgetFree(t, media.UploadBudget)
	full, ok := media.TryReserveUpload(media.UploadBudget)
	if !ok {
		t.Fatal("hold the whole of a free upload budget: refused")
	}
	var once sync.Once
	free := func() { once.Do(full) }
	t.Cleanup(free)

	// With no deadline, the post waits until its request ends, which it does
	// once the post is seen waiting, holding the whole waiting room.
	goneAway := func() error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, _, err := a.reserveUpload(ctx, r, held, n, 0)
			done <- err
		}()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			select {
			case err := <-done:
				t.Fatalf("the post stopped waiting before its request ended: %v", err)
			default:
			}
			if waitHeld, waiting, _ := state(); waitHeld == held && waiting == 1 {
				break
			}
			if time.Now().After(deadline) {
				waitHeld, waiting, _ := state()
				t.Fatalf("%d posts wait holding %d bytes, want 1 holding the waiting room, %d", waiting, waitHeld, held)
			}
		}
		cancel()
		return <-done
	}
	// A deadline 5 s away is nearer than the time an upload stops waiting
	// before its deadline, to be processed in (media's processingAllowance,
	// 15 s), so the post is refused busy as it would start to wait.
	tooLate := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, err := a.reserveUpload(ctx, r, held, n, 0)
		return err
	}

	for _, wait := range []struct {
		ended string
		ask   func() error
		want  error
	}{
		{"its request ended while it waited", goneAway, context.Canceled},
		{"its deadline was too near to wait", tooLate, media.ErrBusy},
		{"the next one's request ended while it waited", goneAway, context.Canceled},
	} {
		if err := wait.ask(); !errors.Is(err, wait.want) {
			t.Fatalf("a post holding the waiting room, with the budget full: %v once %s, want %v", err, wait.ended, wait.want)
		}
		if waitHeld, waiting, shares := state(); waitHeld != 0 || waiting != 0 || shares != 0 {
			t.Fatalf("once %s, %d posts wait holding %d bytes, and %d clients hold shares, want none, 0 and none", wait.ended, waiting, waitHeld, shares)
		}
	}
	// Nor is any of the budget held, or anyone left in its queue.
	free()
	awaitBudgetFree(t, media.UploadBudget)
}
