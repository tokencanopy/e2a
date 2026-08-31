package selftest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
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
	"time"

	"github.com/tokencanopy/e2a/internal/emailauth"
)

// defaultRoundTripTimeout bounds how long the inbound round-trip waits for the
// webhook callback when Probe.Timeout is unset. It must exceed the outbox drain
// + River delivery latency in production; the prober overrides it via
// E2A_PROBE_TIMEOUT (Probe.Timeout).
const defaultRoundTripTimeout = 30 * time.Second

// All is the critical-path battery. Every scenario here is SmokeSafe: read-only,
// a loopback (no egress), the inbound round-trip (synthetic mail to the probe
// agent), or a real outbound send to the AWS mailbox simulator (no real
// recipient). None meters (the probe runs under a system-class account) and none
// emails an owner.
var All = []Scenario{
	{Name: "liveness", SmokeSafe: true, Run: scenarioLiveness},
	{Name: "auth_read", SmokeSafe: true, Run: scenarioAuthRead},
	// delegated_auth obtains a fresh issuer-signed token from a trusted,
	// fixed-principal vending endpoint and uses it for one read-only account
	// request. It is the page-safe availability signal for delegated auth:
	// unlike verifier failure counters, arbitrary public callers cannot choose
	// the token or subject this scenario exercises.
	{Name: "delegated_auth", SmokeSafe: true, Run: scenarioDelegatedAuth},
	{Name: "inbound_round_trip", SmokeSafe: true, Run: scenarioInboundRoundTrip},
	// outbound_send does a REAL SES submit, but only to the mailbox simulator
	// (no real recipient, no cost, no owner notification, system-class = no
	// metering), then confirms the email.sent event is delivered + HMAC-signed.
	{Name: "outbound_send", SmokeSafe: true, Run: scenarioOutboundSend},
	{Name: "self_send_loopback", SmokeSafe: true, Run: scenarioSelfSendLoopback},
	// websocket_round_trip exercises the /v1 WebSocket live-tail transport —
	// the only push channel no other scenario touches: Bearer handshake, a
	// nonce'd loopback self-send, then the email.received push frame on the
	// socket. Loopback ⇒ no egress, no HITL, no metering.
	{Name: "websocket_round_trip", SmokeSafe: true, Run: scenarioWebSocketRoundTrip},
	// agent_lifecycle MUTATES prod (creates then deletes an ephemeral agent on
	// the probe's verified domain) but is self-cleaning — no email, no owner
	// notification, no metering (system-class account). SmokeSafe, but the
	// create/delete churn is heavier than the read-only checks; an operator who
	// wants a purely read-only prod battery can drop it.
	{Name: "agent_lifecycle", SmokeSafe: true, Run: scenarioAgentLifecycle},
	// mcp_http_round_trip exercises the DEPLOYED streamable-HTTP MCP surface
	// (the co-versioned mcp-server image + its Caddy /mcp route), which no other
	// scenario touches — tools/list then a whoami tool call, both read-only. It
	// SKIPS (pass) when E2A_PROBE_MCP_URL is unset, so a prober without an MCP
	// endpoint configured stays green.
	{Name: "mcp_http_round_trip", SmokeSafe: true, Run: scenarioMCPHTTPRoundTrip},
}

func pass(detail string) Result { return Result{Status: StatusPass, Detail: detail} }
func fail(format string, a ...any) Result {
	return Result{Status: StatusFail, Detail: fmt.Sprintf(format, a...)}
}

// scenarioLiveness: GET /api/health is up and reports ok. Shallow by design —
// no dependency checks (those belong to /readyz and /selftest).
func scenarioLiveness(ctx context.Context, p *Probe) Result {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.HTTPBaseURL+"/api/health", nil)
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return fail("GET /api/health: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fail("GET /api/health: HTTP %d", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("ok")) {
		return fail("GET /api/health: unexpected body %q", string(body))
	}
	return pass("health ok")
}

