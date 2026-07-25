package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v3"
	"github.com/go-jose/go-jose/v3/jwt"

	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// testOIDCDiscoveryInitial/MaxBackoff shorten the background discovery retry
// loop for tests that deliberately start against an unreachable issuer:
// production defaults (1s initial, 60s cap) would make those tests slow.
const (
	testOIDCDiscoveryInitialBackoff = 10 * time.Millisecond
	testOIDCDiscoveryMaxBackoff     = 50 * time.Millisecond
	testOIDCReadyPollTimeout        = 2 * time.Second
	testOIDCReadyPollInterval       = 5 * time.Millisecond
)

const (
	testOIDCClientID     = "e2a-test-client"
	testOIDCClientSecret = "e2a-test-secret"
	testOIDCUserIDClaim  = "e2a_user_id"
	testOIDCRedirectURL  = "http://app.example.com/api/auth/oidc/callback"
)

type oidcFixture struct {
	oidc              *auth.OIDCAuth
	store             *identity.Store
	server            *httptest.Server
	privateKey        *rsa.PrivateKey
	keyID             string
	userID            string
	tokenNonce        string
	tokenIssuer       string
	tokenAudience     string
	tokenExpiry       time.Time
	signingKey        *rsa.PrivateKey
	includeIDToken    bool
	includeUserID     bool
	userIDClaimValue  any
	tokenStatus       int
	tokenCalls        int
	expectedChallenge string
}

func setupOIDC(t *testing.T) *oidcFixture {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	fx := &oidcFixture{
		store:            identity.NewStore(testutil.TestDB(t)),
		privateKey:       privateKey,
		signingKey:       privateKey,
		keyID:            "oidc-test-key",
		tokenAudience:    testOIDCClientID,
		tokenExpiry:      time.Now().Add(5 * time.Minute),
		includeIDToken:   true,
		includeUserID:    true,
		userIDClaimValue: "",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", fx.handleDiscovery)
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "authorization is exercised as a redirect only", http.StatusNotImplemented)
	})
	mux.HandleFunc("/token", fx.handleToken)
	mux.HandleFunc("/jwks", fx.handleJWKS)
	fx.server = httptest.NewServer(mux)
	t.Cleanup(fx.server.Close)
	fx.tokenIssuer = fx.server.URL

	cfg := config.OIDCConfig{
		Enabled:      true,
		IssuerURL:    fx.server.URL,
		ClientID:     testOIDCClientID,
		ClientSecret: testOIDCClientSecret,
		RedirectURL:  testOIDCRedirectURL,
		UserIDClaim:  testOIDCUserIDClaim,
	}
	fx.oidc, err = auth.NewOIDCAuth(context.Background(), cfg, fx.store, false, "http://app.example.com",
		auth.WithOIDCDiscoveryBackoff(testOIDCDiscoveryInitialBackoff, testOIDCDiscoveryMaxBackoff))
	if err != nil {
		t.Fatalf("NewOIDCAuth: %v", err)
	}
	if fx.oidc == nil {
		t.Fatal("NewOIDCAuth returned nil for enabled config")
	}
	// Discovery now runs in a background goroutine (see NewOIDCAuth); the
	// issuer here is reachable immediately, so this should resolve on the
	// very first attempt, but we still bound-poll rather than assume a
	// synchronous completion.
	waitOIDCReady(t, fx.oidc)
	return fx
}

// waitOIDCReady bound-polls oa until background issuer discovery has
// completed (HandleLogin stops returning 503), or fails the test. It has no
// visibility into oa's internal readiness state -- by design, that's
// unexported -- so it probes the same way a real client would.
func waitOIDCReady(t *testing.T, oa *auth.OIDCAuth) {
	t.Helper()
	deadline := time.Now().Add(testOIDCReadyPollTimeout)
	for {
		w := httptest.NewRecorder()
		oa.HandleLogin(w, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil))
		if w.Code != http.StatusServiceUnavailable {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("OIDC discovery did not become ready within %s", testOIDCReadyPollTimeout)
		}
		time.Sleep(testOIDCReadyPollInterval)
	}
}

