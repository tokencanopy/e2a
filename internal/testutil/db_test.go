package testutil

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDBErrorChildEnv = "E2A_TEST_DB_ERROR_CHILD"

func TestTruncateAll_CleansSendingProtectionNoFKState(t *testing.T) {
	pool := TestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		UPDATE sending_protection_runtime_policy SET generation=9, activated_by='schema-test';
		UPDATE sending_protection_runtime_attestation SET revision=9, active_billing_digest='sha256:synthetic', active_billing_contract=1, updated_by='schema-test';
		INSERT INTO sending_protection_policy_events (generation,prior_generation,prior_policy_sha256,new_policy_sha256,actor,reason) VALUES (9,0,repeat('a',64),repeat('b',64),'schema-test','synthetic');
		INSERT INTO sending_operator_recipient_versions (logical_version,commitment_key_id,recipient_commitment,created_by) VALUES (9,repeat('a',64),repeat('b',64),'schema-test');
		INSERT INTO sending_ramp_grandfathering (singleton,policy_generation,completed_at,completed_by) VALUES (true,0,now(),'schema-test');
		INSERT INTO account_sending_control_events (id,account_ref,old_state,new_state,reason,actor,expires_at) VALUES ('control_cleanup','usr_cleanup','active','paused','synthetic','schema-test',now()+interval '90 days');
		INSERT INTO sending_provider_operations (operation_id,source_account_ref,policy_subject_ref,purpose,expires_at) VALUES ('op_cleanup','usr_cleanup','usr_cleanup','customer_message',now()+interval '30 days');
		INSERT INTO sending_budget_counters (scope,scope_id,day,daily_limit) VALUES ('account_daily','usr_cleanup',CURRENT_DATE,1);
		INSERT INTO sending_budget_reservations (operation_id,submission_attempt,source_account_ref,policy_subject_ref,purpose,day,units,probation,state) VALUES ('op_cleanup',1,'usr_cleanup','usr_cleanup','customer_message',CURRENT_DATE,1,false,'reserved');
		INSERT INTO sending_protection_notice_events (id,kind,reason_code,budget_scope,ledger_day,expires_at) VALUES ('notice_cleanup','global_guardrail','global_budget_exhausted','global_all',CURRENT_DATE,now()+interval '30 days');
		INSERT INTO sending_protection_notice_deliveries (event_id,audience) VALUES ('notice_cleanup','operator');
		INSERT INTO sending_feedback_correlations (correlation_id,operation_id,submission_attempt,source_account_ref,policy_subject_ref,purpose,tenant_mode) VALUES ('corr_cleanup','op_cleanup',1,'usr_cleanup','usr_cleanup','customer_message','none');
		INSERT INTO sending_feedback_recipients (correlation_id,recipient_hmac,hmac_key_version) VALUES ('corr_cleanup',decode(repeat('ab',32),'hex'),1);
		INSERT INTO sending_feedback_events (provider_event_id,correlation_id,provider_occurred_at) VALUES ('feedback_cleanup','corr_cleanup',now());
	`); err != nil {
		t.Fatalf("seed sending-protection cleanup state: %v", err)
	}

	if err := truncateAll(ctx, pool); err != nil {
		t.Fatalf("truncate sending-protection state: %v", err)
	}

	for _, table := range []string{
		"sending_protection_policy_events",
		"sending_operator_recipient_versions",
		"sending_ramp_grandfathering",
		"account_sending_control_events",
		"sending_provider_operations",
		"sending_budget_counters",
		"sending_budget_reservations",
		"sending_protection_notice_events",
		"sending_protection_notice_deliveries",
		"sending_feedback_correlations",
		"sending_feedback_recipients",
		"sending_feedback_events",
	} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Errorf("%s rows after cleanup=%d err=%v", table, count, err)
		}
	}

	var generation, revision int64
	var policyHash, activeDigest, rollbackDigest string
	var activeContract, rollbackContract int
	if err := pool.QueryRow(ctx, `SELECT generation,policy_sha256 FROM sending_protection_runtime_policy WHERE singleton`).Scan(&generation, &policyHash); err != nil {
		t.Fatalf("read reset runtime policy: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT revision,active_billing_digest,active_billing_contract,rollback_billing_digest,rollback_billing_contract FROM sending_protection_runtime_attestation WHERE singleton`).Scan(&revision, &activeDigest, &activeContract, &rollbackDigest, &rollbackContract); err != nil {
		t.Fatalf("read reset attestation: %v", err)
	}
	if generation != 0 || policyHash != "198d8cfb3220b6094a3b8dfe13cb0e2ff97c512ad87ae14609e580ae335c9ce6" {
		t.Errorf("runtime policy reset=(generation=%d hash=%q)", generation, policyHash)
	}
	if revision != 0 || activeDigest != "" || activeContract != 0 || rollbackDigest != "" || rollbackContract != 0 {
		t.Errorf("runtime attestation reset=(%d,%q,%d,%q,%d)", revision, activeDigest, activeContract, rollbackDigest, rollbackContract)
	}
}

