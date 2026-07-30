# Email Thread Identity

Status: proposed
Date: 2026-07-30
Owners: e2a maintainers
Surfaces: database, inbound SMTP, outbound delivery, HTTP API, webhooks/events,
WebSocket, dashboard, SDKs, CLI, MCP

---

## Summary

e2a needs a first-class, server-owned email thread identity.

Today the dashboard groups messages by `conversation_id`. That field is caller
owned: an agent framework may omit it, reuse it for several unrelated emails,
or deliberately change it during one email exchange. It is useful application
correlation, but it is not a reliable email-thread key. As a result:

- an inbound message and an API reply can appear in separate dashboard threads;
- two fresh sends can be collapsed merely because a caller reused one
  `conversation_id`;
- messages without a `conversation_id` disappear from conversation views;
- a dashboard page can show only one side of an exchange even though the RFC
  reply headers are correct on the wire.

This design adds nullable `messages.thread_id`, owned exclusively by e2a and
scoped to one agent mailbox. e2a derives it from explicit API reply operations,
RFC `Message-ID` / `In-Reply-To` / `References` topology, and authenticated
internal delivery correlation. `conversation_id` remains unchanged as
caller-owned workflow correlation.

The design is additive. Existing request and response fields keep their current
meaning. Existing `/conversations` endpoints remain available. New `/threads`
endpoints expose email topology, and the dashboard moves to them after
historical data has been backfilled.

## Decision

Use three separate identities, each for one job:

| Identity | Owner | Scope | Purpose |
| --- | --- | --- | --- |
| RFC `Message-ID` and reply headers | Sending mail system | On-wire email graph | Make Gmail, Outlook, and other mail clients recognize replies |
| `thread_id` | e2a | One agent mailbox | Stable materialized identity for e2a's email reply graph |
| `conversation_id` | Application layer; caller-controlled when supplied | Application defined | Correlate a workflow, run, ticket, or backend-agent conversation |

The relationship between `thread_id` and `conversation_id` is many-to-many:

- one email thread may contain several caller conversation IDs;
- one caller conversation ID may produce several fresh email threads.

Neither identity silently rewrites the other.

“Caller-owned” below means the caller can choose and reuse the value. e2a's
existing behavior may mint or inherit a fallback when the caller omits one;
that convenience does not turn the field into email topology.

## Goals

1. Show the complete email exchange in e2a when messages are replies to one
   another, whether or not callers set `conversation_id`.
2. Keep fresh sends separate even when subject, participants, or
   `conversation_id` happen to match.
3. Match normal email-client topology without making an external provider's
   proprietary thread identifier authoritative.
4. Preserve current API contracts and `conversation_id` semantics.
5. Make thread assignment deterministic, mailbox-local, idempotent, and safe
   under retries and concurrent delivery.
6. Backfill historical messages conservatively without inventing relationships
   from subject lines or caller metadata.
7. Preserve wire-level reply behavior fixed by the SES qualified
   `Message-ID` work in commit `c618738d091da1a89fdd35a3b1f5cb3b2b11b925`.

## Non-goals

- Reconstruct quoted conversation history inside an email body. Adding a new
  CC recipient does not give that recipient earlier messages; that is a
  separate composition feature.
- Force Gmail, Outlook, or another client to display a thread. Their heuristics
  remain outside e2a's control.
- Merge threads based on normalized subject, sender, recipients, time window,
  or `conversation_id`.
- Introduce a caller-writable `thread_id`.
- Transport `thread_id` in SMTP headers.
- Automatically merge already-established threads after a late or conflicting
  ancestor arrives.
- Preserve topology after a message has been permanently purged. Soft-deleted
  rows remain usable as anchors; purged data does not leave topology
  tombstones.

## Terminology and invariants

### Email thread

A connected reply exchange as understood within one e2a agent mailbox.
A fresh send, inbound root, or forward starts a thread. A reply joins the
thread of its resolved parent.

### Reply parent

The earlier message to which a message is directly replying. For an API reply,
the referenced e2a message resource is authoritative. For inbound SMTP, e2a
resolves the parent from valid RFC reply headers.

A physical delivery twin is not a reply parent. It may share a thread with its
source outbound message, but `thread_parent_id` remains null unless it is also
an actual reply.

### Thread identity

`thread_id` is opaque, stable, non-enumerable, and prefixed `thr_`. Its identity
is the pair `(agent_id, thread_id)`, not the string globally in isolation.

### Required invariants

