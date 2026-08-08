import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { McpClient } from "../client.js";
import { z } from "zod";
import { runTool, strictInputSchema, paginationInput } from "./util.js";

// Slice 8: MCP tool surfaces for the customer-facing events API.
//   list_events       — paginated listing with filters
//   get_event         — single event lookup
//   redeliver_event   — replay an event to a webhook
//
// These bring the MCP catalog from 18 → 21 tools. The new tools are
// auth-bound to the API key the MCP server was launched with — the
// LLM cannot list events for other accounts.

export function registerEventTools(server: McpServer, client: McpClient): void {
  server.registerTool(
    "list_events",
    {
      title: "List webhook events",
      annotations: { readOnlyHint: true },
      description:
        "List the durable webhook event log in reverse-chronological order. Useful for reconciliation (\"did our webhook receiver see this event?\") and for debugging delivery state. Events past the 30-day retention boundary are not returned. **Cursor-paginated:** returns one page in `events` plus a `next_cursor` when more remain — pass it back as `cursor` to walk further back. Returns each event's `data` payload plus a `delivery_status` summary of how many subscribers have received it.",
      inputSchema: strictInputSchema({
        type: z
          .string()
          .optional()
          .describe(
            "Exact event type filter. Valid values: `email.received`, `email.sent`, `email.failed`, `email.review_requested`, `email.review_approved`, `email.review_rejected`, `email.blocked`, `email.delivered`, `email.bounced`, `email.complained`, `email.flagged`, `domain.sending_verified`, `domain.sending_failed`, `domain.suppression_added`, `agent.suppression_added`, `contact.due`.",
          ),
        agent_email: z.string().optional(),
        conversation_id: z.string().optional(),
        message_id: z.string().optional(),
        since: z
          .string()
          .optional()
          .describe("RFC3339 timestamp; returns events with `created_at >= since`."),
        until: z.string().optional().describe("RFC3339; returns events with `created_at < until`."),
        ...paginationInput,
      }),
    },
    async (args) =>
      runTool(async () => {
        const page = await client.listEvents({
          ...(args.type !== undefined ? { type: args.type } : {}),
          ...(args.agent_email !== undefined ? { agentEmail: args.agent_email } : {}),
          ...(args.conversation_id !== undefined
            ? { conversationId: args.conversation_id }
            : {}),
          ...(args.message_id !== undefined ? { messageId: args.message_id } : {}),
          ...(args.since !== undefined ? { since: args.since } : {}),
          ...(args.until !== undefined ? { until: args.until } : {}),
          ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
          ...(args.limit !== undefined ? { limit: args.limit } : {}),
        });
        return { events: page.items, ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}) };
      }),
  );

  server.registerTool(
    "get_event",
    {
      title: "Get one webhook event",
      annotations: { readOnlyHint: true },
      description:
        "Fetch a single event by id. The response includes the full envelope payload AND a `delivery_status` block showing how many of the matched webhooks have delivered/pending/failed. Use this to triage \"did this specific event reach my receiver?\" Returns an error with status 410 if the event has passed the 30-day retention boundary (replay is no longer possible).",
      inputSchema: strictInputSchema({
        event_id: z.string().describe("Stable event id (evt_<32hex>)."),
      }),
    },
    async (args) => runTool(() => client.getEvent(args.event_id)),
  );

  server.registerTool(
    "redeliver_event",
    {
      title: "Replay a webhook event",
      annotations: { destructiveHint: false },
      description:
        "Re-fire a previously-emitted event to a webhook. Pass `webhook_id` to target one subscriber. Omit `webhook_id` to fan out to every webhook that originally matched the event (per the snapshot at fan-out time; does NOT re-evaluate against the current subscriber set, by design). **Important:** the replay uses the SAME envelope id as the original delivery. Customer-side receivers that dedupe on event id will discard the replay as already-processed — replay is recovery, not re-delivery. Use this for outage recovery, not for \"send this event twice on purpose.\"",
      inputSchema: strictInputSchema({
        event_id: z.string(),
        webhook_id: z
          .string()
          .optional()
          .describe(
            "Target webhook id. Must be in the originally-matched set (otherwise 409). Omit to fan out to every originally-matched webhook.",
          ),
      }),
    },
    async (args) =>
      runTool(() => client.redeliverEvent(args.event_id, args.webhook_id)),
  );
}
