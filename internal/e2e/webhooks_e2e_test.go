//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// sendURL builds the /v1 send endpoint for an agent and requests the bounded
// terminal wait. The send still commits 202/accepted and runs through River;
// wait=sent only keeps terminal-state e2e assertions deterministic. A held
// message returns 202/pending_review immediately.
func sendURL(base, agentEmail string) string {
	return base + "/v1/agents/" + url.PathEscape(agentEmail) + "/messages?wait=sent"
}

// Tests in this file exercise the webhooks-as-a-resource path end-to-end.
// Distinct from e2e_test.go which covers the legacy
// agent_identities.webhook_url path (per-agent, single URL, no filters).
//
// Coverage map (from the approved test plan):
//   P1 — outbound events: email.sent, email.review_requested,
//         email.approved, email.rejected fire from real handler triggers
//   P2 — inbound:         email.received fires from a real SMTP message
//   P3 — filters:         agent_ids + conversation_ids + labels (H5)
//   P4 — signing:         rotation grace dual-sig + disabled webhook silent
//   P5 — auto-disable:    10 failed events flip enabled=false
//
// Two design decisions worth re-stating:
//   1) Webhooks are provisioned via Store.CreateWebhook directly (NOT
//      POST /v1/webhooks). The public API rejects 127.0.0.1 URLs
//      (SSRF guard); the only way to test the worker hitting a local
//      receiver is to bypass the handler-side validator. The handler
//      itself has unit-test coverage at internal/agent/webhooks_api_test.go.
//   2) Worker timing: we call ts.DrainAndDeliver(ctx) directly (outbox drain +
//      River DeliverWorker) instead of waiting on any production interval. That
//      keeps the suite deterministic and under 60s.

