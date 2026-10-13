package app

import (
	"sync"
	"time"
)

// cooldownSweepThreshold is the number of tracked keys after which recording a
// new one sweeps expired entries first, keeping the map bounded under a burst
// between two periodic sweeps.
const cooldownSweepThreshold = 4096

// cooldownSweepInterval is how often sweepCooldowns drops every entry whose
// cooldown has passed. The keys are client addresses, so this is the bound on
// how long the server remembers one: an address is held in memory no longer
// than the longest cooldown that applies to it plus one minute, counted from
// its last action that started a cooldown. A quiet server forgets as surely as
// a busy one; the address is never written anywhere else.
const cooldownSweepInterval = time.Minute

// cooldownLimiter enforces a minimum interval between two actions from the same
// key. It is the flood control every imageboard needs: cheap, per-process, and
// good enough to stop a single client from spamming a board.
//
// Its keys carry client addresses, so it keeps an entry only while the entry
// can still refuse something: sweepCooldowns drops it within
// cooldownSweepInterval of its cooldown running out.
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

// sweepExpired drops every entry whose cooldown has elapsed by the limiter's
// clock.
func (l *cooldownLimiter) sweepExpired() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(l.now())
}

// sweepCooldowns drops the expired entries of every limiter once each interval,
// on a goroutine of its own, until stop is called. stop returns once that
// goroutine has finished, and calling it again does nothing, so an App closed
// twice, or a test that stops it early, leaves nothing running.
func sweepCooldowns(every time.Duration, limiters ...*cooldownLimiter) (stop func()) {
	ticker := time.NewTicker(every)
	quit := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				for _, l := range limiters {
					l.sweepExpired()
				}
			case <-quit:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(quit) })
		<-finished
	}
}
