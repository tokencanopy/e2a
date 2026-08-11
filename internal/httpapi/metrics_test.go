package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

// metricsCount builds one tally with deliberately different observation and
// message counts so any place the handler reaches for the wrong grain shows up
// as a wrong number rather than a coincidentally equal one.
func metricsCount(code messagelifecycle.ReasonCode, observations, messages int64) messagelifecycle.ReasonCodeCount {
	definition, ok := messagelifecycle.Lookup(code)
	if !ok {
		panic("unknown reason code in test fixture: " + string(code))
	}
	return messagelifecycle.ReasonCodeCount{
		ReasonCode:   code,
		Stage:        definition.Stage,
		Outcome:      definition.Outcome,
		Retryable:    definition.Retryable,
		Observations: observations,
		Messages:     messages,
	}
}

func newMetricsServer(t *testing.T, metrics messagelifecycle.AgentMetrics, mutate ...func(*Deps)) (*Server, *[]string) {
	t.Helper()
	windows := []string{}
	deps := Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			if r.Header.Get("Authorization") != "Bearer good" {
				return nil, errors.New("unauthorized")
			}
			return &identity.User{ID: "u_1", Email: "owner@example.com"}, nil
		},
		GetAgent: func(_ context.Context, address string) (*identity.AgentIdentity, error) {
			switch address {
			case "agent@example.com", "other@example.com":
				return &identity.AgentIdentity{ID: address, UserID: "u_1", Email: address}, nil
			case "foreign@example.com":
				return &identity.AgentIdentity{ID: address, UserID: "u_2", Email: address}, nil
			}
			return nil, errors.New("not found")
		},
		CountAgentMetrics: func(_ context.Context, agentID string, start, end time.Time) (messagelifecycle.AgentMetrics, error) {
			windows = append(windows, agentID+"|"+start.Format(time.RFC3339)+"|"+end.Format(time.RFC3339))
			return metrics, nil
		},
	}
	for _, fn := range mutate {
		fn(&deps)
	}
	return New(deps), &windows
}

func metricsGET(t *testing.T, handler http.Handler, agent, query string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/v1/agents/"+url.PathEscape(agent)+"/metrics"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer good")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return resp.Code, body
}

func metricsSection(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	section, ok := body[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, body[key])
	}
	return section
}

