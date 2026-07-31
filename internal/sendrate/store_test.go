package sendrate_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/sendrate"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// seedAgent creates a user+domain+agent and returns the agent id — the
// limiter's key (agent_identities.id, the same key the acceptance-time
// limiter uses).
func seedAgent(t *testing.T, pool *pgxpool.Pool, suffix string) string {
	t.Helper()
	ctx := context.Background()
	ids := identity.NewStore(pool)
	user, err := ids.CreateOrGetUser(ctx, "rate-"+suffix+"@example.com", "Rate", "rate-"+suffix)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain := suffix + ".example.com"
	if _, err := ids.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	ag, err := ids.CreateAgent(ctx, "agent@"+domain, domain, "", "", "local", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return ag.ID
}

func reserve(t *testing.T, store *sendrate.Store, agentID string) sendrate.Decision {
	t.Helper()
	d, err := store.Reserve(context.Background(), agentID)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	return d
}

// TestReserveAllowsSixtyThenDefersSixtyFirst pins the production shape: a
// full window of 60 reservations is allowed, the 61st is deferred, and the
// deferral's RetryAt is exactly the oldest kept event + the window.
func TestReserveAllowsSixtyThenDefersSixtyFirst(t *testing.T) {
	pool := testutil.TestDB(t)
	agentID := seedAgent(t, pool, "sixty")
	store := sendrate.NewStore(pool, time.Minute, 60)

	for i := 0; i < 60; i++ {
		if d := reserve(t, store, agentID); !d.Allowed {
			t.Fatalf("reservation %d deferred, want allowed", i+1)
		}
	}
	d := reserve(t, store, agentID)
	if d.Allowed {
		t.Fatal("61st reservation in the window must be deferred")
	}

	var oldest time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT events[1] FROM agent_send_rate_windows WHERE agent_id=$1`, agentID).Scan(&oldest); err != nil {
		t.Fatalf("read oldest event: %v", err)
	}
	if want := oldest.Add(time.Minute); !d.RetryAt.Equal(want) {
		t.Errorf("RetryAt = %s, want oldest event + window = %s", d.RetryAt, want)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(array_length(events,1),0) FROM agent_send_rate_windows WHERE agent_id=$1`, agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 60 {
		t.Errorf("stored events = %d, want exactly 60 (the denied reservation consumes no slot)", n)
	}
}

// TestReserveAllowsAgainAfterWindowPasses proves the sliding window frees
// capacity on its own: once the oldest event ages out, the next reservation
// is allowed without any external cleanup.
func TestReserveAllowsAgainAfterWindowPasses(t *testing.T) {
	pool := testutil.TestDB(t)
	agentID := seedAgent(t, pool, "window")
	window := 400 * time.Millisecond
	store := sendrate.NewStore(pool, window, 2)

	if d := reserve(t, store, agentID); !d.Allowed {
		t.Fatal("first reservation must be allowed")
	}
	if d := reserve(t, store, agentID); !d.Allowed {
		t.Fatal("second reservation must be allowed")
	}
	if d := reserve(t, store, agentID); d.Allowed {
		t.Fatal("third reservation inside the window must be deferred")
	}

	time.Sleep(window + 150*time.Millisecond)
	if d := reserve(t, store, agentID); !d.Allowed {
		t.Fatal("reservation after the window passed must be allowed")
	}
}

// TestReserveAgentsIndependent: each agent has its own window — one agent at
// its limit never throttles another.
func TestReserveAgentsIndependent(t *testing.T) {
	pool := testutil.TestDB(t)
	a := seedAgent(t, pool, "agenta")
	b := seedAgent(t, pool, "agentb")
	store := sendrate.NewStore(pool, time.Minute, 2)

	reserve(t, store, a)
	reserve(t, store, a)
	if d := reserve(t, store, a); d.Allowed {
		t.Fatal("agent A over its limit must be deferred")
	}
	for i := 0; i < 2; i++ {
		if d := reserve(t, store, b); !d.Allowed {
			t.Fatalf("agent B reservation %d deferred by agent A's traffic", i+1)
		}
	}
	if d := reserve(t, store, b); d.Allowed {
		t.Fatal("agent B over its own limit must be deferred")
	}
}

// TestReserveConcurrentNeverExceedsLimit hammers one agent's row from many
// goroutines and proves the row-lock serialization: exactly `limit`
// reservations are allowed in total within the window, never more.
func TestReserveConcurrentNeverExceedsLimit(t *testing.T) {
	pool := testutil.TestDB(t)
	agentID := seedAgent(t, pool, "concurrent")
	const limit = 25
	store := sendrate.NewStore(pool, time.Minute, limit)

	const goroutines = 10
	const perGoroutine = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed, deferred := 0, 0
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				d, err := store.Reserve(context.Background(), agentID)
				if err != nil {
					t.Errorf("Reserve: %v", err)
					return
				}
				mu.Lock()
				if d.Allowed {
					allowed++
				} else {
					deferred++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != limit {
		t.Errorf("allowed = %d, want exactly %d under concurrency", allowed, limit)
	}
	if deferred != goroutines*perGoroutine-limit {
		t.Errorf("deferred = %d, want %d", deferred, goroutines*perGoroutine-limit)
	}
}

// TestReservePrunesExpiredEntries keeps the row bounded: entries older than
// the window are dropped on the next Reserve, so no janitor is needed.
func TestReservePrunesExpiredEntries(t *testing.T) {
	pool := testutil.TestDB(t)
	agentID := seedAgent(t, pool, "prune")
	window := 300 * time.Millisecond
	store := sendrate.NewStore(pool, window, 5)

	reserve(t, store, agentID)
	reserve(t, store, agentID)
	time.Sleep(window + 150*time.Millisecond)
	if d := reserve(t, store, agentID); !d.Allowed {
		t.Fatal("reservation after the window must be allowed")
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(array_length(events,1),0) FROM agent_send_rate_windows WHERE agent_id=$1`, agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("stored events = %d, want 1 (expired entries pruned on Reserve)", n)
	}
}

func TestReserveRejectsEmptyAgentID(t *testing.T) {
	pool := testutil.TestDB(t)
	store := sendrate.NewStore(pool, time.Minute, 60)
	if _, err := store.Reserve(context.Background(), ""); err == nil {
		t.Fatal("empty agent id must error")
	}
}

// TestReserveDeniesAllWhenLimitNotPositive: a non-positive limit is
// constructor misuse; the store fails closed (deny everything, retry a full
// window out) rather than allowing exactly one reservation through the
// empty-kept edge — and must not panic indexing an empty window.
func TestReserveDeniesAllWhenLimitNotPositive(t *testing.T) {
	pool := testutil.TestDB(t)
	agentID := seedAgent(t, pool, "zerolimit")
	store := sendrate.NewStore(pool, time.Minute, 0)
	before := time.Now()
	d := reserve(t, store, agentID)
	if d.Allowed {
		t.Fatal("limit=0 must deny every reservation")
	}
	if d.RetryAt.Before(before.Add(50 * time.Second)) {
		t.Errorf("RetryAt = %s, want ~now+window", d.RetryAt)
	}
}

// TestReserveClosedPoolErrors: a Reserve against an unusable pool surfaces
// the BeginTx error instead of panicking or reporting a phantom decision —
// the worker's fail-closed path depends on getting this error. Uses a
// DEDICATED pool: the shared testutil pool must stay open for other tests.
func TestReserveClosedPoolErrors(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), testutil.TestDBURL())
	if err != nil {
		t.Fatalf("dial test db: %v", err)
	}
	store := sendrate.NewStore(pool, time.Minute, 60)
	pool.Close()
	if _, err := store.Reserve(context.Background(), "agt_anything"); err == nil {
		t.Fatal("Reserve against a closed pool must error")
	}
}
