package webhookdelivery_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhook"
	"github.com/tokencanopy/e2a/internal/webhookdelivery"
)

type fakeDeliverer struct{ out webhook.DeliveryOutcome }

func (f fakeDeliverer) Deliver(_ context.Context, _ string, _ []byte, _, _, _, _ string) webhook.DeliveryOutcome {
	return f.out
}

type fakeWebhooks struct {
	wh  *identity.Webhook
	err error
}

func (f fakeWebhooks) GetWebhookByIDInternal(_ context.Context, _ string) (*identity.Webhook, error) {
	return f.wh, f.err
}

// seed creates a user + webhook (for the FK) and one pending Layer 2 delivery
// row, returning the delivery id and the SubscriberStore.
func seed(t *testing.T, prefix string) (string, *webhook.SubscriberStore, *identity.Store, *identity.Webhook) {
	t.Helper()
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, err := store.CreateOrGetUser(ctx, "owner-"+prefix+"@example.com", "Owner", "google-"+prefix)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	wh, err := store.CreateWebhook(ctx, user.ID, "https://example.com/hook", "", []string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	sub := webhook.NewSubscriberStore(pool)
	id, err := sub.InsertPendingForTest(ctx, wh.ID, "email.received", []byte(`{"type":"email.received"}`))
	if err != nil {
		t.Fatalf("InsertPendingForTest: %v", err)
	}
	return id, sub, store, wh
}

func statusOf(t *testing.T, sub *webhook.SubscriberStore, id string) *webhook.SubscriberDelivery {
	t.Helper()
	d, err := sub.GetSubscriberDeliveryByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSubscriberDeliveryByID: %v", err)
	}
	return d
}

func job(id string, attempt int) *river.Job[webhookdelivery.WebhookDeliverArgs] {
	return &river.Job[webhookdelivery.WebhookDeliverArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: webhookdelivery.MaxDeliveryAttempts, Kind: webhookdelivery.WebhookDeliverArgs{}.Kind()},
		Args:   webhookdelivery.WebhookDeliverArgs{DeliveryID: id},
	}
}

func TestDeliverWorker_Success(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-ok")
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}, fakeWebhooks{wh: wh})
	if err := w.Work(context.Background(), job(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if d := statusOf(t, sub, id); d.Status != "delivered" {
		t.Errorf("status = %q, want delivered", d.Status)
	}
}

func TestDeliverWorker_RetryableFailure(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-retry")
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: false, StatusCode: 500, Error: "boom"}}, fakeWebhooks{wh: wh})
	err := w.Work(context.Background(), job(id, 1))
	if err == nil {
		t.Fatal("Work returned nil on a retryable failure — River wouldn't retry")
	}
	d := statusOf(t, sub, id)
	if d.Status != "pending" {
		t.Errorf("status = %q, want pending (retryable)", d.Status)
	}
	if d.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", d.Attempts)
	}
}

func TestDeliverWorker_LastAttemptFails(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-final")
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: false, StatusCode: 500, Error: "boom"}}, fakeWebhooks{wh: wh})
	if err := w.Work(context.Background(), job(id, webhookdelivery.MaxDeliveryAttempts)); err == nil {
		t.Fatal("Work returned nil on final failed attempt")
	}
	if d := statusOf(t, sub, id); d.Status != "failed" {
		t.Errorf("status = %q, want failed (terminal)", d.Status)
	}
}

func TestDeliverWorker_DisabledSnoozes(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-disabled")
	wh.Enabled = false
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true}}, fakeWebhooks{wh: wh})
	err := w.Work(context.Background(), job(id, 1))
	if err == nil {
		t.Fatal("disabled webhook should return a snooze error, got nil")
	}
	// The delivery must be untouched (not delivered, not failed, no attempt burned).
	d := statusOf(t, sub, id)
	if d.Status != "pending" || d.Attempts != 0 {
		t.Errorf("disabled delivery mutated: status=%q attempts=%d, want pending/0", d.Status, d.Attempts)
	}
}

