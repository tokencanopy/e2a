package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/agent"
)

// EventQuery is the resolved filter + cursor position passed to the events
// query closure.
type EventQuery struct {
	UserID                        string
	Type, AgentID, ConversationID string
	MessageID                     string
	Since, Until                  *time.Time
	CursorCreatedAt               time.Time
	CursorID                      string
	Limit                         int
}

// eventsCursor is the opaque continuation: the last row's (created_at, id) plus
// a snapshot of the filters it was minted under. A keyset position is only
// meaningful against the same filter set, so a continuation that carries
// different filters is rejected (invalid_cursor) rather than silently returning a
// wrong/incomplete page — mirrors the messagesCursor filter-binding.
type eventsCursor struct {
	C  time.Time `json:"c"`
	I  string    `json:"i"`
	Ty string    `json:"ty,omitempty"`
	Ag string    `json:"ag,omitempty"`
	Co string    `json:"co,omitempty"`
	Ms string    `json:"ms,omitempty"`
	Si string    `json:"si,omitempty"`
	Un string    `json:"un,omitempty"`
}

// ListEventsInput — filters + the standardized cursor/limit (replacing the
// legacy page_size/token).
type ListEventsInput struct {
	Type           string `query:"type"`
	AgentID        string `query:"agent_email"`
	ConversationID string `query:"conversation_id"`
	MessageID      string `query:"message_id"`
	Since          string `query:"since" doc:"RFC3339."`
	Until          string `query:"until" doc:"RFC3339."`
	Cursor         string `query:"cursor"`
	Limit          int    `query:"limit" minimum:"1" maximum:"100" default:"100"`
}

type listEventsOutput struct {
	Body Page[agent.EventView]
}

type eventOutput struct {
	Body agent.EventView
}

