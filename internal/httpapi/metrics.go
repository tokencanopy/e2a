package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

const (
	agentMetricsDefaultWindow = 30 * 24 * time.Hour
	agentMetricsBetaDoc       = "Beta: agent metrics may change before it is declared stable."
)

// AgentMetricsCounter aggregates persisted lifecycle observations for one
// agent over a cohort window. Mirrors messagelifecycle.Store.CountByReasonCode.
type AgentMetricsCounter func(ctx context.Context, agentID string, start, end time.Time) (messagelifecycle.AgentMetrics, error)

// MetricsCounterView is one canonical reason code's tally.
type MetricsCounterView struct {
	ReasonCode   string `json:"reason_code" doc:"Canonical lifecycle reason code."`
	Stage        string `json:"stage" doc:"Lifecycle stage the observation was made at."`
	Outcome      string `json:"outcome" doc:"What e2a observed at that stage."`
	Retryable    bool   `json:"retryable" doc:"Whether this observation describes a condition the pipeline retries."`
	Observations int64  `json:"observations" doc:"Ledger rows. Retryable codes are recorded once per attempt, so this exceeds messages whenever the pipeline retried."`
	Messages     int64  `json:"messages" doc:"Distinct messages carrying at least one observation of this code. This is the grain funnel arithmetic uses."`
}

// MetricsSummaryView is the headline counter set, all at message grain
// (distinct messages, never attempts) so every value shares one denominator
// basis and no stage can exceed the stage above it because of retries.
type MetricsSummaryView struct {
	Accepted            int64 `json:"accepted" doc:"Outbound messages e2a accepted from the send API. The denominator for delivered_rate and suppression_block_rate. Counts sends only — the arriving copy of an agent-to-agent loopback delivery is counted under received, not here."`
	Submitted           int64 `json:"submitted" doc:"Messages an upstream provider or the loopback path accepted for delivery. The denominator for bounce_rate."`
	Delivered           int64 `json:"delivered" doc:"Messages a recipient server accepted. This is recipient-server acceptance, NOT inbox placement — no email provider exposes the latter."`
	BouncedHard         int64 `json:"bounced_hard" doc:"Permanent bounces. These add the recipient to the suppression list."`
	BouncedSoft         int64 `json:"bounced_soft" doc:"Transient bounces."`
	BouncedUndetermined int64 `json:"bounced_undetermined" doc:"Bounces the provider did not classify. Kept separate rather than folded into soft, which would understate hard-bounce risk."`
	Complained          int64 `json:"complained" doc:"Recipients who reported the message as spam."`
	Suppressed          int64 `json:"suppressed" doc:"Sends blocked before submission because the recipient was on a suppression list. These never left e2a, so an agent sees no bounce and no reply — watch this counter for silent losses."`
	SendFailed          int64 `json:"send_failed" doc:"Messages that reached a terminal submission failure: provider rejection, local retry exhaustion, or policy cancellation. Break the three apart via counters[]."`

	Received  int64 `json:"received" doc:"Inbound messages accepted: arrivals over SMTP plus the delivered copy of agent-to-agent mail that never left this deployment (the local loopback path)."`
	DMARCPass int64 `json:"dmarc_pass" doc:"Inbound messages with an aligned DMARC pass."`
	DMARCFail int64 `json:"dmarc_fail" doc:"Inbound messages whose DMARC evaluation failed."`
	DMARCNone int64 `json:"dmarc_none" doc:"Inbound messages whose sender publishes no DMARC policy. Common, and not itself suspicious."`
	DMARCError int64 `json:"dmarc_error" doc:"Inbound messages whose DMARC evaluation could not be completed (temporary or permanent resolver error)."`

	ReviewHeld            int64 `json:"review_held" doc:"Messages placed on a human-in-the-loop review hold."`
	ReviewApproved        int64 `json:"review_approved" doc:"Holds a reviewer explicitly approved."`
	ReviewRejected        int64 `json:"review_rejected" doc:"Holds a reviewer explicitly rejected."`
	ReviewExpiredApproved int64 `json:"review_expired_approved" doc:"Holds released by TTL expiry rather than a decision. A rising count means nobody is working the queue."`
	ReviewExpiredRejected int64 `json:"review_expired_rejected" doc:"Holds dropped by TTL expiry rather than a decision. A rising count means work is being lost."`
}

