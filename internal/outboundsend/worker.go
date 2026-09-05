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
// Every provider call passes through the sending-protection Gate
// (internal/sendingpolicy). The worker order is fixed: Reserve the durable
// attempt; on a hold, snooze without provider I/O; on a rate deferral,
// DeferAttempt; on a final suppression match, CancelAttempt; ConsumeAttempt is
// the last serialized decision; the authorized submitter redeems the token
// immediately before the socket opens and settles the provider's answer. A
// later execution after a confirmed attempt returns to Reserve, which
// allocates the next ordinal — the worker never chooses one.
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
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
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

// gateErrorSnoozeInterval keeps a durable message queued when the sending
// protection gate is temporarily unavailable. JobSnooze does not consume a
// River attempt: fail toward retry, never toward an unauthorized submit.
const gateErrorSnoozeInterval = time.Minute

// rateErrorSnoozeInterval keeps a durable message queued when the fire-time
// rate store is temporarily unavailable — fail toward retry, never toward an
// unthrottled submit.
const rateErrorSnoozeInterval = time.Minute

// rateMinSnooze floors a rate deferral so a RetryAt at (or just past) now —
// the window-boundary race — cannot hot-loop the queue.
const rateMinSnooze = 250 * time.Millisecond

// indefiniteHoldSnooze paces a hold that has no clock of its own — an account
// pause waits for an operator, not for midnight.
const indefiniteHoldSnooze = time.Hour

// SendRetryHorizon bounds the outage-tolerant tail: past this age a message in a
// rate/ramp/provider or tenant-setup hold is declared terminally failed. 72h
// matches the industry MTA retry horizon (and the webhook deliverer's envelope)
// — long enough to ride out a multi-hour regional SES incident, not forever.
const SendRetryHorizon = 72 * time.Hour

// PolicyBudgetHoldHorizon bounds a sending-budget hold: a message may wait
// through several UTC days for capacity, but not forever. Seven days is the
// policy's budget_hold_max_days default; the worker holds it as a constant
// because the deadline is derived, never stored, and every execution must
// derive the same one.
const PolicyBudgetHoldHorizon = 7 * 24 * time.Hour

// HoldClass is the durable finite-hold classification persisted on the message
// the first time it waits for something with a clock.
type HoldClass string

const (
	// HoldRateRampOrProvider: per-agent rate, custom-domain ramp, or provider
	// outage. 72-hour deadline; expiry reason submission.local_retries_exhausted.
	HoldRateRampOrProvider HoldClass = "rate_ramp_or_provider"
	// HoldTenantSetup: the account's SES tenant is not ready. 72-hour deadline;
	// expiry reason submission.sending_setup_expired. Transitions exactly once
	// to HoldRateRampOrProvider when readiness lands before the setup deadline.
	HoldTenantSetup HoldClass = "tenant_setup"
	// HoldPolicyBudget: a sending-budget pool is exhausted. Seven-day deadline
	// from the existing anchor; every finite class promotes to it and nothing
	// moves it afterwards. Expiry reason submission.policy_budget_expired.
	HoldPolicyBudget HoldClass = "policy_budget"
)

// horizon is the class's absolute deadline measured from its anchor.
func (c HoldClass) horizon() time.Duration {
	if c == HoldPolicyBudget {
		return PolicyBudgetHoldHorizon
	}
	return SendRetryHorizon
}

// expiryReason is the lifecycle reason a class emits when its deadline passes.
func (c HoldClass) expiryReason() messagelifecycle.ReasonCode {
	switch c {
	case HoldPolicyBudget:
		return messagelifecycle.ReasonSubmissionPolicyBudgetExpired
	case HoldTenantSetup:
		return messagelifecycle.ReasonSubmissionSendingSetupExpired
	}
	return messagelifecycle.ReasonSubmissionLocalRetriesExhausted
}

// ErrSendingPaused is returned by the enqueue entry points when the owning
// account is paused: the acceptance surface must reject the request rather
// than queue mail that can never leave.
var ErrSendingPaused = errors.New("outboundsend: account sending is paused")