1. A message's non-null `thread_id` never changes.
2. A non-null `thread_parent_id` points to a message owned by the same agent.
3. A child and its reply parent have the same `thread_id`.
4. No resolution path crosses an agent-mailbox boundary.
5. A fresh send and a forward mint a new thread.
6. Changing or reusing `conversation_id` never joins or splits an email thread.
7. Retries and idempotency replays preserve the original thread decision.
8. Ambiguous evidence never merges established threads.
9. Hidden rows, including held and soft-deleted rows, may be topology anchors
   even when they are excluded from normal list responses.
10. An RFC identifier is a topology hint, not authorization to read a message
    or another mailbox.

## Current behavior

### Caller correlation is used as display topology

The existing conversation store groups by `messages.conversation_id`.
Messages with an empty value are excluded. This produces correct results only
when a caller uses one `conversation_id` for exactly one email exchange.

The current outbound path generally resolves `conversation_id` in this order:

1. a caller-supplied value;
2. the referenced message's value for reply-like operations;
3. a newly minted value for an outbound root.

Inbound SMTP may recover it through an RFC reply-header lookup. That is useful
for agent workflow continuity, but it does not make `conversation_id` a safe
mailbox thread key.

### Wire and product views can disagree

An outbound reply can carry valid `In-Reply-To` and `References`, allowing
Gmail to group it correctly, while e2a's dashboard still omits the inbound
message because the two rows have different or empty `conversation_id` values.
Conversely, two unrelated fresh sends can share one caller value and be
collapsed in e2a while mail clients display two threads.

### Historical RFC lookup is too permissive

The current conversation lookup includes a wildcard comparison against
`provider_message_id`. That was useful around older unqualified provider IDs,
but wildcard matching is not safe enough for topology identity. Thread
resolution must use canonical exact keys and treat collisions as ambiguity.

### Related delivery behavior

Commit `c618738d091da1a89fdd35a3b1f5cb3b2b11b925` ensured that the SES adapter
returns the qualified `Message-ID` actually used on the wire. That remains a
prerequisite for correct later replies. `thread_id` does not replace or relax
that behavior.

## Data model

Add the following nullable columns to `messages`:

```sql
thread_id            text null
thread_parent_id     text null references messages(id) on delete set null
rfc_message_id_key   text null
```

Use the next sequential migration number available when implementation begins.

### Why columns on `messages`

No `threads` table is needed initially. The thread is a materialized grouping
of messages, and list/detail summaries can be derived in the same way the
current conversation views are derived.

Keeping identity on the message:

- makes every write path independently testable;
- makes idempotency preservation explicit;
- avoids lifecycle rules for empty thread records;
- permits a nullable, old-binary-compatible rollout.

A future `threads` table may add thread-level metadata, but it must not become
a prerequisite for this design.

### Indexes

Add partial indexes equivalent to:

```sql
create index messages_agent_thread_created_idx
    on messages (agent_id, thread_id, created_at, id)
    where thread_id is not null;

create index messages_agent_rfc_message_id_idx
    on messages (agent_id, rfc_message_id_key, created_at, id)
    where rfc_message_id_key is not null;

create index messages_thread_parent_idx
    on messages (thread_parent_id)
    where thread_parent_id is not null;
```

Do not add a unique constraint on `rfc_message_id_key`. Duplicate wire IDs,
delivery twins, imported mail, and historical data make uniqueness invalid.

The implementation PR must assess production table size and use the
repository-supported low-lock index rollout mechanism. The schema migration
must not perform an unbounded historical backfill during server startup.

### ID generation and validation

- Generate `thread_id` with the existing cryptographically random ID facility.
- Encode it as `thr_` followed by 32 lowercase hexadecimal characters, matching
  the existing 128-bit message/conversation ID construction.
- Validate thread path values against `^thr_[0-9a-f]{32}$`.
- Never accept a `thread_id` in send, reply, forward, or inbound request data.
- A thread path lookup always includes the authenticated `agent_id`.

## RFC Message-ID canonicalization

Thread resolution needs one canonical exact lookup key.

### Parser requirements

Implement a bounded RFC message identifier parser rather than splitting on
whitespace. It must:

1. extract syntactically valid bracketed `msg-id` tokens from
   `Message-ID`, `In-Reply-To`, and `References`;
2. tolerate legal comments and folding whitespace around tokens;
3. reject control characters, malformed brackets, and oversized input;
4. preserve the identifier-left portion byte-for-byte;
5. lowercase only the identifier-right/domain portion;
6. store the canonical form with angle brackets;
7. return tokens in wire order, with duplicates removed while preserving the
   last occurrence needed for right-to-left resolution.

