package identity_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// ClaimSendAttempt low-level: covers the four state transitions
// (fresh, in-flight, already-sent, failed-takeover) without going
// through the full ApproveAndSend wrapper.

func TestClaimSendAttempt_FreshAcquires(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	_, a := setupPendingAgent(t, store, "claim-fresh")
	msg, _ := store.CreatePendingOutboundMessage(context.Background(), a.ID,
		[]string{"x@example.com"}, nil, nil, "x", "b", "", nil, "send", "", "", "", 3600)

	res, err := store.ClaimSendAttempt(context.Background(), msg.ID)
	if err != nil {
		t.Fatalf("ClaimSendAttempt: %v", err)
	}
	if res.Outcome != identity.SendAttemptAcquired {
		t.Errorf("Outcome = %d, want SendAttemptAcquired", res.Outcome)
	}
}

func TestClaimSendAttempt_InFlightOnSecondCall(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	_, a := setupPendingAgent(t, store, "claim-inflight")
	msg, _ := store.CreatePendingOutboundMessage(context.Background(), a.ID,
		[]string{"x@example.com"}, nil, nil, "x", "b", "", nil, "send", "", "", "", 3600)

	first, _ := store.ClaimSendAttempt(context.Background(), msg.ID)
	if first.Outcome != identity.SendAttemptAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}

	second, err := store.ClaimSendAttempt(context.Background(), msg.ID)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if second.Outcome != identity.SendAttemptInFlight {
		t.Errorf("Outcome = %d, want SendAttemptInFlight", second.Outcome)
	}
}

func TestClaimSendAttempt_AlreadySentReplaysCachedResult(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	_, a := setupPendingAgent(t, store, "claim-sent")
	msg, _ := store.CreatePendingOutboundMessage(context.Background(), a.ID,
		[]string{"alice@example.com"}, []string{"bob@example.com"}, nil,
		"x", "b", "", nil, "send", "", "", "", 3600)

	first, _ := store.ClaimSendAttempt(context.Background(), msg.ID)
	if first.Outcome != identity.SendAttemptAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}
	want := identity.SendResult{
		ProviderMessageID: "<ses-xyz@amazonses.com>",
		Method:            "smtp",
		To:                []string{"alice@example.com"},
		CC:                []string{"bob@example.com"},
		BCC:               []string{},
	}
	if err := store.MarkSendSucceeded(context.Background(), msg.ID, want); err != nil {
		t.Fatalf("MarkSendSucceeded: %v", err)
	}

	second, err := store.ClaimSendAttempt(context.Background(), msg.ID)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if second.Outcome != identity.SendAttemptAlreadySent {
		t.Fatalf("Outcome = %d, want SendAttemptAlreadySent", second.Outcome)
	}
	if second.Sent.ProviderMessageID != want.ProviderMessageID {
		t.Errorf("provider id = %q, want %q", second.Sent.ProviderMessageID, want.ProviderMessageID)
	}
	if second.Sent.Method != want.Method {
		t.Errorf("method = %q, want %q", second.Sent.Method, want.Method)
	}
	if len(second.Sent.To) != 1 || second.Sent.To[0] != "alice@example.com" {
		t.Errorf("To = %v, want [alice@example.com]", second.Sent.To)
	}
}

func TestClaimSendAttempt_FailedAllowsRetry(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	_, a := setupPendingAgent(t, store, "claim-failed")
	msg, _ := store.CreatePendingOutboundMessage(context.Background(), a.ID,
		[]string{"x@example.com"}, nil, nil, "x", "b", "", nil, "send", "", "", "", 3600)

	first, _ := store.ClaimSendAttempt(context.Background(), msg.ID)
	if first.Outcome != identity.SendAttemptAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}
	if err := store.MarkSendFailed(context.Background(), msg.ID, "ses timeout"); err != nil {
		t.Fatalf("MarkSendFailed: %v", err)
	}

	// After failure, the next call may take over.
	second, err := store.ClaimSendAttempt(context.Background(), msg.ID)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if second.Outcome != identity.SendAttemptAcquired {
		t.Errorf("Outcome = %d, want SendAttemptAcquired (failed should be reclaimable)", second.Outcome)
	}
}

