package identity_test

import (
	"context"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/migrations"
)

func TestEmbeddedMigrationNumbersAreUniqueFrom108(t *testing.T) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}

	seen := make(map[int]string, len(entries))
	lastNumber := 107
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		if len(name) < 5 || name[3] != '_' {
			t.Fatalf("migration %q must start with a three-digit numeric slot", name)
		}
		number, err := strconv.Atoi(name[:3])
		if err != nil {
			t.Fatalf("migration %q has invalid numeric slot: %v", name, err)
		}
		// Historical migrations contain intentionally retained duplicate slots.
		// Enforce uniqueness for the current train and every future migration.
		if number < 108 {
			continue
		}
		if previous, ok := seen[number]; ok {
			t.Fatalf("migration slot %03d is used by both %q and %q", number, previous, name)
		}
		if number <= lastNumber {
			t.Fatalf("migration %q slot %03d is not after slot %03d", name, number, lastNumber)
		}
		seen[number] = name
		lastNumber = number
	}
}

var renamedB1Migrations = []struct {
	legacyName  string
	currentName string
}{
	{"109_sending_protection_policy.sql", "112_sending_protection_policy.sql"},
	{"110_sending_budget_ledger.sql", "113_sending_budget_ledger.sql"},
	{"111_sending_feedback_provenance.sql", "114_sending_feedback_provenance.sql"},
	{"112_sending_controls_foreign_key.sql", "115_sending_controls_foreign_key.sql"},
	{"113_sending_message_holds.sql", "116_sending_message_holds.sql"},
	{"114_sending_suppression_provenance.sql", "117_sending_suppression_provenance.sql"},
	{"115_sending_protection_validate_constraints.sql", "118_sending_protection_validate_constraints.sql"},
}

func TestRunMigrationsRecognizesRenamedB1Migrations(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	expectedNames := make([]string, 0, len(renamedB1Migrations)*2)
	fsEntries := make(map[string]string, len(renamedB1Migrations))
	for _, migration := range renamedB1Migrations {
		expectedNames = append(expectedNames, migration.legacyName, migration.currentName)
		fsEntries[migration.currentName] = "THIS MUST NOT EXECUTE;"
	}
	restoreMigrationRecords(t, pool, expectedNames)
	for _, migration := range renamedB1Migrations {
		if _, err := pool.Exec(ctx,
			"DELETE FROM schema_migrations WHERE filename = $1",
			migration.currentName,
		); err != nil {
			t.Fatalf("clear current migration record %s: %v", migration.currentName, err)
		}
		if _, err := pool.Exec(ctx,
			"INSERT INTO schema_migrations (filename) VALUES ($1) ON CONFLICT DO NOTHING",
			migration.legacyName,
		); err != nil {
			t.Fatalf("seed legacy migration record %s: %v", migration.legacyName, err)
		}
	}

	// A database upgraded through v1.8.2 already ran B1 under the legacy
	// filenames. The collision-free names must be treated as applied without
	// replaying any migration body.
	if err := identity.RunMigrations(ctx, pool, stubFS(fsEntries), identity.ModeAuto); err != nil {
		t.Fatalf("renamed B1 migrations should be skipped from legacy records: %v", err)
	}
	if err := identity.RunMigrations(ctx, pool, stubFS(fsEntries), identity.ModeVerify); err != nil {
		t.Fatalf("verify mode should recognize renamed B1 migrations: %v", err)
	}

	for _, migration := range renamedB1Migrations {
		var currentCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM schema_migrations WHERE filename = $1",
			migration.currentName,
		).Scan(&currentCount); err != nil {
			t.Fatalf("read current migration record %s: %v", migration.currentName, err)
		}
		if currentCount != 0 {
			t.Fatalf("upgrade compatibility must not fabricate %s record, got %d", migration.currentName, currentCount)
		}
	}
}

