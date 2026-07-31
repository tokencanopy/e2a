# Email Thread Identity

Status: proposed
Date: 2026-07-30
Owners: e2a maintainers
Surfaces: database, inbound SMTP, outbound delivery, existing HTTP message
reads, dashboard, generated SDK models, CLI JSON, MCP compatibility adapter

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

The schema and one response field are additive. Existing request fields,
status codes, reply behavior, and `conversation_id` semantics keep their
current meaning. This design adds no thread endpoint, request parameter,
webhook/event field, WebSocket field, CLI command, or MCP tool. The public
projection is optional, response-only `thread_id` on existing message list and
detail responses. It is explicitly beta, flows through the generated SDK
models and the CLI's SDK-shaped JSON output, and lets the dashboard group the
messages it loads. Human-readable CLI output and MCP tool output do not change.

There is no historical bulk backfill. Messages created after thread assignment
is enabled receive `thread_id`. An older threadless message is assigned one
only when a new message directly references it through an authorized API
reply, one exact RFC anchor, or authenticated platform-delivery-twin
correlation. That bounded lazy adoption is part of the new write transaction,
not a historical sweep.

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

1. Group messages loaded by the dashboard according to email reply topology
   after thread assignment is enabled, whether or not callers set
   `conversation_id`.
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
- Add public thread list or detail endpoints, a message filter by `thread_id`,
  or an SDK method for enumerating threads.
- Guarantee retrieval of a complete arbitrarily long thread in one API
  operation. The dashboard continues to use the existing paginated message
  list and can discover older members as it loads older pages.
- Add `thread_id` to webhooks, stored events, WebSocket notifications, data
  exports, human-readable CLI formats, or MCP tool schemas or output.
- Add a CLI command or flag for thread retrieval. Existing SDK-shaped CLI JSON
  may carry the beta field without introducing a new command surface.
- Automatically merge already-established threads after a late or conflicting
  ancestor arrives.
- Bulk-backfill historical messages or reconstruct old threads. Old rows stay
  null unless a new write directly references one through an authorized API
  reply, exact RFC anchor, or authenticated platform-test twin.
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

Invariant 9 applies to SMTP-derived exact resolution. API reply operations
continue using their existing reply-target eligibility rules, which exclude
held rows.

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

The corresponding internal `identity.Message` fields use `json:"-"`.
`UserExport.Messages` serializes `identity.Message` values directly, so an
ordinary JSON tag would unintentionally add `thread_id`,
`thread_parent_id`, or `rfc_message_id_key` to the stable account-export
payload. The public beta `thread_id` projection is instead defined explicitly
on the HTTP message view types.

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

`messages_agent_thread_created_idx` supports the bounded invariant-audit and
observability queries defined below. It does not imply or pre-build a public
thread-enumeration API.

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
- Validate generated and persisted values against `^thr_[0-9a-f]{32}$`.
- Never accept a `thread_id` in send, reply, forward, or inbound request data.
- Every internal thread lookup includes `agent_id`.

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
`rfc_message_id_key` afterward. A transition writes the canonical wire key only
when that path has a provider-qualified identifier proven to equal the emitted
`Message-ID`. Provider acceptance by itself does not make a bare provider
identifier a canonical RFC key.

The required provenance by transition is:

| Transition | Identifier available | `rfc_message_id_key` behavior |
| --- | --- | --- |
| Normal `MarkOutboundSentTx` | Qualified ID returned by the SMTP adapter | Write it in the sent-state transaction |
| Any synchronous approval path that directly sends | Qualified `SendResult` ID | Write it in the message-finalization transaction |
| Queue-first human or TTL approval | No wire ID at approval time | Assign thread identity at approval; let the later normal sent transition write the RFC key |
| `RecordProviderAcceptEvidenceTx` from SES feedback | Bare SES ID | Persist existing provider evidence, but write the RFC key only if the provider adapter first qualifies it using the same configured `MessageIDDomain` rule; otherwise leave the key null |
| `ResolveOutboundProviderAcceptedTx` and terminal reconciliation | No new wire identifier | Preserve any existing key; never synthesize, clear, or replace it |
| Send-attempt repair | Provider result may be qualified or bare | Propagate a key to `messages` only when the adapter proves the qualified form |

