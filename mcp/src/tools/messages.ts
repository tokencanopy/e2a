import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { McpClient, SendOpts } from "../client.js";
import type { MessageSummaryView, MessageView } from "@e2a/sdk/v1";
import { z } from "zod";
import { runTool, strictInputSchema, paginationInput, emailSelector } from "./util.js";
import { attachmentsArraySchema, type AttachmentInput } from "./attachments.js";

// Map the snake_case attachment wire shape (filename, content_type, data)
// to the SDK Attachment model (filename, contentType, data).
function mapAttachments(
  atts?: AttachmentInput[],
): Array<{ filename: string; contentType: string; data: string }> | undefined {
  if (atts === undefined) return undefined;
  return atts.map((a) => ({
    filename: a.filename,
    contentType: a.content_type,
    data: a.data,
  }));
}

// MessageView → the context-safe tool shape shared by `get_message` and
// `get_review`. Attachment metadata comes from the server
// (MessageView.attachments, parsed server-side) — the authoritative, stable
// index the download route also uses. Attachment BYTES are NOT returned here;
// call get_attachment for one. `raw_message` is likewise omitted so the LLM
// never sees the full (potentially multi-MB) MIME blob.
export function messageViewForTool(email: MessageView) {
  return {
    id: email.id,
    conversation_id: email.conversationId,
    direction: email.direction,
    header_from: email.headerFrom,
    envelope_from: email.envelopeFrom,
    verified_domain: email.verifiedDomain,
    authentication: email.authentication,
    delivered_to: email.deliveredTo,
    to: email.to,
    cc: email.cc,
    reply_to: email.replyTo,
    subject: email.subject,
    read_status: email.readStatus,
    labels: email.labels,
    flagged: email.flagged,
    flag_reason: email.flagReason,
    protection: email.protection,
    truncated: email.parsed?.truncated,
    // Inbound messages carry the decoded text in `parsed`; only outbound
    // held drafts populate `body` (mirror the CLI's read fallback).
    text: email.parsed?.text ?? email.body?.text,
    html: email.parsed?.html ?? email.body?.html,
    delivery_status: email.deliveryStatus,
    delivery_detail: email.deliveryDetail,
    review_status: email.reviewStatus,
    sent_as: email.sentAs,
    scheduled_at: email.scheduledAt,
    size_bytes: email.sizeBytes,
    deleted_at: email.deletedAt,
    received_at: email.createdAt,
    attachments: (email.attachments ?? []).map((a) => ({
      index: a.index,
      filename: a.filename,
      content_type: a.contentType,
      size_bytes: a.sizeBytes,
    })),
  };
}

// MessageSummaryView → the frozen MCP list_messages shape. Keep this an
// explicit projection rather than passing generated SDK models through: beta
// REST/SDK fields such as threadId must not silently expand MCP output.
export function messageSummaryViewForTool(message: MessageSummaryView) {
  return {
    id: message.id,
    direction: message.direction,
    header_from: message.headerFrom,
    envelope_from: message.envelopeFrom,
    verified_domain: message.verifiedDomain,
    to: message.to,
    cc: message.cc,
    reply_to: message.replyTo,
    delivered_to: message.deliveredTo,
    subject: message.subject,
    conversation_id: message.conversationId,
    read_status: message.readStatus,
    review_status: message.reviewStatus,
    webhook_status: message.webhookStatus,
    webhook_error: message.webhookError,
    delivery_status: message.deliveryStatus,
    delivery_detail: message.deliveryDetail,
    sent_as: message.sentAs,
    scheduled_at: message.scheduledAt,
    flagged: message.flagged,
    flag_reason: message.flagReason,
    size_bytes: message.sizeBytes,
    labels: message.labels,
    created_at: message.createdAt,
    deleted_at: message.deletedAt,
  };
}

