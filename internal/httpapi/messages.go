package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/mailparse"
)

// MessageView is the full single-message representation: a strict superset of
// MessageSummaryView plus body/parsed/raw_message. The inbox
// read-state is exposed as `read_status` (MSG-1); the four status axes are
// read_status / hitl_status / delivery_status / webhook_status.
type MessageView struct {
	ID             string                    `json:"id"`
	HeaderFrom     *string                   `json:"header_from" nullable:"true" doc:"Parsed RFC 5322 From address for inbound mail or the sender identity for outbound mail; null when unavailable and never replaced by Reply-To."`
	EnvelopeFrom   *string                   `json:"envelope_from" nullable:"true" doc:"SMTP MAIL FROM address for inbound SMTP delivery; null for outbound messages, a null reverse path, or providerless delivery."`
	VerifiedDomain *string                   `json:"verified_domain" nullable:"true" doc:"RFC 5322 Author Domain validated by an aligned DMARC pass. Null for non-pass verdicts and deliveries without inbound SMTP evaluation — this includes dmarc.status=none (sender publishes no DMARC record, common and NOT itself suspicious) as well as dmarc.status=fail (an actual mismatch). Only DMARC ties a passing SPF or DKIM identity back to this header domain; a bare SPF or DKIM pass without DMARC does not. This authenticates the domain, not the address local part, individual sender, or message content."`
	Authentication *emailauth.Authentication `json:"authentication" doc:"Inbound SMTP authentication evidence. Only dmarc.status=pass authenticates the RFC 5322 From domain; even a pass does not authenticate the mailbox local part, a person, or message content. Null means there was no authenticating inbound SMTP peer, as with outbound or providerless loopback delivery."`
	To             []string                  `json:"to" nullable:"false"`
	CC             []string                  `json:"cc" nullable:"false"`
	ReplyTo        []string                  `json:"reply_to" nullable:"false" doc:"The parsed Reply-To header of an inbound message. Populated for inbound only; always empty for outbound (a Reply-To you SET on a send is a request-side field on the send/reply/forward body and is not echoed back here)."`
	Recipient      string                    `json:"delivered_to" doc:"The envelope Delivered-To address — this delivery's per-agent target (the mailbox that actually received this row), distinct from the To header (the to array)."`
	Subject        string                    `json:"subject"`
	ConversationID string                    `json:"conversation_id"`
	// Direction (inbound|outbound) — mirrors MessageSummaryView so a client
	// fetching a single message keeps the full trust-axis context (review F1).
	// Deliberately a CLOSED enum despite being response-side: direction is a
	// binary invariant of the model, not an evolving vocabulary.
	Direction string `json:"direction" enum:"inbound,outbound"`
	// Status is the inbox read-state (unread|read; "" for outbound). Exposed as
	// `read_status` (MSG-1) to disambiguate from hitl_status/delivery_status/
	// webhook_status — the conflation that caused bug B2. Left open (not an enum)
	// because outbound rows carry "".
	Status string `json:"read_status"`
	// HITLStatus is the review-hold lifecycle (e.g. pending_review) — outbound
	// only, mirroring MessageSummaryView. Exposed as `review_status` (the holds
	// vocabulary unified on `review` in migration 044). Distinct from read_status,
	// delivery_status, and webhook_status (each a separate axis).
	HITLStatus string `json:"review_status,omitempty" doc:"Review-hold lifecycle (outbound only). Open set; tolerate unknown values. Known values: pending_review, sent, review_rejected, review_expired_approved, review_expired_rejected. Note: an APPROVED outbound hold reads as sent here — the message view intentionally collapses the approved outcome into the delivery lifecycle. The distinct review_approved spelling appears only in the approve result (SendResultView.status, for inbound release) and the email.review_approved webhook event, not in this field."`
	// WebhookStatus / WebhookError mirror MessageSummaryView so the detail view
	// is a strict superset of the list item (a client fetching one message keeps
	// the webhook delivery context). Apply to both directions; omitempty hides
	// the empty case.
	WebhookStatus string `json:"webhook_status,omitempty"`
	WebhookError  string `json:"webhook_error,omitempty"`
	// SizeBytes is the RAW MIME byte length of the whole stored message
	// (octet length of raw_message) — headers + bodies + encoded attachments as
	// transported. Mirrors MessageSummaryView. Distinct from the per-attachment
	// size_bytes in attachments[], which is the DECODED payload size of one
	// attachment. This raw length is also the dominant term of storage-quota
	// accounting: usage.storage_bytes sums raw_message plus any retained
	// held-draft body columns per message (see AccountView).
	SizeBytes int `json:"size_bytes,omitempty" doc:"RAW MIME byte length of the whole stored message (headers + bodies + encoded attachments as transported). Distinct from attachments[].size_bytes, which is one attachment's DECODED payload size. This value is the dominant term of the account's storage-quota accounting (usage.storage_bytes)."`
	// DeliveryStatus is the outbound delivery rollup (migration 031:
	// 'sent', 'delivered', 'bounced', …) — the worst recipient status by
	// precedence. Outbound-only; omitted on inbound messages.
	DeliveryStatus string `json:"delivery_status,omitempty" doc:"Outbound delivery rollup (worst recipient status by precedence; outbound only). Open set; tolerate unknown values. Known values: accepted, sending, sent, delivered, deferred, bounced, complained, failed. Lifecycle: accepted → sending → sent → delivered | deferred | bounced | complained | failed. (Legacy 'queued' is superseded by 'accepted'.)"`
	// DeliveryDetail is the human-readable diagnostic for the delivery
	// rollup (e.g. bounce sub-type / SMTP response). Outbound-only.
	DeliveryDetail string `json:"delivery_detail,omitempty"`
	// SentAs is the From identity actually used at relay accept time.
	// Outbound-only; omitted on inbound messages.
	SentAs string `json:"sent_as,omitempty" doc:"From identity used at relay accept time (outbound only). Open set; tolerate unknown values. Known values: own_address, relay."`
	// ScheduledAt is the future instant a scheduled outbound send was queued to be
	// submitted (migration 079). Set on outbound rows created with a future
	// send_at and retained afterwards (it records the scheduled instant and is not
	// cleared once the send fires); omitted for immediate sends and all inbound
	// rows. delivery_status stays 'accepted' while scheduled.
	ScheduledAt *time.Time `json:"scheduled_at,omitempty" format:"date-time" doc:"Future instant a scheduled outbound send was queued to be submitted (outbound only; treat as \"not before\"). Set when the message was created with a future send_at and retained afterwards; omitted for immediate sends. Cancel a scheduled send by moving the message to trash — reversible: restoring it before the send time re-arms it."`
	// Flagged + FlagReason carry the beta inbound ingestion verdict: true when
	// the agent's inbound-policy gate flagged this message on arrival while still
	// delivering it. Polling agents need this signal because no review item is
	// created for the flag action.
	Flagged    bool   `json:"flagged,omitempty"`
	FlagReason string `json:"flag_reason,omitempty"`
	// HoldReason is populated only by GET /v1/reviews/{id}. The shared
	// messageViewFromIdentity constructor deliberately leaves it nil so review
	// context never leaks onto agent-facing message APIs.
	HoldReason *HoldReasonView `json:"hold_reason,omitempty"`
	// Protection is the per-producer screening breakdown behind the hold (the
	// detector categories + rationale that explain hold_reason). Review surface
	// only; populated by the review-detail handler, never on the agent /messages
	// path. Beta.
	Protection []ProtectionFindingView `json:"protection,omitempty" doc:"Screening breakdown behind the hold — detector categories + rationale (review surface only, beta)."`
	Labels     []string                `json:"labels" nullable:"false"`
	// CreatedAt is emitted as a full-precision RFC3339Nano date-time (time.Time),
	// consistent with every other timestamp in the surface. This is the keyset
	// pagination ORDERING key, so it must NOT be truncated to whole seconds —
	// two messages in the same second would otherwise be indistinguishable on the
	// wire even though the cursor orders them at finer granularity.
	CreatedAt time.Time `json:"created_at" format:"date-time"`
	// DeletedAt marks a message in the trash (soft-deleted): set when it was
	// moved there, omitted on live messages. Trashed messages appear only in
	// the deleted=true list view and this single-message get; they are purged
	// ~30 days after deletion (docs/design/trash-soft-delete.md).
	DeletedAt  *time.Time `json:"deleted_at,omitempty" format:"date-time" doc:"When the message was moved to the trash. Omitted for live messages. A trashed message is restorable until purged — 30 days after deletion by default (deployment-configurable). Live message data is otherwise retained indefinitely."`
	RawMessage []byte     `json:"raw_message" nullable:"true" doc:"Base64-encoded canonical RAW MIME. Required but null while an outbound message is pending review because reviewer-editable content lives in body until approval composes the final MIME; non-null for inbound and composed outbound messages."`
	// Parsed is the derived view (decision 9 / Slice 4b-3): the raw message
	// rendered to text (`text`, quoted chains stripped + length-capped, for the
	// agent to feed a model by default) plus the decoded HTML part (`html`, for
	// display). Present on any message carrying raw MIME — inbound and sent
	// outbound. A CONVENIENCE — when raw_message is non-null, it is canonical;
	// the security decision is made on `auth` + provenance, never on this derived
	// body. Held outbound drafts have raw_message=null and use Body below.
	Parsed *MessageParsedView `json:"parsed,omitempty"`
	// Body is the mutable draft body for a held outbound message
	// (status=pending_review), which has no raw_message yet. This is the
	// second representation the unified read exposes (decision 9): held drafts
	// carry body_text/body_html, sent/inbound carry raw_message. Omitted when
	// empty (sent/inbound rows).
	Body *MessageBodyView `json:"body,omitempty"`
	// Attachments is per-attachment METADATA (never bytes) parsed server-side
	// from raw_message — the authoritative, stable attachment index (§6a #5).
	// Fetch the bytes via GET …/messages/{id}/attachments/{index}. Always
	// present (empty when none); held drafts (no raw_message) carry [].
	Attachments []AttachmentMetaView `json:"attachments" nullable:"false"`
}