No transition changes the earlier thread decision. A crash-repair row for which
only a bare provider ID survives may therefore remain without a canonical
anchor and conservatively split on a later inbound reply.

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
Legacy inbound rows may likewise miss exact fallback when their stored raw
identifier differs only in domain-letter case from the token emitted by the
replying client; the feature does not apply a table-wide normalization or a
case-folding scan. The existing `conversation_id` behavior remains independent.
These limitations are intentional because the feature guarantees topology for
new messages when reliable anchors are available, not reconstruction of legacy
mail.

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

The human-review TTL worker currently diverges from the human-approval path:
`sendRequestFromStoredMessage` copies `EmailMessageID` into
`ReplyToMessageID` without checking the stored operation type. The
implementation must fix that pre-existing bug by copying reply anchors and
building `In-Reply-To` / `References` only when `message_type == "reply"`.
A forward must not acquire reply headers merely because its held row retains
the source `email_message_id` for review context.

### 4. Self-send delivery twin

When the platform creates both Sent and Inbox representations for one
self-send, both rows share one `thread_id`. Neither twin is the reply parent of
the other.

The thread is assigned before both records become externally observable.

### 5. Authenticated platform delivery twin

An outbound platform test message that returns through real SMTP may create a
physical inbound representation of the same message. That inbound twin copies
the source outbound thread only when all of the following hold:

- the recipient-visible, unsigned `X-E2A-Message-ID` value maps to an outbound
  row owned by the same agent;
- the inbound recipient maps to that same agent;
- the inbound SPF result passes for the envelope MAIL FROM domain;
- that authenticated envelope domain equals or is a subdomain of the
  deployment outbound domain stored on the platform-test source row;
- when the source row already has a canonical outbound anchor, the inbound
  message has its own canonical `Message-ID` and it equals that anchor;
- absence is tolerated only on the source row while its provider anchor has
  not yet been recorded, never as a waiver for a missing or conflicting
  inbound `Message-ID`.

`X-E2A-Message-ID` supplies correlation but no integrity: it is added
post-DKIM and can be copied by a recipient. The authenticated deployment-owned
envelope plus the on-wire-ID consistency check is therefore the security
proof. The consistency check is load-bearing against a tenant copying a
recipient-visible correlation header onto a different platform-delivered
message. The header alone, an envelope-domain string without a passing SPF
result, or a caller-supplied `conversation_id` is insufficient. Platform test
mail always uses the deployment-owned `noreply@<outbound-domain>` envelope;
the agent custom-MAIL-FROM path is not part of this correlation rule.
`thread_id` itself is never put on the wire.

If the correlated source outbound row is an old threadless row, authenticated
twin correlation is a permitted direct lazy-adoption trigger:
`EnsureThreadTx` locks and assigns the source, then the inbound twin copies the
returned thread. This is bounded to the directly correlated source and does
not traverse or stamp any historical relatives.

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
6. compute consensus over distinct non-null thread IDs only; if exactly one
   established thread is present, inherit it even when other matching rows
   remain null;
7. if matching rows have multiple established thread IDs, record ambiguity and
   continue to the next candidate;
8. if exactly one matching row exists and its `thread_id` is null, lock it
   through `EnsureThreadTx`, lazily mint its thread, and inherit the returned
   value;
9. if several matching rows all have null thread IDs, treat the candidate as
   ambiguous rather than choosing one arbitrarily; leave every null row
   unchanged;
10. set `thread_parent_id` only when one exact parent row was selected;
11. if no candidate resolves, mint a new thread.

The immediate `In-Reply-To` relationship has precedence over older
`References`. Within a header, the rightmost usable identifier is the nearest
ancestor.

When several rows share one candidate and one established thread, membership is
safe even if a unique direct parent cannot be chosen. In that case set
`thread_id` on the new message and leave `thread_parent_id` null. Matching null
rows are non-votes: they neither veto one established consensus nor become
implicitly adopted, and the write leaves them null. Several null rows with no
established thread never create vacuous agreement and remain an ambiguous
anchor on later traffic.

### Provider-anchor persistence window

An external reply can arrive after the provider accepted an outbound message
but before that outbound row's qualified wire ID was committed, especially if
the sender crashed in the SMTP-accept-to-mark-sent window. The reply then has a
valid `In-Reply-To` that no stored exact key can resolve. There is no safe
identifier available to correlate it at receipt time, so the inbound write
conservatively mints a new thread. The later provider-key repair does not
rewrite or merge that decision.

