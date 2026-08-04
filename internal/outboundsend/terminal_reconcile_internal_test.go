package outboundsend

import (
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/jobs"
)

func TestTerminalReconcilePeriodicConstructor_RoutesToMaintenance(t *testing.T) {
	if terminalReconcileInterval != time.Minute {
		t.Errorf("terminalReconcileInterval = %s, want %s", terminalReconcileInterval, time.Minute)
	}
	args, opts := terminalReconcilePeriodicConstructor()
	if opts == nil {
		t.Fatal("constructor returned nil InsertOpts")
	}
	if opts.Queue != jobs.QueueMaintenance {
		t.Errorf("periodic routed to queue %q, want %q", opts.Queue, jobs.QueueMaintenance)
	}
	if _, ok := args.(TerminalReconcileArgs); !ok {
		t.Errorf("constructor returned args of type %T, want TerminalReconcileArgs", args)
	}
	if got := args.Kind(); got != "outbound_terminal_reconcile" {
		t.Errorf("TerminalReconcileArgs.Kind() = %q, want outbound_terminal_reconcile", got)
	}
}

func TestSubmissionAnchor(t *testing.T) {
	accept := time.Date(2026, 8, 3, 22, 0, 0, 0, time.UTC)
	later := accept.Add(2 * time.Hour)
	earlier := accept.Add(-2 * time.Hour)

	tests := []struct {
		name                              string
		acceptedAt, scheduledAt, reviewed time.Time
		want                              time.Time
	}{
		{"ordinary send anchors at accept", accept, time.Time{}, time.Time{}, accept},
		{"hitl hold anchors at approve", accept, time.Time{}, later, later},
		{"scheduled send anchors at fire time", accept, later, time.Time{}, later},
		{"held and scheduled take the latest gate", accept, later, later.Add(time.Hour), later.Add(time.Hour)},
		{"a gate before accept is inert", accept, earlier, earlier, accept},
		{"zero accept with a hold still anchors at approve", time.Time{}, time.Time{}, later, later},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := submissionAnchor(tt.acceptedAt, tt.scheduledAt, tt.reviewed); !got.Equal(tt.want) {
				t.Errorf("submissionAnchor = %s, want %s", got, tt.want)
			}
		})
	}
}

// The reconciler and the send worker must agree on the baseline, or a message
// settled by the sweep is measured differently from one settled inline.
func TestTerminalCandidateSubmissionAnchor_MatchesWorkerDefinition(t *testing.T) {
	accept := time.Date(2026, 8, 3, 22, 0, 0, 0, time.UTC)
	reviewed := accept.Add(90 * time.Minute)
	c := terminalCandidate{acceptedAt: accept, reviewedAt: &reviewed}
	j := &SendJob{AcceptedAt: accept, ReviewedAt: reviewed}
	if got, want := c.submissionAnchor(), j.submissionAnchor(); !got.Equal(want) {
		t.Errorf("reconciler anchor = %s, worker anchor = %s; the two paths must agree", got, want)
	}
	if got := c.submissionAnchor(); !got.Equal(reviewed) {
		t.Errorf("anchor = %s, want the approve time %s", got, reviewed)
	}
}

// A nil scheduled_at/reviewed_at (the overwhelmingly common row) must not be
// dereferenced and must leave the accept anchor untouched.
func TestTerminalCandidateSubmissionAnchor_NilTimestamps(t *testing.T) {
	accept := time.Date(2026, 8, 3, 22, 0, 0, 0, time.UTC)
	c := terminalCandidate{acceptedAt: accept}
	if got := c.submissionAnchor(); !got.Equal(accept) {
		t.Errorf("anchor = %s, want %s for a row with no gates", got, accept)
	}
}
