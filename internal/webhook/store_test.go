package webhook_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhook"
)

func setupDeliveryFixture(t *testing.T) (*pgxpool.Pool, *identity.Store, *webhook.DeliveryStore, *identity.AgentIdentity, *identity.Message) {
	t.Helper()

	pool := testutil.TestDB(t)
	identityStore := identity.NewStore(pool)
	deliveryStore := webhook.NewDeliveryStore(pool)
	ctx := context.Background()

	user, err := identityStore.CreateOrGetUser(ctx, "retry@test.com", "Retry User", "retry-google-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := identityStore.ClaimOrCreateDomain(ctx, "retry.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	agent, err := identityStore.CreateAgent(ctx, "bot@retry.example.com", "retry.example.com", "", "https://example.com/webhook", "cloud", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	msg, err := identityStore.CreateOutboundMessage(ctx, agent.ID, []string{"alice@example.com"}, nil, nil, "Hello", "send", "webhook", "", "", nil)
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}

	return pool, identityStore, deliveryStore, agent, msg
}

func TestCreateDeliveryAndGetPendingDeliveriesUsesMessageKeyedSchema(t *testing.T) {
	_, _, deliveryStore, agent, msg := setupDeliveryFixture(t)
	ctx := context.Background()

	delivery, err := deliveryStore.CreateDelivery(ctx, msg.ID, "initial failure")
	if err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}

	if delivery.MessageID != msg.ID {
		t.Fatalf("MessageID = %q, want %q", delivery.MessageID, msg.ID)
	}

	pending, err := deliveryStore.GetPendingDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingDeliveries: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending deliveries = %d, want 1", len(pending))
	}

	got := pending[0]
	if got.MessageID != msg.ID {
		t.Fatalf("pending MessageID = %q, want %q", got.MessageID, msg.ID)
	}
	if got.AgentID != agent.ID {
		t.Fatalf("pending AgentID = %q, want %q", got.AgentID, agent.ID)
	}
	if got.LastError != "initial failure" {
		t.Fatalf("pending LastError = %q, want %q", got.LastError, "initial failure")
	}
}

// Regression (deterministic): a second call to GetPendingDeliveries
// immediately after the first must not re-claim the same rows. Pre-fix,
// the autocommit query had no lease — both calls returned the identical
// N rows. Post-fix, the first call bumps next_retry_at by LeaseDuration,
// so the second call sees nothing eligible.
func TestGetPendingDeliveriesLeasesClaimedRows(t *testing.T) {
	_, identityStore, deliveryStore, agent, _ := setupDeliveryFixture(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		m, err := identityStore.CreateOutboundMessage(ctx, agent.ID,
			[]string{"alice@example.com"}, nil, nil,
			"hi", "send", "webhook", "", "", nil)
		if err != nil {
			t.Fatalf("CreateOutboundMessage: %v", err)
		}
		if _, err := deliveryStore.CreateDelivery(ctx, m.ID, ""); err != nil {
			t.Fatalf("CreateDelivery: %v", err)
		}
	}

	first, err := deliveryStore.GetPendingDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("first GetPendingDeliveries: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("first call returned %d, want 3", len(first))
	}

	second, err := deliveryStore.GetPendingDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("second GetPendingDeliveries: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second call returned %d rows, want 0 — lease did not hide claimed rows", len(second))
	}
}

