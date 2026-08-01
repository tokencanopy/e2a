import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { McpClient } from "../client.js";
import { z } from "zod";
import { paginationInput, runTool, strictInputSchema, toMcpOutput } from "./util.js";

// Suppression management. Two scopes, deliberately kept as distinct tools:
//   - Account level: the per-tenant block list auto-populated by hard bounces
//     and complaints. It spans EVERY agent on the account and is enforced at
//     send time (sends fail with recipient_suppressed). List/remove only —
//     there is no account-level create in the API.
//   - Agent level (beta): unsubscribe/manual blocks scoped to one exact
//     sending agent. List/add/remove.
// All five are ADMIN tier: the server requires account scope on every one of
// them (an agent-scoped credential must not read or edit blocklists).

const suppressedAddress = z.string().email().describe("Recipient email address on the suppression list.");

const agentEmail = z
  .string()
  .email()
  .describe("Owning agent's inbox (full email address). Account-scoped credentials only.");

export function registerSuppressionTools(server: McpServer, client: McpClient): void {
  server.registerTool(
    "list_suppressions",
    {
      title: "List account-suppressed recipients",
      annotations: { readOnlyHint: true },
      description:
        "List the account-wide suppression list: recipient addresses e2a will refuse to send to from ANY agent, auto-added on a hard bounce or complaint. Sends to them fail with recipient_suppressed. Cursor-paginated: returns `suppressions` and optional `next_cursor`. Account scope only.",
      inputSchema: strictInputSchema({ ...paginationInput }),
    },
    async (args) => runTool(async () => {
      const page = await client.listSuppressions({
        ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
        ...(args.limit !== undefined ? { limit: args.limit } : {}),
      });
      return { suppressions: toMcpOutput(page.items), ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}) };
    }),
  );

  server.registerTool(
    "delete_suppression",
    {
      title: "Un-suppress a recipient account-wide",
      annotations: { destructiveHint: true, idempotentHint: true },
      description:
        "Remove an address from the account-wide suppression list so sends to it succeed again. Only do this when the address is known to be deliverable — removing a genuinely bouncing or complaining recipient hurts sender reputation. Requires confirm:true so an LLM cannot restore sendability on ambiguous context. Account scope only.",
      inputSchema: strictInputSchema({
        address: suppressedAddress,
        confirm: z.literal(true).describe("Must be true to proceed."),
      }),
    },
    async (args) => {
      if (args.confirm !== true) {
        throw new Error("delete_suppression requires confirm:true.");
      }
      return runTool(() => client.deleteSuppression(args.address));
    },
  );

  server.registerTool(
    "list_agent_suppressions",
    {
      title: "List an agent's suppressed recipients (beta)",
      annotations: { readOnlyHint: true },
      description:
        "List recipient addresses blocked only for this exact sending agent (unsubscribes and manual blocks). Cursor-paginated: returns `suppressions` and optional `next_cursor`. Account scope only.",
      inputSchema: strictInputSchema({ email: agentEmail, ...paginationInput }),
    },
    async (args) => runTool(async () => {
      const page = await client.listAgentSuppressions(args.email, {
        ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
        ...(args.limit !== undefined ? { limit: args.limit } : {}),
      });
      return { suppressions: toMcpOutput(page.items), ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}) };
    }),
  );

  server.registerTool(
    "create_agent_suppression",
    {
      title: "Suppress a recipient for one agent (beta)",
      annotations: { destructiveHint: false, idempotentHint: true },
      description:
        "Idempotently block a recipient for this exact sending agent only (source: manual). Other agents on the account can still mail the address. Account scope only.",
      inputSchema: strictInputSchema({
        email: agentEmail,
        address: z.string().email().describe("Recipient email address to suppress for this agent."),
        reason: z.string().max(2000).optional().describe("Optional reason recorded on the block."),
      }),
    },
    async (args) => runTool(() => client.createAgentSuppression(args.email, {
      address: args.address,
      ...(args.reason !== undefined ? { reason: args.reason } : {}),
    })),
  );

  server.registerTool(
    "delete_agent_suppression",
    {
      title: "Remove an agent recipient suppression (beta)",
      annotations: { destructiveHint: true, idempotentHint: true },
      description:
        "Remove only the exact agent-scoped block so this agent can mail the address again. Account-wide bounce/complaint suppressions are separate and unaffected. Requires confirm:true so an LLM cannot remove an unsubscribe or manual block on ambiguous context. Account scope only.",
      inputSchema: strictInputSchema({
        email: agentEmail,
        address: suppressedAddress,
        confirm: z.literal(true).describe("Must be true to proceed."),
      }),
    },
    async (args) => {
      if (args.confirm !== true) {
        throw new Error("delete_agent_suppression requires confirm:true.");
      }
      return runTool(() => client.deleteAgentSuppression(args.email, args.address));
    },
  );
}