This is an accepted, pre-existing wire-evidence limitation, not a backfill
trigger. Persist the qualified key in the earliest transaction that has it and
count the unresolved inbound as `no_anchor`; do not add speculative matching or
a delayed thread rewrite. Older usable `References` entries can still recover
the established thread in a deeper exchange.

### 7. Idempotent retry

If a message already exists under an idempotency key, return its existing
message result and retain its existing internal thread identity. Never rerun
assignment in a way that changes the stored decision.

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

If an anchor disappears between candidate resolution and the row lock because
of a concurrent permanent purge, treat that candidate as unresolved and
continue to an older candidate or mint a new thread. A vanished anchor does not
fail the message write. A transient database or lock-timeout error is different:
the synchronous SMTP path returns a temporary failure and the asynchronous
worker retries; it must not silently mint a different topology decision.

The helper is called only from a new-message write that directly references an
old anchor through an API reply, exact RFC candidate, or authenticated delivery
twin. Reads never mutate historical messages, and the helper never walks or
assigns the anchor's old ancestors. Live API replies and live inbound
resolution use this same lock, so concurrent adoption converges on one value.

RFC parsing and candidate preparation may occur before the write transaction,
but the selected candidate set, consensus, row ownership, and null-to-thread
transition are revalidated inside the persistence transaction. Lock in one
order—selected anchor before new-message insert—and lock at most one unique
parent row per message. Indexed exact lookups and bounded parser input keep the
extra work finite on the synchronous pre-`250` path.

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

`thread_id` is an internal identity with a minimal read projection. RFC headers
remain the source of wire-client threading.

### Outbound replies

When its parent has a usable canonical wire anchor, an API reply must:

- set `In-Reply-To` to the exact qualified on-wire Message-ID of its parent;
- construct `References` from the parent's valid reference chain plus that
  parent ID;
- preserve the current chain construction and legal header folding behavior.

This feature does not introduce a `References` count or byte cap. Trimming a
long chain can remove a mid-chain anchor needed by a participant who did not
receive the immediate parent, so any future header-budget policy requires its
own provider evidence, compatibility analysis, and wire-level review.

### Outbound messages without a wire anchor

Preserve the existing reply lifecycle and error behavior:

| Usable canonical wire anchor | Submission state | Result |
| --- | --- | --- |
| Yes | Any state allowed by the existing reply lifecycle rules | Send the reply using that exact RFC parent |
| No | `accepted` or `sending` | Keep the existing retryable `409 message_not_yet_delivered` |
| No | Any other state currently accepted by the reply operation | Preserve the current send behavior without inventing `In-Reply-To` or `References` |

The last case still inherits the referenced message's internal `thread_id`
because the authorized API reply relationship is authoritative. It may appear
as a fresh root in external email clients because e2a has no reliable RFC
anchor to emit. That limitation already exists on the wire; this feature logs
the condition but does not turn it into a new API error or otherwise change
reply or forward authorization and lifecycle gates.

### Edited reply subjects

Human review currently permits subject edits. e2a's internal thread remains
correct because the operation references a message resource, but some external
clients may split a heavily edited subject despite valid RFC headers. Preserve
the API contract; document the best-effort wire behavior and consider a UI
warning rather than rejecting the edit.

## Minimal HTTP API projection

Add optional, response-only `thread_id` to exactly these existing message
representations:

- the message list summary returned by
  `GET /v1/agents/{email}/messages`;
- the message detail returned by
  `GET /v1/agents/{email}/messages/{id}`.

When a message has no assigned thread, serializers omit `thread_id`; they never
emit an explicit JSON `null`. Generated Python and TypeScript models treat it
as optional. The field is server-owned, and no request schema accepts it.

`thread_id` is a beta field on otherwise-stable response schemas:

- mark `MessageSummaryView.thread_id` and `MessageView.thread_id` with
  `x-stability-level: beta` through the existing field-level stability registry;
- describe the beta status in both generated SDK properties;
- keep the containing operations and schemas stable;
- use `thread_id` on the REST wire and the generated language convention
  (`threadId` in TypeScript and SDK-shaped CLI JSON).

Beta means the field may evolve or be removed before it is declared stable.
Its presence does not weaken the compatibility guarantees of unrelated
message properties.

