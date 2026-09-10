package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/dkim"
	"github.com/tokencanopy/e2a/internal/domainteardown"
	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/filterquery"
	"github.com/tokencanopy/e2a/internal/inboundpolicy"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// normalizeDomain lowercases and IDNA-normalizes a domain string.
// Internationalized domains are converted to their ASCII (punycode) form.
func normalizeDomain(domain string) string {
	domain = strings.ToLower(domain)
	if ascii, err := idna.Lookup.ToASCII(domain); err == nil {
		return ascii
	}
	return domain
}

// Domain represents a verified or unverified domain registered by a user.
type Domain struct {
	Domain            string     `json:"domain"`
	UserID            *string    `json:"user_id,omitempty"`
	Verified          bool       `json:"verified"`
	VerificationToken string     `json:"verification_token"`
	CreatedAt         time.Time  `json:"created_at"`
	VerifiedAt        *time.Time `json:"verified_at,omitempty"`
	// IsPrimary marks the user's default domain. At most one TRUE per
	// user (enforced by a partial unique index in migration 013).
	IsPrimary bool `json:"primary"`
	// LastCheckedAt is updated whenever the verification probe runs,
	// successful or not. NULL until the first probe — distinct from
	// "probed and failed" which is captured by `verified=false` + a
	// non-null LastCheckedAt.
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	// AgentCount is computed at read time — ListDomainsByUser,
	// LookupDomain, and ClaimOrCreateDomain's existing-row branch run
	// the same correlated subquery — and is not a persisted column.
	AgentCount int `json:"agent_count"`
	// DKIM keypair fields. The selector + public key
	// are user-facing — the dashboard shows them so users can copy the
	// DNS TXT record. The private key is intentionally NOT in the JSON
	// shape; it's only read by the outbound signer via
	// GetDKIMKey(domain). Domains created before migration 014 ran
	// keep all three NULL until the next ClaimOrCreate or backfill.
	DKIMSelector  string `json:"dkim_selector,omitempty"`
	DKIMPublicKey string `json:"dkim_public_key,omitempty"`
	// Sender identity (decision 4 / Slice 4). Independent of `Verified`
	// (inbound ownership): SendingStatus tracks the async SES sending
	// identity that lets outbound use the agent's own address as From.
	// SendingStatus ∈ {none,pending,verified,failed}; own-address From is
	// used ONLY when "verified" (fail-closed). SendingDNSRecordsJSON is the
	// raw JSONB (nil when unset) — the API layer unmarshals it for display.
	SendingStatus         string     `json:"sending_status"`
	SendingError          string     `json:"sending_error,omitempty"`
	SendingDNSRecordsJSON []byte     `json:"-"`
	SendingLastCheckedAt  *time.Time `json:"sending_last_checked_at,omitempty"`
	// Per-axis SES sending status (migration 049). SES verifies DKIM and the
	// custom MAIL FROM independently; these persist that breakdown so the API
	// can show each sending DNS record its OWN status instead of the
	// all-or-nothing SendingStatus rollup. Empty string ("") when no per-axis
	// signal has been recorded (pre-migration / pre-provision / terminal
	// failure) — the read path falls back to SendingStatus in that case. ∈
	// {"",none,pending,verified,failed}.
	//
	// json:"-" (like SendingDNSRecordsJSON): these are internal read-model
	// fields consumed by httpapi.domainView via Go field access to derive each
	// DNSRecord.status. They are deliberately NOT serialized, so they stay out
	// of the API/export shape — the fix only makes DNSRecord.status VALUES more
	// accurate, it does not add API fields.
	SendingDkimStatus     string `json:"-"`
	SendingMailFromStatus string `json:"-"`
}

type AgentIdentity struct {
	// ID is the agent's full email address and its identifier (id == email;
	// EmailAddress() returns it). It is never serialized: every API surface
	// keys an agent on `email`, and the #436 rename dropped the redundant
	// `id` from the public contract. Kept as a field for internal use only.
	ID               string    `json:"-"`
	Domain           string    `json:"domain"`
	RegisteredDomain string    `json:"registered_domain"`
	Email            string    `json:"email"`
	Name             string    `json:"name"`
	DomainVerified   bool      `json:"domain_verified"`
	Public           bool      `json:"public"`
	CreatedAt        time.Time `json:"created_at"`
	UserID           string    `json:"user_id"`
	// HITL review-queue mechanism. The producer policies hitl_enabled/hitl_mode
	// were retired (Slice 5b/5c, columns dropped in migration 043) — outbound_policy
	// + outbound_scan own holds now. These two knobs govern how the review queue
	// behaves (TTL + expiry action) for both directions.
	HITLTTLSeconds        int    `json:"ttl_seconds"`
	HITLExpirationAction  string `json:"on_expiry"`
	SuppressNotifications bool   `json:"suppress_notifications"`
	// Dashboard enrichment fields. Computed at read
	// time by ListAgentsByUser via correlated subqueries — other load
	// paths (GetAgentByID / GetAgentByEmail) leave them at zero values,
	// same pattern as Domain.AgentCount. Switch to denormalized columns
	// if the read cost ever bites.
	Inbound7d      int        `json:"inbound_7d"`
	Outbound7d     int        `json:"outbound_7d"`
	PendingCount   int        `json:"pending_count"`
	LastDeliveryAt *time.Time `json:"last_delivery_at,omitempty"`
	// WebhookStatus summarizes the webhook posture serving this agent,
	// derived from the /v1/webhooks subscriber resource: which of the
	// account's webhooks match this agent (an empty agent filter matches
	// every agent) and how their recent deliveries have fared. One of the
	// WebhookStatus* constants below; open set. Computed only on enriched
	// read paths (ListAgentsByUser, the account export) — other load paths
	// leave it empty, and omitempty keeps the un-computed zero value off
	// the wire. Replaces the pre-GA webhook_healthy bool, which could not
	// distinguish "no webhook configured" from "healthy".
	WebhookStatus string `json:"webhook_status,omitempty" doc:"Webhook posture for this agent, derived from the account's webhook subscribers that match it (a webhook with no agent filter matches every agent). Open set; tolerate unknown values. Known values: none (no webhook matches this agent), healthy (an enabled webhook matches and none serving this agent has a terminally-failed delivery in the last 24h), failing (an enabled webhook matches but at least one delivery on a matching enabled webhook terminally failed in the last 24h), disabled (webhooks match but every one is disabled, turned off manually), auto_disabled (webhooks match, every one is disabled, and at least one was auto-disabled by the chronic-failure sweep). Present on enriched surfaces (account export, dashboard agent list); absent where not computed."`
	// DeletedAt is non-nil while the agent is in the trash (soft-deleted,
	// migration 063): hidden from every live lookup, restorable until the
	// janitor purges it after TrashRetention. Populated only by the
	// any-state / trash load paths; live lookups filter it out entirely.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// InboundPolicy is the per-agent inbound ingestion gate (migration 033 /
	// Slice 7): one of inboundpolicy.{Open,Allowlist,Domain}.
	// Defaults to 'open' (the column default). InboundAllowlist holds the
	// exact addresses (allowlist policy) or domains (domain policy) the gate
	// trusts; empty for open.
	InboundPolicy    string   `json:"inbound_policy"`
	InboundAllowlist []string `json:"inbound_allowlist,omitempty"`
	// Screening config (migration 038 / Slice 3). The producer-policy actions
	// decide what a gate/scan violation does (flag|review|block); outbound_policy +
	// outbound_allowlist are the egress recipient gate (open|allowlist|domain);
	// inbound_scan/outbound_scan toggle the content scan with a review/block
	// threshold ladder. See docs/design/2026-06-20-agent-screening-hitl.md §4.1.
	InboundPolicyAction         string   `json:"inbound_policy_action"`
	OutboundPolicy              string   `json:"outbound_policy"`
	OutboundAllowlist           []string `json:"outbound_allowlist,omitempty"`
	OutboundPolicyAction        string   `json:"outbound_policy_action"`
	InboundScan                 string   `json:"inbound_scan"`
	InboundScanReviewThreshold  float64  `json:"inbound_scan_review_threshold"`
	InboundScanBlockThreshold   float64  `json:"inbound_scan_block_threshold"`
	OutboundScan                string   `json:"outbound_scan"`
	OutboundScanReviewThreshold float64  `json:"outbound_scan_review_threshold"`
	OutboundScanBlockThreshold  float64  `json:"outbound_scan_block_threshold"`
	// Scan sensitivity (migration 045) is the protection API's content-scan knob
	// (off|low|medium|high). It is the read-back source of truth; the float
	// thresholds above are derived from it on write and are what the piguard
	// engine consumes. See docs/design/2026-06-22-agent-protection-config.md.
	InboundScanSensitivity  string `json:"inbound_scan_sensitivity"`
	OutboundScanSensitivity string `json:"outbound_scan_sensitivity"`
	// AssertionVersion is the auth.md kill-switch counter (migration 035 /
	// Slice 5b-2): stamped into minted identity_assertion/access_token JWTs and
	// re-checked at the token endpoint; a bump invalidates prior tokens.
	AssertionVersion int `json:"-"`
}

// WebhookStatus* are the known AgentIdentity.WebhookStatus values. The set is
// open — API consumers must tolerate unknown values — but the server only
// emits these five today. Precedence when several webhooks match one agent:
// any enabled webhook wins (healthy/failing per recent deliveries); among
// all-disabled matches, an auto-disabled one wins over a manual disable.
const (
	// WebhookStatusNone: no webhook subscriber matches this agent.
	WebhookStatusNone = "none"
	// WebhookStatusHealthy: at least one enabled webhook matches, and no
	// matching enabled webhook has a terminally-failed delivery in the
	// last 24h.
	WebhookStatusHealthy = "healthy"
	// WebhookStatusFailing: at least one enabled webhook matches, but a
	// matching enabled webhook had a terminally-failed delivery (retries
	// exhausted) in the last 24h.
	WebhookStatusFailing = "failing"
	// WebhookStatusDisabled: webhooks match this agent but every one is
	// disabled, all by hand (no auto_disabled_at).
	WebhookStatusDisabled = "disabled"
	// WebhookStatusAutoDisabled: webhooks match this agent, every one is
	// disabled, and at least one was tripped by the chronic-failure sweep
	// (AutoDisableFailingWebhooks).
	WebhookStatusAutoDisabled = "auto_disabled"
)

// webhookMatchesAgentSQL is the SQL twin of webhookpub's matches() agent rule:
// a webhook scopes to an agent when its filters.agent_ids is absent/empty (no
// constraint) or contains the agent id. Expects `w` (webhooks) and `a`
// (agent_identities) aliases in scope. Conversation/label filters are event
// dimensions, not agent dimensions, so they don't participate here.
const webhookMatchesAgentSQL = `(COALESCE(jsonb_array_length(w.filters->'agent_ids'), 0) = 0
	             OR w.filters->'agent_ids' ? a.id)`

// webhookStatusSQL derives AgentIdentity.WebhookStatus for the agent row
// aliased `a`. Health reads webhook_subscriber_deliveries (the live
// /v1/webhooks pipeline), NOT the legacy webhook_deliveries table, which is
// retained only for janitor draining and no longer receives rows. Failure
// attribution is per matching webhook (endpoint), not per event: if an
// endpoint serving this agent is failing, the agent's events are at risk
// regardless of which agent's event tripped the failure.
const webhookStatusSQL = `CASE
	 WHEN EXISTS (
	     SELECT 1 FROM webhooks w
	     WHERE w.user_id = a.user_id AND w.enabled
	       AND ` + webhookMatchesAgentSQL + `
	 ) THEN CASE WHEN EXISTS (
	     SELECT 1 FROM webhook_subscriber_deliveries wsd
	     JOIN webhooks w ON w.id = wsd.webhook_id
	     WHERE w.user_id = a.user_id AND w.enabled
	       AND ` + webhookMatchesAgentSQL + `
	       AND wsd.status = 'failed'
	       AND wsd.last_attempt_at > now() - interval '24 hours'
	 ) THEN '` + WebhookStatusFailing + `' ELSE '` + WebhookStatusHealthy + `' END
	 WHEN EXISTS (
	     SELECT 1 FROM webhooks w
	     WHERE w.user_id = a.user_id AND w.auto_disabled_at IS NOT NULL
	       AND ` + webhookMatchesAgentSQL + `
	 ) THEN '` + WebhookStatusAutoDisabled + `'
	 WHEN EXISTS (
	     SELECT 1 FROM webhooks w
	     WHERE w.user_id = a.user_id
	       AND ` + webhookMatchesAgentSQL + `
	 ) THEN '` + WebhookStatusDisabled + `'
	 ELSE '` + WebhookStatusNone + `'
	END`

// HITL constants mirror the CHECK constraints in migration 003_hitl.sql.
const (
	HITLMaxTTLSeconds        = 604800 // 7 days
	HITLDefaultTTLSeconds    = 604800
	HITLExpirationApprove    = "approve"
	HITLExpirationReject     = "reject"
	HITLDefaultExpirationAct = HITLExpirationReject
)

// ValidateHITLConfig returns an error if the TTL or expiration action is invalid.
// The DB CHECK constraints are the final guard; this mirrors them for a
// clean, pre-query error path.
func ValidateHITLConfig(ttlSeconds int, expirationAction string) error {
	if ttlSeconds <= 0 || ttlSeconds > HITLMaxTTLSeconds {
		return fmt.Errorf("hitl_ttl_seconds must be between 1 and %d", HITLMaxTTLSeconds)
	}
	if expirationAction != HITLExpirationApprove && expirationAction != HITLExpirationReject {
		return fmt.Errorf("hitl_expiration_action must be 'approve' or 'reject'")
	}
	return nil
}

// populateEmail sets the Email field from the agent ID (which is the full email).
func (a *AgentIdentity) populateEmail() {
	a.Email = a.ID
	a.Domain = a.ActualDomain()
	if a.RegisteredDomain == "" {
		a.RegisteredDomain = a.Domain
	}
}

// IsSharedDomain returns true if the agent's domain matches the configured
// shared domain (the host that backs slug-based registration). When
// sharedDomain is empty, the deployment has slug registration disabled
// and no agent can be on the shared domain.
func (a *AgentIdentity) IsSharedDomain(sharedDomain string) bool {
	return sharedDomain != "" && a.ActualDomain() == sharedDomain
}

// ActualDomain returns the exact domain present in the agent's email address.
// It may differ from RegisteredDomain for an inherited subdomain agent.
func (a *AgentIdentity) ActualDomain() string {
	if i := strings.LastIndexByte(a.ID, '@'); i >= 0 && i+1 < len(a.ID) {
		return a.ID[i+1:]
	}
	return a.Domain
}

// RegisteredDomainName returns the explicitly registered domain identity that
// authorizes this agent. It is the DNS/DKIM/sending-state and lifecycle parent.
// The fallback preserves callers that construct exact-domain identities in
// memory without populating RegisteredDomain.
func (a *AgentIdentity) RegisteredDomainName() string {
	if a.RegisteredDomain != "" {
		return a.RegisteredDomain
	}
	return a.ActualDomain()
}

// EmailAddress returns the agent's email address (always the ID).
func (a *AgentIdentity) EmailAddress() string {
	return a.ID
}

type User struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	GoogleSubject string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	// AccountClass (usage.AccountClass) is loaded at auth for metering and
	// rate-limit decisions. Hidden from API JSON. Empty for principals not
	// resolved via API key (e.g. session auth), which fail-closes to standard
	// (metered + limited) in the consuming policies.
	AccountClass string `json:"-"`
	// AcquisitionAnsweredAt is when the onboarding survey was answered or
	// skipped; nil = not yet asked. Loaded by the session/ID loaders that
	// feed /api/auth/me, hidden from API JSON (the auth handler derives a
	// boolean from it).
	AcquisitionAnsweredAt *time.Time `json:"-"`
}

type Message struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_email"`
	Direction string `json:"direction"`
	Sender    string `json:"-"`
	// HeaderFrom and EnvelopeFrom are the canonical inbound identities. They
	// remain separate from ReplyTo and from the legacy Sender projection.
	HeaderFrom        string                    `json:"header_from" doc:"Parsed RFC 5322 From address for inbound mail; null in the export when unavailable and never replaced by Reply-To."`
	EnvelopeFrom      string                    `json:"envelope_from" doc:"SMTP MAIL FROM address for inbound SMTP delivery; null in the export for outbound messages, a null reverse path, or providerless delivery."`
	VerifiedDomain    *string                   `json:"verified_domain" nullable:"true" doc:"DMARC-authenticated RFC 5322 From domain when authentication passed; null when authentication failed, was unavailable, or was not evaluated. A null caused by dmarc.status=none (sender publishes no DMARC record) is common and NOT itself suspicious — distinct from dmarc.status=fail, an actual mismatch. Only DMARC ties a passing SPF or DKIM identity back to this header domain; a bare SPF or DKIM pass without DMARC does not."`
	Authentication    *emailauth.Authentication `json:"authentication" doc:"Inbound SMTP authentication evidence. Only dmarc.status=pass authenticates the RFC 5322 From domain; even a pass does not authenticate the mailbox local part, a person, or message content. Null means there was no authenticating inbound SMTP peer, as with outbound or providerless loopback delivery."`
	Recipient         string                    `json:"delivered_to"`
	Subject           string                    `json:"subject"`
	EmailMessageID    string                    `json:"email_message_id,omitempty"`
	ProviderMessageID string                    `json:"provider_message_id,omitempty"`
	Method            string                    `json:"method,omitempty"`
	Type              string                    `json:"type,omitempty"`
	RawMessage        []byte                    `json:"raw_message,omitempty"`
	AuthHeaders       map[string]string         `json:"-"`
	// Auth carries the parsed inbound authentication verdict
	// (messages.auth_verdict from migration 032): SPF/DKIM/DMARC each with
	// a status and detail. Populated on inbound read paths when the column
	// is non-null; nil on outbound rows (which never have a verdict).
	Auth           *emailauth.AuthVerdict `json:"-"`
	ConversationID string                 `json:"conversation_id,omitempty"`
	// ThreadID is e2a's mailbox-local materialized email reply topology.
	// ThreadParentID is the optional directly resolved reply parent and
	// RFCMessageIDKey is the canonical exact wire anchor. These are internal
	// storage fields: public projections opt in explicitly, and account export
	// must not acquire them through Message's JSON representation.
	ThreadID        string `json:"-"`
	ThreadParentID  string `json:"-"`
	RFCMessageIDKey string `json:"-"`
	// DeliveryStatus is overloaded by direction. On inbound rows it carries
	// the inbox read/unread status (messages.inbox_status) under this legacy
	// JSON key. On outbound rows it carries the outbound delivery rollup
	// (messages.delivery_status from migration 031: 'sent', 'delivered',
	// 'bounced', …) — the worst recipient status by precedence. A message is
	// either inbound or outbound, so the two sources never collide per-row.
	DeliveryStatus string `json:"delivery_status,omitempty"`
	// DeliveryDetail is the human-readable diagnostic for the outbound
	// delivery rollup (e.g. an SES bounce sub-type / SMTP response).
	// Outbound-only; empty on inbound rows. Source: messages.delivery_detail.
	DeliveryDetail string `json:"delivery_detail,omitempty"`
	// SentAs is the From identity actually used when the outbound message was
	// accepted by the relay. Outbound-only; empty on inbound rows. Source:
	// messages.sent_as.
	SentAs string `json:"sent_as,omitempty"`
	// ScheduledAt is the future instant a scheduled outbound send is queued to be
	// submitted (migration 084). Nil for immediate sends and every inbound row.
	// The row stays delivery_status='accepted' while scheduled; this timestamp is
	// the introspection marker (the actual deferral lives on the River job).
	//
	// The doc string mirrors MessageView/MessageSummaryView.scheduled_at so the
	// exported record and the live read views describe one field one way;
	// stability.go marks it beta on all three, because scheduled sending is a
	// beta capability wherever it surfaces. It arrived here undocumented and
	// unmarked, which read as a stable export field it was never meant to be.
	ScheduledAt *time.Time `json:"scheduled_at,omitempty" format:"date-time" doc:"Beta: scheduled sending may change before it is declared stable. Future instant a scheduled outbound send was queued to be submitted (outbound only; treat as \"not before\"). Present while a future send_at is set and retained afterwards; omitted for immediate sends and inbound rows."`
	CreatedAt   time.Time  `json:"created_at"`
	// ExpiresAt is nil for indefinitely retained messages. It remains on the
	// model for compatibility with account exports and legacy database rows.
	ExpiresAt *time.Time `json:"expires_at" nullable:"true" format:"date-time" doc:"Message expiry. Null means the message is retained indefinitely."`
	// DeletedAt is non-nil while the message is in the trash (soft-deleted,
	// migration 063): hidden from every agent-facing read path except the
	// single-message get (so the trash view can open it), restorable until
	// the janitor purges it after TrashRetention. Live data otherwise remains
	// indefinitely retained.
	DeletedAt       *time.Time `json:"deleted_at,omitempty"`
	WebhookStatus   string     `json:"webhook_status,omitempty"`
	WebhookError    string     `json:"webhook_error,omitempty"`
	WebhookAttempts int        `json:"webhook_attempts,omitempty"`
	// SizeBytes is the RAW MIME byte length of the whole stored message —
	// the octet length of raw_message (headers + bodies + encoded attachments
	// as transported). NOT a decoded-attachment size: the per-attachment
	// size_bytes (eventpayload.AttachmentMetaView / httpapi.AttachmentMetaView)
	// is the DECODED payload of one attachment. This raw length is also the
	// dominant term of storage-quota accounting — the messages storage
	// trigger (migrations 016/039) sums octet_length(raw_message) plus the
	// held-draft body columns into account_usage.storage_bytes.
	// Populated by load paths that compute it (e.g. GetMessagesByAgent for
	// the dashboard inbox). Zero on load paths that don't — the inbox
	// renders "—" in that case.
	SizeBytes int `json:"size_bytes,omitempty" doc:"RAW MIME byte length of the whole stored message (octet length of raw_message). Distinct from an attachment's size_bytes (DECODED payload size). Dominant term of storage-quota accounting (usage.storage_bytes)."`
	// InboxStatus mirrors messages.inbox_status ('unread' | 'read') for
	// inbound rows. Kept separate from DeliveryStatus (which currently
	// carries the same value under a confusing JSON key — see line 161)
	// so the dashboard's inbox can read it under a non-overloaded key.
	// Empty on outbound rows. Populated by GetMessagesByAgent.
	InboxStatus string `json:"read_status,omitempty"`

	// Multi-recipient fields. For outbound, these are the addressed
	// To/Cc/Bcc recipients of the send. For inbound, ToRecipients and CC
	// are the parsed To: and Cc: headers of the original message (the
	// per-delivery target for this row is in Recipient). BCC is
	// outbound-only.
	ToRecipients []string `json:"to,omitempty"`
	CC           []string `json:"cc,omitempty"`
	BCC          []string `json:"bcc,omitempty"`

	// ReplyTo is the parsed Reply-To: header on inbound messages — empty
	// when the header was absent. Distinct from Sender so consumers can
	// recover the original From: of forwarded / notification mail whose
	// Reply-To points at a different mailbox. Outbound-irrelevant.
	ReplyTo []string `json:"reply_to,omitempty"`

	// Labels are user-applied string tags (`urgent`, `follow-up`, …).
	// Always lowercase, charset `[a-z0-9:_-]+`, ≤ 64 chars per label,
	// capped at 100 per message. Empty slice means no labels — the DB
	// default is `'{}'` so this is never null on read. Labels with the
	// `e2a:` prefix are reserved for server-applied system labels;
	// caller writes that try to set them are rejected at the API layer.
	Labels []string `json:"labels,omitempty"`

	// HITL approval fields. Body and attachments are retained through terminal
	// transitions so outbound history remains complete.
	Status            string     `json:"status,omitempty"`
	ApprovalExpiresAt *time.Time `json:"approval_expires_at,omitempty"`
	ReviewedAt        *time.Time `json:"reviewed_at,omitempty"`
	// ReviewedByUserID identifies the human reviewer who approved or
	// rejected this message. NULL on worker-triggered transitions
	// (TTL auto-approve / auto-reject) — operator-visible signal "no
	// human looked at this." Set by ApproveAndSend and RejectPending,
	// left null by ExpireApproveAndSend / ExpireReject.
	ReviewedByUserID *string `json:"reviewed_by_user_id,omitempty"`
	// ReviewedByName is the JOIN'd display name from the reviewer's
	// users row, populated only by GetOutboundMessageForUser. List
	// endpoints leave this empty to avoid a join-per-row cost — the
	// pending-detail page is where reviewer attribution matters.
	ReviewedByName  *string `json:"reviewed_by_name,omitempty"`
	RejectionReason string  `json:"rejection_reason,omitempty"`
	Edited          bool    `json:"edited,omitempty"`
	BodyText        string  `json:"text,omitempty"`
	BodyHTML        string  `json:"html,omitempty"`
	// AttachmentsJSON is the INTERNAL storage blob for a held draft's
	// attachments (messages.attachments_json): the []outbound.Attachment
	// shape {filename, content_type, data} with data as base64 bytes. It
	// is populated for reviewed outbound drafts and retained after terminal
	// transitions. It is what the approve path recomposes the send from.
	// Never serialized — the wire representation is Attachments below.
	AttachmentsJSON    json.RawMessage `json:"-"`
	ManagedUnsubscribe bool            `json:"-"`
	// LifecycleTransitions carries exact rows appended by the transaction that
	// returned this message. It is internal event-building context, never part of
	// the Message JSON representation.
	LifecycleTransitions []messagelifecycle.MessageLifecycleTransition `json:"-"`
	// Attachments is the typed per-attachment METADATA for the wire (the
	// user-data export's Message schema) — the same AttachmentMetaView shape
	// {filename, content_type, size_bytes (DECODED), index} the live API
	// (MessageView.attachments, email.received) uses. Populated at export
	// time: parsed from raw_message when present (inbound + sent outbound),
	// else mapped from the held-draft AttachmentsJSON blob. Bytes are never
	// inlined — for sent/inbound messages they are inside the exported
	// raw_message.
	Attachments []eventpayload.AttachmentMetaView `json:"attachments,omitempty"`

	// Flagged + FlagReason carry the inbound ingestion verdict (migration 033 /
	// Slice 7): true when the agent's inbound_policy gate flagged this message
	// on arrival (still delivered, never dropped). FlagReason is the
	// human-readable reason. Inbound-relevant; outbound rows read false/''.
	Flagged    bool   `json:"flagged,omitempty"`
	FlagReason string `json:"flag_reason,omitempty"`

	// ReviewReason / ScanScore / ScanAction carry the applied screening verdict
	// (migration 037 / Slice 2), denormalized onto the row for fast review-queue
	// rendering. ReviewReason is one of sender_gate|recipient_gate|inbound_scan|
	// outbound_scan|outbound_send; ScanAction is the applied action
	// (flag|review|block); ScanScore is the aggregate 0..1 score (nil for gate-only
	// holds). The full per-detector breakdown lives in protection_events.
	ReviewReason string   `json:"review_reason,omitempty"`
	ScanScore    *float64 `json:"scan_score,omitempty"`
	ScanAction   string   `json:"scan_action,omitempty"`
}

// MarshalJSON preserves the database-friendly string representation while
// exposing unavailable inbound identity fields as JSON null. These fields are
// required-but-nullable in the public export contract.
func (m Message) MarshalJSON() ([]byte, error) {
	type messageAlias Message
	var headerFrom, envelopeFrom *string
	if m.HeaderFrom != "" {
		headerFrom = &m.HeaderFrom
	}
	if m.EnvelopeFrom != "" {
		envelopeFrom = &m.EnvelopeFrom
	}
	return json.Marshal(struct {
		messageAlias
		HeaderFrom     *string `json:"header_from"`
		EnvelopeFrom   *string `json:"envelope_from"`
		VerifiedDomain *string `json:"verified_domain"`
	}{
		messageAlias:   messageAlias(m),
		HeaderFrom:     headerFrom,
		EnvelopeFrom:   envelopeFrom,
		VerifiedDomain: m.Authentication.VerifiedDomain(),
	})
}

// InboundAuth is the canonical authentication evidence captured once during
// SMTP intake and persisted atomically with the message.
type InboundAuth struct {
	HeaderFrom     string
	EnvelopeFrom   string
	Authentication *emailauth.Authentication
	// StoredSender preserves internal reply-routing compatibility where it must
	// differ from the public RFC 5322 header identity (providerless loopback).
	// External SMTP callers leave it empty and persist HeaderFrom as before.
	StoredSender string
}

// Message status values mirror the CHECK constraint in migration 044_unify_holds.sql.
const (
	MessageStatusSent = "sent"

	// Unified review-hold statuses (direction-aware — design 2026-06-22). A held
	// message is one primitive regardless of direction; on resolution, approve =
	// send (outbound) / deliver to the agent (inbound), reject = drop. Outbound's
	// "approved" terminal is MessageStatusSent (the approve triggers the send), so
	// there is no separate outbound approved-but-unsent state.
	MessageStatusPendingReview         = "pending_review"
	MessageStatusReviewApproved        = "review_approved"
	MessageStatusReviewRejected        = "review_rejected"
	MessageStatusReviewExpiredApproved = "review_expired_approved"
	MessageStatusReviewExpiredRejected = "review_expired_rejected"
)

type Store struct {
	pool *pgxpool.Pool
	// senderIdentityGate caps provider mutations at one per process. Each
	// cross-process advisory lock owns one pool connection for the remote
	// call, so this prevents an SES slowdown from consuming the whole DB pool.
	// A capacity-1 channel rather than a sync.Mutex so a waiter whose context
	// is cancelled (an HTTP handler on a deadline, a worker shutting down)
	// unblocks with ctx.Err() instead of parking forever behind a slow SES
	// call. Lazily initialized (senderIdentityGateChan) so zero-value Stores
	// in tests keep working.
	senderIdentityGateOnce sync.Once
	senderIdentityGate     chan struct{}
	// dkimCipher envelope-encrypts DKIM private keys at rest (#144 / M4).
	// Optional: nil ⇒ keys are stored as plaintext DER (dev/test without a
	// configured signing secret). cmd/e2a always installs it in production.
	dkimCipher *DKIMCipher
	// outboundJobCanceller removes the delayed durable send when the owning
	// message is permanently deleted. Soft deletion deliberately does not use
	// it: restoring a trashed scheduled message before send_at must re-arm it.
	outboundJobCanceller OutboundJobCanceller
	// scheduledSendFinalizer records a late restore as a guarded terminal
	// failure (or settles authoritative provider-accept evidence as sent) and
	// publishes the matching terminal webhook in the caller's transaction.
	scheduledSendFinalizer ScheduledSendFinalizer
	// threadIdentityMetrics is the narrow, low-cardinality observability
	// surface for topology resolution and lazy legacy adoption. Optional so
	// stores in tests and embedded deployments remain inert by default.
	threadIdentityMetrics ThreadIdentityMetrics
}

// OutboundJobCanceller is the narrow River cancellation surface identity needs
// for hard deletes. The caller's transaction makes cancellation atomic with the
// message/agent/account deletion.
type OutboundJobCanceller interface {
	CancelTx(ctx context.Context, tx pgx.Tx, jobID int64) error
}

