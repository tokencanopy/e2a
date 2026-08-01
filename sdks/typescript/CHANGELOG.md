# Changelog

## 5.5.0

Additive only. Every 5.4.0 call site keeps compiling and behaving identically.
The new contacts/outreach and scheduled-sending surfaces are **beta** and may
change before they are declared stable. The complete GA-vs-beta stability
matrix for the whole `/v1` surface is documented in
[docs/api.md → Stability: GA and beta surface](../../docs/api.md#stability-ga-and-beta-surface).

### Added
- **`client.contacts` — account contacts and per-agent outreach (beta).**
  Account-level contact identity: `list` (filter by `source`, `importBatchId`,
  creation window; auto-paging), `get` / `getWithETag`, `create` (honors
  `idempotencyKey`), `update` (optional `ifMatch` rejects a stale editor with
  a typed 412), and `delete` (the SDK supplies the `?confirm=DELETE` guard).
  Bulk import: `import` accepts up to 1,000 rows per request and returns a
  per-row outcome (`created | updated | skipped | failed`, plus a
  `suppressed` flag), so one malformed row never rejects the upload — pass a
  stable `idempotencyKey` so a timed-out import can be retried without
  double-importing. `deleteImport(batchId)` reverses an import; the server
  removes only verifiably untouched rows the batch created — anything edited,
  corresponded with, or enrolled in surviving outreach since the import is
  retained and reported in the returned counts.
- **Per-agent outreach on `client.contacts` (beta).** `outreach(email, {...})`
  lists the contacts an agent is working, with server-derived reply and
  delivery facts (`replied`, `lastOutboundAt`, `outboundCount`, …). For a
  duplicate-safe follow-up sweep pass `replied: false` together with **both**
  `nextActionBefore` and `lastOutboundBefore` — `lastOutboundAt` is
  server-maintained, so the second filter excludes anyone just contacted even
  if your own state write was lost. `getOutreach` / `getOutreachWithETag`,
  `setOutreach` (caller-owned `stage`, `next_action_at`, `metadata`; derived
  fields are server-owned and rejected; optional `ifMatch`), and
  `deleteOutreach` complete the loop. An agent-scoped credential may drive
  outreach for its own inbox; the account-level contact methods need account
  scope.
- **Scheduled sending via `sendAt` (beta)** on the `messages.send` / `.reply`
  / `.forward` request bodies. A future `sendAt` (RFC 3339 with an explicit
  UTC offset, at most 90 days ahead) returns `status: "scheduled"` — durable
  acceptance, **do not re-send** — with `scheduledAt` echoing the future
  submission time (a "not before" bound, accurate to the scheduler poll
  interval). `wait: "sent"` does **not** wait for the scheduled time: a
  future `sendAt` returns `status: "scheduled"` immediately. Scheduling does
  not survive a review hold (approval sends immediately). Cancel by trashing
  the message before provider submission starts (`messages.delete`); restoring
  before `scheduledAt` re-arms it, restoring at or after leaves the message
  live with `delivery_status: "failed"` and the send canceled.
- **Beta `threadId` on message reads** (`MessageView` / `MessageSummaryView`)
  — a server-owned, read-only, mailbox-local email-topology identity. It is
  omitted for legacy messages without an assignment; `conversationId` remains
  caller-owned application correlation and its filter is unchanged. There is
  no thread filter, request field, or thread endpoint.
- **`contact.due` (beta)** joins the webhook event vocabulary: emitted
  at-least-once when an engagement's `next_action_at` passes. It is a
  notification for a deployed webhook receiver; see `docs/events.md`.

## 5.4.0

Additive only. Every 5.3.0 call site keeps compiling and behaving identically.

### Added
- **`wait: "sent"` on `messages.send` / `.reply` / `.forward`** via the new
  exported `SendOptions` (a `RequestOptions` superset). The request is held
  server-side until the asynchronously delivered message reaches a
  terminal-or-held state, or at most 20 seconds elapse — the frozen contract
  ceiling; the server currently returns in ~15s — and then returns the observed
  state. On timeout the result stays `status: "accepted"`, so always branch on
  the result's `status`, never on the HTTP code. Default is unchanged: no wait.
- **`agents.delete(email, { permanent: true })`** — delete an agent
  irreversibly instead of moving it to the 30-day trash. Accepts both live and
  already-trashed agents. `agents.delete(email)` keeps its 5.3.0 meaning
  (trash, restorable via `agents.restore(email)`).
- **`apiKeys.create(body, { idempotencyKey })`** — the create call now accepts
  a request-scoped idempotency key. The server replays the same credential for
  a keyed retry, so an ambiguous transport failure can be retried without
  minting a second key.
- **`DomainCapabilities` on `DomainView.capabilities`** — per-axis domain
  readiness, splitting the two independent axes that the legacy `verified`
  boolean and `sendingStatus` rollup expressed separately. `inbound` restates
  whether the domain can receive mail (`verified` | `pending` — there is
  deliberately no inbound failure state; a missing or wrong MX stays `pending`).
  `outbound` restates whether agents can send as their own address (`none` |
  `pending` | `verified` | `failed`, with detail in `sendingError`). Both are
  open string sets — tolerate unknown values.

### Fixed
- **HTTP 410 now maps to `E2ANotFoundError`** in the status fallback, matching
  the family the `gone` error code already took in the code table. Previously a
  410 carrying an unrecognized code degraded to the base `E2AError`.

## 5.3.0

### Added
- **`client.messages.delete(email, messageId, { permanent })`** — move a message
  to the trash, reversible via `client.messages.restore(...)` until the trash
  retention window expires (30 days by default), so the soft delete needs no
  confirmation. Pass `permanent: true` to delete forever a message that is
  already in the trash (irreversible, account scope only; the SDK supplies the
  `?confirm=DELETE` guard the raw API requires on that path).
- **`client.messages.getLifecycle(email, messageId, { cursor, limit })`** —
  page through a message's canonical lifecycle transitions (send, delivery,
  bounce, review, deletion, …) as `PageMessageLifecycleTransition`. Beta.

