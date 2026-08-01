package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
)

// AgentSuppressionView is one recipient block scoped to an exact sending
// agent. AgentEmail keeps list items self-describing outside their route.
type AgentSuppressionView struct {
	AgentEmail string    `json:"agent_email"`
	Address    string    `json:"address"`
	Reason     string    `json:"reason,omitempty"`
	Source     string    `json:"source" doc:"How the block was created. Known values: unsubscribe, manual."`
	CreatedAt  time.Time `json:"created_at"`
}

func agentSuppressionView(sp identity.AgentSuppression) AgentSuppressionView {
	return AgentSuppressionView{
		AgentEmail: sp.AgentEmail,
		Address:    sp.Address,
		Reason:     sp.Reason,
		Source:     sp.Source,
		CreatedAt:  sp.CreatedAt,
	}
}

type agentSuppressionsCursor struct {
	CreatedAt  time.Time `json:"c"`
	Address    string    `json:"a"`
	AgentEmail string    `json:"g"`
}

type listAgentSuppressionsInput struct {
	Email string `path:"email"`
	PageParams
}

type listAgentSuppressionsOutput struct {
	Body Page[AgentSuppressionView]
}

type CreateAgentSuppressionRequest struct {
	Address string `json:"address" required:"true" maxLength:"320" doc:"Recipient email address to suppress for this agent. At most 320 Unicode code points."`
	Reason  string `json:"reason,omitempty" maxLength:"2000" doc:"Optional account-supplied reason for the manual block. At most 2000 Unicode code points."`
}

type createAgentSuppressionInput struct {
	Email string `path:"email"`
	Body  CreateAgentSuppressionRequest
}

type createAgentSuppressionOutput struct {
	Body AgentSuppressionView
}

type deleteAgentSuppressionInput struct {
	Email   string `path:"email"`
	Address string `path:"address"`
	DeleteConfirm
}

type deleteAgentSuppressionOutput struct {
	Body DeleteSuppressionResult
}

const agentSuppressionBetaDescription = "Beta: agent-scoped suppression management may change before it is declared stable."