// authedJSON wraps an authenticated JSON request and returns the
// response status + body. Tests assert against both.
func authedJSON(t *testing.T, method, url, apiKey, body string) (int, []byte) {
	t.Helper()
	var rdr *bytes.Buffer
	if body != "" {
		rdr = bytes.NewBufferString(body)
	} else {
		rdr = bytes.NewBuffer(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// verifyHMACv1 returns true iff at least one v1= entry in the
// X-E2A-Signature header verifies against secret using the standard
// "<t>.<rawBody>" payload. Mirrors the SDK helpers' behavior so a
// test failure points at the wire protocol, not the verifier.
func verifyHMACv1(t *testing.T, headers http.Header, rawBody []byte, secret string) bool {
	t.Helper()
	sig := headers.Get("X-E2A-Signature")
	if sig == "" {
		return false
	}
	var ts string
	var v1s []string
	for _, part := range strings.Split(sig, ",") {
		p := strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(p, "t="):
			ts = strings.TrimPrefix(p, "t=")
		case strings.HasPrefix(p, "v1="):
			v1s = append(v1s, strings.TrimPrefix(p, "v1="))
		}
	}
	if ts == "" || len(v1s) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(rawBody)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, v := range v1s {
		if v == want {
			return true
		}
	}
	return false
}

// setupSubscriberOwner provisions a user + API key + a verified domain
// + an agent on that domain. Returns the agent so the test can
// register webhooks scoped to it. mode is "local" or "cloud"; HITL
// flag toggles HITL on the agent.
func setupSubscriberOwner(t *testing.T, ts *testutil.E2ATestServer, prefix string, hitl bool) (*identity.User, *identity.APIKey, *identity.AgentIdentity) {
	t.Helper()
	ctx := context.Background()
	user, err := ts.Store.CreateOrGetUser(ctx, "owner-"+prefix+"@example.com", "Owner", "google-"+prefix)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	key, err := ts.Store.CreateAPIKey(ctx, user.ID, prefix+"-key", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	domain := prefix + ".example.com"
	if _, err := ts.Store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := ts.Store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	email := "bot@" + domain
	agent, err := ts.Store.CreateAgent(ctx, email, domain, "Bot", "", "local", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if hitl {
		// Hold every outbound send for human review. Post-HITL-retirement (migrations
		// 042/043) holds come from the outbound policy gate, not a hitl_enabled flag:
		// outbound_policy=allowlist with an empty allowlist flags every recipient, and
		// outbound_policy_action=review turns that flag into a pending_review hold.
		if err := ts.Store.UpdateAgentScanConfig(ctx, agent.ID, user.ID, identity.ScanConfig{
			InboundPolicyAction:         "flag",
			OutboundPolicy:              "allowlist",
			OutboundAllowlist:           []string{},
			OutboundPolicyAction:        "review",
			InboundScan:                 "off",
			InboundScanReviewThreshold:  0.5,
			InboundScanBlockThreshold:   0.9,
			OutboundScan:                "off",
			OutboundScanReviewThreshold: 0.5,
			OutboundScanBlockThreshold:  0.9,
		}); err != nil {
			t.Fatalf("UpdateAgentScanConfig: %v", err)
		}
		// TTL + expiration action for the sweep.
		if err := ts.Store.UpdateAgentHITL(ctx, agent.ID, user.ID, 3600, "reject"); err != nil {
			t.Fatalf("UpdateAgentHITL: %v", err)
		}
		// Reload so the in-test agent struct reflects the held-outbound config.
		agent, err = ts.Store.GetAgentByID(ctx, agent.ID)
		if err != nil {
			t.Fatalf("GetAgentByID: %v", err)
		}
	}
	return user, key, agent
}

// registerWebhook is the cheap path around the public-API URL
// validator. The validator rejects 127.0.0.1 (SSRF guard); the
// storage layer accepts any URL. The handler's validation has its
// own unit test coverage.
func registerWebhook(t *testing.T, ts *testutil.E2ATestServer, userID, url string, events []string, filters identity.WebhookFilters) *identity.Webhook {
	t.Helper()
	wh, err := ts.Store.CreateWebhook(context.Background(), userID, url, "e2e", events, filters)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	return wh
}

// tick drains the outbox (webhook_events → subscriber_deliveries) and runs the
// River DeliverWorker over every pending row, synchronously. Tests call this
// between trigger and assertion so they don't wait on any production tick.
func tick(t *testing.T, ts *testutil.E2ATestServer) {
	t.Helper()
	ts.DrainAndDeliver(context.Background())
}

// ----------------------------------------------------------------------
// P1 — Outbound events
// ----------------------------------------------------------------------

func TestWebhooksE2E_EmailSent(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	receiver := testutil.SubscriberReceiver(t)

	_, key, agent := setupSubscriberOwner(t, ts, "wh-sent", false)
	wh := registerWebhook(t, ts, agent.UserID, receiver.Server.URL+"/sent",
		[]string{"email.sent"}, identity.WebhookFilters{})

	// Trigger /send through the real HTTP API so the event fires
	// from the actual handler, not a hand-crafted publisher call.
	body := `{"to":["alice@example.com"],"subject":"hi","text":"hello"}`
	status, _ := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey, body)
	if status != 200 {
		t.Fatalf("send status=%d", status)
	}

	tick(t, ts)
	got := receiver.WaitFor(t, 2*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 1 })
	if len(got) != 1 {
		t.Fatalf("got %d captures, want 1", len(got))
	}
	c := got[0]
	if c.URL != "/sent" {
		t.Errorf("path=%q want /sent", c.URL)
	}
	if c.Envelope["type"] != "email.sent" {
		t.Errorf("event=%v want email.sent", c.Envelope["type"])
	}
	if !verifyHMACv1(t, c.Headers, c.RawBody, wh.SigningSecret) {
		t.Errorf("HMAC v1 verification failed: sig=%q", c.Headers.Get("X-E2A-Signature"))
	}
}

func TestWebhooksE2E_HITL_PendingApproved(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	receiver := testutil.SubscriberReceiver(t)

	_, key, agent := setupSubscriberOwner(t, ts, "wh-hitl", true)
	pendingHook := registerWebhook(t, ts, agent.UserID, receiver.Server.URL+"/pending",
		[]string{"email.review_requested"}, identity.WebhookFilters{})
	approvedHook := registerWebhook(t, ts, agent.UserID, receiver.Server.URL+"/approved",
		[]string{"email.review_approved"}, identity.WebhookFilters{})

	// send → held for approval, returns 202 + message_id.
	body := `{"to":["alice@example.com"],"subject":"draft","text":"please review"}`
	status, respBytes := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey, body)
	if status != 202 {
		t.Fatalf("send status=%d body=%s want 202 (HITL hold)", status, string(respBytes))
	}
	var sendResp map[string]any
	json.Unmarshal(respBytes, &sendResp)
	msgID, _ := sendResp["message_id"].(string)
	if msgID == "" {
		t.Fatalf("no message_id in hold response: %s", string(respBytes))
	}

	// First tick — should deliver email.review_requested.
	tick(t, ts)
	receiver.WaitFor(t, 2*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 1 })
	got := receiver.Captured()
	if len(got) != 1 {
		t.Fatalf("after pending: got %d captures, want 1", len(got))
	}
	if got[0].URL != "/pending" || got[0].Envelope["type"] != "email.review_requested" {
		t.Errorf("first capture path=%q event=%v", got[0].URL, got[0].Envelope["type"])
	}
	if !verifyHMACv1(t, got[0].Headers, got[0].RawBody, pendingHook.SigningSecret) {
		t.Errorf("pending HMAC verification failed")
	}

	// Approve via the API.
	approveBody := `{}`
	// A review's id IS the held message's id, so the account-scoped review queue
	// resolves it directly (the deprecated agent-path approve endpoint was removed).
	approveStatus, approveResp := authedJSON(t, "POST",
		ts.HTTPServer.URL+"/v1/reviews/"+msgID+"/approve",
		key.PlaintextKey, approveBody)
	if approveStatus != 202 {
		t.Fatalf("approve status=%d body=%s", approveStatus, string(approveResp))
	}

	// Second tick — should deliver email.approved.
	tick(t, ts)
	receiver.WaitFor(t, 2*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 2 })
	got = receiver.Captured()
	if len(got) != 2 {
		t.Fatalf("after approve: got %d captures, want 2", len(got))
	}
	approved := got[1]
	if approved.URL != "/approved" || approved.Envelope["type"] != "email.review_approved" {
		t.Errorf("second capture path=%q event=%v", approved.URL, approved.Envelope["type"])
	}
	if !verifyHMACv1(t, approved.Headers, approved.RawBody, approvedHook.SigningSecret) {
		t.Errorf("approved HMAC verification failed")
	}
	// reviewed_by_user_id is an internal DB id and must NOT be exposed on the
	// event; agent_email + direction are the public routing fields.
	data, _ := approved.Envelope["data"].(map[string]any)
	if _, ok := data["reviewed_by_user_id"]; ok {
		t.Errorf("approved event must not expose reviewed_by_user_id: %v", data)
	}
	if data["direction"] != "outbound" {
		t.Errorf("approved event direction=%v want outbound", data["direction"])
	}
	if data["agent_email"] != agent.ID {
		t.Errorf("approved event agent_email=%v want %q", data["agent_email"], agent.ID)
	}
}

