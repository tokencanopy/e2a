package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/webhook"
)

const accountMetricsBetaDoc = "Beta: account metrics may change before it is declared stable."

// AccountMetricsCounter aggregates persisted lifecycle observations across an
// account. Mirrors messagelifecycle.Store.CountByReasonCodeForAccount.
type AccountMetricsCounter func(ctx context.Context, userID string, start, end time.Time, groupByAgent bool) (messagelifecycle.AccountMetrics, error)

// AccountDailyCounter returns per-day lifecycle tallies for an account.
// Mirrors messagelifecycle.Store.CountByDayForAccount.
type AccountDailyCounter func(ctx context.Context, userID string, start, end time.Time) ([]messagelifecycle.DayBucket, error)

// MetricsBucketView is one day's slice of the window.
type MetricsBucketView struct {
	Day     time.Time          `json:"day" doc:"Midnight UTC starting this bucket. Buckets are contiguous and gap-filled: a day with no traffic is present with zeroes rather than omitted, so a chart cannot draw a straight line across a quiet day and imply steady volume."`
	Summary MetricsSummaryView `json:"summary"`
	Rates   MetricsRatesView   `json:"rates" doc:"Rates for this day alone, on the same fixed denominators as the window totals. Null members mean that day had no denominator, which is common on low-volume days and is NOT a zero."`
}

// WebhookDeliveryCounter aggregates subscriber-delivery outcomes for an
// account. Mirrors webhook.SubscriberStore.CountDeliveriesForAccount.
type WebhookDeliveryCounter func(ctx context.Context, userID string, start, end time.Time) (webhook.AccountDeliveryMetrics, error)

// WebhookEndpointMetricsView is one subscriber endpoint's delivery slice.
type WebhookEndpointMetricsView struct {
	WebhookID        string   `json:"webhook_id"`
	URLHost          string   `json:"url_host" doc:"The endpoint's host. Only the host is reported — a webhook URL can carry a shared secret in its path or query string, which a metrics payload must not echo back."`
	Deliveries       int64    `json:"deliveries"`
	Delivered        int64    `json:"delivered"`
	Pending          int64    `json:"pending"`
	EndpointRejected int64    `json:"endpoint_rejected"`
	NoResponse       int64    `json:"no_response"`
	SuccessRate      *float64 `json:"success_rate" nullable:"true" doc:"delivered / (delivered + failed). Null when nothing has settled yet."`
	LastStatusCode   *int32   `json:"last_status_code" nullable:"true" doc:"Most recent HTTP status this endpoint returned in the window; null when it never answered. A constant 401 or 405 names the fix directly."`

	Enabled           bool       `json:"enabled" doc:"False means this endpoint is not receiving events right now."`
	AutoDisabledAt    *time.Time `json:"auto_disabled_at" nullable:"true" doc:"When e2a auto-disabled this endpoint after sustained failure. Non-null means events are being DROPPED, not retried — the most urgent thing on this page."`
	AutoDisableReason string     `json:"auto_disable_reason" doc:"Why it was auto-disabled. Empty when it was not."`
}

// WebhookMetricsView is the account's webhook delivery health — the "did my
// code actually receive it" half, which email delivery counters cannot answer.
type WebhookMetricsView struct {
	Deliveries int64 `json:"deliveries" doc:"Delivery rows in the window. The grain is one row per (event, subscriber) pair, so an account with three matching webhooks produces three rows for one message — this legitimately exceeds message counts and does not mean duplicated mail."`
	Delivered  int64 `json:"delivered"`
	Pending    int64 `json:"pending" doc:"Deliveries still retrying. Excluded from success_rate's denominator because they have not settled."`

	EndpointRejected int64 `json:"endpoint_rejected" doc:"The endpoint answered with a non-2xx. Unambiguously the subscriber's own response — check last_status_code per endpoint."`
	NoResponse       int64 `json:"no_response" doc:"No HTTP response was ever received: connect, DNS or TLS failure, a blocked URL, or a delivery that expired while pending. Predominantly an unreachable endpoint, but it can include rare e2a-side failures, so it is not a clean fault split."`

	SuccessRate *float64 `json:"success_rate" nullable:"true" doc:"delivered / (delivered + endpoint_rejected + no_response). Null — never 0 — when nothing has settled."`

	WindowExceedsRetention bool `json:"window_exceeds_retention" doc:"True when the window reaches past the 30-day delivery-retention horizon. Older rows are pruned, so the counts understate that stretch — a drop here is a retention boundary, not a delivery collapse."`

	EndpointsAutoDisabled int64 `json:"endpoints_auto_disabled" doc:"Endpoints e2a has auto-disabled after sustained failure. Counted across every endpoint the account owns, whether or not it had traffic in this window — a disabled endpoint with no recent deliveries is exactly the case worth surfacing."`

	Endpoints          []WebhookEndpointMetricsView `json:"endpoints" doc:"Per-endpoint breakdown, busiest first."`
	EndpointsTruncated bool                         `json:"endpoints_truncated" doc:"True when more endpoints have traffic than are listed (cap: 50). The totals above stay complete."`
}