For example:

```text
<CaseSensitive.Left@MAIL.Example.COM>
```

becomes:

```text
<CaseSensitive.Left@mail.example.com>
```

Do not lowercase the whole value. Although many systems treat message IDs
case-insensitively in practice, the left portion is not defined as a DNS name.

### Outbound source of truth

The outbound transport adapter must persist the exact qualified
`Message-ID` used on the wire. If a provider returns only a bare identifier,
the adapter must either qualify it using a provider-specific rule proven to
match the emitted message or report that no reliable wire anchor exists.

Outbound rows may receive `thread_id` before submission and
`rfc_message_id_key` afterward. The successful provider-status update writes
the canonical wire key in the same transaction as the provider ID and delivery
state. It never changes the earlier thread decision.

Do not use substring, suffix, or SQL wildcard matching for new thread
decisions.

### Historical SES values

The backfill may apply the known historical SES qualification rule only to
records that can be positively identified as SES records from the affected
period. If qualification cannot be proven, leave the anchor unresolved rather
than guessing.

## Assignment algorithm

All new messages receive a thread decision in the same logical transaction as
message persistence. Assignment fills null fields only. It never overwrites a
non-null `thread_id` or merges two established thread IDs.

The following paths are ordered by authority.

### 1. API reply

For a reply operation referencing an e2a message resource:

1. load and authorize the referenced message using the existing reply rules;
2. ensure the parent has a `thread_id`, lazily deriving or minting one if it is
   an old row;
3. copy that `thread_id` to the new message;
4. set `thread_parent_id` to the referenced message ID.

The message-resource relationship is authoritative even if the caller supplies
a different `conversation_id` or the parent's RFC headers are incomplete.

The operation must lock or compare-and-set the old parent row so concurrent
replies cannot mint different thread IDs for it.

### 2. Fresh API send

A normal send without a referenced message always mints a new `thread_id`.
This remains true when recipient, subject, and `conversation_id` match an
earlier send.

### 3. Forward

A forward always mints a new `thread_id` and has no `thread_parent_id`. A reply
to that forward joins the forward's new thread.

The human-review TTL worker must retain the operation type. It must not copy
reply headers from the stored source into a forward merely because the source
has an `email_message_id`.

### 4. Self-send delivery twin

When the platform creates both Sent and Inbox representations for one
self-send, both rows share one `thread_id`. Neither twin is the reply parent of
the other.

The thread is assigned before both records become externally observable.

### 5. Authenticated platform delivery twin

An outbound test message that returns through real SMTP may create a physical
inbound representation of the same message. That inbound twin copies the
source outbound thread only when all of the following hold:

- an internal origin message identifier maps to an outbound row owned by the
  same agent;
- the inbound recipient maps to that same agent;
- the inbound authentication result proves delivery through a
  deployment-owned envelope domain, such as an SPF pass for that domain;
- any available provider/on-wire message identifier is compatible with the
  stored outbound anchor.

An unauthenticated `X-E2A-*` header, an envelope-domain string alone, or a
caller-supplied `conversation_id` is insufficient. `thread_id` itself is never
put on the wire.

### 6. Inbound SMTP reply

For a normal inbound message:

1. canonicalize and persist its own valid `Message-ID` as
   `rfc_message_id_key`;
2. examine valid `In-Reply-To` candidates from right to left;
3. if none resolves, examine valid `References` candidates from right to left;
4. for each candidate, query all earlier messages for the same `agent_id`,
   including held and soft-deleted rows;
5. a candidate is unambiguous when all matching rows that have a thread ID
   agree on one thread;
6. if matching rows have multiple established thread IDs, record ambiguity and
   continue to the next candidate;
7. on the first unambiguous match, inherit that thread and, where one exact
   message row can be selected, set it as `thread_parent_id`;
8. if no candidate resolves, mint a new thread.

The immediate `In-Reply-To` relationship has precedence over older
`References`. Within a header, the rightmost usable identifier is the nearest
ancestor.

When several rows share one candidate and one thread, thread membership is
safe even if a unique direct parent cannot be chosen. In that case set
`thread_id` and leave `thread_parent_id` null.

### 7. Idempotent retry

If a message already exists under an idempotency key, return its existing
thread identity. Never rerun assignment in a way that changes the stored
decision.

### Lazy repair helper

Implement one transactional helper, conceptually:

