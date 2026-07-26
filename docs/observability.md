# Observability: metrics, SLIs, and SLO targets

This page documents the open-source measurement primitives e2a ships for
operating against production SLOs: the Prometheus metric catalog, the health
endpoints, the external prober signals, aggregation guidance (PromQL), and
the initial SLO targets the hosted service holds itself to.

Related: [`docs/design/prober-selftest.md`](design/prober-selftest.md) (the
prober/selftest design), [`docs/deployment.md`](deployment.md) (running the
server), `config.example.yaml` (the `metrics:` block).

## Enabling metrics

Metrics are **off by default**: the API surface is unchanged and the server
emits structured `[metrics]` log lines for moderate-rate outcome events
(SMTP, outbound, webhook, WS), suitable for log-based aggregators — but the
per-request SLIs (HTTP, queue wait) and all gauges exist only on the
Prometheus backend. The 30s queue-stats sampler runs in both modes (two
cheap `river_job` GROUP BY reads). Enable Prometheus exposition with:

```yaml
metrics:
  enabled: true                  # or E2A_METRICS_ENABLED=true
  listen_addr: "127.0.0.1:9091"  # or E2A_METRICS_LISTEN_ADDR
```

`GET /metrics` is served on a **separate listener**, never on the public API
handler, and binds loopback by default. The endpoint is unauthenticated —
binding a non-loopback address logs a warning; front it with your own
network policy before exposing it to a scraper on another host.

### Label-cardinality contract

Metric labels never carry message content, email addresses, webhook URLs,
credentials, or any other tenant data. Every label value passes through an
enum allowlist in the telemetry backend — values outside the documented sets
collapse to `"other"` — and the two code-defined open sets (HTTP route
patterns, webhook event types) are additionally bounded by hard series caps
(256 routes, 64 event types; overflow collapses to `"other"`). The HTTP
`route` label is always the chi route *pattern* (`/v1/agents/{email}`), never
a raw path. These boundaries are pinned by unit tests in
`internal/telemetry`.

## Health endpoints (probes)

| Endpoint | Depth | Meaning | Use for |
|---|---|---|---|
| `GET /api/health` | Liveness | Process is up and serving. Checks **nothing** else — never restart an instance because its DB blipped. | Container restart policy (the Dockerfile healthcheck), LB TCP-level checks |
| `GET /readyz` | Readiness | Instance-local "ready to serve": DB reachable, latest embedded migration applied, **not draining**. Returns `503 {"status":"not_ready","reason":...}` otherwise. From the moment graceful shutdown begins it reports `reason: "draining"` while liveness stays green, so the LB drops the instance before in-flight work drains. | LB routing decisions, K8s `readinessProbe`, deploy gates |
| `GET /selftest` | Deep | Dependency diagnostics (DB, SMTP listener, migrations), IETF `application/health+json`, auth-gated by `E2A_INTERNAL_API_SECRET` (fail-closed in production). | Monitoring/diagnosis — **never** an orchestrator restart loop |

Readiness deliberately checks only what this instance needs to serve traffic;
it does not round-trip downstream providers (that is the prober's job — a
SES outage must not knock every instance out of rotation).

## Metric catalog (server, `e2a_*`)

### HTTP API

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_http_requests_total` | counter | `method`, `route`, `status_class` | Requests served. `route` is the chi pattern; requests that fall through to the legacy (non-`/v1`) mux appear as `route="/legacy"`; `status_class` ∈ `1xx..5xx` (WebSocket upgrades count as `1xx`). |
| `e2a_http_request_duration_seconds` | histogram | `method`, `route` | Request latency, timed across auth, Huma, handler, and legacy fallthrough. Includes an exact `0.75` second SLO bucket. Hijacked (WebSocket) connections are **excluded** — their handler runtime is the connection lifetime, which would otherwise pin the p99. |

### SMTP intake (relay edge)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_smtp_inbound_total` | counter | `outcome` | SMTP intake decisions. Units differ by stage: `accepted` (250), `accepted_dedup` (250 on a lost-ack retry), and `tempfail` (451 — durable persist/enqueue failed) are one per DATA transaction; `rejected_unknown_recipient` / `rejected_unverified_domain` (550) and `rejected_quota` (552) are one per rejected RCPT command — a single transaction can emit several rejections and still accept for its remaining recipients. A DATA phase aborted mid-read (client dropped, size limit) records no outcome. |
| `e2a_smtp_inbound_duration_seconds` | histogram | — | DATA-phase processing time (accepted/tempfail outcomes only; RCPT rejections have no DATA phase). Includes an exact `2` second SLO bucket. |

