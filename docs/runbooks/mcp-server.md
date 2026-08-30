# MCP server runbook

## Overview

The MCP HTTP server (`mcp/`, image `ghcr.io/tokencanopy/e2a-mcp-http`) is the
Streamable HTTP transport that lets MCP-aware hosts (Claude Code, Cursor,
ADK, LangChain, …) drive an e2a inbox over `POST /mcp`. Hosted, it runs
behind Caddy at `https://api.e2a.dev/mcp`; self-hosters run it via
`docker-compose.yaml` or `mcp/Dockerfile`.

The transport is **stateless by design**: no sessions, no `MCP-Session-Id`,
no SSE stream. Every POST builds a fresh server + transport, forwards the
client's `Authorization: Bearer` to the e2a backend unchanged, and is torn
down when the response closes. `GET` and `DELETE /mcp` answer 405. The only
cross-request state is an in-memory bearer→principal cache (a `whoami`
lookup memo) plus the readiness-probe cache — an idle client is never
disconnected, and a cache miss costs one extra `whoami`, never a dropped
connection. "Reconnect" after an interruption is the client simply
re-POSTing; MCP clients do this automatically.

## Availability and connection-success measurements

All signals come from `GET /metrics` (Prometheus text exposition,
unauthenticated).

- **Connection success rate** (the headline SLO):
  `1 - 5xx / total` on the MCP route, from
  `mcp_http_requests_total{route="mcp",status_class="5xx"}` over
  `sum(mcp_http_requests_total{route="mcp"})`. 4xx responses (401, 405,
  421) are client faults and do not count against availability.
  Hosted target: **99.9% connection availability**.
- **Auth-resolution health**: rate of
  `mcp_auth_resolutions_total{result="fallback"}` (backend whoami
  unreachable/5xx — server failed closed) and `{result="invalid"}`
  (bad/revoked credentials). A `fallback` rate above zero tracks backend
  pain; a rising `invalid` rate tracks credential hygiene, not the server.
  Healthy baseline is dominated by `cache_hit` with occasional `resolved`.
- **Readiness flaps**: watch
  `mcp_readyz_checks_total{result="degraded"}`. Because the probe result is
  cached 10s, increments of this counter are distinct probe *windows* failing,
  not individual scrapes.
- **Latency**: `mcp_http_request_duration_seconds` histogram per route.
  A cold `whoami` on `route="mcp"` can legitimately reach the multi-second
  buckets; p99 dominated by `cache_hit` should stay sub-second.

## Health endpoints

| Endpoint | Meaning | Backend contact | Response |
|---|---|---|---|
| `GET /healthz` | Process liveness only | none, ever | 200 `{ "ok": true }` |
| `GET /readyz` | Can reach the e2a API (and therefore resolve credentials — credential resolution whoami-probes the same API) | probes `{E2A_API_URL}/api/health`, 2s timeout | 200 `{ "ok": true, "checks": { "api": "ok" } }`, or 503 `{ "ok": false, "checks": { "api": "unreachable" }, "request_id": "…" }` + `Retry-After: 10` (the cache TTL) |
| `GET /metrics` | Prometheus scrape | none | text exposition (`text/plain; version=0.0.4`) |

All three are unauthenticated. The exposition carries only counters/gauges
over closed label vocabularies — no bearer, user, or request data.

Orchestrator wiring: **liveness probe → `/healthz`**, **readiness probe →
`/readyz`**. Never wire liveness to `/readyz`: a backend outage would
restart healthy MCP processes and turn a backend incident into an MCP
outage.

The readyz probe caches its result — success *and* failure — for 10
seconds. A scraping fleet (every pod in a k8s deployment probing every
replica) therefore cannot fan out into a probe storm against an
already-struggling backend: at most one real probe per 10s per MCP process
reaches `{E2A_API_URL}/api/health`. A corollary for alerting: don't scrape
readyz faster than the cache TTL expecting fresh data, and don't hammer it
to "check if the backend is back" — the answer is up to 10s stale by
design.

## Failure classes

| Class | Primary signal | Status surfaced to client |
|---|---|---|
| Backend API unreachable | `readyz` 503, `mcp_readyz_checks_total{result="degraded"}` | tool errors `retryable: true`; 503 on readyz only |
| Expired / revoked credential | `mcp_auth_resolutions_total{result="invalid"}` | 401 `invalid_token` challenge |
| Backend outage mid-request | `mcp_auth_resolutions_total{result="fallback"}` | request succeeds at agent scope |
| DCR rate limiting | backend 429 `rate_limited` on `/oauth2/register` | 429 + `Retry-After` |
| OAuth token exchange failure | backend `[oauth] /token … error:` logs | RFC 6749 error JSON |
| DNS-rebinding guard | `mcp_http_requests_total{route="mcp",status_class="4xx"}` | 421 (empty body) |
| Oversized request body | `terminal_error` log event | 500 JSON-RPC `-32603` |
| Resolve-cache pressure | `mcp_resolve_cache_entries` pinned at cap | none (latency only) |

