package outboundsend

import (
	"fmt"
	"testing"
	"time"
)

// TestRateSnooze covers the elapsed-time backoff (#771): the base wait is the
// clampRateSnooze bound, but a message that has already been waiting to fire
// sleeps at least as long as it has waited (doubling the total wait each pass →
// logarithmic re-drives), capped at rateMaxSnooze, and the backoff is disabled
// on an unknown (zero) fire time.
func TestRateSnooze(t *testing.T) {
	const window = time.Minute

	t.Run("fresh fire time falls back to the base wait", func(t *testing.T) {
		// elapsed ≈ 0 < base, so the RetryAt-derived base wins unchanged.
		got := rateSnooze(time.Now(), 30*time.Second, window)
		if got < 30*time.Second || got > 30*time.Second+time.Second {
			t.Errorf("rateSnooze = %s, want ~30s (base wait, no backoff yet)", got)
		}
	})

	t.Run("elapsed backoff grows past the one-window cap", func(t *testing.T) {
		// Base would clamp to 250ms, but the message has been waiting ~90s, so it
		// sleeps ~90s — deliberately beyond the old window (1m) cap, and below
		// rateMaxSnooze (3m) so it is not yet capped.
		got := rateSnooze(time.Now().Add(-90*time.Second), rateMinSnooze, window)
		if got < 90*time.Second-time.Second || got > 90*time.Second+time.Second {
			t.Errorf("rateSnooze = %s, want ~90s (backoff by elapsed wait, above the window)", got)
		}
		if got <= window {
			t.Errorf("rateSnooze = %s did not exceed the window %s — backoff not applied", got, window)
		}
	})

	t.Run("capped at rateMaxSnooze", func(t *testing.T) {
		// Waiting an hour must not sleep an hour: bounded well under the 72h horizon.
		if got := rateSnooze(time.Now().Add(-time.Hour), rateMinSnooze, window); got != rateMaxSnooze {
			t.Errorf("rateSnooze = %s, want the rateMaxSnooze cap %s", got, rateMaxSnooze)
		}
	})

	t.Run("zero fire time disables the backoff", func(t *testing.T) {
		// A zero fireTime would make time.Since() ~centuries; the guard must fall
		// back to the base wait instead of snoozing forever (up to the cap).
		if got := rateSnooze(time.Time{}, 30*time.Second, window); got != 30*time.Second {
			t.Errorf("rateSnooze = %s, want the 30s base wait (backoff disabled on zero fire time)", got)
		}
	})

	t.Run("floors at rateMinSnooze via the base clamp", func(t *testing.T) {
		// A RetryAt in the past with no usable fire time still floors off the hot loop.
		if got := rateSnooze(time.Time{}, -5*time.Second, window); got != rateMinSnooze {
			t.Errorf("rateSnooze = %s, want the rateMinSnooze floor %s", got, rateMinSnooze)
		}
	})
}

// TestRateJitterScalesWithSpread pins the #771 anti-herd fix: the jitter spread
// grows with the snooze it decorates, so a capped, minutes-long backlog fans out
// proportionally instead of re-herding in a fixed window-sized band. Every
// sample must stay under spread/4, and a larger spread must reach a strictly
// wider maximum.
func TestRateJitterScalesWithSpread(t *testing.T) {
	maxOver := func(spread time.Duration) time.Duration {
		var hi time.Duration
		for i := 0; i < 1000; i++ {
			j := rateJitter(fmt.Sprintf("msg_%d", i), spread)
			if j >= spread/4 {
				t.Fatalf("jitter %s >= spread/4 %s (must stay within the spread)", j, spread/4)
			}
			if j > hi {
				hi = j
			}
		}
		return hi
	}
	small := maxOver(time.Minute)      // ceiling 15s
	large := maxOver(3 * time.Minute)  // ceiling 45s
	if large <= small {
		t.Errorf("jitter did not scale with spread: 1m→%s, 3m→%s", small, large)
	}
	// A 3m spread must fan out beyond the old fixed 1m/4=15s band.
	if large <= time.Minute/4 {
		t.Errorf("large-spread jitter %s did not exceed the 1m/4=%s band", large, time.Minute/4)
	}
}

// TestSendJobFireTime pins max(accept, scheduled) with the zero-when-unknown
// contract shared by pastRetryHorizon and the rateSnooze backoff.
func TestSendJobFireTime(t *testing.T) {
	accept := time.Now().Add(-time.Hour)
	sched := time.Now().Add(time.Hour)

	if got := (&SendJob{AcceptedAt: accept, ScheduledAt: sched}).fireTime(); !got.Equal(sched) {
		t.Errorf("fireTime = %s, want the later scheduled time %s", got, sched)
	}
	if got := (&SendJob{AcceptedAt: accept}).fireTime(); !got.Equal(accept) {
		t.Errorf("fireTime = %s, want the accept time %s (no schedule)", got, accept)
	}
	if got := (&SendJob{}).fireTime(); !got.IsZero() {
		t.Errorf("fireTime = %s, want zero when both timestamps are unknown", got)
	}
}
