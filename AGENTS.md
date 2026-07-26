# AGENTS.md

Guidance for AI coding agents working in this repository (Claude Code reads
this file via the `@AGENTS.md` import in `CLAUDE.md`). Assumes no prior
knowledge of the project. For deeper prose, see `README.md` (product),
`CONTRIBUTING.md` (contributor workflow), `docs/` (API reference,
deployment, design docs), and `SECURITY.md`.

## Project overview

e2a is an **authenticated email gateway for AI agents**: it gives an agent a
real email address. Inbound mail arrives over SMTP, sender-verified
(SPF/DKIM/DMARC), and reaches the agent as structured authentication evidence
via webhook, WebSocket, REST polling, or MCP tools. Outbound mail goes out
through an HTTP API (SMTP relay agent-to-agent, upstream SMTP such as SES
agent-to-human), with an optional human-in-the-loop (HITL) approval gate and
opt-in prompt-injection content screening (piguard).

Polyglot monorepo, Apache-2.0, GitHub: `tokencanopy/e2a`. The `/v1` API and
SDKs are release candidates; stable compatibility guarantees start at the
announced GA tag.

| Surface | Path | Stack | Package |
|---|---|---|---|
| Backend server | `cmd/e2a/` + `internal/` | Go (module `github.com/tokencanopy/e2a`) | `bin/e2a` binary |
| CLI | `cli/` | TypeScript, Node ≥18 | `@e2a/cli` (npm) |
| TypeScript SDK | `sdks/typescript/` | TypeScript | `@e2a/sdk` (npm) |
| Python SDK | `sdks/python/` | Python ≥3.9, hatchling | `e2a` (PyPI) |
| MCP server | `mcp/` | TypeScript, Express | `@e2a/mcp-server` (hosted-only; npm publish retired) |
| Web dashboard | `web/` | Next.js 16 App Router, React 19, Tailwind CSS 4, static export | private |
| Design system | `design-system/` | React + tsup + Storybook | `@e2a/ui` ("Loft", consumed via `file:` dep) |
| Agent plugin | `plugins/e2a/` | Markdown skills + manifests | Claude / Codex / Cursor marketplaces |

Toolchain versions: `go.mod` declares Go 1.25; CI and the Dockerfiles build
with Go 1.26. Node: engines `>=18`, CI runs on 22. Python: `requires-python
>=3.9`, CI runs on 3.12.

## Repository layout

- `cmd/e2a/` — main server binary (SMTP relay + HTTP API). Other binaries:
  `e2a-contract-server` (SDK contract-test instance), `e2a-prober`
  (critical-path self-test runner), `e2a-openapi-{normalize,codegen-normalize,
  sdk-check,security-check}` (compat-gate/codegen helpers), `piguard-eval`.
- `internal/` — all backend packages (see "Backend architecture").
- `api/openapi.yaml` — committed OpenAPI 3.1 doc, golden-tested against the
  live Huma handlers; `api/testdata/oasdiff/` holds compat-policy fixtures.
- `migrations/` — sequential `0NN_*.sql`, embedded via `migrations/embed.go`
  and auto-applied at startup.
- `tests/contract/` — Go contract tests driven by `scenarios.yaml`;
  `tests/e2e-prod/` — production smoke-test harness (TypeScript).
- `docs/` — `api.md`, `deployment.md`, `events.md`, `templates.md`,
  `data-handling.md`, `api-compatibility-gate.md`, `design/`, `runbooks/`.
- `scripts/` — CI guardrails (OpenAPI compat check, plugin validator,
  SDK version-sync check, repo text-integrity check).
- `examples/adk-cloud-webhook/`, `examples/agent-framework-webhooks/` —
  Python/TypeScript webhook examples, each tested by their own CI job.
- Root config: `go.mod`/`go.sum`; `package.json`/`package-lock.json` (npm
  workspaces `cli`, `sdks/typescript`, `mcp`, `design-system` — `web/` and
  `sdks/python/` are NOT workspaces); `Makefile` (Go-side workflows);
  `config.example.yaml` (annotated server config template);
  `docker-compose.yaml` (local dev); `Dockerfile` (server image);
  `.testcoverage.yml` (coverage floors); `VERSION`.

## Build and test commands

### Go backend (via Makefile)

