# e2a-sdk-monitor

Continuously exercises **five published client surfaces** against production:
the raw HTTP API, the Python SDK, the MCP server, the TypeScript SDK, and the
CLI.

## Why this exists

The two existing continuous validators never touch a published client.
`cmd/e2a-prober` speaks raw HTTP; `tests/e2e-prod` deliberately uses
zero-dependency `fetch` ("no SDK" is a comment in the harness). So client
regressions — including a failed or forgotten publish — were invisible to CI.
That was not theoretical: the SDKs sat at 4.0.1 on PyPI/npm for weeks while
`main` was at 5.2.0, and a downstream integration was broken against every
fresh install in that window with nothing to catch it.

This service closes that gap across every interface a user actually reaches
for, each with its own success signal so a break in one pages distinctly from
a break in another. It lives beside `tests/e2e-prod` because it is the same
kind of thing — a production-targeting harness that is not part of local dev —
rather than under `cmd/`, which is Go binaries only.

## What it exercises

The same canonical agent-to-agent round trip, run once per tick through each
of five interfaces:

| iface key | send + reply performed via | runtime |
| --- | --- | --- |
| `api` | raw HTTP against the server's `/v1/agents/{email}/messages` (and `.../reply`) endpoints — no SDK. Catches a server-contract break distinct from an SDK break. | python (stdlib `urllib`) |
| `python_sdk` | the published `e2a` PyPI package | python |
| `mcp` | the deployed MCP streamable-HTTP server's `send_message` / `reply_to_message` tools | python (hand-rolled JSON-RPC over `urllib`) |
| `ts_sdk` | the published `@e2a/sdk` npm package (v5.4.0) | node, shelled out from python |
| `cli` | the published `@e2a/cli` npm package (v2.1.0, bin `e2a`) | node, shelled out from python |

Anything that breaks any of those — a bad publish, a wire-contract drift, a
packaging regression, an MCP transport change — stops that interface's
success log line, without silencing the other four.

Each round trip also **cleans up after itself through the same interface**:
after the reply leg lands, the webhook hub trashes the probe (from B's
inbox) and the reply (from A's inbox) via the interface named in the nonce —
so four interfaces' `delete` paths (REST `DELETE …/messages/{id}`, the
Python SDK's `messages.delete`, the TS SDK's `messages.delete`, and the MCP
`delete_message` tool) are exercised every tick, and probe residue doesn't
accumulate in the two inboxes forever. The published **CLI has no delete
command**, so the `cli` interface's cleanup is a documented `monitor_skip`
(`stage=cleanup`), never a substitution through another surface. Cleanup is
deliberately decoupled from the success signal: it runs *after* the leg's
`monitor_replied`/`monitor_ok` line, and a delete failure is a
`monitor_error(stage=cleanup)` — never a 5xx that would redeliver the
webhook and double-count the leg. Deletion is the soft (trash) path; the
*sent* copies in the opposite folders are out of scope (the service is
stateless — the send-side ids would have to ride in the nonce).

### Handlers

- **`POST /tick`** (Cloud Scheduler, ~5 min). Fires ONE outbound leg PER
  interface: mints a nonce that encodes the interface, sends
  `probe <nonce>` from agent A to agent B via that interface, and returns a
  per-interface summary. A single interface's send failure is logged
  (`monitor_error`, `stage=send`) and does **not** abort the other four. The
  response's `ok` boolean and HTTP status are computed over **considered**
  interfaces only — one skipped this tick (currently only `mcp` absent
  `E2A_MONITOR_MCP_URL`) doesn't count against the aggregate either way:
  `200` when every considered interface succeeded, `207` when some but not
  all did (partial failure), `500` when every considered interface failed
  (total outage — a non-2xx so Cloud Scheduler doesn't read a full send-side
  blackout as healthy). The per-interface `results` body always carries the
  detail regardless of the aggregate.
