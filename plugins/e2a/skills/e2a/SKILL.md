---
name: e2a
description: "Use when operating an already-connected e2a inbox over MCP: reading, composing, sending, replying, forwarding, handling attachments, managing contacts/outreach, scheduling mail, or using templates. Teaches correct threading, conversation correlation, concise multipart composition, and accepted/pending-review no-retry behavior."
version: 29
---

# Using e2a

<!-- version: 29 -->

e2a is the open-source email API for applications and AI agents. This operate-well skill focuses on agent-owned inboxes: it gives an agent a real two-way address (`agent@agents.localhost` or `agent@example.com`), evaluates SPF, DKIM, and DMARC as structured evidence about the From domain, and threads conversations. That evidence does not prove a person, mailbox, or message content.

## How this fits

This file is the **operate-well manual** — the mental model and gotchas. It assumes you're already connected over MCP (the tools appear as `mcp__e2a__*`).

- **Connect, authorize, select/create an inbox, or configure a custom domain** → use `e2a-setup`.
- **Add e2a sending, receiving, SDK, REST, or webhook code to an application** → use `e2a-integrate`.
- **Investigate a failing connection, inbox, domain, webhook, or delivery** → use `e2a-doctor`.
- **Exact, current tool signatures** → call `tools/list` (authoritative).

The mental model below holds regardless of surface. Tool descriptions teach the precise per-tool contract; this file teaches the model the descriptions assume.

## The mental model

Six load-bearing facts. Internalize these before you start calling tools.

1. **An agent is an email address.** `support-bot@agents.localhost` is an agent. When you send mail, the recipient sees a message FROM that address — not from "the user." When you list messages, you are reading the agent's own inbox, not the user's personal mail. You are not a secretary; you are the mailbox owner.

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

6. **Custom domains are a two-step async dance.** `register_domain` returns DNS records (MX + TXT) to publish — it does NOT make the domain live. The user (or a DNS-provider MCP, such as [Cloudflare MCP](https://github.com/cloudflare/mcp), if one is loaded) must add those records out-of-band, wait for DNS propagation (minutes to hours), then `verify_domain`. Verification is idempotent and safe to retry. Until verification succeeds, the domain cannot send or receive mail. Don't promise the user their domain works the moment registration returns.

## Common workflows

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

`next_action_at` is not a scheduled send — it sends nothing. It makes the row
available to the due query and emits `contact.due`, a notification. Only a
deployed webhook receiver can use that event to wake an agent runtime; it
does not launch a local coding-agent session over MCP or WebSocket. Claude
Code and similar interactive clients work the queue when the user starts or
resumes them. If you want e2a itself to submit an already-composed message at
a future time, that is the separate beta `send_at` scheduled-send capability
(see "Schedule a send for later" below) — do not conflate the two: `send_at`
delivers without anyone waking up, `next_action_at` only flags that someone
should.

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
   - `status: scheduled` — also success (beta). A future `send_at` was durably queued; this is durable acceptance exactly like `accepted`, so do NOT retry or re-send — the schedule is already armed, and a second call is a second email. The returned `scheduled_at` is the **future submission time** (a "not before" bound), not a delivery receipt.
   - `status: pending_review` — accepted but not dispatched. Do not retry; report the status and `message_id`, then stop.

### Schedule a send for later (beta)

Pass `send_at` (RFC 3339 with an explicit UTC offset, at most 90 days ahead)
on send, reply, or forward to defer submission. Know these edges:

- **`wait=sent` does not wait until the future time.** A future `send_at`
  returns `status: scheduled` immediately; the bounded wait applies only to
  immediate sends.
- Scheduling survives a review hold: a held outbound message preserves
  `send_at` (surfaced as `scheduled_at`) and re-arms on approval, sending at
  the scheduled time if it is still in the future or immediately if it has
  already passed.
- To cancel, delete (trash) the message before submission starts. Restoring
  it before `scheduled_at` re-arms the send; restoring at or after
  `scheduled_at` restores the message but leaves the send canceled.
- Scheduled sending (`send_at`) and outreach scheduling (`next_action_at`)
  are different concepts — see the outreach workflow above. `send_at`
  submits one already-composed message at a future time with no further
  action from anyone; `next_action_at` sends nothing and only marks when the
  caller intends to act next.

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

- Setup, OAuth, inbox creation, and custom domains: `e2a-setup`.
- Application SDK, REST, and webhook integration: `e2a-integrate`.
- Connection, inbox, domain, webhook, and delivery diagnosis: `e2a-doctor`.
- Exact tool signatures: call `tools/list` (authoritative).
- The MCP surface is **79 tools** (21 runtime/inbox + 58 admin/setup) spanning agents, messages, attachments, delivery metrics, contacts and outreach, suppressions, domains, events, webhooks, API keys, and templates (beta). The set you see depends on your credential's scope: an agent-scoped credential sees the 21 runtime tools; an account-scoped credential sees all 79. Tool descriptions teach behavior; this skill teaches the mental model. (`create_api_key` mints **agent-scoped** keys only — account-scoped keys come from the dashboard or raw API.)
- Plugin homepage / docs index: https://e2a.dev (machine-readable index: https://e2a.dev/llms.txt)
