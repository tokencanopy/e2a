# Email Thread Identity

Status: proposed
Date: 2026-07-30
Owners: e2a maintainers
Surfaces: database, inbound SMTP, outbound delivery, HTTP API, webhooks/events,
WebSocket, dashboard, SDKs, CLI, MCP

---

## Summary

e2a needs a first-class, server-owned email thread identity.

Today the conversation API groups messages by `conversation_id`, while the
dashboard derives groups from only the message window it has loaded. That
field is caller owned: an agent framework may omit it, reuse it for several
unrelated emails, or deliberately change it during one email exchange. It is
useful application correlation, but it is not a reliable email-thread key. As
a result:

- an inbound message and an API reply can appear in separate dashboard threads;
- two fresh sends can be collapsed merely because a caller reused one
  `conversation_id`;
- messages without a `conversation_id` disappear from `/conversations`
  (the dashboard currently renders loaded rows as synthetic orphan groups);
- a dashboard page can show only one side of an exchange even though the RFC
  reply headers are correct on the wire.

This design adds nullable `messages.thread_id`, owned exclusively by e2a and
scoped to one agent mailbox. e2a derives it from explicit API reply operations,
RFC `Message-ID` / `In-Reply-To` / `References` topology, and authenticated
internal delivery correlation. `conversation_id` remains unchanged as
caller-owned workflow correlation.

The schema, fields, and thread resources are additive. Existing request and
response fields keep their current meaning, and `/conversations` remains
available. One deliberate behavior change is called out separately: replying
to a terminally failed outbound message that has no wire anchor returns a new
409 instead of sending an unthreaded message.

There is no historical bulk backfill. Messages created after thread assignment
is enabled receive `thread_id`. An older threadless message is assigned one
only when a new message directly references it as an exact reply anchor; that
bounded lazy adoption is part of the new write transaction, not a historical
sweep.

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

1. Show the complete email exchange for messages created after thread
   assignment is enabled when they are replies to one another, whether or not
   callers set `conversation_id`.
2. Keep fresh sends separate even when subject, participants, or
   `conversation_id` happen to match.
3. Match normal email-client topology without making an external provider's
   proprietary thread identifier authoritative.
4. Preserve current API contracts and `conversation_id` semantics.
5. Make thread assignment deterministic, mailbox-local, idempotent, and safe
   under retries and concurrent delivery.
6. Lazily adopt an exact old reply anchor when new traffic references it,
   without sweeping or reconstructing historical message graphs.
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
- Bulk-backfill historical messages or reconstruct old threads. Old rows stay
  null unless a new write directly references an exact old anchor.
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
2. A non-null `thread_parent_id` points to a message owned by the same agent
   while that parent remains retained.
3. A child and its reply parent have the same `thread_id`.
4. No resolution path crosses an agent-mailbox boundary.
5. A fresh send and a forward mint a new thread.
6. Changing or reusing `conversation_id` never joins or splits an email thread.
7. Retries and idempotency replays preserve the original thread decision.
8. Ambiguous evidence never merges established threads.
9. Hidden rows, including held and soft-deleted rows, may be topology anchors
   for new traffic even when excluded from normal list responses.
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

Commit `c618738d091da1a89fdd35a3b1f5cb3b2b11b925` and
`TestReplyThreadingSESMessageIDE2E` ensure that the SES adapter returns the
qualified `Message-ID` actually used on the wire. That remains a prerequisite
for correct later replies. `thread_id` does not replace or relax that behavior.

## Data model

Add the following nullable columns to `messages`:

```sql
thread_id            text null
thread_parent_id     text null
rfc_message_id_key   text null
```

`thread_parent_id` deliberately has no self-referencing database foreign key.
The parent link is optional diagnostic topology, not the source of thread
membership. A self-FK would take a heavyweight validation lock and amplify
whole-agent and retention purges. The write transaction enforces same-agent
parent/thread consistency. An individual permanent purge clears pointers from
surviving children in the same transaction. The routine
`DeleteExpiredMessages` retention batch does the same for children that are not
part of that delete batch, using `messages_thread_parent_idx`; a whole-agent
purge deletes the mailbox without first updating children. A periodic invariant
audit detects unexpected dangling or cross-agent pointers.

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

Split schema work across migrations:

1. one transactional migration adds the three nullable columns;
2. one `-- e2a:no-transaction` migration per concurrent index, because the
   runner rejects multi-statement no-transaction migrations;
