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
type fakeMetrics struct {
	attempts []attemptRec
	firstTry []float64
}

func (f *fakeMetrics) WebhookAttempt(outcome, statusClass string, seconds float64) {
	f.attempts = append(f.attempts, attemptRec{outcome, statusClass, seconds})
}

func (f *fakeMetrics) WebhookDeliveryRescued(int) {} // not under test here
func (f *fakeMetrics) WebhookFirstAttemptLatency(seconds float64) {
	f.firstTry = append(f.firstTry, seconds)
}

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

// ── Event→first-attempt latency SLI (docs/observability.md) ──────

// seedEventLinked creates a user + webhook, a webhook_events row aged
// eventAge, and a pending delivery row linked to it (mirroring the fan-out
// insert). replay=true marks the delivery as a customer-initiated replay
// (replay_id set), which must never feed the first-attempt SLI.
func seedEventLinked(t *testing.T, prefix string, eventAge time.Duration, replay bool) (string, *webhook.SubscriberStore, *identity.Store, *identity.Webhook) {
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
	eventID := "evt_" + prefix
	if _, err := pool.Exec(ctx,
		`INSERT INTO webhook_events (id, user_id, type, envelope, created_at)
		 VALUES ($1, $2, 'email.received', '{}'::jsonb, now() - make_interval(secs => $3))`,
		eventID, user.ID, eventAge.Seconds()); err != nil {
		t.Fatalf("insert webhook_events row: %v", err)
	}
	deliveryID := "whd_" + prefix
	var replayID *string
	if replay {
		replayID = &eventID // any non-null value marks a replay row
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO webhook_subscriber_deliveries
		     (id, webhook_id, event_id, replay_id, event_type, event_payload, status)
		 VALUES ($1, $2, $3, $4, 'email.received', '{}'::jsonb, 'pending')`,
		deliveryID, wh.ID, eventID, replayID); err != nil {
		t.Fatalf("insert delivery row: %v", err)
	}
	sub := webhook.NewSubscriberStore(pool)
	return deliveryID, sub, store, wh
}

func TestDeliverWorker_Metrics_FirstAttemptLatencyObservedOnFirstAttempt(t *testing.T) {
	id, sub, _, wh := seedEventLinked(t, "lat-first", 45*time.Second, false)
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(fm.firstTry) != 1 {
		t.Fatalf("first-attempt latencies = %v, want exactly one sample", fm.firstTry)
	}
	if got := fm.firstTry[0]; got < 40 || got > 50 {
		t.Errorf("first-attempt latency = %.1fs, want ~45s (attempt start − event created_at)", got)
	}
}

func TestDeliverWorker_Metrics_FirstAttemptLatencyAlsoOnFailedFirstAttempt(t *testing.T) {
	// The SLI measures event→first-attempt regardless of that attempt's
	// outcome — a failed first POST is still the first attempt.
	id, sub, _, wh := seedEventLinked(t, "lat-fail", 30*time.Second, false)
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: false, StatusCode: 500, Error: "boom"}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(id, 1)); err == nil {
		t.Fatal("Work returned nil on a retryable failure")
	}
	if len(fm.firstTry) != 1 {
		t.Fatalf("first-attempt latencies = %v, want one sample even for a failed first attempt", fm.firstTry)
	}
	if got := fm.firstTry[0]; got < 25 || got > 35 {
		t.Errorf("first-attempt latency = %.1fs, want ~30s", got)
	}
}

func TestDeliverWorker_Metrics_FirstAttemptLatencySkipsRetries(t *testing.T) {
	id, sub, _, wh := seedEventLinked(t, "lat-retry", 45*time.Second, false)
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: false, StatusCode: 500, Error: "boom"}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	// First attempt fails and records the attempt; the retry (job attempt 2)
	// must NOT observe — the SLO is event→FIRST attempt only.
	if err := w.Work(context.Background(), job(id, 1)); err == nil {
		t.Fatal("first attempt should fail (retryable)")
	}
	if err := w.Work(context.Background(), job(id, 2)); err == nil {
		t.Fatal("second attempt should fail (retryable)")
	}
	if len(fm.firstTry) != 1 {
		t.Errorf("first-attempt latencies = %v, want exactly one sample across two attempts", fm.firstTry)
	}
}

func TestDeliverWorker_Metrics_FirstAttemptLatencySkipsNoPostOutcomes(t *testing.T) {
	// skipped_disabled and webhook_deleted perform no HTTP POST, so there is
	// no attempt to time — they must not observe even on job attempt 1.
	idDisabled, sub, _, wh := seedEventLinked(t, "lat-disabled", 45*time.Second, false)
	wh.Enabled = false
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(idDisabled, 1)); err == nil {
		t.Fatal("disabled webhook should snooze")
	}

	idDeleted, _, _, _ := seedEventLinked(t, "lat-deleted", 45*time.Second, false)
	wDeleted := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true}}, fakeWebhooks{err: errors.New("not found")}).WithMetrics(fm)
	if err := wDeleted.Work(context.Background(), job(idDeleted, 1)); err == nil {
		t.Fatal("deleted webhook should cancel")
	}

	if len(fm.firstTry) != 0 {
		t.Errorf("first-attempt latencies = %v, want none for no-POST outcomes", fm.firstTry)
	}
}

func TestDeliverWorker_Metrics_FirstAttemptLatencySkipsReplayRows(t *testing.T) {
	// A replay row's baseline would be the ORIGINAL event's created_at —
	// observing it would record the customer's replay lag (days) as a giant
	// false outlier. Only first-delivery rows (replay_id NULL) feed the SLI.
	id, sub, _, wh := seedEventLinked(t, "lat-replay", 72*time.Hour, true)
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(fm.firstTry) != 0 {
		t.Errorf("first-attempt latencies = %v, want none for a replay row", fm.firstTry)
	}
}

func TestDeliverWorker_Metrics_FirstAttemptLatencySkipsEventlessRows(t *testing.T) {
	// Test-endpoint deliveries (InsertPendingForTest) carry no event link —
	// there is no event created_at to measure against, so no sample.
	id, sub, _, wh := seed(t, "wd-m-noevent")
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(fm.firstTry) != 0 {
		t.Errorf("first-attempt latencies = %v, want none without an event link", fm.firstTry)
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

func TestDeliverWorker_Metrics_FirstAttemptLatencyObservedWhenFirstPostIsNotRiverAttemptOne(t *testing.T) {
	// If a transient pre-POST error (e.g. a DB blip on the row load) consumes
	// River attempt 1, the delivery's first HTTP attempt happens at job
	// attempt 2 — it is STILL the first attempt and must be observed, or the
	// slowest first attempts (the incident population) never reach the SLI.
	id, sub, _, wh := seedEventLinked(t, "lat-att2", 45*time.Second, false)
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	if err := w.Work(context.Background(), job(id, 2)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(fm.firstTry) != 1 {
		t.Fatalf("first-attempt latencies = %v, want one sample for the true first POST", fm.firstTry)
	}
	if got := fm.firstTry[0]; got < 40 || got > 50 {
		t.Errorf("first-attempt latency = %.1fs, want ~45s", got)
	}
}

func TestDeliverWorker_Metrics_FirstAttemptLatencySkipsSnoozedJobs(t *testing.T) {
	// A disabled webhook snoozes the River job WITHOUT burning an attempt
	// (River counts snoozes in job metadata), so the eventual first POST
	// still runs at job attempt 1 — but its latency would be the customer's
	// entire disabled window (hours to days), not e2a's event→attempt
	// latency. Snoozed jobs never observe.
	id, sub, _, wh := seedEventLinked(t, "lat-snoozed", 26*time.Hour, false)
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	snoozedJob := job(id, 1)
	snoozedJob.Metadata = []byte(`{"snoozes":3}`)
	if err := w.Work(context.Background(), snoozedJob); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(fm.firstTry) != 0 {
		t.Errorf("first-attempt latencies = %v, want none for a job that sat through a disabled window", fm.firstTry)
	}
}

func TestDeliverWorker_Metrics_FirstAttemptLatencyObservedDespiteBackoffSchedule(t *testing.T) {
	// Chained PRE-POST failures (e.g. a DB blip on the row load) record no
	// attempt, so the row still shows attempts == 0 when the true first POST
	// finally runs — deep into the retry envelope (scheduled_at far past
	// created_at). That first POST MUST observe: a schedule-shift heuristic
	// would false-skip exactly the outage population this SLI exists to
	// catch. Only an actual snooze (metadata) excludes.
	id, sub, _, wh := seedEventLinked(t, "lat-backoff", 90*time.Minute, false)
	fm := &fakeMetrics{}
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}, fakeWebhooks{wh: wh}).WithMetrics(fm)
	lateJob := job(id, 4)
	lateJob.CreatedAt = time.Now().Add(-90 * time.Minute).UTC()
	lateJob.ScheduledAt = time.Now().Add(-time.Minute).UTC() // deep in the backoff envelope, never snoozed
	if err := w.Work(context.Background(), lateJob); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(fm.firstTry) != 1 {
		t.Fatalf("first-attempt latencies = %v, want one sample for the true first POST after chained pre-POST failures", fm.firstTry)
	}
	if got := fm.firstTry[0]; got < 89*time.Minute.Seconds() || got > 91*time.Minute.Seconds() {
		t.Errorf("first-attempt latency = %.0fs, want ~90min (the honest outage signal)", got)
	}
}