// AgentMetricsGroupView is one agent's slice of an account aggregate.
type AgentMetricsGroupView struct {
	AgentEmail                string               `json:"agent_email"`
	MessagesInWindow          int64                `json:"messages_in_window"`
	MessagesWithLifecycle     int64                `json:"messages_with_lifecycle"`
	ReconstructedObservations int64                `json:"reconstructed_observations"`
	Summary                   MetricsSummaryView   `json:"summary"`
	Rates                     MetricsRatesView     `json:"rates"`
	Counters                  []MetricsCounterView `json:"counters" doc:"Every reason code this agent produced in the window, ordered by code."`
}

// AccountMetricsView is the account-wide metrics response.
type AccountMetricsView struct {
	Start time.Time `json:"start" doc:"Inclusive start of the cohort window."`
	End   time.Time `json:"end" doc:"Exclusive end of the cohort window."`

	MessagesInWindow      int64 `json:"messages_in_window" doc:"Messages created in the window across every agent this account owns."`
	MessagesWithLifecycle int64 `json:"messages_with_lifecycle" doc:"Messages in the window carrying at least one persisted lifecycle observation. Below messages_in_window means part of the window predates the lifecycle ledger, and every counter here undercounts by that difference."`

	ReconstructedObservations int64 `json:"reconstructed_observations" doc:"Observations derived from durable message state rather than recorded at the boundary. Included in the counts above; reported separately so the inferred share is visible."`

	Summary  MetricsSummaryView   `json:"summary" doc:"Account-wide counters. Always computed across every agent, never from the possibly-truncated agents array."`
	Rates    MetricsRatesView     `json:"rates" doc:"Account-wide rates, on the same fixed denominators the per-agent operation uses."`
	Counters []MetricsCounterView `json:"counters" doc:"Every reason code observed across the account in the window, ordered by code."`

	Agents          []AgentMetricsGroupView `json:"agents" doc:"Per-agent breakdown, busiest first. Empty unless group_by=agent was requested."`
	AgentsTruncated bool                    `json:"agents_truncated" doc:"True when the account has more agents with traffic than the breakdown returned (cap: 200). The account-wide totals above stay complete — only the breakdown is cut."`

	// Webhooks answers a different question from every counter above it —
	// "did my code receive the event", not "did the mail reach the
	// recipient" — and comes from a different fact table with a different
	// grain and a shorter retention. It is deliberately its own block rather
	// than more fields on summary, so neither can be mistaken for the other.
	Webhooks WebhookMetricsView `json:"webhooks"`

	Buckets []MetricsBucketView `json:"buckets" doc:"Per-day breakdown, oldest first. Empty unless bucket=day was requested. Each bucket's counters sum to the window totals above; its rates do not average to the window rate, because a rate of rates is not the rate."`
}

type accountMetricsInput struct {
	Start   time.Time `query:"start" doc:"Inclusive start of the cohort window (RFC 3339). Defaults to 30 days before end."`
	End     time.Time `query:"end" doc:"Exclusive end of the cohort window (RFC 3339). Defaults to now."`
	Bucket  string    `query:"bucket" enum:"day" doc:"Set to 'day' to also receive per-day buckets for charting. Buckets are UTC calendar days — a boundary that moved with the reader's timezone would make two people comparing the same chart see different daily numbers."`
	GroupBy string    `query:"group_by" enum:"agent" doc:"Set to 'agent' to also receive a per-agent breakdown. Omit for account totals only, which is the cheaper read."`
}

type accountMetricsOutput struct {
	Body AccountMetricsView
}

func (s *Server) registerAccountMetrics() {
	registerOp(s.API, huma.Operation{
		OperationID: "getAccountMetrics",
		Method:      http.MethodGet,
		Path:        "/v1/metrics",
		Summary:     "Get account-wide delivery metrics (beta)",
		Description: "Counter metrics across every agent this account owns, aggregated from the canonical message lifecycle ledger, " +
			"on the same cohort-window and denominator contract as GET /v1/agents/{email}/metrics — so an account total and the " +
			"per-agent numbers under it can never disagree about what a rate means. Messages are attributed to the window by their " +
			"own creation time, so bounce and complaint feedback keeps arriving for up to 72 hours and the most recent days should " +
			"be read as provisional. Account-scoped credentials only; an agent-scoped credential reads its own agent through " +
			"GET /v1/agents/{email}/metrics instead. " + accountMetricsBetaDoc,
		Tags:       []string{"account"},
		Security:   []map[string][]string{{"bearer": {}}},
		Extensions: beta(),
	}, s.handleAccountMetrics)
}