func (fx *oidcFixture) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                fx.server.URL,
		"authorization_endpoint":                fx.server.URL + "/authorize",
		"token_endpoint":                        fx.server.URL + "/token",
		"jwks_uri":                              fx.server.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
	})
}

func (fx *oidcFixture) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       fx.privateKey.Public(),
		KeyID:     fx.keyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}}})
}

func (fx *oidcFixture) handleToken(w http.ResponseWriter, r *http.Request) {
	fx.tokenCalls++
	if fx.tokenStatus != 0 {
		http.Error(w, "token exchange rejected", fx.tokenStatus)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.Form.Get("code") != "valid-code" || r.Form.Get("grant_type") != "authorization_code" {
		http.Error(w, "invalid grant", http.StatusBadRequest)
		return
	}
	clientID, clientSecret, ok := r.BasicAuth()
	if !ok || clientID != testOIDCClientID || clientSecret != testOIDCClientSecret {
		http.Error(w, "invalid client", http.StatusUnauthorized)
		return
	}
	verifier := r.Form.Get("code_verifier")
	digest := sha256.Sum256([]byte(verifier))
	if verifier == "" || base64.RawURLEncoding.EncodeToString(digest[:]) != fx.expectedChallenge {
		http.Error(w, "invalid PKCE verifier", http.StatusBadRequest)
		return
	}

	response := map[string]any{
		"access_token": "opaque-access-token",
		"token_type":   "Bearer",
		"expires_in":   300,
	}
	if fx.includeIDToken {
		response["id_token"] = fx.signIDToken(r.Context())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (fx *oidcFixture) signIDToken(ctx context.Context) string {
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.RS256,
		Key:       jose.JSONWebKey{Key: fx.signingKey, KeyID: fx.keyID},
	}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		panic(err)
	}
	claims := jwt.Claims{
		Issuer:   fx.tokenIssuer,
		Subject:  "tokencanopy-principal-1",
		Audience: jwt.Audience{fx.tokenAudience},
		Expiry:   jwt.NewNumericDate(fx.tokenExpiry),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}
	private := map[string]any{"nonce": fx.tokenNonce}
	if fx.includeUserID {
		value := fx.userIDClaimValue
		if value == "" {
			value = fx.userID
		}
		private[testOIDCUserIDClaim] = value
	}
	token, err := jwt.Signed(signer).Claims(claims).Claims(private).CompactSerialize()
	if err != nil {
		panic(err)
	}
	return token
}

type loginTransaction struct {
	state   string
	nonce   string
	cookies []*http.Cookie
}

func beginOIDCLogin(t *testing.T, fx *oidcFixture) loginTransaction {
	t.Helper()
	return beginOIDCLoginRaw(t, fx, "/api/auth/oidc/login")
}

func beginOIDCLoginRaw(t *testing.T, fx *oidcFixture, target string) loginTransaction {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	fx.oidc.HandleLogin(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	query := location.Query()
	fx.expectedChallenge = query.Get("code_challenge")
	fx.tokenNonce = query.Get("nonce")
	return loginTransaction{state: query.Get("state"), nonce: query.Get("nonce"), cookies: w.Result().Cookies()}
}

func callbackRequest(tx loginTransaction, rawQuery string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?"+rawQuery, nil)
	for _, cookie := range tx.cookies {
		req.AddCookie(cookie)
	}
	return req
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func TestNewOIDCAuthDisabledReturnsNil(t *testing.T) {
	oidcAuth, err := auth.NewOIDCAuth(context.Background(), config.OIDCConfig{}, nil, false, "")
	if err != nil {
		t.Fatalf("NewOIDCAuth disabled: %v", err)
	}
	if oidcAuth != nil {
		t.Fatal("disabled OIDC must return nil so its routes remain absent")
	}
}

// TestNewOIDCAuthConstructsWithUnreachableIssuer covers target behavior 1+2:
// boot never blocks or fails on discovery, and the handler fails closed
// (503) on both routes until discovery succeeds in the background.
func TestNewOIDCAuthConstructsWithUnreachableIssuer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oidcAuth, err := auth.NewOIDCAuth(ctx, config.OIDCConfig{
		Enabled: true, IssuerURL: server.URL, ClientID: "client", ClientSecret: "secret",
		RedirectURL: testOIDCRedirectURL, UserIDClaim: testOIDCUserIDClaim,
	}, nil, false, "",
		auth.WithOIDCDiscoveryBackoff(testOIDCDiscoveryInitialBackoff, testOIDCDiscoveryMaxBackoff))
	if err != nil {
		t.Fatalf("NewOIDCAuth must not fail synchronously when the issuer is unreachable: %v", err)
	}
	if oidcAuth == nil {
		t.Fatal("enabled OIDC must return a non-nil handler even before discovery completes")
	}

	loginW := httptest.NewRecorder()
	oidcAuth.HandleLogin(loginW, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil))
	if loginW.Code != http.StatusServiceUnavailable {
		t.Fatalf("HandleLogin status = %d, want 503 while discovery is unreachable", loginW.Code)
	}

	callbackW := httptest.NewRecorder()
	oidcAuth.HandleCallback(callbackW, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?code=x&state=y", nil))
	if callbackW.Code != http.StatusServiceUnavailable {
		t.Fatalf("HandleCallback status = %d, want 503 while discovery is unreachable", callbackW.Code)
	}
}

func TestOIDCDiscoveryTimesOutStalledAttemptAndRetries(t *testing.T) {
	var attempts atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 server.URL,
			"authorization_endpoint": server.URL + "/authorize",
			"token_endpoint":         server.URL + "/token",
			"jwks_uri":               server.URL + "/jwks",
		})
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	oidcAuth, err := auth.NewOIDCAuth(ctx, config.OIDCConfig{
		Enabled: true, IssuerURL: server.URL, ClientID: "client", ClientSecret: "secret",
		RedirectURL: testOIDCRedirectURL, UserIDClaim: testOIDCUserIDClaim,
	}, nil, false, "",
		auth.WithOIDCDiscoveryBackoff(testOIDCDiscoveryInitialBackoff, testOIDCDiscoveryMaxBackoff),
		auth.WithOIDCDiscoveryAttemptTimeout(25*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewOIDCAuth: %v", err)
	}

	waitOIDCReady(t, oidcAuth)
	if got := attempts.Load(); got < 2 {
		t.Fatalf("discovery attempts = %d, want at least 2 after stalled attempt timed out", got)
	}
}

func TestOIDCUnavailableHandlersDoNotLogPerRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	oidcAuth, err := auth.NewOIDCAuth(ctx, config.OIDCConfig{
		Enabled: true, IssuerURL: server.URL, ClientID: "client", ClientSecret: "secret",
		RedirectURL: testOIDCRedirectURL, UserIDClaim: testOIDCUserIDClaim,
	}, nil, false, "",
		auth.WithOIDCDiscoveryBackoff(testOIDCDiscoveryInitialBackoff, testOIDCDiscoveryMaxBackoff),
		auth.WithOIDCDiscoveryDone(done),
	)
	if err != nil {
		t.Fatalf("NewOIDCAuth: %v", err)
	}

	oidcAuth.HandleLogin(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil))
	oidcAuth.HandleCallback(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback", nil))
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("discovery goroutine did not exit after cancellation")
	}

	got := logs.String()
	if strings.Contains(got, "OIDC login rejected") || strings.Contains(got, "OIDC callback rejected") {
		t.Fatalf("unavailable handlers logged per-request messages: %q", got)
	}
}

