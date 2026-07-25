// Package outboundsend is Layer 3 of the outbound pipeline
// (docs/design/async-message-pipeline.md): the River execution stage that submits
// an accepted message to the upstream provider (SES) and records the terminal
// outcome. It mirrors internal/webhookdelivery — a River Worker on the shared
// `outbound` queue, with River owning claim / retry / rescue.
//
// Delivery is at-least-once: River re-drives a crashed job, so the provider may
// receive a duplicate if the SMTP submit is accepted but the worker crashes before
// marking the message sent. That residual is narrowed by the X-E2A-Message-ID
// wire header + SNS reconciliation (async-send-contract §3.1): the SNS consumer
// records provider-accept evidence on the row, the re-driven claim then settles
// the message as sent instead of re-submitting, and the terminal-failure guard
// (here and in the terminal reconciler, via the store's guarded MarkFailed)
// never declares a provider-accepted row failed. A final attempt that fails
// ambiguously defers its terminal write to the reconciler's provider-evidence
// grace window rather than firing an immediate — possibly false — email.failed.
//
// One SMTP attempt per job attempt — River owns the multi-attempt envelope via
// NextRetry, so Work() stays short (the deliverer does a single submit, not an
// internal retry loop). See the design's "claim + rescue, not a lease" note.
package outboundsend

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// sendRetryBackoffs is the per-attempt delay schedule for a failed outbound send —
// the decided envelope (design §4). River drives it via NextRetry; indexed by
// attempt. Provider-outage errors snooze instead of counting an attempt.
var sendRetryBackoffs = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	1 * time.Hour,
	4 * time.Hour,
}

// MaxSendAttempts caps app/permanent-error retries (bounded 4xx/unknown tail).
const MaxSendAttempts = 6

// outageSnoozeInterval is how long a provider-outage job snoozes between probes.
// JobSnooze does NOT count an attempt, so an outage defers rather than exhausting
// MaxSendAttempts (design §8 circuit breaker).
const outageSnoozeInterval = 5 * time.Minute

// rampErrorSnoozeInterval keeps a durable message queued when the ramp store is
// temporarily unavailable. JobSnooze does not consume a River attempt.
const rampErrorSnoozeInterval = time.Minute

// sendRetryHorizon bounds the outage-tolerant tail: past this age (from accept) an
// outage-snoozing job stops deferring and is declared terminally failed. 72h matches
// the industry MTA retry horizon (and the webhook deliverer's envelope) — long enough
// to ride out a multi-hour regional SES incident, not forever.
const sendRetryHorizon = 72 * time.Hour

// OutboundSendArgs drives one outbound send. Args carry only the message id; the
// worker re-reads the messages row (the source of truth) each attempt.
type OutboundSendArgs struct {
	MessageID string `json:"message_id"`
}

func (OutboundSendArgs) Kind() string { return "outbound_send" }

// SendJob is the send payload the worker loads from the messages row (Store.LoadForSend).
type SendJob struct {
	MessageID string
	// UserID is the owning account — the tenant scope for the pre-provider
	// suppression guard (suppressions are per-account).
	UserID       string
	AgentID      string // exact sending agent for agent-scoped consent checks
	Domain       string // exact registered sender domain
	MessageType  string // send|reply|test; platform tests are ramp-exempt
	Status       string // messages.delivery_status
	EnvelopeFrom string
	Recipients   []string
	RawMessage   []byte // composed MIME
	SentAs       string // From identity decided at accept ("own_address"|"relay")
	// AcceptedAt is messages.created_at — the outage tail's clock, so a job that has
	// been snoozing through an outage past sendRetryHorizon can be terminated.
	AcceptedAt time.Time
	// ProviderAccepted is set when authoritatively correlated provider-accept
	// evidence (an SNS-verified, header- or provider-id-matched SES
	// notification) has been recorded for this message: the provider already
	// has it — an earlier attempt's submit landed in the SMTP-accept↔mark-sent
	// crash window — so the worker settles the row as sent instead of
	// re-submitting a duplicate.
	ProviderAccepted   bool
	ProviderAcceptedAt *time.Time
	// ProviderMessageID is the evidence-repaired provider id accompanying
	// ProviderAccepted ('' when no evidence).
	ProviderMessageID string
}

// pastRetryHorizon reports whether the accept is older than the outage-tolerant
// retry horizon. Zero AcceptedAt (unknown) is treated as not-past so an outage keeps
// deferring rather than being falsely terminated on a missing timestamp.
func (j *SendJob) pastRetryHorizon() bool {
	return !j.AcceptedAt.IsZero() && time.Since(j.AcceptedAt) > sendRetryHorizon
}

