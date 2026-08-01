---
name: e2a
description: "Use when operating e2a (email for AI agents) over its MCP tools — composing, sending, receiving, or replying to email; managing agents and custom domains; or working with attachments — OR when integrating e2a into software with API keys, SDKs, and webhooks. With e2a YOU are the agent and the inbox IS the agent, not a human reading their mail. Covers concise plain-text and HTML composition, send_message vs reply_to_message threading, multi-agent disambiguation, programmatic integration, and common gotchas."
version: 23
---

# Using e2a

<!-- version: 23 -->

e2a is an authenticated email gateway for AI agents. It gives an agent a real email address (`agent@agents.e2a.dev` or `agent@your-domain.com`), verifies sender identity (SPF/DKIM), and threads conversations.

## How this fits

This file is the **operate-well manual** — the mental model and gotchas. It assumes you're already connected over MCP (the tools appear as `mcp__e2a__*`). For the things this file deliberately doesn't duplicate:

- **Connect / pick a client / first inbox** → https://e2a.dev/setup.md
- **Auth (OAuth 2.1 DCR + PKCE, API keys, scopes)** → https://e2a.dev/auth.md
- **Webhook + SDK code (TypeScript / Python, signature verification)** → https://e2a.dev/sdk.md
- **Exact, current tool signatures** → call `tools/list` (authoritative), or the OpenAPI contract at https://e2a.dev/v1/openapi.yaml

The mental model below holds regardless of surface. Tool descriptions teach the precise per-tool contract; this file teaches the model the descriptions assume.

## The mental model

Six load-bearing facts. Internalize these before you start calling tools.

1. **An agent is an email address.** `support-bot@agents.e2a.dev` is an agent. When you send mail, the recipient sees a message FROM that address — not from "the user." When you list messages, you are reading the agent's own inbox, not the user's personal mail. You are not a secretary; you are the mailbox owner.

2. **Replies preserve threads; new sends do not.** `reply_to_message` carries the `In-Reply-To` and `References` headers from the original message, so the response lands in the same email thread. A fresh `send_message` creates a new thread every time. If a user (or an inbound message) is asking you to respond to something specific, reply with the original `message_id` — even when you could synthesize an equivalent body as a new send. Thread fragmentation is the #1 visible symptom of getting this wrong.

   **Email topology and application correlation do not share a key.** Gmail, Outlook, and Apple Mail thread on RFC `Message-ID` / `In-Reply-To` / `References` plus a **stable `Subject`**. `reply_to_message` sets those headers correctly. `conversation_id`, which backs `list_conversations` / `get_conversation`, is caller-owned workflow correlation: a fresh `send_message` with the same value is still a separate email thread, while a reply with a changed value stays in its parent's email thread. REST message reads may expose optional beta `thread_id`, a server-owned mailbox-local topology identity, but MCP deliberately omits it and provides no thread filter or endpoint. Within one ongoing exchange: reply, and keep the subject stable.

   **Bind email to the agent runtime's conversation.** When received mail starts or resumes a coding-agent task, establish the runtime thread before replying. If the inbound `conversation_id` matches a binding your integration previously stored, resume that internal thread; otherwise create a new internal thread. When the runtime exposes a stable, non-sensitive thread/session ID, pass it as `conversation_id` on the first reply and reuse it on later sends and replies. If its native ID is sensitive or does not meet e2a's 200-character, no-CR/LF constraint, store and pass an opaque alias instead. This keeps e2a's application-conversation view aligned with the agent's memory, but it does **not** replace replying with the original `message_id`, which is what preserves the recipient's email-client thread. Treat `conversation_id` as correlation data, never authorization.

3. **`pending_review` is an accepted outcome, not a retry signal.** A send can return `{ status: "pending_review", message_id: "msg_..." }`. The server accepted the message but did not dispatch it. Do not retry: another call can create a duplicate. Report the status and message ID to the user, then stop.

4. **Account-scoped sessions need an explicit inbox.** `whoami` tells you the
credential scope and returns `agent_email` only for an agent-scoped credential.
An account-scoped MCP session never guesses a default, even when the account has
one inbox: enumerate once with `list_agents`, then pass the tool's `email` field
explicitly. Don't guess or pick at random; use the user's stated context when it
clearly identifies an inbox.