```text
EnsureThreadTx(agent_id, message_id) -> thread_id
```

It:

- returns an existing thread immediately;
- canonicalizes and stores the row's own known wire Message-ID when that key is
  still absent, including when the thread already exists;
- derives from an existing same-agent parent when safe;
- otherwise mints one value with compare-and-set semantics;
- has a bounded recursion depth and cycle detection;
- cannot cross `agent_id`;
- does not rewrite a conflicting non-null value.

All reply and historical-read repair paths use this helper rather than
duplicating assignment logic.

## Conversation identity remains separate

No `conversation_id` precedence or propagation rule changes in this feature.

In particular:

- an API reply with an explicit `conversation_id` keeps that caller value and
  still inherits the parent's `thread_id`;
- an API reply without one keeps the current conversation inheritance behavior;
- an inbound reply may recover a known conversation ID using existing rules;
- one reused conversation ID does not join fresh sends;
- a caller may deliberately change conversation ID during a thread;
- `X-E2A-Conversation-ID` never determines `thread_id`.

The existing `X-E2A-Conversation-ID` trust model should be hardened separately.
This design must not copy that trust decision into thread assignment.

## Reply headers and wire behavior

`thread_id` is an internal projection. RFC headers remain the source of
wire-client threading.

### Outbound replies

An API reply must:

- set `In-Reply-To` to the exact qualified on-wire Message-ID of its parent;
- construct `References` from the parent's valid reference chain plus that
  parent ID;
- preserve the current legal header folding behavior;
- bound the total reference chain to 100 identifiers and 8 KiB before folding,
  whichever is reached first;
- preserve the root identifier and the newest ancestors, including the
  immediate parent, when trimming.

### Terminal outbound messages without a wire anchor

Replying to an outbound row has three relevant states:

| State | Result |
| --- | --- |
| Accepted/sending and no provider Message-ID yet | Existing retryable `409 message_not_yet_delivered` |
| Sent/delivered with an exact provider Message-ID | Send the reply using that RFC parent |
| Terminally failed before provider acceptance and no provider Message-ID | New `409 message_has_no_wire_anchor` |

The new error means the referenced message never existed on the wire and
cannot be the RFC parent of a reply:

```json
{
  "error": {
    "code": "message_has_no_wire_anchor",
    "message": "Referenced outbound message was never submitted and has no RFC Message-ID; create a new send instead."
  }
}
```

Use HTTP 409 because the requested reply conflicts with the current state of
the referenced resource. The caller can start a fresh send, which receives a
new thread.

Register the code in the shared error catalog as `retryable: false`, and update
the reply operation's 409 documentation so it distinguishes this terminal
state from retryable `message_not_yet_delivered`.

This error is reply-specific. A forward is a new root and does not require the
source message to have an RFC wire anchor once the existing forward
authorization and lifecycle rules permit the operation. This feature does not
otherwise relax or change those existing forward gates.

### Edited reply subjects

Human review currently permits subject edits. e2a's internal thread remains
correct because the operation references a message resource, but some external
clients may split a heavily edited subject despite valid RFC headers. Preserve
the API contract; document the best-effort wire behavior and consider a UI
warning rather than rejecting the edit.

## HTTP API

### Additive message fields

Add optional, response-only `thread_id` to every public representation that
already identifies a message and for which a thread decision exists:

- message detail and message list summary;
- send/reply/forward result;
- held-message review views;
- WebSocket message notifications;
- webhook and event payload message data;
- data export rows.

Historical event envelopes remain immutable and may omit the field. SDK models
must therefore treat it as optional even after the backfill completes.

`thread_parent_id` and `rfc_message_id_key` are internal implementation fields
and are not exposed in the initial API.

### New thread resources

Add:

```http
GET /v1/agents/{email}/threads
GET /v1/agents/{email}/threads/{thread_id}
```

These are parallel to, not replacements for, the current conversation
endpoints.

#### List response

```json
{
  "items": [
    {
      "id": "thr_...",
      "last_message_at": "2026-07-30T18:42:11Z",
      "first_message_at": "2026-07-30T18:30:00Z",
      "message_count": 3,
      "inbound_count": 2,
      "outbound_count": 1,
      "has_unread": true,
      "latest_subject": "Re: Hi Jace",
      "latest_from": "gretta@aisa.one"
    }
  ],
  "next_cursor": null
}
```

The list:

- uses the standard v1 `{ "items": [...], "next_cursor": ... }` envelope;
- mirrors `ConversationSummaryView`, with `id` containing the `thr_` resource
  ID;
