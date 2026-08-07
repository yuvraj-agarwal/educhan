package app

import (
	"testing"
	"time"
)

func TestCooldownLimiter(t *testing.T) {
	now := time.Now()
	limiter := newCooldownLimiter(10 * time.Second)
	limiter.now = func() time.Time { return now }

	if _, ok := limiter.Allow("a"); !ok {
		t.Fatal("first action should be allowed")
	}

	wait, ok := limiter.Allow("a")
	if ok {
		t.Fatal("second action within the cooldown should be rejected")
	}
	if wait != 10*time.Second {
		t.Errorf("retry after = %s, want 10s", wait)
	}

	if _, ok := limiter.Allow("b"); !ok {
		t.Error("a different key should have its own cooldown")
	}

	// A rejected attempt must not extend the cooldown.
	now = now.Add(6 * time.Second)
	if wait, _ := limiter.Allow("a"); wait != 4*time.Second {
		t.Errorf("retry after = %s, want 4s", wait)
	}

	now = now.Add(5 * time.Second)
	if _, ok := limiter.Allow("a"); !ok {
		t.Error("action should be allowed once the cooldown elapsed")
	}
}

func TestCooldownLimiterDisabled(t *testing.T) {
	limiter := newCooldownLimiter(0)
	for i := 0; i < 3; i++ {
		if _, ok := limiter.Allow("a"); !ok {
			t.Fatal("a zero cooldown must allow every action")
		}
	}

	var nilLimiter *cooldownLimiter
	if _, ok := nilLimiter.Allow("a"); !ok {
		t.Error("a nil limiter must allow every action")
	}
}

func TestCooldownLimiterSweeps(t *testing.T) {
	now := time.Now()
	limiter := newCooldownLimiter(time.Second)
	limiter.now = func() time.Time { return now }

	for i := 0; i < cooldownSweepThreshold; i++ {
		limiter.Allow(string(rune(i)))
	}
	now = now.Add(2 * time.Second)
	if _, ok := limiter.Allow("fresh"); !ok {
		t.Fatal("expected the action to be allowed")
	}
	if got := len(limiter.seen); got != 1 {
		t.Errorf("tracked keys = %d, want 1 after the sweep", got)
	}
}
