package agent

import (
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/eventpayload/goldenassert"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

func TestBuildSentEvent_PopulatesEnvelope(t *testing.T) {
	a := &API{}
	agent := &identity.AgentIdentity{
		ID:     "bot@x.example.com",
		Domain: "x.example.com",
		UserID: "u_1",
	}
	outMsg := &identity.Message{ID: "msg_1"}
	res := &outbound.SendResult{
		MessageID: "ses_1",
		Method:    "smtp",
		To:        []string{"alice@example.com"},
	}
	req := outbound.SendRequest{
		To:             []string{"alice@example.com"},
		Subject:        "hello",
		ConversationID: "conv_42",
	}
	ev := a.buildSentEvent(agent, outMsg, res, req, "send")
	if ev.Type != webhookpub.EventEmailSent {
		t.Errorf("Type = %q, want email.sent", ev.Type)
	}
	if ev.UserID != "u_1" {
		t.Errorf("UserID = %q, want u_1", ev.UserID)
	}
	if ev.AgentID != agent.ID {
		t.Errorf("AgentID = %q, want %q", ev.AgentID, agent.ID)
	}
	if ev.MessageID != "msg_1" {
		t.Errorf("MessageID = %q, want msg_1", ev.MessageID)
	}
	if ev.ConversationID != "conv_42" {
		t.Errorf("ConversationID = %q, want conv_42", ev.ConversationID)
	}
	data, ok := ev.Data.(eventpayload.EmailSentData)
	if !ok {
		t.Fatalf("Data is not the canonical typed payload: %T", ev.Data)
	}
	if data.Subject != "hello" {
		t.Errorf("subject = %v, want hello", data.Subject)
	}
	if data.Direction != "outbound" || data.AgentEmail != agent.EmailAddress() {
		t.Errorf("data = %+v", data)
	}
}

// TestSentEventGoldenPayloads is this package's side of the cross-channel
// drift lock for fields owned by these pure builders. Lifecycle rows are
// deliberately excluded here: DB-backed worker tests prove the emitted event
// contains the exact row returned by AppendTx, rather than injecting a fixture
// transition into the producer and creating a circular test.
func TestSentEventGoldenPayloads(t *testing.T) {
	const fixture = "../eventpayload/testdata/"
	agent := &identity.AgentIdentity{
		ID:     "support@agents.example.com",
		Domain: "agents.example.com",
		UserID: "user_7a6b5c4d",
	}

	t.Run("email.sent sync builder", func(t *testing.T) {
		a := &API{}
		ev := a.buildSentEvent(
			agent,
			&identity.Message{ID: "msg_01h2xcejqtf2nbrexx3vqjhp42"},
			&outbound.SendResult{
				MessageID: "0100019283abcdef-1a2b3c4d-0000",
				Method:    "smtp",
				To:        []string{"alice@customer.example.com"},
				CC:        []string{"ops@customer.example.com"},
				BCC:       []string{"audit@agents.example.com"},
			},
			outbound.SendRequest{
				Subject:        "Re: Order #1234 delayed",
				ConversationID: "conv_9f8e7d6c",
			},
			"reply",
		)
		goldenassert.DataIgnoringLifecycle(t, fixture+"email.sent.json", ev.Data)
	})

	t.Run("email.sent async builder emits the identical payload", func(t *testing.T) {
		ev := buildEmailSentEventFromRow(&identity.OutboundSentInfo{
			UserID: "user_7a6b5c4d",
			Message: &identity.Message{
				ID:             "msg_01h2xcejqtf2nbrexx3vqjhp42",
				AgentID:        agent.ID,
				Sender:         "support@agents.example.com",
				Method:         "smtp",
				ToRecipients:   []string{"alice@customer.example.com"},
				CC:             []string{"ops@customer.example.com"},
				BCC:            []string{"audit@agents.example.com"},
				Subject:        "Re: Order #1234 delayed",
				Type:           "reply",
				ConversationID: "conv_9f8e7d6c",
			},
		}, "0100019283abcdef-1a2b3c4d-0000")
		goldenassert.DataIgnoringLifecycle(t, fixture+"email.sent.json", ev.Data)
	})

	t.Run("email.failed async builder", func(t *testing.T) {
		ev := buildEmailFailedEventFromRow(&identity.OutboundSentInfo{
			UserID: "user_7a6b5c4d",
			Message: &identity.Message{
				ID:             "msg_01h2xcejqtf2nbrexx3vqjhp43",
				AgentID:        agent.ID,
				Sender:         "support@agents.example.com",
				Method:         "smtp",
				ToRecipients:   []string{"alice@customer.example.com"},
				CC:             []string{"ops@customer.example.com"},
				BCC:            []string{"audit@agents.example.com"},
				Subject:        "Re: Order #1234 delayed",
				Type:           "send",
				ConversationID: "conv_9f8e7d6c",
			},
		}, "550 5.1.1 user unknown")
		goldenassert.DataIgnoringLifecycle(t, fixture+"email.failed.json", ev.Data, "reason_code", "retryable")
	})

	// Minimal (required-fields-only) variants: the same builders fed only the
	// required inputs must byte-match the .min.json fixtures, locking the
	// omitempty presence semantics (no cc/bcc/conversation_id on the wire when
	// unset) that the fully-populated fixtures above can't detect.

	t.Run("email.sent sync builder minimal", func(t *testing.T) {
		a := &API{}
		ev := a.buildSentEvent(
			agent,
			&identity.Message{ID: "msg_01h2xcejqtf2nbrexx3vqjhp42"},
			&outbound.SendResult{
				MessageID: "0100019283abcdef-1a2b3c4d-0000",
				Method:    "smtp",
				To:        []string{"alice@customer.example.com"},
			},
			outbound.SendRequest{Subject: "Re: Order #1234 delayed"},
			"reply",
		)
		goldenassert.Data(t, fixture+"email.sent.min.json", ev.Data)
	})

	t.Run("email.sent async builder minimal emits the identical payload", func(t *testing.T) {
		ev := buildEmailSentEventFromRow(&identity.OutboundSentInfo{
			UserID: "user_7a6b5c4d",
			Message: &identity.Message{
				ID:           "msg_01h2xcejqtf2nbrexx3vqjhp42",
				AgentID:      agent.ID,
				Sender:       "support@agents.example.com",
				Method:       "smtp",
				ToRecipients: []string{"alice@customer.example.com"},
				Subject:      "Re: Order #1234 delayed",
				Type:         "reply",
			},
		}, "0100019283abcdef-1a2b3c4d-0000")
		goldenassert.Data(t, fixture+"email.sent.min.json", ev.Data)
	})

	t.Run("email.failed async builder minimal", func(t *testing.T) {
		ev := buildEmailFailedEventFromRow(&identity.OutboundSentInfo{
			UserID: "user_7a6b5c4d",
			Message: &identity.Message{
				ID:           "msg_01h2xcejqtf2nbrexx3vqjhp43",
				AgentID:      agent.ID,
				Sender:       "support@agents.example.com",
				Method:       "smtp",
				ToRecipients: []string{"alice@customer.example.com"},
				Subject:      "Re: Order #1234 delayed",
				Type:         "send",
			},
		}, "550 5.1.1 user unknown")
		goldenassert.Data(t, fixture+"email.failed.min.json", ev.Data)
	})
}

func TestBuildSentEvent_NilOutMsgUsesEmptyMessageID(t *testing.T) {
	a := &API{}
	agent := &identity.AgentIdentity{ID: "bot@x.example.com", UserID: "u_1"}
	res := &outbound.SendResult{MessageID: "ses_2", Method: "smtp"}
	ev := a.buildSentEvent(agent, nil, res, outbound.SendRequest{}, "send")
	if ev.MessageID != "" {
		t.Errorf("MessageID should be empty when outMsg is nil, got %q", ev.MessageID)
	}
}

func TestBuildPendingApprovalEvent(t *testing.T) {
	a := &API{}
	agent := &identity.AgentIdentity{ID: "bot@x.example.com", UserID: "u_1"}
	expiry := time.Now().Add(1 * time.Hour)
	msg := &identity.Message{ID: "pend_1", ApprovalExpiresAt: &expiry}
	req := outbound.SendRequest{To: []string{"alice@example.com"}, Subject: "review me"}
	ev := a.buildPendingApprovalEvent(agent, msg, req, "send")
	if ev.Type != webhookpub.EventEmailReviewRequested {
		t.Errorf("Type = %q, want email.review_requested", ev.Type)
	}
	pdata := ev.Data.(map[string]interface{})
	if pdata["message_type"] != "send" {
		t.Errorf("message_type = %v, want send", pdata["message_type"])
	}
	if pdata["agent_email"] != agent.ID {
		t.Errorf("agent_email = %v, want %q", pdata["agent_email"], agent.ID)
	}
	if ev.MessageID != "pend_1" {
		t.Errorf("MessageID = %q, want pend_1", ev.MessageID)
	}
	if ev.UserID != "u_1" {
		t.Errorf("UserID = %q", ev.UserID)
	}
}

func TestBuildPendingApprovalEvent_CarriesScheduledAt(t *testing.T) {
	a := &API{}
	agent := &identity.AgentIdentity{ID: "bot@x.example.com", UserID: "u_1"}
	future := time.Now().Add(2 * time.Hour)
	msg := &identity.Message{ID: "pend_1", ScheduledAt: &future}
	req := outbound.SendRequest{To: []string{"alice@example.com"}, Subject: "review me"}
	ev := a.buildPendingApprovalEvent(agent, msg, req, "send")
	pdata := ev.Data.(map[string]interface{})
	if pdata["scheduled_at"] != &future {
		t.Errorf("scheduled_at = %v, want %v", pdata["scheduled_at"], &future)
	}
}

func TestBuildPendingApprovalEvent_OmitsScheduledAtForImmediateSend(t *testing.T) {
	a := &API{}
	agent := &identity.AgentIdentity{ID: "bot@x.example.com", UserID: "u_1"}
	msg := &identity.Message{ID: "pend_1"}
	req := outbound.SendRequest{To: []string{"alice@example.com"}, Subject: "review me"}
	ev := a.buildPendingApprovalEvent(agent, msg, req, "send")
	pdata := ev.Data.(map[string]interface{})
	if _, ok := pdata["scheduled_at"]; ok {
		t.Errorf("scheduled_at should be omitted for an immediate send, got %v", pdata["scheduled_at"])
	}
}

func TestBuildPendingApprovalEventUsesProvidedExactHoldTransition(t *testing.T) {
	transition := messagelifecycle.MessageLifecycleTransition{ID: "mlt_exact", MessageID: "msg_pending", ReasonCode: messagelifecycle.ReasonReviewHoldCreated}
	agent := &identity.AgentIdentity{ID: "bot@example.test", UserID: "user_owner"}
	msg := &identity.Message{ID: "msg_pending", LifecycleTransitions: []messagelifecycle.MessageLifecycleTransition{transition}}
	event := (&API{}).buildPendingApprovalEvent(agent, msg, outbound.SendRequest{To: []string{"alice@example.net"}, Subject: "review me"}, "send")
	got, ok := event.Data.(map[string]interface{})["lifecycle_transitions"].([]messagelifecycle.MessageLifecycleTransition)
	if !ok || len(got) != 1 {
		t.Fatalf("lifecycle_transitions = %#v, want one exact hold transition", event.Data)
	}
	if got[0].ID != transition.ID {
		t.Fatalf("event transition id = %q, provided = %q", got[0].ID, transition.ID)
	}
}

func TestBuildApprovedEvent(t *testing.T) {
	a := &API{}
	agent := &identity.AgentIdentity{ID: "bot@x.example.com", UserID: "u_1"}
	sent := &identity.Message{
		ID:                "msg_a",
		Subject:           "hi",
		Type:              "send",
		ProviderMessageID: "ses_a",
		Method:            "smtp",
		ToRecipients:      []string{"alice@example.com"},
		Edited:            true,
	}
	ev := a.buildApprovedEvent(agent, sent, "u_reviewer")
	if ev.Type != webhookpub.EventEmailReviewApproved {
		t.Errorf("Type = %q", ev.Type)
	}
	if ev.MessageID != "msg_a" {
		t.Errorf("MessageID = %q", ev.MessageID)
	}
	data := ev.Data.(map[string]interface{})
	if data["edited"] != true {
		t.Errorf("edited = %v, want true", data["edited"])
	}
	if data["message_type"] != "send" {
		t.Errorf("message_type = %v, want send", data["message_type"])
	}
	if data["agent_email"] != agent.ID {
		t.Errorf("agent_email = %v, want %q", data["agent_email"], agent.ID)
	}
	if _, ok := data["reviewed_by_user_id"]; ok {
		t.Errorf("reviewed_by_user_id must not be exposed, got %v", data["reviewed_by_user_id"])
	}
	if data["provider_message_id"] != "ses_a" {
		t.Errorf("provider_message_id = %v, want ses_a", data["provider_message_id"])
	}
	sent.Method = "loopback"
	sent.ProviderMessageID = "<local@loopback.x.example.com>"
	localData := a.buildApprovedEvent(agent, sent, "u_reviewer").Data.(map[string]interface{})
	if _, ok := localData["provider_message_id"]; ok {
		t.Errorf("providerless loopback review event leaked provider_message_id: %v", localData)
	}
}

func TestBuildApprovedEvent_CarriesScheduledAt(t *testing.T) {
	a := &API{}
	agent := &identity.AgentIdentity{ID: "bot@x.example.com", UserID: "u_1"}
	future := time.Now().Add(2 * time.Hour)
	sent := &identity.Message{ID: "msg_a", Type: "send", ScheduledAt: &future}
	data := a.buildApprovedEvent(agent, sent, "u_reviewer").Data.(map[string]interface{})
	if data["scheduled_at"] != &future {
		t.Errorf("scheduled_at = %v, want %v", data["scheduled_at"], &future)
	}
}

func TestBuildApprovedEvent_OmitsScheduledAtForImmediateSend(t *testing.T) {
	a := &API{}
	agent := &identity.AgentIdentity{ID: "bot@x.example.com", UserID: "u_1"}
	sent := &identity.Message{ID: "msg_a", Type: "send"}
	data := a.buildApprovedEvent(agent, sent, "u_reviewer").Data.(map[string]interface{})
	if _, ok := data["scheduled_at"]; ok {
		t.Errorf("scheduled_at should be omitted for an immediate send, got %v", data["scheduled_at"])
	}
}

func TestBuildRejectedEvent(t *testing.T) {
	a := &API{}
	rejected := &identity.Message{ID: "msg_r", AgentID: "bot@x.example.com", Type: "send"}
	ev := a.buildRejectedEvent("u_reviewer", rejected, "off-policy")
	if ev.Type != webhookpub.EventEmailReviewRejected {
		t.Errorf("Type = %q", ev.Type)
	}
	if ev.MessageID != "msg_r" {
		t.Errorf("MessageID = %q", ev.MessageID)
	}
	data := ev.Data.(map[string]interface{})
	if data["reason"] != "off-policy" {
		t.Errorf("reason = %v, want off-policy", data["reason"])
	}
	if data["message_type"] != "send" {
		t.Errorf("message_type = %v, want send", data["message_type"])
	}
	if data["agent_email"] != "bot@x.example.com" {
		t.Errorf("agent_email = %v, want bot@x.example.com", data["agent_email"])
	}
	if _, ok := data["reviewed_by_user_id"]; ok {
		t.Errorf("reviewed_by_user_id must not be exposed")
	}
}

func TestEmitBlockedOutbound(t *testing.T) {
	// email.blocked emits through the (unconditional) outbox, not a legacy
	// publisher, so assert the built event's shape/routing directly.
	a := &API{}
	agent := &identity.AgentIdentity{ID: "bot@x.example.com", Domain: "x.example.com", UserID: "u_1"}
	req := outbound.SendRequest{To: []string{"alice@evil.com"}, Subject: "blocked one", ConversationID: "conv_9"}
	v := outboundVerdict{Applied: "block", ReviewReason: "recipient_gate", Reason: "recipient not in allowlist"}
	softRef := blockAuditID(agent.ID, req)

	ev := a.buildBlockedOutboundEvent(agent, softRef, req, v)

	if ev.Type != webhookpub.EventEmailBlocked {
		t.Errorf("Type = %q, want email.blocked", ev.Type)
	}
	// MessageID must stay UNSET: webhook_events.message_id has an FK to
	// messages(id) and a blocked send is rowless — the msgblk_ soft-ref there
	// fails the FK and the event is dropped (the exact bug the staging
	// conformance gate caught). The soft-ref travels in data.message_id only.
	if ev.UserID != "u_1" || ev.AgentID != agent.ID || ev.MessageID != "" {
		t.Errorf("routing keys = (user=%q agent=%q msg=%q), want msg unset", ev.UserID, ev.AgentID, ev.MessageID)
	}
	// Deterministic id keeps a retried block idempotent.
	if want := webhookpub.DeterministicEventID(softRef, webhookpub.EventEmailBlocked); ev.ID != want {
		t.Errorf("ID = %q, want deterministic %q", ev.ID, want)
	}
	data, ok := ev.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("Data is not a map: %T", ev.Data)
	}
	if data["message_id"] != softRef {
		t.Errorf("data.message_id = %v, want soft-ref %q", data["message_id"], softRef)
	}
	if data["direction"] != "outbound" {
		t.Errorf("direction = %v, want outbound", data["direction"])
	}
	if data["reason_source"] != "recipient_gate" {
		t.Errorf("reason_source = %v, want recipient_gate", data["reason_source"])
	}
}