// TestOIDCBecomesReadyAfterDiscoverySucceeds covers target behavior 3: once
// the issuer becomes reachable, the background retry loop discovers it and
// the handler transitions from failing closed to serving the normal flow,
// with no restart or reconstruction required.
func TestOIDCBecomesReadyAfterDiscoverySucceeds(t *testing.T) {
	fx := &oidcFixture{store: identity.NewStore(testutil.TestDB(t))}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	fx.privateKey = privateKey
	fx.signingKey = privateKey
	fx.keyID = "oidc-retry-test-key"
	fx.tokenAudience = testOIDCClientID
	fx.tokenExpiry = time.Now().Add(5 * time.Minute)
	fx.includeIDToken = true
	fx.includeUserID = true
	// Zero value of the `any` field is nil, not "" -- signIDToken's
	// `value == ""` fallback to fx.userID only fires when this is
	// explicitly the empty string (mirrors setupOIDC's fixture init).
	fx.userIDClaimValue = ""

	var discoverable atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if !discoverable.Load() {
			http.Error(w, "issuer not yet available", http.StatusServiceUnavailable)
			return
		}
		fx.handleDiscovery(w, r)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "authorization is exercised as a redirect only", http.StatusNotImplemented)
	})
	mux.HandleFunc("/token", fx.handleToken)
	mux.HandleFunc("/jwks", fx.handleJWKS)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	fx.server = server
	fx.tokenIssuer = server.URL

	cfg := config.OIDCConfig{
		Enabled: true, IssuerURL: server.URL, ClientID: testOIDCClientID,
		ClientSecret: testOIDCClientSecret, RedirectURL: testOIDCRedirectURL,
		UserIDClaim: testOIDCUserIDClaim,
	}
	oidcAuth, err := auth.NewOIDCAuth(context.Background(), cfg, fx.store, false, "http://app.example.com",
		auth.WithOIDCDiscoveryBackoff(testOIDCDiscoveryInitialBackoff, testOIDCDiscoveryMaxBackoff))
	if err != nil {
		t.Fatalf("NewOIDCAuth: %v", err)
	}
	fx.oidc = oidcAuth

	// The issuer is deliberately unreachable at construction: the handler
	// must be unready and fail closed rather than partially serve.
	w := httptest.NewRecorder()
	oidcAuth.HandleLogin(w, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("HandleLogin status = %d, want 503 before discovery succeeds", w.Code)
	}

	// Now let discovery succeed and wait (bounded) for the background retry
	// loop to pick it up.
	discoverable.Store(true)
	waitOIDCReady(t, oidcAuth)

	user, err := fx.store.CreateOrGetUser(context.Background(), "retry@example.com", "Retry", "google-sub-retry")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	fx.userID = user.ID

	tx := beginOIDCLogin(t, fx)
	callbackW := httptest.NewRecorder()
	fx.oidc.HandleCallback(callbackW, callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state)))
	if callbackW.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", callbackW.Code, callbackW.Body.String())
	}
	session := findCookie(callbackW.Result().Cookies(), auth.SessionCookieName)
	if session == nil || session.Value == "" {
		t.Fatal("expected non-empty e2a session cookie once ready")
	}
}

