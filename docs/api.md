# API Reference

This is a human-readable **overview** of the e2a `/v1` REST surface, organized by
resource. It is intentionally not an exhaustive endpoint-by-endpoint table — that
would rot. The canonical, always-current contract is the generated OpenAPI 3.1
spec:

> **Source of truth:** [`api/openapi.yaml`](../api/openapi.yaml). It is emitted
> directly from the typed Huma handlers in `internal/httpapi/` and CI fails if the
> committed copy drifts from the live server. Every path, query parameter,
> request body, and response shape is defined there. If anything here disagrees
> with the spec, **the spec wins.**

For **usage** (ergonomic clients, pagination helpers, webhook verification, the
MCP tool surface), see:

- TypeScript SDK — [`sdks/typescript/README.md`](../sdks/typescript/README.md) (`@e2a/sdk`)
- Python SDK — [`sdks/python/README.md`](../sdks/python/README.md) (`e2a`)
- MCP server — [`mcp/README.md`](../mcp/README.md) (`@e2a/mcp-server`)
- Webhook events & replay — [`events.md`](events.md)

## Conventions

- **Base path.** Every endpoint below is under `/v1` unless explicitly noted
  (`/api/health`, `/api/feedback`, and the WebSocket channel are the exceptions).
- **Auth.** `Authorization: Bearer <api_key>`. Keys are **scoped**:
  - `scope=account` — workspace admin: manage agents, domains, API keys,
    webhooks, and resolve reviews.
  - `scope=agent` — bound to a single inbox; can act only as that one agent and
    cannot manage account-level resources or approve its own held messages.

  The unauthenticated exceptions are `GET /api/health`, `GET /v1/info`,
  `POST /api/feedback`, `GET|POST /u/{token}`, and the HITL magic-link routes
  (which carry a signed token instead).
- **Path parameters with `@`/`+`.** Agent (and suppression/domain) paths are
  addressed by a full email/host (`/v1/agents/{email}/…`). **Percent-encode the
  segment**: `@` → `%40` and — importantly — `+` → `%2B`. A bare `+` in a path is
  often decoded to a space by clients/proxies, which silently corrupts
  plus-tagged addresses (`a+tag@x.com`). The official SDKs encode this for you;
  hand-rolled clients must do it themselves.
- **Pagination.** List endpoints return `{ items, next_cursor }`; pass
  `next_cursor` back as `?cursor=…` to page forward. The SDKs auto-page.
- **Idempotency.** Seven mutating operations honor an opt-in `Idempotency-Key`
  header: `sendMessage`, `replyToMessage`, `forwardMessage`, `approveReview`,
  `createWebhook`, `rotateWebhookSecret`, and `createApiKey`. Semantics:
  - **Replay.** A retry with the same key and a **byte-identical** body replays
    the first request's response instead of re-executing the side effect (the
    dedup hash covers the route + the raw body bytes, so the same key on a
    different route or with re-serialized JSON does not match).
  - **Dedup window: at least 24 hours.** Completed keys are remembered for a
    minimum of 24 hours after completion. Treat 24h as the published floor —
    a deployment may remember keys longer, never shorter.
  - **`409 idempotency_in_flight`** — a request with the same key is still
    executing. Retry-able: wait for the first request to finish, then retry
    unchanged (same key, byte-identical body) to have its response replayed.
  - **`422 idempotency_key_reuse`** — the key was already used with a
    *different* request body. Do **not** blind-retry this one: a legitimate
    retry must resend the byte-identical body, and a genuinely new request
    needs a fresh key.
  - **Best-effort.** Dedup is best-effort, not transactional: under
    idempotency-store degradation or a mid-request crash the protection
    degrades to at-least-once — a keyed retry may re-execute the operation
    rather than replay the cached response.
