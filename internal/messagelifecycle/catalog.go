// Package messagelifecycle defines the canonical vocabulary and validated
// in-memory representation of message lifecycle observations.
package messagelifecycle

import "fmt"

// Stage identifies the boundary at which e2a made an observation.
type Stage string

const (
	StageAccepted       Stage = "accepted"
	StageAuthentication Stage = "authentication"
	StageReview         Stage = "review"
	StageSuppression    Stage = "suppression"
	StageQueued         Stage = "queued"
	StageSubmission     Stage = "submission"
	StageDelivery       Stage = "delivery"
	StageComplaint      Stage = "complaint"
)

// Outcome identifies what e2a observed at a lifecycle stage.
type Outcome string

const (
	OutcomeAccepted      Outcome = "accepted"
	OutcomePassed        Outcome = "passed"
	OutcomeFailed        Outcome = "failed"
	OutcomeIndeterminate Outcome = "indeterminate"
	OutcomePending       Outcome = "pending"
	OutcomeApproved      Outcome = "approved"
	OutcomeRejected      Outcome = "rejected"
	OutcomeBlocked       Outcome = "blocked"
	OutcomeApplied       Outcome = "applied"
	OutcomeEnqueued      Outcome = "enqueued"
	OutcomeDeferred      Outcome = "deferred"
	OutcomeDelivered     Outcome = "delivered"
	OutcomeBounced       Outcome = "bounced"
	OutcomeReported      Outcome = "reported"
)

// ReasonCode is a stable machine-readable lifecycle observation.
type ReasonCode string

const (
	ReasonAcceptanceInboundSMTP             ReasonCode = "acceptance.inbound_smtp"
	ReasonAcceptanceOutboundAPI             ReasonCode = "acceptance.outbound_api"
	ReasonAcceptanceLocalLoopback           ReasonCode = "acceptance.local_loopback"
	ReasonAuthenticationDMARCPass           ReasonCode = "authentication.dmarc_pass"
	ReasonAuthenticationDMARCFail           ReasonCode = "authentication.dmarc_fail"
	ReasonAuthenticationDMARCNone           ReasonCode = "authentication.dmarc_none"
	ReasonAuthenticationDMARCTemporaryError ReasonCode = "authentication.dmarc_temporary_error"
	ReasonAuthenticationDMARCPermanentError ReasonCode = "authentication.dmarc_permanent_error"
	ReasonReviewHoldCreated                 ReasonCode = "review.hold_created"
	ReasonReviewApproved                    ReasonCode = "review.approved"
	ReasonReviewRejected                    ReasonCode = "review.rejected"
	ReasonReviewExpiredApproved             ReasonCode = "review.expired_approved"
	ReasonReviewExpiredRejected             ReasonCode = "review.expired_rejected"
	ReasonSuppressionRecipientBlocked       ReasonCode = "suppression.recipient_blocked"
	ReasonSuppressionHardBounceApplied      ReasonCode = "suppression.hard_bounce_applied"
	ReasonSuppressionComplaintApplied       ReasonCode = "suppression.complaint_applied"
	ReasonQueueInboundProcessing            ReasonCode = "queue.inbound_processing"
	ReasonQueueOutboundSubmission           ReasonCode = "queue.outbound_submission"
	ReasonSubmissionUpstreamAccepted        ReasonCode = "submission.upstream_accepted"
	ReasonSubmissionLocalLoopbackAccepted   ReasonCode = "submission.local_loopback_accepted"
	ReasonSubmissionTemporaryFailure        ReasonCode = "submission.temporary_failure"
	ReasonSubmissionProviderRejected        ReasonCode = "submission.provider_rejected"
	ReasonSubmissionLocalRetriesExhausted   ReasonCode = "submission.local_retries_exhausted"
	ReasonSubmissionCancelled               ReasonCode = "submission.cancelled"
	// ReasonSubmissionPolicyBudgetExpired means a sending-budget hold reached
	// its seven-day deadline without capacity freeing. It is a local policy
	// outcome, never a recipient rejection or a provider outage.
	ReasonSubmissionPolicyBudgetExpired ReasonCode = "submission.policy_budget_expired"
	// ReasonSubmissionSendingSetupExpired means the account's provider-side
	// sending setup (SES tenant readiness) did not complete within the
	// 72-hour setup deadline.
	ReasonSubmissionSendingSetupExpired   ReasonCode = "submission.sending_setup_expired"
	ReasonDeliveryRecipientServerAccepted ReasonCode = "delivery.recipient_server_accepted"
	ReasonDeliveryTemporaryDelay          ReasonCode = "delivery.temporary_delay"
	ReasonDeliveryPermanentBounce         ReasonCode = "delivery.permanent_bounce"
	ReasonDeliveryTransientBounce         ReasonCode = "delivery.transient_bounce"
	ReasonDeliveryUndeterminedBounce      ReasonCode = "delivery.undetermined_bounce"
	ReasonComplaintRecipientReported      ReasonCode = "complaint.recipient_reported"
)

// Definition is the fixed meaning of a reason code.
type Definition struct {
	Stage     Stage
	Outcome   Outcome
	Retryable bool
}

