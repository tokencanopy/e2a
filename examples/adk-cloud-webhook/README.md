# ADK + e2a: an email inbox for a Google ADK agent

A minimal end-to-end example: a Google [Agent Development Kit](https://adk.dev/)
agent receives email through an [e2a](https://e2a.dev) webhook subscription,
runs a turn, and replies — keeping application conversation memory by
mapping e2a's `conversation_id` to ADK's `session_id`.

```
human (Gmail)
    |
    v  SMTP
e2a relay
    |
    v  HTTPS POST /webhook (signed)
this app
    |
    +-- construct_event (verify HMAC + decode to a typed event)
    +-- client.inbound.from_event(event) (fetch normalized InboundEmail)
    +-- look up / create ADK session keyed by conversation_id
    +-- runner.run_async(...)
    +-- email.reply({text, conversation_id}, idempotency_key=event.id)
    |
    v
e2a relay -> SMTP -> human
```

## Prerequisites

- Python 3.10+
- An e2a account with a registered agent. Sign up at
  [e2a.dev](https://e2a.dev) and create an agent. The running app should use an
  **agent-scoped API key** bound to that inbox for fetch + reply. Creating the
  webhook subscription is an account-admin operation and requires a separate
  **account-scoped API key**. The webhook's **HMAC signing secret** is returned
  once by `POST /v1/webhooks` — copy it immediately; rotate it later with
  `POST /v1/webhooks/{id}/rotate-secret`.
- A Google AI Studio API key for Gemini —
  [aistudio.google.com/apikey](https://aistudio.google.com/apikey).

> **Version lines:** this example pins ADK 1.x (`google-adk>=1.31,<2`) and
> requires the ergonomic inbound facade in e2a 5.2 or newer. The MCP
> quick-start example under [`mcp/examples/adk`](../../mcp/examples/adk/)
> targets ADK 2.x. They intentionally track different ADK releases.

## Setup

```bash
cd examples/adk-cloud-webhook
python -m venv .venv && source .venv/bin/activate
pip install -e .
cp .env.example .env
# edit .env with your three secrets
```

## Run the webhook

```bash
uvicorn webhook:app --host 0.0.0.0 --port 18080 --reload
```

Health check:

```bash
curl http://localhost:18080/health
# {"status":"ok"}
```

## Exposing the webhook

The webhook needs a public HTTPS URL e2a can POST to. For local
development, [ngrok](https://ngrok.com/) or
[cloudflared](https://github.com/cloudflare/cloudflared) work well:

```bash
ngrok http 18080
# Forwarding  https://abc123.ngrok.io -> http://localhost:18080
```

Subscribe that public URL to your agent's inbound mail. Webhooks are a separate
resource (`/v1/webhooks`) — pass the agent's address as a filter. This setup
call requires an account-scoped key; the app itself can keep using its narrower
agent-scoped key. Replace `<YOUR_AGENT_EMAIL>` and `<YOUR_ACCOUNT_API_KEY>`:

```bash
curl -X POST https://api.e2a.dev/v1/webhooks \
  -H "Authorization: Bearer <YOUR_ACCOUNT_API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://abc123.ngrok.io/webhook",
    "events": ["email.received"],
    "filters": {"agent_emails": ["<YOUR_AGENT_EMAIL>"]}
  }'
```

## Try it

Send a plain email from any address to your agent's e2a email. Watch the
uvicorn logs — you'll see the inbound POST, ADK pick a session, the agent
generate a reply, and the reply post back to e2a. Reply to the agent's
message from your inbox and you'll see ADK reuse the same session — the
agent remembers the prior turn.

## How `conversation_id` keeps application memory across turns

`conversation_id` is caller-owned application correlation, not an RFC email
thread key. This example propagates it through each round-trip solely to bind
mail to one ADK session:

1. **First inbound** has `conversation_id = None` (the human just
   started a thread). The webhook derives a stable `conv_<full-event-id-suffix>`
   anchor and uses it as the ADK `session_id`. A retry derives the same value,
   so it also reuses an identical reply request body and idempotency key.
2. The webhook hydrates an `AsyncInboundEmail` with
   `client.inbound.from_event(event)`, then calls
   `email.reply({"text": text, "conversation_id": "conv_<full-event-id-suffix>"},
   idempotency_key=event.id)`. e2a stamps
   that value into `X-E2A-Conversation-Id` on the outbound message and
   remembers the binding to the message ID.
3. **Subsequent inbound** (the human replies in their mail client) lands
   with the same `conversation_id` set — recovered from `In-Reply-To` /
   `References` lookup. The webhook calls `get_session` with that ID and
   gets back the existing ADK session, so the agent sees full prior
   context on the next `runner.run_async` call.

The actual email thread is preserved because `email.reply(...)` references the
original e2a message and emits RFC `In-Reply-To` / `References` headers. A
fresh send with the same `conversation_id` would still start a separate email
thread.

Authenticated message list/detail reads may expose optional beta `thread_id`,
the server-owned mailbox-local topology identity. Webhook events deliberately
omit it, so this example neither reads nor writes it. There is no `thread_id`
request field, filter, or thread endpoint.

This is the entire application-memory trick. The webhook is ~30 lines of
business logic; ADK does the memory work, while e2a handles email and reply
topology.

e2a delivers webhooks at least once. The example claims each event ID before
running ADK and reuses that ID as the reply's `Idempotency-Key`, so a timed-out
delivery cannot send the same reply twice in the single-process demo. A
duplicate that is still running receives a retryable 503; a completed duplicate
is acknowledged without another agent turn. The endpoint streams the exact
signed request bytes through a 1 MiB limit before signature verification, so a
chunked request cannot bypass the bound or allocate an unbounded body.

See [webhook.py](webhook.py) for the implementation, including
[HMAC signature verification](https://github.com/tokencanopy/e2a/blob/main/sdks/python/README.md#verify-a-webhook)
which you should *always* do on a public webhook before trusting any
field on the parsed payload.

`construct_event()` is the envelope authentication boundary: it verifies the
raw body and signature before any fetch, model call, or reply. Only after that
succeeds does `client.inbound.from_event(event)` fetch the message and expose
normalized fields such as `email.from_`, `email.subject`, `email.text`, and
`email.verified`. The last field is the message's DMARC result, not webhook
signature status. Sender-controlled address, subject, and body values remain
untrusted model input even when the webhook signature is valid. This example
does not place the full `MessageView` or raw MIME in the prompt or logs.

ADK's `user_id` is a truncated SHA-256 identifier derived from the canonical
sender mailbox and the receiving e2a inbox. Display-name and address-case
variants therefore reuse the same history, the same sender remains isolated
across different agent inboxes, and session storage does not receive the
sender's literal address. Unparseable senders receive a message-scoped private
identifier instead.

For shorter runnable OpenAI webhook references in Python and TypeScript, plus
compact Anthropic, LangChain, and ADK substitution snippets, see the
[minimal agent webhook examples](../agent-framework-webhooks/README.md).

## What this example deliberately doesn't show

- **Durable sessions + event deduplication.** `InMemorySessionService` and the
  example's `EventDeduper` both lose state on restart and live in one process.
  Running `uvicorn ... --workers 4` shards both, so a conversation or duplicate
  delivery can land on different workers. For production, use
  `DatabaseSessionService` (Postgres / SQLite) and replace `EventDeduper` with a
  durable unique claim keyed by `event.id`; keep the event-derived reply
  idempotency key. See [ADK sessions docs](https://adk.dev/sessions/session/).
- **Tools.** The agent has no tools beyond text generation. Add them to
  `agent.py` once the email loop works end-to-end.
- **Attachments.** The verified event carries attachment metadata only. The
  `AsyncInboundEmail` hydrated with `client.inbound.from_event(event)` exposes
  parsed body and attachment wrappers; call `await attachment.get()` to fetch
  attachment bytes only when needed. This example ignores them. ADK's
  `Content` supports inline data parts if you want to feed attachments to a
  vision model.
- **HITL.** The agent replies immediately. If you want human approval
  before the reply goes out, enable review holds on the agent's
  protection config (`PUT /v1/agents/{email}/protection`,
  [docs](https://github.com/tokencanopy/e2a/blob/main/README.md)) and the
  `email.reply(...)` call returns `status: "pending_review"`
  with the reply held for review.