func (s *Server) registerAgentSuppressions() {
	huma.Register(s.API, huma.Operation{
		OperationID: "listAgentSuppressions", Method: http.MethodGet, Path: "/v1/agents/{email}/suppressions",
		Summary: "List an agent's suppressed recipients (beta)", Tags: []string{"agents"},
		Description: "Lists recipient addresses blocked only for this exact sending agent. Account-scoped credentials only. " + agentSuppressionBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleListAgentSuppressions)

	huma.Register(s.API, huma.Operation{
		OperationID: "createAgentSuppression", Method: http.MethodPost, Path: "/v1/agents/{email}/suppressions",
		Summary: "Suppress a recipient for an agent (beta)", Tags: []string{"agents"},
		Description: "Idempotently creates a manual recipient block for this exact sending agent. Account-scoped credentials only. " + agentSuppressionBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleCreateAgentSuppression)

	huma.Register(s.API, huma.Operation{
		OperationID: "deleteAgentSuppression", Method: http.MethodDelete, Path: "/v1/agents/{email}/suppressions/{address}",
		Summary: "Remove an agent recipient suppression (beta)", Tags: []string{"agents"},
		Description: "Removes only the exact agent-scoped block. Requires ?confirm=DELETE. Account-scoped credentials only. " + agentSuppressionBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleDeleteAgentSuppression)
}

// resolveOwnedAgentForSuppressionManagement enforces account scope before
// lookup and deliberately conflates missing and foreign agents into the same
// response, so this administration surface cannot enumerate another account.
func (s *Server) resolveOwnedAgentForSuppressionManagement(ctx context.Context, address string) (*identity.Principal, *identity.AgentIdentity, error) {
	p, err := s.requireAccountScope(ctx)
	if err != nil {
		return nil, nil, err
	}
	if s.deps.GetAgent == nil {
		return nil, nil, NewError(http.StatusInternalServerError, "internal_error", "agent lookup unavailable")
	}
	ag, err := s.deps.GetAgent(ctx, identity.NormalizeEmail(address))
	if err != nil || ag == nil || ag.UserID != p.User.ID {
		return nil, nil, NewError(http.StatusNotFound, "not_found", "agent not found")
	}
	return p, ag, nil
}

func (s *Server) handleListAgentSuppressions(ctx context.Context, in *listAgentSuppressionsInput) (*listAgentSuppressionsOutput, error) {
	p, ag, err := s.resolveOwnedAgentForSuppressionManagement(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	if s.deps.ListAgentSuppressions == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "agent suppressions are not available on this deployment")
	}
	var afterCreatedAt time.Time
	var afterAddress string
	if in.Cursor != "" {
		var cur agentSuppressionsCursor
		if err := s.decodeCursor(p.User.ID, cursorAgentSuppressions, in.Cursor, &cur); err != nil {
			return nil, err
		}
		if identity.NormalizeEmail(cur.AgentEmail) != ag.ID {
			return nil, NewError(http.StatusBadRequest, "invalid_cursor", "invalid pagination cursor")
		}
		afterCreatedAt, afterAddress = cur.CreatedAt, cur.Address
	}
	limit := effectiveLimit(in.Limit)
	rows, err := s.deps.ListAgentSuppressions(ctx, p.User.ID, ag.ID, limit+1, afterCreatedAt, afterAddress)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list agent suppressions")
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	var nextCursor string
	if hasMore {
		last := rows[len(rows)-1]
		nextCursor, err = EncodeCursor(s.deps.CursorSecret, p.User.ID, cursorAgentSuppressions, agentSuppressionsCursor{
			CreatedAt: last.CreatedAt, Address: last.Address, AgentEmail: ag.ID,
		})
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
		}
	}
	items := make([]AgentSuppressionView, len(rows))
	for i := range rows {
		items[i] = agentSuppressionView(rows[i])
	}
	return &listAgentSuppressionsOutput{Body: NewPage(items, nextCursor)}, nil
}

func (s *Server) handleCreateAgentSuppression(ctx context.Context, in *createAgentSuppressionInput) (*createAgentSuppressionOutput, error) {
	p, ag, err := s.resolveOwnedAgentForSuppressionManagement(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	if s.deps.AddAgentSuppression == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "agent suppressions are not available on this deployment")
	}
	address := identity.NormalizeEmail(in.Body.Address)
	parsed, parseErr := mail.ParseAddress(address)
	if address == "" || parseErr != nil || identity.NormalizeEmail(parsed.Address) != address {
		return nil, NewError(http.StatusBadRequest, "invalid_request", "address must be a valid email address")
	}
	sp, _, err := s.deps.AddAgentSuppression(ctx, p.User.ID, ag.ID, address, in.Body.Reason, "manual", s.deps.AgentSuppressionAddedHook)
	if errors.Is(err, identity.ErrAgentNotFound) {
		return nil, NewError(http.StatusNotFound, "not_found", "agent not found")
	}
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to create agent suppression")
	}
	return &createAgentSuppressionOutput{Body: agentSuppressionView(sp)}, nil
}

func (s *Server) handleDeleteAgentSuppression(ctx context.Context, in *deleteAgentSuppressionInput) (*deleteAgentSuppressionOutput, error) {
	p, ag, err := s.resolveOwnedAgentForSuppressionManagement(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	if s.deps.RemoveAgentSuppression == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "agent suppressions are not available on this deployment")
	}
	address := identity.NormalizeEmail(in.Address)
	found, err := s.deps.RemoveAgentSuppression(ctx, p.User.ID, ag.ID, address)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to remove agent suppression")
	}
	if !found {
		return nil, NewError(http.StatusNotFound, "not_found", "address not on the agent suppression list")
	}
	return &deleteAgentSuppressionOutput{Body: DeleteSuppressionResult{Deleted: true, Address: address}}, nil
}
