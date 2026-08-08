package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

const accountMetricsBetaDoc = "Beta: account metrics may change before it is declared stable."

// AccountMetricsCounter aggregates persisted lifecycle observations across an
// account. Mirrors messagelifecycle.Store.CountByReasonCodeForAccount.
type AccountMetricsCounter func(ctx context.Context, userID string, start, end time.Time, groupByAgent bool) (messagelifecycle.AccountMetrics, error)

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
}

type accountMetricsInput struct {
	Start   time.Time `query:"start" doc:"Inclusive start of the cohort window (RFC 3339). Defaults to 30 days before end."`
	End     time.Time `query:"end" doc:"Exclusive end of the cohort window (RFC 3339). Defaults to now."`
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
