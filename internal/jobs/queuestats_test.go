package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// queueStatsRecorder captures the last-set value per gauge key plus call
// counts, mirroring gauge semantics.
type queueStatsRecorder struct {
	depths     map[[2]string]int
	ages       map[string]float64
	depthCalls int
	ageCalls   int
}

func newQueueStatsRecorder() *queueStatsRecorder {
	return &queueStatsRecorder{depths: map[[2]string]int{}, ages: map[string]float64{}}
}

func (r *queueStatsRecorder) SetQueueDepth(queue, state string, n int) {
	r.depths[[2]string{queue, state}] = n
	r.depthCalls++
}

func (r *queueStatsRecorder) SetQueueOldestAge(queue string, seconds float64) {
	r.ages[queue] = seconds
	r.ageCalls++
}

// clearRiverJobs empties river_job: the testutil harness truncates only e2a
// tables, so rows leak between tests and would break the zero-fill and
// whole-table depth assertions.
func clearRiverJobs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM river_job`); err != nil {
		t.Fatalf("clear river_job: %v", err)
	}
}

// insertJob writes a synthetic river_job row directly — states like
// 'retryable' are impractical to reach through a live client.
func insertJob(t *testing.T, pool *pgxpool.Pool, queue, state string, scheduledAt time.Time) {
	t.Helper()
	finalized := "NULL"
	if state == "completed" || state == "cancelled" || state == "discarded" {
		finalized = "now()"
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO river_job (args, kind, max_attempts, priority, queue, scheduled_at, state, finalized_at)
		 VALUES ('{}', 'queue_stats_test_job', 25, 1, $1, $2, $3::river_job_state, `+finalized+`)`,
		queue, scheduledAt, state); err != nil {
		t.Fatalf("insert river_job (%s/%s): %v", queue, state, err)
	}
}

func TestQueueStatsWorker_SampleDepthsAgesAndZeroFill(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	clearRiverJobs(t, pool)

	now := time.Now().UTC()
	insertJob(t, pool, jobs.QueueOutbound, "available", now.Add(-2*time.Minute))
	insertJob(t, pool, jobs.QueueOutbound, "available", now.Add(-30*time.Second))
	insertJob(t, pool, jobs.QueueOutbound, "retryable", now.Add(time.Hour))
	insertJob(t, pool, jobs.QueueWebhook, "running", now)
	insertJob(t, pool, jobs.QueueNotify, "scheduled", now.Add(time.Hour))
	// Available but not yet due (snooze landing): counts toward depth, must
	// NOT count toward oldest runnable age.
	insertJob(t, pool, jobs.QueueInbound, "available", now.Add(time.Hour))
	// Terminal row: invisible to both gauges.
	insertJob(t, pool, jobs.QueueOutbound, "completed", now.Add(-time.Hour))

	rec := newQueueStatsRecorder()
	if err := jobs.NewQueueStatsWorker(pool, rec).Sample(ctx); err != nil {
		t.Fatalf("Sample: %v", err)
	}

	// 7 known queues × 4 states, every pair set every pass.
	if rec.depthCalls != 28 {
		t.Errorf("depth calls = %d, want 28 (7 queues × 4 states)", rec.depthCalls)
	}
	if rec.ageCalls != 7 {
		t.Errorf("age calls = %d, want 7 (one per known queue)", rec.ageCalls)
	}

	for _, tc := range []struct {
		queue, state string
		want         int
	}{
		{jobs.QueueOutbound, "available", 2},
		{jobs.QueueOutbound, "retryable", 1},
		{jobs.QueueOutbound, "running", 0}, // zero-filled
		{jobs.QueueWebhook, "running", 1},
		{jobs.QueueNotify, "scheduled", 1},
		{jobs.QueueInbound, "available", 1},
		{jobs.QueueMaintenance, "available", 0}, // zero-filled: no rows at all
		{jobs.QueueSenderIdentityV2, "available", 0},
		{jobs.QueueDefault, "available", 0}, // zero-filled: no rows at all
	} {
		got, ok := rec.depths[[2]string{tc.queue, tc.state}]
		if !ok {
			t.Errorf("SetQueueDepth(%s, %s) never called", tc.queue, tc.state)
			continue
		}
		if got != tc.want {
			t.Errorf("depth(%s, %s) = %d, want %d", tc.queue, tc.state, got, tc.want)
		}
	}

	// Oldest runnable age: the 2-minute-old available outbound job.
	if age := rec.ages[jobs.QueueOutbound]; age < 115 || age > 300 {
		t.Errorf("outbound oldest age = %.1fs, want ~120s", age)
	}
	// Not-yet-due available job must not register an age; empty queues get 0.
	for _, queue := range []string{jobs.QueueInbound, jobs.QueueWebhook, jobs.QueueMaintenance, jobs.QueueNotify, jobs.QueueSenderIdentityV2, jobs.QueueDefault} {
		age, ok := rec.ages[queue]
		if !ok {
			t.Errorf("SetQueueOldestAge(%s) never called", queue)
			continue
		}
		if age != 0 {
			t.Errorf("oldest age(%s) = %.1fs, want 0", queue, age)
		}
	}
}

