package messagelifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot is the bounded durable state used to reconstruct one message.
type Snapshot struct {
	MessageID             string
	AgentID               string
	UserID                string
	Direction             string
	Method                string
	CreatedAt             time.Time
	Authentication        json.RawMessage
	Status                string
	ApprovalExpiresAt     *time.Time
	ReviewedAt            *time.Time
	SendJobID             *int64
	JobCreatedAt          *time.Time
	ProviderAcceptedAt    *time.Time
	ProviderMessageID     string
	EmailMessageID        string
	DeliveryStatus        string
	DeliveryFailureSource string
	Recipients            []RecipientSnapshot
	Suppressions          []SuppressionSnapshot
	Events                []EventSnapshot
}

type RecipientSnapshot struct {
	ID        string
	Address   string
	Status    string
	Detail    string
	UpdatedAt time.Time
}

type SuppressionSnapshot struct {
	ID              string
	Address         string
	Source          string
	SourceMessageID string
	CreatedAt       time.Time
}

type EventSnapshot struct {
	ID        string
	Type      string
	Envelope  json.RawMessage
	CreatedAt time.Time
}

type reconstructionCandidate struct {
	transition MessageLifecycleTransition
	sourceKind string
	sourceID   string
	event      bool
}

// Reconstruct derives only facts proven by the supplied durable snapshot.
func Reconstruct(snapshot Snapshot) []MessageLifecycleTransition {
	candidates := make(map[string]reconstructionCandidate)
	eventSemantics := make(map[string]bool)
	add := func(candidate reconstructionCandidate) {
		semantic := semanticKey(candidate.transition.ReasonCode, candidate.transition.Recipient)
		if candidate.event {
			// Retained events are the more specific representation of a rollup
			// state fact. Drop only state fallbacks for this semantic; distinct
			// retained source events remain separate observations.
			for key, existing := range candidates {
				if !existing.event && semanticKey(existing.transition.ReasonCode, existing.transition.Recipient) == semantic {
					delete(candidates, key)
				}
			}
			eventSemantics[semantic] = true
		} else if eventSemantics[semantic] {
			return
		}
		key := observationKey(candidate.transition)
		existing, ok := candidates[key]
		if !ok || transitionLess(candidate.transition, existing.transition) {
			candidates[key] = candidate
		}
	}

	acceptanceReason := ReasonAcceptanceOutboundAPI
	if snapshot.Direction == "inbound" {
		acceptanceReason = ReasonAcceptanceInboundSMTP
		if snapshot.Method == "loopback" {
			acceptanceReason = ReasonAcceptanceLocalLoopback
		}
	}
	addReconstructed(add, snapshot, acceptanceReason, "", snapshot.CreatedAt, "messages.created_at", "message", snapshot.MessageID, nil, baseCorrelations(snapshot), false)

	if snapshot.Direction == "inbound" && len(snapshot.Authentication) > 0 && string(snapshot.Authentication) != "null" {
		var authentication map[string]any
		var root struct {
			DMARC struct {
				Status string `json:"status"`
			} `json:"dmarc"`
		}
		if json.Unmarshal(snapshot.Authentication, &root) == nil && json.Unmarshal(snapshot.Authentication, &authentication) == nil {
			if reason, err := AuthenticationReason(root.DMARC.Status); err == nil {
				addReconstructed(add, snapshot, reason, "", snapshot.CreatedAt, "messages.authentication", "authentication", snapshot.MessageID, map[string]any{"authentication": authentication}, baseCorrelations(snapshot), false)
			}
		}
	}

	if snapshot.Status == "pending_review" {
		addReconstructed(add, snapshot, ReasonReviewHoldCreated, "", snapshot.CreatedAt, "messages.status", "review", snapshot.Status, nil, baseCorrelations(snapshot), false)
	}
	if snapshot.ReviewedAt != nil {
		var reason ReasonCode
		switch snapshot.Status {
		case "sent", "review_approved":
			reason = ReasonReviewApproved
		case "review_rejected":
			reason = ReasonReviewRejected
		case "review_expired_approved":
			reason = ReasonReviewExpiredApproved
		case "review_expired_rejected":
			reason = ReasonReviewExpiredRejected
		}
		if reason != "" {
			addReconstructed(add, snapshot, reason, "", *snapshot.ReviewedAt, "messages.reviewed_at", "review", snapshot.Status, nil, baseCorrelations(snapshot), false)
		}
	}

	if snapshot.Direction == "outbound" && snapshot.SendJobID != nil {
		occurredAt := snapshot.CreatedAt
		source := "messages.created_at"
		if snapshot.JobCreatedAt != nil {
			occurredAt, source = *snapshot.JobCreatedAt, "river_job.created_at"
		} else if snapshot.ReviewedAt != nil && approvedReviewStatus(snapshot.Status) {
			occurredAt, source = *snapshot.ReviewedAt, "messages.reviewed_at"
		}
		correlations := baseCorrelations(snapshot)
		correlations["job_id"] = strconv.FormatInt(*snapshot.SendJobID, 10)
		addReconstructed(add, snapshot, ReasonQueueOutboundSubmission, "", occurredAt, source, "job", strconv.FormatInt(*snapshot.SendJobID, 10), nil, correlations, false)
	}

	if snapshot.Direction == "outbound" {
		if snapshot.ProviderAcceptedAt != nil {
			addReconstructed(add, snapshot, ReasonSubmissionUpstreamAccepted, "", *snapshot.ProviderAcceptedAt, "messages.provider_accepted_at", "submission", snapshot.ProviderMessageID, nil, baseCorrelations(snapshot), false)
		} else if snapshot.Method == "loopback" && snapshot.DeliveryStatus == "sent" {
			occurredAt := snapshot.CreatedAt
			source := "messages.created_at"
			if snapshot.ReviewedAt != nil {
				occurredAt, source = *snapshot.ReviewedAt, "messages.reviewed_at"
			}
			addReconstructed(add, snapshot, ReasonSubmissionLocalLoopbackAccepted, "", occurredAt, source, "loopback", snapshot.MessageID, nil, baseCorrelations(snapshot), false)
		}
	}

	if snapshot.Direction == "outbound" {
		for _, recipient := range snapshot.Recipients {
			var reason ReasonCode
			switch recipient.Status {
			case "delivered":
				reason = ReasonDeliveryRecipientServerAccepted
			case "deferred":
				reason = ReasonDeliveryTemporaryDelay
			case "bounced":
				reason = ReasonDeliveryUndeterminedBounce
			case "complained":
				reason = ReasonComplaintRecipientReported
			default:
				continue
			}
			evidence := map[string]any{}
			if recipient.Detail != "" {
				evidence["smtp_detail"] = bounded(recipient.Detail)
			}
			addReconstructed(add, snapshot, reason, recipient.Address, recipient.UpdatedAt, "message_recipients.updated_at", "recipient", recipient.ID, evidence, baseCorrelations(snapshot), false)
		}

		for _, suppression := range snapshot.Suppressions {
			if suppression.SourceMessageID != snapshot.MessageID {
				continue
			}
			var reason ReasonCode
			switch suppression.Source {
			case "bounce":
				reason = ReasonSuppressionHardBounceApplied
			case "complaint":
				reason = ReasonSuppressionComplaintApplied
			default:
				continue
			}
			evidence := map[string]any{"suppression_source": suppression.Source, "suppression_scope": "account"}
			addReconstructed(add, snapshot, reason, suppression.Address, suppression.CreatedAt, "suppressions.created_at", "suppression", suppression.ID, evidence, baseCorrelations(snapshot), false)
		}
	}

	for _, event := range snapshot.Events {
		for _, candidate := range reconstructEvent(snapshot, event) {
			add(candidate)
		}
	}

	result := make([]MessageLifecycleTransition, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, cloneTransition(candidate.transition))
	}
	sort.Slice(result, func(i, j int) bool { return transitionLess(result[i], result[j]) })
	if result == nil {
		return []MessageLifecycleTransition{}
	}
	return result
}

