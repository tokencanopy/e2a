package sendingpolicy_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/migrations"
)

const generationZeroPolicy = `{"all_customer_global_daily_recipients":5000,"bounce_min_outcomes":50,"bounce_pause_basis_points":400,"budget_hold_max_days":7,"budget_mode":"disabled","complaint_pause_basis_points":8,"critical_operational_daily_recipients":100,"daily_unlimited_plan_codes":["starter","pro","scale"],"default_account_daily_recipients":100,"detector_interval_seconds":300,"detector_mode":"disabled","detector_window_days":7,"operator_notice_recipient_version":1,"probation_global_daily_recipients":150,"ramp_days":30,"ramp_enabled":false,"ramp_start_daily":150,"ramp_target_daily":2000,"sending_control_audit_retention_days":90,"sending_feedback_post_account_retention_days":30,"shared_domain_account_daily_recipients":50,"shared_reputation_bounce_min_outcomes":1,"tenant_header_canary_account_ids":[],"tenant_header_mode":"disabled","tenant_provisioning_mode":"disabled","tenant_suppression_sync_mode":"disabled","violation_operational_daily_recipients":100}`

func TestSendingProtectionMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	for _, name := range []string{
		"112_sending_protection_policy.sql",
		"113_sending_budget_ledger.sql",
		"114_sending_feedback_provenance.sql",
		"115_sending_controls_foreign_key.sql",
		"116_sending_message_holds.sql",
		"117_sending_suppression_provenance.sql",
		"118_sending_protection_validate_constraints.sql",
		"119_sending_protection_operator_audit.sql",
	} {
		migration, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("reapply %s: %v", name, err)
		}
	}
}

