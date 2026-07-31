# Contacts and outreach state

Status: **implemented (beta)**. The post-implementation review and corrections
are recorded in
[`2026-07-27-contacts-review-hardening.md`](2026-07-27-contacts-review-hardening.md);
where the documents differ, the hardening decision is authoritative.
Date: 2026-07-24
Surface: `/v1/contacts`, `/v1/agents/{email}/contacts`, `contact.due` webhook event.
Stability: **beta** on every operation (see §3.4).

---

## 1. Problem statement

An agent doing outreach at volume — the motivating case is investor outreach
during a raise — has no durable memory of *who it is talking to*. e2a stores
everything message-first: `messages` rows, a derived conversation view, and
labels. Nothing is indexed by recipient.

Three concrete failures follow:

1. **No recipient-indexed query.** "Which of my 300 investors haven't replied
   in 5 days and need touch 2?" requires listing every message and grouping
   client-side. The data exists — `resolveConversationID`
   (`internal/relay/server.go:838`) already stitches an external reply back to
   the outbound thread via `In-Reply-To`/`References` — but it is only
   reachable per-message.
2. **No liveness.** An outreach agent is a session. It is not running at 09:00
   Tuesday to send touch 2. Nothing in e2a can wake it.
3. **No place to put per-recipient state.** Stage, next action, and
   agent-authored notes have nowhere to live that survives across
   conversations, so every deployment reinvents a sidecar store.

There is also no way to get a list of people *into* e2a at all: contacts can
only come into existence as a side effect of mail.

### Success criteria

- `GET /v1/agents/{email}/contacts?replied=false&next_action_before=<now>`
  answers the touch-2 question in **one request**, served by an index, with no
  client-side aggregation.
- A 500-row CSV of investors is importable in **one request**, with per-row
  outcomes, and re-importable without damaging outreach state.
- An agent that is not running receives a `contact.due` webhook when an
  engagement's `next_action_at` passes.
- Contact keying is **provably identical** to suppression keying — enforced by
  a test, not by convention (§6.2).

---

## 2. Goals and non-goals

### Goals

- A durable, account-level record of a person e2a corresponds with.
- Per-agent outreach state (stage, next action) on the relationship, not the person.
- Bulk import with per-row results and a reversible batch.
- A due-event that wakes an agent to compose the next touch.
- Derived engagement facts e2a alone can compute: reply status, last contact,
  counts, suppression state.

### Non-goals (explicit)

- **Companies, deals, pipelines, opportunities.** Not a CRM. Business objects
  belong in the agent's own store (AgentDrive).
- **Custom field definitions.** No account-wide custom-field schema registry of
  the kind marketing platforms ship. Unmodeled data goes in opaque `metadata`.
- **Segments / lists / audiences.** The per-agent engagement *is* the grouping
  axis (§3.1). No second many-to-many.
- **Enrichment.** No vendor lookups, no auto-filled firmographics.
- **Scheduled or sequenced sending.** e2a never sends on a timer and never
  stores a follow-up chain. See §3.5 — this is the load-bearing product line.
- **Async import jobs.** Bounded-synchronous only for v1 (§3.3).

---

## 3. Relevant context, constraints, and proposed design

### 3.0 Constraints inherited from the codebase

| Constraint | Source | Consequence |
|---|---|---|
| Cursor pagination, `{items, next_cursor}` | `internal/httpapi/pagination.go:22` | Every list uses `Page[T]` + `PageParams`; new resources needing pinned filters define their own cursor struct |
| Address normalization | `identity.NormalizeMailboxAddress` (`internal/identity/email.go:28`) | Parses RFC 5322 mailbox form; **this is what suppression lookups use** |
| Two-level consent | `suppressions` + `agent_suppressions` (`migrations/068`) | The precedent this design follows exactly |
| Scope ceiling | `internal/httpapi/scope.go` | `requireAccountScope` vs `requireAgentAccess` — object-level authZ |
| Error envelope | `internal/httpapi/error_catalog.go` | Machine `code` + human `message`; reuse existing codes where they fit |
| Destructive ops | `DeleteConfirm` (`?confirm=DELETE`) | Applies to contact and import deletion |
| Event catalog | `webhookpub.AllEventTypes` (`internal/webhookpub/event.go:97`) | New events must be added there **and** to the request-side enum tags; `TestWebhookEventEnumMatchesCatalog` gates drift |
| Beta marking | `beta()` (`internal/httpapi/stability.go:61`) | Post-1.0 surface ships beta |
| Migrations | `AGENTS.md` | Idempotent, non-destructive, sequentially numbered, forward-only |

**Assumption (unconfirmed, load-bearing):** real N is hundreds to low
thousands of contacts per account, not 10^5. §3.3 and §3.2 both bet on this.
If it is wrong, import must go async and the derived-field strategy needs
revisiting — but neither changes the resource shape.

### 3.1 The core modeling decision: contact identity vs engagement state

**Contact identity is account-level. Contact state is per-(contact, agent).**

Prior art settles this. One established transactional-email provider originally
nested contacts under a grouping resource and **later reversed it to a top-level
contact keyed by email**, because a contact could then belong to only one group
— so putting the same person in two groups meant creating a duplicate contact
with the same address. Another has always kept contacts top-level with group
membership as an optional many-to-many array. Strictly transactional providers
build no contact entity at all, only per-stream suppressions.

Their nesting parent was a *grouping* (many-to-many by nature, so nesting was a
plain modeling error). Ours would be an *agent* — a sending identity, more like
a sub-account. Different mistake, **identical failure mode**: with contacts
nested under agents, when `raise@` and `jace@` both email the same partner you
get two rows for one human, "has anyone on our side contacted this fund?" needs
a cross-agent scan, reply history splits, and per-contact counters double-count.
A three-cofounder raise hits this immediately.

The counter-pressure is real and must be preserved: **consent is per-relationship.**
An investor unsubscribing from `raise@` has not unsubscribed from `support@`.
e2a already encodes this — `agent_suppressions` is keyed `(user_id, agent_id, address)`.

The split resolves both, and is *the same two-level shape the codebase already
chose for suppressions, for the same reason*. This design introduces no new
architectural pattern.

#### Alternatives considered

