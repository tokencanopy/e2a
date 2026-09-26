package apiserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/apiserver"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

// The restore interstitial over real HTTP, against the production handler
// wiring (apiserver.BuildDeps) and a real store: the restricted cookie, the
// same-origin check, and registration_refused when an identity tombstone
// closes the account's identity.
func restoreHTTP(t *testing.T) (*httptest.Server, *identity.Store, *identity.TombstoneKeyring) {
	t.Helper()
	p, store := realParams(t)
	kr, err := identity.ParseTombstoneKeyring("v1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if err != nil {
		t.Fatal(err)
	}
	store.SetTombstonePolicy(identity.TombstonePolicy{Enabled: true, Keyring: kr})
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	p.API = agent.NewAPI(store, outbound.NewSender(relay, "test.e2a.dev"), relay, nil, usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", "https://app.example.test", false)
	srv := httptest.NewServer(apiserver.New(p))
	t.Cleanup(srv.Close)
	return srv, store, kr
}

func postRestore(t *testing.T, srv *httptest.Server, cookie, origin string) (int, map[string]any, *http.Response) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/account/restore", nil)
	req.AddCookie(&http.Cookie{Name: "e2a_restore_session", Value: cookie})
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body, resp
}

func errorCode(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		c, _ := e["code"].(string)
		return c
	}
	return ""
}

func TestRestoreEndpointRefusesAClosedIdentityOverHTTP(t *testing.T) {
	srv, store, kr := restoreHTTP(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "closed@example.test", "C", "sub-closed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	tok, err := store.CreateRestrictedUserSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// An operator hold on this identity (as an abuse purge of the same
	// identifier would leave).
	d, _ := kr.Digest(1, identity.TombstoneKindEmail, identity.NormalizeTombstoneValue(identity.TombstoneKindEmail, "closed@example.test"))
	if _, err := realPool(t, store).Exec(ctx, `
		INSERT INTO identity_tombstones (kind, digest, key_version, class, account_ref, expires_at)
		VALUES ('email', $1, 1, 'abuse', 'usr_other', now() + interval '1 day')`, d); err != nil {
		t.Fatal(err)
	}

	code, body, _ := postRestore(t, srv, tok, "https://app.example.test")
	if code != http.StatusForbidden || errorCode(body) != "registration_refused" {
		t.Fatalf("restore of a closed identity = %d %v, want 403 registration_refused", code, body)
	}
	if u, err := store.GetUserByIDAnyState(ctx, user.ID); err != nil || u.DeletedAt == nil {
		t.Fatalf("a refused restore changed the account: %+v %v", u, err)
	}
}

func TestRestoreEndpointReissuesAnOrdinarySessionOverHTTP(t *testing.T) {
	srv, store, _ := restoreHTTP(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "back@example.test", "B", "sub-back")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	tok, err := store.CreateRestrictedUserSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The restricted token presented as an ordinary session cookie is useless.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/account", nil)
	req.AddCookie(&http.Cookie{Name: "e2a_session", Value: tok})
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("restricted token as e2a_session: %v %v", resp, err)
	}

	code, body, resp := postRestore(t, srv, tok, "https://app.example.test")
	if code != http.StatusOK || body["restored"] != true {
		t.Fatalf("restore = %d %v", code, body)
	}
	var session, cleared bool
	for _, c := range resp.Cookies() {
		if c.Name == "e2a_session" && c.Value == tok && c.MaxAge > 0 {
			session = true
		}
		if c.Name == "e2a_restore_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !session || !cleared {
		t.Fatalf("cookies after restore: session=%v restricted cleared=%v", session, cleared)
	}
}

// realPool reopens the per-package test database the store uses.
func realPool(t *testing.T, _ *identity.Store) interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
} {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testutil.TestDBURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestRestoreEndpointRejectsCrossSiteForARestorableAccount (M8a): the CSRF
// refusal is proven on an identity that WOULD restore — the cross-site POST
// is refused with code forbidden and changes nothing, and the same session
// from the dashboard origin then restores.
func TestRestoreEndpointRejectsCrossSiteForARestorableAccount(t *testing.T) {
	srv, store, _ := restoreHTTP(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "csrf@example.test", "C", "sub-csrf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	tok, err := store.CreateRestrictedUserSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://evil.example.test", ""} {
		code, body, _ := postRestore(t, srv, tok, origin)
		if code != http.StatusForbidden || errorCode(body) != "forbidden" {
			t.Fatalf("restore from origin %q = %d %v, want 403 forbidden", origin, code, body)
		}
	}
	if u, err := store.GetUserByIDAnyState(ctx, user.ID); err != nil || u.DeletedAt == nil {
		t.Fatalf("a cross-site POST restored the account: %+v %v", u, err)
	}
	if code, body, _ := postRestore(t, srv, tok, "https://app.example.test"); code != http.StatusOK {
		t.Fatalf("same-origin restore = %d %v, want 200 (the identity is restorable)", code, body)
	}
}