// scenarioAuthRead: an authenticated read of the probe agent. Exercises API key
// auth + a DB read. The email is percent-encoded — a real client must do this
// (the in-process test client would otherwise hide the encoding bug).
func scenarioAuthRead(ctx context.Context, p *Probe) Result {
	u := p.HTTPBaseURL + "/v1/agents/" + url.PathEscape(p.AgentEmail)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return fail("GET agent: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fail("GET agent: HTTP %d", resp.StatusCode)
	}
	return pass("authenticated read ok")
}

const maxDelegatedTokenResponseBytes = 20 * 1024

// scenarioDelegatedAuth exercises the active issuer → JWKS → claim policy →
// external-principal mapping → account authorization path. The vending
// endpoint owns the synthetic principal and token shape; this client supplies
// no identity, audience, scope, role, or lifetime input. Neither response body
// is ever included in a result detail.
func scenarioDelegatedAuth(ctx context.Context, p *Probe) Result {
	configured := p.DelegatedTokenURL != "" && p.DelegatedTokenSecret != ""
	if !configured {
		if p.RequireDelegatedAuth {
			return fail("delegated token vending is required but incomplete")
		}
		return pass("skipped: delegated token vending unset")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.DelegatedTokenURL, nil)
	if err != nil {
		return fail("delegated token request configuration invalid")
	}
	req.Header.Set("Authorization", "Bearer "+p.DelegatedTokenSecret)
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return fail("delegated token endpoint unavailable")
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxDelegatedTokenResponseBytes+1))
	if readErr != nil {
		return fail("delegated token response unreadable")
	}
	if resp.StatusCode != http.StatusOK {
		return fail("delegated token endpoint: HTTP %d", resp.StatusCode)
	}
	if len(body) > maxDelegatedTokenResponseBytes {
		return fail("delegated token response too large")
	}
	var exchanged struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&exchanged); err != nil {
		return fail("delegated token response malformed")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fail("delegated token response malformed")
	}
	if exchanged.AccessToken == "" || len(exchanged.AccessToken) > 16*1024 ||
		strings.ContainsAny(exchanged.AccessToken, " \t\r\n") ||
		exchanged.TokenType != "Bearer" || exchanged.Scope != "account" ||
		exchanged.ExpiresIn < 1 || exchanged.ExpiresIn > 120 {
		return fail("delegated token response malformed")
	}

	readURL := p.HTTPBaseURL + "/v1/agents?limit=1"
	readReq, err := http.NewRequestWithContext(ctx, http.MethodGet, readURL, nil)
	if err != nil {
		return fail("delegated auth read configuration invalid")
	}
	readReq.Header.Set("Authorization", "Bearer "+exchanged.AccessToken)
	readResp, err := p.httpClient().Do(readReq)
	if err != nil {
		return fail("delegated auth read unavailable")
	}
	defer readResp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(readResp.Body, 1<<20))
	if readResp.StatusCode != http.StatusOK {
		return fail("delegated auth read: HTTP %d", readResp.StatusCode)
	}
	return pass("delegated issuer, verification, mapping, and account read ok")
}

