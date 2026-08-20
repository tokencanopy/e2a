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
	"hash/fnv"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendrate"
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

// rateErrorSnoozeInterval keeps a durable message queued when the fire-time
// rate store is temporarily unavailable — mirroring rampErrorSnoozeInterval:
// fail toward retry, never toward an unthrottled submit.
const rateErrorSnoozeInterval = time.Minute

// rateMinSnooze floors a rate deferral so a RetryAt at (or just past) now —
// the window-boundary race — cannot hot-loop the queue.
const rateMinSnooze = 250 * time.Millisecond

// rateMaxSnooze caps a rate deferral's elapsed-time backoff (#771). The
// exponential doubling already turns the quadratic drain cost into a logarithmic
// one; the cap only bounds the deep tail (messages that keep losing the slot
// race for minutes). 3m — 3× the 1m production window — keeps essentially all of
// the doubling's wake-up savings while bounding worst-case re-check latency and
// the fairness skew below. Tunable: raise it to trade lower wake-up volume for
// higher tail latency. Well under sendRetryHorizon either way.
const rateMaxSnooze = 3 * time.Minute

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
	// ScheduledAt is messages.scheduled_at for a scheduled send (zero for an
	// immediate one). The retry horizon is measured from max(AcceptedAt,
	// ScheduledAt): a send scheduled far past accept still gets the full
	// outage-tolerant tail from its fire time, instead of a horizon already blown
	// the moment it first runs.
	ScheduledAt time.Time
	// ReviewedAt is messages.reviewed_at — when a HITL hold was resolved into the
	// send pipeline (human approve or TTL auto-approve), zero for a message that
	// was never held. Consumed ONLY by submissionAnchor for the latency SLI; the
	// retry horizon deliberately still measures from AcceptedAt, so the F2
	// limitation in docs/design/hitl-ttl-async-send.md is unchanged by this field.
	ReviewedAt time.Time
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

// fireTime is when the message became eligible to submit: max(accept, scheduled).
// A scheduled send's clocks (retry horizon, deferral backoff) start when it
// fires, not when it was accepted. Zero when both timestamps are unknown.
func (j *SendJob) fireTime() time.Time {
	t := j.AcceptedAt
	if j.ScheduledAt.After(t) {
		t = j.ScheduledAt
	}
	return t
}

// pastRetryHorizon reports whether the accept is older than the outage-tolerant
// retry horizon. Zero AcceptedAt (unknown) is treated as not-past so an outage keeps
// deferring rather than being falsely terminated on a missing timestamp.
func (j *SendJob) pastRetryHorizon() bool {
	// Measure from the fire time (max of accept, scheduled): a scheduled send's
	// outage tail starts when it fires, not when it was accepted, so a >72h-out
	// schedule isn't terminally failed on its very first attempt.
	start := j.fireTime()
	return !start.IsZero() && time.Since(start) > sendRetryHorizon
}

