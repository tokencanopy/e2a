// Thin fetch helpers for all onboarding-related API calls.
// Every function returns the parsed JSON or throws with the server error message.

import type {
  DomainInfo,
  AgentCreateResponse,
  ProtectionConfig,
} from "./types";
import type {
  WebhookDeliveryView,
  WebhookView,
} from "../../../lib/webhooks";
import type {
  AttachmentMeta,
  DashboardAgent,
  InboundMessageDetail,
  ListMessagesResponse,
  PendingMessageSummary,
  PendingMessageDetail,
  PendingAttachment,
} from "../types";

/** Thrown by `request` on any non-2xx HTTP response. Carries the raw
 *  status code so callers can branch on 404 vs 500 vs 401. */
export class ApiError extends Error {
  readonly status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

async function request<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, {
    credentials: "include",
    ...init,
    headers: { "Content-Type": "application/json", ...init?.headers },
  });
  if (!res.ok) {
    const text = await res.text();
    throw new ApiError(text || `Request failed (${res.status})`, res.status);
  }
  // Successful mutation endpoints may return no body.
  if (res.status === 204) return undefined as T;

  const textFn = (res as Response & { text?: () => Promise<string> }).text;
  if (typeof textFn === "function") {
    const text = await textFn.call(res);
    if (!text) return undefined as T;
    return JSON.parse(text) as T;
  }

  return res.json();
}

// ── Domains ──────────────────────────────────────────────

export async function listDomains(): Promise<DomainInfo[]> {
  const data = await request<{ items: DomainInfo[] }>("/v1/domains");
  return data.items ?? [];
}

export async function registerDomain(domain: string): Promise<DomainInfo> {
  return request<DomainInfo>("/v1/domains", {
    method: "POST",
    body: JSON.stringify({ domain }),
  });
}

export async function verifyDomain(
  domain: string,
): Promise<import("./types").VerifyDomainResponse> {
  return request("/v1/domains/" + encodeURIComponent(domain) + "/verify", {
    method: "POST",
  });
}

// DELETE /v1/domains/{domain}. The v1 surface guards destructive deletes
// behind an explicit `?confirm=DELETE` query param.
export async function deleteDomain(domain: string): Promise<void> {
  return request("/v1/domains/" + encodeURIComponent(domain) + "?confirm=DELETE", {
    method: "DELETE",
  });
}


// ── Agents ───────────────────────────────────────────────

// GET /v1/agents → PageAgentView. AgentView carries exactly the slim
// identity fields the dashboard list needs (id/domain/email/name/
// domain_verified/created_at), so the wire rows map straight onto
// DashboardAgent. Follow next_cursor so account-wide consumers see every
// inbox, with the same defensive page cap used by the templates surface.
// (Per-agent config moved to the protection sub-resource.)
export async function listAgents(): Promise<DashboardAgent[]> {
  const agents: DashboardAgent[] = [];
  let cursor: string | null = null;
  for (let page = 0; page < 20; page++) {
    const url: string = cursor
      ? "/v1/agents?cursor=" + encodeURIComponent(cursor)
      : "/v1/agents";
    const data: {
      items?: DashboardAgent[] | null;
      next_cursor?: string | null;
    } = await request(url);
    agents.push(...(data.items ?? []));
    cursor = data.next_cursor ?? null;
    if (!cursor) break;
  }
  if (cursor) {
    throw new Error("Failed to load all agents: pagination exceeded 20 pages");
  }
  return agents;
}

// POST /v1/agents registers by full email only — a shared-domain agent is
// an email on the deployment's shared domain; the legacy `slug` field was
// dropped from the API and is rejected as an unexpected property.
export async function createAgent(params: {
  email: string;
  name?: string;
}): Promise<AgentCreateResponse> {
  return request<AgentCreateResponse>("/v1/agents", {
    method: "POST",
    body: JSON.stringify(params),
  });
}

