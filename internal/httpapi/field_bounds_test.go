package httpapi

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
)

// GA contract — the remaining free-form request fields are bounded at the
// request edge (names 200, reject reason 2000, conversation_id 200, email
// addresses 320). All bounds are declared as maxLength struct tags, which
// Huma validates in Unicode code points (utf8.RuneCountInString — see
// huma/v2 validate.go), matching the JSON-Schema maxLength semantics of the
// emitted OpenAPI document. The hand-written runtime backstops
// (identity.ValidateAgentName, agent.ValidateRecipients, validateReplyTo,
// validateConversationID) count the SAME way, so spec and runtime agree.
// The multi-byte boundary tests below prove that agreement end-to-end: a
// CJK string AT the limit is n runes but 3n bytes — it passes only if every
// layer counts runes.

// cjk returns a string of exactly n Unicode code points, each 3 bytes in
// UTF-8 — so byte-counting validators disagree with the spec by 3x.
func cjk(n int) string { return strings.Repeat("日", n) }

// --- struct-tag / const drift guard (mirrors TestOutboundFieldLimitTagsMatchConsts) ---

func TestGABoundTagsMatchConsts(t *testing.T) {
	addrTag := fmt.Sprintf("%d", agent.MaxAddressLen)
	nameTag := fmt.Sprintf("%d", identity.MaxAgentNameLen)
	convTag := fmt.Sprintf("%d", maxConversationIDLen)
	type want struct {
		field    string
		expected string
	}
	cases := []struct {
		name string
		typ  any
		want []want
	}{
		{"CreateAgentRequest", CreateAgentRequest{}, []want{
			{"Email", addrTag},
			{"Name", nameTag},
		}},
		{"UpdateAgentRequest", UpdateAgentRequest{}, []want{
			{"Name", nameTag},
		}},
		{"CreateAPIKeyRequest", CreateAPIKeyRequest{}, []want{
			{"Name", fmt.Sprintf("%d", maxAPIKeyNameLen)},
		}},
		{"RejectRequest", RejectRequest{}, []want{
			{"Reason", fmt.Sprintf("%d", maxRejectReasonLen)},
		}},
		{"SendEmailRequest", SendEmailRequest{}, []want{
			{"To", addrTag}, {"CC", addrTag}, {"BCC", addrTag},
			{"ReplyTo", addrTag}, {"ConversationID", convTag},
		}},
		{"ReplyRequest", ReplyRequest{}, []want{
			{"CC", addrTag}, {"BCC", addrTag},
			{"ReplyTo", addrTag}, {"ConversationID", convTag},
		}},
		{"ForwardRequest", ForwardRequest{}, []want{
			{"To", addrTag}, {"CC", addrTag}, {"BCC", addrTag},
			{"ReplyTo", addrTag}, {"ConversationID", convTag},
		}},
		{"ApproveOverrides", agent.ApproveOverrides{}, []want{
			{"To", addrTag}, {"CC", addrTag}, {"BCC", addrTag},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := reflect.TypeOf(c.typ)
			for _, w := range c.want {
				f, ok := rt.FieldByName(w.field)
				if !ok {
					t.Fatalf("%s.%s: field not found", c.name, w.field)
				}
				if got := f.Tag.Get("maxLength"); got != w.expected {
					t.Errorf("%s.%s maxLength tag = %q, want %q", c.name, w.field, got, w.expected)
				}
			}
		})
	}
	// The httpapi alias and the webhook/list filter caps must stay equal to the
	// canonical constants — the conversation_id descriptions promise it.
	if maxEmailAddressLen != agent.MaxAddressLen {
		t.Errorf("maxEmailAddressLen = %d, want agent.MaxAddressLen = %d", maxEmailAddressLen, agent.MaxAddressLen)
	}
	if maxConversationIDLen != webhookMaxFilterValueLen {
		t.Errorf("maxConversationIDLen = %d must equal webhookMaxFilterValueLen (%d) so every conversation_id stays webhook-filterable",
			maxConversationIDLen, webhookMaxFilterValueLen)
	}
	if maxConversationIDLen != maxFilterStr {
		t.Errorf("maxConversationIDLen = %d must equal maxFilterStr (%d) so every conversation_id stays list-filterable",
			maxConversationIDLen, maxFilterStr)
	}
}

// --- multi-byte (CJK) boundary tests: spec semantics == runtime semantics ---

// Agent display name: 200 code points (600 bytes) is accepted on create; 201 is
// a schema 422. If any layer counted bytes, the at-limit case would fail.
func TestCreateAgentNameUnicodeBoundary(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "bot@acme.com", "name": cjk(identity.MaxAgentNameLen),
	})
	if code != 201 {
		t.Fatalf("name at limit (200 code points, 600 bytes): want 201, got %d %v", code, body)
	}
	code, body = postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "bot@acme.com", "name": cjk(identity.MaxAgentNameLen + 1),
	})
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("name over limit: want 422 invalid_request, got %d %v", code, body)
	}
}

