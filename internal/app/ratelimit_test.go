package app

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// allow is the single-limiter form of allowAll, which is how most of the
// limiter's behaviour is easiest to state.
func allow(l *cooldownLimiter, key string) (time.Duration, bool) {
	return allowAll(cooldown{limiter: l, key: key})
}

func TestCooldownLimiter(t *testing.T) {
	now := time.Now()
	limiter := newCooldownLimiter(10 * time.Second)
	limiter.now = func() time.Time { return now }

	if _, ok := allow(limiter, "a"); !ok {
		t.Fatal("first action should be allowed")
	}

	wait, ok := allow(limiter, "a")
	if ok {
		t.Fatal("second action within the cooldown should be rejected")
	}
	if wait != 10*time.Second {
		t.Errorf("retry after = %s, want 10s", wait)
	}

	if _, ok := allow(limiter, "b"); !ok {
		t.Error("a different key should have its own cooldown")
	}

	// A rejected attempt must not extend the cooldown.
	now = now.Add(6 * time.Second)
	if wait, _ := allow(limiter, "a"); wait != 4*time.Second {
		t.Errorf("retry after = %s, want 4s", wait)
	}

	now = now.Add(5 * time.Second)
	if _, ok := allow(limiter, "a"); !ok {
		t.Error("action should be allowed once the cooldown elapsed")
	}
}

func TestCooldownLimiterDisabled(t *testing.T) {
	limiter := newCooldownLimiter(0)
	for i := 0; i < 3; i++ {
		if _, ok := allow(limiter, "a"); !ok {
			t.Fatal("a zero cooldown must allow every action")
		}
	}

	var nilLimiter *cooldownLimiter
	if _, ok := allow(nilLimiter, "a"); !ok {
		t.Error("a nil limiter must allow every action")
	}
}

func TestAllowAllReportsTheLongestWait(t *testing.T) {
	now := time.Now()
	short := newCooldownLimiter(5 * time.Second)
	long := newCooldownLimiter(30 * time.Second)
	for _, l := range []*cooldownLimiter{short, long} {
		l.now = func() time.Time { return now }
	}

	both := []cooldown{{limiter: short, key: "a"}, {limiter: long, key: "a"}}
	if _, ok := allowAll(both...); !ok {
		t.Fatal("the first action should be allowed")
	}

	wait, ok := allowAll(both...)
	if ok {
		t.Fatal("the second action should be refused")
	}
	if wait != 30*time.Second {
		t.Errorf("retry after = %s, want 30s: the caller must wait out the longest cooldown", wait)
	}

	// The short cooldown has elapsed but the long one has not, so the wait is
	// what is left of the long one.
	now = now.Add(10 * time.Second)
	if wait, _ := allowAll(both...); wait != 20*time.Second {
		t.Errorf("retry after = %s, want 20s", wait)
	}
}

// A refused action must not start the cooldowns that would have allowed it,
// otherwise a post carrying an image is charged for both limits and the two
// compound into a wait neither of them describes.
func TestAllowAllChargesNothingWhenRefused(t *testing.T) {
	now := time.Now()
	post := newCooldownLimiter(5 * time.Second)
	upload := newCooldownLimiter(30 * time.Second)
	for _, l := range []*cooldownLimiter{post, upload} {
		l.now = func() time.Time { return now }
	}

	// An image post starts both cooldowns.
	if _, ok := allowAll(cooldown{post, "a"}, cooldown{upload, "a"}); !ok {
		t.Fatal("the first image post should be allowed")
	}

	// A second image post is refused by the upload cooldown. The reply cooldown
	// must be left alone, so a text reply still goes through once its own five
	// seconds have passed.
	now = now.Add(6 * time.Second)
	if _, ok := allowAll(cooldown{post, "a"}, cooldown{upload, "a"}); ok {
		t.Fatal("a second image post within the upload cooldown should be refused")
	}
	if _, ok := allowAll(cooldown{post, "a"}); !ok {
		t.Error("the refused image post must not have consumed the reply cooldown")
	}

	// The refused attempt must not have pushed the upload cooldown out either.
	now = now.Add(24 * time.Second)
	if _, ok := allowAll(cooldown{upload, "a"}); !ok {
		t.Error("a refused attempt must not extend the upload cooldown")
	}
}

