package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tokencanopy/e2a/internal/agentauth"
	"github.com/tokencanopy/e2a/internal/identity"
)

// auth.md autonomous agent-identity path (Slice 5b-2). Two endpoints implement
// the token rails on top of the 5b-1 signer:
//
//   POST /agent/identity         — bootstrap: an agent presents its e2a_agt_ key
//                                  and receives a long-lived identity_assertion.
//   POST /oauth2/token           — grant_type=jwt-bearer: present the assertion,
//   (grant_type=jwt-bearer)        receive a short-lived access_token.
//
// e2a-specific adaptation of auth.md: agentdrive's "anonymous" path lets anyone
// self-provision an identity, which e2a cannot allow — its identities are
// emails on verified domains. So the bootstrap credential is the agent-scoped
// API key from Slice 5a (already gated by domain ownership at mint time), not
// an ownerless self-registration.

// jwtBearerGrantType is the RFC 7523 grant URN auth.md uses for the autonomous
// path. fosite doesn't ship it, so the token endpoint dispatches it here before
// falling through to fosite's authorization_code/refresh_token handling.
const jwtBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// agentAuthReady reports whether the agent-identity surface is usable: a signing
// key AND a public URL (needed for iss/aud) must both be configured.
func (a *API) agentAuthReady() bool {
	return a.signer != nil && a.signer.Enabled() && a.publicURL != ""
}

type identityAssertionResponse struct {
	IdentityAssertion string    `json:"identity_assertion"`
	TokenType         string    `json:"token_type"`
	Subject           string    `json:"sub"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// handleAgentIdentity is POST /agent/identity. In Slice 5b-2 it serves the
// bootstrap: authenticate the caller's agent-scoped credential and mint an
// identity_assertion for that one agent. (anonymous/ID-JAG registration types
// are later sub-slices; an unknown/auth-less request is rejected, not
// silently self-provisioned.)
func (a *API) handleAgentIdentity(w http.ResponseWriter, r *http.Request) {
	if !a.agentAuthReady() {
		writeOAuthError(w, r, http.StatusNotImplemented, "not_implemented",
			"agent identity is not enabled on this deployment")
		return
	}
	// Bootstrap auth: the agent presents its e2a_agt_ key (Slice 5a). The
	// credential must be agent-scoped and bound to a specific agent — an
	// account-scoped key has no single agent to assert for.
	p, err := a.authenticatePrincipal(r)
	if err != nil {
		a.writeAuthError(w, r, err)
		return
	}
	if p.Scope != identity.ScopeAgent || p.AgentID == "" {
		writeOAuthError(w, r, http.StatusForbidden, "forbidden",
			"agent identity requires an agent-scoped credential bound to one agent")
		return
	}

	// Load the agent for its current assertion_version (the kill-switch
	// counter stamped into the token).
	ag, err := a.store.GetAgentByID(r.Context(), p.AgentID)
	if err != nil || ag == nil || ag.UserID != p.User.ID {
		writeOAuthError(w, r, http.StatusForbidden, "forbidden", "agent not found")
		return
	}

	assertion, exp, err := a.signer.SignIdentityAssertion(ag.ID, identity.ScopeAgent, ag.AssertionVersion, a.oauthIssuer())
	if err != nil {
		writeOAuthError(w, r, http.StatusInternalServerError, "server_error", "failed to mint identity assertion")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(identityAssertionResponse{
		IdentityAssertion: assertion,
		TokenType:         "N_A", // an assertion is presented at /oauth2/token, not used as a bearer
		Subject:           ag.ID,
		ExpiresAt:         exp.UTC(),
	})
}

type accessTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

// handleJWTBearerGrant implements grant_type=jwt-bearer at /oauth2/token: verify
// the presented identity_assertion, re-check its assertion_version against the
// live agent row (the kill switch), and mint a short-lived access_token.
// Returns RFC 6749 §5.2 error bodies.
func (a *API) handleJWTBearerGrant(w http.ResponseWriter, r *http.Request) {
	if !a.agentAuthReady() {
		writeOAuthError(w, r, http.StatusNotImplemented, "unsupported_grant_type",
			"agent identity is not enabled on this deployment")
		return
	}
	assertion := strings.TrimSpace(r.PostForm.Get("assertion"))
	if assertion == "" {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_request", "assertion is required")
		return
	}
	claims, err := a.signer.VerifyToken(assertion, agentauth.TypIdentityAssertion, a.oauthIssuer())
	if err != nil {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "identity assertion is invalid or expired")
		return
	}
	// Freshness / kill switch: the assertion's assertion_version must match the
	// live agent row. A bump (revocation) makes every prior assertion stale.
	ag, err := a.store.GetAgentByID(r.Context(), claims.Subject)
	if err != nil || ag == nil {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "agent not found")
		return
	}
	if ag.AssertionVersion != claims.AssertionVersion {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_grant", "assertion is stale; re-acquire an identity assertion")
		return
	}

	token, exp, err := a.signer.SignAccessToken(ag.ID, identity.ScopeAgent, ag.AssertionVersion, a.oauthIssuer())
	if err != nil {
		writeOAuthError(w, r, http.StatusInternalServerError, "server_error", "failed to mint access token")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(accessTokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int(time.Until(exp).Seconds()),
		Scope:       identity.ScopeAgent,
	})
}

// resolveAgentAccessToken verifies an e2a-minted access_token JWT and resolves
// it to an agent-scoped principal. Returns (nil, false, nil) when the bearer is
// not one of our JWTs (so the caller can fall through to API-key/OAuth paths);
// returns an error only when it IS our JWT but fails verification or lookup.
func (a *API) resolveAgentAccessToken(r *http.Request, bearer string) (*identity.Principal, bool, error) {
	if !a.agentAuthReady() || !looksLikeJWT(bearer) {
		return nil, false, nil
	}
	claims, err := a.signer.VerifyToken(bearer, agentauth.TypAccessToken, a.oauthIssuer())
	if err != nil {
		// It parses as a JWT but isn't a valid e2a access token — reject
		// rather than fall through (a tampered/expired token is a 401, not an
		// API-key probe).
		return nil, true, errors.New("invalid agent access token")
	}
	ag, err := a.store.GetAgentByID(r.Context(), claims.Subject)
	if err != nil || ag == nil {
		return nil, true, errors.New("agent not found for access token")
	}
	// Kill switch, re-checked per request (the agent row is already loaded, so
	// this is free): a bumped assertion_version invalidates outstanding access
	// tokens immediately rather than only starving new mints — revocation is
	// instant, not bounded by the 15-min token TTL.
	if ag.AssertionVersion != claims.AssertionVersion {
		return nil, true, errors.New("access token revoked (stale assertion_version)")
	}
	user, err := a.store.GetUserByID(r.Context(), ag.UserID)
	if err != nil || user == nil {
		return nil, true, errors.New("owner not found for access token")
	}
	return &identity.Principal{User: user, Scope: identity.ScopeAgent, AgentID: ag.ID}, true, nil
}

// looksLikeJWT is a cheap pre-filter: a compact JWS is three base64url segments
// separated by dots and (for our RS256 tokens) starts with the "eyJ" header.
// Avoids running full verification on API keys / opaque OAuth tokens.
func looksLikeJWT(s string) bool {
	return strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") == 2
}
