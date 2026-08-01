# Suppressions across client surfaces — design

2026-08-01 · branch `feat/suppressions-ui`

## Problem statement

The suppression system (account-level bounce/complaint blocks, agent-level
unsubscribe/manual blocks) is fully implemented server-side with five `/v1`
endpoints and full TS/Python SDK coverage, but three client surfaces cannot
see or manage it:

- **Web dashboard**: no browsable suppression list anywhere. Operators can
  only infer suppression from per-contact chips on the Outreach tab — which
  misses any suppressed address that is not an outreach contact.
- **CLI**: no `suppressions` command at all.
- **MCP server**: no suppression tools at all.

This violates the repo's parity expectation ("every API change is expected to
maintain parity across all client surfaces") — the endpoints shipped without
CLI/MCP/web coverage.

Success criteria: an operator can answer "who is on my suppression list and
why" and remove an entry from (a) the dashboard, (b) `e2a suppressions`, and
(c) MCP tools, for both scopes, without touching the database.

## Goals and non-goals

**Goals**
1. Per-agent **Suppressions** tab under `/inboxes/` (list / add / remove),
   backed by `GET/POST/DELETE /v1/agents/{email}/suppressions`.
2. Account-level **Suppressions** view as a sub-route of Contacts
   (`/contacts/suppressions`, list / remove), backed by
   `GET /v1/account/suppressions` + `DELETE /v1/account/suppressions/{address}`.
3. CLI command group `e2a suppressions` covering both scopes.
4. MCP tools covering both scopes (admin tier).

**Non-goals**
- No wire-API changes: no new endpoints, no OpenAPI/spec regeneration, no
  migrations, no Go changes. (Consequence: no `tests/contract/scenarios.yaml`
  changes — that requirement triggers on `/v1` surface changes only.)
- No account-level *create* (the API deliberately has no
  `POST /v1/account/suppressions`; the `manual` source there is reserved for
  server-side flows). If that's wanted later it's an API design task first.
- No unification with the SES account-level suppression list (AWS-side state,
  visible only in bounce reasons).

## Relevant context and constraints

- **Endpoints** (all live, SDK-covered):
  - Account (stable GA): `listSuppressions`, `deleteSuppression` (requires
    `?confirm=DELETE`). Cursor-paginated `Page[SuppressionView]`
    (`address, reason?, source: bounce|complaint|manual, source_message_id?, created_at`).
  - Agent (**beta**, account-scope-only): `listAgentSuppressions`,
    `createAgentSuppression` (`address`, `reason?`), `deleteAgentSuppression`
    (requires `?confirm=DELETE`). `Page[AgentSuppressionView]`
    (`agent_email, address, reason?, source: unsubscribe|manual, created_at`).
  - Both return `501 not_implemented` when the deployment lacks the deps.
- **SDKs**: TS `client.account.suppressions.list()/delete()` +
  `client.agents.listSuppressions/createSuppression/deleteSuppression`;
  Python equivalents. (Implementation deviation, discovered mid-build: the TS
  account `suppressions.list()` took no page-size parameter, unlike its
  sibling resources — it gained an additive optional `{ limit }` so the MCP
  tool could expose the standard `cursor`+`limit` pagination shape.)
- **Web**: Next.js static export, `"use client"` pages, raw `fetch` with
  `credentials: "include"` against same-origin `/v1/*` (see outreach page —
  the pattern to copy). Tabs live in `AgentHeader.tsx` (`TABS` array +
  `AgentTab` type) and `inboxes/(view)/layout.tsx` (`detectTab`). Contacts
  page uses `PageShell`; it has no sub-tab mechanism today.
- **CLI**: command modules in `cli/src/commands/*.ts`, dispatched from
  `bin/e2a.ts` (manual arg parsing, `EXIT` codes frozen), vitest tests.
- **MCP**: tool modules in `mcp/src/tools/*.ts` registered in `server.ts`;
  thin `client.ts` wrapper over the TS SDK; **every tool must be added to
  exactly one tier set in `tools/tiers.ts`** (a test enforces completeness).