func TestWebhooksE2E_HITL_Rejected(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	receiver := testutil.SubscriberReceiver(t)

	_, key, agent := setupSubscriberOwner(t, ts, "wh-reject", true)
	rejectedHook := registerWebhook(t, ts, agent.UserID, receiver.Server.URL+"/rejected",
		[]string{"email.review_rejected"}, identity.WebhookFilters{})

	body := `{"to":["alice@example.com"],"subject":"nope","text":"please reject"}`
	status, respBytes := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey, body)
	if status != 202 {
		t.Fatalf("send status=%d", status)
	}
	var sendResp map[string]any
	json.Unmarshal(respBytes, &sendResp)
	msgID, _ := sendResp["message_id"].(string)

	// Drain pending_approval (which we don't care about here) before
	// rejecting, so the post-reject Captured() is the rejected event
	// alone.
	tick(t, ts)
	receiver.Reset()

	rejectStatus, rejectResp := authedJSON(t, "POST",
		ts.HTTPServer.URL+"/v1/reviews/"+msgID+"/reject",
		key.PlaintextKey, `{"reason":"off-policy"}`)
	if rejectStatus != 200 {
		t.Fatalf("reject status=%d body=%s", rejectStatus, string(rejectResp))
	}

	tick(t, ts)
	receiver.WaitFor(t, 2*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 1 })
	got := receiver.Captured()
	if len(got) != 1 {
		t.Fatalf("after reject: got %d captures, want 1", len(got))
	}
	if got[0].URL != "/rejected" || got[0].Envelope["type"] != "email.review_rejected" {
		t.Errorf("path=%q event=%v", got[0].URL, got[0].Envelope["type"])
	}
	if !verifyHMACv1(t, got[0].Headers, got[0].RawBody, rejectedHook.SigningSecret) {
		t.Errorf("rejected HMAC verification failed")
	}
	data, _ := got[0].Envelope["data"].(map[string]any)
	if data["reason"] != "off-policy" {
		t.Errorf("reason=%v want off-policy", data["reason"])
	}
}

