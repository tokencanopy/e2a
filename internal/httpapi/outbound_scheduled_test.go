package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// scheduleEchoDeps wires a DeliverOutbound that mirrors the real accept path's
// scheduled/immediate decision (Status="scheduled" + ScheduledAt when the edge
// parsed a future send_at, else "accepted"), and a PollSendOutcome that flips
// *polled so a test can assert wait=sent never polled a scheduled send.
func scheduleEchoDeps(polled *bool) func(*Deps) {
	return func(d *Deps) {
		d.DeliverOutbound = func(_ context.Context, _ *identity.User, _ *identity.AgentIdentity, req outbound.SendRequest, _, _ string, _ *identity.Message, _ agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			if req.ScheduledAt != nil {
				return &agent.OutboundResult{MessageID: "msg_sched_1", Status: "scheduled", ScheduledAt: req.ScheduledAt, Method: "smtp"}, nil
			}
			return &agent.OutboundResult{MessageID: "msg_imm_1", Status: "accepted", Method: "smtp"}, nil
		}
		d.PollSendOutcome = func(_ context.Context, _ string) (identity.SendOutcome, error) {
			if polled != nil {
				*polled = true
			}
			return identity.SendOutcome{DeliveryStatus: "sent", ProviderMessageID: "ses-x", SentAs: "relay"}, nil
		}
	}
}

// TestSend_ScheduledFuture: a future send_at is accepted as status=scheduled with
// scheduled_at echoed, at 202 — and wait=sent must NOT poll (a scheduled send has
// no imminent outcome; the "scheduled" presentation status is what skips the poll
// loop). This pins the edge→DeliverOutbound threading and outboundResultView.
func TestSend_ScheduledFuture(t *testing.T) {
	polled := false
	srv := testServer(t, scheduleEchoDeps(&polled))
	at := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages?wait=sent", "good",
		map[string]any{"to": []string{"x@y.com"}, "subject": "s", "text": "b", "send_at": at})
	if code != 202 {
		t.Fatalf("scheduled send: want 202, got %d (%v)", code, body)
	}
	if body["status"] != "scheduled" {
		t.Fatalf("want status=scheduled, got %v", body["status"])
	}
	if body["scheduled_at"] == nil || body["scheduled_at"] == "" {
		t.Fatalf("want scheduled_at echoed, got %v", body["scheduled_at"])
	}
	if polled {
		t.Fatal("wait=sent must NOT poll a scheduled send")
	}
}

// TestSend_PastSendAt_Immediate: a send_at at/before now is treated as an ordinary
// immediate send (status=accepted), never rejected — clock skew shouldn't turn an
// intended-now send into an error.
func TestSend_PastSendAt_Immediate(t *testing.T) {
	srv := testServer(t, scheduleEchoDeps(nil))
	at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages", "good",
		map[string]any{"to": []string{"x@y.com"}, "subject": "s", "text": "b", "send_at": at})
	if code != 202 || body["status"] != "accepted" {
		t.Fatalf("past send_at: want 202 accepted (immediate), got %d %v", code, body)
	}
	if body["scheduled_at"] != nil {
		t.Fatalf("immediate send must not carry scheduled_at, got %v", body["scheduled_at"])
	}
}

// TestSend_SendAtBeyondHorizon_Rejected: a send_at past the max horizon is a 400
// invalid_request, and never reaches DeliverOutbound.
func TestSend_SendAtBeyondHorizon_Rejected(t *testing.T) {
	delivered := false
	srv := testServer(t, func(d *Deps) {
		scheduleEchoDeps(nil)(d)
		d.DeliverOutbound = func(_ context.Context, _ *identity.User, _ *identity.AgentIdentity, _ outbound.SendRequest, _, _ string, _ *identity.Message, _ agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			delivered = true
			return &agent.OutboundResult{MessageID: "msg_no", Status: "accepted"}, nil
		}
	})
	at := time.Now().Add(100 * 24 * time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages", "good",
		map[string]any{"to": []string{"x@y.com"}, "subject": "s", "text": "b", "send_at": at})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("over-horizon send_at: want 400 invalid_request, got %d %v", code, body)
	}
	if delivered {
		t.Fatal("over-horizon send_at must be rejected before DeliverOutbound")
	}
}

