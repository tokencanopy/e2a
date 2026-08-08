export type AgentData = {
  domain: string;
  email: string;
};

export type UserInfo = {
  id: string;
  email: string;
  name: string;
  created_at: string;
};

export type DashboardAgent = {
  domain: string;
  email: string;
  name: string;
  domain_verified: boolean;
  created_at: string;
  // Set when the inbox is in the trash (soft-deleted). Only present on rows
  // from the trash listing (GET /v1/agents?deleted=true); the janitor purges
  // trashed inboxes ~30 days after this instant.
  deleted_at?: string;
};

// Aggregated client-side from `GET /v1/agents/{address}/messages?
// direction=outbound` rows whose review_status === "pending_review".
// `/v1` has no cross-account pending endpoint, so the pending page
// fans out over the account's agents and tags each row with the
// owning agent's address (`agent_email`) — needed to drive the
// agent-scoped approve/reject/detail endpoints.
export type HoldReason = {
  type: "gate" | "scan" | "send" | "unknown" | string;
  code: string;
  summary: string;
  category?: string;
  detail?: string;
  confidence?: number | null;
};

export type PendingMessageSummary = {
  id: string;
  // Server-owned email topology identity. Optional because review payloads
  // and older servers omit it.
  thread_id?: string;
  // Owning agent's email address. In `/v1` this is how detail/approve/
  // reject are addressed (the path's {address}). Displayed in the queue
  // row's "from" line.
  agent_email: string;
  // A hold can be an outbound draft (send/reply awaiting approval) or an
  // inbound message held by a screening gate. Drives the row's direction
  // annotation + which addresses are shown.
  direction: "inbound" | "outbound";
  // Sender display projection — derived from header_from by the API client.
  from?: string;
  verified_domain?: string | null;
  subject: string;
  type?: string;
  conversation_id?: string;
  to: string[];
  cc?: string[];
  bcc?: string[];
  status: string;
  created_at: string;
  // Product-facing explanation from the review API. Render summary directly;
  // code is an open machine-readable value for automation.
  hold_reason?: HoldReason;
  // Future send_at an outbound hold carried (#815). The schedule survives the
  // hold, so the queue shows when this message will send once approved. Present
  // only on outbound holds with a schedule; absent otherwise.
  scheduled_at?: string;
};

export type PendingAttachment = {
  filename: string;
  content_type: string;
  data: string; // base64
};

// Metadata for one attachment of a received/sent message — never the bytes.
// `index` is the stable 0-based fetch key for the attachment-bytes endpoint;
// `content_id` (when present) is the Content-ID an HTML body's `cid:` URL
// resolves against, marking the part as an inline image embedded in the body.
export type AttachmentMeta = {
  index: number;
  filename?: string;
  content_type?: string;
  size_bytes: number;
  content_id?: string;
};

// One screening producer's verdict behind a hold (review detail only, beta).
// A gate finding names the trust policy that tripped; a scan finding carries the
// content detector's threat categories + a short natural-language rationale.
export type ProtectionFinding = {
  source: string; // gate | scan
  action?: string; // flag | review | block
  detector?: string;
  score?: number | null;
  categories?: { name: string; score?: number }[];
  summary?: string; // the detector's short rationale
};

export type PendingMessageDetail = PendingMessageSummary & {
  email_message_id?: string;
  body_text?: string;
  body_html?: string;
  // Attachment METADATA as returned by the detail endpoints (not the
  // base64-carrying PendingAttachment used when SENDING).
  attachments?: AttachmentMeta[];
  edited?: boolean;
  reviewed_at?: string;
  // Set on approved/rejected rows. Null on worker-triggered transitions
  // (TTL auto-approve / auto-reject) — UI renders "expired" instead of
  // a reviewer name in that case. The two fields move together.
  reviewed_by_user_id?: string | null;
  reviewed_by_name?: string | null;
  rejection_reason?: string;
  provider_message_id?: string;
  method?: string;
  // Screening breakdown behind the hold — detector categories + rationale that
  // support hold_reason. Review detail only; absent on gate-only holds with
  // no scan detail. Beta.
  protection?: ProtectionFinding[];
  // Attached when this is a reply — the inbound message being replied
  // to. Drives the review panel's "In reply to" provenance pane
  // (structured SPF/DKIM/DMARC evidence). Null on send/test messages.
  inbound?: PendingMessageInboundContext | null;
};

export type PendingMessageInboundContext = {
  sender: string;
  subject: string;
  created_at: string;
  authentication?: EmailAuthentication | null;
};

export type EmailAuthentication = {
  spf: {
    status: string;
    domain: string | null;
    aligned: boolean | null;
    detail?: string;
  };
  dkim: {
    status: string;
    domain: string | null;
    selector: string | null;
    aligned: boolean | null;
    detail?: string;
  }[];
  dmarc: {
    status: "pass" | "fail" | "none" | "temperror" | "permerror";
    domain: string | null;
    policy: "none" | "quarantine" | "reject" | null;
    aligned_by: ("spf" | "dkim")[];
    detail?: string;
  };
};