// ScheduledSendFinalizer owns the canonical terminal transition for a
// scheduled send restored after its cutoff. It is implemented by the outbound
// adapter so identity does not duplicate provider-evidence and webhook logic.
type ScheduledSendFinalizer interface {
	FinalizeScheduledCancellationTx(ctx context.Context, tx pgx.Tx, messageID string, jobID int64, occurredAt time.Time) error
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SetDKIMCipher enables envelope encryption of DKIM private keys at rest (#144).
// Optional-setter (matches SetEnforcer) so NewStore's signature —
// and the many tests that call NewStore(pool) — stay unchanged. When unset, keys
// are stored as plaintext DER. cmd/e2a always sets it in production, where
// Signing.HMACSecret is enforced ≥32 bytes.
func (s *Store) SetDKIMCipher(c *DKIMCipher) { s.dkimCipher = c }

// SetOutboundJobCanceller wires durable send-job cleanup into irreversible
// deletion paths. Production and full-stack test composition roots install the
// shared River client before serving requests.
func (s *Store) SetOutboundJobCanceller(c OutboundJobCanceller) {
	s.outboundJobCanceller = c
}

// SetScheduledSendFinalizer wires the canonical outbound terminal transition
// used when a scheduled message is restored after its cutoff.
func (s *Store) SetScheduledSendFinalizer(f ScheduledSendFinalizer) {
	s.scheduledSendFinalizer = f
}

// SetThreadMetrics installs the thread-resolution counter sink.
// Production wires the same telemetry backend used by the janitor gauges.
func (s *Store) SetThreadMetrics(metrics ThreadIdentityMetrics) {
	s.threadIdentityMetrics = metrics
}

func (s *Store) cancelOutboundJobIDsTx(ctx context.Context, tx pgx.Tx, jobIDs []int64) error {
	if len(jobIDs) == 0 {
		return nil
	}
	if s.outboundJobCanceller == nil {
		return errors.New("identity: outbound job canceller is not configured")
	}
	for _, jobID := range jobIDs {
		if err := s.outboundJobCanceller.CancelTx(ctx, tx, jobID); err != nil {
			return fmt.Errorf("cancel outbound job %d: %w", jobID, err)
		}
	}
	return nil
}

func scanOutboundJobIDs(rows pgx.Rows) ([]int64, error) {
	defer rows.Close()
	var jobIDs []int64
	for rows.Next() {
		var jobID *int64
		if err := rows.Scan(&jobID); err != nil {
			return nil, err
		}
		if jobID != nil {
			jobIDs = append(jobIDs, *jobID)
		}
	}
	return jobIDs, rows.Err()
}

type pastDueScheduledJob struct {
	messageID string
	jobID     int64
}

func (s *Store) cancelPastDueScheduledJobsTx(ctx context.Context, tx pgx.Tx, jobs []pastDueScheduledJob) error {
	if len(jobs) == 0 {
		return nil
	}
	if s.scheduledSendFinalizer == nil {
		return errors.New("identity: scheduled send finalizer is not configured")
	}
	jobIDs := make([]int64, len(jobs))
	for i := range jobs {
		jobIDs[i] = jobs[i].jobID
	}
	if err := s.cancelOutboundJobIDsTx(ctx, tx, jobIDs); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, job := range jobs {
		if err := s.scheduledSendFinalizer.FinalizeScheduledCancellationTx(
			ctx, tx, job.messageID, job.jobID, now,
		); err != nil {
			return err
		}
	}
	return nil
}

// sealDKIM encrypts a DKIM private key for storage when a cipher is configured,
// else returns the plaintext DER unchanged. domain is bound as AAD.
func (s *Store) sealDKIM(der []byte, domain string) ([]byte, error) {
	if s.dkimCipher == nil || len(der) == 0 {
		return der, nil
	}
	return s.dkimCipher.seal(der, domain)
}

// unsealDKIM reverses sealDKIM. Legacy plaintext rows (untagged DER) pass through
// unchanged, so reads tolerate a half-migrated table. An encrypted blob with no
// cipher configured is a hard error — fail closed, never return ciphertext as a
// key. domain must be the normalized form used at seal time.
func (s *Store) unsealDKIM(blob []byte, domain string) ([]byte, error) {
	if len(blob) == 0 || blob[0] != dkimBlobV1 {
		return blob, nil
	}
	if s.dkimCipher == nil {
		return nil, fmt.Errorf("dkim key for %q is encrypted but no cipher is configured", domain)
	}
	return s.dkimCipher.open(blob, domain)
}

// EncryptLegacyDKIMKeys re-encrypts any DKIM private keys still stored as
// plaintext DER (rows written before encryption-at-rest, #144). It is idempotent
// and self-terminating: only untagged rows (first byte != dkimBlobV1) are
// selected, so a second run is a no-op. No-op when no cipher is configured. The
// domains table is small, so it is safe to run at every startup. Returns the
// number of rows encrypted.
func (s *Store) EncryptLegacyDKIMKeys(ctx context.Context) (int, error) {
	if s.dkimCipher == nil {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx,
		// octet_length > 0 guards get_byte, which errors on a zero-length bytea
		// (the codebase writes NULL not empty, but be robust to out-of-band rows).
		`SELECT domain, dkim_private_key FROM domains
		  WHERE octet_length(dkim_private_key) > 0 AND get_byte(dkim_private_key, 0) <> $1`,
		int(dkimBlobV1),
	)
	if err != nil {
		return 0, fmt.Errorf("dkim backfill scan: %w", err)
	}
	type legacyRow struct {
		domain string
		der    []byte
	}
	var legacy []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.domain, &r.der); err != nil {
			rows.Close()
			return 0, fmt.Errorf("dkim backfill row: %w", err)
		}
		legacy = append(legacy, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("dkim backfill iterate: %w", err)
	}

	n := 0
	for _, r := range legacy {
		sealed, err := s.dkimCipher.seal(r.der, r.domain)
		if err != nil {
			return n, fmt.Errorf("dkim backfill seal %q: %w", r.domain, err)
		}
		// Re-check the tag in the WHERE so a concurrent run can't double-encrypt.
		tag, err := s.pool.Exec(ctx,
			`UPDATE domains SET dkim_private_key = $2
			  WHERE domain = $1 AND octet_length(dkim_private_key) > 0
			    AND get_byte(dkim_private_key, 0) <> $3`,
			r.domain, sealed, int(dkimBlobV1),
		)
		if err != nil {
			return n, fmt.Errorf("dkim backfill update %q: %w", r.domain, err)
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// --- Domain CRUD ---

// EnsureSharedDomain inserts a system row for the configured shared
// mail domain so slug-based agent registration can satisfy the
// agent_identities.registered_domain → domains.domain foreign key. The row is
// owned by no user (user_id = NULL) and pre-verified — it represents
// infrastructure the operator runs, not user-claimed identity.
//
// Called once at server startup. Idempotent via ON CONFLICT DO NOTHING,
// and a no-op when the operator has not configured a shared domain.
// Without this, any deployment whose shared_domain differs from the
// hardcoded migration seed (`agents.e2a.dev`) gets an FK violation the
// first time a user tries to register a slug-based agent.
func (s *Store) EnsureSharedDomain(ctx context.Context, domain string) error {
	if domain == "" {
		return nil
	}
	domain = normalizeDomain(domain)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO domains (domain, user_id, verified, verified_at)
		 VALUES ($1, NULL, true, now())
		 ON CONFLICT (domain) DO NOTHING`,
		domain,
	)
	if err != nil {
		return fmt.Errorf("ensure shared domain %q: %w", domain, err)
	}
	return nil
}

// ClaimOrCreateDomain atomically claims a DNS namespace, with no cap on how
// many domains userID may hold. See ClaimOrCreateDomainWithLimit for the
// account-facing, cap-enforcing form used outside this unlimited case.
func (s *Store) ClaimOrCreateDomain(ctx context.Context, domain, userID string) (*Domain, error) {
	return s.claimOrCreateDomain(ctx, domain, userID, 0)
}

// ClaimOrCreateDomainWithLimit is ClaimOrCreateDomain plus an atomic
// max_domains check in the same advisory-locked transaction as the INSERT,
// closing the pre-insert-count race (#822). maxDomains <= 0 means unlimited.
func (s *Store) ClaimOrCreateDomainWithLimit(ctx context.Context, domain, userID string, maxDomains int) (*Domain, error) {
	return s.claimOrCreateDomain(ctx, domain, userID, maxDomains)
}

// claimOrCreateDomain atomically claims a DNS namespace. A row reserves its
// exact name plus every ancestor/descendant against other accounts; the same
// account may explicitly register a child. The
// verification_token and DKIM keypair are minted on first INSERT and remain
// stable across re-claims — a caller that has already published the TXT
// record on DNS (or has mail in flight signed with the DKIM key) isn't
// silently invalidated by a second call. A different user cannot take
// over an unverified row; that closes a squatting window where the new
// owner could verify against a TXT record the original owner already
// published. The managed bounce.<domain> subtree is reserved once an account
// owns the parent because SES custom MAIL FROM uses that namespace.
func (s *Store) claimOrCreateDomain(ctx context.Context, domain, userID string, maxDomains int) (*Domain, error) {
	domain = normalizeDomain(domain)

	verificationToken := "e2a-verify=" + generateID()

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Parent and child candidates share at least one lock name. Taking all
	// suffix locks in sorted order makes the subsequent hierarchy check+insert
	// serializable without forcing unrelated registrable domains to contend.
	for _, name := range domainClaimLockNames(domain) {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, name); err != nil {
			return nil, err
		}
	}

	// A cap-enforcing caller also takes a per-user lock (keyspace 1, distinct
	// from the domain-name locks above), on the SAME connection as the count
	// check and insert below, serializing this user's creates against a race.
	if maxDomains > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1))`, userID); err != nil {
			return nil, err
		}
	}

	var crossAccountConflict bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM domains
			 WHERE (domain = $1
			        OR right(domain, char_length($1) + 1) = '.' || $1
			        OR right($1, char_length(domain) + 1) = '.' || domain)
			   AND user_id IS DISTINCT FROM $2
		)`, domain, userID,
	).Scan(&crossAccountConflict); err != nil {
		return nil, err
	}
	if crossAccountConflict {
		return nil, ErrDomainTaken
	}

	var reserved bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM domains
			 WHERE user_id = $2
			   AND ($1 = 'bounce.' || domain
			        OR right($1, char_length(domain) + 8) = '.bounce.' || domain)
		)`, domain, userID,
	).Scan(&reserved); err != nil {
		return nil, err
	}
	if reserved {
		return nil, ErrReservedDomain
	}

	d := &Domain{}
	// A same-owner re-claim is idempotent and returns this existing row, which
	// POST /v1/domains serializes through the same view as GET — so AgentCount
	// must be the real count (same correlated subquery as LookupDomain /
	// ListDomainsByUser, trashed agents excluded), not the Go zero value
	// (#811). The INSERT branch below deliberately omits it: a brand-new row
	// cannot have agents, so its zero is genuinely correct.
	err = tx.QueryRow(ctx,
		`SELECT domain, user_id, verified, verification_token, created_at, verified_at, is_primary, last_checked_at, COALESCE(dkim_selector, ''), COALESCE(dkim_public_key, ''), sending_status, COALESCE(sending_error, ''), sending_dns_records, sending_last_checked_at, COALESCE(sending_dkim_status, ''), COALESCE(sending_mail_from_status, ''),
		        (SELECT count(*) FROM agent_identities a
		           WHERE a.registered_domain = domains.domain AND a.user_id = domains.user_id
		             AND a.deleted_at IS NULL) AS agent_count
		 FROM domains WHERE domain = $1`, domain,
	).Scan(&d.Domain, &d.UserID, &d.Verified, &d.VerificationToken, &d.CreatedAt, &d.VerifiedAt, &d.IsPrimary, &d.LastCheckedAt, &d.DKIMSelector, &d.DKIMPublicKey, &d.SendingStatus, &d.SendingError, &d.SendingDNSRecordsJSON, &d.SendingLastCheckedAt, &d.SendingDkimStatus, &d.SendingMailFromStatus, &d.AgentCount)
	if errors.Is(err, pgx.ErrNoRows) {
		// Only a genuine new row is chargeable (a re-claim already returned
		// above). Read under the per-user lock, so this reflects every insert
		// already committed by a concurrent request, not a stale count (#822).
		if maxDomains > 0 {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM domains WHERE user_id = $1`, userID).Scan(&count); err != nil {
				return nil, err
			}
			if count >= maxDomains {
				return nil, &DomainLimitExceededError{Limit: maxDomains, Current: count}
			}
		}

		// Generate a DKIM keypair for this domain. Only reached on a genuine
		// new row (#826); a re-claim returns above via SELECT and never runs
		// this. Non-fatal on failure (nullable columns; signer treats a
		// missing key as "skip DKIM"), logged since it signals RNG/entropy
		// trouble.
		var dkimSelector string
		var dkimPubKey string
		var dkimPrivKey []byte
		if kp, kerr := dkim.GenerateKeypair(); kerr == nil {
			// Encrypt the private key at rest (#144). On seal failure (catastrophic
			// RNG) drop ALL three DKIM columns so we never publish a public key /
			// selector without a usable private key, non-fatal, same posture as a
			// keygen failure (the signer treats a missing key as "skip DKIM").
			sealed, serr := s.sealDKIM(kp.PrivateKeyDER, domain)
			if serr != nil {
				log.Printf("[identity] dkim key seal failed for %s: %v", domain, serr)
			} else {
				dkimSelector = kp.Selector
				dkimPubKey = kp.PublicKeyDNS
				dkimPrivKey = sealed
			}
		} else {
			log.Printf("[identity] dkim keygen failed for %s: %v", domain, kerr)
		}

		err = tx.QueryRow(ctx,
			`INSERT INTO domains (domain, user_id, verified, verification_token, dkim_selector, dkim_public_key, dkim_private_key)
			 VALUES ($1, $2, false, $3, $4, $5, $6)
			 RETURNING domain, user_id, verified, verification_token, created_at, verified_at, is_primary, last_checked_at, COALESCE(dkim_selector, ''), COALESCE(dkim_public_key, ''), sending_status, COALESCE(sending_error, ''), sending_dns_records, sending_last_checked_at, COALESCE(sending_dkim_status, ''), COALESCE(sending_mail_from_status, '')`,
			domain, userID, verificationToken, nullIfEmpty(dkimSelector), nullIfEmpty(dkimPubKey), nullIfEmptyBytes(dkimPrivKey),
		).Scan(&d.Domain, &d.UserID, &d.Verified, &d.VerificationToken, &d.CreatedAt, &d.VerifiedAt, &d.IsPrimary, &d.LastCheckedAt, &d.DKIMSelector, &d.DKIMPublicKey, &d.SendingStatus, &d.SendingError, &d.SendingDNSRecordsJSON, &d.SendingLastCheckedAt, &d.SendingDkimStatus, &d.SendingMailFromStatus)
	}
	if err != nil {
		return nil, err
	}

	// A pending child reserves its namespace immediately, but inherited agents
	// remain bound to their verified ancestor so registration alone cannot take
	// working inboxes offline. VerifyDomain atomically promotes them once the
	// child is verified.
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

func domainClaimLockNames(domain string) []string {
	names := append([]string{domain}, coveringParentCandidates(domain)...)
	sort.Strings(names)
	return names
}

// AdoptSharedDomain assigns ownership of the server-seeded shared-domain row to
// userID. The server seeds that row on every boot via EnsureSharedDomain
// (user_id NULL, verified true, ON CONFLICT DO NOTHING); ClaimOrCreateDomain
// cannot claim it — it only upserts an unverified, same-user row — yet the
// probe/system identity that seeds itself lives on the shared domain by design.
//
// The UPDATE is guarded to a VERIFIED, currently-unowned row (or one already
// owned by userID, so re-seeding is idempotent). Requiring verified=true — the
// signature EnsureSharedDomain stamps — keeps this method from ever adopting
// some other ownerless row, so its safety does not rely on the domains schema
// never producing another NULL-owner row nor on the caller passing only a
// trusted domain. A row owned by a different account, a nonexistent row, or an
// ownerless-but-unverified row all match nothing and return ErrDomainTaken. The
// verified flag and DKIM columns are not modified.
func (s *Store) AdoptSharedDomain(ctx context.Context, domain, userID string) (*Domain, error) {
	domain = normalizeDomain(domain)
	d := &Domain{}
	err := s.pool.QueryRow(ctx,
		`UPDATE domains SET user_id = $2
		 WHERE domain = $1 AND verified = true AND (user_id IS NULL OR user_id = $2)
		 RETURNING domain, user_id, verified, verification_token, created_at, verified_at, is_primary, last_checked_at, COALESCE(dkim_selector, ''), COALESCE(dkim_public_key, ''), sending_status, COALESCE(sending_error, ''), sending_dns_records, sending_last_checked_at, COALESCE(sending_dkim_status, ''), COALESCE(sending_mail_from_status, '')`,
		domain, userID,
	).Scan(&d.Domain, &d.UserID, &d.Verified, &d.VerificationToken, &d.CreatedAt, &d.VerifiedAt, &d.IsPrimary, &d.LastCheckedAt, &d.DKIMSelector, &d.DKIMPublicKey, &d.SendingStatus, &d.SendingError, &d.SendingDNSRecordsJSON, &d.SendingLastCheckedAt, &d.SendingDkimStatus, &d.SendingMailFromStatus)
	if err == nil {
		return d, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// No adoptable row: it doesn't exist, or a different account owns it.
		return nil, ErrDomainTaken
	}
	return nil, fmt.Errorf("adopt shared domain %q: %w", domain, err)
}

// nullIfEmpty returns nil for empty strings so we can write SQL NULL
// (rather than empty-string) for nullable text columns. Pgx treats an
// untyped nil interface{} as NULL.
func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// nullIfEmptyBytes is the BYTEA counterpart of nullIfEmpty.
func nullIfEmptyBytes(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return b
}

// GetDKIMKeyInternal returns the stored selector + private key bytes
// for a domain. The "Internal" suffix is load-bearing: this function
// does NOT scope by user — it takes a domain name and returns whoever
// owns that domain's signing key. ONLY call from server-internal
// codepaths where the domain has already been resolved from a
// trusted source (e.g. an outbound message's sender field, after the
// agent layer has authenticated the owner). A handler that ever
// takes a user-supplied domain string and feeds it to this function
// becomes a "sign as anyone" primitive: don't.
//
// Returns ("", nil, nil) when the domain has no key — callers MUST
// treat this as "skip signing" and fall back to whatever the
// relay-level fallback does.
func (s *Store) GetDKIMKeyInternal(ctx context.Context, domain string) (string, []byte, error) {
	norm := normalizeDomain(domain)
	var selector string
	var privKey []byte
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(dkim_selector, ''), dkim_private_key FROM domains WHERE domain = $1`,
		norm,
	).Scan(&selector, &privKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("dkim key lookup: %w", err)
	}
	der, err := s.unsealDKIM(privKey, norm)
	if err != nil {
		return "", nil, fmt.Errorf("dkim key unseal: %w", err)
	}
	return selector, der, nil
}

// --- Sender identity (decision 4 / Slice 4) ---
//
// These primitive accessors back the senderidentity.RawStore interface
// (string status, JSON dns records) so the core store stays decoupled from
// the senderidentity package (and its River + AWS SDK deps). The adapter in
// senderidentity converts to its typed Status/DNSRecord.

// SendingProvisionInputs returns the per-domain DKIM selector + private key
// for BYODKIM provisioning. ok=false means no usable key material. Like
// GetDKIMKeyInternal this is unscoped — call only with a server-resolved
// domain.
func (s *Store) SendingProvisionInputs(ctx context.Context, domain string) (selector string, privateKeyDER []byte, ok bool, err error) {
	norm := normalizeDomain(domain)
	var blob []byte
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(dkim_selector, ''), dkim_private_key FROM domains WHERE domain = $1`,
		norm,
	).Scan(&selector, &blob)
	if err != nil {
		return "", nil, false, err // includes pgx.ErrNoRows (domain gone)
	}
	if selector == "" || len(blob) == 0 {
		return "", nil, false, nil
	}
	privateKeyDER, err = s.unsealDKIM(blob, norm)
	if err != nil {
		return "", nil, false, fmt.Errorf("dkim key unseal: %w", err)
	}
	return selector, privateKeyDER, true, nil
}

// senderIdentityExecutorKey pins sender-identity reads/writes to the same
// connection that owns the session advisory lock. Keeping the lock and the
// state snapshot on one connection avoids consuming a second pool connection
// per worker (and the pool-exhaustion deadlock that can otherwise create).
type senderIdentityExecutorKey struct{}

type senderIdentityExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) senderIdentityExecutor(ctx context.Context) senderIdentityExecutor {
	if conn, ok := ctx.Value(senderIdentityExecutorKey{}).(*pgxpool.Conn); ok {
		return conn
	}
	return s.pool
}

func (s *Store) senderIdentityBegin(ctx context.Context) (pgx.Tx, error) {
	if conn, ok := ctx.Value(senderIdentityExecutorKey{}).(*pgxpool.Conn); ok {
		return conn.Begin(ctx)
	}
	return s.pool.Begin(ctx)
}

// WithSendingIdentityMutationLock serializes provider mutations for one
// domain across workers, processes, and blue/green replicas. The callback gets
// a context that pins the sender-identity store methods below to the lock-owning
// connection, so a worker needs only one pool connection while it waits on SES.
func (s *Store) WithSendingIdentityMutationLock(ctx context.Context, domain string, fn func(context.Context) error) (retErr error) {
	gate := s.senderIdentityGateChan()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-gate }()

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, "sender-identity:"+normalizeDomain(domain)); err != nil {
		// Cancellation has an uncertain outcome: PostgreSQL may have acquired
		// the session lock even though the client never received success. Never
		// return that connection to the pool. Closing the hijacked session is the
		// only outcome-safe way to release a possibly-held advisory lock.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		raw := conn.Hijack()
		_ = raw.Close(closeCtx)
		return fmt.Errorf("acquire sender identity mutation lock: %w", err)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, unlockErr := conn.Exec(releaseCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, "sender-identity:"+normalizeDomain(domain)); unlockErr != nil {
			// Never return a connection carrying a session lock to the pool. Hijack
			// removes it from the pool; closing the raw connection releases the lock.
			raw := conn.Hijack()
			_ = raw.Close(releaseCtx)
			retErr = errors.Join(retErr, fmt.Errorf("release sender identity mutation lock: %w", unlockErr))
			return
		}
		conn.Release()
	}()

	lockedCtx := context.WithValue(ctx, senderIdentityExecutorKey{}, conn)
	return fn(lockedCtx)
}

// senderIdentityGateChan lazily initializes the process-wide mutation gate so
// Stores built as zero-value literals (tests) work like NewStore-built ones.
func (s *Store) senderIdentityGateChan() chan struct{} {
	s.senderIdentityGateOnce.Do(func() { s.senderIdentityGate = make(chan struct{}, 1) })
	return s.senderIdentityGate
}

// LoadSendingIdentityState returns one incarnation-consistent snapshot for the
// provider synchronizer. A live but unverified domain is returned with
// verified=false; callers converge that state to no provider identity.
// appliedIncarnation is the ledger's provider-confirmed incarnation ("" when
// the ledger has no row or nothing confirmed) — the signal that lets a forced
// re-check of a healthy domain stay a read-only no-op.
func (s *Store) LoadSendingIdentityState(ctx context.Context, domain string) (incarnation, owner string, verified bool, status, selector string, privateKeyDER []byte, appliedIncarnation string, ledgerUpdatedAt time.Time, err error) {
	norm := normalizeDomain(domain)
	var blob []byte
	var ledgerAt *time.Time
	err = s.senderIdentityExecutor(ctx).QueryRow(ctx,
		`SELECT d.verification_token, COALESCE(d.user_id::text, ''), d.verified, d.sending_status,
		        COALESCE(d.dkim_selector, ''), d.dkim_private_key,
		        COALESCE(m.applied_incarnation, ''), m.updated_at
		   FROM domains d
		   LEFT JOIN sender_identity_managed_domains m ON m.domain = d.domain
		  WHERE d.domain = $1`,
		norm,
	).Scan(&incarnation, &owner, &verified, &status, &selector, &blob, &appliedIncarnation, &ledgerAt)
	if err != nil {
		return "", "", false, "", "", nil, "", time.Time{}, err
	}
	if ledgerAt != nil {
		ledgerUpdatedAt = *ledgerAt
	}
	if !verified || selector == "" || len(blob) == 0 {
		return incarnation, owner, verified, status, selector, nil, appliedIncarnation, ledgerUpdatedAt, nil
	}
	privateKeyDER, err = s.unsealDKIM(blob, norm)
	if err != nil {
		return "", "", false, "", "", nil, "", time.Time{}, fmt.Errorf("dkim key unseal: %w", err)
	}
	return incarnation, owner, verified, status, selector, privateKeyDER, appliedIncarnation, ledgerUpdatedAt, nil
}

// SetSendingStatusForIncarnation refuses to write through a delete/re-register
// race. pgx.ErrNoRows makes the stale worker retry; its next desired-state sync
// then converges the provider to the current row (or to absence).
func (s *Store) SetSendingStatusForIncarnation(ctx context.Context, domain, incarnation, status, dkimStatus, mailFromStatus, errMsg string, recordsJSON []byte) error {
	var errPtr *string
	if errMsg != "" {
		errPtr = &errMsg
	}
	tag, err := s.senderIdentityExecutor(ctx).Exec(ctx,
		`UPDATE domains
		    SET sending_status = $3,
		        sending_error = $4,
		        sending_dns_records = $5,
		        sending_dkim_status = $6,
		        sending_mail_from_status = $7,
		        sending_last_checked_at = now()
		  WHERE domain = $1 AND verification_token = $2`,
		normalizeDomain(domain), incarnation, status, errPtr, recordsJSON, nullIfEmpty(dkimStatus), nullIfEmpty(mailFromStatus),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) TouchSendingCheckedForIncarnation(ctx context.Context, domain, incarnation string) error {
	tag, err := s.senderIdentityExecutor(ctx).Exec(ctx,
		`UPDATE domains SET sending_last_checked_at = now() WHERE domain = $1 AND verification_token = $2`,
		normalizeDomain(domain), incarnation,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SetSendingStatus writes the sending lifecycle state for a domain and stamps
// sending_last_checked_at. recordsJSON may be nil (cleared). dkimStatus and
// mailFromStatus are the per-axis SES breakdown (migration 049); an empty
// string for either is written as SQL NULL so the read path falls back to the
// all-or-nothing sending_status rollup (and the CHECK constraint, which allows
// NULL but not ”, is satisfied).
func (s *Store) SetSendingStatus(ctx context.Context, domain, status, dkimStatus, mailFromStatus, errMsg string, recordsJSON []byte) error {
	var errPtr *string
	if errMsg != "" {
		errPtr = &errMsg
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE domains
		    SET sending_status = $2,
		        sending_error = $3,
		        sending_dns_records = $4,
		        sending_dkim_status = $5,
		        sending_mail_from_status = $6,
		        sending_last_checked_at = now()
		  WHERE domain = $1`,
		normalizeDomain(domain), status, errPtr, recordsJSON, nullIfEmpty(dkimStatus), nullIfEmpty(mailFromStatus),
	)
	return err
}

// TouchSendingChecked stamps sending_last_checked_at without changing status
// (a still-pending poll).
func (s *Store) TouchSendingChecked(ctx context.Context, domain string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE domains SET sending_last_checked_at = now() WHERE domain = $1`,
		normalizeDomain(domain),
	)
	return err
}

// GetSendingStatus returns the domain's sending_status. Propagates
// pgx.ErrNoRows when the domain row is gone.
func (s *Store) GetSendingStatus(ctx context.Context, domain string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT sending_status FROM domains WHERE domain = $1`,
		normalizeDomain(domain),
	).Scan(&status)
	if err != nil {
		return "", err
	}
	return status, nil
}

// DomainOwner returns the user_id owning a domain, or "" for an unowned
// (system) domain. pgx.ErrNoRows → ("", nil) so the caller treats a missing
// domain as "no owner, no event".
func (s *Store) DomainOwner(ctx context.Context, domain string) (string, error) {
	var owner *string
	err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM domains WHERE domain = $1`,
		normalizeDomain(domain),
	).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if owner == nil {
		return "", nil
	}
	return *owner, nil
}

// DomainExists reports whether any account currently owns the exact DNS name.
// It deliberately returns only a boolean: the orphan reaper and historical
// teardown-receipt safety checks need existence without owner disclosure.
func (s *Store) DomainExists(ctx context.Context, domain string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM domains WHERE domain = $1)`,
		normalizeDomain(domain),
	).Scan(&exists)
	return exists, err
}

// MarkSendingIdentityManaged records that e2a may have created a provider
// identity for this domain. Call it before the external create/update so an
// ambiguous transport failure still leaves a durable cleanup candidate.
func (s *Store) MarkSendingIdentityManaged(ctx context.Context, domain, incarnation string) error {
	_, err := s.senderIdentityExecutor(ctx).Exec(ctx,
		`INSERT INTO sender_identity_managed_domains (domain, incarnation, applied_incarnation, updated_at, provider_pending_since)
		 VALUES ($1, $2, NULL, clock_timestamp(), NULL)
		 ON CONFLICT (domain) DO UPDATE
		 SET incarnation = EXCLUDED.incarnation,
		     applied_incarnation = NULL,
		     updated_at = clock_timestamp(),
		     provider_pending_since = NULL`,
		normalizeDomain(domain), incarnation,
	)
	return err
}