```bash
make build              # go build -o bin/e2a ./cmd/e2a
make run                # build + run with config.yaml (copy from config.example.yaml first)
make test               # ALL Go tests, -tags integration, -p 4 parallel (needs Postgres on :5433)
make test-unit          # unit tests only, no DB needed — fast
make test-integration   # integration tests (needs Postgres on :5433)
make test-e2e           # discovers every package with //go:build integration tests
make cover              # coverage profile cover.out across internal/... (needs Postgres)
make cover-check        # cover + enforce per-package floors in .testcoverage.yml
make docker-up          # docker compose up -d: full stack — Postgres (:5433), Mailpit
                        # (SMTP :1025, UI :8025), dockerized API/SMTP (:8080/:2525),
                        # dashboard (:3000), MCP (:8765). For host-binary dev use
                        # `docker compose up -d postgres mailpit` instead (see below).
make migrate            # manually apply migrations/*.sql to the local DB
make spec               # regenerate api/openapi.yaml from the live Huma handlers
make spec-check         # drift gate: committed spec must byte-equal handler output
make generate-sdk       # regenerate TS + Python SDK bases from the spec (Docker, OAG v7.16.0)
make generate           # spec + generate-sdk
make generate-sdk-check # regenerate + fail on git diff (CI freshness gate)
make openapi-compat-check  # oasdiff backward-compat gate vs origin/main:api/openapi.yaml
make openapi-compat-test   # runs the compat-gate's own test harness (scripts/test-openapi-compat.sh)
```

DB-backed tests default to
`E2A_TEST_DATABASE_URL="postgres://e2a:e2a@localhost:5433/e2a_test?sslmode=disable"`;
exporting your own value overrides it (the Make targets honor the environment).
The test harness (`internal/testutil/db.go`) truncates tables between tests
and auto-applies all migrations on connect. Each test binary derives its
database name beneath the configured base along **two** dimensions
(`<base>_ws<workspace>_pkg_<package>`, self-provisioned on first run):

- **per package**, so parallel packages (`-p 4` in the Make targets) cannot
  truncate each other;
- **per workspace** — a digest of the module root's path — so **two checkouts
  never collide**. Concurrent sessions, agents, and worktrees are isolated
  automatically, with nothing to configure.

`E2A_TEST_DB_SHARED=1` forces the old single-DB behavior. Point a run at an
entirely separate server by exporting `E2A_TEST_DATABASE_URL`.

A fresh base database needs no manual setup — the harness creates the derived
databases and applies all `migrations/*.sql` on connect (the BASE database
itself must exist). Derived databases accumulate on the local server; they are
disposable — drop any `*_pkg_*` database at will, the next run recreates it.
Note that dropping a BASE database does not drop its derived siblings
(harmless — migrate+truncate on connect — but a "reset" that only recreates
the base leaves them behind). `-p 1` is no longer required for cross-package
isolation; it remains useful only when reproducing ordering-sensitive failures.

**Filesystem-walking guards must never walk from the module root.** Walk a
known subtree (`<root>/internal`) or ask git for the file list
(`git ls-files --cached --others --exclude-standard`). Checkouts and worktrees
placed under the root would otherwise be scanned as if they were this tree —
which is exactly how the log-redaction guard started reporting other branches'
stale code as violations. Keeping worktrees OUTSIDE the module root avoids the
class entirely.

### TypeScript (npm workspaces — always use `--workspace` and `npm ci`)

```bash
npm ci                                     # deterministic install of workspace deps
npm run build --workspace @e2a/sdk         # SDK (must build before CLI and MCP)
npm test --workspace @e2a/sdk              # typecheck + vitest unit + type tests
npm run test:contract --workspace @e2a/sdk # contract tests (needs live server)
npm test --workspace @e2a/cli              # CLI tests (vitest)
npm run build --workspace @e2a/cli
npm test --workspace @e2a/mcp-server
npm run build --workspace @e2a/mcp-server
npm run build --workspace @e2a/ui          # design system; dist/ is committed
```

### Python SDK (not a workspace; has its own venv/pip flow)

```bash
cd sdks/python
pip install -e ".[dev]"
pytest tests/ -v                     # unit tests
pytest tests/test_contract.py -v     # contract tests (needs live server)
mypy                                 # type gate (CI runs this too)
```

### Web dashboard (not a workspace)

```bash
cd web
npm ci
npm run dev     # dev server :3000, proxies /api/* to localhost:8080
npm test        # Jest
npm run lint    # ESLint
npm run build   # Next.js static export
```

### First run (local backend)

```bash
cp config.example.yaml config.yaml
docker compose up -d postgres mailpit   # Postgres :5433 + Mailpit :1025/:8025 only
                                         # (plain `make docker-up` also starts the
                                         # dockerized API on :8080/:2525 — conflicts
                                         # with `make run` below)
make run           # API on :8080, SMTP relay on :2525; auto-applies migrations
./bin/e2a -config config.yaml -bootstrap-email you@example.com  # create user + API key
```