func TestFreshRenamedMigrationRecordsLegacyFilenameForRollback(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)

	expectedNames := make([]string, 0, len(renamedB1Migrations)*2)
	fsEntries := make(map[string]string, len(renamedB1Migrations))
	for _, migration := range renamedB1Migrations {
		expectedNames = append(expectedNames, migration.legacyName, migration.currentName)
		fsEntries[migration.currentName] = "SELECT 1;"
	}
	restoreMigrationRecords(t, pool, expectedNames)
	if _, err := pool.Exec(ctx,
		"DELETE FROM schema_migrations WHERE filename = ANY($1)",
		expectedNames,
	); err != nil {
		t.Fatalf("clear migration records: %v", err)
	}

	if err := identity.RunMigrations(ctx, pool, stubFS(fsEntries), identity.ModeAuto); err != nil {
		t.Fatalf("apply renamed B1 migration: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT filename
		FROM schema_migrations
		WHERE filename = ANY($1)
		ORDER BY filename
	`, expectedNames)
	if err != nil {
		t.Fatalf("read migration compatibility records: %v", err)
	}
	defer rows.Close()
	var recordedNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan migration compatibility record: %v", err)
		}
		recordedNames = append(recordedNames, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migration compatibility records: %v", err)
	}
	if len(recordedNames) != len(renamedB1Migrations)*2 {
		t.Fatalf("migration records = %v, want all legacy and current filenames", recordedNames)
	}
	seen := make(map[string]bool, len(recordedNames))
	for _, name := range recordedNames {
		seen[name] = true
	}
	for _, migration := range renamedB1Migrations {
		if !seen[migration.legacyName] || !seen[migration.currentName] {
			t.Fatalf("migration records = %v, missing rollback pair %s / %s", recordedNames, migration.legacyName, migration.currentName)
		}
	}

	legacyEntries := make(map[string]string, len(renamedB1Migrations))
	for _, migration := range renamedB1Migrations {
		legacyEntries[migration.legacyName] = "THIS MUST NOT EXECUTE;"
	}
	if err := identity.RunMigrations(ctx, pool, stubFS(legacyEntries), identity.ModeVerify); err != nil {
		t.Fatalf("legacy binary view should see rollback filenames applied: %v", err)
	}
}

func TestMigrationsRewriteLegacySendingJobsWithImmutableAttribution(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate River schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM river_job WHERE kind IN ('outbound_send','hitl_notify','webhook_notify');
			DELETE FROM sending_provider_operations WHERE source_account_ref='usr_legacy_jobs';
			DELETE FROM messages WHERE agent_id='agent_legacy_jobs';
			ALTER TABLE messages DROP CONSTRAINT IF EXISTS messages_sent_as_check;
			ALTER TABLE messages ADD CONSTRAINT messages_sent_as_check
			    CHECK (sent_as IS NULL OR sent_as IN ('own_address','relay'));
		`)
	})
	if _, err := pool.Exec(ctx, `
		DELETE FROM river_job WHERE kind IN ('outbound_send','hitl_notify','webhook_notify');
		DELETE FROM sending_provider_operations WHERE source_account_ref='usr_legacy_jobs';
		DELETE FROM messages WHERE agent_id='agent_legacy_jobs';
		ALTER TABLE messages DROP CONSTRAINT IF EXISTS messages_sent_as_check;
	`); err != nil {
		t.Fatalf("clear River fixtures: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id,email,name,google_subject) VALUES ('usr_legacy_jobs','owner@legacy-jobs.test','Owner','sub-legacy-jobs');
		INSERT INTO domains (domain,user_id,verified) VALUES ('legacy-jobs.test','usr_legacy_jobs',true);
		INSERT INTO agent_identities (id,registered_domain,user_id,name) VALUES ('agent_legacy_jobs','legacy-jobs.test','usr_legacy_jobs','Legacy');
		INSERT INTO messages (id,agent_id,direction,sender,recipient,subject,delivery_status,sent_as,scheduled_at) VALUES
		  ('msg_legacy_outbound','agent_legacy_jobs','outbound','sender@legacy-jobs.test','recipient@example.test','Outbound','accepted','relay',NULL),
		  ('msg_legacy_outbound_own','agent_legacy_jobs','outbound','sender@legacy-jobs.test','recipient@example.test','Outbound own identity','accepted','own_address',NULL),
		  ('msg_legacy_scheduled','agent_legacy_jobs','outbound','sender@legacy-jobs.test','recipient@example.test','Scheduled','accepted','relay',transaction_timestamp()+interval '90 days'),
		  ('msg_legacy_hitl','agent_legacy_jobs','outbound','sender@legacy-jobs.test','recipient@example.test','HITL','accepted','own_address',NULL),
		  ('msg_legacy_null_sent_as','agent_legacy_jobs','outbound','sender@legacy-jobs.test','recipient@example.test','NULL sent_as','accepted',NULL,NULL),
		  ('msg_legacy_invalid_sent_as','agent_legacy_jobs','outbound','sender@legacy-jobs.test','recipient@example.test','Invalid sent_as','accepted','unexpected',NULL),
		  ('msg_legacy_inbound','agent_legacy_jobs','inbound','sender@example.test','recipient@legacy-jobs.test','Inbound mismatch','accepted','relay',NULL);
		INSERT INTO webhooks (id,user_id,url,description,events,filters,signing_secret) VALUES
		  ('wh_legacy_jobs','usr_legacy_jobs','https://hooks.example.test/health','Legacy health',ARRAY['email.received'],'{}','synthetic-secret');
	`); err != nil {
		t.Fatalf("seed legacy sources: %v", err)
	}
	var scheduledAt time.Time
	if err := pool.QueryRow(ctx, `SELECT scheduled_at FROM messages WHERE id='msg_legacy_scheduled'`).Scan(&scheduledAt); err != nil {
		t.Fatalf("read scheduled fixture time: %v", err)
	}

	type fixture struct {
		kind string
		args string
	}
	fixtures := []fixture{
		{"outbound_send", `{"message_id":"msg_legacy_outbound"}`},
		{"outbound_send", `{"message_id":"msg_legacy_outbound_own"}`},
		{"outbound_send", `{"message_id":"msg_legacy_scheduled"}`},
		{"hitl_notify", `{"message_id":"msg_legacy_hitl"}`},
		{"webhook_notify", `{"webhook_id":"wh_legacy_jobs","kind":"warning"}`},
		{"outbound_send", `{"message_id":"msg_orphan_legacy"}`},
		{"webhook_notify", `{}`},
		{"outbound_send", `{"message_id":"msg_legacy_null_sent_as"}`},
		{"outbound_send", `{"message_id":"msg_legacy_invalid_sent_as"}`},
		{"outbound_send", `{"message_id":"msg_legacy_inbound"}`},
	}
	jobIDs := make([]int64, 0, len(fixtures))
	for _, f := range fixtures {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO river_job (args,kind,max_attempts) VALUES ($1::jsonb,$2,3) RETURNING id`, f.args, f.kind).Scan(&id); err != nil {
			t.Fatalf("insert %s legacy job: %v", f.kind, err)
		}
		jobIDs = append(jobIDs, id)
	}
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state='scheduled',scheduled_at=$2 WHERE id=$1`, jobIDs[2], scheduledAt); err != nil {
		t.Fatalf("schedule legacy job: %v", err)
	}

	migration, err := migrations.FS.ReadFile("113_sending_budget_ledger.sql")
	if err != nil {
		t.Fatalf("read sending budget migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply sending budget migration to legacy fixtures: %v", err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("reapply sending budget migration: %v", err)
	}

	for i, want := range []struct {
		purpose string
		shared  bool
		source  string
	}{
		{"customer_message", true, "message_id"},
		{"customer_message", false, "message_id"},
		{"customer_message", true, "message_id"},
		{"customer_notification", true, "message_id"},
		{"customer_notification", true, "webhook_id"},
	} {
		var operationID, purpose, sourceAccount, policySubject, sourceValue string
		var shared bool
		var expiresAt time.Time
		if err := pool.QueryRow(ctx, `
			SELECT j.args->'operation_ref'->>'id',o.purpose,o.shared_reputation,
			       o.source_account_ref,o.policy_subject_ref,j.args->>$2,o.expires_at
			FROM river_job j JOIN sending_provider_operations o ON o.operation_id=j.args->'operation_ref'->>'id'
			WHERE j.id=$1`, jobIDs[i], want.source,
		).Scan(&operationID, &purpose, &shared, &sourceAccount, &policySubject, &sourceValue, &expiresAt); err != nil {
			t.Fatalf("read rewritten job %d: %v", i, err)
		}
		if operationID == "" || purpose != want.purpose || shared != want.shared || sourceAccount != "usr_legacy_jobs" || policySubject != "usr_legacy_jobs" || sourceValue == "" {
			t.Fatalf("rewritten job %d = op=%q purpose=%q shared=%v source=%q subject=%q legacy=%q", i, operationID, purpose, shared, sourceAccount, policySubject, sourceValue)
		}
		if i < 3 && operationID != sourceValue {
			t.Fatalf("outbound operation id=%q, want retained message id %q", operationID, sourceValue)
		}
		if i == 2 && expiresAt.Before(scheduledAt.Add(30*24*time.Hour)) {
			t.Errorf("scheduled operation expires_at=%s, want at least scheduled_at+30d=%s", expiresAt, scheduledAt.Add(30*24*time.Hour))
		}
	}

	for i, jobID := range jobIDs[5:] {
		var unavailableState string
		var unavailableFinalized bool
		var unavailableHasRef bool
		if err := pool.QueryRow(ctx, `SELECT state::text,finalized_at IS NOT NULL,args ? 'operation_ref' FROM river_job WHERE id=$1`, jobID).Scan(&unavailableState, &unavailableFinalized, &unavailableHasRef); err != nil {
			t.Fatalf("read unavailable disposition %d: %v", i, err)
		}
		if unavailableState != "discarded" || !unavailableFinalized || unavailableHasRef {
			t.Errorf("unavailable disposition %d state=%q finalized=%v operation_ref=%v", i, unavailableState, unavailableFinalized, unavailableHasRef)
		}
	}

	var operations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_provider_operations WHERE source_account_ref='usr_legacy_jobs'`).Scan(&operations); err != nil || operations != 5 {
		t.Fatalf("legacy operation count=%d err=%v", operations, err)
	}
	var controls int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_sending_controls WHERE user_id='usr_legacy_jobs' AND state='active' AND outcome_epoch=1`).Scan(&controls); err != nil || controls != 1 {
		t.Fatalf("bootstrapped account controls=%d err=%v", controls, err)
	}
}

func TestSendingBudgetMigrationDoesNotDeadlockSourceFirstJobCancellation(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate River schema: %v", err)
	}
	client, err := jobs.New(pool, jobs.Config{})
	if err != nil {
		t.Fatalf("build real River cancellation client: %v", err)
	}
	migration, err := migrations.FS.ReadFile("113_sending_budget_ledger.sql")
	if err != nil {
		t.Fatal(err)
	}
	cleanupData := func() {
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM river_job
			WHERE kind IN ('outbound_send','hitl_notify','webhook_notify');
			DELETE FROM sending_provider_operations
			WHERE source_account_ref='usr_source_first_race';
			DELETE FROM users WHERE id='usr_source_first_race';
		`)
	}
	cleanupData()
	if _, err := pool.Exec(ctx, `DROP TABLE account_sending_controls`); err != nil {
		t.Fatalf("remove controls/FK for true first-apply race: %v", err)
	}
	t.Cleanup(func() {
		cleanupData()
		if _, err := pool.Exec(context.Background(), string(migration)); err != nil {
			t.Errorf("restore budget migration after source-first race: %v", err)
		}
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id,email,name,google_subject)
		VALUES ('usr_source_first_race','owner@source-first-race.test','Owner','sub-source-first-race');
		INSERT INTO domains (domain,user_id,verified)
		VALUES ('source-first-race.test','usr_source_first_race',true);
		INSERT INTO agent_identities (id,registered_domain,user_id,name)
		VALUES ('agent_source_first_race','source-first-race.test','usr_source_first_race','Race');
		INSERT INTO messages
		    (id,agent_id,direction,sender,recipient,subject,delivery_status,sent_as)
		VALUES ('msg_source_first_race','agent_source_first_race','outbound',
		        'sender@source-first-race.test','recipient@example.test','Source first',
		        'accepted','relay');
	`); err != nil {
		t.Fatalf("seed source-first race: %v", err)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO river_job (args,kind,max_attempts)
		VALUES ('{"message_id":"msg_source_first_race"}'::jsonb,'outbound_send',3)
		RETURNING id
	`).Scan(&jobID); err != nil {
		t.Fatalf("insert source-first River job: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE messages SET send_job_id=$2 WHERE id=$1
	`, "msg_source_first_race", jobID); err != nil {
		t.Fatalf("link source-first River job: %v", err)
	}

	deletionConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deletionConn.Release()
	deletionTx, err := deletionConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deletionOpen := true
	defer func() {
		if deletionOpen {
			_ = deletionTx.Rollback(context.Background())
		}
	}()
	if _, err := deletionTx.Exec(ctx, `
		SELECT id FROM agent_identities
		WHERE id='agent_source_first_race' FOR UPDATE;
		SELECT send_job_id FROM messages
		WHERE id='msg_source_first_race' FOR UPDATE;
	`); err != nil {
		t.Fatalf("hold production source-first locks: %v", err)
	}

	migrator, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Release()
	var migratorPID int
	if err := migrator.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&migratorPID); err != nil {
		t.Fatal(err)
	}
	migrationCtx, cancelMigration := context.WithTimeout(ctx, 8*time.Second)
	defer cancelMigration()
	migrationErrCh := make(chan error, 1)
	go func() {
		_, execErr := migrator.Exec(migrationCtx, string(migration))
		migrationErrCh <- execErr
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, 3*time.Second)
	defer cancelWait()
	waitingOnSource := false
	for !waitingOnSource {
		if err := pool.QueryRow(waitCtx, `
			SELECT COALESCE(wait_event_type='Lock',false)
			FROM pg_stat_activity WHERE pid=$1
		`, migratorPID).Scan(&waitingOnSource); err != nil {
			t.Fatalf("observe migration source wait: %v", err)
		}
		if !waitingOnSource {
			time.Sleep(10 * time.Millisecond)
		}
	}

	jobProbe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, jobProbeErr := jobProbe.Exec(ctx, `
		SELECT id FROM river_job WHERE id=$1 FOR UPDATE NOWAIT
	`, jobID)
	_ = jobProbe.Rollback(ctx)

	cancelErr := client.CancelTx(ctx, deletionTx, jobID)
	if cancelErr == nil {
		_, cancelErr = deletionTx.Exec(ctx, `DELETE FROM users WHERE id='usr_source_first_race'`)
	}
	if cancelErr == nil {
		cancelErr = deletionTx.Commit(ctx)
	} else {
		_ = deletionTx.Rollback(ctx)
	}
	deletionOpen = false
	migrationErr := <-migrationErrCh

	for name, got := range map[string]error{"migration": migrationErr, "cancellation": cancelErr} {
		var pgErr *pgconn.PgError
		if errors.As(got, &pgErr) && pgErr.Code == "40P01" {
			t.Errorf("%s hit source/job lock-order deadlock: %v", name, got)
		} else if got != nil {
			t.Errorf("%s failed: %v", name, got)
		}
	}
	var probePgErr *pgconn.PgError
	if errors.As(jobProbeErr, &probePgErr) && probePgErr.Code == "55P03" && migrationErr == nil && cancelErr == nil {
		t.Error("migration held the River job while waiting on its source")
	} else if jobProbeErr != nil && !(errors.As(jobProbeErr, &probePgErr) && probePgErr.Code == "55P03") {
		t.Fatalf("probe migration River lock: %v", jobProbeErr)
	}

	releaseProbe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseProbe.Exec(ctx, `
		SELECT id FROM river_job WHERE id=$1 FOR UPDATE NOWAIT
	`, jobID); err != nil {
		_ = releaseProbe.Rollback(ctx)
		t.Fatalf("race left River job lock behind: %v", err)
	}
	if err := releaseProbe.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSendingBudgetMigrationLeavesPostSnapshotLegacyJobForResolver(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate River schema: %v", err)
	}

	const advisoryKey int64 = 6_104_112
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS sending_snapshot_barrier ON river_job;
			DROP FUNCTION IF EXISTS sending_snapshot_barrier();
			DELETE FROM river_job WHERE args->>'message_id'='msg_snapshot_late' OR args->>'webhook_id'='wh_snapshot_barrier';
			DELETE FROM sending_provider_operations WHERE source_account_ref='usr_snapshot_test';
			DELETE FROM users WHERE id='usr_snapshot_test';
		`)
	}
	cleanup()
	t.Cleanup(cleanup)

	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id,email,name,google_subject)
		VALUES ('usr_snapshot_test','owner@snapshot.test','Owner','sub-snapshot-test');
		INSERT INTO domains (domain,user_id,verified)
		VALUES ('snapshot.test','usr_snapshot_test',true);
		INSERT INTO agent_identities (id,registered_domain,user_id,name)
		VALUES ('agent_snapshot_test','snapshot.test','usr_snapshot_test','Snapshot');
		INSERT INTO messages
		    (id,agent_id,direction,sender,recipient,subject,delivery_status,sent_as)
		VALUES
		    ('msg_snapshot_late','agent_snapshot_test','outbound','sender@snapshot.test',
		     'recipient@example.test','Late snapshot job','accepted','relay');
		INSERT INTO webhooks (id,user_id,url,description,events,filters,signing_secret)
		VALUES ('wh_snapshot_barrier','usr_snapshot_test','https://hooks.snapshot.test/health',
		        'Snapshot barrier',ARRAY['email.received'],'{}','synthetic-secret');
	`); err != nil {
		t.Fatalf("seed snapshot sources: %v", err)
	}

	var barrierJobID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO river_job (args,kind,max_attempts)
		VALUES ('{"webhook_id":"wh_snapshot_barrier","kind":"warning"}'::jsonb,'webhook_notify',3)
		RETURNING id
	`).Scan(&barrierJobID); err != nil {
		t.Fatalf("insert snapshot barrier job: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION sending_snapshot_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.id = `+strconv.FormatInt(barrierJobID, 10)+` AND NEW.args ? 'operation_ref' THEN
				PERFORM pg_advisory_xact_lock(`+strconv.FormatInt(advisoryKey, 10)+`);
			END IF;
			RETURN NEW;
		END
		$$;
		CREATE TRIGGER sending_snapshot_barrier
		BEFORE UPDATE OF args ON river_job
		FOR EACH ROW EXECUTE FUNCTION sending_snapshot_barrier();
	`); err != nil {
		t.Fatalf("install snapshot barrier: %v", err)
	}

	blocker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire advisory blocker: %v", err)
	}
	defer blocker.Release()
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryKey); err != nil {
		t.Fatalf("hold snapshot barrier: %v", err)
	}
	barrierHeld := true
	defer func() {
		if barrierHeld {
			_, _ = blocker.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryKey)
		}
	}()

	migrationConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire migration connection: %v", err)
	}
	defer migrationConn.Release()
	var migrationPID int
	if err := migrationConn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&migrationPID); err != nil {
		t.Fatalf("read migration backend pid: %v", err)
	}
	migration, err := migrations.FS.ReadFile("113_sending_budget_ledger.sql")
	if err != nil {
		t.Fatalf("read sending budget migration: %v", err)
	}
	migrationErr := make(chan error, 1)
	go func() {
		_, execErr := migrationConn.Exec(context.Background(), string(migration))
		migrationErr <- execErr
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks
				WHERE pid=$1 AND locktype='advisory' AND NOT granted
			)
		`, migrationPID).Scan(&waiting); err != nil {
			t.Fatalf("observe snapshot barrier: %v", err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("migration did not reach the post-snapshot rewrite barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var lateJobID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO river_job (args,kind,max_attempts)
		VALUES ('{"message_id":"msg_snapshot_late"}'::jsonb,'outbound_send',3)
		RETURNING id
	`).Scan(&lateJobID); err != nil {
		t.Fatalf("insert post-snapshot legacy job: %v", err)
	}
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_unlock($1)`, advisoryKey); err != nil {
		t.Fatalf("release snapshot barrier: %v", err)
	}
	barrierHeld = false
	if err := <-migrationErr; err != nil {
		t.Fatalf("apply sending budget migration: %v", err)
	}

	var state string
	var finalized, hasOperationRef bool
	if err := pool.QueryRow(ctx, `
		SELECT state::text,finalized_at IS NOT NULL,args ? 'operation_ref'
		FROM river_job WHERE id=$1
	`, lateJobID).Scan(&state, &finalized, &hasOperationRef); err != nil {
		t.Fatalf("read post-snapshot job: %v", err)
	}
	if state != "available" || finalized || hasOperationRef {
		t.Fatalf("post-snapshot job state=%q finalized=%v operation_ref=%v, want untouched old-format available job", state, finalized, hasOperationRef)
	}
	var lateOperations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_provider_operations WHERE operation_id='msg_snapshot_late'`).Scan(&lateOperations); err != nil || lateOperations != 0 {
		t.Fatalf("post-snapshot operations=%d err=%v, want 0", lateOperations, err)
	}
}

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

func TestAgentPurgeMigrationAddsBoundedLookupIndexes(t *testing.T) {
	ctx := context.Background()
	f := newPurgeFixture(t, "purgeactiveindexplan")
	f.seedMessages(t, 20_000, "mplan_")
	sql, err := migrations.FS.ReadFile("100_messages_active_send_claim_idx.sql")
	if err != nil {
		t.Fatal(err)
	}
	pool := f.pool
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("second index migration application: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO messages
		     (id, agent_id, direction, sender, recipient, subject,
		      delivery_status, send_claimed_at)
		 VALUES ('msg_active_send_plan', $1, 'outbound', $1,
		         'recipient@example.test', 'active', 'sending', now())`, f.agentID); err != nil {
		t.Fatalf("seed active send: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE messages`); err != nil {
		t.Fatalf("analyze messages: %v", err)
	}

	var definition string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = 'public' AND indexname = 'messages_active_send_claim_idx'`,
	).Scan(&definition); err != nil {
		t.Fatalf("read active-send index: %v", err)
	}
	for _, fragment := range []string{
		"(agent_id, send_claimed_at)",
		"delivery_status = 'sending'",
		"send_claimed_at IS NOT NULL",
	} {
		if !strings.Contains(definition, fragment) {
			t.Fatalf("active-send index %q does not contain %q", definition, fragment)
		}
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disable sequential scan: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `RESET enable_seqscan`) }()
	rows, err := conn.Query(ctx,
		`EXPLAIN (COSTS OFF)
		 SELECT EXISTS (
		     SELECT 1 FROM messages
		      WHERE agent_id = $1
		        AND delivery_status = 'sending'
		        AND send_claimed_at > now() - make_interval(secs => 300)
		 )`, f.agentID)
	if err != nil {
		t.Fatalf("explain active-send guard: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if !strings.Contains(plan.String(), "messages_active_send_claim_idx") {
		t.Fatalf("active-send guard did not use bounded partial index:\n%s", plan.String())
	}
}

func TestSenderIdentityMigrationInstallsTriggerBeforeBackfill(t *testing.T) {
	sql, err := migrations.FS.ReadFile("101_sender_identity_managed_domains.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(sql)
	triggerPos := strings.Index(text, "CREATE TRIGGER domains_track_sender_identity_managed")
	backfillPos := strings.Index(text, "INSERT INTO sender_identity_managed_domains (domain, incarnation, applied_incarnation)\nSELECT")
	if triggerPos < 0 || backfillPos < 0 {
		t.Fatalf("migration is missing trigger or backfill")
	}
	if triggerPos > backfillPos {
		t.Fatalf("sender-identity trigger must be installed before the legacy backfill")
	}
}

func TestDomainTeardownReceiptMigrationUpgradesAppliedLegacyShape(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		CREATE SCHEMA domain_receipt_upgrade_probe;
		SET LOCAL search_path TO domain_receipt_upgrade_probe;
		CREATE TABLE users (id TEXT PRIMARY KEY);
		CREATE TABLE domains (domain TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id));
		CREATE TABLE domain_teardown_receipts (
			domain TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			state TEXT NOT NULL CHECK (state IN ('pending', 'manual_review', 'confirmed')),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			CHECK (domain = lower(domain))
		);
		CREATE INDEX idx_domain_teardown_receipts_user ON domain_teardown_receipts(user_id);
		CREATE FUNCTION clear_domain_teardown_receipt_on_registration()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			DELETE FROM domain_teardown_receipts WHERE domain = NEW.domain;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER domains_clear_teardown_receipt
		BEFORE INSERT ON domains FOR EACH ROW
		EXECUTE FUNCTION clear_domain_teardown_receipt_on_registration();
		INSERT INTO users (id) VALUES ('usr_upgrade');
		INSERT INTO domain_teardown_receipts (domain, user_id, state)
		VALUES ('upgrade.example.test', 'usr_upgrade', 'confirmed');
	`); err != nil {
		t.Fatalf("install legacy migration shape: %v", err)
	}

	upgrade, err := migrations.FS.ReadFile("104_domain_teardown_receipts_upgrade.sql")
	if err != nil {
		t.Fatalf("read upgrade migration: %v", err)
	}
	if _, err := tx.Exec(ctx, string(upgrade)); err != nil {
		t.Fatalf("apply upgrade migration: %v", err)
	}
	if _, err := tx.Exec(ctx, string(upgrade)); err != nil {
		t.Fatalf("reapply upgrade migration: %v", err)
	}

	var receiptID int64
	var incarnation, state string
	if err := tx.QueryRow(ctx,
		`SELECT receipt_id, incarnation, state FROM domain_teardown_receipts WHERE domain = 'upgrade.example.test'`,
	).Scan(&receiptID, &incarnation, &state); err != nil {
		t.Fatalf("read upgraded receipt: %v", err)
	}
	if receiptID < 1 || !strings.HasPrefix(incarnation, "legacy:") || state != "confirmed" {
		t.Fatalf("upgraded receipt = (%d, %q, %q)", receiptID, incarnation, state)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO domains (domain, user_id) VALUES ('upgrade.example.test', 'usr_upgrade')`); err != nil {
		t.Fatalf("register replacement: %v", err)
	}
	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM domain_teardown_receipts WHERE domain = 'upgrade.example.test'`,
	).Scan(&count); err != nil {
		t.Fatalf("count retained receipt: %v", err)
	}
	if count != 1 {
		t.Fatalf("replacement registration erased historical receipt: count=%d", count)
	}
}

// TestSenderIdentityStrandedDomainRepairMigrationBackfillsForgottenLedgerRows
// pins batch C finding 3: the pre-adoption v1.7.8 regression's
// ErrIdentityNotOwned handler unconditionally called
// ForgetSendingIdentityManaged, deleting a domain's ledger row even though
// the domain itself stayed live/owned in `domains` at sending_status
// ='failed'. Migration 101's trigger only re-inserts on INSERT, a
// transition FROM sending_status='none', or a verification_token change —
// none of which a domain already sitting at `failed` (its state since the
// moment it was stranded) hits again on its own, so nothing before this
// migration ever re-populates that row. This test models exactly that
// history — a live, owned, `failed` domain with NO ledger row, i.e. the
// pre-101-trigger-repair, post-Forget state — and proves migration 105
// re-inserts it with applied_incarnation NULL (forcing a real reconvergence
// pass, not a false "already applied" no-op) and leaves an already-ledgered
// domain (the non-stranded, common case) untouched.
func TestSenderIdentityStrandedDomainRepairMigrationBackfillsForgottenLedgerRows(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)

	user, err := store.CreateOrGetUser(ctx, "stranded-repair@example.com", "Stranded Repair", "stranded-repair-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	// The stranded domain: live, owned, sending_status='failed', but its
	// ledger row has been deleted — modeling the v1.7.8 Forget call that ran
	// on an ownership-check failure.
	stranded, err := store.ClaimOrCreateDomain(ctx, "stranded.example.com", user.ID)
	if err != nil {
		t.Fatalf("ClaimOrCreateDomain(stranded): %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET sending_status = 'failed' WHERE domain = $1`, stranded.Domain); err != nil {
		t.Fatalf("set stranded domain failed: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sender_identity_managed_domains WHERE domain = $1`, stranded.Domain); err != nil {
		t.Fatalf("delete ledger row (simulate the v1.7.8 Forget call): %v", err)
	}

	// The control domain: also failed, but its ledger row is intact and
	// already carries a confirmed applied_incarnation — the common,
	// non-stranded case. The migration must leave it exactly as-is.
	healthy, err := store.ClaimOrCreateDomain(ctx, "already-ledgered.example.com", user.ID)
	if err != nil {
		t.Fatalf("ClaimOrCreateDomain(healthy): %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET sending_status = 'failed' WHERE domain = $1`, healthy.Domain); err != nil {
		t.Fatalf("set control domain failed: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sender_identity_managed_domains SET applied_incarnation = incarnation WHERE domain = $1`,
		healthy.Domain,
	); err != nil {
		t.Fatalf("mark control domain's ledger row applied: %v", err)
	}

	sql, err := migrations.FS.ReadFile("105_sender_identity_managed_domains_repair_stranded.sql")
	if err != nil {
		t.Fatalf("read repair migration: %v", err)
	}
	// Apply twice: the migration runner only ever applies a given filename
	// once, but the ON CONFLICT DO NOTHING shape must also be safe to
	// re-run (e.g. a future manual repair pass) without disturbing state a
	// first pass already fixed.
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply repair migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("reapply repair migration: %v", err)
	}

	var incarnation string
	var appliedIncarnation *string
	if err := pool.QueryRow(ctx,
		`SELECT incarnation, applied_incarnation FROM sender_identity_managed_domains WHERE domain = $1`,
		stranded.Domain,
	).Scan(&incarnation, &appliedIncarnation); err != nil {
		t.Fatalf("read repaired ledger row: %v", err)
	}
	if incarnation != stranded.VerificationToken {
		t.Fatalf("repaired incarnation = %q, want the domain's verification_token %q", incarnation, stranded.VerificationToken)
	}
	if appliedIncarnation != nil {
		t.Fatalf("repaired applied_incarnation = %v, want NULL so the reaper actually reconverges the domain instead of treating it as already applied", *appliedIncarnation)
	}

	var healthyApplied *string
	if err := pool.QueryRow(ctx,
		`SELECT applied_incarnation FROM sender_identity_managed_domains WHERE domain = $1`,
		healthy.Domain,
	).Scan(&healthyApplied); err != nil {
		t.Fatalf("read control ledger row: %v", err)
	}
	if healthyApplied == nil || *healthyApplied != healthy.VerificationToken {
		t.Fatalf("control domain's already-applied ledger row was disturbed: applied_incarnation = %v, want unchanged %q", healthyApplied, healthy.VerificationToken)
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

func restoreMigrationRecords(t *testing.T, pool *pgxpool.Pool, names []string) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx,
		"SELECT filename FROM schema_migrations WHERE filename = ANY($1)",
		names,
	)
	if err != nil {
		t.Fatalf("snapshot migration records: %v", err)
	}
	var existing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan migration record: %v", err)
		}
		existing = append(existing, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate migration records: %v", err)
	}
	rows.Close()

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM schema_migrations WHERE filename = ANY($1)", names)
		for _, name := range existing {
			_, _ = pool.Exec(context.Background(),
				"INSERT INTO schema_migrations (filename) VALUES ($1) ON CONFLICT DO NOTHING",
				name)
		}
	})
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
