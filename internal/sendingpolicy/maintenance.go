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

// MaintenanceJobs registers the feedback retention periodic. Implements
// jobs.Registrar.
type MaintenanceJobs struct{ module *Module }

// NewMaintenanceJobs builds the registrar over the gate's module.
func NewMaintenanceJobs(module *Module) *MaintenanceJobs { return &MaintenanceJobs{module: module} }

func (m *MaintenanceJobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, &FeedbackMaintenanceWorker{module: m.module})
	return []*river.PeriodicJob{river.NewPeriodicJob(
		river.PeriodicInterval(feedbackMaintenanceInterval),
		func() (river.JobArgs, *river.InsertOpts) {
			return FeedbackMaintenanceArgs{}, &river.InsertOpts{Queue: jobs.QueueMaintenance}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	)}
}
