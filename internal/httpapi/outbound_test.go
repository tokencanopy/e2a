package httpapi

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// sendURL is POST /v1/agents/{address}/messages for the test agent. The sender
// is the path agent (decision 3 — explicit operation, not a body `from`).
const sendURL = "/v1/agents/support%40acme.com/messages"

func TestSendSent(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
	})
	if code != 200 || body["status"] != "sent" || body["message_id"] != "msg_sent_1" || body["method"] != "smtp" {
		t.Fatalf("want 200 sent, got %d %v", code, body)
	}
}

func TestSendHeldForApproval(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "HOLD please", "text": "hello",
	})
	if code != 202 || body["status"] != "pending_review" || body["message_id"] != "msg_pending_1" {
		t.Fatalf("want 202 pending_approval, got %d %v", code, body)
	}
	if body["approval_expires_at"] == nil {
		t.Fatal("held response must carry approval_expires_at")
	}
	if _, present := body["method"]; present {
		t.Fatal("held response must not carry method")
	}
}

func TestSendMissingSubjectBody(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "", "text": "",
	})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request, got %d %v", code, body)
	}
}

func TestSendCRLFSubjectRejected(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "a\r\nInjected: x", "text": "hi",
	})
	if code != 400 {
		t.Fatalf("want 400 for CRLF subject, got %d %v", code, body)
	}
}

func TestSendNoRecipients(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"subject": "Hi", "text": "hello",
	})
	// `to` is now schema-required (MSG-3) → rejected at validation (422).
	if code != 422 {
		t.Fatalf("want 422 missing to, got %d %v", code, body)
	}
}

func TestSendInvalidRecipient(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"not-an-email"}, "subject": "Hi", "text": "hello",
	})
	if code != 400 || errCode(body) != "invalid_recipient" {
		t.Fatalf("want 400 invalid_recipient, got %d %v", code, body)
	}
}

// TestSendReplyToPropagates: a valid reply_to reaches the delivery layer verbatim
// (display name preserved), so the composer can set the Reply-To header.
func TestSendReplyToPropagates(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
		"reply_to": "Support <support@acme.com>",
	})
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent, got %d %v", code, body)
	}
	if got := lastDeliveredReq().ReplyTo; got != "Support <support@acme.com>" {
		t.Fatalf("delivered ReplyTo = %q, want %q", got, "Support <support@acme.com>")
	}
}

// TestSendInvalidReplyTo: a non-address reply_to is rejected at the edge (400)
// rather than silently mangled by the composer or bounced by the relay.
func TestSendInvalidReplyTo(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
		"reply_to": "not an address",
	})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request for bad reply_to, got %d %v", code, body)
	}
}

// TestSendMultiReplyToRejected: Reply-To carries a single mailbox in our contract;
// a comma list is rejected so callers don't rely on unspecified multi-address behavior.
func TestSendMultiReplyToRejected(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
		"reply_to": "a@x.com, b@x.com",
	})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request for multi reply_to, got %d %v", code, body)
	}
}

// TestSendSetsAgentAsSender: there is no body `from` — the sender is the path
// agent and auth scopes it. A plain send (no `from`) succeeds.
func TestSendSetsAgentAsSender(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
	})
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent, got %d %v", code, body)
	}
}

// TestSendNotOwnedAgent: sending through an agent the caller does not own is a
// 404 not_found (resolveOwnedAgent — indistinguishable from a nonexistent
// agent), never a cross-tenant send.
func TestSendNotOwnedAgent(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/other%40nope.com/messages", "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
	})
	if code != 404 {
		t.Fatalf("want 404 for an unowned agent, got %d", code)
	}
}

func TestSendOverCap(t *testing.T) {
	srv := testServer(t)
	// The cap check is covered by the agent-create/domain over-cap tests; here
	// we assert the message path wires EnforceMessageSend by checking a
	// successful send does NOT 402 for u_1.
	code, _ := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
	})
	if code == 402 {
		t.Fatalf("u_1 is under cap; should not 402")
	}
}