// AttachmentMetaView is metadata for one attachment of a message — never the
// bytes. `index` is the stable 0-based attachment index (document order) used to
// fetch the bytes via the attachment endpoint. SizeBytes is the DECODED
// payload size (Content-Transfer-Encoding undone) — the size of the file a
// download yields, NOT the encoded size inside the raw MIME and NOT the
// message-level size_bytes (raw MIME length of the whole message).
//
// Alias of the canonical eventpayload.AttachmentMetaView so the REST message
// views, the stable event payloads, and the account export publish ONE public
// attachment-metadata component schema.
type AttachmentMetaView = eventpayload.AttachmentMetaView

// MessageParsedView is the parsed-body payload (see MessageView.Parsed).
type MessageParsedView struct {
	// Text is the injection-reduced plain body: text/plain preferred (else
	// HTML→text), quoted reply/forward chains stripped, length-capped. This is
	// what an agent feeds a model by default.
	Text string `json:"text"`
	// Truncated is true when the length cap cut `text`.
	Truncated bool `json:"truncated"`
	// HTML is the decoded text/html part for display, present only when the
	// message carries an HTML part. Full fidelity (NOT quote-stripped, unlike
	// `text`) — render it sanitized/sandboxed; it is untrusted sender content.
	// Omitted for text-only messages; `raw_message` stays the canonical copy.
	HTML string `json:"html,omitempty"`
}

