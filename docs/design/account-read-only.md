# Read-only accounts (abuse pause)

Status: implemented. Owner surfaces: `internal/httpapi/read_only.go` (/v1),
`internal/agent/read_only.go` (legacy mux), `mcp/src/tools/mutating.ts` (MCP
classification), `web/src/app/components/ReadOnlyBanner.tsx` (dashboard).

## Decision

An account whose sending is paused with `pause_class = 'abuse'`
(`account_sending_controls`, migration 122) is **read-only**: no write of any
kind succeeds on any customer surface. Pauses with the other classes
(`operator`, `billing`, `system`) keep their behaviour — sending is refused,
everything else works — because those are the automated detector's lever and
may hit legitimate customers.

Motivation: while paused, an abuse operator could still create brand-named
agents, rename them, delete messages (evidence) and register domains; only
sending and permanent deletion were blocked.

The state is exactly `state = 'paused' AND pause_class = 'abuse'`
(`identity.Store.AccountReadOnly`). A resume lifts it immediately even though
the class is kept as history. There is no migration: the columns exist since
122.

## What is refused and what is allowed

Refused (403 `account_read_only`): every operation that mutates state — agent
create/update/rename/restore/trash, message send/reply/forward/schedule,
review approve/reject, message delete/restore/label edits, domain
register/verify/delete, API key create/delete, webhook
create/update/delete/rotate/test, event redelivery, contacts and imports,
templates, outreach (engagement) upserts/deletes, protection settings,
suppression create/delete, the sending-access request, the account's legacy
dashboard writes (profile PATCH, agent PUT/DELETE, key create/delete), OAuth
consent (it mints a new grant), the HITL magic-link approve/reject, and the
internal external-principal attach.

Allowed:

- every read (lists, gets, message reads, exports, `GET /v1/account`,
  metrics, events, attachment downloads, WebSocket live-tail);
- `POST /v1/templates/validate` (validates a draft, stores nothing);
- sign-in and sign-out, OAuth token exchange/revocation and
  `/agent/identity` (the credentials they mint are refused for writes like
  any other);
- moving the account to the trash (`DELETE /v1/account` without `permanent`,
  receipt `mode: "trash"`); the permanent erase stays `409 erase_held` while
  any pause applies, and the dashboard restore/erase interstitial is
  unchanged (erase is held there too);
- anonymous surfaces that belong to no account: dynamic client registration
  (the client is bound to an account only at consent, which is refused), the
  public feedback form (how a paused customer reaches support), the public
  unsubscribe link (a recipient's action);
- operator local commands and the internal limits/provision endpoints.

## Enforcement: one place per surface

| Surface | Enforcement point | Classification |
|---|---|---|
| REST `/v1` (API keys of both scopes, dashboard session, OAuth access tokens, agent access tokens, delegated tokens) | `readOnlyGuard` Huma middleware | `operationAccess`: every operation explicit; rule = HTTP method (GET/HEAD read, else write) with three named exceptions (`validateTemplate` read, `deleteAccount` allowed write, `getInfo` public). An operation missing from the table falls back to the method rule, so an unclassified write is refused. |
| Legacy mux (dashboard `/api/*`, OAuth) | `legacyReadOnlyMiddleware` gorilla middleware | `legacyWriteRoutes`: every non-GET route is `legacyAccountWrite` or `legacyExempt` with its reason. |
| HITL magic links (`/v1/approve`, `/v1/reject` POST) | `refuseMagicIfReadOnly` in the handler | Token-authorized, not a principal request; the owning account is resolved from the message. |
| Internal external-principal attach | handler check | The account is named in the signed body. |
| MCP tools | the `/v1` guard (every tool calls the REST API with the caller's credential) | `MUTATING_TOOLS` / `NON_MUTATING_TOOLS`, pinned against the registered tools and the MCP annotations. The MCP server keeps no account state that could go stale. |
| WebSocket | nothing to enforce | The live-tail socket has no client-to-server message types (client frames are discarded); it is a read surface. |

Freshness: the guards do one primary-key lookup per write, uncached, so an
operator pause or resume takes effect on the very next request. Reads never
pay the lookup. A failed lookup fails closed: `503 auth_unavailable`, the
write does not run.

## Error shape

`403` with code `account_read_only` (catalog family `auth`, not retryable).
The message is the same on every surface
(`identity.AccountReadOnlyMessage`): sending is paused for an abuse review,
the account is read-only, reads still work, contact support — with the
address from `notifications.support_email` (falling back to
`notifications.reply_to`) when configured. The operator's reason text and
evidence reference are never shown.

Clients: both SDKs map the code to their permission error, non-retryable; the
CLI exits with the new frozen code `10` (`READ_ONLY`) and prints guidance;
`whoami` (CLI) and `GET /v1/account` report `read_only`; the dashboard shows a
persistent banner and disables the cheap-to-disable write controls.

## Operator note

`e2a -pause-account-sending -pause-class abuse -account-id … -reason …` now
also freezes every customer write for that account. The readback prints
`read_only: true`. Use `operator`, `billing` or `system` to stop only sending.
`-resume-account-sending` lifts both.

## Tests

- `internal/httpapi/read_only_test.go`: the spec walk — every operation in
  `api/openapi.yaml` is classified, agrees with an independent derivation
  (method rule + named exceptions), and is driven over HTTP as a read-only
  account with account- and agent-scoped principals (writes → 403, reads and
  allowed writes pass without the lookup); fail-closed; writable accounts
  pass.
- `internal/agent/read_only_internal_test.go`: every legacy write route is
  classified.
- DB-backed seams: `internal/identity/account_read_only_test.go`,
  `internal/sendingpolicy/account_read_only_test.go`,
  `internal/agent/read_only_test.go`,
  `internal/apiserver/read_only_db_test.go` (full composition, operator pause
  path, trash/erase).
- Conformance: `account_read_only_refuses_writes` in
  `tests/contract/scenarios.yaml` against the contract server's seeded
  abuse-paused account, through the Go, TypeScript and Python runners.
