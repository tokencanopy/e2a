package hitlworker_test

import (
	"context"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
)

// TestWorkerAutoApproveSelfSendScreensInboundLeg: a held self-send that the TTL
// sweep auto-approves (hitl_expiration_action=approve) delivers via the local
// loopback path — and that inbound leg must run the agent's inbound protection
// exactly like the relay path would. With inbound gate=allowlist (own address
// not on it) + action=review, the auto-approved message's inbox copy is held
// again as pending_review: hidden, email.review_requested fired,
// email.received suppressed. (Double review is the intended semantics — the
// outbound TTL approval releases the Sent copy; inbound protection then judges
// the inbox copy.)
func TestWorkerAutoApproveSelfSendScreensInboundLeg(t *testing.T) {
	w, store, pool, smtpDone := setupWorker(t)
	ctx := context.Background()

	ag := prepareAgent(t, store, "ttl-screen-self", identity.HITLExpirationApprove)
	owner, err := store.CreateOrGetUser(ctx, "owner-ttl-screen-self@example.com", "Owner", "google-ttl-screen-self")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAgentProtection(ctx, ag.ID, owner.ID, identity.ProtectionConfig{
		InboundGatePolicy: "allowlist", InboundAllowlist: []string{"trusted@friend.example.com"}, InboundGateAction: "review",
		InboundScanSensitivity: "off",
		OutboundGatePolicy:     "open", OutboundGateAction: "flag", OutboundScanSensitivity: "off",
		HITLTTLSeconds: identity.HITLDefaultTTLSeconds, HITLExpirationAction: identity.HITLExpirationApprove,
	}); err != nil {
		t.Fatalf("UpdateAgentProtection: %v", err)
	}

	msg, err := store.CreatePendingOutboundMessage(ctx, ag.ID,
		[]string{ag.EmailAddress()}, nil, nil,
		"ttl screened self", "note to self body", "", nil,
		"send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.RunOnce(ctx)

	if msgs := smtpDone(); len(msgs) != 0 {
		t.Fatalf("SMTP should not be hit on a self-send auto-approve, got %d", len(msgs))
	}

	// Outbound leg auto-approved as usual.
	var outStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM messages WHERE id=$1`, msg.ID).Scan(&outStatus); err != nil {
		t.Fatal(err)
	}
	if outStatus != identity.MessageStatusReviewExpiredApproved {
		t.Errorf("outbound status = %q, want review_expired_approved", outStatus)
	}

	// Inbound leg screened → held.
	var inID, inStatus, reviewReason, headerFrom string
	var hasExpiry bool
	if err := pool.QueryRow(ctx,
		`SELECT id, status, COALESCE(review_reason,''), COALESCE(header_from,''), approval_expires_at IS NOT NULL
		   FROM messages WHERE agent_id=$1 AND direction='inbound' AND subject='ttl screened self'`,
		ag.ID).Scan(&inID, &inStatus, &reviewReason, &headerFrom, &hasExpiry); err != nil {
		t.Fatalf("read inbound row: %v", err)
	}
	if inStatus != identity.MessageStatusPendingReview {
		t.Errorf("inbound status = %q, want pending_review (TTL-approve path must screen the loopback leg)", inStatus)
	}
	// Held rows surface header_from in the review queue — it must be the
	// agent's actual email address, not an agent id.
	if headerFrom != ag.EmailAddress() {
		t.Errorf("held row header_from = %q, want the agent address %q", headerFrom, ag.EmailAddress())
	}
	if reviewReason != identity.ReviewReasonSenderGate {
		t.Errorf("review_reason = %q, want %q", reviewReason, identity.ReviewReasonSenderGate)
	}
	if !hasExpiry {
		t.Error("approval_expires_at must be set on the inbound review hold")
	}

	var received, reviewRequested int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE type='email.received'), count(*) FILTER (WHERE type='email.review_requested')
		   FROM webhook_events WHERE message_id=$1`, inID).Scan(&received, &reviewRequested); err != nil {
		t.Fatal(err)
	}
	if received != 0 {
		t.Error("email.received must be suppressed while the inbound leg is held")
	}
	if reviewRequested != 1 {
		t.Errorf("email.review_requested events = %d, want 1", reviewRequested)
	}

	var gateAudit int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM protection_events WHERE message_id=$1 AND source='gate' AND direction='inbound'`,
		inID).Scan(&gateAudit); err != nil {
		t.Fatal(err)
	}
	if gateAudit != 1 {
		t.Errorf("gate protection_events = %d, want 1", gateAudit)
	}

	// Hidden from the inbox until released.
	msgs, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{
		AgentID: ag.ID, Direction: "inbound", Status: "all", Limit: 10,
	})
	if err != nil {
		t.Fatalf("inbox list: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("inbox shows %d messages, want 0 (held message must be hidden)", len(msgs))
	}
}
