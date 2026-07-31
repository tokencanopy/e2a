# Events API & reconciliation

e2a maintains a durable log of every event it emits to webhook subscribers — `email.received`, `email.sent`, the review-hold events (`email.review_requested`, `email.review_approved`, `email.review_rejected`), and the screening events (`email.flagged`, `email.blocked`), among others. The log is queryable via `/v1/events` for 30 days and is the source of truth for replay.

This guide is for customers who:

- Want to **reconcile** their webhook state after their receiver was down.
- Want to **replay** an event manually (one-off recovery).
- Want to **bulk-replay** every event a webhook missed during an outage window.

## Quick start

```bash
# List the most recent events for your account.
curl -H "Authorization: Bearer $E2A_API_KEY" \
  https://api.e2a.dev/v1/events

# Filter by event type and time window.
curl -H "Authorization: Bearer $E2A_API_KEY" \
  "https://api.e2a.dev/v1/events?type=email.received&since=2026-06-01T00:00:00Z"

# Fetch one event in detail (includes delivery_status).
curl -H "Authorization: Bearer $E2A_API_KEY" \
  https://api.e2a.dev/v1/events/evt_abc123
```

In TypeScript:

```ts
import { E2AClient } from "@e2a/sdk/v1";
const client = new E2AClient({ apiKey: process.env.E2A_API_KEY });

// Walk the last 24h of email.received events.
const events = await client.events
  .list({
    type: "email.received",
    since: new Date(Date.now() - 24 * 3600 * 1000).toISOString(),
  })
  .toArray({ limit: 100 }); // auto-pages via next_cursor, bounded in memory
for (const e of events) console.log(e.id, e.type, e.createdAt);
```

In Python:

```python
from e2a.v1 import AsyncE2AClient
import os

async with AsyncE2AClient(api_key=os.environ["E2A_API_KEY"]) as client:
    async for e in client.events.list(type="email.received", limit=20):
        print(e.id, e.type, e.created_at)
```

## Event types

| Type | When it fires | Guarantee |
|---|---|---|
| `email.received` | Inbound SMTP message accepted | **At-least-once** end-to-end |
| `email.flagged` | Inbound message accepted but did not match the agent's `inbound_policy` (delivered + flagged, never dropped) | **At-least-once** end-to-end |
| `email.sent` | Outbound `/send` accepted by SES | Best-effort |
| `email.failed` | Outbound send terminally failed — retries exhausted / permanent reject at send time, or the provider rejected the already-accepted message (SES `Reject` delivery feedback, e.g. content scan) — carries `reason` | **At-least-once** from the send path; best-effort when emitted via delivery feedback |
| `email.review_requested` | Message held for human review (outbound HITL or inbound screening) — carries `direction` | **At-least-once** |
| `email.review_approved` | Review approved (outbound: sent; inbound: released to the inbox) | Best-effort |
| `email.review_rejected` | Review rejected (outbound: discarded; inbound: dropped) | **At-least-once** |
| `email.blocked` | Message refused by screening (inbound accept-then-quarantine / outbound 403) | **At-least-once** |
| `email.delivered` | Outbound message accepted for delivery by the recipient's server (per-recipient async outcome) | Best-effort |
| `email.bounced` | Outbound message bounced for a recipient (hard/soft delivery failure) | Best-effort |
| `email.complained` | Recipient marked an outbound message as spam (feedback-loop complaint) | Best-effort |
| `domain.suppression_added` | An address was auto-suppressed after a bounce/complaint (account-scoped despite the `domain.` prefix) | Best-effort |
| `agent.suppression_added` | A recipient was suppressed for one exact sending agent through managed unsubscribe or the management API | Best-effort; **beta** |
| `domain.sending_verified` | A domain's async SES sending identity reached the verified terminal state | Best-effort |
| `domain.sending_failed` | A domain's async SES sending identity reached a failed terminal state | Best-effort |
| `contact.due` | An outreach engagement's `next_action_at` passed, waking an agent that is not running | **At-least-once**; **beta** |

The review-hold + screening events (`email.flagged`, `email.blocked`, `email.review_requested`, `email.review_approved`, `email.review_rejected`), `agent.suppression_added`, and `contact.due` are **beta** — their payloads may change before they are declared stable.

One `email.blocked` asymmetry to know: an **outbound** gate-block refuses the send outright, so no message row exists — its `data.message_id` is a stable rowless soft-ref (`msgblk_…`), the event's top-level `message_id` is absent, and `GET /v1/events?message_id=…` cannot match it (filter by `type` + `agent_email`, or by `conversation_id`, instead). **Inbound** blocks are accept-then-quarantine, reference a real message, and filter normally.

