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

// cooldown pairs a limiter with the key an action is counted against.
type cooldown struct {
	limiter *cooldownLimiter
	key     string
}

// allowAll reports whether an action is permitted, applying several cooldowns
// as one decision. When it is not, the caller is told how long to wait: the
// longest of the outstanding waits.
//
// A cooldown starts on every limiter only once all of them allow the action. A
// request one cooldown refuses must not consume another, or a reply carrying an
// image would be charged for both and the two limits would compound into a wait
// neither of them describes. Nothing is recorded for a refused action either, so
// a rejected caller cannot extend its own cooldown by retrying.
//
// The check and the record are not atomic across limiters. Two simultaneous
// requests for the same key can therefore both be admitted, which for flood
// control is a much better trade than the lock ordering an atomic version
// would need between limiters that are not always applied together.
func allowAll(cooldowns ...cooldown) (retryAfter time.Duration, ok bool) {
	var longest time.Duration
	for _, c := range cooldowns {
		if wait := c.limiter.peek(c.key); wait > longest {
			longest = wait
		}
	}
	if longest > 0 {
		return longest, false
	}
	for _, c := range cooldowns {
		c.limiter.record(c.key)
	}
	return 0, true
}

// peek reports how long key must wait, recording nothing.
func (l *cooldownLimiter) peek(key string) time.Duration {
	if l == nil || l.interval <= 0 {
		return 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	last, tracked := l.seen[key]
	if !tracked {
		return 0
	}
	return max(l.interval-l.now().Sub(last), 0)
}

// record starts key's cooldown.
func (l *cooldownLimiter) record(key string) {
	if l == nil || l.interval <= 0 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if len(l.seen) >= cooldownSweepThreshold {
		l.sweep(now)
	}
	l.seen[key] = now
}

// sweep drops entries whose cooldown has elapsed. Callers must hold the mutex.
func (l *cooldownLimiter) sweep(now time.Time) {
	for key, last := range l.seen {
		if now.Sub(last) >= l.interval {
			delete(l.seen, key)
		}
	}
}