// MergeTransitions retains every persisted observation and suppresses only a
// reconstructed candidate with the same source observation identity.
func MergeTransitions(persisted, reconstructed []MessageLifecycleTransition) []MessageLifecycleTransition {
	ownedSources := make(map[string]bool, len(persisted))
	ownedSourceLessTimestamps := make(map[string]bool, len(persisted))
	ownedAllTimestamps := make(map[string]bool, len(persisted))
	ownedSemantics := make(map[string]bool, len(persisted))
	ownedTransitionIDs := make(map[string]bool, len(persisted))
	result := make([]MessageLifecycleTransition, 0, len(persisted)+len(reconstructed))
	for _, item := range persisted {
		ownedSemantics[semanticKey(item.ReasonCode, item.Recipient)] = true
		if item.ID != "" {
			ownedTransitionIDs[item.ID] = true
		}
		timestampKey := timestampObservationKey(item)
		ownedAllTimestamps[timestampKey] = true
		if sourceKey, ok := sourceObservationKey(item); ok {
			ownedSources[sourceKey] = true
		} else {
			ownedSourceLessTimestamps[timestampKey] = true
		}
		result = append(result, cloneTransition(item))
	}
	seenFallback := map[string]bool{}
	for _, item := range reconstructed {
		key := observationKey(item)
		sourceKey, hasSource := sourceObservationKey(item)
		timestampKey := timestampObservationKey(item)
		matchesPersisted := (item.SourceTransitionID != "" && ownedTransitionIDs[item.SourceTransitionID]) ||
			(isStateFallback(item) && ownedSemantics[semanticKey(item.ReasonCode, item.Recipient)]) ||
			(hasSource && ownedSources[sourceKey]) || ownedSourceLessTimestamps[timestampKey] || (!hasSource && ownedAllTimestamps[timestampKey])
		if matchesPersisted || seenFallback[key] {
			continue
		}
		seenFallback[key] = true
		result = append(result, cloneTransition(item))
	}
	sort.Slice(result, func(i, j int) bool { return transitionLess(result[i], result[j]) })
	if result == nil {
		return []MessageLifecycleTransition{}
	}
	return result
}

