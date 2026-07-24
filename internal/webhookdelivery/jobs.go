package webhookdelivery

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/webhook"
)

// reconcileInterval is how often the live reconciler re-enqueues any pending
// delivery row that has no River job. Frequent + cheap (a partial index backs the
// job_id IS NULL scan), so a delivery whose in-tx enqueue never happened — the
// separate-tx /test + redelivery paths, or an outbox-drain crash window — is
// re-driven within this bound rather than waiting for a process restart.
const reconcileInterval = 1 * time.Minute

// Jobs is the webhook-delivery integration on the shared River client: a
// jobs.Registrar (contributes DeliverWorker + the reconcile periodic) plus the
// transactional enqueue entry point the outbox drain + redelivery API call. The
// shared client is injected via SetEnqueuer after jobs.New builds it (two-phase
// wiring, same as senderidentity).
type Jobs struct {
	subStore  *webhook.SubscriberStore
	deliverer Deliverer
	webhooks  WebhookReader
	pool      *pgxpool.Pool
	enq       jobs.Enqueuer
	metrics   Metrics // nil ⇒ the DeliverWorker emits nothing (nil-safe)
}

// NewJobs builds the integration with its dependencies (no client yet). pool backs
// the periodic reconciler's scan.
func NewJobs(subStore *webhook.SubscriberStore, deliverer Deliverer, webhooks WebhookReader, pool *pgxpool.Pool) *Jobs {
	return &Jobs{subStore: subStore, deliverer: deliverer, webhooks: webhooks, pool: pool}
}

// SetEnqueuer injects the shared client so EnqueueDeliveryTx can insert jobs.
func (j *Jobs) SetEnqueuer(e jobs.Enqueuer) { j.enq = e }

// WithMetrics wires the observability backend the DeliverWorker emits the
// webhook-attempt SLI on. Nil-safe; call before RegisterJobs.
func (j *Jobs) WithMetrics(m Metrics) *Jobs {
	j.metrics = m
	return j
}

// RegisterJobs adds the DeliverWorker + the reconcile worker, and returns the
// reconcile periodic. Implements jobs.Registrar.
func (j *Jobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, NewDeliverWorker(j.subStore, j.deliverer, j.webhooks).WithMetrics(j.metrics))
	river.AddWorker(w, &ReconcileWorker{jobs: j})
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(reconcileInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return WebhookReconcileArgs{}, &river.InsertOpts{Queue: jobs.QueueMaintenance}
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
	}
}

// WebhookReconcileArgs drives the periodic stranded-delivery reconciler.
type WebhookReconcileArgs struct{}

func (WebhookReconcileArgs) Kind() string { return "webhook_reconcile" }

// ReconcileWorker re-enqueues any pending delivery row with no live River job
// (never enqueued, or its job is terminal/pruned). It is the LIVE backstop for
// the separate-tx enqueue paths (/test, redelivery), any outbox-drain crash
// window, and a lost final-attempt terminal write — turning "recovered only on
// restart" (or never) into "recovered within reconcileInterval". Idempotent
// (ReconcilePending's FOR UPDATE strandedness re-check).
type ReconcileWorker struct {
	river.WorkerDefaults[WebhookReconcileArgs]
	jobs *Jobs
}

func (w *ReconcileWorker) Work(ctx context.Context, _ *river.Job[WebhookReconcileArgs]) error {
	n, err := w.jobs.ReconcilePending(ctx, w.jobs.pool)
	if err != nil {
		return err // River retries the reconcile job — transient DB blip is fine
	}
	if n > 0 {
		log.Printf("[webhook-reconcile] re-enqueued %d stranded deliveries", n)
	}
	return nil
}

// ReconcilePending enqueues a River delivery job for every pending Layer 2 row
// that has no live job: job_id IS NULL, or (RescueDeadJobs) stamped with a
// river_job that is terminal or already pruned. The dead-job arm closes the CW-2
// strand: when the final delivery attempt's terminal 'failed' write is lost (a
// sustained DB outage in exactly that window — the CRITICAL log in
// DeliverWorker.Work), River discards the job and the row sat 'pending' with a
// dead job_id, invisible to an IS-NULL-only reconciler, until its TTL. Re-driving
// is safe: delivery is at-least-once and the DeliverWorker no-ops on rows that
// already terminalized. It runs BOTH at startup (the one-shot cutover from the
// legacy queue) AND on a live schedule (ReconcileWorker) so a stranded row —
// from the separate-tx /test/redelivery enqueue paths, an outbox-drain crash
// window, or a lost terminal write — is re-driven within reconcileInterval
// rather than only on the next restart. Idempotent: the per-row FOR UPDATE
// re-check means a re-run (or a concurrent replica) never double-enqueues.
// Returns the number of rows enqueued.
func (j *Jobs) ReconcilePending(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	return jobs.ReconcilePending(ctx, pool, jobs.ReconcileSpec{
		Table:          "webhook_subscriber_deliveries",
		JobColumn:      "job_id",
		Where:          "status='pending'",
		LogPrefix:      "[webhook-reconcile]",
		RescueDeadJobs: true,
	}, j.EnqueueDeliveryTx)
}

// EnqueueDelivery enqueues a River delivery job for an ALREADY-INSERTED pending
// Layer 2 row, in its own transaction, and stamps job_id. This is for the direct-
// insert API surfaces that bypass the outbox drain — the /test webhook endpoint
// and the redelivery API — which create a subscriber_deliveries row targeting a
// single webhook. Without this, those rows have no River job and (post
// SubscriberRetryWorker deletion) would never deliver. Idempotent per row via the
// job_id IS NULL guard under a row lock.
func (j *Jobs) EnqueueDelivery(ctx context.Context, pool *pgxpool.Pool, deliveryID string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var jobID *int64
		if err := tx.QueryRow(ctx,
			`SELECT job_id FROM webhook_subscriber_deliveries WHERE id=$1 FOR UPDATE`, deliveryID,
		).Scan(&jobID); err != nil {
			return err
		}
		if jobID != nil {
			return nil // already enqueued
		}
		newJobID, err := j.EnqueueDeliveryTx(ctx, tx, deliveryID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE webhook_subscriber_deliveries SET job_id=$2 WHERE id=$1`, deliveryID, newJobID)
		return err
	})
}

// EnqueueDeliveryTx enqueues a delivery job WITHIN the caller's transaction — the
// outbox pattern: the Layer 2 row insert and this job commit together, so a
// delivery record can never exist without a job (or vice versa). The caller only
// calls this when the Layer 2 insert actually inserted a row (dedup ON CONFLICT
// returned an id), so a deduped event enqueues nothing. Returns the river_job id
// for the caller to stamp on the Layer 2 row's job_id.
func (j *Jobs) EnqueueDeliveryTx(ctx context.Context, tx pgx.Tx, deliveryID string) (int64, error) {
	res, err := j.enq.InsertTx(ctx, tx, WebhookDeliverArgs{DeliveryID: deliveryID}, &river.InsertOpts{
		Queue:       jobs.QueueWebhook,
		MaxAttempts: MaxDeliveryAttempts,
	})
	if err != nil {
		return 0, err
	}
	return res.Job.ID, nil
}
