package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
)

// coveringParentDep wires the covering-parent lookup used by the subdomain
// create tests: only acme.team.mnexa.ai resolves, to the verified parent.
func coveringParentDep(d *Deps) {
	d.LookupCoveringDomain = func(ctx context.Context, sub, userID string) (*identity.Domain, error) {
		if sub == "acme.team.mnexa.ai" {
			return &identity.Domain{Domain: "team.mnexa.ai", Verified: true}, nil
		}
		return nil, errors.New("no covering domain")
	}
	d.ResolveMX = func(ctx context.Context, name string) ([]string, error) {
		return []string{"mx.e2a.dev."}, nil
	}
}

func sendJSON(t *testing.T, method, url, bearer string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestUpdateAgentName exercises the post-reshape agent PATCH: the only mutable
// field is the display name (screening config moved to /protection).
func TestUpdateAgentName(t *testing.T) {
	srv := testServer(t)
	code, body := sendJSON(t, "PATCH", srv.URL+"/v1/agents/support%40acme.com", "good", map[string]any{
		"name": "Renamed Support",
	})
	if code != 200 {
		t.Fatalf("status %d body %v", code, body)
	}
	// Returns the reloaded agent.
	if body["email"] != "support@acme.com" {
		t.Fatalf("expected reloaded agent, got %v", body)
	}
}

func TestUpdateAgentNoFields(t *testing.T) {
	srv := testServer(t)
	code, body := sendJSON(t, "PATCH", srv.URL+"/v1/agents/support%40acme.com", "good", map[string]any{})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request, got %d %v", code, body)
	}
}

func TestUpdateAgentNotOwned(t *testing.T) {
	srv := testServer(t)
	code, _ := sendJSON(t, "PATCH", srv.URL+"/v1/agents/other%40acme.com", "good", map[string]any{
		"name": "x",
	})
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}

func TestDeleteAgent(t *testing.T) {
	srv := testServer(t)
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/agents/support%40acme.com?confirm=DELETE", "good", nil)
	if code != 200 {
		t.Fatalf("want 200, got %d %v", code, body)
	}
	// Uniform deletion object: {deleted:true, email, messages_deleted}.
	if body["deleted"] != true {
		t.Fatalf("want deleted:true, got %v", body)
	}
	if body["email"] != "support@acme.com" {
		t.Fatalf("want email echo, got %v", body)
	}
	// Soft delete preserves messages in trash, so no rows are removed.
	if body["messages_deleted"] != float64(0) {
		t.Fatalf("want messages_deleted:0, got %v", body)
	}
}

func TestDeleteAgentNotOwned(t *testing.T) {
	srv := testServer(t)
	code, _ := sendJSON(t, "DELETE", srv.URL+"/v1/agents/other%40acme.com?confirm=DELETE", "good", nil)
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}

func postJSON(t *testing.T, url, bearer string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errCode(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	return ""
}

func TestCreateAgentHappyPath(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "bot@acme.com", "name": "Bot",
	})
	if code != 201 {
		t.Fatalf("status %d body %v", code, body)
	}
	if body["email"] != "bot@acme.com" || body["domain"] != "acme.com" {
		t.Fatalf("unexpected create response: %v", body)
	}
	if _, hasID := body["id"]; hasID {
		t.Fatalf("AgentView must not carry a redundant id (email is the identity): %v", body)
	}
}

func TestCreateAgentUnverifiedDomain(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "bot@pending.com",
	})
	if code != 400 || errCode(body) != "domain_not_verified" {
		t.Fatalf("want 400 domain_not_verified, got %d %v", code, body)
	}
}

func TestCreateAgentUnregisteredDomain(t *testing.T) {
	srv := testServer(t)
	// The security-critical guard: an agent cannot be created on a domain
	// the caller has not registered + verified.
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "bot@someone-elses.com",
	})
	if code != 400 || errCode(body) != "domain_not_registered" {
		t.Fatalf("want 400 domain_not_registered, got %d %v", code, body)
	}
}