func isStateFallback(item MessageLifecycleTransition) bool {
	source, _ := item.Evidence["source"].(string)
	return source != "" && source != "webhook_events.envelope"
}

func addReconstructed(add func(reconstructionCandidate), snapshot Snapshot, reason ReasonCode, recipient string, occurredAt time.Time, source, sourceKind, sourceID string, evidence map[string]any, correlations map[string]string, event bool) {
	if occurredAt.IsZero() {
		return
	}
	if evidence == nil {
		evidence = map[string]any{}
	}
	evidence["source"] = source
	input := AppendInput{MessageID: snapshot.MessageID, DedupeKey: "reconstructed", Direction: snapshot.Direction, Recipient: recipient, ReasonCode: reason, Evidence: evidence, CorrelationIDs: filteredCorrelationCopy(correlations), OccurredAt: occurredAt}
	transition, err := NewTransition(input)
	if err != nil {
		// A remaining validation failure means the durable source does not prove
		// a valid canonical fact (for example malformed authentication evidence
		// or a missing required recipient), so omission is deliberate.
		return
	}
	transition.ID = reconstructedID(snapshot.MessageID, sourceKind, sourceID, recipient, reason, transition.OccurredAt)
	transition.Reconstructed = true
	add(reconstructionCandidate{transition: transition, sourceKind: sourceKind, sourceID: sourceID, event: event})
}

// Correlations are optional metadata. Validate each independently with the
// canonical transition rules so one untrusted identifier cannot erase its fact.
func filteredCorrelationCopy(correlationIDs map[string]string) map[string]string {
	return SafeCorrelationIDs(correlationIDs)
}

