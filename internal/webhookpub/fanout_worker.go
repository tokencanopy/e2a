package webhookpub

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/telemetry"
)

// FanOutArgs drives one webhook fan-out: match the event user's enabled webhooks,
// insert the matching webhook_subscriber_deliveries rows, and enqueue their delivery
// jobs. Carries only the event id — the worker re-reads the durable webhook_events row
// (the three-layer pattern: the job is the trigger, the row is the source of truth),
// so args stay tiny and a schema change to the event never invalidates enqueued jobs.
//
// This is the River replacement for the legacy webhookpub.OutboxWorker drain
// (LISTEN/NOTIFY + poll + SKIP-LOCKED lease). See docs/design/webhook-fanout-river-migration.md.
type FanOutArgs struct {
	EventID string `json:"event_id"`
}

func (FanOutArgs) Kind() string { return "webhook_fanout" }

const (
	// maxFanOutAttempts bounds River's retries of a fan-out job. Fan-out failures are
	// transient (DB blip, identity read) — a handful of retries rides them out. A
	// persistent failure (e.g. a matching bug) surfaces as a discarded job; the
	// reconciler's dead-job rescue then re-drives the still-pending event with a
	// fresh job on the reconcile cadence (the outbox's at-least-once retry-forever
	// contract), so a discarded job is a loud signal, never a permanent strand.
	maxFanOutAttempts = 10

	// fanOutReconcileInterval is how often the reconciler re-drives pending events with
	// no fan-out job — frequent + cheap (a partial index backs the status='pending' AND
	// fanout_job_id IS NULL scan). The per-pass row cap is jobs.DefaultReconcileBatch.
	fanOutReconcileInterval = 1 * time.Minute
)

// FanOutJobs is the webhook fan-out integration on the shared River client: a
// jobs.Registrar (contributes FanOutWorker + the reconcile periodic) plus the
// transactional enqueue entry point (EnqueueFanOutTx) that PublishTx /
// PublishBestEffortTx call in the event's own tx. The shared client is injected via
// SetEnqueuer after jobs.New builds it (two-phase wiring, same as webhookdelivery).
type FanOutJobs struct {
	pool          *pgxpool.Pool
	identityStore identityReader
	deliveryEnq   DeliveryEnqueuer
	metrics       telemetry.Metrics
	enq           jobs.Enqueuer
}

// NewFanOutJobs builds the integration with its dependencies (no client yet). pool
// backs the reconciler's scan; deliveryEnq is the SAME delivery enqueuer the legacy
// OutboxWorker uses — fan-out enqueues Layer-2→3 delivery jobs exactly as before.
func NewFanOutJobs(pool *pgxpool.Pool, identityStore identityReader, deliveryEnq DeliveryEnqueuer, metrics telemetry.Metrics) *FanOutJobs {
	if metrics == nil {
		metrics = telemetry.NoOp{}
	}
	return &FanOutJobs{pool: pool, identityStore: identityStore, deliveryEnq: deliveryEnq, metrics: metrics}
}

// SetEnqueuer injects the shared client so EnqueueFanOutTx can insert fan-out jobs.
func (j *FanOutJobs) SetEnqueuer(e jobs.Enqueuer) { j.enq = e }

// RegisterJobs adds the FanOutWorker + the reconcile worker and returns the reconcile
// periodic. Implements jobs.Registrar.
func (j *FanOutJobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, &FanOutWorker{pool: j.pool, identityStore: j.identityStore, deliveryEnq: j.deliveryEnq, metrics: j.metrics})
	river.AddWorker(w, &FanOutReconcileWorker{jobs: j})
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(fanOutReconcileInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return FanOutReconcileArgs{}, &river.InsertOpts{Queue: jobs.QueueMaintenance}
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
	}
}

// EnqueueFanOutTx enqueues a fan-out job WITHIN the caller's transaction — the outbox
// pattern between Layer 1 (webhook_events) and its fan-out: the event-row write and
// this job commit together, so a pending event can never exist without a job (modulo
// the best-effort publish path, which the reconciler backstops). Returns the river_job
// id for the caller to stamp on webhook_events.fanout_job_id. Routed to QueueWebhook.
func (j *FanOutJobs) EnqueueFanOutTx(ctx context.Context, tx pgx.Tx, eventID string) (int64, error) {
	res, err := j.enq.InsertTx(ctx, tx, FanOutArgs{EventID: eventID}, &river.InsertOpts{
		Queue:       jobs.QueueWebhook,
		MaxAttempts: maxFanOutAttempts,
	})
	if err != nil {
		return 0, err
	}
	return res.Job.ID, nil
}

// FanOutWorker fans out one webhook_events row on River, replacing the legacy
// OutboxWorker drain. It re-reads the event by id, skips it if it is gone (30d GC) or
// no longer 'pending' (a duplicate at-least-once job — already fanned out), and
// otherwise runs the shared fanOutEventCore. Idempotent: the (event_id, webhook_id)
// unique index dedups delivery-row inserts and the status='pending' guard on the
// terminal UPDATE makes a re-run a no-op.
type FanOutWorker struct {
	river.WorkerDefaults[FanOutArgs]
	pool          *pgxpool.Pool
	identityStore identityReader
	deliveryEnq   DeliveryEnqueuer
	metrics       telemetry.Metrics
}

