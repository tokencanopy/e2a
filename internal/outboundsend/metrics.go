package outboundsend

import (
	"time"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

// Metrics is the narrow slice of telemetry.Metrics the outbound send pipeline
// emits (the janitor.Metrics pattern): injectable so tests assert emission
// with a fake, satisfied by every telemetry backend. Label values are
// normalized by the backend — never pass message ids or addresses.
type Metrics interface {
	// OutboundQueueWait is the enqueue→worker-pickup latency of one send
	// attempt (River attempted_at − scheduled_at — due→pickup, never
	// cumulative message age).
	OutboundQueueWait(seconds float64)
	// OutboundTerminal records one terminal outcome for an outbound message.
	// outcome ∈ {sent, failed_suppressed, failed_provider,
	// failed_local_retries, failed_cancelled}.
	OutboundTerminal(outcome string)
	// OutboundTerminalLatency records acceptance→terminal latency for one
	// outbound message (the terminal write's occurred_at −
	// messages.created_at). Observed exactly once per message, co-located
	// with OutboundTerminal so the two share their exactly-once contract.
	OutboundTerminalLatency(seconds float64)
	// OutboundAttempt records one submission attempt to the upstream relay.
	// outcome ∈ {success, temporary_failure, permanent_failure}.
	OutboundAttempt(outcome string, seconds float64)
	// OutboundRateDeferred records one submission deferred by the per-agent
	// fire-time rate gate — a snooze, not an attempt, and never terminal.
	OutboundRateDeferred()
}

// The telemetry.Metrics label enums, pinned as constants so the worker and
// the terminal reconciler cannot drift apart on spelling.
const (
	terminalSent               = "sent"
	terminalFailedSuppressed   = "failed_suppressed"
	terminalFailedProvider     = "failed_provider"
	terminalFailedLocalRetries = "failed_local_retries"
	terminalFailedCancelled    = "failed_cancelled"

	attemptSuccess          = "success"
	attemptTemporaryFailure = "temporary_failure"
	attemptPermanentFailure = "permanent_failure"
)

// noopMetrics is the nil-safe default: a worker built without WithMetrics
// records nothing instead of nil-panicking mid-send.
type noopMetrics struct{}

func (noopMetrics) OutboundQueueWait(float64)       {}
func (noopMetrics) OutboundTerminal(string)         {}
func (noopMetrics) OutboundTerminalLatency(float64) {}
func (noopMetrics) OutboundAttempt(string, float64) {}
func (noopMetrics) OutboundRateDeferred()           {}

// emitTerminal records one terminal outcome count AND its co-located
// acceptance→terminal latency — the two instruments' exactly-once contract
// lives in this single helper so no call site can emit one without the
// other. occurredAt is the terminal write's EFFECTIVE occurred_at (what the
// write actually did: the provider-accept evidence time on an evidence
// settle, the caller's observation time otherwise); anchorAt is the
// submission anchor (see submissionAnchor — messages.created_at for an
// ordinary send). A zero timestamp or non-positive delta records the
// count but no latency sample (same discipline as the queue-wait guard).
func emitTerminal(m Metrics, outcome string, anchorAt, occurredAt time.Time) {
	m.OutboundTerminal(outcome)
	if anchorAt.IsZero() || occurredAt.IsZero() {
		return
	}
	if d := occurredAt.Sub(anchorAt); d > 0 {
		m.OutboundTerminalLatency(d.Seconds())
	}
}

// submissionAnchor is the acceptance→terminal SLI's baseline: the instant a
// message became ELIGIBLE for provider submission, which is not always its
// accept time. Two gates deliberately hold an accepted row before the pipeline
// may touch it, and both are the caller's choice rather than e2a latency:
//
//   - a HITL hold sits in pending_review until a reviewer (or the TTL sweep)
//     resolves it — reviewedAt is when it was approved into the send pipeline;
//   - a scheduled send waits for its fire time — scheduledAt.
//
// Measuring from messages.created_at charged both waits to e2a's error budget,
// so a reviewer taking six minutes, or any send scheduled further out than the
// 300s SLO window, recorded as a miss (docs/observability.md: every target
// measures e2a's own behavior). Taking the latest of the three mirrors
// pastRetryHorizon's existing max(accept, scheduled) reasoning and keeps the
// send worker and the terminal reconciler on one definition. Zero values are
// inert — an unheld, unscheduled message anchors at acceptedAt as before.
func submissionAnchor(acceptedAt, scheduledAt, reviewedAt time.Time) time.Time {
	anchor := acceptedAt
	if scheduledAt.After(anchor) {
		anchor = scheduledAt
	}
	if reviewedAt.After(anchor) {
		anchor = reviewedAt
	}
	return anchor
}

// terminalOutcome maps a MarkFailed call's provenance to the OutboundTerminal
// label: suppression holds blocked recipients; a policy cancel without them
// (a cancelled job settled by the reconciler) is failed_cancelled — NOT
// failed_local_retries, so cancellation volume can't mask a real
// retries-exhausted regression in the alerting signal; a provider-confirmed
// rejection carries provenance 'provider'; everything else is a local
// give-up (retries/horizon exhausted). MarkFailed is the GUARDED terminal
// write — a row holding provider-accept evidence settles as sent instead —
// so this labels the intended outcome; that rare evidence-settle correction
// is invisible here and negligible at SLI granularity.
func terminalOutcome(source delivery.FailureSource, reason messagelifecycle.ReasonCode, blockedRecipients []string) string {
	switch {
	case len(blockedRecipients) > 0:
		return terminalFailedSuppressed
	case reason == messagelifecycle.ReasonSubmissionCancelled:
		return terminalFailedCancelled
	case source == delivery.FailureSourceProvider:
		return terminalFailedProvider
	default:
		return terminalFailedLocalRetries
	}
}