- Assumption: suppression lists are small today but auto-grow on real bounce
  traffic — pagination must work from day one.

## Proposed design

### 1. Web — per-agent Suppressions tab

- `AgentHeader.tsx`: add `"suppressions"` to `AgentTab` and
  `TABS` (`{ key: "suppressions", label: "Suppressions", slug: "suppressions" }`),
  between Outreach and Trash. Make the tab strip `overflow-x-auto` so five
  tabs degrade to horizontal scroll on narrow screens instead of breaking.
- `inboxes/(view)/layout.tsx`: add the `detectTab` branch.
- New page `inboxes/(view)/suppressions/page.tsx`, modeled line-for-line on
  the outreach page (Suspense router keyed on `?email=`, `refresh` with
  cursor append, mobile cards + desktop table, error banner, Beta chip since
  the agent API is beta):
  - Table: Address · Source (Chip: `unsubscribe` warn, `manual` neutral) ·
    Reason · Added (localized date) · Remove action.
  - "Suppress an address" inline panel (address + optional reason →
    `POST /v1/agents/{email}/suppressions`); refresh on success.
  - Remove → `confirm()` → `DELETE .../suppressions/{address}?confirm=DELETE`.
  - Empty state explains what lands here (unsubscribes + manual blocks) and
    that account-level bounce/complaint blocks live under Contacts →
    Suppressions (cross-link).

### 2. Web — account-level Suppressions under Contacts

- New route `contacts/suppressions/page.tsx` (deep-linkable; static-export
  friendly), plus a two-item view-switcher strip (Contacts · Suppressions)
  rendered under the PageShell header on **both** contacts pages — same
  minimal underline style as AgentHeader tabs, no new component abstraction
  until a third view exists.
- Page content mirrors the agent tab minus the add panel:
  - Table: Address · Source (Chip: `bounce`/`complaint` danger, `manual`
    neutral) · Reason (truncated with `title` tooltip — SES reasons run
    ~300 chars) · Source message (mono `source_message_id`, plain text) ·
    Added · Remove.
  - Remove `confirm()` copy warns about the consequence: the address becomes
    sendable again; removing a genuinely-bouncing address hurts sender
    reputation.
  - Subtitle explains scope: account-wide, auto-populated by hard bounces and
    complaints, enforced at send time (`recipient_suppressed`).

### 3. CLI — `e2a suppressions`

New module `cli/src/commands/suppressions.ts`, dispatched from `bin/e2a.ts`:

```
e2a suppressions list [--agent <email>] [--limit N] [--json]
e2a suppressions add <address> --agent <email> [--reason <text>] [--json]
e2a suppressions remove <address> [--agent <email>] [--json]
```

- `list` without `--agent` → account list (`client.suppressions.list()`);
  with `--agent` → agent list. TSV-ish table output like `contacts list`
  (sanitized fields), `--json` for raw items.
- `add` requires `--agent` (usage error without it — account-level create
  does not exist; the error says so explicitly).
- `remove` routes on `--agent` presence. 404 → EXIT.PERMANENT with the
  server's message.
- Help text added to `bin/e2a.ts` usage block. Vitest coverage in
  `cli/src/__tests__/` following the contacts command tests.

### 4. MCP — suppression tools

New module `mcp/src/tools/suppressions.ts` (5 tools, one per operation,
matching contacts.ts conventions — zod `strictInputSchema`, `runTool`,
`readOnlyHint`/`destructiveHint` annotations):

- `list_suppressions` (account; paginated; readOnly)
- `delete_suppression` (account; destructive+idempotent)
- `list_agent_suppressions` (readOnly)
- `create_agent_suppression` (idempotent)
- `delete_agent_suppression` (destructive+idempotent)

