// Package webhooknotify sends the two webhook health emails
// (docs/design/2026-08-08-webhook-health-notifications.md): an early
// WARNING when a webhook's deliveries start failing at the attempt level,
// and a DISABLED notice when the auto-disable breaker trips it.
//
// It mirrors internal/hitlnotify's three-layer shape deliberately — that
// design (docs/design/hitl-notify-river.md) records why the naive inline
// send fails: a crash or SMTP outage between the state change and the
// send loses the notification forever. Here the maintenance sweep
// enqueues the webhook_notify job in the SAME transaction as the state
// transition (warn stamp / disable flip), and this worker recomposes and
// submits the email once off the sweep path, with River owning retries.
package webhooknotify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/identity"
)

// Notification kinds. One worker, two templates: the guards and the
// delivery/error triage are identical, and the bodies differ only in copy
// and severity. The Kind field in the job args is the seam where a third
// kind lands without new plumbing.
const (
	KindWarning  = "warning"
	KindDisabled = "disabled"
)

// notifyRetryBackoffs is the per-attempt delay for a failed notification
// send — same short envelope as hitlnotify's: a health alert is time-
// sensitive, so there is no point in a multi-hour tail.
var notifyRetryBackoffs = []time.Duration{
	15 * time.Second,
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
}

// MaxNotifyAttempts caps the retry tail before River discards the job.
const MaxNotifyAttempts = 6

// notifyOutageSnooze defers a job when the relay is unreachable, without
// burning an attempt (mirrors hitlnotify's outage snooze).
const notifyOutageSnooze = 5 * time.Minute

// WebhookNotifyArgs drives one health-notification job. Args carry only
// the webhook id + kind; the worker re-reads the webhook row (the source
// of truth) each attempt, so the guards always see current state.
type WebhookNotifyArgs struct {
	WebhookID string `json:"webhook_id"`
	// NotifyKind ∈ {warning, disabled}. (Named NotifyKind because river's
	// JobArgs interface reserves the Kind() method name.)
	NotifyKind string `json:"kind"`
}

func (WebhookNotifyArgs) Kind() string { return "webhook_notify" }

// DeliverOutcome is the classified result of one notification send.
// Permanent and Outage split the retry decision exactly as hitlnotify's
// does, using the shared internal/outbound SMTP classifiers.
type DeliverOutcome struct {
	Err       error
	Permanent bool // 5xx / validation / no owner email — no retry
	Outage    bool // relay unreachable — snooze without spending an attempt
}

// Deliverer composes and sends one health email. Implemented by *Notifier
// (compose + SMTPRelay.SendOnce + classify).
type Deliverer interface {
	Deliver(ctx context.Context, wh *identity.Webhook, kind string) DeliverOutcome
}

// Store is the read surface the worker needs. *identity.Store satisfies it.
type Store interface {
	// GetWebhookByIDInternal loads the webhook with no ownership check —
	// ownership was established when the sweep selected the row.
	GetWebhookByIDInternal(ctx context.Context, webhookID string) (*identity.Webhook, error)
}

// Metrics is the narrow slice of telemetry.Metrics this worker emits (same
// pattern as internal/webhookdelivery.Metrics — declare only what is used, so
// tests wire a one-method fake). Satisfied by any telemetry backend.
type Metrics interface {
	// WebhookNotify records one notification job outcome. kind ∈ {warning,
	// disabled}; outcome ∈ {sent, permanent, outage, retryable, skipped}.
	// skipped means a staleness guard decided not to send, which is
	// counted apart from the failure outcomes on purpose: otherwise a fall
	// in sends cannot be told apart from a send path that has died.
	WebhookNotify(kind, outcome string)
}

// Notification outcomes (the outcome label). Every exit from Work emits
// exactly one.
const (
	outcomeSent      = "sent"      // email accepted by the relay
	outcomePermanent = "permanent" // JobCancel — no retry (5xx, no owner address)
	outcomeOutage    = "outage"    // relay unreachable — snoozed
	outcomeRetryable = "retryable" // transient — River reschedules
	outcomeSkipped   = "skipped"   // a guard decided not to send
)

