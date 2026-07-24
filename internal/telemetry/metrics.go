// Package telemetry defines the metrics interface for the e2a backend.
// Implementations:
//
//   - NoOp — production default until an operator wires a real backend.
//   - Log  — structured log emitter; cheap; aggregator-friendly.
//   - (future) Prometheus, OTLP, statsd, etc.
//
// The interface is small by design. Call sites should depend on
// telemetry.Metrics, not on a concrete backend. To swap backends,
// change the constructor at the cmd/e2a/main.go wiring; nothing else
// moves.
package telemetry

import (
	"log"
	"sync/atomic"
)

// Metrics is the observability surface for the slice 10 design.
// Counter-style methods record a discrete event; SetPublisherLag is a
// gauge that should be set on each tick.
//
// Stable across implementations. Adding a new metric is additive (add
// a method, default it to a no-op on existing implementations).
type Metrics interface {
	// OutboxEventsPublished is incremented each time PublishTx or
	// PublishBestEffortTx successfully writes a webhook_events row.
	OutboxEventsPublished(eventType string)

	// OutboxEventsFanOut is incremented each time the worker finishes
	// fanning an event out to its matched webhooks. matched is the
	// number of webhook_subscriber_deliveries rows written.
	OutboxEventsFanOut(eventType string, matched int)

	// OutboxEventsNoMatch is incremented each time the worker
	// transitions an event to status='no_match' because zero
	// subscribers matched. Useful for spotting "why didn't my
	// webhook fire?"
	OutboxEventsNoMatch(eventType string)

	// OutboxFailures is incremented on any outbox failure — worker-side
	// (stage in {"lease", "list_webhooks", "insert_delivery", "update_status"})
	// or emit-side when a fire-and-forget producer's PublishTx fails and the
	// event is DROPPED (stage "publish" — today only the outbound
	// email.blocked producer; other producers either surface the error to
	// the caller or log via PublishBestEffortTx). A non-zero "publish" rate
	// means contract events are silently missing from the log.
	OutboxFailures(stage string)

	// RedeliverRequests is incremented on each customer-driven replay.
	// scope in {"single", "since"}.
	RedeliverRequests(scope string)

	// JanitorRowsDeleted is incremented by the cleanup tick.
	// table in {"webhook_events", "webhook_subscriber_deliveries", "webhook_deliveries", "messages", "user_sessions", "oauth"}.
	JanitorRowsDeleted(table string, count int)

	// NotifyMissed is incremented when the 1-second fallback poll
	// finds work that LISTEN/NOTIFY didn't wake us for. A non-zero
	// rate indicates reconnect churn or a dropped notification.
	NotifyMissed()

	// SetPublisherLag is a gauge: the age in seconds of the oldest
	// pending webhook_events row. Should be set on every Tick. Alert
	// if it stays > 30s.
	SetPublisherLag(seconds float64)

	// --- SLI instruments (docs/observability.md) ---
	//
	// Label arguments are normalized by the backend: values outside the
	// documented enum collapse to "other", so callers can pass what they
	// know without minting unbounded series. Never pass message content,
	// addresses, URLs, or credentials — even though the backend would
	// collapse them, the call site must not depend on that.

	// HTTPRequest records one served HTTP request. route is the chi
	// route pattern (e.g. "/v1/agents/{email}"), NEVER the raw path.
	// statusClass is "1xx".."5xx". A negative seconds means "count the
	// request but record no duration sample" — used for hijacked
	// (WebSocket) connections, whose handler runtime is the connection
	// lifetime, not a request latency.
	HTTPRequest(method, route, statusClass string, seconds float64)

	// SMTPInbound records one SMTP intake decision. outcome ∈
	// {accepted, accepted_dedup, tempfail, rejected_unknown_recipient,
	// rejected_unverified_domain, rejected_quota}. Units differ by
	// stage: accepted/accepted_dedup/tempfail are per DATA transaction;
	// rejected_* are per rejected RCPT command (one transaction can
	// emit several rejections and still accept). seconds is DATA
	// processing time (0 for RCPT-stage rejections).
	SMTPInbound(outcome string, seconds float64)

	// OutboundQueueWait records due→pickup latency for one outbound
	// send attempt (River attempted_at − scheduled_at; created_at would
	// count each retry's full backoff as queue wait).
	OutboundQueueWait(seconds float64)

	// OutboundTerminal records a terminal outcome for an outbound
	// message. outcome ∈ {sent, failed_suppressed, failed_provider,
	// failed_local_retries, failed_cancelled}. Exactly one per message:
	// a deferred final attempt is counted by the terminal reconciler
	// when it settles.
	OutboundTerminal(outcome string)

	// OutboundTerminalLatency records acceptance→terminal latency for
	// one outbound message (the terminal write's occurred_at −
	// messages.created_at). Observed exactly once per message,
	// co-located with the OutboundTerminal emission so the two share
	// their exactly-once contract (the SNS-feedback settle path stays
	// uninstrumented for both — see the guard comment at the worker's
	// MarkSent emit in internal/outboundsend).
	OutboundTerminalLatency(seconds float64)

	// OutboundAttempt records one submission attempt to the upstream
	// relay. outcome ∈ {success, temporary_failure, permanent_failure}.
	// seconds is the submission duration.
	OutboundAttempt(outcome string, seconds float64)

	// WebhookAttempt records one webhook delivery attempt. outcome ∈
	// {delivered, retryable_failure, exhausted, webhook_deleted,
	// skipped_disabled}. statusClass is the HTTP status class of the
	// endpoint's response, or "none" when no response was received
	// (connect/DNS/SSRF-blocked).
	WebhookAttempt(outcome, statusClass string, seconds float64)

	// WebhookFirstAttemptLatency records event→first-attempt latency
	// for one subscriber delivery (attempt start − the webhook_events
	// row's created_at). Observed only on a delivery's FIRST HTTP
	// attempt — retries and the no-POST outcomes (webhook_deleted,
	// skipped_disabled) never observe.
	WebhookFirstAttemptLatency(seconds float64)

	// WSConnected / WSDisconnected count WebSocket connection
	// lifecycle events. reason ∈ {replaced, ping_timeout,
	// client_close, error, shutdown}.
	WSConnected()
	WSDisconnected(reason string)

	// WSHandshakeRejected counts pre-upgrade WebSocket handshake
	// rejections. reason ∈ {unauthorized, not_found, forbidden,
	// upgrade_failed, internal_error}. Never label with emails or tokens.
	WSHandshakeRejected(reason string)

	// WSDrained counts unread messages pushed during connect-drain.
	WSDrained(count int)

	// WSSendFailure counts failed pushes to a registered connection.
	WSSendFailure()

	// SetWSActive is a gauge: current registered WS connections.
	SetWSActive(n int)

	// InboundProcess records an async inbound-worker outcome. outcome
	// ∈ {processed, noop, failed_recipient_gone, failed_exhausted,
	// retryable}.
	InboundProcess(outcome string, seconds float64)

	// SetQueueDepth / SetQueueOldestAge are gauges sampled by the
	// queue-stats maintenance job. queue ∈ jobs.Queue* names; state ∈
	// {available, running, retryable, scheduled}. Oldest age is for
	// runnable (available) jobs only — a growing value means workers
	// are not keeping up.
	SetQueueDepth(queue, state string, n int)
	SetQueueOldestAge(queue string, seconds float64)
}

