# Production-safe self-test, synthetic prober & deploy bake-gate

| | |
|---|---|
| **Status** | Partially implemented (2026-06-20) — main-repo slices landed; ops slices + deferrals below |
| **Spans** | `e2a` (Go backend) + `e2a-ops` (deploy/infra) |
| **Risk** | MEDIUM — touches metering/billing gate, outbound send wiring, and the prod deploy gate; must not pollute usage/analytics, send real mail, or email agent owners |
| **GA scope** | account-class gate + `scenarios`/`selftest` package + `cmd/e2a-prober` + `/readyz` + `/selftest` + fail-safe bake-gate. The `e2a` `/metrics` 5xx signal is a fast-follow. |

## Implementation status (as built)

Landed on `design/prober-selftest` (main repo), each slice committed + verified:

- **Slice 1 — account-class metering gate.** `migrations/037_account_class.sql` (`users.account_class`), `usage.PolicyFor` + gate in `RecordAndCheck`. Verified: zero `usage_events` for a `system` account, in-process and over the wire.
- **Slice 3a — `internal/selftest`.** The `[]Scenario` battery (liveness, auth read, inbound SMTP→webhook + HMAC, self-send loopback) + `HTTPSink`. Verified against a real in-process server.
- **Slice 3b — `cmd/e2a-prober`.** `seed`/`validate`/`run-once`/`serve` (+ `/sink` `/healthz` `/metrics` `/status`). Verified over the wire against the real `e2a` binary.
- **Slice 3c — `/readyz`** (DB + migrations-applied) and **`/selftest`** (dependency diagnostics, health+json, auth-gated) on the non-Huma mux.

**Deviations from the original plan (decided during implementation):**
- **`Sender` interface (D2) deferred.** It rewrites the live SES send path and is not required by the prober (inbound uses the real listener; outbound uses loopback). Tracked as an optional, separately-reviewed follow-up.
- **In-server `/selftest` is dependency-diagnostics, not the full round-trip.** Implementation revealed the full in-server round-trip would require injecting probe credentials into the server's env (unrecoverable from the DB) and an HTTPS exemption in the webhook deliverer for the internal sink. The full round-trip already lives in the external `e2a-prober` (verified), so `/selftest` does deep dependency checks instead. See §D3.
- **Ops slices 4 & 5 (compose service, `deploy.yml` bake-gate) not yet landed** — config-only, not deploy-verifiable in this environment.

**Known follow-up (prod wiring):** the webhook subscriber deliverer requires HTTPS in production, but the prober's internal sink is plain HTTP — the ops wiring needs a deliverer exemption (or TLS) for the internal sink before the round-trip works against prod.

## Problem

`deploy.yml` gates a release on `curl -sf http://localhost:8080/api/health` for 30×1s.
That endpoint (`internal/agent/api.go:1200`, wired at `cmd/e2a/main.go:556`) is
**liveness only** — no DB, no mail path. A release can pass it while inbound
mail→webhook delivery, auth, or the DB are broken (this is how the 2026-05-19
`list_messages` incident — an unapplied migration — reached prod). We need:

1. A continuous signal that the **real product paths** work in prod.
2. A deploy gate that consumes that signal and **auto-rolls-back** on a regression.

…without sending real email, emailing agent owners (the HITL landmine, see
`feedback_hitl_sends_notify`), burning SES/Resend IP reputation, or polluting
`usage_events` / `usage_summaries` / analytics.

## Goals and non-goals

**Goals**
- One **critical-path round-trip** definition (`SMTP → emailauth → agent lookup → DB → outbox → webhook delivery → HMAC verify`) reused by e2e tests, a self-host CLI, and the prod prober/gate — no duplication.
- **Zero external side effects by default**: outbound exercised without egress; no owner notifications; no metering/billing/analytics writes for probe traffic.
- Extend the existing `deploy.yml` rollback machinery rather than replace it; **fail-safe** (a broken prober must not permanently block deploys).
- Leave the OSS genuinely more testable and give self-hosters a deployment validator (open-core/self-host value, see `project_opencore_strategy`).

