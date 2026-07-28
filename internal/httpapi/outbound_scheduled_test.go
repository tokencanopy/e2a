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
