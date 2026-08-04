package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// jsonResponse builds an extra OpenAPI response entry for an operation whose
// handler can return a non-default status with the given body schema. The
// schema is registered (and reused via $ref) in the API's component registry,
// so the declared response stays in lockstep with the Go type — no hand-edits
// to api/openapi.yaml. Used to document the 202 (HITL hold) / 412 / 409 codes
// the typed handlers emit but Huma can't infer from the single DefaultStatus.
func (s *Server) jsonResponse(bodyType reflect.Type, schemaName, description string) *huma.Response {
	schema := s.API.OpenAPI().Components.Schemas.Schema(bodyType, true, schemaName)
	return &huma.Response{
		Description: description,
		Content: map[string]*huma.MediaType{
			"application/json": {Schema: schema},
		},
	}
}

// errorEnvelopeResponse is the generic `default` error response (ErrorEnvelope).
// Huma auto-adds it to every operation, but declaring a custom `Responses` map
// SUPPRESSES that auto default — so any op with a custom map must re-add this, or
// its OpenAPI contract omits the error shape and generated clients fall back to
// raw-string error bodies (losing the machine `code`; api-v1-redesign §6a #4).
func (s *Server) errorEnvelopeResponse() *huma.Response {
	return s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
		"Error — the standard envelope; branch on error.code.")
}

// limitExceededResponse is the typed 402 response for the cap-enforcing
// operations (create agent, register domain, send/reply/forward/test). Its
// schema is the LimitExceededEnvelope, whose error.details is a typed
// LimitExceededDetails so codegen surfaces a concrete shape (resource keyed to
// the AccountView usage/limits field stems) instead of a bare `any`.
func (s *Server) limitExceededResponse() *huma.Response {
	return s.jsonResponse(reflect.TypeOf(LimitExceededEnvelope{}), "LimitExceededEnvelope",
		"Payment required — a per-account resource cap was hit (code limit_exceeded). error.details.resource is the AccountView usage/limits field stem (agents, domains, messages_month, storage_bytes), so the client can key it to usage.<resource> / limits.max_<resource>. This is a QUOTA (stock/flow) cap — distinct from a 429 rate_limited (throughput). A retry alone will not clear it; surface a quota/upgrade path.")
}

// rateLimitedResponse is the typed 429 response for the throughput-limited write
// operations (send/reply/forward/test, create agent, approve review). Its schema
// is the RateLimitedEnvelope, whose error.details is a typed RateLimitedDetails
// (retry_after_seconds) so codegen surfaces a concrete backoff hint instead of a
// bare `any`. It is the request-RATE counterpart to limitExceededResponse (402
// QUOTA) — together they are the permanent GA split clients branch on by status.
func (s *Server) rateLimitedResponse() *huma.Response {
	return s.jsonResponse(reflect.TypeOf(RateLimitedEnvelope{}), "RateLimitedEnvelope",
		"Too Many Requests — a request-RATE / throughput limit was hit (code rate_limited). This is distinct from a 402 limit_exceeded (a QUOTA cap): a 429 is transient and retry-able — wait error.details.retry_after_seconds (mirrored on the Retry-After header), then the same request succeeds. Branch on the HTTP status: 429 → back off and retry; 402 → surface a quota/upgrade path.")
}

// idempotencyInFlightResponse is the declared 409 for every operation that
// honors the Idempotency-Key header
// (send/reply/forward/approve/rotate-secret/create-api-key):
// a request with the same key is still executing. It is RETRY-ABLE — the retry
// contract is the opposite of the 422 below, so the two must stay separately
// documented on every keyed operation.
func (s *Server) idempotencyInFlightResponse() *huma.Response {
	return s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
		"Conflict — code idempotency_in_flight: a request with this Idempotency-Key is still executing. Retry-able: wait for the first request to finish, then retry with the SAME key and byte-identical body — the retry replays the first request's response instead of re-executing the side effect.")
}

// idempotencyReuseResponse is the declared 422 for every Idempotency-Key
// operation: the key was already used with a DIFFERENT request (the dedup hash
// covers route + raw body bytes). This is the dangerous-retry case — the caller
// MUST NOT blind-retry, so the contract has to be declared, not just emitted.
func (s *Server) idempotencyReuseResponse() *huma.Response {
	return s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
		"Unprocessable — branch on error.code. idempotency_key_reuse: this Idempotency-Key was already used with a DIFFERENT request body (the dedup hash covers the route + the raw body bytes) — do NOT retry as-is; a legitimate retry must resend the byte-identical body, and a genuinely new request needs a fresh key. invalid_request: a semantic validation failure in the request body.")
}

// replyForwardConflictResponse is the declared 409 for reply/forward. They honor
// Idempotency-Key like /send, so idempotency_in_flight applies — but they alone
// also resolve a PARENT message, which adds message_not_yet_delivered. Both are
// retry-able and render the same envelope, so the two codes share one declared
// 409; a client branches on error.code.
func (s *Server) replyForwardConflictResponse() *huma.Response {
	return s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
		"Conflict — branch on error.code; both codes are retry-able. idempotency_in_flight: a request with this Idempotency-Key is still executing — wait for the first request to finish, then retry with the SAME key and byte-identical body to replay its response. message_not_yet_delivered: the referenced message is one this agent sent that is still queued for provider submission. A reply cannot thread until the provider assigns the source a Message-ID; a forward requires the source message to have actually been sent. Retry once it reaches status=sent (poll GET /v1/messages/{id} or await the email.sent event), or send the original with wait=sent so it is terminal before you reply to or forward it.")
}

const composedMessageCeilingDoc = "Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large."

// outboundPayloadTooLargeResponse documents both independent outbound size
// contracts. Attachment limits bound files individually and in aggregate; the
// lower composed-message ceiling bounds the final subject/body/decoded-file
// total. Direct send/reply/forward return the named composed-size detail keys.
func (s *Server) outboundPayloadTooLargeResponse() *huma.Response {
	return s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
		"Payload Too Large — error.code = payload_too_large. An attachment exceeds 10 MiB decoded; combined attachments exceed 25 MiB decoded; or the composed message exceeds 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes. error.details uses PayloadTooLargeDetails: {scope, actual_bytes, max_bytes, filename?}; scope identifies composed_message, attachment, attachments_total, or request_body.")
}

// SendResultView is the single outbound result for send/reply/forward/approve/
// test (MSG-9). Per scenario:
//   - sent:  status="sent" + message_id (the e2a msg_ id) + provider_message_id
//     (SES id) + sent_as + method.
//   - held:  status="pending_review" + message_id + approval_expires_at.
//   - approved+sent: the "sent" set + edited (reviewer edited the draft).
//
// message_id is always the e2a message id (GET-able), never the provider id —
// the SES id is provider_message_id. `reject` keeps its own RejectResultView
// (it is not a send).
type SendResultView struct {
	// review_approved is the inbound-release outcome of POST .../approve (an
	// inbound hold released to the agent's inbox — no send). sent/pending_review
	// are the send/outbound-approve outcomes.
	Status            string     `json:"status" doc:"Outcome. Open set; tolerate unknown values. Known values: accepted, scheduled, sent, pending_review, review_approved, failed. accepted = durably persisted and queued for immediate submission (async pipeline); the terminal outcome arrives via webhook events (email.sent / email.failed) or GET /v1/messages/{id}. scheduled is beta and may change before it is declared stable. scheduled = durably persisted and queued for future submission at scheduled_at; this is successful acceptance, so do not re-send. failed = terminal failure. Always branch on this field, not the HTTP status code."`
	MessageID         string     `json:"message_id"`
	ProviderMessageID string     `json:"provider_message_id,omitempty" doc:"Upstream provider (SES) id. Optional/absent until the message is actually sent — an accepted-but-not-yet-sent message has no provider id."`
	SentAs            string     `json:"sent_as,omitempty" doc:"From identity used. Open set; tolerate unknown values. Known values: own_address, relay."`
	Method            string     `json:"method,omitempty" doc:"Send transport. Open set; tolerate unknown values. Known values: smtp, loopback."`
	ScheduledAt       *time.Time `json:"scheduled_at,omitempty" format:"date-time" doc:"Beta: scheduled sending may change before it is declared stable. Set only when status=scheduled: the future instant this message is queued to be submitted (approximate — treat as \"not before\"). Moving the message to trash before provider submission starts prevents submission; if submission already has a fresh lease, delete returns 409 send_in_progress. Restoring before scheduled_at re-arms it; restoring at or after scheduled_at returns it live with delivery_status=failed and leaves the send canceled."`
	ApprovalExpiresAt *time.Time `json:"approval_expires_at,omitempty"`
	// Edited is set only by approve (true/false = did the reviewer edit the
	// draft before sending); omitted on the plain send path.
	Edited *bool `json:"edited,omitempty"`
}