// ----------------------------------------------------------------------
// P2 — Inbound (email.received via SMTP)
// ----------------------------------------------------------------------

func TestWebhooksE2E_EmailReceived(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	receiver := testutil.SubscriberReceiver(t)

	_, _, agent := setupSubscriberOwner(t, ts, "wh-recv", false)
	wh := registerWebhook(t, ts, agent.UserID, receiver.Server.URL+"/received",
		[]string{"email.received"}, identity.WebhookFilters{})

	// SMTP a message into the relay. The relay marks it unverified
	// in dev (no SPF/DKIM for example.com) but still persists +
	// publishes — the event fires regardless of verification status.
	msg := "From: alice@gmail.com\r\nTo: " + agent.EmailAddress() + "\r\nSubject: Hi\r\n\r\nhello from SMTP"
	if err := smtp.SendMail(ts.SMTPAddr, nil, "alice@gmail.com",
		[]string{agent.EmailAddress()}, []byte(msg)); err != nil {
		t.Fatalf("SendMail: %v", err)
	}

	tick(t, ts)
	got := receiver.WaitFor(t, 3*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 1 })
	if len(got) != 1 {
		t.Fatalf("got %d captures, want 1", len(got))
	}
	if got[0].URL != "/received" || got[0].Envelope["type"] != "email.received" {
		t.Errorf("path=%q event=%v", got[0].URL, got[0].Envelope["type"])
	}
	if !verifyHMACv1(t, got[0].Headers, got[0].RawBody, wh.SigningSecret) {
		t.Errorf("received HMAC verification failed")
	}
	data, _ := got[0].Envelope["data"].(map[string]any)
	if data["message_id"] == "" || !strings.HasPrefix(fmt.Sprint(data["message_id"]), "msg_") {
		t.Errorf("expected msg_ id in data.message_id, got %v", data["message_id"])
	}
	// email.received is a metadata-only notification: the delivered payload must
	// NOT carry the message body, only fetch keys + authentication evidence.
	if _, present := data["raw_message"]; present {
		t.Errorf("metadata-only email.received must not carry raw_message over the wire")
	}
	if data["delivered_to"] != agent.EmailAddress() {
		t.Errorf("delivered_to (fetch key) = %v, want %s", data["delivered_to"], agent.EmailAddress())
	}
	if _, ok := data["authentication"].(map[string]any); !ok {
		t.Errorf("expected structured data.authentication evidence")
	}
}

// ----------------------------------------------------------------------
// P3 — Filter matching
// ----------------------------------------------------------------------