// OutboundSendArgs drives one outbound send. Args carry the message id and the
// durable operation reference the accept transaction prepared; the worker
// re-reads the messages row (the source of truth) each attempt. A job enqueued
// before the reference existed (a pre-floor slot) carries none and is resolved
// at fire time through the same Prepare path.
type OutboundSendArgs struct {
	MessageID    string                      `json:"message_id"`
	OperationRef *sendingpolicy.OperationRef `json:"operation_ref,omitempty"`
}

func (OutboundSendArgs) Kind() string { return "outbound_send" }

// SendJob is the send payload the worker loads from the messages row (Store.ClaimSend).
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
	// AcceptedAt is messages.created_at.
	AcceptedAt time.Time
	// ScheduledAt is messages.scheduled_at for a scheduled send (zero for an
	// immediate one).
	ScheduledAt time.Time
	// ReviewedAt is messages.reviewed_at — when a HITL hold was resolved into the
	// send pipeline, zero for a message that was never held.
	ReviewedAt time.Time
	// ProviderAccepted is set when authoritatively correlated provider-accept
	// evidence has been recorded for this message: the provider already has it,
	// so the worker settles the row as sent instead of re-submitting a duplicate.
	ProviderAccepted   bool
	ProviderAcceptedAt *time.Time
	// ProviderMessageID is the evidence-repaired provider id accompanying
	// ProviderAccepted ('' when no evidence).
	ProviderMessageID string
	// LocalHoldClass / LocalHoldAnchor are the durable finite-hold pair a
	// previous execution persisted (empty/zero when never held). The deadline
	// is derived from them on every execution and never stored.
	LocalHoldClass  HoldClass
	LocalHoldAnchor time.Time
	// LastResumedAt is the owning account's last pause→active transition; a
	// first finite hold anchors no earlier than it, so a pause that preceded
	// the hold does not consume its horizon. Zero when unknown.
	LastResumedAt time.Time
	// TenantReadyAt is when the account's SES tenant became ready (zero until
	// it is). Drives the one-way tenant_setup → rate_ramp_or_provider move.
	TenantReadyAt time.Time
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

// initialHoldAnchor is where a message's first finite hold starts its clock:
// the latest of accept, schedule, review, and the account's last resume, so
// time spent in review or under an earlier pause is not charged to the hold.
func (j *SendJob) initialHoldAnchor() time.Time {
	anchor := j.AcceptedAt
	for _, t := range []time.Time{j.ScheduledAt, j.ReviewedAt, j.LastResumedAt} {
		if t.After(anchor) {
			anchor = t
		}
	}
	return anchor
}

// DeliverOutcome is the result of one authorized provider submission.
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
	// AcceptanceUnknown marks a failure AFTER the whole body was handed to the
	// provider (the 250 never came): the provider may hold the message. Never
	// permanent; the next attempt is a new ordinal, and provider feedback
	// carrying the attempt header is the only authoritative answer.
	AcceptanceUnknown bool
	// SettlementErr reports that the provider ACCEPTED the message but the
	// local settlement did not commit. The send happened; the caller must not
	// resubmit.
	SettlementErr error
}

// Deliverer performs a SINGLE authorized SMTP submit — River owns re-attempts.
// The token is the authorization for exactly this call; the production
// implementation (the outbound.ProviderSubmitter) redeems it immediately before
// the socket opens and refuses to dial without it.
type Deliverer interface {
	Deliver(ctx context.Context, j *SendJob, auth sendingpolicy.ProviderAuthorization) DeliverOutcome
}

// RateDecision is the fire-time rate gate's answer for one submission slot:
// Allowed=false carries RetryAt, the earliest the agent's window frees
// capacity. Aliased to the storage type so a *sendrate.Store satisfies
// RateGate directly — no adapter.
type RateDecision = sendrate.Decision

// RateGate reserves one slot in the per-agent fire-time submission budget
// (internal/sendrate) — the durable counterpart to the acceptance-time
// in-memory send limit, enforced immediately before provider submission so
// scheduled-send bursts and multi-replica deployments cannot exceed it. It
// stays separate from the sending-protection gate because it controls provider
// throughput, not reputation admission. A nil gate allows everything.
type RateGate interface {
	Reserve(ctx context.Context, agentID string) (RateDecision, error)
	Window() time.Duration
}