3. every index uses `IF NOT EXISTS`;
4. use a migration prefix strictly greater than the highest existing prefix
   and never reuse a number (the tree already contains a duplicated `067`).

Add partial indexes equivalent to:

```sql
create index concurrently if not exists messages_agent_thread_created_idx
    on messages (agent_id, thread_id, created_at, id)
    where thread_id is not null;

create index concurrently if not exists messages_agent_rfc_message_id_idx
    on messages (agent_id, rfc_message_id_key, created_at, id)
    where rfc_message_id_key is not null;

create index concurrently if not exists messages_thread_parent_idx
    on messages (thread_parent_id)
    where thread_parent_id is not null;

create index concurrently if not exists messages_agent_inbound_message_id_idx
    on messages (agent_id, email_message_id)
    where direction = 'inbound' and email_message_id <> '';
```

Do not add a unique constraint on `rfc_message_id_key`. Duplicate wire IDs,
delivery twins, imported mail, and historical data make uniqueness invalid.

The existing provider-ID index supports the outbound side of exact legacy
anchor lookup. No migration updates existing message rows or performs a
historical sweep. The implementation must confirm with `EXPLAIN` under the
driver's prepared/generic-plan behavior that the legacy inbound query uses
`messages_agent_inbound_message_id_idx`.

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

Identifier provenance is direction-specific:

- inbound `email_message_id` is that inbound row's own RFC `Message-ID`;
- outbound `provider_message_id` is that outbound row's own on-wire ID once
  provider acceptance is known;
- outbound `email_message_id` may contain the parent's RFC ID used for reply
  composition and must never populate the child's `rfc_message_id_key`.

Outbound rows may receive `thread_id` before submission and
`rfc_message_id_key` afterward. Every path that transitions an outbound row to
a provider-accepted state writes the canonical wire key in the same transaction
as the provider ID and delivery state. This includes the normal sent update,
provider-acceptance reconciliation, terminal reconciliation, and send-attempt
repair. None changes the earlier thread decision.

Do not use substring, suffix, or SQL wildcard matching for new thread
decisions.

### Exact adoption of legacy anchors

There is no historical normalization pass. When new inbound traffic references
an old message, resolution first uses `rfc_message_id_key`, then may perform an
exact, direction-aware fallback:

- old inbound anchor: exact `email_message_id`;
- old outbound anchor: exact `provider_message_id`.

The fallback uses the original parsed token and its canonical equivalent, never
`LIKE`, suffix, substring, or subject matching. When it finds one unambiguous
old anchor, the new-write transaction locks that row, canonicalizes its own ID,
and lazily assigns its thread.

Some pre-fix SES rows contain only a bare provider ID while live replies carry
the qualified value. Those values do not match exactly and are not guessed or
rewritten by this feature. A new reply to such an old row starts a new thread.
The existing `conversation_id` behavior remains independent. This limitation
is intentional because the feature guarantees complete topology for new
messages, not reconstruction of legacy mail.

## Assignment algorithm

All new messages receive a thread decision in the same logical transaction as
message persistence. Assignment fills null fields only. It never overwrites a
non-null `thread_id` or merges two established thread IDs.

The following paths are ordered by authority.

### 1. API reply

For a reply operation referencing an e2a message resource:

1. load and authorize the referenced message using the existing reply rules;
2. ensure the parent has a `thread_id`, minting one under a row lock if it is
   an old row;
3. copy that `thread_id` to the new message;
4. set `thread_parent_id` to the referenced message ID.

The message-resource relationship is authoritative even if the caller supplies
a different `conversation_id` or the parent's RFC headers are incomplete.

The operation does not traverse or reconstruct the old parent's historical
ancestors. It adopts only that directly referenced row. The operation must lock
or compare-and-set the old parent so concurrent replies cannot mint different
thread IDs for it.

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

- the recipient-visible, unsigned `X-E2A-Message-ID` value maps to an outbound
  row owned by the same agent;
- the inbound recipient maps to that same agent;
- the inbound authentication result proves delivery through a
  deployment-owned envelope domain, such as an SPF pass for that domain;
- any available provider/on-wire message identifier is equal after
  canonicalization to the stored outbound anchor, or is absent.

