package idempotency_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func newStoreAndUser(t *testing.T) (*idempotency.Store, string, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.TestDB(t)
	identStore := identity.NewStore(pool)
	u, err := identStore.CreateOrGetUser(context.Background(), "idem-"+t.Name()+"@example.com", "Idem", "google-idem-"+t.Name())
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	return idempotency.NewStore(pool), u.ID, pool
}

func TestClaim_FreshAcquires(t *testing.T) {
	store, userID, _ := newStoreAndUser(t)
	ctx := context.Background()

	res, err := store.Claim(ctx, userID, "key-fresh", "/api/v1/send", idempotency.HashBody([]byte(`{"a":1}`)))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if res.Outcome != idempotency.OutcomeAcquired {
		t.Errorf("Outcome = %d, want OutcomeAcquired", res.Outcome)
	}
}

func TestClaim_SameKeySameBodyAfterComplete_Replays(t *testing.T) {
	store, userID, _ := newStoreAndUser(t)
	ctx := context.Background()

	body := []byte(`{"to":["a@b.com"],"subject":"x","body":"y"}`)
	hash := idempotency.HashBody(body)

	first, err := store.Claim(ctx, userID, "key-replay", "/api/v1/send", hash)
	if err != nil || first.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("first claim: outcome=%d err=%v", first.Outcome, err)
	}

	cached := idempotency.CachedResponse{
		StatusCode:  200,
		ContentType: "application/json",
		Body:        []byte(`{"status":"sent","message_id":"msg_abc"}`),
	}
	if err := store.Complete(ctx, userID, "key-replay", first.Token, cached); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	second, err := store.Claim(ctx, userID, "key-replay", "/api/v1/send", hash)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if second.Outcome != idempotency.OutcomeReplay {
		t.Fatalf("Outcome = %d, want OutcomeReplay", second.Outcome)
	}
	if second.Cached.StatusCode != cached.StatusCode {
		t.Errorf("StatusCode = %d, want %d", second.Cached.StatusCode, cached.StatusCode)
	}
	if second.Cached.ContentType != cached.ContentType {
		t.Errorf("ContentType = %q, want %q", second.Cached.ContentType, cached.ContentType)
	}
	if string(second.Cached.Body) != string(cached.Body) {
		t.Errorf("Body = %q, want %q", second.Cached.Body, cached.Body)
	}
}

func TestClaim_SameKeyDifferentBody_Mismatches(t *testing.T) {
	store, userID, _ := newStoreAndUser(t)
	ctx := context.Background()

	hash1 := idempotency.HashBody([]byte(`{"a":1}`))
	hash2 := idempotency.HashBody([]byte(`{"a":2}`))

	first, _ := store.Claim(ctx, userID, "key-mismatch", "/api/v1/send", hash1)
	if first.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}
	_ = store.Complete(ctx, userID, "key-mismatch", first.Token, idempotency.CachedResponse{StatusCode: 200, Body: []byte("ok")})

	second, err := store.Claim(ctx, userID, "key-mismatch", "/api/v1/send", hash2)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if second.Outcome != idempotency.OutcomeMismatch {
		t.Errorf("Outcome = %d, want OutcomeMismatch", second.Outcome)
	}
}

func TestClaim_InFlightDuringActiveClaim(t *testing.T) {
	store, userID, _ := newStoreAndUser(t)
	ctx := context.Background()

	hash := idempotency.HashBody([]byte(`{"a":1}`))

	first, _ := store.Claim(ctx, userID, "key-inflight", "/api/v1/send", hash)
	if first.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}

	// Same body, but first hasn't completed — second must 409.
	second, err := store.Claim(ctx, userID, "key-inflight", "/api/v1/send", hash)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if second.Outcome != idempotency.OutcomeInFlight {
		t.Errorf("Outcome = %d, want OutcomeInFlight", second.Outcome)
	}
}

