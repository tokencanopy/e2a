package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/testutil/testdb"
)

// The database helpers live in the leaf package testdb so that packages this
// package depends on — outbound, and through it sendingpolicy — can use them
// from INTERNAL test files without an import cycle. These wrappers keep every
// existing caller working unchanged; new internal tests in those packages
// should import testdb directly.

// TestDBURL returns the per-workspace, per-package test database URL.
func TestDBURL() string { return testdb.TestDBURL() }

// OpenPreparedTestDB opens and prepares the database at dbURL.
func OpenPreparedTestDB(ctx context.Context, dbURL string) (*pgxpool.Pool, error) {
	return testdb.OpenPreparedTestDB(ctx, dbURL)
}

// TestDB returns a migrated, truncated pool for this test, skipping when no
// database is reachable.
func TestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testdb.TestDB(t)
}

// TruncateAll empties every application table.
func TruncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	testdb.TruncateAll(t, pool)
}