5. **Most users don't need a custom domain — default to the shared one.** Every account can create agents on the shared `agents.e2a.dev` domain with zero DNS setup: call `create_agent` with the full address (for example, `support-bot@agents.e2a.dev`), and it is live immediately. This is the right default for onboarding and for anyone who doesn't already **own** a domain. Only reach for a custom domain when the user explicitly owns a domain and wants branded addresses — if they don't own one, stay on `agents.e2a.dev` and skip the domain flow entirely. Don't send a user who just wants to get started down the DNS dance.

6. **Custom domains are a two-step async dance.** `register_domain` returns DNS records (MX + TXT) to publish — it does NOT make the domain live. The user (or a DNS-provider MCP, if one is loaded) must add those records out-of-band, wait for DNS propagation (minutes to hours), then `verify_domain`. Verification is idempotent and safe to retry. Until verification succeeds, the domain cannot send or receive mail. Don't promise the user their domain works the moment registration returns.

## Common workflows

### First run: connect and verify e2a

Before the first e2a operation in a task, inspect the current client's available e2a MCP tools. This is a tool-registry check, not a shell command.

1. **Tools available:** call the e2a MCP `whoami` tool (often exposed as `mcp__e2a__whoami`). Never run the Unix or shell `whoami` command.
2. **Tools absent:** identify the client and guide the user through its shortest setup path. Do not silently edit their configuration.
   - **Claude Code:** run `claude mcp add --transport http --scope user e2a https://api.e2a.dev/mcp`, then have the user run `/mcp` and authorize in the browser.
   - **Codex:** add `[mcp_servers.e2a]` with `url = "https://api.e2a.dev/mcp"` to the Codex config, then have the user run `codex mcp login e2a`.
   - **Cursor / Windsurf:** add `{ "mcpServers": { "e2a": { "url": "https://api.e2a.dev/mcp" } } }` to the client's MCP configuration and complete its OAuth prompt.
   - **Other remote-MCP clients:** configure the Streamable HTTP endpoint `https://api.e2a.dev/mcp` and complete OAuth 2.1 authorization. See https://e2a.dev/setup.md for client-specific details.

   These interactive paths use OAuth. Never ask the user to paste an API key.
3. After the user completes authorization, inspect the tool registry again and call the e2a MCP `whoami` tool.
4. **Classify failures:** an authentication failure means the user should reauthorize through the current client's MCP flow. A network, timeout, or server failure is operational: report it and preserve known-good configuration and credentials.
5. **Select the inbox:** for agent scope, use the returned `agent_email`. For account scope, call `list_agents`. Honor an inbox identified by the task; use the sole result when there is one; ask the user to choose when there are several. If there are none, offer to create `name@agents.e2a.dev` after they choose the local part—no custom domain or DNS is required.
6. Verify readiness with `list_messages`, passing the selected `email` for account scope. This read is harmless and does not mark messages read. Once it succeeds, resume the user's original e2a task.

### Optional: require human review for every outbound email

Only when the user asks for every outbound email to require human review,
configure this policy. After selecting the inbox, call `update_protection` for
that inbox with:

```json
{
  "outbound_gate_policy": "allowlist",
  "outbound_gate_allowlist": [],
  "outbound_gate_action": "review",
  "holds_on_expiry": "reject"
}
```

The empty allowlist makes every recipient a gate non-match, `review` holds each
non-match for a human, and `reject` prevents an unreviewed message from being
sent when its hold expires. Do not use `open` with `review` for this outcome:
`open` matches every recipient, so the recipient gate holds nothing. This is
opt-in; never enable it merely because an inbox was created.

### Triage the inbox

1. List unread messages with `list_messages` (defaults to `read_status: unread`).
2. Read one fully with `get_message` (the `message_id`).
3. Create or resume the coding agent's internal thread. If the runtime exposes a safe stable thread/session ID, pass it as `conversation_id`.
4. Reply in-thread with `reply_to_message` and the original `message_id`; reuse the bound `conversation_id` on later replies.

For attachment bytes, use `get_attachment` with a 0-based index. It returns the attachment's metadata plus a short-lived `download_url`; pass `inline: true` to get base64 `data` inline for small files. Indexes are stable within a message.

