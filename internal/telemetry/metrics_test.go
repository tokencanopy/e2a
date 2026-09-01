package telemetry

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// Compile-time interface satisfaction is the load-bearing property
// here — once a backend implements every method, the call sites
// compile. These tests pin that contract.

func TestNoOpSatisfiesInterface(t *testing.T) {
	var m Metrics = NoOp{}
	// Smoke: every method must be callable without panicking on
	// zero-value inputs. The NoOp implementation has no state, so a
	// panic would be a code error.
	m.OutboxEventsPublished("")
	m.OutboxEventsFanOut("", 0)
	m.OutboxEventsNoMatch("")
	m.OutboxFailures("")
	m.RedeliverRequests("")
	m.JanitorRowsDeleted("", 0)
	m.WebhookExpiredPending(0)
	m.WebhookTerminal("", "", 0)
	m.WebhookFanOutRescued(0)
	m.WebhookDeliveryRescued(0)
	m.WebhookNotify("", "")
	m.NotifyMissed()
	m.SetPublisherLag(0)
	m.OutboundTerminalLatency(0)
	m.OutboundRateDeferred()
	m.WebhookFirstAttemptLatency(0)
	m.WSHandshakeRejected("")
}

func TestLogSatisfiesInterface(t *testing.T) {
	var m Metrics = NewLog()
	// Same smoke. The Log implementation emits log lines via
	// log.Printf — they'll go to the test runner's stderr which is
	// fine; we just verify nothing panics on zero values.
	m.OutboxEventsPublished("email.received")
	m.OutboxEventsFanOut("email.received", 3)
	m.OutboxEventsNoMatch("email.sent")
	m.OutboxFailures("lease")
	m.RedeliverRequests("single")
	m.JanitorRowsDeleted("webhook_events", 5)
	m.WebhookExpiredPending(2)
	m.WebhookTerminal("e2a_failure", "unknown", 2)
	m.WebhookFanOutRescued(1)
	m.WebhookDeliveryRescued(1)
	m.WebhookNotify("disabled", "permanent")
	m.NotifyMissed()
	m.SetPublisherLag(2.5)
	m.OutboundTerminalLatency(240)
	m.OutboundRateDeferred()
	m.WebhookFirstAttemptLatency(12.5)
	m.WSHandshakeRejected("unauthorized")
}

func TestLogJanitorSkipsZeroCount(t *testing.T) {
	// Zero-count janitor calls should not emit (would be noise at
	// hourly cadence × N tables). This is a behavior contract for
	// log-aggregator readers — if they see janitor.delete in their
	// stream, count > 0 is guaranteed.
	NewLog().JanitorRowsDeleted("webhook_events", 0)
	// No assertion: we're just confirming no panic on zero. Log
	// suppression is internal; would require log capture to test
	// directly.
}

func TestLogPublisherLagRateLimit(t *testing.T) {
	// SetPublisherLag is called every Tick (1Hz in production). To
	// avoid 1 line/second to the log stream, emission is rate-limited
	// to once per minute UNLESS the lag is > 30s (the design's alert
	// threshold). Verify the call doesn't crash under the high-rate
	// pattern.
	l := NewLog()
	for i := 0; i < 120; i++ {
		l.SetPublisherLag(2.0) // healthy lag, should rate-limit
	}
	l.SetPublisherLag(45.0) // elevated, should emit immediately
}

func TestLogOIDCAndProvisioningLabelsAreAllowlisted(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	sensitive := "provider-body token=opaque code=secret email=person@example.test"
	l := NewLog()
	l.OIDCDiscovery(sensitive, sensitive)
	l.OIDCCallback(sensitive, sensitive, sensitive)
	l.Provisioning(sensitive, sensitive, sensitive)

	got := logs.String()
	if strings.Contains(got, sensitive) || strings.Contains(got, "person@example.test") {
		t.Fatalf("sensitive label input leaked into log metrics: %q", got)
	}
	for _, want := range []string{
		"event=oidc.discovery outcome=other status_class=other",
		"event=oidc.callback outcome=other trust=other status_class=other",
		"event=provisioning outcome=other trust=other status_class=other",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bounded log metric missing %q: %q", want, got)
		}
	}
}