// scenarioInboundRoundTrip is the core check: inject a unique inbound message
// over real SMTP and confirm it comes back out the webhook with a valid HMAC.
// Covers the SMTP listener, emailauth, agent lookup, DB write, outbox, the
// subscriber worker, webhook HTTP delivery, and signing.
func scenarioInboundRoundTrip(ctx context.Context, p *Probe) Result {
	if p.Sink == nil {
		return fail("no sink configured")
	}
	nonce, err := randHex(16)
	if err != nil {
		return fail("nonce: %v", err)
	}
	msg := fmt.Sprintf("From: e2a-selftest <selftest@e2a-selftest.invalid>\r\n"+
		"To: %s\r\n"+
		"Subject: e2a-selftest %s\r\n"+
		"Message-ID: <%s@e2a-selftest.invalid>\r\n"+
		"\r\n"+
		"e2a selftest round-trip %s\r\n", p.AgentEmail, nonce, nonce, nonce)

	if err := smtp.SendMail(p.SMTPAddr, nil, "selftest@e2a-selftest.invalid", []string{p.AgentEmail}, []byte(msg)); err != nil {
		return fail("SMTP send: %v", err)
	}

	d, err := p.Sink.Await(ctx, func(d Delivery) bool {
		return bytes.Contains(d.Body, []byte(nonce))
	}, p.roundTripTimeout())
	if err != nil {
		return fail("await webhook for nonce %s: %v", nonce, err)
	}
	if !verifyHMAC(d.Headers.Get("X-E2A-Signature"), d.Body, p.WebhookSecret) {
		return fail("webhook HMAC verification failed")
	}

	// This is the only scenario that originates real inbound SMTP (every other
	// scenario is loopback or outbound), so it's the only place `authentication`
	// evidence can be checked over the wire at all — the conformance suite runs
	// off a GitHub-hosted runner with no SMTP access and can only assert the
	// null-for-loopback case. The synthetic sender domain (e2a-selftest.invalid,
	// an RFC 2606 reserved TLD) publishes no SPF/DKIM/DMARC records and is never
	// signed, so a genuine DMARC pass isn't achievable here — but the field must
	// still be present, well-formed, and correctly reflect "evaluated, did not
	// authenticate" rather than being silently dropped, null, or malformed.
	var env struct {
		Data struct {
			Authentication *emailauth.Authentication `json:"authentication"`
		} `json:"data"`
	}
	if err := json.Unmarshal(d.Body, &env); err != nil {
		return fail("decode email.received envelope: %v", err)
	}
	auth := env.Data.Authentication
	if auth == nil {
		return fail("email.received data.authentication is null for a real inbound SMTP delivery (want populated evidence, even unauthenticated)")
	}
	if len(auth.DKIM) != 0 {
		return fail("data.authentication.dkim: want empty (unsigned synthetic message), got %d signature(s)", len(auth.DKIM))
	}
	if auth.DMARC.Status == emailauth.StatusPass {
		return fail("data.authentication.dmarc.status: want not-pass (e2a-selftest.invalid has no DNS/SPF/DKIM/DMARC infrastructure), got pass")
	}
	return pass(fmt.Sprintf("inbound round-trip + HMAC ok; authentication evidence present (dmarc=%s)", auth.DMARC.Status))
}

// scenarioOutboundSend is the real-egress counterpart to the inbound round-trip:
// the probe agent sends a unique message to the AWS SES mailbox simulator
// (success@simulator.amazonses.com — accepted + blackholed, no real recipient,
// no cost, no reputation impact), then confirms the resulting email.sent event
// is delivered out the webhook with a valid HMAC. Covers the outbound API +
// screening + compose + real SES submit + the outbound event → outbox →
// subscriber worker → webhook delivery → signing path. Correlated by the
// returned message_id (the outbound worker emits it after the SES submit).
// Requires the probe
// webhook to subscribe to email.sent (see cmd/e2a-prober seed).
func scenarioOutboundSend(ctx context.Context, p *Probe) Result {
	if p.Sink == nil {
		return fail("no sink configured")
	}
	nonce, err := randHex(16)
	if err != nil {
		return fail("nonce: %v", err)
	}
	u := p.HTTPBaseURL + "/v1/agents/" + url.PathEscape(p.AgentEmail) + "/messages"
	payload := map[string]any{
		"to":      []string{"success@simulator.amazonses.com"},
		"subject": "e2a-selftest outbound " + nonce,
		"text":    "e2a selftest outbound " + nonce,
	}
	b, _ := json.Marshal(payload)
	st, respBody, err := p.do(ctx, http.MethodPost, u, b)
	if err != nil {
		return fail("send: %v", err)
	}
	if st != http.StatusAccepted {
		return fail("send: HTTP %d", st)
	}
	var out struct {
		MessageID string `json:"message_id"`
		Status    string `json:"status"`
	}
	if jerr := json.Unmarshal(respBody, &out); jerr != nil || out.MessageID == "" {
		return fail("send: could not parse message_id from response (status=%q)", out.Status)
	}
	if out.Status != "accepted" {
		return fail("send: status=%q, want accepted", out.Status)
	}

	d, err := p.Sink.Await(ctx, func(d Delivery) bool {
		return bytes.Contains(d.Body, []byte(out.MessageID)) &&
			bytes.Contains(d.Body, []byte("email.sent"))
	}, p.roundTripTimeout())
	if err != nil {
		return fail("await email.sent for message %s: %v", out.MessageID, err)
	}
	if !verifyHMAC(d.Headers.Get("X-E2A-Signature"), d.Body, p.WebhookSecret) {
		return fail("email.sent webhook HMAC verification failed")
	}
	return pass("outbound send → email.sent + HMAC ok")
}

