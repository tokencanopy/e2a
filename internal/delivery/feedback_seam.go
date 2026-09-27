package delivery

import (
	"context"
	"time"
)

// ProviderAttemptHeader is the random per-attempt correlation marker the
// provider adapter stamps on every submission (outbound.ProviderAttemptHeader
// — that package imports this one, so the name is mirrored here and pinned
// equal by a test there). SES echoes it back in mail.headers; it is the
// correlation fallback when the worker died between SES accepting a message
// and the provider id being stored.
const ProviderAttemptHeader = "X-E2A-Provider-Attempt"

// ProviderFeedback is one signed provider notification reduced to what the
// deletion-resistant accounting needs: identity (event id, time), kind and
// subtypes, the two correlation keys, and the plaintext recipients the
// provider named. It carries no message, agent, or account reference — the
// processor derives those from its own retained rows.
type ProviderFeedback struct {
	ProviderEventID string
	OccurredAt      time.Time
	Kind            EventKind
	// BounceType is the normalized permanent | transient | undetermined;
	// BounceSubType and ComplaintSubType are the raw SES subtypes, retained
	// because the detector bucket is derived from them.
	BounceType       string
	BounceSubType    string
	ComplaintSubType string
	// ProviderMessageID is the SES mail.messageId; AttemptCorrelationID is
	// the echoed ProviderAttemptHeader (empty when headers were absent).
	ProviderMessageID    string
	AttemptCorrelationID string
	// Recipients are the normalized addresses this notification is about.
	Recipients []string
}

// FeedbackSuppression reports a suppression the processor upserted for one
// recipient while its account still existed.
type FeedbackSuppression struct {
	Address  string
	ID       string
	Source   string
	Reason   string
	Inserted bool
}

// FeedbackResult is what the processor did with one notification.
type FeedbackResult struct {
	// Correlated is false when no retained correlation matched either key;
	// the notification is then foreign or past its retention.
	Correlated bool
	// Duplicate is true when this provider event id was already processed;
	// nothing changed.
	Duplicate bool
	// AccountRef is the opaque source account when it still exists; empty
	// after account deletion or for non-customer purposes.
	AccountRef string
	// Suppressions lists the account suppressions upserted, keyed by address.
	Suppressions []FeedbackSuppression
}

// SuppressionFor returns the upsert recorded for a normalized address.
func (r FeedbackResult) SuppressionFor(address string) (FeedbackSuppression, bool) {
	for _, s := range r.Suppressions {
		if s.Address == address {
			return s, true
		}
	}
	return FeedbackSuppression{}, false
}

// FeedbackProcessor is the deletion-resistant accounting seam the consumer
// calls BEFORE it looks for a live message: it must succeed (or fail, so the
// provider retries) without any message, agent, or user row surviving.
// Implemented by the sending-policy module.
type FeedbackProcessor interface {
	ProcessProviderFeedback(context.Context, ProviderFeedback) (FeedbackResult, error)
}