// MessageBodyView is the held-draft body (see MessageView.Body).
type MessageBodyView struct {
	Text string `json:"text,omitempty"`
	HTML string `json:"html,omitempty"`
}

func messageViewFromIdentity(m *identity.Message) MessageView {
	v := MessageView{
		ID:             m.ID,
		HeaderFrom:     messageHeaderFrom(m),
		EnvelopeFrom:   nullableMessageString(m.EnvelopeFrom),
		VerifiedDomain: m.Authentication.VerifiedDomain(),
		Authentication: m.Authentication,
		To:             orEmptyStrings(m.ToRecipients),
		CC:             orEmptyStrings(m.CC),
		ReplyTo:        inboundReplyToView(m),
		Recipient:      m.Recipient,
		Subject:        m.Subject,
		ConversationID: m.ConversationID,
		Direction:      m.Direction,
		// `status` is the inbox read-state, identical to the summary view (B2);
		// the outbound delivery rollup lives in `delivery_status`, the HITL
		// lifecycle in `hitl_status`. (The store resolves m.DeliveryStatus to
		// inbox_status for inbound and the rollup for outbound, so the detail
		// view must read InboxStatus to agree with the summary.)
		Status:     m.InboxStatus,
		Labels:     orEmptyStrings(m.Labels),
		CreatedAt:  m.CreatedAt.UTC(),
		DeletedAt:  utcPtr(m.DeletedAt),
		RawMessage: m.RawMessage,
		Flagged:    m.Flagged,
		FlagReason: m.FlagReason,
	}
	// Webhook delivery context + raw size — apply to both directions so the
	// detail view stays a superset of the summary view (omitempty hides empties).
	v.WebhookStatus = m.WebhookStatus
	v.WebhookError = m.WebhookError
	v.SizeBytes = m.SizeBytes
	// Outbound delivery feedback (migration 031). On outbound rows
	// identity.Message.DeliveryStatus carries the delivery rollup; on
	// inbound rows it carries inbox_status, so these stay empty there.
	if m.Direction == "outbound" {
		v.DeliveryStatus = m.DeliveryStatus
		v.DeliveryDetail = m.DeliveryDetail
		v.SentAs = m.SentAs
		v.ScheduledAt = utcPtr(m.ScheduledAt)
		// HITL lifecycle (status column) — outbound only, mirroring the summary
		// view; on inbound rows `status` is not the HITL value (review F1).
		v.HITLStatus = m.Status
	}
	// Parsed view (decision 9): derived from the raw message — any direction
	// that carries one (inbound + sent outbound). Outbound draft columns remain
	// retained after terminal transitions; held drafts have no raw_message yet.
	if len(m.RawMessage) > 0 {
		pv := mailparse.Parse(m.RawMessage, mailparse.DefaultMaxBytes)
		v.Parsed = &MessageParsedView{Text: pv.Text, Truncated: pv.Truncated, HTML: pv.HTML}
	}
	// Attachment metadata (§6a #5): parsed from raw_message for ANY direction
	// that has one (inbound + sent outbound). Always an array; the bytes are
	// fetched via the attachment endpoint, never inlined here.
	v.Attachments = []AttachmentMetaView{}
	if len(m.RawMessage) > 0 {
		for i, a := range mailparse.Attachments(m.RawMessage) {
			v.Attachments = append(v.Attachments, AttachmentMetaView{
				Index:       i,
				Filename:    a.Filename,
				ContentType: a.ContentType,
				SizeBytes:   len(a.Data),
				ContentID:   a.ContentID,
			})
		}
	}
	// Held-draft body (decision 9 unification): the second representation a
	// pending_review outbound message carries instead of raw_message. Gated on
	// outbound direction so it can never surface on an inbound row even if a
	// future load path populates the body columns.
	if m.Direction == "outbound" && (m.BodyText != "" || m.BodyHTML != "") {
		v.Body = &MessageBodyView{Text: m.BodyText, HTML: m.BodyHTML}
	}
	return v
}

// MessageIDParam is the path input for single-message operations.
type MessageIDParam struct {
	Address   string `path:"email" doc:"The agent's full email address."`
	MessageID string `path:"id" doc:"The message id, e.g. msg_abc123."`
}

type messageOutput struct {
	Body MessageView
}

