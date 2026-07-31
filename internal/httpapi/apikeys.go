package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
)

// APIKeyView is the non-secret metadata for an API key (list + create). The
// plaintext secret is never in this shape — it appears once, in
// CreateAPIKeyResponse.Key.
type APIKeyView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix" doc:"Non-secret visible prefix (e.g. e2a_acct_… / e2a_agt_…)."`
	// Scope is an OPEN set on this response view (evolving vocabulary); the
	// REQUEST-side CreateAPIKeyRequest.scope keeps its closed enum, which is
	// where validation belongs.
	Scope      string     `json:"scope" doc:"account = workspace admin; agent = bound to one inbox. Open set: new values may be added over time, so treat these as strings and tolerate unknown values. Known values: account, agent."`
	Agent      string     `json:"agent_email,omitempty" doc:"Bound inbox email for agent-scoped keys; omitted for account scope."`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// CreateAPIKeyResponse is APIKeyView plus the one-time plaintext key — shown
// once at creation and never retrievable again.
type CreateAPIKeyResponse struct {
	APIKeyView
	Key string `json:"key" doc:"The secret key. Shown once; store it now — it cannot be retrieved later."`
}

func apiKeyView(k identity.APIKey) APIKeyView {
	agent := ""
	if k.AgentID != nil {
		agent = *k.AgentID
	}
	return APIKeyView{
		ID:         k.ID,
		Name:       k.Name,
		KeyPrefix:  k.KeyPrefix,
		Scope:      k.Scope,
		Agent:      agent,
		CreatedAt:  k.CreatedAt,
		LastUsedAt: k.LastUsedAt,
		ExpiresAt:  k.ExpiresAt,
	}
}

type listAPIKeysOutput struct{ Body Page[APIKeyView] }

// maxAPIKeyNameLen bounds the API-key display name (a human label, not an
// identifier) — same budget as the agent display name. Enforced declaratively
// via the maxLength struct tag (Huma validates in Unicode code points);
// TestGABoundTagsMatchConsts guards tag/const drift. Aliased to the canonical
// identity.MaxAPIKeyNameLen so this and the legacy dashboard POST /api/keys
// handler (internal/auth) share one source of truth.
const maxAPIKeyNameLen = identity.MaxAPIKeyNameLen

// CreateAPIKeyRequest mirrors the legacy /api/keys body. scope defaults to
// account; scope=agent requires `agent_email` (an owned inbox email).
type CreateAPIKeyRequest struct {
	Name      string `json:"name,omitempty" maxLength:"200" doc:"Human label for the key. At most 200 characters (Unicode code points)."`
	ExpiresAt string `json:"expires_at,omitempty" format:"date-time" doc:"Optional hard expiry (RFC 3339, must be in the future). Omit for a never-expiring key."`
	Scope     string `json:"scope,omitempty" enum:"account,agent" doc:"account = workspace admin (default); agent = bound to a single inbox."`
	Agent     string `json:"agent_email,omitempty" doc:"Inbox email to bind the key to; required when scope=agent."`
}

type createAPIKeyInput struct {
	RawBody        []byte
	IdempotencyKey string `header:"Idempotency-Key" doc:"Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request's response — the SAME key — instead of minting a second live credential. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may mint a new key rather than replay."`
	Body           CreateAPIKeyRequest
}
type createAPIKeyOutput struct{ Body CreateAPIKeyResponse }

type deleteAPIKeyInput struct {
	ID string `path:"id"`
	DeleteConfirm
}
type deleteAPIKeyOutput struct{ Body DeleteApiKeyResult }

