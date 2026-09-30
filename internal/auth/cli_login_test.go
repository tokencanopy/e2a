package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

func TestHandleLogin_RequestsGoogleAccountChooser(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/login", nil)
	w := httptest.NewRecorder()
	ua.HandleLogin(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}

	location := w.Result().Header.Get("Location")
	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}

	query := u.Query()
	for key, want := range map[string]string{
		"prompt":        "select_account",
		"client_id":     "test",
		"redirect_uri":  "http://localhost/api/auth/callback",
		"response_type": "code",
		"scope":         "openid email profile",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if query.Get("state") == "" {
		t.Error("state parameter missing from redirect URL")
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

func TestHandleLogin_EncodesInboxThreadReturnToInOAuthState(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	returnTo := "/inboxes/messages?email=bot%40example.com#conv:%E5%AE%A2"
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
}

func TestHandleLogin_EncodesGetStartedReturnToInOAuthState(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	returnTo := "/get-started?step=address"
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
		"/get-started/other",                 // sender setup allow-list is exact
		"/get-started/../dashboard",          // sender setup path traversal
		"/reviews/other",                     // review allow-list is exact
		"/reviews/../dashboard",              // review path traversal
		"/inboxes/messages/view",             // inbox allow-list is exact
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

// TestHandleCallback_RecordsOwnerEmailProof: a verified Google login records
// owner-mailbox proof bound to the exact verified address; bootstrap and
// provisioned subjects never get it through the store helper.
func TestHandleCallback_RecordsOwnerEmailProof(t *testing.T) {
	ua, store, srv := setupUserAuthWithFakeOAuth(t)
	_ = srv
	nonce := "test-nonce-proof"
	state := auth.EncodeOAuthState(&auth.OAuthState{Nonce: nonce})
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/auth/callback?code=fake-code&state=%s", url.QueryEscape(state)), nil)
	req.AddCookie(&http.Cookie{Name: "e2a_oauth_state", Value: nonce})
	w := httptest.NewRecorder()
	ua.HandleCallback(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}

	pool, err := pgxpool.New(context.Background(), testutil.TestDBURL())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var address, source string
	var at *time.Time
	if err := pool.QueryRow(context.Background(), `
		SELECT owner_email_verified_address, owner_email_verified_source, owner_email_verified_at
		  FROM users WHERE google_subject = 'google-sub-cli-test'`).Scan(&address, &source, &at); err != nil {
		t.Fatalf("read proof: %v", err)
	}
	if address != "cliuser@test.com" || source != "google_oauth" || at == nil {
		t.Fatalf("proof = %q/%q/%v", address, source, at)
	}

	boot, err := store.BootstrapUser(context.Background(), "boot@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.RecordGoogleOwnerEmailProof(context.Background(), boot.ID, boot.GoogleSubject, boot.Email); err != nil || ok {
		t.Fatalf("a bootstrap subject must never receive proof: ok=%v err=%v", ok, err)
	}
	// A subject/email mismatch writes nothing either.
	var userID string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE google_subject = 'google-sub-cli-test'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.RecordGoogleOwnerEmailProof(context.Background(), userID, "google-sub-cli-test", "someone-else@example.test"); err != nil || ok {
		t.Fatalf("a mismatched address must not be recorded: ok=%v err=%v", ok, err)
	}
}

// TestHandleLogin_EncodesSanitizedDeviceNameInOAuthState: device_name rides
// the OAuth state alongside cli_callback/cli_state, sanitized per
// sanitizeDeviceName's contract (printable ASCII only, capped at 64 runes).
func TestHandleLogin_EncodesSanitizedDeviceNameInOAuthState(t *testing.T) {
	raw := "joshzhang-MBP\x07\x1b[31m" + strings.Repeat("x", 100) // \x07/\x1b are control bytes and dropped;
	// "[31m" is ordinary printable text (the rest of what would be an ANSI
	// color escape, minus its non-printable lead-in) and survives, so the
	// printable stream is "joshzhang-MBP[31m" (17 runes) + 100 x's, capped at 64.
	want := "joshzhang-MBP[31m" + strings.Repeat("x", 47) // 17 + 47 = 64

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/auth/login?cli_callback=http://127.0.0.1:43123/callback&cli_state=cli_state_123&device_name="+url.QueryEscape(raw),
		nil,
	)
	ua, _, _ := setupUserAuth(t)
	w := httptest.NewRecorder()
	ua.HandleLogin(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	u, err := url.Parse(w.Result().Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	stateJSON, err := base64.URLEncoding.DecodeString(u.Query().Get("state"))
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	var state struct {
		DeviceName string `json:"dn"`
	}
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.DeviceName != want {
		t.Fatalf("device name = %q (len %d), want %q (len %d)", state.DeviceName, len(state.DeviceName), want, len(want))
	}
}

// TestHandleLogin_WebLoginIgnoresDeviceName: device_name only means anything
// alongside a CLI handoff; a plain web login (no cli_callback) must not
// carry it into the state even if the query string sends one.
func TestHandleLogin_WebLoginIgnoresDeviceName(t *testing.T) {
	ua, _, _ := setupUserAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/login?device_name=some-host", nil)
	w := httptest.NewRecorder()
	ua.HandleLogin(w, req)

	u, _ := url.Parse(w.Result().Header.Get("Location"))
	stateJSON, _ := base64.URLEncoding.DecodeString(u.Query().Get("state"))
	var state struct {
		DeviceName string `json:"dn"`
	}
	json.Unmarshal(stateJSON, &state)
	if state.DeviceName != "" {
		t.Fatalf("web login should not carry a device name, got %q", state.DeviceName)
	}
}

// TestHandleCallback_CLILogin_SameDeviceReplacesPriorKey is the regression
// test for the bug this fix targets: two logins from the same named device
// leave exactly one live "CLI login on <device>" key, and it is the second
// mint, not the first.
func TestHandleCallback_CLILogin_SameDeviceReplacesPriorKey(t *testing.T) {
	ua, store, srv := setupUserAuthWithFakeOAuth(t)
	_ = srv
	ctx := context.Background()

	login := func(nonce string) {
		t.Helper()
		state := auth.EncodeOAuthState(&auth.OAuthState{
			Nonce:       nonce,
			CLICallback: "http://127.0.0.1:43123/callback",
			CLIState:    "cli_state_abc",
			DeviceName:  "joshzhang-MBP",
		})
		req := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf("/api/auth/callback?code=fake-code&state=%s", url.QueryEscape(state)),
			nil,
		)
		req.AddCookie(&http.Cookie{Name: "e2a_oauth_state", Value: nonce})
		w := httptest.NewRecorder()
		ua.HandleCallback(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
	}

	login("nonce-device-1")
	login("nonce-device-2")

	user, err := store.CreateOrGetUser(ctx, "cliuser@test.com", "CLI User", "google-sub-cli-test")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	keys, err := store.ListAPIKeys(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	var named []identity.APIKey
	for _, k := range keys {
		if k.Name == "CLI login on joshzhang-MBP" {
			named = append(named, k)
		}
	}
	if len(named) != 1 {
		t.Fatalf("live keys named %q = %d, want 1 (re-login from the same device must replace, not accumulate)", "CLI login on joshzhang-MBP", len(named))
	}
}

// TestHandleCallback_CLILogin_NoDeviceName_PreservesLegacyAccumulation pins
// the deliberate scope boundary: without a device name (older CLI binaries,
// or a login door that never plumbs one) writeCLIHandoffPage must keep
// minting distinct "CLI login" keys rather than revoking by that
// un-device-scoped shared name, which could otherwise revoke a different,
// still-live device's key out from under it.
func TestHandleCallback_CLILogin_NoDeviceName_PreservesLegacyAccumulation(t *testing.T) {
	ua, store, srv := setupUserAuthWithFakeOAuth(t)
	_ = srv
	ctx := context.Background()

	login := func(nonce string) {
		t.Helper()
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
		w := httptest.NewRecorder()
		ua.HandleCallback(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
	}

	login("nonce-legacy-1")
	login("nonce-legacy-2")

	user, err := store.CreateOrGetUser(ctx, "cliuser@test.com", "CLI User", "google-sub-cli-test")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	keys, err := store.ListAPIKeys(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	var plain int
	for _, k := range keys {
		if k.Name == "CLI login" {
			plain++
		}
	}
	if plain != 2 {
		t.Fatalf("live \"CLI login\" keys = %d, want 2 (unnamed-device logins are unchanged by this fix)", plain)
	}
}
