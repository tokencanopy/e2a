package messagelifecycle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SendSuppressionDedupeKey is the stable message-local identity for a
// recipient suppression observed by one outbound River attempt.
func SendSuppressionDedupeKey(jobID int64, attempt int, recipient string) string {
	return "suppression:send:job:" + strconv.FormatInt(jobID, 10) + ":attempt:" + strconv.Itoa(attempt) + ":recipient:" + recipient
}

// SafeDiagnostic bounds optional untrusted diagnostics to the canonical
// per-string limit without splitting a UTF-8 encoding. Invalid input bytes are
// discarded so optional evidence can never reject the lifecycle observation.
func SafeDiagnostic(value string) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "")
	}
	if len(value) <= maxDiagnosticStringBytes {
		return value
	}
	value = value[:maxDiagnosticStringBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

const (
	maxDiagnosticStringBytes = 2 * 1024
	maxEvidenceBytes         = 16 * 1024
)

var allowedEvidenceKeys = map[string]bool{
	"authentication":     true,
	"smtp_detail":        true,
	"bounce_type":        true,
	"bounce_sub_type":    true,
	"failure_reason":     true,
	"failure_code":       true,
	"review_resolution":  true,
	"suppression_scope":  true,
	"suppression_source": true,
	"source":             true,
}

var allowedCorrelationKeys = map[string]bool{
	"event_id":            true,
	"job_id":              true,
	"provider_message_id": true,
	"provider_event_id":   true,
	"email_message_id":    true,
}

// MessageLifecycleTransition is one validated canonical lifecycle observation.
type MessageLifecycleTransition struct {
	ID                 string            `json:"id"`
	MessageID          string            `json:"message_id"`
	DedupeKey          string            `json:"-"`
	SourceTransitionID string            `json:"-"`
	Direction          string            `json:"direction" enum:"inbound,outbound"`
	Recipient          string            `json:"recipient,omitempty" nullable:"true"`
	Stage              Stage             `json:"stage" enum:"accepted,authentication,review,suppression,queued,submission,delivery,complaint"`
	Outcome            Outcome           `json:"outcome" enum:"accepted,passed,failed,indeterminate,pending,approved,rejected,blocked,applied,enqueued,deferred,delivered,bounced,reported"`
	ReasonCode         ReasonCode        `json:"reason_code" enum:"acceptance.inbound_smtp,acceptance.outbound_api,acceptance.local_loopback,authentication.dmarc_pass,authentication.dmarc_fail,authentication.dmarc_none,authentication.dmarc_temporary_error,authentication.dmarc_permanent_error,review.hold_created,review.approved,review.rejected,review.expired_approved,review.expired_rejected,suppression.recipient_blocked,suppression.hard_bounce_applied,suppression.complaint_applied,queue.inbound_processing,queue.outbound_submission,submission.upstream_accepted,submission.local_loopback_accepted,submission.temporary_failure,submission.provider_rejected,submission.local_retries_exhausted,submission.cancelled,submission.policy_budget_expired,submission.sending_setup_expired,delivery.recipient_server_accepted,delivery.temporary_delay,delivery.permanent_bounce,delivery.transient_bounce,delivery.undetermined_bounce,complaint.recipient_reported"`
	Retryable          bool              `json:"retryable"`
	Evidence           map[string]any    `json:"evidence"`
	CorrelationIDs     map[string]string `json:"correlation_ids"`
	OccurredAt         time.Time         `json:"occurred_at"`
	Reconstructed      bool              `json:"reconstructed"`
}

// AppendInput contains the producer-controlled fields used to construct a
// transition. DedupeKey is validated here and retained by the persistence
// operation introduced with the lifecycle store.
type AppendInput struct {
	MessageID      string
	DedupeKey      string
	Direction      string
	Recipient      string
	ReasonCode     ReasonCode
	Evidence       map[string]any
	CorrelationIDs map[string]string
	OccurredAt     time.Time
}

// NewTransition validates input and derives the semantic tuple from the closed
// reason catalog. The returned unpersisted transition has an empty ID; the
// lifecycle store assigns its stable mlt_ identifier when appending it.
func NewTransition(input AppendInput) (MessageLifecycleTransition, error) {
	if strings.TrimSpace(input.MessageID) == "" {
		return MessageLifecycleTransition{}, fmt.Errorf("message ID is required")
	}
	if strings.TrimSpace(input.DedupeKey) == "" {
		return MessageLifecycleTransition{}, fmt.Errorf("dedupe key is required")
	}
	if input.Direction != "inbound" && input.Direction != "outbound" {
		return MessageLifecycleTransition{}, fmt.Errorf("invalid direction %q", input.Direction)
	}
	definition, ok := Lookup(input.ReasonCode)
	if !ok {
		return MessageLifecycleTransition{}, fmt.Errorf("unknown reason code %q", input.ReasonCode)
	}
	if input.OccurredAt.IsZero() {
		return MessageLifecycleTransition{}, fmt.Errorf("occurred at is required")
	}
	if err := validateRecipient(input.ReasonCode, input.Recipient); err != nil {
		return MessageLifecycleTransition{}, err
	}

	evidence, err := validatedEvidenceCopy(input.Evidence)
	if err != nil {
		return MessageLifecycleTransition{}, err
	}
	correlationIDs, err := validatedCorrelationCopy(input.CorrelationIDs)
	if err != nil {
		return MessageLifecycleTransition{}, err
	}

	return MessageLifecycleTransition{
		MessageID:      input.MessageID,
		Direction:      input.Direction,
		Recipient:      input.Recipient,
		Stage:          definition.Stage,
		Outcome:        definition.Outcome,
		ReasonCode:     input.ReasonCode,
		Retryable:      definition.Retryable,
		Evidence:       evidence,
		CorrelationIDs: correlationIDs,
		OccurredAt:     input.OccurredAt.UTC(),
		Reconstructed:  false,
	}, nil
}

func validateRecipient(reason ReasonCode, recipient string) error {
	if recipientRequired(reason) {
		if strings.TrimSpace(recipient) == "" {
			return fmt.Errorf("recipient is required for reason %q", reason)
		}
		return nil
	}
	if recipient != "" {
		return fmt.Errorf("recipient is not allowed for reason %q", reason)
	}
	return nil
}

func recipientRequired(reason ReasonCode) bool {
	switch reason {
	case ReasonSuppressionRecipientBlocked,
		ReasonSuppressionHardBounceApplied,
		ReasonSuppressionComplaintApplied,
		ReasonDeliveryRecipientServerAccepted,
		ReasonDeliveryTemporaryDelay,
		ReasonDeliveryPermanentBounce,
		ReasonDeliveryTransientBounce,
		ReasonDeliveryUndeterminedBounce,
		ReasonComplaintRecipientReported:
		return true
	default:
		return false
	}
}

func validatedEvidenceCopy(evidence map[string]any) (map[string]any, error) {
	if evidence == nil {
		return map[string]any{}, nil
	}
	for key, value := range evidence {
		if !allowedEvidenceKeys[key] {
			return nil, fmt.Errorf("evidence key %q is not allowed", key)
		}
		if key == "authentication" {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("evidence %q must be a string", key)
		}
		if len(text) > maxDiagnosticStringBytes {
			return nil, fmt.Errorf("evidence %q exceeds 2 KiB", key)
		}
	}

	encoded, err := json.Marshal(evidence)
	if err != nil {
		return nil, fmt.Errorf("marshal evidence: %w", err)
	}
	if len(encoded) > maxEvidenceBytes {
		return nil, fmt.Errorf("serialized evidence exceeds 16 KiB")
	}

	var copied map[string]any
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return nil, fmt.Errorf("copy evidence: %w", err)
	}
	if authentication, ok := copied["authentication"]; ok {
		encodedAuthentication, err := json.Marshal(authentication)
		if err != nil {
			return nil, fmt.Errorf("marshal evidence authentication: %w", err)
		}
		if err := validateAuthentication(encodedAuthentication); err != nil {
			return nil, fmt.Errorf("evidence authentication: %w", err)
		}
		if err := validateNestedStrings(authentication); err != nil {
			return nil, fmt.Errorf("evidence authentication: %w", err)
		}
	}
	return copied, nil
}

