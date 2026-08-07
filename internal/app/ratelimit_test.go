package app

import (
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
