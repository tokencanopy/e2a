// Package contactdue owns the outreach wake-up: it finds engagements whose
// next_action_at has passed and emits contact.due so an agent that is not
// running gets woken.
//
// This is deliberately NOT part of the janitor. Everything the janitor does is
// pruning — expired messages, purged agents, dead sessions — and this is a
// scheduled product event with user-visible latency. Folding it in would have
// meant inheriting an hourly cadence chosen for TTL cleanup, sharing one error
// list and retry policy across two very different severities ("cleanup was
// skipped" vs "an agent was not woken"), and reporting through pruning metrics.
// It gets its own queue lane, interval, and metrics for the same reason every
// other domain here does.
package contactdue

import (
	"context"
	"log"
	"time"

	"github.com/riverqueue/river"
	"github.com/tokencanopy/e2a/internal/jobs"
)

// SweepInterval is how often due engagements are checked.
//
// Minutes, not the janitor's hour: this bounds how late a wake-up can be. A
// five-day follow-up cadence tolerates a few minutes of slack and would
// tolerate an hour, but the latency should be a decision rather than a
// side effect of which sweep the code happened to live in.
const SweepInterval = 5 * time.Minute

// Batch bounds one sweep. Anything not claimed is picked up next pass, so a
// campaign coming due all at once spreads across runs instead of flooding a
// subscriber in a single burst.
const Batch = 200

// BatchPublisher claims schedules and persists their durable events atomically.
// attempted is the number of schedules affected by a failed transaction (zero
// when the failure happened before the claim).
type BatchPublisher interface {
	PublishDueBatch(ctx context.Context, now time.Time, limit int) (attempted int, err error)
}

// Metrics reports sweep outcomes. Separate from the janitor's row-deletion
// counters because nothing here is a deletion.
type Metrics interface {
	ContactDuePublished(count int)
	ContactDueFailed(count int)
}

type noopMetrics struct{}

func (noopMetrics) ContactDuePublished(int) {}
func (noopMetrics) ContactDueFailed(int)    {}

// Sweeper is the unit of work. Exported and directly callable so an integration
// test can drive a real sweep without waiting on a periodic schedule.
type Sweeper struct {
	publisher BatchPublisher
	metrics   Metrics
	now       func() time.Time
}

// NewSweeper builds the sweeper. metrics may be nil.
func NewSweeper(p BatchPublisher, m Metrics) *Sweeper {
	if m == nil {
		m = noopMetrics{}
	}
	return &Sweeper{publisher: p, metrics: m, now: func() time.Time { return time.Now().UTC() }}
}

// Sweep atomically claims one batch and writes its wake-ups to the durable
// outbox. Subscriber delivery is decoupled per event after commit, so one
// unreachable endpoint cannot block the batch. A database/outbox error aborts
// the transaction and is returned to River for retry.
func (s *Sweeper) Sweep(ctx context.Context) (published int, err error) {
	attempted, err := s.publisher.PublishDueBatch(ctx, s.now(), Batch)
	if err != nil {
		failed := attempted
		if failed == 0 {
			failed = 1
		}
		s.metrics.ContactDueFailed(failed)
		log.Printf("[contact.due] atomic publish failed attempted=%d: %v", attempted, err)
		return 0, err
	}
	published = attempted
	if published > 0 {
		s.metrics.ContactDuePublished(published)
		log.Printf("[contact.due] published %d wake-up(s)", published)
	}
	return published, nil
}

// SweepArgs drives the periodic. No fields — each run claims the next batch.
type SweepArgs struct{}

// Kind is the River job kind.
func (SweepArgs) Kind() string { return "contact_due_sweep" }

// InsertOpts routes the sweep to the maintenance lane: it is periodic
// background work and must never compete with customer sends or webhook
// delivery for worker slots.
func (SweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: jobs.QueueMaintenance}
}

// Worker runs one sweep per fire.
type Worker struct {
	river.WorkerDefaults[SweepArgs]
	sweeper *Sweeper
}

// NewWorker builds the River worker.
func NewWorker(s *Sweeper) *Worker { return &Worker{sweeper: s} }

// Work executes one sweep.
func (w *Worker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	_, err := w.sweeper.Sweep(ctx)
	return err
}

// Jobs registers the worker and its periodic schedule.
type Jobs struct{ sweeper *Sweeper }

// NewJobs builds the registration bundle.
func NewJobs(s *Sweeper) *Jobs { return &Jobs{sweeper: s} }

// RegisterJobs adds the sweep worker and its periodic.
//
// RunOnStart is true: a deploy or restart should pick up anything that came due
// while the process was down, rather than leaving agents unwoken until the next
// interval. The claim is idempotent per schedule, so an extra run is harmless.
func (j *Jobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, NewWorker(j.sweeper))
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(SweepInterval),
			func() (river.JobArgs, *river.InsertOpts) { return SweepArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}
}
