package webhookdelivery_test

import (
	"context"
	"errors"
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

// recordingDeliverer captures the arguments the worker passes to Deliver so tests
// can assert signing-secret handling, and counts calls so no-op paths can prove
// the endpoint was never hit.
type recordingDeliverer struct {
	out        webhook.DeliveryOutcome
	calls      int
	secretPrev string
}

func (r *recordingDeliverer) Deliver(_ context.Context, _ string, _ []byte, _, secretPrev, _, _ string) webhook.DeliveryOutcome {
	r.calls++
	r.secretPrev = secretPrev
	return r.out
}

// failEnq is a jobs.Enqueuer whose inserts always fail.
type failEnq struct{ err error }

func (f failEnq) Insert(_ context.Context, _ river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return nil, f.err
}
func (f failEnq) InsertTx(_ context.Context, _ pgx.Tx, _ river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return nil, f.err
}

// TestDeliverWorker_MissingDeliveryRowIsNoop: the delivery row can disappear
// between enqueue and execution (webhook deleted, cascade) — Work treats a gone
// row as done rather than erroring (an error would burn River retries forever).
func TestDeliverWorker_MissingDeliveryRowIsNoop(t *testing.T) {
	pool := testutil.TestDB(t)
	sub := webhook.NewSubscriberStore(pool)
	rec := &recordingDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}
	w := webhookdelivery.NewDeliverWorker(sub, rec, fakeWebhooks{err: errors.New("unreachable")})
	if err := w.Work(context.Background(), job("whd_does_not_exist", 1)); err != nil {
		t.Fatalf("Work on missing row = %v, want nil (nothing to do)", err)
	}
	if rec.calls != 0 {
		t.Errorf("deliverer called %d times for a missing row, want 0", rec.calls)
	}
}

// TestDeliverWorker_AlreadyDeliveredIsNoop: a re-driven job after a crash
// post-MarkDelivered must not POST the endpoint again — the endpoint sees the
// delivery exactly once even though River is at-least-once at the job level.
func TestDeliverWorker_AlreadyDeliveredIsNoop(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-redeliver")
	ctx := context.Background()
	if err := sub.MarkDelivered(ctx, id, 200); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	rec := &recordingDeliverer{out: webhook.DeliveryOutcome{Success: false, StatusCode: 500, Error: "should never fire"}}
	w := webhookdelivery.NewDeliverWorker(sub, rec, fakeWebhooks{wh: wh})
	if err := w.Work(ctx, job(id, 1)); err != nil {
		t.Fatalf("Work on delivered row = %v, want nil (idempotent no-op)", err)
	}
	if rec.calls != 0 {
		t.Errorf("deliverer called %d times for an already-delivered row, want 0 (duplicate POST)", rec.calls)
	}
}

// TestDeliverWorker_DBErrorIsRetryable: a transient store error on the initial
// fetch is returned verbatim so River retries — a crash-safe redrive beats a
// silent skip that would strand the row.
func TestDeliverWorker_DBErrorIsRetryable(t *testing.T) {
	ctx := context.Background()
	pool, err := testutil.OpenPreparedTestDB(ctx, testutil.TestDBURL())
	if err != nil {
		t.Skipf("test database not available: %v", err)
	}
	sub := webhook.NewSubscriberStore(pool)
	pool.Close() // simulate a DB outage

	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{}, fakeWebhooks{})
	if err := w.Work(ctx, job("whd_any", 1)); err == nil {
		t.Fatal("Work with a dead DB returned nil — River would not retry a transient error")
	}
}

// TestDeliverWorker_ExpiredPrevSigningSecretNotHonored: once the rotation grace
// window closes, the previous signing secret must not be sent — a consumer still
// trusting it would accept signatures with a retired secret.
func TestDeliverWorker_ExpiredPrevSigningSecretNotHonored(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-prev-expired")
	past := time.Now().Add(-time.Hour)
	wh.SigningSecret = "current-secret"
	wh.SigningSecretPrev = "retired-secret"
	wh.SigningSecretPrevExpiresAt = &past

	rec := &recordingDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}
	w := webhookdelivery.NewDeliverWorker(sub, rec, fakeWebhooks{wh: wh})
	if err := w.Work(context.Background(), job(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("deliverer called %d times, want 1", rec.calls)
	}
	if rec.secretPrev != "" {
		t.Errorf("prev secret after grace expiry = %q, want empty (retired secret must not sign)", rec.secretPrev)
	}
}

