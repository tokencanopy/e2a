# Contributing to e2a

Thanks for considering a contribution. e2a is a polyglot monorepo
(Go backend + TypeScript CLI/SDK + Python SDK + Next.js dashboard +
MCP server) and the bar for a friction-free first PR is mostly about
getting a working local environment.

This document covers:

- [Fork and pull request workflow](#fork-and-pull-request-workflow)
- [Prerequisites](#prerequisites)
- [First run](#first-run)
- [Project layout](#project-layout)
- [Running tests](#running-tests)
- [Making changes that touch the API](#making-changes-that-touch-the-api)
- [Database migrations](#database-migrations)
- [Commit + PR conventions](#commit--pr-conventions)
- [Where to ask](#where-to-ask)

For deeper architecture detail and command reference, see
[CLAUDE.md](./CLAUDE.md) (it doubles as Claude Code agent context but
the architecture and commands sections are useful regardless of how
you read it).

---

## Fork and pull request workflow

e2a is a public repository, so **you don't need to be granted access to
contribute** — you don't push to `tokencanopy/e2a` directly. Instead you
work on your own fork and open a pull request. If you cloned the
upstream repo and got a `remote: Permission to tokencanopy/e2a.git denied`
(or `403`) error on `git push`, that's the "no access" symptom — the
fix is to push to your fork, not upstream.

```bash
# 1. Fork on GitHub: click "Fork" at https://github.com/tokencanopy/e2a
#    (creates https://github.com/<you>/e2a).

# 2. Clone YOUR fork (not the upstream repo):
git clone https://github.com/<you>/e2a.git
cd e2a

# 3. Add upstream so you can pull in new changes later:
git remote add upstream https://github.com/tokencanopy/e2a.git

# 4. Branch, commit, and push to YOUR fork (origin):
git checkout -b my-feature
# ...make changes...
git commit -am "feat(scope): my change"
git push origin my-feature

# 5. Open a PR from your fork's branch → tokencanopy/e2a:main.
#    GitHub shows a "Compare & pull request" button after you push.
```

Keep your fork current before starting new work:

```bash
git fetch upstream
git checkout main
git merge upstream/main    # or: git rebase upstream/main
git push origin main
```

CI (including all test tiers) runs automatically on pull requests from
forks — you don't need any special access for checks to run. A
maintainer reviews and merges; you never need write access to the
upstream repo.

**Want to be added as a direct collaborator instead?** That's only for
trusted, ongoing contributors and is at the maintainers' discretion —
ask in a [discussion](https://github.com/tokencanopy/e2a/discussions).
The fork workflow above is the expected path for everyone else.

---

## Prerequisites

| Tool | Version | Why |
|---|---|---|
| Go | 1.25 | backend |
| Node | 18+ (CI tests on 22) | CLI, TypeScript SDK, MCP server, web dashboard |
| Python | 3.9+ | Python SDK |
| Docker (Desktop or engine) | any recent | local Postgres + Mailpit |

You don't need all four to contribute — touching only the TS SDK
doesn't require Python, etc. But you do need Docker for any change
that runs the backend.

---

## First run

The fastest path to a running backend with working outbound mail:

```bash
# 1. clone + bootstrap configs
#    (clone your fork — see "Fork and pull request workflow" above)
git clone https://github.com/<you>/e2a.git
cd e2a
cp config.example.yaml config.yaml

# 2. start Postgres (:5433) + Mailpit (:1025 SMTP, :8025 UI) only
#    (plain `make docker-up` also starts the dockerized API on :8080/:2525,
#    which conflicts with the host binary started in step 4)
docker compose up -d postgres mailpit

# 3. point outbound at Mailpit so HITL approvals + test-email work
#    Edit config.yaml under `outbound_smtp:`:
#      host: "localhost"
#      port: 1025
#      from_domain: "e2a.localhost"
#    (Captured outbound mail will appear at http://localhost:8025.)

# 4. build + run the backend
make run
```

The first run auto-applies all migrations against the local Postgres
(`E2A_MIGRATION_MODE=auto` is the default). The API is now at
`http://localhost:8080`; the SMTP relay is on `:2525`.

### Bootstrap your first user

The dashboard uses Google OAuth, which is more setup than most
contributors want for a first run. The bootstrap CLI flag creates a
user + API key in one step without an OAuth handshake:

```bash
./bin/e2a -config config.yaml -bootstrap-email you@example.com
```

The command prints the plaintext API key. Save it — you can't recover
it later (only the hash is stored).

### Verify the install

```bash
curl http://localhost:8080/api/health
# → {"status":"ok"}

curl -H "Authorization: Bearer <your-key>" http://localhost:8080/v1/agents
# → {"items":[],"next_cursor":null}
```

### Optional: web dashboard

If you want the Next.js dashboard too:

```bash
cd web
npm ci
npm run dev    # http://localhost:3000, proxies /api/* to :8080
```

Google OAuth credentials are required for sign-in — set
`oauth.google_client_id` and `oauth.google_client_secret` in `config.yaml`
(or export the `E2A_GOOGLE_CLIENT_ID` / `E2A_GOOGLE_CLIENT_SECRET` env
overrides).
For API-only contributions you can skip the dashboard.

---

## Project layout

| Path | What's there |
|---|---|
| `cmd/e2a/` | Backend binary entry point |
| `internal/relay/` | SMTP server, inbound mail |
| `internal/emailauth/` | SPF / DKIM / DMARC evaluation and alignment |
| `internal/agent/` | Agent CRUD, HITL, REST API |
| `internal/identity/` | Domain ownership, message store |
| `internal/webhook/`, `internal/webhookdelivery/`, `internal/webhookpub/` | Webhook subscriptions, durable fan-out + delivery outbox |
| `internal/ws/` | WebSocket hub for real-time push |
| `internal/outbound/`, `internal/outboundsend/` | Compose + send via upstream SMTP; queue-first send worker |
| `internal/inboundprocess/`, `internal/inboundpolicy/` | Async inbound processing worker + screening/policy |
| `internal/hitlworker/`, `internal/hitlnotify/` | HITL review holds + approval notifications |
| `internal/jobs/`, `internal/janitor/` | River durable-job runtime; periodic TTL sweeps |
| `internal/idempotency/` | `Idempotency-Key` storage + replay |
| `internal/usage/`, `internal/limits/` | Usage metering + plan/account entitlement limits |
| `internal/auth/` | API key + user auth |
| `internal/config/` | YAML config + env var overrides |
| `cli/` | `e2a` CLI (TypeScript) |
| `sdks/typescript/` | `@e2a/sdk` |
| `sdks/python/` | Python `e2a` package |
| `mcp/` | `@e2a/mcp-server` Model Context Protocol surface |
| `web/` | Next.js 16 dashboard |
| `migrations/` | SQL migrations, embedded into the binary |
| `docs/` | API reference, deployment, design docs |

`CLAUDE.md` has a deeper write-up of the inbound flow and the SDK
type-generation pipeline.

---

## Running tests

The Go suite is split into three tiers because integration tests are
slow and need a database.

```bash
make test-unit          # no DB needed — fast
make test-integration   # needs Postgres on :5433 (make docker-up)
make test-e2e           # discovers and runs every package with integration-tagged tests
make test               # all three, -p 4 parallel (per-package test DBs)
make cover-check        # tests + per-package coverage floors (.testcoverage.yml)
```

Run a single Go test against the local DB:

```bash
E2A_TEST_DATABASE_URL="postgres://e2a:e2a@localhost:5433/e2a_test?sslmode=disable" \
  go test ./internal/identity/ -run TestApproveAndSendHappyPath -v
```

Other surfaces:

```bash
# TypeScript SDK
npm test --workspace @e2a/sdk
npm run test:contract --workspace @e2a/sdk    # needs live server

# CLI
npm test --workspace @e2a/cli

# MCP server
npm test --workspace @e2a/mcp-server

# Python SDK
cd sdks/python && pip install -e ".[dev]" && pytest tests/ -v

# Web dashboard (Jest)
cd web && npm test
```

If you change the SMTP relay or anything that interacts with the
database, prefer adding an integration test in
`internal/relay/`, `internal/agent/`, or `internal/e2e/` — unit tests
alone don't catch schema drift.

---

## Making changes that touch the API

e2a maintains parity across **seven client surfaces** for every API
change: the Go handler, OpenAPI spec, TypeScript SDK (raw + high-level),
Python SDK (raw + hand-written `AsyncE2AClient`), CLI, MCP server, and the
web dashboard. Each surface has its own tests. The Python sync client
(`sync_client.py`) is a background-thread facade over `AsyncE2AClient` that
mirrors its resources dynamically via `__getattr__`, so it needs no separate
parity work — only `AsyncE2AClient` is hand-edited.

The `/v1` OpenAPI document at `api/openapi.yaml` is emitted directly
from the live Huma handlers, and generated SDK code lives in
`sdks/{typescript,python}/.../generated/`. After changing a `/v1`
handler:

```bash
make spec           # regenerates api/openapi.yaml from the live handlers
make generate-sdk   # regenerates generated/ for TS + Python from the spec
make generate       # both of the above
```

Commit `api/openapi.yaml` and the regenerated `sdks/**/generated/`
bases. CI gates this with `spec-check` (the committed spec must
byte-equal what the handlers emit) and `generate-sdk-check` — run
`make generate` and commit the diff or the PR fails. The PR template
([`.github/pull_request_template.md`](.github/pull_request_template.md))
includes a checkbox list of every client surface; tick what's in scope
and explain anything intentionally skipped.

---

## Database migrations

Migrations live in `migrations/*.sql` and are **embedded into the
binary** (`migrations/embed.go`). On startup the binary applies any
pending migrations against a `schema_migrations` tracker table.

**Rules every migration must follow:**

1. **Idempotent** — `CREATE TABLE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`,
   `CREATE INDEX IF NOT EXISTS`. Migrations have to be re-runnable.
2. **Non-destructive on prod-sized tables** — `ADD COLUMN` is safe;
   `ALTER COLUMN TYPE` can rewrite the entire table and lock it. Avoid
   on `messages` and `usage_events` especially.
3. **Numbered sequentially** — `0NN_short_description.sql`. Don't
   renumber existing files.
4. **Forward-only** — there are no down migrations. If you need to
   undo something, write a new migration that does it.
5. **Declare binary rollback compatibility** — `ROLLBACK_UNSAFE_BEFORE` is
   embedded as `org.e2a.rollback-unsafe-before` in release images. Leave it at
   `none` only while every rollback-eligible retained production binary can
   still run safely after every embedded migration applies. Hosted S1 enforces
   that claim by booting those exact image digests against its migrated prod
   clone. If a migration crosses that boundary, set the first affected release
   semver and carry it forward unchanged; CI scans the reachable file history
   and rejects any later reset or move. Tag and manual image publishing are
   also restricted to commits already contained in `main`.

Add or update tests in any Go package that writes raw SQL against the
touched table. Higher-level e2e coverage doesn't catch query drift
because the migration runner is idempotent — runtime SQL will compile
and silently misbehave.

Local control:

```bash
make migrate        # apply pending migrations manually
# E2A_MIGRATION_MODE=auto    (default) — apply on startup
# E2A_MIGRATION_MODE=verify  — refuse to start if any are pending
# E2A_MIGRATION_MODE=skip    — emergency surgery only
```

---

## Commit + PR conventions

**Commit messages** follow a `type(scope): subject` shape with a
short imperative subject. The format is not formally
Conventional-Commits (no `!:` breaking markers, no `BREAKING CHANGE`
footers), just a consistent convention enforced by review.

```
type(scope): short imperative summary (≤ 72 chars)

Optional body paragraphs explaining *why*, not what (the diff is the
what). Wrap at 72 chars. Reference issues with #123. End with a
Co-Authored-By footer for AI co-author when applicable.
```

Types observed in the repo: `feat`, `fix`, `chore`, `docs`, `test`,
`refactor`, `deps`, `release`. Scopes are package- or surface-shaped
(e.g. `feat(api)`, `fix(web)`, `chore(retention)`, `test(e2e-prod)`).
Skim `git log --oneline -30` before opening your first PR — that
beats any written rule.

**PRs** use the template at [`.github/pull_request_template.md`](.github/pull_request_template.md).
Keep the description tight; the checklist is the load-bearing part.

**One PR per feature.** Don't bundle unrelated cleanup — that makes
review harder and rollback risky. A small refactor that genuinely
enables the feature is fine; a drive-by typo fix should be its own PR.

**CI must be green.** All CI test jobs run on every PR. If a
flake hits you, re-run the job rather than disabling the test.

---

## Where to ask

- **Bug reports** — open a [GitHub issue](https://github.com/tokencanopy/e2a/issues)
- **Feature ideas / design discussion** — open a [GitHub discussion](https://github.com/tokencanopy/e2a/discussions)
- **Security issues** — see [SECURITY.md](./SECURITY.md). Don't open a
  public issue for security reports.

A good bug report includes: the e2a version (the git SHA or release
tag), the exact command, the relevant log lines, and what you
expected vs. what happened. For SMTP / inbound issues, the
`[mail:<id>]` log line is the most useful thing to paste.