// TestOIDCDiscoveryGoroutineExitsOnContextCancel covers target behavior 1's
// lifecycle requirement: the retry goroutine must stop (not leak) when the
// ctx passed to NewOIDCAuth is cancelled, e.g. on server shutdown.
func TestOIDCDiscoveryGoroutineExitsOnContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	_, err := auth.NewOIDCAuth(ctx, config.OIDCConfig{
		Enabled: true, IssuerURL: server.URL, ClientID: "client", ClientSecret: "secret",
		RedirectURL: testOIDCRedirectURL, UserIDClaim: testOIDCUserIDClaim,
	}, nil, false, "",
		auth.WithOIDCDiscoveryBackoff(testOIDCDiscoveryInitialBackoff, testOIDCDiscoveryMaxBackoff),
		auth.WithOIDCDiscoveryDone(done),
	)
	if err != nil {
		t.Fatalf("NewOIDCAuth: %v", err)
	}

	select {
	case <-done:
		t.Fatal("discovery goroutine exited before ctx was cancelled")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("discovery goroutine did not exit within 2s of ctx cancellation")
	}
}

func TestOIDCLoginUsesStateNonceAndPKCE(t *testing.T) {
	fx := setupOIDC(t)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil)
	w := httptest.NewRecorder()
	fx.oidc.HandleLogin(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	query := location.Query()
	for key, want := range map[string]string{
		"response_type":         "code",
		"client_id":             testOIDCClientID,
		"redirect_uri":          testOIDCRedirectURL,
		"scope":                 "openid",
		"code_challenge_method": "S256",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"state", "nonce", "code_challenge"} {
		if query.Get(key) == "" {
			t.Errorf("missing %s", key)
		}
	}
	for _, name := range []string{"e2a_oidc_state", "e2a_oidc_nonce", "e2a_oidc_verifier"} {
		cookie := findCookie(w.Result().Cookies(), name)
		if cookie == nil {
			t.Errorf("missing transaction cookie %s", name)
			continue
		}
		if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge <= 0 {
			t.Errorf("unsafe transaction cookie %s: %+v", name, cookie)
		}
	}
}