func validateAuthentication(encoded []byte) error {
	root, err := validateAuthenticationObject(encoded, "authentication",
		[]string{"spf", "dkim", "dmarc"},
		[]string{"spf", "dkim", "dmarc"},
	)
	if err != nil {
		return err
	}

	spf, err := validateAuthenticationObject(root["spf"], "spf",
		[]string{"status", "domain", "aligned", "detail"},
		[]string{"status", "domain", "aligned"},
	)
	if err != nil {
		return err
	}
	if err := validateEnumString(spf["status"], "spf status", "pass", "fail", "none", "neutral", "softfail", "temperror", "permerror"); err != nil {
		return err
	}
	if err := validateNullableString(spf["domain"], "spf domain"); err != nil {
		return err
	}
	if err := validateNullableBool(spf["aligned"], "spf aligned"); err != nil {
		return err
	}
	if err := validateOptionalString(spf, "detail", "spf detail"); err != nil {
		return err
	}

	var dkimItems []json.RawMessage
	if isJSONNull(root["dkim"]) {
		return fmt.Errorf("dkim must be an array")
	}
	if err := json.Unmarshal(root["dkim"], &dkimItems); err != nil {
		return fmt.Errorf("dkim must be an array")
	}
	for index, item := range dkimItems {
		name := fmt.Sprintf("dkim[%d]", index)
		dkim, err := validateAuthenticationObject(item, name,
			[]string{"status", "domain", "selector", "aligned", "detail"},
			[]string{"status", "domain", "selector", "aligned"},
		)
		if err != nil {
			return err
		}
		if err := validateEnumString(dkim["status"], name+" status", "pass", "fail", "none", "neutral", "policy", "temperror", "permerror"); err != nil {
			return err
		}
		if err := validateNullableString(dkim["domain"], name+" domain"); err != nil {
			return err
		}
		if err := validateNullableString(dkim["selector"], name+" selector"); err != nil {
			return err
		}
		if err := validateNullableBool(dkim["aligned"], name+" aligned"); err != nil {
			return err
		}
		if err := validateOptionalString(dkim, "detail", name+" detail"); err != nil {
			return err
		}
	}

	dmarc, err := validateAuthenticationObject(root["dmarc"], "dmarc",
		[]string{"status", "domain", "policy", "aligned_by", "detail"},
		[]string{"status", "domain", "policy", "aligned_by"},
	)
	if err != nil {
		return err
	}
	if err := validateEnumString(dmarc["status"], "dmarc status", "pass", "fail", "none", "temperror", "permerror"); err != nil {
		return err
	}
	if err := validateNullableString(dmarc["domain"], "dmarc domain"); err != nil {
		return err
	}
	if !isJSONNull(dmarc["policy"]) {
		if err := validateEnumString(dmarc["policy"], "dmarc policy", "none", "quarantine", "reject"); err != nil {
			return err
		}
	}
	if err := validateAlignedBy(dmarc["aligned_by"]); err != nil {
		return err
	}
	if err := validateOptionalString(dmarc, "detail", "dmarc detail"); err != nil {
		return err
	}
	return nil
}