// OperationResolver recovers the durable operation for a job that carries no
// reference — a legacy argument shape from a pre-floor slot. It runs the same
// Prepare path an accept transaction runs, idempotently, so an old job and a
// new one authorize identically.
type OperationResolver func(ctx context.Context, messageID string) (sendingpolicy.AcceptanceDecision, sendingpolicy.OperationRef, error)

// DailyQuotaDeferredError is returned by Store.ClaimSend when the owning
// account's per-day send cap is exhausted at fire time. The store has already
// released the send claim; the worker snoozes the job until RetryAt (the next
// UTC midnight, when the daily window resets) instead of failing the message.
type DailyQuotaDeferredError struct {
	RetryAt time.Time
}

func (e *DailyQuotaDeferredError) Error() string {
	return fmt.Sprintf("daily send cap exhausted; deferred until %s", e.RetryAt.Format(time.RFC3339))
}

// Store is the messages-store surface the worker needs. Implemented over
// internal/identity in the binary. ClaimSend atomically checks that the message
// and agent are live and persists delivery_status='sending' for the stamped River
// job before provider I/O begins.
type Store interface {
	// ClaimSend returns nil when the message is gone, trashed, terminal, or owned
	// by a different River job. It returns *DailyQuotaDeferredError (claim
	// released) when the account's daily send cap is exhausted at fire time.
	// (agent-delete cascade / TTL) — the worker treats a nil job as a no-op.
	ClaimSend(ctx context.Context, messageID string, jobID int64) (*SendJob, error)
	// ReleaseSend clears a side-effect-free attempt before River backoff.
	ReleaseSend(ctx context.Context, messageID string, jobID int64) error
	// RecordHold persists the message's finite-hold class and anchor. Terminal
	// writes clear the pair.
	RecordHold(ctx context.Context, messageID string, class HoldClass, anchor time.Time) error
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
	// time is the occurred_at the write actually used.
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
	gate      sendingpolicy.Gate
	resolve   OperationResolver
	rate      RateGate
	metrics   Metrics
	now       func() time.Time
}