// DELETE /v1/agents/{email}. The v1 surface guards destructive deletes
// behind an explicit `?confirm=DELETE` query param and returns 200 with a
// deletion receipt ({deleted:true, email, messages_deleted}); the dashboard
// doesn't consume it, so this stays Promise<void>. The default delete moves
// the inbox to the trash, where it remains restorable for about 30 days.
export async function deleteAgent(email: string): Promise<void> {
  return request(
    "/v1/agents/" + encodeURIComponent(email) + "?confirm=DELETE",
    { method: "DELETE" },
  );
}

// GET /v1/agents?deleted=true — the inbox trash: soft-deleted agents,
// restorable until the janitor purges them (~30 days after deletion).
// Rows carry `deleted_at`.
export async function listDeletedAgents(): Promise<DashboardAgent[]> {
  const data = await request<{ items?: DashboardAgent[] | null }>(
    "/v1/agents?deleted=true",
  );
  return data.items ?? [];
}

// POST /v1/agents/{email}/restore — bring a trashed inbox back, messages
// and configuration intact.
export async function restoreAgent(email: string): Promise<DashboardAgent> {
  return request<DashboardAgent>(
    "/v1/agents/" + encodeURIComponent(email) + "/restore",
    { method: "POST" },
  );
}

// DELETE /v1/agents/{email}?permanent=true — irreversible ("delete
// forever" from the trash view).
export async function permanentDeleteAgent(email: string): Promise<void> {
  return request(
    "/v1/agents/" +
      encodeURIComponent(email) +
      "?confirm=DELETE&permanent=true",
    { method: "DELETE" },
  );
}

// Fires a synthetic "Test email from e2a" send to the agent's own
// address — exercises outbound SMTP + inbound delivery + webhook (or
// WebSocket for local agents). Used by the dashboard card's Test
// button. Routes through `request<T>` so 4xx/5xx surface as `ApiError`
// with the server's body text, consistent with every other mutation.
export async function sendAgentTestEmail(
  email: string,
): Promise<{ status: string; message_id: string }> {
  return request(
    "/v1/agents/" + encodeURIComponent(email) + "/test",
    { method: "POST" },
  );
}

// Wire shape of a row in PageMessageSummaryView.items
// (GET /v1/agents/{address}/messages). Mirrors MessageSummaryView in
// api/openapi.yaml. Kept local; the dashboard projects it into the
// MessageSummary type the UI reads.
type MessageSummaryWire = {
  id: string;
  direction: "inbound" | "outbound";
  header_from: string | null;
  envelope_from: string | null;
  verified_domain: string | null;
  to?: string[] | null;
  cc?: string[] | null;
  reply_to?: string[] | null;
  delivered_to: string;
  subject: string;
  conversation_id?: string;
  // v1 splits message state into delivery rollup (delivery_status) and the
  // review/HITL lifecycle (review_status). Both optional on the wire.
  delivery_status?: string;
  review_status?: string;
  read_status?: string;
  webhook_status?: string;
  webhook_error?: string;
  size_bytes?: number;
  created_at: string;
  deleted_at?: string;
  // Future submit instant for a scheduled outbound send (outbound-only,
  // present only while a future send_at is set). Drives the "Scheduled" chip.
  scheduled_at?: string;
};

type PageMessageSummaryWire = {
  items?: MessageSummaryWire[] | null;
  next_cursor?: string | null;
};

function projectSummary(w: MessageSummaryWire): import("../types").MessageSummary {
  return {
    id: w.id,
    direction: w.direction,
    from: w.header_from ?? "",
    verified_domain: w.verified_domain,
    to: w.to ?? [],
    cc: w.cc ?? undefined,
    reply_to: w.reply_to ?? undefined,
    recipient: w.delivered_to,
    subject: w.subject,
    conversation_id: w.conversation_id,
    // App keeps `status` (delivery rollup) + `review_status` (review
    // lifecycle) field names; v1 sources them from delivery_status /
    // review_status.
    status: w.delivery_status ?? "",
    review_status: w.review_status,
    // Inbound unread state lives in read_status on v1 (delivery_status is
    // outbound-only); the inbox's unread affordance reads this.
    read_status: w.read_status,
    webhook_status: w.webhook_status,
    webhook_error: w.webhook_error,
    size_bytes: w.size_bytes,
    created_at: w.created_at,
    deleted_at: w.deleted_at,
    scheduled_at: w.scheduled_at,
  };
}

