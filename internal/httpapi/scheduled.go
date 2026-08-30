package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
)

const scheduledBetaDoc = "Beta: scheduled sending is unstable — its shape may change before it is declared stable."

// The scheduled queue (/v1/scheduled) is the operator surface for outbound
// messages that have been accepted and are waiting for a future send_at to fire
// (#815). It is a first-class, ACCOUNT-SCOPED resource, deliberately parallel to
// the review queue (/v1/reviews): the two are disjoint (a held draft is never
// delivery_status='accepted', so it never appears here). The agent's /messages
// surface can already show a scheduled row inline, but only this endpoint lets a
// client ask the server for the scheduled set as a whole.

// ScheduledMessageView is one item in the scheduled queue — non-secret summary
// of an outbound message queued to send at a future instant.
type ScheduledMessageView struct {
	ID    string `json:"id" doc:"The scheduled message's id (msg_…). Interchangeable with the message id on GET /v1/agents/{email}/messages/{id}."`
	Agent string `json:"agent_email" doc:"The inbox (agent address) the scheduled message belongs to."`
	// Direction is always outbound for a scheduled send; kept as a field (and a
	// closed enum) to mirror ReviewView so a client can reuse one row renderer.
	Direction      string     `json:"direction" enum:"outbound"`
	HeaderFrom     *string    `json:"header_from" nullable:"true" doc:"The sender identity for this outbound message; null when unavailable."`
	To             []string   `json:"to" nullable:"false"`
	Subject        string     `json:"subject"`
	ConversationID string     `json:"conversation_id,omitempty"`
	DeliveryStatus string     `json:"delivery_status" doc:"Always accepted while a future scheduled_at is pending: scheduling introduces no new delivery_status (migration 084). Open set; tolerate unknown values."`
	CreatedAt      time.Time  `json:"created_at"`
	ScheduledAt    *time.Time `json:"scheduled_at" format:"date-time" doc:"Beta: scheduled sending may change before it is declared stable. The future instant the message is queued to be submitted. Always present and in the future for items in this queue."`
}

func scheduledMessageView(it identity.ScheduledListItem) ScheduledMessageView {
	headerFrom := it.HeaderFrom
	if headerFrom == "" {
		headerFrom = it.Sender
	}
	return ScheduledMessageView{
		ID:             it.ID,
		Agent:          it.AgentID,
		Direction:      it.Direction,
		HeaderFrom:     nullableMessageString(headerFrom),
		To:             orEmptyStrings(it.To),
		Subject:        it.Subject,
		ConversationID: it.ConversationID,
		DeliveryStatus: it.DeliveryStatus,
		CreatedAt:      it.CreatedAt,
		ScheduledAt:    utcPtr(it.ScheduledAt),
	}
}

type listScheduledOutput struct{ Body Page[ScheduledMessageView] }

// listScheduledInput carries the standard cursor/limit (PageParams). The queue
// is keyset-paginated on (scheduled_at, id); it grows with the scheduled-send
// backlog, so it must not return the whole set in one page.
type listScheduledInput struct {
	PageParams
}

func (s *Server) registerScheduled() {
	registerOp(s.API, huma.Operation{
		OperationID: "listScheduled", Method: http.MethodGet, Path: "/v1/scheduled",
		Summary: "List messages awaiting a scheduled send (beta)", Tags: []string{"scheduled"},
		Description: "The scheduled-send queue: every outbound message accepted and waiting for a future send_at to fire, across the account's inboxes, soonest-first. Account-scoped credentials only. Disjoint from GET /v1/reviews — held drafts are not yet accepted and appear there instead. " + scheduledBetaDoc,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleListScheduled)
}

func (s *Server) handleListScheduled(ctx context.Context, in *listScheduledInput) (*listScheduledOutput, error) {
	p, err := s.requireAccountScope(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.ListScheduled == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "scheduled sending is not available on this deployment")
	}
	afterScheduledAt, afterID, err := s.decodeKeyset(p.User.ID, cursorScheduled, in.Cursor)
	if err != nil {
		return nil, err
	}
	limit := effectiveLimit(in.Limit)
	// Fetch limit+1 to detect a further page.
	items, err := s.deps.ListScheduled(ctx, p.User.ID, limit+1, afterScheduledAt, afterID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list scheduled messages")
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	views := make([]ScheduledMessageView, 0, len(items))
	for _, it := range items {
		views = append(views, scheduledMessageView(it))
	}
	var nextCursor string
	if hasMore {
		last := items[len(items)-1]
		var lastScheduled time.Time
		if last.ScheduledAt != nil {
			lastScheduled = *last.ScheduledAt
		}
		if nextCursor, err = s.encodeKeyset(p.User.ID, cursorScheduled, lastScheduled, last.ID); err != nil {
			return nil, err
		}
	}
	return &listScheduledOutput{Body: NewPage(views, nextCursor)}, nil
}
