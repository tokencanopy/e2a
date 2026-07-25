package inboundprocess

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
)

// Jobs is the inbound-processing integration on the shared River client: a
// jobs.Registrar (contributes InboundProcessWorker) plus the transactional enqueue
// entry point the SMTP accept-tx calls. The shared client + the Processor are both
// injected AFTER construction (two-phase wiring) — the client via SetEnqueuer, the
// Processor (the relay Server) via SetProcessor, because the relay is built after
// jobs.New. Jobs itself is the worker's Processor, late-binding to the concrete one.
type Jobs struct {
	store   Store
	enq     jobs.Enqueuer
	metrics Metrics // nil ⇒ the InboundProcessWorker emits nothing (nil-safe)

	mu        sync.RWMutex
	processor Processor
}

// NewJobs builds the integration with just its store (no client, no processor yet).
func NewJobs(store Store) *Jobs {
	return &Jobs{store: store}
}

// SetEnqueuer injects the shared client so EnqueueInboundProcessTx can insert jobs.
func (j *Jobs) SetEnqueuer(e jobs.Enqueuer) { j.enq = e }

// WithMetrics wires the observability backend the InboundProcessWorker emits
// the inbound-process SLI on. Nil-safe; call before RegisterJobs.
func (j *Jobs) WithMetrics(m Metrics) *Jobs {
	j.metrics = m
	return j
}

// SetProcessor injects the concrete Processor (the relay Server), built after
// jobs.New. Guarded so the River worker goroutines read it race-free.
func (j *Jobs) SetProcessor(p Processor) {
	j.mu.Lock()
	j.processor = p
	j.mu.Unlock()
}

// ProcessIntake makes Jobs itself the worker's Processor, delegating to the concrete
// one set via SetProcessor. Until that is wired (the brief startup window before the
// relay is built) it returns a retryable error, so a pending job simply retries
// rather than panicking on a nil processor.
func (j *Jobs) ProcessIntake(ctx context.Context, it *identity.InboundIntake) error {
	j.mu.RLock()
	p := j.processor
	j.mu.RUnlock()
	if p == nil {
		return errors.New("inbound processor not wired yet — retrying")
	}
	return p.ProcessIntake(ctx, it)
}

// RegisterJobs adds the InboundProcessWorker (with Jobs as the late-binding
// Processor) + the RetentionWorker, and schedules the retention sweep as a periodic
// on QueueMaintenance. Implements jobs.Registrar.
func (j *Jobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, NewInboundProcessWorker(j.store, j).WithMetrics(j.metrics))
	river.AddWorker(w, &RetentionWorker{pruner: j.store})
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(retentionInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return InboundRetentionArgs{}, &river.InsertOpts{Queue: jobs.QueueMaintenance}
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
	}
}

// ReconcilePending enqueues an inbound_process job for every accepted intake row that
// has no job yet (process_job_id IS NULL). Run ONCE at startup as the cutover.
//
// Because the accept-tx is a single transaction (intake insert + job enqueue + job-id
// stamp commit together), a committed accepted row in steady state ALWAYS has its
// job — so this set is normally empty. It exists to enqueue (a) any accepted rows at
// the moment the mode is first flipped on, and (b) rows stranded by a crash between
// insert and enqueue. Idempotent: the per-row FOR UPDATE + process_job_id IS NULL
// guard means a re-run (or a concurrent replica) never double-enqueues.
//
// NOTE (follow-up, matching outboundsend): no live periodic reconciler yet. The one
// residual it would close is an intake left accepted-with-a-terminal/dead-job if the
// worker's MarkInboundIntakeFailed somehow never lands — rare, and the startup pass
// re-drives NULL-job rows on the next deploy.
func (j *Jobs) ReconcilePending(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	// The stamp is an inline UPDATE inside jobs.ReconcilePending — identical SQL to
	// store.StampInboundIntakeJobIDTx (which the accept-tx still uses).
	res, err := jobs.ReconcilePending(ctx, pool, jobs.ReconcileSpec{
		Table:     "inbound_intake",
		JobColumn: "process_job_id",
		Where:     "status='accepted'",
		LogPrefix: "[inbound-reconcile]",
	}, j.EnqueueInboundProcessTx)
	return res.Total(), err
}

// EnqueueInboundProcessTx inserts the inbound_process job in the caller's accept-tx
// (the same tx as the inbound_intake insert), returning the River job id to stamp on
// the intake row so a committed accepted row always has its job.
func (j *Jobs) EnqueueInboundProcessTx(ctx context.Context, tx pgx.Tx, intakeID string) (int64, error) {
	res, err := j.enq.InsertTx(ctx, tx, InboundProcessArgs{IntakeID: intakeID}, &river.InsertOpts{
		Queue:       jobs.QueueInbound,
		MaxAttempts: MaxInboundAttempts,
	})
	if err != nil {
		return 0, err
	}
	return res.Job.ID, nil
}
