# Changelog

## 5.4.0

Additive only. Every 5.3.0 call site keeps working identically; the new
parameters are keyword-only with defaults that preserve 5.3.0 behavior.

### Added
- **``unsubscribe=`` on ``messages.send`` / ``.reply`` / ``.forward``** — opt a
  single message into e2a-managed unsubscribe handling by passing
  ``unsubscribe={"mode": "managed"}`` (or an ``UnsubscribeOptions``), instead of
  having to set the field inside the request body. When both are given the
  kwarg wins. Beta, together with the raw ``GET|POST /u/{token}`` confirmation
  flow it enables. Mirrors the TypeScript SDK's ``ManagedUnsubscribeOptions``.
- **``wait="sent"`` on ``messages.send`` / ``.reply`` / ``.forward``** — an
  optional bounded wait. The request is held server-side until the
  asynchronously delivered message reaches a terminal-or-held state, or at most
  20 seconds elapse (the frozen contract ceiling; the server currently returns
  in ~15s), and then returns the observed state. On timeout the result stays
  ``status="accepted"``, so always branch on the result's ``status``, never on
  the HTTP code. Default is unchanged: no wait. Available on both
  ``AsyncE2AClient`` and the synchronous ``E2AClient``.
- **``DomainCapabilities`` on ``DomainView.capabilities``** — per-axis domain
  readiness, splitting the two independent axes that the legacy ``verified``
  boolean and ``sending_status`` rollup expressed separately. ``inbound``
  restates whether the domain can receive mail (``verified`` | ``pending`` —
  there is deliberately no inbound failure state; a missing or wrong MX stays
  ``pending``). ``outbound`` restates whether agents can send as their own
  address (``none`` | ``pending`` | ``verified`` | ``failed``, with detail in
  ``sending_error``). Both are open string sets — tolerate unknown values.

### Fixed
- **``unsubscribe=`` no longer mutates the caller's request model.** When the
  body was already a ``SendEmailRequest`` / ``ReplyRequest`` / ``ForwardRequest``
  instance, the kwarg was assigned onto that object, leaking into the caller's
  model and into any later reuse of it. The SDK now copies before assigning.

## 5.3.0

### Added
- **`client.messages.delete(email, message_id, *, permanent=False)`** — move a
  message to the trash, reversible via `client.messages.restore(...)` until the
  trash retention window expires (30 days by default), so the soft delete needs
  no confirmation. Pass `permanent=True` to delete forever a message that is
  already in the trash (irreversible, account scope only; the SDK supplies the
  `?confirm=DELETE` guard the raw API requires on that path). Available on both
  `AsyncE2AClient` and the synchronous `E2AClient`.
- **`client.messages.get_lifecycle(email, message_id, *, cursor=None, limit=None)`**
  — page through a message's canonical lifecycle transitions (send, delivery,
  bounce, review, deletion, …) as `PageMessageLifecycleTransition`. Beta.

## 5.2.0

The first publish to PyPI since 4.0.1: 5.0.0, 5.1.0, and 5.2.0 all reach users
in this release, so read those three sections together when upgrading from 4.x.

### Added
- `client.inbound.from_event(event)` returns `AsyncInboundEmail` on
  `AsyncE2AClient` and a blocking `InboundEmail` on `E2AClient`. The facade
  exposes explicit envelope/auth verdicts, reply targets, parsed-body
  truncation, policy flags, bound reply/forward, and lazy attachment `get()`.
  Shared cross-SDK vectors gate async/sync/TypeScript semantics.