func TestClaimSendAttempt_ConcurrentOnlyOneAcquires(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	_, a := setupPendingAgent(t, store, "claim-conc")
	msg, _ := store.CreatePendingOutboundMessage(context.Background(), a.ID,
		[]string{"x@example.com"}, nil, nil, "x", "b", "", nil, "send", "", "", "", 3600)

	const N = 20
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		acquired int
		inflight int
		otherCnt int
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			res, err := store.ClaimSendAttempt(context.Background(), msg.ID)
			if err != nil {
				t.Errorf("Claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch res.Outcome {
			case identity.SendAttemptAcquired:
				acquired++
			case identity.SendAttemptInFlight:
				inflight++
			default:
				otherCnt++
			}
		}()
	}
	wg.Wait()

	if acquired != 1 {
		t.Errorf("acquired = %d, want exactly 1 of %d concurrent ClaimSendAttempt calls", acquired, N)
	}
	if otherCnt != 0 {
		t.Errorf("unexpected outcomes = %d", otherCnt)
	}
	if acquired+inflight != N {
		t.Errorf("acquired(%d)+inflight(%d) != N(%d)", acquired, inflight, N)
	}
}

// ApproveAndSend-level: the integration scenarios that motivated
// send_attempts.

// Simulates the documented crash window: send() succeeded at SES,
// the approval-tx UPDATE/COMMIT then "rolled back" (we mimic this by
// directly resetting messages.status back to pending_approval and
// clearing the provider_message_id). The next ApproveAndSend call
// must NOT re-invoke send() and must finalize the message row with
// the originally-recorded SendResult.
func TestApproveAndSend_TxRollbackRetry_DoesNotCallSendTwice(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, a := setupPendingAgent(t, store, "approve-rollback")
	msg, _ := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"alice@example.com"}, nil, nil, "x", "body", "", nil, "send", "", "", "", 3600)

	var sendCalls int32

	// First call — succeeds end-to-end.
	_, err := store.ApproveAndSend(ctx, msg.ID, user.ID, identity.PendingApprovalEdit{},
		func(m *identity.Message) (identity.SendResult, error) {
			atomic.AddInt32(&sendCalls, 1)
			return identity.SendResult{
				ProviderMessageID: "<ses-orig@amazonses.com>",
				Method:            "smtp",
				To:                m.ToRecipients,
			}, nil
		})
	if err != nil {
		t.Fatalf("first ApproveAndSend: %v", err)
	}

	// Simulate the surrounding approval-tx having rolled back AFTER
	// SES accepted the send. The send_attempts row stays
	// status='sent' (this is what the new exactly-once gate
	// preserves); the messages row goes back to pending_approval
	// with the provider id cleared.
	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET status = 'pending_review',
		        provider_message_id = '',
		        reviewed_at = NULL,
		        reviewed_by_user_id = NULL,
		        body_text = 'body',
		        body_html = ''
		  WHERE id = $1`, msg.ID); err != nil {
		t.Fatalf("simulate rollback: %v", err)
	}

	// Second call — must reuse the cached result, not re-invoke send().
	second, err := store.ApproveAndSend(ctx, msg.ID, user.ID, identity.PendingApprovalEdit{},
		func(m *identity.Message) (identity.SendResult, error) {
			atomic.AddInt32(&sendCalls, 1)
			return identity.SendResult{}, errors.New("send should not have been called on retry")
		})
	if err != nil {
		t.Fatalf("retry ApproveAndSend: %v", err)
	}
	if got := atomic.LoadInt32(&sendCalls); got != 1 {
		t.Errorf("send() invoked %d time(s), want exactly 1 across the rollback + retry", got)
	}
	if second.ProviderMessageID != "<ses-orig@amazonses.com>" {
		t.Errorf("retry returned ProviderMessageID = %q, want the cached <ses-orig@amazonses.com>", second.ProviderMessageID)
	}
	if second.Status != identity.MessageStatusSent {
		t.Errorf("retry returned status = %q, want sent", second.Status)
	}
}

func TestApproveAndSend_SendErrorMarksFailedAndAllowsRetry(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, a := setupPendingAgent(t, store, "approve-failed")
	msg, _ := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"alice@example.com"}, nil, nil, "x", "body", "", nil, "send", "", "", "", 3600)

	var sendCalls int32

	// First call — send() returns error. ApproveAndSend should bubble
	// the error and mark send_attempts.status='failed'.
	_, err := store.ApproveAndSend(ctx, msg.ID, user.ID, identity.PendingApprovalEdit{},
		func(m *identity.Message) (identity.SendResult, error) {
			atomic.AddInt32(&sendCalls, 1)
			return identity.SendResult{}, errors.New("ses temporarily unavailable")
		})
	if err == nil {
		t.Fatalf("first ApproveAndSend: nil error, want send failure")
	}

	// Status row must NOT have transitioned to sent (the approval tx
	// rolled back).
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM messages WHERE id = $1`, msg.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != identity.MessageStatusPendingReview {
		t.Errorf("messages.status = %q, want pending_approval", status)
	}

	// Second call — send() must be re-invoked (failed attempts allow retry).
	second, err := store.ApproveAndSend(ctx, msg.ID, user.ID, identity.PendingApprovalEdit{},
		func(m *identity.Message) (identity.SendResult, error) {
			atomic.AddInt32(&sendCalls, 1)
			return identity.SendResult{
				ProviderMessageID: "<ses-retry@amazonses.com>",
				Method:            "smtp",
				To:                m.ToRecipients,
			}, nil
		})
	if err != nil {
		t.Fatalf("retry ApproveAndSend: %v", err)
	}
	if got := atomic.LoadInt32(&sendCalls); got != 2 {
		t.Errorf("send() invoked %d time(s), want exactly 2 (one failure + one retry)", got)
	}
	if second.Status != identity.MessageStatusSent {
		t.Errorf("retry status = %q, want sent", second.Status)
	}
	if second.ProviderMessageID != "<ses-retry@amazonses.com>" {
		t.Errorf("retry ProviderMessageID = %q", second.ProviderMessageID)
	}
}