func TestOperatorRecipientRegistryRejectsTruncate(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO sending_operator_recipient_versions
		    (logical_version,commitment_key_id,recipient_commitment,created_by)
		VALUES (2147483646,repeat('c',64),repeat('d',64),'truncate-test')
		ON CONFLICT DO NOTHING
	`); err != nil {
		t.Fatalf("insert operator commitment: %v", err)
	}
	if _, err := tx.Exec(ctx, `TRUNCATE sending_operator_recipient_versions`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("operator commitment truncate err=%v, want append-only rejection", err)
	}
}

func TestSendingProtectionHotTableDDLIsEndLoadedBoundedAndNotValid(t *testing.T) {
	budgetBytes, err := migrations.FS.ReadFile("113_sending_budget_ledger.sql")
	if err != nil {
		t.Fatal(err)
	}
	budget := string(budgetBytes)
	discardPos := strings.LastIndex(budget, "SET state = 'discarded'")
	if discardPos < 0 || strings.Contains(budget, "ALTER TABLE messages") ||
		strings.Contains(budget, "LOCK TABLE users") ||
		strings.Contains(budget, "ADD CONSTRAINT account_sending_controls_user_id_fkey") {
		t.Fatal("migration 110 must end after its source/job rewrite without users, controls-FK, or messages locks")
	}
	snapshotPos := strings.Index(budget, "captured_kind TEXT NOT NULL")
	messageLockPos := strings.Index(budget, "FOR UPDATE OF message")
	webhookLockPos := strings.Index(budget, "FOR UPDATE OF webhook")
	jobLockPos := strings.Index(budget, "FOR UPDATE OF job")
	if snapshotPos < 0 || messageLockPos < snapshotPos || webhookLockPos < snapshotPos ||
		jobLockPos < messageLockPos || jobLockPos < webhookLockPos ||
		strings.Contains(budget[:max(jobLockPos, 0)], "FOR UPDATE OF job") {
		t.Fatal("migration 110 must snapshot targets unlocked, then lock every source before any target job")
	}
	for _, fragment := range []string{
		"job.current_kind = target.captured_kind",
		"job.current_args IS NOT DISTINCT FROM target.captured_args",
		"locked.current_args IS NOT DISTINCT FROM target.captured_args",
	} {
		if !strings.Contains(budget, fragment) {
			t.Errorf("migration 110 missing fixed-snapshot recheck %q", fragment)
		}
	}

	controlsBytes, err := migrations.FS.ReadFile("115_sending_controls_foreign_key.sql")
	if err != nil {
		t.Fatalf("read controls FK migration: %v", err)
	}
	controls := strings.Join(strings.Fields(string(controlsBytes)), " ")
	for _, fragment := range []string{
		"SET LOCAL lock_timeout = '2s'",
		"LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE",
		"LOCK TABLE account_sending_controls IN SHARE ROW EXCLUSIVE MODE",
		"DELETE FROM account_sending_controls AS controls WHERE NOT EXISTS",
		"ADD CONSTRAINT account_sending_controls_user_id_fkey",
		"FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE NOT VALID",
	} {
		if !strings.Contains(controls, fragment) {
			t.Errorf("source-free controls FK migration missing %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"FROM river_job", "FROM agent_identities", "ALTER TABLE messages",
		"LOCK TABLE messages", "FROM webhooks",
	} {
		if strings.Contains(controls, forbidden) {
			t.Errorf("source-free controls FK migration references %q", forbidden)
		}
	}

	feedbackBytes, err := migrations.FS.ReadFile("114_sending_feedback_provenance.sql")
	if err != nil {
		t.Fatal(err)
	}
	feedback := string(feedbackBytes)
	outcomesFKPos := strings.Index(feedback, "ADD CONSTRAINT account_sending_outcomes_daily_user_id_fkey")
	outcomesLockTimeoutPos := strings.LastIndex(feedback[:max(outcomesFKPos, 0)], "SET LOCAL lock_timeout = '2s'")
	if strings.Contains(feedback, "ALTER TABLE suppressions") {
		t.Fatal("migration 111 must finish account FK work without taking suppressions table locks")
	}
	if strings.Contains(feedback, "user_id TEXT NOT NULL REFERENCES users") ||
		outcomesFKPos < outcomesLockTimeoutPos ||
		!strings.Contains(feedback[outcomesFKPos:], "FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE NOT VALID") {
		t.Fatal("account outcomes FK must be named, NOT VALID, and installed in migration 111's bounded tail")
	}

	messagesBytes, err := migrations.FS.ReadFile("116_sending_message_holds.sql")
	if err != nil {
		t.Fatalf("read messages hot-DDL migration: %v", err)
	}
	messages := strings.Join(strings.Fields(string(messagesBytes)), " ")
	for _, fragment := range []string{
		"SET LOCAL lock_timeout = '2s'",
		"ALTER TABLE messages ADD COLUMN IF NOT EXISTS local_hold_class TEXT",
		"ALTER TABLE messages ADD COLUMN IF NOT EXISTS local_hold_anchor TIMESTAMPTZ",
		"messages_local_hold_check",
		"NOT VALID",
	} {
		if !strings.Contains(messages, fragment) {
			t.Errorf("messages hot-DDL migration missing %q", fragment)
		}
	}
	if strings.Contains(messages, "LOCK TABLE users") ||
		strings.Contains(messages, "ALTER TABLE account_sending") ||
		strings.Contains(messages, "ALTER TABLE suppressions") {
		t.Fatal("messages hot-DDL migration must contain no other strong-table work")
	}

	suppressionsBytes, err := migrations.FS.ReadFile("117_sending_suppression_provenance.sql")
	if err != nil {
		t.Fatalf("read suppressions hot-DDL migration: %v", err)
	}
	suppressions := strings.Join(strings.Fields(string(suppressionsBytes)), " ")
	for _, fragment := range []string{
		"SET LOCAL lock_timeout = '2s'",
		"ALTER TABLE suppressions ADD COLUMN IF NOT EXISTS sync_generation BIGINT NOT NULL DEFAULT 1",
		"ALTER TABLE suppressions ADD COLUMN IF NOT EXISTS removal_pending BOOLEAN NOT NULL DEFAULT false",
		"suppressions_sync_generation_check",
		"NOT VALID",
	} {
		if !strings.Contains(suppressions, fragment) {
			t.Errorf("suppressions hot-DDL migration missing %q", fragment)
		}
	}
	if strings.Contains(suppressions, "LOCK TABLE users") ||
		strings.Contains(suppressions, "ALTER TABLE account_sending") ||
		strings.Contains(suppressions, "ALTER TABLE messages") {
		t.Fatal("suppressions hot-DDL migration must contain no other strong-table work")
	}

	validationBytes, err := migrations.FS.ReadFile("118_sending_protection_validate_constraints.sql")
	if err != nil {
		t.Fatalf("read validation migration: %v", err)
	}
	validation := strings.Join(strings.Fields(string(validationBytes)), " ")
	for _, fragment := range []string{
		"SET LOCAL lock_timeout = '2s'",
		"ALTER TABLE messages VALIDATE CONSTRAINT messages_local_hold_check",
		"ALTER TABLE suppressions VALIDATE CONSTRAINT suppressions_sync_generation_check",
		"ALTER TABLE account_sending_controls VALIDATE CONSTRAINT account_sending_controls_user_id_fkey",
		"ALTER TABLE account_sending_outcomes_daily VALIDATE CONSTRAINT account_sending_outcomes_daily_user_id_fkey",
	} {
		if !strings.Contains(validation, fragment) {
			t.Errorf("validation migration missing %q", fragment)
		}
	}
}

func TestSendingBudgetMigrationDoesNotDeadlockAccountDeletionOrder(t *testing.T) {
	for _, sourceKind := range []string{"outbound", "webhook"} {
		t.Run(sourceKind, func(t *testing.T) {
			testSendingBudgetMigrationDoesNotDeadlockAccountDeletionOrder(t, sourceKind)
		})
	}
}

func testSendingBudgetMigrationDoesNotDeadlockAccountDeletionOrder(t *testing.T, sourceKind string) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate River schema: %v", err)
	}
	migration, err := migrations.FS.ReadFile("113_sending_budget_ledger.sql")
	if err != nil {
		t.Fatal(err)
	}
	controlsMigration, err := migrations.FS.ReadFile("115_sending_controls_foreign_key.sql")
	if err != nil {
		t.Fatal(err)
	}
	const barrierKey int64 = 8364511027
	cleanupData := func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS sending_controls_tail_barrier ON river_job;
			DROP FUNCTION IF EXISTS sending_controls_tail_barrier();
			DELETE FROM river_job
			WHERE args->>'message_id'='msg_tail_deadlock'
			   OR args->>'webhook_id'='wh_tail_deadlock';
			DELETE FROM sending_provider_operations
			WHERE source_account_ref='usr_tail_deadlock';
			DELETE FROM users WHERE id='usr_tail_deadlock';
		`)
	}
	cleanupData()
	if _, err := pool.Exec(ctx, `DROP TABLE account_sending_controls`); err != nil {
		t.Fatalf("remove controls/FK for true first-apply race: %v", err)
	}
	t.Cleanup(func() {
		cleanupData()
		if _, err := pool.Exec(context.Background(), string(migration)); err != nil {
			t.Errorf("restore budget migration after controls-tail race: %v", err)
			return
		}
		if _, err := pool.Exec(context.Background(), string(controlsMigration)); err != nil {
			t.Errorf("restore controls FK after controls-tail race: %v", err)
		}
	})
	seedSQL := `
		INSERT INTO users (id,email,name,google_subject)
		VALUES ('usr_tail_deadlock','owner@tail-deadlock.test','Owner','sub-tail-deadlock')`
	jobArgs := `{"message_id":"msg_tail_deadlock"}`
	jobKind := "outbound_send"
	if sourceKind == "outbound" {
		seedSQL += `;
			INSERT INTO domains (domain,user_id,verified)
			VALUES ('tail-deadlock.test','usr_tail_deadlock',true);
			INSERT INTO agent_identities (id,registered_domain,user_id,name)
			VALUES ('agent_tail_deadlock','tail-deadlock.test','usr_tail_deadlock','Tail');
			INSERT INTO messages
			    (id,agent_id,direction,sender,recipient,subject,delivery_status,sent_as)
			VALUES ('msg_tail_deadlock','agent_tail_deadlock','outbound',
			        'sender@tail-deadlock.test','recipient@example.test','Tail lock',
			        'accepted','relay')`
	} else {
		seedSQL += `;
			INSERT INTO webhooks
			    (id,user_id,url,description,events,filters,signing_secret)
			VALUES ('wh_tail_deadlock','usr_tail_deadlock',
			        'https://hooks.tail-deadlock.test/notify','Tail lock',
			        ARRAY['email.received'],'{}','synthetic-secret')`
		jobArgs = `{"webhook_id":"wh_tail_deadlock","kind":"warning"}`
		jobKind = "webhook_notify"
	}
	if _, err := pool.Exec(ctx, seedSQL); err != nil {
		t.Fatalf("seed tail deadlock fixture: %v", err)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO river_job (args,kind,max_attempts)
		VALUES ($1::jsonb,$2,3)
		RETURNING id
	`, jobArgs, jobKind).Scan(&jobID); err != nil {
		t.Fatalf("insert tail-deadlock River job: %v", err)
	}
	if sourceKind == "outbound" {
		if _, err := pool.Exec(ctx, `UPDATE messages SET send_job_id=$2 WHERE id=$1`, "msg_tail_deadlock", jobID); err != nil {
			t.Fatalf("link tail-deadlock River job: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION sending_controls_tail_barrier() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(8364511027);
			RETURN NEW;
		END
		$$;
		CREATE TRIGGER sending_controls_tail_barrier
		BEFORE UPDATE ON river_job
		FOR EACH ROW EXECUTE FUNCTION sending_controls_tail_barrier();
	`); err != nil {
		t.Fatalf("install controls-tail barrier: %v", err)
	}

	barrier, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Release()
	barrierHeld := true
	defer func() {
		if barrierHeld {
			_, _ = barrier.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, barrierKey)
		}
	}()
	if _, err := barrier.Exec(ctx, `SELECT pg_advisory_lock($1)`, barrierKey); err != nil {
		t.Fatalf("hold controls-tail barrier: %v", err)
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
	waitingAtBarrier := false
	for !waitingAtBarrier {
		if err := pool.QueryRow(waitCtx, `
			SELECT COALESCE(wait_event_type='Lock',false)
			FROM pg_stat_activity WHERE pid=$1
		`, migratorPID).Scan(&waitingAtBarrier); err != nil {
			t.Fatalf("observe migration after source locks: %v", err)
		}
		if !waitingAtBarrier {
			time.Sleep(10 * time.Millisecond)
		}
	}

	deletion, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deletion.Release()
	var deletionPID int
	if err := deletion.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&deletionPID); err != nil {
		t.Fatal(err)
	}
	deletionErrCh := make(chan error, 1)
	go func() {
		_, deleteErr := deletion.Exec(migrationCtx, `DELETE FROM users WHERE id='usr_tail_deadlock'`)
		deletionErrCh <- deleteErr
	}()
	waitingOnSource := false
	for !waitingOnSource {
		if err := pool.QueryRow(waitCtx, `
			SELECT COALESCE(wait_event_type='Lock',false)
			FROM pg_stat_activity WHERE pid=$1
		`, deletionPID).Scan(&waitingOnSource); err != nil {
			t.Fatalf("observe account deletion waiting on locked source: %v", err)
		}
		if !waitingOnSource {
			time.Sleep(10 * time.Millisecond)
		}
	}

	if _, err := barrier.Exec(ctx, `SELECT pg_advisory_unlock($1)`, barrierKey); err != nil {
		t.Fatalf("release controls-tail barrier: %v", err)
	}
	barrierHeld = false
	migrationErr := <-migrationErrCh
	deletionErr := <-deletionErrCh

	for name, got := range map[string]error{"migration": migrationErr, "deletion": deletionErr} {
		var pgErr *pgconn.PgError
		if errors.As(got, &pgErr) && pgErr.Code == "40P01" {
			t.Errorf("%s hit lock-order deadlock: %v", name, got)
		} else if got != nil {
			t.Errorf("%s failed: %v", name, got)
		}
	}

	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Exec(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE NOWAIT`); err != nil {
		_ = probe.Rollback(ctx)
		t.Fatalf("race left users lock behind: %v", err)
	}
	sourceProbeSQL := `SELECT id FROM agent_identities WHERE id='agent_tail_deadlock' FOR UPDATE NOWAIT`
	if sourceKind == "webhook" {
		sourceProbeSQL = `SELECT id FROM webhooks WHERE id='wh_tail_deadlock' FOR UPDATE NOWAIT`
	}
	if _, err := probe.Exec(ctx, sourceProbeSQL); err != nil {
		_ = probe.Rollback(ctx)
		t.Fatalf("race left source lock behind: %v", err)
	}
	if _, err := probe.Exec(ctx, `SELECT id FROM river_job WHERE id=$1 FOR UPDATE NOWAIT`, jobID); err != nil {
		_ = probe.Rollback(ctx)
		t.Fatalf("race left River job lock behind: %v", err)
	}
	if err := probe.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSendingProtectionAccountFKFirstApplyLockWaitIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name       string
		migration  string
		resetSQL   string
		seedHotRow string
		hotUpdate  string
		hotRead    string
	}{
		{
			"controls", "115_sending_controls_foreign_key.sql",
			`ALTER TABLE account_sending_controls DROP CONSTRAINT IF EXISTS account_sending_controls_user_id_fkey`,
			`INSERT INTO users (id,email,name,google_subject)
			 VALUES ('usr_first_apply_hot','owner@first-apply-hot.test','Owner','sub-first-apply-hot');
			 INSERT INTO domains (domain,user_id,verified)
			 VALUES ('first-apply-hot.test','usr_first_apply_hot',true);
			 INSERT INTO agent_identities (id,registered_domain,user_id,name)
			 VALUES ('agent_first_apply_hot','first-apply-hot.test','usr_first_apply_hot','Hot');
			 INSERT INTO messages (id,agent_id,direction,sender,recipient,subject)
			 VALUES ('msg_first_apply_hot','agent_first_apply_hot','outbound',
			         'sender@first-apply-hot.test','recipient@example.test','Hot row')`,
			`UPDATE messages SET subject=subject WHERE id='msg_first_apply_hot'`,
			`SELECT count(*) FROM messages WHERE id='msg_first_apply_hot'`,
		},
		{
			"outcomes", "114_sending_feedback_provenance.sql",
			`DROP TABLE account_sending_outcomes_daily`,
			`INSERT INTO users (id,email,name,google_subject)
			 VALUES ('usr_first_apply_hot','owner@first-apply-hot.test','Owner','sub-first-apply-hot');
			 INSERT INTO suppressions (id,user_id,address,reason,source)
			 VALUES ('sup_first_apply_hot','usr_first_apply_hot','recipient@example.test','synthetic','manual')`,
			`UPDATE suppressions SET reason=reason WHERE id='sup_first_apply_hot'`,
			`SELECT count(*) FROM suppressions WHERE id='sup_first_apply_hot'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := testutil.TestDB(t)
			migration, err := migrations.FS.ReadFile(tc.migration)
			if err != nil {
				t.Fatal(err)
			}
			validation, err := migrations.FS.ReadFile("118_sending_protection_validate_constraints.sql")
			if err != nil {
				t.Fatal(err)
			}

			if _, err := pool.Exec(ctx, tc.seedHotRow); err != nil {
				t.Fatalf("seed %s hot-table row: %v", tc.name, err)
			}
			if _, err := pool.Exec(ctx, tc.resetSQL); err != nil {
				t.Fatalf("reset %s FK migration for first-apply probe: %v", tc.name, err)
			}

			restored := false
			t.Cleanup(func() {
				if restored {
					return
				}
				if _, err := pool.Exec(context.Background(), string(migration)); err != nil {
					t.Errorf("restore %s migration: %v", tc.name, err)
					return
				}
				if _, err := pool.Exec(context.Background(), string(validation)); err != nil {
					t.Errorf("restore validation migration after %s: %v", tc.name, err)
				}
			})

			writer, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Release()
			writerTx, err := writer.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writerTx.Rollback(ctx)
			if _, err := writerTx.Exec(ctx, `
				INSERT INTO users (id,email,name,google_subject)
				VALUES ('usr_first_apply_writer','owner@first-apply.test','Owner','sub-first-apply')
			`); err != nil {
				t.Fatalf("hold users writer: %v", err)
			}

			migrator, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer migrator.Release()
			var migratorPID int
			if err := migrator.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&migratorPID); err != nil {
				t.Fatalf("read migrator backend PID: %v", err)
			}
			type migrationResult struct {
				err     error
				elapsed time.Duration
			}
			migrationResultCh := make(chan migrationResult, 1)
			migrationCtx, cancelMigration := context.WithTimeout(ctx, 4*time.Second)
			go func() {
				started := time.Now()
				_, migrationErr := migrator.Exec(migrationCtx, string(migration))
				migrationResultCh <- migrationResult{migrationErr, time.Since(started)}
			}()

			waitCtx, cancelWait := context.WithTimeout(ctx, time.Second)
			defer cancelWait()
			waitingOnUsers := false
			for !waitingOnUsers {
				if err := pool.QueryRow(waitCtx, `
					SELECT EXISTS (
						SELECT 1 FROM pg_locks
						WHERE pid=$1
						  AND relation='users'::regclass
						  AND mode='ShareRowExclusiveLock'
						  AND NOT granted
					)`, migratorPID).Scan(&waitingOnUsers); err != nil {
					cancelMigration()
					<-migrationResultCh
					t.Fatalf("observe %s migration waiting on users: %v", tc.name, err)
				}
				if !waitingOnUsers {
					time.Sleep(10 * time.Millisecond)
				}
			}

			hotCtx, cancelHot := context.WithTimeout(ctx, 500*time.Millisecond)
			hotStarted := time.Now()
			_, hotUpdateErr := pool.Exec(hotCtx, tc.hotUpdate)
			var hotRows int
			hotReadErr := pool.QueryRow(hotCtx, tc.hotRead).Scan(&hotRows)
			cancelHot()
			hotElapsed := time.Since(hotStarted)

			result := <-migrationResultCh
			cancelMigration()
			if hotUpdateErr != nil || hotReadErr != nil || hotRows != 1 {
				t.Errorf("%s traffic blocked while migration waited on users after %s: update=%v read=%v rows=%d", tc.name, hotElapsed, hotUpdateErr, hotReadErr, hotRows)
			}

			err = result.err
			elapsed := result.elapsed
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				t.Fatalf("first-apply %s under users writer err=%v after %s, want bounded PostgreSQL lock timeout", tc.name, err, elapsed)
			}
			if elapsed > 3*time.Second {
				t.Fatalf("first-apply %s users lock wait took %s, want <=3s", tc.name, elapsed)
			}

			if err := writerTx.Rollback(ctx); err != nil {
				t.Fatalf("release users writer: %v", err)
			}
			if _, err := pool.Exec(ctx, string(migration)); err != nil {
				t.Fatalf("restore %s after bounded failure: %v", tc.name, err)
			}
			if _, err := pool.Exec(ctx, string(validation)); err != nil {
				t.Fatalf("validate restored %s constraints: %v", tc.name, err)
			}
			restored = true
		})
	}
}