**Non-goals**
- Comprehensive correctness testing — stays in CI (`internal/e2e`, contract tests).
- True traffic-splitting canary — separate, later effort (single VM today).
- A full metrics/observability platform; real inbox-placement/deliverability testing (optional low-freq canary, see Open questions).
- WebSocket-path probing in v1 (extensible scenario, phase 2).

## Decisions (research-backed)

Three decisions were settled against OSS best-practice research; they revise an
earlier sketch that used a per-agent `synthetic` boolean and a loopback trick.

### D1 — Account `class` enum, not a per-record `synthetic` boolean

Billing and quota are **per-account** (`usage_summaries` is keyed by `user_id`),
so "this is not a real customer" is a property of the **account**, not each agent.

- Add **`account_class`** to the user/account: `standard | internal | system | demo`,
  default `standard`, `NOT NULL`. **Orthogonal to the paid plan** — do *not*
  invent an "internal plan" (that still routes usage through metering/analytics).
- Precedent: Lago's `customer_account_type` ENUM kept separate from plan; Stripe's
  `livemode`/sandbox separation. Anti-pattern avoided: a single `is_test`/`synthetic`
  boolean → "boolean blindness", impossible states, and an expensive late
  boolean→enum migration on a `usage_events`/`messages`-scale table.
- **Centralize the decision**: a single `meteringPolicy(class) -> {meter, bill, analytics}`
  resolved at the *one* point usage is written — **before** the insert, so the
  daily-summary rollup never counts it. This replaces scattering `if synthetic`
  across the relay/outbound/storage write paths ("shotgun surgery").

```go
// internal/usage (or internal/limits) — single source of truth.
type AccountClass string
const (
    ClassStandard AccountClass = "standard"
    ClassInternal AccountClass = "internal"
    ClassSystem   AccountClass = "system" // synthetic-monitoring probe traffic
    ClassDemo     AccountClass = "demo"
)

type MeteringPolicy struct{ Meter, Bill, Analytics bool }

func PolicyFor(c AccountClass) MeteringPolicy {
    switch c {
    case ClassSystem, ClassInternal, ClassDemo:
        return MeteringPolicy{Meter: false, Bill: false, Analytics: false}
    default:
        return MeteringPolicy{Meter: true, Bill: true, Analytics: true}
    }
}
```

`usage.RecordAndCheck` resolves the account class once and short-circuits when
`!policy.Meter` (returns allowed, records nothing, consumes no quota). The probe
account is `system`-class, so it is excluded by the **same gate** as internal
dogfood traffic — no parallel `synthetic` concept. Optional defense-in-depth: stamp
probe requests with a header (the Datadog/Checkly tag-at-edge pattern).

Analytics: in `e2a-ops/analytics/views.sql`, join account class and filter
`WHERE class = 'standard'` on message-count views (usage-based views are already
clean since no usage rows are written for non-`standard` classes).

### D2 — Consumer-side `Sender` interface, not a loopback special-case

Today `internal/outbound.Sender` is a concrete struct wrapping `*SMTPRelay`; the
real-vs-fake relay is a config toggle. Replace with a narrow consumer-side
interface + two adapters (the dead-uniform Go OSS idiom — Grafana, Bytebase, ntfy):

```go
// internal/outbound — interface defined where it is consumed.
type Sender interface { Send(ctx context.Context, msg Message) (*SendResult, error) }

// Real adapter: existing *SMTPRelay-backed sender satisfies Sender.
// CapturingSender — tests assert on .Sent.
// NopSender — the prober: exercises the real compose + DKIM-sign + route path
//             with ZERO egress and ZERO HITL owner-email, by construction.
```

This makes outbound safety a **wiring choice**, not a code branch, and defuses the
HITL owner-notification landmine. Add a **conformance test** so the fake doesn't
drift from the real sender (Google SWE guidance). This supersedes using
`internal/loopback` as the prober's outbound-safety mechanism.

### D3 — One round-trip, three consumers; health endpoints split by depth

No email OSS ships a self-contained inject→deliver→verify-signature loop
(Mail-in-a-Box/Mailcow/Postal do static DNS/config checks only). e2a owns both
ends, so this is both a differentiator and a self-host validator.

