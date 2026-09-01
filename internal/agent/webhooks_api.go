package agent

import (
	"time"

	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// --- Webhooks-as-a-resource HTTP layer (slice 2) ---
//
// The handlers here serve POST/GET/LIST/PATCH/DELETE on /v1/webhooks
// plus /rotate-secret, /test, /deliveries subresources. The storage
// layer in internal/identity/webhooks.go does the per-row work; this
// layer applies the public-facing validation rules from the design.

// NOTE: webhook create/update validation (URL/SSRF, event types, filter caps,
// agent-ownership) now lives in the typed /v1 layer (internal/httpapi/
// webhooks.go) — the legacy copy that lived here was removed in the v1 cutover
// along with its routes. The event builders below remain (they feed the live
// publisher, not the removed HTTP handlers).

// --- event builders (slice 3) ---
//
// Each helper translates an in-process trigger into a webhookpub.Event.
// They live here (next to the webhook resource) so the publisher's
// envelope shape is co-located with the handler shape, and the
// build sites in api.go / hitl_api.go stay one-line.

// buildSentEvent constructs an email.sent event for /send, /reply,
// /forward. outMsg may be nil if CreateOutboundMessage failed — in
// that case we still publish (the SES send already happened) but with
// an empty message_id; receivers see the event without a row to fetch.
//
// Data is the canonical typed eventpayload.EmailSentData — the SAME struct the
// async send worker's buildEmailSentEventFromRow emits, so subscribers see an
// identical payload on both paths (golden-fixture-locked).
func (a *API) buildSentEvent(
	agent *identity.AgentIdentity,
	outMsg *identity.Message,
	result *outbound.SendResult,
	req outbound.SendRequest,
	msgType string,
) webhookpub.Event {
	messageID := ""
	if outMsg != nil {
		messageID = outMsg.ID
	}
	data := eventpayload.EmailSentData{
		MessageID:         messageID,
		AgentEmail:        agent.EmailAddress(),
		Direction:         "outbound",
		ConversationID:    req.ConversationID,
		ProviderMessageID: result.MessageID,
		Method:            result.Method,
		From:              agent.EmailAddress(),
		To:                orEmpty(result.To),
		CC:                result.CC,
		BCC:               result.BCC,
		Subject:           req.Subject,
		MessageType:       msgType,
	}
	return webhookpub.Event{
		ID:             generateEventIDForAgent(),
		Type:           webhookpub.EventEmailSent,
		CreatedAt:      time.Now().UTC(),
		UserID:         agent.UserID,
		AgentID:        agent.ID,
		ConversationID: req.ConversationID,
		MessageID:      messageID,
		Data:           data,
	}
}

// buildPendingApprovalEvent fires when a HITL-enabled agent holds an
// outbound message for human review. msg is the pending row; req is
// the composed SendRequest that produced it.
func (a *API) buildPendingApprovalEvent(
	agent *identity.AgentIdentity,
	msg *identity.Message,
	req outbound.SendRequest,
	msgType string,
) webhookpub.Event {
	data := map[string]interface{}{
		"message_id":          msg.ID,
		"direction":           "outbound",
		"agent_email":         agent.EmailAddress(),
		"from":                agent.EmailAddress(),
		"to":                  req.To,
		"cc":                  req.CC,
		"bcc":                 req.BCC,
		"subject":             req.Subject,
		"message_type":        msgType,
		"conversation_id":     req.ConversationID,
		"approval_expires_at": msg.ApprovalExpiresAt,
	}
	if msg.ScheduledAt != nil {
		data["scheduled_at"] = msg.ScheduledAt
	}
	attachReviewLifecycle(data, msg.LifecycleTransitions)
	return webhookpub.Event{
		ID:             generateEventIDForAgent(),
		Type:           webhookpub.EventEmailReviewRequested,
		CreatedAt:      time.Now().UTC(),
		UserID:         agent.UserID,
		AgentID:        agent.ID,
		ConversationID: req.ConversationID,
		MessageID:      msg.ID,
		Data:           data,
	}
}

// buildApprovedEvent fires after ApproveAndSend hands the message to
// SES. sent carries the post-approve message row (now status=sent).
func (a *API) buildApprovedEvent(
	agent *identity.AgentIdentity,
	sent *identity.Message,
	reviewerUserID string,
) webhookpub.Event {
	data := map[string]interface{}{
		"message_id":   sent.ID,
		"direction":    "outbound",
		"agent_email":  agent.EmailAddress(),
		"method":       sent.Method,
		"from":         agent.EmailAddress(),
		"to":           sent.ToRecipients,
		"subject":      sent.Subject,
		"message_type": sent.Type,
		"edited":       sent.Edited,
	}
	if sent.ProviderMessageID != "" && sent.Method != "loopback" {
		data["provider_message_id"] = sent.ProviderMessageID
	}
	if sent.ScheduledAt != nil {
		data["scheduled_at"] = sent.ScheduledAt
	}
	attachReviewLifecycle(data, sent.LifecycleTransitions)
	return webhookpub.Event{
		ID:        generateEventIDForAgent(),
		Type:      webhookpub.EventEmailReviewApproved,
		CreatedAt: time.Now().UTC(),
		UserID:    agent.UserID,
		AgentID:   agent.ID,
		MessageID: sent.ID,
		Data:      data,
	}
}

// buildRejectedEvent fires when a reviewer rejects a pending outbound.
// userID is the reviewer (a separate identity from the agent's owner
// when multi-reviewer ACLs land in a future slice — today they're
// always the same).
func (a *API) buildRejectedEvent(
	userID string,
	rejected *identity.Message,
	reason string,
) webhookpub.Event {
	data := map[string]interface{}{
		"message_id":   rejected.ID,
		"direction":    "outbound",
		"agent_email":  rejected.AgentID,
		"message_type": rejected.Type,
		"reason":       reason,
	}
	attachReviewLifecycle(data, rejected.LifecycleTransitions)
	return webhookpub.Event{
		ID:        generateEventIDForAgent(),
		Type:      webhookpub.EventEmailReviewRejected,
		CreatedAt: time.Now().UTC(),
		UserID:    userID,
		AgentID:   rejected.AgentID,
		MessageID: rejected.ID,
		Data:      data,
	}
}

// buildInboundReleasedEvent fires when a reviewer releases a held inbound
// message to the agent (status pending_review → review_approved, now inbox-
// visible). This is the ONLY push signal an approved inbound message gets:
// email.received was suppressed while it was held and is not re-fired on release
// (push re-delivery is a tracked follow-up), so without this event an approved
// inbound message is invisible to subscribers. See design 2026-06-22 §4 (Q2).
//
// ownerUserID is the ROUTING key (whose webhooks fire) — always the agent's
// owner, mirroring buildApprovedEvent. reviewerUserID is the human who acted; it
// is NOT exposed in the payload (an internal DB user id) but is kept as a distinct
// parameter from ownerUserID so a future multi-reviewer ACL can't misroute the
// event to a non-owner reviewer.
func (a *API) buildInboundReleasedEvent(msg *identity.ReviewMessageMeta, ownerUserID, reviewerUserID string, transitions ...messagelifecycle.MessageLifecycleTransition) webhookpub.Event {
	data := map[string]interface{}{
		"message_id":   msg.ID,
		"direction":    "inbound",
		"agent_email":  msg.Recipient,
		"from":         msg.Sender,
		"subject":      msg.Subject,
		"message_type": msg.Type,
	}
	attachReviewLifecycle(data, transitions)
	return webhookpub.Event{
		ID:        generateEventIDForAgent(),
		Type:      webhookpub.EventEmailReviewApproved,
		CreatedAt: time.Now().UTC(),
		UserID:    ownerUserID,
		AgentID:   msg.AgentID,
		MessageID: msg.ID,
		Data:      data,
	}
}

// buildInboundRejectedEvent fires when a reviewer drops a held inbound message
// (status pending_review → review_rejected; it stays hidden from the agent and
// its raw payload is retained for forensics — design §4.4). Routing key is the
// agent owner (see buildInboundReleasedEvent).
func (a *API) buildInboundRejectedEvent(msg *identity.ReviewMessageMeta, ownerUserID, reviewerUserID, reason string, transitions ...messagelifecycle.MessageLifecycleTransition) webhookpub.Event {
	data := map[string]interface{}{
		"message_id":   msg.ID,
		"direction":    "inbound",
		"agent_email":  msg.Recipient,
		"message_type": msg.Type,
		"reason":       reason,
	}
	attachReviewLifecycle(data, transitions)
	return webhookpub.Event{
		ID:        generateEventIDForAgent(),
		Type:      webhookpub.EventEmailReviewRejected,
		CreatedAt: time.Now().UTC(),
		UserID:    ownerUserID,
		AgentID:   msg.AgentID,
		MessageID: msg.ID,
		Data:      data,
	}
}

// generateEventIDForAgent wraps webhookpub's id helper so this file
// doesn't need to import internal/webhookpub just for the constructor.
func generateEventIDForAgent() string {
	return webhookpub.NewEvent("", "", nil).ID
}

// --- helpers ---

// orEmpty keeps a REQUIRED wire array present-but-empty rather than null.
func orEmpty(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func attachReviewLifecycle(data map[string]interface{}, transitions []messagelifecycle.MessageLifecycleTransition) {
	if len(transitions) > 0 {
		data["lifecycle_transitions"] = transitions
	}
}