- includes live, normally visible messages only;
- excludes held inbound messages until approved, consistent with inbox views;
- excludes soft-deleted messages from counts and previews;
- still retains their thread identity internally for restore and topology;
- supports the same `since`, `until`, and bounded page-size behavior as
  `/conversations`;
- orders by `(last_message_at desc, thread_id desc)`;
- binds cursor state to agent and filters.

An approved held message keeps the thread chosen when it was received.

#### Detail response

```json
{
  "id": "thr_...",
  "last_message_at": "2026-07-30T18:42:11Z",
  "first_message_at": "2026-07-30T18:30:00Z",
  "message_count": 3,
  "inbound_count": 2,
  "outbound_count": 1,
  "has_unread": true,
  "latest_subject": "Re: Hi Jace",
  "latest_from": "gretta@aisa.one",
  "participants": [
    "jace@team.tokencanopy.com",
    "gretta@aisa.one"
  ],
  "labels": [],
  "messages": [
    {
      "id": "msg_...",
      "thread_id": "thr_...",
      "conversation_id": "run_42",
      "direction": "inbound",
      "subject": "Hi Jace",
      "created_at": "2026-07-30T18:30:00Z"
    }
  ],
  "next_cursor": null
}
```

The summary, participants, and labels cover all live visible members. Messages
use the existing public message summary schema and chronological
`(created_at, id)` ordering. The detail operation accepts `cursor` and `limit`
(default and maximum 100), and `next_cursor` paginates only the `messages`
array. Cursor state binds to agent and thread. The dashboard must follow this
pagination rather than infer a thread by grouping only the first page of
`/messages`.

### Authorization and caching

- Apply the same agent ownership checks as message and conversation reads.
- Return 400 `invalid_request` for a malformed thread ID.
- Return 404 for an unknown, cross-agent, or non-visible thread.
- Send `Cache-Control: no-store` on authenticated thread reads.
- A known RFC Message-ID never grants access to a thread.

### Existing conversation endpoints

Keep:

```http
GET /v1/agents/{email}/conversations
GET /v1/agents/{email}/conversations/{conversation_id}
```

unchanged for compatibility. Update their documentation to call them
application conversations derived from caller-owned `conversation_id`, not
email threads.

### Contract symmetry

The implementation must land together across:

- OpenAPI schema and generated/handwritten server types;
- Python SDK;
- TypeScript SDK;
- CLI;
- MCP server;
- dashboard.

Adding optional response fields is backward compatible. No request gains a
caller-writable `thread_id`.

## Webhooks, events, and WebSocket

Newly emitted message events include optional `thread_id` in the typed message
payload and, where the event store maintains indexed routing columns, in a
nullable routing column.

Initial scope does not add a webhook subscription filter by `thread_id`.
Consumers can correlate the payload field. A later filter can be added once
thread backfill and event-index cardinality are measured.

Delivery lifecycle events must copy the persisted message's thread identity;
they must not resolve topology independently.

WebSocket notifications follow the same optional-field rule. A missing value
during rollout means “not yet assigned,” not a separate thread.

## Dashboard behavior

After backfill reaches the rollout threshold:

1. inbox thread lists use `/threads`;
2. thread detail uses the server-paginated `/threads/{thread_id}` resource;
3. the UI displays `conversation_id` only as optional workflow metadata;
4. trash groups messages by their preserved `thread_id`, but trash membership
   remains per-message;
5. restoring any message returns it to the same thread;
6. messages with a temporarily null `thread_id` are shown as individual rows
   rather than hidden.

Roll out behind a server capability or web feature flag so an older server does
not break the dashboard during rollback.

## Scenario matrix

