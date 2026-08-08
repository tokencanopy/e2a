package identity_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhook"
)

// notifyRecorder is a WebhookNotifyTx that records the webhook ids it was
// invoked for, optionally failing to prove the sweep's transactional boundary.
type notifyRecorder struct {
	ids []string
	err error
}

func (r *notifyRecorder) enqueue(_ context.Context, _ pgx.Tx, webhookID string) error {
	if r.err != nil {
		return r.err
	}
	r.ids = append(r.ids, webhookID)
	return nil
}

func TestAutoDisableFailingWebhooks_EnqueuesExactlyOncePerTransition(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-disable-notify@example.com", "Owner", "google-wh-disable-notify")
	wh, err := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	for i := 0; i < 10; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 8, 'HTTP 404', now())`,
			fmt.Sprintf("whd_dn_%d_%s", i, wh.ID), wh.ID,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rec := &notifyRecorder{}
	n, err := store.AutoDisableFailingWebhooks(ctx, rec.enqueue)
	if err != nil {
		t.Fatalf("AutoDisableFailingWebhooks: %v", err)
	}
	if n != 1 {
		t.Fatalf("disabled count = %d, want 1", n)
	}
	if len(rec.ids) != 1 || rec.ids[0] != wh.ID {
		t.Errorf("notify enqueues = %v, want exactly [%s]", rec.ids, wh.ID)
	}

	after, err := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if err != nil {
		t.Fatalf("GetWebhookByID: %v", err)
	}
	if after.Enabled {
		t.Errorf("webhook still enabled after auto-disable")
	}
	// The customer-facing reason is captured from the most recent terminal
	// delivery error at disable time.
	if after.AutoDisableReason != "HTTP 404" {
		t.Errorf("AutoDisableReason = %q, want %q", after.AutoDisableReason, "HTTP 404")
	}

	// A second sweep over the same state must be a no-op: the enabled=true
	// predicate makes the transition observable exactly once (SC2).
	rec2 := &notifyRecorder{}
	n2, err := store.AutoDisableFailingWebhooks(ctx, rec2.enqueue)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n2 != 0 || len(rec2.ids) != 0 {
		t.Errorf("second sweep disabled %d, enqueued %v — want 0 and none", n2, rec2.ids)
	}
}

// TestAutoDisableFailingWebhooks_ReasonBoundedToWindow: the captured
// auto_disable_reason must come from the disable window, never from an
// arbitrarily old failure that happens to be the newest row carrying a
// last_error.
func TestAutoDisableFailingWebhooks_ReasonBoundedToWindow(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-reason-window@example.com", "Owner", "google-wh-reason-window")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	// A stale failure far outside AutoDisableWindow with a distinctive error.
	if _, err := pool.Exec(ctx,
		`INSERT INTO webhook_subscriber_deliveries
		    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at, created_at)
		 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 8, 'HTTP 410 stale', now() - interval '100 hours', now() - interval '100 hours')`,
		"whd_rw_stale_"+wh.ID, wh.ID,
	); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	// In-window terminal failures that trip the breaker but carry no error text.
	for i := 0; i < 10; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 8)`,
			fmt.Sprintf("whd_rw_%d_%s", i, wh.ID), wh.ID,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if n, err := store.AutoDisableFailingWebhooks(ctx, nil); err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", n, err)
	}
	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if after.AutoDisableReason != "" {
		t.Errorf("AutoDisableReason = %q — must not surface a failure outside the disable window", after.AutoDisableReason)
	}
}

func TestAutoDisableFailingWebhooks_EnqueueFailureRollsBackDisable(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-disable-rollback@example.com", "Owner", "google-wh-disable-rollback")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	for i := 0; i < 10; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 8, 'HTTP 500', now())`,
			fmt.Sprintf("whd_dr_%d_%s", i, wh.ID), wh.ID,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rec := &notifyRecorder{err: errors.New("river insert failed")}
	if _, err := store.AutoDisableFailingWebhooks(ctx, rec.enqueue); err == nil {
		t.Fatal("expected the sweep to surface the enqueue failure")
	}

	// The disable must not commit without its notification job: the webhook
	// stays enabled so the next sweep retries the whole transition.
	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if !after.Enabled {
		t.Errorf("webhook disabled despite enqueue failure — transition and job must commit atomically")
	}
	if after.AutoDisableReason != "" {
		t.Errorf("AutoDisableReason = %q, want empty after rollback", after.AutoDisableReason)
	}
}

