import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { ProtectionConfigRequest } from "@e2a/sdk/v1";
import type { McpClient } from "../client.js";
import { z } from "zod";
import { runTool, strictInputSchema, paginationInput } from "./util.js";

export function registerAgentTools(server: McpServer, client: McpClient): void {
  server.registerTool(
    "list_agents",
    {
      title: "List agents",
      annotations: { readOnlyHint: true },
      description:
        "List the agent inboxes owned by the authenticated account, newest first. Set `deleted:true` to list the 30-day trash instead of live agents. Account scope only. **Cursor-paginated:** returns one page in `agents` plus a `next_cursor` when more remain — pass it back as `cursor` for the next page. Read-only.",
      inputSchema: strictInputSchema({
        ...paginationInput,
        deleted: z.boolean().optional().describe("List trashed agents instead of live agents."),
      }),
    },
    async (args) =>
      runTool(async () => {
        const page = await client.listAgents({
          ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
          ...(args.limit !== undefined ? { limit: args.limit } : {}),
          ...(args.deleted !== undefined ? { deleted: args.deleted } : {}),
        });
        return { agents: page.items, ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}) };
      }),
  );

  server.registerTool(
    "get_agent",
    {
      title: "Get one agent's configuration",
      annotations: { readOnlyHint: true },
      description:
        "Fetch a single agent by its email address — identity + status (name, domain, verification, created_at). The screening/protection config (gate, scan, holds) is NOT here: it is account-only and lives on `get_protection`. Use to inspect or confirm one agent without listing every agent. Read-only.",
      inputSchema: strictInputSchema({
        email: z
          .string()
          .email()
          .describe("Agent to fetch (full email, e.g. support@acme.com)."),
      }),
    },
    async (args) => runTool(() => client.getAgent(args.email)),
  );

  server.registerTool(
    "whoami",
    {
      title: "Get the authenticated account's identity",
      annotations: { readOnlyHint: true },
      description:
        "Use first when starting work on e2a to learn WHO you are: the authenticated user (email), the credential's scope (`account` or `agent`), and your plan + usage limits. For an agent-scoped credential it also returns `agent_email` — the single agent that credential IS. Account-scoped credentials own many agents; discover them with `list_agents`. This is identity, not an agent — it never guesses a 'default' agent.",
      inputSchema: strictInputSchema({}),
    },
    async () => runTool(() => client.whoami()),
  );

  server.registerTool(
    "create_agent",
    {
      title: "Create a new agent inbox",
      annotations: { destructiveHint: false },
      description:
        "Register a new agent by its full email address. For a custom domain, the domain must be one you've registered and verified (`register_domain` → `verify_domain` → poll `get_domain` for sending). For the deployment's shared domain, just use an email on it (e.g. support-bot@<shared-domain>). Inbound is always available via `list_messages` (poll) or a `create_webhook` subscription — there is no delivery 'mode' to choose. Returns the full agent.",
      inputSchema: strictInputSchema({
        email: z
          .string()
          .email()
          .describe(
            "Full email address of the new agent, e.g. support@acme.com. The domain must be a verified domain you own, or the deployment's shared domain.",
          ),
        name: z
          .string()
          .optional()
          .describe("Display name for the agent."),
      }),
    },
    async (args) =>
      runTool(() =>
        client.createAgent({
          email: args.email,
          ...(args.name !== undefined ? { name: args.name } : {}),
        }),
      ),
  );

  server.registerTool(
    "update_agent",
    {
      title: "Rename an agent",
      annotations: { idempotentHint: true, destructiveHint: false },
      description:
        "Update an agent's display name (a UI label; the agent's identity is its email). The screening/protection config (gate, scan, holds) is NOT here — use `update_protection`.",
      inputSchema: strictInputSchema({
        email: z
          .string()
          .email()
          .optional()
          .describe(
            "Agent to update (full email). Defaults to the credential's bound agent (agent-scoped credentials); required otherwise.",
          ),
        name: z.string().max(200).describe("New display name for the agent."),
      }),
    },
    async (args) => runTool(() => client.updateAgent({ name: args.name }, args.email)),
  );

  server.registerTool(
    "get_protection",
    {
      title: "Get an agent's protection config (beta)",
      annotations: { readOnlyHint: true },
      description:
        "Read an agent's protection posture: the inbound/outbound trust gate (policy + allowlist + action), the content-scan sensitivity, and the hold-queue mechanism. BETA — the shape may change. Account scope only: an agent-scoped credential cannot read its own protection config.",
      inputSchema: strictInputSchema({
        email: z
          .string()
          .email()
          .optional()
          .describe(
            "Agent to read (full email). Defaults to the credential's bound agent; required for account-scoped credentials.",
          ),
      }),
    },
    async (args) => runTool(() => client.getProtection(args.email)),
  );

  server.registerTool(
    "update_protection",
    {
      title: "Update an agent's protection config (beta)",
      annotations: { idempotentHint: true, destructiveHint: false },
      description:
        "Set an agent's protection posture. Read-modify-write: only the fields you pass change; the rest keep their current value. Inbound allowlist/domain gates first require DMARC pass, then match the aligned RFC 5322 From address. Outbound policy semantics: open matches every recipient; allowlist matches exact addresses in outbound_gate_allowlist; domain matches recipients on the agent's own domain. The outbound gate action applies when any recipient does not match. To require human review for every outbound message, set outbound_gate_policy=allowlist, outbound_gate_allowlist=[], outbound_gate_action=review, and holds_on_expiry=reject — this guarantees the recipient GATE routes every message to review; when scanning is enabled, messages crossing the scan block threshold are refused outright (blocked, not held). Using open with review, the gate will hold nothing (every recipient matches); content scanning, when enabled, can still hold or block a message. The scan sensitivity (off|low|medium|high) tunes content screening; holds govern the review queue. BETA. Account scope only.",
      inputSchema: strictInputSchema({
        email: z
          .string()
          .email()
          .optional()
          .describe("Agent to update (full email). Defaults to the credential's bound agent."),
        inbound_gate_policy: z
          .enum(["open", "allowlist", "domain"])
          .optional()
          .describe("Inbound trust gate: open (all), domain (listed domains), allowlist (listed addresses)."),
        inbound_gate_allowlist: z
          .array(z.string())
          .optional()
          .describe("Inbound trusted addresses (allowlist) or domains (domain). These gates first require DMARC pass, then match the aligned RFC 5322 From address."),
        inbound_gate_action: z
          .enum(["flag", "review", "block"])
          .optional()
          .describe("What an inbound gate non-match does: flag (deliver+annotate), review (hold), block."),
        inbound_scan_sensitivity: z
          .enum(["off", "low", "medium", "high"])
          .optional()
          .describe("Inbound content-scan sensitivity. off disables; low|medium|high increase aggressiveness."),
        outbound_gate_policy: z
          .enum(["open", "allowlist", "domain"])
          .optional()
          .describe(
            "Outbound recipient gate: open matches every recipient; allowlist matches exact addresses in outbound_gate_allowlist; domain matches recipients on the agent's own domain. The gate action applies when any recipient does not match.",
          ),
        outbound_gate_allowlist: z
          .array(z.string())
          .optional()
          .describe(
            "Exact recipient addresses matched when outbound_gate_policy is allowlist. An empty list makes every recipient a non-match; combine it with outbound_gate_action=review to review every outbound message.",
          ),
        outbound_gate_action: z
          .enum(["flag", "review", "block"])
          .optional()
          .describe(
            "What an outbound gate non-match does: flag (send + annotate), review (hold as pending_review for human approval), or block. With policy=open there are no non-matches, so this action never fires.",
          ),
        outbound_scan_sensitivity: z
          .enum(["off", "low", "medium", "high"])
          .optional()
          .describe("Outbound content-scan sensitivity."),
        holds_ttl_seconds: z
          .number()
          .int()
          .min(0)
          .optional()
          .describe("How long a held item waits before its on_expiry action fires."),
        holds_on_expiry: z
          .enum(["approve", "reject"])
          .optional()
          .describe(
            "What happens to a held item at TTL expiry. Use reject when outbound mail must never be sent without explicit human approval.",
          ),
        holds_suppress_notifications: z
          .boolean()
          .optional()
          .describe("When true, keep creating holds but do not email approval notifications."),
      }),
    },
    async (args) =>
      runTool(async () => {
        // Read-modify-write over the full-replace PUT: fetch current, overlay
        // only the provided fields, write back. Avoids a partial PUT resetting
        // sections the caller didn't mean to touch.
        const cfg = await client.getProtection(args.email);
        // The generated view uses string-enum types; the Zod enums produce the
        // same literals, so cast each to the field's own type.
        if (args.inbound_gate_policy !== undefined)
          cfg.inbound.gate.policy = args.inbound_gate_policy as typeof cfg.inbound.gate.policy;
        if (args.inbound_gate_allowlist !== undefined) cfg.inbound.gate.allowlist = args.inbound_gate_allowlist;
        if (args.inbound_gate_action !== undefined)
          cfg.inbound.gate.action = args.inbound_gate_action as typeof cfg.inbound.gate.action;
        if (args.inbound_scan_sensitivity !== undefined)
          cfg.inbound.scan.sensitivity = args.inbound_scan_sensitivity as typeof cfg.inbound.scan.sensitivity;
        if (args.outbound_gate_policy !== undefined)
          cfg.outbound.gate.policy = args.outbound_gate_policy as typeof cfg.outbound.gate.policy;
        if (args.outbound_gate_allowlist !== undefined) cfg.outbound.gate.allowlist = args.outbound_gate_allowlist;
        if (args.outbound_gate_action !== undefined)
          cfg.outbound.gate.action = args.outbound_gate_action as typeof cfg.outbound.gate.action;
        if (args.outbound_scan_sensitivity !== undefined)
          cfg.outbound.scan.sensitivity = args.outbound_scan_sensitivity as typeof cfg.outbound.scan.sensitivity;
        if (args.holds_ttl_seconds !== undefined) cfg.holds.ttlSeconds = args.holds_ttl_seconds;
        if (args.holds_on_expiry !== undefined)
          cfg.holds.onExpiry = args.holds_on_expiry as typeof cfg.holds.onExpiry;
        if (args.holds_suppress_notifications !== undefined)
          cfg.holds.suppressNotifications = args.holds_suppress_notifications;
        // ProtectionConfigRequest is the View's field-for-field request twin
        // (the spec splits them only so responses can be additive-open while
        // requests stay strict), but the generated enum types are nominal, so
        // the shape-identical view needs an explicit cast to PUT back.
        return client.updateProtection(cfg as unknown as ProtectionConfigRequest, args.email);
      }),
  );

  server.registerTool(
    "restore_agent",
    {
      title: "Restore an agent from trash",
      annotations: { destructiveHint: false, idempotentHint: false },
      description:
        "Restore an agent that was soft-deleted within the 30-day trash window, including its messages and configuration. Scheduled messages restored before `scheduled_at` re-arm; at/after `scheduled_at` they return live as failed with submission canceled. Account scope only. Returns the restored agent; a live agent returns `not_in_trash`, while an agent already claimed by permanent deletion returns `purge_in_progress` and cannot be restored.",
      inputSchema: strictInputSchema({
        email: z.string().email().describe("Full email address of the trashed agent to restore."),
      }),
    },
    async (args) => runTool(() => client.restoreAgent(args.email)),
  );

  server.registerTool(
    "delete_agent",
    {
      title: "Delete an agent inbox (DESTRUCTIVE)",
      annotations: { destructiveHint: true, idempotentHint: true },
      description:
        "Move the agent inbox to trash for about 30 days by default. The agent stops receiving mail and disappears from normal lists, but its messages and configuration are retained so it can be restored before automatic purge. Pass `permanent: true` to skip the trash and delete the inbox and every message in it irreversibly right away instead (accepts live and trashed agents); `delete_domain` succeeds only when the domain has no agents, so permanently delete every live or trashed agent on it first. Requires `confirm: true` — set it explicitly to acknowledge the destructive action.",
      inputSchema: strictInputSchema({
        email: z
          .string()
          .email()
          .optional()
          .describe(
            "Agent to delete (full email). Defaults to the credential's bound agent (agent-scoped credentials); required otherwise.",
          ),
        confirm: z
          .literal(true)
          .describe(
            "Must be set to true to proceed. Guard against an LLM hallucinating a delete from ambiguous context.",
          ),
        permanent: z
          .boolean()
          .optional()
          .describe(
            "Skip the trash and delete irreversibly right away, for a live or already-trashed agent. Purges every message in the inbox with no restore path. Defaults to false (moves to trash, restorable for about 30 days).",
          ),
      }),
    },
    async (args) =>
      runTool(async () => {
        if (args.confirm !== true) {
          throw new Error(
            "delete_agent requires confirm:true — refusing to proceed without explicit confirmation.",
          );
        }
        // Return the server's deletion receipt verbatim:
        // {deleted:true, email, messages_deleted}.
        return client.deleteAgent(args.email, args.permanent);
      }),
  );
}