| Alternative | Why it loses |
|---|---|
| **Contacts nested under agent only** | Duplicate identity per agent; an established provider shipped this shape and later reversed it. Cross-agent collision detection — the thing a raise needs most — becomes impossible. |
| **Contacts account-level only, no engagement row** | Nowhere to put per-agent stage/next-action; forces flattening consent, which would regress the correct behavior `agent_suppressions` already has. |
| **No entity — labels + a recipient-facet query** | Genuinely viable, ~70% of value for ~20% of cost (`stage:contacted` labels, `conversation_id` as investor key). Loses: identity before first message (so **import is impossible**), and the `replied` rollup stays a per-request aggregate over a prod-sized table. Import is what kills it. |
| **Third resource: segments/lists** | A second many-to-many with no demand behind it. The engagement row already groups. Deliberately deferred; §5 keeps the door open. |

### 3.2 Data model

Two tables. Migrations `079_contacts.sql`, `081_contact_engagements.sql` (081 because 080 was taken on main while this was in flight).

```sql
-- 079_contacts.sql
CREATE TABLE IF NOT EXISTS contacts (
    id              TEXT PRIMARY KEY,              -- cnt_<random>
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    address         TEXT NOT NULL,                 -- identity.NormalizeMailboxAddress
    display_name    TEXT NOT NULL DEFAULT '',
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    source          TEXT NOT NULL CHECK (source IN ('import','manual','inbound')),
    import_batch_id TEXT,                          -- NULL unless source='import'
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, address)
);
CREATE INDEX IF NOT EXISTS contacts_list_idx   ON contacts (user_id, created_at DESC, id);
CREATE INDEX IF NOT EXISTS contacts_batch_idx  ON contacts (user_id, import_batch_id)
    WHERE import_batch_id IS NOT NULL;
```

```sql
-- 081_contact_engagements.sql
CREATE TABLE IF NOT EXISTS contact_engagements (
    id              TEXT PRIMARY KEY,              -- eng_<random>
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    contact_id      TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    agent_id        TEXT NOT NULL,                 -- no FK; see note
    address         TEXT NOT NULL,                 -- denormalized, matches agent_suppressions

    -- agent-owned
    stage           TEXT NOT NULL DEFAULT '',      -- opaque; e2a never enumerates values
    next_action_at  TIMESTAMPTZ,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- e2a-derived (materialized; see below)
    first_outbound_at TIMESTAMPTZ,
    last_outbound_at  TIMESTAMPTZ,
    last_inbound_at   TIMESTAMPTZ,
    outbound_count    INTEGER NOT NULL DEFAULT 0,
    inbound_count     INTEGER NOT NULL DEFAULT 0,
    last_conversation_id TEXT NOT NULL DEFAULT '',

    -- due-event dedupe: the next_action_at value a contact.due was already
    -- emitted for. Fire only when it differs from next_action_at, so writing a
    -- new next_action_at re-arms automatically. NULL = never notified.
    notified_next_action_at TIMESTAMPTZ,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, agent_id, contact_id)
);
CREATE INDEX IF NOT EXISTS ce_list_idx ON contact_engagements (user_id, agent_id, created_at DESC, id);
CREATE INDEX IF NOT EXISTS ce_due_idx  ON contact_engagements (next_action_at)
    WHERE next_action_at IS NOT NULL AND notified_next_action_at IS DISTINCT FROM next_action_at;
```

`agent_id` carries no FK, matching `agent_suppressions`' deliberate choice — but
for a *different* reason and with a different lifetime rule, see open question
Q1. `replied` is **not stored**; it is computed in the view as
`last_inbound_at IS NOT NULL AND last_inbound_at > first_outbound_at`, and
filtered in SQL by the same expression.

#### Derived fields: materialized, with reconciliation

Computing counters on read means a correlated aggregate over `messages` — a
prod-sized table — for every row of every list request. That defeats the one
query the resource exists to serve.

So they are materialized, updated at seams that already exist:

- **Outbound:** the `messagelifecycle` transition recorder — already the
  canonical place terminal outcomes land.
- **Inbound:** `internal/relay`, immediately after `resolveConversationID`,
  which already has the agent, the sender address, and the conversation.

Materialized counters drift (this is exactly the class `AGENTS.md`'s
schema-change rule warns about). Mitigation: a **reconciliation sweep** in
`internal/janitor` recomputes counters from `messages` for engagements touched
since the last run and corrects mismatches, emitting a metric on every
correction. Non-zero drift is a bug signal, not routine.

Precedent for materialized rollups already exists: `usage_summaries`,
`domain_send_counters`.

### 3.3 HTTP surface

Ideal call site first — the outreach loop, which is what the whole design is for:

```ts
// Who needs touch 2?
const due = await e2a.agents("raise@fund.com").contacts.list({
  replied: false,
  next_action_before: new Date(),
});

for (const c of due.items) {
  await e2a.agents("raise@fund.com").send({
    to: c.address,
    subject: `Following up — ${c.contact.metadata.fund}`,
    text: compose(c),                       // the agent writes it
  });
  await e2a.agents("raise@fund.com").contacts.update(c.address, {
    stage: "touch2",
    next_action_at: addDays(new Date(), 5),
  });
}
```

No comment needed to explain it. That is the bar.

#### Account-level — identity

| Method | Path | Notes |
|---|---|---|
| `GET` | `/v1/contacts` | List. `PageParams`; filters `source`, `import_batch_id`, `created_after/before`. Sort `created_at DESC, id`. `Cache-Control: no-store`. |
| `POST` | `/v1/contacts` | Create one → **201** + `Location`. Accepts `Idempotency-Key`. |
| `GET` | `/v1/contacts/{address}` | Fetch by address. `ETag`. |
| `PATCH` | `/v1/contacts/{address}` | Partial update of `display_name`, `metadata`. `If-Match` honored (STRONG comparison per RFC 9110 §13.1.1 — a `W/` weak validator never matches; a present-but-empty header is 400, not an unconditional write) → 412. |
| `DELETE` | `/v1/contacts/{address}` | Requires `?confirm=DELETE`. Cascades engagements. **Does not clear suppressions.** |
| `POST` | `/v1/contacts/import` | Bulk upsert (§3.3.1). |
| `DELETE` | `/v1/contacts/imports/{batch_id}` | Reverse an import. `?confirm=DELETE`. |

**Path key is `{address}`, not `{id}`**, matching `/v1/account/suppressions/{address}`.
This makes the natural key the URL key and lets a caller go straight from a CSV
row to a resource. Cost: `@` requires URL encoding — see §6.3, this has bitten
us before.

