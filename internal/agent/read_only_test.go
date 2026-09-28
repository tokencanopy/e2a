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

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/approvaltoken"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

// Read-only accounts on the legacy surface (docs/design/account-read-only.md),
// against real Postgres: the dashboard's legacy account routes and OAuth
// consent refuse writes for an abuse-paused account, reads and sign-out keep
// working, and other pause classes change nothing.

func setPause(t *testing.T, store *identity.Store, userID, state, class string) {
	t.Helper()
	ctx := context.Background()
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO account_sending_controls (user_id, state, reason, actor, pause_class)
			VALUES ($1, $2, 'synthetic', 'test', $3)
			ON CONFLICT (user_id) DO UPDATE SET state = $2, pause_class = $3`, userID, state, class)
		return err
	}); err != nil {
		t.Fatalf("set pause: %v", err)
	}
}

func setupLegacyReadOnlyAPI(t *testing.T) (*httptest.Server, *identity.Store) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	userAuth := auth.NewUserAuth(&config.OAuthConfig{}, store, false)
	api := agent.NewAPI(store, sender, smtpRelay, userAuth, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetSupportContact("help@example.test")
	router := mux.NewRouter()
	api.RegisterRoutes(router)
	// An unclassified write route mounted after RegisterRoutes, the way
	// cmd/e2a mounts POST /webhooks/ses on the same router.
	router.HandleFunc("/test/unclassified-write", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}).Methods(http.MethodPost)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, store
}

func legacyDo(t *testing.T, method, url, session, body string) (int, string, string) {
	t.Helper()
	return legacyDoAuth(t, method, url, session, "", body)
}

// legacyDoAuth sends a legacy request with the session cookie (when session is
// non-empty) and an Authorization header (when authz is non-empty).
func legacyDoAuth(t *testing.T, method, url, session, authz, body string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session})
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env.Error.Code, env.Error.Message
}

func TestLegacyAccountWritesRefusedForAnAbusePausedAccount(t *testing.T) {
	server, store := setupLegacyReadOnlyAPI(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "legacy-ro@example.test", "RO", "sub-legacy-ro")
	if err != nil {
		t.Fatal(err)
	}
	ag, err := store.CreateAgent(ctx, "ro-bot@agents.e2a.dev", "agents.e2a.dev", "RO", "", "cloud", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.CreateAPIKey(ctx, user.ID, "existing", nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateUserSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	setPause(t, store, user.ID, "paused", "abuse")

	writes := []struct{ method, path, body string }{
		{http.MethodPatch, "/api/auth/me", `{"name":"Brand Name Inc"}`},
		{http.MethodPut, "/api/dashboard/agents/" + ag.EmailAddress(), `{"name":"Brand Name Support"}`},
		{http.MethodDelete, "/api/dashboard/agents/" + ag.EmailAddress(), ``},
		{http.MethodPost, "/api/keys", `{"name":"new"}`},
		{http.MethodDelete, "/api/keys/" + key.ID, ``},
	}
	for _, w := range writes {
		status, code, msg := legacyDo(t, w.method, server.URL+w.path, session, w.body)
		if status != http.StatusForbidden || code != "account_read_only" {
			t.Errorf("%s %s = %d %q, want 403 account_read_only", w.method, w.path, status, code)
			continue
		}
		if !strings.Contains(msg, "help@example.test") || strings.Contains(msg, "synthetic") {
			t.Errorf("%s %s message %q: want the support contact and never the pause reason", w.method, w.path, msg)
		}
	}
	// The guard authenticates exactly as the handlers do (the session cookie)
	// and cannot be steered onto another identity by an Authorization header:
	// a junk bearer, or a valid key of another, unpaused account, alongside
	// the read-only session is refused outright on every cookie-only route.
	other, err := store.CreateOrGetUser(ctx, "legacy-other@example.test", "Other", "sub-legacy-other")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := store.CreateAPIKey(ctx, other.ID, "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, authz := range []struct{ name, header string }{
		{"junk bearer", "Bearer junk"},
		{"foreign account key", "Bearer " + otherKey.PlaintextKey},
	} {
		for _, w := range writes {
			status, code, _ := legacyDoAuth(t, w.method, server.URL+w.path, session, authz.header, w.body)
			if status != http.StatusBadRequest || code != "ambiguous_credentials" {
				t.Errorf("%s: %s %s = %d %q, want 400 ambiguous_credentials", authz.name, w.method, w.path, status, code)
			}
		}
	}
	// A bare bearer (no session) on a cookie-only route is refused the same
	// way: the route cannot authenticate it and the guard will not guess.
	for _, w := range writes {
		status, code, _ := legacyDoAuth(t, w.method, server.URL+w.path, "", "Bearer "+key.PlaintextKey, w.body)
		if status != http.StatusBadRequest || code != "ambiguous_credentials" {
			t.Errorf("bare bearer: %s %s = %d %q, want 400 ambiguous_credentials", w.method, w.path, status, code)
		}
	}
	// OAuth consent enforces read-only in its handler, after its own
	// provider/authorize handling (TestConsentRefusesAllowButNotDenyWhileReadOnly):
	// on this deployment, which has no OAuth provider, it is a plain 404 for
	// any caller.
	for _, sess := range []string{session, ""} {
		if status, code, _ := legacyDoAuth(t, http.MethodPost, server.URL+"/oauth2/consent", sess, "", ""); status != http.StatusNotFound {
			t.Errorf("consent without an OAuth provider (session=%v) = %d %q, want 404", sess != "", status, code)
		}
	}
	// No credential at all: refused by the guard with 401, never passed on.
	for _, w := range writes {
		if status, _, _ := legacyDoAuth(t, w.method, server.URL+w.path, "", "", w.body); status != http.StatusUnauthorized {
			t.Errorf("anonymous: %s %s = %d, want 401", w.method, w.path, status)
		}
	}

	// Nothing changed underneath the refusals.
	if got, err := store.GetAgentByEmail(ctx, ag.EmailAddress()); err != nil || got == nil || got.Name != "RO" {
		t.Fatalf("agent changed or vanished under a refused write: %+v err=%v", got, err)
	}
	if got, err := store.GetUserByID(ctx, user.ID); err != nil || got.Name != "RO" {
		t.Fatalf("profile changed under a refused write: %+v err=%v", got, err)
	}
	keys, err := store.ListAPIKeys(ctx, user.ID, 100, time.Time{}, "")
	if err != nil || len(keys) != 1 {
		t.Fatalf("api keys after refused writes = %d (err %v), want the one existing key", len(keys), err)
	}

	// Reads and sign-out keep working.
	if status, _, _ := legacyDo(t, http.MethodGet, server.URL+"/api/auth/me", session, ""); status != http.StatusOK {
		t.Errorf("GET /api/auth/me = %d, want 200", status)
	}
	if status, _, _ := legacyDo(t, http.MethodGet, server.URL+"/api/keys", session, ""); status != http.StatusOK {
		t.Errorf("GET /api/keys = %d, want 200", status)
	}
	// Sign-out (which redirects) is not refused, and it ends the session.
	if status, code, _ := legacyDo(t, http.MethodPost, server.URL+"/api/auth/logout", session, ""); code == "account_read_only" || status == http.StatusForbidden {
		t.Errorf("POST /api/auth/logout = %d %q, want it allowed", status, code)
	}
	if status, _, _ := legacyDo(t, http.MethodGet, server.URL+"/api/auth/me", session, ""); status != http.StatusUnauthorized {
		t.Errorf("GET /api/auth/me after sign-out = %d, want 401 (the session must be gone)", status)
	}
}

// An unclassified legacy write route is refused for a read-only account
// whichever credential names it, a credential that fails to resolve is
// refused, and an anonymous request (no account) still reaches the handler.
func TestUnclassifiedLegacyWriteRouteDefaultsToRefuse(t *testing.T) {
	server, store := setupLegacyReadOnlyAPI(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "legacy-uncl@example.test", "U", "sub-legacy-uncl")
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.CreateAPIKey(ctx, user.ID, "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateUserSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateOrGetUser(ctx, "legacy-uncl-other@example.test", "O", "sub-legacy-uncl-other")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := store.CreateAPIKey(ctx, other.ID, "o", nil)
	if err != nil {
		t.Fatal(err)
	}
	url := server.URL + "/test/unclassified-write"
	if status, code, _ := legacyDoAuth(t, http.MethodPost, url, session, "", ""); status != http.StatusNoContent {
		t.Fatalf("writable account: %d %q, want 204", status, code)
	}
	setPause(t, store, user.ID, "paused", "abuse")
	cases := []struct {
		name, session, authz string
		want                 int
	}{
		{"anonymous", "", "", http.StatusNoContent},
		{"stale session", "sess_unknown", "", http.StatusNoContent},
		{"read-only session", session, "", http.StatusForbidden},
		{"read-only key", "", "Bearer " + key.PlaintextKey, http.StatusForbidden},
		{"foreign key + read-only session", session, "Bearer " + otherKey.PlaintextKey, http.StatusForbidden},
		{"foreign key alone", "", "Bearer " + otherKey.PlaintextKey, http.StatusNoContent},
		{"junk bearer", "", "Bearer junk", http.StatusUnauthorized},
		{"junk bearer + read-only session", session, "Bearer junk", http.StatusUnauthorized},
	}
	for _, c := range cases {
		if status, code, _ := legacyDoAuth(t, http.MethodPost, url, c.session, c.authz, ""); status != c.want {
			t.Errorf("%s: %d %q, want %d", c.name, status, code, c.want)
		}
	}
}

func TestLegacyAccountWritesUnaffectedByOtherPauseClasses(t *testing.T) {
	server, store := setupLegacyReadOnlyAPI(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "legacy-op@example.test", "OP", "sub-legacy-op")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateUserSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"operator", "billing", "system"} {
		setPause(t, store, user.ID, "paused", class)
		if status, code, _ := legacyDo(t, http.MethodPatch, server.URL+"/api/auth/me", session, `{"name":"n-`+class+`"}`); status != http.StatusOK {
			t.Errorf("%s pause: PATCH /api/auth/me = %d %q, want 200", class, status, code)
		}
	}
	// A resume after an abuse pause lifts read-only at once.
	setPause(t, store, user.ID, "paused", "abuse")
	if status, code, _ := legacyDo(t, http.MethodPatch, server.URL+"/api/auth/me", session, `{"name":"paused"}`); code != "account_read_only" {
		t.Fatalf("abuse pause: PATCH = %d %q, want account_read_only", status, code)
	}
	setPause(t, store, user.ID, "active", "abuse")
	if status, code, _ := legacyDo(t, http.MethodPatch, server.URL+"/api/auth/me", session, `{"name":"resumed"}`); status != http.StatusOK {
		t.Fatalf("after resume: PATCH = %d %q, want 200", status, code)
	}
}

func TestAttachExternalPrincipalRefusesAReadOnlyAccount(t *testing.T) {
	server, store := setupAttachAPI(t, attachTestIssuer)
	ctx := context.Background()
	user, err := store.BootstrapUser(ctx, "attach-ro@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// A principal attached before the pause replays idempotently (200): the
	// reconciler re-sends attaches it has already made, and a replay changes
	// nothing.
	resp := attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "principal-before", user.ID))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("attach before the pause = %d, want 201", resp.StatusCode)
	}
	_ = resp.Body.Close()
	setPause(t, store, user.ID, "paused", "abuse")
	resp = attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "principal-before", user.ID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("idempotent re-attach to a read-only account = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	// A NEW principal for a read-only account is refused.
	resp = attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "principal-ro", user.ID))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("attach of a new principal to a read-only account = %d, want 403", resp.StatusCode)
	}
	if got := decodeAttachJSON(t, resp)["error"]; got != "account_read_only" {
		t.Fatalf("attach error = %q, want account_read_only", got)
	}
	if u, err := store.GetUserByExternalPrincipal(ctx, attachTestIssuer, "principal-ro"); err == nil && u != nil {
		t.Fatalf("a refused attach created a mapping to %s", u.ID)
	}
	// An operator pause does not block it.
	setPause(t, store, user.ID, "paused", "operator")
	resp = attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "principal-ro", user.ID))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("attach under an operator pause = %d, want 201", resp.StatusCode)
	}
}

func TestMagicLinksRefuseAReadOnlyAccount(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	a, userID := prepareHITLAgent(t, store, "magic-ro")
	msg := issuePending(t, store, a.ID)
	setPause(t, store, userID, "paused", "abuse")

	for _, action := range []string{approvaltoken.ActionApprove, approvaltoken.ActionReject} {
		tok, _ := signer.Sign(msg.ID, action, time.Now().Add(time.Hour))
		resp := postForm(t, server.URL+"/v1/"+action, map[string]string{"t": tok, "reason": "x"})
		body := readBody(t, resp)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "read-only") {
			t.Errorf("magic %s on a read-only account = %d, want 403 read-only page; body: %s", action, resp.StatusCode, body)
		}
	}
	got, _ := store.GetOutboundMessageForUser(context.Background(), msg.ID, userID)
	if got.Status != identity.MessageStatusPendingReview {
		t.Fatalf("held message status = %q after refused magic links, want still pending", got.Status)
	}
}

// OAuth consent while read-only: allow (which mints a grant) is refused with
// account_read_only and creates nothing; deny still reaches the client as
// fosite's access_denied redirect. An unauthenticated caller still sees
// consent's own authorize-request handling before any session or read-only
// check.
func TestConsentRefusesAllowButNotDenyWhileReadOnly(t *testing.T) {
	f := newConsentFixture(t)
	setPause(t, f.store, f.userID, "paused", "abuse")
	_, challenge := newPKCE(t)

	allow := authorizeParams(challenge, f.clientID, "s1s1s1s1s1s1s1s1")
	allow.Set("action", "allow")
	allow.Set("agent_choice", "create_new")
	allow.Set("new_agent_slug", "roconsentbot")
	resp := f.consentPOST(t, allow)
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || env.Error.Code != "account_read_only" {
		t.Fatalf("allow while read-only = %d %q, want 403 account_read_only", resp.StatusCode, env.Error.Code)
	}
	var agents, codes int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_identities WHERE user_id = $1`, f.userID).Scan(&agents); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_auth_codes WHERE user_id = $1`, f.userID).Scan(&codes); err != nil {
		t.Fatal(err)
	}
	if agents != 0 || codes != 0 {
		t.Fatalf("refused consent created %d agents and %d auth codes, want none", agents, codes)
	}

	deny := authorizeParams(challenge, f.clientID, "s2s2s2s2s2s2s2s2")
	deny.Set("action", "deny")
	resp = f.consentPOST(t, deny)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deny while read-only = %d, want the 302/303 access_denied redirect", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loc.Query().Get("error"); got != "access_denied" || !strings.HasPrefix(loc.String(), "http://localhost:8765/callback") {
		t.Fatalf("deny while read-only redirected to %q, want the client callback with error=access_denied", loc.String())
	}

	// Unauthenticated, with an authorize request the provider rejects (an
	// unregistered redirect_uri): consent's own authorize error, not a
	// session 401 from any guard.
	bad := authorizeParams(challenge, f.clientID, "s3s3s3s3s3s3s3s3")
	bad.Set("redirect_uri", "http://localhost:9999/not-registered")
	bad.Set("action", "allow")
	anon, err := http.Post(f.server.URL+"/oauth2/consent", "application/x-www-form-urlencoded", strings.NewReader(bad.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(anon.Body)
	_ = anon.Body.Close()
	if anon.StatusCode == http.StatusUnauthorized || anon.StatusCode == http.StatusForbidden || !strings.Contains(string(body), "invalid_request") {
		t.Fatalf("unauthenticated consent with a bad authorize request = %d %s, want consent's own invalid_request", anon.StatusCode, body)
	}
}
