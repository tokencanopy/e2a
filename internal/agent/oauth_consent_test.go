package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/apiserver"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/oauth"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhook"
	"github.com/tokencanopy/e2a/internal/ws"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/fosite"
)

// consentFixture wires the API with provider + storage + userAuth, and
// seeds a logged-in user. Returns the server URL, the fosite provider
// (for tests that need to bypass /authorize via mintAuthCode), the
// session cookie value, the user ID, the client ID, and the underlying
// pool (for tests that need to look at row state).
type consentFixture struct {
	server       *httptest.Server
	provider     fosite.OAuth2Provider
	pool         *pgxpool.Pool
	sessionToken string
	userID       string
	clientID     string
	issuer       string
}

func newConsentFixture(t *testing.T) *consentFixture {
	t.Helper()
	const issuer = "https://test.e2a.dev"
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")

	// UserAuth wired with a stub OAuth config — we never actually
	// drive Google login through it; the test creates sessions
	// directly via CreateUserSession.
	userAuth := auth.NewUserAuth(&config.OAuthConfig{
		GoogleClientID:     "test-id",
		GoogleClientSecret: "test-secret",
		RedirectURL:        "http://localhost/api/auth/callback",
	}, store, false)

	api := agent.NewAPI(store, sender, smtpRelay, userAuth, usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", issuer, false)

	secret := []byte("test-secret-test-secret-test-sec")
	storage := oauth.NewStorage(pool)
	provider, err := oauth.NewProvider(storage, issuer, secret)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	api.SetOAuthProvider(provider)
	api.SetOAuthStorage(storage)

	router := mux.NewRouter()
	api.RegisterRoutes(router)

	// Wrap the legacy mux with the typed /v1 surface so /v1/agents is
	// routable (bearer auth tests hit it). Mirrors testutil.TestServer's
	// apiserver wiring; the /oauth2/* + /consent routes fall through to
	// the legacy mux via chi's NotFound handler.
	usageStore := usage.NewStore(pool)
	enforcer := limits.NewEnforcer(limits.NewStore(pool), usageStore, limits.Defaults{
		PlanCode: "test", MaxAgents: 100000, MaxDomains: 100000,
		MaxMessagesMonth: 100000, MaxStorageBytes: 1 << 40,
	}, time.Minute)
	subscriberStore := webhook.NewSubscriberStore(pool)
	idempotencyStore := idempotency.NewStore(pool)
	api.SetIdempotencyStore(idempotencyStore)
	api.SetSubscriberStore(subscriberStore)
	api.SetEnforcer(enforcer)
	api.SetUsageStore(usageStore)
	wsHub := ws.NewHub()
	wsHandler := ws.NewHandler(wsHub, store)
	t.Cleanup(wsHub.Close)

	v1 := apiserver.New(apiserver.Params{
		API: api, Store: store, Enforcer: enforcer, UsageStore: usageStore,
		SubscriberStore: subscriberStore, Idempotency: idempotencyStore, Pool: pool,
		SMTPDomain: "test.e2a.dev", SharedDomain: "agents.e2a.dev",
		PublicURL: issuer, Production: false,
		Legacy: router, WSHandle: wsHandler.ServeWithEmail,
	})
	server := httptest.NewServer(v1)
	t.Cleanup(server.Close)

	// Seed a user + an active session.
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "consent-user-"+randHex8(t)+"@example.com", "Test", "google-"+randHex8(t))
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	sessionToken, err := store.CreateUserSession(ctx, user.ID)
	if err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}

	// Seed the shared domain row that auto-create-agent needs.
	if err := store.EnsureSharedDomain(ctx, "agents.e2a.dev"); err != nil {
		t.Fatalf("EnsureSharedDomain: %v", err)
	}

	// Seed a public DCR client.
	clientID := "mcp_consent_test"
	if _, err := pool.Exec(ctx, `
		INSERT INTO oauth_clients
		    (client_id, client_name, redirect_uris, grant_types,
		     response_types, scopes, audiences, token_endpoint_auth_method,
		     public, created_via)
		VALUES ($1, 'consent test client',
		        ARRAY['http://localhost:8765/callback'],
		        ARRAY['authorization_code','refresh_token'],
		        ARRAY['code'],
		        ARRAY['agent','account'],
		        ARRAY[]::TEXT[],
		        'none', TRUE, 'dcr')
		ON CONFLICT (client_id) DO NOTHING
	`, clientID); err != nil {
		t.Fatalf("seed client: %v", err)
	}

	return &consentFixture{
		server:       server,
		provider:     provider,
		pool:         pool,
		sessionToken: sessionToken,
		userID:       user.ID,
		clientID:     clientID,
		issuer:       issuer,
	}
}

