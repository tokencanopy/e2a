// Package webhookdelivery is Layer 3 of the webhook pipeline
// (docs/design/webhook-delivery-river-migration.md): the River execution stage
// that POSTs a webhook_subscriber_deliveries (Layer 2) row to the customer
// endpoint and retries it. It replaces the hand-rolled SKIP-LOCKED claim + retry
// worker (internal/webhook/subscriber_retry.go) with a River Worker on the shared
// `webhook` queue; River owns claim/lease/retry/backoff, and Layer 2 becomes pure
// delivery state written here for the history API.
//
// Delivery is at-least-once (the industry standard — Stripe/Svix/Postmark):
// River re-drives a crashed job, so an endpoint may receive a duplicate if the
// POST succeeds but the worker crashes before writing `delivered`. Consumers
// dedup on the event id in the signed payload.
package webhookdelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/webhook"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// retryBackoffs is the GA-frozen eight-attempt delivery envelope. Entry zero is
// the delay after failed attempt one; the eighth attempt is terminal, so seven
// delays span 29h21m from the initial attempt to the final attempt.
var retryBackoffs = []time.Duration{
	1 * time.Minute, 5 * time.Minute, 15 * time.Minute, 1 * time.Hour,
	4 * time.Hour, 8 * time.Hour, 16 * time.Hour,
}

// MaxDeliveryAttempts is River's MaxAttempts for a delivery job — after this many
// failed attempts River discards and the worker marks Layer 2 'failed'.
const MaxDeliveryAttempts = 8

// disabledSnooze is how long a delivery whose webhook is currently disabled is
// rescheduled (no attempt burned) — mirrors the legacy disabledDeferral.
const disabledSnooze = time.Hour

// WebhookReader is the narrow webhook-lookup surface the worker needs.
// *identity.Store satisfies it.
type WebhookReader interface {
	GetWebhookByIDInternal(ctx context.Context, webhookID string) (*identity.Webhook, error)
}

// Metrics is the narrow slice of telemetry.Metrics the delivery worker emits
// (same pattern as internal/janitor.Metrics). Injectable so tests use a fake
// recorder; satisfied by any telemetry backend.
type Metrics interface {
	// WebhookAttempt records one delivery attempt. outcome ∈ {delivered,
	// retryable_failure, exhausted, webhook_deleted, skipped_disabled};
	// statusClass is "1xx".."5xx" or "none" (no HTTP response).
	// A negative seconds means "count the attempt, record no duration
	// sample" — used for outcomes with no HTTP POST (webhook_deleted,
	// skipped_disabled), which would otherwise drag the duration
	// quantiles toward zero.
	WebhookAttempt(outcome, statusClass string, seconds float64)

	// WebhookDeliveryRescued counts delivery rows the reconciler's dead-job
	// arm re-drove (fresh job replacing a terminal/pruned one). A climbing
	// rate is the poison-row signal.
	WebhookDeliveryRescued(count int)
	// WebhookFirstAttemptLatency records event→first-attempt latency for
	// one subscriber delivery (attempt start − webhook_events.created_at).
	// Observed only on a first-delivery row's FIRST HTTP attempt — retries,
	// replays, and the no-POST outcomes never observe.
	WebhookFirstAttemptLatency(seconds float64)
}

// statusClassOf maps an HTTP status code to its metrics label ("1xx".."5xx"),
// or "none" when no response was received (connect/DNS/SSRF-blocked → code 0).
func statusClassOf(code int) string {
	if code <= 0 {
		return "none"
	}
	return fmt.Sprintf("%dxx", code/100)
}

// Deliverer is the POST surface. *webhook.SubscriberDeliverer satisfies it.
type Deliverer interface {
	Deliver(ctx context.Context, url string, body []byte, secret, secretPrev, eventType, schemaVersion string) webhook.DeliveryOutcome
}

// WebhookDeliverArgs carries only the Layer 2 delivery id — the worker reads the
// payload + webhook from the DB (keeps river_job rows tiny; Layer 2 is source of
// truth).
type WebhookDeliverArgs struct {
	DeliveryID string `json:"delivery_id"`
}

func (WebhookDeliverArgs) Kind() string { return "webhook_deliver" }

