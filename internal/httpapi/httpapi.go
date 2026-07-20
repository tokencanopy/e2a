package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendramp"
	"github.com/tokencanopy/e2a/internal/webhook"
)

// Authenticator resolves the calling user from the raw request. It is
// injected so this package reuses the *single* auth path that already lives
// in the agent layer (API key, OAuth bearer, session cookie) instead of
// forking a second one — there is exactly one place credentials are checked.
type Authenticator func(r *http.Request) (*identity.User, error)

// PrincipalAuthenticator is the scope-aware seam (Slice 5a): the same single
// auth path, but returning the full principal (user + credential scope + bound
// agent) so the v1 handlers can enforce the hard scope ceiling (design §5).
// When set it supersedes Authenticator; when nil the server wraps Authenticator
// and treats every caller as account-scoped (pre-Slice-5a behavior).
type PrincipalAuthenticator func(r *http.Request) (*identity.Principal, error)

// AgentLister returns one page of the agents owned by a user, keyset-paginated
// on (created_at, id): limit is the page size (the handler passes limit+1 to
// detect a further page; limit<=0 returns every agent, which the webhook
// filter-ownership validation relies on), and afterCreatedAt/afterID is the
// position from the previous page's last row (zero afterCreatedAt = first page).
// Injected as a narrow function so the foundation slice doesn't depend on the
// whole store.
type AgentLister func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.AgentIdentity, error)

// AgentGetter loads a single agent by its full email address (the
// identifier). Ownership is checked by the caller against the resolved
// agent's UserID.
type AgentGetter func(ctx context.Context, address string) (*identity.AgentIdentity, error)

// MessageGetter loads a single message (with content) scoped to an agent.
// Mirrors store.GetMessageWithContent(messageID, agentID).
type MessageGetter func(ctx context.Context, messageID, agentID string) (*identity.Message, error)

// MessageLister returns a filtered page of message summaries for an agent.
// Mirrors store.GetMessagesByAgent(filter).
type MessageLister func(ctx context.Context, filter identity.MessageListFilter) ([]identity.Message, error)

// MessageLifecycleLister returns the complete canonical lifecycle for a
// message scoped to its owning agent. The handler applies cursor pagination
// after this ownership-bound read so reconstructed and persisted transitions
// share one deterministic ordering contract.
type MessageLifecycleLister func(ctx context.Context, messageID, agentID string) ([]messagelifecycle.MessageLifecycleTransition, error)

// ConversationLister mirrors store.ListConversationsByAgent(filter).
type ConversationLister func(ctx context.Context, filter identity.ConversationListFilter) ([]identity.ConversationSummary, error)

// ConversationGetter mirrors store.GetConversationByID(agentID, conversationID).
type ConversationGetter func(ctx context.Context, agentID, conversationID string) (*identity.ConversationDetail, error)

// --- write collaborators ---

// AgentCreator mirrors store.CreateAgent. The webhookURL/agentMode params are
// retained for signature compatibility with the store but are ignored — the
// legacy columns were dropped (migration 029). Handlers pass "".
type AgentCreator func(ctx context.Context, email, domain, name, webhookURL, agentMode, userID string) (*identity.AgentIdentity, error)

// DomainLookup mirrors store.LookupDomain(domain, userID) — the create-time
// ownership guard.
type DomainLookup func(ctx context.Context, domain, userID string) (*identity.Domain, error)

// CoveringDomainLookup mirrors store.LookupCoveringDomain(sub, userID): the
// create-time fallback that finds the most-specific registered parent domain
// the user owns which covers an agent's subdomain (label-boundary match). The
// caller checks its Verified state so a pending child cannot be masked. Nil is
// tolerated (feature disabled ⇒ exact-match-only behavior).
type CoveringDomainLookup func(ctx context.Context, sub, userID string) (*identity.Domain, error)

// MXResolver returns the effective MX hosts published for a name. Used for the
// create-time subdomain MX gate: a single
// lookup on the subdomain FQDN answers both the explicit-subdomain-MX and the
// wildcard-MX-on-parent cases, because a resolver synthesizes the wildcard for
// the queried name. Injected so unit tests mock the resolver and the httpapi
// layer stays off the network.
type MXResolver func(ctx context.Context, name string) ([]string, error)

// AgentCreateEnforcer mirrors enforcer.CheckAgentCreate; returns a
// limits.LimitExceededError when the per-user cap is hit.
type AgentCreateEnforcer func(ctx context.Context, userID string) error

// Agent mutation funcs mirror the like-named store methods.
type (
	// AgentDeleter deletes an agent, returning the number of message rows
	// removed by the cascade (surfaced in the DeleteAgentResult receipt).
	AgentDeleter func(ctx context.Context, agentID, userID string) (messagesDeleted int64, err error)
	// AgentTrashOp moves an agent into or out of trash without deleting messages.
	AgentTrashOp func(ctx context.Context, agentID, userID string) error
	// AgentRestoreOp is AgentTrashOp's returning form: restore answers with the
	// agent as restored. The store reads that row inside the restore's own
	// transaction, so the response cannot describe a state this restore never
	// produced — a post-commit re-read raced concurrent renames and re-trashes.
	AgentRestoreOp func(ctx context.Context, agentID, userID string) (*identity.AgentIdentity, error)
)

// MessageTrashOp mirrors the store's per-message trash mutations
// (SoftDeleteMessage / PurgeMessage): scoped to (messageID, agentID),
// returning the sentinel errors ErrMessageHeld / ErrNotInTrash /
// ErrMessageNotFound for the handler to map.
type MessageTrashOp func(ctx context.Context, messageID, agentID string) error

