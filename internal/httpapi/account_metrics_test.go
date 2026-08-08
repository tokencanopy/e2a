package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/webhook"
)

func newAccountMetricsServer(t *testing.T, metrics messagelifecycle.AccountMetrics, mutate ...func(*Deps)) (*Server, *[]string) {
	t.Helper()
	calls := []string{}
	deps := Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			if r.Header.Get("Authorization") != "Bearer good" {
				return nil, errors.New("unauthorized")
			}
			return &identity.User{ID: "u_1", Email: "owner@example.com"}, nil
		},
		CountAccountMetrics: func(_ context.Context, userID string, start, end time.Time, groupByAgent bool) (messagelifecycle.AccountMetrics, error) {
			calls = append(calls, userID+"|"+start.Format(time.RFC3339)+"|"+end.Format(time.RFC3339)+"|group="+boolText(groupByAgent))
			return metrics, nil
		},
	}
	for _, fn := range mutate {
		fn(&deps)
	}
	return New(deps), &calls
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func accountMetricsGET(t *testing.T, handler http.Handler, query string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/v1/metrics"+query, nil)
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

// TestAccountMetricsSharesTheAgentDenominators is the reason this operation
// exists rather than letting the dashboard sum per-agent calls itself: the
// account rates must come from the same derivation the per-agent route uses.
func TestAccountMetricsSharesTheAgentDenominators(t *testing.T) {
	srv, _ := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{
		Totals: messagelifecycle.AgentMetrics{
			MessagesInWindow:      10,
			MessagesWithLifecycle: 10,
			Counts: []messagelifecycle.ReasonCodeCount{
				metricsCount(messagelifecycle.ReasonAcceptanceOutboundAPI, 10, 10),
				metricsCount(messagelifecycle.ReasonSubmissionUpstreamAccepted, 8, 8),
				metricsCount(messagelifecycle.ReasonDeliveryRecipientServerAccepted, 6, 6),
				metricsCount(messagelifecycle.ReasonDeliveryPermanentBounce, 2, 2),
			},
		},
	})

	status, body := accountMetricsGET(t, srv, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	summary := metricsSection(t, body, "summary")
	if got := summary["accepted"].(float64); got != 10 {
		t.Errorf("accepted = %v, want 10", got)
	}
	rates := metricsSection(t, body, "rates")
	if got := rates["delivered_rate"].(float64); got != 0.6 {
		t.Errorf("delivered_rate = %v, want 0.6", got)
	}
	// Denominated on submitted, exactly as the per-agent operation does.
	if got := rates["bounce_rate"].(float64); got != 0.25 {
		t.Errorf("bounce_rate = %v, want 0.25 (2 bounced / 8 submitted)", got)
	}
	if agents, ok := body["agents"].([]any); !ok || len(agents) != 0 {
		t.Errorf("agents = %#v, want an empty array without group_by", body["agents"])
	}
	if body["agents_truncated"] != false {
		t.Errorf("agents_truncated = %#v, want false", body["agents_truncated"])
	}
}

func TestAccountMetricsGroupByAgentReturnsPerAgentSlices(t *testing.T) {
	srv, calls := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{
		Totals: messagelifecycle.AgentMetrics{
			MessagesInWindow:      5,
			MessagesWithLifecycle: 5,
			Counts: []messagelifecycle.ReasonCodeCount{
				metricsCount(messagelifecycle.ReasonAcceptanceOutboundAPI, 5, 5),
				metricsCount(messagelifecycle.ReasonDeliveryRecipientServerAccepted, 5, 5),
			},
		},
		Agents: []messagelifecycle.AgentMetricsGroup{
			{
				AgentEmail: "busy@example.com",
				Metrics: messagelifecycle.AgentMetrics{
					MessagesInWindow:      4,
					MessagesWithLifecycle: 4,
					Counts: []messagelifecycle.ReasonCodeCount{
						metricsCount(messagelifecycle.ReasonAcceptanceOutboundAPI, 4, 4),
						metricsCount(messagelifecycle.ReasonDeliveryRecipientServerAccepted, 4, 4),
					},
				},
			},
			{
				AgentEmail: "quiet@example.com",
				Metrics: messagelifecycle.AgentMetrics{
					MessagesInWindow:      1,
					MessagesWithLifecycle: 1,
					Counts: []messagelifecycle.ReasonCodeCount{
						metricsCount(messagelifecycle.ReasonAcceptanceOutboundAPI, 1, 1),
					},
				},
			},
		},
		AgentsTruncated: true,
	})

	status, body := accountMetricsGET(t, srv, "?group_by=agent")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
	if len(*calls) != 1 || !strings.HasSuffix((*calls)[0], "group=true") {
		t.Errorf("store call = %v, want group=true", *calls)
	}
	agents, ok := body["agents"].([]any)
	if !ok || len(agents) != 2 {
		t.Fatalf("agents = %#v, want 2 entries", body["agents"])
	}
	busy := agents[0].(map[string]any)
	if busy["agent_email"] != "busy@example.com" {
		t.Errorf("first agent = %v, want the busiest first", busy["agent_email"])
	}
	// Each slice carries its OWN rates, derived the same way as the totals.
	if got := busy["rates"].(map[string]any)["delivered_rate"].(float64); got != 1 {
		t.Errorf("busy delivered_rate = %v, want 1", got)
	}
	quiet := agents[1].(map[string]any)
	if got := quiet["rates"].(map[string]any)["delivered_rate"].(float64); got != 0 {
		t.Errorf("quiet delivered_rate = %v, want 0 (accepted but never delivered)", got)
	}
	if body["agents_truncated"] != true {
		t.Error("agents_truncated must survive to the wire")
	}
}

