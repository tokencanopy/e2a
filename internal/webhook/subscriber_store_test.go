package webhook_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhook"
)

// TestSubscriberStore_DeliveryLifecycle exercises the River-era store methods
// (the DeliverWorker + /test path) end to end: insert → get → record attempt →
// terminal failed, a parallel delivered row, list (all + filtered), missing-id
// lookup, and the expiry sweep.
func TestSubscriberStore_DeliveryLifecycle(t *testing.T) {
	pool := testutil.TestDB(t)
	istore := identity.NewStore(pool)
	ss := webhook.NewSubscriberStore(pool)
	ctx := context.Background()
	user, _ := istore.CreateOrGetUser(ctx, "wsd-life@example.com", "Owner", "google-wsd-life")
	wh, _ := istore.CreateWebhook(ctx, user.ID, "https://example.com/hook", "", []string{"email.received"}, identity.WebhookFilters{})
	env := []byte(`{"type":"email.received"}`)

	// InsertPendingForTest (+ generateDeliveryID) → GetSubscriberDeliveryByID.
	id, err := ss.InsertPendingForTest(ctx, wh.ID, "email.received", env)
	if err != nil {
		t.Fatalf("InsertPendingForTest: %v", err)
	}
	d, err := ss.GetSubscriberDeliveryByID(ctx, id)
	if err != nil {
		t.Fatalf("GetSubscriberDeliveryByID: %v", err)
	}
	if d.Status != "pending" || d.Attempts != 0 {
		t.Errorf("initial: status=%q attempts=%d, want pending/0", d.Status, d.Attempts)
	}

	// RecordSubscriberAttempt keeps status pending (River owns the retry decision).
	if err := ss.RecordSubscriberAttempt(ctx, id, 1, "boom", 500); err != nil {
		t.Fatalf("RecordSubscriberAttempt: %v", err)
	}
	if d, _ = ss.GetSubscriberDeliveryByID(ctx, id); d.Status != "pending" || d.Attempts != 1 {
		t.Errorf("after attempt: status=%q attempts=%d, want pending/1", d.Status, d.Attempts)
	}

	// MarkSubscriberFailed → terminal failed.
	if err := ss.MarkSubscriberFailed(ctx, id, 8, "gave up", 500); err != nil {
		t.Fatalf("MarkSubscriberFailed: %v", err)
	}
	if d, _ = ss.GetSubscriberDeliveryByID(ctx, id); d.Status != "failed" {
		t.Errorf("after MarkSubscriberFailed: status=%q, want failed", d.Status)
	}

	// A second row → delivered.
	id2, err := ss.InsertPendingForTest(ctx, wh.ID, "email.received", env)
	if err != nil {
		t.Fatalf("InsertPendingForTest 2: %v", err)
	}
	if err := ss.MarkDelivered(ctx, id2, 200); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	// ListDeliveriesByWebhook — all, then filtered by status.
	all, err := ss.ListDeliveriesByWebhook(ctx, wh.ID, "", 100, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListDeliveriesByWebhook: %v", err)
	}
	if len(all) < 2 {
		t.Errorf("list all: got %d, want >=2", len(all))
	}
	if failed, _ := ss.ListDeliveriesByWebhook(ctx, wh.ID, "failed", 100, time.Time{}, ""); len(failed) != 1 {
		t.Errorf("list failed: got %d, want 1", len(failed))
	}

	// Missing id → error.
	if _, err := ss.GetSubscriberDeliveryByID(ctx, "whd_does_not_exist"); err == nil {
		t.Error("GetSubscriberDeliveryByID on missing id: want error, got nil")
	}

	// Expiry sweep runs (nothing expired here — just exercise the path).
	if _, _, err := ss.DeleteExpiredSubscriberDeliveries(ctx); err != nil {
		t.Fatalf("DeleteExpiredSubscriberDeliveries: %v", err)
	}
}