// MessageRestoreOp is MessageTrashOp's returning form, used by RestoreMessage
// for the same reason AgentRestoreOp exists: the restored view comes from
// inside the restore transaction rather than a racy re-read afterwards.
type MessageRestoreOp func(ctx context.Context, messageID, agentID string) (*identity.Message, error)

// Deps are the collaborators the v1 layer needs. Everything is injected so
// the package has no hidden globals and is straightforward to test.
type Deps struct {
	Authenticator          Authenticator
	PrincipalAuthenticator PrincipalAuthenticator
	// AuthChallenge builds the RFC 6750 §3 WWW-Authenticate header value for a
	// request that failed authentication. Injected so the v1 surface advertises
	// the Bearer scheme (and OAuth error params) on every 401 exactly like the
	// legacy mux did, from the same definition (agent.API.WWWAuthenticateChallenge).
	// Optional — nil disables the challenge header.
	AuthChallenge        func(r *http.Request) string
	ListAgents           AgentLister
	GetAgent             AgentGetter
	GetMessage           MessageGetter
	ListMessages         MessageLister
	ListMessageLifecycle MessageLifecycleLister
	// CountAgentMetrics aggregates persisted lifecycle observations into the
	// per-agent counter set behind GET /v1/agents/{email}/metrics. Optional —
	// nil makes the operation answer 501 rather than an empty, misleading zero.
	CountAgentMetrics AgentMetricsCounter
	// CountAccountMetrics is CountAgentMetrics' account-wide sibling behind
	// GET /v1/metrics. Optional on the same terms — nil answers 501.
	CountAccountMetrics AccountMetricsCounter
	// CountWebhookDeliveries backs the webhooks block on GET /v1/metrics.
	// Optional — nil yields a zeroed block rather than failing the read, so a
	// deployment without the subscriber store still serves email counters.
	CountWebhookDeliveries WebhookDeliveryCounter
	// ModifyMessageLabels applies a labels delta to a message scoped to an
	// agent, returning the post-update set. Mirrors store.ModifyMessageLabels.
	ModifyMessageLabels func(ctx context.Context, messageID, agentID string, add, remove []string) ([]string, error)

	ListConversations ConversationLister
	GetConversation   ConversationGetter

	CreateAgent          AgentCreator
	LookupDomain         DomainLookup
	LookupCoveringDomain CoveringDomainLookup
	// ResolveMX backs the required create-time subdomain MX gate.
	ResolveMX          MXResolver
	EnforceAgentCreate AgentCreateEnforcer
	// UpdateAgentName updates an agent's display name (the only mutable field on
	// the agent PATCH after the screening config moved to /protection) and
	// returns the agent as written — read inside the write's transaction, so
	// the PATCH response describes this write and not a concurrent one.
	UpdateAgentName func(ctx context.Context, agentID, userID, name string) (*identity.AgentIdentity, error)
	// UpdateAgentProtection writes the full per-agent protection posture (gate +
	// scan sensitivity + holds) for the /v1/agents/{email}/protection resource
	// and returns the agent as written, on the same in-transaction contract as
	// UpdateAgentName. Returns a validation error for an invalid posture, which
	// the handler maps to 400 invalid_request.
	UpdateAgentProtection func(ctx context.Context, agentID, userID string, cfg identity.ProtectionConfig) (*identity.AgentIdentity, error)
	// DeleteAgent is the DEFAULT delete: soft (move to trash, restorable for
	// identity.TrashRetention, docs/design/trash-soft-delete.md).
	// PermanentDeleteAgent is the irreversible hard delete behind
	// ?permanent=true; RestoreAgent brings a trashed agent back.
	DeleteAgent          AgentTrashOp
	PermanentDeleteAgent AgentDeleter
	RestoreAgent         AgentRestoreOp
	// GetAgentAnyState loads an agent regardless of trash state (DeletedAt set
	// when trashed) — the resolution path for restore / permanent delete, which
	// must find agents the live GetAgent treats as nonexistent.
	GetAgentAnyState AgentGetter
	// ListDeletedAgents is the account's agent trash (GET /v1/agents?deleted=true).
	ListDeletedAgents AgentLister

	// Message trash ops (DELETE / POST restore on
	// /v1/agents/{email}/messages/{id}): soft delete, restore, and the
	// trash-only permanent purge.
	DeleteMessage  MessageTrashOp
	RestoreMessage MessageRestoreOp
	PurgeMessage   MessageTrashOp

	// domains. ListDomains is keyset-paginated on (created_at, domain): the
	// handler passes limit+1 to detect a further page (limit<=0 = all), and the
	// after-key from the previous page's last row (zero afterCreatedAt = first
	// page).
	ListDomains         func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterDomain string) ([]identity.Domain, error)
	SendingRampSnapshot func(ctx context.Context, userID, domain string, now time.Time) (sendramp.Snapshot, error)
	ClaimDomain         func(ctx context.Context, domain, userID string) (*identity.Domain, error)
	EnforceDomainCreate func(ctx context.Context, userID string) error
	DeleteDomain        func(ctx context.Context, domain, userID string) error
	CountAgentsOnDomain func(ctx context.Context, domain, userID string) (live, trashed int, err error)

	// SMTPDomain is the relay's MX host, surfaced in the DNS records a
	// domain must publish (config smtp.domain).
	SMTPDomain string

	// SESRegion is the AWS region of the SES sending identity
	// (config sender_identity.ses_region). Non-empty ⇒ the sending feature is
	// enabled: domainView emits the deterministic mail_from_* records. Empty ⇒
	// sending is off and those records are omitted.
	SESRegion string

	// CursorSecret is the deployment HMAC secret (config.Signing.HMACSecret)
	// used to sign/verify pagination cursors so they are tamper-evident
	// (issue #144 M2). The same deployment key used by approvaltoken — no new
	// key. Handlers pass it to EncodeCursor and wrap it
	// in a 1-element slice for DecodeCursor (whose verify loop supports N for
	// a future secret rotation). Empty in minimal test setups, which is fine:
	// encode and verify stay consistent under the same (empty) key.
	CursorSecret string

	// Idempotency is the retry-safety store for unsafe writes (send/reply/
	// forward/redeliver). Optional — nil disables the Idempotency-Key path.
	Idempotency IdemStore

	// outbound (the shared live delivery path extracted from agent.API)
	DeliverOutbound func(ctx context.Context, user *identity.User, ag *identity.AgentIdentity, req outbound.SendRequest, msgType, replyToEmailMessageID string, referenced *identity.Message, idemCompleteTx agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError)
	// DeliverBatch is the batch-send accept-tx orchestrator, called by
	// handleSendBatch. Same shape as DeliverOutbound but for a slice of
	// SendRequest items — see docs/design/batch-send.md §9. Optional; when
	// nil the /v1/agents/{email}/batches operation is still registered but
	// returns 501 not_implemented for every call (matches the
	// nil-DeliverOutbound behavior of single-send). Wired in apiserver
	// via agent.API.DeliverBatch.
	DeliverBatch func(ctx context.Context, user *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, idemCompleteTx agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError)
	SendTest     func(ctx context.Context, ag *identity.AgentIdentity) (*agent.OutboundResult, *agent.OutboundError)
	// PollSendOutcome reads an async send's current delivery_status for wait=sent.
	// Optional — nil disables the wait valve (accepted is returned immediately).
	PollSendOutcome func(ctx context.Context, messageID string) (identity.SendOutcome, error)
	// HITL approve/reject (the held-draft decision)
	ApprovePending     func(ctx context.Context, userID, messageID, expectedAgentEmail string, ovr agent.ApproveOverrides, idemCompleteTx agent.ApproveIdemCompleter) (*identity.Message, *agent.OutboundError)
	RejectPending      func(ctx context.Context, userID, messageID, expectedAgentEmail, reason string) (*identity.Message, *agent.OutboundError)
	EnforceMessageSend func(ctx context.Context, userID string) error
	// Inbound review release — the held-screening decision (design 2026-06-22 §5).
	// GetReviewMessage resolves a held message's direction so /approve+/reject can
	// branch (it intentionally sees held inbound statuses, scoped to the resolved
	// owned agent — account-scope only). ApproveInboundReview releases the message
	// to the agent's inbox; RejectInboundReview drops it. Both fire the unified
	// review_approved/review_rejected events. Optional — nil leaves the endpoints
	// outbound-only (pre-slice-3 behavior).
	GetReviewMessage     func(ctx context.Context, messageID, agentID string) (*identity.ReviewMessageMeta, error)
	ApproveInboundReview func(ctx context.Context, userID string, msg *identity.ReviewMessageMeta) *agent.OutboundError
	RejectInboundReview  func(ctx context.Context, userID, reason string, msg *identity.ReviewMessageMeta) *agent.OutboundError

	// Review queue (account-scoped /v1/reviews). ListReviews returns all holds
	// (both directions) across the user's agents; GetReviewWithContent loads one
	// held message (ownership-scoped) for the detail view + approve/reject
	// resolution. Both intentionally include held inbound — operator surface only.
	// ListReviews is keyset-paginated on (created_at, id): the handler passes
	// limit+1 to detect a further page and the after-key from the previous page's
	// last row (zero afterCreatedAt = first page).
	ListReviews          func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.ReviewListItem, error)
	GetReviewWithContent func(ctx context.Context, userID, messageID string) (*identity.Message, error)
	// ListProtectionEventsByMessage returns the per-message screening audit
	// rows (gate + scan producers) behind a hold — the source of the detector
	// rationale/categories shown on the review detail. Optional; when nil the
	// detail omits the `protection` breakdown and clients fall back to the coded
	// review_reason. Callers must have already proven ownership of the message
	// (the review detail handler does, via GetReviewWithContent).
	ListProtectionEventsByMessage func(ctx context.Context, messageID string) ([]identity.ProtectionEvent, error)
	// SendLimit is the per-agent outbound rate limiter (mirrors
	// sendLimit.AllowWithRetryAfter; key = agent id). Optional.
	SendLimit func(key string) (ok bool, retryAfter time.Duration)
	// PollLimit is the per-user read limiter (key = user id) and RegLimit is
	// the per-IP agent-registration limiter (key = client ip). Both return
	// the IETF RateLimit snapshot so the middleware can set the headers.
	// Optional — nil disables that limiter on the /v1 surface.
	PollLimit RateSnapshot
	RegLimit  RateSnapshot
	// Raw capability routes sit outside Huma's authenticated middleware and use
	// separate per-IP budgets so traffic to one surface cannot starve another.
	// Optional — nil disables the corresponding limiter.
	DownloadLimit    RateSnapshot
	UnsubscribeLimit RateSnapshot
	// GetRepliableMessage loads a message that can be replied to or forwarded —
	// either an inbound the agent received or an outbound the agent sent — as
	// long as it is live (not expired) and not held/rejected in review. The
	// reply/forward handlers use this so an agent can continue a thread off its
	// own sent message (Gmail-style), which GetInboundMessage's direction filter
	// forbids.
	GetRepliableMessage func(ctx context.Context, messageID string) (*identity.Message, error)

	// AttachmentStore mints/verifies short-lived attachment downloads (§6a #5).
	// Native by default; when nil, the attachment endpoints are unavailable.
	AttachmentStore AttachmentStore

	// account
	GetLimits      func(ctx context.Context, userID string) (limits.Limits, error)
	GetUsage       func(ctx context.Context, userID string) LimitsUsageView
	ExportUserData func(ctx context.Context, userID string) (*identity.UserExport, error)

	// Suppression list (decision 9 / Slice 4b). Optional — nil deployments
	// return 501 from the /v1/account/suppressions endpoints.
	ListSuppressions  func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterAddress string) ([]identity.Suppression, error)
	RemoveSuppression func(ctx context.Context, userID, address string) (bool, error)
	// Agent suppressions are recipient consent blocks scoped to one exact
	// sending identity. Management is account-admin-only; the handler resolves
	// live ownership before calling these tenant-bound store methods.
	AddAgentSuppression    func(ctx context.Context, userID, agentID, address, reason, source string, onAdded identity.AgentSuppressionTxHook) (identity.AgentSuppression, bool, error)
	ListAgentSuppressions  func(ctx context.Context, userID, agentID string, limit int, afterCreatedAt time.Time, afterAddress string) ([]identity.AgentSuppression, error)
	RemoveAgentSuppression func(ctx context.Context, userID, agentID, address string) (bool, error)
	// Contacts are ACCOUNT-scoped identity for the people this account
	// corresponds with. Per-agent outreach state lives on a separate
	// engagement resource, so nothing here is agent-bound. All of these are
	// reachable only from account-scoped credentials — an agent credential
	// reading every contact the account owns would be a scope escalation.
	CreateContact            func(ctx context.Context, userID, address, displayName string, metadata map[string]any, source, importBatchID string) (identity.Contact, error)
	GetContact               func(ctx context.Context, userID, address string) (identity.Contact, error)
	ListContacts             func(ctx context.Context, userID string, f identity.ContactFilter, limit int, afterCreatedAt time.Time, afterID string) ([]identity.Contact, error)
	UpdateContact            func(ctx context.Context, userID, address string, displayName *string, metadata map[string]any) (identity.Contact, error)
	UpdateContactIfUnchanged func(ctx context.Context, userID, address string, displayName *string, metadata map[string]any, expectedUpdatedAt time.Time) (identity.Contact, error)
	DeleteContact            func(ctx context.Context, userID, address string) (bool, error)
	// Bulk import. ImportContacts applies one batch in a single transaction and
	// returns a per-row outcome, so a malformed row fails alone rather than
	// rejecting the upload. SuppressedAddresses lets the handler MARK rows the
	// account has already blocked without dropping them — the count a user sees
	// stays honest.
	ImportContacts            func(ctx context.Context, userID, batchID string, rows []identity.ContactImportRow, merge bool) ([]identity.ContactImportOutcome, error)
	ImportContactsWithOptions func(ctx context.Context, userID, batchID string, rows []identity.ContactImportRow, options identity.ContactImportOptions) ([]identity.ContactImportOutcome, error)
	DeleteImportBatch         func(ctx context.Context, userID, batchID string) (deleted int, retained int, engagementsDeleted int, err error)
	SuppressedAddresses       func(ctx context.Context, userID string, addresses []string) ([]string, error)
	EffectiveSuppressions     func(ctx context.Context, userID, agentID string, addresses []string) ([]string, error)
	// Per-agent outreach state. Unlike the contact capabilities above, these are
	// reachable by an AGENT-scoped credential acting as itself — the agent runs
	// its own outreach loop. Consent stays out of reach: suppression is only
	// ever read through a join here.
	UpsertEngagement            func(ctx context.Context, userID, agentID, address string, stage *string, nextActionAt **time.Time, metadata map[string]any) (identity.ContactEngagement, bool, error)
	UpdateEngagementIfUnchanged func(ctx context.Context, userID, agentID, address string, stage *string, nextActionAt **time.Time, metadata map[string]any, expectedUpdatedAt time.Time) (identity.ContactEngagement, error)
	GetEngagement               func(ctx context.Context, userID, agentID, address string) (identity.ContactEngagement, error)
	ListEngagements             func(ctx context.Context, userID, agentID string, f identity.EngagementFilter, limit int, afterCreatedAt time.Time, afterID string) ([]identity.ContactEngagement, error)
	DeleteEngagement            func(ctx context.Context, userID, agentID, address string) (bool, error)
	// Public managed-unsubscribe capabilities. Resolve accepts only a token hash;
	// the write capability accepts only the exact scope returned by that lookup,
	// so the unauthenticated route cannot choose an account, agent, or recipient.
	ResolveUnsubscribeToken           func(ctx context.Context, tokenHash []byte) (*identity.UnsubscribeScope, error)
	AddAgentSuppressionFromTokenScope func(ctx context.Context, scope identity.UnsubscribeScope, onAdded identity.AgentSuppressionTxHook) (identity.AgentSuppression, bool, error)
	// Shared transaction hook for both authenticated manual creation and the
	// public recipient flow; the store invokes it only for a newly inserted row.
	AgentSuppressionAddedHook identity.AgentSuppressionTxHook
	DeleteUserData            func(ctx context.Context, user *identity.User) (*identity.DeleteUserDataResult, error)

	// events (delivery log). EventQuery carries the filters + cursor
	// position; the closures bind the events pool in main.
	ListEvents func(ctx context.Context, q EventQuery) ([]agent.EventView, error)
	GetEvent2  func(ctx context.Context, userID, eventID string) (*agent.EventView, error)
	// redeliver
	LoadReplayEvent      func(ctx context.Context, userID, eventID string) (*agent.ReplayEvent, error)
	InsertReplayDelivery func(ctx context.Context, eventID, webhookID, eventType string, messageID *string, envelope []byte) (string, error)

	// EventsEnabled reflects whether the durable event log (the webhook_events
	// outbox) is populated on this deployment. Now unconditional in production;
	// the events handlers still gate on it so a deployment that ever disables the
	// outbox returns 501 events_log_disabled from list/get/redeliver instead of
	// masquerading as "no events". Webhook delivery is unaffected either way.
	EventsEnabled bool

	// webhooks
	// CreateWebhook mirrors identity.Store.CreateWebhookIdem: when the request
	// carries an Idempotency-Key the handler passes a completer that the store
	// runs INSIDE the insert transaction, committing the webhook row and the
	// cached replay response atomically (same-tx pattern as the send/approve
	// paths). idemCompleteTx is nil for unkeyed creates.
	CreateWebhook func(ctx context.Context, userID, url, description string, events []string, filters identity.WebhookFilters, idemCompleteTx identity.WebhookIdemCompleter) (*identity.Webhook, error)
	// ListWebhooks is keyset-paginated on (created_at, id): the handler passes
	// limit+1 to detect a further page (limit<=0 = all) and the after-key from the
	// previous page's last row (zero afterCreatedAt = first page).
	ListWebhooks  func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.Webhook, error)
	GetWebhook    func(ctx context.Context, webhookID, userID string) (*identity.Webhook, error)
	UpdateWebhook func(ctx context.Context, webhookID, userID string, u identity.WebhookUpdate) (*identity.Webhook, error)
	DeleteWebhook func(ctx context.Context, webhookID, userID string) error
	RotateSecret  func(ctx context.Context, webhookID, userID string) (string, time.Time, error)
	// TestWebhookInsert schedules a synthetic delivery (subscriberStore.
	// InsertPendingForTest). ListDeliveries reads the per-webhook delivery
	// log (subscriberStore.ListDeliveriesByWebhook).
	TestWebhookInsert func(ctx context.Context, webhookID, eventType string, envelope []byte) (string, error)
	// ListDeliveries is keyset-paginated on (created_at, id): the handler passes
	// limit+1 to detect a further page and the after-key from the previous page's
	// last row (zero afterCreatedAt = first page). status optionally restricts to
	// pending|delivered|failed.
	ListDeliveries func(ctx context.Context, webhookID, status string, limit int, afterCreatedAt time.Time, afterID string) ([]webhook.SubscriberDelivery, error)
	// EnqueueDelivery enqueues a River webhook_deliver job for a
	// webhook_subscriber_deliveries row that was inserted directly — the /test
	// endpoint and the event-redelivery API. Those two surfaces bypass the outbox
	// drain (which enqueues in-tx), so without this call their rows carry no River
	// job and, now that River is the sole delivery engine, would never deliver.
	// Wired unconditionally in production. Optional — nil in minimal test setups
	// with no River client, where a test drains delivery rows by other means.
	EnqueueDelivery func(ctx context.Context, deliveryID string) error

	// templates (beta). Mirror the like-named identity.Store methods; every
	// lookup is scoped to the owning user (cross-user reads behave as
	// not-found). GetTemplate/GetTemplateByAlias also serve the send path's
	// template_id/template_alias resolution.
	CreateTemplate     func(ctx context.Context, userID string, in identity.TemplateCreate) (*identity.Template, error)
	ListTemplates      func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.TemplateSummary, error)
	GetTemplate        func(ctx context.Context, templateID, userID string) (*identity.Template, error)
	GetTemplateByAlias func(ctx context.Context, alias, userID string) (*identity.Template, error)
	UpdateTemplate     func(ctx context.Context, templateID, userID string, u identity.TemplateUpdate) (*identity.Template, error)
	DeleteTemplate     func(ctx context.Context, templateID, userID string) error

	// API keys (account-scope management). CreateScopedAPIKey returns the
	// minted key including its one-time plaintext; agentID is "" for account
	// scope and a resolved agent id for agent scope.
	CreateScopedAPIKey func(ctx context.Context, userID, name, scope, agentID string, expiresAt *time.Time) (*identity.APIKey, error)
	ListAPIKeys        func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.APIKey, error)
	DeleteAPIKey       func(ctx context.Context, keyID, userID string) error

	// domain verification
	TouchDomainChecked func(ctx context.Context, domain, userID string) error
	VerifyDomain       func(ctx context.Context, domain, userID string) error
	// EnqueueSenderProvision (decision 4 / Slice 4) schedules SES sending-
	// identity provisioning for a verified domain. Called on every successful
	// verify check (newly OR already verified), so POST /domains/{domain}/verify
	// doubles as the forced sending re-check. Optional — nil when SES is not
	// configured (dev/self-host), leaving sending_status at none (relay From).
	EnqueueSenderProvision func(ctx context.Context, domain string)
	// VerifyProbe runs the live DNS check for a domain's published records.
	// Injected so it is fakeable in tests (the real one wraps
	// agent.CheckDomainRecords).
	VerifyProbe func(domain, verificationToken, dkimSelector, dkimPublicKey string) DomainCheckResult

	// Deployment info surfaced by GET /v1/info.
	SharedDomain string
	PublicURL    string

	// WSHandle serves the WebSocket upgrade for an agent address (the real-
	// time inbound transport). Injected so httpapi need not depend on the ws
	// package; the real one is ws.Handler.ServeWithEmail.
	WSHandle func(w http.ResponseWriter, r *http.Request, address string)

	// MagicLinkApprove / MagicLinkReject serve the HITL magic-link endpoints
	// at /v1/approve and /v1/reject: GET renders the token-gated confirmation
	// page, POST executes the action. Raw HTML handlers owned by
	// internal/agent (API.ApproveMagicLinkHandler / RejectMagicLinkHandler),
	// injected so the chi root serves them directly — the legacy mux no
	// longer registers these paths, and routeNotFound would answer them with
	// the JSON 404 envelope (it never consults Legacy for /v1/*).
	MagicLinkApprove http.Handler
	MagicLinkReject  http.Handler

	// Legacy is the existing gorilla/mux handler. The chi root falls back
	// to it for every route not yet ported onto Huma (the strangler), so
	// the service stays fully functional through the multi-sub-slice port.
	Legacy http.Handler

	// Metrics receives one HTTPRequest sample per served request (the
	// availability + latency SLI, docs/observability.md). Optional — nil
	// disables the instrumentation middleware.
	Metrics RequestMetrics
}

