package agent_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
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

// setupAPIWithProvisioning returns a live test server plus the underlying
// store so tests can verify the rows the endpoint writes. testutil.TestDB(t)
// truncates the schema on every call — callers must NOT call it again from
// inside the test or they'll wipe their own setup.
func setupAPIWithProvisioning(t *testing.T, provisioningEnabled bool, provisioningSecret string) (*httptest.Server, *identity.Store) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	userAuth := auth.NewUserAuth(&config.OAuthConfig{}, store, false)

	api := agent.NewAPI(store, sender, smtpRelay, userAuth, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.ConfigureProvisioning(provisioningEnabled, provisioningSecret)

	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, store
}

func provisionRequest(t *testing.T, server *httptest.Server, secret string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", server.URL+"/api/internal/users/provision", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		h := hmac.New(sha256.New, []byte(secret))
		h.Write(body)
		req.Header.Set("X-E2A-Internal-Signature", hex.EncodeToString(h.Sum(nil)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readProvisionResponse(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("response is not a JSON object: status=%d body=%q", resp.StatusCode, string(buf))
	}
	return out
}

var userIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestProvisionUser_CreateReturns201(t *testing.T) {
	secret := "provision-secret-create"
	server, store := setupAPIWithProvisioning(t, true, secret)

	body := []byte(`{"external_ref":"ext_ref_create1","email":"NewUser@Example.com","name":"New User"}`)
	resp := provisionRequest(t, server, secret, body)
	got := readProvisionResponse(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%v", resp.StatusCode, got)
	}
	if !userIDShape.MatchString(got["user_id"]) {
		t.Errorf("user_id = %q, want 32 lowercase hex chars", got["user_id"])
	}

	user, err := store.GetUserByID(context.Background(), got["user_id"])
	if err != nil {
		t.Fatalf("created user not readable: %v", err)
	}
	if user.GoogleSubject != "bootstrap:ext_ref_create1" {
		t.Errorf("google_subject = %q, want bootstrap:ext_ref_create1", user.GoogleSubject)
	}
	if user.Email != "newuser@example.com" {
		t.Errorf("email = %q, want normalized newuser@example.com", user.Email)
	}
	if user.Name != "New User" {
		t.Errorf("name = %q, want %q", user.Name, "New User")
	}
}

func TestProvisionUser_ReplaySameRefReturns200SameID(t *testing.T) {
	secret := "provision-secret-replay"
	server, _ := setupAPIWithProvisioning(t, true, secret)

	body := []byte(`{"external_ref":"ext_ref_replay1","email":"replay@example.com"}`)
	first := readProvisionResponse(t, provisionRequest(t, server, secret, body))
	second := readProvisionResponse(t, provisionRequest(t, server, secret, body))
	if first["user_id"] == "" || second["user_id"] != first["user_id"] {
		t.Fatalf("replay returned different ids: first=%q second=%q", first["user_id"], second["user_id"])
	}

	// Replay with a DIFFERENT email under the same external_ref: the ref is
	// the idempotency key, so this must return the same user and must NOT
	// rewrite the row's email (and must not 409).
	body2 := []byte(`{"external_ref":"ext_ref_replay1","email":"someone-else@example.com"}`)
	resp := provisionRequest(t, server, secret, body2)
	third := readProvisionResponse(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay with different email: status = %d, want 200; body=%v", resp.StatusCode, third)
	}
	if third["user_id"] != first["user_id"] {
		t.Fatalf("replay with different email returned id %q, want %q", third["user_id"], first["user_id"])
	}
}

func TestProvisionUser_ConcurrentSameRefCreatesExactlyOnce(t *testing.T) {
	secret := "provision-secret-concurrent"
	server, _ := setupAPIWithProvisioning(t, true, secret)
	body := []byte(`{"external_ref":"ext_ref_concurrent1","email":"concurrent@example.com"}`)

	type result struct {
		status int
		userID string
		err    error
	}
	results := make(chan result, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})

	send := func() {
		ready.Done()
		<-start
		req, err := http.NewRequest("POST", server.URL+"/api/internal/users/provision", bytes.NewReader(body))
		if err != nil {
			results <- result{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		h := hmac.New(sha256.New, []byte(secret))
		h.Write(body)
		req.Header.Set("X-E2A-Internal-Signature", hex.EncodeToString(h.Sum(nil)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			results <- result{err: err}
			return
		}
		defer resp.Body.Close()
		var payload map[string]string
		err = json.NewDecoder(resp.Body).Decode(&payload)
		results <- result{status: resp.StatusCode, userID: payload["user_id"], err: err}
	}

	go send()
	go send()
	ready.Wait()
	close(start)

	first := <-results
	second := <-results
	for _, got := range []result{first, second} {
		if got.err != nil {
			t.Fatal(got.err)
		}
	}
	statuses := []int{first.status, second.status}
	sort.Ints(statuses)
	if statuses[0] != http.StatusOK || statuses[1] != http.StatusCreated {
		t.Fatalf("statuses = %v, want [200 201]", statuses)
	}
	if first.userID == "" || first.userID != second.userID {
		t.Fatalf("concurrent requests returned different ids: first=%q second=%q", first.userID, second.userID)
	}
}

func TestProvisionUser_EmailConflictReturns409AndNoRow(t *testing.T) {
	secret := "provision-secret-conflict"
	server, store := setupAPIWithProvisioning(t, true, secret)

	first := readProvisionResponse(t, provisionRequest(t, server, secret,
		[]byte(`{"external_ref":"ext_ref_conflict_a","email":"taken@example.com"}`)))
	if first["user_id"] == "" {
		t.Fatalf("setup create failed: %v", first)
	}

	resp := provisionRequest(t, server, secret,
		[]byte(`{"external_ref":"ext_ref_conflict_b","email":"taken@example.com"}`))
	got := readProvisionResponse(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%v", resp.StatusCode, got)
	}
	if got["error"] != "email_conflict" {
		t.Errorf("error = %q, want email_conflict", got["error"])
	}

	// The losing ref must not have produced a row: replaying it now is a
	// create (201), not a replay (200).
	resp2 := provisionRequest(t, server, secret,
		[]byte(`{"external_ref":"ext_ref_conflict_b","email":"fresh@example.com"}`))
	got2 := readProvisionResponse(t, resp2)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("ref that lost the email race should have no row: status = %d, want 201; body=%v", resp2.StatusCode, got2)
	}
	user, err := store.GetUserByID(context.Background(), got2["user_id"])
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "fresh@example.com" {
		t.Errorf("email = %q, want fresh@example.com", user.Email)
	}
}

func TestProvisionUser_RejectsMissingSignature(t *testing.T) {
	server, _ := setupAPIWithProvisioning(t, true, "provision-secret-missingsig")

	body := []byte(`{"external_ref":"ext_ref_sig1","email":"sig@example.com"}`)
	req, _ := http.NewRequest("POST", server.URL+"/api/internal/users/provision", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing sig: status = %d, want 401", resp.StatusCode)
	}
}

func TestProvisionUser_RejectsWrongSignature(t *testing.T) {
	server, _ := setupAPIWithProvisioning(t, true, "provision-secret-wrongsig")

	body := []byte(`{"external_ref":"ext_ref_sig2","email":"sig2@example.com"}`)
	req, _ := http.NewRequest("POST", server.URL+"/api/internal/users/provision", bytes.NewReader(body))
	req.Header.Set("X-E2A-Internal-Signature", "deadbeef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong sig: status = %d, want 401", resp.StatusCode)
	}
}

func TestProvisionUser_503WhenSecretUnset(t *testing.T) {
	server, _ := setupAPIWithProvisioning(t, true, "")

	body := []byte(`{"external_ref":"ext_ref_disabled","email":"off@example.com"}`)
	req, _ := http.NewRequest("POST", server.URL+"/api/internal/users/provision", bytes.NewReader(body))
	req.Header.Set("X-E2A-Internal-Signature", "anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no-secret config: status = %d, want 503", resp.StatusCode)
	}
}

func TestProvisionUser_503WhenDisabledEvenWithSecretPresent(t *testing.T) {
	server, _ := setupAPIWithProvisioning(t, false, "provision-secret-present-but-disabled")

	body := []byte(`{"external_ref":"ext_ref_disabled_flag","email":"off-flag@example.com"}`)
	resp := provisionRequest(t, server, "provision-secret-present-but-disabled", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("disabled config with secret present: status = %d, want 503", resp.StatusCode)
	}
}

func TestProvisionUser_ValidationFailures(t *testing.T) {
	secret := "provision-secret-validation"
	server, _ := setupAPIWithProvisioning(t, true, secret)

	cases := map[string][]byte{
		"bad email":             []byte(`{"external_ref":"ext_ref_v1","email":"not-an-email"}`),
		"missing email":         []byte(`{"external_ref":"ext_ref_v2"}`),
		"display-name email":    []byte(`{"external_ref":"ext_ref_v3","email":"Bob <bob@example.com>"}`),
		"missing external_ref":  []byte(`{"email":"v4@example.com"}`),
		"oversize external_ref": []byte(`{"external_ref":"` + strings.Repeat("r", 129) + `","email":"v5@example.com"}`),
		"malformed json":        []byte(`{"external_ref":`),
		"empty external_ref":    []byte(`{"external_ref":"","email":"v7@example.com"}`),
	}
	for name, body := range cases {
		resp := provisionRequest(t, server, secret, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestProvisionUser_RejectsOversizedBody(t *testing.T) {
	secret := "provision-secret-oversize"
	server, _ := setupAPIWithProvisioning(t, true, secret)

	// Validly signed body that exceeds the 1 KB cap.
	body := []byte(`{"external_ref":"ext_ref_big","email":"big@example.com","name":"` + strings.Repeat("n", 2048) + `"}`)
	resp := provisionRequest(t, server, secret, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized body: status = %d, want 400", resp.StatusCode)
	}
}
