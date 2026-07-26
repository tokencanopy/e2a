import { E2AClient, CreateAPIKeyRequestScopeEnum } from "@e2a/sdk/v1";
import type {
  AccountView,
  AgentView,
  MessageView,
  AttachmentView,
  MessageSummaryView,
  ReviewView,
  SendResultView,
  RejectResultView,
  UpdateMessageResultView,
  ConversationSummaryView,
  ConversationDetailView,
  DomainView,
  VerifyDomainView,
  EventView,
  RedeliverView,
  WebhookView,
  WebhookDeliveryView,
  CreateWebhookResponse,
  RotateSecretResponse,
  TestWebhookResponse,
  Attachment,
  APIKeyView,
  CreateAPIKeyRequest,
  CreateAPIKeyResponse,
  UpdateAgentRequest,
  ProtectionConfigView,
  ProtectionConfigRequest,
  CreateWebhookRequest,
  UpdateWebhookRequest,
  TestWebhookRequest,
  TemplateView,
  TemplateSummaryView,
  CreateTemplateRequest,
  UpdateTemplateRequest,
  ValidateTemplateRequest,
  ValidateTemplateResponse,
  StarterTemplateView,
  StarterTemplateDetailView,
  DeleteAgentResult,
  DeleteMessageResult,
  DeleteDomainResult,
  DeleteWebhookResult,
  DeleteTemplateResult,
  DeleteApiKeyResult,
  Page,
  PageMessageLifecycleTransition,
  ListMessagesParams,
} from "@e2a/sdk/v1";
import type { McpConfig } from "./config.js";
import type { Scope } from "./tools/tiers.js";

const DEFAULT_LIST_LIMIT = 1000;

// Keep the SDK-owned message-filter contract in one place. MCP adds only the
// pagination cursor (consumed by AutoPager) and the agent-address selector.
type McpListMessagesParams = ListMessagesParams & {
  cursor?: string;
  explicitAddress?: string;
};

/** Per-call options for unsafe writes. */
export interface SendOpts {
  idempotencyKey?: string;
}

/**
 * Thin MCP-facing wrapper over the namespaced v1 {@link E2AClient}.
 *
 * The MCP tools and the HTTP session layer used to lean on the retired
 * flat SDK surface (`client.send`, `client.api.*`, `client.agentEmail`).
 * The v1 client is namespaced (`client.agents`, `.messages`, …) and
 * every per-agent method takes an explicit address as its first arg.
 *
 * This wrapper concentrates the address-resolution policy (a single
 * default agent email pinned at session init from the credential) and
 * the small amount of shape-mapping the tools need, so each tool stays a
 * thin pass-through and the tool contracts (names, schemas) are
 * unchanged. The session prefetch in http-server.ts still constructs one
 * of these per session and may re-pin a resolved single-agent default.
 */
export class McpClient {
  readonly sdk: E2AClient;
  /** Default per-agent address; mirrors the old flat `agentEmail`. */
  readonly agentEmail: string;
  /**
   * The connecting credential's scope (§6a tier-gating signal). Resolved at
   * session init from whoami (GET /account). Defaults to "account" (full
   * surface) so direct constructions / tests are unchanged; buildSessionClient
   * sets the real value per credential.
   */
  readonly scope: Scope;

  constructor(sdk: E2AClient, agentEmail = "", scope: Scope = "account") {
    this.sdk = sdk;
    this.agentEmail = agentEmail;
    this.scope = scope;
  }

  // resolveAddress picks the explicit per-call address, falling back to
  // the pinned default. Throws a directive error when neither resolves.
  private resolveAddress(explicit?: string): string {
    const addr = explicit || this.agentEmail;
    if (!addr) {
      throw new Error(
        "email is required — pass it explicitly, or connect with an agent-scoped credential (which resolves the agent for you). Run list_agents to see your agents.",
      );
    }
    return addr;
  }

  // ── Account / identity ──────────────────────────────────────────

  // whoami → GET /account (§6a): the authenticated principal (user + scope;
  // agent_email only for agent-scoped credentials) + plan/limits. NOT an
  // agent — discover agents via list_agents.
  whoami(): Promise<AccountView> {
    return this.sdk.account.get();
  }

  // ── Agents ──────────────────────────────────────────────────────

  // Cursor-paginated (GET /v1/agents). One page in `agents` + a next_cursor
  // when more remain; pass it back as `cursor`.
  listAgents(params: { cursor?: string; limit?: number; deleted?: boolean } = {}): Promise<Page<AgentView>> {
    const { cursor, ...rest } = params;
    return this.sdk.agents.list(rest).page(cursor);
  }

  getAgent(address: string): Promise<AgentView> {
    return this.sdk.agents.get(address);
  }