// TestCreateAgentSubdomainCoveredByVerifiedParent: an agent on a subdomain of a
// verified parent domain keeps its exact address domain in the public contract
// while exposing the registered parent that drives DKIM, sending status, and
// quota. No exact registration for the subdomain is required.
func TestCreateAgentSubdomainCoveredByVerifiedParent(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.LookupCoveringDomain = func(ctx context.Context, sub, userID string) (*identity.Domain, error) {
			if sub == "acme.team.mnexa.ai" {
				return &identity.Domain{Domain: "team.mnexa.ai", Verified: true}, nil
			}
			return nil, errors.New("no covering domain")
		}
		d.ResolveMX = func(ctx context.Context, name string) ([]string, error) {
			return []string{"mx.e2a.dev."}, nil
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "otto@acme.team.mnexa.ai",
	})
	if code != 201 {
		t.Fatalf("status %d body %v", code, body)
	}
	if body["email"] != "otto@acme.team.mnexa.ai" {
		t.Fatalf("email = %v, want the full subdomain address", body["email"])
	}
	if body["domain"] != "acme.team.mnexa.ai" {
		t.Fatalf("domain must remain the exact address domain, got %v", body["domain"])
	}
	if body["registered_domain"] != "team.mnexa.ai" {
		t.Fatalf("registered_domain must identify the verified parent, got %v", body["registered_domain"])
	}
}

// TestCreateAgentSubdomainNoCoveringParent: even with the covering lookup wired,
// a subdomain that no verified parent covers is still rejected — the ownership
// guard is not weakened.
func TestCreateAgentSubdomainNoCoveringParent(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.LookupCoveringDomain = func(ctx context.Context, sub, userID string) (*identity.Domain, error) {
			return nil, errors.New("no covering domain")
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "bot@uncovered.example.org",
	})
	if code != 400 || errCode(body) != "domain_not_registered" {
		t.Fatalf("want 400 domain_not_registered, got %d %v", code, body)
	}
}

// TestCreateAgentSubdomainLabelBoundaryRejected: the handler surfaces the
// store's label-boundary rejection (evilteam.mnexa.ai is NOT a child of the
// registered team.mnexa.ai) as domain_not_registered. The load-bearing
// label-matching security proof lives in identity.TestLookupCoveringDomain_
// LabelBoundaryRejection; this pins the handler mapping.
func TestCreateAgentSubdomainLabelBoundaryRejected(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.LookupCoveringDomain = func(ctx context.Context, sub, userID string) (*identity.Domain, error) {
			if sub == "acme.team.mnexa.ai" {
				return &identity.Domain{Domain: "team.mnexa.ai", Verified: true}, nil
			}
			// evilteam.mnexa.ai shares a string suffix but is not a label child.
			return nil, errors.New("no covering domain")
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "otto@evilteam.mnexa.ai",
	})
	if code != 400 || errCode(body) != "domain_not_registered" {
		t.Fatalf("SECURITY: evilteam.mnexa.ai must be rejected, got %d %v", code, body)
	}
}

// TestCreateAgentExactUnverifiedNotMaskedByParent pins resolution precedence
// (trap #3): an EXACT registered-but-unverified row wins over a verified parent
// — the parent fallback must not mask it. The user must verify the exact domain,
// not silently inherit the parent's identity.
func TestCreateAgentExactUnverifiedNotMaskedByParent(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.LookupDomain = func(ctx context.Context, domain, userID string) (*identity.Domain, error) {
			if domain == "sub.mnexa.ai" {
				return &identity.Domain{Domain: domain, Verified: false}, nil // exact row, unverified
			}
			return nil, errors.New("not registered")
		}
		d.LookupCoveringDomain = func(ctx context.Context, sub, userID string) (*identity.Domain, error) {
			// A verified parent EXISTS — precedence must still reject on the
			// exact unverified row rather than fall through to this.
			return &identity.Domain{Domain: "mnexa.ai", Verified: true}, nil
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "bot@sub.mnexa.ai",
	})
	if code != 400 || errCode(body) != "domain_not_verified" {
		t.Fatalf("exact unverified row must yield domain_not_verified (not masked by parent), got %d %v", code, body)
	}
}

func TestCreateAgentUnverifiedIntermediateNotMaskedByGrandparent(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.LookupCoveringDomain = func(ctx context.Context, sub, userID string) (*identity.Domain, error) {
			return &identity.Domain{Domain: "team.mnexa.ai", Verified: false}, nil
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "bot@acme.team.mnexa.ai",
	})
	if code != 400 || errCode(body) != "domain_not_verified" {
		t.Fatalf("pending intermediate must be authoritative, got %d %v", code, body)
	}
}