// NewFanOutWorker builds a FanOutWorker. RegisterJobs builds an identical one for the
// client; this is exported so tests can drive Work directly without a River client.
func NewFanOutWorker(pool *pgxpool.Pool, identityStore identityReader, deliveryEnq DeliveryEnqueuer, metrics telemetry.Metrics) *FanOutWorker {
	if metrics == nil {
		metrics = telemetry.NoOp{}
	}
	return &FanOutWorker{pool: pool, identityStore: identityStore, deliveryEnq: deliveryEnq, metrics: metrics}
}

func (w *FanOutWorker) Work(ctx context.Context, job *river.Job[FanOutArgs]) error {
	ev, err := loadEventForFanOut(ctx, w.pool, job.Args.EventID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // event GC'd (30d retention) before fan-out — nothing to do
		}
		return err
	}
	if ev.status != "pending" {
		return nil // already fanned out (processed/no_match) — idempotent re-run
	}
	return fanOutEventCore(ctx, w.pool, w.identityStore, w.deliveryEnq, w.metrics, ev.leasedEvent)
}

// Timeout bounds a fan-out job. It is a handful of queries + N inserts — never the long
// synchronous drain the janitor's 60s-cut hazard came from — but a bounded timeout
// keeps a pathologically slow DB from pinning a QueueWebhook worker indefinitely.
func (w *FanOutWorker) Timeout(*river.Job[FanOutArgs]) time.Duration { return 2 * time.Minute }

// loadedEvent is a leasedEvent plus its current status, for the fan-out re-read.
type loadedEvent struct {
	leasedEvent
	status string
}

// loadEventForFanOut re-reads the columns fanOutEventCore needs from webhook_events by
// id, plus status for the idempotency guard. Returns pgx.ErrNoRows if the row is gone.
func loadEventForFanOut(ctx context.Context, pool *pgxpool.Pool, eventID string) (loadedEvent, error) {
	var ev loadedEvent
	err := pool.QueryRow(ctx,
		`SELECT id, user_id, type, envelope, agent_id, conversation_id, message_id, status
		   FROM webhook_events WHERE id = $1`,
		eventID,
	).Scan(&ev.id, &ev.userID, &ev.eventType, &ev.envelope,
		&ev.agentID, &ev.conversationID, &ev.messageID, &ev.status)
	if err != nil {
		return loadedEvent{}, err
	}
	return ev, nil
}

// FanOutReconcileArgs drives the periodic stranded-event reconciler.
type FanOutReconcileArgs struct{}

func (FanOutReconcileArgs) Kind() string { return "webhook_fanout_reconcile" }

// FanOutReconcileWorker re-enqueues any pending event with no live fan-out job
// (never enqueued, or its job is terminal/pruned). It is the LIVE backstop for the
// best-effort publish path (PublishBestEffortTx must not fail the caller's tx, so an
// event can commit with its enqueue lost), any crash window between the event commit
// and the job insert, and a fan-out job discarded after exhausting its attempts —
// turning "recovered only on restart" (or never) into "recovered within
// fanOutReconcileInterval". Idempotent (ReconcilePending's FOR UPDATE strandedness
// re-check). Mirrors webhookdelivery.ReconcileWorker.
type FanOutReconcileWorker struct {
	river.WorkerDefaults[FanOutReconcileArgs]
	jobs *FanOutJobs
}

func (w *FanOutReconcileWorker) Work(ctx context.Context, _ *river.Job[FanOutReconcileArgs]) error {
	n, err := w.jobs.ReconcilePending(ctx, w.jobs.pool)
	if err != nil {
		return err // River retries the reconcile job — a transient DB blip is fine
	}
	if n > 0 {
		log.Printf("[webhook-fanout-reconcile] re-enqueued %d stranded events", n)
	}
	return nil
}

// ReconcilePending enqueues a fan-out job for every pending webhook_events row that
// has no live job: fanout_job_id IS NULL, or (RescueDeadJobs) stamped with a job
// River has discarded (maxFanOutAttempts exhausted), cancelled, completed without
// the event terminalizing, or already pruned. Without the dead-job arm, an event
// whose fan-out job burned all its attempts was stranded forever: still 'pending'
// (so never swept by the janitor, by design — the outbox is at-least-once), stamped
// (so invisible to an IS-NULL-only reconciler). Re-driving is safe: the FanOutWorker
// re-reads status under a pending guard and the (event_id, webhook_id) unique index
// dedups delivery inserts. Runs at startup (cutover) AND on the live schedule
// (FanOutReconcileWorker). Idempotent: the per-row FOR UPDATE re-check means a re-run
// (or a concurrent replica) never double-enqueues. Returns the number of events
// enqueued.
func (j *FanOutJobs) ReconcilePending(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	return jobs.ReconcilePending(ctx, pool, jobs.ReconcileSpec{
		Table:          "webhook_events",
		JobColumn:      "fanout_job_id",
		Where:          "status='pending'",
		LogPrefix:      "[webhook-fanout-reconcile]",
		RescueDeadJobs: true,
	}, j.EnqueueFanOutTx)
}
