package eventpayload_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// contactDueEvents are the canonical contact.due envelopes: the fully
// populated shape and the required-fields-only shape that locks the omitempty
// presence semantics. internal/contactdue asserts its real builder against the
// same files, so a field rename fails on both sides against one artifact.
//
// contact.due is BETA, so it lives here rather than in the frozen StableEvents
// catalog — but it is fixtured exactly like a stable event. Beta means the
// shape may still change; it does not mean the shape may change by accident.
func contactDueEvents() []struct {
	fixture string
	event   webhookpub.Event
} {
	lastOutbound := time.Date(2026, 6, 26, 10, 30, 0, 123456789, time.UTC)
	nextAction := time.Date(2026, 7, 1, 10, 30, 0, 123456789, time.UTC)
	contact := eventpayload.ContactDueContact{
		Address:     "a.partner@fund.vc",
		DisplayName: "A. Partner",
		Metadata:    map[string]any{"fund": "Example Capital", "warm": true},
	}
	return []struct {
		fixture string
		event   webhookpub.Event
	}{
		{"contact.due.json", webhookpub.Event{
			ID: "evt_0123456789abcdef0123456789abcdef", Type: webhookpub.EventContactDue,
			CreatedAt: fixtureCreatedAt,
			Data: eventpayload.ContactDueData{
				Address:            "a.partner@fund.vc",
				AgentEmail:         "raise@agents.example.com",
				Contact:            contact,
				LastConversationID: "conv_0123456789abcdef0123456789abcdef",
				LastOutboundAt:     &lastOutbound,
				NextActionAt:       nextAction,
				OutboundCount:      2,
				Replied:            false,
				Stage:              "touch1",
			},
		}},
		{"contact.due.min.json", webhookpub.Event{
			ID: "evt_0123456789abcdef0123456789abcdef", Type: webhookpub.EventContactDue,
			CreatedAt: fixtureCreatedAt,
			Data: eventpayload.ContactDueData{
				Address:    "a.partner@fund.vc",
				AgentEmail: "raise@agents.example.com",
				Contact: eventpayload.ContactDueContact{
					Address:  "a.partner@fund.vc",
					Metadata: map[string]any{},
				},
				NextActionAt: nextAction,
			},
		}},
	}
}

func TestContactDueGoldenFixtures(t *testing.T) {
	for _, c := range contactDueEvents() {
		c := c
		t.Run(c.fixture, func(t *testing.T) {
			got, err := json.MarshalIndent(c.event.AsEnvelope(), "", "  ")
			if err != nil {
				t.Fatalf("marshal envelope: %v", err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", c.fixture)
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("regenerated %s (%d bytes)", path, len(got))
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s (first time? run with -update): %v", path, err)
			}
			if !bytes.Equal(want, got) {
				t.Errorf("fixture %s drifted from ContactDueData — if the change is intentional, regenerate with -update and update the SDK types + docs\n got: %s\nwant: %s", path, got, want)
			}
		})
	}
}