`POST /v1/contacts` (single, returns an object) and `POST /v1/contacts/import`
(bulk, returns per-item results) are deliberately separate endpoints rather than
one polymorphic endpoint, so neither response shape varies by request shape.

#### Agent-level — engagement

| Method | Path | Notes |
|---|---|---|
| `GET` | `/v1/agents/{email}/contacts` | **The outreach query.** Filters: `stage`, `replied`, `suppressed`, `next_action_before`, `last_outbound_before`. Embeds contact identity. |
| `GET` | `/v1/agents/{email}/contacts/{address}` | One engagement. |
| `PUT` | `/v1/agents/{email}/contacts/{address}` | Enroll/upsert agent-owned fields. Idempotent — PUT is correct. Creates the contact if absent (`source='manual'`). |
| `DELETE` | `/v1/agents/{email}/contacts/{address}` | Un-enroll. `?confirm=DELETE`. **Deletes neither the contact nor any suppression.** |

The list response **embeds the contact** rather than returning a contact ref:

```json
{
  "items": [{
    "agent_email": "raise@fund.com",
    "address": "partner@fund.vc",
    "stage": "touch1",
    "next_action_at": "2026-07-29T09:00:00Z",
    "replied": false,
    "suppressed": false,
    "first_outbound_at": "2026-07-24T09:00:00Z",
    "last_outbound_at": "2026-07-24T09:00:00Z",
    "last_inbound_at": null,
    "outbound_count": 1,
    "inbound_count": 0,
    "last_conversation_id": "raise-2026-a16z",
    "metadata": {},
    "contact": {
      "address": "partner@fund.vc",
      "display_name": "A. Partner",
      "metadata": {"fund": "Example Capital", "check": "1-3M"},
      "source": "import"
    },
    "created_at": "2026-07-24T09:00:00Z",
    "updated_at": "2026-07-24T09:00:00Z"
  }],
  "next_cursor": null
}
```

Embedding is not just ergonomics — it is what makes the scope split work (§3.4):
an agent-scoped credential can read its own engagements including the identity
fields it needs to compose, without being granted account-wide contact reads.

#### 3.3.1 Import

```http
POST /v1/contacts/import
Idempotency-Key: import-investors-2026-07-24
{
  "contacts": [
    {"address": "A. Partner <partner@fund.vc>", "display_name": "A. Partner",
     "metadata": {"fund": "Example Capital"}},
    {"address": "not-an-email"}
  ],
  "agent_email": "raise@fund.com",     // optional: also create engagements
  "stage": "imported",                 // optional: initial stage on those engagements
  "on_conflict": "merge"               // merge (default) | skip
}
```

`stage` exists so the very first outreach query is expressible. Without it,
imported engagements land at `stage: ""` and "everyone imported, never emailed"
has no clean filter — exact-matching an empty string in a query param is a poor
interface. One optional field beats adding an `outbound_count=0` query
dimension. Ignored when `agent_email` is absent.

**200** (not 202 — synchronous), always per-item results:

```json
{
  "batch_id": "imp_a1b2c3",
  "created": 1, "updated": 0, "skipped": 0, "failed": 1,
  "results": [
    {"index": 0, "address": "partner@fund.vc", "status": "created", "suppressed": false},
    {"index": 1, "address": null, "status": "failed",
     "code": "invalid_recipient", "message": "not a valid email address"}
  ]
}
```

Decisions and why:

- **Synchronous, capped at 1,000 items / 20 MiB.** Marketing platforms go async
  (202 + a job id, tens of thousands per request) because they serve bulk
  mailing lists. Our N is hundreds. Sync skips the entire job + status-polling +
  partial-progress surface. Over the cap → **413** `payload_too_large`; client
  paginates. Going async later adds an endpoint without changing this one.
- **JSON in the API; CSV parsed at the edge.** Server-side CSV means column
  mapping, encoding detection (spreadsheet BOM, latin-1), and quoting edge cases
  in a mail gateway. CLI auto-detects `email`/`name` and exposes
  `--email-column` / `--name-column`; the dashboard parses in-browser and
  provides a column-preview UI.
- **Per-item results, never all-or-nothing.** Row 37 malformed → 499 land.
- **`Idempotency-Key`** reusing `internal/httpapi/idempotency.go` (today
  sends-only; this extends a known shape) so a retry after timeout can't double-create.
- **`on_conflict: merge` touches identity + metadata only.** It must never
  reset `stage`, `next_action_at`, or any derived field — otherwise fixing one
  typo rewinds the whole pipeline. `replace` is not offered in v1.
- **Unknown metadata keys accepted**, capped (Q2). Investor CSVs carry
  genuinely useful columns (fund, check size, warm-intro path) that e2a should
  never model.

**Reversal (DELETE `/v1/contacts/imports/{batch_id}`) is defensive, not
tag-driven.** `import_batch_id` on both tables is origin provenance: it records
which batch created the row and never moves (a merge re-import leaves it
pointing at the original batch). Because the tag alone cannot distinguish
"still just an import artifact" from "the account has since built on this",
the reversal removes a tagged row only when it is verifiably untouched:

- `updated_at = created_at` — no PATCH, upsert, activity record, or due
  notification has ever landed (every mutation path sets `updated_at`), so
  even stale provenance cannot cause data loss;
- an engagement must additionally carry no derived wire activity and no
  message history for that agent/address;
- a contact must additionally have no message history and **no surviving
  engagement** — batch-created untouched enrolments are deleted first, so any
  engagement still present (one created independently of the import, or one
  edited since) retains the contact. The guard decides; the
  contacts→engagements `ON DELETE CASCADE` is never relied upon, because
  reaching it with a live engagement would silently destroy that state.

Anything failing a check is retained and counted, so
`contacts_deleted + contacts_retained` accounts for every batch-created
contact that still exists at reversal time (a contact deleted by other means
beforehand drops out of the accounting).

### 3.4 Authorization and scope

| Surface | Guard | Rationale |
|---|---|---|
| `/v1/contacts*` | `requireAccountScope` | Account-wide identity. An agent-scoped credential reading every contact the account owns is cross-agent visibility — a privilege escalation. **403.** |
| `/v1/agents/{email}/contacts*` | `requireAgentAccess` | The agent runs its own outreach loop. This is runtime state the agent owns. |

