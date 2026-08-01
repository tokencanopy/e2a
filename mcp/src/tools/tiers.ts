// Tool tier map — the single source of truth for §6a scope-gating.
//
// The MCP surface splits by persona, exposed per credential scope (§5/§6a):
//   - runtime (scope=agent): what a deployed agent uses every turn. An
//     `agent`-scoped credential sees ONLY these.
//   - admin   (scope=account): provisioning/setup. An `account`-scoped
//     credential sees runtime + admin (the full surface).
//
// This is a UX / decision-space optimization, NOT the security boundary — the
// backend enforces scope per-handler regardless (an agent-scoped credential is
// 403'd on account ops even if a tool were mis-listed). Keeping the classifica-
// tion here (one auditable place) mirrors the design's "the drift-gate map
// records each tool's tier next to its operationId".
//
// INVARIANT: every registered tool name MUST appear in exactly one set below.
// A new tool with no tier would be silently gated out of EVERY scope (it's not
// in any allowed set), so it'd vanish without a dedicated failure. The exported
// `assertToolTiersComplete` makes that loud: a test collects the actual
// registered tool names and asserts the tier map covers them exactly (see
// tools.test.ts "every registered tool has exactly one tier").

/** Runtime/inbox tools — visible to BOTH agent- and account-scoped credentials. */
export const RUNTIME_TOOLS: ReadonlySet<string> = new Set([
  "whoami",
  "get_agent",
  "list_messages",
  "get_message",
  "get_message_lifecycle",
  "get_attachment",
  "get_attachment_data",
  // Soft delete + restore are both per-agent inbox hygiene: the REST handler
  // pins an agent-scoped credential to its own bound agent (resolveOwnedAgent).
  // The PERMANENT purge is account-only server-side, and the MCP tool doesn't
  // expose it at all — so no agent-scoped session can destroy evidence
  // irreversibly, which is why this stays runtime while delete_agent is admin.
  "delete_message",
  "restore_message",
  "update_message_labels",
  "list_conversations",
  "get_conversation",
  "send_message",
  "send_email",
  "reply_to_message",
  "forward_message",
  "list_outreach_contacts",
  "get_outreach_contact",
  "set_outreach_contact",
  "delete_outreach_contact",
  // Review discovery and decisions are deliberately NOT here. They expose the
  // account-wide queue and belong to the account owner / human reviewer.
]);

/** Admin/setup tools — visible ONLY to account-scoped credentials. */
export const ADMIN_TOOLS: ReadonlySet<string> = new Set([
  // Listing the account's agent inventory (including trash) requires
  // requireAccountUser at the REST handler; an agent-scoped credential can
  // address only its bound agent via get_agent.
  "list_agents",
  "create_agent",
  "update_agent",
  // Protection config is account-only — an agent-scoped session must not read or
  // change its own detection tuning (audit #13). get_protection is admin even
  // though get_agent is runtime.
  "get_protection",
  "update_protection",
  "delete_agent",
  "restore_agent",
  // All review operations are account-only. Letting the gated agent inspect or
  // resolve holds could disclose quarantined inbound content or enable
  // self-approval of its own outbound mail.
  "list_reviews",
  "get_review",
  "list_pending_messages",
  "get_pending_message",
  // Review approval is an account-owner / human review action — NOT something the
  // gated agent may do to its own held outbound (that would be self-approval,
  // defeating the review gate). The backend enforces this too: the approve/reject
  // handlers (internal/httpapi/reviews.go) require account scope (403 for agent-scoped).
  "approve_review",
  "reject_review",
  "approve_pending_message",
  "reject_pending_message",
  "approve_message",
  "reject_message",
  "list_domains",
  "get_domain",
  "register_domain",
  "verify_domain",
  "delete_domain",
  "list_webhooks",
  "get_webhook",
  "create_webhook",
  "update_webhook",
  "delete_webhook",
  "rotate_webhook_secret",
  "test_webhook",
  "list_webhook_deliveries",
  "list_events",
  "get_event",
  "redeliver_event",
  // Templates (beta) are account-scope end to end — every /v1/templates and
  // /v1/starter-templates handler calls requireAccountUser, 403ing
  // agent-scoped credentials — so the whole group is admin-tier. (Sending
  // WITH a template stays on the runtime send_message tool.)
  "list_templates",
  "get_template",
  "create_template",
  "update_template",
  "delete_template",
  "validate_template",
  "list_starter_templates",
  "get_starter_template",
  // API keys are admin-tier AND further restricted inside the tool surface:
  // create_api_key mints ONLY agent-scoped keys (scope is hardwired in
  // McpClient.createAgentApiKey, not an input), so MCP can never mint an
  // account-scoped (workspace-admin) credential — that stays a dashboard /
  // raw-API action. list is metadata-only; delete revokes (de-escalation).
  "list_api_keys",
  "create_api_key",
  "delete_api_key",
  "list_contacts",
  "get_contact",
  "create_contact",
  "update_contact",
  "delete_contact",
  "import_contacts",
  "delete_contact_import",
  // Suppression administration is account-only on the server for BOTH scopes:
  // the agent-scoped endpoints deliberately reject agent credentials (an agent
  // must not edit its own blocklist), so exposing these at runtime tier would
  // only surface guaranteed-403 tools.
  "list_suppressions",
  "delete_suppression",
  "list_agent_suppressions",
  "create_agent_suppression",
  "delete_agent_suppression",
]);

export type Scope = "account" | "agent";

/**
 * The set of tool names a given credential scope may see.
 * - `agent`   → runtime only.
 * - `account` → runtime + admin (full surface).
 * Any unrecognized scope falls back to runtime (least privilege).
 */
export function toolNamesForScope(scope: Scope | string): ReadonlySet<string> {
  if (scope === "account") {
    return new Set([...RUNTIME_TOOLS, ...ADMIN_TOOLS]);
  }
  // agent, or anything unexpected → least privilege.
  return RUNTIME_TOOLS;
}

/** True if `name` is allowed for `scope`. */
export function toolAllowedForScope(name: string, scope: Scope | string): boolean {
  return toolNamesForScope(scope).has(name);
}

/**
 * Drift guard: assert the tier map covers the actually-registered tools EXACTLY.
 * Throws if any registered tool is untiered (would be silently hidden from every
 * scope), double-tiered, or if a tier lists a name that isn't registered
 * (phantom). Call from a test with the real registered tool names.
 */
export function assertToolTiersComplete(registered: Iterable<string>): void {
  const reg = new Set(registered);
  const untiered = [...reg].filter((n) => !RUNTIME_TOOLS.has(n) && !ADMIN_TOOLS.has(n));
  const doubled = [...reg].filter((n) => RUNTIME_TOOLS.has(n) && ADMIN_TOOLS.has(n));
  const phantom = [...RUNTIME_TOOLS, ...ADMIN_TOOLS].filter((n) => !reg.has(n));
  const problems: string[] = [];
  if (untiered.length) problems.push(`untiered (hidden from all scopes): ${untiered.join(", ")}`);
  if (doubled.length) problems.push(`in both tiers: ${doubled.join(", ")}`);
  if (phantom.length) problems.push(`tiered but not registered: ${phantom.join(", ")}`);
  if (problems.length) {
    throw new Error(`tool tier map out of sync — ${problems.join("; ")}`);
  }
}