func validateAuthenticationObject(encoded json.RawMessage, name string, allowedKeys, requiredKeys []string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	allowed := make(map[string]bool, len(allowedKeys))
	for _, key := range allowedKeys {
		allowed[key] = true
	}
	for key := range object {
		if !allowed[key] {
			return nil, fmt.Errorf("%s key %q is not allowed", name, key)
		}
	}
	for _, key := range requiredKeys {
		if _, ok := object[key]; !ok {
			return nil, fmt.Errorf("%s key %q is required", name, key)
		}
	}
	return object, nil
}

func validateEnumString(encoded json.RawMessage, name string, allowed ...string) error {
	value, err := requiredJSONString(encoded, name)
	if err != nil {
		return err
	}
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("%s has invalid value %q", name, value)
}

func validateNullableString(encoded json.RawMessage, name string) error {
	if isJSONNull(encoded) {
		return nil
	}
	_, err := requiredJSONString(encoded, name)
	return err
}

func validateNullableBool(encoded json.RawMessage, name string) error {
	if isJSONNull(encoded) {
		return nil
	}
	var value bool
	if err := json.Unmarshal(encoded, &value); err != nil {
		return fmt.Errorf("%s must be a boolean or null", name)
	}
	return nil
}

func validateOptionalString(object map[string]json.RawMessage, key, name string) error {
	encoded, ok := object[key]
	if !ok {
		return nil
	}
	_, err := requiredJSONString(encoded, name)
	return err
}

