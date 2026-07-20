package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// ---------------------------------------------------------------------------
// Wire types (§1.2, §1.3)
// ---------------------------------------------------------------------------

// BatchMessage is one item in a batch-send request. Field-for-field a
// near-clone of SendEmailRequest minus `from` (the sending agent is the
// path parameter, shared across the batch). See docs/design/batch-send.md
// §0.5 and §1.2 for the shape rationale.
type BatchMessage struct {
	To             []string              `json:"to" nullable:"false" maxLength:"320" doc:"Primary recipients for this item. Same per-item cap as single-send (50 combined across to+cc+bcc). Each item's envelope is independent — item i's cc/bcc are not visible in item j."`
	CC             []string              `json:"cc,omitempty" nullable:"false" maxLength:"320" doc:"Cc recipients for this item."`
	BCC            []string              `json:"bcc,omitempty" nullable:"false" maxLength:"320" doc:"Bcc recipients for this item."`
	Subject        string                `json:"subject,omitempty" maxLength:"2000" doc:"Literal subject. Required unless a template reference is used. Same caps as single-send."`
	Body           string                `json:"text,omitempty" maxLength:"1048576" doc:"Plain-text body."`
	HTMLBody       string                `json:"html,omitempty" maxLength:"1048576" doc:"HTML body."`
	TemplateID     string                `json:"template_id,omitempty" doc:"Send using a stored template. Mutually exclusive with template_alias and with literal subject/text/html on this item."`
	TemplateAlias  string                `json:"template_alias,omitempty" doc:"Human-handle alternative to template_id."`
	TemplateData   TemplateData          `json:"template_data,omitempty" doc:"Per-item template data. Populated freely across items — this is what makes native mail-merge possible without a server-side templating loop (docs/design/batch-send.md §0.5)."`
	ConversationID string                `json:"conversation_id,omitempty" maxLength:"200" doc:"Caller-assigned conversation (thread) id. Auto-minted per item if omitted."`
	ReplyTo        string                `json:"reply_to,omitempty" maxLength:"320" doc:"Per-item Reply-To override. If empty, the batch-level reply_to default (if set) applies."`
	Attachments    []outbound.Attachment `json:"attachments,omitempty" nullable:"false" doc:"Per-item attachments. Single attachment ≤ 10 MiB, item combined ≤ 25 MiB. Additionally the SUM across all batch items must be ≤ 25 MiB (docs/design/batch-send.md §14 Q15)."`
}

// SendBatchRequest is the body of POST /v1/agents/{email}/batches. Each
// item in Messages is a self-contained near-SendEmailRequest; ReplyTo at
// this level is a batch-wide default applied to items that leave their
// own ReplyTo empty (the only MVP batch-level field per §1.2).
type SendBatchRequest struct {
	Messages []BatchMessage `json:"messages" minItems:"1" maxItems:"100" nullable:"false" doc:"1..100 BatchMessage items. Each item is a self-contained near-clone of SendEmailRequest minus from — its own to/cc/bcc/content/attachments/template/reply_to. Ordering is significant: results[] in the response is positionally aligned with this array."`
	ReplyTo  string         `json:"reply_to,omitempty" maxLength:"320" doc:"Optional batch-level Reply-To default. Applied to any item that leaves reply_to empty; a per-item value always wins. This is the ONLY batch-level default in MVP; every other field is per-item (docs/design/batch-send.md §1.2)."`
}

// BatchResult is one slot in the results[] array. Discriminated:
//   - Accepted item: MessageID non-empty, Suppressed absent.
//   - Suppressed item: MessageID empty, Suppressed populated.
type BatchResult struct {
	MessageID  string                 `json:"message_id,omitempty" doc:"Minted message id when the item was accepted (delivery_status='accepted' persisted and River outbound_send job enqueued). Absent when Suppressed is present."`
	Suppressed *BatchSuppressedResult `json:"suppressed,omitempty" doc:"Present when the item was dropped by the suppression-list filter (docs/design/batch-send.md §2.2). No message row exists for a suppressed slot; the caller can un-suppress via DELETE /v1/account/suppressions/{address} and resubmit."`
}

