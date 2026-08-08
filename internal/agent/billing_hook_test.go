package agent_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

// The billing hook fires from DeleteUserDataCore (best-effort, AFTER the
// cascade commits). The legacy DELETE /api/v1/users/me route that once reached it was
// removed in the v1 cutover — /v1's deleteAccount now calls the same core —
// so these tests drive DeleteUserDataCore directly. The /v1 httpapi test for
// deleteAccount uses a fake DeleteUserData closure and therefore cannot cover
// the hook-firing / HMAC / cascade-proceeds-on-failure behavior; this is its
// only home.

type recordedHookCall struct {
	mu        sync.Mutex
	called    bool
	body      []byte
	signature string
}

// setupCoreAPIWithBillingHook wires an *agent.API to a real test DB with the
// billing hook pointed at a recording httptest server that responds with
// hookStatus.
func setupCoreAPIWithBillingHook(t *testing.T, secret string, hookStatus int) (*agent.API, *identity.Store, *recordedHookCall) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")

	rec := &recordedHookCall{}
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.body = body
		rec.signature = r.Header.Get("X-E2A-Internal-Signature")
		rec.called = true
		rec.mu.Unlock()
		w.WriteHeader(hookStatus)
	}))
	t.Cleanup(hookSrv.Close)

	api := agent.NewAPI(store, sender, smtpRelay, nil, usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetInternalAPISecret(secret)
	api.SetBillingHookURL(hookSrv.URL)
	return api, store, rec
}

// expectedHMAC re-derives the X-E2A-Internal-Signature for the recorded body.
// Independent computation, so a bug in either side shows up.
func expectedHMAC(secret string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// TestDeleteUser_FiresBillingHook: after deletion commits, billing is notified
// with the user_id body + a matching HMAC signature.
func TestDeleteUser_FiresBillingHook(t *testing.T) {
	secret := "test-internal-secret"
	api, store, rec := setupCoreAPIWithBillingHook(t, secret, http.StatusNoContent)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "hook-fires@test.com", "Test", "google-hook-fires@test.com")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	if _, err := api.DeleteUserDataCore(ctx, user); err != nil {
		t.Fatalf("DeleteUserDataCore: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.called {
		t.Fatalf("billing hook was not called")
	}
	var hookBody struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(rec.body, &hookBody); err != nil {
		t.Fatalf("hook body not JSON: %v (body=%q)", err, string(rec.body))
	}
	if hookBody.UserID != user.ID {
		t.Errorf("hook user_id = %q, want %q", hookBody.UserID, user.ID)
	}
	if got, want := rec.signature, expectedHMAC(secret, rec.body); got != want {
		t.Errorf("signature mismatch:\n  got      %s\n  expected %s", got, want)
	}
	if _, err := store.GetUserByID(ctx, user.ID); err == nil {
		t.Errorf("user still exists after delete; cascade should have removed them")
	}
}

// TestDeleteUser_HookFailureDoesNotBlockDeletion: a billing-side outage (hook
// 500) must not hold the user's right-of-deletion hostage — the cascade runs
// regardless.
func TestDeleteUser_HookFailureDoesNotBlockDeletion(t *testing.T) {
	api, store, rec := setupCoreAPIWithBillingHook(t, "secret", http.StatusInternalServerError)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "hook-fails@test.com", "Test", "google-hook-fails@test.com")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	if _, err := api.DeleteUserDataCore(ctx, user); err != nil {
		t.Fatalf("DeleteUserDataCore should not error on hook failure: %v", err)
	}

	rec.mu.Lock()
	called := rec.called
	rec.mu.Unlock()
	if !called {
		t.Errorf("hook should have been attempted after deletion")
	}
	if _, err := store.GetUserByID(ctx, user.ID); err == nil {
		t.Errorf("hook failed but user was not deleted; cascade must proceed regardless")
	}
}

func TestDeleteUser_SendInProgressDoesNotNotifyBilling(t *testing.T) {
	api, store, rec := setupCoreAPIWithBillingHook(t, "secret", http.StatusNoContent)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "billing-send-in-progress")
	message, err := store.CreateOutboundMessage(ctx, ag.ID,
		[]string{"recipient@example.com"}, nil, nil, "sending", "send", "smtp", "", "", []byte("raw"))
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE messages
			    SET delivery_status = 'sending',
			        send_claimed_at = now(),
			        send_job_id = 7001
			  WHERE id = $1`,
			message.ID)
		return err
	}); err != nil {
		t.Fatalf("mark sending: %v", err)
	}

	if _, err := api.DeleteUserDataCore(ctx, user); !errors.Is(err, identity.ErrSendInProgress) {
		t.Fatalf("DeleteUserDataCore = %v, want ErrSendInProgress", err)
	}
	rec.mu.Lock()
	called := rec.called
	rec.mu.Unlock()
	if called {
		t.Fatal("billing hook was called even though account deletion did not commit")
	}
	if _, err := store.GetUserByID(ctx, user.ID); err != nil {
		t.Fatalf("user missing after send_in_progress conflict: %v", err)
	}
}

// TestDeleteUser_NoHookConfigured: the self-host pattern (no billing hook URL)
// still deletes, with no outbound HTTP call.
func TestDeleteUser_NoHookConfigured(t *testing.T) {
	api, store, _ := setupCoreAPI(t) // no SetBillingHookURL
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "no-hook@test.com", "Test", "google-no-hook@test.com")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	if _, err := api.DeleteUserDataCore(ctx, user); err != nil {
		t.Fatalf("DeleteUserDataCore: %v", err)
	}
	if _, err := store.GetUserByID(ctx, user.ID); err == nil {
		t.Errorf("user still exists after delete")
	}
}