// MessageSummaryView from `GET /v1/agents/{address}/messages`
// (PageMessageSummaryView.items). v1 splits state into `delivery_status`
// (the delivery rollup) and `review_status` (the review/HITL lifecycle:
// pending_review | sent | review_rejected | review_expired_approved |
// review_expired_rejected). The projection in api.ts maps those onto the
// app's `status` (delivery) + `review_status` (review) fields below. The
// dashboard inbox uses this projection directly.
export type MessageSummary = {
  id: string;
  // Server-owned email topology identity (beta). Older servers and legacy
  // rows omit it; those rows retain conversation/orphan grouping.
  thread_id?: string;
  direction: "inbound" | "outbound";
  from: string;
  verified_domain?: string | null;
  to: string[];
  cc?: string[];
  reply_to?: string[];
  recipient: string;
  subject: string;
  conversation_id?: string;
  // Delivery rollup (from v1 delivery_status): accepted | sending | sent |
  // delivered | deferred | bounced | complained | failed. The UI labels
  // accepted as "Queued". Empty on a held draft.
  status: string;
  // Review lifecycle (from v1 review_status): pending_review | sent |
  // review_rejected | review_expired_approved | review_expired_rejected.
  review_status?: string;
  // Future instant a scheduled outbound send will be submitted (from v1
  // scheduled_at). Present only while a future send_at is set; absent on
  // immediate sends and inbound rows. Drives the "Scheduled" chip + send time.
  scheduled_at?: string;
  // Inbound read state (from v1 read_status): "unread" | "read". Empty on
  // outbound rows. Drives the inbox's unread/bold affordance.
  read_status?: string;
  // Outbound webhook delivery state.
  webhook_status?: string;
  webhook_error?: string;
  // Byte length of the raw RFC-5322 message. 0 if not stored (older
  // outbound rows pre-dating the size projection).
  size_bytes?: number;
  created_at: string;
  // Set when the message is in the trash (soft-deleted). Only present on
  // rows from the trash view (?deleted=true); purged ~30 days after this.
  deleted_at?: string;
};

// PageMessageSummaryView — the cursor-paginated envelope returned by
// `GET /v1/agents/{address}/messages`.
export type ListMessagesResponse = {
  items: MessageSummary[];
  next_cursor?: string | null;
};

// MessageView from `GET /v1/agents/{address}/messages/{id}`. Used by the
// inline inbox thread. The `/v1` detail endpoint returns the
// same MessageView shape for inbound and outbound; inbound rows carry
// canonical authentication evidence + `raw_message`, and the parsed text/plain body comes
// through `body.text`.
export type InboundMessageDetail = {
  id: string;
  // Server-owned email topology identity (beta). Optional for legacy rows
  // and older servers.
  thread_id?: string;
  direction: "inbound";
  header_from: string | null;
  envelope_from: string | null;
  verified_domain: string | null;
  authentication: EmailAuthentication | null;
  to: string[];
  cc: string[];
  reply_to: string[];
  recipient: string;
  subject: string;
  conversation_id: string;
  review_status?: string;
  // UI projection aliases for delivered_to and read_status.
  status: string;
  created_at: string;
  // Backend-derived body (preferred): `text` is the injection-reduced plain
  // body (text/plain, else HTML→text, QP/base64 decoded, quoted chains
  // stripped); `html` is the decoded text/html part for rich display, present
  // only when the message has an HTML part. Render these rather than the raw
  // bytes.
  parsed?: { text?: string; html?: string };
  // Held-draft body shape (outbound). Inbound rows carry `parsed` instead.
  body?: { text?: string; html?: string };
  // Per-attachment metadata (never bytes). Inline images (those with a
  // content_id referenced by a `cid:` in `parsed.html`) render in the body;
  // the rest surface as download chips. Bytes are fetched on demand.
  attachments?: AttachmentMeta[];
  // Raw RFC-5322 bytes, base64-encoded by the JSON layer. Decoded only as a
  // last-resort fallback when neither `parsed.html` nor `parsed.text` is present.
  raw_message: string;
};

export type APIKeyData = {
  id: string;
  key?: string;        // one-time plaintext, only present on creation response
  key_prefix?: string; // non-secret prefix, shown in list view
  name: string;
  // Credential scope: "account" (workspace admin) or "agent" (bound to a
  // single inbox). `agent_email` is the bound inbox email, present only for
  // agent scope.
  scope?: string;
  agent_email?: string;
  created_at: string;
  // Updated on every successful authenticated request. Null until the
  // key is first used. Surfaces in the "Last used" column.
  last_used_at?: string | null;
  // Optional hard expiry — keys with null expires_at never expire.
  // AuthenticateRequest rejects expired keys at the auth gate.
  expires_at?: string | null;
};

// Domain enrichment fields — chips on the Domains page. last_checked_at
// moves on every verification probe (success or failure).
//
// NOTE: the live, consumed DomainInfo lives in
// ./onboarding/types.ts. This standalone copy is kept in sync (unified
// purpose-tagged dns_records array) so it doesn't read as a stale contract.
export type DomainInfo = {
  domain: string;
  verified: boolean;
  // Per-axis capability rollup (inbound = receive, outbound = send as own
  // address). Optional — a server predating the field omits it.
  capabilities?: { inbound: string; outbound: string };
  verification_token: string;
  dns_records: Array<{
    type: string;
    name: string;
    value: string;
    priority?: number | null;
    purpose: string;
    status: string;
  }>;
  created_at: string;
  verified_at?: string | null;
  last_checked_at?: string | null;
  agent_count: number;
};

// Request body for POST /v1/account/api-keys. expires_at is an RFC 3339
// timestamp; omit or null to issue a never-expiring key.
export type CreateAPIKeyRequest = {
  name: string;
  expires_at?: string;
};

// Request body for PATCH /api/auth/me. Only `name` is updatable today;
// other identity fields come from the OAuth provider.
export type UpdateMeRequest = {
  name: string;
};
