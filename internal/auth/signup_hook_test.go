package auth_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/identity"
)

// The signup hook fires from HandleCallback — asynchronously, best-effort,
// and only when the OAuth upsert actually INSERTed a new user row. These
// tests drive the same fake-Google harness as cli_login_test.go and point
// the hook at a recording httptest server. The fake userinfo endpoint
// always returns sub "google-sub-cli-test" / email "cliuser@test.com" /
// name "CLI User"; the per-test TestDB truncation means each test decides
// whether that identity is new or returning.

type recordedSignupHook struct {
	mu        sync.Mutex
	called    bool
	body      []byte
	signature string
}

func (r *recordedSignupHook) snapshot() (bool, []byte, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called, r.body, r.signature
}

// waitForSignupHook polls until the async hook goroutine has landed or the
// deadline passes. Returns whether it was called.
func (r *recordedSignupHook) waitForSignupHook(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if called, _, _ := r.snapshot(); called {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	called, _, _ := r.snapshot()
	return called
}

// newRecordingSignupHookServer starts an httptest server that records the
// hook body + signature header and responds 204.
func newRecordingSignupHookServer(t *testing.T) (*httptest.Server, *recordedSignupHook) {
	t.Helper()
	rec := &recordedSignupHook{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.body = body
		rec.signature = r.Header.Get("X-E2A-Internal-Signature")
		rec.called = true
		rec.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// runWebCallback drives HandleCallback through a plain web login (no CLI
// handoff, no return_to) with a matching nonce cookie and returns the
// recorder.
func runWebCallback(t *testing.T, ua *auth.UserAuth) *httptest.ResponseRecorder {
	t.Helper()
	nonce := "signup-hook-nonce"
	state := auth.EncodeOAuthState(&auth.OAuthState{Nonce: nonce})
	req := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/auth/callback?code=fake-code&state=%s", url.QueryEscape(state)),
		nil,
	)
	req.AddCookie(&http.Cookie{Name: "e2a_oauth_state", Value: nonce})
	w := httptest.NewRecorder()
	ua.HandleCallback(w, req)
	return w
}

// signupHMAC re-derives the expected X-E2A-Internal-Signature for the
// recorded body — computed independently so a bug on either side shows up.
func signupHMAC(secret string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// TestHandleCallback_NewUser_FiresSignupHook: a first-time OAuth signup
// POSTs {user_id, email, name} to the hook with a matching HMAC signature,
// and the login itself still lands on /dashboard.
func TestHandleCallback_NewUser_FiresSignupHook(t *testing.T) {
	secret := "test-signup-secret"
	ua, store, _ := setupUserAuthWithFakeOAuth(t)
	hookSrv, rec := newRecordingSignupHookServer(t)
	ua.SetSignupHook(hookSrv.URL, secret)

	w := runWebCallback(t, ua)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusFound, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/dashboard") {
		t.Fatalf("expected redirect to dashboard, got %q", loc)
	}

	if !rec.waitForSignupHook(3 * time.Second) {
		t.Fatalf("signup hook was not called for a new user")
	}
	_, body, signature := rec.snapshot()

	var payload struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("hook body not JSON: %v (body=%q)", err, string(body))
	}
	// Resolve the user the callback created (idempotent upsert on the
	// fake userinfo's fixed subject) and compare against the payload.
	user, created, err := store.CreateOrGetUserWithCreated(context.Background(), "cliuser@test.com", "CLI User", "google-sub-cli-test")
	if err != nil {
		t.Fatalf("CreateOrGetUserWithCreated: %v", err)
	}
	if created {
		t.Fatalf("callback should already have created the user")
	}
	if payload.UserID != user.ID {
		t.Errorf("hook user_id = %q, want %q", payload.UserID, user.ID)
	}
	if payload.Email != "cliuser@test.com" {
		t.Errorf("hook email = %q, want %q", payload.Email, "cliuser@test.com")
	}
	if payload.Name != "CLI User" {
		t.Errorf("hook name = %q, want %q", payload.Name, "CLI User")
	}
	if want := signupHMAC(secret, body); signature != want {
		t.Errorf("signature mismatch:\n  got      %s\n  expected %s", signature, want)
	}
}

// TestHandleCallback_ReturningUser_DoesNotFireSignupHook: a login by an
// existing user (same google_subject) takes the upsert's UPDATE path and
// must not re-fire the provisioning hook.
func TestHandleCallback_ReturningUser_DoesNotFireSignupHook(t *testing.T) {
	ua, store, _ := setupUserAuthWithFakeOAuth(t)
	hookSrv, rec := newRecordingSignupHookServer(t)
	ua.SetSignupHook(hookSrv.URL, "test-signup-secret")

	// Pre-create the user the fake userinfo endpoint will report.
	if _, err := store.CreateOrGetUser(context.Background(), "cliuser@test.com", "CLI User", "google-sub-cli-test"); err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	w := runWebCallback(t, ua)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusFound, w.Body.String())
	}

	// The hook fires (if at all) from a goroutine; give a wrongly-fired
	// one a moment to land before asserting silence.
	if rec.waitForSignupHook(300 * time.Millisecond) {
		t.Errorf("signup hook fired for a returning user")
	}
}

// TestHandleCallback_NoSignupHookConfigured: with an empty hook URL (the
// self-host default) a new-user signup completes with no outbound call.
func TestHandleCallback_NoSignupHookConfigured(t *testing.T) {
	ua, _, _ := setupUserAuthWithFakeOAuth(t)
	_, rec := newRecordingSignupHookServer(t)
	// Recording server exists but is deliberately not wired in: empty
	// URL disables the hook even with a secret configured.
	ua.SetSignupHook("", "test-signup-secret")

	w := runWebCallback(t, ua)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusFound, w.Body.String())
	}
	if rec.waitForSignupHook(300 * time.Millisecond) {
		t.Errorf("signup hook fired despite empty URL")
	}
}

// TestHandleCallback_SignupHookDown_LoginStillSucceeds: an unreachable hook
// endpoint must never break or block a first-time login — best-effort means
// the failure is logged and dropped.
func TestHandleCallback_SignupHookDown_LoginStillSucceeds(t *testing.T) {
	ua, store, _ := setupUserAuthWithFakeOAuth(t)
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close() // connection refused from here on
	ua.SetSignupHook(deadURL, "test-signup-secret")

	w := runWebCallback(t, ua)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusFound, w.Body.String())
	}

	// The user was still created despite the hook outage.
	var user *identity.User
	user, created, err := store.CreateOrGetUserWithCreated(context.Background(), "cliuser@test.com", "CLI User", "google-sub-cli-test")
	if err != nil {
		t.Fatalf("CreateOrGetUserWithCreated: %v", err)
	}
	if created || user == nil {
		t.Fatalf("callback should have created the user even with the hook down")
	}
}