// alreadyDone reports whether the message has already been submitted to the
// provider — its delivery_status has moved past the pre-send states
// (`accepted`/`sending`) to `sent` or any later/terminal value — and so must not
// be re-sent. This is the idempotent-re-drive gate (a crash re-drive of a `sent`
// row is a no-op). Note delivery.Status.Terminal() is NOT the right check: it
// reports the final SNS outcome (delivered/bounced/complained/failed) and treats
// `sent` as non-terminal, but `sent` still means "already submitted, don't resend".
func (j *SendJob) alreadyDone() bool {
	s := delivery.Status(j.Status)
	return s != delivery.StatusAccepted && s != delivery.StatusSending
}

// DeliverOutcome is the result of one SMTP submit attempt.
type DeliverOutcome struct {
	ProviderMessageID string
	SentAs            string
	Err               error
	// Permanent marks a non-retryable failure (validation / permanent 5xx): the
	// worker fails the message terminally instead of retrying.
	Permanent bool
	// Outage marks a provider-connection failure (relay unreachable/misconfigured):
	// the worker snoozes without burning an attempt (design §8), up to the retry
	// horizon. Mutually exclusive with Permanent in practice.
	Outage bool
}

// Deliverer performs a SINGLE SMTP submit — River owns re-attempts. Implemented in
// the binary over internal/outbound's single-attempt path.
type Deliverer interface {
	Deliver(ctx context.Context, j *SendJob) DeliverOutcome
}

type RampRequest struct {
	MessageID string
	UserID    string
	Domain    string
	Units     int
}

type RampDecision struct {
	Allowed bool
	RetryAt time.Time
}

// RampGate reserves recipient capacity for an eligible custom-domain send.
// Implementations must make a same-message/day call idempotent.
type RampGate interface {
	Reserve(ctx context.Context, req RampRequest) (RampDecision, error)
	Confirm(ctx context.Context, messageID string) error
	Release(ctx context.Context, messageID string) error
	Resolve(ctx context.Context, messageID string) error
}

// Store is the messages-store surface the worker needs. Implemented over
// internal/identity in the binary. ClaimSend atomically checks that the message
// and agent are live and persists delivery_status='sending' for the stamped River
// job before provider I/O begins.
type Store interface {
	// ClaimSend returns nil when the message is gone, trashed, terminal, or owned
	// by a different River job.
	// (agent-delete cascade / TTL) — the worker treats that as a no-op.
	ClaimSend(ctx context.Context, messageID string, jobID int64) (*SendJob, error)
	// ReleaseSend clears a side-effect-free attempt before River backoff.
	ReleaseSend(ctx context.Context, messageID string, jobID int64) error
	// MarkSent records the provider outcome monotonically from a pre-terminal
	// state, including when trash won after ClaimSend.
	MarkSent(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, providerMessageID, sentAs string) error
	// MarkFailed is the GUARDED terminal write (async-send-contract §3.1): if
	// provider-accept evidence has reached the row it settles the message as
	// sent (+ email.sent) instead; otherwise it sets delivery_status='failed'
	// with the given failure provenance + detail and emits email.failed — all
	// in one transaction. Callers therefore invoke it to "finalize a terminal
	// state", not to unconditionally fail.
	// The returned status reports what the guarded write actually did:
	// StatusFailed, StatusSent (evidence settle), or "" (no-op). The returned
	// time is the occurred_at the write actually used — the provider-accept
	// evidence time on an evidence settle, the passed occurredAt on a
	// failure, zero on a no-op — so observability reports what the write
	// did, not what the caller asked for.
	MarkFailed(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, detail string, source delivery.FailureSource, reason messagelifecycle.ReasonCode, blockedRecipients []string) (delivery.Status, time.Time, error)
	PreserveTerminalFailure(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, detail string, source delivery.FailureSource, reason messagelifecycle.ReasonCode, blockedRecipients []string) error
	// DeferTerminalFailure records a final attempt's diagnostic + releases the
	// I/O claim WITHOUT declaring failed: the terminal reconciler declares the
	// outcome after the provider-evidence grace window (or settles the row as
	// sent when evidence arrives first).
	DeferTerminalFailure(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, detail string) error
	// RecordTemporaryFailure atomically records the retryable observation and
	// releases the send claim for River's next attempt.
	RecordTemporaryFailure(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, detail string) error
	// SuppressedRecipients returns the effective account-wide + exact-agent
	// subset — the last-line guard before provider I/O.
	SuppressedRecipients(ctx context.Context, userID, agentID string, recipients []string) ([]string, error)
}

