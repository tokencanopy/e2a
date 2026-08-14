// Package apiserver builds the process HTTP handler: the typed Huma /v1
// surface (internal/httpapi) wrapping the legacy gorilla/mux fallback.
//
// It exists so the production binary (cmd/e2a) and the contract-test harness
// (internal/testutil) construct the SAME handler from the SAME dependency
// wiring. Before this, the contract harness served only the legacy mux, so it
// could not exercise /v1 at all and would silently drift from production. With
// one builder, a dep that production wires but the harness forgets shows up as
// a failing contract test, not a silent gap.
package apiserver

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/httpapi"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/sendramp"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhook"
)

// Params bundles the already-constructed components the /v1 Deps closures bind
// to. Callers build these once (the binary for real, the harness for tests)
// and hand them over; the builder owns only the mapping into httpapi.Deps.
type Params struct {
	API             *agent.API
	Store           *identity.Store
	Enforcer        *limits.DBEnforcer
	UsageStore      *usage.Store
	SubscriberStore *webhook.SubscriberStore
	Idempotency     *idempotency.Store
	Pool            *pgxpool.Pool

	SMTPDomain   string
	SharedDomain string
	PublicURL    string
	Production   bool

	// SESRegion is the SES sending-identity region (config
	// sender_identity.ses_region). Non-empty enables the sending feature, which
	// makes domainView emit the deterministic mail_from_* DNS records. Mirrors
	// the gate used to wire SenderIdentity below.
	SESRegion string

	// SigningSecret is the deployment HMAC secret (config.Signing.HMACSecret) —
	// used to mint/verify short-lived attachment download tokens (§6a #5), the
	// same primitive as the HITL magic-link. When empty, attachment endpoints
	// are left unwired.
	SigningSecret string

	// EventsEnabled mirrors the outbox's flag (now unconditional in production).
	// When false the webhook_events durable log is never written, so the events
	// list/get/redeliver endpoints return 501 events_log_disabled instead of an
	// empty result. Webhook delivery (River) is unaffected.
	EventsEnabled bool

	// Legacy is the gorilla/mux handler the chi root falls back to for any
	// route not on /v1. WSHandle serves the /v1 WebSocket upgrade.
	Legacy   http.Handler
	WSHandle func(w http.ResponseWriter, r *http.Request, address string)

	// Metrics feeds the per-request HTTP SLI middleware (availability +
	// latency). Optional — nil leaves the /v1 surface uninstrumented.
	Metrics httpapi.RequestMetrics

	// SenderIdentity (decision 4 / Slice 4) schedules SES sending-identity
	// provisioning on domain verify and teardown on domain delete. Optional —
	// nil when SES is not configured (dev/self-host), leaving sending_status
	// at none and the relay From in place. *senderidentity.Manager satisfies it.
	SenderIdentity SenderIdentityEnqueuer

	// EnqueueDelivery enqueues a River webhook_deliver job for a
	// webhook_subscriber_deliveries row inserted directly by the /test endpoint
	// or the event-redelivery API (both bypass the outbox drain). River is the
	// sole delivery engine, so without this those rows would never deliver.
	// Wired from *webhookdelivery.Jobs.EnqueueDelivery in the binary. Optional —
	// nil in minimal test setups with no River client.
	EnqueueDelivery func(ctx context.Context, deliveryID string) error

	// AgentSuppressionAddedHook writes the beta suppression event in the same
	// transaction as any newly inserted agent suppression, whether it came from
	// authenticated management or a recipient token. Optional until the event
	// slice is wired by the caller.
	AgentSuppressionAddedHook identity.AgentSuppressionTxHook
	ManagedUnsubscribeIssuer  agent.ManagedUnsubscribeIssuer
}