// TestAgentMetricsSummaryUsesMessageGrain is the load-bearing test for the
// whole endpoint: the summary must be built from distinct-message counts. If it
// used observations, a single message the pipeline retried would lift a stage
// above the stage that feeds it and produce a delivery rate over 1.
func TestAgentMetricsSummaryUsesMessageGrain(t *testing.T) {
	srv, _ := newMetricsServer(t, messagelifecycle.AgentMetrics{
		MessagesInWindow:      10,
		MessagesWithLifecycle: 10,
		Counts: []messagelifecycle.ReasonCodeCount{
			metricsCount(messagelifecycle.ReasonAcceptanceOutboundAPI, 10, 10),
			// Retried nine times across two messages.
			metricsCount(messagelifecycle.ReasonSubmissionTemporaryFailure, 9, 2),
			metricsCount(messagelifecycle.ReasonSubmissionUpstreamAccepted, 8, 8),
			metricsCount(messagelifecycle.ReasonDeliveryRecipientServerAccepted, 6, 6),
			metricsCount(messagelifecycle.ReasonDeliveryPermanentBounce, 2, 2),
		},
	})

	status, body := metricsGET(t, srv, "agent@example.com", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	summary := metricsSection(t, body, "summary")
	if got := summary["accepted"].(float64); got != 10 {
		t.Errorf("accepted = %v, want 10", got)
	}
	if got := summary["submitted"].(float64); got != 8 {
		t.Errorf("submitted = %v, want 8", got)
	}
	if got := summary["delivered"].(float64); got != 6 {
		t.Errorf("delivered = %v, want 6", got)
	}

	rates := metricsSection(t, body, "rates")
	if got := rates["delivered_rate"].(float64); got != 0.6 {
		t.Errorf("delivered_rate = %v, want 0.6 (6 delivered / 10 accepted)", got)
	}
	// Denominated on submitted, not accepted — this is what makes the number
	// comparable to the provider thresholds that trigger account review.
	if got := rates["bounce_rate"].(float64); got != 0.25 {
		t.Errorf("bounce_rate = %v, want 0.25 (2 bounced / 8 submitted)", got)
	}

	// The retried code must still expose its attempt count in counters[].
	counters, ok := body["counters"].([]any)
	if !ok {
		t.Fatalf("counters = %#v, want array", body["counters"])
	}
	var found bool
	for _, raw := range counters {
		entry := raw.(map[string]any)
		if entry["reason_code"] != string(messagelifecycle.ReasonSubmissionTemporaryFailure) {
			continue
		}
		found = true
		if entry["observations"].(float64) != 9 || entry["messages"].(float64) != 2 {
			t.Errorf("temporary_failure counter = %#v, want observations 9 / messages 2", entry)
		}
		if entry["retryable"] != true {
			t.Error("temporary_failure must report retryable=true")
		}
	}
	if !found {
		t.Error("counters[] omitted submission.temporary_failure")
	}
}

// TestAgentMetricsKeepsLoopbackArrivalsOutOfTheSendDenominator.
//
// A loopback delivery writes acceptance.outbound_api on the sent message and
// acceptance.local_loopback on the arriving copy — the latter with direction
// "inbound", at every writer and both reconstruction paths. Counting it as an
// accepted SEND inflates the denominator of delivered_rate and
// suppression_block_rate, so an agent talking to another agent on the same
// deployment reads as though half its mail vanished.
func TestAgentMetricsKeepsLoopbackArrivalsOutOfTheSendDenominator(t *testing.T) {
	// A MIXED account: one external send that was delivered, plus one
	// agent-to-agent pair. The earlier fixture gave a single message both a
	// loopback submission and a recipient-server delivery, which the pipeline
	// never produces — a loopback send has no recipient server to accept it.
	srv, _ := newMetricsServer(t, messagelifecycle.AgentMetrics{
		MessagesInWindow:      3,
		MessagesWithLifecycle: 3,
		Counts: []messagelifecycle.ReasonCodeCount{
			metricsCount(messagelifecycle.ReasonAcceptanceOutboundAPI, 2, 2),
			metricsCount(messagelifecycle.ReasonAcceptanceLocalLoopback, 1, 1),
			metricsCount(messagelifecycle.ReasonSubmissionUpstreamAccepted, 1, 1),
			metricsCount(messagelifecycle.ReasonSubmissionLocalLoopbackAccepted, 1, 1),
			metricsCount(messagelifecycle.ReasonDeliveryRecipientServerAccepted, 1, 1),
		},
	})

	status, body := metricsGET(t, srv, "agent@example.com", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	summary := metricsSection(t, body, "summary")
	if got := summary["accepted"].(float64); got != 2 {
		t.Errorf("accepted = %v, want 2: the loopback ARRIVAL is not a send", got)
	}
	if got := summary["received"].(float64); got != 1 {
		t.Errorf("received = %v, want 1: the loopback arrival is inbound mail", got)
	}
	// One external send, one delivery — the loopback pair leaves the rate
	// entirely, so this reads 1.0 rather than 0.5.
	if got := metricsSection(t, body, "rates")["delivered_rate"].(float64); got != 1 {
		t.Errorf("delivered_rate = %v, want 1", got)
	}
}

// TestAgentMetricsExcludesLoopbackFromRates: agent-to-agent mail never reaches
// a recipient server, so it can neither be delivered nor bounce. Leaving it in
// the denominator made e2a's flagship flow report a delivery rate near zero.
func TestAgentMetricsExcludesLoopbackFromRates(t *testing.T) {
	srv, _ := newMetricsServer(t, messagelifecycle.AgentMetrics{
		MessagesInWindow:      20,
		MessagesWithLifecycle: 20,
		Counts: []messagelifecycle.ReasonCodeCount{
			// 10 sends: 6 external, 4 loopback.
			metricsCount(messagelifecycle.ReasonAcceptanceOutboundAPI, 10, 10),
			metricsCount(messagelifecycle.ReasonSubmissionUpstreamAccepted, 6, 6),
			metricsCount(messagelifecycle.ReasonSubmissionLocalLoopbackAccepted, 4, 4),
			metricsCount(messagelifecycle.ReasonDeliveryRecipientServerAccepted, 6, 6),
		},
	})

	status, body := metricsGET(t, srv, "agent@example.com", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	summary := metricsSection(t, body, "summary")
	if got := summary["loopback"].(float64); got != 4 {
		t.Errorf("loopback = %v, want 4", got)
	}
	// 6 delivered of 6 external sends — not 6 of 10.
	if got := metricsSection(t, body, "rates")["delivered_rate"].(float64); got != 1 {
		t.Errorf("delivered_rate = %v, want 1: loopback must leave the denominator", got)
	}
}

// An account that ONLY talks agent-to-agent has no external traffic to rate,
// so the rate is null rather than a misleading 0%%.
func TestAgentMetricsRateIsNullForPureLoopbackTraffic(t *testing.T) {
	srv, _ := newMetricsServer(t, messagelifecycle.AgentMetrics{
		MessagesInWindow:      10,
		MessagesWithLifecycle: 10,
		Counts: []messagelifecycle.ReasonCodeCount{
			metricsCount(messagelifecycle.ReasonAcceptanceOutboundAPI, 5, 5),
			metricsCount(messagelifecycle.ReasonSubmissionLocalLoopbackAccepted, 5, 5),
			metricsCount(messagelifecycle.ReasonAcceptanceLocalLoopback, 5, 5),
		},
	})

	_, body := metricsGET(t, srv, "agent@example.com", "")
	rates := metricsSection(t, body, "rates")
	if rates["delivered_rate"] != nil {
		t.Errorf("delivered_rate = %#v, want null: there is no external mail to rate", rates["delivered_rate"])
	}
	if rates["bounce_rate"] != nil {
		t.Errorf("bounce_rate = %#v, want null", rates["bounce_rate"])
	}
	// The traffic is still visible in the counters — only the rate ignores it.
	if got := metricsSection(t, body, "summary")["received"].(float64); got != 5 {
		t.Errorf("received = %v, want 5", got)
	}
}

// TestAgentMetricsRatesAreNullWithoutTraffic: zero-over-zero must not render
// as 0.0, which reads identically to a total delivery failure.
func TestAgentMetricsRatesAreNullWithoutTraffic(t *testing.T) {
	srv, _ := newMetricsServer(t, messagelifecycle.AgentMetrics{})

	status, body := metricsGET(t, srv, "agent@example.com", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	rates := metricsSection(t, body, "rates")
	for _, key := range []string{"delivered_rate", "bounce_rate", "complaint_rate", "suppression_block_rate"} {
		value, present := rates[key]
		if !present {
			t.Errorf("%s missing from rates", key)
			continue
		}
		if value != nil {
			t.Errorf("%s = %#v, want null when the denominator is zero", key, value)
		}
	}
	if counters, ok := body["counters"].([]any); !ok || len(counters) != 0 {
		t.Errorf("counters = %#v, want an empty array", body["counters"])
	}
}

// TestAgentMetricsSurfacesLedgerCoverage: an undercount must be visible in the
// response rather than presented as a delivery collapse.
func TestAgentMetricsSurfacesLedgerCoverage(t *testing.T) {
	srv, _ := newMetricsServer(t, messagelifecycle.AgentMetrics{
		MessagesInWindow:          100,
		MessagesWithLifecycle:     40,
		ReconstructedObservations: 7,
		Counts: []messagelifecycle.ReasonCodeCount{
			metricsCount(messagelifecycle.ReasonAcceptanceOutboundAPI, 40, 40),
		},
	})

	status, body := metricsGET(t, srv, "agent@example.com", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	if got := body["messages_in_window"].(float64); got != 100 {
		t.Errorf("messages_in_window = %v, want 100", got)
	}
	if got := body["messages_with_lifecycle"].(float64); got != 40 {
		t.Errorf("messages_with_lifecycle = %v, want 40", got)
	}
	if got := body["reconstructed_observations"].(float64); got != 7 {
		t.Errorf("reconstructed_observations = %v, want 7", got)
	}
}

func TestAgentMetricsDefaultsToThirtyDayWindow(t *testing.T) {
	srv, windows := newMetricsServer(t, messagelifecycle.AgentMetrics{})
	before := time.Now().UTC().Truncate(time.Second)

	status, body := metricsGET(t, srv, "agent@example.com", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	if len(*windows) != 1 {
		t.Fatalf("counter calls = %v", *windows)
	}

	start, err := time.Parse(time.RFC3339, body["start"].(string))
	if err != nil {
		t.Fatal(err)
	}
	end, err := time.Parse(time.RFC3339, body["end"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if span := end.Sub(start); span != 30*24*time.Hour {
		t.Errorf("default window = %s, want 720h", span)
	}
	if end.Before(before) {
		t.Errorf("default end = %s, want at or after the request instant %s", end, before)
	}
}

func TestAgentMetricsEchoesExplicitWindow(t *testing.T) {
	srv, windows := newMetricsServer(t, messagelifecycle.AgentMetrics{})
	status, body := metricsGET(t, srv, "agent@example.com",
		"?start=2026-07-01T00:00:00Z&end=2026-07-08T00:00:00Z")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	if body["start"] != "2026-07-01T00:00:00Z" || body["end"] != "2026-07-08T00:00:00Z" {
		t.Errorf("window echo = %v..%v", body["start"], body["end"])
	}
	want := "agent@example.com|2026-07-01T00:00:00Z|2026-07-08T00:00:00Z"
	if len(*windows) != 1 || (*windows)[0] != want {
		t.Errorf("store call = %v, want %q", *windows, want)
	}
}

func TestAgentMetricsRejectsInvalidWindows(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"end equals start", "?start=2026-07-01T00:00:00Z&end=2026-07-01T00:00:00Z"},
		{"end before start", "?start=2026-07-08T00:00:00Z&end=2026-07-01T00:00:00Z"},
		{"window too wide", "?start=2026-01-01T00:00:00Z&end=2026-07-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, windows := newMetricsServer(t, messagelifecycle.AgentMetrics{})
			status, body := metricsGET(t, srv, "agent@example.com", tc.query)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %#v", status, body)
			}
			if len(*windows) != 0 {
				t.Errorf("store was queried despite an invalid window: %v", *windows)
			}
		})
	}
}

func TestAgentMetricsHidesForeignAgentAsNotFound(t *testing.T) {
	srv, windows := newMetricsServer(t, messagelifecycle.AgentMetrics{})
	status, _ := metricsGET(t, srv, "foreign@example.com", "")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 so another account's agents cannot be enumerated", status)
	}
	if len(*windows) != 0 {
		t.Errorf("store was queried for a foreign agent: %v", *windows)
	}
}

// TestAgentMetricsPinsAgentScopedCredential: an agent-scoped key must not read
// a sibling agent's numbers, even under the same owner.
func TestAgentMetricsPinsAgentScopedCredential(t *testing.T) {
	srv, windows := newMetricsServer(t, messagelifecycle.AgentMetrics{}, func(d *Deps) {
		d.PrincipalAuthenticator = func(r *http.Request) (*identity.Principal, error) {
			if r.Header.Get("Authorization") != "Bearer good" {
				return nil, errors.New("unauthorized")
			}
			return &identity.Principal{
				User:    &identity.User{ID: "u_1", Email: "owner@example.com"},
				Scope:   identity.ScopeAgent,
				AgentID: "agent@example.com",
			}, nil
		}
	})

	if status, _ := metricsGET(t, srv, "agent@example.com", ""); status != http.StatusOK {
		t.Fatalf("own agent status = %d, want 200", status)
	}
	if status, _ := metricsGET(t, srv, "other@example.com", ""); status != http.StatusForbidden {
		t.Fatalf("sibling agent status = %d, want 403", status)
	}
	if len(*windows) != 1 {
		t.Errorf("store calls = %v, want only the permitted agent's read", *windows)
	}
}

func TestAgentMetricsReportsNotImplementedWithoutStore(t *testing.T) {
	srv, _ := newMetricsServer(t, messagelifecycle.AgentMetrics{}, func(d *Deps) {
		d.CountAgentMetrics = nil
	})
	status, body := metricsGET(t, srv, "agent@example.com", "")
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
}

func TestAgentMetricsMapsStoreFailureToInternalError(t *testing.T) {
	srv, _ := newMetricsServer(t, messagelifecycle.AgentMetrics{}, func(d *Deps) {
		d.CountAgentMetrics = func(context.Context, string, time.Time, time.Time) (messagelifecycle.AgentMetrics, error) {
			return messagelifecycle.AgentMetrics{}, errors.New("boom")
		}
	})
	status, body := metricsGET(t, srv, "agent@example.com", "")
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	// The upstream failure text must not reach the caller.
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "boom") {
		t.Errorf("response leaked the internal error: %s", raw)
	}
}