// authorizeRequest sends a GET /oauth2/authorize. When session=true
// the request carries the user session cookie; otherwise it goes in
// anonymously to test the "no session → login" path.
func (f *consentFixture) authorizeRequest(t *testing.T, q url.Values, session bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", f.server.URL+"/oauth2/authorize?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if session {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: f.sessionToken})
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// consentPOST submits the consent form. Carries the session cookie by
// default (consent requires it).
func (f *consentFixture) consentPOST(t *testing.T, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", f.server.URL+"/oauth2/consent",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: f.sessionToken})
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// authorizeParams returns a baseline set of /authorize query params.
// Tests override individual fields via the second argument.
func authorizeParams(challenge, clientID, state string) url.Values {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "http://localhost:8765/callback")
	q.Set("scope", "agent")
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return q
}

// ──────────────────────── /authorize ────────────────────────

// TestHTTP_Authorize_NoSessionRoutesThroughConsentChooser is the handler half
// of the signed-out handler/UI contract. A valid request must reach the
// provider chooser before either login door is selected, and the chooser must
// receive the exact authorize request URI that both doors resume after login.
func TestHTTP_Authorize_NoSessionRoutesThroughConsentChooser(t *testing.T) {
	f := newConsentFixture(t)
	_, challenge := newPKCE(t)
	q := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	q.Set("response_mode", "query")
	resp := f.authorizeRequest(t, q, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Location parse: %v", err)
	}
	if !strings.HasSuffix(loc.Path, "/oauth/consent") {
		t.Errorf("Location path = %q, want /oauth/consent", loc.Path)
	}
	wantReturnTo := "/oauth2/authorize?" + q.Encode()
	if got := loc.Query().Get("return_to"); got != wantReturnTo {
		t.Errorf("return_to = %q, want exact authorize request %q", got, wantReturnTo)
	}
	for key, values := range q {
		if got := loc.Query()[key]; len(got) != len(values) || strings.Join(got, "\x00") != strings.Join(values, "\x00") {
			t.Errorf("consent redirect missing/wrong %q: got %q, want %q", key, got, values)
		}
	}
}

// TestHTTP_Authorize_WithSession redirects to {publicURL}/oauth/consent
// preserving every authorize parameter so the consent page can hidden-
// field them back into its POST.
func TestHTTP_Authorize_WithSession(t *testing.T) {
	f := newConsentFixture(t)
	_, challenge := newPKCE(t)
	q := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	resp := f.authorizeRequest(t, q, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Location parse: %v", err)
	}
	if !strings.HasSuffix(loc.Path, "/oauth/consent") {
		t.Errorf("Location path = %q, want /oauth/consent", loc.Path)
	}
	for _, key := range []string{"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method"} {
		if got, want := loc.Query().Get(key), q.Get(key); got != want {
			t.Errorf("consent redirect missing/wrong %q: got %q, want %q", key, got, want)
		}
	}
}

// TestHTTP_Authorize_ExplicitQueryMode verifies that a client may explicitly
// request the sole response mode advertised in OAuth discovery.
func TestHTTP_Authorize_ExplicitQueryMode(t *testing.T) {
	f := newConsentFixture(t)
	_, challenge := newPKCE(t)
	q := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	q.Set("response_mode", "query")

	resp := f.authorizeRequest(t, q, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 consent redirect", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Location parse: %v", err)
	}
	if !strings.HasSuffix(loc.Path, "/oauth/consent") {
		t.Errorf("Location path = %q, want /oauth/consent", loc.Path)
	}
	if got := loc.Query().Get("response_mode"); got != "query" {
		t.Errorf("response_mode = %q, want query", got)
	}
}