func TestSendingControlsFKTailRemovesOnlyOrphans(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	migration, err := migrations.FS.ReadFile("115_sending_controls_foreign_key.sql")
	if err != nil {
		t.Fatal(err)
	}
	validation, err := migrations.FS.ReadFile("118_sending_protection_validate_constraints.sql")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), string(migration))
		_, _ = pool.Exec(context.Background(), string(validation))
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM users
			WHERE id IN ('usr_controls_live','usr_controls_orphan')
		`)
	})

	if _, err := pool.Exec(ctx, `
		ALTER TABLE account_sending_controls
		    DROP CONSTRAINT IF EXISTS account_sending_controls_user_id_fkey;
		INSERT INTO users (id,email,name,google_subject) VALUES
		    ('usr_controls_live','owner@controls-live.test','Owner','sub-controls-live'),
		    ('usr_controls_orphan','owner@controls-orphan.test','Owner','sub-controls-orphan');
		INSERT INTO account_sending_controls (user_id,reason) VALUES
		    ('usr_controls_live','live synthetic control'),
		    ('usr_controls_orphan','orphan synthetic control');
		DELETE FROM users WHERE id='usr_controls_orphan';
	`); err != nil {
		t.Fatalf("seed realistic controls orphan: %v", err)
	}
	var orphanBefore int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM account_sending_controls
		WHERE user_id='usr_controls_orphan'
	`).Scan(&orphanBefore); err != nil || orphanBefore != 1 {
		t.Fatalf("pre-migration orphan controls=%d err=%v, want 1", orphanBefore, err)
	}

	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply controls FK tail to realistic orphan: %v", err)
	}
	var liveRows, orphanRows int
	if err := pool.QueryRow(ctx, `
		SELECT
		    count(*) FILTER (WHERE user_id='usr_controls_live'),
		    count(*) FILTER (WHERE user_id='usr_controls_orphan')
		FROM account_sending_controls
	`).Scan(&liveRows, &orphanRows); err != nil {
		t.Fatalf("read controls after orphan cleanup: %v", err)
	}
	if liveRows != 1 || orphanRows != 0 {
		t.Fatalf("controls after FK tail live=%d orphan=%d, want 1 and 0", liveRows, orphanRows)
	}
	if _, err := pool.Exec(ctx, string(validation)); err != nil {
		t.Fatalf("validate controls FK after orphan cleanup: %v", err)
	}
}