// MetricsRatesView carries the four rates whose denominators are fixed by the
// server so every client surface reports the same number.
//
// Each is null when its denominator is zero. A zero-over-zero rendered as 0.0
// is indistinguishable from a genuine total failure, which is the one reading
// a caller must never make by accident.
type MetricsRatesView struct {
	DeliveredRate        *float64 `json:"delivered_rate" nullable:"true" doc:"delivered / accepted. Everything the agent asked to send, including what suppression or review stopped — the honest 'did my mail arrive' number."`
	BounceRate           *float64 `json:"bounce_rate" nullable:"true" doc:"(bounced_hard + bounced_soft + bounced_undetermined) / submitted. Denominated on submitted, not accepted, so it is directly comparable to the provider thresholds that trigger account review."`
	ComplaintRate        *float64 `json:"complaint_rate" nullable:"true" doc:"complained / delivered. Mailbox providers compute spam rate over delivered mail, so this denominator matches theirs."`
	SuppressionBlockRate *float64 `json:"suppression_block_rate" nullable:"true" doc:"suppressed / accepted. The share of requested sends that never left e2a."`
}

// AgentMetricsView is the metrics response.
type AgentMetricsView struct {
	AgentEmail string    `json:"agent_email" doc:"The agent these metrics describe."`
	Start      time.Time `json:"start" doc:"Inclusive start of the cohort window."`
	End        time.Time `json:"end" doc:"Exclusive end of the cohort window."`

	MessagesInWindow      int64 `json:"messages_in_window" doc:"Messages created in the window, from the message table itself."`
	MessagesWithLifecycle int64 `json:"messages_with_lifecycle" doc:"Messages in the window carrying at least one persisted lifecycle observation. Below messages_in_window means part of the window predates the lifecycle ledger, and every counter here undercounts by that difference."`

	ReconstructedObservations int64 `json:"reconstructed_observations" doc:"Observations in the window derived from durable message state rather than recorded at the boundary. Included in the counts above; reported separately so the inferred share is visible."`

	Summary  MetricsSummaryView   `json:"summary"`
	Rates    MetricsRatesView     `json:"rates"`
	Counters []MetricsCounterView `json:"counters" doc:"Every reason code observed in the window, ordered by code. Codes with no observations are omitted rather than reported as zero."`
}

type agentMetricsInput struct {
	Email string    `path:"email"`
	Start time.Time `query:"start" doc:"Inclusive start of the cohort window (RFC 3339). Defaults to 30 days before end."`
	End   time.Time `query:"end" doc:"Exclusive end of the cohort window (RFC 3339). Defaults to now."`
}

type agentMetricsOutput struct {
	Body AgentMetricsView
}

func (s *Server) registerAgentMetrics() {
	registerOp(s.API, huma.Operation{
		OperationID: "getAgentMetrics",
		Method:      http.MethodGet,
		Path:        "/v1/agents/{email}/metrics",
		Summary:     "Get an agent's delivery metrics (beta)",
		Description: "Counter metrics for one agent over a cohort window, aggregated from the canonical message lifecycle ledger. " +
			"Messages are attributed to the window by their own creation time, not by when each observation landed, so a rate never mixes " +
			"numerator and denominator from different populations. The cost of that is a settling period: bounce and complaint feedback " +
			"arrives for up to 72 hours, so the most recent days keep moving and should be read as provisional. " +
			"Delivery means recipient-server acceptance and does not claim inbox placement. " + agentMetricsBetaDoc,
		Tags:       []string{"messages"},
		Security:   []map[string][]string{{"bearer": {}}},
		Extensions: beta(),
	}, s.handleAgentMetrics)
}

