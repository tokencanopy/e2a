package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// TestSetWebhookHealthLimits_OverridesWarnThreshold is the regression for
// issue #863: WarnThreshold used to be a compiled constant, so a deployment
// whose traffic could never accumulate 5 attempt-level failures in 24h could
// never warn no matter how thoroughly broken. SetWebhookHealthLimits lets an
// operator lower it per-deployment.
func TestSetWebhookHealthLimits_OverridesWarnThreshold(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	store.SetWebhookHealthLimits(2, 0) // sweepMaxPerTick left at its default
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-limit-warn@example.com", "Owner", "google-wh-limit-warn")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/limit-warn", "", []string{"email.received"}, identity.WebhookFilters{})

	// 2 attempt-level failures: below the compiled WarnThreshold (5), at the
	// configured override (2).
	seedFailedDeliveries(t, pool, ctx, wh.ID, "limit_warn", 2, time.Minute)

	rec := &notifyRecorder{}
	n, err := store.WarnFailingWebhooks(ctx, rec.enqueue)
	if err != nil {
		t.Fatalf("WarnFailingWebhooks: %v", err)
	}
	if n != 1 || len(rec.ids) != 1 || rec.ids[0] != wh.ID {
		t.Fatalf("warned %d, enqueued %v, want 1 and [%s] under the threshold=2 override", n, rec.ids, wh.ID)
	}
}

// TestWarnFailingWebhooks_DefaultWarnThresholdUnaffectedWithoutSetter pins the
// backward-compatible half: a Store nobody configured (every existing
// NewStore(pool) call site, including every other test in this package)
// keeps behaving exactly as the compiled WarnThreshold constant, even though
// the override mechanism now exists.
func TestWarnFailingWebhooks_DefaultWarnThresholdUnaffectedWithoutSetter(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-limit-default@example.com", "Owner", "google-wh-limit-default")
	wh, _ := store.CreateWebhook(ctx, user.ID, "https://example.com/limit-default", "", []string{"email.received"}, identity.WebhookFilters{})

	// 2 failures: below the compiled WarnThreshold (5). With no override in
	// effect this must NOT warn.
	seedFailedDeliveries(t, pool, ctx, wh.ID, "limit_default", 2, time.Minute)

	rec := &notifyRecorder{}
	if n, err := store.WarnFailingWebhooks(ctx, rec.enqueue); err != nil || n != 0 {
		t.Errorf("warned %d (err %v), want 0 below the unconfigured default threshold of %d", n, err, identity.WarnThreshold)
	}
}

// TestSetWebhookHealthLimits_OverridesSweepMaxPerTick is the regression for
// the incident-response half of #863: WarnSweepMaxPerTick and
// DisableSweepMaxPerTick used to be compiled constants, so an operator could
// not turn either cap down during a real e2a-side outage without shipping a
// release. A single override lowers both the warn and disable per-tick caps.
func TestSetWebhookHealthLimits_OverridesSweepMaxPerTick(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	store.SetWebhookHealthLimits(0, 2) // warnThreshold left at its default
	ctx := context.Background()
	user, _ := store.CreateOrGetUser(ctx, "wh-limit-sweep@example.com", "Owner", "google-wh-limit-sweep")

	const total = 5 // comfortably over the override cap of 2, well under the compiled 100
	for i := 0; i < total; i++ {
		wh, err := store.CreateWebhook(ctx, user.ID,
			"https://example.com/limit-sweep-"+string(rune('a'+i)), "", []string{"email.received"}, identity.WebhookFilters{})
		if err != nil {
			t.Fatalf("CreateWebhook %d: %v", i, err)
		}
		seedFailedDeliveries(t, pool, ctx, wh.ID, "limit_sweep", identity.WarnThreshold, time.Minute)
	}

	rec := &notifyRecorder{}
	n, err := store.WarnFailingWebhooks(ctx, rec.enqueue)
	if err != nil {
		t.Fatalf("WarnFailingWebhooks: %v", err)
	}
	if n != 2 {
		t.Fatalf("warned %d webhooks in one tick, want the overridden cap of 2 (total eligible = %d)", n, total)
	}

	// The remaining webhooks drain across further ticks (each still capped at
	// the same override) rather than being dropped: same shape as
	// TestWarnFailingWebhooks_CapDrainsAcrossTicks, against the override
	// instead of the compiled default.
	n2, err := store.WarnFailingWebhooks(ctx, rec.enqueue)
	if err != nil {
		t.Fatalf("WarnFailingWebhooks (tick 2): %v", err)
	}
	if n2 != 2 {
		t.Fatalf("second tick warned %d, want the override cap of 2 again", n2)
	}
	n3, err := store.WarnFailingWebhooks(ctx, rec.enqueue)
	if err != nil {
		t.Fatalf("WarnFailingWebhooks (tick 3): %v", err)
	}
	if n3 != total-4 {
		t.Fatalf("third tick warned %d, want the final remaining %d", n3, total-4)
	}
}