// scenarioSelfSendLoopback: the probe agent sends to itself. Self-send routes
// through the loopback path (method=loopback) — no SMTP egress, no HITL owner
// notification — exercising the outbound API + compose path safely.
func scenarioSelfSendLoopback(ctx context.Context, p *Probe) Result {
	u := p.HTTPBaseURL + "/v1/agents/" + url.PathEscape(p.AgentEmail) + "/messages"
	payload := map[string]any{
		"to":      []string{p.AgentEmail},
		"subject": "e2a-selftest loopback",
		"text":    "e2a selftest loopback",
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return fail("self-send: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fail("self-send: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Method != "loopback" {
		return fail("self-send method=%q, want loopback (no egress)", out.Method)
	}
	return pass("self-send loopback ok")
}

// scenarioAgentLifecycle exercises the create/get/delete agent endpoints with a
// unique, self-cleaning ephemeral agent on the probe's verified domain. It is
// the only mutating scenario; a deferred best-effort delete guarantees cleanup
// even if an assertion fails partway through.
func scenarioAgentLifecycle(ctx context.Context, p *Probe) Result {
	at := strings.LastIndex(p.AgentEmail, "@")
	if at < 0 {
		return fail("probe agent email %q has no domain", p.AgentEmail)
	}
	domain := p.AgentEmail[at+1:]
	nonce, err := randHex(8)
	if err != nil {
		return fail("nonce: %v", err)
	}
	email := "probe-life-" + nonce + "@" + domain
	agentURL := p.HTTPBaseURL + "/v1/agents/" + url.PathEscape(email)

	// CREATE → 201.
	body, _ := json.Marshal(map[string]string{"email": email, "name": "e2a selftest lifecycle"})
	st, _, err := p.do(ctx, http.MethodPost, p.HTTPBaseURL+"/v1/agents", body)
	if err != nil {
		return fail("create: %v", err)
	}
	if st != http.StatusCreated {
		return fail("create agent: HTTP %d, want 201", st)
	}
	// Safety net: ensure the ephemeral agent is removed even on an early return
	// below. Best-effort, fresh context so it runs even if ctx is done.
	// permanent=true: the default delete is now a soft delete into the trash
	// (30-day retention), and a synthetic probe agent must not pile up there
	// on every battery run.
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _, _ = p.do(cctx, http.MethodDelete, agentURL+"?confirm=DELETE&permanent=true", nil)
	}()

	// GET → 200.
	if st, _, err := p.do(ctx, http.MethodGet, agentURL, nil); err != nil {
		return fail("get created agent: %v", err)
	} else if st != http.StatusOK {
		return fail("get created agent: HTTP %d, want 200", st)
	}

	// PATCH (update display name) → 200.
	patchBody, _ := json.Marshal(map[string]string{"name": "e2a selftest lifecycle (updated)"})
	if st, _, err := p.do(ctx, http.MethodPatch, agentURL, patchBody); err != nil {
		return fail("update agent: %v", err)
	} else if st != http.StatusOK {
		return fail("update agent: HTTP %d, want 200", st)
	}

	// DELETE (confirmed, permanent) → 200 + deletion object. Ephemeral probe
	// agents skip the trash so repeated battery runs cannot accumulate them.
	if st, body, err := p.do(ctx, http.MethodDelete, agentURL+"?confirm=DELETE&permanent=true", nil); err != nil {
		return fail("delete agent: %v", err)
	} else if st != http.StatusOK {
		return fail("delete agent: HTTP %d, want 200", st)
	} else {
		var res struct {
			Deleted bool `json:"deleted"`
		}
		if err := json.Unmarshal(body, &res); err != nil || !res.Deleted {
			return fail("delete agent: want deletion object {deleted:true}, got %s", string(body))
		}
	}

	// Confirm it's gone — a follow-up GET must not return 200.
	if st, _, err := p.do(ctx, http.MethodGet, agentURL, nil); err != nil {
		return fail("get after delete: %v", err)
	} else if st == http.StatusOK {
		return fail("agent still readable after delete (HTTP 200)")
	}
	return pass("agent create→get→delete ok")
}

// do issues an authenticated request and returns the status, body, and error.
func (p *Probe) do(ctx context.Context, method, u string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

// scenarioMCPHTTPRoundTrip exercises the DEPLOYED streamable-HTTP MCP server —
// the surface that ships to prod but that every other scenario ignores. It is
// two read-only JSON-RPC calls over the real transport:
//
//  1. tools/list — proves the /mcp route, the SSE transport, Bearer auth, and
//     the tool registry are all live on the candidate mcp-server image;
//  2. tools/call whoami — a genuine end-to-end MCP dispatch (transport → tool
//     handler → e2a backend → back), with no mutation and nothing to clean up.
//
// The server is stateless (sessionIdGenerator=undefined), so each POST stands
// alone — no initialize handshake, no Mcp-Session-Id. It authenticates with the
// probe agent's own Bearer key (the MCP server forwards it to the backend).
//
// E2A_PROBE_MCP_URL is the in-cluster endpoint (e.g. http://mcp-server:3000/mcp),
// matching how the other probe vars address containers directly. NOTE: the MCP
// server enforces a Host allowlist (MCP_ALLOWED_HOSTS) on /mcp and 421s anything
// else, so that URL's host MUST be in the deployment's allowlist — the staging
// compose adds the in-cluster service name there. When E2A_PROBE_MCP_URL is unset
// the scenario SKIPS (returns pass) so a prober with no MCP endpoint configured
// stays green rather than warning; the release pipeline's prober gate separately
// asserts the scenario actually ran (didn't skip) on staging.
func scenarioMCPHTTPRoundTrip(ctx context.Context, p *Probe) Result {
	if p.MCPBaseURL == "" {
		if p.RequireMCP {
			// A prod prober that silently skips MCP would report green while
			// never probing the surface the 99.9% MCP availability target is
			// measured on. RequireMCP (E2A_PROBE_REQUIRE_MCP) makes the
			// misconfiguration loud instead.
			return fail("E2A_PROBE_MCP_URL unset but E2A_PROBE_REQUIRE_MCP=true")
		}
		return pass("skipped: E2A_PROBE_MCP_URL unset")
	}

	// 1. tools/list
	env, st, err := p.mcpCall(ctx, 1, "tools/list", nil)
	if err != nil {
		return fail("mcp tools/list: %v", err)
	}
	if st != http.StatusOK {
		return fail("mcp tools/list: HTTP %d, want 200", st)
	}
	names := mcpToolNames(env)
	if len(names) == 0 {
		return fail("mcp tools/list: no tools returned")
	}
	found := false
	for _, n := range names {
		if n == "whoami" {
			found = true
			break
		}
	}
	if !found {
		return fail("mcp tools/list: missing expected tool 'whoami' (got %d tools)", len(names))
	}

	// 2. tools/call whoami
	env, st, err = p.mcpCall(ctx, 2, "tools/call", map[string]any{
		"name":      "whoami",
		"arguments": map[string]any{},
	})
	if err != nil {
		return fail("mcp whoami call: %v", err)
	}
	if st != http.StatusOK {
		return fail("mcp whoami call: HTTP %d, want 200", st)
	}
	if e, ok := env["error"]; ok && e != nil {
		return fail("mcp whoami call: JSON-RPC error: %v", e)
	}
	res, ok := env["result"].(map[string]any)
	if !ok {
		return fail("mcp whoami call: response has no result object")
	}
	if isErr, _ := res["isError"].(bool); isErr {
		return fail("mcp whoami call: tool result isError=true")
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		return fail("mcp whoami call: empty tool result content")
	}
	// Require an actual non-empty text block — a bare 200 with an empty/typeless
	// content array (a backend returning nothing useful) must not pass.
	hasText := false
	for _, c := range content {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cm["type"].(string); t == "text" {
			if s, _ := cm["text"].(string); strings.TrimSpace(s) != "" {
				hasText = true
				break
			}
		}
	}
	if !hasText {
		return fail("mcp whoami call: result has no non-empty text content")
	}
	return pass("mcp http tools/list + whoami round-trip ok")
}

// mcpCall POSTs one JSON-RPC request to the streamable-HTTP MCP endpoint and
// returns the decoded response envelope. The MCP SDK answers a POST as an SSE
// stream (text/event-stream) unless JSON responses are enabled, so the body is
// parsed accordingly. A non-200 returns (nil, status, nil) — the caller decides.
func (p *Probe) mcpCall(ctx context.Context, id int, method string, params any) (map[string]any, int, error) {
	reqBody := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		reqBody["params"] = params
	}
	b, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.MCPBaseURL, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	// Streamable-HTTP requires the client to accept both framings.
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	env, err := parseJSONRPCEnvelope(raw, resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return env, resp.StatusCode, nil
}

// parseJSONRPCEnvelope decodes a JSON-RPC message from an MCP streamable-HTTP
// response, accepting either a bare JSON body or an SSE stream. For SSE it walks
// each event's data: lines and returns the first that decodes to a message
// carrying a "jsonrpc" key (ignoring pings / other event types).
func parseJSONRPCEnvelope(raw []byte, contentType string) (map[string]any, error) {
	if strings.Contains(contentType, "text/event-stream") {
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")
		for _, event := range strings.Split(body, "\n\n") {
			var data strings.Builder
			for _, line := range strings.Split(event, "\n") {
				if after, ok := strings.CutPrefix(line, "data:"); ok {
					// SSE joins successive data: lines within one event with \n.
					if data.Len() > 0 {
						data.WriteByte('\n')
					}
					data.WriteString(strings.TrimPrefix(after, " "))
				}
			}
			if data.Len() == 0 {
				continue
			}
			var env map[string]any
			if err := json.Unmarshal([]byte(data.String()), &env); err != nil {
				continue
			}
			if _, ok := env["jsonrpc"]; ok {
				return env, nil
			}
		}
		return nil, fmt.Errorf("no JSON-RPC message in SSE stream")
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode JSON-RPC body: %w", err)
	}
	return env, nil
}

// mcpToolNames pulls the tool names out of a tools/list result envelope.
func mcpToolNames(env map[string]any) []string {
	res, _ := env["result"].(map[string]any)
	rawTools, _ := res["tools"].([]any)
	names := make([]string, 0, len(rawTools))
	for _, t := range rawTools {
		if tm, ok := t.(map[string]any); ok {
			if n, ok := tm["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	return names
}

// verifyHMAC checks the X-E2A-Signature header against the body using the
// webhook secret. Header format: t=<unix>,v1=<hex>[,v1=<hex>]; signed string is
// "<t>.<body>" (hmac-sha256, hex). Any v1 matching is accepted (rotation grace).
func verifyHMAC(header string, body []byte, secret string) bool {
	if header == "" || secret == "" {
		return false
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "t="):
			ts = strings.TrimPrefix(part, "t=")
		case strings.HasPrefix(part, "v1="):
			sigs = append(sigs, strings.TrimPrefix(part, "v1="))
		}
	}
	if ts == "" || len(sigs) == 0 {
		return false
	}
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s.", ts)
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return true
		}
	}
	return false
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
