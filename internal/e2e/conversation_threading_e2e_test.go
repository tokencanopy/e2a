//go:build integration

package e2e_test

import (
	"context"
	"encoding/json"
	"net/smtp"
	"net/url"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

type threadSendResp struct {
	Status            string `json:"status"`
	MessageID         string `json:"message_id"`
	ProviderMessageID string `json:"provider_message_id"`
}

func parseThreadSend(t *testing.T, body []byte) threadSendResp {
	t.Helper()
	var r threadSendResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("parse send response %q: %v", body, err)
	}
	if r.Status != "sent" {
		t.Fatalf("send not sent: status=%q body=%s", r.Status, body)
	}
	return r
}

func convOf(t *testing.T, ts *testutil.E2ATestServer, agentID, msgID string) string {
	t.Helper()
	return messageOf(t, ts, agentID, msgID).ConversationID
}

func messageOf(t *testing.T, ts *testutil.E2ATestServer, agentID, msgID string) *identity.Message {
	t.Helper()
	m, err := ts.Store.GetMessageWithContent(context.Background(), msgID, agentID)
	if err != nil || m == nil {
		t.Fatalf("GetMessageWithContent(%s): %v", msgID, err)
	}
	return m
}

func latestInbound(t *testing.T, ts *testutil.E2ATestServer, agentID string) identity.Message {
	t.Helper()
	msgs, err := ts.Store.GetMessagesByAgent(context.Background(),
		identity.MessageListFilter{AgentID: agentID, Direction: "inbound", Limit: 20})
	if err != nil {
		t.Fatalf("GetMessagesByAgent: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatalf("no inbound messages for %s", agentID)
	}
	return msgs[0] // newest-first
}

func subResource(base, agentEmail, msgID, action string) string {
	return base + "/v1/agents/" + url.PathEscape(agentEmail) + "/messages/" + msgID + "/" + action + "?wait=sent"
}

// TestConversationThreadingE2E_OutboundRooted exercises application
// conversation correlation and mailbox-local email topology independently,
// through the real API + relay:
//  1. an agent send that omits conversation_id is minted both identities;
//  2. an external reply referencing that send (In-Reply-To the
//     provider_message_id the recipient replies to) joins its email thread;
//  3. the agent's own reply, with no conversation_id, inherits both identities
//     for their separate purposes.
func TestConversationThreadingE2E_OutboundRooted(t *testing.T) {
	pool := testutil.TestDB(t)
	fakeSMTP, _ := testutil.FakeSMTPServer(t)
	ts := testutil.TestServer(t, pool,
		testutil.WithOutboundSMTP(fakeSMTP.Host, fakeSMTP.Port, "test.e2a.dev"),
		testutil.WithOutboundSMTPMessageIDDomain("test.e2a.dev"))
	_, key, agent := setupDomainAndAgent(t, ts, "agent@thr1.example.com", "thr1.example.com", "", "")

	// (1) Agent sends first, NO conversation_id.
	status, body := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey,
		`{"to":["alice@gmail.com"],"subject":"Project kickoff","text":"Let's start."}`)
	if status != 200 {
		t.Fatalf("send status=%d body=%s", status, body)
	}
	out := parseThreadSend(t, body)

	outMessage := messageOf(t, ts, agent.ID, out.MessageID)
	c1 := outMessage.ConversationID
	if !strings.HasPrefix(c1, "conv_") {
		t.Fatalf("outbound conversation_id = %q, want an auto-generated conv_ anchor (#328 fix A)", c1)
	}
	if !identity.IsValidThreadID(outMessage.ThreadID) || outMessage.ThreadParentID != "" {
		t.Fatalf("outbound email topology = thread %q parent %q", outMessage.ThreadID, outMessage.ThreadParentID)
	}
	if out.ProviderMessageID == "" {
		t.Fatalf("send response missing provider_message_id (the reply anchor)")
	}
	if outMessage.RFCMessageIDKey == "" {
		t.Fatalf("qualified provider_message_id %q did not populate rfc_message_id_key", out.ProviderMessageID)
	}

	// (2) Alice replies, referencing the agent's message. The relay must recover
	// the agent's conversation_id via the In-Reply-To lookup.
	reply := "From: alice@gmail.com\r\nTo: agent@thr1.example.com\r\n" +
		"Subject: Re: Project kickoff\r\nMessage-ID: <alice-1@gmail.com>\r\n" +
		"In-Reply-To: " + out.ProviderMessageID + "\r\n" +
		"References: " + out.ProviderMessageID + "\r\n\r\nSounds good."
	if err := smtp.SendMail(ts.SMTPAddr, nil, "alice@gmail.com", []string{"agent@thr1.example.com"}, []byte(reply)); err != nil {
		t.Fatalf("SendMail reply: %v", err)
	}
	tick(t, ts)

	in := latestInbound(t, ts, agent.ID)
	if in.ConversationID != c1 {
		t.Fatalf("inbound reply conversation_id = %q, want %q (must thread onto the agent's anchor)", in.ConversationID, c1)
	}
	inDetail := messageOf(t, ts, agent.ID, in.ID)
	if inDetail.ThreadID != outMessage.ThreadID || inDetail.ThreadParentID != outMessage.ID {
		t.Fatalf("inbound reply topology = thread %q parent %q, want %q / %q",
			inDetail.ThreadID, inDetail.ThreadParentID, outMessage.ThreadID, outMessage.ID)
	}

	// (3) Agent replies to Alice with NO conversation_id — application
	// correlation inherits as before, and email topology follows the resource.
	status, body = authedJSON(t, "POST", subResource(ts.HTTPServer.URL, agent.EmailAddress(), in.ID, "reply"),
		key.PlaintextKey, `{"text":"On it."}`)
	if status != 200 {
		t.Fatalf("reply status=%d body=%s", status, body)
	}
	rep := parseThreadSend(t, body)
	if c := convOf(t, ts, agent.ID, rep.MessageID); c != c1 {
		t.Fatalf("agent reply conversation_id = %q, want %q (reply must inherit the thread, #328 fix B)", c, c1)
	}
	replyMessage := messageOf(t, ts, agent.ID, rep.MessageID)
	if replyMessage.ThreadID != outMessage.ThreadID || replyMessage.ThreadParentID != in.ID {
		t.Fatalf("agent reply topology = thread %q parent %q, want %q / %q",
			replyMessage.ThreadID, replyMessage.ThreadParentID, outMessage.ThreadID, in.ID)
	}
}

