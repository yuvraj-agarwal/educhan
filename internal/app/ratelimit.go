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
// cooldown has passed. The keys are client addresses, as are the captcha
// meter's, whose one-minute window counts as a cooldown here, so this is the
// bound on how long the server remembers one while the process has CPU: an
// address is held in memory no longer than the longest cooldown that applies
// to it plus one minute, counted from its last action that started a cooldown,
// however quiet the server is. A host that gives the process CPU only while it
// answers a request, which is Cloud Run's default, holds the timer back while the
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
// as one decision, and starts them all when it is. When it is not, the caller
// is told how long to wait: the longest of the outstanding waits.
//
// A cooldown starts on every limiter only once all of them allow the action. A
// request one cooldown refuses must not consume another, or a reply carrying an
// image would be charged for both and the two limits would compound into a wait
// neither of them describes. Nothing is recorded for a refused action either, so
// a rejected caller cannot extend its own cooldown by retrying.
//
// Each limiter checks and starts its cooldown in one step (take), so of two
// requests for the same key at the same moment exactly one is admitted. A
// cooldown taken before a later one refuses is taken back (forget). The order
// is the caller's, the same for every request, so two requests never hold
// each other's first cooldown; at worst a request is refused because another
// from the same client holds, for that instant, a cooldown it then gives back.
//
// A handler that spends a captcha answer checks first with longestWait, which
// starts nothing, so a request that has to wait keeps its answer, and calls
// allowAll only once the answer is spent, so a request refused for its answer
// starts no cooldown.
func allowAll(cooldowns ...cooldown) (retryAfter time.Duration, ok bool) {
	started := make([]time.Time, 0, len(cooldowns))
	for i, c := range cooldowns {
		wait, at, ok := c.limiter.take(c.key)
		if !ok {
			for j, at := range started {
				cooldowns[j].limiter.forget(cooldowns[j].key, at)
			}
			return max(wait, longestWait(cooldowns[i+1:]...)), false
		}
		started = append(started, at)
	}
	return 0, true
}

// longestWait is the longest of the waits the cooldowns still impose, recording
// nothing: what allowAll would refuse with, for a caller that wants to know
// before it does anything else with a request.
func longestWait(cooldowns ...cooldown) time.Duration {
	var longest time.Duration
	for _, c := range cooldowns {
		if wait := c.limiter.peek(c.key); wait > longest {
			longest = wait
		}
	}
	return longest
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

// take starts key's cooldown unless key still has one to wait out, checking
// and starting under one lock. It reports how long key must wait when it
// cannot, and when the cooldown started when it could, which forget takes to
// tell this start from a later one.
func (l *cooldownLimiter) take(key string) (wait time.Duration, started time.Time, ok bool) {
	if l == nil || l.interval <= 0 {
		return 0, time.Time{}, true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if last, tracked := l.seen[key]; tracked {
		if wait := l.interval - now.Sub(last); wait > 0 {
			return wait, time.Time{}, false
		}
	}
	if len(l.seen) >= cooldownSweepThreshold {
		l.sweep(now)
	}
	l.seen[key] = now
	return 0, now, true
}

// forget takes back the cooldown take started for key at started, if it is
// still the one key is waiting on. A key with nothing to wait on is not
// tracked at all, so the entry goes rather than being wound back.
func (l *cooldownLimiter) forget(key string, started time.Time) {
	if l == nil || l.interval <= 0 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if last, tracked := l.seen[key]; tracked && last.Equal(started) {
		delete(l.seen, key)
	}
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

// expiring is anything a cooldownSweep keeps short-lived: the cooldown
// limiters, and the captcha's challenge meter.
type expiring interface {
	sweepExpired()
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
