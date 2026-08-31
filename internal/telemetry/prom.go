package telemetry

import (
	"net/http"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prom is the Prometheus backend. It owns a private registry (no global
// state — tests construct as many as they like) exposed via Handler().
//
// Label hygiene is enforced here, not at call sites: every label value
// passes through an enum allowlist (unknown → "other") or a hard series
// cap (route, event type). Metric labels never carry message content,
// addresses, URLs, or credentials — see docs/observability.md for the
// full catalog and the cardinality contract.
type Prom struct {
	reg *prometheus.Registry

	httpRequests       *prometheus.CounterVec
	httpDuration       *prometheus.HistogramVec
	smtpInbound        *prometheus.CounterVec
	smtpDuration       prometheus.Histogram
	outQueueWait       prometheus.Histogram
	outTerminal        *prometheus.CounterVec
	outTerminalLat     prometheus.Histogram
	outAttempts        *prometheus.CounterVec
	outAttemptDur      prometheus.Histogram
	outRateDeferred    prometheus.Counter
	whAttempts         *prometheus.CounterVec
	whAttemptDur       prometheus.Histogram
	whTerminal         *prometheus.CounterVec
	whNotify           *prometheus.CounterVec
	whExpiredPending   prometheus.Counter
	whFanOutRescued    prometheus.Counter
	whDeliveryRescued  prometheus.Counter
	whFirstTryLat      prometheus.Histogram
	wsConnects         prometheus.Counter
	wsDisconnects      *prometheus.CounterVec
	wsRejected         *prometheus.CounterVec
	delegatedFailures  *prometheus.CounterVec
	delegatedRefresh   *prometheus.CounterVec
	wsDrained          prometheus.Counter
	wsSendFailures     prometheus.Counter
	wsActive           prometheus.Gauge
	inboundProcess     *prometheus.CounterVec
	inboundDuration    prometheus.Histogram
	queueDepth         *prometheus.GaugeVec
	queueOldestAge     *prometheus.GaugeVec
	threadResolution   *prometheus.CounterVec
	threadHeaderParse  *prometheus.CounterVec
	threadNull         *prometheus.GaugeVec
	threadViolations   *prometheus.GaugeVec
	threadRelationship *prometheus.GaugeVec

	// legacy outbox instruments (same events the Log backend emits)
	outboxPublished *prometheus.CounterVec
	outboxFanOut    *prometheus.CounterVec
	outboxMatched   *prometheus.CounterVec
	outboxNoMatch   *prometheus.CounterVec
	outboxFailures  *prometheus.CounterVec
	redeliver       *prometheus.CounterVec
	janitorDeleted  *prometheus.CounterVec
	contactDue      *prometheus.CounterVec
	notifyMissed    prometheus.Counter
	publisherLag    prometheus.Gauge

	// series-cap state for the two open-ended labels
	mu         sync.Mutex
	routesSeen map[string]struct{}
	typesSeen  map[string]struct{}
}

// Hard caps on the two labels whose value sets are code-defined but not
// enumerable here (chi route patterns, webhook event types). Both are
// bounded by construction; the cap is the backstop that turns a routing
// or catalog bug into a collapsed "other" series instead of a
// cardinality explosion.
const (
	maxRouteSeries = 256
	maxTypeSeries  = 64
)

// Enum allowlists. Values outside these sets collapse to "other".
var (
	methodSet = set("GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS")
	classSet  = set("1xx", "2xx", "3xx", "4xx", "5xx", "none")
	smtpSet   = set("accepted", "accepted_dedup", "tempfail",
		"rejected_unknown_recipient", "rejected_unverified_domain", "rejected_quota",
		"rejected_line_too_long")
	outTermSet = set("sent", "failed_suppressed", "failed_provider",
		"failed_local_retries", "failed_cancelled")
	outAttemptSet = set("success", "temporary_failure", "permanent_failure")
	whSet         = set("delivered", "retryable_failure", "exhausted",
		"webhook_deleted", "skipped_disabled")
	whTerminalSet = set("delivered", "e2a_failure", "endpoint_failure", "excluded")
	whScopeSet    = set("initial", "replay", "test", "unknown")
	// Health-notification labels (internal/webhooknotify). A job carrying
	// an unrecognized kind collapses to "other" like every other enum.
	whNotifyKindSet    = set("warning", "disabled")
	whNotifyOutcomeSet = set("sent", "permanent", "outage", "retryable", "skipped")
	wsReasonSet        = set("replaced", "ping_timeout", "client_close", "error", "shutdown")
	wsRejectSet        = set("unauthorized", "not_found", "forbidden", "upgrade_failed", "internal_error")
	// Delegated-auth labels are category-only by contract: never subjects,
	// issuer response text, token data, or per-check detail.
	delegatedFailSet    = set("invalid_token", "unknown_subject", "verifier_unavailable", "identity_store_failure")
	delegatedRefreshSet = set("success", "key_absent", "transport_error", "parse_error", "rate_limited")
	inboundSet          = set("processed", "noop", "failed_recipient_gone",
		"failed_exhausted", "retryable")
	queueSet = set("outbound", "inbound", "webhook", "maintenance", "notify", "default")
	stateSet = set("available", "running", "retryable", "scheduled")
	stageSet = set("lease", "list_webhooks", "insert_delivery", "update_status", "publish")
	scopeSet = set("single", "since")
	tableSet = set("webhook_events", "webhook_subscriber_deliveries",
		"webhook_deliveries", "messages", "agent_identities",
		"user_sessions", "oauth")
	threadResolutionSet = set(
		"api_reply", "fresh_send", "forward", "rfc_in_reply_to",
		"rfc_references", "self_twin", "authenticated_delivery_twin",
		"lazy_legacy_anchor", "anchor_found_without_thread",
		"legacy_anchor_unmatched", "ambiguous_anchor", "no_anchor",
		"cycle_detected",
	)
	threadHeaderSet    = set("in_reply_to", "references")
	threadNullAgeSet   = set("lt_1h", "1h_6h", "6h_24h")
	threadViolationSet = set(
		"dangling_parent", "cross_agent_parent", "thread_mismatch",
		"cycle", "cycle_depth_limit",
	)
	threadRelationshipSet = set(
		"threads_multi_conversation", "conversations_multi_thread",
	)
)

func set(vals ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		m[v] = struct{}{}
	}
	return m
}

func enum(allowed map[string]struct{}, v string) string {
	if _, ok := allowed[v]; ok {
		return v
	}
	return "other"
}

// Latency buckets. HTTP/webhook/SMTP work completes in ms-to-seconds;
// queue wait can legitimately reach minutes under backlog, so it gets a
// longer tail.
var (
	fastBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, .75, 1, 2, 2.5, 5, 10, 30}
	waitBuckets = []float64{.05, .1, .25, .5, 1, 2.5, 5, 15, 30, 60, 120, 300, 900, 3600}
	// longBuckets spans seconds-to-days: outbound acceptance→terminal can
	// legitimately reach the 72h retry horizon under a provider outage,
	// and webhook event→first-attempt includes fan-out + queue wait. The
	// 60s and 300s edges are the SLO thresholds (docs/observability.md).
	longBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 86400, 259200}
)

