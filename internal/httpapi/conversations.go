package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
)

// ConversationSummaryView is one conversation in the list. Timestamps are
// typed time.Time with format date-time (C-1) — consistent with every other
// timestamp in the surface (was previously plain strings, which generated an
// untyped `string` in the SDKs and risked a .getTime()/parse crash).
type ConversationSummaryView struct {
	ID             string    `json:"id"`
	LastMessageAt  time.Time `json:"last_message_at" format:"date-time"`
	FirstMessageAt time.Time `json:"first_message_at" format:"date-time"`
	MessageCount   int       `json:"message_count"`
	InboundCount   int       `json:"inbound_count"`
	OutboundCount  int       `json:"outbound_count"`
	HasUnread      bool      `json:"has_unread"`
	LatestSubject  string    `json:"latest_subject"`
	LatestSender   string    `json:"latest_from"`
}

func conversationSummaryView(c identity.ConversationSummary) ConversationSummaryView {
	return ConversationSummaryView{
		ID:             c.ID,
		LastMessageAt:  c.LastMessageAt.UTC(),
		FirstMessageAt: c.FirstMessageAt.UTC(),
		MessageCount:   c.MessageCount,
		InboundCount:   c.InboundCount,
		OutboundCount:  c.OutboundCount,
		HasUnread:      c.HasUnread,
		LatestSubject:  c.LatestSubject,
		LatestSender:   c.LatestSender,
	}
}

// ConversationDetailView is the single-conversation shape: the summary
// fields (flattened via embedding, matching the legacy top-level layout)
// plus participants, labels, and the member message summaries.
type ConversationDetailView struct {
	ConversationSummaryView
	Participants []string             `json:"participants" nullable:"false"`
	Labels       []string             `json:"labels" nullable:"false"`
	Messages     []MessageSummaryView `json:"messages" nullable:"false"`
}

// ListConversationsInput — since/until + cursor/limit. Cursor continuation IS
// supported: the handler fetches limit+1, and when there are more it keyset-
// encodes the last row's (last_message_at, conversation_id) into next_cursor
// (see handleListConversations: hasMore → EncodeCursor(conversationsCursor{...})).
// next_cursor is null only on the last page.
type ListConversationsInput struct {
	Address string `path:"email"`
	Since   string `query:"since" doc:"RFC3339."`
	Until   string `query:"until" doc:"RFC3339."`
	Cursor  string `query:"cursor" doc:"Opaque pagination cursor from a previous response's next_cursor. Continuation requests must not change since/until."`
	Limit   int    `query:"limit" minimum:"1" maximum:"100" default:"100"`
}

// conversationsCursor is the opaque keyset position + the filter identity so a
// continuation can't silently change the window under the cursor.
type conversationsCursor struct {
	LastMessageAt  time.Time `json:"l"`
	ConversationID string    `json:"i"`
	AgentID        string    `json:"a"`
	Since          string    `json:"s,omitempty"`
	Until          string    `json:"u,omitempty"`
}

type listConversationsOutput struct {
	Body Page[ConversationSummaryView]
}

// ConversationIDParam is the path input for the single-conversation read.
type ConversationIDParam struct {
	Address        string `path:"email"`
	ConversationID string `path:"id"`
}

type conversationOutput struct {
	Body ConversationDetailView
}

func (s *Server) registerConversations() {
	huma.Register(s.API, huma.Operation{
		OperationID: "listConversations",
		Method:      http.MethodGet,
		Path:        "/v1/agents/{email}/conversations",
		Summary:     "List conversations",
		Description: "List an agent's application conversation groups (derived from messages.conversation_id). Conversation IDs are independent of email thread topology.",
		Tags:        []string{"conversations"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleListConversations)

	huma.Register(s.API, huma.Operation{
		OperationID: "getConversation",
		Method:      http.MethodGet,
		Path:        "/v1/agents/{email}/conversations/{id}",
		Summary:     "Get a conversation",
		Description: "Fetch a single application conversation group with its participants, labels, and member messages. Conversation IDs are independent of email thread topology.",
		Tags:        []string{"conversations"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleGetConversation)
}

func (s *Server) handleListConversations(ctx context.Context, in *ListConversationsInput) (*listConversationsOutput, error) {
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	if s.deps.ListConversations == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "conversation list unavailable")
	}
	since, err := parseRFC3339Filter(in.Since, "since")
	if err != nil {
		return nil, err
	}
	until, err := parseRFC3339Filter(in.Until, "until")
	if err != nil {
		return nil, err
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return nil, NewError(http.StatusBadRequest, "invalid_filter", "since must be earlier than until")
	}
	// Decode + validate the cursor against the current filter identity (CV-3).
	var afterTime time.Time
	var afterID string
	if in.Cursor != "" {
		var cur conversationsCursor
		if err := s.decodeCursor(ag.UserID, cursorConversations, in.Cursor, &cur); err != nil {
			return nil, err
		}
		if cur.AgentID != ag.ID || cur.Since != rfc3339OrEmpty(since) || cur.Until != rfc3339OrEmpty(until) {
			return nil, NewError(http.StatusBadRequest, "invalid_cursor",
				"cursor was created with different filters — start a new query without a cursor")
		}
		afterTime = cur.LastMessageAt
		afterID = cur.ConversationID
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	// Fetch limit+1 to detect a further page.
	convos, err := s.deps.ListConversations(ctx, identity.ConversationListFilter{
		AgentID:             ag.ID,
		Limit:               limit + 1,
		Since:               since,
		Until:               until,
		AfterLastMessageAt:  afterTime,
		AfterConversationID: afterID,
	})
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to fetch conversations")
	}
	hasMore := len(convos) > limit
	if hasMore {
		convos = convos[:limit]
	}
	items := make([]ConversationSummaryView, len(convos))
	for i, c := range convos {
		items[i] = conversationSummaryView(c)
	}
	var nextCursor string
	if hasMore {
		last := convos[len(convos)-1]
		nextCursor, err = EncodeCursor(s.deps.CursorSecret, ag.UserID, cursorConversations, conversationsCursor{
			LastMessageAt:  last.LastMessageAt,
			ConversationID: last.ID,
			AgentID:        ag.ID,
			Since:          rfc3339OrEmpty(since),
			Until:          rfc3339OrEmpty(until),
		})
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
		}
	}
	return &listConversationsOutput{Body: NewPage(items, nextCursor)}, nil
}

func (s *Server) handleGetConversation(ctx context.Context, in *ConversationIDParam) (*conversationOutput, error) {
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	// Length (200, counted in runes so any stored conversation_id the request
	// schemas admit stays fetchable) + CR/LF via the shared check.
	if err := validateConversationID(in.ConversationID); err != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	if s.deps.GetConversation == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "conversation lookup unavailable")
	}
	detail, err := s.deps.GetConversation(ctx, ag.ID, in.ConversationID)
	if err != nil || detail == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "conversation not found")
	}
	out := &conversationOutput{Body: ConversationDetailView{
		ConversationSummaryView: conversationSummaryView(detail.ConversationSummary),
		Participants:            orEmptyStrings(detail.Participants),
		Labels:                  orEmptyStrings(detail.Labels),
		Messages:                make([]MessageSummaryView, len(detail.Messages)),
	}}
	for i, m := range detail.Messages {
		out.Body.Messages[i] = messageSummaryFromIdentity(m)
	}
	return out, nil
}