// NotifyWorker sends one health notification. Mirrors hitlnotify.NotifyWorker.
type NotifyWorker struct {
	river.WorkerDefaults[WebhookNotifyArgs]
	store     Store
	deliverer Deliverer
	metrics   Metrics // nil ⇒ no emission (nil-safe via emitNotify)
}

func NewNotifyWorker(store Store, deliverer Deliverer) *NotifyWorker {
	return &NotifyWorker{store: store, deliverer: deliverer}
}

// WithMetrics swaps in a metrics backend. Nil-safe: unset (or nil) means no
// emission, so tests and self-host builds don't have to wire anything.
func (w *NotifyWorker) WithMetrics(m Metrics) *NotifyWorker {
	w.metrics = m
	return w
}

// emitNotify records one outcome, tolerating an unwired backend.
func (w *NotifyWorker) emitNotify(kind, outcome string) {
	if w.metrics != nil {
		w.metrics.WebhookNotify(kind, outcome)
	}
}

// NextRetry overrides River's default backoff with the notify envelope.
func (w *NotifyWorker) NextRetry(job *river.Job[WebhookNotifyArgs]) time.Time {
	i := job.Attempt
	if i < 0 || i >= len(notifyRetryBackoffs) {
		return time.Time{} // fall back to River's default at the tail
	}
	return time.Now().Add(notifyRetryBackoffs[i])
}

// Work re-reads the webhook and applies the staleness guards before
// delivering. Every guard is a silent no-op (return nil): the email would
// be misleading if sent, so dropping it is the correct, fail-closed
// outcome. An unknown kind also refuses to send (fail-closed).
//
// Every exit emits exactly one WebhookNotify sample, so the counter's total
// tracks job completions and no branch can go dark.
func (w *NotifyWorker) Work(ctx context.Context, job *river.Job[WebhookNotifyArgs]) error {
	kind := job.Args.NotifyKind
	if kind != KindWarning && kind != KindDisabled {
		log.Printf("[webhook-notify] unknown notification kind %q for webhook %s — dropping (fail-closed)", kind, job.Args.WebhookID)
		w.emitNotify(kind, outcomeSkipped)
		return nil
	}

	wh, err := w.store.GetWebhookByIDInternal(ctx, job.Args.WebhookID)
	if errors.Is(err, identity.ErrWebhookNotFound) {
		w.emitNotify(kind, outcomeSkipped)
		return nil // guard 1: webhook deleted — nothing to report
	}
	if err != nil {
		w.emitNotify(kind, outcomeRetryable)
		return err // DB error — retryable
	}
	if kind == KindDisabled && wh.Enabled {
		// Guard 2: the user already fixed and re-enabled inside the window;
		// "we disabled your webhook" is now misleading.
		w.emitNotify(kind, outcomeSkipped)
		return nil
	}
	if kind == KindWarning && !wh.Enabled {
		// Guard 3: disabled outright since the warn was enqueued — the
		// disable email supersedes the warning.
		w.emitNotify(kind, outcomeSkipped)
		return nil
	}
	if kind == KindWarning && wh.WarnNotifiedAt == nil {
		// Guard 4: a successful delivery cleared the warn marker after this
		// job was enqueued — the endpoint recovered on its own.
		w.emitNotify(kind, outcomeSkipped)
		return nil
	}

	out := w.deliverer.Deliver(ctx, wh, kind)
	if out.Err == nil {
		w.emitNotify(kind, outcomeSent)
		return nil
	}
	if out.Permanent {
		// e.g. the owner address is rejected 5xx, or there is no owner email
		// on record. Cancel (no retry) rather than churn the tail.
		log.Printf("[webhook-notify] permanent send failure for %s (%s, no retry): %v", wh.ID, kind, out.Err)
		w.emitNotify(kind, outcomePermanent)
		return river.JobCancel(out.Err)
	}
	if out.Outage {
		// Relay unreachable — snooze without burning an attempt. The guards
		// above re-run on the next attempt, so a notification that goes
		// stale during the outage still drops correctly.
		w.emitNotify(kind, outcomeOutage)
		return river.JobSnooze(notifyOutageSnooze)
	}
	// Transient: let River reschedule per NextRetry until MaxNotifyAttempts.
	w.emitNotify(kind, outcomeRetryable)
	return fmt.Errorf("webhook notify attempt %d failed: %w", job.Attempt, out.Err)
}