// MarkSendingIdentityApplied records the incarnation whose selector/key was
// confirmed installed at the provider. Keeping this separate from the
// pre-mutation ownership mark lets the reaper repair ambiguous/exhausted
// creates without re-provisioning healthy identities every hour.
func (s *Store) MarkSendingIdentityApplied(ctx context.Context, domain, incarnation string) error {
	tag, err := s.senderIdentityExecutor(ctx).Exec(ctx,
		`UPDATE sender_identity_managed_domains
		    SET applied_incarnation = $2,
		        updated_at = clock_timestamp(),
		        provider_pending_since = NULL
		  WHERE domain = $1 AND incarnation = $2`,
		normalizeDomain(domain), incarnation,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SendingIdentityLedgerExpired evaluates the pending-verification backstop on
// the database clock. Comparing this persisted timestamp with the application
// host clock would make clock skew alter the state machine.
func (s *Store) SendingIdentityLedgerExpired(ctx context.Context, domain, incarnation string, olderThan time.Duration) (bool, error) {
	var expired bool
	err := s.senderIdentityExecutor(ctx).QueryRow(ctx,
		`SELECT updated_at <= clock_timestamp() - make_interval(secs => $3)
		   FROM sender_identity_managed_domains
		  WHERE domain = $1 AND incarnation = $2`,
		normalizeDomain(domain), incarnation, olderThan.Seconds(),
	).Scan(&expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return expired, err
}

// ObserveSendingIdentityProviderPending records the first time a DB-verified
// identity is seen provider-pending and returns whether that persisted grace
// period has elapsed. Repeated observations never reset the timestamp.
func (s *Store) ObserveSendingIdentityProviderPending(ctx context.Context, domain, incarnation string, olderThan time.Duration) (bool, error) {
	var expired bool
	err := s.senderIdentityExecutor(ctx).QueryRow(ctx,
		`UPDATE sender_identity_managed_domains
		    SET provider_pending_since = COALESCE(provider_pending_since, clock_timestamp())
		  WHERE domain = $1 AND incarnation = $2
		  RETURNING provider_pending_since <= clock_timestamp() - make_interval(secs => $3)`,
		normalizeDomain(domain), incarnation, olderThan.Seconds(),
	).Scan(&expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return expired, err
}

func (s *Store) ClearSendingIdentityProviderPending(ctx context.Context, domain, incarnation string) error {
	_, err := s.senderIdentityExecutor(ctx).Exec(ctx,
		`UPDATE sender_identity_managed_domains
		    SET provider_pending_since = NULL
		  WHERE domain = $1 AND incarnation = $2`,
		normalizeDomain(domain), incarnation,
	)
	return err
}

// ForgetSendingIdentityManaged removes the ledger row for domain UNLESS
// domain still exists in domains with a live owner (a non-NULL user_id).
//
// As of #908, this method has ZERO callers: both ErrIdentityNotOwned
// handlers in internal/senderidentity/worker.go that used to call it after
// an ownership failure were changed to call nothing instead, not to call
// something else here. It is kept rather than deleted: doing so would also
// require rewriting the rationale comments in those two handlers (which cite
// this method's guard by name while explaining why they must never call it)
// and the poisoned-fake regression tests in
// internal/senderidentity/worker_test.go that exist specifically to catch a
// reintroduced call — a larger, riskier change than this method's own
// dead-code status justifies in a hygiene pass. If a future change ever adds
// a call to this method again, it must NOT be from an ownership-failure
// branch: an ownership failure is never evidence of teardown, and forgetting
// the ledger row for a domain that is still live and owned would permanently
// strand it, because nothing else ever revisits a domain absent from this
// ledger. FinalizeSendingIdentityTombstone is the method for confirmed-
// teardown ledger cleanup.
//
// The guard is expressed as part of the DELETE's WHERE clause rather than a
// check-then-act at the call site: WithSendingIdentityMutationLock (held by
// both former callers) and the domain-delete transaction's advisory locks
// use different lock names and do not exclude each other, so a separate
// existence check beforehand could race a concurrent domain delete. A
// genuinely deleted (or ownerless system) domain still has its row removed
// here exactly as before; a DB failure leaves the ledger row for a later
// retry either way.
func (s *Store) ForgetSendingIdentityManaged(ctx context.Context, domain string) error {
	_, err := s.senderIdentityExecutor(ctx).Exec(ctx,
		`DELETE FROM sender_identity_managed_domains m
		  WHERE m.domain = $1
		    AND NOT EXISTS (
		      SELECT 1 FROM domains d WHERE d.domain = m.domain AND d.user_id IS NOT NULL
		    )`,
		normalizeDomain(domain),
	)
	return err
}

// FinalizeSendingIdentityTombstone removes the ledger row only when BOTH:
// its last mutation (updated_at — bumped by provision marks, the migration
// trigger, and TouchSendingIdentityTombstoneTx on delete) is older than
// olderThan, AND domain no longer exists in domains as a LIVE, OWNED,
// VERIFIED row (the same live-owner guard ForgetSendingIdentityManaged uses,
// now also requiring d.verified — see batch C finding 8 below). The age gate
// alone stops an audit or sweep running inside a LATER mutation's drain
// window from finalizing that mutation's tombstone. The live-owner guard is
// what makes "never forget a live owned VERIFIED domain's row" a genuine
// STORE invariant rather than something only ForgetSendingIdentityManaged
// upholds: this method's own callers include the no-key branch of
// syncProviderIdentityWithInspection, which can run against a domain that is
// verified, live, and owned but has NULL dkim_selector/dkim_private_key
// (e.g. a stuck migration) — without this guard that call strands the ledger
// row exactly like the original incident, just via a different trigger.
// Despite the name, this method's contract was never actually "only ever
// called for a genuinely torn-down domain" — that was true of every
// call site, not of the method itself. A domain that is torn down for real
// (DELETE FROM domains) satisfies NOT EXISTS immediately, so this is a
// tightening, not a behavior change for genuine teardown. A row that fails
// either gate survives as a no-op; a later audit or the hourly reaper
// finalizes it once both hold.
//
// The AND d.verified clause (added in batch C finding 8) is deliberate, not
// a widening of the guard's original NOT EXISTS(user_id IS NOT NULL): the
// teardown branch in syncProviderIdentityWithInspection fires whenever
// LoadSendingIdentityState reports pgx.ErrNoRows OR (err == nil &&
// !state.Verified) — the second case is a LIVE, OWNED row that is simply not
// (yet) DNS-verified, e.g. a delete immediately followed by a re-register of
// the same domain, landing a fresh unverified row before the post-drain
// audit runs. Guarding on user_id alone made Finalize a permanent no-op for
// that row: NOT EXISTS saw the live owned row and always blocked, so the old
// incarnation's ledger tombstone survived forever and the reaper re-swept it
// hourly. Requiring d.verified too means that legitimate re-register case
// (owned but unverified) no longer blocks Finalize, while both protected
// populations stay protected: the incident population this ledger exists
// for (verified/live/owned) still satisfies user_id IS NOT NULL AND
// verified, and so does the no-key branch's domain (verified, live, owned,
// merely missing key material).
func (s *Store) FinalizeSendingIdentityTombstone(ctx context.Context, domain string, olderThan time.Duration) error {
	_, err := s.senderIdentityExecutor(ctx).Exec(ctx,
		`DELETE FROM sender_identity_managed_domains m
		  WHERE m.domain = $1
		    AND m.updated_at <= now() - ($2 * interval '1 second')
		    AND NOT EXISTS (
		      SELECT 1 FROM domains d WHERE d.domain = m.domain AND d.user_id IS NOT NULL AND d.verified
		    )`,
		normalizeDomain(domain), olderThan.Seconds(),
	)
	return err
}

// TouchSendingIdentityTombstoneTx stamps the ledger row's updated_at inside a
// domain-delete transaction, marking the delete as the tombstone's latest
// mutation. Without it, a long-lived domain's ledger row is old at delete
// time and the very first audit/sweep could finalize the tombstone while the
// legacy slot is still draining. A domain with no ledger row (sender identity
// never provisioned) is a no-op.
func (s *Store) TouchSendingIdentityTombstoneTx(ctx context.Context, tx pgx.Tx, domain string) error {
	// clock_timestamp(), not now(): now() is transaction-START time, and the
	// delete tx can wait on the domain-claim advisory locks — a long wait
	// would commit an already-aged stamp and erode the drain-window guard.
	_, err := tx.Exec(ctx,
		`UPDATE sender_identity_managed_domains SET updated_at = clock_timestamp() WHERE domain = $1`,
		normalizeDomain(domain),
	)
	return err
}

// ListManagedSendingIdentityDomains returns only identities e2a has claimed
// ownership of. It deliberately does not scan/delete arbitrary SES account
// identities, which may belong to other applications.
func (s *Store) ListManagedSendingIdentityDomains(ctx context.Context) ([]string, map[string]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT domain, applied_incarnation IS DISTINCT FROM incarnation
		   FROM sender_identity_managed_domains ORDER BY domain`,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var domains []string
	needsProvision := make(map[string]bool)
	for rows.Next() {
		var domain string
		var needs bool
		if err := rows.Scan(&domain, &needs); err != nil {
			return nil, nil, err
		}
		domains = append(domains, domain)
		needsProvision[domain] = needs
	}
	return domains, needsProvision, rows.Err()
}

// ListManagedSendingIdentityDomainsPage keyset-pages the durable ledger so a
// single v2 reaper job cannot monopolize the sender-identity queue as the
// account grows. hasMore is derived with a limit+1 read; only limit rows are
// returned.
func (s *Store) ListManagedSendingIdentityDomainsPage(ctx context.Context, afterDomain string, limit int) ([]string, map[string]bool, bool, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.pool.Query(ctx,
		`SELECT domain, applied_incarnation IS DISTINCT FROM incarnation
		   FROM sender_identity_managed_domains
		  WHERE domain > $1
		  ORDER BY domain
		  LIMIT $2`,
		normalizeDomain(afterDomain), limit+1,
	)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	domains := make([]string, 0, limit+1)
	needsProvision := make(map[string]bool, limit+1)
	for rows.Next() {
		var domain string
		var needs bool
		if err := rows.Scan(&domain, &needs); err != nil {
			return nil, nil, false, err
		}
		domains = append(domains, domain)
		needsProvision[domain] = needs
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	hasMore := len(domains) > limit
	if hasMore {
		delete(needsProvision, domains[limit])
		domains = domains[:limit]
	}
	return domains, needsProvision, hasMore, nil
}

// LookupManagedSendingIdentityDomain checks exact ledger membership for one
// provider identity during the bounded orphan-audit phase.
func (s *Store) LookupManagedSendingIdentityDomain(ctx context.Context, domain string) (needsProvision, found bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT applied_incarnation IS DISTINCT FROM incarnation
		   FROM sender_identity_managed_domains WHERE domain = $1`,
		normalizeDomain(domain),
	).Scan(&needsProvision)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return needsProvision, true, nil
}

// LookupDomain returns a domain if it exists and is owned by the given user.
// AgentCount is populated with the same correlated subquery ListDomainsByUser
// uses (trashed agents excluded), so the single-resource and list responses
// agree instead of this path leaving it at the Go zero value.
func (s *Store) LookupDomain(ctx context.Context, domain, userID string) (*Domain, error) {
	d := &Domain{}
	err := s.pool.QueryRow(ctx,
		`SELECT domain, user_id, verified, verification_token, created_at, verified_at, is_primary, last_checked_at, COALESCE(dkim_selector, ''), COALESCE(dkim_public_key, ''), sending_status, COALESCE(sending_error, ''), sending_dns_records, sending_last_checked_at, COALESCE(sending_dkim_status, ''), COALESCE(sending_mail_from_status, ''),
		        (SELECT count(*) FROM agent_identities a
		           WHERE a.registered_domain = domains.domain AND a.user_id = domains.user_id
		             AND a.deleted_at IS NULL) AS agent_count
		 FROM domains WHERE domain = $1 AND user_id = $2`,
		normalizeDomain(domain), userID,
	).Scan(&d.Domain, &d.UserID, &d.Verified, &d.VerificationToken, &d.CreatedAt, &d.VerifiedAt, &d.IsPrimary, &d.LastCheckedAt, &d.DKIMSelector, &d.DKIMPublicKey, &d.SendingStatus, &d.SendingError, &d.SendingDNSRecordsJSON, &d.SendingLastCheckedAt, &d.SendingDkimStatus, &d.SendingMailFromStatus, &d.AgentCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDomainNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// coveringParentCandidates returns the proper DNS-label ancestors of sub,
// most-specific first, that could serve as a covering parent domain. For
// "acme.team.mnexa.ai" it yields ["team.mnexa.ai", "mnexa.ai"] — each is an
// exact ancestor on a label boundary, so this is the injection-safe,
// suffix-attack-proof basis for the parent match (a naive HasSuffix would let
// "evilteam.mnexa.ai" match "team.mnexa.ai"; here stripping the first label of
// "evilteam.mnexa.ai" yields "mnexa.ai", never "team.mnexa.ai"). sub itself is
// excluded (that is the exact-match case, handled by LookupDomain). Any
// candidate that is itself a public suffix (e.g. "ai", "co.uk") is dropped:
// no one can own+verify a public suffix, so it can never be a real parent, and
// excluding it is defense-in-depth against a pathological registered row.
func coveringParentCandidates(sub string) []string {
	sub = normalizeDomain(sub)
	var out []string
	rest := sub
	for {
		i := strings.IndexByte(rest, '.')
		if i < 0 {
			break
		}
		parent := rest[i+1:]
		rest = parent
		if parent == "" || !strings.Contains(parent, ".") {
			// A single-label remainder is a TLD; stop before it.
			if parent != "" && !isPublicSuffix(parent) {
				out = append(out, parent)
			}
			break
		}
		if isPublicSuffix(parent) {
			continue
		}
		out = append(out, parent)
	}
	return out
}

// isPublicSuffix reports whether d is itself a public suffix (ICANN or private),
// i.e. it has no registrable label of its own — mirrors emailauth.isPublicSuffix.
func isPublicSuffix(d string) bool {
	suffix, _ := publicsuffix.PublicSuffix(d)
	return suffix == d
}

// LookupCoveringDomain returns the MOST-SPECIFIC registered domain owned by
// userID that is a proper label-boundary parent of sub, or an error if none
// covers it. This backs subdomain-agent creation: a verified parent SES
// identity (e.g. team.mnexa.ai) DKIM-signs and DMARC-aligns mail from any
// subdomain (acme.team.mnexa.ai), so e2a lets an agent be created on the
// subdomain without a separate registration. The caller must inspect Verified:
// an explicitly registered pending child is authoritative for its subtree and
// must not be masked by a verified grandparent. The parent must be an exact
// ancestor on a DNS label boundary — see coveringParentCandidates. The
// returned Domain.Domain is what the caller stores in agent_identities.registered_domain
// so the FK, quota JOIN, DKIM signer, and sending-status lookup all resolve to
// the parent.
func (s *Store) LookupCoveringDomain(ctx context.Context, sub, userID string) (*Domain, error) {
	candidates := coveringParentCandidates(sub)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no covering domain")
	}
	// candidates is most-specific-first; ANY() ignores order, so pick the
	// most specific (longest, i.e. most labels) among the owned matches in Go.
	rows, err := s.pool.Query(ctx,
		`SELECT domain, user_id, verified, verification_token, created_at, verified_at, is_primary, last_checked_at, COALESCE(dkim_selector, ''), COALESCE(dkim_public_key, ''), sending_status, COALESCE(sending_error, ''), sending_dns_records, sending_last_checked_at, COALESCE(sending_dkim_status, ''), COALESCE(sending_mail_from_status, '')
		 FROM domains WHERE user_id = $1 AND domain = ANY($2)`,
		userID, candidates,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var best *Domain
	for rows.Next() {
		d := &Domain{}
		if err := rows.Scan(&d.Domain, &d.UserID, &d.Verified, &d.VerificationToken, &d.CreatedAt, &d.VerifiedAt, &d.IsPrimary, &d.LastCheckedAt, &d.DKIMSelector, &d.DKIMPublicKey, &d.SendingStatus, &d.SendingError, &d.SendingDNSRecordsJSON, &d.SendingLastCheckedAt, &d.SendingDkimStatus, &d.SendingMailFromStatus); err != nil {
			return nil, err
		}
		// Most-specific = most DNS labels. Ties are impossible (a domain is
		// unique in the table), so a strict '>' is deterministic.
		if best == nil || strings.Count(d.Domain, ".") > strings.Count(best.Domain, ".") {
			best = d
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if best == nil {
		return nil, fmt.Errorf("no covering domain")
	}

	// Cross-tenant namespace guard (QA F1): `best` proves the REQUESTER owns some
	// verified ANCESTOR of the address domain — but a DIFFERENT user may own a
	// MORE-SPECIFIC name inside that ancestor. Binding the agent to `best` would
	// then plant it inside another tenant's verified namespace (land-grab of
	// unclaimed subdomain addresses, inbound interception, DKIM-signed
	// impersonation). Reject the cover if ANY registered domain owned by another
	// user is equal to the address domain OR strictly more specific than `best`
	// (i.e. any name from the address domain up to, but excluding, `best`). A
	// same-user registration does not count as an intruder; when it is an
	// intermediate, the most-specific-owned selection above already returns it
	// and lets the caller gate on its verification state. Ownership is checked
	// regardless of another owner's verified state: registration claims the
	// namespace, including ownerless system rows.
	intruders := namesMoreSpecificThan(sub, best.Domain)
	if len(intruders) > 0 {
		var taken bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM domains WHERE domain = ANY($1) AND user_id IS DISTINCT FROM $2)`,
			intruders, userID,
		).Scan(&taken); err != nil {
			return nil, err
		}
		if taken {
			return nil, fmt.Errorf("no covering domain")
		}
	}
	return best, nil
}

// namesMoreSpecificThan returns the DNS names covering sub that are strictly
// more specific than match: sub itself plus every ancestor of sub with more
// labels than match (i.e. the names strictly between sub and match, inclusive of
// sub, exclusive of match). For sub="acme.team.mnexa.ai", match="mnexa.ai" it
// yields ["acme.team.mnexa.ai", "team.mnexa.ai"]. These are exactly the names a
// different tenant could own that would make covering by `match` an intrusion.
func namesMoreSpecificThan(sub, match string) []string {
	sub = normalizeDomain(sub)
	matchLabels := strings.Count(match, ".")
	var out []string
	name := sub
	for strings.Count(name, ".") > matchLabels {
		out = append(out, name)
		i := strings.IndexByte(name, '.')
		if i < 0 {
			break
		}
		name = name[i+1:]
	}
	return out
}

// VerifyDomain marks a domain as verified, only if owned by the given user.
func (s *Store) VerifyDomain(ctx context.Context, domain, userID string) error {
	return s.VerifyDomainTx(ctx, domain, userID, nil)
}

// VerifyDomainTx marks a domain verified and runs inTx before commit. Sender
// identity provisioning uses this hook to make its River job an atomic outbox:
// a verified row can never commit without the corresponding durable job.
func (s *Store) VerifyDomainTx(ctx context.Context, domain, userID string, inTx func(context.Context, pgx.Tx) error) error {
	domain = normalizeDomain(domain)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE domains SET verified = true, verified_at = now()
		 WHERE domain = $1 AND user_id = $2 AND verified = false`,
		domain, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("domain not found, not owned by user, or already verified")
	}

	// The now-verified child becomes the sending/verification identity for
	// inherited agents in its subtree. Only bindings to a less-specific
	// ancestor qualify; an agent already bound to a deeper child is untouched.
	if _, err := tx.Exec(ctx,
		`UPDATE agent_identities
		    SET registered_domain = $1
		  WHERE user_id = $2
		    AND (split_part(id, '@', 2) = $1
		         OR right(split_part(id, '@', 2), char_length($1) + 1) = '.' || $1)
		    AND right($1, char_length(registered_domain) + 1) = '.' || registered_domain`,
		domain, userID,
	); err != nil {
		return err
	}
	if inTx != nil {
		if err := inTx(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AgentCount is computed inline via a correlated subquery — one round-trip
// regardless of how many domains the page has, and the per-row count is
// supported by the existing user and registered-domain indexes. Excludes
// system rows.
//
// ListDomainsByUser returns one page of the user's domains, newest-first,
// keyset-paginated on (created_at, domain) — domain is the table's unique key,
// so it is the deterministic tiebreak. limit<=0 returns every domain
// unpaginated; a positive limit fetches that many (pass limit+1 to detect a
// further page) starting after the (afterCreatedAt, afterDomain) key from the
// previous page's last row (zero afterCreatedAt = first page).
func (s *Store) ListDomainsByUser(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterDomain string) ([]Domain, error) {
	q := `SELECT d.domain, d.user_id, d.verified, d.verification_token, d.created_at, d.verified_at,
	        d.is_primary, d.last_checked_at,
	        COALESCE(d.dkim_selector, ''), COALESCE(d.dkim_public_key, ''),
	        d.sending_status, COALESCE(d.sending_error, ''), d.sending_dns_records, d.sending_last_checked_at,
	        COALESCE(d.sending_dkim_status, ''), COALESCE(d.sending_mail_from_status, ''),
	        -- Trashed agents are excluded: a soft-deleted agent is not "on" the
	        -- domain from the caller's point of view (it does not appear in
	        -- list_agents), so counting it here over-reports.
	        (SELECT count(*) FROM agent_identities a
	           WHERE a.registered_domain = d.domain AND a.user_id = d.user_id
	             AND a.deleted_at IS NULL) AS agent_count
	 FROM domains d
	 WHERE d.user_id = $1`
	args := []interface{}{userID}
	if !afterCreatedAt.IsZero() {
		i := len(args) + 1
		q += fmt.Sprintf(` AND (d.created_at < $%d OR (d.created_at = $%d AND d.domain < $%d))`, i, i, i+1)
		args = append(args, afterCreatedAt, afterDomain)
	}
	q += ` ORDER BY d.created_at DESC, d.domain DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT $%d`, len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var domains []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.Domain, &d.UserID, &d.Verified, &d.VerificationToken, &d.CreatedAt, &d.VerifiedAt, &d.IsPrimary, &d.LastCheckedAt, &d.DKIMSelector, &d.DKIMPublicKey, &d.SendingStatus, &d.SendingError, &d.SendingDNSRecordsJSON, &d.SendingLastCheckedAt, &d.SendingDkimStatus, &d.SendingMailFromStatus, &d.AgentCount); err != nil {
			return nil, err
		}
		domains = append(domains, d)
	}
	return domains, rows.Err()
}

// TouchDomainLastChecked records that the verification probe ran. Call
// this from POST /v1/domains/{domain}/verify whether the probe
// succeeded or not — the LastCheckedAt column is "when did we last try",
// not "when did we last succeed" (the latter is verified_at).
func (s *Store) TouchDomainLastChecked(ctx context.Context, domain, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE domains SET last_checked_at = now() WHERE domain = $1 AND user_id = $2`,
		normalizeDomain(domain), userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDomainNotFound
	}
	return nil
}

// CountAgentsOnDomain reports how many agents still sit on the owned domain,
// split into LIVE and TRASHED. Both block the delete, and deliberately so: the
// FK agent_identities.registered_domain -> domains.domain is ON DELETE NO ACTION
// (migration 001), and a trashed agent is still a row. It also still owns its
// address for the 30-day restore window, so dropping the domain under it would
// break restore.
//
// The split exists purely so the caller can say WHICH kind is blocking. A
// trashed agent is invisible to list_agents, so "agents exist" alone sends
// someone hunting for agents they cannot see; naming the trash and the remedy
// turns a dead end into a signpost.
func (s *Store) CountAgentsOnDomain(ctx context.Context, domain, userID string) (live, trashed int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT
		   COUNT(*) FILTER (WHERE deleted_at IS NULL),
		   COUNT(*) FILTER (WHERE deleted_at IS NOT NULL)
		 FROM agent_identities WHERE registered_domain = $1 AND user_id = $2`,
		normalizeDomain(domain), userID,
	).Scan(&live, &trashed)
	if err != nil {
		return 0, 0, err
	}
	return live, trashed, nil
}

// ErrDomainHasAgents is returned when a domain delete is blocked by existing agents.
var ErrDomainHasAgents = fmt.Errorf("cannot delete domain: agents still exist")

// ErrDomainNotFound is returned when a domain is not found or not owned by the user.
var ErrDomainNotFound = fmt.Errorf("domain not found or not owned by user")

// ErrDomainTaken is returned by ClaimOrCreateDomain when the domain row exists
// and is owned by a different user (verified, or an unverified claim that must
// not be squatted). The API layer maps it to 409 conflict, distinct from the
// 400 used for malformed input.
var ErrDomainTaken = fmt.Errorf("domain not available: already claimed by another account")

// ErrReservedDomain is returned when a claim falls inside infrastructure e2a
// manages beneath an owned domain (currently the bounce.<domain> MAIL FROM
// subtree). The API maps it to reserved_domain.
var ErrReservedDomain = fmt.Errorf("domain is reserved for managed infrastructure")

// DomainLimitExceededError is returned by ClaimOrCreateDomainWithLimit when
// userID is already at maxDomains. Identity's own type, so this package does
// not need to import limits for the shared LimitExceededError shape.
type DomainLimitExceededError struct {
	Limit, Current int
}

func (e *DomainLimitExceededError) Error() string {
	return fmt.Sprintf("domain limit exceeded: %d/%d domains", e.Current, e.Limit)
}

// DeleteDomain deletes a domain only if owned by the user.
// The handler should check for existing agents first.
func (s *Store) DeleteDomain(ctx context.Context, domain, userID string) error {
	return s.DeleteDomainTx(ctx, domain, userID, nil, nil)
}

// DeleteDomainTx deletes a domain and, before committing, runs inTx within
// the SAME transaction. The hook is how sender-identity teardown is enqueued
// transactionally (decision 4): the River deprovision job is committed
// atomically with the domain-row delete, so it can never be lost if SES is
// unreachable at delete time. A nil hook is a plain delete (dev / no SES).
//
// inTx runs only after the DELETE affected a row (the domain existed and was
// owned by userID). onMissing, when non-nil, runs under the same domain locks
// when no live row exists; domain DELETE uses it to atomically bind a new
// idempotency key to an existing historical receipt. With no onMissing hook,
// a missing/cross-owner domain returns ErrDomainNotFound.
func (s *Store) DeleteDomainTx(ctx context.Context, domain, userID string, inTx func(ctx context.Context, tx pgx.Tx, incarnation string) error, onMissing func(ctx context.Context, tx pgx.Tx) error) error {
	domain = normalizeDomain(domain)
	tx, err := s.senderIdentityBegin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Serialize deletion with same-name and hierarchical registration. Without
	// these transaction locks, a same-owner ClaimOrCreateDomain can observe the
	// old row while this DELETE is uncommitted, return it as an idempotent
	// success, and then lose it when this transaction commits after provider
	// teardown. Use the exact sorted namespace shared by the claim path.
	for _, name := range domainClaimLockNames(domain) {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, name); err != nil {
			return err
		}
	}

	var incarnation string
	err = tx.QueryRow(ctx,
		`DELETE FROM domains WHERE domain = $1 AND user_id = $2 RETURNING verification_token`,
		domain, userID,
	).Scan(&incarnation)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// DELETE ... WHERE user_id cannot distinguish a globally absent row
			// from a same-name registration owned by another account. Historical
			// receipt polling is valid only for global absence; a live replacement
			// must remain an ordinary ownership-scoped 404. The advisory locks above
			// keep this check stable through the transaction.
			var live bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM domains WHERE domain = $1)`, domain,
			).Scan(&live); err != nil {
				return err
			}
			if live {
				return ErrDomainNotFound
			}
			if onMissing == nil {
				return ErrDomainNotFound
			}
			if err := onMissing(ctx, tx); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		if strings.Contains(err.Error(), "violates foreign key") {
			return ErrDomainHasAgents
		}
		return err
	}
	if inTx != nil {
		if err := inTx(ctx, tx, incarnation); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// BeginDomainTeardownReceiptTx persists the retry-visible outcome before the
// domain-delete transaction commits. A configured provider always starts
// pending until it confirms absence. With no provider configured, a domain
// that was never in the managed ledger is immediately confirmed; a ledgered
// identity stays pending until the provider is enabled and the reaper can
// prove absence.
func (s *Store) BeginDomainTeardownReceiptTx(ctx context.Context, tx pgx.Tx, domain, incarnation, userID string, providerConfigured bool) (domainteardown.Receipt, error) {
	domain = normalizeDomain(domain)
	state := domainteardown.Confirmed
	if providerConfigured {
		state = domainteardown.Pending
	} else {
		var managed bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM sender_identity_managed_domains WHERE domain = $1)`,
			domain,
		).Scan(&managed); err != nil {
			return domainteardown.Receipt{}, err
		}
		if managed {
			state = domainteardown.Pending
		}
	}
	// The provider identity is domain-global, not registration-scoped. Starting
	// teardown for a replacement registration therefore invalidates a prior
	// incarnation's confirmed DNS-release signal until this newest teardown
	// converges. Keep every historical keyed receipt fail-closed; the reaper's
	// SetDomainTeardownState advances them together after proving absence.
	if _, err := tx.Exec(ctx,
		`UPDATE domain_teardown_receipts
		 SET state = $2, updated_at = now()
		 WHERE domain = $1 AND state IS DISTINCT FROM $2`,
		domain, state,
	); err != nil {
		return domainteardown.Receipt{}, err
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO domain_teardown_receipts (domain, incarnation, user_id, state, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, now(), now())
		 ON CONFLICT (domain, incarnation) DO UPDATE
		 SET user_id = EXCLUDED.user_id,
		     state = EXCLUDED.state,
		     created_at = now(),
		     updated_at = now()`,
		domain, incarnation, userID, state,
	)
	return domainteardown.Receipt{Incarnation: incarnation, State: state}, err
}

// LookupDomainTeardownReceipt returns only the requesting owner's receipt.
// pgx.ErrNoRows deliberately preserves DELETE's anti-enumeration behavior for
// absent domains and receipts owned by another account.
func (s *Store) LookupDomainTeardownReceipt(ctx context.Context, domain, userID string) (domainteardown.State, error) {
	receipt, err := s.LookupDomainTeardownReceiptRecord(ctx, domain, userID)
	return receipt.State, err
}

// LookupDomainTeardownReceiptRecord returns the newest deletion receipt for
// an absent domain, including the incarnation needed to bind a later keyed
// poll to this deletion rather than a future same-name deletion.
func (s *Store) LookupDomainTeardownReceiptRecord(ctx context.Context, domain, userID string) (domainteardown.Receipt, error) {
	return lookupDomainTeardownReceiptRecord(ctx, s.pool, domain, userID)
}

// LookupDomainTeardownReceiptRecordTx is the transaction-bound form used when
// binding an idempotency key to an already-deleted incarnation. The caller
// holds the same advisory locks as registration/deletion, so a replacement
// cannot appear between receipt selection and key completion.
func (s *Store) LookupDomainTeardownReceiptRecordTx(ctx context.Context, tx pgx.Tx, domain, userID string) (domainteardown.Receipt, error) {
	return lookupDomainTeardownReceiptRecord(ctx, tx, domain, userID)
}

func lookupDomainTeardownReceiptRecord(ctx context.Context, q senderIdentityExecutor, domain, userID string) (domainteardown.Receipt, error) {
	var receipt domainteardown.Receipt
	err := q.QueryRow(ctx,
		`SELECT incarnation, state FROM domain_teardown_receipts
		 WHERE domain = $1 AND user_id = $2
		 ORDER BY receipt_id DESC LIMIT 1`,
		normalizeDomain(domain), userID,
	).Scan(&receipt.Incarnation, &receipt.State)
	return receipt, err
}

// LookupDomainTeardownReceiptForIncarnation follows one historical deletion.
// It is the safe polling path for a keyed retry after the DNS name has been
// registered again: the lookup cannot drift onto the replacement receipt.
func (s *Store) LookupDomainTeardownReceiptForIncarnation(ctx context.Context, domain, incarnation, userID string) (domainteardown.State, error) {
	var state domainteardown.State
	err := s.pool.QueryRow(ctx,
		`SELECT state FROM domain_teardown_receipts
		 WHERE domain = $1 AND incarnation = $2 AND user_id = $3`,
		normalizeDomain(domain), incarnation, userID,
	).Scan(&state)
	return state, err
}

// LookupDomainTeardownSnapshot returns an exact historical receipt together
// with whether the DNS name is currently registered, from one PostgreSQL
// statement snapshot. Keeping these reads together is load-bearing: a newer
// deletion resets old receipts to pending while removing the replacement row,
// and two separate statements could otherwise observe confirmed before that
// commit and absent after it.
func (s *Store) LookupDomainTeardownSnapshot(ctx context.Context, domain, incarnation, userID string) (domainteardown.State, bool, error) {
	var (
		state domainteardown.State
		live  bool
	)
	err := s.pool.QueryRow(ctx,
		`SELECT r.state,
		        EXISTS (SELECT 1 FROM domains d WHERE d.domain = $1)
		 FROM domain_teardown_receipts r
		 WHERE r.domain = $1 AND r.incarnation = $2 AND r.user_id = $3`,
		normalizeDomain(domain), incarnation, userID,
	).Scan(&state, &live)
	return state, live, err
}

// SetDomainTeardownState advances a receipt after provider convergence. It is
// intentionally a no-op when no domain-delete receipt exists (for example an
// account delete, whose user and receipts cascade together).
func (s *Store) SetDomainTeardownState(ctx context.Context, domain string, state domainteardown.State) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE domain_teardown_receipts SET state = $2, updated_at = now() WHERE domain = $1`,
		normalizeDomain(domain), state,
	)
	return err
}

// --- Agent CRUD ---

// GetAgentByID looks up an agent by its ID (full email) with domain verification status.
func (s *Store) GetAgentByID(ctx context.Context, id string) (*AgentIdentity, error) {
	return s.getAgentByID(ctx, id, false)
}

// GetAgentByIDAnyState loads an agent regardless of trash state (deleted_at
// populated when trashed). For the trash surfaces only — restore, permanent
// delete, trash listing detail. Every live path uses GetAgentByID, which
// treats a trashed agent as nonexistent.
func (s *Store) GetAgentByIDAnyState(ctx context.Context, id string) (*AgentIdentity, error) {
	return s.getAgentByID(ctx, id, true)
}

func (s *Store) getAgentByID(ctx context.Context, id string, includeDeleted bool) (*AgentIdentity, error) {
	return loadAgentByID(ctx, s.pool, id, includeDeleted)
}

// agentRowQuerier is the read half of *pgxpool.Pool and pgx.Tx. It exists so
// loadAgentByID can serve both the stand-alone getters and the write paths
// that must re-read the agent INSIDE their own transaction.
type agentRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// loadAgentByID is the one agent projection, parameterized by executor. The
// domain JOIN (domain_verified lives on `domains`, not `agent_identities`) is
// why the agent write paths cannot answer with a plain UPDATE … RETURNING, and
// call this inside their own transaction instead.
func loadAgentByID(ctx context.Context, exec agentRowQuerier, id string, includeDeleted bool) (*AgentIdentity, error) {
	q := `SELECT a.id, a.registered_domain, a.user_id, a.name, a.public, a.created_at,
		        a.hitl_ttl_seconds, a.hitl_expiration_action, a.suppress_notifications,
		        COALESCE(a.inbound_policy, 'open'), a.inbound_allowlist,
		        a.inbound_policy_action,
		        a.outbound_policy, a.outbound_allowlist, a.outbound_policy_action,
		        a.inbound_scan, a.inbound_scan_review_threshold, a.inbound_scan_block_threshold,
		        a.outbound_scan, a.outbound_scan_review_threshold, a.outbound_scan_block_threshold,
		        a.inbound_scan_sensitivity, a.outbound_scan_sensitivity,
		        COALESCE(a.assertion_version, 1),
		        a.deleted_at,
		        d.verified as domain_verified
		 FROM agent_identities a
		 JOIN domains d ON a.registered_domain = d.domain
		 WHERE a.id = $1`
	if !includeDeleted {
		q += ` AND a.deleted_at IS NULL`
	}
	a := &AgentIdentity{}
	err := exec.QueryRow(ctx, q, id).Scan(&a.ID, &a.RegisteredDomain, &a.UserID, &a.Name, &a.Public, &a.CreatedAt,
		&a.HITLTTLSeconds, &a.HITLExpirationAction, &a.SuppressNotifications,
		&a.InboundPolicy, &a.InboundAllowlist,
		&a.InboundPolicyAction,
		&a.OutboundPolicy, &a.OutboundAllowlist, &a.OutboundPolicyAction,
		&a.InboundScan, &a.InboundScanReviewThreshold, &a.InboundScanBlockThreshold,
		&a.OutboundScan, &a.OutboundScanReviewThreshold, &a.OutboundScanBlockThreshold,
		&a.InboundScanSensitivity, &a.OutboundScanSensitivity,
		&a.AssertionVersion,
		&a.DeletedAt,
		&a.DomainVerified)
	if err != nil {
		return nil, err
	}
	a.populateEmail()
	return a, nil
}

// GetAgentByEmail looks up an agent by email address (same as GetAgentByID since ID = email).
func (s *Store) GetAgentByEmail(ctx context.Context, email string) (*AgentIdentity, error) {
	return s.GetAgentByID(ctx, email)
}

// CreateAgent inserts an agent with a domain FK. Does NOT check domain ownership —
// that's the API handler's responsibility (shared domain skips the check).
//
// webhookURL and agentMode are accepted for signature compatibility but are
// now IGNORED: the legacy per-agent webhook_url + agent_mode columns were
// dropped (migration 029). Push is delivered solely via the /v1/webhooks
// subscriber resource and WebSocket is open to all agents. The params are
// retained to avoid churning the ~80 call-sites that still pass them; the
// internal-signature cleanup is a separate follow-up.
func (s *Store) CreateAgent(ctx context.Context, agentEmail, domain, name, webhookURL, agentMode, userID string) (*AgentIdentity, error) {
	return createAgent(ctx, s.pool, agentEmail, domain, name, userID)
}

// CreateAgentTx inserts an agent inside a caller-owned transaction.
// Used by the OAuth consent flow so the slug auto-create row and the
// authorization-code insert (in oauth_auth_codes) commit together —
// without this, a code-issue failure after the agent commit would
// leave a phantom inbox the user never authorized.
// webhookURL and agentMode are accepted but IGNORED — see CreateAgent.
func (s *Store) CreateAgentTx(ctx context.Context, tx pgx.Tx, agentEmail, domain, name, webhookURL, agentMode, userID string) (*AgentIdentity, error) {
	return createAgent(ctx, tx, agentEmail, domain, name, userID)
}

// AgentLimitExceededError is returned by CreateAgentWithLimit when userID is
// already at maxAgents. Mirrors DomainLimitExceededError.
type AgentLimitExceededError struct {
	Limit, Current int
}

func (e *AgentLimitExceededError) Error() string {
	return fmt.Sprintf("agent limit exceeded: %d/%d agents", e.Current, e.Limit)
}

// CreateAgentWithLimit is CreateAgent plus an atomic max_agents check
// (keyspace-2 advisory lock) inside the INSERT's transaction, closing the
// pre-insert-count race (#822's class). maxAgents <= 0 means unlimited.
func (s *Store) CreateAgentWithLimit(ctx context.Context, agentEmail, domain, name, userID string, maxAgents int) (*AgentIdentity, error) {
	if maxAgents <= 0 {
		return createAgent(ctx, s.pool, agentEmail, domain, name, userID)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	a, err := s.CreateAgentWithLimitTx(ctx, tx, agentEmail, domain, name, userID, maxAgents)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// CreateAgentWithLimitTx is CreateAgentWithLimit for a caller-owned
// transaction: the advisory lock, count check and INSERT all run on tx,
// and the caller commits (or rolls back). Used by the OAuth auto-provision
// path so the cap check and the authorization-code insert (in
// oauth_auth_codes) commit or roll back together, the same reason
// CreateAgentTx exists alongside CreateAgent. maxAgents <= 0 means
// unlimited (no lock taken, matching CreateAgentWithLimit).
func (s *Store) CreateAgentWithLimitTx(ctx context.Context, tx pgx.Tx, agentEmail, domain, name, userID string, maxAgents int) (*AgentIdentity, error) {
	if maxAgents <= 0 {
		return createAgent(ctx, tx, agentEmail, domain, name, userID)
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 2))`, userID); err != nil {
		return nil, err
	}

	// Read under the per-user lock, so this reflects every insert already
	// committed by a concurrent request, not a stale count. Mirrors
	// usage.Store.CountAgentsByUser's predicate; keep the two in sync.
	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*)
		   FROM agent_identities a
		   JOIN domains d ON a.registered_domain = d.domain
		  WHERE a.user_id = $1 AND a.deleted_at IS NULL`,
		userID,
	).Scan(&count); err != nil {
		return nil, err
	}
	if count >= maxAgents {
		return nil, &AgentLimitExceededError{Limit: maxAgents, Current: count}
	}

	return createAgent(ctx, tx, agentEmail, domain, name, userID)
}

// agentExecutor is the subset of pgxpool.Pool + pgx.Tx that
// createAgent needs. Lets the same body serve both stand-alone and
// in-transaction callers without duplicating the SQL.
type agentExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func createAgent(ctx context.Context, exec agentExecutor, agentEmail, domain, name, userID string) (*AgentIdentity, error) {
	// Same display-name bound as UpdateAgentName (one shared constant +
	// check), so create and update can never disagree on the cap.
	if err := ValidateAgentName(name); err != nil {
		return nil, err
	}
	a := &AgentIdentity{
		ID:                   NormalizeEmail(agentEmail),
		RegisteredDomain:     normalizeDomain(domain),
		Name:                 name,
		Public:               true,
		CreatedAt:            time.Now(),
		UserID:               userID,
		HITLTTLSeconds:       HITLDefaultTTLSeconds,
		HITLExpirationAction: HITLDefaultExpirationAct,
	}
	// Report the same domain_verified the read paths (GetAgentByID /
	// ListAgentsByUser) derive from domains.verified. createAgent builds the
	// identity in-memory from only the INSERT columns, so DomainVerified would
	// otherwise be the Go zero value (false) regardless of the domain's real
	// state — wrong for an agent on a verified domain, and it would flip to the
	// correct value on the next GET.
	//
	// The scalar subquery in RETURNING folds the read back into the INSERT: one
	// round-trip, and no post-commit window (a separate SELECT after the INSERT
	// auto-commits on the pool path would, if it errored transiently, surface a
	// 500 even though the agent row is already committed — a retry then 409s).
	// The FK on agent_identities.registered_domain guarantees the domains row exists and is
	// visible here, so the subquery always resolves to a non-NULL bool. Works
	// identically on the pool and inside a caller-owned tx.
	if err := exec.QueryRow(ctx,
		`INSERT INTO agent_identities (id, registered_domain, user_id, name, public, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING (SELECT verified FROM domains WHERE domain = $2)`,
		a.ID, a.RegisteredDomain, a.UserID, a.Name, a.Public, a.CreatedAt,
	).Scan(&a.DomainVerified); err != nil {
		return nil, err
	}
	a.populateEmail()
	return a, nil
}

// UpdateAgentHITL updates all three HITL settings on an agent owned by userID.
// The TTL and expiration action are validated against the same rules as the
// DB CHECK constraints so callers get a clean error rather than a raw SQL error.
func (s *Store) UpdateAgentHITL(ctx context.Context, agentID, userID string, ttlSeconds int, expirationAction string) error {
	if err := ValidateHITLConfig(ttlSeconds, expirationAction); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE agent_identities
		    SET hitl_ttl_seconds = $1,
		        hitl_expiration_action = $2
		  WHERE id = $3 AND user_id = $4`,
		ttlSeconds, expirationAction, agentID, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Sentinel, not a fresh error with the same text: handlers map this
		// to 404 (a lost race against the caller's own agent) rather than a
		// 500, and they can only do that if it's matchable with errors.Is.
		return ErrAgentNotFound
	}
	return nil
}

// UpdateAgentInboundPolicy sets the inbound ingestion gate (migration 033 /
// Slice 7) on an agent owned by userID. The policy is validated against
// inboundpolicy.Valid so callers get a clean error rather than a raw CHECK
// violation. allowlist may be empty (the gate then flags everything for the
// gating postures — fail-closed). Returns an error if the agent isn't found
// or isn't owned by the user.
// maxInboundAllowlist bounds the per-agent inbound_allowlist. The relay scans
// it linearly on every inbound message, so an unbounded list is an owner-scoped
// DoS vector; 1000 entries is far beyond any real allow/deny need.
const maxInboundAllowlist = 1000

func (s *Store) UpdateAgentInboundPolicy(ctx context.Context, agentID, userID, policy string, allowlist []string) error {
	if !inboundpolicy.Valid(policy) {
		return fmt.Errorf("invalid inbound_policy %q", policy)
	}
	if len(allowlist) > maxInboundAllowlist {
		return fmt.Errorf("inbound_allowlist has %d entries, max %d", len(allowlist), maxInboundAllowlist)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE agent_identities
		    SET inbound_policy = $3,
		        inbound_allowlist = $4
		  WHERE id = $1 AND user_id = $2`,
		agentID, userID, policy, allowlist,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("agent not found or not owned by user")
	}
	return nil
}

// MaxAgentNameLen bounds the agent display name (a UI label, not an
// identifier). It is the single source of truth shared by the create and
// update paths: the /v1 request schemas declare it as maxLength (validated by
// Huma in Unicode code points) and ValidateAgentName enforces the same
// rune-count semantics here, so the spec and the runtime always agree.
const MaxAgentNameLen = 200

// MaxAPIKeyNameLen bounds the API-key display name (a human label, not an
// identifier). One source of truth for every entry path: the /v1 createApiKey
// schema tag AND the legacy dashboard POST /api/keys handler (internal/auth).
// Counted in Unicode code points, matching OpenAPI maxLength semantics.
const MaxAPIKeyNameLen = 200

// MaxRejectReasonLen bounds a reviewer-supplied rejection reason. One source
// of truth for every entry path: the /v1 rejectReview schema tag AND the
// magic-link reject form (internal/agent), which clamps rather than fails a
// human's rejection. Counted in Unicode code points.
const MaxRejectReasonLen = 2000

// ValidateAgentName checks the display-name bound. The length is counted in
// Unicode code points (runes), NOT bytes, to match the OpenAPI maxLength
// semantics of the /v1 request schemas (JSON Schema counts code points).
func ValidateAgentName(name string) error {
	if n := utf8.RuneCountInString(name); n > MaxAgentNameLen {
		return fmt.Errorf("name has %d characters, max %d", n, MaxAgentNameLen)
	}
	return nil
}

// UpdateAgentName sets an agent's display name for an agent owned by userID
// and returns the agent AS WRITTEN. The name is a UI label only — the agent's
// identity is its email. Returns an error if the agent isn't found or not
// owned.
//
// The read-back happens INSIDE the write's transaction. Reading afterwards
// from the pool was a torn read: the UPDATE takes the row lock, but once it
// commits a concurrent rename could land before the re-read and the caller saw
// the OTHER writer's name echoed back as the result of their own PATCH, while
// a concurrent trash/delete turned a committed rename into a 500 "failed to
// reload agent". Same recipe as UpdateEngagementIfUnchanged. The re-read
// includes trashed rows on purpose: the row it reports is the one this
// statement wrote, and AgentView carries deleted_at, so a racing trash is
// reported honestly instead of as a server error.
func (s *Store) UpdateAgentName(ctx context.Context, agentID, userID, name string) (*AgentIdentity, error) {
	if err := ValidateAgentName(name); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE agent_identities SET name = $3 WHERE id = $1 AND user_id = $2`,
		agentID, userID, name,
	)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("agent not found or not owned by user")
	}
	a, err := loadAgentByID(ctx, tx, agentID, true)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// Agents are joined with domain verification AND enriched with per-agent stats
// for the dashboard. Correlated subqueries compute inbound/outbound 7-day
// counts, pending approvals, last delivery, and webhook status in a single
// round-trip. Other load paths (GetAgentByID, GetAgentByEmail) intentionally
// don't compute these — only the dashboard and the account export need them.
//
// ListAgentsByUser returns one page of the user's agents, newest-first,
// keyset-paginated on (created_at, id). limit<=0 returns every agent
// unpaginated (the all-consumers: auth dashboard views + webhook filter
// ownership validation); a positive limit fetches that many (pass limit+1 to
// detect a further page) starting after the (afterCreatedAt, afterID) key from
// the previous page's last row (zero afterCreatedAt = first page).
func (s *Store) ListAgentsByUser(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]AgentIdentity, error) {
	return s.listAgentsByUser(ctx, userID, limit, afterCreatedAt, afterID, false)
}

// ListDeletedAgentsByUser is the trash listing: agents the user soft-deleted,
// newest-first, same keyset pagination as the live list. DeletedAt is
// populated on every row.
func (s *Store) ListDeletedAgentsByUser(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]AgentIdentity, error) {
	return s.listAgentsByUser(ctx, userID, limit, afterCreatedAt, afterID, true)
}

func (s *Store) listAgentsByUser(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string, deleted bool) ([]AgentIdentity, error) {
	// The per-agent activity stats exclude trashed messages (deleted_at IS
	// NOT NULL) — the dashboard's 7-day counters and last-delivery must agree
	// with what the inbox actually shows. For the TRASH listing (deleted=
	// true) the stats are skipped entirely below: the trash view renders
	// identity fields only, and five correlated probes per trashed agent
	// against the prod-sized messages table would be pure waste.
	q := `SELECT a.id, a.registered_domain, a.user_id, a.name, a.public, a.created_at, a.deleted_at,
		        a.hitl_ttl_seconds, a.hitl_expiration_action, a.suppress_notifications,
		        COALESCE(a.inbound_policy, 'open'), a.inbound_allowlist,
		        a.inbound_policy_action,
		        a.outbound_policy, a.outbound_allowlist, a.outbound_policy_action,
		        a.inbound_scan, a.inbound_scan_review_threshold, a.inbound_scan_block_threshold,
		        a.outbound_scan, a.outbound_scan_review_threshold, a.outbound_scan_block_threshold,
		        a.inbound_scan_sensitivity, a.outbound_scan_sensitivity,
		        d.verified as domain_verified,`
	if deleted {
		// Trash view: identity fields only — zero-value the stats columns so
		// the scan shape stays uniform without paying five correlated probes
		// against the prod-sized messages table per trashed agent (the trash
		// UI renders none of them).
		q += `
		        0 AS inbound_7d, 0 AS outbound_7d, 0 AS pending_count,
		        NULL::timestamptz AS last_delivery_at, ''::text AS webhook_status`
	} else {
		// Live stats exclude trashed messages so the dashboard counters agree
		// with what the inbox shows.
		q += `
		        (SELECT count(*) FROM messages m
		           WHERE m.agent_id = a.id AND m.direction = 'inbound'
		             AND m.deleted_at IS NULL
		             AND m.created_at > now() - interval '7 days') AS inbound_7d,
		        (SELECT count(*) FROM messages m
		           WHERE m.agent_id = a.id AND m.direction = 'outbound'
		             AND m.deleted_at IS NULL
		             AND m.created_at > now() - interval '7 days') AS outbound_7d,
		        (SELECT count(*) FROM messages m
		           WHERE m.agent_id = a.id AND m.status = 'pending_review' AND m.direction = 'outbound') AS pending_count,
		        (SELECT max(m.created_at) FROM messages m
		           WHERE m.agent_id = a.id AND m.direction = 'outbound'
		             AND m.deleted_at IS NULL
		             AND m.status = 'sent') AS last_delivery_at,
		        ` + webhookStatusSQL + ` AS webhook_status`
	}
	q += `
		 FROM agent_identities a
		 JOIN domains d ON a.registered_domain = d.domain
		 WHERE a.user_id = $1`
	if deleted {
		q += ` AND a.deleted_at IS NOT NULL`
	} else {
		q += ` AND a.deleted_at IS NULL`
	}
	args := []interface{}{userID}
	if !afterCreatedAt.IsZero() {
		i := len(args) + 1
		q += fmt.Sprintf(` AND (a.created_at < $%d OR (a.created_at = $%d AND a.id < $%d))`, i, i, i+1)
		args = append(args, afterCreatedAt, afterID)
	}
	q += ` ORDER BY a.created_at DESC, a.id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT $%d`, len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []AgentIdentity
	for rows.Next() {
		var a AgentIdentity
		var lastDeliveryAt *time.Time
		if err := rows.Scan(&a.ID, &a.RegisteredDomain, &a.UserID, &a.Name, &a.Public, &a.CreatedAt, &a.DeletedAt,
			&a.HITLTTLSeconds, &a.HITLExpirationAction, &a.SuppressNotifications,
			&a.InboundPolicy, &a.InboundAllowlist,
			&a.InboundPolicyAction,
			&a.OutboundPolicy, &a.OutboundAllowlist, &a.OutboundPolicyAction,
			&a.InboundScan, &a.InboundScanReviewThreshold, &a.InboundScanBlockThreshold,
			&a.OutboundScan, &a.OutboundScanReviewThreshold, &a.OutboundScanBlockThreshold,
			&a.InboundScanSensitivity, &a.OutboundScanSensitivity,
			&a.DomainVerified,
			&a.Inbound7d, &a.Outbound7d, &a.PendingCount,
			&lastDeliveryAt, &a.WebhookStatus); err != nil {
			return nil, err
		}
		a.LastDeliveryAt = lastDeliveryAt
		a.populateEmail()
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// DeleteAgent permanently removes the exact agent incarnation visible when
// the call starts. The shape decision is made under its row lock. Small work
// remains atomic; larger work receives a durable purge token and is drained in
// bounded committed transactions before this synchronous call returns.
func (s *Store) DeleteAgent(ctx context.Context, agentID, userID string) (messagesDeleted int64, err error) {
	var createdAt time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT created_at FROM agent_identities WHERE id = $1 AND user_id = $2`,
		agentID, userID).Scan(&createdAt); errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrAgentNotFound
	} else if err != nil {
		return 0, err
	}
	return s.DeleteAgentIncarnation(ctx, agentID, userID, createdAt)
}