func (s *Server) handleAccountMetrics(ctx context.Context, in *accountMetricsInput) (*accountMetricsOutput, error) {
	// Account administration: an agent-scoped credential must not read across
	// the sibling agents it is not bound to.
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.CountAccountMetrics == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "account metrics are not available on this deployment")
	}

	end := in.End
	if end.IsZero() {
		end = time.Now().UTC()
	}
	start := in.Start
	if start.IsZero() {
		start = end.Add(-agentMetricsDefaultWindow)
	}
	if !end.After(start) {
		return nil, NewError(http.StatusBadRequest, "invalid_request", "end must be after start")
	}
	if end.Sub(start) > messagelifecycle.MaxMetricsWindow {
		return nil, NewError(http.StatusBadRequest, "invalid_request",
			"window must not exceed 92 days; request a narrower range")
	}

	metrics, err := s.deps.CountAccountMetrics(ctx, user.ID, start.UTC(), end.UTC(), in.GroupBy == "agent")
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to aggregate account metrics")
	}

	// Webhook delivery is a separate, optional dependency: a deployment
	// without it still answers with email counters and a zeroed webhook block
	// rather than failing the whole read.
	var deliveries webhook.AccountDeliveryMetrics
	if s.deps.CountWebhookDeliveries != nil {
		deliveries, err = s.deps.CountWebhookDeliveries(ctx, user.ID, start.UTC(), end.UTC())
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to aggregate webhook delivery metrics")
		}
	}

	buckets := []MetricsBucketView{}
	if in.Bucket == "day" && s.deps.CountAccountDaily != nil {
		days, err := s.deps.CountAccountDaily(ctx, user.ID, start.UTC(), end.UTC())
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to aggregate daily metrics")
		}
		buckets = make([]MetricsBucketView, 0, len(days))
		for _, day := range days {
			_, daySummary, dayRates := deriveMetrics(messagelifecycle.AgentMetrics{Counts: day.Counts})
			buckets = append(buckets, MetricsBucketView{Day: day.Day, Summary: daySummary, Rates: dayRates})
		}
	}

	counters, summary, rates := deriveMetrics(metrics.Totals)
	view := AccountMetricsView{
		Start:                     start.UTC(),
		End:                       end.UTC(),
		MessagesInWindow:          metrics.Totals.MessagesInWindow,
		MessagesWithLifecycle:     metrics.Totals.MessagesWithLifecycle,
		ReconstructedObservations: metrics.Totals.ReconstructedObservations,
		Summary:                   summary,
		Rates:                     rates,
		Counters:                  counters,
		AgentsTruncated:           metrics.AgentsTruncated,
		Agents:                    make([]AgentMetricsGroupView, 0, len(metrics.Agents)),
		Webhooks:                  webhookMetricsView(deliveries),
		Buckets:                   buckets,
	}
	for _, group := range metrics.Agents {
		groupCounters, groupSummary, groupRates := deriveMetrics(group.Metrics)
		view.Agents = append(view.Agents, AgentMetricsGroupView{
			AgentEmail:                group.AgentEmail,
			MessagesInWindow:          group.Metrics.MessagesInWindow,
			MessagesWithLifecycle:     group.Metrics.MessagesWithLifecycle,
			ReconstructedObservations: group.Metrics.ReconstructedObservations,
			Summary:                   groupSummary,
			Rates:                     groupRates,
			Counters:                  groupCounters,
		})
	}
	return &accountMetricsOutput{Body: view}, nil
}

// webhookMetricsView shapes the delivery aggregate for the wire.
//
// success_rate excludes still-pending deliveries from its denominator: a
// delivery mid-retry has not failed, and counting it as one would make a
// healthy endpoint look broken during a transient blip.
func webhookMetricsView(m webhook.AccountDeliveryMetrics) WebhookMetricsView {
	view := WebhookMetricsView{
		Deliveries:             m.Totals.Total,
		Delivered:              m.Totals.Delivered,
		Pending:                m.Totals.Pending,
		EndpointRejected:       m.Totals.EndpointRejected,
		NoResponse:             m.Totals.NoResponse,
		SuccessRate:            ratio(m.Totals.Delivered, m.Totals.Delivered+m.Totals.Failed()),
		EndpointsAutoDisabled:  m.EndpointsAutoDisabled,
		WindowExceedsRetention: m.WindowExceedsRetention,
		EndpointsTruncated:     m.EndpointsTruncated,
		Endpoints:              make([]WebhookEndpointMetricsView, 0, len(m.Endpoints)),
	}
	for _, e := range m.Endpoints {
		view.Endpoints = append(view.Endpoints, WebhookEndpointMetricsView{
			WebhookID:         e.WebhookID,
			URLHost:           e.URLHost,
			Deliveries:        e.Counts.Total,
			Delivered:         e.Counts.Delivered,
			Pending:           e.Counts.Pending,
			EndpointRejected:  e.Counts.EndpointRejected,
			NoResponse:        e.Counts.NoResponse,
			SuccessRate:       ratio(e.Counts.Delivered, e.Counts.Delivered+e.Counts.Failed()),
			LastStatusCode:    e.LastStatusCode,
			Enabled:           e.Enabled,
			AutoDisabledAt:    e.AutoDisabledAt,
			AutoDisableReason: e.AutoDisableReason,
		})
	}
	return view
}