func TestWarnFailingWebhooks(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-warn@example.com", "Owner", "google-wh-warn")

	seedFailures := func(t *testing.T, webhookID, prefix string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO webhook_subscriber_deliveries
				    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
				 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'pending', 1, 'HTTP 404', now())`,
				fmt.Sprintf("whd_%s_%d_%s", prefix, i, webhookID), webhookID,
			); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}

	t.Run("fires on attempt-level failures before any terminal row", func(t *testing.T) {
		wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-a", "", []string{"email.received"}, identity.WebhookFilters{})
		seedFailures(t, wh.ID, "warn_a", identity.WarnThreshold)

		rec := &notifyRecorder{}
		n, err := store.WarnFailingWebhooks(ctx, rec.enqueue)
		if err != nil {
			t.Fatalf("WarnFailingWebhooks: %v", err)
		}
		if n != 1 || len(rec.ids) != 1 || rec.ids[0] != wh.ID {
			t.Fatalf("warned %d, enqueued %v — want 1 and [%s]", n, rec.ids, wh.ID)
		}
		after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
		if after.WarnNotifiedAt == nil {
			t.Errorf("warn_notified_at not stamped")
		}

		// Dedupe: a second sweep over the same state warns nobody.
		rec2 := &notifyRecorder{}
		if n2, err := store.WarnFailingWebhooks(ctx, rec2.enqueue); err != nil || n2 != 0 || len(rec2.ids) != 0 {
			t.Errorf("second warn sweep: n=%d ids=%v err=%v — want all-zero", n2, rec2.ids, err)
		}
	})

	t.Run("does not fire below the threshold", func(t *testing.T) {
		wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-b", "", []string{"email.received"}, identity.WebhookFilters{})
		seedFailures(t, wh.ID, "warn_b", identity.WarnThreshold-1)

		rec := &notifyRecorder{}
		if n, err := store.WarnFailingWebhooks(ctx, rec.enqueue); err != nil || n != 0 {
			t.Errorf("warned %d (err %v), want 0 below threshold", n, err)
		}
	})

	t.Run("does not fire when any delivery succeeded in the window", func(t *testing.T) {
		wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-c", "", []string{"email.received"}, identity.WebhookFilters{})
		seedFailures(t, wh.ID, "warn_c", identity.WarnThreshold)
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'delivered', 1)`,
			"whd_warn_c_ok_"+wh.ID, wh.ID,
		); err != nil {
			t.Fatalf("seed delivered: %v", err)
		}

		rec := &notifyRecorder{}
		if n, err := store.WarnFailingWebhooks(ctx, rec.enqueue); err != nil || n != 0 {
			t.Errorf("warned %d (err %v), want 0 when a delivery succeeded", n, err)
		}
	})

	t.Run("does not fire for a disabled webhook", func(t *testing.T) {
		wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-d", "", []string{"email.received"}, identity.WebhookFilters{})
		seedFailures(t, wh.ID, "warn_d", identity.WarnThreshold)
		enabled := false
		if _, err := store.UpdateWebhook(ctx, wh.ID, user.ID, identity.WebhookUpdate{Enabled: &enabled}); err != nil {
			t.Fatalf("disable: %v", err)
		}

		rec := &notifyRecorder{}
		if n, err := store.WarnFailingWebhooks(ctx, rec.enqueue); err != nil || n != 0 {
			t.Errorf("warned %d (err %v), want 0 for a disabled webhook", n, err)
		}
	})

	t.Run("ignores failures outside the window", func(t *testing.T) {
		wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-e", "", []string{"email.received"}, identity.WebhookFilters{})
		seedFailures(t, wh.ID, "warn_e", identity.WarnThreshold)
		if _, err := pool.Exec(ctx,
			`UPDATE webhook_subscriber_deliveries SET created_at = now() - interval '25 hours' WHERE webhook_id = $1`,
			wh.ID,
		); err != nil {
			t.Fatalf("age rows: %v", err)
		}

		rec := &notifyRecorder{}
		if n, err := store.WarnFailingWebhooks(ctx, rec.enqueue); err != nil || n != 0 {
			t.Errorf("warned %d (err %v), want 0 for stale failures", n, err)
		}
	})

	t.Run("enqueue failure rolls back the warn stamp", func(t *testing.T) {
		wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-f", "", []string{"email.received"}, identity.WebhookFilters{})
		seedFailures(t, wh.ID, "warn_f", identity.WarnThreshold)

		rec := &notifyRecorder{err: errors.New("river insert failed")}
		if _, err := store.WarnFailingWebhooks(ctx, rec.enqueue); err == nil {
			t.Fatal("expected the warn sweep to surface the enqueue failure")
		}
		after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
		if after.WarnNotifiedAt != nil {
			t.Errorf("warn_notified_at stamped despite enqueue failure — stamp and job must commit atomically")
		}
	})
}