| Scenario | Internal result | Wire/client expectation |
| --- | --- | --- |
| Inbound root, then API reply without `conversation_id` | Same thread | Reply headers join in capable clients |
| Inbound root, then API reply with a new explicit `conversation_id` | Same thread; caller value preserved | Same as above |
| Outbound root, then external reply | Same thread | External reply references outbound ID |
| Agent sends two fresh messages to the same recipient and subject | Two threads | Usually two client threads unless client heuristics merge |
| Agent sends two fresh messages with the same `conversation_id` | Two threads | `conversation_id` does not affect wire topology |
| Agent replies twice to one inbound message | One branched thread | Both replies reference the same parent |
| Agent replies to its latest outbound reply | Same thread once exact provider ID exists | New reply references that outbound message |
| Agent replies while outbound is still being submitted | Retryable 409 | No guessed RFC parent |
| Agent replies to terminally failed outbound with no provider ID | `message_has_no_wire_anchor` | Caller must make a fresh send |
| Forward an inbound or outbound message | New thread | Forward is a new root |
| Reply to the forwarded message | Forward's thread | Normal reply headers |
| Reply-all with changed CC/BCC | Same thread | New recipients see only content included in this message |
| Existing participant is removed | Same thread | Remaining clients use reply headers |
| New CC recipient is added | Same thread internally | No automatic historical body backfill |
| Self-send creates Sent and Inbox twins | One mailbox-local thread | One physical message |
| Platform test outbound returns through SMTP | One thread only with authenticated twin correlation | Same on-wire ID where available |
| Agent A emails Agent B | Separate mailbox-local thread IDs | Each mailbox has its own topology projection |
| Same external message is delivered to two agents | Separate thread IDs | No cross-agent grouping |
| Multi-recipient outbound receives replies from several recipients | One sender-mailbox thread | Branches share the outbound root |
| Inbound reply is held for review | Thread chosen at receipt and preserved | Not visible in normal list until approved |
| Held reply is rejected | Hidden from normal thread; identity retained until purge | No outbound effect |
| Held reply is TTL-approved | Original thread retained | Reply/forward operation type remains correct |
| Idempotent API retry | Existing thread returned | No duplicate wire send |
| Provider delivery retry | Existing thread retained | Same logical message |
| Soft-delete then restore | Same thread | No topology change |
| Parent is soft-deleted before a reply arrives | Child can still resolve through hidden anchor | Parent remains hidden |
| Parent is permanently purged and no other ancestor survives | Conservative new thread | No deletion tombstone |
| `In-Reply-To` is absent but an older valid `References` anchor exists | Existing thread | Best-effort client continuity |
| Reply headers are stripped or malformed | New thread unless another valid anchor exists | Client may also split |
| Duplicate RFC ID rows all have one thread | Join that thread | Safe consensus |
| Duplicate RFC ID rows span several threads | Do not merge; try another ancestor or mint | Ambiguity recorded |
| `In-Reply-To` and `References` point to established different threads | Prefer unambiguous immediate parent; never merge the other thread | Conflict logged |
| A late unknown ancestor arrives after descendants were split | No automatic merge/rewrite | Possible conservative split remains |
| Mailing list or forwarder rewrites topology | New thread when no valid known anchor survives | Provider-dependent |
| Subject is edited during approved reply | Same internal thread | Some clients may split despite headers |
| Inbound root lacks `Message-ID` | Mint a thread; API reply inherits by resource | Recipient-side reply threading is not guaranteed |

## Conflict and abuse handling

### Conflicting anchors

Never merge established thread IDs. Prefer the unambiguous direct
`In-Reply-To` parent. If it is ambiguous, inspect older `References`. If no
candidate yields one thread, mint a new thread.

Emit a structured counter and rate-limited log with internal message IDs and
the number of candidate threads. Do not log full email addresses, subjects, raw
headers, or full RFC IDs. RFC IDs may contain identifying information; hash
them when correlation is needed.

### Spoofed known Message-ID

An external sender can copy a known RFC ID and cause its new message to appear
in that mailbox's thread. This mirrors normal email-client behavior and does
not grant data access: resolution is same-agent only, and the sender still sees
no previous content unless it was quoted or addressed to them.

Authentication and abuse verdicts remain attached to the individual message.
Thread membership must not lower review, spam, or policy controls.

### Cycles

Malformed data can create a parent cycle. Assignment and lazy repair use a
visited set and bounded depth. On a detected cycle, leave the parent unset,
mint or preserve one thread according to existing values, and emit an
invariant-violation metric.

## Historical backfill

The schema migration adds nullable columns and indexes only. Historical
assignment runs in bounded batches outside the migration transaction.

### Backfill rules

Process messages per agent in stable chronological order, with `id` as the
tie-breaker:

1. populate canonical `rfc_message_id_key` where valid;
2. preserve any already-assigned `thread_id`;
3. use stored, exact message-resource reply relationships where available;
4. correlate positively identified self-send and authenticated delivery twins;
5. resolve valid RFC reply headers against earlier same-agent rows;
6. mint a thread for unresolved roots;
7. never group by subject, participants, time window, or `conversation_id`.

Use `thread_id is null` as the resumable work queue. Batches use row locking
with skip-locked or an equivalent single-writer mechanism so multiple workers
do not stamp conflicting values. Commits are bounded by row count and time.