// Agent name on update (PATCH) shares the same constant.
func TestUpdateAgentNameUnicodeBoundary(t *testing.T) {
	srv := testServer(t)
	code, body := sendJSON(t, "PATCH", srv.URL+"/v1/agents/support%40acme.com", "good", map[string]any{
		"name": cjk(identity.MaxAgentNameLen),
	})
	if code != 200 {
		t.Fatalf("name at limit: want 200, got %d %v", code, body)
	}
	code, body = sendJSON(t, "PATCH", srv.URL+"/v1/agents/support%40acme.com", "good", map[string]any{
		"name": cjk(identity.MaxAgentNameLen + 1),
	})
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("name over limit: want 422 invalid_request, got %d %v", code, body)
	}
}

// Agent email: 320 code points total is accepted; 321 is a schema 422. The
// at-limit email is multi-byte in the local part, so byte-counting would
// reject it.
func TestCreateAgentEmailUnicodeBoundary(t *testing.T) {
	srv := testServer(t)
	domainPart := "@acme.com" // 9 code points
	atLimit := cjk(agent.MaxAddressLen-len(domainPart)) + domainPart
	if utf8.RuneCountInString(atLimit) != agent.MaxAddressLen {
		t.Fatalf("fixture bug: at-limit email is %d code points", utf8.RuneCountInString(atLimit))
	}
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{"email": atLimit})
	if code != 201 {
		t.Fatalf("email at limit (320 code points): want 201, got %d %v", code, body)
	}
	code, body = postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{"email": cjk(1) + atLimit})
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("email over limit: want 422 invalid_request, got %d %v", code, body)
	}
}

// API-key name: 200 code points accepted, 201 rejected at the schema layer.
func TestCreateAPIKeyNameUnicodeBoundary(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/account/api-keys", "good", map[string]any{
		"name": cjk(maxAPIKeyNameLen),
	})
	if code != 201 {
		t.Fatalf("api-key name at limit: want 201, got %d %v", code, body)
	}
	code, body = postJSON(t, srv.URL+"/v1/account/api-keys", "good", map[string]any{
		"name": cjk(maxAPIKeyNameLen + 1),
	})
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("api-key name over limit: want 422 invalid_request, got %d %v", code, body)
	}
}

// Reject reason: 2000 code points accepted, 2001 rejected at the schema layer.
func TestRejectReasonUnicodeBoundary(t *testing.T) {
	srv := testServer(t)
	reason := cjk(maxRejectReasonLen)
	code, body := postJSON(t, srv.URL+"/v1/reviews/msg_pending/reject", "good", map[string]any{
		"reason": reason,
	})
	if code != 200 || body["rejection_reason"] != reason {
		t.Fatalf("reason at limit: want 200 with reason echoed, got %d", code)
	}
	code, body = postJSON(t, srv.URL+"/v1/reviews/msg_pending/reject", "good", map[string]any{
		"reason": cjk(maxRejectReasonLen + 1),
	})
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("reason over limit: want 422 invalid_request, got %d %v", code, body)
	}
}

// conversation_id: 200 code points pass BOTH the schema tag and the runtime
// validateConversationID backstop (the send path runs both); 201 is a schema
// 422. If the runtime counted bytes, the at-limit case would 400.
func TestSendConversationIDUnicodeBoundary(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"r@x.com"}, "subject": "Hi", "text": "hello",
		"conversation_id": cjk(maxConversationIDLen),
	})
	if code != 200 {
		t.Fatalf("conversation_id at limit: want 200, got %d %v", code, body)
	}
	code, body = postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"r@x.com"}, "subject": "Hi", "text": "hello",
		"conversation_id": cjk(maxConversationIDLen + 1),
	})
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("conversation_id over limit: want 422 invalid_request, got %d %v", code, body)
	}
}

// A conversation_id at the request cap is usable as a message-list filter —
// the "every accepted conversation_id stays filterable" invariant (the filter
// counts runes too, not bytes).
func TestListMessagesConversationFilterAcceptsMaxLenUnicode(t *testing.T) {
	srv := testServer(t)
	code, body := getJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages?conversation_id="+
		strings.Repeat("%E6%97%A5", maxConversationIDLen), "good")
	if code != 200 {
		t.Fatalf("list filter with 200-code-point conversation_id: want 200, got %d %v", code, body)
	}
}

// A conversation_id at the request cap is fetchable via GET
// /conversations/{id} — the byte-counting pre-check this replaced would have
// 400'd a schema-legal 200-code-point non-ASCII id. At-limit reaches the
// store lookup (404 from the fixture, NOT 400); over-limit is a 400.
func TestGetConversationAcceptsMaxLenUnicodeID(t *testing.T) {
	srv := testServer(t)
	enc := strings.Repeat("%E6%97%A5", maxConversationIDLen) // 200 × 日
	code, body := getJSON(t, srv.URL+"/v1/agents/support%40acme.com/conversations/"+enc, "good")
	if code != 404 {
		t.Fatalf("GET conversation with 200-code-point id: want 404 (past validation, unknown id), got %d %v", code, body)
	}
	code, body = getJSON(t, srv.URL+"/v1/agents/support%40acme.com/conversations/"+enc+"%E6%97%A5", "good")
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("GET conversation with 201-code-point id: want 400 invalid_request, got %d %v", code, body)
	}
}

