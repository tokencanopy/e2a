package telemetry

import (
	"net/http"
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

	httpRequests    *prometheus.CounterVec
	httpDuration    *prometheus.HistogramVec
	smtpInbound     *prometheus.CounterVec
	smtpDuration    prometheus.Histogram
	outQueueWait    prometheus.Histogram
	outTerminal     *prometheus.CounterVec
	outTerminalLat  prometheus.Histogram
	outAttempts     *prometheus.CounterVec
	outAttemptDur   prometheus.Histogram
	whAttempts      *prometheus.CounterVec
	whAttemptDur    prometheus.Histogram
	whFirstTryLat   prometheus.Histogram
	wsConnects      prometheus.Counter
	wsDisconnects   *prometheus.CounterVec
	wsRejected      *prometheus.CounterVec
	wsDrained       prometheus.Counter
	wsSendFailures  prometheus.Counter
	wsActive        prometheus.Gauge
	inboundProcess  *prometheus.CounterVec
	inboundDuration prometheus.Histogram
	queueDepth      *prometheus.GaugeVec
	queueOldestAge  *prometheus.GaugeVec

	// legacy outbox instruments (same events the Log backend emits)
	outboxPublished *prometheus.CounterVec
	outboxFanOut    *prometheus.CounterVec
	outboxMatched   *prometheus.CounterVec
	outboxNoMatch   *prometheus.CounterVec
	outboxFailures  *prometheus.CounterVec
	redeliver       *prometheus.CounterVec
	janitorDeleted  *prometheus.CounterVec
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
		"rejected_unknown_recipient", "rejected_unverified_domain", "rejected_quota")
	outTermSet = set("sent", "failed_suppressed", "failed_provider",
		"failed_local_retries", "failed_cancelled")
	outAttemptSet = set("success", "temporary_failure", "permanent_failure")
	whSet         = set("delivered", "retryable_failure", "exhausted",
		"webhook_deleted", "skipped_disabled")
	wsReasonSet = set("replaced", "ping_timeout", "client_close", "error", "shutdown")
	wsRejectSet = set("unauthorized", "not_found", "forbidden", "upgrade_failed", "internal_error")
	inboundSet  = set("processed", "noop", "failed_recipient_gone",
		"failed_exhausted", "retryable")
	queueSet = set("outbound", "inbound", "webhook", "maintenance", "notify", "default")
	stateSet = set("available", "running", "retryable", "scheduled")
	stageSet = set("lease", "list_webhooks", "insert_delivery", "update_status", "publish")
	scopeSet = set("single", "since")
	tableSet = set("webhook_events", "webhook_subscriber_deliveries",
		"webhook_deliveries", "messages", "agent_identities",
		"user_sessions", "oauth")
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
	fastBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
	waitBuckets = []float64{.05, .1, .25, .5, 1, 2.5, 5, 15, 30, 60, 120, 300, 900, 3600}
	// longBuckets spans seconds-to-days: outbound acceptance→terminal can
	// legitimately reach the 72h retry horizon under a provider outage,
	// and webhook event→first-attempt includes fan-out + queue wait. The
	// 60s and 300s edges are the SLO thresholds (docs/observability.md).
	longBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 86400, 259200}
)

func NewProm() *Prom {
	reg := prometheus.NewRegistry()
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
			Help:    "Outbound acceptance→terminal latency per message (terminal occurred_at - messages.created_at), observed exactly once per message.",
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
		whAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "e2a_webhook_attempts_total",
			Help: "Webhook delivery attempts, by outcome and endpoint response class.",
		}, []string{"outcome", "status_class"}),
		whAttemptDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "e2a_webhook_attempt_duration_seconds",
			Help:    "Webhook delivery attempt duration (HTTP POST to subscriber).",
			Buckets: fastBuckets,
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
		notifyMissed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "e2a_notify_missed_total",
			Help: "Fallback-poll wakeups that LISTEN/NOTIFY missed.",
		}),
		publisherLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "e2a_webhook_publisher_lag_seconds",
			Help: "Age of the oldest pending webhook_events row.",
		}),
	}

	reg.MustRegister(
		p.httpRequests, p.httpDuration,
		p.smtpInbound, p.smtpDuration,
		p.outQueueWait, p.outTerminal, p.outTerminalLat, p.outAttempts, p.outAttemptDur,
		p.whAttempts, p.whAttemptDur, p.whFirstTryLat,
		p.wsConnects, p.wsDisconnects, p.wsRejected, p.wsDrained, p.wsSendFailures, p.wsActive,
		p.inboundProcess, p.inboundDuration,
		p.queueDepth, p.queueOldestAge,
		p.outboxPublished, p.outboxFanOut, p.outboxMatched, p.outboxNoMatch,
		p.outboxFailures, p.redeliver, p.janitorDeleted, p.notifyMissed, p.publisherLag,
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
	// RCPT-stage rejections carry no DATA duration; only observe real ones.
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

func (p *Prom) WebhookAttempt(outcome, statusClass string, seconds float64) {
	p.whAttempts.WithLabelValues(enum(whSet, outcome), enum(classSet, statusClass)).Inc()
	// seconds < 0 = "no duration sample" (outcomes with no HTTP POST —
	// webhook_deleted / skipped_disabled — must not drag quantiles to 0).
	if seconds >= 0 {
		p.whAttemptDur.Observe(seconds)
	}
}

func (p *Prom) WSConnected()      { p.wsConnects.Inc() }
func (p *Prom) WSSendFailure()    { p.wsSendFailures.Inc() }
func (p *Prom) SetWSActive(n int) { p.wsActive.Set(float64(n)) }

func (p *Prom) WSHandshakeRejected(reason string) {
	p.wsRejected.WithLabelValues(enum(wsRejectSet, reason)).Inc()
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

func (p *Prom) NotifyMissed()               { p.notifyMissed.Inc() }
func (p *Prom) SetPublisherLag(sec float64) { p.publisherLag.Set(sec) }

// Compile guard.
var _ Metrics = (*Prom)(nil)
