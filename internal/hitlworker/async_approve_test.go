package hitlworker_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outboundsend"
)

// fakeEnq records EnqueueSendTx / EnqueueScheduledSendTx calls (the outbound_send
// enqueue) so the async branch can be asserted without a real River client.
// scheduledCalls captures the re-armed schedule instant for #815 assertions.
type fakeEnq struct {
	calls          []string
	scheduledCalls map[string]time.Time
	err            error
}

func (f *fakeEnq) EnqueueSendTx(_ context.Context, _ pgx.Tx, messageID string) (int64, error) {
	f.calls = append(f.calls, messageID)
	if f.err != nil {
		return 0, f.err
	}
	return 7777, nil
}

func (f *fakeEnq) EnqueueScheduledSendTx(_ context.Context, _ pgx.Tx, messageID string, at time.Time) (int64, error) {
	f.calls = append(f.calls, messageID)
	if f.scheduledCalls == nil {
		f.scheduledCalls = map[string]time.Time{}
	}
	f.scheduledCalls[messageID] = at
	return 7778, nil
}

// TestWorkerAutoApproveAsync: with an outbound enqueuer wired (async mode), an
// expired approve-hold to an EXTERNAL recipient is transitioned to
// review_expired_approved + delivery_status='accepted', an outbound_send job is
// enqueued (+ send_job_id stamped), and NO inline SMTP send happens — the
// SendWorker owns the submit.
func TestWorkerAutoApproveAsync(t *testing.T) {
	w, store, pool, smtpDone := setupWorker(t)
	ctx := context.Background()

	agent := prepareAgent(t, store, "approve-async", identity.HITLExpirationApprove)
	enq := &fakeEnq{}
	w.SetOutboundEnqueuer(enq)

	msg, err := store.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{"alice@external.test"}, nil, nil,
		"Held", "body", "<p>html</p>", nil, "send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.RunOnce(ctx)

	if msgs := smtpDone(); len(msgs) != 0 {
		t.Fatalf("async approve must NOT send inline, got %d SMTP messages", len(msgs))
	}
	if len(enq.calls) != 1 || enq.calls[0] != msg.ID {
		t.Fatalf("EnqueueSendTx calls = %v, want [%s]", enq.calls, msg.ID)
	}

	var status, deliveryStatus string
	var sendJobID *int64
	if err := pool.QueryRow(ctx,
		`SELECT status, COALESCE(delivery_status,''), send_job_id FROM messages WHERE id=$1`, msg.ID,
	).Scan(&status, &deliveryStatus, &sendJobID); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusReviewExpiredApproved {
		t.Errorf("status = %q, want %q", status, identity.MessageStatusReviewExpiredApproved)
	}
	if deliveryStatus != "accepted" {
		t.Errorf("delivery_status = %q, want accepted", deliveryStatus)
	}
	if sendJobID == nil || *sendJobID != 7777 {
		t.Errorf("send_job_id = %v, want 7777", sendJobID)
	}
}

// TestWorkerAutoApproveAsync_ReArmsFutureSchedule: a TTL auto-approve of a hold
// that carried a still-future send_at re-arms the schedule rather than sending
// immediately — the send is enqueued on the scheduled arm at scheduled_at (#815).
func TestWorkerAutoApproveAsync_ReArmsFutureSchedule(t *testing.T) {
	w, store, pool, smtpDone := setupWorker(t)
	ctx := context.Background()

	agent := prepareAgent(t, store, "approve-async-sched", identity.HITLExpirationApprove)
	enq := &fakeEnq{}
	w.SetOutboundEnqueuer(enq)

	msg, err := store.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{"alice@external.test"}, nil, nil,
		"Held + scheduled", "body", "", nil, "send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return store.StampScheduledAtTx(ctx, tx, msg.ID, at)
	}); err != nil {
		t.Fatalf("StampScheduledAtTx: %v", err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.RunOnce(ctx)

	if msgs := smtpDone(); len(msgs) != 0 {
		t.Fatalf("async approve must NOT send inline, got %d SMTP messages", len(msgs))
	}
	got, ok := enq.scheduledCalls[msg.ID]
	if !ok {
		t.Fatalf("message was not enqueued on the scheduled arm; scheduledCalls=%v calls=%v", enq.scheduledCalls, enq.calls)
	}
	if !got.Equal(at) {
		t.Fatalf("re-armed ScheduledAt = %v, want %v", got, at)
	}
}

// TestWorkerAutoApproveAsync_SelfSendStaysLoopback: even with the enqueuer wired,
// a self-send (single To == the agent's own address) must NOT be enqueued onto
// QueueOutbound (the relay would strip the address). It falls through to the sync
// loopback path and resolves to review_expired_approved.
func TestWorkerAutoApproveAsync_SelfSendStaysLoopback(t *testing.T) {
	w, store, pool, smtpDone := setupWorker(t)
	ctx := context.Background()

	agent := prepareAgent(t, store, "selfsend-async", identity.HITLExpirationApprove)
	enq := &fakeEnq{}
	w.SetOutboundEnqueuer(enq)

	msg, err := store.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{agent.EmailAddress()}, nil, nil,
		"To myself", "body", "", nil, "send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.RunOnce(ctx)

	if len(enq.calls) != 0 {
		t.Errorf("self-send must NOT enqueue onto QueueOutbound, got calls %v", enq.calls)
	}
	if msgs := smtpDone(); len(msgs) != 0 {
		t.Errorf("self-send delivers via loopback, not SMTP, got %d messages", len(msgs))
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM messages WHERE id=$1`, msg.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusReviewExpiredApproved {
		t.Errorf("self-send status = %q, want %q (resolved via loopback)", status, identity.MessageStatusReviewExpiredApproved)
	}
}

// TestWorkerAutoApprovePausedAccountDefersWithoutBlocking: a TTL-expired hold on
// an account paused for sending stays pending — held, as the pause promises —
// but its TTL is pushed forward so it does not sit at the head of the sweep and
// starve every other expired review, and it is not retried every cycle.
func TestWorkerAutoApprovePausedAccountDefersWithoutBlocking(t *testing.T) {
	w, store, pool, smtpDone := setupWorker(t)
	ctx := context.Background()
	agent := prepareAgent(t, store, "approve-paused", identity.HITLExpirationApprove)
	enq := &fakeEnq{err: outboundsend.ErrSendingPaused}
	w.SetOutboundEnqueuer(enq)
	msg, err := store.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{"alice@external.test"}, nil, nil,
		"Held", "body", "<p>html</p>", nil, "send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.RunOnce(ctx)
	if msgs := smtpDone(); len(msgs) != 0 {
		t.Fatalf("paused account must not send inline, got %d SMTP messages", len(msgs))
	}
	if len(enq.calls) != 1 {
		t.Fatalf("enqueue attempts = %v, want exactly one", enq.calls)
	}
	var status string
	var expiresAt time.Time
	if err := pool.QueryRow(ctx, `SELECT status, approval_expires_at FROM messages WHERE id=$1`, msg.ID).Scan(&status, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusPendingReview {
		t.Fatalf("status = %q, want pending_review (held, not rejected)", status)
	}
	if expiresAt.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("approval_expires_at = %v, want deferred about an hour ahead", expiresAt)
	}
	// Deferred out of the window: the next sweep leaves it alone.
	w.RunOnce(ctx)
	if len(enq.calls) != 1 {
		t.Fatalf("enqueue attempts after deferral = %v, want still one", enq.calls)
	}
}