// reply_to: an address with a multi-byte display name at exactly 320 code
// points passes the schema tag, the runtime rune count, and the address
// parser; 321 is a schema 422.
func TestSendReplyToUnicodeBoundary(t *testing.T) {
	srv := testServer(t)
	// "<cjk...>" <r@x.com> — 2 quotes + 1 space + len("<r@x.com>")==9 → 12
	// code points of scaffolding around the display name.
	display := agent.MaxAddressLen - 12
	atLimit := `"` + cjk(display) + `" <r@x.com>`
	if utf8.RuneCountInString(atLimit) != agent.MaxAddressLen {
		t.Fatalf("fixture bug: at-limit reply_to is %d code points", utf8.RuneCountInString(atLimit))
	}
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"r@x.com"}, "subject": "Hi", "text": "hello", "reply_to": atLimit,
	})
	if code != 200 {
		t.Fatalf("reply_to at limit (320 code points): want 200, got %d %v", code, body)
	}
	over := `"` + cjk(display+1) + `" <r@x.com>`
	code, body = postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"r@x.com"}, "subject": "Hi", "text": "hello", "reply_to": over,
	})
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("reply_to over limit: want 422 invalid_request, got %d %v", code, body)
	}
}

// Recipient array items: an item with a multi-byte display name at exactly
// 320 code points passes the schema item bound AND the runtime
// agent.ValidateRecipients rune count; 321 is a schema 422 (Huma applies the
// maxLength tag to each array item).
func TestSendRecipientItemUnicodeBoundary(t *testing.T) {
	srv := testServer(t)
	display := agent.MaxAddressLen - 12
	atLimit := `"` + cjk(display) + `" <r@x.com>`
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{atLimit}, "subject": "Hi", "text": "hello",
	})
	if code != 200 {
		t.Fatalf("recipient at limit (320 code points): want 200, got %d %v", code, body)
	}
	over := `"` + cjk(display+1) + `" <r@x.com>`
	code, body = postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{over}, "subject": "Hi", "text": "hello",
	})
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("recipient over limit: want 422 invalid_request, got %d %v", code, body)
	}
}

// Approve-override recipients carry the same per-item bound (the reviewer
// edit path must not be a bypass).
func TestApproveOverrideRecipientUnicodeBoundary(t *testing.T) {
	srv := testServer(t)
	display := agent.MaxAddressLen - 12
	atLimit := `"` + cjk(display) + `" <a@x.com>`
	code, body := postJSON(t, srv.URL+"/v1/reviews/msg_pending/approve", "good",
		map[string]any{"to": []string{atLimit}})
	if code != 200 {
		t.Fatalf("override recipient at limit: want 200, got %d %v", code, body)
	}
	over := `"` + cjk(display+1) + `" <a@x.com>`
	code, body = postJSON(t, srv.URL+"/v1/reviews/msg_pending/approve", "good",
		map[string]any{"to": []string{over}})
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("override recipient over limit: want 422 invalid_request, got %d %v", code, body)
	}
}

// --- runtime backstops agree with the schema (direct unit checks) ---

// agent.ValidateRecipients counts runes, not bytes: 320 code points (~950
// bytes) passes, 321 fails — byte-counting would reject the first case.
func TestValidateRecipientsRuneSemantics(t *testing.T) {
	display := agent.MaxAddressLen - 12
	atLimit := `"` + cjk(display) + `" <r@x.com>`
	if err := agent.ValidateRecipients([]string{atLimit}); err != nil {
		t.Fatalf("recipient at 320 code points must pass the runtime check, got %v", err)
	}
	over := `"` + cjk(display+1) + `" <r@x.com>`
	if err := agent.ValidateRecipients([]string{over}); err == nil {
		t.Fatal("recipient at 321 code points must fail the runtime check")
	}
}

// validateReplyTo and validateConversationID (runtime backstops for paths the
// schema doesn't cover) count runes too.
func TestReplyToAndConversationIDBackstopRuneSemantics(t *testing.T) {
	display := maxEmailAddressLen - 12
	if env := validateReplyTo(`"` + cjk(display) + `" <r@x.com>`); env != nil {
		t.Fatalf("reply_to at 320 code points must pass, got %v", env)
	}
	if env := validateReplyTo(`"` + cjk(display+1) + `" <r@x.com>`); env == nil {
		t.Fatal("reply_to at 321 code points must fail")
	}
	if env := validateReplyTo(strings.Repeat("😀", 250) + "@b.test"); env == nil {
		t.Fatal("reply_to with an oversized UTF-8 addr-spec must fail")
	}
	if err := validateConversationID(cjk(maxConversationIDLen)); err != nil {
		t.Fatalf("conversation_id at 200 code points must pass, got %v", err)
	}
	if err := validateConversationID(cjk(maxConversationIDLen + 1)); err == nil {
		t.Fatal("conversation_id at 201 code points must fail")
	}
}