### Manage contacts and outreach (beta)

Contacts are durable account-level identity; outreach is one inbox's state for
working that contact. e2a stores the state and derives real send/reply facts,
but it never writes or sends a follow-up on its own.

1. Import and optionally enroll with `import_contacts`. Pass already-parsed
   rows, an explicit `agent_email`, and an optional initial `stage`. Import
   never sends email. Account scope is required for the account-wide import.
2. When the user asks to work the queue, call `list_outreach_contacts` with
   `replied=false`, `suppressed=false`, `next_action_before=<now>`, and
   `last_outbound_before=<stale cutoff>`. The last filter is the duplicate-send
   safety net when a prior send succeeded but the later state update failed.
3. Start first contact with `send_message`. Continue an existing thread with
   `reply_to_message` and a message from that thread; a new send fragments the
   recipient's inbox even if `conversation_id` is reused.
4. After an accepted send, update the caller-owned `stage` and
   `next_action_at` with `set_outreach_contact`. e2a updates reply status,
   activity timestamps, counts, and the latest conversation from real mail.
   Preserve the normal `pending_review` no-retry rule.

`next_action_at` is not a scheduled send. It makes the row available to the due
query and emits `contact.due`. Only a deployed webhook receiver can use that
event to wake an agent runtime; it does not launch a local coding-agent session
over MCP or WebSocket. Claude Code and similar interactive clients work the
queue when the user starts or resumes them.

### Compose before sending

Write for a busy recipient scanning on a phone.

1. Lead with the outcome, decision, request, or blocker in one sentence.
2. Include only information that changes understanding or action.
3. Use short labeled sections and bullets when the message has more than one point.
4. Put any required decision or action in its own line.
5. Link the primary artifact once; leave exhaustive logs and test inventories in the linked artifact.

For a status update, prefer this shape:

```text
Outcome: <one sentence>

Shipped
- <material change>
- <material change>

Verified
- <highest-signal evidence>

Blocker / decision
- <specific ask or owner>

Next: <one sentence>

Artifact: <URL>
```

Omit empty sections. Default to 120–180 words and no more than five bullets. Use one idea per bullet and one or two sentences per paragraph. Do not narrate the work chronologically, repeat the same status in prose and bullets, or paste implementation details that do not affect readiness, risk, or a decision.

Always provide a complete plain-text body. Also provide an equivalent `html` body when the email has sections, bullets, or links; plain text alone is fine for a one- or two-sentence reply. Keep HTML email-safe: use simple `<p>`, `<strong>`, `<ul>`, `<li>`, and `<a>` elements; avoid scripts, images, tables, custom fonts, and elaborate CSS. Make links descriptive and clickable. Preserve the same facts, order, and links in both bodies.

Before sending, verify that the first sentence states what changed or what is needed, any blocker or decision is unmistakable, the message can be understood in ten seconds, and the plain-text and HTML bodies agree.

### Send a new email

1. Compose the message using the guidance above, then call `send_message` with `to`, `subject`, `text`, and `html` when appropriate.
2. Check the response:
   - `status: sent` — done.
   - `status: accepted` — also success, not a maybe. The send was durably persisted and queued for submission (async pipeline). Do NOT re-send. The terminal outcome (delivered or failed) arrives later via webhook events (`email.sent` / `email.failed`) or by polling `get_message`/`list_messages`.
   - `status: pending_review` — accepted but not dispatched. Do not retry; report the status and `message_id`, then stop.

### Templates (beta): recurring sends without free-writing

When the same *kind* of email goes out repeatedly — run reports, digests, approval asks — don't compose it fresh each time. A stored template gives every send the same structure. Reach for one by the third same-shaped send; keep free-writing for one-offs and conversation.

Three starters are agent-native:

- **`agent-status`** — a run report: what you did, what happened.
- **`approval-request`** — ask a human to approve an action before you take it.
- **`daily-digest`** — a scheduled summary of many items.

(The catalog — `list_starter_templates` — also has `welcome`, `verify-code`, `password-reset`, `receipt` for product mail.)

The flow is copy once, send many:

1. `create_template` with `{ "from_starter": "agent-status", "alias": "run-report" }` — copies the starter verbatim into the account's library (account scope; once at setup). Customize the copy later with `update_template` if needed.
2. Send by alias — no literal subject/body (a template reference is mutually exclusive with them):