// SendWorker submits an accepted message and records the terminal outcome. Mirrors
// webhookdelivery.DeliverWorker.
type SendWorker struct {
	river.WorkerDefaults[OutboundSendArgs]
	store     Store
	deliverer Deliverer
	ramp      RampGate
	metrics   Metrics
}

func NewSendWorker(store Store, deliverer Deliverer, ramp ...RampGate) *SendWorker {
	w := &SendWorker{store: store, deliverer: deliverer, metrics: noopMetrics{}}
	if len(ramp) > 0 {
		w.ramp = ramp[0]
	}
	return w
}

// WithMetrics injects the SLI recorder. Chainable; nil keeps the no-op
// default so metrics stay optional wiring.
func (w *SendWorker) WithMetrics(m Metrics) *SendWorker {
	if m != nil {
		w.metrics = m
	}
	return w
}

// NextRetry overrides River's default backoff with the decided send envelope.
func (w *SendWorker) NextRetry(job *river.Job[OutboundSendArgs]) time.Time {
	i := job.Attempt
	if i < 0 || i >= len(sendRetryBackoffs) {
		return time.Time{} // fall back to River's default at the tail
	}
	return time.Now().Add(sendRetryBackoffs[i])
}

// Work intentionally has no Timeout() override — a single SES submit comfortably fits
// River's 60s default JobTimeout. (Contrast the maintenance/sweep workers, which
// override it because they can run for minutes.)
func (w *SendWorker) Work(ctx context.Context, job *river.Job[OutboundSendArgs]) error {
	// Queue-wait SLI: due→pickup latency for THIS attempt (River stamps
	// scheduled_at at enqueue, at each retry's backoff target, and on snooze;
	// attempted_at at claim). scheduled_at — NOT created_at — is the baseline:
	// a retried/snoozed/ramp-deferred message would otherwise record its entire
	// cumulative age as "queue wait" on every pass, poisoning the p95. Guarded
	// against zero/negative deltas (clock skew, hand-built rows).
	if job.AttemptedAt != nil && !job.ScheduledAt.IsZero() {
		if wait := job.AttemptedAt.Sub(job.ScheduledAt); wait > 0 {
			w.metrics.OutboundQueueWait(wait.Seconds())
		}
	}
	observedAt := jobObservationTime(job)
	j, err := w.store.ClaimSend(ctx, job.Args.MessageID, job.ID)
	if err != nil {
		return err // DB error — retryable
	}
	if j == nil {
		// A previous terminal attempt may have committed the durable message
		// outcome before ramp cleanup failed. Terminal rows cannot be claimed on
		// retry, so resolve any reservation from that durable outcome here. Resolve
		// is also safe for deleted, non-ramped, and missing messages.
		if w.ramp != nil {
			if err := w.ramp.Resolve(ctx, job.Args.MessageID); err != nil {
				return fmt.Errorf("resolve sending ramp for unclaimable message: %w", err)
			}
		}
		return nil // message gone or already terminal — nothing to provider-submit
	}
	if j.alreadyDone() {
		if w.ramp != nil && j.rampEligible() {
			if err := w.ramp.Resolve(ctx, j.MessageID); err != nil {
				return fmt.Errorf("resolve sending ramp for completed message: %w", err)
			}
		}
		return nil // already submitted (sent+) — idempotent re-drive
	}
	if j.ProviderAccepted {
		// Provider-evidence guard (§3.1): an SNS notification already proved an
		// earlier attempt's submit reached the provider — the crash window
		// between SMTP accept and mark-sent. Re-submitting would duplicate the
		// email; settle the row as sent (email.sent + metering, in the store).
		if j.ProviderAcceptedAt != nil {
			observedAt = j.ProviderAcceptedAt.UTC()
		}
		if err := w.store.MarkSent(ctx, j.MessageID, job.ID, 0, observedAt, j.ProviderMessageID, j.SentAs); err != nil {
			return err
		}
		// Terminal 'sent', but NOT an attempt — the submit happened on an
		// earlier attempt; only the settle lands here. occurredAt is the
		// provider-accept evidence time, so the latency measures
		// acceptance→provider-accept, not acceptance→settle.
		emitTerminal(w.metrics, terminalSent, j.AcceptedAt, observedAt)
		if w.ramp != nil && j.rampEligible() {
			return w.ramp.Confirm(ctx, j.MessageID)
		}
		return nil
	}

	// Ramp only mail that uses a verified customer identity. Platform-originated
	// test mail uses the relay identity and remains exempt; loopback never enters
	// this worker. Reserve after the provider-evidence guard. The final suppression
	// check deliberately follows an allowed reservation, closing the policy window
	// while Reserve waits on shared capacity. Retryable work after Reserve keeps
	// that reservation: same-message/day Reserve is idempotent, while a released
	// reservation is terminal and cannot be re-reserved.
	if w.ramp != nil && j.rampEligible() {
		decision, rerr := w.ramp.Reserve(ctx, RampRequest{
			MessageID: j.MessageID,
			UserID:    j.UserID,
			Domain:    j.Domain,
			Units:     uniqueRecipientCount(j.Recipients),
		})
		observedAt = time.Now().UTC()
		if rerr != nil {
			if isPermanentRampError(rerr) {
				if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.AcceptedAt, observedAt, "sending_ramp_invalid: "+rerr.Error(), delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionCancelled, nil); err != nil {
					return err
				}
				return river.JobCancel(rerr)
			}
			if j.pastRetryHorizon() {
				if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.AcceptedAt, observedAt, "ramp_capacity_timeout: "+rerr.Error(), delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionLocalRetriesExhausted, nil); err != nil {
					return err
				}
				_ = w.ramp.Release(ctx, j.MessageID)
				return river.JobCancel(fmt.Errorf("sending ramp unavailable past %s horizon: %w", sendRetryHorizon, rerr))
			}
			if err := w.store.ReleaseSend(ctx, j.MessageID, job.ID); err != nil {
				return fmt.Errorf("release outbound send claim after ramp-check failure: %w", err)
			}
			log.Printf("[outbound-send] ramp reservation failed for %s (snoozing): %v", j.MessageID, rerr)
			return river.JobSnooze(rampErrorSnoozeInterval)
		}
		if !decision.Allowed {
			if j.pastRetryHorizon() {
				if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.AcceptedAt, observedAt, "ramp_capacity_timeout", delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionLocalRetriesExhausted, nil); err != nil {
					return err
				}
				if err := w.ramp.Release(ctx, j.MessageID); err != nil {
					return fmt.Errorf("release ramp reservation after timeout: %w", err)
				}
				return river.JobCancel(fmt.Errorf("sending ramp deferred past %s horizon", sendRetryHorizon))
			}
			if err := w.store.ReleaseSend(ctx, j.MessageID, job.ID); err != nil {
				return fmt.Errorf("release outbound send claim after ramp deferral: %w", err)
			}
			delay := time.Until(decision.RetryAt)
			if delay < time.Minute {
				delay = time.Minute
			}
			return river.JobSnooze(delay)
		}
	}

	// Final suppression guard immediately before provider I/O: a suppression
	// added after acceptance or while an allowed ramp reservation was in flight
	// must still prevent delivery. A match is terminal; a store error fails
	// closed, releasing the side-effect-free claim while preserving an allowed
	// ramp reservation for the idempotent River retry.
	suppressed, serr := w.store.SuppressedRecipients(ctx, j.UserID, j.AgentID, j.Recipients)
	observedAt = time.Now().UTC()
	if serr != nil {
		if err := w.store.ReleaseSend(ctx, j.MessageID, job.ID); err != nil {
			// Keep the idempotent ramp reservation while the message claim remains
			// held. Releasing capacity first would let another message consume it,
			// then a retry could reserve the same message a second time.
			return fmt.Errorf("suppression check and claim cleanup before outbound send: %w",
				errors.Join(serr, fmt.Errorf("release outbound send claim: %w", err)))
		}
		return fmt.Errorf("suppression check before outbound send: %w", serr)
	}
	if len(suppressed) > 0 {
		supErr := fmt.Errorf("recipient_suppressed: %s%s", strings.Join(suppressed, ", "), outbound.SuppressionRemediation(j.AgentID))
		if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.AcceptedAt, observedAt, supErr.Error(), delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionCancelled, suppressed); err != nil {
			return err
		}
		if w.ramp != nil && j.rampEligible() {
			if err := w.ramp.Release(ctx, j.MessageID); err != nil {
				return fmt.Errorf("release ramp reservation after suppression: %w", err)
			}
		}
		return river.JobCancel(supErr)
	}

	deliverStart := time.Now()
	out := w.deliverer.Deliver(ctx, j)
	observedAt = time.Now().UTC()
	// Every Deliver call is exactly one submission attempt; classify it here
	// so no downstream branch (outage, horizon, deferral) can drop the sample.
	deliverSeconds := time.Since(deliverStart).Seconds()
	switch {
	case out.Err == nil:
		w.metrics.OutboundAttempt(attemptSuccess, deliverSeconds)
	case out.Permanent:
		w.metrics.OutboundAttempt(attemptPermanentFailure, deliverSeconds)
	default:
		w.metrics.OutboundAttempt(attemptTemporaryFailure, deliverSeconds)
	}
	if out.Err == nil {
		// Success — one tx (in the store): mark sent + provider id + email.sent.
		if err := w.store.MarkSent(ctx, j.MessageID, job.ID, job.Attempt, observedAt, out.ProviderMessageID, out.SentAs); err != nil {
			return err
		}
		// Emitted even when MarkSent was a no-op (the row was already
		// finalized sent by a racing SNS delivery notification): that path is
		// NOT instrumented, so this is still the message's ONLY sent count.
		// If FinalizeProviderAcceptedTx is ever given its own emission, this
		// site must become status-aware (like MarkFailed) or the race
		// double-counts. The latency observation shares this exactly-once
		// contract — emitTerminal emits count and latency together, here and
		// everywhere else, and the SNS-feedback path stays uninstrumented
		// for both.
		emitTerminal(w.metrics, terminalSent, j.AcceptedAt, observedAt)
		if w.ramp != nil && j.rampEligible() {
			if err := w.ramp.Confirm(ctx, j.MessageID); err != nil {
				return fmt.Errorf("confirm sending ramp: %w", err)
			}
		}
		return nil
	}

	// Permanent failure (validation / permanent 5xx) — terminal now, no retries.
	// Provenance 'provider': SES itself refused this submission, so the §3.1
	// correction never revives it.
	if out.Permanent {
		if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.AcceptedAt, observedAt, out.Err.Error(), delivery.FailureSourceProvider, messagelifecycle.ReasonSubmissionProviderRejected, nil); err != nil {
			return err
		}
		if w.ramp != nil && j.rampEligible() {
			if err := w.ramp.Release(ctx, j.MessageID); err != nil {
				return fmt.Errorf("release ramp reservation after provider rejection: %w", err)
			}
		}
		return river.JobCancel(out.Err)
	}
	// Provider outage (relay unreachable) — snooze WITHOUT burning an attempt so a
	// multi-hour SES incident defers instead of exhausting MaxSendAttempts and
	// mass-firing false email.failed (§8 circuit breaker). Bounded by the retry
	// horizon: once the accept is older than sendRetryHorizon, give up terminally
	// (provenance 'local': the provider never confirmed a rejection).
	if out.Outage {
		if j.pastRetryHorizon() {
			if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.AcceptedAt, observedAt, out.Err.Error(), delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionLocalRetriesExhausted, nil); err != nil {
				return err
			}
			if w.ramp != nil && j.rampEligible() {
				_ = w.ramp.Release(ctx, j.MessageID)
			}
			return fmt.Errorf("outbound send failed (provider outage past %s horizon): %w", sendRetryHorizon, out.Err)
		}
		if err := w.store.RecordTemporaryFailure(ctx, j.MessageID, job.ID, job.Attempt, observedAt, out.Err.Error()); err != nil {
			return fmt.Errorf("record outbound provider outage and release claim: %w", err)
		}
		return river.JobSnooze(outageSnoozeInterval)
	}
	// Last attempt — River discards after this. Do NOT declare failed inline:
	// this attempt's error can be ambiguous (the connection may have died after
	// SES accepted the DATA), and its Send/Delivery notification may still be in
	// flight. Record the diagnostic + release the claim; the terminal reconciler
	// declares the outcome after the provider-evidence grace window — evidence →
	// sent, none → failed + exactly one email.failed (deterministic event id).
	if job.Attempt >= MaxSendAttempts {
		if err := w.store.DeferTerminalFailure(ctx, j.MessageID, job.ID, job.Attempt, observedAt, out.Err.Error()); err != nil {
			log.Printf("[outbound-send] defer terminal failure for %s: %v", j.MessageID, err)
		}
		// Not counted as terminal: the reconciler declares the real outcome
		// (sent on evidence, failed otherwise) after the grace window and
		// emits it then — counting the deferral too would double-count the
		// message in e2a_outbound_terminal_total.
		return fmt.Errorf("outbound send failed (final attempt %d; outcome deferred to terminal reconciler): %w", job.Attempt, out.Err)
	}
	// Retryable — River reschedules per NextRetry.
	if err := w.store.RecordTemporaryFailure(ctx, j.MessageID, job.ID, job.Attempt, observedAt, out.Err.Error()); err != nil {
		return fmt.Errorf("record outbound temporary failure and release claim: %w", err)
	}
	return fmt.Errorf("outbound send attempt %d failed: %w", job.Attempt, out.Err)
}

