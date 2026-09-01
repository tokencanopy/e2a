package usage_test

import (
	"context"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

// TestIncrementUsageSummary_Units pins multi-unit accumulation: one call
// with units=3 must land exactly 3 in the direction column and total.
// Expected values are the hand-summed unit counts, independent of the
// SQL's own arithmetic.
func TestIncrementUsageSummary_Units(t *testing.T) {
	pool := testutil.TestDB(t)
	store := usage.NewStore(pool)
	idStore := identity.NewStore(pool)
	ctx := context.Background()
	bucketDate := "2026-04-02"

	user, err := idStore.CreateOrGetUser(ctx, "units-test@example.com", "Units", "google-units-test")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	// outbound 3 + outbound 2 = 5; inbound 1 → total 6.
	if err := store.IncrementUsageSummary(ctx, user.ID, bucketDate, "outbound", 3); err != nil {
		t.Fatalf("outbound 3: %v", err)
	}
	if err := store.IncrementUsageSummary(ctx, user.ID, bucketDate, "outbound", 2); err != nil {
		t.Fatalf("outbound 2: %v", err)
	}
	if err := store.IncrementUsageSummary(ctx, user.ID, bucketDate, "inbound", 1); err != nil {
		t.Fatalf("inbound 1: %v", err)
	}

	sum, err := store.GetUsageSummary(ctx, user.ID, bucketDate)
	if err != nil {
		t.Fatalf("GetUsageSummary: %v", err)
	}
	if sum.OutboundCount != 5 {
		t.Errorf("OutboundCount = %d, want 5", sum.OutboundCount)
	}
	if sum.InboundCount != 1 {
		t.Errorf("InboundCount = %d, want 1", sum.InboundCount)
	}
	if sum.TotalCount != 6 {
		t.Errorf("TotalCount = %d, want 6", sum.TotalCount)
	}

	// units < 1 is normalized to 1, never 0 or negative.
	if err := store.IncrementUsageSummary(ctx, user.ID, bucketDate, "outbound", 0); err != nil {
		t.Fatalf("outbound 0: %v", err)
	}
	sum, err = store.GetUsageSummary(ctx, user.ID, bucketDate)
	if err != nil {
		t.Fatalf("GetUsageSummary after zero-units: %v", err)
	}
	if sum.OutboundCount != 6 {
		t.Errorf("OutboundCount after zero-units call = %d, want 6 (0 normalized to 1)", sum.OutboundCount)
	}
}

// TestMessagesThisMonth_OutboundOnly pins the metering semantics change:
// inbound recording continues (analytics), but only outbound units consume
// the monthly allowance. With 7 outbound units and 100 inbound units in the
// current month, the metered count must be exactly 7.
func TestMessagesThisMonth_OutboundOnly(t *testing.T) {
	pool := testutil.TestDB(t)
	store := usage.NewStore(pool)
	idStore := identity.NewStore(pool)
	ctx := context.Background()

	user, err := idStore.CreateOrGetUser(ctx, "outbound-only@example.com", "OutboundOnly", "google-outbound-only")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	today := usage.CurrentDate()
	if err := store.IncrementUsageSummary(ctx, user.ID, today, "outbound", 7); err != nil {
		t.Fatalf("outbound: %v", err)
	}
	if err := store.IncrementUsageSummary(ctx, user.ID, today, "inbound", 100); err != nil {
		t.Fatalf("inbound: %v", err)
	}

	got, err := store.MessagesThisMonth(ctx, user.ID)
	if err != nil {
		t.Fatalf("MessagesThisMonth: %v", err)
	}
	if got != 7 {
		t.Errorf("MessagesThisMonth = %d, want 7 (inbound must not consume quota)", got)
	}
}

// TestRecordUsageEvent_UnitsColumn pins the auditable per-event units value
// (migration 109): the event row carries the units the tracker recorded.
func TestRecordUsageEvent_UnitsColumn(t *testing.T) {
	pool := testutil.TestDB(t)
	store := usage.NewStore(pool)
	idStore := identity.NewStore(pool)
	ctx := context.Background()

	user, err := idStore.CreateOrGetUser(ctx, "event-units@example.com", "EventUnits", "google-event-units")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	ev := &usage.UsageEvent{UserID: user.ID, AgentID: "agent-units", Domain: "example.com", Direction: "outbound", Units: 4}
	if err := store.RecordUsageEvent(ctx, ev); err != nil {
		t.Fatalf("RecordUsageEvent: %v", err)
	}

	var units int
	if err := pool.QueryRow(ctx, `SELECT units FROM usage_events WHERE id = $1`, ev.ID).Scan(&units); err != nil {
		t.Fatalf("read units: %v", err)
	}
	if units != 4 {
		t.Errorf("usage_events.units = %d, want 4", units)
	}
}

// TestMessagesToday_OutboundOnlyCurrentDay pins the daily counter: only
// today's outbound units count; yesterday's volume and today's inbound are
// invisible to it.
func TestMessagesToday_OutboundOnlyCurrentDay(t *testing.T) {
	pool := testutil.TestDB(t)
	store := usage.NewStore(pool)
	idStore := identity.NewStore(pool)
	ctx := context.Background()

	user, err := idStore.CreateOrGetUser(ctx, "day-count@example.com", "DayCount", "google-day-count")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	if err := store.IncrementUsageSummary(ctx, user.ID, "2020-01-01", "outbound", 500); err != nil {
		t.Fatalf("historic outbound: %v", err)
	}
	today := usage.CurrentDate()
	if err := store.IncrementUsageSummary(ctx, user.ID, today, "outbound", 4); err != nil {
		t.Fatalf("today outbound: %v", err)
	}
	if err := store.IncrementUsageSummary(ctx, user.ID, today, "inbound", 50); err != nil {
		t.Fatalf("today inbound: %v", err)
	}

	got, err := store.MessagesToday(ctx, user.ID)
	if err != nil {
		t.Fatalf("MessagesToday: %v", err)
	}
	if got != 4 {
		t.Errorf("MessagesToday = %d, want 4 (today's outbound only)", got)
	}

	// A user with no rows at all reads 0, no error.
	fresh, err := idStore.CreateOrGetUser(ctx, "day-count-fresh@example.com", "Fresh", "google-day-count-fresh")
	if err != nil {
		t.Fatalf("CreateOrGetUser fresh: %v", err)
	}
	got, err = store.MessagesToday(ctx, fresh.ID)
	if err != nil || got != 0 {
		t.Errorf("fresh MessagesToday = %d, %v; want 0, nil", got, err)
	}
}