Do not populate `thread_id` in send/reply/forward results, held-message review
payloads, webhooks, stored events, WebSocket notifications, data exports, or
MCP tool output in this feature. The CLI's JSON modes intentionally serialize
the generated SDK message models and therefore expose the beta field for
message list, get, and listen reads. Human-readable CLI output is unchanged.
`thread_parent_id` and `rfc_message_id_key` remain internal implementation
fields.

Do not add `/threads` endpoints or a `thread_id` filter to `/messages`.
Consequently, the API does not promise server-side thread enumeration or
complete thread retrieval. A caller can observe the identity on messages it
already reads, but the initial product consumer is the dashboard.

The current Go/OpenAPI types are reused across several operations:
`MessageSummaryView` also appears inside conversation detail, while
`MessageView` also backs review detail and restore responses. Add the optional
property to those existing shared schemas so generated SDK method return types
do not change. Populate it only in the message-list and message-detail
handlers; conversation, review, and restore handlers continue omitting it.
This means generated models tolerate the optional property anywhere the shared
schema is referenced, but no additional operation intentionally emits it.
Do not populate it inside the shared `messageSummaryFromIdentity` or
`messageViewFromIdentity` helpers; the two message-read handlers set it after
the shared conversion. Do not fork stable operation return types merely to hide
an optional property. The OpenAPI compatibility gate, field-level stability
tests, emitted-payload tests, and generated SDK diff are the acceptance tests.

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

The minimal response-field change lands together across:

- the Go message summary/detail views and serializers;
- OpenAPI;
- generated Python and TypeScript message models;
- SDK-shaped CLI JSON output and documentation;
- the dashboard.

CLI commands, flags, exit behavior, and human-readable formats remain
unchanged; only JSON gains the additive beta model property. MCP tool schemas
and output, webhook/event schemas, WebSocket payloads, and export schemas remain
unchanged. Because MCP `list_messages` currently returns generated SDK items
verbatim, it must switch to an explicit summary projection that omits
`threadId`; `get_message` already uses an explicit projection. Conversation,
review, and restore handlers also keep omitting the field despite their reuse
of shared OpenAPI components. Tests must prove that the underlying model
addition does not accidentally expand those emitted payloads.

Adding the optional response field is backward compatible. No request gains a
caller-writable `thread_id`, and no existing status code or operation behavior
changes.

## Webhooks, events, and WebSocket

These contracts do not change. They do not expose `thread_id`, add a routing
column for it, or resolve topology independently. A future proposal may add
event correlation only after an external consumer demonstrates the need.

## Dashboard behavior

After the new-write path and optional response field are deployed:

1. the dashboard continues loading the existing cursor-paginated `/messages`
   resource;
2. a row with non-null `thread_id` is grouped by `thr:<thread_id>`;
3. a legacy row with no `thread_id` uses the current
   `conv:<conversation_id>` or `orphan:<message_id>` fallback so it remains
   visible;
4. `conversation_id` is displayed only as optional workflow metadata for new
   threaded rows and never determines their group;
5. loading older message pages may add older members to an already-visible
   thread; the UI does not claim that a group is complete while more message
   pages remain;
6. trash rows carry optional `thread_id`, but the trash screen remains the
   existing flat list; grouping takes effect after a restored row returns to
   the inbox;
7. restoring a threaded message preserves its thread, while restoring a
   legacy null row leaves it null unless new traffic later references it;
8. existing `#conv:` / `#orphan:` URL fragments may no longer identify one
   unique group after the cutover, because one conversation can span several
   threads. A stale fragment safely falls back to the inbox list rather than
   guessing a thread; new selections use `#thr:<thread_id>`;
9. thread-state heuristics run over the new topology groups. In particular, a
   forward is a new root and no longer marks its source thread as
   `handed-off`; any future redesign of that state is separate UI work.

