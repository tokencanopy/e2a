import { readFileSync } from "node:fs";
import { basename, extname } from "node:path";
import type { SendResultView, Attachment } from "@e2a/sdk/v1";
import { createClient, requireAgentEmail } from "../sdk.js";
import { EXIT, fail } from "../exit.js";
import { htmlToText } from "../html.js";

export interface SendOptions {
  to: string[];
  subject?: string;
  body?: string;
  bodyFile?: string;
  htmlFile?: string;
  conversationId?: string;
  replyTo?: string;
  agent?: string;
  json?: boolean;
  idempotencyKey?: string;
  attach?: string[];
  sendAt?: string;
}

export interface ReplyOptions {
  body?: string;
  bodyFile?: string;
  htmlFile?: string;
  replyTo?: string;
  agent?: string;
  json?: boolean;
  idempotencyKey?: string;
  attach?: string[];
  sendAt?: string;
}

const MIME_BY_EXT: Record<string, string> = {
  ".pdf": "application/pdf",
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".gif": "image/gif",
  ".txt": "text/plain",
  ".md": "text/markdown",
  ".html": "text/html",
  ".json": "application/json",
  ".csv": "text/csv",
  ".zip": "application/zip",
};

function readAttachments(paths: string[] | undefined): Attachment[] | undefined {
  if (!paths || paths.length === 0) return undefined;
  return paths.map((p) => {
    let buf: Buffer;
    try {
      buf = readFileSync(p);
    } catch {
      return fail(EXIT.USAGE, `--attach file not found or unreadable: ${p}`);
    }
    return {
      filename: basename(p),
      contentType: MIME_BY_EXT[extname(p).toLowerCase()] ?? "application/octet-stream",
      data: buf.toString("base64"),
    };
  });
}

const SEND_USAGE =
  "usage: e2a send --to <email> --subject <s> (--body <text> | --body-file <f> | --html-file <f>) [--conversation-id <id>] [--reply-to <email>] [--send-at <rfc3339>] [--agent <inbox>] [--json]";
const REPLY_USAGE =
  "usage: e2a reply <message-id> (--body <text> | --body-file <f> | --html-file <f>) [--reply-to <email>] [--send-at <rfc3339>] [--agent <inbox>] [--json]";

/**
 * Parse the optional --send-at flag into a Date for scheduled send. Accepts any
 * value the Date constructor understands; an RFC 3339 timestamp with an explicit
 * offset (e.g. 2026-08-01T09:00:00Z) is recommended so the instant is
 * unambiguous. A value at or before now sends immediately (the server treats a
 * past instant as "now"); a value more than 90 days ahead is rejected server-
 * side. An unparseable value is a local usage error. Returns undefined when the
 * flag is absent (an immediate send).
 */
export function parseSendAt(value: string | undefined, usage: string): Date | undefined {
  if (value === undefined) return undefined;
  // Require an explicit UTC offset (Z or ±HH:MM), matching the MCP tool's strict
  // RFC 3339 rule. Without it, `new Date()` reads a bare date-time as LOCAL time
  // and a date-only value as UTC midnight — silently shifting the intended send
  // instant across timezones.
  const rfc3339WithOffset =
    /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;
  if (!rfc3339WithOffset.test(value)) {
    return fail(
      EXIT.USAGE,
      `--send-at must be an RFC 3339 date-time WITH an explicit offset, e.g. 2026-08-01T09:00:00Z or 2026-08-01T09:00:00-07:00 (got "${value}")\n${usage}`,
    );
  }
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) {
    return fail(EXIT.USAGE, `--send-at is not a valid date-time: "${value}" (use RFC 3339, e.g. 2026-08-01T09:00:00Z)\n${usage}`);
  }
  return at;
}

function readFileOrUsage(path: string, flag: string): string {
  try {
    return readFileSync(path, "utf-8");
  } catch {
    return fail(EXIT.USAGE, `${flag} file not found or unreadable: ${path}`);
  }
}

/**
 * Resolve the text body + optional HTML body from the flag combination.
 * `--html-file` without an explicit text body derives a plain-text fallback
 * from the HTML, so multipart sends never ship an empty text alternative.
 */
export function resolveBodies(
  opts: { body?: string; bodyFile?: string; htmlFile?: string },
  usage: string,
): { body: string; htmlBody?: string } {
  const htmlBody = opts.htmlFile ? readFileOrUsage(opts.htmlFile, "--html-file") : undefined;
  let body = opts.body ?? (opts.bodyFile ? readFileOrUsage(opts.bodyFile, "--body-file") : undefined);
  if (body === undefined && htmlBody !== undefined) body = htmlToText(htmlBody);
  // Only "no body source at all" is a usage error. An empty string is legal:
  // markup-only HTML (images/tables, no text nodes) derives "" and must still
  // send — the HTML part is the real content.
  if (body === undefined) fail(EXIT.USAGE, usage);
  return { body, htmlBody };
}