// NormalizeBuildLabel keeps operator-provided release identifiers safe and
// bounded for use as a Prometheus label.
func NormalizeBuildLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	const maxLen = 128
	var b strings.Builder
	b.Grow(min(len(value), maxLen))
	for _, r := range value {
		if b.Len() >= maxLen {
			break
		}
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			strings.ContainsRune("._:+@/-", r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func NewProm(build string) *Prom {
	reg := prometheus.NewRegistry()
	registerer := prometheus.WrapRegistererWith(
		prometheus.Labels{"build": NormalizeBuildLabel(build)},
		reg,
	)
	p := &Prom{
		reg:        reg,
		routesSeen: make(map[string]struct{}),
		typesSeen:  make(map[string]struct{}),

		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_http_requests_total",
			Help: "HTTP requests served, by method, chi route pattern, and status class.",
		}, []string{"method", "route", "status_class"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "e2a_http_request_duration_seconds",
			Help:    "HTTP request latency by method and chi route pattern.",
			Buckets: fastBuckets,
		}, []string{"method", "route"}),
		smtpInbound: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_smtp_inbound_total",
			Help: "SMTP intake decisions at the relay edge, by outcome.",
		}, []string{"outcome"}),
		smtpDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "e2a_smtp_inbound_duration_seconds",
			Help:    "SMTP DATA processing duration (accepted and tempfail outcomes).",
			Buckets: fastBuckets,
		}),
		outQueueWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "e2a_outbound_queue_wait_seconds",
			Help:    "Outbound send due→pickup wait per attempt (River attempted_at - scheduled_at).",
			Buckets: waitBuckets,
		}),
		outTerminal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_outbound_terminal_total",
			Help: "Outbound messages reaching a terminal submission outcome.",
		}, []string{"outcome"}),
		outTerminalLat: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "e2a_outbound_terminal_latency_seconds",
			Help:    "Outbound eligibility→terminal latency per message (terminal occurred_at - the submission anchor: the latest of messages.created_at, scheduled_at and reviewed_at), observed exactly once per message.",
			Buckets: longBuckets,
		}),
		outAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_outbound_attempts_total",
			Help: "Outbound submission attempts to the upstream relay, by outcome.",
		}, []string{"outcome"}),
		outAttemptDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "e2a_outbound_attempt_duration_seconds",
			Help:    "Upstream submission attempt duration.",
			Buckets: fastBuckets,
		}),
		outRateDeferred: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "e2a_outbound_rate_deferred_total",
			Help: "Outbound submissions deferred by the per-agent fire-time rate limiter (snoozed, re-fired when the window frees capacity).",
		}),
		whAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_webhook_attempts_total",
			Help: "Webhook delivery attempts, by outcome and endpoint response class.",
		}, []string{"outcome", "status_class"}),
		whAttemptDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "e2a_webhook_attempt_duration_seconds",
			Help:    "Webhook delivery attempt duration (HTTP POST to subscriber).",
			Buckets: fastBuckets,
		}),
		whTerminal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_webhook_delivery_terminal_total",
			Help: "Webhook deliveries reaching a terminal state, split by e2a- versus endpoint-attributable outcome and delivery scope.",
		}, []string{"outcome", "scope"}),
		whNotify: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_webhook_notify_total",
			Help: "Webhook health-notification (warning/disabled email) job outcomes, by notification kind and send outcome; skipped = a staleness guard decided not to send.",
		}, []string{"kind", "outcome"}),
		whExpiredPending: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "e2a_webhook_deliveries_expired_pending_total",
			Help: "Delivery rows that hit their retention TTL still pending and were marked failed by the janitor.",
		}),
		whFanOutRescued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "e2a_webhook_fanout_rescued_total",
			Help: "Pending webhook events re-driven after their fan-out job died (discarded/pruned); a climbing rate = poison event.",
		}),
		whDeliveryRescued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "e2a_webhook_deliveries_rescued_total",
			Help: "Pending delivery rows re-driven after their delivery job died (discarded/pruned); a climbing rate = poison row.",
		}),
		whFirstTryLat: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "e2a_webhook_first_attempt_latency_seconds",
			Help:    "Webhook event→first-attempt latency per subscriber delivery (attempt start - webhook_events.created_at); first HTTP attempt only.",
			Buckets: longBuckets,
		}),
		wsConnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "e2a_ws_connects_total",
			Help: "WebSocket connections accepted and registered.",
		}),
		wsRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_ws_handshake_rejected_total",
			Help: "WebSocket handshakes rejected before upgrade, by reason.",
		}, []string{"reason"}),
		wsDisconnects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_ws_disconnects_total",
			Help: "WebSocket disconnects, by reason.",
		}, []string{"reason"}),
		wsDrained: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "e2a_ws_drained_messages_total",
			Help: "Unread messages pushed during WebSocket connect-drain.",
		}),
		wsSendFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "e2a_ws_send_failures_total",
			Help: "Failed pushes to a registered WebSocket connection.",
		}),
		wsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "e2a_ws_connections_active",
			Help: "Currently registered WebSocket connections.",
		}),
		inboundProcess: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_inbound_process_total",
			Help: "Async inbound-intake worker outcomes.",
		}, []string{"outcome"}),
		inboundDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "e2a_inbound_process_duration_seconds",
			Help:    "Async inbound-intake processing duration (processed outcomes).",
			Buckets: fastBuckets,
		}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "e2a_queue_depth",
			Help: "River job counts by queue and state (sampled by the queue-stats maintenance job).",
		}, []string{"queue", "state"}),
		queueOldestAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "e2a_queue_oldest_age_seconds",
			Help: "Age of the oldest runnable (available) job per queue.",
		}, []string{"queue"}),
		threadResolution: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_thread_resolution_total",
			Help: "Mailbox-local thread identity decisions, by bounded resolution source.",
		}, []string{"source"}),
		threadHeaderParse: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_thread_header_parse_failures_total",
			Help: "Inbound RFC threading headers rejected by the strict parser.",
		}, []string{"header"}),
		threadNull: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "e2a_thread_null_messages",
			Help: "Recent sampled messages without a materialized thread ID, by age bucket.",
		}, []string{"age_bucket"}),
		threadViolations: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "e2a_thread_invariant_violations",
			Help: "Current thread-topology violations found in the bounded audit sample.",
		}, []string{"kind"}),
		threadRelationship: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "e2a_thread_relationship_percent",
			Help: "Sampled mailbox-local thread/conversation relationship percentages.",
		}, []string{"kind"}),

		outboxPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_outbox_events_published_total",
			Help: "Webhook events written to the outbox.",
		}, []string{"type"}),
		outboxFanOut: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_outbox_events_fanout_total",
			Help: "Outbox events fanned out to matched webhooks.",
		}, []string{"type"}),
		outboxMatched: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_outbox_fanout_matched_total",
			Help: "Subscriber delivery rows written during fan-out.",
		}, []string{"type"}),
		outboxNoMatch: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_outbox_events_nomatch_total",
			Help: "Outbox events with zero matching subscribers.",
		}, []string{"type"}),
		outboxFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_outbox_failures_total",
			Help: "Outbox worker/publish failures by stage.",
		}, []string{"stage"}),
		redeliver: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_redeliver_requests_total",
			Help: "Customer-driven webhook redelivery requests.",
		}, []string{"scope"}),
		janitorDeleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_janitor_rows_deleted_total",
			Help: "Rows deleted by the cleanup janitor, by table.",
		}, []string{"table"}),
		contactDue: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_contact_due_events_total",
			Help: "contact.due outreach wake-ups, by outcome.",
		}, []string{"outcome"}),
		delegatedFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_delegated_auth_failures_total",
			Help: "Delegated-token authentication failures by category (invalid_token, unknown_subject, verifier_unavailable, identity_store_failure).",
		}, []string{"category"}),
		delegatedRefresh: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_delegated_jwks_refresh_total",
			Help: "Delegated-verifier JWKS refresh outcomes (success, key_absent, transport_error, parse_error, rate_limited).",
		}, []string{"outcome"}),
		notifyMissed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "e2a_notify_missed_total",
			Help: "Fallback-poll wakeups that LISTEN/NOTIFY missed.",
		}),
		publisherLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "e2a_webhook_publisher_lag_seconds",
			Help: "Age of the oldest pending webhook_events row.",
		}),
	}

	// These bounded counter families feed increase()-based alert policies.
	// Materialize every allowed child (plus the collapsed "other" child) at
	// zero so failures between process start and the first scrape are observed
	// as deltas instead of becoming an unobservable initial counter value.
	for category := range delegatedFailSet {
		p.delegatedFailures.WithLabelValues(category).Add(0)
	}
	p.delegatedFailures.WithLabelValues("other").Add(0)
	for outcome := range delegatedRefreshSet {
		p.delegatedRefresh.WithLabelValues(outcome).Add(0)
	}
	p.delegatedRefresh.WithLabelValues("other").Add(0)

	registerer.MustRegister(
		p.httpRequests, p.httpDuration,
		p.smtpInbound, p.smtpDuration,
		p.outQueueWait, p.outTerminal, p.outTerminalLat, p.outAttempts, p.outAttemptDur, p.outRateDeferred,
		p.whAttempts, p.whAttemptDur, p.whTerminal, p.whNotify, p.whExpiredPending, p.whFanOutRescued, p.whDeliveryRescued, p.whFirstTryLat,
		p.wsConnects, p.wsDisconnects, p.wsRejected, p.wsDrained, p.wsSendFailures, p.wsActive,
		p.delegatedFailures, p.delegatedRefresh,
		p.inboundProcess, p.inboundDuration,
		p.queueDepth, p.queueOldestAge,
		p.threadResolution, p.threadHeaderParse, p.threadNull, p.threadViolations, p.threadRelationship,
		p.outboxPublished, p.outboxFanOut, p.outboxMatched, p.outboxNoMatch,
		p.outboxFailures, p.redeliver, p.janitorDeleted, p.contactDue, p.notifyMissed, p.publisherLag,
	)
	return p
}