// DeleteAgentIncarnation permanently deletes only the incarnation previously
// resolved by the caller. Carrying createdAt across the handler/store boundary
// prevents a delayed request from attaching to a same-owner recreation at the
// same address before any purge token has been claimed.
func (s *Store) DeleteAgentIncarnation(ctx context.Context, agentID, userID string, createdAt time.Time) (messagesDeleted int64, err error) {
	var token string
	var chunked bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		var decisionErr error
		token, chunked, decisionErr = s.agentPurgeDecisionTx(ctx, tx, agentID, userID, createdAt)
		if decisionErr != nil || chunked {
			return decisionErr
		}
		var deleteErr error
		messagesDeleted, deleteErr = s.deleteAgentAtomicTx(ctx, tx, agentID, userID)
		return deleteErr
	})
	if err != nil || !chunked {
		return messagesDeleted, err
	}
	return s.purgeAgentChunked(ctx, agentID, userID, token)
}

// SoftDeleteAgent moves a live agent to the trash (docs/design/
// trash-soft-delete.md): it disappears from every live lookup — inbound mail
// bounces, per-agent API calls 404, its held messages leave the review
// queue — until restored or purged by the janitor after TrashRetention.
// The email address stays reserved while in the trash.
func (s *Store) SoftDeleteAgent(ctx context.Context, agentID, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE agent_identities SET deleted_at = now()
		  WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`, agentID, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAgentNotFound
	}
	return nil
}

// RestoreAgent brings a trashed agent back to life, messages and config
// intact. Live messages never expire; for still-held drafts only
// approval_expires_at is shifted forward by the time spent in the trash so a
// hold cannot silently lapse while the inbox is trashed.
// Returns ErrNotInTrash when the agent exists but is live, and ErrAgentNotFound
// when it doesn't exist (or isn't the caller's).
//
// It returns the restored agent, read INSIDE the restore transaction. Reading
// afterwards from the pool was a torn read: a concurrent rename could land
// between the commit and the re-read (the response then showed the other
// writer's name), and a concurrent re-trash made a committed restore answer
// 500 "failed to reload agent". The in-transaction read follows the row lock
// this transaction already holds, so it always describes this restore.
func (s *Store) RestoreAgent(ctx context.Context, agentID, userID string) (*AgentIdentity, error) {
	var restored *AgentIdentity
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var (
			deletedAt  *time.Time
			purgeToken *string
		)
		err := tx.QueryRow(ctx,
			`SELECT deleted_at, purge_token FROM agent_identities
			  WHERE id = $1 AND user_id = $2 FOR UPDATE`, agentID, userID,
		).Scan(&deletedAt, &purgeToken)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentNotFound
		}
		if err != nil {
			return err
		}
		if deletedAt == nil {
			return ErrNotInTrash
		}
		if purgeToken != nil {
			return ErrPurgeInProgress
		}
		rows, err := tx.Query(ctx,
			`SELECT id, send_job_id, scheduled_at
			   FROM messages
			  WHERE agent_id = $1
			    AND deleted_at IS NULL
			    AND direction = 'outbound'
			    AND delivery_status = 'accepted'
			    AND scheduled_at IS NOT NULL
			    AND send_job_id IS NOT NULL
			  FOR UPDATE`,
			agentID)
		if err != nil {
			return err
		}
		type lockedSchedule struct {
			job         pastDueScheduledJob
			scheduledAt time.Time
		}
		var locked []lockedSchedule
		for rows.Next() {
			var schedule lockedSchedule
			if err := rows.Scan(&schedule.job.messageID, &schedule.job.jobID, &schedule.scheduledAt); err != nil {
				rows.Close()
				return err
			}
			locked = append(locked, schedule)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		var cutoff time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
			return err
		}
		var pastDue []pastDueScheduledJob
		for _, schedule := range locked {
			if !schedule.scheduledAt.After(cutoff) {
				pastDue = append(pastDue, schedule.job)
			}
		}
		if err := s.cancelPastDueScheduledJobsTx(ctx, tx, pastDue); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE agent_identities SET deleted_at = NULL WHERE id = $1`, agentID); err != nil {
			return err
		}
		// Past-due outreach remains visible to the agent's pull query, but a
		// restore must not enqueue every wake-up that accumulated while the
		// inbox was intentionally inactive. A later reschedule changes
		// next_action_at and naturally re-arms contact.due.
		if _, err := tx.Exec(ctx,
			`UPDATE contact_engagements
			    SET notified_next_action_at = next_action_at,
			        updated_at = `+monotonicUpdatedAt("")+`
			  WHERE user_id = $1
			    AND agent_id = $2
			    AND next_action_at IS NOT NULL
			    AND next_action_at <= now()
			    AND notified_next_action_at IS DISTINCT FROM next_action_at`,
			userID, agentID); err != nil {
			return err
		}
		// Give back the trash time to pending holds on the agent's LIVE
		// messages only. Message retention itself is indefinite.
		if _, err := tx.Exec(ctx,
			`UPDATE messages
			    SET approval_expires_at = CASE
			          WHEN status = 'pending_review' AND approval_expires_at IS NOT NULL
			          THEN approval_expires_at + (now() - $2::timestamptz)
			          ELSE approval_expires_at END
			  WHERE agent_id = $1 AND deleted_at IS NULL`, agentID, *deletedAt); err != nil {
			return err
		}
		// The LIVE projection: this transaction just cleared deleted_at, so a
		// row is guaranteed, and finding one still proves the agent is visible
		// again — the property the handler's post-restore read used to assert.
		restored, err = loadAgentByID(ctx, tx, agentID, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return restored, nil
}

// agentPurgeBatch bounds one janitor pass of PurgeDeletedAgents: one agent
// per transaction, so the messages drained with it are bounded by that one
// inbox rather than a whole batch's worth of cascades.
var agentPurgeBatch = 100

// PurgeDeletedAgents claims and resumes the same bounded purge state machine
// as explicit permanent deletion. A claimed partial purge is eligible
// immediately; ordinary trash becomes eligible after TrashRetention.
func (s *Store) PurgeDeletedAgents(ctx context.Context) (int64, error) {
	var total int64
	attempted := make([]string, 0)
	var sweepErrs []error
	for i := 0; i < agentPurgeBatch; i++ {
		var (
			agentID, userID, token string
			found                  bool
		)
		err := s.WithTx(ctx, func(tx pgx.Tx) error {
			var purgeToken *string
			err := tx.QueryRow(ctx,
				`SELECT id, user_id, purge_token FROM agent_identities
				  WHERE (purge_token IS NOT NULL
				     OR (deleted_at IS NOT NULL AND deleted_at <= now() - make_interval(secs => $1)))
				    AND NOT (id = ANY($2::text[]))
				  LIMIT 1 FOR UPDATE SKIP LOCKED`,
				TrashRetention.Seconds(), attempted).Scan(&agentID, &userID, &purgeToken)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := ensureNoAgentSendInProgressTx(ctx, tx, agentID); err != nil {
				return err
			}
			if purgeToken == nil {
				token = "pur_" + generateID()
				_, err = tx.Exec(ctx,
					`UPDATE agent_identities
					    SET deleted_at = now(), purge_token = $2
					  WHERE id = $1`,
					agentID, token)
				if err != nil {
					return err
				}
			} else {
				token = *purgeToken
			}
			found = true
			return nil
		})
		if agentID != "" {
			attempted = append(attempted, agentID)
		}
		if err != nil {
			sweepErrs = append(sweepErrs, err)
			if ctx.Err() != nil {
				return total, errors.Join(sweepErrs...)
			}
			continue
		}
		if !found {
			return total, errors.Join(sweepErrs...)
		}
		_, completed, err := s.drainAgentChunksResult(ctx, agentID, userID, token)
		if err != nil {
			sweepErrs = append(sweepErrs, err)
			if ctx.Err() != nil {
				return total, errors.Join(sweepErrs...)
			}
			continue
		}
		if completed {
			total++
		}
	}
	return total, errors.Join(sweepErrs...)
}

// --- Messages ---

// TrashRetention is how long a soft-deleted resource (agent inbox or
// message) stays in the trash before the janitor purges it permanently —
// the Gmail-style 30-day window (docs/design/trash-soft-delete.md). A var
// (not a const): cmd/e2a/main.go assigns it at startup from the deployment config
// (trash.retention_days / E2A_TRASH_RETENTION_DAYS, validated ≥1 day), and
// tests may tune it directly. Default 30 days — the number the stable API
// contract documents ("30 days by default, deployment-configurable").
// Every consumer reads it at query time, so the startup assignment (before
// any janitor/worker starts) governs the whole process.
var TrashRetention = 30 * 24 * time.Hour

// ErrNotInTrash is returned by restore/purge operations that target a
// resource that exists but is not soft-deleted.
var ErrNotInTrash = fmt.Errorf("resource is not in the trash")

// ErrPurgeInProgress is returned when restore targets an agent whose
// irreversible permanent purge has already committed its durable claim.
var ErrPurgeInProgress = fmt.Errorf("permanent purge is already in progress")

// ErrMessageHeld is returned when a trash operation targets a message that
// is held for review (status pending_review) — the review queue is its
// resolution surface; approve or reject it first.
var ErrMessageHeld = fmt.Errorf("message is held for review")

// ErrAgentNotFound is returned by agent trash operations (soft delete /
// restore / permanent delete) when the agent row isn't there for the caller
// — either it never existed, belongs to another user, or was hard-deleted
// between resolution and the mutation (a race the handler maps to 404
// instead of a generic 500). Mirrors ErrMessageNotFound / ErrDomainNotFound.
var ErrAgentNotFound = fmt.Errorf("agent not found or not owned by user")

// NewMessageID returns a fresh internal message ID. Callers can use this
// to generate the ID up-front when they need it before storing — for
// example, the SMTP relay generates the ID before signing auth headers
// so the ID is part of the canonical string fed to HMAC.
func NewMessageID() string {
	return "msg_" + generateID()
}

// NewConversationID returns a fresh application conversation/grouping ID. An
// outbound send that omits conversation_id gets one minted here so external
// replies can recover the grouping from the referenced outbound message.
// Email reply topology is tracked independently by thread_id and RFC headers.
func NewConversationID() string {
	return "conv_" + generateID()
}

// NewThreadID returns an opaque mailbox-local email thread identifier. The
// random suffix matches message and conversation IDs: 128 bits rendered as
// lowercase hexadecimal.
func NewThreadID() string {
	return "thr_" + generateID()
}