- **`POST /webhook`** (e2a delivers here — a real public HTTPS URL). This
  handler is the correlation hub and is itself the service's own
  infrastructure — verification and hydration always go through the
  published Python SDK's `construct_event` / `inbound.from_event`,
  regardless of which interface the nonce names. It ignores non-
  `email.received` types, then branches on which agent received the mail:
  delivered to **B** → reply **via the interface named in the nonce** (so
  each interface's reply/threading path is exercised too, not just send),
  then trash the probe from B's inbox via that same interface; delivered to
  **A** → that's the reply coming home, emit the success line with that
  interface's key, then trash the reply from A's inbox (same interface
  again). Both deletes are best-effort cleanup — see "What it exercises".
- **`GET /health`** — liveness.

The service is **stateless** across requests. Cloud Run scales to zero, so
nothing may live in memory between requests except the five interface
strategy objects built once at import time from static config (API key, base
URL, MCP URL) — never per-round-trip state. All correlation state (which
round trip is in flight, when it started, which interface owns it) travels in
the message subject; the send timestamp embedded in it yields latency for
free.

`http.server` from the stdlib is deliberate for the Python side — traffic is
roughly one request per five minutes plus the webhook, and a framework would
add dependencies that dilute the "do these clients install and work" signal
this image exists to produce. The Node interfaces are invoked as short-lived
subprocesses rather than embedding a JS runtime elsewhere in the request path.

### Correlation, interface encoding, and stale replies

The nonce is `e2asdkmon.<iface>.<epoch_ms>.<16 hex>.<16 hex tag>`, where
`<iface>` is one of `api`, `python_sdk`, `mcp`, `ts_sdk`, `cli`. The iface
segment lets the webhook hub dispatch the reply leg to the exact interface
that sent the probe — a `ts_sdk` outbound leg gets a `ts_sdk` reply, never a
different interface silently substituting for it. `NONCE_RE` captures the
iface generically (not hardcoded to the five known keys); a nonce naming an
interface this build doesn't recognize is handled safely — logged
(`monitor_error`, `stage=unknown_iface`) and ignored, never crashed on and
never credited with a success. Older, pre-multi-interface or pre-MAC nonces
(no iface/tag segment) simply don't match; there is no deployed consumer of
that shape to stay compatible with.

The random suffix keeps concurrent probes distinct; the timestamp gives
latency without storage. The trailing `tag` is the first 16 hex chars of
`HMAC-SHA256(NONCE_KEY, "e2asdkmon-nonce:<iface>.<epoch_ms>.<16 hex>")` —
see "Nonce authentication" below; every leg of the round trip re-derives and
constant-time-compares it before acting. The leg is decided by `email.inbox`
(the delivered-to agent), not by a `Re:` prefix, since subjects get rewritten
in transit. None of the five interfaces sets a subject override on reply —
every wire shape (the REST `ReplyRequest`, both SDKs' `reply()`, the CLI's
`reply` subcommand, the MCP `reply_to_message` tool) derives it server-side
as `Re: <original subject>`, which still satisfies `NONCE_RE.search()` since
the nonce is a substring.

### Nonce authentication

Without a MAC, the nonce is entirely attacker-choosable: anyone able to
email agent A could pick any `iface`/timestamp/random suffix and forge a
`monitor_ok{iface=X}` for a chosen interface, with no binding to a real
`/tick`. `handle_webhook` closes this by re-deriving the expected tag from
the parsed `iface`/`epoch_ms`/16-hex segments and comparing it to the
received tag with `hmac.compare_digest` **before acting on either leg** — a
mismatch is logged (`monitor_error`, `stage=nonce_auth`, never the key or the
tag) and the request is treated as not-our-probe (`200`,
`{"ignored": "bad nonce mac"}`): no reply is sent, no `monitor_ok` is
emitted. This also authenticates `sent_ms` itself, so a forged nonce can't
look artificially fresh to defeat the `MAX_AGE_MS` staleness guard.

The signing key (`NONCE_KEY`) is `E2A_MONITOR_NONCE_SECRET` if set, else the
already-required `E2A_MONITOR_WEBHOOK_SECRET` — no new required config.
Reusing the webhook secret is safe: the fixed `e2asdkmon-nonce:` prefix in
the signed message domain-separates this MAC from the webhook's own HMAC
over the same value, so a forger who somehow obtained one signature could
still not derive the other.

**This is defense-in-depth, not the primary control.** The primary
mitigation is infrastructure, not code — see "One-time setup" below: create
agents A and B with an inbound allowlist naming only each other, so e2a
rejects a third party's mail to them at the source, before it ever reaches
this service. The signed nonce protects against the case where that
allowlist is misconfigured or absent.

A reply whose nonce timestamp is older than `E2A_MONITOR_MAX_AGE_MS` (default
15 min) logs `monitor_stale` and **not** `monitor_ok`, so a redelivered or
long-delayed message can never refresh the freshness alert — on both the
outbound leg (blocks a duplicate reply from firing) and the reply leg (blocks
a stale success). Replies carry `idempotency_key=sdkmon:<event id>` on every
interface so a webhook redelivery does not fan out into duplicate sends.

## Log markers

One JSON object per line on stdout. Build the alert on the success marker,
labeled by `iface`:

```json
{"event":"monitor_ok","iface":"ts_sdk","nonce":"e2asdkmon.ts_sdk.1753056000000.…","latency_ms":4231,"message_id":"msg_…"}
```

| `event` | Meaning |
| --- | --- |
| `monitor_ok` | Round trip completed for `iface`. **Alert on the absence of this, per iface.** `latency_ms` is the metric. |
| `monitor_tick` | Outbound leg sent for `iface`. |
| `monitor_replied` | Inbound leg received and replied to, via `iface`. |
| `monitor_deleted` | Cleanup done for `iface`: the leg's message was trashed via `iface` (carries `inbox` + `message_id`). |
| `monitor_stale` | A probe arrived past `MAX_AGE_MS` on `leg` (`outbound` or `reply`) for `iface`; not a success. |
| `monitor_sweep` | The per-tick hygiene sweep moved rows in `inbox`: `trashed` live messages, `purged` from the trash. Housekeeping, never a verdict on the stack. |
| `monitor_dropped` | A leg was abandoned rather than retried, because the failure is terminal — currently only `stage=hydrate` on a `not_found` (the message is gone, so redelivery can only 404 again). The iface falls silent and the absent-metric alert is the signal. |
| `monitor_skip` | `iface` was skipped this tick because its config is absent (`stage=send`, currently only `mcp` without `E2A_MONITOR_MCP_URL`), or its cleanup was skipped because the interface's published client has no delete verb (`stage=cleanup`, currently always `cli`). |
| `monitor_error` | Failure, with `stage` = `config` \| `send` \| `signature` \| `hydrate` \| `reply` \| `cleanup` \| `unknown_iface` \| `nonce_auth`, and (where applicable) `iface`. A `send`/`reply` failure includes a held (`pending_review`), terminal-`failed`, or unrecognized send-response `status` — see "Uniform send-status handling" below. A `cleanup` failure means the round trip succeeded (its `monitor_replied`/`monitor_ok` was already logged) but the trash step failed — hygiene, not an outage. |
| `monitor_start` | Process start; lists the configured `ifaces` and whether `mcp` is configured. |

Secret values are never logged — only env var *names* on a config failure. A
`nonce_auth` failure logs `iface` only, never the nonce-signing key or the
received/expected tag.

### Uniform send-status handling

`SendResultView.status` (`api/openapi.yaml`) is an open string set; only
`sent` and `accepted` are genuinely successful outcomes. Every interface —
`api`, `python_sdk`, `mcp`, `ts_sdk` (checked in `js/monitor-helper.mjs`, the
node helper it shells out to), and `cli` (via its own `emitSendResult`,
`cli/src/commands/send.ts`) — treats `pending_review` (held for review),
`failed` (terminal failure), and any status this build doesn't recognize as
an immediate failure: `monitor_error(stage=send` or `reply)` at send/reply
time, not a success logged now that only surfaces later as a
`monitor_stale` timeout once the reply never arrives.

### Hygiene sweep

Each tick ends by trashing and then purging up to `E2A_MONITOR_SWEEP_BUDGET`
messages per inbox — the two agents would otherwise keep every probe the
monitor ever sent (47k outbound rows per inbox on staging). Purge only accepts
a message already in the trash, so the order is a requirement, not a
preference.

**The sweep may never touch a message younger than `MAX_AGE_MS`.** The tick
returns as soon as the sends are away, but a round trip finishes later, on
`/webhook` — so the tick's own probes are still in flight when the sweep runs,
and the listing is newest-first unless asked otherwise. An unscoped sweep
therefore deletes the round trip it just started: the leg 404s on hydrate or
reply, the iface goes silent, and the alert fires for an outage the monitor
manufactured. `_sweep_ids` pins both `sort=asc` and an `until` cutoff for this
reason; `sort=asc` alone only postpones the collision until the backlog drains
below one page.

`MAX_AGE_MS` is exactly the right floor. A message's `created_at` is at or
after the `sent_ms` its nonce carries, and a leg only counts while
`now - sent_ms <= MAX_AGE_MS`, so anything past the cutoff can no longer
produce a `monitor_ok` — purging it cannot cost a success.

### Ops contract

Build a log-based metric on `monitor_ok`, labeled by `iface`. Alert per
interface — "no `monitor_ok{iface=X}` in N minutes" — for each of the five
`iface` values independently, so a regression in one client surface (say, a
broken `@e2a/cli` publish) pages distinctly from a break in another (say, the
raw API contract). This is intentionally built *after* this service, as a
separate ops step; the log line is the whole contract this service owes ops.

## Configuration

All via environment.

| Var | Required | Default | Notes |
| --- | --- | --- | --- |
| `E2A_API_KEY` | yes | — | Account-scoped key owning both agents; shared by every interface. |
| `E2A_MONITOR_AGENT_A` | yes | — | Sender; also receives the reply. |
| `E2A_MONITOR_AGENT_B` | yes | — | Receiver; sends the reply. |
| `E2A_MONITOR_WEBHOOK_SECRET` | yes | — | The `whsec_…` from the subscription. Also the nonce-signing key's fallback — see `E2A_MONITOR_NONCE_SECRET`. |
| `E2A_MONITOR_NONCE_SECRET` | no | falls back to `E2A_MONITOR_WEBHOOK_SECRET` | Dedicated key for the nonce HMAC tag (see "Nonce authentication" above). Optional so protection never depends on an unset var — set it only if you want to rotate the nonce key independently of the webhook secret. |
| `E2A_BASE_URL` | no | `https://api.e2a.dev` | API host, shared by `api`, `python_sdk`, `ts_sdk` (as `E2A_API_URL`), and `cli` (as `E2A_URL`). |
| `E2A_MONITOR_MCP_URL` | no | — | Full streamable-HTTP MCP endpoint (e.g. `https://api.e2a.dev/mcp`), matching the prober's `E2A_PROBE_MCP_URL` convention. **Optional**: when unset, the `mcp` interface is skipped every tick (`monitor_skip`) instead of failing the whole service — see "Design decisions to review" below. |
| `E2A_MONITOR_MAX_AGE_MS` | no | `900000` | Stale-reply cutoff. Also the floor on what the hygiene sweep may delete — see "Hygiene sweep" below. |
| `E2A_MONITOR_SWEEP_BUDGET` | no | `50` | Messages the hygiene sweep may touch per inbox per phase, per tick. Must stay within the endpoint's `limit` bounds (1–100): outside them the listing is a 422, which the sweep swallows as `monitor_error(stage=sweep)` and does nothing — a usable kill switch, but a deliberate one. |
| `PORT` | no | `8080` | Cloud Run injects this. |

`api`, `python_sdk`, `ts_sdk`, and `cli` are **core** interfaces: they need no
config beyond the vars already required at startup, so they run every tick.
`mcp` is the one **optional** interface — its endpoint may not be reachable
or fixed in every deployment, so an unset `E2A_MONITOR_MCP_URL` skips it
(mirrors the prober's convention for `E2A_PROBE_MCP_URL`) rather than failing
the whole service.

Missing REQUIRED vars exit 1 at startup so a misconfigured deploy fails
loudly (names only, never values). The webhook additionally fails closed at
request time: an unset secret or a bad signature is a 401, never an accepted
delivery.

## One-time setup (infrastructure, not application code)

The service does **not** create its own webhook subscription — subscriptions
are infrastructure, created once out of band:

1. Create the two agents (A and B) on the monitoring account, then set each
   one's inbound protection to an allowlist naming only the other
   (`inbound_gate_policy=allowlist`, `inbound_gate_allowlist=[the other
   agent's address]`, `inbound_gate_action=block` or `review`). This is the
   **primary** mitigation against a forged probe — e2a rejects (or holds)
   any third-party inbound mail to A or B at the source, before it ever
   reaches this service. The signed nonce (see "Nonce authentication" above)
   is defense-in-depth for the case where this allowlist is misconfigured,
   absent, or loosened later.
2. Create an account-scoped webhook subscription pointing at the deployed
   service's `https://…/webhook`, via the e2a MCP `create_webhook` tool or the
   dashboard. The `whsec_…` secret is **shown only at creation** — capture it
   then and store it as the deployment's `E2A_MONITOR_WEBHOOK_SECRET`. If it is
   lost, rotate rather than trying to read it back.
3. Point a Cloud Scheduler job at `POST /tick`.
4. If the deployed MCP server is reachable at a fixed URL, set
   `E2A_MONITOR_MCP_URL` to its full `/mcp` endpoint to enable that interface.

Note the subscription fans out every account event (`email.sent`, bounces,
domain events), not just `email.received` — the handler narrows on its own.

## Multi-runtime image

The image now needs **both** Python and Node: Python drives the service and
the `api`/`python_sdk`/`mcp` interfaces directly; `ts_sdk` and `cli` are
short-lived Node subprocesses (`js/monitor-helper.mjs` for the TS SDK; the
`@e2a/cli` bin directly for the CLI). The build stays faithful to "published
packages only, never workspace source" for BOTH ecosystems: `requirements.txt`
pins the exact PyPI `e2a` version; `package.json` pins the exact npm
`@e2a/sdk` and `@e2a/cli` versions. Neither `sdks/python`, `sdks/typescript`,
nor `cli/` is reachable from the build context (`tests/sdk-monitor`), so both
dependency trees can only come from a real registry install. The Dockerfile
verifies this at build time (`pip show e2a`, `npm ls @e2a/sdk @e2a/cli`) —
a version mismatch or failed install fails the build, which is the intended
signal, not something to work around.

## SDK / package version bump policy

`requirements.txt` pins an exact published Python SDK version (currently
**5.4.0**); `package.json` pins exact published `@e2a/sdk` (**5.4.0**) and
`@e2a/cli` (**2.1.0**) versions. None of these is ever a path or workspace
dependency on the corresponding source directory — installing from local
source would defeat the entire purpose by hiding a broken publish.

Bump each pin as a step of its package's release, *after* the new version is
live on PyPI/npm. Rebuilding the image then proves the release is installable
and works end to end against production.

## Local checks

```bash
python3 -m venv venv && ./venv/bin/pip install -r requirements.txt
E2A_MONITOR_WEBHOOK_SECRET=whsec_testsecret ./venv/bin/python test_monitor.py
```

`test_monitor.py` is offline and dependency-free (no pytest): it covers
signature accept/reject, replay rejection, fail-closed on an unset secret,
non-inbound event types, the nonce↔iface encoding round trip (including the
HMAC tag) for all five interfaces, nonce-MAC authentication (a tampered
iface/timestamp/tag is rejected — no reply, no `monitor_ok`; a pre-MAC
3-part nonce doesn't even match), the stale-reply guard on both legs,
interface-strategy dispatch (a reply goes to the exact interface the nonce
names, and no other), the unknown-iface safety net, the `/tick` aggregate +
status-code semantics (all-ok → 200, one optional interface skipped but the
rest ok → 200, partial failure → 207, total outage across every considered
interface → 500), uniform non-`sent`/`accepted` send-status handling across
every offline-exercisable interface (a stubbed `pending_review`/`failed`
response raises immediately rather than logging success-shaped), the cleanup
behavior on both legs (the probe/reply is trashed via the nonce's own
interface after the leg's success marker, a delete failure logs
`stage=cleanup` without revoking `monitor_ok`/`monitor_replied` or the 200,
and a delete-less interface is a `monitor_skip`, not an error), the wire
shape of the `api`/`python_sdk`/`ts_sdk`/`cli`/`mcp` strategies for
send/reply AND delete (argv/URL/headers/tool-arguments, `deleted:true`
receipt validation, and that secrets never land in argv), the minimal
subprocess environment (the full parent env, including the webhook secret,
is never handed to a node/CLI child), and `McpStrategy`'s JSON-RPC response
parsing (SSE framing, a JSON-RPC error, a tool-level `isError`, a leading
notification or mismatched-id frame that must be skipped rather than
mistaken for the real response, and the malformed-response guard). Every
interface's actual send/reply/delete I/O is stubbed with fakes injected into
`monitor.STRATEGIES`, or with `urllib.request.urlopen` / `subprocess.run`
monkeypatched to capture/replay a call — no real network call, no real
subprocess to node/npm. The live round trips are only exercisable against a
real deployment with a reachable webhook URL.

## Design decisions to review

A few choices were made from reading the OSS source rather than a written
spec; flagging them explicitly:

- **MCP transport**: the deployed MCP server is stateless
  (`sessionIdGenerator: undefined` in `mcp/src/http-server.ts`), which skips
  all session/initialize gating — so a bare `tools/call` JSON-RPC request
  dispatches with no prior `initialize`. The server answers with an SSE
  response (`enableJsonResponse` is left unset, i.e. `false`), so the `mcp`
  interface hand-rolls a single JSON-RPC POST over stdlib `urllib` and parses
  the `data:` frames itself, mirroring
  `internal/selftest/scenarios.go`'s `parseJSONRPCEnvelope` rather than
  pulling in the `mcp` Python package — one dependency-free request/response,
  no session to keep alive, matches this service's "stateless, minimal deps"
  design.
- **`E2A_MONITOR_MCP_URL` is optional and un-defaulted.** It is NOT derived
  from `E2A_BASE_URL` + `/mcp` even though the hosted deployment happens to
  serve both on `api.e2a.dev` today, because the deployed MCP server's
  reachable URL is a deployment detail this monitor shouldn't assume; unset
  it and the `mcp` interface just skips (`monitor_skip`) rather than the
  whole service refusing to start.
- **CLI reply does NOT need a send-only fallback.** `e2a reply <message-id>
  --body … --agent …` (`cli/src/commands/send.ts`) calls
  `client.messages.reply`, whose wire `ReplyRequest` has no subject field —
  the server always derives `Re: <original subject>`, so the CLI's reply is a
  genuine in-thread reply like every other interface, not a plain send.