// TestHTTP_Authorize_InvalidClient — fosite rejects before we get a
// chance to check the session. The response is a fosite-emitted
// direct error (not a redirect to redirect_uri, since redirect_uri
// isn't trusted yet).
func TestHTTP_Authorize_InvalidClient(t *testing.T) {
	f := newConsentFixture(t)
	_, challenge := newPKCE(t)
	q := authorizeParams(challenge, "mcp_unknown_client", "s1s1s1s1s1s1s1s1")
	resp := f.authorizeRequest(t, q, true)
	defer resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("status = %d, want direct 4xx", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("unknown client must not redirect to an untrusted redirect_uri; Location=%q", loc)
	}
}

// TestHTTP_Authorize_InvalidScope_RedirectIncludesIssuer covers an authorize
// error after fosite has verified the client and redirect_uri. RFC 9207
// requires the redirect to identify the authorization server just like a
// successful code response does.
func TestHTTP_Authorize_InvalidScope_RedirectIncludesIssuer(t *testing.T) {
	f := newConsentFixture(t)
	_, challenge := newPKCE(t)
	q := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	q.Set("scope", "agent unknown")

	resp := f.authorizeRequest(t, q, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 302/303 redirect-with-error", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loc.Query().Get("error"); got != "invalid_scope" {
		t.Errorf("error = %q, want invalid_scope", got)
	}
	if got := loc.Query().Get("state"); got != "s1s1s1s1s1s1s1s1" {
		t.Errorf("state = %q, want s1s1s1s1s1s1s1s1", got)
	}
	if got := loc.Query().Get("iss"); got != f.issuer {
		t.Errorf("RFC 9207 iss = %q, want %q", got, f.issuer)
	}
}

// TestHTTP_Authorize_DangerousStoredRedirectDoesNotRedirect covers the
// defense-in-depth boundary for client rows inserted outside DCR validation.
// Even when fosite exact-matches the stored redirect_uri, the server must not
// emit an executable Location header on an authorization error.
func TestHTTP_Authorize_DangerousStoredRedirectDoesNotRedirect(t *testing.T) {
	f := newConsentFixture(t)
	ctx := context.Background()
	clientID := "mcp_dangerous_" + randHex8(t)
	const redirectURI = "javascript:alert(1)"
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO oauth_clients
		    (client_id, client_name, redirect_uris, grant_types,
		     response_types, scopes, audiences, token_endpoint_auth_method,
		     public, created_via)
		VALUES ($1, 'dangerous redirect fixture', ARRAY[$2],
		        ARRAY['authorization_code','refresh_token'], ARRAY['code'],
		        ARRAY['agent'], ARRAY[]::TEXT[], 'none', TRUE, 'admin')
	`, clientID, redirectURI); err != nil {
		t.Fatalf("seed dangerous client: %v", err)
	}

	_, challenge := newPKCE(t)
	q := authorizeParams(challenge, clientID, "s1s1s1s1s1s1s1s1")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "agent unknown")
	resp := f.authorizeRequest(t, q, true)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("dangerous redirect must not emit Location; got %q", loc)
	}
}

// TestHTTP_Authorize_ErrorUsesAdvertisedQueryMode verifies that an invalid
// request cannot move the error parameters away from the server's sole
// advertised response mode. In particular, fosite otherwise honors an
// unsupported response_mode while writing an error, putting the error in a
// fragment or an HTML form where RFC 9207's iss decorator cannot accompany it.
func TestHTTP_Authorize_ErrorUsesAdvertisedQueryMode(t *testing.T) {
	for _, responseMode := range []string{"fragment", "form_post"} {
		t.Run(responseMode, func(t *testing.T) {
			f := newConsentFixture(t)
			_, challenge := newPKCE(t)
			q := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
			q.Set("scope", "agent unknown")
			q.Set("response_mode", responseMode)

			resp := f.authorizeRequest(t, q, true)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("status = %d, want 302/303 redirect-with-error", resp.StatusCode)
			}
			loc, err := url.Parse(resp.Header.Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if loc.Fragment != "" {
				t.Errorf("fragment = %q, want empty because only query mode is supported", loc.Fragment)
			}
			if got := loc.Query().Get("error"); got != "invalid_scope" {
				t.Errorf("error = %q, want invalid_scope", got)
			}
			if got := loc.Query().Get("state"); got != "s1s1s1s1s1s1s1s1" {
				t.Errorf("state = %q, want s1s1s1s1s1s1s1s1", got)
			}
			if got := loc.Query().Get("iss"); got != f.issuer {
				t.Errorf("RFC 9207 iss = %q, want %q", got, f.issuer)
			}
		})
	}
}

// ──────────────────────── /consent ────────────────────────

// TestHTTP_Consent_Allow_CreateNew is the happy path: user picks
// "create_new" + a slug, gets a code redirected to the client's
// redirect_uri. Verifies (a) status 303, (b) code has our prefix,
// (c) iss is present per RFC 9207, (d) state round-trips, and
// (e) the agent row was actually created.
func TestHTTP_Consent_Allow_CreateNew(t *testing.T) {
	f := newConsentFixture(t)
	_, challenge := newPKCE(t)

	form := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	form.Set("action", "allow")
	form.Set("agent_choice", "create_new")
	form.Set("new_agent_slug", "myconsentbot")

	resp := f.consentPOST(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 See Other", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Location parse: %v", err)
	}
	if !strings.HasPrefix(loc.String(), "http://localhost:8765/callback") {
		t.Errorf("must redirect back to client redirect_uri: got %q", loc.String())
	}
	code := loc.Query().Get("code")
	if !strings.HasPrefix(code, oauth.AuthCodePrefix) {
		t.Errorf("code missing %q prefix: %q", oauth.AuthCodePrefix, code)
	}
	if got := loc.Query().Get("state"); got != "s1s1s1s1s1s1s1s1" {
		t.Errorf("state round-trip: got %q, want s1s1s1s1s1s1s1s1", got)
	}
	if got := loc.Query().Get("iss"); got != f.issuer {
		t.Errorf("RFC 9207 iss missing/wrong: got %q, want %q", got, f.issuer)
	}

	// Verify the agent was actually created on the shared domain.
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_identities WHERE id = $1 AND user_id = $2`,
		"myconsentbot@agents.e2a.dev", f.userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 agent row for new slug, got %d", count)
	}
}