// TestReply_ScheduledFuture: a future send_at on REPLY takes the same
// scheduled path as send — 202 status=scheduled with scheduled_at echoed, and
// wait=sent returns immediately without polling. The referenced message is the
// default fixture inbound msg_in1 (From: alice@x.com).
func TestReply_ScheduledFuture(t *testing.T) {
	polled := false
	srv := testServer(t, scheduleEchoDeps(&polled))
	at := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply?wait=sent", "good",
		map[string]any{"text": "later reply", "send_at": at})
	if code != 202 {
		t.Fatalf("scheduled reply: want 202, got %d (%v)", code, body)
	}
	if body["status"] != "scheduled" {
		t.Fatalf("want status=scheduled, got %v", body["status"])
	}
	if body["scheduled_at"] == nil || body["scheduled_at"] == "" {
		t.Fatalf("want scheduled_at echoed, got %v", body["scheduled_at"])
	}
	if polled {
		t.Fatal("wait=sent must NOT poll a scheduled reply")
	}
}

// TestReply_PastSendAt_Immediate: a send_at at/before now on reply is an
// ordinary immediate reply (status=accepted), never rejected.
func TestReply_PastSendAt_Immediate(t *testing.T) {
	srv := testServer(t, scheduleEchoDeps(nil))
	at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply", "good",
		map[string]any{"text": "now reply", "send_at": at})
	if code != 202 || body["status"] != "accepted" {
		t.Fatalf("past send_at on reply: want 202 accepted (immediate), got %d %v", code, body)
	}
	if body["scheduled_at"] != nil {
		t.Fatalf("immediate reply must not carry scheduled_at, got %v", body["scheduled_at"])
	}
}

// TestReply_SendAtBeyondHorizon_Rejected: an over-horizon send_at on reply is a
// 400 invalid_request before DeliverOutbound.
func TestReply_SendAtBeyondHorizon_Rejected(t *testing.T) {
	delivered := false
	srv := testServer(t, func(d *Deps) {
		scheduleEchoDeps(nil)(d)
		d.DeliverOutbound = func(_ context.Context, _ *identity.User, _ *identity.AgentIdentity, _ outbound.SendRequest, _, _ string, _ *identity.Message, _ agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			delivered = true
			return &agent.OutboundResult{MessageID: "msg_no", Status: "accepted"}, nil
		}
	})
	at := time.Now().Add(100 * 24 * time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply", "good",
		map[string]any{"text": "too far reply", "send_at": at})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("over-horizon send_at on reply: want 400 invalid_request, got %d %v", code, body)
	}
	if delivered {
		t.Fatal("over-horizon send_at must be rejected before DeliverOutbound")
	}
}

// TestForward_ScheduledFuture: a future send_at on FORWARD takes the same
// scheduled path — 202 status=scheduled with scheduled_at echoed, and wait=sent
// returns immediately without polling.
func TestForward_ScheduledFuture(t *testing.T) {
	polled := false
	srv := testServer(t, scheduleEchoDeps(&polled))
	at := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/forward?wait=sent", "good",
		map[string]any{"to": []string{"x@y.com"}, "text": "later forward", "send_at": at})
	if code != 202 {
		t.Fatalf("scheduled forward: want 202, got %d (%v)", code, body)
	}
	if body["status"] != "scheduled" {
		t.Fatalf("want status=scheduled, got %v", body["status"])
	}
	if body["scheduled_at"] == nil || body["scheduled_at"] == "" {
		t.Fatalf("want scheduled_at echoed, got %v", body["scheduled_at"])
	}
	if polled {
		t.Fatal("wait=sent must NOT poll a scheduled forward")
	}
}

// TestForward_PastSendAt_Immediate: a send_at at/before now on forward is an
// ordinary immediate forward (status=accepted), never rejected.
func TestForward_PastSendAt_Immediate(t *testing.T) {
	srv := testServer(t, scheduleEchoDeps(nil))
	at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/forward", "good",
		map[string]any{"to": []string{"x@y.com"}, "text": "now forward", "send_at": at})
	if code != 202 || body["status"] != "accepted" {
		t.Fatalf("past send_at on forward: want 202 accepted (immediate), got %d %v", code, body)
	}
	if body["scheduled_at"] != nil {
		t.Fatalf("immediate forward must not carry scheduled_at, got %v", body["scheduled_at"])
	}
}