// submissionAnchor is this job's acceptance→terminal SLI baseline — see the
// package helper. Held and scheduled sends anchor at the moment they became
// eligible to submit, not at accept, so a reviewer's dwell time and a
// deliberate schedule delay stay out of e2a's error budget.
func (j *SendJob) submissionAnchor() time.Time {
	return submissionAnchor(j.AcceptedAt, j.ScheduledAt, j.ReviewedAt)
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

// RateDecision is the fire-time rate gate's answer for one submission slot:
// Allowed=false carries RetryAt, the earliest the agent's window frees
// capacity. Aliased to the storage type so a *sendrate.Store satisfies
// RateGate directly — no adapter.
type RateDecision = sendrate.Decision

// RateGate reserves one slot in the per-agent fire-time submission budget
// (internal/sendrate) — the durable counterpart to the acceptance-time
// in-memory send limit, enforced immediately before provider submission so
// scheduled-send bursts and multi-replica deployments cannot exceed it.
// Unlike RampGate there is no Confirm/Release: the slot is consumed at
// Reserve and ages out of the sliding window on its own (see the sendrate
// package doc for the crash semantics). A nil gate allows everything.
// Window exposes the gate's sliding window so the deferral snooze clamp
// cannot diverge from the limiter's real window.
type RateGate interface {
	Reserve(ctx context.Context, agentID string) (RateDecision, error)
	Window() time.Duration
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
	rate      RateGate
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

// WithRateGate injects the fire-time per-agent rate gate (internal/sendrate).
// Chainable; nil keeps the allow-all default (no gate wired).
func (w *SendWorker) WithRateGate(g RateGate) *SendWorker {
	if g != nil {
		w.rate = g
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
		emitTerminal(w.metrics, terminalSent, j.submissionAnchor(), observedAt)
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
				if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, "sending_ramp_invalid: "+rerr.Error(), delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionCancelled, nil); err != nil {
					return err
				}
				return river.JobCancel(rerr)
			}
			if j.pastRetryHorizon() {
				if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, "ramp_capacity_timeout: "+rerr.Error(), delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionLocalRetriesExhausted, nil); err != nil {
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
				if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, "ramp_capacity_timeout", delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionLocalRetriesExhausted, nil); err != nil {
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

	// Fire-time per-agent rate gate (internal/sendrate): the durable,
	// cross-replica counterpart of the acceptance-time in-memory send limit —
	// scheduled sends accumulate as River jobs and would otherwise burst past
	// the advertised 60/min/agent at the provider when they fire. Grouped with
	// the other wait-gates: after the ramp reservation, before the final
	// suppression check. A deferral RELEASES the send claim but KEEPS the ramp
	// reservation (same invariant as the outage snooze above — same-message
	// Reserve is idempotent, a released reservation is terminal), and snoozes
	// WITHOUT burning an attempt, metering, or emitting lifecycle/terminal
	// events: the message simply fires when the window frees capacity.
	if w.rate != nil {
		decision, rerr := w.rate.Reserve(ctx, j.AgentID)
		observedAt = time.Now().UTC()
		if rerr != nil {
			// Fail toward retry, never toward an unthrottled submit: the
			// provider is never exposed because the limiter is down.
			if j.pastRetryHorizon() {
				if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, "send_rate_timeout: "+rerr.Error(), delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionLocalRetriesExhausted, nil); err != nil {
					return err
				}
				if w.ramp != nil && j.rampEligible() {
					_ = w.ramp.Release(ctx, j.MessageID)
				}
				return river.JobCancel(fmt.Errorf("send rate gate unavailable past %s horizon: %w", sendRetryHorizon, rerr))
			}
			if err := w.store.ReleaseSend(ctx, j.MessageID, job.ID); err != nil {
				return fmt.Errorf("release outbound send claim after rate-gate failure: %w", err)
			}
			log.Printf("[outbound-send] rate gate unavailable for %s (snoozing): %v", j.MessageID, rerr)
			return river.JobSnooze(rateErrorSnoozeInterval)
		}
		if !decision.Allowed {
			if j.pastRetryHorizon() {
				if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, "send_rate_timeout", delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionLocalRetriesExhausted, nil); err != nil {
					return err
				}
				if w.ramp != nil && j.rampEligible() {
					if err := w.ramp.Release(ctx, j.MessageID); err != nil {
						return fmt.Errorf("release ramp reservation after send-rate timeout: %w", err)
					}
				}
				return river.JobCancel(fmt.Errorf("send rate deferred past %s horizon", sendRetryHorizon))
			}
			if err := w.store.ReleaseSend(ctx, j.MessageID, job.ID); err != nil {
				return fmt.Errorf("release outbound send claim after rate deferral: %w", err)
			}
			delay := rateSnooze(j.fireTime(), time.Until(decision.RetryAt), w.rate.Window())
			// Jitter scales with the snooze it decorates (not the fixed window) so
			// a capped, minutes-long backlog still fans out proportionally instead
			// of re-herding in a 15s band. Adds up to a quarter on top of delay.
			delay += rateJitter(j.MessageID, delay)
			w.metrics.OutboundRateDeferred()
			// IDs only — never recipient data.
			log.Printf("[outbound-send] rate_limited agent=%s msg=%s retry_in=%s", j.AgentID, j.MessageID, delay)
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
		if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, supErr.Error(), delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionCancelled, suppressed); err != nil {
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
		emitTerminal(w.metrics, terminalSent, j.submissionAnchor(), observedAt)
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
		if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, out.Err.Error(), delivery.FailureSourceProvider, messagelifecycle.ReasonSubmissionProviderRejected, nil); err != nil {
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
			if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, out.Err.Error(), delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionLocalRetriesExhausted, nil); err != nil {
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

// clampRateSnooze bounds a rate deferral to [rateMinSnooze, window]: the floor
// avoids a hot loop when RetryAt lands on (or just past) now — the
// window-boundary race — and the cap guarantees a deferred job re-fires within
// ~1 window even if the decision's RetryAt is skewed. In the normal path
// RetryAt = oldest-kept-event + window ≤ now + window already; the cap is a
// backstop, not the common case.
func clampRateSnooze(d, window time.Duration) time.Duration {
	if d < rateMinSnooze {
		return rateMinSnooze
	}
	if window > 0 && d > window {
		return window
	}
	return d
}

// rateSnooze picks how long a rate-deferred job sleeps before re-driving.
//
// The base wait is the time until the agent's window frees a slot
// (clampRateSnooze: floored off the boundary race, capped at one window). On top
// of that, the deferral backs off by how long the message has ALREADY been
// waiting to fire: sleeping at least "as long as we've already waited" (elapsed
// since fireTime) doubles the total wait on each pass, so a deep backlog
// re-drives a logarithmic number of times instead of re-waking every window —
// the quadratic drain cost in #771 (each re-drive is 3 txs / 2 messages
// UPDATEs and occupies a send worker, adding latency to other tenants' sends).
// Bounded by rateMaxSnooze so the job still drains well under the 72h retry
// horizon.
//
// Trade-off: because a longer-waiting message sleeps longer, a draining backlog
// skews toward newer-first (LIFO-ish) — an old message can cede a freed slot to
// a fresher one. This is inherent to any elapsed/attempt-scaled backoff and is
// bounded: the rateMaxSnooze cap keeps an old message re-driving on a fixed
// cadence (hundreds of retries before the 72h horizon), and pastRetryHorizon —
// which runs BEFORE this in the deferral branch — still terminates a genuinely
// stuck job rather than letting it churn forever.
//
// A zero fireTime (unknown timestamps) disables the backoff and falls back to
// the base wait, mirroring pastRetryHorizon's zero-handling.
func rateSnooze(fireTime time.Time, retryDelay, window time.Duration) time.Duration {
	delay := clampRateSnooze(retryDelay, window)
	if !fireTime.IsZero() {
		if elapsed := time.Since(fireTime); elapsed > delay {
			delay = elapsed
		}
	}
	if delay > rateMaxSnooze {
		delay = rateMaxSnooze
	}
	return delay
}

// rateJitter spreads a deferred backlog's re-fire across a quarter of the
// snooze it decorates. Every job deferred by the same burst gets a
// near-identical snooze (their blocking events were stamped ~simultaneously);
// without jitter the whole backlog re-wakes in lockstep and serializes on the
// agent's one hot rate row — a self-inflicted thundering herd of
// claim/reserve/release txs, log lines, and metric increments. Scaling the
// spread to the actual snooze (not the fixed window) keeps the fan-out
// proportional even when rateSnooze's elapsed backoff has grown the snooze to
// minutes — otherwise a capped backlog re-herds in a window-sized band.
// Deterministic per message (FNV over the message id) so there is no RNG state
// and a given message's spread is stable across workers and replicas.
func rateJitter(messageID string, spread time.Duration) time.Duration {
	maxJitter := spread / 4
	if maxJitter <= 0 {
		return 0
	}
	// Sub-millisecond spreads (tiny test windows) truncate to 0ms — guard the
	// modulo against the divide-by-zero, not just the non-positive spread.
	ms := maxJitter.Milliseconds()
	if ms <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(messageID))
	return time.Duration(h.Sum32()%uint32(ms)) * time.Millisecond
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

func (w *SendWorker) markFailed(ctx context.Context, messageID string, jobID int64, attempt int, anchorAt, occurredAt time.Time, detail string, source delivery.FailureSource, reason messagelifecycle.ReasonCode, blockedRecipients []string) error {
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
				emitTerminal(w.metrics, terminalOutcome(source, reason, blockedRecipients), anchorAt, settledAt)
			case delivery.StatusSent:
				emitTerminal(w.metrics, terminalSent, anchorAt, settledAt)
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