// BatchSuppressedResult describes one dropped item — the first
// suppression-list address found in the item's `to` list plus the
// suppressions.source category. Matches identity.BatchSuppressedItem
// (the durable record stored in batches.suppressed_json) minus the
// item_index which is implicit in results[]'s position.
type BatchSuppressedResult struct {
	Address string `json:"address" doc:"The recipient address in this item's to list that triggered the drop. If multiple addresses in the same item are suppressed, only the first is surfaced (dropping the whole item is enough signal)."`
	Reason  string `json:"reason" doc:"The suppression-list category from suppressions.source. Known values: bounce, complaint, manual. Open set — treat as string and tolerate unknown values."`
}

// SendBatchResponse is the 202 body of POST /v1/agents/{email}/batches.
// Results is positionally aligned with SendBatchRequest.Messages;
// Accepted/SuppressedCount are convenience counters callers can use
// without walking Results.
type SendBatchResponse struct {
	BatchID         string        `json:"batch_id" doc:"Durable id for this batch (bat_<base32>). Use GET /v1/batches/{batch_id} to retrieve the header + status rollup."`
	Results         []BatchResult `json:"results" nullable:"false" doc:"One slot per request item, positionally aligned. Each slot is either {message_id} (accepted) or {suppressed:{address,reason}} (dropped by the suppression filter)."`
	Accepted        int           `json:"accepted" doc:"Count of results[] slots that are {message_id}. Redundant with results[] but convenient for logging + zero-check."`
	SuppressedCount int           `json:"suppressed_count" doc:"Count of results[] slots that are {suppressed}. Zero when no per-item drops occurred."`
}

// sendBatchInput is the Huma input struct for POST /v1/agents/{email}/batches.
// Mirrors createMessageInput's shape (path email + Idempotency-Key header +
// RawBody for body-hash idempotency + Body).
type sendBatchInput struct {
	Address        string           `path:"email"`
	RawBody        []byte           // populated by Huma for idempotency body-hashing (see runIdempotent)
	IdempotencyKey string           `header:"Idempotency-Key" doc:"Optional idempotency key for safe retries. Same semantics as single-send: 24h TTL, path+body hash, replay returns the cached 202 verbatim (409 in-flight, 422 mismatch)."`
	Body           SendBatchRequest
}

// sendBatchOutput bridges the DeliverBatch result to the wire.
type sendBatchOutput struct {
	Status int
	Body   SendBatchResponse
}

// ---------------------------------------------------------------------------
// Registration (§1.1)
// ---------------------------------------------------------------------------

// maxBatchAttachmentBytes is the batch-wide combined-attachment cap (§14
// Q15). Applied on top of the per-item cap (already enforced by
// validateOutboundBody / validateAttachments).
const maxBatchAttachmentBytes = 25 * 1024 * 1024 // 25 MiB

// maxBatchRequestBodyBytes bounds the raw HTTP request body Huma will
// accept before returning 413 payload_too_large. Sized to comfortably
// fit 100 items at the per-item body caps (subject 2 KiB + text 1 MiB +
// html 1 MiB) plus the batch attachment ceiling (25 MiB) + overhead.
// Base-64 attachment encoding is 4/3 the byte size, so 25 MiB of
// attachments occupy ~33.3 MiB on the wire; add ~5 MiB per item × 100 =
// 500 MiB overhead theoretical max, but real batches don't put max
// bodies on every item — settle for 60 MiB.
const maxBatchRequestBodyBytes = 60 * 1024 * 1024 // 60 MiB

// batchAccepted202 returns the 202 response schema declaration for
// sendBatch. Mirrors accepted202 in outbound.go but for the batch shape.
func (s *Server) batchAccepted202() *huma.Response {
	return s.jsonResponse(reflect.TypeOf(SendBatchResponse{}), "SendBatchResponse",
		"Batch accepted — durably persisted, with up to len(request.messages) River outbound_send jobs enqueued. results[] is positionally aligned with request.messages; each slot is either {message_id} (accepted) or {suppressed} (dropped by the suppression filter — the request itself is not an error). An all-suppressed batch (every slot suppressed) is also a 202; accepted=0.")
}

