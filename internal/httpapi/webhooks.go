package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

const (
	webhookMaxAgentIDs        = 50
	webhookMaxConversationIDs = 50
	webhookMaxLabels          = 50
	webhookMaxFilterValueLen  = 200
	webhookMaxURLLen          = 2048
	webhookMaxDescriptionLen  = 200
)

// WebhookFiltersView mirrors identity.WebhookFilters (response side).
type WebhookFiltersView struct {
	AgentIDs        []string `json:"agent_emails,omitempty" nullable:"false"`
	ConversationIDs []string `json:"conversation_ids,omitempty" nullable:"false"`
	Labels          []string `json:"labels,omitempty" nullable:"false"`
}

// WebhookFiltersRequest is WebhookFiltersView's request-side twin (create/
// update bodies). Dedicated input type because the spec's forward-compat
// stance is asymmetric: request schemas stay `additionalProperties: false`
// (an unknown filter key is a 422, not a silently ignored no-op filter) while
// response schemas are open so clients tolerate additive fields; one shared
// schema cannot carry both (stability.go panics if one tries). Keep the fields
// in lockstep with the View. Convertible via WebhookFiltersView(f) — the
// compiler enforces the mirror.
type WebhookFiltersRequest struct {
	AgentIDs        []string `json:"agent_emails,omitempty" nullable:"false"`
	ConversationIDs []string `json:"conversation_ids,omitempty" nullable:"false"`
	Labels          []string `json:"labels,omitempty" nullable:"false"`
}

// WebhookView is the webhook resource as returned by get/list/update. It
// carries NO signing secret (WH-3): the secret is shown once, only in the
// create response (CreateWebhookResponse) and on rotate (rotateSecretResponse).
type WebhookView struct {
	ID             string             `json:"id"`
	URL            string             `json:"url"`
	Description    string             `json:"description"`
	Events         []string           `json:"events" nullable:"false" doc:"The event types this webhook matches. Open set: new event types may be added over time, so treat these as strings and tolerate unknown values. Known values: email.received, email.sent, email.failed, email.delivered, email.bounced, email.complained, email.flagged, email.blocked, email.review_requested, email.review_approved, email.review_rejected, domain.sending_verified, domain.sending_failed, domain.suppression_added, agent.suppression_added, contact.due. Beta: the screening, review-hold, agent.suppression_added, and contact.due events are unstable — their payload may change before they are declared stable."`
	Filters        WebhookFiltersView `json:"filters"`
	Enabled        bool               `json:"enabled"`
	AutoDisabledAt *time.Time         `json:"auto_disabled_at,omitempty" format:"date-time"`
	// AutoDisabledReason is additive and optional (backward compatible):
	// present only while the webhook is auto-disabled.
	AutoDisabledReason string     `json:"auto_disabled_reason,omitempty" doc:"Why e2a auto-disabled this webhook — the delivery failure observed when the breaker tripped (e.g. \"HTTP 404\"). Open set: treat as a short human-readable string and tolerate unknown values. Present only while the webhook is auto-disabled; cleared on re-enable."`
	CreatedAt          time.Time  `json:"created_at" format:"date-time"`
	LastDeliveredAt    *time.Time `json:"last_delivered_at,omitempty" format:"date-time"`
}

// CreateWebhookResponse is WebhookView plus the one-time signing secret (WH-3),
// returned only by createWebhook. Persist the secret on receipt — it is never
// shown again (use rotate-secret to mint a new one).
type CreateWebhookResponse struct {
	WebhookView
	SigningSecret string `json:"signing_secret"`
}

func webhookView(wh *identity.Webhook) WebhookView {
	v := WebhookView{
		ID:          wh.ID,
		URL:         wh.URL,
		Description: wh.Description,
		Events:      orEmptyStrings(wh.Events),
		Filters: WebhookFiltersView{
			AgentIDs:        wh.Filters.AgentIDs,
			ConversationIDs: wh.Filters.ConversationIDs,
			Labels:          wh.Filters.Labels,
		},
		Enabled:            wh.Enabled,
		AutoDisabledReason: wh.AutoDisableReason,
		CreatedAt:          wh.CreatedAt.UTC(),
	}
	v.AutoDisabledAt = utcPtr(wh.AutoDisabledAt)
	v.LastDeliveredAt = utcPtr(wh.LastDeliveredAt)
	return v
}

