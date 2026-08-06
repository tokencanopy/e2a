<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/e2a-wordmark-dark.svg">
  <img src="assets/e2a-wordmark-light.svg" width="320" alt="e2a">
</picture>

### Give your AI agents a real, authenticated email address.

Receive inbound over **webhook · WebSocket · REST · MCP**. Send through an **HTTP API**. Every sender — human or agent — **identity-verified**.

<sub>A [Token Canopy](https://tokencanopy.com) product</sub>

[![Tests](https://github.com/tokencanopy/e2a/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/tokencanopy/e2a/actions/workflows/test.yml)
[![Build image](https://github.com/tokencanopy/e2a/actions/workflows/build-image.yml/badge.svg?branch=main)](https://github.com/tokencanopy/e2a/actions/workflows/build-image.yml)
[![License](https://img.shields.io/github/license/tokencanopy/e2a)](LICENSE)
[![npm @e2a/sdk](https://img.shields.io/npm/v/%40e2a%2Fsdk?label=%40e2a%2Fsdk)](https://www.npmjs.com/package/@e2a/sdk)
[![PyPI e2a](https://img.shields.io/pypi/v/e2a)](https://pypi.org/project/e2a/)
[![MCP Toplist](https://img.shields.io/badge/MCP%20Toplist-Top%201%25-4F46FF)](https://mcptoplist.com/server/dev.e2a%2Fmcp-server)
[![Release](https://img.shields.io/github/v/release/tokencanopy/e2a?label=release&color=2ea44f)](https://github.com/tokencanopy/e2a/releases/latest)

**`/v1` is now generally available** — shipped in [**v1.5.0**](https://github.com/tokencanopy/e2a/releases/tag/v1.5.0).

[Hosted (e2a.dev)](https://e2a.dev) · [Quickstart](#quickstart) · [Concepts](#concepts) · [API](#api) · [SDKs](#sdks) · [MCP](#mcp-server) · [Deploy](#deployment) · [FAQ](#faq)

<a href="https://www.producthunt.com/products/e2a-open-source-email-api-for-agents?embed=true&utm_source=badge-featured&utm_medium=badge&utm_campaign=badge-e2a-open-source-email-api-for-agents" target="_blank" rel="noopener noreferrer"><img alt="e2a – open-source email API for agents - Give your AI agents a real, authenticated email address. | Product Hunt" width="250" height="54" src="https://api.producthunt.com/widgets/embed-image/v1/featured.svg?post_id=1145559&theme=light&t=1778615217650"></a>

</div>

---

> [!IMPORTANT]
> **The core `/v1` API and SDKs are stable and generally available (GA) as of [v1.5.0](https://github.com/tokencanopy/e2a/releases/tag/v1.5.0): no breaking changes within `/v1`.** That tag is the compatibility baseline — every later release is audited against it. A small, explicitly enumerated surface is still **beta** and may change before it is declared stable — contacts & outreach, scheduled sending (`send_at`), email templates & starter templates, the reviews (HITL) queue, agent protection config, agent-scoped suppressions, managed unsubscribe, message lifecycle diagnostics, and the `thread_id` message-read field. Beta surface is marked `x-stability-level: beta` in the OpenAPI spec and `(beta)` in the docs; where only specific *values* of a stable field are beta (the `scheduled` send status, the screening/review-hold event types, the `blocked_by_policy` error code), the field carries `x-experimental-values` naming exactly those values. Everything else is covered by the GA freeze. See the full matrix in [docs/api.md → Stability: GA and beta surface](docs/api.md#stability-ga-and-beta-surface). Existing `v1.0.x` application/cherry-pick tags predate the API freeze and are not `/v1` compatibility baselines.

e2a is an **authenticated email gateway for AI agents**. It receives inbound mail, evaluates SPF, every DKIM signature, and DMARC, and delivers structured authentication evidence over whichever channel fits your runtime. Outbound goes back out through an HTTP API, with an optional human-in-the-loop approval gate.

**Four ways to plug an agent in:**

- **MCP** — point any MCP-aware runtime at the hosted server (`https://api.e2a.dev/mcp`) and your agent gets an inbox toolset (`list_messages`, `send_message`, `reply_to_message`, …). The fastest path for agent frameworks. → [MCP server](#mcp-server)
- **SDKs** — TypeScript (`@e2a/sdk`) and Python (`e2a`) clients with one-call webhook verification and a WebSocket `listen()` stream. → [SDKs](#sdks)
- **Raw delivery** — subscribe a **webhook**, open a **WebSocket**, or **poll** the REST API directly. → [Delivery channels](#delivery-channels)
- **CLI** — `e2a listen` bridges inbound mail to a local HTTP handler (including an OpenAI Responses auto-reply mode). → [CLI](#cli)

What you get on top of bare SMTP:

- **Authenticated inbound identity** — normalized SPF, DKIM, and DMARC evidence, with an explicit aligned DMARC verdict
- **No public URL required** — WebSocket, REST polling, and MCP all work from a laptop or behind a firewall
- **Outbound API** — agents send to other agents (SMTP relay) or humans (upstream SMTP, e.g. SES, Resend)
- **Human in the loop** — opt-in approval gate that holds outbound mail until a reviewer approves via dashboard, magic-link email, the MCP tools, or the API
- **Inbound threat screening** — opt-in content scan flags **prompt-injection** payloads (hidden HTML, Unicode-tag smuggling, encoded text) — and, with the LLM detector, **phishing** — then routes each message to *allow · review · block*, feeding the same review queue as HITL → [Content screening](#content-screening)
- **Email reply topology** — standards-compliant reply headers plus optional beta `thread_id` metadata on message reads; caller-owned `conversation_id` remains application correlation
- **Email templates (beta)** — reusable `{{variable}}` templates rendered server-side at send time, plus a pre-built starter catalog → [docs/templates.md](docs/templates.md)
- **Contacts & outreach (beta)** — account-level contact identity (CRUD + bulk import with safe reversal) and per-agent outreach state with server-derived reply/delivery facts, plus the `contact.due` due-queue notification event → [docs/api.md](docs/api.md#contacts--outreach-v1contacts-v1agentsemailcontacts-beta)
- **Scheduled sending (beta)** — `send_at` on send/reply/forward defers submission up to 90 days ahead; a scheduled send is durable acceptance (`status=scheduled`) and can be canceled by trashing the message before submission

## Quickstart

The fastest path is to give your AI agent an inbox directly. Install the e2a plugin — it registers the hosted [MCP server](#mcp-server) and an operate-well skill, so your agent can send, receive, reply in-thread, and hold mail for review out of the box. On first tool use it runs an OAuth flow in your browser — no API key to paste.

**Claude Code**

```
claude plugin marketplace add tokencanopy/e2a
claude plugin install e2a@e2a
```

**Codex**

```
codex plugin marketplace add tokencanopy/e2a
```

Then launch `codex`, run `/plugins`, and install **e2a**.

**Cursor** — add the MCP server directly. Put this in `.cursor/mcp.json` (or `~/.cursor/mcp.json` to get it in every project); Cursor opens your browser to authorize on first use, no API key to paste:

```json
{
  "mcpServers": {
    "e2a": { "url": "https://api.e2a.dev/mcp" }
  }
}
```

**Other MCP clients** (Zed, Goose, Windsurf, Claude Desktop, raw `mcp.json`) — point straight at `https://api.e2a.dev/mcp`; ready-to-paste configs are in [plugins/e2a/clients/](plugins/e2a/clients). See [plugins/e2a/README.md](plugins/e2a/README.md) for the full per-client guide.

## Use it

You can either use the hosted instance or self-host.

- **Hosted** — sign up at [e2a.dev](https://e2a.dev). Includes the shared `agents.e2a.dev` domain for instant slug-based onboarding (no DNS setup), a dashboard, the hosted MCP server, and managed deliverability.
- **Self-host** — see [Self-host (Docker)](#self-host-docker) and [Deployment](#deployment). Every feature works the same; the shared-domain slug shortcut just needs you to point a mail domain at your relay and set `shared_domain` in `config.yaml`.

## How it works

```
   Human (Gmail/Outlook)  ·  another e2a agent
          │   ▲
  inbound │   │ outbound
    SMTP  ▼   │ upstream SMTP (to humans) / relay (to agents)
   ┌───────────────┐
   │   e2a relay   │  ← MX for your agent domain points here
   │               │
   │   inbound  ↓  │  ← evaluate SPF/DKIM/DMARC · deliver
   │   outbound ↑  │  ← optional HITL hold · send
   └───────────────┘
          │   ▲
  deliver │   │ send · reply · forward (HTTP API)
          ▼   │
   ┌───────────────┐
   │   your agent  │  ← webhook / WebSocket / REST poll / MCP · SDK · CLI
   └───────────────┘
```

Inbound flow: SMTP → SPF/DKIM/DMARC evaluation → agent lookup → webhook / WebSocket / REST / MCP delivery.

Outbound flow: API call → optional HITL hold → SMTP relay (agent-to-agent) or upstream SMTP (agent-to-human).

## Concepts

### Delivery channels

Inbound mail reaches you several complementary ways — **chosen per integration, not set on the agent**. There is no delivery "mode" on the agent record; any agent the caller owns can be consumed over any of these:

| Channel | How | Public URL needed? |
|---------|-----|---------------------|
| **Webhooks** | Account-level subscriptions (`POST /v1/webhooks`) — HTTPS POST per event, filterable by agent / application conversation / event type | Yes |
| **WebSocket** | Per-agent real-time notification stream (`/v1/agents/{email}/ws`) + REST fetch | No |
| **REST polling** | Pull messages via `GET /v1/agents/{email}/messages` — the default path for MCP-based agents | No |
| **MCP tools** | The e2a [MCP server](#mcp-server)'s inbox tools (`list_messages`, `get_message`, `get_attachment`, `list_conversations`, …) layered over the REST API | No |

Notifications carry lightweight metadata (message id, sender, subject); you fetch the full body + attachments over REST when you want them. A disconnected WebSocket client accumulates "unread" messages; on reconnect, the server drains them as notifications.

Webhooks are an **account-level resource** (`/v1/webhooks`), chosen per integration rather than configured on the agent.

### Inbound authentication

Inbound messages expose `header_from` (the parsed RFC 5322 From address), `envelope_from` (SMTP MAIL FROM), `verified_domain` (a nullable DMARC-pass convenience projection), and—on detail responses—`authentication` (SPF, every DKIM signature, and the aligned DMARC result). Reply-To remains separate and never replaces `header_from`.

For list and review decisions, a non-null `verified_domain` means DMARC passed for that RFC 5322 From domain. On detail responses, the equivalent check is `authentication?.dmarc.status === "pass"`. Neither field authenticates the mailbox local part, a person, or message content. `authentication` is null for outbound messages and providerless local loopback delivery.

Before trusting any webhook field, verify the delivery envelope's `X-E2A-Signature` with the webhook's `whsec_…` signing secret. The envelope signature covers the complete structured payload, including `authentication`.

The one-call shortcut parses **and** verifies a delivery, returning a typed event — use it instead of trusting any field on an unverified payload:

For small signed-webhook references using the ergonomic inbound facade, see
the [minimal Python and TypeScript OpenAI examples with provider snippets](examples/agent-framework-webhooks/README.md).

```python
from e2a.v1 import construct_event, E2AWebhookSignatureError

# raw request body + the X-E2A-Signature header + your whsec_… secret
try:
    event = construct_event(request_body, signature_header, webhook_secret)
except E2AWebhookSignatureError:
    abort(400)  # bad signature — reject the delivery
if event.type == "email.received":
    email = await client.inbound.from_event(event)
    print(email.envelope_from, email.verified, email.reply_targets)
    result = await email.reply({"text": "Got it"})
    if result.status == "pending_review":
        notify_human(result.message_id)
```

```typescript
import { constructEvent, E2AWebhookSignatureError } from "@e2a/sdk/v1";

let event;
try {
  event = constructEvent(req.body, req.header("X-E2A-Signature")!, webhookSecret);
} catch (err) {
  if (err instanceof E2AWebhookSignatureError) return res.status(400).end(); // bad signature
  throw err;
}
if (event.type === "email.received") {
  const email = await client.inbound.fromEvent(event);
  console.log(email.envelopeFrom, email.verified, email.replyTargets);
  const result = await email.reply({ text: "Got it" });
  if (result.status === "pending_review") notifyHuman(result.messageId);
}
```

`construct_event` / `constructEvent` checks that the HMAC matches the canonical signing string and the timestamp is within a 5-minute replay window. Pass an array of secrets to accept either during a rotation: `constructEvent(body, header, [oldSecret, newSecret])`.

Messages fetched over an authenticated channel — `client.messages.get(address, id)` or the `client.listen(...)` stream — are already trusted (the bearer token authenticated the call), so no verify step is needed there.

### Email threads and application conversations

Email clients build reply threads from the RFC `Message-ID`, `In-Reply-To`,
and `References` graph. Use `reply` with the original e2a message ID so e2a can
emit those headers; a fresh `send` or `forward` starts a new email thread.

`conversation_id` is separate, caller-owned application correlation. Both
`send` and `reply` accept the optional opaque value, and e2a keeps its existing
minting, inheritance, and delivery-correlation behavior when it is omitted.
Applications can use it to associate mail with a workflow, ticket, or model
session, but reusing it does not join fresh sends into one email thread, and
changing it does not split replies out of their RFC thread.

Existing message list and detail responses may also include `thread_id`
(`threadId` in TypeScript and SDK-shaped CLI JSON). This optional beta field is
server-owned, read-only, scoped to one agent mailbox, and derived from reply
topology. It is omitted for legacy rows without an assignment. There is no
`thread_id` request field, message filter, or thread list/detail endpoint, and
the field is not added to webhook events, WebSocket notifications, exports, or
MCP output.

Agent frameworks should bind model memory with `conversation_id`: create or
resume the runtime's internal conversation, pass its stable, non-sensitive
session ID (or an opaque stored alias), and scope that binding to the inbox and
sender. Continue replying by the original message ID—the correlation value
aligns application state, while RFC reply headers preserve the Gmail/Outlook
thread. Never treat either identifier as authorization.

### Content screening

Inbound email is a prime **indirect prompt-injection** vector — a message can smuggle instructions aimed at your agent's LLM (hidden HTML, zero-width / Unicode-tag text, encoded payloads) or phish the human behind it. Opt in per agent and e2a inspects message *content* — subject, plaintext, and both visible **and hidden** HTML — before your agent ever sees it.

A built-in, dependency-free **heuristics** detector flags prompt-injection, jailbreak, obfuscation, and data-exfiltration patterns (mapped to OWASP LLM01 / MITRE ATLAS); an optional LLM detector adds semantic injection **and phishing** classification. Each message gets a verdict — **allow · review · block** — set by the agent's scan sensitivity (`off · low · medium · high`): `review` routes it into the shared [HITL](#human-in-the-loop-hitl) queue, `block` drops it before delivery. Screening is **fail-safe** — if a detector times out or degrades, the message fails *to review*, never to a silent allow — and every verdict is written to `protection_events` for audit and threshold tuning.

Turn it on with `PUT /v1/agents/{email}/protection` (the same sub-resource as HITL holds), which carries the inbound/outbound × gate/scan posture.

### Human in the loop (HITL)

When an agent's protection config holds an outbound message for review, `send` and `reply` calls do **not** dispatch immediately. The message is stored with status `pending_review` and the API returns HTTP `202 Accepted`. A reviewer must approve it before delivery; otherwise, after a configurable TTL, the protection config's `holds.on_expiry` decides the terminal: `approve` (the message just goes out, terminal status `sent` — for outbound, approving *is* sending) or `reject` (discard, `review_expired_rejected`). (Inbound messages can be held for review too — there, the auto-approve terminal is `review_expired_approved`, releasing the message to the inbox.)

Reviewers can approve or reject via:

- **Dashboard / API** — the account-scoped review queue `POST /v1/reviews/{id}/approve` or `/reject` (id-addressed, no inbox email needed; lists held items across all the account's inboxes via `GET /v1/reviews`). This is the only approve/reject path — a review's `id` is the held message's `id`.
- **MCP tools** — `approve_review` / `reject_review` (with `list_reviews` / `get_review` to find them).
- **Magic-link email** — sent automatically when a hold fires; one-click `GET /v1/approve?t=…` and `/v1/reject?t=…` URLs (requires `E2A_PUBLIC_URL` and outbound SMTP configured).

Enable review holds on an agent via `PUT /v1/agents/{email}/protection`: set the outbound gate action to `review` (or turn on the content scan), plus the hold TTL (`holds.ttl_seconds`) and its expiry behavior (`holds.on_expiry` = `approve` or `reject`). Posture lives entirely on the protection sub-resource.

## API

All endpoints are under `/v1` unless noted. Auth is `Authorization: Bearer <api_key>` except for `/api/health`, `/v1/info`, `/api/feedback`, and the HITL magic-link routes. Path parameters containing `@` (agent emails) must be URL-encoded.

The surface covers domain registration + verification, agent CRUD, inbound/outbound messages, webhook subscriptions, HITL approve/reject (API key or signed magic-link token), GDPR-style export and deletion, and a WebSocket channel for real-time inbound delivery.

See [docs/api.md](docs/api.md) for the full endpoint reference, or [`api/openapi.yaml`](api/openapi.yaml) for the machine-readable spec.

## MCP server

The fastest way to give an AI-agent runtime an inbox. e2a runs a hosted [Model Context Protocol](https://modelcontextprotocol.io) server — point any MCP-aware host (Claude Desktop, Cursor, Cline, Google ADK, LangChain, OpenAI Agents SDK, …) at the Streamable HTTP endpoint:

```
https://api.e2a.dev/mcp
```

Authenticate either with **OAuth 2.1** (add e2a as a connector and authorize in the browser) or a **Bearer API key** (`Authorization: Bearer <e2a API key>`). An agent-scoped credential resolves its agent server-side; account-scoped callers pass the agent `email` per tool call.

The toolset covers the full agent loop — inbox (`list_messages`, `get_message`, `get_attachment`, `list_conversations`, `get_conversation`, `update_message_labels`), outbound (`send_message`, `reply_to_message`, `forward_message`), HITL review (`list_reviews`, `get_review`, `approve_review`, `reject_review`), plus agent/domain/webhook management. Inbound is consumed by polling (`list_messages`) or a `create_webhook` subscription.

The hosted server is the primary path; npm publishing of `@e2a/mcp-server` is retired (frozen at `0.5.0`). See [mcp/README.md](mcp/README.md) for per-framework setup and the full tool reference.

## CLI

```bash
npm install -g @e2a/cli
e2a login
```

The CLI covers both scripting (send/reply/messages/whoami, with a stable
exit-code contract) and account management (agents, keys, protection). Drive
agents interactively over the **MCP tools** or the **SDKs** instead; manage
domains/webhooks in the **web dashboard**.

| Command | Description |
|---------|-------------|
| `e2a login` | Open a browser login and save an account-scoped API key to `~/.e2a/config.json` (does not set a default agent — use `e2a config set agent_email` or `--agent`) |
| `e2a whoami` | Show the key identity: user, scope, bound agent, plan |
| `e2a doctor` | Read-only diagnostics of the production email path: config, API, agent access, custom-domain DNS (live), MCP, webhooks, outbound SMTP visibility. Never sends mail or mutates anything; `--json` for a versioned report |
| `e2a agents list\|create\|get` | Manage inboxes (requires an account-scoped key) |
| `e2a keys create\|list\|delete` | Mint, list, and revoke API keys (requires an account-scoped key) |
| `e2a protection get\|set` | Show or update an agent's HITL screening/review config |
| `e2a contacts list\|get\|create\|update\|delete\|import\|outreach ...` | Manage account contacts and per-agent outreach state, with suppression visibility |
| `e2a suppressions list\|add\|remove` | Inspect and manage recipient block lists (account-wide GA; agent-scoped is beta) |
| `e2a send` / `e2a reply` | Send an email as the agent, or reply in-thread |
| `e2a messages list\|get` | List or fetch messages for an agent |
| `e2a listen --agent <email>` | Stream inbound email for an agent over WebSocket (real-time; `--json` for raw, `--forward <url>` to bridge to a local HTTP handler) |
| `e2a config [list\|get\|set]` | View or update the local config |

When the `--forward <url>` endpoint path ends in `/v1/responses`, `listen` switches to **OpenAI Responses API forwarding**: each inbound email is formatted as a Responses payload and the model's output is sent back as an auto-reply. Add `--forward-token <token>` to attach a bearer token to the forwarded request:

```bash
e2a listen --forward http://localhost:18789/v1/responses --forward-token <token>
```

See [cli/README.md](cli/README.md) for full reference.

## SDKs

### Python

```bash
pip install e2a            # webhook mode
pip install 'e2a[ws]'      # adds WebSocket support
```

```python
from e2a.v1 import AsyncE2AClient, construct_event

client = AsyncE2AClient()                                       # reads E2A_API_KEY
event = construct_event(request_body, signature_header, webhook_secret)  # parse + HMAC-verify
if event.type == "email.received":
    email = await client.inbound.from_event(event)
    print(email.envelope_from, email.verified, email.reply_targets)
    result = await email.reply({"text": "Got it!"})
    if result.status == "pending_review":
        print("awaiting approval", result.message_id)
```

WebSocket (no public URL needed):

```python
from e2a.v1 import AsyncE2AClient

async with AsyncE2AClient(api_key="e2a_…") as client:
    async for event in client.listen("bot@your-domain.com"):
        if event.type != "email.received":
            continue  # tolerate future event kinds
        email = await client.inbound.from_event(event)
        result = await email.reply({"text": "Got it!"})
        if result.status == "pending_review":
            print("awaiting approval", result.message_id)
```

See [sdks/python/README.md](sdks/python/README.md).

### TypeScript

```bash
npm install @e2a/sdk
```

See [sdks/typescript/README.md](sdks/typescript/README.md).

## Deployment

Three audiences each configure a different surface:

| Audience | What they configure | Where |
|---|---|---|
| **Server operator** — runs the Go backend | DB, signing key, SMTP, OAuth, optional shared domain | `config.yaml` + `E2A_*` env |
| **CLI user** — drives an inbox from a terminal | Deployment URL + login | `E2A_URL` + `e2a login` |
| **SDK / MCP user** — calls `/v1` from code | API host + key | `E2A_API_URL` + `E2A_API_KEY` |
| **Web dashboard deployer** — hosts the Next.js dashboard | Public site URL + branding | `NEXT_PUBLIC_*` build-time env |

The Go binary runs on any container host; storage is plain Postgres 14+; outbound mail goes through standard SMTP. Most workers coordinate via `SELECT … FOR UPDATE SKIP LOCKED`, so multi-replica is safe — the two real horizontal-scaling caveats are in-memory WebSocket fan-out and per-process rate limits.

See [docs/deployment.md](docs/deployment.md) for the full env-var reference, shared-domain DNS setup, and scaling/limitation notes.

## Security

- **Identity** — agent registration requires DNS TXT verification of domain ownership (custom domains)
- **Domain auth** — SPF and DKIM checked on every inbound message
- **Header signatures** — HMAC-SHA256 over the `<t>.<body>` signing string delivered in the `X-E2A-Signature` header; receivers reject timestamps older than 5 minutes
- **SSRF protection** — webhook URLs must be HTTPS (in production), resolve to public IPs, use domain names (no raw IPs, no private/loopback ranges)
- **OAuth CSRF** — single-use, time-limited nonce in the `state` parameter
- **Production mode** (`env: production` in `config.yaml`) enforces the above where development mode is more permissive

Report security issues privately — see [SECURITY.md](SECURITY.md) for the disclosure process and what's in scope. **Do not file public GitHub issues for vulnerabilities.**

## Data handling

Live inbound and outbound message data is retained indefinitely; soft-deleted messages and inboxes are purged after 30 days by default. Outbound bodies and attachments remain retained through terminal review and delivery transitions. API keys are stored as hashes; attachments go in JSONB rows (no S3/GCS). Application logs include sender/recipient addresses (standard MTA practice) but never bodies, attachments, raw keys, or HMAC secrets. Users can self-export (`GET /v1/account/export`) and self-delete (`DELETE /v1/account?confirm=DELETE`) for GDPR Art. 15 / Art. 17 / CCPA.

See [docs/data-handling.md](docs/data-handling.md) for the full retention table, log fields, user-rights endpoints, and the operator-side responsibilities (backups, TLS, at-rest encryption, log redaction, compliance).

## FAQ

### Why not just use SendGrid / Resend / Postmark for sending and their inbound parsing for receiving?

Four things that aren't possible to bolt on without significant rework:

1. **Inbound with no public URL.** Agents authenticate with their API key and consume inbound mail over a WebSocket to `/v1/agents/{email}/ws`, by polling the REST API, or through the MCP tools — no webhook URL, no ngrok, no port forward. Useful for agents on developer laptops, edge devices, or behind corporate firewalls. SendGrid/Resend are webhook-only by design.

2. **Email threading on every reply.** e2a resolves the RFC
   `Message-ID` / `In-Reply-To` / `References` graph within each agent mailbox,
   emits correct reply headers, and keeps a server-owned topology identity for
   message reads. `conversation_id` remains independent application
   correlation. SendGrid/Resend never see inbound mail—they are not
   receivers—so they cannot provide this bidirectional mailbox-local topology
   without you building the receiving side yourself.

3. **Slug provisioning on a shared domain.** Operators set `shared_domain: agents.e2a.dev` and users `POST {"email": "my-agent@agents.e2a.dev"}` to immediately register an agent on the shared domain with no DNS configuration. Possible because e2a *is* the SMTP relay claiming the domain — Resend / SendGrid are providers, not platforms, and can't multi-tenant a shared address space without you running the relay yourself.

4. **Built-in review hold + auto-expiration.** A per-agent protection policy (outbound gate action `review`, or the content scan) holds mail in `pending_review` state. Reviewers approve via dashboard, magic-link email, the MCP tools, or the API; a background worker auto-acts on expired holds based on the `holds.on_expiry` config. Magic-link tokens are HMAC-encoded — stateless, no session backend. With Resend / SendGrid you'd hold the message in your own DB, build the timer, the approval UI, and the stateless review tokens.

You can absolutely use SES / Resend / SendGrid as e2a's *outbound* SMTP for delivery to humans — that's what `outbound_smtp` in `config.yaml` is for. They complement e2a; they don't replace the inbound receiver, agent abstraction, or any of the layers above transport.

### Why email at all? Why not webhooks, gRPC, or MCP between agents?

Email is the only protocol where every human already has an address and a working client. Webhooks / gRPC / MCP are great inside systems you control, but they don't reach Gmail or Outlook. If you want an agent that talks to humans (or to *other organizations'* agents) without forcing everyone to install a new client, email is the universal substrate.

e2a doesn't replace webhooks or MCP — your agent *receives* email through them. It bridges email's universal addressability to the structured-data world the agent code already lives in.

### What stops an attacker from spoofing authentication results?

The relay discards any sender-supplied authentication claims and evaluates SPF, DKIM, and DMARC itself. For webhooks, verify `X-E2A-Signature` with the subscription's `whsec_` secret before trusting the structured result; a forged POST then fails verification regardless of its claimed DMARC status.

Receivers verify with the SDK — `construct_event(body, header, secret)` / `constructEvent(body, header, secret)` does parse + HMAC verify in one call (or `verify_webhook_signature(...)` / `verifyWebhookSignature(...)` if you only need the boolean check). No API call back to e2a needed. If a signing secret leaks, rotate it via the dashboard; the previous secret keeps verifying through a 24h grace window, then stops. If it is stolen from the relay itself, treat the relay as compromised rather than trusting any webhook headers it emits.

### Isn't this just SMTP with extra steps?

Yes — and the extra steps are the point. Concretely:

- SPF/DKIM verdict normalization so receivers don't reimplement domain auth
- Structured SPF/DKIM/DMARC evidence with explicit identifier alignment
- WebSocket / REST / MCP transport for agents without public URLs
- HITL approval flow with auto-expiration and stateless magic-link review
- Mailbox-local reply topology plus caller-owned application correlation
- Slug-based agent provisioning on a shared domain
- Per-agent webhook routing, rate limits, and HITL config

Building those on top of bare Postfix is a real project. e2a is that project, open source.

### How does this compare to running Postfix or Postal myself?

If you want a full MTA, run an MTA — Postfix and Postal are great. e2a isn't trying to replace them at the SMTP transport level (it uses `go-smtp` for receiving and dial-out for sending). The value is the layer above transport: the auth model, agent abstraction, signed delivery contract, retry policy for webhook failures, HITL approval flow, SDKs and CLI. If you're comfortable operating an MTA and only need email plumbing, e2a may be more than you want. If you want the agent abstraction and signed identity layer prebuilt, that's what this is.

### Why open source if there's a hosted version?

Two reasons:

1. **Auditability.** Identity infrastructure for your agents should be readable code, not a vendor black box. You can verify the cosign signature on `ghcr.io/tokencanopy/e2a`, reproduce the build, and confirm what's actually running.
2. **Self-host as a real option.** The hosted instance at e2a.dev runs the same `ghcr.io/tokencanopy/e2a` image you can pull right now. Convenience features on the hosted side (the shared `agents.e2a.dev` domain, managed deliverability) are config + DNS, not closed-source extras.

The hosted version at [e2a.dev](https://e2a.dev) has paid tiers (a free tier plus paid plans); billing is opt-in on the hosted side — config (settable via env) points the OSS server at an external limits/billing sidecar, and the OSS code path stays unchanged. Self-hosting runs on generous default limits with no billing.

## Development

```bash
make build               # go build -o bin/e2a ./cmd/e2a
make run                 # build + run (cp config.example.yaml config.yaml first)
make test                # all Go tests (needs Postgres on :5433)
make test-unit           # Go unit tests only (no DB)
make test-integration    # integration tests (needs Postgres)
make test-e2e            # e2e tests (needs Postgres)
make cover-check         # tests + per-package coverage floors (needs Postgres)
docker compose up -d postgres mailpit  # local Postgres + Mailpit only
make migrate             # apply SQL migrations to local DB
```

See [CLAUDE.md](CLAUDE.md) for the full developer guide (architecture, tests, code generation, conventions).

## Self-host (Docker)

Requires Docker.

```bash
git clone https://github.com/tokencanopy/e2a.git
cd e2a
docker compose up -d
```

Postgres comes up first (migrations run automatically), then the API server, then the dashboard. Four host ports:

- `:8080` — HTTP API
- `:2525` — SMTP relay
- `:3000` — Dashboard (Caddy + Next.js, proxies `/api/*` to the API server)
- `:8765` — Local MCP HTTP endpoint (what `claude mcp add` expects)

Health check:

```bash
curl http://localhost:8080/api/health
# {"status":"ok"}
```

Open `http://localhost:3000` in a browser to view the dashboard. Sign-in requires Google OAuth credentials configured in `config.yaml`; for an API-only smoke test you can skip the dashboard and use the bootstrap flow below.

Create your first user and API key (no OAuth required):

```bash
docker compose exec e2a e2a -config /etc/e2a/config.yaml -bootstrap-email you@example.com
# User:    you@example.com (id=...)
# API key: e2a_...
```

Save the key — it's only shown once. Register an agent and confirm it works:

```bash
KEY=e2a_...
curl -X POST http://localhost:8080/v1/agents \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"email":"my-bot@agents.localhost"}'   # the local compose shared domain (or a domain you've verified)

curl -H "Authorization: Bearer $KEY" http://localhost:8080/v1/agents
```

To receive real inbound mail, point a domain's MX record at your relay host:

- **A**: `your-domain.com` → server IP
- **MX**: `your-domain.com` → `your-domain.com` (priority 10)

Then register and verify the domain through the API (see [Domains](docs/api.md)). Without DNS, the API still works for testing — but external email won't reach your relay.

> **Upgrades and migrations.** The e2a binary embeds `migrations/*.sql` and **auto-applies any pending ones at startup** (tracked in a `schema_migrations` table). When you upgrade e2a, restarting the container applies new schema migrations automatically — no manual step. `E2A_MIGRATION_MODE` controls this: `auto` (default, applies pending), `verify` (refuse startup and report pending), or `skip` (emergency surgery). Migrations are idempotent and non-destructive, so re-applying is safe.

> Thread-identity upgrades include several `CREATE INDEX CONCURRENTLY` migrations. The
> direction-aware legacy-anchor indexes inspect existing message rows and can keep the
> first upgraded process in migration startup until each build completes; schedule the
> rollout with normal migration headroom and monitor startup logs. They take the
> migration advisory lock but do not block ordinary reads or writes.
>
> (The compose file also mounts `migrations/` into Postgres' init directory, but that path only runs on first start with an empty data volume — the binary's startup auto-apply is what keeps an upgraded deployment current.)

## Contributing

By submitting a pull request, you certify the [Developer Certificate of Origin](https://developercertificate.org/) for your contribution. Sign your commits with `git commit -s`.

## License

Apache 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