// TestSubscriberStore_RecordAttemptOnTerminalRowIsNoop pins the status='pending'
// guard on RecordSubscriberAttempt: a worker mid-POST racing the expiry
// janitor's mark-failed (or any terminal write) must not resurrect the row to
// an in-flight state or clobber its terminal error when its non-final attempt
// result lands late.
func TestSubscriberStore_RecordAttemptOnTerminalRowIsNoop(t *testing.T) {
	pool := testutil.TestDB(t)
	istore := identity.NewStore(pool)
	ss := webhook.NewSubscriberStore(pool)
	ctx := context.Background()
	user, _ := istore.CreateOrGetUser(ctx, "wsd-guard@example.com", "Owner", "google-wsd-guard")
	wh, _ := istore.CreateWebhook(ctx, user.ID, "https://example.com/hook", "", []string{"email.received"}, identity.WebhookFilters{})

	id, err := ss.InsertPendingForTest(ctx, wh.ID, "email.received", []byte(`{"type":"email.received"}`))
	if err != nil {
		t.Fatalf("InsertPendingForTest: %v", err)
	}
	if err := ss.MarkSubscriberFailed(ctx, id, 8, "expired before delivery", 0); err != nil {
		t.Fatalf("MarkSubscriberFailed: %v", err)
	}

	// The late attempt write must be a no-op on the terminal row.
	if err := ss.RecordSubscriberAttempt(ctx, id, 3, "late attempt", 500); err != nil {
		t.Fatalf("RecordSubscriberAttempt: %v", err)
	}
	d, err := ss.GetSubscriberDeliveryByID(ctx, id)
	if err != nil {
		t.Fatalf("GetSubscriberDeliveryByID: %v", err)
	}
	if d.Status != "failed" {
		t.Errorf("status = %q, want failed (terminal row must not be resurrected)", d.Status)
	}
	if d.Attempts != 8 || d.LastError != "expired before delivery" {
		t.Errorf("attempts=%d last_error=%q, want untouched 8/%q", d.Attempts, d.LastError, "expired before delivery")
	}

	// Sanity: the same write still applies to a pending row.
	id2, err := ss.InsertPendingForTest(ctx, wh.ID, "email.received", []byte(`{"type":"email.received"}`))
	if err != nil {
		t.Fatalf("InsertPendingForTest 2: %v", err)
	}
	if err := ss.RecordSubscriberAttempt(ctx, id2, 1, "boom", 500); err != nil {
		t.Fatalf("RecordSubscriberAttempt (pending): %v", err)
	}
	if d, _ := ss.GetSubscriberDeliveryByID(ctx, id2); d.Status != "pending" || d.Attempts != 1 {
		t.Errorf("pending row after attempt: status=%q attempts=%d, want pending/1", d.Status, d.Attempts)
	}
}

