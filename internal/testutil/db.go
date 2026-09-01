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
// E2A_TEST_DB_SHARED=1 gets
// the base URL verbatim. Missing databases self-provision on first open
// (see OpenPreparedTestDB). Concurrent sessions, agents, and worktrees are
// isolated by the per-workspace component below, so handing each runner its
// own base URL is no longer required — only useful for pointing a run at an
// entirely separate server.
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
	// Postgres truncates identifiers past maxPostgresIdentifier bytes, and it does
	// so SILENTLY. Truncation lands at the END — inside the package component — so
	// sibling packages sharing a prefix collapse onto ONE database: internal/identity
	// and internal/idempotency both become ..._pkg_ide, then truncate each other's
	// tables under -p 4. That is exactly the corruption this derivation exists to
	// prevent, so a base too long to derive from has to fail loudly rather than
	// quietly reintroduce it.
	if name := strings.TrimPrefix(u.Path, "/"); len(name) > maxPostgresIdentifier {
		panic(fmt.Sprintf("testutil: derived test database name %q is %d bytes, over Postgres's "+
			"%d-byte identifier limit — Postgres would truncate it silently and collide sibling "+
			"packages onto one database. Shorten the base in E2A_TEST_DATABASE_URL; the derived "+
			"suffix needs %d bytes.", name, len(name), maxPostgresIdentifier, len(suffix)))
	}
	return u.String()
}

// maxPostgresIdentifier is Postgres's NAMEDATALEN-1 ceiling for identifiers,
// database names included. Measured in BYTES, which is what len() reports.
const maxPostgresIdentifier = 63

// derivedDBSuffix derives the database-name suffix beneath the configured base:
// a per-WORKSPACE component plus a per-PACKAGE component, or "" when the process
// shares via E2A_TEST_DB_SHARED=1.
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
//   - sender_identity_managed_domains: deliberately survives domain deletion
//     until asynchronous provider teardown is confirmed.
//   - sending-protection security ledgers: provider operations, budget rows,
//     control audit, notice outbox, and feedback provenance deliberately have no
//     customer-tree FK so account/message deletion cannot erase them.
//   - sending-protection policy state: the event/marker tables have no FK, while
//     the runtime-policy and attestation singletons must be restored to their
//     migration-owned generation-zero sentinels between tests.
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
		DELETE FROM sender_identity_managed_domains;
		DELETE FROM sending_protection_notice_deliveries;
		DELETE FROM sending_protection_notice_events;
		DELETE FROM sending_feedback_recipients;
		DELETE FROM sending_feedback_events;
		DELETE FROM sending_feedback_correlations;
		DELETE FROM sending_budget_reservations;
		DELETE FROM sending_budget_counters;
		DELETE FROM sending_provider_operations;
		DELETE FROM account_sending_control_events;
		DELETE FROM sending_protection_policy_events;
		DELETE FROM sending_protection_runtime_attestation_events;
		DELETE FROM sending_ramp_grandfathering;

		-- This registry is append-only in application/migration use; its
		-- unconditional trigger intentionally rejects DELETE. The disposable
		-- test database bypasses user triggers for this one row-lock-scoped
		-- cleanup instead of using TRUNCATE's ACCESS EXCLUSIVE table lock.
		SET LOCAL session_replication_role = replica;
		DELETE FROM sending_operator_recipient_versions;
		SET LOCAL session_replication_role = origin;

		DELETE FROM sending_protection_runtime_policy;
		INSERT INTO sending_protection_runtime_policy
		    (singleton, generation, schema_version, policy, policy_sha256, activated_at, activated_by)
		VALUES (
		    true, 0, 1,
		    '{"all_customer_global_daily_recipients":5000,"bounce_min_outcomes":50,"bounce_pause_basis_points":400,"budget_hold_max_days":7,"budget_mode":"disabled","complaint_pause_basis_points":8,"critical_operational_daily_recipients":100,"daily_unlimited_plan_codes":["starter","pro","scale"],"default_account_daily_recipients":100,"detector_interval_seconds":300,"detector_mode":"disabled","detector_window_days":7,"operator_notice_recipient_version":1,"probation_global_daily_recipients":150,"ramp_days":30,"ramp_enabled":false,"ramp_start_daily":150,"ramp_target_daily":2000,"sending_control_audit_retention_days":90,"sending_feedback_post_account_retention_days":30,"shared_domain_account_daily_recipients":50,"shared_reputation_bounce_min_outcomes":1,"tenant_header_canary_account_ids":[],"tenant_header_mode":"disabled","tenant_provisioning_mode":"disabled","tenant_suppression_sync_mode":"disabled","violation_operational_daily_recipients":100}'::jsonb,
		    '198d8cfb3220b6094a3b8dfe13cb0e2ff97c512ad87ae14609e580ae335c9ce6',
		    now(), 'migration'
		);

		DELETE FROM sending_protection_runtime_attestation;
		INSERT INTO sending_protection_runtime_attestation
		    (singleton, revision, active_billing_digest, active_billing_contract,
		     rollback_billing_digest, rollback_billing_contract, updated_by)
		VALUES (true, 0, '', 0, '', 0, 'migration');

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