The absence fallback makes the dashboard compatible with an older server
during rollback without a new capability endpoint.

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
| External reply arrives before the outbound provider anchor is durably recorded | Conservative new thread unless an older `References` anchor resolves | No speculative match or later rewrite |
| Agent sends two fresh messages to the same recipient and subject | Two threads | Usually two client threads unless client heuristics merge |
| Agent sends two fresh messages with the same `conversation_id` | Two threads | `conversation_id` does not affect wire topology |
| Agent replies twice to one inbound message | One branched thread | Both replies reference the same parent |
| Agent replies to its latest outbound reply | Same thread once exact provider ID exists | New reply references that outbound message |
| Agent replies while outbound is still being submitted | Retryable 409 | No guessed RFC parent |
| Agent replies to terminally failed outbound with no provider ID | Inherit the referenced row's internal thread | Preserve current send behavior; without reply headers the client may show a fresh root |
| Forward an inbound or outbound message | New thread | Forward is a new root |
| Reply to the forwarded message | Forward's thread | Normal reply headers |
| Reply-all with changed CC/BCC | Same thread | New recipients see only content included in this message |
| Existing participant is removed | Same thread | Remaining clients use reply headers |
| New CC recipient is added | Same thread internally | No automatic historical body backfill |
| Self-send creates Sent and Inbox twins | One mailbox-local thread | One physical message |
| Platform test outbound returns through SMTP | One thread only with authenticated twin correlation | Same on-wire ID where available |
| A pre-deployment threadless platform test returns after enablement | Lazily assign the directly correlated source and copy its thread | No historical traversal |
| Agent A emails Agent B | Separate mailbox-local thread IDs | Each mailbox has its own topology projection |
| Same external message is delivered to two agents | Separate thread IDs | No cross-agent grouping |
| Multi-recipient outbound receives replies from several recipients | One sender-mailbox thread | Branches share the outbound root |
| Inbound reply is held for review | Thread chosen at receipt and preserved | Not visible in normal list until approved |
| Held reply is rejected | Hidden from normal thread; identity retained until purge | No outbound effect |
| Held reply is TTL-approved | Original thread retained | Reply headers are rebuilt from its parent |
| Held forward is TTL-approved | Forward's new thread retained | Worker emits no `In-Reply-To` or `References` |
| Idempotent API retry | Existing internal thread retained | No duplicate wire send |
| Provider delivery retry | Existing thread retained | Same logical message |
| Soft-delete then restore | Same thread | No topology change |
| Parent is soft-deleted before a reply arrives | Child can still resolve through hidden anchor | Parent remains hidden |
| Parent is permanently purged | Surviving child pointer is cleared; existing child thread remains stable | No deletion tombstone |
| Parent was purged before a new reply and no other ancestor survives | Conservative new thread | No deletion tombstone |
| `In-Reply-To` is absent but an older valid `References` anchor exists | Existing thread | Best-effort client continuity |
| Reply headers are stripped or malformed | New thread unless another valid anchor exists | Client may also split |
| Duplicate RFC ID rows all have one thread | Join that thread | Safe consensus |
| Duplicate RFC ID rows contain one established thread plus null rows | Join the established thread; leave null rows unchanged and omit direct parent | Null rows are non-votes |
| Duplicate RFC ID rows all remain null | Do not adopt any; try another ancestor or mint | Ambiguity remains until exact evidence changes |
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

### Duplicate Message-ID injection

A sender can deliberately reuse one `Message-ID` across different bodies.
Async intake deduplicates only the same
`(recipient, message_id, content_hash)` tuple, so reused IDs can still produce
several mailbox rows. The consensus rules fail closed: conflicting established
threads never merge, and several all-null rows remain untouched. The effect is
a display split within the target mailbox, not authorization, content
disclosure, or a cross-mailbox join. `ambiguous_anchor` is also the abuse
signal for this case.

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
  references it through an authorized API reply, one exact RFC anchor, or
  authenticated platform-delivery-twin correlation.
- That direct old row is locked and stamped in the same transaction as the new
  message; its historical parents, siblings, and conversation peers remain
  untouched.
- `/messages`, `/conversations`, trash, and historical events continue to
  expose legacy rows.
- Message list/detail clients always treat absent `thread_id` as a supported
  legacy state.
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
  old anchor or authenticated platform-test source.
- After every old writer has drained, verify no new-code writer creates an
  unexplained null.
- Preserve existing reply status codes and lifecycle behavior.

### Phase 3: minimal read projection

- Add optional beta `thread_id` only to existing message list/detail responses.
- Regenerate OpenAPI plus Python and TypeScript message models.
- Mark the two shared response properties `x-stability-level: beta`.
- Expose the beta SDK property in CLI JSON while proving CLI commands and
  human-readable formats did not change.
- Prove webhook/event, WebSocket, export, and MCP contracts did not change.
- Keep `/conversations` unchanged.

