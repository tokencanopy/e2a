package identity_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/migrations"
)

func TestMessagesFailureReasonCodeMigrationIsNullableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	sql, err := migrations.FS.ReadFile("076_messages_failure_reason_code.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("second migration application: %v", err)
	}
	var nullable, defaultValue string
	if err := pool.QueryRow(ctx, `SELECT is_nullable,COALESCE(column_default,'') FROM information_schema.columns WHERE table_schema='public' AND table_name='messages' AND column_name='delivery_failure_reason_code'`).Scan(&nullable, &defaultValue); err != nil {
		t.Fatal(err)
	}
	if nullable != "YES" || defaultValue != "" {
		t.Fatalf("column nullable=%q default=%q", nullable, defaultValue)
	}
}

func TestMessagesFailureProvenanceMigrationIsNullableDefaultFreeAndIdempotent(t *testing.T) {
	ctx := context.Background()
	sql, err := migrations.FS.ReadFile("077_messages_failure_provenance.sql")
	if err != nil {
		t.Fatal(err)
	}
	pool := testutil.TestDB(t)
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("second migration application: %v", err)
	}
	for _, column := range []string{
		"delivery_failure_occurred_at",
		"delivery_failure_attempt",
		"delivery_failure_blocked_recipients",
	} {
		var nullable, defaultValue string
		if err := pool.QueryRow(ctx, `SELECT is_nullable,COALESCE(column_default,'') FROM information_schema.columns WHERE table_schema='public' AND table_name='messages' AND column_name=$1`, column).Scan(&nullable, &defaultValue); err != nil {
			t.Fatal(err)
		}
		if nullable != "YES" || defaultValue != "" {
			t.Fatalf("column %s nullable=%q default=%q", column, nullable, defaultValue)
		}
	}
}

// stubFS builds an fs.FS with the given filename → SQL body mapping.
// Order isn't preserved by MapFS but RunMigrations sorts by filename.
func stubFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func TestParseMigrationMode(t *testing.T) {
	cases := []struct {
		in   string
		want identity.MigrationMode
		err  bool
	}{
		{"", identity.ModeAuto, false},
		{"auto", identity.ModeAuto, false},
		{"verify", identity.ModeVerify, false},
		{"skip", identity.ModeSkip, false},
		{"AUTO", "", true}, // case-sensitive on purpose
		{"yolo", "", true},
		{"true", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := identity.ParseMigrationMode(c.in)
			if (err != nil) != c.err {
				t.Fatalf("err = %v, want err=%v", err, c.err)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// TestRunMigrations_AppliesPending exercises the auto path against a
// real Postgres test DB. The DB starts with all real migrations applied
// (via testutil), so we test a *fresh* set of stub migrations layered
// on top — both run cleanly and record into schema_migrations.
func TestRunMigrations_AppliesPending(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_dummy_a, migrate_test_dummy_b")
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename IN ('test_a.sql','test_b.sql')")

	fsys := stubFS(map[string]string{
		"test_a.sql": "CREATE TABLE IF NOT EXISTS migrate_test_dummy_a (id TEXT PRIMARY KEY);",
		"test_b.sql": "CREATE TABLE IF NOT EXISTS migrate_test_dummy_b (id TEXT PRIMARY KEY);",
	})

	if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// Both tables should exist now.
	for _, table := range []string{"migrate_test_dummy_a", "migrate_test_dummy_b"} {
		var ok bool
		err := pool.QueryRow(ctx,
			"SELECT to_regclass($1) IS NOT NULL", "public."+table,
		).Scan(&ok)
		if err != nil || !ok {
			t.Fatalf("expected table %s to exist (err=%v)", table, err)
		}
	}

	// Both filenames should be in schema_migrations.
	for _, name := range []string{"test_a.sql", "test_b.sql"} {
		var count int
		err := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM schema_migrations WHERE filename = $1", name,
		).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("expected %s recorded once (count=%d, err=%v)", name, count, err)
		}
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_dummy_a, migrate_test_dummy_b")
		_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename IN ('test_a.sql','test_b.sql')")
	})
}

// TestRunMigrations_Idempotent verifies a second invocation is a no-op
// and doesn't double-record. Relies on schema_migrations tracking.
func TestRunMigrations_Idempotent(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_idemp")
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = 'idemp.sql'")

	fsys := stubFS(map[string]string{
		"idemp.sql": "CREATE TABLE IF NOT EXISTS migrate_test_idemp (id TEXT PRIMARY KEY);",
	})

	if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto); err != nil {
		t.Fatalf("second run: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE filename = 'idemp.sql'",
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected single record, got %d", count)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_idemp")
		_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = 'idemp.sql'")
	})
}

