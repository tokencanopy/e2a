package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/idempotency"
)

// fakeIdem is a programmable in-memory IdemStore for unit tests.
type fakeIdem struct {
	claim     idempotency.ClaimResult
	claimErr  error
	completed *idempotency.CachedResponse
	released  bool
}

func (f *fakeIdem) Claim(ctx context.Context, userID, key, path, bodyHash string) (idempotency.ClaimResult, error) {
	return f.claim, f.claimErr
}
func (f *fakeIdem) Complete(ctx context.Context, userID, key string, _ idempotency.ClaimToken, resp idempotency.CachedResponse) error {
	f.completed = &resp
	return nil
}
func (f *fakeIdem) CompleteTx(ctx context.Context, tx pgx.Tx, userID, key string, _ idempotency.ClaimToken, resp idempotency.CachedResponse) error {
	f.completed = &resp
	return nil
}
func (f *fakeIdem) Release(ctx context.Context, userID, key string, _ idempotency.ClaimToken) error {
	f.released = true
	return nil
}

type sendBody struct {
	Status    string `json:"status"`
	MessageID string `json:"message_id"`
}

func serverWithIdem(f IdemStore) *Server {
	return New(Deps{Idempotency: f})
}

func TestIdempotentNoKeyRunsFn(t *testing.T) {
	s := serverWithIdem(&fakeIdem{})
	called := false
	status, body, err := runIdempotent(s, context.Background(), "u", "", "/v1/x", nil, func(_ idemClaimToken) (int, sendBody, error) {
		called = true
		return 201, sendBody{Status: "sent", MessageID: "m1"}, nil
	})
	if err != nil || !called || status != 201 || body.MessageID != "m1" {
		t.Fatalf("no-key should run fn directly: status=%d body=%+v called=%v err=%v", status, body, called, err)
	}
}

func TestIdempotentExistingEndpointsDoNotGainGlobalKeyLimit(t *testing.T) {
	// Existing GA endpoints historically accepted longer keys. Domain DELETE
	// enforces its new 255-byte contract locally rather than silently tightening
	// every endpoint that shares this helper.
	f := &fakeIdem{claim: idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}}
	_, _, err := runIdempotent(serverWithIdem(f), context.Background(), "u", strings.Repeat("k", idempotency.MaxKeyLength+1), "/v1/x", nil,
		func(_ idemClaimToken) (int, sendBody, error) { return 200, sendBody{Status: "ok"}, nil })
	if err != nil {
		t.Fatalf("shared helper rejected existing long key: %v", err)
	}
}

func TestIdempotentAcquiredCaches(t *testing.T) {
	f := &fakeIdem{claim: idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}}
	s := serverWithIdem(f)
	status, body, err := runIdempotent(s, context.Background(), "u", "k1", "/v1/x", []byte(`{"a":1}`), func(_ idemClaimToken) (int, sendBody, error) {
		return 201, sendBody{Status: "sent", MessageID: "m1"}, nil
	})
	if err != nil || status != 201 || body.MessageID != "m1" {
		t.Fatalf("acquired path: status=%d body=%+v err=%v", status, body, err)
	}
	if f.completed == nil || f.completed.StatusCode != 201 {
		t.Fatal("acquired success must Complete (cache) the response")
	}
	var cached sendBody
	_ = json.Unmarshal(f.completed.Body, &cached)
	if cached.MessageID != "m1" {
		t.Fatalf("cached body wrong: %s", f.completed.Body)
	}
	if f.released {
		t.Fatal("success must not Release")
	}
}

func TestIdempotentReplayReturnsCached(t *testing.T) {
	cachedJSON, _ := json.Marshal(sendBody{Status: "sent", MessageID: "cached_m"})
	f := &fakeIdem{claim: idempotency.ClaimResult{
		Outcome: idempotency.OutcomeReplay,
		Cached:  idempotency.CachedResponse{StatusCode: 201, ContentType: "application/json", Body: cachedJSON},
	}}
	s := serverWithIdem(f)
	called := false
	status, body, err := runIdempotent(s, context.Background(), "u", "k1", "/v1/x", []byte(`{"a":1}`), func(_ idemClaimToken) (int, sendBody, error) {
		called = true
		return 500, sendBody{}, errors.New("should not run")
	})
	if err != nil || called {
		t.Fatalf("replay must NOT run fn: called=%v err=%v", called, err)
	}
	if status != 201 || body.MessageID != "cached_m" {
		t.Fatalf("replay should return cached: status=%d body=%+v", status, body)
	}
}

func TestIdempotentMismatch422(t *testing.T) {
	f := &fakeIdem{claim: idempotency.ClaimResult{Outcome: idempotency.OutcomeMismatch}}
	s := serverWithIdem(f)
	_, _, err := runIdempotent(s, context.Background(), "u", "k1", "/v1/x", []byte(`{"a":2}`), func(_ idemClaimToken) (int, sendBody, error) {
		return 201, sendBody{}, nil
	})
	env, ok := err.(*ErrorEnvelope)
	if !ok || env.GetStatus() != 422 || env.Code() != "idempotency_key_reuse" {
		t.Fatalf("mismatch should be 422 idempotency_key_reuse, got %v", err)
	}
}

