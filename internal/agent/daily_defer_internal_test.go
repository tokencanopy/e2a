package agent

import (
	"testing"
	"time"
)

// TestNextUTCMidnight pins the deferral boundary math: any instant in a UTC
// day maps to the first instant of the NEXT day — including exactly-midnight
// (a send deferred at 00:00:00Z waits a full day; the cap that blocked it
// belongs to the day that just began).
func TestNextUTCMidnight(t *testing.T) {
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 8, 28, 12, 30, 0, 0, time.UTC), time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 8, 28, 23, 59, 59, 999_999_999, time.UTC), time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)},
		// Month and year boundaries.
		{time.Date(2026, 8, 31, 5, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		// Non-UTC input is converted, not trusted.
		{time.Date(2026, 8, 28, 20, 0, 0, 0, time.FixedZone("PDT", -7*3600)), time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		if got := nextUTCMidnight(tc.in); !got.Equal(tc.want) {
			t.Errorf("nextUTCMidnight(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestDailyDeferJitter pins determinism and range: same ID → same offset
// (stable across worker restarts), always within [0, 1h).
func TestDailyDeferJitter(t *testing.T) {
	a1 := dailyDeferJitter("msg_a")
	a2 := dailyDeferJitter("msg_a")
	if a1 != a2 {
		t.Errorf("jitter not deterministic: %v vs %v", a1, a2)
	}
	spread := map[time.Duration]bool{}
	for _, id := range []string{"msg_a", "msg_b", "msg_c", "msg_d", "msg_e"} {
		j := dailyDeferJitter(id)
		if j < 0 || j >= time.Hour {
			t.Errorf("jitter(%s) = %v, want [0, 1h)", id, j)
		}
		spread[j] = true
	}
	if len(spread) < 2 {
		t.Error("five distinct IDs produced one offset; jitter is not spreading the backlog")
	}
}
