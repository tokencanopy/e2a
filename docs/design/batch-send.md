# Batch Send — Design Spec and MVP Implementation Plan

**Status:** Draft. Awaiting mentor review before implementation.
**Scope:** Add a batch-send endpoint that accepts up to 100 independent messages in one request. Each recipient receives its own SMTP-separate message with its own `message_id`, its own state machine, and its own River retry envelope. Reuses the existing async pipeline; introduces a parent `batches` row and a `batch_id` correlation.
**Not in scope for MVP** *(reserved for post-MVP polish)*: batch-level HITL approval, batch cancellation, `listBatches`, `wait=sent` variant for batch. Per-item content, per-item templates, per-item attachments, per-item cc/bcc/reply_to — all **ARE** in MVP as a natural consequence of the Resend-shaped request (each batch item is a self-contained near-copy of `SendEmailRequest`, inheriting the single-send content model). See §14 Q8 for the shape rationale.

**Background.** Today `POST /v1/agents/{email}/messages` sends ONE message with up to 50 combined `to/cc/bcc` recipients delivered in a single SMTP envelope — every recipient sees the other addresses in the message headers ([internal/httpapi/outbound.go:179](../../internal/httpapi/outbound.go), `maxRecipients=50`). AI agents driving outbound need N *independent* messages: each recipient receives their own separate email and cannot see the other recipients. e2a's positioning is specifically the "agent-first" case — the [README](../../README.md) tagline is *"Give your AI agents a real, authenticated email address"* and every product concept (conversation threading, HITL, per-agent screening, 10-day retention, in-DB attachments) is built around **1-on-1 agent↔human conversations**, not marketing broadcasts. AI agents' unique capability is programmatic per-recipient content generation (LLM-generated outreach, personalized transactional emails, per-user follow-ups), so batch send's dominant use case is "N heterogeneous 1-on-1 sends in one API call" — not "1 shared body to a subscriber list." There is no batch write path in the /v1 surface today (`api/openapi.yaml`, `internal/httpapi/*`). The async-send contract already reserved a `batch_id` correlation field on webhook events for this feature (see [`async-send-contract.md` §4.4](async-send-contract.md)). This document specifies the contract, the reconciled all-or-nothing semantics, the storage/lifecycle model, and a phased implementation plan that reuses the existing River outbound pipeline verbatim so the new surface area is confined to the accept side of the wire.

## 0.5 Terminology

Definitions used throughout this doc — surfaced up-front so readers unfamiliar with e2a's template system, mail-merge semantics, or the Resend-vs-SendGrid axis can follow §14 without side-quests.

- **Template** — a stored subject/text/html body with `{{variable}}` placeholders. Implemented in [internal/emailtemplate/emailtemplate.go](../../internal/emailtemplate/emailtemplate.go) as a hand-rolled restricted subset of Mustache: `{{ident}}` (HTML-escaped) and `{{{ident}}}` (raw); no loops/conditionals/partials — those syntaxes are reserved parse errors so a future upgrade stays additive. Currently **Beta**. Rendered server-side at accept-tx time with `template_data`. Referenced from a send request via `template_id` (opaque) or `template_alias` (human handle); mutually exclusive with literal `subject`/`text`/`html`. **Batch send does not add any new template features** — it inherits the existing beta template system per batch item.
- **Fanout** — the pattern where one API call produces **N *independent* outbound messages**, one per recipient, each with its own `message_id`, its own SMTP envelope, its own retry envelope. The recipient of one message never sees the addresses of the others. Contrast **envelope batching** (cc/bcc), which produces ONE message with N recipients visible to each other. This document's `POST /batches` implements fanout. Fanout does NOT imply shared content — each item can be fully independent (see mail-merge below).
- **Mail-merge** — fanout **with per-recipient content variation**: recipient A gets content_A, recipient B gets content_B, etc. In our design, mail-merge is **native to the MVP shape** — each `BatchMessage` item in the request carries its own subject/text/html (or its own `template_id`+`template_data`), so per-recipient variation is expressed as "different bodies per item" rather than a server-side templating loop. Callers who want fully identical content across recipients just duplicate the shared body across items.
- **Resend-shape vs SendGrid-shape** — two competing wire shapes for a batch endpoint. Resend's `/emails/batch` accepts N complete, independent `Email` objects (the "bulk-submit N single-sends" model). SendGrid's `/mail/send` with personalizations accepts ONE shared content block + a `personalizations[]` array with per-recipient overrides (the "one email to a list" model). **e2a batches (MVP) is Resend-shape** — see §14 Q8.
- **`BatchMessage`** — the request-body sub-type for one item in a batch. Field-for-field a near-clone of `SendEmailRequest` minus `from` (agent is the path param, shared across the batch) — so `to`/`cc`/`bcc`/`subject`/`text`/`html`/`template_id`/`template_alias`/`template_data`/`reply_to`/`conversation_id`/`attachments` all exist per item, with the same caps and XOR rules as single-send.
- **Accept-tx** — the single Postgres transaction inside `handleSendBatch` that (a) inserts the `batches` row, (b) inserts up-to-N `messages` rows (one per non-suppressed item), (c) enqueues that many River jobs, (d) commits the idempotency-key completion. Either all four succeed or none do. See §9.
- **Screening** — the outbound protection step that produces a verdict per message: `Block` (refuse), `Review` (hold for HITL), `Flag` (send with annotation), or `Allow`. Configured via `internal/httpapi/protection.go` `ProtectionConfigView`. Batch send's HITL-refuse gate (§5.1) and block-whole-batch rule (§14 Q14) both derive from this verdict; screening runs **per item** because each item has its own content.

## 1. Public API contract

### 1.1 Endpoint

- `POST /v1/agents/{email}/batches` — `operationId: sendBatch`. Registered in `internal/httpapi/outbound.go` alongside `sendMessage`. The `{email}` path parameter selects the sending agent, identical resolution to `sendMessage` (ownership check via `resolveOwnedAgent`).
- `GET /v1/batches/{batch_id}` — `operationId: getBatch`. Returns the batch header + per-status counts (§7.2).
- `listMessages` (`GET /v1/messages`) grows an optional `batch_id` query filter (§7.3).

Only the POST and GET-batch endpoints add surface area; the list filter is one column added to an existing cursor struct.

### 1.2 Request shape (`SendBatchRequest`)

```jsonc
{
  "messages": [
    {
      "to":       ["alice@example.com"],
      "subject":  "Alice, your Q3 report is ready",
      "text":     "Hi Alice, ...",
      "html":     "<p>Hi Alice, ...</p>"
    },
    {
      "to":       ["bob@example.com"],
      "template_alias": "welcome",
      "template_data":  { "name": "Bob", "plan": "Pro" }
    },
    {
      "to":       ["carol@example.com"],
      "subject":  "Ping",
      "text":     "Carol, quick follow-up on yesterday...",
      "reply_to": "carol-thread@yourdomain.com",   // per-item override wins over batch-level default
      "attachments": [ { "filename": "notes.pdf", "content_base64": "..." } ]
    }
    // 1..100 items
  ],
  "reply_to": "support@yourdomain.com"     // OPTIONAL batch-level default; each item may override
}
```

- `messages[]` — **required**, length in `[1, 100]`. Each item is a `BatchMessage` — field-for-field a near-clone of `SendEmailRequest` minus `from`. See §0.5 Terminology for the full field list. All XOR rules from single-send apply per item (literal `subject`/`text`/`html` XOR `template_id`/`template_alias`+`template_data`).
- **Per-item independence is native to MVP** — each item carries its own `to`/`cc`/`bcc`, content (literal or template), attachments, `conversation_id`, and `reply_to`. Callers who need mail-merge produce N items with N different bodies (or N different `template_data` maps against the same `template_alias`); callers who want identical content across recipients duplicate the body across items.
- **Batch-level fields that apply as defaults**:
  - `reply_to` — if the batch body sets `reply_to` and an item leaves it unset, the batch value applies. Per-item `reply_to` always wins if present. This is the ONLY MVP batch-level default; every other content field must live on the item. Rationale: `reply_to` is the field most callers want uniform (e.g. `support@…`) and it's the only single-value scalar where duplication would be pure boilerplate.
  - Future batch-level defaults (§11 polish) may add `conversation_id` propagation if telemetry shows it, but MVP keeps the surface minimal.
- **Attachment size ceiling (§14 Q15 decision)**: each item honors the single-send per-item cap (≤10 attachments per item, single attachment ≤10 MiB, item combined ≤25 MiB — matching `SendEmailRequest`). ADDITIONALLY the **batch-level combined attachment bytes across all items must be ≤ 25 MiB**. Callers can distribute freely: 100 items × 250 KiB, 5 items × 5 MiB + 95 empty, one item with 25 MiB + 99 empty (all equivalent to a "shared attachment" scenario) — all valid.
- Header: `Idempotency-Key` supported (path+body-hash scheme, `internal/idempotency/store.go:120`); replay returns the original 202 response verbatim.
- The `Prefer: return=minimal` / `wait=sent` shortcut from single-send is **not** offered in MVP (§2.4 rationale).