func reconstructEvent(snapshot Snapshot, event EventSnapshot) []reconstructionCandidate {
	var envelope struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		CreatedAt time.Time       `json:"created_at"`
		Data      json.RawMessage `json:"data"`
	}
	if json.Unmarshal(event.Envelope, &envelope) != nil ||
		envelope.Type != event.Type || envelope.ID != event.ID ||
		!strings.HasPrefix(event.ID, "evt_") || envelope.CreatedAt.IsZero() {
		return nil
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(envelope.Data, &data) != nil || jsonString(data, "message_id") != snapshot.MessageID {
		return nil
	}
	if direction := jsonString(data, "direction"); direction != "" && direction != snapshot.Direction {
		return nil
	}
	var specs []struct {
		reason    ReasonCode
		recipient string
		evidence  map[string]any
	}
	switch event.Type {
	case "email.received":
		if snapshot.Direction != "inbound" {
			return nil
		}
		reason := ReasonAcceptanceInboundSMTP
		if snapshot.Method == "loopback" {
			reason = ReasonAcceptanceLocalLoopback
		}
		specs = append(specs, struct {
			reason    ReasonCode
			recipient string
			evidence  map[string]any
		}{reason: reason})
		if raw := data["authentication"]; len(raw) > 0 && string(raw) != "null" {
			var auth map[string]any
			var root struct {
				DMARC struct {
					Status string `json:"status"`
				} `json:"dmarc"`
			}
			if json.Unmarshal(raw, &root) == nil && json.Unmarshal(raw, &auth) == nil {
				if authReason, err := AuthenticationReason(root.DMARC.Status); err == nil {
					specs = append(specs, struct {
						reason    ReasonCode
						recipient string
						evidence  map[string]any
					}{reason: authReason, evidence: map[string]any{"authentication": auth}})
				}
			}
		}
	case "email.sent":
		if snapshot.Direction != "outbound" {
			return nil
		}
		reason := ReasonSubmissionUpstreamAccepted
		if jsonString(data, "method") == "loopback" || snapshot.Method == "loopback" {
			reason = ReasonSubmissionLocalLoopbackAccepted
		}
		specs = append(specs, struct {
			reason    ReasonCode
			recipient string
			evidence  map[string]any
		}{reason: reason})
	case "email.delivered":
		if snapshot.Direction != "outbound" {
			return nil
		}
		specs = append(specs, eventRecipientSpec(data, ReasonDeliveryRecipientServerAccepted))
	case "email.bounced":
		if snapshot.Direction != "outbound" {
			return nil
		}
		evidence := diagnosticEvidence(data, "smtp_detail", "bounce_type", "bounce_sub_type")
		reason := BounceReason(jsonString(data, "bounce_type"))
		specs = append(specs, struct {
			reason    ReasonCode
			recipient string
			evidence  map[string]any
		}{reason, jsonString(data, "delivered_to"), evidence})
	case "email.complained":
		if snapshot.Direction != "outbound" {
			return nil
		}
		specs = append(specs, eventRecipientSpec(data, ReasonComplaintRecipientReported))
	case "email.review_requested":
		specs = append(specs, struct {
			reason    ReasonCode
			recipient string
			evidence  map[string]any
		}{reason: ReasonReviewHoldCreated})
	case "email.review_approved":
		reason := ReasonReviewApproved
		if snapshot.Status == "review_expired_approved" {
			reason = ReasonReviewExpiredApproved
		}
		specs = append(specs, reviewEventSpec(data, reason))
	case "email.review_rejected":
		reason := ReasonReviewRejected
		if snapshot.Status == "review_expired_rejected" {
			reason = ReasonReviewExpiredRejected
		}
		specs = append(specs, reviewEventSpec(data, reason))
	case "domain.suppression_added":
		if snapshot.Direction != "outbound" {
			return nil
		}
		var reason ReasonCode
		source := jsonString(data, "source")
		if source == "bounce" {
			reason = ReasonSuppressionHardBounceApplied
		} else if source == "complaint" {
			reason = ReasonSuppressionComplaintApplied
		} else {
			return nil
		}
		specs = append(specs, struct {
			reason    ReasonCode
			recipient string
			evidence  map[string]any
		}{reason, jsonString(data, "address"), map[string]any{"suppression_source": source, "suppression_scope": "account"}})
	case "email.failed":
		if snapshot.Direction != "outbound" {
			return nil
		}
		var reason ReasonCode
		if snapshot.DeliveryFailureSource == "provider" {
			reason = ReasonSubmissionProviderRejected
		} else if snapshot.DeliveryFailureSource == "local" {
			reason = ReasonSubmissionLocalRetriesExhausted
			// A definitive external-sending-access refusal carries its own
			// stable reason on the event; reconstruct it as that reason so it
			// matches (and dedupes against) the persisted transition instead
			// of appearing a second time as retries-exhausted.
			if ReasonCode(jsonString(data, "reason_code")) == ReasonSubmissionExternalSendingNotEnabled {
				reason = ReasonSubmissionExternalSendingNotEnabled
			}
		} else {
			return nil
		}
		evidence := map[string]any{}
		if value := jsonString(data, "reason"); value != "" {
			evidence["failure_reason"] = bounded(value)
		}
		if value := jsonString(data, "reason_code"); value != "" {
			evidence["failure_code"] = bounded(value)
		}
		specs = append(specs, struct {
			reason    ReasonCode
			recipient string
			evidence  map[string]any
		}{reason: reason, evidence: evidence})
	default:
		return nil
	}

	result := make([]reconstructionCandidate, 0, len(specs))
	for _, spec := range specs {
		if recipientRequired(spec.reason) && strings.TrimSpace(spec.recipient) == "" {
			continue
		}
		correlations := baseCorrelations(snapshot)
		correlations["event_id"] = event.ID
		if provider := jsonString(data, "provider_message_id"); provider != "" {
			correlations["provider_message_id"] = provider
		}
		if providerEvent := jsonString(data, "provider_event_id"); providerEvent != "" {
			correlations["provider_event_id"] = providerEvent
		}
		var candidate reconstructionCandidate
		addReconstructed(func(value reconstructionCandidate) { candidate = value }, snapshot, spec.reason, spec.recipient, envelope.CreatedAt, "webhook_events.envelope", "event", event.ID, spec.evidence, correlations, true)
		if candidate.transition.ID != "" {
			candidate.transition.SourceTransitionID = embeddedTransitionID(data["lifecycle_transitions"], snapshot, spec.reason, spec.recipient)
			result = append(result, candidate)
		}
	}
	return result
}