// NoOp swallows every call. Default for tests that don't care.
type NoOp struct{}

func (NoOp) OutboxEventsPublished(string)   {}
func (NoOp) OutboxEventsFanOut(string, int) {}
func (NoOp) OutboxEventsNoMatch(string)     {}
func (NoOp) OutboxFailures(string)          {}
func (NoOp) RedeliverRequests(string)       {}
func (NoOp) JanitorRowsDeleted(string, int) {}
func (NoOp) NotifyMissed()                  {}
func (NoOp) SetPublisherLag(float64)        {}

func (NoOp) HTTPRequest(string, string, string, float64) {}
func (NoOp) SMTPInbound(string, float64)                 {}
func (NoOp) OutboundQueueWait(float64)                   {}
func (NoOp) OutboundTerminal(string)                     {}
func (NoOp) OutboundTerminalLatency(float64)             {}
func (NoOp) OutboundAttempt(string, float64)             {}
func (NoOp) WebhookAttempt(string, string, float64)      {}
func (NoOp) WebhookFirstAttemptLatency(float64)          {}
func (NoOp) WSConnected()                                {}
func (NoOp) WSDisconnected(string)                       {}
func (NoOp) WSHandshakeRejected(string)                  {}
func (NoOp) WSDrained(int)                               {}
func (NoOp) WSSendFailure()                              {}
func (NoOp) SetWSActive(int)                             {}
func (NoOp) InboundProcess(string, float64)              {}
func (NoOp) SetQueueDepth(string, string, int)           {}
func (NoOp) SetQueueOldestAge(string, float64)           {}

// Log emits a structured log line for every metric call. Cheap and
// portable; production aggregators (Loki, CloudWatch, Datadog) can
// build counters and gauges from these directly.
//
// All lines share the [metrics] prefix so they're easy to filter.
// Format is key=value space-separated, which both jq and Splunk parse
// natively.
type Log struct {
	// inflightPublisherLag is set by SetPublisherLag and emitted
	// every N calls (currently 60 — once a minute at the worker's
	// 1s poll cadence). Saves log volume.
	calls atomic.Int64
}