// MessageSummaryView is the lightweight list representation. It mirrors the
// legacy messageSummary json shape field-for-field (Slice 1 keeps the item
// shape; only the *pagination envelope* changes to the standardized
// items/next_cursor — §4 decision 7). Replicated here rather than imported
// from the legacy agent package so the new layer carries no backwards
// dependency on the surface it replaces; it moves home when legacy is
// deleted at the 1Z cutover.
type MessageSummaryView struct {
	ID string `json:"id"`
	// Deliberately a CLOSED enum despite being response-side: direction is a
	// binary invariant of the model, not an evolving vocabulary.
	Direction      string   `json:"direction" enum:"inbound,outbound"`
	HeaderFrom     *string  `json:"header_from" nullable:"true" doc:"Parsed RFC 5322 From address for inbound mail or the sender identity for outbound mail; null when unavailable and never replaced by Reply-To."`
	EnvelopeFrom   *string  `json:"envelope_from" nullable:"true" doc:"SMTP MAIL FROM address for inbound SMTP delivery; null for outbound messages, a null reverse path, or providerless delivery."`
	VerifiedDomain *string  `json:"verified_domain" nullable:"true" doc:"RFC 5322 Author Domain validated by an aligned DMARC pass. Null otherwise — including dmarc.status=none (no DMARC record published, common and NOT itself suspicious), not just dmarc.status=fail (an actual mismatch). Only DMARC ties a passing SPF or DKIM identity back to this header domain; a bare SPF or DKIM pass without DMARC does not. This authenticates the domain, not the address local part, individual sender, or message content."`
	To             []string `json:"to" nullable:"false"`
	CC             []string `json:"cc,omitempty" nullable:"false"`
	ReplyTo        []string `json:"reply_to,omitempty" nullable:"false" doc:"The parsed Reply-To header of an inbound message. Populated for inbound only; always empty for outbound (a Reply-To you SET on a send is a request-side field and is not echoed back here)."`
	Recipient      string   `json:"delivered_to" doc:"The envelope Delivered-To address — this delivery's per-agent target (the mailbox that actually received this row), distinct from the To header (the to array)."`
	Subject        string   `json:"subject"`
	ConversationID string   `json:"conversation_id,omitempty"`
	// Status is the inbox read-state, exposed as `read_status` (MSG-1).
	Status        string `json:"read_status"`
	HITLStatus    string `json:"review_status,omitempty" doc:"Review-hold lifecycle (outbound only). Open set; tolerate unknown values. Known values: pending_review, sent, review_rejected, review_expired_approved, review_expired_rejected. Note: an APPROVED outbound hold reads as sent here — the message view intentionally collapses the approved outcome into the delivery lifecycle. The distinct review_approved spelling appears only in the approve result (SendResultView.status, for inbound release) and the email.review_approved webhook event, not in this field."`
	WebhookStatus string `json:"webhook_status,omitempty"`
	WebhookError  string `json:"webhook_error,omitempty"`
	// DeliveryStatus / DeliveryDetail / SentAs are the outbound delivery
	// rollup (migration 031). Outbound-only; omitted on inbound rows.
	DeliveryStatus string `json:"delivery_status,omitempty" doc:"Outbound delivery rollup (worst recipient status by precedence; outbound only). Open set; tolerate unknown values. Known values: accepted, sending, sent, delivered, deferred, bounced, complained, failed. Lifecycle: accepted → sending → sent → delivered | deferred | bounced | complained | failed. (Legacy 'queued' is superseded by 'accepted'.)"`
	DeliveryDetail string `json:"delivery_detail,omitempty"`
	SentAs         string `json:"sent_as,omitempty" doc:"From identity used at relay accept time (outbound only). Open set; tolerate unknown values. Known values: own_address, relay."`
	// ScheduledAt mirrors MessageView.ScheduledAt on list rows (migration 079):
	// the future instant a scheduled outbound send is queued to be submitted.
	// Outbound-only and present only when a future send_at was set — omitted
	// otherwise — so a list consumer can distinguish a scheduled send from an
	// ordinary queued one without a per-message drill-down.
	ScheduledAt *time.Time `json:"scheduled_at,omitempty" format:"date-time" doc:"Future instant a scheduled outbound send was queued to be submitted (outbound only; treat as \"not before\"). Present while a future send_at is set and retained afterwards; omitted for immediate sends and inbound rows."`
	// Flagged + FlagReason are the beta inbound ingestion verdict. They remain in
	// list projections so polling agents can identify delivered flag outcomes
	// without a per-message drill-down.
	Flagged    bool   `json:"flagged,omitempty"`
	FlagReason string `json:"flag_reason,omitempty"`
	// SizeBytes is the RAW MIME byte length of the whole stored message —
	// same semantics as MessageView.SizeBytes (see there for the full note,
	// including its role as the dominant term of storage-quota accounting).
	SizeBytes int      `json:"size_bytes,omitempty" doc:"RAW MIME byte length of the whole stored message (headers + bodies + encoded attachments as transported). Distinct from an attachment's size_bytes, which is its DECODED payload size. This value is the dominant term of the account's storage-quota accounting (usage.storage_bytes)."`
	Labels    []string `json:"labels" nullable:"false"`
	// CreatedAt is the keyset pagination ordering key, emitted at full RFC3339Nano
	// precision (time.Time) so sub-second ordering is visible on the wire.
	CreatedAt time.Time `json:"created_at" format:"date-time"`
	// DeletedAt marks a message in the trash — set on rows of the deleted=true
	// list view, omitted on live messages. See MessageView.DeletedAt.
	DeletedAt *time.Time `json:"deleted_at,omitempty" format:"date-time" doc:"When the message was moved to the trash. Omitted for live messages. A trashed message is restorable until purged — 30 days after deletion by default (deployment-configurable). Live message data is otherwise retained indefinitely."`
}

// inboundReplyToView returns the parsed inbound Reply-To for the wire view. The
// reply_to field is defined as the Reply-To header PARSED off an inbound message;
// on outbound rows the same column now doubles as internal storage for a caller's
// Reply-To OVERRIDE (so a held send survives the approval recompose), which is a
// different concept and must not leak into the message view. Gate on direction so
// the field keeps its single documented meaning.
func inboundReplyToView(m *identity.Message) []string {
	if m.Direction != "inbound" {
		return []string{}
	}
	return orEmptyStrings(m.ReplyTo)
}

