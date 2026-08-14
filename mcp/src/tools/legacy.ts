import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { McpClient, SendOpts } from "../client.js";
import { z } from "zod";
import { attachmentsArraySchema, type AttachmentInput } from "./attachments.js";
import { reviewViewForTool } from "./review.js";
import { runTool, strictInputSchema } from "./util.js";

function mapAttachments(
  attachments?: AttachmentInput[],
): Array<{ filename: string; contentType: string; data: string }> | undefined {
  if (attachments === undefined) return undefined;
  return attachments.map((attachment) => ({
    filename: attachment.filename,
    contentType: attachment.content_type,
    data: attachment.data,
  }));
}

type ReviewOverrides = {
  subject?: string;
  text?: string;
  html?: string;
  to?: string[];
  cc?: string[];
  bcc?: string[];
  attachments?: AttachmentInput[];
};

function mapReviewOverrides(overrides: ReviewOverrides) {
  return {
    ...(overrides.subject !== undefined ? { subject: overrides.subject } : {}),
    ...(overrides.text !== undefined ? { text: overrides.text } : {}),
    ...(overrides.html !== undefined ? { html: overrides.html } : {}),
    ...(overrides.to !== undefined ? { to: overrides.to } : {}),
    ...(overrides.cc !== undefined ? { cc: overrides.cc } : {}),
    ...(overrides.bcc !== undefined ? { bcc: overrides.bcc } : {}),
    ...(mapAttachments(overrides.attachments) !== undefined
      ? { attachments: mapAttachments(overrides.attachments) }
      : {}),
  };
}

/**
 * Register frozen v1 compatibility names. These adapters intentionally retain
 * their historical schemas; new callers should use the canonical tools named
 * in each description.
 */