func TestTruncateAll_CleansOperatorRegistryWithoutExclusiveTableLock(t *testing.T) {
	pool := TestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO sending_operator_recipient_versions
		    (logical_version,commitment_key_id,recipient_commitment,created_by)
		VALUES (10,repeat('a',64),repeat('b',64),'schema-lock-test')
	`); err != nil {
		t.Fatalf("seed operator registry: %v", err)
	}

	reader, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin registry reader: %v", err)
	}
	defer reader.Rollback(ctx) //nolint:errcheck
	var before int
	if err := reader.QueryRow(ctx, `SELECT count(*) FROM sending_operator_recipient_versions`).Scan(&before); err != nil || before != 1 {
		t.Fatalf("registry rows before cleanup=%d err=%v", before, err)
	}

	cleanupCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := truncateAll(cleanupCtx, pool); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			t.Fatalf("operator registry cleanup requested an exclusive lock: %v", err)
		}
		t.Fatalf("operator registry cleanup failed: %v", err)
	}
	if err := reader.Rollback(ctx); err != nil {
		t.Fatalf("rollback registry reader: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_operator_recipient_versions`).Scan(&after); err != nil || after != 0 {
		t.Fatalf("registry rows after cleanup=%d err=%v", after, err)
	}
}

func TestTruncateAll_CleansInboundIntakeWithoutExclusiveTableLock(t *testing.T) {
	pool := TestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO inbound_intake (id, recipient, raw_message, content_hash)
		VALUES ('intk_cleanup_lock', 'agent@example.com', 'raw', 'hash')
	`)
	if err != nil {
		t.Fatalf("seed inbound_intake: %v", err)
	}

	reader, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin reader transaction: %v", err)
	}
	defer reader.Rollback(ctx) //nolint:errcheck

	var before int
	if err := reader.QueryRow(ctx, `SELECT count(*) FROM inbound_intake`).Scan(&before); err != nil {
		t.Fatalf("read inbound_intake: %v", err)
	}
	if before != 1 {
		t.Fatalf("inbound_intake rows before cleanup = %d, want 1", before)
	}

	// The budget here is a safety net against a genuine hang, NOT the assertion.
	// What proves the invariant is the SQLSTATE below: truncateAll sets
	// lock_timeout, so a conflicting lock surfaces as 55P03 (lock_not_available)
	// within seconds rather than blocking until this context expires.
	//
	// It must stay generous. An earlier version budgeted one second and treated
	// ANY error as proof of a lock conflict, which made this test flaky in CI:
	// under `make cover` (-p 4, every package provisioning a database and
	// applying all migrations against one Postgres) cleanup legitimately exceeds
	// a second, and the deadline error was reported as a lock problem that did
	// not exist. Slowness and lock contention are different failures and this
	// test must only fail for the second one.
	cleanupCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := truncateAll(cleanupCtx, pool); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			t.Fatalf("cleanup blocked on a conflicting lock while a reader held ACCESS SHARE on "+
				"inbound_intake — it must be cleaned with DELETE, not TRUNCATE: %v", err)
		}
		t.Fatalf("cleanup failed for a non-lock reason: %v", err)
	}

	if err := reader.Rollback(ctx); err != nil {
		t.Fatalf("rollback reader transaction: %v", err)
	}

	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbound_intake`).Scan(&after); err != nil {
		t.Fatalf("count inbound_intake after cleanup: %v", err)
	}
	if after != 0 {
		t.Fatalf("inbound_intake rows after cleanup = %d, want 0", after)
	}
}