// TestSendLargeBodyAccepted guards the outbound request wire cap: Huma's default
// is 1 MiB, which would 413 attachment-bearing mail. The send op raises it to
// maxOutboundBytes (40 MB), so a request body over 1 MiB is accepted, not
// rejected. Proven with an attachment whose base64 wire size exceeds 1 MiB (the
// production reason the cap was raised) — the per-field maxLength limits now cap
// text/html at 1 MiB each, so an oversized single field is rejected by design
// (see TestSendRejectsOversizedTextBody) and can't exercise the wire cap.
func TestSendLargeBodyAccepted(t *testing.T) {
	srv := testServer(t)
	// 1.5 MiB decoded → ~2 MiB base64 on the wire: over Huma's 1 MiB default wire
	// cap, but under the 10 MiB per-attachment and composed caps.
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
		"attachments": attField(map[string]any{
			"filename": "big.bin", "content_type": "application/octet-stream",
			"data": b64(1500 * 1024),
		}),
	})
	if code == 413 {
		t.Fatalf("a request with a ~2 MiB base64 wire body must be accepted (cap raised to 40 MB), got 413")
	}
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent for a large-but-under-cap wire body, got %d %v", code, body)
	}
}

// b64 encodes n zero bytes as standard base64 — a cheaply-constructed attachment
// payload of a known DECODED size.
func b64(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

func attField(atts ...map[string]any) []map[string]any { return atts }

// TestSendAttachmentAccepted: a normal small attachment sends (200) — proves
// attachments flow through the send path and pass validation.
func TestSendAttachmentAccepted(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
		"attachments": attField(map[string]any{
			"filename": "note.txt", "content_type": "text/plain", "data": b64(1024),
		}),
	})
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent for a small attachment, got %d %v", code, body)
	}
}

// TestSendAttachmentTooLarge: a single attachment over 10 MB decoded → 413
// payload_too_large (proves the per-attachment size limit is wired on the send
// path and the status propagates).
func TestSendAttachmentTooLarge(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
		"attachments": attField(map[string]any{
			"filename": "big.bin", "content_type": "application/octet-stream",
			"data": b64(maxAttachmentBytes + 1),
		}),
	})
	if code != 413 || errCode(body) != "payload_too_large" {
		t.Fatalf("want 413 payload_too_large for an oversized attachment, got %d %v", code, body)
	}
}

// TestSendTooManyAttachments: more than 10 attachments → 400 invalid_request
// (count is a shape error, not an oversize payload).
func TestSendTooManyAttachments(t *testing.T) {
	srv := testServer(t)
	atts := make([]map[string]any, 0, maxAttachmentCount+1)
	for i := 0; i < maxAttachmentCount+1; i++ {
		atts = append(atts, map[string]any{
			"filename": "f.txt", "content_type": "text/plain", "data": b64(8),
		})
	}
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
		"attachments": atts,
	})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request for too many attachments, got %d %v", code, body)
	}
}

// TestValidateAttachments covers the decoded-byte enforcement directly (fast,
// no multi-tens-of-MB HTTP body): per-attachment cap, combined-total cap, count
// cap, base64 validity, and the happy path.
func TestValidateAttachments(t *testing.T) {
	half := maxAttachmentBytes / 2
	cases := []struct {
		name string
		atts []outbound.Attachment
		want string // "" = accept; otherwise the expected error code
	}{
		{"empty", nil, ""},
		{"ok", []outbound.Attachment{{Filename: "a", Data: b64(1024)}}, ""},
		{"filename-at-limit", []outbound.Attachment{{Filename: strings.Repeat("x", outbound.MaxAttachmentFilenameBytes), Data: b64(4)}}, ""},
		{"filename-over", []outbound.Attachment{{Filename: strings.Repeat("x", outbound.MaxAttachmentFilenameBytes+1), Data: b64(4)}}, "invalid_attachment"},
		{"per-attachment-over", []outbound.Attachment{{Filename: "a", Data: b64(maxAttachmentBytes + 1)}}, "payload_too_large"},
		{"per-attachment-at-limit", []outbound.Attachment{{Filename: "a", Data: b64(maxAttachmentBytes)}}, ""},
		{"total-over", []outbound.Attachment{
			{Filename: "a", Data: b64(half)}, {Filename: "b", Data: b64(half)},
			{Filename: "c", Data: b64(half)}, {Filename: "d", Data: b64(half)},
			{Filename: "e", Data: b64(half)}, {Filename: "f", Data: b64(half + 1)},
		}, "payload_too_large"},
		{"bad-base64", []outbound.Attachment{{Filename: "a", Data: "!!!not-base64!!!"}}, "invalid_attachment"},
		// Header injection through an attachment MIME header. The composer has
		// always refused these; the point of checking here is the STATUS. On
		// staging sha-32ce45dc they escaped as 500 internal_error
		// (req_e7be6c6d4b2ed074056b2717 CR, req_5221059f93a505a8755e18f6 LF,
		// req_d80a0151080139d2ac01d320 content_type) — a permanent client error
		// that SDK retry logic treats as transient and hammers.
		{"filename-cr", []outbound.Attachment{{Filename: "a\rb.txt", Data: b64(4)}}, "invalid_attachment"},
		{"filename-lf", []outbound.Attachment{{Filename: "a\nb.txt", Data: b64(4)}}, "invalid_attachment"},
		{"filename-crlf", []outbound.Attachment{{Filename: "ok.txt\r\nContent-Type: text/html", Data: b64(4)}}, "invalid_attachment"},
		{"content-type-crlf", []outbound.Attachment{{Filename: "a.txt", ContentType: "text/plain\r\nX-I: 1", Data: b64(4)}}, "invalid_attachment"},
		{"content-type-clean", []outbound.Attachment{{Filename: "a.txt", ContentType: "text/plain", Data: b64(4)}}, ""},
		{"too-many", func() []outbound.Attachment {
			a := make([]outbound.Attachment, maxAttachmentCount+1)
			for i := range a {
				a[i] = outbound.Attachment{Filename: "x", Data: b64(4)}
			}
			return a
		}(), "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validateAttachments(tc.atts)
			if tc.want == "" {
				if env != nil {
					t.Fatalf("want accept, got %d %s", env.status, env.Code())
				}
				return
			}
			if env == nil || env.Code() != tc.want {
				t.Fatalf("want error code %q, got %v", tc.want, env)
			}
		})
	}
}

