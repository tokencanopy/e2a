package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/tokencanopy/e2a/internal/delegated"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/telemetry"
)

// atJWTBearer is a syntactically valid compact JWT whose protected header
// says typ "at+jwt" — delegated-owned by classification regardless of its
// (garbage) signature.
func atJWTBearer(t *testing.T) string {
	t.Helper()
	hdr, err := json.Marshal(map[string]string{"typ": "at+jwt", "alg": "RS256", "kid": "k1"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"sub": "principal-1"})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc(hdr) + "." + enc(payload) + "." + enc([]byte("sig"))
}

// stubDelegatedVerifier returns a fixed outcome for every Verify call.
type stubDelegatedVerifier struct {
	claims *delegated.Claims
	err    error
}

func (s *stubDelegatedVerifier) Verify(context.Context, string) (*delegated.Claims, error) {
	return s.claims, s.err
}

// delegatedMetricsRecorder captures the delegated failure categories.
type delegatedMetricsRecorder struct {
	telemetry.NoOp
	mu         sync.Mutex
	categories []string
}

func (m *delegatedMetricsRecorder) DelegatedAuthFailure(category string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.categories = append(m.categories, category)
}

func (m *delegatedMetricsRecorder) last(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.categories) == 0 {
		t.Fatal("no delegated auth failure recorded")
	}
	return m.categories[len(m.categories)-1]
}