// validateWebhookFields replicates the legacy validation. The two
// security-critical checks are reused from their canonical homes (SSRF via
// agent.ValidateWebhookURL, event allowlist via webhookpub.IsValidEventType)
// so they can't drift; the charset/length/ownership checks are explicit.
func (s *Server) validateWebhookFields(ctx context.Context, userID, url string, events []string, f WebhookFiltersView, description string) *ErrorEnvelope {
	if url == "" {
		return NewError(http.StatusBadRequest, "invalid_webhook_url", "url must not be empty")
	}
	if err := agent.ValidateWebhookURL(url); err != nil {
		return NewError(http.StatusBadRequest, "invalid_webhook_url", "invalid url: "+err.Error())
	}
	if len(url) > webhookMaxURLLen {
		return NewError(http.StatusBadRequest, "invalid_request", "url too long (max 2048 chars)")
	}
	if len(events) == 0 {
		return NewError(http.StatusBadRequest, "invalid_request", "events must be a non-empty array")
	}
	for _, e := range events {
		if !webhookpub.IsValidEventType(e) {
			return NewError(http.StatusBadRequest, "invalid_event_type",
				fmt.Sprintf("unknown event type %q (allowed: %s)", e, strings.Join(webhookpub.AllEventTypes, ", ")))
		}
	}
	if len(description) > webhookMaxDescriptionLen {
		return NewError(http.StatusBadRequest, "invalid_request", "description too long (max 200 chars)")
	}
	if strings.ContainsAny(description, "\r\n") {
		return NewError(http.StatusBadRequest, "invalid_request", "description must not contain CR or LF")
	}
	if len(f.AgentIDs) > webhookMaxAgentIDs {
		return NewError(http.StatusBadRequest, "invalid_request", fmt.Sprintf("filters.agent_emails exceeds cap of %d", webhookMaxAgentIDs))
	}
	if len(f.ConversationIDs) > webhookMaxConversationIDs {
		return NewError(http.StatusBadRequest, "invalid_request", fmt.Sprintf("filters.conversation_ids exceeds cap of %d", webhookMaxConversationIDs))
	}
	if len(f.Labels) > webhookMaxLabels {
		return NewError(http.StatusBadRequest, "invalid_request", fmt.Sprintf("filters.labels exceeds cap of %d", webhookMaxLabels))
	}
	for _, a := range f.AgentIDs {
		if a == "" || len(a) > webhookMaxFilterValueLen {
			return NewError(http.StatusBadRequest, "invalid_request", "filters.agent_emails contains empty entry or one over 200 chars")
		}
	}
	// agent_ids must reference agents the caller owns.
	if err := s.assertAgentsOwned(ctx, userID, f.AgentIDs); err != nil {
		return err
	}
	for _, c := range f.ConversationIDs {
		if c == "" || len(c) > webhookMaxFilterValueLen {
			return NewError(http.StatusBadRequest, "invalid_request", "filters.conversation_ids contains empty entry or one over 200 chars")
		}
		for _, r := range c {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
				return NewError(http.StatusBadRequest, "invalid_request", fmt.Sprintf("filters.conversation_ids[%q]: invalid character", c))
			}
		}
	}
	for _, l := range f.Labels {
		if l == "" || len(l) > 64 {
			return NewError(http.StatusBadRequest, "invalid_request", "filters.labels contains empty entry or one over 64 chars")
		}
		for _, r := range l {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ':' || r == '-' || r == '_') {
				return NewError(http.StatusBadRequest, "invalid_request", fmt.Sprintf("filters.labels[%q]: invalid character (expected [a-z0-9:_-]+, lowercase)", l))
			}
		}
	}
	return nil
}

// assertAgentsOwned verifies every referenced agent_id is owned by the user,
// mirroring the legacy assertAgentsOwnedByUser (uses ListAgents at filter-list
// scale ≤50).
func (s *Server) assertAgentsOwned(ctx context.Context, userID string, agentIDs []string) *ErrorEnvelope {
	if len(agentIDs) == 0 {
		return nil
	}
	// limit<=0 = every agent: ownership validation must see the caller's whole
	// agent set, not one page.
	agents, err := s.deps.ListAgents(ctx, userID, 0, time.Time{}, "")
	if err != nil {
		return NewError(http.StatusInternalServerError, "internal_error", "failed to validate agent filters")
	}
	owned := make(map[string]struct{}, len(agents))
	for i := range agents {
		owned[agents[i].ID] = struct{}{}
	}
	for _, id := range agentIDs {
		if _, ok := owned[identity.NormalizeEmail(id)]; !ok {
			return NewError(http.StatusBadRequest, "invalid_request", fmt.Sprintf("filters.agent_emails references an agent you don't own: %q", id))
		}
	}
	return nil
}

