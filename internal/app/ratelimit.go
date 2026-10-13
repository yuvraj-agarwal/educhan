package app

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// cooldownSweepThreshold is the number of tracked keys after which recording a
// new one sweeps expired entries first, keeping the map bounded under a burst
// between two periodic sweeps.
const cooldownSweepThreshold = 4096

// cooldownSweepInterval is how often a cooldownSweep drops every entry whose
// cooldown has passed. The keys are client addresses, so this is the bound on
// how long the server remembers one while the process has CPU: an address is
// held in memory no longer than the longest cooldown that applies to it plus
// one minute, counted from its last action that started a cooldown, however
// quiet the server is; the refusal budget holds one for its window plus one
// minute, counted from the first refusal in it. A host that gives the process
// CPU only while it answers
// a request, which is Cloud Run's default, holds the timer back while the
// instance is idle; there the sweep runs before the instance answers its next
// request, so an idle instance keeps expired entries until then or until it is
// shut down. The address is never written anywhere else.
const cooldownSweepInterval = time.Minute

// cooldownLimiter enforces a minimum interval between two actions from the same
// key. It is the flood control every imageboard needs: cheap, per-process, and
// good enough to stop a single client from spamming a board.
//
// Its keys carry client addresses, so it keeps an entry only while the entry
// can still refuse something: a cooldownSweep drops it within
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

// refusalWindow is the period a refusalBudget counts refusals over.
const refusalWindow = time.Minute

// refusalBudget counts, per client, the posts the content rules sent back, and
// stops checking that client's posts once it has had limit of them in one
// window. A refused post costs no cooldown, so that a poster told to take a
// phone number out can post again at once; without a budget it would also cost
// the client nothing, and the rules are the most expensive thing a post does
// before it is stored. A person correcting a post is refused once or twice; a
// script is refused as often as it can send.
//
// The window is fixed rather than sliding: it starts at a client's first
// refusal and the count starts again once it has passed. That allows twice the
// limit across a window's edge, which is still a bound, and keeps one time and
// one count per client. Its keys are client addresses, swept with the
// cooldowns once their window has passed.
type refusalBudget struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	seen map[string]refusalCount
}

type refusalCount struct {
	since time.Time
	n     int
}

// newRefusalBudget allows limit refusals per client per window; a limit of
// zero or less turns the budget off.
func newRefusalBudget(limit int, window time.Duration) *refusalBudget {
	return &refusalBudget{limit: limit, window: window, now: time.Now, seen: map[string]refusalCount{}}
}

// peek reports how long key must wait before its posts are checked again,
// recording nothing.
func (b *refusalBudget) peek(key string) time.Duration {
	if b == nil || b.limit <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c, tracked := b.seen[key]
	if !tracked || c.n < b.limit {
		return 0
	}
	return max(b.window-b.now().Sub(c.since), 0)
}

// record counts one refusal against key.
func (b *refusalBudget) record(key string) {
	if b == nil || b.limit <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	c, tracked := b.seen[key]
	if !tracked || now.Sub(c.since) >= b.window {
		if !tracked && len(b.seen) >= cooldownSweepThreshold {
			b.sweep(now)
		}
		b.seen[key] = refusalCount{since: now, n: 1}
		return
	}
	c.n++
	b.seen[key] = c
}

// sweep drops entries whose window has passed. Callers must hold the mutex.
func (b *refusalBudget) sweep(now time.Time) {
	for key, c := range b.seen {
		if now.Sub(c.since) >= b.window {
			delete(b.seen, key)
		}
	}
}

// sweepExpired drops every entry whose window has passed by the budget's
// clock.
func (b *refusalBudget) sweepExpired() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep(b.now())
}

// expiring is what a cooldownSweep sweeps: anything that keeps client
// addresses only until they can no longer refuse anything.
type expiring interface {
	sweepExpired()
}

// cooldownSweep drops the expired entries of a set of limiters once each
// period. Two things run it. A goroutine of its own does, on a timer, so a
// quiet server forgets without a request arriving to prompt it. And every
// request first checks whether a sweep is overdue and runs it if so, because a
// host that gives the process CPU only while it answers a request — Cloud
// Run's default — holds the timer back while the instance is idle, and the
// next request is the first moment the process is sure to run again.
type cooldownSweep struct {
	every    time.Duration
	limiters []expiring
	// last is when the most recent sweep began, in Unix nanoseconds.
	last atomic.Int64
}

func newCooldownSweep(every time.Duration, limiters ...expiring) *cooldownSweep {
	s := &cooldownSweep{every: every, limiters: limiters}
	s.last.Store(time.Now().UnixNano())
	return s
}

// run sweeps every limiter, noting when it began.
func (s *cooldownSweep) run(at time.Time) {
	s.last.Store(at.UnixNano())
	for _, l := range s.limiters {
		l.sweepExpired()
	}
}

// runIfDue sweeps every limiter if a whole period has passed by now since the
// last sweep began, which with the timer running happens only when it has been
// held back. Of several requests that find a sweep due at once, one runs it.
func (s *cooldownSweep) runIfDue(now time.Time) {
	if s == nil {
		return
	}
	last := s.last.Load()
	if now.UnixNano()-last < int64(s.every) || !s.last.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	for _, l := range s.limiters {
		l.sweepExpired()
	}
}

// beforeEachRequest runs an overdue sweep before the request is handled.
func (s *cooldownSweep) beforeEachRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.runIfDue(time.Now())
		next.ServeHTTP(w, r)
	})
}

// start runs the sweep once each period on a goroutine of its own, until stop
// is called. stop returns once that goroutine has finished, and calling it
// again does nothing, so an App closed twice, or a test that stops it early,
// leaves nothing running.
func (s *cooldownSweep) start() (stop func()) {
	ticker := time.NewTicker(s.every)
	quit := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.run(time.Now())
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
