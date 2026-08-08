# e2a CLI

A thin developer convenience for [e2a](https://e2a.dev) — email for AI agents.

The CLI is two things in one: a developer convenience (**browser login** and
**real-time inbound streaming**, with a local forward proxy for testing webhook
handlers) and the **scripting surface** — `whoami`/`send`/`reply`/`messages` are
stateless primitives for shell-based harnesses (skills, hooks, CI), with a
documented, frozen exit-code contract (see [Exit codes](#exit-codes) below) so
scripts can branch on the process exit status instead of parsing JSON. For
interactive, stateful agent work (an MCP client or a long-running process),
use the **MCP tools** or the **SDK** (`@e2a/sdk`, `e2a`) instead.

| Task | Use |
|---|---|
| Script a send/reply/read from a shell harness, with exit codes | the **CLI** (`send`, `reply`, `messages`, `whoami`) |
| Drive an agent interactively (MCP client, long-running process) | the **MCP tools** or the **SDK** (`@e2a/sdk`, `e2a`) |
| Manage domains, webhooks, HITL review queues | the **web dashboard**, MCP tools, or SDK |
| React to inbound mail in production | **webhooks** (public URL) or `client.listen()` (SDK) |

## Install

```bash
npm install -g @e2a/cli
# or, without installing:
npx @e2a/cli login
```

## Commands

### `e2a login`

Open a browser login and save an account-scoped API key to `~/.e2a/config.json`
(also caches the deployment's shared mail domain, discovered from `GET /v1/info`).

```bash
e2a login
```

On a headless machine, set `E2A_API_KEY` instead of running `login`. To persist
that key locally, use `e2a config set api_key <key>`; `e2a whoami` validates it.

Login does **not** set a default sending inbox. The key is account-scoped — it
spans every inbox on the account — so the CLI never guesses which one you meant.
Commands that send or read mail take `--agent <email>`; to avoid passing it every
time, set a default explicitly:

```bash
e2a agents list
e2a config set agent_email bot@acme.com   # or export E2A_AGENT_EMAIL
```

Without one, those commands exit `2` (usage) rather than picking an inbox for you.
A default you set this way survives re-login.

Need a least-privilege key bound to a single inbox? Mint one after logging in
(the agent must already exist — create it first with `e2a agents create`,
below):

```bash
e2a keys create --agent bot@acme.com
```

### `e2a whoami`

Show the key identity: user, scope, bound agent, plan.

```bash
e2a whoami
e2a whoami --json
```

### `e2a doctor`

Read-only diagnostics for the production email path. Doctor never sends mail
and never mutates anything — no DNS changes, no webhook test deliveries, no
domain re-verification — so it is safe to run against production at any time
(including from CI and cron). Every network operation is bounded by a
5-second timeout.

```bash
e2a doctor                     # human pass/warn/fail report
e2a doctor --json              # versioned machine-readable report (e2a.doctor/v1)
e2a doctor --agent bot@acme.com --domain acme.com
```

Checks, where applicable: CLI config and credential scope; API connectivity
and server version (`GET /v1/info`); agent existence and access; for each
registered custom domain, **live DNS lookups** of the server-prescribed
records — ownership TXT, inbound MX, DKIM TXT, MAIL FROM MX, SPF TXT — plus
an advisory DMARC check (warn-only; e2a does not prescribe a DMARC record)
and the SES sending status; MCP endpoint reachability and advertised OAuth
metadata; webhook configuration and recent delivery history; and outbound
SMTP configuration visibility. Webhook *reachability* is reported as an
explicit skip: the server's webhook test endpoint delivers a real event, so
no safe non-delivering probe exists — recent delivery history is the
observed signal instead.

**Hosted (e2a.dev):**

```bash
e2a doctor --agent bot@acme.com
# doctor: read-only diagnostics for https://e2a.dev (sends no mail; changes no DNS or webhooks)
#
# pass  cli.config            api key from ~/.e2a/config.json; deployment https://e2a.dev
# pass  api.reachability      server version 1.0.0
# pass  api.auth              key valid — scope account, plan scale
# pass  agent.access          agent exists
# pass  domain.mx             acme.com: MX record found (mx.e2a.dev)
# warn  domain.dmarc          acme.com: no DMARC record
#       fix: add TXT record _dmarc.acme.com with value "v=DMARC1; p=none;" …
# pass  mcp.reachability      endpoint responded (HTTP 405)
# …
# 0 fail, 1 warn, 14 pass, 2 skip — warnings (exit 8)
```

The hosted MCP endpoint (`https://api.e2a.dev/mcp`) is probed automatically
when the deployment root is `https://e2a.dev`.

**Self-hosted:**

```bash
E2A_URL=https://mail.internal.example e2a doctor \
  --mcp-url https://mail.internal.example/mcp
```

Self-hosted deployments skip the MCP checks unless `--mcp-url` names the MCP
endpoint. Run doctor **on the server host** to also surface the outbound
SMTP configuration (`smtp.config` reads `E2A_OUTBOUND_SMTP_HOST`, `_PORT`,
and `_FROM_DOMAIN` from the environment; credentials are reported only as
set/not set and never printed).

With `--json`, doctor emits a stable, versioned report (`schema:
"e2a.doctor/v1"`): top-level `status` (`healthy`/`warnings`/`failed`),
`exit_code`, a `summary` count, and one entry per check with `id`, `status`
(`pass`/`warn`/`fail`/`skip`), `reason_code`, optional `target`, a human
`detail` line, structured `evidence`, and a `remediation` when something
needs fixing. New check IDs and reason codes may be added over time; existing
ones are never renamed.

Doctor's exit code separates the failure classes so scripts and CI can
branch without parsing the report: `0` healthy, `8` warnings only, `9` a
definite configuration failure (missing DNS record, unregistered domain,
auto-disabled webhook — retrying cannot fix it), `4` bad or rejected
credentials, `1` transient connectivity failure (retry may help), `2` usage
error.

### `e2a agents`

Manage inboxes (requires an account-scoped key).

```bash
e2a agents list
e2a agents create bot@acme.com --name "Support bot"
e2a agents get bot@acme.com
```

`list`, `create`, and `get` all accept `--json` (print the raw JSON response).

Creating an address on a custom domain (`bot@acme.com`) requires the domain to
be registered and verified on your account first (web dashboard, MCP tools, or
SDK — the CLI has no `domains` command). The deployment's shared domain needs
no setup, and a bare name expands onto it (`e2a agents create mybot` →
`mybot@<shared-domain>`). Throughout this README, `bot@acme.com` stands for
any agent you've created.

### `e2a keys`

Mint, list, and revoke API keys (requires an account-scoped key).

```bash
e2a keys create --agent bot@acme.com --name "prod key"   # bound, least-privilege
                                                            # (plaintext printed once)
e2a keys list
e2a keys delete <key-id>
```

`create` and `list` accept `--json` (print the raw JSON response).

### `e2a protection`

Show or update an agent's protection (screening/review) config.

```bash
e2a protection get bot@acme.com
e2a protection set bot@acme.com --outbound-review off   # sends go out unheld
e2a protection set bot@acme.com --inbound-review off     # inbound delivered unheld
e2a protection set bot@acme.com --suppress-notifications on
```

`get` and `set` accept `--json` (print the raw JSON response).

### `e2a send` / `e2a reply`

Send an email as the agent, or reply in-thread. Together with `whoami` and
`messages`, these are the stateless scripting primitives — see
[Exit codes](#exit-codes).

```bash
e2a send --to alice@example.com --subject "Hi" --body "Plain-text body." \
  --agent bot@acme.com
e2a send --to alice@example.com --subject "Hi" --html-file body.html \
  --attach report.pdf --conversation-id conv_123 --idempotency-key <uuid>
e2a send --to alice@example.com --subject "Tomorrow" --body "Later." \
  --send-at "<future-rfc3339>"
e2a reply msg_abc123 --body "On it." --agent bot@acme.com
```

Common `send`/`reply` flags: `--body` / `--body-file`, `--html-file` (text
fallback derived if no `--body`), `--attach` (repeatable; max 10 files, 10 MB
each, 25 MB total), `--reply-to` (repeatable; max 5 addresses), `--send-at`
(RFC 3339 with an explicit UTC offset, at most 90 days ahead),
`--idempotency-key`, `--agent`, `--json`
(print the full send result). `send`-only: `--to` (repeatable), `--subject`,
`--conversation-id` (alias `--conversation`) — `reply` infers these from the
message being replied to and rejects them as unknown flags.

`--conversation-id` sets caller-owned application correlation; it does not
place a fresh send into an existing RFC email thread. Use `reply` with the
original message ID to preserve `In-Reply-To` / `References`.

Scheduled sending via `--send-at` is **beta and may change before it is
declared stable**.
A future schedule exits `0` with `status=scheduled`; it is durably queued, so
do not retry. Direct self-send cannot be scheduled and returns a permanent
request error unless a review hold takes precedence (held sends drop the
schedule). Trashing the message before provider submission starts prevents
submission (an in-flight submission returns `409 send_in_progress`); restoring it before the
send time re-arms it, while restoring at or after that time restores the
message but leaves the send canceled.

### `e2a messages`

List or fetch messages for an agent.

```bash
e2a messages list --agent bot@acme.com --direction inbound --read-status unread
e2a messages list --agent bot@acme.com --since 2026-07-01T00:00:00Z --json
e2a messages get msg_abc123 --agent bot@acme.com --text
e2a messages lifecycle msg_abc123 --agent bot@acme.com --json   # beta
```

`list` flags: `--direction` (`inbound`/`outbound`/`all`), `--since` (inclusive
ISO timestamp), `--conversation` (alias `--conversation-id`), `--read-status`
(`unread`/`read`/`all`, default `all`), `--limit`, `--agent`, `--json` (NDJSON
instead of TSV). `get` flags: `--text` (print parsed body text only),
`--agent`, `--json` (print the full message as JSON). `lifecycle` (beta) shows
a message's observed lifecycle transitions; flags: `--cursor` (continue from a
prior page), `--limit` (page size, 1–100), `--agent`, `--json` (print the
canonical lifecycle page as JSON).

The conversation filter selects the existing caller-owned
`conversation_id`, not email topology. On servers that expose it, SDK-shaped
JSON from message list/get/listen may include optional beta `threadId`,
a server-owned, read-only mailbox-local identity. Human-readable formats do
not change, and there is no `threadId` request flag, filter, or thread
endpoint.

### `e2a contacts` (beta)

Manage account-level contact identity and per-agent outreach state, with
suppression visibility. Contact identity operations require account scope;
`outreach` also supports an agent-scoped credential for its bound inbox.
The whole contacts surface is **beta and may change before it is declared
stable** — it tracks the beta `/v1/contacts` and `/v1/agents/{email}/contacts`
API resources.

```bash
e2a contacts list --source import --limit 50 --json
e2a contacts get alice@example.com
e2a contacts create alice@example.com --idempotency-key <uuid>
e2a contacts update alice@example.com --metadata '{"tier":"gold"}' --if-match <etag>
e2a contacts delete alice@example.com
e2a contacts import contacts.csv --agent bot@acme.com --stage new --on-conflict merge
e2a contacts imports delete <import-batch-id>
e2a contacts outreach list --agent bot@acme.com --stage new --replied false
e2a contacts outreach get alice@example.com --agent bot@acme.com
e2a contacts outreach set alice@example.com --agent bot@acme.com --stage replied --next-action clear
e2a contacts outreach delete alice@example.com --agent bot@acme.com
```

`contacts delete` removes the account contact and all of its per-agent outreach
rows; suppression and consent records survive.

`list`/`outreach list` flags: `--source` (`import`/`manual`/`inbound`),
`--import-batch`, `--created-after`/`--created-before` (list) or
`--stage`/`--replied`/`--suppressed`/`--next-action-before`/
`--last-outbound-before` (outreach list), `--limit`, `--json` (NDJSON instead
of TSV). `create`/`import` accept `--idempotency-key` to safely replay a
timed-out request. `update`/`outreach set` accept `--if-match <etag>` to
reject a stale edit. `import` reads an RFC 4180 CSV (`--email-column`,
`--name-column`, `--on-conflict merge|skip`, `--dry-run` to preview without
writing); `imports delete` reverses one import batch.

### `e2a suppressions`

Inspect and manage recipient block lists. Without `--agent`, commands target
the ACCOUNT-wide list — auto-populated by hard bounces and complaints and
enforced for every inbox (sends fail with `recipient_suppressed`). With
`--agent`, they target that inbox's beta unsubscribe/manual blocks. All
commands require account scope.

```bash
e2a suppressions list                                   # account-wide list
e2a suppressions list --agent bot@acme.com --json       # one inbox's blocks
e2a suppressions add alice@example.com --agent bot@acme.com --reason "asked us to stop"
e2a suppressions remove alice@example.com               # account-wide
e2a suppressions remove alice@example.com --agent bot@acme.com
```

`add` requires `--agent` — manual blocks are per-agent; account entries only
come from bounces/complaints. `remove` without `--agent` un-suppresses
account-wide: do that only for addresses known to be deliverable, since
removing a genuine bouncer hurts sender reputation. `list` supports `--limit`
and `--json` (NDJSON instead of TSV: address, source, reason, created-at).

### `e2a listen`

Stream inbound email for an agent over WebSocket in real time. The connection is
outbound, so it works from behind NAT — the simplest way for a **local** agent to
get push delivery without a public webhook URL.

```bash
e2a listen --agent bot@acme.com
# [10:30:15] Claimed From: alice@example.com | DMARC: pass (verified domain: example.com) | Subject: Meeting tomorrow

# --forward bridges each message to a local HTTP handler (the
# `stripe listen --forward-to` pattern) — ideal for developing a webhook
# handler locally without exposing a public URL. Each message is POSTed as
# the full v1 MessageView JSON (SDK camelCase: headerFrom, authentication, …):
e2a listen --agent bot@acme.com --forward http://localhost:3000/inbound

# --forward-token adds an `Authorization: Bearer <token>` header to the POST:
e2a listen --agent bot@acme.com --forward http://localhost:3000/inbound --forward-token <secret>

# Emit the full message as JSON (one object per line) for piping:
e2a listen --agent bot@acme.com --json

# Only messages with one caller-owned application conversation ID:
e2a listen --agent bot@acme.com --conversation conv_123

# Exit after the first (matching) message, or TIMEOUT (exit 6) if none arrives
# by the deadline — useful for a script waiting on one reply:
e2a listen --agent bot@acme.com --once --until 2026-07-18T13:00:00Z --text
```

`--agent` falls back to the `agent_email` saved in config.

Note: `listen --once --text` / `--json` fetches the message via the API GET,
which marks it as read (same side effect as `messages get`).

The server keeps **one WebSocket connection per agent**. If another listener
for the same agent connects (a second `e2a listen`, or an SDK
`client.listen()` elsewhere), this one is superseded: it prints a
`listener replaced` explanation and exits `5` instead of reconnecting —
auto-reconnecting would steal the socket back from the newer listener and
loop.

`listen` also participates in the exit-code contract below: a long-running
listen (no `--once`) exits `1` whenever the stream actually ends, such as
after a peer's normal WebSocket close (code 1000). Deploy drains use close code
1001 and reconnect with backoff, so they do not end the stream. A supervisor
(`systemd Restart=on-failure`, a retry loop) should treat exit `1` as
"restart me," not "stopped on purpose." Under `--once`, a forward that never
reaches the `--forward` endpoint also exits `1` even though the message itself
was printed to stdout — the message was consumed off the stream, so a silent
exit `0` would read as a successful hand-off to a harness when it wasn't.

#### OpenAI Responses auto-reply

When the `--forward <url>` path ends in `/v1/responses`, `listen` switches to
**auto-reply mode**: it formats each inbound email as an OpenAI
[Responses API](https://platform.openai.com/docs/api-reference/responses) request,
POSTs it, and sends the model's output text back as a reply in the thread. Use
`--forward-token` for the model endpoint's bearer token.

```bash
e2a listen --agent bot@acme.com \
  --forward http://localhost:18789/v1/responses \
  --forward-token <token>
```

### `e2a config`

View or update the local config (`~/.e2a/config.json`).

```bash
e2a config list
e2a config get agent_email
e2a config set agent_email bot@acme.com
```

Only `api_key` and `agent_email` are user-settable. Deployment URL, shared
domain, and cached key scope are managed by login or environment variables.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `E2A_API_KEY` | — | API key. Skips `e2a login` — useful in CI and scripts |
| `E2A_URL` | `https://e2a.dev` | The e2a deployment root. Set for self-host |
| `E2A_AGENT_EMAIL` | — | Default sending/listening inbox (what `--agent` overrides) |
| `E2A_SHARED_DOMAIN` | auto-discovered | Force the shared domain instead of discovering it via `GET /v1/info` |

**Precedence:** command-line flags beat environment variables, which beat
`~/.e2a/config.json`, which beats the defaults above.

`E2A_URL` is the deployment root — the host that serves the `e2a login` browser
flow and `/get-started`, and proxies the `/v1` API. It is *not* the SDKs'
`E2A_API_URL`, which names the API host alone; pointing the CLI at an API host
breaks `e2a login`. The CLI does not read `E2A_API_URL` or the SDKs' older
`E2A_BASE_URL`.

Environment variables take precedence over stored `api_key` and `agent_email`
values until they are unset. Deployment URL and shared-domain overrides are
environment-only (`E2A_URL` and `E2A_SHARED_DOMAIN`).

## Options

- `--help`, `-h` — show help
- `--version`, `-v` — show version

## Exit codes

`whoami`, `doctor`, `send`, `reply`, `messages`, and `listen` publish a stable, frozen
exit-code contract (`cli/src/exit.ts`) so shell harnesses can branch on the
process exit status instead of parsing JSON. Codes are never renumbered —
only added to.

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Transient error (network / 5xx / rate limit) — retry may help |
| `2` | Usage error (bad flags or arguments) |
| `3` | Send held for review (`pending_review`) — HTTP-successful but not delivered |
| `4` | Bad credentials or wrong key scope |
| `5` | Permanent request error (not found / invalid / conflict) — do not retry |
| `6` | Bounded wait (`listen --once --until`) expired with no matching message |
| `7` | A persisted send failed or returned an unrecognized outcome — do not retry; inspect the returned message id |
| `8` | Diagnostics (`doctor`) completed with warnings only — nothing broken |
| `9` | Diagnostics (`doctor`) found a definite configuration failure — do not retry; fix the reported configuration |