  // create_agent takes a full email (§6a / AG-1/2): a custom-domain agent on a
  // verified owned domain, or an email on the shared domain. slug/agent_mode/
  // webhook_url are gone. Returns the full AgentView.
  createAgent(body: { email: string; name?: string }): Promise<AgentView> {
    return this.sdk.agents.create(body);
  }

  // Consume the generated UpdateAgentRequest directly (no hand-declared
  // duplicate, no cast): a field removed/renamed at the API layer is a compile
  // error here. Post-reshape the only mutable agent field is the display name;
  // screening config lives on the protection sub-resource below.
  updateAgent(patch: UpdateAgentRequest, explicitAddress?: string): Promise<AgentView> {
    return this.sdk.agents.update(this.resolveAddress(explicitAddress), patch);
  }

  getProtection(explicitAddress?: string): Promise<ProtectionConfigView> {
    return this.sdk.agents.getProtection(this.resolveAddress(explicitAddress));
  }

  updateProtection(
    config: ProtectionConfigRequest,
    explicitAddress?: string,
  ): Promise<ProtectionConfigView> {
    return this.sdk.agents.replaceProtection(this.resolveAddress(explicitAddress), config);
  }

  async deleteAgent(explicitAddress?: string): Promise<DeleteAgentResult> {
    const address = this.resolveAddress(explicitAddress);
    return this.sdk.agents.delete(address);
  }

  restoreAgent(explicitAddress?: string): Promise<AgentView> {
    return this.sdk.agents.restore(this.resolveAddress(explicitAddress));
  }

  // ── Messages ────────────────────────────────────────────────────

  send(
    body: {
      to: Array<string>;
      // Literal content — required on the wire unless a template reference
      // (templateId XOR templateAlias) is used; the server enforces the
      // mutual exclusivity and returns 400 invalid_request on conflicts.
      subject?: string;
      text?: string;
      html?: string;
      templateId?: string;
      templateAlias?: string;
      templateData?: Record<string, unknown>;
      cc?: Array<string>;
      bcc?: Array<string>;
      attachments?: Array<Attachment>;
      conversationId?: string;
      replyTo?: string;
    },
    opts: SendOpts = {},
    explicitAddress?: string,
  ): Promise<SendResultView> {
    return this.sdk.messages.send(this.resolveAddress(explicitAddress), body, opts);
  }

  reply(
    messageId: string,
    body: {
      text: string;
      html?: string;
      replyAll?: boolean;
      cc?: Array<string>;
      bcc?: Array<string>;
      attachments?: Array<Attachment>;
      conversationId?: string;
      replyTo?: string;
    },
    opts: SendOpts = {},
    explicitAddress?: string,
  ): Promise<SendResultView> {
    return this.sdk.messages.reply(
      this.resolveAddress(explicitAddress),
      messageId,
      body,
      opts,
    );
  }

  forward(
    messageId: string,
    to: Array<string>,
    body: {
      text: string; // required (MSG-3): forward must carry a note; subject is derived
      html?: string;
      cc?: Array<string>;
      bcc?: Array<string>;
      attachments?: Array<Attachment>;
      conversationId?: string;
      replyTo?: string;
    },
    opts: SendOpts = {},
    explicitAddress?: string,
  ): Promise<SendResultView> {
    return this.sdk.messages.forward(
      this.resolveAddress(explicitAddress),
      messageId,
      { to, ...body },
      opts,
    );
  }

  updateMessageLabels(
    messageId: string,
    body: { addLabels?: Array<string>; removeLabels?: Array<string> },
    explicitAddress?: string,
  ): Promise<UpdateMessageResultView> {
    return this.sdk.messages.updateLabels(
      this.resolveAddress(explicitAddress),
      messageId,
      body,
    );
  }

  getMessage(messageId: string, explicitAddress?: string): Promise<MessageView> {
    return this.sdk.messages.get(this.resolveAddress(explicitAddress), messageId);
  }

  /** Beta: fetch the canonical observations for one message. */
  getMessageLifecycle(
    messageId: string,
    params: { cursor?: string; limit?: number } = {},
    explicitAddress?: string,
  ): Promise<PageMessageLifecycleTransition> {
    return this.sdk.messages.getLifecycle(
      this.resolveAddress(explicitAddress),
      messageId,
      params,
    );
  }

  // getAttachment (§6a #5): metadata + a short-lived download_url for one
  // attachment. inline:true also returns base64 `data` for small attachments.
  getAttachment(
    messageId: string,
    index: number,
    opts: { inline?: boolean } = {},
    explicitAddress?: string,
  ): Promise<AttachmentView> {
    return this.sdk.messages.getAttachment(this.resolveAddress(explicitAddress), messageId, index, opts);
  }

  // Cursor pagination (§6a #3): returns ONE page + next_cursor. `limit` is the
  // page size; pass a prior response's next_cursor as `cursor` for the next page.
  listMessages(params: McpListMessagesParams): Promise<Page<MessageSummaryView>> {
    const { explicitAddress, cursor, ...rest } = params;
    return this.sdk.messages.list(this.resolveAddress(explicitAddress), rest).page(cursor);
  }