// TestHTTP_Consent_Allow_CreateNew_AtAgentCap: the auto-create path must
// enforce the same max_agents cap the REST create path does; it previously
// did not check at all.
func TestHTTP_Consent_Allow_CreateNew_AtAgentCap(t *testing.T) {
	f := newConsentFixture(t)
	ctx := context.Background()

	if err := limits.NewStore(f.pool).Upsert(ctx, f.userID, limits.Limits{
		PlanCode: "test", MaxAgents: 0, MaxDomains: 100000,
		MaxMessagesMonth: 100000, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert limits: %v", err)
	}

	_, challenge := newPKCE(t)
	form := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	form.Set("action", "allow")
	form.Set("agent_choice", "create_new")
	form.Set("new_agent_slug", "capconsentbot")

	resp := f.consentPOST(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 Payment Required", resp.StatusCode)
	}

	var agentCount int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_identities WHERE id = $1`,
		"capconsentbot@agents.e2a.dev").Scan(&agentCount); err != nil {
		t.Fatal(err)
	}
	if agentCount != 0 {
		t.Errorf("agent must not be created over cap, got %d rows", agentCount)
	}

	var codeCount int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM oauth_auth_codes WHERE user_id = $1`, f.userID).Scan(&codeCount); err != nil {
		t.Fatal(err)
	}
	if codeCount != 0 {
		t.Errorf("no auth code should be issued when the agent cap blocks creation, got %d", codeCount)
	}
}