// TestSubscriberStore_ExpiredPendingMarkedFailedNotSilentlyDeleted is the
// unguarded-janitor regression test: a row that reaches its 30-day TTL while
// still 'pending' (a strand, or a row snoozing behind a long-disabled webhook)
// must never silently vanish. The sweep instead marks it terminally 'failed'
// ("expired before delivery") — observable in the delivery-history API until
// the NEXT sweep deletes it as a normal terminal expired row. Terminal expired
// rows keep being deleted as before; unexpired rows are untouched.
func TestSubscriberStore_ExpiredPendingMarkedFailedNotSilentlyDeleted(t *testing.T) {
	pool := testutil.TestDB(t)
	istore := identity.NewStore(pool)
	ss := webhook.NewSubscriberStore(pool)
	ctx := context.Background()
	user, _ := istore.CreateOrGetUser(ctx, "wsd-expire@example.com", "Owner", "google-wsd-expire")
	wh, _ := istore.CreateWebhook(ctx, user.ID, "https://example.com/hook", "", []string{"email.received"}, identity.WebhookFilters{})
	env := []byte(`{"type":"email.received"}`)

	insert := func() string {
		t.Helper()
		id, err := ss.InsertPendingForTest(ctx, wh.ID, "email.received", env)
		if err != nil {
			t.Fatalf("InsertPendingForTest: %v", err)
		}
		return id
	}
	expire := func(id string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`UPDATE webhook_subscriber_deliveries SET expires_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
			t.Fatalf("expire %s: %v", id, err)
		}
	}

	expPending := insert()
	expFailed := insert()
	expDelivered := insert()
	freshPending := insert()
	if err := ss.MarkSubscriberFailed(ctx, expFailed, 8, "gave up", 500); err != nil {
		t.Fatalf("MarkSubscriberFailed: %v", err)
	}
	if err := ss.MarkDelivered(ctx, expDelivered, 200); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	expire(expPending)
	expire(expFailed)
	expire(expDelivered)

	// Sweep 1: terminal expired rows deleted; the expired PENDING row survives,
	// now observable-terminal instead of silently gone — and is reported via the
	// marked count (the janitor logs it and emits WebhookExpiredPending).
	n1, marked1, err := ss.DeleteExpiredSubscriberDeliveries(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSubscriberDeliveries: %v", err)
	}
	if n1 != 2 {
		t.Errorf("sweep 1 deleted %d rows, want 2 (the terminal expired rows only)", n1)
	}
	if marked1 != 1 {
		t.Errorf("sweep 1 marked %d rows, want 1 (the expired pending row)", marked1)
	}
	d, err := ss.GetSubscriberDeliveryByID(ctx, expPending)
	if err != nil {
		t.Fatalf("expired pending row is gone after sweep 1 — it was deleted while pending: %v", err)
	}
	if d.Status != "failed" {
		t.Errorf("expired pending row status = %q, want failed (marked, not deleted)", d.Status)
	}
	if d.LastError != "expired before delivery" {
		t.Errorf("expired pending row last_error = %q, want %q", d.LastError, "expired before delivery")
	}
	fresh, err := ss.GetSubscriberDeliveryByID(ctx, freshPending)
	if err != nil {
		t.Fatalf("unexpired pending row is gone after sweep 1: %v", err)
	}
	if fresh.Status != "pending" {
		t.Errorf("unexpired pending row status = %q, want untouched pending", fresh.Status)
	}

	// Sweep 2: the marked row is now a normal terminal expired row — deleted,
	// and nothing new to mark.
	n2, marked2, err := ss.DeleteExpiredSubscriberDeliveries(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSubscriberDeliveries (sweep 2): %v", err)
	}
	if n2 != 1 {
		t.Errorf("sweep 2 deleted %d rows, want 1 (the previously marked row)", n2)
	}
	if marked2 != 0 {
		t.Errorf("sweep 2 marked %d rows, want 0", marked2)
	}
	if _, err := ss.GetSubscriberDeliveryByID(ctx, expPending); err == nil {
		t.Error("marked row still present after sweep 2, want deleted")
	}
}

func TestSubscriberStore_MarkDeliveredBumpsLastDeliveredAt(t *testing.T) {
	pool := testutil.TestDB(t)
	istore := identity.NewStore(pool)
	wstore := webhook.NewSubscriberStore(pool)
	ctx := context.Background()
	user, _ := istore.CreateOrGetUser(ctx, "wsd-bump@example.com", "Owner", "google-wsd-bump")
	wh, _ := istore.CreateWebhook(ctx, user.ID, "https://example.com/hook", "", []string{"email.received"}, identity.WebhookFilters{})

	envelope, _ := json.Marshal(map[string]any{"type": "email.received"})
	_, _ = pool.Exec(ctx,
		`INSERT INTO webhook_subscriber_deliveries (id, webhook_id, event_type, event_payload, status)
		 VALUES ($1, $2, 'email.received', $3, 'pending')`,
		"whd_bump_"+wh.ID, wh.ID, envelope,
	)
	if err := wstore.MarkDelivered(ctx, "whd_bump_"+wh.ID, 200); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	var lastDelivered *time.Time
	pool.QueryRow(ctx, `SELECT last_delivered_at FROM webhooks WHERE id = $1`, wh.ID).Scan(&lastDelivered)
	if lastDelivered == nil {
		t.Error("last_delivered_at not bumped on the webhook row after MarkDelivered")
	}
}

// TestMarkSubscriberFailedIfPending pins the blind terminal write used when
// the worker cannot read the row (a final-attempt row-load failure): it must
// fail a PENDING row but never clobber a row that already reached a terminal
// state (the read failing means the row's true state is unknown).
func TestMarkSubscriberFailedIfPending(t *testing.T) {
	pool := testutil.TestDB(t)
	istore := identity.NewStore(pool)
	ss := webhook.NewSubscriberStore(pool)
	ctx := context.Background()
	user, _ := istore.CreateOrGetUser(ctx, "wsd-cond@example.com", "Owner", "google-wsd-cond")
	wh, _ := istore.CreateWebhook(ctx, user.ID, "https://example.com/hook", "", []string{"email.received"}, identity.WebhookFilters{})
	env := []byte(`{"type":"email.received"}`)

	// Pending row → failed, with the constant customer-safe last_error.
	pendingID, err := ss.InsertPendingForTest(ctx, wh.ID, "email.received", env)
	if err != nil {
		t.Fatalf("InsertPendingForTest: %v", err)
	}
	if err := ss.MarkSubscriberFailedIfPending(ctx, pendingID, 8, "internal error loading delivery", 0); err != nil {
		t.Fatalf("MarkSubscriberFailedIfPending on pending row: %v", err)
	}
	d, _ := ss.GetSubscriberDeliveryByID(ctx, pendingID)
	if d.Status != "failed" || d.Attempts != 8 || d.LastError != "internal error loading delivery" {
		t.Errorf("pending row: status=%q attempts=%d last_error=%q, want failed/8/constant", d.Status, d.Attempts, d.LastError)
	}

	// Already-failed row → untouched (the original, more informative
	// last_error must survive).
	failedID, err := ss.InsertPendingForTest(ctx, wh.ID, "email.received", env)
	if err != nil {
		t.Fatalf("InsertPendingForTest (failed): %v", err)
	}
	if err := ss.MarkSubscriberFailed(ctx, failedID, 8, "smtp 550 rejected", 550); err != nil {
		t.Fatalf("MarkSubscriberFailed: %v", err)
	}
	if err := ss.MarkSubscriberFailedIfPending(ctx, failedID, 8, "internal error loading delivery", 0); err != nil {
		t.Fatalf("MarkSubscriberFailedIfPending on failed row: %v", err)
	}
	if d, _ := ss.GetSubscriberDeliveryByID(ctx, failedID); d.Status != "failed" || d.LastError != "smtp 550 rejected" {
		t.Errorf("failed row: status=%q last_error=%q, want failed with the ORIGINAL last_error", d.Status, d.LastError)
	}

	// Delivered row → untouched (never clobbered).
	deliveredID, err := ss.InsertPendingForTest(ctx, wh.ID, "email.received", env)
	if err != nil {
		t.Fatalf("InsertPendingForTest 2: %v", err)
	}
	if err := ss.MarkDelivered(ctx, deliveredID, 200); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	if err := ss.MarkSubscriberFailedIfPending(ctx, deliveredID, 8, "internal error loading delivery", 0); err != nil {
		t.Fatalf("MarkSubscriberFailedIfPending on delivered row: %v", err)
	}
	if d, _ := ss.GetSubscriberDeliveryByID(ctx, deliveredID); d.Status != "delivered" {
		t.Errorf("delivered row: status=%q, want delivered (must not be clobbered)", d.Status)
	}

	// Missing row → no error, no panic (the blind write tolerates a gone row).
	if err := ss.MarkSubscriberFailedIfPending(ctx, "whd_does_not_exist", 8, "internal error loading delivery", 0); err != nil {
		t.Errorf("MarkSubscriberFailedIfPending on missing row: %v, want nil", err)
	}
}