func (s *Server) registerEvents() {
	huma.Register(s.API, huma.Operation{
		OperationID: "listEvents", Method: http.MethodGet, Path: "/v1/events",
		Summary: "List events", Tags: []string{"events"},
		Description: "The webhook-event delivery log, filterable by type/agent/conversation/message and time range, with cursor pagination.",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleListEvents)

	huma.Register(s.API, huma.Operation{
		OperationID: "getEvent", Method: http.MethodGet, Path: "/v1/events/{id}",
		Summary: "Get an event", Tags: []string{"events"},
		Security: []map[string][]string{{"bearer": {}}},
	}, s.handleGetEvent)

	huma.Register(s.API, huma.Operation{
		OperationID: "redeliverEvent", Method: http.MethodPost, Path: "/v1/events/{id}/redeliver",
		Summary: "Redeliver an event", Tags: []string{"events"},
		Description:   "Re-enqueue webhook delivery for an event. With a webhook_id, replays to that subscriber; without, fans out to every originally-matched subscriber. Auto-deduplicated within a short window — receivers must dedup on event id. Returns 202 Accepted: the redelivery is durably enqueued for async submission, not delivered synchronously — the per-subscriber outcome surfaces via the delivery log, and each delivery's status is 'pending' (or 'scheduled' for the fan-out).",
		Security:      []map[string][]string{{"bearer": {}}},
		DefaultStatus: http.StatusAccepted,
	}, s.handleRedeliverEvent)
}

// RedeliverView mirrors the legacy redeliverResponse.
type RedeliverView struct {
	DeliveryID string `json:"delivery_id,omitempty"`
	EventID    string `json:"event_id"`
	WebhookID  string `json:"webhook_id,omitempty"`
	// Status is "pending" for a single-webhook replay (one scheduled delivery)
	// or "scheduled" for a bulk fan-out (see Deliveries for per-subscriber state).
	Status     string              `json:"status" doc:"Open set; tolerate unknown values. Known values: pending (single-webhook replay), scheduled (bulk fan-out)."`
	Deliveries []RedeliverDelivery `json:"deliveries,omitempty" nullable:"false"`
}

type RedeliverDelivery struct {
	WebhookID  string `json:"webhook_id"`
	DeliveryID string `json:"delivery_id,omitempty"`
	// Status is "pending" when the replay was scheduled, or "skipped" when this
	// subscriber's delivery could not be enqueued (see Reason).
	Status string `json:"status" doc:"Open set; tolerate unknown values. Known values: pending (replay scheduled), skipped (could not enqueue — see reason)."`
	Reason string `json:"reason,omitempty"`
}

// RedeliverEventRequest is the redeliver body (WH-6 naming).
type RedeliverEventRequest struct {
	WebhookID string `json:"webhook_id,omitempty"`
}

// UnmarshalJSON preserves the contract distinction between an omitted optional
// webhook_id (bulk fan-out) and an explicitly null, non-nullable webhook_id.
func (r *RedeliverEventRequest) UnmarshalJSON(data []byte) error {
	type plain RedeliverEventRequest
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["webhook_id"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("webhook_id must be a string")
	}
	*r = RedeliverEventRequest(decoded)
	return nil
}

type RedeliverEventInput struct {
	ID   string `path:"id"`
	Body RedeliverEventRequest
}

type redeliverOutput struct {
	Body RedeliverView
}

func (s *Server) handleRedeliverEvent(ctx context.Context, in *RedeliverEventInput) (*redeliverOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireEventsEnabled(); err != nil {
		return nil, err
	}
	if s.deps.LoadReplayEvent == nil || s.deps.InsertReplayDelivery == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "events API not configured")
	}
	if in.ID == "" {
		return nil, NewError(http.StatusBadRequest, "invalid_request", "missing event id")
	}
	webhookID := in.Body.WebhookID

	// Auto-idempotency keyed on (event, webhook): the same redeliver within
	// the window replays the cached schedule rather than double-enqueueing.
	// This is a SERVER-MINTED key — runIdempotentAuto namespaces it apart from
	// caller-supplied Idempotency-Key headers so a crafted header can't poison
	// (422) a later genuine redelivery of the same event.
	//
	// The key (event + webhook) is the discriminator here; the body hash is
	// constant by design (synthetic route + nil body), so it does no work for
	// this endpoint — uniqueness/replay is decided entirely by the key. That's
	// intentional, not a missing body hash.
	key := "replay:" + in.ID + ":" + webhookID
	_, body, err := runIdempotentAuto(s, ctx, user.ID, key, "/v1/events/redeliver", nil, func() (int, RedeliverView, error) {
		row, lerr := s.deps.LoadReplayEvent(ctx, user.ID, in.ID)
		if lerr != nil {
			switch {
			case errors.Is(lerr, agent.ErrEventNotFound):
				return 0, RedeliverView{}, NewError(http.StatusNotFound, "not_found", "event not found")
			case errors.Is(lerr, agent.ErrEventExpired):
				return 0, RedeliverView{}, NewError(http.StatusGone, "gone", "event expired (past 30-day retention)")
			default:
				return 0, RedeliverView{}, NewError(http.StatusInternalServerError, "internal_error", "failed to load event")
			}
		}
		if webhookID != "" {
			if !containsStr(row.MatchedWebhookIDs, webhookID) {
				return 0, RedeliverView{}, NewError(http.StatusConflict, "conflict", "webhook was not among the originally-matched subscribers")
			}
			dl, derr := s.deps.InsertReplayDelivery(ctx, in.ID, webhookID, row.EventType, row.MessageID, row.Envelope)
			if derr != nil {
				return 0, RedeliverView{}, NewError(http.StatusInternalServerError, "internal_error", "failed to schedule replay")
			}
			// Enqueue for immediate delivery. If it fails the row is durable and the
			// periodic reconciler re-drives it within a minute — report pending
			// (don't 500: a retry would create a duplicate replay row).
			if s.deps.EnqueueDelivery != nil {
				if err := s.deps.EnqueueDelivery(ctx, dl); err != nil {
					log.Printf("[events] replay %s enqueue failed (reconciler will retry): %v", dl, err)
				}
			}
			return http.StatusAccepted, RedeliverView{DeliveryID: dl, EventID: in.ID, WebhookID: webhookID, Status: "pending"}, nil
		}
		// Bulk fan-out to every originally-matched subscriber.
		deliveries := make([]RedeliverDelivery, 0, len(row.MatchedWebhookIDs))
		for _, whID := range row.MatchedWebhookIDs {
			dl, derr := s.deps.InsertReplayDelivery(ctx, in.ID, whID, row.EventType, row.MessageID, row.Envelope)
			if derr != nil {
				deliveries = append(deliveries, RedeliverDelivery{WebhookID: whID, Status: "skipped", Reason: "failed to schedule"})
				continue
			}
			// Enqueue for immediate delivery. On failure the row is durable +
			// reconciler-backed, so report pending (not skipped) — it will deliver.
			if s.deps.EnqueueDelivery != nil {
				if err := s.deps.EnqueueDelivery(ctx, dl); err != nil {
					log.Printf("[events] bulk replay %s enqueue failed (reconciler will retry): %v", dl, err)
				}
			}
			deliveries = append(deliveries, RedeliverDelivery{WebhookID: whID, DeliveryID: dl, Status: "pending"})
		}
		return http.StatusAccepted, RedeliverView{EventID: in.ID, Status: "scheduled", Deliveries: deliveries}, nil
	})
	if err != nil {
		return nil, err
	}
	return &redeliverOutput{Body: body}, nil
}

