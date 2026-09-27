package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, store
}

func legacyDo(t *testing.T, method, url, session, body string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session})
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
		{http.MethodPost, "/oauth2/consent", ``},
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
	// Nothing changed underneath the refusals.
	if got, err := store.GetAgentByEmail(ctx, ag.EmailAddress()); err != nil || got == nil || got.Name != "RO" {
		t.Fatalf("agent changed or vanished under a refused write: %+v err=%v", got, err)
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
	setPause(t, store, user.ID, "paused", "abuse")
	resp := attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "principal-ro", user.ID))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("attach to a read-only account = %d, want 403", resp.StatusCode)
	}
	if got := decodeAttachJSON(t, resp)["error"]; got != "account_read_only" {
		t.Fatalf("attach error = %q, want account_read_only", got)
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
