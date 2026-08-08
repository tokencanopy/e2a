//go:build integration

package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func TestDeleteExpiredMessagesBoundsHighFanoutChildDetachPerTransaction(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "retention-high-fanout")

	var parentIndexDefinition string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_indexdef(indexrelid)
		   FROM pg_index
		  WHERE indexrelid = 'messages_thread_parent_id_idx'::regclass`,
	).Scan(&parentIndexDefinition); err != nil {
		t.Fatalf("read parent index: %v", err)
	}
	if !strings.Contains(parentIndexDefinition, "(thread_parent_id, id)") {
		t.Fatalf("parent index does not support bounded ordered scan: %s", parentIndexDefinition)
	}

	parent, err := store.CreateInboundMessage(
		ctx, "", agentID, "a@example.net", agentID,
		"<retention-high-fanout@example.net>", "Parent", "", "unread",
		nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{},
	)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	children := make([]*identity.Message, 0, 5)
	for i := 0; i < 5; i++ {
		var child *identity.Message
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			var createErr error
			child, createErr = store.CreateOutboundMessageThreadedTx(
				ctx, tx, parent.ID, agentID, []string{"a@example.net"},
				nil, nil, "Re", "reply", "smtp", "", "", nil, "sent",
				agentID, "relay",
			)
			return createErr
		}); err != nil {
			t.Fatalf("create child %d: %v", i, err)
		}
		children = append(children, child)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET deleted_at = now() - interval '31 days'
		  WHERE id = $1`,
		parent.ID,
	); err != nil {
		t.Fatalf("expire parent: %v", err)
	}

	installThreadDetachStatementAudit(t, ctx, pool)
	restoreParentBatch := identity.SetExpiredDeleteBatchForTest(1)
	defer restoreParentBatch()
	restoreChildBatch := identity.SetThreadChildDetachBatchForTest(2)
	defer restoreChildBatch()

	deleted, err := store.DeleteExpiredMessages(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredMessages: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}

	rows, err := pool.Query(ctx,
		`SELECT txid, affected
		   FROM thread_detach_statement_audit
		  WHERE affected > 0
		  ORDER BY txid`,
	)
	if err != nil {
		t.Fatalf("read detach audit: %v", err)
	}
	defer rows.Close()
	var txids []int64
	var affected []int
	for rows.Next() {
		var txid int64
		var count int
		if err := rows.Scan(&txid, &count); err != nil {
			t.Fatalf("scan detach audit: %v", err)
		}
		txids = append(txids, txid)
		affected = append(affected, count)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("detach audit rows: %v", err)
	}
	if len(affected) != 3 {
		t.Fatalf("detach statements = %v, want three batches [2 2 1]", affected)
	}
	for i, count := range affected {
		if count > 2 {
			t.Fatalf("detach statement %d affected %d rows, bound is 2", i, count)
		}
		if i > 0 && txids[i] == txids[i-1] {
			t.Fatalf("detach batches %d and %d shared transaction %d; want one bounded child batch per transaction", i-1, i, txids[i])
		}
	}
	if affected[0] != 2 || affected[1] != 2 || affected[2] != 1 {
		t.Fatalf("detach batches = %v, want [2 2 1]", affected)
	}

	var parentExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`,
		parent.ID,
	).Scan(&parentExists); err != nil {
		t.Fatalf("check parent: %v", err)
	}
	if parentExists {
		t.Fatal("expired parent survived after all child batches drained")
	}
	childIDs := make([]string, 0, len(children))
	for _, child := range children {
		childIDs = append(childIDs, child.ID)
	}
	var surviving, detachedChildren, originalThread int
	if err := pool.QueryRow(ctx,
		`SELECT count(*),
		        count(*) FILTER (WHERE thread_parent_id IS NULL),
		        count(*) FILTER (WHERE thread_id = $2)
		   FROM messages
		  WHERE id = ANY($1)`,
		childIDs, parent.ThreadID,
	).Scan(&surviving, &detachedChildren, &originalThread); err != nil {
		t.Fatalf("check children: %v", err)
	}
	if surviving != 5 || detachedChildren != 5 || originalThread != 5 {
		t.Fatalf(
			"surviving/detached/same-thread children = %d/%d/%d, want 5/5/5",
			surviving, detachedChildren, originalThread,
		)
	}
}

func TestPurgeMessageWaitsForLockedChildBeforeDeletingParent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "purge-locked-child")

	parent, err := store.CreateInboundMessage(
		ctx, "", agentID, "a@example.net", agentID,
		"<purge-locked-child@example.net>", "Parent", "", "unread",
		nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{},
	)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	var child *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var createErr error
		child, createErr = store.CreateOutboundMessageThreadedTx(
			ctx, tx, parent.ID, agentID, []string{"a@example.net"},
			nil, nil, "Re", "reply", "smtp", "", "", nil, "sent",
			agentID, "relay",
		)
		return createErr
	}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := store.SoftDeleteMessage(ctx, parent.ID, agentID); err != nil {
		t.Fatalf("trash parent: %v", err)
	}

	childLocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin child locker: %v", err)
	}
	defer childLocker.Rollback(context.Background())
	if _, err := childLocker.Exec(ctx,
		`SELECT id FROM messages WHERE id = $1 FOR UPDATE`,
		child.ID,
	); err != nil {
		t.Fatalf("lock child: %v", err)
	}

	purgeDone := make(chan error, 1)
	go func() {
		purgeDone <- store.PurgeMessage(ctx, parent.ID, agentID)
	}()

	waitForMessageRowLock(t, ctx, pool, parent.ID)
	select {
	case purgeErr := <-purgeDone:
		t.Fatalf("PurgeMessage returned while child remained locked: %v", purgeErr)
	default:
	}

	if err := childLocker.Commit(ctx); err != nil {
		t.Fatalf("release child lock: %v", err)
	}
	select {
	case purgeErr := <-purgeDone:
		if purgeErr != nil {
			t.Fatalf("PurgeMessage after child unlock: %v", purgeErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PurgeMessage did not finish after child lock released")
	}

	var parentExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`,
		parent.ID,
	).Scan(&parentExists); err != nil {
		t.Fatalf("check parent: %v", err)
	}
	if parentExists {
		t.Fatal("purged parent still exists")
	}
	var childParentID, childThreadID string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(thread_parent_id, ''), thread_id
		   FROM messages
		  WHERE id = $1`,
		child.ID,
	).Scan(&childParentID, &childThreadID); err != nil {
		t.Fatalf("check child: %v", err)
	}
	if childParentID != "" || childThreadID != child.ThreadID {
		t.Fatalf(
			"surviving child topology = parent %q thread %q, want empty / %q",
			childParentID, childThreadID, child.ThreadID,
		)
	}
}

func waitForMessageRowLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, messageID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin lock probe: %v", err)
		}
		_, lockErr := tx.Exec(ctx,
			`SELECT id FROM messages WHERE id = $1 FOR UPDATE NOWAIT`,
			messageID,
		)
		_ = tx.Rollback(ctx)
		var pgErr *pgconn.PgError
		if errors.As(lockErr, &pgErr) && pgErr.Code == "55P03" {
			return
		}
		if lockErr != nil {
			t.Fatalf("probe parent lock: %v", lockErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for PurgeMessage to lock parent")
}

func installThreadDetachStatementAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	// testutil gives this package its own derived database, so these stable
	// probe names cannot collide with another package's integration tests.
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS thread_detach_statement_audit_trigger ON messages`,
		`DROP FUNCTION IF EXISTS record_thread_detach_statement_audit()`,
		`DROP TABLE IF EXISTS thread_detach_statement_audit`,
		`CREATE TABLE thread_detach_statement_audit (
		   txid bigint NOT NULL,
		   affected integer NOT NULL
		 )`,
		`CREATE FUNCTION record_thread_detach_statement_audit()
		 RETURNS trigger
		 LANGUAGE plpgsql
		 AS $$
		 BEGIN
		   INSERT INTO thread_detach_statement_audit (txid, affected)
		   SELECT txid_current(), count(*)::integer
		     FROM old_rows AS old_row
		     JOIN new_rows AS new_row USING (id)
		    WHERE old_row.thread_parent_id IS DISTINCT FROM new_row.thread_parent_id;
		   RETURN NULL;
		 END
		 $$`,
		`CREATE TRIGGER thread_detach_statement_audit_trigger
		   AFTER UPDATE ON messages
		   REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
		   FOR EACH STATEMENT
		   EXECUTE FUNCTION record_thread_detach_statement_audit()`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("install detach audit %q: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS thread_detach_statement_audit_trigger ON messages`,
			`DROP FUNCTION IF EXISTS record_thread_detach_statement_audit()`,
			`DROP TABLE IF EXISTS thread_detach_statement_audit`,
		} {
			if _, err := pool.Exec(cleanupCtx, statement); err != nil {
				t.Errorf("cleanup detach audit %q: %v", statement, err)
			}
		}
	})
}
