package identity

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationMode controls how RunMigrations handles pending migrations.
//
// Set via E2A_MIGRATION_MODE env var. Default is ModeAuto.
//
//   - ModeAuto: apply pending migrations in order. Fail (return error)
//     if any apply errors. This is what the hosted deployment uses.
//   - ModeVerify: do not apply anything. Return an error listing pending
//     migrations. For cautious operators who want a separate manual
//     migration step before binary rollout.
//   - ModeSkip: do not apply or check. Log and proceed. For emergency
//     surgery — deploy a binary that won't touch schema.
type MigrationMode string

const (
	ModeAuto   MigrationMode = "auto"
	ModeVerify MigrationMode = "verify"
	ModeSkip   MigrationMode = "skip"
)

// ParseMigrationMode reads a string (typically from env) and returns the
// matching mode, defaulting to ModeAuto on empty input. Returns an error
// for unknown values so a typo is loud, not silent.
func ParseMigrationMode(s string) (MigrationMode, error) {
	switch s {
	case "", string(ModeAuto):
		return ModeAuto, nil
	case string(ModeVerify):
		return ModeVerify, nil
	case string(ModeSkip):
		return ModeSkip, nil
	default:
		return "", fmt.Errorf("invalid migration mode %q (want auto|verify|skip)", s)
	}
}