// Dashboard inbox + SDK polling share this endpoint
// (GET /v1/agents/{address}/messages). Cursor pagination: pass `cursor`
// (the prior page's next_cursor) to fetch the next page. `direction=all`
// fetches mixed inbound+outbound newest-first.
export async function listAgentMessages(
  email: string,
  opts: {
    direction?: "all" | "inbound" | "outbound";
    status?: "all" | "unread" | "read";
    pageSize?: number;
    cursor?: string;
    // Trash view: soft-deleted messages only (server defaults the trash
    // view to direction=all / status=all).
    deleted?: boolean;
  } = {},
): Promise<ListMessagesResponse> {
  const params = new URLSearchParams();
  if (opts.direction) params.set("direction", opts.direction);
  // `status` is inbound-only in /v1; sending it on direction=all/outbound
  // is harmless but we only forward it when meaningful.
  if (opts.status && opts.direction !== "outbound") params.set("status", opts.status);
  if (opts.pageSize) params.set("limit", String(opts.pageSize));
  if (opts.cursor) params.set("cursor", opts.cursor);
  if (opts.deleted) params.set("deleted", "true");
  const qs = params.toString();
  const page = await request<PageMessageSummaryWire>(
    "/v1/agents/" + encodeURIComponent(email) + "/messages" + (qs ? "?" + qs : ""),
  );
  return {
    items: (page.items ?? []).map(projectSummary),
    next_cursor: page.next_cursor ?? null,
  };
}

// DELETE /v1/agents/{email}/messages/{id} — move a message to the trash
// (reversible for ~30 days, so no confirm is required). A held
// (pending_review) message 409s — resolve it in the review queue first.
export async function deleteMessage(email: string, id: string): Promise<void> {
  return request(
    "/v1/agents/" +
      encodeURIComponent(email) +
      "/messages/" +
      encodeURIComponent(id),
    { method: "DELETE" },
  );
}

// POST /v1/agents/{email}/messages/{id}/restore — bring a trashed message
// back to the inbox.
export async function restoreMessage(email: string, id: string): Promise<void> {
  await request(
    "/v1/agents/" +
      encodeURIComponent(email) +
      "/messages/" +
      encodeURIComponent(id) +
      "/restore",
    { method: "POST" },
  );
}

// DELETE …?permanent=true&confirm=DELETE — permanently delete a message
// that is already in the trash ("delete forever").
export async function purgeMessage(email: string, id: string): Promise<void> {
  return request(
    "/v1/agents/" +
      encodeURIComponent(email) +
      "/messages/" +
      encodeURIComponent(id) +
      "?permanent=true&confirm=DELETE",
    { method: "DELETE" },
  );
}

// Max unread we count before showing "N+" — keeps the per-card probe a
// small fixed page instead of walking the whole unread backlog.
export const UNREAD_BADGE_CAP = 99;

// Inbox-level unread rollup for the Inboxes list. Asks the messages
// endpoint for unread inbound rows (read_status=unread is the backend's
// inbound read-state filter — MSG-1) and reports how many there are. We
// pull one capped page (limit=CAP) and treat a returned cursor as "more
// than CAP" so the card can render "99+". One call per inbox card
// (Option A); a native per-agent unread count on GET /v1/agents would
// replace this later.
export async function getInboxUnread(
  email: string,
): Promise<{ count: number; more: boolean }> {
  const page = await request<PageMessageSummaryWire>(
    "/v1/agents/" +
      encodeURIComponent(email) +
      "/messages?direction=inbound&read_status=unread&limit=" +
      UNREAD_BADGE_CAP,
  );
  const count = (page.items ?? []).length;
  return { count, more: Boolean(page.next_cursor) };
}

