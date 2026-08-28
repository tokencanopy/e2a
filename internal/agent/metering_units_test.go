package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// TestMarkSent_MetersRecipientUnits drives the REAL terminal send path
// (MarkSent → finalizeSentTx → meterSentTx) against a real row and asserts
// the metered unit count equals the deduplicated to ∪ cc ∪ bcc set — the
// recipient-delivery unit of usage-based pricing v1. The message carries
// to=[a, b], cc=[B (case-dupe of b), c], bcc=[a (dupe)] ⇒ 3 distinct
// recipients, an expectation derived by hand, not by re-running the
// normalizer.
func TestMarkSent_MetersRecipientUnits(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "meter-units@example.com", "Meter", "google-meter-units")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "meter.example.com", user.ID); err != nil {
		t.Fatalf("claim domain: %v", err)
	}
	const agentAddr = "billing@meter.example.com"
	if _, err := store.CreateAgent(ctx, agentAddr, "meter.example.com", "",
		"https://example.com/webhook", "", user.ID); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	msg, err := store.CreateOutboundMessage(ctx, agentAddr,
		[]string{"a@dest.test", "b@dest.test"},
		[]string{"B@dest.test", "c@dest.test"},
		[]string{"a@dest.test"},
		"Units", "send", "smtp", "", "conv-units-1", []byte("raw"))
	if err != nil {
		t.Fatalf("create outbound message: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET delivery_status = 'sending' WHERE id = $1`, msg.ID); err != nil {
		t.Fatalf("claim message: %v", err)
	}

	tracker := usage.NewUsageTracker(usage.NewStore(pool))
	sendStore := agent.NewOutboundSendStore(store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)), tracker)

	sentAt := time.Now().UTC().Truncate(time.Second)
	if err := sendStore.MarkSent(ctx, msg.ID, 0, 0, sentAt, "provider-units-1", ""); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	// The daily summary must carry exactly 3 outbound units for this send.
	var outbound int
	if err := pool.QueryRow(ctx,
		`SELECT outbound_count FROM usage_summaries WHERE user_id = $1 AND bucket_date = $2`,
		user.ID, usage.CurrentDate()).Scan(&outbound); err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if outbound != 3 {
		t.Errorf("outbound_count = %d, want 3 (unique of to+cc+bcc)", outbound)
	}

	// One event row, units=3 — cardinality one-per-message, units auditable.
	var events, units int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(sum(units), 0) FROM usage_events WHERE user_id = $1 AND direction = 'outbound'`,
		user.ID).Scan(&events, &units); err != nil {
		t.Fatalf("read events: %v", err)
	}
	if events != 1 || units != 3 {
		t.Errorf("usage_events: rows=%d unitsSum=%d, want rows=1 unitsSum=3", events, units)
	}
}