// TestAccountMetricsRejectsAgentScopedCredential: reading across every agent
// in the account is account administration, so a pinned agent key is barred.
func TestAccountMetricsRejectsAgentScopedCredential(t *testing.T) {
	srv, calls := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{}, func(d *Deps) {
		d.PrincipalAuthenticator = func(r *http.Request) (*identity.Principal, error) {
			return &identity.Principal{
				User:    &identity.User{ID: "u_1", Email: "owner@example.com"},
				Scope:   identity.ScopeAgent,
				AgentID: "agent@example.com",
			}, nil
		}
	})
	status, _ := accountMetricsGET(t, srv, "")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if len(*calls) != 0 {
		t.Errorf("store was queried for an agent-scoped caller: %v", *calls)
	}
}

func TestAccountMetricsRejectsInvalidWindows(t *testing.T) {
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
			srv, calls := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{})
			status, body := accountMetricsGET(t, srv, tc.query)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %#v", status, body)
			}
			if len(*calls) != 0 {
				t.Errorf("store was queried despite an invalid window: %v", *calls)
			}
		})
	}
}

func TestAccountMetricsRejectsUnknownGroupBy(t *testing.T) {
	srv, calls := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{})
	status, _ := accountMetricsGET(t, srv, "?group_by=domain")
	if status != http.StatusUnprocessableEntity && status != http.StatusBadRequest {
		t.Fatalf("status = %d, want a validation rejection", status)
	}
	if len(*calls) != 0 {
		t.Errorf("store was queried for an unknown group_by: %v", *calls)
	}
}

func TestAccountMetricsReportsNotImplementedWithoutStore(t *testing.T) {
	srv, _ := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{}, func(d *Deps) {
		d.CountAccountMetrics = nil
	})
	status, body := accountMetricsGET(t, srv, "")
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %#v", status, body)
	}
}

func withDailyBuckets(days []messagelifecycle.DayBucket) func(*Deps) {
	return func(d *Deps) {
		d.CountAccountDaily = func(context.Context, string, time.Time, time.Time) ([]messagelifecycle.DayBucket, error) {
			return days, nil
		}
	}
}

// TestAccountMetricsBucketsOnlyWhenAsked: the daily read is a second query, so
// it must not run for callers that only want totals.
func TestAccountMetricsBucketsOnlyWhenAsked(t *testing.T) {
	day := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	buckets := []messagelifecycle.DayBucket{{
		Day: day,
		Counts: []messagelifecycle.ReasonCodeCount{
			metricsCount(messagelifecycle.ReasonAcceptanceOutboundAPI, 10, 10),
			metricsCount(messagelifecycle.ReasonDeliveryRecipientServerAccepted, 9, 9),
		},
	}}

	srv, _ := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{}, withDailyBuckets(buckets))
	_, plain := accountMetricsGET(t, srv, "")
	if got, ok := plain["buckets"].([]any); !ok || len(got) != 0 {
		t.Errorf("buckets = %#v, want empty without bucket=day", plain["buckets"])
	}

	_, bucketed := accountMetricsGET(t, srv, "?bucket=day")
	rows, ok := bucketed["buckets"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("buckets = %#v, want 1 row", bucketed["buckets"])
	}
	first := rows[0].(map[string]any)
	if first["day"] != "2026-08-01T00:00:00Z" {
		t.Errorf("day = %v, want midnight UTC", first["day"])
	}
	// Each bucket carries its own rates, on the same denominators.
	if got := first["rates"].(map[string]any)["delivered_rate"].(float64); got != 0.9 {
		t.Errorf("bucket delivered_rate = %v, want 0.9", got)
	}
	if got := first["summary"].(map[string]any)["accepted"].(float64); got != 10 {
		t.Errorf("bucket accepted = %v, want 10", got)
	}
}

// A quiet day is present with null rates, never a fabricated 0%.
func TestAccountMetricsQuietBucketHasNullRates(t *testing.T) {
	day := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	srv, _ := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{},
		withDailyBuckets([]messagelifecycle.DayBucket{{Day: day}}))

	_, body := accountMetricsGET(t, srv, "?bucket=day")
	first := body["buckets"].([]any)[0].(map[string]any)
	if rate := first["rates"].(map[string]any)["delivered_rate"]; rate != nil {
		t.Errorf("quiet day delivered_rate = %#v, want null", rate)
	}
	if got := first["summary"].(map[string]any)["accepted"].(float64); got != 0 {
		t.Errorf("quiet day accepted = %v, want 0", got)
	}
}