func embeddedTransitionID(raw json.RawMessage, snapshot Snapshot, reason ReasonCode, recipient string) string {
	var transitions []MessageLifecycleTransition
	if len(raw) == 0 || json.Unmarshal(raw, &transitions) != nil {
		return ""
	}
	for _, transition := range transitions {
		if !strings.HasPrefix(transition.ID, "mlt_") || transition.MessageID != snapshot.MessageID ||
			transition.Direction != snapshot.Direction || transition.ReasonCode != reason || transition.Recipient != recipient {
			continue
		}
		canonical, err := NewTransition(AppendInput{
			MessageID: transition.MessageID, DedupeKey: "embedded", Direction: transition.Direction, Recipient: transition.Recipient,
			ReasonCode: transition.ReasonCode, Evidence: transition.Evidence,
			CorrelationIDs: transition.CorrelationIDs, OccurredAt: transition.OccurredAt,
		})
		if err == nil && transition.Stage == canonical.Stage && transition.Outcome == canonical.Outcome && transition.Retryable == canonical.Retryable {
			return transition.ID
		}
	}
	return ""
}

func eventRecipientSpec(data map[string]json.RawMessage, reason ReasonCode) struct {
	reason    ReasonCode
	recipient string
	evidence  map[string]any
} {
	return struct {
		reason    ReasonCode
		recipient string
		evidence  map[string]any
	}{reason, jsonString(data, "delivered_to"), diagnosticEvidence(data, "smtp_detail")}
}

func reviewEventSpec(data map[string]json.RawMessage, reason ReasonCode) struct {
	reason    ReasonCode
	recipient string
	evidence  map[string]any
} {
	evidence := map[string]any{}
	if value := jsonString(data, "reason"); value != "" {
		evidence["review_resolution"] = bounded(value)
	}
	return struct {
		reason    ReasonCode
		recipient string
		evidence  map[string]any
	}{reason: reason, evidence: evidence}
}

func diagnosticEvidence(data map[string]json.RawMessage, keys ...string) map[string]any {
	evidence := map[string]any{}
	for _, key := range keys {
		if value := jsonString(data, key); value != "" {
			evidence[key] = bounded(value)
		}
	}
	return evidence
}

func jsonString(data map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(data[key], &value)
	return value
}

func baseCorrelations(snapshot Snapshot) map[string]string {
	result := map[string]string{}
	if snapshot.Direction == "outbound" && snapshot.ProviderMessageID != "" {
		result["provider_message_id"] = snapshot.ProviderMessageID
	}
	if snapshot.EmailMessageID != "" {
		result["email_message_id"] = snapshot.EmailMessageID
	}
	return result
}

