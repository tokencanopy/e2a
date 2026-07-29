package auth_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"golang.org/x/oauth2"
)

func TestHandleLogin_EncodesCliParamsInOAuthState(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/auth/login?cli_callback=http://127.0.0.1:43123/callback&cli_state=cli_state_123",
		nil,
	)
	w := httptest.NewRecorder()

	ua.HandleLogin(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}

	res := w.Result()
	location := res.Header.Get("Location")
	if !strings.Contains(location, "accounts.google.com") {
		t.Fatalf("redirect location = %q, want Google OAuth URL", location)
	}

	// Parse the OAuth state parameter from the redirect URL
	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	stateParam := u.Query().Get("state")
	if stateParam == "" {
		t.Fatal("state parameter not set in redirect URL")
	}

	// Decode and verify the state contains CLI params
	stateJSON, err := base64.URLEncoding.DecodeString(stateParam)
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	var state struct {
		Nonce       string `json:"n"`
		CLICallback string `json:"cb"`
		CLIState    string `json:"cs"`
	}
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.Nonce == "" {
		t.Fatal("nonce not set in state")
	}
	if state.CLICallback != "http://127.0.0.1:43123/callback" {
		t.Fatalf("cli callback = %q, want %q", state.CLICallback, "http://127.0.0.1:43123/callback")
	}
	if state.CLIState != "cli_state_123" {
		t.Fatalf("cli state = %q, want %q", state.CLIState, "cli_state_123")
	}

	// Verify the nonce cookie is still set (for CSRF verification in callback)
	var sawNonceCookie bool
	for _, cookie := range res.Cookies() {
		if cookie.Name == "e2a_oauth_state" && cookie.Value == state.Nonce {
			sawNonceCookie = true
		}
	}
	if !sawNonceCookie {
		t.Fatal("oauth state nonce cookie not set")
	}
}

func TestHandleLogin_WebLoginOmitsCliParams(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/login", nil)
	w := httptest.NewRecorder()
	ua.HandleLogin(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}

	u, _ := url.Parse(w.Result().Header.Get("Location"))
	stateParam := u.Query().Get("state")
	stateJSON, _ := base64.URLEncoding.DecodeString(stateParam)
	var state struct {
		CLICallback string `json:"cb"`
		CLIState    string `json:"cs"`
	}
	json.Unmarshal(stateJSON, &state)
	if state.CLICallback != "" || state.CLIState != "" {
		t.Fatalf("web login should not contain CLI params, got cb=%q cs=%q", state.CLICallback, state.CLIState)
	}
}

// TestHandleLogin_EncodesReturnToInOAuthState: /api/auth/login?return_to=
// /oauth2/authorize?... encodes that path into the Google OAuth state
// so HandleCallback can bounce the user back into the MCP authorize flow.
func TestHandleLogin_EncodesReturnToInOAuthState(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	returnTo := "/oauth2/authorize?client_id=mcp_abc&response_type=code&state=xyz"
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/auth/login?return_to="+url.QueryEscape(returnTo),
		nil,
	)
	w := httptest.NewRecorder()

	ua.HandleLogin(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}

	loc, _ := url.Parse(w.Result().Header.Get("Location"))
	stateParam := loc.Query().Get("state")
	stateJSON, _ := base64.URLEncoding.DecodeString(stateParam)
	var state struct {
		ReturnTo string `json:"rt"`
	}
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.ReturnTo != returnTo {
		t.Errorf("return_to = %q, want %q", state.ReturnTo, returnTo)
	}
}

func TestHandleLogin_EncodesReviewReturnToInOAuthState(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	returnTo := "/reviews?id=msg_held"
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/auth/login?return_to="+url.QueryEscape(returnTo),
		nil,
	)
	w := httptest.NewRecorder()

	ua.HandleLogin(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusFound, w.Body.String())
	}
	loc, _ := url.Parse(w.Result().Header.Get("Location"))
	stateParam := loc.Query().Get("state")
	stateJSON, _ := base64.URLEncoding.DecodeString(stateParam)
	var state struct {
		ReturnTo string `json:"rt"`
	}
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.ReturnTo != returnTo {
		t.Errorf("return_to = %q, want %q", state.ReturnTo, returnTo)
	}
}

// TestHandleLogin_RejectsReturnToOutsideAllowList: every value the
// allow-list refuses must produce 400 rather than silently strip — a
// silent strip would land the user on /dashboard, leaving the original
// flow stuck without a visible error.
func TestHandleLogin_RejectsReturnToOutsideAllowList(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	bad := []string{
		"/dashboard",                         // unrelated dashboard route
		"/reviews/other",                     // review allow-list is exact
		"/reviews/../dashboard",              // review path traversal
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
			req := httptest.NewRequest(
				http.MethodGet,
				"/api/auth/login?return_to="+url.QueryEscape(rt),
				nil,
			)
			w := httptest.NewRecorder()
			ua.HandleLogin(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("return_to=%q: status=%d, want 400", rt, w.Code)
			}
		})
	}
}

func TestHandleLogin_RejectsInvalidCLICallback(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/auth/login?cli_callback=https://example.com/callback&cli_state=cli_state_123",
		nil,
	)
	w := httptest.NewRecorder()

	ua.HandleLogin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "loopback") && !strings.Contains(w.Body.String(), "http") {
		t.Fatalf("unexpected error body: %q", w.Body.String())
	}
}