// TestQueueStatsWorker_GaugesDropToZeroWhenQueueDrains pins the zero-fill
// contract across passes: a depth observed on one pass must be re-set to 0 on
// the next once the rows are gone.
func TestQueueStatsWorker_GaugesDropToZeroWhenQueueDrains(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	clearRiverJobs(t, pool)

	insertJob(t, pool, jobs.QueueOutbound, "available", time.Now().UTC().Add(-time.Minute))
	rec := newQueueStatsRecorder()
	w := jobs.NewQueueStatsWorker(pool, rec)
	if err := w.Sample(ctx); err != nil {
		t.Fatalf("first Sample: %v", err)
	}
	if got := rec.depths[[2]string{jobs.QueueOutbound, "available"}]; got != 1 {
		t.Fatalf("depth(outbound, available) = %d, want 1", got)
	}
	if rec.ages[jobs.QueueOutbound] <= 0 {
		t.Fatalf("outbound oldest age = %.1f, want > 0", rec.ages[jobs.QueueOutbound])
	}

	if _, err := pool.Exec(ctx, `DELETE FROM river_job WHERE kind='queue_stats_test_job'`); err != nil {
		t.Fatalf("drain queue: %v", err)
	}
	if err := w.Sample(ctx); err != nil {
		t.Fatalf("second Sample: %v", err)
	}
	if got := rec.depths[[2]string{jobs.QueueOutbound, "available"}]; got != 0 {
		t.Errorf("after drain: depth(outbound, available) = %d, want 0", got)
	}
	if got := rec.ages[jobs.QueueOutbound]; got != 0 {
		t.Errorf("after drain: outbound oldest age = %.1f, want 0", got)
	}
}

// TestQueueStatsJobs_RegistersPeriodicSampler proves the registrar contributes
// the worker plus exactly one periodic job (nil metrics must be tolerated).
func TestQueueStatsJobs_RegistersPeriodicSampler(t *testing.T) {
	periodics := jobs.NewQueueStatsJobs(nil, nil).RegisterJobs(river.NewWorkers())
	if len(periodics) != 1 {
		t.Fatalf("RegisterJobs periodics = %d, want 1", len(periodics))
	}
}

func TestQueueStatsWorker_WorkIsBestEffort(t *testing.T) {
	pool := testutil.TestDB(t)

	// Healthy pass: Work delegates to Sample and reports success.
	w := jobs.NewQueueStatsWorker(pool, nil) // nil metrics must degrade to no-op, not panic
	if err := w.Work(context.Background(), &river.Job[jobs.QueueStatsArgs]{}); err != nil {
		t.Fatalf("Work with healthy DB: %v", err)
	}

	// Failed pass: Work must swallow the error (log-only) instead of feeding
	// River's retry loop — the 30s periodic IS the retry, and a DB outage
	// must not error-spam river_job while gauges are frozen anyway.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Work(cancelled, &river.Job[jobs.QueueStatsArgs]{}); err != nil {
		t.Fatalf("Work with failing sample = %v, want nil (best-effort)", err)
	}

	// The underlying Sample DOES surface the error — tests and callers that
	// want the failure signal read it there, not from Work.
	if err := w.Sample(cancelled); err == nil {
		t.Fatal("Sample with cancelled context = nil, want error")
	}
}