// TestDeliverWorker_PrevSigningSecretWithinGraceIsHonored: inside the rotation
// grace window the previous secret IS passed through, so consumers mid-rotation
// can verify against either secret.
func TestDeliverWorker_PrevSigningSecretWithinGraceIsHonored(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-prev-grace")
	future := time.Now().Add(time.Hour)
	wh.SigningSecret = "current-secret"
	wh.SigningSecretPrev = "rotating-secret"
	wh.SigningSecretPrevExpiresAt = &future

	rec := &recordingDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}
	w := webhookdelivery.NewDeliverWorker(sub, rec, fakeWebhooks{wh: wh})
	if err := w.Work(context.Background(), job(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if rec.secretPrev != "rotating-secret" {
		t.Errorf("prev secret within grace = %q, want %q", rec.secretPrev, "rotating-secret")
	}
}

// TestDeliverWorker_TerminalWriteFailureIsReturned: when the webhook is gone AND
// the terminal 'failed' write itself keeps failing, Work must surface the store
// error (River retries) rather than cancelling with an unmarked row — a cancel
// would strand the row 'pending' with a dead job the reconciler can't see.
// The negative attempt number trips the attempts >= 0 CHECK constraint on every
// try, exhausting markFailedReliably's bounded retries deterministically.
func TestDeliverWorker_TerminalWriteFailureIsReturned(t *testing.T) {
	id, sub, _, _ := seed(t, "wd-markfail")
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true}},
		fakeWebhooks{err: identity.ErrWebhookNotFound})
	err := w.Work(context.Background(), job(id, -1))
	if err == nil {
		t.Fatal("Work returned nil even though the terminal 'failed' write never succeeded")
	}
	// The row must NOT have been marked failed — every write errored.
	if d := statusOf(t, sub, id); d.Status != "pending" {
		t.Errorf("status = %q, want pending (the failing terminal write must not half-apply)", d.Status)
	}
}

// TestDeliverWorker_TerminalWriteHonorsContextCancel: a cancelled context aborts
// markFailedReliably's backoff sleep instead of sleeping out the full retry
// budget — the worker stays responsive to River's stop signal. The row lock makes
// the terminal write block until the deadline so the cancel branch fires.
func TestDeliverWorker_TerminalWriteHonorsContextCancel(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	// Seed inline on this one pool (the shared seed helper acquires its own pool,
	// and a second testutil.TestDB call would truncate the row out from under us).
	store := identity.NewStore(pool)
	user, err := store.CreateOrGetUser(ctx, "owner-wd-markcancel@example.com", "Owner", "google-wd-markcancel")
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

	// Hold an exclusive lock on the delivery row so MarkSubscriberFailed's UPDATE
	// blocks (a plain SELECT does not, so the initial fetch still succeeds).
	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	defer lockTx.Rollback(ctx)
	if _, err := lockTx.Exec(ctx,
		`UPDATE webhook_subscriber_deliveries SET last_error = 'lock-holder' WHERE id = $1`, id); err != nil {
		t.Fatalf("lock row: %v", err)
	}

	workCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true}},
		fakeWebhooks{err: identity.ErrWebhookNotFound})
	err = w.Work(workCtx, job(id, 1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Work error = %v, want context.DeadlineExceeded (cancel must abort the retry backoff)", err)
	}
}

