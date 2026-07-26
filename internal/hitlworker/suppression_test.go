package hitlworker_test

// TTL auto-approval must never submit to a suppressed recipient. A recipient
// suppressed while the draft was held resolves the expired hold through the
// EXISTING rejected/expired lifecycle (review_expired_rejected + rejection
// reason + review-resolution event plumbing) instead of enqueuing a send; a
// suppression-store error is treated as transient (the row stays
// pending_review for the next sweep) — conservative in both directions, never
// a silent send.

import (
	"context"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
)

// TestWorkerAutoApproveAsync_SuppressedRecipientAutoRejected: an expired
// approve-hold whose CC recipient used a display name and was suppressed with
// case differences after the hold was created is auto-REJECTED with an
// explicit suppression reason — no enqueue, no SMTP.
func TestWorkerAutoApproveAsync_SuppressedRecipientAutoRejected(t *testing.T) {
	w, store, pool, smtpDone := setupWorker(t)
	ctx := context.Background()

	agent := prepareAgent(t, store, "approve-suppressed", identity.HITLExpirationApprove)
	enq := &fakeEnq{}
	w.SetOutboundEnqueuer(enq)

	msg, err := store.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{"alice@external.test"}, []string{"Bob Recipient <bob@external.test>"}, nil,
		"Held", "body", "", nil, "send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	// Suppressed AFTER the hold was created, with different case — the check
	// must extract the stored display-name recipient's canonical addr-spec and
	// match against the exact sending agent's list.
	if _, _, err := store.AddAgentSuppression(ctx, agent.UserID, agent.ID, "BOB@External.Test", "opted out", "unsubscribe", nil); err != nil {
		t.Fatalf("AddAgentSuppression: %v", err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.RunOnce(ctx)

	if len(enq.calls) != 0 {
		t.Fatalf("suppressed auto-approve must NOT enqueue a send, got %v", enq.calls)
	}
	if msgs := smtpDone(); len(msgs) != 0 {
		t.Fatalf("suppressed auto-approve sent %d SMTP messages, want zero", len(msgs))
	}

	var status, reason string
	if err := pool.QueryRow(ctx,
		`SELECT status, COALESCE(rejection_reason,'') FROM messages WHERE id=$1`, msg.ID,
	).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusReviewExpiredRejected {
		t.Errorf("status = %q, want %q (resolved via the existing rejected/expired lifecycle)",
			status, identity.MessageStatusReviewExpiredRejected)
	}
	// The reason is customer-facing (stored on the message row, returned by the
	// HITL API, emitted in email.review_rejected), so it must name the exact
	// suppressed recipient — the owner's own recipient, and ambiguous by domain
	// alone. Log-side redaction lives in autoReject (reason_len only), not here.
	if !strings.Contains(reason, "suppression") || !strings.Contains(reason, "bob@external.test") {
		t.Errorf("rejection_reason = %q, want an explicit suppression reason naming the address", reason)
	}
}

// TestWorkerAutoApproveAsync_OtherTenantSuppressionDoesNotBlock: the sweep's
// check is scoped to the message's owning account — a different tenant
// suppressing the same address must not affect this send.
func TestWorkerAutoApproveAsync_OtherTenantSuppressionDoesNotBlock(t *testing.T) {
	w, store, pool, _ := setupWorker(t)
	ctx := context.Background()

	agent := prepareAgent(t, store, "approve-supp-tenant", identity.HITLExpirationApprove)
	other := prepareAgent(t, store, "approve-supp-other", identity.HITLExpirationApprove)
	enq := &fakeEnq{}
	w.SetOutboundEnqueuer(enq)

	if _, err := store.AddSuppression(ctx, other.UserID, "alice@external.test", "manual", "manual", ""); err != nil {
		t.Fatalf("AddSuppression(other tenant): %v", err)
	}

	msg, err := store.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{"alice@external.test"}, nil, nil,
		"Held", "body", "", nil, "send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.RunOnce(ctx)

	if len(enq.calls) != 1 || enq.calls[0] != msg.ID {
		t.Fatalf("EnqueueSendTx calls = %v, want [%s] (other tenant's suppression must not block)", enq.calls, msg.ID)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM messages WHERE id=$1`, msg.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusReviewExpiredApproved {
		t.Errorf("status = %q, want %q", status, identity.MessageStatusReviewExpiredApproved)
	}
}
