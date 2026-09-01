package auth_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"golang.org/x/oauth2"
)

// --- POST /api/auth/logout ---

func TestHandleLogout_DeletesSessionAndClearsCookie(t *testing.T) {
	ua, store, token := setupUserAuth(t)

	req := authedRequest("POST", "/api/auth/logout", token)
	w := httptest.NewRecorder()
	ua.HandleLogout(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if location := w.Header().Get("Location"); location != "http://localhost/" {
		t.Fatalf("Location = %q, want http://localhost/", location)
	}

	// The response must expire the session cookie.
	var cleared *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cleared = c
		}
	}
	if cleared == nil {
		t.Fatal("logout response did not set a session cookie")
	}
	if cleared.Value != "" || cleared.MaxAge != -1 {
		t.Errorf("logout cookie = %+v, want empty value with MaxAge -1", cleared)
	}

	// The session row must be gone: the old token no longer authenticates.
	if _, err := store.GetUserSession(context.Background(), token); err == nil {
		t.Error("session still resolves after logout")
	}
	if user := ua.AuthenticateRequest(authedRequest("GET", "/api/auth/me", token)); user != nil {
		t.Error("AuthenticateRequest still returns a user after logout")
	}
}

func TestHandleLogout_WithoutCookieStillSucceeds(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("POST", "/api/auth/logout", nil)
	w := httptest.NewRecorder()
	ua.HandleLogout(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (logout is idempotent)", w.Code)
	}
	if location := w.Header().Get("Location"); location != "http://localhost/" {
		t.Fatalf("Location = %q, want http://localhost/", location)
	}
}