// TestNextRetry_OutOfRangeAttemptsFallsBackToClientPolicy: attempts outside the
// frozen envelope (0 is invalid; MaxDeliveryAttempts is terminal and discarded
// by River anyway) return the zero time so River's client-wide policy applies —
// never a panic from indexing past the backoff table.
func TestNextRetry_OutOfRangeAttemptsFallsBackToClientPolicy(t *testing.T) {
	w := webhookdelivery.NewDeliverWorker(nil, nil, nil)
	for _, attempt := range []int{0, webhookdelivery.MaxDeliveryAttempts, webhookdelivery.MaxDeliveryAttempts + 5} {
		if got := w.NextRetry(job("x", attempt)); !got.IsZero() {
			t.Errorf("NextRetry(attempt %d) = %v, want zero time (fall back to client policy)", attempt, got)
		}
	}
}

// TestEnqueueDelivery_UnknownDeliveryID: enqueueing for a delivery row that
// doesn't exist is an error, not a silent no-op — the caller (/test endpoint,
// redelivery API) needs to know the row it just created isn't the one found.
func TestEnqueueDelivery_UnknownDeliveryID(t *testing.T) {
	pool := testutil.TestDB(t)
	sub := webhook.NewSubscriberStore(pool)
	j := webhookdelivery.NewJobs(sub, fakeDeliverer{}, fakeWebhooks{}, pool)
	j.SetEnqueuer(&fakeEnq{})
	if err := j.EnqueueDelivery(context.Background(), pool, "whd_does_not_exist"); err == nil {
		t.Fatal("EnqueueDelivery on a missing row returned nil, want an error")
	}
}

// TestEnqueueDeliveryTx_EnqueuerError: a River insert failure inside the outbox
// tx propagates (and returns no job id) so the caller's transaction rolls back —
// the Layer 2 row and its job must commit together or not at all.
func TestEnqueueDeliveryTx_EnqueuerError(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	sub := webhook.NewSubscriberStore(pool)
	wantErr := errors.New("river insert boom")
	j := webhookdelivery.NewJobs(sub, fakeDeliverer{}, fakeWebhooks{}, pool)
	j.SetEnqueuer(failEnq{err: wantErr})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	jobID, err := j.EnqueueDeliveryTx(ctx, tx, "whd_any")
	if !errors.Is(err, wantErr) {
		t.Errorf("EnqueueDeliveryTx error = %v, want %v", err, wantErr)
	}
	if jobID != 0 {
		t.Errorf("EnqueueDeliveryTx job id = %d, want 0 on failure", jobID)
	}
}

// TestReconcileWorker_EndToEnd drives the live reconciler through a REAL River
// client: a stranded pending row (job_id IS NULL — the /test or redelivery
// separate-tx path, or an outbox-drain crash window) gets a webhook_deliver job
// enqueued and job_id stamped within one reconcile run, not just on restart.
func TestReconcileWorker_EndToEnd(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}

	id1, sub, _, wh := seed(t, "wd-reconcile-a")
	id2, err := sub.InsertPendingForTest(ctx, wh.ID, "email.received", []byte(`{"type":"email.received"}`))
	if err != nil {
		t.Fatalf("InsertPendingForTest: %v", err)
	}

	j := webhookdelivery.NewJobs(sub, fakeDeliverer{}, fakeWebhooks{wh: wh}, pool)
	client, err := jobs.New(pool, jobs.Config{}, j)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	j.SetEnqueuer(client)

	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = client.Stop(stopCtx)
	})

	// Fire the reconcile worker directly (the periodic runs every minute — too
	// slow for a test).
	if _, err := client.Insert(ctx, webhookdelivery.WebhookReconcileArgs{}, &river.InsertOpts{Queue: jobs.QueueMaintenance}); err != nil {
		t.Fatalf("insert reconcile job: %v", err)
	}

	// Both stranded rows must get a job_id stamped by the reconcile run.
	jobIDOf := func(id string) *int64 {
		var jobID *int64
		if err := pool.QueryRow(ctx, `SELECT job_id FROM webhook_subscriber_deliveries WHERE id=$1`, id).Scan(&jobID); err != nil {
			t.Fatalf("read job_id for %s: %v", id, err)
		}
		return jobID
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if jobIDOf(id1) != nil && jobIDOf(id2) != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("reconcile worker did not stamp job_id: %s=%v %s=%v", id1, jobIDOf(id1), id2, jobIDOf(id2))
}
