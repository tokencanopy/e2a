package agent_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

// setupAPIWithLimits returns a live test server plus the underlying
// pool/store/enforcer so callers can write directly to the DB the API
// is reading from. testutil.TestDB(t) creates a fresh pool AND
// truncates the schema on every call — so callers must NOT call it
// again from inside the test or they'll wipe their own setup.
func setupAPIWithLimits(t *testing.T, internalSecret string) (*httptest.Server, *identity.Store, *pgxpool.Pool, limits.Enforcer) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	userAuth := auth.NewUserAuth(&config.OAuthConfig{}, store, false)
	usageStore := usage.NewStore(pool)

	defaults := limits.Defaults{
		PlanCode: "default", MaxAgents: 100, MaxDomains: 10,
		MaxMessagesMonth: 1_000_000, MaxStorageBytes: 1 << 40,
	}
	enf := limits.NewEnforcer(limits.NewStore(pool), usageStore, defaults, 0)

	api := agent.NewAPI(store, sender, smtpRelay, userAuth, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetEnforcer(enf)
	api.SetUsageStore(usageStore)
	api.SetInternalAPISecret(internalSecret)

	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, store, pool, enf
}

// validUserID is the shape identity.generateID emits: 16 random bytes hex
// encoded => 32 lowercase hex chars. NOT a UUID — validating the endpoint's
// user_id as a UUID would reject every real user.
const validUserID = "0123456789abcdef0123456789abcdef"

// recordingEnforcer captures Invalidate calls so a test can assert the
// handler never forwards a malformed user_id (and therefore never mints a
// permanent cache entry for it).
type recordingEnforcer struct {
	limits.Enforcer
	calls []string
}

func (r *recordingEnforcer) Invalidate(userID string) { r.calls = append(r.calls, userID) }

// setupAPIWithRecorder is setupAPIWithLimits with the enforcer swapped for a
// recorder, so tests can observe exactly what reaches Invalidate.
func setupAPIWithRecorder(t *testing.T, internalSecret string) (*httptest.Server, *recordingEnforcer) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	userAuth := auth.NewUserAuth(&config.OAuthConfig{}, store, false)
	usageStore := usage.NewStore(pool)
	rec := &recordingEnforcer{Enforcer: limits.NewEnforcer(
		limits.NewStore(pool), usageStore,
		limits.Defaults{PlanCode: "default", MaxAgents: 100, MaxDomains: 10, MaxMessagesMonth: 1_000_000, MaxStorageBytes: 1 << 40},
		0,
	)}

	api := agent.NewAPI(store, sender, smtpRelay, userAuth, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetEnforcer(rec)
	api.SetUsageStore(usageStore)
	api.SetInternalAPISecret(internalSecret)

	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, rec
}

// postInvalidate signs and posts an invalidate request.
func postInvalidate(t *testing.T, server *httptest.Server, secret, userID string) int {
	t.Helper()
	body := []byte(`{"user_id":"` + userID + `"}`)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	req, _ := http.NewRequest("POST", server.URL+"/api/internal/limits/invalidate", bytes.NewReader(body))
	req.Header.Set("X-E2A-Internal-Signature", hex.EncodeToString(h.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestInvalidateLimits_RejectsMissingSignature(t *testing.T) {
	server, _, _, _ := setupAPIWithLimits(t, "test-secret-1")

	body := []byte(`{"user_id":"u_x"}`)
	req, _ := http.NewRequest("POST", server.URL+"/api/internal/limits/invalidate", bytes.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing sig: status = %d, want 401", resp.StatusCode)
	}
}

func TestInvalidateLimits_RejectsWrongSignature(t *testing.T) {
	server, _, _, _ := setupAPIWithLimits(t, "test-secret-2")

	body := []byte(`{"user_id":"u_x"}`)
	req, _ := http.NewRequest("POST", server.URL+"/api/internal/limits/invalidate", bytes.NewReader(body))
	req.Header.Set("X-E2A-Internal-Signature", "deadbeef")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong sig: status = %d, want 401", resp.StatusCode)
	}
}

func TestInvalidateLimits_AcceptsCorrectSignature(t *testing.T) {
	secret := "test-secret-3"
	server, _, _, _ := setupAPIWithLimits(t, secret)

	body := []byte(`{"user_id":"0123456789abcdef0123456789abcdef"}`)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	sig := hex.EncodeToString(h.Sum(nil))

	req, _ := http.NewRequest("POST", server.URL+"/api/internal/limits/invalidate", bytes.NewReader(body))
	req.Header.Set("X-E2A-Internal-Signature", sig)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		buf, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d body=%s, want 204", resp.StatusCode, string(buf))
	}
}

func TestInvalidateLimits_503WhenSecretUnset(t *testing.T) {
	server, _, _, _ := setupAPIWithLimits(t, "")

	body := []byte(`{"user_id":"u_x"}`)
	req, _ := http.NewRequest("POST", server.URL+"/api/internal/limits/invalidate", bytes.NewReader(body))
	req.Header.Set("X-E2A-Internal-Signature", "anything")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no-secret config: status = %d, want 503", resp.StatusCode)
	}
}

// TestInvalidateLimits_RejectsMalformedUserID guards the growth vector the
// generation change introduced. Invalidate now WRITES a tombstone for whatever
// key it is given (it must — see the comment on limits.DBEnforcer.Invalidate),
// and the cache map has no TTL sweep, so an unvalidated user_id would let a
// caller mint permanent entries. The bound lives at the edge.
func TestInvalidateLimits_RejectsMalformedUserID(t *testing.T) {
	const secret = "test-secret-malformed"
	server, rec := setupAPIWithRecorder(t, secret)

	for _, bad := range []string{
		"u_xyz",                             // legacy-looking, wrong shape
		"0123456789ABCDEF0123456789ABCDEF",  // uppercase hex
		"0123456789abcdef0123456789abcde",   // 31 chars
		"0123456789abcdef0123456789abcdef0", // 33 chars
		"0123456789abcdef0123456789abcdeg",  // non-hex char
		"../../etc/passwd",                  // traversal-ish junk
	} {
		if code := postInvalidate(t, server, secret, bad); code != http.StatusBadRequest {
			t.Errorf("user_id %q: status = %d, want 400", bad, code)
		}
	}
	if len(rec.calls) != 0 {
		t.Errorf("malformed user_ids reached Invalidate (would mint cache entries): %v", rec.calls)
	}
}

// TestInvalidateLimits_AcceptsWellFormedUncachedUserID is the counterweight to
// the test above and pins the trap in the fix: the guard must still fire for a
// user who has NO cached entry, because that is exactly the racing case
// (miss -> DB read -> invalidate -> cachePut). A "only tombstone if an entry
// already exists" bound would pass the malformed test and silently reintroduce
// the original stale-limits bug.
func TestInvalidateLimits_AcceptsWellFormedUncachedUserID(t *testing.T) {
	const secret = "test-secret-uncached"
	server, rec := setupAPIWithRecorder(t, secret)

	if code := postInvalidate(t, server, secret, validUserID); code != http.StatusNoContent {
		t.Fatalf("well-formed uncached user_id: status = %d, want 204", code)
	}
	if len(rec.calls) != 1 || rec.calls[0] != validUserID {
		t.Fatalf("Invalidate calls = %v, want exactly [%s]", rec.calls, validUserID)
	}
}