// Handler returns the exposition endpoint for this backend's registry.
func (p *Prom) Handler() http.Handler {
	return promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{})
}

// capped admits a label value until the family's series cap is reached,
// then collapses new values to "other". Existing values keep counting.
func (p *Prom) capped(seen map[string]struct{}, cap int, v string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := seen[v]; ok {
		return v
	}
	if len(seen) >= cap {
		return "other"
	}
	seen[v] = struct{}{}
	return v
}

// --- SLI instruments ---

func (p *Prom) HTTPRequest(method, route, statusClass string, seconds float64) {
	m := enum(methodSet, method)
	r := p.capped(p.routesSeen, maxRouteSeries, route)
	p.httpRequests.WithLabelValues(m, r, enum(classSet, statusClass)).Inc()
	// seconds < 0 = "no duration sample" (hijacked WS connections: their
	// handler runtime is the connection lifetime and would pin the p99).
	if seconds >= 0 {
		p.httpDuration.WithLabelValues(m, r).Observe(seconds)
	}
}

func (p *Prom) SMTPInbound(outcome string, seconds float64) {
	o := enum(smtpSet, outcome)
	p.smtpInbound.WithLabelValues(o).Inc()
	// Only fully-processed DATA transactions feed the duration histogram
	// (it backs the 2s latency SLO): RCPT-stage rejections have no DATA
	// phase, and rejected_line_too_long aborts mid-read, so its elapsed
	// time is not a processing latency.
	if o == "accepted" || o == "accepted_dedup" || o == "tempfail" {
		p.smtpDuration.Observe(seconds)
	}
}