func TestWebhooksE2E_FilterByAgentID(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	receiver := testutil.SubscriberReceiver(t)

	// One owner, two agents (different mailboxes on different domains
	// so a verified-domain check passes for both).
	_, keyA, agentA := setupSubscriberOwner(t, ts, "wh-filta", false)
	domainB := "wh-filtb.example.com"
	if _, err := ts.Store.ClaimOrCreateDomain(context.Background(), domainB, agentA.UserID); err != nil {
		t.Fatalf("claim b: %v", err)
	}
	if err := ts.Store.VerifyDomain(context.Background(), domainB, agentA.UserID); err != nil {
		t.Fatalf("verify b: %v", err)
	}
	agentB, err := ts.Store.CreateAgent(context.Background(),
		"bot@"+domainB, domainB, "Bot B", "", "local", agentA.UserID)
	if err != nil {
		t.Fatalf("create agent b: %v", err)
	}

	// One webhook per agent, filtered to that agent's id.
	whA := registerWebhook(t, ts, agentA.UserID, receiver.Server.URL+"/a",
		[]string{"email.sent"}, identity.WebhookFilters{AgentIDs: []string{agentA.ID}})
	whB := registerWebhook(t, ts, agentA.UserID, receiver.Server.URL+"/b",
		[]string{"email.sent"}, identity.WebhookFilters{AgentIDs: []string{agentB.ID}})

	// Send through agent A → only /a fires.
	body := `{"to":["alice@example.com"],"subject":"from A","text":"x"}`
	status, _ := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agentA.EmailAddress()), keyA.PlaintextKey, body)
	if status != 200 {
		t.Fatalf("send-A status=%d", status)
	}

	tick(t, ts)
	got := receiver.WaitFor(t, 2*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 1 })
	if len(got) != 1 || got[0].URL != "/a" {
		t.Fatalf("expected exactly /a; got %d captures: %v", len(got), summarize(got))
	}
	if !verifyHMACv1(t, got[0].Headers, got[0].RawBody, whA.SigningSecret) {
		t.Errorf("A HMAC failed")
	}

	// Send through agent B → only /b fires.
	receiver.Reset()
	bodyB := `{"to":["bob@example.com"],"subject":"from B","text":"x"}`
	status, _ = authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agentB.EmailAddress()), keyA.PlaintextKey, bodyB)
	if status != 200 {
		t.Fatalf("send-B status=%d", status)
	}

	tick(t, ts)
	got = receiver.WaitFor(t, 2*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 1 })
	if len(got) != 1 || got[0].URL != "/b" {
		t.Fatalf("expected exactly /b; got %d captures: %v", len(got), summarize(got))
	}
	if !verifyHMACv1(t, got[0].Headers, got[0].RawBody, whB.SigningSecret) {
		t.Errorf("B HMAC failed")
	}
}

func TestWebhooksE2E_FilterByConversationID(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	receiver := testutil.SubscriberReceiver(t)

	_, key, agent := setupSubscriberOwner(t, ts, "wh-conv", false)
	registerWebhook(t, ts, agent.UserID, receiver.Server.URL+"/match",
		[]string{"email.sent"}, identity.WebhookFilters{ConversationIDs: []string{"conv-match"}})

	// Send WITHOUT conversation_id — should NOT match (H5 rule:
	// filter requires X but event has none).
	body := `{"to":["alice@example.com"],"subject":"no conv","text":"x"}`
	status, _ := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey, body)
	if status != 200 {
		t.Fatalf("send-noconv status=%d", status)
	}
	tick(t, ts)
	// Use a small explicit wait then assert no posts: short timeout
	// to avoid a flaky pass if the worker is slow.
	receiver.WaitFor(t, 500*time.Millisecond, func(c []testutil.SubscriberCaptured) bool { return false })
	if got := receiver.Captured(); len(got) != 0 {
		t.Errorf("filter required conversation_id but event had none → should skip; got %v", summarize(got))
	}

	// Send WITH the matching conversation_id → fires.
	body = `{"to":["alice@example.com"],"subject":"yes conv","text":"x","conversation_id":"conv-match"}`
	status, _ = authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey, body)
	if status != 200 {
		t.Fatalf("send-conv status=%d", status)
	}
	tick(t, ts)
	got := receiver.WaitFor(t, 2*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 1 })
	if len(got) != 1 || got[0].URL != "/match" {
		t.Fatalf("expected /match; got %v", summarize(got))
	}
}