The sending agent (`from` of every message) is the path agent — no `from` in the body, same as single-send ([internal/httpapi/outbound.go:864-866](../../internal/httpapi/outbound.go)). This is the single field that stays batch-uniform because it derives from the URL, not the body.

### 1.3 Response shape (`SendBatchResponse`)

`202 Accepted` — batches always return 202 at MVP (no synchronous shortcut). The body:

```jsonc
{
  "batch_id": "bat_...",
  "results": [
    { "message_id": "msg_..." },
    { "message_id": "msg_..." },
    { "suppressed": { "address": "spammy@example.com", "reason": "hard_bounce" } },
    { "message_id": "msg_..." }
    // ... length == len(request.messages)
  ],
  "accepted":   98,
  "suppressed_count": 2
}
```

- `batch_id` — durable id, format `bat_<26-char base32 lower>` (same alphabet as `msg_` per project convention).
- `results[]` — **positionally aligned with `request.messages`**. Each slot is a discriminated union:
  - **Accepted item**: `{ "message_id": "msg_..." }` — a `messages` row was inserted and a River `outbound_send` job enqueued.
  - **Suppressed item**: `{ "suppressed": { "address": "...", "reason": "..." } }` — the (first) recipient address in this item hit the suppression list; no `messages` row exists. `reason` is the suppression-list category (`hard_bounce` / `complaint` / `unsubscribe` / `manual`) as recorded in the `suppressions` table.
  This shape preserves per-item correlation (the i-th request item produced `results[i]`) while making the caller's success/skip branching explicit.
- `accepted` — count of `results[]` entries whose shape is `{ message_id }`. Redundant but useful for logs and metrics.
- `suppressed_count` — count of `results[]` entries whose shape is `{ suppressed }`. Also redundant; useful for zero-check without walking `results[]`.

**Per-item suppression semantics** — when a `BatchMessage` has multiple recipients (`to`/`cc`/`bcc`) and ANY one of them is on the suppression list, the whole item is dropped (`results[i]` becomes `{ suppressed }`). This matches the single-send behavior where a suppressed recipient in the envelope fails the whole message; batch send does NOT partially drop recipients within a single item's SMTP envelope.

### 1.4 Response codes

Send-time (accept-tx) — **all-or-nothing on validation** (§2.1):

| HTTP | Code | Trigger |
|---|---|---|
| **202** | — | Batch durably persisted; up-to-N messages enqueued (some may be suppressed per-item, reported via `results[]` — §1.3) |
| **400** | `invalid_request` | Body malformed; XOR (literal vs template) violated on any item; item field misses per-item validation |
| **400** | `invalid_recipient` | Any recipient address in any item fails RFC 5322 parse. `details.item_index`, `details.address` |
| **400** | `too_many_messages` | `len(messages) > 100` or `< 1`. `details` = `TooManyMessagesDetails { max_messages: 100, provided }` — new typed detail (§8) |
| **400** | `duplicate_recipient` | Same recipient address appears in two different items' `to` sets (batch-wide dedup — see §14 Q11). `details.address`, `details.item_indices` |
| **400** | `domain_not_verified` | Agent's sending domain isn't verified |
| **402** | `limit_exceeded` | Batch would exceed the account's monthly send quota (measured as N, §4.2) |
| **403** | `blocked_by_policy` | Screening returns Block for any item — content-scan block on that item's content, or a per-item recipient-policy block under `action=block`. `details.item_index`, `details.reason` (§14 Q14, all-or-nothing) |
| **403** | `batch_hitl_unsupported` | *(new code, §5)* Agent has HITL enabled — MVP refuses batch |
| **409** | `idempotency_in_flight` | Same `Idempotency-Key` currently running |
| **413** | `payload_too_large` | ANY per-item attachment cap exceeded (single attachment ≤10 MiB, item combined ≤25 MiB) OR **batch-level combined attachment bytes > 25 MiB** (§14 Q15). `details.scope` = `item` \| `batch`; on `item`, `details.item_index`; on `batch`, `details.computed_batch_bytes` + `details.max_batch_bytes` |
| **422** | `idempotency_key_reuse` | Same key + different body |
| **429** | `rate_limited` | Adding N would exceed the agent's send-per-minute rate limit. `details.retry_after_seconds` (existing shape) |

Delivery-time (post-accept, per-message) — same webhook events, per-message `email.sent`/`email.failed`/`email.deferred` etc., **each carrying `batch_id`** (already reserved in `async-send-contract.md §4.4`).

## 2. Semantics — the reconciled "all or nothing"

The word "all-or-nothing" has two failure classes hiding under it. The MVP treats them differently on purpose.

### 2.1 Structural errors → all-or-nothing (reject the whole batch)

Validation runs **per item** for content-related checks, but any single failure rejects the whole batch. If **any** of the following is true, the request is rejected with an appropriate 4xx **before** any DB row is inserted:

- Malformed body / unknown field / any item violates the `SendEmailRequest` schema (missing required fields, wrong types, XOR of literal vs template violated) → 400 `invalid_request`. `details.item_index` when the failure is per-item.
- Any recipient address in any item fails RFC 5322 parse → 400 `invalid_recipient` with `details.item_index` + `details.address`
- `len(messages) > 100` or `< 1` → 400 `too_many_messages` (new code)
- Same recipient address appears in the `to` set of two different items → 400 `duplicate_recipient` (**not silently deduplicated** — silent dedup would send N-k messages when the caller asked for N and there's no way to know whether the duplicate was intentional; see §14 Q11). Only cross-item duplicates in `to` are checked; duplicates in cc/bcc across items are allowed (matches how real callers cc a common address across items).
- Any item exceeds per-item attachment caps (single attachment > 10 MiB, item combined > 25 MiB) → 413 `payload_too_large` with `details.scope = "item"`, `details.item_index`
- **Batch-level combined attachment bytes across all items > 25 MiB** → 413 `payload_too_large` with `details.scope = "batch"`, `details.computed_batch_bytes`, `details.max_batch_bytes` (§14 Q15)
- Sending domain not verified → 400 `domain_not_verified`
- Screening produces a **block** verdict for any item — either that item's content trips a content-scan block, or any recipient in that item's envelope is not in the outbound `allowlist`/`domain` gate under `action=block` → 403 `blocked_by_policy` with `details.item_index`, `details.reason` (§14 Q14 all-or-nothing; block-only agents still reach batch, HITL agents were already refused at §5.1)
- Adding N sends would exceed rate limit or plan quota → 429 `rate_limited` / 402 `limit_exceeded` (see §4)
- Agent has HITL enabled (`OutboundPolicyAction=="review"` OR `OutboundScanSensitivity!="off"`, §14 Q13) → 403 `batch_hitl_unsupported` (§5)

Frozen invariant: **if a 4xx is returned from `sendBatch`, zero messages exist.** Callers can retry with the same `Idempotency-Key` after fixing the input without worrying about half-sent state.

### 2.2 Business filters → per-item skip (do NOT reject the whole batch)

The only per-item filter in MVP is **suppression list membership** (existing `recipient_suppressed` semantics on single-send). Rationale:

- The suppression list is compliance infrastructure that e2a *maintains on the caller's behalf* — hard-bounces, complaints, unsubscribe list entries. In a real 100-address list from any long-running product, 1-3% will be suppressed. All major North American ESPs (Postmark, SendGrid, Mailgun, Resend, SES) handle this per-item, not per-batch. Rejecting the batch on the ESP's own compliance work punishes the caller for e2a doing its job.
- Suppression is checked at send-time against the `suppressions` table, not derivable from the request body — so it belongs to the "per-item business filter" class, distinct from structural validation.

Behavior:
- Any `BatchMessage` item whose `to` envelope contains a suppressed address is dropped in its entirety. No `messages` row is inserted for that item; no River job is enqueued. The item's slot in `results[]` becomes `{ suppressed: { address, reason } }` (§1.3).
- `accepted` = number of items whose slot in `results[]` is `{ message_id }`; `suppressed_count` = number of slots that are `{ suppressed }`.
- If *every* item is suppressed, still return 202 — every `results[]` slot is `{ suppressed }`, `accepted: 0`, `suppressed_count: N`. This is a legitimate outcome per §14 Q9 (the caller submitted valid input; e2a's own compliance layer filtered everything).
- If a single item has multiple recipients (`to` array of length ≥ 2, or cc/bcc addresses) and ANY of them is suppressed, the whole item is dropped. Batch send does NOT partially prune recipients from within an item's SMTP envelope — that would silently change what the caller asked to send.

### 2.3 Delivery-time semantics (post-accept)

Once accepted, each message is a standalone entity. Each has its own River `outbound_send` job (`OutboundSendArgs{ MessageID: msgID }`, [internal/outboundsend/worker.go:57-61](../../internal/outboundsend/worker.go)). Retries, snoozing, terminal reconciliation are unchanged from single-send:
- 6 attempts with 30s → 2m → 10m → 1h → 4h backoff ([worker.go:33-42](../../internal/outboundsend/worker.go))
- Outage-class errors snooze 5m without burning attempts, up to a 72h horizon from `messages.created_at`
- 4xx SMTP → transient (retry); 5xx SMTP → permanent (JobCancel + `markFailed`); connection-class → outage (snooze)
- Terminal reconciler ([terminal_reconcile.go](../../internal/outboundsend/terminal_reconcile.go)) sweeps stuck rows every 1 min

No batch-level retry envelope exists. If 3 of 100 messages permanently fail SMTP (5xx), those 3 emit `email.failed` webhook events; the other 97 continue independently. This IS partial success at the delivery layer — and that is unavoidable for email (once a message is out the door you cannot un-send it).

### 2.4 No `wait=sent` in MVP

Single-send offers `wait=sent` (poll up to 15s, up to 20s ceiling per async-send-contract §2.3). Batch does not:
- The polling shape (`PollSendOutcome`, [outbound.go:760-786](../../internal/httpapi/outbound.go)) is single-message. Extending to N-message poll would multiply worker load and add contract complexity for a use case (agent fires 100 emails then blocks 20s) that isn't real.
- Batch is inherently asynchronous by user intent — the caller is fanning out, not waiting on delivery.

## 3. Data model

### 3.1 New `batches` table

```sql
CREATE TABLE batches (
  batch_id        TEXT        PRIMARY KEY,                                         -- 'bat_<base32>'
  user_id         TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,     -- batch owner
  agent_id        TEXT        NOT NULL REFERENCES agent_identities(id) ON DELETE CASCADE,  -- sending agent
  requested       INTEGER     NOT NULL,                                            -- len(request.messages)
  accepted        INTEGER     NOT NULL,                                            -- requested minus suppression drops
  suppressed_json JSONB       NOT NULL DEFAULT '[]',
    -- [{ item_index: int, address: str, reason: str }] captured at accept.
    -- Mirrors the response `results[]` slots whose shape is { suppressed }.
  request_id      TEXT        NOT NULL DEFAULT '',                                 -- for audit trail
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX batches_user_created_at_idx  ON batches (user_id,  created_at DESC);
CREATE INDEX batches_agent_created_at_idx ON batches (agent_id, created_at DESC);
```

**Type corrections from the initial draft.** The initial draft referenced `accounts(account_id)` and `agents(agent_id)` with `UUID` FKs. Neither exists in this codebase — the actual ownership chain is `users.id` (TEXT, `usr_...`) → `agent_identities.id` (TEXT, `agt_...`); there is no `accounts` table (the word appears only in the /v1 `AccountView` API resource, which is a projection over users + limits + usage). The schema above uses the real column shapes and reference targets; migration `067_batches.sql` embeds this SQL.

Rationale for storing `suppressed_json` on the batch row (not per-message): a suppressed item produces NO `messages` row, so there is no other durable place to record the drop. The batch row is the only place that remembers "item i was in your request but we skipped it because address X hit the suppression list."

### 3.2 `messages` gains `batch_id`

```sql
ALTER TABLE messages ADD COLUMN batch_id TEXT
  REFERENCES batches(batch_id) ON DELETE SET NULL;
CREATE INDEX messages_batch_id_idx ON messages (batch_id) WHERE batch_id IS NOT NULL;
```

- NULL for single-sends. Non-null for batch children.
- `ON DELETE SET NULL` — deleting a batch row must not cascade-delete messages (messages are the record of what was sent; a batch is metadata). The 90-day retention/janitor sweep for messages is unchanged.

### 3.3 State model

**Messages** — no change. Same `accepted → sending → sent → {delivered, deferred, bounced, complained, failed}` from [internal/delivery/status.go](../../internal/delivery/status.go). The batch itself has no lifecycle state — it is a static header describing what was accepted. Rollup is computed on read (§7.1).

## 4. Rate limiting and quota

### 4.1 The two limits, and where each applies

e2a has two capacity concepts (from [internal/httpapi/errors.go](../../internal/httpapi/errors.go) and [outbound.go:64-66](../../internal/httpapi/outbound.go)):

- **`rate_limited` (429)** — throughput. Currently 60 sends/agent/minute (sliding window, `sendLimit` in [internal/agent/api.go:448](../../internal/agent/api.go)). Retryable.
- **`limit_exceeded` (402)** — monthly quota / plan. Not retryable.

### 4.2 How batch counts against them

**A batch of N `BatchMessage` items counts as N sends against BOTH limits.** (Suppression-filtered items don't count — the accept-time reservation uses `len(keep)`, not `len(messages)`.) Rationale:

- Rate limits and quotas are throughput/capacity signals. A batch of 100 puts the same load on downstream SMTP as 100 individual sends — same DKIM signings, same SMTP conns from the worker pool, same billing units. Counting a batch as "1" would create arbitrage: a user could bypass rate-limiting by wrapping their sends in `batches` calls.
- The industry standard for send-count / quota limits is per-message; batch is a *transport* optimization (fewer HTTP round trips), not a *rate* one. Postmark, SendGrid, Mailgun all count per-message against send quotas.

### 4.3 Rate-limit check happens at accept, not per-item

`checkSendLimit(agentID, count=N)` is called once, before any DB writes. If it would exceed, the whole batch is rejected with 429 (§2.1). Retry-after is the existing `retry_after_seconds` computed by [internal/ratelimit/ratelimit.go:40-72](../../internal/ratelimit/ratelimit.go). The current `AllowWithRetryAfter` takes one hit at a time; we add a variant `AllowN(count int)` that atomically reserves N slots or rejects.

### 4.4 HTTP-layer request rate limit — out of scope

If e2a later adds a request-level rate limit (like Postmark's 100 req/min API cap), that layer *should* count a batch as 1 (it's one HTTP request). No such limit exists today; noted for completeness.

## 5. HITL interaction

HITL (human-in-the-loop review) is e2a's differentiator — an agent's outbound can be gated on a reviewer's approval before the SMTP submit ([internal/agent/api.go](../../internal/agent/api.go), `HoldForApprovalCore`). Batch send must not silently break HITL guarantees; the MVP decision is to sidestep the interaction entirely:

### 5.1 MVP: refuse batch for HITL-enabled agents

If `agentUsesHITL(agent)` returns true — i.e., `OutboundPolicyAction == "review"` **or** `OutboundScanSensitivity != "off"` (see §14 Q13 for the frozen formula and rationale) — `sendBatch` rejects with:

- **HTTP 403** `batch_hitl_unsupported` (new code)
- `details.hint`: `"batch send is not available for agents with HITL enabled; use single-send POST /v1/agents/{email}/messages per recipient, or disable HITL on this agent"`

**Block-only agents (`Gate.Action == "block"` with `Scan.Sensitivity == "off"`) are NOT considered HITL** and DO reach the batch send path. Block verdicts encountered during accept-time screening fail the whole batch with 403 `blocked_by_policy` per §14 Q14 — block is an automated policy filter, not a review workflow.

Rationale: HITL creates a per-message review UI that is designed for one-at-a-time inspection. A batch of 100 messages would either (a) create 100 review items — usability nightmare for the reviewer, (b) create one collective review — new UI surface, or (c) short-circuit review — silently unsafe. All three are v1-polish scope, not MVP.

### 5.2 Polish v1: batch-level review (deferred)

Reviewer sees one review item for the whole batch (template + recipient count + first 3 recipients preview + a "show all N" link). One approve/reject decision releases or fails the whole batch. This is what Salesforce campaign send-approval looks like and matches how humans actually want to review "send X to 500 people."

### 5.3 Polish v2 (may never ship): per-message review

Every message in a batch becomes an independent review item. Only useful for tightly regulated verticals (legal, medical). Deferred until a customer asks.

## 6. Idempotency

### 6.1 Same shape as single-send

- `Idempotency-Key` header, `(user_id, key)` scope, 24h TTL, hash covers path + raw body — no code changes to [internal/idempotency/store.go](../../internal/idempotency/store.go).
- Replay (`OutcomeReplay`) returns the cached 202 body byte-for-byte, including `batch_id` and `message_ids`. `Idempotent-Replayed: true` header set.
- Body mismatch → 422 `idempotency_key_reuse`.
- Concurrent same-key → 409 `idempotency_in_flight`.

### 6.2 Complete inside the batch accept-tx

The idempotency completion (`CompleteTx`, [store.go:270-282](../../internal/idempotency/store.go)) is called inside the same DB transaction that inserts the batch row + all N message rows + enqueues all N River jobs. If the tx fails, no state changes; caller sees a 5xx and can retry safely with the same key.

## 7. Observability

### 7.1 `GET /v1/batches/{batch_id}` — batch header + status rollup

Response body:

```jsonc
{
  "batch_id":  "bat_...",
  "agent_id":  "agt_...",
  "requested": 100,
  "accepted":  98,
  "suppressed": [
    { "item_index": 1,  "address": "spammy@example.com",     "reason": "complaint" },
    { "item_index": 47, "address": "hardbounce@example.com", "reason": "hard_bounce" }
  ],
  "created_at": "2026-07-16T21:58:06Z",
  "status_rollup": {
    "accepted":   5,
    "sending":   10,
    "sent":      40,
    "delivered": 30,
    "deferred":   3,
    "bounced":   10,
    "complained": 0,
    "failed":     0
  }
}
```

`status_rollup` is computed on read via a single grouped query against `messages` (indexed on `batch_id`). Not cached — batch observation is a rare operation (poll after send), cheap enough at 100 rows per batch.

### 7.2 `listMessages?batch_id=...`

Adds `batch_id` to the existing `messagesCursor` filter struct ([internal/httpapi/messages.go:368-382](../../internal/httpapi/messages.go)). Cursor pagination over batch children, same shape as unfiltered list. Necessary for callers who want per-recipient detail beyond the aggregate rollup.

### 7.3 Webhook events

Every send-related event (`email.sent`, `email.failed`, `email.deferred`, `email.delivered`, `email.bounced`, `email.complained`) already carries an optional `batch_id` per async-send-contract §4.4. Batch children populate this field; single-sends leave it empty. **No new event types.** Callers subscribe once and their existing handlers work for batch outputs unchanged.

### 7.4 No `GET /v1/batches` list endpoint in MVP

We defer a "list all my batches" endpoint. Callers who want batch history can query messages filtered by any predicate; batch is a header, not a first-class entity worth listing. If demand emerges post-GA, add `listBatches` following the standard cursor pattern.

## 8. Error contract additions

Three new codes must be added to [internal/httpapi/error_catalog.go](../../internal/httpapi/error_catalog.go):

- `too_many_messages` — HTTP 400, retryable=false. New typed detail (batch-specific; distinct from single-send's `too_many_recipients` which stays intact):
  ```go
  type TooManyMessagesDetails struct {
      MaxMessages int `json:"max_messages"`
      Provided    int `json:"provided"`
  }
  ```
- `duplicate_recipient` — HTTP 400, retryable=false. New typed detail:
  ```go
  type DuplicateRecipientDetails struct {
      Address     string `json:"address"`
      ItemIndices []int  `json:"item_indices"`   // cross-item positions of the duplicate `to`
  }
  ```
- `batch_hitl_unsupported` — HTTP 403, retryable=false. `details.hint` (existing `HintDetails` shape).

Two existing error codes need per-item `details.item_index` extension to disambiguate WHICH item in a batch tripped the check:

- `invalid_request`, `invalid_recipient`, `template_render_failed`, `payload_too_large`, `blocked_by_policy` — extend their existing typed details with an optional `ItemIndex *int json:"item_index,omitempty"` field. Present only on batch responses; absent on single-send responses (backward-compatible).

`payload_too_large` additionally gains a `Scope` discriminator (`"item"` | `"batch"`) to distinguish per-item cap breaches from the batch-level 25 MiB cap (§14 Q15).

No changes to the envelope shape ([internal/httpapi/errors.go:40-61](../../internal/httpapi/errors.go)); adding new codes and extending typed details is expected under the "open set" policy documented at `errors.go:57`.

## 9. Accept-transaction sketch

The single DB transaction the batch handler runs, in order. Each step listed as its own numbered item; short-circuit on the first failure with the mapped 4xx. Because each `BatchMessage` item is a near-clone of `SendEmailRequest`, most steps below reuse the single-send helpers **called in a loop** rather than being reimplemented — this is the biggest architectural win of the Resend-shape.

1. **Idempotency claim** — `idempotencyGuard.Claim(request)`. If replay, return cached 202. If in-flight, 409. If mismatch, 422.
2. **Structural validation** — parse request; then for each item run `validateOutboundBody(item)` (existing single-send validator at [internal/httpapi/outbound.go](../../internal/httpapi/outbound.go)): RFC 5322 on `to`/`cc`/`bcc`, XOR of literal vs template, size caps on subject/text/html, attachment per-item caps. First failure short-circuits with 400 `invalid_request` or `invalid_recipient` (`details.item_index` set). Batch-level structural checks: `len(messages) ∈ [1, 100]` (else 400 `too_many_messages`); cross-item `to` duplicate detection (else 400 `duplicate_recipient`); sum of all items' `attachments[].size_bytes` ≤ 25 MiB (else 413 `payload_too_large` with `details.scope="batch"`, §14 Q15).
3. **HITL gate** — call `agentUsesHITL(agent)` (§14 Q13 formula). If true → 403 `batch_hitl_unsupported`.
4. **Domain verification** — `checkDomainVerified(agentID)` (existing single-send code).
5. **Per-item screening (block whole-batch on any block verdict)** — for each item, call `screenOutbound(agent, item.composedContent, item.envelopeAddresses)` (existing `screenOutbound` at [internal/agent/api.go:1248](../../internal/agent/api.go)) with THAT item's content and recipient list. If ANY item returns `verdict.Block()`, reject the whole batch with 403 `blocked_by_policy` (`details.item_index` set) and emit an audit event per single-send convention. §14 Q14 all-or-nothing: block never silently drops in batch. Screening is called **N times in a loop** because each item's content and recipient set differ — but the underlying screener is the single-send one, unchanged.
6. **Suppression check** — collect the `to` addresses from all items, SELECT from `suppressions` for the union in one query, partition items into `keep[]` (no address suppressed) and `suppressed[]` (any address in `to` suppressed). Per §14 Q9, an all-suppressed batch (`len(keep) == 0`) is a valid 202 with all `results[]` slots as `{ suppressed }`.
7. **Rate/quota reservation** — `checkSendLimit(agentID, count=len(keep))` reserves the throughput slots atomically (`AllowN(count)` variant, §4.3); `checkMonthlyQuota(accountID, count=len(keep))` on the plan quota. Attachment storage per §14 Q10 is content-hash deduped — sum unique-hash bytes across the batch's attachments and call `checkAttachmentStorageQuota(accountID, uniqueBytes)`; egress cost is naturally captured by the N-multiplied `checkSendLimit`. All rejections release any partial reservations.
8. **Per-item template render** (for items using `template_id`/`template_alias`) — for each such item, call the existing single-send `resolveTemplate(item)` + `emailtemplate.Render(item.template_data)` — a per-item render, once per item. `template_render_failed` on any item's failure short-circuits with 400 (`details.item_index`). Items using literal `subject`/`text`/`html` skip this step. This is the mail-merge behavior described in §0.5: N different `template_data` maps against the same template alias produce N different rendered bodies.
9. **Prepare `MessageForAccept` structs for each item in `keep[]`** — one struct per accepted item, each with its own minted `message_id` (`ulid`), its own rendered content (from step 8 or the literal fields), its own `to`/`cc`/`bcc` envelope. `batch_id` is minted here too (a `bat_<ulid>` value); every prepared struct references it.
10. **Open DB tx** and inside it:
    - a. INSERT `batches` row (id = the minted `batch_id`, `requested = len(messages)`, `accepted = len(keep)`, `suppressed_json` = the drop list with `{item_index, address, reason}`)
    - b. Bulk-INSERT `len(keep)` `messages` rows with `batch_id`, `delivery_status='accepted'` (single round trip via `CreateOutboundMessagesTx`, §10 Phase B)
    - c. Do NOT insert `message_recipients` at accept-time (matches single-send — those rows are written at `MarkSent`, [internal/identity/delivery_store.go:156-195](../../internal/identity/delivery_store.go)).
    - d. `outboundEnq.EnqueueBatchTx(ctx, tx, msgIDs)` — enqueues `len(keep)` `OutboundSendArgs` in one `river.InsertManyTx` call. The `outbound_send` worker is unchanged — it sees N distinct jobs with distinct `MessageID` args.
    - e. `idemCompleteTx` — commit the idempotency-key completion with the 202 response body cached (`batch_id`, `results[]`, `accepted`, `suppressed_count`).
11. **Commit** — on success, return 202. On any error, tx rolls back; no state changes; the reservation from step 7 is released; caller sees the appropriate 4xx/5xx.

**Consistency note on step 10c** — single-send inserts `message_recipients` rows at `MarkSent` time (post-SMTP), not at accept ([internal/identity/delivery_store.go:156-195](../../internal/identity/delivery_store.go)). Batch follows the same pattern for consistency: don't pre-populate `message_recipients` at accept-tx. Rollup queries and per-recipient status pages continue to work off `messages` for the pre-SMTP window.

**Complexity note** — steps 2, 5, 8 all iterate N times but call unchanged single-send helpers. The batch handler is thus roughly `handleSendMessage in a loop + batch-level cross-item checks (dup, attachment sum, rate reservation) + one bulk-INSERT + one InsertManyTx`. This is the concrete meaning of "Resend-shape reuses the single-send code path" — it's a loop, not a reimplementation.

## 10. Implementation plan (2 PRs)

Ships as **2 PRs** — matching the project's convention of landing a coherent feature per PR (cf. PR #536 `feat(auth): generic external-auth`, PR #370 `Add per-domain sending ramp-up` — both single-PR features with internal commit sequencing). Every commit that touches the spec runs `make spec` + `make generate-sdk`; contract drift is already CI-gated.

### PR 1: Design doc + backend feature

One PR delivering the complete server-side batch-send capability. Commits are ordered so the reviewer reads the design doc first, then follows the implementation top-down.

**Commit sequence:**

1. **`docs/design/batch-send.md`** — this file. Lands as the first commit so any reviewer can sign off on the shape before reading a single line of code.
2. **OpenAPI spec + error catalog** — `sendBatch` and `getBatch` operations; `SendBatchRequest` / `SendBatchResponse` / `BatchView` / `BatchMessage` / `BatchResult` schemas in `api/openapi.yaml`; register `too_many_messages`, `duplicate_recipient`, `batch_hitl_unsupported` in `internal/httpapi/error_catalog.go`; fixture JSONs under `api/fixtures/errors/`; regenerate SDK bases via `make generate-sdk` (hand-written wrappers land in PR 2). Contract-only, no runtime behavior.
3. **Migration + storage layer** — `migrations/NNN_batches.sql` (new `batches` table + `messages.batch_id` column + index). `internal/store` methods: `CreateBatchTx`, `GetBatch`, `BatchStatusRollup(batchID)`, and the bulk-insert `CreateOutboundMessagesTx(msgs []MessageForAccept)` (bulk variant of the single-row inserter at [identity/delivery_store.go](../../internal/identity/delivery_store.go)).
4. **Handler + accept-tx** — `internal/httpapi/outbound.go` registers `sendBatch` and implements `handleSendBatch` following §9; `internal/agent/api.go` gets `DeliverBatch(ctx, req)` (the batch analog of `DeliverOutbound` driving §9 steps 3–9); `internal/outboundsend/jobs.go` gets `EnqueueBatchTx(ctx, tx, msgIDs)` wrapping `river.InsertManyTx`; `internal/ratelimit/ratelimit.go` gains `AllowN(count int)` (§4.3).
5. **Observability** — `getBatch` handler in `internal/httpapi/`; `messagesCursor` extended with a `batch_id` filter and wired into `handleListMessages`; `PublishTx` argument plumbing so batch children populate the `batch_id` field on webhook event payloads (field is already reserved in the event schema per `async-send-contract.md §4.4`).
6. **Tests** — contract tests for the happy path (100 accepted) and every validation error in §1.4; storage-rollback tests injecting failure at each step of §9 (verify zero orphan state); Mailpit-based integration in `tests/` (submit 5-recipient batch → 5 SMTP deliveries → 5 `email.sent` events all carrying the same `batch_id` → `GET /v1/batches/{id}` rollup shows 5 delivered); prober scenario for a scheduled loopback batch.
7. **User-facing docs** — `docs/api.md` gains a batch section; `docs/events.md` notes that `batch_id` is now populated on batch children.

**Feature flag.** Ships behind `E2A_BATCH_SEND_ENABLED` (default `false`) — merge and enable are separable (§13).

**Estimated size:** ~20–25 files across 7 commits.

**Fault line if the mentor asks to split.** A natural break sits between commits 4 and 5 — cut into "design + spec + backend core" (commits 1-4) and "observability + tests + docs" (commits 5-7). Still 3 PRs total, still fewer than the phased-6-PR plan.

### PR 2: Clients (SDKs + CLI)

Depends on PR 1 merged. Adds hand-written client wrappers now that the generated bases are stable.

- **TS SDK** (`sdks/typescript/src/`) — `client.batches.send(...)` and `client.batches.get(...)` over the generated base; types re-exported at the top-level entry.
- **Python SDK** (`sdks/python/`) — the same for both sync and async clients.
- **CLI** (`cli/src/`) — `e2a batch send --agent <email> --messages messages.jsonl` reading JSON-lines input where each line is one `BatchMessage`; exit codes follow the frozen contract (0 = 202 accepted, 3 = validation, 4 = server error), matching `e2a send`.
- SDK-level tests hit a mock server; CLI exit-code test.

**Estimated size:** ~10–15 files across 3 languages.

---

**Total: 2 PRs.** Design + backend + observability + tests + user docs in one coherent PR; clients in a follow-up. If PR 1 review feedback surfaces a scope disagreement, splitting at the fault line above is a documented option.

## 11. Post-MVP polish (deferred, listed for scope clarity)

Two items previously listed here — per-recipient template variables and per-recipient content overrides (cc/bcc/reply_to/subject/attachments) — are **ALREADY IN MVP** as a natural consequence of the Resend-shaped request (each `BatchMessage` is a self-contained near-clone of `SendEmailRequest`, so per-item content variation is expressed by having the caller send different bodies per item). What remains in polish is genuinely orthogonal to the request shape:

- **Batch-level HITL review** — §5.2. Adds a `pending_review` state to the batch row, a batch review item in the reviewer UI, one approve/reject decision releasing the whole batch. Unlocks batch send for HITL-enabled agents (currently refused at MVP per §5.1).
- **Batch cancellation** — `POST /v1/batches/{id}/cancel`. Sets all still-`accepted` children to a terminal `cancelled` state, evicts their River jobs. Only useful if we build (a) review-held batches (needs the HITL polish above) or (b) a scheduled-send feature.
- **`listBatches`** — cursor endpoint over `batches` for the "show me my recent batches" dashboard use case. Follows the standard cursor pattern (`internal/httpapi/messages.go:342-382`). Deferred because in the meantime `listMessages?batch_id=…` already answers most callers; `listBatches` adds convenience, not capability.
- **Batch-level shared field expansion** — if telemetry shows callers frequently duplicate the same field (e.g. `conversation_id`, tags, a common `attachments` list) across every item, add batch-level defaults + per-item override for those fields. Pure wire-efficiency, no new semantics. MVP ships with `reply_to` as the only batch-level default per §1.2.
- **`wait=sent` for batch** — probably never; the shape is a bad fit (§2.4). Listed only so a future reader knows this was considered and dropped, not overlooked.

**Note on template polish** — the template system itself (Mustache subset, beta status, no loops/conditions) is orthogonal to batch send. Any template-engine polish (adding `{{#if}}`/`{{#each}}`, promoting from beta to stable, adding partials/includes) is a template-system project, not a batch-send project. Batch send inherits whatever the template system supports at that moment — no additional work on batch send is needed to consume future template features.

## 12. Testing plan (summary — details in Phase F)

- **Contract tests** — every 4xx/5xx path from §1.4; happy path with `Idempotency-Key`; replay; mismatch; in-flight.
- **Storage tests** — accept-tx rollback under simulated failure at each step of §9 (idempotency, message insert, River insert, complete). Verify zero orphan state after rollback.
- **Integration tests** — end-to-end via Mailpit: submit 5-recipient batch → verify 5 SMTP deliveries → verify 5 `email.sent` webhook events all carrying same `batch_id` → verify `GET /v1/batches/{id}` rollup shows 5 delivered.
- **Load smoke** — one batch of 100, measure accept-tx latency at p50/p95 (target: <100ms accept, all 100 River jobs enqueued in-transaction).
- **Prober scenario** — a scheduled canary that fires a batch to loopback and verifies terminal states.

## 13. Rollout / migration notes

- Purely additive to the /v1 surface — no back-compat concerns. Callers who ignore `batches` see no change.
- The `messages.batch_id` column is nullable; existing rows and existing single-send inserts are unaffected.
- Feature can ship dark: register the endpoint but keep it behind an internal feature flag (e.g., `E2A_BATCH_SEND_ENABLED=false` default) until Phase F testing passes. Flag flip is a config change, no code redeploy.

## 14. Design decision journal (frozen 2026-07-16)

This section is the record of *why* the spec looks the way it does — the questions raised while designing this feature, the options weighed, the decisions taken, and the reasoning behind each. It is written for a future maintainer (or the reviewer at GA+6mo) who needs to understand whether a decision can be revisited without breaking a load-bearing invariant.

Organized in three tiers:
- **Tier 1 — Foundational framing**: what "batch send" and "all or nothing" actually mean. (Q1, Q2)
- **Tier 2 — Strategic model choices**: MVP semantics with reference to North-American ESP norms (suppression, rate limits, HITL, attachments, provider modeling). (Q3–Q8)
- **Tier 3 — MVP detail decisions**: contract-level details asked and answered before Phase C. (Q9–Q15)

### Tier 1 — Foundational framing

#### Q1. What does "batch send" actually mean in e2a's context?

- **Options considered.**
  - (a) *Envelope batch (cc/bcc)*: one message with N recipients on to+cc+bcc, one SMTP envelope — each recipient sees the other addresses.
  - (b) *Fanout*: N *independent* messages, one recipient each (or one small recipient group each), N SMTP envelopes — recipients do not see each other.
- **Decision:** **(b) Fanout.** Batch send introduces one API call that creates N independent messages, each with its own `message_id`, its own content, and its own SMTP delivery.
- **Reasoning.** Envelope batching (a) already exists on `POST /v1/agents/{email}/messages` with a combined 50-recipient cap ([internal/httpapi/outbound.go:179](../../internal/httpapi/outbound.go)). Every AI-agent outbound use-case (LLM-personalized outreach, per-user transactional notifications, per-lead follow-ups) requires (b) — recipient A must not see recipient B in the message headers, and their content is almost always different from A's. All North-American ESPs offering "batch" (Resend `/emails/batch`, SendGrid mail-merge, Postmark batch API) implement (b). Naming this feature "batch send" was consistent with peer terminology.
- **What fanout does NOT imply.** Fanout is orthogonal to whether content is shared or per-item. Both Resend's shape (fully independent content per item) and SendGrid's personalizations shape (shared content template + per-recipient variables) are fanout patterns. The content-model choice is Q8, not Q1.

#### Q2. What does "all or nothing" mean concretely?

- **Options considered.**
  - (a) *Both accept-time and delivery-time all-or-nothing*: if any of N recipients ends up as an SMTP failure (bounce, defer, permanent 5xx), retract the entire batch and mark all N failed.
  - (b) *Only accept-time all-or-nothing*: the ACCEPT decision is binary (batch is fully accepted or fully rejected); once accepted, each message goes through the normal per-message retry pipeline independently.
- **Decision:** **(b) Accept-time all-or-nothing; delivery-time per-message.** See §2.1 vs §2.3.
- **Reasoning.** (a) is physically impossible for email — once a message is submitted to an upstream MTA, it cannot be un-sent. Retract-on-partial-failure is not a real option; the mentor's brief could only have meant (b). This interpretation also aligns with the existing e2a error contract, which has NO partial-success envelope shape ([internal/httpapi/errors.go](../../internal/httpapi/errors.go)) — a batch endpoint that returned "partial-success at accept" would introduce a shape inconsistent with every other /v1 endpoint. Peer providers (Resend, SendGrid, Postmark) all take shape (b).

### Tier 2 — Strategic model choices (semantics with commercial context)

#### Q3. Suppression list hit — reject the batch or skip per-item?

- **Options considered.**
  - (a) *Batch-level*: any suppressed recipient in the batch → reject entire batch (strict all-or-nothing).
  - (b) *Per-item*: drop suppressed recipients, accept the rest, report drops in a `suppressed[]` response field.
- **Decision:** **(b) Per-item skip.** See §2.2.
- **Reasoning.** Suppression is *compliance infrastructure e2a maintains on the caller's behalf* — hard-bounces, complaints, unsubscribe list entries, mandated by CAN-SPAM (US) and CASL (Canada). Every major North-American ESP handles suppression per-item, not per-batch: **Postmark** rejects the individual message with per-message error; **SendGrid** silently skips and reports in Activity Feed; **Mailgun** skips and logs; **Resend** returns per-item errors; **AWS SES** silently skips. Rejecting the whole batch on the ESP's own compliance work is punishing the caller for e2a doing its job — a 1-3% suppression rate is normal on any long-running contact list, and the caller cannot pre-filter (the suppression store is server-owned). This is the one legitimate "per-item" concession inside an otherwise all-or-nothing accept contract; the response's `suppressed[]` array preserves per-caller visibility.

#### Q4. Rate limit — count a batch as 1 request or N sends?

- **Options considered.**
  - (a) *Count as 1*: batch is one API call, decrement rate-limit budget by 1.
  - (b) *Count as N*: batch consumes N slots against send-per-minute throughput and monthly send quota.
- **Decision:** **(b) Count as N.** See §4.
- **Reasoning.** The industry standard is **two distinct rate limits**: an *HTTP request rate* (per-second/minute API cap) that counts a batch as 1, and a *send quota* (per-hour/day or per-account) that counts as N. e2a's current `sendLimit` at 60/min/agent ([internal/agent/api.go:448](../../internal/agent/api.go)) is the second kind — send throughput, not API request throughput — so a batch must consume N slots. Counting as 1 would create arbitrage: two users at identical downstream load see different rate-limit behavior based on whether they wrap sends in a batch. Postmark, SendGrid, and Mailgun all use "count as N" for their send quotas. If e2a later adds an HTTP-request-layer rate limit, that layer *should* count a batch as 1 (§4.4) — orthogonal.

#### Q5. Batch size cap — 100, 500, or 1000?

- **Options considered.** 100 (Resend); 500 (mid-range); 1000 (SendGrid personalizations).
- **Decision:** **100 for MVP.** See §1.2.
- **Reasoning.** 100 matches Resend's cap (the closer peer, Q8). Larger caps increase accept-tx latency (100 message inserts + 100 River enqueues fit comfortably in a single Postgres transaction; 1000 does too but with less headroom for concurrent workload). Reversible upward — raising the cap post-GA is additive; lowering it is a breaking change.

#### Q6. HITL interaction — batch-level review, per-message review, or refuse for HITL-enabled?

- **Options considered.**
  - (a) *Per-message review*: batch of 100 creates 100 review items in the reviewer UI.
  - (b) *Batch-level review*: one review item for the whole batch (template + recipient count + first-N preview), one approve/reject decision releases or fails the whole batch.
  - (c) *Refuse batch for HITL-enabled agents*: 403 error, caller must use single-send.
- **Decision:** **MVP = (c). v1 polish = (b). v2 (maybe never) = (a).** See §5.
- **Reasoning.** HITL is e2a's differentiator; batch send must not silently break its guarantees. (a) is a usability nightmare — a reviewer reading 100 near-identical drafts is worse than paging through them one-at-a-time. (b) is the correct long-term shape (matches Salesforce's campaign-approval UX, HubSpot's send-approval flow) but requires new review-UI surface area — an entire vertical of scope not needed to prove the batch primitive works. (c) is the "scope isolation" move: refuse the interaction, document the escape hatch (disable HITL or use single-send per recipient). MVP ships without touching the HITL system at all. HITL detection formula is Q13.

#### Q7. Attachments — shared, per-item, or reference-based?

- **Options considered.**
  - (a) *Batch-level shared*: attachments live in the batch body, same one blob delivered to every item.
  - (b) *Per-item*: each `BatchMessage` carries its own `attachments[]` (Resend's shape). Each item can have zero attachments, or the same, or completely different.
  - (c) *Reference-based (media library)*: upload once out-of-band, reference by attachment id from within the batch — client can compose per-item references cheaply.
- **Decision:** **(b) Per-item with a batch-wide 25 MiB total ceiling.** See §1.2, §14 Q15.
- **Reasoning.** With Q8's decision to adopt Resend-shape (each item is a near-clone of `SendEmailRequest`), per-item attachments come for free — inherited directly from the single-send request model. The pathological case where per-item + no-ceiling would blow up ("100 items × 25 MiB = 2.5 GiB request body") is bounded by adding a **batch-level combined ≤ 25 MiB cap** on top of the existing per-item cap (Q15). Callers who want the shared-attachment shape (one 25 MiB PDF to 100 recipients) express it by putting the attachment on one item and referencing it in the others — or, more commonly, by keeping their batches modest in total attachment weight. Option (a) is subsumed: it becomes a client-side convention rather than a wire-shape restriction. Option (c) requires a separate `POST /attachments` endpoint that e2a does not have; it's deferred to a future storage-attachment redesign, unrelated to batch send.
- **Reversibility.** Fully reversible in both directions: adding a batch-level shared `attachments[]` field as an optional default (like `reply_to` in §1.2) is a superset; tightening the batch-level cap or removing per-item attachments is a breaking change and hard to reverse.

#### Q8. Should batch-send follow Resend's or SendGrid's model?

The honest answer separates two axes — **request shape** (what the wire looks like) and **API philosophy** (what the endpoint is trying to be). Our design lands **Resend-shape on the wire, Resend-philosophy on the defaults, and stricter-than-both on error handling**. This is the single biggest architectural decision in this document; it drives the shape of §1.2, §1.3, §9, and §11.

- **Peer models on the wire.**
  - **Resend `/emails/batch`**: N complete, independent `Email` objects in one call. Each item carries its own `to`/`subject`/`body`/`attachments`. The batch has no "shared content" concept — the mental model is "bulk-submit N single-sends." Cap: 100. Idempotency: yes.
  - **SendGrid `/mail/send` with personalizations**: ONE shared content block (subject + body + template ref) + `personalizations[]` — a per-recipient list where each entry has `to`, optional `substitutions` (variables), optional per-recipient overrides. Cap: ~1000 personalizations. Server-side Handlebars-like mail-merge is the intended primary use.

- **Where our MVP lands on each axis.**

  | Dimension | Resend `/emails/batch` | SendGrid personalizations | **e2a batches (MVP)** |
  |---|---|---|---|
  | Request shape | N independent items | 1 shared content + N personalizations | **N independent `BatchMessage` items** ← Resend-shape |
  | Per-item variables / body | ✅ (native — each item has full body) | ✅ (`substitutions` maps against shared content) | **✅ (native — each item can carry its own body OR its own `template_data`)** |
  | Per-item subject/body override | ✅ | ✅ | ✅ (each item's `subject`/`text`/`html` is its own) |
  | Per-item attachments | ✅ | ❌ | ✅ (each item's `attachments[]` is its own; batch-wide 25 MiB total cap — Q15) |
  | Batch cap | 100 | ~1000 | **100** — Resend-aligned |
  | Idempotency | ✅ | ❌ | ✅ (reuses e2a's Idempotency-Key) |
  | Error return | per-item errors returned | complex per-personalization errors | **all-or-nothing on validation + per-item `suppressed[]`** — stricter than either |
  | Server-side mail-merge language | ❌ (client renders) | ✅ (Handlebars-like) | Mustache subset per item (existing beta) — no server-side loops/conditions |
  | API-shape "vibe" | minimal, opinionated | rich, many knobs, marketing-oriented | **minimal, opinionated** — Resend-aligned |
  | Dominant use case | "Bulk-submit heterogeneous sends from an application" | "Mail-merge to a subscriber list" | **"AI-agent fanout: N heterogeneous 1-on-1 sends generated per-recipient"** ← Resend fit |

- **Decision (frozen).** MVP adopts **Resend request shape** (each `BatchMessage` is a self-contained near-clone of `SendEmailRequest`) with **Resend API philosophy** (100-item cap, idempotency, minimal per-item knobs, opinionated defaults, no server-side mail-merge language), and a **stricter-than-both error model** (accept-time all-or-nothing on validation; per-item skip only for suppression per §2.2). The only field promoted to batch-level as a default is `reply_to` (§1.2 rationale).

- **Why Resend-shape fits e2a specifically.**
  - **The README says so.** The tagline is "*Give your AI agents a real, authenticated email address*." Every product concept — conversation threading, HITL per-message review, in-Postgres attachments (no S3/GCS), 10-day retention, outbound-body scrub-on-terminal — describes **1-on-1 agent↔human transactional conversations**, not marketing broadcast. There is no `list`/`audience`/`campaign`/`segment` concept anywhere in the /v1 surface. e2a's users are not marketing operators; they are developers building AI agents that generate content programmatically per-recipient.
  - **AI agents' unique capability is per-recipient generation.** An LLM can trivially produce 50 different outbound emails ("outreach to Alice about her recent order X" / "outreach to Bob about his recent order Y") without a template system — that's the value of driving email with an agent. A batch API that forces a shared content block would leave that capability unreachable through `batches`, sending callers back to a single-send loop. SendGrid-shape's central affordance (mail-merge templates + variables) is aimed at a persona (marketing operator) that e2a explicitly does not target.
  - **Reuse of the single-send code path.** Each `BatchMessage` is field-for-field almost a `SendEmailRequest`. The batch handler is roughly *"validate/screen/render each item via the existing single-send helpers in a loop, then one bulk-INSERT + one River `InsertManyTx`"* — see §9. This is measurably less new code than a SendGrid-shape handler that would need to introduce a shared-content + per-recipient-overrides merge layer that has no analog in single-send.
  - **Templates still work.** MVP inherits the existing beta template system directly at the item level — an item can set `template_alias` + `template_data` and get server-side rendered. So callers who WANT the shared-template-with-per-recipient-variables model can achieve it by (a) creating a template once via `POST /v1/templates`, (b) putting the same `template_alias` on every item with different `template_data`. That's mail-merge, expressed one level up.

- **Why NOT SendGrid-shape.**
  - The killer use case for SendGrid personalizations is "one campaign to a subscriber list of 500, rendered from a Handlebars template." e2a does not have subscriber lists, campaigns, or Handlebars. Adopting the shape would import a wire language that maps to zero features.
  - SendGrid-shape MVP would force AI-agent callers with per-recipient content to fall back to a single-send loop for their most common use case — degrading `batches` from "the way batch is done" to "the way announcements are done." That's a product-fit failure.

- **Why stricter-than-both on errors.**
  - Both peers return per-item errors uniformly. Our model is: **validation failures reject the whole batch** (§2.1) and **only suppression is a per-item skip** (§2.2). The mentor's brief explicitly asked for all-or-nothing semantics, and e2a's already-frozen `/v1` error contract has NO partial-success envelope shape ([internal/httpapi/errors.go](../../internal/httpapi/errors.go)) — introducing one just for `/batches` would break contract symmetry.

- **Reversibility.** Shape reversal to SendGrid-shape is a **major breaking change** (wire shape + accept-tx flow both differ). Reversal to per-item errors is a superset (previously-4xx-ing input becomes 202 with per-item error slots) — safe additively. Adding batch-level shared defaults beyond `reply_to` (attachments, `conversation_id`) is additive — safe. In short: the shape choice is expensive to walk back; the error-strictness choice can be relaxed later without hurting callers.

- **Correction from earlier drafts.** An earlier iteration of this doc chose SendGrid-shape MVP under the assumption that e2a's dominant use case was "one shared body to a list." Closer reading of the README + FAQ + data-handling posture made clear that e2a's dominant use case is per-recipient per-item content — the opposite fit. The 2026-07-16 revision reverses that choice and rewrites §1–§9 to match.

### Tier 3 — MVP detail decisions

Contract-level questions asked during the design pass. Reversibility noted per item — decisions marked "reversible" can be flipped post-GA without breaking clients; decisions marked "hard to reverse" would need a codegen bump.

#### Q9. All-suppressed edge case — return 202 with `accepted: 0`, or 422 `all_recipients_suppressed`?

- **Decision: 202 with `accepted: 0`.** `suppressed[]` in the response body carries all 100 dropped addresses.
- **Reasoning.** `body.status` / `accepted` is the discriminator per the project's "always branch on body, never on HTTP status" doctrine (`async-send-contract.md §2.1`). The request itself is legitimate — the drop is compliance work e2a does on the caller's behalf, not a caller mistake — so a 4xx would mis-classify it. Client code stays simple: parse the success response, branch on `accepted > 0`.
- **Reversibility.** Reversible (flipping to 422 is one branch in the handler + a new error-catalog entry). Document intended behavior in `api.md`.

#### Q10. Attachment cost — 1 unit or N units against quota?

- **Decision: split by cost layer.**
  - **Physical storage quota (accept-time)**: **1 unit** — content-hash dedup means one physical blob regardless of N.
  - **Send/egress quota**: **N units** — captured naturally by `checkSendLimit(count=N)` (each SMTP submission is one egress event).
  - **Commercial billing**: **out of scope for this spec** — orthogonal, deferred to pricing/product.
- **Reasoning.** The quota check must reflect actual server-side cost, not customer-facing billing. Physical storage is genuinely 1× (dedup); egress is genuinely N× (each SMTP recipient is a separate submission). Conflating the two — either 1× everywhere or N× everywhere — is dishonest either about resource use or about pricing.
- **Reversibility.** Fully reversible; quota checks are internal calls, no wire-format implication.

#### Q11. Duplicate recipient — reject with 400 or silently dedupe?

- **Decision: reject with 400 `duplicate_recipient`.** `details.address` + `details.indices` name the offending entry.
- **Reasoning.** Duplicates are usually a caller-side bug (bad list construction, CSV double-paste). Silent dedupe hides the bug indefinitely; explicit rejection surfaces it once so the caller fixes their input. Matches Resend and Postmark. Also matches e2a's "opinionated request-side validation" stance (request bodies are strict, `additionalProperties: false`; responses are open — [internal/httpapi/protection.go](../../internal/httpapi/protection.go) preamble discusses this asymmetry).
- **Reversibility.** Reversible in the caller-friendly direction — adding silent dedupe with a new `deduplicated[]` response array is a superset of the current behavior (previously 400ed input now 202-succeeds), so no existing client breaks.

#### Q12. Batch of 1 — allow `len(messages) == 1`, or forbid?

- **Decision: allow.** `len(messages) ∈ [1, 100]`.
- **Reasoning.** SDK ergonomics. A caller with a variable-length `users[]` writes `batches.send({messages: users.map(u => buildMessage(u))})` without a branch on `if (len == 1) use single-send`. The cost is one extra row in `batches` per length-1 batch — negligible. Semantic purity ("a batch should be plural") does not justify the boilerplate every caller would otherwise write.
- **Reversibility.** **Hard to reverse.** Forbidding `len == 1` post-GA would break every caller using the natural pattern above. If a change is ever needed, it must be forward-compatible (e.g., server-side auto-forward to single-send).

#### Q13. HITL detection — single bool on the agent record, or derived from protection config?

- **Decision: derived from protection config, per-flag.**
  ```go
  // Batch send is refused iff the agent's outbound protection can produce a Review verdict.
  // Content-scan-enabled agents can hold on shared content; review-gated agents can hold on
  // recipient policy. Block-only configs are NOT HITL (no human in the loop) — those go through
  // §2.1's block-trigger path instead (§14 Q14).
  func agentUsesHITL(ag *identity.AgentIdentity) bool {
      return ag.OutboundPolicyAction == "review" ||
             ag.OutboundScanSensitivity != "off"
  }
  ```
- **Reasoning.** No single "HITL enabled" bool exists on the agent. HITL is a derived property of the outbound protection config (`ProtectionConfigView`, [internal/httpapi/protection.go](../../internal/httpapi/protection.go)): action=review OR scan!=off can produce a review-hold. action=block is *automated* denial (no human in the loop), so is NOT HITL and does NOT refuse the batch — block-triggered messages go through Q14's whole-batch reject instead. Missing scan sensitivity here would let content-scan-configured agents silently accept a batch that then floods the review queue.
- **Correction from the initial draft.** The reference to `internal/protectionconfig/` in an earlier draft was wrong — no such package exists in the repo. Correct locations: `internal/httpapi/protection.go` (API surface) + `internal/identity/protection.go` (server-side threshold derivation).
- **Reversibility.** Per-direction. Adding `block` to the HITL set (make batch refuse block-only agents too) converts current behavior to a proper subset — safe. Removing `scan` from the set is a contract narrowing (batch accepts more, then some messages land in review queue post-accept) — client-visible behavior change; hard to reverse cleanly.

#### Q14. Block-trigger handling — whole-batch 403 or per-item skip?

- **Decision: whole-batch 403 `blocked_by_policy`.** Any item that trips a block verdict (content-scan block on that item's content, or a per-recipient gate block under `action=block` for any address in that item's envelope) rejects the entire batch. `details.item_index` identifies the offending item.
- **Reasoning.** Block is the strong side of the automated-policy invariant: existing single-send returns 403 for a blocked message, and no "silently skipped" block outcome exists anywhere in the current send path. Downgrading block to per-item skip in batch would (a) create a shape asymmetry with single-send that any security review will flag, and (b) hide the fact that some part of the batch was refused by policy — which is exactly the information a caller under review needs.
- **Reversibility.** Reversible in the caller-friendly direction: adding a `blocked[]` per-item response array (analogous to `suppressed[]`) is a superset of the current behavior. But: doing so weakens a security invariant, so any reversal should require an explicit security sign-off.

#### Q15. Attachment size ceiling — how to bound a batch of per-item attachments?

With Q7 landing on per-item attachments (each `BatchMessage` carries its own `attachments[]` inherited from `SendEmailRequest`), the naive request-body ceiling explodes: 100 items × 25 MiB per item = 2.5 GiB. Some ceiling on the batch total is required.

- **Options considered.**
  - (a) *Sum of per-item caps*: no batch-level cap; request body bounded only by `100 × 25 MiB = 2.5 GiB`. Practical for the wire? No — most reverse proxies (nginx default `client_max_body_size` 1 MiB; ALB soft cap around 5 MiB unless raised; ingress controllers similar) cap far below this, and Postgres request-tx memory pressure is real.
  - (b) *Force shared attachments at batch level*: strip per-item `attachments[]`; add a batch-level `attachments[]` field that applies to every item. Simple ceiling (25 MiB). Contradicts Q7/Q8's decision to keep per-item independence.
  - (c) *Per-item + batch-level combined cap*: keep per-item `attachments[]`, and additionally enforce `sum(item.attachments.size_bytes) ≤ 25 MiB` across the batch. Caller can distribute freely (100 × 250 KiB, 5 × 5 MiB + 95 empty, 1 × 25 MiB + 99 empty).
- **Decision:** **(c) Per-item attachments + batch-level 25 MiB total cap.**
- **Reasoning.** Preserves Q7's per-item independence (single-send request shape at the item level, no shared-attachments concept to explain). Bounds the wire — 25 MiB is the existing single-send ceiling, so no reverse-proxy or backend limit needs raising for batch endpoints. Flexible: the caller who wants the "shared attachment" scenario expresses it by putting the blob on one item (25 MiB) with the other 99 empty, which is exactly what "shared attachment" means physically (one blob content-hash deduped in storage — Q10). Enforcement is one line in the validator (`sum(bytes) ≤ 25*1024*1024`).
- **Enforcement.** §9 step 2 (structural validation), before rate-limit reservation. 413 `payload_too_large` with `details.scope = "batch"`, `details.computed_batch_bytes`, `details.max_batch_bytes = 26214400`.
- **Reversibility.** Fully reversible upward (raising the 25 MiB cap is additive) and downward with warning (lowering breaks existing accepting callers). If real-world traffic shows the cap is too tight (e.g. compliance-heavy verticals shipping 50-page contracts), raise per traffic data — do not raise reflexively at MVP.

### Where these decisions are enforced in the code

| Decision | Enforcement point |
|---|---|
| Q1 fanout, not envelope | new endpoint `POST /v1/agents/{email}/batches` (§1.1) |
| Q2 accept-time all-or-nothing | §2.1 rejection list; §9 steps 2, 5 short-circuit pre-tx |
| Q3 suppression per-item | §9 step 6 partitions items into `keep[]` and `suppressed[]`; response `results[]` slot is `{ suppressed }` for dropped items |
| Q4 rate limit N | `AllowN(count=len(keep))` variant in §9 step 7 |
| Q5 cap 100 | schema `minItems: 1, maxItems: 100` on `messages` |
| Q6 HITL-refuse (MVP) | §9 step 3 `agentUsesHITL(agent)` gate → 403 `batch_hitl_unsupported` |
| Q7 per-item attachments + batch cap | request schema — `BatchMessage.attachments[]` per item; batch total ≤ 25 MiB enforced in §9 step 2 (see also Q15) |
| Q8 Resend-shape wire + Resend-philosophy defaults + stricter errors | overall API contract (§1) — `messages: [BatchMessage]`, 100 cap, `Idempotency-Key`, no server-side mail-merge, all-or-nothing validation + per-item `suppressed[]` in `results[]` |
| Q9 all-suppressed → 202 | `handleSendBatch` returns 202 unconditionally when `len(keep) == 0`; all `results[]` slots are `{ suppressed }` |
| Q10 attachment quota 1 for storage / N for egress | `checkAttachmentStorageQuota(uniqueBytes)` + `checkSendLimit(count=N)` in §9 step 7 |
| Q11 duplicate reject | pre-tx validation loop (§9 step 2): detect cross-item `to` duplicates → 400 `duplicate_recipient` |
| Q12 batch-of-1 allowed | schema `minItems: 1, maxItems: 100` |
| Q13 HITL check formula | `agentUsesHITL()` in §9 step 3 |
| Q14 block whole-batch | `screenOutbound()` called per item in §9 step 5 — any `Block()` → 403 with `details.item_index` |
| Q15 batch-level 25 MiB attachment cap | §9 step 2: `sum(item.attachments.size_bytes) ≤ 25 MiB` else 413 `payload_too_large` `details.scope="batch"` |

---

## Change log

- 2026-07-16 (Le Xiao): initial draft. Decisions reconciled with mentor's brief (`all or nothing`, `resend vs sendgrid`, `templates existing but polish`) after research into existing send pipeline, idempotency, River queue, error contract, and rate limits.
- 2026-07-16 (Le Xiao): §14 all 6 open questions resolved. Corrected `internal/protectionconfig/` → `internal/httpapi/protection.go` + `internal/identity/protection.go`. HITL detection formula frozen. Block-trigger handling frozen (whole-batch 403). §2.1, §5.1, §9 updated to reflect the new §14 decisions.
- 2026-07-16 (Le Xiao): §14 expanded into a full decision journal — Tier 1 (foundational framing: what "batch send" means, what "all or nothing" means), Tier 2 (strategic model choices with North-American ESP benchmarks: suppression, rate limit, batch cap, HITL model, attachments, Resend vs SendGrid), Tier 3 (Q9-Q14 MVP details). Each Q now carries `Options considered / Decision / Reasoning / Reversibility`. Enforcement cross-ref table extended to all 14 questions.
- 2026-07-16 (Le Xiao): Added §0.5 Terminology (template / fanout / mail-merge / per-recipient variables / accept-tx / screening) so §14 is readable without deep e2a familiarity. Rewrote §14 Q8 to be honest about the hybrid — SendGrid-shape wire + Resend-philosophy defaults + stricter-than-both error model. Original "Resend-shaped" characterization was true on the philosophy axis and misleading on the wire axis; the new answer separates the two.
- 2026-07-16 (Le Xiao): **Shape reversal — MVP now Resend-shape (per-item content), not SendGrid-shape (shared content).** Triggered by re-reading the README + FAQ + data-handling posture, which unambiguously position e2a as "1-on-1 agent↔human transactional conversations," not marketing-broadcast. AI-agent callers' unique capability is per-recipient content generation, so a SendGrid-shape MVP that forced shared content would leave the dominant use case unreachable through `batches`. Full rewrite of §0.5 Terminology (fanout no longer implies shared content; mail-merge is now MVP-native), §1.2 (`recipients: [{to}]` → `messages: [BatchMessage]`), §1.3 (`message_ids[]` → `results[]` with per-item `{ message_id }` \| `{ suppressed }` discriminated slots), §1.4 (error codes gain `item_index` disambiguation; `too_many_recipients` → `too_many_messages`), §2.1 (per-item structural validation), §2.2 (per-item suppression skip mechanics), §3.1 (`suppressed_json` shape gains `item_index`), §7.1 (GET rollup response shape), §8 (three new error codes and existing-code `item_index`/`scope` extensions), §9 (per-item render/screen loop, no shared content render step), §11 (per-recipient variables + per-item overrides move from polish to MVP; polish shrinks to 4 items + a note). §14 Q1 refined; Q7 rewritten (per-item attachments); Q8 rewritten (Resend-shape decision + rationale from README evidence); new Q15 added (batch-level 25 MiB attachment cap). §14 enforcement table refreshed. Template polish was NOT added — batch send inherits the beta template system as-is; any template-engine enhancement is a separate project.
- 2026-07-16 (Le Xiao): §10 rewritten from a 4-6 phased-PR plan into a **2-PR plan** — matching the project's observed convention in `tokencanopy/e2a` (features like PR #536 external-auth and PR #370 per-domain ramp-up ship as single-PR features with internal commit sequencing, not fanned out across many small PRs). PR 1 = design doc (commit 1) + OpenAPI spec + migration + handler + observability + tests + user docs (7 commits total, ~20-25 files, feature-flagged behind `E2A_BATCH_SEND_ENABLED`). PR 2 = TS SDK + Python SDK + CLI (~10-15 files). A fault line between commits 4 and 5 documented in case the mentor requests a split during review.
