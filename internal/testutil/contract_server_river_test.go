package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/testutil/testdb"
)

const (
	contractDBReachabilityTimeout = 10 * time.Second
	contractDBPreparationTimeout  = 90 * time.Second
)

func TestContractServerRepeatedStartClearsRiverStateAndPreservesMigrations(t *testing.T) {
	dbURL := requireReachableContractTestDB(t)
	ctx := context.Background()
	first, err := StartContractServer(ctx, dbURL)
	if err != nil {
		t.Fatalf("start first contract server after successful DB preflight: %v", err)
	}
	t.Cleanup(func() { _ = first.Close(context.Background()) })

	var migrationCount int
	if err := first.DBPool.QueryRow(ctx, `SELECT count(*) FROM river_migration`).Scan(&migrationCount); err != nil {
		t.Fatalf("count River migrations: %v", err)
	}
	if migrationCount == 0 {
		t.Fatal("River migration ledger is empty")
	}
	if _, err := first.DBPool.Exec(ctx, `INSERT INTO river_job (args, kind) VALUES ('{}', 'contract_stale_job')`); err != nil {
		t.Fatalf("insert stale River job: %v", err)
	}
	_ = first.Close(ctx)

	second, err := StartContractServer(ctx, dbURL)
	if err != nil {
		t.Fatalf("restart contract server: %v", err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })

	var jobCount, migrationCountAfter int
	if err := second.DBPool.QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&jobCount); err != nil {
		t.Fatalf("count River jobs after restart: %v", err)
	}
	if err := second.DBPool.QueryRow(ctx, `SELECT count(*) FROM river_migration`).Scan(&migrationCountAfter); err != nil {
		t.Fatalf("count River migrations after restart: %v", err)
	}
	if jobCount != 0 {
		t.Fatalf("River jobs after restart = %d, want 0", jobCount)
	}
	if migrationCountAfter != migrationCount {
		t.Fatalf("River migration ledger count after restart = %d, want %d", migrationCountAfter, migrationCount)
	}
}

func runContractDBPreflight(
	parent context.Context,
	reachabilityTimeout time.Duration,
	preparationTimeout time.Duration,
	checkReachability func(context.Context) error,
	prepare func(context.Context) error,
) error {
	reachabilityCtx, cancelReachability := context.WithTimeout(parent, reachabilityTimeout)
	err := checkReachability(reachabilityCtx)
	cancelReachability()
	if err != nil {
		return err
	}

	preparationCtx, cancelPreparation := context.WithTimeout(parent, preparationTimeout)
	defer cancelPreparation()
	return prepare(preparationCtx)
}

func TestContractDBPreflightGivesPreparationAnIndependentBudget(t *testing.T) {
	err := runContractDBPreflight(
		context.Background(),
		0,
		time.Minute,
		func(ctx context.Context) error {
			if err := ctx.Err(); err != context.DeadlineExceeded {
				t.Fatalf("reachability context error = %v, want deadline exceeded", err)
			}
			return nil
		},
		func(ctx context.Context) error {
			return ctx.Err()
		},
	)
	if err != nil {
		t.Fatalf("preparation should retain its own timeout budget: %v", err)
	}
}

// requireReachableContractTestDB is the test's only skip gate. Once this
// check succeeds, migration, River reset, and server startup errors are
// regressions and the caller must fail rather than classifying them as DB
// unavailability. Reachability probes the configured base database because a
// raw ping of the derived per-package URL fails with 3D000 on every cold
// database (CI's service container is always cold). Preparation then goes
// through OpenPreparedTestDB with its own timeout so self-provisioning,
// migrations, and truncation do not consume the short reachability budget.
func requireReachableContractTestDB(t *testing.T) string {
	t.Helper()
	dbURL := TestDBURL()

	var pool *pgxpool.Pool
	reachable := false
	err := runContractDBPreflight(
		context.Background(),
		contractDBReachabilityTimeout,
		contractDBPreparationTimeout,
		func(ctx context.Context) error {
			probe, err := pgxpool.New(ctx, testdb.BaseTestDBURL())
			if err != nil {
				return err
			}
			defer probe.Close()
			if err := probe.Ping(ctx); err != nil {
				return err
			}
			reachable = true
			return nil
		},
		func(ctx context.Context) error {
			var err error
			pool, err = OpenPreparedTestDB(ctx, dbURL)
			return err
		},
	)
	if err != nil {
		if !reachable {
			t.Skipf("private test database unavailable: %v", err)
		}
		t.Fatalf("prepare contract test database: %v", err)
	}
	pool.Close()
	return dbURL
}