// Wire shape of MessageView, returned by BOTH message-detail endpoints
// (GET /v1/agents/{address}/messages/{id} and GET /v1/reviews/{id}).
//
// This raw wire — never a projection — is what the SWR cache holds under
// messageDetailKey(id). Two surfaces fetch the same message through
// different endpoints, so caching a projection meant one surface could
// read the other's shape and crash on a missing field. Caching the wire
// and projecting at the point of use makes the shape uniform by
// construction. See lib/swrKeys.ts.
export type MessageViewWire = {
  id: string;
  header_from: string | null;
  envelope_from: string | null;
  verified_domain: string | null;
  authentication: import("../types").EmailAuthentication | null;
  to?: string[] | null;
  cc?: string[] | null;
  reply_to?: string[] | null;
  delivered_to: string;
  subject: string;
  conversation_id?: string;
  direction: "inbound" | "outbound";
  delivery_status?: string;
  review_status?: string;
  read_status?: string;
  // Populated only on the review-detail surface; agent message reads never
  // return review context.
  hold_reason?: PendingMessageDetail["hold_reason"];
  // Screening breakdown (detector categories + rationale) — review detail only.
  protection?: {
    source: string;
    action?: string;
    detector?: string;
    score?: number | null;
    categories?: { name: string; score?: number }[];
    summary?: string;
  }[];
  created_at: string;
  body?: { text?: string; html?: string };
  // Backend-derived body: `text` (injection-reduced) for any message with raw
  // MIME, plus `html` (decoded text/html part) for rich display when present.
  parsed?: { text?: string; html?: string };
  attachments?: AttachmentMeta[];
  raw_message?: string;
};

// Projects a MessageView into the PendingMessageDetail shape the review
// surfaces read. Fields the `/v1` MessageView doesn't expose
// (attachments, the parent inbound context, the reviewer identity) come
// through undefined — the UI degrades gracefully, hiding those
// affordances.
export function projectPending(
  email: string,
  w: MessageViewWire,
): PendingMessageDetail {
  return {
    id: w.id,
    agent_email: email,
    direction: "outbound",
    subject: w.subject,
    conversation_id: w.conversation_id,
    to: w.to ?? [],
    cc: w.cc ?? undefined,
    // A held draft's lifecycle state is the review_status (pending_review);
    // the delivery rollup is empty until it's approved + sent.
    status: w.review_status ?? "",
    created_at: w.created_at,
    hold_reason: w.hold_reason,
    protection: w.protection,
    // Outbound drafts and terminal outbound messages retain `body`; inbound
    // content is exposed through `parsed`. Fall back for legacy rows.
    body_text: w.body?.text ?? w.parsed?.text,
    body_html: w.body?.html ?? w.parsed?.html,
    attachments: w.attachments ?? [],
  };
}

// Projects a MessageView into the inbound InboundMessageDetail shape the
// received-mail surfaces read.
export function projectInbound(
  w: MessageViewWire,
): InboundMessageDetail {
  return {
    id: w.id,
    direction: "inbound",
    header_from: w.header_from,
    envelope_from: w.envelope_from,
    verified_domain: w.verified_domain,
    authentication: w.authentication,
    to: w.to ?? [],
    cc: w.cc ?? [],
    reply_to: w.reply_to ?? [],
    recipient: w.delivered_to,
    subject: w.subject,
    conversation_id: w.conversation_id ?? "",
    review_status: w.review_status,
    status: w.read_status ?? "",
    created_at: w.created_at,
    parsed: w.parsed,
    body: w.body,
    attachments: w.attachments ?? [],
    raw_message: w.raw_message ?? "",
  };
}

// A MessageView projected by its authoritative wire direction. The optional
// fallback keeps older cached payloads and pre-contract deep links readable.
export type LoadedMessageDetail =
  | { direction: "outbound"; data: PendingMessageDetail }
  | { direction: "inbound"; data: InboundMessageDetail };

export function projectMessageDetail(
  email: string,
  w: MessageViewWire,
  fallbackDirection: "inbound" | "outbound" = "inbound",
): LoadedMessageDetail {
  const direction = w.direction ?? fallbackDirection;
  return direction === "outbound"
    ? { direction: "outbound", data: projectPending(email, w) }
    : { direction: "inbound", data: projectInbound(w) };
}