### Phase 4: dashboard enablement

- Group loaded rows by non-null `thread_id`.
- Preserve the existing conversation/orphan fallback for legacy null rows and
  older servers.
- Keep existing message pagination and make incomplete loaded history clear
  while older pages remain.

### Permanent nullable state

- Record Phase-2 assignment enablement only after traffic has cut over and
  every old writer has drained. Suppress or ratio-bound the null-thread alert
  during rollout and rollback. Rows written by an old binary during either
  window are supported threadless messages and may be lazily adopted later.
- Do not add a database `NOT NULL` constraint: historical rows are
  intentionally null and the message-read contract permanently supports
  absence.

### Rollback

Old binaries ignore the additive nullable columns. Rolling application code
back does not require removing or rewriting data. If the dashboard reaches an
older server, the absent-field fallback automatically preserves its current
conversation/orphan grouping. Do not drop columns or indexes as part of an
incident rollback. Lazily stamped old anchors remain harmless additive
metadata after rollback.

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

Periodic measurements must use bounded age windows or sampling rather than an
unbounded full-table aggregate. The thread index exists in part to support
those bounded scans. Before shipping retention pointer clearing, measure the
cost of updating surviving children in 5,000-row purge batches: every message
`UPDATE` invokes the storage-metering trigger even when body byte counts do not
change.

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
- terminal outbound replies without a usable anchor retain their existing
  status and send behavior while inheriting internal thread identity.

### Database integration tests

Every direct SQL message writer must assert `thread_id` behavior. Cover:

- inbound accepted, held, approved, rejected, and restored;
- queued, scheduled, sent, delivered, failed, and retried outbound messages;
- reply, reply-all, forward, test-send, and self-send;
- concurrent replies and concurrent inbound/API adoption of one old anchor;
- proof that deployment does not sweep unrelated old null rows;
- every provider-acceptance and reconciliation writer follows the provenance
  table: qualified paths populate the canonical key, bare/state-only paths
  leave it unchanged;
- soft delete, individual permanent purge, retention-batch purge, and
  whole-agent purge;
- duplicate canonical RFC IDs, including established-plus-null and all-null
  candidate sets;
- TTL-approved replies retain reply headers while TTL-approved forwards emit
  none.

### API contract tests

- old requests remain valid without `thread_id`;
- message list/detail include `thread_id` when assigned and omit it, rather
  than emit null, when absent;
- `thread_id` remains optional in generated Python and TypeScript message
  models and carries `x-stability-level: beta` on both shared OpenAPI
  properties;
- no request schema accepts `thread_id`;
- OpenAPI contains no `/threads` path or `thread_id` message filter;
- `/conversations` meaning and emitted payload stay unchanged; its reused
  generated message model merely tolerates the optional property;
- send/reply/forward, review, restore, webhook/event, WebSocket, export, and
  MCP emitted payloads do not populate `thread_id`/`threadId`;
- CLI message list/get/listen JSON exposes optional SDK `threadId`, while its
  human-readable output remains byte-compatible;
- the three internal `identity.Message` fields remain excluded from account
  export serialization;
- reply status codes and documented errors remain unchanged.

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
- terminal failed outbound preserves its existing API outcome;
- dashboard grouping remains correct as older `/messages` pages are appended.

Wire assertions must verify `Message-ID`, `In-Reply-To`, and `References`
independently of internal `thread_id`. The change must not regress the
qualified-ID behavior established by commit
`c618738d091da1a89fdd35a3b1f5cb3b2b11b925` and
`TestReplyThreadingSESMessageIDE2E`.

The shared contract-scenario interpreters cover the read projection: use
`body_match` for assigned beta fields and `body_excludes` to distinguish
omission from explicit null. RFC-header topology belongs in Go integration and
end-to-end tests unless all contract interpreters first gain equivalent raw
header-injection support.

### Production verification

Follow the production policy: use throwaway agents and inboxes only, never real
agent mailboxes or external personal addresses, and delete the throwaway agents
afterward. Verify the optional message-read field, dashboard grouping, and raw
received headers.

## Implementation work packages

The implementation should be reviewable in this order:

1. **Schema and domain primitives**
   - columns, indexes, ID generation, canonical RFC parser;
   - store types and invariant tests.