Point `outbound_smtp` at Mailpit — `host: localhost`, `port: 1025`,
`from_domain: e2a.localhost` in `config.yaml`, or the env vars
`E2A_OUTBOUND_SMTP_{HOST,PORT,FROM_DOMAIN}` — to exercise outbound flows
(HITL approval notifications, `/v1/agents/{email}/test`) without real SMTP
creds; captured mail appears at http://localhost:8025.

## Backend architecture (`internal/`)

Single Go process (`cmd/e2a/main.go`) running an SMTP relay, the HTTP API,
and background workers. Inbound flow: SMTP → `emailauth` (SPF/DKIM/DMARC) → agent
lookup → canonical authentication persistence → webhook fan-out or WebSocket
push. Outbound is **always queue-first**: the accept transaction atomically
persists the message and enqueues a River job; a worker submits to the relay
and records the terminal outcome.

Key packages, grouped (name — a few words each):

- Intake/parsing: `relay` SMTP intake, `emailauth`/`dkim` SPF/DKIM/DMARC
  eval+alignment, `mailparse` RFC 5322 parsing.
- API surface: `httpapi` typed `/v1` Huma handlers (OpenAPI source of truth),
  `apiserver` assembles the process HTTP handler.
- Agent identity: `agent` CRUD/HITL/REST, `agentauth` agent-identity
  JWKS/JWT, `identity`/`senderidentity` domain-ownership verification +
  sender-identity resolution (incl. SES BYODKIM).
- Delivery: `ws` WebSocket push hub, `loopback` agent-to-self (no network),
  `outbound`/`outboundsend` compose+send via upstream SMTP (queue-first
  River worker), `delivery` SES bounce/complaint feedback via SNS
  (`POST /webhooks/ses`, signature-verified, fail-closed).
- Inbound screening: `inboundprocess`/`inboundpolicy` async worker +
  allow/review/block decisions; `inboundscreen` shared screening core used by
  both the SMTP and loopback self-send paths; `piguard` prompt-injection/
  phishing screening — dependency-free heuristics plus optional Gemini
  LLM-as-detector (`GEMINI_API_KEY`, kill switch
  `E2A_GEMINI_DETECTOR_ENABLED=false`).
- HITL & lifecycle: `hitlworker`/`hitlnotify` review holds (`pending_review`)
  + approval notifications; `approvaltoken` signs/verifies their HMAC
  magic-link tokens; `messagelifecycle` canonical lifecycle-event vocabulary
  (stages/outcomes), durable storage+dedupe, and snapshot reconstruction of
  a message's full history (~4.5k LOC: catalog/model/reconstruct/store).
- Webhooks & jobs: `webhook`/`webhookdelivery`/`webhookpub` subscriptions,
  durable fan-out, SSRF-guarded HTTP POST delivery+retry; `eventpayload`
  typed webhook `data` payloads; `jobs` River-backed durable job runtime
  (Postgres mandatory); `janitor` periodic TTL sweeps (trash, expired holds).
- Send guards & accounting: `idempotency` `Idempotency-Key` storage+replay;
  `unsubscribe` opaque per-recipient unsubscribe tokens/URLs
  (List-Unsubscribe); `usage`/`limits` usage metering + plan/account
  entitlements; `sendramp` per-domain recipient-volume ramping.
- Auth: `auth` API key authentication; `oauth` fosite-based MCP OAuth
  server; Google OAuth + optional generic OIDC login.
