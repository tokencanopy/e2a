package selftest

// Internal failure-path tests: assert each scenario reports StatusFail when the
// thing it checks is broken. This is the monitor's whole job — a scenario that
// only ever returns pass on the happy path could mask a real outage. Driven by
// httptest mocks (no DB) so each failure mode is isolated.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func failProbe(baseURL, smtpAddr string, sink *HTTPSink) *Probe {
	return &Probe{
		HTTPBaseURL:   baseURL,
		APIKey:        "k",
		AgentEmail:    "agent@probe.test",
		SMTPAddr:      smtpAddr,
		WebhookSecret: "whsec_test",
		Sink:          sink,
		Timeout:       200 * time.Millisecond,
	}
}

func mustFail(t *testing.T, name string, r Result) {
	t.Helper()
	if r.Status != StatusFail {
		t.Errorf("%s: status = %s (%q), want fail", name, r.Status, r.Detail)
	}
}

func TestScenarioLiveness_Fail(t *testing.T) {
	// Non-200 health.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	mustFail(t, "health 500", scenarioLiveness(context.Background(), failProbe(srv.URL, "", nil)))

	// 200 but wrong body.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"degraded"}`))
	}))
	defer srv2.Close()
	mustFail(t, "health wrong body", scenarioLiveness(context.Background(), failProbe(srv2.URL, "", nil)))

	// Unreachable server.
	mustFail(t, "health unreachable", scenarioLiveness(context.Background(), failProbe("http://127.0.0.1:1", "", nil)))
}

func TestScenarioAuthRead_Fail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	mustFail(t, "auth 401", scenarioAuthRead(context.Background(), failProbe(srv.URL, "", nil)))
}

func TestScenarioDelegatedAuth_ReadsWithFreshVendedToken(t *testing.T) {
	const token = "synthetic.jwt.value"
	var tokenCalls, apiCalls int
	var api *httptest.Server
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			t.Fatalf("token request = %s %s, want POST without query", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer probe-secret" {
			t.Fatalf("token authorization = %q", got)
		}
		if r.ContentLength > 0 {
			t.Fatalf("token request body length = %d, want empty", r.ContentLength)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":60,"scope":"account"}`, token)
	}))
	defer tokenServer.Close()
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/agents" || r.URL.Query().Get("limit") != "1" {
			t.Fatalf("API request = %s %s", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("API authorization = %q", got)
		}
		fmt.Fprint(w, `{"agents":[{"email":"must-not-be-reported@example.test"}]}`)
	}))
	defer api.Close()

	p := failProbe(api.URL, "", nil)
	p.DelegatedTokenURL = tokenServer.URL
	p.DelegatedTokenSecret = "probe-secret"
	p.RequireDelegatedAuth = true
	result := scenarioDelegatedAuth(context.Background(), p)
	if result.Status != StatusPass {
		t.Fatalf("status = %s detail=%q, want pass", result.Status, result.Detail)
	}
	if tokenCalls != 1 || apiCalls != 1 {
		t.Fatalf("calls token=%d api=%d, want 1/1", tokenCalls, apiCalls)
	}
	if strings.Contains(result.Detail, token) || strings.Contains(result.Detail, "must-not-be-reported") {
		t.Fatalf("result leaked token or response body: %q", result.Detail)
	}
}

func TestScenarioDelegatedAuth_ConfigurationAndOpaqueFailures(t *testing.T) {
	p := failProbe("http://127.0.0.1:1", "", nil)
	if result := scenarioDelegatedAuth(context.Background(), p); result.Status != StatusPass || !strings.Contains(result.Detail, "skipped") {
		t.Fatalf("optional unconfigured result = %+v, want skipped pass", result)
	}
	p.RequireDelegatedAuth = true
	mustFail(t, "required config missing", scenarioDelegatedAuth(context.Background(), p))

	const sensitive = "sensitive-upstream-body"
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, sensitive)
	}))
	defer tokenServer.Close()
	p.DelegatedTokenURL = tokenServer.URL
	p.DelegatedTokenSecret = "probe-secret"
	result := scenarioDelegatedAuth(context.Background(), p)
	mustFail(t, "token endpoint unavailable", result)
	if strings.Contains(result.Detail, sensitive) || strings.Contains(result.Detail, "probe-secret") {
		t.Fatalf("failure leaked body or secret: %q", result.Detail)
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"access_token":"","token_type":"Bearer","expires_in":60,"scope":"account"}`)
	}))
	defer malformed.Close()
	p.DelegatedTokenURL = malformed.URL
	mustFail(t, "malformed token response", scenarioDelegatedAuth(context.Background(), p))
}

func TestScenarioSelfSendLoopback_Fail(t *testing.T) {
	// 200 but method != loopback → a real send would have egressed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"method":"smtp"}`))
	}))
	defer srv.Close()
	mustFail(t, "self-send smtp not loopback", scenarioSelfSendLoopback(context.Background(), failProbe(srv.URL, "", nil)))
}

