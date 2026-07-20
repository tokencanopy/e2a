// Package eventpayload defines the canonical typed `data` payloads for the
// STABLE webhook/WS event types (the pre-GA contract freeze, PR-2 of the
// event/webhook contract audit).
//
// One struct per stable event, used by EVERY builder that emits that event —
// the relay (email.received), the queue-first outbound path (email.sent /
// email.failed), the SES delivery-feedback consumer (email.delivered /
// email.bounced / email.complained, domain.suppression_added), and the
// sender-identity worker (domain.sending_verified / domain.sending_failed).
// The WebSocket channel reuses EmailReceivedData verbatim, so webhook and WS
// payloads for the same event are identical by construction.
//
// The structs are also registered as OpenAPI component schemas (see
// internal/httpapi's event-payload schema registration), and the committed
// golden fixtures under testdata/ are the cross-channel drift lock: the
// server-side builder tests and the TS/Python SDK payload tests all assert
// against the same fixture bytes.
//
// A few BETA payloads are also typed here and listed in BetaEvents
// (AgentSuppressionAddedData, ContactDueData): typing and fixturing a payload
// documents and locks it without freezing it — `x-stability-level: beta` keeps
// it outside the GA compatibility guarantee. The screening and review-hold
// events (email.flagged, email.blocked, email.review_requested,
// email.review_approved, email.review_rejected) intentionally stay
// map[string]any at their trigger sites — those payloads are still open and
// must NOT be typed here until their shape settles.
//
// This package stays lightweight (mail parsing plus the lifecycle wire model)
// so delivery producers can import it without dragging in webhookpub's storage
// dependencies.
package eventpayload

