import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { ImportContactsRequestOnConflictEnum } from "@e2a/sdk/v1";
import type { McpClient } from "../client.js";
import { z } from "zod";
import { emailSelector, paginationInput, runTool, strictInputSchema } from "./util.js";

const metadata = z.record(
  z.string().max(128),
  z.union([z.string().max(4096), z.number(), z.boolean(), z.null()]),
).optional().describe("Flat caller-owned metadata. e2a stores it but never interprets it.");

const contactAddress = z.string().email().describe("Contact email address.");

// RFC 3339 date-time with an explicit UTC offset (Z or ±HH:MM both accepted).
// Bare date-times and date-only values are rejected rather than guessed at:
// `new Date()` would read them in LOCAL time and silently shift the filter
// across timezones. Same rule as scheduled sending (messages.ts sendAtField).
const contactTimestamp = z.string().datetime({ offset: true });

export function registerContactTools(server: McpServer, client: McpClient): void {
  server.registerTool(
    "list_contacts",
    {
      title: "List account contacts (beta)",
      annotations: { readOnlyHint: true },
      description:
        "List account-scoped contact identity. Account scope only. This is identity/provenance, not an outreach sequence. Cursor-paginated: returns `contacts` and optional `next_cursor`.",
      inputSchema: strictInputSchema({
        ...paginationInput,
        source: z.enum(["import", "manual", "inbound"]).optional(),
        import_batch_id: z.string().optional(),
        created_after: contactTimestamp.optional(),
        created_before: contactTimestamp.optional(),
      }),
    },
    async (args) => runTool(async () => {
      const page = await client.listContacts({
        ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
        ...(args.limit !== undefined ? { limit: args.limit } : {}),
        ...(args.source !== undefined ? { source: args.source } : {}),
        ...(args.import_batch_id !== undefined ? { importBatchId: args.import_batch_id } : {}),
        ...(args.created_after !== undefined ? { createdAfter: new Date(args.created_after) } : {}),
        ...(args.created_before !== undefined ? { createdBefore: new Date(args.created_before) } : {}),
      });
      return { contacts: page.items, ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}) };
    }),
  );

  server.registerTool(
    "get_contact",
    {
      title: "Get an account contact (beta)",
      annotations: { readOnlyHint: true },
      description: "Fetch one account-scoped contact by address and return its `etag`. Pass that validator as `if_match` when updating to reject stale edits. Account scope only.",
      inputSchema: strictInputSchema({ address: contactAddress }),
    },
    async (args) => runTool(async () => {
      const { data, etag } = await client.getContactWithETag(args.address);
      return { ...data, ...(etag ? { etag } : {}) };
    }),
  );

  server.registerTool(
    "create_contact",
    {
      title: "Create an account contact (beta)",
      annotations: { destructiveHint: false, idempotentHint: true },
      description:
        "Record one contact identity. This NEVER sends email and does not enroll the person in outreach. Account scope only.",
      inputSchema: strictInputSchema({
        address: contactAddress,
        display_name: z.string().max(320).optional(),
        metadata,
        idempotency_key: z.string().min(1).max(255).optional(),
      }),
    },
    async (args) => runTool(() => client.createContact({
      address: args.address,
      ...(args.display_name !== undefined ? { displayName: args.display_name } : {}),
      ...(args.metadata !== undefined ? { metadata: args.metadata } : {}),
    }, args.idempotency_key)),
  );

  server.registerTool(
    "update_contact",
    {
      title: "Update account contact identity (beta)",
      annotations: { destructiveHint: false, idempotentHint: true },
      description:
        "Partially update display name or metadata. Address and provenance are immutable. Pass `if_match` from get_contact to reject a stale write. Account scope only.",
      inputSchema: strictInputSchema({
        address: contactAddress,
        display_name: z.string().max(320).optional(),
        metadata,
        if_match: z.string().optional(),
      }).refine(
        (value) => value.display_name !== undefined || value.metadata !== undefined,
        { message: "display_name or metadata is required" },
      ),
    },
    async (args) => runTool(() => client.updateContact(args.address, {
      ...(args.display_name !== undefined ? { displayName: args.display_name } : {}),
      ...(args.metadata !== undefined ? { metadata: args.metadata } : {}),
    }, args.if_match)),
  );

  server.registerTool(
    "delete_contact",
    {
      title: "Delete account contact identity (beta)",
      annotations: { destructiveHint: true, idempotentHint: true },
      description:
        "Delete one contact and its outreach rows. Suppression/consent survives. Account scope only.",
      inputSchema: strictInputSchema({ address: contactAddress }),
    },
    async (args) => runTool(() => client.deleteContact(args.address)),
  );

  server.registerTool(
    "import_contacts",
    {
      title: "Import contacts and optionally enroll them (beta)",
      annotations: { destructiveHint: false, idempotentHint: false },
      description:
        "Import up to 1000 already-parsed rows. This NEVER sends email. Optionally set `agent_email` to enroll every valid row atomically; `stage` initializes only new outreach records. Existing engagement state and all suppression survive. Account scope only. Parse CSV at the client edge.",
      inputSchema: strictInputSchema({
        contacts: z.array(z.object({
          address: contactAddress,
          display_name: z.string().max(320).optional(),
          metadata,
        }).strict()).min(1).max(1000),
        on_conflict: z.enum(["merge", "skip"]).optional(),
        agent_email: z.string().email().optional(),
        stage: z.string().max(128).optional(),
        idempotency_key: z.string().min(1).max(255).optional()
          .describe("Stable key for replaying the same logical upload after an ambiguous failure."),
      }).refine(
        (value) => value.stage === undefined || value.agent_email !== undefined,
        { message: "stage requires agent_email" },
      ),
    },
    async (args) => runTool(() => {
      const body = {
        contacts: args.contacts.map((row) => ({
          address: row.address,
          ...(row.display_name !== undefined ? { displayName: row.display_name } : {}),
          ...(row.metadata !== undefined ? { metadata: row.metadata } : {}),
        })),
        ...(args.on_conflict !== undefined ? {
          onConflict: args.on_conflict === "skip"
            ? ImportContactsRequestOnConflictEnum.Skip
            : ImportContactsRequestOnConflictEnum.Merge,
        } : {}),
        ...(args.agent_email !== undefined ? { agentEmail: args.agent_email } : {}),
        ...(args.stage !== undefined ? { stage: args.stage } : {}),
      };
      return args.idempotency_key
        ? client.importContacts(body, args.idempotency_key)
        : client.importContacts(body);
    }),
  );

  server.registerTool(
    "delete_contact_import",
    {
      title: "Reverse a contact import (beta)",
      annotations: { destructiveHint: true, idempotentHint: true },
      description:
        "Remove only verifiably untouched contacts and per-agent enrolments created by one import batch. Contacts or enrolments edited since the import, contacts with correspondence history or any surviving engagement, pre-existing outreach, and suppressions all survive, and the receipt reports each category. Account scope only.",
      inputSchema: strictInputSchema({ batch_id: z.string().min(1) }),
    },
    async (args) => runTool(() => client.deleteContactImport(args.batch_id)),
  );

  server.registerTool(
    "list_outreach_contacts",
    {
      title: "List an agent's outreach contacts (beta)",
      annotations: { readOnlyHint: true },
      description:
        "List the contacts this agent is working. For a safe follow-up pull use `replied:false`, `suppressed:false`, `next_action_before:now`, and `last_outbound_before:<stale cutoff>` together. Never-contacted rows are included by the last-outbound filter. e2a stores state and wakes the agent; it NEVER composes or sends a follow-up. Cursor-paginated.",
      inputSchema: strictInputSchema({
        ...paginationInput,
        email: emailSelector,
        stage: z.string().max(128).optional(),
        replied: z.boolean().optional(),
        suppressed: z.boolean().optional(),
        next_action_before: contactTimestamp.optional(),
        last_outbound_before: contactTimestamp.optional(),
      }),
    },
    async (args) => runTool(async () => {
      const page = await client.listOutreach({
        ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
        ...(args.limit !== undefined ? { limit: args.limit } : {}),
        ...(args.stage !== undefined ? { stage: args.stage } : {}),
        ...(args.replied !== undefined ? { replied: args.replied } : {}),
        ...(args.suppressed !== undefined ? { suppressed: args.suppressed } : {}),
        ...(args.next_action_before !== undefined ? { nextActionBefore: new Date(args.next_action_before) } : {}),
        ...(args.last_outbound_before !== undefined ? { lastOutboundBefore: new Date(args.last_outbound_before) } : {}),
      }, args.email);
      return { contacts: page.items, ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}) };
    }),
  );

  server.registerTool(
    "get_outreach_contact",
    {
      title: "Get one outreach contact (beta)",
      annotations: { readOnlyHint: true },
      description: "Fetch one agent/contact outreach record, including reply facts, suppression, and an `etag`. Pass the validator as `if_match` when updating to reject stale agent loops.",
      inputSchema: strictInputSchema({ email: emailSelector, address: contactAddress }),
    },
    async (args) => runTool(async () => {
      const { data, etag } = await client.getOutreachWithETag(args.address, args.email);
      return { ...data, ...(etag ? { etag } : {}) };
    }),
  );

  server.registerTool(
    "set_outreach_contact",
    {
      title: "Enroll or update an outreach contact (beta)",
      annotations: { destructiveHint: false, idempotentHint: true },
      description:
        "Enroll a contact or update agent-owned stage, next action, and metadata. Omitted fields remain unchanged. Pass `if_match` from get_outreach_contact to reject a stale update; conditional writes never enroll a missing contact. This NEVER sends email; `next_action_at` schedules `contact.due` for configured webhooks. A deployed agent runtime can consume that event; it does not launch a local coding-agent session over MCP or WebSocket.",
      inputSchema: strictInputSchema({
        email: emailSelector,
        address: contactAddress,
        stage: z.string().max(128).optional(),
        next_action_at: contactTimestamp.nullable().optional(),
        metadata,
        if_match: z.string().optional(),
      }).refine(
        (value) => value.stage !== undefined || value.next_action_at !== undefined || value.metadata !== undefined,
        { message: "stage, next_action_at, or metadata is required" },
      ),
    },
    async (args) => runTool(() => client.setOutreach(args.address, {
      ...(args.stage !== undefined ? { stage: args.stage } : {}),
      ...(args.next_action_at !== undefined ? {
        nextActionAt: args.next_action_at === null ? null : new Date(args.next_action_at),
      } : {}),
      ...(args.metadata !== undefined ? { metadata: args.metadata } : {}),
    }, args.email, args.if_match)),
  );

  server.registerTool(
    "delete_outreach_contact",
    {
      title: "Un-enroll an outreach contact (beta)",
      annotations: { destructiveHint: true, idempotentHint: true },
      description:
        "Remove only this agent's outreach state. Account contact identity and suppression survive; this does not restore sendability.",
      inputSchema: strictInputSchema({ email: emailSelector, address: contactAddress }),
    },
    async (args) => runTool(() => client.deleteOutreach(args.address, args.email)),
  );
}
