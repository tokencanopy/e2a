package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
)

// Slice 5a — the hard scope ceiling. This exercises the 403 matrix over the
// real v1 handlers: account-only routes reject agent-scoped credentials;
// per-agent routes pin an agent-scoped credential to its bound agent; an
// account-scoped credential passes everywhere it owns.

func scopeTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	u := &identity.User{ID: "u_1", Email: "owner@acme.com"}
	deps := Deps{
		// Scope-aware auth keyed by bearer:
		//   acct        → account scope
		//   agtSupport  → agent scope bound to support@acme.com
		//   agtOther    → agent scope bound to other@acme.com
		PrincipalAuthenticator: func(r *http.Request) (*identity.Principal, error) {
			switch r.Header.Get("Authorization") {
			case "Bearer acct":
				return &identity.Principal{User: u, Scope: identity.ScopeAccount}, nil
			case "Bearer agtSupport":
				return &identity.Principal{User: u, Scope: identity.ScopeAgent, AgentID: "support@acme.com"}, nil
			case "Bearer agtOther":
				return &identity.Principal{User: u, Scope: identity.ScopeAgent, AgentID: "other@acme.com"}, nil
			}
			return nil, errors.New("unauthorized")
		},
		ListAgents: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.AgentIdentity, error) {
			return []identity.AgentIdentity{sampleAgent()}, nil
		},
		GetAgent: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			switch address {
			case "support@acme.com":
				a := sampleAgent()
				return &a, nil
			case "other@acme.com":
				a := sampleAgent()
				a.ID = "other@acme.com"
				return &a, nil
			}
			return nil, errors.New("not found")
		},
		GetAgentAnyState: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			switch address {
			case "support@acme.com":
				a := sampleAgent()
				return &a, nil
			case "other@acme.com":
				a := sampleAgent()
				a.ID = "other@acme.com"
				return &a, nil
			}
			return nil, errors.New("not found")
		},
		UpdateAgentName: func(ctx context.Context, agentID, userID, name string) (*identity.AgentIdentity, error) {
			a := sampleAgent()
			a.ID, a.Name = agentID, name
			return &a, nil
		},
		UpdateAgentProtection: func(ctx context.Context, agentID, userID string, cfg identity.ProtectionConfig) (*identity.AgentIdentity, error) {
			a := sampleAgent()
			a.ID = agentID
			return &a, nil
		},
		DeleteAgent: func(ctx context.Context, agentID, userID string) error { return nil },
		Legacy:      http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }),
	}
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)
	return srv
}

func TestScope_AccountOnlyRoutesRejectAgentKeys(t *testing.T) {
	srv := scopeTestServer(t)

	cases := []struct {
		name, method, path, bearer string
		wantStatus                 int
	}{
		// List agents is account-admin discovery.
		{"list/account-ok", "GET", "/v1/agents", "acct", 200},
		{"list/agent-403", "GET", "/v1/agents", "agtSupport", 403},
		// Delete agent is admin even on the bound agent.
		{"delete/account-ok", "DELETE", "/v1/agents/support%40acme.com?confirm=DELETE", "acct", 200},
		// confirm=DELETE clears Huma's required-param validation so the request
		// reaches the scope gate (403), like the empty JSON body does for POSTs.
		{"delete/agent-bound-403", "DELETE", "/v1/agents/support%40acme.com?confirm=DELETE", "agtSupport", 403},
		// Protection config is account-only — the #13 fix: the screened agent
		// cannot even READ its own detection tuning, let alone change it.
		{"protection-get/account-ok", "GET", "/v1/agents/support%40acme.com/protection", "acct", 200},
		{"protection-get/agent-bound-403", "GET", "/v1/agents/support%40acme.com/protection", "agtSupport", 403},
		// The review queue is an operator surface — agents can neither see nor
		// resolve holds (of either direction). 403 fires at the scope gate before
		// any dep, so these hold even with the reviews deps unwired.
		{"reviews-list/agent-403", "GET", "/v1/reviews", "agtSupport", 403},
		{"reviews-get/agent-403", "GET", "/v1/reviews/msg_1", "agtSupport", 403},
		{"reviews-approve/agent-403", "POST", "/v1/reviews/msg_1/approve", "agtSupport", 403},
		{"reviews-reject/agent-403", "POST", "/v1/reviews/msg_1/reject", "agtSupport", 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// POST routes need a valid (empty) JSON object so the request clears
			// Huma body validation and reaches the scope gate.
			var reqBody any
			if c.method == "POST" {
				reqBody = map[string]any{}
			}
			code, body := sendJSON(t, c.method, srv.URL+c.path, c.bearer, reqBody)
			if code != c.wantStatus {
				t.Fatalf("%s %s as %s: status %d, want %d (body %v)", c.method, c.path, c.bearer, code, c.wantStatus, body)
			}
			if c.wantStatus == 403 && errCode(body) != "forbidden" {
				t.Errorf("expected error code 'forbidden', got %v", body)
			}
		})
	}
}

