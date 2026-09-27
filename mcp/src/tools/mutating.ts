// Per-tool `mutating` flag — the MCP surface's read-only classification
// (docs/design/account-read-only.md).
//
// An account whose sending is paused for an abuse review is READ-ONLY: every
// write on every surface is refused with 403 `account_read_only`. Every MCP
// tool call goes through the /v1 REST API with the caller's own credential,
// so the enforcement point for MCP is the server's /v1 read-only guard — the
// MCP server keeps no account state it could let go stale, and a pause or a
// resume applies to the very next tool call. A refused call surfaces like any
// other API error: `e2a error [account_read_only]: …` in the text and
// `{ code: "account_read_only", retryable: false, status: 403 }` in
// structuredContent.
//
// This map records which tools that guard will refuse (mutating: the tool
// calls a /v1 write operation) and which stay available (reads). It is the
// contract an agent can rely on — "while read-only, exactly these tools keep
// working" — advertised on every tool as `_meta["e2a/mutating"]` (set in
// server.ts at registration) and pinned by tests: every registered tool must
// be in exactly one set; the sets must agree with the HTTP methods of the /v1
// operations each tool calls (TOOL_OPERATIONS, walked against
// api/openapi.yaml); and they must agree with the MCP annotations (a
// readOnlyHint tool never mutates; a destructiveHint tool always does).

/** Tools that call a /v1 write operation — refused for a read-only account. */
export const MUTATING_TOOLS: ReadonlySet<string> = new Set([
  // agents
  "create_agent",
  "update_agent",
  "update_protection",
  "delete_agent",
  "restore_agent",
  // messages
  "send_message",
  "send_email",
  "reply_to_message",
  "forward_message",
  "update_message_labels",
  "delete_message",
  "restore_message",
  // reviews (both directions)
  "approve_review",
  "reject_review",
  "approve_pending_message",
  "reject_pending_message",
  "approve_message",
  "reject_message",
  // domains
  "register_domain",
  "verify_domain",
  "delete_domain",
  // webhooks + events
  "create_webhook",
  "update_webhook",
  "delete_webhook",
  "rotate_webhook_secret",
  "test_webhook",
  "redeliver_event",
  // templates
  "create_template",
  "update_template",
  "delete_template",
  // api keys
  "create_api_key",
  "delete_api_key",
  // contacts + outreach
  "create_contact",
  "update_contact",
  "delete_contact",
  "import_contacts",
  "delete_contact_import",
  "set_outreach_contact",
  "delete_outreach_contact",
  // suppressions
  "delete_suppression",
  "create_agent_suppression",
  "delete_agent_suppression",
]);

/** Tools that only read — they keep working for a read-only account. */
export const NON_MUTATING_TOOLS: ReadonlySet<string> = new Set([
  "whoami",
  "list_agents",
  "get_agent",
  "get_protection",
  "list_messages",
  // get_message is not readOnlyHint (fetching marks an inbound message read),
  // but it calls the GET read operation, which a read-only account keeps.
  "get_message",
  "get_message_lifecycle",
  "get_attachment",
  "get_attachment_data",
  "list_conversations",
  "get_conversation",
  "list_reviews",
  "get_review",
  "list_pending_messages",
  "get_pending_message",
  "list_domains",
  "get_domain",
  "list_webhooks",
  "get_webhook",
  "list_webhook_deliveries",
  "list_events",
  "get_event",
  "list_templates",
  "get_template",
  // validate_template POSTs a draft for validation and stores nothing.
  "validate_template",
  "list_starter_templates",
  "get_starter_template",
  "list_api_keys",
  "list_contacts",
  "get_contact",
  "list_outreach_contacts",
  "get_outreach_contact",
  "list_suppressions",
  "list_agent_suppressions",
  "get_agent_metrics",
  "get_account_metrics",
]);

/**
 * The /v1 operation(s) (OpenAPI operationId) each tool calls. This is the
 * independent evidence behind the two sets above: a test walks it against
 * api/openapi.yaml and requires a tool to be in MUTATING_TOOLS exactly when
 * one of its operations is a write under the server's rule (any method other
 * than GET/HEAD, except validateTemplate, which stores nothing) — the same
 * rule as `classifyOperation` in internal/httpapi/read_only.go.
 */