func (p *Prom) OutboundQueueWait(seconds float64) { p.outQueueWait.Observe(seconds) }

func (p *Prom) OutboundTerminal(outcome string) {
	p.outTerminal.WithLabelValues(enum(outTermSet, outcome)).Inc()
}

func (p *Prom) OutboundTerminalLatency(seconds float64) { p.outTerminalLat.Observe(seconds) }

func (p *Prom) OutboundAttempt(outcome string, seconds float64) {
	p.outAttempts.WithLabelValues(enum(outAttemptSet, outcome)).Inc()
	p.outAttemptDur.Observe(seconds)
}

func (p *Prom) OutboundRateDeferred() { p.outRateDeferred.Inc() }

func (p *Prom) WebhookAttempt(outcome, statusClass string, seconds float64) {
	p.whAttempts.WithLabelValues(enum(whSet, outcome), enum(classSet, statusClass)).Inc()
	// seconds < 0 = "no duration sample" (outcomes with no HTTP POST —
	// webhook_deleted / skipped_disabled — must not drag quantiles to 0).
	if seconds >= 0 {
		p.whAttemptDur.Observe(seconds)
	}
}

func (p *Prom) WebhookTerminal(outcome, scope string, count int) {
	if count > 0 {
		p.whTerminal.WithLabelValues(enum(whTerminalSet, outcome), enum(whScopeSet, scope)).Add(float64(count))
	}
}