Backfill includes held and soft-deleted messages because they participate in
topology. Purged messages are absent by definition.

Implement the sweep as a unique River maintenance job. On startup after the
schema is present, enqueue one job when null rows exist. Each execution handles
at most 500 rows or two seconds of database work, whichever comes first, then
re-enqueues itself while work remains. It emits no message, webhook, or
WebSocket events and does not modify user-visible timestamps. A failed batch
uses River's normal retry policy; completed rows make the operation resumable
without a separate checkpoint table.

If a child predates a discoverable parent or historical provider data is
ambiguous, the backfill may conservatively split it. It must not rewrite a
non-null thread later in order to merge.

### Lazy coexistence

During the backfill:

- all new writes receive a thread;
- API replies lazily ensure the referenced old row has one;
- `/threads` excludes null rows;
- the old `/conversations` and message APIs continue to work;
- optional `thread_id` may be absent on old rows;
- the dashboard remains on its old view until the null rate is below the
  deployment threshold.

## Rollout and rollback

### Phase 1: schema

- Add nullable columns and indexes.
- Deploy code that can read either old or populated rows.
- Do not expose new dashboard behavior.

### Phase 2: dual write and shadow resolution

- Assign threads on every message writer.
- Add metrics comparing the proposed thread grouping with current
  conversation grouping.
- Verify no writer path creates unexplained nulls.

### Phase 3: backfill

- Run bounded historical batches.
- Monitor null count, throughput, ambiguity, and conflicts.
- Sample only with throwaway/test accounts for content-level production checks.

### Phase 4: API exposure

- Add optional response fields and `/threads`.
- Release SDK, CLI, and MCP support in the coordinated API batch.
- Keep `/conversations` unchanged.

### Phase 5: dashboard cutover

- Enable the new thread endpoints behind a capability/feature flag.
- Verify complete history beyond the first 100 messages.
- Retain a fallback while the minimum supported server version can lack
  `/threads`.

### Phase 6: completeness

- Alert on new rows without a thread.
- Consider a database `NOT NULL` constraint only after the rollback floor no
  longer includes binaries that do not write the column.
- Keeping the column nullable indefinitely is acceptable because public
  representations must already tolerate historical absence.

### Rollback

Old binaries ignore the additive nullable columns. Rolling application code
back does not require removing or rewriting data. Disable the dashboard feature
flag before or with an application rollback. Do not drop columns or indexes as
part of an incident rollback.

## Observability

Add counters by resolution source:

- `api_reply`
- `fresh_send`
- `forward`
- `rfc_in_reply_to`
- `rfc_references`
- `self_twin`
- `authenticated_delivery_twin`
- `backfill`
- `ambiguous_anchor`
- `no_anchor`
- `cycle_detected`

Add gauges or periodic measurements for:

- messages with null `thread_id`, by bounded age bucket;
- backfill batch age and throughput;
- parent/thread invariant violations;
- percentage of threads containing multiple conversation IDs;
- percentage of conversation IDs spanning multiple threads.

Those last two are expected and help validate why the identities must remain
separate; they are not error metrics.

Trace/log fields may include internal `message_id`, `thread_id`, agent ID, and
resolution source. Hash RFC IDs and omit subjects, bodies, and recipient
addresses from routine logs.

## Verification

### Unit tests

- RFC token parsing, comments, folding, malformed input, bounds, case rules,
  and duplicate removal.
- Right-to-left candidate precedence.
- Ambiguous duplicate-ID handling.
- immutable assignment and compare-and-set behavior.
- recursion depth and cycle handling.
- `References` construction, trimming, and legal folding.
- terminal outbound error classification.

### Database integration tests

Every direct SQL message writer must assert `thread_id` behavior. Cover:

- inbound accepted, held, approved, rejected, and restored;
- queued, scheduled, sent, delivered, failed, and retried outbound messages;
- reply, reply-all, forward, test-send, and self-send;
- webhook/event and WebSocket reads;
- concurrent replies and concurrent backfill;
- soft delete and permanent purge;
- duplicate canonical RFC IDs.

### API contract tests

- old requests remain valid without `thread_id`;
- optional fields do not become required in generated SDK models;
- no request schema accepts `thread_id`;
- `/conversations` response shape and meaning stay unchanged;
- cross-agent thread access returns 404;
- pagination cursors cannot be replayed across agents or filter sets;
- new error code is represented across OpenAPI, SDKs, CLI, and MCP.

### End-to-end tests

