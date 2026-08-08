package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// These tests pin the paused-clock invariant the stable trash contract
// documents: a review hold whose agent sits in the trash must NEVER
// auto-resolve — not by the TTL sweep's reject arm (which scrubs the draft
// body unrecoverably), not by its approve arm, and not via a stale
// human-approve. The guard must be atomic with the resolve CAS itself:
// the sweep's candidate SELECT (ListExpiredPending / ListExpiredReviews)
// filters trashed agents, but an agent can be trashed between that SELECT
// and the per-row resolve (TOCTOU), so each resolve UPDATE re-checks.

// pendingHoldRow reads back the columns the guard protects.
func pendingHoldRow(t *testing.T, pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, msgID string) (status string, bodyText *string, approvalExpiresAt *time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT status, body_text, approval_expires_at FROM messages WHERE id=$1`, msgID,
	).Scan(&status, &bodyText, &approvalExpiresAt); err != nil {
		t.Fatalf("read back hold row: %v", err)
	}
	return status, bodyText, approvalExpiresAt
}

// TestExpireRejectSkipsTrashedAgentHold: the reject arm of the TTL sweep is
// the destructive one (it NULLs body_text/body_html/attachments_json), so a
// trashed agent's expired hold must survive it byte-intact.
func TestExpireRejectSkipsTrashedAgentHold(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, a := setupPendingAgent(t, store, "trash-guard-rej")
	msg, err := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"x@example.com"}, nil, nil, "held subject", "held body", "", nil,
		"send", "", "", "", 60)
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}
	// Expired hold — exactly what the sweep would pick up…
	if _, err := pool.Exec(ctx, `UPDATE messages SET approval_expires_at = now() - interval '10 minutes' WHERE id=$1`, msg.ID); err != nil {
		t.Fatal(err)
	}
	// …but the agent is trashed between the sweep's SELECT and the resolve.
	if err := store.SoftDeleteAgent(ctx, a.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	if _, err := store.ExpireReject(ctx, msg.ID, "ttl_expired"); !errors.Is(err, identity.ErrNotPendingApproval) {
		t.Fatalf("ExpireReject on trashed agent's hold = %v, want ErrNotPendingApproval (no-op)", err)
	}
	status, body, _ := pendingHoldRow(t, pool, msg.ID)
	if status != identity.MessageStatusPendingReview {
		t.Errorf("status = %q, want pending_review (hold must survive the trash)", status)
	}
	if body == nil || *body != "held body" {
		t.Errorf("body_text = %v, want intact draft body", body)
	}

	// Sanity: the sweep's candidate list also excludes it while trashed.
	cands, err := store.ListExpiredPending(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.MessageID == msg.ID {
			t.Error("ListExpiredPending must exclude a trashed agent's hold")
		}
	}
}

// TestApproveAndAcceptSkipsTrashedAgentHold: the approve arm (shared by the
// TTL sweep's auto-approve and the human/magic-link async approve) must not
// resolve+enqueue a trashed agent's hold — otherwise the SendWorker's own
// trash check cancels it into a spurious terminal failure.
func TestApproveAndAcceptSkipsTrashedAgentHold(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, a := setupPendingAgent(t, store, "trash-guard-app")
	msg, err := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"x@example.com"}, nil, nil, "held subject", "held body", "", nil,
		"send", "", "", "", 60)
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET approval_expires_at = now() - interval '10 minutes' WHERE id=$1`, msg.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDeleteAgent(ctx, a.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	enqueued := 0
	enqueue := func(_ context.Context, _ pgx.Tx, _ string) (int64, error) {
		enqueued++
		return 1, nil
	}
	acc := identity.AcceptedSend{
		To: []string{"x@example.com"}, Subject: "held subject", Method: "smtp",
		EnvelopeFrom: "bot@trash-guard-app.example.com", SentAs: "relay", Raw: []byte("raw"),
	}
	_, err = store.ApproveAndAccept(ctx, msg.ID, "", identity.MessageStatusReviewExpiredApproved, false, acc, enqueue, nil, nil)
	if !errors.Is(err, identity.ErrNotPendingApproval) {
		t.Fatalf("ApproveAndAccept on trashed agent's hold = %v, want ErrNotPendingApproval (no-op)", err)
	}
	if enqueued != 0 {
		t.Errorf("enqueue called %d times, want 0 — no send job may exist for a trashed agent's hold", enqueued)
	}
	status, body, _ := pendingHoldRow(t, pool, msg.ID)
	if status != identity.MessageStatusPendingReview {
		t.Errorf("status = %q, want pending_review", status)
	}
	if body == nil || *body != "held body" {
		t.Errorf("body_text = %v, want intact draft body", body)
	}
}

// TestExpireApproveAndSendSkipsTrashedAgentHold covers the synchronous
// expire-approve path: its locked SELECT must not hand a trashed agent's
// draft to the send callback.
func TestExpireApproveAndSendSkipsTrashedAgentHold(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, a := setupPendingAgent(t, store, "trash-guard-sync")
	msg, err := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"x@example.com"}, nil, nil, "held subject", "held body", "", nil,
		"send", "", "", "", 60)
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET approval_expires_at = now() - interval '10 minutes' WHERE id=$1`, msg.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDeleteAgent(ctx, a.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	_, err = store.ExpireApproveAndSend(ctx, msg.ID, func(_ *identity.Message) (identity.SendResult, error) {
		t.Error("send callback must not run for a trashed agent's hold")
		return identity.SendResult{}, errors.New("must not send")
	})
	if !errors.Is(err, identity.ErrNotPendingApproval) {
		t.Fatalf("ExpireApproveAndSend on trashed agent's hold = %v, want ErrNotPendingApproval", err)
	}
	status, _, _ := pendingHoldRow(t, pool, msg.ID)
	if status != identity.MessageStatusPendingReview {
		t.Errorf("status = %q, want pending_review", status)
	}
}

// TestInboundExpireArmsSkipTrashedAgentHold: the inbound TTL arms
// (ExpireApproveReview / ExpireRejectReview) share the same TOCTOU window
// against ListExpiredReviews and must no-op on a trashed agent's hold.
func TestInboundExpireArmsSkipTrashedAgentHold(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := seedReviewAgent(t, store, ctx, "trash-guard-in.example.com")

	past := time.Now().Add(-10 * time.Minute)
	heldApprove := createInbound(t, store, ctx, agentID, "s@x.com", "held-a", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ScanAction: "review",
		ReviewReason: identity.ReviewReasonInboundScan, ApprovalExpiresAt: &past,
	})
	heldReject := createInbound(t, store, ctx, agentID, "s@x.com", "held-r", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ScanAction: "review",
		ReviewReason: identity.ReviewReasonInboundScan, ApprovalExpiresAt: &past,
	})
	if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	if err := store.ExpireApproveReview(ctx, heldApprove); !errors.Is(err, identity.ErrNotPendingReview) {
		t.Errorf("ExpireApproveReview on trashed agent's hold = %v, want ErrNotPendingReview", err)
	}
	if err := store.ExpireRejectReview(ctx, heldReject, "ttl_expired"); !errors.Is(err, identity.ErrNotPendingReview) {
		t.Errorf("ExpireRejectReview on trashed agent's hold = %v, want ErrNotPendingReview", err)
	}
	for _, id := range []string{heldApprove, heldReject} {
		status, _, _ := pendingHoldRow(t, pool, id)
		if status != identity.MessageStatusPendingReview {
			t.Errorf("inbound hold %s status = %q, want pending_review", id, status)
		}
	}
}

// TestRestoreAgentShiftsHoldClockAndKeepsHoldPending pins the restore half
// of the invariant: an agent restored after time in the trash gets its held
// drafts' approval_expires_at shifted forward by exactly the trashed
// interval, so a hold that had review time left cannot fire the instant the
// agent returns — it re-enters the queue with its remaining TTL.
func TestRestoreAgentShiftsHoldClockAndKeepsHoldPending(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, a := setupPendingAgent(t, store, "trash-guard-res")
	msg, err := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"x@example.com"}, nil, nil, "held subject", "held body", "", nil,
		"send", "", "", "", 300) // 5 minutes of review time left
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, a.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}
	// Simulate that the trashing happened 10 minutes ago with 5 minutes of
	// review TTL left at that moment: original expiry = trash time + 5m =
	// now - 5m. Without the restore shift the hold would come back 5
	// minutes PAST its TTL and the next sweep would fire it immediately.
	if _, err := pool.Exec(ctx,
		`UPDATE agent_identities SET deleted_at = now() - interval '10 minutes' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET approval_expires_at = now() - interval '5 minutes' WHERE id=$1`, msg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RestoreAgent(ctx, a.ID, user.ID); err != nil {
		t.Fatalf("RestoreAgent: %v", err)
	}

	status, body, expiresAt := pendingHoldRow(t, pool, msg.ID)
	if status != identity.MessageStatusPendingReview {
		t.Fatalf("status after restore = %q, want pending_review", status)
	}
	if body == nil || *body != "held body" {
		t.Errorf("body_text after restore = %v, want intact draft body", body)
	}
	if expiresAt == nil {
		t.Fatal("approval_expires_at is NULL after restore")
	}
	// ~5 minutes of TTL remained at trash time; the shift must return it.
	remaining := time.Until(*expiresAt)
	if remaining < 4*time.Minute || remaining > 6*time.Minute {
		t.Errorf("approval_expires_at remaining = %v, want ≈5m (clock resumed where it stopped)", remaining)
	}
	// Not expired → the sweep must not list it.
	cands, err := store.ListExpiredPending(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.MessageID == msg.ID {
			t.Error("restored hold with remaining TTL must not appear in ListExpiredPending")
		}
	}
}