func TestDeliverWorker_DeletedWebhookCancels(t *testing.T) {
	id, sub, _, _ := seed(t, "wd-deleted")
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true}}, fakeWebhooks{err: errors.New("not found")})
	if err := w.Work(context.Background(), job(id, 1)); err == nil {
		t.Fatal("deleted webhook should return a cancel error")
	}
	if d := statusOf(t, sub, id); d.Status != "failed" {
		t.Errorf("status = %q, want failed", d.Status)
	}
}

// fakeEnq is a jobs.Enqueuer that hands back monotonic job ids.
type fakeEnq struct{ n int64 }

func (f *fakeEnq) Insert(_ context.Context, _ river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	f.n++
	return &rivertype.JobInsertResult{Job: &rivertype.JobRow{ID: f.n}}, nil
}
func (f *fakeEnq) InsertTx(_ context.Context, _ pgx.Tx, _ river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	f.n++
	return &rivertype.JobInsertResult{Job: &rivertype.JobRow{ID: f.n}}, nil
}

// TestReconcilePending: the one-shot migration enqueues a job + stamps job_id for
// every pending row with no job, and a re-run is idempotent (no double-enqueue).
// Uses a REAL River client: the reconciler also rescues rows whose stamped job is
// dead (missing/terminal), so idempotency requires the stamped ids to reference
// live river_job rows — a fake enqueuer's synthetic ids would read as pruned jobs.
func TestReconcilePending(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}
	store := identity.NewStore(pool)
	user, err := store.CreateOrGetUser(ctx, "owner-cutover@example.com", "Owner", "google-cutover")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	wh, err := store.CreateWebhook(ctx, user.ID, "https://example.com/hook", "", []string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	sub := webhook.NewSubscriberStore(pool)
	var ids []string
	for i := 0; i < 3; i++ {
		id, err := sub.InsertPendingForTest(ctx, wh.ID, "email.received", []byte(`{}`))
		if err != nil {
			t.Fatalf("InsertPendingForTest: %v", err)
		}
		ids = append(ids, id)
	}

	j := webhookdelivery.NewJobs(sub, fakeDeliverer{}, fakeWebhooks{wh: wh}, pool)
	client, err := jobs.New(pool, jobs.Config{}, j)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	j.SetEnqueuer(client)

	res, err := j.ReconcilePending(ctx, pool)
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if res.Total() != 3 || res.Enqueued != 3 {
		t.Errorf("cutover result = %+v, want 3 on the IS-NULL arm", res)
	}
	// Every row got a job_id.
	for _, id := range ids {
		var jobID *int64
		if err := pool.QueryRow(ctx, `SELECT job_id FROM webhook_subscriber_deliveries WHERE id=$1`, id).Scan(&jobID); err != nil {
			t.Fatalf("read job_id: %v", err)
		}
		if jobID == nil {
			t.Errorf("row %s has no job_id after cutover", id)
		}
	}
	// Idempotent: a re-run enqueues nothing.
	res2, err := j.ReconcilePending(ctx, pool)
	if err != nil {
		t.Fatalf("ReconcilePending re-run: %v", err)
	}
	if res2.Total() != 0 {
		t.Errorf("cutover re-run enqueued %d, want 0 (idempotent)", res2.Total())
	}
}

// ── Webhook-attempt SLI (docs/observability.md) ─────────────────

// attemptRec is one recorded WebhookAttempt call.
type attemptRec struct {
	outcome     string
	statusClass string
	seconds     float64
}

// fakeMetrics records WebhookAttempt calls for assertion.
type fakeMetrics struct{ attempts []attemptRec }

func (f *fakeMetrics) WebhookAttempt(outcome, statusClass string, seconds float64) {
	f.attempts = append(f.attempts, attemptRec{outcome, statusClass, seconds})
}

func (f *fakeMetrics) WebhookDeliveryRescued(int) {} // not under test here

// one asserts exactly one attempt was recorded and returns it.
func (f *fakeMetrics) one(t *testing.T) attemptRec {
	t.Helper()
	if len(f.attempts) != 1 {
		t.Fatalf("recorded %d attempts, want 1: %+v", len(f.attempts), f.attempts)
	}
	return f.attempts[0]
}