`X-E2A-Message-ID` supplies correlation but no integrity: it is added
post-DKIM and can be copied by a recipient. The authenticated deployment-owned
envelope is therefore the security proof. The header alone, an envelope-domain
string without a passing authentication result, or a caller-supplied
`conversation_id` is insufficient. `thread_id` itself is never put on the
wire.

### 6. Inbound SMTP reply

For a normal inbound message:

1. canonicalize and persist its own valid `Message-ID` as
   `rfc_message_id_key`;
2. examine valid `In-Reply-To` candidates from right to left;
3. if none resolves, examine valid `References` candidates from right to left;
4. for each candidate, query all existing messages for the same `agent_id`,
   regardless of `created_at`, including held and soft-deleted rows;
5. query canonical `rfc_message_id_key` first, followed by the exact
   direction-aware legacy fallback described above;
6. if matching rows with non-null thread IDs all agree on one thread, inherit
   that thread;
7. if matching rows have multiple established thread IDs, record ambiguity and
   continue to the next candidate;
8. if exactly one matching row exists and its `thread_id` is null, lock it
   through `EnsureThreadTx`, lazily mint its thread, and inherit the returned
   value;
9. if several matching rows all have null thread IDs, treat the candidate as
   ambiguous rather than choosing one arbitrarily;
10. set `thread_parent_id` only when one exact parent row was selected;
11. if no candidate resolves, mint a new thread.

The immediate `In-Reply-To` relationship has precedence over older
`References`. Within a header, the rightmost usable identifier is the nearest
ancestor.

When several rows share one candidate and one established thread, membership is
safe even if a unique direct parent cannot be chosen. In that case set
`thread_id` and leave `thread_parent_id` null. A null thread is never treated as
vacuous agreement.

### 7. Idempotent retry

If a message already exists under an idempotency key, return its existing
thread identity. Never rerun assignment in a way that changes the stored
decision.

### Lazy anchor helper

Implement one transactional helper, conceptually:

```text
EnsureThreadTx(agent_id, message_id) -> thread_id
```

It:

- returns an existing thread immediately;
- locks the same anchor row used by live inbound resolution;
- canonicalizes and stores only the row's own direction-appropriate wire ID
  when that key is still absent;
- otherwise mints one value with compare-and-set semantics;
- cannot cross `agent_id`;
- does not rewrite a conflicting non-null value.

The helper is called only from a new-message write that directly references an
old anchor. Reads never mutate historical messages, and the helper never walks
or assigns the anchor's old ancestors. Live API replies and live inbound
resolution use this same lock, so concurrent adoption converges on one value.

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
- preserve the current chain construction and legal header folding behavior.

This feature does not introduce a `References` count or byte cap. Trimming a
long chain can remove a mid-chain anchor needed by a participant who did not
receive the immediate parent, so any future header-budget policy requires its
own provider evidence, compatibility analysis, and wire-level review.

### Terminal outbound messages without a wire anchor

Replying to an outbound row has three relevant states:

| Usable canonical wire anchor | Submission state | Result |
| --- | --- | --- |
| Yes | Any state allowed by the existing reply lifecycle rules | Send the reply using that exact RFC parent |
| No | `accepted` or `sending` | Existing retryable `409 message_not_yet_delivered` |
| No | Any other state, including legacy null status, terminal failure, or a nominal `sent` row whose provider ID cannot be canonicalized | New non-retryable `409 message_has_no_wire_anchor` |

The new error means the referenced message either never existed on the wire or
cannot be proven to have a usable RFC parent for a reply. The predicate is based
on a canonicalizable, direction-appropriate wire anchor, not merely a non-empty
`provider_message_id`:

```json
{
  "error": {
    "code": "message_has_no_wire_anchor",
    "message": "Referenced outbound message has no usable RFC Message-ID; create a new send instead."
  }
}
```

Use HTTP 409 because the requested reply conflicts with the current state of
the referenced resource. The caller can start a fresh send, which receives a
new thread.

Register the code in the shared error catalog as `retryable: false`, and update
the reply operation's 409 documentation so it distinguishes this terminal
state from retryable `message_not_yet_delivered`.

This is a coordinated behavior change, not a purely additive one. Today a
reply to a terminally failed outbound row can succeed without
`In-Reply-To`/`References`. Release notes must tell callers to create a fresh
send instead, and the new error ships with the coordinated API batch rather
than the schema-only phase.