func approvedReviewStatus(status string) bool {
	return status == "sent" || status == "review_approved" || status == "review_expired_approved"
}
func semanticKey(reason ReasonCode, recipient string) string {
	return string(reason) + "\x00" + recipient
}

// observationKey identifies one logical source observation. Provider event IDs
// are the strongest cross-path identity, followed by retained event IDs. Legacy
// rows without either use their observed timestamp; differing timestamps are
// distinct facts rather than retries of one fact.
func observationKey(item MessageLifecycleTransition) string {
	if item.SourceTransitionID != "" {
		return semanticKey(item.ReasonCode, item.Recipient) + "\x00transition_id\x00" + item.SourceTransitionID
	}
	if key, ok := sourceObservationKey(item); ok {
		return key
	}
	return timestampObservationKey(item)
}

func sourceObservationKey(item MessageLifecycleTransition) (string, bool) {
	semantic := semanticKey(item.ReasonCode, item.Recipient)
	// Review state is message-local and its producer dedupe contract allows one
	// logical transition per reason. The owning message ID therefore joins a
	// canonical row to a retained beta review event even when that event did not
	// carry the transition ID.
	if item.Stage == StageReview && item.MessageID != "" {
		return semantic + "\x00message_id\x00" + item.MessageID, true
	}
	if value := item.CorrelationIDs["provider_event_id"]; value != "" {
		return semantic + "\x00provider_event_id\x00" + value, true
	}
	// Acceptance and submission producers historically used both names for the
	// same RFC 5322 message identity. Restrict the alias to pre-delivery stages
	// so one provider message can still have multiple feedback observations.
	if item.Stage == StageAccepted || item.ReasonCode == ReasonSubmissionUpstreamAccepted || item.ReasonCode == ReasonSubmissionLocalLoopbackAccepted {
		for _, correlation := range []string{"provider_message_id", "email_message_id"} {
			if value := item.CorrelationIDs[correlation]; value != "" {
				return semantic + "\x00message_id\x00" + value, true
			}
		}
	}
	if strings.HasPrefix(item.DedupeKey, "provider-feedback:") {
		if sourceID, _, ok := strings.Cut(strings.TrimPrefix(item.DedupeKey, "provider-feedback:"), ":"); ok && sourceID != "" {
			return semantic + "\x00provider_event_id\x00" + sourceID, true
		}
	}
	if value := item.CorrelationIDs["event_id"]; value != "" {
		return semantic + "\x00event_id\x00" + value, true
	}
	return "", false
}

func timestampObservationKey(item MessageLifecycleTransition) string {
	return semanticKey(item.ReasonCode, item.Recipient) + "\x00occurred_at\x00" + item.OccurredAt.UTC().Format(time.RFC3339Nano)
}

func transitionLess(left, right MessageLifecycleTransition) bool {
	if left.OccurredAt.Equal(right.OccurredAt) {
		return left.ID < right.ID
	}
	return left.OccurredAt.Before(right.OccurredAt)
}

func reconstructedID(messageID, sourceKind, sourceID, recipient string, reason ReasonCode, occurredAt time.Time) string {
	tuple, _ := json.Marshal([]string{messageID, sourceKind, sourceID, recipient, string(reason), occurredAt.UTC().Format(time.RFC3339Nano)})
	sum := sha256.Sum256(tuple)
	return "mlt_recon_" + hex.EncodeToString(sum[:16])
}

func bounded(value string) string {
	if len(value) <= maxDiagnosticStringBytes {
		return value
	}
	return value[:maxDiagnosticStringBytes]
}

func cloneTransition(item MessageLifecycleTransition) MessageLifecycleTransition {
	evidence, _ := json.Marshal(item.Evidence)
	_ = json.Unmarshal(evidence, &item.Evidence)
	if item.Evidence == nil {
		item.Evidence = map[string]any{}
	}
	correlations := make(map[string]string, len(item.CorrelationIDs))
	for key, value := range item.CorrelationIDs {
		correlations[key] = value
	}
	item.CorrelationIDs = correlations
	item.OccurredAt = item.OccurredAt.UTC()
	return item
}