// Server is the v1 HTTP surface: a chi root router with the Huma API mounted
// on it and the legacy handler wired as the fallback.
type Server struct {
	Router chi.Router
	API    huma.API
	deps   Deps
}

// New builds the v1 server. It installs the e2a error envelope globally,
// stands up the Huma API on a chi router under the `/v1` documentation
// paths, registers the ported operations, and points chi's not-found/
// method-not-allowed handlers at the legacy surface.
func New(deps Deps) *Server {
	installErrorEnvelope()

	root := chi.NewRouter()
	root.Use(requestID)
	// After requestID so the sample times everything below it (auth, Huma,
	// handlers, legacy fallback). No-op when deps.Metrics is nil.
	root.Use(requestMetrics(deps.Metrics))
	root.Use(securityHeaders)
	root.Use(authChallenge(deps.AuthChallenge))
	root.Use(withRawRequest)

	config := huma.DefaultConfig("e2a API", APIVersion)
	// Reject bodies that are not valid UTF-8 BEFORE JSON decoding. This must
	// happen on the raw bytes: encoding/json silently launders invalid UTF-8
	// into U+FFFD, so a post-decode check can never see it (request_content.go
	// has the full rule). DefaultConfig registers the JSON format under both
	// its media type and its content-negotiation suffix; wrap every entry so a
	// format added later fails loudly here rather than bypassing the guard.
	for name, format := range config.Formats {
		config.Formats[name] = requireUTF8Body(format)
	}
	// Serve the spec and human docs under the versioned prefix so they sit
	// beside the operations (api-v1-redesign §1: everything lives under the
	// api host; here, under /v1).
	config.OpenAPIPath = "/v1/openapi"
	config.DocsPath = "/v1/docs"
	config.SchemasPath = "/v1/schemas"
	// Drop Huma's default schema-link transformer: it injects a `$schema`
	// field and Link header into response bodies, which would change the
	// clean contract shape this redesign is standardizing. Keep only our
	// request-id stamper.
	config.CreateHooks = nil
	config.Transformers = []huma.Transformer{stampRequestID}
	// The stability policy below is the contract's constitution — the
	// machine-readable markers it refers to (`additionalProperties`,
	// `x-stability-level`, `x-experimental-values`) are stamped
	// onto the document by applyEvolutionStance (stability.go). Keep them in sync.
	config.Info.Description = "e2a — authenticated email gateway for AI agents. v1 contract.\n\n" +
		"## Stability policy\n\n" +
		"The v1 surface is stable and evolves **additively only**: new endpoints, new optional request " +
		"fields, new response fields, and new values in open string sets (event types, statuses) may " +
		"appear at any time without a version bump. Clients MUST tolerate unknown response fields and " +
		"unknown values in open string sets. This is machine-readable in the schemas: response schemas " +
		"declare `additionalProperties: true`; request schemas stay strict (`additionalProperties: false` " +
		"— an unknown request field is rejected with 422).\n\n" +
		"Operations and schemas marked `x-stability-level: beta` are exempt from this freeze and may " +
		"change or be removed without a major version. A field marked `x-experimental-values` is itself " +
		"stable, but the listed values (and their event payloads) are experimental. Everything not marked " +
		"beta, or enumerated as experimental, is stable.\n\n" +
		"Removing or changing stable surface only happens on a new major version path (/v2); deprecations " +
		"are announced ahead of time via `deprecated: true` in this document and keep working within v1."
	// Canonical production host (api-v1-redesign §1: "Canonical base URL
	// https://api.e2a.dev/v1"). Operations already carry the /v1 prefix, so the
	// server URL stops at the host — otherwise clients would double it. Without a
	// servers block, generated SDKs default to http://localhost (a
	// Bearer-over-cleartext footgun).
	config.Servers = []*huma.Server{
		{URL: "https://api.e2a.dev", Description: "Production"},
	}
	// One auth scheme across the surface: a Bearer credential that is
	// either an API key or an OAuth 2.1 access token (api-v1-redesign §5).
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer": {
			Type:        "http",
			Scheme:      "bearer",
			Description: "API key (e2a_…) or OAuth 2.1 access token, sent as `Authorization: Bearer <token>`.",
		},
	}

	api := humachi.New(root, config)

	s := &Server{Router: root, API: api, deps: deps}
	// Rate limiting runs as Huma middleware so it can stamp the IETF
	// RateLimit-* headers on the response and short-circuit a 429 before the
	// handler. Registered once; applies to every operation.
	api.UseMiddleware(s.rateLimit)
	s.registerOperations()
	s.applyAuthenticationNullability()
	// Post-registration document passes, in order: drop the phantom
	// octet-stream request-body variants first (a Huma RawBody artifact), then
	// stamp the forward-compat stance onto the cleaned document — response
	// schemas open (additionalProperties: true), request schemas strict,
	// Stability markers derived from the beta operations. See
	// stability.go.
	s.suppressRawBodyOctetStream()
	s.applyEvolutionStance()
	s.applyResponseHeaderContract()
	// Last: publish the enforced-but-previously-unexpressed field bounds. It
	// runs after applyEvolutionStance so the stance pass cannot overwrite the
	// keywords it adds.
	s.applyContactMetadataBounds()

	// WebSocket transport — registered directly on chi (not Huma; it's a raw
	// upgrade, not a JSON operation). First-class /v1 inbound transport.
	if deps.WSHandle != nil {
		root.Get("/v1/agents/{email}/ws", func(w http.ResponseWriter, r *http.Request) {
			// chi routes on RawPath when the request URI is percent-encoded and
			// returns URL params STILL ENCODED — and every SDK client encodes the
			// address (encodeURIComponent), so without this decode the handler
			// looked up an agent literally named "x%40y" and 404'd every real
			// WebSocket client. Huma decodes its own params; this bypass route
			// must do it explicitly.
			address := chi.URLParam(r, "email")
			if decoded, err := url.PathUnescape(address); err == nil {
				address = decoded
			}
			deps.WSHandle(w, r, address)
		})
	}

	// Attachment download — raw chi route (not Huma): a binary stream authorized
	// by the capability token in the URL, not the bearer (§6a #5). The metadata
	// endpoint that mints these URLs IS a Huma operation (registerAttachments).
	if deps.AttachmentStore != nil {
		root.Get("/v1/agents/{email}/messages/{id}/attachments/{index}/download", s.handleAttachmentDownload)
	}

	// Managed unsubscribe is a bearer-capability route, not an authenticated
	// /v1 management operation. GET is deliberately read-only for link scanners.
	root.Handle("/u/{token}", http.HandlerFunc(s.handlePublicUnsubscribe))

	// HITL magic-link pages (approve/reject confirmation + execution). Raw
	// token-gated HTML handlers owned by internal/agent — NOT Huma
	// operations. They must be registered here explicitly: routeNotFound
	// short-circuits unmatched /v1/* with the JSON envelope and never
	// consults Legacy, so without these registrations every approve/reject
	// link in notification emails 404s.
	if deps.MagicLinkApprove != nil {
		root.Handle("/v1/approve", deps.MagicLinkApprove)
	}
	if deps.MagicLinkReject != nil {
		root.Handle("/v1/reject", deps.MagicLinkReject)
	}

	root.NotFound(s.routeNotFound)
	root.MethodNotAllowed(s.routeMethodNotAllowed)
	return s
}

