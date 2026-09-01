package telemetry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape renders the Prom backend's exposition output as text so tests can
// assert on emitted series without depending on client_golang internals.
func scrape(t *testing.T, p *Prom) string {
	t.Helper()
	body := scrapeRaw(t, p)
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Count(line, `build="unknown"`) != 1 {
			t.Errorf("sample missing exactly one default build label: %s", line)
		}
	}
	body = strings.ReplaceAll(body, `{build="unknown",`, "{")
	body = strings.ReplaceAll(body, `,build="unknown"}`, "}")
	body = strings.ReplaceAll(body, `{build="unknown"}`, "")
	return body
}

func scrapeRaw(t *testing.T, p *Prom) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	p.Handler().ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read exposition body: %v", err)
	}
	return string(body)
}

func TestPromAddsBuildLabelToEverySample(t *testing.T) {
	p := NewProm("1.3.0")
	p.HTTPRequest("GET", "/v1/agents", "2xx", 0.01)
	p.SMTPInbound("accepted", 0.02)
	p.SetPublisherLag(0)

	for _, line := range strings.Split(scrapeRaw(t, p), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Count(line, `build="1.3.0"`) != 1 {
			t.Errorf("sample missing exactly one build label: %s", line)
		}
	}
}

func TestPromHandlerOverHTTPIncludesBuildLabel(t *testing.T) {
	p := NewProm("v1.3.0")
	p.SetPublisherLag(0)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(body), `e2a_webhook_publisher_lag_seconds{build="v1.3.0"} 0`) {
		t.Fatalf("wire exposition missing build label:\n%s", body)
	}
}