2. **Assignment on all write paths**
   - inbound, outbound, reply, forward, HITL, scheduling, self-send, retries;
   - transactional `EnsureThreadTx`.
3. **Wire-anchor correctness**
   - exact provider ID contract and per-transition provenance table;
   - preserved terminal no-anchor reply behavior;
   - unchanged `References` behavior;
   - authenticated twin correlation;
   - fix TTL-approved forwards so they emit no reply headers.
4. **Legacy coexistence and observability**
   - exact lazy adoption, no-sweep proof, metrics, invariant audit.
5. **Minimal message-read projection**
   - existing Go message list/detail views, OpenAPI, Python, and TypeScript;
   - beta field-level stability markers;
   - CLI JSON exposure;
   - negative contract checks for events, WebSocket, export, and MCP,
     including an explicit MCP summary projection.
6. **Dashboard cutover**
   - grouping over existing message pages, trash/restore handling, legacy
     fallback.
7. **Documentation cleanup**
   - distinguish application conversations from email threads throughout API
     docs and examples.

Each package must preserve nullable compatibility throughout rollout.

## Current code audit map

Implementation must begin with a fresh search for every direct
`INSERT INTO messages` and message-copy path. At the time of this decision, the
known high-risk touchpoints are:

Also classify direct and dynamic `UPDATE messages` helpers to prove that none
rewrites the three thread columns. Unrelated status/job-ID updates do not need
thread-assignment logic merely because they touch the same row; row creation,
copying, lazy adoption, and canonical-key transitions are the mutation paths
that require behavior changes.

- `internal/identity/store.go`: main message persistence, message reads,
  current `LookupConversationID`, conversation aggregation, trash, and restore;
- `internal/identity/delivery_store.go`: delivery-state transitions and a
  historical provider-ID wildcard lookup that must not be reused for threads;
- `internal/identity/send_attempts.go` and
  `internal/outboundsend/terminal_reconcile.go`: provider-acceptance repair
  paths that must follow the per-transition provenance table and never
  canonicalize a bare provider ID implicitly;
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
- `internal/httpapi/messages.go` and message view types: the only new public
  REST projection, limited to existing list/detail responses and populated
  after—not inside—the shared identity-to-view helpers;
- `internal/httpapi/outbound.go`: regression coverage that reply/forward
  status codes, results, and error behavior remain unchanged;
- `internal/hitlworker/worker.go`: stored operation reconstruction.
  `sendRequestFromStoredMessage` currently copies `EmailMessageID`
  unconditionally, so the implementation must add the same
  `message_type == "reply"` gate as the human-approval path and prove with a
  regression test that TTL-approved forwards do not acquire reply headers. Its
  `attachReferencesChain` path also uses `GetMessageByEmailMessageID` and
  belongs in the RFC-ID audit;
- `internal/eventpayload`, `internal/webhookpub`, `internal/ws`,
  `internal/httpapi/events.go`, and export serializers: negative contract
  checks proving no thread projection was added;
- `web/src/app/(app)/inboxes/(view)/messages/page.tsx` and message components:
  grouping over the existing paginated message window;
- `internal/httpapi/stability.go`, its stability/golden tests,
  `api/openapi.yaml`, and both generated SDKs: optional beta field parity for
  the existing message list/detail schemas;
- CLI: expose the SDK property in JSON without changing commands, flags, exit
  behavior, or human-readable formats;
- MCP: keep tool schemas and output unchanged; replace the raw
  `list_messages` SDK-item passthrough with an explicit projection that omits
  `threadId`.

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
- Schema and response compatibility are additive and rollback-safe; existing
  request, reply, event, WebSocket, export, CLI command/human-output, and MCP
  behavior is preserved while SDK and CLI JSON readers may observe the beta
  field.

### Costs

- Every message writer and serializer must be audited.
- Legacy rows remain threadless unless directly adopted by new traffic, so
  old exchanges are intentionally not reconstructed.
- e2a and an external mail client can still disagree when headers are stripped,
  identifiers conflict, or client-specific subject heuristics intervene.
- The dashboard still groups only the message pages it has loaded; there is no
  server-side thread enumeration or one-call complete-thread retrieval.
- Public clients can observe `thread_id` on messages but receive no thread
  resource or thread-oriented query surface.

These costs are preferable to overloading a caller-owned identifier or making
provider-specific thread IDs part of the public contract.
