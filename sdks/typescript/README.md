# e2a TypeScript SDK

TypeScript/Node.js SDK for [e2a](https://e2a.dev) — email for AI agents.

## Install

```bash
npm install @e2a/sdk
```

The SDK major version tracks the SDK package's own breaking changes and is
independent of the API version path (`/v1`): SDK 5.x targets the e2a v1 API.

## Upgrading to 5.2

Inbound sender and authentication fields now use the final DMARC-aligned
contract. `Message`, `MessageView`, `MessageSummaryView`, `ReviewView`, and
`EmailReceivedData` expose the literal RFC 5322 `headerFrom`, SMTP
`envelopeFrom`, and nullable `verifiedDomain`. `Message`, `MessageView`, and
`EmailReceivedData` additionally expose structured `authentication` evidence;
the summary and review views omit it. The former inbound `from`/`from_`
projection is removed from these models; Reply-To remains separate. A non-null
`verifiedDomain` means DMARC passed for that From domain, not that the mailbox
local part, person, or message content was authenticated.

The `InboundEmail` facade returned by `client.inbound.fromEvent(event)` is
unaffected: it keeps `email.from` as the literal RFC 5322 header From
(`MessageView.headerFrom`) alongside `email.envelopeFrom`, `email.verified`,
and `email.authentication`. Reach through `email.message.verifiedDomain` for
the DMARC-passed domain itself.

`authentication` is `null` for outbound messages and providerless loopback
delivery. Guard it before reading `authentication.dmarc`. The outbound-only
`EmailSentData`/`EmailFailedData` webhook payloads and the `listMessages`
sender filter are unaffected and still use `from_` (an OpenAPI Generator
escape artifact for the reserved word `from`) — that request parameter is not
an inbound identity projection.

## Upgrading to 5.1

Every `.delete(...)` now returns a typed deletion object instead of `void`.
The API's delete endpoints all return `200 OK` with
`{deleted: true, <identity key>}` instead of the previous mix of
`204 No Content` and `200`. New return types: `agents.delete` →
`DeleteAgentResult`, `domains.delete` → `DeleteDomainResult`,
`webhooks.delete` → `DeleteWebhookResult`, `templates.delete` →
`DeleteTemplateResult`, `account.apiKeys.delete` → `DeleteApiKeyResult`,
`account.suppressions.delete` → `DeleteSuppressionResult`;
`account.delete()` still returns `DeleteUserDataResult`, which now also
carries `deleted: true`. `deleted` is always `true` — a failed delete throws
a typed error. Callers that ignored the old `void` return need no changes.
Older SDK versions expecting `204` are incompatible with servers running
this contract — upgrade together.

## Upgrading to 4.0

4.0 is a breaking change to the domain DNS-records shape (server #304).
`DomainView.dnsRecords` is now a single purpose-tagged `DNSRecord[]` array
instead of the old `dnsRecords.{ mx, txt, dkim }` object (and the separate
`sendingDnsRecords` array is gone). Each record carries `type`, `name`,
`value`, `priority`, `purpose`, and a per-record `status`. Address records by
`purpose` (`ownership`, `inbound_mx`, `dkim`, `mail_from_mx`, `mail_from_spf`)
rather than `dnsRecords.mx`/`.txt`/`.dkim` — the MAIL FROM records now live in
the same array. `purpose` and `status` are open sets, so tolerate unknown
values. No other public symbols changed.

## Upgrading from 2.x to 3.0

3.0 is a breaking redesign. The SDK now wraps a generated `/v1` client behind a
namespaced, resource-oriented surface, with a typed error hierarchy, automatic
retries + idempotency, and auto-pagination.

- **Namespaced resources.** Flat methods are gone. `client.getMessages()` →
  `client.messages.list(address)`, `client.getMessage(id)` →
  `client.messages.get(address, id)`, `client.send(...)` →
  `client.messages.send(address, body)`, etc. Per-agent calls take an explicit
  `address` — the SDK no longer infers it.
- **Webhook verification.** `client.parse` / `client.parseWebhook` /
  `InboundEmail` were removed. Verify and parse a delivery with the standalone
  `constructEvent(rawBody, header, secret)`, which returns a typed
  `WebhookEvent`. Signatures are per-webhook (`whsec_…`), Stripe-style.
  (5.2 later re-introduced `InboundEmail` as a different thing: the inbound
  facade returned by `client.inbound.fromEvent(event)`.)
- **Typed errors.** Failures throw `E2AError` subclasses
  (`E2ANotFoundError`, `E2AConflictError`, `E2AValidationError`,
  `E2ARateLimitError`, …) carrying `.code`, `.status`, `.requestId`, and
  `.retryable`.

```diff
- const { messages } = await client.getMessages({ status: "unread" });
- const email = await client.getMessage(messages[0].messageId);
- await email.reply("Thanks!");
+ const messages = await client.messages.list(address, { readStatus: "unread" }).toArray({ limit: 50 });
+ await client.messages.reply(address, messages[0].id, { text: "Thanks!" });
```

## Quick Start

For signed-webhook examples that fetch and reply through the ergonomic inbound
facade, see the [minimal Python and TypeScript OpenAI examples with provider
snippets](../../examples/agent-framework-webhooks/README.md).

```typescript
import { E2AClient } from "@e2a/sdk/v1";

const client = new E2AClient(); // reads E2A_API_KEY; baseUrl defaults to https://api.e2a.dev
const address = "my-agent@agents.e2a.dev";
```

### Poll an inbox

```typescript
// List endpoints return an AutoPager: iterate, or collect with a required limit.
for await (const m of client.messages.list(address, { readStatus: "unread" })) {
  const email = await client.messages.get(address, m.id);
  console.log(email.subject, email.parsed?.text);
  await client.messages.reply(address, m.id, { text: "Got it!" });
}
```

### Send mail

```typescript
await client.messages.send(address, {
  to: ["alice@example.com"],
  subject: "Hello",
  text: "Hi from my agent!",
  html: "<p>Hi!</p>",
  cc: ["carol@example.com"],
});
```

Unsafe writes (`messages.send` / `.reply` / `.forward`, `reviews.approve`, and
`webhooks.create`) auto-mint an `Idempotency-Key` and reuse it across retries,
so a network blip can't double-send. Supply a stable key to also survive a
process restart:

```typescript
await client.messages.send(address, body, { idempotencyKey: deriveFromEvent(evt) });
```

Sends are asynchronous by default: the API accepts the message and delivers it
via a background worker. Pass `wait: "sent"` to `messages.send` / `.reply` /
`.forward` to hold the request server-side (up to 20s, currently ~15s) until the message
reaches a terminal-or-held state, then read the observed state from the result
— on timeout the result stays `status: "accepted"`. Always branch on the
result's `status`, not the HTTP code:

```typescript
const res = await client.messages.send(address, body, { wait: "sent" });
if (res.status === "sent") { /* delivered to the relay */ }
```

Scheduled sending is **beta and may change before it is declared stable**.
Schedule a send by passing a `Date`. The durable `scheduled` result is success,
not a reason to retry; even with `wait: "sent"` it returns immediately rather
than holding the HTTP request until the future time:

```typescript
const tomorrow = new Date(Date.now() + 24 * 60 * 60 * 1000);
const res = await client.messages.send(address, {
  to: ["alice@example.com"],
  subject: "Tomorrow's update",
  text: "Hello later",
  sendAt: tomorrow,
}, { wait: "sent" });
if (res.status === "scheduled") console.log(res.scheduledAt);
```

`sendAt` must be no more than 90 days ahead. Direct loopback to the sending
agent's own address cannot be scheduled and returns `400 invalid_request`
unless a review hold takes precedence; held messages drop the schedule and send
when approved.

### Managed unsubscribe (beta)

Opt a single-recipient send, reply, or forward into e2a-managed unsubscribe.
This capability, the agent-scoped suppression management methods, and the raw
`GET|POST /u/{token}` confirmation flow are beta and may change before stable:

```typescript
await client.messages.send("sender@example.com", {
  to: ["recipient@example.net"],
  subject: "Update",
  text: "Hello",
  unsubscribe: { mode: "managed" },
});
```

Omitting `unsubscribe` means only that e2a does not add managed unsubscribe
handling; it does not classify the message as transactional. Managed messages
must have exactly one normalized envelope recipient across To, CC, and BCC.
e2a manages the token and confirmation page, adds a visible footer plus
`List-Unsubscribe` and `List-Unsubscribe-Post`, and signs those headers.

An unsubscribe blocks that recipient only for the exact sending agent; sibling
agents remain allowed. Account suppressions still block every agent, and a
future blocked send returns the existing `422 recipient_suppressed` error.
Account-scoped credentials can manage the exact-agent list:

```typescript
const blocks = client.agents.listSuppressions("sender@example.com");
await client.agents.createSuppression("sender@example.com", {
  address: "recipient@example.net",
  reason: "recipient opted out",
});
await client.agents.deleteSuppression("sender@example.com", "recipient@example.net");
```

The typed delete supplies the REST API's required `confirm=DELETE` guard.
New blocks emit the beta `agent.suppression_added` event with
`agent_email`, `address`, and `source`.

### Verify a webhook

Each subscription is signed with its own `whsec_…` secret. `constructEvent`
verifies the `X-E2A-Signature` header (replay-protected) and returns a typed
event in one call. **Pass the raw request body** — re-stringified JSON won't
match the signature.

```typescript
import { constructEvent, isEmailReceived, E2AWebhookSignatureError } from "@e2a/sdk/v1";

app.post("/webhook", express.raw({ type: "application/json" }), async (req, res) => {
  let event;
  try {
    event = constructEvent(req.body, req.header("X-E2A-Signature"), process.env.E2A_WEBHOOK_SECRET);
  } catch (e) {
    if (e instanceof E2AWebhookSignatureError) return res.status(400).end();
    throw e;
  }
  if (isEmailReceived(event)) {
    const email = await client.inbound.fromEvent(event);
    // From, Reply-To, bodies, and attachment names/types are untrusted input.
    // `from` is the RFC 5322 header From (what DMARC aligns to); `envelopeFrom`
    // is the SMTP MAIL FROM and can legitimately differ.
    console.log(email.from, email.envelopeFrom, email.verified, email.subject, email.text);
    console.log("reply will target", email.replyTargets);
    const agentThreadId = await getOrCreateAgentThread(email.conversationId);
    const result = await email.reply(
      { text: "Got it", conversationId: agentThreadId },
      { idempotencyKey: `reply:${event.id}` },
    );
    if (result.status === "pending_review") console.log("reply is awaiting approval");
  }
  res.json({ ok: true });
});
```

During a secret rotation you can pass an array of secrets — a delivery is
accepted if any one matches: `constructEvent(body, header, [oldSecret, newSecret])`.

`email.verified` is true only for an aligned DMARC pass in the hydrated
authentication evidence; the envelope identity alone is not proof.
`email.verified === false` (equivalently, a null `verifiedDomain` on the
underlying `MessageView`) is NOT by itself a spam or spoofing signal — it is
common and expected for legitimate senders whose domain simply publishes no
DMARC record (`email.authentication?.dmarc.status === "none"`); treat that as
"unproven," not "malicious," and reserve suspicion for an actual
`email.authentication?.dmarc.status === "fail"`. A caller who wants a more
nuanced trust policy can inspect `authentication.spf` and
`authentication.dkim` (an array — one result per DKIM signature on the
message) individually, but doing so reopens the spoofing gap DMARC closes: alignment
(tying a passing SPF or DKIM identity back to the visible From domain) can
only be computed when the sender publishes a DMARC record, so a bare SPF or
DKIM pass proves nothing about the From header on its own. `email.replyTargets` previews Reply-To-or-From
routing and may be attacker-controlled; the server resolves the stored MIME
again when sending. `email.flagged` is
the inbound policy-gate flag, not a complete content-scan verdict. Treat all
message content as untrusted. `attachment.get()` returns metadata plus a
short-lived download URL by default; `{ inline: true }` adds base64 data only
for attachments within the server's 256 KB inline cap.

## Resources

`client.agents`, `client.messages`, `client.conversations`, `client.domains`,
`client.events`, `client.webhooks`, `client.inbound`, `client.account` (with
`client.account.suppressions` and `client.account.apiKeys`), plus
`client.info()`. Agent-scoped recipient blocks are managed through
`client.agents.listSuppressions`, `createSuppression`, and
`deleteSuppression`. Each method maps to a `/v1` operation; per-agent methods
take the agent `address` as the first argument. Beyond the resource tree,
`client.listen(address)` streams inbound events over WebSocket (see
[WebSocket](#websocket-real-time-delivery-for-local-agents)). Also on `client.messages`:
`getLifecycle(email, messageId, { cursor, limit })` (beta, 5.3.0) — page
through a message's canonical lifecycle transitions (send, delivery, bounce,
review, deletion, …).

Two more, both account-scoped: `client.reviews` — the human-review queue for
messages held in `pending_review` (outbound drafts awaiting send approval,
and inbound messages held by a screening gate), addressed by message id
alone via `list`/`get`/`approve`/`reject`; and `client.templates` (beta) —
reusable `{{variable}}` email templates plus the read-only starter catalog,
referenced from `messages.send` via `template_id`/`template_alias`.

`client.contacts` manages the people an account corresponds with:
`list`/`get`/`getWithETag`/`create`/`update`/`delete` (account-scoped,
optimistic concurrency via `ifMatch`), plus `import`/`deleteImport` for
CSV-driven bulk upload. `client.contacts.outreach(address, params)` and its
`get`/`set`/`delete` counterparts track one agent's per-contact engagement
(stage, next action, reply/suppression state) and may be driven by an
agent-scoped credential.

### `new E2AClient(options?)`

| Option         | Type     | Default                 | Description                              |
|----------------|----------|-------------------------|------------------------------------------|
| `apiKey`       | `string` | `E2A_API_KEY` env       | Account (`e2a_acct_`) or agent key/token |
| `baseUrl`      | `string` | `E2A_API_URL` env, else `https://api.e2a.dev` | API base URL (override for self-host)    |
| `maxRetries`   | `number` | `2`                     | Retries on 429/5xx/connection            |
| `maxElapsedMs` | `number` | —                       | Optional total deadline across attempts  |
| `timeoutMs`    | `number` | `30000`                 | Per-attempt request timeout (see below)  |

`baseUrl` names the **API host**, not the deployment root the CLI's `E2A_URL`
points at (that one also serves the dashboard). `E2A_BASE_URL` is this SDK's
former name for `E2A_API_URL` — still read, with a deprecation warning.

`timeoutMs` bounds each individual attempt; a timed-out attempt is treated as a
retryable connection failure, so it composes with `maxRetries`/`maxElapsedMs`.
Setting `timeoutMs: 0` removes the SDK timeout entirely — a request is then
bounded only by the runtime's own `fetch` default (effectively unbounded in
Node). Note this differs from the Python SDK, where `timeout_ms=0` falls back to
the HTTP transport's built-in 300s ceiling rather than going unbounded.

### Errors

Every failure throws an `E2AError` (or subclass) with `.code` (the stable
machine code from the response envelope), `.status`, `.requestId`, and
`.retryable`. Subclasses: `E2AAuthError` (401), `E2APermissionError` (403),
`E2ANotFoundError` (404), `E2AConflictError` (409), `E2AValidationError` (422),
`E2AIdempotencyError`, `E2ALimitExceededError` (402 — a **quota** cap; not
retryable), `E2ARateLimitError` (429 — a request-**rate** limit; retryable after
`retryAfterSeconds`), `E2AServerError` (5xx), `E2AConnectionError` (no response),
`E2AWebhookSignatureError` (local verify failure). The 402/429 split is
permanent — branch on the subclass: 402 → surface a quota/upgrade path, 429 →
back off and retry.

> Note: e2a hides the existence of agents you don't own — `agents.get` of an
> unknown *or* unowned address returns `404` (`E2ANotFoundError`); the two
> cases are deliberately indistinguishable, so a 404 is not proof the agent
> doesn't exist. `E2APermissionError` (403) means something else: an
> *agent-scoped* credential tried to act on a different agent in the account.

### Pagination

List methods return an `AutoPager<T>` — an `AsyncIterable` that threads the
cursor for you. Use `for await`, or `.toArray({ limit })` (the limit is
required, to bound memory on a large inbox), or `.forEach(fn)` (return `false`
to stop early).

For manual, caller-driven pagination (e.g. checkpoint/resume from a queue), use
`.page(cursor)`: it fetches a SINGLE page and returns a `{ items, next_cursor }`
object. Omit the cursor for the first page and pass the previous page's
`next_cursor` to resume; a `null`/`undefined`/empty `next_cursor` means there
are no more pages.

```ts
const page = await client.messages.list("bot@agents.e2a.dev", { limit: 100 }).page();
process(page.items);
checkpoint(page.next_cursor); // resume later with .page(savedCursor)
```

## WebSocket (real-time delivery for local agents)

Agents receive lightweight notifications over a WebSocket; auth is the
`Authorization: Bearer <api_key>` handshake header (the key never appears in the
URL) — no public URL needed.

```typescript
import { E2AClient, isEmailReceived } from "@e2a/sdk/v1";

const client = new E2AClient({ apiKey: "e2a_..." });

for await (const event of client.listen("bot@agents.e2a.dev")) {
  if (!isEmailReceived(event)) continue; // tolerate future event kinds
  const email = await client.inbound.fromEvent(event);
  console.log(email.from, email.envelopeFrom, email.verified, email.subject, email.text);
}
```

`client.listen(address)` returns a
`WSStream` that is **both** an `AsyncIterable<WSEvent>` and an
`EventEmitter` — each item is the same versioned `{type, id, schema_version,
created_at, data}` envelope a webhook delivery carries, so
`client.inbound.fromEvent(event)` works on either channel and returns the
bound facade. The lower-level `client.webhooks.fetchMessage(event)` still
returns the raw generated `MessageView`. Use
`.on("error" | "close", …)` for connection-level events and `.close()` to
stop. Reconnects with exponential backoff (1s → 30s, configurable via
`maxBackoffMs`) on transient closes. The server keeps **one connection per
agent**: if a newer connection for the same agent takes over, the stream
stops with `E2AConnectionReplacedError` (WS close code `4000 "replaced"`)
instead of reconnecting — reconnecting would steal the socket back and loop.
The lower-level `WSListener` is also exported for advanced use.

## Trash and restore

`delete()` is a soft delete: agents and messages move to the trash and stay
restorable for about 30 days. List the trash with `deleted: true`, then restore
an item through the same resource:

```ts
await client.agents.delete("bot@agents.e2a.dev");
const trashedAgents = client.agents.list({ deleted: true });
await client.agents.restore("bot@agents.e2a.dev");

await client.messages.delete("bot@agents.e2a.dev", "msg_abc123");
const trashedMessages = client.messages.list("bot@agents.e2a.dev", { deleted: true });
await client.messages.restore("bot@agents.e2a.dev", "msg_abc123");
```

A message already in the trash can be purged early and irreversibly. That path
needs an account-scoped credential; the SDK supplies the API's `?confirm=DELETE`
guard for you:

```ts
await client.messages.delete("bot@agents.e2a.dev", "msg_abc123", { permanent: true });
```

Agents support the same escape hatch — `{ permanent: true }` deletes
irreversibly right away instead of moving to the trash (accepts live and
trashed agents):

```ts
await client.agents.delete("bot@agents.e2a.dev", { permanent: true });
```

## Conversation threading

`conversationId` is an opaque string that ties multiple emails to one thread
across the email boundary. Pass it on any `send` / `reply` and e2a surfaces it
on the recipient's inbound — via `In-Reply-To` for humans, or a forge-resistant
`X-E2A-Conversation-Id` header for same-platform agent-to-agent mail. It is not
a security boundary; for sender identity require
`message.authentication?.dmarc.status === "pass"` and compare the literal
`message.headerFrom` address separately. On first
contact from a human it arrives `null`. Create the agent runtime's internal
thread before replying, then pass its stable, non-sensitive thread/session ID
(or an opaque stored alias) as `conversationId`; reuse it on every later send
or reply. If a later inbound ID matches a binding you previously stored, resume
that internal thread. Keep replying by the original message ID as well — the
conversation ID aligns e2a grouping with agent memory, while the reply endpoint
sets the email headers Gmail/Outlook use. Scope bindings to the inbox and sender,
and never use the conversation ID as authorization.

## License

Apache-2.0 — see [LICENSE](https://github.com/tokencanopy/e2a/blob/main/LICENSE) and [NOTICE](https://github.com/tokencanopy/e2a/blob/main/NOTICE) in the upstream repo.
