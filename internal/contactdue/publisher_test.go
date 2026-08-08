package contactdue

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/eventpayload/goldenassert"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

func fixture(name string) string {
	return filepath.Join("..", "eventpayload", "testdata", name)
}

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
	data, ok := e.Data.(eventpayload.ContactDueData)
	if !ok {
		t.Fatalf("data is %T, want eventpayload.ContactDueData", e.Data)
	}
	if data.AgentEmail == "" || data.Address == "" || data.Stage == "" || data.NextActionAt.IsZero() {
		t.Errorf("payload is missing a required field: %+v", data)
	}
	if data.Contact.DisplayName != "A. Partner" {
		t.Errorf("embedded contact name = %v", data.Contact.DisplayName)
	}
	if data.Contact.Metadata["fund"] != "Example Capital" {
		t.Errorf("embedded contact metadata lost: %v", data.Contact.Metadata)
	}
	// The type system now enforces the "e2a wakes, the agent writes" rule that
	// this test used to assert key-by-key: ContactDueData has no content field.
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"body", "text", "subject", "suggested_message"} {
		if _, present := keys[key]; present {
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

// TestEventForDueMatchesGoldenFixture joins this builder to the cross-channel
// drift lock every other published event already sits behind: the emitted
// `data` must be byte-identical to the committed fixture, in both the fully
// populated and the required-fields-only shape.
func TestEventForDueMatchesGoldenFixture(t *testing.T) {
	goldenassert.Data(t, fixture("contact.due.json"), eventForDue(goldenDueEngagement()).Data)
	goldenassert.Data(t, fixture("contact.due.min.json"), eventForDue(minimalDueEngagement()).Data)
}

// TestEventForDueIsByteIdenticalToTheLegacyMap is the wire-neutrality proof for
// typing this payload. contact.due was the one delivered event whose `data` was
// an ad-hoc map[string]any documented only in prose; legacyEventData below is
// that exact builder, kept verbatim so the change can be shown to have altered
// nothing a consumer can observe — same keys, same values, same presence rules,
// same bytes. Delete it once contact.due is declared stable and the fixtures
// are the only lock that matters.
func TestEventForDueIsByteIdenticalToTheLegacyMap(t *testing.T) {
	for name, due := range map[string]identity.DueEngagement{
		"full":    goldenDueEngagement(),
		"minimal": minimalDueEngagement(),
		"no_last_outbound": {
			UserID: "u_1", AgentEmail: "raise@fund.com", Address: "partner@fund.vc",
			Stage: "touch2", NextActionAt: time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC),
			LastConversationID: "conv-1", ContactMetadata: map[string]any{},
		},
		"nil_metadata": {
			UserID: "u_1", AgentEmail: "raise@fund.com", Address: "partner@fund.vc",
			NextActionAt: time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC),
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := json.Marshal(eventForDue(due).Data)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(legacyEventData(due))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("typing contact.due changed the wire payload\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// legacyEventData is the pre-typing map builder, preserved only as the
// comparison target of TestEventForDueIsByteIdenticalToTheLegacyMap.
func legacyEventData(d identity.DueEngagement) map[string]any {
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
	return data
}

// goldenDueEngagement is the fully populated fixture input: every optional
// field present.
func goldenDueEngagement() identity.DueEngagement {
	lastOutbound := time.Date(2026, 6, 26, 10, 30, 0, 123456789, time.UTC)
	return identity.DueEngagement{
		EngagementID: "cen_0123456789abcdef0123456789abcdef",
		UserID:       "usr_0123456789abcdef0123456789abcdef",
		AgentEmail:   "raise@agents.example.com",
		Address:      "a.partner@fund.vc",
		Stage:        "touch1",
		NextActionAt: time.Date(2026, 7, 1, 10, 30, 0, 123456789, time.UTC),
		// Deliberately BEFORE next_action_at: a due schedule that has already
		// been mailed once is the common shape.
		LastOutboundAt:     &lastOutbound,
		OutboundCount:      2,
		LastConversationID: "conv_0123456789abcdef0123456789abcdef",
		DisplayName:        "A. Partner",
		ContactMetadata:    map[string]any{"fund": "Example Capital", "warm": true},
		Replied:            false,
	}
}

// minimalDueEngagement is the required-fields-only fixture input: it locks the
// omitempty presence semantics (no last_outbound_at, no last_conversation_id)
// and that display_name/metadata stay PRESENT when empty.
func minimalDueEngagement() identity.DueEngagement {
	return identity.DueEngagement{
		EngagementID:    "cen_0123456789abcdef0123456789abcdef",
		UserID:          "usr_0123456789abcdef0123456789abcdef",
		AgentEmail:      "raise@agents.example.com",
		Address:         "a.partner@fund.vc",
		Stage:           "",
		NextActionAt:    time.Date(2026, 7, 1, 10, 30, 0, 123456789, time.UTC),
		ContactMetadata: map[string]any{},
	}
}
