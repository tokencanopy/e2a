import { readFileSync } from "node:fs";
import type { SendBatchRequest, SendBatchResponse, BatchView } from "@e2a/sdk/v1";
import { createClient, requireAgentEmail } from "../sdk.js";
import { EXIT, fail } from "../exit.js";

export interface BatchSendOptions {
  file?: string;
  agent?: string;
  json?: boolean;
  idempotencyKey?: string;
}

export interface BatchGetOptions {
  json?: boolean;
}

export const BATCH_SEND_USAGE =
  "usage: e2a batch send --file <path|-> [--agent <inbox>] [--idempotency-key <k>] [--json]";
export const BATCH_GET_USAGE = "usage: e2a batch get <batch-id> [--json]";

/**
 * Read the batch request body from a JSON file (or stdin with `-`). The file
 * is the SendBatchRequest shape: a JSON object with a non-empty `messages`
 * array, each item a message (to, subject, text/html, cc, bcc, attachments,
 * conversationId, replyTo, template*). Field names are the SDK's camelCase
 * form, mirroring what `e2a send` accepts per item. See
 * docs/design/batch-send.md for the caps (≤100 items, 60 MiB aggregate).
 */
function readBatchRequest(file: string | undefined): SendBatchRequest {
  if (!file) return fail(EXIT.USAGE, BATCH_SEND_USAGE);
  let raw: string;
  try {
    // fd 0 is stdin, so `--file -` pipes a batch in from a generator script.
    raw = file === "-" ? readFileSync(0, "utf-8") : readFileSync(file, "utf-8");
  } catch {
    return fail(EXIT.USAGE, `--file not found or unreadable: ${file}`);
  }
  let body: unknown;
  try {
    body = JSON.parse(raw);
  } catch (e) {
    return fail(EXIT.USAGE, `--file is not valid JSON: ${(e as Error).message}`);
  }
  const messages = (body as { messages?: unknown } | null)?.messages;
  if (typeof body !== "object" || body === null || !Array.isArray(messages) || messages.length === 0) {
    return fail(
      EXIT.USAGE,
      `--file must be a JSON object with a non-empty "messages" array (see docs/design/batch-send.md)`,
    );
  }
  return body as SendBatchRequest;
}

/**
 * Print the batch accept result. A batch is always accepted async (202); the
 * per-item results are positionally aligned to the input. Suppressed items are
 * a compliance drop, not a send failure, so the batch still exits 0 — the
 * per-item lines and the stderr summary surface the drops for a human/script.
 */
function emitBatchSendResult(result: SendBatchResponse, json?: boolean): void {
  if (json) {
    process.stdout.write(JSON.stringify(result) + "\n");
    return;
  }
  process.stdout.write(result.batchId + "\n");
  result.results.forEach((r, i) => {
    if (r.status === "accepted") {
      process.stdout.write(`${i}\taccepted\t${r.messageId ?? ""}\n`);
    } else {
      process.stdout.write(`${i}\tsuppressed\t${r.suppressed?.address ?? ""}\t${r.suppressed?.reason ?? ""}\n`);
    }
  });
  process.stderr.write(
    `batch ${result.batchId}: accepted=${result.accepted} suppressed=${result.suppressedCount}\n`,
  );
}

export async function batchSend(opts: BatchSendOptions): Promise<void> {
  const body = readBatchRequest(opts.file);
  const client = createClient();
  const agentEmail = requireAgentEmail(opts.agent);
  const result = await client.messages.sendBatch(
    agentEmail,
    body,
    opts.idempotencyKey ? { idempotencyKey: opts.idempotencyKey } : undefined,
  );
  emitBatchSendResult(result, opts.json);
}

function emitBatchView(v: BatchView): void {
  process.stdout.write(`batch ${v.batchId} (agent ${v.agentId}, created ${v.createdAt})\n`);
  process.stdout.write(`requested=${v.requested} accepted=${v.accepted} suppressed=${v.suppressed.length}\n`);
  const r = v.statusRollup;
  process.stdout.write(
    `rollup: accepted=${r.accepted} sending=${r.sending} sent=${r.sent} delivered=${r.delivered} ` +
      `deferred=${r.deferred} bounced=${r.bounced} complained=${r.complained} failed=${r.failed}\n`,
  );
  for (const s of v.suppressed) {
    process.stdout.write(`suppressed\t${s.itemIndex}\t${s.address}\t${s.reason}\n`);
  }
}

export async function batchGet(batchId: string | undefined, opts: BatchGetOptions): Promise<void> {
  if (!batchId) fail(EXIT.USAGE, BATCH_GET_USAGE);
  const client = createClient();
  const view = await client.messages.getBatch(batchId);
  if (opts.json) {
    process.stdout.write(JSON.stringify(view) + "\n");
    return;
  }
  emitBatchView(view);
}