Policy rejections (550/552) are *correct* behavior, not failures — the
acceptance SLI below deliberately excludes them.

### Outbound send pipeline

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_outbound_queue_wait_seconds` | histogram | — | Due→pickup wait per attempt (River `attempted_at − scheduled_at`). Measures worker keep-up, deliberately not cumulative message age: a retry's backoff or a ramp deferral does not count as queue wait. |
| `e2a_outbound_attempts_total` | counter | `outcome` | Upstream submission attempts: `success`, `temporary_failure`, `permanent_failure`. |
| `e2a_outbound_attempt_duration_seconds` | histogram | — | Upstream (SES/SMTP relay) submission duration. |
| `e2a_outbound_terminal_total` | counter | `outcome` | Messages reaching a terminal submission outcome, **exactly once per message**: `sent`, `failed_suppressed`, `failed_provider`, `failed_local_retries`, `failed_cancelled` (policy cancel settled by the reconciler — kept out of `failed_local_retries` so cancellations can't mask a retries-exhausted regression). A deferred final attempt is counted when the terminal reconciler settles it — as `sent` when provider-accept evidence arrived (never a false failure), else as a failure. |
| `e2a_outbound_terminal_latency_seconds` | histogram | — | Acceptance→terminal latency per message (the terminal write's occurred_at − `messages.created_at`), observed **exactly once per message**, co-located with the `e2a_outbound_terminal_total` emission so the two share their exactly-once contract (the SNS-feedback settle path is deliberately uninstrumented for both). Provider-evidence settles use the evidence's accept time as occurred_at, so they measure acceptance→provider-accept, not acceptance→sweep. Buckets span seconds→days (the 72h retry horizon is the tail). |

### Webhook delivery

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_webhook_attempts_total` | counter | `outcome`, `status_class` | Delivery attempts to subscriber endpoints: `delivered`, `retryable_failure`, `exhausted` (terminal after max attempts — including a delivery that exhausted its retries on a pre-POST infrastructure error, e.g. a sustained webhook-lookup outage), `webhook_deleted`, `skipped_disabled`. `status_class` is the endpoint's response class, or `none` when no HTTP response was received — connect/DNS/SSRF-blocked, or no POST was ever made. |
| `e2a_webhook_attempt_duration_seconds` | histogram | — | HTTP POST duration per attempt. |
| `e2a_webhook_delivery_terminal_total` | counter | `outcome`, `scope` | Delivery rows reaching a terminal state, emitted only after the terminal DB transition succeeds. `outcome` is `delivered`, `e2a_failure` (internal lookup/retry exhaustion or TTL expiry), `endpoint_failure` (the customer endpoint never accepted within eight attempts), or `excluded` (for example, a deleted webhook). `scope` is `initial`, `replay`, `test`, or `unknown`; `unknown` is the conservative classification when a pre-load failure or batched TTL transition cannot inspect the row. The hosted eventual-delivery SLO uses initial + unknown deliveries and excludes endpoint failures, so customer behavior cannot burn e2a's budget. |
| `e2a_webhook_first_attempt_latency_seconds` | histogram | — | Event→first-attempt latency per subscriber delivery (attempt start − the `webhook_events` row's `created_at`), observed **only on a first-delivery row's first HTTP attempt** (no recorded prior attempt — regardless of River attempt number, so a first POST delayed by pre-POST failures still observes). Retries, the no-POST outcomes (`webhook_deleted`, `skipped_disabled`), replay rows (their baseline would be the original event's age), eventless `/test` deliveries, and jobs that sat through a customer-disabled window (River snooze) never observe. |
| `e2a_outbox_events_published_total` | counter | `type` | Events written to the outbox (fan-out input). |
| `e2a_outbox_events_fanout_total` / `e2a_outbox_fanout_matched_total` | counter | `type` | Fan-out completions / subscriber delivery rows written. |
| `e2a_outbox_events_nomatch_total` | counter | `type` | Events with zero matching subscribers. |
| `e2a_outbox_failures_total` | counter | `stage` | Outbox worker/publish failures (`lease`, `list_webhooks`, `insert_delivery`, `update_status`, `publish`). A non-zero `publish` rate means contract events are being dropped. |
| `e2a_webhook_publisher_lag_seconds` | gauge | — | Age of the oldest pending outbox row. Alert if it stays > 30s. |

### WebSocket

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_ws_connections_active` | gauge | — | Currently registered connections. |
| `e2a_ws_connects_total` | counter | — | Accepted + registered connections. |
| `e2a_ws_handshake_rejected_total` | counter | `reason` | Pre-upgrade handshake rejections: `unauthorized` (missing/invalid credential), `not_found` (unknown agent — including the deliberate cross-tenant 404 collapse), `forbidden` (agent-scoped credential pinned to a different agent), `upgrade_failed` (authenticated, but the WebSocket upgrade itself failed), `internal_error` (a store/infra error while authenticating or resolving the agent — the only e2a-attributable reason). Never labeled with emails or tokens. |
| `e2a_ws_disconnects_total` | counter | `reason` | `replaced` (one-conn-per-agent takeover), `ping_timeout`, `client_close`, `error`, `shutdown`. |
| `e2a_ws_drained_messages_total` | counter | — | Unread messages pushed during connect-drain. The prober's WS scenario trashes its own probe messages after each run so this stays customer signal, not prober noise. |
| `e2a_ws_send_failures_total` | counter | — | Failed pushes to a registered connection. |

### Async inbound processing (`E2A_INBOUND_MODE=async`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_inbound_process_total` | counter | `outcome` | Worker outcomes: `processed`, `noop` (idempotent re-drive), `failed_recipient_gone`, `failed_exhausted`, `retryable`. |
| `e2a_inbound_process_duration_seconds` | histogram | — | Processing duration (`processed` outcomes). |

### Job queues (River)

Sampled every 30s by a maintenance periodic; gauges zero-fill when a queue
empties.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_queue_depth` | gauge | `queue`, `state` | Job counts per queue (`outbound`, `inbound`, `webhook`, `maintenance`, `notify`, `default`) and state (`available`, `running`, `retryable`, `scheduled`). |
| `e2a_queue_oldest_age_seconds` | gauge | `queue` | Age of the oldest runnable (`available`) job. Growth means workers aren't keeping up. |

### Maintenance

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_janitor_rows_deleted_total` | counter | `table` | TTL sweep deletions. |
| `e2a_notify_missed_total` | counter | — | Fallback-poll wakeups LISTEN/NOTIFY missed (reconnect churn indicator). |
| `e2a_redeliver_requests_total` | counter | `scope` | Customer-driven webhook replays. |

## Prober metrics (external black-box, `cmd/e2a-prober`)

The prober measures what in-process metrics cannot: the availability of each
critical path **as an outside client experiences it**, including MCP (which
runs as a separate service) and the WebSocket push path. Its `/metrics`
endpoint (`E2A_PROBE_LISTEN`, default `:8090`) exposes:

| Metric | Type | Meaning |
|---|---|---|
| `e2a_selftest_success` | gauge | Last full battery run passed (1) or not (0). |
| `e2a_selftest_scenario_success{scenario}` | gauge | Per-scenario pass/fail of the last run. |
| `e2a_selftest_scenario_runs_total{scenario,outcome}` | counter | Monotonic completed scenario runs by `pass`, `warn`, or `fail`. Use this counter—not the last-result gauge—for rolling-window SLO ratios and burn alerts. It resets normally on prober restart. |
| `e2a_selftest_scenario_duration_seconds{scenario}` | gauge | Per-scenario latency of the last run — a scenario can pass while degrading; this is where it shows first. |
| `e2a_selftest_duration_seconds` | gauge | Total battery duration. |

Scenarios (all non-destructive; see `internal/selftest/scenarios.go`):
`liveness`, `auth_read`, `inbound_round_trip` (SMTP→webhook→HMAC),
`outbound_send` (real submit to the SES mailbox simulator),
`self_send_loopback`, `websocket_round_trip` (WS handshake + live push),
`agent_lifecycle` (self-cleaning ephemeral agent), `mcp_http_round_trip`
(tools/list + whoami over the deployed MCP endpoint). Set
`E2A_PROBE_REQUIRE_MCP=true` on stacks where MCP must be probed — it turns
the skip-as-pass on an unset `E2A_PROBE_MCP_URL` into a failure.

`/status` (recent runs + `consecutive_green`) is the deploy bake-gate
contract and also the natural feed for a hosted status page: scenario names
map one-to-one to status-page components, with no message content in any
result.

## SLI definitions and aggregation

Recording-rule-style PromQL over the catalog above. Windows are examples;
pick windows to match your alerting burn rates.

**HTTP availability** — fraction of non-5xx responses:

```promql
1 - (sum(rate(e2a_http_requests_total{status_class="5xx",route!="/legacy"}[5m]))
     / sum(rate(e2a_http_requests_total{route!="/legacy"}[5m])))
```

4xx responses are client errors and count as *available*. `route="/legacy"`
is excluded: it aggregates the non-`/v1` operational surface **plus** all
unmatched paths — LB health probes and internet scanner noise — which would
dilute the denominator. Panicking handlers DO count (recorded as 5xx by the
middleware's deferred sample), so a crash loop cannot hide from this SLI.

**HTTP latency** — fraction of `/v1` requests within the 750 ms p99
objective (the exact threshold bucket makes this suitable for burn alerts):

```promql
sum(rate(e2a_http_request_duration_seconds_bucket{route=~"/v1/.*",le="0.75"}[5m]))
/ sum(rate(e2a_http_request_duration_seconds_count{route=~"/v1/.*"}[5m]))
```

WebSocket upgrades never enter this histogram (see the catalog), so no
route exclusion is needed.

**SMTP acceptance** — fraction of non-policy DATA transactions accepted
(tempfails are our failures; 550/552 policy rejections are excluded):

```promql
sum(rate(e2a_smtp_inbound_total{outcome=~"accepted|accepted_dedup"}[5m]))
/ sum(rate(e2a_smtp_inbound_total{outcome=~"accepted|accepted_dedup|tempfail"}[5m]))
```

**SMTP DATA latency** — fraction completing within the 2 second p95
objective:

```promql
sum(rate(e2a_smtp_inbound_duration_seconds_bucket{le="2"}[5m]))
/ sum(rate(e2a_smtp_inbound_duration_seconds_count[5m]))
```

**Outbound submission success** — terminal outcomes that reached the
provider:

```promql
sum(rate(e2a_outbound_terminal_total{outcome="sent"}[1h]))
/ sum(rate(e2a_outbound_terminal_total{outcome!~"failed_suppressed|failed_cancelled"}[1h]))
```

`failed_suppressed` and `failed_cancelled` are excluded: suppression and
policy cancellation are e2a protecting the sender, not delivery failures.

**Outbound queue wait** — p95 pickup latency and backlog age:

```promql
sum(rate(e2a_outbound_queue_wait_seconds_bucket{le="30"}[5m]))
/ sum(rate(e2a_outbound_queue_wait_seconds_count[5m]))
max(e2a_queue_oldest_age_seconds{queue="outbound"})
```

**Outbound acceptance→terminal** — fraction of messages reaching a terminal
outcome within 5 minutes of acceptance (the `le="300"` bucket edge is the
SLO threshold):

```promql
sum(rate(e2a_outbound_terminal_latency_seconds_bucket{le="300"}[1h]))
/ sum(rate(e2a_outbound_terminal_latency_seconds_count[1h]))
```

The histogram's exactly-once contract (one sample per message, co-located
with `e2a_outbound_terminal_total`) is what makes this ratio a per-message
fraction; an outage-tail message can legitimately land in the day-scale
buckets, so alert on the ratio, not the tail.

**Webhook eventual delivery** — initial deliveries that reached a terminal
state without an e2a-attributable failure. `endpoint_failure` is deliberately
outside the denominator: an unhealthy customer endpoint is not an e2a
failure. `unknown` is included conservatively so an internal failure cannot
hide merely because the worker could not load the row:

```promql
sum(rate(e2a_webhook_delivery_terminal_total{
  outcome="delivered",scope=~"initial|unknown"}[1h]))
/ sum(rate(e2a_webhook_delivery_terminal_total{
  outcome=~"delivered|e2a_failure",scope=~"initial|unknown"}[1h]))
```

The attempt counter remains useful diagnostic data, but it is not the
eventual-delivery SLI: it mixes attempts with deliveries and would
overweight endpoints that consume the full retry envelope. A delivery
retrying a pre-POST infrastructure error cannot become terminal until it
exhausts (up to 29h21m), so queue age and job-error alerts cover that window;
the terminal counter records the e2a failure when the row settles.

**Webhook event→first attempt** — fraction within the 60 second p95
objective (covers fan-out, queue wait, and worker pickup):

```promql
sum(rate(e2a_webhook_first_attempt_latency_seconds_bucket{le="60"}[5m]))
/ sum(rate(e2a_webhook_first_attempt_latency_seconds_count[5m]))
```

**WebSocket handshake success (valid credentials)** — accepted handshakes
over accepted plus e2a-attributable rejections:

```promql
sum(rate(e2a_ws_connects_total[5m]))
/ (sum(rate(e2a_ws_connects_total[5m]))
   + sum(rate(e2a_ws_handshake_rejected_total{reason="internal_error"}[5m])))
```

The split is deliberate, by who can act on the rejection. Client faults are
excluded from the denominator, or scanner noise and customer
misconfiguration would burn the SLO: `unauthorized` (missing/invalid
credential), `not_found` (unknown agent — including the cross-tenant 404
collapse), `forbidden` (an agent-scoped credential pinned to a different
agent — deterministic scope enforcement, not e2a-actionable), and
`upgrade_failed` (predominantly client protocol defects, e.g. a plain GET
with a valid key; genuine server-side upgrade breakage — a proxy stripping
Upgrade headers fleet-wide — is covered by the prober's
`websocket_round_trip` SLI below). `internal_error` (a store/infra error
while authenticating or resolving the agent, distinguished from a genuine
miss by `pgx.ErrNoRows`) is the only e2a-attributable reason and the only
one in the denominator — a Postgres outage burns this SLI rather than
reading 0/0 behind the client-fault labels.

**WebSocket health** — active connections, abnormal disconnect rate, and
the black-box push-path success ratio:

```promql
sum(rate(e2a_ws_disconnects_total{reason=~"ping_timeout|error"}[15m]))
sum(rate(e2a_selftest_scenario_runs_total{
  scenario="websocket_round_trip",outcome="pass"}[6h]))
/ sum(rate(e2a_selftest_scenario_runs_total{
  scenario="websocket_round_trip"}[6h]))
```

**MCP availability** — measured *independently* by the prober (strategy
target: "measured independently"), not self-reported by the MCP process:

```promql
sum(rate(e2a_selftest_scenario_runs_total{
  scenario="mcp_http_round_trip",outcome="pass"}[6h]))
/ sum(rate(e2a_selftest_scenario_runs_total{
  scenario="mcp_http_round_trip"}[6h]))
```

## Initial SLO targets

Starting targets for the hosted service; self-hosters can adopt them as-is.
These are objectives over a 30-day rolling window, not guarantees; alert on
burn rate, not on single samples (the prober design's discipline: page only
after M ≥ 2 consecutive failed probes).

| Surface | SLI | Initial target |
|---|---|---|
| HTTP API | non-5xx fraction | ≥ 99.9% |
| HTTP API | p99 latency (`/v1`, excluding WS upgrades) | < 750 ms |
| SMTP intake | acceptance (non-policy) | ≥ 99.9% |
| SMTP intake | DATA processing p95 | < 2 s |
| Outbound | terminal outcome within 5 min of acceptance | ≥ 99% |
| Outbound | queue wait p95 | < 30 s |
| Webhooks | event → first delivery attempt | < 60 s (p95) |
| Webhooks | e2a-attributable eventual delivery execution (initial deliveries; customer endpoint failures excluded; ≤ 8 attempts) | ≥ 99% |
| WebSocket | handshake success (valid credentials) | ≥ 99.9% |
| WebSocket | prober round-trip (connect → live push) | ≥ 99.9% of probes |
| MCP | prober connection + tool-call success | ≥ 99.9% of probes |

## Product SLOs vs. inbox placement

Every target above measures **e2a's own behavior**: accepting mail,
processing queues, submitting to the upstream provider, delivering webhooks,
and serving connections. None of them is a deliverability claim.

**SMTP acceptance ≠ inbox placement.** A `sent` terminal outcome means the
upstream provider accepted the message — it says nothing about whether the
recipient's mailbox provider placed it in the inbox, a spam folder, or
dropped it. e2a reports these as separate signals and does not infer one
from another:

1. **Submission** — `e2a_outbound_terminal_total{outcome="sent"}` (our SLO);
2. **Provider feedback** — delivery/bounce/complaint events from SES via
   `/webhooks/ses`, exposed per-message in the lifecycle API (a provider
   signal, not an e2a SLO);
3. **Observed placement** — not measured today. e2a states no
   inbox-placement SLO and will not until there is a defensible,
   independently measurable method.

Anything marketed against these metrics must preserve that distinction.