func TestSendingProtectionHotTableAlterLockWaitIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		table     string
		migration string
	}{
		{"messages", "messages", "116_sending_message_holds.sql"},
		{"suppressions", "suppressions", "117_sending_suppression_provenance.sql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.TestDB(t)
			ctx := context.Background()
			migration, err := migrations.FS.ReadFile(tc.migration)
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Release()
			tx, err := blocker.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `LOCK TABLE `+tc.table+` IN ACCESS SHARE MODE`); err != nil {
				t.Fatalf("hold reader lock on %s: %v", tc.table, err)
			}

			migrationCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			started := time.Now()
			_, err = pool.Exec(migrationCtx, string(migration))
			elapsed := time.Since(started)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				t.Fatalf("migration under %s reader lock err=%v after %s, want bounded PostgreSQL lock timeout", tc.table, err, elapsed)
			}
			if elapsed > 3*time.Second {
				t.Fatalf("migration lock wait on %s took %s, want <=3s", tc.table, elapsed)
			}
		})
	}
}

func TestSendingProtectionConstraintValidationAllowsNormalDML(t *testing.T) {
	validation, err := migrations.FS.ReadFile("118_sending_protection_validate_constraints.sql")
	if err != nil {
		t.Fatalf("read validation migration: %v", err)
	}
	ctx := context.Background()
	pool := testutil.TestDB(t)
	if _, err := pool.Exec(ctx, `
		DELETE FROM users WHERE id='usr_validation_dml';
		INSERT INTO users (id,email,name,google_subject)
		VALUES ('usr_validation_dml','owner@validation-dml.test','Owner','sub-validation-dml');
		INSERT INTO domains (domain,user_id,verified)
		VALUES ('validation-dml.test','usr_validation_dml',true);
		INSERT INTO agent_identities (id,registered_domain,user_id,name)
		VALUES ('agent_validation_dml','validation-dml.test','usr_validation_dml','Validation');
		INSERT INTO messages (id,agent_id,direction,sender,recipient,subject)
		VALUES ('msg_validation_dml','agent_validation_dml','outbound','sender@validation-dml.test','recipient@example.test','Validation');
		INSERT INTO suppressions (id,user_id,address,reason,source)
		VALUES ('sup_validation_dml','usr_validation_dml','recipient@example.test','synthetic','manual');
		INSERT INTO account_sending_controls (user_id)
		VALUES ('usr_validation_dml');
		INSERT INTO account_sending_outcomes_daily
		    (user_id,outcome_epoch,day,shared_reputation,delivered_count)
		VALUES ('usr_validation_dml',1,CURRENT_DATE,true,1);
		ALTER TABLE messages DROP CONSTRAINT IF EXISTS messages_local_hold_check;
		ALTER TABLE messages ADD CONSTRAINT messages_local_hold_check CHECK (
			(local_hold_class IS NULL AND local_hold_anchor IS NULL)
			OR (local_hold_class IN ('rate_ramp_or_provider','tenant_setup','policy_budget')
			    AND local_hold_anchor IS NOT NULL)
		) NOT VALID;
		ALTER TABLE suppressions DROP CONSTRAINT IF EXISTS suppressions_sync_generation_check;
		ALTER TABLE suppressions ADD CONSTRAINT suppressions_sync_generation_check
		    CHECK (sync_generation > 0) NOT VALID;
		ALTER TABLE account_sending_controls
		    DROP CONSTRAINT IF EXISTS account_sending_controls_user_id_fkey;
		ALTER TABLE account_sending_controls
		    ADD CONSTRAINT account_sending_controls_user_id_fkey
		    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE NOT VALID;
		ALTER TABLE account_sending_outcomes_daily
		    DROP CONSTRAINT IF EXISTS account_sending_outcomes_daily_user_id_fkey;
		ALTER TABLE account_sending_outcomes_daily
		    ADD CONSTRAINT account_sending_outcomes_daily_user_id_fkey
		    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE NOT VALID;
	`); err != nil {
		t.Fatalf("seed validation fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), string(validation))
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id='usr_validation_dml'`)
	})

	writer, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release()
	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE users SET name=name WHERE id='usr_validation_dml';
		UPDATE messages SET subject=subject WHERE id='msg_validation_dml';
		UPDATE suppressions SET reason=reason WHERE id='sup_validation_dml';
		UPDATE account_sending_controls SET reason=reason WHERE user_id='usr_validation_dml';
		UPDATE account_sending_outcomes_daily SET delivered_count=delivered_count
		WHERE user_id='usr_validation_dml';
	`); err != nil {
		t.Fatalf("hold normal DML transaction: %v", err)
	}

	validationCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := pool.Exec(validationCtx, string(validation)); err != nil {
		t.Fatalf("constraint validation blocked behind normal DML: %v", err)
	}
}

func TestSendingProtectionSchemaContracts(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)

	tables := []string{
		"sending_protection_runtime_policy", "sending_protection_policy_events",
		"sending_protection_runtime_attestation", "sending_operator_recipient_versions",
		"sending_ramp_grandfathering", "sending_provider_operations",
		"sending_budget_counters", "sending_budget_reservations",
		"account_sending_controls", "account_sending_control_events",
		"sending_protection_notice_events", "sending_protection_notice_deliveries",
		"sending_feedback_correlations", "sending_feedback_recipients",
		"sending_feedback_events", "account_sending_outcomes_daily",
	}
	for _, table := range tables {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("missing table %s", table)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	var generation int64
	var schemaVersion int
	var policyJSON []byte
	var policyHash, budgetMode string
	if err := pool.QueryRow(ctx, `
		SELECT generation, schema_version, policy, policy_sha256, policy->>'budget_mode'
		FROM sending_protection_runtime_policy WHERE singleton`).Scan(
		&generation, &schemaVersion, &policyJSON, &policyHash, &budgetMode,
	); err != nil {
		t.Fatalf("read generation zero: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(policyJSON, &decoded); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("canonicalize policy fixture: %v", err)
	}
	sum := sha256.Sum256(canonical)
	if generation != 0 || schemaVersion != 1 || budgetMode != "disabled" || string(canonical) != generationZeroPolicy || policyHash != hex.EncodeToString(sum[:]) || policyHash != "198d8cfb3220b6094a3b8dfe13cb0e2ff97c512ad87ae14609e580ae335c9ce6" {
		t.Fatalf("generation zero mismatch: generation=%d schema=%d mode=%q hash=%q canonical=%s", generation, schemaVersion, budgetMode, policyHash, canonical)
	}

	var revision int64
	var activeDigest, rollbackDigest string
	var activeContract, rollbackContract int
	if err := pool.QueryRow(ctx, `SELECT revision, active_billing_digest, active_billing_contract, rollback_billing_digest, rollback_billing_contract FROM sending_protection_runtime_attestation WHERE singleton`).Scan(&revision, &activeDigest, &activeContract, &rollbackDigest, &rollbackContract); err != nil {
		t.Fatalf("read runtime attestation: %v", err)
	}
	if revision != 0 || activeDigest != "" || rollbackDigest != "" || activeContract != 0 || rollbackContract != 0 {
		t.Fatalf("runtime attestation sentinel = (%d,%q,%d,%q,%d)", revision, activeDigest, activeContract, rollbackDigest, rollbackContract)
	}

	for _, table := range []string{"sending_provider_operations", "sending_budget_reservations", "account_sending_control_events", "sending_protection_notice_events", "sending_feedback_correlations", "sending_feedback_recipients", "sending_feedback_events"} {
		var fkCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid=$1::regclass AND contype='f'`, table).Scan(&fkCount); err != nil {
			t.Fatal(err)
		}
		if fkCount != 0 {
			t.Errorf("%s has %d foreign keys; security ledger rows must not reference customer trees", table, fkCount)
		}
	}
	var deliveryFKs int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE conrelid='sending_protection_notice_deliveries'::regclass
		  AND contype='f'
		  AND confrelid='sending_protection_notice_events'::regclass
		  AND confdeltype='c'`).Scan(&deliveryFKs); err != nil || deliveryFKs != 1 {
		t.Fatalf("notice delivery cascade foreign keys=%d err=%v, want exactly one", deliveryFKs, err)
	}

	var addressColumns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name IN ('sending_protection_notice_events','sending_protection_notice_deliveries') AND column_name ILIKE '%address%'`).Scan(&addressColumns); err != nil {
		t.Fatal(err)
	}
	if addressColumns != 0 {
		t.Fatalf("notice outbox exposes %d address column(s)", addressColumns)
	}

	for _, index := range []string{
		"account_sending_controls_active_scan_idx", "sending_protection_notice_pending_idx",
		"sending_protection_notice_deadline_idx",
		"sending_budget_reservations_expiry_idx", "sending_feedback_correlations_provider_message_idx",
		"sending_feedback_correlations_account_retention_idx", "account_sending_outcomes_daily_scan_idx",
		"sending_protection_notice_account_budget_uniq", "sending_protection_notice_global_guardrail_uniq",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, index).Scan(&exists); err != nil || !exists {
			t.Errorf("missing index %s (err=%v)", index, err)
		}
	}

	for _, constraint := range []string{
		"messages_local_hold_check",
		"suppressions_sync_generation_check",
		"account_sending_controls_user_id_fkey",
		"account_sending_outcomes_daily_user_id_fkey",
	} {
		var validated bool
		if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname=$1`, constraint).Scan(&validated); err != nil {
			t.Fatalf("read validation state for %s: %v", constraint, err)
		}
		if !validated {
			t.Errorf("constraint %s remains NOT VALID after migration 115", constraint)
		}
	}

	// Registry versions are an immutable, lowercase commitment ledger. Keep the
	// probe transactional because a successful row must otherwise live forever.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO sending_operator_recipient_versions (logical_version, commitment_key_id, recipient_commitment, created_by) VALUES (2147483647, repeat('a',64), repeat('b',64), 'schema-test') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("insert operator commitment: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sending_operator_recipient_versions SET created_by='changed' WHERE logical_version=2147483647`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("operator commitment update err=%v, want append-only rejection", err)
	}

	for _, code := range []string{"submission.policy_budget_expired", "submission.sending_setup_expired"} {
		var stage, outcome string
		var retryable bool
		if err := pool.QueryRow(ctx, `SELECT stage,outcome,retryable FROM message_lifecycle_reason_codes WHERE code=$1`, code).Scan(&stage, &outcome, &retryable); err != nil || stage != "submission" || outcome != "failed" || retryable {
			t.Errorf("lifecycle tuple %s=(%q,%q,%v) err=%v", code, stage, outcome, retryable, err)
		}
	}
}

