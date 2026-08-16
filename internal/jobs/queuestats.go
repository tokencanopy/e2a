package jobs

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// queueStatsInterval is the gauge sampling cadence. 30s is fine-grained
// enough to catch a backlog forming and cheap enough (two GROUP BY reads of
// river_job) to run forever.
const queueStatsInterval = 30 * time.Second

// queueStatsStates are the live river_job states worth graphing. Terminal
// states (completed/cancelled/discarded) are history, not backlog, and are
// pruned by River anyway.
var queueStatsStates = []string{"available", "running", "retryable", "scheduled"}

// knownQueues is the fixed set the sampler reports on — every queue the shared
// client works (queues.go). Fixed so gauges can be ZERO-FILLED: a gauge that
// simply stops being set when its queue empties would freeze at its last
// value on most metric backends.
func knownQueues() []string {
	return []string{QueueOutbound, QueueInbound, QueueWebhook, QueueMaintenance, QueueNotify, QueueSenderIdentityV2, QueueDefault}
}

// QueueStatsMetrics is the narrow gauge surface the sampler sets. Satisfied
// by telemetry.Metrics; injectable so tests assert with a fake.
type QueueStatsMetrics interface {
	SetQueueDepth(queue, state string, n int)
	SetQueueOldestAge(queue string, seconds float64)
}

// noopQueueStats keeps a nil metrics dependency from panicking the sampler.
type noopQueueStats struct{}

func (noopQueueStats) SetQueueDepth(string, string, int) {}
func (noopQueueStats) SetQueueOldestAge(string, float64) {}

// QueueStatsArgs drives one gauge sampling pass.
type QueueStatsArgs struct{}

func (QueueStatsArgs) Kind() string { return "queue_stats_sample" }

// QueueStatsWorker samples river_job depth and oldest-runnable-age gauges per
// queue. Read-only and bounded (two single-round-trip aggregates over the
// indexed state column), so River's default JobTimeout is ample.
type QueueStatsWorker struct {
	river.WorkerDefaults[QueueStatsArgs]
	pool    *pgxpool.Pool
	metrics QueueStatsMetrics
}

// NewQueueStatsWorker builds the sampler. nil metrics degrades to a no-op.
func NewQueueStatsWorker(pool *pgxpool.Pool, metrics QueueStatsMetrics) *QueueStatsWorker {
	if metrics == nil {
		metrics = noopQueueStats{}
	}
	return &QueueStatsWorker{pool: pool, metrics: metrics}
}

func (w *QueueStatsWorker) Work(ctx context.Context, _ *river.Job[QueueStatsArgs]) error {
	// Best-effort: a failed sample logs and returns nil instead of feeding
	// River's retry loop — the next 30s periodic is the retry, and a DB
	// outage shouldn't error-spam the job table while gauges are frozen
	// anyway (Sample is all-or-nothing, so stale values stay consistent).
	if err := w.Sample(ctx); err != nil {
		log.Printf("[queue-stats] sample failed: %v", err)
	}
	return nil
}

// Sample runs one gauge pass: depth per (queue, state) and the oldest
// RUNNABLE (available, scheduled_at due) job age per queue. Every known
// queue × state pair is set on every pass — pairs absent from the query
// zero-fill, so gauges drop to 0 when a queue drains. Rows for queues outside
// the known set are ignored: they cannot be zero-filled once they empty, so
// reporting them would leave stuck gauges.
func (w *QueueStatsWorker) Sample(ctx context.Context) error {
	depths := make(map[string]map[string]int, len(knownQueues()))
	rows, err := w.pool.Query(ctx,
		`SELECT queue, state::text, count(*)
		   FROM river_job
		  WHERE state IN ('available','running','retryable','scheduled')
		  GROUP BY queue, state`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var queue, state string
		var n int
		if err := rows.Scan(&queue, &state, &n); err != nil {
			rows.Close()
			return err
		}
		if depths[queue] == nil {
			depths[queue] = make(map[string]int, len(queueStatsStates))
		}
		depths[queue][state] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// Oldest runnable age: only jobs a worker could claim right now.
	// An 'available' row with a future scheduled_at (snooze landing) is not
	// yet waiting on worker capacity, so it doesn't count.
	ages := make(map[string]float64, len(knownQueues()))
	rows, err = w.pool.Query(ctx,
		`SELECT queue, EXTRACT(EPOCH FROM (now() - min(scheduled_at)))
		   FROM river_job
		  WHERE state = 'available' AND scheduled_at <= now()
		  GROUP BY queue`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var queue string
		var age float64
		if err := rows.Scan(&queue, &age); err != nil {
			rows.Close()
			return err
		}
		ages[queue] = age
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, queue := range knownQueues() {
		for _, state := range queueStatsStates {
			w.metrics.SetQueueDepth(queue, state, depths[queue][state])
		}
		w.metrics.SetQueueOldestAge(queue, ages[queue])
	}
	return nil
}

// QueueStatsJobs contributes the periodic queue-gauge sampler to the shared
// client. Implements Registrar (same shape as sendramp.MaintenanceJobs).
type QueueStatsJobs struct {
	pool    *pgxpool.Pool
	metrics QueueStatsMetrics
}

// NewQueueStatsJobs builds the sampler registrar. nil metrics degrades to a
// no-op sampler rather than a nil-panic.
func NewQueueStatsJobs(pool *pgxpool.Pool, metrics QueueStatsMetrics) *QueueStatsJobs {
	return &QueueStatsJobs{pool: pool, metrics: metrics}
}

func (q *QueueStatsJobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, NewQueueStatsWorker(q.pool, q.metrics))
	return []*river.PeriodicJob{river.NewPeriodicJob(
		river.PeriodicInterval(queueStatsInterval),
		func() (river.JobArgs, *river.InsertOpts) {
			return QueueStatsArgs{}, &river.InsertOpts{Queue: QueueMaintenance}
		},
		// RunOnStart: gauges should exist moments after boot, not be absent
		// for the first interval.
		&river.PeriodicJobOpts{RunOnStart: true},
	)}
}