// Two-way inbox invariant: a subdomain authorized by its parent is not created
// until its exact address domain routes inbound mail to the relay.
func TestCreateAgentSubdomainBlocksWhenNoMXCoverage(t *testing.T) {
	srv := testServer(t, coveringParentDep, func(d *Deps) {
		d.ResolveMX = func(ctx context.Context, name string) ([]string, error) {
			return []string{"aspmx.someone-else.example."}, nil
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "otto@acme.team.mnexa.ai",
	})
	if code != 400 || errCode(body) != "inbound_mx_missing" {
		t.Fatalf("want 400 inbound_mx_missing, got %d %v", code, body)
	}
}

func TestCreateAgentSubdomainNXDOMAINIsMissingMX(t *testing.T) {
	srv := testServer(t, coveringParentDep, func(d *Deps) {
		d.ResolveMX = func(ctx context.Context, name string) ([]string, error) {
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "otto@acme.team.mnexa.ai",
	})
	if code != 400 || errCode(body) != "inbound_mx_missing" {
		t.Fatalf("want 400 inbound_mx_missing, got %d %v", code, body)
	}
}

// TestCreateAgentSubdomainAllowsMatchingMX verifies that an explicit MX on the
// subdomain or a wildcard synthesized by the resolver satisfies the gate. It
// also exercises trailing-dot and case-insensitive host matching.
func TestCreateAgentSubdomainAllowsMatchingMX(t *testing.T) {
	srv := testServer(t, coveringParentDep, func(d *Deps) {
		d.ResolveMX = func(ctx context.Context, name string) ([]string, error) {
			return []string{"MX.E2A.DEV."}, nil // matches the fixture SMTPDomain mx.e2a.dev
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "otto@acme.team.mnexa.ai",
	})
	if code != 201 {
		t.Fatalf("status %d body %v", code, body)
	}
	if _, has := body["warnings"]; has {
		t.Fatalf("create response must remain the shared AgentView shape: %v", body)
	}
}

func TestCreateAgentSubdomainMXLookupFailureIsRetryable(t *testing.T) {
	srv := testServer(t, coveringParentDep, func(d *Deps) {
		d.ResolveMX = func(ctx context.Context, name string) ([]string, error) {
			return nil, errors.New("resolver unavailable")
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "otto@acme.team.mnexa.ai",
	})
	if code != 503 || errCode(body) != "inbound_mx_check_failed" {
		t.Fatalf("want 503 inbound_mx_check_failed, got %d %v", code, body)
	}
}

func TestCreateAgentReservedMailFromSubtreeRejected(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.LookupCoveringDomain = func(ctx context.Context, sub, userID string) (*identity.Domain, error) {
			return &identity.Domain{Domain: "team.mnexa.ai", Verified: true}, nil
		}
	})
	for _, address := range []string{
		"bot@bounce.team.mnexa.ai",
		"bot@tenant.bounce.team.mnexa.ai",
	} {
		code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{"email": address})
		if code != 400 || errCode(body) != "reserved_domain" {
			t.Errorf("%s: want 400 reserved_domain, got %d %v", address, code, body)
		}
	}
}

// TestCreateAgentExactDomainNoMXProbe: an exact-domain agent (not parent-resolved)
// is never probed, even with a resolver wired that would report no coverage. The
// required MX gate is scoped to inherited subdomain agents only.
func TestCreateAgentExactDomainNoMXProbe(t *testing.T) {
	probed := false
	srv := testServer(t, func(d *Deps) {
		d.ResolveMX = func(ctx context.Context, name string) ([]string, error) {
			probed = true
			return nil, errors.New("no mx")
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "bot@acme.com", // acme.com is an exact verified domain in the fixture
	})
	if code != 201 {
		t.Fatalf("status %d body %v", code, body)
	}
	if probed {
		t.Fatalf("exact-domain create must not run the subdomain MX probe")
	}
	if _, has := body["warnings"]; has {
		t.Fatalf("exact-domain create must not carry warnings, got %v", body["warnings"])
	}
}

// TestCreateAgentMalformedSubdomainRejected (QA F2/F3): malformed address
// domains — empty, leading/trailing-dot, or consecutive-dot labels — are
// rejected at the create boundary BEFORE covering resolution, so they can never
// mint a junk agent under a parent. A covering fake that WOULD resolve is wired
// to prove the malformed check short-circuits it.
func TestCreateAgentMalformedSubdomainRejected(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		// Would cover anything if reached — the F2 guard must run first.
		d.LookupCoveringDomain = func(ctx context.Context, sub, userID string) (*identity.Domain, error) {
			return &identity.Domain{Domain: "mnexa.ai", Verified: true}, nil
		}
	})
	for _, bad := range []string{
		"x@acme..mnexa.ai", // consecutive dots → empty middle label
		"x@.team.mnexa.ai", // leading dot → empty first label
		"x@team.mnexa.ai.", // trailing dot (F3 normalization-fragility case)
		"x@team..",         // multiple empty labels
	} {
		code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{"email": bad})
		if code != 400 || errCode(body) != "invalid_request" {
			t.Errorf("malformed %q: want 400 invalid_request, got %d %v", bad, code, body)
		}
	}
}

