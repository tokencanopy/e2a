package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/migrations"
)

// migratedTestDB returns a test pool prepared through the production migration
// runner. The explicit second call verifies the startup path is idempotent before
// /readyz and /selftest query the schema_migrations tracker.
func migratedTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testutil.TestDB(t)
	if err := identity.RunMigrations(context.Background(), pool, migrations.FS, identity.ModeAuto); err != nil {
		t.Fatalf("record migrations: %v", err)
	}
	return pool
}

func TestLatestMigration(t *testing.T) {
	got := latestMigration()
	if got == "" {
		t.Fatal("latestMigration() = empty, want a migration filename")
	}
	if !strings.HasSuffix(got, ".sql") {
		t.Errorf("latestMigration() = %q, want a .sql filename", got)
	}
	// 037 is the floor at the time of writing; later migrations sort after it.
	if got < "037_account_class.sql" {
		t.Errorf("latestMigration() = %q, want >= 037_account_class.sql", got)
	}
}

func TestReadyzHandler_Ready(t *testing.T) {
	pool := migratedTestDB(t)
	rec := httptest.NewRecorder()
	newReadinessMonitor(pool, nil).handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct{ Status string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "ready" {
		t.Errorf("status = %q, want ready", out.Status)
	}
}

func TestReadyzHandler_DBUnreachable(t *testing.T) {
	// A closed pool makes Ping fail → /readyz reports 503 not_ready. Use a
	// dedicated pool (not the shared test pool) so closing it is safe.
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testutil.TestDBURL())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	pool.Close()

	rec := httptest.NewRecorder()
	newReadinessMonitor(pool, nil).handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var out struct{ Status, Reason string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Status != "not_ready" {
		t.Errorf("status = %q, want not_ready (reason=%q)", out.Status, out.Reason)
	}
}

func TestReadyzHandler_Draining(t *testing.T) {
	// Once shutdown begins, /readyz must flip to 503 "draining" BEFORE any
	// dependency check — a draining instance must leave the LB rotation even
	// while its DB is perfectly healthy. Liveness (/api/health) stays green
	// throughout so orchestrators don't kill the instance mid-drain.
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testutil.TestDBURL())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	pool.Close() // deliberately unusable: drain must short-circuit ahead of Ping

	var drain atomic.Bool
	drain.Store(true)
	rec := httptest.NewRecorder()
	newReadinessMonitor(pool, &drain).handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var out struct{ Status, Reason string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Status != "not_ready" || out.Reason != "draining" {
		t.Errorf("got status=%q reason=%q, want not_ready/draining", out.Status, out.Reason)
	}
}

// TestReadinessToleratesTransientFailureWithinGrace is the regression guard for
// the outage this monitor exists to prevent: a saturated (not absent) database
// must not evict the instance from the load balancer.
//
// Previously /readyz called pool.Ping inline, so pool exhaustion timed out the
// check and reported "database unreachable". HAProxy marked the only backend
// DOWN and answered every request — including ones that never touch the DB —
// with its own 503. Here a monitor that has been healthy keeps reporting ready
// while probes fail inside the grace window.
func TestReadinessToleratesTransientFailureWithinGrace(t *testing.T) {
	healthy := migratedTestDB(t)

	// Start healthy so lastOK is set, then swap in a dead pool to simulate the
	// probe failing without the database having actually gone away.
	m := newReadinessMonitorWithConfig(healthy, nil, time.Hour, time.Hour, 2*time.Second)
	defer m.Stop()

	rec := httptest.NewRecorder()
	m.handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("precondition: status = %d, want 200", rec.Code)
	}

	dead, err := pgxpool.New(context.Background(), testutil.TestDBURL())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	dead.Close()
	m.pool = dead
	m.evaluate() // probe fails, but we are well inside the grace window

	rec = httptest.NewRecorder()
	m.handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a transient probe failure inside the grace window evicted the instance; body=%s",
			rec.Code, rec.Body.String())
	}
}

// A failure that outlives the grace window must still flip readiness — the
// tolerance above must not become "never report a dead database".
func TestReadinessFlipsAfterGraceExpires(t *testing.T) {
	healthy := migratedTestDB(t)
	m := newReadinessMonitorWithConfig(healthy, nil, time.Hour, 0, 2*time.Second)
	defer m.Stop()

	dead, err := pgxpool.New(context.Background(), testutil.TestDBURL())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	dead.Close()
	m.pool = dead
	time.Sleep(2 * time.Millisecond) // ensure now-lastOK exceeds the zero grace
	m.evaluate()

	rec := httptest.NewRecorder()
	m.handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once the grace window has expired", rec.Code)
	}
}

// The handler must never block on the pool: that head-of-line blocking is what
// let a busy database stall the health check in the first place.
func TestReadinessHandlerDoesNotTouchPool(t *testing.T) {
	dead, err := pgxpool.New(context.Background(), testutil.TestDBURL())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	dead.Close()

	m := newReadinessMonitorWithConfig(dead, nil, time.Hour, time.Hour, 2*time.Second)
	defer m.Stop()

	start := time.Now()
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		m.handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("50 readiness responses took %s: the handler is waiting on the pool", elapsed)
	}
}