Automate the scenario matrix where the environment permits. In particular:

- inbound root → agent reply without `conversation_id`;
- inbound root → reply with a different explicit `conversation_id`;
- fresh sends sharing a conversation ID remain separate;
- outbound → external reply → agent reply is one thread;
- two branches from one parent are one thread;
- agent-to-agent messages have mailbox-local thread IDs;
- authenticated test delivery twin shares a thread;
- a spoofed internal-origin header does not activate twin correlation;
- terminal failed outbound returns `message_has_no_wire_anchor`;
- a thread with more than 100 messages is fully retrievable by pagination.

Wire assertions must verify `Message-ID`, `In-Reply-To`, and `References`
independently of internal `thread_id`. The change must not regress the
qualified-ID behavior established by commit
`c618738d091da1a89fdd35a3b1f5cb3b2b11b925`.

### Production verification

Follow the production policy: use throwaway agents and inboxes only, never real
agent mailboxes or external personal addresses, and delete the throwaway agents
afterward. Verify both API grouping and raw received headers.

## Implementation work packages

The implementation should be reviewable in this order:

1. **Schema and domain primitives**
   - columns, indexes, ID generation, canonical RFC parser;
   - store types and invariant tests.
2. **Assignment on all write paths**
   - inbound, outbound, reply, forward, HITL, scheduling, self-send, retries;
   - transactional `EnsureThreadTx`.
3. **Wire-anchor correctness**
   - exact provider ID contract;
   - terminal no-anchor error;
   - bounded `References`;
   - authenticated twin correlation.
4. **Backfill and observability**
   - bounded worker, metrics, invariant audit.
5. **Public API symmetry**
   - OpenAPI, HTTP endpoints, events, WebSocket, Python, TypeScript, CLI, MCP.
6. **Dashboard cutover**
   - server-paginated thread views, trash/restore handling, feature fallback.
7. **Documentation cleanup**
   - distinguish application conversations from email threads throughout API
     docs and examples.

Each package must preserve nullable compatibility until the coordinated API and
dashboard rollout is complete.

## Current code audit map

Implementation must begin with a fresh search for every direct
`INSERT INTO messages` and message-copy path. At the time of this decision, the
known high-risk touchpoints are:

- `internal/identity/store.go`: main message persistence, message reads,
  current `LookupConversationID`, conversation aggregation, trash, and restore;
- `internal/identity/delivery_store.go`: delivery-state transitions and a
  historical provider-ID wildcard lookup that must not be reused for threads;
- `internal/relay/server.go`: inbound RFC parsing and the current
  `X-E2A-Conversation-ID` trust path, which must remain separate from thread
  trust;
- `internal/outbound/compose.go` and `internal/outbound/sender.go`: reply
  headers, transport result, and provider wire ID;
- `internal/httpapi/outbound.go`: reply/forward parent validation, send
  results, and error catalog;
- `internal/hitlworker/worker.go`: stored operation reconstruction.
  `sendRequestFromStoredMessage` currently copies `EmailMessageID`
  unconditionally, so TTL-approved forwards require a regression test proving
  they do not acquire reply headers;
- `internal/eventpayload`, `internal/webhookpub`, `internal/ws`, and
  `internal/httpapi/events.go`: additive event and notification projection;
- `web/src/app/(app)/inboxes/(view)/messages/page.tsx` and message components:
  current client-window grouping and the dashboard cutover;
- `api/openapi.yaml`, both SDKs, CLI, and MCP: coordinated public contract.

The implementation review should reject a helper that infers `thread_id` by
calling `LookupConversationID` or trusting `X-E2A-Conversation-ID`. Those paths
serve caller correlation and intentionally have different semantics.

## Consequences

### Benefits

- The dashboard reflects email reply topology even when agents omit or change
  `conversation_id`.
- Backend agent frameworks retain full control of their own conversation/run
  correlation.
- Fresh sends no longer collapse because of subject or caller metadata.
- Thread decisions are stable, inspectable, and queryable without reparsing all
  headers on every read.
- API compatibility is additive and rollback-safe.

### Costs

- Every message writer and serializer must be audited.
- Historical backfill can only be conservative where old provider IDs are
  incomplete.
- e2a and an external mail client can still disagree when headers are stripped,
  identifiers conflict, or client-specific subject heuristics intervene.
- Maintaining both `/threads` and `/conversations` requires precise
  terminology and SDK support.

These costs are preferable to overloading a caller-owned identifier or making
provider-specific thread IDs part of the public contract.
