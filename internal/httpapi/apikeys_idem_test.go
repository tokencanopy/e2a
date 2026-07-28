package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
)

// bodyAwareIdem is an in-memory idempotency store that (unlike statefulIdem)
// honors the request body-hash: a second Claim of the same key with a DIFFERENT
// body-hash is a Mismatch (→422), matching the real Store. This lets the tests
// exercise both the retry-replay path and the reuse-with-different-body path,
// and proves the handler threads RawBody through for the body-hash. Like the
// real Store it scopes rows by (user_id, key) — two accounts may reuse the same
// key — and Complete only lands on an in-flight claim (the real Store's
// `WHERE status = 'in_progress'` guard), so a post-hoc Complete after an
// in-transaction CompleteTx is a harmless no-op instead of corrupting the
// cached row.
type bodyAwareIdem struct {
	mu     sync.Mutex
	cached map[string]struct {
		hash string
		resp idempotency.CachedResponse
	}
	inflight map[string]string // (uid|key) -> body hash
}

func newBodyAwareIdem() *bodyAwareIdem {
	return &bodyAwareIdem{
		cached: map[string]struct {
			hash string
			resp idempotency.CachedResponse
		}{},
		inflight: map[string]string{},
	}
}

func (m *bodyAwareIdem) Claim(_ context.Context, uid, key, _, bodyHash string) (idempotency.ClaimResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := uid + "|" + key
	if c, ok := m.cached[k]; ok {
		if c.hash != bodyHash {
			return idempotency.ClaimResult{Outcome: idempotency.OutcomeMismatch}, nil
		}
		return idempotency.ClaimResult{Outcome: idempotency.OutcomeReplay, Cached: c.resp}, nil
	}
	if h, ok := m.inflight[k]; ok {
		if h != bodyHash {
			return idempotency.ClaimResult{Outcome: idempotency.OutcomeMismatch}, nil
		}
		return idempotency.ClaimResult{Outcome: idempotency.OutcomeInFlight}, nil
	}
	m.inflight[k] = bodyHash
	return idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}, nil
}

func (m *bodyAwareIdem) Complete(_ context.Context, uid, key string, resp idempotency.CachedResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := uid + "|" + key
	h, ok := m.inflight[k]
	if !ok {
		return nil // already completed (e.g. via CompleteTx) — mirrors the real in_progress guard
	}
	m.cached[k] = struct {
		hash string
		resp idempotency.CachedResponse
	}{hash: h, resp: resp}
	delete(m.inflight, k)
	return nil
}

func (m *bodyAwareIdem) CompleteTx(_ context.Context, _ pgx.Tx, uid, key string, resp idempotency.CachedResponse) error {
	return m.Complete(context.Background(), uid, key, resp)
}

func (m *bodyAwareIdem) Release(_ context.Context, uid, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inflight, uid+"|"+key)
	return nil
}

// createKeyIdemServer wires a stateful in-memory idempotency store plus a
// CreateScopedAPIKey fake that counts mints and returns a distinct id/secret per
// call, so a retried request that mints a SECOND credential is observable.
func createKeyIdemServer(t *testing.T, mints *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			if r.Header.Get("Authorization") == "Bearer good" {
				return &identity.User{ID: "u_1", Email: "owner@acme.com"}, nil
			}
			return nil, errors.New("unauthorized")
		},
		Idempotency: newBodyAwareIdem(),
		CreateScopedAPIKey: func(_ context.Context, userID, name, scope, agentID string, expiresAt *time.Time) (*identity.APIKey, error) {
			*mints++
			return &identity.APIKey{
				ID: fmt.Sprintf("apk_%d", *mints), UserID: userID, Name: name,
				KeyPrefix: "e2a_acct_abcd", PlaintextKey: fmt.Sprintf("e2a_account_secret_%d", *mints),
				Scope: scope, CreatedAt: time.Unix(1700000400, 0).UTC(), ExpiresAt: expiresAt,
			}, nil
		},
	}))
	t.Cleanup(srv.Close)
	return srv
}

func createKeyCall(t *testing.T, url, idemKey, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer good")
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// A retried createApiKey carrying the SAME Idempotency-Key must replay the first
// key and NOT mint a second live credential — otherwise a network-blip retry
// silently doubles the caller's live API keys. (GA blocker: createApiKey
// idempotency, issue #493.)
func TestCreateAPIKey_IdempotentReplay(t *testing.T) {
	var mints int
	srv := createKeyIdemServer(t, &mints)
	url := srv.URL + "/v1/account/api-keys"
	body := `{"name":"ci","scope":"account"}`

	code1, b1 := createKeyCall(t, url, "mkkey-1", body)
	code2, b2 := createKeyCall(t, url, "mkkey-1", body) // network retry, byte-identical

	if code1 != 201 || code2 != 201 {
		t.Fatalf("want 201/201, got %d/%d (b1=%v b2=%v)", code1, code2, b1, b2)
	}
	if mints != 1 {
		t.Fatalf("retry minted a SECOND live key: mints=%d, want 1", mints)
	}
	if b1["id"] != b2["id"] || b1["key"] != b2["key"] {
		t.Fatalf("retry returned a different credential: id %v vs %v, key %v vs %v",
			b1["id"], b2["id"], b1["key"], b2["key"])
	}
}

// Same key + a DIFFERENT body is a caller bug, not a retry: 422 (do not replay).
func TestCreateAPIKey_KeyReuseDifferentBody422(t *testing.T) {
	var mints int
	srv := createKeyIdemServer(t, &mints)
	url := srv.URL + "/v1/account/api-keys"

	code1, _ := createKeyCall(t, url, "mkkey-2", `{"name":"a","scope":"account"}`)
	code2, _ := createKeyCall(t, url, "mkkey-2", `{"name":"DIFFERENT","scope":"account"}`)

	if code1 != 201 {
		t.Fatalf("first create: want 201, got %d", code1)
	}
	if code2 != 422 {
		t.Fatalf("same key + different body: want 422 idempotency_key_reuse, got %d", code2)
	}
}

// Distinct keys still mint separately — the guard dedupes retries, it does not
// block intentional key creation.
func TestCreateAPIKey_DistinctKeysMint(t *testing.T) {
	var mints int
	srv := createKeyIdemServer(t, &mints)
	url := srv.URL + "/v1/account/api-keys"

	createKeyCall(t, url, "mkkey-a", `{"name":"a","scope":"account"}`)
	createKeyCall(t, url, "mkkey-b", `{"name":"b","scope":"account"}`)

	if mints != 2 {
		t.Fatalf("two distinct creates should both mint: mints=%d, want 2", mints)
	}
}
