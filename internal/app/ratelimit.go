package app

import (
	"sync"
	"time"
)

// cooldownSweepThreshold is the number of tracked keys after which expired
// entries are swept, keeping the map bounded without a background goroutine.
const cooldownSweepThreshold = 4096

// cooldownLimiter enforces a minimum interval between two actions from the same
// key. It is the flood control every imageboard needs: cheap, per-process, and
// good enough to stop a single client from spamming a board.
//
// A distributed deployment would move this to shared storage; the interface is
// kept small so that swap is contained.
type cooldownLimiter struct {
	interval time.Duration
	now      func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

func newCooldownLimiter(interval time.Duration) *cooldownLimiter {
	return &cooldownLimiter{
		interval: interval,
		now:      time.Now,
		seen:     map[string]time.Time{},
	}
}

// Allow reports whether an action is permitted for key. When it is not, the
// caller is told how long to wait; the recorded timestamp only advances on an
// allowed action, so a rejected caller cannot extend its own cooldown.
func (l *cooldownLimiter) Allow(key string) (retryAfter time.Duration, ok bool) {
	if l == nil || l.interval <= 0 {
		return 0, true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if last, tracked := l.seen[key]; tracked {
		if wait := l.interval - now.Sub(last); wait > 0 {
			return wait, false
		}
	}
	if len(l.seen) >= cooldownSweepThreshold {
		l.sweep(now)
	}
	l.seen[key] = now
	return 0, true
}

// sweep drops entries whose cooldown has elapsed. Callers must hold the mutex.
func (l *cooldownLimiter) sweep(now time.Time) {
	for key, last := range l.seen {
		if now.Sub(last) >= l.interval {
			delete(l.seen, key)
		}
	}
}