// Shared scheduled-send input. A future send_at defers submission to that
// instant (status "scheduled"); the tool passes it through as a Date so the SDK
// serializes it as a date-time. Same field on send/reply/forward.
const sendAtField = z
  .string()
  .datetime({ offset: true })
  .optional()
  .describe(
    'Beta: scheduled sending may change before it is declared stable. Optional scheduled-send time in RFC 3339 format with an explicit UTC offset. When set to a future instant, the message is accepted immediately with status "scheduled" and submitted at approximately that time ("not before", accurate to seconds). A value at or before now sends immediately; more than 90 days ahead is rejected. A future send_at whose only recipient is the sending agent\'s own address returns 400 invalid_request because self-delivery is an immediate loopback with no scheduled arm — even when the message would otherwise be held for review. Scheduling survives a review hold: if held, send_at is preserved on the pending_review message (surfaced as scheduled_at) and re-armed on approval — submitted at send_at if still in the future, or immediately if it has already passed. Moving the message to trash before provider submission starts prevents submission; if submission already has a fresh lease, delete returns 409 send_in_progress. Restoring before send_at re-arms it; restoring at or after send_at returns it live with delivery_status=failed and leaves the send canceled.',
  );

export function registerMessageTools(server: McpServer, client: McpClient): void {
  server.registerTool(
    "send_message",
    {
      title: "Send email",
      annotations: { destructiveHint: false },
      description:
        "Use when starting a NEW email thread to a fresh recipient. To respond to a message you can see in `list_messages`, use `reply_to_message` instead — it preserves the In-Reply-To / References headers so the reply lands in the same thread, which this tool deliberately does not do. Attach files via `attachments`; pass base64 strings produced by other tools (e.g. `get_attachment`) verbatim — don't hand-encode raw text. **`accepted`, `scheduled`, and `pending_review` are all success, not failure — do NOT re-send.** `{ status: \"accepted\", message_id: ... }` means the send was durably persisted and queued for immediate submission. `{ status: \"scheduled\", message_id: ..., scheduled_at: ... }` means it was durably queued for future submission at `scheduled_at`; `wait=sent` does not wait until then. `{ status: \"pending_review\", message_id: ... }` means a human review hold caught it first. In every case, re-calling this tool (especially without reusing the same `idempotency_key`) risks a real second send — the terminal outcome (delivered or failed) arrives later via webhook events (`email.sent` / `email.failed`) or by polling `get_message`/`list_messages`, not by retrying. **Templates (beta):** instead of literal subject/text, reference a stored template with `template_id` XOR `template_alias` plus `template_data` — a template reference is mutually exclusive with subject/text/html (pass neither literal field). The server renders before any review hold, so a reviewer sees final content. Missing variables render as empty strings (no error) — validate data against the template's variables first. Only send supports templates; reply/forward do not.",
      inputSchema: strictInputSchema({
        to: z.array(z.string()).describe("Recipient email addresses (one or more)."),
        subject: z.string().optional().describe("Literal subject. Required unless a template reference is used (then it must be omitted)."),
        text: z.string().optional().describe("Literal plain-text body; use `html` for HTML. Required unless a template reference is used (then it must be omitted)."),
        html: z.string().optional(),
        template_id: z
          .string()
          .optional()
          .describe(
            "Send using a stored template by id (tmpl_…), rendered server-side. Mutually exclusive with template_alias and with literal subject/text/html. Beta.",
          ),
        template_alias: z
          .string()
          .optional()
          .describe(
            "Send using a stored template by its per-user alias (see `list_templates`). Mutually exclusive with template_id and with literal subject/text/html. Beta.",
          ),
        template_data: z
          .record(z.string(), z.unknown())
          .optional()
          .describe(
            "Variables for the referenced template ({{name}}, dot paths into nested objects). Missing variables render as EMPTY strings — no error. For raw {{{…_html}}} fragment slots, HTML-escape any user content you splice in. Requires template_id or template_alias. Beta.",
          ),
        cc: z.array(z.string()).optional(),
        bcc: z.array(z.string()).optional(),
        attachments: attachmentsArraySchema,
        conversation_id: z
          .string()
          .optional()
          .describe(
            "Optional stable conversation grouping ID. When bridging email to an agent runtime, pass that runtime's non-sensitive thread/session ID (or an opaque alias) and reuse it on later sends and replies so e2a grouping follows the agent's internal conversation. Maximum 200 characters; no CR/LF. Server generates one if omitted.",
          ),
        reply_to: z
          .union([z.string(), z.array(z.string()).max(5)])
          .optional()
          .describe(
            "Sets the Reply-To header — where replies to this message are directed. Either a single address (optionally with a display name, e.g. \"Support <support@acme.com>\") or an array of up to 5 addresses to direct replies to several destinations. Defaults to the sending agent's own address.",
          ),
        idempotency_key: z
          .string()
          .optional()
          .describe(
            "Stable key for retry-safe sends. Set to deduplicate when the caller has its own retry loop (e.g. a stable triggering event id). When omitted the SDK mints a fresh UUIDv4 per call — protects against network-layer retries only, not user-driven retries.",
          ),
        send_at: sendAtField,
        email: emailSelector,
      }),
    },
    async (args) =>
      runTool(() => {
        const opts: SendOpts =
          args.idempotency_key !== undefined
            ? { idempotencyKey: args.idempotency_key }
            : {};
        return client.send(
          {
            to: args.to,
            // subject/body are optional on the wire when a template reference
            // is used — only forward what the caller passed so the server's
            // mutual-exclusivity check sees the true shape.
            ...(args.subject !== undefined ? { subject: args.subject } : {}),
            ...(args.text !== undefined ? { text: args.text } : {}),
            ...(args.html !== undefined ? { html: args.html } : {}),
            ...(args.template_id !== undefined ? { templateId: args.template_id } : {}),
            ...(args.template_alias !== undefined ? { templateAlias: args.template_alias } : {}),
            ...(args.template_data !== undefined ? { templateData: args.template_data } : {}),
            ...(args.cc !== undefined ? { cc: args.cc } : {}),
            ...(args.bcc !== undefined ? { bcc: args.bcc } : {}),
            ...(mapAttachments(args.attachments) !== undefined
              ? { attachments: mapAttachments(args.attachments) }
              : {}),
            ...(args.conversation_id !== undefined
              ? { conversationId: args.conversation_id }
              : {}),
            ...(args.reply_to !== undefined ? { replyTo: args.reply_to } : {}),
            ...(args.send_at !== undefined ? { sendAt: new Date(args.send_at) } : {}),
          },
          opts,
          args.email,
        );
      }),
  );

  server.registerTool(
    "reply_to_message",
    {
      title: "Reply to a message",
      annotations: { destructiveHint: false },
      description:
        "Use whenever you're responding to a message you can see — preserves the In-Reply-To and References headers so the reply joins the original email thread instead of starting a new one. Works on both a message the agent RECEIVED (replies to its sender) and a message the agent SENT (continues the thread to its original recipients, i.e. a Gmail-style follow-up on your own message). Prefer this over `send_message` for any in-thread response; thread fragmentation (broken conversation view in the recipient's mail client) is the most visible symptom of using `send_message` by mistake. Pass `reply_all: true` to copy the original Cc list; subject is auto-derived as `Re: …` by the server. Same review caveat as `send_message`: **`accepted`, `scheduled`, and `pending_review` are all success, not failure — do NOT re-send.** `accepted` means the reply was durably persisted and queued for immediate submission; `scheduled` means it was durably queued for future submission at `scheduled_at`; `pending_review` means a human review hold caught it first. The terminal outcome arrives later via webhook events (`email.sent` / `email.failed`) or by polling `get_message`, not by retrying.",
      inputSchema: strictInputSchema({
        message_id: z.string().describe("ID of the message to reply to — inbound or one the agent sent (e.g. msg_…)."),
        text: z.string().describe("Plain-text reply body."),
        html: z.string().optional(),
        reply_all: z
          .boolean()
          .optional()
          .describe("If true, copy the original message's Cc list."),
        cc: z.array(z.string()).optional(),
        bcc: z.array(z.string()).optional(),
        attachments: attachmentsArraySchema,
        conversation_id: z
          .string()
          .optional()
          .describe(
            "Optional conversation grouping override. On the first reply to received mail, set this to the agent runtime's stable, non-sensitive thread/session ID (or an opaque alias), then reuse it on later replies. This aligns e2a grouping with internal memory; message_id still preserves the recipient's email-client thread. Maximum 200 characters; no CR/LF.",
          ),
        reply_to: z
          .union([z.string(), z.array(z.string()).max(5)])
          .optional()
          .describe(
            "Sets the Reply-To header — where replies to this message are directed. Either a single address (optionally with a display name) or an array of up to 5 addresses to direct replies to several destinations. Defaults to the sending agent's own address.",
          ),
        idempotency_key: z
          .string()
          .optional()
          .describe(
            "Stable key for retry-safe replies. A natural choice is the inbound `message_id` you're replying to — the same triggering event yields the same key, so a retry replays the original response instead of double-sending. Omit to let the SDK mint a fresh UUIDv4 per call.",
          ),
        send_at: sendAtField,
        quote_history: z
          .boolean()
          .optional()
          .describe(
            "Experimental (may change or be removed before stable): if true, the server appends the message being replied to as mail-client-style quoted history beneath the reply body — an 'On <date>, <sender> wrote:' attribution line followed by the original text ('>'-prefixed) and, when `html` is supplied, the original HTML in a blockquote. Composition happens server-side at accept time, so a held reply shows the reviewer the final quoted content. Only the body parts you supply are quoted (a text-only reply stays text-only). Defaults to false (the body is sent exactly as provided).",
          ),
        email: emailSelector,
      }),
    },
    async (args) =>
      runTool(() => {
        const opts: SendOpts =
          args.idempotency_key !== undefined
            ? { idempotencyKey: args.idempotency_key }
            : {};
        return client.reply(
          args.message_id,
          {
            text: args.text,
            ...(args.html !== undefined ? { html: args.html } : {}),
            ...(args.reply_all !== undefined ? { replyAll: args.reply_all } : {}),
            ...(args.cc !== undefined ? { cc: args.cc } : {}),
            ...(args.bcc !== undefined ? { bcc: args.bcc } : {}),
            ...(mapAttachments(args.attachments) !== undefined
              ? { attachments: mapAttachments(args.attachments) }
              : {}),
            ...(args.conversation_id !== undefined
              ? { conversationId: args.conversation_id }
              : {}),
            ...(args.reply_to !== undefined ? { replyTo: args.reply_to } : {}),
            ...(args.send_at !== undefined ? { sendAt: new Date(args.send_at) } : {}),
            ...(args.quote_history !== undefined ? { quoteHistory: args.quote_history } : {}),
          },
          opts,
          args.email,
        );
      }),
  );

  server.registerTool(
    "forward_message",
    {
      title: "Forward a message",
      annotations: { destructiveHint: false },
      description:
        "Forward a message the agent has received OR one it sent to one or more new recipients. The server auto-prepends a Gmail-style header block (From/Date/Subject/To/Cc) and the original body to whatever optional comment you pass in `text`/`html`, **and carries over the original message's attachments by default** — you do NOT need to re-fetch them via `get_attachment`. Anything you pass in `attachments[]` is added on top of the originals. **Unlike `reply_to_message`, a forward is a NEW thread** — no In-Reply-To / References headers are emitted, so the recipient sees a fresh conversation. Use this when the user asks to share an email with someone else; use `reply_to_message` when continuing the existing conversation. Same review behavior as send/reply: **`accepted`, `scheduled`, and `pending_review` are all success, not failure — do NOT re-send.** `accepted` means the forward was durably persisted and queued for immediate submission; `scheduled` means it was durably queued for future submission at `scheduled_at`; `pending_review` means a human review hold caught it first. The terminal outcome arrives later via webhook events (`email.sent` / `email.failed`) or by polling `get_message`, not by retrying.",
      inputSchema: strictInputSchema({
        message_id: z.string().describe("ID of the message to forward — inbound or one the agent sent (e.g. msg_…)."),
        to: z.array(z.string()).describe("Forward target addresses (one or more)."),
        cc: z.array(z.string()).optional(),
        bcc: z.array(z.string()).optional(),
        text: z
          .string()
          .optional()
          .describe(
            "Optional plain-text comment to prepend above the forwarded content. The original body is appended automatically.",
          ),
        html: z.string().optional(),
        attachments: attachmentsArraySchema,
        conversation_id: z
          .string()
          .optional()
          .describe(
            "Optional application conversation/grouping ID. A forward always starts a new email thread; setting this value only groups it with related application activity. Maximum 200 characters; no CR/LF.",
          ),
        reply_to: z
          .union([z.string(), z.array(z.string()).max(5)])
          .optional()
          .describe(
            "Sets the Reply-To header — where replies to the forward are directed. Either a single address (optionally with a display name) or an array of up to 5 addresses to direct replies to several destinations. Defaults to the sending agent's own address.",
          ),
        idempotency_key: z
          .string()
          .optional()
          .describe(
            "Stable key for retry-safe forwards. The inbound `message_id` plus target list is a natural choice.",
          ),
        send_at: sendAtField,
        email: emailSelector,
      }),
    },
    async (args) =>
      runTool(() => {
        const opts: SendOpts =
          args.idempotency_key !== undefined
            ? { idempotencyKey: args.idempotency_key }
            : {};
        return client.forward(
          args.message_id,
          args.to,
          {
            // text is required on the wire (MSG-3); the original is auto-quoted,
            // so an empty comment is fine — default to "".
            text: args.text ?? "",
            ...(args.html !== undefined ? { html: args.html } : {}),
            ...(args.cc !== undefined ? { cc: args.cc } : {}),
            ...(args.bcc !== undefined ? { bcc: args.bcc } : {}),
            ...(mapAttachments(args.attachments) !== undefined
              ? { attachments: mapAttachments(args.attachments) }
              : {}),
            ...(args.conversation_id !== undefined
              ? { conversationId: args.conversation_id }
              : {}),
            ...(args.reply_to !== undefined ? { replyTo: args.reply_to } : {}),
            ...(args.send_at !== undefined ? { sendAt: new Date(args.send_at) } : {}),
          },
          opts,
          args.email,
        );
      }),
  );

  server.registerTool(
    "update_message_labels",
    {
      title: "Add or remove labels on an inbound message",
      annotations: { idempotentHint: true, destructiveHint: false },
      description:
        "Apply a labels delta — `add_labels` and/or `remove_labels`. Labels are lowercase strings drawn from `[a-z0-9:_-]+`, capped at 64 chars each; the `e2a:` prefix is reserved for server-applied system labels and rejected on writes. A label appearing in both lists is removed (remove wins). Per-request cap is 50 entries per list; per-message cap is 100 total labels. The response includes the post-update label set so you can echo back to the user without a follow-up read. Use this when the user wants to categorize a message (e.g. `add: urgent`) or clear a tag (`remove: follow-up`).",
      inputSchema: strictInputSchema({
        message_id: z.string().describe("ID of the message to label."),
        add_labels: z
          .array(z.string())
          .optional()
          .describe("Labels to add. Already-set entries are no-ops."),
        remove_labels: z
          .array(z.string())
          .optional()
          .describe("Labels to remove. Entries not on the message are no-ops."),
        email: emailSelector,
      }),
    },
    async (args) =>
      runTool(() =>
        client.updateMessageLabels(
          args.message_id,
          {
            ...(args.add_labels !== undefined ? { addLabels: args.add_labels } : {}),
            ...(args.remove_labels !== undefined ? { removeLabels: args.remove_labels } : {}),
          },
          args.email,
        ),
      ),
  );

  server.registerTool(
    "list_conversations",
    {
      title: "List conversations for the agent",
      annotations: { readOnlyHint: true },
      description:
        "Lists the agent's application conversations — groups of messages sharing a `conversation_id` — one row per group, sorted by most recent activity. `conversation_id` is independent of email thread topology. Each row carries `message_count`, `inbound_count`, `outbound_count`, `has_unread`, and the latest message's subject + sender so you can render grouped activity without loading every message. **Cursor-paginated:** returns one page in `conversations` plus a `next_cursor` when more remain — pass it back as `cursor` for the next page. To read a single conversation's messages, call `get_conversation`.",
      inputSchema: strictInputSchema({
        ...paginationInput,
        since: z
          .string()
          .optional()
          .describe(
            "RFC3339 timestamp. Only conversations whose latest message is >= since.",
          ),
        until: z
          .string()
          .optional()
          .describe(
            "RFC3339 timestamp. Only conversations whose latest message is < until.",
          ),
        email: emailSelector,
      }),
    },
    async (args) =>
      runTool(async () => {
        const page = await client.listConversations(
          {
            ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
            ...(args.limit !== undefined ? { limit: args.limit } : {}),
            ...(args.since !== undefined ? { since: args.since } : {}),
            ...(args.until !== undefined ? { until: args.until } : {}),
          },
          args.email,
        );
        return { conversations: page.items, ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}) };
      }),
  );

  server.registerTool(
    "get_conversation",
    {
      title: "Get an application conversation with all member messages",
      annotations: { readOnlyHint: true },
      description:
        "Returns the full application conversation group — aggregate counts, the participants union (sender + recipient + to + cc + bcc across members), the labels union, and every live member message in chronological order (oldest first). This groups by `conversation_id`, which is independent of email thread topology. Returns a not-found error when no live messages exist for `(agent, conversation_id)`. Use this after `list_conversations` (or whenever you have a `conversation_id` from an inbound/outbound payload) to read the full group.",
      inputSchema: strictInputSchema({
        conversation_id: z.string(),
        email: emailSelector,
      }),
    },
    async (args) =>
      runTool(() => client.getConversation(args.conversation_id, args.email)),
  );

  server.registerTool(
    "list_messages",
    {
      title: "List messages (inbox or sent)",
      annotations: { readOnlyHint: true },
      description:
        "List the agent's messages, newest first by default. Use `direction` to pick the folder: `inbound` (the Inbox — received mail, the default), `outbound` (the Sent folder — mail this agent sent, including held drafts), or `all` (both). Filter received mail by `read_status` (unread/read/all; default unread — applies to inbound only; sent mail has no read-state). **Cursor-paginated:** returns one page in `messages` plus a `next_cursor` when more remain — pass it back as `cursor` for the next page (keep the same filters + sort). Pass `sort: \"asc\"` for FIFO order (oldest first) to drain in arrival order. **Search filters** (`from_`, `subject_contains`, `conversation_id`, `since`, `until`, `filter` (boolean expression)) narrow server-side — use them instead of paging the whole folder. In inbound summaries, `header_from` is the claimed RFC 5322 From address; a non-null `verified_domain` means DMARC passed for that From domain. It does not authenticate the mailbox local part, a person, or message content. Returns summaries only — use `get_message` for the full body and SPF/DKIM/DMARC evidence.",
      inputSchema: strictInputSchema({
        direction: z
          .enum(["inbound", "outbound", "all"])
          .optional()
          .describe(
            "Which folder to list: `inbound` = Inbox (received, default), `outbound` = Sent (this agent's sent mail + held drafts), `all` = both.",
          ),
        read_status: z.enum(["unread", "read", "all"]).optional(),
        ...paginationInput,
        sort: z
          .enum(["asc", "desc"])
          .optional()
          .describe(
            "Sort order by created_at. Defaults to `desc` (newest first). Pass `asc` for FIFO polling — drain the inbox in arrival order. Switching sort mid-pagination rejects the existing cursor.",
          ),
        from_: z
          .string()
          .max(200)
          .optional()
          .describe(
            "Case-insensitive substring on the claimed RFC 5322 From address. This is not an authenticated-sender filter; inspect `verified_domain` or full DMARC evidence before trusting the domain. Example: `acme.com` matches claimed addresses at `*@acme.com`.",
          ),
        subject_contains: z
          .string()
          .max(200)
          .optional()
          .describe(
            "Case-insensitive substring on the subject line. Example: `invoice` matches `Invoice #123` and `Your invoice`.",
          ),
        conversation_id: z
          .string()
          .max(200)
          .optional()
          .describe("Exact match on the application conversation/grouping id."),
        since: z
          .string()
          .optional()
          .describe(
            "RFC3339 timestamp. Only messages with `created_at >= since` are returned. Example: `2026-05-25T00:00:00Z`.",
          ),
        until: z
          .string()
          .optional()
          .describe(
            "RFC3339 timestamp. Only messages with `created_at < until` are returned. Combine with `since` to bracket a date range.",
          ),
        labels: z
          .array(z.string())
          .optional()
          .describe(
            "AND-match filter on labels. A row is returned only if ALL given labels are present. Use lowercase strings matching `[a-z0-9:_-]+`; `e2a:*` system labels can be filtered even though setting them is server-only.",
          ),
        deleted: z
          .boolean()
          .optional()
          .describe("List the message trash instead of live messages."),
        filter: z
          .string()
          .refine((value) => [...value].length <= 500, {
            message: "filter must contain at most 500 Unicode code points",
          })
          .optional()
          .describe(
            "Boolean filter expression (AIP-160-derived). v1 fields: label, from, subject, created. Operators : = != < <= > >= with AND/OR/NOT and parentheses; whitespace is implicit AND (binds looser than OR). Example: 'label:urgent OR (from:alerts AND NOT subject:newsletter) created>=2026-07-01'. Composes (AND) with the flat filters (labels, from_, subject_contains, since, until). Invalid expressions are rejected with a positioned invalid_filter error.",
          ),
        email: emailSelector,
      }),
    },
    async (args) =>
      runTool(async () => {
        const page = await client.listMessages({
          ...(args.direction !== undefined ? { direction: args.direction } : {}),
          ...(args.read_status !== undefined ? { readStatus: args.read_status } : {}),
          ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
          ...(args.limit !== undefined ? { limit: args.limit } : {}),
          ...(args.sort !== undefined ? { sort: args.sort } : {}),
          ...(args.from_ !== undefined ? { from_: args.from_ } : {}),
          ...(args.subject_contains !== undefined
            ? { subjectContains: args.subject_contains }
            : {}),
          ...(args.conversation_id !== undefined
            ? { conversationId: args.conversation_id }
            : {}),
          ...(args.since !== undefined ? { since: args.since } : {}),
          ...(args.until !== undefined ? { until: args.until } : {}),
          ...(args.labels !== undefined ? { labels: args.labels } : {}),
          ...(args.deleted !== undefined ? { deleted: args.deleted } : {}),
          ...(args.filter !== undefined ? { filter: args.filter } : {}),
          ...(args.email !== undefined ? { explicitAddress: args.email } : {}),
        });
        return {
          messages: page.items.map(messageSummaryViewForTool),
          ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}),
        };
      }),
  );

  server.registerTool(
    "restore_message",
    {
      title: "Restore a message from trash",
      annotations: { destructiveHint: false, idempotentHint: false },
      description:
        "Restore a soft-deleted message before its trash-retention window expires. A scheduled message restored before `scheduled_at` re-arms; at/after `scheduled_at` it returns live as failed with submission canceled. Returns the restored message; a live message returns `not_in_trash`.",
      inputSchema: strictInputSchema({
        message_id: z.string().describe("ID of the trashed message to restore."),
        email: emailSelector,
      }),
    },
    async (args) => runTool(() => client.restoreMessage(args.message_id, args.email)),
  );

  server.registerTool(
    "delete_message",
    {
      title: "Delete a message (move to trash)",
      annotations: { destructiveHint: true, idempotentHint: true },
      description:
        "Move a message to the trash. It disappears from lists, threads, and reply targets, but stays restorable with `restore_message` until the trash-retention window expires (30 days by default). Permanent deletion (\"delete forever\") is deliberately NOT exposed here — use the REST API/SDK for that. A message held for review cannot be deleted (`message_held`) — resolve it in the review queue first. Requires `confirm: true` — set it explicitly to acknowledge the destructive action.",
      inputSchema: strictInputSchema({
        message_id: z.string().describe("ID of the message to move to trash."),
        email: emailSelector,
        confirm: z
          .literal(true)
          .describe(
            "Must be set to true to proceed. Guard against an LLM hallucinating a delete from ambiguous context.",
          ),
      }),
    },
    async (args) =>
      runTool(async () => {
        if (args.confirm !== true) {
          throw new Error(
            "delete_message requires confirm:true — refusing to proceed without explicit confirmation.",
          );
        }
        // Return the server's deletion receipt verbatim: {deleted:true, id}.
        return client.deleteMessage(args.message_id, args.email);
      }),
  );

  server.registerTool(
    "get_message",
    {
      title: "Get a message",
      annotations: { readOnlyHint: false },
      description:
        "Use after `list_messages` to read one inbound or outbound message in full; for outbound messages, this is also how you poll a send's terminal outcome. Returns text + HTML, direction, labels, delivery/review lifecycle, suspicious-message flags and protection findings, header_from, envelope_from, verified_domain, SPF/DKIM/DMARC evidence, conversation id, and attachment metadata. `truncated:true` means the inbound parser clipped the decoded body. A non-null verified_domain means DMARC passed for the RFC 5322 From domain; it does not authenticate the mailbox local part, a person, or message content. Pass the message's `id` from the list response. **Side effect:** fetching an unread inbound message marks it read — there is no peek-without-consuming and no mark-unread, so only open a message when you mean to consume it. Attachment bytes and raw MIME are intentionally omitted to protect context; the response lists each attachment's filename, content_type, 0-based `index`, and size_bytes. Call `get_attachment` with that index to fetch one file by reference.",
      inputSchema: strictInputSchema({
        message_id: z.string(),
        email: emailSelector,
      }),
    },
    async (args) =>
      runTool(async () => {
        // McpClient.getMessage resolves the address (explicit arg →
        // pinned default) and throws a directive error when neither is
        // available, so we don't pre-check here.
        const email = await client.getMessage(args.message_id, args.email);
        return messageViewForTool(email);
      }),
  );

  server.registerTool(
    "get_message_lifecycle",
    {
      title: "Get message lifecycle (beta)",
      annotations: { readOnlyHint: true, idempotentHint: true },
      description:
        "Beta: returns the ordered transitions e2a observed for one persisted inbound or outbound message; the lifecycle contract may change before it is declared stable. SMTP acceptance, upstream submission, provider delivery feedback, and complaint feedback remain distinct; this does not claim inbox placement. **Cursor-paginated:** returns one page in `transitions` plus `next_cursor` only when more pages remain; pass it back as `cursor` to continue, and stop when it is absent.",
      inputSchema: strictInputSchema({
        message_id: z.string(),
        email: emailSelector,
        cursor: z.string().optional(),
        limit: z.number().int().min(1).max(100).optional(),
      }),
    },
    async (args) =>
      runTool(async () => {
        const page = await client.getMessageLifecycle(
          args.message_id,
          {
            ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
            ...(args.limit !== undefined ? { limit: args.limit } : {}),
          },
          args.email,
        );
        // Frozen MCP list envelope (see paginationInput in util.ts): a
        // domain-named array + next_cursor OMITTED at the last page — not the
        // raw REST page ({items, next_cursor: null}).
        return {
          transitions: page.items,
          ...(page.nextCursor ? { next_cursor: page.nextCursor } : {}),
        };
      }),
  );

  server.registerTool(
    "get_attachment",
    {
      title: "Get one attachment (download URL; bytes by reference)",
      annotations: { readOnlyHint: true },
      description:
        "Returns one attachment's metadata plus a short-lived `download_url` (+ `expires_at`) — fetch the bytes out of band so binary content never streams through your context (no size limit). `attachment_index` is the 0-based `attachments[].index` from `get_message`. Pass `inline: true` to ALSO get base64 `data` for small attachments (≤256 KB; larger inline requests error) — use this only when you must re-attach the bytes (e.g. forwarding a small file via `send_message`'s `attachments[]`); otherwise hand the `download_url` to whatever needs the file.",
      inputSchema: strictInputSchema({
        message_id: z.string(),
        attachment_index: z
          .number()
          .int()
          .min(0)
          .describe(
            "0-based index from `get_message`'s `attachments[].index` (stable for a given message_id).",
          ),
        inline: z
          .boolean()
          .optional()
          .describe(
            "When true, also include the bytes as base64 `data` — ONLY for attachments ≤256 KB (larger requests error). Default false: use `download_url`.",
          ),
        email: emailSelector,
      }),
    },
    async (args) =>
      runTool(async () => {
        // Server-side: mints the signed URL + (optionally) inlines small bytes;
        // index-out-of-range (404) and inline-too-large (413) surface as the
        // structured error code. No client-side MIME re-parse or size wall.
        const att = await client.getAttachment(
          args.message_id,
          args.attachment_index,
          args.inline ? { inline: true } : {},
          args.email,
        );
        return {
          index: att.index,
          filename: att.filename,
          content_type: att.contentType,
          size_bytes: att.sizeBytes,
          download_url: att.downloadUrl,
          expires_at: att.expiresAt,
          ...(att.data ? { data: att.data } : {}),
        };
      }),
  );
}