func TestOIDCCallbackEstablishesSessionForExistingUser(t *testing.T) {
	fx := setupOIDC(t)
	user, err := fx.store.CreateOrGetUser(context.Background(), "existing@example.com", "Existing", "google-sub-existing")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	fx.userID = user.ID
	tx := beginOIDCLogin(t, fx)

	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state)))
	if w.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "http://app.example.com/dashboard" {
		t.Errorf("Location = %q", got)
	}
	session := findCookie(w.Result().Cookies(), auth.SessionCookieName)
	if session == nil || session.Value == "" {
		t.Fatal("expected non-empty e2a session cookie")
	}
	sessionUser, err := fx.store.GetUserSession(context.Background(), session.Value)
	if err != nil {
		t.Fatalf("GetUserSession: %v", err)
	}
	if sessionUser.ID != user.ID {
		t.Errorf("session user = %s, want %s", sessionUser.ID, user.ID)
	}
	for _, name := range []string{"e2a_oidc_state", "e2a_oidc_nonce", "e2a_oidc_verifier"} {
		cookie := findCookie(w.Result().Cookies(), name)
		if cookie == nil || cookie.MaxAge >= 0 {
			t.Errorf("transaction cookie %s was not deleted", name)
		}
	}
}

func TestOIDCCallbackRejectsStateMismatchBeforeExchange(t *testing.T) {
	fx := setupOIDC(t)
	tx := beginOIDCLogin(t, fx)
	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, callbackRequest(tx, "code=valid-code&state=attacker-state"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if fx.tokenCalls != 0 {
		t.Fatalf("token endpoint called %d times before state validation", fx.tokenCalls)
	}
}

func TestOIDCCallbackRejectsMissingTransactionCookie(t *testing.T) {
	fx := setupOIDC(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?code=valid-code&state=state", nil)
	fx.oidc.HandleCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestOIDCCallbackRejectsProviderErrorAndDeletesCookies(t *testing.T) {
	fx := setupOIDC(t)
	tx := beginOIDCLogin(t, fx)
	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, callbackRequest(tx, "error=access_denied&state="+url.QueryEscape(tx.state)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if findCookie(w.Result().Cookies(), "e2a_oidc_state") == nil {
		t.Fatal("provider error must clear transaction cookies")
	}
}

func TestOIDCCallbackRejectsMissingCode(t *testing.T) {
	fx := setupOIDC(t)
	tx := beginOIDCLogin(t, fx)
	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, callbackRequest(tx, "state="+url.QueryEscape(tx.state)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestOIDCCallbackRejectsTokenExchangeFailure(t *testing.T) {
	fx := setupOIDC(t)
	fx.tokenStatus = http.StatusBadRequest
	tx := beginOIDCLogin(t, fx)
	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestOIDCCallbackRejectsMissingIDToken(t *testing.T) {
	fx := setupOIDC(t)
	fx.includeIDToken = false
	tx := beginOIDCLogin(t, fx)
	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestOIDCCallbackRejectsInvalidIDTokens(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *oidcFixture, loginTransaction)
	}{
		{name: "signature", mutate: func(t *testing.T, fx *oidcFixture, _ loginTransaction) {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			fx.signingKey = key
		}},
		{name: "issuer", mutate: func(_ *testing.T, fx *oidcFixture, _ loginTransaction) {
			fx.tokenIssuer = "https://wrong-issuer.example"
		}},
		{name: "audience", mutate: func(_ *testing.T, fx *oidcFixture, _ loginTransaction) { fx.tokenAudience = "wrong-client" }},
		{name: "expiry", mutate: func(_ *testing.T, fx *oidcFixture, _ loginTransaction) { fx.tokenExpiry = time.Now().Add(-time.Hour) }},
		{name: "nonce", mutate: func(_ *testing.T, fx *oidcFixture, _ loginTransaction) { fx.tokenNonce = "wrong-nonce" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fx := setupOIDC(t)
			tx := beginOIDCLogin(t, fx)
			test.mutate(t, fx, tx)
			w := httptest.NewRecorder()
			fx.oidc.HandleCallback(w, callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state)))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			if findCookie(w.Result().Cookies(), auth.SessionCookieName) != nil {
				t.Fatal("invalid ID token established a session")
			}
		})
	}
}

func TestOIDCCallbackRejectsInvalidUserClaims(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*oidcFixture)
	}{
		{name: "missing", mutate: func(fx *oidcFixture) { fx.includeUserID = false }},
		{name: "empty", mutate: func(fx *oidcFixture) { fx.userIDClaimValue = " " }},
		{name: "wrong type", mutate: func(fx *oidcFixture) { fx.userIDClaimValue = 42 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fx := setupOIDC(t)
			test.mutate(fx)
			tx := beginOIDCLogin(t, fx)
			w := httptest.NewRecorder()
			fx.oidc.HandleCallback(w, callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state)))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
		})
	}
}

