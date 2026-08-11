package webhook_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhook"
)

// recordingNotifier records which notification kinds Tick enqueued, per
// webhook id.
type recordingNotifier struct {
	disabled []string
	warned   []string
}

func (r *recordingNotifier) EnqueueDisabledTx(_ context.Context, _ pgx.Tx, id string) error {
	r.disabled = append(r.disabled, id)
	return nil
}

func (r *recordingNotifier) EnqueueWarningTx(_ context.Context, _ pgx.Tx, id string) error {
	r.warned = append(r.warned, id)
	return nil
}

// TestAutoDisableWorker_TickDisablesAFailingWebhook drives the worker
// once and asserts both passes (auto-disable + clear-prev) run without
// panic and that the threshold-exceeding webhook ends up disabled.
func TestAutoDisableWorker_TickDisablesAFailingWebhook(t *testing.T) {
	pool := testutil.TestDB(t)
	istore := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := istore.CreateOrGetUser(ctx, "ad-worker-tick@example.com", "Owner", "google-ad-worker-tick")
	wh, _ := istore.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	// Seed 10 failed deliveries — at the auto-disable threshold.
	for i := 0; i < 10; i++ {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed')`,
			fmt.Sprintf("whd_adw_fail_%d_%s", i, wh.ID), wh.ID,
		)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	w := webhook.NewAutoDisableWorker(istore)
	w.Tick(ctx)

	after, _ := istore.GetWebhookByID(ctx, wh.ID, user.ID)
	if after.Enabled {
		t.Errorf("worker.Tick should have auto-disabled the failing webhook")
	}
}

// TestAutoDisableWorker_BurstCrossingBothThresholdsSendsOnlyDisable pins the
// sweep's pass ordering: when a single tick sees a burst that satisfies both
// the warn condition AND the disable condition, the customer gets exactly one
// email — the disable one. The disable pass runs first, so the warn pass's
// enabled = true predicate excludes the freshly-disabled row.
func TestAutoDisableWorker_BurstCrossingBothThresholdsSendsOnlyDisable(t *testing.T) {
	pool := testutil.TestDB(t)
	istore := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := istore.CreateOrGetUser(ctx, "ad-burst@example.com", "Owner", "google-ad-burst")
	wh, _ := istore.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	// Terminal failures at the disable threshold; each also carries a
	// recorded failed attempt, so the warn condition is satisfied too.
	for i := 0; i < identity.AutoDisableThreshold; i++ {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 8, 'HTTP 404', now())`,
			fmt.Sprintf("whd_burst_%d_%s", i, wh.ID), wh.ID,
		)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rec := &recordingNotifier{}
	w := webhook.NewAutoDisableWorker(istore)
	w.SetNotifier(rec)
	w.Tick(ctx)

	if len(rec.disabled) != 1 || rec.disabled[0] != wh.ID {
		t.Errorf("disabled notifications = %v, want exactly [%s]", rec.disabled, wh.ID)
	}
	if len(rec.warned) != 0 {
		t.Errorf("warning notifications = %v, want none — the disable email supersedes the warning in a single sweep", rec.warned)
	}
}

// TestAutoDisableWorker_WarnsBeforeBreakerCanTrip drives the incident shape
// the design exists for: attempt-level failures only (all rows still
// pending, nothing terminal), which the breaker cannot see but the warn
// pass must.
func TestAutoDisableWorker_WarnsBeforeBreakerCanTrip(t *testing.T) {
	pool := testutil.TestDB(t)
	istore := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := istore.CreateOrGetUser(ctx, "ad-warn-early@example.com", "Owner", "google-ad-warn-early")
	wh, _ := istore.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	for i := 0; i < identity.WarnThreshold; i++ {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'pending', 1, 'HTTP 404', now())`,
			fmt.Sprintf("whd_warnearly_%d_%s", i, wh.ID), wh.ID,
		)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rec := &recordingNotifier{}
	w := webhook.NewAutoDisableWorker(istore)
	w.SetNotifier(rec)
	w.Tick(ctx)

	if len(rec.warned) != 1 || rec.warned[0] != wh.ID {
		t.Errorf("warning notifications = %v, want exactly [%s]", rec.warned, wh.ID)
	}
	if len(rec.disabled) != 0 {
		t.Errorf("disabled notifications = %v, want none (nothing terminal yet)", rec.disabled)
	}
	after, _ := istore.GetWebhookByID(ctx, wh.ID, user.ID)
	if !after.Enabled {
		t.Errorf("webhook must stay enabled — the warn pass never disables")
	}
}

// TestAutoDisableWorker_NoNotifierSkipsWarnPass: with no notification
// pipeline wired (self-host without SMTP), the warn pass must not run at
// all — stamping warn_notified_at without an email is dead bookkeeping
// that would also suppress the first REAL warning if the operator later
// configures SMTP. The disable pass (core breaker) still runs.
func TestAutoDisableWorker_NoNotifierSkipsWarnPass(t *testing.T) {
	pool := testutil.TestDB(t)
	istore := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := istore.CreateOrGetUser(ctx, "ad-no-notifier@example.com", "Owner", "google-ad-no-notifier")
	wh, _ := istore.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})

	for i := 0; i < identity.WarnThreshold; i++ {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'pending', 1, 'HTTP 404', now())`,
			fmt.Sprintf("whd_nonotif_%d_%s", i, wh.ID), wh.ID,
		)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	w := webhook.NewAutoDisableWorker(istore) // no SetNotifier
	w.Tick(ctx)

	after, _ := istore.GetWebhookByID(ctx, wh.ID, user.ID)
	if after.WarnNotifiedAt != nil {
		t.Errorf("warn_notified_at stamped with no notifier wired — the warn pass must be skipped entirely")
	}
	if !after.Enabled {
		t.Errorf("webhook must stay enabled (warn-level failures only)")
	}
}
