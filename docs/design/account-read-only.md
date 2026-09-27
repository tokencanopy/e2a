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
consent `allow` (it mints a new grant), the HITL magic-link approve/reject, and
attaching a new external principal.

Allowed:

- every read (lists, gets, message reads, exports, `GET /v1/account`,
  metrics, events, attachment downloads, WebSocket live-tail);
- `POST /v1/templates/validate` (validates a draft, stores nothing);
- OAuth consent `deny` (it grants nothing and must reach the client), and an
  idempotent re-attach of an already-attached external principal;
- sign-in and sign-out, OAuth token exchange/revocation and
  `/agent/identity` (the credentials they mint are refused for writes like
  any other);
- moving the account to the trash (`DELETE /v1/account` without `permanent`,
  receipt `mode: "trash"`); the permanent erase stays `409 erase_held` while
  any pause applies, and the dashboard restore/erase interstitial is
  unchanged (erase is held there too). While the account is read-only the
  trash does **not** tear down its SES sender identities (the domains are
  still unverified, so nothing is sent from them): they stay as provider-side
  evidence until the purge at the end of the trash window;
- anonymous surfaces that belong to no account: dynamic client registration
  (the client is bound to an account only at consent, which is refused), the
  public feedback form (how a paused customer reaches support), the public
  unsubscribe link (a recipient's action);
- operator local commands and the internal limits/provision endpoints.

## Enforcement: one place per surface

| Surface | Enforcement point | Classification |
|---|---|---|
| REST `/v1` (API keys of both scopes, dashboard session, OAuth access tokens, agent access tokens, delegated tokens) | `readOnlyGuard` Huma middleware | `operationAccess`: every operation explicit; rule = HTTP method (GET/HEAD read, else write) with three named exceptions (`validateTemplate` read, `deleteAccount` allowed write, `getInfo` public). An operation missing from the table falls back to the method rule, so an unclassified write is refused. |
| Legacy mux (dashboard `/api/*`, OAuth) | `legacyReadOnlyMiddleware` gorilla middleware | `legacyWriteRoutes`: every non-GET route is `legacyAccountWrite` or `legacyExempt` with its reason. A `legacyAccountWrite` route authenticates the session cookie only, so the guard resolves the caller exactly as the handler does and refuses outright: an `Authorization` header on such a route is `400 ambiguous_credentials` (the guard and the handler can never disagree about the caller), no valid session is `401` from the guard itself. A write route missing from the table defaults to refuse: every credential presented must resolve and none may be read-only (an anonymous request carries no account and passes, which keeps routes the binary mounts on the same router — the SNS `/webhooks/ses` — working). |
| Raw routes on the `/v1` chi root (not Huma operations) | per route, see `rawRouteReadOnly` | Every non-GET raw route is on an explicit list with its reason (unsubscribe = recipient action; magic links = handler check; trash interstitial unchanged). `TestRawRootRoutesAreClassifiedForReadOnly` walks the root with `chi.Walk` and fails on any other. |
| OAuth consent (`POST /oauth2/consent`) | `handleOAuthConsent`, after the provider, authorize-request and session checks | Only `allow` is refused (it mints a grant). `deny` still returns fosite's `access_denied` redirect to the client, and unauthenticated callers keep consent's own 404/503/authorize errors. Consent authenticates the session cookie only, so an `Authorization` header cannot steer it. |
| HITL magic links (`/v1/approve`, `/v1/reject` POST) | `refuseMagicIfReadOnly` in the handler | Token-authorized, not a principal request; the owning account is resolved from the message. |
| Internal external-principal attach | the store's attach transaction (`AttachExternalPrincipal`) | The account is named in the signed body. Attaching a NEW (issuer, subject) to a read-only account is `403 account_read_only`; an idempotent re-attach of an already-attached triple returns `200` as before (the external reconciler replays these). |
| MCP tools | the `/v1` guard (every tool calls the REST API with the caller's credential) | `MUTATING_TOOLS` / `NON_MUTATING_TOOLS`, advertised on every tool as `_meta["e2a/mutating"]` and pinned by tests: against the registered tools; against the HTTP methods of the `/v1` operations each tool calls (`TOOL_OPERATIONS`, walked against `api/openapi.yaml` with the server's rule); and against the MCP annotations (no readOnlyHint tool mutates, every destructiveHint tool does). The MCP server keeps no account state that could go stale. |
| HITL expiry sweep (`internal/hitlworker`) | the candidate queries (`ListExpiredPending`, `ListExpiredReviews`) | An approve-on-expiry hold of a read-only account is not a candidate: it stays `pending_review`, so suspicious inbound mail is never released into the inbox or webhooks and held outbound mail is never sent. Excluded at selection, not skipped in the worker, so such holds cannot sit at the head of the ordered, limited sweep and starve other accounts. Reject-on-expiry holds still resolve (rejecting releases nothing). A resume makes them candidates again. |
| WebSocket | nothing to enforce | The live-tail socket has no client-to-server message types (client frames are discarded); it is a read surface. |

Freshness: the guards do one primary-key lookup per write, uncached, so an
operator pause or resume takes effect on the very next request. Reads never
pay the lookup. A failed lookup fails closed: `503 auth_unavailable`, the
write does not run; so does a principal that resolved without an account.

Rate limiting runs before the guard on `/v1`, so a refused write still
counts against the caller's request budget. That order is deliberate: the
guard does a database lookup, and running it first would let an
over-the-limit caller drive unlimited lookups.

Accepted window: the guard reads the control row before the handler runs,
outside the handler's transaction. A write whose guard check passed a few
milliseconds before an operator's pause commits can still complete (the same
holds for the HITL sweep between candidate selection and the hold's
transition). This is accepted: the pause is an operator action on a human
timescale, the next request is refused, and the one consequence that matters
most — sending — is re-checked at the sending gate
(`sendingpolicy`, immediately before provider submission), which refuses a
paused account regardless of what the guard saw.

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
- `internal/agent/read_only_test.go`: every legacy account-write route
  refused for a read-only session, including with a junk bearer or another
  account's valid key alongside the cookie (`400 ambiguous_credentials`), a
  bare bearer, and no credential (`401`); an unclassified write route
  defaults to refuse.
- `internal/httpapi/read_only_routes_test.go`: every non-GET raw route on
  the `/v1` chi root is a Huma operation or explicitly exempt.
- `internal/agent/read_only_test.go` (consent): `allow` refused and creates
  nothing, `deny` still redirects with `access_denied`, an unauthenticated
  bad authorize request gets consent's own error; (attach) a replay of an
  attached principal is `200`, a new one `403`.
- `mcp/tests/tools.test.ts`: the mutating flag against `TOOL_OPERATIONS` ×
  `api/openapi.yaml`, destructiveHint ⇒ mutating, and `_meta["e2a/mutating"]`
  on every listed tool.
- `internal/hitlworker/read_only_test.go`: the expiry sweep leaves a
  read-only account's approve-on-expiry holds pending (inbound and
  outbound), still rejects reject-on-expiry holds, and releases after a
  resume.
- DB-backed seams: `internal/identity/account_read_only_test.go` (incl. the
  trash keeping sender identities while read-only),
  `internal/sendingpolicy/account_read_only_test.go`,
  `internal/apiserver/read_only_db_test.go` (full composition, operator pause
  path, trash/erase).
- Conformance: `account_read_only_refuses_writes` in
  `tests/contract/scenarios.yaml` against the contract server's seeded
  abuse-paused account, through the Go, TypeScript and Python runners.