func TestHandleLogout_RedirectsToConfiguredOIDCLogoutURL(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	const logoutURL = "https://auth.example.com/auth/logout/upstream"
	ua.SetOIDCLogoutURL(logoutURL)

	req := authedRequest("POST", "/api/auth/logout", token)
	req.Header.Set("Origin", "http://localhost")
	w := httptest.NewRecorder()
	ua.HandleLogout(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if location := w.Header().Get("Location"); location != logoutURL {
		t.Fatalf("Location = %q, want %q", location, logoutURL)
	}
	if _, err := store.GetUserSession(context.Background(), token); err == nil {
		t.Fatal("session still resolves after upstream logout redirect")
	}
}

func TestHandleLogout_UsesCanonicalOIDCOriginWithoutGoogleOAuth(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	user, err := store.CreateOrGetUser(context.Background(), "oidc@example.test", "OIDC User", "oidc-subject")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	token, err := store.CreateUserSession(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}
	ua := auth.NewUserAuth(&config.OAuthConfig{}, store, false)
	ua.SetLogoutOrigin("https://APP.example.com:443")
	ua.SetOIDCLogoutURL("https://auth.example.com/auth/logout/upstream")

	req := authedRequest("POST", "/api/auth/logout", token)
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	ua.HandleLogout(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if location := w.Header().Get("Location"); location != "https://auth.example.com/auth/logout/upstream" {
		t.Fatalf("Location = %q, want upstream logout URL", location)
	}
}

func TestHandleLogout_ConfiguredWithoutCookieDoesNotCascade(t *testing.T) {
	ua, _, _ := setupUserAuth(t)
	ua.SetOIDCLogoutURL("https://auth.example.com/auth/logout/upstream")

	w := httptest.NewRecorder()
	ua.HandleLogout(w, httptest.NewRequest("POST", "/api/auth/logout", nil))

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if location := w.Header().Get("Location"); location != "http://localhost/" {
		t.Fatalf("Location = %q, want local root", location)
	}
}

func TestHandleLogout_ConfiguredRejectsCrossOriginHandoff(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ua.SetOIDCLogoutURL("https://auth.example.com/auth/logout/upstream")

	req := authedRequest("POST", "/api/auth/logout", token)
	req.Header.Set("Origin", "https://attacker.test")
	w := httptest.NewRecorder()
	ua.HandleLogout(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if location := w.Header().Get("Location"); location != "" {
		t.Fatalf("Location = %q, want no upstream redirect", location)
	}
	if _, err := store.GetUserSession(context.Background(), token); err == nil {
		t.Fatal("session still resolves after cross-origin logout")
	}
}

func TestHandleLogout_ReturnsUnavailableWhenSessionLookupFails(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ua.SetOIDCLogoutURL("https://auth.example.com/auth/logout/upstream")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := authedRequest("POST", "/api/auth/logout", token).WithContext(ctx)
	w := httptest.NewRecorder()
	ua.HandleLogout(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if _, err := store.GetUserSession(context.Background(), token); err != nil {
		t.Fatalf("session should remain when revocation cannot be confirmed: %v", err)
	}
}

func TestHandleLogout_UsesRelativeRootWithoutOAuthBaseURL(t *testing.T) {
	pool := testutil.TestDB(t)
	ua := auth.NewUserAuth(&config.OAuthConfig{}, identity.NewStore(pool), false)

	w := httptest.NewRecorder()
	ua.HandleLogout(w, httptest.NewRequest("POST", "/api/auth/logout", nil))

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if location := w.Header().Get("Location"); location != "/" {
		t.Fatalf("Location = %q, want /", location)
	}
}

// --- GET /api/auth/me ---

func TestHandleMe_Unauthenticated(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	w := httptest.NewRecorder()
	ua.HandleMe(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleMe_ReturnsCurrentUser(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedRequest("GET", "/api/auth/me", token)
	w := httptest.NewRecorder()
	ua.HandleMe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got identity.User
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Email != "josh@test.com" {
		t.Errorf("Email = %q, want josh@test.com", got.Email)
	}
	if got.Name != "Josh" {
		t.Errorf("Name = %q, want Josh", got.Name)
	}
}

// --- GET /api/dashboard/agents ---

func TestHandleDashboardAgents_Unauthenticated(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("GET", "/api/dashboard/agents", nil)
	w := httptest.NewRecorder()
	ua.HandleDashboardAgents(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleDashboardAgents_EmptyListIsJSONArray(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedRequest("GET", "/api/dashboard/agents", token)
	w := httptest.NewRecorder()
	ua.HandleDashboardAgents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// The dashboard iterates the array; null would break rendering, so an
	// empty workspace must serialize as [] rather than null.
	var got struct {
		Agents []identity.AgentIdentity `json:"agents"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Agents == nil {
		t.Errorf("agents = null, want [] (body=%s)", w.Body.String())
	}
}

func TestHandleDashboardAgents_ListsOwnedAgents(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.GetUserSession(ctx, token)
	store.ClaimOrCreateDomain(ctx, "dash-list.example.com", user.ID)
	if _, err := store.CreateAgent(ctx, "bot@dash-list.example.com", "dash-list.example.com", "Bot", "https://example.com/webhook", "", user.ID); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// An agent owned by someone else must not leak into the listing.
	other, _ := store.CreateOrGetUser(ctx, "other@dash-list.example.com", "Other", "google-other-dash")
	store.ClaimOrCreateDomain(ctx, "dash-foreign.example.com", other.ID)
	store.CreateAgent(ctx, "bot@dash-foreign.example.com", "dash-foreign.example.com", "", "https://example.com/webhook", "", other.ID)

	req := authedRequest("GET", "/api/dashboard/agents", token)
	w := httptest.NewRecorder()
	ua.HandleDashboardAgents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Agents []identity.AgentIdentity `json:"agents"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Agents) != 1 {
		t.Fatalf("expected exactly 1 owned agent, got %d: %+v", len(got.Agents), got.Agents)
	}
	if got.Agents[0].Email != "bot@dash-list.example.com" {
		t.Errorf("agent email = %q, want bot@dash-list.example.com", got.Agents[0].Email)
	}
}

// --- DELETE /api/dashboard/agents/{email} ---

func TestHandleDeleteAgent_Unauthenticated(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("DELETE", "/api/dashboard/agents/agent%40test.example.com", nil)
	w := httptest.NewRecorder()
	ua.HandleDeleteAgent(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleDeleteAgent_EmptyEmail(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedRequest("DELETE", "/api/dashboard/agents/", token)
	w := httptest.NewRecorder()
	ua.HandleDeleteAgent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleDeleteAgent_NotFound(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedRequest("DELETE", "/api/dashboard/agents/agent%40missing.example.com", token)
	w := httptest.NewRecorder()
	ua.HandleDeleteAgent(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandleDeleteAgent_NotOwned — an agent owned by another account is
// refused as if it did not exist. It must NOT be a 403: that told any
// signed-up user which arbitrary addresses are live agents on other
// accounts. Same class as GHSA-jh7v-7hx6-2mc2 on the consent endpoint.
func TestHandleDeleteAgent_NotOwned(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	other, _ := store.CreateOrGetUser(ctx, "other@del.example.com", "Other", "google-other-del")
	store.ClaimOrCreateDomain(ctx, "del-foreign.example.com", other.ID)
	store.CreateAgent(ctx, "agent@del-foreign.example.com", "del-foreign.example.com", "", "https://example.com/webhook", "", other.ID)

	req := authedRequest("DELETE", "/api/dashboard/agents/agent%40del-foreign.example.com", token)
	w := httptest.NewRecorder()
	ua.HandleDeleteAgent(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (must not distinguish not-owned from nonexistent)", w.Code)
	}
	// The store's own message names the distinction ("agent not found or not
	// owned by user"); echoing it would leak just as loudly as the status.
	if strings.Contains(w.Body.String(), "not owned") {
		t.Errorf("body must not reveal the ownership distinction: %q", w.Body.String())
	}
}

// TestHandleDeleteAgent_NotEnumerable is the regression guard: a real agent
// on another account and an address that does not exist at all must produce
// byte-identical responses. Asserting each case's status separately would
// not catch a change that keeps both at 404 but reintroduces distinct bodies.
func TestHandleDeleteAgent_NotEnumerable(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	other, _ := store.CreateOrGetUser(ctx, "other@del-enum.example.com", "Other", "google-other-del-enum")
	store.ClaimOrCreateDomain(ctx, "del-enum-foreign.example.com", other.ID)
	store.CreateAgent(ctx, "agent@del-enum-foreign.example.com", "del-enum-foreign.example.com", "", "https://example.com/webhook", "", other.ID)

	probe := func(path string) (int, string) {
		req := authedRequest("DELETE", "/api/dashboard/agents/"+path, token)
		w := httptest.NewRecorder()
		ua.HandleDeleteAgent(w, req)
		return w.Code, w.Body.String()
	}

	existsStatus, existsBody := probe("agent%40del-enum-foreign.example.com")
	missingStatus, missingBody := probe("agent%40del-enum-nowhere.example.com")

	if existsStatus != missingStatus {
		t.Errorf("status leaks existence: exists=%d missing=%d", existsStatus, missingStatus)
	}
	if existsBody != missingBody {
		t.Errorf("body leaks existence:\n exists  = %q\n missing = %q", existsBody, missingBody)
	}
	if existsStatus != http.StatusNotFound {
		t.Errorf("both cases should be 404, got %d", existsStatus)
	}
}

// TestHandleUpdateAgent_NotEnumerable pins the same invariant on the update
// route. The empty body is deliberate: the ownership check has to run before
// the field-parsing branches, or a foreign agent falls through to the "no
// recognized fields" 400 while a nonexistent one 404s — the same oracle by a
// different route.
func TestHandleUpdateAgent_NotEnumerable(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	other, _ := store.CreateOrGetUser(ctx, "other@upd-enum.example.com", "Other", "google-other-upd-enum")
	store.ClaimOrCreateDomain(ctx, "upd-enum-foreign.example.com", other.ID)
	store.CreateAgent(ctx, "agent@upd-enum-foreign.example.com", "upd-enum-foreign.example.com", "", "https://example.com/webhook", "", other.ID)

	probe := func(path, body string) (int, string) {
		req := authedJSON("PUT", "/api/dashboard/agents/"+path, token, body)
		w := httptest.NewRecorder()
		ua.HandleUpdateAgent(w, req)
		return w.Code, w.Body.String()
	}

	// The invalid-config body is the one that pins the ownership check's
	// PLACEMENT. With the check above ValidateHITLConfig (correct), a foreign
	// agent 404s like a nonexistent one. Move the check below validation — a
	// plausible "validate input early" refactor — and this body alone
	// reopens the oracle: the foreign agent returns 400 with the validation
	// message while the nonexistent one still 404s. The two valid bodies
	// cannot tell those orderings apart.
	for _, body := range []string{`{}`, `{"hitl_ttl_seconds":3600}`, `{"hitl_ttl_seconds":-1}`} {
		existsStatus, existsBody := probe("agent%40upd-enum-foreign.example.com", body)
		missingStatus, missingBody := probe("agent%40upd-enum-nowhere.example.com", body)

		if existsStatus != missingStatus || existsBody != missingBody {
			t.Errorf("body %s leaks existence: exists=(%d,%q) missing=(%d,%q)",
				body, existsStatus, existsBody, missingStatus, missingBody)
		}
		if existsStatus != http.StatusNotFound {
			t.Errorf("body %s: both cases should be 404, got %d", body, existsStatus)
		}
	}
}

func TestHandleDeleteAgent_HappyPath(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.GetUserSession(ctx, token)
	store.ClaimOrCreateDomain(ctx, "del.example.com", user.ID)
	if _, err := store.CreateAgent(ctx, "agent@del.example.com", "del.example.com", "", "https://example.com/webhook", "", user.ID); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	req := authedRequest("DELETE", "/api/dashboard/agents/agent%40del.example.com", token)
	w := httptest.NewRecorder()
	ua.HandleDeleteAgent(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Soft delete: live lookups must stop resolving the agent.
	if _, err := store.GetAgentByEmail(ctx, "agent@del.example.com"); err == nil {
		t.Error("agent still resolves via GetAgentByEmail after delete")
	}
	// Deleting again surfaces the not-found path (agent no longer resolves).
	req = authedRequest("DELETE", "/api/dashboard/agents/agent%40del.example.com", token)
	w = httptest.NewRecorder()
	ua.HandleDeleteAgent(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", w.Code)
	}
}

// --- PUT /api/dashboard/agents/{email} ---

func TestHandleUpdateAgent_Unauthenticated(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("PUT", "/api/dashboard/agents/agent%40test.example.com", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	ua.HandleUpdateAgent(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleUpdateAgent_EmptyEmail(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedJSON("PUT", "/api/dashboard/agents/", token, `{"hitl_ttl_seconds":60}`)
	w := httptest.NewRecorder()
	ua.HandleUpdateAgent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleUpdateAgent_MalformedBody(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedJSON("PUT", "/api/dashboard/agents/agent%40test.example.com", token, `{not-json}`)
	w := httptest.NewRecorder()
	ua.HandleUpdateAgent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleUpdateAgent_NotFound(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedJSON("PUT", "/api/dashboard/agents/agent%40missing.example.com", token, `{"hitl_ttl_seconds":60}`)
	w := httptest.NewRecorder()
	ua.HandleUpdateAgent(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleUpdateAgent_NoRecognizedFields(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.GetUserSession(ctx, token)
	store.ClaimOrCreateDomain(ctx, "upd-empty.example.com", user.ID)
	store.CreateAgent(ctx, "agent@upd-empty.example.com", "upd-empty.example.com", "", "https://example.com/webhook", "", user.ID)

	req := authedJSON("PUT", "/api/dashboard/agents/agent%40upd-empty.example.com", token, `{}`)
	w := httptest.NewRecorder()
	ua.HandleUpdateAgent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an empty PUT (no silent no-op)", w.Code)
	}
}

func TestHandleUpdateAgent_InvalidHITLConfig(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.GetUserSession(ctx, token)
	store.ClaimOrCreateDomain(ctx, "upd-bad.example.com", user.ID)
	store.CreateAgent(ctx, "agent@upd-bad.example.com", "upd-bad.example.com", "", "https://example.com/webhook", "", user.ID)

	// TTL below the store's valid range (1..HITLMaxTTLSeconds).
	req := authedJSON("PUT", "/api/dashboard/agents/agent%40upd-bad.example.com", token, `{"hitl_ttl_seconds":-5}`)
	w := httptest.NewRecorder()
	ua.HandleUpdateAgent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateAgent_UpdatesHITLSettings(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.GetUserSession(ctx, token)
	store.ClaimOrCreateDomain(ctx, "upd.example.com", user.ID)
	before, err := store.CreateAgent(ctx, "agent@upd.example.com", "upd.example.com", "", "https://example.com/webhook", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// TTL only — the expiration action must keep its current value.
	req := authedJSON("PUT", "/api/dashboard/agents/agent%40upd.example.com", token, `{"hitl_ttl_seconds":3600}`)
	w := httptest.NewRecorder()
	ua.HandleUpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("TTL update status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	after, err := store.GetAgentByEmail(ctx, "agent@upd.example.com")
	if err != nil {
		t.Fatalf("GetAgentByEmail: %v", err)
	}
	if after.HITLTTLSeconds != 3600 {
		t.Errorf("HITLTTLSeconds = %d, want 3600", after.HITLTTLSeconds)
	}
	if after.HITLExpirationAction != before.HITLExpirationAction {
		t.Errorf("HITLExpirationAction changed from %q to %q; untouched fields must be preserved",
			before.HITLExpirationAction, after.HITLExpirationAction)
	}

	// Both fields in one PUT.
	req = authedJSON("PUT", "/api/dashboard/agents/agent%40upd.example.com", token,
		`{"hitl_ttl_seconds":7200,"hitl_expiration_action":"approve"}`)
	w = httptest.NewRecorder()
	ua.HandleUpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("combined update status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	after, err = store.GetAgentByEmail(ctx, "agent@upd.example.com")
	if err != nil {
		t.Fatalf("GetAgentByEmail: %v", err)
	}
	if after.HITLTTLSeconds != 7200 || after.HITLExpirationAction != "approve" {
		t.Errorf("HITL settings = ttl:%d action:%q, want ttl:7200 action:approve",
			after.HITLTTLSeconds, after.HITLExpirationAction)
	}
}

// --- DELETE /api/keys/{id} ---

func TestHandleDeleteAPIKey_Unauthenticated(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("DELETE", "/api/keys/key_123", nil)
	w := httptest.NewRecorder()
	ua.HandleDeleteAPIKey(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleDeleteAPIKey_EmptyKeyID(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedRequest("DELETE", "/api/keys/", token)
	w := httptest.NewRecorder()
	ua.HandleDeleteAPIKey(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleDeleteAPIKey_NotFound(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedRequest("DELETE", "/api/keys/key_does_not_exist", token)
	w := httptest.NewRecorder()
	ua.HandleDeleteAPIKey(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleDeleteAPIKey_OtherUsersKey(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	other, _ := store.CreateOrGetUser(ctx, "other@keys.example.com", "Other", "google-other-keys")
	otherKey, err := store.CreateAPIKey(ctx, other.ID, "foreign key", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	req := authedRequest("DELETE", "/api/keys/"+otherKey.ID, token)
	w := httptest.NewRecorder()
	ua.HandleDeleteAPIKey(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no cross-user key deletion)", w.Code)
	}
}

func TestHandleDeleteAPIKey_HappyPath(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.GetUserSession(ctx, token)
	key, err := store.CreateAPIKey(ctx, user.ID, "to-delete", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	req := authedRequest("DELETE", "/api/keys/"+key.ID, token)
	w := httptest.NewRecorder()
	ua.HandleDeleteAPIKey(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Revoked keys disappear from the listing.
	keys, err := store.ListAPIKeys(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	for _, k := range keys {
		if k.ID == key.ID {
			t.Errorf("deleted key %s still listed", key.ID)
		}
	}

	// Deleting again is a 404 (already revoked).
	req = authedRequest("DELETE", "/api/keys/"+key.ID, token)
	w = httptest.NewRecorder()
	ua.HandleDeleteAPIKey(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", w.Code)
	}
}

// --- GET /api/keys (uncovered branches) ---

func TestHandleListAPIKeys_Unauthenticated(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("GET", "/api/keys", nil)
	w := httptest.NewRecorder()
	ua.HandleListAPIKeys(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleListAPIKeys_EmptyListIsJSONArray(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedRequest("GET", "/api/keys", token)
	w := httptest.NewRecorder()
	ua.HandleListAPIKeys(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Errorf("body = %q, want [] for a user with no keys", body)
	}
}

// --- POST /api/keys (uncovered branches) ---

func TestHandleCreateAPIKey_Unauthenticated(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	req := httptest.NewRequest("POST", "/api/keys", strings.NewReader(`{"name":"x"}`))
	w := httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleCreateAPIKey_RejectsInvalidScope(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedJSON("POST", "/api/keys", token, `{"name":"x","scope":"universe"}`)
	w := httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleCreateAPIKey_AgentScopeRequiresAgentEmail(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedJSON("POST", "/api/keys", token, `{"name":"x","scope":"agent"}`)
	w := httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleCreateAPIKey_AgentScopeRejectsUnknownAgent(t *testing.T) {
	ua, _, token := setupUserAuth(t)

	req := authedJSON("POST", "/api/keys", token, `{"name":"x","scope":"agent","agent":"ghost@nowhere.example.com"}`)
	w := httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleCreateAPIKey_AgentScopeRejectsForeignAgent(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	other, _ := store.CreateOrGetUser(ctx, "other@scoped.example.com", "Other", "google-other-scoped")
	store.ClaimOrCreateDomain(ctx, "scoped-foreign.example.com", other.ID)
	store.CreateAgent(ctx, "agent@scoped-foreign.example.com", "scoped-foreign.example.com", "", "https://example.com/webhook", "", other.ID)

	req := authedJSON("POST", "/api/keys", token, `{"name":"x","scope":"agent","agent":"agent@scoped-foreign.example.com"}`)
	w := httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an agent owned by another user", w.Code)
	}
}

func TestHandleCreateAPIKey_AgentScopeHappyPath(t *testing.T) {
	ua, store, token := setupUserAuth(t)
	ctx := context.Background()

	user, _ := store.GetUserSession(ctx, token)
	store.ClaimOrCreateDomain(ctx, "scoped.example.com", user.ID)
	agent, err := store.CreateAgent(ctx, "agent@scoped.example.com", "scoped.example.com", "", "https://example.com/webhook", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	req := authedJSON("POST", "/api/keys", token, `{"name":"agent key","scope":"agent","agent":"agent@scoped.example.com"}`)
	w := httptest.NewRecorder()
	ua.HandleCreateAPIKey(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created identity.APIKey
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Scope != identity.ScopeAgent {
		t.Errorf("Scope = %q, want %q", created.Scope, identity.ScopeAgent)
	}
	if created.AgentID == nil || *created.AgentID != agent.ID {
		t.Errorf("AgentID = %v, want %s", created.AgentID, agent.ID)
	}
	if !strings.HasPrefix(created.PlaintextKey, "e2a_agt_") {
		t.Errorf("agent-scoped key prefix missing e2a_agt_: %q", created.PlaintextKey)
	}
}

// --- GET /api/auth/login cli_callback validation (uncovered branches) ---

// TestHandleLogin_CLICallbackValidation exercises validateCLICallbackURL's
// remaining reject branches plus the hostname (not IP-literal) loopback path.
func TestHandleLogin_CLICallbackValidation(t *testing.T) {
	cases := []struct {
		name       string
		cliCB      string
		wantStatus int
	}{
		{"user info rejected", "http://user@127.0.0.1:9000/cb", http.StatusBadRequest},
		{"missing host rejected", "http:///cb", http.StatusBadRequest},
		{"non-loopback IP rejected", "http://8.8.8.8/cb", http.StatusBadRequest},
		{"non-loopback hostname rejected", "http://callback.example.com/cb", http.StatusBadRequest},
		{"localhost hostname allowed", "http://localhost:9000/cb", http.StatusFound},
		{"ipv4 loopback allowed", "http://127.0.0.1:9000/cb", http.StatusFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ua, _, _ := setupUserAuth(t)
			req := httptest.NewRequest(http.MethodGet,
				"/api/auth/login?cli_callback="+url.QueryEscape(tc.cliCB)+"&cli_state=cli_state_x", nil)
			w := httptest.NewRecorder()
			ua.HandleLogin(w, req)
			if w.Code != tc.wantStatus {
				t.Errorf("cli_callback=%q: status = %d, want %d; body=%s", tc.cliCB, w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

func TestHandleLogin_CLIParamsMustComeInPairs(t *testing.T) {
	ua, _, _ := setupUserAuth(t)

	// cli_callback without cli_state (and vice versa) is a 400.
	req := httptest.NewRequest(http.MethodGet,
		"/api/auth/login?cli_callback="+url.QueryEscape("http://127.0.0.1:9000/cb"), nil)
	w := httptest.NewRecorder()
	ua.HandleLogin(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("cli_callback alone: status = %d, want 400", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/auth/login?cli_state=cli_state_x", nil)
	w = httptest.NewRecorder()
	ua.HandleLogin(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("cli_state alone: status = %d, want 400", w.Code)
	}
}

// --- GET /api/auth/callback error branches ---

// setupUserAuthWithFailingOAuth builds a UserAuth whose token exchange and/or
// userinfo call fails, for HandleCallback's post-nonce error branches.
// Either handler may be nil to use a working default.
func setupUserAuthWithFailingOAuth(t *testing.T, tokenHandler, userInfoHandler http.HandlerFunc) *auth.UserAuth {
	t.Helper()
	mux := http.NewServeMux()
	if tokenHandler == nil {
		tokenHandler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "fake-access-token",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
		}
	}
	if userInfoHandler == nil {
		userInfoHandler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"sub":            "google-sub-cb-test",
				"email":          "cbuser@test.com",
				"email_verified": true,
				"name":           "Callback User",
			})
		}
	}
	mux.HandleFunc("/token", tokenHandler)
	mux.HandleFunc("/userinfo", userInfoHandler)
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
	cfg := &config.OAuthConfig{
		GoogleClientID:     oauthCfg.ClientID,
		GoogleClientSecret: oauthCfg.ClientSecret,
		RedirectURL:        oauthCfg.RedirectURL,
	}
	store := identity.NewStore(testutil.TestDB(t))
	return auth.NewUserAuthWithOAuthConfig(cfg, oauthCfg, store, false, srv.URL+"/userinfo")
}

// callbackReqWithValidState builds a callback request whose nonce cookie
// matches the OAuth state, so the handler proceeds past CSRF validation
// into the exchange/userinfo branches under test.
func callbackReqWithValidState(query string) *http.Request {
	nonce := "cb-test-nonce"
	state := auth.EncodeOAuthState(&auth.OAuthState{Nonce: nonce})
	sep := "&"
	if query == "" {
		sep = ""
	}
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/auth/callback?%s%sstate=%s", query, sep, url.QueryEscape(state)), nil)
	req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: nonce})
	return req
}

func TestHandleCallback_MissingCodeOrState(t *testing.T) {
	ua, _, srv := setupUserAuthWithFakeOAuth(t)
	_ = srv

	// No code, no state at all.
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback", nil)
	w := httptest.NewRecorder()
	ua.HandleCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("no params: status = %d, want 400", w.Code)
	}

	// State present but code missing.
	req = callbackReqWithValidState("")
	w = httptest.NewRecorder()
	ua.HandleCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing code: status = %d, want 400", w.Code)
	}
}

func TestHandleCallback_ExchangeFailure(t *testing.T) {
	ua := setupUserAuthWithFailingOAuth(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}, nil)

	req := callbackReqWithValidState("code=bad-code")
	w := httptest.NewRecorder()
	ua.HandleCallback(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on token exchange failure", w.Code)
	}
}

func TestHandleCallback_UserInfoFailure(t *testing.T) {
	ua := setupUserAuthWithFailingOAuth(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		// fetchGoogleUserInfo decodes the body without a status check, so an
		// undecodable payload is what drives its error branch.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not-json"))
	})

	req := callbackReqWithValidState("code=fake-code")
	w := httptest.NewRecorder()
	ua.HandleCallback(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on userinfo fetch failure", w.Code)
	}
}

func TestHandleCallback_UnverifiedEmailRejected(t *testing.T) {
	ua := setupUserAuthWithFailingOAuth(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"sub":            "google-sub-unverified",
			"email":          "unverified@test.com",
			"email_verified": false,
			"name":           "Unverified",
		})
	})

	req := callbackReqWithValidState("code=fake-code")
	w := httptest.NewRecorder()
	ua.HandleCallback(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an unverified Google email", w.Code)
	}
}