func TestIdempotentInFlight409(t *testing.T) {
	f := &fakeIdem{claim: idempotency.ClaimResult{Outcome: idempotency.OutcomeInFlight}}
	s := serverWithIdem(f)
	_, _, err := runIdempotent(s, context.Background(), "u", "k1", "/v1/x", nil, func(_ idemClaimToken) (int, sendBody, error) {
		return 201, sendBody{}, nil
	})
	env, ok := err.(*ErrorEnvelope)
	if !ok || env.GetStatus() != 409 {
		t.Fatalf("in-flight should be 409, got %v", err)
	}
}

// unmarshalable has a channel field, so json.Marshal always fails — used to
// prove the post-side-effect marshal-failure path still Completes (locks) the
// key rather than orphaning it (which would risk a double-send on retry).
type unmarshalable struct {
	Ch chan int `json:"ch"`
}

func TestIdempotentMarshalFailureStillCompletes(t *testing.T) {
	f := &fakeIdem{claim: idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}}
	s := serverWithIdem(f)
	status, _, err := runIdempotent(s, context.Background(), "u", "k1", "/v1/x", nil, func(_ idemClaimToken) (int, unmarshalable, error) {
		return 200, unmarshalable{Ch: make(chan int)}, nil // side effect "committed"
	})
	if err != nil || status != 200 {
		t.Fatalf("expected success despite marshal failure: status=%d err=%v", status, err)
	}
	if f.completed == nil {
		t.Fatal("BLOCKER regression: marshal failure must still Complete (lock) the key, never orphan it")
	}
	if f.released {
		t.Fatal("must NOT Release after the side effect committed (would allow a double-send on retry)")
	}
	if string(f.completed.Body) != "{}" {
		t.Fatalf("expected fallback {} body, got %s", f.completed.Body)
	}
}

func TestIdempotentFnErrorReleases(t *testing.T) {
	f := &fakeIdem{claim: idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}}
	s := serverWithIdem(f)
	sentinel := NewError(400, "bad", "nope")
	_, _, err := runIdempotent(s, context.Background(), "u", "k1", "/v1/x", nil, func(_ idemClaimToken) (int, sendBody, error) {
		return 0, sendBody{}, sentinel
	})
	if err != sentinel {
		t.Fatalf("fn error should propagate, got %v", err)
	}
	if !f.released {
		t.Fatal("pre-side-effect failure must Release the key")
	}
	if f.completed != nil {
		t.Fatal("failure must not Complete (cache)")
	}
}

// keyRecordingIdem captures the key string passed to Claim so tests can assert
// the namespace the helper applied.
type keyRecordingIdem struct {
	fakeIdem
	claimedKey string
}

func (f *keyRecordingIdem) Claim(ctx context.Context, userID, key, path, bodyHash string) (idempotency.ClaimResult, error) {
	f.claimedKey = key
	return f.fakeIdem.Claim(ctx, userID, key, path, bodyHash)
}

// TestIdempotentNamespaceSeparation locks in the fix for the (user_id, key)
// collision: a caller-supplied Idempotency-Key header and a server-minted
// automatic key with the SAME raw string must land in disjoint namespaces, so
// a crafted header (e.g. "replay:evt_x:") cannot poison a later server-minted
// redelivery key of the same string.
func TestIdempotentNamespaceSeparation(t *testing.T) {
	raw := "replay:evt_x:"

	fUser := &keyRecordingIdem{fakeIdem: fakeIdem{claim: idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}}}
	sUser := serverWithIdem(fUser)
	_, _, _ = runIdempotent(sUser, context.Background(), "u", raw, "/v1/send", nil, func(_ idemClaimToken) (int, sendBody, error) {
		return 200, sendBody{}, nil
	})

	fAuto := &keyRecordingIdem{fakeIdem: fakeIdem{claim: idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}}}
	sAuto := serverWithIdem(fAuto)
	_, _, _ = runIdempotentAuto(sAuto, context.Background(), "u", raw, "/v1/events/redeliver", nil, func(_ idemClaimToken) (int, sendBody, error) {
		return 200, sendBody{}, nil
	})

	if fUser.claimedKey == fAuto.claimedKey {
		t.Fatalf("header and server-minted keys must not collide; both claimed %q", fUser.claimedKey)
	}
	if fUser.claimedKey != idemUserNS+raw {
		t.Fatalf("header key namespace: got %q want %q", fUser.claimedKey, idemUserNS+raw)
	}
	if fAuto.claimedKey != idemAutoNS+raw {
		t.Fatalf("auto key namespace: got %q want %q", fAuto.claimedKey, idemAutoNS+raw)
	}
}

// TestIdempotentPanicReleases proves a panic in fn releases the key (so retries
// aren't 409-locked for the stale window) and re-raises.
func TestIdempotentPanicReleases(t *testing.T) {
	f := &fakeIdem{claim: idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}}
	s := serverWithIdem(f)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("panic must propagate after release")
		}
		if !f.released {
			t.Fatal("panic between claim and completion must Release the key")
		}
		if f.completed != nil {
			t.Fatal("panic must not Complete (cache) the key")
		}
	}()
	_, _, _ = runIdempotent(s, context.Background(), "u", "k1", "/v1/x", nil, func(_ idemClaimToken) (int, sendBody, error) {
		panic("boom")
	})
}
