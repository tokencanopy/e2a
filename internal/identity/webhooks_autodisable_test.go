package identity_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func TestAutoDisableFailingWebhooks_TripsAfterThresholdFailures(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-autodisable-fail@example.com", "Owner", "google-wh-autodisable-fail")

	wh, err := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	// 10 failed + 0 delivered → trips the auto-disable threshold.
	for i := 0; i < 10; i++ {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed')`,
			fmt.Sprintf("whd_fail_%d_%s", i, wh.ID), wh.ID,
		)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	n, err := store.AutoDisableFailingWebhooks(ctx, nil)
	if err != nil {
		t.Fatalf("AutoDisableFailingWebhooks: %v", err)
	}
	if n != 1 {
		t.Errorf("disabled count = %d, want 1", n)
	}

	after, err := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if err != nil {
		t.Fatalf("GetWebhookByID: %v", err)
	}
	if after.Enabled {
		t.Errorf("webhook still enabled after auto-disable")
	}
	if after.AutoDisabledAt == nil {
		t.Errorf("auto_disabled_at not set")
	}
}

func TestAutoDisableFailingWebhooks_SpareWebhooksWithDeliveries(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-autodisable-spare@example.com", "Owner", "google-wh-autodisable-spare")

	wh, err := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	// 10 failed BUT also 1 delivered → spared. The zero-delivered
	// guard prevents a working-but-noisy webhook from being killed.
	for i := 0; i < 10; i++ {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed')`,
			fmt.Sprintf("whd_failmix_%d_%s", i, wh.ID), wh.ID,
		)
		if err != nil {
			t.Fatalf("seed failed row: %v", err)
		}
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO webhook_subscriber_deliveries
		    (id, webhook_id, event_type, event_payload, status)
		 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'delivered')`,
		"whd_failmix_ok_"+wh.ID, wh.ID,
	)
	if err != nil {
		t.Fatalf("seed delivered row: %v", err)
	}

	n, err := store.AutoDisableFailingWebhooks(ctx, nil)
	if err != nil {
		t.Fatalf("AutoDisableFailingWebhooks: %v", err)
	}
	if n != 0 {
		t.Errorf("disabled count = %d, want 0", n)
	}

	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if !after.Enabled {
		t.Errorf("webhook should still be enabled (had a delivery)")
	}
}

func TestClearExpiredPrevSecrets_NullsExpiredRows(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-clear-prev@example.com", "Owner", "google-wh-clear-prev")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	// Stamp a prev secret with an expiry an hour in the past.
	pastExpiry := time.Now().Add(-1 * time.Hour)
	_, err := pool.Exec(ctx,
		`UPDATE webhooks
		 SET signing_secret_prev = 'whsec_expired',
		     signing_secret_prev_expires_at = $2
		 WHERE id = $1`,
		wh.ID, pastExpiry,
	)
	if err != nil {
		t.Fatalf("seed expired prev: %v", err)
	}

	n, err := store.ClearExpiredPrevSecrets(ctx)
	if err != nil {
		t.Fatalf("ClearExpiredPrevSecrets: %v", err)
	}
	if n < 1 {
		t.Errorf("cleared count = %d, want >= 1", n)
	}

	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if after.SigningSecretPrev != "" {
		t.Errorf("SigningSecretPrev should be empty after clear, got %q", after.SigningSecretPrev)
	}
	if after.SigningSecretPrevExpiresAt != nil {
		t.Errorf("expires_at should be nil after clear")
	}
}

func TestClearExpiredPrevSecrets_KeepsActiveGraceRows(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-keep-prev@example.com", "Owner", "google-wh-keep-prev")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	// Stamp a prev secret with an expiry IN THE FUTURE — should be kept.
	futureExpiry := time.Now().Add(12 * time.Hour)
	_, err := pool.Exec(ctx,
		`UPDATE webhooks
		 SET signing_secret_prev = 'whsec_active',
		     signing_secret_prev_expires_at = $2
		 WHERE id = $1`,
		wh.ID, futureExpiry,
	)
	if err != nil {
		t.Fatalf("seed active prev: %v", err)
	}

	if _, err := store.ClearExpiredPrevSecrets(ctx); err != nil {
		t.Fatalf("ClearExpiredPrevSecrets: %v", err)
	}

	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if after.SigningSecretPrev == "" {
		t.Errorf("SigningSecretPrev should be preserved while grace window is open")
	}
}