// ── Message-detail fetchers ──────────────────────────────
//
// Both return the RAW wire so every per-message SWR entry holds one
// uniform shape; callers project with the helpers above. See the
// MessageViewWire comment for why.

// Agent-scoped read: GET /v1/agents/{address}/messages/{id}. Carries the
// server-side side effect of flipping an inbound message's inbox_status
// unread → read, which is why the mail surfaces use this rather than the
// review endpoint.
export async function getMessageDetailWire(
  email: string,
  id: string,
): Promise<MessageViewWire> {
  return request<MessageViewWire>(
    "/v1/agents/" +
      encodeURIComponent(email) +
      "/messages/" +
      encodeURIComponent(id),
  );
}

// Review-scoped read: GET /v1/reviews/{id} (account-scoped; id is
// globally unique so no agent address is needed). Superset of the
// agent-scoped read — it additionally populates `hold_reason` and
// `protection` — but it does NOT flip unread → read.
export async function getReviewDetailWire(
  id: string,
): Promise<MessageViewWire> {
  return request<MessageViewWire>("/v1/reviews/" + encodeURIComponent(id));
}

// ── Attachments ──────────────────────────────────────────

// Wire shape of AttachmentView (GET …/messages/{id}/attachments/{index}):
// metadata + a short-lived signed download_url; `data` (base64) is present
// only when `?inline=true` was requested for an attachment within the 256 KB
// inline cap.
type AttachmentViewWire = {
  index: number;
  filename?: string;
  content_type?: string;
  content_id?: string;
  size_bytes: number;
  download_url: string;
  expires_at: string;
  data?: string;
};

// GET one attachment's metadata + a short-lived signed download_url. Pass
// inline=true to also receive base64 `data` for small attachments (≤256 KB);
// the server rejects larger inline requests (413).
export async function getAttachment(
  email: string,
  messageId: string,
  index: number,
  opts?: { inline?: boolean },
): Promise<AttachmentViewWire> {
  const q = opts?.inline ? "?inline=true" : "";
  return request<AttachmentViewWire>(
    "/v1/agents/" +
      encodeURIComponent(email) +
      "/messages/" +
      encodeURIComponent(messageId) +
      "/attachments/" +
      index +
      q,
  );
}

// The largest attachment the backend will base64-inline (`?inline=true`); must
// mirror httpapi.attachmentInlineMaxBytes. At/below it we fetch one JSON
// round-trip that already carries the base64; above it we stream the bytes and
// encode them client-side.
const ATTACHMENT_INLINE_CAP = 256 * 1024;

// Strip an absolute download_url down to a same-origin path so the browser
// fetches it through the dashboard's own /v1 proxy (the signed token, not a
// cookie, authorizes it) — avoiding a cross-origin request to the API host.
function sameOriginPath(absoluteURL: string): string {
  try {
    const u = new URL(absoluteURL);
    return u.pathname + u.search;
  } catch {
    return absoluteURL; // already relative
  }
}

function blobToDataUrl(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result as string);
    reader.onerror = () => reject(reader.error ?? new Error("read failed"));
    reader.readAsDataURL(blob);
  });
}

// Load one inline image's bytes as a `data:` URL usable inside the email
// iframe. Small images come straight back base64-encoded from the inline
// endpoint (one round-trip); larger ones stream through the signed download URL
// and are encoded client-side. A data: URL is used (not blob:) because
// DOMPurify's URI filter permits data: images but not blob:, and the sandboxed
// iframe's CSP already allows `img-src data:`. Base64 in the browser is a
// memory concern only — never the agent-context concern the API's inline cap
// guards, so any size is fine to render here.
// Load one attachment's bytes as a `blob:` object URL for the in-app viewer.
//
// blob:, not data:, for two reasons. Chrome refuses to render a PDF from a
// data: URL in an iframe, so the PDF path needs one; and the data:-only rule
// documented on loadInlineAttachmentUrl is a DOMPurify/CSP constraint for
// images INSIDE the sandboxed email iframe, which the viewer sits outside of.
//
// The blob is typed from the attachment METADATA, not the response's
// Content-Type: the download endpoint serves bytes for saving (it sets
// Content-Disposition: attachment), and an octet-stream response type would
// leave the browser with nothing to render.
//
// Callers MUST call revoke() when done — an object URL pins its bytes in
// memory for the lifetime of the document otherwise.
export async function loadAttachmentObjectUrl(
  email: string,
  messageId: string,
  meta: AttachmentMeta,
): Promise<{ url: string; revoke: () => void }> {
  const a = await getAttachment(email, messageId, meta.index);
  const res = await fetch(sameOriginPath(a.download_url), {
    credentials: "include",
  });
  if (!res.ok) throw new ApiError("attachment fetch failed", res.status);
  const blob = new Blob([await res.arrayBuffer()], {
    type: meta.content_type || "application/octet-stream",
  });
  const url = URL.createObjectURL(blob);
  return { url, revoke: () => URL.revokeObjectURL(url) };
}

