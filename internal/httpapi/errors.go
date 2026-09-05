// Package httpapi is the e2a v1 HTTP contract layer, built on Huma + chi.
//
// It exists to make the OpenAPI 3.1 spec the single source of truth: every
// operation is declared with typed Go input/output structs, and Huma emits
// the spec *and* validates requests from those same definitions, so the
// handler is the contract and the spec cannot drift by construction
// (api-v1-redesign §6). This package is the foundation slice (Slice 1):
// the canonical error envelope, cursor pagination, idempotency, and shared
// middleware that every ported operation reuses.
//
// chi owns the `/v1` prefix and falls back to the legacy gorilla/mux for the
// remaining non-v1 routes (OAuth, session auth, health/feedback, the magic-link
// approve/reject pages). The `/api/v1` surface this strangler replaced is fully
// retired — no `/api/v1` route is registered anymore.
package httpapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
)

// ErrorEnvelope is the one error shape across every v1 endpoint
// (api-v1-redesign §4 decision 6):
//
//	{ "error": { "code": "machine_branchable", "message": "human text",
//	             "details": {…}, "request_id": "req_…" } }
//
// `code` is the stable, machine-branchable discriminator agents switch on;
// `message` is human-facing; `details` is optional structured context; and
// `request_id` echoes the per-request id (also on the X-Request-Id header)
// so a failing call is greppable in logs without correlation guesswork.
//
// It implements huma.StatusError so it can be returned directly from a
// handler and is installed as the global huma.NewError constructor, which
// means Huma's own validation/automatic errors render in this envelope too.
type ErrorEnvelope struct {
	// status is the HTTP status; unexported so it never serializes into
	// the body (the status already rides the status line).
	status int

	// retryAfter, when > 0, is the seconds value stampRequestID copies into the
	// Retry-After response header. A StatusError returned from a handler renders
	// status + body only, so a 429 raised inside a handler (the per-agent send
	// limiter) carries its Retry-After here; the middleware limiter path sets
	// the header directly instead. Unexported so it never serializes.
	retryAfter int

	Err ErrorBody `json:"error"`
}

// ErrorBody is the inner object of the envelope.
type ErrorBody struct {
	Code      string `json:"code" doc:"Machine-branchable error code — the stable discriminator clients switch on. Open set: treat it as a string and tolerate unknown values, since new codes may be added over time (branch on the ones you handle, fall back to the HTTP status otherwise). Exact current vocabulary (machine-checked): unauthorized, forbidden, blocked_by_policy, sending_paused, invalid_request, invalid_cursor, invalid_filter, invalid_domain, invalid_slug, invalid_recipient, invalid_attachment, invalid_template, invalid_event_type, invalid_webhook_url, invalid_expires_at, invalid_scope, reserved_domain, too_many_recipients, template_render_failed, template_rendered_empty, recipient_suppressed, not_found, attachment_not_found, contact_not_found, engagement_not_found, import_batch_not_found, template_not_found, starter_template_not_found, gone, conflict, precondition_failed, agent_taken, domain_taken, alias_taken, address_in_trash, message_held, message_not_pending, message_not_yet_delivered, not_in_trash, purge_in_progress, send_in_progress, webhook_disabled, webhook_cooldown, domain_not_registered, domain_has_agents, domain_not_verified, inbound_mx_missing, limit_exceeded, rate_limited, contact_limit_reached, template_limit_reached, webhook_limit_reached, idempotency_in_flight, idempotency_key_reuse, payload_too_large, attachment_too_large, not_implemented, events_log_disabled, limits_unavailable, inbound_mx_check_failed, auth_unavailable, internal_error, method_not_allowed, unsupported_media_type, error. Grouped semantics: auth: unauthorized (401), forbidden (403), blocked_by_policy (403, outbound policy gate; experimental), sending_paused (403, outbound sending is paused for the account by the platform's abuse controls; queued mail is held, new sends are refused until an operator resumes). Validation: invalid_request is the single canonical code for input-validation failures whether they arrive as 400 (malformed) or 422 (semantically invalid); field/resource-specific invalid_* refinements (invalid_cursor, invalid_filter, invalid_domain, invalid_slug, invalid_recipient, invalid_attachment, invalid_template, invalid_event_type, invalid_webhook_url, invalid_expires_at, invalid_scope), reserved_domain, too_many_recipients, template_render_failed, template_rendered_empty (all 400); recipient_suppressed (422). Not found: not_found (404) plus the *_not_found family (attachment_not_found, contact_not_found, engagement_not_found, import_batch_not_found, template_not_found, starter_template_not_found); gone (410, past retention). Conflict/state: conflict (409, generic), precondition_failed (412, optimistic-concurrency validator is stale), the *_taken family — the requested identifier is already claimed — (agent_taken, domain_taken, alias_taken, all 409), address_in_trash (409), message_held (409), message_not_pending (409), message_not_yet_delivered (409, retry after the source outbound message is sent), not_in_trash (409), purge_in_progress (409, permanent delete already claimed), send_in_progress (409), webhook_disabled (409), webhook_cooldown (409), domain_not_registered (400), domain_has_agents (400), domain_not_verified (400 on create-agent, 403 on send), inbound_mx_missing (400). Capacity: limit_exceeded (402, plan quota — see LimitExceededDetails), rate_limited (429, request rate — see RateLimitedDetails), contact_limit_reached, template_limit_reached and webhook_limit_reached (400, fixed per-account caps). Idempotency: idempotency_in_flight (409, wait then retry the byte-identical request), idempotency_key_reuse (422, caller bug — do not retry as-is). Size: payload_too_large (413, request body), attachment_too_large (413, inline fetch over the cap — use download_url). Availability: not_implemented (501, feature not available on this deployment), events_log_disabled (501), limits_unavailable (503), inbound_mx_check_failed (503), auth_unavailable (503, an auth backend — e.g. a delegated-token verifier or the identity store — could not judge the credential; retry). Server/fallback: internal_error (5xx), method_not_allowed (405), unsupported_media_type (415), and the generic code error for any otherwise-unmapped status."`
	Message   string `json:"message" doc:"Human-readable explanation. Not for branching — use code."`
	Details   any    `json:"details,omitempty" doc:"Optional structured context, polymorphic by code. Treat it as an open object keyed off code; unknown codes and fields must be preserved."`
	RequestID string `json:"request_id" doc:"Echoes the X-Request-Id response header so a failing call is greppable in logs."`
}