## 5.2.0

The first publish to npm since 4.0.1: 5.0.0, 5.1.0, and 5.2.0 all reach users
in this release, so read those three sections together when upgrading from 4.x.

### Added
- `client.inbound.fromEvent(event)` returns a client-bound `InboundEmail`
  facade for verified webhook or authenticated WebSocket `email.received`
  envelopes. It exposes explicit envelope/auth verdicts, reply targets,
  parsed-body truncation, policy flags, bound reply/forward, and lazy
  attachment `get()`. Shared cross-SDK vectors gate the facade semantics.

### Breaking (pre-GA)
- **Inbound sender and authentication fields now use the final DMARC-aligned
  contract.** Generated message models and raw webhook/WS payloads expose the
  literal RFC 5322 `header_from`, SMTP `envelope_from`, nullable
  `verified_domain`, and structured `authentication` (`spf`, every `dkim`
  result, and `dmarc`). The old aggregate and signed nested-header fields are
  removed. A non-null `verified_domain` means DMARC passed for that From domain;
  it does not authenticate the mailbox local part, a person, or message content.

  | Previous 5.x generated property/type | Replacement |
  |---|---|
  | `from_` | `headerFrom` on generated models; raw event JSON uses `header_from`. Reply routing remains in `replyTo` / `reply_to`. |
  | `authenticatedFrom` | `verifiedDomain`, or inspect `authentication.dmarc.status === "pass"`. |
  | `auth: AuthVerdict` | `authentication: Authentication \| null`; `AuthVerdict` is removed. |
  | `CheckResult` | `SPFResult`; DKIM and DMARC use the new `DKIMResult` and `DMARCResult` types. |
  | `authHeaders` / `X-E2A-Auth-*` | Removed. For webhooks, verify the envelope `X-E2A-Signature`; REST and WebSocket already use authenticated transports. |

