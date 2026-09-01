package limits

import (
	"context"
	"errors"
	"testing"
)

// TestCheckMessageSend_UnitsAware pins the recipient-unit flow-cap math:
// a send of N units is rejected exactly when used + N > cap (the whole
// message crosses the cap; no partial sends), and units == 1 preserves the
// historical `used >= max` boundary bit-for-bit. Expected outcomes are
// stated as independent arithmetic facts, not recomputed via the enforcer.
func TestCheckMessageSend_UnitsAware(t *testing.T) {
	cases := []struct {
		name      string
		used      int
		cap       int
		units     int
		wantBlock bool
	}{
		// 90 used of 100: 10 more lands exactly at the cap — allowed;
		// 11 would cross it — blocked.
		{"fits_exactly", 90, 100, 10, false},
		{"crosses_by_one", 90, 100, 11, true},
		// Historical single-unit boundary: at cap-1 allowed, at cap blocked.
		{"single_under", 99, 100, 1, false},
		{"single_at_cap", 100, 100, 1, true},
		// Zero cap blocks the very first unit (no 0-means-unlimited escape).
		{"zero_cap", 0, 0, 1, true},
		// units < 1 is a caller bug normalized to 1: at cap it must still block.
		{"zero_units_at_cap", 100, 100, 0, true},
		{"zero_units_under_cap", 99, 100, 0, false},
		// A large batch against a fresh account.
		{"batch_fresh", 0, 100, 50, false},
		{"batch_too_big", 0, 100, 101, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{found: true, row: Limits{
				MaxAgents: 100, MaxDomains: 100,
				MaxMessagesMonth: tc.cap, MaxStorageBytes: 1 << 40,
			}}
			counter := &fakeCounter{messagesMonth: tc.used}
			e := newEnforcerWithReader(store, counter, defaultsForTest(), 0)

			err := e.CheckMessageSend(context.Background(), "user1", tc.units)
			le, blocked := IsLimitExceeded(err)
			if blocked != tc.wantBlock {
				t.Fatalf("used=%d cap=%d units=%d: blocked=%v want %v (err=%v)",
					tc.used, tc.cap, tc.units, blocked, tc.wantBlock, err)
			}
			if blocked {
				if le.Resource != "messages_month" {
					t.Errorf("Resource = %q, want messages_month", le.Resource)
				}
				// Current reports what the user has already consumed, not
				// the hypothetical post-send total.
				if le.Current != tc.used {
					t.Errorf("Current = %d, want %d", le.Current, tc.used)
				}
				if le.Limit != tc.cap {
					t.Errorf("Limit = %d, want %d", le.Limit, tc.cap)
				}
			}
		})
	}
}

func intPtr(v int) *int { return &v }

// TestCheckMessageSend_DailyCap pins the optional per-day cap: nil = no
// daily policy (and the day counter must not even be consulted); a set cap
// blocks when usedToday + units > cap; 0 hard-blocks; the monthly cap wins
// precedence when both would fail; a day-counter error propagates
// (fail-closed at the API seam).
func TestCheckMessageSend_DailyCap(t *testing.T) {
	newE := func(day *int, usedMonth, usedToday int) (*DBEnforcer, *fakeCounter) {
		store := &fakeStore{found: true, row: Limits{
			MaxAgents: 100, MaxDomains: 100,
			MaxMessagesMonth: 1000, MaxMessagesDay: day, MaxStorageBytes: 1 << 40,
		}}
		counter := &fakeCounter{messagesMonth: usedMonth, messagesToday: usedToday}
		return newEnforcerWithReader(store, counter, defaultsForTest(), 0), counter
	}

	t.Run("nil_cap_uncapped", func(t *testing.T) {
		e, _ := newE(nil, 500, 999_999) // absurd day usage; must not matter
		if err := e.CheckMessageSend(context.Background(), "u", 1); err != nil {
			t.Fatalf("nil daily cap must not block: %v", err)
		}
	})
	t.Run("fits_exactly", func(t *testing.T) {
		e, _ := newE(intPtr(100), 0, 90)
		if err := e.CheckMessageSend(context.Background(), "u", 10); err != nil {
			t.Fatalf("90+10 = cap 100 must pass: %v", err)
		}
	})
	t.Run("crosses_by_one", func(t *testing.T) {
		e, _ := newE(intPtr(100), 0, 90)
		err := e.CheckMessageSend(context.Background(), "u", 11)
		le, ok := IsLimitExceeded(err)
		if !ok {
			t.Fatalf("90+11 > 100 must block, got %v", err)
		}
		if le.Resource != "messages_day" {
			t.Errorf("Resource = %q, want messages_day", le.Resource)
		}
		if le.Limit != 100 || le.Current != 90 {
			t.Errorf("Limit/Current = %d/%d, want 100/90", le.Limit, le.Current)
		}
	})
	t.Run("zero_hard_blocks", func(t *testing.T) {
		e, _ := newE(intPtr(0), 0, 0)
		if _, ok := IsLimitExceeded(e.CheckMessageSend(context.Background(), "u", 1)); !ok {
			t.Fatal("cap 0 must block the first unit")
		}
	})
	t.Run("monthly_takes_precedence", func(t *testing.T) {
		e, _ := newE(intPtr(0), 1000, 0) // both exhausted
		err := e.CheckMessageSend(context.Background(), "u", 1)
		le, ok := IsLimitExceeded(err)
		if !ok || le.Resource != "messages_month" {
			t.Fatalf("want messages_month precedence, got %v", err)
		}
	})
	t.Run("day_counter_error_propagates", func(t *testing.T) {
		e, c := newE(intPtr(100), 0, 0)
		c.messagesDayErr = errors.New("day counter down")
		if err := e.CheckMessageSend(context.Background(), "u", 1); err == nil {
			t.Fatal("day-counter error must propagate")
		}
	})
	t.Run("rowless_defaults_carry_day_cap", func(t *testing.T) {
		d := defaultsForTest()
		d.MaxMessagesDay = intPtr(3)
		counter := &fakeCounter{messagesToday: 3}
		e := newEnforcerWithReader(&fakeStore{found: false}, counter, d, 0)
		err := e.CheckMessageSend(context.Background(), "u", 1)
		le, ok := IsLimitExceeded(err)
		if !ok || le.Resource != "messages_day" {
			t.Fatalf("row-less default daily cap must enforce, got %v", err)
		}
	})
}
