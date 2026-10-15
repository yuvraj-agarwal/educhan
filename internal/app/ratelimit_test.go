package app

import (
	"sort"
	"strings"
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

// An admitted action the server then could not carry out is refunded: the
// cooldowns it started are taken back, and the poster can try again at once.
// A cooldown another action started since is not, so a refund cannot cut
// short a wait it did not begin.
func TestChargeAllRefundsWhatItStarted(t *testing.T) {
	now := time.Now()
	post := newCooldownLimiter(5 * time.Second)
	upload := newCooldownLimiter(30 * time.Second)
	for _, l := range []*cooldownLimiter{post, upload} {
		l.now = func() time.Time { return now }
	}
	both := []cooldown{{post, "a"}, {upload, "a"}}

	_, refund, ok := chargeAll(both...)
	if !ok {
		t.Fatal("the first image post should be allowed")
	}
	refund()
	if _, ok := allowAll(both...); !ok {
		t.Fatal("after a refund the same post should be allowed at once")
	}
	if len(post.seen) != 1 || len(upload.seen) != 1 {
		t.Fatalf("tracked %d and %d keys, want the one admitted post's", len(post.seen), len(upload.seen))
	}

	// A refund that comes after another action restarted the cooldown leaves
	// that action's cooldown in place.
	now = now.Add(31 * time.Second)
	_, stale, ok := chargeAll(both...)
	if !ok {
		t.Fatal("the post after the cooldowns ran out should be allowed")
	}
	now = now.Add(31 * time.Second)
	if _, ok := allowAll(both...); !ok {
		t.Fatal("the next post should be allowed")
	}
	stale()
	if _, ok := allowAll(both...); ok {
		t.Error("a stale refund took back the cooldown a later post started")
	}

	// A disabled or absent limiter has nothing to refund.
	var none *cooldownLimiter
	_, refund, ok = chargeAll(cooldown{newCooldownLimiter(0), "a"}, cooldown{none, "a"})
	if !ok {
		t.Fatal("disabled limiters must allow every action")
	}
	refund()
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

// The refusal budget lets a client be refused limit times in a window, then
// makes it wait out the rest of the window, then starts counting again. Only
// refusals count, and each client has its own.
func TestRefusalBudget(t *testing.T) {
	now := time.Now()
	budget := newRefusalBudget(3, time.Minute)
	budget.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if wait := budget.peek("a"); wait != 0 {
			t.Fatalf("after %d refusals: wait %s, want none", i, wait)
		}
		budget.record("a")
		now = now.Add(10 * time.Second)
	}
	if wait := budget.peek("a"); wait != 30*time.Second {
		t.Fatalf("after the limit: wait %s, want the 30s left of the window", wait)
	}
	if wait := budget.peek("b"); wait != 0 {
		t.Errorf("another client: wait %s, want none", wait)
	}
	// Peeking records nothing, however often it is done.
	for i := 0; i < 10; i++ {
		budget.peek("b")
	}
	if wait := budget.peek("b"); wait != 0 {
		t.Errorf("another client after peeks: wait %s, want none", wait)
	}

	now = now.Add(30 * time.Second)
	if wait := budget.peek("a"); wait != 0 {
		t.Fatalf("once the window has passed: wait %s, want none", wait)
	}
	budget.record("a")
	if got := budgetKeys(budget); len(got) != 1 || got[0] != "a" {
		t.Fatalf("tracked %v, want only the refused client", got)
	}
	if c := budget.seen["a"]; c.n != 1 || !c.since.Equal(now) {
		t.Errorf("a refusal after the window: %+v, want a fresh count of one", c)
	}

	now = now.Add(time.Minute)
	budget.sweepExpired()
	if got := budgetKeys(budget); len(got) != 0 {
		t.Errorf("after the window: tracked %v, want nothing", got)
	}

	off := newRefusalBudget(0, time.Minute)
	for i := 0; i < 5; i++ {
		off.record("a")
	}
	if wait := off.peek("a"); wait != 0 || len(budgetKeys(off)) != 0 {
		t.Errorf("a zero limit: wait %s, tracked %v; want it off", wait, budgetKeys(off))
	}
	var none *refusalBudget
	none.record("a")
	none.sweepExpired()
	if wait := none.peek("a"); wait != 0 {
		t.Errorf("a nil budget: wait %s, want none", wait)
	}
}

// TestRefusalBudgetSweeps is TestCooldownLimiterSweeps for the refusal
// budget: a burst of refused addresses between two ticks of the periodic sweep
// is swept when the next new address is recorded, so the map holds no address
// past its window however many arrive at once. An address still inside its
// window is kept.
func TestRefusalBudgetSweeps(t *testing.T) {
	now := time.Now()
	budget := newRefusalBudget(3, time.Minute)
	budget.now = func() time.Time { return now }

	for i := 0; i < cooldownSweepThreshold; i++ {
		budget.record(string(rune(i)))
	}
	now = now.Add(2 * time.Minute)
	budget.record("recent")
	if got := len(budgetKeys(budget)); got != 1 {
		t.Fatalf("tracked keys = %d, want 1 after the sweep", got)
	}
	// One that comes in while "recent" is inside its window leaves it alone.
	budget.record("fresh")
	if got := budgetKeys(budget); len(got) != 2 || got[0] != "fresh" || got[1] != "recent" {
		t.Errorf("tracked %v, want the two addresses inside their window", got)
	}
}

func budgetKeys(b *refusalBudget) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.seen))
	for key := range b.seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// setBudgetClock replaces a budget's clock under its lock, as setClock does
// for a cooldown.
func setBudgetClock(b *refusalBudget, now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = now
}
