package agent

import (
	"context"
	"net/http"

	"github.com/tokencanopy/e2a/internal/identity"
)

// authMemoKey carries a one-shot per-request credential-resolution memo.
type authMemoKey struct{}

// authMemo holds the single resolution outcome for one request. A request
// is handled on one goroutine and the credential is resolved sequentially
// (rate-limit middleware, then the handler, then — on a 401 — the
// WWW-Authenticate challenge builder), so no locking is needed.
type authMemo struct {
	done bool
	p    *identity.Principal
	err  error
}

// WithAuthMemo returns r carrying a one-shot auth memo, so every
// authenticatePrincipal call for that request resolves the credential
// exactly once. Without it, a request that resolves auth more than once
// (rate-limit middleware + handler + challenge re-run) would run the
// delegated verifier repeatedly; because the verifier's JWKS refresh
// bucket is stateful, a second Verify of the same failing token can flip
// a 401 (unknown key) into a 503 (rate-limited) and double-count the
// failure metric. Requests without a memo (the legacy mux, direct calls)
// resolve on every call, exactly as before.
func WithAuthMemo(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authMemoKey{}, &authMemo{}))
}
