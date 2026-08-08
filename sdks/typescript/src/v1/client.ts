// The e2a high-level client (Slice 8b). A thin, namespaced ergonomic layer over
// the generated `generated/` base: resource sub-clients (`client.agents`,
// `.messages`, …) wrap the generated `Promise*Api` classes (composition, never
// inheritance), map the generated `ApiException` to the typed `E2AError`
// hierarchy, unwrap envelope output bodies, and expose cursor lists as an
// `AutoPager`. The generated base supplies transport (the retry-wrapped
// `HttpLibrary`), bearer auth, models, and `ApiException`.

import { createConfiguration } from "./generated/configuration.js";
import { ServerConfiguration } from "./generated/servers.js";
import { IsomorphicFetchHttpLibrary } from "./generated/http/isomorphic-fetch.js";
import { ApiException } from "./generated/apis/exception.js";
import {
  PromiseAgentsApi,
  PromiseMessagesApi,
  PromiseConversationsApi,
  PromiseDomainsApi,
  PromiseEventsApi,
  PromiseWebhooksApi,
  PromiseAccountApi,
  PromiseReviewsApi,
  PromiseTemplatesApi,
  PromiseContactsApi,
  PromiseMetaApi,
} from "./generated/types/PromiseAPI.js";
import type {
  AgentView,
  CreateAgentRequest,
  UpdateAgentRequest,
  ProtectionConfigView,
  ProtectionConfigRequest,
  MessageView,
  PageMessageLifecycleTransition,
  AgentMetricsView,
  AccountMetricsView,
  AttachmentView,
  MessageSummaryView,
  SendEmailRequest,
  ReplyRequest,
  ForwardRequest,
  ApproveRequest,
  RejectRequest,
  UpdateMessageRequest,
  UpdateMessageResultView,
  SendResultView,
  RejectResultView,
  ConversationSummaryView,
  ConversationDetailView,
  DomainView,
  RegisterDomainRequest,
  VerifyDomainView,
  EventView,
  RedeliverEventRequest,
  RedeliverView,
  WebhookView,
  CreateWebhookResponse,
  CreateWebhookRequest,
  UpdateWebhookRequest,
  RotateSecretResponse,
  TestWebhookRequest,
  TestWebhookResponse,
  WebhookDeliveryView,
  AccountView,
  UserExport,
  DeleteUserDataResult,
  DeleteAgentResult,
  DeleteMessageResult,
  DeleteDomainResult,
  DeleteSuppressionResult,
  DeleteApiKeyResult,
  DeleteTemplateResult,
  DeleteWebhookResult,
  SuppressionView,
  APIKeyView,
  CreateAPIKeyRequest,
  CreateAPIKeyResponse,
  DeploymentInfoView,
  ReviewView,
  TemplateView,
  ContactView,
  CreateContactRequest,
  UpdateContactRequest,
  DeleteContactResult,
  ImportContactsRequest,
  ContactImportResult,
  DeleteImportBatchResult,
  ContactEngagementView,
  UpsertEngagementRequest,
  DeleteEngagementResult,
  TemplateSummaryView,
  CreateTemplateRequest,
  UpdateTemplateRequest,
  ValidateTemplateRequest,
  ValidateTemplateResponse,
  StarterTemplateView,
  StarterTemplateDetailView,
  AgentSuppressionView,
  CreateAgentSuppressionRequest,
} from "./generated/index.js";
import { RetryHttpLibrary, type RetryOptions } from "./retry.js";
import { E2AError, fromApiException, connectionError } from "./errors.js";
import { AutoPager } from "./pagination.js";
import { WSStream } from "./ws.js";
import type { WebhookEvent, EmailReceivedData } from "./webhook-signature.js";
import { InboundResource } from "./inbound.js";

export interface E2AClientOptions {
  /** Account (`e2a_acct_`) or agent (`e2a_agt_`) key, or an OAuth access token.
   *  Falls back to `E2A_API_KEY`. */
  apiKey?: string;
  /** API base URL. Falls back to `E2A_API_URL`, then the deprecated
   *  `E2A_BASE_URL`. Default `https://api.e2a.dev`; override for self-host.
   *  This is the API host — not the deployment root the CLI's `E2A_URL`
   *  points at (that one also serves the dashboard). */
  baseUrl?: string;
  /** Max retry attempts on 429/5xx/connection (default 2). */
  maxRetries?: RetryOptions["maxRetries"];
  /** Optional total deadline across attempts (ms). */
  maxElapsedMs?: RetryOptions["maxElapsedMs"];
  /** Per-attempt request timeout in ms. Default 30000; pass 0 to disable. A
   *  timed-out attempt is a retryable connection failure, so it composes with
   *  maxRetries/maxElapsedMs. */
  timeoutMs?: RetryOptions["timeoutMs"];
}