// TestScope_UpdateAgentIsAccountOnly: mutating agent config is barred for an
// agent-scoped credential even on its own bound agent.
func TestScope_UpdateAgentIsAccountOnly(t *testing.T) {
	srv := scopeTestServer(t)
	body := map[string]any{"name": "Renamed"}

	code, _ := sendJSON(t, "PATCH", srv.URL+"/v1/agents/support%40acme.com", "acct", body)
	if code != 200 {
		t.Errorf("account key PATCH agent: status %d, want 200", code)
	}
	code, resp := sendJSON(t, "PATCH", srv.URL+"/v1/agents/support%40acme.com", "agtSupport", body)
	if code != 403 || errCode(resp) != "forbidden" {
		t.Errorf("agent key PATCH own agent: status %d body %v, want 403 forbidden", code, resp)
	}
}

// TestScope_ProtectionPutIsAccountOnly: writing protection config is barred for
// an agent-scoped credential even on its own bound agent (#13). A valid body is
// sent so the request clears Huma schema validation and reaches the scope gate.
func TestScope_ProtectionPutIsAccountOnly(t *testing.T) {
	srv := scopeTestServer(t)
	body := map[string]any{
		"inbound":  map[string]any{"gate": map[string]any{}, "scan": map[string]any{}},
		"outbound": map[string]any{"gate": map[string]any{}, "scan": map[string]any{}},
		"holds":    map[string]any{},
	}
	code, _ := sendJSON(t, "PUT", srv.URL+"/v1/agents/support%40acme.com/protection", "acct", body)
	if code != 200 {
		t.Errorf("account key PUT protection: status %d, want 200", code)
	}
	code, resp := sendJSON(t, "PUT", srv.URL+"/v1/agents/support%40acme.com/protection", "agtSupport", body)
	if code != 403 || errCode(resp) != "forbidden" {
		t.Errorf("agent key PUT own protection: status %d body %v, want 403 forbidden", code, resp)
	}
}

// Note: that HITL approve/reject is barred for an agent-scoped credential
// (self-approval would defeat the human-in-the-loop gate) is covered by the
// reviews-approve/reject agent-403 cases in the table above
// (/v1/reviews/{id}/approve|reject). The deprecated agent-path approve/reject
// endpoints were removed in the pre-GA vocabulary freeze.

// TestScope_AgentKeyPinnedToBoundAgent: a per-agent runtime route lets an
// agent-scoped credential act as its bound agent but 403s on any other agent;
// an account-scoped credential reaches both.
func TestScope_AgentKeyPinnedToBoundAgent(t *testing.T) {
	srv := scopeTestServer(t)

	cases := []struct {
		name, path, bearer string
		wantStatus         int
	}{
		{"bound-agent-ok", "/v1/agents/support%40acme.com", "agtSupport", 200},
		{"other-agent-403", "/v1/agents/other%40acme.com", "agtSupport", 403},
		{"account-reaches-support", "/v1/agents/support%40acme.com", "acct", 200},
		{"account-reaches-other", "/v1/agents/other%40acme.com", "acct", 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := sendJSON(t, "GET", srv.URL+c.path, c.bearer, nil)
			if code != c.wantStatus {
				t.Fatalf("GET %s as %s: status %d, want %d (body %v)", c.path, c.bearer, code, c.wantStatus, body)
			}
		})
	}
}

// TestScope_LegacyAuthenticatorIsAccount: with only the legacy Authenticator
// wired (no PrincipalAuthenticator), every caller is treated as account-scoped
// — the pre-Slice-5a behavior, so the ceiling never falsely 403s old deployments.
func TestScope_LegacyAuthenticatorIsAccount(t *testing.T) {
	deps := Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			if r.Header.Get("Authorization") == "Bearer good" {
				return &identity.User{ID: "u_1"}, nil
			}
			return nil, errors.New("unauthorized")
		},
		ListAgents: func(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]identity.AgentIdentity, error) {
			return []identity.AgentIdentity{sampleAgent()}, nil
		},
		Legacy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }),
	}
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)

	code, body := sendJSON(t, "GET", srv.URL+"/v1/agents", "good", nil)
	if code != 200 {
		t.Fatalf("legacy authenticator on account route: status %d, want 200 (body %v)", code, body)
	}
}