func (s *Server) handleAgentMetrics(ctx context.Context, in *agentMetricsInput) (*agentMetricsOutput, error) {
	agent, err := s.resolveOwnedAgent(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	if s.deps.CountAgentMetrics == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "agent metrics are not available on this deployment")
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

	metrics, err := s.deps.CountAgentMetrics(ctx, agent.ID, start.UTC(), end.UTC())
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to aggregate agent metrics")
	}

	view := AgentMetricsView{
		AgentEmail:                agent.Email,
		Start:                     start.UTC(),
		End:                       end.UTC(),
		MessagesInWindow:          metrics.MessagesInWindow,
		MessagesWithLifecycle:     metrics.MessagesWithLifecycle,
		ReconstructedObservations: metrics.ReconstructedObservations,
		Counters:                  make([]MetricsCounterView, 0, len(metrics.Counts)),
	}

	// byCode is message-grained on purpose: the summary and every rate built
	// from it must not inflate when the pipeline retries a single message.
	byCode := make(map[messagelifecycle.ReasonCode]int64, len(metrics.Counts))
	for _, count := range metrics.Counts {
		byCode[count.ReasonCode] = count.Messages
		view.Counters = append(view.Counters, MetricsCounterView{
			ReasonCode:   string(count.ReasonCode),
			Stage:        string(count.Stage),
			Outcome:      string(count.Outcome),
			Retryable:    count.Retryable,
			Observations: count.Observations,
			Messages:     count.Messages,
		})
	}
	sum := func(codes ...messagelifecycle.ReasonCode) int64 {
		var total int64
		for _, code := range codes {
			total += byCode[code]
		}
		return total
	}

	view.Summary = MetricsSummaryView{
		// Outbound acceptance is acceptance.outbound_api ALONE.
		// acceptance.local_loopback is an INBOUND code — every writer and both
		// reconstruction paths emit it only for the arriving copy of a loopback
		// delivery. Summing it here mixed an inbound arrival into the outbound
		// denominator, which understated delivered_rate for any agent sending
		// to another agent on the same deployment.
		Accepted:            sum(messagelifecycle.ReasonAcceptanceOutboundAPI),
		Submitted:           sum(messagelifecycle.ReasonSubmissionUpstreamAccepted, messagelifecycle.ReasonSubmissionLocalLoopbackAccepted),
		Delivered:           sum(messagelifecycle.ReasonDeliveryRecipientServerAccepted),
		BouncedHard:         sum(messagelifecycle.ReasonDeliveryPermanentBounce),
		BouncedSoft:         sum(messagelifecycle.ReasonDeliveryTransientBounce),
		BouncedUndetermined: sum(messagelifecycle.ReasonDeliveryUndeterminedBounce),
		Complained:          sum(messagelifecycle.ReasonComplaintRecipientReported),
		Suppressed:          sum(messagelifecycle.ReasonSuppressionRecipientBlocked),
		SendFailed: sum(messagelifecycle.ReasonSubmissionProviderRejected,
			messagelifecycle.ReasonSubmissionLocalRetriesExhausted,
			messagelifecycle.ReasonSubmissionCancelled),

		Received:  sum(messagelifecycle.ReasonAcceptanceInboundSMTP, messagelifecycle.ReasonAcceptanceLocalLoopback),
		DMARCPass: sum(messagelifecycle.ReasonAuthenticationDMARCPass),
		DMARCFail: sum(messagelifecycle.ReasonAuthenticationDMARCFail),
		DMARCNone: sum(messagelifecycle.ReasonAuthenticationDMARCNone),
		DMARCError: sum(messagelifecycle.ReasonAuthenticationDMARCTemporaryError,
			messagelifecycle.ReasonAuthenticationDMARCPermanentError),

		ReviewHeld:            sum(messagelifecycle.ReasonReviewHoldCreated),
		ReviewApproved:        sum(messagelifecycle.ReasonReviewApproved),
		ReviewRejected:        sum(messagelifecycle.ReasonReviewRejected),
		ReviewExpiredApproved: sum(messagelifecycle.ReasonReviewExpiredApproved),
		ReviewExpiredRejected: sum(messagelifecycle.ReasonReviewExpiredRejected),
	}

	bounced := view.Summary.BouncedHard + view.Summary.BouncedSoft + view.Summary.BouncedUndetermined
	view.Rates = MetricsRatesView{
		DeliveredRate:        ratio(view.Summary.Delivered, view.Summary.Accepted),
		BounceRate:           ratio(bounced, view.Summary.Submitted),
		ComplaintRate:        ratio(view.Summary.Complained, view.Summary.Delivered),
		SuppressionBlockRate: ratio(view.Summary.Suppressed, view.Summary.Accepted),
	}

	return &agentMetricsOutput{Body: view}, nil
}

// ratio returns nil rather than zero when the denominator is zero, so "no
// traffic" stays distinguishable from "everything failed".
func ratio(numerator, denominator int64) *float64 {
	if denominator <= 0 {
		return nil
	}
	value := float64(numerator) / float64(denominator)
	return &value
}
