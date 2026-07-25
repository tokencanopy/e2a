package loopback

import (
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/inboundpolicy"
	"github.com/tokencanopy/e2a/internal/inboundscreen"
	"github.com/tokencanopy/e2a/internal/piguard"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

func screeningFixtureAgent() *identity.AgentIdentity {
	return &identity.AgentIdentity{
		ID:            "bot@scr.example.com",
		Email:         "bot",
		Domain:        "scr.example.com",
		UserID:        "user_1",
		InboundPolicy: inboundpolicy.Allowlist,
	}
}

func screeningFixtureMessage() *identity.Message {
	return &identity.Message{
		ID:             "msg_inbound1",
		ConversationID: "conv_1",
		Subject:        "note",
		ReplyTo:        []string{"support@example.com"},
	}
}

func payload(t *testing.T, e webhookpub.Event) map[string]interface{} {
	t.Helper()
	data, ok := e.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("event data is %T, want map", e.Data)
	}
	return data
}

// Allow verdict, no gate flag → no screening events at all (the caller's
// email.received is the only signal, unchanged).
func TestScreeningEvents_AllowProducesNothing(t *testing.T) {
	events := ScreeningEvents(screeningFixtureAgent(), screeningFixtureMessage(),
		inboundpolicy.Decision{}, inboundscreen.Result{AppliedAction: piguard.ActionAllow})
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0 for a clean allow", len(events))
	}
}

// Gate flag, delivered → exactly one email.flagged mirroring the relay payload
// shape with loopback identities (header_from = the agent, no envelope/auth).
func TestScreeningEvents_FlagDelivered(t *testing.T) {
	agent := screeningFixtureAgent()
	msg := screeningFixtureMessage()
	gate := inboundpolicy.Decision{Flagged: true, Reason: "sender not on the agent's inbound allowlist"}
	events := ScreeningEvents(agent, msg, gate, inboundscreen.Result{AppliedAction: piguard.ActionFlag})
	if len(events) != 1 || events[0].Type != webhookpub.EventEmailFlagged {
		t.Fatalf("events = %+v, want exactly one email.flagged", events)
	}
	e := events[0]
	if e.AgentID != agent.ID || e.MessageID != msg.ID || e.ConversationID != msg.ConversationID {
		t.Errorf("routing fields drifted: %+v", e)
	}
	if e.ID != webhookpub.DeterministicEventID(msg.ID, webhookpub.EventEmailFlagged) {
		t.Errorf("event id must be deterministic, got %q", e.ID)
	}
	data := payload(t, e)
	if data["reason"] != gate.Reason || data["policy"] != agent.InboundPolicy || data["direction"] != "inbound" {
		t.Errorf("payload drifted: %v", data)
	}
	if hf, ok := data["header_from"].(*string); !ok || *hf != agent.EmailAddress() {
		t.Errorf("header_from = %v, want the agent's own address", data["header_from"])
	}
	if data["envelope_from"] != nil || data["authentication"] != nil {
		t.Errorf("loopback events must carry nil envelope_from/authentication: %v", data)
	}
}

// Gate flag escalated to a hold → email.flagged is suppressed (relay parity:
// a held message emits only its hold event).
func TestScreeningEvents_FlagSuppressedWhenHeld(t *testing.T) {
	gate := inboundpolicy.Decision{Flagged: true, Reason: "miss"}
	res := inboundscreen.Result{
		AppliedAction: piguard.ActionReview, Hold: true, Reason: "miss",
		Denorm: identity.InboundScreening{Status: identity.MessageStatusPendingReview, ReviewReason: identity.ReviewReasonSenderGate},
	}
	events := ScreeningEvents(screeningFixtureAgent(), screeningFixtureMessage(), gate, res)
	if len(events) != 1 || events[0].Type != webhookpub.EventEmailReviewRequested {
		t.Fatalf("events = %+v, want exactly one email.review_requested (no email.flagged)", events)
	}
}

// Review verdict → email.review_requested with reason_source + the hold TTL.
func TestScreeningEvents_Review(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	res := inboundscreen.Result{
		AppliedAction: piguard.ActionReview, Hold: true, Reason: "content scan: prompt_injection",
		Denorm: identity.InboundScreening{
			Status: identity.MessageStatusPendingReview, ReviewReason: identity.ReviewReasonInboundScan,
			ApprovalExpiresAt: &exp,
		},
	}
	events := ScreeningEvents(screeningFixtureAgent(), screeningFixtureMessage(), inboundpolicy.Decision{}, res)
	if len(events) != 1 || events[0].Type != webhookpub.EventEmailReviewRequested {
		t.Fatalf("events = %+v, want exactly one email.review_requested", events)
	}
	data := payload(t, events[0])
	if data["reason_source"] != identity.ReviewReasonInboundScan || data["reason"] != res.Reason {
		t.Errorf("payload drifted: %v", data)
	}
	if got, ok := data["approval_expires_at"].(*time.Time); !ok || !got.Equal(exp) {
		t.Errorf("approval_expires_at = %v, want %v", data["approval_expires_at"], exp)
	}
	if events[0].ID != webhookpub.DeterministicEventID("msg_inbound1", webhookpub.EventEmailReviewRequested) {
		t.Errorf("event id must be deterministic, got %q", events[0].ID)
	}
}

// Block verdict → email.blocked (accept-then-quarantine's only signal).
func TestScreeningEvents_Block(t *testing.T) {
	res := inboundscreen.Result{
		AppliedAction: piguard.ActionBlock, Hold: true, Reason: "content scan: prompt_injection",
		Denorm: identity.InboundScreening{Status: identity.MessageStatusReviewRejected, ReviewReason: identity.ReviewReasonInboundScan},
	}
	events := ScreeningEvents(screeningFixtureAgent(), screeningFixtureMessage(), inboundpolicy.Decision{}, res)
	if len(events) != 1 || events[0].Type != webhookpub.EventEmailBlocked {
		t.Fatalf("events = %+v, want exactly one email.blocked", events)
	}
	data := payload(t, events[0])
	if data["reason_source"] != identity.ReviewReasonInboundScan || data["subject"] != "note" || data["delivered_to"] != "bot@scr.example.com" {
		t.Errorf("payload drifted: %v", data)
	}
	if events[0].ID != webhookpub.DeterministicEventID("msg_inbound1", webhookpub.EventEmailBlocked) {
		t.Errorf("event id must be deterministic, got %q", events[0].ID)
	}
}
