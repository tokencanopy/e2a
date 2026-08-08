package identity_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// --- B1 regression suite: the breaker must not eat its own output, and
// re-enable must reset the evidence window (adversarial review, 2026-08-08).

// The snooze cap writes terminal rows with last_error='webhook disabled'.
// Those are e2a-synthetic bookkeeping, not endpoint failures: if the breaker
// counted them, a user who manually disabled an endpoint for >24h and then
// re-enabled it would be auto-disabled again within one sweep, with the
// self-referential reason "webhook disabled" mailed to them.
func TestAutoDisableFailingWebhooks_IgnoresSnoozeCapSyntheticRows(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-synthetic@example.com", "Owner", "google-wh-synthetic")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	for i := 0; i < identity.AutoDisableThreshold+2; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 1, $3, now())`,
			fmt.Sprintf("whd_syn_%d_%s", i, wh.ID), wh.ID, identity.LastErrorWebhookDisabled,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rec := &notifyRecorder{}
	n, err := store.AutoDisableFailingWebhooks(ctx, rec.enqueue)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 || len(rec.ids) != 0 {
		t.Errorf("sweep disabled %d / enqueued %v on synthetic snooze-cap rows — the breaker must ignore its own output", n, rec.ids)
	}
	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if !after.Enabled {
		t.Errorf("webhook disabled by synthetic rows")
	}
}

// Mixed genuine + synthetic: still trips on the genuine failures, and the
// captured reason is the genuine error — never the marker string.
func TestAutoDisableFailingWebhooks_ReasonNeverTheSyntheticMarker(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-syn-mixed@example.com", "Owner", "google-wh-syn-mixed")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	for i := 0; i < identity.AutoDisableThreshold; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 8, 'HTTP 404', now() - interval '1 hour')`,
			fmt.Sprintf("whd_mix_g_%d_%s", i, wh.ID), wh.ID,
		); err != nil {
			t.Fatalf("seed genuine: %v", err)
		}
	}
	// A synthetic row NEWER than every genuine one — the naive "most recent
	// last_error" would pick it.
	if _, err := pool.Exec(ctx,
		`INSERT INTO webhook_subscriber_deliveries
		    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
		 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 1, $3, now())`,
		"whd_mix_s_"+wh.ID, wh.ID, identity.LastErrorWebhookDisabled,
	); err != nil {
		t.Fatalf("seed synthetic: %v", err)
	}

	if n, err := store.AutoDisableFailingWebhooks(ctx, nil); err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1 (genuine failures still trip)", n, err)
	}
	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if after.AutoDisableReason != "HTTP 404" {
		t.Errorf("AutoDisableReason = %q, want the genuine HTTP 404 — never the synthetic marker", after.AutoDisableReason)
	}
}

