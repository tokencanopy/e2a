package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// Compile-time proof the shared client is an Enqueuer (so business code can
// depend on the narrow interface and River satisfies it with no adapter).
var _ jobs.Enqueuer = (*jobs.Client)(nil)

// pingArgs is a trivial job used to prove the composition root assembles a
// working client end-to-end.
type pingArgs struct {
	ID string `json:"id"`
}

func (pingArgs) Kind() string { return "jobs_test_ping" }

type pingWorker struct {
	river.WorkerDefaults[pingArgs]
	got chan string
}

func (w *pingWorker) Work(_ context.Context, job *river.Job[pingArgs]) error {
	w.got <- job.Args.ID
	return nil
}

// pingRegistrar contributes the ping worker to the shared client.
type pingRegistrar struct{ w *pingWorker }

func (r pingRegistrar) RegisterJobs(workers *river.Workers) []*river.PeriodicJob {
	river.AddWorker(workers, r.w)
	return nil
}

// TestClient_EndToEnd proves New assembles a client from a Registrar, that the
// Enqueuer inserts a job, and that a registered worker runs it on its queue.
func TestClient_EndToEnd(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	got := make(chan string, 1)
	client, err := jobs.New(pool, jobs.Config{}, pingRegistrar{w: &pingWorker{got: got}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer client.Stop(ctx)

	// Enqueue through the narrow Enqueuer surface, onto the outbound queue.
	if _, err := client.Insert(ctx, pingArgs{ID: "hello"}, &river.InsertOpts{Queue: jobs.QueueOutbound}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	select {
	case id := <-got:
		if id != "hello" {
			t.Errorf("worked job id = %q, want hello", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the job to be worked")
	}
}

// TestConfig_Defaults confirms the per-queue worker pools fall back to sane
// non-zero sizes (a zero MaxWorkers would silently never work a queue).
func TestConfig_Defaults(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// New with a zero Config must build without error (defaults applied).
	if _, err := jobs.New(pool, jobs.Config{}); err != nil {
		t.Fatalf("New with zero config: %v", err)
	}
}

// TestClient_DefaultQueue covers the queue senderidentity actually uses in
// production (nil InsertOpts → QueueDefault) — the jobs foundation's CI-run test
// otherwise only exercises QueueOutbound.
func TestClient_DefaultQueue(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got := make(chan string, 1)
	client, err := jobs.New(pool, jobs.Config{}, pingRegistrar{w: &pingWorker{got: got}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer client.Stop(ctx)

	// nil opts → default queue (exactly how senderidentity enqueues).
	if _, err := client.Insert(ctx, pingArgs{ID: "default"}, nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	select {
	case id := <-got:
		if id != "default" {
			t.Errorf("worked id = %q, want default", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("default-queue job never ran — is QueueDefault in the client's Queues map?")
	}
}

// TestClient_InsertTxCommitRuns proves the outbox path: a job enqueued via
// InsertTx runs iff its transaction commits.
func TestClient_InsertTxCommitRuns(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got := make(chan string, 1)
	client, err := jobs.New(pool, jobs.Config{}, pingRegistrar{w: &pingWorker{got: got}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer client.Stop(ctx)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := client.InsertTx(ctx, tx, pingArgs{ID: "committed"}, nil); err != nil {
		t.Fatalf("InsertTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	select {
	case id := <-got:
		if id != "committed" {
			t.Errorf("worked id = %q, want committed", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("committed InsertTx job never ran")
	}
}

// TestClient_InsertTxRollbackDropsJob proves the other half of the outbox
// guarantee ("can never be lost" ⇒ also "never enqueued if the tx rolls back"):
// a rolled-back InsertTx leaves no job. A sentinel enqueued afterward bounds the
// wait — only the sentinel may ever be worked.
func TestClient_InsertTxRollbackDropsJob(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got := make(chan string, 2)
	client, err := jobs.New(pool, jobs.Config{}, pingRegistrar{w: &pingWorker{got: got}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer client.Stop(ctx)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := client.InsertTx(ctx, tx, pingArgs{ID: "rolledback"}, nil); err != nil {
		t.Fatalf("InsertTx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// Sentinel via a committed insert — if the rolled-back job were durable it
	// would race here, but it isn't, so only the sentinel is ever worked.
	if _, err := client.Insert(ctx, pingArgs{ID: "sentinel"}, nil); err != nil {
		t.Fatalf("Insert sentinel: %v", err)
	}
	select {
	case id := <-got:
		if id == "rolledback" {
			t.Fatal("a rolled-back InsertTx job was worked — the outbox atomicity is broken")
		}
		if id != "sentinel" {
			t.Errorf("worked id = %q, want sentinel", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sentinel job never ran")
	}
	// Nothing else should follow the sentinel.
	select {
	case id := <-got:
		t.Fatalf("unexpected second job worked: %q", id)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestClient_CancelTxIsAtomicAndMissingIsSuccess(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	client, err := jobs.New(pool, jobs.Config{}, pingRegistrar{w: &pingWorker{got: make(chan string, 1)}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	inserted, err := client.Insert(ctx, pingArgs{ID: "scheduled"}, &river.InsertOpts{
		ScheduledAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	jobID := inserted.Job.ID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin rollback tx: %v", err)
	}
	if err := client.CancelTx(ctx, tx, jobID); err != nil {
		t.Fatalf("CancelTx before rollback: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state::text FROM river_job WHERE id = $1`, jobID).Scan(&state); err != nil {
		t.Fatalf("read rolled-back job: %v", err)
	}
	if state != "scheduled" {
		t.Fatalf("state after rollback = %q, want scheduled", state)
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin commit tx: %v", err)
	}
	if err := client.CancelTx(ctx, tx, jobID); err != nil {
		t.Fatalf("CancelTx before commit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state::text FROM river_job WHERE id = $1`, jobID).Scan(&state); err != nil {
		t.Fatalf("read cancelled job: %v", err)
	}
	if state != "cancelled" {
		t.Fatalf("state after commit = %q, want cancelled", state)
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin missing tx: %v", err)
	}
	if err := client.CancelTx(ctx, tx, 1<<62); err != nil {
		t.Fatalf("CancelTx missing job: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback missing tx: %v", err)
	}
}
