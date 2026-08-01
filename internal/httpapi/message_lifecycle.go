package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

const messageLifecycleCursorVersion = 1
const messageLifecycleDefaultLimit = 50
const messageLifecycleSortAscending = "asc"
const messageLifecycleBetaDoc = "Beta: message lifecycle may change before it is declared stable."

type messageLifecycleInput struct {
	Email  string `path:"email"`
	ID     string `path:"id"`
	Cursor string `query:"cursor" doc:"Opaque pagination cursor from a previous response's next_cursor."`
	Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50" doc:"Maximum number of lifecycle transitions to return (1-100)."`
}

type messageLifecycleOutput struct {
	Body Page[messagelifecycle.MessageLifecycleTransition]
}

type messageLifecycleCursor struct {
	Version    int       `json:"v"`
	AgentID    string    `json:"g"`
	MessageID  string    `json:"m"`
	OccurredAt time.Time `json:"t"`
	ID         string    `json:"i"`
	Sort       string    `json:"s"`
}

func (s *Server) registerMessageLifecycle() {
	huma.Register(s.API, huma.Operation{
		OperationID: "getMessageLifecycle",
		Method:      http.MethodGet,
		Path:        "/v1/agents/{email}/messages/{id}/lifecycle",
		Summary:     "Get a message's lifecycle (beta)",
		Description: "Returns the observations e2a recorded for one inbound or outbound message in deterministic ascending (occurred_at, id) order. Delivery means recipient-server acceptance and does not claim inbox placement. " + messageLifecycleBetaDoc,
		Tags:        []string{"messages"},
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleMessageLifecycle)
}

func (s *Server) handleMessageLifecycle(ctx context.Context, in *messageLifecycleInput) (*messageLifecycleOutput, error) {
	agent, err := s.resolveOwnedAgent(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	if s.deps.ListMessageLifecycle == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "message lifecycle is not available on this deployment")
	}

	var afterTime time.Time
	var afterID string
	if in.Cursor != "" {
		var cursor messageLifecycleCursor
		if err := s.decodeCursor(agent.UserID, cursorMessageLifecycle, in.Cursor, &cursor); err != nil {
			return nil, err
		}
		if cursor.Version != messageLifecycleCursorVersion || cursor.AgentID != agent.ID || cursor.MessageID != in.ID ||
			cursor.OccurredAt.IsZero() || cursor.ID == "" || cursor.Sort != messageLifecycleSortAscending {
			return nil, NewError(http.StatusBadRequest, "invalid_cursor", "invalid pagination cursor")
		}
		afterTime, afterID = cursor.OccurredAt.UTC(), cursor.ID
	}

	items, err := s.deps.ListMessageLifecycle(ctx, in.ID, agent.ID)
	if errors.Is(err, messagelifecycle.ErrMessageNotFound) {
		return nil, NewError(http.StatusNotFound, "not_found", "message not found")
	}
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list message lifecycle")
	}

	// Defensive ordering keeps every injected implementation on the same wire
	// contract as the PostgreSQL store: ascending (occurred_at, id).
	items = append([]messagelifecycle.MessageLifecycleTransition(nil), items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].OccurredAt.Equal(items[j].OccurredAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].OccurredAt.Before(items[j].OccurredAt)
	})
	if !afterTime.IsZero() {
		start := sort.Search(len(items), func(i int) bool {
			return items[i].OccurredAt.After(afterTime) || (items[i].OccurredAt.Equal(afterTime) && items[i].ID > afterID)
		})
		items = items[start:]
	}

	limit := in.Limit
	if limit <= 0 {
		limit = messageLifecycleDefaultLimit
	}
	// Keep one extra item only long enough to determine whether another page
	// exists; it is never exposed in the current response.
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	var nextCursor string
	if hasMore {
		last := items[len(items)-1]
		nextCursor, err = EncodeCursor(s.deps.CursorSecret, agent.UserID, cursorMessageLifecycle, messageLifecycleCursor{
			Version: messageLifecycleCursorVersion, AgentID: agent.ID, MessageID: in.ID,
			OccurredAt: last.OccurredAt.UTC(), ID: last.ID, Sort: messageLifecycleSortAscending,
		})
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
		}
	}
	return &messageLifecycleOutput{Body: NewPage(items, nextCursor)}, nil
}
