package webhookdelivery_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhook"
	"github.com/tokencanopy/e2a/internal/webhookdelivery"
)

// seedRiverJob inserts a bare river_job row in the given state and returns its
// id. Terminal states get the finalized_at river_job's CHECK requires. Caller
// must have applied River's schema (jobs.Migrate).
func seedRiverJob(t *testing.T, pool *pgxpool.Pool, kind, state string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO river_job (state, kind, args, max_attempts, finalized_at)
		 VALUES ($1::river_job_state, $2, '{}'::jsonb, 8,
		         CASE WHEN $1 IN ('cancelled','completed','discarded') THEN now() END)
		 RETURNING id`, state, kind).Scan(&id); err != nil {
		t.Fatalf("insert river_job(%s): %v", state, err)
	}
	return id
}

// TestReconcilePending_RescuesDeadJob is the CW-2 (delivery strand) regression
// test: when the final delivery attempt's terminal 'failed' write is lost (a DB
// outage in exactly that window), River discards the job and the Layer 2 row
// stays 'pending' with a job_id pointing at a discarded — or later pruned —
// river_job. The reconciler must treat that dead-job row exactly like a
// job_id IS NULL strand: enqueue a fresh webhook_deliver job, re-stamp job_id,
// and let the normal delivery path terminalize the row.
func TestReconcilePending_RescuesDeadJob(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}

	// seed() truncates (its own testutil.TestDB call) then seeds — call it after
	// Migrate, before stamping anything.
	idDiscarded, sub, _, wh := seed(t, "wd-dead")
	idMissing, err := sub.InsertPendingForTest(ctx, wh.ID, "email.received", []byte(`{"type":"email.received"}`))
	if err != nil {
		t.Fatalf("InsertPendingForTest: %v", err)
	}
	idLive, err := sub.InsertPendingForTest(ctx, wh.ID, "email.received", []byte(`{"type":"email.received"}`))
	if err != nil {
		t.Fatalf("InsertPendingForTest: %v", err)
	}
	// A dead-job row still INSIDE the quiet-age envelope: must not be rescued
	// this pass (its job could plausibly still have been retrying).
	idFreshDead, err := sub.InsertPendingForTest(ctx, wh.ID, "email.received", []byte(`{"type":"email.received"}`))
	if err != nil {
		t.Fatalf("InsertPendingForTest: %v", err)
	}

	discardedJob := seedRiverJob(t, pool, "webhook_deliver", "discarded")
	freshDeadJob := seedRiverJob(t, pool, "webhook_deliver", "discarded")
	liveJob := seedRiverJob(t, pool, "webhook_deliver", "available")
	missingJob := int64(1) << 60 // never a real river_job id (pruned strand)
	stamp := func(deliveryID string, jobID int64) {
		if _, err := pool.Exec(ctx,
			`UPDATE webhook_subscriber_deliveries SET job_id = $2 WHERE id = $1`, deliveryID, jobID); err != nil {
			t.Fatalf("stamp %s: %v", deliveryID, err)
		}
	}
	stamp(idDiscarded, discardedJob)
	stamp(idMissing, missingJob)
	stamp(idLive, liveJob)
	stamp(idFreshDead, freshDeadJob)

	// The rescue arm is age-gated to rows quiet longer than the retry envelope
	// (rescueQuietFor = 30h): backdate the two real strands past it — one via
	// last_attempt_at, one via created_at only (the COALESCE arm for a
	// never-attempted row). idFreshDead stays at now() (in-envelope).
	if _, err := pool.Exec(ctx,
		`UPDATE webhook_subscriber_deliveries SET last_attempt_at = now() - interval '31 hours' WHERE id = $1`,
		idDiscarded); err != nil {
		t.Fatalf("backdate %s: %v", idDiscarded, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE webhook_subscriber_deliveries SET created_at = now() - interval '31 hours' WHERE id = $1`,
		idMissing); err != nil {
		t.Fatalf("backdate %s: %v", idMissing, err)
	}

	// Real River client so rescued rows get real (alive) jobs — that is what
	// makes the re-run idempotency assertion meaningful.
	j := webhookdelivery.NewJobs(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}, fakeWebhooks{wh: wh}, pool)
	client, err := jobs.New(pool, jobs.Config{}, j)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	j.SetEnqueuer(client)

	res, err := j.ReconcilePending(ctx, pool)
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if res.Rescued != 2 || res.Enqueued != 0 {
		t.Errorf("reconcile result = %+v, want exactly 2 rescues (discarded-job + missing-job strands)", res)
	}

	jobIDOf := func(id string) int64 {
		var jobID *int64
		if err := pool.QueryRow(ctx,
			`SELECT job_id FROM webhook_subscriber_deliveries WHERE id = $1`, id).Scan(&jobID); err != nil {
			t.Fatalf("read job_id for %s: %v", id, err)
		}
		if jobID == nil {
			t.Fatalf("row %s has NULL job_id", id)
		}
		return *jobID
	}
	if got := jobIDOf(idDiscarded); got == discardedJob {
		t.Errorf("discarded-job row still stamped %d, want a fresh job id", got)
	}
	if got := jobIDOf(idMissing); got == missingJob {
		t.Errorf("missing-job row still stamped %d, want a fresh job id", got)
	}
	if got := jobIDOf(idLive); got != liveJob {
		t.Errorf("live-job row job id = %d, want untouched %d", got, liveJob)
	}
	if got := jobIDOf(idFreshDead); got != freshDeadJob {
		t.Errorf("in-envelope dead-job row job id = %d, want untouched %d (quiet-age gate)", got, freshDeadJob)
	}
	// The fresh jobs are real webhook_deliver river_jobs carrying the row ids.
	for _, id := range []string{idDiscarded, idMissing} {
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM river_job WHERE kind = 'webhook_deliver' AND state = 'available' AND args->>'delivery_id' = $1`,
			id).Scan(&count); err != nil {
			t.Fatalf("count fresh jobs for %s: %v", id, err)
		}
		if count != 1 {
			t.Errorf("fresh available webhook_deliver jobs for %s = %d, want 1", id, count)
		}
	}

	// Idempotent re-run (single reconciler): the rescued rows now carry live
	// jobs — a re-run enqueues nothing. (Concurrent reconcilers are
	// at-least-once on this arm; see jobs.ReconcilePending.)
	res2, err := j.ReconcilePending(ctx, pool)
	if err != nil {
		t.Fatalf("ReconcilePending re-run: %v", err)
	}
	if res2.Total() != 0 {
		t.Errorf("re-run enqueued %d rows, want 0 (rescued rows carry live jobs)", res2.Total())
	}

	// And the rescued row actually terminalizes: drive the worker for it.
	w := webhookdelivery.NewDeliverWorker(sub, fakeDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}, fakeWebhooks{wh: wh})
	if err := w.Work(ctx, job(idDiscarded, 1)); err != nil {
		t.Fatalf("Work on rescued row: %v", err)
	}
	if d := statusOf(t, sub, idDiscarded); d.Status != "delivered" {
		t.Errorf("rescued row status = %q, want delivered", d.Status)
	}
}

// TestDeliverWorker_FailedRowIsNoop: a job waking up against a row already in a
// terminal 'failed' state — the expiry janitor marked it (expired before
// delivery) or the final-attempt write landed after all — must not POST the
// endpoint. Mirrors the delivered no-op guard.
func TestDeliverWorker_FailedRowIsNoop(t *testing.T) {
	id, sub, _, wh := seed(t, "wd-failed-noop")
	ctx := context.Background()
	if err := sub.MarkSubscriberFailed(ctx, id, 8, "expired before delivery", 0); err != nil {
		t.Fatalf("MarkSubscriberFailed: %v", err)
	}
	rec := &recordingDeliverer{out: webhook.DeliveryOutcome{Success: true, StatusCode: 200}}
	w := webhookdelivery.NewDeliverWorker(sub, rec, fakeWebhooks{wh: wh})
	if err := w.Work(ctx, job(id, 1)); err != nil {
		t.Fatalf("Work on failed row = %v, want nil (idempotent no-op)", err)
	}
	if rec.calls != 0 {
		t.Errorf("deliverer called %d times for a failed row, want 0", rec.calls)
	}
	if d := statusOf(t, sub, id); d.Status != "failed" {
		t.Errorf("status = %q, want failed (unchanged)", d.Status)
	}
}