func TestAccountMetricsRejectsUnknownBucket(t *testing.T) {
	srv, calls := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{})
	status, _ := accountMetricsGET(t, srv, "?bucket=hour")
	if status != http.StatusUnprocessableEntity && status != http.StatusBadRequest {
		t.Fatalf("status = %d, want a validation rejection", status)
	}
	if len(*calls) != 0 {
		t.Errorf("store was queried for an unknown bucket: %v", *calls)
	}
}

func webhookCounts(delivered, pending, rejected, noResponse int64) webhook.DeliveryOutcomeCounts {
	return webhook.DeliveryOutcomeCounts{
		Total:            delivered + pending + rejected + noResponse,
		Delivered:        delivered,
		Pending:          pending,
		EndpointRejected: rejected,
		NoResponse:       noResponse,
	}
}

func withWebhookMetrics(m webhook.AccountDeliveryMetrics) func(*Deps) {
	return func(d *Deps) {
		d.CountWebhookDeliveries = func(context.Context, string, time.Time, time.Time) (webhook.AccountDeliveryMetrics, error) {
			return m, nil
		}
	}
}

// TestAccountMetricsReportsWebhookDeliveryHealth: the "did my code receive it"
// half. It answers a different question from the email counters, from a
// different table, so it lives in its own block rather than on summary.
func TestAccountMetricsReportsWebhookDeliveryHealth(t *testing.T) {
	status := int32(405)
	srv, _ := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{},
		withWebhookMetrics(webhook.AccountDeliveryMetrics{
			Totals: webhookCounts(96, 2, 3, 1),
			Endpoints: []webhook.EndpointDeliveryMetrics{{
				WebhookID:      "wh_1",
				URLHost:        "hooks.example.test",
				Counts:         webhookCounts(96, 2, 3, 1),
				LastStatusCode: &status,
			}},
		}))

	code, body := accountMetricsGET(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", code, body)
	}
	hooks := metricsSection(t, body, "webhooks")
	if hooks["deliveries"].(float64) != 102 {
		t.Errorf("deliveries = %v, want 102", hooks["deliveries"])
	}
	if hooks["endpoint_rejected"].(float64) != 3 || hooks["no_response"].(float64) != 1 {
		t.Errorf("failure split = %v / %v, want 3 / 1", hooks["endpoint_rejected"], hooks["no_response"])
	}
	// Pending is excluded from the denominator: a delivery mid-retry has not
	// failed, and counting it as one makes a healthy endpoint look broken.
	if got := hooks["success_rate"].(float64); got != 0.96 {
		t.Errorf("success_rate = %v, want 0.96 (96 / 100 settled)", got)
	}
	endpoints := hooks["endpoints"].([]any)
	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %#v, want 1", hooks["endpoints"])
	}
	first := endpoints[0].(map[string]any)
	if first["url_host"] != "hooks.example.test" {
		t.Errorf("url_host = %v", first["url_host"])
	}
	if first["last_status_code"].(float64) != 405 {
		t.Errorf("last_status_code = %v, want 405", first["last_status_code"])
	}
}

// A zero denominator must stay null here too, for the same reason it does on
// the email rates: 0%% reads as total failure rather than "nothing happened".
func TestAccountMetricsWebhookRateIsNullWithoutTraffic(t *testing.T) {
	srv, _ := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{},
		withWebhookMetrics(webhook.AccountDeliveryMetrics{}))

	_, body := accountMetricsGET(t, srv, "")
	hooks := metricsSection(t, body, "webhooks")
	if value, present := hooks["success_rate"]; !present || value != nil {
		t.Errorf("success_rate = %#v, want null", hooks["success_rate"])
	}
	if hooks["endpoints"] == nil {
		t.Error("endpoints must be an empty array, not null")
	}
}

// Retention differs from the email counters: delivery rows are pruned at 30
// days, so a wider window must say so rather than look like a volume collapse.
func TestAccountMetricsFlagsWebhookRetentionHorizon(t *testing.T) {
	srv, _ := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{},
		withWebhookMetrics(webhook.AccountDeliveryMetrics{
			Totals:                 webhookCounts(10, 0, 0, 0),
			WindowExceedsRetention: true,
		}))

	_, body := accountMetricsGET(t, srv, "")
	if metricsSection(t, body, "webhooks")["window_exceeds_retention"] != true {
		t.Error("window_exceeds_retention must survive to the wire")
	}
}

// A deployment without the subscriber store still serves email counters.
func TestAccountMetricsToleratesMissingWebhookStore(t *testing.T) {
	srv, _ := newAccountMetricsServer(t, messagelifecycle.AccountMetrics{
		Totals: messagelifecycle.AgentMetrics{MessagesInWindow: 5},
	})
	code, body := accountMetricsGET(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", code, body)
	}
	if metricsSection(t, body, "webhooks")["deliveries"].(float64) != 0 {
		t.Error("a missing webhook store must yield a zeroed block, not an error")
	}
}
