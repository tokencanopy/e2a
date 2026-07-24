package loopback

import (
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/inboundpolicy"
	"github.com/tokencanopy/e2a/internal/inboundscreen"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// ScreeningEvents builds the screening-outcome events for the inbound leg of a
// loopback self-send, mirroring the relay's SMTP-path payload shapes
// (internal/relay/server.go processInbound) with loopback identities:
// header_from is the agent itself, envelope_from and authentication are nil
// (there was no wire hop). msg is the just-created inbound row.
//
//   - gate flag, delivered (not held) → email.flagged (in addition to
//     email.received, which the caller publishes on the delivered path).
//   - block → email.blocked: the message was accept-then-quarantined
//     (review_rejected); this is the only signal a subscriber gets.
//   - review → email.review_requested: held as pending_review awaiting a
//     human / TTL; carries approval_expires_at.
//
// Deterministic event ids keep a retried local delivery idempotent, matching
// the relay's MTA-retry semantics.
func ScreeningEvents(agent *identity.AgentIdentity, msg *identity.Message, gate inboundpolicy.Decision, res inboundscreen.Result) []webhookpub.Event {
	var events []webhookpub.Event
	agentEmail := agent.EmailAddress()

	if gate.Flagged && !res.Hold {
		fe := webhookpub.NewEvent(webhookpub.EventEmailFlagged, agent.UserID, map[string]interface{}{
			"message_id":      msg.ID,
			"conversation_id": msg.ConversationID,
			"direction":       "inbound",
			"agent_email":     agentEmail,
			"header_from":     &agentEmail,
			"envelope_from":   nil,
			"authentication":  nil,
			"reply_to":        msg.ReplyTo,
			"delivered_to":    agentEmail,
			"subject":         msg.Subject,
			"policy":          agent.InboundPolicy,
			"reason":          gate.Reason,
		})
		fe.AgentID = agent.ID
		fe.ConversationID = msg.ConversationID
		fe.MessageID = msg.ID
		fe.ID = webhookpub.DeterministicEventID(msg.ID, webhookpub.EventEmailFlagged)
		events = append(events, fe)
	}

	if res.Blocked() {
		be := webhookpub.NewEvent(webhookpub.EventEmailBlocked, agent.UserID, map[string]interface{}{
			"message_id":      msg.ID,
			"conversation_id": msg.ConversationID,
			"agent_email":     agentEmail,
			"direction":       "inbound",
			"header_from":     &agentEmail,
			"envelope_from":   nil,
			"authentication":  nil,
			"delivered_to":    agentEmail,
			"subject":         msg.Subject,
			"reason":          res.Reason,
			"reason_source":   res.Denorm.ReviewReason,
		})
		be.AgentID = agent.ID
		be.ConversationID = msg.ConversationID
		be.MessageID = msg.ID
		be.ID = webhookpub.DeterministicEventID(msg.ID, webhookpub.EventEmailBlocked)
		events = append(events, be)
	}

	if res.Review() {
		pe := webhookpub.NewEvent(webhookpub.EventEmailReviewRequested, agent.UserID, map[string]interface{}{
			"message_id":          msg.ID,
			"conversation_id":     msg.ConversationID,
			"agent_email":         agentEmail,
			"direction":           "inbound",
			"header_from":         &agentEmail,
			"envelope_from":       nil,
			"authentication":      nil,
			"delivered_to":        agentEmail,
			"subject":             msg.Subject,
			"reason":              res.Reason,
			"reason_source":       res.Denorm.ReviewReason,
			"approval_expires_at": res.Denorm.ApprovalExpiresAt,
		})
		pe.AgentID = agent.ID
		pe.ConversationID = msg.ConversationID
		pe.MessageID = msg.ID
		pe.ID = webhookpub.DeterministicEventID(msg.ID, webhookpub.EventEmailReviewRequested)
		events = append(events, pe)
	}
	return events
}