// acquireAgentAccessToken runs the real autonomous flow (e2a_agt_ key →
// /agent/identity → /oauth2/token jwt-bearer) and returns the minted
// access token.
func acquireAgentAccessToken(t *testing.T, f *agentIDFixture) string {
	t.Helper()
	req, err := http.NewRequest("POST", f.srv.URL+"/agent/identity", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var idResp struct {
		IdentityAssertion string `json:"identity_assertion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&idResp); err != nil || idResp.IdentityAssertion == "" {
		t.Fatalf("identity assertion: %v (%+v)", err, idResp)
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {idResp.IdentityAssertion},
	}
	resp2, err := http.PostForm(f.srv.URL+"/oauth2/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var tokResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&tokResp); err != nil || tokResp.AccessToken == "" {
		t.Fatalf("access token: %v (%+v)", err, tokResp)
	}
	return tokResp.AccessToken
}

func bearerRequest(t *testing.T, bearer string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("GET", "/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	return r
}

// TestDelegatedOwnershipNeverFallsThrough is the load-bearing dispatch
// property: a positively classified at+jwt is delegated-owned even when
// (a) the verifier is disabled, (b) agent auth is ready, and (c) the
// EXACT compact string is enrolled as a valid account API key. It must
// 401, never authenticate through another path.
func TestDelegatedOwnershipNeverFallsThrough(t *testing.T) {
	f := newAgentIDFixture(t) // agent auth READY (real RS256 signer)
	metrics := &delegatedMetricsRecorder{}
	f.api.SetMetrics(metrics)

	tok := atJWTBearer(t)

	// Enroll the at+jwt compact string itself as a valid API key row: the
	// API-key path WOULD authenticate it if dispatch ever let it through.
	hash := sha256.Sum256([]byte(tok))
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO api_keys (id, user_id, name, key_prefix, key_hash, scope, created_at)
		 VALUES ('epk_test_1', $1, 'trap', 'eyJ', $2, 'account', now())`,
		f.user.ID, hex.EncodeToString(hash[:]),
	); err != nil {
		t.Fatal(err)
	}
	if p, err := f.store.GetPrincipalByAPIKey(context.Background(), tok); err != nil || p == nil {
		t.Fatalf("test setup: the trap key must be directly valid, got (%v, %v)", p, err)
	}

	// Verifier DISABLED (nil): still delegated-owned, still rejected.
	if p, err := f.api.AuthenticatePrincipal(bearerRequest(t, tok)); err == nil {
		t.Fatalf("at+jwt authenticated as %v via a non-delegated path with the verifier disabled", p.Scope)
	}
	if got := metrics.last(t); got != "invalid_token" {
		t.Fatalf("category = %q, want invalid_token", got)
	}

	// Verifier ENABLED but the token is invalid: same ownership.
	f.api.SetDelegatedVerifier(&stubDelegatedVerifier{err: fmt.Errorf("%w: bad signature", delegated.ErrInvalidToken)})
	if _, err := f.api.AuthenticatePrincipal(bearerRequest(t, tok)); err == nil {
		t.Fatal("invalid delegated token must not fall through to the API-key path")
	}
}

func TestDelegatedTokenResolvesMappedAccountPrincipal(t *testing.T) {
	f := newAgentIDFixture(t)
	issuer := "https://issuer.example.test/oidc"
	if _, err := f.store.AttachExternalPrincipal(context.Background(), issuer, "principal-1", f.user.ID); err != nil {
		t.Fatal(err)
	}
	f.api.SetDelegatedVerifier(&stubDelegatedVerifier{claims: &delegated.Claims{Issuer: issuer, Subject: "principal-1"}})

	p, err := f.api.AuthenticatePrincipal(bearerRequest(t, atJWTBearer(t)))
	if err != nil {
		t.Fatalf("mapped delegated token must authenticate: %v", err)
	}
	if p.Scope != identity.ScopeAccount || p.AgentID != "" || p.User.ID != f.user.ID {
		t.Fatalf("principal = {scope=%s agent=%q user=%s}, want account scope, empty agent, user %s",
			p.Scope, p.AgentID, p.User.ID, f.user.ID)
	}
}

func TestDelegatedUnknownSubjectIs401NotOracle(t *testing.T) {
	f := newAgentIDFixture(t)
	metrics := &delegatedMetricsRecorder{}
	f.api.SetMetrics(metrics)
	f.api.SetDelegatedVerifier(&stubDelegatedVerifier{
		claims: &delegated.Claims{Issuer: "https://issuer.example.test/oidc", Subject: "unmapped"},
	})
	_, err := f.api.AuthenticatePrincipal(bearerRequest(t, atJWTBearer(t)))
	if err == nil {
		t.Fatal("unmapped subject must not authenticate")
	}
	if errors.Is(err, identity.ErrAuthUnavailable) {
		t.Fatal("unknown subject is a 401-class failure, not availability")
	}
	if got := metrics.last(t); got != "unknown_subject" {
		t.Fatalf("category = %q, want unknown_subject", got)
	}
}

func TestDelegatedAvailabilitySplits503(t *testing.T) {
	f := newAgentIDFixture(t)
	metrics := &delegatedMetricsRecorder{}
	f.api.SetMetrics(metrics)

	// Verifier unavailable (JWKS outage / not discovered): 503 class.
	f.api.SetDelegatedVerifier(&stubDelegatedVerifier{err: fmt.Errorf("%w: no keys", delegated.ErrUnavailable)})
	_, err := f.api.AuthenticatePrincipal(bearerRequest(t, atJWTBearer(t)))
	if !errors.Is(err, identity.ErrAuthUnavailable) {
		t.Fatalf("verifier outage err = %v, want ErrAuthUnavailable", err)
	}
	if got := metrics.last(t); got != "verifier_unavailable" {
		t.Fatalf("category = %q, want verifier_unavailable", got)
	}

	// Identity-store failure: token verified, mapping unreadable — 503
	// class (a canceled request context makes the store query fail).
	f.api.SetDelegatedVerifier(&stubDelegatedVerifier{
		claims: &delegated.Claims{Issuer: "https://issuer.example.test/oidc", Subject: "principal-1"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.api.AuthenticatePrincipal(bearerRequest(t, atJWTBearer(t)).WithContext(ctx))
	if !errors.Is(err, identity.ErrAuthUnavailable) {
		t.Fatalf("store outage err = %v, want ErrAuthUnavailable", err)
	}
	if got := metrics.last(t); got != "identity_store_failure" {
		t.Fatalf("category = %q, want identity_store_failure", got)
	}
}

// TestDelegatedWireSplitOnLegacyMux exercises the two wire classes
// through a real HTTP endpoint that uses writeAuthError (/agent/identity
// authenticates bearers): invalid → 401 + bare Bearer challenge,
// unavailable → 503 with NO challenge.
func TestDelegatedWireSplitOnLegacyMux(t *testing.T) {
	f := newAgentIDFixture(t)

	do := func(t *testing.T) *http.Response {
		t.Helper()
		req, err := http.NewRequest("POST", f.srv.URL+"/agent/identity", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+atJWTBearer(t))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	// Disabled verifier: 401 with the existing bare Bearer challenge.
	resp := do(t)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled verifier status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="e2a"` {
		t.Fatalf("challenge = %q, want bare Bearer realm", got)
	}

	// Unavailable verifier: 503 with no challenge.
	f.api.SetDelegatedVerifier(&stubDelegatedVerifier{err: fmt.Errorf("%w: outage", delegated.ErrUnavailable)})
	resp = do(t)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unavailable verifier status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Fatalf("503 must carry no WWW-Authenticate, got %q", got)
	}
}

// TestExistingCredentialPrecedenceUnchanged locks the non-delegated
// paths with the delegated verifier wired and failing: API keys, agent
// JWTs, and ate2a_ bearers keep today's exact behavior.
func TestExistingCredentialPrecedenceUnchanged(t *testing.T) {
	f := newAgentIDFixture(t)
	f.api.SetDelegatedVerifier(&stubDelegatedVerifier{err: fmt.Errorf("%w: always", delegated.ErrInvalidToken)})

	// Agent-scoped API key still authenticates.
	p, err := f.api.AuthenticatePrincipal(bearerRequest(t, f.apiKey))
	if err != nil || p.Scope != identity.ScopeAgent || p.AgentID != f.agent.ID {
		t.Fatalf("agent API key = (%+v, %v), want agent scope bound to %s", p, err, f.agent.ID)
	}

	// A real e2a-minted agent access token (payload typ, not header typ)
	// still resolves through the agent-JWT path. Acquired over the real
	// bootstrap flow: e2a_agt_ key → identity_assertion → access_token.
	tok := acquireAgentAccessToken(t, f)
	p, err = f.api.AuthenticatePrincipal(bearerRequest(t, tok))
	if err != nil || p.Scope != identity.ScopeAgent || p.AgentID != f.agent.ID {
		t.Fatalf("agent JWT = (%+v, %v), want agent scope bound to %s", p, err, f.agent.ID)
	}

	// A tampered agent JWT still hard-rejects (no API-key fall-through).
	if _, err := f.api.AuthenticatePrincipal(bearerRequest(t, tok+"x")); err == nil {
		t.Fatal("tampered agent JWT must reject")
	}

	// An unknown ate2a_ bearer still fails via the OAuth path with the
	// OAuth challenge semantics (provider not wired here).
	if _, err := f.api.AuthenticatePrincipal(bearerRequest(t, "ate2a_unknown")); err == nil {
		t.Fatal("unknown OAuth bearer must reject")
	}

	// A plain unknown opaque bearer still fails via the API-key path.
	if _, err := f.api.AuthenticatePrincipal(bearerRequest(t, "e2a_acct_unknown")); err == nil {
		t.Fatal("unknown API key must reject")
	}

	// No Authorization header still reaches the session fallback (none
	// wired here → authorization required).
	r := httptest.NewRequest("GET", "/v1/agents", nil)
	if _, err := f.api.AuthenticatePrincipal(r); err == nil || strings.Contains(err.Error(), "delegated") {
		t.Fatalf("cookie fallback err = %v, want plain authorization-required", err)
	}
}