func messageSummaryFromIdentity(m identity.Message) MessageSummaryView {
	s := MessageSummaryView{
		ID:             m.ID,
		Direction:      m.Direction,
		HeaderFrom:     messageHeaderFrom(&m),
		EnvelopeFrom:   nullableMessageString(m.EnvelopeFrom),
		VerifiedDomain: m.Authentication.VerifiedDomain(),
		To:             orEmptyStrings(m.ToRecipients),
		CC:             orEmptyStrings(m.CC),
		ReplyTo:        inboundReplyToView(&m),
		Recipient:      m.Recipient,
		Subject:        m.Subject,
		ConversationID: m.ConversationID,
		Status:         m.InboxStatus,
		SizeBytes:      m.SizeBytes,
		Labels:         orEmptyStrings(m.Labels),
		CreatedAt:      m.CreatedAt.UTC(),
		DeletedAt:      utcPtr(m.DeletedAt),
		Flagged:        m.Flagged,
		FlagReason:     m.FlagReason,
	}
	if m.Direction == "outbound" {
		s.HITLStatus = m.Status
		s.WebhookStatus = m.WebhookStatus
		s.WebhookError = m.WebhookError
		// On outbound rows identity.Message.DeliveryStatus carries the
		// delivery rollup (migration 031); inbound rows carry inbox_status,
		// already surfaced as Status above.
		s.DeliveryStatus = m.DeliveryStatus
		s.DeliveryDetail = m.DeliveryDetail
		s.SentAs = m.SentAs
		s.ScheduledAt = utcPtr(m.ScheduledAt)
	}
	return s
}

func messageHeaderFrom(m *identity.Message) *string {
	if m.HeaderFrom != "" {
		return nullableMessageString(m.HeaderFrom)
	}
	if m.Direction == "outbound" {
		return nullableMessageString(m.Sender)
	}
	return nil
}

func nullableMessageString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

// ListMessagesInput is the typed query surface for the message list. Cursor
// pagination (cursor/limit) replaces the legacy page_size/token (§4
// decision 7); the filters preserve legacy semantics.
type ListMessagesInput struct {
	Address         string   `path:"email"`
	Direction       string   `query:"direction" enum:"inbound,outbound,all" doc:"Defaults to inbound."`
	Status          string   `query:"read_status" enum:"unread,read,all" doc:"Inbound only. Filters by inbox read-state (MSG-1). Defaults to unread for inbound, all otherwise."`
	Sort            string   `query:"sort" enum:"asc,desc" doc:"Defaults to desc (newest first)."`
	From            string   `query:"from" doc:"Case-insensitive substring match on sender."`
	SubjectContains string   `query:"subject_contains" doc:"Case-insensitive substring match on subject."`
	ConversationID  string   `query:"conversation_id"`
	Labels          []string `query:"labels" doc:"Comma-separated list (e.g. labels=urgent,follow-up); AND-matched — a message must carry every given label."`
	Since           string   `query:"since" doc:"RFC3339; created_at >= since."`
	Until           string   `query:"until" doc:"RFC3339; created_at < until."`
	Cursor          string   `query:"cursor"`
	Limit           int      `query:"limit" minimum:"1" maximum:"100" default:"100"`
	Deleted         bool     `query:"deleted" doc:"List the trash instead: messages that were soft-deleted and are restorable until purged (30 days after deletion by default, deployment-configurable). Defaults to false (live messages only)."`
}

type listMessagesOutput struct {
	Body Page[MessageSummaryView]
}

// messagesCursor is the opaque continuation payload. It captures the last
// row's position plus the full filter identity so a continuation request
// can't silently change the result set under the cursor.
type messagesCursor struct {
	CreatedAt       time.Time `json:"c"`
	ID              string    `json:"i"`
	Status          string    `json:"s"`
	Direction       string    `json:"d"`
	AgentID         string    `json:"a"`
	Sort            string    `json:"so"`
	From            string    `json:"f,omitempty"`
	SubjectContains string    `json:"sc,omitempty"`
	ConversationID  string    `json:"cv,omitempty"`
	Since           string    `json:"sn,omitempty"`
	Until           string    `json:"un,omitempty"`
	Labels          []string  `json:"lb,omitempty"`
	Deleted         bool      `json:"dl,omitempty"`
}