// TestConversationThreadingE2E_ExplicitForwardFirstContact covers the rest of
// the precedence ladder and the unchanged first-contact rule:
//   - an explicit conversation_id is preserved verbatim;
//   - a first-contact inbound (no References) keeps conversation_id="" by design;
//   - a forward starts a NEW thread (it does not inherit the forwarded message).
func TestConversationThreadingE2E_ExplicitForwardFirstContact(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	_, key, agent := setupDomainAndAgent(t, ts, "agent@thr2.example.com", "thr2.example.com", "", "")

	// Explicit application-correlation id is preserved; email topology is
	// still a fresh server-owned root.
	status, body := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey,
		`{"to":["x@gmail.com"],"subject":"s","text":"b","conversation_id":"conv_explicit_xyz"}`)
	if status != 200 {
		t.Fatalf("explicit send status=%d %s", status, body)
	}
	explicit := messageOf(t, ts, agent.ID, parseThreadSend(t, body).MessageID)
	if c := explicit.ConversationID; c != "conv_explicit_xyz" {
		t.Fatalf("explicit conversation_id = %q, want conv_explicit_xyz (precedence 1)", c)
	}
	if !identity.IsValidThreadID(explicit.ThreadID) || explicit.ThreadParentID != "" {
		t.Fatalf("explicit send topology = thread %q parent %q", explicit.ThreadID, explicit.ThreadParentID)
	}

	// First-contact inbound (no References) → "".
	inbound := "From: bob@gmail.com\r\nTo: agent@thr2.example.com\r\nSubject: FYI\r\nMessage-ID: <bob-1@gmail.com>\r\n\r\nlook at this"
	if err := smtp.SendMail(ts.SMTPAddr, nil, "bob@gmail.com", []string{"agent@thr2.example.com"}, []byte(inbound)); err != nil {
		t.Fatalf("SendMail inbound: %v", err)
	}
	tick(t, ts)
	in := latestInbound(t, ts, agent.ID)
	if in.ConversationID != "" {
		t.Fatalf("first-contact inbound conversation_id = %q, want \"\" (design unchanged)", in.ConversationID)
	}

	// Forward starts a new email thread and keeps the established conversation
	// behavior (a newly minted application-correlation id).
	status, body = authedJSON(t, "POST", subResource(ts.HTTPServer.URL, agent.EmailAddress(), in.ID, "forward"),
		key.PlaintextKey, `{"to":["carol@gmail.com"],"text":"fyi"}`)
	if status != 200 {
		t.Fatalf("forward status=%d %s", status, body)
	}
	forward := messageOf(t, ts, agent.ID, parseThreadSend(t, body).MessageID)
	if c := forward.ConversationID; !strings.HasPrefix(c, "conv_") {
		t.Fatalf("forward conversation_id = %q, want a fresh conv_ anchor (a forward starts a new thread)", c)
	}
	if !identity.IsValidThreadID(forward.ThreadID) || forward.ThreadID == in.ThreadID || forward.ThreadParentID != "" {
		t.Fatalf("forward email topology = thread %q parent %q, source thread %q",
			forward.ThreadID, forward.ThreadParentID, in.ThreadID)
	}
}