func (p *Prom) WebhookNotify(kind, outcome string) {
	p.whNotify.WithLabelValues(enum(whNotifyKindSet, kind), enum(whNotifyOutcomeSet, outcome)).Inc()
}

func (p *Prom) WebhookExpiredPending(count int) {
	if count > 0 {
		p.whExpiredPending.Add(float64(count))
	}
}

func (p *Prom) WebhookFanOutRescued(count int) {
	if count > 0 {
		p.whFanOutRescued.Add(float64(count))
	}
}

func (p *Prom) WebhookDeliveryRescued(count int) {
	if count > 0 {
		p.whDeliveryRescued.Add(float64(count))
	}
}

func (p *Prom) WSConnected()      { p.wsConnects.Inc() }
func (p *Prom) WSSendFailure()    { p.wsSendFailures.Inc() }
func (p *Prom) SetWSActive(n int) { p.wsActive.Set(float64(n)) }

func (p *Prom) WSHandshakeRejected(reason string) {
	p.wsRejected.WithLabelValues(enum(wsRejectSet, reason)).Inc()
}

func (p *Prom) DelegatedAuthFailure(category string) {
	p.delegatedFailures.WithLabelValues(enum(delegatedFailSet, category)).Inc()
}

func (p *Prom) DelegatedJWKSRefresh(outcome string) {
	p.delegatedRefresh.WithLabelValues(enum(delegatedRefreshSet, outcome)).Inc()
}

