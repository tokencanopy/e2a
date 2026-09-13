package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
)

type AgentSignupRequest struct {
	HumanEmail    string `json:"human_email" required:"true" maxLength:"320" format:"email" doc:"Email address of the human who will verify and own this agent."`
	DisplayName   string `json:"display_name" required:"true" minLength:"1" maxLength:"200" doc:"Human-readable agent name. The inbox slug is derived from this value."`
	NoteToHuman   string `json:"note_to_human,omitempty" maxLength:"2000" doc:"Optional explanation included verbatim in the verification email."`
	Harness       string `json:"harness,omitempty" maxLength:"100" doc:"Optional agent harness identifier for diagnostics."`
	CurrentAPIKey string `json:"current_api_key,omitempty" maxLength:"256" doc:"Current agent-scoped key. Required when resuming the same pending human_email + display_name; successful resume rotates it."`
}

type agentSignupInput struct{ Body AgentSignupRequest }

type AgentSignupRestrictionsView struct {
	CanReceiveFrom      string   `json:"can_receive_from" doc:"Always anyone while pending."`
	SendTo              []string `json:"send_to" doc:"The only recipient allowed while pending."`
	SendsPer24h         int      `json:"sends_per_24h"`
	CanCreateIdentities bool     `json:"can_create_identities"`
}

type AgentSignupView struct {
	ID                 string    `json:"id"`
	Inbox              string    `json:"inbox" doc:"Provisioned inbox on the deployment shared agent domain."`
	HumanEmail         string    `json:"human_email"`
	DisplayName        string    `json:"display_name"`
	NoteToHuman        string    `json:"note_to_human,omitempty"`
	Harness            string    `json:"harness,omitempty"`
	Status             string    `json:"status" doc:"Open lifecycle value. Known values: pending, verified, rejected."`
	ReviewOutbound     bool      `json:"review_outbound"`
	VerificationExpiry time.Time `json:"verification_expires_at"`
	CreatedAt          time.Time `json:"created_at"`
}

type AgentSignupCreateResponse struct {
	AgentSignupView
	APIKey       string                      `json:"api_key" doc:"One-time agent-scoped API key. Store it now; a re-signup rotates it."`
	ConsoleURL   string                      `json:"console_url,omitempty"`
	Restrictions AgentSignupRestrictionsView `json:"restrictions"`
}

type agentSignupCreateOutput struct {
	Status       int
	CacheControl string `header:"Cache-Control"`
	Body         AgentSignupCreateResponse
}

type VerifyAgentSignupRequest struct {
	Code           string `json:"code" required:"true" pattern:"^[0-9]{6}$" doc:"Six-digit code sent to human_email."`
	ReviewOutbound bool   `json:"review_outbound,omitempty" doc:"If true, outbound to recipients other than the human enters the existing review queue after verification."`
}
type verifyAgentSignupInput struct{ Body VerifyAgentSignupRequest }
type agentSignupOutput struct{ Body AgentSignupView }

type listPendingAgentSignupsInput struct{ PageParams }
type listPendingAgentSignupsOutput struct{ Body Page[AgentSignupView] }

type approveAgentSignupInput struct {
	ID   string `path:"id"`
	Body struct {
		ReviewOutbound bool `json:"review_outbound,omitempty"`
	}
}
type rejectAgentSignupInput struct {
	ID   string `path:"id"`
	Body struct{}
}
type rejectAgentSignupOutput struct {
	Body struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
}

func signupView(s *identity.AgentSignup) AgentSignupView {
	return AgentSignupView{ID: s.ID, Inbox: s.AgentID, HumanEmail: s.HumanEmail, DisplayName: s.DisplayName, NoteToHuman: s.NoteToHuman,
		Harness: s.Harness, Status: s.Status, ReviewOutbound: s.ReviewOutbound,
		VerificationExpiry: s.CodeExpiresAt, CreatedAt: s.CreatedAt}
}