export function registerLegacyTools(server: McpServer, client: McpClient): void {
  server.registerTool(
    "send_email",
    {
      title: "Deprecated alias: send_message",
      annotations: { destructiveHint: false },
      description:
        "DEPRECATED — use `send_message` instead. Sends a new email from the agent's inbox to one or more recipients, identical to `send_message` except that it keeps the historical field names (`body`/`html_body`/`agent_email` rather than `text`/`html`/`email`). It exists only for MCP clients pinned to the old schema and gains no new features: templates and scheduled sending are unavailable here. Like `send_message`, it starts a NEW thread — use `reply_to_message` to respond to a message you can see. **`accepted` and `pending_review` are both success, not failure — do NOT re-send.** `accepted` means the send was durably persisted and queued; `pending_review` means a human review hold caught it first. The terminal outcome arrives later via webhook events or by polling `get_message`, not by retrying.",
      inputSchema: strictInputSchema({
        to: z.array(z.string()).describe("Recipient email addresses (one or more)."),
        subject: z.string().describe("Subject line of the new message."),
        body: z.string().describe("Plain-text body. Use html_body for HTML."),
        html_body: z
          .string()
          .optional()
          .describe(
            "Optional HTML body, sent alongside `body` as a multipart alternative. `body` stays required — it is what recipients with HTML disabled receive. (`send_message` calls this field `html`.)",
          ),
        cc: z
          .array(z.string())
          .optional()
          .describe("Carbon-copy addresses. Visible to every recipient."),
        bcc: z
          .array(z.string())
          .optional()
          .describe(
            "Blind-carbon-copy addresses. NOT visible to any other recipient, and absent from the headers the recipient sees.",
          ),
        attachments: attachmentsArraySchema,
        conversation_id: z
          .string()
          .optional()
          .describe(
            "Optional stable conversation grouping ID; reuse it across related sends so e2a grouping follows your runtime's own thread. Maximum 200 characters; no CR/LF. Server generates one if omitted.",
          ),
        idempotency_key: z
          .string()
          .optional()
          .describe(
            "Stable key for retry-safe sends. Set it to deduplicate when the caller has its own retry loop; omit to let the SDK mint a fresh UUIDv4 per call, which protects against network-layer retries only.",
          ),
        agent_email: z
          .string()
          .optional()
          .describe(
            "Sending agent's inbox (full email address). REQUIRED when the credential is account-scoped, since such a credential owns many agents and has no bound agent to default to. Defaults to the bound agent for agent-scoped credentials (omit it there). (`send_message` calls this field `email`.)",
          ),
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
            subject: args.subject,
            text: args.body,
            ...(args.html_body !== undefined ? { html: args.html_body } : {}),
            ...(args.cc !== undefined ? { cc: args.cc } : {}),
            ...(args.bcc !== undefined ? { bcc: args.bcc } : {}),
            ...(mapAttachments(args.attachments) !== undefined
              ? { attachments: mapAttachments(args.attachments) }
              : {}),
            ...(args.conversation_id !== undefined
              ? { conversationId: args.conversation_id }
              : {}),
          },
          opts,
          args.agent_email,
        );
      }),
  );

  server.registerTool(
    "get_attachment_data",
    {
      title: "Deprecated alias: get_attachment",
      annotations: { readOnlyHint: true },
      description:
        "DEPRECATED — use `get_attachment` with `inline: true` instead. Returns one attachment's metadata plus its bytes as base64 `data`, always inline. It exists only for MCP clients pinned to the old schema, and it is strictly weaker than `get_attachment`: it cannot return a short-lived `download_url`, so every fetch streams the whole file through your context, and attachments over the 256 KB inline limit fail outright rather than falling back to a URL. Prefer `get_attachment` and hand the `download_url` to whatever needs the file.",
      inputSchema: strictInputSchema({
        message_id: z
          .string()
          .describe("ID of the message the attachment belongs to (e.g. msg_…)."),
        attachment_index: z
          .number()
          .int()
          .min(0)
          .describe(
            "0-based index from `get_message`'s `attachments[].index` (stable for a given message_id).",
          ),
        agent_email: z
          .string()
          .optional()
          .describe(
            "Owning agent's inbox (full email address). REQUIRED when the credential is account-scoped; defaults to the bound agent for agent-scoped credentials. (`get_attachment` calls this field `email`.)",
          ),
      }),
    },
    async (args) =>
      runTool(async () => {
        const attachment = await client.getAttachment(
          args.message_id,
          args.attachment_index,
          { inline: true },
          args.agent_email,
        );
        if (!attachment.data) {
          throw new Error("inline attachment response omitted base64 data");
        }
        return {
          filename: attachment.filename,
          content_type: attachment.contentType,
          size_bytes: attachment.sizeBytes,
          data: attachment.data,
        };
      }),
  );

  server.registerTool(
    "list_pending_messages",
    {
      title: "Deprecated alias: list_reviews",
      annotations: { readOnlyHint: true },
      description:
        "DEPRECATED: use `list_reviews`. This account-only compatibility alias walks the unified review queue and preserves the historical messages response envelope.",
      inputSchema: strictInputSchema({}),
    },
    async () =>
      runTool(async () => {
        const messages: unknown[] = [];
        let cursor: string | undefined;
        do {
          const page = await client.listReviews({
            ...(cursor !== undefined ? { cursor } : {}),
            limit: 100,
          });
          messages.push(...page.items);
          cursor = page.next_cursor ?? undefined;
        } while (cursor !== undefined);
        return { messages };
      }),
  );

  server.registerTool(
    "get_pending_message",
    {
      title: "Deprecated alias: get_review",
      annotations: { readOnlyHint: true },
      description:
        "DEPRECATED: use `get_review`. This account-only compatibility alias accepts the historical pending message ID and returns canonical review detail.",
      inputSchema: strictInputSchema({
        message_id: z.string(),
      }),
    },
    // Same context-safe projection as get_review: raw_message and attachment
    // bytes stay out of the model's context; hold_reason is kept (it is the
    // review surface's primary hold explanation).
    async (args) =>
      runTool(async () => reviewViewForTool(await client.getReview(args.message_id))),
  );

  server.registerTool(
    "approve_pending_message",
    {
      title: "Deprecated alias: approve_review",
      annotations: { destructiveHint: false },
      description:
        "DEPRECATED: use `approve_review`. This account-only compatibility alias preserves the historical body_text/body_html reviewer override fields.",
      inputSchema: strictInputSchema({
        message_id: z.string(),
        subject: z.string().optional(),
        body_text: z.string().optional(),
        body_html: z.string().optional(),
        to: z.array(z.string()).optional(),
        cc: z.array(z.string()).optional(),
        bcc: z.array(z.string()).optional(),
        attachments: attachmentsArraySchema,
        idempotency_key: z.string().optional(),
      }),
    },
    async (args) => {
      const { message_id, idempotency_key, body_text, body_html, ...rest } = args;
      const overrides = mapReviewOverrides({
        ...rest,
        ...(body_text !== undefined ? { text: body_text } : {}),
        ...(body_html !== undefined ? { html: body_html } : {}),
      });
      return runTool(() =>
        idempotency_key !== undefined
          ? client.approveReview(message_id, overrides, { idempotencyKey: idempotency_key })
          : client.approveReview(message_id, overrides),
      );
    },
  );

  server.registerTool(
    "approve_message",
    {
      title: "Deprecated alias: approve_review",
      annotations: { destructiveHint: false },
      description:
        "DEPRECATED: use `approve_review`. This account-only compatibility alias preserves the previous approve_message name and override fields.",
      inputSchema: strictInputSchema({
        message_id: z.string(),
        subject: z.string().optional(),
        text: z.string().optional(),
        html: z.string().optional(),
        to: z.array(z.string()).optional(),
        cc: z.array(z.string()).optional(),
        bcc: z.array(z.string()).optional(),
        attachments: attachmentsArraySchema,
        idempotency_key: z.string().optional(),
      }),
    },
    async (args) => {
      const { message_id, idempotency_key, ...rest } = args;
      const overrides = mapReviewOverrides(rest);
      return runTool(() =>
        idempotency_key !== undefined
          ? client.approveReview(message_id, overrides, { idempotencyKey: idempotency_key })
          : client.approveReview(message_id, overrides),
      );
    },
  );

  for (const [name, replacement] of [
    ["reject_pending_message", "reject_review"],
    ["reject_message", "reject_review"],
  ] as const) {
    server.registerTool(
      name,
      {
        title: `Deprecated alias: ${replacement}`,
        annotations: { destructiveHint: true },
        description: `DEPRECATED: use \`${replacement}\`. This account-only compatibility alias preserves message_id and the optional rejection reason.`,
        inputSchema: strictInputSchema({
          message_id: z.string(),
          reason: z.string().optional(),
        }),
      },
      async (args) => runTool(() => client.rejectReview(args.message_id, args.reason)),
    );
  }
}