// rfc3339PtrOrEmpty renders an optional time filter as a stable string for the
// cursor's filter snapshot (nil → "").
func rfc3339PtrOrEmpty(p *time.Time) string {
	if p == nil {
		return ""
	}
	return rfc3339OrEmpty(*p)
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// requireEventsEnabled returns a 501 when the durable event log isn't
// populated on this deployment. Now unconditional in production; the guard
// remains so that a deployment which disables the outbox returns an explicit
// error rather than an empty list/get/redeliver — indistinguishable from a
// deployment where events simply haven't happened. Webhook *delivery* (River)
// is unaffected either way.
func (s *Server) requireEventsEnabled() error {
	if !s.deps.EventsEnabled {
		return NewError(http.StatusNotImplemented, "events_log_disabled",
			"the event log (list/get/redeliver events) is not enabled on this deployment; webhook delivery is unaffected")
	}
	return nil
}

func (s *Server) handleListEvents(ctx context.Context, in *ListEventsInput) (*listEventsOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireEventsEnabled(); err != nil {
		return nil, err
	}
	if s.deps.ListEvents == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "events API not configured")
	}
	since, err := parseRFC3339FilterPtr(in.Since, "since")
	if err != nil {
		return nil, err
	}
	until, err := parseRFC3339FilterPtr(in.Until, "until")
	if err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	var cur eventsCursor
	if in.Cursor != "" {
		if err := DecodeCursor([]string{s.deps.CursorSecret}, in.Cursor, &cur); err != nil {
			return nil, NewError(http.StatusBadRequest, "invalid_cursor", "invalid pagination cursor")
		}
		if cur.Ty != in.Type || cur.Ag != in.AgentID || cur.Co != in.ConversationID ||
			cur.Ms != in.MessageID || cur.Si != rfc3339PtrOrEmpty(since) || cur.Un != rfc3339PtrOrEmpty(until) {
			return nil, NewError(http.StatusBadRequest, "invalid_cursor",
				"cursor was created with different filters — start a new query without a cursor")
		}
	}
	// Fetch limit+1 to detect a further page (avoids a spurious next_cursor on an
	// exact-multiple final page).
	events, err := s.deps.ListEvents(ctx, EventQuery{
		UserID: user.ID, Type: in.Type, AgentID: in.AgentID, ConversationID: in.ConversationID,
		MessageID: in.MessageID, Since: since, Until: until,
		CursorCreatedAt: cur.C, CursorID: cur.I, Limit: limit + 1,
	})
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list events")
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	var nextCursor string
	if hasMore {
		last := events[len(events)-1]
		nextCursor, err = EncodeCursor(s.deps.CursorSecret, eventsCursor{
			C: last.CreatedAt, I: last.ID,
			Ty: in.Type, Ag: in.AgentID, Co: in.ConversationID, Ms: in.MessageID,
			Si: rfc3339PtrOrEmpty(since), Un: rfc3339PtrOrEmpty(until),
		})
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
		}
	}
	return &listEventsOutput{Body: NewPage(events, nextCursor)}, nil
}

func (s *Server) handleGetEvent(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*eventOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireEventsEnabled(); err != nil {
		return nil, err
	}
	if s.deps.GetEvent2 == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "events API not configured")
	}
	if in.ID == "" {
		return nil, NewError(http.StatusBadRequest, "invalid_request", "missing event id")
	}
	ej, err := s.deps.GetEvent2(ctx, user.ID, in.ID)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrEventNotFound):
			return nil, NewError(http.StatusNotFound, "not_found", "event not found")
		case errors.Is(err, agent.ErrEventExpired):
			return nil, NewError(http.StatusGone, "gone", "event expired (past 30-day retention)")
		default:
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to fetch event")
		}
	}
	return &eventOutput{Body: *ej}, nil
}

// parseRFC3339FilterPtr is the *time.Time variant of parseRFC3339Filter.
func parseRFC3339FilterPtr(raw, name string) (*time.Time, error) {
	t, err := parseRFC3339Filter(raw, name)
	if err != nil {
		return nil, err
	}
	if t.IsZero() {
		return nil, nil
	}
	return &t, nil
}
