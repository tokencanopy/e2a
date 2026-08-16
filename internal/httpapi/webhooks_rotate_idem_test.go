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

// statefulIdem is an in-memory idempotency store: first Claim of a key wins
// (Acquired), Complete caches the response, and a later Claim of the same key
// replays it. Enough to exercise the retry-replay path end to end.
type statefulIdem struct {
	mu       sync.Mutex
	cached   map[string]idempotency.CachedResponse
	inflight map[string]bool
}

func newStatefulIdem() *statefulIdem {
	return &statefulIdem{cached: map[string]idempotency.CachedResponse{}, inflight: map[string]bool{}}
}

func (m *statefulIdem) Claim(_ context.Context, _, key, _, _ string) (idempotency.ClaimResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cached[key]; ok {
		return idempotency.ClaimResult{Outcome: idempotency.OutcomeReplay, Cached: c}, nil
	}
	if m.inflight[key] {
		return idempotency.ClaimResult{Outcome: idempotency.OutcomeInFlight}, nil
	}
	m.inflight[key] = true
	return idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}, nil
}

func (m *statefulIdem) Complete(_ context.Context, _, key string, _ idempotency.ClaimToken, resp idempotency.CachedResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached[key] = resp
	delete(m.inflight, key)
	return nil
}

func (m *statefulIdem) CompleteTx(_ context.Context, _ pgx.Tx, uid, key string, token idempotency.ClaimToken, resp idempotency.CachedResponse) error {
	return m.Complete(context.Background(), uid, key, token, resp)
}

func (m *statefulIdem) Release(_ context.Context, _, key string, _ idempotency.ClaimToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inflight, key)
	return nil
}

func rotateTestServer(t *testing.T, rotations *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			if r.Header.Get("Authorization") == "Bearer good" {
				return &identity.User{ID: "u_1", Email: "owner@acme.com"}, nil
			}
			return nil, errors.New("unauthorized")
		},
		Idempotency: newStatefulIdem(),
		RotateSecret: func(_ context.Context, _, _ string) (string, time.Time, error) {
			*rotations++
			return fmt.Sprintf("whsec_rot_%d", *rotations), time.Unix(1700086400, 0).UTC(), nil
		},
	}))
	t.Cleanup(srv.Close)
	return srv
}

func rotateCall(t *testing.T, url, key string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer good")
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// A retried rotate carrying the SAME Idempotency-Key must replay the first
// secret and NOT mint a second one — otherwise the retry silently invalidates
// the secret the caller already stored. (GA blocker #8.)
func TestRotateWebhookSecret_IdempotentReplay(t *testing.T) {
	var rotations int
	srv := rotateTestServer(t, &rotations)
	url := srv.URL + "/v1/webhooks/wh_1/rotate-secret"

	code1, body1 := rotateCall(t, url, "rot-key-1")
	code2, body2 := rotateCall(t, url, "rot-key-1") // network retry, same key

	if code1 != 200 || code2 != 200 {
		t.Fatalf("want 200/200, got %d/%d", code1, code2)
	}
	if rotations != 1 {
		t.Fatalf("retry minted a SECOND secret: rotations=%d, want 1", rotations)
	}
	if body1["signing_secret"] != body2["signing_secret"] {
		t.Fatalf("retry returned a different secret: %v vs %v", body1["signing_secret"], body2["signing_secret"])
	}
}

// Sanity: distinct keys (genuinely separate rotations) still rotate each time —
// the idempotency guard dedupes retries, it does not block intentional rotation.
func TestRotateWebhookSecret_DistinctKeysRotate(t *testing.T) {
	var rotations int
	srv := rotateTestServer(t, &rotations)
	url := srv.URL + "/v1/webhooks/wh_1/rotate-secret"

	rotateCall(t, url, "rot-key-a")
	rotateCall(t, url, "rot-key-b")

	if rotations != 2 {
		t.Fatalf("two distinct rotations should both run: rotations=%d, want 2", rotations)
	}
}