// TestSendAttachmentHeaderInjectionIsA400 is the wire-level half of the CR/LF
// cases above: the caller must see 400 invalid_attachment, the same answer the
// filename length cap already gives, rather than the 500 that staging returned.
func TestSendAttachmentHeaderInjectionIsA400(t *testing.T) {
	cases := map[string]map[string]any{
		"filename CR":       {"filename": "a\rb.txt", "content_type": "text/plain", "data": b64(4)},
		"filename LF":       {"filename": "a\nb.txt", "content_type": "text/plain", "data": b64(4)},
		"content_type CRLF": {"filename": "a.txt", "content_type": "text/plain\r\nX-Injected: 1", "data": b64(4)},
	}
	for name, att := range cases {
		t.Run(name, func(t *testing.T) {
			srv := testServer(t)
			code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
				"to": []string{"alice@x.com"}, "subject": "att", "text": "t",
				"attachments": attField(att),
			})
			if code != 400 || errCode(body) != "invalid_attachment" {
				t.Fatalf("got %d %v; want 400 invalid_attachment", code, body)
			}
		})
	}
}

func TestSendUnauthorized(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+sendURL, "", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "Hi", "text": "hello",
	})
	if code != 401 {
		t.Fatalf("want 401, got %d", code)
	}
}

func TestReplySent(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply", "good", map[string]any{"text": "thanks"})
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent, got %d %v", code, body)
	}
}

// TestReplyReplyToPropagates / TestForwardReplyToPropagates: reply and forward
// each build their own outbound.SendRequest literal, so a dropped ReplyTo mapping
// there wouldn't be caught by the send-path test. Assert the override reaches the
// delivery layer on both.
func TestReplyReplyToPropagates(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply", "good",
		map[string]any{"text": "thanks", "reply_to": "Support <support@acme.com>"})
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	if got := lastDeliveredReq().ReplyTo; got != "Support <support@acme.com>" {
		t.Fatalf("reply delivered ReplyTo = %q, want override", got)
	}
}

func TestReplyInvalidReplyTo(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply", "good",
		map[string]any{"text": "thanks", "reply_to": "not an address"})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request, got %d %v", code, body)
	}
}

func TestForwardReplyToPropagates(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/forward", "good",
		map[string]any{"to": []string{"newperson@x.com"}, "text": "fyi", "reply_to": "Support <support@acme.com>"})
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	if got := lastDeliveredReq().ReplyTo; got != "Support <support@acme.com>" {
		t.Fatalf("forward delivered ReplyTo = %q, want override", got)
	}
}

