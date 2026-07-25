package hitlnotify

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

// Jobs is the HITL-notification integration on the shared River client: a
// jobs.Registrar (contributes NotifyWorker) plus the transactional enqueue entry
// point the hold accept-tx calls. Both the shared client and the concrete Deliverer
// are injected AFTER construction (two-phase wiring) — the client via SetEnqueuer,
// the Deliverer (the Notifier, which needs the relay + signer resolved) via
// SetDeliverer. Jobs itself is the worker's Deliverer, late-binding to the concrete
// one, mirroring inboundprocess's late-bound Processor.
type Jobs struct {
	store Store
	enq   jobs.Enqueuer

	mu        sync.RWMutex
	deliverer Deliverer
}

// NewJobs builds the integration with just its store (no client, no deliverer yet).
func NewJobs(store Store) *Jobs { return &Jobs{store: store} }

// SetEnqueuer injects the shared client so EnqueueNotifyTx can insert jobs.
func (j *Jobs) SetEnqueuer(e jobs.Enqueuer) { j.enq = e }

// SetDeliverer injects the concrete Deliverer (the Notifier), built after the
// relay/signer gating resolves. Guarded so the River worker goroutines read it
// race-free.
func (j *Jobs) SetDeliverer(d Deliverer) {
	j.mu.Lock()
	j.deliverer = d
	j.mu.Unlock()
}

// Deliver makes Jobs itself the worker's Deliverer, delegating to the concrete one
// set via SetDeliverer. Until that is wired (the brief startup window before the
// notifier is built) it returns a retryable outcome, so a pending job simply
// retries rather than dropping on a nil deliverer.
func (j *Jobs) Deliver(ctx context.Context, pn *identity.PendingNotify) DeliverOutcome {
	j.mu.RLock()
	d := j.deliverer
	j.mu.RUnlock()
	if d == nil {
		return DeliverOutcome{Err: errors.New("hitl notifier not wired yet — retrying")}
	}
	return d.Deliver(ctx, pn)
}

// RegisterJobs adds the NotifyWorker (with Jobs as the late-binding Deliverer).
// No periodics — the reconciler is a one-shot startup cutover. Implements
// jobs.Registrar.
func (j *Jobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, NewNotifyWorker(j.store, j))
	return nil
}

// EnqueueNotifyTx inserts the hitl_notify job in the caller's hold accept-tx (the
// same tx as the pending_review insert), returning the River job id to stamp on the
// message so a committed pending_review row always has its notification job.
func (j *Jobs) EnqueueNotifyTx(ctx context.Context, tx pgx.Tx, messageID string) (int64, error) {
	res, err := j.enq.InsertTx(ctx, tx, HITLNotifyArgs{MessageID: messageID}, &river.InsertOpts{
		Queue:       jobs.QueueNotify,
		MaxAttempts: MaxNotifyAttempts,
	})
	if err != nil {
		return 0, err
	}
	return res.Job.ID, nil
}

// ReconcilePending enqueues a hitl_notify job for every pending_review message that
// has no job yet AND was never notified (notify_job_id IS NULL AND notified_at IS
// NULL). Run ONCE at startup as the cutover.
//
// Because the accept-tx is a single transaction (message insert + job enqueue +
// job-id stamp commit together), a committed pending_review row in steady state
// ALWAYS has its job — so this set is normally empty. It exists to enqueue holds
// created on the no-notifier plain path (notified_at NULL) if a relay is later
// configured, plus any row stranded by a crash between insert and enqueue.
//
// The `notified_at IS NULL` guard is what makes the feature's very first deploy
// safe: every hold already pending_review at cutover was notified by the old code
// path, and migration 057 stamps notified_at on exactly those rows, so this scan
// skips them — no owner is emailed twice. Idempotent: the per-row FOR UPDATE +
// notify_job_id IS NULL re-check means a re-run (or a concurrent replica) never
// double-enqueues.
func (j *Jobs) ReconcilePending(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	// The stamp is an inline UPDATE inside jobs.ReconcilePending — identical SQL to
	// store.StampNotifyJobIDTx (which the accept-tx still uses). The notified_at IS NULL
	// guard keeps already-emailed holds out of the reconcile set.
	res, err := jobs.ReconcilePending(ctx, pool, jobs.ReconcileSpec{
		Table:     "messages",
		JobColumn: "notify_job_id",
		Where:     "status='pending_review' AND notified_at IS NULL",
		LogPrefix: "[hitl-notify] reconcile",
	}, j.EnqueueNotifyTx)
	return res.Total(), err
}