  restoreMessage(messageId: string, explicitAddress?: string): Promise<MessageView> {
    return this.sdk.messages.restore(this.resolveAddress(explicitAddress), messageId);
  }

  deleteMessage(messageId: string, explicitAddress?: string): Promise<DeleteMessageResult> {
    // Soft delete only — the MCP surface deliberately does not expose
    // permanent=true ("delete forever"). An LLM should never be one hallucinated
    // tool call away from an irreversible purge; restore_message is the safety
    // net, and permanent deletion stays a deliberate REST/SDK action.
    return this.sdk.messages.delete(this.resolveAddress(explicitAddress), messageId);
  }

  // ── Conversations ───────────────────────────────────────────────

  listConversations(
    params: { since?: string; until?: string; cursor?: string; limit?: number },
    explicitAddress?: string,
  ): Promise<Page<ConversationSummaryView>> {
    const { cursor, ...rest } = params;
    return this.sdk.conversations.list(this.resolveAddress(explicitAddress), rest).page(cursor);
  }

  getConversation(
    conversationId: string,
    explicitAddress?: string,
  ): Promise<ConversationDetailView> {
    return this.sdk.conversations.get(
      this.resolveAddress(explicitAddress),
      conversationId,
    );
  }

  // ── Account review queue ─────────────────────────────────────────

  listReviews(params: { cursor?: string; limit?: number } = {}): Promise<Page<ReviewView>> {
    const { cursor, ...rest } = params;
    return this.sdk.reviews.list(rest).page(cursor);
  }

  getReview(messageId: string): Promise<MessageView> {
    return this.sdk.reviews.get(messageId);
  }

  async approveReview(
    messageId: string,
    overrides: {
      subject?: string;
      text?: string;
      html?: string;
      to?: Array<string>;
      cc?: Array<string>;
      bcc?: Array<string>;
      attachments?: Array<Attachment>;
    },
    opts?: SendOpts,
  ): Promise<SendResultView> {
    return opts
      ? this.sdk.reviews.approve(messageId, overrides, opts)
      : this.sdk.reviews.approve(messageId, overrides);
  }

  async rejectReview(messageId: string, reason?: string): Promise<RejectResultView> {
    return this.sdk.reviews.reject(
      messageId,
      reason !== undefined ? { reason } : {},
    );
  }

  // ── Domains ─────────────────────────────────────────────────────

  // Cursor-paginated (GET /v1/domains). One page + a next_cursor when more remain.
  listDomains(params: { cursor?: string; limit?: number } = {}): Promise<Page<DomainView>> {
    const { cursor, limit } = params;
    return this.sdk.domains.list(limit !== undefined ? { limit } : {}).page(cursor);
  }

  getDomain(domain: string): Promise<DomainView> {
    return this.sdk.domains.get(domain);
  }

  registerDomain(domain: string): Promise<DomainView> {
    return this.sdk.domains.create({ domain });
  }

  verifyDomain(domain: string): Promise<VerifyDomainView> {
    return this.sdk.domains.verify(domain);
  }

  deleteDomain(domain: string): Promise<DeleteDomainResult> {
    return this.sdk.domains.delete(domain);
  }

  // ── Webhooks ────────────────────────────────────────────────────

  // Cursor-paginated (GET /v1/webhooks). One page + a next_cursor when more remain.
  listWebhooks(params: { cursor?: string; limit?: number } = {}): Promise<Page<WebhookView>> {
    const { cursor, limit } = params;
    return this.sdk.webhooks.list(limit !== undefined ? { limit } : {}).page(cursor);
  }

  getWebhook(id: string): Promise<WebhookView> {
    return this.sdk.webhooks.get(id);
  }

  createWebhook(body: {
    url: string;
    events: Array<string>;
    description?: string;
    filters?: { agentEmails?: Array<string>; conversationIds?: Array<string>; labels?: Array<string> };
  }): Promise<CreateWebhookResponse> {
    return this.sdk.webhooks.create(body as CreateWebhookRequest);
  }

  updateWebhook(
    id: string,
    patch: {
      url?: string;
      events?: Array<string>;
      description?: string;
      enabled?: boolean;
      filters?: { agentEmails?: Array<string>; conversationIds?: Array<string>; labels?: Array<string> };
    },
  ): Promise<WebhookView> {
    return this.sdk.webhooks.update(id, patch as UpdateWebhookRequest);
  }

  deleteWebhook(id: string): Promise<DeleteWebhookResult> {
    return this.sdk.webhooks.delete(id);
  }

  rotateWebhookSecret(id: string): Promise<RotateSecretResponse> {
    return this.sdk.webhooks.rotateSecret(id);
  }

