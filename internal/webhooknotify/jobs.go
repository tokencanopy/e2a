package webhooknotify

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
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
	store Store
	enq   jobs.Enqueuer

	mu        sync.RWMutex
	deliverer Deliverer
}

// NewJobs builds the integration with just its store (no client, no
// deliverer yet).
func NewJobs(store Store) *Jobs { return &Jobs{store: store} }

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

// Deliver makes Jobs itself the worker's Deliverer, delegating to the
// concrete one set via SetDeliverer. Until that is wired (the brief
// startup window before the notifier is built) it returns a retryable
// outcome, so a pending job simply retries rather than dropping.
func (j *Jobs) Deliver(ctx context.Context, wh *identity.Webhook, kind string) DeliverOutcome {
	j.mu.RLock()
	d := j.deliverer
	j.mu.RUnlock()
	if d == nil {
		return DeliverOutcome{Err: errors.New("webhook notifier not wired yet — retrying")}
	}
	return d.Deliver(ctx, wh, kind)
}

// RegisterJobs adds the NotifyWorker (with Jobs as the late-binding
// Deliverer). No periodics — the maintenance sweep is the only producer.
// Implements jobs.Registrar.
func (j *Jobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, NewNotifyWorker(j.store, j))
	return nil
}

// EnqueueWebhookNotifyTx inserts one webhook_notify job in the caller's
// transaction — the maintenance sweep's, so the state transition and its
// notification job commit atomically (the design's SC2 argument).
func (j *Jobs) EnqueueWebhookNotifyTx(ctx context.Context, tx pgx.Tx, webhookID, kind string) (int64, error) {
	res, err := j.enq.InsertTx(ctx, tx, WebhookNotifyArgs{WebhookID: webhookID, NotifyKind: kind}, &river.InsertOpts{
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