- Put the round-trip in a **non-`_test.go` `internal/selftest` (scenarios) package**
  with a per-scenario **`SmokeSafe`** flag (the Kubernetes `e2e.test` /
  Vault `operator diagnose` + `/sys/health` reuse pattern). Consumed by:
  (a) `internal/e2e` tests, (b) `e2a selftest` CLI, (c) `cmd/e2a-prober`/the gate.
  `SmokeSafe` is load-bearing — only inbound round-trip and read-only scenarios run
  against prod.
- **Health-endpoint depth split** (cascading-failure foot-gun):
  - `/api/health` (`/livez`) stays **shallow** — liveness, no Postgres. **Never deepen it.**
  - add **`/readyz`** — instance-local readiness (migrations applied, config loaded,
    listeners bound, not draining). A runtime "migrations applied" check here is the
    direct fix for the 2026-05-19 class of incident.
  - add an auth-gated **`/selftest`** — deep round-trip + dependency diagnostics,
    consumed by monitoring/the gate, **never** by an orchestrator restart/route loop.
- Output: IETF `application/health+json` (`status: pass/warn/fail`, per-component
  `checks{}` with `observedUnit: "ms"`) **plus** a process exit code; emit
  `selftest_success` + `selftest_duration_seconds`; redact secrets (the loop touches
  HMAC/DKIM keys).

```go
// internal/selftest
type Scenario struct {
    Name      string
    SmokeSafe bool // safe against prod: read-only / round-trip, no owner-emailing
    Run       func(ctx context.Context, c *Client) Result // Result: pass|warn|fail + ms + detail
}
var All = []Scenario{ /* liveness, authRead, inboundRoundTrip, persistRead, loopbackSend, ... */ }
```

## Proposed design

### Probe account & identity (seed, idempotent)

A `system`-class account; a slug agent `probe@agents.e2a.dev` (shared domain → no
DNS verification) with `inbound_policy='open'` (so unverified probe mail isn't
rejected/dropped) and `HITLEnabled=false`; an API key → `E2A_PROBE_API_KEY`; one
durable webhook subscriber for `email.received` filtered to the probe agent, URL
`http://prober:8090/sink`, **excluded from auto-disable** (else a sink outage
disables it permanently: 10 fails/72h with zero delivered). Provisioned by
`e2a-prober seed` (idempotent). Synthetic inbound messages get a short `expires_at`
so the existing janitor reaps them.

### `cmd/e2a-prober` (main repo)

One binary, several modes (mirrors `cmd/e2a-contract-server`):
- **`serve`** — loop every `PROBE_INTERVAL` (start 30s); hosts `/sink` (webhook
  callback), `/healthz` (prober liveness), `/metrics` (Prometheus), and
  `/status?since=<ts>` (recent results + rolling success rate) for the gate.
- **`run-once`** — single battery run, exit 0/non-0 (local/CI). The gate does **not**
  use this; it queries the always-on `serve` instance so probe logic lives once.
- **`seed`** — idempotent provisioning above.
- **`validate`** — *pre-flight*, no round-trip: parse config, check DB reachability
  and **migrations-applied**, and confirm the probe identity/webhook exist. This is
  the install-time validator self-hosters run *before* a full start (the
  `vault operator diagnose` / `consul validate` split between install-time validation
  and runtime health). Distinct from `run-once`, which exercises the live loop.

**Battery v1** (`internal/selftest.All`, as built): liveness; auth+read
(`GET /v1/agents/{probe}`); **inbound round-trip** (`smtp.SendMail` a unique nonce
to the SMTP listener → await webhook at `/sink` within the round-trip timeout →
verify nonce correlation + HMAC); self-send loopback (outbound API + compose path,
method=loopback, no egress); **agent_lifecycle** (create → get → delete an
ephemeral agent on the probe's verified domain — the only mutating scenario,
self-cleaning via a deferred best-effort delete, no email/owner-notify/metering);
and **mcp_http_round_trip** (the deployed streamable-HTTP MCP surface: a stateless
Bearer-authed `tools/list` then a `whoami` `tools/call` over the real SSE
transport — both read-only; skips-as-pass when `E2A_PROBE_MCP_URL` is unset, so a
stack without an MCP endpoint stays green); and **delegated_auth**, which asks a
trusted fixed-principal issuer endpoint for one fresh short-lived token and
uses it for `GET /v1/agents?limit=1`. The endpoint accepts no identity,
audience, scope, role, or TTL input. This covers issuer signing, JWKS discovery,
the delegated claim policy, external-principal mapping, and account
authorization without allowing arbitrary public traffic to manufacture the
availability signal. It skips while unset and becomes fail-closed when
`E2A_PROBE_REQUIRE_DELEGATED_AUTH=true`.
*(fast-follow)* 5xx SLI from `e2a:/metrics`.