func TestClaim_StaleInProgressTakeoverIsHashBoundAndFenced(t *testing.T) {
	store, userID, pool := newStoreAndUser(t)
	ctx := context.Background()
	originalHash := idempotency.HashBody([]byte(`{"original":1}`))

	// Acquire a claim, then age it past the stale window by direct UPDATE.
	first, _ := store.Claim(ctx, userID, "key-stale", "/api/v1/send", originalHash)
	if first.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}

	// Age the in_progress row past StaleClaimWindow.
	if _, err := pool.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - $2::interval WHERE user_id = $1 AND key = 'key-stale'`,
		userID, (idempotency.StaleClaimWindow + time.Minute).String()); err != nil {
		t.Fatalf("age row: %v", err)
	}

	// A different logical request never takes over, even after the old owner is
	// stale. Otherwise two destructive requests could execute under one key.
	mismatch, err := store.Claim(ctx, userID, "key-stale", "/api/v1/send", idempotency.HashBody([]byte(`{"replacement":1}`)))
	if err != nil {
		t.Fatalf("mismatched Claim: %v", err)
	}
	if mismatch.Outcome != idempotency.OutcomeMismatch {
		t.Fatalf("mismatched Outcome = %d, want OutcomeMismatch", mismatch.Outcome)
	}

	// The byte-identical logical request may take over and gets a fresh fencing
	// token. Delayed work from the first owner can neither Complete nor Release
	// the replacement generation.
	second, err := store.Claim(ctx, userID, "key-stale", "/api/v1/send", originalHash)
	if err != nil || second.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("same-request stale takeover = (%d, %v), want acquired", second.Outcome, err)
	}
	response := idempotency.CachedResponse{StatusCode: 200, Body: []byte("new-owner")}
	if err := store.Complete(ctx, userID, "key-stale", first.Token, response); !errors.Is(err, idempotency.ErrClaimLost) {
		t.Fatalf("stale owner Complete = %v, want ErrClaimLost", err)
	}
	if err := store.Release(ctx, userID, "key-stale", first.Token); !errors.Is(err, idempotency.ErrClaimLost) {
		t.Fatalf("stale owner Release = %v, want ErrClaimLost", err)
	}
	if err := store.Complete(ctx, userID, "key-stale", second.Token, response); err != nil {
		t.Fatalf("current owner Complete: %v", err)
	}
	replay, err := store.Claim(ctx, userID, "key-stale", "/api/v1/send", originalHash)
	if err != nil || replay.Outcome != idempotency.OutcomeReplay || string(replay.Cached.Body) != "new-owner" {
		t.Fatalf("replay after fenced takeover = (%+v, %v)", replay, err)
	}
}

func TestClaim_DifferentUsersIsolated(t *testing.T) {
	pool := testutil.TestDB(t)
	identStore := identity.NewStore(pool)
	idemStore := idempotency.NewStore(pool)
	ctx := context.Background()

	uA, _ := identStore.CreateOrGetUser(ctx, "user-a@example.com", "A", "google-a")
	uB, _ := identStore.CreateOrGetUser(ctx, "user-b@example.com", "B", "google-b")

	hash := idempotency.HashBody([]byte(`{"x":1}`))

	a, _ := idemStore.Claim(ctx, uA.ID, "shared", "/api/v1/send", hash)
	if a.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("A Outcome = %d", a.Outcome)
	}
	// Same key, different user — must not collide.
	b, _ := idemStore.Claim(ctx, uB.ID, "shared", "/api/v1/send", hash)
	if b.Outcome != idempotency.OutcomeAcquired {
		t.Errorf("B Outcome = %d, want OutcomeAcquired (different user)", b.Outcome)
	}
}

func TestRelease_AllowsFreshClaim(t *testing.T) {
	store, userID, _ := newStoreAndUser(t)
	ctx := context.Background()

	first, _ := store.Claim(ctx, userID, "key-release", "/api/v1/send", idempotency.HashBody([]byte(`{"a":1}`)))
	if first.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}

	if err := store.Release(ctx, userID, "key-release", first.Token); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// After release, a fresh claim with any body should re-acquire.
	second, err := store.Claim(ctx, userID, "key-release", "/api/v1/send", idempotency.HashBody([]byte(`{"a":99}`)))
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if second.Outcome != idempotency.OutcomeAcquired {
		t.Errorf("Outcome = %d, want OutcomeAcquired after Release", second.Outcome)
	}
}

func TestRelease_DoesNotDeleteCompletedRow(t *testing.T) {
	store, userID, _ := newStoreAndUser(t)
	ctx := context.Background()

	hash := idempotency.HashBody([]byte(`{"a":1}`))
	first, _ := store.Claim(ctx, userID, "key-rel-completed", "/api/v1/send", hash)
	if first.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}
	_ = store.Complete(ctx, userID, "key-rel-completed", first.Token, idempotency.CachedResponse{StatusCode: 200, Body: []byte("ok")})

	// A buggy caller invokes Release post-Complete; it loses the fence and must
	// leave the durable response untouched.
	if err := store.Release(ctx, userID, "key-rel-completed", first.Token); !errors.Is(err, idempotency.ErrClaimLost) {
		t.Fatalf("Release after completion = %v, want ErrClaimLost", err)
	}

	// Replay must still work.
	second, _ := store.Claim(ctx, userID, "key-rel-completed", "/api/v1/send", hash)
	if second.Outcome != idempotency.OutcomeReplay {
		t.Errorf("Outcome = %d, want OutcomeReplay (Release should not have wiped completed row)", second.Outcome)
	}
}

func TestComplete_DoubleCallIsFenced(t *testing.T) {
	store, userID, _ := newStoreAndUser(t)
	ctx := context.Background()

	hash := idempotency.HashBody([]byte(`{"a":1}`))
	first, _ := store.Claim(ctx, userID, "key-dbl", "/api/v1/send", hash)
	if first.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}
	originalCached := idempotency.CachedResponse{StatusCode: 200, ContentType: "application/json", Body: []byte(`{"status":"sent"}`)}
	_ = store.Complete(ctx, userID, "key-dbl", first.Token, originalCached)

	// A buggy caller re-Completes with a different body. Must not
	// overwrite the cached response.
	if err := store.Complete(ctx, userID, "key-dbl", first.Token, idempotency.CachedResponse{StatusCode: 500, Body: []byte("oops")}); !errors.Is(err, idempotency.ErrClaimLost) {
		t.Fatalf("second Complete = %v, want ErrClaimLost", err)
	}

	replay, _ := store.Claim(ctx, userID, "key-dbl", "/api/v1/send", hash)
	if replay.Outcome != idempotency.OutcomeReplay {
		t.Fatalf("Outcome = %d", replay.Outcome)
	}
	if string(replay.Cached.Body) != string(originalCached.Body) {
		t.Errorf("cached body = %q, want %q (Complete should be no-op after first call)", replay.Cached.Body, originalCached.Body)
	}
}

func TestSweep_DeletesCompletedRowsPastTTL(t *testing.T) {
	store, userID, pool := newStoreAndUser(t)
	ctx := context.Background()

	hash := idempotency.HashBody([]byte(`{"a":1}`))
	first, _ := store.Claim(ctx, userID, "key-sweep", "/api/v1/send", hash)
	if first.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}
	_ = store.Complete(ctx, userID, "key-sweep", first.Token, idempotency.CachedResponse{StatusCode: 200, Body: []byte("ok")})

	// Age the row past TTL.
	if _, err := pool.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - $2::interval WHERE user_id = $1 AND key = 'key-sweep'`,
		userID, (idempotency.TTL + time.Hour).String()); err != nil {
		t.Fatalf("age row: %v", err)
	}

	deleted, err := store.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted < 1 {
		t.Errorf("deleted = %d, want >= 1", deleted)
	}

	// After sweep, the key is reusable.
	third, _ := store.Claim(ctx, userID, "key-sweep", "/api/v1/send", hash)
	if third.Outcome != idempotency.OutcomeAcquired {
		t.Errorf("Outcome = %d, want OutcomeAcquired (sweep should have removed completed row)", third.Outcome)
	}
}