// FieldError is one per-field validation failure inside ValidationErrorDetails.
// It deliberately omits the raw offending value (Huma's ErrorDetail.Value) from
// the public contract so the API never echoes bad input back to the client.
type FieldError struct {
	Location string `json:"location" doc:"Path-like pointer to the offending field, prefixed with the request part it came from, e.g. body.events, body.items[3].tags, query.limit, path.id. Empty when the failure is not tied to a single field."`
	Message  string `json:"message" doc:"Human-readable reason this field is invalid."`
}

// ValidationErrorDetails is the typed error.details payload for the
// invalid_request code: the list of per-field validation failures that made the
// request invalid. It is the validation arm of the polymorphic-by-code details
// contract (other codes carry their own typed detail object — e.g. a
// limit_exceeded detail for the 402 quota path), so codegen emits a concrete
// model clients can read after branching on code == "invalid_request".
type ValidationErrorDetails struct {
	Fields []FieldError `json:"fields" nullable:"false" doc:"The fields that failed validation. May be empty when the failure is request-wide rather than field-specific."`
}

// TransformSchema publishes known-code metadata without closing either code or
// details. A generic client sees a string and open object; tooling that
// understands the extensions can offer stable typed views for known codes.
func (ErrorBody) TransformSchema(r huma.Registry, s *huma.Schema) *huma.Schema {
	detailTypes := map[string]reflect.Type{
		"ValidationErrorDetails":   reflect.TypeOf(ValidationErrorDetails{}),
		"TooManyRecipientsDetails": reflect.TypeOf(TooManyRecipientsDetails{}),
		"PayloadTooLargeDetails":   reflect.TypeOf(PayloadTooLargeDetails{}),
		"LimitExceededDetails":     reflect.TypeOf(LimitExceededDetails{}),
		"RateLimitedDetails":       reflect.TypeOf(RateLimitedDetails{}),
		"RetryAfterDetails":        reflect.TypeOf(RetryAfterDetails{}),
	}
	for name, typ := range detailTypes {
		r.Schema(typ, true, name)
	}
	seen := map[string]bool{}
	for name := range detailTypes {
		openResponseComponent(r, name, seen)
	}
	if code := s.Properties["code"]; code != nil {
		if code.Extensions == nil {
			code.Extensions = map[string]any{}
		}
		contracts := make(map[string]any, len(errorCodeCatalog))
		for _, entry := range errorCodeCatalog {
			metadata := map[string]any{
				"statuses":  contractStatuses(entry.Status),
				"family":    entry.Family,
				"retryable": entry.Retryable,
			}
			if entry.DetailsSchema != "" {
				metadata["details_schema"] = "#/components/schemas/" + entry.DetailsSchema
			}
			contracts[entry.Code] = metadata
		}
		code.Extensions["x-e2a-error-contracts"] = contracts
	}
	if details := s.Properties["details"]; details != nil {
		details.Type = huma.TypeObject
		details.AnyOf = nil
		details.OneOf = nil
		details.AdditionalProperties = true
		if details.Extensions == nil {
			details.Extensions = map[string]any{}
		}
		mapping := map[string]any{}
		for _, entry := range errorCodeCatalog {
			if entry.DetailsSchema != "" {
				mapping[entry.Code] = "#/components/schemas/" + entry.DetailsSchema
			}
		}
		details.Extensions["x-e2a-error-details-schemas"] = mapping
	}
	return s
}