### SLI exposure on `e2a` (phase 2 / fast-follow)

A chi/Huma middleware counting requests by route+status + latency, on an internal
`GET /metrics`, built on the existing `internal/telemetry` abstraction. The prober
scrapes it for the gate's 5xx signal — catches regressions on *real* traffic.

### Wiring (e2a-ops)

- 5th compose service `prober` (`e2a-prober serve`), internal, port published to
  **loopback only** (`127.0.0.1:8090:8090` — reachable by the VM deploy script,
  never by Caddy/public). Runs across `e2a` restarts and observes whichever version
  is live. `E2A_PROBE_API_KEY` added to `/opt/e2a/.env`.
- **Continuous monitor**: UptimeRobot / GCP Cloud Monitoring scrape
  `127.0.0.1:8090/metrics`; alert on probe failure/latency; prober-down pages.
  **Alert discipline** (distinct from the gate's N-consecutive-green): page only
  after **M consecutive failed probes** (start M=2) with a per-probe `PROBE_TIMEOUT`,
  so a single transient blip doesn't wake on-call ("the internet is flaky — retry
  before alerting"). The gate gates on *green-after-deploy*; the monitor pages on
  *sustained-red*.
- **Deploy bake-gate** — extend `deploy.yml` *after* the existing `/api/health` pass:
  1. record `deploy_ts`;
  2. for ≤ `BAKE_SECONDS` (start 300) poll `http://localhost:8090/status?since=<deploy_ts>`,
     requiring **N consecutive green probes** (start N=3) and *(phase 2)* 5xx < threshold;
  3. on breach → reuse the existing `:rollback` re-tag + `up -d --pull never` + re-poll path;
  4. **fail-safe**: if the prober is unreachable/inconclusive, fall back to today's
     liveness-only behavior and log loudly.

### API surface impact

**No changes to the public `/v1` contract or the SDKs.** The prober is only a
*client* of existing endpoints (`GET /v1/agents/{email}`, `GET .../messages`, the
`POST` send) — no `/v1` request/response shapes change, so `api/openapi.yaml` stays
byte-identical and `TestSpecGoldenNoDrift` / `make spec-check` stay green with no
SDK regen.

Two rules keep that true:
- **New endpoints (`/readyz`, `/selftest`) are operational, not `/v1`.** Register
  them on the **non-Huma mux** (the gorilla/mux fallback where `/api/health` already
  lives, `cmd/e2a/main.go:556`), **not** the Huma `/v1` router — so they never enter
  `api/openapi.yaml` or the SDK pipeline. (`/metrics`, the fast-follow, likewise.)
- **`account_class` stays server-side only.** It must not appear in any `/v1`
  agent/account response body; it is read by the metering gate alone. Surfacing it
  would be a (technically additive) contract change touching the spec + SDKs — avoid.

## Edge cases and failure handling

- **Quota false alarms** → solved by D1 (probe traffic never metered, never counts against quota).
- **SPF/DKIM fail on synthetic inbound** → expected; `inbound_policy='open'` keeps it delivered (the existing `internal/e2e/webhooks_e2e_test.go` round-trip confirms unverified mail still fires the webhook). Don't assert auth=pass.
- **Retry-worker cadence** → round-trip latency includes the `SubscriberRetryWorker` poll interval; set `PROBE_TIMEOUT` ≥ that + margin (confirm exact interval during impl; it's a tuning knob).
- **Synthetic webhook auto-disabled** → exclude it from `AutoDisableFailingWebhooks` and/or re-enable on prober start.
- **Prober as SPOF for deploys** → fail-safe gate (inconclusive ≠ block).
- **Nonce cross-talk** → correlate each send to its delivery by unique Message-ID; ignore unrelated deliveries; don't over-assert on volatile fields (timestamps, generated IDs) → flapping.
- **Sink reachability** → `e2a` resolves `prober` on the shared compose network; if prober is down, only synthetic deliveries fail (no customer impact).
- **Deep checks behind liveness/readiness** → forbidden (cascading restart/eviction); the round-trip lives only on `/selftest` + the prober.

## Scalability and extensibility notes

- The battery is a `[]Scenario` — add WebSocket-receive, the deliverability canary, or new SLIs without touching the harness.
- `account_class` + `meteringPolicy` is reusable for demo accounts, load tests, and analytics hygiene beyond probing.
- `/status` decouples the gate from probe internals — a future real canary (second `e2a` behind Caddy weighted upstreams) consumes the same endpoint.
- `/metrics` on `e2a` seeds real observability (the `telemetry` abstraction anticipates Prometheus).
- Go 1.25 `testing/synctest` gives deterministic fake time with no `Clock` interface; keep the `Start`/`Tick`/`processOne` worker split; replace e2e `time.Sleep`/`WaitFor` polling with a completion signal (river-style `TestSignal`, no-op in prod).

## Verification strategy

- **Unit**: `meteringPolicy` per class; `Sender` interface + `CapturingSender`/`NopSender` conformance test; nonce correlation; HMAC verify; `/status` window logic.
- **Integration** (against `cmd/e2a-contract-server`): run `e2a-prober run-once` end-to-end; assert all checks pass **and that no `usage_events`/`usage_summaries` rows are written for the `system` account** (the regression that matters most).
- **Gate simulation**: point the bake-gate at a deliberately broken build (webhook worker disabled) → confirm rollback; at a healthy build → confirm promote; kill the prober → confirm fail-safe fallback.
- **Migration**: idempotency (re-run) + a DB-backed test for any package writing the account-class column (per CLAUDE.md schema-change rule).
- **Manual**: one staged deploy confirming the rollback re-tag / `--pull never` path still works with the added bake step.

## Slices (dependency order)

1. **Account-class gate + `Sender` interface** (`e2a`) — idempotent migration adding
   `account_class`; thread onto the account; `meteringPolicy` single gate in
   `usage.RecordAndCheck`; `Sender` interface + `NopSender`/`CapturingSender` +
   conformance test; analytics view filter. Test: zero usage rows for `system` class.
2. **`internal/selftest` + `cmd/e2a-prober`** (`e2a`) — scenarios package (`SmokeSafe`),
   battery 1–5, `seed`/`run-once`/`serve`, `/sink` `/healthz` `/metrics` `/status`.
   Add `/readyz` (migrations-applied check); leave `/api/health` shallow. Verify
   `run-once` against `e2a-contract-server`. Confirm `SubscriberRetryWorker` interval
   → set `PROBE_TIMEOUT`.
3. **`prober` compose service** (`e2a-ops`) — `e2a-prober serve`, loopback-only port,
   `E2A_PROBE_API_KEY` into `/opt/e2a/.env`, analytics view filter.
4. **Bake-gate in `deploy.yml`** (`e2a-ops`) — `/status?since=` poll, N consecutive
   green, reuse `:rollback` path, fail-safe. Stage-test rollback + promote.

**Fast-follow (post-GA)**: `e2a` `/metrics` 5xx instrumentation (battery #6 + gate
signal); opt-in, rate-limited real-send deliverability canary (separate from the
per-cycle battery).

## Open questions

- `SubscriberRetryWorker` poll interval — confirm exact value during slice 2 to set `PROBE_TIMEOUT`.
- `BAKE_SECONDS` / consecutive-green `N` / 5xx threshold — starting point 300s / 3 / 2%; tune on staging.
- Deliverability canary cadence (hourly?) and the owned sink address — deferred to fast-follow.
- Whether to expose `e2a selftest` as a CLI subcommand in the OSS binary at GA or fast-follow (self-host value vs. GA surface area).