## Lifecycle transitions on events (beta)

**Beta:** message lifecycle and the optional `data.lifecycle_transitions` field
may change before they are declared stable, while existing event envelopes remain stable,
as do existing event names and their non-lifecycle payload fields.

Mapped message events may include `data.lifecycle_transitions`. Every item is
the exact canonical transition row committed for that observation, with the
same ID and fields returned by
`GET /v1/agents/{email}/messages/{id}/lifecycle`. Webhooks, WebSocket frames,
and REST event reads all use the stored envelope, so redelivery does not rebuild
or reinterpret lifecycle data. The field is optional for additive
compatibility: historical envelopes created before the ledger remain valid and
are redelivered byte-for-byte without a fabricated array.

The event-to-reason mapping is:

| Event | Canonical lifecycle reason(s) |
|---|---|
| `email.received` | `acceptance.inbound_smtp` (or `acceptance.local_loopback`); DMARC `pass` → `authentication.dmarc_pass`, DMARC `fail` → `authentication.dmarc_fail`, DMARC `none` → `authentication.dmarc_none`, DMARC `temperror` → `authentication.dmarc_temporary_error`, and DMARC `permerror` → `authentication.dmarc_permanent_error`; plus `queue.inbound_processing` when async intake was durably queued. |
| `email.sent` | `submission.upstream_accepted` or `submission.local_loopback_accepted`. |
| `email.failed` | `submission.provider_rejected`, `submission.local_retries_exhausted`, or `submission.cancelled`, matching the terminal cause. Temporary attempts use `submission.temporary_failure` in the ledger but do not emit a terminal `email.failed` event. |
| `email.delivered` | `delivery.recipient_server_accepted` for `delivered_to`. |
| `email.bounced` | `delivery.permanent_bounce`, `delivery.transient_bounce`, or `delivery.undetermined_bounce` for `delivered_to`. |
| `email.complained` | `complaint.recipient_reported` for `delivered_to`. |
| `email.review_requested` | `review.hold_created`. |
| `email.review_approved` | `review.approved` or `review.expired_approved`. |
| `email.review_rejected` | `review.rejected` or `review.expired_rejected`. |
| `domain.suppression_added` | `suppression.hard_bounce_applied` or `suppression.complaint_applied` for the suppressed address. |

Review and suppression events carry the exact newly committed transition when
their producer has one. If an older or screening-originated review envelope
does not carry the optional array, the lifecycle read may reconstruct only the
fact proven by that durable envelope and marks it `reconstructed: true`.
`agent.suppression_added` is account/agent consent administration rather than a
provider observation for one message, so it has no message-lifecycle mapping.

`email.flagged` and `email.blocked` remain screening events outside the lifecycle ledger;
prompt-injection detections likewise remain in the existing protection-event
contract. They are not converted into lifecycle stages or reason codes, and
their existing documentation above remains in force.

delivered means the recipient mail server accepted the message; e2a does not observe or claim inbox placement.

## Envelope and typed payloads

