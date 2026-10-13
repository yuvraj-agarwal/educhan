package media

import (
	"container/list"
	"context"
	"sync"
)

// budget shares out processingBudget among the uploads being decoded and
// re-encoded at once, each taking its estimated cost (processingCost) for as
// long as it runs. Uploads are served in the order they asked: a large one at
// the head of the queue waits for room and smaller ones queue behind it rather
// than slipping past, so it cannot be starved. It is a weighted semaphore;
// golang.org/x/sync/semaphore is the same idea, but not a dependency here.
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

// processing is the budget Process takes from. It is a variable so tests can
// narrow it; nothing else changes it.
var processing = newBudget(processingBudget)

// acquireProcessing waits until cost fits in the processing budget and returns
// the function that gives it back, or the context's error if the context ends
// first. A cost over the whole budget is refused by Inspect before this is
// reached; here it is capped at the budget, so it waits for the budget to be
// empty rather than forever.
func acquireProcessing(ctx context.Context, cost int64) (func(), error) {
	return processing.acquire(ctx, cost)
}

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
	w := &budgetWaiter{n: n, ready: make(chan struct{})}
	elem := b.waiters.PushBack(w)
	b.mu.Unlock()

	select {
	case <-w.ready:
		return release, nil
	case <-ctx.Done():
		b.mu.Lock()
		select {
		case <-w.ready:
			// Granted as the context ended: give it straight back.
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
		return nil, ctx.Err()
	}
}

func (b *budget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	if b.used < 0 {
		b.mu.Unlock()
		panic("media: processing budget released more than it held")
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