func NewLog() *Log { return &Log{} }

func (l *Log) OutboxEventsPublished(eventType string) {
	log.Printf("[metrics] event=outbox.published type=%s", eventType)
}

func (l *Log) OutboxEventsFanOut(eventType string, matched int) {
	log.Printf("[metrics] event=outbox.fanout type=%s matched=%d", eventType, matched)
}

func (l *Log) OutboxEventsNoMatch(eventType string) {
	log.Printf("[metrics] event=outbox.no_match type=%s", eventType)
}

func (l *Log) OutboxFailures(stage string) {
	log.Printf("[metrics] event=outbox.failure stage=%s", stage)
}

func (l *Log) RedeliverRequests(scope string) {
	log.Printf("[metrics] event=redeliver.request scope=%s", scope)
}

func (l *Log) JanitorRowsDeleted(table string, count int) {
	if count == 0 {
		return // skip noise when nothing was cleaned
	}
	log.Printf("[metrics] event=janitor.delete table=%s count=%d", table, count)
}

func (l *Log) NotifyMissed() {
	log.Printf("[metrics] event=notify.missed")
}

func (l *Log) SetPublisherLag(seconds float64) {
	// Rate-limit: emit every 60th call (~once a minute at 1s poll).
	n := l.calls.Add(1)
	if n%60 == 0 || seconds > 30 {
		log.Printf("[metrics] gauge=publisher.lag_seconds value=%.2f", seconds)
	}
}

// --- SLI instruments on the Log backend ---
//
// The Log backend exists for aggregator-based operations (Loki, CloudWatch)
// where one line per event is acceptable. Per-request/high-rate instruments
// (HTTPRequest, OutboundQueueWait) are intentionally silent here — a log
// line per HTTP request would swamp the stream; operators who want those
// SLIs enable the Prometheus backend (metrics.enabled in config).
// Moderate-rate outcome events still log so a log-only deployment keeps
// SMTP/outbound/webhook visibility.

func (l *Log) HTTPRequest(string, string, string, float64) {} // high-rate: Prom only
func (l *Log) OutboundQueueWait(float64)                   {} // high-rate: Prom only

func (l *Log) SMTPInbound(outcome string, seconds float64) {
	log.Printf("[metrics] event=smtp.inbound outcome=%s duration=%.3f", outcome, seconds)
}

func (l *Log) OutboundTerminal(outcome string) {
	log.Printf("[metrics] event=outbound.terminal outcome=%s", outcome)
}

func (l *Log) OutboundTerminalLatency(seconds float64) {
	log.Printf("[metrics] event=outbound.terminal_latency duration=%.3f", seconds)
}

func (l *Log) OutboundAttempt(outcome string, seconds float64) {
	log.Printf("[metrics] event=outbound.attempt outcome=%s duration=%.3f", outcome, seconds)
}

func (l *Log) WebhookAttempt(outcome, statusClass string, seconds float64) {
	log.Printf("[metrics] event=webhook.attempt outcome=%s status_class=%s duration=%.3f", outcome, statusClass, seconds)
}

func (l *Log) WebhookFirstAttemptLatency(seconds float64) {
	log.Printf("[metrics] event=webhook.first_attempt_latency duration=%.3f", seconds)
}

func (l *Log) WSConnected() {
	log.Printf("[metrics] event=ws.connected")
}

func (l *Log) WSHandshakeRejected(reason string) {
	log.Printf("[metrics] event=ws.handshake_rejected reason=%s", reason)
}

func (l *Log) WSDisconnected(reason string) {
	log.Printf("[metrics] event=ws.disconnected reason=%s", reason)
}

func (l *Log) WSDrained(count int) {
	if count == 0 {
		return
	}
	log.Printf("[metrics] event=ws.drained count=%d", count)
}

func (l *Log) WSSendFailure() {
	log.Printf("[metrics] event=ws.send_failure")
}

func (l *Log) SetWSActive(int) {} // gauge churns on every connect/disconnect; Prom only

func (l *Log) InboundProcess(outcome string, seconds float64) {
	log.Printf("[metrics] event=inbound.process outcome=%s duration=%.3f", outcome, seconds)
}

func (l *Log) SetQueueDepth(queue, state string, n int) {
	if n == 0 {
		return // skip noise: empty queues are the healthy steady state
	}
	log.Printf("[metrics] gauge=queue.depth queue=%s state=%s value=%d", queue, state, n)
}

func (l *Log) SetQueueOldestAge(queue string, seconds float64) {
	if seconds < 30 {
		return // same alert-threshold discipline as SetPublisherLag
	}
	log.Printf("[metrics] gauge=queue.oldest_age_seconds queue=%s value=%.2f", queue, seconds)
}

// Compile guard.
var _ Metrics = NoOp{}
var _ Metrics = (*Log)(nil)