export const TOOL_OPERATIONS: Readonly<Record<string, readonly string[]>> = {
  // agents
  whoami: ["getAccount"],
  list_agents: ["listAgents"],
  get_agent: ["getAgent"],
  create_agent: ["createAgent"],
  update_agent: ["updateAgent"],
  get_protection: ["getAgentProtection"],
  update_protection: ["getAgentProtection", "putAgentProtection"],
  delete_agent: ["deleteAgent"],
  restore_agent: ["restoreAgent"],
  // messages
  list_messages: ["listMessages"],
  get_message: ["getMessage"],
  get_message_lifecycle: ["getMessageLifecycle"],
  get_attachment: ["getAttachment"],
  get_attachment_data: ["getAttachment"],
  send_message: ["sendMessage"],
  send_email: ["sendMessage"],
  reply_to_message: ["replyToMessage"],
  forward_message: ["forwardMessage"],
  update_message_labels: ["updateMessage"],
  delete_message: ["deleteMessage"],
  restore_message: ["restoreMessage"],
  list_conversations: ["listConversations"],
  get_conversation: ["getConversation"],
  // reviews (canonical + deprecated aliases)
  list_reviews: ["listReviews"],
  get_review: ["getReview"],
  approve_review: ["approveReview"],
  reject_review: ["rejectReview"],
  list_pending_messages: ["listReviews"],
  get_pending_message: ["getReview"],
  approve_pending_message: ["approveReview"],
  reject_pending_message: ["rejectReview"],
  approve_message: ["approveReview"],
  reject_message: ["rejectReview"],
  // domains
  list_domains: ["listDomains"],
  get_domain: ["getDomain"],
  register_domain: ["registerDomain"],
  verify_domain: ["verifyDomain"],
  delete_domain: ["deleteDomain"],
  // webhooks + events
  list_webhooks: ["listWebhooks"],
  get_webhook: ["getWebhook"],
  create_webhook: ["createWebhook"],
  update_webhook: ["updateWebhook"],
  delete_webhook: ["deleteWebhook"],
  rotate_webhook_secret: ["rotateWebhookSecret"],
  test_webhook: ["testWebhook"],
  list_webhook_deliveries: ["listWebhookDeliveries"],
  list_events: ["listEvents"],
  get_event: ["getEvent"],
  redeliver_event: ["redeliverEvent"],
  // templates
  list_templates: ["listTemplates"],
  get_template: ["getTemplate"],
  create_template: ["createTemplate"],
  update_template: ["updateTemplate"],
  delete_template: ["deleteTemplate"],
  validate_template: ["validateTemplate"],
  list_starter_templates: ["listStarterTemplates"],
  get_starter_template: ["getStarterTemplate"],
  // api keys
  list_api_keys: ["listApiKeys"],
  create_api_key: ["createApiKey"],
  delete_api_key: ["deleteApiKey"],
  // contacts + outreach
  list_contacts: ["listContacts"],
  get_contact: ["getContact"],
  create_contact: ["createContact"],
  update_contact: ["updateContact"],
  delete_contact: ["deleteContact"],
  import_contacts: ["importContacts"],
  delete_contact_import: ["deleteImportBatch"],
  list_outreach_contacts: ["listEngagements"],
  get_outreach_contact: ["getEngagement"],
  set_outreach_contact: ["upsertEngagement"],
  delete_outreach_contact: ["deleteEngagement"],
  // suppressions
  list_suppressions: ["listSuppressions"],
  delete_suppression: ["deleteSuppression"],
  list_agent_suppressions: ["listAgentSuppressions"],
  create_agent_suppression: ["createAgentSuppression"],
  delete_agent_suppression: ["deleteAgentSuppression"],
  // metrics
  get_agent_metrics: ["getAgentMetrics"],
  get_account_metrics: ["getAccountMetrics"],
};

/** The `_meta` key under which every tool advertises its mutating flag. */
export const MUTATING_META_KEY = "e2a/mutating";

/** True when `name` calls a /v1 write (refused while the account is read-only). */
export function isMutatingTool(name: string): boolean {
  return MUTATING_TOOLS.has(name);
}

/**
 * Drift guard: every registered tool is classified exactly once, and nothing
 * classified is unregistered. Call from a test with the real registered names.
 */
export function assertMutatingClassificationComplete(registered: Iterable<string>): void {
  const reg = new Set(registered);
  const unclassified = [...reg].filter((n) => !MUTATING_TOOLS.has(n) && !NON_MUTATING_TOOLS.has(n));
  const doubled = [...reg].filter((n) => MUTATING_TOOLS.has(n) && NON_MUTATING_TOOLS.has(n));
  const phantom = [...MUTATING_TOOLS, ...NON_MUTATING_TOOLS].filter((n) => !reg.has(n));
  const problems: string[] = [];
  if (unclassified.length) problems.push(`unclassified: ${unclassified.join(", ")}`);
  if (doubled.length) problems.push(`both mutating and non-mutating: ${doubled.join(", ")}`);
  if (phantom.length) problems.push(`classified but not registered: ${phantom.join(", ")}`);
  if (problems.length) {
    throw new Error(`MCP read-only classification out of sync — ${problems.join("; ")}`);
  }
}