func TestRunMigrations_RepeatedPreparationUsesTrackerWithoutConsumingAttributeSlots(t *testing.T) {
	pool := TestDB(t)
	ctx := context.Background()

	var before int
	if err := pool.QueryRow(ctx, `
		SELECT max(attnum)
		FROM pg_attribute
		WHERE attrelid = 'public.agent_identities'::regclass
		  AND attnum > 0
	`).Scan(&before); err != nil {
		t.Fatalf("read agent_identities max attnum before replay: %v", err)
	}

	if err := runMigrations(ctx, pool); err != nil {
		t.Fatalf("repeat migrations: %v", err)
	}

	var after int
	if err := pool.QueryRow(ctx, `
		SELECT max(attnum)
		FROM pg_attribute
		WHERE attrelid = 'public.agent_identities'::regclass
		  AND attnum > 0
	`).Scan(&after); err != nil {
		t.Fatalf("read agent_identities max attnum after replay: %v", err)
	}

	var hasTracker bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.schema_migrations') IS NOT NULL`,
	).Scan(&hasTracker); err != nil {
		t.Fatalf("check schema_migrations tracker: %v", err)
	}

	if after != before || !hasTracker {
		t.Fatalf("repeated migration preparation: max attnum %d -> %d, schema_migrations exists=%v; want unchanged attnum and tracker", before, after, hasTracker)
	}

	var tracked int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM schema_migrations
		WHERE filename IN ('003_hitl.sql', '036_hitl_mode.sql', '043_drop_hitl_columns.sql')
	`).Scan(&tracked); err != nil {
		t.Fatalf("count tracked HITL migrations: %v", err)
	}
	if tracked != 3 {
		t.Fatalf("tracked HITL migrations = %d, want 3", tracked)
	}
}

func TestTestDB_PreparationFailureIsFatal(t *testing.T) {
	if os.Getenv(testDBErrorChildEnv) == t.Name() {
		_ = TestDB(t)
		return
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, TestDBURL())
	if err != nil {
		t.Skipf("test database not available: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("test database not available: %v", err)
	}

	dbURL, err := url.Parse(TestDBURL())
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	query := dbURL.Query()
	query.Set("search_path", "e2a_test_missing_schema")
	dbURL.RawQuery = query.Encode()

	output, err := runTestDBErrorChild(t, dbURL.String())
	if err == nil {
		t.Fatalf("TestDB preparation failure exited successfully; want fatal failure\n%s", output)
	}
	if !strings.Contains(output, "failed to prepare test database") {
		t.Fatalf("fatal output missing preparation context:\n%s", output)
	}
}

func TestTestDB_UnavailableDatabaseStillSkips(t *testing.T) {
	if os.Getenv(testDBErrorChildEnv) == t.Name() {
		_ = TestDB(t)
		return
	}

	const unavailableURL = "postgres://e2a:e2a@127.0.0.1:1/e2a_test?sslmode=disable&connect_timeout=1"
	output, err := runTestDBErrorChild(t, unavailableURL)
	if err != nil {
		t.Fatalf("unavailable test database should skip, not fail: %v\n%s", err, output)
	}
	if !strings.Contains(output, "SKIP") {
		t.Fatalf("unavailable test database output missing skip:\n%s", output)
	}
}

func runTestDBErrorChild(t *testing.T, dbURL string) (string, error) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = testDBChildEnv(t.Name(), dbURL)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func testDBChildEnv(testName, dbURL string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, testDBErrorChildEnv+"=") ||
			strings.HasPrefix(item, "E2A_TEST_DATABASE_URL=") {
			continue
		}
		env = append(env, item)
	}
	return append(env, testDBErrorChildEnv+"="+testName, "E2A_TEST_DATABASE_URL="+dbURL)
}

// workspaceSuffix is the per-CHECKOUT half of the isolation. Per-package alone
// let two agents (or two worktrees, or a second terminal) running the same
// package share a database and corrupt each other, which is how an unmerged
// branch's migration ended up producing ~40 spurious failures in a run.
func TestWorkspaceSuffixIsStablePerPathAndDistinctAcrossPaths(t *testing.T) {
	const a = "/Users/dev/Desktop/e2a"
	const b = "/Users/dev/.agents/worktrees/e2a/feature-x"

	if got, again := workspaceSuffix(a), workspaceSuffix(a); got != again {
		t.Errorf("same path must derive the same suffix: %q vs %q", got, again)
	}
	if workspaceSuffix(a) == workspaceSuffix(b) {
		t.Errorf("different checkouts must derive different suffixes; both gave %q", workspaceSuffix(a))
	}
	// Trailing separators and redundant elements name the same checkout.
	if workspaceSuffix(a) != workspaceSuffix(a+"/") {
		t.Errorf("path must be cleaned before hashing: %q vs %q", workspaceSuffix(a), workspaceSuffix(a+"/"))
	}
	if got := workspaceSuffix(a); !strings.HasPrefix(got, "_ws") || len(got) != len("_ws")+8 {
		t.Errorf("suffix = %q, want _ws + 8 hex chars", got)
	}
	// Unresolvable root degrades to per-package-only rather than failing.
	if got := workspaceSuffix(""); got != "" {
		t.Errorf("empty root should yield no workspace component, got %q", got)
	}
}

// The derived name must carry BOTH components, so a database is unique to
// (checkout, package) rather than to package alone.
func TestTestDBURLIsUniquePerWorkspaceAndPackage(t *testing.T) {
	// Pin BOTH inputs. Reading the ambient environment made this test fail for
	// reasons that were nothing to do with the code: E2A_TEST_DB_SHARED=1 (a
	// documented escape hatch) suppresses derivation entirely, and a long custom
	// base — which this PR newly makes possible through make — blew the length
	// assertion below. A test that fails because of how the developer configured
	// their machine is the same defect as a test with a wall-clock budget.
	t.Setenv("E2A_TEST_DB_SHARED", "")
	t.Setenv("E2A_TEST_DATABASE_URL", "postgres://e2a:e2a@localhost:5433/e2a_test?sslmode=disable")

	u, err := url.Parse(TestDBURL())
	if err != nil {
		t.Fatalf("parse TestDBURL: %v", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(name, "_ws") {
		t.Errorf("dbname = %q, want a _ws<hash> workspace component", name)
	}
	if !strings.HasSuffix(name, "_pkg_testutil") {
		t.Errorf("dbname = %q, want the _pkg_testutil suffix retained", name)
	}
	if ws := workspaceSuffix(moduleRootDir()); ws == "" || !strings.Contains(name, ws) {
		t.Errorf("dbname = %q, want it to contain this checkout's suffix %q", name, ws)
	}
	// With the base pinned above this is a real assertion about the derivation's
	// size, not about the machine it runs on.
	if len(name) > maxPostgresIdentifier {
		t.Errorf("dbname %q is %d bytes, over Postgres's %d-byte identifier limit",
			name, len(name), maxPostgresIdentifier)
	}
}

// A base too long to derive from must FAIL, not silently truncate. Postgres cuts
// identifiers at 63 bytes from the end — inside the package component — so
// internal/identity and internal/idempotency would collapse onto ..._pkg_ide and
// truncate each other's tables: precisely the corruption this derivation prevents.
// Reachable now that the Makefile honors an exported base, where naming it after a
// branch is the obvious first thing someone tries.
func TestTestDBURLPanicsRatherThanLetPostgresTruncate(t *testing.T) {
	t.Setenv("E2A_TEST_DB_SHARED", "")
	t.Setenv("E2A_TEST_DATABASE_URL",
		"postgres://e2a:e2a@localhost:5433/"+strings.Repeat("b", 60)+"?sslmode=disable")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("an over-long derived name must panic, not silently truncate into a collision")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "63") || !strings.Contains(msg, "E2A_TEST_DATABASE_URL") {
			t.Errorf("panic should name the limit and the fix, got: %q", msg)
		}
	}()

	_ = TestDBURL()
}

func TestTestDBURLDerivesPerPackageDatabase(t *testing.T) {
	// Inside a `go test` binary (os.Args[0] ends in ".test"), TestDBURL
	// appends a per-package suffix to the base database name so packages
	// running in parallel (-p N) cannot truncate each other's rows — the
	// harness truncates between tests, which made a shared DB the
	// documented cross-package flake source. This binary is testutil.test,
	// so the derived name is <base>_pkg_testutil.
	u, err := url.Parse(TestDBURL())
	if err != nil {
		t.Fatalf("parse TestDBURL: %v", err)
	}
	if got := strings.TrimPrefix(u.Path, "/"); !strings.HasSuffix(got, "_pkg_testutil") {
		t.Errorf("TestDBURL dbname = %q, want *_pkg_testutil suffix", got)
	}

	// E2A_TEST_DB_SHARED=1 restores the verbatim single-DB behavior (escape
	// hatch for tooling that must target one known database).
	t.Setenv("E2A_TEST_DB_SHARED", "1")
	u2, err := url.Parse(TestDBURL())
	if err != nil {
		t.Fatalf("parse shared TestDBURL: %v", err)
	}
	if got := strings.TrimPrefix(u2.Path, "/"); strings.Contains(got, "_pkg_") {
		t.Errorf("shared-mode dbname = %q, want no _pkg_ suffix", got)
	}
}

// TestTestDBURLDerivesForNonTestBinaries proves the derivation also covers
// cmd/e2a-contract-server (see #827): a symlink to this test binary under a
// non-test name reproduces a real non-".test" argv[0].
func TestTestDBURLDerivesForNonTestBinaries(t *testing.T) {
	if os.Getenv(testDBErrorChildEnv) == t.Name() {
		u, err := url.Parse(TestDBURL())
		if err != nil {
			t.Fatalf("parse TestDBURL: %v", err)
		}
		fmt.Printf("DBNAME=%s\n", strings.TrimPrefix(u.Path, "/"))
		return
	}

	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve this test binary's path: %v", err)
	}
	renamed := filepath.Join(t.TempDir(), "e2a-contract-server")
	if err := os.Symlink(self, renamed); err != nil {
		t.Fatalf("symlink the test binary under a non-test name: %v", err)
	}

	cmd := exec.Command(renamed, "-test.run=^"+t.Name()+"$")
	cmd.Env = testDBChildEnv(t.Name(), "postgres://e2a:e2a@localhost:5433/e2a_test?sslmode=disable")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, output)
	}

	var dbname string
	for _, line := range strings.Split(string(output), "\n") {
		if rest, ok := strings.CutPrefix(line, "DBNAME="); ok {
			dbname = rest
			break
		}
	}
	if dbname == "" {
		t.Fatalf("child did not report a dbname:\n%s", output)
	}
	if !strings.Contains(dbname, "_ws") {
		t.Errorf("dbname = %q from a non-test binary, want a _ws<hash> workspace component", dbname)
	}
	if !strings.HasSuffix(dbname, "_pkg_e2a_contract_server") {
		t.Errorf("dbname = %q, want the _pkg_e2a_contract_server suffix", dbname)
	}
}

func TestOpenPreparedTestDBCreatesMissingDatabase(t *testing.T) {
	// First use of a package database must self-provision: connect failure
	// with SQLSTATE 3D000 creates the database from the base URL's server
	// and retries. Drop the derived DB first so this exercises creation.
	ctx := context.Background()
	base, err := url.Parse(TestDBURL())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	scratch := *base
	scratch.Path = "/" + strings.TrimPrefix(base.Path, "/") + "_selfprov"

	admin, err := pgxpool.New(ctx, TestDBURL())
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("test database not available: %v", err)
	}
	name := strings.TrimPrefix(scratch.Path, "/")
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("drop scratch db: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize())
	})

	pool, err := OpenPreparedTestDB(ctx, scratch.String())
	if err != nil {
		t.Fatalf("OpenPreparedTestDB should create the missing database, got: %v", err)
	}
	pool.Close()

	// Idempotent on the second open (database now exists and is migrated).
	pool2, err := OpenPreparedTestDB(ctx, scratch.String())
	if err != nil {
		t.Fatalf("second OpenPreparedTestDB: %v", err)
	}
	pool2.Close()
}

func TestTestDBURLDerivationIsIdempotentAndURLOnly(t *testing.T) {
	// Re-deriving an already-derived URL must be a no-op — the harness's
	// child-exec tests hand TestDBURL() to a spawned .test process, which
	// re-derives; double-suffixing would self-provision junk databases.
	t.Setenv("E2A_TEST_DATABASE_URL", TestDBURL())
	u, err := url.Parse(TestDBURL())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.TrimPrefix(u.Path, "/"); strings.Contains(got, "_pkg_testutil_pkg_") {
		t.Errorf("double-derived dbname %q", got)
	}

	// DSN keyword/value form must pass through verbatim: url.Parse "accepts"
	// it into u.Path, and appending a suffix there produces garbage.
	dsn := "host=localhost port=5433 user=e2a dbname=e2a_test sslmode=disable"
	t.Setenv("E2A_TEST_DATABASE_URL", dsn)
	if got := TestDBURL(); got != dsn {
		t.Errorf("DSN-form base mangled: %q", got)
	}
}