type TooManyRecipientsDetails struct {
	MaxRecipients int `json:"max_recipients" minimum:"1" doc:"Maximum recipients allowed across to, cc, and bcc."`
	Provided      int `json:"provided" minimum:"1" doc:"Combined recipient count supplied by the caller."`
}

type PayloadTooLargeDetails struct {
	// Scope is an OPEN set (evolving response-side vocabulary): a new byte
	// budget (e.g. a future batch or template cap) means a new value here,
	// and that must not break spec-generated clients.
	Scope       string `json:"scope" doc:"Which byte budget was exceeded. Open set: new values may be added over time, so treat these as strings and tolerate unknown values. Known values: composed_message, attachment, attachments_total, request_body."`
	ActualBytes int64  `json:"actual_bytes" minimum:"0" doc:"Observed byte count. Exact when Content-Length or decoded content is available; for chunked request bodies this is the lower bound observed before rejection."`
	MaxBytes    int64  `json:"max_bytes" minimum:"1" doc:"Maximum bytes accepted for this scope."`
	Filename    string `json:"filename,omitempty" doc:"Attachment filename when scope is attachment."`
}

// LimitExceededDetails is the typed `error.details` payload carried by a 402
// limit_exceeded response. `resource` is one of the AccountView usage/limits
// field stems, so a client can key the error straight to the usage/cap field:
// usage.<resource> for the current value and limits.max_<resource> for the cap
// (e.g. resource "messages_month" → usage.messages_month / limits.max_messages_month).
// `limit` and `current` echo the cap that was hit and the account's usage at the
// time. `plan_code`/`upgrade_url` are the account's plan label and any upgrade
// affordance the operator configured.
type LimitExceededDetails struct {
	// Resource is an OPEN set (evolving response-side vocabulary): a new
	// capped resource means a new value here, and that must not break
	// spec-generated clients.
	Resource   string `json:"resource" doc:"The capped resource stem. For stems with AccountView fields, key it to usage.<resource> and limits.max_<resource>. Open set: new values may be added over time, so treat these as strings and tolerate unknown values. Known values: agents, domains, messages_month, storage_bytes, messages_day (per-UTC-day send cap; no AccountView field — resets at midnight UTC)."`
	Limit      int64  `json:"limit" doc:"The cap that was hit (matches limits.max_<resource>)."`
	Current    int64  `json:"current" doc:"The account's usage at the time the cap was hit (matches usage.<resource>)."`
	PlanCode   string `json:"plan_code,omitempty" doc:"The account's plan label."`
	UpgradeURL string `json:"upgrade_url,omitempty" doc:"An upgrade affordance URL, when the operator has configured one."`
}

// LimitExceededErrorBody mirrors ErrorBody but with typed limit_exceeded details,
// so codegen surfaces a concrete detail shape for the 402 case instead of `any`.
type LimitExceededErrorBody struct {
	Code      string               `json:"code" enum:"limit_exceeded" doc:"Always limit_exceeded for this response."`
	Message   string               `json:"message"`
	Details   LimitExceededDetails `json:"details"`
	RequestID string               `json:"request_id"`
}

// LimitExceededEnvelope is the 402 error envelope with typed details. It is the
// declared schema for the 402 response on the cap-enforcing operations (create
// agent, register domain, send/reply/forward/test); the runtime envelope is the
// generic ErrorEnvelope whose `details` is populated with a LimitExceededDetails
// value, so the wire shape matches this schema byte-for-byte.
type LimitExceededEnvelope struct {
	Err LimitExceededErrorBody `json:"error"`
}

