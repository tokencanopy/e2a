package httpapi

import (
	"context"
	"errors"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
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

// #825: an at-cap caller asking for a domain someone else already owns
// must get 409 domain_taken, not a 402 that tells them to upgrade a plan
// that would not fix anything.
func TestRegisterDomainAtCapStillReturnsConflictForAnotherAccountsDomain(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		// Model claimOrCreateDomain's real ordering: a cross-account name
		// is ErrDomainTaken no matter what maxDomains is.
		d.ClaimDomain = func(ctx context.Context, domain, userID string, maxDomains int) (*identity.Domain, error) {
			if domain == "other-account.com" {
				return nil, identity.ErrDomainTaken
			}
			return &identity.Domain{Domain: domain, Verified: false, VerificationToken: "e2a-verify=new"}, nil
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/domains", "overcap", map[string]any{"domain": "other-account.com"})
	if code != 409 || errCode(body) != "domain_taken" {
		t.Fatalf("want 409 domain_taken for a domain owned by another account even when the caller is at cap, got %d %v", code, body)
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

// ClaimDomain is the race-proof source of truth for max_domains (#822): a
// DomainLimitExceededError from it must still surface as 402 limit_exceeded,
// with EnforceDomainCreate consulted only afterward for the richer details (#825).
func TestRegisterDomainClaimDomainLimitExceededMapsTo402(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.ClaimDomain = func(ctx context.Context, domain, userID string, maxDomains int) (*identity.Domain, error) {
			return nil, &identity.DomainLimitExceededError{Limit: maxDomains, Current: maxDomains}
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/domains", "good", map[string]any{"domain": "raced.com"})
	if code != 402 || errCode(body) != "limit_exceeded" {
		t.Fatalf("want 402 limit_exceeded when ClaimDomain reports the cap, got %d %v", code, body)
	}
}

// A GetLimits failure must not fall through with maxDomains left at its zero
// value: that reads as "unlimited" to ClaimDomain and would silently drop
// the cap for this request instead of failing safe.
func TestRegisterDomainAtCapEnforcesWhenGetLimitsErrors(t *testing.T) {
	claimedWith := -1
	srv := testServer(t, func(d *Deps) {
		d.GetLimits = func(ctx context.Context, userID string) (limits.Limits, error) {
			return limits.Limits{}, errors.New("connection refused")
		}
		d.ClaimDomain = func(ctx context.Context, domain, userID string, maxDomains int) (*identity.Domain, error) {
			claimedWith = maxDomains
			return &identity.Domain{Domain: domain, Verified: false, VerificationToken: "e2a-verify=new"}, nil
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/domains", "good", map[string]any{"domain": "new-domain.com"})
	if code != 500 || errCode(body) != "internal_error" {
		t.Fatalf("want 500 internal_error when GetLimits fails, got %d %v", code, body)
	}
	if claimedWith != -1 {
		t.Fatalf("ClaimDomain must not be called after GetLimits fails, but it was called with maxDomains=%d", claimedWith)
	}
}
