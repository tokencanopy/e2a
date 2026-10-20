package sendramp

import (
	"context"
	"time"

	"github.com/riverqueue/river"
	"github.com/tokencanopy/e2a/internal/jobs"
)

const maintenanceInterval = 24 * time.Hour

func (s *Store) Sweep(ctx context.Context, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM sending_ramp_reservations WHERE state IN ('confirmed','released') AND updated_at < $1`, now.Add(-7*24*time.Hour)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM domain_send_counters c
		 WHERE c.day < $1
		   AND NOT EXISTS (
		       SELECT 1 FROM sending_ramp_reservations r
		        WHERE r.user_id=c.user_id AND r.domain=c.domain AND r.day=c.day AND r.state='reserved'
		   )`, utcDay(now).AddDate(0, 0, -35)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return s.sweepAccounts(ctx, now)
}

type MaintenanceArgs struct{}

func (MaintenanceArgs) Kind() string { return "sending_ramp_maintenance" }

type MaintenanceWorker struct {
	river.WorkerDefaults[MaintenanceArgs]
	store *Store
}

func NewMaintenanceWorker(store *Store) *MaintenanceWorker {
	return &MaintenanceWorker{store: store}
}

func (w *MaintenanceWorker) Work(ctx context.Context, _ *river.Job[MaintenanceArgs]) error {
	return w.store.Sweep(ctx, time.Now().UTC())
}

type MaintenanceJobs struct{ store *Store }

func NewMaintenanceJobs(store *Store) *MaintenanceJobs { return &MaintenanceJobs{store: store} }

func (m *MaintenanceJobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, NewMaintenanceWorker(m.store))
	return []*river.PeriodicJob{river.NewPeriodicJob(
		river.PeriodicInterval(maintenanceInterval),
		func() (river.JobArgs, *river.InsertOpts) {
			return MaintenanceArgs{}, &river.InsertOpts{Queue: jobs.QueueMaintenance}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	)}
}