// TestWebhooksE2E_FilterByLabelsH5 documents H5: when a filter
// requires a value of type T but the event has no T (here, an
// email.sent event whose envelope carries no labels), the subscriber
// MUST be skipped. Without this rule, an "urgent-only" subscriber
// would receive every unlabelled message — the design explicitly
// rejects that as a foot-gun.
func TestWebhooksE2E_FilterByLabelsH5(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	receiver := testutil.SubscriberReceiver(t)

	_, key, agent := setupSubscriberOwner(t, ts, "wh-label", false)
	registerWebhook(t, ts, agent.UserID, receiver.Server.URL+"/labelled",
		[]string{"email.sent"}, identity.WebhookFilters{Labels: []string{"urgent"}})

	body := `{"to":["alice@example.com"],"subject":"unlabelled","text":"x"}`
	if status, _ := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey, body); status != 200 {
		t.Fatalf("send status=%d", status)
	}
	tick(t, ts)
	receiver.WaitFor(t, 500*time.Millisecond, func(c []testutil.SubscriberCaptured) bool { return false })
	if got := receiver.Captured(); len(got) != 0 {
		t.Errorf("H5 violation: filter required labels but event had none → should skip; got %v", summarize(got))
	}
}

// ----------------------------------------------------------------------
// P4 — Signing edge cases (rotation grace + disabled webhook silent)
// ----------------------------------------------------------------------

func TestWebhooksE2E_RotationGrace_DualSig(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	receiver := testutil.SubscriberReceiver(t)

	_, key, agent := setupSubscriberOwner(t, ts, "wh-rot", false)
	wh := registerWebhook(t, ts, agent.UserID, receiver.Server.URL+"/rot",
		[]string{"email.sent"}, identity.WebhookFilters{})
	oldSecret := wh.SigningSecret

	// Rotate. After this, the worker should dual-sign every delivery
	// for the 24h grace window.
	newSecret, _, err := ts.Store.RotateSecret(context.Background(), wh.ID, agent.UserID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if newSecret == oldSecret {
		t.Fatalf("rotation returned same secret")
	}

	body := `{"to":["alice@example.com"],"subject":"rotated","text":"x"}`
	if status, _ := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey, body); status != 200 {
		t.Fatalf("send status=%d", status)
	}
	tick(t, ts)
	got := receiver.WaitFor(t, 2*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 1 })
	if len(got) != 1 {
		t.Fatalf("got %d captures, want 1", len(got))
	}

	// Parse signature; expect exactly 2 v1= entries.
	sig := got[0].Headers.Get("X-E2A-Signature")
	v1count := strings.Count(sig, "v1=")
	if v1count != 2 {
		t.Errorf("expected 2 v1= entries during rotation grace, got %d (sig=%q)", v1count, sig)
	}
	// Both must verify — caller migrating from old → new must still
	// pass during the grace window.
	if !verifyHMACv1(t, got[0].Headers, got[0].RawBody, oldSecret) {
		t.Errorf("old secret should still verify during grace")
	}
	if !verifyHMACv1(t, got[0].Headers, got[0].RawBody, newSecret) {
		t.Errorf("new secret should verify")
	}
}

func TestWebhooksE2E_DisabledWebhookNoFire(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	receiver := testutil.SubscriberReceiver(t)

	_, key, agent := setupSubscriberOwner(t, ts, "wh-disab", false)
	wh := registerWebhook(t, ts, agent.UserID, receiver.Server.URL+"/dis",
		[]string{"email.sent"}, identity.WebhookFilters{})

	// Disable via storage (the public API does it via PATCH, which is
	// exercised in the handler tests).
	enabled := false
	if _, err := ts.Store.UpdateWebhook(context.Background(), wh.ID, agent.UserID,
		identity.WebhookUpdate{Enabled: &enabled}); err != nil {
		t.Fatalf("UpdateWebhook disable: %v", err)
	}

	body := `{"to":["alice@example.com"],"subject":"x","text":"x"}`
	if status, _ := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey, body); status != 200 {
		t.Fatalf("send status=%d", status)
	}
	tick(t, ts)
	// Give the worker + publisher a beat; assert nothing arrived.
	receiver.WaitFor(t, 500*time.Millisecond, func(c []testutil.SubscriberCaptured) bool { return false })
	if got := receiver.Captured(); len(got) != 0 {
		t.Errorf("disabled webhook should receive no events; got %v", summarize(got))
	}
}