// NewSendWorker builds a worker with no sending-protection gate. Without a
// gate every provider call is made with an empty authorization, which the
// production submitter refuses before it dials; the composition root always
// installs one via WithGate, and its wiring test proves it.
func NewSendWorker(store Store, deliverer Deliverer) *SendWorker {
	return &SendWorker{store: store, deliverer: deliverer, metrics: noopMetrics{}, now: time.Now}
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

// WithGate injects the sending-protection gate every provider call must pass.
// Chainable; nil keeps the gateless default described on NewSendWorker.
func (w *SendWorker) WithGate(g sendingpolicy.Gate) *SendWorker {
	if g != nil {
		w.gate = g
	}
	return w
}

// WithOperationResolver injects the legacy-argument resolver. Chainable; nil
// leaves a legacy job failing closed.
func (w *SendWorker) WithOperationResolver(r OperationResolver) *SendWorker {
	if r != nil {
		w.resolve = r
	}
	return w
}

// WithClock overrides the worker's clock for deadline tests. Chainable.
func (w *SendWorker) WithClock(now func() time.Time) *SendWorker {
	if now != nil {
		w.now = now
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
	// Queue-wait SLI: due→pickup latency for THIS attempt. scheduled_at — NOT
	// created_at — is the baseline: a retried/snoozed/deferred message would
	// otherwise record its entire cumulative age as "queue wait" on every
	// pass, poisoning the p95. Guarded against zero/negative deltas.
	if job.AttemptedAt != nil && !job.ScheduledAt.IsZero() {
		if wait := job.AttemptedAt.Sub(job.ScheduledAt); wait > 0 {
			w.metrics.OutboundQueueWait(wait.Seconds())
		}
	}
	observedAt := jobObservationTime(job)
	j, err := w.store.ClaimSend(ctx, job.Args.MessageID, job.ID)
	if err != nil {
		// Daily-cap deferral: the store already released the claim; park the
		// job until the messages_day window resets (next UTC midnight). Not a
		// failure — the send fires unchanged tomorrow.
		var dqd *DailyQuotaDeferredError
		if errors.As(err, &dqd) {
			delay := time.Until(dqd.RetryAt)
			if delay < time.Minute {
				delay = time.Minute
			}
			return river.JobSnooze(delay)
		}
		return err // DB error — retryable
	}
	if j == nil {
		return nil // message gone or already terminal — nothing to provider-submit
	}
	if j.alreadyDone() {
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
		// earlier attempt; only the settle lands here.
		emitTerminal(w.metrics, terminalSent, j.submissionAnchor(), observedAt)
		w.settleFromEvidence(ctx, job, j.ProviderMessageID)
		return nil
	}

	// A message whose SES tenant became ready in time leaves the setup class
	// before any later gate is consulted, so the setup deadline it has already
	// escaped cannot fail it and the new 72-hour horizon starts at readiness.
	if err := w.applyTenantReadiness(ctx, j); err != nil {
		return err
	}

	// Without a gate (unit tests only) the gate steps are skipped and every
	// other guard still runs; the production deliverer refuses the empty
	// authorization that results, so a deployment that reaches the provider
	// this way sends nothing. The composition root's wiring test proves
	// production never builds this shape.
	var attempt sendingpolicy.AttemptRef
	if w.gate != nil {
		ref, holdErr := w.operationFor(ctx, job, j)
		if holdErr != nil {
			return holdErr
		}
		// 1. Reserve the durable attempt. Reserve is idempotent per ordinal,
		//    so a re-driven execution that never reached ConsumeAttempt finds
		//    its own reservation, and a confirmed one is followed by a fresh
		//    ordinal.
		early, reserved, err := w.gate.Reserve(ctx, ref)
		observedAt = w.now().UTC()
		if err != nil {
			if errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
				return w.cancelTerminally(ctx, job, j, reserved, observedAt, "sending_policy: operation unavailable: "+err.Error())
			}
			return w.snoozeOnGateError(ctx, job, j, "reserve", err)
		}
		if !early.Allow {
			return w.hold(ctx, job, j, reserved, early, observedAt)
		}
		attempt = reserved
	}

	// 2. Fire-time per-agent rate gate: a deferral DeferAttempts (the budget
	//    is given back; the ramp reservation is kept) and snoozes WITHOUT
	//    burning an attempt, metering, or emitting lifecycle/terminal events.
	if w.rate != nil {
		decision, rerr := w.rate.Reserve(ctx, j.AgentID)
		observedAt = w.now().UTC()
		if rerr != nil || !decision.Allowed {
			w.deferAttempt(ctx, attempt, "rate")
			if rerr != nil {
				log.Printf("[outbound-send] rate gate unavailable for %s (snoozing): %v", j.MessageID, rerr)
				return w.holdFinite(ctx, job, j, attempt, HoldRateRampOrProvider, "send_rate_timeout: "+rerr.Error(), rateErrorSnoozeInterval, observedAt)
			}
			delay := clampRateSnooze(time.Until(decision.RetryAt), w.rate.Window()) + rateJitter(j.MessageID, w.rate.Window())
			if !w.holdExpired(j, HoldRateRampOrProvider, observedAt) {
				// A deferral is counted only when it defers; an expiry is a
				// terminal outcome and is counted as one by markFailed.
				w.metrics.OutboundRateDeferred()
				// IDs only — never recipient data.
				log.Printf("[outbound-send] rate_limited agent=%s msg=%s retry_in=%s", j.AgentID, j.MessageID, delay)
			}
			return w.holdFinite(ctx, job, j, attempt, HoldRateRampOrProvider, "send_rate_timeout", delay, observedAt)
		}
	}

	// 3. Final suppression guard immediately before authorization: a
	//    suppression added after acceptance must still prevent delivery. A
	//    match is terminal and cancels the attempt (both ledgers); a store
	//    error fails closed, releasing the side-effect-free claim.
	suppressed, serr := w.store.SuppressedRecipients(ctx, j.UserID, j.AgentID, j.Recipients)
	observedAt = w.now().UTC()
	if serr != nil {
		if err := w.store.ReleaseSend(ctx, j.MessageID, job.ID); err != nil {
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
		w.cancelAttempt(ctx, attempt, "suppression")
		return river.JobCancel(supErr)
	}

	if w.gate == nil {
		return w.submit(ctx, job, j, sendingpolicy.ProviderAuthorization{}, observedAt)
	}

	// 4. Final authorization. ConsumeAttempt re-checks account state, tenant
	//    readiness, both ledgers, and the post-lock UTC day under lock; a hold
	//    here is handled exactly like an early one, and an error leaves the
	//    reservation standing for the idempotent retry.
	decision, auth, err := w.gate.ConsumeAttempt(ctx, attempt)
	observedAt = w.now().UTC()
	if err != nil {
		if errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
			return w.cancelTerminally(ctx, job, j, attempt, observedAt, "sending_policy: operation unavailable: "+err.Error())
		}
		return w.snoozeOnGateError(ctx, job, j, "authorize", err)
	}
	if !decision.Allow || auth == nil {
		return w.hold(ctx, job, j, attempt, decision, observedAt)
	}

	// 5-6. The authorized submitter redeems the token immediately before the
	//      socket opens and settles the provider's answer.
	return w.submit(ctx, job, j, *auth, observedAt)
}

// submit makes the single authorized provider call and records its outcome.
func (w *SendWorker) submit(ctx context.Context, job *river.Job[OutboundSendArgs], j *SendJob, auth sendingpolicy.ProviderAuthorization, observedAt time.Time) error {
	deliverStart := time.Now()
	out := w.deliverer.Deliver(ctx, j, auth)
	observedAt = w.now().UTC()
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
		emitTerminal(w.metrics, terminalSent, j.submissionAnchor(), observedAt)
		if out.SettlementErr != nil {
			// The provider has the message; only the local settlement (ramp
			// progress, provider-id binding) is behind. Never a resend. The
			// delayed feedback path settles the same attempt idempotently.
			log.Printf("[outbound-send] WARNING: %s accepted by provider but not settled: %v", j.MessageID, out.SettlementErr)
		}
		return nil
	}

	// Permanent failure (validation / permanent 5xx) — terminal now, no retries.
	// Provenance 'provider': SES itself refused this submission, so the §3.1
	// correction never revives it. The submitter has already settled it.
	if out.Permanent {
		if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, out.Err.Error(), delivery.FailureSourceProvider, messagelifecycle.ReasonSubmissionProviderRejected, nil); err != nil {
			return err
		}
		return river.JobCancel(out.Err)
	}
	// Provider outage (relay unreachable) — snooze WITHOUT burning an attempt so a
	// multi-hour SES incident defers instead of exhausting MaxSendAttempts and
	// mass-firing false email.failed (§8 circuit breaker). Bounded by the hold
	// deadline: a message under a policy_budget hold keeps its seven-day
	// clock; any other message gets the 72-hour provider horizon.
	if out.Outage {
		class, anchor, changed := w.nextHoldState(j, HoldRateRampOrProvider, observedAt)
		if changed {
			if err := w.store.RecordHold(ctx, j.MessageID, class, anchor); err != nil {
				return fmt.Errorf("record outbound hold: %w", err)
			}
		}
		if !observedAt.Before(anchor.Add(class.horizon())) {
			if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, out.Err.Error(), delivery.FailureSourceLocal, class.expiryReason(), nil); err != nil {
				return err
			}
			return fmt.Errorf("outbound send failed (provider outage past %s horizon): %w", class.horizon(), out.Err)
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
		return fmt.Errorf("outbound send failed (final attempt %d; outcome deferred to terminal reconciler): %w", job.Attempt, out.Err)
	}
	// Retryable — River reschedules per NextRetry. The next execution returns
	// to Reserve, which allocates the next ordinal; an acceptance-unknown
	// failure takes the same path because only provider feedback can say
	// whether the body was kept.
	if err := w.store.RecordTemporaryFailure(ctx, j.MessageID, job.ID, job.Attempt, observedAt, out.Err.Error()); err != nil {
		return fmt.Errorf("record outbound temporary failure and release claim: %w", err)
	}
	return fmt.Errorf("outbound send attempt %d failed: %w", job.Attempt, out.Err)
}

