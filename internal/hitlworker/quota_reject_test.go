package hitlworker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
)

// TestWorkerAutoApproveOverQuotaRejects pins the TTL sweep's flow-cap
// re-check (usage-based pricing v1): a hold whose account is over cap at
// expiry must be terminally auto-rejected with the quota reason — never
// delivered-then-metered, and never left to re-fire the sweep forever.
func TestWorkerAutoApproveOverQuotaRejects(t *testing.T) {
	w, store, pool, smtpDone := setupWorker(t)
	ctx := context.Background()

	agent := prepareAgent(t, store, "quota-reject", identity.HITLExpirationApprove)
	msg, err := store.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{"alice@example.com"}, nil, nil,
		"Held over quota", "body", "", nil,
		"send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.SetQuotaCheck(func(_ context.Context, _ string, units int) error {
		if units != 1 {
			t.Errorf("quota probe units = %d, want 1", units)
		}
		return &limits.LimitExceededError{Resource: "messages_month", Limit: 3000, Current: 3000}
	})

	w.RunOnce(ctx)

	if msgs := smtpDone(); len(msgs) != 0 {
		t.Fatalf("over-quota auto-approve submitted %d SMTP messages, want zero", len(msgs))
	}
	var status string
	var reason *string
	if err := pool.QueryRow(ctx,
		`SELECT status, rejection_reason FROM messages WHERE id = $1`, msg.ID,
	).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusReviewExpiredRejected {
		t.Errorf("status = %q, want %q", status, identity.MessageStatusReviewExpiredRejected)
	}
	if reason == nil || !strings.Contains(*reason, "messages_month") {
		t.Errorf("rejection_reason = %v, want it to name the exceeded resource", reason)
	}
}

// TestWorkerAutoApproveQuotaTransientErrorFailsOpen pins the fail-open
// posture: a transient enforcer error must not reject the hold — the
// external path is re-checked at worker claim time.
func TestWorkerAutoApproveQuotaTransientErrorFailsOpen(t *testing.T) {
	w, store, pool, _ := setupWorker(t)
	ctx := context.Background()

	agent := prepareAgent(t, store, "quota-open", identity.HITLExpirationApprove)
	msg, err := store.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{"alice@example.com"}, nil, nil,
		"Held transient", "body", "", nil,
		"send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.SetQuotaCheck(func(context.Context, string, int) error {
		return errors.New("limits lookup: db down")
	})

	w.RunOnce(ctx)

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM messages WHERE id = $1`, msg.ID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusReviewExpiredApproved {
		t.Errorf("status = %q, want %q (transient quota error must fail open)", status, identity.MessageStatusReviewExpiredApproved)
	}
}
