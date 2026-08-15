package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/domainteardown"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/startertemplates"
	"github.com/tokencanopy/e2a/internal/webhook"
)

// sampleAgent is the canonical fixture agent owned by user u_1.
func sampleAgent() identity.AgentIdentity {
	return identity.AgentIdentity{
		ID:                   "support@acme.com",
		Domain:               "acme.com",
		Name:                 "Acme Support",
		DomainVerified:       true,
		UserID:               "u_1",
		CreatedAt:            time.Unix(1700000000, 0).UTC(),
		HITLTTLSeconds:       604800,
		HITLExpirationAction: "reject",
	}
}

// lastDelivered captures the most recent SendRequest handed to the fake
// DeliverOutbound so template-send tests can assert RENDERED content reached
// the delivery seam (the same seam that persists a HITL draft). The mutex
// makes the cross-goroutine handoff (server handler goroutine → test
// goroutine) race-detector clean.
var lastDelivered struct {
	mu  sync.Mutex
	req outbound.SendRequest
}

func recordDelivered(req outbound.SendRequest) {
	lastDelivered.mu.Lock()
	defer lastDelivered.mu.Unlock()
	lastDelivered.req = req
}

func lastDeliveredReq() outbound.SendRequest {
	lastDelivered.mu.Lock()
	defer lastDelivered.mu.Unlock()
	return lastDelivered.req
}

// sampleTemplate is the canonical fixture template owned by user u_1. Its
// subject/body/html exercise escaped, raw and dot-path interpolation.
func sampleTemplate() identity.Template {
	return identity.Template{
		ID: "tmpl_1", UserID: "u_1", Name: "Welcome", Alias: "welcome",
		Subject:   "Hello {{name}}",
		Body:      "Hi {{name}}, your plan is {{plan.tier}}.",
		HTMLBody:  "<p>Hi {{name}}: {{{markup}}}</p>",
		CreatedAt: time.Unix(1700000000, 0).UTC(),
		UpdatedAt: time.Unix(1700000000, 0).UTC(),
	}
}