// ----------------------------------------------------------------------
// P5 — Auto-disable
// ----------------------------------------------------------------------

func TestWebhooksE2E_AutoDisableAfterFailures(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	receiver := testutil.SubscriberReceiver(t)
	receiver.SetStatus("/dead", 503)

	_, _, agent := setupSubscriberOwner(t, ts, "wh-autodis", false)
	wh := registerWebhook(t, ts, agent.UserID, receiver.Server.URL+"/dead",
		[]string{"email.sent"}, identity.WebhookFilters{})

	// Seed 10 failed delivery rows directly. Going through real
	// /send and exhausting retries would take longer than the test
	// budget (5 attempts × backoff). The auto-disable janitor only
	// reads the rows' final status; seeding them keeps the test
	// deterministic.
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, max_attempts, last_error, last_status_code)
			 VALUES ($1, $2, 'email.sent', '{"event":"email.sent"}'::jsonb, 'failed', 5, 5, 'HTTP 503', 503)`,
			"whd_seed_autodis_"+strconv.Itoa(i)+"_"+wh.ID, wh.ID,
		)
		if err != nil {
			t.Fatalf("seed failed delivery: %v", err)
		}
	}

	n, err := ts.Store.AutoDisableFailingWebhooks(ctx, nil)
	if err != nil {
		t.Fatalf("AutoDisableFailingWebhooks: %v", err)
	}
	if n != 1 {
		t.Errorf("disabled count = %d, want 1", n)
	}
	after, err := ts.Store.GetWebhookByID(ctx, wh.ID, agent.UserID)
	if err != nil {
		t.Fatalf("GetWebhookByID: %v", err)
	}
	if after.Enabled {
		t.Errorf("webhook should be disabled after threshold failures")
	}
	if after.AutoDisabledAt == nil {
		t.Errorf("auto_disabled_at not set")
	}
	if after.AutoDisableReason != "HTTP 503" {
		t.Errorf("AutoDisableReason = %q, want the most recent terminal error (HTTP 503)", after.AutoDisableReason)
	}

	// The reason must reach the API view (additive optional field): the
	// first question a user asks about an auto-disabled endpoint is WHY.
	owner, err := ts.Store.GetUserByID(ctx, agent.UserID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	ownerKey, err := ts.Store.CreateAPIKey(ctx, owner.ID, "autodis-view", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	status, body := authedJSON(t, "GET", ts.HTTPServer.URL+"/v1/webhooks/"+wh.ID, ownerKey.PlaintextKey, "")
	if status != 200 {
		t.Fatalf("GET webhook status=%d body=%s", status, body)
	}
	var view struct {
		Enabled            bool   `json:"enabled"`
		AutoDisabledReason string `json:"auto_disabled_reason"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("unmarshal webhook view: %v", err)
	}
	if view.Enabled {
		t.Errorf("API view still shows enabled")
	}
	if view.AutoDisabledReason != "HTTP 503" {
		t.Errorf("auto_disabled_reason on the wire = %q, want HTTP 503", view.AutoDisabledReason)
	}
}

// summarize turns captured payloads into a debuggable one-line summary
// for failed assertions. Returning the full envelope blob would dwarf
// the actual cause.
func summarize(c []testutil.SubscriberCaptured) string {
	parts := make([]string, 0, len(c))
	for _, x := range c {
		evt, _ := x.Envelope["type"].(string)
		parts = append(parts, x.URL+"["+evt+"]")
	}
	return strings.Join(parts, " ")
}

// ----------------------------------------------------------------------
// P6 — conversation_id is agent-owned and maintained across the thread
// ----------------------------------------------------------------------