### Breaking (pre-GA)
- **Inbound sender and authentication fields now use the final DMARC-aligned
  contract.** Generated message models and raw webhook/WS payloads expose the
  literal RFC 5322 ``header_from``, SMTP ``envelope_from``, nullable
  ``verified_domain``, and structured ``authentication`` (``spf``, every
  ``dkim`` result, and ``dmarc``). The old aggregate and signed nested-header
  fields are removed. A non-null ``verified_domain`` means DMARC passed for that
  From domain; it does not authenticate the mailbox local part, a person, or
  message content.

  | Previous 5.x generated property/model | Replacement |
  |---|---|
  | ``from_`` | ``header_from``. Reply routing remains in ``reply_to``. |
  | ``authenticated_from`` | ``verified_domain``, or inspect ``authentication.dmarc.status == "pass"``. |
  | ``auth: AuthVerdict`` | ``authentication: Optional[Authentication]``; ``AuthVerdict`` is removed. |
  | ``CheckResult`` | ``SPFResult``; DKIM and DMARC use the new ``DKIMResult`` and ``DMARCResult`` models. |
  | ``auth_headers`` / ``X-E2A-Auth-*`` | Removed. For webhooks, verify the envelope ``X-E2A-Signature``; REST and WebSocket already use authenticated transports. |

- **Implementation-leaked schema names renamed; duplicate schemas collapsed.**
  Generated models: ``EventJSON`` → ``EventView`` (module
  ``event_json`` → ``event_view``), ``PageEventJSON`` → ``PageEventView``,
  ``Suppression`` → ``SuppressionView``, ``PageSuppression`` →
  ``PageSuppressionView``; the duplicate ``Result`` collapsed into the
  existing ``Authentication``, and the duplicate ``AttachmentMeta`` collapsed
  into the canonical ``AttachmentMetaView`` (one attachment-metadata shape for
  REST responses, stable event payloads, and the account export — the
  hand-written webhook payload TypedDict in ``webhook_signature.py`` follows
  the same rename). The wire JSON is unchanged — field names, optionality, and
  values are identical; only the exported type names changed. Migrate:
  ``EventJSON`` → ``EventView``, ``Suppression`` → ``SuppressionView``,
  ``Result`` → ``Authentication``, ``AttachmentMeta`` → ``AttachmentMetaView``.

- **The reserved-word wire field `from` is exposed as `from_` (PEP 8 trailing
  underscore) where a sender is still projected** (was the generator-mangled
  `var_from`). This is the generated `list_messages` sender filter parameter
  and the outbound-only `EmailSentData` / `EmailFailedData` models; the
  hand-written layer's `messages.list(from_=...)` already used `from_`, so the
  SDK teaches exactly one spelling, and the TypeScript SDK exposes the same
  `from_`. The wire JSON is unchanged — requests and responses still carry
  `from` (pydantic alias). The webhook/WS `data` payload TypedDicts are
  wire-true dicts and keep the literal `"from"` key (access as `data["from"]`).

  An intermediate step on `main` also renamed the *inbound* sender projection
  on `Message`, `MessageView`, `MessageSummaryView`, `ReviewView`, and
  `EmailReceivedData` from `var_from` to `from_`. The DMARC-aligned contract
  above replaced that projection with `header_from` before this release, so
  `from_` never reached PyPI on those models and there is nothing to migrate
  off — 4.x callers reading `message.var_from` go straight to
  `message.header_from`.

## 5.1.0

### Breaking (pre-GA)
- **Uniform DELETE responses: every `.delete(...)` now returns a typed deletion
  object instead of `None`.** The API's seven delete endpoints all return
  `200 OK` with `{"deleted": true, <identity key>}` instead of the previous mix
  of `204 No Content` and `200`. New return types: `agents.delete` →
  `DeleteAgentResult` (`deleted`, `email`, `messages_deleted` — the message
  cascade count), `domains.delete` → `DeleteDomainResult` (`domain`),
  `webhooks.delete` → `DeleteWebhookResult` (`id`), `templates.delete` →
  `DeleteTemplateResult` (`id`), `account.api_keys.delete` →
  `DeleteApiKeyResult` (`id`), `account.suppressions.delete` →
  `DeleteSuppressionResult` (`address`); `account.delete()` still returns
  `DeleteUserDataResult`, which now also carries `deleted: true`. `deleted` is
  always `True` — a failed delete raises a typed error, never returns
  `deleted: False`. Applies identically to the sync `E2AClient` facade (it
  mirrors the async surface). Callers that ignored the old `None` return need
  no changes; the SDK still auto-sends the `?confirm=DELETE` guard. Older SDK
  versions whose generated bases expected `204` are incompatible with servers
  running this contract — upgrade together (pre-GA break).

