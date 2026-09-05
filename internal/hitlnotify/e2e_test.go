package hitlnotify_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/approvaltoken"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/hitlnotify"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// TestEndToEnd_AcceptTxThroughRiverToSMTP drives the whole durable path with a REAL
// River client and a real Notifier (talking to a fake SMTP): the accept-tx enqueues
// a hitl_notify job in the same tx as the pending_review row, River works it, the
// NotifyWorker composes + SendOnce's the email, and notified_at is stamped. Proves
// the seams (tx enqueue → worker → deliver → mark) compose end to end.
func TestEndToEnd_AcceptTxThroughRiverToSMTP(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}
	store := identity.NewStore(pool)

	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{
		Host: smtpAddr.Host, Port: smtpAddr.Port, FromDomain: "notify.test",
	})
	signer := approvaltoken.NewSigner("hitl-notify-e2e-secret")
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	notifier := hitlnotify.New(store, outbound.NewProviderSubmitter(relay, gate), signer, "notify.test", "", "", "https://app.example.test")

	// Seed a verified HITL agent + owner.
	user, err := store.CreateOrGetUser(ctx, "owner-e2e@reviewer.test", "Owner", "google-notify-e2e")
	if err != nil {
		t.Fatal(err)
	}
	domain := "e2e.bot.test"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatal(err)
	}
	ag, err := store.CreateAgent(ctx, "bot@"+domain, domain, "", "https://example.com/webhook", "", user.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Build the integration on a real client and bind the concrete Notifier.
	j := hitlnotify.NewJobs(store).WithGate(gate, pool)
	client, err := jobs.New(pool, jobs.Config{}, j)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	j.SetEnqueuer(client)
	j.SetDeliverer(notifier)

	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	// The accept-tx: create the pending_review row + enqueue the notify job + stamp,
	// all in one tx — exactly what HoldForApprovalCore does.
	var msgID string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := store.CreatePendingOutboundMessageTx(ctx, tx, ag.ID,
			[]string{"alice@example.com"}, nil, nil, "E2E subject", "body", "", nil,
			"send", "conv-e2e", "", "", 3600)
		if err != nil {
			return err
		}
		msgID = m.ID
		jobID, err := j.EnqueueNotifyTx(ctx, tx, m.ID)
		if err != nil {
			return err
		}
		return store.StampNotifyJobIDTx(ctx, tx, m.ID, jobID)
	}); err != nil {
		t.Fatalf("accept tx: %v", err)
	}

	// Wait for the worker to run the full path: notified_at is stamped only after a
	// successful SendOnce.
	deadline := time.Now().Add(15 * time.Second)
	var notifiedAt *time.Time
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, `SELECT notified_at FROM messages WHERE id=$1`, msgID).Scan(&notifiedAt); err != nil {
			t.Fatalf("poll notified_at: %v", err)
		}
		if notifiedAt != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if notifiedAt == nil {
		t.Fatal("notified_at never stamped — worker did not complete the send path")
	}

	msgs := smtpDone()
	if len(msgs) != 1 {
		t.Fatalf("fake SMTP got %d messages, want 1", len(msgs))
	}
	if msgs[0].To != "owner-e2e@reviewer.test" {
		t.Errorf("notification went to %q, want the owner", msgs[0].To)
	}
}