// TestRunMigrations_FailingMigrationRollsBack ensures the tracker
// insert is rolled back when the SQL itself errors — so a retry can
// fix the SQL and re-apply.
func TestRunMigrations_FailingMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = 'bad.sql'")

	fsys := stubFS(map[string]string{
		"bad.sql": "THIS IS NOT VALID SQL;",
	})

	err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto)
	if err == nil {
		t.Fatal("expected error from invalid SQL")
	}
	if !strings.Contains(err.Error(), "bad.sql") {
		t.Fatalf("error should name the failing migration, got: %v", err)
	}

	// schema_migrations must NOT have the failed migration recorded.
	var count int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE filename = 'bad.sql'",
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed migration should not be recorded, got count=%d", count)
	}
}

// TestRunMigrations_VerifyModeRefusesToApply ensures verify mode
// returns an error listing the pending file(s) and applies nothing.
func TestRunMigrations_VerifyModeRefusesToApply(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_verify_only")
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = 'verify_only.sql'")

	fsys := stubFS(map[string]string{
		"verify_only.sql": "CREATE TABLE IF NOT EXISTS migrate_test_verify_only (id TEXT);",
	})

	err := identity.RunMigrations(ctx, pool, fsys, identity.ModeVerify)
	if err == nil {
		t.Fatal("verify mode with pending should error")
	}
	if !strings.Contains(err.Error(), "verify_only.sql") {
		t.Fatalf("error should list the pending file, got: %v", err)
	}

	// The table must NOT have been created.
	var ok bool
	if err := pool.QueryRow(ctx,
		"SELECT to_regclass('public.migrate_test_verify_only') IS NOT NULL",
	).Scan(&ok); err != nil || ok {
		t.Fatalf("verify mode should not have applied (ok=%v, err=%v)", ok, err)
	}
}

// TestRunMigrations_SkipModeIsNoop ensures skip mode does not apply pending SQL.
func TestRunMigrations_SkipModeIsNoop(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)

	fsys := stubFS(map[string]string{
		"any.sql": "CREATE TABLE IF NOT EXISTS migrate_test_skip (id TEXT);",
	})

	if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeSkip); err != nil {
		t.Fatalf("skip should not error: %v", err)
	}

	var ok bool
	if err := pool.QueryRow(ctx,
		"SELECT to_regclass('public.migrate_test_skip') IS NOT NULL",
	).Scan(&ok); err != nil || ok {
		t.Fatalf("skip mode should not have applied (ok=%v, err=%v)", ok, err)
	}
}

// TestRunMigrations_OrdersByFilename ensures lexicographic order is
// the apply order — important because the project numbers migrations
// (001_, 002_, …) to enforce sequence.
func TestRunMigrations_OrdersByFilename(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_seq")
	for _, n := range []string{"001_seq.sql", "002_seq.sql", "003_seq.sql"} {
		_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", n)
	}

	fsys := stubFS(map[string]string{
		"003_seq.sql": "INSERT INTO migrate_test_seq (n, ord) VALUES (3, 3);",
		"001_seq.sql": "CREATE TABLE IF NOT EXISTS migrate_test_seq (n INT PRIMARY KEY, ord INT);",
		"002_seq.sql": "INSERT INTO migrate_test_seq (n, ord) VALUES (2, 2);",
	})

	if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto); err != nil {
		// If lex order is wrong, 003 would try to INSERT before the
		// table exists in 001 and we'd error here.
		t.Fatalf("apply: %v", err)
	}

	var n, ord int
	rows, err := pool.Query(ctx, "SELECT n, ord FROM migrate_test_seq ORDER BY ord")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var rowsSeen [][2]int
	for rows.Next() {
		if err := rows.Scan(&n, &ord); err != nil {
			t.Fatal(err)
		}
		rowsSeen = append(rowsSeen, [2]int{n, ord})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration error: %v", err)
	}
	if len(rowsSeen) != 2 {
		t.Fatalf("expected 2 rows (from 002 and 003), got %d: %v", len(rowsSeen), rowsSeen)
	}
	// Verify the inserts landed in apply order — if 003 had run before
	// 002 (or before 001), one would have errored or the rows would be
	// missing. Belt-and-suspenders on top of the fact that out-of-order
	// would have errored at RunMigrations.
	if rowsSeen[0] != [2]int{2, 2} {
		t.Fatalf("first row should be (2,2) from migration 002, got %v", rowsSeen[0])
	}
	if rowsSeen[1] != [2]int{3, 3} {
		t.Fatalf("second row should be (3,3) from migration 003, got %v", rowsSeen[1])
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_seq")
		for _, n := range []string{"001_seq.sql", "002_seq.sql", "003_seq.sql"} {
			_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", n)
		}
	})
}