type webhookOutput struct{ Body WebhookView }
type webhookCreateOutput struct{ Body CreateWebhookResponse }

// listWebhooksOutput uses the shared Page[T] envelope (items + next_cursor);
// next_cursor is null at launch. See listAgentsOutput. (GA blocker #3.)
type listWebhooksOutput struct {
	Body Page[WebhookView]
}

// CreateWebhookRequest — url + events are required (WH-1). events items are
// constrained to the canonical vocabulary (WH-2; keep in sync with
// webhookpub.AllEventTypes).
type CreateWebhookRequest struct {
	URL         string                 `json:"url" doc:"Required, non-empty webhook delivery URL."`
	Events      []string               `json:"events" nullable:"false" enum:"email.received,email.sent,email.failed,email.review_approved,email.review_rejected,domain.sending_verified,domain.sending_failed,email.delivered,email.bounced,email.complained,domain.suppression_added,agent.suppression_added,email.flagged,email.blocked,email.review_requested,contact.due" doc:"Beta: the screening, review-hold, agent.suppression_added, and contact.due events are unstable — their payload may change before they are declared stable. All other events are stable."`
	Filters     *WebhookFiltersRequest `json:"filters,omitempty"`
	Description string                 `json:"description,omitempty"`
}

// createWebhookInput carries an optional Idempotency-Key so a retried create
// replays the first subscription (same id, same one-time signing secret)
// instead of registering a SECOND active subscription with a second secret.
// Intentional duplicates — including several subscriptions to the same URL —
// remain expressible by omitting the key (idempotency is opt-in; there is
// deliberately no unique(user_id, url) constraint).
type createWebhookInput struct {
	RawBody        []byte
	IdempotencyKey string `header:"Idempotency-Key" doc:"Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request's response — the SAME webhook id and one-time signing secret — instead of registering a second active subscription. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). A keyed create commits the webhook and its replay response atomically, so an accepted create always replays; dedup is best-effort only under idempotency-store degradation before that commit."`
	Body           CreateWebhookRequest
}

// WebhookIDParam is the path input for single-webhook read/update ops.
type WebhookIDParam struct {
	ID string `path:"id"`
}

// deleteWebhookInput is WebhookIDParam plus the uniform destructive-delete
// guard (DeleteConfirm) — a dedicated input so only DELETE requires ?confirm.
type deleteWebhookInput struct {
	ID string `path:"id"`
	DeleteConfirm
}

