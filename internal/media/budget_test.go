package media

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// narrowUploads replaces the upload budget with one of n bytes for the
// length of a test.
func narrowUploads(t *testing.T, n int64) *budget {
	t.Helper()
	old := uploads
	uploads = newBudget(n)
	t.Cleanup(func() { uploads = old })
	return uploads
}

// A wait for room ends processingAllowance before the context's deadline,
// with ErrBusy, so that what is left of it is the time the work needs. The
// queue is left as it was: whoever comes next is served.
func TestWaitingForRoomEndsInTimeForTheWork(t *testing.T) {
	upload := encodePNG(t, 32, 32)
	b := narrowBudget(t, 1)
	release, err := b.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("take the whole budget: %v", err)
	}

	const waited = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), processingAllowance+waited)
	defer cancel()
	start := time.Now()
	_, err = Process(ctx, upload)
	took := time.Since(start)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Process error = %v, want ErrBusy", err)
	}
	if took < waited/2 || took > processingAllowance/2 {
		t.Errorf("Process gave up after %v; want about %v, the deadline less processingAllowance", took, waited)
	}

	release()
	if _, err := Process(context.Background(), upload); err != nil {
		t.Fatalf("Process once the budget was free: %v", err)
	}
	if n := b.held(); n != 0 {
		t.Errorf("%d bytes of the budget still held", n)
	}
}

// Room that is there is taken however near the deadline: only waiting is cut
// short. A post whose body was slow to arrive is not turned away from an idle
// server.
func TestRoomThatIsThereIsTakenWhateverTheDeadline(t *testing.T) {
	narrowBudget(t, processingBudget)
	ctx, cancel := context.WithTimeout(context.Background(), processingAllowance/2)
	defer cancel()
	if _, err := Process(ctx, encodePNG(t, 32, 32)); err != nil {
		t.Fatalf("Process with the budget free and %v to go: %v", processingAllowance/2, err)
	}
}

// ReserveUpload shares out the upload budget the same way: in order, with a
// request over the whole of it waiting for all of it, and a wait that ends
// in time for the work.
func TestUploadsReserveTheirBytes(t *testing.T) {
	b := narrowUploads(t, 100)

	first, err := ReserveUpload(context.Background(), 60)
	if err != nil {
		t.Fatalf("reserve 60 of 100: %v", err)
	}
	if n := b.held(); n != 60 {
		t.Fatalf("held %d, want 60", n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), processingAllowance+100*time.Millisecond)
	defer cancel()
	if _, err := ReserveUpload(ctx, 50); !errors.Is(err, ErrBusy) {
		t.Fatalf("reserve 50 more: %v, want ErrBusy", err)
	}

	// Over the whole budget: it waits for all of it, and gets it.
	got := make(chan func(), 1)
	go func() {
		release, err := ReserveUpload(context.Background(), 1<<40)
		if err != nil {
			t.Errorf("reserve more than the budget: %v", err)
		}
		got <- release
	}()
	select {
	case <-got:
		t.Fatal("a reservation of the whole budget was granted while 60 of it was held")
	case <-time.After(50 * time.Millisecond):
	}
	first()
	select {
	case release := <-got:
		if n := b.held(); n != 100 {
			t.Errorf("held %d, want the whole budget", n)
		}
		release()
	case <-time.After(5 * time.Second):
		t.Fatal("the reservation of the whole budget was never granted")
	}
	if n := b.held(); n != 0 {
		t.Errorf("held %d after every reservation was given back, want 0", n)
	}
}

// Save holds the processing budget until both renditions are stored: until
// then it holds them, and a budget given back before the writes would let in
// more than it was sized for.
func TestSaveHoldsItsShareUntilItIsStored(t *testing.T) {
	upload := encodePNG(t, 64, 64)
	in, err := inspect(upload)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	b := narrowBudget(t, processingBudget)
	local, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	store := &heldStore{Store: local, entered: make(chan string), proceed: make(chan struct{})}

	done := make(chan error, 1)
	go func() {
		_, err := Save(context.Background(), store, "general", upload, time.Now())
		done <- err
	}()
	for _, prefix := range []string{OriginalPrefix, ThumbPrefix} {
		select {
		case key := <-store.entered:
			if !strings.HasPrefix(key, prefix+"/") {
				t.Fatalf("wrote %q, want a key under %s/", key, prefix)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Save never wrote")
		}
		if n := b.held(); n != in.cost {
			t.Errorf("writing under %s/: %d bytes of the budget held, want the upload's %d", prefix, n, in.cost)
		}
		store.proceed <- struct{}{}
	}
	if err := <-done; err != nil {
		t.Fatalf("Save: %v", err)
	}
	if n := b.held(); n != 0 {
		t.Errorf("%d bytes of the budget held after Save returned, want 0", n)
	}
}

// heldStore stops each Put until the test lets it go on.
type heldStore struct {
	Store
	entered chan string
	proceed chan struct{}
}

func (h *heldStore) Put(ctx context.Context, key, contentType string, r io.Reader) error {
	h.entered <- key
	<-h.proceed
	return h.Store.Put(ctx, key, contentType, r)
}

// The budget's waiters all leave the queue whichever way their wait ends, so
// a context that ends, a deadline that comes too near and a grant that races
// either leave nothing behind (TestProcessWaitsForItsShareAndGivesUpWithItsContext
// covers a cancelled wait). Here many waits end together.
func TestEveryEndedWaitLeavesTheQueue(t *testing.T) {
	b := newBudget(10)
	hold, err := b.acquire(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), processingAllowance+20*time.Millisecond)
	defer cancel()
	errs := make(chan error, 20)
	for i := 0; i < cap(errs); i++ {
		go func() {
			release, err := b.acquire(ctx, 3)
			if err == nil {
				release()
			}
			errs <- err
		}()
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; !errors.Is(err, ErrBusy) {
			t.Errorf("wait %d ended with %v, want ErrBusy", i, err)
		}
	}
	hold()
	b.mu.Lock()
	queued := b.waiters.Len()
	b.mu.Unlock()
	if queued != 0 || b.held() != 0 {
		t.Errorf("after every wait ended: %d queued, %d held; want none", queued, b.held())
	}
	if release, err := b.acquire(context.Background(), 10); err != nil {
		t.Errorf("the whole budget after every wait ended: %v", err)
	} else {
		release()
	}
}