### Backend API unreachable

- **Symptoms**: `GET /readyz` returns 503 with `checks.api = "unreachable"`
  and `Retry-After: 10`; load balancer drains the MCP replica. Tool calls
  that reach the server fail with `structuredContent.retryable: true`.
- **Detection**: `mcp_readyz_checks_total{result="degraded"}` rising;
  `auth_resolution` log events with `result: "fallback"` at WARNING.
- **Likely cause**: e2a API down or unreachable — wrong `E2A_API_URL`,
  network partition between the MCP container and the API, or the API
  itself failing its own `/api/health`.
- **Remediation**: confirm `E2A_API_URL` points at the API server (inside
  compose: the internal hostname, e.g. `http://e2a:8080`); curl
  `$E2A_API_URL/api/health` from inside the MCP container/network; then
  follow the backend's own health trail. The MCP server recovers on its
  own — the 10s probe cache means readyz flips back within one window of
  the backend returning.

### Expired or revoked credentials

- **Symptoms**: 401 from `POST /mcp` with
  `WWW-Authenticate: Bearer realm="e2a", resource_metadata="…", error="invalid_token"`.
- **Detection**: `mcp_auth_resolutions_total{result="invalid"}` rising;
  `auth_resolution` WARNING log `bearer rejected by the backend`.
- **Likely cause**: API key revoked/deleted, OAuth access token expired
  with a broken refresh, or a client pointing at the wrong deployment.
- **Remediation**: the *client* must re-authorize the OAuth flow (or the
  user must mint a replacement API key). **Do not retry in a loop** — the
  backend re-checks the credential on every uncached resolution and the
  answer won't change. A sustained invalid rate from one client usually
  means a stuck retry loop; rate-limit or fix the client.

### Backend outage mid-request (fail-closed fallback)

- **Symptoms**: requests succeed, but the credential is served at
  least-privilege **agent scope**: account-scoped callers see the tool list
  shrink from 78 tools to the 21 runtime tools, and the per-request default
  agent is unset (explicit `email` required).
- **Detection**: `auth_resolution` WARNING `whoami probe failed; serving
  least-privilege fallback`; `mcp_auth_resolutions_total{result="fallback"}`.