func TestOIDCCallbackRejectsUnknownUserWithoutProvisioning(t *testing.T) {
	fx := setupOIDC(t)
	fx.userID = "usr_does_not_exist"
	tx := beginOIDCLogin(t, fx)
	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if _, err := fx.store.GetUserByID(context.Background(), fx.userID); err == nil {
		t.Fatal("OIDC callback must never provision an unknown user")
	}
}

func TestOIDCLoginUsesSecureCookiesInProduction(t *testing.T) {
	fx := setupOIDC(t)
	cfg := config.OIDCConfig{
		Enabled: true, IssuerURL: fx.server.URL, ClientID: testOIDCClientID,
		ClientSecret: testOIDCClientSecret, RedirectURL: testOIDCRedirectURL,
		UserIDClaim: testOIDCUserIDClaim,
	}
	oidcAuth, err := auth.NewOIDCAuth(context.Background(), cfg, fx.store, true, "https://app.example.com",
		auth.WithOIDCDiscoveryBackoff(testOIDCDiscoveryInitialBackoff, testOIDCDiscoveryMaxBackoff))
	if err != nil {
		t.Fatal(err)
	}
	waitOIDCReady(t, oidcAuth)
	w := httptest.NewRecorder()
	oidcAuth.HandleLogin(w, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil))
	for _, cookie := range w.Result().Cookies() {
		if strings.HasPrefix(cookie.Name, "e2a_oidc_") && !cookie.Secure {
			t.Errorf("production transaction cookie %s is not Secure", cookie.Name)
		}
	}
}

// decodeResumeCookieValue decodes the e2a_oidc_resume transaction cookie the
// way the callback does, so tests can assert what HandleLogin stashed.
func decodeResumeCookieValue(t *testing.T, cookie *http.Cookie) struct {
	ReturnTo    string `json:"rt"`
	CLICallback string `json:"cb"`
	CLIState    string `json:"cs"`
} {
	t.Helper()
	var resume struct {
		ReturnTo    string `json:"rt"`
		CLICallback string `json:"cb"`
		CLIState    string `json:"cs"`
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		t.Fatalf("resume cookie is not base64url: %v", err)
	}
	if err := json.Unmarshal(raw, &resume); err != nil {
		t.Fatalf("resume cookie is not JSON: %v", err)
	}
	return resume
}