// IsValidThreadID reports whether id has the only persisted thread-ID shape
// emitted by e2a. Thread IDs are server-owned and are never accepted from API
// callers.
func IsValidThreadID(id string) bool {
	if len(id) != len("thr_")+32 || !strings.HasPrefix(id, "thr_") {
		return false
	}
	for _, c := range id[len("thr_"):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CreateInboundMessage stores an inbound message. If id is empty a new
// one is generated; otherwise the caller's pre-generated ID is used so
// the upstream signer can bind auth headers to the same ID that gets
// stored. toRecipients and cc are the parsed To: and Cc: headers from
// the original RFC 2822 message; recipient is the per-delivery target
// for this row (may be one of the To: addresses, or absent from the
// header list when the agent was Bcc'd). replyTo is the parsed Reply-To:
// header (empty when absent — never silently falls back to sender).
func (s *Store) CreateInboundMessage(ctx context.Context, id, agentID, senderEmail, recipient, emailMessageID, subject, conversationID, deliveryStatus string, rawMessage []byte, authHeaders map[string]string, authVerdict []byte, flagged bool, flagReason string, toRecipients, cc, replyTo []string, screening InboundScreening) (*Message, error) {
	assignment := freshInboundMessageThread(emailMessageID)
	assignment.resolutionSource = "no_anchor"
	message, err := createInboundMessage(ctx, s.pool, assignment, id, agentID, senderEmail, recipient, emailMessageID, subject, conversationID, deliveryStatus, rawMessage, authHeaders, authVerdict, flagged, flagReason, toRecipients, cc, replyTo, screening, nil)
	if err == nil {
		s.recordThreadAssignment(assignment)
	}
	return message, err
}

// CreateInboundMessageAuthenticated stores the canonical identity and
// authentication model. The legacy CreateInboundMessage remains temporarily
// available while older tests and relay call sites migrate.
func (s *Store) CreateInboundMessageAuthenticated(ctx context.Context, id, agentID string, auth InboundAuth, recipient, emailMessageID, subject, conversationID, deliveryStatus string, rawMessage []byte, flagged bool, flagReason string, toRecipients, cc, replyTo []string, screening InboundScreening) (*Message, error) {
	assignment := freshInboundMessageThread(emailMessageID)
	assignment.resolutionSource = "no_anchor"
	message, err := createInboundMessage(ctx, s.pool, assignment, id, agentID, storedInboundSender(auth), recipient, emailMessageID, subject, conversationID, deliveryStatus, rawMessage, nil, nil, flagged, flagReason, toRecipients, cc, replyTo, screening, &auth)
	if err == nil {
		s.recordThreadAssignment(assignment)
	}
	return message, err
}

// WithTx opens a transaction, runs fn inside it, and commits if fn
// returns nil (or rolls back if fn returns an error). Used by the
// slice-3 relay refactor so the messages INSERT and the
// webhook_events outbox INSERT commit together, closing the
// at-least-once publish-loss window.
//
// The relay handler is the primary v1 caller; future trigger sites
// (slice 4 outbound + HITL) reuse the same helper. Keeps callers from
// having to import pgxpool directly. Store-owned in-memory side effects may
// register on the wrapped transaction and run only after a successful commit.
func (s *Store) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	rawTx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	tx := newPostCommitTx(rawTx, nil)
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateInboundMessageInTx writes the messages row inside the caller's
// transaction. Used by the slice-3 relay refactor (per design §4.2) so
// the messages INSERT and the webhook_events outbox INSERT commit
// together, closing the at-least-once publish-loss window.
//
// Mirrors the CreateAgentTx pattern at store.go:596-607 — same SQL
// body, executed against either *pgxpool.Pool or pgx.Tx via the
// messageExecutor interface below.
func (s *Store) CreateInboundMessageInTx(ctx context.Context, tx pgx.Tx, id, agentID, senderEmail, recipient, emailMessageID, subject, conversationID, deliveryStatus string, rawMessage []byte, authHeaders map[string]string, authVerdict []byte, flagged bool, flagReason string, toRecipients, cc, replyTo []string, screening InboundScreening) (*Message, error) {
	assignment := freshInboundMessageThread(emailMessageID)
	assignment.resolutionSource = "no_anchor"
	message, err := createInboundMessage(ctx, tx, assignment, id, agentID, senderEmail, recipient, emailMessageID, subject, conversationID, deliveryStatus, rawMessage, authHeaders, authVerdict, flagged, flagReason, toRecipients, cc, replyTo, screening, nil)
	if err == nil {
		s.recordThreadAssignmentTx(tx, assignment)
	}
	return message, err
}

func (s *Store) CreateInboundMessageAuthenticatedInTx(ctx context.Context, tx pgx.Tx, id, agentID string, auth InboundAuth, recipient, emailMessageID, subject, conversationID, deliveryStatus string, rawMessage []byte, flagged bool, flagReason string, toRecipients, cc, replyTo []string, screening InboundScreening) (*Message, error) {
	assignment := freshInboundMessageThread(emailMessageID)
	assignment.resolutionSource = "no_anchor"
	message, err := createInboundMessage(ctx, tx, assignment, id, agentID, storedInboundSender(auth), recipient, emailMessageID, subject, conversationID, deliveryStatus, rawMessage, nil, nil, flagged, flagReason, toRecipients, cc, replyTo, screening, &auth)
	if err == nil {
		s.recordThreadAssignmentTx(tx, assignment)
	}
	return message, err
}

func (s *Store) CreateInboundMessageAuthenticatedThreadedInTx(ctx context.Context, tx pgx.Tx, id, agentID string, auth InboundAuth, recipient, emailMessageID, subject, conversationID, deliveryStatus string, rawMessage []byte, flagged bool, flagReason string, toRecipients, cc, replyTo []string, screening InboundScreening, evidence InboundThreadEvidence) (*Message, error) {
	assignment, err := s.resolveInboundThreadTx(ctx, tx, agentID, recipient, emailMessageID, auth, evidence)
	if err != nil {
		return nil, err
	}
	message, err := createInboundMessage(ctx, tx, assignment, id, agentID, storedInboundSender(auth), recipient, emailMessageID, subject, conversationID, deliveryStatus, rawMessage, nil, nil, flagged, flagReason, toRecipients, cc, replyTo, screening, &auth)
	if err == nil {
		s.recordThreadAssignmentTx(tx, assignment)
	}
	return message, err
}

// CreateInboundMessageAuthenticatedTwinInTx creates the Inbox representation
// of a providerless physical delivery that the platform itself already
// authenticated (self-send/local loopback). It copies the source outbound
// thread without making either twin the reply parent of the other.
func (s *Store) CreateInboundMessageAuthenticatedTwinInTx(ctx context.Context, tx pgx.Tx, sourceMessageID, id, agentID string, auth InboundAuth, recipient, emailMessageID, subject, conversationID, deliveryStatus string, rawMessage []byte, flagged bool, flagReason string, toRecipients, cc, replyTo []string, screening InboundScreening) (*Message, error) {
	threadID, err := s.EnsureThreadTx(ctx, tx, agentID, sourceMessageID)
	if err != nil {
		return nil, err
	}
	assignment := freshInboundMessageThread(emailMessageID)
	assignment.threadID = threadID
	assignment.resolutionSource = "self_twin"
	message, err := createInboundMessage(ctx, tx, assignment, id, agentID, storedInboundSender(auth), recipient, emailMessageID, subject, conversationID, deliveryStatus, rawMessage, nil, nil, flagged, flagReason, toRecipients, cc, replyTo, screening, &auth)
	if err == nil {
		s.recordThreadAssignmentTx(tx, assignment)
	}
	return message, err
}

func storedInboundSender(auth InboundAuth) string {
	if auth.StoredSender != "" {
		return auth.StoredSender
	}
	return auth.HeaderFrom
}

// messageExecutor is the subset of *pgxpool.Pool and pgx.Tx that
// createInboundMessage needs. Parallel to agentExecutor (which already
// lives in this file for createAgent) — same shape, different scope.
type messageExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func createInboundMessage(ctx context.Context, exec messageExecutor, assignment messageThreadAssignment, id, agentID, senderEmail, recipient, emailMessageID, subject, conversationID, deliveryStatus string, rawMessage []byte, authHeaders map[string]string, authVerdict []byte, flagged bool, flagReason string, toRecipients, cc, replyTo []string, screening InboundScreening, canonical *InboundAuth) (*Message, error) {
	if id == "" {
		id = NewMessageID()
	}
	if !IsValidThreadID(assignment.threadID) {
		return nil, fmt.Errorf("invalid thread assignment")
	}
	now := time.Now()

	var authHeadersJSON []byte
	if authHeaders != nil {
		var err error
		authHeadersJSON, err = json.Marshal(authHeaders)
		if err != nil {
			return nil, fmt.Errorf("marshal auth headers: %w", err)
		}
	}
	var authenticationJSON []byte
	if canonical != nil && canonical.Authentication != nil {
		var err error
		authenticationJSON, err = json.Marshal(canonical.Authentication)
		if err != nil {
			return nil, fmt.Errorf("marshal authentication: %w", err)
		}
	}

	// Held messages (review/block) carry a review-queue status; everything else is
	// 'sent' (the inbound default — delivered).
	status := MessageStatusSent
	if screening.Status != "" {
		status = screening.Status
	}

	m := &Message{
		ID:                id,
		AgentID:           agentID,
		Direction:         "inbound",
		Sender:            senderEmail,
		Recipient:         recipient,
		ToRecipients:      toRecipients,
		CC:                cc,
		ReplyTo:           replyTo,
		Subject:           subject,
		EmailMessageID:    emailMessageID,
		RawMessage:        rawMessage,
		AuthHeaders:       authHeaders,
		ConversationID:    conversationID,
		ThreadID:          assignment.threadID,
		ThreadParentID:    assignment.threadParentID,
		RFCMessageIDKey:   assignment.rfcMessageIDKey,
		DeliveryStatus:    deliveryStatus,
		Flagged:           flagged,
		FlagReason:        flagReason,
		ReviewReason:      screening.ReviewReason,
		ScanScore:         screening.ScanScore,
		ScanAction:        screening.ScanAction,
		Status:            status,
		ApprovalExpiresAt: screening.ApprovalExpiresAt,
		CreatedAt:         now,
		ExpiresAt:         nil,
	}
	if canonical != nil {
		m.HeaderFrom = canonical.HeaderFrom
		m.EnvelopeFrom = canonical.EnvelopeFrom
		m.Authentication = canonical.Authentication
	}
	// inbox_status column has CHECK constraint: must be 'unread', 'read', or NULL
	var inboxStatus *string
	if m.DeliveryStatus == "unread" || m.DeliveryStatus == "read" {
		inboxStatus = &m.DeliveryStatus
	}
	_, err := exec.Exec(ctx,
		`INSERT INTO messages (id, agent_id, direction, sender, header_from, envelope_from, authentication, recipient, to_recipients, cc, reply_to, subject, email_message_id, raw_message, auth_headers, auth_verdict, flagged, flag_reason, conversation_id, thread_id, thread_parent_id, rfc_message_id_key, inbox_status, created_at, expires_at, review_reason, scan_score, scan_action, status, approval_expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30)`,
		m.ID, m.AgentID, m.Direction, m.Sender, nullIfEmptyString(m.HeaderFrom), nullIfEmptyString(m.EnvelopeFrom), nullIfEmptyBytes(authenticationJSON), m.Recipient, m.ToRecipients, m.CC, m.ReplyTo, m.Subject, m.EmailMessageID, m.RawMessage, authHeadersJSON, nullIfEmptyBytes(authVerdict), m.Flagged, nullIfEmptyString(m.FlagReason), m.ConversationID, m.ThreadID, nullIfEmptyString(m.ThreadParentID), nullIfEmptyString(m.RFCMessageIDKey), inboxStatus, m.CreatedAt, m.ExpiresAt, nullIfEmptyString(m.ReviewReason), m.ScanScore, nullIfEmptyString(m.ScanAction), m.Status, m.ApprovalExpiresAt,
	)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// InboundScreening carries the applied screening verdict denormalized onto the
// inbound message row (migration 037). Zero value = no screening (delivered
// normally as status 'sent'). When Status is set to a review-hold status
// (pending_review / review_rejected) the message is persisted but NOT delivered;
// ApprovalExpiresAt sets the review TTL deadline for the expiry worker.
type InboundScreening struct {
	ReviewReason      string
	ScanScore         *float64
	ScanAction        string
	Status            string
	ApprovalExpiresAt *time.Time
}

func (s *Store) GetInboundMessage(ctx context.Context, id string) (*Message, error) {
	m := &Message{}
	var authentication, authVerdict []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, agent_id, direction, sender, COALESCE(header_from, ''), COALESCE(envelope_from, ''), authentication, recipient, to_recipients, cc, reply_to, subject, email_message_id, raw_message, auth_verdict, COALESCE(flagged, false), COALESCE(flag_reason, ''), COALESCE(conversation_id, ''), COALESCE(method, ''), created_at, expires_at
		 FROM messages WHERE id = $1 AND direction = 'inbound'
		   AND deleted_at IS NULL
		   AND status NOT IN (`+heldInboundStatuses+`)`, id,
	).Scan(&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.HeaderFrom, &m.EnvelopeFrom, &authentication, &m.Recipient, &m.ToRecipients, &m.CC, &m.ReplyTo, &m.Subject, &m.EmailMessageID, &m.RawMessage, &authVerdict, &m.Flagged, &m.FlagReason, &m.ConversationID, &m.Method, &m.CreatedAt, &m.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if err := unmarshalAuthVerdict(authVerdict, m); err != nil {
		return nil, err
	}
	if err := unmarshalAuthentication(authentication, authVerdict, m); err != nil {
		return nil, err
	}
	return m, nil
}

// ThreadMessageID returns the RFC 5322 Message-ID to anchor a reply's
// In-Reply-To / References on. An inbound message carries the sender's
// Message-ID in email_message_id; an outbound message the agent sent has no
// email_message_id (the composer omits Message-ID — see
// internal/outbound/compose.go) and instead carries the relay/SES-assigned
// Message-ID, angle-bracketed, in provider_message_id. Threading off the wrong
// field forks the recipient's mail thread, so callers replying to their own
// outbound must use this rather than EmailMessageID directly.
func (m *Message) ThreadMessageID() string {
	if m.Direction == "outbound" {
		return m.ProviderMessageID
	}
	return m.EmailMessageID
}

// GetRepliableMessage loads a message that can be the target of a reply or
// forward, regardless of direction: an inbound the agent received or an
// outbound the agent sent. It is the direction-agnostic sibling of
// GetInboundMessage — same columns (plus provider_message_id, which carries
// the outbound Message-ID for threading; see ThreadMessageID) — but without
// the `direction = 'inbound'` predicate, so an agent can continue a thread off
// its own sent message (mirrors how mail clients let you reply to a message in
// your Sent folder).
//
// The held-status exclusion is kept for BOTH directions: a message still in
// review (pending/rejected/expired) has not actually been delivered, so it is
// not a legitimate reply/forward anchor. Callers still scope the result to the
// owning agent (id-only lookup here does not).
//
// method + delivery_status are loaded (beyond GetInboundMessage's column list)
// because `status` is the review/hold axis, not the delivery axis: an outbound
// row reads status='sent' the instant it is accepted, long before the send
// worker submits it. The reply/forward handlers need the delivery axis to tell
// an outbound that is still queued for external submission (no
// provider_message_id yet → ThreadMessageID() would return "" and silently drop
// In-Reply-To/References) from one that is genuinely terminal. See
// httpapi.parentNotYetSubmitted.
func (s *Store) GetRepliableMessage(ctx context.Context, id string) (*Message, error) {
	m := &Message{}
	var authentication, authVerdict []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, agent_id, direction, sender, COALESCE(header_from, ''), COALESCE(envelope_from, ''), authentication, recipient, to_recipients, cc, reply_to, subject, email_message_id, COALESCE(provider_message_id, ''), COALESCE(method, ''), COALESCE(delivery_status, ''), raw_message, auth_verdict, COALESCE(flagged, false), COALESCE(flag_reason, ''), COALESCE(conversation_id, ''), created_at, expires_at
		 FROM messages WHERE id = $1
		   AND deleted_at IS NULL
		   AND status NOT IN (`+heldInboundStatuses+`)`, id,
	).Scan(&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.HeaderFrom, &m.EnvelopeFrom, &authentication, &m.Recipient, &m.ToRecipients, &m.CC, &m.ReplyTo, &m.Subject, &m.EmailMessageID, &m.ProviderMessageID, &m.Method, &m.DeliveryStatus, &m.RawMessage, &authVerdict, &m.Flagged, &m.FlagReason, &m.ConversationID, &m.CreatedAt, &m.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if err := unmarshalAuthVerdict(authVerdict, m); err != nil {
		return nil, err
	}
	if err := unmarshalAuthentication(authentication, authVerdict, m); err != nil {
		return nil, err
	}
	return m, nil
}

// unmarshalAuthVerdict parses the messages.auth_verdict JSONB column into
// m.Auth. A NULL/empty column (every outbound row, and inbound rows written
// before migration 032) leaves m.Auth nil.
func unmarshalAuthVerdict(b []byte, m *Message) error {
	if len(b) == 0 {
		return nil
	}
	var r emailauth.AuthVerdict
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("unmarshal auth verdict: %w", err)
	}
	m.Auth = &r
	return nil
}

func unmarshalAuthentication(authenticationJSON, legacyVerdictJSON []byte, m *Message) error {
	if len(authenticationJSON) > 0 {
		var authentication emailauth.Authentication
		if err := json.Unmarshal(authenticationJSON, &authentication); err != nil {
			return fmt.Errorf("unmarshal authentication: %w", err)
		}
		if authentication.DKIM == nil {
			authentication.DKIM = []emailauth.DKIMResult{}
		}
		if authentication.DMARC.AlignedBy == nil {
			authentication.DMARC.AlignedBy = []emailauth.AlignmentMechanism{}
		}
		m.Authentication = &authentication
		return nil
	}
	if m.Direction == "inbound" && m.Method != "loopback" {
		detail := "inbound SMTP authentication evidence unavailable"
		spf := emailauth.SPFResult{Status: emailauth.StatusPermError, Detail: detail}
		if len(legacyVerdictJSON) > 0 {
			detail = "authentication evidence predates RFC 9989 evaluation"
			legacy := m.Auth
			if legacy == nil {
				legacy = &emailauth.AuthVerdict{}
				if err := json.Unmarshal(legacyVerdictJSON, legacy); err != nil {
					return fmt.Errorf("unmarshal legacy authentication evidence: %w", err)
				}
			}
			spf.Status = legacySPFStatus(legacy.SPF.Status)
			spf.Detail = legacy.SPF.Detail
			if spf.Detail == "" {
				spf.Detail = detail
			}
		}
		m.Authentication = &emailauth.Authentication{
			SPF:  spf,
			DKIM: []emailauth.DKIMResult{},
			DMARC: emailauth.DMARCResult{
				Status:    emailauth.StatusPermError,
				AlignedBy: []emailauth.AlignmentMechanism{},
				Detail:    detail,
			},
		}
	}
	return nil
}

// legacySPFStatus keeps persisted SPF evidence inside the current closed enum.
// Legacy rows do not retain enough context to reconstruct alignment, so callers
// must still use the synthetic DMARC permerror as the fail-closed decision.
func legacySPFStatus(status emailauth.Status) emailauth.Status {
	switch status {
	case emailauth.StatusPass,
		emailauth.StatusFail,
		emailauth.StatusNone,
		emailauth.StatusNeutral,
		emailauth.StatusSoftFail,
		emailauth.StatusTempError,
		emailauth.StatusPermError:
		return status
	default:
		return emailauth.StatusPermError
	}
}

// GetInboundByEmailMessageID looks up an inbound message by its RFC 5322
// Message-ID for the given agent. Used by HITL flows to reach the parent
// inbound at approval time so the References chain can be rebuilt — the
// pending-outbound row only stores the parent's Message-ID, not its raw
// message. Scoped to agent_id to prevent any cross-agent reach across
// shared infra. Returns sql.ErrNoRows when the inbound has expired or
// was never persisted; callers must tolerate that and fall back to
// legacy single-id threading.
//
// This reply-threading lookup selects the stored header context needed to
// rebuild References, including the raw message and legacy auth-header map. It
// omits structured authentication evidence, which message/review detail paths
// load separately.
func (s *Store) GetInboundByEmailMessageID(ctx context.Context, agentID, emailMessageID string) (*Message, error) {
	if emailMessageID == "" {
		return nil, fmt.Errorf("empty email_message_id")
	}
	m := &Message{}
	var authHeaders map[string]string
	err := s.pool.QueryRow(ctx,
		`SELECT id, agent_id, direction, sender, recipient, to_recipients, cc, reply_to, subject, email_message_id, raw_message, auth_headers, created_at, expires_at
		 FROM messages
		 WHERE agent_id = $1
		   AND direction = 'inbound'
		   AND email_message_id = $2
		   AND deleted_at IS NULL
		   AND status NOT IN (`+heldInboundStatuses+`)
		 ORDER BY created_at DESC LIMIT 1`,
		agentID, emailMessageID,
	).Scan(&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.Recipient, &m.ToRecipients, &m.CC, &m.ReplyTo, &m.Subject, &m.EmailMessageID, &m.RawMessage, &authHeaders, &m.CreatedAt, &m.ExpiresAt)
	if err != nil {
		return nil, err
	}
	m.AuthHeaders = authHeaders
	return m, nil
}

// GetMessageByEmailMessageID looks up a message by its RFC 5322 Message-ID for
// the given agent, regardless of direction. It is the direction-agnostic
// sibling of GetInboundByEmailMessageID: the HITL approve path uses it to
// rebuild the References chain of a held reply, and the reply's parent can be
// an outbound the agent sent (reply-to-own-message), not just a received
// inbound.
//
// The id is matched against email_message_id (where inbound rows carry the
// sender's Message-ID) OR provider_message_id (where outbound rows carry the
// relay/SES-assigned Message-ID — outbound rows have no email_message_id, see
// ThreadMessageID), so a held reply threaded onto either kind of parent
// resolves. Same expiry/held exclusions apply. Returns sql.ErrNoRows when the
// parent has expired or was never persisted; callers must tolerate that and
// fall back to legacy single-id threading.
func (s *Store) GetMessageByEmailMessageID(ctx context.Context, agentID, messageID string) (*Message, error) {
	if messageID == "" {
		return nil, fmt.Errorf("empty message id")
	}
	m := &Message{}
	var authHeaders map[string]string
	err := s.pool.QueryRow(ctx,
		`SELECT id, agent_id, direction, sender, recipient, to_recipients, cc, reply_to, subject, email_message_id, raw_message, auth_headers, created_at, expires_at
		 FROM messages
		 WHERE agent_id = $1
		   AND (email_message_id = $2 OR provider_message_id = $2)
		   AND deleted_at IS NULL
		   AND status NOT IN (`+heldInboundStatuses+`)
		 ORDER BY created_at DESC LIMIT 1`,
		agentID, messageID,
	).Scan(&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.Recipient, &m.ToRecipients, &m.CC, &m.ReplyTo, &m.Subject, &m.EmailMessageID, &m.RawMessage, &authHeaders, &m.CreatedAt, &m.ExpiresAt)
	if err != nil {
		return nil, err
	}
	m.AuthHeaders = authHeaders
	return m, nil
}

// CreateOutboundMessage stores an outbound message with multi-recipient support.
// The recipient param is kept for backward compat with the singular recipient column;
// toRecipients, cc, and bcc are the canonical outbound-only multi-recipient fields.
func (s *Store) CreateOutboundMessage(ctx context.Context, agentID string, toRecipients []string, cc []string, bcc []string, subject, msgType, method, providerMessageID, conversationID string, rawMessage []byte) (*Message, error) {
	var out *Message
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := s.CreateOutboundMessageTx(ctx, tx, agentID, toRecipients, cc, bcc, subject, msgType, method, providerMessageID, conversationID, rawMessage, "", "", "")
		out = m
		return err
	})
	return out, err
}

// CreateOutboundMessageTx is CreateOutboundMessage on the caller's transaction,
// for the async accept path (async-message-pipeline.md, slice C). It persists the
// two-column model: status=MessageStatusSent (the hold/lifecycle column — this row
// is not held) while delivery_status carries the send progression (the accept-tx
// passes 'accepted'; the send worker later advances it to 'sent'/'failed'). It
// also stamps envelope_from + sent_as, decided once at compose time, so the worker
// can submit the persisted bytes without re-composing. provider_message_id is
// empty until the worker records the SES id in MarkOutboundSentTx.
func (s *Store) CreateOutboundMessageTx(ctx context.Context, tx pgx.Tx, agentID string, toRecipients, cc, bcc []string, subject, msgType, method, providerMessageID, conversationID string, rawMessage []byte, deliveryStatus, envelopeFrom, sentAs string) (*Message, error) {
	return s.CreateOutboundMessageThreadedTx(
		ctx, tx, "", agentID, toRecipients, cc, bcc, subject, msgType, method,
		providerMessageID, conversationID, rawMessage, deliveryStatus,
		envelopeFrom, sentAs,
	)
}

func (s *Store) createOutboundMessageAssignedTx(ctx context.Context, tx pgx.Tx, assignment messageThreadAssignment, agentID string, toRecipients, cc, bcc []string, subject, msgType, method, providerMessageID, conversationID string, rawMessage []byte, deliveryStatus, envelopeFrom, sentAs string) (*Message, error) {
	if !IsValidThreadID(assignment.threadID) {
		return nil, fmt.Errorf("invalid thread assignment")
	}
	id := "msg_" + generateID()
	now := time.Now()

	var recipient string
	if len(toRecipients) > 0 {
		recipient = toRecipients[0]
	}

	m := &Message{
		ID:                id,
		AgentID:           agentID,
		Direction:         "outbound",
		Recipient:         recipient,
		Subject:           subject,
		Type:              msgType,
		Method:            method,
		ProviderMessageID: providerMessageID,
		ConversationID:    conversationID,
		ThreadID:          assignment.threadID,
		ThreadParentID:    assignment.threadParentID,
		RFCMessageIDKey:   assignment.rfcMessageIDKey,
		CreatedAt:         now,
		ExpiresAt:         nil,
		ToRecipients:      toRecipients,
		CC:                cc,
		BCC:               bcc,
		RawMessage:        rawMessage,
		Sender:            agentID,
		DeliveryStatus:    deliveryStatus,
		SentAs:            sentAs,
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO messages (id, agent_id, direction, recipient, subject, message_type, method, provider_message_id, conversation_id, thread_id, thread_parent_id, rfc_message_id_key, created_at, expires_at, to_recipients, cc, bcc, status, sender, raw_message, delivery_status, sent_as, envelope_from)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`,
		m.ID, m.AgentID, m.Direction, m.Recipient, m.Subject, m.Type, m.Method, m.ProviderMessageID, m.ConversationID, m.ThreadID, nullIfEmptyString(m.ThreadParentID), nullIfEmptyString(m.RFCMessageIDKey), m.CreatedAt, m.ExpiresAt, m.ToRecipients, m.CC, m.BCC, MessageStatusSent, m.Sender, nullIfEmptyBytes(m.RawMessage), nullIfEmpty(deliveryStatus), nullIfEmpty(sentAs), nullIfEmpty(envelopeFrom),
	)
	if err != nil {
		return nil, err
	}
	m.Status = MessageStatusSent
	if _, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
		MessageID: m.ID, DedupeKey: "acceptance", Direction: "outbound",
		ReasonCode: messagelifecycle.ReasonAcceptanceOutboundAPI, OccurredAt: m.CreatedAt,
	}); err != nil {
		return nil, err
	}
	return m, nil
}

// StampSendJobIDTx records the River outbound_send job id on the accepted message,
// within the accept-tx, so the async-send reconciler can find stranded rows
// ('accepted' with send_job_id IS NULL). Mirrors the webhook_subscriber_deliveries
// .job_id stamp.
func (s *Store) StampSendJobIDTx(ctx context.Context, tx pgx.Tx, messageID string, jobID int64) error {
	var stampedMessageID string
	if err := tx.QueryRow(ctx, `UPDATE messages SET send_job_id = $2 WHERE id = $1 AND direction = 'outbound' RETURNING id`, messageID, jobID).Scan(&stampedMessageID); errors.Is(err, pgx.ErrNoRows) {
		return ErrMessageNotFound
	} else if err != nil {
		return err
	}
	_, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
		MessageID: messageID, DedupeKey: "queue:outbound", Direction: "outbound",
		ReasonCode:     messagelifecycle.ReasonQueueOutboundSubmission,
		CorrelationIDs: messagelifecycle.SafeCorrelationIDs(map[string]string{"job_id": fmt.Sprint(jobID)}),
		OccurredAt:     time.Now(),
	})
	return err
}

// StampScheduledAtTx records the future send instant on a scheduled outbound
// message, WITHIN the accept-tx (mirrors StampSendJobIDTx). Called only when the
// caller supplied a future send_at; immediate sends never call this, leaving
// scheduled_at NULL. The corresponding River job is enqueued with the same
// instant as its ScheduledAt, so the column and the job agree.
func (s *Store) StampScheduledAtTx(ctx context.Context, tx pgx.Tx, messageID string, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE messages SET scheduled_at = $2 WHERE id = $1`, messageID, at)
	return err
}

// CreatePendingOutboundMessage stores a fully composed outbound email in
// pending_review status, including body_text, body_html, and attachments so
// that approval can reconstruct the original SendRequest (or accept edits)
// without the caller needing to retain it. ttlSeconds sets how long the
// message remains pending before the expiration worker resolves it.
//
// replyToEmailMessageID is the RFC 5322 Message-ID of the inbound being
// replied to (e.g. "<abc@gmail.com>"), or "" for fresh sends and test emails.
// It reuses the email_message_id column, which is unused for outbound rows
// in every other path — the column semantically carries "the Message-ID this
// row references" in both directions.
//
// attachmentsJSON must be a JSON array matching the public Attachment shape
// ([{filename, content_type, data}, ...]) or nil. Callers that already have
// an []outbound.Attachment slice should json.Marshal it before passing in.
func (s *Store) CreatePendingOutboundMessage(ctx context.Context, agentID string, toRecipients, cc, bcc []string, subject, bodyText, bodyHTML string, attachmentsJSON []byte, msgType, conversationID, replyToEmailMessageID, replyTo string, ttlSeconds int) (*Message, error) {
	return s.CreatePendingOutboundMessageManaged(ctx, agentID, toRecipients, cc, bcc, subject, bodyText, bodyHTML, attachmentsJSON, msgType, conversationID, replyToEmailMessageID, replyTo, ttlSeconds, false)
}

func (s *Store) CreatePendingOutboundMessageManaged(ctx context.Context, agentID string, toRecipients, cc, bcc []string, subject, bodyText, bodyHTML string, attachmentsJSON []byte, msgType, conversationID, replyToEmailMessageID, replyTo string, ttlSeconds int, managedUnsubscribe bool) (*Message, error) {
	var out *Message
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := s.CreatePendingOutboundMessageManagedTx(ctx, tx, agentID, toRecipients, cc, bcc, subject, bodyText, bodyHTML, attachmentsJSON, msgType, conversationID, replyToEmailMessageID, replyTo, ttlSeconds, managedUnsubscribe)
		out = m
		return err
	})
	return out, err
}

// CreatePendingOutboundMessageTx is the in-tx sibling of
// CreatePendingOutboundMessage, letting the HITL hold write the pending_review
// row and enqueue its approval-notification job (QueueNotify) in ONE transaction
// so neither can exist without the other (docs/design/hitl-notify-river.md).
// Mirrors CreateOutboundMessageTx / the send accept-tx.
func (s *Store) CreatePendingOutboundMessageTx(ctx context.Context, tx pgx.Tx, agentID string, toRecipients, cc, bcc []string, subject, bodyText, bodyHTML string, attachmentsJSON []byte, msgType, conversationID, replyToEmailMessageID, replyTo string, ttlSeconds int) (*Message, error) {
	return s.CreatePendingOutboundMessageManagedTx(ctx, tx, agentID, toRecipients, cc, bcc, subject, bodyText, bodyHTML, attachmentsJSON, msgType, conversationID, replyToEmailMessageID, replyTo, ttlSeconds, false)
}

func (s *Store) CreatePendingOutboundMessageManagedTx(ctx context.Context, tx pgx.Tx, agentID string, toRecipients, cc, bcc []string, subject, bodyText, bodyHTML string, attachmentsJSON []byte, msgType, conversationID, replyToEmailMessageID, replyTo string, ttlSeconds int, managedUnsubscribe bool) (*Message, error) {
	return s.CreatePendingOutboundMessageManagedThreadedTx(ctx, tx, "", agentID, toRecipients, cc, bcc, subject, bodyText, bodyHTML, attachmentsJSON, msgType, conversationID, replyToEmailMessageID, replyTo, ttlSeconds, managedUnsubscribe)
}

func (s *Store) CreatePendingOutboundMessageManagedThreadedTx(ctx context.Context, tx pgx.Tx, parentMessageID, agentID string, toRecipients, cc, bcc []string, subject, bodyText, bodyHTML string, attachmentsJSON []byte, msgType, conversationID, replyToEmailMessageID, replyTo string, ttlSeconds int, managedUnsubscribe bool) (*Message, error) {
	assignment, err := s.outboundThreadAssignmentTx(ctx, tx, agentID, msgType, parentMessageID, "")
	if err != nil {
		return nil, err
	}
	m, err := createPendingOutboundMessage(ctx, tx, assignment, agentID, toRecipients, cc, bcc, subject, bodyText, bodyHTML, attachmentsJSON, msgType, conversationID, replyToEmailMessageID, replyTo, ttlSeconds, managedUnsubscribe)
	if err != nil {
		return nil, err
	}
	if _, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
		MessageID: m.ID, DedupeKey: "acceptance", Direction: "outbound",
		ReasonCode: messagelifecycle.ReasonAcceptanceOutboundAPI, OccurredAt: m.CreatedAt,
	}); err != nil {
		return nil, err
	}
	hold, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
		MessageID: m.ID, DedupeKey: "review:hold", Direction: "outbound",
		ReasonCode: messagelifecycle.ReasonReviewHoldCreated,
		Evidence:   map[string]any{"review_resolution": "pending"}, OccurredAt: m.CreatedAt,
	})
	if err != nil {
		return nil, err
	}
	m.LifecycleTransitions = []messagelifecycle.MessageLifecycleTransition{hold}
	s.recordThreadAssignmentTx(tx, assignment)
	return m, nil
}

// createPendingOutboundMessage is the shared body of the pool and in-tx pending
// creators; exec is satisfied by both *pgxpool.Pool and pgx.Tx (messageExecutor).
// replyTo, when non-empty, is a caller-supplied Reply-To override persisted on
// the reply_to column (single element) so it survives the recompose at approval.
func createPendingOutboundMessage(ctx context.Context, exec messageExecutor, assignment messageThreadAssignment, agentID string, toRecipients, cc, bcc []string, subject, bodyText, bodyHTML string, attachmentsJSON []byte, msgType, conversationID, replyToEmailMessageID, replyTo string, ttlSeconds int, managedUnsubscribe bool) (*Message, error) {
	if ttlSeconds <= 0 || ttlSeconds > HITLMaxTTLSeconds {
		return nil, fmt.Errorf("ttl_seconds must be between 1 and %d", HITLMaxTTLSeconds)
	}
	if !IsValidThreadID(assignment.threadID) {
		return nil, fmt.Errorf("invalid thread assignment")
	}

	id := "msg_" + generateID()
	now := time.Now()
	approvalExpiresAt := now.Add(time.Duration(ttlSeconds) * time.Second)

	var recipient string
	if len(toRecipients) > 0 {
		recipient = toRecipients[0]
	}

	var attachmentsArg interface{}
	if len(attachmentsJSON) > 0 {
		attachmentsArg = attachmentsJSON
	}

	// Persist a caller Reply-To override as a single-element array (matching how
	// the reply_to column stores the parsed header on inbound rows). Empty ⇒ NULL.
	var replyToArg []string
	if replyTo != "" {
		replyToArg = []string{replyTo}
	}

	m := &Message{
		ID:                 id,
		AgentID:            agentID,
		Direction:          "outbound",
		Recipient:          recipient,
		Subject:            subject,
		EmailMessageID:     replyToEmailMessageID,
		Type:               msgType,
		ConversationID:     conversationID,
		ThreadID:           assignment.threadID,
		ThreadParentID:     assignment.threadParentID,
		CreatedAt:          now,
		ExpiresAt:          nil,
		ToRecipients:       toRecipients,
		CC:                 cc,
		BCC:                bcc,
		ReplyTo:            replyToArg,
		Status:             MessageStatusPendingReview,
		ApprovalExpiresAt:  &approvalExpiresAt,
		BodyText:           bodyText,
		BodyHTML:           bodyHTML,
		AttachmentsJSON:    json.RawMessage(attachmentsJSON),
		ManagedUnsubscribe: managedUnsubscribe,
		// Sender is the agent itself (agent ID == email) so `from` isn't empty
		// on a held draft's detail/list view (B1).
		Sender: agentID,
	}
	_, err := exec.Exec(ctx,
		`INSERT INTO messages (
		    id, agent_id, direction, recipient, subject, email_message_id, message_type,
		    conversation_id, thread_id, thread_parent_id, created_at, expires_at,
		    to_recipients, cc, bcc, reply_to,
		    status, approval_expires_at,
		    body_text, body_html, attachments_json, sender, managed_unsubscribe)
		 VALUES ($1, $2, $3, $4, $5, $6, $7,
		         $8, $9, $10, $11, $12,
		         $13, $14, $15, $16,
		         $17, $18,
		         $19, $20, $21, $22, $23)`,
		m.ID, m.AgentID, m.Direction, m.Recipient, m.Subject, m.EmailMessageID, m.Type,
		m.ConversationID, m.ThreadID, nullIfEmptyString(m.ThreadParentID), m.CreatedAt, m.ExpiresAt,
		m.ToRecipients, m.CC, m.BCC, replyToArg,
		m.Status, m.ApprovalExpiresAt,
		nullIfEmptyString(m.BodyText), nullIfEmptyString(m.BodyHTML), attachmentsArg, m.Sender, m.ManagedUnsubscribe,
	)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// StampNotifyJobIDTx records the River QueueNotify job id on the pending_review
// message, within the hold accept-tx, so the notification reconciler can find
// rows stranded without a job (pending_review AND notify_job_id IS NULL).
// Mirrors StampSendJobIDTx.
func (s *Store) StampNotifyJobIDTx(ctx context.Context, tx pgx.Tx, messageID string, jobID int64) error {
	_, err := tx.Exec(ctx, `UPDATE messages SET notify_job_id = $2 WHERE id = $1`, messageID, jobID)
	return err
}

// MarkMessageNotified stamps notified_at after the approval-notification email is
// sent. Set only AFTER a successful send, so it is the send-dedup marker that makes
// a crash-after-send River re-drive a no-op without ever risking a lost
// notification (loss would require setting it before the send).
func (s *Store) MarkMessageNotified(ctx context.Context, messageID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE messages SET notified_at = now() WHERE id = $1`, messageID)
	return err
}

// PendingNotify is what the HITL approval-notification worker re-reads for one
// message: the held message (the fields the notifier composes from), its owning
// agent, and whether it was already notified.
type PendingNotify struct {
	Message  *Message
	Agent    *AgentIdentity
	Notified bool
}

// LoadPendingNotify returns the row the notification worker needs, or (nil, nil)
// when there is nothing to notify about — the message was deleted/pruned, or its
// owning agent no longer exists (an orphaned hold; other paths finalize it). The
// worker treats a nil return as a no-op.
func (s *Store) LoadPendingNotify(ctx context.Context, messageID string) (*PendingNotify, error) {
	m := &Message{ID: messageID}
	var agentID string
	var notifiedAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT status, subject, to_recipients, cc, bcc, approval_expires_at, agent_id, notified_at
		   FROM messages WHERE id = $1`, messageID,
	).Scan(&m.Status, &m.Subject, &m.ToRecipients, &m.CC, &m.BCC, &m.ApprovalExpiresAt, &agentID, &notifiedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // message gone
		}
		return nil, err
	}
	m.AgentID = agentID

	agent, err := s.GetAgentByID(ctx, agentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // owning agent deleted — orphaned hold, nothing to notify
		}
		return nil, err
	}
	return &PendingNotify{Message: m, Agent: agent, Notified: notifiedAt != nil}, nil
}

// nullIfEmptyString returns nil interface when s is empty so the column is
// inserted as SQL NULL rather than ”. Keeps absent content distinguishable
// from a non-empty retained body.
func nullIfEmptyString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// --- HITL approval store helpers ---

// ErrNotPendingApproval is returned when an approve or reject operation
// targets a message that is not (or is no longer) in pending_review status.
// Handlers map this to HTTP 409 Conflict.
var ErrNotPendingApproval = fmt.Errorf("message is not pending approval")

// ErrInvalidApprovalTarget is returned when ApproveAndAccept is called with a
// status that is not an approval outcome. Validation happens before a
// transaction is opened so unsupported resolutions cannot mutate the hold or
// enqueue work.
var ErrInvalidApprovalTarget = fmt.Errorf("invalid approval target")

// approvalTxTimeout caps how long a single approve-and-send transaction may
// hold its row-level lock. Chosen to sit just above SMTPRelay's worst-case
// retry envelope: 30s dial + 2min deadline per attempt × 3 attempts, plus
// 21s of backoff sleeps, rounded up. If the underlying send ever ignores
// its own deadlines, this safeguard cancels the tx and releases the lock.
const approvalTxTimeout = 7 * time.Minute

// ErrMessageNotFound is returned when a message is not found for the given
// user (either the ID doesn't exist or the message belongs to another user's
// agent). Handlers map this to HTTP 404.
var ErrMessageNotFound = fmt.Errorf("message not found")

// PendingApprovalEdit holds optional overrides a reviewer can apply when
// approving a pending message. Pointer-typed strings distinguish "not
// provided" (nil) from "explicitly empty" (pointer to ""). Slice fields
// distinguish "unset" (nil) from "empty list" (non-nil zero-length slice).
type PendingApprovalEdit struct {
	Subject         *string
	BodyText        *string
	BodyHTML        *string
	To              []string
	CC              []string
	BCC             []string
	AttachmentsJSON []byte
	// AttachmentsSet must be true when the caller intends to override
	// AttachmentsJSON, since nil and empty [] are both valid overrides
	// (empty [] clears attachments; nil preserves).
	AttachmentsSet bool
}