/**
 * Print the send result and enforce the held contract: a held send is an HTTP
 * success with `status: "pending_review"` — the recipient got NOTHING. Scripts
 * must be able to branch on that without parsing JSON, hence exit HELD (3).
 */
function emitSendResult(result: SendResultView, json?: boolean): void {
  if (json) {
    process.stdout.write(JSON.stringify(result) + "\n");
  } else {
    process.stdout.write(result.messageId + "\n");
  }
  if (result.status === "pending_review") {
    process.stderr.write(
      "WARNING: send returned status \"pending_review\" — the message did NOT go out now. " +
        "It is held for approval: disable outbound protection on this " +
        "agent or approve it in the review queue.\n",
    );
    // exitCode + return, NOT process.exit(): a hard exit can truncate piped
    // stdout before the message id above flushes, and scripts need that id to
    // approve the held message.
    process.exitCode = EXIT.HELD;
  } else if (result.status === "scheduled") {
    // Scheduled is a SUCCESSFUL, intended acceptance (exit 0): the message is
    // durably queued to go out at send_at, not held by mistake. Note it so a
    // human sees the deferral, and point at how to cancel.
    const when = result.scheduledAt ? new Date(result.scheduledAt).toISOString() : "the requested time";
    process.stderr.write(
      `NOTE: send accepted as "scheduled" — it will be submitted at ${when} (not before), not now. ` +
        `Cancel it before then by moving message ${result.messageId} to trash (via the dashboard or the delete-message API).\n`,
    );
  } else if (result.status === "failed") {
    process.stderr.write(
      `WARNING: send reached terminal status "failed" for message "${result.messageId}". ` +
        `The server persisted the failure outcome; do NOT retry automatically. ` +
        `Inspect it with: e2a messages get ${result.messageId}\n`,
    );
    process.exitCode = EXIT.SEND_OUTCOME;
  } else if (result.status !== "sent" && result.status !== "accepted") {
    // Response status values are an open set. Do not silently report an
    // unfamiliar outcome as success, but do not misclassify it as a known
    // review hold or a retryable transport error either: this successful API
    // response carries a message id and may already be durably persisted.
    process.stderr.write(
      `WARNING: send returned unrecognized status "${result.status}" for message "${result.messageId}". ` +
        `The message may already be durably persisted; do NOT retry automatically. ` +
        `Inspect it with: e2a messages get ${result.messageId}\n`,
    );
    process.exitCode = EXIT.SEND_OUTCOME;
  }
}

export async function send(opts: SendOptions): Promise<void> {
  if (opts.to.length === 0 || !opts.subject) fail(EXIT.USAGE, SEND_USAGE);
  const { body, htmlBody } = resolveBodies(opts, SEND_USAGE);

  const client = createClient();
  const agentEmail = requireAgentEmail(opts.agent);
  const sendAt = parseSendAt(opts.sendAt, SEND_USAGE);
  // Optional fields may be undefined — the generated serializer drops
  // undefined-valued keys before they reach the wire. An explicit
  // --idempotency-key makes a *re-invocation* after an ambiguous failure
  // dedupe server-side (the SDK's auto-minted key only survives in-process
  // retries).
  const result = await client.messages.send(
    agentEmail,
    {
      to: opts.to,
      subject: opts.subject,
      text: body,
      html: htmlBody,
      conversationId: opts.conversationId,
      replyTo: opts.replyTo,
      attachments: readAttachments(opts.attach),
      sendAt,
    },
    opts.idempotencyKey ? { idempotencyKey: opts.idempotencyKey } : undefined,
  );
  emitSendResult(result, opts.json);
}

export async function reply(messageId: string | undefined, opts: ReplyOptions): Promise<void> {
  if (!messageId) fail(EXIT.USAGE, REPLY_USAGE);
  const { body, htmlBody } = resolveBodies(opts, REPLY_USAGE);

  const client = createClient();
  const agentEmail = requireAgentEmail(opts.agent);
  const sendAt = parseSendAt(opts.sendAt, REPLY_USAGE);
  const result = await client.messages.reply(
    agentEmail,
    messageId,
    { text: body, html: htmlBody, replyTo: opts.replyTo, attachments: readAttachments(opts.attach), sendAt },
    opts.idempotencyKey ? { idempotencyKey: opts.idempotencyKey } : undefined,
  );
  emitSendResult(result, opts.json);
}