func TestDeletionLeavesLedgerAndFeedbackProvenance(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM sending_protection_notice_deliveries WHERE event_id='notice_ledger_test';
			DELETE FROM sending_protection_notice_events WHERE id='notice_ledger_test';
			DELETE FROM account_sending_control_events WHERE id='control_event_ledger_test';
			DELETE FROM sending_feedback_recipients WHERE correlation_id='corr_ledger_test';
			DELETE FROM sending_feedback_events WHERE correlation_id='corr_ledger_test';
			DELETE FROM sending_feedback_correlations WHERE operation_id='msg_ledger_test';
			DELETE FROM sending_budget_reservations WHERE operation_id='msg_ledger_test';
			DELETE FROM sending_provider_operations WHERE operation_id='msg_ledger_test';
		`)
	})
	if _, err := pool.Exec(ctx, `
		DELETE FROM sending_protection_notice_deliveries WHERE event_id='notice_ledger_test';
		DELETE FROM sending_protection_notice_events WHERE id='notice_ledger_test';
		DELETE FROM account_sending_control_events WHERE id='control_event_ledger_test';
		DELETE FROM sending_feedback_recipients WHERE correlation_id='corr_ledger_test';
		DELETE FROM sending_feedback_events WHERE correlation_id='corr_ledger_test';
		DELETE FROM sending_feedback_correlations WHERE operation_id='msg_ledger_test';
		DELETE FROM sending_budget_reservations WHERE operation_id='msg_ledger_test';
		DELETE FROM sending_provider_operations WHERE operation_id='msg_ledger_test';
	`); err != nil {
		t.Fatalf("clear durable fixture rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id,email,name,google_subject) VALUES ('usr_ledger_test','owner@ledger.test','Owner','sub-ledger-test');
		INSERT INTO domains (domain,user_id,verified) VALUES ('ledger.test','usr_ledger_test',true);
		INSERT INTO agent_identities (id,registered_domain,user_id,name) VALUES ('agent_ledger_test','ledger.test','usr_ledger_test','Ledger');
		INSERT INTO messages (id,agent_id,direction,sender,recipient,subject) VALUES ('msg_ledger_test','agent_ledger_test','outbound','sender@ledger.test','recipient@example.test','Synthetic');
		INSERT INTO suppressions (id,user_id,address,reason,source) VALUES ('sup_ledger_test','usr_ledger_test','recipient@example.test','synthetic','manual');
		INSERT INTO account_sending_controls (user_id) VALUES ('usr_ledger_test');
		INSERT INTO account_sending_outcomes_daily (user_id,outcome_epoch,day,shared_reputation,delivered_count) VALUES ('usr_ledger_test',1,CURRENT_DATE,true,1);
		INSERT INTO account_sending_control_events (id,account_ref,old_state,new_state,reason,actor,expires_at) VALUES ('control_event_ledger_test','usr_ledger_test','active','paused','synthetic','schema-test',now()+interval '90 days');
		INSERT INTO sending_protection_notice_events (id,account_ref,kind,reason_code,source_event_id,expires_at) VALUES ('notice_ledger_test','usr_ledger_test','pause','manual','control_event_ledger_test',now()+interval '30 days');
		INSERT INTO sending_protection_notice_deliveries (event_id,audience) VALUES ('notice_ledger_test','owner'),('notice_ledger_test','operator');
		INSERT INTO sending_provider_operations (operation_id,source_account_ref,policy_subject_ref,purpose,expires_at) VALUES ('msg_ledger_test','usr_ledger_test','usr_ledger_test','customer_message',now()+interval '30 days');
		INSERT INTO sending_budget_reservations (operation_id,submission_attempt,source_account_ref,policy_subject_ref,purpose,day,units,probation,state) VALUES ('msg_ledger_test',1,'usr_ledger_test','usr_ledger_test','customer_message',CURRENT_DATE,1,false,'reserved');
		INSERT INTO sending_feedback_correlations (correlation_id,operation_id,submission_attempt,source_account_ref,policy_subject_ref,purpose,tenant_mode) VALUES ('corr_ledger_test','msg_ledger_test',1,'usr_ledger_test','usr_ledger_test','customer_message','none');
		INSERT INTO sending_feedback_recipients (correlation_id,recipient_hmac,hmac_key_version) VALUES ('corr_ledger_test',decode(repeat('ab',32),'hex'),1);
		INSERT INTO sending_feedback_events (provider_event_id,correlation_id,provider_occurred_at) VALUES ('feedback_event_ledger_test','corr_ledger_test',now());
		DELETE FROM messages WHERE id='msg_ledger_test';
	`); err != nil {
		t.Fatalf("seed and delete message: %v", err)
	}
	for table, column := range map[string]string{"sending_budget_reservations": "operation_id", "sending_feedback_correlations": "operation_id"} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE `+column+`='msg_ledger_test'`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s rows after message deletion=%d err=%v", table, count, err)
		}
	}
	var generation int64
	var removalPending bool
	if err := pool.QueryRow(ctx, `SELECT sync_generation,removal_pending FROM suppressions WHERE id='sup_ledger_test'`).Scan(&generation, &removalPending); err != nil || generation != 1 || removalPending {
		t.Fatalf("suppression safe defaults=(%d,%v) err=%v", generation, removalPending, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sending_budget_counters(scope,scope_id,day,daily_limit) VALUES('invalid_scope','x',CURRENT_DATE,1)`); err == nil {
		t.Fatal("invalid budget scope unexpectedly accepted")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id='usr_ledger_test'`); err != nil {
		t.Fatalf("delete synthetic account: %v", err)
	}
	for table, predicate := range map[string]string{
		"sending_provider_operations":          "operation_id='msg_ledger_test'",
		"sending_budget_reservations":          "operation_id='msg_ledger_test'",
		"sending_feedback_correlations":        "operation_id='msg_ledger_test'",
		"sending_feedback_recipients":          "correlation_id='corr_ledger_test'",
		"sending_feedback_events":              "correlation_id='corr_ledger_test'",
		"account_sending_control_events":       "id='control_event_ledger_test'",
		"sending_protection_notice_events":     "id='notice_ledger_test'",
		"sending_protection_notice_deliveries": "event_id='notice_ledger_test'",
	} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE `+predicate).Scan(&count); err != nil || count == 0 {
			t.Errorf("deletion-resistant %s rows=%d err=%v", table, count, err)
		}
	}
	for _, table := range []string{"account_sending_controls", "account_sending_outcomes_daily"} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE user_id='usr_ledger_test'`).Scan(&count); err != nil || count != 0 {
			t.Errorf("account-owned %s rows after account deletion=%d err=%v", table, count, err)
		}
	}
}
