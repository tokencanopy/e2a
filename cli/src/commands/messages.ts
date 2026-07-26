import type { ListMessagesParams } from "@e2a/sdk/v1";
import { createClient, requireAgentEmail } from "../sdk.js";
import { EXIT, fail } from "../exit.js";

export interface MessagesListOptions {
  agent?: string;
  direction?: string;
  since?: string;
  q?: string;
  conversation?: string;
  limit?: string;
  readStatus?: string;
  json?: boolean;
}

export interface MessagesGetOptions {
  agent?: string;
  text?: boolean;
  json?: boolean;
}

export interface MessagesLifecycleOptions {
  agent?: string;
  cursor?: string;
  limit?: string;
  json?: boolean;
}

const LIST_USAGE =
  "usage: e2a messages list [--direction inbound|outbound|all] [--since <ISO>] [--q <expr>] [--conversation <id>] [--read-status unread|read|all] [--limit <n>] [--agent <inbox>] [--json]";
const GET_USAGE = "usage: e2a messages get <message-id> [--text] [--agent <inbox>] [--json]";
const LIFECYCLE_USAGE =
  "usage: e2a messages lifecycle <message-id> (beta) [--agent <inbox>] [--limit <1-100>] [--cursor <cursor>] [--json]";

const DIRECTIONS = ["inbound", "outbound", "all"] as const;
const READ_STATUSES = ["unread", "read", "all"] as const;

// The server pages at 100 by default, which is also the max; always ask for the
// max so draining a mailbox costs the fewest round trips.
const MAX_PAGE_SIZE = 100;

// Default output is TSV (id, header_from, created_at) in ascending order —
// the shape a shell poll loop wants (`while IFS=$'\t' read -r id header_from at`).
// --json emits one full summary object per line (NDJSON), in the SDK's
// camelCase model shape like `listen --json`.
export async function messagesList(opts: MessagesListOptions): Promise<void> {
  // readStatus defaults to "all", NOT the server default. The server defaults
  // inbound lists to unread-only, and `messages get` (and any other consumer)
  // marks a message read on fetch — so a list→get→list poll loop would
  // silently lose every message it had already touched. tether's curl layer
  // learned this the hard way (read_status=all); the CLI must not regress it.
  const params: ListMessagesParams = { sort: "asc", readStatus: "all" };

  if (opts.direction) {
    if (!(DIRECTIONS as readonly string[]).includes(opts.direction)) fail(EXIT.USAGE, LIST_USAGE);
    params.direction = opts.direction as (typeof DIRECTIONS)[number];
  }
  if (opts.readStatus) {
    if (!(READ_STATUSES as readonly string[]).includes(opts.readStatus)) {
      fail(EXIT.USAGE, LIST_USAGE);
    }
    params.readStatus = opts.readStatus as (typeof READ_STATUSES)[number];
  }
  if (opts.since) params.since = opts.since;
  if (opts.q !== undefined) params.q = opts.q;
  if (opts.conversation) params.conversationId = opts.conversation;

  let max: number | undefined;
  if (opts.limit !== undefined) {
    max = Number(opts.limit);
    if (!Number.isInteger(max) || max <= 0) fail(EXIT.USAGE, LIST_USAGE);
  }
  params.limit = Math.min(max ?? MAX_PAGE_SIZE, MAX_PAGE_SIZE);

  const client = createClient();
  const agentEmail = requireAgentEmail(opts.agent);

  let count = 0;
  for await (const m of client.messages.list(agentEmail, params)) {
    if (opts.json) {
      process.stdout.write(JSON.stringify(withWireFrom(m)) + "\n");
    } else {
      process.stdout.write(
        `${m.id}\t${sanitizeTsvField(m.headerFrom ?? "")}\t${m.createdAt.toISOString()}\n`,
      );
    }
    count++;
    if (max !== undefined && count >= max) break;
  }
}

/**
 * TSV fields must never contain the delimiters. The From header is
 * sender-controlled: a display name with an embedded tab shifts a poll loop's
 * fields (corrupting its cursor) and a newline injects phantom rows.
 */
export function sanitizeTsvField(s: string): string {
  return s.replace(/[\t\r\n]+/g, " ");
}

/**
 * Preserve the SDK model object for CLI JSON output. Kept as a named helper so
 * the listen and messages commands continue sharing one serialization path.
 */
export function withWireFrom(model: object): Record<string, unknown> {
  const obj: Record<string, unknown> = { ...model };
  return obj;
}

export async function messagesGet(
  messageId: string | undefined,
  opts: MessagesGetOptions,
): Promise<void> {
  if (!messageId) fail(EXIT.USAGE, GET_USAGE);
  // JSON is the default output, so --json is accepted as an explicit alias —
  // but combining it with --text is a contradiction, not a precedence puzzle.
  if (opts.text && opts.json) fail(EXIT.USAGE, "--text and --json are mutually exclusive");

  const client = createClient();
  const agentEmail = requireAgentEmail(opts.agent);
  const message = await client.messages.get(agentEmail, messageId);

  if (opts.text) {
    // Parsed text (quoted history stripped) wins when the parse ran; `??` (not
    // ||) so a legitimately-empty parsed result ("" = the reply was ALL quoted
    // history) doesn't fall through to the unstripped raw body. An inbound
    // message that prints "" truly has no textual content — parsing is
    // synchronous server-side, there is no async-parse race to retry. An
    // outbound row can print "" when its retained body is legitimately empty.
    const text = message.parsed?.text ?? message.body?.text ?? "";
    process.stdout.write(text.trim() + "\n");
    return;
  }
  process.stdout.write(JSON.stringify(withWireFrom(message)) + "\n");
}

export async function messagesLifecycle(
  messageId: string | undefined,
  opts: MessagesLifecycleOptions,
): Promise<void> {
  if (!messageId) fail(EXIT.USAGE, LIFECYCLE_USAGE);

  let limit: number | undefined;
  if (opts.limit !== undefined) {
    limit = Number(opts.limit);
    if (!Number.isInteger(limit) || limit < 1 || limit > 100) {
      fail(EXIT.USAGE, LIFECYCLE_USAGE);
    }
  }

  const client = createClient();
  const agentEmail = requireAgentEmail(opts.agent);
  const page = await client.messages.getLifecycle(agentEmail, messageId, {
    ...(opts.cursor !== undefined ? { cursor: opts.cursor } : {}),
    ...(limit !== undefined ? { limit } : {}),
  });
  process.stdout.write(JSON.stringify(withWireFrom(page)) + "\n");
}
