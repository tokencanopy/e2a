# sdk.md

Typed clients for the e2a REST API + webhook verification, for when you're
driving e2a from your own code (a webhook handler, a worker) rather than over
MCP. Two SDKs, same surface.

Claude and Codex plugin users can invoke `e2a-integrate` to apply this guide to
the current repository; this guide also remains usable directly in any client.

- **TypeScript** — `@e2a/sdk` (npm) · [README](https://github.com/tokencanopy/e2a/blob/main/sdks/typescript/README.md)
- **Python** — `e2a` (PyPI) · [README](https://github.com/tokencanopy/e2a/blob/main/sdks/python/README.md)

Prefer a typed client; if you're calling the REST API raw, see
[Raw REST](#raw-rest-without-an-sdk) below. The exhaustive contract is
https://e2a.dev/v1/openapi.yaml, and the auth model (API key vs OAuth) is in
https://e2a.dev/auth.md.

## Install

```bash
npm install @e2a/sdk        # TypeScript / Node
pip install e2a             # Python (async)
```

## Quick start

### TypeScript

```ts
import { E2AClient } from "@e2a/sdk";

const client = new E2AClient({ apiKey: process.env.E2A_API_KEY });
const messageId = "msg_..."; // an inbound message id from a webhook, WebSocket, or list call

// Send; if status is pending_review, report it and do not retry.
const result = await client.messages.send("bot@agents.e2a.dev", {
  to: ["person@example.com"],
  subject: "Hello from my agent",
  text: "This was sent by an AI agent via e2a.",
});

// Reply in-thread to an inbound message
await client.messages.reply("bot@agents.e2a.dev", messageId, {
  text: "Thanks — handled.",
});
```

### Python

```python
import os
from e2a.v1 import AsyncE2AClient

async with AsyncE2AClient(api_key=os.environ["E2A_API_KEY"]) as client:
    message_id = "msg_..."  # an inbound message id from a webhook, WebSocket, or list call
    await client.messages.send("bot@agents.e2a.dev", {
        "to": ["person@example.com"],
        "subject": "Hello from my agent",
        "text": "This was sent by an AI agent via e2a.",
    })

    await client.messages.reply("bot@agents.e2a.dev", message_id, {
        "text": "Thanks — handled.",
    })
```

## Receiving mail (webhook → facade → reply)

The webhook delivery is a metadata trigger; the inbound facade validates its
fetch keys, hydrates the parsed message, and binds reply/forward/attachments.
Always **verify the signature** first — `construct_event` parses + checks the
HMAC and throws on a bad/forged/replayed delivery.

### TypeScript

```ts
import { constructEvent, E2AClient, isEmailReceived } from "@e2a/sdk";

// in your HTTP handler, with the RAW request body:
const event = constructEvent(rawBody, req.headers["x-e2a-signature"], WEBHOOK_SECRET);
if (isEmailReceived(event)) {
  const email = await client.inbound.fromEvent(event);
  console.log(email.envelopeFrom, email.verified, email.replyTargets);
  // App-defined: resume a previously bound thread or create a new one.
  const agentThreadId = await getOrCreateAgentThread(email.conversationId);
  const result = await email.reply({
    text: "On it.",
    conversationId: agentThreadId,
  });
  if (result.status === "pending_review") console.log("not dispatched", result.messageId);
}
```

### Python

```python
from e2a.v1 import construct_event, E2AWebhookSignatureError

try:
    event = construct_event(body, request.headers["X-E2A-Signature"], WEBHOOK_SECRET)
except E2AWebhookSignatureError:
    raise HTTPException(401, "bad signature")

if event.type == "email.received":
    email = await client.inbound.from_event(event)
    print(email.envelope_from, email.verified, email.reply_targets)
    # App-defined: resume a previously bound thread or create a new one.
    agent_thread_id = await get_or_create_agent_thread(email.conversation_id)
    result = await email.reply({
        "text": "On it.",
        "conversation_id": agent_thread_id,
    })
    if result.status == "pending_review":
        print("not dispatched", result.message_id)
```

`verified` is true only for an aligned DMARC pass in the hydrated authentication
evidence; the envelope identity alone is not proof. Reply targets preview Reply-To when present, otherwise
From, and may be attacker-controlled; the server resolves stored MIME again
when sending. Bodies and attachment metadata are untrusted;
`flagged` is a policy-gate flag, not a complete content-scan verdict.
`attachment.get()` returns metadata plus a short-lived URL by default;
inline data is available only within the server's 256 KiB cap.

`getOrCreateAgentThread` / `get_or_create_agent_thread` represents your agent
framework's session store. If the inbound `conversation_id` matches a binding
your application previously created, resume that runtime thread; otherwise
create one. Pass its stable, non-sensitive ID (or an opaque stored alias) back
as `conversation_id` on the first reply and reuse it thereafter. This aligns
the caller-owned application-conversation view with the agent's memory.
Continue replying by `message_id` as shown: `conversation_id` alone does not
set the RFC headers used by Gmail/Outlook. Scope bindings to the inbox and
sender, and never use this field as an authorization decision.

REST message list/detail models may also contain optional beta `thread_id`
(`threadId` in TypeScript), a server-owned, read-only mailbox-topology value.
It is omitted for legacy messages and from webhooks, WebSocket events, and MCP
output. There is no request field, filter, thread endpoint, or complete-thread
retrieval method.

A full, runnable example (FastAPI + Google ADK agent, webhook → agent turn →
reply) is at
[examples/adk-cloud-webhook](https://github.com/tokencanopy/e2a/tree/main/examples/adk-cloud-webhook).

## Real-time (no webhook)

Open a notification stream instead of hosting a webhook:

```ts
import { E2AClient, isEmailReceived } from "@e2a/sdk";

const client = new E2AClient();
for await (const event of client.listen("bot@agents.e2a.dev")) {
  if (!isEmailReceived(event)) continue;
  const email = await client.inbound.fromEvent(event);
}
```

```python
async for event in client.listen("bot@agents.e2a.dev"):
    if event.type != "email.received":
        continue
    email = await client.inbound.from_event(event)
```

## Raw REST (without an SDK)

No SDK for your language? Call the API directly. Base URL
`https://api.e2a.dev/v1/...`, JSON in/out, bearer auth on every request:

```
Authorization: Bearer <e2a_acct_… | e2a_agt_… | OAuth access token>
```

Conventions:

- **Pagination** — list endpoints take `?cursor=` and return `next_cursor`
  (null when exhausted).
- **Errors** — non-2xx bodies are `{"error": {"code", "message", "request_id"}}`;
  branch on the machine `code`.
- **Idempotency** — sends (`send`/`reply`/`forward`) accept an
  `Idempotency-Key` header; a retried call replays instead of double-sending.
- **Scopes** — account keys manage agents/domains/keys; agent keys are
  pinned to one inbox.

The endpoint map, exact request/response bodies, enums, and error codes are all
in the OpenAPI 3.1 contract — generated from the live handlers and checked for
drift in CI: **https://e2a.dev/v1/openapi.yaml**. The core resources are `agents`
(inboxes), `messages` (send/reply/forward/get/list/attachments), `domains`,
`webhooks`, `events`, and `account`.