func TestScenarioAgentLifecycle_Fail(t *testing.T) {
	// Create returns 500 → scenario fails before any cleanup is registered.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	mustFail(t, "create 500", scenarioAgentLifecycle(context.Background(), failProbe(srv.URL, "", nil)))
}

func TestScenarioInboundRoundTrip_Fail(t *testing.T) {
	// SMTP listener unreachable → send fails.
	mustFail(t, "smtp unreachable",
		scenarioInboundRoundTrip(context.Background(), failProbe("http://127.0.0.1:1", "127.0.0.1:1", NewHTTPSink())))

	// No sink configured → fail fast.
	mustFail(t, "no sink",
		scenarioInboundRoundTrip(context.Background(), failProbe("http://127.0.0.1:1", "127.0.0.1:1", nil)))
}

func TestSinkAwait_Timeout(t *testing.T) {
	// The round-trip relies on Await timing out (StatusFail) when no webhook
	// arrives. Assert that mechanism directly.
	sink := NewHTTPSink()
	_, err := sink.Await(context.Background(), func(Delivery) bool { return true }, 50*time.Millisecond)
	if err == nil {
		t.Fatal("Await returned nil error with no delivery, want timeout")
	}
}

func TestRunWorst_WithFailure(t *testing.T) {
	// Run aggregates a failing scenario; Worst reports fail.
	scenarios := []Scenario{
		{Name: "ok", SmokeSafe: true, Run: func(context.Context, *Probe) Result { return pass("ok") }},
		{Name: "bad", SmokeSafe: true, Run: func(context.Context, *Probe) Result { return fail("boom") }},
		{Name: "unsafe", SmokeSafe: false, Run: func(context.Context, *Probe) Result { return pass("skipme") }},
	}
	results := Run(context.Background(), failProbe("http://127.0.0.1:1", "", nil), scenarios, true /* smokeOnly */)
	if len(results) != 2 {
		t.Fatalf("ran %d scenarios, want 2 (unsafe one skipped under smokeOnly)", len(results))
	}
	if Worst(results) != StatusFail {
		t.Errorf("Worst = %s, want fail", Worst(results))
	}
	// Worst of empty is fail ("no checks ran" is not healthy).
	if Worst(nil) != StatusFail {
		t.Errorf("Worst(nil) = %s, want fail", Worst(nil))
	}
}

// mcpStub is a minimal streamable-HTTP MCP server: it decodes the JSON-RPC
// method and answers over SSE (the SDK's default framing), so it exercises the
// scenario's real transport + SSE parsing. tools/list returns the given tool
// names; tools/call returns a non-error text result.
func mcpStub(t *testing.T, toolNames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		var result string
		switch req.Method {
		case "tools/list":
			tools := make([]string, 0, len(toolNames))
			for _, n := range toolNames {
				tools = append(tools, fmt.Sprintf(`{"name":%q}`, n))
			}
			result = fmt.Sprintf(`{"tools":[%s]}`, joinComma(tools))
		case "tools/call":
			result = `{"content":[{"type":"text","text":"agent@probe.test"}],"isError":false}`
		default:
			result = `{}`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}\n\n", req.ID, result)
	}))
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func TestScenarioMCPHTTPRoundTrip(t *testing.T) {
	// Unconfigured (no MCP URL) → skip-as-pass, so a prober without an MCP
	// endpoint stays green.
	if r := scenarioMCPHTTPRoundTrip(context.Background(), failProbe("http://127.0.0.1:1", "", nil)); r.Status != StatusPass {
		t.Errorf("unconfigured MCP: status = %s (%q), want pass (skip)", r.Status, r.Detail)
	}

	// Happy path over the real SSE transport: tools/list (incl. whoami) then a
	// whoami tool call.
	srv := mcpStub(t, []string{"whoami", "create_agent", "list_agents"})
	defer srv.Close()
	p := failProbe("http://127.0.0.1:1", "", nil)
	p.MCPBaseURL = srv.URL
	if r := scenarioMCPHTTPRoundTrip(context.Background(), p); r.Status != StatusPass {
		t.Errorf("happy path: status = %s (%q), want pass", r.Status, r.Detail)
	}
}

