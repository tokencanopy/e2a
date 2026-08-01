# Changelog

## 2.2.0

Additive only — no flag, output-field, or exit-code meaning changes to any
command that shipped in 2.1.0. The new contacts and scheduled-sending
surfaces are **beta** and may change before they are declared stable. The
complete GA-vs-beta stability matrix for the whole `/v1` surface is
documented in
[docs/api.md → Stability: GA and beta surface](../docs/api.md#stability-ga-and-beta-surface).

**Added:** `e2a contacts` (beta) — manage account-level contact identity and
per-agent outreach state, with suppression visibility. Subcommands:
`list | get | create | update | delete` for contacts (account scope;
`--source`, `--import-batch`, `--created-after`/`--created-before` filters on
`list`; `--idempotency-key` on `create`; `--if-match <etag>` on `update` to
reject a stale edit), `import` (reads an RFC 4180 CSV; `--email-column`,
`--name-column`, `--on-conflict merge|skip`, `--dry-run` to preview without
writing, `--agent`/`--stage` to enroll rows in an agent's outreach in the same
transaction, `--idempotency-key` to safely replay a timed-out upload; up to
1,000 rows per request with per-row outcomes), `imports delete
<import-batch-id>` (reverses one import batch — the server removes only
verifiably untouched rows the batch created and reports what it kept), and
`outreach list | get | set | delete` for per-agent outreach state
(`--stage`, `--replied`, `--suppressed`, `--next-action-before`,
`--last-outbound-before` filters on `outreach list`; `--if-match` on
`outreach set`; also usable with an agent-scoped credential for its bound
inbox). `contacts delete` removes the account contact and its per-agent
outreach rows; suppression and consent records survive.

**Added:** `--send-at <rfc3339>` (beta) on `e2a send` and `e2a reply` —
scheduled sending. Requires an explicit UTC offset and can be at most 90 days
ahead. A future schedule exits `0` with `status=scheduled`: the message is
durably queued for future submission, so do not retry. `scheduled_at` in
`--json` output is the future submission time — a "not before" bound, not an
exact fire time. Direct self-send cannot be scheduled (permanent request
error) unless a review hold takes precedence — held sends drop the schedule
and send on approval. Trashing the message before provider submission starts
cancels the send; restoring it before the send time re-arms it, restoring at
or after that time restores the message but leaves the send canceled.

**Added:** on servers that expose it, SDK-shaped JSON from `messages
list`/`messages get`/`listen` may include the optional beta `threadId` — a
server-owned, read-only, mailbox-local email-thread identity. Human-readable
output formats are unchanged, and there is no `threadId` request flag,
filter, or thread endpoint. `--conversation-id` remains caller-owned
application correlation.

**Added:** `e2a suppressions` — inspect and manage recipient block lists.
Account-wide suppressions are GA; agent-scoped suppression management (the
`--agent` form of `list`/`remove`, and all of `add`) is beta and may change
before it is declared stable. Subcommands: `list` (without `--agent`, the
account-wide list — auto-populated by hard bounces and complaints and
enforced for every inbox at send time, where a listed recipient fails with
`recipient_suppressed`; with `--agent <email>`, that one inbox's
unsubscribe/manual blocks; `--limit <n>` bounds the listing, 1–10000,
default 100; output is TSV of address, source, reason, and created-at, or
NDJSON with `--json`), `add <address> --agent <email>` (`--agent` is
required — manual blocks are per-agent; there is no account-level create,
account entries only ever come from bounces/complaints; `--reason <text>`
attaches an optional note), and `remove <address>` (un-suppresses
account-wide without `--agent`, removes only that agent's block with it; the
CLI supplies the API's `?confirm=DELETE` guard). All subcommands require an
account-scoped credential — an agent-scoped credential cannot manage even
its own inbox's blocks. Un-suppress account-wide only for addresses known to
be deliverable: removing a genuine bouncer hurts sender reputation.

## 2.1.0

Additive only — no flag, output-field, or exit-code meaning changes to any
command that shipped in 2.0.0.

**Added:** `e2a messages lifecycle <message-id>` (beta) — print a message's
observed lifecycle transitions (send, delivery, bounce, review, deletion, …)
as the canonical lifecycle page in JSON. Flags: `--cursor` (continue from a
prior page), `--limit` (page size, 1–100), `--agent` (owning inbox, or the
configured `agent_email`), `--json`. Beta because it tracks the beta
`GET /v1/agents/{email}/messages/{id}/lifecycle` API surface.

**Documented:** `listen --once` with `--text`/`--json` fetches the message over
the API `GET`, which marks it read — the same side effect as `messages get`.
Behavior is unchanged; it was previously undocumented.

**Added:** `e2a doctor` — read-only diagnostics for the production email path.
Checks CLI config and credential scope, API connectivity (`GET /v1/info`),
agent access, custom-domain DNS (live ownership TXT / MX / DKIM / SPF lookups
against the server-prescribed records, plus an advisory warn-only DMARC check
and the SES sending status), MCP reachability and advertised OAuth metadata,
webhook configuration and delivery history, and outbound SMTP config
visibility for self-hosted operators (credential presence only — values are
never printed). Record matching mirrors the server's own probe semantics:
ownership TXT is a substring match, SPF is case-insensitive and tolerates
extended records that keep the prescribed include, and DKIM compares only the
extracted `p=` payload (extra tags and reordering are fine). An unexpected
internal error while checking one domain or webhook is reported as a
`doctor.internal` check instead of aborting the report.
It never sends mail and never mutates DNS, webhooks, or any other resource;
in particular it does not call `POST /v1/domains/{domain}/verify` (which
mutates verification state), `POST /v1/webhooks/{id}/test` (which delivers a
real event), or `POST /v1/agents/{email}/test` (which sends real mail).
`--json` emits a versioned `e2a.doctor/v1` report. Two exit codes were ADDED
to the frozen contract (existing codes are unchanged): `8` — diagnostics
completed with warnings only; `9` — diagnostics found a definite
configuration failure. `0`/`1`/`4` keep their existing meanings for healthy,
transient-connectivity, and authentication outcomes. Every network operation
is bounded by a 5-second timeout.

**Added:** `e2a contacts` — manage account-level contact identity and
per-agent outreach state, with suppression visibility (`list`/`get`/
`create`/`update`/`delete`/`import`/`imports delete`, plus `outreach
list`/`get`/`set`/`delete`). Contact identity operations require account
scope; `outreach` also supports an agent-scoped credential for its bound
inbox. `create`/`import` accept `--idempotency-key`; `update`/`outreach set`
accept `--if-match <etag>` to reject a stale edit.

**Added:** `--send-at` on `send`/`reply` — schedule a future send (RFC 3339
with an explicit UTC offset, at most 90 days ahead). Beta and may change
before it is declared stable. A future schedule exits `0` with
`status=scheduled` and is durably queued, so do not retry.

**Added:** beta `threadId` on message list/get/listen JSON — a server-owned,
read-only mailbox-local identity, distinct from the caller-owned
`conversation_id` filter. Human-readable formats are unchanged.

## 2.0.0

A major bump: the published 1.6.0 and this tree had diverged
under one version number, and the intervening work removes login flags, renames
output fields, and changes exit codes — every script driving the CLI should be
re-read against the breaking notes below before upgrading.

**Breaking:** `messages list` renamed both of its output identity fields.
The TSV's first column is now the message's bare `id` (was `message_id`) and its
second column is the claimed header From (was the unqualified `from`); `--json`
likewise emits `id` and `headerFrom`. The CLI no longer rewrites the SDK's
escaped `_from` property back to `from` on output — that rewrite existed to hide
a codegen artifact that the field rename made obsolete. A poll loop reading
`while IFS=$'\t' read -r id from at` keeps working positionally, but anything
selecting `.message_id` or `.from` out of `--json` must be updated. Note that
`listen`'s output is a different surface and still names the field `message_id`:
it carries the WebSocket notification envelope, not the REST message model.

**Breaking:** the CLI no longer reads `E2A_BASE_URL`. That name now configures
the SDKs and points at the API host alone; the CLI's deployment root is
`E2A_URL`, which must serve the `e2a login` browser flow as well as proxy `/v1`.
Setting `E2A_BASE_URL` to steer the CLI used to work and now silently would not,
so the CLI prints a warning naming the host it actually resolved. Self-host
users must switch to `E2A_URL`.

**Breaking:** `send`/`reply` exit codes changed for non-`sent` outcomes. The
old rule was "anything other than `sent` exits `3` (HELD)", which wrongly failed
a durably-queued async send. The CLI now branches on the response body's status:
`accepted` is a success and exits `0`, `pending_review` still exits `3`, and a
terminal `failed` or an unrecognized 2xx outcome exits the new code `7`
(`SEND_OUTCOME`) — a persisted result that must not be blindly retried, since
the returned message id proves the server already created something observable.

**Breaking:** `listen`'s WebSocket close codes are now distinct — `4000` for a
connection replaced by a newer one, `1008` for a rejected connection. Scripts
that treated every `1008` as "replaced" (or as a retryable blip) will now
misclassify a genuine rejection.

**Breaking:** outbound attachments are enforced against documented limits —
10 MB per attachment, 10 attachments, 25 MB total. `--attach` invocations that
previously went out are now rejected at the API.

**Breaking:** list pagination converged on a single cap and default of `100`
(the default was `50`). `messages list` asks for the maximum page size, so a
caller relying on the old implicit 50-row page will see larger pages.

Delete operations now return `200` with a typed deletion object rather than a
bare `204`, and enqueued async work returns `202 Accepted`. The CLI surfaces the
new bodies through `--json`.

The CLI now runs on the 5.x TypeScript SDK (`@e2a/sdk` `^5.0.0`).

`send` and `reply` accept `--reply-to <email>` to set a caller-supplied
`Reply-To` on the outbound message.

**Breaking:** inbound JSON uses `headerFrom` instead of `from` and includes
structured `authentication` evidence. Plain `listen` output labels the address
as a claim and prints the DMARC verdict; `listen --once` TSV now emits
`message_id`, claimed header From, and `received_at`.
OpenClaw forwarding now wraps the body as untrusted content and includes the
claimed Header-From, DMARC summary, and nullable verified domain in the input
prompt instead of forwarding an unlabeled sender claim.

**Breaking:** `e2a login --agent <inbox>` is removed. The CLI is now
account-credential only on the login path — the browser handoff always saves an
account-scoped key. Mint a least-privilege inbox-bound key with
`e2a keys create --agent <inbox>` after logging in.

**Breaking:** `e2a login --with-key` is removed. `login` is now exclusively the
interactive browser flow. Headless environments should set `E2A_API_KEY` (and
may persist it with `e2a config set api_key <key>` before validating it with
`e2a whoami`).

`e2a config set` now accepts only `api_key` and `agent_email`; deployment URL,
shared domain, and cached key scope are managed internally or through their
documented environment variables.

**Breaking:** `e2a login` no longer sets `agent_email`. It previously persisted
whichever inbox the server's handoff happened to name first, which silently
chose the `From:` address for every later `send`/`reply`. An account-scoped key
spans every inbox, so there is no inbox to infer. Commands needing one resolve
`--agent` → `E2A_AGENT_EMAIL` → an explicitly-set `agent_email`, or exit `2`.
Set a default with `e2a config set agent_email <email>`; a value set that way is
preserved across re-login.

`listen` now exits `1` (previously `0`) in two cases that used to look like a
clean stop: a long-running listen whose stream ends for any reason, such as a
peer's normal WebSocket close (code 1000); and, under `--once --forward`, a
forward POST that fails after the message was already consumed off the
stream and printed to stdout.

## 1.6.0

Adds the CLI's scripting/harness surface: `whoami`,
`send`/`reply` (with `--attach`), `messages list`/`get`, and a stable 0–7
exit-code contract (`cli/src/exit.ts`) for shell-based harnesses (skills,
hooks, CI) — scripts can branch on the process exit status instead of
parsing JSON. Also adds:

- `agents list`/`create`/`get`, `keys create`/`list`/`delete`, and
  `protection get`/`set` — provision an inbox and a least-privilege key
  end to end without the dashboard.
- `login --agent <inbox>` (mint a least-privilege agent-scoped key, revoking
  the account bootstrap key — removed in 2.0.0) and `login --with-key`
  (headless: validate and save a key from the arg, `$E2A_API_KEY`, or stdin).
- `listen --conversation`/`--once`/`--until`/`--text` — a blocking-wait
  primitive for a script waiting on one reply.

## 1.5.1

Republishes the CLI with a corrected `@e2a/sdk` dependency
range (`^4.0.0`); the previously published 1.5.0 still declared `^3.0.0`, so a
fresh `npm i -g @e2a/cli` could resolve an SDK major incompatible with the
current API. No CLI behavior changes.

## 1.5.0

The `e2a` CLI targets the e2a v1 API and runs on the 4.x TypeScript SDK
(`@e2a/sdk`).

### Notes
- Commands are thin wrappers over the namespaced SDK surface
  (`client.agents`, `client.messages`, `client.domains`, `client.webhooks`,
  `client.account`). Auth reads `E2A_API_KEY`; `E2A_URL` overrides the endpoint
  for self-hosted deployments (default `https://e2a.dev`, the hosted product's
  unified host — it serves the `e2a login` browser flow and proxies the `/v1`
  API). Direct SDK users (no browser login) can point at the API host
  `https://api.e2a.dev` instead.