// DeliverWorker delivers one Layer 2 row. Mirrors the legacy processOne: re-fetch
// the webhook per attempt (disabled → snooze, deleted → cancel), POST, write
// Layer 2 state. River owns retry/backoff via NextRetry below.
type DeliverWorker struct {
	river.WorkerDefaults[WebhookDeliverArgs]
	subStore  *webhook.SubscriberStore
	deliverer Deliverer
	webhooks  WebhookReader
	metrics   Metrics // nil ⇒ no emission (nil-safe via emitAttempt)
}

// NewDeliverWorker constructs the worker. Used by the Registrar and by tests.
func NewDeliverWorker(subStore *webhook.SubscriberStore, deliverer Deliverer, webhooks WebhookReader) *DeliverWorker {
	return &DeliverWorker{subStore: subStore, deliverer: deliverer, webhooks: webhooks}
}

// WithMetrics swaps in a metrics backend. Nil-safe: unset (or nil) means no
// emission, so tests don't have to wire anything.
func (w *DeliverWorker) WithMetrics(m Metrics) *DeliverWorker {
	w.metrics = m
	return w
}

// emitAttempt records one WebhookAttempt, tolerating an unwired backend.
func (w *DeliverWorker) emitAttempt(outcome, statusClass string, seconds float64) {
	if w.metrics != nil {
		w.metrics.WebhookAttempt(outcome, statusClass, seconds)
	}
}

// emitFirstAttemptLatency records the event→first-attempt latency, tolerating
// an unwired backend.
func (w *DeliverWorker) emitFirstAttemptLatency(seconds float64) {
	if w.metrics != nil {
		w.metrics.WebhookFirstAttemptLatency(seconds)
	}
}

// snoozeCount reports how often River snoozed this job. The disabled-webhook
// deferral is this worker's ONLY snooze source, so a non-zero count means
// the job sat through a customer-disabled window. River's job executor
// increments a "snoozes" counter in the job's metadata JSON on every snooze
// (absent before the first); JobRow.Metadata is the public carrier.
func snoozeCount(job *river.Job[WebhookDeliverArgs]) int {
	if job == nil || len(job.Metadata) == 0 {
		return 0
	}
	var md struct {
		Snoozes int `json:"snoozes"`
	}
	if err := json.Unmarshal(job.Metadata, &md); err != nil {
		return 0
	}
	return md.Snoozes
}

// NextRetry overrides River's client-wide policy for webhook jobs only, returning
// the exact 29h21m envelope. job.Attempt is the just-failed attempt (1-based); River
// discards at MaxAttempts so this is called for attempts 1..MaxDeliveryAttempts-1.
func (w *DeliverWorker) NextRetry(job *river.Job[WebhookDeliverArgs]) time.Time {
	i := job.Attempt - 1
	if i < 0 || i >= len(retryBackoffs) {
		return time.Time{} // fall back to client policy; River discards at MaxAttempts anyway
	}
	return time.Now().Add(retryBackoffs[i])
}