// The feature's own recovery instruction is "fix the endpoint, then
// re-enable". Re-enabling must therefore RESET the evidence window: the
// >=10 genuine failed rows that caused the disable are still inside the
// 72h window, and without the reset the next sweep re-disables within 5
// minutes and mails a false "we disabled your webhook" — a loop for any
// low-traffic webhook.
func TestAutoDisableFailingWebhooks_ReenableResetsEvidenceWindow(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-reenable-window@example.com", "Owner", "google-wh-reenable-window")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	for i := 0; i < identity.AutoDisableThreshold; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 8, 'HTTP 404', now())`,
			fmt.Sprintf("whd_rw2_%d_%s", i, wh.ID), wh.ID,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if n, err := store.AutoDisableFailingWebhooks(ctx, nil); err != nil || n != 1 {
		t.Fatalf("first sweep: n=%d err=%v, want 1", n, err)
	}

	// Past the cooldown, the user fixes the endpoint and re-enables.
	if _, err := pool.Exec(ctx,
		`UPDATE webhooks SET auto_disabled_at = now() - interval '10 minutes' WHERE id = $1`, wh.ID,
	); err != nil {
		t.Fatalf("backdate cooldown: %v", err)
	}
	enabled := true
	if _, err := store.UpdateWebhook(ctx, wh.ID, user.ID, identity.WebhookUpdate{Enabled: &enabled}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	// The next sweep must NOT re-trip on the pre-re-enable evidence.
	rec := &notifyRecorder{}
	if n, err := store.AutoDisableFailingWebhooks(ctx, rec.enqueue); err != nil || n != 0 || len(rec.ids) != 0 {
		t.Fatalf("post-re-enable sweep: n=%d enqueued=%v err=%v — re-enable must reset the evidence window", n, rec.ids, err)
	}
	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if !after.Enabled {
		t.Errorf("webhook re-disabled from stale evidence")
	}

	// New failures AFTER the re-enable still count: the breaker is reset,
	// not lobotomized.
	for i := 0; i < identity.AutoDisableThreshold; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 8, 'HTTP 500', now())`,
			fmt.Sprintf("whd_rw3_%d_%s", i, wh.ID), wh.ID,
		); err != nil {
			t.Fatalf("seed fresh: %v", err)
		}
	}
	if n, err := store.AutoDisableFailingWebhooks(ctx, nil); err != nil || n != 1 {
		t.Errorf("fresh-failure sweep: n=%d err=%v, want 1", n, err)
	}
}

// Same two properties for the warn pass: synthetic rows don't warn, and
// re-enable resets the warn evidence window.
func TestWarnFailingWebhooks_SyntheticAndReenable(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-warn-b1@example.com", "Owner", "google-wh-warn-b1")

	t.Run("ignores snooze-cap synthetic rows", func(t *testing.T) {
		wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-syn", "", []string{"email.received"}, identity.WebhookFilters{})
		for i := 0; i < identity.WarnThreshold; i++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO webhook_subscriber_deliveries
				    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
				 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 1, $3, now())`,
				fmt.Sprintf("whd_wsyn_%d_%s", i, wh.ID), wh.ID, identity.LastErrorWebhookDisabled,
			); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		rec := &notifyRecorder{}
		if n, err := store.WarnFailingWebhooks(ctx, rec.enqueue); err != nil || n != 0 {
			t.Errorf("warned %d (err %v) on synthetic rows, want 0", n, err)
		}
	})

	t.Run("re-enable resets the warn window", func(t *testing.T) {
		wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-reen", "", []string{"email.received"}, identity.WebhookFilters{})
		for i := 0; i < identity.WarnThreshold; i++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO webhook_subscriber_deliveries
				    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
				 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'pending', 1, 'HTTP 404', now())`,
				fmt.Sprintf("whd_wreen_%d_%s", i, wh.ID), wh.ID,
			); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		// User toggles the webhook (disable + re-enable): the explicit
		// re-enable asserts "this endpoint is fine now" and resets evidence.
		off, on := false, true
		if _, err := store.UpdateWebhook(ctx, wh.ID, user.ID, identity.WebhookUpdate{Enabled: &off}); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if _, err := store.UpdateWebhook(ctx, wh.ID, user.ID, identity.WebhookUpdate{Enabled: &on}); err != nil {
			t.Fatalf("re-enable: %v", err)
		}
		rec := &notifyRecorder{}
		if n, err := store.WarnFailingWebhooks(ctx, rec.enqueue); err != nil || n != 0 {
			t.Errorf("warned %d (err %v) on pre-re-enable evidence, want 0", n, err)
		}
	})
}

// --- Regression tests for the four blockers found in review of #852. ---
//
// Each of these failed before its fix and none had any coverage, which is why
// the defects survived implementation, self-review, and a full green suite.

// seedFailedDeliveries inserts n attempt-level failures for a webhook, stamped
// at `age` before now so a test can order them against a success.
func seedFailedDeliveries(t *testing.T, pool *pgxpool.Pool, ctx context.Context, webhookID, prefix string, n int, age time.Duration) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, created_at, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'pending', 1, 'HTTP 404', now() - $3::interval, now() - $3::interval)`,
			fmt.Sprintf("whd_%s_%d_%s", prefix, i, webhookID), webhookID, age.String(),
		); err != nil {
			t.Fatalf("seed failures: %v", err)
		}
	}
}