This **deliberately diverges** from `agent_suppressions`, which is
account-scoped-only ("Account-scoped credentials only"). Justification:
suppressions are *consent administration* — a runtime credential must not be
able to un-suppress itself. Engagements are *operational state* the agent
authors. The divergence is safe precisely because the two are separate tables:
an agent can move its own `stage`, and cannot touch consent.

All operations ship `beta()` — new post-1.0 surface, matching
`agentSuppressionBetaDescription`. Additive, so `openapi-compat-check` passes.

### 3.5 `contact.due` — waking the agent

New event type in `webhookpub.AllEventTypes` **and** `ExperimentalEventTypes`:
`contact.due`.

Namespace: `contact.` is a fourth namespace alongside `email.`, `domain.`,
`agent.`. Justified because it leaves room for `contact.replied` — the obvious
next event — where `agent.contact_due` would not. See Q4.

Mechanics, reusing `internal/janitor`'s existing River periodic sweep:

1. Sweep selects engagements where `next_action_at <= now()` and
   `notified_next_action_at IS DISTINCT FROM next_action_at` (the partial index `ce_due_idx`).
2. **Skips, fail-closed, on either condition:**
   - the address is suppressed for that agent — a due-event on a suppressed
     contact is an invitation to send mail the recipient opted out of;
   - the agent is in trash (`agent_identities.deleted_at IS NOT NULL`) — a
     deleted agent must not keep emitting due-events for its 30-day retention
     window (see Q1).
3. Publishes `contact.due` with the embedded engagement + contact payload.
4. Sets `notified_next_action_at = next_action_at` in the same transaction.

Re-arming: writing a new `next_action_at` makes `notified_next_action_at` stale again, so
the next sweep fires once more. Exactly-once per distinct `next_action_at`
value; at-least-once overall, consistent with the rest of the event system.

**The line this design holds:** e2a stores state and wakes the agent. **The
agent decides and composes.** e2a never fires a pre-written follow-up on a
timer and never stores a sequence definition. Cross that line and e2a becomes a
cold-outreach sequencer — a marketing-automation product with a spam-abuse
surface, a shared-reputation problem to police, and a different roadmap. Staying
on this side also keeps every message genuinely agent-authored, which is the
only honest posture for investor email.

---

## 4. Edge cases and failure handling

| Case | Behavior |
|---|---|
| Display-name form in import (`"Jane" <j@f.com>`) | Normalized via `NormalizeMailboxAddress`. **The #1 real-world CSV shape.** |
| `jane+vc@gmail.com` vs `jane@gmail.com` | Distinct contacts. No plus-tag or dot folding — deliberate, documented. |
| Import contains a suppressed address | Contact created, engagement created, **`suppressed: true` in the result.** Marked, not silently dropped — the count stays honest. |
| Re-import after unsubscribe | Suppression is in `agent_suppressions`, untouched by import. **Cannot resurrect sendability.** The single most important invariant here. |
| Re-import corrected CSV | Merges identity + metadata. `stage` / `next_action_at` / derived fields untouched. |
| Duplicate addresses within one import body | First wins, subsequent → `status: "skipped"`, `code: "duplicate_in_batch"`. |
| `DELETE` engagement | Contact survives. Suppression survives. Un-enroll ≠ un-suppress. |
| `DELETE` contact | Cascades engagements. Suppressions survive (they key on address, not contact_id). |
| Import batch delete | Only contacts that batch **created** and that have **no message history**. Others → reported as retained with a reason. |
| `next_action_at` in the past on write | Accepted; fires on the next sweep. No backfill storm — one event per engagement. |
| Agent moved to trash | Engagements survive (restore must return outreach state). Due sweep **skips** them for the whole retention window. |
| Agent hard-deleted (purged) | Engagements purged in the same janitor sweep as the agent. |
| Agent recreated at the same address | Inherits suppressions (consent, by design) but **no engagements** — they were purged. Prevents a resurrected campaign firing touch 4 at investors it never contacted. |
| Agent restored from trash with past-due engagements | Backlog is not push-fired; `notified_next_action_at` is set on restore so the agent pulls it via `?next_action_before=now` (Q1 secondary). |
| Derived counters drift | Counts are computed from message history on read, so no mutable counter can drift. |
| Inbound from an unknown address | Auto-creates a contact with `source='inbound'` — **only if** the address already has an engagement with that agent. Otherwise no contact is created; inbound mail from strangers must not silently populate a contact list. |
| Concurrent `PATCH` on the same contact | `ETag` / `If-Match` → 412. |
| Import over cap | 413 `payload_too_large`. |
| Malformed cursor | 400 `invalid_cursor`, existing behavior. |

Fail-closed defaults: due-events skip suppressed contacts; import never sends;
`on_conflict` defaults to the non-destructive `merge`; unknown-address inbound
does not auto-create.

---

## 5. Scalability and extensibility

**Grows:** `contact_engagements` row count (contacts × agents) and the due
sweep's scan. Both are indexed for the access pattern; `ce_due_idx` is partial
so it stays small regardless of total engagements.

**Deliberately narrow for v1:** no segments, no `sort=` param (default
`created_at DESC, id`; the filters carry the load), no async import, no
`replace` conflict mode, `stage` is an opaque string with no state machine.

**Made easier later, not harder:**
- Segments/lists — a third table joining `contacts`; the account-level identity
  is already the right anchor, which is precisely what nesting under agents
  would have foreclosed.
- Async import — a new endpoint returning 202 + `job_id`; the sync one keeps its contract.
- `contact.replied` and `contact.stage_changed` — the `contact.` namespace exists.
- `sort=next_action_at` — additive query param.
- Cross-agent rollup on `GET /v1/contacts` (`engagement_count`, `agents[]`) —
  additive response fields, and the reason "has anyone contacted a16z?" is
  answerable at all.

**Hyrum's Law watch:** `stage` is opaque today. The moment the dashboard renders
known stage values with colors, those strings become a de facto contract.
Keep stage rendering generic, or document a reserved vocabulary before shipping UI.

---

## 6. Verification strategy

Seams, fewest that matter — the boundaries callers actually cross.

### 6.1 HTTP handler seam (`internal/httpapi`) — primary

Per changed endpoint, the four-test minimum from `api-design`:
happy path; validation rejection; **authZ denial** (agent-scoped credential →
`/v1/contacts` → 403; agent A's credential → agent B's contacts → 403);
response-shape regression pinning the JSON.

Plus tenant isolation: user A cannot read, patch, or delete user B's contact,
asserted at the handler.

### 6.2 Normalization parity — the correctness test that matters most