func (s *Server) registerWebhooks() {
	registerOp(s.API, huma.Operation{
		OperationID: "createWebhook", Method: http.MethodPost, Path: "/v1/webhooks",
		Summary: "Create a webhook", Tags: []string{"webhooks"},
		Description: "Register a webhook subscriber; the one-time signing secret is returned only on this response. Honors Idempotency-Key so a retried create replays the same webhook (same id + secret) instead of registering a second subscription; omit the key to intentionally create distinct subscriptions, including several to the same URL.",
		Security:    []map[string][]string{{"bearer": {}}}, DefaultStatus: http.StatusCreated,
		Responses: map[string]*huma.Response{
			"409":     s.idempotencyInFlightResponse(),
			"422":     s.idempotencyReuseResponse(),
			"default": s.errorEnvelopeResponse(),
		},
	}, s.handleCreateWebhook)

	registerOp(s.API, huma.Operation{
		OperationID: "listWebhooks", Method: http.MethodGet, Path: "/v1/webhooks",
		Summary: "List webhooks", Tags: []string{"webhooks"},
		Description: "List the webhooks owned by the authenticated account, newest first, with cursor pagination.",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleListWebhooks)

	registerOp(s.API, huma.Operation{
		OperationID: "getWebhook", Method: http.MethodGet, Path: "/v1/webhooks/{id}",
		Summary: "Get a webhook", Tags: []string{"webhooks"},
		Security: []map[string][]string{{"bearer": {}}},
	}, s.handleGetWebhook)

	registerOp(s.API, huma.Operation{
		OperationID: "deleteWebhook", Method: http.MethodDelete, Path: "/v1/webhooks/{id}",
		Summary: "Delete a webhook", Tags: []string{"webhooks"},
		Description: "Delete a webhook subscriber by id. Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}).",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleDeleteWebhook)

	registerOp(s.API, huma.Operation{
		OperationID: "updateWebhook", Method: http.MethodPatch, Path: "/v1/webhooks/{id}",
		Summary: "Update a webhook", Tags: []string{"webhooks"},
		Description: "Partial update. url/events/filters are full-replace when present. Re-enabling within the auto-disable cooldown returns 409.",
		Security:    []map[string][]string{{"bearer": {}}},
		Responses: map[string]*huma.Response{
			"409": s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
				"Conflict — code webhook_cooldown: the webhook was auto-disabled for persistent delivery failures and cannot be re-enabled until the cooldown elapses. SDKs do not automatically retry this code; retry manually only after the cooldown, and fix the endpoint first."),
			"default": s.errorEnvelopeResponse(),
		},
	}, s.handleUpdateWebhook)

	registerOp(s.API, huma.Operation{
		OperationID: "rotateWebhookSecret", Method: http.MethodPost, Path: "/v1/webhooks/{id}/rotate-secret",
		Summary: "Rotate a webhook signing secret", Tags: []string{"webhooks"},
		Description: "Mint a new signing secret; the previous one stays valid for a 24h grace window. Returns the new secret (shown once). Honors Idempotency-Key so a retried rotate replays the same secret instead of rotating twice (rotate has no request body, so the dedup hash covers the route alone — the same key on a different webhook id is a 422 idempotency_key_reuse).",
		Security:    []map[string][]string{{"bearer": {}}},
		Responses: map[string]*huma.Response{
			"409":     s.idempotencyInFlightResponse(),
			"422":     s.idempotencyReuseResponse(),
			"default": s.errorEnvelopeResponse(),
		},
	}, s.handleRotateWebhookSecret)

	registerOp(s.API, huma.Operation{
		OperationID: "testWebhook", Method: http.MethodPost, Path: "/v1/webhooks/{id}/test",
		Summary: "Fire a synthetic event", Tags: []string{"webhooks"},
		Description: "Schedule a one-off synthetic delivery to this webhook for development. Returns the delivery id.",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleTestWebhook)

	registerOp(s.API, huma.Operation{
		OperationID: "listWebhookDeliveries", Method: http.MethodGet, Path: "/v1/webhooks/{id}/deliveries",
		Summary: "List webhook deliveries", Tags: []string{"webhooks"},
		Description: "The per-webhook delivery log (read-only debug view).",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleListWebhookDeliveries)
}

// TestWebhookRequest mirrors the legacy body.
type TestWebhookRequest struct {
	Event string         `json:"type,omitempty" enum:"email.received,email.sent,email.failed,email.review_approved,email.review_rejected,domain.sending_verified,domain.sending_failed,email.delivered,email.bounced,email.complained,domain.suppression_added,agent.suppression_added,email.flagged,email.blocked,email.review_requested,contact.due"`
	Data  map[string]any `json:"data,omitempty"`
}
type testWebhookInput struct {
	ID   string `path:"id"`
	Body TestWebhookRequest
}

// TestWebhookResponse is the test-delivery result (WH-6 naming).
type TestWebhookResponse struct {
	DeliveryID string `json:"delivery_id"`
}
type testWebhookOutput struct {
	Body TestWebhookResponse
}

func (s *Server) handleTestWebhook(ctx context.Context, in *testWebhookInput) (*testWebhookOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.TestWebhookInsert == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "test endpoint not configured")
	}
	wh, err := s.deps.GetWebhook(ctx, in.ID, user.ID)
	if err != nil || wh == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "webhook not found")
	}
	if !wh.Enabled {
		return nil, NewError(http.StatusConflict, "webhook_disabled", "webhook is disabled")
	}
	event := in.Body.Event
	if event == "" {
		event = webhookpub.EventEmailReceived
	}
	if !webhookpub.IsValidEventType(event) {
		return nil, NewError(http.StatusBadRequest, "invalid_event_type", fmt.Sprintf("unknown event type %q", event))
	}
	data := in.Body.Data
	if data == nil {
		data = map[string]any{"test": true}
	}
	envelope, err := json.Marshal(webhookpub.NewEvent(event, user.ID, data).AsEnvelope())
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to marshal test event")
	}
	deliveryID, err := s.deps.TestWebhookInsert(ctx, wh.ID, event, envelope)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to schedule test delivery")
	}
	// River is the sole delivery engine: the direct insert above leaves the row
	// with no job, so enqueue one now (own tx, stamps job_id) for immediate
	// delivery. If that fails the row is still durable and the periodic reconciler
	// (webhookdelivery.ReconcileWorker) re-enqueues it within a minute — so log and
	// report success rather than 500 (a retry would just create a second test row).
	if s.deps.EnqueueDelivery != nil {
		if err := s.deps.EnqueueDelivery(ctx, deliveryID); err != nil {
			log.Printf("[webhooks] test delivery %s enqueue failed (reconciler will retry): %v", deliveryID, err)
		}
	}
	out := &testWebhookOutput{}
	out.Body.DeliveryID = deliveryID
	return out, nil
}