// maxOutboundBytes is the coarse WIRE-size backstop on the outbound request body
// (send/reply/forward): Huma reads at most this many raw bytes before parsing, so
// an unbounded body can't exhaust memory. It is deliberately larger than the
// decoded attachment limits below, because attachment bytes arrive base64-encoded
// on the wire (~33% larger than decoded): a request at the 25 MB DECODED total is
// ~33.3 MB of base64 plus the JSON envelope and text body. 40 MB admits any valid
// request with headroom while the real ceilings — the per-attachment / total
// DECODED limits — are enforced after decode in validateAttachments. (The legacy
// 25 MB value was on the WIRE, so it silently rejected legitimately-sized
// attachment payloads; that raw-vs-decoded mismatch is reconciled here.)
const maxOutboundBytes = 40 * 1024 * 1024

// Attachment limits, enforced on DECODED bytes (not the base64 wire size) across
// every outbound path that accepts attachments — send, reply, forward, and an
// approve that edits the held draft's attachments. Conservative starting values
// (GA freeze): raising a limit later is non-breaking, lowering is breaking, so we
// start small and leave headroom under the downstream ceiling. The combined total
// (25 MB decoded ≈ 33.3 MB base64 in the composed MIME) stays safely under the
// AWS SES 40 MB per-message ceiling.
const (
	// maxAttachmentBytes caps a single attachment's decoded size. Over → 413.
	maxAttachmentBytes = 10 * 1024 * 1024
	// maxAttachmentCount caps how many attachments one message may carry. Over → 400.
	maxAttachmentCount = 10
	// maxAttachmentsTotalBytes caps the combined decoded size of all attachments on
	// one message. Over → 413. Aligned to the whole-request budget and kept under
	// the SES 40 MB encoded ceiling once base64-inflated.
	maxAttachmentsTotalBytes = 25 * 1024 * 1024
)

// Per-field GA contract limits on outbound request bodies. These are the named
// source of truth for maxLength struct tags on SendEmailRequest, ReplyRequest,
// ForwardRequest, and agent.ApproveOverrides. Struct tags use numeric literals,
// so TestOutboundFieldLimitTagsMatchConsts guards against drift. Recipient count
// is handler-validated separately so every distribution across to/cc/bcc returns
// the same structured too_many_recipients error.
const (
	// maxSubjectLen caps a single subject line. Enforced via maxLength struct tags.
	maxSubjectLen = 2000
	// maxBodyFieldBytes caps a single text/html body field (1 MiB). Enforced via
	// maxLength struct tags. Distinct from the composed-message ceiling below.
	maxBodyFieldBytes = 1 << 20
	// maxComposedMessageBytes is the hard cap on a composed outbound message —
	// subject + text + html + DECODED attachments. The SES v1 stored-message
	// ceiling (the real upstream limit). Enforced at runtime by
	// composedMessageSizeError; over → 413 payload_too_large. Aliased to the
	// canonical outbound.MaxComposedMessageBytes so this and the HITL approve-
	// override path (internal/agent) share one source of truth.
	maxComposedMessageBytes = outbound.MaxComposedMessageBytes

	// maxConversationIDLen caps a caller-supplied conversation_id, in Unicode
	// code points (the maxLength semantics Huma enforces). Deliberately equal
	// to the webhook filter-value cap (webhookMaxFilterValueLen = 200) and the
	// message-list filter cap (maxFilterStr = 200) so EVERY accepted
	// conversation_id remains usable as a filter value. Enforced declaratively
	// via maxLength struct tags AND at runtime by validateConversationID (the
	// shared backstop for non-schema paths).
	maxConversationIDLen = 200

	// maxEmailAddressLen caps any single email-address-bearing request string —
	// a to/cc/bcc recipient item (display name + <addr> combined), a reply_to
	// override, or an agent email — in Unicode code points. Aliased to the
	// canonical agent.MaxAddressLen so the schema tags and the runtime
	// recipient validation (agent.ValidateRecipients) share one source of
	// truth. On array fields the maxLength tag applies to EACH ITEM (Huma puts
	// scalar constraints on the items schema).
	maxEmailAddressLen = agent.MaxAddressLen
)

// maxRecipients caps the combined to+cc+bcc fan-out of a single outbound
// message. A body-size ceiling alone doesn't bound recipient count, so a tiny
// body could still address thousands of addresses; this keeps a single send
// from becoming a blast. Over the cap is a 400 too_many_recipients.
const maxRecipients = 50

// maxReplyToAddresses caps how many addresses a single reply_to may name.
// Reply-To is a routing hint (RFC 5322 allows an address-list), not a delivery
// fan-out mechanism — to/cc/bcc are — so a small cap is deliberate. Over the
// cap is a 400 invalid_request.
const maxReplyToAddresses = 5

// recipientCountError returns a too_many_recipients envelope when the combined
// to+cc+bcc count exceeds maxRecipients, else nil.
func recipientCountError(groups ...[]string) *ErrorEnvelope {
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	if total > maxRecipients {
		return NewError(http.StatusBadRequest, "too_many_recipients",
			"too many recipients — at most 50 across to, cc and bcc combined").
			WithDetails(TooManyRecipientsDetails{MaxRecipients: maxRecipients, Provided: total})
	}
	return nil
}

// maxScheduleHorizon caps how far into the future a send_at may be scheduled.
// River retains a `scheduled` job indefinitely (pruning only touches finalized
// jobs), so an unbounded send_at is a footgun that also stretches the
// trash-cancel window; 90 days is a generous planning horizon well short of
// "forever".
const maxScheduleHorizon = 90 * 24 * time.Hour

const scheduledSendBetaDoc = "Beta: scheduled sending may change before it is declared stable."

// scheduledInstant validates a caller-supplied send_at and returns the effective
// future schedule instant (UTC), or nil to send immediately. A nil/zero value or
// one at/before `now` is immediate (nil, nil). A value beyond maxScheduleHorizon
// is rejected with 400 invalid_request. Keeping the "past = immediate" rule (vs
// a hard error) is deliberate: minor client/server clock skew shouldn't turn an
// intended-now send into an error.
func scheduledInstant(sendAt *time.Time, now time.Time) (*time.Time, *ErrorEnvelope) {
	if sendAt == nil || sendAt.IsZero() {
		return nil, nil
	}
	if !sendAt.After(now) {
		return nil, nil // at/before now — send immediately
	}
	if sendAt.After(now.Add(maxScheduleHorizon)) {
		return nil, NewError(http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("send_at is too far in the future — at most %d days ahead", int(maxScheduleHorizon/(24*time.Hour))))
	}
	at := sendAt.UTC()
	return &at, nil
}

// SendEmailRequest is the new-thread send body. `to` is required (RFC 5321
// requires ≥1 recipient; From/Date are server-set). Content comes in one of
// two mutually exclusive shapes: literal subject + text (+ optional
// html) — both required at the handler for a usable new email (MSG-3) —
// or a template reference (template_id XOR template_alias, + template_data),
// which the server renders into subject/text/html before any further
// processing. subject/text moved from schema-required to handler-enforced so
// the template shape can omit them.
type SendEmailRequest struct {
	To             []string              `json:"to" nullable:"false" maxLength:"320" doc:"Primary recipients. The message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters. The address itself must also fit SMTP's mailbox octet limits — local part at most 64 octets and the whole addr-spec at most 254 octets, counted in UTF-8 BYTES rather than characters — or the request is rejected with 400 invalid_recipient. A long plus-addressed local part is the usual way to exceed this."`
	CC             []string              `json:"cc,omitempty" nullable:"false" maxLength:"320" doc:"Cc recipients. The message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters. The address itself must also fit SMTP's mailbox octet limits — local part at most 64 octets and the whole addr-spec at most 254 octets, counted in UTF-8 BYTES rather than characters — or the request is rejected with 400 invalid_recipient. A long plus-addressed local part is the usual way to exceed this."`
	BCC            []string              `json:"bcc,omitempty" nullable:"false" maxLength:"320" doc:"Bcc recipients. The message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters. The address itself must also fit SMTP's mailbox octet limits — local part at most 64 octets and the whole addr-spec at most 254 octets, counted in UTF-8 BYTES rather than characters — or the request is rejected with 400 invalid_recipient. A long plus-addressed local part is the usual way to exceed this."`
	Subject        string                `json:"subject,omitempty" maxLength:"2000" doc:"Literal subject. Required unless a template reference is used (mutually exclusive with template_id/template_alias)."`
	Body           string                `json:"text,omitempty" maxLength:"1048576" doc:"Literal plain-text body. Required unless a template reference is used (mutually exclusive with template_id/template_alias)."`
	HTMLBody       string                `json:"html,omitempty" maxLength:"1048576" doc:"Literal HTML body. Mutually exclusive with template_id/template_alias."`
	TemplateID     string                `json:"template_id,omitempty" doc:"Send using a stored template (rendered server-side, before any review hold). Mutually exclusive with template_alias and with literal subject/text/html. Beta: templates are unstable — their shape may change before they are declared stable."`
	TemplateAlias  string                `json:"template_alias,omitempty" doc:"Send using a stored template resolved by its per-user alias. Mutually exclusive with template_id and with literal subject/text/html. Beta: templates are unstable — their shape may change before they are declared stable."`
	TemplateData   TemplateData          `json:"template_data,omitempty" doc:"Variables for the referenced template ({{name}}, dot paths into nested objects). Missing variables render as empty strings. Beta: templates are unstable — their shape may change before they are declared stable."`
	ConversationID string                `json:"conversation_id,omitempty" maxLength:"200" doc:"Caller-assigned application conversation/grouping id. This value is independent of email thread topology. At most 200 characters — deliberately the same cap as the webhook conversation_ids filter-value limit and the message-list conversation_id filter limit (both 200), so an accepted conversation_id is never too long to filter by. Must not contain CR or LF."`
	ReplyTo        ReplyToField          `json:"reply_to,omitempty"`
	Attachments    []outbound.Attachment `json:"attachments,omitempty" nullable:"false" doc:"File attachments (base64 in each item's data). Limits: at most 10 attachments, each ≤ 10 MiB decoded, and ≤ 25 MiB decoded combined. Exceeding the count → 400 invalid_request; exceeding a size → 413 payload_too_large."`
	Unsubscribe    UnsubscribeOptions    `json:"unsubscribe,omitempty" doc:"Beta: opts this message into e2a-managed unsubscribe handling. This field may change before it is declared stable."`
	SendAt         *time.Time            `json:"send_at,omitempty" format:"date-time" doc:"Beta: scheduled sending may change before it is declared stable. Optional scheduled-send time (RFC 3339 with a UTC offset). When set to a future instant the message is accepted immediately and returns status=scheduled; it is submitted to the provider at approximately this time. Treat it as \"not before\" — accurate to within the scheduler's poll interval (seconds), not exact-to-the-millisecond, and actual delivery can be later under provider retry/outage. A value at or before now sends immediately (identical to omitting it). Must be no more than 90 days ahead (over → 400 invalid_request). A future direct loopback whose only recipient is the sending agent's own address returns 400 invalid_request because loopback is immediate. Scheduling does NOT survive a review hold: if held, send_at is dropped and the message sends on approval (the hold takes precedence over the loopback check). Moving the message to trash before provider submission starts prevents submission; if submission already has a fresh lease, delete returns 409 send_in_progress. Restoring before send_at re-arms it; restoring at or after send_at returns it live with delivery_status=failed and leaves the send canceled."`
}