func (s *Server) registerAPIKeys() {
	registerOp(s.API, huma.Operation{
		OperationID: "listApiKeys", Method: http.MethodGet, Path: "/v1/account/api-keys",
		Summary: "List API keys", Tags: []string{"account"},
		Description: "API keys for the account (metadata only — secrets are shown once, at creation). Account scope only: an agent-scoped credential cannot manage keys.",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleListAPIKeys)

	registerOp(s.API, huma.Operation{
		OperationID: "createApiKey", Method: http.MethodPost, Path: "/v1/account/api-keys",
		Summary: "Create an API key", Tags: []string{"account"},
		Description: "Mint a new API key; the plaintext key is returned once. scope=account is workspace admin (agent/domain/key management); scope=agent binds the key to one inbox so it can act only as that agent. Account scope only.",
		Security:    []map[string][]string{{"bearer": {}}}, DefaultStatus: http.StatusCreated,
		Responses: map[string]*huma.Response{
			"409":     s.idempotencyInFlightResponse(),
			"422":     s.idempotencyReuseResponse(),
			"default": s.errorEnvelopeResponse(),
		},
	}, s.handleCreateAPIKey)

	registerOp(s.API, huma.Operation{
		OperationID: "deleteApiKey", Method: http.MethodDelete, Path: "/v1/account/api-keys/{id}",
		Summary: "Revoke an API key", Tags: []string{"account"},
		Description: "Revoke a key by id. Integrations using it stop authenticating immediately. Account scope only. Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}).",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleDeleteAPIKey)
}

// listAPIKeysInput carries the standard cursor/limit (PageParams).
type listAPIKeysInput struct {
	PageParams
}

func (s *Server) handleListAPIKeys(ctx context.Context, in *listAPIKeysInput) (*listAPIKeysOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.ListAPIKeys == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "API keys are not available on this deployment")
	}
	afterCreatedAt, afterID, err := s.decodeKeyset(in.Cursor)
	if err != nil {
		return nil, err
	}
	limit := effectiveLimit(in.Limit)
	// Fetch limit+1 to detect a further page.
	keys, err := s.deps.ListAPIKeys(ctx, user.ID, limit+1, afterCreatedAt, afterID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list API keys")
	}
	hasMore := len(keys) > limit
	if hasMore {
		keys = keys[:limit]
	}
	views := make([]APIKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, apiKeyView(k))
	}
	var nextCursor string
	if hasMore {
		last := keys[len(keys)-1]
		if nextCursor, err = s.encodeKeyset(last.CreatedAt, last.ID); err != nil {
			return nil, err
		}
	}
	return &listAPIKeysOutput{Body: NewPage(views, nextCursor)}, nil
}

func (s *Server) handleCreateAPIKey(ctx context.Context, in *createAPIKeyInput) (*createAPIKeyOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.CreateScopedAPIKey == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "API keys are not available on this deployment")
	}

	// Optional expiry: RFC 3339, must be in the future. Empty = never expires.
	var expiresAt *time.Time
	if in.Body.ExpiresAt != "" {
		t, perr := time.Parse(time.RFC3339, in.Body.ExpiresAt)
		if perr != nil {
			return nil, NewError(http.StatusBadRequest, "invalid_expires_at", "expires_at must be an RFC 3339 timestamp")
		}
		if !t.After(time.Now()) {
			return nil, NewError(http.StatusBadRequest, "invalid_expires_at", "expires_at must be in the future")
		}
		expiresAt = &t
	}

	scope := in.Body.Scope
	if scope == "" {
		scope = identity.ScopeAccount
	}
	if !identity.ValidScope(scope) {
		return nil, NewError(http.StatusBadRequest, "invalid_scope", "scope must be 'account' or 'agent'")
	}

	// For an agent-scoped key, resolve the named inbox to its id (ownership
	// re-checked by resolveOwnedAgent) so a wrong/foreign agent is rejected
	// rather than minting an over-broad or cross-tenant key.
	var agentID string
	if scope == identity.ScopeAgent {
		if in.Body.Agent == "" {
			return nil, NewError(http.StatusBadRequest, "invalid_request", "agent_email (inbox email) is required when scope=agent")
		}
		ag, aerr := s.resolveOwnedAgent(ctx, in.Body.Agent)
		if aerr != nil {
			return nil, aerr
		}
		agentID = ag.ID
	}

	// Wrap the mint in the keyed-idempotency guard (same machinery as
	// send/reply/rotate-secret): a network-retried create carrying the same
	// Idempotency-Key + byte-identical body replays the first response — the
	// SAME key — instead of silently minting a SECOND live credential. Without a
	// key the mint runs unguarded (idempotency is opt-in). See issue #493.
	_, body, err := runIdempotent(s, ctx, user.ID, in.IdempotencyKey,
		"/v1/account/api-keys", in.RawBody,
		func() (int, CreateAPIKeyResponse, error) {
			key, kerr := s.deps.CreateScopedAPIKey(ctx, user.ID, in.Body.Name, scope, agentID, expiresAt)
			if kerr != nil {
				return 0, CreateAPIKeyResponse{}, NewError(http.StatusInternalServerError, "internal_error", "failed to create API key")
			}
			return http.StatusCreated, CreateAPIKeyResponse{
				APIKeyView: apiKeyView(*key),
				Key:        key.PlaintextKey,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return &createAPIKeyOutput{Body: body}, nil
}

func (s *Server) handleDeleteAPIKey(ctx context.Context, in *deleteAPIKeyInput) (*deleteAPIKeyOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.DeleteAPIKey == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "API keys are not available on this deployment")
	}
	if err := s.deps.DeleteAPIKey(ctx, in.ID, user.ID); err != nil {
		if errors.Is(err, identity.ErrAPIKeyNotFound) {
			return nil, NewError(http.StatusNotFound, "not_found", "API key not found")
		}
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to revoke API key")
	}
	return &deleteAPIKeyOutput{Body: DeleteApiKeyResult{Deleted: true, ID: in.ID}}, nil
}