// Work intentionally has no Timeout() override — a single HTTP POST (bounded by the
// deliverer's own client timeout) fits River's 60s default JobTimeout.
func (w *DeliverWorker) Work(ctx context.Context, job *river.Job[WebhookDeliverArgs]) error {
	d, err := w.subStore.GetSubscriberDeliveryByID(ctx, job.Args.DeliveryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // delivery row gone (webhook deleted, cascaded) — nothing to do
	}
	if err != nil {
		// DB error — retryable. On the FINAL attempt, River discards after
		// this: give the row its terminal write NOW, or it becomes a poison
		// row — every dead-job rescue (ReconcilePending rescues discarded
		// jobs) re-drives it with a fresh 29h21m envelope that fails at
		// row-load again, leaving it customer-visibly 'pending' forever. The
		// write is conditional on still-pending — the read failed, so the
		// row's true state is unknown and a delivered row must never be
		// clobbered — and last_error is a constant (customer-facing; a raw
		// pgx error would leak internal hosts/IPs and DB identifiers).
		// Counted exhausted (attempts consumed without a POST), never
		// webhook_deleted.
		if job.Attempt >= MaxDeliveryAttempts {
			w.emitAttempt("exhausted", "none", -1)
			if merr := w.subStore.MarkSubscriberFailedIfPending(ctx, job.Args.DeliveryID, job.Attempt, "internal error loading delivery", 0); merr != nil {
				log.Printf("[webhook-deliver] CRITICAL: terminal 'failed' write for delivery %s failed after row-load error (row stays pending; the reconciler will re-drive it once this job is discarded): %v", job.Args.DeliveryID, merr)
			}
		}
		return err
	}
	if d.Status != "pending" {
		// Terminal row — idempotent no-op. Covers a re-drive after a crash
		// post-MarkDelivered ('delivered') and a job waking against a row the
		// expiry janitor already marked 'failed' (expired before delivery) or
		// whose terminal write landed after all: a terminal row must never be
		// POSTed again.
		return nil
	}

	wh, err := w.webhooks.GetWebhookByIDInternal(ctx, d.WebhookID)
	if err != nil && !errors.Is(err, identity.ErrWebhookNotFound) {
		// Anything but a genuine miss is an infrastructure error (Postgres
		// blip, pool exhaustion, network reset) — retryable. Terminally
		// cancelling here would permanently fail a deliverable row, mislabel
		// it "webhook not found", and hide the loss behind the
		// customer-fault webhook_deleted metric.
		if job.Attempt >= MaxDeliveryAttempts {
			// Final attempt — River discards after this regardless, so the
			// terminal 'failed' write is the row's last chance (mirrors the
			// POST-failure path below): without it the row would sit
			// 'pending' with a dead job_id the reconciler can't see. The
			// delivery exhausted its attempts without a POST — count it as
			// exhausted (e2a-side, in the attempt-health denominator), never
			// webhook_deleted. last_error is a CONSTANT: it is returned
			// verbatim by GET /v1/webhooks/{id}/deliveries, and a raw pgx
			// error would leak internal hosts/IPs and DB identifiers. The
			// returned err carries the real detail to River's job-error log.
			w.emitAttempt("exhausted", "none", -1)
			if merr := w.markFailedReliably(ctx, d.ID, job.Attempt, "internal error resolving webhook", 0); merr != nil {
				log.Printf("[webhook-deliver] CRITICAL: terminal 'failed' write for delivery %s failed after retries (row stays pending; the reconciler will re-drive it once this job is discarded): %v", d.ID, merr)
			}
		}
		return err
	}
	if err != nil {
		// Webhook deleted (the store's ErrWebhookNotFound sentinel — a real
		// miss) — terminal. Write the terminal status; if that write
		// fails, return the error (retryable) rather than cancelling with an
		// unmarked row — a JobCancel here would leave the row 'pending' with a
		// dead job until the reconciler's dead-job rescue re-drives it, an
		// avoidable extra delivery cycle when the retryable error path settles
		// it directly.
		if merr := w.markFailedReliably(ctx, d.ID, job.Attempt, "webhook not found", 0); merr != nil {
			return merr
		}
		w.emitAttempt("webhook_deleted", "none", -1) // no POST happened — no duration sample
		return river.JobCancel(fmt.Errorf("webhook %s not found: %w", d.WebhookID, err))
	}
	if !wh.Enabled {
		// Disabled — reschedule without burning an attempt (matches legacy defer).
		w.emitAttempt("skipped_disabled", "none", -1) // no POST happened — no duration sample
		return river.JobSnooze(disabledSnooze)
	}

	// Honor the previous signing secret only within its rotation grace window.
	prevSecret := wh.SigningSecretPrev
	if wh.SigningSecretPrevExpiresAt != nil && time.Now().After(*wh.SigningSecretPrevExpiresAt) {
		prevSecret = ""
	}

	// Schema version is stamped from the current constant at delivery time (not read
	// from the stored envelope bytes) so a redelivered pre-schema_version event still
	// carries it; the event type comes from the Layer 2 delivery row.
	start := time.Now()
	out := w.deliverer.Deliver(ctx, wh.URL, d.EventPayload, wh.SigningSecret, prevSecret,
		d.EventType, strconv.Itoa(webhookpub.SchemaVersion))
	dur := time.Since(start).Seconds()
	// Event→first-attempt latency SLI: observed ONLY on this delivery row's
	// first HTTP attempt — the row shows no recorded prior attempt — and only
	// for first-delivery rows. Not gated on River's attempt number: if
	// transient pre-POST errors consume early attempts, the true first POST
	// runs later and must still be observed, or the slowest first attempts
	// (the incident population) never reach the SLI. A replay row (replay_id
	// set) is excluded: its baseline would be the ORIGINAL event's
	// created_at, recording the customer's replay lag as a giant false
	// outlier. The no-POST outcomes (webhook_deleted,
	// skipped_disabled) returned above and never reach here; the SLO
	// measures the first attempt regardless of its outcome. At-most-once:
	// once any attempt is recorded the row never observes again.
	//
	// Disabled-window exclusion: the disabled path is this worker's ONLY
	// snooze source, and River counts snoozes in job metadata — a snoozed
	// job sat through a customer-disabled window (hour-scale, possibly
	// days), which is not e2a's event→attempt latency, so it never observes.
	// (River does not burn the attempt number on snooze, so job.Attempt
	// cannot discriminate; a scheduled_at heuristic would false-skip jobs
	// whose chained PRE-POST failures walked the retry envelope with zero
	// recorded attempts — the exact incident population this SLI exists to
	// catch.) The residual — a crash after the POST but before the attempt
	// record — can double-observe on the retry; that is the same accepted
	// residual as at-least-once delivery itself, and rare.
	if d.Attempts == 0 && d.ReplayID == nil && d.EventCreatedAt != nil && snoozeCount(job) == 0 {
		if latency := start.Sub(*d.EventCreatedAt).Seconds(); latency > 0 {
			w.emitFirstAttemptLatency(latency)
		}
	}
	if out.Success {
		w.emitAttempt("delivered", statusClassOf(out.StatusCode), dur)
		return w.subStore.MarkDelivered(ctx, d.ID, out.StatusCode) // nil → River completes the job
	}

	if job.Attempt >= MaxDeliveryAttempts {
		w.emitAttempt("exhausted", statusClassOf(out.StatusCode), dur)
		// Last attempt — River discards after this regardless of return value, so
		// this is the row's best chance at its terminal 'failed' write.
		// markFailedReliably retries it; if a sustained DB outage swallows every
		// try, the row stays 'pending' with a soon-discarded job and the
		// reconciler's dead-job rescue re-drives it with a fresh job (a fresh
		// retry envelope, ending in another terminal-write window) — logged
		// CRITICAL because reaching this line at all means the DB was down
		// across every retry of a single-row UPDATE.
		if merr := w.markFailedReliably(ctx, d.ID, job.Attempt, out.Error, out.StatusCode); merr != nil {
			log.Printf("[webhook-deliver] CRITICAL: terminal 'failed' write for delivery %s failed after retries (row stays pending; the reconciler will re-drive it once this job is discarded): %v", d.ID, merr)
		}
		return fmt.Errorf("webhook delivery failed (final attempt %d, status %d): %s", job.Attempt, out.StatusCode, out.Error)
	}
	// Retryable failure — record the attempt (status stays pending) and return an
	// error so River retries per NextRetry.
	w.emitAttempt("retryable_failure", statusClassOf(out.StatusCode), dur)
	if rerr := w.subStore.RecordSubscriberAttempt(ctx, d.ID, job.Attempt, out.Error, out.StatusCode); rerr != nil {
		return rerr
	}
	return fmt.Errorf("webhook delivery attempt %d failed (status %d): %s", job.Attempt, out.StatusCode, out.Error)
}

// markFailedReliably writes the terminal 'failed' status, retrying a transient DB
// error a few times. The terminal write keeps the row's status in sync with its
// (about-to-be-terminal) River job; if it is lost, the row sits 'pending' with a
// dead job_id until the reconciler's dead-job rescue re-drives it — recoverable,
// but only after another full delivery cycle, so this write still tries hard.
// Bounded, short backoff so the (rare) failure case adds < ~1s.
func (w *DeliverWorker) markFailedReliably(ctx context.Context, id string, attempt int, errMsg string, statusCode int) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = w.subStore.MarkSubscriberFailed(ctx, id, attempt, errMsg, statusCode); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * 150 * time.Millisecond):
		}
	}
	return err
}
