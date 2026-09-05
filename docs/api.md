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

## Stability: GA and beta surface

The core `/v1` surface is the **stable, generally-available (GA) contract**; a
small, explicitly enumerated set of newer resources is **beta**. The
machine-readable source of truth is **`x-stability-level`** in
[`api/openapi.yaml`](../api/openapi.yaml): anything marked
`x-stability-level: beta` may change before it is promoted to stable, and
**everything not marked beta (or experimental) is GA**, covered by the
[compatibility rules](#compatibility-rules) below. At the operation level that
is currently 43 GA operations and 31 beta operations:

| Resource group | Stability | Operations |
| --- | --- | --- |
| Account — whoami, export, delete, API keys, account-wide suppressions | **GA** | `getAccount`, `exportAccount`, `deleteAccount`, `listApiKeys`, `createApiKey`, `deleteApiKey`, `listSuppressions`, `deleteSuppression` |
| Agents — CRUD, restore, test | **GA** | `listAgents`, `createAgent`, `getAgent`, `updateAgent`, `deleteAgent`, `restoreAgent`, `testAgent` |
| Messages & attachments — send/reply/forward, list/get/update, trash/restore | **GA** | `sendMessage`, `replyToMessage`, `forwardMessage`, `listMessages`, `getMessage`, `updateMessage`, `deleteMessage`, `restoreMessage`, `getAttachment` |
| Conversations | **GA** | `listConversations`, `getConversation` |
| Domains — register, verify, delete | **GA** | `listDomains`, `registerDomain`, `getDomain`, `deleteDomain`, `verifyDomain` |
| Webhooks — CRUD, secret rotation, deliveries, test | **GA** | `listWebhooks`, `createWebhook`, `getWebhook`, `updateWebhook`, `deleteWebhook`, `rotateWebhookSecret`, `listWebhookDeliveries`, `testWebhook` |
| Events — durable log + redelivery | **GA** | `listEvents`, `getEvent`, `redeliverEvent` |
| Meta — deployment discovery | **GA** | `getInfo` |
| [Contacts & outreach](#contacts--outreach-v1contacts-v1agentsemailcontacts-beta) | **beta** | `createContact`, `listContacts`, `getContact`, `updateContact`, `deleteContact`, `importContacts`, `deleteImportBatch`, `listEngagements`, `getEngagement`, `upsertEngagement`, `deleteEngagement` |
| [Templates & starter templates](#templates-v1templates-v1starter-templates-beta) | **beta** | `createTemplate`, `listTemplates`, `getTemplate`, `updateTemplate`, `deleteTemplate`, `validateTemplate`, `listStarterTemplates`, `getStarterTemplate` |
| [Reviews (HITL queue)](#reviews-v1reviews-beta) | **beta** | `listReviews`, `getReview`, `approveReview`, `rejectReview` |
| Agent protection config | **beta** | `getAgentProtection`, `putAgentProtection` |
| Agent-scoped suppressions | **beta** | `listAgentSuppressions`, `createAgentSuppression`, `deleteAgentSuppression` |
| [Message lifecycle diagnostics](#message-lifecycle-diagnostic-contract-beta) | **beta** | `getMessageLifecycle` |
| Delivery metrics — per agent and account-wide | **beta** | `getAgentMetrics`, `getAccountMetrics` |

**Beta fields and capabilities on otherwise-GA operations** (property-level
`x-stability-level: beta` in the spec — or, where only specific *values* of a
stable field are beta, `x-experimental-values` on that field):

- **Scheduled sending** — the `send_at` request field, the `scheduled_at`
  response field, and the `scheduled` send-result `status` value on
  send/reply/forward.
- **`thread_id`** on message list/detail reads (server-owned, read-only
  email-topology identity).
- **Message-list boolean filtering** — the `filter` query parameter on
  `listMessages` (the marker rides on the parameter's inline schema; the
  field/operator vocabulary may still evolve — see
  [Message filtering](filtering.md)).
- **Template references on send** — the `template_id` / `template_alias` /
  `template_data` request fields.
- **Managed unsubscribe** — the `unsubscribe: {mode: "managed"}` request
  object and its raw `GET|POST /u/{token}` confirmation flow.
- **Quoted-history replies** — the `quote_history` request field on reply
  (server-composed mail-client-style quoted history; see the field reference
  in [Messages](#messages-v1agentsemailmessages)).
- **Review-hold projections** — `hold_reason`, the review-detail `protection`
  evidence, and the `flagged` / `flag_reason` verdict fields.
- **Lifecycle transitions on events** — the optional `lifecycle_transitions`
  payload field on the seven otherwise-stable event payload schemas
  (`email.received`, `email.sent`, `email.failed`, `email.delivered`,
  `email.bounced`, `email.complained`, `domain.suppression_added`). See
  [Message lifecycle diagnostics](#message-lifecycle-diagnostic-contract-beta)
  and [events.md](events.md#lifecycle-transitions-on-events-beta).
- **Account export interior schemas** — `GET /v1/account/export` is a GA
  operation, but its interior record shapes are versioned by the export's
  `schema_version` envelope field rather than the v1 freeze, and are
  beta-marked to record that exemption.

**Beta events:** `contact.due`, `agent.suppression_added`, and the payloads of
the screening + review-hold event types (`email.flagged`, `email.blocked`,
`email.review_requested`, `email.review_approved`, `email.review_rejected` —
marked via `x-experimental-values` on the stable `type` field). The stable
`error.code` vocabulary likewise marks only `blocked_by_policy` experimental.
See [events.md](events.md).

The exact operation-level list is repeated with methods and paths in
[Beta operations](#beta-operations) below; everything else in this document is
GA unless its heading or prose says `(beta)`.

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
- **Email-address limits.** Two independent bounds apply to address-bearing
  request fields, and they count different things:
  - The schema's `maxLength: 320` bounds the whole submitted **string** —
    display name + address combined — in Unicode **code points**.
  - SMTP's mailbox limits bound the parsed **addr-spec** in **octets** (UTF-8
    bytes): the local part is at most **64 octets** and the whole
    `local@domain` at most **254 octets**. They are enforced synchronously, at
    the edge, on the send paths: a violating `to`/`cc`/`bcc` entry on
    send/reply/forward is `400 invalid_recipient`, and a violating `reply_to`
    is `400 invalid_request`.

  The octet limits are the ones that surprise callers, because they are not the
  320 the schema advertises: a long plus-addressed local part
  (`orders+2026-07-30-region-eu-west-batch-000123@…`) or a non-ASCII SMTPUTF8
  local part can pass `maxLength` and still be rejected — one emoji is four
  octets but only one code point.
- **`Location` on `201`.** Creates that mint an addressable resource return a
  path-relative `Location`. Its path segments are percent-encoded **per
  segment**, which legally leaves the sub-delimiters `@ + & = : $`
  **unescaped**: `/v1/contacts/a.partner@fund.vc` is a valid, expected value,
  and so is the fully-escaped `/v1/contacts/a.partner%40fund.vc`. Neither form
  is promised. Do not string-compare the header against a URL you built
  locally — to recover the resource key, **percent-decode the final path
  segment**, then compare.
- **`ETag` / `If-Match`.** Reads that support optimistic concurrency return an
  `ETag`: a **strong** validator, currently rendered as a quoted 32-hex
  character token (`"9f86d081884c7d65…"`). Treat it as **opaque** — never
  parse it, derive it, or construct one. Store the received value and replay it
  **verbatim** in a later `If-Match`. Any accepted write moves the validator,
  so a stale one cannot match and the conditional write is rejected with
  `412 precondition_failed`.
- **Pagination.** List endpoints return `{ items, next_cursor }`; pass
  `next_cursor` back as `?cursor=…` to page forward. The SDKs auto-page. A
  cursor is bound to the exact request that issued it: the account that minted
  it, the collection, its path parameters (the agent, webhook, or message it
  was listed under), and the filters it was minted with. Replaying one on a
  different list, a different parent, with different filters, or under a
  different account's credentials is a `400 invalid_cursor` rather than a
  silently wrong page. Cursors are opaque; treat them as ephemeral — on
  `invalid_cursor`, drop the cursor and restart the query from the first page.
- **Idempotency.** Ten mutating operations honor an opt-in `Idempotency-Key`
  header: `sendMessage`, `replyToMessage`, `forwardMessage`, `approveReview`,
  `createWebhook`, `rotateWebhookSecret`, and `createApiKey`, plus the beta
  contact operations `createContact` and `importContacts`, plus
  `deleteDomain`. Semantics:
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
  - **Atomic exceptions.** Accepted keyed sends, webhook creation, and domain
    deletion commit their replay record in the same transaction as the side
    effect. Other operations remain best-effort: under idempotency-store
    degradation or a mid-request crash, a keyed retry may re-execute them.
- **No NUL bytes and no invalid UTF-8 on `/v1`.** No client-supplied string in
  a `/v1` request may contain `U+0000`, and every client-supplied byte sequence
  must be well-formed UTF-8 — anywhere in the JSON body (at any depth,
  including object *keys*), or in a path, query, or header parameter.
  Violations are `400 invalid_request` with the offending field in
  `error.details.fields[].location`; a body whose raw bytes are not valid
  UTF-8 is rejected before parsing, located at `body`. The rules are blanket
  rather than per-field on purpose: neither a `NUL` nor a broken byte sequence
  can be stored in a text column at all, and a caller cannot tell from the
  outside which strings are persisted and which are only composed, so the
  answer is the same everywhere. In JSON, a `NUL` can only arrive as the
  `\u0000` escape; invalid UTF-8 can only arrive as raw bytes, which RFC 8259
  §8.1 forbids in JSON anyway. Valid multi-byte UTF-8 — CJK, emoji, even a
  properly encoded `U+FFFD` — is always accepted; only malformed byte
  sequences are refused, and the offending bytes are never echoed back in the
  error.

  Three consequences are worth calling out because they change answers a client
  may already branch on:

  - **A malformed path param is now `400`, not `404`.** Reading a resource whose
    id carries an invalid byte — `GET /v1/agents/%FF`, `GET /v1/webhooks/%FF` —
    used to look up a name that could not exist and answer `404 not_found`.
    Every such read now answers `400 invalid_request` located at the path
    param, matching what the write on the same id always should have done. A
    well-formed id that simply does not exist is still `404`.
  - **A malformed `cursor` changes `error.code`.** `?cursor=<invalid bytes>` was
    `400 invalid_cursor`; it is now `400 invalid_request` located at
    `query.cursor`, because the content rule runs before the cursor is decoded.
    The status is unchanged, but a client branching on `error.code` sees the
    new value. A cursor that is well-formed text yet not a valid cursor is
    still `400 invalid_cursor`.
  - **The rules run before authentication.** An unauthenticated request that
    also carries an invalid byte gets `400 invalid_request`, not `401
    unauthorized`. Nothing about the credential is disclosed — the answer
    depends only on bytes the caller sent — but a client that treats "`401` ⇒
    refresh the token" must not read this `400` as a credential problem. A
    clean unauthenticated request is still `401`.

  (The rules are enforced structurally on the `/v1` **operations** — every
  operation is registered through one seam that walks its bound params, and
  every request body is checked as raw bytes before decoding — so a new
  operation inherits them by construction. Four `/v1` paths are not operations
  and are guarded individually instead: the HITL magic-link pages `/v1/approve`
  and `/v1/reject`, where `reason` — the only caller-authored text on either —
  is checked in the handler; and the read-only WebSocket handshake
  `/v1/agents/{email}/ws` plus the attachment download
  `/v1/agents/{email}/messages/{id}/attachments/{index}/download`, which
  normalize or capability-check every caller string before it can reach
  storage. Non-`/v1` entry points — the dashboard's session-authenticated
  `/api/*` routes, the public unsubscribe handler, and inbound SMTP — are
  outside the rules and validate independently.)
- **Conditional writes (`ETag` / `If-Match`).** Single-resource `GET`s return an
  `ETag`; pass it back as `If-Match` on the write to reject a lost update with
  `412 precondition_failed`. Three rules are worth stating because they are easy
  to get wrong — two follow RFC 9110 §13.1.1, and the first **deliberately
  deviates** from it, for the reason given there:
  - Send the validator exactly as you received it. §13.1.1 specifies *strong*
    comparison for `If-Match`, but the `W/` weak prefix is **tolerated** here on
    purpose. `api.e2a.dev` is behind a Cloudflare edge that transforms responses
    (it compresses; our origin does not), and Cloudflare downgrades a strong
    `ETag` to its weak form whenever it transforms one — a downgrade we cannot
    disable on our plan. Refusing the weak form would hand a client a permanent
    `412` for echoing back exactly the validator it was given, and no retry
    would ever clear it. The comparison still covers the full validator, which
    changes on every accepted write, so a stale tag never matches.
  - `If-Match: *` means "if any current representation exists": it succeeds on
    an existing resource and is refused when there is none (it never creates).
  - Sending the header with an **empty value** is `400 invalid_request`, not an
    unconditional write. Omit the header entirely to write unconditionally —
    otherwise an unset variable interpolated into the header would silently
    perform the very write the guard was there to prevent.
- **Bulk import: row-level vs request-level.** `POST /v1/contacts/import`
  returns a per-row outcome so one bad row never rejects the upload. That
  isolation covers a row's *content* — an unparseable or over-long address, an
  over-long `display_name`, metadata outside the per-contact bounds — each of
  which fails only its own row (`status: "failed"` with `invalid_recipient` or
  `invalid_request`). Problems with the *request* still reject the whole thing:
  malformed JSON, an unknown field, a missing `address` key, a violation of the
  well-formed-text rules above (a `NUL` anywhere in the body, or a body whose
  raw bytes are not valid UTF-8), more than 1000 rows, or a body over 20 MiB.
  Both text rules are request-level for the same reason: neither is row
  *content* — they are bytes the request document may not contain at all, the
  same class as malformed JSON — so they reject the upload rather than failing
  the row that happens to carry them.
- **Errors.** Non-2xx responses use a single `ErrorEnvelope` shape; branch on
  `error.code` (see [Error codes](#error-codes) below for the full vocabulary).
- **Capacity limits — the permanent `402` / `429` split.** Two different limits
  can block a write, and they are **permanently distinct** — branch on the HTTP
  status:
  - **`402 limit_exceeded`** is a **quota** (a stock/flow cap): the send
    allowance, storage bytes, agent/domain counts. Message-flow allowances
    count **outbound recipient-deliveries** — a message to N distinct
    recipients consumes N units, and received mail is free and unmetered.
    A retry alone will not clear it — surface an upgrade/quota path.
    `error.details` is a `LimitExceededDetails` whose `resource`
    (`agents | domains | messages_month | storage_bytes | messages_day`) keys
    the cap to `usage.<resource>` / `limits.max_<resource>` on
    `GET /v1/account` (`messages_day` — a per-UTC-day send cap some accounts
    carry — has no AccountView field; it clears when the UTC day rolls over).
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
or the state first); `rate_limited`, `idempotency_in_flight`,
`message_not_yet_delivered`, and the 5xx
`internal_error`/`limits_unavailable`/`inbound_mx_check_failed` are the
retryable ones (the per-row retry notes in the table below are authoritative).

| Code | Status | Meaning |
| --- | --- | --- |
| **Auth / policy** | | |
| `unauthorized` | 401 | Missing or invalid credentials (REST and the WebSocket handshake). |
| `forbidden` | 403 | Authenticated but not allowed (key scope, cross-tenant access). |
| `blocked_by_policy` | 403 | **Experimental.** The outbound message was blocked by the agent's outbound policy gate. |
| `sending_paused` | 403 | Outbound sending is paused for the account by the platform abuse controls. Nothing was queued; queued mail is held until an operator resumes. |
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
| `attachment_not_found`, `contact_not_found`, `engagement_not_found`, `import_batch_not_found`, `template_not_found`, `starter_template_not_found` | 404 | The `*_not_found` family — a specific sub-resource is missing. |
| `gone` | 410 | The event exists but is past the 30-day retention window. |
| **Conflict / state** | | |
| `conflict` | 409 | Generic state conflict (e.g. redelivery to a webhook that never matched the event). |
| `agent_taken`, `domain_taken`, `alias_taken` | 409 | The `*_taken` family — the requested identifier (agent address, domain, template alias) is already claimed. |
| `address_in_trash` | 409 | The requested agent address is reserved by a soft-deleted agent; restore or permanently delete it first. |
| `message_held` | 409 | The message is held for review and cannot perform the requested operation until the hold is resolved. |
| `message_not_pending` | 409 | The review hold was already resolved (approved/rejected/expired). |
| `message_not_yet_delivered` | 409 | Reply/forward target is an outbound message still queued for provider submission. A reply cannot thread until the provider assigns its Message-ID; a forward requires the source message to have actually been sent. Retry-able: retry once it is sent, or use `wait=sent` on the original send. |
| `not_in_trash` | 409 | Restore or permanent-delete was requested for a resource that is not currently in trash. |
| `purge_in_progress` | 409 | Permanent agent deletion has been claimed and can no longer be reversed; retry deletion to resume it. |
| `send_in_progress` | 409 | The message send is already executing; wait for its terminal outcome. |
| `webhook_disabled` | 409 | Operation requires an enabled webhook. |
| `webhook_cooldown` | 409 | The webhook was auto-disabled and cannot be re-enabled until the cooldown elapses. SDKs do not automatically retry it; retry manually only after the cooldown. |
| `precondition_failed` | 412 | The resource changed since the supplied `If-Match` value was read. Fetch the latest representation and retry the edit deliberately. |
| `domain_not_registered` | 400 | Create-agent on a domain the account has not registered. |
| `domain_has_agents` | 400 | Domain delete blocked while agents exist on it — **including agents in the trash**, which keep their addresses for the 30-day restore window and do not appear in `list_agents`. The message names which kind is blocking and how many; purge trashed ones with `?confirm=DELETE&permanent=true`. |
| `domain_not_verified` | 400 / 403 | Domain verification pending — 400 on create-agent, 403 on send paths. |
| `inbound_mx_missing` | 400 | Inherited subdomain agent creation requires an exact or wildcard MX routing to e2a. |
| **Capacity — see the 402/429 split above** | | |
| `limit_exceeded` | 402 | Plan **quota** (stock/flow cap); `details` is `LimitExceededDetails`. Not retryable. |
| `rate_limited` | 429 | Request-**rate** limit; wait `details.retry_after_seconds` / `Retry-After`, then retry. |
| `contact_limit_reached`, `template_limit_reached`, `webhook_limit_reached` | 400 | Fixed per-account count caps (not plan quotas) — delete one first. |
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
| `auth_unavailable` | 503 | An auth backend could not judge the credential (e.g. a delegated-token verifier not yet ready, or the identity store) — transient, retryable. The credential itself was neither accepted nor rejected. |
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

The `/v1` surface is the **stable, generally-available contract** as of e2a
**1.5.0**, the release that completed the API freeze. Earlier `v1.0.x`
application/cherry-pick tags predate the freeze and are not `/v1`
compatibility baselines. Our commitment, and what you can rely on:

### Beta operations

This is the complete operation-level exception list. These operations carry
`x-stability-level: beta` and may change before they are promoted to stable;
every `/v1` operation not listed here is covered by the GA freeze.

| operationId | Method and path | Surface |
| --- | --- | --- |
| `approveReview` | `POST /v1/reviews/{id}/approve` | Reviews |
| `createAgentSuppression` | `POST /v1/agents/{email}/suppressions` | Agent suppressions |
| `createContact` | `POST /v1/contacts` | Contacts |
| `createTemplate` | `POST /v1/templates` | Templates |
| `deleteAgentSuppression` | `DELETE /v1/agents/{email}/suppressions/{address}` | Agent suppressions |
| `deleteContact` | `DELETE /v1/contacts/{address}` | Contacts |
| `deleteEngagement` | `DELETE /v1/agents/{email}/contacts/{address}` | Contacts |
| `deleteImportBatch` | `DELETE /v1/contacts/imports/{batch_id}` | Contacts |
| `deleteTemplate` | `DELETE /v1/templates/{id}` | Templates |
| `getAccountMetrics` | `GET /v1/metrics` | Delivery metrics |
| `getAgentMetrics` | `GET /v1/agents/{email}/metrics` | Delivery metrics |
| `getAgentProtection` | `GET /v1/agents/{email}/protection` | Protection config |
| `getContact` | `GET /v1/contacts/{address}` | Contacts |
| `getEngagement` | `GET /v1/agents/{email}/contacts/{address}` | Contacts |
| `getMessageLifecycle` | `GET /v1/agents/{email}/messages/{id}/lifecycle` | Message lifecycle |
| `getReview` | `GET /v1/reviews/{id}` | Reviews |
| `getStarterTemplate` | `GET /v1/starter-templates/{alias}` | Starter templates |
| `getTemplate` | `GET /v1/templates/{id}` | Templates |
| `importContacts` | `POST /v1/contacts/import` | Contacts |
| `listAgentSuppressions` | `GET /v1/agents/{email}/suppressions` | Agent suppressions |
| `listContacts` | `GET /v1/contacts` | Contacts |
| `listEngagements` | `GET /v1/agents/{email}/contacts` | Contacts |
| `listReviews` | `GET /v1/reviews` | Reviews |
| `listStarterTemplates` | `GET /v1/starter-templates` | Starter templates |
| `listTemplates` | `GET /v1/templates` | Templates |
| `putAgentProtection` | `PUT /v1/agents/{email}/protection` | Protection config |
| `rejectReview` | `POST /v1/reviews/{id}/reject` | Reviews |
| `updateContact` | `PATCH /v1/contacts/{address}` | Contacts |
| `updateTemplate` | `PATCH /v1/templates/{id}` | Templates |
| `upsertEngagement` | `PUT /v1/agents/{email}/contacts/{address}` | Contacts |
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
- **Beta surfaces are marked `x-stability-level: beta`** in the spec for
  automated compatibility tools (operations, schemas, and individual fields)
  and `(beta)` in prose. The complete current inventory — resource groups,
  beta fields on otherwise-GA operations, and beta events — is the
  [Stability: GA and beta surface](#stability-ga-and-beta-surface) matrix at
  the top of this document (the operation-level list is repeated with methods
  and paths in [Beta operations](#beta-operations) above); this bullet states
  the policy and deliberately does not repeat the list. Beta surface is
  **exempt from the freeze**: it may change or be removed without a major
  version. Where only specific *values* of a stable field are beta (the
  screening + review-hold event types on webhook `events` fields, the
  `scheduled` send-result `status` value), the field carries
  `x-experimental-values` listing exactly those values — the field itself
  stays stable, the listed values (and their payloads) may still change, and
  every unlisted value is stable. The stable `ErrorBody.code` discriminator
  similarly marks only `blocked_by_policy` experimental. Anything not marked
  beta or experimental is stable surface. One deliberate schema-level use of
  the beta marker under a **stable** operation: the account export's interior
  record schemas (`GET /v1/account/export`) are beta-marked because they are
  versioned by the export's stable `schema_version` envelope field rather
  than by the v1 freeze — see the account section below.
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
- **Deprecation & sunset.** If we ever need to wind something down, it stays
  functional and is marked `deprecated` in the OpenAPI spec; we will not
  remove it within GA `/v1`. (While the API was being frozen pre-GA, the
  legacy agent-path `…/messages/{id}/approve|reject` endpoints were removed in
  favor of the account-scoped `/v1/reviews/{id}/approve|reject` queue — the
  last such removal before the freeze took effect.)

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
- `GET /v1/account/export` — self-service account-data export supporting
  access requests: profile, agents, domains, API key metadata, messages,
  usage events, protection events, OAuth connections, and suppressions.
  Omits internal identifiers. It is not yet an exhaustive export of every
  account-owned resource; examples currently omitted include contacts,
  contact engagements, contact import batches, templates, webhook
  subscriptions, and webhook event/delivery history. See
  [data-handling.md](data-handling.md).
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
  The exported `Message` record is **not** the same shape as the live
  `MessageView`, by design. It deliberately omits the beta `thread_id`: thread
  identity is a server-owned read projection of e2a's mailbox-local reply
  topology, not a fact about the user's data, and the same reasoning already
  keeps it out of stored events and webhook payloads. It does carry the beta
  `scheduled_at` (marked `x-stability-level: beta`, like its `MessageView`
  sibling) because a scheduled-but-unsent message is genuinely the account's
  own pending data.
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
of `deleted: true`. Domain deletion adds the durable, open-set
`sending_teardown` state described below.

### Domains (`/v1/domains`)

Custom sending/receiving domains and their DNS verification.

- `GET /v1/domains`, `POST /v1/domains` — list / register (returns required MX +
  TXT records and the DKIM selector/key).
- `GET /v1/domains/{domain}`, `DELETE /v1/domains/{domain}?confirm=DELETE` —
  fetch / delete (delete deprovisions the sending identity; irreversible).
  The deletion receipt's open `sending_teardown` value is the DNS-release
  contract: only `confirmed` proves provider absence. Keep DNS published for
  `pending`, `manual_review`, missing, or unknown values. Send a unique
  `Idempotency-Key` for each logical deletion and reuse it after an ambiguous
  network failure: within the published key-retention window, the server
  atomically binds it to the deleted registration and follows that receipt as
  `pending` advances, without deleting a later registration of the same domain.
  After retention expires, the same key starts a new operation. If a
  replacement is live, the DNS-release signal fails closed as `pending`. Use a
  new key only to delete that
  replacement registration. While the domain remains absent, an unkeyed repeat
  polls the newest owner-scoped receipt; never use an unkeyed retry across
  re-registration.
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

Every domain response also carries **`sending_ramp`** — the platform-managed
recipient-volume ramp state for newly verified custom sender domains:
`status` (open set; known values `inactive | ramping | complete | exempt`),
`daily_recipient_limit` (zero means no cap applies), `recipients_used_today`,
`active_days` / `ramp_days`, and `resets_at` / `estimated_completion_at`. You
can read this state but cannot change the schedule, exempt yourself, or reset
progression through the API — see
[`docs/runbooks/sending-ramp.md`](runbooks/sending-ramp.md) for the
operator-side mechanics.

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
  not_in_trash` if the agent isn't in the trash; `409 purge_in_progress` once
  irreversible permanent deletion has begun.
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
single message. The send, reply, and forward operations remain stable, but their
optional scheduled-sending capability is **beta** and may change before it is
declared stable.

- `GET …/messages` — list inbound + outbound with filters (`direction`,
  `read_status`, `sort`, `from`, `subject_contains`, `conversation_id`, `labels`,
  `since`, `until`) and cursor pagination. `filter` (beta) adds boolean
  composition; see
  [message filtering](filtering.md) for its grammar and v1 fields. Held outbound
  drafts appear with `status=pending_review`.
  The optional beta `thread_id` on each returned message is a server-owned,
  mailbox-local email-topology identity. It is omitted for legacy messages
  without an assignment. `conversation_id` remains caller-owned application
  correlation and its existing filter does not filter by email thread.
- `POST …/messages` — send a new email (a new thread). Returns `202 Accepted` for
  every non-terminal outcome — `pending_review` when the agent's protection policy
  holds it for review, `scheduled` **(beta)** when a future `send_at` is durably
  queued, or `accepted` when the async pipeline durably queues immediate
  submission — and `200 OK` for the terminal-synchronous `sent`. The send result
  `status` is an open set — known values `accepted | scheduled | sent |
  pending_review | review_approved | failed`. **Always branch on `status`, not
  the HTTP code.**
  `accepted` (async pipeline) means the message is durably persisted and queued;
  the terminal outcome then arrives via the `email.sent` / `email.failed` webhook
  events or `GET …/messages/{id}`. `provider_message_id` is absent until the
  message is actually sent. `scheduled` **(beta)** is also successful durable
  acceptance: do not re-send it. `scheduled_at` **(beta)** is the future
  submission time (a “not before” bound; provider retries can make submission
  later). Optional `?wait=sent` holds an immediately queued request until the
  message reaches a terminal-or-held state or a bounded timeout; a future
  `send_at` instead returns `status=scheduled` immediately and does not wait
  until that time.
- `quote_history` **(beta)** on reply: when `true`, the server appends
  the referenced message beneath the reply body as mail-client-style quoted
  history — an `On <date>, <sender> wrote:` attribution line, the original
  text `>`-prefixed, and (when an `html` body is supplied) the original HTML
  in a blockquote. Composed at accept time, so a held reply shows the reviewer
  the final quoted content. Only body parts the caller supplies are quoted (a
  text-only reply stays text-only). Which parts the *parent* carries does not
  change that: quoting a parent that has no `text/plain` part renders its HTML
  to plain text for the quoted text body, so both alternatives of the reply
  carry the same history. A parent with no readable body at all (unparseable
  MIME, or markup that renders to nothing) sends the reply verbatim rather
  than emitting an empty quote block — the response is the same either way, so
  do not treat a `202` as proof that history was attached. The quoted parent
  counts against the 10 MiB composed-message ceiling, so a small reply to a
  huge parent can return `413 payload_too_large` (scope `composed_message`).
  Defaults to `false` (bodies are sent exactly as provided). May change before
  it is declared stable.
- `send_at` **(beta)** on send/reply/forward must be RFC 3339 with an explicit
  UTC offset, can be at most 90 days ahead, and **survives a review hold**: a
  held message keeps its `send_at` (surfaced as `scheduled_at` on the
  `pending_review` message and in the review queue), and approval re-arms the
  send — submitted at `send_at` if still in the future, or immediately if it has
  already passed. A future `send_at` whose only recipient is the sending agent's
  own address returns `400 invalid_request` because self-delivery is an
  immediate loopback with no scheduled arm (even when the message would
  otherwise be held for review). Trashing a scheduled message before provider
  submission starts prevents submission; once submission has a fresh lease,
  delete returns `409 send_in_progress` and must be retried.
  Restoring it before `scheduled_at` re-arms the existing job; restoring at or
  after `scheduled_at` restores the message but leaves the send canceled.
  `scheduled_at` is a **"not before" bound, not an exact fire time**: provider
  submission is capped at 60 messages/min/agent (a durable fire-time limit
  shared with immediate sends), so a large burst scheduled for one instant
  drains over minutes — e.g. 3,600 messages scheduled for 9:00 finish
  submitting around 10:00 — with each message holding `delivery_status=
  accepted` until its turn.
- **`delivery_status`** on a message follows `accepted → sending → sent →
  delivered | deferred | bounced | complained | failed`. Note **`sent` ≠
  `delivered`**: `sent` means the upstream provider (SES) accepted the message,
  not that the recipient's server did. Delivery/bounce/complaint are per-recipient
  async outcomes reported later via SNS and the corresponding webhook events.
  While a future `scheduled_at` is pending, `delivery_status` remains
  `accepted`; `scheduled` is the send-result `status`, not a
  `delivery_status` value.
- `GET …/messages/{id}` — fetch one message (inbound or outbound), including the
  raw message and structured inbound authentication evidence. Reading an unread
  inbound message flips it to `read`. The response may carry the same optional
  beta, read-only `thread_id` as the list summary. A soft-deleted message remains
  readable by this direct GET and carries `deleted_at` until it is permanently
  purged (30 days after deletion by default; the trash retention window is
  deployment-configurable).
  Ordinary message lists, conversations, reply targets, and forward targets hide
  trashed messages; use `GET …/messages?deleted=true` to enumerate the trash.
- `POST …/messages/{id}/restore` — bring a trashed message back into the inbox.
  Restored message data is retained indefinitely unless deleted again. `409
  not_in_trash` if the message isn't in the trash.
- `PATCH …/messages/{id}` — apply a labels delta (`add_labels` / `remove_labels`).
- `POST …/messages/{id}/reply`, `POST …/messages/{id}/forward` — reply to /
  forward a message; `202` covers `accepted`, `scheduled`, and
  `pending_review`, all distinguished by the response `status`.

Replies preserve the RFC reply headers needed to join the source email thread.
Forwards deliberately do not copy those headers, including when a held forward
is later approved; a forward is a new email thread. This is a wire-level
threading correction, not a request/response schema change. A held outbound
message's idempotency completion is recorded in the hold transaction, so a
post-commit retry cannot create a duplicate held draft.
- `GET …/messages/{id}/attachments/{index}` — attachment metadata + a short-lived
  `download_url` (so binary bytes never stream through an agent's context);
  `?inline=true` returns base64 `data` for small attachments.

`thread_id` is response-only on these existing message list/detail reads.
There is no caller-writable request field, `/threads` resource,
`thread_id` message filter, or complete-thread retrieval guarantee. The field
is not added to send/reply/forward results, conversation responses, reviews,
webhooks/events, WebSocket notifications, exports, or MCP output.

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
| `submission.policy_budget_expired` | `submission` | `failed` | true |
| `submission.sending_setup_expired` | `submission` | `failed` | true |
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

Application conversations derived from caller-owned
`messages.conversation_id`. These resources are workflow/correlation views,
not RFC email threads: one application conversation can span several email
threads, and one email thread can contain several conversation IDs.

- `GET …/conversations` — list application conversations (`since`/`until`,
  cursor).
- `GET …/conversations/{id}` — one application conversation with participants,
  labels, and member messages.

### Contacts & Outreach (`/v1/contacts`, `/v1/agents/{email}/contacts`) (beta)

Contacts are durable **account-level identity** for the people the account
corresponds with; an **engagement** is one agent's outreach state for working a
contact. e2a stores the state and derives real send/reply facts from mail
activity, but it never composes or sends a follow-up on its own. The whole
surface is **beta** and may change before it is declared stable.

**Scope split:** the account-level contact operations require an
account-scoped credential. The per-agent engagement operations additionally
accept an **agent-scoped** credential for its own inbox — that is the
outreach loop a single sending agent drives.

Account-level contact identity:

- `GET /v1/contacts` — list contacts, newest first, with cursor pagination
  and filters: `source` (provenance — open set; known values `import`,
  `manual`, `inbound`; it never changes after creation), `import_batch_id`,
  `created_after` / `created_before`.
- `POST /v1/contacts` — create one contact. The address is canonicalized
  before storage, so a display-name form (`"A. Partner <partner@fund.vc>"`)
  and the bare address are the same contact — a second create returns `409`.
  Honors `Idempotency-Key`.
- `GET /v1/contacts/{address}` — fetch one contact; returns an **`ETag`** for
  use with `If-Match` on a later update.
- `PATCH /v1/contacts/{address}` — partial update (`display_name`,
  `metadata`); omitted fields are left unchanged, and `metadata` is replaced
  wholesale when present. Address and provenance are immutable. An optional
  `If-Match` from a prior read rejects a stale edit with
  `412 precondition_failed`.
- `DELETE /v1/contacts/{address}?confirm=DELETE` — remove a contact (and its
  per-agent engagements). **Suppressions are not affected** — consent
  outlives the contact record, so deleting a contact never makes a
  previously-blocked address sendable.
- `POST /v1/contacts/import` — bulk import, **at most 1,000 rows per
  request** (paginate client-side above that). Returns a `batch_id` plus a
  **per-row outcome** (`created | updated | skipped | failed`, with a
  machine-branchable `code` and a `suppressed` flag), so one malformed row
  never rejects the rest. Import is **inert**: it records identity and sends
  nothing; suppressed addresses are still recorded (flagged) so the counts
  stay honest. `on_conflict` is `merge` (default — refreshes `display_name`
  and `metadata`, leaves provenance and everything hanging off the contact
  untouched) or `skip`. Optional `agent_email` (+ initial `stage`) enrolls
  every valid row with that live owned agent in the same transaction without
  overwriting existing engagement state. Honors `Idempotency-Key` — strongly
  recommended, since a keyed retry of a timed-out upload replays the original
  `batch_id` and per-row results instead of importing twice.
- `DELETE /v1/contacts/imports/{batch_id}?confirm=DELETE` — **reverse an
  import.** Reversal is deliberately defensive: `import_batch_id` is origin
  provenance (it records which batch *created* a row and never moves — a
  later merge re-import keeps it pointing at the original batch), so the tag
  alone cannot distinguish an untouched import artifact from state the
  account has since built on. A batch-created row is therefore removed
  **only when it is verifiably untouched**:
  - an **engagement** is removed only if it has never been mutated since the
    import (`updated_at` still equals `created_at`), carries no derived wire
    activity (no first/last outbound, no last inbound, no last conversation,
    no delivered due notification), and has no message history between its
    agent and the address;
  - a **contact** is removed only if it has never been mutated since the
    import, has no message history (as sender or as a To/Cc/Bcc recipient),
    and has **no surviving engagement** — including one created independently
    of the import. Batch-created engagements are deleted first, so any
    engagement still present is live outreach state the reversal must not
    destroy.

  Everything else is retained, and pre-existing outreach and suppressions are
  never affected. The response reports each category;
  `contacts_deleted + contacts_retained` accounts for every batch-created
  contact that still exists at reversal time.

Per-agent outreach state:

- `GET /v1/agents/{email}/contacts` — list the contacts this agent is
  working, with cursor pagination and filters: `stage`, `replied`,
  `suppressed`, `next_action_before`, `last_outbound_before`. For a
  follow-up sweep combine `replied=false`, `next_action_before=<now>`, and
  `last_outbound_before=<stale cutoff>` — `last_outbound_at` is
  server-maintained, so the last filter excludes anyone just contacted even
  when the caller's own state write was lost (the duplicate-send safety net).
- `GET /v1/agents/{email}/contacts/{address}` — one engagement; returns an
  **`ETag`** for `If-Match` on a later upsert.
- `PUT /v1/agents/{email}/contacts/{address}` — enroll a contact in this
  agent's outreach (creates the contact if needed; returns `201` on first
  enrolment, `200` on update) or update the **caller-owned** fields.
  Omitted fields are left unchanged, so advancing the stage after a send
  does not disturb the schedule. An optional `If-Match` makes the write
  conditional (the engagement must already exist and still match, else
  `412`); a conditional request never creates.
- `DELETE /v1/agents/{email}/contacts/{address}?confirm=DELETE` — un-enroll.
  The contact itself survives (identity is account-level and other agents may
  still be working them) and suppressions are untouched — un-enrolling is not
  consent and never restores sendability.

**Caller-owned vs server-derived engagement fields.** The caller owns
`stage` (opaque — there is no server-side state machine, any string is
valid), `next_action_at` (when the caller wants to act next; e2a does not
act on it), and `metadata`. Everything else is **server-owned and derived
from real mail activity** — writes to these fields are rejected:
`replied` (computed as `last_inbound_at > first_outbound_at`, i.e. "replied
to us", not "has ever written"), `first_outbound_at` / `last_outbound_at` /
`last_inbound_at`, `outbound_count` (successfully submitted sends since
enrollment) / `inbound_count` (DMARC-authenticated inbound since enrollment —
spoofed, held, blocked, and pre-enrollment messages are excluded),
`last_conversation_id`, and the suppression mirror (`suppressed`,
`suppression_source`, `suppression_reason` — the same state the send path
enforces).

**`contact.due` behavior.** When an engagement's `next_action_at` passes, a
periodic sweep emits the beta, at-least-once `contact.due` event (see
[events.md](events.md)). It is a **notification, not a send and not an
execution mechanism**: e2a sends no mail and starts no agent. Only a deployed
webhook receiver (or an events-log poller) can react to it and wake an agent
runtime — it does not start an MCP, WebSocket, Claude Code, or Codex session
by itself. To have e2a submit an already-composed message at a future time,
use the separate scheduled-sending capability (`send_at`, above).

**Metadata bounds** (contact and engagement `metadata`, and each import
row's `metadata`): a **flat** JSON object (no nested objects/arrays) with at
most **50 keys**, each key at most **128 bytes**, each value at most
**4 KiB**, and the whole encoded object at most **16 KiB**. `metadata` is
opaque to e2a — never interpreted, only stored and returned. An import row
exceeding the bounds fails on its own without affecting the rest of the
batch. Contact addresses are at most 320 Unicode code points and accept a
bare address or an RFC 5322 mailbox form.

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
once when a new exact-agent block is created. Its current payload is
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
content via `GET /v1/agents/{email}/messages/{id}`. The event's optional
`conversation_id` is application correlation; events and WebSocket
notifications do not expose message-read-only `thread_id`:

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