func (s *Server) registerAgentSignup() {
	registerOp(s.API, huma.Operation{
		OperationID: "createAgentSignup", Method: http.MethodPost, Path: "/v1/agent-signup",
		Summary: "Create a provisional agent identity (beta)", Tags: []string{"agent signup"},
		Description: "Public, no API key required for first signup. Creates one receiving inbox and an agent-scoped key, then sends a six-digit code to the human. Repeating the same human_email + display_name requires current_api_key, rotates that key, and resends verification.",
		Extensions:  beta(), Responses: map[string]*huma.Response{"429": s.rateLimitedResponse(), "default": s.errorEnvelopeResponse()},
	}, s.handleCreateAgentSignup)
	registerOp(s.API, huma.Operation{
		OperationID: "verifyAgentSignup", Method: http.MethodPost, Path: "/v1/agent-signup/verify",
		Summary: "Verify a provisional identity with its code (beta)", Tags: []string{"agent signup"},
		Description: "Requires the agent-scoped key returned by signup. A successful code verification unlocks normal plan limits and may enable the existing human-review queue.",
		Security:    []map[string][]string{{"bearer": {}}}, Extensions: beta(),
	}, s.handleVerifyAgentSignup)
	registerOp(s.API, huma.Operation{
		OperationID: "listPendingAgentSignups", Method: http.MethodGet, Path: "/v1/agent-signup/pending",
		Summary: "List pending agent identities for the signed-in human (beta)", Tags: []string{"agent signup"},
		Security: []map[string][]string{{"bearer": {}}}, Extensions: beta(),
	}, s.handleListPendingAgentSignups)
	registerOp(s.API, huma.Operation{
		OperationID: "approveAgentSignup", Method: http.MethodPost, Path: "/v1/agent-signup/{id}/approve",
		Summary: "Approve a pending agent identity (beta)", Tags: []string{"agent signup"},
		Description: "Account-scoped. Optionally routes the verified agent's non-human outbound through the existing review queue.",
		Security:    []map[string][]string{{"bearer": {}}}, Extensions: beta(),
	}, s.handleApproveAgentSignup)
	registerOp(s.API, huma.Operation{
		OperationID: "rejectAgentSignup", Method: http.MethodPost, Path: "/v1/agent-signup/{id}/reject",
		Summary: "Reject and deactivate a pending agent identity (beta)", Tags: []string{"agent signup"},
		Description: "Account-scoped. Revokes the signup key and deactivates the inbox.",
		Security:    []map[string][]string{{"bearer": {}}}, Extensions: beta(),
	}, s.handleRejectAgentSignup)

	// Make the create response status-dependent (201 new, 200 replay) while
	// documenting both concrete bodies rather than an opaque default.
	op := s.API.OpenAPI().Paths["/v1/agent-signup"].Post
	op.Responses["200"] = s.jsonResponse(reflect.TypeOf(AgentSignupCreateResponse{}), "AgentSignupCreateResponse", "Existing pending signup; key rotated and verification resent.")
	op.Responses["201"] = s.jsonResponse(reflect.TypeOf(AgentSignupCreateResponse{}), "AgentSignupCreateResponse", "Provisional identity created.")
}

func validBareEmail(v string) bool {
	trimmed := strings.TrimSpace(v)
	parsed, err := mail.ParseAddress(trimmed)
	return err == nil && strings.EqualFold(parsed.Address, trimmed)
}

func signupError(err error) error {
	switch {
	case errors.Is(err, agent.ErrAgentSignupUnavailable):
		return NewError(http.StatusNotImplemented, "not_implemented", "agent signup is not configured on this deployment")
	case errors.Is(err, identity.ErrAgentSignupNotFound):
		return NewError(http.StatusNotFound, "not_found", "agent signup not found")
	case errors.Is(err, identity.ErrAgentSignupFinal):
		return NewError(http.StatusConflict, "conflict", "agent signup is already verified or rejected")
	case errors.Is(err, identity.ErrAgentSignupResumeRequired):
		return NewError(http.StatusConflict, "conflict", "an agent signup with this human_email and display_name already exists; provide its current_api_key to rotate and resend verification")
	case errors.Is(err, identity.ErrAgentSignupMailLimit):
		return NewError(http.StatusTooManyRequests, "rate_limited", "too many verification emails were requested for this human email; retry after 24 hours")
	case errors.Is(err, identity.ErrAgentSignupCodeInvalid):
		return NewError(http.StatusBadRequest, "invalid_request", "verification code is invalid")
	case errors.Is(err, identity.ErrAgentSignupCodeExpired):
		return NewError(http.StatusGone, "gone", "verification code has expired; repeat signup to receive a new code")
	case errors.Is(err, identity.ErrAgentSignupAttemptsExhausted):
		return NewError(http.StatusTooManyRequests, "rate_limited", "too many verification attempts; repeat signup to receive a new code")
	}
	var capErr *identity.AgentLimitExceededError
	if errors.As(err, &capErr) {
		return NewError(http.StatusPaymentRequired, "limit_exceeded", "agent limit reached")
	}
	var limErr *limits.LimitExceededError
	if errors.As(err, &limErr) {
		if env, ok := limitEnvelope(err); ok {
			return env
		}
	}
	return NewError(http.StatusInternalServerError, "internal_error", "agent signup failed")
}