// RateLimitedDetails is the typed `error.details` payload carried by a 429
// rate_limited response. `retry_after_seconds` is the seconds a client should
// wait before retrying — it mirrors the Retry-After response header, so a
// client that can only read the body still gets the backoff hint.
//
// This is the THROUGHPUT/request-RATE arm of the contract, distinct from the
// 402 limit_exceeded (stock/flow QUOTA) arm: a 429 is a short-lived, retry-able
// signal (wait retry_after_seconds and the same request succeeds), whereas a
// 402 is a persistent cap that a retry alone will not clear (see
// LimitExceededDetails). Clients MUST branch on the HTTP status: 429 → back off
// and retry; 402 → surface a quota/upgrade path, do not hammer-retry.
type RateLimitedDetails struct {
	RetryAfterSeconds int `json:"retry_after_seconds" doc:"Seconds to wait before retrying; mirrors the Retry-After response header. Always ≥ 1."`
}

// RetryAfterDetails is the open transient-availability retry hint used by
// limits_unavailable. It intentionally matches the rate-limit field name so
// generic clients can share one backoff parser across 429 and 503 responses.
type RetryAfterDetails struct {
	RetryAfterSeconds int `json:"retry_after_seconds" minimum:"1" doc:"Seconds to wait before retrying; mirrors the Retry-After response header."`
}

// RateLimitedErrorBody mirrors ErrorBody but with typed rate_limited details, so
// codegen surfaces a concrete detail shape for the 429 case instead of `any`.
type RateLimitedErrorBody struct {
	Code      string             `json:"code" enum:"rate_limited" doc:"Always rate_limited for this response."`
	Message   string             `json:"message"`
	Details   RateLimitedDetails `json:"details"`
	RequestID string             `json:"request_id"`
}

// RateLimitedEnvelope is the 429 error envelope with typed details. It is the
// declared schema for the 429 response on the throughput-limited write
// operations (send/reply/forward/test, create agent, approve review); the
// runtime envelope is the generic ErrorEnvelope whose `details` is populated
// with a RateLimitedDetails value (or the equivalent map), so the wire shape
// matches this schema byte-for-byte. It is the request-RATE counterpart to the
// 402 LimitExceededEnvelope (stock/flow QUOTA) — the two are the permanent GA
// split clients branch on by HTTP status.
type RateLimitedEnvelope struct {
	Err RateLimitedErrorBody `json:"error"`
}

// Error implements the error interface (huma.StatusError embeds error).
func (e *ErrorEnvelope) Error() string { return e.Err.Message }

// GetStatus implements huma.StatusError so Huma writes the right status.
func (e *ErrorEnvelope) GetStatus() int { return e.status }

// Code returns the machine-branchable code (used by tests and middleware).
func (e *ErrorEnvelope) Code() string { return e.Err.Code }

// NewError builds an envelope with an explicit machine-branchable code.
// Prefer this over the status-only helpers when the caller should be able
// to branch on something more specific than the HTTP status (e.g.
// "domain_not_verified" vs a bare 400).
func NewError(status int, code, message string) *ErrorEnvelope {
	env := &ErrorEnvelope{status: status, Err: ErrorBody{Code: code, Message: message}}
	if code == "invalid_request" {
		env.Err.Details = ValidationErrorDetails{Fields: []FieldError{{Location: "", Message: message}}}
	}
	return env
}

// WithDetails attaches structured details and returns the envelope for
// fluent construction.
func (e *ErrorEnvelope) WithDetails(details any) *ErrorEnvelope {
	e.Err.Details = details
	return e
}

// WithRetryAfter records a Retry-After delay (seconds) for a handler-returned
// error; stampRequestID copies it into the Retry-After response header. Use it
// on 429s raised inside a handler (the per-agent send limiter) — the
// middleware-enforced limiters set the header themselves.
func (e *ErrorEnvelope) WithRetryAfter(seconds int) *ErrorEnvelope {
	e.retryAfter = seconds
	return e
}

// defaultCodeForStatus maps an HTTP status to a stable default `code` for
// the cases where a handler (or Huma's built-in validation) produced only a
// status + message. Ported handlers should pass an explicit code via
// NewError; this is the fallback so every error still carries a non-empty,
// branchable code.
func defaultCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		// invalid_request is the single canonical code for input-validation
		// failures; a malformed 400 and a semantic 422 (below) share it so
		// clients branch on one code while the HTTP status still distinguishes
		// the two conditions.
		return "invalid_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestEntityTooLarge:
		return "payload_too_large"
	case http.StatusUnprocessableEntity:
		// Semantically-invalid input shares the canonical validation code with
		// 400; the 422 status still marks it as "well-formed but unprocessable".
		return "invalid_request"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "error"
	}
}

