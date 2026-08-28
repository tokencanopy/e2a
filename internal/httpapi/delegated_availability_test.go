package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
)

// TestAuthAvailabilityWireSplit pins the /v1 envelope behavior for the
// two authentication failure classes: a credential judged invalid is 401
// unauthorized with the Bearer challenge, while an auth backend that
// could not judge it (identity.ErrAuthUnavailable — delegated verifier
// not ready, identity-store outage) is 503 with a generic envelope and
// NO WWW-Authenticate challenge.
func TestAuthAvailabilityWireSplit(t *testing.T) {
	authErr := errors.New("unauthorized")
	srv := httptest.NewServer(New(Deps{
		PrincipalAuthenticator: func(r *http.Request) (*identity.Principal, error) {
			switch r.Header.Get("Authorization") {
			case "Bearer unavailable":
				return nil, fmt.Errorf("%w: delegated verifier", identity.ErrAuthUnavailable)
			default:
				return nil, authErr
			}
		},
		AuthChallenge: func(r *http.Request) string { return `Bearer realm="e2a"` },
	}))
	defer srv.Close()

	get := func(t *testing.T, bearer string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+"/v1/agents", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	t.Run("invalid credential is 401 unauthorized with challenge", func(t *testing.T) {
		resp := get(t, "bad")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="e2a"` {
			t.Fatalf("challenge = %q, want Bearer realm", got)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != "unauthorized" {
			t.Fatalf("code = %q, want unauthorized", body.Error.Code)
		}
	})

	t.Run("availability failure is 503 with no challenge", func(t *testing.T) {
		resp := get(t, "unavailable")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != "" {
			t.Fatalf("503 must carry no WWW-Authenticate, got %q", got)
		}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != "internal_error" {
			t.Fatalf("code = %q, want internal_error (generic availability envelope)", body.Error.Code)
		}
	})
}
