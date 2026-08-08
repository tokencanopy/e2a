package webhook

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
)

// HealthNotifyEnqueuer enqueues webhook health-notification jobs inside the
// sweep's transaction — one per state transition, atomically with it.
// Implemented by *webhooknotify.Jobs; nil (the default) leaves the sweep's
// state transitions running with no notifications, exactly the pre-feature
// behavior (self-hosts without an SMTP relay).
type HealthNotifyEnqueuer interface {
	// EnqueueDisabledTx enqueues the "we disabled your webhook" email job.
	EnqueueDisabledTx(ctx context.Context, tx pgx.Tx, webhookID string) error
	// EnqueueWarningTx enqueues the "your webhook is failing" early-warning
	// email job.
	EnqueueWarningTx(ctx context.Context, tx pgx.Tx, webhookID string) error
}

// AutoDisableWorker scans for chronically-failing webhooks and
// disables them, warns owners whose webhooks have started failing at
// the attempt level (long before the breaker can trip), and clears
// expired signing_secret_prev rows past their 24h grace window.
//
// The passes share a worker because they're all cheap, idempotent, and
// run on the same low cadence. The schedule is owned by River
// (webhookdelivery.MaintenanceJobs, a periodic on QueueMaintenance)
// which drives Tick; this type is the sweep body.
type AutoDisableWorker struct {
	store    *identity.Store
	notifier HealthNotifyEnqueuer
}

// NewAutoDisableWorker constructs the sweep. River drives Tick on a
// periodic schedule; tests can call Tick directly.
func NewAutoDisableWorker(store *identity.Store) *AutoDisableWorker {
	return &AutoDisableWorker{store: store}
}

// SetNotifier injects the health-notification enqueuer (two-phase wiring:
// the concrete *webhooknotify.Jobs is built alongside the registrars,
// before the shared River client exists). nil-safe — unset means the
// sweep transitions state without enqueuing notifications.
func (w *AutoDisableWorker) SetNotifier(n HealthNotifyEnqueuer) { w.notifier = n }

// Tick runs the maintenance passes once. Driven by the River periodic
// (and directly by tests).
//
// Order matters: the disable pass runs BEFORE the warn pass so that a
// burst crossing both thresholds inside one sweep interval produces only
// the disable email — the warn pass's enabled = true predicate excludes
// the rows this tick just disabled. (The notify worker's kind=warning
// guard drops a stale warning against a since-disabled webhook as a
// second line of defense.)
func (w *AutoDisableWorker) Tick(ctx context.Context) {
	var disabledTx identity.WebhookNotifyTx
	if w.notifier != nil {
		disabledTx = w.notifier.EnqueueDisabledTx
	}
	if n, err := w.store.AutoDisableFailingWebhooks(ctx, disabledTx); err != nil {
		log.Printf("[wsd-autodisable] AutoDisableFailingWebhooks err: %v", err)
	} else if n > 0 {
		log.Printf("[wsd-autodisable] disabled %d failing webhook(s)", n)
	}
	// The warn pass exists ONLY to feed the notification pipeline, so with
	// no notifier wired it is skipped entirely rather than run with a nil
	// enqueuer: stamping warn_notified_at without an email would be dead
	// bookkeeping that also suppresses the first real warning if the
	// operator later configures SMTP. (The disable pass above is different
	// — the breaker is core behavior and runs with or without emails.)
	if w.notifier != nil {
		if n, err := w.store.WarnFailingWebhooks(ctx, w.notifier.EnqueueWarningTx); err != nil {
			log.Printf("[wsd-autodisable] WarnFailingWebhooks err: %v", err)
		} else if n > 0 {
			log.Printf("[wsd-autodisable] warned %d failing webhook(s)", n)
		}
	}
	if n, err := w.store.ClearExpiredPrevSecrets(ctx); err != nil {
		log.Printf("[wsd-autodisable] ClearExpiredPrevSecrets err: %v", err)
	} else if n > 0 {
		log.Printf("[wsd-autodisable] cleared %d expired prev secret(s)", n)
	}
}
