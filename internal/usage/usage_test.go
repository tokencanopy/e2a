package usage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

// -- NoopUsageTracker tests (no DB needed) --

func TestNoopUsageTracker_AlwaysAllows(t *testing.T) {
	tracker := usage.NewNoopUsageTracker()
	ctx := context.Background()

	allowed, err := tracker.RecordAndCheck(ctx, "user1", "agent1", "test.com", "outbound", 1)
	if err != nil || !allowed {
		t.Errorf("RecordAndCheck: allowed=%v, err=%v", allowed, err)
	}
}

func TestLiveUsageTracker_RecordAndCheckTxParticipatesInCallerTransaction(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := usage.NewStore(pool)
	tracker := usage.NewUsageTracker(store)
	identityStore := identity.NewStore(pool)
	user, err := identityStore.CreateOrGetUser(ctx, "usage-tx@example.test", "Usage", "usage-tx")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tracker.RecordAndCheckTx(ctx, tx, user.ID, "agent_usage", "example.test", "outbound", 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE user_id=$1`, user.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled back usage events=%d", count)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tracker.RecordAndCheckTx(ctx, tx, user.ID, "agent_usage", "example.test", "outbound", 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE user_id=$1`, user.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("committed usage events=%d", count)
	}
}

// -- Store tests (DB required) --

func TestUsageSummaryIncrementAndGet(t *testing.T) {
	pool := testutil.TestDB(t)
	store := usage.NewStore(pool)
	idStore := identity.NewStore(pool)

	ctx := context.Background()
	bucketDate := "2026-03-29"

	// Create a real user to satisfy FK constraint
	user, err := idStore.CreateOrGetUser(ctx, "billing-test@example.com", "Billing Test", "google-billing-test")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	// Increment inbound
	if err := store.IncrementUsageSummary(ctx, user.ID, bucketDate, "inbound", 1); err != nil {
		t.Fatalf("IncrementUsageSummary inbound: %v", err)
	}
	// Increment outbound twice
	for i := 0; i < 2; i++ {
		if err := store.IncrementUsageSummary(ctx, user.ID, bucketDate, "outbound", 1); err != nil {
			t.Fatalf("IncrementUsageSummary outbound: %v", err)
		}
	}

	sum, err := store.GetUsageSummary(ctx, user.ID, bucketDate)
	if err != nil {
		t.Fatalf("GetUsageSummary: %v", err)
	}
	if sum.InboundCount != 1 {
		t.Errorf("InboundCount = %d, want 1", sum.InboundCount)
	}
	if sum.OutboundCount != 2 {
		t.Errorf("OutboundCount = %d, want 2", sum.OutboundCount)
	}
	if sum.TotalCount != 3 {
		t.Errorf("TotalCount = %d, want 3", sum.TotalCount)
	}
}

func TestLiveUsageTracker_AlwaysAllows(t *testing.T) {
	pool := testutil.TestDB(t)
	store := usage.NewStore(pool)
	idStore := identity.NewStore(pool)

	tracker := usage.NewUsageTracker(store)
	ctx := context.Background()

	// Create a real user to satisfy FK constraint
	user, err := idStore.CreateOrGetUser(ctx, "billing-tracker@example.com", "Billing Tracker", "google-billing-tracker")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	// Inbound always allowed
	allowed, err := tracker.RecordAndCheck(ctx, user.ID, "agent1", "test.com", "inbound", 1)
	if err != nil || !allowed {
		t.Errorf("inbound should always be allowed: allowed=%v, err=%v", allowed, err)
	}

	// Outbound always allowed (no quota enforcement)
	allowed, err = tracker.RecordAndCheck(ctx, user.ID, "agent1", "test.com", "outbound", 1)
	if err != nil || !allowed {
		t.Errorf("outbound should always be allowed: allowed=%v, err=%v", allowed, err)
	}
}

// TestLiveUsageTracker_SystemClassNotMetered is the load-bearing assertion for
// the metering gate: a system-class account (synthetic probe traffic) writes
// ZERO usage_events and ZERO usage_summaries, so it never accrues quota. A
// standard account in the same test writes both (regression guard that the gate
// only short-circuits non-standard classes).
func TestLiveUsageTracker_SystemClassNotMetered(t *testing.T) {
	pool := testutil.TestDB(t)
	store := usage.NewStore(pool)
	idStore := identity.NewStore(pool)
	tracker := usage.NewUsageTracker(store)
	ctx := context.Background()

	countEvents := func(userID string) int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM usage_events WHERE user_id = $1`, userID).Scan(&n); err != nil {
			t.Fatalf("count usage_events: %v", err)
		}
		return n
	}

	// system-class account: gate short-circuits, no rows written.
	sysUser, err := idStore.CreateOrGetUser(ctx, "probe-system@example.com", "Probe System", "google-probe-system")
	if err != nil {
		t.Fatalf("CreateOrGetUser system: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET account_class = 'system' WHERE id = $1`, sysUser.ID); err != nil {
		t.Fatalf("set system class: %v", err)
	}

	for _, dir := range []string{"inbound", "outbound"} {
		allowed, err := tracker.RecordAndCheck(ctx, sysUser.ID, "agent-sys", "probe.example.com", dir, 1)
		if err != nil || !allowed {
			t.Errorf("system %s RecordAndCheck: allowed=%v err=%v", dir, allowed, err)
		}
	}
	if n := countEvents(sysUser.ID); n != 0 {
		t.Errorf("system account wrote %d usage_events, want 0", n)
	}
	if _, err := store.GetUsageSummary(ctx, sysUser.ID, usage.CurrentDate()); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("system account usage_summaries lookup err = %v, want ErrNoRows (no summary written)", err)
	}

	// standard-class account (regression): gate does NOT short-circuit.
	stdUser, err := idStore.CreateOrGetUser(ctx, "probe-standard@example.com", "Probe Standard", "google-probe-standard")
	if err != nil {
		t.Fatalf("CreateOrGetUser standard: %v", err)
	}
	for _, dir := range []string{"inbound", "outbound"} {
		if _, err := tracker.RecordAndCheck(ctx, stdUser.ID, "agent-std", "real.example.com", dir, 1); err != nil {
			t.Fatalf("standard %s RecordAndCheck: %v", dir, err)
		}
	}
	if n := countEvents(stdUser.ID); n != 2 {
		t.Errorf("standard account wrote %d usage_events, want 2", n)
	}
	sum, err := store.GetUsageSummary(ctx, stdUser.ID, usage.CurrentDate())
	if err != nil {
		t.Fatalf("standard GetUsageSummary: %v", err)
	}
	if sum.TotalCount != 2 {
		t.Errorf("standard account summary TotalCount = %d, want 2", sum.TotalCount)
	}
}

func TestCurrentDate(t *testing.T) {
	date := usage.CurrentDate()
	if len(date) != 10 { // "2026-03-29" format
		t.Errorf("CurrentDate() = %q, expected YYYY-MM-DD format", date)
	}
	if date[4] != '-' || date[7] != '-' {
		t.Errorf("CurrentDate() = %q, missing dash separators", date)
	}
}