Webhook deliveries and WebSocket frames use the canonical OpenAPI
`EventEnvelope` component. Its five core fields are required, while the
envelope and `data` objects remain open for additive fields and future event
types. The REST event log uses `EventView`: it contains the same five core
fields and payload, plus REST-only processing/routing fields such as `status`,
`agent_email`, and `delivery_status`. Consumers should not require those
REST-only fields on push deliveries.
(For the WebSocket channel's connection lifecycle — the one-connection-per-agent policy and the frozen close-code
table, including `4000 replaced` — see [api.md → Connection lifecycle & close codes](api.md#connection-lifecycle--close-codes).)

```json
{
  "type": "email.received",
  "id": "evt_62eb7644b075459043c358bc6448d754",
  "schema_version": "1",
  "created_at": "2026-07-01T10:30:00.123456789Z",
  "data": { }
}
```

The server currently emits `schema_version: "1"`. Treat the version as an open
string: a future version and an unknown `type` must still parse into the generic
envelope. SDK stable-event guards narrow `data` only when both the type matches
and `schema_version === "1"`.

Event `conversation_id` values are caller-owned application correlation, not
email-thread identity. The optional beta `thread_id` available on authenticated
message list/detail reads is deliberately absent from stored events, webhook
payloads, and WebSocket notifications.

`data` is deliberately **open at the envelope level** (no discriminator,
`oneOf`, or closed event enum). OpenAPI publishes the non-constraining
`x-e2a-event-data-schemas` map on `EventEnvelope.data`; it maps each stable
event type to the named schema to validate after inspecting `type`. Unknown or
beta event types must still parse. The stable mapping is:

| Event type | `data` schema | Required fields | Optional fields |
|---|---|---|---|
| `email.received` | `EmailReceivedData` | `message_id`, `agent_email`, `direction` (`inbound`), `header_from` (nullable RFC 5322 From), `envelope_from` (nullable SMTP MAIL FROM), `verified_domain` (nullable; non-null only for DMARC pass), `authentication` (nullable for providerless delivery), `to[]`, `cc[]`, `reply_to[]`, `delivered_to` (scalar — the one per-agent copy; the fetch key), `subject`, `received_at` | `conversation_id`, `attachments[]` (metadata only: `filename`, `content_type`, `size_bytes` — the DECODED payload size, `index`) |
| `email.sent` | `EmailSentData` | `message_id`, `agent_email`, `direction` (`outbound`), `method`, `from`, `to[]`, `subject`, `message_type` | `conversation_id`, `cc[]`, `bcc[]`, `provider_message_id` (omitted for providerless local-loopback sends) |
| `email.failed` | `EmailFailedData` | `message_id`, `agent_email`, `direction`, `method`, `from`, `to[]`, `subject`, `message_type`, `reason` | `conversation_id`, `cc[]`, `bcc[]`, `reason_code`, `retryable` (present only when genuinely known) |
| `email.delivered` | `EmailDeliveredData` | `message_id`, `agent_email`, `direction`, `delivered_to` (the one recipient this outcome is about) | `subject`, `smtp_detail` |
| `email.bounced` | `EmailBouncedData` | `EmailDeliveredData` fields + `bounce_type` (`permanent` \| `transient` \| `undetermined`, from the SES bounce classification) | `subject`, `smtp_detail`, `bounce_sub_type` (raw SES sub-type, e.g. `General`) |
| `email.complained` | `EmailComplainedData` | `message_id`, `agent_email`, `direction`, `delivered_to` | `subject`, `smtp_detail` |
| `domain.sending_verified` | `DomainSendingVerifiedData` | `domain`, `sending_status` | — |
| `domain.sending_failed` | `DomainSendingFailedData` | `domain`, `sending_status` | `reason` |
| `domain.suppression_added` | `DomainSuppressionAddedData` | `address`, `source` (open set — known values `bounce`, `complaint`; tolerate unknown values) | `reason`, `message_id` |
| `agent.suppression_added` (**beta**) | `AgentSuppressionAddedData` | `agent_email`, `address`, `source` (`unsubscribe` \| `manual`) | — |
| `contact.due` (**beta**) | — (untyped, built in `internal/contactdue`) | `agent_email`, `address`, `stage`, `next_action_at`, `replied`, `outbound_count`, `contact` (`address`, `display_name`, `metadata`) | `last_outbound_at`, `last_conversation_id` |

Notes:

- For `email.received`, trust the RFC 5322 From domain only when
  `verified_domain` is non-null, equivalently when
  `authentication?.dmarc.status === "pass"`. Only then compare `header_from`
  with an address allowlist. This authenticates domain-authorized use of the
  From domain, not the mailbox local part, a person, or message content. Verify
  a webhook delivery's `X-E2A-Signature` before trusting any payload field;
  WebSocket events arrive over an authenticated transport.
- The delivery-outcome events (`email.delivered`/`bounced`/`complained`) carry **no `status` field** — the event type IS the outcome.
- `email.failed` is **message-level** (its `to`/`cc`/`bcc` lists carry the recipients — never one event per recipient) and fires **at most once per message** across both emission paths: the send worker and the SES `Reject` delivery-feedback path derive the same deterministic event id, so duplicate SNS deliveries and cross-path double emission collapse in the outbox.
- `delivered_to` is always a **scalar**: on `email.received` it's the one per-agent copy (the relay emits one event per delivery); on the delivery-outcome events it's the one recipient the outcome is about. The peer `to`/`cc` lists are the message's parsed headers.
- These shapes are locked by committed golden fixtures (`internal/eventpayload/testdata/`) that the server builders AND both SDKs test against — a payload change is a conscious, reviewed fixture regeneration.
- Every full and minimal stable-event fixture validates twice: once against the
  generic `EventEnvelope`, then against the mapped typed `data` schema. A future
  unknown event/version fixture validates against only the generic envelope.

Every event payload is **metadata only** — it carries identifiers, routing fields, and verdicts, never message content. In particular `email.received` is a notification: it carries the fetch keys plus attachment *metadata* but **not** the body. Fetch the full message — body + attachment bytes — with `client.webhooks.fetch_message(event)` / `webhooks.fetchMessage(event)` (which resolves to `GET /v1/agents/{delivered_to}/messages/{message_id}`). This keeps the fan-out payload bounded and makes the REST resource the single source of truth for content.

"At-least-once" means the event is written to the durable outbox in the same database transaction as the business state, so a process crash between the trigger and webhook fan-out cannot drop the event. "Best-effort" means the outbox write is attempted but a failure logs and continues — used for post-side-effect triggers where the underlying action (SES delivery) has already happened and rolling back would orphan it. See the [design doc](design/2026-06-01-stripe-tier-webhooks.md) §4.2 for the full taxonomy.

## Cursor pagination

`GET /v1/events` returns a `next_cursor` when more pages are available. Pass it back via `?cursor=…` to walk forward in time. Use `since` / `until` (RFC3339) to bracket a specific window — both are optional and stack with the cursor.

```bash
# Get the first page.
RESP=$(curl -s -H "Authorization: Bearer $E2A_API_KEY" \
  "https://api.e2a.dev/v1/events?limit=50")
CURSOR=$(echo "$RESP" | jq -r '.next_cursor')

# Walk forward.
while [ "$CURSOR" != "null" ] && [ -n "$CURSOR" ]; do
  RESP=$(curl -s -H "Authorization: Bearer $E2A_API_KEY" \
    "https://api.e2a.dev/v1/events?limit=50&cursor=$CURSOR")
  # process events…
  CURSOR=$(echo "$RESP" | jq -r '.next_cursor')
done
```

Page size (`limit`) is 1–100 (default 100). Events past the 30-day retention boundary are not returned; querying their id directly returns **410 Gone**.

## Reconciliation pattern

If your webhook receiver went down for an hour, the steps to reconcile are:

1. Pick a `since` timestamp covering the outage (with a buffer).
2. List events filtered to the affected window — `GET /events?since=…&until=…`.
3. Compare event IDs against what your receiver actually processed.
4. For any gaps, use `POST /events/{id}/redeliver` to re-fire one event.

```python
# Pseudocode for the reconciliation flow.
processed = set(load_processed_event_ids_from_my_db())
async with AsyncE2AClient(api_key=os.environ["E2A_API_KEY"]) as client:
    async for e in client.events.list(since=outage_start, until=outage_end):
        if e.id not in processed:
            await client.events.redeliver(e.id, {"webhook_id": "wh_my_handler"})
```

## Replay

### Per-event replay

```bash
# Replay event evt_abc123 to one specific webhook.
curl -X POST -H "Authorization: Bearer $E2A_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"webhook_id": "wh_aaa"}' \
  https://api.e2a.dev/v1/events/evt_abc123/redeliver

# Empty body = replay to every webhook that originally matched.
curl -X POST -H "Authorization: Bearer $E2A_API_KEY" \
  https://api.e2a.dev/v1/events/evt_abc123/redeliver
```

The replay reuses the original event id (`evt_abc123`). Customer-side receivers that dedupe on event id will discard the replay if they've already processed it — by design. **Replay is recovery, not re-delivery.** If you want your handler to run twice for real, you need to call the underlying API again, not replay.

To reconcile a whole window after an outage, walk `GET /v1/events?since=…&until=…` and redeliver each missing event id individually (see the reconciliation flow above).

## Listing, fetching, and replaying events

The event log is driven over the REST API (`GET /v1/events`,
`GET /v1/events/{id}`, `POST /v1/events/{id}/redeliver`) or the MCP tools
(`list_events`, `get_event`, `redeliver_event`) — there is no CLI for events.
The TypeScript / Python SDKs wrap the same endpoints (`client.events.*`).

## Retention and expiry

Events live for **30 days** then drop out of the log. Delivery rows in
`webhook_subscriber_deliveries` carry their own **90-day** TTL, established by
migration 027 to leave a healthy margin beyond the retry envelope. They
therefore become detached from the parent event once it expires —
`GET /events/{id}` returns 410 Gone while the delivery history endpoint
`GET /webhooks/{id}/deliveries` still shows the row until its own retention
window ends.

Replay requires the source event to still exist. If you need a longer reconciliation window, plumb your own copy into your DB at event-receipt time.

## See also

- [API reference — Webhooks](api.md#webhooks-v1webhooks) — subscriber model, signing, retry policy.
- [Design: Stripe-tier webhooks](design/2026-06-01-stripe-tier-webhooks.md) — full rationale for the outbox + event log architecture.