func (s *Server) handleCreateAgentSignup(ctx context.Context, in *agentSignupInput) (*agentSignupCreateOutput, error) {
	if !validBareEmail(in.Body.HumanEmail) || strings.TrimSpace(in.Body.DisplayName) == "" {
		return nil, NewError(http.StatusUnprocessableEntity, "invalid_request", "human_email must be a bare email address and display_name must not be blank")
	}
	if s.deps.RegisterAgentSignup == nil {
		return nil, signupError(agent.ErrAgentSignupUnavailable)
	}
	result, err := s.deps.RegisterAgentSignup(ctx, identity.NormalizeEmail(in.Body.HumanEmail), strings.TrimSpace(in.Body.DisplayName), strings.TrimSpace(in.Body.NoteToHuman), strings.TrimSpace(in.Body.Harness), strings.TrimSpace(in.Body.CurrentAPIKey))
	if err != nil {
		return nil, signupError(err)
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	return &agentSignupCreateOutput{Status: status, CacheControl: "no-store", Body: AgentSignupCreateResponse{
		AgentSignupView: signupView(result.Signup), APIKey: result.APIKey.PlaintextKey, ConsoleURL: s.deps.AgentSignupConsoleURL,
		Restrictions: AgentSignupRestrictionsView{CanReceiveFrom: "anyone", SendTo: []string{result.Signup.HumanEmail}, SendsPer24h: identity.AgentSignupSendLimit, CanCreateIdentities: false},
	}}, nil
}

func (s *Server) handleVerifyAgentSignup(ctx context.Context, in *verifyAgentSignupInput) (*agentSignupOutput, error) {
	p, err := s.requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if p.Scope != identity.ScopeAgent || p.AgentID == "" {
		return nil, NewError(http.StatusForbidden, "forbidden", "verification requires the agent-scoped key returned by signup")
	}
	if s.deps.VerifyAgentSignup == nil {
		return nil, signupError(agent.ErrAgentSignupUnavailable)
	}
	v, err := s.deps.VerifyAgentSignup(ctx, p.AgentID, in.Body.Code, in.Body.ReviewOutbound)
	if err != nil {
		return nil, signupError(err)
	}
	return &agentSignupOutput{Body: signupView(v)}, nil
}

func (s *Server) handleListPendingAgentSignups(ctx context.Context, in *listPendingAgentSignupsInput) (*listPendingAgentSignupsOutput, error) {
	p, err := s.requireAccountScope(ctx)
	if err != nil {
		return nil, err
	}
	after, afterID, err := s.decodeKeyset(p.User.ID, cursorAgentSignups, in.Cursor)
	if err != nil {
		return nil, err
	}
	limit := effectiveLimit(in.Limit)
	items, err := s.deps.ListPendingAgentSignups(ctx, identity.NormalizeEmail(p.User.Email), limit+1, after, afterID)
	if err != nil {
		return nil, signupError(err)
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	views := make([]AgentSignupView, 0, len(items))
	for i := range items {
		views = append(views, signupView(&items[i]))
	}
	next := ""
	if hasMore {
		last := items[len(items)-1]
		next, err = s.encodeKeyset(p.User.ID, cursorAgentSignups, last.CreatedAt, last.ID)
		if err != nil {
			return nil, err
		}
	}
	return &listPendingAgentSignupsOutput{Body: NewPage(views, next)}, nil
}

func (s *Server) handleApproveAgentSignup(ctx context.Context, in *approveAgentSignupInput) (*agentSignupOutput, error) {
	p, err := s.requireAccountScope(ctx)
	if err != nil {
		return nil, err
	}
	v, err := s.deps.ApproveAgentSignup(ctx, in.ID, identity.NormalizeEmail(p.User.Email), p.User.ID, in.Body.ReviewOutbound)
	if err != nil {
		return nil, signupError(err)
	}
	return &agentSignupOutput{Body: signupView(v)}, nil
}

func (s *Server) handleRejectAgentSignup(ctx context.Context, in *rejectAgentSignupInput) (*rejectAgentSignupOutput, error) {
	p, err := s.requireAccountScope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.deps.RejectAgentSignup(ctx, in.ID, identity.NormalizeEmail(p.User.Email)); err != nil {
		return nil, signupError(err)
	}
	out := &rejectAgentSignupOutput{}
	out.Body.ID, out.Body.Status = in.ID, identity.AgentSignupRejected
	return out, nil
}