func (s *Server) registerMessages() {
	huma.Register(s.API, huma.Operation{
		OperationID: "listMessages",
		Method:      http.MethodGet,
		Path:        "/v1/agents/{email}/messages",
		Summary:     "List messages",
		Description: "List an agent's messages (inbound + outbound) with filters and cursor pagination. Held outbound drafts appear as status=pending_review. Pass deleted=true for the trash (soft-deleted messages, restorable until purged — 30 days after deletion by default, deployment-configurable); the trash view defaults to direction=all and read_status=all.",
		Tags:        []string{"messages"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleListMessages)

	huma.Register(s.API, huma.Operation{
		OperationID: "deleteMessage",
		Method:      http.MethodDelete,
		Path:        "/v1/agents/{email}/messages/{id}",
		Summary:     "Delete a message (move to trash)",
		Description: "Move a message to the trash. Trashed messages disappear from lists, threads, and reply targets, but can be restored via POST …/messages/{id}/restore until they are purged — 30 days after deletion by default (the trash retention window is deployment-configurable). Live message data is otherwise retained indefinitely. No confirmation is required because the default delete is reversible. Pass permanent=true with confirm=DELETE to permanently delete a message that is ALREADY in the trash (\"delete forever\"). A message held for review (review_status=pending_review) cannot be deleted — resolve it in the review queue first (409 message_held).",
		Tags:        []string{"messages"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleDeleteMessage)

	huma.Register(s.API, huma.Operation{
		OperationID: "restoreMessage",
		Method:      http.MethodPost,
		Path:        "/v1/agents/{email}/messages/{id}/restore",
		Summary:     "Restore a message from the trash",
		Description: "Bring a trashed (soft-deleted) message back to the inbox. Restored message data is retained indefinitely unless it is deleted again. Returns the restored message. 409 not_in_trash when the message is not in the trash.",
		Tags:        []string{"messages"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleRestoreMessage)

	huma.Register(s.API, huma.Operation{
		OperationID: "getMessage",
		Method:      http.MethodGet,
		Path:        "/v1/agents/{email}/messages/{id}",
		Summary:     "Get a message",
		Description: "Fetch a single message (inbound or outbound) by id, scoped to an agent the caller owns. A trashed message remains readable by this direct GET and includes deleted_at until it is permanently purged (30 days after deletion by default, deployment-configurable); ordinary lists, conversations, reply targets, and forward targets exclude it. Includes the raw message and canonical inbound authentication evidence. Fetching an unread inbound message marks it read as a side effect.",
		Tags:        []string{"messages"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *MessageIDParam) (*messageOutput, error) {
		ag, err := s.resolveOwnedAgent(ctx, in.Address)
		if err != nil {
			return nil, err
		}
		if s.deps.GetMessage == nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "message lookup unavailable")
		}
		msg, err := s.deps.GetMessage(ctx, in.MessageID, ag.ID)
		if err != nil || msg == nil {
			return nil, NewError(http.StatusNotFound, "not_found", "message not found")
		}
		return &messageOutput{Body: messageViewFromIdentity(msg)}, nil
	})

	huma.Register(s.API, huma.Operation{
		OperationID: "updateMessage",
		Method:      http.MethodPatch,
		Path:        "/v1/agents/{email}/messages/{id}",
		Summary:     "Update a message (labels)",
		Description: "Apply a labels delta (`add_labels` / `remove_labels`) to a message the caller owns; returns the post-update label set. Each list is capped at 50 entries; labels are lowercase `[a-z0-9:_-]+` up to 64 chars; the `e2a:` prefix is reserved for system labels. A message carries at most 100 labels. An empty delta is a read of the current labels.",
		Tags:        []string{"messages"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleUpdateMessage)
}

// UpdateMessageRequest is the labels-delta body for PATCH …/messages/{id}.
// A label in both add and remove is removed (remove wins, per the store).
type UpdateMessageRequest struct {
	AddLabels    []string `json:"add_labels,omitempty" nullable:"false"`
	RemoveLabels []string `json:"remove_labels,omitempty" nullable:"false"`
}

type updateMessageInput struct {
	Address string `path:"email"`
	ID      string `path:"id"`
	Body    UpdateMessageRequest
}

// UpdateMessageResultView echoes the post-update label set so callers can
// reflect state without a follow-up fetch.
type UpdateMessageResultView struct {
	MessageID string   `json:"message_id"`
	Labels    []string `json:"labels" nullable:"false"`
}

type updateMessageOutput struct {
	Body UpdateMessageResultView
}

// handleUpdateMessage applies a labels delta (PATCH
// /v1/agents/{email}/messages/{id}; replaced the now-removed legacy
// /v1 PATCH). This is a per-agent operation,
// so an agent-scoped credential may label its own messages — it goes through
// resolveOwnedAgent (which pins an agent-scoped credential to its bound agent),
// NOT requireAccountScope. Label rules are validated via the shared
// agent.NormalizeAndValidateLabelList so they can't drift from the legacy
// surface; the store enforces the per-message cap.
func (s *Server) handleUpdateMessage(ctx context.Context, in *updateMessageInput) (*updateMessageOutput, error) {
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	if s.deps.ModifyMessageLabels == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "label update unavailable")
	}
	add, verr := agent.NormalizeAndValidateLabelList(in.Body.AddLabels, "add")
	if verr != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_request", verr.Error())
	}
	remove, verr := agent.NormalizeAndValidateLabelList(in.Body.RemoveLabels, "remove")
	if verr != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_request", verr.Error())
	}
	final, err := s.deps.ModifyMessageLabels(ctx, in.ID, ag.ID, add, remove)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrMessageNotFound):
			return nil, NewError(http.StatusNotFound, "not_found", "message not found")
		case errors.Is(err, identity.ErrLabelLimitExceeded):
			return nil, NewError(http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("label limit exceeded — a message may carry at most %d labels", identity.MaxLabelsPerMessage))
		default:
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to update labels")
		}
	}
	return &updateMessageOutput{Body: UpdateMessageResultView{MessageID: in.ID, Labels: orEmptyStrings(final)}}, nil
}

// deleteMessageInput is the DELETE …/messages/{id} input. The default delete
// is SOFT (trash, reversible) so it needs no confirmation; the trash-only
// permanent purge requires both permanent=true and the uniform confirm=DELETE
// literal. Confirm is conditionally required — the default path doesn't take
// it, so it can't be schema-required like DeleteConfirm — but the handler
// enforces the identical contract: a missing/wrong confirm when
// permanent=true is a 422 invalid_request, same as the declarative guard.
type deleteMessageInput struct {
	MessageIDParam
	Permanent bool   `query:"permanent" doc:"Permanently delete a message that is already in the trash (irreversible). Requires confirm=DELETE and an account-scoped credential."`
	Confirm   string `query:"confirm" doc:"Must be the literal string DELETE when permanent=true; ignored otherwise."`
}

type deleteMessageOutput struct {
	Body DeleteMessageResult
}

// mapTrashErr converts the store's trash sentinel errors to the wire envelope.
func mapTrashErr(err error, resource string) error {
	switch {
	case errors.Is(err, identity.ErrMessageNotFound):
		return NewError(http.StatusNotFound, "not_found", resource+" not found")
	case errors.Is(err, identity.ErrMessageHeld):
		return NewError(http.StatusConflict, "message_held",
			"message is held for review — approve or reject it in the review queue first")
	case errors.Is(err, identity.ErrNotInTrash):
		return NewError(http.StatusConflict, "not_in_trash", resource+" is not in the trash")
	case errors.Is(err, identity.ErrSendInProgress):
		return NewError(http.StatusConflict, "send_in_progress",
			resource+" has an outbound send in progress; retry permanent deletion after it finishes")
	default:
		return NewError(http.StatusInternalServerError, "internal_error", "operation failed")
	}
}