A test asserting the contact storage key and the suppression lookup key are
produced by **the same function**. If they diverge, contacts appear sendable in
the list and get suppressed at send time (or worse, the inverse). Table-driven
over: display-name form, mixed case, surrounding whitespace, plus-tags, unicode.

### 6.3 Real-router encoded-address test

`{address}` in the path means `@` and `+` arrive percent-encoded. We have
shipped a routing bug of exactly this class before (the WS-404 incident:
framework-bypass routes with encoded params need **real-router** tests, not
handler-level ones). Every address-keyed route gets a test through the actual
chi/Huma router with `partner%40fund.vc` and a `+`-tagged address.

### 6.4 Double-send safety net (§8.3.1 pattern A)

The property that makes the recommended query safe: `last_outbound_at` **MUST**
be updated by the send path itself, independently of any client `PUT`. Test:
send to a contact, never call `PUT`, assert `last_outbound_at` advanced and the
contact drops out of `?last_outbound_before=<now-interval>`. This is what
prevents a lost client write from turning into a duplicate email.

### 6.5 Import (DB-backed)

Partial failure (row 37 bad, rest land); idempotency replay returns the
identical batch without double-creating; suppressed address marked not dropped;
re-import does not clobber `stage`/`next_action_at`; duplicate-in-batch;
over-cap 413; batch delete spares contacts with message history.

### 6.6 Derived fields and due-event

Reconciliation test: mutate counters out from under the row, run the janitor
sweep, assert correction + metric. Due-event: fires once per `next_action_at`;
re-arms on rewrite; **does not fire for a suppressed contact**; **does not fire
for a trashed agent**; does not re-fire across sweeps.

**Agent lifecycle (Q1) — the highest-value test in this design.** Because
`agent_id` is the address, a recreated agent silently inherits anything not
purged, and the blast radius is real outbound mail. Assert end to end:
create agent → engagements with future `next_action_at` → trash the agent →
sweep emits nothing → restore → engagements intact and past-due backlog not
push-fired → trash again → janitor purge past retention → **recreate the same
address** → zero engagements, and suppressions still present. That last pair of
assertions in one test is what pins the deliberate asymmetry between consent
and operational state.

### 6.7 Pipeline gates (mandatory, per `AGENTS.md`)

`make generate` → commit `api/openapi.yaml` + both `generated/` trees.
`make spec-check`, `make generate-sdk-check`, `make openapi-compat-check`.
`TestWebhookEventEnumMatchesCatalog` will fail until `contact.due` is added to
`AllEventTypes` **and** every request-side enum tag — that gate is doing its job.
Coverage floors in `.testcoverage.yml` ratchet up for any touched package.

### 6.8 Parity surfaces

Go handler, OpenAPI, TS SDK, Python SDK, CLI (`e2a contacts …`), MCP tools,
**and the web dashboard** — the PR template omits web, so it must be checked
manually.

### Most likely regressions

1. Normalization divergence between contact key and suppression key (§6.2).
2. Import clobbering outreach state on re-import.
3. Due-events firing for suppressed contacts.
4. Counter drift going unnoticed without the reconciliation metric.
5. Encoded-address routing 404s (§6.3).

---

## 7. Open questions

**Q1 — RESOLVED. Engagements are purged on agent hard-delete and survive trash.**

The key fact: **`agent_id` is the agent's email address**, not a random id.
`agent_identities.id` is the address (`migrations/001_init.sql:43`) and the
suppression lookup passes `NormalizeEmail(agentID)`
(`internal/identity/agent_suppressions.go:178`). That is *why* migration 068 can
claim consent "survives hard deletion and recreation of an address" — a
recreated `raise@fund.com` gets the same key, so its suppressions carry over.
Correct and deliberate for consent.

For engagements the identical property is a **misfire**, not a feature. Delete
`raise@fund.com` after a raise, recreate it a year later, and the new agent
inherits every stale engagement — `stage: "touch3"`, last year's
`next_action_at`, `replied: false`. `ce_due_idx` then fires `contact.due` across
a dead campaign and wakes the new agent to send touch 4 to investors it never
contacted. That is outbound mail to real people from a resurrected campaign, so
this is a correctness decision, not a hygiene one.

Resolution — three states, matching the existing trash lifecycle
(`migrations/063_soft_delete_trash.sql`, `identity.TrashRetention` = 30d):

1. **Agent in trash** (`agent_identities.deleted_at` non-NULL, restorable):
   engagements **survive untouched**. Restoring an agent mid-raise must give
   back its outreach state, not an inbox with amnesia. **But the due sweep must
   skip engagements whose agent is trashed** — otherwise a deleted agent keeps
   emitting `contact.due` for 30 days. This is a second fail-closed condition
   on the sweep alongside the suppression check (§3.5 step 2).
2. **Agent hard-deleted** (janitor purge past retention): engagements are purged
   **in the same janitor sweep that purges the agent**, not by a separate
   orphan collector — one place, no window where orphans are live.
3. **Recreation at the same address**: nothing to inherit, because purge already
   ran. Clean by construction rather than by a special case.

The contrast is the point and belongs in the `080` migration comment, mirroring
how `068` documents its own choice: **suppressions survive recreation (consent);
engagements do not (operational state).** Same no-FK, address-keyed shape;
opposite lifetime; both deliberate.

*Secondary, still open:* restoring an agent after 20 days in trash leaves a
backlog of past-due engagements that would all fire at once. Recommendation: on
restore, set `notified_next_action_at = next_action_at` for already-past-due rows so the
backlog is not *pushed*, and let the agent *pull* it via the normal
`?next_action_before=now` filter. Push for newly due, pull for a restored
backlog. One line in the restore path; prevents a 300-event thundering herd.

**Q2 — RESOLVED (pinned, revisable). `metadata` caps.**

Per object, on both `ContactView.metadata` and `ContactEngagementView.metadata`:

| Limit | Value | Rationale |
|---|---|---|
| Serialized size | **16 KB** | A wide investor CSV row is <2 KB; 8× headroom. |
| Key count | **50** | Widest realistic export is ~25 columns. |
| Key length | **128 bytes** | Spreadsheet headers are short; long keys signal misuse. |
| Value length | **4 KB** | Fits a warm-intro note or a short thesis blurb. |
| Nesting depth | **flat only** | Scalars and strings. Nested objects invite querying, which invites indexing, which is the CRM slope. |