// TestWebhooksE2E_ConversationThreadedFromAgentReply proves the
// agent-owned conversation model end to end: first-contact inbound has no
// thread context (conversation_id=""), but once the agent has replied with
// its OWN id (stored on the outbound with that reply's Message-ID in
// provider_message_id), an external follow-up that References the reply is
// threaded back to the agent's id — and the email.received webhook echoes
// that id so the agent re-maps to its internal conversation.
//
// The agent's reply is recorded via the store rather than POSTed: replying
// to an external recipient needs an upstream SMTP relay the CI harness
// doesn't wire (the skipped TestOutbound* tests), and Mailpit's
// 250-response provider_message_id is not a real Message-ID, so it makes a
// poor References anchor. The row written here is the shape the reply
// handler persists, with the clean Message-ID an SES send would yield. The
// threading itself — the behavior under test — runs through the real relay
// + resolveConversationID.
func TestWebhooksE2E_ConversationThreadedFromAgentReply(t *testing.T) {
	pool := testutil.TestDB(t)
	receiver := testutil.SubscriberReceiver(t)
	ts := testutil.TestServer(t, pool)
	ctx := context.Background()

	user, _, agent := setupDomainAndAgent(t, ts, "agent@conv.example.com", "conv.example.com", "", "")
	registerWebhook(t, ts, user.ID, receiver.Server.URL+"/received",
		[]string{"email.received"}, identity.WebhookFilters{})

	// (1) First-contact inbound — no References, no prior thread → "".
	msg1 := "From: alice@gmail.com\r\nTo: agent@conv.example.com\r\n" +
		"Subject: Project kickoff\r\nMessage-ID: <kick-1@gmail.com>\r\n\r\nLet's start."
	if err := smtp.SendMail(ts.SMTPAddr, nil, "alice@gmail.com", []string{"agent@conv.example.com"}, []byte(msg1)); err != nil {
		t.Fatalf("SendMail #1: %v", err)
	}
	tick(t, ts)
	got := receiver.WaitFor(t, 5*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 1 })
	if len(got) != 1 {
		t.Fatalf("first inbound: got %d captures, want 1", len(got))
	}
	d1, _ := got[0].Envelope["data"].(map[string]any)
	// conversation_id is optional-with-omitempty on the typed EmailReceivedData
	// payload — a first-contact message (no thread context yet) omits the key.
	if cv, present := d1["conversation_id"]; present && fmt.Sprint(cv) != "" {
		t.Errorf("first-contact conversation_id = %v, want omitted (no thread context yet)", cv)
	}

	// (2) The agent replies, stamping its own conversation id. Recorded
	// directly (see doc comment) with the reply's Message-ID in
	// provider_message_id — exactly what an external client will reference.
	const agentConvID = "conv_agent_thread_1"
	const replyMsgID = "<agent-reply-1@conv.example.com>"
	if _, err := ts.Store.CreateOutboundMessage(ctx, agent.ID,
		[]string{"alice@gmail.com"}, nil, nil, "Re: Project kickoff",
		"reply", "smtp", replyMsgID, agentConvID, nil); err != nil {
		t.Fatalf("record agent reply: %v", err)
	}

	// (3) Alice replies to the agent's reply (References it, Gmail-style).
	// The relay must resolve the agent's conversation_id and the webhook
	// must carry it back.
	receiver.Reset()
	msg2 := "From: alice@gmail.com\r\nTo: agent@conv.example.com\r\n" +
		"Subject: Re: Project kickoff\r\nMessage-ID: <kick-2@gmail.com>\r\n" +
		"In-Reply-To: " + replyMsgID + "\r\n" +
		"References: <kick-1@gmail.com> " + replyMsgID + "\r\n\r\nFollowing up."
	if err := smtp.SendMail(ts.SMTPAddr, nil, "alice@gmail.com", []string{"agent@conv.example.com"}, []byte(msg2)); err != nil {
		t.Fatalf("SendMail #2: %v", err)
	}
	tick(t, ts)
	got2 := receiver.WaitFor(t, 5*time.Second, func(c []testutil.SubscriberCaptured) bool { return len(c) >= 1 })
	if len(got2) != 1 {
		t.Fatalf("follow-up inbound: got %d captures, want 1", len(got2))
	}
	d2, _ := got2[0].Envelope["data"].(map[string]any)
	if cv := fmt.Sprint(d2["conversation_id"]); cv != agentConvID {
		t.Errorf("follow-up conversation_id = %q, want %q (threaded back to the agent's id)", cv, agentConvID)
	}
}