func (p *Prom) WebhookFirstAttemptLatency(seconds float64) { p.whFirstTryLat.Observe(seconds) }

func (p *Prom) WSDisconnected(reason string) {
	p.wsDisconnects.WithLabelValues(enum(wsReasonSet, reason)).Inc()
}

func (p *Prom) WSDrained(count int) {
	if count > 0 {
		p.wsDrained.Add(float64(count))
	}
}

func (p *Prom) InboundProcess(outcome string, seconds float64) {
	o := enum(inboundSet, outcome)
	p.inboundProcess.WithLabelValues(o).Inc()
	if o == "processed" {
		p.inboundDuration.Observe(seconds)
	}
}

func (p *Prom) SetQueueDepth(queue, state string, n int) {
	p.queueDepth.WithLabelValues(enum(queueSet, queue), enum(stateSet, state)).Set(float64(n))
}

func (p *Prom) SetQueueOldestAge(queue string, seconds float64) {
	p.queueOldestAge.WithLabelValues(enum(queueSet, queue)).Set(seconds)
}

func (p *Prom) ThreadResolution(source string, count int) {
	if count > 0 {
		p.threadResolution.WithLabelValues(enum(threadResolutionSet, source)).Add(float64(count))
	}
}

func (p *Prom) ThreadHeaderParseFailure(header string) {
	p.threadHeaderParse.WithLabelValues(enum(threadHeaderSet, header)).Inc()
}

func (p *Prom) SetThreadNullMessages(ageBucket string, count int) {
	p.threadNull.WithLabelValues(enum(threadNullAgeSet, ageBucket)).Set(float64(max(count, 0)))
}

func (p *Prom) SetThreadInvariantViolations(kind string, count int) {
	p.threadViolations.WithLabelValues(enum(threadViolationSet, kind)).Set(float64(max(count, 0)))
}

func (p *Prom) SetThreadRelationshipPercent(kind string, percent float64) {
	p.threadRelationship.WithLabelValues(enum(threadRelationshipSet, kind)).Set(clampPercent(percent))
}

func clampPercent(percent float64) float64 {
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

// --- legacy outbox instruments ---

func (p *Prom) OutboxEventsPublished(eventType string) {
	p.outboxPublished.WithLabelValues(p.capped(p.typesSeen, maxTypeSeries, eventType)).Inc()
}

func (p *Prom) OutboxEventsFanOut(eventType string, matched int) {
	t := p.capped(p.typesSeen, maxTypeSeries, eventType)
	p.outboxFanOut.WithLabelValues(t).Inc()
	if matched > 0 {
		p.outboxMatched.WithLabelValues(t).Add(float64(matched))
	}
}

func (p *Prom) OutboxEventsNoMatch(eventType string) {
	p.outboxNoMatch.WithLabelValues(p.capped(p.typesSeen, maxTypeSeries, eventType)).Inc()
}

func (p *Prom) OutboxFailures(stage string) {
	p.outboxFailures.WithLabelValues(enum(stageSet, stage)).Inc()
}

func (p *Prom) RedeliverRequests(scope string) {
	p.redeliver.WithLabelValues(enum(scopeSet, scope)).Inc()
}

func (p *Prom) JanitorRowsDeleted(table string, count int) {
	if count > 0 {
		p.janitorDeleted.WithLabelValues(enum(tableSet, table)).Add(float64(count))
	}
}

// ContactDuePublished counts wake-ups that reached the outbox. A sustained
// zero while engagements are enrolled and scheduled means the sweep is not
// running, which is silent from the outside — the agent simply never wakes.
func (p *Prom) ContactDuePublished(count int) {
	if count > 0 {
		p.contactDue.WithLabelValues("published").Add(float64(count))
	}
}

// ContactDueFailed counts wake-ups whose publish failed. Non-zero means an
// agent was NOT woken for a schedule that has already been consumed, so the
// miss will not retry — worth alerting on rather than merely graphing.
func (p *Prom) ContactDueFailed(count int) {
	if count > 0 {
		p.contactDue.WithLabelValues("failed").Add(float64(count))
	}
}

func (p *Prom) NotifyMissed()               { p.notifyMissed.Inc() }
func (p *Prom) SetPublisherLag(sec float64) { p.publisherLag.Set(sec) }

// Compile guard.
var _ Metrics = (*Prom)(nil)
