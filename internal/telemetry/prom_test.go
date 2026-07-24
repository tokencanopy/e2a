package telemetry

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape renders the Prom backend's exposition output as text so tests can
// assert on emitted series without depending on client_golang internals.
func scrape(t *testing.T, p *Prom) string {
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

func TestPromSatisfiesInterface(t *testing.T) {
	var _ Metrics = NewProm()
}

func TestPromEmitsHTTPSeries(t *testing.T) {
	p := NewProm()
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

func TestPromEmitsSMTPOutboundWebhookWSSeries(t *testing.T) {
	p := NewProm()
	p.SMTPInbound("accepted", 0.2)
	p.SMTPInbound("tempfail", 0.1)
	p.SMTPInbound("rejected_unknown_recipient", 0)
	p.OutboundQueueWait(3.5)
	p.OutboundTerminal("sent")
	p.OutboundTerminal("failed_provider")
	p.OutboundTerminal("failed_cancelled")
	p.OutboundTerminalLatency(240)
	p.OutboundAttempt("success", 0.8)
	p.WebhookAttempt("delivered", "2xx", 0.3)
	p.WebhookAttempt("retryable_failure", "5xx", 0.2)
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
		`e2a_webhook_attempts_total{outcome="delivered",status_class="2xx"} 1`,
		`e2a_webhook_attempts_total{outcome="retryable_failure",status_class="5xx"} 1`,
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

func TestPromEmitsLegacyOutboxSeries(t *testing.T) {
	p := NewProm()
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
	p := NewProm()
	secret := "hunter2-api-key"
	addr := "victim@example.com"
	p.SMTPInbound(addr, 0.1)                // raw address must not become a label
	p.OutboundTerminal("weird_new_outcome") // unknown enum
	p.WebhookAttempt(secret, "banana", 0.1) // junk outcome + junk status class
	p.WSDisconnected("some very long free text reason with details")
	p.WSHandshakeRejected(addr) // raw address must not become a rejection-reason label
	p.InboundProcess(secret, 0)
	p.SetQueueDepth("attacker_queue", "exploded", 1)
	p.HTTPRequest("PROPFIND", "/v1/agents/{email}", "7xx", 0.1) // unknown method + class

	out := scrape(t, p)
	if strings.Contains(out, secret) || strings.Contains(out, addr) {
		t.Fatalf("raw label value leaked into exposition:\n%s", out)
	}
	for _, want := range []string{
		`e2a_smtp_inbound_total{outcome="other"} 1`,
		`e2a_outbound_terminal_total{outcome="other"} 1`,
		`e2a_webhook_attempts_total{outcome="other",status_class="other"} 1`,
		`e2a_ws_disconnects_total{reason="other"} 1`,
		`e2a_ws_handshake_rejected_total{reason="other"} 1`,
		`e2a_inbound_process_total{outcome="other"} 1`,
		`e2a_queue_depth{queue="other",state="other"} 1`,
		`e2a_http_requests_total{method="other",route="/v1/agents/{email}",status_class="other"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing normalized series %q in exposition", want)
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
	p := NewProm()
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
	p := NewProm()
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