// SenderIdentityEnqueuer is the slice of *senderidentity.Manager apiserver
// needs. Defined as an interface so apiserver does not hard-depend on the
// senderidentity package (River + AWS SDK) just to wire two optional deps.
type SenderIdentityEnqueuer interface {
	EnqueueProvision(ctx context.Context, domain string) error
	EnqueueProvisionTx(ctx context.Context, tx pgx.Tx, domain string) error
	EnqueueDeprovisionTx(ctx context.Context, tx pgx.Tx, domain string) error
	// TryDeprovisionNow is the post-commit best-effort provider convergence for
	// a just-deleted domain. Errors are logged, never returned to the client.
	TryDeprovisionNow(ctx context.Context, domain string) error
}

// BuildDeps maps Params into the httpapi dependency set. Kept as the single
// definition of the /v1 wiring so production and tests cannot diverge.
func BuildDeps(p Params) httpapi.Deps {
	if p.API != nil {
		p.API.SetManagedUnsubscribeIssuer(p.ManagedUnsubscribeIssuer)
	}
	var rampSnapshot func(context.Context, string, string, time.Time) (sendramp.Snapshot, error)
	if p.Pool != nil {
		rampSnapshot = sendramp.NewStore(p.Pool).Snapshot
	}
	var listMessageLifecycle httpapi.MessageLifecycleLister
	var countAgentMetrics httpapi.AgentMetricsCounter
	var countAccountMetrics httpapi.AccountMetricsCounter
	var countWebhookDeliveries httpapi.WebhookDeliveryCounter
	if p.Pool != nil {
		countWebhookDeliveries = webhook.NewSubscriberStore(p.Pool).CountDeliveriesForAccount
		lifecycleStore := messagelifecycle.NewStore(p.Pool)
		listMessageLifecycle = lifecycleStore.ListForMessage
		countAgentMetrics = lifecycleStore.CountByReasonCode
		countAccountMetrics = lifecycleStore.CountByReasonCodeForAccount
	}
	deps := httpapi.Deps{
		Authenticator:          p.API.AuthenticateUser,
		PrincipalAuthenticator: p.API.AuthenticatePrincipal,
		AuthChallenge:          p.API.WWWAuthenticateChallenge,
		MagicLinkApprove:       p.API.ApproveMagicLinkHandler(),
		MagicLinkReject:        p.API.RejectMagicLinkHandler(),
		ListAgents:             p.Store.ListAgentsByUser,
		GetAgent:               p.Store.GetAgentByEmail,
		GetMessage:             p.Store.GetMessageWithContent,
		AttachmentStore:        attachmentStore(p),
		ListMessages:           p.Store.GetMessagesByAgent,
		ListMessageLifecycle:   listMessageLifecycle,
		CountAgentMetrics:      countAgentMetrics,
		CountAccountMetrics:    countAccountMetrics,
		CountWebhookDeliveries: countWebhookDeliveries,
		ModifyMessageLabels:    p.Store.ModifyMessageLabels,

		ListConversations: p.Store.ListConversationsByAgent,
		GetConversation:   p.Store.GetConversationByID,

		CreateAgent:          p.Store.CreateAgent,
		LookupDomain:         p.Store.LookupDomain,
		LookupCoveringDomain: p.Store.LookupCoveringDomain,
		// Exact-domain MX probe for the create-time two-way-inbox gate.
		// net.Resolver honors the ctx deadline the handler sets.
		ResolveMX: func(ctx context.Context, name string) ([]string, error) {
			var r net.Resolver
			mxs, err := r.LookupMX(ctx, name)
			if err != nil {
				var dnsErr *net.DNSError
				if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
					return []string{}, nil
				}
				return nil, err
			}
			hosts := make([]string, len(mxs))
			for i, m := range mxs {
				hosts[i] = m.Host
			}
			return hosts, nil
		},
		EnforceAgentCreate:    p.Enforcer.CheckAgentCreate,
		UpdateAgentName:       p.Store.UpdateAgentName,
		UpdateAgentProtection: p.Store.UpdateAgentProtection,
		// Trash semantics (docs/design/trash-soft-delete.md): the default
		// delete is soft; the hard delete sits behind ?permanent=true.
		DeleteAgent:          p.Store.SoftDeleteAgent,
		PermanentDeleteAgent: p.Store.DeleteAgentIncarnation,
		RestoreAgent:         p.Store.RestoreAgent,
		GetAgentAnyState:     p.Store.GetAgentByIDAnyState,
		ListDeletedAgents:    p.Store.ListDeletedAgentsByUser,
		DeleteMessage:        p.Store.SoftDeleteMessage,
		RestoreMessage:       p.Store.RestoreMessage,
		PurgeMessage:         p.Store.PurgeMessage,

		ListDomains:            p.Store.ListDomainsByUser,
		SendingRampSnapshot:    rampSnapshot,
		ClaimDomain:            p.Store.ClaimOrCreateDomain,
		EnforceDomainCreate:    p.Enforcer.CheckDomainCreate,
		DeleteDomain:           deleteDomainFunc(p),
		CountAgentsOnDomain:    p.Store.CountAgentsOnDomain,
		SMTPDomain:             p.SMTPDomain,
		SESRegion:              p.SESRegion,
		CursorSecret:           p.SigningSecret,
		EventsEnabled:          p.EventsEnabled,
		Idempotency:            p.Idempotency,
		DeliverOutbound:        p.API.DeliverOutbound,
		SendTest:               p.API.SendTestCore,
		PollSendOutcome:        p.Store.GetSendOutcome,
		ApprovePending:         p.API.ApprovePendingCore,
		SendLimit:              p.API.SendLimitAllow,
		PollLimit:              p.API.PollLimitAllow,
		RegLimit:               p.API.RegLimitAllow,
		DownloadLimit:          p.API.DownloadLimitAllow,
		UnsubscribeLimit:       p.API.UnsubscribeLimitAllow,
		RejectPending:          p.API.RejectPendingCore,
		GetReviewMessage:       p.Store.GetReviewMessage,
		ApproveInboundReview:   p.API.ApproveInboundReviewCore,
		RejectInboundReview:    p.API.RejectInboundReviewCore,
		ListReviews:            p.Store.ListReviews,
		GetReviewWithContent:   p.Store.GetReviewWithContent,
		EnforceMessageSend:     p.Enforcer.CheckMessageSend,
		GetRepliableMessage:    p.Store.GetRepliableMessage,
		GetLimits:              p.Enforcer.Get,
		ExportUserData:         p.API.ExportUserDataCore,
		DeleteUserData:         p.API.DeleteUserDataCore,
		ListSuppressions:       p.Store.ListSuppressions,
		RemoveSuppression:      p.Store.RemoveSuppression,
		AddAgentSuppression:    p.Store.AddAgentSuppression,
		ListAgentSuppressions:  p.Store.ListAgentSuppressions,
		RemoveAgentSuppression: p.Store.RemoveAgentSuppression,

		CreateContact:            p.Store.CreateContact,
		GetContact:               p.Store.GetContactByAddress,
		ListContacts:             p.Store.ListContacts,
		UpdateContact:            p.Store.UpdateContact,
		UpdateContactIfUnchanged: p.Store.UpdateContactIfUnchanged,
		DeleteContact:            p.Store.DeleteContact,

		UpsertEngagement:            p.Store.UpsertEngagement,
		UpdateEngagementIfUnchanged: p.Store.UpdateEngagementIfUnchanged,
		GetEngagement:               p.Store.GetEngagement,
		ListEngagements:             p.Store.ListEngagements,
		DeleteEngagement:            p.Store.DeleteEngagement,

		ImportContacts:            p.Store.ImportContacts,
		ImportContactsWithOptions: p.Store.ImportContactsWithOptions,
		DeleteImportBatch:         p.Store.DeleteImportBatch,
		EffectiveSuppressions:     p.Store.EffectiveSuppressions,
		// Account-wide suppression view for import marking: agentID is empty so
		// this asks only "has the account blocked this address", which is the
		// right scope for an import that is not yet bound to a sending agent.
		SuppressedAddresses: func(ctx context.Context, userID string, addresses []string) ([]string, error) {
			return p.Store.EffectiveSuppressions(ctx, userID, "", addresses)
		},

		ListProtectionEventsByMessage: p.Store.ListProtectionEventsByMessage,
		GetUsage: func(ctx context.Context, userID string) httpapi.LimitsUsageView {
			var u httpapi.LimitsUsageView
			if n, err := p.UsageStore.CountAgentsByUser(ctx, userID); err == nil {
				u.Agents = n
			}
			if n, err := p.UsageStore.CountDomainsByUser(ctx, userID); err == nil {
				u.Domains = n
			}
			if n, err := p.UsageStore.MessagesThisMonth(ctx, userID); err == nil {
				u.MessagesMonth = n
			}
			if n, err := p.UsageStore.GetStorageBytes(ctx, userID); err == nil {
				u.StorageBytes = n
			}
			return u
		},

		ListEvents: func(ctx context.Context, q httpapi.EventQuery) ([]agent.EventView, error) {
			return agent.ListEventsForUser(ctx, p.Pool, q.UserID, q.Type, q.AgentID, q.ConversationID, q.MessageID, q.Since, q.Until, q.CursorCreatedAt, q.CursorID, q.Limit)
		},
		GetEvent2: func(ctx context.Context, userID, eventID string) (*agent.EventView, error) {
			return agent.GetEventForUser(ctx, p.Pool, userID, eventID)
		},
		LoadReplayEvent: func(ctx context.Context, userID, eventID string) (*agent.ReplayEvent, error) {
			return agent.LoadReplayEvent(ctx, p.Pool, userID, eventID)
		},
		InsertReplayDelivery: func(ctx context.Context, eventID, webhookID, eventType string, messageID *string, envelope []byte) (string, error) {
			return agent.InsertReplayDelivery(ctx, p.Pool, eventID, webhookID, eventType, messageID, envelope)
		},

		CreateWebhook:     p.Store.CreateWebhookIdem,
		ListWebhooks:      p.Store.ListWebhooksByUser,
		GetWebhook:        p.Store.GetWebhookByID,
		UpdateWebhook:     p.Store.UpdateWebhook,
		DeleteWebhook:     p.Store.DeleteWebhook,
		RotateSecret:      p.Store.RotateSecret,
		TestWebhookInsert: p.SubscriberStore.InsertPendingForTest,
		ListDeliveries:    p.SubscriberStore.ListDeliveriesByWebhook,
		EnqueueDelivery:   p.EnqueueDelivery,

		CreateTemplate:     p.Store.CreateTemplate,
		ListTemplates:      p.Store.ListTemplatesByUser,
		GetTemplate:        p.Store.GetTemplateByID,
		GetTemplateByAlias: p.Store.GetTemplateByAlias,
		UpdateTemplate:     p.Store.UpdateTemplate,
		DeleteTemplate:     p.Store.DeleteTemplate,

		CreateScopedAPIKey: p.Store.CreateScopedAPIKey,
		ListAPIKeys:        p.Store.ListAPIKeys,
		DeleteAPIKey:       p.Store.DeleteAPIKey,

		TouchDomainChecked: p.Store.TouchDomainLastChecked,
		VerifyDomain:       verifyDomainFunc(p),
		VerifyProbe: func(domain, token, dkimSel, dkimKey string) httpapi.DomainCheckResult {
			c := agent.CheckDomainRecords(domain, p.SMTPDomain, token, dkimSel, dkimKey, p.Production)
			return httpapi.DomainCheckResult{TXTFound: c.TXTFound, MX: c.MX, SPF: c.SPF, DKIM: c.DKIM}
		},
		EnqueueSenderProvision: enqueueSenderProvisionFunc(p),

		SharedDomain: p.SharedDomain,
		PublicURL:    p.PublicURL,
		WSHandle:     p.WSHandle,
		Legacy:       p.Legacy,
		Metrics:      p.Metrics,
	}
	deps.ResolveUnsubscribeToken = p.Store.ResolveUnsubscribeToken
	deps.AddAgentSuppressionFromTokenScope = p.Store.AddAgentSuppressionFromTokenScope
	deps.AgentSuppressionAddedHook = p.AgentSuppressionAddedHook
	return deps
}