- Misc/infra: `ratelimit`; `telemetry` (metrics interface); `logredact`
  PII redaction for process logs before centralized log shipping;
  `emailtemplate`+`startertemplates` (server templates + starter catalog);
  `mailfrom` (custom MAIL FROM); `selftest` (prober's self-test);
  `openapicompat` (compat-gate normalization); `config` (YAML+env);
  `testutil` (test harness); `e2e` (end-to-end suites).

Async-migration feature flags: inbound async processing is opt-in
(`E2A_INBOUND_MODE=async`, default `sync`) and webhook fan-out on River is
opt-in (`E2A_WEBHOOK_FANOUT_MODE=river`, default `legacy`); unknown values
fall back to the historical in-process path. Outbound has no such flag — the
legacy `E2A_OUTBOUND_MODE` switch was removed (guarded by
`TestOutboundModeConfigurationRemoved`).

## OpenAPI spec and SDK generation (contract pipeline)

The `/v1` surface (`internal/httpapi`, Huma v2 on chi) emits OpenAPI 3.1 from
the typed handlers — the handlers are the single source of truth.

- `make spec` regenerates the committed `api/openapi.yaml`;
  `TestSpecGoldenNoDrift` (runs in unit tests and CI) fails on drift.
- `make generate-sdk` runs OpenAPI Generator (pinned image
  `openapitools/openapi-generator-cli:v7.16.0`, via Docker) to regenerate the
  TS base in `sdks/typescript/src/v1/generated/` and the Python base in
  `sdks/python/src/e2a/v1/generated/`. Hand-written ergonomic layers
  (`client.ts` / `client.py`, etc.) wrap the generated bases; package
  scaffolding is suppressed via `.openapi-generator-ignore`.
- **After any `/v1` handler change: run `make generate` and commit
  `api/openapi.yaml` plus both `generated/` trees.** CI gates this with
  `spec-check` and `generate-sdk-check`.
- `make openapi-compat-check` rejects breaking `/v1` changes on PRs (oasdiff
  policy; see `docs/api-compatibility-gate.md`).
- The old swag-annotation pipeline is fully retired — do not reintroduce it.
  The dashboard renders the API reference live from `/v1/openapi.yaml`.

Every API change is expected to maintain parity across all client surfaces:
Go handler, OpenAPI spec, TS SDK, Python SDK, CLI, MCP server, and web
dashboard. `.github/pull_request_template.md`'s "Client surface checklist"
checklists most of these — Go handler + tests, migration, OpenAPI spec +
generated types, TS SDK, Python SDK, CLI, MCP tool, tests at each surface —
but it does **not** list the web dashboard, so that surface must be checked
manually on every API change even though the template won't remind you.

## Client surfaces

- **TS SDK** (`sdks/typescript/`): layered — generated types → `E2AApi` (raw
  HTTP) → `E2AClient` (high-level `.parse()`, `.reply()`); WebSocket in
  `v1/ws.ts`; webhook signature verification in `v1/webhook-signature.ts`.
- **Python SDK** (`sdks/python/`): src layout, async-native (httpx), sync +
  async high-level clients over the generated base; PEP 561 `py.typed`.
- **CLI** (`cli/`): commands login, whoami, agents, keys, protection, send,
  reply, messages, listen, config; config in `~/.e2a/config.json`; `listen
  --forward` proxies WebSocket messages to a local HTTP endpoint. **Exit codes
  (`cli/src/exit.ts`) are a frozen contract** — 0 ok, 1 transient, 2 usage,
  3 held-for-review, 4 auth, 5 permanent request error, 6 timeout,
  7 send-outcome, 8 warn (`doctor` warnings only), 9 config (`doctor` found a
  definite configuration failure). Add new codes, never renumber.
- **MCP server** (`mcp/`): inbox tools over the REST API; hosted HTTP
  transport (image `ghcr.io/tokencanopy/e2a-mcp-http`). **npm publishing is
  retired** (`@e2a/mcp-server` frozen at 0.4.0) — do not configure a trusted
  publisher.
- **Web** (`web/`): Next.js 16 static export; dev rewrites `/api/*` to
  :8080; consumes `@e2a/ui` via a `file:` dep, so `design-system/dist/` is
  committed and CI fails if it drifts (rebuild with
  `npm run build --workspace @e2a/ui` and commit).

## Testing strategy

- **Go tiers**: `test-unit` (no DB), `test-integration` (Postgres),
  `test-e2e` (auto-discovers `//go:build integration` packages); `make test`
  runs everything with `-tags integration -p 4` (safe: per-package test DBs). CI additionally race-checks
  `./internal/sendramp` and `./internal/outboundsend` with `-race`.
- **Coverage ratchet**: `.testcoverage.yml` sets per-package floors (currently
  webhook, webhookpub, webhookdelivery, httpapi, outboundsend, sendramp,
  inboundprocess, jobs, auth, apiserver, loopback, inboundscreen).
  Ratchet floors UP, never down. `make cover-check` runs the same gate CI
  runs (`vladopajic/go-test-coverage`).
- **Contract tests**: TS and Python SDK contract tests run against
  `cmd/e2a-contract-server`, a real e2a instance backed by Postgres (CI builds
  and launches it automatically; locally you need it running for
  `test:contract` / `test_contract.py`).
- **Schema-change rule**: when changing a table shape, add/update DB-backed
  tests in every package that writes direct SQL against that table — the
  idempotent migration runner will not catch drifted runtime SQL.
- TS uses vitest (+ `tsc` type tests), web uses Jest, Python uses pytest +
  mypy. CI workflow: `.github/workflows/test.yml` (jobs: Go, coverage gate,
  Go e2e, web, ts-sdk, agent-framework-examples, adk-cloud-webhook-example,
  ts-contract, cli, mcp, spec gates, python-sdk, python-contract,
  generated-code freshness, design-system dist freshness, SDK-version-sync,
  plugin manifests, repo text integrity, SDK operation coverage, test
  harnesses).
- `tests/e2e-prod/` is a production smoke harness — not part of local dev.
- Two more CI workflows cover the `plugins/e2a/skills/agentify` framework:
  `.github/workflows/agentify-test.yml` (deterministic script/addon/config
  self-tests, every PR touching it) and `agentify-lane-fixtures.yml`
  (golden-fixture lane tests driving `claude -p` over a mocked world; skips
  without `CLAUDE_CODE_OAUTH_TOKEN`).

## Conventions

- **npm**: root `package.json` declares the workspaces; always `npm ci` and
  `--workspace <name>`; commit intentional lockfile updates. In-repo consumers
  must declare an `@e2a/sdk` range the workspace SDK satisfies (CI guardrail:
  `scripts/check-sdk-version-sync.mjs`).
- **Migrations** (`migrations/0NN_*.sql`): must be **idempotent**
  (`CREATE TABLE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`, …),
  **non-destructive on prod-sized tables** (no `ALTER COLUMN TYPE` on
  `messages` / `usage_events`), **sequentially numbered** (never renumber),
  **forward-only** (no down migrations — write a new migration to undo).
  Embedded in the binary and auto-applied at startup;
  `E2A_MIGRATION_MODE=auto|verify|skip` controls behavior.
- **IDs**: resources use `{type}_{random}` IDs (e.g. `msg_abc123`).
- **Config**: `config.yaml` for non-secrets + `E2A_*` env var overrides (env
  wins). Secrets go in env vars only, never in the file. `env: production`
  enforces TLS, HTTPS webhook URLs, and HMAC-secret strength (≥32 bytes).
- **Commits**: `type(scope): short imperative subject` (≤72 chars); types
  include `feat`, `fix`, `chore`, `docs`, `test`, `refactor`, `deps`,
  `release`; scopes are package/surface-shaped (e.g. `feat(api)`,
  `fix(web)`). Not formally Conventional Commits. One PR per feature — no
  bundled drive-by cleanup. CI must be green.
- **Coverage floors** only move up (see Testing strategy).
- **Postgres**: local dev runs on port **5433** (not 5432) via docker compose.
- The Mailpit service in `docker-compose.yaml` is local-dev only — production
  deployments must drop it and point `E2A_OUTBOUND_SMTP_*` at a real relay.

## Security considerations

- The security model rests on: SPF/DKIM/DMARC evaluation, webhook endpoint
  signing + SSRF guard, HITL approval tokens, API keys (only hashes are
  stored), and fail-closed external integrations (SNS signature
  verification, OIDC discovery).
- Never commit secrets. `.env` is gitignored; use it or env vars for
  `E2A_HMAC_SECRET`, OAuth secrets, SMTP credentials, `GEMINI_API_KEY`, etc.
  `docker-compose.yaml`'s baked-in `E2A_HMAC_SECRET` is demo-only.
- Webhook delivery is SSRF-guarded; keep URL validation fail-closed when
  touching `internal/webhookdelivery`. Production mode forces HTTPS webhook
  URLs.
- Report vulnerabilities privately per `SECURITY.md`
  (security@tokencanopy.com or GitHub private advisories) — never a public
  issue. Data handling/retention: `docs/data-handling.md`.

## Deployment and publishing

- **Server image**: root `Dockerfile` (multi-arch, CGO-free, alpine,
  non-root, `/api/health` healthcheck) → `ghcr.io/tokencanopy/e2a` via
  `build-image.yml`. `Dockerfile.prober` builds the co-versioned self-test
  runner. Ports: SMTP 2525, HTTP 8080. Deployment guide:
  `docs/deployment.md`.
- **Migrations ship with the binary** and apply on startup — no manual
  migration ceremony on deploy.
- **Publishing** (all tag/dispatch-triggered GitHub workflows):
  - Python SDK: bump `sdks/python/pyproject.toml` version, tag `python-v*`.
  - TS SDK: bump `sdks/typescript/package.json` version, tag `ts-sdk-v*`.
  - CLI: bump `cli/package.json` version, then GitHub release publish or
    `gh workflow run "Publish CLI" --ref main`.
  - MCP: hosted-only — `publish-mcp-http.yml` pushes the HTTP image on tag;
    npm publish is retired.