// registerSendBatch is called from the API constructor to wire the
// sendBatch operation. Kept as a method-on-Server to match the other
// per-endpoint register* helpers in outbound.go.
func (s *Server) registerSendBatch() {
	huma.Register(s.API, huma.Operation{
		OperationID: "sendBatch",
		Method:      http.MethodPost,
		Path:        "/v1/agents/{email}/batches",
		Summary:     "Send a batch of up to 100 emails",
		Tags:        []string{"messages"},
		Description: "Fan out N independent emails in one API call. Each `messages[i]` item is a full send request in its own right (to/subject/body/template/attachments/reply_to) — the batch endpoint is essentially single-send in a loop, sharing rate-limit reservation and idempotency across all N items. Response `results[]` is positionally aligned with the input `messages[]`; each slot is either `{message_id}` (accepted) or `{suppressed: {address, reason}}` (dropped because a recipient was on this account's suppression list). See docs/design/batch-send.md for the full contract.\n\nMVP restrictions: HITL-enabled agents are refused with 403 `batch_hitl_unsupported` (§5.1); per-item content override is native (each item carries its own body or template_data); attachments are per-item with a 25 MiB batch-wide combined cap (§14 Q15); rate limits count as N sends (§4.2); duplicate recipients across items are rejected (§14 Q11). All error responses include `details.item_index` (or `details.item_indices`) to identify the offending item where relevant.",
		Security:     []map[string][]string{{"bearer": {}}},
		MaxBodyBytes: maxBatchRequestBodyBytes,
		Responses: map[string]*huma.Response{
			"202": s.batchAccepted202(),
			"400": s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
				"Bad Request — request-shape/validation failure. error.code includes invalid_request, invalid_recipient, too_many_messages, duplicate_recipient, invalid_attachment (undecodable base64). Per-item errors carry details.item_index identifying the offending messages[] index; batch-wide errors omit it."),
			"402":     s.limitExceededResponse(),
			"403":     s.errorEnvelopeResponse(),
			"409":     s.idempotencyInFlightResponse(),
			"413":     s.outboundPayloadTooLargeResponse(),
			"422":     s.idempotencyReuseResponse(),
			"429":     s.rateLimitedResponse(),
			"default": s.errorEnvelopeResponse(),
		},
	}, s.handleSendBatch)
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// handleSendBatch implements POST /v1/agents/{email}/batches per the
// design doc's accept-tx sketch (§9). Flow:
//  1. Auth + resolveOwnedAgent.
//  2. Structural validation on the batch as a whole (len, cross-item
//     dup, sum of attachments).
//  3. Idempotency claim (runIdempotent) wrapping the whole accept path.
//  4. Per-item validate + template resolve + compose (mirrors single-
//     send's prepare()).
//  5. Delegate to agent.API.DeliverBatch, which runs the accept-tx
//     (HITL gate, per-item screening, suppression partition, rate
//     reservation, DB tx that inserts batches + messages bulk + River
//     jobs + idempotency completion).
//  6. Map the BatchAcceptResult to the wire SendBatchResponse.
func (s *Server) handleSendBatch(ctx context.Context, in *sendBatchInput) (*sendBatchOutput, error) {
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	user, uerr := s.requireUser(ctx)
	if uerr != nil {
		return nil, uerr
	}
	body := in.Body
	if err := validateBatchStructure(&body); err != nil {
		return nil, err
	}

	// Idempotency route is agent-scoped (matches single-send). A batch
	// on the same key with an identical body replays the cached 202;
	// same key + different body → 422; concurrent same key → 409.
	route := "/v1/agents/" + ag.ID + "/batches"

	status, response, ferr := runIdempotentNS(s, ctx, user.ID, idemUserNS, in.IdempotencyKey, route, in.RawBody, func() (int, SendBatchResponse, error) {
		return s.deliverBatch(ctx, user, ag, body, in.IdempotencyKey)
	})
	if ferr != nil {
		return nil, ferr
	}
	return &sendBatchOutput{Status: status, Body: response}, nil
}