Violation → 400 `invalid_request` with the offending key in `details`. In
import, this is a **per-item** failure (`status: "failed"`), not a whole-batch
rejection — one fat row must not sink 499 good ones.

These are deliberately generous and easy to raise later; lowering a shipped cap
is breaking, so start where we are unlikely to need to tighten.

**Q3 — RESOLVED. Import enrollment is opt-in.** `agent_email` is optional;
when present, every valid resolved row is enrolled transactionally and `stage`
initializes only a newly created engagement. Re-import never resets existing
outreach state. This preserves the import-then-distribute workflow.

**Q4 — RESOLVED. `contact.due`, opening a `contact.` namespace.**

The existing vocabulary reads two ways, which is why this looked ambiguous:

- `email.*` and `domain.sending_*` name a **subject** — the thing the event is about.
- `domain.suppression_added` / `agent.suppression_added` name a **scope**. They
  exist as a *pair* because one conceptual event (a suppression was added)
  genuinely occurs at two different scopes and a subscriber needs to tell them
  apart.

A due-event has only one scope, so the pairing rationale that justifies the
scope-prefixed names does not apply here. Subject-first is the dominant reading
and the one to follow.

The decisive argument is that **scope prefixes do not scale as namespaces in a
per-agent product** — nearly everything in e2a is agent-scoped. Take
`agent.contact_due` and its natural successors are `agent.contact_replied` and
`agent.contact_stage_changed`: a stuttering prefix, and `agent.` degrades into a
junk drawer holding suppressions, contacts, and whatever comes next. `email.`
and `domain.` stay coherent precisely because they name subjects.

Scope is not lost by this choice: webhook filters already key on `agent_ids`
(`migrations/023_webhooks.sql:44`), so per-agent subscription works regardless of
the namespace. That frees the namespace to do the one job a namespace is good at.

The asymmetry settles it. Choosing `contact.` costs nothing today and preserves
room; choosing `agent.contact_due` costs nothing today and forecloses a clean
grouping forever. Event names are the most conservative surface we ship —
consumers cannot be forced to upgrade — so take the option that keeps the door
open.

Stated plainly for future readers: this treats `agent.suppression_added` as the
**exception** (justified by its `domain.*` pair), not the rule. That is a
deliberate reading, not an oversight.

Implementation note: `contact.due` must be added to `webhookpub.AllEventTypes`,
to `ExperimentalEventTypes` (beta payload), **and** to every request-side enum
tag — `CreateWebhookRequest.events`, `UpdateWebhookRequest.events`, and the
test-webhook event. `TestWebhookEventEnumMatchesCatalog` fails until all copies
agree; that gate is the whole reason this stays consistent.

**Not shipping in v1:** `contact.replied` and `contact.stage_changed`. They
motivate the namespace but are additive later, and `email.received` already
covers the reply case for now.

**Q5 — Does `GET /v1/contacts` expose a per-agent rollup?** "Has anyone
contacted a16z?" is the collision question a raise most needs. Additive later,
but if it's the headline use case it may belong in v1.

**Q6 — Consent attestation on import.** A required "I have a basis to contact
these addresses" acknowledgment establishes intent in the record but stops
nothing determined. Leaning skip for v1; flagging the natural insertion point.

---

## 8. API reference (normative)

§3.3 gives the rationale; **this section is the contract**. `api/openapi.yaml`
is generated from the Huma handlers (`make spec`), so this is what the handlers
are written from — the YAML is downstream, not hand-authored.

### 8.1 Conventions (apply to every operation)

| Aspect | Rule |
|---|---|
| Auth | Bearer API key. Every operation authenticated; none public. |
| Stability | `beta()` on all operations. Additive to a 1.0 surface → `openapi-compat-check` passes. |
| Errors | Existing `ErrorEnvelope`: `{error:{code,message,details?}}`. Branch on `code`. |
| Lists | `PageParams` (`cursor`; `limit` 1–100, default 100) → `Page[T]` = `{items,next_cursor}`. `next_cursor: null` on last page. |
| Sort | Both lists: `created_at DESC, id`. Stable, no client-selectable sort in v1. |
| Cursors | Opaque, HMAC-signed. The engagement cursor **pins its filter set**; a continuation that changes filters → 400 `invalid_cursor`. |
| Timestamps | ISO-8601 UTC. |
| Headers | `X-Request-Id` on all responses. `Cache-Control: no-store` on all GETs (private, volatile). `ETag` on single-resource GETs, `If-Match` honored on `PATCH` → 412. `Location` on 201. `Retry-After` on 429. `X-Content-Type-Options: nosniff`. |
| Unknown fields | Rejected on writes → 400 `invalid_request`. |
| Address in path | Normalized via `identity.NormalizeMailboxAddress` before lookup. Must be URL-encoded by the caller. |

### 8.2 Schemas

**`ContactView`**

| Field | Type | Notes |
|---|---|---|
| `address` | string | Normalized. Natural key. |
| `display_name` | string | May be `""`. |
| `metadata` | object | Opaque to e2a. Caps per Q2. |
| `source` | string | `import` \| `manual` \| `inbound`. Open string in responses. |
| `import_batch_id` | string\|null | Set only when `source=import`. |
| `created_at` / `updated_at` | timestamp | |

**`ContactEngagementView`**

| Field | Type | Owner | Notes |
|---|---|---|---|
| `agent_email` | string | server | Self-describing outside its route. |
| `address` | string | server | |
| `stage` | string | **agent** | Opaque. e2a never enumerates values. |
| `next_action_at` | timestamp\|null | **agent** | |
| `metadata` | object | **agent** | Opaque. |
| `replied` | bool | server | Computed: `last_inbound_at > first_outbound_at`. |
| `suppressed` | bool | server | Joined from `agent_suppressions`. |
| `suppression_source` | string\|null | server | `unsubscribe` \| `manual` \| bounce/complaint origin. Null when not suppressed. |
| `suppression_reason` | string\|null | server | Free text from the suppression record. |
| `first_outbound_at` | timestamp\|null | server | |
| `last_outbound_at` | timestamp\|null | server | |
| `last_inbound_at` | timestamp\|null | server | |
| `outbound_count` / `inbound_count` | int | server | |
| `last_conversation_id` | string | server | |
| `contact` | `ContactView` | server | **Embedded**, not a ref — see §3.4. |
| `created_at` / `updated_at` | timestamp | server | |