func (j *SendJob) rampEligible() bool {
	return j.SentAs == "own_address" && j.MessageType != "test"
}

func isPermanentRampError(err error) bool {
	var permanent interface{ Permanent() bool }
	return errors.As(err, &permanent) && permanent.Permanent()
}

func uniqueRecipientCount(recipients []string) int {
	seen := make(map[string]struct{}, len(recipients))
	for _, recipient := range recipients {
		recipient = strings.ToLower(strings.TrimSpace(recipient))
		if recipient != "" {
			seen[recipient] = struct{}{}
		}
	}
	return len(seen)
}

// markFailed writes the terminal 'failed' status, retrying a transient DB error a
// few times so the row's status never desyncs from a discarded River job (mirrors
// webhookdelivery.markFailedReliably).
const (
	// terminalWriteRetries / terminalWriteBackoff bound the retry of the terminal
	// 'failed' write in markFailed — a best-effort last resort when the DB write of the
	// final send outcome itself fails. Backoff is linear (i+1)×base.
	terminalWriteRetries = 3
	terminalWriteBackoff = 150 * time.Millisecond
)

func (w *SendWorker) markFailed(ctx context.Context, messageID string, jobID int64, attempt int, acceptedAt, occurredAt time.Time, detail string, source delivery.FailureSource, reason messagelifecycle.ReasonCode, blockedRecipients []string) error {
	var err error
	for i := 0; i < terminalWriteRetries; i++ {
		var settled delivery.Status
		var settledAt time.Time
		if settled, settledAt, err = w.store.MarkFailed(ctx, messageID, jobID, attempt, occurredAt, detail, source, reason, blockedRecipients); err == nil {
			// Emit what the guarded write actually did, exactly once, only
			// after the durable write: a failure with the caller's provenance,
			// or "sent" when provider evidence settled the row. A no-op write
			// (row gone/already terminal) records nothing. The
			// PreserveTerminalFailure fallback below deliberately does NOT
			// emit — the reconciler declares (and counts) that row later.
			// emitTerminal uses the write's EFFECTIVE occurred_at (the
			// provider-accept evidence time on an evidence settle), so the
			// latency reports what the write did, not what the caller asked.
			switch settled {
			case delivery.StatusFailed:
				emitTerminal(w.metrics, terminalOutcome(source, reason, blockedRecipients), acceptedAt, settledAt)
			case delivery.StatusSent:
				emitTerminal(w.metrics, terminalSent, acceptedAt, settledAt)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * terminalWriteBackoff):
		}
	}
	log.Printf("[outbound-send] CRITICAL: terminal 'failed' write for %s failed after retries: %v", messageID, err)
	if fallbackErr := w.store.PreserveTerminalFailure(ctx, messageID, jobID, attempt, occurredAt, messagelifecycle.SafeDiagnostic(detail), source, reason, blockedRecipients); fallbackErr != nil {
		return fmt.Errorf("terminal write failed: %w; preserve terminal provenance: %v", err, fallbackErr)
	}
	return nil
}

// SubmissionDedupeKey is the stable message-local identity for one observed
// River submission attempt and reason.
func SubmissionDedupeKey(jobID int64, attempt int, reason messagelifecycle.ReasonCode) string {
	return "submission:job:" + strconv.FormatInt(jobID, 10) + ":attempt:" + strconv.Itoa(attempt) + ":" + string(reason)
}

func jobObservationTime(job *river.Job[OutboundSendArgs]) time.Time {
	if job != nil && job.JobRow != nil {
		if job.AttemptedAt != nil && !job.AttemptedAt.IsZero() {
			return job.AttemptedAt.UTC()
		}
		if !job.CreatedAt.IsZero() {
			return job.CreatedAt.UTC()
		}
	}
	return time.Now().UTC()
}