// TestRunMigrations_ConcurrentInvocations exercises the advisory-lock
// path. Four goroutines call RunMigrations simultaneously against the
// same DB and the same set of pending migrations. The lock should
// serialize them; the result must be exactly one application per file
// (no double-records, no duplicate side effects from a
// not-quite-idempotent migration).
func TestRunMigrations_ConcurrentInvocations(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	const filename = "concurrent_target.sql"
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_concurrent")
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", filename)

	// A non-trivially-idempotent migration: insert a row but no
	// UNIQUE constraint to catch a double-apply. If two runners both
	// pass the NOT EXISTS check and both INSERT, we'd see 2 rows.
	fsys := stubFS(map[string]string{
		filename: `
			CREATE TABLE IF NOT EXISTS migrate_test_concurrent (id TEXT, marker TEXT);
			INSERT INTO migrate_test_concurrent (id, marker)
			SELECT 'sentinel', 'inserted'
			WHERE NOT EXISTS (
				SELECT 1 FROM migrate_test_concurrent WHERE id = 'sentinel'
			);
		`,
	})

	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("RunMigrations from a goroutine: %v", err)
	}

	// Exactly one row recorded in the tracker.
	var trackerCount int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE filename = $1", filename,
	).Scan(&trackerCount); err != nil {
		t.Fatal(err)
	}
	if trackerCount != 1 {
		t.Fatalf("expected tracker count = 1, got %d (lock failed to serialize?)", trackerCount)
	}

	// Exactly one row in the target table — proves the migration body
	// ran exactly once, not four times. Without the advisory lock, the
	// non-atomic NOT EXISTS check would let multiple inserts through.
	var bodyCount int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM migrate_test_concurrent WHERE id = 'sentinel'",
	).Scan(&bodyCount); err != nil {
		t.Fatal(err)
	}
	if bodyCount != 1 {
		t.Fatalf("expected one body insert, got %d — migration ran more than once under concurrency", bodyCount)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_concurrent")
		_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", filename)
	})
}

// TestRunMigrations_NoTransactionDirective verifies that migrations
// with the "-- e2a:no-transaction" directive run on the connection
// directly rather than inside BeginTx. We use VACUUM as the canary
// (illegal in a transaction block, legal outside). VACUUM also acts
// as a sanity check for the single-statement constraint: multi-
// statement scripts get implicitly wrapped server-side under pgx's
// simple protocol, so no-tx migrations must be one statement —
// matching the real-world use case (CREATE INDEX CONCURRENTLY is
// always one statement).
func TestRunMigrations_NoTransactionDirective(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	const filename = "no_tx.sql"
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", filename)

	// VACUUM users — `users` table exists from the testutil's pre-
	// applied 001_init.sql migration. Single statement; would error
	// in a transaction block.
	fsys := stubFS(map[string]string{
		filename: `-- e2a:no-transaction
VACUUM users;`,
	})

	if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto); err != nil {
		t.Fatalf("RunMigrations should succeed with no-transaction directive: %v", err)
	}

	// Recorded in the tracker.
	var count int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE filename = $1", filename,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected tracker count = 1, got %d", count)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", filename)
	})
}

func TestRunMigrations_NoTransactionConcurrentIndexRetryRejectsInvalidIndex(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	const (
		filename = "invalid_concurrent_index.sql"
		table    = "migrate_test_invalid_concurrent"
		index    = "migrate_test_invalid_concurrent_idx"
	)
	_, _ = pool.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+index)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+table)
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", filename)
	if _, err := pool.Exec(ctx,
		`CREATE TABLE `+table+` (value integer NOT NULL);
		 INSERT INTO `+table+` (value) VALUES (1), (1)`,
	); err != nil {
		t.Fatalf("seed duplicate values: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP INDEX CONCURRENTLY IF EXISTS `+index)
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+table)
		_, _ = pool.Exec(context.Background(), "DELETE FROM schema_migrations WHERE filename = $1", filename)
	})

	fsys := stubFS(map[string]string{
		filename: `-- e2a:no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS ` + index + ` ON ` + table + ` (value);`,
	})

	if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto); err == nil {
		t.Fatal("first concurrent unique build unexpectedly succeeded on duplicate values")
	}
	var valid bool
	if err := pool.QueryRow(ctx,
		`SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass`,
		index,
	).Scan(&valid); err != nil {
		t.Fatalf("read interrupted-build artifact: %v", err)
	}
	if valid {
		t.Fatal("failed concurrent build did not leave the expected invalid index")
	}

	err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto)
	if err == nil {
		t.Fatal("retry silently recorded an invalid same-name index")
	}
	if !strings.Contains(err.Error(), index) || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("retry error does not identify invalid index recovery: %v", err)
	}
	var recorded int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE filename = $1`,
		filename,
	).Scan(&recorded); err != nil {
		t.Fatalf("read tracker: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("invalid concurrent index migration recorded %d time(s), want 0", recorded)
	}
}

// TestRunMigrations_NoTransactionDirective_RejectsMultiStatement
// verifies that a migration with the directive AND multiple statements
// fails with a clear, actionable error — rather than the confusing
// "cannot run inside a transaction block" Postgres would otherwise
// emit when its simple protocol implicitly wraps multi-statement
// scripts in a server-side txn.
func TestRunMigrations_NoTransactionDirective_RejectsMultiStatement(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	const filename = "multi_nostmt.sql"
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", filename)

	// Multi-statement script with the directive. Should be refused at
	// runtime; the error must name the migration and the multi-statement
	// problem so the operator can fix it without diving into pg internals.
	fsys := stubFS(map[string]string{
		filename: `-- e2a:no-transaction
