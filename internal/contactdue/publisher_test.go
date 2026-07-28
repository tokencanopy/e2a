package contactdue

import (
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// TestEventForDueCarriesEverythingNeededToCompose pins the wake-up payload as
// a contract. The event reaches an agent that is not running, so it must carry
// enough context to decide without granting an account-wide contact read.
func TestEventForDueCarriesEverythingNeededToCompose(t *testing.T) {
	due := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	lastOut := due.Add(-5 * 24 * time.Hour)
	e := eventForDue(identity.DueEngagement{
		UserID: "u_1", AgentEmail: "raise@fund.com", Address: "partner@fund.vc",
		Stage: "touch1", NextActionAt: due, LastOutboundAt: &lastOut,
		OutboundCount: 1, LastConversationID: "conv-1", DisplayName: "A. Partner",
		ContactMetadata: map[string]any{"fund": "Example Capital"}, Replied: false,
	})
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
	for _, key := range []string{"agent_email", "address", "stage", "next_action_at", "replied", "contact"} {
		if _, present := data[key]; !present {
			t.Errorf("payload missing %q", key)
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
	for _, key := range []string{"body", "text", "subject", "suggested_message"} {
		if _, present := data[key]; present {
			t.Errorf("payload carries %q — e2a must not compose", key)
		}
	}
}

func TestEventForDueIsDeterministicPerSchedule(t *testing.T) {
	d := identity.DueEngagement{
		UserID: "u_1", AgentEmail: "raise@fund.com", Address: "partner@fund.vc",
		NextActionAt: time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC),
	}
	first := eventForDue(d)
	second := eventForDue(d)
	if first.ID != second.ID {
		t.Errorf("same schedule produced different event ids (%s vs %s)", first.ID, second.ID)
	}
	d.NextActionAt = d.NextActionAt.Add(7 * 24 * time.Hour)
	if third := eventForDue(d); third.ID == first.ID {
		t.Error("a re-armed schedule reused the previous event id")
	}
}