- **Errors.** Non-2xx responses use a single `ErrorEnvelope` shape; branch on
  `error.code` (see [Error codes](#error-codes) below for the full vocabulary).
- **Capacity limits — the permanent `402` / `429` split.** Two different limits
  can block a write, and they are **permanently distinct** — branch on the HTTP
  status:
  - **`402 limit_exceeded`** is a **quota** (a stock/flow cap): monthly-message
    allowance, storage bytes, agent/domain counts. A retry alone will not clear
    it — surface an upgrade/quota path. `error.details` is a `LimitExceededDetails`
    whose `resource` (`agents | domains | messages_month | storage_bytes`) keys the
    cap to `usage.<resource>` / `limits.max_<resource>` on `GET /v1/account`.
  - **`429 rate_limited`** is a **throughput / request-rate** limit (e.g. the
    per-agent send rate). It is transient and retry-able: wait
    `error.details.retry_after_seconds` (mirrored on the `Retry-After` header),
    then the same request succeeds.

  This is frozen GA semantics: `402` = QUOTA, `429` = RATE. Clients (and the
  official SDKs — `E2ALimitExceededError` vs `E2ARateLimitError`) must branch on
  the status, never conflate the two. The write operations that declare both are
  `sendMessage` / `replyToMessage` / `forwardMessage` / `testAgent` / `createAgent`
  (`registerDomain` declares only `402`; `approveReview` declares only `429`).

## Error codes

`error.code` is the stable, machine-branchable discriminator of the error
contract. It is an **open set**: treat it as a string and tolerate unknown
values — new codes may be added over time without a version bump. Branch on the
codes you handle and fall back to the HTTP status otherwise. The catalog below
is exhaustive for the current server (a drift test pins the source to this
vocabulary); codes are never renamed or removed within `/v1` — that would be
breaking.

Naming families: `invalid_*` = a validation refinement of `invalid_request`;
`*_not_found` = a specific missing (sub)resource; `*_taken` = the requested
identifier is already claimed (409). The official SDKs map these families to
their typed error classes code-first, so an unfamiliar member of a family still
lands in the right class.

Retry guidance: unless noted, 4xx codes are **not** retryable (fix the request
or the state first); `rate_limited`, `idempotency_in_flight`, and 5xx
`internal_error`/`limits_unavailable` are the retryable ones.

| Code | Status | Meaning |
| --- | --- | --- |
| **Auth / policy** | | |
| `unauthorized` | 401 | Missing or invalid credentials (REST and the WebSocket handshake). |
| `forbidden` | 403 | Authenticated but not allowed (key scope, cross-tenant access). |
| `blocked_by_policy` | 403 | **Experimental.** The outbound message was blocked by the agent's outbound policy gate. |
| **Validation** | | |
| `invalid_request` | 400 / 422 | The canonical input-validation code — malformed (400) or semantically invalid (422). `error.details` carries the per-field list. |
| `invalid_cursor` | 400 | Bad pagination cursor — drop it and re-fetch from the start. |
| `invalid_filter` | 400 | Bad list-filter parameter (messages/conversations/events). |
| `invalid_domain`, `invalid_slug`, `invalid_recipient`, `invalid_attachment`, `invalid_template`, `invalid_event_type`, `invalid_webhook_url`, `invalid_expires_at`, `invalid_scope` | 400 | Field/resource-specific refinements of `invalid_request`. |
| `reserved_domain` | 400 | The domain is reserved by the deployment (e.g. the shared domain). |
| `too_many_recipients` | 400 | Send/reply/forward recipient count over the cap. |
| `template_render_failed`, `template_rendered_empty` | 400 | Template send: rendering failed / produced an empty body. |
| `recipient_suppressed` | 422 | A recipient is on the account-wide or exact sending-agent suppression list — un-suppress or drop it. |
| **Not found / gone** | | |
| `not_found` | 404 | No such resource (agents, messages, webhooks, …). |
| `attachment_not_found`, `template_not_found`, `starter_template_not_found` | 404 | The `*_not_found` family — a specific sub-resource is missing. |
| `gone` | 410 | The event exists but is past the 30-day retention window. |
| **Conflict / state** | | |
| `conflict` | 409 | Generic state conflict (e.g. redelivery to a webhook that never matched the event). |
| `agent_taken`, `domain_taken`, `alias_taken` | 409 | The `*_taken` family — the requested identifier (agent address, domain, template alias) is already claimed. |
| `address_in_trash` | 409 | The requested agent address is reserved by a soft-deleted agent; restore or permanently delete it first. |
| `message_held` | 409 | The message is held for review and cannot perform the requested operation until the hold is resolved. |
| `message_not_pending` | 409 | The review hold was already resolved (approved/rejected/expired). |
| `message_not_yet_delivered` | 409 | Reply/forward target is an outbound message still queued for provider submission. A reply cannot thread until the provider assigns its Message-ID; a forward requires the source message to have actually been sent. Retry-able: retry once it is sent, or use `wait=sent` on the original send. |
| `not_in_trash` | 409 | Restore or permanent-delete was requested for a resource that is not currently in trash. |
| `send_in_progress` | 409 | The message send is already executing; wait for its terminal outcome. |
| `webhook_disabled` | 409 | Operation requires an enabled webhook. |
| `webhook_cooldown` | 409 | The webhook was auto-disabled and cannot be re-enabled until the cooldown elapses. SDKs do not automatically retry it; retry manually only after the cooldown. |
| `domain_not_registered` | 400 | Create-agent on a domain the account has not registered. |
| `domain_has_agents` | 400 | Domain delete blocked while agents exist on it — **including agents in the trash**, which keep their addresses for the 30-day restore window and do not appear in `list_agents`. The message names which kind is blocking and how many; purge trashed ones with `?confirm=DELETE&permanent=true`. |
| `domain_not_verified` | 400 / 403 | Domain verification pending — 400 on create-agent, 403 on send paths. |
| `inbound_mx_missing` | 400 | Inherited subdomain agent creation requires an exact or wildcard MX routing to e2a. |
| **Capacity — see the 402/429 split above** | | |
| `limit_exceeded` | 402 | Plan **quota** (stock/flow cap); `details` is `LimitExceededDetails`. Not retryable. |
| `rate_limited` | 429 | Request-**rate** limit; wait `details.retry_after_seconds` / `Retry-After`, then retry. |
| `template_limit_reached`, `webhook_limit_reached` | 400 | Fixed per-account count caps (not plan quotas) — delete one first. |
| **Idempotency** | | |
| `idempotency_in_flight` | 409 | Same key still executing — wait, then retry the byte-identical request to replay it. |
| `idempotency_key_reuse` | 422 | Same key, different body — caller bug; never blind-retry. |
| **Size** | | |
| `payload_too_large` | 413 | Request body / total attachments over the cap. |
| `attachment_too_large` | 413 | `?inline=true` requested for an attachment over the inline cap — use `download_url`. |
| **Availability** | | |
| `not_implemented` | 501 | The feature (API keys, reviews, suppressions) is not available on this deployment. Not retryable. |
| `events_log_disabled` | 501 | The events log is disabled on this deployment (expected on some hosted configurations). Not retryable. |
| `limits_unavailable` | 503 | The limits subsystem is not available — transient, retryable. |
| `inbound_mx_check_failed` | 503 | DNS could not be queried while validating an inherited subdomain inbox — transient, retryable. |
| **Server / fallback** | | |
| `internal_error` | 5xx | Server-side failure; safe to retry with backoff unless the message says otherwise. |
| `method_not_allowed` | 405 | Fallback code (wrong HTTP method on a real route). |
| `unsupported_media_type` | 415 | Fallback code (non-JSON request body). |
| `error` | other 4xx | Generic fallback for any otherwise-unmapped status (e.g. 406). |

The SDKs additionally synthesize the client-side code `connection_error`
(status `0`) when no HTTP response was received at all; it never comes from the
server and is always retryable.

### Typed error details

The envelope stays forward-compatible: `error.details` is an optional open
object and unknown fields must be preserved. The following stable codes have a
published typed schema, also exposed machine-readably by
`ErrorBody.details.x-e2a-error-details-schemas` in OpenAPI:

| Code | Schema |
| --- | --- |
| invalid_request | `ValidationErrorDetails` — `fields[]` with required, non-null `location` and `message`. |
| too_many_recipients | `TooManyRecipientsDetails` — `max_recipients`, `provided`. |
| payload_too_large | `PayloadTooLargeDetails` — `scope`, `actual_bytes`, `max_bytes`, optional `filename`. |
| limit_exceeded | `LimitExceededDetails`. |
| rate_limited | `RateLimitedDetails` — `retry_after_seconds`. |
| limits_unavailable | `RetryAfterDetails` — `retry_after_seconds`. |

`error.code`, `error.message`, and `error.request_id` are required on every
`/v1` error, including router-level 404/405 responses. The body request ID
equals the `X-Request-Id` response header.

## Versioning & stability

The `/v1` surface is the **stable, generally-available contract** as of e2a 1.0.
Our commitment, and what you can rely on:

### Beta operations

This is the complete operation-level exception list. These operations carry
`x-stability-level: beta` and may change before they are promoted to stable;
every `/v1` operation not listed here is covered by the GA freeze.

| operationId | Method and path | Surface |
| --- | --- | --- |
| `approveReview` | `POST /v1/reviews/{id}/approve` | Reviews |
| `createAgentSuppression` | `POST /v1/agents/{email}/suppressions` | Agent suppressions |
| `createTemplate` | `POST /v1/templates` | Templates |
| `deleteAgentSuppression` | `DELETE /v1/agents/{email}/suppressions/{address}` | Agent suppressions |
| `deleteTemplate` | `DELETE /v1/templates/{id}` | Templates |
| `getAgentProtection` | `GET /v1/agents/{email}/protection` | Protection config |
| `getMessageLifecycle` | `GET /v1/agents/{email}/messages/{id}/lifecycle` | Message lifecycle |
| `getReview` | `GET /v1/reviews/{id}` | Reviews |
| `getStarterTemplate` | `GET /v1/starter-templates/{alias}` | Starter templates |
| `getTemplate` | `GET /v1/templates/{id}` | Templates |
| `listAgentSuppressions` | `GET /v1/agents/{email}/suppressions` | Agent suppressions |
| `listReviews` | `GET /v1/reviews` | Reviews |
| `listStarterTemplates` | `GET /v1/starter-templates` | Starter templates |
| `listTemplates` | `GET /v1/templates` | Templates |
| `putAgentProtection` | `PUT /v1/agents/{email}/protection` | Protection config |
| `rejectReview` | `POST /v1/reviews/{id}/reject` | Reviews |
| `updateTemplate` | `PATCH /v1/templates/{id}` | Templates |
| `validateTemplate` | `POST /v1/templates/validate` | Templates |

### Compatibility rules

- **No breaking changes within `/v1`.** We will not remove an endpoint, remove a
  response field, rename anything, tighten a type, or change documented semantics
  under `/v1`. A breaking change means a new major version path (`/v2`), and the
  two would run side by side during a published migration window.
- **Additive changes can happen anytime** and are *not* breaking: new endpoints,
  new optional request fields, and **new response fields**. Clients must ignore
  fields they don't recognize. This is machine-readable in the spec: every
  **response** schema declares `additionalProperties: true` (a client generated
  from the spec tolerates additive fields), while every **request** schema stays
  strict (`additionalProperties: false`) — an unknown request field is rejected
  with a 422, which is intentional input validation (it catches typos like
  `body` vs `text`), not a stability concern.
- **Beta surfaces are marked `x-stability-level: beta`** in the spec
  for automated compatibility tools
  (operations, schemas, and individual fields — e.g. the `template_*` fields on
  send, `hold_reason`, the review-detail `protection` evidence, and the
  `flagged` / `flag_reason` verdict) and `(beta)` in prose — today: templates,
  starter templates, reviews, the agent protection config, agent-scoped
  suppression management, and managed unsubscribe (including its raw
  confirmation flow). They are **exempt from the
  freeze**: they may change or be removed without a major version. Where only
  specific *values* of a stable field are experimental (the screening +
  review-hold event types `email.flagged`, `email.blocked`,
  `email.review_requested`, `email.review_approved`, `email.review_rejected`),
  the field carries `x-experimental-values` listing exactly those values —
  their payloads may still change; all other event types are stable. Anything
  not marked experimental is stable surface. The stable `ErrorBody.code`
  discriminator similarly marks only `blocked_by_policy` experimental. One deliberate schema-level use
  of the beta marker under a **stable** operation: the account export's
  interior record schemas (`GET /v1/account/export`) are beta-marked because
  they are versioned by the export's stable `schema_version` envelope field
  rather than by the v1 freeze — see the account section below.
- **Enums in responses are open.** Treat any `type` / `*_status` / `event_type`
  value as an open string set: we may introduce new values (e.g. a new event
  type or delivery state) without a major bump, so a client **must not crash on
  an unknown value** — handle it as a default/passthrough case. (The official
  SDKs already do this.) Enum values you *send* in requests are validated and
  rejected if unknown — that's intentional and not a stability concern.
  The governing rule is two-sided: **response-side vocabularies that can
  evolve are open sets** (plain strings whose known values are documented in
  the field description), while **normalized exhaustive classifications and
  binary invariants remain closed enums** — e.g. `bounce_type`
  (`permanent | transient | undetermined`, exhaustive after server-side
  normalization with `undetermined` as the catch-all) and `direction`
  (`inbound | outbound`, a binary invariant of the model). A closed response
  enum is a promise the vocabulary can never grow; we make that promise only
  where the server actively guarantees it.
- **Version discovery.** `GET /v1/info` reports the running API version (and
  deployment flags such as whether shared-domain slug registration is enabled),
  so clients can adapt instead of hard-coding assumptions.
- **Deprecation & sunset.** Once `/v1` is GA, if we ever need to wind something
  down it stays functional and is marked `deprecated` in the OpenAPI spec; we
  will not remove it within GA `/v1`. (Pre-GA, the API is still being frozen:
  the legacy agent-path `…/messages/{id}/approve|reject` endpoints were removed
  in favor of the account-scoped `/v1/reviews/{id}/approve|reject` queue.)

The canonical machine-readable contract is always
[`api/openapi.yaml`](../api/openapi.yaml); CI fails if it drifts from the server.

## Resources

The surface is **agent-first**: messages, conversations, and the real-time
channel all hang off an agent (inbox). Reviews, events, webhooks, domains, and
account/key management are account-level.

### Account (`/v1/account`)

Workspace identity, plan limits, keys, suppressions, and data rights.

- `GET /v1/account` — whoami: the authenticated principal (user + scope, plus
  `agent_email` for agent-scoped keys), plan caps, and current usage. Works for
  both scopes. (Public *deployment* discovery is the separate `GET /v1/info`.)
- `DELETE /v1/account?confirm=DELETE` — permanently delete the account and cascade
  all owned data; returns per-table row counts (GDPR Art. 17). Irreversible.
- `GET /v1/account/export` — JSON dump of every record the account owns (GDPR
  Art. 15). Omits internal identifiers; see [data-handling.md](data-handling.md).
  The export **envelope** (the top-level keys and `schema_version`) is stable;
  the **interior** record shapes are versioned by `schema_version` and may
  evolve — branch on `schema_version` before interpreting interior records.
  The current export version is `4`; v4 messages expose canonical
  `header_from`, `envelope_from`, `verified_domain`, and `authentication`
  fields, and retain v3 suppression provenance — suppression entries may
  include `agent_email`, which identifies an exact-agent block. Entries
  without `agent_email` remain account-wide.
  Interior schemas carry `x-stability-level: beta` in the OpenAPI document to
  mark that exemption machine-readably; the operation itself is stable.
  Each exported message carries `attachments` as the same typed
  `AttachmentMetaView` metadata (`{filename, content_type, size_bytes, index}`,
  `size_bytes` = decoded payload) the live API uses; the attachment **bytes**
  of sent/inbound messages are inside the exported `raw_message`. A held
  draft's (`pending_review`) staged attachment bytes are internal transient
  storage and are not inlined.
- `GET/POST /v1/account/api-keys`, `DELETE /v1/account/api-keys/{id}?confirm=DELETE`
  — mint (plaintext shown once), list (metadata only), and revoke API keys.
  Account scope only.
- `GET /v1/account/suppressions`, `DELETE /v1/account/suppressions/{address}?confirm=DELETE`
  — the recipient suppression list (auto-added on hard bounce/complaint; sends to
  a suppressed address fail with `recipient_suppressed`). These blocks apply to
  every sending agent. Delete to un-suppress; this does not remove agent-scoped
  blocks.

Irreversible deletes require the `?confirm=DELETE` query param — schema-required
(`enum: [DELETE]`) on every `DELETE` endpoint except message delete, where it is
required only together with `permanent=true` (the default message delete is a
reversible trash move and takes no confirmation). A missing or wrong value is
rejected with `422 invalid_request` before the delete runs. The SDKs and CLI
supply it automatically for their typed `delete(...)` calls.

**Uniform delete responses.** Every `DELETE` returns `200 OK` with a small
typed deletion object — never `204 No Content`. The base shape is
`{"deleted": true, "<identity key>": ...}` where the identity key matches the
resource's identity field: `id` for webhooks/templates/API keys, `email` for
agents, `domain` for domains, `address` for suppressions. `deleted` is always
`true` — a failed delete is an error envelope, never `deleted: false`.
Cascading deletes may additionally carry receipt counts (all additive):
`DELETE /v1/agents/{email}` includes `messages_deleted`, and `DELETE
/v1/account` returns the full per-table `DeleteUserDataResult` receipt on top
of `deleted: true`.

### Domains (`/v1/domains`)

Custom sending/receiving domains and their DNS verification.

- `GET /v1/domains`, `POST /v1/domains` — list / register (returns required MX +
  TXT records and the DKIM selector/key).
- `GET /v1/domains/{domain}`, `DELETE /v1/domains/{domain}?confirm=DELETE` —
  fetch / delete (delete deprovisions the sending identity; irreversible).
- `POST /v1/domains/{domain}/verify` — verify ownership via the TXT record.

Every domain response (list, fetch, and register) carries **`capabilities`** —
`{ inbound, outbound }` — the per-axis rollup of what the domain can actually
do. `inbound` is whether it can receive mail (the ownership TXT plus the inbound
MX); `outbound` is whether agents on it can send as their own address (the async
SES sending identity: DKIM + custom MAIL FROM). The two axes are provisioned on
different schedules and are independent in both directions, so read them
separately rather than treating one as implying the other. `capabilities`
restates the legacy `verified` boolean (inbound) and `sending_status`
(outbound) — prefer it, since `verified` names only the inbound axis while
reading as though it covered the domain as a whole. Both legacy fields keep
working unchanged. Per-record detail stays in `dns_records[].status`:
`capabilities.outbound` is the all-or-nothing rollup, so when DKIM is healthy
but the MAIL FROM records are not, `outbound` reads `failed` while the `dkim`
record still reads `verified`.

The domain surface deliberately speaks **two record-state vocabularies**, one
per axis. `dns_records[].status` (on `GET /v1/domains/{domain}`) is the
**persisted** verification state of each record — `verified | pending |
missing | failed` — what the platform has recorded, updated by verification
checks and the SES reconciler. The `mx` / `spf` / `dkim` fields returned by
`POST /v1/domains/{domain}/verify` are the **live probe** outcome of that
verification attempt — `found | missing | deferred | mismatch` — what DNS
returned just now. Seeing `found` from verify while the same record still
reads `pending` on GET is intentional, not a bug: the probe result feeds the
persisted state, it does not replace its vocabulary. (`deferred` means the
DKIM probe was skipped because no per-domain keypair is stored yet; `mismatch`
means a DKIM record is published but its key doesn't match the issued one —
usually a truncated TXT.)

### Agents (`/v1/agents`)

An agent is an addressable inbox. Its email must be on a verified domain you own,
or on the deployment's shared domain (see `GET /v1/info`).

- `GET /v1/agents`, `POST /v1/agents` — list / register (body `{ email, name? }`).
- `GET/PATCH/DELETE /v1/agents/{email}` — fetch / rename / delete. `PATCH` updates
  the display name only; screening/protection config lives on the sub-resource
  below. `DELETE` requires `?confirm=DELETE` and moves the agent to the trash: it
  stops receiving mail, disappears from lists, and its held messages leave the
  review queue. Restore it via `POST /v1/agents/{email}/restore` within the trash
  retention window (30 days by default, deployment-configurable), after which
  it's purged permanently. Pass `?permanent=true` to skip the trash and delete
  irreversibly right away (accepts live and trashed agents).
- `POST /v1/agents/{email}/restore` — bring a trashed agent back into service,
  messages and configuration intact. For drafts still held for review,
  `approval_expires_at` is shifted forward by the time the agent spent in trash
  so a review hold can't lapse while the inbox was unavailable. `409
  not_in_trash` if the agent isn't in the trash.
- `GET/PUT /v1/agents/{email}/protection` — **(beta)** read / wholesale-replace the
  agent's protection posture: inbound/outbound trust gate, content-scan
  sensitivity, and the hold-queue mechanism (TTL + expiration action). Setting the
  outbound gate to `review` (or enabling the scan) is what turns on HITL holds.
  Account scope only. Beta — shape may change before it is declared stable.
- `POST /v1/agents/{email}/test` — send a platform test email to the agent's own
  address to confirm inbound delivery.
- `GET/POST /v1/agents/{email}/suppressions`,
  `DELETE /v1/agents/{email}/suppressions/{address}?confirm=DELETE` — list,
  idempotently add, or remove recipient blocks for this exact sending agent.
  These account-admin operations use `{items,next_cursor}` pagination; an
  agent-scoped credential cannot manage its own blocks. Manual create accepts
  `{address, reason?}`. The official SDK delete methods supply the confirmation
  guard automatically.

### Messages (`/v1/agents/{email}/messages`)

The message surface is agent-scoped: the agent in the path is the sender (there is
no `from` field). `reply`, `forward`, and `attachments` are sub-resources of a
single message.

- `GET …/messages` — list inbound + outbound with filters (`direction`,
  `read_status`, `sort`, `from`, `subject_contains`, `conversation_id`, `labels`,
  `since`, `until`) and cursor pagination. Held outbound drafts appear with
  `status=pending_review`.
- `POST …/messages` — send a new email (a new thread). Returns `202 Accepted` for
  every non-terminal outcome — `pending_review` when the agent's protection policy
  holds it for review, or `accepted` when the async pipeline durably queues it —
  and `200 OK` for the terminal-synchronous `sent`. The send result
  `status` is an open set — known values `accepted | sent | pending_review |
  review_approved | failed`. **Always branch on `status`, not the HTTP code.**
  `accepted` (async pipeline) means the message is durably persisted and queued;
  the terminal outcome then arrives via the `email.sent` / `email.failed` webhook
  events or `GET …/messages/{id}`. `provider_message_id` is absent until the
  message is actually sent. Optional `?wait=sent` holds the request until the
  message reaches a terminal-or-held state or a bounded timeout (a synchronous
  server treats it as a no-op).
- **`delivery_status`** on a message follows `accepted → sending → sent →
  delivered | deferred | bounced | complained | failed`. Note **`sent` ≠
  `delivered`**: `sent` means the upstream provider (SES) accepted the message,
  not that the recipient's server did. Delivery/bounce/complaint are per-recipient
  async outcomes reported later via SNS and the corresponding webhook events.
- `GET …/messages/{id}` — fetch one message (inbound or outbound), including the
  raw message and structured inbound authentication evidence. Reading an unread
  inbound message flips it to `read`. A soft-deleted message remains readable by
  this direct GET and carries `deleted_at` until it is permanently purged (30 days
  after deletion by default; the trash retention window is deployment-configurable).
  Ordinary message lists, conversations, reply targets, and forward targets hide
  trashed messages; use `GET …/messages?deleted=true` to enumerate the trash.
- `POST …/messages/{id}/restore` — bring a trashed message back into the inbox.
  Restored message data is retained indefinitely unless deleted again. `409
  not_in_trash` if the message isn't in the trash.
- `PATCH …/messages/{id}` — apply a labels delta (`add_labels` / `remove_labels`).
- `POST …/messages/{id}/reply`, `POST …/messages/{id}/forward` — reply to /
  forward a message; `202` when held for review.
- `GET …/messages/{id}/attachments/{index}` — attachment metadata + a short-lived
  `download_url` (so binary bytes never stream through an agent's context);
  `?inline=true` returns base64 `data` for small attachments.

### Message lifecycle diagnostic contract (beta)

**Beta:** the complete message-lifecycle feature may change before it is
declared stable. The operation and lifecycle schemas carry
`x-stability-level: beta`; the optional lifecycle fields embedded in stable
event payloads carry the same property-level marker.

`GET /v1/agents/{email}/messages/{id}/lifecycle` returns the ordered facts e2a
observed for one persisted inbound or outbound message. It is a diagnostic
ledger, not a synthetic status history. For example:

```bash
curl -H "Authorization: Bearer $E2A_API_KEY" \
  "https://api.e2a.dev/v1/agents/bot%40agents.e2a.dev/messages/msg_abc/lifecycle?limit=50"
```

The response is `{ "items": [...], "next_cursor": string | null }`. Items are
always in ascending `(occurred_at, id)` order. A cursor continues strictly
after that tuple and is bound to both the owning agent and message ID; it
cannot be replayed for another inbox or message. Page size is 1–100 (default
50). Missing and foreign messages both return `404 not_found`.

Each transition has `id`, `message_id`, `direction`, `stage`, `outcome`,
`reason_code`, `retryable`, `evidence`, `correlation_ids`, `occurred_at`, and
`reconstructed`. `recipient` is nullable: it is present for per-recipient
delivery, complaint, and suppression observations and null/omitted for
message-level observations. `evidence` contains safe, bounded diagnostic metadata
only: keys are allowlisted, diagnostic strings are at most 2 KiB,
the complete object is at most 16 KiB, and message bodies and secrets are not
included. `correlation_ids` is an allowlisted open object for identifiers such
as `event_id`, `job_id`, `provider_message_id`, `provider_event_id`, and
`email_message_id`; consumers must tolerate additional map entries.

The stage vocabulary is closed and exhaustive for this version:

| Stage | Boundary observed by e2a |
|---|---|
| `accepted` | e2a accepted the message from SMTP, the outbound API, or local loopback. |
| `authentication` | e2a evaluated inbound DMARC evidence. |
| `review` | A human/TTL review hold was created or resolved. |
| `suppression` | A recipient block or feedback suppression was applied. |
| `queued` | e2a durably queued inbound processing or outbound submission. |
| `submission` | e2a attempted or completed submission to an upstream provider or local loopback. |
| `delivery` | Recipient-server feedback was observed for one recipient. |
| `complaint` | Recipient complaint feedback was observed. |

Reason codes are also closed. Each code fixes its stage, outcome, and
retryability; clients must not reinterpret those fields independently:

| Reason code | Stage | Outcome | Retryable |
|---|---|---|---|
| `acceptance.inbound_smtp` | `accepted` | `accepted` | false |
| `acceptance.outbound_api` | `accepted` | `accepted` | false |
| `acceptance.local_loopback` | `accepted` | `accepted` | false |
| `authentication.dmarc_pass` | `authentication` | `passed` | false |
| `authentication.dmarc_fail` | `authentication` | `failed` | false |
| `authentication.dmarc_none` | `authentication` | `indeterminate` | false |
| `authentication.dmarc_temporary_error` | `authentication` | `indeterminate` | true |
| `authentication.dmarc_permanent_error` | `authentication` | `indeterminate` | false |
| `review.hold_created` | `review` | `pending` | false |
| `review.approved` | `review` | `approved` | false |
| `review.rejected` | `review` | `rejected` | false |
| `review.expired_approved` | `review` | `approved` | false |
| `review.expired_rejected` | `review` | `rejected` | false |
| `suppression.recipient_blocked` | `suppression` | `blocked` | false |
| `suppression.hard_bounce_applied` | `suppression` | `applied` | false |
| `suppression.complaint_applied` | `suppression` | `applied` | false |
| `queue.inbound_processing` | `queued` | `enqueued` | false |
| `queue.outbound_submission` | `queued` | `enqueued` | false |
| `submission.upstream_accepted` | `submission` | `accepted` | false |
| `submission.local_loopback_accepted` | `submission` | `accepted` | false |
| `submission.temporary_failure` | `submission` | `deferred` | true |
| `submission.provider_rejected` | `submission` | `failed` | false |
| `submission.local_retries_exhausted` | `submission` | `failed` | true |
| `submission.cancelled` | `submission` | `failed` | false |
| `delivery.recipient_server_accepted` | `delivery` | `delivered` | false |
| `delivery.temporary_delay` | `delivery` | `deferred` | true |
| `delivery.permanent_bounce` | `delivery` | `bounced` | false |
| `delivery.transient_bounce` | `delivery` | `bounced` | true |
| `delivery.undetermined_bounce` | `delivery` | `bounced` | false |
| `complaint.recipient_reported` | `complaint` | `reported` | false |

Producers append a transition in the same database transaction as the message,
queue, recipient, suppression, or event change that it explains. A
message-local `dedupe_key` makes an identical worker retry return the original
row; reusing that key for different semantics is a hard conflict. This keeps
duplicate job delivery from creating duplicate logical transitions.

For messages created before this ledger existed, the read may conservatively
derive facts from durable message, job, recipient, suppression, and stored-event
records. Such rows carry `reconstructed: true`, a deterministic ID, and safe
source evidence. Reconstruction does not fabricate an event or transition when
the durable records do not prove it, is read-only, and never overwrites a
persisted observation. Historical event envelopes remain unchanged.

This is an additive beta `/v1` contract: the endpoint and optional
`lifecycle_transitions` event field may be consumed by new clients without
changing historical responses or stored webhook redeliveries. Existing event
envelopes, event types, and payload schemas remain stable; only the optional
lifecycle property is beta. The closed stage, outcome, and reason-code vocabularies
change only through deliberate versioned contract handling. Any addition requires coordinated OpenAPI and generated-SDK regeneration plus handwritten client updates.
It is never an unannounced additive response value. delivered means the recipient mail server accepted the message; e2a does not observe or claim inbox placement.
Screening and prompt-injection detections remain outside the lifecycle ledger.
Their existing protection events and documentation remain authoritative; a
screening verdict is not rewritten as delivery, authentication, or inbox state.

For sender-trust decisions, a non-null `verified_domain` means DMARC passed for
that RFC 5322 From domain. On detail responses the equivalent check is
`authentication?.dmarc.status === "pass"`. Only after that check should an
application compare `header_from` with an address allowlist. Neither result
authenticates the mailbox local part, a person, or message content. Before
trusting any field from a webhook, first verify the delivery envelope's
`X-E2A-Signature`; authenticated REST and WebSocket transports need no separate
payload-signature step.

**Managed unsubscribe (beta).** Send, reply, and forward accept the optional strict
object `"unsubscribe":{"mode":"managed"}`. Omission means only that e2a does
not add its managed unsubscribe mechanism; it does not classify the message as
transactional. Managed mode requires exactly one normalized envelope recipient
across To, CC, and BCC. e2a owns the opaque token, adds a visible footer and the
`List-Unsubscribe` / `List-Unsubscribe-Post` headers before DKIM signing, and
hosts the beta raw confirmation flow at `GET|POST /u/{token}`. GET is
scanner-safe confirmation only and never
changes state; the RFC 8058 one-click POST body is
`List-Unsubscribe=One-Click`. The public POST accepts bounded (1 KiB maximum)
`application/x-www-form-urlencoded` and `multipart/form-data` bodies, including
standard charset parameters. The application never stores or constructs the
token. Invalid managed-unsubscribe issuer configuration fails server startup.

Malformed unsubscribe objects (including `null`, missing/unknown modes, or
unsupported fields) use the API's standard schema-validation response,
`422 invalid_request`. A valid managed object with a recipient count other than
one is rejected as `400 invalid_request` with a stable explanatory message.

A confirmed unsubscribe blocks the recipient only for the exact sending agent,
so a sibling agent can still send. Account-wide suppressions continue to block
all agents. Either scope produces the existing `422 recipient_suppressed` on a
future send.

**Outbound attachment limits** (send / reply / forward, enforced on the **decoded**
bytes — not the base64 wire size): at most **10 attachments** per message, each
**≤ 10 MiB**, and **≤ 25 MiB combined**. Too many attachments → `400 invalid_request`;
an attachment or combined total over its size limit → `413 payload_too_large` (the
limit and offending size are in `error.details`). On `forward`, the limits apply to
the **combined** set — the original message's carried-over attachments plus any you
supply. These are conservative starting limits and may be raised over time.

**Byte semantics** — the field name `size_bytes` appears at two levels with two
different meanings:

- **On a message** (`MessageView` / `MessageSummaryView` / the export's
  `Message`): the **raw MIME byte length** of the whole stored message —
  headers + bodies + attachments *as transported* (i.e. base64-encoded
  attachment parts count at their encoded size). It is the octet length of
  `raw_message`.
- **On an attachment** (`attachments[]` on a message, the attachment endpoint,
  and the `email.received` event's attachment metadata): the **decoded payload
  size** — the byte count of the file after the Content-Transfer-Encoding is
  undone; exactly what `download_url` serves, what the 256 KB `?inline` cap is
  checked against, and what the outbound attachment limits above are enforced
  on. Because base64 inflates by ~4/3, a message's `size_bytes` is expected to
  exceed the sum of its attachments' `size_bytes` plus body text.
- **Storage-quota accounting** (`usage.storage_bytes` vs
  `limits.max_storage_bytes` on `GET /v1/account`): per stored message, the
  raw MIME length (the message-level `size_bytes`) **plus** any retained
  held-draft body/attachment columns (these exist only while a message is
  outbound messages and remain retained through terminal transitions) — so for
  sent/inbound messages, storage usage is exactly the sum of their
  `size_bytes`.

**Outbound composed-message ceiling** (send / reply / forward and an outbound
HITL approval after merging reviewer overrides): **10 MiB (10,485,760 bytes)**,
measured as the UTF-8 byte lengths of `subject + text + html` plus the **decoded**
attachment bytes. This is independent of the larger 25 MB aggregate attachment
allowance: a request can satisfy every attachment limit and still exceed the
composed ceiling once its subject and bodies are included. A breach returns
`413 payload_too_large`. Direct send/reply/forward errors include
`error.details.scope = "composed_message"`, `error.details.actual_bytes`, and
`error.details.max_bytes`
(`10485760`); callers should treat `error.details` as optional on other paths.

> **Note:** approve/reject a held message via the account-scoped **Reviews**
> queue below (`POST /v1/reviews/{id}/approve|reject`), which addresses holds by
> id with no inbox email needed. (The older per-message
> `POST …/messages/{id}/approve|reject` endpoints were removed in the pre-GA
> vocabulary freeze.)

### Conversations (`/v1/agents/{email}/conversations`)

Threads derived from `messages.conversation_id`.

- `GET …/conversations` — list threads (`since`/`until`, cursor).
- `GET …/conversations/{id}` — one thread with participants, labels, and member
  messages.

### Reviews (`/v1/reviews`) (beta)

The unified review queue: every message held in `pending_review` across the
account's inboxes — outbound drafts awaiting send approval **and** inbound
messages held by a screening gate. **Account-scoped credentials only**; an agent
cannot see or resolve its own holds (self-approval would defeat the gate).
The unified review resource is beta and may change before it is declared stable.
The product-facing `hold_reason` explanation and the optional technical
`protection` evidence on detail responses are beta and may change before they
are declared stable.
The `flagged` and `flag_reason` projections remain available on review,
message-detail, message-list, and conversation responses so polling agents can
identify delivered policy-flag outcomes. These fields are beta and may change
before they are declared stable.

- `GET /v1/reviews`, `GET /v1/reviews/{id}` — list the queue / full detail of one
  held message.
- `POST /v1/reviews/{id}/approve` — branches on direction: an outbound draft is
  sent via SES (honors `Idempotency-Key` + optional reviewer overrides); an
  inbound hold is released to the inbox. Returns `202 Accepted` with
  `status=accepted` when the outbound delivery is durably queued for async
  submission; synchronous `sent` and inbound `review_approved` outcomes return
  `200 OK`.
- `POST /v1/reviews/{id}/reject` — outbound draft discarded (never sent); inbound
  hold dropped (never reaches the agent; payload retained, hidden, for forensics).

### Templates (`/v1/templates`, `/v1/starter-templates`) (beta)

Reusable email sources (subject + plain-text body + optional HTML body),
stored on the account and rendered server-side at send time via
`{{variable}}` interpolation. Reference one on send with `template_id` or
`template_alias` (mutually exclusive with literal `subject`/`text`/`html`)
plus `template_data`. Full syntax, the raw-slot escaping warning, and the
starter-template catalog are documented in
[`docs/templates.md`](templates.md); this resource and its operations are
beta and may change before they are declared stable.

- `GET /v1/templates`, `POST /v1/templates` — list the account's templates
  (metadata only) / create one (or copy a starter verbatim via
  `from_starter`).
- `GET/PATCH/DELETE /v1/templates/{id}` — fetch full sources / partially
  update (changed parts are re-parsed) / delete (`?confirm=DELETE`; in-flight
  sends using the template are unaffected since rendering happens at send
  time).
- `POST /v1/templates/validate` — dry-run template source without
  persisting: per-part parse errors, a rendered preview, and `suggested_data`
  (a placeholder value for every referenced variable).
- `GET /v1/starter-templates`, `GET /v1/starter-templates/{alias}` — the
  read-only, pre-built starter catalog; list returns metadata only, fetch by
  alias returns full body sources.

### Webhooks (`/v1/webhooks`)

Webhook subscribers (the delivery side of the event log). Each webhook carries its
own **per-webhook signing secret** that signs the payloads sent to it.

- `GET /v1/webhooks`, `POST /v1/webhooks` — list / create (the secret is returned
  once, at creation).
- `GET/PATCH /v1/webhooks/{id}`, `DELETE /v1/webhooks/{id}?confirm=DELETE` —
  fetch / partial-update (`url`/`events`/`filters` are full-replace when present)
  / delete.
- `POST /v1/webhooks/{id}/rotate-secret` — mint a new secret; the previous one
  stays valid for a 24h grace window.
- `GET /v1/webhooks/{id}/deliveries` — the per-webhook delivery log (debug view).
- `POST /v1/webhooks/{id}/test` — fire a one-off synthetic delivery.

Agent-scoped suppression management is beta. The authenticated list, create,
and delete operations and their request/response schemas may change before
they are declared stable. `agent.suppression_added` is a beta event emitted
once when a new exact-agent
block is created. Its current payload is
`{agent_email, address, source}`, where `source` is `unsubscribe` or `manual`.
Consumers must tolerate additive or other beta payload changes. The existing
stable `domain.suppression_added` event remains account-scoped and unchanged.

To verify an inbound webhook payload, pass the webhook's signing secret to the SDK
helper — `construct_event(body, header, secret)` /
`constructEvent(body, header, secret)` does parse + verify in one call. See the
[Python](../sdks/python/README.md#quick-start) and
[TypeScript](../sdks/typescript/README.md#verify-a-webhook) SDK READMEs.

Webhook delivery is **at least once**. Store and deduplicate on the envelope
`id` before applying side effects. A delivery succeeds on any `2xx` response.
Network failures and every non-`2xx` response (including redirects) are retried;
redirects are not followed. The frozen eight-attempt schedule is:

| Attempt | Time after the previous attempt | Cumulative time from attempt 1 |
|---:|---:|---:|
| 1 | immediate | `0` |
| 2 | `1m` | `1m` |
| 3 | `5m` | `6m` |
| 4 | `15m` | `21m` |
| 5 | `1h` | `1h21m` |
| 6 | `4h` | `5h21m` |
| 7 | `8h` | `13h21m` |
| 8 | `16h` | `29h21m` |

After attempt 8 fails, the delivery is terminally `failed` and remains visible
in delivery history for manual redelivery. River persists and rescues jobs, and
the pending-row reconciler re-enqueues a row whose initial job insertion was
interrupted. This provides at-least-once execution, not exactly-once effects.

Every delivery carries these frozen headers:

| Header | Contract |
|---|---|
| `Content-Type` | `application/json` |
| `X-E2A-Signature` | `t=<unix-seconds>,v1=<lowercase-hex-hmac>`; during rotation it contains one `v1` for each active secret |
| `X-E2A-Event-Type` | Exact value of body `type` |
| `X-E2A-Schema-Version` | Exact value of body `schema_version` |
| `User-Agent` | `e2a-webhooks/1` |

The HMAC input is the exact byte sequence
`<decimal-unix-seconds>.<raw-request-body>` and the algorithm is HMAC-SHA256.
Do not parse and reserialize the body before verification. SDK verifiers accept
timestamps within 300 seconds by default and compare every `v1` signature in
constant time. Secret rotation dual-signs with the current and previous secret
for 24 hours; accept a request when either secret verifies during that window.

<a id="webhook-signing-secrets"></a>
> **Signing.** Webhook deliveries are signed per-webhook with the `whsec_`
> secret (rotatable via the `rotate-secret` route above). The envelope signature
> authenticates the structured inbound authentication evidence. HITL approval /
> magic-link tokens use the deployment-wide HMAC secret (`E2A_HMAC_SECRET`).

### Events (`/v1/events`)

The durable, queryable log of every event e2a emits to webhook subscribers
(30-day retention), and the source of truth for replay. See
[events.md](events.md) for the event taxonomy, reconciliation pattern, and replay
semantics.

- `GET /v1/events` — filter by `type`/`agent_email`/`conversation_id`/`message_id`
  and time range; cursor pagination.
- `GET /v1/events/{id}` — one event (returns `410 Gone` past retention).
- `POST /v1/events/{id}/redeliver` — re-enqueue delivery for an event (to one
  webhook or all originally-matched). Returns `202 Accepted`: the redelivery is
  durably enqueued for async submission (per-delivery `status: pending`, or
  `scheduled` for the fan-out), not delivered synchronously. Receivers must dedup
  on event id.

## Real-time delivery (WebSocket)

`GET /v1/agents/{email}/ws` — WebSocket for real-time inbound. Authenticated by
the `Authorization: Bearer <api_key>` handshake header (the credential never
appears in the URL). Not part of the OpenAPI document (it is not an HTTP
request/response operation).

The server pushes the SAME versioned event envelope a webhook delivery
carries — `{type, id, schema_version, created_at, data}` with the
`email.received` payload (`EmailReceivedData`; see
[events.md](events.md#envelope-and-typed-payloads)) — so one parser serves
both channels, and the event `id` (identical across channels for the same
event) lets a consumer dedup WS-vs-webhook. Tolerate unknown `type` values:
future WS event kinds arrive in the same envelope. Metadata only; fetch full
content via `GET /v1/agents/{email}/messages/{id}`:

```json
{
  "type": "email.received",
  "id": "evt_62eb7644b075459043c358bc6448d754",
  "schema_version": "1",
  "created_at": "2026-04-24T10:00:00.123456789Z",
  "data": {
    "message_id": "msg_abc123",
    "agent_email": "bot@your-domain.com",
    "direction": "inbound",
    "conversation_id": "conv_xyz",
    "header_from": "alice@example.com",
    "envelope_from": "bounce@example.com",
    "verified_domain": "example.com",
    "authentication": {
      "spf": {"status": "pass", "domain": "example.com", "aligned": true},
      "dkim": [],
      "dmarc": {"status": "pass", "domain": "example.com", "policy": "reject", "aligned_by": ["spf"]}
    },
    "to": ["bot@your-domain.com"],
    "cc": [],
    "reply_to": [],
    "delivered_to": "bot@your-domain.com",
    "subject": "Meeting tomorrow",
    "received_at": "2026-04-24T10:00:00.123456789Z"
  }
}
```

On connect, all unread messages are drained as `email.received` events
automatically. Live events carry the same marshaled event envelope as the
webhook delivery — identical fields and event id; byte layout may differ
(JSON key order/escaping is not contractual). Reconnect drain reuses the
durable event envelope, including `header_from`, `envelope_from`, the derived
`verified_domain`, and structured `authentication` evidence.

### Connection lifecycle & close codes

**One connection per agent.** The server holds at most one WebSocket per
agent: when a newer connection for the same agent completes its handshake,
it wins, and the older connection is closed with code **4000 `replaced`**.
WS is an opportunistic push channel on top of the durable pollable inbox and
webhook subscriptions — if you need fan-out to several consumers, use
webhooks (or poll), not multiple sockets.

Close codes are a frozen part of the API contract. Standard codes keep their
standard semantics; e2a-specific conditions use application codes in the
4000–4999 range:

| Code | Reason token | Meaning | Client action |
|---|---|---|---|
| `1000` | *(empty)* | Normal closure. | None — expected after your own close. |
| `1001` | `shutting_down` | Server shutdown/restart (e.g. a deploy). | Reconnect with backoff. |
| `1001` | `ping_timeout` | The server dropped an unresponsive connection (missed keepalive pong). Usually observed as a `1006` abnormal close instead, since the peer is already gone. | Reconnect with backoff. |
| `1006` | *(n/a — never sent; synthesized locally)* | Abnormal close / network drop. | Reconnect with backoff. |
| `1008` | *(human-readable message)* | Genuine policy rejection of an **established** connection. Reserved: the server does not currently close established connections with 1008 — all credential/ownership rejections happen at the handshake as HTTP errors (below). | Do **not** reconnect — retrying the same connection cannot succeed. |
| `1011` | *(human-readable message)* | Internal server error. | Reconnect with backoff. |
| `4000` | `replaced` | A **newer connection for this agent** superseded this one (one-connection-per-agent). Benign — but the superseded client must stop: auto-reconnecting would steal the socket back from its replacement and loop. | Do **not** reconnect. Surface the condition (SDKs raise/emit `E2AConnectionReplacedError`). |
| `4001`–`4999` | — | Reserved for future e2a-specific terminal conditions (e.g. agent deleted mid-connection). | Treat any unrecognized 4xxx as terminal: do **not** auto-reconnect. |

Reason strings on e2a-specific closes are short stable `snake_case` tokens
(`replaced`, `shutting_down`, `ping_timeout`) — safe to branch on, though
clients should branch on the **code** first; reasons on standard codes may be
human-readable text.

Handshake rejections (missing/invalid key → `401`, agent-scoped key for a
different agent → `403`, nonexistent or not-your agent → `404`) happen
**before** the upgrade and return the canonical HTTP error envelope, never a
close code. The SDKs treat those as fatal too (typed error, no retry loop).

SDK behavior (TS `WSListener`/`WSStream`, Python `WSStream`, and the CLI
`listen` command, which inherits from the TS SDK): transient closes reconnect
with exponential backoff; `1000` stops cleanly; `4000 replaced`, `1008`, unknown 4xxx, and fatal
handshake rejections stop the stream with a typed error. The CLI prints a
`listener replaced` explanation and exits `5` (permanent — retry wrappers
must not rerun it).

## HITL magic links

These accept a signed `t` query parameter (from notification emails) instead of an
API key, so a reviewer can approve/reject from any mail client without auth. They
live under `/v1` because the paths are the literal links embedded in notification
emails (not part of the OpenAPI document):

- `GET`/`POST` `/v1/approve?t=…` — approve a held message via signed token.
- `GET`/`POST` `/v1/reject?t=…` — reject a held message via signed token.

## Meta / unauthenticated

- `GET /v1/info` — public deployment discovery: `shared_domain`,
  `slug_registration_enabled`, `public_url`, `version`. CLIs/SDKs hit this to
  self-configure from a single base URL.
- `GET /api/health` — health check.
- `POST /api/feedback` — submit feedback (rate-limited per-IP).