  testWebhook(id: string, body: { type?: string }): Promise<TestWebhookResponse> {
    return this.sdk.webhooks.test(id, body as TestWebhookRequest);
  }

  // Per-delivery debugging (status/attempts/last_error/last_status_code) for one
  // webhook. Cursor-paginated (GET /v1/webhooks/{id}/deliveries): one page + a
  // next_cursor when more remain. The status filter is pinned into the cursor.
  listWebhookDeliveries(
    id: string,
    params: { status?: "pending" | "delivered" | "failed"; cursor?: string; limit?: number },
  ): Promise<Page<WebhookDeliveryView>> {
    const { cursor, ...rest } = params;
    return this.sdk.webhooks.deliveries(id, rest).page(cursor);
  }

  // ── API keys ────────────────────────────────────────────────────

  // Metadata only (GET /v1/account/api-keys) — secrets appear once, at
  // creation, and are never in list rows. Cursor-paginated.
  listApiKeys(params: { cursor?: string; limit?: number } = {}): Promise<Page<APIKeyView>> {
    const { cursor, limit } = params;
    return this.sdk.account.apiKeys.list(limit !== undefined ? { limit } : {}).page(cursor);
  }

  // createAgentApiKey mints an AGENT-scoped key only. scope is hardwired here —
  // not caller-supplied — so no MCP code path can mint an account-scoped
  // (workspace-admin) credential; that stays a dashboard / raw-API action.
  // The response carries the one-time plaintext key in `.key`.
  createAgentApiKey(body: {
    agentEmail: string;
    name?: string;
    expiresAt?: Date;
  }): Promise<CreateAPIKeyResponse> {
    // Typed request, no cast: if a codegen regen ever renames/retypes `scope`,
    // this line must fail to compile rather than silently drop the field on
    // the wire (the backend defaults an omitted scope to account — admin).
    const req: CreateAPIKeyRequest = { ...body, scope: CreateAPIKeyRequestScopeEnum.Agent };
    return this.sdk.account.apiKeys.create(req);
  }

  deleteApiKey(id: string): Promise<DeleteApiKeyResult> {
    return this.sdk.account.apiKeys.delete(id);
  }

  // ── Templates (beta) ────────────────────────────────────────────
  //
  // SDK-backed (sdk.templates): same retry layer, typed E2AError mapping,
  // and camelCase views as every other tool. Both list endpoints are
  // cursor-paginated (GET /v1/templates and /v1/starter-templates).

  listTemplates(params: { cursor?: string; limit?: number } = {}): Promise<Page<TemplateSummaryView>> {
    const { cursor, limit } = params;
    return this.sdk.templates.list(limit !== undefined ? { limit } : {}).page(cursor);
  }

  getTemplate(id: string): Promise<TemplateView> {
    return this.sdk.templates.get(id);
  }

  createTemplate(body: CreateTemplateRequest): Promise<TemplateView> {
    return this.sdk.templates.create(body);
  }

  updateTemplate(id: string, patch: UpdateTemplateRequest): Promise<TemplateView> {
    return this.sdk.templates.update(id, patch);
  }

  deleteTemplate(id: string): Promise<DeleteTemplateResult> {
    return this.sdk.templates.delete(id);
  }

  validateTemplate(body: ValidateTemplateRequest): Promise<ValidateTemplateResponse> {
    return this.sdk.templates.validate(body);
  }

  listStarterTemplates(params: { cursor?: string; limit?: number } = {}): Promise<Page<StarterTemplateView>> {
    const { cursor, limit } = params;
    return this.sdk.templates.listStarters(limit !== undefined ? { limit } : {}).page(cursor);
  }

  getStarterTemplate(alias: string): Promise<StarterTemplateDetailView> {
    return this.sdk.templates.getStarter(alias);
  }

  // ── Events ──────────────────────────────────────────────────────

  listEvents(params: {
    type?: string;
    agentEmail?: string;
    conversationId?: string;
    messageId?: string;
    since?: string;
    until?: string;
    cursor?: string;
    limit?: number;
  }): Promise<Page<EventView>> {
    const { cursor, ...rest } = params;
    return this.sdk.events.list(rest).page(cursor);
  }

  getEvent(id: string): Promise<EventView> {
    return this.sdk.events.get(id);
  }

  redeliverEvent(id: string, webhookId?: string): Promise<RedeliverView> {
    return this.sdk.events.redeliver(
      id,
      webhookId !== undefined ? { webhookId } : {},
    );
  }
}

export function makeClient(cfg: McpConfig): McpClient {
  const sdk = new E2AClient({
    apiKey: cfg.apiKey,
    ...(cfg.baseUrl ? { baseUrl: cfg.baseUrl } : {}),
  });
  return new McpClient(sdk, cfg.agentEmail ?? "", "account");
}