```json
{ "to": ["owner@acme.com"], "template_alias": "run-report",
  "template_data": { "company_name": "Acme", "support_email": "ops@acme.com",
    "company_address": "100 Main St, San Francisco, CA 94105",
    "agent_name": "deploy-bot", "run_summary": "3 services deployed, 0 failed",
    "sections_html": "<p>api: ok</p>", "sections_text": "api: ok",
    "dashboard_url": "https://app.acme.com/runs/123" } }
```

Syntax is a small Mustache-like subset: `{{var}}` (HTML-escaped in the HTML part), `{{{var}}}` raw, and dot paths into nested data — no loops or conditionals. **Missing variables render as empty strings, silently.** Preview with `validate_template` (its `suggestedData` names every variable the source references) instead of discovering blanks in sent mail. List/table content goes through raw `{{{…_html}}}` fragment slots: you build the HTML fragment, and you must HTML-escape any user-supplied text inside it — raw slots bypass escaping.

**Approval links must be confirmation pages.** For `approval-request`, `approve_url` / `reject_url` must point to pages that require an explicit human click to act — never state-changing GET endpoints. Email security scanners prefetch every link in a message, so a GET-to-approve URL gets "approved" by a robot before the human ever opens the mail.

Templates are beta: shapes may change before they're declared stable. Only `send_message` takes template references — reply and forward don't.

### Add a custom domain (e.g. `mail.acme.com`)

**First: does the user actually own this domain?** If they just want to get started and don't own a domain, skip this entirely — create the agent on the shared `agents.e2a.dev` (mental-model fact #5), which is live with no DNS. Only run the flow below when the user owns the domain and wants branded addresses.

1. `register_domain` with the FQDN — returns MX + TXT records and an unverified domain row.
2. Hand the records to the user (or to a DNS-provider MCP — Cloudflare, Route 53, etc. — if one is loaded; call its `create_dns_record`-style tool with the returned values).
3. Wait. DNS propagation is asynchronous — minutes typically, occasionally hours.
4. `verify_domain` with the same FQDN. If it returns `verified: true`, the domain can RECEIVE mail. If still false, the response shows what DNS state was resolved so the user can debug. Retry as needed.
5. Once verified, agents can be created on (or moved to) that domain.
6. Sending as your own address is a **separate axis** that verifies asynchronously afterwards. Poll `get_domain` until `capabilities.outbound` is `verified` before promising the user mail will send from `@their-domain` — until then outbound still goes out as "… via e2a". `capabilities.inbound` is the receive axis (it restates `verified`); the two are independent, so don't infer one from the other.

### Receive mail in your own backend (webhooks)

If the user is building a service that handles inbound mail in their own code, that's an SDK/webhook job, not an MCP one. Subscribe a webhook (`create_webhook`, a separate `/v1/webhooks` resource — NOT a per-agent "mode") and verify deliveries with the per-webhook `whsec_…` secret returned once at creation. The full handler code (FastAPI / Express, `construct_event` / `constructEvent`) is at https://e2a.dev/sdk.md — defer to it rather than reconstructing it here.

## Integrating e2a into software

Everything above assumes **you** are the agent operating an inbox over MCP. But e2a is also a plain email API any software can build on — the "send and receive email from code" use case, except every address is an authenticated *agent* identity. When a user asks you to **integrate e2a into their app, service, or agent framework**, you're helping them write code against the REST API with an API key — not driving these MCP tools. Make the pivot explicit:

- **MCP** (these tools) — interactive; the agent itself reads/sends mail; auth is OAuth; no integration code. This is you, right now.
- **SDK / REST API** — programmatic; the user's software sends/receives mail; auth is an **API key**. This is what an integration uses.

The mental model in this skill carries over unchanged: the REST/SDK responses use the same `status`, `message_id`, and reply-threading semantics the MCP tools do — only the transport and auth differ.

### Set it up