/** Per-call options for unsafe writes. */
export interface RequestOptions {
  /** Stable idempotency key. Omit and the SDK mints one (and reuses it across
   *  retries). Supply a stable value derived from the triggering event to also
   *  survive a process restart. */
  idempotencyKey?: string;
}

/** Per-call options for send/reply/forward. */
export interface SendOptions extends RequestOptions {
  /** Optional bounded wait for an immediate send: `wait: "sent"` holds the
   *  request server-side until the message reaches a terminal-or-held state or
   *  at most 20 seconds elapse (currently ~15s), then returns the observed
   *  state; on timeout the result stays `status: "accepted"`. A future
   *  `sendAt` returns `status: "scheduled"` immediately and does not wait for
   *  that time. Default: no wait. Always branch on the result's `status`, not
   *  the HTTP code. */
  wait?: "sent";
}

/** Beta per-message opt-in to e2a-managed unsubscribe handling. This API and
 * its raw GET|POST /u/{token} confirmation flow may change before stable. */
export interface ManagedUnsubscribeOptions {
  mode: "managed";
}

export type SendEmailInput = Omit<SendEmailRequest, "unsubscribe"> & {
  unsubscribe?: ManagedUnsubscribeOptions;
};
export type ReplyInput = Omit<ReplyRequest, "unsubscribe"> & {
  unsubscribe?: ManagedUnsubscribeOptions;
};
export type ForwardInput = Omit<ForwardRequest, "unsubscribe"> & {
  unsubscribe?: ManagedUnsubscribeOptions;
};

function envVar(name: string): string | undefined {
  if (typeof process !== "undefined" && process.env && process.env[name]) return process.env[name];
  return undefined;
}

let warnedBaseUrlDeprecated = false;

// resolveBaseUrl reads the API host. Canonical is E2A_API_URL — the same
// concept the server names with E2A_API_URL (its externally visible API base).
// E2A_BASE_URL is the name the SDKs shipped with; still honoured so published
// integrations keep working, with a one-shot deprecation note.
function resolveBaseUrl(): string | undefined {
  const canonical = envVar("E2A_API_URL");
  if (canonical) return canonical;
  const legacy = envVar("E2A_BASE_URL");
  if (legacy && !warnedBaseUrlDeprecated) {
    warnedBaseUrlDeprecated = true;
    console.warn(
      "[e2a] E2A_BASE_URL is deprecated — rename it to E2A_API_URL. " +
        "The old name still works for now but will be dropped.",
    );
  }
  return legacy;
}

// Map generated/transport failures to the typed hierarchy: ApiException →
// envelope-mapped E2AError; an already-typed E2AError passes through; anything
// else (a transport throw from the retry layer) is a connection error.
async function call<T>(fn: () => Promise<T>): Promise<T> {
  try {
    return await fn();
  } catch (e) {
    if (e instanceof E2AError) throw e;
    if (e instanceof ApiException) throw fromApiException(e);
    throw connectionError(e instanceof Error ? e.message : String(e), e);
  }
}

export class E2AClient {
  readonly agents: AgentsResource;
  readonly messages: MessagesResource;
  readonly conversations: ConversationsResource;
  readonly domains: DomainsResource;
  readonly events: EventsResource;
  readonly webhooks: WebhooksResource;
  readonly inbound: InboundResource;
  readonly account: AccountResource;
  readonly reviews: ReviewsResource;
  readonly templates: TemplatesResource;
  readonly contacts: ContactsResource;
  private readonly meta: PromiseMetaApi;
  private readonly apiKey: string;
  private readonly baseUrl: string;

  constructor(opts: E2AClientOptions = {}) {
    const apiKey = opts.apiKey ?? envVar("E2A_API_KEY");
    if (!apiKey) {
      throw new E2AError({
        code: "no_api_key",
        message: "apiKey is required — pass { apiKey } or set E2A_API_KEY",
        status: 0,
        retryable: false,
      });
    }
    const baseUrl = opts.baseUrl ?? resolveBaseUrl() ?? "https://api.e2a.dev";
    this.apiKey = apiKey;
    this.baseUrl = baseUrl;
    const httpApi = new RetryHttpLibrary(new IsomorphicFetchHttpLibrary(), {
      maxRetries: opts.maxRetries,
      maxElapsedMs: opts.maxElapsedMs,
      // `?? 30000` defaults the timeout; an explicit 0 disables it (0 is not nullish).
      timeoutMs: opts.timeoutMs ?? 30000,
    });
    const config = createConfiguration({
      baseServer: new ServerConfiguration(baseUrl, {}),
      httpApi,
      authMethods: { bearer: { tokenProvider: { getToken: () => apiKey } } },
    });

    this.agents = new AgentsResource(new PromiseAgentsApi(config));
    this.messages = new MessagesResource(new PromiseMessagesApi(config));
    this.conversations = new ConversationsResource(new PromiseConversationsApi(config));
    this.domains = new DomainsResource(new PromiseDomainsApi(config));
    this.events = new EventsResource(new PromiseEventsApi(config));
    this.webhooks = new WebhooksResource(new PromiseWebhooksApi(config), this.messages);
    this.inbound = new InboundResource(this.messages);
    this.account = new AccountResource(new PromiseAccountApi(config));
    this.reviews = new ReviewsResource(new PromiseReviewsApi(config));
    this.templates = new TemplatesResource(new PromiseTemplatesApi(config));
    this.contacts = new ContactsResource(new PromiseContactsApi(config));
    this.meta = new PromiseMetaApi(config);
  }