// TestWarnFailingWebhooks_RearmsAfterRecovery is the regression the design
// calls the easiest to miss: a successful delivery must clear
// warn_notified_at (alongside the existing last_delivered_at bump), or a
// webhook that recovers and later degrades again stays permanently
// unwarnable.
func TestWarnFailingWebhooks_RearmsAfterRecovery(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-warn-rearm@example.com", "Owner", "google-wh-warn-rearm")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-rearm", "", []string{"email.received"}, identity.WebhookFilters{})

	seed := func(prefix string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO webhook_subscriber_deliveries
				    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
				 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'pending', 1, 'HTTP 404', now())`,
				fmt.Sprintf("whd_%s_%d_%s", prefix, i, wh.ID), wh.ID,
			); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}

	// Episode 1: degrade → warn.
	seed("rearm1", identity.WarnThreshold)
	rec := &notifyRecorder{}
	if n, err := store.WarnFailingWebhooks(ctx, rec.enqueue); err != nil || n != 1 {
		t.Fatalf("episode 1: warned %d (err %v), want 1", n, err)
	}

	// Recovery: mark one pending delivery delivered — this must clear the
	// warn marker in the same transaction as the last_delivered_at bump.
	var deliveryID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM webhook_subscriber_deliveries WHERE webhook_id = $1 AND status = 'pending' LIMIT 1`,
		wh.ID,
	).Scan(&deliveryID); err != nil {
		t.Fatalf("pick delivery: %v", err)
	}
	if changed, err := webhook.NewSubscriberStore(pool).MarkDeliveredIfPending(ctx, deliveryID, 200); err != nil || !changed {
		t.Fatalf("MarkDeliveredIfPending: changed=%v err=%v", changed, err)
	}

	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if after.WarnNotifiedAt != nil {
		t.Fatalf("warn_notified_at not cleared on successful delivery — recovered webhooks become permanently unwarnable")
	}

	// Age episode 1 (including the delivered row) out of the warn window,
	// then degrade again.
	if _, err := pool.Exec(ctx,
		`UPDATE webhook_subscriber_deliveries SET created_at = now() - interval '25 hours' WHERE webhook_id = $1`,
		wh.ID,
	); err != nil {
		t.Fatalf("age rows: %v", err)
	}
	seed("rearm2", identity.WarnThreshold)

	rec2 := &notifyRecorder{}
	if n, err := store.WarnFailingWebhooks(ctx, rec2.enqueue); err != nil || n != 1 {
		t.Errorf("episode 2: warned %d (err %v), want 1 — the warn must re-arm after recovery", n, err)
	}
}

func TestUpdateWebhook_ReenableClearsAutoDisableReason(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-reenable-reason@example.com", "Owner", "google-wh-reenable-reason")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	// Simulate an auto-disable outside the cooldown window.
	if _, err := pool.Exec(ctx,
		`UPDATE webhooks SET enabled = false, auto_disabled_at = now() - interval '10 minutes', auto_disable_reason = 'HTTP 404' WHERE id = $1`,
		wh.ID,
	); err != nil {
		t.Fatalf("seed auto-disable: %v", err)
	}

	enabled := true
	updated, err := store.UpdateWebhook(ctx, wh.ID, user.ID, identity.WebhookUpdate{Enabled: &enabled})
	if err != nil {
		t.Fatalf("UpdateWebhook: %v", err)
	}
	if updated.AutoDisabledAt != nil {
		t.Errorf("auto_disabled_at not cleared on re-enable")
	}
	if updated.AutoDisableReason != "" {
		t.Errorf("AutoDisableReason = %q, want cleared on re-enable (a healthy webhook must not show a stale cause)", updated.AutoDisableReason)
	}
}