## 5.0.0

Breaking: the async client class was renamed (the freed name now ships a
synchronous client), and the WebSocket frame is now the versioned event
envelope (server #456).

### Changed
- **`E2AClient` → `AsyncE2AClient`.** The 4.x client was async-only, but its
  name inverted the Python-ecosystem convention (httpx, openai, anthropic:
  plain name = sync client, `Async*` prefix = async client) and squatted the
  name the synchronous client needs. The class, exports (`e2a` and `e2a.v1`),
  docs, and examples all now use `AsyncE2AClient`; its behavior is unchanged.
  Migration from 4.x is mechanical: `from e2a.v1 import AsyncE2AClient`.
- **The WebSocket frame is the versioned event envelope** — the same
  ``{type, id, schema_version, created_at, data}`` shape a webhook delivery
  carries, so one parser (and one dedup key: the event ``id``) serves both
  channels. Frames were previously a flat ad-hoc notification object.
- **`WSNotification` is removed.** ``client.listen(...)`` now yields
  ``WSEvent`` envelopes: branch on ``event.type`` (tolerate unknown values —
  forward-compat) and read the payload from ``event.data`` (for
  ``email.received`` the flat ``notif.message_id`` / ``notif.delivered_to``
  attributes become ``event.data["message_id"]`` /
  ``event.data["delivered_to"]``). The ``is_email_*`` / ``is_domain_*``
  guards narrow the stable payloads.

### Added
- **Typed per-event payload models** for the nine stable event types
  (``EmailReceivedData``, ``EmailSentData``, ``EmailFailedData``,
  ``EmailDeliveredData``, ``EmailBouncedData``, ``EmailComplainedData``,
  ``DomainSendingVerifiedData``, ``DomainSendingFailedData``,
  ``DomainSuppressionAddedData``, plus ``AttachmentMeta``) with narrowing
  guards (``is_email_received``, ``is_email_sent``, …) shared by the webhook
  and WS channels. The shapes are locked to the server's committed golden
  fixtures.
- ``client.webhooks.fetch_message(event)`` accepts both a verified
  ``WebhookEvent`` and a ``WSEvent`` (any envelope-shaped object with
  ``type`` and ``data``).
- **`E2AClient` — the synchronous client** — under the name the rename freed
  (there is deliberately no compatibility alias to the async client). It is a
  facade over `AsyncE2AClient`: a background daemon thread runs an event loop
  for the client's lifetime and every call bridges the corresponding async
  coroutine onto it, so there is exactly one implementation of
  resources/retries/typed errors/pagination and the two surfaces cannot drift.
  - Identical constructor (`api_key`, `base_url`, `max_retries`,
    `max_elapsed_ms`, `timeout_ms`) and resource tree; typed `E2AError`
    subclasses propagate unwrapped, so `except E2ALimitExceededError:` works
    the same as in async code.
  - List endpoints return a **sync pager**: plain `for` iteration, plus
    `page(cursor)` / `to_list(limit=N)` / `for_each(fn)`.
  - `client.listen(address)` returns a plain iterable of `WSEvent` envelopes
    (the same envelope the async `listen()` yields); `close()` from another
    thread unblocks a pending iteration cleanly.
  - Lifecycle: use as a context manager or call `close()` (idempotent). An
    unclosed client is cleaned up at GC/interpreter exit and cannot hang
    shutdown.
  - **Async-context guard:** calling any sync method while an event loop is
    running in the current thread raises a guiding `RuntimeError`
    ("use AsyncE2AClient") instead of blocking the loop. 4.x code that still
    imports `E2AClient` now gets the sync client — update those imports to
    `AsyncE2AClient`.

## 4.3.0

### Breaking (pre-GA)
- **`AgentIdentity.webhook_healthy` (bool) replaced by `AgentIdentity.webhook_status`
  (optional string enum).** The bool could not distinguish "no webhook
  configured" from "healthy". The new field is an open set — tolerate unknown
  values. Known values: `none` (no webhook matches the agent), `healthy` (an
  enabled matching webhook, no terminally-failed delivery in the last 24h),
  `failing` (an enabled matching webhook had a terminally-failed delivery in
  the last 24h), `disabled` (matching webhooks exist but all are manually
  disabled), `auto_disabled` (all matching webhooks disabled, at least one by
  the chronic-failure sweep). `AgentIdentity` only appears in the account
  export (`account.export()`), so most integrations are unaffected.

## 4.2.0

Additive, no breaking changes.

### Fixed
- **`templates.list()` / `templates.list_starters()` silently truncated to the
  first page.** Both ignored the server's `next_cursor` and stopped after one
  request, dropping every template/starter past page one. They now thread the
  cursor and auto-page to completion like every other `.list()` (TS SDK
  parity), and accept a `limit=` per-page size.

### Added
- **`AutoPager.page(cursor=None)`** — the manual, caller-driven pagination
  primitive (parity with the TS SDK's `pager.page()`): fetch a SINGLE page and
  get back a `Page` of `items` + `next_cursor`. Pass the previous page's
  `next_cursor` to resume (e.g. checkpoint/restart from a queue); a `None`
  `next_cursor` in the result means there are no more pages. The page size is
  the `limit` baked into the list call that produced the pager.

## 4.1.0

Additive, no breaking changes.

### Added
- **`E2ALimitExceededError`** — the typed error for a `402 limit_exceeded`
  response (a per-account **quota** cap: monthly messages, storage, agent/domain
  counts). It is **not** retryable. This completes the permanent GA split with
  `E2ARateLimitError` (`429 rate_limited`, a request-**rate**/throughput limit,
  which **is** retryable): branch on the exception type (equivalently the HTTP
  status) — `402` → surface a quota/upgrade path, `429` → back off
  `retry_after_seconds` and retry. A `402` previously surfaced as the base
  `E2AError`; it now surfaces as this subclass (still an `E2AError`, so existing
  `except E2AError` handlers are unaffected).
- `email.received` is a metadata-only notification; `webhooks.fetch_message(event)`
  + the `EmailReceivedPayload` type fetch the full message (body + attachments)
  on demand (#321).
- Per-axis SES sending status surfaced on the domain/sending types (#309).
- DKIM verification support (#312).

## 4.0.0

Breaking: the domain DNS-records shape changed (server #304).

### Changed
- **`DomainView.dns_records` is now a single purpose-tagged array
  (`list[DNSRecord]`).** Each record carries `type`, `name`, `value`,
  `priority`, `purpose`, and `status`. Replaces the old
  `dns_records.{ mx, txt, dkim }` object and the separate `sending_dns_records`
  list. Address records by `purpose` (`ownership`, `inbound_mx`, `dkim`,
  `mail_from_mx`, `mail_from_spf`) rather than `dns_records.mx`/`.txt`/`.dkim`.
  The MAIL FROM records live in the same list (returned at registration when the
  sending feature is enabled), and each record now has a per-record `status`
  (`verified`/`pending`/`missing`/`failed`). `purpose` and `status` are open
  sets — tolerate unknown values.

## 3.0.0

Breaking redesign. The SDK is now a namespaced, **async-only** `E2AClient`
wrapping a generated client over the agent-scoped `/v1` API surface, with a
typed error hierarchy, automatic retries + idempotency, and async
auto-pagination.

### Changed
- **Namespaced, async-only surface.** Resources are grouped under the client:
  `client.agents`, `client.messages`, `client.conversations`, `client.domains`,
  `client.events`, `client.webhooks`, `client.account`. Per-agent methods take
  the agent `address` as the first argument
  (`await client.messages.send(address, {...})`,
  `await client.messages.list(address).to_list(limit=...)`,
  `await client.messages.get(address, id)`,
  `await client.messages.reply(address, id, {...})`). Use the client as an async
  context manager (`async with E2AClient() as client:`).
- **Webhook verification.** Verify and decode a delivery with the standalone
  `construct_event(raw_body, signature_header, secret)`, which checks the
  `X-E2A-Signature` header and returns a typed event (raising
  `E2AWebhookSignatureError` on a bad signature). Per-webhook `whsec_…` secrets,
  Stripe-style.
- **Typed errors.** Failures raise `E2AError` subclasses (`E2ANotFoundError`,
  `E2AConflictError`, `E2AValidationError`, `E2ARateLimitError`,
  `E2AWebhookSignatureError`, …) carrying `.code`, `.status`, `.request_id`, and
  `.retryable`.

### Removed
- The flat methods `send` / `reply` / `get_messages` / `get_message` and the
  per-call `agent_email` inference. Pass the agent `address` explicitly.
- The lower-level `E2AApi` class.
- The synchronous client — the SDK is async-only.
- `InboundEmail` / `AsyncInboundEmail` and the `parse_webhook` / `parse` +
  `verify_signature()` flow. Replaced by `construct_event`. There is no
  unverified-email type and no field-access gating.

## 2.5.0

### Added
- Generated types for the per-user resource-limits primitive that
  shipped with #158: `LimitsInfo`, `LimitsCaps`, `LimitsUsage`. These
  describe the response shape of `GET /api/v1/users/me/limits`, which
  the hosted dashboard uses to render the upgrade affordance and the
  "you've used X of Y" surface. The high-level `E2AClient` doesn't
  yet expose a typed helper for this endpoint — it's surfaced as a
  dashboard-only concern today, and SDK consumers querying their own
  usage should call `/agents` / `/messages` directly. The types are
  emitted so anyone consuming the raw OpenAPI generation has the
  shapes available.

### Notes
- No runtime client behavior changed in this release. If you're not
  using the limits primitive (self-host deployments without a paid
  tier), 2.5.0 is functionally identical to 2.4.0.

## 2.4.0

### Added
- `idempotency_key` parameter on `E2AClient.approve_message()` and its
  async counterpart (and on the lower-level `E2AApi.approve_message()`).
  Approve fires a real SES send, so without a stable key a retry after
  a transient failure could double-send. When supplied it's threaded
  through as the `Idempotency-Key` header; when omitted the SDK mints
  a fresh UUIDv4 per call — that gives network-layer retry safety only.
  Supply a stable key derived from the review event (typically the
  pending `message_id`) to dedupe across an explicit retry loop.
- `sort`, `from_`, `subject_contains`, `conversation_id`, `since`,
  `until` kwargs on `E2AApi.list_messages()` and the high-level
  `E2AClient.get_messages()` (sync + async). `sort` defaults
  server-side to newest-first; pass `"asc"` for FIFO polling. The
  substring filters are case-insensitive and capped at 200 chars
  server-side. `since` / `until` accept RFC3339 timestamps and
  bracket `created_at`. Filter values are encoded into `next_token`,
  so continuation requests must keep the same filter values.

### Changed
- **Default sort flipped to newest-first** on `GET /messages`. Prior
  releases silently returned oldest-first for `direction=inbound` (the
  SDK default) and newest-first for `direction=all`. A polling agent
  that relied on FIFO drain order should now pass `sort="asc"` to
  preserve the old behavior.
- `agent_mode` is now a required field on `RegisterAgentRequest`. The
  server previously silently defaulted to `"cloud"` and then 400'd
  with a cryptic "webhook_url is required" message; it now explicitly
  rejects requests missing `agent_mode` with a clear error. Pydantic
  v2 will raise a validation error if you instantiate the request
  without it. Set `agent_mode="local"` or `"cloud"` explicitly.

## 2.3.0

### Added
- `idempotency_key` parameter on `E2AClient.send()` / `.reply()` and their async
  counterparts (and on the lower-level `E2AApi.send_email()` /
  `reply_to_message()`). When supplied, it is sent as the `Idempotency-Key`
  header so the server can deduplicate retries of the same send/reply. When
  omitted, the SDK generates a fresh UUIDv4 per call — that gives
  network-layer retry safety only; supply a stable key derived from the
  triggering event (e.g. the inbound message id or a job id) to deduplicate
  across an explicit retry loop.
- `InboundEmail.reply_to` and `AsyncInboundEmail.reply_to` (`list[str]`) — the
  parsed `Reply-To:` header from the inbound message, surfaced as a first-class
  field so consumers no longer need to re-parse `raw_message` with stdlib
  `email.message_from_bytes()`. Empty list when the header is absent; the SDK
  never silently falls back to `sender`. Use this when the sender is a no-reply
  notifications mailbox (Granola, GitHub, CI bots) and you need the actual
  correspondent.
- `MessageSummary.reply_to` (`list[str]`) on the REST polling path — the list
  endpoint now mirrors the same field.
- `reply_to` added to `unverified_payload` for forensic inspection without
  unlocking gated access.

### Reply-To trust path (decision)
`reply_to` is trusted on the same terms as `to`, `cc`, `recipient`,
`subject`, and the body fields: the e2a server parses it from
`raw_message`, places it in the JSON envelope, and TLS protects the wire
to your webhook URL. Treat the field as trustworthy once
`verify_signature()` succeeds **and** you're confident in your
relay-to-webhook connection (or via `client.get_message(...)`, which uses
the authenticated REST channel).

**What `verify_signature()` does not prove:** the HMAC binds a fixed set
of auth headers and `body_hash = SHA-256(raw_message)`. It does not sign
the JSON envelope itself, and the SDK reads `reply_to`, `to`, `cc`, etc.
from that envelope rather than re-parsing `raw_message`. So an attacker
who can modify the JSON wrapping after signing — but cannot modify
`raw_message` or the signed headers — can rewrite `reply_to` and the
HMAC will still verify. TLS to your webhook URL is the actual integrity
layer for the envelope fields; the HMAC is defense-in-depth for proven
origin and covers the body bytes. If you need byte-exact assurance for a
specific field, re-parse it from `raw_message` (whose integrity
`body_hash` *does* cover).

**Also not guaranteed:** upstream-DKIM coverage of `Reply-To:`. If the
original sender's DKIM signature did not sign `Reply-To` (whether
because they didn't sign it, or there was no DKIM at all), a MITM
between sender and e2a could have rewritten the header before it reached
the relay. e2a does not re-verify or surface per-header DKIM coverage
today — the `Authentication-Results` / SPF/DKIM surface is unchanged.
For routing decisions where attacker-controlled `Reply-To` would matter,
also confirm `email.is_verified` and that the sender's domain is one you
expect.

We chose to keep `reply_to` populated whenever it's present (rather than
masking it on partially-trusted messages or exposing a `reply_to_signed`
flag) so the field shape stays uniform with `to`/`cc` and consumers can
make their own policy decision. The trust model is documented on the
property docstring.

### Wire change
The webhook payload schema now includes an optional `reply_to: string[]`
field. Existing consumers that ignore unknown fields are unaffected; older
SDK versions parsing the same payload continue to work and simply do not
see the new key.

### Other generated-type additions
The high-level surface above is what most consumers will touch. For users
of `client.api.*` or `e2a.v1.generated.*` directly, the following backend
endpoints / fields also landed since 2.2.0 and are reflected in the
regenerated types:

- Per-record DNS verification — separate MX / SPF / DKIM diagnostic
  responses on the domain-verification endpoints.
- Enriched `DashboardAgent` — `Inbound7d`, `Outbound7d`, `Pending`,
  `LastDelivery`, `WebhookHealthy` fields on the dashboard list.
- OAuth 2.1 authorization-server endpoints (fosite-backed) used by the
  MCP server flow.
- Per-domain DKIM key generation endpoint.
- One-time signing-secret reveal on creation.
- Pending-review polish — provenance, quoted-inbound, headers-preview,
  draft-footer fields on the review payload.

These are additive and don't break existing 2.2.0 callers.
