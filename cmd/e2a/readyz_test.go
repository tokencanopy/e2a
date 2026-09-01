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

func TestLatestMigrationAppliedRecognizesLegacyAlias(t *testing.T) {
	ctx := context.Background()
	pool := migratedTestDB(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tracker fixture transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Migration 118 was renamed after it had already shipped under the 115
	// prefix. Exercise that compatibility mapping directly: it must remain valid
	// even after later migrations become the repository's latest migration.
	renamedMigration := "118_sending_protection_validate_constraints.sql"
	if _, err := tx.Exec(ctx,
		"DELETE FROM schema_migrations WHERE filename = $1",
		renamedMigration,
	); err != nil {
		t.Fatalf("remove current tracker marker: %v", err)
	}

	applied, err := latestMigrationApplied(ctx, tx, renamedMigration)
	if err != nil {
		t.Fatalf("check legacy-only latest migration: %v", err)
	}
	if !applied {
		t.Fatal("legacy-only tracker row must satisfy readiness after a filename-compatible upgrade")
	}

	if _, err := tx.Exec(ctx,
		"DELETE FROM schema_migrations WHERE filename = $1",
		"115_sending_protection_validate_constraints.sql",
	); err != nil {
		t.Fatalf("remove legacy tracker marker: %v", err)
	}
	applied, err = latestMigrationApplied(ctx, tx, renamedMigration)
	if err != nil {
		t.Fatalf("check missing latest migration: %v", err)
	}
	if applied {
		t.Fatal("readiness accepted latest migration with neither current nor legacy tracker row")
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
// with its own 503.
func TestReadinessToleratesTransientFailureWithinGrace(t *testing.T) {
	m := newReadinessMonitorWithConfig(migratedTestDB(t), nil, time.Hour, time.Hour, 2*time.Second)
	defer m.Stop()

	rec := httptest.NewRecorder()
	m.handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("precondition: status = %d, want 200", rec.Code)
	}

	m.probeFn = func() (string, error) { return "database unreachable", errNotApplied }
	m.evaluate() // fails, but we are well inside the grace window

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
	m := newReadinessMonitorWithConfig(migratedTestDB(t), nil, time.Hour, 0, 2*time.Second)
	defer m.Stop()

	m.probeFn = func() (string, error) { return "database unreachable", errNotApplied }
	time.Sleep(2 * time.Millisecond) // ensure now-lastOK exceeds the zero grace
	m.evaluate()

	rec := httptest.NewRecorder()
	m.handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once the grace window has expired", rec.Code)
	}
}

// The probe must not borrow from the pool. Probing through pool.Ping would
// reintroduce the original bug in slower motion: a saturated pool starves the
// probe just as it starved the inline check, so a saturation outlasting the
// grace window would evict the instance anyway. Here every pooled connection is
// held open and the probe must still succeed.
func TestReadinessProbeSurvivesPoolExhaustion(t *testing.T) {
	pool := migratedTestDB(t)
	m := newReadinessMonitorWithConfig(pool, nil, time.Hour, 0, 3*time.Second)
	defer m.Stop()

	ctx := context.Background()
	held := make([]*pgxpool.Conn, 0, pool.Config().MaxConns)
	for i := int32(0); i < pool.Config().MaxConns; i++ {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		held = append(held, c)
	}
	defer func() {
		for _, c := range held {
			c.Release()
		}
	}()
	if pool.Stat().AcquiredConns() != pool.Config().MaxConns {
		t.Fatalf("precondition: pool not exhausted (%d/%d acquired)",
			pool.Stat().AcquiredConns(), pool.Config().MaxConns)
	}

	// grace is 0, so any probe failure flips readiness immediately.
	m.evaluate()

	rec := httptest.NewRecorder()
	m.handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: an exhausted pool made the probe report the database unreachable; body=%s",
			rec.Code, rec.Body.String())
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
