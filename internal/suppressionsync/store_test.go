package suppressionsync_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/suppressionsync"
	"github.com/tokencanopy/e2a/internal/testutil/testdb"
)

// TestUpsertAdvancesGenerationAndDefeatsStaleRemoval pins the race contract
// the provider-facing reconciliation (Task 11) will build on: a feedback
// upsert on an existing row bumps sync_generation and clears
// removal_pending, so a removal that observed the earlier generation
// deletes nothing.
func TestUpsertAdvancesGenerationAndDefeatsStaleRemoval(t *testing.T) {
	ctx := context.Background()
	pool := testdb.TestDB(t)
	const user = "usr_suppsync_1"
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, google_subject, account_class) VALUES ($1, 'suppsync@reviewer.test', 'google-suppsync', 'standard') ON CONFLICT (id) DO NOTHING`, user); err != nil {
		t.Fatal(err)
	}
	inTx := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var first, second suppressionsync.Upsert
	inTx(func(tx pgx.Tx) error {
		var err error
		first, err = suppressionsync.UpsertTx(ctx, tx, "supp_a", user, "bounce@example.test", "bounce:General", suppressionsync.SourceBounce, "msg_1")
		return err
	})
	if !first.Inserted || first.Generation != 1 {
		t.Fatalf("first upsert = %+v, want inserted at generation 1", first)
	}
	inTx(func(tx pgx.Tx) error {
		var err error
		second, err = suppressionsync.UpsertTx(ctx, tx, "supp_b", user, "bounce@example.test", "complaint", suppressionsync.SourceComplaint, "")
		return err
	})
	if second.Inserted || second.ID != first.ID || second.Generation != 2 {
		t.Fatalf("second upsert = %+v, want refresh of %s at generation 2", second, first.ID)
	}
	var reason, source string
	if err := pool.QueryRow(ctx, `SELECT reason, source FROM suppressions WHERE id = $1`, first.ID).Scan(&reason, &source); err != nil {
		t.Fatal(err)
	}
	if reason != "bounce:General" || source != "bounce" {
		t.Fatalf("refresh must keep the first evidence, got reason=%q source=%q", reason, source)
	}

	// A remover marks the row pending at generation 2; feedback re-proves
	// the address (generation 3, pending cleared); the stale removal at 2
	// deletes nothing and the address stays suppressed.
	var gen int64
	inTx(func(tx pgx.Tx) error {
		var found bool
		var err error
		gen, found, err = suppressionsync.MarkRemovalPendingTx(ctx, tx, user, "bounce@example.test")
		if err == nil && !found {
			t.Fatal("row must be found")
		}
		return err
	})
	if gen != 2 {
		t.Fatalf("pending generation = %d, want 2", gen)
	}
	inTx(func(tx pgx.Tx) error {
		up, err := suppressionsync.UpsertTx(ctx, tx, "supp_c", user, "bounce@example.test", "bounce:General", suppressionsync.SourceBounce, "")
		if err == nil && (up.Generation != 3 || up.Inserted) {
			t.Fatalf("racing upsert = %+v, want generation 3 refresh", up)
		}
		return err
	})
	inTx(func(tx pgx.Tx) error {
		removed, err := suppressionsync.CompleteRemovalTx(ctx, tx, user, "bounce@example.test", gen)
		if err == nil && removed {
			t.Fatal("a stale removal must delete nothing after a racing upsert")
		}
		return err
	})
	var pending bool
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*), bool_or(removal_pending) FROM suppressions WHERE user_id = $1 AND address = 'bounce@example.test'`, user).Scan(&count, &pending); err != nil {
		t.Fatal(err)
	}
	if count != 1 || pending {
		t.Fatalf("row count=%d pending=%v, want the row kept and not pending", count, pending)
	}

	// An uncontested removal at the current generation succeeds.
	inTx(func(tx pgx.Tx) error {
		g, _, err := suppressionsync.MarkRemovalPendingTx(ctx, tx, user, "bounce@example.test")
		if err != nil {
			return err
		}
		removed, err := suppressionsync.CompleteRemovalTx(ctx, tx, user, "bounce@example.test", g)
		if err == nil && !removed {
			t.Fatal("an uncontested removal must delete the row")
		}
		return err
	})
	if _, found, err := func() (int64, bool, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return 0, false, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		return suppressionsync.MarkRemovalPendingTx(ctx, tx, user, "bounce@example.test")
	}(); err != nil || found {
		t.Fatalf("row should be gone: found=%v err=%v", found, err)
	}
}