All five go in **ADMIN_TOOLS** in `tiers.ts`: the server enforces account
scope on every one of them (agent list/create/delete are account-scope-only
by design — an agent must not edit its own blocklist), so exposing them to
agent-scoped sessions would only surface guaranteed-403 tools.
`client.ts` gains the five thin wrappers (delegating to the SDK resources).
Register in `server.ts`; the existing tier-completeness test picks them up.

### Alternatives considered

- **Per-agent suppressions as an Outreach filter** — rejected: unsubscribe
  suppressions exist for addresses that were never outreach contacts; a
  filter over outreach rows silently misses them.
- **Account suppressions under Settings** — rejected: Settings is account
  configuration; suppression rows are address-shaped operational data, and
  Contacts is the account-wide address surface. Also keeps the two
  suppression views structurally parallel (both live where their scope
  lives).
- **One combined "Deliverability" page for both scopes** — rejected: blurs
  the scope distinction that the data model is explicitly built around
  (account blocks affect every agent; agent blocks only one), and the
  per-agent context already exists as inbox tabs.
- **Client-side state toggle instead of a `/contacts/suppressions` route** —
  rejected: not deep-linkable, and the contacts page is already 670 lines.

## Edge cases and failure handling

- **501 not_implemented** (self-host deployments without the deps): all three
  surfaces surface the server's error envelope message verbatim (existing
  error paths already do this); the web pages show it in the standard error
  banner instead of an empty table.
- **404 on remove** (already removed elsewhere): CLI exits EXIT.PERMANENT
  with the message; web shows the banner and refreshes.
- **Address encoding**: `encodeURIComponent` on every path segment (web);
  SDK handles it for CLI/MCP.
- **Invalid address on add**: server 400 `invalid_request` relayed as-is;
  no client-side pre-validation beyond `required`.
- **Pagination**: cursor + "Load more" append, same as outreach; MCP/CLI
  expose `limit`/cursor via the SDK AutoPager (CLI caps via `--limit`).
- **Beta stability**: agent-scope UI/CLI/MCP text carries the beta marker
  (chip / "(beta)" in tool titles) exactly like contacts; account scope is
  GA and unmarked.
- **Destructive default fails closed**: every remove goes through an explicit
  confirm (web `confirm()`, API `?confirm=DELETE` supplied by SDK).

## Scalability and extensibility notes

- Lists auto-grow with real bounce traffic; both web views paginate from
  day one and never fetch-all.
- The view-switcher on Contacts is deliberately minimal (two hardcoded
  links); promote it to a shared component only when a third account-level
  address view appears.
- If account-level manual suppression is added to the API later, the account
  page gains the same add-panel the agent page has — the layout anticipates
  it (actions slot in PageShell header).

## Verification strategy

Seams under test are the boundaries callers cross: page ↔ `/v1` fetch (web,
Jest with mocked fetch), command ↔ SDK (CLI, vitest), tool ↔ SDK wrapper
(MCP, vitest). Per surface:

- **Web**: Jest tests for both new pages (render, list, remove-confirm flow,
  add flow, error banner on 501) following `outreach/page.test.tsx`;
  `npm run lint` + `npm run build` (static export catches route mistakes).
- **CLI**: vitest for list/add/remove arg handling incl. `add` without
  `--agent` → usage error; `npm test --workspace @e2a/cli`.
- **MCP**: tier-completeness test (automatic), plus tool tests exercising the
  five wrappers; `npm test --workspace @e2a/mcp-server`.
- **Cross-cutting**: `git diff --exit-code api/openapi.yaml sdks/` proves the
  no-API-change claim; full workspace builds (SDK → CLI → MCP).
- **Manual**: against local stack (`make docker-up`), verify tab render, an
  add/remove round-trip, and the contacts view-switcher.

Most likely regressions: `detectTab` ordering (a new sub-path must be matched
before the messages default), and the 5-tab strip on mobile (checked via the
existing `responsive.test.tsx` pattern).

## Open questions

None blocking. One product follow-up deliberately out of scope: whether
account-level manual suppression (a `POST /v1/account/suppressions`) is
wanted — today `manual` at account level is a schema value with no API
writer.