func TestNormalizeBuildLabel(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", "unknown"},
		{"  ", "unknown"},
		{"v1.3.0", "v1.3.0"},
		{"sha-abc123", "sha-abc123"},
		{"release\nsecret", "release_secret"},
	} {
		if got := NormalizeBuildLabel(tc.in); got != tc.want {
			t.Errorf("NormalizeBuildLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPromSatisfiesInterface(t *testing.T) {
	var _ Metrics = NewProm("")
}

func TestPromEmitsHTTPSeries(t *testing.T) {
	p := NewProm("")
	p.HTTPRequest("GET", "/v1/agents/{email}", "2xx", 0.042)
	p.HTTPRequest("GET", "/v1/agents/{email}", "2xx", 0.010)
	p.HTTPRequest("POST", "/v1/agents/{email}/messages", "5xx", 1.5)

	out := scrape(t, p)
	if !strings.Contains(out, `e2a_http_requests_total{method="GET",route="/v1/agents/{email}",status_class="2xx"} 2`) {
		t.Fatalf("missing GET counter series in exposition:\n%s", out)
	}
	if !strings.Contains(out, `e2a_http_requests_total{method="POST",route="/v1/agents/{email}/messages",status_class="5xx"} 1`) {
		t.Fatalf("missing POST 5xx counter series in exposition:\n%s", out)
	}
	if !strings.Contains(out, `e2a_http_request_duration_seconds_count{method="GET",route="/v1/agents/{email}"} 2`) {
		t.Fatalf("missing duration histogram series in exposition:\n%s", out)
	}
}

func TestPromEmitsAllOIDCAndProvisioningOutcomeCategories(t *testing.T) {
	p := NewProm("")

	for _, outcome := range []string{"success", "issuer_unavailable", "discovery_invalid"} {
		p.OIDCDiscovery(outcome, "5xx")
	}
	for _, outcome := range []string{
		"success", "discovery_unavailable", "state_invalid", "provider_rejected", "provider_failed",
		"response_invalid", "token_exchange_failed", "id_token_invalid",
		"claim_invalid", "unknown_user", "session_failed", "post_login_failed",
	} {
		p.OIDCCallback(outcome, "trusted", "4xx")
	}
	for _, outcome := range []string{
		"created", "existing", "rejected", "internal_error", "not_configured",
		"malformed_request", "unauthorized",
	} {
		p.Provisioning(outcome, "authenticated", "2xx")
	}

	out := scrape(t, p)
	for _, want := range []string{
		`e2a_oidc_discovery_total{outcome="success",status_class="5xx"} 1`,
		`e2a_oidc_discovery_total{outcome="issuer_unavailable",status_class="5xx"} 1`,
		`e2a_oidc_discovery_total{outcome="discovery_invalid",status_class="5xx"} 1`,
		`e2a_oidc_callback_total{outcome="success",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="discovery_unavailable",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="state_invalid",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="provider_rejected",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="provider_failed",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="response_invalid",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="token_exchange_failed",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="id_token_invalid",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="claim_invalid",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="unknown_user",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="session_failed",status_class="4xx",trust="trusted"} 1`,
		`e2a_oidc_callback_total{outcome="post_login_failed",status_class="4xx",trust="trusted"} 1`,
		`e2a_provisioning_total{outcome="created",status_class="2xx",trust="authenticated"} 1`,
		`e2a_provisioning_total{outcome="existing",status_class="2xx",trust="authenticated"} 1`,
		`e2a_provisioning_total{outcome="rejected",status_class="2xx",trust="authenticated"} 1`,
		`e2a_provisioning_total{outcome="internal_error",status_class="2xx",trust="authenticated"} 1`,
		`e2a_provisioning_total{outcome="not_configured",status_class="2xx",trust="authenticated"} 1`,
		`e2a_provisioning_total{outcome="malformed_request",status_class="2xx",trust="authenticated"} 1`,
		`e2a_provisioning_total{outcome="unauthorized",status_class="2xx",trust="authenticated"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series %q in exposition", want)
		}
	}
	if t.Failed() {
		t.Logf("exposition:\n%s", out)
	}
}

func TestPromEmitsSMTPOutboundWebhookWSSeries(t *testing.T) {
	p := NewProm("")
	p.SMTPInbound("accepted", 0.2)
	p.SMTPInbound("tempfail", 0.1)
	p.SMTPInbound("rejected_unknown_recipient", 0)
	p.OutboundQueueWait(3.5)
	p.OutboundTerminal("sent")
	p.OutboundTerminal("failed_provider")
	p.OutboundTerminal("failed_cancelled")
	p.OutboundTerminalLatency(240)
	p.OutboundAttempt("success", 0.8)
	p.OutboundRateDeferred()
	p.WebhookAttempt("delivered", "2xx", 0.3)
	p.WebhookAttempt("retryable_failure", "5xx", 0.2)
	p.WebhookTerminal("delivered", "initial", 1)
	p.WebhookTerminal("e2a_failure", "unknown", 2)
	p.WebhookNotify("warning", "sent")
	p.WebhookNotify("disabled", "permanent")
	p.WebhookNotify("disabled", "skipped")
	p.WebhookFirstAttemptLatency(12.5)
	p.WSConnected()
	p.WSHandshakeRejected("unauthorized")
	p.WSHandshakeRejected("forbidden")
	p.WSHandshakeRejected("internal_error")
	p.WSDisconnected("ping_timeout")
	p.WSDrained(7)
	p.WSSendFailure()
	p.SetWSActive(3)
	p.InboundProcess("processed", 0.4)
	p.SetQueueDepth("outbound", "available", 12)
	p.SetQueueOldestAge("outbound", 45.5)

	out := scrape(t, p)
	for _, want := range []string{
		`e2a_smtp_inbound_total{outcome="accepted"} 1`,
		`e2a_smtp_inbound_total{outcome="tempfail"} 1`,
		`e2a_smtp_inbound_total{outcome="rejected_unknown_recipient"} 1`,
		`e2a_outbound_terminal_total{outcome="sent"} 1`,
		`e2a_outbound_terminal_total{outcome="failed_provider"} 1`,
		`e2a_outbound_terminal_total{outcome="failed_cancelled"} 1`,
		`e2a_outbound_terminal_latency_seconds_count 1`,
		`e2a_outbound_attempts_total{outcome="success"} 1`,
		`e2a_webhook_notify_total{kind="warning",outcome="sent"} 1`,
		`e2a_webhook_notify_total{kind="disabled",outcome="permanent"} 1`,
		`e2a_webhook_notify_total{kind="disabled",outcome="skipped"} 1`,
		`e2a_outbound_rate_deferred_total 1`,
		`e2a_webhook_attempts_total{outcome="delivered",status_class="2xx"} 1`,
		`e2a_webhook_attempts_total{outcome="retryable_failure",status_class="5xx"} 1`,
		`e2a_webhook_delivery_terminal_total{outcome="delivered",scope="initial"} 1`,
		`e2a_webhook_delivery_terminal_total{outcome="e2a_failure",scope="unknown"} 2`,
		`e2a_webhook_first_attempt_latency_seconds_count 1`,
		`e2a_ws_connects_total 1`,
		`e2a_ws_handshake_rejected_total{reason="unauthorized"} 1`,
		`e2a_ws_handshake_rejected_total{reason="forbidden"} 1`,
		`e2a_ws_handshake_rejected_total{reason="internal_error"} 1`,
		`e2a_ws_disconnects_total{reason="ping_timeout"} 1`,
		`e2a_ws_drained_messages_total 7`,
		`e2a_ws_send_failures_total 1`,
		`e2a_ws_connections_active 3`,
		`e2a_inbound_process_total{outcome="processed"} 1`,
		`e2a_queue_depth{queue="outbound",state="available"} 12`,
		`e2a_queue_oldest_age_seconds{queue="outbound"} 45.5`,
		`e2a_outbound_queue_wait_seconds_count 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series %q in exposition", want)
		}
	}
	if t.Failed() {
		t.Logf("exposition:\n%s", out)
	}
}

func TestPromLatencyHistogramsExposeExactSLOThresholdBuckets(t *testing.T) {
	p := NewProm("")
	p.HTTPRequest("GET", "/v1/agents", "2xx", 3)
	p.SMTPInbound("accepted", 3)

	out := scrape(t, p)
	for _, want := range []string{
		`e2a_http_request_duration_seconds_bucket{method="GET",route="/v1/agents",le="0.75"}`,
		`e2a_smtp_inbound_duration_seconds_bucket{le="2"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing exact SLO threshold bucket %q", want)
		}
	}
	if t.Failed() {
		t.Logf("exposition:\n%s", out)
	}
}

func TestPromEmitsLegacyOutboxSeries(t *testing.T) {
	p := NewProm("")
	p.OutboxEventsPublished("email.received")
	p.OutboxEventsFanOut("email.received", 3)
	p.OutboxEventsNoMatch("email.sent")
	p.OutboxFailures("lease")
	p.RedeliverRequests("single")
	p.JanitorRowsDeleted("webhook_events", 5)
	p.NotifyMissed()
	p.SetPublisherLag(2.5)

	out := scrape(t, p)
	for _, want := range []string{
		`e2a_outbox_events_published_total{type="email.received"} 1`,
		`e2a_outbox_events_fanout_total{type="email.received"} 1`,
		`e2a_outbox_fanout_matched_total{type="email.received"} 3`,
		`e2a_outbox_events_nomatch_total{type="email.sent"} 1`,
		`e2a_outbox_failures_total{stage="lease"} 1`,
		`e2a_redeliver_requests_total{scope="single"} 1`,
		`e2a_janitor_rows_deleted_total{table="webhook_events"} 5`,
		`e2a_notify_missed_total 1`,
		`e2a_webhook_publisher_lag_seconds 2.5`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series %q in exposition", want)
		}
	}
	if t.Failed() {
		t.Logf("exposition:\n%s", out)
	}
}

// Label cardinality boundary: unknown enum values must collapse to "other" so
// a bug (or attacker-influenced string) can never mint unbounded series or
// leak message content / addresses / secrets into the metrics surface.
func TestPromNormalizesUnknownLabelValues(t *testing.T) {
	p := NewProm("")
	secret := "hunter2-api-key"
	addr := "victim@example.com"
	p.SMTPInbound(addr, 0.1)                // raw address must not become a label
	p.OutboundTerminal("weird_new_outcome") // unknown enum
	p.WebhookAttempt(secret, "banana", 0.1) // junk outcome + junk status class
	p.WebhookTerminal(secret, addr, 1)      // junk outcome + scope
	p.WebhookNotify(addr, secret)           // junk notify kind + outcome
	p.WSDisconnected("some very long free text reason with details")
	p.WSHandshakeRejected(addr) // raw address must not become a rejection-reason label
	p.OIDCDiscovery(addr, secret)
	p.OIDCCallback(secret, addr, "7xx")
	p.Provisioning(addr, secret, "banana")
	p.InboundProcess(secret, 0)
	p.SetQueueDepth("attacker_queue", "exploded", 1)
	p.HTTPRequest("PROPFIND", "/v1/agents/{email}", "7xx", 0.1) // unknown method + class
	p.ThreadResolution(secret, 1)
	p.ThreadHeaderParseFailure("not-a-header")
	p.SetThreadNullMessages(addr, 2)
	p.SetThreadInvariantViolations(secret, 3)
	p.SetThreadRelationshipPercent(addr, 50)

	out := scrape(t, p)
	if strings.Contains(out, secret) || strings.Contains(out, addr) {
		t.Fatalf("raw label value leaked into exposition:\n%s", out)
	}
	for _, want := range []string{
		`e2a_smtp_inbound_total{outcome="other"} 1`,
		`e2a_outbound_terminal_total{outcome="other"} 1`,
		`e2a_webhook_attempts_total{outcome="other",status_class="other"} 1`,
		`e2a_webhook_delivery_terminal_total{outcome="other",scope="other"} 1`,
		`e2a_webhook_notify_total{kind="other",outcome="other"} 1`,
		`e2a_ws_disconnects_total{reason="other"} 1`,
		`e2a_ws_handshake_rejected_total{reason="other"} 1`,
		`e2a_oidc_discovery_total{outcome="other",status_class="other"} 1`,
		`e2a_oidc_callback_total{outcome="other",status_class="other",trust="other"} 1`,
		`e2a_provisioning_total{outcome="other",status_class="other",trust="other"} 1`,
		`e2a_inbound_process_total{outcome="other"} 1`,
		`e2a_queue_depth{queue="other",state="other"} 1`,
		`e2a_http_requests_total{method="other",route="/v1/agents/{email}",status_class="other"} 1`,
		`e2a_thread_resolution_total{source="other"} 1`,
		`e2a_thread_header_parse_failures_total{header="other"} 1`,
		`e2a_thread_null_messages{age_bucket="other"} 2`,
		`e2a_thread_invariant_violations{kind="other"} 3`,
		`e2a_thread_relationship_percent{kind="other"} 50`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing normalized series %q in exposition", want)
		}
	}
	if t.Failed() {
		t.Logf("exposition:\n%s", out)
	}
}

func TestPromEmitsThreadIdentitySeries(t *testing.T) {
	p := NewProm("")
	p.ThreadResolution("rfc_in_reply_to", 2)
	p.ThreadHeaderParseFailure("references")
	p.ThreadResolution("lazy_legacy_anchor", 1)
	p.SetThreadNullMessages("lt_1h", 3)
	p.SetThreadInvariantViolations("dangling_parent", 4)
	p.SetThreadRelationshipPercent("threads_multi_conversation", 25)

	out := scrape(t, p)
	for _, want := range []string{
		`e2a_thread_resolution_total{source="rfc_in_reply_to"} 2`,
		`e2a_thread_resolution_total{source="lazy_legacy_anchor"} 1`,
		`e2a_thread_header_parse_failures_total{header="references"} 1`,
		`e2a_thread_null_messages{age_bucket="lt_1h"} 3`,
		`e2a_thread_invariant_violations{kind="dangling_parent"} 4`,
		`e2a_thread_relationship_percent{kind="threads_multi_conversation"} 25`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing thread identity series %q in exposition", want)
		}
	}
	if t.Failed() {
		t.Logf("exposition:\n%s", out)
	}
}

// Route label cardinality cap: routes come from chi route patterns (bounded by
// construction), but the backend still enforces a hard cap so a routing bug
// can't blow up series count. Past the cap, new route values collapse to
// "other".
func TestPromRouteCardinalityCap(t *testing.T) {
	p := NewProm("")
	for i := 0; i < maxRouteSeries+50; i++ {
		p.HTTPRequest("GET", "/v1/synthetic/"+strings.Repeat("x", 1+i%7)+string(rune('a'+i%26))+itoa(i), "2xx", 0.01)
	}
	out := scrape(t, p)
	distinct := strings.Count(out, "e2a_http_requests_total{")
	if distinct > maxRouteSeries+1 { // +1 for the "other" bucket
		t.Fatalf("route cardinality cap not enforced: %d series > cap %d", distinct, maxRouteSeries+1)
	}
	if !strings.Contains(out, `route="other"`) {
		t.Fatalf("overflow routes did not collapse to \"other\"")
	}
}

// Type-label cardinality cap for legacy outbox metrics: event types are a
// server-defined catalog, but enforce the same overflow guard.
func TestPromEventTypeCardinalityCap(t *testing.T) {
	p := NewProm("")
	for i := 0; i < maxTypeSeries+20; i++ {
		p.OutboxEventsPublished("synthetic.event." + itoa(i))
	}
	out := scrape(t, p)
	distinct := strings.Count(out, "e2a_outbox_events_published_total{")
	if distinct > maxTypeSeries+1 {
		t.Fatalf("type cardinality cap not enforced: %d series > cap %d", distinct, maxTypeSeries+1)
	}
	if !strings.Contains(out, `type="other"`) {
		t.Fatalf("overflow event types did not collapse to \"other\"")
	}
}

func itoa(i int) string {
	// tiny local helper to avoid strconv import noise in table strings
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
