package inboundprocess_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/inboundprocess"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// TestPruneProcessedIntake covers retention: an old processed row is pruned; a recent
// processed row and an accepted (unprocessed) row of any age are kept.
func TestPruneProcessedIntake(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	insert := func(id, msgID, hash string) {
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			_, e := store.InsertInboundIntakeTx(ctx, tx, id, "bot@prune.test", "a@b.test", "", "1.2.3.4", msgID, hash, []byte("raw"))
			return e
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	process := func(id, msgFK string) {
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			return store.MarkInboundIntakeProcessedTx(ctx, tx, id, msgFK)
		}); err != nil {
			t.Fatalf("process %s: %v", id, err)
		}
	}

	oldID := identity.NewInboundIntakeID()
	insert(oldID, "<old@b.test>", "h1")
	process(oldID, "msg_1")
	if _, err := pool.Exec(ctx, `UPDATE inbound_intake SET processed_at = now() - interval '100 hours' WHERE id=$1`, oldID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	recentID := identity.NewInboundIntakeID()
	insert(recentID, "<recent@b.test>", "h2")
	process(recentID, "msg_2")

	accID := identity.NewInboundIntakeID()
	insert(accID, "<acc@b.test>", "h3") // stays accepted

	n, err := store.PruneProcessedIntake(ctx, 72*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n < 1 {
		t.Fatalf("should prune the old processed row, got %d", n)
	}

	exists := func(id string) bool {
		var c int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM inbound_intake WHERE id=$1`, id).Scan(&c)
		return c > 0
	}
	if exists(oldID) {
		t.Error("old processed row should be pruned")
	}
	if !exists(recentID) {
		t.Error("recent processed row must be kept")
	}
	if !exists(accID) {
		t.Error("accepted (unprocessed) row must be kept regardless of age")
	}
}

// fakeEnq is a minimal jobs.Enqueuer that hands back an increasing job id without a
// live River client — enough to exercise ReconcilePending's stamp path.
type fakeEnq struct{ n int64 }

func (f *fakeEnq) Insert(_ context.Context, _ river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	f.n++
	return &rivertype.JobInsertResult{Job: &rivertype.JobRow{ID: f.n}}, nil
}

func (f *fakeEnq) InsertTx(_ context.Context, _ pgx.Tx, _ river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	f.n++
	return &rivertype.JobInsertResult{Job: &rivertype.JobRow{ID: f.n}}, nil
}

// InsertManyTx is unused by this test's fake — it only exercises single-job
// enqueue paths — but is required by the widened jobs.Enqueuer interface
// (see internal/jobs/jobs.go). Present to satisfy the interface only.
func (f *fakeEnq) InsertManyTx(_ context.Context, _ pgx.Tx, _ []river.InsertManyParams) ([]*rivertype.JobInsertResult, error) {
	panic("fakeEnq.InsertManyTx: not implemented in this test suite")
}

// TestReconcilePending covers the startup cutover: an accepted intake row with no job
// (a crash between insert and enqueue, or a pre-async row) gets a job stamped, and a
// re-run does not re-enqueue it (idempotent via the process_job_id IS NULL guard).
func TestReconcilePending(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	// Plant an accepted intake with NO job (stamp step skipped).
	id := identity.NewInboundIntakeID()
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := store.InsertInboundIntakeTx(ctx, tx, id, "bot@recon.test", "a@b.test", "", "1.2.3.4", "<recon@b.test>", "h", []byte("raw"))
		return e
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	j := inboundprocess.NewJobs(store)
	j.SetEnqueuer(&fakeEnq{})

	n, err := j.ReconcilePending(ctx, pool)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n < 1 {
		t.Fatalf("reconcile should enqueue at least the stranded row, got %d", n)
	}
	var jobID *int64
	if err := pool.QueryRow(ctx, `SELECT process_job_id FROM inbound_intake WHERE id=$1`, id).Scan(&jobID); err != nil {
		t.Fatalf("read job id: %v", err)
	}
	if jobID == nil {
		t.Fatal("reconcile must stamp a job id on the stranded row")
	}
	stamped := *jobID

	// Idempotent: a re-run must not re-enqueue this row (its job id is unchanged).
	if _, err := j.ReconcilePending(ctx, pool); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	var jobID2 *int64
	if err := pool.QueryRow(ctx, `SELECT process_job_id FROM inbound_intake WHERE id=$1`, id).Scan(&jobID2); err != nil {
		t.Fatalf("read job id 2: %v", err)
	}
	if jobID2 == nil || *jobID2 != stamped {
		t.Fatalf("re-run must not re-enqueue an already-stamped row: was %d, now %v", stamped, jobID2)
	}
}
