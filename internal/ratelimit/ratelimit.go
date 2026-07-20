package ratelimit

import (
	"sync"
	"time"
)

// Limiter is an in-memory sliding-window rate limiter keyed by string.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time
	window  time.Duration
	max     int
}

func New(window time.Duration, max int) *Limiter {
	l := &Limiter{
		buckets: make(map[string][]time.Time),
		window:  window,
		max:     max,
	}
	go l.cleanupLoop()
	return l
}

// SetMax updates the request budget without replacing the limiter, preserving
// its cleanup goroutine and any existing per-key window state.
func (l *Limiter) SetMax(max int) {
	if max <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.max = max
}

// Allow returns true if the key has not exceeded the rate limit.
// If allowed, the attempt is recorded.
func (l *Limiter) Allow(key string) bool {
	ok, _ := l.AllowWithRetryAfter(key)
	return ok
}

// AllowWithRetryAfter behaves like Allow but additionally returns the
// duration the caller must wait before the next attempt could succeed
// (rounded up to the next whole second so callers can use it for the
// Retry-After response header). When the request is allowed, the
// returned duration is zero. The returned duration is conservative
// (>= one second when blocked) so that a Retry-After header always
// communicates a meaningful, RFC 7231 §7.1.3-compatible delay.
func (l *Limiter) AllowWithRetryAfter(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)

	// Prune expired entries in place.
	valid := l.buckets[key][:0]
	for _, t := range l.buckets[key] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= l.max {
		l.buckets[key] = valid
		// The oldest in-window hit drops off the window at oldest+window,
		// so that's when one more slot opens up.
		retryAfter := valid[0].Add(l.window).Sub(now)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		// Round up to the next whole second for header use.
		if retryAfter%time.Second != 0 {
			retryAfter = retryAfter.Truncate(time.Second) + time.Second
		}
		return false, retryAfter
	}

	l.buckets[key] = append(valid, now)
	return true, 0
}

// AllowN behaves like AllowWithRetryAfter but atomically reserves n slots
// against the window. Returns (true, 0) when all n slots fit; (false,
// retryAfter) when the reservation would overflow the window, without
// recording any of the n hits. Used by batch-send accept-tx to charge N
// against a per-agent throughput limit (docs/design/batch-send.md §4.2,
// §14 Q4). n == 1 is behaviourally identical to AllowWithRetryAfter.
// n == 0 is a no-op — returns (true, 0) without touching state. n < 0 is
// treated as n == 0.
//
// Semantics: all-or-nothing on the reservation. A batch of 100 against a
// window with 50 slots left is rejected outright — batch send never
// consumes some but not all of its reservation, because a partial reserve
// would silently degrade throughput accounting for the caller.
func (l *Limiter) AllowN(key string, n int) (bool, time.Duration) {
	if n <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)

	// Prune expired entries in place (same shape as AllowWithRetryAfter).
	valid := l.buckets[key][:0]
	for _, t := range l.buckets[key] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid)+n > l.max {
		l.buckets[key] = valid
		// If the bucket has no in-window hits yet, no oldest to age out —
		// the caller's n itself exceeds l.max. Retry-after in that case
		// clamps to the full window: no waiting can make an unbounded
		// reservation fit.
		var retryAfter time.Duration
		if len(valid) == 0 {
			retryAfter = l.window
		} else {
			retryAfter = valid[0].Add(l.window).Sub(now)
		}
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		if retryAfter%time.Second != 0 {
			retryAfter = retryAfter.Truncate(time.Second) + time.Second
		}
		return false, retryAfter
	}

	// Record n hits at the same timestamp — they all consumed one slot
	// concurrently at accept-tx time, so they share `now`.
	for i := 0; i < n; i++ {
		valid = append(valid, now)
	}
	l.buckets[key] = valid
	return true, 0
}

// AllowSnapshot behaves like AllowWithRetryAfter but also returns the IETF
// RateLimit header values: the window quota (limit), the remaining quota after
// this request, and the seconds until the window resets (when the oldest
// in-window hit ages out). resetSeconds is >= 1 whenever any hits are recorded.
func (l *Limiter) AllowSnapshot(key string) (ok bool, retryAfter time.Duration, limit, remaining, resetSeconds int) {
	ok, retryAfter = l.AllowWithRetryAfter(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	limit = l.max
	used := len(l.buckets[key])
	remaining = limit - used
	if remaining < 0 {
		remaining = 0
	}
	if used > 0 {
		reset := l.buckets[key][0].Add(l.window).Sub(time.Now())
		resetSeconds = int(reset.Round(time.Second).Seconds())
		if resetSeconds < 1 {
			resetSeconds = 1
		}
	}
	return ok, retryAfter, limit, remaining, resetSeconds
}

func (l *Limiter) cleanupLoop() {
	ticker := time.NewTicker(l.window)
	defer ticker.Stop()
	for range ticker.C {
		l.Cleanup()
	}
}

// Cleanup removes expired entries for all keys. Call periodically to prevent memory growth.
func (l *Limiter) Cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-l.window)
	for key, times := range l.buckets {
		valid := times[:0]
		for _, t := range times {
			if t.After(cutoff) {
				valid = append(valid, t)
			}
		}
		if len(valid) == 0 {
			delete(l.buckets, key)
		} else {
			l.buckets[key] = valid
		}
	}
}