func TestDeliverWorker_Metrics_Delivered(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-m-ok")
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	got := fm.one(t)
	if got.outcome != "delivered" || got.statusClass != "2xx" {
		t.Errorf("attempt = %+v, want delivered/2xx", got)
	}
}

func TestDeliverWorker_Metrics_RetryableFailure(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-m-retry")
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: false, StatusCode: 503, Error: "boom"}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(id, 1)); err == nil {
		t.Fatal("Work returned nil on a retryable failure")
	}
	got := fm.one(t)
	if got.outcome != "retryable_failure" || got.statusClass != "5xx" {
		t.Errorf("attempt = %+v, want retryable_failure/5xx", got)
	}
}

func TestDeliverWorker_Metrics_Exhausted(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-m-final")
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: false, StatusCode: 500, Error: "boom"}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(id, webhookdelivery.MaxDeliveryAttempts)); err == nil {
		t.Fatal("Work returned nil on final failed attempt")
	}
	got := fm.one(t)
	if got.outcome != "exhausted" || got.statusClass != "5xx" {
		t.Errorf("attempt = %+v, want exhausted/5xx", got)
	}
}

func TestDeliverWorker_Metrics_DisabledSkip(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-m-disabled")
	wh.Enabled = false
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(id, 1)); err == nil {
		t.Fatal("disabled webhook should return a snooze error, got nil")
	}
	got := fm.one(t)
	if got.outcome != "skipped_disabled" || got.statusClass != "none" || got.seconds >= 0 {
		t.Errorf("attempt = %+v, want skipped_disabled/none/negative (no POST → no duration sample)", got)
	}
}

func TestDeliverWorker_Metrics_DeletedWebhook(t *testing.T) {
	id, sub, _, _ := seed(t, "wd-m-deleted")
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true}}, fakeWebhooks{err: errors.New("not found")}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(id, 1)); err == nil {
		t.Fatal("deleted webhook should return a cancel error")
	}
	got := fm.one(t)
	if got.outcome != "webhook_deleted" || got.statusClass != "none" || got.seconds >= 0 {
		t.Errorf("attempt = %+v, want webhook_deleted/none/negative (no POST → no duration sample)", got)
	}
}

// TestDeliverWorker_Metrics_StatusClassMapping pins the code→class label
// mapping through the retryable seam, including 0 → "none" (no HTTP
// response: connect/DNS/SSRF-blocked).
func TestDeliverWorker_Metrics_StatusClassMapping(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, "none"},
		{199, "1xx"},
		{404, "4xx"},
		{503, "5xx"},
	}
	for _, tc := range cases {
		id, sub, _, wh := seed(t, fmt.Sprintf("wd-m-class-%d", tc.code))
		fm := &fakeMetrics{}
		w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: false, StatusCode: tc.code, Error: "boom"}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
		if err := w.Work(context.Background(), job(id, 1)); err == nil {
			t.Fatalf("code %d: Work returned nil on a failure", tc.code)
		}
		if got := fm.one(t); got.statusClass != tc.want {
			t.Errorf("code %d: statusClass = %q, want %q", tc.code, got.statusClass, tc.want)
		}
	}
}

func TestDeliverWorker_NextRetryMatchesEnvelope(t *testing.T) {
	w := webhookdelivery.NewDeliverWorker(nil, nil, nil)
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour}
	if webhookdelivery.MaxDeliveryAttempts != 8 {
		t.Fatalf("MaxDeliveryAttempts = %d, want 8", webhookdelivery.MaxDeliveryAttempts)
	}
	var total time.Duration
	for i, wantDur := range want {
		attempt := i + 1 // attempts 1..7
		total += wantDur
		got := time.Until(w.NextRetry(job("x", attempt))).Round(time.Second)
		if diff := got - wantDur; diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("attempt %d: next retry in %v, want ~%v", attempt, got, wantDur)
		}
	}
	if total != 29*time.Hour+21*time.Minute {
		t.Errorf("retry envelope spans %v, want 29h21m", total)
	}
}