// TestOIDCLoginCarriesReturnToInResumeCookie mirrors the legacy door's
// TestHandleLogin_EncodesReturnToInOAuthState: a valid return_to survives
// the round trip for HandleCallback to honor, without leaking into the
// provider-facing authorize URL (the OIDC state stays an opaque random
// string; the value rides the HttpOnly resume cookie instead).
func TestOIDCLoginCarriesReturnToInResumeCookie(t *testing.T) {
	fx := setupOIDC(t)

	returnTo := "/oauth2/authorize?client_id=mcp_abc&response_type=code&state=xyz"
	tx := beginOIDCLoginRaw(t, fx, "/api/auth/oidc/login?return_to="+url.QueryEscape(returnTo))

	if strings.Contains(tx.state, "oauth2") {
		t.Errorf("return_to leaked into the OIDC state parameter: %q", tx.state)
	}
	cookie := findCookie(tx.cookies, "e2a_oidc_resume")
	if cookie == nil {
		t.Fatal("resume cookie not set for return_to login")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge <= 0 {
		t.Errorf("unsafe resume cookie: %+v", cookie)
	}
	resume := decodeResumeCookieValue(t, cookie)
	if resume.ReturnTo != returnTo {
		t.Errorf("resume return_to = %q, want %q", resume.ReturnTo, returnTo)
	}
	if resume.CLICallback != "" || resume.CLIState != "" {
		t.Errorf("web login resume should not contain CLI params, got cb=%q cs=%q", resume.CLICallback, resume.CLIState)
	}
}

// TestOIDCLoginRejectsReturnToOutsideAllowList applies the legacy door's
// exact reject list (TestHandleLogin_RejectsReturnToOutsideAllowList):
// every value the allow-list refuses must 400 the login, before any
// transaction state is created — never silently strip to /dashboard.
func TestOIDCLoginRejectsReturnToOutsideAllowList(t *testing.T) {
	fx := setupOIDC(t)

	bad := []string{
		"/dashboard",                         // wrong prefix
		"/api/v1/agents",                     // wrong prefix
		"https://evil.com/oauth2/authorize",  // absolute
		"//evil.com/oauth2/authorize",        // protocol-relative
		"/oauth2/authorize\nSet-Cookie: x=y", // header injection
		"\\api\\oauth\\authorize",            // backslash bypass
		"http://localhost/oauth2/authorize",  // scheme present
		"/oauth2/../../dashboard",            // path traversal escaping the allow-list
		"/oauth2/../v1/agents",               // path traversal into another API surface
		"/oauth2//evil.com/path",             // empty segment after prefix
	}
	for _, rt := range bad {
		t.Run(rt, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login?return_to="+url.QueryEscape(rt), nil)
			w := httptest.NewRecorder()
			fx.oidc.HandleLogin(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("return_to=%q: status=%d, want 400", rt, w.Code)
			}
			if findCookie(w.Result().Cookies(), "e2a_oidc_state") != nil {
				t.Errorf("return_to=%q: transaction cookies were created for a rejected login", rt)
			}
		})
	}
}

// TestOIDCLoginRequiresCLIParamsTogether mirrors the legacy door: the
// cli_callback/cli_state pair is all-or-nothing, a lone value is a 400.
func TestOIDCLoginRequiresCLIParamsTogether(t *testing.T) {
	fx := setupOIDC(t)

	for _, target := range []string{
		"/api/auth/oidc/login?cli_callback=" + url.QueryEscape("http://127.0.0.1:43123/callback"),
		"/api/auth/oidc/login?cli_state=cli_state_123",
	} {
		w := httptest.NewRecorder()
		fx.oidc.HandleLogin(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", target, w.Code)
		}
		if !strings.Contains(w.Body.String(), "cli_callback and cli_state must be provided together") {
			t.Fatalf("%s: unexpected error body: %q", target, w.Body.String())
		}
	}
}

// TestOIDCLoginRejectsNonLoopbackCLICallback mirrors the legacy door's
// TestHandleLogin_RejectsInvalidCLICallback: anything but a loopback http
// URL must 400 the login.
func TestOIDCLoginRejectsNonLoopbackCLICallback(t *testing.T) {
	fx := setupOIDC(t)

	bad := []string{
		"https://example.com/callback",   // non-loopback host
		"http://example.com/callback",    // loopback scheme, remote host
		"ftp://127.0.0.1/callback",       // non-http scheme
		"http://user@127.0.0.1/callback", // user info present
	}
	for _, cb := range bad {
		t.Run(cb, func(t *testing.T) {
			target := "/api/auth/oidc/login?cli_callback=" + url.QueryEscape(cb) + "&cli_state=cli_state_123"
			w := httptest.NewRecorder()
			fx.oidc.HandleLogin(w, httptest.NewRequest(http.MethodGet, target, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("cli_callback=%q: status=%d, want 400", cb, w.Code)
			}
		})
	}
}

// TestOIDCCallbackHonorsReturnTo mirrors the legacy door's
// TestHandleCallback_ReturnTo_BouncesUser: a successful callback whose
// validated return_to is present redirects there instead of /dashboard.
func TestOIDCCallbackHonorsReturnTo(t *testing.T) {
	fx := setupOIDC(t)
	user, err := fx.store.CreateOrGetUser(context.Background(), "returnto@example.com", "Return To", "google-sub-returnto")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	fx.userID = user.ID

	returnTo := "/oauth2/authorize?client_id=mcp_abc&state=xyz"
	tx := beginOIDCLoginRaw(t, fx, "/api/auth/oidc/login?return_to="+url.QueryEscape(returnTo))

	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state)))
	if w.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	if got, want := w.Header().Get("Location"), "http://app.example.com"+returnTo; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if session := findCookie(w.Result().Cookies(), auth.SessionCookieName); session == nil || session.Value == "" {
		t.Error("expected non-empty e2a session cookie")
	}
	if cookie := findCookie(w.Result().Cookies(), "e2a_oidc_resume"); cookie == nil || cookie.MaxAge >= 0 {
		t.Error("resume cookie was not deleted after callback")
	}
}

