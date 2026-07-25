package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// writeJSON encodes payload as the response body and logs encode errors
// rather than swallowing them — useful for debugging truncated responses
// when a client drops mid-write.
func writeJSON(w http.ResponseWriter, payload any) {
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("[auth] json encode failed: %v", err)
	}
}

const (
	SessionCookieName = "e2a_session"
	StateCookieName   = "e2a_oauth_state"
	SessionMaxAge     = 7 * 24 * time.Hour
)

const defaultUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"

type UserAuth struct {
	oauthConfig *oauth2.Config
	store       *identity.Store
	secure      bool   // true in production (Secure cookie flag)
	baseURL     string // frontend origin for post-login redirect
	userInfoURL string // Google userinfo endpoint (overridable for testing)
}

type cliLoginHandoff struct {
	CallbackURL string
	State       string
}

var cliLoginTemplate = template.Must(template.New("cli-login").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Connecting e2a CLI</title>
  <style>
    body {
      margin: 0;
      font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: #f5f7fb;
      color: #111827;
      display: grid;
      place-items: center;
      min-height: 100vh;
      padding: 24px;
    }
    main {
      width: 100%;
      max-width: 480px;
      background: white;
      border: 1px solid #e5e7eb;
      border-radius: 16px;
      padding: 28px;
      box-shadow: 0 18px 50px rgba(15, 23, 42, 0.08);
    }
    h1 { margin: 0 0 8px; font-size: 24px; }
    p { margin: 0 0 16px; color: #4b5563; line-height: 1.5; }
    button {
      width: 100%;
      padding: 12px 16px;
      border: 0;
      border-radius: 10px;
      background: #2563eb;
      color: white;
      font-weight: 600;
      cursor: pointer;
    }
    .meta { font-size: 13px; color: #6b7280; margin-top: 12px; }
  </style>
</head>
<body>
  <main>
    <h1>Connecting your CLI</h1>
    <p>Finish signing in and send your API key back to the terminal automatically.</p>
    <form id="cli-login" action="{{ .CallbackURL }}" method="post">
      <input type="hidden" name="cli_state" value="{{ .State }}">
      <input type="hidden" name="api_key" value="{{ .APIKey }}">
      <input type="hidden" name="agent_email" value="{{ .AgentEmail }}">
      <button type="submit">Continue to e2a CLI</button>
    </form>
    <p class="meta">If the terminal does not update automatically, click the button above.</p>
  </main>
  <script>document.getElementById("cli-login")?.submit();</script>
</body>
</html>`))

func NewUserAuth(cfg *config.OAuthConfig, store *identity.Store, production bool) *UserAuth {
	// Derive the user auth callback URL from the existing redirect URL.
	// e.g. "https://e2a.example.com/api/verify/callback" → "https://e2a.example.com/api/auth/callback"
	callbackURL := cfg.RedirectURL
	baseURL := ""
	if u, err := url.Parse(cfg.RedirectURL); err == nil && u.Host != "" {
		u.Path = "/api/auth/callback"
		callbackURL = u.String()
		baseURL = u.Scheme + "://" + u.Host
	}

	return &UserAuth{
		oauthConfig: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  callbackURL,
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint:     google.Endpoint,
		},
		store:       store,
		secure:      production,
		baseURL:     baseURL,
		userInfoURL: defaultUserInfoURL,
	}
}

// NewUserAuthWithOAuthConfig creates a UserAuth with a custom oauth2.Config and
// userinfo URL. This is intended for testing against fake OAuth servers.
func NewUserAuthWithOAuthConfig(cfg *config.OAuthConfig, oauthCfg *oauth2.Config, store *identity.Store, production bool, userInfoURL string) *UserAuth {
	baseURL := ""
	if u, err := url.Parse(cfg.RedirectURL); err == nil && u.Host != "" {
		baseURL = u.Scheme + "://" + u.Host
	}
	return &UserAuth{
		oauthConfig: oauthCfg,
		store:       store,
		secure:      production,
		baseURL:     baseURL,
		userInfoURL: userInfoURL,
	}
}

func generateNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// OAuth state nonce — an all-zero nonce defeats the CSRF
		// protection it exists to provide. Panic rather than silently
		// emit a predictable value.
		panic(fmt.Sprintf("auth: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

func validateCLICallbackURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("cli callback URL required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid cli callback URL: %w", err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("cli callback URL must use http")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("cli callback URL must include a host")
	}
	if u.User != nil {
		return nil, fmt.Errorf("cli callback URL must not include user info")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("cli callback URL must include a host")
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("cli callback URL must use a loopback host")
		}
	}
	return u, nil
}

// OAuthState is encoded into the OAuth state parameter. It carries the CSRF
// nonce and, for CLI-initiated logins, the callback URL and CLI state token.
// ReturnTo, if set, is a same-origin server-path the user is bounced back to
// after callback succeeds — used by the MCP authorize flow to resume after
// a session is established. Validated at HandleLogin time.
type OAuthState struct {
	Nonce       string `json:"n"`
	CLICallback string `json:"cb,omitempty"`
	CLIState    string `json:"cs,omitempty"`
	ReturnTo    string `json:"rt,omitempty"`
}

func EncodeOAuthState(s *OAuthState) string {
	b, _ := json.Marshal(s)
	return base64.URLEncoding.EncodeToString(b)
}

func decodeOAuthState(raw string) (*OAuthState, error) {
	b, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid oauth state encoding")
	}
	var s OAuthState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("invalid oauth state")
	}
	return &s, nil
}

func (ua *UserAuth) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   ua.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	})
}

func defaultAgentEmail(ctx context.Context, store *identity.Store, userID string) string {
	agents, err := store.ListAgentsByUser(ctx, userID, 0, time.Time{}, "")
	if err != nil || len(agents) == 0 {
		return ""
	}
	return agents[0].EmailAddress()
}

// writeCLIHandoffPage mints a fresh "CLI login" API key for the user and
// renders the auto-submitting page that POSTs it (plus the CLI's state
// token) to the loopback listener the CLI opened. Shared by every browser
// login door that supports the CLI handoff.
func writeCLIHandoffPage(store *identity.Store, w http.ResponseWriter, r *http.Request, user *identity.User, handoff *cliLoginHandoff) error {
	key, err := store.CreateAPIKey(r.Context(), user.ID, "CLI login", nil)
	if err != nil {
		return fmt.Errorf("failed to create api key: %w", err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return cliLoginTemplate.Execute(w, map[string]string{
		"CallbackURL": handoff.CallbackURL,
		"State":       handoff.State,
		"APIKey":      key.PlaintextKey,
		"AgentEmail":  defaultAgentEmail(r.Context(), store, user.ID),
	})
}

// HandleLogin redirects the user to Google OAuth.
// CLI login params (cli_callback, cli_state) are encoded into the OAuth state
// parameter so they survive the redirect through Google without relying on cookies.
// return_to (optional) is a same-origin server path the user resumes on after
// callback success — only paths under /oauth2/ are permitted, used to bounce
// MCP OAuth clients back into /oauth2/authorize after a session is created.
func (ua *UserAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	cliCallback := r.URL.Query().Get("cli_callback")
	cliState := r.URL.Query().Get("cli_state")
	if (cliCallback == "") != (cliState == "") {
		http.Error(w, "cli_callback and cli_state must be provided together", http.StatusBadRequest)
		return
	}

	nonce := generateNonce()
	state := &OAuthState{Nonce: nonce}

	if cliCallback != "" {
		callbackURL, err := validateCLICallbackURL(cliCallback)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state.CLICallback = callbackURL.String()
		state.CLIState = cliState
	}

	if returnTo := r.URL.Query().Get("return_to"); returnTo != "" {
		if err := validateReturnToPath(returnTo); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state.ReturnTo = returnTo
	}

	ua.setCookie(w, StateCookieName, nonce, 600)

	http.Redirect(w, r, ua.oauthConfig.AuthCodeURL(EncodeOAuthState(state)), http.StatusFound)
}

// validateReturnToPath enforces the same-origin / known-prefix allow-list
// for return_to values. Accepting an arbitrary URL would turn /api/auth/login
// into an open redirector that an attacker could chain with phishing-class
// social engineering. Limiting to /oauth2/-prefixed server paths means
// the bounce can only land inside the OAuth flow we own (Slice 5b renamed the
// OAuth surface from /oauth2/* to /oauth2/*).
func validateReturnToPath(raw string) error {
	if !strings.HasPrefix(raw, "/oauth2/") {
		return errors.New("return_to must be a server path starting with /oauth2/")
	}
	if strings.ContainsAny(raw, "\\\n\r\x00") {
		return errors.New("return_to contains forbidden characters")
	}
	// url.Parse should produce a clean path-only URL: no scheme, no host.
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("return_to is not a valid URL path")
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil {
		return errors.New("return_to must be a path with no scheme or authority")
	}
	// Reject path traversal that survives the HasPrefix check by being
	// collapsed by the browser. e.g. raw "/oauth2/../../dashboard"
	// matches the prefix but http.Redirect emits a Location header that
	// the browser resolves to "/dashboard" — escaping the allow-list.
	// path.Clean folds the "../" segments and we re-check the prefix on
	// the normalized form.
	cleaned := path.Clean(u.Path)
	if !strings.HasPrefix(cleaned, "/oauth2/") && cleaned != "/oauth2" {
		return errors.New("return_to escapes the allow-list after normalization")
	}
	// Also reject empty segments which a future router refactor might
	// treat as authority. "/oauth2//foo" survives path.Clean as
	// "/oauth2/foo" but the raw value carries the empty segment
	// which some HTTP stacks parse differently — fail closed.
	if strings.Contains(u.Path, "//") {
		return errors.New("return_to must not contain empty path segments")
	}
	return nil
}

// HandleCallback processes the Google OAuth callback and creates a session.
func (ua *UserAuth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := r.URL.Query().Get("code")
	stateParam := r.URL.Query().Get("state")

	if code == "" || stateParam == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	// Decode the OAuth state parameter
	state, err := decodeOAuthState(stateParam)
	if err != nil {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}

	// Verify the nonce matches the cookie
	cookie, err := r.Cookie(StateCookieName)
	if err != nil || cookie.Value != state.Nonce {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}

	// Clear the state cookie
	ua.setCookie(w, StateCookieName, "", -1)

	token, err := ua.oauthConfig.Exchange(ctx, code)
	if err != nil {
		http.Error(w, fmt.Sprintf("oauth exchange failed: %v", err), http.StatusInternalServerError)
		return
	}

	userInfo, err := fetchGoogleUserInfo(ctx, ua.oauthConfig, token, ua.userInfoURL)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch user info: %v", err), http.StatusInternalServerError)
		return
	}

	if !userInfo.EmailVerified {
		http.Error(w, "email not verified with Google", http.StatusForbidden)
		return
	}

	user, err := ua.store.CreateOrGetUser(ctx, userInfo.Email, userInfo.Name, userInfo.Sub)
	if err != nil {
		http.Error(w, "failed to create user", http.StatusInternalServerError)
		return
	}

	sessionToken, err := ua.store.CreateUserSession(ctx, user.ID)
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	ua.setCookie(w, SessionCookieName, sessionToken, int(SessionMaxAge.Seconds()))

	// If this login was initiated by the CLI, hand off the token
	if state.CLICallback != "" {
		callbackURL, err := validateCLICallbackURL(state.CLICallback)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		handoff := &cliLoginHandoff{
			CallbackURL: callbackURL.String(),
			State:       state.CLIState,
		}
		if err := writeCLIHandoffPage(ua.store, w, r, user, handoff); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// If this login was triggered by an /oauth2/ flow that wanted the
	// user to land back where they started, bounce to that path. The
	// allow-list was enforced at HandleLogin time; re-validate defensively
	// in case state was tampered with somehow (the OAuth state is integrity-
	// protected by the nonce cookie, but cheap to double-check).
	if state.ReturnTo != "" {
		if err := validateReturnToPath(state.ReturnTo); err == nil {
			http.Redirect(w, r, ua.baseURL+state.ReturnTo, http.StatusFound)
			return
		}
	}

	http.Redirect(w, r, ua.baseURL+"/dashboard", http.StatusFound)
}

// HandleLogout deletes the session and clears the cookie.
func (ua *UserAuth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(SessionCookieName)
	if err == nil {
		ua.store.DeleteUserSession(r.Context(), cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   ua.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	w.WriteHeader(http.StatusOK)
}

// HandleMe returns the current authenticated user's info.
func (ua *UserAuth) HandleMe(w http.ResponseWriter, r *http.Request) {
	user := ua.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, user)
}

// HandleUpdateMe accepts a PATCH that updates the authenticated user's
// display name. Other identity fields (email, google_subject) come from
// the OAuth provider and are not user-editable.
//
// Validation:
//   - name: required, 1–80 chars after TrimSpace
//   - rejects leading/trailing whitespace by comparing to TrimSpace
//     (we don't silently normalize — that would surprise a caller who
//     expected their exact bytes back from /me)
const (
	minDisplayNameLen = 1
	maxDisplayNameLen = 80
)

func (ua *UserAuth) HandleUpdateMe(w http.ResponseWriter, r *http.Request) {
	user := ua.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	var req struct {
		Name *string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Name == nil {
		http.Error(w, "no fields to update", http.StatusBadRequest)
		return
	}

	name := *req.Name
	if name != strings.TrimSpace(name) {
		http.Error(w, "name must not have leading or trailing whitespace", http.StatusBadRequest)
		return
	}
	if len(name) < minDisplayNameLen || len(name) > maxDisplayNameLen {
		http.Error(w, "name must be 1–80 characters", http.StatusBadRequest)
		return
	}

	updated, err := ua.store.UpdateUserName(r.Context(), user.ID, name)
	if err != nil {
		http.Error(w, "failed to update profile", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, updated)
}

// AuthenticateRequest extracts the user from the session cookie. Returns nil if not authenticated.
func (ua *UserAuth) AuthenticateRequest(r *http.Request) *identity.User {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return nil
	}
	user, err := ua.store.GetUserSession(r.Context(), cookie.Value)
	if err != nil {
		return nil
	}
	return user
}

type googleUserInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

func fetchGoogleUserInfo(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token, userInfoURL string) (*googleUserInfo, error) {
	client := cfg.Client(ctx, token)
	resp, err := client.Get(userInfoURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var info googleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// HandleDashboardStats returns the workspace-level aggregates for the
// redesigned dashboard's stats strip. Accepts ?window=N (days) to vary
// the lookback for inbound/outbound totals + delivery success — the
// dashboard at-a-glance strip omits it (defaults to 7), the settings
// usage card passes ?window=30. Invalid/out-of-range values fall back
// to the store's defaults (see DashboardDefaultWindowDays /
// DashboardMaxWindowDays). See identity.GetDashboardStats for the
// data sources and graceful-degradation behavior when usage tracking
// is disabled.
func (ua *UserAuth) HandleDashboardStats(w http.ResponseWriter, r *http.Request) {
	user := ua.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	// Parse optional ?window=N. Bad values get treated as "use default"
	// rather than 400 — the dashboard simply shouldn't break if a
	// caller fat-fingers the query string.
	windowDays := 0
	if raw := r.URL.Query().Get("window"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			windowDays = n
		}
	}

	stats, err := ua.store.GetDashboardStats(r.Context(), user.ID, windowDays)
	if err != nil {
		http.Error(w, "failed to fetch dashboard stats", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, stats)
}

// HandleDashboardAgents lists agents owned by the authenticated user.
func (ua *UserAuth) HandleDashboardAgents(w http.ResponseWriter, r *http.Request) {
	user := ua.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	agents, err := ua.store.ListAgentsByUser(r.Context(), user.ID, 0, time.Time{}, "")
	if err != nil {
		http.Error(w, "failed to list agents", http.StatusInternalServerError)
		return
	}

	if agents == nil {
		agents = []identity.AgentIdentity{}
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string][]identity.AgentIdentity{"agents": agents})
}

// HandleDeleteAgent moves an agent owned by the authenticated user to the
// trash (soft delete — restorable for identity.TrashRetention, then purged by
// the janitor). Kept in lockstep with the /v1 deleteAgent default semantics.
func (ua *UserAuth) HandleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	user := ua.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	// Extract agent email from URL path
	path := strings.TrimPrefix(r.URL.Path, "/api/dashboard/agents/")
	email, _ := url.PathUnescape(path)
	email = identity.NormalizeEmail(email)
	if email == "" {
		http.Error(w, "agent email required", http.StatusBadRequest)
		return
	}

	agent, err := ua.store.GetAgentByEmail(r.Context(), email)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	if err := ua.store.SoftDeleteAgent(r.Context(), agent.ID, user.ID); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// HandleUpdateAgent updates an agent owned by the authenticated user.
func (ua *UserAuth) HandleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	user := ua.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/dashboard/agents/")
	email, _ := url.PathUnescape(path)
	email = identity.NormalizeEmail(email)
	if email == "" {
		http.Error(w, "agent email required", http.StatusBadRequest)
		return
	}

	// Use pointer fields so we can distinguish "not present in request"
	// from "explicitly set to zero value". Clients PATCH individual
	// HITL settings — each may appear alone or combined in a single PUT.
	var req struct {
		HITLTTLSeconds       *int    `json:"hitl_ttl_seconds"`
		HITLExpirationAction *string `json:"hitl_expiration_action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	agnt, err := ua.store.GetAgentByEmail(r.Context(), email)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	// Track whether the body carried any understood field. An empty PUT
	// is treated as a 400 so mistakes don't silently no-op.
	touched := false

	// HITL settings update. Individual fields may be present; missing
	// fields keep their current value.
	if req.HITLTTLSeconds != nil || req.HITLExpirationAction != nil {
		ttl := agnt.HITLTTLSeconds
		if req.HITLTTLSeconds != nil {
			ttl = *req.HITLTTLSeconds
		}
		action := agnt.HITLExpirationAction
		if req.HITLExpirationAction != nil {
			action = *req.HITLExpirationAction
		}
		if err := ua.store.UpdateAgentHITL(r.Context(), agnt.ID, user.ID, ttl, action); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		touched = true
	}

	if !touched {
		http.Error(w, "no recognized fields in request", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// HandleAgentActivity returns recent message activity for an agent owned by the authenticated user.
func (ua *UserAuth) HandleAgentActivity(w http.ResponseWriter, r *http.Request) {
	user := ua.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	// Extract agent email from URL path: /api/dashboard/agents/{email}/activity
	path := strings.TrimPrefix(r.URL.Path, "/api/dashboard/agents/")
	email, _ := url.PathUnescape(strings.TrimSuffix(path, "/activity"))
	email = identity.NormalizeEmail(email)
	if email == "" {
		http.Error(w, "agent email required", http.StatusBadRequest)
		return
	}

	agent, err := ua.store.GetAgentByEmail(r.Context(), email)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	if agent.UserID != user.ID {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	activity, err := ua.store.ListActivityByAgent(r.Context(), agent.ID, 50)
	if err != nil {
		http.Error(w, "failed to list activity", http.StatusInternalServerError)
		return
	}

	if activity == nil {
		activity = []identity.Message{}
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, activity)
}

// HandleCreateAPIKey creates a new API key for the authenticated user.
func (ua *UserAuth) HandleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	user := ua.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	var req struct {
		Name      string  `json:"name"`
		ExpiresAt *string `json:"expires_at,omitempty"` // optional ISO 8601 timestamp
		// Scope (Slice 5a) selects the credential's blast radius. Omitted/""
		// defaults to account (backward compatible). "agent" requires Agent to
		// name one of the caller's agents by email; the minted key can act ONLY
		// as that agent.
		Scope string `json:"scope,omitempty"`
		Agent string `json:"agent,omitempty"` // agent email, required when scope=agent
	}
	json.NewDecoder(r.Body).Decode(&req)

	// Same key-name cap as /v1 createApiKey (identity.MaxAPIKeyNameLen),
	// counted in Unicode code points to match the OpenAPI maxLength
	// semantics — the dashboard path must not be a bypass around the
	// request-edge bound.
	if n := utf8.RuneCountInString(req.Name); n > identity.MaxAPIKeyNameLen {
		http.Error(w, fmt.Sprintf("name too long — %d characters, max %d", n, identity.MaxAPIKeyNameLen), http.StatusBadRequest)
		return
	}

	// Parse optional expires_at. Empty string and missing field both mean
	// "never expires" — symmetric with the NULL column default. Malformed
	// or already-past timestamps are client errors, not "use NULL silently."
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			http.Error(w, "expires_at must be an RFC 3339 timestamp", http.StatusBadRequest)
			return
		}
		if !t.After(time.Now()) {
			http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
			return
		}
		expiresAt = &t
	}

	// Default to an account-scoped key (the pre-Slice-5a behavior). When the
	// caller asks for an agent-scoped key, resolve the named agent to its id —
	// CreateScopedAPIKey re-checks ownership, so a wrong/foreign agent is
	// rejected rather than minting an over-broad or cross-tenant key.
	scope := req.Scope
	if scope == "" {
		scope = identity.ScopeAccount
	}
	if !identity.ValidScope(scope) {
		http.Error(w, "scope must be 'account' or 'agent'", http.StatusBadRequest)
		return
	}
	var agentID string
	if scope == identity.ScopeAgent {
		if req.Agent == "" {
			http.Error(w, "agent (email) is required when scope=agent", http.StatusBadRequest)
			return
		}
		ag, err := ua.store.GetAgentByEmail(r.Context(), identity.NormalizeEmail(req.Agent))
		if err != nil || ag == nil || ag.UserID != user.ID {
			http.Error(w, "agent not found", http.StatusBadRequest)
			return
		}
		agentID = ag.ID
	}

	key, err := ua.store.CreateScopedAPIKey(r.Context(), user.ID, req.Name, scope, agentID, expiresAt)
	if err != nil {
		http.Error(w, "failed to create API key", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, key)
}

// HandleListAPIKeys lists API keys for the authenticated user (without key values).
func (ua *UserAuth) HandleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	user := ua.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	keys, err := ua.store.ListAPIKeys(r.Context(), user.ID, 0, time.Time{}, "")
	if err != nil {
		http.Error(w, "failed to list API keys", http.StatusInternalServerError)
		return
	}
	if keys == nil {
		keys = []identity.APIKey{}
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, keys)
}

// HandleDeleteAPIKey deletes an API key owned by the authenticated user.
func (ua *UserAuth) HandleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	user := ua.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	keyID := path
	if keyID == "" {
		http.Error(w, "key id required", http.StatusBadRequest)
		return
	}

	if err := ua.store.DeleteAPIKey(r.Context(), keyID, user.ID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
}
