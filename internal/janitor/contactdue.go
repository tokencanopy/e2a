package janitor

import (
	"context"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// ContactDuePublisher emits contact.due through the standard webhook outbox, so
// the wake-up inherits durable at-least-once fan-out, retries, HMAC signing,
// and SSRF-guarded delivery rather than getting a bespoke delivery path.
type ContactDuePublisher struct {
	publisher interface {
		Publish(ctx context.Context, e webhookpub.Event)
	}
}

// NewContactDuePublisher builds the publisher over an outbox publisher.
func NewContactDuePublisher(p interface {
	Publish(ctx context.Context, e webhookpub.Event)
}) *ContactDuePublisher {
	return &ContactDuePublisher{publisher: p}
}

// PublishContactDue emits the wake-up for one claimed engagement.
//
// The payload carries everything an agent needs to decide and compose without a
// second round trip — stage, schedule, and the contact's identity and metadata
// — because the whole point is waking an agent that is not running.
//
// It deliberately does NOT carry a suggested action or body. e2a wakes the
// agent; the agent decides and writes. Putting content here would be the first
// step toward e2a owning the sequence.
func (p *ContactDuePublisher) PublishContactDue(ctx context.Context, d identity.DueEngagement) error {
	if p == nil || p.publisher == nil {
		return nil
	}
	contact := map[string]any{
		"address":      d.Address,
		"display_name": d.DisplayName,
		"metadata":     d.ContactMetadata,
	}
	data := map[string]any{
		"agent_email":    d.AgentEmail,
		"address":        d.Address,
		"stage":          d.Stage,
		"next_action_at": d.NextActionAt,
		"replied":        d.Replied,
		"outbound_count": d.OutboundCount,
		"contact":        contact,
	}
	if d.LastOutboundAt != nil {
		data["last_outbound_at"] = *d.LastOutboundAt
	}
	if d.LastConversationID != "" {
		data["last_conversation_id"] = d.LastConversationID
	}
	e := webhookpub.NewEvent(webhookpub.EventContactDue, d.UserID, data)
	e.AgentID = d.AgentEmail
	e.ConversationID = d.LastConversationID
	// Deterministic per (engagement, schedule): the store already guarantees one
	// claim per next_action_at, and this makes an at-least-once redelivery
	// dedupable by subscribers too.
	e.ID = webhookpub.DeterministicEventID(
		d.AgentEmail+"\x00"+d.Address+"\x00"+d.NextActionAt.UTC().Format("20060102150405.000000000"),
		webhookpub.EventContactDue)
	p.publisher.Publish(ctx, e)
	return nil
}