// TestForward_SendAtBeyondHorizon_Rejected: an over-horizon send_at on forward
// is a 400 invalid_request before DeliverOutbound.
func TestForward_SendAtBeyondHorizon_Rejected(t *testing.T) {
	delivered := false
	srv := testServer(t, func(d *Deps) {
		scheduleEchoDeps(nil)(d)
		d.DeliverOutbound = func(_ context.Context, _ *identity.User, _ *identity.AgentIdentity, _ outbound.SendRequest, _, _ string, _ *identity.Message, _ agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			delivered = true
			return &agent.OutboundResult{MessageID: "msg_no", Status: "accepted"}, nil
		}
	})
	at := time.Now().Add(100 * 24 * time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/forward", "good",
		map[string]any{"to": []string{"x@y.com"}, "text": "too far forward", "send_at": at})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("over-horizon send_at on forward: want 400 invalid_request, got %d %v", code, body)
	}
	if delivered {
		t.Fatal("over-horizon send_at must be rejected before DeliverOutbound")
	}
}

// TestTrashRestoreScheduledMessage pins the thin HTTP seam over the scheduled
// trash/restore store semantics (covered deeply in internal/identity and the
// contract scenarios): a scheduled outbound message moves to trash with a 200
// deletion receipt, and restoring BEFORE its scheduled_at returns the live
// view with delivery_status=accepted and scheduled_at preserved.
func TestTrashRestoreScheduledMessage(t *testing.T) {
	at := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	deleted := false
	msg := func() *identity.Message {
		m := &identity.Message{
			ID: "msg_sched", AgentID: "support@acme.com", Direction: "outbound",
			Sender: "support@acme.com", Subject: "scheduled", DeliveryStatus: "accepted",
			ScheduledAt: &at, CreatedAt: time.Unix(1700000000, 0).UTC(),
		}
		if deleted {
			dt := time.Unix(1700001000, 0).UTC()
			m.DeletedAt = &dt
		}
		return m
	}
	srv := testServer(t, func(d *Deps) {
		d.DeleteMessage = func(_ context.Context, messageID, agentID string) error {
			if messageID != "msg_sched" || agentID != "support@acme.com" {
				return identity.ErrMessageNotFound
			}
			deleted = true
			return nil
		}
		d.RestoreMessage = func(_ context.Context, messageID, agentID string) (*identity.Message, error) {
			if !deleted {
				return nil, identity.ErrNotInTrash
			}
			deleted = false
			return msg(), nil
		}
		d.GetMessage = func(_ context.Context, messageID, agentID string) (*identity.Message, error) {
			return msg(), nil // direct GET is intentionally any-state
		}
	})

	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com/messages/msg_sched", "good", nil)
	if code != 200 || body["deleted"] != true || body["id"] != "msg_sched" {
		t.Fatalf("trash scheduled message: want 200 deletion receipt, got %d %v", code, body)
	}

	code, body = sendJSON(t, "POST", srv.URL+"/v1/agents/support%40acme.com/messages/msg_sched/restore", "good", nil)
	if code != 200 {
		t.Fatalf("restore scheduled message: want 200, got %d %v", code, body)
	}
	if body["id"] != "msg_sched" || body["delivery_status"] != "accepted" {
		t.Fatalf("restored scheduled view = %v, want accepted rollup", body)
	}
	if body["scheduled_at"] != at.Format(time.RFC3339) {
		t.Fatalf("restored view must preserve scheduled_at, got %v want %v", body["scheduled_at"], at.Format(time.RFC3339))
	}
	if _, present := body["deleted_at"]; present {
		t.Fatalf("restored view must omit deleted_at, got %v", body["deleted_at"])
	}
}

// TestScheduledInstant pins the edge validation/normalization directly.
func TestScheduledInstant(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour)

	if got, env := scheduledInstant(nil, now); got != nil || env != nil {
		t.Fatalf("nil send_at: want (nil,nil), got (%v,%v)", got, env)
	}
	var zero time.Time
	if got, env := scheduledInstant(&zero, now); got != nil || env != nil {
		t.Fatalf("zero send_at: want (nil,nil), got (%v,%v)", got, env)
	}
	past := now.Add(-time.Minute)
	if got, env := scheduledInstant(&past, now); got != nil || env != nil {
		t.Fatalf("past send_at: want immediate (nil,nil), got (%v,%v)", got, env)
	}
	got, env := scheduledInstant(&future, now)
	if env != nil || got == nil || !got.Equal(future) {
		t.Fatalf("future send_at: want (%v,nil), got (%v,%v)", future, got, env)
	}
	if got.Location() != time.UTC {
		t.Fatalf("scheduled instant must be normalized to UTC, got %v", got.Location())
	}
	tooFar := now.Add(maxScheduleHorizon + time.Hour)
	if got, env := scheduledInstant(&tooFar, now); env == nil || got != nil {
		t.Fatalf("over-horizon send_at: want error, got (%v,%v)", got, env)
	}
}