// Requests from one client racing for the same cooldowns: exactly one is
// admitted, because allowAll checks and starts them in one step, and a racer
// the shared upload cooldown refuses starts nothing, so the reply cooldown is
// held only on the board the winner posted to.
func TestAllowAllAdmitsOneOfARace(t *testing.T) {
	post := newCooldownLimiter(time.Hour)
	upload := newCooldownLimiter(time.Hour)

	const racers = 64
	var admitted atomic.Int32
	var winner atomic.Value
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		board := fmt.Sprint("board ", i%2)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := allowAll(cooldown{post, board + "|a"}, cooldown{upload, "a"}); ok {
				admitted.Add(1)
				winner.Store(board)
			}
		}()
	}
	close(start)
	wg.Wait()

	if n := admitted.Load(); n != 1 {
		t.Fatalf("%d of %d racing image posts admitted, want 1", n, racers)
	}
	won := winner.Load().(string)
	for _, board := range []string{"board 0", "board 1"} {
		held := post.peek(board+"|a") > 0
		if held != (board == won) {
			t.Errorf("reply cooldown on %s held %t after the race, want it held only where the post went (%s)", board, held, won)
		}
	}
}

// Requests from one client racing with different sets of cooldowns, text and
// image replies to three boards: no two admitted requests share a cooldown,
// and every refused request meets a cooldown an admitted one holds. A refusal
// by a cooldown that nothing admitted holds would mean another request had
// held it for an instant and let it go, refusing a post that could have gone
// ahead.
func TestAllowAllRefusesOnlyForACooldownAnAdmittedRequestHolds(t *testing.T) {
	for round := range 1000 {
		post := newCooldownLimiter(time.Hour)
		upload := newCooldownLimiter(time.Hour)

		const racers = 12
		requests := make([][]cooldown, racers)
		admitted := make([]bool, racers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range racers {
			requests[i] = []cooldown{{post, fmt.Sprintf("board %d|a", (i+round)%3)}}
			if (i+round)%2 == 0 {
				requests[i] = append(requests[i], cooldown{upload, "a"})
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, admitted[i] = allowAll(requests[i]...)
			}()
		}
		close(start)
		wg.Wait()

		held := map[*cooldownLimiter]map[string]int{post: {}, upload: {}}
		for i, cooldowns := range requests {
			if admitted[i] {
				for _, c := range cooldowns {
					held[c.limiter][c.key]++
				}
			}
		}
		for limiter, keys := range held {
			for key, n := range keys {
				if n > 1 {
					t.Fatalf("round %d: %d admitted requests share the cooldown %q", round, n, key)
				}
				if limiter.peek(key) == 0 {
					t.Fatalf("round %d: the admitted cooldown %q is not running", round, key)
				}
			}
		}
		for i, cooldowns := range requests {
			if admitted[i] {
				continue
			}
			met := false
			for _, c := range cooldowns {
				met = met || held[c.limiter][c.key] > 0
			}
			if !met {
				t.Fatalf("round %d: request %d (%v) was refused, but no admitted request holds any of its cooldowns", round, i, cooldowns)
			}
		}
	}
}

func TestCooldownLimiterSweeps(t *testing.T) {
	now := time.Now()
	limiter := newCooldownLimiter(time.Second)
	limiter.now = func() time.Time { return now }

	for i := 0; i < cooldownSweepThreshold; i++ {
		allow(limiter, string(rune(i)))
	}
	now = now.Add(2 * time.Second)
	if _, ok := allow(limiter, "fresh"); !ok {
		t.Fatal("expected the action to be allowed")
	}
	if got := len(limiter.seen); got != 1 {
		t.Errorf("tracked keys = %d, want 1 after the sweep", got)
	}
}

// A map full of live cooldowns cannot be shrunk by a sweep, so sweeping it on
// every record would cost each admission a pass over the whole map, under the
// lock every admission takes. Past the threshold, record sweeps again only once
// the map has doubled since the last sweep.
func TestCooldownLimiterSweepsOnceEachTimeItDoubles(t *testing.T) {
	now := time.Now()
	limiter := newCooldownLimiter(time.Second)
	limiter.now = func() time.Time { return now }

	for i := range cooldownSweepThreshold {
		allow(limiter, fmt.Sprintf("live %d", i))
	}
	// At the threshold, a sweep that finds every cooldown live frees nothing.
	allow(limiter, "one more")
	if got := len(limiter.seen); got != cooldownSweepThreshold+1 {
		t.Fatalf("tracked keys = %d, want %d", got, cooldownSweepThreshold+1)
	}
	// Every cooldown so far runs out, but the map has not doubled since that
	// sweep, so nothing sweeps them until it has.
	now = now.Add(2 * time.Second)
	for i := cooldownSweepThreshold + 1; i < 2*cooldownSweepThreshold; i++ {
		allow(limiter, fmt.Sprintf("later %d", i))
	}
	if got := len(limiter.seen); got != 2*cooldownSweepThreshold {
		t.Fatalf("tracked keys = %d, want %d: record swept before the map doubled", got, 2*cooldownSweepThreshold)
	}
	// Doubled: the next record sweeps, and keeps only the live cooldowns.
	allow(limiter, "doubled")
	if got, want := len(limiter.seen), cooldownSweepThreshold; got != want {
		t.Errorf("tracked keys = %d after the map doubled, want %d: the expired ones swept", got, want)
	}
}

// trackedKeys lists what a limiter holds, under its lock, so a test can read it
// while the sweep goroutine runs.
func trackedKeys(l *cooldownLimiter) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := make([]string, 0, len(l.seen))
	for key := range l.seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// setClock replaces a limiter's clock under its lock, so it can be moved while
// the sweep goroutine reads it.
func setClock(l *cooldownLimiter, now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// waitUntil polls cond until it holds, failing the test after a few seconds.
// It stands in for a sweep goroutine's next tick, which a test cannot see.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A sweep keeps exactly the entries that can still refuse something: an entry
// goes the moment its cooldown has run out by the limiter's clock, and not a
// moment before. With a sweep every cooldownSweepInterval, that is what holds
// an address in memory to its cooldown plus one interval.
func TestSweepExpiredKeepsOnlyLiveCooldowns(t *testing.T) {
	now := time.Now()
	limiter := newCooldownLimiter(10 * time.Second)
	limiter.now = func() time.Time { return now }

	allow(limiter, "198.51.100.1")
	now = now.Add(5 * time.Second)
	allow(limiter, "198.51.100.2")

	now = now.Add(5*time.Second - time.Nanosecond)
	limiter.sweepExpired()
	if got := strings.Join(trackedKeys(limiter), ","); got != "198.51.100.1,198.51.100.2" {
		t.Errorf("a nanosecond before the first cooldown ends: tracked %s, want both", got)
	}

	now = now.Add(time.Nanosecond)
	limiter.sweepExpired()
	if got := strings.Join(trackedKeys(limiter), ","); got != "198.51.100.2" {
		t.Errorf("as the first cooldown ends: tracked %s, want only the second", got)
	}

	now = now.Add(5 * time.Second)
	limiter.sweepExpired()
	if got := trackedKeys(limiter); len(got) != 0 {
		t.Errorf("after both cooldowns: tracked %v, want nothing", got)
	}

	var nilLimiter *cooldownLimiter
	nilLimiter.sweepExpired()
}

// A started cooldownSweep sweeps every limiter it is given, on its own, with
// no request arriving to prompt it, and stop leaves nothing running, however
// often it is called.
func TestSweepCooldownsRunsUntilStopped(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }

	short := newCooldownLimiter(time.Second)
	long := newCooldownLimiter(time.Minute)
	for _, l := range []*cooldownLimiter{short, long} {
		l.now = now
		allow(l, "198.51.100.1")
	}

	stop := newCooldownSweep(time.Millisecond, short, long).start()
	defer stop()

	clock.Add(int64(2 * time.Second))
	waitUntil(t, "the short cooldown's entry is swept", func() bool { return len(trackedKeys(short)) == 0 })
	if got := trackedKeys(long); len(got) != 1 {
		t.Errorf("the long cooldown holds %v, want its entry kept while it can still refuse", got)
	}

	clock.Add(int64(time.Minute))
	waitUntil(t, "the long cooldown's entry is swept", func() bool { return len(trackedKeys(long)) == 0 })

	stop()
	stop()
}

// A host that gives the process CPU only while it answers a request holds the
// timer back while it is idle, so a request runs the sweep first once a whole
// period has passed since the last one began — and not before, or every
// request would scan the limiters. Of the requests that find it due, one runs
// it.
func TestCooldownSweepRunsBeforeARequestOnceItIsOverdue(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	limiter := newCooldownLimiter(time.Second)
	limiter.now = func() time.Time { return time.Unix(0, clock.Load()) }

	sweep := newCooldownSweep(time.Minute, limiter) // never started: the timer is held back
	began := time.Unix(0, sweep.last.Load())

	allow(limiter, "198.51.100.1")
	clock.Add(int64(2 * time.Second)) // the cooldown has run out

	sweep.runIfDue(began.Add(time.Minute - time.Nanosecond))
	if got := trackedKeys(limiter); len(got) != 1 {
		t.Fatalf("a request before the sweep fell due: tracked %v, want the entry kept for the timer", got)
	}
	sweep.runIfDue(began.Add(time.Minute))
	if got := trackedKeys(limiter); len(got) != 0 {
		t.Fatalf("the first request after the sweep fell due: tracked %v, want nothing", got)
	}

	// That request ran the sweep, so the next one a moment later does not.
	allow(limiter, "198.51.100.2")
	clock.Add(int64(2 * time.Second))
	sweep.runIfDue(began.Add(time.Minute + time.Second))
	if got := trackedKeys(limiter); len(got) != 1 {
		t.Errorf("a request just after a sweep: tracked %v, want the entry kept for the next one", got)
	}

	var none *cooldownSweep
	none.runIfDue(time.Now())
}