// Regression (probabilistic): two workers (or two replicas) calling
// GetPendingDeliveries in parallel must not both receive the same row.
// Pre-fix the FOR UPDATE SKIP LOCKED was a no-op because the SELECT ran
// in autocommit mode, so the lock was released the moment the rows were
// returned to the caller — both workers would then deliver the same
// webhook. The fix wraps the lookup in a transaction and bumps
// next_retry_at as a lease so the second caller sees an empty result.
func TestGetPendingDeliveriesIsSafeForConcurrentCallers(t *testing.T) {
	pool, identityStore, deliveryStore, agent, _ := setupDeliveryFixture(t)
	_ = pool
	_ = agent
	ctx := context.Background()

	// Create N pending deliveries so the test is meaningful.
	const n = 4
	for i := 0; i < n; i++ {
		m, err := identityStore.CreateOutboundMessage(ctx, agent.ID,
			[]string{"alice@example.com"}, nil, nil,
			"hi", "send", "webhook", "", "", nil)
		if err != nil {
			t.Fatalf("CreateOutboundMessage: %v", err)
		}
		if _, err := deliveryStore.CreateDelivery(ctx, m.ID, ""); err != nil {
			t.Fatalf("CreateDelivery: %v", err)
		}
	}

	// Two workers race to claim the batch.
	type result struct {
		ds  []webhook.Delivery
		err error
	}
	out := make(chan result, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			ds, err := deliveryStore.GetPendingDeliveries(ctx, n)
			out <- result{ds, err}
		}()
	}
	close(start)
	a := <-out
	b := <-out
	if a.err != nil || b.err != nil {
		t.Fatalf("GetPendingDeliveries errors: %v / %v", a.err, b.err)
	}

	// Every message_id should appear at most once across the two batches.
	seen := make(map[string]int)
	for _, d := range a.ds {
		seen[d.MessageID]++
	}
	for _, d := range b.ds {
		seen[d.MessageID]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("delivery %s claimed %d times — same row leased to two workers", id, count)
		}
	}
	total := len(a.ds) + len(b.ds)
	if total < 1 || total > n {
		t.Errorf("total deliveries claimed = %d, want in [1, %d]", total, n)
	}
}

func TestDeliveryStatusUpdatesByMessageID(t *testing.T) {
	_, identityStore, deliveryStore, _, msg := setupDeliveryFixture(t)
	ctx := context.Background()

	if _, err := deliveryStore.CreateDelivery(ctx, msg.ID, ""); err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}

	retryAt := time.Now().Add(5 * time.Minute).UTC().Round(time.Second)
	if err := deliveryStore.MarkAttemptFailed(ctx, msg.ID, "temporary failure", retryAt); err != nil {
		t.Fatalf("MarkAttemptFailed: %v", err)
	}

	activity, err := identityStore.ListActivityByAgent(ctx, msg.AgentID, 10)
	if err != nil {
		t.Fatalf("ListActivityByAgent after failed attempt: %v", err)
	}
	if len(activity) != 1 {
		t.Fatalf("activity len after failed attempt = %d, want 1", len(activity))
	}
	if activity[0].WebhookStatus != "pending" {
		t.Fatalf("WebhookStatus after failed attempt = %q, want pending", activity[0].WebhookStatus)
	}
	if activity[0].WebhookAttempts != 1 {
		t.Fatalf("WebhookAttempts after failed attempt = %d, want 1", activity[0].WebhookAttempts)
	}
	if activity[0].WebhookError != "temporary failure" {
		t.Fatalf("WebhookError after failed attempt = %q, want temporary failure", activity[0].WebhookError)
	}

	if err := deliveryStore.MarkDelivered(ctx, msg.ID); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	activity, err = identityStore.ListActivityByAgent(ctx, msg.AgentID, 10)
	if err != nil {
		t.Fatalf("ListActivityByAgent: %v", err)
	}
	if len(activity) != 1 {
		t.Fatalf("activity len = %d, want 1", len(activity))
	}
	if activity[0].WebhookStatus != "delivered" {
		t.Fatalf("WebhookStatus = %q, want delivered", activity[0].WebhookStatus)
	}
	if activity[0].WebhookAttempts != 2 {
		t.Fatalf("WebhookAttempts = %d, want 2", activity[0].WebhookAttempts)
	}
}

// TestGetPendingDeliveriesSkipsTrash: migration 063's trash gate — a pending
// delivery whose message (or whole inbox) was soft-deleted must not be
// claimed; it resumes if the message is restored inside the retry window.
func TestGetPendingDeliveriesSkipsTrash(t *testing.T) {
	_, identityStore, deliveryStore, agent, msg := setupDeliveryFixture(t)
	ctx := context.Background()

	if _, err := deliveryStore.CreateDelivery(ctx, msg.ID, "boom"); err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}

	// Trash the message → not claimable.
	if err := identityStore.SoftDeleteMessage(ctx, msg.ID, agent.ID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	pending, err := deliveryStore.GetPendingDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingDeliveries: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("claimed %d deliveries for a trashed message, want 0", len(pending))
	}

	// Restore → claimable again (the skip left the row unleased).
	if _, err := identityStore.RestoreMessage(ctx, msg.ID, agent.ID); err != nil {
		t.Fatalf("RestoreMessage: %v", err)
	}
	pending, err = deliveryStore.GetPendingDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingDeliveries after restore: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("claimed %d deliveries after restore, want 1", len(pending))
	}
}