// handleDeleteMessage moves a message to the trash (default), or permanently
// purges an already-trashed one (permanent=true&confirm=DELETE). The default
// (reversible) delete is a per-agent operation like labels: an agent-scoped
// credential may manage its own messages' trash (resolveOwnedAgent pins it
// to its bound agent). The PERMANENT purge is account-only, like every other
// irreversible delete on the surface — a leaked/injected agent credential
// must not be able to destroy inbox evidence beyond recovery.
func (s *Server) handleDeleteMessage(ctx context.Context, in *deleteMessageInput) (*deleteMessageOutput, error) {
	if in.Permanent && in.Confirm != "DELETE" {
		// Same error contract AND precedence as the declarative DeleteConfirm
		// guard: Huma validates the schema-required confirm before the handler
		// runs (so ahead of any scope check or resource resolution), and the
		// identical caller mistake gets the identical 422 invalid_request
		// envelope here — before the account-scope check and agent/message
		// lookup. Safe to answer first: auth middleware 401s still precede the
		// handler, and the 422 discloses only public spec knowledge, never
		// resource existence.
		return nil, NewError(http.StatusUnprocessableEntity, "invalid_request",
			"permanent deletion is irreversible — query parameter confirm must be the literal string DELETE when permanent=true").
			WithDetails(ValidationErrorDetails{Fields: []FieldError{{
				Location: "query.confirm",
				Message:  "must be the literal string DELETE when permanent=true",
			}}})
	}
	if in.Permanent {
		if _, err := s.requireAccountScope(ctx); err != nil {
			return nil, err
		}
	}
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	if in.Permanent {
		if s.deps.PurgeMessage == nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "delete unavailable")
		}
		if err := s.deps.PurgeMessage(ctx, in.MessageID, ag.ID); err != nil {
			return nil, mapTrashErr(err, "message")
		}
		return &deleteMessageOutput{Body: DeleteMessageResult{Deleted: true, ID: in.MessageID}}, nil
	}
	if s.deps.DeleteMessage == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "delete unavailable")
	}
	if err := s.deps.DeleteMessage(ctx, in.MessageID, ag.ID); err != nil {
		return nil, mapTrashErr(err, "message")
	}
	return &deleteMessageOutput{Body: DeleteMessageResult{Deleted: true, ID: in.MessageID}}, nil
}

// handleRestoreMessage brings a trashed message back to the inbox and returns
// the restored message view. Per-agent operation, like delete.
func (s *Server) handleRestoreMessage(ctx context.Context, in *MessageIDParam) (*messageOutput, error) {
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	if s.deps.RestoreMessage == nil || s.deps.GetMessage == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "restore unavailable")
	}
	if err := s.deps.RestoreMessage(ctx, in.MessageID, ag.ID); err != nil {
		return nil, mapTrashErr(err, "message")
	}
	msg, err := s.deps.GetMessage(ctx, in.MessageID, ag.ID)
	if err != nil || msg == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to reload message")
	}
	return &messageOutput{Body: messageViewFromIdentity(msg)}, nil
}

// handleListMessages ports the legacy list handler: same filter semantics
// and defaults, but the standardized cursor envelope. Validation failures
// return the machine-branchable error envelope.
func (s *Server) handleListMessages(ctx context.Context, in *ListMessagesInput) (*listMessagesOutput, error) {
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	if s.deps.ListMessages == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "message list unavailable")
	}

	// Direction (default inbound for SDK back-compat; the trash view defaults
	// to all — a trash shows everything you deleted, either direction).
	direction := in.Direction
	if direction == "" {
		if in.Deleted {
			direction = "all"
		} else {
			direction = "inbound"
		}
	}

	// Status default depends on direction; only meaningful for inbound. The
	// trash view defaults to all — read state is irrelevant to a trash listing.
	status := in.Status
	if status == "" {
		if direction == "inbound" && !in.Deleted {
			status = "unread"
		} else {
			status = "all"
		}
	}
	if direction == "outbound" && status != "all" {
		return nil, NewError(http.StatusBadRequest, "invalid_filter",
			"status filter only applies to inbound messages — pass status=all when direction=outbound")
	}

	// Bounded substring filters.
	if len(in.From) > maxFilterStr {
		return nil, NewError(http.StatusBadRequest, "invalid_filter", "from filter too long (max 200 chars)")
	}
	if len(in.SubjectContains) > maxFilterStr {
		return nil, NewError(http.StatusBadRequest, "invalid_filter", "subject_contains filter too long (max 200 chars)")
	}
	// Length (200, counted in runes so any stored conversation_id the request
	// schemas admit stays filterable) + CR/LF via the shared check.
	if err := validateConversationID(in.ConversationID); err != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_filter", err.Error())
	}

	// Labels filter: validate + dedup (read access allows the e2a: system
	// namespace, matching legacy allowSystemPrefix=true).
	labelsFilter, err := normalizeLabelFilter(in.Labels)
	if err != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_filter", err.Error())
	}

	// Time range.
	since, err := parseRFC3339Filter(in.Since, "since")
	if err != nil {
		return nil, err
	}
	until, err := parseRFC3339Filter(in.Until, "until")
	if err != nil {
		return nil, err
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return nil, NewError(http.StatusBadRequest, "invalid_filter", "since must be earlier than until")
	}

	// Effective sort (default newest-first).
	sort := in.Sort
	if sort == "" {
		sort = "desc"
	}

	// Decode + validate the cursor against the current filter identity.
	var afterTime time.Time
	var afterID string
	if in.Cursor != "" {
		var cur messagesCursor
		if err := DecodeCursor([]string{s.deps.CursorSecret}, in.Cursor, &cur); err != nil {
			return nil, NewError(http.StatusBadRequest, "invalid_cursor", "invalid pagination cursor")
		}
		if cur.AgentID != ag.ID || cur.Status != status || cur.Direction != direction || cur.Sort != sort ||
			cur.From != in.From || cur.SubjectContains != in.SubjectContains ||
			cur.ConversationID != in.ConversationID ||
			cur.Since != rfc3339OrEmpty(since) || cur.Until != rfc3339OrEmpty(until) ||
			cur.Deleted != in.Deleted ||
			!stringSlicesEqual(cur.Labels, labelsFilter) {
			return nil, NewError(http.StatusBadRequest, "invalid_cursor",
				"cursor was created with different filters — start a new query without a cursor")
		}
		afterTime = cur.CreatedAt
		afterID = cur.ID
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}

	// Fetch limit+1 to detect a further page.
	msgs, err := s.deps.ListMessages(ctx, identity.MessageListFilter{
		AgentID:         ag.ID,
		Status:          status,
		Direction:       direction,
		Descending:      sort == "desc",
		Limit:           limit + 1,
		AfterTime:       afterTime,
		AfterID:         afterID,
		From:            in.From,
		SubjectContains: in.SubjectContains,
		ConversationID:  in.ConversationID,
		Since:           since,
		Until:           until,
		Labels:          labelsFilter,
		Deleted:         in.Deleted,
	})
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to fetch messages")
	}

	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[:limit]
	}
	items := make([]MessageSummaryView, len(msgs))
	for i, m := range msgs {
		items[i] = messageSummaryFromIdentity(m)
	}

	var nextCursor string
	if hasMore {
		last := msgs[len(msgs)-1]
		nextCursor, err = EncodeCursor(s.deps.CursorSecret, messagesCursor{
			CreatedAt: last.CreatedAt, ID: last.ID,
			Status: status, Direction: direction, AgentID: ag.ID, Sort: sort,
			From: in.From, SubjectContains: in.SubjectContains, ConversationID: in.ConversationID,
			Since: rfc3339OrEmpty(since), Until: rfc3339OrEmpty(until), Labels: labelsFilter,
			Deleted: in.Deleted,
		})
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
		}
	}

	return &listMessagesOutput{Body: NewPage(items, nextCursor)}, nil
}