  /** Public deployment metadata. */
  info(): Promise<DeploymentInfoView> {
    return call(() => this.meta.getInfo());
  }

  /**
   * Open a notification stream for an agent's inbox. Yields versioned
   * {@link WSEvent} envelopes — the same shape as webhook deliveries
   * (`email.received` today; tolerate unknown types). Fetch the body with
   * `client.inbound.fromEvent(event)` when you need the bound facade (or
   * `client.webhooks.fetchMessage(event)` for the raw MessageView).
   *
   *     for await (const event of client.listen("bot@acme.dev")) {
   *       if (!isEmailReceived(event)) continue;
   *       const email = await client.inbound.fromEvent(event);
   *     }
   */
  listen(email: string): WSStream {
    if (!email) {
      throw new E2AError({
        code: "missing_email",
        message: "email is required — pass client.listen(email)",
        status: 0,
        retryable: false,
      });
    }
    return new WSStream({ apiKey: this.apiKey, agentEmail: email, baseUrl: this.baseUrl });
  }
}

class AgentsResource {
  constructor(private readonly api: PromiseAgentsApi) {}
  list(params: { limit?: number; deleted?: boolean } = {}): AutoPager<AgentView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion.
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listAgents(cursor, params.limit, params.deleted));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  get(email: string): Promise<AgentView> {
    return call(() => this.api.getAgent(email));
  }
  create(body: CreateAgentRequest): Promise<AgentView> {
    return call(() => this.api.createAgent(body));
  }
  update(email: string, patch: UpdateAgentRequest): Promise<AgentView> {
    return call(() => this.api.updateAgent(email, patch));
  }
  /**
   * Read an agent's protection config (gate + scan sensitivity + holds). Beta;
   * account scope only — an agent-scoped key cannot read its own config.
   */
  getProtection(email: string): Promise<ProtectionConfigView> {
    return call(() => this.api.getAgentProtection(email));
  }
  /**
   * Replace an agent's protection config wholesale (all three top-level keys
   * required). Beta; account scope only.
   */
  replaceProtection(email: string, config: ProtectionConfigRequest): Promise<ProtectionConfigView> {
    return call(() => this.api.putAgentProtection(email, config));
  }
  /**
   * Move an agent to the trash by default (restorable via `restore()` within
   * the 30-day window). Pass `{ permanent: true }` to delete irreversibly
   * right away instead — accepts live and trashed agents. The typed .delete()
   * call is itself the confirmation; the ?confirm=DELETE guard exists to
   * protect raw/curl callers (AG-6). Returns the deletion receipt
   * ({deleted:true, email, messages_deleted}).
   */
  delete(email: string, opts: { permanent?: boolean } = {}): Promise<DeleteAgentResult> {
    return call(() => this.api.deleteAgent(email, "DELETE", opts.permanent));
  }
  /**
   * Restore an agent from the 30-day trash. Scheduled messages restored before
   * scheduled_at re-arm; at/after scheduled_at they return live as failed with
   * submission canceled. Account-scoped credentials only.
   */
  restore(email: string): Promise<AgentView> {
    return call(() => this.api.restoreAgent(email));
  }
  test(email: string): Promise<SendResultView> {
    return call(() => this.api.testAgent(email));
  }
  /** Beta: list recipient blocks scoped to this exact sending agent. */
  listSuppressions(email: string, params: { limit?: number } = {}): AutoPager<AgentSuppressionView> {
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listAgentSuppressions(email, cursor, params.limit));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  /** Beta: idempotently add a manual recipient block for this exact agent. */
  createSuppression(email: string, body: CreateAgentSuppressionRequest): Promise<AgentSuppressionView> {
    return call(() => this.api.createAgentSuppression(email, body));
  }
  /** Beta: remove only this exact agent-recipient block. */
  deleteSuppression(email: string, address: string): Promise<DeleteSuppressionResult> {
    return call(() => this.api.deleteAgentSuppression(email, address, "DELETE"));
  }
}

