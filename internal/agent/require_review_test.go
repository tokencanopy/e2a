package agent_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// TestDeliverOutbound_RequireReviewHoldsEverySend is the #989 regression: with
// require_review set on a permissive open gate — every recipient matches, so no
// non-match ever fires — the send is still held for review. Before the fix the
// only config that held everything was an allowlist with an empty list, which
// reached the same state by making "nothing matched" mean "we decided to hold".
func TestDeliverOutbound_RequireReviewHoldsEverySend(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "requirereview")

	if _, err := store.UpdateAgentProtection(ctx, ag.ID, user.ID, identity.ProtectionConfig{
		InboundGatePolicy:       "open",
		InboundGateAction:       "flag",
		InboundScanSensitivity:  identity.SensitivityOff,
		OutboundGatePolicy:      "open", // permissive: every recipient matches
		OutboundGateAction:      "flag", // a real non-match would only annotate
		OutboundRequireReview:   true,
		OutboundScanSensitivity: identity.SensitivityOff,
		HITLTTLSeconds:          3600,
		HITLExpirationAction:    "approve",
	}); err != nil {
		t.Fatalf("UpdateAgentProtection: %v", err)
	}
	ag, err := store.GetAgentByID(ctx, ag.ID)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "hold every send", Body: "b",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	if res == nil || !res.Held {
		t.Fatalf("result = %+v, want a held result", res)
	}

	var status, reason string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, COALESCE(review_reason, '') FROM messages WHERE id=$1`, res.PendingMessageID).Scan(&status, &reason)
	}); err != nil {
		t.Fatalf("read held row: %v", err)
	}
	if status != identity.MessageStatusPendingReview {
		t.Errorf("held row status = %q, want %q", status, identity.MessageStatusPendingReview)
	}
	if reason != identity.ReviewReasonRecipientGate {
		t.Errorf("held row review_reason = %q, want %q", reason, identity.ReviewReasonRecipientGate)
	}

	// The gate audit row records the action that actually applied (review), not
	// the configured non-match action (flag).
	var action string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT action FROM protection_events WHERE message_id=$1 AND source='gate'`, res.PendingMessageID).Scan(&action)
	}); err != nil {
		t.Fatalf("read gate audit row: %v", err)
	}
	if action != "review" {
		t.Errorf("gate audit action = %q, want review", action)
	}
}