// deliverBatch runs the batch handler's per-item preparation and
// delegates to agent.API.DeliverBatch for the accept-tx. Extracted from
// handleSendBatch so the idempotency wrapper can invoke it as a closure.
func (s *Server) deliverBatch(ctx context.Context, user *identity.User, ag *identity.AgentIdentity, body SendBatchRequest, idemKey string) (int, SendBatchResponse, error) {
	items := make([]outbound.SendRequest, len(body.Messages))
	for i := range body.Messages {
		bm := body.Messages[i]

		// Per-item send-body shape (same checks as single-send).
		asSend := SendEmailRequest{
			To: bm.To, CC: bm.CC, BCC: bm.BCC,
			Subject: bm.Subject, Body: bm.Body, HTMLBody: bm.HTMLBody,
			TemplateID: bm.TemplateID, TemplateAlias: bm.TemplateAlias, TemplateData: bm.TemplateData,
			ConversationID: bm.ConversationID, ReplyTo: bm.ReplyTo, Attachments: bm.Attachments,
		}
		if env := validateSendTemplateShape(&asSend); env != nil {
			return 0, SendBatchResponse{}, envelopeWithItemIndex(env, i)
		}
		if env := s.resolveSendTemplate(ctx, user.ID, &asSend); env != nil {
			return 0, SendBatchResponse{}, envelopeWithItemIndex(env, i)
		}
		if env := s.validateOutboundBody(asSend.Subject, asSend.Body, asSend.To, asSend.CC, asSend.BCC, asSend.ConversationID); env != nil {
			return 0, SendBatchResponse{}, envelopeWithItemIndex(env, i)
		}

		// Apply the batch-level reply_to default only when the item
		// didn't set its own. Per-item ReplyTo always wins (§1.2).
		replyTo := asSend.ReplyTo
		if replyTo == "" {
			replyTo = body.ReplyTo
		}
		if env := validateReplyTo(replyTo); env != nil {
			return 0, SendBatchResponse{}, envelopeWithItemIndex(env, i)
		}

		items[i] = outbound.SendRequest{
			From:           ag.EmailAddress(),
			To:             asSend.To,
			CC:             asSend.CC,
			BCC:            asSend.BCC,
			Subject:        asSend.Subject,
			Body:           asSend.Body,
			HTMLBody:       asSend.HTMLBody,
			ConversationID: asSend.ConversationID,
			ReplyTo:        replyTo,
			Attachments:    asSend.Attachments,
		}
	}

	// Batch-wide checks that can only run after per-item template resolve
	// (rendered content is subject to the same per-item attachment cap).
	if env := checkBatchAttachmentSum(items); env != nil {
		return 0, SendBatchResponse{}, env
	}
	if env := checkCrossItemDuplicates(items); env != nil {
		return 0, SendBatchResponse{}, env
	}

	// Wire an idempotency-completion closure that writes inside the accept-tx.
	var idemCompleteTx agent.BatchAcceptIdemCompleter
	if idemKey != "" && s.deps.Idempotency != nil {
		nsKey := idemUserNS + idemKey
		uid := user.ID
		idemCompleteTx = func(ctx context.Context, tx pgx.Tx, result *agent.BatchAcceptResult) error {
			resp := sendBatchResponseFromAcceptResult(result)
			raw, mErr := json.Marshal(resp)
			if mErr != nil {
				raw = []byte("{}")
			}
			return s.deps.Idempotency.CompleteTx(ctx, tx, uid, nsKey, idempotency.CachedResponse{
				StatusCode:  http.StatusAccepted,
				ContentType: "application/json",
				Body:        raw,
			})
		}
	}

	if s.deps.DeliverBatch == nil {
		return 0, SendBatchResponse{}, NewError(http.StatusNotImplemented, "not_implemented",
			"batch send is not enabled on this deployment")
	}
	result, oerr := s.deps.DeliverBatch(ctx, user, ag, items, idemCompleteTx)
	if oerr != nil {
		return 0, SendBatchResponse{}, envelopeFromOutboundError(oerr)
	}
	return http.StatusAccepted, sendBatchResponseFromAcceptResult(result), nil
}