export interface ListMessagesParams {
  direction?: "inbound" | "outbound" | "all";
  readStatus?: "unread" | "read" | "all";
  sort?: "asc" | "desc";
  from_?: string;
  subjectContains?: string;
  conversationId?: string;
  labels?: string[];
  since?: string;
  until?: string;
  limit?: number;
  /** List soft-deleted messages in the trash instead of live messages. */
  deleted?: boolean;
  filter?: string;
}

/** Message operations. Scheduled sending through `sendAt` and the resulting
 * `scheduledAt` / `status: "scheduled"` fields is beta and may change before
 * it is declared stable. Managed unsubscribe is independently beta. */
class MessagesResource {
  constructor(private readonly api: PromiseMessagesApi) {}

  list(email: string, params: ListMessagesParams = {}): AutoPager<MessageSummaryView> {
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listMessages(
        email,
        params.direction,
        params.readStatus,
        params.sort,
        params.from_,
        params.subjectContains,
        params.conversationId,
        params.labels,
        params.since,
        params.until,
        cursor,
        params.limit,
        params.deleted,
        params.filter,
      ));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  get(email: string, id: string): Promise<MessageView> {
    return call(() => this.api.getMessage(email, id));
  }
  /**
   * Beta: return the ordered observations e2a recorded for one message. The
   * lifecycle contract may change before it is declared stable.
   */
  getLifecycle(
    email: string,
    messageId: string,
    params: { cursor?: string; limit?: number } = {},
  ): Promise<PageMessageLifecycleTransition> {
    return call(() =>
      this.api.getMessageLifecycle(email, messageId, params.cursor, params.limit),
    );
  }
  /**
   * Beta: counter metrics for one agent over a cohort window, aggregated from
   * the message lifecycle ledger.
   *
   * Messages belong to the window by their own creation time, not by when each
   * observation landed, so a rate never mixes numerator and denominator from
   * different populations. Bounce and complaint feedback keeps arriving for up
   * to 72 hours, so treat the most recent days as provisional.
   *
   * Each entry in `rates` is null — never 0 — when its denominator is zero, so
   * "no traffic" stays distinguishable from "everything failed".
   *
   * Defaults to the last 30 days; the window may not exceed 92 days.
   */
  getMetrics(
    email: string,
    params: { start?: Date; end?: Date } = {},
  ): Promise<AgentMetricsView> {
    return call(() => this.api.getAgentMetrics(email, params.start, params.end));
  }
  /**
   * Move a message to the trash. Reversible via `restore()` until the trash
   * retention window expires (30 days by default), so the default soft delete
   * needs no confirmation.
   *
   * Pass `{ permanent: true }` to permanently delete a message that is ALREADY
   * in the trash ("delete forever") — irreversible, account-scoped credentials
   * only. The typed .delete() call is itself the confirmation; the SDK supplies
   * the ?confirm=DELETE guard the raw API requires on that path (the query
   * guard exists to protect raw/curl callers). It is ignored when permanent is
   * unset.
   *
   * A message held for review cannot be deleted (409 message_held) — resolve it
   * on the review queue first. Returns the deletion receipt ({deleted:true, id}).
   */
  delete(email: string, id: string, opts: { permanent?: boolean } = {}): Promise<DeleteMessageResult> {
    return call(() => this.api.deleteMessage(email, id, opts.permanent, "DELETE"));
  }
  /**
   * Restore a soft-deleted message. A scheduled message restored before
   * scheduled_at re-arms; at/after scheduled_at it returns live as failed with
   * submission canceled.
   */
  restore(email: string, id: string): Promise<MessageView> {
    return call(() => this.api.restoreMessage(email, id));
  }
  // getAttachment returns one attachment's metadata + a short-lived download_url
  // (+ expires_at). Pass { inline: true } to also receive base64 `data` for small
  // attachments (the server caps inline; larger requests error). Fetch the bytes
  // out of band via download_url so they never stream through an agent's context.
  getAttachment(email: string, id: string, index: number, opts: { inline?: boolean } = {}): Promise<AttachmentView> {
    return call(() => this.api.getAttachment(email, id, index, opts.inline));
  }
  send(email: string, body: SendEmailInput, opts: SendOptions = {}): Promise<SendResultView> {
    return call(() => this.api.sendMessage(email, body as SendEmailRequest, opts.idempotencyKey, opts.wait));
  }
  reply(email: string, messageId: string, body: ReplyInput, opts: SendOptions = {}): Promise<SendResultView> {
    return call(() => this.api.replyToMessage(email, messageId, body as ReplyRequest, opts.idempotencyKey, opts.wait));
  }
  forward(email: string, messageId: string, body: ForwardInput, opts: SendOptions = {}): Promise<SendResultView> {
    return call(() => this.api.forwardMessage(email, messageId, body as ForwardRequest, opts.idempotencyKey, opts.wait));
  }
  // Approve/reject a held message live on the account-scoped review queue —
  // `client.reviews.approve(id, body)` / `client.reviews.reject(id, body)`. The
  // deprecated per-inbox messages.approve/reject was removed in the pre-GA
  // vocabulary freeze (a review is addressed by message id alone).
  updateLabels(email: string, id: string, body: UpdateMessageRequest): Promise<UpdateMessageResultView> {
    return call(() => this.api.updateMessage(email, id, body));
  }
}