var canonicalCatalog = map[ReasonCode]Definition{
	ReasonAcceptanceInboundSMTP:             {StageAccepted, OutcomeAccepted, false},
	ReasonAcceptanceOutboundAPI:             {StageAccepted, OutcomeAccepted, false},
	ReasonAcceptanceLocalLoopback:           {StageAccepted, OutcomeAccepted, false},
	ReasonAuthenticationDMARCPass:           {StageAuthentication, OutcomePassed, false},
	ReasonAuthenticationDMARCFail:           {StageAuthentication, OutcomeFailed, false},
	ReasonAuthenticationDMARCNone:           {StageAuthentication, OutcomeIndeterminate, false},
	ReasonAuthenticationDMARCTemporaryError: {StageAuthentication, OutcomeIndeterminate, true},
	ReasonAuthenticationDMARCPermanentError: {StageAuthentication, OutcomeIndeterminate, false},
	ReasonReviewHoldCreated:                 {StageReview, OutcomePending, false},
	ReasonReviewApproved:                    {StageReview, OutcomeApproved, false},
	ReasonReviewRejected:                    {StageReview, OutcomeRejected, false},
	ReasonReviewExpiredApproved:             {StageReview, OutcomeApproved, false},
	ReasonReviewExpiredRejected:             {StageReview, OutcomeRejected, false},
	ReasonSuppressionRecipientBlocked:       {StageSuppression, OutcomeBlocked, false},
	ReasonSuppressionHardBounceApplied:      {StageSuppression, OutcomeApplied, false},
	ReasonSuppressionComplaintApplied:       {StageSuppression, OutcomeApplied, false},
	ReasonQueueInboundProcessing:            {StageQueued, OutcomeEnqueued, false},
	ReasonQueueOutboundSubmission:           {StageQueued, OutcomeEnqueued, false},
	ReasonSubmissionUpstreamAccepted:        {StageSubmission, OutcomeAccepted, false},
	ReasonSubmissionLocalLoopbackAccepted:   {StageSubmission, OutcomeAccepted, false},
	ReasonSubmissionTemporaryFailure:        {StageSubmission, OutcomeDeferred, true},
	ReasonSubmissionProviderRejected:        {StageSubmission, OutcomeFailed, false},
	ReasonSubmissionLocalRetriesExhausted:   {StageSubmission, OutcomeFailed, true},
	ReasonSubmissionCancelled:               {StageSubmission, OutcomeFailed, false},
	ReasonSubmissionPolicyBudgetExpired:     {StageSubmission, OutcomeFailed, true},
	ReasonSubmissionSendingSetupExpired:     {StageSubmission, OutcomeFailed, true},
	ReasonDeliveryRecipientServerAccepted:   {StageDelivery, OutcomeDelivered, false},
	ReasonDeliveryTemporaryDelay:            {StageDelivery, OutcomeDeferred, true},
	ReasonDeliveryPermanentBounce:           {StageDelivery, OutcomeBounced, false},
	ReasonDeliveryTransientBounce:           {StageDelivery, OutcomeBounced, true},
	ReasonDeliveryUndeterminedBounce:        {StageDelivery, OutcomeBounced, false},
	ReasonComplaintRecipientReported:        {StageComplaint, OutcomeReported, false},
}

// Catalog returns a copy of the canonical reason-code catalog.
func Catalog() map[ReasonCode]Definition {
	result := make(map[ReasonCode]Definition, len(canonicalCatalog))
	for reason, definition := range canonicalCatalog {
		result[reason] = definition
	}
	return result
}

// Lookup returns the immutable definition for reason.
func Lookup(reason ReasonCode) (Definition, bool) {
	definition, ok := canonicalCatalog[reason]
	return definition, ok
}

// IsTerminalSubmissionFailure reports whether code is a catalog-owned
// submission/failed observation suitable for the durable message failure
// rollup. Arbitrary database text must never reach AppendTx through this path.
func IsTerminalSubmissionFailure(code ReasonCode) bool {
	definition, ok := Lookup(code)
	return ok && definition.Stage == StageSubmission && definition.Outcome == OutcomeFailed
}

// AuthenticationReason maps a normalized DMARC status to its canonical reason.
func AuthenticationReason(status string) (ReasonCode, error) {
	switch status {
	case "pass":
		return ReasonAuthenticationDMARCPass, nil
	case "fail":
		return ReasonAuthenticationDMARCFail, nil
	case "none":
		return ReasonAuthenticationDMARCNone, nil
	case "temperror":
		return ReasonAuthenticationDMARCTemporaryError, nil
	case "permerror":
		return ReasonAuthenticationDMARCPermanentError, nil
	default:
		return "", fmt.Errorf("unknown DMARC status %q", status)
	}
}

// BounceReason maps a normalized provider bounce type to its canonical reason.
// Unknown values use the provider normalizer's undetermined catch-all.
func BounceReason(bounceType string) ReasonCode {
	switch bounceType {
	case "permanent":
		return ReasonDeliveryPermanentBounce
	case "transient":
		return ReasonDeliveryTransientBounce
	default:
		return ReasonDeliveryUndeterminedBounce
	}
}