// testServer builds a Server with fake collaborators and a sentinel legacy
// handler, returning an httptest server so tests exercise the real chi+Huma
// stack over the wire (transport layer in scope per the implement skill).
func testServer(t *testing.T, opts ...func(*Deps)) *httptest.Server {
	t.Helper()
	deps := Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			switch r.Header.Get("Authorization") {
			case "Bearer good":
				return &identity.User{ID: "u_1", Email: "owner@acme.com"}, nil
			case "Bearer overcap":
				return &identity.User{ID: "u_overcap", Email: "full@acme.com"}, nil
			default:
				return nil, errors.New("unauthorized")
			}
		},
		ListAgents: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.AgentIdentity, error) {
			if userID != "u_1" {
				return nil, errors.New("unexpected user")
			}
			return []identity.AgentIdentity{sampleAgent()}, nil
		},
		GetAgent: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			if address == "support@acme.com" {
				a := sampleAgent()
				return &a, nil
			}
			return nil, errors.New("not found")
		},
		// Any-state resolution mirrors the live getter by default (no trashed
		// agents in the base fixture); trash tests override it.
		GetAgentAnyState: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			if address == "support@acme.com" {
				a := sampleAgent()
				return &a, nil
			}
			return nil, errors.New("not found")
		},
		CreateTemplate: func(ctx context.Context, userID string, in identity.TemplateCreate) (*identity.Template, error) {
			// "welcome" is held by the fixture template tmpl_1, so a create
			// defaulting to a starter's alias collides exactly like "taken".
			if in.Alias == "taken" || in.Alias == "welcome" {
				return nil, identity.ErrTemplateAliasTaken
			}
			if userID == "u_overcap" {
				return nil, identity.ErrTemplateLimitReached
			}
			return &identity.Template{
				ID: "tmpl_new", UserID: userID, Name: in.Name, Alias: in.Alias,
				Subject: in.Subject, Body: in.Body, HTMLBody: in.HTMLBody,
				FromStarterAlias: in.FromStarterAlias, FromStarterVersion: in.FromStarterVersion,
				CreatedAt: time.Unix(1700000000, 0).UTC(), UpdatedAt: time.Unix(1700000000, 0).UTC(),
			}, nil
		},
		ListTemplates: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.TemplateSummary, error) {
			tp := sampleTemplate()
			return []identity.TemplateSummary{{
				ID: tp.ID, UserID: tp.UserID, Name: tp.Name, Alias: tp.Alias,
				Subject: tp.Subject, CreatedAt: tp.CreatedAt, UpdatedAt: tp.UpdatedAt,
			}}, nil
		},
		GetTemplate: func(ctx context.Context, templateID, userID string) (*identity.Template, error) {
			switch {
			case userID != "u_1":
				return nil, identity.ErrTemplateNotFound
			case templateID == "tmpl_1":
				tp := sampleTemplate()
				return &tp, nil
			case templateID == "tmpl_stale":
				// A stored row whose source no longer parses (simulates a
				// pre-tightening legacy row) — the send path must map its
				// parse failure to template_render_failed.
				tp := sampleTemplate()
				tp.ID = "tmpl_stale"
				tp.Body = "{{#section}}"
				return &tp, nil
			case templateID == "tmpl_onlyvar":
				// Subject is a single variable — empty template_data renders
				// an empty subject, the template_rendered_empty case.
				tp := sampleTemplate()
				tp.ID = "tmpl_onlyvar"
				tp.Subject = "{{name}}"
				tp.Body = "static body"
				tp.HTMLBody = ""
				return &tp, nil
			case templateID == "tmpl_dberr":
				// A store failure that is NOT a miss — handlers must 500,
				// never collapse it to 404.
				return nil, context.DeadlineExceeded
			default:
				return nil, identity.ErrTemplateNotFound
			}
		},
		GetTemplateByAlias: func(ctx context.Context, alias, userID string) (*identity.Template, error) {
			if alias == "welcome" && userID == "u_1" {
				tp := sampleTemplate()
				return &tp, nil
			}
			if alias == "starter-welcome" && userID == "u_1" {
				// A user template created from the `welcome` starter (the
				// from_starter copy is verbatim) — the end-to-end templated
				// send fixture.
				m, ok := startertemplates.Get("welcome")
				if !ok {
					return nil, identity.ErrTemplateNotFound
				}
				return &identity.Template{
					ID: "tmpl_starter", UserID: "u_1", Name: m.Name, Alias: "starter-welcome",
					Subject: m.Subject, Body: m.TextBody, HTMLBody: m.HTMLBody,
					CreatedAt: time.Unix(1700000000, 0).UTC(), UpdatedAt: time.Unix(1700000000, 0).UTC(),
				}, nil
			}
			return nil, identity.ErrTemplateNotFound
		},
		UpdateTemplate: func(ctx context.Context, templateID, userID string, u identity.TemplateUpdate) (*identity.Template, error) {
			if templateID != "tmpl_1" || userID != "u_1" {
				return nil, identity.ErrTemplateNotFound
			}
			if u.Alias != nil && *u.Alias == "taken" {
				return nil, identity.ErrTemplateAliasTaken
			}
			// Apply ALL five pointer fields, mirroring the real store —
			// including clear-to-"" for Alias/HTMLBody (stored as NULL,
			// round-tripped as "").
			tp := sampleTemplate()
			if u.Name != nil {
				tp.Name = *u.Name
			}
			if u.Alias != nil {
				tp.Alias = *u.Alias
			}
			if u.Subject != nil {
				tp.Subject = *u.Subject
			}
			if u.Body != nil {
				tp.Body = *u.Body
			}
			if u.HTMLBody != nil {
				tp.HTMLBody = *u.HTMLBody
			}
			return &tp, nil
		},
		DeleteTemplate: func(ctx context.Context, templateID, userID string) error {
			if templateID == "tmpl_1" && userID == "u_1" {
				return nil
			}
			return identity.ErrTemplateNotFound
		},
		CreateScopedAPIKey: func(ctx context.Context, userID, name, scope, agentID string, expiresAt *time.Time) (*identity.APIKey, error) {
			if userID != "u_1" {
				return nil, errors.New("unexpected user")
			}
			var agentCol *string
			if agentID != "" {
				a := agentID
				agentCol = &a
			}
			return &identity.APIKey{
				ID: "apk_new", UserID: userID, Name: name,
				KeyPrefix: "e2a_" + scope[:3] + "_abcd", PlaintextKey: "e2a_" + scope + "_secret",
				Scope: scope, AgentID: agentCol,
				CreatedAt: time.Unix(1700000400, 0).UTC(), ExpiresAt: expiresAt,
			}, nil
		},
		ListAPIKeys: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.APIKey, error) {
			if userID != "u_1" {
				return nil, errors.New("unexpected user")
			}
			return []identity.APIKey{{
				ID: "apk_1", UserID: userID, Name: "default",
				KeyPrefix: "e2a_acct_abcd", Scope: "account",
				CreatedAt: time.Unix(1700000100, 0).UTC(),
			}}, nil
		},
		DeleteAPIKey: func(ctx context.Context, keyID, userID string) error {
			if keyID == "apk_1" && userID == "u_1" {
				return nil
			}
			return identity.ErrAPIKeyNotFound
		},
		ListMessages: func(ctx context.Context, f identity.MessageListFilter) ([]identity.Message, error) {
			if f.AgentID != "support@acme.com" {
				return nil, errors.New("unexpected agent")
			}
			// Two messages, newest-first; honor Limit + AfterID so the
			// cursor round-trip is exercised end to end.
			all := []identity.Message{
				{ID: "msg_b", Direction: "inbound", Sender: "b@x.com", Recipient: "support@acme.com", Subject: "B", InboxStatus: "unread", CreatedAt: time.Unix(1700000200, 0).UTC()},
				{ID: "msg_a", Direction: "inbound", Sender: "a@x.com", Recipient: "support@acme.com", Subject: "A", InboxStatus: "unread", CreatedAt: time.Unix(1700000100, 0).UTC()},
			}
			start := 0
			if f.AfterID != "" {
				for i, m := range all {
					if m.ID == f.AfterID {
						start = i + 1
						break
					}
				}
			}
			rest := all[start:]
			if f.Limit > 0 && len(rest) > f.Limit {
				rest = rest[:f.Limit]
			}
			return rest, nil
		},
		ListConversations: func(ctx context.Context, f identity.ConversationListFilter) ([]identity.ConversationSummary, error) {
			if f.AgentID != "support@acme.com" {
				return nil, errors.New("unexpected agent")
			}
			return []identity.ConversationSummary{{
				ID: "conv_1", MessageCount: 2, InboundCount: 1, OutboundCount: 1,
				HasUnread: true, LatestSubject: "Help", LatestSender: "alice@example.com",
				LastMessageAt: time.Unix(1700000200, 0).UTC(), FirstMessageAt: time.Unix(1700000100, 0).UTC(),
			}}, nil
		},
		ListSuppressions: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterAddress string) ([]identity.Suppression, error) {
			// Three suppressions, newest-first; honor limit + after-key so the
			// cursor round-trip is exercised end to end (A-5).
			all := []identity.Suppression{
				{Address: "c@x.com", Source: "bounce", CreatedAt: time.Unix(1700000300, 0).UTC()},
				{Address: "b@x.com", Source: "complaint", CreatedAt: time.Unix(1700000200, 0).UTC()},
				{Address: "a@x.com", Source: "manual", CreatedAt: time.Unix(1700000100, 0).UTC()},
			}
			start := 0
			if !afterCreatedAt.IsZero() {
				for i, sp := range all {
					if sp.CreatedAt.Equal(afterCreatedAt) && sp.Address == afterAddress {
						start = i + 1
						break
					}
				}
			}
			rest := all[start:]
			if limit > 0 && len(rest) > limit {
				rest = rest[:limit]
			}
			return rest, nil
		},
		GetConversation: func(ctx context.Context, agentID, convoID string) (*identity.ConversationDetail, error) {
			if agentID == "support@acme.com" && convoID == "conv_1" {
				return &identity.ConversationDetail{
					ConversationSummary: identity.ConversationSummary{
						ID: "conv_1", MessageCount: 1, LatestSubject: "Help",
						LastMessageAt: time.Unix(1700000200, 0).UTC(), FirstMessageAt: time.Unix(1700000200, 0).UTC(),
					},
					Participants: []string{"alice@example.com", "support@acme.com"},
					Labels:       []string{"urgent"},
					Messages:     []identity.Message{{ID: "msg_1", Direction: "inbound", Sender: "alice@example.com", Subject: "Help", InboxStatus: "unread", CreatedAt: time.Unix(1700000200, 0).UTC()}},
				}, nil
			}
			return nil, errors.New("not found")
		},
		GetMessage: func(ctx context.Context, messageID, agentID string) (*identity.Message, error) {
			if agentID == "support@acme.com" && messageID == "msg_out" {
				return &identity.Message{
					ID: "msg_out", Direction: "outbound", Sender: "support@acme.com",
					Recipient: "alice@example.com", ToRecipients: []string{"alice@example.com"},
					Subject: "Sent", CreatedAt: time.Unix(1700000300, 0).UTC(),
				}, nil
			}
			if agentID == "support@acme.com" && messageID == "msg_1" {
				return &identity.Message{
					ID:             "msg_1",
					Direction:      "inbound",
					Sender:         "alice@example.com",
					ToRecipients:   []string{"support@acme.com"},
					Recipient:      "support@acme.com",
					Subject:        "Help",
					ConversationID: "conv_1",
					// Real inbound rows carry the read-state in inbox_status; the
					// store mirrors it into DeliveryStatus for inbound. `status`
					// on the detail view is the inbox read-state (B2).
					InboxStatus: "unread",
					CreatedAt:   time.Unix(1700000000, 0).UTC(),
					AuthHeaders: map[string]string{"spf": "pass"},
					RawMessage:  []byte("raw"),
				}, nil
			}
			return nil, errors.New("not found")
		},
		LookupDomain: fakeLookupDomain,
		// DeleteDomain's base fixture models a completed deletion, so the
		// post-delete snapshot has a confirmed receipt and no live replacement.
		// Receipt-transition and ABA tests override both values explicitly.
		LookupDomainTeardownSnapshot: func(context.Context, string, string, string) (domainteardown.State, bool, error) {
			return domainteardown.Confirmed, false, nil
		},
		ListDomains: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterDomain string) ([]identity.Domain, error) {
			return []identity.Domain{{Domain: "acme.com", Verified: true, VerificationToken: "e2a-verify=tok", IsPrimary: true, AgentCount: 2}}, nil
		},
		ClaimDomain: func(ctx context.Context, domain, userID string) (*identity.Domain, error) {
			if domain == "taken.com" {
				return nil, identity.ErrDomainTaken
			}
			return &identity.Domain{Domain: domain, Verified: false, VerificationToken: "e2a-verify=new"}, nil
		},
		EnforceDomainCreate: func(ctx context.Context, userID string) error {
			if userID == "u_overcap" {
				return &limits.LimitExceededError{Resource: "domains", Limit: 1, Current: 1, Limits: limits.Limits{PlanCode: "free"}}
			}
			return nil
		},
		DeleteDomain: func(ctx context.Context, domain, userID string, complete DomainDeleteIdemCompleter) (domainteardown.Receipt, error) {
			if _, err := fakeLookupDomain(ctx, domain, userID); err != nil {
				return domainteardown.Receipt{}, identity.ErrDomainNotFound
			}
			return domainteardown.Receipt{Incarnation: "test-incarnation", State: domainteardown.Confirmed}, nil
		},
		CountAgentsOnDomain: func(ctx context.Context, domain, userID string) (int, int, error) {
			if domain == "busy.com" {
				return 1, 0, nil
			}
			return 0, 0, nil
		},
		SMTPDomain: "mx.e2a.dev",
		GetLimits: func(ctx context.Context, userID string) (limits.Limits, error) {
			return limits.Limits{PlanCode: "pro", MaxAgents: 10, MaxDomains: 5, MaxMessagesMonth: 1000, MaxStorageBytes: 1 << 30, UpgradeURL: "https://e2a.dev/upgrade"}, nil
		},
		GetUsage: func(ctx context.Context, userID string) LimitsUsageView {
			return LimitsUsageView{Agents: 2, Domains: 1, MessagesMonth: 42, StorageBytes: 1234}
		},
		ExportUserData: func(ctx context.Context, userID string) (*identity.UserExport, error) {
			return &identity.UserExport{}, nil
		},
		DeleteUserData: func(ctx context.Context, user *identity.User) (*identity.DeleteUserDataResult, error) {
			return &identity.DeleteUserDataResult{}, nil
		},
		EventsEnabled: true,
		ListEvents: func(ctx context.Context, q EventQuery) ([]agent.EventView, error) {
			// Two events, honoring Limit + cursor (CursorID) so the
			// cursor round-trip is exercised.
			all := []agent.EventView{
				{ID: "evt_b", Type: "email.received", Status: "delivered", CreatedAt: time.Unix(1700000200, 0).UTC()},
				{ID: "evt_a", Type: "email.sent", Status: "delivered", CreatedAt: time.Unix(1700000100, 0).UTC()},
			}
			start := 0
			if q.CursorID != "" {
				for i, e := range all {
					if e.ID == q.CursorID {
						start = i + 1
						break
					}
				}
			}
			rest := all[start:]
			if q.Limit > 0 && len(rest) > q.Limit {
				rest = rest[:q.Limit]
			}
			return rest, nil
		},
		GetEvent2: func(ctx context.Context, userID, eventID string) (*agent.EventView, error) {
			switch eventID {
			case "evt_a":
				return &agent.EventView{ID: "evt_a", Type: "email.sent", Status: "delivered", CreatedAt: time.Unix(1700000100, 0).UTC()}, nil
			case "evt_expired":
				return nil, agent.ErrEventExpired
			default:
				return nil, agent.ErrEventNotFound
			}
		},
		LoadReplayEvent: func(ctx context.Context, userID, eventID string) (*agent.ReplayEvent, error) {
			switch eventID {
			case "evt_a":
				return &agent.ReplayEvent{EventType: "email.received", MatchedWebhookIDs: []string{"wh_1", "wh_2"}}, nil
			case "evt_expired":
				return nil, agent.ErrEventExpired
			default:
				return nil, agent.ErrEventNotFound
			}
		},
		InsertReplayDelivery: func(ctx context.Context, eventID, webhookID, eventType string, messageID *string, envelope []byte) (string, error) {
			return "whd_" + webhookID, nil
		},
		CreateWebhook: func(ctx context.Context, userID, url, description string, events []string, filters identity.WebhookFilters, idemCompleteTx identity.WebhookIdemCompleter) (*identity.Webhook, error) {
			if strings.Contains(url, "capped") {
				return nil, identity.ErrWebhookCapReached
			}
			wh := &identity.Webhook{ID: "wh_1", URL: url, Description: description, Events: events, Filters: filters, SigningSecret: "whsec_xyz", Enabled: true, CreatedAt: time.Unix(1700000000, 0).UTC()}
			// Mirror identity.CreateWebhookIdem: a keyed create completes its
			// idempotency row atomically with the insert.
			if idemCompleteTx != nil {
				if err := idemCompleteTx(ctx, nil, wh); err != nil {
					return nil, err
				}
			}
			return wh, nil
		},
		ListWebhooks: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.Webhook, error) {
			return []identity.Webhook{{ID: "wh_1", URL: "https://x.com/h", Events: []string{"email.received"}, Enabled: true, SigningSecret: "whsec_should_be_hidden", CreatedAt: time.Unix(1700000000, 0).UTC()}}, nil
		},
		GetWebhook: func(ctx context.Context, webhookID, userID string) (*identity.Webhook, error) {
			if webhookID == "wh_1" || webhookID == "wh_cooldown" {
				return &identity.Webhook{ID: webhookID, URL: "https://x.com/h", Events: []string{"email.received"}, Enabled: true, SigningSecret: "whsec_should_be_hidden", CreatedAt: time.Unix(1700000000, 0).UTC()}, nil
			}
			return nil, identity.ErrWebhookNotFound
		},
		UpdateWebhook: func(ctx context.Context, webhookID, userID string, u identity.WebhookUpdate) (*identity.Webhook, error) {
			switch webhookID {
			case "wh_cooldown":
				return nil, identity.ErrWebhookCooldown
			case "wh_1":
				wh := &identity.Webhook{ID: "wh_1", URL: "https://x.com/h", Events: []string{"email.received"}, Enabled: true, CreatedAt: time.Unix(1700000000, 0).UTC()}
				if u.Description != nil {
					wh.Description = *u.Description
				}
				return wh, nil
			default:
				return nil, identity.ErrWebhookNotFound
			}
		},
		DeleteWebhook: func(ctx context.Context, webhookID, userID string) error {
			if webhookID == "wh_1" {
				return nil
			}
			return identity.ErrWebhookNotFound
		},
		RotateSecret: func(ctx context.Context, webhookID, userID string) (string, time.Time, error) {
			if webhookID == "wh_1" {
				return "whsec_rotated", time.Unix(1700086400, 0).UTC(), nil
			}
			return "", time.Time{}, identity.ErrWebhookNotFound
		},
		TestWebhookInsert: func(ctx context.Context, webhookID, eventType string, envelope []byte) (string, error) {
			return "whd_test_1", nil
		},
		ListDeliveries: func(ctx context.Context, webhookID, status string, limit int, afterCreatedAt time.Time, afterID string) ([]webhook.SubscriberDelivery, error) {
			return []webhook.SubscriberDelivery{
				{ID: "whd_1", EventType: "email.received", Status: "delivered", Attempts: 1, NextRetryAt: time.Unix(1700000000, 0).UTC(), CreatedAt: time.Unix(1700000000, 0).UTC()},
			}, nil
		},
		EnforceMessageSend: func(ctx context.Context, userID string) error {
			if userID == "u_overcap" {
				return &limits.LimitExceededError{Resource: "messages_month", Limit: 1, Current: 1, Limits: limits.Limits{PlanCode: "free"}}
			}
			return nil
		},
		SendTest: func(ctx context.Context, ag *identity.AgentIdentity) (*agent.OutboundResult, *agent.OutboundError) {
			// Queue-first platform test: durably accepted, provider id unknown
			// until the SendWorker submits (mirrors SendTestCore).
			return &agent.OutboundResult{MessageID: "msg_test_1", Status: "accepted", Method: "smtp"}, nil
		},
		ApprovePending: func(ctx context.Context, userID, messageID, expectedAgentEmail string, ovr agent.ApproveOverrides, _ agent.ApproveIdemCompleter) (*identity.Message, *agent.OutboundError) {
			switch messageID {
			case "msg_pending":
				return &identity.Message{ID: "msg_pending", Status: "sent", ProviderMessageID: "<prov@ses>", Method: "smtp"}, nil
			case "msg_notpending":
				return nil, &agent.OutboundError{Status: http.StatusConflict, Code: "message_not_pending", Msg: "message is not pending approval"}
			default:
				return nil, &agent.OutboundError{Status: http.StatusNotFound, Code: "not_found", Msg: "message not found"}
			}
		},
		RejectPending: func(ctx context.Context, userID, messageID, expectedAgentEmail, reason string) (*identity.Message, *agent.OutboundError) {
			if messageID == "msg_pending" {
				return &identity.Message{ID: "msg_pending", Status: "review_rejected", RejectionReason: reason}, nil
			}
			return nil, &agent.OutboundError{Status: http.StatusNotFound, Code: "not_found", Msg: "message not found"}
		},
		// Inbound review dispatch (slice 3). GetReviewMessage resolves direction
		// so the approve/reject handlers branch: outbound ids fall through to the
		// send-approval path above; inbound ids route to the release path.
		GetReviewMessage: func(ctx context.Context, messageID, agentID string) (*identity.ReviewMessageMeta, error) {
			switch messageID {
			case "msg_pending", "msg_notpending":
				return &identity.ReviewMessageMeta{ID: messageID, AgentID: agentID, Direction: "outbound", Status: "pending_review"}, nil
			case "msg_in_held":
				return &identity.ReviewMessageMeta{ID: messageID, AgentID: agentID, Direction: "inbound", Status: "pending_review", Sender: "attacker@x.com", Subject: "Held"}, nil
			case "msg_in_notpending":
				return &identity.ReviewMessageMeta{ID: messageID, AgentID: agentID, Direction: "inbound", Status: "review_approved"}, nil
			default:
				return nil, errors.New("not found")
			}
		},
		ApproveInboundReview: func(ctx context.Context, userID string, msg *identity.ReviewMessageMeta) *agent.OutboundError {
			if msg.ID == "msg_in_notpending" {
				return &agent.OutboundError{Status: http.StatusConflict, Code: "message_not_pending", Msg: "message is not pending review"}
			}
			return nil
		},
		RejectInboundReview: func(ctx context.Context, userID, reason string, msg *identity.ReviewMessageMeta) *agent.OutboundError {
			if msg.ID == "msg_in_notpending" {
				return &agent.OutboundError{Status: http.StatusConflict, Code: "message_not_pending", Msg: "message is not pending review"}
			}
			return nil
		},
		// GetReviewWithContent backs the account-scoped /v1/reviews/{id}
		// approve/reject ownership guard (requireOwnedReview). It resolves a held
		// message by id and yields its owning agent; the deeper approve/reject
		// dispatch (ApprovePending/RejectPending/GetReviewMessage above) then
		// branches on direction. Unknown ids 404.
		GetReviewWithContent: func(ctx context.Context, userID, id string) (*identity.Message, error) {
			switch id {
			case "msg_pending", "msg_notpending":
				return &identity.Message{ID: id, AgentID: "support@acme.com", Direction: "outbound", Status: "pending_review"}, nil
			case "msg_in_held", "msg_in_notpending":
				return &identity.Message{ID: id, AgentID: "support@acme.com", Direction: "inbound", Status: "pending_review"}, nil
			default:
				return nil, errors.New("not found")
			}
		},
		GetRepliableMessage: func(ctx context.Context, messageID string) (*identity.Message, error) {
			if messageID == "msg_in1" {
				return &identity.Message{
					ID: "msg_in1", AgentID: "support@acme.com", Sender: "alice@x.com",
					Subject: "Question", EmailMessageID: "<abc@x.com>",
					RawMessage: []byte("From: alice@x.com\r\nTo: support@acme.com\r\nSubject: Question\r\nMessage-ID: <abc@x.com>\r\n\r\nhi"),
				}, nil
			}
			if messageID == "msg_in1dated" {
				// The same body shape as msg_in1 but WITH a Date header, so the
				// reply's attribution takes the full "On <date>, <sender> wrote:"
				// form — the shape mailparse.quoteMarker strips cleanly on the
				// receiving side (residue-free). Every other quote fixture is
				// Date-less and pins the residue path, so this is the only
				// end-to-end exercise of the common real-mail form.
				return &identity.Message{
					ID: "msg_in1dated", AgentID: "support@acme.com", Sender: "alice@x.com",
					Subject: "Question", EmailMessageID: "<dated@x.com>",
					RawMessage: []byte("From: alice@x.com\r\nTo: support@acme.com\r\nSubject: Question\r\n" +
						"Message-ID: <dated@x.com>\r\nDate: Thu, 31 Jul 2026 03:30:47 +0000\r\n\r\nhi"),
				}, nil
			}
			if messageID == "msg_in_htmlonly" {
				// An inbound carrying ONLY a text/html part — no text/plain to
				// quote from. Exercises the reply-quote path's HTML→text
				// derivation (a bare text/html message is common from bulk
				// senders and some clients).
				return &identity.Message{
					ID: "msg_in_htmlonly", AgentID: "support@acme.com", Sender: "alice@x.com",
					Subject: "Question", EmailMessageID: "<htmlonly@x.com>",
					RawMessage: []byte("From: alice@x.com\r\nTo: support@acme.com\r\nSubject: Question\r\n" +
						"Message-ID: <htmlonly@x.com>\r\nContent-Type: text/html\r\n\r\n<p>hi &amp; hello</p>"),
				}, nil
			}
			if messageID == "msg_bigthread" {
				// An inbound on a thread with 60 To recipients — reply_all fans
				// them all into the outbound set, exceeding the 50 cap.
				var to []string
				for i := 0; i < 60; i++ {
					to = append(to, fmt.Sprintf("person%d@x.com", i))
				}
				raw := "From: alice@x.com\r\nTo: " + strings.Join(to, ", ") +
					"\r\nSubject: Big\r\nMessage-ID: <big@x.com>\r\n\r\nhi"
				return &identity.Message{
					ID: "msg_bigthread", AgentID: "support@acme.com", Sender: "alice@x.com",
					Subject: "Big", EmailMessageID: "<big@x.com>", RawMessage: []byte(raw),
				}, nil
			}
			return nil, errors.New("not found")
		},
		DeliverOutbound: func(ctx context.Context, user *identity.User, ag *identity.AgentIdentity, req outbound.SendRequest, msgType, replyTo string, referenced *identity.Message, ic agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			recordDelivered(req)
			switch {
			case strings.Contains(req.Subject, "HOLD"):
				exp := time.Unix(1700090000, 0).UTC()
				return &agent.OutboundResult{Held: true, PendingMessageID: "msg_pending_1", ApprovalExpiresAt: &exp}, nil
			case strings.Contains(req.Subject, "FAIL"):
				return nil, &agent.OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "send failed"}
			default:
				return &agent.OutboundResult{MessageID: "msg_sent_1", Method: "smtp"}, nil
			}
		},
		TouchDomainChecked: func(ctx context.Context, domain, userID string) error { return nil },
		VerifyDomain:       func(ctx context.Context, domain, userID string) error { return nil },
		VerifyProbe: func(domain, token, dkimSel, dkimKey string) DomainCheckResult {
			// "pending.com" has not published its TXT yet; "nomx.com" published
			// the ownership TXT but not the inbound MX; everything else is fully set up.
			if domain == "pending.com" {
				return DomainCheckResult{TXTFound: false, MX: "missing", SPF: "missing", DKIM: "missing"}
			}
			if domain == "nomx.com" {
				return DomainCheckResult{TXTFound: true, MX: "missing", SPF: "missing", DKIM: "missing"}
			}
			return DomainCheckResult{TXTFound: true, MX: "found", SPF: "found", DKIM: "found"}
		},
		CreateAgent: func(ctx context.Context, email, domain, name, webhookURL, agentMode, userID string) (*identity.AgentIdentity, error) {
			if email == "dupe@acme.com" {
				// Real unique-violation shape (SQLSTATE 23505) so the handler's
				// typed isUniqueViolation check classifies it as a conflict —
				// a plain errors.New("duplicate key value") no longer matches.
				return nil, &pgconn.PgError{Code: "23505", Message: "duplicate key value"}
			}
			return &identity.AgentIdentity{ID: email, RegisteredDomain: domain, Email: email, Name: name, UserID: userID}, nil
		},
		EnforceAgentCreate: func(ctx context.Context, userID string) error {
			if userID == "u_overcap" {
				return &limits.LimitExceededError{Resource: "agents", Limit: 1, Current: 1, Limits: limits.Limits{PlanCode: "free", UpgradeURL: "https://e2a.dev/upgrade"}}
			}
			return nil
		},
		UpdateAgentName: func(ctx context.Context, agentID, userID, name string) (*identity.AgentIdentity, error) {
			return &identity.AgentIdentity{ID: agentID, RegisteredDomain: "acme.com", Email: agentID, Name: name, UserID: userID}, nil
		},
		UpdateAgentProtection: func(ctx context.Context, agentID, userID string, cfg identity.ProtectionConfig) (*identity.AgentIdentity, error) {
			return &identity.AgentIdentity{ID: agentID, RegisteredDomain: "acme.com", Email: agentID, UserID: userID}, nil
		},
		DeleteAgent: func(ctx context.Context, agentID, userID string) error {
			if userID != "u_1" {
				return errors.New("unexpected user")
			}
			return nil
		},
		SharedDomain: "agents.e2a.dev",
		PublicURL:    "https://api.e2a.dev",
		Legacy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("legacy:" + r.URL.Path))
		}),
	}
	for _, opt := range opts {
		opt(&deps)
	}
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)
	return srv
}