func isV1Path(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}

func (s *Server) routeNotFound(w http.ResponseWriter, r *http.Request) {
	if isV1Path(r.URL.Path) {
		WriteError(w, r, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if s.deps.Legacy != nil {
		s.deps.Legacy.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// probedMethods is the set of methods allowHeaderValue asks the router about.
// It is exactly chi v5.3.1's built-in method table (tree.go `methodMap`),
// including the QUERY method (RFC 10008) that chi routes via Router.Query —
// no route registers QUERY today, so it is probed for future routes, not for
// current ones. chi does not export that table, so this list is a mirror
// rather than a derivation: TestProbedMethodsMatchChiMethodTable discovers
// chi's real set at runtime and fails if the two ever diverge. Custom methods
// added via the package-global chi.RegisterMethod are the one thing this
// cannot track — chi exposes no way to enumerate them from here, so a caller
// that registers one must extend this list by hand (that test will say so).
var probedMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodConnect,
	http.MethodOptions, http.MethodTrace, "QUERY",
}

// allowHeaderValue derives the Allow header for a 405 response (RFC 9110
// §15.5.6: an origin server MUST generate Allow on a 405) by asking the router
// itself which methods route this request's path. chi v5 collects the matched
// node's method set internally but does not expose it to a custom
// MethodNotAllowed handler, so this re-runs the match once per method in
// probedMethods — ten tree lookups on a rare error path. Probing the live
// routing table keeps the header in lockstep with the routes chi knows about;
// nothing is hardcoded per route.
//
// An empty result is meaningful, not a failure: chi dispatches
// MethodNotAllowedHandler with no allowed methods when the request method is
// absent from its table (mux.go routeHTTP), so an unknown method against a
// nonexistent path lands here with every probe missing. RFC 9110 §10.2.1
// permits an empty Allow field value to mean "no methods are supported", which
// is the honest answer there — callers must still set the header.
func (s *Server) allowHeaderValue(r *http.Request) string {
	// Mirror chi's own routing-path selection: it routes on RawPath when the
	// request URI is percent-encoded (see the WS route comment above).
	//
	// chi actually consults rctx.RoutePath first and only falls back to
	// RawPath/Path; RoutePath is populated for requests routed into a
	// subrouter via Router.Mount. This repo mounts no subrouters, so RoutePath
	// is always empty here and this mirror is exact — but if a Mount is ever
	// added, this probe must consult chi.RouteContext(r.Context()).RoutePath
	// too or it will probe the outer path and derive the wrong method set.
	routePath := r.URL.RawPath
	if routePath == "" {
		routePath = r.URL.Path
	}
	var allowed []string
	for _, method := range probedMethods {
		if s.Router.Match(chi.NewRouteContext(), method, routePath) {
			allowed = append(allowed, method)
		}
	}
	return strings.Join(allowed, ", ")
}

func (s *Server) routeMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	if isV1Path(r.URL.Path) {
		// Set unconditionally: an empty value is a valid Allow (RFC 9110
		// §10.2.1) and omitting the header violates §15.5.6.
		w.Header().Set("Allow", s.allowHeaderValue(r))
		WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	// Non-/v1 paths belong to the legacy gorilla/mux surface, which emits its
	// own 405 without an Allow header (gorilla does not hand the allowed-method
	// set to a MethodNotAllowedHandler). Allow coverage here therefore stops at
	// the /v1 surface plus /u/{token}; the legacy 405s are a separate fix.
	if s.deps.Legacy != nil {
		s.deps.Legacy.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Allow", s.allowHeaderValue(r))
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}

// ServeHTTP makes Server a drop-in http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Router.ServeHTTP(w, r)
}

// OpenAPIYAML renders the generated spec as YAML. Used by the codegen step
// and the drift test — the spec is emitted from the live handlers, never
// hand-authored.
func (s *Server) OpenAPIYAML() ([]byte, error) {
	return s.API.OpenAPI().YAML()
}

// registerOperations wires every ported Huma operation. As resources move
// off the legacy mux they are added here and removed from the legacy
// RegisterRoutes in the same commit.
func (s *Server) registerOperations() {
	s.registerInfo()
	s.registerAgents()
	s.registerMessages()
	s.registerMessageLifecycle()
	s.registerAgentMetrics()
	s.registerAccountMetrics()
	s.registerAttachments()
	s.registerConversations()
	s.registerAgentWrites()
	s.registerAgentProtection()
	s.registerDomains()
	s.registerWebhooks()
	s.registerTemplates()
	s.registerStarterTemplates()
	s.registerEvents()
	s.registerAccount()
	s.registerAgentSuppressions()
	s.registerContacts()
	s.registerContactImport()
	s.registerEngagements()
	s.registerAPIKeys()
	s.registerOutbound()
	s.registerSendBatch()
	s.registerReviews()
	// Not an operation: exports the typed per-event `data` payload schemas
	// (EmailReceivedData, …) into components.schemas for docs + codegen.
	s.registerEventPayloadSchemas()
	// Preserve the intentionally public schemas that are selected through
	// string-valued metadata rather than referenced by an HTTP operation.
	s.registerStandaloneSchemaExports()
}

// suppressRawBodyOctetStream removes the phantom `application/octet-stream`
// request-body variant Huma adds for every input struct that carries a
// `RawBody []byte` capture field (send/reply/forward/approve keep the raw
// bytes for the Idempotency-Key body hash). The field is a server-side
// artifact — those operations accept ONLY application/json — but Huma
// unconditionally documents a binary media type for it
// (setRequestBodyFromRawBody), so clients generating from the spec would see
// a bogus content type they must "choose" between. Tagging the field with
// contentType:"application/json" is not an option: Huma would then OVERWRITE
// the JSON media type's schema with a bare binary string.
//
// Runtime behavior is untouched: Huma parses non-multipart request bodies via
// the Body schema captured at Register time and never consults
// RequestBody.Content afterwards (the only runtime readers are the multipart
// path and RequestBody.Required, both unaffected). The octet-stream entry is
// dropped only where a JSON variant coexists, so a future genuinely-binary
// endpoint would keep its declared content type.
func (s *Server) suppressRawBodyOctetStream() {
	for _, item := range s.API.OpenAPI().Paths {
		for _, op := range []*huma.Operation{
			item.Get, item.Put, item.Post, item.Delete,
			item.Options, item.Head, item.Patch, item.Trace,
		} {
			if op == nil || op.RequestBody == nil || op.RequestBody.Content == nil {
				continue
			}
			c := op.RequestBody.Content
			if c["application/json"] != nil && c["application/octet-stream"] != nil {
				delete(c, "application/octet-stream")
			}
		}
	}
}

// reqCtxKey carries the raw *http.Request through to Huma handlers so they
// can reuse the injected Authenticator (which reads headers + cookies).
type reqCtxKey struct{}

// withRawRequest stashes the request so Huma handlers can recover it for
// the auth path. Storing the request in its own derived context is the
// standard bridge; only headers/cookies are read downstream, so the
// pre-derivation request is equivalent for authentication.
func withRawRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), reqCtxKey{}, r)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestFromContext recovers the raw request stashed by withRawRequest.
func RequestFromContext(ctx context.Context) *http.Request {
	if r, ok := ctx.Value(reqCtxKey{}).(*http.Request); ok {
		return r
	}
	return nil
}

// requireUser authenticates the caller or returns a 401 envelope carrying
// the machine-branchable "unauthorized" code.
func (s *Server) requireUser(ctx context.Context) (*identity.User, error) {
	p, err := s.requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	return p.User, nil
}

// requirePrincipal authenticates the caller and returns the full principal
// (user + scope + bound agent), or a 401 envelope. The scope-aware basis for
// the hard scope ceiling (requireAccountScope / requireAgentAccess).
func (s *Server) requirePrincipal(ctx context.Context) (*identity.Principal, error) {
	// The rate-limit middleware may have already authenticated this request
	// on the read path; reuse that principal instead of hitting auth twice.
	if p := principalFromContext(ctx); p != nil {
		return p, nil
	}
	r := RequestFromContext(ctx)
	if r == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "authentication unavailable")
	}
	p, err := s.resolvePrincipal(r)
	if err != nil {
		return nil, NewError(http.StatusUnauthorized, "unauthorized", "authentication required")
	}
	return p, nil
}

// resolvePrincipal runs the injected auth path. It prefers the scope-aware
// PrincipalAuthenticator; if only the legacy Authenticator is wired it treats
// the caller as account-scoped (pre-Slice-5a behavior — no scope ceiling).
func (s *Server) resolvePrincipal(r *http.Request) (*identity.Principal, error) {
	if s.deps.PrincipalAuthenticator != nil {
		return s.deps.PrincipalAuthenticator(r)
	}
	if s.deps.Authenticator == nil {
		return nil, fmt.Errorf("authentication unavailable")
	}
	u, err := s.deps.Authenticator(r)
	if err != nil {
		return nil, err
	}
	return &identity.Principal{User: u, Scope: identity.ScopeAccount}, nil
}