// humaErrorConstructor is installed as huma.NewError so that *all* errors —
// handler-returned, body-validation, content-negotiation — render in the
// e2a envelope. Huma passes the status, a message, and zero or more detail
// errors; we fold the detail errors into `details` so field-level
// validation failures survive.
func humaErrorConstructor(status int, message string, errs ...error) huma.StatusError {
	env := &ErrorEnvelope{
		status: status,
		Err: ErrorBody{
			Code:    defaultCodeForStatus(status),
			Message: message,
		},
	}
	if len(errs) > 0 {
		// huma.ErrorDetailer values carry structured field/location info. Fold
		// them into the typed ValidationErrorDetails shape ({fields:[{location,
		// message}]}) so the validation details are machine-readable AND match
		// the schema codegen emits. The raw offending value (ErrorDetail.Value)
		// is deliberately dropped from the public contract so we never echo bad
		// input back to the client.
		fields := make([]FieldError, 0, len(errs))
		for _, err := range errs {
			if err == nil {
				continue
			}
			if d, ok := err.(huma.ErrorDetailer); ok {
				det := d.ErrorDetail()
				fields = append(fields, FieldError{Location: det.Location, Message: det.Message})
			} else {
				fields = append(fields, FieldError{Message: err.Error()})
			}
		}
		if len(fields) > 0 {
			env.Err.Details = ValidationErrorDetails{Fields: fields}
		}
	}
	return env
}

// stampRequestID is a Huma transformer that copies the per-request id into
// the error envelope body just before serialization, so the body matches
// the X-Request-Id header (api-v1-redesign §4 — "echo the same id in the
// error envelope"). Success bodies are left untouched.
func stampRequestID(ctx huma.Context, status string, v any) (any, error) {
	env, ok := v.(*ErrorEnvelope)
	if !ok {
		return v, nil
	}
	if env.Err.RequestID == "" {
		env.Err.RequestID = RequestIDFromContext(ctx.Context())
	}
	if env.Err.Code == "payload_too_large" && env.Err.Details == nil {
		var maxBytes int64
		if operation := ctx.Operation(); operation != nil {
			maxBytes = operation.MaxBodyBytes
		}
		var actualBytes int64
		if request := RequestFromContext(ctx.Context()); request != nil {
			actualBytes = request.ContentLength
		}
		if actualBytes <= 0 {
			// Huma has already observed maxBytes bytes before returning 413. A
			// chunked request has no exact Content-Length, so publish that lower
			// bound rather than inventing an exact size.
			actualBytes = maxBytes
		}
		env.Err.Details = PayloadTooLargeDetails{
			Scope:       "request_body",
			ActualBytes: actualBytes,
			MaxBytes:    maxBytes,
		}
	}
	// A StatusError returned from a handler renders status + body only, so stamp
	// the Retry-After header here for rate-limit errors that carry a delay —
	// matching the middleware limiter path, which sets the header itself.
	if env.retryAfter > 0 {
		ctx.SetHeader("Retry-After", strconv.Itoa(env.retryAfter))
	}
	return v, nil
}

// writeRawEnvelope serializes an ErrorEnvelope to a raw (non-Huma)
// ResponseWriter, giving handlers that bypass Huma the SAME error contract every
// operation emits. It reuses the request id the requestID middleware already
// stamped (the production chi root always sets one, so header == body == what
// REST would return) and mints one only when absent (a direct call in a test),
// then mirrors it onto the X-Request-Id header and sets Content-Type:
// application/json before writing the status + body. This is the one place raw
// chi routes stay in lockstep with the Huma surface on the envelope shape.
// (The middleware path uses the huma.Context-based writeEnvelope in ratelimit.go.)
func writeRawEnvelope(w http.ResponseWriter, r *http.Request, env *ErrorEnvelope) {
	id := RequestIDFromContext(r.Context())
	if id == "" {
		id = newRequestID()
	}
	env.Err.RequestID = id
	w.Header().Set(requestIDHeader, id)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(env.status)
	_ = json.NewEncoder(w).Encode(env)
}

// WriteError writes the canonical v1 error envelope to a raw ResponseWriter for
// handlers OUTSIDE this package that bypass Huma — specifically the WebSocket
// upgrade handshake (internal/ws), which authenticates and authorizes BEFORE the
// upgrade and so rejects a bad handshake with a normal HTTP response. Routing
// those rejections through here makes the WS handshake body byte-for-byte
// consistent with every /v1 REST endpoint: {error:{code,message,request_id}} +
// X-Request-Id. The caller supplies the status and a code from the REST
// vocabulary (unauthorized / forbidden / not_found / invalid_request); status codes
// are the caller's to choose so this never rewrites them.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeRawEnvelope(w, r, NewError(status, code, message))
}

// installErrorEnvelope wires the envelope constructor globally. It is called
// once from New(); calling it is idempotent.
func installErrorEnvelope() {
	huma.NewError = humaErrorConstructor
}