func TestGetInfo(t *testing.T) {
	srv := testServer(t)
	resp, err := http.Get(srv.URL + "/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("missing X-Request-Id header")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
	var body DeploymentInfoView
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.SharedDomain != "agents.e2a.dev" || !body.SlugRegistrationEnabled || body.PublicURL != "https://api.e2a.dev" {
		t.Fatalf("unexpected info: %+v", body)
	}
}

func TestListAgentsAuthorized(t *testing.T) {
	srv := testServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body=%s", resp.StatusCode, b)
	}
	var body struct {
		Items      []AgentView `json:"items"`
		NextCursor *string     `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("want 1 agent, got %d", len(body.Items))
	}
	if body.NextCursor != nil {
		t.Fatalf("want null next_cursor on single page, got %q", *body.NextCursor)
	}
	a := body.Items[0]
	if a.Email != "support@acme.com" || a.Domain != "acme.com" || !a.DomainVerified {
		t.Fatalf("unexpected agent view: %+v", a)
	}
}

func TestGetAgentOwned(t *testing.T) {
	srv := testServer(t)
	// The address is URL-encoded in the path (@ -> %40); the real chi+Huma
	// stack must decode it before lookup.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/agents/support%40acme.com", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body=%s", resp.StatusCode, b)
	}
	var a AgentView
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	if a.Email != "support@acme.com" || a.Name != "Acme Support" {
		t.Fatalf("unexpected agent: %+v", a)
	}
}

func TestGetAgentNotFoundWhenUnknown(t *testing.T) {
	srv := testServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/agents/other%40acme.com", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// An unknown OR non-owned agent is 404 not_found — consistent with every
	// other per-resource lookup, and the two cases are indistinguishable so the
	// response never reveals another account's agent (anti-enumeration).
	if resp.StatusCode != 404 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != "not_found" {
		t.Fatalf("want code not_found, got %q", env.Error.Code)
	}
}

func TestGetMessageOwned(t *testing.T) {
	srv := testServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/agents/support%40acme.com/messages/msg_1", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body=%s", resp.StatusCode, b)
	}
	// Decode into a map to assert the canonical required keys are present.
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "header_from", "envelope_from", "authentication", "to", "cc", "reply_to", "delivered_to", "subject", "conversation_id", "read_status", "labels", "created_at", "raw_message"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in message view", k)
		}
	}
	if m["id"] != "msg_1" || m["read_status"] != "unread" {
		t.Fatalf("unexpected message: %+v", m)
	}
	// raw_message is []byte -> base64 string ("raw" -> "cmF3").
	if m["raw_message"] != "cmF3" {
		t.Fatalf("raw_message not base64-encoded: %v", m["raw_message"])
	}
}

func TestGetOutboundMessageIncludesNullAuthentication(t *testing.T) {
	srv := testServer(t)
	code, body := getJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_out", "good")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, body)
	}
	if authentication, present := body["authentication"]; !present || authentication != nil {
		t.Fatalf("authentication = %#v, present=%v; want explicit null", authentication, present)
	}
}

func TestGetMessageNotFound(t *testing.T) {
	srv := testServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/agents/support%40acme.com/messages/msg_missing", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestListAgentsUnauthorizedEnvelope(t *testing.T) {
	srv := testServer(t)
	resp, err := http.Get(srv.URL + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	headerID := resp.Header.Get("X-Request-Id")
	var env struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "unauthorized" {
		t.Fatalf("want code unauthorized, got %q", env.Error.Code)
	}
	if env.Error.RequestID == "" || env.Error.RequestID != headerID {
		t.Fatalf("request_id body=%q header=%q must match and be non-empty", env.Error.RequestID, headerID)
	}
}

func TestRequestIDPropagation(t *testing.T) {
	srv := testServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/info", nil)
	req.Header.Set("X-Request-Id", "req_caller_supplied")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "req_caller_supplied" {
		t.Fatalf("request id not propagated: %q", got)
	}
}

func TestLegacyFallback(t *testing.T) {
	srv := testServer(t)
	// A route the v1 layer does not own must fall through to the legacy
	// handler unchanged (strangler) — and still carry the new request id.
	resp, err := http.Get(srv.URL + "/api/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("expected legacy 418, got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "legacy:/api/v1/agents" {
		t.Fatalf("unexpected legacy body: %s", b)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("legacy fallback should still carry X-Request-Id")
	}
}

// fakeLookupDomain mirrors identity.Store.LookupDomain's USER SCOPING
// (`WHERE domain = $1 AND user_id = $2`). Modeling ownership matters: a fake
// that switched on the domain alone — as this one used to — cannot observe
// WHICH user id a handler passes, so it silently passes even when a handler
// drops the scoping entirely. That scoping is what keeps the domain-create cap
// from being bypassable (#819), so it needs a fake that can fail.
//
// Every account in the harness owns the shared fixtures, preserving the old
// behavior for tests that authenticate as either user. "other-account.com" is
// owned by u_1 ALONE and exists so a cross-account lookup has something real to
// miss on.
func fakeLookupDomain(ctx context.Context, domain, userID string) (*identity.Domain, error) {
	shared := map[string]identity.Domain{
		"acme.com":    {Domain: domain, Verified: true, VerificationToken: "e2a-verify=tok", IsPrimary: true},
		"pending.com": {Domain: domain, Verified: false},
		"busy.com":    {Domain: domain, Verified: true},
		// Registered, TXT published, not yet marked verified.
		"fresh.com": {Domain: domain, Verified: false, VerificationToken: "e2a-verify=fresh"},
		// Registered, ownership TXT published, but the inbound MX is missing.
		"nomx.com": {Domain: domain, Verified: false, VerificationToken: "e2a-verify=nomx"},
	}
	if row, ok := shared[domain]; ok {
		return &row, nil
	}
	// Singly-owned fixtures. These are what let a test observe the user id:
	// each is invisible to every account but its owner, so a handler that
	// passes "" — or any other account's id — misses.
	soleOwner := map[string]string{
		"other-account.com": "u_1",
		"capped-owned.com":  "u_overcap",
	}
	if owner, ok := soleOwner[domain]; ok && owner == userID {
		return &identity.Domain{Domain: domain, Verified: true}, nil
	}
	return nil, errors.New("not registered")
}
