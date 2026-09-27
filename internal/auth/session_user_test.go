package auth_test

import (
	"net/http/httptest"
	"testing"

	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
)

// A session user a guard already resolved (WithSessionUser) is what
// AuthenticateRequest returns, without a second lookup (the store here is nil,
// so any lookup would panic); without one, a request with no cookie is
// unauthenticated.
func TestAuthenticateRequestReusesTheResolvedSessionUser(t *testing.T) {
	ua := auth.NewUserAuth(&config.OAuthConfig{}, nil, false)
	r := httptest.NewRequest("PATCH", "/api/auth/me", nil)
	if got := ua.AuthenticateRequest(r); got != nil {
		t.Fatalf("no cookie, no resolved user: got %+v, want nil", got)
	}
	want := &identity.User{ID: "u_1"}
	if got := ua.AuthenticateRequest(auth.WithSessionUser(r, want)); got != want {
		t.Fatalf("AuthenticateRequest = %+v, want the resolved session user", got)
	}
}