- **What happened**: the bounded whoami probe (`MCP_RESOLVE_TIMEOUT_MS`,
  default 5000 ms, max 1 retry) timed out or got a backend 5xx. The server
  fails closed rather than hanging the POST (previously this could stretch
  to the SDK's 30s × 3-attempt worst case) or refusing the request. The
  fallback resolution is deliberately **not cached** — the next request
  re-probes, so the full surface returns automatically once the backend
  recovers. The backend still authenticates the bearer on every API call,
  so no privilege is widened.
- **Remediation**: same as "Backend API unreachable". No MCP-side action
  needed for recovery; persistent fallback means the whoami path
  (`GET /account`) is down even if `/api/health` passes.

### DCR rate limiting

- **Symptoms**: 429 with `rate_limited` and a `Retry-After` header from
  `POST /oauth2/register` on the backend.
- **Likely cause**: dynamic client registration is capped at **10
  registrations per IP per hour** (per server process). NAT'd fleets of MCP
  clients share one IP and burn the budget together.
- **Remediation**: clients must persist their `client_id` and re-use it
  instead of re-registering on every connect; honor `Retry-After` before
  retrying. On the hosted deployment, operators seeing legitimate shared-IP
  pressure should file it as a product issue rather than widening the cap
  ad hoc.

### OAuth token exchange failures

- **Symptoms**: client gets an RFC 6749 error JSON (e.g. `invalid_grant`)
  from `POST /oauth2/token`.
- **Detection**: backend log lines
  `[oauth] /token <stage> error: request_id=<id> client="<id>" grant="<type>" code=<code> hint="<hint>" debug="<debug>"`.
  `code` alone (`invalid_grant`) cannot distinguish causes; `hint` (and
  `debug`, when present) separates **expired/used authorization code** from
  **PKCE verifier mismatch** from **token replay**. These are static
  category strings — no secrets are logged.
- **Remediation**: join the client-visible `request_id` (present in the
  DCR/register error body and always equal to the `X-Request-Id` response
  header) to the log line's `request_id=`. Expired-code → restart the
  browser flow; PKCE mismatch → fix the client's verifier persistence;
  replay → investigate credential handling on that client.

### 421 Misdirected Request

- **Symptoms**: `POST /mcp` (and the discovery document) answered 421 with
  an empty body, before any auth or body parsing.
- **Detection**: `mcp_http_requests_total{route="mcp",status_class="4xx"}`
  with a `Host` header outside the allowlist; correlating `http_request`
  log line shows `status: 421`.
- **Likely cause**: DNS-rebinding guard — the request's `Host` is not in
  `MCP_ALLOWED_HOSTS` (no literal default: derived from `MCP_PUBLIC_URL`'s
  host if that's set, otherwise the process refuses to start). Common
  self-host mistakes: accessing via a hostname/IP not listed, or a fronting
  proxy rewriting `Host`.
- **Remediation**: add the externally used hostname(s) to
  `MCP_ALLOWED_HOSTS` (comma-separated; ports are stripped before
  comparison). Never work around it by disabling the guard. Clients must
  not retry 421 — fix the URL.

### Oversized request body

- **Symptoms**: a `POST /mcp` with a JSON body over **40 MB** is rejected
  at the parser. The limit clears the attachment contract (10 MB per file,
  25 MB combined, base64-encoded ≈ 34 MB plus envelope) with headroom.
- **Detection**: `terminal_error` log event with `error: "PayloadTooLargeError"`
  and the request's `request_id`.
- **Client-visible status**: the terminal error handler answers 500 with a
  JSON-RPC `-32603` body (it does not currently special-case the parser's
  413). The client should treat this as non-retryable and shrink the
  payload (fewer/smaller attachments per call).
- **Remediation**: if legitimate traffic exceeds 40 MB, the fronting proxy
  limit and the attachment contract must move together — do not raise just
  the parser limit.

### Resolve-cache pressure

- **Symptoms**: `mcp_resolve_cache_entries` gauge pinned at the cap;
  `cache_hit` share drops and `resolved` (a real whoami round-trip per
  request) rises, adding backend load and p99 latency.
- **Likely cause**: more distinct live bearers than the cap (default 500),
  or a TTL (default 300000 ms) too short for the client population.
- **Remediation**: raise `MCP_MAX_SESSIONS` and/or `MCP_SESSION_IDLE_MS`
  (legacy env names — they size the bearer→principal resolve cache, not
  sessions; the transport is stateless). Eviction is oldest-first and a
  miss costs one `whoami`, so this is a latency/load tuning issue, never a
  correctness one.

## Client retry guidance (the bounded-retry contract)

The e2a SDK retries on the client's behalf (2 retries = 3 attempts,
30s per attempt, jittered exponential backoff, honors `Retry-After`). When
writing your own MCP caller:

- Branch on `structuredContent`, not the error text. Honor
  `retryable` and `retry_after_seconds`.
- Retry only `retryable: true` errors, with exponential backoff + jitter,
  capped at ~3 attempts.
- **Never retry 4xx** — especially 401 and 421; they are deterministic.
  After a 401 `invalid_token`, re-authenticate (OAuth re-authorize or new
  API key), then retry the operation.
- `pending_review` on send tools is a **success** status, not an error —
  do not retry it.
- Don't poll `/readyz` faster than ~10s per replica; the result is cached
  and hammering it gains nothing.

## Correlation: tracing one failing request

Every MCP response carries an `X-Request-Id` header. An inbound
`X-Request-Id` matching `^[A-Za-z0-9_-]{1,64}$` is honored verbatim;
anything else is replaced with a server-minted `mcpreq_<12hex>`. The id is
echoed on **all** responses including 401/405/421/500, and JSON-RPC error
bodies for 401/405/500 carry it additively as `error.data.request_id`
(error codes and messages are unchanged).

MCP log lines are single-line JSON on stderr (GCE-shaped: `severity`,
`event`, `message`, plus structured fields):

- `auth_resolution` — `{request_id, result, duration_ms, scope?}`;
  `invalid` and `fallback` are logged at WARNING.
- `http_request` — `{request_id, route, method, status, duration_ms}`;
  one per request, emitted when the response finishes. Successful (<400)
  probe/scrape traffic (`healthz`, `readyz`, `metrics` routes) is metered
  but not logged — it would be ~14k noise lines/day/replica; failures on
  those routes still log.
- `tool_execution` — `{request_id, tool, outcome, duration_ms, error_code?}`.
- `terminal_error` — `{request_id, error}` for unhandled throws.

Bearer tokens are never logged; the resolve cache is keyed by SHA-256
fingerprints of the bearer.

To trace across MCP → backend: the MCP request id is **not** forwarded to
the backend API (the SDK has no per-request header seam). Join by timestamp
and by the backend's own `req_*` id from its `X-Request-Id` header. For
OAuth failures specifically, the backend's DCR error bodies include a
`request_id` field equal to their `X-Request-Id` header, and all `[oauth]`
log lines on register/token/consent carry `request_id=`, so that side of
the trail is directly joinable.
