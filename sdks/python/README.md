# e2a Python SDK

Python SDK for [e2a](https://e2a.dev) — email for AI agents. Ships both a
synchronous client (`E2AClient`) and an async one (`AsyncE2AClient`) with an
identical surface.

## Install

```bash
pip install e2a          # add the [ws] extra for client.listen(): pip install "e2a[ws]"
```

The SDK major version tracks the SDK package's own breaking changes and is
independent of the API version path (`/v1`): SDK 5.x targets the e2a v1 API.

## Upgrading to 5.2

Inbound sender and authentication fields now use the final DMARC-aligned
contract. Message, summary, review, webhook, and WebSocket payloads expose the
literal RFC 5322 `header_from`, SMTP `envelope_from`, nullable
`verified_domain`, and structured `authentication` evidence. The former
inbound `from`/`from_` projection is removed; Reply-To remains separate. A
non-null `verified_domain` means DMARC passed for that From domain, not that the
mailbox local part, person, or message content was authenticated.

`authentication` is `None` for outbound messages and providerless loopback
delivery. Guard it before reading `authentication.dmarc`. The sender filter is
still passed as `messages.list(from_=...)`; that request parameter is not an
inbound identity projection.

## Upgrading to 5.1

Every `.delete(...)` now returns a typed deletion object instead of `None`.
The API's seven delete endpoints all return `200 OK` with
`{"deleted": true, <identity key>}` instead of the previous mix of
`204 No Content` and `200`. New return types: `agents.delete` →
`DeleteAgentResult`, `domains.delete` → `DeleteDomainResult`,
`webhooks.delete` → `DeleteWebhookResult`, `templates.delete` →
`DeleteTemplateResult`, `account.api_keys.delete` → `DeleteApiKeyResult`,
`account.suppressions.delete` → `DeleteSuppressionResult`;
`account.delete()` still returns `DeleteUserDataResult`, which now also
carries `deleted: true`. `deleted` is always `True` — a failed delete raises
a typed error. Applies identically to the sync `E2AClient` facade. Callers
that ignored the old `None` return need no changes. Older SDK versions
expecting `204` are incompatible with servers running this contract —
upgrade together.

## Upgrading to 5.0

5.0 renames the async client and introduces a synchronous client under the
freed name:

- **`E2AClient` → `AsyncE2AClient`** (the 4.x client, unchanged in behavior).
  If you're upgrading from 4.x, the one mechanical change is:

  ```python
  from e2a.v1 import AsyncE2AClient   # was: from e2a.v1 import E2AClient
  ```

- **`E2AClient` is now the synchronous client.** Python convention (httpx,
  openai, anthropic) is plain name = sync client, `Async*` = async client; the
  rename freed the plain name for exactly this. The sync client is a facade
  over `AsyncE2AClient` — same constructor, resources, typed errors,
  retry/idempotency behavior, and pagination semantics, bridged through a
  background event loop, so the two cannot drift.

  ⚠️ 4.x code that still says `E2AClient` will now import successfully but get
  the **sync** client — `await client.messages.send(...)` no longer works on
  it. Calling any sync method from inside a running event loop raises a guiding
  `RuntimeError` ("use AsyncE2AClient"), so the misuse is caught immediately
  rather than deadlocking.

## Upgrading to 4.0

4.0 is a breaking change to the domain DNS-records shape (server #304).
`DomainView.dns_records` is now a single purpose-tagged `list[DNSRecord]`
instead of the old `dns_records.{ mx, txt, dkim }` object (and the separate
`sending_dns_records` list is gone). Each record carries `type`, `name`,
`value`, `priority`, `purpose`, and a per-record `status`. Address records by
`purpose` (`ownership`, `inbound_mx`, `dkim`, `mail_from_mx`, `mail_from_spf`)
rather than `dns_records.mx`/`.txt`/`.dkim` — the MAIL FROM records now live in
the same list. `purpose` and `status` are open sets, so tolerate unknown
values. No other public symbols changed.

## Upgrading from 2.x to 3.0

3.0 is a breaking redesign. The SDK now wraps a generated `/v1` client behind a
namespaced, **async-only** surface, with a typed error hierarchy, automatic
retries + idempotency, and async auto-pagination.

- **Async-only, namespaced.** The sync client and the flat methods are gone.
  `client.get_messages()` → `client.messages.list(address)`,
  `client.get_message(id)` → `client.messages.get(address, id)`,
  `client.send(...)` → `client.messages.send(address, body)`. Per-agent calls
  take an explicit `address`.
- **Webhook verification.** `client.parse` / `client.parse_webhook` /
  `InboundEmail` were removed. Verify and parse a delivery with the standalone
  `construct_event(raw_body, header, secret)`, which returns a typed
  `WebhookEvent`. Signatures are per-webhook (`whsec_…`), Stripe-style.
  (5.2 later re-introduced `InboundEmail` as a different thing: the sync
  inbound facade returned by `client.inbound.from_event(event)`.)
- **Typed errors.** Failures raise `E2AError` subclasses (`E2ANotFoundError`,
  `E2AConflictError`, `E2AValidationError`, `E2ARateLimitError`, …) carrying
  `.code`, `.status`, `.request_id`, and `.retryable`.

## Quick Start

For signed-webhook examples that fetch and reply through the ergonomic inbound
facade, see the [minimal Python and TypeScript OpenAI examples with provider
snippets](../../examples/agent-framework-webhooks/README.md).

### Synchronous

```python
from e2a.v1 import E2AClient

# reads E2A_API_KEY; base_url defaults to https://api.e2a.dev
with E2AClient() as client:
    address = "my-agent@agents.e2a.dev"

    # List endpoints return a sync pager: iterate, or collect with a limit.
    for m in client.messages.list(address, read_status="unread"):
        email = client.messages.get(address, m.id)
        print(email.subject)
        client.messages.reply(address, m.id, {"text": "Got it!"})
```

The sync client must not be used from async code — any call made while an
event loop is running in the current thread raises `RuntimeError` pointing you
at `AsyncE2AClient`.

### Async

```python
import asyncio
from e2a.v1 import AsyncE2AClient

async def main():
    # reads E2A_API_KEY; base_url defaults to https://api.e2a.dev
    async with AsyncE2AClient() as client:
        address = "my-agent@agents.e2a.dev"

        # List endpoints return an AutoPager: async-iterate, or collect with a limit.
        async for m in client.messages.list(address, read_status="unread"):
            email = await client.messages.get(address, m.id)
            print(email.subject)
            await client.messages.reply(address, m.id, {"text": "Got it!"})

asyncio.run(main())
```

### Send mail

```python
await client.messages.send(address, {
    "to": ["alice@example.com"],
    "subject": "Hello",
    "text": "Hi from my agent!",
    "html": "<p>Hi!</p>",
})
```

The mail-sending writes (`send` / `reply` / `forward` / `approve`) auto-mint an
`Idempotency-Key` and reuse it across retries, so a network blip can't
double-send. Pass a stable key to also survive a process restart:

```python
await client.messages.send(address, body, idempotency_key=derive_from(event))
```

Request bodies accept a plain `dict` (shown above) or the generated model
(`from e2a.v1 import SendEmailRequest`).

### Bounded wait for delivery (`wait="sent"`)

Sends are queue-first: by default `send` / `reply` / `forward` return as soon
as the message is durably accepted (`status="accepted"`). Pass `wait="sent"`
to hold the request server-side until the message reaches a terminal-or-held
state or at most 20 seconds elapse (currently ~15s):

```python
result = await client.messages.send(
    "sender@example.com",
    {"to": ["recipient@example.net"], "subject": "Update", "text": "Hello"},
    wait="sent",
)
if result.status == "sent":
    ...  # delivered; on timeout the status stays "accepted"
```

Always branch on the result's `status`, not the HTTP code — a timeout is not
a failure, the message is still queued for delivery.

Schedule a send with a timezone-aware `datetime`. The durable `scheduled`
result is success, not a reason to retry; even with `wait="sent"` it returns
immediately rather than holding the HTTP request until the future time:

```python
from datetime import datetime, timedelta, timezone

result = await client.messages.send(
    "sender@example.com",
    {
        "to": ["recipient@example.net"],
        "subject": "Tomorrow's update",
        "text": "Hello later",
        "send_at": datetime.now(timezone.utc) + timedelta(days=1),
    },
    wait="sent",
)
if result.status == "scheduled":
    print(result.scheduled_at)
```

`send_at` must be no more than 90 days ahead. Direct loopback to the sending
agent's own address cannot be scheduled and returns `400 invalid_request`
unless a review hold takes precedence; held messages drop the schedule and send
when approved.

### Managed unsubscribe (beta)

Opt a single-recipient send, reply, or forward into e2a-managed unsubscribe.
This capability, the agent-scoped suppression management methods, and the raw
`GET|POST /u/{token}` confirmation flow are beta and may change before stable:

```python
await client.messages.send("sender@example.com", {
    "to": ["recipient@example.net"],
    "subject": "Update",
    "text": "Hello",
    "unsubscribe": {"mode": "managed"},
})
```

Equivalently, pass it as the `unsubscribe=` keyword on `send` / `reply` /
`forward` (a plain dict or an `UnsubscribeOptions`; the keyword wins over any
`unsubscribe` already in the body):

```python
await client.messages.send(
    "sender@example.com",
    {"to": ["recipient@example.net"], "subject": "Update", "text": "Hello"},
    unsubscribe={"mode": "managed"},
)
```

Omitting `unsubscribe` means only that e2a does not add managed unsubscribe
handling; it does not classify the message as transactional. Managed messages
must have exactly one normalized envelope recipient across To, CC, and BCC.
e2a manages the token and confirmation page, adds a visible footer plus
`List-Unsubscribe` and `List-Unsubscribe-Post`, and signs those headers.

An unsubscribe blocks that recipient only for the exact sending agent; sibling
agents remain allowed. Account suppressions still block every agent, and a
future blocked send raises the existing `422 recipient_suppressed` error.
Account-scoped credentials can manage the exact-agent list:

```python
blocks = client.agents.list_suppressions("sender@example.com")
await client.agents.create_suppression(
    "sender@example.com",
    {"address": "recipient@example.net", "reason": "recipient opted out"},
)
await client.agents.delete_suppression(
    "sender@example.com", "recipient@example.net"
)
```

The typed delete supplies the REST API's required `confirm=DELETE` guard.
New blocks emit the beta `agent.suppression_added` event with
`agent_email`, `address`, and `source`.

### Verify a webhook

Each subscription is signed with its own `whsec_…` secret. `construct_event`
verifies the `X-E2A-Signature` header (replay-protected) and returns a typed
event. **Pass the raw request body** — re-serialized JSON won't match.

```python
from e2a.v1 import construct_event, is_email_received, E2AWebhookSignatureError

@app.post("/webhook")
async def webhook(request):
    try:
        event = construct_event(await request.body(), request.headers["X-E2A-Signature"], SECRET)
    except E2AWebhookSignatureError:
        return Response(status_code=400)
    if is_email_received(event):
        email = await client.inbound.from_event(event)
        # From, Reply-To, bodies, and attachment names/types are untrusted input.
        print(email.envelope_from, email.verified, email.subject, email.text)
        print("reply will target", email.reply_targets)
        agent_thread_id = await get_or_create_agent_thread(email.conversation_id)
        result = await email.reply(
            {"text": "Got it", "conversation_id": agent_thread_id},
            idempotency_key=f"reply:{event.id}",
        )
        if result.status == "pending_review":
            print("reply is awaiting approval")
    return {"ok": True}
```

During a rotation you can pass a list of secrets — accepted if any matches:
`construct_event(body, header, [old_secret, new_secret])`.

`email.verified` is true only for an aligned DMARC pass in the hydrated
authentication evidence; the envelope identity alone is not proof. `email.verified is False` /
`verified_domain is None` is NOT by itself a spam or spoofing signal — it is
common and expected for legitimate senders whose domain simply publishes no
DMARC record (`authentication.dmarc.status == "none"`); treat that as
"unproven," not "malicious," and reserve suspicion for an actual
`authentication.dmarc.status == "fail"`. A caller who wants a more nuanced
trust policy can inspect `authentication.spf`/`authentication.dkim`
individually, but doing so reopens the spoofing gap DMARC closes: alignment
(tying a passing SPF or DKIM identity back to the visible From domain) can
only be computed when the sender publishes a DMARC record, so a bare SPF or
DKIM pass proves nothing about the From header on its own. `email.reply_targets` previews Reply-To-or-From
routing and may be attacker-controlled; the server resolves the stored MIME
again when sending. `email.flagged` is
the inbound policy-gate flag, not a complete content-scan verdict. Treat all
message content as untrusted. `attachment.get()` returns metadata plus a
short-lived download URL by default; `inline=True` adds base64 data only for
attachments within the server's 256 KB inline cap.

## Resources

`client.agents`, `client.messages`, `client.conversations`, `client.domains`,
`client.events`, `client.webhooks`, `client.inbound`, `client.account` (with
`client.account.suppressions` and `client.account.api_keys`), plus
`await client.info()`. Agent-scoped recipient blocks are managed through
`client.agents.list_suppressions`, `create_suppression`, and
`delete_suppression`. Each method maps to a `/v1` operation; per-agent
methods take the agent `address` first. Also on `client.messages`:
`get_lifecycle(email, message_id, *, cursor=None, limit=None)` (beta) —
page through a message's canonical lifecycle transitions (send, delivery,
bounce, review, deletion, …).

Two more, both account-scoped: `client.reviews` — the human-review queue for
messages held in `pending_review` (outbound drafts awaiting send approval,
and inbound messages held by a screening gate), addressed by message id
alone via `list`/`get`/`approve`/`reject`; and `client.templates` (beta) —
reusable `{{variable}}` email templates plus the read-only starter catalog,
referenced from `messages.send` via `template_id`/`template_alias`.

The sync `E2AClient` exposes the **same resource tree** — drop the `await`.
It mirrors the async client dynamically (every async method is bridged, not
re-implemented), so the two surfaces are identical by construction.

### `AsyncE2AClient(api_key=None, *, base_url=None, max_retries=2, max_elapsed_ms=None, timeout_ms=30000)`

`E2AClient` (sync) takes exactly the same arguments.

`api_key` falls back to `E2A_API_KEY`; `base_url` to `E2A_API_URL` then
`https://api.e2a.dev`. (`E2A_BASE_URL` is the SDK's former name for
`E2A_API_URL` — still read, with a `DeprecationWarning`.) `timeout_ms` is the per-request timeout (default 30s); a
timed-out request retries like any other connection failure. Passing
`timeout_ms=0` or `None` removes the SDK's override and falls back to the HTTP
transport's built-in **300s** ceiling — it does **not** make requests unbounded
(this differs from the TypeScript SDK, where `timeoutMs: 0` is fully unbounded).
Use the async client as an async context manager (or call
`await client.aclose()`) and the sync client as a plain context manager (or
call `client.close()`) to close the underlying HTTP connections — for the sync
client this also stops its background event-loop thread. An unclosed sync
client cleans itself up at garbage collection / interpreter exit and never
hangs shutdown, but closing explicitly is preferred.

### Errors

Every failure raises an `E2AError` (or subclass) with `.code`, `.status`,
`.request_id`, `.retryable`: `E2AAuthError` (401), `E2APermissionError` (403),
`E2ANotFoundError` (404), `E2AConflictError` (409), `E2AValidationError` (422),
`E2AIdempotencyError`, `E2ALimitExceededError` (402 — a **quota** cap; not
retryable), `E2ARateLimitError` (429 — a request-**rate** limit; retryable after
`retry_after_seconds`), `E2AServerError` (5xx), `E2AConnectionError` (no
response), `E2AConnectionReplacedError` (WebSocket close code 4000 — a newer
listener for the same agent superseded this one; terminal, no auto-reconnect),
`E2AWebhookSignatureError`. The 402/429 split is permanent — branch on
the exception type: 402 → surface a quota/upgrade path, 429 → back off and retry.

> e2a hides the existence of agents you don't own — `agents.get` of an
> unknown *or* unowned address raises `E2ANotFoundError` (404); the two cases
> are deliberately indistinguishable, so a 404 is not proof the agent doesn't
> exist. `E2APermissionError` (403) means something else: an *agent-scoped*
> credential tried to act on a different agent in the account.

### Pagination

List methods return an `AutoPager` — async-iterate it, or use
`await pager.to_list(limit=N)` (the limit is required, to bound memory) or
`await pager.for_each(fn)` (return `False` to stop early).

For manual, caller-driven pagination (e.g. checkpoint/resume from a queue), use
`await pager.page(cursor)`: it fetches a SINGLE page and returns a `Page` of
`items` + `next_cursor`. Omit the cursor for the first page and pass the
previous page's `next_cursor` to resume; a `None` `next_cursor` means there are
no more pages.

```python
page = await client.messages.list("bot@agents.e2a.dev", limit=100).page()
process(page.items)
checkpoint(page.next_cursor)  # resume later with .page(saved_cursor)
```

On the sync client the same list methods return a **sync** pager: iterate it
with a plain `for`, and `page(cursor)` / `to_list(limit=N)` / `for_each(fn)`
are ordinary blocking calls with the same semantics.

```python
for m in client.messages.list("bot@agents.e2a.dev"):   # sync iteration
    ...
page = client.messages.list("bot@agents.e2a.dev", limit=100).page()
```

### Trash and restore

`delete()` is a soft delete: agents and messages move to the trash and stay
restorable for about 30 days. List the trash with `deleted=True`, then restore
an item through the same resource. The sync client exposes the same methods
without `await`.

```python
await client.agents.delete("bot@agents.e2a.dev")
trashed_agents = client.agents.list(deleted=True)
await client.agents.restore("bot@agents.e2a.dev")

await client.messages.delete("bot@agents.e2a.dev", "msg_abc123")
trashed_messages = client.messages.list("bot@agents.e2a.dev", deleted=True)
await client.messages.restore("bot@agents.e2a.dev", "msg_abc123")
```

A message already in the trash can be purged early and irreversibly. That path
needs an account-scoped credential; the SDK supplies the API's `?confirm=DELETE`
guard for you:

```python
await client.messages.delete("bot@agents.e2a.dev", "msg_abc123", permanent=True)
```

## WebSocket (real-time delivery for local agents)

```python
async for event in client.listen("bot@agents.e2a.dev"):
    if event.type != "email.received":
        continue  # tolerate future event kinds
    email = await client.inbound.from_event(event)
    print(email.envelope_from, email.verified, email.subject, email.text)
```

`client.listen(address)` returns a `WSStream` (async-iterable of `WSEvent` —
the same versioned `{type, id, schema_version, created_at, data}` envelope a
webhook delivery carries) that reconnects with exponential backoff on
transient closes. The server keeps **one connection per agent**: if a newer
connection for the same agent takes over, iteration raises
`E2AConnectionReplacedError` (WS close code `4000 "replaced"`) instead of
reconnecting — reconnecting would steal the socket back and loop. Requires
the `[ws]` extra (`pip install "e2a[ws]"`).

On the sync client, `client.listen(address)` returns a plain iterable instead:

```python
for event in client.listen("bot@agents.e2a.dev"):
    if event.type != "email.received":
        continue  # tolerate future event kinds
    email = client.inbound.from_event(event)
    print(email.envelope_from, email.verified, email.subject, email.text)
```

Calling `client.close()` from another thread unblocks a pending iteration and
ends the loop cleanly.

## Conversation threading

`conversation_id` is an opaque string that ties multiple emails to one thread
across the email boundary. Pass it on any `send` / `reply` (as a body field) and
e2a surfaces it on the recipient's inbound — via `In-Reply-To` for humans, or a
forge-resistant `X-E2A-Conversation-Id` header for same-platform agent-to-agent
mail. It is not a security boundary; for sender-domain authentication require
`message.authentication is not None and message.authentication.dmarc.status == "pass"`
and compare the literal `message.header_from` address separately. On first
contact from a human the conversation ID arrives `None`. Create the agent
runtime's internal thread before replying, then pass its stable, non-sensitive
thread/session ID (or an opaque stored alias) as `conversation_id`; reuse it on
every later send or reply. If a later inbound ID matches a binding you
previously stored, resume that internal thread. Keep replying by the original
message ID as well — the conversation ID aligns e2a grouping with agent memory,
while the reply endpoint sets the email headers Gmail/Outlook use. Scope
bindings to the inbox and sender, and never use the conversation ID as
authorization.

```python
await client.messages.send(address, {
    "to": ["alice@example.com"],
    "subject": "Hello",
    "text": "Hi from my agent!",
    "conversation_id": "thread-42",
})

# Filter an inbox down to a single thread:
async for m in client.messages.list(address, conversation_id="thread-42"):
    ...
```

## License

Apache-2.0 — see [LICENSE](https://github.com/tokencanopy/e2a/blob/main/LICENSE) and [NOTICE](https://github.com/tokencanopy/e2a/blob/main/NOTICE) in the upstream repo.
