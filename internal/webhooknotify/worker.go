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
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
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
	// OperationRef is the durable sending operation the sweep's transaction
	// prepared; a job from a pre-floor slot carries none and is resolved at
	// fire time, then stamped.
	OperationRef *sendingpolicy.OperationRef `json:"operation_ref,omitempty"`
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

// Deliverer is the two-phase send of one health email. Compose does every
// fallible, provider-free step (owner lookup, failure stats, MIME, DKIM) and
// returns the envelope; Submit hands that envelope and a freshly consumed
// authorization to the provider seam. The split lets the worker
// ConsumeAttempt immediately before the socket opens, so a compose failure
// costs no charged ordinal. Implemented by *Notifier.
type Deliverer interface {
	Compose(ctx context.Context, wh *identity.Webhook, kind string) (outbound.Envelope, DeliverOutcome)
	Submit(ctx context.Context, env outbound.Envelope, auth sendingpolicy.ProviderAuthorization) DeliverOutcome
}

// OperationResolver recovers the durable operation for a job that carries no
// reference, through the same Prepare path the sweep's enqueue runs. The kind
// selects the episode (warning or disable) the operation is keyed by.
type OperationResolver func(ctx context.Context, webhookID, kind string) (sendingpolicy.OperationRef, error)

// errOperationMismatch marks a job whose operation reference names another
// episode's (or another webhook's) operation: authorizing it would charge the
// wrong operation, and a reference for a superseded episode is stale anyway.
var errOperationMismatch = errors.New("webhook notify: job operation reference does not name this episode")

// maxNotifyAge bounds how long a health notice may wait behind a gate hold.
// A pause has no clock of its own, and a disabled webhook never self-clears,
// so without this a held notice would snooze forever; a week-old health
// notice is stale by any reading.
const maxNotifyAge = 7 * 24 * time.Hour

// ArgStamper persists a resolved reference into the job's args.
type ArgStamper func(ctx context.Context, jobID int64, ref sendingpolicy.OperationRef) error

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
	gate      sendingpolicy.Gate
	resolve   OperationResolver
	stamp     ArgStamper
	restamp   ArgStamper
	metrics   Metrics // nil ⇒ no emission (nil-safe via emitNotify)
}

func NewNotifyWorker(store Store, deliverer Deliverer) *NotifyWorker {
	return &NotifyWorker{store: store, deliverer: deliverer}
}

// WithMetrics swaps in a metrics backend. Nil-safe: unset (or nil) means no
// emission, so tests and self-host builds don't have to wire anything.
// WithGate injects the sending-protection gate every notification must pass.
func (w *NotifyWorker) WithGate(g sendingpolicy.Gate) *NotifyWorker {
	if g != nil {
		w.gate = g
	}
	return w
}

// WithOperationResolver injects the legacy-argument resolver.
func (w *NotifyWorker) WithOperationResolver(r OperationResolver) *NotifyWorker {
	if r != nil {
		w.resolve = r
	}
	return w
}

// WithArgStamper injects the job-args stamp used after a legacy resolution
// (adds the reference only when absent).
func (w *NotifyWorker) WithArgStamper(s ArgStamper) *NotifyWorker {
	if s != nil {
		w.stamp = s
	}
	return w
}

// WithArgRestamper injects the unconditional re-key used when a job carries
// a pre-derivation reference.
func (w *NotifyWorker) WithArgRestamper(s ArgStamper) *NotifyWorker {
	if s != nil {
		w.restamp = s
	}
	return w
}

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
	if kind == KindDisabled && wh.AutoDisabledAt == nil {
		// Guard 5: disabled by hand, not by the breaker — there is no
		// auto-disable episode to report.
		w.emitNotify(kind, outcomeSkipped)
		return nil
	}
	if !job.CreatedAt.IsZero() && time.Since(job.CreatedAt) > maxNotifyAge {
		// Guard 6: a notice that waited a week behind a hold is stale; drop
		// it rather than snooze forever behind a paused account.
		log.Printf("[webhook-notify] dropping %s notice for %s: older than %s", kind, wh.ID, maxNotifyAge)
		w.emitNotify(kind, outcomeSkipped)
		return nil
	}

	// Compose first: the owner lookup, failure stats, MIME and DKIM are
	// fallible and provider-free, so they run before any attempt is charged.
	env, out := w.deliverer.Compose(ctx, wh, kind)
	if out.Err != nil {
		return w.verdict(job, wh.ID, kind, "compose", out)
	}

	// Every provider call is authorized: Reserve, hold without I/O, then
	// ConsumeAttempt as the LAST decision before Submit, whose submitter
	// redeems the token immediately before the socket opens. A health notice
	// has no durable hold class; the guards above re-run on every execution
	// and drop a notice that went stale while it waited.
	auth := sendingpolicy.ProviderAuthorization{}
	if w.gate != nil {
		ref, err := w.operationFor(ctx, job, wh)
		if err != nil {
			if errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
				w.emitNotify(kind, outcomeSkipped)
				return nil
			}
			if errors.Is(err, errOperationMismatch) {
				w.emitNotify(kind, outcomeSkipped)
				return river.JobCancel(err)
			}
			w.emitNotify(kind, outcomeRetryable)
			return err
		}
		early, attempt, err := w.gate.Reserve(ctx, ref)
		if err != nil {
			if errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
				w.emitNotify(kind, outcomeSkipped)
				return nil
			}
			w.emitNotify(kind, outcomeOutage)
			return river.JobSnooze(notifyOutageSnooze)
		}
		if !early.Allow {
			return w.holdVerdict(kind, early)
		}
		decision, token, err := w.gate.ConsumeAttempt(ctx, attempt)
		if err != nil {
			if errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
				w.emitNotify(kind, outcomeSkipped)
				return nil
			}
			w.emitNotify(kind, outcomeOutage)
			return river.JobSnooze(notifyOutageSnooze)
		}
		if !decision.Allow || token == nil {
			return w.holdVerdict(kind, decision)
		}
		auth = *token
	}

	out = w.deliverer.Submit(ctx, env, auth)
	if out.Err == nil {
		log.Printf("[webhook-notify] sent %s email: webhook=%s", kind, wh.ID)
		w.emitNotify(kind, outcomeSent)
		return nil
	}
	return w.verdict(job, wh.ID, kind, "send", out)
}