- **Implementation-leaked schema names renamed; duplicate schemas collapsed.**
  Generated types: `EventJSON` → `EventView`, `PageEventJSON` →
  `PageEventView`, `Suppression` → `SuppressionView`, `PageSuppression` →
  `PageSuppressionView`; the duplicate `Result` collapsed into the existing
  `Authentication`, and the duplicate `AttachmentMeta` collapsed into the
  canonical `AttachmentMetaView` (one attachment-metadata shape for REST
  responses, stable event payloads, and the account export — the hand-written
  webhook payload interface in `webhook-signature.ts` follows the same
  rename). The wire JSON is unchanged — field names, optionality, and values
  are identical; only the exported type names changed. Migrate:
  `EventJSON` → `EventView`, `Suppression` → `SuppressionView`,
  `Result` → `Authentication`, `AttachmentMeta` → `AttachmentMetaView`.

- **The reserved-word wire field `from` is exposed as `from_` where a sender is
  still projected** (was the private-looking `_from`, an OpenAPI Generator
  escape artifact). This is the `listMessages` sender filter and the
  outbound-only `EmailSentData` / `EmailFailedData` payloads; the Python SDK
  exposes the same `from_` spelling, so both SDKs teach exactly one name. The
  wire JSON is unchanged — requests and responses still carry `from`. The
  hand-written webhook/WS payload interfaces in `webhook-signature.ts` are
  wire-true and keep the literal `from` property (legal in TS) — those are
  raw-JSON shapes, not generated models. Migrate:
  `list(..., { _from })` → `{ from_ }`.

  An intermediate step on `main` also renamed the *inbound* sender projection
  on `Message`, `MessageView`, `MessageSummaryView`, `ReviewView`, and
  `EmailReceivedData` from `_from` to `from_`. The DMARC-aligned contract above
  replaced that projection with `headerFrom` before this release, so `from_`
  never reached npm on those models and there is nothing to migrate off — 4.x
  callers reading `message._from` go straight to `message.headerFrom`.

## 5.1.0

### Breaking (pre-GA)
- **Uniform DELETE responses: every `.delete(...)` now returns a typed deletion
  object instead of `void`.** The API's seven delete endpoints all return
  `200 OK` with `{deleted: true, <identity key>}` instead of the previous mix
  of `204 No Content` and `200`. New return types: `agents.delete` →
  `DeleteAgentResult` (`deleted`, `email`, `messagesDeleted` — the message
  cascade count), `domains.delete` → `DeleteDomainResult` (`domain`),
  `webhooks.delete` → `DeleteWebhookResult` (`id`), `templates.delete` →
  `DeleteTemplateResult` (`id`), `account.apiKeys.delete` →
  `DeleteApiKeyResult` (`id`), `account.suppressions.delete` →
  `DeleteSuppressionResult` (`address`); `account.delete()` still returns
  `DeleteUserDataResult`, which now also carries `deleted: true`. `deleted` is
  always `true` — a failed delete throws a typed error, never resolves with
  `deleted: false`. Callers that ignored the old `void` return need no changes;
  the SDK still auto-sends the `?confirm=DELETE` guard. Older SDK versions
  whose generated bases expected `204` are incompatible with servers running
  this contract — upgrade together (pre-GA break).

## 5.0.0