Server-owned fields are **read-only**; a write containing one → 400
`invalid_request`. `notified_next_action_at` is internal and never serialized.

**`ContactImportResult`** — `{batch_id, created, updated, skipped, failed, results[]}`
where each result is `{index, address|null, status, code?, message?, suppressed?}`
and `status` ∈ `created|updated|skipped|failed`.

### 8.3 Endpoints

Account-level — `requireAccountScope` (agent-scoped credential → **403 `forbidden`**):

| Method | Path | Success | Errors |
|---|---|---|---|
| `GET` | `/v1/contacts` | 200 `Page[ContactView]` | 400 `invalid_cursor`/`invalid_filter` |
| `POST` | `/v1/contacts` | **201** + `Location` | 400 `invalid_recipient`, 409 `conflict`, 403 `contact_limit_reached` |
| `GET` | `/v1/contacts/{address}` | 200 `ContactView` + `ETag` | 404 `contact_not_found` |
| `PATCH` | `/v1/contacts/{address}` | 200 `ContactView` | 400, 404, 412 |
| `DELETE` | `/v1/contacts/{address}?confirm=DELETE` | 200 `{deleted:true,address}` | 400 (missing confirm), 404 |
| `POST` | `/v1/contacts/import` | 200 `ContactImportResult` | 400, **413 `payload_too_large`** |
| `DELETE` | `/v1/contacts/imports/{batch_id}?confirm=DELETE` | 200 `{deleted:true,batch_id,contacts_deleted,contacts_retained,engagements_deleted}` | 400, 404 `import_batch_not_found` |

`GET /v1/contacts` filters: `source`, `import_batch_id`, `created_after`, `created_before`.
`POST /v1/contacts` and `POST /v1/contacts/import` accept `Idempotency-Key`.

Agent-level — `requireAgentAccess` (deliberate divergence from
`agent_suppressions`, justified in §3.4):

| Method | Path | Success | Errors |
|---|---|---|---|
| `GET` | `/v1/agents/{email}/contacts` | 200 `Page[ContactEngagementView]` | 400, 403, 404 agent |
| `GET` | `/v1/agents/{email}/contacts/{address}` | 200 `ContactEngagementView` + `ETag` | 404 |
| `PUT` | `/v1/agents/{email}/contacts/{address}` | **201** on create (+`Location`), **200** on update | 400, 403 |
| `DELETE` | `/v1/agents/{email}/contacts/{address}?confirm=DELETE` | 200 `{deleted:true,address}` | 400, 404 |

`GET` filters: `stage`, `replied` (bool), `suppressed` (bool),
`next_action_before`, `last_outbound_before`.
`PUT` body: `{stage?, next_action_at?, metadata?}` — creates the contact
(`source=manual`) if absent. Idempotent; identical body → identical result.

#### 8.3.1 Recommended consumption pattern (documented, not enforced)

Two client-side patterns are load-bearing enough that the docs and SDKs must
teach them; getting either wrong produces real, visible harm.

**A. Always include `last_outbound_before` in the due query.**

```
GET /v1/agents/{email}/contacts
  ?replied=false
  &next_action_before=<now>
  &last_outbound_before=<now - followup_interval>
```

Sending and advancing state are two calls. Send succeeds → the `PUT` that moves
`stage`/`next_action_at` fails (network, crash, rate limit) → the naive query
(`next_action_before` alone) still returns that contact as due and **the agent
emails the investor twice**.

`last_outbound_at` is server-maintained and updates from the send itself, so
adding `last_outbound_before` excludes anyone recently contacted *even when the
client's own state write was lost*. The server-owned field is the safety net for
the agent-owned one. Costs nothing; omitting it is a live double-send hole.

**B. Treat the first `contact.due` as a trigger, then pull the batch.**

At outreach scale a sweep can emit hundreds of `contact.due` events at once (250
is ordinary mid-raise). Each is individually correct, but handling them as 250
independent wake-ups is wasteful and easy to get wrong. The intended shape is:
first event wakes the agent → agent issues **one** query (pattern A) → agent
processes the batch → subsequent events for that sweep are no-ops.

A digest event is the natural escape hatch if the burst proves painful in
practice; it is deliberately not in v1.

### 8.4 Error codes

Reused: `invalid_request`, `invalid_recipient`, `invalid_cursor`,
`invalid_filter`, `forbidden`, `not_found`, `conflict`, `payload_too_large`,
`rate_limited`, `idempotency_key_reuse`, `idempotency_in_flight`.

New: `contact_not_found`, `import_batch_not_found`, `contact_limit_reached`
(follows `template_limit_reached`). Per-item only, never an envelope code:
`duplicate_in_batch`.

### 8.5 Event: `contact.due`

Added to `webhookpub.AllEventTypes`, `ExperimentalEventTypes`, and every
request-side enum tag. `data` payload:

```json
{
  "agent_email": "raise@fund.com",
  "address": "partner@fund.vc",
  "stage": "touch1",
  "next_action_at": "2026-07-29T09:00:00Z",
  "replied": false,
  "last_outbound_at": "2026-07-24T09:00:00Z",
  "outbound_count": 1,
  "last_conversation_id": "raise-2026-a16z",
  "contact": { "address": "partner@fund.vc", "display_name": "A. Partner",
               "metadata": {"fund": "Example Capital"} }
}
```

Subscribers scope per-agent with the existing `agent_ids` webhook filter.

**Transports.** `contact.due` is published through `webhookpub.PublishTx` into
the standard event outbox, so it inherits durable at-least-once fan-out,
retries, HMAC signing, and SSRF-guarded delivery.

| Transport | Carries `contact.due`? | Why |
|---|---|---|
| Webhook | **Yes — the intended path** | Reaches an agent that is not running. |
| `GET /v1/events`, `/v1/events/{id}`, redeliver | Yes, automatically | Published events land in `webhook_events`. |
| MCP `list_events` / `get_event` | Yes | Sits on the REST events surface. |
| WebSocket | **No** | The hub is scoped to live-tailing inbound mail (`internal/ws/hub.go:36`). |

The WebSocket exclusion is correct by construction, not a gap: a WS-connected
agent is by definition already running, and a running agent does not need
waking — it queries `?next_action_before=now` directly. `contact.due` exists
for the agent that is *not* running, which means delivering to a deployed HTTP
endpoint.

**Documented caveat:** with no webhook subscribed to `contact.due`, nothing is
woken. The event still lands in `/v1/events`, so no state is lost, but the loop
degrades from push to pull. "e2a wakes your agent" is conditional on there being
an endpoint to wake — this belongs in the onboarding docs, not just here.

