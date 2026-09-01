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
  build: "1.3.0"                 # or E2A_METRICS_BUILD; labels every sample
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

Every server and prober sample also carries one bounded `build` label. Hosted
deploys set it to the real release/image tag even though blue/green containers
run from temporary local aliases; self-hosted processes that do not set
`metrics.build` / `E2A_METRICS_BUILD` expose `build="unknown"`. Aggregations
that should span a cutover should continue to use `sum(...)`; group by
`build` when correlating an SLO change with a release.

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

### OIDC login and control-plane provisioning

These counters are the independently matchable signals for the legacy-mux
OIDC and provisioning routes. Do not infer either surface from
`e2a_http_requests_total{route="/legacy"}`: that route label intentionally
collapses every non-`/v1` handler.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_oidc_discovery_total` | counter | `outcome`, `status_class` | Generic browser-login provider discovery attempts. `outcome` is `success`, `issuer_unavailable` (no response, 429, or 5xx), or `discovery_invalid` (malformed/mismatched discovery or another non-retryable response). `status_class` is the provider response class, `none` when no response was received, or `other` if an unexpected value reaches the backend. |
| `e2a_oidc_callback_total` | counter | `outcome`, `trust`, `status_class` | Browser callback outcomes. `outcome` is `success`, `discovery_unavailable`, `state_invalid`, `provider_rejected`, `response_invalid`, `token_exchange_failed`, `id_token_invalid`, `claim_invalid`, `unknown_user`, `session_failed`, or `post_login_failed`. `trust=public` covers requests rejected before browser transaction state is validated; `trust=trusted` means the server-authenticated state and transaction cookies matched first. `status_class` is e2a's response class. Alert on sustained actionable `trusted` failures (especially token exchange, invalid claim, unknown user, or session failure), not scanner-shaped `public` traffic or user-denied `provider_rejected` callbacks. |
| `e2a_provisioning_total` | counter | `outcome`, `trust`, `status_class` | Internal `POST /api/internal/users/provision` outcomes. `outcome` is `created`, `existing`, `rejected`, `internal_error`, `not_configured`, `malformed_request`, or `unauthorized`. `trust=public` means the HMAC was absent/not yet verified; `trust=authenticated` means the request HMAC passed before the outcome. Alert on sustained `authenticated` `rejected`/`internal_error` outcomes; public malformed/auth failures are diagnostic and must not page. |

All three label families are enum-allowlisted and collapse unknown values to
`other`. Discovery and callback logs use only the same bounded category/trust/
status fields. Provider response bodies, OAuth tokens or codes, claims,
cookies, issuer-supplied text, and email addresses are never emitted.

### SMTP intake (relay edge)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_smtp_inbound_total` | counter | `outcome` | SMTP intake decisions. Units differ by stage: `accepted` (250), `accepted_dedup` (250 on a lost-ack retry), and `tempfail` (451 — durable persist/enqueue failed) are one per DATA transaction; `rejected_unknown_recipient` / `rejected_unverified_domain` (550) and `rejected_quota` (552) are one per rejected RCPT command. **Semantics change (usage-based pricing v1, e2a ≥ 1.8):** `rejected_quota` now means **storage-cap only** — message-flow caps are outbound-only and never reject inbound mail, so pre-1.8 series (which included flow-cap rejections) are not comparable — a single transaction can emit several rejections and still accept for its remaining recipients; `rejected_line_too_long` (554 — a line over the relay's `MaxLineLength`) is one per DATA transaction aborted mid-read. Other mid-read DATA aborts (client dropped, size limit) record no outcome. |
| `e2a_smtp_inbound_duration_seconds` | histogram | — | DATA-phase processing time (accepted/tempfail outcomes only; RCPT rejections have no DATA phase). Includes an exact `2` second SLO bucket. |

Policy rejections (550/552) are *correct* behavior, not failures — the
acceptance SLI below deliberately excludes them.

### Outbound send pipeline

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_outbound_queue_wait_seconds` | histogram | — | Due→pickup wait per attempt (River `attempted_at − scheduled_at`). Measures worker keep-up, deliberately not cumulative message age: a retry's backoff, a ramp deferral, or a rate-limit deferral does not count as queue wait. |
| `e2a_outbound_attempts_total` | counter | `outcome` | Upstream submission attempts: `success`, `temporary_failure`, `permanent_failure`. |
| `e2a_outbound_attempt_duration_seconds` | histogram | — | Upstream (SES/SMTP relay) submission duration. |
| `e2a_outbound_rate_deferred_total` | counter | — | Submissions deferred by the per-agent fire-time rate limiter (`internal/sendrate`, 60/min/agent sliding window, enforced in the send worker immediately before provider submission). A deferral snoozes the job without burning an attempt, metering, or emitting a terminal event — the message fires when the window frees capacity (re-fire spread by a deterministic per-message jitter). A sustained high rate means agents are queueing behind their own budget. Deferral loops are bounded by the 72h retry horizon, so a backlog beyond ~259,200 messages/agent/burst (60 × 60 × 72) fails its tail terminally with `send_rate_timeout` (`failed_local_retries`). |
| `e2a_outbound_terminal_total` | counter | `outcome` | Messages reaching a terminal submission outcome, **exactly once per message**: `sent`, `failed_suppressed`, `failed_provider`, `failed_local_retries`, `failed_cancelled` (policy cancel settled by the reconciler — kept out of `failed_local_retries` so cancellations can't mask a retries-exhausted regression). A deferred final attempt is counted when the terminal reconciler settles it — as `sent` when provider-accept evidence arrived (never a false failure), else as a failure. |
| `e2a_outbound_terminal_latency_seconds` | histogram | — | Eligibility→terminal latency per message (the terminal write's occurred_at − the **submission anchor**), observed **at most once per message**, co-located with the `e2a_outbound_terminal_total` emission so the two share their exactly-once contract (the SNS-feedback settle path is deliberately uninstrumented for both; a terminal landing before its own anchor records the count with no sample — see the SLO section). The anchor is `messages.created_at` for an ordinary send, and the latest of `created_at`, `scheduled_at`, and `reviewed_at` otherwise: a HITL hold anchors at the moment it was approved into the send pipeline, a scheduled send at its fire time. Neither wait is e2a latency, and charging them here made every hold approved after >5 min — and every send scheduled further out than the window — an automatic SLO miss. Provider-evidence settles use the evidence's accept time as occurred_at, so they measure anchor→provider-accept, not anchor→sweep. Buckets span seconds→days (the 72h retry horizon is the tail). |

### Webhook delivery

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_webhook_attempts_total` | counter | `outcome`, `status_class` | Delivery attempts to subscriber endpoints: `delivered`, `retryable_failure`, `exhausted` (terminal after a budget ran out without a successful POST — max attempts, a delivery that exhausted its retries on a pre-POST infrastructure error such as a sustained webhook-lookup outage, or the disabled-webhook snooze budget reaching its cap), `webhook_deleted`, `skipped_disabled`. `status_class` is the endpoint's response class, or `none` when no HTTP response was received — connect/DNS/SSRF-blocked, or no POST was ever made. |
| `e2a_webhook_attempt_duration_seconds` | histogram | — | HTTP POST duration per attempt. |
| `e2a_webhook_delivery_terminal_total` | counter | `outcome`, `scope` | Delivery rows reaching a terminal state, emitted only after the terminal DB transition succeeds. `outcome` is `delivered`, `e2a_failure` (internal lookup/retry exhaustion or TTL expiry), `endpoint_failure` (the customer endpoint never accepted within eight attempts), `excluded` (for example, a deleted webhook), or `webhook_disabled` (the delivery exhausted the disabled-webhook snooze budget and was written terminally failed with `last_error` "webhook disabled"). `scope` is `initial`, `replay`, `test`, or `unknown`; `unknown` is the conservative classification when a pre-load failure or batched TTL transition cannot inspect the row. The hosted eventual-delivery SLO uses initial + unknown deliveries and excludes endpoint failures, so customer behavior cannot burn e2a's budget. |
| `e2a_webhook_first_attempt_latency_seconds` | histogram | — | Event→first-attempt latency per subscriber delivery (attempt start − the `webhook_events` row's `created_at`), observed **only on a first-delivery row's first HTTP attempt** (no recorded prior attempt — regardless of River attempt number, so a first POST delayed by pre-POST failures still observes). Retries, the no-POST outcomes (`webhook_deleted`, `skipped_disabled`), replay rows (their baseline would be the original event's age), eventless `/test` deliveries, and jobs that sat through a customer-disabled window (River snooze) never observe. |
| `e2a_webhook_notify_total` | counter | `kind`, `outcome` | Webhook health-notification jobs (the WARNING / DISABLED emails that tell an owner their endpoint is failing), one sample per job completion. `kind` is `warning` or `disabled`. `outcome` is `sent`, `permanent` (cancelled with no retry — a rejected owner address, no owner email on record, or an upstream policy refusal), `outage` (relay unreachable, snoozed), `retryable` (transient — River reschedules), or `skipped` (a staleness guard decided **not** to send: webhook deleted, re-enabled, recovered, or an unrecognized kind). `skipped` is separate from the failure outcomes on purpose — without it, a fall in sends cannot be told apart from a send path that has died. **A sustained `permanent` rate is the alert**: it means the mechanism that reports broken webhooks is itself broken, which is the exact failure the feature exists to eliminate, one level up. |
| `e2a_webhook_deliveries_expired_pending_total` | counter | — | Delivery rows that hit their retention TTL still pending and were marked failed by the janitor. |
| `e2a_webhook_fanout_rescued_total` | counter | — | Pending webhook events re-driven after their fan-out job died (discarded/pruned). A climbing rate means a poison event. |
| `e2a_webhook_deliveries_rescued_total` | counter | — | Pending delivery rows re-driven after their delivery job died (discarded/pruned). A climbing rate means a poison row. |
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
| `e2a_delegated_auth_failures_total` | counter | `category` | Delegated access-token (RFC 9068 `at+jwt`) authentication failures, by category only: `invalid_token` (any 401-class rejection — bad signature/type/algorithm/issuer/audience/`azp`/scope/time/claim, or the verifier disabled), `unknown_subject` (verified token whose `(issuer, subject)` maps to no local user — a 401, no existence oracle), `verifier_unavailable` (503-class: verifier not ready, or a JWKS outage with no usable cached key), `identity_store_failure` (503-class: the mapping store could not be read; caller cancellation is excluded). Emitted only when that category occurs. The label never carries a subject, issuer response text, or any token/message data. |
| `e2a_delegated_jwks_refresh_total` | counter | `outcome` | Delegated-verifier JWKS refresh outcomes: `success`, `key_absent` (a successful refresh that did not contain the requested `kid` — the token is then a 401), `transport_error` / `parse_error` (fetch or decode failed; the last good keyset is retained), `rate_limited` (a refresh suppressed by the per-issuer cooldown or token bucket). A sustained non-`success` rate means the issuer's JWKS endpoint is unhealthy or rotating faster than the refresh policy allows. |

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

### Thread identity

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_thread_resolution_total` | counter | `source` | Committed mailbox-local topology decisions and diagnostics. Final decision sources are `api_reply`, `fresh_send`, `forward`, `rfc_in_reply_to`, `rfc_references`, `self_twin`, `authenticated_delivery_twin`, and `no_anchor`. Diagnostic sources are `lazy_legacy_anchor`, `anchor_found_without_thread`, `legacy_anchor_unmatched`, `ambiguous_anchor`, and `cycle_detected`; a diagnostic can accompany a final source for the same committed write. |
| `e2a_thread_header_parse_failures_total` | counter | `header` | Inbound `In-Reply-To` or `References` headers rejected by the strict RFC Message-ID parser. The header label is bounded to `in_reply_to` or `references`; rejected fields fail closed and are treated as having no candidates. This is a processing-attempt diagnostic and can repeat for fan-out recipients or retried async work. |
| `e2a_thread_null_messages` | gauge | `age_bucket` | Threadless messages in the current bounded sample, split into `lt_1h`, `1h_6h`, and `6h_24h`. Rows older than 24 hours are intentionally excluded because historical threadless rows are supported migration state. |
| `e2a_thread_invariant_violations` | gauge | `kind` | Invalid parent edges in the current bounded sample: `dangling_parent`, `cross_agent_parent`, `thread_mismatch`, `cycle`, or `cycle_depth_limit`. |
| `e2a_thread_relationship_percent` | gauge | `kind` | Sampled mailbox-local topology ratios: `threads_multi_conversation` and `conversations_multi_thread`. These are expected measurements, not error signals. |

Resolution samples are emitted only after the write transaction commits.
Rolled-back attempts, serialization retries, transient SMTP failures, and
idempotency replays that create no new topology decision do not increment the
counter. This makes the counter suitable for measuring durable assignment
outcomes rather than database-attempt volume.

The hourly janitor walks at most 1,000 messages in primary-key order and
rotates a cursor through the table; parent traversal is capped at 64 edges.
This is a bounded sample, not a full-table aggregate. It clears a sampled
child's invalid `thread_parent_id` (including a cycle edge) with a guarded
update, but never changes `thread_id` or `rfc_message_id_key`. A chain that
merely reaches the traversal cap is measured as `cycle_depth_limit` and left
unchanged.

Lazy adoption volume over the last hour is:

```promql
sum(increase(e2a_thread_resolution_total{source="lazy_legacy_anchor"}[1h]))
```

Ambiguous anchors also emit a structured, process-wide rate-limited log (at
most one line per minute). It contains only candidate/thread counts: no
addresses, subjects, message content, or RFC Message-IDs.

### Maintenance

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `e2a_janitor_rows_deleted_total` | counter | `table` | TTL sweep deletions. |
| `e2a_notify_missed_total` | counter | — | Fallback-poll wakeups LISTEN/NOTIFY missed (reconnect churn indicator). |
| `e2a_redeliver_requests_total` | counter | `scope` | Customer-driven webhook replays. |
| `e2a_contact_due_events_total` | counter | `outcome` | `contact.due` sweep outcomes. `published` counts wake-ups committed to the durable outbox. `failed` counts a failed atomic claim/publish attempt (or the affected batch size when known); the transaction rolls back and River retries the job. A sustained zero `published` despite known overdue, armed engagements can indicate a stalled sweep. Alert on sustained `failed` growth or exhausted River jobs rather than treating one increment as a permanent miss. |

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
`liveness`, `auth_read`, `delegated_auth` (trusted issuer token vending →
JWKS verification → external-principal mapping → account read),
`inbound_round_trip` (SMTP→webhook→HMAC),
`outbound_send` (real submit to the SES mailbox simulator),
`self_send_loopback`, `websocket_round_trip` (WS handshake + live push),
`agent_lifecycle` (self-cleaning ephemeral agent), `mcp_http_round_trip`
(tools/list + whoami over the deployed MCP endpoint). Set
`E2A_PROBE_REQUIRE_MCP=true` on stacks where MCP must be probed — it turns
the skip-as-pass on an unset `E2A_PROBE_MCP_URL` into a failure.
The delegated-auth scenario similarly skips when its token URL/secret are
absent unless `E2A_PROBE_REQUIRE_DELEGATED_AUTH=true`. Its vending endpoint
must own one fixed synthetic principal and accept no caller-selected identity,
audience, scope, role, or lifetime. The prober fetches a fresh token for one
read-only `GET /v1/agents?limit=1`, never emits the token or response body, and
discards it after the request.

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

**Outbound eligibility→terminal** (historically "acceptance→terminal", which
is still the hosted alert's name — the query below is unchanged by the
rename) — fraction of messages reaching a terminal outcome within 5 minutes of
becoming *eligible to submit* (the `le="300"` bucket edge is the SLO
threshold). The baseline is the submission anchor in the catalog above, not raw
`created_at`: a HITL hold's review dwell and a scheduled send's deliberate delay
are outside the measurement, so a reviewer who takes an hour cannot burn e2a's
budget:

```promql
sum(rate(e2a_outbound_terminal_latency_seconds_bucket{le="300"}[1h]))
/ sum(rate(e2a_outbound_terminal_latency_seconds_count[1h]))
```

The histogram's per-message contract (at most one sample per message,
co-located with `e2a_outbound_terminal_total`) is what makes this ratio a
per-message fraction; an outage-tail message can legitimately land in the
day-scale buckets, so alert on the ratio, not the tail. A terminal that lands
before its own anchor — a scheduled send cancelled ahead of its fire time, or
an approve submitted inside the app/DB clock skew — records the count with no
latency sample, so `_count` can trail `e2a_outbound_terminal_total` slightly
wherever holds or schedules are used. Both sides of the ratio lose the same
sample, so the drop cannot flatter the SLI.

When the anchor first ships, the reported late fraction steps down on any
deployment using HITL or scheduled sends — those samples were never e2a
latency. Annotate the dashboard at that release so the step is not read as a
regression, and do not re-baseline the alert against pre-anchor history.

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

**Delegated console authentication** — this is the page-safe signal for the
trusted issuer/verifier/mapping path. Public invalid-token counters are useful
diagnostics but are attacker-influenceable and therefore are not availability
evidence by themselves:

```promql
sum(rate(e2a_selftest_scenario_runs_total{
  scenario="delegated_auth",outcome="pass"}[6h]))
/ sum(rate(e2a_selftest_scenario_runs_total{
  scenario="delegated_auth"}[6h]))
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