Breaking: the WebSocket frame is now the versioned event envelope (server #456).

### Changed
- **The WebSocket frame is the versioned event envelope** — the same
  `{type, id, schema_version, created_at, data}` shape a webhook delivery
  carries, so one parser (and one dedup key: the event `id`) serves both
  channels. Frames were previously a flat ad-hoc notification object.
- **`WSNotification` and the `"notification"` emitter event are removed.**
  Listen for `"event"` on `WSListener` (or iterate `client.listen(...)`)
  and use the new `WSEvent` type — an alias of `WebhookEvent`. Narrow with
  the type guards (e.g. `isEmailReceived(event)`) and read the payload from
  `event.data`.

### Added
- **Typed per-event payloads** for the nine stable event types
  (`EmailReceivedData`, `EmailSentData`, `EmailFailedData`,
  `EmailDeliveredData`, `EmailBouncedData`, `EmailComplainedData`,
  `DomainSendingVerifiedData`, `DomainSendingFailedData`,
  `DomainSuppressionAddedData`, plus `AttachmentMeta`) with narrowing guards
  (`isEmailReceived`, `isEmailSent`, …) shared by the webhook and WS
  channels. The shapes are locked to the server's committed golden fixtures.

## 4.2.0

### Breaking (pre-GA)
- **`AgentIdentity.webhookHealthy` (boolean) replaced by `AgentIdentity.webhookStatus`
  (optional string enum).** The bool could not distinguish "no webhook
  configured" from "healthy". The new field is an open set — tolerate unknown
  values. Known values: `none` (no webhook matches the agent), `healthy` (an
  enabled matching webhook, no terminally-failed delivery in the last 24h),
  `failing` (an enabled matching webhook had a terminally-failed delivery in
  the last 24h), `disabled` (matching webhooks exist but all are manually
  disabled), `auto_disabled` (all matching webhooks disabled, at least one by
  the chronic-failure sweep). `AgentIdentity` only appears in the account
  export (`account.export()`), so most integrations are unaffected.

## 4.1.0

Additive, no breaking changes.

### Added
- **`E2ALimitExceededError`** — the typed error for a `402 limit_exceeded`
  response (a per-account **quota** cap: monthly messages, storage, agent/domain
  counts). It is **not** retryable. This completes the permanent GA split with
  `E2ARateLimitError` (`429 rate_limited`, a request-**rate**/throughput limit,
  which **is** retryable): branch on the error subclass (equivalently the HTTP
  status) — `402` → surface a quota/upgrade path, `429` → back off
  `retryAfterSeconds` and retry. A `402` previously surfaced as the base
  `E2AError`; it now surfaces as this subclass (still an `E2AError` via
  `instanceof`, so existing catch-all handling is unaffected).
- `email.received` is a metadata-only notification; `webhooks.fetchMessage(event)`
  + the `EmailReceivedPayload` type fetch the full message (body + attachments)
  on demand (#321).
- Per-axis SES sending status surfaced on the domain/sending types (#309).
- DKIM verification support (#312).

## 4.0.0

Breaking: the domain DNS-records shape changed (server #304).

### Changed
- **`DomainView.dns_records` is now a single purpose-tagged array
  (`DNSRecord[]`).** Each record carries `type`, `name`, `value`, `priority`,
  `purpose`, and `status`. Replaces the old `dns_records.{ mx, txt, dkim }`
  object and the separate `sending_dns_records` array. Address records by
  `purpose` (`ownership`, `inbound_mx`, `dkim`, `mail_from_mx`, `mail_from_spf`)
  rather than `dns_records.mx`/`.txt`/`.dkim`. The MAIL FROM records live in the
  same array (returned at registration when the sending feature is enabled), and
  each record now has a per-record `status`
  (`verified`/`pending`/`missing`/`failed`). `purpose` and `status` are open
  sets — tolerate unknown values.

## 3.0.0

Breaking redesign. The SDK now wraps a generated `/v1` client behind a
namespaced, resource-oriented `E2AClient`, with a typed error hierarchy,
automatic retries + idempotency, and auto-pagination. Targets the e2a v1 API.

### Changed
- **Namespaced resources.** Flat methods are gone. Resources are grouped under
  the client: `client.agents`, `client.messages`, `client.conversations`,
  `client.domains`, `client.events`, `client.webhooks`, `client.account`.
  Per-agent methods take the agent `address` as the first argument
  (`client.messages.send(address, {...})`,
  `client.messages.list(address).toArray({ limit })`,
  `client.messages.get(address, id)`,
  `client.messages.reply(address, id, {...})`).
- **Webhook verification.** Verify and decode a delivery with the standalone
  `constructEvent(rawBody, signatureHeader, secret)`, which checks the
  `X-E2A-Signature` header and returns a typed `WebhookEvent` (throwing
  `E2AWebhookSignatureError` on a bad signature). Per-webhook `whsec_…` secrets,
  Stripe-style.
- **Typed errors.** Failures throw `E2AError` subclasses (`E2ANotFoundError`,
  `E2AConflictError`, `E2AValidationError`, `E2ARateLimitError`,
  `E2AWebhookSignatureError`, …) carrying `.code`, `.status`, `.requestId`, and
  `.retryable`.

### Removed
- The flat methods `getMessages` / `getMessage` / `send` / `reply` and the
  per-call address inference. Pass the agent `address` explicitly.
- `client.parse` / `client.parseWebhook` and `InboundEmail`. Replaced by
  `constructEvent`.