### 8.6 Contract invariants (normative MUSTs)

These are the behavioral contract. Each maps to a test in §6.

1. Contact keys **MUST** be produced by `identity.NormalizeMailboxAddress` —
   the same function suppression lookup uses. (§6.2)
2. Import **MUST NOT** send mail or create messages, conversations, or events
   beyond the import record.
3. Import **MUST NOT** clear or weaken any suppression. A re-import **MUST NOT**
   make a suppressed address sendable.
4. `on_conflict=merge` **MUST** touch identity and `metadata` only; `stage`,
   `next_action_at`, and all derived fields **MUST** be preserved.
5. Deleting an engagement **MUST NOT** delete the contact or any suppression.
   Deleting a contact **MUST NOT** clear suppressions.
6. `contact.due` **MUST NOT** fire when the address is suppressed for that
   agent, or when the agent is in trash. (Both fail-closed.)
7. `contact.due` **MUST** fire at most once per distinct `next_action_at` value;
   writing a new value re-arms.
8. Engagements **MUST** survive trash, **MUST** be purged on agent hard-delete,
   and **MUST NOT** be inherited by an agent recreated at the same address —
   while suppressions **MUST** survive recreation. (§6.5, the asymmetry test)
9. Server-owned engagement fields **MUST** reject client writes.
10. Agent-scoped credentials **MUST** receive 403 on `/v1/contacts*`, and
    **MUST NOT** reach another agent's engagements.
11. Every list **MUST** paginate with a stable sort; a continuation cursor with
    changed filters **MUST** be rejected.
12. All operations **MUST** ship marked beta until explicitly graduated.

## 9. Rejected alternatives (summary)

- **Contacts under agents only** — duplicate identity; an established provider shipped this and reversed it.
- **No entity, labels + facet query** — ~70% of the value, but import is impossible without an entity.
- **Segments/lists resource** — second many-to-many with no demand; engagement already groups.
- **Async bulk import (the marketing-platform shape)** — large surface for a scale we don't have; addable later without breaking the sync contract.
- **Server-side CSV parsing** — column mapping and encoding detection don't belong in a mail gateway.
- **e2a-driven sequences** — the product line in §3.5; makes e2a a cold-outreach sequencer.
- **A single global `unsubscribed` boolean on the contact** — the common industry shape; would flatten per-agent consent that e2a currently models correctly.

---

## 10. Amendment (2026-07-26): stop materializing the message counters

Status: **implemented** — reverses part of §3.2.

### What §3.2 decided, and what it got wrong

§3.2 said derived fields are materialized because "computing counters on read
means a correlated aggregate over `messages` — a prod-sized table — for every
row of every list request", and accepted counter drift as the price, mitigated
by `ReconcileEngagementCounts`.

The reasoning was sound for **one** field and was over-applied to all of them.
The cost argument is about the `replied` **filter**: `replied` appears in the
`WHERE` clause, so evaluating it per-request means aggregating over every
candidate row rather than a page. That genuinely requires materialized
timestamps.

`outbound_count` and `inbound_count` are different in kind. They are
**projected, never filtered** — `EngagementFilter` offers stage, replied,
suppressed, next_action_before and last_outbound_before, and nothing selects on
a count. Filtering therefore happens first, on indexed materialized columns, and
any aggregate runs over at most one page. That is a completely different cost
profile from the one §3.2 rejected, and the two cases were collapsed together.

### Only the counters can drift

Split the derived columns by whether their update is idempotent:

| Field | Update | Can drift? |
|---|---|---|
| `first_outbound_at` | `LEAST(…)` | No — converges on the earliest value regardless of arrival order or repetition |
| `last_outbound_at`, `last_inbound_at` | `GREATEST(…)` | No — converges upward |
| `replied` | derived from the above | No — inherits their convergence |
| `outbound_count`, `inbound_count` | `+ 1` | **Yes** — the only non-idempotent writes in the feature |

Every drift-prone field is a counter, and every counter is display-only. The
fields the outreach query filters on are precisely the ones that cannot drift.

### The codebase already answered this

`ConversationSummaryView` — a pre-existing, neighbouring resource — exposes the
same `inbound_count` / `outbound_count` pair and computes them at query time
(`internal/identity/store.go`, the conversation-summary query):

```sql
COUNT(*)                                     AS message_count,
COUNT(*) FILTER (WHERE direction='inbound')  AS inbound_count,
COUNT(*) FILTER (WHERE direction='outbound') AS outbound_count,
```

One grouped scan, no materialized column, no counter, no reconciliation, no
drift. Contacts is the outlier here, not the precedent — which also means this
amendment is an alignment rather than a novel trade-off.

### Proposal

Drop `outbound_count` and `inbound_count` from `contact_engagements` and compute
them in the engagement read paths, after filtering, over the returned page.
Keep the timestamps materialized: they are what the filter needs and they cannot
drift.

Deletes, rather than fixes: `ReconcileEngagementCounts`, `EngagementCountDrift`,
its janitor wiring and batch bound, the drift-report log vocabulary, and the
read-then-write race documented in §10.1 below. It is less code than repairing
the sweep.

### 10.1 The race this removes

`ReconcileEngagementCounts` reads stored and recomputed values in one snapshot,
collects the rows that disagree, then writes absolute values in a loop — none of
it in a transaction. An increment landing between the read and that row's write
is overwritten. The window is not milliseconds: the read happens once per batch
and writes are sequential, so for the last row of a 500-row pass it spans the
whole loop.

The counter inaccuracy self-corrects next sweep. The **reporting** damage does
not: drift reports are deliberately loud because they mean "an activity hook was
missed — go find the bug". A sweep that manufactures drift on the very rows it
just repaired trains the reader to ignore the alarm, at which point a real
missed hook goes unnoticed. That is the stronger reason to remove the mechanism
rather than tune it.

### What would reverse this

A filter that selects on volume (`outbound_count > 3`). The outreach query is
about time and reply state, not volume, so this is judged unlikely — but it is
the condition under which materializing becomes correct again.

### Cost of the change

`contact.due` carries `outbound_count` in its payload, and a webhook payload is
a harder contract to change than a response body. The field stays; only its
source changes. Migration is additive-by-deletion: a new migration drops the two
columns (forward-only, per AGENTS.md), and nothing reads them in between.