func TestSweep_DoesNotDeleteRecentInProgress(t *testing.T) {
	store, userID, _ := newStoreAndUser(t)
	ctx := context.Background()

	hash := idempotency.HashBody([]byte(`{"a":1}`))
	first, _ := store.Claim(ctx, userID, "key-sweep-active", "/api/v1/send", hash)
	if first.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("first Outcome = %d", first.Outcome)
	}

	if _, err := store.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Active in_progress claim must survive sweep.
	second, _ := store.Claim(ctx, userID, "key-sweep-active", "/api/v1/send", hash)
	if second.Outcome != idempotency.OutcomeInFlight {
		t.Errorf("Outcome = %d, want OutcomeInFlight (sweep should have kept active claim)", second.Outcome)
	}
}

func TestClaim_ConcurrentSameKeyOnlyOneAcquires(t *testing.T) {
	store, userID, _ := newStoreAndUser(t)
	ctx := context.Background()

	hash := idempotency.HashBody([]byte(`{"a":1}`))
	const N = 25

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		acquired int
		inflight int
		other    int
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			res, err := store.Claim(ctx, userID, "key-concurrent", "/api/v1/send", hash)
			if err != nil {
				t.Errorf("Claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch res.Outcome {
			case idempotency.OutcomeAcquired:
				acquired++
			case idempotency.OutcomeInFlight:
				inflight++
			default:
				other++
			}
		}()
	}
	wg.Wait()

	if acquired != 1 {
		t.Errorf("acquired = %d, want exactly 1 across %d concurrent Claims", acquired, N)
	}
	if other != 0 {
		t.Errorf("unexpected outcomes = %d (expected only Acquired+InFlight)", other)
	}
	if acquired+inflight != N {
		t.Errorf("acquired(%d) + inflight(%d) != N(%d)", acquired, inflight, N)
	}
}

func TestClaim_EmptyArgsRejected(t *testing.T) {
	store, userID, _ := newStoreAndUser(t)
	ctx := context.Background()

	if _, err := store.Claim(ctx, "", "key", "/path", "hash"); err == nil {
		t.Error("Claim with empty userID returned nil error")
	}
	if _, err := store.Claim(ctx, userID, "", "/path", "hash"); err == nil {
		t.Error("Claim with empty key returned nil error")
	}
}