// fakeGoogleOAuth starts a test server that mimics Google's token and userinfo
// endpoints. Returns the server and an oauth2.Config pointing at it.
func fakeGoogleOAuth(t *testing.T) (*httptest.Server, *oauth2.Config) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"sub":            "google-sub-cli-test",
			"email":          "cliuser@test.com",
			"email_verified": true,
			"name":           "CLI User",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oauthCfg := &oauth2.Config{
		ClientID:     "test",
		ClientSecret: "test",
		RedirectURL:  "http://localhost/api/auth/callback",
		Endpoint: oauth2.Endpoint{
			TokenURL: srv.URL + "/token",
			AuthURL:  srv.URL + "/authorize",
		},
		Scopes: []string{"openid", "email", "profile"},
	}
	return srv, oauthCfg
}

// setupUserAuthWithFakeOAuth creates a UserAuth backed by a fake Google OAuth
// server so we can test HandleCallback without hitting real Google.
func setupUserAuthWithFakeOAuth(t *testing.T) (*auth.UserAuth, *identity.Store, *httptest.Server) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)

	srv, oauthCfg := fakeGoogleOAuth(t)

	cfg := &config.OAuthConfig{
		GoogleClientID:     oauthCfg.ClientID,
		GoogleClientSecret: oauthCfg.ClientSecret,
		RedirectURL:        oauthCfg.RedirectURL,
	}
	ua := auth.NewUserAuthWithOAuthConfig(cfg, oauthCfg, store, false, srv.URL+"/userinfo")

	return ua, store, srv
}

func TestHandleCallback_CLILogin_HandsOffToCLI(t *testing.T) {
	ua, _, srv := setupUserAuthWithFakeOAuth(t)

	nonce := "test-nonce-123"
	state := auth.EncodeOAuthState(&auth.OAuthState{
		Nonce:       nonce,
		CLICallback: "http://127.0.0.1:43123/callback",
		CLIState:    "cli_state_abc",
	})

	req := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/auth/callback?code=fake-code&state=%s", url.QueryEscape(state)),
		nil,
	)
	req.AddCookie(&http.Cookie{Name: "e2a_oauth_state", Value: nonce})
	_ = srv // keep server alive
	w := httptest.NewRecorder()

	ua.HandleCallback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()
	// The CLI handoff page should contain the callback URL and CLI state
	if !strings.Contains(body, "http://127.0.0.1:43123/callback") {
		t.Fatalf("handoff page missing callback URL, body: %s", body)
	}
	if !strings.Contains(body, "cli_state_abc") {
		t.Fatalf("handoff page missing cli_state, body: %s", body)
	}
	if !strings.Contains(body, "api_key") {
		t.Fatalf("handoff page missing api_key, body: %s", body)
	}
}

func TestHandleCallback_WebLogin_RedirectsToDashboard(t *testing.T) {
	ua, _, srv := setupUserAuthWithFakeOAuth(t)

	nonce := "test-nonce-456"
	state := auth.EncodeOAuthState(&auth.OAuthState{
		Nonce: nonce,
	})

	req := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/auth/callback?code=fake-code&state=%s", url.QueryEscape(state)),
		nil,
	)
	req.AddCookie(&http.Cookie{Name: "e2a_oauth_state", Value: nonce})
	_ = srv
	w := httptest.NewRecorder()

	ua.HandleCallback(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusFound, w.Body.String())
	}

	location := w.Header().Get("Location")
	if !strings.Contains(location, "/dashboard") {
		t.Fatalf("expected redirect to dashboard, got %q", location)
	}
}

// TestHandleCallback_ReturnTo_BouncesUser: a successful callback whose
// state.ReturnTo passes the allow-list redirects there instead of
// /dashboard, so the MCP /authorize flow can resume on the now-
// authenticated request.
func TestHandleCallback_ReturnTo_BouncesUser(t *testing.T) {
	ua, _, srv := setupUserAuthWithFakeOAuth(t)

	nonce := "nonce-rt-bounce"
	returnTo := "/oauth2/authorize?client_id=mcp_abc&state=xyz"
	state := auth.EncodeOAuthState(&auth.OAuthState{
		Nonce:    nonce,
		ReturnTo: returnTo,
	})

	req := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/auth/callback?code=fake-code&state=%s", url.QueryEscape(state)),
		nil,
	)
	req.AddCookie(&http.Cookie{Name: "e2a_oauth_state", Value: nonce})
	_ = srv
	w := httptest.NewRecorder()

	ua.HandleCallback(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasSuffix(loc, returnTo) {
		t.Fatalf("Location = %q, want suffix %q", loc, returnTo)
	}
	if strings.Contains(loc, "/dashboard") {
		t.Errorf("ReturnTo path should preempt the /dashboard fallback: got %q", loc)
	}
}

func TestHandleCallback_NonceMismatch_Rejected(t *testing.T) {
	ua, _, srv := setupUserAuthWithFakeOAuth(t)

	state := auth.EncodeOAuthState(&auth.OAuthState{
		Nonce:       "correct-nonce",
		CLICallback: "http://127.0.0.1:43123/callback",
		CLIState:    "cli_state_abc",
	})

	req := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/auth/callback?code=fake-code&state=%s", url.QueryEscape(state)),
		nil,
	)
	req.AddCookie(&http.Cookie{Name: "e2a_oauth_state", Value: "wrong-nonce"})
	_ = srv
	w := httptest.NewRecorder()

	ua.HandleCallback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleCallback_InvalidState_Rejected(t *testing.T) {
	ua, _, srv := setupUserAuthWithFakeOAuth(t)

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/auth/callback?code=fake-code&state=not-valid-base64!!!",
		nil,
	)
	req.AddCookie(&http.Cookie{Name: "e2a_oauth_state", Value: "whatever"})
	_ = srv
	w := httptest.NewRecorder()

	ua.HandleCallback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