// verdict turns a classified failure into River's answer.
func (w *NotifyWorker) verdict(job *river.Job[WebhookNotifyArgs], webhookID, kind, phase string, out DeliverOutcome) error {
	if out.Permanent {
		// e.g. the owner address is rejected 5xx, or there is no owner email
		// on record. Cancel (no retry) rather than churn the tail.
		log.Printf("[webhook-notify] permanent %s failure for %s (%s, no retry): %v", phase, webhookID, kind, out.Err)
		w.emitNotify(kind, outcomePermanent)
		return river.JobCancel(out.Err)
	}
	if out.Outage {
		// Relay unreachable — snooze without burning an attempt. The guards
		// re-run on the next attempt, so a notification that goes stale
		// during the outage still drops correctly.
		w.emitNotify(kind, outcomeOutage)
		return river.JobSnooze(notifyOutageSnooze)
	}
	// Transient: let River reschedule per NextRetry until MaxNotifyAttempts.
	w.emitNotify(kind, outcomeRetryable)
	return fmt.Errorf("webhook notify attempt %d %s failed: %w", job.Attempt, phase, out.Err)
}

// operationFor returns the job's durable operation, resolving and stamping a
// legacy job through the sweep's Prepare path.
func (w *NotifyWorker) operationFor(ctx context.Context, job *river.Job[WebhookNotifyArgs], wh *identity.Webhook) (sendingpolicy.OperationRef, error) {
	// The episode's operation is derived from the webhook, the kind and the
	// timestamp the sweep stamped, so a reference naming any other operation
	// is either another account's (never authorize it) or a superseded
	// episode's (nothing left to say): the binding the message worker
	// enforces, checked before Reserve.
	want := ExpectedOperationID(wh, job.Args.NotifyKind)
	stamp := w.stamp
	if job.Args.OperationRef != nil && !job.Args.OperationRef.IsZero() {
		stored := job.Args.OperationRef.ID()
		if stored == want {
			return *job.Args.OperationRef, nil
		}
		if sendingpolicy.IsWebhookHealthOperationID(stored) {
			// A derived id for another webhook or a superseded episode. (Any
			// other shape is re-derived from this job's own source below, so no
			// stored id can redirect attribution.)
			return sendingpolicy.OperationRef{}, errOperationMismatch
		}
		// A pre-derivation reference (migration 113's op_<md5>, or the first
		// build of this seam): its source is still this job's own webhook,
		// so re-derive through the same Prepare path and replace it, once.
		log.Printf("[webhook-notify] job %d carries a pre-derivation operation reference %s; re-keying", job.ID, stored)
		stamp = w.restamp
	}
	if w.resolve == nil {
		return sendingpolicy.OperationRef{}, fmt.Errorf("webhook notify: legacy job %d carries no operation and no resolver is wired", job.ID)
	}
	ref, err := w.resolve(ctx, job.Args.WebhookID, job.Args.NotifyKind)
	if err != nil {
		return sendingpolicy.OperationRef{}, err
	}
	if ref.ID() != want {
		return sendingpolicy.OperationRef{}, errOperationMismatch
	}
	if stamp != nil {
		if err := stamp(ctx, job.ID, ref); err != nil {
			log.Printf("[webhook-notify] stamp operation on legacy job %d: %v", job.ID, err)
		}
	}
	return ref, nil
}

// holdVerdict turns a gate hold into River's answer: a terminal hold cancels
// the job; everything else waits for the gate's retry time or the outage pace.
func (w *NotifyWorker) holdVerdict(kind string, d sendingpolicy.Decision) error {
	if d.Terminal {
		w.emitNotify(kind, outcomePermanent)
		return river.JobCancel(fmt.Errorf("webhook notify: sending policy: %s", d.Reason))
	}
	w.emitNotify(kind, outcomeOutage)
	delay := notifyOutageSnooze
	if !d.RetryAt.IsZero() {
		if until := time.Until(d.RetryAt); until > delay {
			delay = until
		}
	}
	return river.JobSnooze(delay)
}

// ExpectedOperationID is the operation a notice of the given kind for this
// webhook's current episode must carry: the same derivation the gate's
// PrepareNotificationTx uses. Empty when the episode was never stamped.
func ExpectedOperationID(wh *identity.Webhook, kind string) string {
	if wh == nil {
		return ""
	}
	var episode *time.Time
	switch kind {
	case KindWarning:
		episode = wh.WarnNotifiedAt
	case KindDisabled:
		episode = wh.AutoDisabledAt
	}
	if episode == nil {
		return ""
	}
	return sendingpolicy.WebhookHealthOperationID(wh.ID, kind, *episode)
}

// Gate exposes the wired gate (nil when gateless), for the composition
// root's wiring test.
func (w *NotifyWorker) Gate() sendingpolicy.Gate { return w.gate }
