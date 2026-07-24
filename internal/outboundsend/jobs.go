package outboundsend

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
)

// Jobs is the outbound-send integration on the shared River client: a
// jobs.Registrar (contributes SendWorker + the terminal reconciler) plus the
// transactional enqueue entry point the accept-tx calls. The shared client is
// injected via SetEnqueuer after jobs.New builds it (two-phase wiring, same as
// webhookdelivery / senderidentity).
type Jobs struct {
	store     Store
	deliverer Deliverer
	ramp      RampGate
	pool      *pgxpool.Pool
	enq       jobs.Enqueuer
	metrics   Metrics
}

// NewJobs builds the integration with its dependencies (no client yet). pool
// backs the periodic terminal-state reconciler's scan.
func NewJobs(store Store, deliverer Deliverer, pool *pgxpool.Pool, ramp ...RampGate) *Jobs {
	j := &Jobs{store: store, deliverer: deliverer, pool: pool, metrics: noopMetrics{}}
	if len(ramp) > 0 {
		j.ramp = ramp[0]
	}
	return j
}

// SetEnqueuer injects the shared client so EnqueueSendTx can insert jobs.
func (j *Jobs) SetEnqueuer(e jobs.Enqueuer) { j.enq = e }

// WithMetrics injects the outbound SLI recorder, threaded to both workers at
// RegisterJobs. Chainable so the cmd wiring stays one expression; nil keeps
// the no-op default.
func (j *Jobs) WithMetrics(m Metrics) *Jobs {
	if m != nil {
		j.metrics = m
	}
	return j
}

// RegisterJobs adds the SendWorker and terminal-state safety net to the shared
// client's bundle. Implements jobs.Registrar.
func (j *Jobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, NewSendWorker(j.store, j.deliverer, j.ramp).WithMetrics(j.metrics))
	river.AddWorker(w, NewTerminalReconcileWorker(j.pool, j.store, j.ramp).WithMetrics(j.metrics))
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(terminalReconcileInterval),
			terminalReconcilePeriodicConstructor,
			&river.PeriodicJobOpts{RunOnStart: false},
		),
	}
}

// ReconcilePending enqueues an outbound_send job for every accepted message that
// has no send job yet (send_job_id IS NULL). Run ONCE at startup as the cutover.
//
// Because the accept-tx is a single transaction (message insert + job enqueue +
// send_job_id stamp all commit together), a committed `accepted` row in steady
// state ALWAYS has send_job_id set — so the send_job_id IS NULL set is normally
// empty. This exists to enqueue (a) any pre-async `accepted` rows at the moment the
// mode is first flipped on, and (b) rows from a future accept-tx variant that
// doesn't stamp atomically. Idempotent: the per-row FOR UPDATE + send_job_id IS NULL
// guard means a re-run (or concurrent replica) never
// double-enqueues. Mirrors webhookdelivery.ReconcilePending. Returns the count
// enqueued.
func (j *Jobs) ReconcilePending(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	res, err := jobs.ReconcilePending(ctx, pool, jobs.ReconcileSpec{
		Table:     "messages",
		JobColumn: "send_job_id",
		Where:     "direction='outbound' AND delivery_status='accepted'",
		LogPrefix: "[outbound-reconcile]",
	}, j.EnqueueSendTx)
	return res.Total(), err
}

// EnqueueSendTx enqueues a send job WITHIN the caller's transaction — the outbox
// pattern: the accept-tx's messages-row insert and this job commit together, so an
// `accepted` message can never exist without a send job (or vice versa). The
// accept-tx stamps the returned river_job id on messages.send_job_id so
// the reconciler can find stranded rows (`accepted` with no job). Mirrors
// webhookdelivery.EnqueueDeliveryTx.
func (j *Jobs) EnqueueSendTx(ctx context.Context, tx pgx.Tx, messageID string) (int64, error) {
	res, err := j.enq.InsertTx(ctx, tx, OutboundSendArgs{MessageID: messageID}, &river.InsertOpts{
		Queue:       jobs.QueueOutbound,
		MaxAttempts: MaxSendAttempts,
	})
	if err != nil {
		return 0, err
	}
	return res.Job.ID, nil
}