1. **Issue an API key and choose its scope.** Programmatic access authenticates with a key sent as `Authorization: Bearer e2a_…` (not the OAuth flow MCP uses). Scope is the load-bearing decision:
   - **account** — workspace admin: provision agents, domains, webhooks, and API keys. Use for a backend that manages inboxes.
   - **agent** — bound to one inbox (`agent` email at creation): send / read / reply for that identity only. Use for a service that *is* a single sender.

   The secret is returned **once** at creation — store it in the app's secret manager, never in source. (Keys + scopes: https://e2a.dev/auth.md.) An account-scoped MCP session can mint **agent**-scoped keys directly with `create_api_key`; **account**-scoped keys can only be issued from the dashboard or the raw API.

2. **Have an agent identity to send as.** Mail goes out FROM an agent address — `name@agents.e2a.dev` out of the box, or `name@their-domain.com` after the custom-domain verify dance (see "Add a custom domain" above). Create it once (`create_agent` / `POST /v1/agents`) or reuse an existing one.

3. **Send from code.** `POST /v1/agents/{email}/messages` to send, `…/messages/{id}/reply` to reply in-thread — or the equivalent helper in the TypeScript (`@e2a/sdk`) or Python SDK. If a response has `status: "pending_review"`, surface the status and message ID and do not retry.

4. **Receive from code (optional).** To handle inbound mail or delivery *events* in their backend, subscribe a webhook (`create_webhook` / `POST /v1/webhooks`) and verify every POST with the per-webhook `whsec_…` secret — see "Receive mail in your own backend" above. Don't poll the API for new mail.

The full, current integration code — SDK install, send / reply / parse, webhook handlers with signature verification — lives in the docs, not here. Point the user at:

- SDK + webhook code (TypeScript / Python): https://e2a.dev/sdk.md
- Auth (API keys, scopes, OAuth): https://e2a.dev/auth.md
- REST contract: https://e2a.dev/v1/openapi.yaml

## Gotchas

- **Don't encode raw text as base64 yourself for attachments.** The `data` field expects base64 produced by another tool (a file reader, a doc generator, `get_attachment`). If you have plain text and want to attach it, write it to a file first and read it back, or generate the encoding via a Bash call — don't construct base64 from a Markdown string in your head.
- **Forwarding attachments is a verbatim copy.** Pass the `{filename, content_type, data}` tuple from `get_attachment` straight into the next send's `attachments[]`. No re-encoding, no re-naming necessary.
- **`get_message` deliberately omits raw MIME and attachment bytes.** Don't ask for the "full message" — you have what you need (decoded text/html bodies, headers, attachment metadata). Use `get_attachment` for actual bytes when you need them.
- **Destructive ops require `confirm: true`.** `delete_agent` and `delete_domain` refuse without explicit confirmation. This is a guard against hallucinated deletes; pass it only when the user has clearly asked for the destructive action.
- **Token expiry on OAuth flows.** The hosted MCP runs over OAuth; if a tool starts erroring with auth failures across multiple calls, the token may have expired or been revoked — re-auth via `/mcp` in Claude Code.

## When NOT to use a tool

- Don't send a fresh message to respond to something in the inbox — reply (threading).
- Don't verify a custom domain immediately after registering it — DNS has not propagated. If the user wants a verification check, call it once and report the result; don't poll.
- Don't delete agents or domains from inferred intent. Require the user to say it.
- Don't enumerate agents on every turn. Call `whoami` first; use `list_agents` when it reports account scope and the task does not already identify an inbox.

## Reference

- Connect / clients / first inbox: https://e2a.dev/setup.md
- Auth (OAuth 2.1 DCR + PKCE, API keys, scopes): https://e2a.dev/auth.md
- Webhook + SDK code: https://e2a.dev/sdk.md
- Exact tool signatures: call `tools/list` (authoritative).
- OpenAPI contract: https://e2a.dev/v1/openapi.yaml
- The MCP surface is **76 tools** (20 runtime/inbox + 56 admin/setup) spanning agents, messages, attachments, contacts and outreach, suppressions, domains, events, webhooks, API keys, and templates (beta). The set you see depends on your credential's scope: an agent-scoped credential sees the 20 runtime tools; an account-scoped credential sees all 76. Tool descriptions teach behavior; this skill teaches the mental model. (`create_api_key` mints **agent-scoped** keys only — account-scoped keys come from the dashboard or raw API.)
- Plugin homepage / docs index: https://e2a.dev (machine-readable index: https://e2a.dev/llms.txt)