func TestForwardInvalidReplyTo(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/forward", "good",
		map[string]any{"to": []string{"newperson@x.com"}, "text": "fyi", "reply_to": "a@x.com, b@x.com"})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request for multi reply_to, got %d %v", code, body)
	}
}

// TestReplyAllRespectsRecipientCap: reply_all expands the thread's recipients
// into the outbound set, so the cap must be enforced on the FINAL set — not just
// the caller-supplied cc/bcc (which is empty here).
func TestReplyAllRespectsRecipientCap(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_bigthread/reply", "good",
		map[string]any{"text": "thanks", "reply_all": true})
	if code != 400 || errCode(body) != "too_many_recipients" {
		t.Fatalf("want 400 too_many_recipients for a reply_all over the cap, got %d %v", code, body)
	}
}

// TestReplyAllUnderCapSends: a normal reply_all under the cap still sends.
func TestReplyAllUnderCapSends(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply", "good",
		map[string]any{"text": "thanks", "reply_all": true})
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent for a small reply_all, got %d %v", code, body)
	}
}

func TestReplyBodyRequired(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply", "good", map[string]any{"text": ""})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request, got %d %v", code, body)
	}
}

func TestReplyMessageNotFound(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_missing/reply", "good", map[string]any{"text": "x"})
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}

func TestReplyNotOwnedAgent(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/other%40acme.com/messages/msg_in1/reply", "good", map[string]any{"text": "x"})
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}

func TestForwardSent(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/forward", "good", map[string]any{
		"to": []string{"bob@x.com"}, "text": "fyi",
	})
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent, got %d %v", code, body)
	}
}

func TestForwardNoRecipients(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/forward", "good", map[string]any{"text": "fyi"})
	// `to` is now schema-required (MSG-3) → rejected at validation (422).
	if code != 422 {
		t.Fatalf("want 422 missing to, got %d %v", code, body)
	}
}

func TestForwardMessageNotFound(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_missing/forward", "good", map[string]any{"to": []string{"bob@x.com"}, "text": "x"})
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}

// TestTestSendAccepted: the test send is queue-first — the immediate response
// is 202 status=accepted carrying the durable e2a msg_ id and NO provider id
// (that arrives later via GET /v1/messages/{id} and email.sent/email.failed).
func TestTestSendAccepted(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/test", "good", nil)
	if code != 202 || body["status"] != "accepted" || body["message_id"] != "msg_test_1" {
		t.Fatalf("want 202 accepted msg_test_1, got %d %v", code, body)
	}
	if _, ok := body["provider_message_id"]; ok {
		t.Fatalf("provider_message_id must be absent before worker submission, got %v", body)
	}
}

// TestSendResultViewMessageIDContract pins the SendResultView promise on every
// outbound outcome shape: message_id is ALWAYS the GET-able e2a msg_ resource
// id and never the upstream provider id (which belongs exclusively in
// provider_message_id).
func TestSendResultViewMessageIDContract(t *testing.T) {
	providerID := "<0100019-abc-def@email.amazonses.com>"
	cases := []struct {
		name string
		res  *agent.OutboundResult
	}{
		{"accepted", &agent.OutboundResult{MessageID: "msg_a1", Status: "accepted", Method: "smtp"}},
		{"sent", &agent.OutboundResult{MessageID: "msg_a2", ProviderMessageID: providerID, SentAs: "relay", Method: "smtp"}},
		{"held", &agent.OutboundResult{Held: true, PendingMessageID: "msg_a3"}},
		{"loopback", &agent.OutboundResult{MessageID: "msg_a4", SentAs: "own_address", Method: "loopback"}},
	}
	for _, tc := range cases {
		_, view := outboundResultView(tc.res)
		if !strings.HasPrefix(view.MessageID, "msg_") {
			t.Errorf("%s: message_id = %q, want an e2a msg_ id", tc.name, view.MessageID)
		}
		if view.MessageID == providerID || strings.HasPrefix(view.MessageID, "<") {
			t.Errorf("%s: message_id = %q leaks a provider id", tc.name, view.MessageID)
		}
		if view.ProviderMessageID != "" && !strings.HasPrefix(view.MessageID, "msg_") {
			t.Errorf("%s: provider id present but message_id %q is not a msg_ resource", tc.name, view.MessageID)
		}
	}
}

func TestTestSendNotOwned(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/other%40acme.com/test", "good", nil)
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}