// BLOCKER 1. A webhook that was delivering fine and then breaks must warn on
// the next sweep. The original guard counted ANY success in the trailing 24h,
// so a success an hour before the breakage suppressed the warning for ~24h —
// i.e. for every integration that was actually working, which is the entire
// population this feature exists to protect.
func TestWarnFailingWebhooks_WarnsAfterARecentSuccess(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-warn-recent@example.com", "Owner", "google-wh-warn-recent")

	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-recent", "", []string{"email.received"}, identity.WebhookFilters{})

	// It delivered successfully an hour ago — exactly as a real delivery does.
	if _, err := pool.Exec(ctx,
		`UPDATE webhooks SET last_delivered_at = now() - interval '1 hour' WHERE id = $1`, wh.ID); err != nil {
		t.Fatalf("seed success: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO webhook_subscriber_deliveries
		    (id, webhook_id, event_type, event_payload, status, attempts, created_at)
		 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'delivered', 1, now() - interval '2 hours')`,
		"whd_warn_recent_ok_"+wh.ID, wh.ID,
	); err != nil {
		t.Fatalf("seed delivered row: %v", err)
	}

	// Then it broke five minutes ago.
	seedFailedDeliveries(t, pool, ctx, wh.ID, "warn_recent", identity.WarnThreshold, 5*time.Minute)

	rec := &notifyRecorder{}
	n, err := store.WarnFailingWebhooks(ctx, rec.enqueue)
	if err != nil {
		t.Fatalf("WarnFailingWebhooks: %v", err)
	}
	if n != 1 || len(rec.ids) != 1 || rec.ids[0] != wh.ID {
		t.Fatalf("warned %d ids=%v — want 1 and [%s]: a webhook that succeeded an hour ago and has been failing for five minutes MUST warn now, not in 24h", n, rec.ids, wh.ID)
	}
}

// BLOCKER 1 (converse). Failures that predate the last success are stale
// evidence and must not warn — this is the "noisy but still working" case the
// zero-delivered guard existed to protect, which the fix must preserve.
func TestWarnFailingWebhooks_IgnoresFailuresBeforeTheLastSuccess(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-warn-stale@example.com", "Owner", "google-wh-warn-stale")

	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/warn-stale", "", []string{"email.received"}, identity.WebhookFilters{})

	// Failures three hours ago, then a success one hour ago: recovered.
	seedFailedDeliveries(t, pool, ctx, wh.ID, "warn_stale", identity.WarnThreshold+3, 3*time.Hour)
	if _, err := pool.Exec(ctx,
		`UPDATE webhooks SET last_delivered_at = now() - interval '1 hour' WHERE id = $1`, wh.ID); err != nil {
		t.Fatalf("seed success: %v", err)
	}

	rec := &notifyRecorder{}
	if n, err := store.WarnFailingWebhooks(ctx, rec.enqueue); err != nil || n != 0 {
		t.Errorf("warned %d (err %v) — want 0: every failure predates the last success, so the endpoint is working", n, err)
	}
}

// BLOCKER 2. The per-tick cap sat inside the candidate subquery while the
// eligibility filters sat outside it, so the cap truncated a set full of
// already-warned webhooks. Past ~WarnSweepMaxPerTick simultaneous failures the
// pass warned nobody new, forever, and silently.
func TestWarnFailingWebhooks_CapDrainsAcrossTicks(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-warn-cap@example.com", "Owner", "google-wh-warn-cap")

	const extra = 5
	const perUser = 25 // stay under the per-user webhook cap
	total := identity.WarnSweepMaxPerTick + extra
	ids := make(map[string]bool, total)
	owner := user
	for i := 0; i < total; i++ {
		if i > 0 && i%perUser == 0 {
			owner, _ = store.CreateOrGetUser(ctx,
				fmt.Sprintf("wh-warn-cap-%d@example.com", i), "Owner",
				fmt.Sprintf("google-wh-warn-cap-%d", i))
		}
		wh, err := store.CreateWebhook(ctx, owner.ID, fmt.Sprintf("https://example.com/cap-%d", i), "", []string{"email.received"}, identity.WebhookFilters{})
		if err != nil {
			t.Fatalf("create webhook %d: %v", i, err)
		}
		ids[wh.ID] = true
		seedFailedDeliveries(t, pool, ctx, wh.ID, fmt.Sprintf("cap%d", i), identity.WarnThreshold, time.Minute)
	}

	warned := map[string]bool{}
	for tick := 1; tick <= 3; tick++ {
		rec := &notifyRecorder{}
		n, err := store.WarnFailingWebhooks(ctx, rec.enqueue)
		if err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		if n > identity.WarnSweepMaxPerTick {
			t.Fatalf("tick %d warned %d — exceeds the per-tick cap of %d", tick, n, identity.WarnSweepMaxPerTick)
		}
		for _, id := range rec.ids {
			if warned[id] {
				t.Errorf("tick %d re-warned %s — dedupe broken", tick, id)
			}
			warned[id] = true
		}
		if len(warned) == total {
			break
		}
	}

	if len(warned) != total {
		t.Fatalf("warned %d of %d webhooks after three ticks — the cap does not drain, so genuinely-broken endpoints are never warned", len(warned), total)
	}
}

// BLOCKER 3. Re-enabling cleared the disable evidence but left
// warn_notified_at set. Its only other reset is a successful delivery — which
// never happens on a still-broken endpoint — so the second failure episode got
// no early warning at all.
func TestUpdateWebhook_ReenableClearsWarnMarkerSoEpisodeTwoWarns(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-warn-ep2@example.com", "Owner", "google-wh-warn-ep2")

	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/episode-two", "", []string{"email.received"}, identity.WebhookFilters{})

	// Episode 1: warn fires.
	seedFailedDeliveries(t, pool, ctx, wh.ID, "ep1", identity.WarnThreshold, time.Minute)
	rec1 := &notifyRecorder{}
	if n, err := store.WarnFailingWebhooks(ctx, rec1.enqueue); err != nil || n != 1 {
		t.Fatalf("episode 1 warn: n=%d err=%v — want 1", n, err)
	}

	// The breaker disables it, then the user "fixes" the endpoint and re-enables.
	if _, err := pool.Exec(ctx,
		`UPDATE webhooks SET enabled = false, auto_disabled_at = now() - interval '10 minutes' WHERE id = $1`, wh.ID); err != nil {
		t.Fatalf("simulate disable: %v", err)
	}
	yes := true
	if _, err := store.UpdateWebhook(ctx, wh.ID, user.ID, identity.WebhookUpdate{Enabled: &yes}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if after.WarnNotifiedAt != nil {
		t.Fatalf("warn_notified_at survived the re-enable — episode 2 can never warn")
	}

	// Episode 2: the fix did not work. Fresh failures must warn again.
	seedFailedDeliveries(t, pool, ctx, wh.ID, "ep2", identity.WarnThreshold, 0)
	rec2 := &notifyRecorder{}
	n, err := store.WarnFailingWebhooks(ctx, rec2.enqueue)
	if err != nil {
		t.Fatalf("episode 2 warn: %v", err)
	}
	if n != 1 || len(rec2.ids) != 1 || rec2.ids[0] != wh.ID {
		t.Fatalf("episode 2 warned %d ids=%v — want 1 and [%s]", n, rec2.ids, wh.ID)
	}
}

// BLOCKER 4. reenabled_at was stamped on EVERY enabled:true PATCH rather than
// on a real disabled→enabled transition, so an idempotent reconciler kept
// pushing the evidence window forward and the breaker could never trip. This
// was remotely reachable through the public API.
func TestUpdateWebhook_EnableNoOpDoesNotResetTheEvidenceWindow(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-noop@example.com", "Owner", "google-wh-noop")

	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/noop-patch", "", []string{"email.received"}, identity.WebhookFilters{})

	// Evidence accumulates while the endpoint is broken.
	for i := 0; i < identity.AutoDisableThreshold; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, created_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 8, 'HTTP 404', now() - interval '1 hour')`,
			fmt.Sprintf("whd_noop_%d_%s", i, wh.ID), wh.ID,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// A reconciliation loop asserts the desired state on an ALREADY-enabled
	// webhook. This must not count as a re-enable.
	yes := true
	for i := 0; i < 3; i++ {
		if _, err := store.UpdateWebhook(ctx, wh.ID, user.ID, identity.WebhookUpdate{Enabled: &yes}); err != nil {
			t.Fatalf("no-op enable PATCH %d: %v", i, err)
		}
	}
	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if after.ReenabledAt != nil {
		t.Errorf("reenabled_at stamped by a no-op enable PATCH — an idempotent client can blind both sweeps indefinitely")
	}

	// The breaker must still see the evidence and fire.
	rec := &notifyRecorder{}
	n, err := store.AutoDisableFailingWebhooks(ctx, rec.enqueue)
	if err != nil {
		t.Fatalf("AutoDisableFailingWebhooks: %v", err)
	}
	if n != 1 || len(rec.ids) != 1 || rec.ids[0] != wh.ID {
		t.Fatalf("disabled %d ids=%v — want 1 and [%s]: repeated enable:true PATCHes must not defeat the breaker", n, rec.ids, wh.ID)
	}
}

// BLOCKER 4 (converse). A genuine disabled→enabled transition must still reset
// the window, or the stale failures re-disable the webhook within one sweep.
func TestUpdateWebhook_RealReenableStillResetsTheEvidenceWindow(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-realreenable@example.com", "Owner", "google-wh-realreenable")

	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/real-reenable", "", []string{"email.received"}, identity.WebhookFilters{})
	for i := 0; i < identity.AutoDisableThreshold; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, created_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 8, 'HTTP 404', now() - interval '1 hour')`,
			fmt.Sprintf("whd_rr_%d_%s", i, wh.ID), wh.ID,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`UPDATE webhooks SET enabled = false, auto_disabled_at = now() - interval '10 minutes' WHERE id = $1`, wh.ID); err != nil {
		t.Fatalf("simulate disable: %v", err)
	}

	yes := true
	if _, err := store.UpdateWebhook(ctx, wh.ID, user.ID, identity.WebhookUpdate{Enabled: &yes}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	after, _ := store.GetWebhookByID(ctx, wh.ID, user.ID)
	if after.ReenabledAt == nil {
		t.Fatalf("reenabled_at not stamped on a real disabled→enabled transition")
	}

	rec := &notifyRecorder{}
	if n, err := store.AutoDisableFailingWebhooks(ctx, rec.enqueue); err != nil || n != 0 {
		t.Errorf("disabled %d (err %v) — want 0: pre-re-enable failures are stale evidence and must not immediately re-disable", n, err)
	}
}
