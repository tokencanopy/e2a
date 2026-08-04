package httpapi

import (
	"context"
	"errors"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
)

// POST /v1/domains is idempotent for a domain the caller already owns: the
// claim returns the existing row rather than creating one (#811). Charging
// that no-op against max_domains 402s a caller for re-POSTing a domain they
// already hold — the SDK retry, the re-run of a provisioning script, the
// dashboard's "register" on a domain already in the list all fail once the
// account sits at its cap, and the error names a resource the request would
// not have consumed. The create limit belongs on creates.
func TestRegisterDomainAtCapAllowsReclaimOfOwnedDomain(t *testing.T) {
	srv := testServer(t)
	// "capped-owned.com" is owned by u_overcap ALONE in the shared fake, and
	// "overcap" authenticates as u_overcap — whom EnforceDomainCreate rejects.
	// A singly-owned domain is deliberate: it is only found when the handler
	// passes the authenticated caller's id, so this assertion fails if the
	// scoping is dropped (passing "") or aimed at the wrong account.
	code, body := postJSON(t, srv.URL+"/v1/domains", "overcap", map[string]any{"domain": "capped-owned.com"})
	if code != 201 {
		t.Fatalf("re-register of an owned domain at cap = %d %v, want 201 (creates nothing, so the create cap does not apply)", code, body)
	}
	if body["domain"] != "capped-owned.com" {
		t.Errorf("domain = %v, want capped-owned.com", body["domain"])
	}
}

// The skip is scoped to domains the caller actually owns, so it cannot become
// a way to bypass the cap: LookupDomain filters on user_id, so another
// account's domain is not "already owned" and stays charged (and would then
// be rejected as a conflict by the claim itself).
//
// This runs against the SHARED ownership-aware fake, not a local override, and
// that is the point: "other-account.com" is owned by u_1 alone, so the request
// below only 402s if the handler passes the AUTHENTICATED caller's id. An
// earlier version of this test used a domain no fake matched for anyone, which
// meant it exercised the fake's not-found branch and passed even with the
// scoping removed — the mutation it exists to catch.
func TestRegisterDomainAtCapStillChargesAnotherAccountsDomain(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/domains", "overcap", map[string]any{"domain": "other-account.com"})
	if code != 402 || errCode(body) != "limit_exceeded" {
		t.Fatalf("want 402 limit_exceeded for a domain owned by another account, got %d %v", code, body)
	}
}

// Fail-safe: an unwired lookup means we cannot tell a create from a re-claim,
// so the cap is enforced. A missing dependency must never silently disable a
// limit.
func TestRegisterDomainAtCapEnforcesWhenLookupUnavailable(t *testing.T) {
	srv := testServer(t, func(d *Deps) { d.LookupDomain = nil })
	code, body := postJSON(t, srv.URL+"/v1/domains", "overcap", map[string]any{"domain": "capped-owned.com"})
	if code != 402 || errCode(body) != "limit_exceeded" {
		t.Fatalf("want 402 limit_exceeded when LookupDomain is unwired, got %d %v", code, body)
	}
}

// A lookup that fails for a reason other than "not found" must not be read as
// "already owned" — the store collapses every failure into one error, so an
// unreachable database would otherwise open the cap.
func TestRegisterDomainAtCapEnforcesWhenLookupErrors(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.LookupDomain = func(ctx context.Context, domain, userID string) (*identity.Domain, error) {
			return nil, errors.New("connection refused")
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/domains", "overcap", map[string]any{"domain": "capped-owned.com"})
	if code != 402 || errCode(body) != "limit_exceeded" {
		t.Fatalf("want 402 limit_exceeded when the lookup fails, got %d %v", code, body)
	}
}