// TestOIDCCallbackCLILoginHandsOffToCLI mirrors the legacy door's
// TestHandleCallback_CLILogin_HandsOffToCLI: a login initiated with a valid
// loopback cli_callback/cli_state pair renders the auto-submitting handoff
// page carrying the callback URL, the CLI state, and a fresh API key.
func TestOIDCCallbackCLILoginHandsOffToCLI(t *testing.T) {
	fx := setupOIDC(t)
	user, err := fx.store.CreateOrGetUser(context.Background(), "cli@example.com", "CLI", "google-sub-cli")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	fx.userID = user.ID

	target := "/api/auth/oidc/login?cli_callback=" + url.QueryEscape("http://127.0.0.1:43123/callback") + "&cli_state=cli_state_abc"
	tx := beginOIDCLoginRaw(t, fx, target)

	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state)))
	if w.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"http://127.0.0.1:43123/callback", "cli_state_abc", "api_key"} {
		if !strings.Contains(body, want) {
			t.Errorf("handoff page missing %q, body: %s", want, body)
		}
	}
	if session := findCookie(w.Result().Cookies(), auth.SessionCookieName); session == nil || session.Value == "" {
		t.Error("expected non-empty e2a session cookie")
	}
	if cookie := findCookie(w.Result().Cookies(), "e2a_oidc_resume"); cookie == nil || cookie.MaxAge >= 0 {
		t.Error("resume cookie was not deleted after callback")
	}
}

// TestOIDCLoginClearsStaleResumeParams: the resume cookie is refreshed on
// every login, so instructions from an earlier, abandoned transaction
// cannot leak into a fresh param-less one — the fresh login clears it.
func TestOIDCLoginClearsStaleResumeParams(t *testing.T) {
	fx := setupOIDC(t)

	beginOIDCLoginRaw(t, fx, "/api/auth/oidc/login?return_to="+url.QueryEscape("/oauth2/authorize"))

	w := httptest.NewRecorder()
	fx.oidc.HandleLogin(w, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	cookie := findCookie(w.Result().Cookies(), "e2a_oidc_resume")
	if cookie == nil || cookie.MaxAge >= 0 {
		t.Fatalf("param-less login must clear the resume cookie, got %+v", cookie)
	}
}

// TestOIDCCallbackIgnoresCorruptResumeCookie: a resume cookie that does not
// decode degrades to "no post-login instructions" — the login still
// completes and lands on /dashboard (the cookie carries no security
// material; the state/nonce cookies own the CSRF binding).
func TestOIDCCallbackIgnoresCorruptResumeCookie(t *testing.T) {
	fx := setupOIDC(t)
	user, err := fx.store.CreateOrGetUser(context.Background(), "corrupt@example.com", "Corrupt", "google-sub-corrupt")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	fx.userID = user.ID

	tx := beginOIDCLogin(t, fx)
	req := callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state))
	req.AddCookie(&http.Cookie{Name: "e2a_oidc_resume", Value: "!!!not-a-resume!!!"})

	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	if got, want := w.Header().Get("Location"), "http://app.example.com/dashboard"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}