export async function loadInlineAttachmentUrl(
  email: string,
  messageId: string,
  meta: AttachmentMeta,
): Promise<{ url: string }> {
  const contentType = meta.content_type || "application/octet-stream";
  if (meta.size_bytes <= ATTACHMENT_INLINE_CAP) {
    const a = await getAttachment(email, messageId, meta.index, { inline: true });
    if (a.data) {
      return { url: `data:${contentType};base64,${a.data}` };
    }
  }
  const a = await getAttachment(email, messageId, meta.index);
  const res = await fetch(sameOriginPath(a.download_url), { credentials: "include" });
  if (!res.ok) throw new ApiError("attachment fetch failed", res.status);
  return { url: await blobToDataUrl(await res.blob()) };
}

// ── Protection config (review-queue holds) ──────────────

// GET /v1/agents/{address}/protection — the agent's protection posture
// (inbound/outbound trust gate + content scan + the review-hold queue).
// Account-scope only; the dashboard's session cookie qualifies. Beta:
// the shape may change before it's declared stable.
export async function getProtection(email: string): Promise<ProtectionConfig> {
  return request<ProtectionConfig>(
    "/v1/agents/" + encodeURIComponent(email) + "/protection",
  );
}

// Replace the agent's full protection posture. The PUT is a wholesale
// replace (inbound/outbound/holds all required), so callers send the
// complete config — the ProtectionEditor edits every section in one form
// and submits the whole thing, which matches this contract exactly.
export async function setProtection(
  email: string,
  config: ProtectionConfig,
): Promise<void> {
  return request(
    "/v1/agents/" + encodeURIComponent(email) + "/protection",
    { method: "PUT", body: JSON.stringify(config) },
  );
}

// ── HITL pending messages ───────────────────────────────

// Wire shape of a row in the review queue (GET /v1/reviews → { items }).
// Mirrors ReviewView in api/openapi.yaml. The review queue is the
// account-scoped operator surface for holds of BOTH directions (outbound
// drafts awaiting send + inbound messages held by a screening gate).
type ReviewWire = {
  id: string;
  agent_email: string;
  direction: "inbound" | "outbound";
  header_from: string | null;
  envelope_from: string | null;
  verified_domain: string | null;
  to?: string[] | null;
  subject: string;
  conversation_id?: string;
  review_status: string;
  hold_reason?: PendingMessageSummary["hold_reason"];
  created_at: string;
};

type ReviewPageWire = {
  items?: ReviewWire[] | null;
  next_cursor?: string | null;
};

function pendingSummary(r: ReviewWire): PendingMessageSummary {
  return {
    id: r.id,
    agent_email: r.agent_email,
    direction: r.direction,
    from: r.header_from ?? "",
    verified_domain: r.verified_domain,
    subject: r.subject,
    conversation_id: r.conversation_id,
    to: r.to ?? [],
    status: r.review_status,
    created_at: r.created_at,
    hold_reason: r.hold_reason,
  };
}