func requiredJSONString(encoded json.RawMessage, name string) (string, error) {
	if isJSONNull(encoded) {
		return "", fmt.Errorf("%s must be a string", name)
	}
	var value string
	if err := json.Unmarshal(encoded, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return value, nil
}

func validateAlignedBy(encoded json.RawMessage) error {
	if isJSONNull(encoded) {
		return fmt.Errorf("dmarc aligned_by must be an array")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(encoded, &items); err != nil {
		return fmt.Errorf("dmarc aligned_by must be an array")
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		value, err := requiredJSONString(item, "dmarc aligned_by item")
		if err != nil {
			return err
		}
		if value != "spf" && value != "dkim" {
			return fmt.Errorf("dmarc aligned_by has invalid value %q", value)
		}
		if seen[value] {
			return fmt.Errorf("dmarc aligned_by contains duplicate %q", value)
		}
		seen[value] = true
	}
	return nil
}

func isJSONNull(encoded json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(encoded), []byte("null"))
}

func validateNestedStrings(value any) error {
	switch value := value.(type) {
	case string:
		if len(value) > maxDiagnosticStringBytes {
			return fmt.Errorf("string exceeds 2 KiB")
		}
	case []any:
		for _, item := range value {
			if err := validateNestedStrings(item); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range value {
			if err := validateNestedStrings(item); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatedCorrelationCopy(correlationIDs map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(correlationIDs))
	for key, value := range correlationIDs {
		if !allowedCorrelationKeys[key] {
			return nil, fmt.Errorf("correlation key %q is not allowed", key)
		}
		if len(value) > maxDiagnosticStringBytes {
			return nil, fmt.Errorf("correlation value %q exceeds 2 KiB", key)
		}
		if value == "" {
			continue
		}
		result[key] = value
	}
	return result, nil
}

// SafeCorrelationIDs keeps independently valid optional correlations and drops
// unknown, empty, or oversized values. Producers use it for identifiers derived
// from remote input so an unsafe optional correlation can never reject the
// lifecycle fact it merely annotates.
func SafeCorrelationIDs(correlationIDs map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range correlationIDs {
		validated, err := validatedCorrelationCopy(map[string]string{key: value})
		if err != nil {
			continue
		}
		for validatedKey, validatedValue := range validated {
			result[validatedKey] = validatedValue
		}
	}
	return result
}

// SafeAuthenticationEvidence returns canonical authentication evidence that is
// safe to append even when diagnostic fields originated in untrusted SMTP/DNS
// input. In-bounds authentication JSON is preserved exactly. Oversized optional
// diagnostics are omitted, oversized nullable identifiers become null, and
// excess DKIM observations are dropped from the tail deterministically until the
// complete evidence fits the canonical aggregate limit.
//
// The input is intentionally any rather than emailauth.Authentication so this
// package remains the contract-owning leaf and does not depend on an auth
// producer package.
func SafeAuthenticationEvidence(authentication any) (map[string]any, error) {
	encoded, err := json.Marshal(authentication)
	if err != nil {
		return nil, fmt.Errorf("marshal authentication evidence: %w", err)
	}
	var sanitized map[string]any
	if err := json.Unmarshal(encoded, &sanitized); err != nil {
		return nil, fmt.Errorf("decode authentication evidence: %w", err)
	}

	spf, _ := sanitized["spf"].(map[string]any)
	sanitizeNullableDiagnostic(spf, "domain")
	sanitizeOptionalDiagnostic(spf, "detail")
	dmarc, _ := sanitized["dmarc"].(map[string]any)
	sanitizeNullableDiagnostic(dmarc, "domain")
	sanitizeOptionalDiagnostic(dmarc, "detail")

	dkim, _ := sanitized["dkim"].([]any)
	for _, raw := range dkim {
		item, _ := raw.(map[string]any)
		sanitizeNullableDiagnostic(item, "domain")
		sanitizeNullableDiagnostic(item, "selector")
		sanitizeOptionalDiagnostic(item, "detail")
	}
	sanitized["dkim"] = dkim
	evidence := map[string]any{"authentication": sanitized}
	for {
		encodedEvidence, marshalErr := json.Marshal(evidence)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal sanitized authentication evidence: %w", marshalErr)
		}
		if len(encodedEvidence) <= maxEvidenceBytes {
			break
		}
		if len(dkim) == 0 {
			return nil, fmt.Errorf("authentication evidence cannot fit canonical limit")
		}
		dkim = dkim[:len(dkim)-1]
		sanitized["dkim"] = dkim
	}

	validated, err := validatedEvidenceCopy(evidence)
	if err != nil {
		return nil, fmt.Errorf("validate sanitized authentication evidence: %w", err)
	}
	return validated, nil
}

func sanitizeNullableDiagnostic(object map[string]any, key string) {
	if object == nil {
		return
	}
	value, exists := object[key]
	if !exists || value == nil {
		return
	}
	text, ok := value.(string)
	if !ok || len(text) > maxDiagnosticStringBytes {
		object[key] = nil
	}
}

func sanitizeOptionalDiagnostic(object map[string]any, key string) {
	if object == nil {
		return
	}
	value, exists := object[key]
	if !exists {
		return
	}
	text, ok := value.(string)
	if !ok || len(text) > maxDiagnosticStringBytes {
		delete(object, key)
	}
}
