# Contacts system review and hardening

Status: implemented

Scope: the cumulative contacts stack in PRs #685, #689, #690, #699, #702,
and #704, rebased onto the current `main`.

## 1. Problem

The six stacked PRs establish the right domain split:

- `contacts` is account-scoped identity and provenance;
- `contact_engagements` is per-agent outreach state;
- suppression remains in the existing account and agent suppression tables;
- message activity supplies system-owned history; and
- `contact.due` wakes an agent without composing or sending on its behalf.

The cumulative implementation is not yet safe to ship as a complete product.
The review found correctness failures at concurrency and durability boundaries,
an outreach query that excludes never-contacted people, a bulk-import workflow
that cannot enroll the imported rows, and no first-class CLI, MCP, or dashboard
surface. The original design document also describes several behaviors that no
longer match the implementation.

## 2. Goals and non-goals

### Goals

1. Make the recommended outreach query return both never-contacted and stale
   contacts.
2. Make contact limits and contact `If-Match` checks correct under concurrency.
3. Persist each `contact.due` event atomically with claiming its schedule.
4. Avoid a burst of historical due events when a trashed agent is restored.
5. Let one import both create/update contacts and enroll them with an agent.
6. Expose the complete beta workflow through REST, both SDKs, CLI, MCP, and the
   dashboard.
7. Keep the API resource-oriented, tenant-safe, bounded, and evolvable.
8. Prove the critical flows with database-backed and end-to-end tests.

### Non-goals

- e2a does not become a CRM, sequence engine, or autonomous sender.
- Stages remain opaque strings. Clients must not assign semantics or colors to
  particular stage values.
- Sending ordinary mail does not auto-enroll recipients.
- Receiving mail from a stranger does not auto-enroll the sender.
- Import parsing stays at the client edge; the API continues to accept
  normalized JSON rows rather than multipart CSV.

## 3. API decisions

### 3.1 Keep the resource model

The existing paths remain:

- account scope: `/v1/contacts`
- agent scope: `/v1/agents/{email}/contacts`
- wake-up event: `contact.due`

The account surface requires account credentials. The agent surface is usable by
an account credential or by the named agent's credential. An agent-scoped
response embeds the contact fields needed to act, so it never requires an
account-wide contact read.

### 3.2 Correct filter semantics

`last_outbound_before=t` means "no outbound has ever been recorded, or the last
outbound was at or before `t`." SQL `NULL` is therefore included. Combined with
`replied=false`, `suppressed=false`, and `next_action_before`, this is the
single-call first-touch/follow-up query promised by the API.

All filter values remain pinned in the opaque cursor. Changing a filter while
reusing a cursor remains a validation error.

### 3.3 Atomic optimistic concurrency

The contact `PATCH` keeps optional `If-Match` for compatibility and easy
single-writer use. When supplied, the version predicate is part of the `UPDATE`
statement; it is not a read followed by an unconditional write. A stale value
returns `412 Precondition Failed`.

Agent engagement writes receive the same optional protection: `GET` and
successful `PUT` return an `ETag`, and `PUT` accepts `If-Match`. Without the
header, upsert semantics remain convenient. With it, the engagement must exist
and match atomically.

### 3.4 Bounded import plus optional enrollment

The import JSON request gains:

- `agent_email` (optional)
- `stage` (optional, valid only with `agent_email`)

When `agent_email` is present, every valid row that resolves to a contact is
enrolled with that owned, live agent in the same database transaction as the
import. Existing engagement state is preserved. `stage` initializes only a new
engagement; it never rewrites an existing one.

This applies to contacts reported as created, updated, or skipped because they
already exist. Invalid, duplicate-in-batch, and over-limit rows are not
enrolled. Per-row suppression receipts consider both account and selected-agent
suppression.

The endpoint accepts at most 1,000 rows and a 20 MiB body. Twenty MiB is large
enough for 1,000 individually valid 16 KiB metadata objects plus JSON overhead;
a 1 MiB cap would contradict the documented row limits.

### 3.5 Contact cap

All paths that may create a contact—manual create, import, and engagement
upsert—take the same transaction-scoped advisory lock keyed by account before
checking capacity. The lock is held through commit. Existing-contact updates do
not consume headroom, and a full account can still re-import or enroll an
existing contact.

This serializes only contact creation within one account. Accounts remain
independent and reads remain lock-free.

### 3.6 Errors

The existing structured error envelope is retained. Important mappings are:

- invalid field combinations or values: `400 invalid_request`
- contact cap reached: `400 contact_limit_reached`
- absent tenant-scoped resource: `404 not_found`
- stale `If-Match`: `412 precondition_failed`
- oversized body: `413 payload_too_large`

No response distinguishes "owned by another tenant" from "does not exist."

## 4. Durable `contact.due`

Claiming an engagement and writing its deterministic `webhook_events` row form
one PostgreSQL transaction:

1. begin;
2. select and update up to 200 due engagements with
   `FOR UPDATE SKIP LOCKED`;
3. write each deterministic event through `webhookpub.Outbox.PublishTx`;
4. commit.

Any query, serialization, outbox, or fan-out-enqueue error rolls the whole
transaction back. River retries the sweep, and the schedule remains claimable.
The deterministic event ID and outbox uniqueness make retry safe. This is
at-least-once wake-up delivery without a schedule-to-outbox loss window.

The transaction belongs to the `contactdue` module because it is the invariant's
owner. `identity` supplies the transaction-aware due query; `webhookpub`
supplies the transaction-aware outbox write. The generic best-effort
`webhookpub.Publisher` is not used here.

Suppressed addresses and trashed agents remain fail-closed exclusions. The agent
join includes `user_id` as well as the globally unique agent ID to keep tenant
scope explicit.

On restore, past-due schedules are acknowledged by setting
`notified_next_action_at = next_action_at`. They remain visible to a pull query,
but restoring an old agent cannot immediately enqueue hundreds of historical
wake-ups. A future reschedule clears the equality and produces a new event.

## 5. Client experience

### TypeScript and Python

The generated request fields and optional concurrency headers flow from
OpenAPI. The existing ergonomic `contacts` namespaces remain the canonical SDK
entry points.

### CLI

`e2a contacts` covers account identity operations and bulk import.
`e2a contacts outreach` covers the named agent's engagement list/get/set/delete.
CSV parsing handles UTF-8 BOMs, RFC 4180 quotes, explicit column mapping, and a
preview/dry-run path. `--agent` and `--stage` map to import enrollment. Existing
exit codes are unchanged.

### MCP

Runtime/agent tiers receive only agent-scoped outreach tools. The admin tier
also receives account contact CRUD, import, and import reversal. Tool
descriptions state that e2a stores state and emits wake-ups but never composes
or sends a follow-up. Schemas remain strict and paginated list tools expose the
opaque cursor.

### Dashboard

The dashboard adds:

- an account Contacts page with search/filter, create/edit/delete, import
  preview, agent enrollment, row-level results, and reversible batch receipt;
- an inbox Outreach tab with due/replied/suppressed filters and inline editing
  of stage and next action.

Suppressed rows are visibly non-sendable. Dates use the user's local time while
API values remain UTC. Stage is rendered as user data, not a hard-coded funnel.
Empty, loading, error, partial-import, and stale-write states all have explicit
copy and recovery actions.

## 6. Failure and edge cases

- Two concurrent creates at the last slot yield one success and one limit
  response.
- Engagement upsert cannot bypass the contact cap.
- Two writers using one ETag yield one success and one `412`.
- An outbox failure leaves the schedule unclaimed and creates no event.
- A retry after commit deduplicates by deterministic event ID.
- An import duplicate reports its original row and is not enrolled twice.
- A skipped existing contact is still enrolled when requested.
- Agent-scoped suppression marks the corresponding import result suppressed.
- Deleting a contact or engagement never deletes suppression.
- Import reversal keeps rows with message history and reports retained count.
- Every import has a durable batch receipt, even when it only updates existing
  contacts. Reversal also removes only the per-agent enrolments that batch
  created and reports `engagements_deleted`.
- Restoring a trashed agent creates no wake-up backlog; the pull query still
  shows due rows.
- `last_outbound_before` includes `NULL` and old timestamps, and excludes only
  newer timestamps.

## 7. Scale and evolution

The current default cap of 10,000 contacts per account bounds scans and metadata
storage. Keyset pagination and filter-pinned cursors avoid offset degradation.
The due sweep remains bounded at 200 and can scale horizontally because rows are
locked with `SKIP LOCKED`. Computed message counts are limited to the selected
page and cover only successfully delivered/authenticated activity since
enrollment; materialized timestamps remain idempotent under duplicate activity
hooks. Releasing an authenticated inbound review hold updates its engagement in
the same transaction as the review transition.

If computed counts become a measured bottleneck, a later design may add an
event-derived projection. It must preserve one authoritative source and ship
with reconciliation before becoming API-visible. This hardening pass does not
restore the removed drift-prone counters.

## 8. Verification

Implementation is complete only when:

- focused tests demonstrate each bug before and after its fix;
- migration numbering and idempotency checks pass;
- OpenAPI, TS generated code, and Python generated code are fresh;
- Go unit/integration/e2e, TS SDK, Python SDK, CLI, MCP, web, and design-system
  gates pass;
- a real database-backed due sweep creates the durable event and an injected
  outbox failure rolls it back;
- the production-shaped contact/outreach e2e exercises import enrollment, the
  first-touch query, activity hooks, suppression, and due wake-up; and
- the dashboard is verified at runtime for desktop and narrow layouts with a
  clean console.