// sendBatchResponseFromAcceptResult maps the agent-layer BatchAcceptResult
// into the wire-facing SendBatchResponse. Positionally aligned; counts
// are derived from the item shapes.
func sendBatchResponseFromAcceptResult(r *agent.BatchAcceptResult) SendBatchResponse {
	if r == nil {
		return SendBatchResponse{}
	}
	resp := SendBatchResponse{BatchID: r.BatchID, Results: make([]BatchResult, len(r.Items))}
	for i, item := range r.Items {
		if item.Suppressed != nil {
			resp.Results[i] = BatchResult{Suppressed: &BatchSuppressedResult{
				Address: item.Suppressed.Address,
				Reason:  item.Suppressed.Reason,
			}}
			resp.SuppressedCount++
			continue
		}
		resp.Results[i] = BatchResult{MessageID: item.MessageID}
		resp.Accepted++
	}
	return resp
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

// validateBatchStructure runs the checks that don't depend on template
// resolution: array bounds. Per-item structural checks + cross-item dup
// + attachment sum are done AFTER template render inside deliverBatch,
// because rendered content shape informs some of them.
func validateBatchStructure(body *SendBatchRequest) *ErrorEnvelope {
	n := len(body.Messages)
	if n < 1 || n > 100 {
		return NewError(http.StatusBadRequest, "too_many_messages",
			fmt.Sprintf("messages[] must contain 1..100 items; got %d", n)).
			WithDetails(TooManyMessagesDetails{MaxMessages: 100, Provided: n})
	}
	return nil
}

// checkBatchAttachmentSum enforces the §14 Q15 batch-wide cap: the sum
// of every item's attachment byte totals must be ≤ 25 MiB. The per-item
// caps (single attachment ≤ 10 MiB, item combined ≤ 25 MiB) are enforced
// by the shared attachment validator on the composed SendRequest.
func checkBatchAttachmentSum(items []outbound.SendRequest) *ErrorEnvelope {
	var total int64
	for _, item := range items {
		for _, att := range item.Attachments {
			// Per-item validateAttachments has already accepted every
			// item's attachments individually, so each Data is valid
			// base64. Whitespace-strip + decode gives the exact byte
			// count for the batch sum, matching the per-item cap
			// accounting.
			clean := stripBase64Whitespace(att.Data)
			decoded, err := base64.StdEncoding.DecodeString(clean)
			if err != nil {
				// Should not happen — per-item validation ran first —
				// but if it does, fall back to an approximation so we
				// still enforce a bound rather than silently pass a
				// giant payload through.
				total += int64(base64.StdEncoding.DecodedLen(len(clean)))
				continue
			}
			total += int64(len(decoded))
		}
	}
	if total > maxBatchAttachmentBytes {
		return NewError(http.StatusRequestEntityTooLarge, "payload_too_large",
			"combined attachment bytes across all batch items exceed the batch cap").
			WithDetails(PayloadTooLargeDetails{
				Scope:       "batch",
				ActualBytes: total,
				MaxBytes:    maxBatchAttachmentBytes,
			})
	}
	return nil
}

// stripBase64Whitespace removes CR/LF/space/tab that RFC 2045 allows in a
// base64 body, matching the pre-decode step validateAttachments does.
func stripBase64Whitespace(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

// checkCrossItemDuplicates rejects a batch where the same recipient
// address appears in the `to` set of two or more items. Silent dedupe
// would send N-k messages when the caller asked for N and hide the
// input mistake — §14 Q11 chose reject. Only cross-item `to` duplicates
// are checked; duplicates within one item (or in cc/bcc) are the same
// pattern the existing single-send validator already handles.
func checkCrossItemDuplicates(items []outbound.SendRequest) *ErrorEnvelope {
	seen := map[string][]int{}
	for i, item := range items {
		for _, addr := range item.To {
			norm := strings.ToLower(strings.TrimSpace(addr))
			seen[norm] = append(seen[norm], i)
		}
	}
	for addr, indexes := range seen {
		if len(indexes) < 2 {
			continue
		}
		return NewError(http.StatusBadRequest, "duplicate_recipient",
			"same recipient address appears in more than one batch item").
			WithDetails(DuplicateRecipientDetails{Address: addr, ItemIndices: indexes})
	}
	return nil
}

// envelopeWithItemIndex is a helper for stamping an item_index onto a
// per-item validation error. It mutates the details in place when the
// error carries a typed detail struct known to have an ItemIndex field
// (currently just PayloadTooLargeDetails); otherwise it falls back to a
// generic details.item_index injection so the caller can still find the
// offending item.
func envelopeWithItemIndex(env *ErrorEnvelope, itemIndex int) *ErrorEnvelope {
	if env == nil {
		return nil
	}
	// The typed details are opaque here — we can't reach in and set
	// ItemIndex without knowing the type. Simplest is to overlay a
	// generic map that both preserves the code and stamps the index.
	// Callers reading `details.item_index` see it either way.
	if env.Err.Details == nil {
		env.Err.Details = map[string]any{"item_index": itemIndex}
		return env
	}
	// If details is already a map, augment in place.
	if m, ok := env.Err.Details.(map[string]any); ok {
		m["item_index"] = itemIndex
		return env
	}
	// If details is a typed struct, re-marshal into a map and add the
	// field. This keeps every existing field name intact while adding
	// item_index.
	raw, err := json.Marshal(env.Err.Details)
	if err != nil {
		env.Err.Details = map[string]any{"item_index": itemIndex}
		return env
	}
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	m["item_index"] = itemIndex
	env.Err.Details = m
	return env
}