// TestHTTP_Consent_Allow_Existing — user picks an agent they already
// own. No new agent created; code issued bound to the chosen email.
func TestHTTP_Consent_Allow_Existing(t *testing.T) {
	f := newConsentFixture(t)
	// Pre-create an agent for this user on the shared domain.
	ctx := context.Background()
	store := identity.NewStore(f.pool)
	if _, err := store.CreateAgent(ctx, "mineconsent@agents.e2a.dev", "agents.e2a.dev", "", "", "local", f.userID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	_, challenge := newPKCE(t)
	form := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	form.Set("action", "allow")
	form.Set("agent_choice", "existing:mineconsent@agents.e2a.dev")

	resp := f.consentPOST(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("code") == "" {
		t.Fatal("expected code in redirect")
	}
}

// TestHTTP_Consent_Allow_Existing_NotOwned — picking an agent owned by a
// different user is refused with the generic, non-revealing error. It must
// NOT be a 403 "you do not own that agent": that told any logged-in user
// which arbitrary addresses are live agents on other accounts
// (GHSA-jh7v-7hx6-2mc2). See the anti-enumeration comment in
// handleOAuthConsent.
func TestHTTP_Consent_Allow_Existing_NotOwned(t *testing.T) {
	f := newConsentFixture(t)
	ctx := context.Background()
	store := identity.NewStore(f.pool)
	// Create another user + an agent owned by them.
	other, err := store.CreateOrGetUser(ctx, "other-"+randHex8(t)+"@example.com", "Other", "google-other-"+randHex8(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAgent(ctx, "victim@agents.e2a.dev", "agents.e2a.dev", "", "", "local", other.ID); err != nil {
		t.Fatal(err)
	}

	_, challenge := newPKCE(t)
	form := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	form.Set("action", "allow")
	form.Set("agent_choice", "existing:victim@agents.e2a.dev")

	resp := f.consentPOST(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (must not distinguish not-owned from nonexistent)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// Exact match, not a substring check: the message must be the generic
	// one shared with the nonexistent-agent case, and nothing about
	// ownership or existence may leak into it.
	if got := strings.TrimSpace(string(body)); got != consentUnknownAgentMsg {
		t.Errorf("body = %q, want the generic %q (must not reveal that the agent exists but belongs to another account)", got, consentUnknownAgentMsg)
	}
}

// consentUnknownAgentMsg is the single response body both the "no such
// agent" and "not your agent" paths must return — see the anti-enumeration
// comment in handleOAuthConsent.
const consentUnknownAgentMsg = "unknown or inaccessible agent"

// TestHTTP_Consent_Allow_Existing_NotEnumerable is the actual regression
// guard for GHSA-jh7v-7hx6-2mc2: probing a real agent owned by another
// account and probing an address that does not exist at all must produce
// byte-identical responses, so the endpoint cannot be used as an existence
// oracle. Asserting each case's status separately would not catch a future
// change that keeps both at 400 but reintroduces distinct bodies.
func TestHTTP_Consent_Allow_Existing_NotEnumerable(t *testing.T) {
	f := newConsentFixture(t)
	ctx := context.Background()
	store := identity.NewStore(f.pool)
	other, err := store.CreateOrGetUser(ctx, "other-"+randHex8(t)+"@example.com", "Other", "google-other-"+randHex8(t))
	if err != nil {
		t.Fatal(err)
	}
	// A real agent on another account, on the shared domain (which the
	// fixture seeds verified via EnsureSharedDomain). Domain verification is
	// not what this test turns on: handleOAuthConsent never consults
	// DomainVerified, so the invariant it pins — existence must not be
	// observable — holds for verified and unverified domains alike. That
	// matters because the SMTP edge hides unverified-domain agents behind the
	// same 550 it uses for unknown recipients (internal/relay/server.go), so
	// this surface must not be the one that discloses them.
	if _, err := store.CreateAgent(ctx, "realvictim@agents.e2a.dev", "agents.e2a.dev", "", "", "local", other.ID); err != nil {
		t.Fatal(err)
	}

	probe := func(t *testing.T, address string) (int, string) {
		t.Helper()
		_, challenge := newPKCE(t)
		form := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
		form.Set("action", "allow")
		form.Set("agent_choice", "existing:"+address)
		resp := f.consentPOST(t, form)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}

	existsStatus, existsBody := probe(t, "realvictim@agents.e2a.dev")
	missingStatus, missingBody := probe(t, "no-such-agent-"+randHex8(t)+"@agents.e2a.dev")

	if existsStatus != missingStatus {
		t.Errorf("status leaks existence: exists=%d missing=%d", existsStatus, missingStatus)
	}
	if existsBody != missingBody {
		t.Errorf("body leaks existence:\n exists  = %q\n missing = %q", existsBody, missingBody)
	}
	if existsStatus != http.StatusBadRequest {
		t.Errorf("both cases should be 400, got %d", existsStatus)
	}
}

// TestHTTP_Consent_Deny — user clicks Deny. We redirect back to
// redirect_uri with error=access_denied, state, and iss.
func TestHTTP_Consent_Deny(t *testing.T) {
	f := newConsentFixture(t)
	_, challenge := newPKCE(t)
	form := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	form.Set("action", "deny")

	resp := f.consentPOST(t, form)
	defer resp.Body.Close()
	// Redirect-uri-bound authorization errors remain 302/303 after the
	// RFC 9207 issuer is added.
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 302/303 redirect-with-error", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loc.String(), "http://localhost:8765/callback") {
		t.Errorf("must redirect back to client redirect_uri on deny: got %q", loc.String())
	}
	if got := loc.Query().Get("error"); got != "access_denied" {
		t.Errorf("error = %q, want access_denied", got)
	}
	if got := loc.Query().Get("state"); got != "s1s1s1s1s1s1s1s1" {
		t.Errorf("state = %q, want s1s1s1s1s1s1s1s1", got)
	}
	if got := loc.Query().Get("iss"); got != f.issuer {
		t.Errorf("RFC 9207 iss = %q, want %q", got, f.issuer)
	}
}

// TestHTTP_Consent_NoSession — consent without the session cookie 401s.
func TestHTTP_Consent_NoSession(t *testing.T) {
	f := newConsentFixture(t)
	_, challenge := newPKCE(t)
	form := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	form.Set("action", "allow")
	form.Set("agent_choice", "create_new")
	form.Set("new_agent_slug", "x")

	// Direct POST without the helper (no cookie).
	resp, err := http.Post(f.server.URL+"/oauth2/consent",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHTTP_Consent_DuplicateSlug — submitting an already-taken slug
// returns 409 and rolls back the tx (no orphan rows).
func TestHTTP_Consent_DuplicateSlug(t *testing.T) {
	f := newConsentFixture(t)
	ctx := context.Background()
	store := identity.NewStore(f.pool)
	if _, err := store.CreateAgent(ctx, "takendup@agents.e2a.dev", "agents.e2a.dev", "", "", "local", f.userID); err != nil {
		t.Fatal(err)
	}

	_, challenge := newPKCE(t)
	form := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	form.Set("action", "allow")
	form.Set("agent_choice", "create_new")
	form.Set("new_agent_slug", "takendup")

	resp := f.consentPOST(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	// And no auth-code row should have been inserted (tx rolled back).
	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM oauth_auth_codes WHERE user_id = $1`, f.userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("rolled-back tx should leave 0 auth_codes for user, got %d", count)
	}
}

// TestHTTP_FullE2E_AuthorizeConsentToken: the headline integration
// test. Drive /authorize → /consent → /token end-to-end via real HTTP
// calls. Verifies the protocol surface plus the cross-package
// transaction plus the iss/code/state plumbing all line up.
func TestHTTP_FullE2E_AuthorizeConsentToken(t *testing.T) {
	f := newConsentFixture(t)
	verifier, challenge := newPKCE(t)

	// Step 1: /authorize → 302 to consent UI.
	q := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	resp1 := f.authorizeRequest(t, q, true)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusFound {
		t.Fatalf("step 1 status = %d, want 302", resp1.StatusCode)
	}

	// Step 2: simulate the consent UI submitting the form with allow.
	// In production the web/ consent page would render hidden inputs
	// from the redirect URL's query string and POST them back. We
	// shortcut by submitting authorizeParams directly with the
	// action/agent_choice fields added.
	form := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	form.Set("action", "allow")
	form.Set("agent_choice", "create_new")
	form.Set("new_agent_slug", "e2e-bot-"+randHex8(t))

	resp2 := f.consentPOST(t, form)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("step 2 status = %d, want 303", resp2.StatusCode)
	}
	loc, _ := url.Parse(resp2.Header.Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatal("step 2: missing code")
	}
	if got := loc.Query().Get("iss"); got != f.issuer {
		t.Errorf("step 2: iss = %q, want %q", got, f.issuer)
	}

	// Step 3: exchange the code at /token.
	tokForm := url.Values{}
	tokForm.Set("grant_type", "authorization_code")
	tokForm.Set("code", code)
	tokForm.Set("client_id", f.clientID)
	tokForm.Set("redirect_uri", "http://localhost:8765/callback")
	tokForm.Set("code_verifier", verifier)
	resp3, err := http.Post(f.server.URL+"/oauth2/token",
		"application/x-www-form-urlencoded", strings.NewReader(tokForm.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("step 3 status = %d, want 200", resp3.StatusCode)
	}
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body.AccessToken, oauth.AccessTokenPrefix) {
		t.Errorf("access_token missing prefix: %q", body.AccessToken)
	}
	if !strings.HasPrefix(body.RefreshToken, oauth.RefreshTokenPrefix) {
		t.Errorf("refresh_token missing prefix: %q", body.RefreshToken)
	}
}
