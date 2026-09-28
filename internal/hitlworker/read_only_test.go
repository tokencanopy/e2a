package hitlworker_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
)

// Read-only accounts (docs/design/account-read-only.md): the TTL sweep must not
// release an abuse-paused account's held mail. An approve-on-expiry hold stays
// pending — inbound suspicious mail is not released into the inbox/webhooks,
// outbound mail is not sent — while reject-on-expiry holds still resolve
// (rejecting releases nothing). A resume releases the holds on the next sweep.

func pauseAccount(t *testing.T, pool *pgxpool.Pool, userID, state, class string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO account_sending_controls (user_id, state, reason, actor, pause_class)
		VALUES ($1, $2, 'synthetic', 'test', $3)
		ON CONFLICT (user_id) DO UPDATE SET state = $2, pause_class = $3`, userID, state, class); err != nil {
		t.Fatalf("pause: %v", err)
	}
}

func messageStatus(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var st string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM messages WHERE id=$1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSweepLeavesAReadOnlyAccountsApproveHoldsPending(t *testing.T) {
	w, store, pool, smtpDone := setupWorker(t)
	ctx := context.Background()

	approver := prepareAgent(t, store, "ro-approve", identity.HITLExpirationApprove)
	rejecter := prepareAgent(t, store, "ro-reject", identity.HITLExpirationReject)
	pauseAccount(t, pool, approver.UserID, "paused", "abuse")
	pauseAccount(t, pool, rejecter.UserID, "paused", "abuse")

	exp := time.Now().Add(time.Hour)
	inbound := func(a *identity.AgentIdentity) string {
		m, err := store.CreateInboundMessage(ctx, "", a.ID, "evil@x.test", a.ID, "", "held", "", "unread",
			[]byte("Subject: held\r\n\r\nx"), nil, nil, false, "", []string{a.ID}, nil, nil,
			identity.InboundScreening{Status: identity.MessageStatusPendingReview, ApprovalExpiresAt: &exp})
		if err != nil {
			t.Fatal(err)
		}
		backdateExpiry(t, pool, m.ID)
		return m.ID
	}
	inApprove := inbound(approver)
	inReject := inbound(rejecter)
	out, err := store.CreatePendingOutboundMessage(ctx, approver.ID,
		[]string{"alice@external.test"}, nil, nil, "Held", "body", "<p>html</p>", nil, "send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, out.ID)

	w.RunOnce(ctx)

	if st := messageStatus(t, pool, inApprove); st != identity.MessageStatusPendingReview {
		t.Errorf("inbound approve-on-expiry hold of a read-only account = %q, want pending_review (not released)", st)
	}
	if st := messageStatus(t, pool, out.ID); st != identity.MessageStatusPendingReview {
		t.Errorf("outbound approve-on-expiry hold of a read-only account = %q, want pending_review (not sent)", st)
	}
	if msgs := smtpDone(); len(msgs) != 0 {
		t.Errorf("read-only account sent %d messages from the sweep", len(msgs))
	}
	if st := messageStatus(t, pool, inReject); st != identity.MessageStatusReviewExpiredRejected {
		t.Errorf("inbound reject-on-expiry hold = %q, want review_expired_rejected", st)
	}

	// Other pause classes keep today's behaviour; a resume releases the hold.
	pauseAccount(t, pool, approver.UserID, "active", "abuse")
	w.RunOnce(ctx)
	if st := messageStatus(t, pool, inApprove); st != identity.MessageStatusReviewExpiredApproved {
		t.Errorf("after resume: inbound hold = %q, want review_expired_approved", st)
	}
}
