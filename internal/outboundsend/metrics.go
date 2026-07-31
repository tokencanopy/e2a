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
// settle, the caller's observation time otherwise); acceptedAt is
// messages.created_at. A zero timestamp or non-positive delta records the
// count but no latency sample (same discipline as the queue-wait guard).
func emitTerminal(m Metrics, outcome string, acceptedAt, occurredAt time.Time) {
	m.OutboundTerminal(outcome)
	if acceptedAt.IsZero() || occurredAt.IsZero() {
		return
	}
	if d := occurredAt.Sub(acceptedAt); d > 0 {
		m.OutboundTerminalLatency(d.Seconds())
	}
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
