package ratelimit

import (
	"testing"
	"time"
)

func TestAllow(t *testing.T) {
	l := New(1*time.Second, 3)

	for i := 0; i < 3; i++ {
		if !l.Allow("key1") {
			t.Fatalf("expected Allow on attempt %d", i+1)
		}
	}

	if l.Allow("key1") {
		t.Fatal("expected rate limit to deny 4th attempt")
	}

	// Different key should still be allowed
	if !l.Allow("key2") {
		t.Fatal("expected Allow for different key")
	}
}

func TestSetMaxIgnoresNonPositiveValues(t *testing.T) {
	l := New(time.Minute, 3)

	l.SetMax(0)

	ok, _, limit, _, _ := l.AllowSnapshot("key")
	if !ok || limit != 3 {
		t.Fatalf("snapshot after SetMax(0) = ok=%v limit=%d, want ok=true limit=3", ok, limit)
	}
}

func TestWindowExpiry(t *testing.T) {
	l := New(50*time.Millisecond, 2)

	l.Allow("k")
	l.Allow("k")

	if l.Allow("k") {
		t.Fatal("expected denial at limit")
	}

	time.Sleep(60 * time.Millisecond)

	if !l.Allow("k") {
		t.Fatal("expected Allow after window expired")
	}
}

func TestCleanup(t *testing.T) {
	l := New(50*time.Millisecond, 10)

	l.Allow("a")
	l.Allow("b")

	time.Sleep(60 * time.Millisecond)
	l.Cleanup()

	l.mu.Lock()
	count := len(l.buckets)
	l.mu.Unlock()

	if count != 0 {
		t.Errorf("expected 0 buckets after cleanup, got %d", count)
	}
}

func TestCleanupLoopRunsAutomatically(t *testing.T) {
	// Use a very short window so the cleanup ticker fires quickly
	l := New(50*time.Millisecond, 10)

	l.Allow("x")
	l.Allow("y")

	l.mu.Lock()
	if len(l.buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(l.buckets))
	}
	l.mu.Unlock()

	// Wait for entries to expire and cleanup loop to fire
	time.Sleep(120 * time.Millisecond)

	l.mu.Lock()
	count := len(l.buckets)
	l.mu.Unlock()

	if count != 0 {
		t.Errorf("expected cleanup loop to remove expired buckets, got %d", count)
	}
}

func TestAllowWithRetryAfter_AllowedReturnsZero(t *testing.T) {
	l := New(1*time.Second, 3)
	ok, retry := l.AllowWithRetryAfter("k")
	if !ok {
		t.Fatal("expected allow on first attempt")
	}
	if retry != 0 {
		t.Errorf("expected retryAfter=0 when allowed, got %v", retry)
	}
}

func TestAllowWithRetryAfter_BlockedReturnsPositiveDuration(t *testing.T) {
	l := New(60*time.Second, 2)
	for i := 0; i < 2; i++ {
		if ok, _ := l.AllowWithRetryAfter("k"); !ok {
			t.Fatalf("attempt %d should have been allowed", i+1)
		}
	}
	ok, retry := l.AllowWithRetryAfter("k")
	if ok {
		t.Fatal("3rd attempt over limit must be denied")
	}
	// Should be roughly the window (60s) minus elapsed test time;
	// the helper rounds up to whole seconds.
	if retry < time.Second {
		t.Errorf("retryAfter must be at least 1s, got %v", retry)
	}
	if retry > 60*time.Second {
		t.Errorf("retryAfter must not exceed the window (%v), got %v", 60*time.Second, retry)
	}
	if retry%time.Second != 0 {
		t.Errorf("retryAfter should be a whole number of seconds (for HTTP Retry-After), got %v", retry)
	}
}

func TestAllowWithRetryAfter_RetryAfterShrinksAsWindowAdvances(t *testing.T) {
	// Use a multi-second window so the ceiling-to-whole-second rounding
	// (required for HTTP Retry-After) doesn't mask sub-second shrinkage.
	l := New(10*time.Second, 1)
	if ok, _ := l.AllowWithRetryAfter("k"); !ok {
		t.Fatal("first attempt should be allowed")
	}
	_, retry1 := l.AllowWithRetryAfter("k")
	time.Sleep(2 * time.Second)
	_, retry2 := l.AllowWithRetryAfter("k")
	if retry2 >= retry1 {
		t.Errorf("retryAfter should shrink as the window advances: first=%v second=%v", retry1, retry2)
	}
}

func TestCleanupLoopKeepsActiveEntries(t *testing.T) {
	l := New(100*time.Millisecond, 10)

	l.Allow("active")

	// Wait less than the window so entries are still valid
	time.Sleep(50 * time.Millisecond)

	l.mu.Lock()
	count := len(l.buckets)
	l.mu.Unlock()

	if count != 1 {
		t.Errorf("expected active bucket to remain, got %d buckets", count)
	}
}

func TestAllowN_ReservesAllOrNone(t *testing.T) {
	l := New(1*time.Second, 10)
	// Batch of 5 in an empty bucket → allowed.
	if ok, _ := l.AllowN("k", 5); !ok {
		t.Fatalf("expected AllowN(5) to succeed on empty bucket")
	}
	// Bucket now has 5. Another batch of 5 → allowed (fills to 10).
	if ok, _ := l.AllowN("k", 5); !ok {
		t.Fatalf("expected AllowN(5) to succeed when bucket has 5")
	}
	// Bucket is now full. Any batch >= 1 → denied without recording.
	if ok, _ := l.AllowN("k", 1); ok {
		t.Fatal("expected AllowN(1) to deny on full bucket")
	}
	// The denied attempt above must not have consumed a slot — retrying
	// as a single Allow (in the same window) should also deny.
	if l.Allow("k") {
		t.Fatal("denied AllowN must not consume a slot")
	}
}

func TestAllowN_AllOrNothingOnOverflow(t *testing.T) {
	l := New(1*time.Second, 10)
	// Use 8 slots.
	if ok, _ := l.AllowN("k", 8); !ok {
		t.Fatalf("expected AllowN(8) to succeed")
	}
	// Now request 5 — bucket has 8, request would overflow to 13 > 10.
	// Must deny WITHOUT recording any of the 5.
	if ok, retry := l.AllowN("k", 5); ok || retry < time.Second {
		t.Fatalf("expected AllowN(5) to deny with >=1s retryAfter, got ok=%v retry=%v", ok, retry)
	}
	// The 2 remaining slots must still be available for smaller reservations.
	if ok, _ := l.AllowN("k", 2); !ok {
		t.Fatalf("expected AllowN(2) to succeed on remaining capacity")
	}
}

func TestAllowN_ZeroIsNoOp(t *testing.T) {
	l := New(1*time.Second, 1)
	if ok, retry := l.AllowN("k", 0); !ok || retry != 0 {
		t.Errorf("AllowN(0) should be no-op, got ok=%v retry=%v", ok, retry)
	}
	// The one slot must still be available afterwards.
	if !l.Allow("k") {
		t.Fatal("AllowN(0) must not consume the slot")
	}
}

func TestAllowN_NExceedsMaxCleanRetry(t *testing.T) {
	l := New(1*time.Second, 10)
	// Request 100 against a max-10 window → cannot ever succeed. Still
	// deny cleanly (no negative reservation, no panic) with retryAfter
	// clamped to at least 1s.
	ok, retry := l.AllowN("k", 100)
	if ok {
		t.Fatal("expected AllowN(100) with max=10 to deny")
	}
	if retry < time.Second {
		t.Errorf("expected retry >= 1s, got %v", retry)
	}
}
