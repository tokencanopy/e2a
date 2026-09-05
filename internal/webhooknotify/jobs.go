package webhooknotify

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// Jobs is the webhook health-notification integration on the shared River
// client: a jobs.Registrar (contributes NotifyWorker) plus the
// transactional enqueue entry points the maintenance sweep calls. Both the
// shared client and the concrete Deliverer are injected AFTER construction
// (two-phase wiring, mirroring hitlnotify.Jobs) — the client via
// SetEnqueuer, the Deliverer (the Notifier, which needs the relay
// resolved) via SetDeliverer. Jobs itself is the worker's Deliverer,
// late-binding to the concrete one, so a job enqueued during the startup
// window retries instead of hitting a nil deliverer.
//
// Jobs also implements webhook.HealthNotifyEnqueuer (EnqueueDisabledTx /
// EnqueueWarningTx), which is how the AutoDisableWorker sweep reaches it
// without importing this package.
type Jobs struct {
	store   Store
	enq     jobs.Enqueuer
	metrics Metrics
	gate    sendingpolicy.Gate
	pool    *pgxpool.Pool

	mu        sync.RWMutex
	deliverer Deliverer
}

// NewJobs builds the integration with just its store (no client, no
// deliverer yet).
func NewJobs(store Store) *Jobs { return &Jobs{store: store} }

// WithGate injects the sending-protection gate and the pool its legacy
// resolver and arg stamp use. Chainable; nil keeps the gateless default.
func (j *Jobs) WithGate(g sendingpolicy.Gate, pool *pgxpool.Pool) *Jobs {
	if g != nil {
		j.gate = g
	}
	if pool != nil {
		j.pool = pool
	}
	return j
}

// SetEnqueuer injects the shared client so the EnqueueTx methods can
// insert jobs.
func (j *Jobs) SetEnqueuer(e jobs.Enqueuer) { j.enq = e }

// SetDeliverer injects the concrete Deliverer (the Notifier), built after
// the relay gating resolves. Guarded so the River worker goroutines read
// it race-free.
func (j *Jobs) SetDeliverer(d Deliverer) {
	j.mu.Lock()
	j.deliverer = d
	j.mu.Unlock()
}

// Compose makes Jobs itself the worker's Deliverer, delegating to the
// concrete one set via SetDeliverer. Until that is wired (the brief startup
// window before the notifier is built) it returns a retryable outcome — and
// because Compose runs before any attempt is charged, that window costs
// nothing.
func (j *Jobs) Compose(ctx context.Context, wh *identity.Webhook, kind string) (outbound.Envelope, DeliverOutcome) {
	d := j.currentDeliverer()
	if d == nil {
		return outbound.Envelope{}, DeliverOutcome{Err: errors.New("webhook notifier not wired yet — retrying")}
	}
	return d.Compose(ctx, wh, kind)
}

// Submit delegates the authorized submission to the concrete Deliverer.
func (j *Jobs) Submit(ctx context.Context, env outbound.Envelope, auth sendingpolicy.ProviderAuthorization) DeliverOutcome {
	d := j.currentDeliverer()
	if d == nil {
		return DeliverOutcome{Err: errors.New("webhook notifier not wired yet — retrying")}
	}
	return d.Submit(ctx, env, auth)
}

func (j *Jobs) currentDeliverer() Deliverer {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.deliverer
}

// Gate exposes the wired sending-protection gate (nil when gateless), so the
// composition root's wiring test can prove the production bundle is armed.
func (j *Jobs) Gate() sendingpolicy.Gate { return j.gate }

// WithMetrics wires the observability backend the NotifyWorker emits the
// notification-outcome counter on. Nil-safe; call before RegisterJobs.
func (j *Jobs) WithMetrics(m Metrics) *Jobs {
	j.metrics = m
	return j
}

// RegisterJobs adds the NotifyWorker (with Jobs as the late-binding
// Deliverer). No periodics — the maintenance sweep is the only producer.
// Implements jobs.Registrar.
func (j *Jobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, j.NotifyWorker())
	return nil
}

// NotifyWorker builds the fully armed worker RegisterJobs registers.
func (j *Jobs) NotifyWorker() *NotifyWorker {
	w := NewNotifyWorker(j.store, j).WithMetrics(j.metrics).WithGate(j.gate).WithOperationResolver(j.ResolveLegacyOperation)
	if j.pool != nil {
		w = w.WithArgStamper(func(ctx context.Context, jobID int64, ref sendingpolicy.OperationRef) error {
			return jobs.StampJobArg(ctx, j.pool, jobID, "operation_ref", ref)
		})
	}
	return w
}

// ResolveLegacyOperation prepares the notification operation for a job that
// carries no reference, in its own committed transaction, through the same
// PrepareNotificationTx the sweep's enqueue runs.
func (j *Jobs) ResolveLegacyOperation(ctx context.Context, webhookID, kind string) (sendingpolicy.OperationRef, error) {
	if j.gate == nil || j.pool == nil {
		return sendingpolicy.OperationRef{}, fmt.Errorf("webhook notify: legacy operation resolver is not wired")
	}
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return sendingpolicy.OperationRef{}, fmt.Errorf("begin legacy resolve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ref, err := j.gate.PrepareNotificationTx(ctx, tx, sendingpolicy.NewWebhookHealthNotificationRef(webhookID, kind))
	if err != nil {
		return sendingpolicy.OperationRef{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return sendingpolicy.OperationRef{}, fmt.Errorf("commit legacy resolve: %w", err)
	}
	return ref, nil
}

// EnqueueWebhookNotifyTx inserts one webhook_notify job in the caller's
// transaction — the maintenance sweep's, so the state transition and its
// notification job commit atomically (the design's SC2 argument).
//
// With a gate wired the notification's operation is prepared here, against
// the locked webhook row, so the owning account is charged and the worker
// never derives attribution.
func (j *Jobs) EnqueueWebhookNotifyTx(ctx context.Context, tx pgx.Tx, webhookID, kind string) (int64, error) {
	args := WebhookNotifyArgs{WebhookID: webhookID, NotifyKind: kind}
	if j.gate != nil {
		ref, err := j.gate.PrepareNotificationTx(ctx, tx, sendingpolicy.NewWebhookHealthNotificationRef(webhookID, kind))
		if err != nil {
			return 0, fmt.Errorf("prepare notification operation: %w", err)
		}
		args.OperationRef = &ref
	}
	res, err := j.enq.InsertTx(ctx, tx, args, &river.InsertOpts{
		Queue:       jobs.QueueNotify,
		MaxAttempts: MaxNotifyAttempts,
	})
	if err != nil {
		return 0, err
	}
	return res.Job.ID, nil
}

// EnqueueDisabledTx / EnqueueWarningTx implement webhook.HealthNotifyEnqueuer.

func (j *Jobs) EnqueueDisabledTx(ctx context.Context, tx pgx.Tx, webhookID string) error {
	_, err := j.EnqueueWebhookNotifyTx(ctx, tx, webhookID, KindDisabled)
	return err
}

func (j *Jobs) EnqueueWarningTx(ctx context.Context, tx pgx.Tx, webhookID string) error {
	_, err := j.EnqueueWebhookNotifyTx(ctx, tx, webhookID, KindWarning)
	return err
}
