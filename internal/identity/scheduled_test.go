package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func setScheduledAt(t *testing.T, pool *pgxpool.Pool, id string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE messages SET scheduled_at=$2 WHERE id=$1`, id, at); err != nil {
		t.Fatalf("set scheduled_at on %s: %v", id, err)
	}
}

func TestListScheduled_ReturnsAcceptedPendingSendsOverdueFirst(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := seedReviewAgent(t, store, ctx, "sched.example.com")

	// Two accepted outbound sends scheduled for the future. The later one is
	// seeded FIRST to prove ordering is by scheduled_at, not insertion order.
	laterID := seedOutboundRow(t, store, agentID, []string{"a@example.com"}, nil, nil, "later")
	soonerID := seedOutboundRow(t, store, agentID, []string{"b@example.com"}, nil, nil, "sooner")
	setScheduledAt(t, pool, laterID, time.Now().Add(2*time.Hour))
	setScheduledAt(t, pool, soonerID, time.Now().Add(1*time.Hour))

	// Included: an accepted send whose scheduled_at is already past. This is the
	// daily-cap deferral state (outbound_async.go releases the claim, snoozes the
	// job, and leaves scheduled_at stale) — still pending, so it must stay
	// visible rather than vanish. soonest-first sorts it to the TOP as the most
	// overdue.
	pastID := seedOutboundRow(t, store, agentID, []string{"c@example.com"}, nil, nil, "past")
	setScheduledAt(t, pool, pastID, time.Now().Add(-time.Hour))

	// Excluded: accepted but with no schedule (immediate send).
	_ = seedOutboundRow(t, store, agentID, []string{"d@example.com"}, nil, nil, "immediate")

	// Excluded: scheduled but no longer 'accepted' (already sent — the row drops
	// out once the job fires).
	sentID := seedOutboundRow(t, store, agentID, []string{"e@example.com"}, nil, nil, "sent")
	setScheduledAt(t, pool, sentID, time.Now().Add(3*time.Hour))
	if _, err := pool.Exec(ctx, `UPDATE messages SET delivery_status='sent' WHERE id=$1`, sentID); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	items, err := store.ListScheduled(ctx, userID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListScheduled: %v", err)
	}
	got := make([]string, 0, len(items))
	for _, it := range items {
		got = append(got, it.ID)
	}
	want := []string{pastID, soonerID, laterID}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("ListScheduled ids = %v, want overdue-then-soonest %v", got, want)
	}

	// The overdue row sorts first and is a genuine pending send: accepted,
	// outbound, with recipients, and a scheduled_at now in the past.
	first := items[0]
	if first.DeliveryStatus != "accepted" {
		t.Errorf("delivery_status = %q, want accepted", first.DeliveryStatus)
	}
	if first.ScheduledAt == nil || !first.ScheduledAt.Before(time.Now()) {
		t.Errorf("scheduled_at = %v, want a past instant (the overdue row)", first.ScheduledAt)
	}
	if first.Direction != "outbound" {
		t.Errorf("direction = %q, want outbound", first.Direction)
	}
	if len(first.To) == 0 {
		t.Errorf("to = %v, want recipients", first.To)
	}
}

func TestListScheduled_ScopedToUser(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userA, agentA := seedReviewAgent(t, store, ctx, "sched-a.example.com")
	_, agentB := seedReviewAgent(t, store, ctx, "sched-b.example.com")

	mineID := seedOutboundRow(t, store, agentA, []string{"x@example.com"}, nil, nil, "mine")
	setScheduledAt(t, pool, mineID, time.Now().Add(time.Hour))
	theirsID := seedOutboundRow(t, store, agentB, []string{"y@example.com"}, nil, nil, "theirs")
	setScheduledAt(t, pool, theirsID, time.Now().Add(time.Hour))

	items, err := store.ListScheduled(ctx, userA, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListScheduled: %v", err)
	}
	if len(items) != 1 || items[0].ID != mineID {
		t.Fatalf("ListScheduled returned %d items, want only %s", len(items), mineID)
	}
}