import (
	"time"

	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/mailparse"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

// AttachmentMetaView is the CANONICAL wire shape for one attachment's
// metadata — never the bytes. It is shared by the REST message views
// (httpapi aliases it), the stable event payloads (EmailReceivedData), and
// the account export: `index` is the stable 0-based attachment index
// (document order) used to fetch the bytes via
// GET /v1/agents/{email}/messages/{id}/attachments/{index}.
type AttachmentMetaView struct {
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	// SizeBytes is the DECODED attachment payload size — the byte count of the
	// file after the MIME Content-Transfer-Encoding (base64 / quoted-printable)
	// is undone: the size of the file a download yields. NOT the encoded
	// on-the-wire size inside the raw MIME, and NOT the message-level
	// size_bytes (which is the raw MIME length of the whole message).
	SizeBytes int `json:"size_bytes" doc:"DECODED attachment payload size in bytes (Content-Transfer-Encoding undone) — the size of the file a download yields, not its encoded size inside the raw MIME."`
	Index     int `json:"index" doc:"Stable 0-based attachment index (document order) — the fetch key for the attachment-bytes endpoint."`
	// ContentID is the attachment's Content-ID with angle brackets stripped
	// (e.g. "ii_abc@mail.gmail.com"), present only for parts that carry one —
	// typically inline images embedded by a mail client. It is the key an HTML
	// body's `cid:` reference resolves against: a renderer maps
	// `<img src="cid:ii_abc@mail.gmail.com">` to the attachment whose
	// content_id matches, then fetches the bytes via the attachment endpoint.
	// Omitted (empty) for ordinary, non-inline attachments.
	ContentID string `json:"content_id,omitempty" doc:"Content-ID (angle brackets stripped) for an inline part referenced by a body 'cid:' URL, e.g. an embedded image; the key a cid: reference resolves against. Omitted for non-inline attachments."`
}

// EmailReceivedData is the `data` payload of an email.received event. The
// event is a metadata-only NOTIFICATION, never a content carrier: fetch the
// full message (body + attachment bytes) from
// GET /v1/agents/{delivered_to}/messages/{message_id}.
type EmailReceivedData struct {
	MessageID  string `json:"message_id"`
	AgentEmail string `json:"agent_email" doc:"The receiving agent's email — its id and address (an agent's id IS its email)."`
	Direction  string `json:"direction" doc:"Always \"inbound\" on this event."`
	// ConversationID is empty for a thread-starting message.
	ConversationID string                    `json:"conversation_id,omitempty"`
	HeaderFrom     *string                   `json:"header_from" nullable:"true" doc:"Parsed RFC 5322 From address; never replaced by Reply-To."`
	EnvelopeFrom   *string                   `json:"envelope_from" nullable:"true" doc:"SMTP MAIL FROM address for inbound SMTP delivery; null for a null reverse path or providerless delivery."`
	VerifiedDomain *string                   `json:"verified_domain" nullable:"true" doc:"DMARC-authenticated RFC 5322 From domain when authentication passed; null when authentication failed, was unavailable, or was not evaluated. A null caused by dmarc.status=none (sender publishes no DMARC record) is common and NOT itself suspicious — distinct from dmarc.status=fail, an actual mismatch. Only DMARC ties a passing SPF or DKIM identity back to this header domain; a bare SPF or DKIM pass without DMARC does not."`
	To             []string                  `json:"to" nullable:"false"`
	CC             []string                  `json:"cc" nullable:"false"`
	ReplyTo        []string                  `json:"reply_to" nullable:"false"`
	Authentication *emailauth.Authentication `json:"authentication" doc:"Inbound SMTP authentication evidence. Only dmarc.status=pass authenticates the RFC 5322 From domain; even a pass does not authenticate the mailbox local part, a person, or message content. Null means there was no authenticating inbound SMTP peer, as with outbound or providerless loopback delivery."`
	// DeliveredTo is the agent address this copy was delivered to — a SCALAR
	// by construction: the relay emits one event per per-agent delivery, so
	// unlike the peer To/CC lists (the message's parsed headers) this is
	// always exactly one address. It is the fetch key for the message.
	DeliveredTo string    `json:"delivered_to" doc:"The one agent address this per-agent copy was delivered to (scalar by construction — one event per delivery). Fetch key for the message."`
	Subject     string    `json:"subject"`
	ReceivedAt  time.Time `json:"received_at" format:"date-time"`
	// Attachments is per-attachment METADATA (never bytes) parsed from the
	// raw message. Omitted when the message has none.
	Attachments []AttachmentMetaView `json:"attachments,omitempty" nullable:"false"`
	// LifecycleTransitions are the exact canonical rows committed with this
	// event. Omitted on historical events created before the lifecycle ledger.
	LifecycleTransitions []messagelifecycle.MessageLifecycleTransition `json:"lifecycle_transitions,omitempty" nullable:"false"`
}

// EmailSentData is the `data` payload of an email.sent event — an outbound
// send reached its terminal sent state, either by provider acceptance or by
// atomic local loopback delivery.
type EmailSentData struct {
	MessageID      string `json:"message_id"`
	AgentEmail     string `json:"agent_email"`
	Direction      string `json:"direction" doc:"Always \"outbound\" on this event."`
	ConversationID string `json:"conversation_id,omitempty"`
	// ProviderMessageID is the provider-assigned (SES) message id — distinct
	// from the e2a message_id, and the correlation key for the async
	// delivered/bounced/complained feedback events. Omitted for providerless
	// local loopback delivery.
	ProviderMessageID string   `json:"provider_message_id,omitempty"`
	Method            string   `json:"method" doc:"Transport used for the send. Open set; tolerate unknown values. Known values: smtp, loopback."`
	From              string   `json:"from"`
	To                []string `json:"to" nullable:"false"`
	CC                []string `json:"cc,omitempty" nullable:"false"`
	BCC               []string `json:"bcc,omitempty" nullable:"false"`
	Subject           string   `json:"subject"`
	MessageType       string   `json:"message_type" doc:"Send kind. Open set; tolerate unknown values. Known values: send, reply, forward."`
	// BatchID correlates this send to the batch it was submitted under
	// (POST /v1/agents/{email}/batches). Omitted for single sends. Lets a
	// subscriber group per-recipient delivery outcomes back to the batch
	// call (docs/design/batch-send.md §7.3).
	BatchID              string                                        `json:"batch_id,omitempty"`
	LifecycleTransitions []messagelifecycle.MessageLifecycleTransition `json:"lifecycle_transitions,omitempty" nullable:"false"`
}

// EmailFailedData is the `data` payload of an email.failed event — an
// outbound send terminally failed (retries exhausted / permanent reject).
// Same fields as EmailSentData minus provider_message_id (the provider never
// accepted it), plus the failure reason.
type EmailFailedData struct {
	MessageID      string   `json:"message_id"`
	AgentEmail     string   `json:"agent_email"`
	Direction      string   `json:"direction" doc:"Always \"outbound\" on this event."`
	ConversationID string   `json:"conversation_id,omitempty"`
	Method         string   `json:"method" doc:"Transport used for the send. Open set; tolerate unknown values. Known values: smtp."`
	From           string   `json:"from"`
	To             []string `json:"to" nullable:"false"`
	CC             []string `json:"cc,omitempty" nullable:"false"`
	BCC            []string `json:"bcc,omitempty" nullable:"false"`
	Subject        string   `json:"subject"`
	MessageType    string   `json:"message_type" doc:"Send kind. Open set; tolerate unknown values. Known values: send, reply, forward."`
	// BatchID correlates this failure to the batch it was submitted under
	// (POST /v1/agents/{email}/batches). Omitted for single sends
	// (docs/design/batch-send.md §7.3).
	BatchID string `json:"batch_id,omitempty"`
	// Reason is the human-readable terminal failure diagnostic (e.g. the SMTP
	// response of a permanent reject).
	Reason string `json:"reason"`
	// ReasonCode is an optional machine-readable failure code. Omitted when
	// the send path has no classification beyond Reason.
	ReasonCode string `json:"reason_code,omitempty"`
	// Retryable reports whether re-submitting the same send could succeed.
	// Populated only where the send path genuinely knows it; omitted
	// otherwise (absent ≠ false).
	Retryable *bool `json:"retryable,omitempty"`
	// LifecycleTransitions are the exact canonical rows committed with this
	// event. Omitted on historical events created before the lifecycle ledger.
	LifecycleTransitions []messagelifecycle.MessageLifecycleTransition `json:"lifecycle_transitions,omitempty" nullable:"false"`
}

// EmailDeliveredData is the `data` payload of an email.delivered event — the
// recipient's server accepted an outbound message, per recipient (one event
// per (message, recipient)). The event type IS the outcome; there is no
// redundant `status` field.
type EmailDeliveredData struct {
	MessageID  string `json:"message_id"`
	AgentEmail string `json:"agent_email"`
	Direction  string `json:"direction" doc:"Always \"outbound\" on this event."`
	// DeliveredTo is the ONE recipient address this per-recipient outcome is
	// about (scalar by construction).
	DeliveredTo string `json:"delivered_to" doc:"The one recipient address this per-recipient outcome is about."`
	Subject     string `json:"subject,omitempty"`
	// SMTPDetail is the provider diagnostic string (e.g. the remote SMTP
	// response), when the feedback notification carried one.
	SMTPDetail           string                                        `json:"smtp_detail,omitempty"`
	LifecycleTransitions []messagelifecycle.MessageLifecycleTransition `json:"lifecycle_transitions,omitempty" nullable:"false"`
}

// EmailBouncedData is the `data` payload of an email.bounced event — an
// outbound message bounced for a recipient. EmailDeliveredData's fields plus
// the SES bounce classification.
type EmailBouncedData struct {
	MessageID   string `json:"message_id"`
	AgentEmail  string `json:"agent_email"`
	Direction   string `json:"direction" doc:"Always \"outbound\" on this event."`
	DeliveredTo string `json:"delivered_to" doc:"The one recipient address this per-recipient outcome is about."`
	Subject     string `json:"subject,omitempty"`
	SMTPDetail  string `json:"smtp_detail,omitempty"`
	// BounceType is the normalized SES bounce classification. Only a
	// permanent (hard) bounce auto-suppresses the address.
	//
	// Deliberately a CLOSED enum, unlike the evolving response vocabularies
	// (which are open sets): this is a normalized, exhaustive classification —
	// normalizeBounceType in internal/delivery/ses.go maps every provider
	// value into exactly these three, with `undetermined` as the guaranteed
	// catch-all — so the vocabulary cannot grow without a deliberate contract
	// change.
	BounceType string `json:"bounce_type" enum:"permanent,transient,undetermined"`
	// BounceSubType is the raw SES bounceSubType (e.g. General, NoEmail,
	// MailboxFull), when present.
	BounceSubType        string                                        `json:"bounce_sub_type,omitempty"`
	LifecycleTransitions []messagelifecycle.MessageLifecycleTransition `json:"lifecycle_transitions,omitempty" nullable:"false"`
}

// EmailComplainedData is the `data` payload of an email.complained event — a
// recipient marked an outbound message as spam (feedback-loop complaint).
// Same shape as EmailDeliveredData; SMTPDetail carries the complaint
// feedback type when present.
type EmailComplainedData struct {
	MessageID            string                                        `json:"message_id"`
	AgentEmail           string                                        `json:"agent_email"`
	Direction            string                                        `json:"direction" doc:"Always \"outbound\" on this event."`
	DeliveredTo          string                                        `json:"delivered_to" doc:"The one recipient address this per-recipient outcome is about."`
	Subject              string                                        `json:"subject,omitempty"`
	SMTPDetail           string                                        `json:"smtp_detail,omitempty"`
	LifecycleTransitions []messagelifecycle.MessageLifecycleTransition `json:"lifecycle_transitions,omitempty" nullable:"false"`
}

// DomainSendingVerifiedData is the `data` payload of a domain.sending_verified
// event — the domain's async SES sending identity reached the verified
// terminal state.
type DomainSendingVerifiedData struct {
	Domain        string `json:"domain"`
	SendingStatus string `json:"sending_status" doc:"Terminal sending-identity status. Open set; tolerate unknown values. Known values: verified."`
}

// DomainSendingFailedData is the `data` payload of a domain.sending_failed
// event — the domain's async SES sending identity reached a failed terminal
// state.
type DomainSendingFailedData struct {
	Domain        string `json:"domain"`
	SendingStatus string `json:"sending_status" doc:"Terminal sending-identity status. Open set; tolerate unknown values. Known values: failed."`
	Reason        string `json:"reason,omitempty"`
}

// DomainSuppressionAddedData is the `data` payload of a
// domain.suppression_added event — an address was auto-suppressed after a
// hard bounce or complaint. Account-scoped despite the `domain.` prefix.
type DomainSuppressionAddedData struct {
	Address string `json:"address"`
	// Source is an OPEN set (evolving response-side vocabulary, like the REST
	// Suppression.source it mirrors — and the DB CHECK in
	// migrations/031_delivery_feedback.sql already admits source='manual').
	Source string `json:"source" doc:"How the suppression was created. Open set: new values may be added over time, so treat these as strings and tolerate unknown values. Known values: bounce, complaint."`
	Reason string `json:"reason,omitempty"`
	// MessageID is the outbound message whose feedback triggered the
	// suppression, when still known.
	MessageID            string                                        `json:"message_id,omitempty"`
	LifecycleTransitions []messagelifecycle.MessageLifecycleTransition `json:"lifecycle_transitions,omitempty" nullable:"false"`
}

// AgentSuppressionAddedData is the exact agent-recipient consent scope added
// by either the public unsubscribe flow or account-admin management API.
type AgentSuppressionAddedData struct {
	AgentEmail string `json:"agent_email"`
	Address    string `json:"address"`
	Source     string `json:"source" doc:"How the suppression was created. Known values: unsubscribe, manual."`
}

// ContactDueContact is the contact identity embedded in a contact.due payload.
// It is carried INLINE on purpose: the event wakes an agent-scoped consumer,
// and requiring a follow-up read would force an account-wide contact-read
// credential onto something that only needs one person's details.
type ContactDueContact struct {
	Address string `json:"address" doc:"The contact's canonical address — identical to the payload's top-level address."`
	// DisplayName is always present, empty when the contact has no name.
	DisplayName string `json:"display_name" doc:"The contact's display name; empty string when unset."`
	// Metadata is always present — an empty object when the contact carries
	// none. Flat and caller-owned: e2a never interprets it. The bounds are
	// enforced on the contacts write path (see ContactView.metadata).
	Metadata map[string]any `json:"metadata" doc:"The contact's caller-owned metadata, verbatim and uninterpreted. An empty object when the contact carries none."`
}

// ContactDueData is the `data` payload of a contact.due event — an outreach
// engagement's next_action_at has passed, so e2a wakes the agent. It is a
// WAKE-UP, never a suggestion: it deliberately carries no body, subject, or
// proposed content. e2a wakes; the agent decides and writes.
//
// Built by internal/contactdue from identity.DueEngagement. The event fires at
// most once per (engagement, next_action_at) — the claim advances
// notified_next_action_at in the same transaction as the outbox write — and
// its id is deterministic over that pair, so a retried sweep collapses.
//
// BETA (see BetaEvents): published and fixture-locked, but not GA-frozen.
//
// Field ORDER here is the alphabetical order encoding/json used while this
// payload was still built as a map[string]any, so typing it changed no emitted
// byte. internal/contactdue's TestEventForDueIsByteIdenticalToTheLegacyMap is
// the evidence; it is what keeps this ordering meaningful.
type ContactDueData struct {
	Address    string            `json:"address" doc:"The contact's canonical address — who the agent owes an action to."`
	AgentEmail string            `json:"agent_email" doc:"The agent that owns this outreach — its id and address (an agent's id IS its email)."`
	Contact    ContactDueContact `json:"contact" doc:"The contact's identity, inlined so an agent-scoped consumer needs no account-wide contact read."`
	// LastConversationID is omitted when the agent has never corresponded with
	// this contact — i.e. there is no thread to continue.
	LastConversationID string `json:"last_conversation_id,omitempty" doc:"Conversation id of the most recent exchange with this contact, to continue that thread. Omitted when there is none."`
	// LastOutboundAt is omitted when nothing has been sent yet.
	LastOutboundAt *time.Time `json:"last_outbound_at,omitempty" format:"date-time" doc:"When this agent last sent to the contact. Omitted when it never has."`
	NextActionAt   time.Time  `json:"next_action_at" format:"date-time" doc:"The schedule instant that came due — the value set on the engagement, not the time the sweep observed it."`
	OutboundCount  int        `json:"outbound_count" doc:"How many messages this agent has sent to the contact since the engagement was created. Computed at claim time, not a stored counter."`
	Replied        bool       `json:"replied" doc:"True when the contact has replied since this agent's first outbound to them."`
	Stage          string     `json:"stage" doc:"The caller-owned outreach stage, verbatim. e2a never interprets it; empty when unset."`
}

// AttachmentMetadata extracts the per-attachment metadata of a raw RFC 5322
// message for EmailReceivedData.Attachments — the same extraction the REST
// message views use (mailparse.Attachments), so the event and the resource
// agree on indexes and sizes. Returns nil when the message has none.
func AttachmentMetadata(raw []byte) []AttachmentMetaView {
	if len(raw) == 0 {
		return nil
	}
	atts := mailparse.Attachments(raw)
	if len(atts) == 0 {
		return nil
	}
	out := make([]AttachmentMetaView, 0, len(atts))
	for i, a := range atts {
		out = append(out, AttachmentMetaView{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			SizeBytes:   len(a.Data),
			Index:       i,
			ContentID:   a.ContentID,
		})
	}
	return out
}