The error is reply-specific. Reply and forward currently share
`loadRepliableMessage` / `parentNotYetSubmitted`, and an end-to-end regression
test deliberately pins the existing forward gate. Implement
`message_has_no_wire_anchor` in the reply handler after the shared load, not in
that shared seam. A forward remains a new root and this feature does not relax
or otherwise change its existing authorization and lifecycle gates.

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

Historical event envelopes remain immutable and omit the field. Old messages
may also remain threadless indefinitely, so SDK models always treat the field
as optional.

`thread_parent_id` and `rfc_message_id_key` are internal implementation fields
and are not exposed in the initial API.

### New thread resources

Add:

```http
GET /v1/agents/{email}/threads
GET /v1/agents/{email}/threads/{id}
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
- uses a distinct `ThreadSummaryView` whose field names mirror
  `ConversationSummaryView`, with `id` containing the `thr_` resource ID;
- includes live, normally visible messages only;
- excludes held inbound messages until approved, consistent with inbox views;
- therefore excludes held inbound messages from `has_unread`;
- excludes soft-deleted messages from counts and previews;
- still retains their thread identity internally for restore and topology;
- supports the same `since`, `until`, and bounded page-size behavior as
  `/conversations`;
- orders by `(last_message_at desc, thread_id desc)`;
- binds cursor state to agent and filters;
- captures `as_of` on the first page and evaluates thread aggregates using
  messages with `created_at <= as_of` on later pages, preventing new arrivals
  from reordering that pagination snapshot.

Concurrent delete, restore, or lazy adoption of an old anchor may still change
membership during pagination. `has_unread` may also change because read status
is mutable independently of `created_at`. The endpoint documents that limited
best-effort behavior.

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
  "participants_truncated": false,
  "labels_truncated": false,
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

Use a distinct `ThreadDetailView`. The summary covers all live visible members
inside the cursor's `as_of` snapshot. Participants and labels are computed over
that same snapshot by bounded aggregate queries, capped at 100 unique values
each, with the corresponding truncation flag. Messages use the existing public
message summary schema and chronological `(created_at, id)` ordering.

The detail operation accepts `cursor` and `limit` (default and maximum 100),
and `next_cursor` paginates only the `messages` array. It captures an `as_of`
snapshot like the list operation and binds cursor state to agent, thread, and
snapshot. The dashboard must follow this pagination rather than infer a thread
by grouping only the first page of `/messages`.

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
GET /v1/agents/{email}/conversations/{id}
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
Consumers can correlate the payload field. A later filter can be added after
event-index cardinality is measured.

Delivery lifecycle events must copy the persisted message's thread identity;
they must not resolve topology independently.

WebSocket notifications follow the same optional-field rule. A missing value
means a legacy threadless message, not a new independent thread identity.

## Dashboard behavior

After the new-write path and API are deployed:

1. assignment-enabled inbox thread lists use `/threads`;
2. thread detail uses the server-paginated `/threads/{id}` resource;
3. the UI displays `conversation_id` only as optional workflow metadata;
4. rows with null `thread_id` remain accessible through the existing message
   view under a clearly labeled unthreaded-messages path and never
   disappear merely because `/threads` excludes them;
5. trash grouping uses the existing trash-message list plus each message's
   optional `thread_id`; `/threads` remains live-only;
6. restoring a threaded message preserves its thread, while restoring a
   legacy null row leaves it null unless new traffic later references it.

Roll out behind a server capability or web feature flag so an older server does
not break the dashboard during rollback.

## Scenario matrix

| Scenario | Internal result | Wire/client expectation |
| --- | --- | --- |
| Inbound root, then API reply without `conversation_id` | Same thread | Reply headers join in capable clients |
| Inbound root, then API reply with a new explicit `conversation_id` | Same thread; caller value preserved | Same as above |
| New API reply directly references an old threadless message | Lazily assign that old parent and inherit; do not traverse older history | Wire behavior uses the exact available parent ID |
| New inbound reply exactly references one old threadless anchor | Lazily assign that old anchor and inherit | Best effort through exact RFC ID |
| Old messages never referenced by new traffic | Remain null and visible through legacy message views | No change |
| New inbound reply carries a qualified ID but the old SES row stores only a bare ID | Conservative new thread | No wildcard or guessed legacy qualification |
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
| Parent is permanently purged | Surviving child pointer is cleared; existing child thread remains stable | No deletion tombstone |
| Parent was purged before a new reply and no other ancestor survives | Conservative new thread | No deletion tombstone |
| `In-Reply-To` is absent but an older valid `References` anchor exists | Existing thread | Best-effort client continuity |
| Reply headers are stripped or malformed | New thread unless another valid anchor exists | Client may also split |
| Duplicate RFC ID rows all have one thread | Join that thread | Safe consensus |
| Duplicate RFC ID rows span several threads | Do not merge; try another ancestor or mint | Ambiguity recorded |
| `In-Reply-To` and `References` point to established different threads | Prefer unambiguous immediate parent; never merge the other thread | Conflict logged |
| A late unknown ancestor arrives after descendants were split | No automatic merge/rewrite | Possible conservative split remains |
| Mailing list or forwarder rewrites topology | New thread when no valid known anchor survives | Provider-dependent |
| Subject is edited during approved reply | Same internal thread | Some clients may split despite headers |
| Inbound root lacks `Message-ID` | Mint a thread; API reply inherits by resource | Recipient-side reply threading is not guaranteed |
| `References` grows very long | Internal thread is unchanged | Preserve existing chain construction/folding in this feature |

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

Malformed data can contain a parent cycle, but assignment never follows
`thread_parent_id` recursively, so a cycle cannot influence membership. The
periodic invariant audit detects a stored cycle, clears the invalid parent link
without changing any established `thread_id`, and emits an invariant-violation
metric.

## Legacy coexistence without backfill

The migration and deployment perform no historical message update, River job,
startup sweep, or read-time repair.

- Every message created by the new code receives a `thread_id`.
- An old row remains null indefinitely unless a new message directly
  references it through an authorized API reply or one exact RFC anchor.
- That direct old row is locked and stamped in the same transaction as the new
  message; its historical parents, siblings, and conversation peers remain
  untouched.
- `/threads` includes only rows with non-null thread IDs.
- `/messages`, `/conversations`, trash, and historical events retain their
  existing behavior and continue to expose legacy rows.
- SDKs and consumers always treat absent `thread_id` as a supported legacy
  state.
- Neither reads nor dashboard navigation mutate old data.

This boundary keeps database work proportional to new traffic. It deliberately
accepts that a post-assignment reply may contain only its exact old parent plus
new descendants rather than the parent's entire historical exchange.

## Rollout and rollback

### Phase 1: schema

- Add nullable columns and indexes.
- Deploy code that can read either old or populated rows.
- Perform no historical updates.
- Do not expose new dashboard behavior yet.

### Phase 2: new-write assignment

- Assign threads on every message writer.
- Enable exact, locked lazy adoption when new traffic directly references an
  old anchor.
- Verify no writer after assignment enablement creates an unexplained null.
- Keep the new terminal-reply behavior disabled until the coordinated API
  phase.

### Phase 3: API exposure

- Add optional response fields and `/threads`.
- Release SDK, CLI, and MCP support in the coordinated API batch.
- Enable and document `message_has_no_wire_anchor`.
- Keep `/conversations` unchanged.

### Phase 4: dashboard enablement

- Enable the new thread endpoints behind a capability/feature flag.
- Verify complete post-assignment history beyond the first 100 messages.
- Preserve a visible route to legacy null messages.
- Retain a fallback while the minimum supported server version can lack
  `/threads`.

### Permanent nullable state

- Record the Phase-2 assignment-enablement instant operationally. Alert only on
  rows created after that instant that unexpectedly lack a thread; Phase-1 and
  rollback-window rows are supported threadless messages.
- Do not add a database `NOT NULL` constraint: historical rows are
  intentionally null and the public contract permanently supports absence.

### Rollback

Old binaries ignore the additive nullable columns. Rolling application code
back does not require removing or rewriting data. Disable the dashboard feature
flag before or with an application rollback. Do not drop columns or indexes as
part of an incident rollback. Lazily stamped old anchors remain harmless
additive metadata after rollback.

## Observability

Add counters by resolution source:

- `api_reply`
- `fresh_send`
- `forward`
- `rfc_in_reply_to`
- `rfc_references`
- `self_twin`
- `authenticated_delivery_twin`
- `lazy_legacy_anchor`
- `anchor_found_without_thread`
- `legacy_anchor_unmatched`
- `ambiguous_anchor`
- `no_anchor`
- `cycle_detected`

Add gauges or periodic measurements for:

- post-assignment messages with null `thread_id`, by bounded age bucket;
- old rows lazily adopted per hour;
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
- lazy old-anchor locking and concurrent adoption.
- direction-specific RFC key provenance.
- `References` construction and legal folding remain byte-compatible.
- terminal outbound error classification, including null status, terminal
  failure, and nominal `sent` with an unusable provider ID.

### Database integration tests

Every direct SQL message writer must assert `thread_id` behavior. Cover:

- inbound accepted, held, approved, rejected, and restored;
- queued, scheduled, sent, delivered, failed, and retried outbound messages;
- reply, reply-all, forward, test-send, and self-send;
- webhook/event and WebSocket reads;
- concurrent replies and concurrent inbound/API adoption of one old anchor;
- proof that deployment does not sweep unrelated old null rows;
- every provider-acceptance and reconciliation writer populates the canonical
  outbound key;
- soft delete, individual permanent purge, retention-batch purge, and
  whole-agent purge;
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
`c618738d091da1a89fdd35a3b1f5cb3b2b11b925` and
`TestReplyThreadingSESMessageIDE2E`.

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
   - unchanged `References` behavior;
   - authenticated twin correlation.
4. **Legacy coexistence and observability**
   - exact lazy adoption, no-sweep proof, metrics, invariant audit.
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
- `internal/identity/send_attempts.go` and
  `internal/outboundsend/terminal_reconcile.go`: provider-acceptance repair
  paths that must persist the canonical outbound key;
- `internal/relay/server.go`: inbound RFC parsing and the current
  `X-E2A-Conversation-ID` trust path, which must remain separate from thread
  trust;
- `internal/outbound/compose.go` and `internal/outbound/sender.go`: reply
  headers, transport result, and provider wire ID;
- `internal/agent/api.go`: outbound acceptance, pending-review persistence, and
  the platform test-send source row;
- `internal/agent/selfsend.go`: both self-send twin rows created in one
  transaction;
- `internal/identity/local_delivery.go`: approved local/self-send delivery and
  its Inbox twin;
- `internal/httpapi/outbound.go`: reply/forward parent validation, send
  results, and error catalog;
- `internal/hitlworker/worker.go`: stored operation reconstruction.
  `sendRequestFromStoredMessage` currently copies `EmailMessageID`
  unconditionally, so TTL-approved forwards require a regression test proving
  they do not acquire reply headers. Its `attachReferencesChain` path also uses
  `GetMessageByEmailMessageID` and belongs in the RFC-ID audit;
- `internal/eventpayload`, `internal/webhookpub`, `internal/ws`, and
  `internal/httpapi/events.go`: additive event and notification projection;
- `web/src/app/(app)/inboxes/(view)/messages/page.tsx` and message components:
  current client-window grouping and the dashboard cutover;
- `api/openapi.yaml`, both SDKs, CLI, and MCP: coordinated public contract.

The implementation review should reject a helper that infers `thread_id` by
calling `LookupConversationID` or trusting `X-E2A-Conversation-ID`. Those paths
serve caller correlation and intentionally have different semantics.

`identity.Message.ThreadMessageID()` already means the RFC reply anchor
(`provider_message_id` outbound, `email_message_id` inbound), not e2a thread
identity. Work package 1 either renames it to `ReplyAnchorMessageID` or
documents the distinction at every new `Message.ThreadID` call site.

## Consequences

### Benefits

- The dashboard reflects email reply topology even when agents omit or change
  `conversation_id`.
- Backend agent frameworks retain full control of their own conversation/run
  correlation.
- Fresh sends no longer collapse because of subject or caller metadata.
- Thread decisions are stable, inspectable, and queryable without reparsing all
  headers on every read.
- Schema and response compatibility are additive and rollback-safe; the one
  terminal-reply behavior change is explicit, coordinated, and documented.

### Costs

- Every message writer and serializer must be audited.
- Legacy rows remain threadless unless directly adopted by new traffic, so
  old exchanges are intentionally not reconstructed.
- e2a and an external mail client can still disagree when headers are stripped,
  identifiers conflict, or client-specific subject heuristics intervene.
- Maintaining both `/threads` and `/conversations` requires precise
  terminology and SDK support.

These costs are preferable to overloading a caller-owned identifier or making
provider-specific thread IDs part of the public contract.