// Apply mutates msg to reflect any fields the reviewer changed. Returns true
// if any field was actually different from what msg already held (signals
// the edited flag should be set).
func (e PendingApprovalEdit) Apply(msg *Message) bool {
	edited := false
	if e.Subject != nil && *e.Subject != msg.Subject {
		msg.Subject = *e.Subject
		edited = true
	}
	if e.BodyText != nil && *e.BodyText != msg.BodyText {
		msg.BodyText = *e.BodyText
		edited = true
	}
	if e.BodyHTML != nil && *e.BodyHTML != msg.BodyHTML {
		msg.BodyHTML = *e.BodyHTML
		edited = true
	}
	if e.To != nil && !stringSlicesEqual(e.To, msg.ToRecipients) {
		msg.ToRecipients = e.To
		edited = true
	}
	if e.CC != nil && !stringSlicesEqual(e.CC, msg.CC) {
		msg.CC = e.CC
		edited = true
	}
	if e.BCC != nil && !stringSlicesEqual(e.BCC, msg.BCC) {
		msg.BCC = e.BCC
		edited = true
	}
	if e.AttachmentsSet {
		msg.AttachmentsJSON = json.RawMessage(e.AttachmentsJSON)
		edited = true
	}
	return edited
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// GetOutboundMessageForUser returns a full message row (including body, HITL
// fields, and attachments) if it exists and is owned by userID (via the
// agent the row belongs to). Inbound messages and cross-user access both
// return ErrMessageNotFound — the caller should not be able to distinguish
// "does not exist" from "belongs to someone else".
func (s *Store) GetOutboundMessageForUser(ctx context.Context, messageID, userID string) (*Message, error) {
	m := &Message{}
	var (
		bodyText, bodyHTML *string
		attachments        []byte
		method, msgType    *string
		approvalExpires    *time.Time
		reviewedAt         *time.Time
		scheduledAt        *time.Time
		rejectionReason    *string
		reviewedByID       *string
		reviewedByName     *string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT m.id, m.agent_id, m.direction, m.sender, m.recipient, m.subject,
		        m.email_message_id, COALESCE(m.provider_message_id, ''),
		        m.method, m.message_type,
		        m.conversation_id, m.created_at, m.expires_at,
		        m.to_recipients, m.cc, m.bcc, m.reply_to,
		        m.status, m.approval_expires_at, m.reviewed_at, m.scheduled_at,
		        m.rejection_reason, m.edited,
		        m.body_text, m.body_html, m.attachments_json, m.managed_unsubscribe,
		        m.reviewed_by_user_id, r.name,
		        COALESCE(m.delivery_status, ''), COALESCE(m.delivery_detail, ''), COALESCE(m.sent_as, '')
		 FROM messages m
		 JOIN agent_identities a ON a.id = m.agent_id
		 LEFT JOIN users r ON r.id = m.reviewed_by_user_id
		 WHERE m.id = $1 AND a.user_id = $2 AND m.direction = 'outbound'
		   AND a.deleted_at IS NULL`,
		messageID, userID,
	).Scan(
		&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.Recipient, &m.Subject,
		&m.EmailMessageID, &m.ProviderMessageID,
		&method, &msgType,
		&m.ConversationID, &m.CreatedAt, &m.ExpiresAt,
		&m.ToRecipients, &m.CC, &m.BCC, &m.ReplyTo,
		&m.Status, &approvalExpires, &reviewedAt, &scheduledAt,
		&rejectionReason, &m.Edited,
		&bodyText, &bodyHTML, &attachments, &m.ManagedUnsubscribe,
		&reviewedByID, &reviewedByName,
		&m.DeliveryStatus, &m.DeliveryDetail, &m.SentAs,
	)
	if err != nil {
		return nil, ErrMessageNotFound
	}
	if method != nil {
		m.Method = *method
	}
	if msgType != nil {
		m.Type = *msgType
	}
	if approvalExpires != nil {
		m.ApprovalExpiresAt = approvalExpires
	}
	if reviewedAt != nil {
		m.ReviewedAt = reviewedAt
	}
	if scheduledAt != nil {
		m.ScheduledAt = scheduledAt
	}
	if rejectionReason != nil {
		m.RejectionReason = *rejectionReason
	}
	if bodyText != nil {
		m.BodyText = *bodyText
	}
	if bodyHTML != nil {
		m.BodyHTML = *bodyHTML
	}
	if len(attachments) > 0 {
		m.AttachmentsJSON = json.RawMessage(attachments)
	}
	m.ReviewedByUserID = reviewedByID
	m.ReviewedByName = reviewedByName
	return m, nil
}

// ListPendingOutboundForUser returns pending-approval messages across all of
// the user's agents, sorted by approval_expires_at ASC (expiring-soonest
// first). Body columns are not returned from this path — callers should use
// GetOutboundMessageForUser for detail.
func (s *Store) ListPendingOutboundForUser(ctx context.Context, userID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.agent_id, m.subject, m.email_message_id,
		        COALESCE(m.message_type, ''),
		        m.conversation_id, m.created_at,
		        m.to_recipients, m.cc, m.bcc,
		        m.status, m.approval_expires_at,
		        COALESCE(m.delivery_status, ''), COALESCE(m.delivery_detail, ''), COALESCE(m.sent_as, '')
		 FROM messages m
		 JOIN agent_identities a ON a.id = m.agent_id
		 WHERE a.user_id = $1 AND a.deleted_at IS NULL
		   AND m.status = 'pending_review' AND m.direction = 'outbound'
		 ORDER BY m.approval_expires_at ASC
		 LIMIT $2`, userID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var approvalExpires *time.Time
		if err := rows.Scan(
			&m.ID, &m.AgentID, &m.Subject, &m.EmailMessageID,
			&m.Type,
			&m.ConversationID, &m.CreatedAt,
			&m.ToRecipients, &m.CC, &m.BCC,
			&m.Status, &approvalExpires,
			&m.DeliveryStatus, &m.DeliveryDetail, &m.SentAs,
		); err != nil {
			return nil, err
		}
		m.Direction = "outbound"
		m.ApprovalExpiresAt = approvalExpires
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// SendResult carries the outcome of a sender.Send invocation back to the
// store for final persistence. Handlers wrap their sender.Send call in a
// closure that returns this type.
type SendResult struct {
	ProviderMessageID string
	Method            string
	To                []string
	CC                []string
	BCC               []string
	// Sender is the inbound-facing From value for providerless local delivery.
	// It prefers an explicit Reply-To while authenticated identity remains the
	// owning agent address in the event envelope.
	Sender string
	// Raw is the composed MIME that was sent, retained as the message's
	// "Sent folder" copy (raw_message). Local loopback retains the same bytes on
	// both Sent and Inbox rows; it is empty only on already-sent replay.
	Raw []byte
}

// ApproveAndSend finalizes a pending_review message by running it through
// a caller-supplied send function inside a transaction that holds a row lock
// on the pending row. If send returns an error the transaction rolls back
// and the message remains pending. On success the row is updated to
// 'sent' with the provider-assigned Message-ID while body and attachment
// columns remain retained.
//
// edits, if any fields are populated, are applied to the in-memory message
// before send is called and the 'edited' column is set to true when any
// field differs from what was stored. Approval-via-magic-link callers
// pass the zero edits value.
//
// Ownership is enforced by the agent -> user join. Messages owned by
// another user return ErrMessageNotFound. Messages whose status is not
// 'pending_review' return ErrNotPendingApproval. If another worker is
// already mid-send for this message (rare; only possible after the
// approval row lock was released without status changing — e.g. a
// pool drop mid-send), this returns ErrSendInProgress.
//
// Concurrency / failure mode notes:
//
//   - The row-level FOR NO KEY UPDATE lock is held on the messages row
//     for the duration of the send callback. In practice that is
//     bounded by outbound.SMTPRelay's per-attempt deadline (2min) plus
//     its internal retry backoff (1s/5s/15s) — worst case ~6.5min of
//     lock on this single row. Other rows are unaffected; deadlock is
//     not possible because only one row is ever locked per call.
//
//     Why NO KEY UPDATE rather than the stricter FOR UPDATE: the
//     send_attempts INSERT below runs on a SEPARATE pool connection
//     and needs a KEY SHARE lock on this messages row for FK
//     enforcement. FOR UPDATE blocks KEY SHARE; FOR NO KEY UPDATE
//     allows it. The downgrade is safe because nothing in this
//     codebase mutates messages.id (the only key column) after
//     creation — all UPDATEs touch non-key columns, which NO KEY
//     UPDATE serializes against itself exactly like FOR UPDATE.
//
//   - The old crash window where send() succeeded at SES but the
//     subsequent UPDATE/Commit failed (DB blip, pool exhaustion) is now
//     closed by the send_attempts table. Around send() we run two small
//     auxiliary transactions that outlive the surrounding approval
//     transaction: ClaimSendAttempt before send(), MarkSendSucceeded
//     (or MarkSendFailed) after. If the approval tx rolls back AFTER
//     send() succeeded, the next retry of ApproveAndSend reads
//     send_attempts.status='sent', reuses the recorded SendResult, and
//     skips the upstream send entirely.
func (s *Store) ApproveAndSend(
	ctx context.Context,
	messageID, userID string,
	edits PendingApprovalEdit,
	send func(msg *Message) (SendResult, error),
) (*Message, error) {
	// Bound the transaction's lifetime at just above SMTPRelay's worst-case
	// retry envelope (~6.5min). This is a defensive cap: if the relay ever
	// ignores its own deadlines or a send stalls indefinitely, the tx gets
	// cancelled and the row lock releases rather than held forever.
	txCtx, cancel := context.WithTimeout(ctx, approvalTxTimeout)
	defer cancel()

	tx, err := s.pool.Begin(txCtx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(txCtx)
		}
	}()

	// Lock the pending row and verify ownership in one query.
	var (
		m                  Message
		ownerUserID        string
		bodyText, bodyHTML *string
		attachments        []byte
		method, msgType    *string
		approvalExpires    *time.Time
	)
	err = tx.QueryRow(txCtx,
		`SELECT m.id, m.agent_id, m.direction, m.sender, m.recipient, m.subject,
		        m.email_message_id,
		        m.method, m.message_type,
		        m.conversation_id, m.created_at, m.expires_at,
		        m.to_recipients, m.cc, m.bcc, m.reply_to,
		        m.status, m.approval_expires_at, m.edited,
		        m.body_text, m.body_html, m.attachments_json,
		        a.user_id
		 FROM messages m
		 JOIN agent_identities a ON a.id = m.agent_id
		 WHERE m.id = $1 AND m.direction = 'outbound'
		   AND a.deleted_at IS NULL
		 FOR NO KEY UPDATE OF m`,
		messageID,
	).Scan(
		&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.Recipient, &m.Subject,
		&m.EmailMessageID,
		&method, &msgType,
		&m.ConversationID, &m.CreatedAt, &m.ExpiresAt,
		&m.ToRecipients, &m.CC, &m.BCC, &m.ReplyTo,
		&m.Status, &approvalExpires, &m.Edited,
		&bodyText, &bodyHTML, &attachments,
		&ownerUserID,
	)
	if err != nil {
		return nil, ErrMessageNotFound
	}
	if ownerUserID != userID {
		return nil, ErrMessageNotFound
	}
	if m.Status != MessageStatusPendingReview {
		return nil, ErrNotPendingApproval
	}
	if method != nil {
		m.Method = *method
	}
	if msgType != nil {
		m.Type = *msgType
	}
	if approvalExpires != nil {
		m.ApprovalExpiresAt = approvalExpires
	}
	if bodyText != nil {
		m.BodyText = *bodyText
	}
	if bodyHTML != nil {
		m.BodyHTML = *bodyHTML
	}
	if len(attachments) > 0 {
		m.AttachmentsJSON = json.RawMessage(attachments)
	}

	editedByReviewer := edits.Apply(&m)

	// Exactly-once gate around the upstream send. Runs OUTSIDE the
	// approval transaction so its outcome survives an approval-tx
	// rollback — that's the whole point of send_attempts.
	claim, err := s.ClaimSendAttempt(ctx, messageID)
	if err != nil {
		return nil, err
	}

	var result SendResult
	switch claim.Outcome {
	case SendAttemptAcquired:
		result, err = send(&m)
		if err != nil {
			// Mark failed in a separate tx so the next retry can
			// take over. Best-effort: log if the mark itself fails,
			// don't shadow the original send error.
			if markErr := s.MarkSendFailed(ctx, messageID, err.Error()); markErr != nil {
				log.Printf("[approve] MarkSendFailed for %s: %v", messageID, markErr)
			}
			return nil, err
		}
		if markErr := s.MarkSendSucceededWithRetry(messageID, result); markErr != nil {
			// The upstream send DID succeed but we exhausted the
			// retry budget recording that fact. Log loudly so ops
			// can reconcile against the SES Configuration Set
			// events log; the approval tx below still finalizes
			// the message row from this attempt so the customer
			// sees a successful approve. Residual risk: the 10-min
			// stale takeover could re-invoke send() if the row
			// stays `attempting` until the worker fires.
			log.Printf("[approve] MarkSendSucceeded exhausted retries for %s: %v (manual reconciliation may be needed)", messageID, markErr)
		}
	case SendAttemptAlreadySent:
		// A prior approval-tx attempt succeeded at SES but its
		// surrounding tx rolled back. Reuse the recorded result and
		// skip the upstream send.
		result = claim.Sent
	case SendAttemptInFlight:
		return nil, ErrSendInProgress
	}

	_, err = tx.Exec(txCtx,
		`UPDATE messages
		    SET status            = $2,
		        provider_message_id = $3,
		        method            = $4,
		        to_recipients     = $5,
		        cc                = $6,
		        bcc               = $7,
		        recipient         = $8,
		        subject           = $9,
		        edited            = $10,
		        reviewed_at       = now(),
		        reviewed_by_user_id = $11,
		        raw_message       = $12::bytea,
		        rfc_message_id_key = CASE
		          WHEN rfc_message_id_key IS NULL AND $13 <> '' THEN $13
		          ELSE rfc_message_id_key
		        END
		  WHERE id = $1`,
		messageID,
		MessageStatusSent,
		result.ProviderMessageID,
		result.Method,
		result.To,
		result.CC,
		result.BCC,
		firstOr(result.To, ""),
		m.Subject,
		editedByReviewer || m.Edited,
		userID,
		// Retain the sent MIME as the canonical Sent-folder copy alongside the
		// accepted draft columns. Empty on the rare already-sent replay path
		// (send_attempts doesn't cache bytes) -> NULL, best-effort.
		nullIfEmptyBytes(result.Raw),
		canonicalRFCMessageIDKey(result.ProviderMessageID),
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(txCtx); err != nil {
		return nil, err
	}
	committed = true

	// Reflect post-commit state on the returned message.
	m.Status = MessageStatusSent
	m.ProviderMessageID = result.ProviderMessageID
	m.Method = result.Method
	m.ToRecipients = result.To
	m.CC = result.CC
	m.BCC = result.BCC
	if len(result.To) > 0 {
		m.Recipient = result.To[0]
	}
	m.Edited = editedByReviewer || m.Edited
	now := time.Now()
	m.ReviewedAt = &now
	reviewerID := userID
	m.ReviewedByUserID = &reviewerID
	return &m, nil
}

func firstOr(s []string, fallback string) string {
	if len(s) > 0 {
		return s[0]
	}
	return fallback
}

// ResolveOutboundOwner looks up the user_id and agent_id for an outbound
// message without requiring the caller to know the user_id up-front. It
// exists for token-authenticated paths (magic-link approve/reject) where
// the HMAC token itself is the authorization and the handler just needs
// enough context to dispatch into the existing user-scoped store methods.
//
// Returns ErrMessageNotFound if the message doesn't exist or isn't
// outbound. The returned user_id is guaranteed to own the message's
// agent (via the agent_identities.user_id join).
func (s *Store) ResolveOutboundOwner(ctx context.Context, messageID string) (userID, agentID string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT a.user_id, m.agent_id
		 FROM messages m
		 JOIN agent_identities a ON a.id = m.agent_id
		 WHERE m.id = $1 AND m.direction = 'outbound'
		   AND a.deleted_at IS NULL`,
		messageID,
	).Scan(&userID, &agentID)
	if err != nil {
		return "", "", ErrMessageNotFound
	}
	return userID, agentID, nil
}

// ExpirationCandidate is the minimal row the expiration worker needs to
// decide how to finalize an expired pending message.
type ExpirationCandidate struct {
	MessageID        string
	AgentID          string
	ExpirationAction string // 'approve' or 'reject'
}

// ListExpiredPending returns pending_review messages whose
// approval_expires_at is in the past, joined with their agent's
// hitl_expiration_action. Ordered by approval_expires_at ASC so
// earliest-expired are handled first.
func (s *Store) ListExpiredPending(ctx context.Context, limit int) ([]ExpirationCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.agent_id, a.hitl_expiration_action
		 FROM messages m
		 JOIN agent_identities a ON a.id = m.agent_id
		 WHERE m.status = 'pending_review' AND m.direction = 'outbound'
		   AND m.approval_expires_at < now()
		   AND a.deleted_at IS NULL
		 ORDER BY m.approval_expires_at ASC
		 LIMIT $1`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExpirationCandidate
	for rows.Next() {
		var c ExpirationCandidate
		if err := rows.Scan(&c.MessageID, &c.AgentID, &c.ExpirationAction); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ExpireApproveAndSend is the worker-side counterpart to ApproveAndSend:
// no user ownership check (the caller is the expiration worker, which is
// system-scoped), SELECT ... FOR NO KEY UPDATE SKIP LOCKED so concurrent
// workers don't race for the same row, and the terminal status is
// 'review_expired_approved' instead of 'sent'. On send failure the transaction
// rolls back; the worker should then call ExpireReject to move the row
// to a final state so the row doesn't get picked up on every sweep.
//
// Exactly-once guarantee: like ApproveAndSend, this method runs the
// send() callback under a send_attempts gate so a crash between SES
// acceptance and the surrounding tx commit does NOT cause the next
// worker poll to re-send. ClaimSendAttempt / MarkSendSucceeded /
// MarkSendFailed run in separate small transactions that outlive the
// approval tx; on retry, an AlreadySent verdict reuses the cached
// SendResult and skips the upstream send entirely. Without this, the
// polling-loop nature of the worker would guarantee a re-send on any
// commit failure — strictly worse than the human-approval path,
// where a re-send needs an explicit click.
//
// SKIP LOCKED means multiple app instances can run the worker without
// contending on the same row. The row-level FOR NO KEY UPDATE lock on
// messages is held for the duration of the send callback (bounded by
// SMTPRelay timeouts); FOR NO KEY UPDATE rather than FOR UPDATE so
// the send_attempts INSERT in a separate connection can acquire its
// KEY SHARE lock for FK enforcement — see ApproveAndSend's docstring
// for the full rationale.
//
// If a concurrent worker is mid-send for the same row (the
// send_attempts row is 'attempting' and not yet stale), returns
// ErrSendInProgress. The worker loop should treat this like
// ErrNotPendingApproval — skip silently and let the next poll handle
// it.
func (s *Store) ExpireApproveAndSend(
	ctx context.Context,
	messageID string,
	send func(msg *Message) (SendResult, error),
) (*Message, error) {
	txCtx, cancel := context.WithTimeout(ctx, approvalTxTimeout)
	defer cancel()

	tx, err := s.pool.Begin(txCtx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(txCtx)
		}
	}()

	var (
		m                  Message
		bodyText, bodyHTML *string
		attachments        []byte
		method, msgType    *string
		approvalExpires    *time.Time
	)
	err = tx.QueryRow(txCtx,
		`SELECT id, agent_id, direction, sender, recipient, subject,
		        email_message_id,
		        method, message_type,
		        conversation_id, created_at, expires_at,
		        to_recipients, cc, bcc, reply_to,
		        status, approval_expires_at, edited,
		        body_text, body_html, attachments_json
		 FROM messages
		 WHERE id = $1
		   AND direction = 'outbound'
		   AND status = 'pending_review'
		   AND approval_expires_at < now()
		   AND NOT EXISTS (SELECT 1 FROM agent_identities ai
		                    WHERE ai.id = messages.agent_id AND ai.deleted_at IS NOT NULL)
		 FOR NO KEY UPDATE SKIP LOCKED`,
		messageID,
	).Scan(
		&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.Recipient, &m.Subject,
		&m.EmailMessageID,
		&method, &msgType,
		&m.ConversationID, &m.CreatedAt, &m.ExpiresAt,
		&m.ToRecipients, &m.CC, &m.BCC, &m.ReplyTo,
		&m.Status, &approvalExpires, &m.Edited,
		&bodyText, &bodyHTML, &attachments,
	)
	if err != nil {
		// Row is either gone, no longer pending, not yet expired, or is
		// currently locked by another worker. Any of those means "someone
		// else will handle it, or nothing to do" — don't bubble as an error.
		return nil, ErrNotPendingApproval
	}
	if method != nil {
		m.Method = *method
	}
	if msgType != nil {
		m.Type = *msgType
	}
	if approvalExpires != nil {
		m.ApprovalExpiresAt = approvalExpires
	}
	if bodyText != nil {
		m.BodyText = *bodyText
	}
	if bodyHTML != nil {
		m.BodyHTML = *bodyHTML
	}
	if len(attachments) > 0 {
		m.AttachmentsJSON = json.RawMessage(attachments)
	}

	// Exactly-once gate, identical to ApproveAndSend's bracket. Runs
	// OUTSIDE this approval tx so the SES outcome survives an approval
	// tx rollback. See ApproveAndSend's docstring for the full
	// rationale.
	claim, err := s.ClaimSendAttempt(ctx, messageID)
	if err != nil {
		return nil, err
	}

	var result SendResult
	switch claim.Outcome {
	case SendAttemptAcquired:
		result, err = send(&m)
		if err != nil {
			if markErr := s.MarkSendFailed(ctx, messageID, err.Error()); markErr != nil {
				log.Printf("[expire] MarkSendFailed for %s: %v", messageID, markErr)
			}
			return nil, err
		}
		if markErr := s.MarkSendSucceededWithRetry(messageID, result); markErr != nil {
			log.Printf("[expire] MarkSendSucceeded exhausted retries for %s: %v (manual reconciliation may be needed)", messageID, markErr)
		}
	case SendAttemptAlreadySent:
		// A prior auto-approve attempt succeeded at SES but its
		// approval tx rolled back. Reuse the recorded result and
		// skip the upstream send.
		result = claim.Sent
	case SendAttemptInFlight:
		return nil, ErrSendInProgress
	}

	_, err = tx.Exec(txCtx,
		`UPDATE messages
		    SET status            = $2,
		        provider_message_id = $3,
		        method            = $4,
		        to_recipients     = $5,
		        cc                = $6,
		        bcc               = $7,
		        recipient         = $8,
		        reviewed_at       = now(),
		        raw_message       = $9::bytea,
		        rfc_message_id_key = CASE
		          WHEN rfc_message_id_key IS NULL AND $10 <> '' THEN $10
		          ELSE rfc_message_id_key
		        END
		  WHERE id = $1`,
		messageID,
		MessageStatusReviewExpiredApproved,
		result.ProviderMessageID,
		result.Method,
		result.To,
		result.CC,
		result.BCC,
		firstOr(result.To, ""),
		nullIfEmptyBytes(result.Raw),
		canonicalRFCMessageIDKey(result.ProviderMessageID),
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(txCtx); err != nil {
		return nil, err
	}
	committed = true

	m.Status = MessageStatusReviewExpiredApproved
	m.ProviderMessageID = result.ProviderMessageID
	m.Method = result.Method
	m.ToRecipients = result.To
	m.CC = result.CC
	m.BCC = result.BCC
	if len(result.To) > 0 {
		m.Recipient = result.To[0]
	}
	now := time.Now()
	m.ReviewedAt = &now
	return &m, nil
}

// AcceptedSend carries the composed, ready-to-submit values for an approved HITL
// message being handed to the async outbound queue (QueueOutbound) instead of being
// sent inline. Populated by the caller from outbound.ComposeResult.
type AcceptedSend struct {
	To, CC, BCC  []string
	Subject      string
	Method       string
	EnvelopeFrom string
	SentAs       string
	Raw          []byte
}

// ApproveAndAccept resolves a pending_review outbound hold to an APPROVED,
// ASYNC-QUEUED state in one transaction, mirroring the API's async accept-tx: it
// flips status to targetStatus (review_approved for a human approve,
// review_expired_approved for the TTL sweep) AND delivery_status to 'accepted',
// persists the composed bytes + envelope, then enqueues the outbound_send job and
// stamps its id. The existing SendWorker picks the row up by id and performs the
// actual SMTP submit + email.sent/failed + metering; this method does NOT send and
// does NOT use the send_attempts gate (async idempotency is the accept-tx atomicity
// + the worker's delivery_status/alreadyDone guard). reviewedByUserID is "" (→ NULL)
// for the sweep. When completeIdempotency is non-nil, it runs after the send job
// is stamped but before commit; an error rolls back the approval, job, and key.
//
// The WHERE status='pending_review' is the compare-and-set guard: RETURNING no row
// means a human/other worker already resolved the hold → ErrNotPendingApproval (a
// no-op for the caller). The agent-not-trashed guard sits in the same WHERE so it
// is atomic with the CAS: a hold whose agent was trashed after the caller's
// pre-checks (the TTL sweep's candidate SELECT, or a human path's ownership load)
// must NOT resolve — trashed holds stay pending_review with their clock paused
// until RestoreAgent shifts approval_expires_at (or the trash purge drops them).
// Draft body and attachment columns remain retained after approval.
//
// Scheduling survives the hold (#815): the CAS RETURNING reads back the draft's
// own scheduled_at (never modified by this UPDATE, so this is snapshot-safe — not
// a data-modifying-CTE re-select). When it is still in the future, the send is
// re-armed via enqueueScheduled (River first-run = scheduled_at) instead of the
// immediate enqueue, and the returned Message carries ScheduledAt so the caller
// renders status=scheduled. A scheduled_at that has already passed by approval
// time falls through to the immediate enqueue — "not before" is satisfied and
// approval was the last blocker.
func (s *Store) ApproveAndAccept(
	ctx context.Context,
	messageID, reviewedByUserID, targetStatus string,
	edited bool,
	acc AcceptedSend,
	enqueue func(ctx context.Context, tx pgx.Tx, messageID string) (int64, error),
	enqueueScheduled func(ctx context.Context, tx pgx.Tx, messageID string, at time.Time) (int64, error),
	completeIdempotency func(ctx context.Context, tx pgx.Tx, approved *Message) error,
) (*Message, error) {
	var reviewReason messagelifecycle.ReasonCode
	switch targetStatus {
	case MessageStatusSent, MessageStatusReviewApproved:
		reviewReason = messagelifecycle.ReasonReviewApproved
	case MessageStatusReviewExpiredApproved:
		reviewReason = messagelifecycle.ReasonReviewExpiredApproved
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidApprovalTarget, targetStatus)
	}
	var out *Message
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var m Message
		var msgType *string
		err := tx.QueryRow(ctx,
			`UPDATE messages
			    SET status              = $2,
			        delivery_status     = 'accepted',
			        to_recipients       = $3,
			        cc                  = $4,
			        bcc                 = $5,
			        subject             = $6,
			        recipient           = $7,
			        method              = $8,
			        envelope_from       = $9,
			        sent_as             = $10,
			        raw_message         = $11::bytea,
			        provider_message_id = '',
			        reviewed_at         = now(),
			        reviewed_by_user_id = $12,
			        edited              = $13
			  WHERE id = $1 AND direction = 'outbound' AND status = 'pending_review'
			    AND NOT EXISTS (SELECT 1 FROM agent_identities ai
			                     WHERE ai.id = messages.agent_id AND ai.deleted_at IS NOT NULL)
			  RETURNING id, agent_id, message_type, subject, to_recipients, cc, bcc, status, edited, scheduled_at`,
			messageID,
			targetStatus,
			acc.To, acc.CC, acc.BCC,
			acc.Subject,
			firstOr(acc.To, ""),
			acc.Method,
			acc.EnvelopeFrom,
			acc.SentAs,
			nullIfEmptyBytes(acc.Raw),
			nullIfEmptyString(reviewedByUserID),
			edited,
		).Scan(&m.ID, &m.AgentID, &msgType, &m.Subject, &m.ToRecipients, &m.CC, &m.BCC, &m.Status, &m.Edited, &m.ScheduledAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotPendingApproval
		}
		if err != nil {
			return err
		}
		if msgType != nil {
			m.Type = *msgType
		}
		transition, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
			MessageID: m.ID, DedupeKey: "review:resolution", Direction: "outbound",
			ReasonCode: reviewReason, Evidence: map[string]any{"review_resolution": targetStatus}, OccurredAt: time.Now(),
		})
		if err != nil {
			return err
		}
		m.LifecycleTransitions = []messagelifecycle.MessageLifecycleTransition{transition}
		// Re-arm a preserved schedule (#815): a held draft's future send_at is
		// honored by enqueueing the send to run no earlier than scheduled_at. A
		// scheduled_at already in the past submits immediately (the immediate enqueue
		// below), exactly like a directly-scheduled row whose instant has arrived —
		// scheduled_at stays on the row as the historical marker in both cases.
		// enqueueScheduled may be nil in setups without the scheduled arm wired — fall
		// back to immediate.
		var jobID int64
		if m.ScheduledAt != nil && m.ScheduledAt.After(time.Now()) && enqueueScheduled != nil {
			jobID, err = enqueueScheduled(ctx, tx, m.ID, *m.ScheduledAt)
		} else {
			jobID, err = enqueue(ctx, tx, m.ID)
		}
		if err != nil {
			return err
		}
		if err := s.StampSendJobIDTx(ctx, tx, m.ID, jobID); err != nil {
			return err
		}
		m.Direction = "outbound"
		m.DeliveryStatus = "accepted"
		// Surface the composed envelope on the returned row so the approve view +
		// review_approved event report method/sent_as (like the sync path). The
		// provider_message_id stays empty — the SendWorker fills it on email.sent.
		m.Method = acc.Method
		m.SentAs = acc.SentAs
		if completeIdempotency != nil {
			if err := completeIdempotency(ctx, tx, &m); err != nil {
				return err
			}
		}
		out = &m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LoadOutboundDraft loads a pending_review outbound message's full draft content
// (recipients, subject, body, attachments, reply-to) by id, system-scoped (no user
// filter, no side effects) — the TTL sweep uses it to reconstruct the SendRequest
// and compose before ApproveAndAccept. Returns ErrMessageNotFound if the row is
// gone or not an outbound message. The caller must still handle the pending_review
// CAS in ApproveAndAccept (a human may resolve the hold before the transition).
func (s *Store) LoadOutboundDraft(ctx context.Context, messageID string) (*Message, error) {
	m := &Message{ID: messageID, Direction: "outbound"}
	var bodyText, bodyHTML *string
	var attachments []byte
	var msgType *string
	err := s.pool.QueryRow(ctx,
		`SELECT agent_id, sender, subject, email_message_id, message_type, conversation_id,
		        to_recipients, cc, bcc, reply_to, status, body_text, body_html, attachments_json, managed_unsubscribe
		   FROM messages WHERE id=$1 AND direction='outbound'`,
		messageID,
	).Scan(&m.AgentID, &m.Sender, &m.Subject, &m.EmailMessageID, &msgType, &m.ConversationID,
		&m.ToRecipients, &m.CC, &m.BCC, &m.ReplyTo, &m.Status, &bodyText, &bodyHTML, &attachments, &m.ManagedUnsubscribe)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, err
	}
	if msgType != nil {
		m.Type = *msgType
	}
	if bodyText != nil {
		m.BodyText = *bodyText
	}
	if bodyHTML != nil {
		m.BodyHTML = *bodyHTML
	}
	if len(attachments) > 0 {
		m.AttachmentsJSON = json.RawMessage(attachments)
	}
	return m, nil
}

// ExpireReject transitions a pending_review message to review_expired_rejected
// while retaining its content. No user ownership check — this is the worker
// path. If the row is no longer pending (racing worker, already handled),
// or the agent was moved to the trash after the sweep listed the row (the
// hold must survive intact, and
// RestoreAgent shifts approval_expires_at so the clock resumes on restore),
// returns ErrNotPendingApproval; caller can treat as a no-op. The trash
// guard lives INSIDE the CAS so it is atomic with the transition — a
// separate read would reopen the sweep's TOCTOU window.
func (s *Store) ExpireReject(ctx context.Context, messageID, reason string) (*Message, error) {
	var transition messagelifecycle.MessageLifecycleTransition
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE messages
		    SET status = $2,
		        rejection_reason = $3,
		        reviewed_at = now()
		  WHERE id = $1 AND status = 'pending_review' AND direction = 'outbound'
		    AND NOT EXISTS (SELECT 1 FROM agent_identities ai
		                     WHERE ai.id = messages.agent_id AND ai.deleted_at IS NOT NULL)`,
			messageID, MessageStatusReviewExpiredRejected, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotPendingApproval
		}
		transition, err = messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
			MessageID: messageID, DedupeKey: "review:resolution", Direction: "outbound",
			ReasonCode: messagelifecycle.ReasonReviewExpiredRejected,
			Evidence:   map[string]any{"review_resolution": MessageStatusReviewExpiredRejected}, OccurredAt: time.Now(),
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	// Read back with ownership skipped — the worker doesn't have a userID.
	m := &Message{}
	var (
		method, msgType   *string
		approvalExpiresAt *time.Time
		reviewedAt        *time.Time
		rejectionReason   *string
	)
	err = s.pool.QueryRow(ctx,
		`SELECT id, agent_id, direction, subject, email_message_id,
		        method, message_type,
		        conversation_id, created_at, expires_at,
		        to_recipients, cc, bcc,
		        status, approval_expires_at, reviewed_at,
		        rejection_reason, edited,
		        COALESCE(body_text, ''), COALESCE(body_html, ''), attachments_json
		 FROM messages WHERE id = $1`, messageID,
	).Scan(
		&m.ID, &m.AgentID, &m.Direction, &m.Subject, &m.EmailMessageID,
		&method, &msgType,
		&m.ConversationID, &m.CreatedAt, &m.ExpiresAt,
		&m.ToRecipients, &m.CC, &m.BCC,
		&m.Status, &approvalExpiresAt, &reviewedAt,
		&rejectionReason, &m.Edited,
		&m.BodyText, &m.BodyHTML, &m.AttachmentsJSON,
	)
	if err != nil {
		return nil, err
	}
	if method != nil {
		m.Method = *method
	}
	if msgType != nil {
		m.Type = *msgType
	}
	m.ApprovalExpiresAt = approvalExpiresAt
	m.ReviewedAt = reviewedAt
	if rejectionReason != nil {
		m.RejectionReason = *rejectionReason
	}
	m.LifecycleTransitions = []messagelifecycle.MessageLifecycleTransition{transition}
	return m, nil
}

// RejectPending transitions a pending_review message to rejected, records the
// reviewer's reason (empty string allowed), and retains outbound content.
// Ownership checked; missing
// rows return ErrMessageNotFound. Non-pending rows return ErrNotPendingApproval.
func (s *Store) RejectPending(ctx context.Context, messageID, userID, reason string) (*Message, error) {
	// Single atomic UPDATE with status guard. We distinguish "not found" from
	// "not pending" with a follow-up existence check only when rows-affected
	// is 0.
	var rowsAffected int64
	var transition messagelifecycle.MessageLifecycleTransition
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE messages
		    SET status = $3,
		        rejection_reason = $4,
		        reviewed_at = now(),
		        reviewed_by_user_id = $2
		  WHERE id = $1
		    AND status = 'pending_review'
		    AND direction = 'outbound'
		    AND agent_id IN (SELECT id FROM agent_identities WHERE user_id = $2 AND deleted_at IS NULL)`,
			messageID, userID, MessageStatusReviewRejected, reason)
		if err != nil {
			return err
		}
		rowsAffected = tag.RowsAffected()
		if rowsAffected == 0 {
			return nil
		}
		transition, err = messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
			MessageID: messageID, DedupeKey: "review:resolution", Direction: "outbound",
			ReasonCode: messagelifecycle.ReasonReviewRejected,
			Evidence:   map[string]any{"review_resolution": MessageStatusReviewRejected}, OccurredAt: time.Now(),
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if rowsAffected == 0 {
		// Figure out why: missing, not owned, or not pending.
		var status string
		err := s.pool.QueryRow(ctx,
			`SELECT m.status
			 FROM messages m
			 JOIN agent_identities a ON a.id = m.agent_id
			 WHERE m.id = $1 AND a.user_id = $2`,
			messageID, userID,
		).Scan(&status)
		if err != nil {
			return nil, ErrMessageNotFound
		}
		return nil, ErrNotPendingApproval
	}
	m, err := s.GetOutboundMessageForUser(ctx, messageID, userID)
	if err == nil {
		m.LifecycleTransitions = []messagelifecycle.MessageLifecycleTransition{transition}
	}
	return m, err
}

func (s *Store) ListActivityByAgent(ctx context.Context, agentID string, limit int) ([]Message, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.agent_id, m.direction, m.sender, COALESCE(m.header_from, ''), COALESCE(m.envelope_from, ''), m.authentication, m.recipient, m.subject, m.email_message_id, COALESCE(m.method, ''), COALESCE(m.message_type, ''), COALESCE(m.inbox_status, ''), m.created_at, m.expires_at,
		        COALESCE(wd.status, ''), COALESCE(wd.last_error, ''), COALESCE(wd.attempts, 0),
		        m.to_recipients, m.cc, m.bcc,
		        COALESCE(m.conversation_id, ''), COALESCE(octet_length(m.raw_message), 0),
		        m.labels,
		        COALESCE(m.delivery_status, ''), COALESCE(m.delivery_detail, ''), COALESCE(m.sent_as, ''), m.auth_verdict,
		        COALESCE(m.flagged, false), COALESCE(m.flag_reason, '')
		 FROM messages m
		 LEFT JOIN webhook_deliveries wd ON wd.message_id = m.id
		 WHERE m.agent_id = $1
		   AND m.deleted_at IS NULL
		   AND NOT (m.direction = 'inbound' AND m.status IN (`+heldInboundStatuses+`))
		 ORDER BY m.created_at DESC
		 LIMIT $2`, agentID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var inboxStatus, outboundDeliveryStatus string
		var authentication, authVerdict []byte
		if err := rows.Scan(&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.HeaderFrom, &m.EnvelopeFrom, &authentication, &m.Recipient, &m.Subject, &m.EmailMessageID, &m.Method, &m.Type, &inboxStatus, &m.CreatedAt, &m.ExpiresAt, &m.WebhookStatus, &m.WebhookError, &m.WebhookAttempts, &m.ToRecipients, &m.CC, &m.BCC, &m.ConversationID, &m.SizeBytes, &m.Labels, &outboundDeliveryStatus, &m.DeliveryDetail, &m.SentAs, &authVerdict, &m.Flagged, &m.FlagReason); err != nil {
			return nil, err
		}
		if err := unmarshalAuthVerdict(authVerdict, &m); err != nil {
			return nil, err
		}
		if err := unmarshalAuthentication(authentication, authVerdict, &m); err != nil {
			return nil, err
		}
		// DeliveryStatus is overloaded by direction (see Message.DeliveryStatus):
		// inbound carries inbox_status, outbound carries the delivery rollup.
		m.InboxStatus = inboxStatus
		if m.Direction == "outbound" {
			m.DeliveryStatus = outboundDeliveryStatus
		} else {
			m.DeliveryStatus = inboxStatus
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// escapeLikePattern escapes the three SQL LIKE/ILIKE metacharacters
// (%, _, \) by prefixing them with backslash. Callers pair the
// returned pattern with `ESCAPE '\'` in the SQL fragment so the
// driver treats backslash as the escape char.
//
// This is NOT for SQL injection protection — pgx parameter binding
// already handles that — it's for "user-typed substring search,
// not glob". Without this, `?from=foo_bar` would match `fooXbar`,
// and `?from=%@acme.com` would match every row in the table.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func escapeLikePattern(s string) string {
	return likeEscaper.Replace(s)
}

// MessageListFilter bundles the params for GetMessagesByAgent. Zero
// values on the optional substring / time / ID filters mean "no
// constraint" — callers omit what they don't want to filter on.
type MessageListFilter struct {
	AgentID    string
	Status     string // "unread" | "read" | "all"
	Direction  string // "inbound" | "outbound" | "all"
	Descending bool
	Limit      int
	AfterTime  time.Time
	AfterID    string
	// Optional search filters. Empty / zero means "no constraint".
	// From / SubjectContains are case-insensitive substring matches
	// (Postgres ILIKE) and bound to 200 chars at the handler layer.
	From            string
	SubjectContains string
	ConversationID  string    // exact match
	Since           time.Time // created_at >= Since
	Until           time.Time // created_at <  Until
	// Labels filters rows where ALL given labels are present on the
	// message (AND-match via Postgres @> array containment). Empty slice
	// means "no label constraint" — matches both labelled and unlabelled
	// rows. Handler-layer validates each entry against the same charset
	// rule used on writes so callers can't smuggle SQL through here.
	Labels []string
	// Filter is a parsed and registry-validated expression. The store emits it
	// after built-in filters so placeholder numbering remains contiguous.
	Filter *filterquery.Expr
	// Deleted flips the query to the TRASH view: only soft-deleted rows.
	// False (default) lists indefinitely retained live rows only.
	Deleted bool
}

// GetMessagesByAgent returns messages for an agent, filtered by status,
// direction, and the optional search filters on the MessageListFilter
// struct.
//
//   - direction: "inbound" (default for SDK polling), "outbound", or "all"
//     (used by the dashboard inbox).
//   - status: "unread" | "read" | "all" — only applies when direction
//     selects inbound rows; ignored on pure outbound queries.
//   - descending: cursor walks newest→oldest when true; oldest→newest
//     when false (FIFO polling).
//   - From / SubjectContains: case-insensitive substring (ILIKE).
//   - ConversationID: exact match.
//   - Since / Until: time-range bracket on created_at.
//
// The SELECT includes columns both consumers need: the inbox needs
// `status` (outbound HITL lifecycle), `webhook_status`/`last_error`
// (outbound delivery), and `octet_length(raw_message)` (size column);
// the polling SDK ignores these fields and reads only the existing
// inbound-relevant ones from the Message struct. Canonical identity and
// authentication evidence is selected so the WS drain fallback matches live
// delivery; raw_message stays deliberately unselected (the blob). Legacy auth
// columns remain selected only for fail-closed migration fallback.
func (s *Store) GetMessagesByAgent(ctx context.Context, f MessageListFilter) ([]Message, error) {
	var query string
	var args []interface{}

	baseSelect := `SELECT m.id, m.agent_id, m.direction, m.sender, COALESCE(m.header_from, ''), COALESCE(m.envelope_from, ''), m.authentication, m.recipient, m.to_recipients, m.cc, m.reply_to, m.subject, m.email_message_id, COALESCE(m.method, ''), m.conversation_id, COALESCE(m.thread_id, ''), COALESCE(m.inbox_status, ''), COALESCE(m.status, ''), COALESCE(wd.status, ''), COALESCE(wd.last_error, ''), COALESCE(octet_length(m.raw_message), 0), m.created_at, m.deleted_at, m.labels, COALESCE(m.delivery_status, ''), COALESCE(m.delivery_detail, ''), COALESCE(m.sent_as, ''), m.auth_verdict, COALESCE(m.flagged, false), COALESCE(m.flag_reason, ''), m.auth_headers, m.scheduled_at
		 FROM messages m
		 LEFT JOIN webhook_deliveries wd ON wd.message_id = m.id
		 WHERE m.agent_id = $1`

	// Live view excludes trash. Trash view returns only soft-deleted rows.
	if f.Deleted {
		baseSelect += ` AND m.deleted_at IS NOT NULL`
	} else {
		baseSelect += ` AND m.deleted_at IS NULL`
	}

	switch f.Direction {
	case "outbound":
		query = baseSelect + ` AND m.direction = 'outbound'`
	case "all":
		query = baseSelect
	default: // "inbound" — default keeps SDK polling contract
		query = baseSelect + ` AND m.direction = 'inbound'`
	}

	// Inbound review holds are NOT delivered — exclude them from the agent inbox
	// until approved (Slice 4b). pending_review (awaiting a human), review_rejected
	// (blocked / human-rejected), and review_expired_rejected (TTL-dropped) stay
	// hidden; review_approved / review_expired_approved (and plain 'sent') are
	// delivered and shown. The clause is direction-aware so outbound rows
	// (pending_review etc.) are unaffected.
	switch f.Direction {
	case "outbound":
		// no inbound rows in the result set
	case "all":
		query += ` AND (m.direction = 'outbound' OR m.status NOT IN (` + heldInboundStatuses + `))`
	default: // inbound
		query += ` AND m.status NOT IN (` + heldInboundStatuses + `)`
	}

	// Inbox status filter only applies when inbound rows are in the
	// result set. Silently ignored for pure outbound queries — the
	// handler validates 400 on bad combinations before reaching here.
	if f.Direction != "outbound" {
		switch f.Status {
		case "all":
			// no extra clause
		case "read":
			query += ` AND m.inbox_status = 'read'`
		default: // "unread"
			if f.Direction == "inbound" {
				query += ` AND m.inbox_status = 'unread'`
			}
			// For direction='all', "unread" would silently drop every
			// outbound row (they have no inbox_status). That's a footgun
			// the dashboard never invokes — it always passes status="all"
			// when direction="all" — so we don't filter here.
		}
	}

	args = append(args, f.AgentID)

	// Optional search filters — each appends one arg and one WHERE
	// clause. Ordering matches the docstring so a code reader can
	// see at a glance which knobs map to which SQL fragment.
	//
	// ILIKE filters use ESCAPE '\' so the caller's literal `%`, `_`,
	// and `\` characters match themselves instead of acting as SQL
	// pattern wildcards. Without this, `?from=foo_bar` would also
	// match `fooXbar`, and `?from=%@acme.com` would match every row.
	// pgx parameter binding still protects against injection — this
	// is purely a "users expect substring search, not glob" fix.
	if f.From != "" {
		query += fmt.Sprintf(` AND m.sender ILIKE $%d ESCAPE '\'`, len(args)+1)
		args = append(args, "%"+escapeLikePattern(f.From)+"%")
	}
	if f.SubjectContains != "" {
		query += fmt.Sprintf(` AND m.subject ILIKE $%d ESCAPE '\'`, len(args)+1)
		args = append(args, "%"+escapeLikePattern(f.SubjectContains)+"%")
	}
	if f.ConversationID != "" {
		query += fmt.Sprintf(` AND m.conversation_id = $%d`, len(args)+1)
		args = append(args, f.ConversationID)
	}
	if !f.Since.IsZero() {
		query += fmt.Sprintf(` AND m.created_at >= $%d`, len(args)+1)
		args = append(args, f.Since)
	}
	if !f.Until.IsZero() {
		query += fmt.Sprintf(` AND m.created_at < $%d`, len(args)+1)
		args = append(args, f.Until)
	}
	if len(f.Labels) > 0 {
		// AND-match via @> array containment. The GIN index on labels
		// makes this O(log n) for the typical case (≤ 5 filter labels,
		// ≤ 100 labels per row). Empty caller-supplied labels are
		// stripped at the handler layer so we never produce
		// "labels @> ARRAY['']" which would match nothing.
		query += fmt.Sprintf(` AND m.labels @> $%d`, len(args)+1)
		args = append(args, f.Labels)
	}
	if f.Filter != nil {
		fragment, filterArgs, err := f.Filter.Emit(
			filterquery.PostgresDialect{},
			len(args)+1,
		)
		if err != nil {
			return nil, fmt.Errorf("emit message filter: %w", err)
		}
		if fragment != "" {
			query += " AND " + fragment
			args = append(args, filterArgs...)
		}
	}

	cursorCmp := ">"
	sortDir := "ASC"
	if f.Descending {
		cursorCmp = "<"
		sortDir = "DESC"
	}

	if f.AfterID != "" {
		query += fmt.Sprintf(` AND (m.created_at, m.id) %s ($%d, $%d)`, cursorCmp, len(args)+1, len(args)+2)
		args = append(args, f.AfterTime, f.AfterID)
	}

	query += fmt.Sprintf(` ORDER BY m.created_at %s, m.id %s LIMIT $%d`, sortDir, sortDir, len(args)+1)
	args = append(args, f.Limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var outboundDeliveryStatus string
		var authentication, authVerdict []byte
		var authHeadersJSON []byte
		if err := rows.Scan(
			&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.HeaderFrom, &m.EnvelopeFrom, &authentication, &m.Recipient, &m.ToRecipients, &m.CC, &m.ReplyTo,
			&m.Subject, &m.EmailMessageID, &m.Method, &m.ConversationID, &m.ThreadID,
			&m.InboxStatus, &m.Status, &m.WebhookStatus, &m.WebhookError, &m.SizeBytes,
			&m.CreatedAt, &m.DeletedAt, &m.Labels,
			&outboundDeliveryStatus, &m.DeliveryDetail, &m.SentAs, &authVerdict, &m.Flagged, &m.FlagReason,
			&authHeadersJSON, &m.ScheduledAt,
		); err != nil {
			return nil, err
		}
		if err := unmarshalAuthVerdict(authVerdict, &m); err != nil {
			return nil, err
		}
		if err := unmarshalAuthentication(authentication, authVerdict, &m); err != nil {
			return nil, err
		}
		if authHeadersJSON != nil {
			if err := json.Unmarshal(authHeadersJSON, &m.AuthHeaders); err != nil {
				return nil, fmt.Errorf("unmarshal auth headers: %w", err)
			}
		}
		// DeliveryStatus is overloaded by direction: inbound rows carry the
		// inbox read/unread status under the legacy JSON key (the polling SDK
		// reads it there); outbound rows carry the messages.delivery_status
		// rollup. A row is one direction, so the sources never collide.
		if m.Direction == "outbound" {
			m.DeliveryStatus = outboundDeliveryStatus
		} else {
			m.DeliveryStatus = m.InboxStatus
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// GetMessageWithContent returns a full message including raw MIME and canonical
// authentication evidence.
// Marks the message as 'read' if it was 'unread'.
//
// Unlike the list/reply/threading paths, this deliberately returns TRASHED
// rows too (DeletedAt set) so
// the dashboard trash can open a deleted message — Gmail's "view message in
// trash". Callers branch on DeletedAt when trash rows must not qualify.
func (s *Store) GetMessageWithContent(ctx context.Context, messageID, agentID string) (*Message, error) {
	return getMessageWithContent(ctx, s.pool, messageID, agentID)
}

// getMessageWithContent is GetMessageWithContent parameterized by executor, so
// a write path (RestoreMessage) can produce its own authoritative post-write
// view from INSIDE its transaction instead of re-reading from the pool after
// the commit. The CTE is snapshot-safe: its outer SELECT reads the CTE's own
// RETURNING output (`FROM upd`), never the modified table.
func getMessageWithContent(ctx context.Context, exec agentRowQuerier, messageID, agentID string) (*Message, error) {
	m := &Message{}
	var authHeadersJSON []byte
	var authentication, authVerdict []byte
	var outboundDeliveryStatus string
	// CTE so the read-marking UPDATE can still LEFT JOIN webhook_deliveries —
	// the detail view is a superset of the summary view, so it must carry the
	// same webhook_status/webhook_error the list exposes. Mirrors the
	// wd.status/wd.last_error JOIN used by GetMessagesByAgent/GetConversationByID.
	err := exec.QueryRow(ctx,
		`WITH upd AS (
		   UPDATE messages SET inbox_status = CASE WHEN inbox_status = 'unread' THEN 'read' ELSE inbox_status END
		   WHERE id = $1 AND agent_id = $2
		     AND NOT (direction = 'inbound' AND status IN (`+heldInboundStatuses+`))
		   RETURNING id, agent_id, direction, sender, COALESCE(header_from, '') AS header_from, COALESCE(envelope_from, '') AS envelope_from, authentication, recipient, to_recipients, cc, reply_to, subject, email_message_id, conversation_id, COALESCE(thread_id, '') AS thread_id, COALESCE(thread_parent_id, '') AS thread_parent_id, COALESCE(rfc_message_id_key, '') AS rfc_message_id_key, COALESCE(inbox_status, '') AS inbox_status, raw_message, auth_headers, auth_verdict, COALESCE(flagged, false) AS flagged, COALESCE(flag_reason, '') AS flag_reason, created_at, expires_at, deleted_at, labels, COALESCE(delivery_status, '') AS delivery_status, COALESCE(delivery_detail, '') AS delivery_detail, COALESCE(sent_as, '') AS sent_as, COALESCE(body_text, '') AS body_text, COALESCE(body_html, '') AS body_html, COALESCE(status, '') AS status, COALESCE(method, '') AS method, scheduled_at
		 )
		 SELECT upd.id, upd.agent_id, upd.direction, upd.sender, upd.header_from, upd.envelope_from, upd.authentication, upd.recipient, upd.to_recipients, upd.cc, upd.reply_to, upd.subject, upd.email_message_id, upd.conversation_id, upd.thread_id, upd.thread_parent_id, upd.rfc_message_id_key, upd.inbox_status, upd.raw_message, upd.auth_headers, upd.auth_verdict, upd.flagged, upd.flag_reason, upd.created_at, upd.expires_at, upd.deleted_at, upd.labels, upd.delivery_status, upd.delivery_detail, upd.sent_as, upd.body_text, upd.body_html, upd.status, upd.method, upd.scheduled_at, COALESCE(wd.status, ''), COALESCE(wd.last_error, '')
		 FROM upd LEFT JOIN webhook_deliveries wd ON wd.message_id = upd.id`,
		messageID, agentID,
	).Scan(&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.HeaderFrom, &m.EnvelopeFrom, &authentication, &m.Recipient, &m.ToRecipients, &m.CC, &m.ReplyTo, &m.Subject, &m.EmailMessageID, &m.ConversationID, &m.ThreadID, &m.ThreadParentID, &m.RFCMessageIDKey, &m.InboxStatus, &m.RawMessage, &authHeadersJSON, &authVerdict, &m.Flagged, &m.FlagReason, &m.CreatedAt, &m.ExpiresAt, &m.DeletedAt, &m.Labels, &outboundDeliveryStatus, &m.DeliveryDetail, &m.SentAs, &m.BodyText, &m.BodyHTML, &m.Status, &m.Method, &m.ScheduledAt, &m.WebhookStatus, &m.WebhookError)
	if err != nil {
		return nil, err
	}
	// raw_message is loaded on the detail path, so size derives from it directly
	// (the summary path uses octet_length in SQL since it never loads the blob).
	m.SizeBytes = len(m.RawMessage)
	// DeliveryStatus is overloaded by direction (see Message.DeliveryStatus):
	// inbound carries inbox_status, outbound carries the delivery rollup.
	if m.Direction == "outbound" {
		m.DeliveryStatus = outboundDeliveryStatus
	} else {
		m.DeliveryStatus = m.InboxStatus
	}
	if authHeadersJSON != nil {
		if err := json.Unmarshal(authHeadersJSON, &m.AuthHeaders); err != nil {
			return nil, fmt.Errorf("unmarshal auth headers: %w", err)
		}
	}
	if err := unmarshalAuthVerdict(authVerdict, m); err != nil {
		return nil, err
	}
	if err := unmarshalAuthentication(authentication, authVerdict, m); err != nil {
		return nil, err
	}
	return m, nil
}

// ErrLabelLimitExceeded reports that an add operation would push a
// message past MaxLabelsPerMessage. Mapped to HTTP 400 at the handler.
var ErrLabelLimitExceeded = errors.New("label limit exceeded")

// MaxLabelsPerMessage is the post-add cap on the labels[] column. The
// per-operation cap (max items in add_labels / remove_labels) is
// enforced earlier at the handler. The two together bound the array
// at a size where GIN containment + JSON marshalling stay cheap.
const MaxLabelsPerMessage = 100

// ModifyMessageLabels applies a delta — add then remove — to a
// message's labels[] in a single atomic statement. Returns the updated
// labels (deduplicated, sorted) so the caller can echo them back in
// the response without a second round-trip.
//
// Inputs are assumed already normalized (lowercased, charset-validated,
// dedup'd within each list, e2a:* gated). The store layer:
//   - applies adds first, then removes (so a label in both lists ends up removed)
//   - rejects if the post-add total would exceed MaxLabelsPerMessage
//   - returns ErrMessageNotFound if the row is missing / trashed / cross-agent
//
// The whole thing runs as one UPDATE so a concurrent PATCH from a
// second client can't observe a partial state.
func (s *Store) ModifyMessageLabels(ctx context.Context, messageID, agentID string, add, remove []string) ([]string, error) {
	// Pre-check the post-add length against the cap. Done as a
	// dedicated SELECT-then-UPDATE so we can return a specific error
	// rather than a generic constraint violation — the handler maps
	// ErrLabelLimitExceeded to 400 with a useful message.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var current []string
	err = tx.QueryRow(ctx,
		`SELECT labels FROM messages WHERE id = $1 AND agent_id = $2 AND deleted_at IS NULL AND NOT (direction = 'inbound' AND status IN (`+heldInboundStatuses+`)) FOR UPDATE`,
		messageID, agentID,
	).Scan(&current)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, err
	}

	// Apply the delta in-memory so the cap check is exact. The set
	// semantics here mirror what the SQL UPDATE below does:
	// labels' = (labels ∪ add) \ remove.
	labelSet := map[string]struct{}{}
	for _, l := range current {
		labelSet[l] = struct{}{}
	}
	for _, l := range add {
		labelSet[l] = struct{}{}
	}
	for _, l := range remove {
		delete(labelSet, l)
	}
	if len(labelSet) > MaxLabelsPerMessage {
		return nil, ErrLabelLimitExceeded
	}

	final := make([]string, 0, len(labelSet))
	for l := range labelSet {
		final = append(final, l)
	}
	sort.Strings(final)

	if _, err := tx.Exec(ctx,
		`UPDATE messages SET labels = $1 WHERE id = $2 AND agent_id = $3 AND NOT (direction = 'inbound' AND status IN (`+heldInboundStatuses+`))`,
		final, messageID, agentID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if final == nil {
		final = []string{}
	}
	return final, nil
}

// UpdateMessageDeliveryStatus sets the inbox_status on a message.
func (s *Store) UpdateMessageDeliveryStatus(ctx context.Context, messageID, agentID, status string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE messages SET inbox_status = $1 WHERE id = $2 AND agent_id = $3 AND NOT (direction = 'inbound' AND status IN (`+heldInboundStatuses+`))`,
		status, messageID, agentID,
	)
	return err
}

// expiredDeleteBatch bounds one transaction in the janitor's batched sweeps.
// messages is prod-sized, so a single unbounded `DELETE ... WHERE expired` would take
// a long row-lock + emit a huge WAL burst on the first sweep of a backlog. Deleting
// in bounded chunks keeps each transaction small; the caller's ctx bounds total
// runtime (a partial sweep resumes next hour — the delete is idempotent). A var (not
// const) so tests can shrink it to exercise the multi-batch loop cheaply.
var expiredDeleteBatch int64 = 5000

// threadChildDetachBatch bounds the number of parent-pointer rows rewritten by
// one statement. DeleteExpiredMessages executes at most one such statement per
// transaction; a high-fanout parent remains in trash and is resumed by the next
// transaction until no children point at it. PurgeMessage keeps its stronger
// message-delete/job-cancellation atomicity by draining multiple bounded
// statements in its single caller transaction.
var threadChildDetachBatch int64 = 5000

// detachThreadChildrenBatchTx clears at most limit direct-parent pointers.
// ORDER BY makes overlapping callers acquire child locks deterministically.
// Periodic maintenance uses skipLocked so contention is resumed next sweep;
// explicit purge blocks because a successful synchronous delete cannot leave a
// surviving child pointing at the removed parent.
func detachThreadChildrenBatchTx(ctx context.Context, tx pgx.Tx, parentIDs []string, limit int64, skipLocked bool) (int64, error) {
	if len(parentIDs) == 0 || limit <= 0 {
		return 0, nil
	}
	lockClause := "FOR UPDATE"
	if skipLocked {
		lockClause += " SKIP LOCKED"
	}
	tag, err := tx.Exec(ctx,
		`WITH children AS (
		   SELECT id, thread_parent_id
		     FROM messages
		    WHERE thread_parent_id = ANY($1)
		    ORDER BY thread_parent_id, id
		    LIMIT $2
		    `+lockClause+`
		 )
		 UPDATE messages AS m
		    SET thread_parent_id = NULL
		   FROM children
		  WHERE m.id = children.id`,
		parentIDs, limit,
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredMessages purges message rows whose TrashRetention window has
// lapsed. Live rows are retained indefinitely. Messages belonging to an agent
// in trash are removed by PurgeDeletedAgents when that agent reaches the same
// retention boundary.
func (s *Store) DeleteExpiredMessages(ctx context.Context) (int64, error) {
	var total int64
	for {
		var selected, detached, deleted int64
		err := s.WithTx(ctx, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx,
				`SELECT m.id,
				        CASE WHEN m.direction = 'outbound'
				                   AND m.delivery_status IN ('accepted', 'sending')
				             THEN m.send_job_id END
				   FROM messages m
				  WHERE m.deleted_at IS NOT NULL
				    AND m.deleted_at <= now() - make_interval(secs => $2)
				  LIMIT $1
				  FOR UPDATE SKIP LOCKED`,
				expiredDeleteBatch, TrashRetention.Seconds())
			if err != nil {
				return err
			}
			defer rows.Close()

			messageIDs := make([]string, 0, expiredDeleteBatch)
			jobIDByMessage := make(map[string]int64)
			for rows.Next() {
				var messageID string
				var jobID *int64
				if err := rows.Scan(&messageID, &jobID); err != nil {
					return err
				}
				messageIDs = append(messageIDs, messageID)
				if jobID != nil {
					jobIDByMessage[messageID] = *jobID
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			rows.Close()
			if len(messageIDs) == 0 {
				return nil
			}
			selected = int64(len(messageIDs))
			// A hard-deleted parent cannot remain as a dangling internal pointer
			// on a surviving child. Only one bounded child batch is detached in
			// this transaction. Parents with remaining children stay in trash,
			// so a later transaction (or janitor run) can resume safely.
			detached, err = detachThreadChildrenBatchTx(
				ctx, tx, messageIDs, threadChildDetachBatch, true,
			)
			if err != nil {
				return err
			}

			deletableRows, err := tx.Query(ctx,
				`SELECT parent.id
				   FROM messages AS parent
				  WHERE parent.id = ANY($1)
				    AND NOT EXISTS (
				      SELECT 1
				        FROM messages AS child
				       WHERE child.thread_parent_id = parent.id
				    )
				  ORDER BY parent.id`,
				messageIDs,
			)
			if err != nil {
				return err
			}
			deletableIDs := make([]string, 0, len(messageIDs))
			deletableJobs := make([]int64, 0, len(jobIDByMessage))
			for deletableRows.Next() {
				var messageID string
				if err := deletableRows.Scan(&messageID); err != nil {
					deletableRows.Close()
					return err
				}
				deletableIDs = append(deletableIDs, messageID)
				if jobID, ok := jobIDByMessage[messageID]; ok {
					deletableJobs = append(deletableJobs, jobID)
				}
			}
			if err := deletableRows.Err(); err != nil {
				deletableRows.Close()
				return err
			}
			deletableRows.Close()
			if len(deletableIDs) == 0 {
				return nil
			}
			if err := s.cancelOutboundJobIDsTx(ctx, tx, deletableJobs); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `DELETE FROM messages WHERE id = ANY($1)`, deletableIDs)
			if err != nil {
				return err
			}
			deleted = tag.RowsAffected()
			return nil
		})
		if err != nil {
			return total, err
		}
		total += deleted
		switch {
		case selected == 0:
			return total, nil
		case detached == 0 && deleted == 0:
			// Every selected row is currently blocked by a surviving child
			// that SKIP LOCKED could not claim. Leave the parent intact and let
			// the next periodic sweep resume instead of spinning.
			return total, nil
		}
	}
}

// SoftDeleteMessage moves a live message to the trash. Idempotent on an
// already-trashed message (nil). A held message (status pending_review,
// either direction) cannot be trashed — the review queue is its resolution
// surface — and returns ErrMessageHeld. A missing message returns
// ErrMessageNotFound.
func (s *Store) SoftDeleteMessage(ctx context.Context, messageID, agentID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE messages SET deleted_at = now()
		  WHERE id = $1 AND agent_id = $2 AND deleted_at IS NULL
		    AND COALESCE(status, '') <> 'pending_review'
		    AND NOT (
		      COALESCE(delivery_status, '') = 'sending'
		      AND COALESCE(send_claimed_at > now() - make_interval(secs => $3), false)
		    )`,
		messageID, agentID, int64(OutboundSendClaimStaleWindow/time.Second),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return s.classifyTrashMiss(ctx, messageID, agentID, true)
	}
	return nil
}

// RestoreMessage brings an indefinitely retained trashed message back to the
// inbox and returns the restored message. Returns ErrNotInTrash when the
// message exists but is live, ErrMessageNotFound otherwise.
//
// The post-restore view is produced INSIDE the restore transaction. Reading it
// afterwards from the pool was a torn read: a concurrent re-trash or purge
// landing in the gap made a committed restore answer 500 "failed to reload
// message", and any concurrent mutation could describe the message by state
// this restore never produced. The in-transaction read follows the row lock
// this transaction already holds. The detail projection still marks an unread
// inbound message read, exactly as the handler's post-restore read did.
func (s *Store) RestoreMessage(ctx context.Context, messageID, agentID string) (*Message, error) {
	var restored *Message
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var deletedAt *time.Time
		var scheduledAt *time.Time
		var deliveryStatus string
		var sendJobID *int64
		err := tx.QueryRow(ctx,
			`SELECT deleted_at, scheduled_at,
			        COALESCE(delivery_status, ''), send_job_id
			   FROM messages
			  WHERE id = $1 AND agent_id = $2
			  FOR UPDATE`,
			messageID, agentID,
		).Scan(&deletedAt, &scheduledAt, &deliveryStatus, &sendJobID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMessageNotFound
		}
		if err != nil {
			return err
		}
		if deletedAt == nil {
			return ErrNotInTrash
		}
		var cutoff time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
			return err
		}
		if scheduledAt != nil && !scheduledAt.After(cutoff) &&
			deliveryStatus == "accepted" && sendJobID != nil {
			if err := s.cancelPastDueScheduledJobsTx(ctx, tx, []pastDueScheduledJob{{
				messageID: messageID,
				jobID:     *sendJobID,
			}}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE messages SET deleted_at = NULL WHERE id = $1`, messageID); err != nil {
			return err
		}
		restored, err = getMessageWithContent(ctx, tx, messageID, agentID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return restored, nil
}

// PurgeMessage permanently deletes a message that is already in the trash
// ("delete forever" — the Gmail journey is delete → trash → delete forever,
// so a live message must be trashed first). Returns ErrNotInTrash for a
// live message, ErrMessageNotFound otherwise.
func (s *Store) PurgeMessage(ctx context.Context, messageID, agentID string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var deletedAt *time.Time
		var deliveryStatus string
		var activeSend bool
		var sendJobID *int64
		err := tx.QueryRow(ctx,
			`SELECT deleted_at, COALESCE(delivery_status, ''),
			        COALESCE(send_claimed_at > now() - make_interval(secs => $3), false),
			        send_job_id
			   FROM messages
			  WHERE id = $1 AND agent_id = $2 FOR UPDATE`,
			messageID, agentID, int64(OutboundSendClaimStaleWindow/time.Second),
		).Scan(&deletedAt, &deliveryStatus, &activeSend, &sendJobID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMessageNotFound
		}
		if err != nil {
			return err
		}
		if deliveryStatus == "sending" && activeSend {
			return ErrSendInProgress
		}
		if deletedAt == nil {
			return ErrNotInTrash
		}
		if sendJobID != nil && (deliveryStatus == "accepted" || deliveryStatus == "sending") {
			if err := s.cancelOutboundJobIDsTx(ctx, tx, []int64{*sendJobID}); err != nil {
				return err
			}
		}
		for {
			detached, err := detachThreadChildrenBatchTx(
				ctx, tx, []string{messageID}, threadChildDetachBatch, false,
			)
			if err != nil {
				return err
			}
			if detached < threadChildDetachBatch {
				break
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM messages WHERE id = $1`, messageID)
		return err
	})
}

// classifyTrashMiss turns a zero-row trash mutation into the precise error:
// held → ErrMessageHeld (soft delete only), already-trashed soft delete →
// nil (idempotent), live restore/purge target → ErrNotInTrash, otherwise
// ErrMessageNotFound.
func (s *Store) classifyTrashMiss(ctx context.Context, messageID, agentID string, softDelete bool) error {
	var deletedAt *time.Time
	var status string
	var deliveryStatus string
	var activeSend bool
	err := s.pool.QueryRow(ctx,
		`SELECT deleted_at, COALESCE(status, ''), COALESCE(delivery_status, ''),
		        COALESCE(send_claimed_at > now() - make_interval(secs => $3), false)
		   FROM messages
		  WHERE id = $1 AND agent_id = $2`,
		messageID, agentID, int64(OutboundSendClaimStaleWindow/time.Second),
	).Scan(&deletedAt, &status, &deliveryStatus, &activeSend)
	if err != nil {
		return ErrMessageNotFound
	}
	if softDelete {
		if deletedAt != nil {
			return nil // already in the trash — idempotent
		}
		if deliveryStatus == "sending" && activeSend {
			return ErrSendInProgress
		}
		if status == "pending_review" {
			return ErrMessageHeld
		}
		return ErrMessageNotFound
	}
	if deletedAt == nil {
		return ErrNotInTrash
	}
	return ErrMessageNotFound
}

// LookupConversationID finds a conversation_id by matching In-Reply-To / References
// message IDs against stored messages. Checks both email_message_id (inbound) and
// provider_message_id (outbound). Uses prefix matching because SES bare IDs stored
// in provider_message_id (e.g. <010f...>) may lack the @region.amazonses.com suffix
// that appears in the actual email headers sent to recipients.
func (s *Store) LookupConversationID(ctx context.Context, agentID string, messageIDs []string) (string, error) {
	if len(messageIDs) == 0 {
		return "", fmt.Errorf("no message IDs to look up")
	}

	var conversationID string
	err := s.pool.QueryRow(ctx,
		`SELECT conversation_id FROM messages
		 WHERE agent_id = $1
		   AND conversation_id <> ''
		   AND (
		     email_message_id = ANY($2)
		     OR provider_message_id = ANY($2)
		     OR EXISTS (
		       SELECT 1 FROM unnest($2::text[]) AS lookup(id)
		       WHERE lookup.id LIKE REPLACE(provider_message_id, '>', '%')
		         AND provider_message_id <> ''
		     )
		   )
		 ORDER BY created_at DESC LIMIT 1`,
		agentID, messageIDs,
	).Scan(&conversationID)
	if err != nil {
		return "", err
	}
	return conversationID, nil
}

// --- Conversations (thin read layer over messages.conversation_id) ---
//
// A conversation is the set of messages an agent sees that share a
// non-empty conversation_id. The shape is computed at read time —
// there's no `conversations` table, no persistence cost on top of
// the existing messages row. The trade-off is that listing requires
// an aggregate query; the partial index from migration 022 keeps it
// cheap on large agents.
//
// All Conversation* methods scope by agent_id. Cross-agent
// conversations (a user owns two agents and uses the same
// conversation_id) are intentionally split — a conversation is an
// agent-level concept, not a user-level one. If we ever want a
// user-wide "all conversations" view it gets a separate endpoint
// (mirrors the agents+messages vs. user+pending split that already
// exists).

// ConversationSummary is one row in the list endpoint. Aggregated
// counts + the "latest message" preview fields are enough to render
// an inbox-style conversation list without a per-row drill-down.
//
// HasUnread is true iff at least one INBOUND member is in
// inbox_status='unread'. Outbound pending_review doesn't count —
// the conversation list is the agent's mailbox view, not the
// reviewer's HITL queue.
type ConversationSummary struct {
	ID             string    `json:"conversation_id"`
	LastMessageAt  time.Time `json:"last_message_at"`
	FirstMessageAt time.Time `json:"first_message_at"`
	MessageCount   int       `json:"message_count"`
	InboundCount   int       `json:"inbound_count"`
	OutboundCount  int       `json:"outbound_count"`
	HasUnread      bool      `json:"has_unread"`
	LatestSubject  string    `json:"latest_subject"`
	LatestSender   string    `json:"latest_sender"`
}

// ConversationDetail extends the summary with member messages and
// computed aggregates (participants set, label union). Messages are
// returned chronologically (oldest first) — the rendering convention
// for a thread view.
type ConversationDetail struct {
	ConversationSummary
	Participants []string  `json:"participants"`
	Labels       []string  `json:"labels"`
	Messages     []Message `json:"messages"`
}

// ConversationListFilter is the input to ListConversationsByAgent.
// Limit is capped to ConversationListHardCap at the storage layer
// regardless of what the caller passes; pagination is intentionally
// not in this slice (most agents have dozens of conversations, not
// thousands) and can be added cursor-style if a deployment needs it.
type ConversationListFilter struct {
	AgentID string
	Limit   int
	// Since / Until bracket the conversation's last_message_at —
	// "show me conversations that had activity in this window".
	// Zero values disable each bound.
	Since time.Time
	Until time.Time
	// After* is the keyset cursor position (CV-3): the previous page's last
	// row's (last_message_at, conversation_id). Zero AfterLastMessageAt = first
	// page. Pass Limit+1 to detect a further page.
	AfterLastMessageAt  time.Time
	AfterConversationID string
}

// ConversationListHardCap is the maximum number of conversations a
// single list call returns. Higher requests are silently clamped.
// 100 covers the inbox-style use case; a deployment that needs more
// can either ask for higher (we'll bump it) or paginate (slice 2).
const ConversationListHardCap = 100

// ListConversationsByAgent groups the agent's live messages
// by conversation_id and returns one row per conversation sorted by
// most-recent activity. Messages without a conversation_id are not
// included in any conversation — they remain individually visible
// via GetMessagesByAgent.
func (s *Store) ListConversationsByAgent(ctx context.Context, f ConversationListFilter) ([]ConversationSummary, error) {
	limit := f.Limit
	// Honor the caller's limit (the handler passes page-size+1 to detect a
	// further page); cap one above the hard cap so limit+1 at the cap still works.
	if limit <= 0 {
		limit = ConversationListHardCap
	} else if limit > ConversationListHardCap+1 {
		limit = ConversationListHardCap + 1
	}

	query := `
		SELECT
		  conversation_id,
		  MAX(created_at)                          AS last_message_at,
		  MIN(created_at)                          AS first_message_at,
		  COUNT(*)                                 AS message_count,
		  COUNT(*) FILTER (WHERE direction='inbound')  AS inbound_count,
		  COUNT(*) FILTER (WHERE direction='outbound') AS outbound_count,
		  -- BOOL_OR returns NULL when every row's expression is NULL
		  -- (e.g. all-outbound conversations where inbox_status is
		  -- NULL — the column is nullable). COALESCE to false so
		  -- the *bool scan never fails on legitimate edge cases.
		  COALESCE(BOOL_OR(direction='inbound' AND inbox_status='unread'), false) AS has_unread,
		  (ARRAY_AGG(COALESCE(subject, '') ORDER BY created_at DESC))[1] AS latest_subject,
		  (ARRAY_AGG(COALESCE(sender, '')  ORDER BY created_at DESC))[1] AS latest_sender
		FROM messages
		WHERE agent_id = $1
		  AND conversation_id <> ''
		  AND deleted_at IS NULL
		  AND NOT (direction = 'inbound' AND status IN (` + heldInboundStatuses + `))
		GROUP BY conversation_id`

	args := []interface{}{f.AgentID}
	var having []string
	if !f.Since.IsZero() {
		having = append(having, fmt.Sprintf(`MAX(created_at) >= $%d`, len(args)+1))
		args = append(args, f.Since)
	}
	if !f.Until.IsZero() {
		having = append(having, fmt.Sprintf(`MAX(created_at) < $%d`, len(args)+1))
		args = append(args, f.Until)
	}
	// Keyset cursor (CV-3): rows strictly after the cursor in (last_message_at,
	// conversation_id) DESC order. Applied in HAVING since last_message_at is an
	// aggregate.
	if !f.AfterLastMessageAt.IsZero() {
		i := len(args) + 1
		having = append(having, fmt.Sprintf(`(MAX(created_at) < $%d OR (MAX(created_at) = $%d AND conversation_id < $%d))`, i, i, i+1))
		args = append(args, f.AfterLastMessageAt, f.AfterConversationID)
	}
	if len(having) > 0 {
		query += ` HAVING ` + strings.Join(having, " AND ")
	}
	query += fmt.Sprintf(` ORDER BY MAX(created_at) DESC, conversation_id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConversationSummary
	for rows.Next() {
		var c ConversationSummary
		if err := rows.Scan(
			&c.ID, &c.LastMessageAt, &c.FirstMessageAt,
			&c.MessageCount, &c.InboundCount, &c.OutboundCount,
			&c.HasUnread, &c.LatestSubject, &c.LatestSender,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConversationByID returns the aggregate summary fields plus every
// member message, ordered oldest-first (chronological reading order).
// Returns ErrMessageNotFound when no live messages exist for
// the given (agentID, conversationID) — mirrors the
// "looks-like-not-found-on-cross-agent" convention used by single-
// message reads. The same code path handles "wrong agent" and "real
// non-existent": either way the agent has no business seeing it.
//
// Participants are computed as the union of sender + recipient +
// each row's to_recipients / cc / bcc (when populated). Empty
// strings are dropped. Labels are the union of all members'
// labels[]; both are sorted lexicographically for stable output.
func (s *Store) GetConversationByID(ctx context.Context, agentID, conversationID string) (*ConversationDetail, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.agent_id, m.direction, m.sender, COALESCE(m.header_from, ''), COALESCE(m.envelope_from, ''), m.authentication, m.recipient,
		        m.to_recipients, m.cc, m.bcc, m.reply_to,
		        m.subject, COALESCE(m.email_message_id, ''),
		        COALESCE(m.method, ''), COALESCE(m.message_type, ''),
		        m.conversation_id, COALESCE(m.inbox_status, ''),
		        COALESCE(m.status, ''),
		        m.created_at, m.expires_at,
		        m.labels,
		        COALESCE(m.delivery_status, ''), COALESCE(m.delivery_detail, ''), COALESCE(m.sent_as, ''), m.auth_verdict,
		        COALESCE(m.flagged, false), COALESCE(m.flag_reason, ''),
		        COALESCE(wd.status, ''), COALESCE(wd.last_error, ''), m.scheduled_at
		 FROM messages m
		 LEFT JOIN webhook_deliveries wd ON wd.message_id = m.id
		 WHERE m.agent_id = $1
		   AND m.conversation_id = $2
		   AND m.deleted_at IS NULL
		   AND NOT (m.direction = 'inbound' AND m.status IN (`+heldInboundStatuses+`))
		 ORDER BY m.created_at ASC, m.id ASC`,
		agentID, conversationID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	d := &ConversationDetail{}
	participantSet := map[string]struct{}{}
	labelSet := map[string]struct{}{}

	for rows.Next() {
		var m Message
		var outboundDeliveryStatus string
		var authentication, authVerdict []byte
		if err := rows.Scan(
			&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.HeaderFrom, &m.EnvelopeFrom, &authentication, &m.Recipient,
			&m.ToRecipients, &m.CC, &m.BCC, &m.ReplyTo,
			&m.Subject, &m.EmailMessageID,
			&m.Method, &m.Type,
			&m.ConversationID, &m.InboxStatus,
			&m.Status,
			&m.CreatedAt, &m.ExpiresAt,
			&m.Labels,
			&outboundDeliveryStatus, &m.DeliveryDetail, &m.SentAs, &authVerdict,
			&m.Flagged, &m.FlagReason,
			&m.WebhookStatus, &m.WebhookError, &m.ScheduledAt,
		); err != nil {
			return nil, err
		}
		if err := unmarshalAuthVerdict(authVerdict, &m); err != nil {
			return nil, err
		}
		if err := unmarshalAuthentication(authentication, authVerdict, &m); err != nil {
			return nil, err
		}
		// DeliveryStatus is overloaded by direction (see Message.DeliveryStatus):
		// inbound carries inbox_status, outbound carries the delivery rollup.
		if m.Direction == "outbound" {
			m.DeliveryStatus = outboundDeliveryStatus
		} else {
			m.DeliveryStatus = m.InboxStatus
		}
		d.Messages = append(d.Messages, m)

		// Accumulate aggregates as we go — cheaper than a second
		// pass and keeps memory bounded to the unique-strings set.
		if m.Sender != "" {
			participantSet[m.Sender] = struct{}{}
		}
		if m.Recipient != "" {
			participantSet[m.Recipient] = struct{}{}
		}
		for _, a := range m.ToRecipients {
			if a != "" {
				participantSet[a] = struct{}{}
			}
		}
		for _, a := range m.CC {
			if a != "" {
				participantSet[a] = struct{}{}
			}
		}
		for _, a := range m.BCC {
			if a != "" {
				participantSet[a] = struct{}{}
			}
		}
		for _, l := range m.Labels {
			labelSet[l] = struct{}{}
		}

		// Maintain the aggregate counts inline.
		d.MessageCount++
		if m.Direction == "inbound" {
			d.InboundCount++
			if m.InboxStatus == "unread" {
				d.HasUnread = true
			}
		} else if m.Direction == "outbound" {
			d.OutboundCount++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if d.MessageCount == 0 {
		return nil, ErrMessageNotFound
	}

	d.ID = conversationID
	// Messages are oldest-first so [0] is first and [n-1] is last.
	d.FirstMessageAt = d.Messages[0].CreatedAt
	d.LastMessageAt = d.Messages[d.MessageCount-1].CreatedAt
	latest := d.Messages[d.MessageCount-1]
	d.LatestSubject = latest.Subject
	d.LatestSender = latest.Sender

	d.Participants = make([]string, 0, len(participantSet))
	for p := range participantSet {
		d.Participants = append(d.Participants, p)
	}
	sort.Strings(d.Participants)

	d.Labels = make([]string, 0, len(labelSet))
	for l := range labelSet {
		d.Labels = append(d.Labels, l)
	}
	sort.Strings(d.Labels)

	return d, nil
}

// --- User management ---

func (s *Store) CreateOrGetUser(ctx context.Context, email, name, googleSub string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (id, email, name, google_subject)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (google_subject) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name
		 RETURNING id, email, name, google_subject, created_at`,
		generateID(), email, name, googleSub,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// SetAccountClass sets a user's account_class (standard|internal|system|demo).
// Used by the prober's seed to mark the synthetic probe account as system so its
// traffic is never metered (see usage.PolicyFor). The CHECK constraint in
// migration 037 rejects any other value.
func (s *Store) SetAccountClass(ctx context.Context, userID, class string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET account_class = $2 WHERE id = $1`, userID, class)
	return err
}

// BootstrapUser finds a user by email, or creates one with a synthetic
// google_subject if none exists. Used by the -bootstrap-email CLI flag
// for self-host first-run, where there's no Google OAuth flow yet.
func (s *Store) BootstrapUser(ctx context.Context, email string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, name, google_subject, created_at FROM users WHERE email = $1`, email,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt)
	if err == nil {
		return u, nil
	}
	id := generateID()
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users (id, email, name, google_subject)
		 VALUES ($1, $2, 'bootstrap', $3)
		 RETURNING id, email, name, google_subject, created_at`,
		id, email, "bootstrap:"+id,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ErrEmailConflict is returned by ProvisionUser when the requested email is
// already held by a DIFFERENT user row. Callers must surface it as a
// conflict — never attach to or merge the existing account.
var ErrEmailConflict = errors.New("identity: email already held by another user")

// ProvisionUser creates a user row on behalf of an external control plane
// (POST /api/internal/users/provision), idempotently keyed by externalRef:
// the row's google_subject is "bootstrap:"+externalRef, so a replay of the
// same ref returns the existing user (created=false) without touching its
// email or name. A different ref carrying an email that another user already
// holds fails with ErrEmailConflict. account_class stays at the DB default.
//
// A nonempty externalIssuer additionally inserts/replays the delegated
// (issuer, externalRef) → user mapping in the SAME transaction, so a
// native control-plane activation atomically becomes delegated-token
// resolvable while google_subject keeps its exact legacy behavior. The
// empty string preserves the pre-delegated contract byte-for-byte and
// writes no mapping. A pair already attached to a different user aborts
// with ErrExternalPrincipalConflict.
func (s *Store) ProvisionUser(ctx context.Context, externalRef, email, name, externalIssuer string) (*User, bool, error) {
	if externalIssuer == "" {
		return s.provisionUser(ctx, s.pool, externalRef, email, name)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	u, created, err := s.provisionUser(ctx, tx, externalRef, email, name)
	if err != nil {
		return nil, false, err
	}
	if err := provisionExternalPrincipalTx(ctx, tx, externalIssuer, externalRef, u.ID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return u, created, nil
}

func (s *Store) provisionUser(ctx context.Context, q rowQuerier, externalRef, email, name string) (*User, bool, error) {
	subject := "bootstrap:" + externalRef
	u := &User{}
	err := q.QueryRow(ctx,
		`INSERT INTO users (id, email, name, google_subject)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (google_subject) DO NOTHING
		 RETURNING id, email, name, google_subject, created_at`,
		generateID(), email, name, subject,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt)
	if err == nil {
		return u, true, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
		// ON CONFLICT covers google_subject; the only other unique key the
		// caller can collide with is email. (An id collision is astronomically
		// unlikely and is not caller-actionable — surface it as a plain error.)
		if pgErr.ConstraintName == "users_email_key" {
			return nil, false, ErrEmailConflict
		}
		return nil, false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	// The insert was swallowed by ON CONFLICT (google_subject) DO NOTHING:
	// this ref was already provisioned. Re-read and report the existing row.
	u = &User{}
	if err := q.QueryRow(ctx,
		`SELECT id, email, name, google_subject, created_at FROM users WHERE google_subject = $1`, subject,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt); err != nil {
		return nil, false, err
	}
	return u, false, nil
}

func (s *Store) GetUserByID(ctx context.Context, id string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, name, google_subject, created_at, account_class, acquisition_answered_at FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AccountClass, &u.AcquisitionAnsweredAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// UpdateUserName persists a new display name on the user row and
// returns the updated User. Input validation (length, whitespace) is
// the caller's responsibility — this layer only enforces that the row
// exists.
func (s *Store) UpdateUserName(ctx context.Context, userID, name string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`UPDATE users SET name = $1 WHERE id = $2
		 RETURNING id, email, name, google_subject, created_at, acquisition_answered_at`,
		name, userID,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AcquisitionAnsweredAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// RecordAcquisitionSurvey stores the onboarding survey answer for a user,
// write-once: the UPDATE is conditioned on acquisition_answered_at IS
// NULL so two concurrent submits cannot both win. A no-op UPDATE is
// disambiguated with a follow-up lookup — ErrAcquisitionSurveyAnswered
// when the user exists, the lookup's not-found error otherwise. Source
// validity is the caller's job (the handler maps it to a 400), but the
// value is re-checked here so no path can write outside the enum.
func (s *Store) RecordAcquisitionSurvey(ctx context.Context, userID, source string, detail *string) (*User, error) {
	if !IsAcquisitionSource(source) {
		return nil, fmt.Errorf("invalid acquisition source %q", source)
	}
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`UPDATE users
		 SET acquisition_source = $1, acquisition_detail = $2, acquisition_answered_at = now()
		 WHERE id = $3 AND acquisition_answered_at IS NULL
		 RETURNING id, email, name, google_subject, created_at, account_class, acquisition_answered_at`,
		source, detail, userID,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AccountClass, &u.AcquisitionAnsweredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, lookupErr := s.GetUserByID(ctx, userID); lookupErr != nil {
			return nil, lookupErr
		}
		return nil, ErrAcquisitionSurveyAnswered
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// --- Session management ---

const SessionTTL = 7 * 24 * time.Hour

func (s *Store) CreateUserSession(ctx context.Context, userID string) (string, error) {
	token := "sess_" + randomHex32() // opaque session cookie value
	expiresAt := time.Now().Add(SessionTTL)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO user_sessions (token, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
		token, userID, time.Now(), expiresAt,
	)
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) GetUserSession(ctx context.Context, token string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT u.id, u.email, u.name, u.google_subject, u.created_at, u.account_class, u.acquisition_answered_at
		 FROM user_sessions s JOIN users u ON s.user_id = u.id
		 WHERE s.token = $1 AND s.expires_at > now()`, token,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AccountClass, &u.AcquisitionAnsweredAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) DeleteUserSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE token = $1`, token)
	return err
}

func (s *Store) DeleteExpiredUserSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// --- Dashboard aggregates ---

// DashboardStats is the workspace-level summary returned by
// GetDashboardStats. Each section corresponds to one of the cards on the
// redesigned dashboard's stats strip; null/zero values render as "—"
// in the UI, so deployments without E2A_USAGE_TRACKING enabled
// degrade gracefully.
type DashboardStats struct {
	Today              DashboardTodayStats   `json:"today"`
	Pending            DashboardPendingStats `json:"pending"`
	DeliverySuccessPct float64               `json:"delivery_success_pct"`
	SampleWindowDays   int                   `json:"sample_window_days"`
	// InboundWindow / OutboundWindow are the totals over the same
	// SampleWindowDays as DeliverySuccessPct. The dashboard at-a-glance
	// strip uses Today.*; the settings page uses these window totals
	// at a 30-day window (?window=30). Sum over usage_summaries rows
	// in the lookback period.
	InboundWindow  int `json:"inbound_window"`
	OutboundWindow int `json:"outbound_window"`
}

type DashboardTodayStats struct {
	Inbound          int `json:"inbound"`
	Outbound         int `json:"outbound"`
	InboundDeltaPct  int `json:"inbound_delta_pct"`
	OutboundDeltaPct int `json:"outbound_delta_pct"`
}

type DashboardPendingStats struct {
	Count         int `json:"count"`
	OldestSeconds int `json:"oldest_seconds"`
}

// DashboardDefaultWindowDays is the lookback for the dashboard strip
// when the caller doesn't request a specific window.
const DashboardDefaultWindowDays = 7

// DashboardMaxWindowDays caps the lookback to keep the underlying SQL
// scan bounded. 90 days is generous for any UI surface we currently
// have and remains efficient given the per-user index on
// usage_summaries.
const DashboardMaxWindowDays = 90

// GetDashboardStats returns workspace-level aggregates for the
// authenticated user, with a configurable lookback window. windowDays
// controls Inbound/Outbound totals AND the delivery-success ratio's
// sample period — passing 0 falls back to DashboardDefaultWindowDays
// (7), values above DashboardMaxWindowDays (90) are clamped.
//
// Three independent reads — kept separate because the source tables
// have different indexes and one slow read shouldn't slow the others.
// All reads are O(rows-for-this-user-only) thanks to the existing
// per-user indexes.
//
// Robust to missing data: deployments without usage tracking enabled
// (E2A_USAGE_TRACKING=false — the default for self-hosters) return
// zero counts rather than erroring. Same for users who have no
// messages yet. The UI renders zero values as "—".
//
// Delta percentages: today vs yesterday on usage_summaries. Avoids
// divide-by-zero when yesterday was zero by returning 0. 100% in/de-
// crease maps to ±100; values clipped at ±999 for integer width.
func (s *Store) GetDashboardStats(ctx context.Context, userID string, windowDays int) (*DashboardStats, error) {
	if windowDays <= 0 {
		windowDays = DashboardDefaultWindowDays
	}
	if windowDays > DashboardMaxWindowDays {
		windowDays = DashboardMaxWindowDays
	}
	stats := &DashboardStats{
		SampleWindowDays: windowDays,
	}

	// 1) Today's usage and yesterday's baseline. LEFT JOIN trick keeps
	// the query a single row even when one or both buckets are absent.
	// Unit note (usage-based pricing v1): outbound_count is in RECIPIENT-
	// DELIVERIES (a message to N recipients contributes N), not messages.
	var todayInbound, todayOutbound, yesterdayInbound, yesterdayOutbound int
	err := s.pool.QueryRow(ctx,
		`SELECT
		   COALESCE((SELECT inbound_count  FROM usage_summaries WHERE user_id = $1 AND bucket_date = current_date), 0),
		   COALESCE((SELECT outbound_count FROM usage_summaries WHERE user_id = $1 AND bucket_date = current_date), 0),
		   COALESCE((SELECT inbound_count  FROM usage_summaries WHERE user_id = $1 AND bucket_date = current_date - 1), 0),
		   COALESCE((SELECT outbound_count FROM usage_summaries WHERE user_id = $1 AND bucket_date = current_date - 1), 0)`,
		userID).Scan(&todayInbound, &todayOutbound, &yesterdayInbound, &yesterdayOutbound)
	if err != nil {
		return nil, fmt.Errorf("today/yesterday usage: %w", err)
	}
	stats.Today = DashboardTodayStats{
		Inbound:          todayInbound,
		Outbound:         todayOutbound,
		InboundDeltaPct:  deltaPct(todayInbound, yesterdayInbound),
		OutboundDeltaPct: deltaPct(todayOutbound, yesterdayOutbound),
	}

	// 2) Pending HITL approvals across the user's agents. Joining via
	// the agent_id keeps the per-user partial index on messages
	// (idx_messages_pending_review) usable.
	var pendingCount int
	var oldestSec *int
	err = s.pool.QueryRow(ctx,
		`SELECT count(*),
		        CASE WHEN count(*) = 0 THEN NULL
		             ELSE EXTRACT(EPOCH FROM (now() - MIN(m.created_at)))::int
		        END
		 FROM messages m
		 JOIN agent_identities a ON a.id = m.agent_id
		 WHERE a.user_id = $1 AND a.deleted_at IS NULL
		   AND m.status = 'pending_review' AND m.direction = 'outbound'`,
		userID).Scan(&pendingCount, &oldestSec)
	if err != nil {
		return nil, fmt.Errorf("pending count: %w", err)
	}
	stats.Pending.Count = pendingCount
	if oldestSec != nil {
		stats.Pending.OldestSeconds = *oldestSec
	}

	// 3) Window aggregates: inbound + outbound totals and the delivery
	// success ratio, all over the same lookback. Three subqueries in
	// one round-trip — usage_summaries is keyed (user_id, bucket_date)
	// so the per-user index handles each scan cheaply. windowDays is
	// validated above (1..90), so direct interpolation into the SQL
	// is safe and keeps the query plan-cacheable.
	var winInbound, winOutbound int
	var successRatio *float64
	err = s.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT
		   COALESCE((SELECT sum(inbound_count)::int  FROM usage_summaries
		             WHERE user_id = $1 AND bucket_date > current_date - %d), 0) AS inbound_window,
		   COALESCE((SELECT sum(outbound_count)::int FROM usage_summaries
		             WHERE user_id = $1 AND bucket_date > current_date - %d), 0) AS outbound_window,
		   (SELECT (count(*) FILTER (WHERE wd.status = 'delivered'))::float
		            / NULLIF(count(*) FILTER (WHERE wd.status IN ('delivered','failed')), 0)
		      FROM webhook_deliveries wd
		      JOIN messages m ON m.id = wd.message_id
		      JOIN agent_identities a ON a.id = m.agent_id
		      WHERE a.user_id = $1
		        AND wd.created_at > now() - interval '%d days')`,
			windowDays, windowDays, windowDays),
		userID).Scan(&winInbound, &winOutbound, &successRatio)
	if err != nil {
		return nil, fmt.Errorf("window aggregates: %w", err)
	}
	stats.InboundWindow = winInbound
	stats.OutboundWindow = winOutbound
	if successRatio != nil {
		// Round to one decimal place — 99.6 is more useful than 99.555555.
		stats.DeliverySuccessPct = float64(int(*successRatio*1000+0.5)) / 10.0
	}

	return stats, nil
}

// deltaPct computes the integer percentage change of current vs
// previous. Zero previous → 0 (no arrow in UI). Clipped to ±999 to
// keep the value width manageable.
func deltaPct(current, previous int) int {
	if previous == 0 {
		return 0
	}
	delta := float64(current-previous) / float64(previous) * 100
	if delta > 999 {
		return 999
	}
	if delta < -999 {
		return -999
	}
	return int(delta)
}

// --- Per-user API keys ---

// Credential scope (Slice 5a / design §5). The scope a credential carries —
// not the auth method — determines its blast radius.
const (
	// ScopeAccount is account-wide admin: agent/domain/key management, account
	// settings. The pre-redesign default; what an `e2a_acct_…` key holds.
	ScopeAccount = "account"
	// ScopeAgent is bound to a single agent (runtime/inbox tier): the credential
	// IS the agent. Pinned to one agent_id and barred from account-only ops.
	ScopeAgent = "agent"
)

// ValidScope reports whether s is a known credential scope.
func ValidScope(s string) bool { return s == ScopeAccount || s == ScopeAgent }

// Principal is the authenticated caller resolved from a credential: the owning
// user plus the credential's scope and (for agent-scoped credentials) the agent
// it is bound to. The scope/agent binding is what the v1 handlers enforce the
// hard scope ceiling against (design §5 / decision 10).
type Principal struct {
	User    *User
	Scope   string // ScopeAccount | ScopeAgent
	AgentID string // non-empty only when Scope == ScopeAgent
}

type APIKey struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	Name         string    `json:"name"`
	KeyPrefix    string    `json:"key_prefix"`
	PlaintextKey string    `json:"key,omitempty"` // only set once at creation, never stored
	CreatedAt    time.Time `json:"created_at"`
	// Scope is the credential's blast radius (ScopeAccount | ScopeAgent).
	// Backfilled to ScopeAccount for pre-Slice-5a keys.
	Scope string `json:"scope"`
	// AgentID is the bound agent for ScopeAgent keys; nil for account keys.
	AgentID *string `json:"agent_id,omitempty"`
	// LastUsedAt is updated by GetUserByAPIKey on every successful
	// AuthenticateRequest. NULL on keys that have never been used.
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// ExpiresAt is the optional hard expiry. AuthenticateRequest rejects
	// keys whose expires_at has passed. NULL means "never expires"
	// (the backward-compatible default).
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func hashAPIKey(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}

// CreateAPIKey issues a fresh ACCOUNT-scoped API key for the user. expiresAt is
// the optional hard expiration; pass nil to issue a never-expiring key (the
// backward-compatible default). This is the account-tier convenience wrapper
// over CreateScopedAPIKey — the self-host default key.
func (s *Store) CreateAPIKey(ctx context.Context, userID, name string, expiresAt *time.Time) (*APIKey, error) {
	return s.CreateScopedAPIKey(ctx, userID, name, ScopeAccount, "", expiresAt)
}

// CreateScopedAPIKey issues a fresh API key with an explicit scope (Slice 5a).
//   - ScopeAccount: account-wide admin; agentID must be empty; prefix e2a_acct_.
//   - ScopeAgent: bound to agentID (which must be a non-empty agent owned by the
//     user); prefix e2a_agt_. The key can only act as that one agent.
//
// The visible prefix makes a key's blast radius obvious at a glance, and the DB
// CHECK (scope='agent') == (agent_id IS NOT NULL) backstops the binding.
func (s *Store) CreateScopedAPIKey(ctx context.Context, userID, name, scope, agentID string, expiresAt *time.Time) (*APIKey, error) {
	if !ValidScope(scope) {
		return nil, fmt.Errorf("invalid credential scope %q", scope)
	}
	if scope == ScopeAgent && agentID == "" {
		return nil, fmt.Errorf("agent-scoped key requires an agent_id")
	}
	if scope == ScopeAccount && agentID != "" {
		return nil, fmt.Errorf("account-scoped key must not name an agent")
	}
	// For an agent-scoped key, the named agent must exist and be owned by the
	// same user — otherwise a caller could mint a key bound to someone else's
	// agent (the FK alone wouldn't catch cross-user binding).
	if scope == ScopeAgent {
		owns, err := s.userOwnsAgent(ctx, agentID, userID)
		if err != nil {
			return nil, err
		}
		if !owns {
			return nil, fmt.Errorf("agent %q not found or not owned by user", agentID)
		}
	}

	id := "apk_" + generateID()
	plaintext := generateAPIKey(scope)
	keyHash := hashAPIKey(plaintext)
	// Show the scoped prefix + a few key chars (e.g. "e2a_agt_abcd…").
	prefix := plaintext[:16]
	now := time.Now()
	var agentCol *string
	if scope == ScopeAgent {
		agentCol = &agentID
	}
	ak := &APIKey{
		ID:           id,
		UserID:       userID,
		Name:         name,
		KeyPrefix:    prefix,
		PlaintextKey: plaintext,
		CreatedAt:    now,
		Scope:        scope,
		AgentID:      agentCol,
		ExpiresAt:    expiresAt,
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO api_keys (id, user_id, name, key_prefix, key_hash, scope, agent_id, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		ak.ID, ak.UserID, ak.Name, ak.KeyPrefix, keyHash, ak.Scope, agentCol, ak.CreatedAt, ak.ExpiresAt,
	)
	if err != nil {
		return nil, err
	}
	return ak, nil
}

// userOwnsAgent reports whether agentID exists and is owned by userID.
func (s *Store) userOwnsAgent(ctx context.Context, agentID, userID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM agent_identities WHERE id = $1 AND user_id = $2)`,
		agentID, userID,
	).Scan(&exists)
	return exists, err
}

// ListAPIKeys returns one page of the user's live (non-revoked) API keys,
// newest-first, keyset-paginated on (created_at, id). limit<=0 returns every
// key unpaginated (the all-consumers: auth dashboard + prober seed); a positive
// limit fetches that many (pass limit+1 to detect a further page) starting after
// the (afterCreatedAt, afterID) key from the previous page's last row (zero
// afterCreatedAt = first page).
func (s *Store) ListAPIKeys(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]APIKey, error) {
	q := `SELECT id, user_id, name, key_prefix, COALESCE(scope, 'account'), agent_id, created_at, last_used_at, expires_at
	   FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL`
	args := []interface{}{userID}
	if !afterCreatedAt.IsZero() {
		i := len(args) + 1
		q += fmt.Sprintf(` AND (created_at < $%d OR (created_at = $%d AND id < $%d))`, i, i, i+1)
		args = append(args, afterCreatedAt, afterID)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT $%d`, len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyPrefix, &k.Scope, &k.AgentID, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// ErrAPIKeyNotFound is returned by DeleteAPIKey when no live key matched the
// (id, user) — i.e. it doesn't exist, isn't owned by the caller, or was
// already revoked. Distinct from a DB/connection error so the HTTP layer can
// map it to 404 while surfacing real failures as 500 (mirrors
// ErrWebhookNotFound).
var ErrAPIKeyNotFound = errors.New("api key not found")

func (s *Store) DeleteAPIKey(ctx context.Context, keyID, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, keyID, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAPIKeyNotFound
	}
	return nil
}

// GetUserByAPIKey authenticates a bearer token and returns the owning
// user. Rejects revoked keys and time-expired keys; touches last_used_at
// only on the success path so the column stays a real "last successful
// authentication" signal (rather than "last attempt").
//
// Expiration semantics: expires_at IS NULL means the key never expires
// (preserves the pre-migration default). A non-null expires_at must be in
// the future, evaluated against now() in the same query so there's no
// clock skew between row read and check.
func (s *Store) GetUserByAPIKey(ctx context.Context, apiKey string) (*User, error) {
	p, err := s.GetPrincipalByAPIKey(ctx, apiKey)
	if err != nil {
		return nil, err
	}
	return p.User, nil
}

// GetPrincipalByAPIKey authenticates a bearer token and returns the full
// principal — the owning user PLUS the key's scope and bound agent (Slice 5a).
// Same validation/last_used semantics as GetUserByAPIKey (it delegates here).
// A legacy key with a NULL scope column resolves to ScopeAccount, preserving
// pre-redesign authority.
func (s *Store) GetPrincipalByAPIKey(ctx context.Context, apiKey string) (*Principal, error) {
	keyHash := hashAPIKey(apiKey)
	u := &User{}
	var scope string
	var agentID *string
	err := s.pool.QueryRow(ctx,
		`WITH touched AS (
		   UPDATE api_keys SET last_used_at = now()
		   WHERE key_hash = $1
		     AND revoked_at IS NULL
		     AND (expires_at IS NULL OR expires_at > now())
		   RETURNING user_id, COALESCE(scope, 'account') AS scope, agent_id
		 )
		 SELECT u.id, u.email, u.name, u.google_subject, u.created_at, u.account_class, t.scope, t.agent_id
		 FROM touched t JOIN users u ON u.id = t.user_id`, keyHash,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AccountClass, &scope, &agentID)
	if err != nil {
		return nil, err
	}
	p := &Principal{User: u, Scope: scope}
	if agentID != nil {
		p.AgentID = *agentID
	}
	return p, nil
}

func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure means the OS RNG is broken — without it
		// we'd silently emit an all-zero ID. Panic to surface a 500
		// rather than poison the database with predictable identifiers.
		panic(fmt.Sprintf("identity: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// generateAPIKey mints a random key with a scope-revealing prefix (Slice 5a):
// e2a_acct_… for account keys, e2a_agt_… for agent keys. The prefix is cosmetic
// for validation (keys are matched by hash of the full string), but makes a
// key's blast radius obvious wherever it's pasted or logged. Legacy `e2a_…`
// keys minted before this change keep validating — the hash is over the whole
// string, so the prefix change only affects newly minted keys.
func generateAPIKey(scope string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Same reasoning as generateID — an all-zero API key would be
		// catastrophic (predictable auth credential).
		panic(fmt.Sprintf("identity: crypto/rand failed: %v", err))
	}
	prefix := "e2a_acct_"
	if scope == ScopeAgent {
		prefix = "e2a_agt_"
	}
	return prefix + hex.EncodeToString(b)
}

// randomHex32 returns 32 bytes of crypto-random data hex-encoded. Shared by the
// session-token path; panics on RNG failure (same reasoning as generateID).
func randomHex32() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("identity: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}