// operationFor returns the job's durable operation, resolving a legacy job
// through the accept path. It returns a River verdict (snooze/cancel) as its
// error when the message cannot proceed.
func (w *SendWorker) operationFor(ctx context.Context, job *river.Job[OutboundSendArgs], j *SendJob) (sendingpolicy.OperationRef, error) {
	if job.Args.OperationRef != nil && !job.Args.OperationRef.IsZero() {
		return *job.Args.OperationRef, nil
	}
	observedAt := w.now().UTC()
	if w.resolve == nil {
		return sendingpolicy.OperationRef{}, w.cancelTerminally(ctx, job, j, sendingpolicy.AttemptRef{}, observedAt, "sending_policy: legacy job carries no operation and no resolver is wired")
	}
	decision, ref, err := w.resolve(ctx, j.MessageID)
	if err != nil {
		if errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
			return sendingpolicy.OperationRef{}, w.cancelTerminally(ctx, job, j, sendingpolicy.AttemptRef{}, observedAt, "sending_policy: legacy source unavailable: "+err.Error())
		}
		return sendingpolicy.OperationRef{}, w.snoozeOnGateError(ctx, job, j, "resolve", err)
	}
	if decision == sendingpolicy.AcceptanceSendingPaused {
		return sendingpolicy.OperationRef{}, w.hold(ctx, job, j, sendingpolicy.AttemptRef{}, sendingpolicy.Decision{Reason: sendingpolicy.ReasonAccountPaused}, observedAt)
	}
	if ref.IsZero() {
		// The only accepted shape with no operation is an exact self-send,
		// which never enqueues. A queued message that resolves to nothing is
		// not something this worker can authorize.
		return sendingpolicy.OperationRef{}, w.cancelTerminally(ctx, job, j, sendingpolicy.AttemptRef{}, observedAt, "sending_policy: message has no provider operation")
	}
	return ref, nil
}

