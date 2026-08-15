// Onboarding domain model types.
// These map to the backend API shapes but are owned by the UI layer.

/** Web-UI address path — shared e2a slug vs custom domain (under SetupMethod=web). */
export type AddressType = "shared" | "custom";

/** Top-level setup method — hand off to an agent over MCP, or use the web UI. */
export type SetupMethod = "agent" | "web";

/** Domain verification status for the checklist model. */
export type DomainStatus = "unverified" | "verified";

/** Custom-domain checklist steps — derivable from backend state only.
 *  domain_added:    domain registered but not yet verified
 *  domain_verified: domain verified, no agents on it yet
 *  agent_created:   at least one agent exists on this domain
 */
export type ChecklistStep =
  | "domain_added"
  | "domain_verified"
  | "agent_created";

/** Async SES sending-identity state (decision 4 / Slice 4), independent of
 *  inbound `verified`. Drives the "send as your own address" onboarding:
 *  none → not provisioned (feature off, or pre-verify); pending → registered,
 *  awaiting SES; verified → own-address From is live (no "via e2a"); failed →
 *  verification failed or timed out (see `sending_error`). Open set — tolerate
 *  unknown values. */
export type DomainSendingStatus = "none" | "pending" | "verified" | "failed";

/** One axis of `DomainInfo.capabilities`. Documented OPEN set — tolerate
 *  unknown values (render generically, never crash). Same vocabulary on both
 *  axes; today `inbound` only ever emits verified/pending. */
export type DomainCapabilityStatus = "none" | "pending" | "verified" | "failed";

/** Per-axis rollup of what a domain can actually do, mirroring the backend
 *  DomainCapabilities. `inbound` = can it RECEIVE mail (ownership TXT + inbound
 *  MX); `outbound` = can agents on it SEND as their own address (the async SES
 *  sending identity). The axes are provisioned on different schedules and are
 *  independent in both directions — never treat one as implying the other.
 *
 *  Restates the legacy `verified` (inbound) and `sending_status` (outbound)
 *  fields. Prefer this via inboundCapability()/outboundCapability() in
 *  ./state, which fall back to the legacy fields when a server predating
 *  `capabilities` omits it. */
export type DomainCapabilities = {
  inbound: DomainCapabilityStatus;
  outbound: DomainCapabilityStatus;
};

/** What a DNS record is for. Documented OPEN set — tolerate unknown values
 *  (a future record kind should render generically, not crash the card).
 *  ownership/inbound_mx are inbound; dkim/mail_from_* are sending. */
export type DNSRecordPurpose =
  | "ownership"
  | "inbound_mx"
  | "inbound_mx_wildcard"
  | "dkim"
  | "mail_from_mx"
  | "mail_from_spf";

/** Per-record verification state. Documented OPEN set — tolerate unknown. */
export type DNSRecordStatus = "verified" | "pending" | "missing" | "failed";

/** One row in the unified `dns_records` array (mirrors the backend DNSRecord).
 *  Collapses the old `dns_records` object + `sending_dns_records` array into a
 *  single purpose-tagged list. MX records carry their priority in `priority`
 *  (TXT records leave it null); the value is the bare mail-server host. */
export type DNSRecord = {
  type: string; // "MX" | "TXT"
  name: string;
  value: string;
  priority?: number | null;
  purpose: DNSRecordPurpose;
  status: DNSRecordStatus;
};

/** Domain as returned by GET /v1/domains. */
export type DomainInfo = {
  domain: string;
  verified: boolean;
  // Per-axis rollup of inbound (receive) vs outbound (send-as-own-address).
  // Optional: a server predating the field omits it, so read it through
  // inboundCapability()/outboundCapability() rather than directly.
  capabilities?: DomainCapabilities;
  verification_token: string;
  // Unified, purpose-tagged record set. ALL applicable records (inbound +
  // sending) are returned at register time — they are deterministic — so the
  // onboarding paste is one shot. mail_from_* rows are present only when the
  // sending feature is enabled server-side (ses_region set).
  dns_records: DNSRecord[];
  created_at: string;
  verified_at: string | null;
  // Enrichment fields. last_checked_at moves on every verification probe
  // (success or failure); agent_count is computed at read time.
  last_checked_at?: string | null;
  agent_count?: number;
  // Sender identity (decision 4 / Slice 4), independent of `verified`. The
  // rollup over the dkim + mail_from_* records' status; drives the section
  // header chip. none/absent when the feature is off (ses_region unset).
  sending_status?: DomainSendingStatus;
  sending_error?: string;
  sending_last_checked_at?: string | null;
};

export type DeleteDomainResult = {
  deleted: boolean;
  domain: string;
  /** Open set: only "confirmed" proves provider absence and permits DNS removal. */
  sending_teardown?: string;
};

/** Response from POST /v1/domains/{domain}/verify — per-record
 * diagnostic. `dkim` reports "found" or "missing" against the
 * per-domain public key registered at claim time. "deferred" is
 * returned only for pre-migration domains that have no stored
 * keypair. */
export type VerifyDomainResponse = {
  domain: string;
  verified: boolean;
  verified_at?: string | null;
  mx?: "found" | "missing";
  spf?: "found" | "missing";
  dkim?: "found" | "missing" | "deferred";
};

/** The progress state for a domain through onboarding. */
export type DomainProgress = {
  domain: DomainInfo;
  step: ChecklistStep;
  agentCount: number;
};

/** Shared-domain flow steps (linear, fast). */
export type SharedFlowStep = "address" | "details" | "connect";

/** Custom-domain flow steps (checklist, resumable). */
export type CustomFlowStep =
  | "choose_domain"
  | "dns"
  | "verify"
  | "create_agent"
  | "connect";

/** Top-level onboarding step before branching. */
export type OnboardingPath = "choose" | "shared" | "custom";

/** Parameters for creating an agent via the shared-domain flow. */
export type SharedAgentParams = {
  slug: string;
};

/** Parameters for creating an agent via the custom-domain flow. */
export type CustomAgentParams = {
  domain: string;
  localPart: string;
};

/** Normalized create-agent request for the API layer. */
export type CreateAgentRequest =
  | { type: "shared"; slug: string }
  | { type: "custom"; email: string };

/** Response from POST /v1/agents. */
export type AgentCreateResponse = {
  id: string;
  domain: string;
  email: string;
};

// ── Protection config (GET/PUT /v1/agents/{email}/protection) ──
// Mirrors ProtectionConfigView. Beta. The dashboard only edits the
// `holds` section; inbound/outbound are read + passed back unchanged on
// the wholesale PUT.

export type ProtectionGate = {
  policy?: "open" | "allowlist" | "domain";
  allowlist?: string[];
  action?: "flag" | "review" | "block";
};

export type ProtectionScan = {
  sensitivity?: "off" | "low" | "medium" | "high";
};

export type ProtectionDirection = {
  gate: ProtectionGate;
  scan: ProtectionScan;
};

export type ProtectionHolds = {
  ttl_seconds?: number;
  on_expiry?: "approve" | "reject";
};

export type ProtectionConfig = {
  inbound: ProtectionDirection;
  outbound: ProtectionDirection;
  holds: ProtectionHolds;
};
