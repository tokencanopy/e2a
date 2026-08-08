package httpapi

import (
	"strconv"
	"strings"
)

// errorCodeContract is the canonical metadata for a known stable error code.
// The set remains open to consumers; adding a code is additive, while changing
// an existing entry requires an explicit contract review.
type errorCodeContract struct {
	Code          string
	Status        string
	Family        string
	Retryable     bool
	FallbackOnly  bool
	DetailsSchema string
}

var errorCodeCatalog = []errorCodeContract{
	{Code: "unauthorized", Status: "401", Family: "auth"},
	{Code: "forbidden", Status: "403", Family: "auth"},
	{Code: "blocked_by_policy", Status: "403", Family: "auth"},
	{Code: "invalid_request", Status: "400 / 422", Family: "validation", DetailsSchema: "ValidationErrorDetails"},
	{Code: "invalid_cursor", Status: "400", Family: "validation"},
	{Code: "invalid_filter", Status: "400", Family: "validation"},
	{Code: "invalid_domain", Status: "400", Family: "validation"},
	{Code: "invalid_slug", Status: "400", Family: "validation"},
	{Code: "invalid_recipient", Status: "400", Family: "validation"},
	{Code: "invalid_attachment", Status: "400", Family: "validation"},
	{Code: "invalid_template", Status: "400", Family: "validation"},
	{Code: "invalid_event_type", Status: "400", Family: "validation"},
	{Code: "invalid_webhook_url", Status: "400", Family: "validation"},
	{Code: "invalid_expires_at", Status: "400", Family: "validation"},
	{Code: "invalid_scope", Status: "400", Family: "validation"},
	{Code: "reserved_domain", Status: "400", Family: "validation"},
	{Code: "too_many_recipients", Status: "400", Family: "validation", DetailsSchema: "TooManyRecipientsDetails"},
	{Code: "template_render_failed", Status: "400", Family: "validation"},
	{Code: "template_rendered_empty", Status: "400", Family: "validation"},
	{Code: "recipient_suppressed", Status: "422", Family: "validation"},
	{Code: "not_found", Status: "404", Family: "not_found"},
	{Code: "attachment_not_found", Status: "404", Family: "not_found"},
	{Code: "template_not_found", Status: "404", Family: "not_found"},
	{Code: "contact_not_found", Status: "404", Family: "not_found"},
	{Code: "import_batch_not_found", Status: "404", Family: "not_found"},
	{Code: "engagement_not_found", Status: "404", Family: "not_found"},
	{Code: "starter_template_not_found", Status: "404", Family: "not_found"},
	{Code: "gone", Status: "410", Family: "not_found"},
	{Code: "conflict", Status: "409", Family: "state"},
	{Code: "precondition_failed", Status: "412", Family: "state"},
	{Code: "agent_taken", Status: "409", Family: "state"},
	{Code: "domain_taken", Status: "409", Family: "state"},
	{Code: "alias_taken", Status: "409", Family: "state"},
	{Code: "address_in_trash", Status: "409", Family: "state"},
	{Code: "message_held", Status: "409", Family: "state"},
	{Code: "message_not_pending", Status: "409", Family: "state"},
	// Retryable: the referenced outbound parent is queued for provider submission
	// but not yet submitted, so it has no Message-ID to thread onto yet. Once the
	// send worker submits, the identical reply/forward succeeds.
	{Code: "message_not_yet_delivered", Status: "409", Family: "state", Retryable: true},
	{Code: "not_in_trash", Status: "409", Family: "state"},
	{Code: "send_in_progress", Status: "409", Family: "state"},
	{Code: "webhook_disabled", Status: "409", Family: "state"},
	{Code: "webhook_cooldown", Status: "409", Family: "state"},
	{Code: "domain_not_registered", Status: "400", Family: "state"},
	{Code: "domain_has_agents", Status: "400", Family: "state"},
	{Code: "domain_not_verified", Status: "400 / 403", Family: "state"},
	{Code: "inbound_mx_missing", Status: "400", Family: "state"},
	{Code: "limit_exceeded", Status: "402", Family: "capacity", DetailsSchema: "LimitExceededDetails"},
	{Code: "rate_limited", Status: "429", Family: "capacity", Retryable: true, DetailsSchema: "RateLimitedDetails"},
	{Code: "template_limit_reached", Status: "400", Family: "capacity"},
	{Code: "contact_limit_reached", Status: "400", Family: "capacity"},
	{Code: "webhook_limit_reached", Status: "400", Family: "capacity"},
	{Code: "idempotency_in_flight", Status: "409", Family: "idempotency", Retryable: true},
	{Code: "idempotency_key_reuse", Status: "422", Family: "idempotency"},
	{Code: "payload_too_large", Status: "413", Family: "size", DetailsSchema: "PayloadTooLargeDetails"},
	{Code: "attachment_too_large", Status: "413", Family: "size"},
	{Code: "not_implemented", Status: "501", Family: "availability"},
	{Code: "events_log_disabled", Status: "501", Family: "availability"},
	{Code: "limits_unavailable", Status: "503", Family: "availability", Retryable: true, DetailsSchema: "RetryAfterDetails"},
	{Code: "inbound_mx_check_failed", Status: "503", Family: "availability", Retryable: true},
	{Code: "internal_error", Status: "5xx", Family: "server", Retryable: true},
	{Code: "method_not_allowed", Status: "405", Family: "server"},
	{Code: "unsupported_media_type", Status: "415", Family: "server", FallbackOnly: true},
	{Code: "error", Status: "other 4xx", Family: "server", FallbackOnly: true},
}

func catalogCodes(fallbackOnly bool) []string {
	codes := make([]string, 0, len(errorCodeCatalog))
	for _, entry := range errorCodeCatalog {
		if entry.FallbackOnly == fallbackOnly {
			codes = append(codes, entry.Code)
		}
	}
	return codes
}

func contractStatuses(status string) []any {
	parts := strings.Split(status, " / ")
	values := make([]any, 0, len(parts))
	for _, part := range parts {
		if value, err := strconv.Atoi(part); err == nil {
			values = append(values, value)
		} else {
			values = append(values, part)
		}
	}
	return values
}
