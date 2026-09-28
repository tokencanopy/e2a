package sendingpolicy

import (
	"context"
	"log"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
)

// feedbackMaintenanceInterval paces the retention pass. Nothing here is
// urgent: expiries are stamped days ahead, and the detector window is
// measured in days.
const feedbackMaintenanceInterval = time.Hour

// FeedbackMaintenanceArgs is the periodic retention job.
type FeedbackMaintenanceArgs struct{}

func (FeedbackMaintenanceArgs) Kind() string { return "sending_feedback_maintenance" }

// FeedbackMaintenanceWorker runs one retention pass over feedback provenance
// and daily outcome aggregates.
type FeedbackMaintenanceWorker struct {
	river.WorkerDefaults[FeedbackMaintenanceArgs]
	module *Module
}

// NewFeedbackMaintenanceWorker builds the retention worker over a module.
// Exported so the janitor — the one component here that DELETES evidence —
// can be driven directly by a test rather than only through River.
func NewFeedbackMaintenanceWorker(module *Module) *FeedbackMaintenanceWorker {
	return &FeedbackMaintenanceWorker{module: module}
}

func (w *FeedbackMaintenanceWorker) Work(ctx context.Context, _ *river.Job[FeedbackMaintenanceArgs]) error {
	// The EFFECTIVE policy, not the config file: on a database-source
	// deployment an operator who widened the detector window would
	// otherwise get a janitor that keeps deleting daily outcomes at the old
	// width, silently shrinking the evidence the detector sums.
	window, err := w.module.EffectiveDetectorWindowDays(ctx)
	if err != nil {
		return err
	}
	st, err := w.module.GCFeedback(ctx, w.module.now(), window)
	if err != nil {
		return err
	}
	if st.Events+st.Recipients+st.Correlations+st.Outcomes > 0 {
		log.Printf("[sendingpolicy:feedback-gc] removed events=%d recipients=%d correlations=%d daily_outcomes=%d",
			st.Events, st.Recipients, st.Correlations, st.Outcomes)
	}
	return nil
}

// feedbackReconcileInterval paces the retention reconciliation: daily is
// ample for a 30-day horizon. It also runs on start, because an interval
// periodic's first tick is one interval after process start and a
// deployment that rolls more often than daily would otherwise never run it.
const feedbackReconcileInterval = 24 * time.Hour

// FeedbackReconcileArgs is the periodic retention backstop.
type FeedbackReconcileArgs struct{}

func (FeedbackReconcileArgs) Kind() string { return "sending_feedback_reconcile" }

// FeedbackReconcileWorker stamps the post-deletion horizon on provenance of
// accounts that are gone but were never stamped, and sweeps orphaned
// events. See Module.ReconcileFeedbackRetention.
type FeedbackReconcileWorker struct {
	river.WorkerDefaults[FeedbackReconcileArgs]
	module *Module
}

// NewFeedbackReconcileWorker builds the reconcile worker over a module.
func NewFeedbackReconcileWorker(module *Module) *FeedbackReconcileWorker {
	return &FeedbackReconcileWorker{module: module}
}

func (w *FeedbackReconcileWorker) Work(ctx context.Context, _ *river.Job[FeedbackReconcileArgs]) error {
	// The same effective-policy horizon the purge seal stamps with.
	retention, err := w.module.EffectiveFeedbackRetention(ctx)
	if err != nil {
		return err
	}
	st, err := w.module.ReconcileFeedbackRetention(ctx, w.module.now(), retention)
	if err != nil {
		return err
	}
	if st.StampedCorrelations+st.StampedEvents+st.OrphanEvents > 0 {
		log.Printf("[sendingpolicy:feedback-reconcile] stamped correlations=%d events=%d; removed orphan events=%d",
			st.StampedCorrelations, st.StampedEvents, st.OrphanEvents)
	}
	return nil
}

// MaintenanceJobs registers the feedback retention periodics. Implements
// jobs.Registrar.
type MaintenanceJobs struct{ module *Module }

// NewMaintenanceJobs builds the registrar over the gate's module.
func NewMaintenanceJobs(module *Module) *MaintenanceJobs { return &MaintenanceJobs{module: module} }

func (m *MaintenanceJobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, &FeedbackMaintenanceWorker{module: m.module})
	river.AddWorker(w, &FeedbackReconcileWorker{module: m.module})
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(feedbackMaintenanceInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return FeedbackMaintenanceArgs{}, &river.InsertOpts{Queue: jobs.QueueMaintenance}
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
		river.NewPeriodicJob(
			river.PeriodicInterval(feedbackReconcileInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return FeedbackReconcileArgs{}, &river.InsertOpts{Queue: jobs.QueueMaintenance}
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}
}
