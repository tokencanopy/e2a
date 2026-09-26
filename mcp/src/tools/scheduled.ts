import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { McpClient } from "../client.js";
import { paginationInput, runTool, strictInputSchema } from "./util.js";

// The scheduled-send queue is the account-only, read-only companion to the
// review queue: outbound messages accepted and waiting for a future send_at to
// fire. It is deliberately separate from list_reviews — a held draft is not yet
// accepted and appears there, not here — and it carries no approve/reject
// action, because a scheduled send is not a hold.
export function registerScheduledTools(server: McpServer, client: McpClient): void {
  server.registerTool(
    "list_scheduled_messages",
    {
      title: "List messages awaiting a scheduled send",
      annotations: { readOnlyHint: true },
      description:
        "Account scope only. Use when the account owner asks what outbound mail is queued to go out later. Lists every outbound message that was accepted with a send_at and has not fired yet, across every inbox in the authenticated account, soonest-first. This includes overdue-but-pending sends — a scheduled_at in the past means the send is still queued but its fire time has passed (for example, deferred by the account's daily send cap), surfaced here rather than hidden until it fires. Each item carries the target recipients, subject, and the scheduled_at instant; delivery_status stays `accepted` until the send fires (scheduling introduces no new status). These are NOT holds — there is nothing to approve or reject; a message drops off this list automatically once it sends (or if it is canceled). Held drafts that also carry a schedule appear in `list_reviews`, not here. **Cursor-paginated:** returns one page in `scheduled` plus `next_cursor` only when more remain; pass it back as `cursor`. Read-only; don't poll it on a loop.",
      inputSchema: strictInputSchema({ ...paginationInput }),
    },
    async (args) =>
      runTool(async () => {
        const page = await client.listScheduledMessages({
          ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
          ...(args.limit !== undefined ? { limit: args.limit } : {}),
        });
        return {
          scheduled: page.items,
          ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}),
        };
      }),
  );
}