// hold handles a gate hold: a terminal one fails the message now; a pause
// waits for an operator; every other one is a finite hold with a clock.
func (w *SendWorker) hold(ctx context.Context, job *river.Job[OutboundSendArgs], j *SendJob, attempt sendingpolicy.AttemptRef, d sendingpolicy.Decision, observedAt time.Time) error {
	if d.Terminal {
		return w.cancelTerminally(ctx, job, j, attempt, observedAt, "sending_policy: "+d.Reason)
	}
	class := holdClassFor(d.Reason)
	delay := indefiniteHoldSnooze
	if !d.RetryAt.IsZero() {
		delay = time.Until(d.RetryAt)
		if delay < time.Minute {
			delay = time.Minute
		}
	}
	if class == "" {
		// An account pause has no clock of its own. It does not start a finite
		// hold, but a deadline already running keeps running.
		if j.LocalHoldClass == "" {
			if err := w.store.ReleaseSend(ctx, j.MessageID, job.ID); err != nil {
				return fmt.Errorf("release outbound send claim during account pause: %w", err)
			}
			return river.JobSnooze(delay)
		}
		class = j.LocalHoldClass
	}
	return w.holdFinite(ctx, job, j, attempt, class, "sending_policy_hold: "+d.Reason, delay, observedAt)
}

// holdFinite persists the hold state, expires the message when its derived
// deadline has passed, and otherwise releases the claim and snoozes.
func (w *SendWorker) holdFinite(ctx context.Context, job *river.Job[OutboundSendArgs], j *SendJob, attempt sendingpolicy.AttemptRef, requested HoldClass, detail string, delay time.Duration, observedAt time.Time) error {
	class, anchor, changed := w.nextHoldState(j, requested, observedAt)
	if changed {
		if err := w.store.RecordHold(ctx, j.MessageID, class, anchor); err != nil {
			return fmt.Errorf("record outbound hold: %w", err)
		}
		j.LocalHoldClass, j.LocalHoldAnchor = class, anchor
	}
	if !observedAt.Before(anchor.Add(class.horizon())) {
		if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, detail, delivery.FailureSourceLocal, class.expiryReason(), nil); err != nil {
			return err
		}
		w.cancelAttempt(ctx, attempt, "hold expiry")
		return river.JobCancel(fmt.Errorf("%s: %s hold expired after %s", detail, class, class.horizon()))
	}
	if err := w.store.ReleaseSend(ctx, j.MessageID, job.ID); err != nil {
		return fmt.Errorf("release outbound send claim during hold: %w", err)
	}
	return river.JobSnooze(delay)
}