// The pending-review queue: one account-scoped call to GET /v1/reviews
// returns every hold (both directions) across the account's inboxes,
// newest-first. (Replaces the old per-agent fan-out over /messages — the
// dedicated /reviews resource is account-only, so agents can't see holds,
// and held inbound is surfaced here without leaking onto the agent inbox.)
export async function listPendingMessages(): Promise<PendingMessageSummary[]> {
  const page = await request<ReviewPageWire>("/v1/reviews");
  return (page.items ?? []).map(pendingSummary);
}

// Resolve a notification deep link that points beyond the first queue page.
// The detail endpoint does not carry agent_email, so walk the account-scoped
// summaries until the target is found. Repeated cursors are treated as a
// malformed terminal page rather than looping forever.
export async function findPendingMessage(
  id: string,
): Promise<PendingMessageSummary | null> {
  let cursor = "";
  const seen = new Set<string>();
  for (;;) {
    const query = new URLSearchParams({ limit: "100" });
    if (cursor) query.set("cursor", cursor);
    const page = await request<ReviewPageWire>(`/v1/reviews?${query}`);
    const found = (page.items ?? []).find((item) => item.id === id);
    if (found) return pendingSummary(found);
    const next = page.next_cursor ?? "";
    if (!next || seen.has(next)) return null;
    seen.add(next);
    cursor = next;
  }
}

export type ApprovePayload = {
  subject?: string;
  text?: string;
  html?: string;
  to?: string[];
  cc?: string[];
  bcc?: string[];
  attachments?: PendingAttachment[];
};

// approvePendingMessage / rejectPendingMessage hit the account-scoped review
// queue (POST /v1/reviews/{id}/approve|reject). A review's id IS the held
// message's id, so no inbox email is needed; agentEmail is kept in the
// signature only for call-site compatibility. (The deprecated agent-path
// approve/reject endpoints were removed in the pre-GA vocabulary freeze.)
export async function approvePendingMessage(
  agentEmail: string,
  id: string,
  overrides: ApprovePayload = {},
): Promise<{
  status: string;
  message_id: string;
  provider_message_id?: string;
  method?: string;
  edited?: boolean;
}> {
  void agentEmail; // not needed by /reviews (id is globally unique); kept for call-site compat
  return request(
    "/v1/reviews/" + encodeURIComponent(id) + "/approve",
    {
      method: "POST",
      body: JSON.stringify(overrides),
    },
  );
}

export async function rejectPendingMessage(
  agentEmail: string,
  id: string,
  reason: string,
): Promise<{ status: string; message_id: string; rejection_reason?: string }> {
  void agentEmail;
  return request(
    "/v1/reviews/" + encodeURIComponent(id) + "/reject",
    {
      method: "POST",
      body: JSON.stringify({ reason }),
    },
  );
}

// ---------------------------------------------------------------------------
// Webhook observability reads.
//
// The delivery log is per-subscriber: one row is one attempt series against
// one endpoint. Rows are returned verbatim rather than projected — every
// field the UI reads is either optional or an open-set string, so narrowing
// here would silently drop values the server adds later. Classification into
// a rendered state lives in lib/webhooks.ts.
// ---------------------------------------------------------------------------

export async function getWebhook(id: string): Promise<WebhookView> {
  return request<WebhookView>("/v1/webhooks/" + encodeURIComponent(id));
}

export type WebhookDeliveryPage = {
  items: WebhookDeliveryView[];
  next_cursor: string | null;
};

export async function listWebhookDeliveries(
  id: string,
  opts: {
    // Server-side filter; the API accepts pending | delivered | failed.
    status?: string;
    cursor?: string;
    pageSize?: number;
  } = {},
): Promise<WebhookDeliveryPage> {
  const params = new URLSearchParams();
  if (opts.status) params.set("status", opts.status);
  if (opts.cursor) params.set("cursor", opts.cursor);
  if (opts.pageSize) params.set("limit", String(opts.pageSize));
  const qs = params.toString();
  const page = await request<{
    items?: WebhookDeliveryView[];
    next_cursor?: string | null;
  }>(
    "/v1/webhooks/" + encodeURIComponent(id) + "/deliveries" + (qs ? "?" + qs : ""),
  );
  return {
    items: page.items ?? [],
    next_cursor: page.next_cursor ?? null,
  };
}