func TestScenarioMCPHTTPRoundTrip_Fail(t *testing.T) {
	// tools/list succeeds but omits whoami → the registry is wrong.
	srv := mcpStub(t, []string{"send_message"})
	defer srv.Close()
	p := failProbe("http://127.0.0.1:1", "", nil)
	p.MCPBaseURL = srv.URL
	mustFail(t, "tools/list missing whoami", scenarioMCPHTTPRoundTrip(context.Background(), p))

	// 500 from the MCP endpoint → fail.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv2.Close()
	p2 := failProbe("http://127.0.0.1:1", "", nil)
	p2.MCPBaseURL = srv2.URL
	mustFail(t, "mcp 500", scenarioMCPHTTPRoundTrip(context.Background(), p2))

	// Endpoint unreachable → fail.
	p3 := failProbe("http://127.0.0.1:1", "", nil)
	p3.MCPBaseURL = "http://127.0.0.1:1/mcp"
	mustFail(t, "mcp unreachable", scenarioMCPHTTPRoundTrip(context.Background(), p3))
}

// sseMsg frames a JSON-RPC result as one SSE message event.
func sseMsg(id int, result string) string {
	return fmt.Sprintf("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}\n\n", id, result)
}

// mcpWhoamiStub answers tools/list with a whoami tool, and tools/call with the
// caller-supplied result JSON — to drive the whoami-result assertion branches.
func mcpWhoamiStub(t *testing.T, whoamiResult string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		w.Header().Set("Content-Type", "text/event-stream")
		if req.Method == "tools/list" {
			fmt.Fprint(w, sseMsg(req.ID, `{"tools":[{"name":"whoami"}]}`))
			return
		}
		fmt.Fprint(w, sseMsg(req.ID, whoamiResult))
	}))
}

func TestScenarioMCPHTTPRoundTrip_WhoamiBranches(t *testing.T) {
	cases := []struct {
		name   string
		result string
	}{
		{"isError true", `{"content":[{"type":"text","text":"boom"}],"isError":true}`},
		{"empty content", `{"content":[]}`},
		{"content but no text block", `{"content":[{"type":"image","data":"x"}]}`},
		{"no result object", `null`},
	}
	for _, tc := range cases {
		srv := mcpWhoamiStub(t, tc.result)
		p := failProbe("http://127.0.0.1:1", "", nil)
		p.MCPBaseURL = srv.URL
		mustFail(t, "whoami "+tc.name, scenarioMCPHTTPRoundTrip(context.Background(), p))
		srv.Close()
	}

	// JSON-RPC error envelope on the whoami call → fail.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		w.Header().Set("Content-Type", "text/event-stream")
		if req.Method == "tools/list" {
			fmt.Fprint(w, sseMsg(req.ID, `{"tools":[{"name":"whoami"}]}`))
			return
		}
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":-32603,\"message\":\"internal\"}}\n\n", req.ID)
	}))
	defer srv.Close()
	p := failProbe("http://127.0.0.1:1", "", nil)
	p.MCPBaseURL = srv.URL
	mustFail(t, "whoami json-rpc error", scenarioMCPHTTPRoundTrip(context.Background(), p))
}

func TestParseJSONRPCEnvelope(t *testing.T) {
	// application/json body (enableJsonResponse path).
	env, err := parseJSONRPCEnvelope([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`), "application/json")
	if err != nil || env["result"] == nil {
		t.Fatalf("json branch: env=%v err=%v", env, err)
	}

	// SSE with a ping/comment line, CRLF terminators, and a charset param — all
	// of which a real proxy/SDK may emit.
	sse := ":ping\r\nevent: message\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\r\n\r\n"
	env, err = parseJSONRPCEnvelope([]byte(sse), "text/event-stream; charset=utf-8")
	if err != nil || env["result"] == nil {
		t.Fatalf("sse+comment+crlf branch: env=%v err=%v", env, err)
	}

	// One JSON object split across two data: lines (joined with \n → valid JSON).
	split := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\ndata: \"result\":{\"ok\":true}}\n\n"
	env, err = parseJSONRPCEnvelope([]byte(split), "text/event-stream")
	if err != nil || env["result"] == nil {
		t.Fatalf("sse multi-data join: env=%v err=%v", env, err)
	}

	// Non-JSON body (e.g. an HTML error page from a proxy) → error, never a pass.
	if _, err := parseJSONRPCEnvelope([]byte("<html>bad gateway</html>"), "text/html"); err == nil {
		t.Error("html body: want decode error")
	}
	// SSE stream carrying no JSON-RPC message → error.
	if _, err := parseJSONRPCEnvelope([]byte(":keep-alive\n\nevent: ping\ndata: {}\n\n"), "text/event-stream"); err == nil {
		t.Error("sse without jsonrpc message: want error")
	}
}