// holdExpired reports whether encountering `requested` now would find the
// message past its derived deadline, without persisting anything.
func (w *SendWorker) holdExpired(j *SendJob, requested HoldClass, observedAt time.Time) bool {
	class, anchor, _ := w.nextHoldState(j, requested, observedAt)
	return !observedAt.Before(anchor.Add(class.horizon()))
}

// nextHoldState applies the durable hold rules to the message's persisted pair
// and the class it is now encountering, reporting whether anything changed.
//
//   - First finite hold: the requested class, anchored at the latest of accept,
//     schedule, review, and last resume — or at the observation time for a
//     tenant-setup hold observed later than that.
//   - A budget hold promotes any class to policy_budget, keeping the anchor.
//   - policy_budget never changes again.
//   - Otherwise the persisted class stands: a later readiness loss does not
//     replace a rate class, and a rate hold does not replace a setup class.
func (w *SendWorker) nextHoldState(j *SendJob, requested HoldClass, observedAt time.Time) (HoldClass, time.Time, bool) {
	if j.LocalHoldClass == "" {
		anchor := j.initialHoldAnchor()
		// A tenant-setup hold observed later than the anchor starts its
		// clock at the observation; so does a message whose timestamps are
		// unknown, which must never be treated as already expired.
		if (requested == HoldTenantSetup && observedAt.After(anchor)) || anchor.IsZero() {
			anchor = observedAt
		}
		return requested, anchor, true
	}
	if j.LocalHoldClass == HoldPolicyBudget {
		return HoldPolicyBudget, j.LocalHoldAnchor, false
	}
	if requested == HoldPolicyBudget {
		return HoldPolicyBudget, j.LocalHoldAnchor, true
	}
	return j.LocalHoldClass, j.LocalHoldAnchor, false
}

// applyTenantReadiness performs the one-way setup→rate transition when the
// tenant became ready on or before the setup deadline. The comparison uses the
// stored readiness time, so a worker waking after the old deadline still
// honors readiness that committed in time.
func (w *SendWorker) applyTenantReadiness(ctx context.Context, j *SendJob) error {
	if j.LocalHoldClass != HoldTenantSetup || j.TenantReadyAt.IsZero() {
		return nil
	}
	if j.TenantReadyAt.After(j.LocalHoldAnchor.Add(HoldTenantSetup.horizon())) {
		return nil
	}
	if err := w.store.RecordHold(ctx, j.MessageID, HoldRateRampOrProvider, j.TenantReadyAt); err != nil {
		return fmt.Errorf("record tenant readiness transition: %w", err)
	}
	j.LocalHoldClass, j.LocalHoldAnchor = HoldRateRampOrProvider, j.TenantReadyAt
	return nil
}

// holdClassFor maps a gate hold reason to its finite-hold class; "" means the
// hold has no clock (an account pause).
func holdClassFor(reason string) HoldClass {
	switch {
	case reason == sendingpolicy.ReasonAccountPaused:
		return ""
	case strings.HasSuffix(reason, "_budget_exhausted"):
		return HoldPolicyBudget
	case reason == sendingpolicy.ReasonTenantNotReady, reason == sendingpolicy.ReasonTenantUnnamed:
		return HoldTenantSetup
	}
	return HoldRateRampOrProvider
}

// cancelTerminally fails the message for a reason no retry can change and
// gives its attempt back where the gate still allows it.
func (w *SendWorker) cancelTerminally(ctx context.Context, job *river.Job[OutboundSendArgs], j *SendJob, attempt sendingpolicy.AttemptRef, observedAt time.Time, detail string) error {
	if err := w.markFailed(ctx, j.MessageID, job.ID, job.Attempt, j.submissionAnchor(), observedAt, detail, delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionCancelled, nil); err != nil {
		return err
	}
	w.cancelAttempt(ctx, attempt, "terminal")
	return river.JobCancel(errors.New(detail))
}

