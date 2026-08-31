package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/tokencanopy/e2a/internal/delegated"
	"github.com/tokencanopy/e2a/internal/identity"
)

// DelegatedVerifier is the narrow seam over delegated.Verifier so tests
// can substitute outcomes without an issuer.
type DelegatedVerifier interface {
	Verify(ctx context.Context, bearer string) (*delegated.Claims, error)
}

// DelegatedIdentityLookup is the narrow store seam used after a delegated
// token has been cryptographically verified. identity.Store satisfies it.
type DelegatedIdentityLookup interface {
	GetUserByExternalPrincipal(ctx context.Context, issuer, subject string) (*identity.User, error)
}

// SetDelegatedVerifier wires the delegated-token verifier (config
// delegated.enabled). Nil (the default) keeps delegated-owned tokens
// failing authentication — ownership itself is decided by Classify and
// never depends on this being set.
func (a *API) SetDelegatedVerifier(v DelegatedVerifier) { a.delegated = v }

// SetDelegatedIdentityLookup replaces the external-principal lookup seam.
// Production uses the identity.Store installed by NewAPI; tests use this to
// distinguish a genuine store failure from request cancellation.
func (a *API) SetDelegatedIdentityLookup(lookup DelegatedIdentityLookup) {
	a.delegatedLookup = lookup
}

// errDelegatedInvalid classifies every delegated 401: the bare Bearer
// challenge with no check-specific detail (which check failed — type,
// signature, claim, unknown subject, or verifier disabled — must not be
// distinguishable on the wire).
var errDelegatedInvalid = errors.New("invalid delegated token")

// principalFromDelegatedToken owns every positively classified at+jwt
// bearer. Verification and the one exact (issuer, subject) mapping
// lookup produce an account-scoped principal with no bound agent —
// exactly an account API key's authority. Failures split into the two
// wire classes: invalid (401) and unavailable (503, via
// identity.ErrAuthUnavailable).
func (a *API) principalFromDelegatedToken(r *http.Request, bearer string) (*identity.Principal, error) {
	if a.delegated == nil {
		// Disabled verifier: delegated-owned tokens still never fall
		// through to any other credential path.
		a.emit().DelegatedAuthFailure("invalid_token")
		return nil, errDelegatedInvalid
	}
	claims, err := a.delegated.Verify(r.Context(), bearer)
	if err != nil {
		if errors.Is(err, delegated.ErrUnavailable) {
			a.emit().DelegatedAuthFailure("verifier_unavailable")
			return nil, fmt.Errorf("%w: delegated verifier", identity.ErrAuthUnavailable)
		}
		a.emit().DelegatedAuthFailure("invalid_token")
		return nil, errDelegatedInvalid
	}
	user, err := a.delegatedLookup.GetUserByExternalPrincipal(r.Context(), claims.Issuer, claims.Subject)
	if err != nil {
		if isDelegatedRequestCancellation(r.Context(), err) {
			// The caller went away while the lookup was in flight. Preserve the
			// fail-closed 503 class for any observer still waiting, but do not
			// turn caller-controlled disconnects into an availability metric.
			return nil, fmt.Errorf("%w: delegated request canceled", identity.ErrAuthUnavailable)
		}
		// The token may well be valid — the mapping store couldn't say.
		// 503, not 401, so a database blip doesn't read as revocation.
		a.emit().DelegatedAuthFailure("identity_store_failure")
		return nil, fmt.Errorf("%w: identity store", identity.ErrAuthUnavailable)
	}
	if user == nil {
		// Unknown subject: plain 401, indistinguishable from any other
		// invalid token (no existence oracle).
		a.emit().DelegatedAuthFailure("unknown_subject")
		return nil, errDelegatedInvalid
	}
	return &identity.Principal{User: user, Scope: identity.ScopeAccount}, nil
}

func isDelegatedRequestCancellation(ctx context.Context, err error) bool {
	return errors.Is(ctx.Err(), context.Canceled) && errors.Is(err, context.Canceled)
}