/** The account-scoped human-review queue: every message held in
 *  pending_review (outbound drafts awaiting send approval + inbound messages
 *  held by a screening gate). Supersedes the per-inbox messages.approve/reject
 *  path — reviews are addressed by message id alone, no inbox email needed.
 *  Account-scoped credentials only; an agent cannot see or resolve holds. */
class ReviewsResource {
  constructor(private readonly api: PromiseReviewsApi) {}
  /** List every held message across the account's inboxes. */
  list(params: { limit?: number } = {}): AutoPager<ReviewView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion.
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listReviews(cursor, params.limit));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  /** Full detail of one held message (body + recipients + screening context). */
  get(id: string): Promise<MessageView> {
    return call(() => this.api.getReview(id));
  }
  /** Approve a hold: send the outbound draft (honoring Idempotency-Key +
   *  optional reviewer overrides) or release the inbound hold to the inbox. */
  approve(messageId: string, body: ApproveRequest = {}, opts: RequestOptions = {}): Promise<SendResultView> {
    return call(() => this.api.approveReview(messageId, body, opts.idempotencyKey));
  }
  /** Reject a hold: discard the outbound draft / drop the inbound hold. */
  reject(id: string, body: RejectRequest = {}): Promise<RejectResultView> {
    return call(() => this.api.rejectReview(id, body));
  }
}

/** Reusable email templates + the read-only starter catalog (beta — shapes may
 *  change before templates are declared stable). Account scope only; the
 *  send-side reference lives on `messages.send` (template_id / template_alias /
 *  template_data, mutually exclusive with literal subject/text). */
class TemplatesResource {
  constructor(private readonly api: PromiseTemplatesApi) {}
  /** List the account's stored templates, newest first. Summary rows only (no
   *  text/html sources) — `get(id)` returns the full sources. */
  list(params: { limit?: number } = {}): AutoPager<TemplateSummaryView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion.
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listTemplates(cursor, params.limit));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  /** Fetch one stored template by id (tmpl_…), including its sources. */
  get(id: string): Promise<TemplateView> {
    return call(() => this.api.getTemplate(id));
  }
  /** Create a template from literal source (name + subject + text), or copy a
   *  starter verbatim via `fromStarter` (mutually exclusive with the source
   *  fields — edit the created copy afterwards with `update`). */
  create(body: CreateTemplateRequest): Promise<TemplateView> {
    return call(() => this.api.createTemplate(body));
  }
  /** Partial update; omitted fields are left unchanged. Changed parts are
   *  re-parsed. Set alias or html to "" to clear them. */
  update(id: string, patch: UpdateTemplateRequest): Promise<TemplateView> {
    return call(() => this.api.updateTemplate(id, patch));
  }
  delete(id: string): Promise<DeleteTemplateResult> {
    // The typed .delete() call is itself the confirmation; the SDK supplies the
    // ?confirm=DELETE guard the raw API requires so callers aren't burdened.
    // Returns the deletion object ({deleted:true, id}).
    return call(() => this.api.deleteTemplate(id, "DELETE"));
  }
  /** Dry-run template source without persisting: per-part parse errors, a
   *  rendered preview against testData (present only when valid), and
   *  suggestedData — a nested placeholder object covering every variable the
   *  source references. */
  validate(body: ValidateTemplateRequest): Promise<ValidateTemplateResponse> {
    return call(() => this.api.validateTemplate(body));
  }
  /** List the pre-built starter templates shipped with the deployment (catalog
   *  metadata + variables; `getStarter(alias)` adds the full body sources). */
  listStarters(params: { limit?: number } = {}): AutoPager<StarterTemplateView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion.
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listStarterTemplates(cursor, params.limit));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  /** Fetch one starter by alias, including its full body sources. Starters are
   *  read-only masters — copy one with `create({ fromStarter: alias })`. */
  getStarter(alias: string): Promise<StarterTemplateDetailView> {
    return call(() => this.api.getStarterTemplate(alias));
  }
}