// schemaMigrationsDDL is bootstrapped by the runner before reading state.
// It can't live in migrations/ itself — chicken-and-egg.
const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    filename    TEXT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`

// e2aMigrationLockID is the Postgres advisory-lock key reserved for
// serializing schema migrations across concurrent binary instances.
// The compose deploy pattern (`docker compose up -d` with
// `restart: always` and no health-conditioned depends_on) routinely
// runs two containers simultaneously during a rolling restart; without
// this lock both would race into the same migration set. Idempotent
// migrations would mostly absorb the race, but a backfill like
// `INSERT ... WHERE NOT EXISTS` at READ COMMITTED is NOT race-free —
// two transactions can both see NOT EXISTS and double-insert.
//
// Value is an arbitrary stable int64; the only requirement is that all
// instances connecting to the same database use the same constant.
// Hex bytes spell "e2_MIGR\x00" — easy to grep, won't collide with
// random libraries.
const e2aMigrationLockID int64 = 0x65325F4D49475200

// noTransactionDirective marks a migration that must NOT run inside a
// transaction (Postgres rejects e.g. CREATE INDEX CONCURRENTLY or
// REINDEX CONCURRENTLY in a txn block). The runner scans leading
// comment lines for this string and bypasses the BeginTx wrapper.
//
// IMPORTANT: such migrations MUST be a single SQL statement. Multi-
// statement scripts sent through pgx's simple protocol get wrapped in
// an implicit transaction server-side, defeating the purpose. If you
// need a CREATE INDEX CONCURRENTLY plus surrounding setup, split them
// into two numbered files: one transactional (the setup), one with
// this directive (the CONCURRENTLY statement).
const noTransactionDirective = "e2a:no-transaction"

// migrationFilenameAliases preserves upgrade and rollback compatibility for
// migrations that were merged with colliding numeric slots. The map is
// current filename -> legacy filename. Existing databases keep the legacy
// tracker row and treat the current filename as applied in memory; fresh
// databases record both names so an older binary does not replay the legacy
// file after rollback.
var migrationFilenameAliases = map[string]string{
	"112_sending_protection_policy.sql":               "109_sending_protection_policy.sql",
	"113_sending_budget_ledger.sql":                   "110_sending_budget_ledger.sql",
	"114_sending_feedback_provenance.sql":             "111_sending_feedback_provenance.sql",
	"115_sending_controls_foreign_key.sql":            "112_sending_controls_foreign_key.sql",
	"116_sending_message_holds.sql":                   "113_sending_message_holds.sql",
	"117_sending_suppression_provenance.sql":          "114_sending_suppression_provenance.sql",
	"118_sending_protection_validate_constraints.sql": "115_sending_protection_validate_constraints.sql",
}

var (
	createConcurrentIndexMarker = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\b`)
	createConcurrentIndexTarget = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\s+(?:IF\s+NOT\s+EXISTS\s+)?((?:"(?:[^"]|"")*"|[a-z_][a-z0-9_$]*)(?:\.(?:"(?:[^"]|"")*"|[a-z_][a-z0-9_$]*))?)`)
)

// acquireConnTimeout caps how long the runner will wait for a pooled
// connection to be available before bailing out. Hit during rolling
// restarts when the old container is still holding pgxpool.MaxConns
// connections (queries, autovacuum, idle); without this bound the new
// binary would block in pool.Acquire forever with no log line.
const acquireConnTimeout = 60 * time.Second

// RunMigrations applies every embedded migration that isn't yet recorded
// in the schema_migrations table. Migrations run in filename-sorted
// order, each in its own transaction (unless tagged with the
// "e2a:no-transaction" directive); on the first error the function
// returns without applying later migrations.
//
// A Postgres session advisory lock serializes concurrent invocations
// from rolling restarts and multi-instance deploys. The lock is held
// for the duration of the function and auto-released on session
// disconnect (so a crashed binary doesn't leave the DB stuck).
//
// All migrations should be written idempotent (CREATE/ALTER ... IF NOT
// EXISTS) so even-without-the-lock re-runs are harmless. The tracker
// is the source of truth for "should we attempt to run this one
// again"; idempotence + the lock are layered safety nets.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool, migrationsFS fs.FS, mode MigrationMode) error {
	if mode == ModeSkip {
		log.Printf("[migrate] mode=skip — not running migrations (operator override)")
		return nil
	}

	// Acquire a dedicated connection: pg_advisory_lock is session-scoped,
	// so the holder of the lock must be the same backend until release.
	// Bounded by acquireConnTimeout — without it, a rolling deploy could
	// hang forever if the previous instance still holds every conn.
	acqCtx, cancel := context.WithTimeout(ctx, acquireConnTimeout)
	defer cancel()
	conn, err := pool.Acquire(acqCtx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration within %s: %w (previous instance may still be holding connections)", acquireConnTimeout, err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", e2aMigrationLockID); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	log.Printf("[migrate] holding advisory lock %d (e2a-migrations namespace)", e2aMigrationLockID)
	defer func() {
		// Use a fresh context for unlock: if ctx is cancelled by the time
		// we get here, we still want to release the lock cleanly. The
		// session would auto-release on disconnect anyway, but explicit
		// unlock is hygiene.
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(releaseCtx, "SELECT pg_advisory_unlock($1)", e2aMigrationLockID)
	}()

	if _, err := conn.Exec(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("create schema_migrations table: %w (does this DB user have CREATE privileges?)", err)
	}

	files, err := listMigrations(migrationsFS)
	if err != nil {
		return err
	}

	applied, err := loadApplied(ctx, conn)
	if err != nil {
		return fmt.Errorf("load applied migrations: %w", err)
	}
	compatibilityRepairs := migrationFilenameCompatibilityRepairs(applied, files)
	recognizeMigrationFilenameAliases(applied, files)

	// Defensive: warn about migrations recorded in schema_migrations
	// that no longer exist in the embedded FS (rename/delete since
	// they were applied). This is exactly the silent-drift class we're
	// trying to prevent; logging it surfaces the drift to operators.
	warnOrphans(applied, files)

	pending := make([]string, 0, len(files))
	for _, f := range files {
		if !applied[f] {
			pending = append(pending, f)
		}
	}
	if mode == ModeVerify && len(compatibilityRepairs) > 0 {
		return fmt.Errorf(
			"verify mode: rollback compatibility repair pending for %s — set E2A_MIGRATION_MODE=auto to record the legacy filename marker(s) without replaying migration SQL",
			strings.Join(compatibilityRepairs, ", "),
		)
	}
	if len(compatibilityRepairs) > 0 {
		if err := recordMigrationFilenames(ctx, conn, compatibilityRepairs); err != nil {
			return fmt.Errorf("repair rollback compatibility migration markers: %w", err)
		}
		for _, legacy := range compatibilityRepairs {
			applied[legacy] = true
		}
		log.Printf("[migrate] repaired %d rollback compatibility migration marker(s)", len(compatibilityRepairs))
	}

	if len(pending) == 0 {
		log.Printf("[migrate] all %d migrations applied", len(files))
		return nil
	}

	if mode == ModeVerify {
		return fmt.Errorf(
			"verify mode: %d pending migration(s): %s — set E2A_MIGRATION_MODE=auto to apply or run them manually",
			len(pending), strings.Join(pending, ", "),
		)
	}

	start := time.Now()
	for _, name := range pending {
		migStart := time.Now()
		if err := applyOne(ctx, conn, migrationsFS, name); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		log.Printf("[migrate] applied %s in %s", name, time.Since(migStart).Round(time.Millisecond))
	}
	log.Printf("[migrate] applied %d new migration(s) (%d total) in %s",
		len(pending), len(files), time.Since(start).Round(time.Millisecond))
	return nil
}

// listMigrations returns every *.sql filename in fsys's root, sorted
// lexicographically. The numeric prefix in our naming scheme makes the
// lexicographic order the apply order.
func listMigrations(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

func loadApplied(ctx context.Context, conn *pgxpool.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT filename FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = true
	}
	return applied, rows.Err()
}

// warnOrphans logs a warning for every filename recorded in
// schema_migrations that's missing from the current FS — a rename or
// deletion across versions. We don't fail because a legitimate
// squash-migration workflow could intentionally retire old files; the
// warning surfaces drift to operators who can investigate.
func warnOrphans(applied map[string]bool, files []string) {
	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		fileSet[f] = true
	}
	for name := range applied {
		if current, ok := currentMigrationFilenameForLegacy(name); ok && fileSet[current] {
			continue
		}
		if !fileSet[name] {
			log.Printf("[migrate] WARN: %s recorded in schema_migrations but not in embedded migrations/ — possible rename or deletion", name)
		}
	}
}

func recognizeMigrationFilenameAliases(applied map[string]bool, files []string) {
	fileSet := make(map[string]bool, len(files))
	for _, name := range files {
		fileSet[name] = true
	}
	for current, legacy := range migrationFilenameAliases {
		if fileSet[current] && applied[legacy] {
			applied[current] = true
		}
	}
}

// migrationFilenameCompatibilityRepairs returns legacy tracker names that are
// missing even though their current aliases are already recorded. The current
// record proves the migration body ran; inserting only the legacy marker makes
// a rollback binary skip the same body without replaying any SQL.
func migrationFilenameCompatibilityRepairs(applied map[string]bool, files []string) []string {
	repairs := make([]string, 0, len(migrationFilenameAliases))
	for _, current := range files {
		legacy, ok := migrationFilenameAliases[current]
		if ok && applied[current] && !applied[legacy] {
			repairs = append(repairs, legacy)
		}
	}
	return repairs
}

func currentMigrationFilenameForLegacy(legacy string) (string, bool) {
	for current, candidate := range migrationFilenameAliases {
		if candidate == legacy {
			return current, true
		}
	}
	return "", false
}

// EquivalentMigrationFilenames returns every tracker filename that proves the
// given current migration has run. The current name is always first; a legacy
// alias follows when a previously released binary used a different filename.
// Callers receive a new slice and may modify it safely.
func EquivalentMigrationFilenames(current string) []string {
	legacy, ok := migrationFilenameAliases[current]
	if !ok {
		return []string{current}
	}
	return []string{current, legacy}
}

func recordMigrationFilenames(ctx context.Context, conn *pgxpool.Conn, filenames []string) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tracker tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := recordMigrationFilenamesTx(ctx, tx, filenames); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tracker tx: %w", err)
	}
	return nil
}

func recordMigrationFilenamesTx(ctx context.Context, tx pgx.Tx, filenames []string) error {
	for _, filename := range filenames {
		if _, err := tx.Exec(ctx,
			"INSERT INTO schema_migrations (filename) VALUES ($1) ON CONFLICT DO NOTHING",
			filename,
		); err != nil {
			return fmt.Errorf("record migration %s: %w", filename, err)
		}
	}
	return nil
}

// hasNoTransactionDirective scans the leading comment block of a
// migration body for the "e2a:no-transaction" marker. Only inspects
// the first few lines so an in-body reference (e.g. inside a string
// literal) doesn't trigger.
func hasNoTransactionDirective(body string) bool {
	for i, line := range strings.SplitN(body, "\n", 6) {
		if i >= 5 {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "--") {
			return false // first non-comment line — stop scanning
		}
		if strings.Contains(trimmed, noTransactionDirective) {
			return true
		}
	}
	return false
}

// looksMultiStatement returns true if body appears to contain more
// than one SQL statement. Not a full parser — handles the common
// shapes (line comments, block comments, single-quoted strings) and
// catches the case the no-transaction directive is most likely to
// trip on: a user writing `SET something; CREATE INDEX CONCURRENTLY …`
// and getting a confusing Postgres error about txn blocks.
//
// Doesn't handle dollar-quoted strings or PL/pgSQL function bodies;
// those would need a real parser. The directive isn't intended for
// those cases anyway — it's for single CONCURRENTLY-class statements.
func looksMultiStatement(body string) bool {
	stripped := stripSQLCommentsAndStrings(body)
	// A single statement (with optional trailing whitespace + semicolon)
	// has zero ';' after we trim. A multi-statement script has 1+.
	stripped = strings.TrimRight(stripped, "; \t\n\r")
	return strings.Contains(stripped, ";")
}

// stripSQLCommentsAndStrings removes -- line comments, /* */ block
// comments, and '...' single-quoted strings from the body. Used by
// looksMultiStatement so semicolons inside comments / strings don't
// false-positive the multi-statement check.
func stripSQLCommentsAndStrings(body string) string {
	var b strings.Builder
	i := 0
	for i < len(body) {
		// -- line comment
		if i+1 < len(body) && body[i] == '-' && body[i+1] == '-' {
			for i < len(body) && body[i] != '\n' {
				i++
			}
			continue
		}
		// /* block comment */
		if i+1 < len(body) && body[i] == '/' && body[i+1] == '*' {
			i += 2
			for i+1 < len(body) && !(body[i] == '*' && body[i+1] == '/') {
				i++
			}
			if i+1 < len(body) {
				i += 2 // consume the closing */
			}
			continue
		}
		// '...' single-quoted string (Postgres uses '' for escaped quotes,
		// not \'; tolerate both since these are migrations not adversarial)
		if body[i] == '\'' {
			i++
			for i < len(body) && body[i] != '\'' {
				if body[i] == '\\' && i+1 < len(body) {
					i += 2
					continue
				}
				i++
			}
			if i < len(body) {
				i++ // consume closing '
			}
			continue
		}
		b.WriteByte(body[i])
		i++
	}
	return b.String()
}

// validateConcurrentIndexPostcondition closes the non-atomic retry gap of
// CREATE INDEX CONCURRENTLY. PostgreSQL can leave an INVALID same-name index
// after an interrupted build; a retry with IF NOT EXISTS then returns success.
// Never record that migration as applied unless its exact target exists and is
// both ready and valid.
func validateConcurrentIndexPostcondition(ctx context.Context, conn *pgxpool.Conn, body string) error {
	stripped := stripSQLCommentsAndStrings(body)
	if !createConcurrentIndexMarker.MatchString(stripped) {
		return nil
	}
	match := createConcurrentIndexTarget.FindStringSubmatch(stripped)
	if len(match) != 2 {
		return fmt.Errorf("could not identify CREATE INDEX CONCURRENTLY target")
	}
	target := match[1]
	var exists, valid, ready bool
	if err := conn.QueryRow(ctx,
		`SELECT target.oid IS NOT NULL,
		        COALESCE(idx.indisvalid, false),
		        COALESCE(idx.indisready, false)
		   FROM (SELECT to_regclass($1) AS oid) AS target
		   LEFT JOIN pg_index AS idx ON idx.indexrelid = target.oid`,
		target,
	).Scan(&exists, &valid, &ready); err != nil {
		return fmt.Errorf("verify concurrent index %s: %w", target, err)
	}
	if !exists || !valid || !ready {
		return fmt.Errorf(
			"concurrent index %s is missing or invalid (exists=%t indisvalid=%t indisready=%t); DROP INDEX CONCURRENTLY IF EXISTS %s and retry",
			target, exists, valid, ready, target,
		)
	}
	return nil
}

// applyOne reads a migration file and applies it.
//
// For migrations without the no-transaction directive: SQL + tracker
// insert run in a single transaction so a partial-failure migration is
// safe to retry. Tx isolation defaults to READ COMMITTED, which is
// correct here (we don't want SERIALIZABLE forcing retries on every
// concurrent backfill — we already hold the migration advisory lock).
//
// For migrations tagged with the no-transaction directive (e.g.
// CREATE INDEX CONCURRENTLY): SQL runs directly on the connection,
// then the tracker insert in a separate small transaction. This
// breaks the "both apply atomically" invariant for the migration
// itself; the directive is opt-in for the specific Postgres features
// that require it.
func applyOne(ctx context.Context, conn *pgxpool.Conn, fsys fs.FS, name string) error {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}

	if hasNoTransactionDirective(string(body)) {
		// pgx's simple protocol sends multi-statement strings to the
		// server as a single Query message, which Postgres wraps in an
		// implicit transaction — defeating the directive's purpose and
		// surfacing as a confusing "cannot run inside a transaction
		// block" error from the actual offending statement. Catch it
		// here with a clear message.
		if looksMultiStatement(string(body)) {
			return fmt.Errorf("migration %s uses -- e2a:no-transaction but contains multiple statements; split into separate files (one per statement)", name)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("exec migration (no-transaction): %w", err)
		}
		if err := validateConcurrentIndexPostcondition(ctx, conn, string(body)); err != nil {
			return fmt.Errorf("verify migration %s: %w", name, err)
		}
		return recordMigrationFilenames(ctx, conn, EquivalentMigrationFilenames(name))
	}

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("exec migration: %w", err)
	}
	if err := recordMigrationFilenamesTx(ctx, tx, EquivalentMigrationFilenames(name)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