// snoozeOnGateError releases the claim and snoozes when the gate itself is
// unavailable: fail toward retry, never toward an unauthorized submit, and
// never burn a River attempt on infrastructure.
func (w *SendWorker) snoozeOnGateError(ctx context.Context, job *river.Job[OutboundSendArgs], j *SendJob, step string, gerr error) error {
	if err := w.store.ReleaseSend(ctx, j.MessageID, job.ID); err != nil {
		return fmt.Errorf("release outbound send claim after gate %s failure: %w", step, errors.Join(gerr, err))
	}
	log.Printf("[outbound-send] sending policy %s failed for %s (snoozing): %v", step, j.MessageID, gerr)
	return river.JobSnooze(gateErrorSnoozeInterval)
}

// deferAttempt gives the budget back for a rate deferral; a stale or already
// released attempt is not an error here — the next Reserve is idempotent.
func (w *SendWorker) deferAttempt(ctx context.Context, attempt sendingpolicy.AttemptRef, why string) {
	if w.gate == nil {
		return
	}
	if err := w.gate.DeferAttempt(ctx, attempt); err != nil &&
		!errors.Is(err, sendingpolicy.ErrAttemptStale) && !errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
		log.Printf("[outbound-send] defer attempt (%s) for %s: %v", why, attempt.OperationID(), err)
	}
}

// cancelAttempt gives both ledgers back for a terminal local outcome. A
// started attempt cannot be refunded and says so; that is expected on a
// terminal path reached after a socket opened.
func (w *SendWorker) cancelAttempt(ctx context.Context, attempt sendingpolicy.AttemptRef, why string) {
	if w.gate == nil {
		return
	}
	// A zero attempt (no reservation was ever made) has nothing to give back;
	// the gate says so with ErrSourceUnavailable and that is not worth a log.
	if err := w.gate.CancelAttempt(ctx, attempt); err != nil &&
		!errors.Is(err, sendingpolicy.ErrAttemptStale) && !errors.Is(err, sendingpolicy.ErrProviderCallStarted) &&
		!errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
		log.Printf("[outbound-send] cancel attempt (%s) for %s: %v", why, attempt.OperationID(), err)
	}
}

// settleFromEvidence applies provider-accept evidence to the operation's
// latest dialed attempt. Best effort: the row is already settled as sent, and
// an attempt that predates the gate has nothing to settle.
func (w *SendWorker) settleFromEvidence(ctx context.Context, job *river.Job[OutboundSendArgs], providerMessageID string) {
	if w.gate == nil {
		return
	}
	ref, err := w.gate.LookupOperation(ctx, job.Args.MessageID)
	if err != nil {
		if !errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
			log.Printf("[outbound-send] lookup operation for evidence settle of %s: %v", job.Args.MessageID, err)
		}
		return
	}
	if err := w.gate.SettleOperation(ctx, ref, sendingpolicy.SettlementProviderAccepted, providerMessageID); err != nil && !errors.Is(err, sendingpolicy.ErrAttemptStale) {
		log.Printf("[outbound-send] settle %s from provider evidence: %v", job.Args.MessageID, err)
	}
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

// rateJitter spreads a deferred backlog's re-fire across a quarter-window.
// Every job deferred by the same burst gets a near-identical RetryAt (their
// blocking events were stamped ~simultaneously); without jitter the whole
// backlog re-wakes in lockstep every window and serializes on the agent's one
// hot rate row — a self-inflicted thundering herd of claim/reserve/release
// txs, log lines, and metric increments. Deterministic per message (FNV over
// the message id) so there is no RNG state and a given message's spread is
// stable across workers and replicas.
func rateJitter(messageID string, window time.Duration) time.Duration {
	maxJitter := window / 4
	if maxJitter <= 0 {
		return 0
	}
	// Sub-millisecond-precision windows (tests) truncate to 0ms — guard the
	// modulo against the divide-by-zero, not just the non-positive window.
	ms := maxJitter.Milliseconds()
	if ms <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(messageID))
	return time.Duration(h.Sum32()%uint32(ms)) * time.Millisecond
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