class ContactsResource {
  constructor(private readonly api: PromiseContactsApi) {}
  /** List the people this account corresponds with, newest first. Optionally
   *  narrow by provenance, upload, or creation-time window. */
  list(params: {
    source?: string;
    importBatchId?: string;
    createdAfter?: Date;
    createdBefore?: Date;
    limit?: number;
  } = {}): AutoPager<ContactView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion.
    return new AutoPager(async (cursor) => {
      const page = await call(() =>
        this.api.listContacts(
          params.source,
          params.importBatchId,
          params.createdAfter,
          params.createdBefore,
          cursor,
          params.limit,
        ));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  /** Fetch one contact. `address` may be a bare address or a display-name form
   *  ("A. Partner <partner@fund.vc>") — both resolve to the same contact. */
  get(address: string): Promise<ContactView> {
    return call(() => this.api.getContact(address));
  }
  /** Fetch one contact together with the current optimistic-concurrency
   *  validator. Pass `etag` back as `ifMatch` on update to reject a stale
   *  editor instead of silently overwriting a newer change. */
  async getWithETag(address: string): Promise<{ data: ContactView; etag?: string }> {
    const response = await call(() => this.api.getContactWithHttpInfo(address));
    return { data: response.data, etag: response.headers.etag ?? response.headers.ETag };
  }
  /** Create one contact. The address is canonicalized, so creating the same
   *  person twice (in any form) is a 409 rather than a duplicate row. */
  create(body: CreateContactRequest, opts: RequestOptions = {}): Promise<ContactView> {
    return call(() => this.api.createContact(body, opts.idempotencyKey));
  }
  /** Partial update; omitted fields are left unchanged, so editing the name
   *  never erases metadata. Address and provenance are immutable. */
  update(
    address: string,
    patch: UpdateContactRequest,
    opts: { ifMatch?: string } = {},
  ): Promise<ContactView> {
    return call(() => this.api.updateContact(address, patch, opts.ifMatch));
  }
  delete(address: string): Promise<DeleteContactResult> {
    // The typed .delete() call is itself the confirmation; the SDK supplies the
    // ?confirm=DELETE guard the raw API requires.
    return call(() => this.api.deleteContact(address, "DELETE"));
  }
  /** Import up to 1000 contacts in one request. Every submitted row gets its own
   *  result, so one bad line never rejects the upload. Import is inert — it
   *  records identity and sends nothing. Rows omitting a field keep the stored
   *  value, so a narrower re-upload does not erase columns it no longer carries. */
  import(body: ImportContactsRequest, opts: RequestOptions = {}): Promise<ContactImportResult> {
    return call(() => this.api.importContacts(body, opts.idempotencyKey));
  }
  /** Reverse an import, removing untouched contacts and agent enrolments it created. */
  deleteImport(batchId: string): Promise<DeleteImportBatchResult> {
    return call(() => this.api.deleteImportBatch(batchId, "DELETE"));
  }

  // ── Per-agent outreach ────────────────────────────────────────────────────
  // Engagements are one agent's relationship with a contact. Unlike the
  // account-level methods above, an agent-scoped credential may drive these for
  // its own agent — that is the outreach loop.

  /** List the contacts an agent is working, with the reply and delivery facts
   *  e2a derives from real message activity.
   *
   *  For a follow-up sweep pass `replied: false` together with BOTH
   *  `nextActionBefore` and `lastOutboundBefore`. `lastOutboundAt` is
   *  server-maintained, so including it drops anyone just contacted even if
   *  your own state write was lost — omit it and a failed write can send twice. */
  outreach(
    email: string,
    params: {
      stage?: string;
      replied?: boolean;
      suppressed?: boolean;
      nextActionBefore?: Date;
      lastOutboundBefore?: Date;
      limit?: number;
    } = {},
  ): AutoPager<ContactEngagementView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion.
    return new AutoPager(async (cursor) => {
      const page = await call(() =>
        this.api.listEngagements(
          email,
          params.stage,
          params.replied === undefined ? undefined : (params.replied ? "true" : "false"),
          params.suppressed === undefined ? undefined : (params.suppressed ? "true" : "false"),
          params.nextActionBefore,
          params.lastOutboundBefore,
          cursor,
          params.limit,
        ));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }

  /** Fetch one agent's outreach record for a contact. */
  getOutreach(email: string, address: string): Promise<ContactEngagementView> {
    return call(() => this.api.getEngagement(email, address));
  }
  /** Fetch one outreach record with its current validator for a guarded
   *  read-modify-write loop. */
  async getOutreachWithETag(
    email: string,
    address: string,
  ): Promise<{ data: ContactEngagementView; etag?: string }> {
    const response = await call(() => this.api.getEngagementWithHttpInfo(email, address));
    return { data: response.data, etag: response.headers.etag ?? response.headers.ETag };
  }

  /** Enrol a contact in an agent's outreach, or update the agent-owned fields.
   *  Omitted fields are left unchanged, so advancing the stage after a send
   *  does not disturb the schedule. */
  setOutreach(
    email: string,
    address: string,
    body: UpsertEngagementRequest,
    opts: { ifMatch?: string } = {},
  ): Promise<ContactEngagementView> {
    return call(() => this.api.upsertEngagement(email, address, body, opts.ifMatch));
  }

  /** Un-enrol a contact from an agent's outreach. The contact itself survives,
   *  and suppressions are untouched — this is not consent. */
  deleteOutreach(email: string, address: string): Promise<DeleteEngagementResult> {
    return call(() => this.api.deleteEngagement(email, address, "DELETE"));
  }
}

class ConversationsResource {
  constructor(private readonly api: PromiseConversationsApi) {}
  // Returns an AutoPager for ergonomic consistency with every other `.list()`.
  // Cursor-paginated (CV-3): the AutoPager walks next_cursor to completion.
  list(email: string, params: { since?: string; until?: string; limit?: number } = {}): AutoPager<ConversationSummaryView> {
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listConversations(email, params.since, params.until, cursor, params.limit));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  get(email: string, id: string): Promise<ConversationDetailView> {
    return call(() => this.api.getConversation(email, id));
  }
}

class DomainsResource {
  constructor(private readonly api: PromiseDomainsApi) {}
  list(params: { limit?: number } = {}): AutoPager<DomainView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion.
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listDomains(cursor, params.limit));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  get(domain: string): Promise<DomainView> {
    return call(() => this.api.getDomain(domain));
  }
  create(body: RegisterDomainRequest): Promise<DomainView> {
    return call(() => this.api.registerDomain(body));
  }
  delete(domain: string): Promise<DeleteDomainResult> {
    // Returns the deletion object ({deleted:true, domain}).
    return call(() => this.api.deleteDomain(domain, "DELETE"));
  }
  verify(domain: string): Promise<VerifyDomainView> {
    return call(() => this.api.verifyDomain(domain));
  }
}

export interface ListEventsParams {
  type?: string;
  agentEmail?: string;
  conversationId?: string;
  messageId?: string;
  since?: string;
  until?: string;
  limit?: number;
}

class EventsResource {
  constructor(private readonly api: PromiseEventsApi) {}
  list(params: ListEventsParams = {}): AutoPager<EventView> {
    return new AutoPager(async (cursor) => {
      const page = await call(() =>
        this.api.listEvents(params.type, params.agentEmail, params.conversationId, params.messageId,
          params.since, params.until, cursor, params.limit),
      );
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  get(id: string): Promise<EventView> {
    return call(() => this.api.getEvent(id));
  }
  redeliver(id: string, body: RedeliverEventRequest = {}): Promise<RedeliverView> {
    return call(() => this.api.redeliverEvent(id, body));
  }
}

class WebhooksResource {
  constructor(
    private readonly api: PromiseWebhooksApi,
    private readonly messages: MessagesResource,
  ) {}

  /**
   * Fetch the full message referenced by an `email.received` event. The event
   * is a metadata-only notification; this resolves its (delivered_to, message_id)
   * fetch keys and returns the full {@link MessageView} (body, attachments,
   * signed headers). Throws if the event is not an `email.received` carrying
   * those keys.
   */
  fetchMessage(event: WebhookEvent): Promise<MessageView> {
    const d = event.data as EmailReceivedData | undefined;
    if (event.type !== "email.received" || !d?.message_id || !d?.delivered_to) {
      throw new Error(
        "fetchMessage expects an email.received event with message_id and delivered_to",
      );
    }
    return this.messages.get(d.delivered_to, d.message_id);
  }

  list(params: { limit?: number } = {}): AutoPager<WebhookView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion.
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listWebhooks(cursor, params.limit));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  get(id: string): Promise<WebhookView> {
    return call(() => this.api.getWebhook(id));
  }
  // create returns the one-time signing secret in `.signingSecret` — store it
  // now. Server-deduped via Idempotency-Key: a keyed retry replays the same
  // webhook (id + secret) instead of registering a second subscription, so the
  // retry layer can safely re-send ambiguous transport failures. Omit
  // opts.idempotencyKey and the SDK mints one per call (intentional duplicate
  // subscriptions — even to the same URL — stay expressible: each create call
  // gets its own key).
  create(body: CreateWebhookRequest, opts: RequestOptions = {}): Promise<CreateWebhookResponse> {
    return call(() => this.api.createWebhook(body, opts.idempotencyKey));
  }
  update(id: string, patch: UpdateWebhookRequest): Promise<WebhookView> {
    return call(() => this.api.updateWebhook(id, patch));
  }
  delete(id: string): Promise<DeleteWebhookResult> {
    // The typed .delete() call is itself the confirmation; the SDK supplies the
    // ?confirm=DELETE guard the raw API requires so callers aren't burdened.
    // Returns the deletion object ({deleted:true, id}).
    return call(() => this.api.deleteWebhook(id, "DELETE"));
  }
  rotateSecret(id: string): Promise<RotateSecretResponse> {
    return call(() => this.api.rotateWebhookSecret(id));
  }
  test(id: string, body: TestWebhookRequest = {}): Promise<TestWebhookResponse> {
    return call(() => this.api.testWebhook(id, body));
  }
  deliveries(id: string, params: { status?: "pending" | "delivered" | "failed"; limit?: number } = {}): AutoPager<WebhookDeliveryView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion. The status
    // filter is pinned into the cursor server-side (a continuation must not
    // change it), which the AutoPager honors by keeping status constant.
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listWebhookDeliveries(id, params.status, cursor, params.limit));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
}

class SuppressionsResource {
  constructor(private readonly api: PromiseAccountApi) {}
  list(params: { limit?: number } = {}): AutoPager<SuppressionView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion.
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listSuppressions(cursor, params.limit));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  delete(email: string): Promise<DeleteSuppressionResult> {
    // The typed .delete() call is itself the confirmation; the SDK supplies the
    // ?confirm=DELETE guard the raw API requires so callers aren't burdened.
    // Returns the deletion object ({deleted:true, address}).
    return call(() => this.api.deleteSuppression(email, "DELETE"));
  }
}

class APIKeysResource {
  constructor(private readonly api: PromiseAccountApi) {}
  list(params: { limit?: number } = {}): AutoPager<APIKeyView> {
    // Cursor-paginated: the AutoPager walks next_cursor to completion.
    return new AutoPager(async (cursor) => {
      const page = await call(() => this.api.listApiKeys(cursor, params.limit));
      return { items: page.items ?? [], next_cursor: page.nextCursor };
    });
  }
  // create returns the one-time plaintext key in `.key` — store it now. The
  // server replays the same credential for a keyed retry, so the SDK can safely
  // retry an ambiguous transport failure without minting twice.
  create(body: CreateAPIKeyRequest, opts: RequestOptions = {}): Promise<CreateAPIKeyResponse> {
    return call(() => this.api.createApiKey(body, opts.idempotencyKey));
  }
  delete(id: string): Promise<DeleteApiKeyResult> {
    // The typed .delete() call is itself the confirmation; the SDK supplies the
    // ?confirm=DELETE guard the raw API requires so callers aren't burdened.
    // Returns the deletion object ({deleted:true, id}).
    return call(() => this.api.deleteApiKey(id, "DELETE"));
  }
}

class AccountResource {
  readonly suppressions: SuppressionsResource;
  readonly apiKeys: APIKeysResource;
  constructor(private readonly api: PromiseAccountApi) {
    this.suppressions = new SuppressionsResource(api);
    this.apiKeys = new APIKeysResource(api);
  }
  get(): Promise<AccountView> {
    return call(() => this.api.getAccount());
  }
  export(): Promise<UserExport> {
    return call(() => this.api.exportAccount());
  }
  /**
   * Beta: counter metrics across every agent this account owns, on the same
   * cohort-window and denominator contract as `messages.getMetrics()` — so an
   * account total and the per-agent numbers under it can never disagree about
   * what a rate means.
   *
   * Pass `{ groupBy: "agent" }` for a per-agent breakdown, busiest first. It is
   * capped at 200 agents and sets `agentsTruncated` when it cuts; the totals
   * stay complete regardless.
   *
   * Account-scoped credentials only — an agent-scoped key reads its own agent
   * through `messages.getMetrics()` instead.
   */
  metrics(
    params: { start?: Date; end?: Date; groupBy?: "agent" } = {},
  ): Promise<AccountMetricsView> {
    return call(() =>
      this.api.getAccountMetrics(params.start, params.end, params.groupBy),
    );
  }
  delete(): Promise<DeleteUserDataResult> {
    // Irreversible. The typed .delete() call is the confirmation; the SDK
    // supplies the ?confirm=DELETE guard the raw API requires. Returns the
    // deletion receipt ({deleted:true} plus per-table cascade counts).
    return call(() => this.api.deleteAccount("DELETE"));
  }
}