// TestCreateAgentCrossTenantCoveringSurfacedAsUnregistered (QA F1, handler
// surface): when the store's covering lookup refuses the cover because a
// different tenant owns a more-specific name (returns no-covering), the handler
// surfaces it exactly like an uncovered domain — 400 domain_not_registered — so
// no cross-tenant land-grab is possible via the API. (The label-boundary guard
// itself is exercised against a real DB in identity.TestLookupCoveringDomain_
// CrossTenantIntrusionRejected.)
func TestCreateAgentCrossTenantCoveringSurfacedAsUnregistered(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		// Mirror the store's F1 rejection: no cover for the intruded name.
		d.LookupCoveringDomain = func(ctx context.Context, sub, userID string) (*identity.Domain, error) {
			return nil, errors.New("no covering domain")
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "otto@acme.team.mnexa.ai",
	})
	if code != 400 || errCode(body) != "domain_not_registered" {
		t.Fatalf("cross-tenant refusal must surface as domain_not_registered, got %d %v", code, body)
	}
}

func TestCreateAgentDuplicate(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{
		"email": "dupe@acme.com",
	})
	if code != 409 || errCode(body) != "agent_taken" {
		t.Fatalf("want 409 agent_taken, got %d %v", code, body)
	}
}

func TestCreateAgentLimitExceeded(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/agents", "overcap", map[string]any{
		"email": "bot@acme.com",
	})
	if code != 402 || errCode(body) != "limit_exceeded" {
		t.Fatalf("want 402 limit_exceeded, got %d %v", code, body)
	}
	// The structured cap details ride in the envelope.
	if e, _ := body["error"].(map[string]any); e != nil {
		if d, _ := e["details"].(map[string]any); d == nil || d["resource"] != "agents" {
			t.Fatalf("missing limit details: %v", body)
		}
	}
}

// CreateAgentWithLimit must still 402 an over-cap caller when
// EnforceAgentCreate is unwired (nil), riding the bare AgentLimitExceededError
// rather than EnforceAgentCreate's richer envelope.
func TestCreateAgentLimitExceededWithoutEnforcer(t *testing.T) {
	srv := testServer(t, func(d *Deps) { d.EnforceAgentCreate = nil })
	code, body := postJSON(t, srv.URL+"/v1/agents", "overcap", map[string]any{
		"email": "bot@acme.com",
	})
	if code != 402 || errCode(body) != "limit_exceeded" {
		t.Fatalf("want 402 limit_exceeded, got %d %v", code, body)
	}
	e, _ := body["error"].(map[string]any)
	if e == nil {
		t.Fatalf("missing error envelope: %v", body)
	}
	d, _ := e["details"].(map[string]any)
	if d == nil || d["resource"] != "agents" {
		t.Fatalf("missing limit details: %v", body)
	}
	// 10 is testServer's default GetLimits MaxAgents.
	if d["limit"] != float64(10) || d["current"] != float64(10) {
		t.Fatalf("want limit=current=10 from AgentLimitExceededError, got %v", d)
	}
}

// A GetLimits failure must not fall through with maxAgents left at its zero
// value, which CreateAgentWithLimit reads as unlimited.
func TestCreateAgentEnforcesWhenGetLimitsErrors(t *testing.T) {
	calledWith := -1
	srv := testServer(t, func(d *Deps) {
		d.GetLimits = func(ctx context.Context, userID string) (limits.Limits, error) {
			return limits.Limits{}, errors.New("connection refused")
		}
		d.CreateAgentWithLimit = func(ctx context.Context, email, domain, name, userID string, maxAgents int) (*identity.AgentIdentity, error) {
			calledWith = maxAgents
			return &identity.AgentIdentity{ID: email, RegisteredDomain: domain, Email: email, Name: name, UserID: userID}, nil
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/agents", "good", map[string]any{"email": "new-agent@acme.com"})
	if code != 500 || errCode(body) != "internal_error" {
		t.Fatalf("want 500 internal_error when GetLimits fails, got %d %v", code, body)
	}
	if calledWith != -1 {
		t.Fatalf("CreateAgentWithLimit must not be called after GetLimits fails, but it was called with maxAgents=%d", calledWith)
	}
}

func TestCreateAgentUnauthorized(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents", "", map[string]any{
		"email": "bot@acme.com",
	})
	if code != 401 {
		t.Fatalf("want 401, got %d", code)
	}
}
