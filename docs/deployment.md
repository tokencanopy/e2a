# Deployment

There are three audiences who configure something — and confusing them is the main UX pothole of self-hosted projects. The split:

| Audience | What they configure | Where |
|---|---|---|
| **Server operator** — runs the Go backend | DB, signing key, SMTP, OAuth, optional shared domain | `config.yaml` + `E2A_*` env |
| **CLI user** — drives an inbox from a terminal | Deployment URL + login | `E2A_URL` + `e2a login` |
| **SDK / MCP user** — calls `/v1` from code | API host + key | `E2A_API_URL` + `E2A_API_KEY` |
| **Web dashboard deployer** — hosts the Next.js dashboard | Public site URL + branding | `NEXT_PUBLIC_*` build-time env |

## Server operator

Copy `config.example.yaml` to `config.yaml` and fill in values, or set the environment variables below (env wins over file). All secrets should be set via env, never the file.

| Variable | Required | Description |
|----------|----------|-------------|
| `E2A_DATABASE_URL` | yes | Postgres connection string |
| `E2A_HMAC_SECRET` | yes | Master HMAC secret for approval/magic-link tokens and internal key derivation |
| `E2A_ENV` | for production | `development` (default) or `production`; overrides `env:` in `config.yaml`, and any other value fails startup. This is the **only** way to reach production mode on the published Docker image, which bakes `config.example.yaml` (`env: "development"`) to a fixed path — pass `-e E2A_ENV=production` (or set it in your compose `environment:`) or the deployment runs in development mode permanently. Development mode short-circuits DNS domain verification (every domain reports verified with no lookup, so it doubles as a domain-ownership bypass on multi-user deployments; a loud `WARNING:` is logged per check) and skips the production config guards below. |
| `E2A_PUBLIC_URL` | for HITL emails | Externally visible base URL (e.g. `https://e2a.example.com`); required to render absolute magic-link URLs |
| `E2A_SHARED_DOMAIN` | optional | Mail domain backing slug-based agent registration (e.g. `agents.example.com`). When set, users can register agents with just a slug; when empty, every agent must use a custom domain that the user verifies. The shared domain itself becomes reserved (cannot be claimed as a custom domain). |
| `E2A_GOOGLE_CLIENT_ID` | for OAuth login | Google OAuth client ID for dashboard sign-in |
| `E2A_GOOGLE_CLIENT_SECRET` | for OAuth login | Google OAuth client secret |
| `E2A_OIDC_ENABLED` | no (default off) | Turns on generic OIDC login as an additional sign-in path (see below) |
| `E2A_OIDC_ISSUER_URL` | if OIDC enabled | Expected ID-token issuer / discovery base URL |
| `E2A_OIDC_CLIENT_ID` | if OIDC enabled | Confidential client ID registered with the OIDC provider |
| `E2A_OIDC_CLIENT_SECRET` | if OIDC enabled | Confidential client secret |
| `E2A_OIDC_REDIRECT_URL` | if OIDC enabled | Registered absolute callback URL |
| `E2A_OIDC_USER_ID_CLAIM` | if OIDC enabled | ID-token claim naming an existing `users.id` — OIDC login never provisions new users |
| `E2A_PROVISIONING_ENABLED` | no (default off) | Internal-only. Turns on `POST /api/internal/users/provision`, which lets an external control plane create users idempotently ahead of their first sign-in (see below) |
| `E2A_PROVISIONING_SECRET` | if provisioning enabled | Internal-only, env-only. Shared HMAC key the control plane signs provisioning request bodies with (`X-E2A-Internal-Signature`); must match on both ends. Production requires ≥32 bytes — generate with `openssl rand -hex 32` |
| `E2A_DELEGATED_ENABLED` | no (default off) | Turns on delegated access-token verification (RFC 9068 `at+jwt`) so an external control plane can call `/v1` on behalf of its signed-in humans (see below). The rest of the policy — `issuer_url`, `audience`, `authorized_party`, `required_scope`, `allowed_algorithms`, `max_token_lifetime_seconds`, `clock_skew_seconds`, `required_claims`, `forbidden_claims` — is non-secret and lives in the `delegated:` block of `config.yaml`; there is no shared key |
| `E2A_OUTBOUND_SMTP_HOST` | for outbound | Upstream SMTP host (e.g. `email-smtp.us-east-1.amazonaws.com`) |
| `E2A_OUTBOUND_SMTP_PORT` | for outbound | Upstream SMTP port (typically `587`) |
| `E2A_OUTBOUND_SMTP_USERNAME` | for outbound | Upstream SMTP username |
| `E2A_OUTBOUND_SMTP_PASSWORD` | for outbound | Upstream SMTP password |
| `E2A_OUTBOUND_SMTP_FROM_DOMAIN` | for outbound | Domain used in `From:` of outbound mail |
| `E2A_SMTP_PROXY_TRUSTED_CIDRS` | no (default empty) | Comma-separated CIDRs allowed to present PROXY protocol headers on the SMTP listener (e.g. `172.30.0.0/24,10.0.0.0/8` for HAProxy in front); empty disables parsing. Mirrors `smtp.proxy_trusted_cidrs`. A trusted peer connecting without a header waits up to 5s for one before the SMTP banner — keep health checks TCP-connect-only (or tolerate the delay); direct SMTP clients in a trusted CIDR see a ~5s greeting delay. |
| `E2A_USAGE_TRACKING` | no (default `false`) | Set to `true` to write per-message rows into `usage_events` / `usage_summaries`. The hosted deployment uses these for billing reconciliation; self-hosters typically don't need them. |
| `E2A_CONTENT_SCAN_ENABLED` | no (default `false`) | Master switch for [content screening](../README.md#content-screening) (prompt-injection / phishing detection, `internal/identity/protection.go`). Off by default everywhere, including self-host — set to `true` to turn it on. This only unlocks the capability at the deployment level: each agent's scan still defaults to `off` and must be turned on per-agent via `PUT /v1/agents/{email}/protection` (`scan_sensitivity` = `low`/`medium`/`high`); while this var is unset or `false`, the protection API clamps every agent's sensitivity to `off` regardless of what's requested. |
| `GEMINI_API_KEY` | no | Google AI Studio key. When set, the Gemini LLM-as-detector layer is added to the inbound piguard screening engine alongside the built-in heuristics detector (outbound agent-mail screening stays heuristics-only). Requires `E2A_CONTENT_SCAN_ENABLED=true` and a per-agent scan sensitivity above `off` to actually run — see above. Obtain at [aistudio.google.com/apikey](https://aistudio.google.com/apikey). `GOOGLE_API_KEY` is accepted as a fallback. |
| `GEMINI_EVAL_MODEL` | no (default `gemini-3.1-flash-lite`) | Overrides the Gemini model used by the LLM-as-detector layer. Only takes effect when `GEMINI_API_KEY`/`GOOGLE_API_KEY` is also set. |
| `E2A_GEMINI_DETECTOR_ENABLED` | no (default `true`) | Set to `false` to disable the Gemini detector even when an API key is configured — an operator kill-switch independent of the credential, useful for isolating whether Gemini or heuristics drove a given block/review outcome, or for rolling back without touching secrets. |

`env: production` in [config.example.yaml](../config.example.yaml) — or the `E2A_ENV=production` override above, which is the only way to set it on the published Docker image — enforces TLS for SMTP, HTTPS for webhook URLs, HMAC-secret strength, and real DNS domain verification. Leave it as `development` for local work.

Non-secret request-rate tuning lives in `config.yaml`. The
`rate_limits.poll_per_minute` setting controls the per-user budget shared by
authenticated message, conversation, and webhook reads across all agent
clients and dashboard sessions for that account. It defaults to 240 requests
per minute and, like the other in-memory rate limits, applies independently to
each server process.

### Shared-domain setup

If you set `E2A_SHARED_DOMAIN` (or `shared_domain` in `config.yaml`) so users can register agents with just a slug — `alice@agents.yourcompany.com` — there are two parts to it: DNS you set up once, and a database row the server takes care of for you.

**You do (once, externally):**

1. Pick the subdomain (e.g. `agents.yourcompany.com`).
2. Add an `MX` record pointing it at the host running the e2a SMTP relay.
3. Add `A`/`AAAA` records for that host.
4. Open inbound port 25 (the SMTP listener defaults to `:2525` — either change `smtp.listen_addr` to `:25` or NAT 25→2525).
5. Provision a TLS cert for the SMTP domain and set `smtp.tls_cert` / `smtp.tls_key`.
6. Add SPF/DKIM TXT records on the subdomain so outbound mail from your relay isn't rejected by recipient mail servers.

**The server does (automatically, at startup):**

The shared domain needs a row in the `domains` table — it's the FK target for every agent registered against it. The server seeds this row idempotently every time it boots: `INSERT … ON CONFLICT DO NOTHING` against the configured `shared_domain`, with `user_id = NULL` and `verified = true` (system-owned, pre-verified). You don't run a migration, you don't `psql` anything by hand. Change the configured domain later? Restart and the new row appears; the old one stays as a harmless orphan because the API layer reads `cfg.SharedDomain` to decide what's reserved, not the table.

If you leave `shared_domain` empty, slug registration is disabled and every agent must use a custom domain the user verifies — no DNS setup required from you.

### Observability

Wire your orchestrator to the two probe endpoints — they answer different
questions and must not be swapped: `GET /api/health` is shallow liveness
(restart policy; never checks the DB), `GET /readyz` is instance-local
readiness (DB reachable + migrations applied + not draining; use it for load
balancer routing so instances leave rotation during deploys and graceful
shutdown). Enable Prometheus metrics with the `metrics:` config block (or
`E2A_METRICS_ENABLED=true`) — exposition binds a separate loopback-default
listener, never the public API handler. For continuous black-box monitoring
of the full critical path (SMTP round-trip, outbound, WebSocket push, MCP),
run `e2a-prober serve` alongside the stack. Metric catalog, SLI/PromQL
aggregation guidance, and initial SLO targets: [observability.md](observability.md).

## CLI user

The CLI only needs the deployment URL — the rest is auto-discovered.

```bash
export E2A_URL=https://e2a.example.com   # default: https://e2a.dev
e2a login                                # browser flow; saves api key + auto-discovers shared domain
```

The CLI hits `GET /v1/info` on login and caches `shared_domain` to `~/.e2a/config.json`, so it resolves agent addresses to the right shared domain on any deployment without further config. Escape hatches if you need to override or skip the discovery step:

| Variable | Description |
|---|---|
| `E2A_URL` | CLI base URL (default `https://e2a.dev`) — the deployment root that serves the `e2a login` browser flow and proxies the `/v1` API |
| `E2A_API_KEY` | Bypass `e2a login` — useful in CI |
| `E2A_SHARED_DOMAIN` | Force the shared domain instead of auto-discovering it |

The CLI does **not** read `E2A_API_URL` (the SDK var below). It uses `E2A_URL` and defaults to `https://e2a.dev`, so a self-hoster who only exports `E2A_API_URL` leaves the CLI pointed at production.

## SDK / MCP user

The SDKs and the MCP server only ever call `/v1`, so they take the **API host** — not the CLI's deployment root:

```bash
export E2A_API_URL=https://api.e2a.example.com   # default: https://api.e2a.dev
export E2A_API_KEY=e2a_…
```

```ts
// env is only the fallback — you can pass it directly instead
const client = new E2AClient({ baseUrl: "https://api.e2a.example.com", apiKey: "e2a_…" });
```

| Variable | Description |
|---|---|
| `E2A_API_URL` | SDK + MCP base URL (default `https://api.e2a.dev`) — the `/v1` API host alone. `E2A_BASE_URL` is the SDKs' former name for it, still read with a deprecation warning. |
| `E2A_API_KEY` | The API key the SDK / MCP authenticates with |

`E2A_URL` and `E2A_API_URL` are deliberately separate: the CLI opens browser
pages (the login flow, `/get-started`) that only the web front serves, so it
needs the deployment root, while the SDKs and the MCP server only ever call
`/v1` and want the API host. **Pointing the CLI at an API host breaks
`e2a login`; pointing an SDK at the deployment root only works if that host
also proxies `/v1`.** On a single-host deployment both can be the same URL.
Setting `E2A_API_URL` for an SDK also tells a **server** running on that host
what its own externally visible API base is (it is the OAuth issuer) — keep
that in mind if you run a server and point an SDK at a *different* deployment
from the same environment.

The TypeScript and Python SDKs follow the same pattern: pass `baseUrl` (or `base_url`) once and call `client.info()` (TS `E2AClient`, Python `AsyncE2AClient`) if you need the deployment's shared domain in your own code.

## Web dashboard deployer

The Next.js dashboard ships as a static export, so its config is inlined at build time via `NEXT_PUBLIC_*` env vars. Copy [`web/.env.example`](../web/.env.example) to `web/.env.local` and adjust:

| Variable | Description |
|---|---|
| `NEXT_PUBLIC_SITE_URL` | Externally visible base URL of the dashboard. Used for SEO metadata, sitemap, and canonical URLs. Default: `http://localhost:3000`. |
| `NEXT_PUBLIC_SITE_NAME` | Display name in titles, OpenGraph, and structured data. Default: `e2a`. |
| `NEXT_PUBLIC_AGENTS_DOMAIN` | Shared mail domain shown in landing-page code samples (e.g. `agents.example.com`). When empty, samples fall back to `your-domain.com`. |
| `NEXT_PUBLIC_FEEDBACK_EMAIL` | Address shown on the feedback form. Empty hides the "or email us at …" line. |
| `NEXT_PUBLIC_GOOGLE_SITE_VERIFICATION` | Google Search Console token. Only emitted into `<head>` when set, so forks don't inherit upstream's property. |
| `NEXT_PUBLIC_PRICING_PATH` | Site-relative path of the pricing page, when the deployment serves one (the hosted deployment sets `/pricing`). Only used to add the page to the sitemap. Leave empty if there is no pricing route — a sitemap entry that 404s is a crawl-quality problem. |
| `NEXT_PUBLIC_E2A_SIGN_IN_URL` | Sign-in door for the dashboard's "Sign in" links. Default: `/api/auth/login` (legacy Google OAuth). Set to `/api/auth/oidc/login` to make the hosted TokenCanopy OIDC door the default — only when the server runs with `E2A_OIDC_ENABLED=true`, otherwise the link 404s. Button copy follows: "Sign in with Google" for the legacy door, "Sign in with TokenCanopy" for the hosted OIDC route, and provider-neutral "Sign in" for any other configured URL. |

## MCP HTTP server

The MCP transport (`ghcr.io/tokencanopy/e2a-mcp-http`, built from
[`mcp/Dockerfile`](../mcp/Dockerfile)) is a separate stateless process that
forwards client bearers to the API server. It listens on port **3000**
(container-internal; compose maps host 8765). The full configuration and
operations reference lives in [`mcp/README.md`](../mcp/README.md) and
[`docs/runbooks/mcp-server.md`](runbooks/mcp-server.md); the deployment
essentials:

- **Required env**: `E2A_API_URL` must point at the API server (inside a
  compose network that's the container hostname, e.g. `http://e2a:8080`).
  `MCP_ALLOWED_HOSTS` must list every externally used hostname or clients
  get 421. Behind a proxy, set `MCP_PUBLIC_URL` (and
  `MCP_AUTHORIZATION_SERVER_URL` when the OAuth AS must be reached via a
  different, host-visible origin).
- **Endpoints**: `GET /healthz` (liveness — never touches the backend),
  `GET /readyz` (readiness — probes `{E2A_API_URL}/api/health`, 2s timeout,
  result cached 10s; 503 + `Retry-After: 10` (the failure-cache TTL) when the API is unreachable),
  `GET /metrics` (Prometheus exposition). All unauthenticated. Wire
  liveness → `/healthz`, readiness → `/readyz` — never liveness →
  `/readyz`, or a backend outage restarts healthy MCP replicas.
- **Healthcheck examples** — the image ships one (`HEALTHCHECK` in
  `mcp/Dockerfile`, node fetch against `/healthz`). For another
  orchestrator:

  ```yaml
  # docker-compose
  healthcheck:
    test: ["CMD", "node", "-e", "fetch('http://localhost:3000/healthz').then(r => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))"]
    interval: 10s
    timeout: 3s
    retries: 5
  ```

  ```yaml
  # kubernetes
  livenessProbe:  { httpGet: { path: /healthz, port: 3000 } }
  readinessProbe: { httpGet: { path: /readyz,  port: 3000 } }
  ```

- **Graceful shutdown**: SIGTERM/SIGINT stops the listener and drains with
  a 30s ceiling; stateless, so there is nothing else to drain.

## Scaling and limitations

**Most state is already DB-coordinated.** The HITL expiration worker, the webhook retry worker, and the periodic cleanup worker all use Postgres `SELECT … FOR UPDATE SKIP LOCKED` (or rely on `DELETE` idempotency for cleanup), so running multiple replicas concurrently is safe — only one worker claims a given pending message at a time, no duplicate sends. User sessions live in Postgres and the OAuth nonce travels in a cookie + the OAuth state parameter, so dashboard sign-in survives load-balancer rebalancing.

That leaves two real horizontal-scaling caveats:

1. **WebSocket fan-out is per-replica.** The hub is an in-memory `map[agentID]*conn` ([internal/ws/hub.go](../internal/ws/hub.go)). An agent connected to replica A won't receive real-time notifications for events that happen on replica B — an inbound mail arriving at B's SMTP relay, a HITL approval firing on B's API, etc. Messages aren't lost: they stay `unread` in Postgres and the agent drains them on the next reconnect or REST fetch. They're just not pushed in real-time. Fix: a shared pub/sub (Redis, NATS) for cross-replica notification fan-out, or sticky sessions plus a per-replica routing layer.
2. **Rate limits multiply with replica count.** Limiters are in-process (per-IP, per-agent, per-user — see `ratelimit.New(...)` calls in [internal/agent/api.go](../internal/agent/api.go)). With two replicas the effective caps are 2× looser, not stricter. Operators who need exact global limits would move the limiters to a shared store (Redis, or a Postgres-backed token bucket).

**Vertical scaling is fine.** The API, the SMTP relay, and all three background workers run safely on multiple replicas today — the only paths that need attention before you do are the two above.

**Dashboard auth is Google OAuth plus an optional generic OIDC login.** [`internal/auth/auth.go`](../internal/auth/auth.go) imports `golang.org/x/oauth2/google` directly and the config exposes `google_client_id` / `google_client_secret`. [`internal/auth/oidc.go`](../internal/auth/oidc.go) additionally implements an off-by-default OpenID Connect relying party (`E2A_OIDC_ENABLED` + the `E2A_OIDC_*` vars above) for teams on Microsoft Entra, Okta, or another standards-compliant OIDC provider. Start the flow at `GET /api/auth/oidc/login`, which accepts the same optional `return_to` and `cli_callback`/`cli_state` query parameters as the Google login (same validation rules, same post-login behavior). It is an additional login path, not a Google OAuth replacement: it maps a configured ID-token claim to an existing e2a user and never provisions new ones, so accounts must already exist (e.g. via Google OAuth or `-bootstrap-email`) before OIDC login works for them. The CLI and SDKs authenticate with API keys, which are provider-agnostic.

**User provisioning for an external control plane is opt-in and internal-only.** When `E2A_PROVISIONING_ENABLED=true`, the server exposes `POST /api/internal/users/provision` (not part of the public `/v1` API or the OpenAPI spec) so an operator's external control plane can create e2a users ahead of their first sign-in. The caller sends `{"external_ref", "email", "name"?}` and authenticates with a hex HMAC-SHA256 of the raw request body in `X-E2A-Internal-Signature`, keyed by `E2A_PROVISIONING_SECRET` (env-only, deliberately separate from the limits internal API secret so each can be rotated independently). `external_ref` is the idempotency key — it becomes the row's `google_subject` (`bootstrap:<ref>`), so a replay returns `200` with the same `user_id`; a fresh create returns `201`; a different `external_ref` carrying an email another account already holds returns `409 {"error":"email_conflict"}` and never attaches or merges. Provisioning creates only the user row — no session, API key, or limits row. Disabled (the self-host default), the endpoint 503s.

**Delegated access-token verification is opt-in and generic.** When `E2A_DELEGATED_ENABLED=true` (plus the `delegated:` config block), the server verifies short-lived, audience-bound OAuth 2.0 access tokens (RFC 9068, JOSE header `typ="at+jwt"`) minted by one configured OIDC issuer, so an external control plane can call the public `/v1` API on behalf of its signed-in humans without holding e2a credentials. e2a runs OIDC discovery against `issuer_url`, verifies the token's signature against the issuer's published JWKS, and pins the exact `issuer`/`audience`/`authorized_party`/singleton `scope`, the `exp − iat` lifetime, the required context claims (with optional allowed-value sets), and the absence of the forbidden claims — all deployment data, so OSS ships no operator-specific claim policy. A verified token maps its `(issuer, subject)` pair to an existing local user and acts with **account** scope, exactly like an account API key; it never provisions on token presentation, and an unmapped subject is a `401`. The feature is fully off by default and isolated: every existing credential path (API keys, OAuth `ate2a_`, agent JWTs, session cookies) keeps working unchanged, including through a total delegated-verifier outage (which fails delegated requests `503`, never the others). A compact JWT whose protected header says `typ="at+jwt"` is owned by this verifier even while it is disabled — it never falls through to the API-key path. Issuer *network* unavailability is not fatal to startup (discovery retries in the background); a malformed static `delegated:` policy is.

**Delegated identity mapping is populated explicitly, never on token presentation.** The `(issuer, subject) → users.id` map lives in its own table (`external_principal_mappings`), additive beside `google_subject` (standalone sign-in is never disturbed). Two internal, HMAC-signed surfaces (same `E2A_PROVISIONING_SECRET` boundary as provisioning) populate it: the provision request above accepts an optional `external_issuer` (when present, it must byte-equal the configured delegated issuer, and the provision transactionally also inserts the `(issuer, external_ref)` mapping); and a dedicated `POST /api/internal/users/external-principals/attach` accepts `{"issuer", "external_ref", "user_id"}` for reconciling accounts that predate delegated provisioning — `201` create / `200` same-triple replay / `409 {"error":"external_principal_conflict"}` (the pair is mapped to a different user, never auto-merged) / `404 {"error":"user_not_found"}`. Attach `503`s with `delegated_verifier_not_configured` until a delegated issuer is configured. Neither surface ever changes a user's email, name, or `google_subject`.

**Otherwise infra-agnostic.** The Go binary runs on any container host (Docker, Podman, k8s, ECS, Fly, Cloud Run, …). Storage is plain Postgres (tested on Postgres 16 — the version exercised by docker-compose and CI) — managed (RDS, Cloud SQL, Neon, Supabase) or self-managed. Email goes out via standard SMTP, not a vendor SDK. Attachments live in Postgres rows, so there's no S3/GCS dependency. No queue, no Redis, no separate worker process. Secrets are read from env vars, so any secret manager that injects env at start time works.
