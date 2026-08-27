package agent_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

const (
	attachTestSecret = "attach-test-secret-0123456789abcdef0123456789"
	attachTestIssuer = "https://issuer.example.test/oidc"
)

// setupAttachAPI mirrors setupAPIWithProvisioning plus the delegated
// issuer wiring the attach endpoint gates on.
func setupAttachAPI(t *testing.T, delegatedIssuer string) (*httptest.Server, *identity.Store) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	userAuth := auth.NewUserAuth(&config.OAuthConfig{}, store, false)

	api := agent.NewAPI(store, sender, smtpRelay, userAuth, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.ConfigureProvisioning(true, attachTestSecret)
	api.SetDelegatedIssuer(delegatedIssuer)

	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, store
}

func attachRequest(t *testing.T, server *httptest.Server, secret string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", server.URL+"/api/internal/users/external-principals/attach", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-E2A-Internal-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func attachBody(t *testing.T, issuer, ref, userID string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"issuer": issuer, "external_ref": ref, "user_id": userID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeAttachJSON(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAttachExternalPrincipal_LifecycleStatuses(t *testing.T) {
	server, store := setupAttachAPI(t, attachTestIssuer)
	ctx := context.Background()
	alice, err := store.BootstrapUser(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.BootstrapUser(ctx, "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}

	// 201 on first attach; body echoes the user id.
	resp := attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "principal-1", alice.ID))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first attach status = %d, want 201", resp.StatusCode)
	}
	if got := decodeAttachJSON(t, resp)["user_id"]; got != alice.ID {
		t.Fatalf("user_id = %q, want %q", got, alice.ID)
	}

	// 200 on the identical triple.
	resp = attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "principal-1", alice.ID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", resp.StatusCode)
	}
	if got := decodeAttachJSON(t, resp)["user_id"]; got != alice.ID {
		t.Fatalf("replay user_id = %q, want %q", got, alice.ID)
	}

	// 409 external_principal_conflict when the pair belongs to another user.
	resp = attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "principal-1", bob.ID))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", resp.StatusCode)
	}
	if got := decodeAttachJSON(t, resp)["error"]; got != "external_principal_conflict" {
		t.Fatalf("conflict code = %q, want external_principal_conflict", got)
	}

	// 404 user_not_found for a user id that names no row.
	resp = attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "principal-2", "00000000000000000000000000000000"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-user status = %d, want 404", resp.StatusCode)
	}
	if got := decodeAttachJSON(t, resp)["error"]; got != "user_not_found" {
		t.Fatalf("unknown-user code = %q, want user_not_found", got)
	}
}

func TestAttachExternalPrincipal_SecurityGates(t *testing.T) {
	server, store := setupAttachAPI(t, attachTestIssuer)
	u, err := store.BootstrapUser(context.Background(), "carol@example.com")
	if err != nil {
		t.Fatal(err)
	}
	body := attachBody(t, attachTestIssuer, "principal-3", u.ID)

	// Missing signature.
	resp := attachRequest(t, server, "", body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing signature status = %d, want 401", resp.StatusCode)
	}
	// Wrong signature.
	resp = attachRequest(t, server, "wrong-secret", body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong signature status = %d, want 401", resp.StatusCode)
	}
	// Issuer must byte-equal the configured delegated issuer.
	resp = attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer+"/", "principal-3", u.ID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("issuer mismatch status = %d, want 400", resp.StatusCode)
	}
	// Oversized body.
	big := attachBody(t, attachTestIssuer, strings.Repeat("r", 2000), u.ID)
	resp = attachRequest(t, server, attachTestSecret, big)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 400", resp.StatusCode)
	}
	// external_ref with control characters.
	resp = attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "bad\x01ref", u.ID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("control-char ref status = %d, want 400", resp.StatusCode)
	}
	// external_ref over 128 code points.
	resp = attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, strings.Repeat("r", 129), u.ID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("long ref status = %d, want 400", resp.StatusCode)
	}
	// external_ref at 128 code points passes validation (404s only if the
	// user were unknown; here it attaches).
	resp = attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, strings.Repeat("r", 128), u.ID))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("128-cp ref status = %d, want 201", resp.StatusCode)
	}
}

func TestAttachExternalPrincipal_503WhenNotConfigured(t *testing.T) {
	// No delegated issuer configured: 503 delegated_verifier_not_configured.
	server, store := setupAttachAPI(t, "")
	u, err := store.BootstrapUser(context.Background(), "dave@example.com")
	if err != nil {
		t.Fatal(err)
	}
	resp := attachRequest(t, server, attachTestSecret, attachBody(t, attachTestIssuer, "principal-4", u.ID))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := decodeAttachJSON(t, resp)["error"]; got != "delegated_verifier_not_configured" {
		t.Fatalf("code = %q, want delegated_verifier_not_configured", got)
	}
}

func TestProvisionUser_ExternalIssuerCreatesMapping(t *testing.T) {
	server, store := setupAttachAPI(t, attachTestIssuer)

	body, err := json.Marshal(map[string]string{
		"external_issuer": attachTestIssuer,
		"external_ref":    "principal-7",
		"email":           "erin@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", server.URL+"/api/internal/users/provision", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(attachTestSecret))
	mac.Write(body)
	req.Header.Set("X-E2A-Internal-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("provision status = %d, want 201", resp.StatusCode)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	mapped, err := store.GetUserByExternalPrincipal(context.Background(), attachTestIssuer, "principal-7")
	if err != nil || mapped == nil || mapped.ID != out["user_id"] {
		t.Fatalf("mapping after provision = (%+v, %v), want user %s", mapped, err, out["user_id"])
	}
}