// --- replicated, contract-stable validation helpers (see MessageSummaryView
// doc for why these live here rather than importing the legacy package) ---

// maxFilterStr bounds free-form query filters (mirrors the legacy cap).
const maxFilterStr = 200

// maxLabelLength / maxLabelsPerOp / labelSystemPrefix mirror the legacy
// label invariants verbatim so /v1 filter validation can't drift from the
// write-side charset rule that guards the GIN index.
const (
	maxLabelLength    = 64
	maxLabelsPerOp    = 50
	labelSystemPrefix = "e2a:"
)

func validateConversationID(id string) error {
	// Runes, not bytes: matches the OpenAPI maxLength semantics of the
	// request schemas (JSON Schema counts Unicode code points), so a
	// conversation_id the schema admits is never rejected here — and, at the
	// same 200 cap as the webhook/list filter values, every accepted id
	// remains filterable. The schema tags reject this at the edge for /v1
	// bodies; this is the shared runtime backstop.
	if n := utf8.RuneCountInString(id); n > maxConversationIDLen {
		return fmt.Errorf("conversation_id too long — %d characters, max %d", n, maxConversationIDLen)
	}
	if strings.ContainsAny(id, "\r\n") {
		return errors.New("conversation_id must not contain CR or LF")
	}
	return nil
}

// normalizeLabel canonicalizes a single label (lowercase, charset
// [a-z0-9:_-], 1..maxLabelLength). allowSystem mirrors the read-side
// allowSystemPrefix=true: filtering by an e2a: system label is permitted.
func normalizeLabel(raw string, allowSystem bool) (string, error) {
	l := strings.ToLower(strings.TrimSpace(raw))
	if l == "" {
		return "", errors.New("label must not be empty")
	}
	if len(l) > maxLabelLength {
		return "", fmt.Errorf("label too long (max %d chars)", maxLabelLength)
	}
	for _, r := range l {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == ':':
		default:
			return "", fmt.Errorf("label %q has invalid character; allowed: a-z 0-9 : - _", l)
		}
	}
	if !allowSystem && strings.HasPrefix(l, labelSystemPrefix) {
		return "", fmt.Errorf("labels starting with %q are reserved for system use", labelSystemPrefix)
	}
	return l, nil
}

// normalizeLabelFilter validates + dedups a labels= filter list.
func normalizeLabelFilter(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxLabelsPerOp {
		return nil, fmt.Errorf("labels filter exceeds cap of %d", maxLabelsPerOp)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		l, err := normalizeLabel(r, true)
		if err != nil {
			return nil, fmt.Errorf("labels filter: %w", err)
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out, nil
}

// parseRFC3339Filter parses an optional RFC3339 timestamp query param into
// a time, returning a 400 envelope on a malformed value.
func parseRFC3339Filter(raw, name string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, NewError(http.StatusBadRequest, "invalid_filter",
			name+" must be RFC3339 (e.g. 2026-05-25T00:00:00Z)")
	}
	return t, nil
}

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// utcPtr normalizes a nullable timestamp to UTC while preserving nil (absent)
// semantics — so a nil *time.Time stays omitted rather than becoming a zero
// time. Emitted as a full-precision RFC3339Nano date-time when present.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// orEmptyStrings normalizes a nil slice to a non-nil empty slice so the
// field renders as [] rather than null — matching the legacy orEmptySlice
// behavior for `to` and `labels`.
func orEmptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// orEmpty coalesces a nil slice of any element type to an empty slice so the
// JSON renders as [] rather than null (A-3). Pair with `nullable:"false"` on
// the field so the spec and the runtime agree.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