SELECT 1;
SELECT 2;`,
	})

	err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto)
	if err == nil {
		t.Fatal("expected error: multi-statement no-transaction migration should be refused")
	}
	if !strings.Contains(err.Error(), "multiple statements") {
		t.Fatalf("error should mention multi-statement issue, got: %v", err)
	}
	if !strings.Contains(err.Error(), filename) {
		t.Fatalf("error should name the migration, got: %v", err)
	}

	// Nothing recorded — the runner refused before any SQL ran.
	var count int
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE filename = $1", filename,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("refused migration should not be recorded, got count=%d", count)
	}
}

// TestRunMigrations_NoTransactionDirective_AcceptsSemicolonInComment
// makes sure the multi-statement detector ignores semicolons inside
// comments and string literals.
func TestRunMigrations_NoTransactionDirective_AcceptsSemicolonInComment(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	const filename = "tricky_nostmt.sql"
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", filename)

	// Single SQL statement, but with ';' appearing inside both a
	// comment and a string literal — must not trip the detector.
	fsys := stubFS(map[string]string{
		filename: `-- e2a:no-transaction
-- This comment has a semicolon; see?
SELECT 'literal with ; inside' FROM users LIMIT 1;`,
	})

	if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto); err != nil {
		t.Fatalf("single-statement migration with semicolons in comments/strings should succeed: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", filename)
	})
}

// TestRunMigrations_NoTransactionDirective_RejectsInsideTx is the
// negative: the same VACUUM without the directive must fail,
// confirming the directive is what makes it work.
func TestRunMigrations_NoTransactionDirective_RejectsInsideTx(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	const filename = "wraps_vacuum.sql"
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", filename)

	fsys := stubFS(map[string]string{
		filename: `VACUUM users;`,
	})

	err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto)
	if err == nil {
		t.Fatal("expected error: VACUUM inside transaction should fail")
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", filename)
	})
}

// TestRunMigrations_OrphanedTrackerRecord exercises the orphan-warning
// path: a filename recorded in schema_migrations that no longer exists
// in the FS should produce a WARN log but NOT fail the run.
func TestRunMigrations_OrphanedTrackerRecord(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	const orphan = "999_removed_migration.sql"
	_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", orphan)

	// Ensure schema_migrations exists, then plant the orphan.
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations (filename) VALUES ($1) ON CONFLICT DO NOTHING", orphan); err != nil {
		t.Fatal(err)
	}

	// Empty FS — every recorded migration is now an orphan.
	fsys := stubFS(map[string]string{})

	// Should NOT error — orphans are warnings, not failures.
	if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto); err != nil {
		t.Fatalf("orphan record should not fail RunMigrations: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", orphan)
	})
}

// TestRunMigrations_PartialState verifies that when some migrations are
// already in schema_migrations, only the pending ones are applied.
func TestRunMigrations_PartialState(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_partial_a, migrate_test_partial_b")
	for _, n := range []string{"a.sql", "b.sql"} {
		_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", n)
	}

	// Pretend a.sql was applied externally (e.g., the testutil path).
	if _, err := pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS migrate_test_partial_a (id TEXT)"); err != nil {
		t.Fatal(err)
	}
	// Create schema_migrations and record a.sql so the runner skips it.
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations (filename) VALUES ('a.sql')"); err != nil {
		t.Fatal(err)
	}

	fsys := stubFS(map[string]string{
		"a.sql": "SELECT 1/0;", // would error if it ran; should be skipped
		"b.sql": "CREATE TABLE IF NOT EXISTS migrate_test_partial_b (id TEXT);",
	})

	if err := identity.RunMigrations(ctx, pool, fsys, identity.ModeAuto); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	var ok bool
	if err := pool.QueryRow(ctx,
		"SELECT to_regclass('public.migrate_test_partial_b') IS NOT NULL",
	).Scan(&ok); err != nil || !ok {
		t.Fatalf("b should be applied: ok=%v err=%v", ok, err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS migrate_test_partial_a, migrate_test_partial_b")
		for _, n := range []string{"a.sql", "b.sql"} {
			_, _ = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = $1", n)
		}
	})
}
