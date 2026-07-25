package contactdue_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/contactdue"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

type capturingOutbox struct{ events []webhookpub.Event }

func (c *capturingOutbox) Publish(_ context.Context, e webhookpub.Event) {
	c.events = append(c.events, e)
}

// TestPublishContactDueCarriesEverythingNeededToCompose pins the wake-up
// payload as a contract. The whole point of the event is reaching an agent
// that is NOT running, so anything missing here forces a second round trip
// before it can decide what to send — and subscribers will depend on these
// field names whether or not we document them.
func TestPublishContactDueCarriesEverythingNeededToCompose(t *testing.T) {
	out := &capturingOutbox{}
	due := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	lastOut := due.Add(-5 * 24 * time.Hour)

	err := contactdue.NewOutboxPublisher(out).PublishContactDue(context.Background(), identity.DueEngagement{
		UserID: "u_1", AgentEmail: "raise@fund.com", Address: "partner@fund.vc",
		Stage: "touch1", NextActionAt: due, LastOutboundAt: &lastOut,
		OutboundCount: 1, LastConversationID: "conv-1", DisplayName: "A. Partner",
		ContactMetadata: map[string]any{"fund": "Example Capital"}, Replied: false,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(out.events) != 1 {
		t.Fatalf("published %d events, want 1", len(out.events))
	}
	e := out.events[0]
	if e.Type != webhookpub.EventContactDue {
		t.Errorf("type = %q, want %q", e.Type, webhookpub.EventContactDue)
	}
	if e.AgentID != "raise@fund.com" {
		t.Errorf("agent_id = %q — subscribers filter on this", e.AgentID)
	}
	data, ok := e.Data.(map[string]any)
	if !ok {
		t.Fatalf("data is %T, want map", e.Data)
	}
	for _, k := range []string{"agent_email", "address", "stage", "next_action_at", "replied", "contact"} {
		if _, present := data[k]; !present {
			t.Errorf("payload missing %q — the agent cannot decide without it", k)
		}
	}
	contact, _ := data["contact"].(map[string]any)
	if contact["display_name"] != "A. Partner" {
		t.Errorf("embedded contact name = %v", contact["display_name"])
	}
	meta, _ := contact["metadata"].(map[string]any)
	if meta["fund"] != "Example Capital" {
		t.Errorf("embedded contact metadata lost: %v", contact["metadata"])
	}
	// Deliberately absent: e2a wakes the agent, the agent writes the message.
	// A suggested body here would be the first step toward e2a owning the
	// sequence, which the design refuses.
	for _, k := range []string{"body", "text", "subject", "suggested_message"} {
		if _, present := data[k]; present {
			t.Errorf("payload carries %q — e2a must not compose on the agent's behalf", k)
		}
	}
}

// TestPublishContactDueIsDeterministicPerSchedule pins that redelivery of the
// same wake-up is dedupable by subscribers: at-least-once delivery means the
// same event can arrive twice, and a duplicate here invites a duplicate email.
func TestPublishContactDueIsDeterministicPerSchedule(t *testing.T) {
	out := &capturingOutbox{}
	p := contactdue.NewOutboxPublisher(out)
	d := identity.DueEngagement{
		UserID: "u_1", AgentEmail: "raise@fund.com", Address: "partner@fund.vc",
		NextActionAt: time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC),
	}
	_ = p.PublishContactDue(context.Background(), d)
	_ = p.PublishContactDue(context.Background(), d)
	if out.events[0].ID != out.events[1].ID {
		t.Errorf("same schedule produced different event ids (%s vs %s) — a redelivery "+
			"would not be dedupable", out.events[0].ID, out.events[1].ID)
	}

	// A different schedule for the same contact must be a distinct event.
	d.NextActionAt = d.NextActionAt.Add(7 * 24 * time.Hour)
	_ = p.PublishContactDue(context.Background(), d)
	if out.events[2].ID == out.events[0].ID {
		t.Error("a re-armed schedule reused the previous event id — the second wake-up " +
			"would be discarded as a duplicate")
	}
}
