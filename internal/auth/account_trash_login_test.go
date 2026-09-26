package auth_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// A sign-in that resolves to a TRASHED account must get only the restricted
// session behind the restore interstitial (docs/design/account-soft-deletion
// .md §4.4): no ordinary session, no CLI key, no owner-proof write.

func googleCallback(t *testing.T, ua *auth.UserAuth, st *auth.OAuthState) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/auth/callback?code=fake-code&state=%s", url.QueryEscape(auth.EncodeOAuthState(st))), nil)
	req.AddCookie(&http.Cookie{Name: "e2a_oauth_state", Value: st.Nonce})
	w := httptest.NewRecorder()
	ua.HandleCallback(w, req)
	return w
}

func TestGoogleCallbackOnATrashedAccountIssuesOnlyARestrictedSession(t *testing.T) {
	ua, store, _ := setupUserAuthWithFakeOAuth(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "cliuser@test.com", "CLI User", "google-sub-cli-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}

	// Even a CLI-initiated login must not hand a key to a trashed account.
	w := googleCallback(t, ua, &auth.OAuthState{Nonce: "n-trash", CLICallback: "http://127.0.0.1:43123/callback", CLIState: "s"})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc == "" || !endsWith(loc, auth.AccountRestorePath) {
		t.Fatalf("Location = %q, want the restore interstitial", loc)
	}
	if findCookie(w.Result().Cookies(), auth.SessionCookieName) != nil {
		t.Fatal("a trashed sign-in received the ordinary e2a_session cookie")
	}
	session := findCookie(w.Result().Cookies(), auth.RestoreSessionCookieName)
	if session == nil || session.Value == "" {
		t.Fatal("no restricted session cookie")
	}
	if _, err := store.GetUserSession(ctx, session.Value); err == nil {
		t.Fatal("the cookie resolves as an ORDINARY session")
	}
	if u, err := store.GetRestrictedSession(ctx, session.Value); err != nil || u.ID != user.ID {
		t.Fatalf("restricted session lookup: %+v err=%v", u, err)
	}
	pool, err := pgxpool.New(ctx, testutil.TestDBURL())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var keys int
	var proof *string
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE user_id = $1`, user.ID).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT owner_email_verified_address FROM users WHERE id = $1`, user.ID).Scan(&proof); err != nil {
		t.Fatal(err)
	}
	if keys != 0 || proof != nil {
		t.Fatalf("trashed sign-in minted keys=%d or wrote owner proof=%v", keys, proof)
	}
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func TestGoogleCallbackOnATombstonedIdentityLandsOnTheRefusalPage(t *testing.T) {
	ua, store, _ := setupUserAuthWithFakeOAuth(t)
	ctx := context.Background()
	kr, err := identity.ParseTombstoneKeyring("v1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if err != nil {
		t.Fatal(err)
	}
	store.SetTombstonePolicy(identity.TombstonePolicy{Enabled: true, Keyring: kr})
	user, err := store.CreateOrGetUser(ctx, "cliuser@test.com", "CLI User", "google-sub-cli-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EraseAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	w := googleCallback(t, ua, &auth.OAuthState{Nonce: "n-tomb"})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !endsWith(loc, auth.AccountUnavailablePath+"?code=registration_refused") {
		t.Fatalf("Location = %q, want the unavailable page carrying registration_refused", loc)
	}
	if findCookie(w.Result().Cookies(), auth.SessionCookieName) != nil {
		t.Fatal("a refused sign-in received a session cookie")
	}
}

func TestOIDCCallbackOnATrashedAccountIssuesOnlyARestrictedSession(t *testing.T) {
	fx := setupOIDC(t)
	ctx := context.Background()
	user, err := fx.store.CreateOrGetUser(ctx, "oidc-trash@example.com", "T", "google-sub-oidc-trash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	fx.userID = user.ID
	tx := beginOIDCLogin(t, fx)
	w := httptest.NewRecorder()
	fx.oidc.HandleCallback(w, callbackRequest(tx, "code=valid-code&state="+url.QueryEscape(tx.state)))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "http://app.example.com"+auth.AccountRestorePath {
		t.Fatalf("callback = %d Location=%q", w.Code, w.Header().Get("Location"))
	}
	if findCookie(w.Result().Cookies(), auth.SessionCookieName) != nil {
		t.Fatal("OIDC gave a trashed sign-in the ordinary e2a_session cookie")
	}
	session := findCookie(w.Result().Cookies(), auth.RestoreSessionCookieName)
	if session == nil {
		t.Fatal("no restricted session cookie")
	}
	if _, err := fx.store.GetUserSession(ctx, session.Value); err == nil {
		t.Fatal("OIDC issued an ordinary session to a trashed account")
	}
	assertCallbackMetric(t, fx, oidcMetricEvent{outcome: "account_trashed", trust: "trusted", statusClass: "3xx"})
}

func TestGoogleCallbackEmailCollisionWithATrashedAccountLandsOnThePage(t *testing.T) {
	ua, store, _ := setupUserAuthWithFakeOAuth(t)
	ctx := context.Background()
	// A different subject already holds the fake IdP's email and is trashed.
	holder, err := store.CreateOrGetUser(ctx, "cliuser@test.com", "Holder", "google-sub-original")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, holder.ID, nil); err != nil {
		t.Fatal(err)
	}
	w := googleCallback(t, ua, &auth.OAuthState{Nonce: "n-collide"})
	if w.Code != http.StatusFound || !endsWith(w.Header().Get("Location"), auth.AccountUnavailablePath+"?code=account_trashed") {
		t.Fatalf("callback = %d Location=%q, want the unavailable page (not a 500)", w.Code, w.Header().Get("Location"))
	}
}