// ExpireApproveAndSend (worker path) — same exactly-once guarantee
// as ApproveAndSend. The polling-loop nature of the auto-expire
// worker makes a missing gate strictly worse than the human-approval
// path: any commit failure after SES success guarantees a re-send on
// the next poll. These tests verify the gate behaves identically on
// the worker side.

// Mirrors TestApproveAndSend_TxRollbackRetry_DoesNotCallSendTwice
// but for the worker-side expiration path.
func TestExpireApproveAndSend_TxRollbackRetry_DoesNotCallSendTwice(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	_, a := setupPendingAgent(t, store, "expire-rollback")
	msg, _ := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"alice@example.com"}, nil, nil, "x", "body", "", nil, "send", "", "", "", 60)
	// ExpireApproveAndSend only picks up rows whose approval_expires_at
	// is in the past — push the expiry back so the WHERE matches.
	if _, err := pool.Exec(ctx, `UPDATE messages SET approval_expires_at = now() - interval '1 minute' WHERE id = $1`, msg.ID); err != nil {
		t.Fatalf("age approval_expires_at: %v", err)
	}

	var sendCalls int32

	// First poll — succeeds end-to-end.
	_, err := store.ExpireApproveAndSend(ctx, msg.ID,
		func(m *identity.Message) (identity.SendResult, error) {
			atomic.AddInt32(&sendCalls, 1)
			return identity.SendResult{
				ProviderMessageID: "<ses-exp-orig@amazonses.com>",
				Method:            "smtp",
				To:                m.ToRecipients,
			}, nil
		})
	if err != nil {
		t.Fatalf("first ExpireApproveAndSend: %v", err)
	}

	// Simulate the surrounding approval-tx having rolled back AFTER
	// SES accepted the send: status goes back to pending_approval,
	// reviewed_at + provider_message_id cleared, approval_expires_at
	// re-aged so the next poll still matches.
	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET status              = 'pending_review',
		        provider_message_id = '',
		        reviewed_at         = NULL,
		        body_text           = 'body',
		        body_html           = '',
		        approval_expires_at = now() - interval '1 minute'
		  WHERE id = $1`, msg.ID); err != nil {
		t.Fatalf("simulate rollback: %v", err)
	}

	// Second poll — must reuse the cached SendResult, NOT re-invoke send().
	second, err := store.ExpireApproveAndSend(ctx, msg.ID,
		func(m *identity.Message) (identity.SendResult, error) {
			atomic.AddInt32(&sendCalls, 1)
			return identity.SendResult{}, errors.New("send should not have been called on retry")
		})
	if err != nil {
		t.Fatalf("retry ExpireApproveAndSend: %v", err)
	}
	if got := atomic.LoadInt32(&sendCalls); got != 1 {
		t.Errorf("send() invoked %d time(s), want exactly 1 across the rollback + retry", got)
	}
	if second.ProviderMessageID != "<ses-exp-orig@amazonses.com>" {
		t.Errorf("retry returned ProviderMessageID = %q, want the cached <ses-exp-orig@amazonses.com>", second.ProviderMessageID)
	}
	if second.Status != identity.MessageStatusReviewExpiredApproved {
		t.Errorf("retry returned status = %q, want expired_approved", second.Status)
	}
}

// ExpireApproveAndSend in the face of a peer-worker mid-send: the
// claim sees status='attempting' and returns ErrSendInProgress. The
// hitlworker loop treats this as "skip silently" so it does NOT
// auto-reject a row that may have actually been sent.
func TestExpireApproveAndSend_InFlightReturnsErrSendInProgress(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	_, a := setupPendingAgent(t, store, "expire-inflight")
	msg, _ := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"alice@example.com"}, nil, nil, "x", "body", "", nil, "send", "", "", "", 60)
	if _, err := pool.Exec(ctx, `UPDATE messages SET approval_expires_at = now() - interval '1 minute' WHERE id = $1`, msg.ID); err != nil {
		t.Fatalf("age approval_expires_at: %v", err)
	}

	// Plant a recent 'attempting' row as if a peer worker had claimed it.
	if _, err := pool.Exec(ctx,
		`INSERT INTO send_attempts (message_id, status, attempted_at)
		 VALUES ($1, 'attempting', now())`, msg.ID); err != nil {
		t.Fatalf("seed inflight row: %v", err)
	}

	var sendCalls int32
	_, err := store.ExpireApproveAndSend(ctx, msg.ID,
		func(m *identity.Message) (identity.SendResult, error) {
			atomic.AddInt32(&sendCalls, 1)
			return identity.SendResult{}, nil
		})
	if !errors.Is(err, identity.ErrSendInProgress) {
		t.Errorf("err = %v, want ErrSendInProgress", err)
	}
	if got := atomic.LoadInt32(&sendCalls); got != 0 {
		t.Errorf("send() invoked %d time(s), want 0 when InFlight", got)
	}
}

// Direct InFlight: force a stale in_progress claim into the table
// (without going through ApproveAndSend) and verify the next call
// surfaces ErrSendInProgress via the wrapper.
func TestApproveAndSend_InFlightReturnsErrSendInProgress(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, a := setupPendingAgent(t, store, "approve-inflight")
	msg, _ := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"alice@example.com"}, nil, nil, "x", "body", "", nil, "send", "", "", "", 3600)

	// Plant a recent 'attempting' row directly so the next
	// ClaimSendAttempt observes InFlight.
	if _, err := pool.Exec(ctx,
		`INSERT INTO send_attempts (message_id, status, attempted_at)
		 VALUES ($1, 'attempting', now())`, msg.ID); err != nil {
		t.Fatalf("seed inflight row: %v", err)
	}

	var sendCalls int32
	_, err := store.ApproveAndSend(ctx, msg.ID, user.ID, identity.PendingApprovalEdit{},
		func(m *identity.Message) (identity.SendResult, error) {
			atomic.AddInt32(&sendCalls, 1)
			return identity.SendResult{}, nil
		})
	if !errors.Is(err, identity.ErrSendInProgress) {
		t.Errorf("err = %v, want ErrSendInProgress", err)
	}
	if got := atomic.LoadInt32(&sendCalls); got != 0 {
		t.Errorf("send() invoked %d time(s), want 0 when InFlight", got)
	}
}

// TestMarkSendSucceededWithRetry_HappyPath_TransitionsRowToSent verifies
// the C4 fix wrapper integrates with the real send_attempts row
// transitions: a fresh `attempting` row → MarkSendSucceededWithRetry →
// the row's status becomes `sent` with the recorded provider message
// ID. The retry path is verified at the unit level
// (TestRetryWithBackoff_RecoversAfterTransientFailures); this test
// just confirms the wrapper composes correctly with the existing
// MarkSendSucceeded under realistic DB conditions.
func TestMarkSendSucceededWithRetry_HappyPath_TransitionsRowToSent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	_, a := setupPendingAgent(t, store, "mark-success-retry")
	msg, _ := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"alice@example.com"}, nil, nil, "x", "b", "", nil, "send", "", "", "", 3600)

	// Acquire the slot via the normal claim path so the row exists
	// with status='attempting' — same shape as the real ApproveAndSend
	// flow before MarkSendSucceeded runs.
	claim, err := store.ClaimSendAttempt(ctx, msg.ID)
	if err != nil {
		t.Fatalf("ClaimSendAttempt: %v", err)
	}
	if claim.Outcome != identity.SendAttemptAcquired {
		t.Fatalf("ClaimSendAttempt outcome = %d, want Acquired", claim.Outcome)
	}

	result := identity.SendResult{
		ProviderMessageID: "<test-msg-id@amazonses.com>",
		Method:            "smtp",
		To:                []string{"alice@example.com"},
	}
	if err := store.MarkSendSucceededWithRetry(msg.ID, result); err != nil {
		t.Fatalf("MarkSendSucceededWithRetry happy path: %v", err)
	}

	// Verify the row transitioned to status='sent' and the provider
	// id is recorded — a subsequent ClaimSendAttempt MUST see
	// AlreadySent (the exactly-once gate engaged).
	follow, err := store.ClaimSendAttempt(ctx, msg.ID)
	if err != nil {
		t.Fatalf("follow-up Claim: %v", err)
	}
	if follow.Outcome != identity.SendAttemptAlreadySent {
		t.Errorf("follow-up Claim Outcome = %d, want SendAttemptAlreadySent (the C4 fix's load-bearing invariant)", follow.Outcome)
	}
	if follow.Sent.ProviderMessageID != result.ProviderMessageID {
		t.Errorf("cached provider id = %q, want %q", follow.Sent.ProviderMessageID, result.ProviderMessageID)
	}
}
