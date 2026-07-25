package testutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/migrations"
)

const defaultTestDBURL = "postgres://e2a:e2a@localhost:5433/e2a_test?sslmode=disable"

type testDBPreparationError struct {
	stage string
	err   error
}

func (e *testDBPreparationError) Error() string {
	return fmt.Sprintf("%s: %v", e.stage, e.err)
}

func (e *testDBPreparationError) Unwrap() error {
	return e.err
}

// baseTestDBURL is the configured URL before per-package derivation: the
// E2A_TEST_DATABASE_URL override or the local-dev default. Also the admin
// connection target for creating missing package databases.
func baseTestDBURL() string {
	if dbURL := os.Getenv("E2A_TEST_DATABASE_URL"); dbURL != "" {
		return dbURL
	}
	return defaultTestDBURL
}

// TestDBURL returns the database URL tests should use. Inside a `go test`
// binary it derives a PER-PACKAGE database name (<base>_pkg_<package>) so
// packages can run in parallel: the harness truncates tables between tests,
// which made one shared database the documented cross-package flake source
// and forced -p 1 on every DB-backed run. The suffix comes from the test
// binary's name (os.Args[0] = <package>.test — unique per package in this
// repo), so every URL consumer in one test binary — TestDB, hand-built
// pools, the in-process contract server — lands on the same database.
// Non-test binaries (cmd/e2a-contract-server) and E2A_TEST_DB_SHARED=1 get
// the base URL verbatim. Missing databases self-provision on first open
// (see OpenPreparedTestDB); concurrent SESSIONS running the SAME package
// still contend, so per-session base URLs remain the guidance in AGENTS.md.
func TestDBURL() string {
	base := baseTestDBURL()
	suffix := derivedDBSuffix()
	if suffix == "" {
		return base
	}
	u, err := url.Parse(base)
	// Only derive on genuine postgres:// URLs. DSN keyword/value form
	// ("host=… dbname=…") "parses" into u.Path and would be mangled into
	// garbage; pass anything unrecognizable through verbatim.
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") ||
		strings.TrimPrefix(u.Path, "/") == "" {
		return base
	}
	// Idempotent: a child .test process handed an already-derived URL (the
	// harness's own re-exec tests do this) must not double-suffix it.
	if strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), suffix) {
		return base
	}
	u.Path = u.Path + suffix
	return u.String()
}

// derivedDBSuffix derives the database-name suffix beneath the configured base:
// a per-WORKSPACE component plus a per-PACKAGE component, or "" when the process
// is not a test binary or sharing is forced.
//
// Two dimensions, because per-package alone was not enough. It stops packages in
// ONE run from truncating each other, but every checkout computed the same names,
// so two agents (or two worktrees, or a second terminal) running the same package
// shared a database and corrupted each other. AGENTS.md asked people to hand out
// their own base URL; that is convention, and convention does not scale across
// callers who do not know about each other. Deriving from the module root path
// makes the isolation structural: two checkouts cannot collide even when nobody
// configures anything.
//
// Name length: <base>_ws<8>_pkg_<package> runs ~40 chars for this repo's longest
// package names, well inside Postgres's 63-byte identifier limit. A much longer
// custom base could push past it, where Postgres truncates silently — keep bases
// short.
func derivedDBSuffix() string {
	switch strings.ToLower(os.Getenv("E2A_TEST_DB_SHARED")) {
	case "1", "true", "yes":
		return ""
	}
	bin := filepath.Base(os.Args[0])
	if !strings.HasSuffix(bin, ".test") {
		return ""
	}
	name := strings.ToLower(strings.TrimSuffix(bin, ".test"))
	sanitized := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sanitized = append(sanitized, r)
		default:
			sanitized = append(sanitized, '_')
		}
	}
	return workspaceSuffix(moduleRootDir()) + "_pkg_" + string(sanitized)
}

// workspaceSuffix is the per-checkout component: a short, stable digest of the
// module root's absolute path. Empty when the root cannot be resolved, which
// degrades to the previous per-package-only behavior rather than failing.
//
// Pure and path-taking so it is directly testable: the same path must always
// give the same suffix, and different paths must differ.
func workspaceSuffix(moduleRoot string) string {
	if moduleRoot == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Clean(moduleRoot)))
	return "_ws" + hex.EncodeToString(sum[:])[:8]
}

// moduleRootDir returns the directory holding go.mod at or above the working
// directory, or "" if there is none. Symlinks are resolved so two paths that
// reach the same checkout derive the same workspace suffix.
func moduleRootDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	if resolved, rerr := filepath.EvalSymlinks(dir); rerr == nil {
		dir = resolved
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func OpenPreparedTestDB(ctx context.Context, dbURL string) (*pgxpool.Pool, error) {
	if dbURL == "" {
		dbURL = defaultTestDBURL
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return nil, err
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		// SQLSTATE 3D000 (invalid_catalog_name): the server is up but this
		// per-package database doesn't exist yet — self-provision it from
		// the base URL's server and retry once. Any other error (server
		// down, bad credentials) keeps the caller's skip-vs-fail semantics.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "3D000" {
			return nil, err
		}
		if cerr := createTestDatabase(ctx, dbURL); cerr != nil {
			return nil, cerr
		}
		pool, err = pgxpool.New(ctx, dbURL)
		if err != nil {
			return nil, err
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return nil, err
		}
	}

	if err := runMigrations(ctx, pool); err != nil {
		pool.Close()
		return nil, &testDBPreparationError{stage: "run migrations", err: err}
	}

	if err := truncateAll(ctx, pool); err != nil {
		pool.Close()
		return nil, &testDBPreparationError{stage: "truncate tables", err: err}
	}

	return pool, nil
}

func TestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx := context.Background()
	pool, err := OpenPreparedTestDB(ctx, TestDBURL())
	if err != nil {
		var preparationErr *testDBPreparationError
		if errors.As(err, &preparationErr) {
			t.Fatalf("failed to prepare test database: %v", err)
		}
		t.Skipf("test database not available: %v", err)
	}

	t.Cleanup(func() {
		TruncateAll(t, pool)
		pool.Close()
	})

	return pool
}

func TruncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	err := truncateAll(context.Background(), pool)
	if err != nil {
		t.Fatalf("failed to truncate tables: %v", err)
	}
}

// createTestDatabase creates dbURL's database via the base URL's server.
// A concurrent creator racing us is success — the database exists either
// way. Postgres reports that race two ways: 42P04 (duplicate_database, the
// already-committed case) and 23505 (unique violation on
// pg_database_datname_index, the losing side of a true concurrent race —
// empirically what 8 parallel same-name creates produce on PG16).
//
// Error classification is load-bearing for skip-vs-fail: a failure to
// CONNECT to the base URL keeps the caller's "DB unavailable → skip"
// semantics, but a failure to CREATE on a reachable server (e.g. a role
// without CREATEDB) is a preparation error — TestDB must FAIL loudly, not
// silently skip the entire DB tier green.
func createTestDatabase(ctx context.Context, dbURL string) error {
	target, err := url.Parse(dbURL)
	if err != nil {
		return &testDBPreparationError{stage: "parse target db url", err: err}
	}
	name := strings.TrimPrefix(target.Path, "/")
	if name == "" {
		return &testDBPreparationError{stage: "derive database name", err: fmt.Errorf("no database name in %s", dbURL)}
	}
	conn, err := pgx.Connect(ctx, baseTestDBURL())
	if err != nil {
		return fmt.Errorf("connect base db to create %s: %w", name, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42P04" || pgErr.Code == "23505") {
			return nil
		}
		return &testDBPreparationError{stage: "create database " + name, err: err}
	}
	return nil
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	return identity.RunMigrations(ctx, pool, migrations.FS, identity.ModeAuto)
}

// truncateAll resets the DB between tests. Most tables are reached implicitly by
// TRUNCATE ... CASCADE via their FK path to users/messages/webhooks, so they need
// no explicit mention. Tables with NO foreign key at all cannot be reached by
// CASCADE, so they need explicit cleanup. Currently that is:
//
//   - inbound_intake: written at the SMTP edge BEFORE the agent lookup, so it
//     deliberately has no FK. Omitting it left stale dedup rows behind and made
//     TestInboundIntake_InsertLoadDedup / _StampProcessAndFail fail on a re-run
//     (the "insert must be new" assertions saw the previous run's rows).
//
// Use DELETE for FK-less tables instead of adding them to TRUNCATE. The test suite
// calls this helper hundreds of times; repeatedly truncating inbound_intake also
// recreates and fsyncs its three indexes and requires an ACCESS EXCLUSIVE lock.
// Any future FK-less table MUST be added to the DELETE section here.
// truncateAllLockTimeout bounds how long cleanup will WAIT ON A LOCK — not how
// long it may take. Cleanup is expected to be lock-free (inbound_intake is
// DELETEd precisely so a concurrent reader's ACCESS SHARE cannot block it), so a
// wait this long means something genuinely holds a conflicting lock. Failing
// fast with SQLSTATE 55P03 (lock_not_available) makes that case
// self-identifying, instead of hanging until the caller's context expires and
// reporting an indistinguishable deadline error.
//
// Deliberately NOT a statement timeout: cleanup is legitimately slow under a
// loaded parallel run (`-p 4` across every package), and slowness must not be
// conflated with a lock conflict — that conflation is what made
// TestTruncateAll_CleansInboundIntakeWithoutExclusiveTableLock flaky in CI.
const truncateAllLockTimeout = "5s"

func truncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		SET LOCAL lock_timeout = '`+truncateAllLockTimeout+`';

		DELETE FROM inbound_intake;

		TRUNCATE oauth_pkce_requests, oauth_refresh_tokens, oauth_access_tokens,
		         oauth_auth_codes, oauth_clients,
		         usage_summaries, usage_events, webhook_deliveries,
		         send_attempts, protection_events, messages,
		         idempotency_keys, api_keys,
		         agent_identities, domains,
		         user_sessions, users CASCADE
	`)
	if err != nil {
		return err
	}
	// Re-seed shared domain (migration seeds it but truncation removes it)
	pool.Exec(ctx, `INSERT INTO domains (domain, user_id, verified, verified_at) VALUES ('agents.e2a.dev', NULL, true, now()) ON CONFLICT DO NOTHING`)
	return nil
}
