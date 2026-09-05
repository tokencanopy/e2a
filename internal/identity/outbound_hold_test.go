package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// The finite-hold pair rides the claim payload so every worker execution
// re-derives the same deadline, and it is cleared by the terminal write so a
// stale hold can never outlive its message's outcome.
func TestOutboundHoldRidesTheClaimAndClearsOnTerminal(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "hold-claim")

	var userID string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM agent_identities WHERE id = $1`, agentID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	resumed := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	ready := time.Date(2026, 9, 2, 9, 30, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO account_sending_controls (user_id, last_resumed_at, ses_tenant_name, ses_tenant_ready, ses_tenant_ready_at)
		VALUES ($1, $2, 'tenant_hold_test', true, $3)
		ON CONFLICT (user_id) DO UPDATE SET last_resumed_at = $2, ses_tenant_ready = true, ses_tenant_ready_at = $3`,
		userID, resumed, ready,
	); err != nil {
		t.Fatal(err)
	}

	var msgID string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := store.CreateOutboundMessageTx(ctx, tx, agentID,
			[]string{"one@example.test"}, nil, nil, "Hold", "send", "smtp", "", "conv-hold",
			[]byte("From: bot\r\n\r\nbody"), "accepted", "agent@test.e2a.dev", "relay")
		if err != nil {
			return err
		}
		msgID = m.ID
		return store.StampSendJobIDTx(ctx, tx, m.ID, 4242)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p, err := store.ClaimOutboundForSend(ctx, msgID, 4242)
	if err != nil || p == nil {
		t.Fatalf("claim: payload=%v err=%v", p, err)
	}
	if p.LocalHoldClass != "" || p.LocalHoldAnchor != nil {
		t.Fatalf("fresh claim carries a hold: %q %v", p.LocalHoldClass, p.LocalHoldAnchor)
	}
	if p.LastResumedAt == nil || !p.LastResumedAt.Equal(resumed) || p.TenantReadyAt == nil || !p.TenantReadyAt.Equal(ready) {
		t.Fatalf("control timestamps = %v / %v, want %v / %v", p.LastResumedAt, p.TenantReadyAt, resumed, ready)
	}
	if err := store.ReleaseOutboundSendClaim(ctx, msgID, 4242); err != nil {
		t.Fatal(err)
	}

	anchor := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	if err := store.RecordOutboundHold(ctx, msgID, "policy_budget", anchor); err != nil {
		t.Fatalf("record hold: %v", err)
	}
	p, err = store.ClaimOutboundForSend(ctx, msgID, 4242)
	if err != nil || p == nil {
		t.Fatalf("re-claim: payload=%v err=%v", p, err)
	}
	if p.LocalHoldClass != "policy_budget" || p.LocalHoldAnchor == nil || !p.LocalHoldAnchor.Equal(anchor) {
		t.Fatalf("hold on re-claim = %q %v, want policy_budget @ %v", p.LocalHoldClass, p.LocalHoldAnchor, anchor)
	}

	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := store.MarkOutboundSentTx(ctx, tx, msgID, "<ses-hold@example.test>")
		return err
	}); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	var class *string
	var holdAnchor *time.Time
	if err := pool.QueryRow(ctx, `SELECT local_hold_class, local_hold_anchor FROM messages WHERE id = $1`, msgID).Scan(&class, &holdAnchor); err != nil {
		t.Fatal(err)
	}
	if class != nil || holdAnchor != nil {
		t.Fatalf("hold survived the terminal write: %v %v", class, holdAnchor)
	}
	// A terminal row refuses a late hold write.
	if err := store.RecordOutboundHold(ctx, msgID, "policy_budget", anchor); err != nil {
		t.Fatalf("late hold write errored: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT local_hold_class FROM messages WHERE id = $1`, msgID).Scan(&class); err != nil {
		t.Fatal(err)
	}
	if class != nil {
		t.Fatalf("hold written on a sent row: %q", *class)
	}
}

func TestOutboundHoldRejectsAnEmptyPair(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	if err := store.RecordOutboundHold(context.Background(), "msg_none", "", time.Time{}); err == nil {
		t.Fatal("empty class and anchor accepted")
	}
}
