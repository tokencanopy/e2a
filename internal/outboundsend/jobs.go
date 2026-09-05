package outboundsend

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// Jobs is the outbound-send integration on the shared River client: a
// jobs.Registrar (contributes SendWorker + the terminal reconciler) plus the
// transactional enqueue entry point the accept-tx calls. The shared client is
// injected via SetEnqueuer after jobs.New builds it (two-phase wiring, same as
// webhookdelivery / senderidentity).
type Jobs struct {
	store     Store
	deliverer Deliverer
	gate      sendingpolicy.Gate
	rate      RateGate
	pool      *pgxpool.Pool
	enq       jobs.Enqueuer
	metrics   Metrics
}

// NewJobs builds the integration with its dependencies (no client yet). pool
// backs the periodic terminal-state reconciler's scan and the legacy-argument
// resolver's transaction.
func NewJobs(store Store, deliverer Deliverer, pool *pgxpool.Pool) *Jobs {
	return &Jobs{store: store, deliverer: deliverer, pool: pool, metrics: noopMetrics{}}
}

// WithGate injects the sending-protection gate. Every enqueue then prepares a
// durable operation in the accept transaction, and every worker execution
// authorizes through it. Chainable; nil keeps the gateless default (unit
// tests only — see NewSendWorker).
func (j *Jobs) WithGate(g sendingpolicy.Gate) *Jobs {
	if g != nil {
		j.gate = g
	}
	return j
}

// SendWorker builds the fully armed send worker RegisterJobs registers: the
// gate, the legacy resolver, the rate gate, and metrics. It is the one place
// those are wired, and the composition root's test inspects its result.
func (j *Jobs) SendWorker() *SendWorker {
	return NewSendWorker(j.store, j.deliverer).WithMetrics(j.metrics).WithRateGate(j.rate).WithGate(j.gate).WithOperationResolver(j.ResolveLegacyOperation)
}

// TerminalReconcileWorker builds the reconciler RegisterJobs registers.
func (j *Jobs) TerminalReconcileWorker() *TerminalReconcileWorker {
	return NewTerminalReconcileWorker(j.pool, j.store).WithMetrics(j.metrics).WithGate(j.gate)
}

// Gate exposes the wired sending-protection gate, for the composition root's
// wiring test. nil when none is wired.
func (j *Jobs) Gate() sendingpolicy.Gate { return j.gate }

// Deliverer exposes the wired provider deliverer, for the same test.
func (j *Jobs) Deliverer() Deliverer { return j.deliverer }

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

// WithRateGate injects the fire-time per-agent rate gate (internal/sendrate),
// threaded to the SendWorker at RegisterJobs. Chainable; nil keeps the
// allow-all default.
func (j *Jobs) WithRateGate(g RateGate) *Jobs {
	if g != nil {
		j.rate = g
	}
	return j
}

// RegisterJobs adds the SendWorker and terminal-state safety net to the shared
// client's bundle. Implements jobs.Registrar.
func (j *Jobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, j.SendWorker())
	river.AddWorker(w, j.TerminalReconcileWorker())
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
	return j.enqueueSendTx(ctx, tx, messageID, time.Time{})
}

// EnqueueScheduledSendTx is EnqueueSendTx for a scheduled send: it enqueues the
// same outbound_send job in the caller's transaction, but with
// river.InsertOpts.ScheduledAt=at so River holds the job in state `scheduled`
// and does not promote it to a worker until `at`. Everything downstream (claim,
// suppression re-check, retry envelope, terminal reconciler) is byte-identical
// to an immediate send — scheduling changes only WHEN the job first runs. A zero
// `at` behaves exactly like EnqueueSendTx (immediate).
func (j *Jobs) EnqueueScheduledSendTx(ctx context.Context, tx pgx.Tx, messageID string, at time.Time) (int64, error) {
	return j.enqueueSendTx(ctx, tx, messageID, at)
}

// enqueueSendTx is the shared outbox insert behind the immediate and scheduled
// entry points. A non-zero `at` sets InsertOpts.ScheduledAt; a zero value omits
// it (River defaults ScheduledAt to now, i.e. immediately available).
//
// With a gate wired, the durable provider operation is prepared HERE, after
// the message insert and before the River insert, in the caller's transaction:
// a paused account is refused at the door (ErrSendingPaused) rather than
// queueing mail that can never leave, and the job carries the operation
// reference so the worker never derives purpose or attribution on its own.
func (j *Jobs) enqueueSendTx(ctx context.Context, tx pgx.Tx, messageID string, at time.Time) (int64, error) {
	args := OutboundSendArgs{MessageID: messageID}
	if j.gate != nil {
		decision, ref, err := j.gate.PrepareExternalTx(ctx, tx, messageID)
		if err != nil {
			return 0, fmt.Errorf("prepare sending operation: %w", err)
		}
		if decision == sendingpolicy.AcceptanceSendingPaused {
			return 0, ErrSendingPaused
		}
		if ref.IsZero() {
			// The only accepted shape without an operation is an exact
			// self-send, and those never enqueue. Refusing here keeps a
			// prepared-but-operationless job from masquerading as a legacy
			// one that the worker would then kill.
			return 0, fmt.Errorf("prepare sending operation: message %s has no provider operation", messageID)
		}
		args.OperationRef = &ref
	}
	opts := &river.InsertOpts{
		Queue:       jobs.QueueOutbound,
		MaxAttempts: MaxSendAttempts,
	}
	if !at.IsZero() {
		opts.ScheduledAt = at
	}
	res, err := j.enq.InsertTx(ctx, tx, args, opts)
	if err != nil {
		return 0, err
	}
	return res.Job.ID, nil
}

// ResolveLegacyOperation is the compatibility resolver for a job enqueued by
// a pre-floor slot with no operation reference. It runs the same
// PrepareExternalTx an accept transaction runs — idempotent on the durable
// operation row — in its own committed transaction, so an old job and a new
// one authorize identically. There is deliberately no other way to obtain an
// operation from a bare message id.
func (j *Jobs) ResolveLegacyOperation(ctx context.Context, messageID string) (sendingpolicy.AcceptanceDecision, sendingpolicy.OperationRef, error) {
	if j.gate == nil || j.pool == nil {
		return "", sendingpolicy.OperationRef{}, fmt.Errorf("legacy operation resolver is not wired")
	}
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return "", sendingpolicy.OperationRef{}, fmt.Errorf("begin legacy resolve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	decision, ref, err := j.gate.PrepareExternalTx(ctx, tx, messageID)
	if err != nil {
		return "", sendingpolicy.OperationRef{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", sendingpolicy.OperationRef{}, fmt.Errorf("commit legacy resolve: %w", err)
	}
	return decision, ref, nil
}