// TestSpecDocumentsScheduledSendContract pins the machine-readable contract
// consumed by generated SDKs and API-reference tooling. Scheduled acceptance is
// a successful, non-retry outcome, wait=sent returns it immediately, and the
// immediate loopback path cannot honor a future send_at.
func TestSpecDocumentsScheduledSendContract(t *testing.T) {
	doc := renderSpec(t)
	requestSchemas := map[string]string{
		"sendMessage":    "SendEmailRequest",
		"replyToMessage": "ReplyRequest",
		"forwardMessage": "ForwardRequest",
	}
	for operationID, schemaName := range requestSchemas {
		operation := specOperation(t, doc, operationID)
		description, _ := operation["description"].(string)
		requireContractText(t, operationID, strings.ToLower(description),
			"beta: scheduled sending",
			"status=scheduled",
		)
		if operation["x-stability-level"] != nil {
			t.Errorf("%s must remain a stable operation; only its scheduled-send fields and value are beta", operationID)
		}

		acceptedDescription := specResponseDescription(t, operation, "202")
		requireContractText(t, operationID+" 202", acceptedDescription,
			"status=scheduled",
			"do not re-send",
		)

		parameters, _ := operation["parameters"].([]any)
		var waitDescription string
		for _, raw := range parameters {
			parameter, _ := raw.(map[string]any)
			if parameter["in"] == "query" && parameter["name"] == "wait" {
				waitDescription, _ = parameter["description"].(string)
				break
			}
		}
		requireContractText(t, operationID+" wait", waitDescription,
			"status=scheduled immediately",
			"does not wait",
		)

		sendAt, _ := schemaProps(t, doc, schemaName)["send_at"].(map[string]any)
		sendAtDescription, _ := sendAt["description"].(string)
		requireContractText(t, schemaName+".send_at", strings.ToLower(sendAtDescription),
			"beta:",
			"may change before it is declared stable",
			"own address",
			"400 invalid_request",
			"restoring at or after",
			"leaves the send canceled",
		)
		if sendAt["x-stability-level"] != "beta" {
			t.Errorf("%s.send_at must carry canonical x-stability-level: beta", schemaName)
		}

		badRequestDescription := specResponseDescription(t, operation, "400")
		requireContractText(t, operationID+" 400", strings.ToLower(badRequestDescription),
			"send_at",
			"own address",
			"not held for review",
		)
	}

	for _, schemaName := range []string{"MessageSummaryView", "MessageView", "SendResultView"} {
		scheduledAt, _ := schemaProps(t, doc, schemaName)["scheduled_at"].(map[string]any)
		description, _ := scheduledAt["description"].(string)
		requireContractText(t, schemaName+".scheduled_at", strings.ToLower(description),
			"beta:",
			"may change before it is declared stable",
		)
		if schemaName != "MessageSummaryView" {
			requireContractText(t, schemaName+".scheduled_at restore cutoff", strings.ToLower(description),
				"restoring at or after",
				"leaves the send canceled",
			)
		}
		if scheduledAt["x-stability-level"] != "beta" {
			t.Errorf("%s.scheduled_at must carry canonical x-stability-level: beta", schemaName)
		}
	}

	for _, schemaName := range []string{"MessageSummaryView", "MessageView"} {
		deliveryStatus, _ := schemaProps(t, doc, schemaName)["delivery_status"].(map[string]any)
		description, _ := deliveryStatus["description"].(string)
		requireContractText(t, schemaName+".delivery_status", strings.ToLower(description),
			"future scheduled_at",
			"remains accepted",
			"sendresultview.status",
		)
	}

	status, _ := schemaProps(t, doc, "SendResultView")["status"].(map[string]any)
	statusDescription, _ := status["description"].(string)
	requireContractText(t, "SendResultView.status", strings.ToLower(statusDescription),
		"scheduled is beta",
		"may change before it is declared stable",
	)
	experimentalValues, _ := status["x-experimental-values"].([]any)
	if len(experimentalValues) != 1 || experimentalValues[0] != "scheduled" {
		t.Errorf("SendResultView.status x-experimental-values = %v, want [scheduled]", experimentalValues)
	}
}