// UnsubscribeOptions is the beta per-message opt-in to e2a-managed unsubscribe
// handling. It is presence-aware because omission is the compatibility path:
// it means only that managed unsubscribe was not requested and does not
// classify the message as transactional. This type may change before stable.
type UnsubscribeOptions struct {
	Mode    string `json:"mode" enum:"managed" doc:"Beta: managed requests e2a-hosted unsubscribe handling. This option may change before it is declared stable."`
	Present bool   `json:"-"`
}

func (o *UnsubscribeOptions) UnmarshalJSON(data []byte) error {
	o.Present = true
	if string(data) == "null" {
		return fmt.Errorf("unsubscribe must be an object")
	}
	var wire struct {
		Mode string `json:"mode"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return fmt.Errorf("invalid unsubscribe object: %w", err)
	}
	if wire.Mode != "managed" {
		return fmt.Errorf("unsubscribe.mode must be managed")
	}
	o.Mode = wire.Mode
	return nil
}

type createMessageInput struct {
	Address        string `path:"email"`
	RawBody        []byte
	IdempotencyKey string `header:"Idempotency-Key" doc:"Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request's response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together."`
	Wait           string `query:"wait" doc:"Optional bounded wait. wait=sent holds an immediately queued message until it reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status=accepted. A future send_at returns status=scheduled immediately and does not wait for the scheduled time. Default: no wait. Always branch on body.status, not the HTTP code."`
	Body           SendEmailRequest
}

type sendOutput struct {
	Status int
	Body   SendResultView
}

func (s *Server) registerOutbound() {
	// 202 Accepted covers every non-terminal outbound outcome: the message was
	// durably accepted but not yet delivered — either queued for async
	// immediate submission (status=accepted; the terminal sent/failed arrives
	// via GET / webhook events), queued for future submission
	// (status=scheduled), or held for human approval (status=pending_review).
	// Declared explicitly because Huma infers only the single DefaultStatus
	// (200, kept for the terminal-synchronous status=sent result).
	accepted202 := func() *huma.Response {
		return s.jsonResponse(reflect.TypeOf(SendResultView{}), "SendResultView",
			"Accepted — durably accepted but not yet delivered: status=accepted (queued for immediate async submission; terminal outcome via GET/webhook events), status=scheduled (queued for future submission at scheduled_at), or status=pending_review (held for human approval). All three are successful durable acceptance; do not re-send based on these statuses.")
	}
	testAccepted202 := func() *huma.Response {
		return s.jsonResponse(reflect.TypeOf(SendResultView{}), "SendResultView",
			"Accepted — durably accepted but not yet delivered: status=accepted (queued for async submission; terminal outcome via GET/webhook events) or status=pending_review (held for human approval).")
	}
	// 400 and 413 are declared explicitly on every attachment-bearing operation so
	// a client knows the failure modes up front. 400 invalid_request covers too many
	// attachments (> 10) and the other request-shape validations; 413 payload_too_large
	// covers the attachment limits and the distinct composed-message ceiling. Both
	// render the standard ErrorEnvelope — branch on error.code.
	badRequest400 := func() *huma.Response {
		return s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
			"Bad Request — request-shape/validation failure. error.code includes invalid_request (e.g. more than 10 attachments, send_at more than 90 days ahead, a reply_to whose address exceeds SMTP's mailbox octet limits, or a future send_at whose only recipient is the sending agent's own address when the message is not held for review because direct loopback is immediate), too_many_recipients, invalid_recipient (an unparseable recipient, or one whose address exceeds SMTP's mailbox octet limits — local part at most 64 octets, whole addr-spec at most 254 octets, counted in UTF-8 bytes), invalid_attachment (undecodable base64, an over-long filename, or a CR/LF in an attachment filename or content_type).")
	}
	registerOp(s.API, huma.Operation{
		OperationID: "sendMessage", Method: http.MethodPost, Path: "/v1/agents/{email}/messages",
		Summary: "Send a new email", Tags: []string{"messages"},
		Description:  scheduledSendBetaDoc + " Send a new email from the agent named in the path (a new thread). The sender is the path agent — `reply`/`forward` are their own sub-resources. A future send_at returns 202 + status=scheduled; an HITL hold returns 202 + status=pending_review. Both are successful durable acceptance and must not be re-sent. Honors Idempotency-Key. Attachment limits: at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). " + composedMessageCeilingDoc + " Two capacity limits apply and are permanently distinct — branch on the HTTP status: 402 limit_exceeded is a QUOTA (monthly-message / storage stock-or-flow cap; a retry will not clear it — surface an upgrade path), 429 rate_limited is a throughput/request-RATE cap (transient; back off Retry-After seconds and retry).",
		Security:     []map[string][]string{{"bearer": {}}},
		MaxBodyBytes: maxOutboundBytes,
		Responses:    map[string]*huma.Response{"202": accepted202(), "400": badRequest400(), "402": s.limitExceededResponse(), "409": s.idempotencyInFlightResponse(), "413": s.outboundPayloadTooLargeResponse(), "422": s.idempotencyReuseResponse(), "429": s.rateLimitedResponse(), "default": s.errorEnvelopeResponse()},
	}, s.handleCreateMessage)

	registerOp(s.API, huma.Operation{
		OperationID: "replyToMessage", Method: http.MethodPost, Path: "/v1/agents/{email}/messages/{id}/reply",
		Summary: "Reply to a message", Tags: []string{"messages"},
		Description:  scheduledSendBetaDoc + " Reply to a message (inbound or outbound); recipients and threading are derived from the original. Replying to a message the agent received targets its sender; replying to a message the agent sent continues the thread to its original recipients (`reply_all` also re-includes the original Cc). A future send_at returns 202 + status=scheduled; an HITL hold returns 202 + status=pending_review. Both are successful durable acceptance and must not be re-sent. Replying to a message this agent sent that has not been submitted to the provider yet returns 409 message_not_yet_delivered — it has no Message-ID to thread onto; retry once it is sent, or use wait=sent on the original send. Attachment limits: at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). " + composedMessageCeilingDoc,
		Security:     []map[string][]string{{"bearer": {}}},
		MaxBodyBytes: maxOutboundBytes,
		Responses:    map[string]*huma.Response{"202": accepted202(), "400": badRequest400(), "402": s.limitExceededResponse(), "409": s.replyForwardConflictResponse(), "413": s.outboundPayloadTooLargeResponse(), "422": s.idempotencyReuseResponse(), "429": s.rateLimitedResponse(), "default": s.errorEnvelopeResponse()},
	}, s.handleReply)

	registerOp(s.API, huma.Operation{
		OperationID: "forwardMessage", Method: http.MethodPost, Path: "/v1/agents/{email}/messages/{id}/forward",
		Summary: "Forward a message", Tags: []string{"messages"},
		Description:  scheduledSendBetaDoc + " Forward a message (inbound or outbound) to new recipients; the original is quoted and its attachments are carried over by default. Any attachments[] you supply are added on top of the originals. A future send_at returns 202 + status=scheduled; an HITL hold returns 202 + status=pending_review. Both are successful durable acceptance and must not be re-sent. Forwarding a message this agent sent that has not been submitted to the provider yet returns 409 message_not_yet_delivered — a forward requires the source message to have actually been sent; retry once it is sent, or use wait=sent on the original send. Attachment limits apply to the combined set (carried-over originals + supplied): at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). " + composedMessageCeilingDoc,
		Security:     []map[string][]string{{"bearer": {}}},
		MaxBodyBytes: maxOutboundBytes,
		Responses:    map[string]*huma.Response{"202": accepted202(), "400": badRequest400(), "402": s.limitExceededResponse(), "409": s.replyForwardConflictResponse(), "413": s.outboundPayloadTooLargeResponse(), "422": s.idempotencyReuseResponse(), "429": s.rateLimitedResponse(), "default": s.errorEnvelopeResponse()},
	}, s.handleForward)

	registerOp(s.API, huma.Operation{
		OperationID: "testAgent", Method: http.MethodPost, Path: "/v1/agents/{email}/test",
		Summary: "Send a test email to the agent's own address", Tags: []string{"agents"},
		Description: "Send a platform-originated test email (From: the platform noreply identity) to the agent's own address over the real external SMTP route, to confirm inbound delivery end to end. Returns 202: status=accepted (the message is durably persisted and queued; message_id is the GET-able e2a message id, and the terminal outcome arrives via GET /v1/messages/{id} or the email.sent / email.failed webhook events — provider_message_id appears only after provider submission) or status=pending_review when held for review. Always branch on body.status.",
		Security:    []map[string][]string{{"bearer": {}}},
		Responses:   map[string]*huma.Response{"202": testAccepted202(), "402": s.limitExceededResponse(), "429": s.rateLimitedResponse(), "default": s.errorEnvelopeResponse()},
	}, s.handleTestSend)
}

func (s *Server) handleTestSend(ctx context.Context, in *AddressParam) (*sendOutput, error) {
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	user, uerr := s.requireUser(ctx)
	if uerr != nil {
		return nil, uerr
	}
	if env := s.checkSendLimit(ag.ID); env != nil {
		return nil, env
	}
	if !ag.DomainVerified {
		return nil, NewError(http.StatusForbidden, "domain_not_verified", "agent domain must be verified before sending test email")
	}
	if s.deps.EnforceMessageSend != nil {
		if err := s.deps.EnforceMessageSend(ctx, user.ID); err != nil {
			if env, ok := limitEnvelope(err); ok {
				return nil, env
			}
			return nil, NewError(http.StatusInternalServerError, "internal_error", "limits check failed")
		}
	}
	if s.deps.SendTest == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "test send unavailable")
	}
	res, derr := s.deps.SendTest(ctx, ag)
	if derr != nil {
		return nil, envelopeFromOutboundError(derr)
	}
	// Same wire mapping as send/reply/forward (outboundResultView): held →
	// 202 pending_review, queued → 202 accepted (message_id = the durable e2a
	// msg_ id; provider_message_id arrives only after the worker submits). The
	// test send is queue-first, so a terminal-synchronous 200 no longer occurs.
	status, view := outboundResultView(res)
	return &sendOutput{Status: status, Body: view}, nil
}

// ReplyRequest mirrors the legacy reply body.
type ReplyRequest struct {
	Body           string                `json:"text" maxLength:"1048576"` // required (MSG-3); to/subject derived from the original
	HTMLBody       string                `json:"html,omitempty" maxLength:"1048576"`
	ReplyAll       bool                  `json:"reply_all,omitempty"`
	CC             []string              `json:"cc,omitempty" nullable:"false" maxLength:"320" doc:"Additional Cc recipients. The final message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters. The address itself must also fit SMTP's mailbox octet limits — local part at most 64 octets and the whole addr-spec at most 254 octets, counted in UTF-8 BYTES rather than characters — or the request is rejected with 400 invalid_recipient. A long plus-addressed local part is the usual way to exceed this."`
	BCC            []string              `json:"bcc,omitempty" nullable:"false" maxLength:"320" doc:"Additional Bcc recipients. The final message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters. The address itself must also fit SMTP's mailbox octet limits — local part at most 64 octets and the whole addr-spec at most 254 octets, counted in UTF-8 BYTES rather than characters — or the request is rejected with 400 invalid_recipient. A long plus-addressed local part is the usual way to exceed this."`
	ConversationID string                `json:"conversation_id,omitempty" maxLength:"200" doc:"Caller-assigned application conversation/grouping id override. This value is independent of email thread topology, which is derived from the referenced message. At most 200 characters — deliberately the same cap as the webhook conversation_ids filter-value limit and the message-list conversation_id filter limit (both 200), so an accepted conversation_id is never too long to filter by. Must not contain CR or LF."`
	ReplyTo        ReplyToField          `json:"reply_to,omitempty"`
	Attachments    []outbound.Attachment `json:"attachments,omitempty" nullable:"false" doc:"File attachments (base64 in each item's data). Limits: at most 10 attachments, each ≤ 10 MiB decoded, and ≤ 25 MiB decoded combined. Exceeding the count → 400 invalid_request; exceeding a size → 413 payload_too_large."`
	Unsubscribe    UnsubscribeOptions    `json:"unsubscribe,omitempty" doc:"Beta: opts this message into e2a-managed unsubscribe handling. This field may change before it is declared stable."`
	SendAt         *time.Time            `json:"send_at,omitempty" format:"date-time" doc:"Beta: scheduled sending may change before it is declared stable. Optional scheduled-send time (RFC 3339 with a UTC offset). When set to a future instant the reply is accepted immediately and returns status=scheduled; it is submitted at approximately this time (\"not before\", accurate to the scheduler poll interval). A value at or before now sends immediately. Must be no more than 90 days ahead (over → 400 invalid_request). A future direct loopback whose only recipient is the sending agent's own address returns 400 invalid_request because loopback is immediate. Scheduling does not survive a review hold: if held, send_at is dropped and the reply sends on approval (the hold takes precedence over the loopback check). Moving the message to trash before provider submission starts prevents submission; if submission already has a fresh lease, delete returns 409 send_in_progress. Restoring before send_at re-arms it; restoring at or after send_at returns it live with delivery_status=failed and leaves the send canceled."`
}

type replyInput struct {
	Address        string `path:"email"`
	ID             string `path:"id"`
	RawBody        []byte
	IdempotencyKey string `header:"Idempotency-Key" doc:"Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request's response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together."`
	Wait           string `query:"wait" doc:"Optional bounded wait. wait=sent holds an immediately queued reply until it reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status=accepted. A future send_at returns status=scheduled immediately and does not wait for the scheduled time. Default: no wait. Always branch on body.status, not the HTTP code."`
	Body           ReplyRequest
}

// loadRepliableMessage resolves the owned agent + the reply/forward target
// message — inbound or outbound — (404 if missing, expired/held, or not on
// this agent).
func (s *Server) loadRepliableMessage(ctx context.Context, address, msgID string) (*identity.AgentIdentity, *identity.Message, *identity.User, error) {
	ag, err := s.resolveOwnedAgent(ctx, address)
	if err != nil {
		return nil, nil, nil, err
	}
	user, uerr := s.requireUser(ctx)
	if uerr != nil {
		return nil, nil, nil, uerr
	}
	if s.deps.GetRepliableMessage == nil {
		return nil, nil, nil, NewError(http.StatusInternalServerError, "internal_error", "outbound unavailable")
	}
	msg, err := s.deps.GetRepliableMessage(ctx, msgID)
	if err != nil || msg == nil || msg.AgentID != ag.ID {
		return nil, nil, nil, NewError(http.StatusNotFound, "not_found", "message not found")
	}
	if env := parentNotYetSubmitted(msg); env != nil {
		return nil, nil, nil, env
	}
	return ag, msg, user, nil
}

// parentNotYetSubmitted returns a retriable 409 when the reply/forward target is
// the agent's own outbound message that is still queued for external submission.
//
// Threading is composed ONCE, at accept time, off the parent's RFC 5322
// Message-ID (identity.Message.ThreadMessageID). For an outbound parent that id
// is provider_message_id — which the send worker only records when it submits to
// SES (MarkOutboundSentTx). Reply to such a parent inside that window and
// ThreadMessageID() returns "", so the reply's In-Reply-To/References are
// omitted from its raw bytes PERMANENTLY and the recipient's client forks a new
// thread. The window is normally sub-second but widens to the retry horizon
// during a provider outage, so failing closed with a retriable conflict beats
// silently forking the thread.
//
// Each clause is load-bearing; together they say "queued for external
// submission, not yet submitted":
//
//   - direction == outbound — only an outbound parent threads off
//     provider_message_id. Inbound parents anchor on email_message_id, which is
//     written at intake and never empty-then-filled, so they can't hit this.
//   - method != loopback — a self-send's outbound copy is delivered locally and
//     terminally by performSelfSend (internal/agent/selfsend.go), which commits
//     it with delivery_status='sent' and a synthetic loopback provider id. It is
//     never awaiting external submission, so replying to it must not 409. It is
//     already excluded by both clauses below; this states the intent explicitly
//     rather than relying on that overlap.
//   - provider_message_id == "" — the exact condition that makes
//     ThreadMessageID() return "". Once populated, threading resolves and the
//     reply is unaffected.
//   - delivery_status in (accepted, sending) — the row is genuinely still in
//     flight. This is what keeps the guard from firing on a TERMINAL send that
//     never got a provider id (delivery_status='failed', e.g. SES rejected the
//     submit): that parent will never gain one, so a retry cannot clear the
//     condition and a 409 would strand the caller forever.
func parentNotYetSubmitted(msg *identity.Message) *ErrorEnvelope {
	if msg.Direction != "outbound" || msg.Method == "loopback" || msg.ProviderMessageID != "" {
		return nil
	}
	if msg.DeliveryStatus != "accepted" && msg.DeliveryStatus != "sending" {
		return nil
	}
	// A scheduled parent won't gain its provider Message-ID until it fires, which
	// may be far off — say so (and when) instead of the bare "retry after it is
	// sent", which reads as imminent.
	if msg.ScheduledAt != nil && msg.ScheduledAt.After(time.Now()) {
		return NewError(http.StatusConflict, "message_not_yet_delivered",
			fmt.Sprintf("referenced message is scheduled to send at %s and has not been submitted yet — reply or forward once it sends (threading anchors on the parent's provider Message-ID, assigned only at submission)",
				msg.ScheduledAt.UTC().Format(time.RFC3339Nano)))
	}
	return NewError(http.StatusConflict, "message_not_yet_delivered",
		"referenced message not yet delivered — retry after it is sent, or use wait=sent on the original send")
}

func (s *Server) handleReply(ctx context.Context, in *replyInput) (*sendOutput, error) {
	ag, msg, user, err := s.loadRepliableMessage(ctx, in.Address, in.ID)
	if err != nil {
		return nil, err
	}
	b := in.Body
	if b.Body == "" {
		return nil, NewError(http.StatusBadRequest, "invalid_request", "text is required")
	}
	// Validate only the user-supplied CC/BCC; the implicit To comes from the
	// (already-validated) referenced message — mirrors the legacy handler.
	if env := recipientCountError(b.CC, b.BCC); env != nil {
		return nil, env
	}
	if e := agent.ValidateRecipients(b.CC, b.BCC); e != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_recipient", e.Error())
	}
	if e := validateConversationID(b.ConversationID); e != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_request", e.Error())
	}
	normReplyTo, env := validateReplyTo(b.ReplyTo)
	if env != nil {
		return nil, env
	}
	// Build the reply request via the same outbound helpers the legacy
	// handler uses (subject normalization, recipient parsing, References).
	subject := msg.Subject
	if subject != "" && !strings.HasPrefix(strings.ToLower(subject), "re: ") {
		subject = "Re: " + subject
	} else if subject == "" {
		subject = "Re: your message"
	}
	// Recipient derivation branches on direction. Replying to a message the
	// agent RECEIVED targets its sender (Reply-To/From). Replying to a message
	// the agent SENT continues the thread to its original recipients (To, plus
	// Cc on reply_all) — reply-to-From would just address the agent itself.
	// BCC is never carried in either case.
	rr, e := s.replyRecipients(msg, b.ReplyAll, b.CC)
	if e != nil {
		return nil, e
	}
	// Anchor threading on the parent's RFC Message-ID. For an inbound that's the
	// sender's Message-ID (email_message_id); for the agent's own outbound it's
	// the relay-assigned provider_message_id — email_message_id is empty there,
	// so using it would drop In-Reply-To/References and fork the recipient's
	// thread (see identity.Message.ThreadMessageID).
	parentMessageID := msg.ThreadMessageID()
	req := outbound.SendRequest{
		To: rr.To, CC: rr.CC, BCC: b.BCC, Subject: subject, Body: b.Body, HTMLBody: b.HTMLBody,
		ReplyToMessageID: parentMessageID,
		References:       outbound.BuildReferencesChain(msg.RawMessage, parentMessageID),
		// conversation_id resolution (caller id > inherit-from-referenced > mint)
		// is centralized in DeliverOutbound, which receives this message as the
		// referenced message — so the reply inherits its thread there (#328).
		ConversationID: b.ConversationID, ReplyTo: normReplyTo, Attachments: b.Attachments,
		Unsubscribe: outboundUnsubscribe(b.Unsubscribe),
	}
	req.CC = agent.StripAgentSelfAliases(req.CC, ag.EmailAddress())
	req.BCC = agent.StripAgentSelfAliases(req.BCC, ag.EmailAddress())
	// Re-count the FINAL, post-expansion recipient set. reply_all fans the
	// thread's To+Cc into req.To/req.CC above, so the earlier b.CC/b.BCC check is
	// not the real fan-out — without this, a reply_all to a large thread bypasses
	// the cap that /send and /forward enforce (the downstream send path has no
	// cap of its own).
	if env := recipientCountError(req.To, req.CC, req.BCC); env != nil {
		return nil, env
	}
	sched, senv := scheduledInstant(b.SendAt, time.Now())
	if senv != nil {
		return nil, senv
	}
	req.ScheduledAt = sched
	return s.deliver(ctx, user, ag, literalRequest(req), "reply", parentMessageID, "/v1/reply/"+in.ID, in.IdempotencyKey, in.Wait, in.RawBody, msg)
}

// replyRecipients resolves a reply's To/CC from the referenced message,
// branching on direction. An inbound (received) message replies to its sender;
// an outbound (sent) message continues the thread to its original recipients.
// Returns a 400 envelope for an outbound target with no recorded recipients —
// falling back to the message's Sender there would address the agent itself, so
// we fail closed rather than emit a self-addressed reply.
func (s *Server) replyRecipients(msg *identity.Message, replyAll bool, extraCC []string) (*outbound.ReplyRecipients, *ErrorEnvelope) {
	if msg.Direction == "outbound" {
		rr := outbound.ReplyRecipientsForOutbound(msg.ToRecipients, msg.CC, extraCC, replyAll)
		if len(rr.To) == 0 {
			return nil, NewError(http.StatusBadRequest, "invalid_recipient",
				"cannot reply: the original message has no recorded recipients")
		}
		return rr, nil
	}
	rr, err := outbound.ParseReplyRecipients(msg.RawMessage, replyAll, extraCC)
	if err != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_recipient", err.Error())
	}
	if len(rr.To) == 0 {
		if len(msg.ReplyTo) > 0 && strings.TrimSpace(msg.ReplyTo[0]) != "" {
			rr.To = []string{msg.ReplyTo[0]}
		} else if strings.TrimSpace(msg.HeaderFrom) != "" {
			rr.To = []string{msg.HeaderFrom}
		}
	}
	return rr, nil
}

// ForwardRequest mirrors the legacy forward body.
type ForwardRequest struct {
	To             []string              `json:"to" nullable:"false" maxLength:"320" doc:"Primary recipients. The message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters. The address itself must also fit SMTP's mailbox octet limits — local part at most 64 octets and the whole addr-spec at most 254 octets, counted in UTF-8 BYTES rather than characters — or the request is rejected with 400 invalid_recipient. A long plus-addressed local part is the usual way to exceed this."` // required (MSG-3)
	CC             []string              `json:"cc,omitempty" nullable:"false" maxLength:"320" doc:"Cc recipients. The message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters. The address itself must also fit SMTP's mailbox octet limits — local part at most 64 octets and the whole addr-spec at most 254 octets, counted in UTF-8 BYTES rather than characters — or the request is rejected with 400 invalid_recipient. A long plus-addressed local part is the usual way to exceed this."`
	BCC            []string              `json:"bcc,omitempty" nullable:"false" maxLength:"320" doc:"Bcc recipients. The message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters. The address itself must also fit SMTP's mailbox octet limits — local part at most 64 octets and the whole addr-spec at most 254 octets, counted in UTF-8 BYTES rather than characters — or the request is rejected with 400 invalid_recipient. A long plus-addressed local part is the usual way to exceed this."`
	Body           string                `json:"text" maxLength:"1048576"` // required (MSG-3); subject derived as "Fwd:"
	HTMLBody       string                `json:"html,omitempty" maxLength:"1048576"`
	ConversationID string                `json:"conversation_id,omitempty" maxLength:"200" doc:"Caller-assigned application conversation/grouping id override. This value is independent of email thread topology; a forward starts a new email thread. At most 200 characters — deliberately the same cap as the webhook conversation_ids filter-value limit and the message-list conversation_id filter limit (both 200), so an accepted conversation_id is never too long to filter by. Must not contain CR or LF."`
	ReplyTo        ReplyToField          `json:"reply_to,omitempty"`
	Attachments    []outbound.Attachment `json:"attachments,omitempty" nullable:"false" doc:"Additional attachments to include alongside the forwarded message's original attachments, which are carried over automatically. Limits apply to the combined set (originals + these): at most 10 attachments, each ≤ 10 MiB decoded, and ≤ 25 MiB decoded combined. Exceeding the count → 400 invalid_request; exceeding a size → 413 payload_too_large."`
	Unsubscribe    UnsubscribeOptions    `json:"unsubscribe,omitempty" doc:"Beta: opts this message into e2a-managed unsubscribe handling. This field may change before it is declared stable."`
	SendAt         *time.Time            `json:"send_at,omitempty" format:"date-time" doc:"Beta: scheduled sending may change before it is declared stable. Optional scheduled-send time (RFC 3339 with a UTC offset). When set to a future instant the forward is accepted immediately and returns status=scheduled; it is submitted at approximately this time (\"not before\", accurate to the scheduler poll interval). A value at or before now sends immediately. Must be no more than 90 days ahead (over → 400 invalid_request). A future direct loopback whose only recipient is the sending agent's own address returns 400 invalid_request because loopback is immediate. Scheduling does not survive a review hold: if held, send_at is dropped and the forward sends on approval (the hold takes precedence over the loopback check). Moving the message to trash before provider submission starts prevents submission; if submission already has a fresh lease, delete returns 409 send_in_progress. Restoring before send_at re-arms it; restoring at or after send_at returns it live with delivery_status=failed and leaves the send canceled."`
}

type forwardInput struct {
	Address        string `path:"email"`
	ID             string `path:"id"`
	RawBody        []byte
	IdempotencyKey string `header:"Idempotency-Key" doc:"Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request's response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together."`
	Wait           string `query:"wait" doc:"Optional bounded wait. wait=sent holds an immediately queued forward until it reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status=accepted. A future send_at returns status=scheduled immediately and does not wait for the scheduled time. Default: no wait. Always branch on body.status, not the HTTP code."`
	Body           ForwardRequest
}

func (s *Server) handleForward(ctx context.Context, in *forwardInput) (*sendOutput, error) {
	ag, msg, user, err := s.loadRepliableMessage(ctx, in.Address, in.ID)
	if err != nil {
		return nil, err
	}
	b := in.Body
	if len(b.To) == 0 && len(b.CC) == 0 {
		return nil, NewError(http.StatusBadRequest, "invalid_request", "at least one recipient in to or cc is required")
	}
	if env := recipientCountError(b.To, b.CC, b.BCC); env != nil {
		return nil, env
	}
	if e := agent.ValidateRecipients(b.To, b.CC, b.BCC); e != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_recipient", e.Error())
	}
	if e := validateConversationID(b.ConversationID); e != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_request", e.Error())
	}
	normReplyTo, env := validateReplyTo(b.ReplyTo)
	if env != nil {
		return nil, env
	}
	subject := outbound.BuildForwardSubject(msg.Subject)
	fwdCtx := outbound.ExtractForwardContext(msg.RawMessage)
	composedBody := outbound.BuildForwardBody(b.Body, fwdCtx)
	var composedHTML string
	if b.HTMLBody != "" || fwdCtx.HTML != "" || fwdCtx.Text != "" {
		composedHTML = outbound.BuildForwardHTMLBody(b.HTMLBody, fwdCtx)
	}
	// Carry the source message's attachment parts by default (#298): a
	// forward should ship the original files the way mail clients do, without
	// the caller re-fetching and re-encoding each one. Caller-supplied
	// attachments are additive on top of the originals.
	attachments := outbound.ForwardAttachments(msg.RawMessage)
	attachments = append(attachments, b.Attachments...)
	req := outbound.SendRequest{
		To: b.To, CC: b.CC, BCC: b.BCC, Subject: subject, Body: composedBody, HTMLBody: composedHTML,
		ConversationID: b.ConversationID, ReplyTo: normReplyTo, Attachments: attachments,
		Unsubscribe: outboundUnsubscribe(b.Unsubscribe),
	}
	req.CC = agent.StripAgentSelfAliases(req.CC, ag.EmailAddress())
	req.BCC = agent.StripAgentSelfAliases(req.BCC, ag.EmailAddress())
	sched, senv := scheduledInstant(b.SendAt, time.Now())
	if senv != nil {
		return nil, senv
	}
	req.ScheduledAt = sched
	return s.deliver(ctx, user, ag, literalRequest(req), "forward", msg.ThreadMessageID(), "/v1/forward/"+in.ID, in.IdempotencyKey, in.Wait, in.RawBody, msg)
}

// validateOutboundBody runs the shared pre-send validation.
func (s *Server) validateOutboundBody(subject, body string, to, cc, bcc []string, conversationID string) *ErrorEnvelope {
	if subject == "" || body == "" {
		return NewError(http.StatusBadRequest, "invalid_request", "subject and text are required")
	}
	if strings.ContainsAny(subject, "\r\n") {
		return NewError(http.StatusBadRequest, "invalid_request", "subject must not contain CR or LF characters")
	}
	if len(to) == 0 && len(cc) == 0 {
		return NewError(http.StatusBadRequest, "invalid_request", "at least one recipient in to or cc is required")
	}
	if env := recipientCountError(to, cc, bcc); env != nil {
		return env
	}
	if err := agent.ValidateRecipients(to, cc, bcc); err != nil {
		return NewError(http.StatusBadRequest, "invalid_recipient", err.Error())
	}
	if err := validateConversationID(conversationID); err != nil {
		return NewError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	return nil
}

// composedMessageSizeError enforces the composed-message hard cap: the sum of
// subject + text + html + DECODED attachment bytes must stay under the SES v1
// stored-message ceiling (maxComposedMessageBytes — the real upstream limit).
// This is distinct from the per-attachment and per-wire-body limits: a caller
// can stay under each individual limit while the composed MIME exceeds what the
// upstream provider will accept, so the real ceiling is checked here on the
// fully-composed content. Over → 413 payload_too_large (reuses the existing 413
// path/error code — no new status). The byte total (DECODED attachment bytes,
// not the base64-inflated wire size) is computed by the shared
// outbound.ComposedSize so this and the HITL approve-override path agree exactly.
func composedMessageSizeError(subject, text, html string, atts []outbound.Attachment) *ErrorEnvelope {
	total := outbound.ComposedSize(subject, text, html, atts)
	if total > maxComposedMessageBytes {
		return NewError(http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("composed message too large — %d bytes (subject + text + html + decoded attachments), limit is %d (%d MB)",
				total, maxComposedMessageBytes, maxComposedMessageBytes/(1024*1024))).
			WithDetails(PayloadTooLargeDetails{
				Scope:       "composed_message",
				ActualBytes: int64(total),
				MaxBytes:    int64(maxComposedMessageBytes),
			})
	}
	return nil
}

// ReplyToField is the send/reply/forward reply_to input. Reply-To is an
// address-list in RFC 5322, so the field accepts EITHER a single address string
// (the historical form) OR a JSON array of up to maxReplyToAddresses address
// strings. Both decode into values; validateReplyTo checks each entry and
// flattens them to one canonical address-list string, so the rest of the
// outbound pipeline (compose, storage, recompose-on-approval) still sees a
// single string and is untouched.
type ReplyToField struct {
	// values holds the caller-supplied address strings VERBATIM: a one-element
	// slice for the string form, or each array element. Nil when omitted.
	// Preserving the caller's exact bytes keeps the single-address form
	// byte-identical to the pre-list behavior (no display-name re-quoting).
	values []string
}

// UnmarshalJSON accepts a JSON string or a JSON array of strings. Structural
// validation (types, per-entry length, array bound) is the schema's job (see
// Schema); this only decodes.
func (f *ReplyToField) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		f.values = nil
		return nil
	}
	if trimmed[0] == '[' {
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		f.values = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return err
	}
	if s == "" {
		// An explicit empty string means "not set" — the historical scalar
		// reply_to behavior (the compose layer then defaults Reply-To to the
		// agent's own address). Preserve it so a caller that sends "" instead of
		// omitting the field keeps working. An empty element INSIDE an array is
		// a different case: it stays a malformed address and validateReplyTo
		// rejects it.
		f.values = nil
		return nil
	}
	f.values = []string{s}
	return nil
}

// Schema makes Huma emit — and validate against — a string-OR-bounded-array
// union for reply_to, keeping the historical single-string form working while
// exposing the address-list the header itself permits. Semantic address checks
// (parseability, mailbox octet limits) stay in validateReplyTo.
func (ReplyToField) Schema(huma.Registry) *huma.Schema {
	maxLen := maxEmailAddressLen
	maxItems := maxReplyToAddresses
	return &huma.Schema{
		OneOf: []*huma.Schema{
			{Type: huma.TypeString, MaxLength: &maxLen},
			{
				Type:     huma.TypeArray,
				Items:    &huma.Schema{Type: huma.TypeString, MaxLength: &maxLen},
				MaxItems: &maxItems,
			},
		},
		Description: "Sets the Reply-To header — where replies to this message are directed. Either a single RFC 5322 address (optionally with a display name, e.g. \"Support <support@acme.com>\") or an array of up to 5 such addresses to direct replies to several destinations. Each address string is at most 320 characters (display name + address combined), and each address must fit SMTP's mailbox octet limits (local part at most 64 octets, whole addr-spec at most 254 octets, counted in UTF-8 bytes) — a violation is 400 invalid_request. Defaults to the sending agent's own address.",
	}
}

// validateReplyTo checks a caller-supplied Reply-To override and returns the
// canonical value for the header. Empty (field omitted) → ("", nil): the
// compose layer then defaults Reply-To to the agent's own address. A non-empty
// value must be 1..maxReplyToAddresses entries, each exactly one RFC 5322
// address (optionally display-named) within the length + mailbox octet limits;
// a bare comma-separated string is rejected — the array form is how you name
// several. Entries are preserved verbatim and joined with ", " into one RFC
// 5322 address-list, so the single-address form is byte-identical to before and
// a multi-address send emits a well-formed list. Validating here keeps a bad
// Reply-To from reaching the composer (where sanitizeHeaderValue would silently
// mangle it) or the SMTP relay (a generic 500).
func validateReplyTo(f ReplyToField) (string, *ErrorEnvelope) {
	if len(f.values) == 0 {
		return "", nil
	}
	if len(f.values) > maxReplyToAddresses {
		return "", NewError(http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("reply_to accepts at most %d addresses, got %d", maxReplyToAddresses, len(f.values)))
	}
	for _, entry := range f.values {
		// Length bound BEFORE parsing (runes, matching the schema maxLength
		// semantics) so an oversized entry is rejected on a cheap count, never
		// handed to the address parser. The schema rejects this at the edge for
		// /v1 bodies; this is the shared runtime backstop.
		if n := utf8.RuneCountInString(entry); n > maxEmailAddressLen {
			return "", NewError(http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("reply_to entry too long — %d characters, max %d (display name + address combined)", n, maxEmailAddressLen))
		}
		// ParseAddress (not ParseAddressList) rejects a comma-bearing multi-
		// address string, so the string form stays a single address; several
		// addresses must come as the array form.
		addr, err := mail.ParseAddress(entry)
		if err != nil {
			return "", NewError(http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("reply_to is not a valid email address: %v", err))
		}
		if err := outbound.ValidateMailboxAddress(addr.Address); err != nil {
			return "", NewError(http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("reply_to is not a valid SMTP mailbox: %v", err))
		}
	}
	return strings.Join(f.values, ", "), nil
}

// validateAttachments enforces the attachment contract on every outbound path
// (send/reply/forward, and approve-with-edits). It checks, in order:
//
//   - count ≤ maxAttachmentCount → 400 invalid_request (too many is a shape/
//     validation error, not an oversize payload)
//   - no CR or LF in att.Filename or att.ContentType → 400 invalid_attachment.
//     Both are written into MIME headers, so a newline there is a header-
//     injection attempt. The composer refuses it too and always did, but that
//     refusal surfaces on a path whose errors are 500s — and a permanent client
//     error returned as a 5xx is precisely what SDK retry logic hammers. Doing
//     it here matches how CR/LF in the subject is already rejected and keeps
//     this in the same 400 invalid_attachment family as the filename length cap.
//   - each att.Data is decodable base64 → 400 invalid_attachment (the composer
//     passes att.Data verbatim into the MIME body with Content-Transfer-Encoding:
//     base64, so malformed base64 would otherwise slip past every check and only
//     fail at the SMTP relay as a generic 500)
//   - each attachment's DECODED size ≤ maxAttachmentBytes → 413 payload_too_large
//   - the combined DECODED size ≤ maxAttachmentsTotalBytes → 413 payload_too_large
//
// All size checks are on DECODED bytes, not the base64 wire size, so the limits a
// caller sees are the real file sizes and match the bytes SES ultimately carries.
// Whitespace (line-wrapping) is stripped before decoding to match how mail decoders
// treat base64 bodies, so a caller that pre-wraps its base64 is not falsely rejected.
func validateAttachments(atts []outbound.Attachment) *ErrorEnvelope {
	if len(atts) > maxAttachmentCount {
		return NewError(http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("too many attachments — at most %d per message (got %d)", maxAttachmentCount, len(atts))).
			WithDetails(ValidationErrorDetails{Fields: []FieldError{{
				Location: "body.attachments",
				Message:  fmt.Sprintf("must contain at most %d items (got %d)", maxAttachmentCount, len(atts)),
			}}})
	}
	var total int
	for i, att := range atts {
		name := att.Filename
		if name == "" {
			name = fmt.Sprintf("#%d", i)
		}
		if len(att.Filename) > outbound.MaxAttachmentFilenameBytes {
			return NewError(http.StatusBadRequest, "invalid_attachment",
				fmt.Sprintf("attachment %q filename is too long — %d bytes, limit is %d",
					name, len(att.Filename), outbound.MaxAttachmentFilenameBytes))
		}
		if strings.ContainsAny(att.Filename, "\r\n") {
			return NewError(http.StatusBadRequest, "invalid_attachment",
				fmt.Sprintf("attachment #%d: filename must not contain CR or LF characters", i)).
				WithDetails(ValidationErrorDetails{Fields: []FieldError{{
					Location: fmt.Sprintf("body.attachments[%d].filename", i),
					Message:  "must not contain CR or LF characters",
				}}})
		}
		if strings.ContainsAny(att.ContentType, "\r\n") {
			return NewError(http.StatusBadRequest, "invalid_attachment",
				fmt.Sprintf("attachment %q: content_type must not contain CR or LF characters", name)).
				WithDetails(ValidationErrorDetails{Fields: []FieldError{{
					Location: fmt.Sprintf("body.attachments[%d].content_type", i),
					Message:  "must not contain CR or LF characters",
				}}})
		}
		clean := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, att.Data)
		decoded, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return NewError(http.StatusBadRequest, "invalid_attachment",
				fmt.Sprintf("attachment %q: data is not valid base64", name))
		}
		if len(decoded) > maxAttachmentBytes {
			return NewError(http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("attachment %q is too large — %d bytes decoded, limit is %d (%d MB)",
					name, len(decoded), maxAttachmentBytes, maxAttachmentBytes/(1024*1024))).
				WithDetails(PayloadTooLargeDetails{
					Scope:       "attachment",
					ActualBytes: int64(len(decoded)),
					MaxBytes:    int64(maxAttachmentBytes),
					Filename:    att.Filename,
				})
		}
		total += len(decoded)
	}
	if total > maxAttachmentsTotalBytes {
		return NewError(http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("attachments too large — %d bytes decoded in total, limit is %d (%d MB)",
				total, maxAttachmentsTotalBytes, maxAttachmentsTotalBytes/(1024*1024))).
			WithDetails(PayloadTooLargeDetails{
				Scope:       "attachments_total",
				ActualBytes: int64(total),
				MaxBytes:    int64(maxAttachmentsTotalBytes),
			})
	}
	return nil
}

// deliver runs the idempotency handshake, then — inside the claimed
// execution — builds the request via prepare, runs the send-limit /
// domain-verified / enforce-cap checks, and calls DeliverOutbound, mapping
// the OutboundResult to the wire view.
//
// Everything that consults MUTABLE state (template resolution inside
// prepare, rate limits, plan caps) runs after the Claim so a keyed retry
// replays the cached response instead of re-evaluating state that may have
// changed since the first attempt (deleted template, exhausted quota, …).
// Failures inside the closure happen strictly before the DeliverOutbound
// side effect, so runIdempotent releases the key and a retry can proceed —
// exactly fn's documented contract.
func (s *Server) deliver(ctx context.Context, user *identity.User, ag *identity.AgentIdentity, prepare func() (outbound.SendRequest, *ErrorEnvelope), msgType, replyTo, route, idemKey, wait string, rawBody []byte, referenced *identity.Message) (*sendOutput, error) {
	if s.deps.DeliverOutbound == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "outbound delivery unavailable")
	}
	// idemCompleteTx lets the async accept-tx (agent.DeliverOutbound) commit this
	// request's idempotency-key completion in the SAME transaction as the message
	// insert + send-job enqueue — so a crash after that commit replays 'accepted'
	// instead of re-persisting. It caches the EXACT wire body deliver() returns for
	// an accepted result (built below), keeping replay byte-faithful. nil when the
	// request carries no Idempotency-Key or no store is wired (then agent skips it,
	// and the synchronous path is unaffected). Uses the same user namespace + key
	// runIdempotent Claims/Completes under, so its in-tx Complete and runIdempotent's
	// post-hoc Complete address the same row (the latter no-ops on the in_progress
	// guard once this has run).
	var idemCompleteTx agent.AcceptIdemCompleter
	if idemKey != "" && s.deps.Idempotency != nil {
		nsKey := idemUserNS + idemKey
		uid := user.ID
		idemCompleteTx = func(ctx context.Context, tx pgx.Tx, result *agent.OutboundResult) error {
			status, view := outboundResultView(result)
			raw, mErr := json.Marshal(view)
			if mErr != nil {
				raw = []byte("{}")
			}
			return s.deps.Idempotency.CompleteTx(ctx, tx, uid, nsKey, idempotency.CachedResponse{
				StatusCode: status, ContentType: "application/json", Body: raw,
			})
		}
	}
	status, view, err := runIdempotent(s, ctx, user.ID, idemKey, route, rawBody, func() (int, SendResultView, error) {
		req, env := prepare()
		if env != nil {
			return 0, SendResultView{}, env
		}
		if env := validateAttachments(req.Attachments); env != nil {
			return 0, SendResultView{}, env
		}
		if env := composedMessageSizeError(req.Subject, req.Body, req.HTMLBody, req.Attachments); env != nil {
			return 0, SendResultView{}, env
		}
		if env := s.checkSendLimit(ag.ID); env != nil {
			return 0, SendResultView{}, env
		}
		if !ag.DomainVerified {
			return 0, SendResultView{}, NewError(http.StatusForbidden, "domain_not_verified", "agent domain must be verified before sending")
		}
		if s.deps.EnforceMessageSend != nil {
			if err := s.deps.EnforceMessageSend(ctx, user.ID); err != nil {
				if env, ok := limitEnvelope(err); ok {
					return 0, SendResultView{}, env
				}
				return 0, SendResultView{}, NewError(http.StatusInternalServerError, "internal_error", "limits check failed")
			}
		}
		res, derr := s.deps.DeliverOutbound(ctx, user, ag, req, msgType, replyTo, referenced, idemCompleteTx)
		if derr != nil {
			return 0, SendResultView{}, envelopeFromOutboundError(derr)
		}
		status, view := outboundResultView(res)
		return status, view, nil
	})
	if err != nil {
		return nil, err
	}
	// wait=sent (contract §2): after an async accept, hold the request until the
	// send reaches sent/failed or the ceiling, then return that state. The
	// idempotency cache already holds the accept-time 'accepted' body (§2.4), so a
	// replay does NOT re-wait — only this live caller sees the polled outcome.
	if wait == "sent" && view.Status == "accepted" && s.deps.PollSendOutcome != nil {
		status, view = s.waitForSent(ctx, status, view)
	}
	return &sendOutput{Status: status, Body: view}, nil
}

const (
	waitSentCeiling = 15 * time.Second // below the 20s contract ceiling (§2.3) + proxy timeouts
	waitSentPoll    = 250 * time.Millisecond
)

// waitForSent polls the async send's delivery_status until sent/failed or the
// ceiling. Timeout → the accepted view (the caller polls GET / waits for the event).
func (s *Server) waitForSent(ctx context.Context, acceptedStatus int, accepted SendResultView) (int, SendResultView) {
	deadline := time.Now().Add(waitSentCeiling)
	for {
		if o, err := s.deps.PollSendOutcome(ctx, accepted.MessageID); err == nil {
			switch o.DeliveryStatus {
			case "sent", "delivered", "deferred", "bounced", "complained":
				return http.StatusOK, SendResultView{Status: "sent", MessageID: accepted.MessageID, ProviderMessageID: o.ProviderMessageID, SentAs: o.SentAs, Method: accepted.Method}
			case "failed":
				return http.StatusOK, SendResultView{Status: "failed", MessageID: accepted.MessageID, Method: accepted.Method}
			}
		}
		if time.Now().After(deadline) {
			return acceptedStatus, accepted // still in flight
		}
		select {
		case <-ctx.Done():
			return acceptedStatus, accepted
		case <-time.After(waitSentPoll):
		}
	}
}

// acceptedView is the single source of the async-accept wire body (slice C). Both
// the live response and the idempotency cache entry are built from it, so a replay
// is byte-identical. Deliberately minimal — status + message_id + method; the
// provider id / sent_as / delivery outcome are not known at accept time and surface
// later via GET /v1/messages/{id} and the email.sent / email.failed webhooks.
func acceptedView(messageID string) SendResultView {
	return SendResultView{Status: "accepted", MessageID: messageID, Method: "smtp"}
}

// scheduledView is the async-accept wire body for a scheduled send. Like
// acceptedView it is built identically for the live response and the idempotency
// cache entry (byte-identical replay), and it deliberately reports status
// "scheduled" (not "accepted") so the wait=sent poll loop is skipped — a
// scheduled send has no imminent outcome to wait for.
func scheduledView(messageID string, at *time.Time) SendResultView {
	return SendResultView{Status: "scheduled", MessageID: messageID, Method: "smtp", ScheduledAt: at}
}

// outboundResultView is shared by the live response and the same-transaction
// idempotency completion. That keeps queue acceptance (202/accepted) distinct
// from terminal providerless loopback delivery (200/sent) on every replay.
func outboundResultView(res *agent.OutboundResult) (int, SendResultView) {
	if res.Held {
		return http.StatusAccepted, SendResultView{
			Status: "pending_review", MessageID: res.PendingMessageID,
			ApprovalExpiresAt: res.ApprovalExpiresAt,
		}
	}
	if res.Status == "scheduled" {
		return http.StatusAccepted, scheduledView(res.MessageID, res.ScheduledAt)
	}
	if res.Status == "accepted" {
		return http.StatusAccepted, acceptedView(res.MessageID)
	}
	return http.StatusOK, SendResultView{
		Status: "sent", MessageID: res.MessageID,
		ProviderMessageID: res.ProviderMessageID,
		SentAs:            res.SentAs,
		Method:            res.Method,
	}
}

// literalRequest wraps an already-built SendRequest as a deliver prepare
// closure — used by reply/forward, whose request is fully derived from the
// request bytes plus the (already-loaded) referenced message.
func literalRequest(req outbound.SendRequest) func() (outbound.SendRequest, *ErrorEnvelope) {
	return func() (outbound.SendRequest, *ErrorEnvelope) { return req, nil }
}

// checkSendLimit applies the per-agent outbound rate limit (mirrors the
// legacy sendLimit). On block it returns a 429 envelope carrying the
// retry-after seconds in the body AND — via WithRetryAfter → stampRequestID —
// the IETF Retry-After response header, so a handler-raised send 429 matches
// the middleware-enforced registration/poll limiters (which set it directly).
func (s *Server) checkSendLimit(agentID string) *ErrorEnvelope {
	if s.deps.SendLimit == nil {
		return nil
	}
	ok, retryAfter := s.deps.SendLimit(agentID)
	if ok {
		return nil
	}
	secs := int(retryAfter.Round(time.Second).Seconds())
	if secs < 1 {
		secs = 1
	}
	return NewError(http.StatusTooManyRequests, "rate_limited",
		"rate limit exceeded — max 60 sends per minute per agent").
		WithDetails(map[string]any{"retry_after_seconds": secs}).
		WithRetryAfter(secs)
}

func (s *Server) handleCreateMessage(ctx context.Context, in *createMessageInput) (*sendOutput, error) {
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	user, uerr := s.requireUser(ctx)
	if uerr != nil {
		return nil, uerr
	}
	b := in.Body
	// The deterministic template-shape checks (mutual exclusions) depend only
	// on the request bytes, so they stay in the prologue. Resolution +
	// rendering consult the mutable templates table and therefore run inside
	// the idempotent execution (in prepare, below).
	if env := validateSendTemplateShape(&b); env != nil {
		return nil, env
	}
	// The agent moved from the body (`from`) to the path, so fold the agent id
	// into the idempotency route — otherwise two agents owned by the same user
	// could collide on an identical key+body (the body hash alone no longer
	// separates them).
	route := "/v1/agents/" + ag.ID + "/messages"
	prepare := func() (outbound.SendRequest, *ErrorEnvelope) {
		// Resolve + render any template reference FIRST (in place), so the
		// rendered subject/body flow through the exact same validation below
		// and any HITL hold persists rendered content (see resolveSendTemplate
		// for both ordering invariants: after the idempotency claim, before
		// the hold).
		if env := s.resolveSendTemplate(ctx, user.ID, &b); env != nil {
			return outbound.SendRequest{}, env
		}
		if env := s.validateOutboundBody(b.Subject, b.Body, b.To, b.CC, b.BCC, b.ConversationID); env != nil {
			return outbound.SendRequest{}, env
		}
		normReplyTo, env := validateReplyTo(b.ReplyTo)
		if env != nil {
			return outbound.SendRequest{}, env
		}
		sched, senv := scheduledInstant(b.SendAt, time.Now())
		if senv != nil {
			return outbound.SendRequest{}, senv
		}
		// The sender is the path agent (decision 3) — there is no body `from`;
		// the agent is the path and auth scopes the sender, so no spoofing is
		// possible.
		return outbound.SendRequest{
			From: ag.EmailAddress(), To: b.To, CC: b.CC, BCC: b.BCC, Subject: b.Subject,
			Body: b.Body, HTMLBody: b.HTMLBody, ConversationID: b.ConversationID,
			ReplyTo: normReplyTo, Attachments: b.Attachments,
			Unsubscribe: outboundUnsubscribe(b.Unsubscribe),
			ScheduledAt: sched,
		}, nil
	}
	// A cold send has no referenced inbound (nil) — it's not a reply/forward.
	return s.deliver(ctx, user, ag, prepare, "send", "", route, in.IdempotencyKey, in.Wait, in.RawBody, nil)
}

func outboundUnsubscribe(o UnsubscribeOptions) *outbound.UnsubscribeOptions {
	if !o.Present {
		return nil
	}
	return &outbound.UnsubscribeOptions{Mode: o.Mode}
}