// WebhookDeliveryView mirrors the legacy per-delivery wire shape.
type WebhookDeliveryView struct {
	ID             string     `json:"id"`
	EventType      string     `json:"type" doc:"The event type that triggered this delivery. Open set: new event types may be added, so treat as a string and tolerate unknown values. Known values are the webhook event catalog (email.received, email.sent, email.failed, email.delivered, …, domain.*)."`
	Status         string     `json:"status" doc:"Delivery state. Open set; tolerate unknown values. Known values: pending, delivered, failed."`
	Attempts       int        `json:"attempts"`
	LastError      string     `json:"last_error,omitempty"`
	LastStatusCode *int       `json:"last_status_code,omitempty"`
	LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty" format:"date-time"`
	NextRetryAt    time.Time  `json:"next_retry_at" format:"date-time"`
	CreatedAt      time.Time  `json:"created_at" format:"date-time"`
}

// ListDeliveriesInput — the per-webhook delivery log, keyset-paginated on
// (created_at, id) like every other v1 list. The delivery log grows unbounded on
// a busy webhook, so a cursor (not a fixed cap) is what keeps the whole log
// reachable; the limit is generous (default 100, up to 100) so a recent view is
// rarely more than one page. `status` restricts to pending|delivered|failed and
// is pinned into the cursor (a continuation must not change it).
type ListDeliveriesInput struct {
	ID     string `path:"id"`
	Status string `query:"status" enum:"pending,delivered,failed"`
	Cursor string `query:"cursor" doc:"Opaque pagination cursor from a previous response's next_cursor. Continuation requests must not change the status filter."`
	Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"100"`
}

// deliveriesCursor is the opaque keyset position for the delivery log: the last
// row's (created_at, id) plus the PARENT WEBHOOK and the status filter it was
// minted under, so a continuation that changes either is rejected rather than
// silently returning a wrong page (mirrors the messages/events filter-binding).
//
// WebhookID is what makes this list's binding complete. The resource
// discriminator only proves a cursor came from *some* deliveries list, and this
// endpoint is parameterized by {id}: without the parent, a cursor minted on
// webhook A was accepted on webhook B and A's keyset anchor was handed to B's
// query — an arbitrarily truncated, commonly empty page. Both webhooks are
// caller-owned (the handler checks GetWebhook before it touches the cursor), so
// this was never cross-tenant leakage, but it is the same silent-wrong-answer
// failure the resource binding exists to eliminate. Every other parameterized
// list already pins its parent: messages/conversations pin AgentID,
// engagements/agent suppressions pin AgentEmail, message_lifecycle pins
// AgentID + MessageID.
type deliveriesCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
	Status    string    `json:"s,omitempty"`
	WebhookID string    `json:"w,omitempty"`
}
type listDeliveriesOutput struct {
	Body Page[WebhookDeliveryView]
}

func (s *Server) handleListWebhookDeliveries(ctx context.Context, in *ListDeliveriesInput) (*listDeliveriesOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.ListDeliveries == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "deliveries endpoint not configured")
	}
	// Ownership: deliveries are scoped to a webhook the caller owns.
	if wh, err := s.deps.GetWebhook(ctx, in.ID, user.ID); err != nil || wh == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "webhook not found")
	}
	var cur deliveriesCursor
	if in.Cursor != "" {
		if err := s.decodeCursor(user.ID, cursorWebhookDeliveries, in.Cursor, &cur); err != nil {
			return nil, err
		}
		if cur.WebhookID != in.ID {
			return nil, NewError(http.StatusBadRequest, "invalid_cursor",
				"cursor was created for a different webhook — start a new query without a cursor")
		}
		if cur.Status != in.Status {
			return nil, NewError(http.StatusBadRequest, "invalid_cursor",
				"cursor was created with a different status filter — start a new query without a cursor")
		}
	}
	limit := effectiveLimit(in.Limit)
	// Fetch limit+1 to detect a further page.
	rows, err := s.deps.ListDeliveries(ctx, in.ID, in.Status, limit+1, cur.CreatedAt, cur.ID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list deliveries")
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]WebhookDeliveryView, 0, len(rows))
	for _, d := range rows {
		v := WebhookDeliveryView{
			ID: d.ID, EventType: d.EventType, Status: d.Status, Attempts: d.Attempts,
			LastError: d.LastError, LastStatusCode: d.LastStatusCode,
			NextRetryAt: d.NextRetryAt.UTC(),
			CreatedAt:   d.CreatedAt.UTC(),
		}
		v.LastAttemptAt = utcPtr(d.LastAttemptAt)
		items = append(items, v)
	}
	var nextCursor string
	if hasMore {
		last := rows[len(rows)-1]
		nextCursor, err = EncodeCursor(s.deps.CursorSecret, user.ID, cursorWebhookDeliveries, deliveriesCursor{
			CreatedAt: last.CreatedAt, ID: last.ID, Status: in.Status, WebhookID: in.ID,
		})
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
		}
	}
	return &listDeliveriesOutput{Body: NewPage(items, nextCursor)}, nil
}

// UpdateWebhookRequest mirrors the legacy PATCH body — pointer fields so
// absent != zero; url/events/filters are full-replace when present.
type UpdateWebhookRequest struct {
	URL         *string                `json:"url,omitempty" doc:"When present, must be a non-empty webhook delivery URL; omit to leave the URL unchanged."`
	Events      *[]string              `json:"events,omitempty" enum:"email.received,email.sent,email.failed,email.review_approved,email.review_rejected,domain.sending_verified,domain.sending_failed,email.delivered,email.bounced,email.complained,domain.suppression_added,agent.suppression_added,email.flagged,email.blocked,email.review_requested,contact.due" doc:"Beta: the screening, review-hold, agent.suppression_added, and contact.due events are unstable — their payload may change before they are declared stable. All other events are stable."`
	Filters     *WebhookFiltersRequest `json:"filters,omitempty"`
	Description *string                `json:"description,omitempty"`
	Enabled     *bool                  `json:"enabled,omitempty"`
}
type updateWebhookInput struct {
	ID   string `path:"id"`
	Body UpdateWebhookRequest
}

func (s *Server) handleUpdateWebhook(ctx context.Context, in *updateWebhookInput) (*webhookOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	req := in.Body
	// A webhook with no events or no URL is a broken state — reject the
	// clearing patch (DELETE the webhook to remove it).
	if req.Events != nil && len(*req.Events) == 0 {
		return nil, NewError(http.StatusBadRequest, "invalid_request", "events must not be empty (DELETE the webhook to remove it)")
	}
	// Validate the effective post-patch state against the create-time rules.
	current, err := s.deps.GetWebhook(ctx, in.ID, user.ID)
	if err != nil || current == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "webhook not found")
	}
	effURL := current.URL
	if req.URL != nil {
		effURL = *req.URL
	}
	effEvents := current.Events
	if req.Events != nil {
		effEvents = *req.Events
	}
	effFilters := WebhookFiltersView{AgentIDs: current.Filters.AgentIDs, ConversationIDs: current.Filters.ConversationIDs, Labels: current.Filters.Labels}
	if req.Filters != nil {
		effFilters = WebhookFiltersView(*req.Filters)
	}
	effDesc := current.Description
	if req.Description != nil {
		effDesc = *req.Description
	}
	if env := s.validateWebhookFields(ctx, user.ID, effURL, effEvents, effFilters, effDesc); env != nil {
		return nil, env
	}
	var idFilters *identity.WebhookFilters
	if req.Filters != nil {
		idFilters = &identity.WebhookFilters{AgentIDs: req.Filters.AgentIDs, ConversationIDs: req.Filters.ConversationIDs, Labels: req.Filters.Labels}
	}
	wh, err := s.deps.UpdateWebhook(ctx, in.ID, user.ID, identity.WebhookUpdate{
		URL: req.URL, Events: req.Events, Filters: idFilters, Description: req.Description, Enabled: req.Enabled,
	})
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrWebhookNotFound):
			return nil, NewError(http.StatusNotFound, "not_found", "webhook not found")
		case errors.Is(err, identity.ErrWebhookCooldown):
			return nil, NewError(http.StatusConflict, "webhook_cooldown", err.Error())
		default:
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to update webhook")
		}
	}
	return &webhookOutput{Body: webhookView(wh)}, nil
}

type rotateSecretResponse struct {
	SigningSecret           string    `json:"signing_secret"`
	PreviousSecretExpiresAt time.Time `json:"previous_secret_expires_at" format:"date-time"`
}

type rotateSecretOutput struct {
	Body rotateSecretResponse
}

// rotateSecretInput carries an optional Idempotency-Key so a retried rotate
// replays the first secret instead of minting a second one — without it, a
// network retry would rotate twice within the grace window and silently drop
// the secret the caller already stored. Rotate has no request body; the
// idempotency dedup hashes the route (which includes the webhook id) alone.
type rotateSecretInput struct {
	ID             string `path:"id"`
	IdempotencyKey string `header:"Idempotency-Key" doc:"Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request's response instead of re-executing it. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may re-execute rather than replay."`
}

func (s *Server) handleRotateWebhookSecret(ctx context.Context, in *rotateSecretInput) (*rotateSecretOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	_, body, err := runIdempotent(s, ctx, user.ID, in.IdempotencyKey,
		"/v1/webhooks/"+in.ID+"/rotate-secret", nil,
		func(_ idemClaimToken) (int, rotateSecretResponse, error) {
			secret, prevExpires, rerr := s.deps.RotateSecret(ctx, in.ID, user.ID)
			if rerr != nil {
				if errors.Is(rerr, identity.ErrWebhookNotFound) {
					return 0, rotateSecretResponse{}, NewError(http.StatusNotFound, "not_found", "webhook not found")
				}
				return 0, rotateSecretResponse{}, NewError(http.StatusInternalServerError, "internal_error", "failed to rotate webhook secret")
			}
			return http.StatusOK, rotateSecretResponse{
				SigningSecret:           secret,
				PreviousSecretExpiresAt: prevExpires.UTC(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return &rotateSecretOutput{Body: body}, nil
}

func (s *Server) handleCreateWebhook(ctx context.Context, in *createWebhookInput) (*webhookCreateOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	// idemCompleteTx lets the store (identity.CreateWebhookIdem) commit this
	// request's idempotency-key completion in the SAME transaction as the
	// webhook insert — so a crash after that commit replays the first response
	// (same webhook id + one-time secret) instead of re-creating; there is no
	// post-insert window in which a webhook exists without a replayable
	// response. It caches the EXACT wire body this handler returns, keeping the
	// replay byte-faithful. The cached body carries the one-time signing secret
	// — intended: a same-account byte-identical keyed retry re-reads the secret
	// it already owns, and the row lives in the same TTL-swept idempotency
	// store that already caches the createApiKey plaintext key. nil when the
	// request has no Idempotency-Key or no store is wired; after an in-transaction
	// completion, runIdempotent's post-hoc Complete observes ErrClaimLost and is
	// deliberately ignored (or, unkeyed, the create runs unguarded — idempotency
	// is opt-in). Mirrors deliver()'s
	// AcceptIdemCompleter.
	// Validation runs INSIDE the claimed execution (like the send path): a
	// keyed replay short-circuits before it — byte-faithful even if mutable
	// state (agent-filter ownership, the webhook cap) has changed since the
	// first attempt — and a validation/limit failure returns an error strictly
	// before the insert, so runIdempotent releases the key for reuse instead
	// of consuming it.
	_, body, err := runIdempotent(s, ctx, user.ID, in.IdempotencyKey, "/v1/webhooks", in.RawBody,
		func(claimToken idemClaimToken) (int, CreateWebhookResponse, error) {
			var idemCompleteTx identity.WebhookIdemCompleter
			if in.IdempotencyKey != "" && s.deps.Idempotency != nil {
				nsKey := idemUserNS + in.IdempotencyKey
				uid := user.ID
				idemCompleteTx = func(ctx context.Context, tx pgx.Tx, wh *identity.Webhook) error {
					raw, mErr := json.Marshal(CreateWebhookResponse{WebhookView: webhookView(wh), SigningSecret: wh.SigningSecret})
					if mErr != nil {
						raw = []byte("{}")
					}
					return s.deps.Idempotency.CompleteTx(ctx, tx, uid, nsKey, claimToken, idempotency.CachedResponse{
						StatusCode: http.StatusCreated, ContentType: "application/json", Body: raw,
					})
				}
			}
			var filters WebhookFiltersView
			if in.Body.Filters != nil {
				filters = WebhookFiltersView(*in.Body.Filters)
			}
			if env := s.validateWebhookFields(ctx, user.ID, in.Body.URL, in.Body.Events, filters, in.Body.Description); env != nil {
				return 0, CreateWebhookResponse{}, env
			}
			wh, cerr := s.deps.CreateWebhook(ctx, user.ID, in.Body.URL, in.Body.Description, in.Body.Events, identity.WebhookFilters{
				AgentIDs:        filters.AgentIDs,
				ConversationIDs: filters.ConversationIDs,
				Labels:          filters.Labels,
			}, idemCompleteTx)
			if cerr != nil {
				if errors.Is(cerr, identity.ErrWebhookCapReached) {
					return 0, CreateWebhookResponse{}, NewError(http.StatusBadRequest, "webhook_limit_reached", cerr.Error())
				}
				return 0, CreateWebhookResponse{}, NewError(http.StatusInternalServerError, "internal_error", "failed to create webhook")
			}
			return http.StatusCreated, CreateWebhookResponse{
				WebhookView:   webhookView(wh),
				SigningSecret: wh.SigningSecret,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return &webhookCreateOutput{Body: body}, nil
}

// listWebhooksInput carries the standard cursor/limit (PageParams).
type listWebhooksInput struct {
	PageParams
}

func (s *Server) handleListWebhooks(ctx context.Context, in *listWebhooksInput) (*listWebhooksOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	afterCreatedAt, afterID, err := s.decodeKeyset(user.ID, cursorWebhooks, in.Cursor)
	if err != nil {
		return nil, err
	}
	limit := effectiveLimit(in.Limit)
	// Fetch limit+1 to detect a further page.
	hooks, err := s.deps.ListWebhooks(ctx, user.ID, limit+1, afterCreatedAt, afterID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list webhooks")
	}
	hasMore := len(hooks) > limit
	if hasMore {
		hooks = hooks[:limit]
	}
	items := make([]WebhookView, 0, len(hooks))
	for i := range hooks {
		items = append(items, webhookView(&hooks[i]))
	}
	var nextCursor string
	if hasMore {
		last := hooks[len(hooks)-1]
		if nextCursor, err = s.encodeKeyset(user.ID, cursorWebhooks, last.CreatedAt, last.ID); err != nil {
			return nil, err
		}
	}
	return &listWebhooksOutput{Body: NewPage(items, nextCursor)}, nil
}

func (s *Server) handleGetWebhook(ctx context.Context, in *WebhookIDParam) (*webhookOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	wh, err := s.deps.GetWebhook(ctx, in.ID, user.ID)
	if err != nil || wh == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "webhook not found")
	}
	return &webhookOutput{Body: webhookView(wh)}, nil
}

type deleteWebhookOutput struct{ Body DeleteWebhookResult }

func (s *Server) handleDeleteWebhook(ctx context.Context, in *deleteWebhookInput) (*deleteWebhookOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.deps.DeleteWebhook(ctx, in.ID, user.ID); err != nil {
		if errors.Is(err, identity.ErrWebhookNotFound) {
			return nil, NewError(http.StatusNotFound, "not_found", "webhook not found")
		}
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to delete webhook")
	}
	return &deleteWebhookOutput{Body: DeleteWebhookResult{Deleted: true, ID: in.ID}}, nil
}
