package agent_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/httpapi"
)

// TestDelegatedAuthWireSplitThroughHTTPAPI is the M2 end-to-end proof: a
// failing delegated request routed through the real v1 middleware chain
// (rate-limit middleware resolve → handler resolve → 401 challenge
// re-run) must resolve the credential exactly once, so a stateful
// verifier returns 401 rather than flipping to 503 on a second Verify.
// listWebhooks is poll-limited, so with PollLimit wired the rate-limit
// middleware resolves the principal before the handler — the exact
// double-resolve path the per-request auth memo collapses.
func TestDelegatedAuthWireSplitThroughHTTPAPI(t *testing.T) {
	f := newAgentIDFixture(t)
	verifier := &statefulDelegatedVerifier{}
	f.api.SetDelegatedVerifier(verifier)

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		PrincipalAuthenticator: f.api.AuthenticatePrincipal,
		AuthChallenge:          f.api.WWWAuthenticateChallenge,
		// A permissive poll limiter so the rate-limit middleware resolves
		// the principal on this read op before the handler does.
		PollLimit: func(string) (bool, time.Duration, int, int, int) {
			return true, 0, 240, 240, 0
		},
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest("GET", srv.URL+"/v1/webhooks", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+atJWTBearer(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a second Verify would flip a stateful failure to 503)", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="e2a"` {
		t.Fatalf("challenge = %q, want the bare Bearer challenge", got)
	}
	if got := verifier.count(); got != 1 {
		t.Fatalf("delegated verifier ran %d times for one request, want 1", got)
	}
}