func verifyDomainFunc(p Params) func(ctx context.Context, domain, userID string) error {
	if p.SenderIdentity == nil {
		return p.Store.VerifyDomain
	}
	return func(ctx context.Context, domain, userID string) error {
		return p.Store.VerifyDomainTx(ctx, domain, userID, func(ctx context.Context, tx pgx.Tx) error {
			return p.SenderIdentity.EnqueueProvisionTx(ctx, tx, domain)
		})
	}
}

// bestEffortDeprovisionTimeout bounds the post-commit provider convergence a
// DELETE /domains response waits for. Generous enough for Deprovision's
// bounded absence-confirmation polling (~3s worst case) against a healthy
// provider; a degraded provider hits this instead of holding the response.
const bestEffortDeprovisionTimeout = 10 * time.Second

// deleteDomainFunc wires DELETE /domains. The transaction commits the guarded
// domain-row delete together with the durable teardown job (the success
// boundary: the delete cannot be lost even with SES unreachable). A
// best-effort provider deprovision then runs post-commit, so the SES identity
// is usually already confirmed absent when the response returns — but a
// provider failure there (outage, throttle, foreign/untagged identity) is
// logged and left to the committed job + hourly reaper to converge, never
// surfaced to the client. Without SES configured, this is a plain delete.
func deleteDomainFunc(p Params) func(ctx context.Context, domain, userID string) error {
	if p.SenderIdentity == nil {
		return p.Store.DeleteDomain
	}
	return func(ctx context.Context, domain, userID string) error {
		if err := p.Store.DeleteDomainTx(ctx, domain, userID, func(ctx context.Context, tx pgx.Tx) error {
			return p.SenderIdentity.EnqueueDeprovisionTx(ctx, tx, domain)
		}); err != nil {
			return err
		}
		depCtx, cancel := context.WithTimeout(ctx, bestEffortDeprovisionTimeout)
		defer cancel()
		if err := p.SenderIdentity.TryDeprovisionNow(depCtx, domain); err != nil {
			log.Printf("[apiserver] best-effort sender-identity deprovision for %s: %v (async teardown will converge)", domain, err)
		}
		return nil
	}
}

// enqueueSenderProvisionFunc wires the verify-time provisioning hook. Nil when
// SES is not configured (the httpapi handler then no-ops). Best-effort: a
// failed enqueue is logged and recovered by the next POST /verify.
func enqueueSenderProvisionFunc(p Params) func(ctx context.Context, domain string) {
	if p.SenderIdentity == nil {
		return nil
	}
	return func(ctx context.Context, domain string) {
		if err := p.SenderIdentity.EnqueueProvision(ctx, domain); err != nil {
			log.Printf("[apiserver] enqueue sender provision for %s: %v", domain, err)
		}
	}
}

// attachmentStore wires the default (native) attachment store when the signing
// secret + public URL are present; returns nil otherwise (attachment endpoints
// stay unwired, e.g. in minimal test setups) — the handlers guard on nil.
func attachmentStore(p Params) httpapi.AttachmentStore {
	if p.SigningSecret == "" || p.PublicURL == "" {
		return nil
	}
	return httpapi.NewNativeAttachmentStore(p.SigningSecret, p.PublicURL)
}

// New builds the process HTTP handler (chi root owning /v1, legacy fallback).
func New(p Params) *httpapi.Server {
	return httpapi.New(BuildDeps(p))
}
