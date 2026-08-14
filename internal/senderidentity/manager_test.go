package senderidentity

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/jobs"
)

// recordingEnqueuer captures job inserts so the compat-mode kind/queue
// selection can be asserted without a live River client.
type recordingEnqueuer struct {
	inserts []recordedInsert
}

type recordedInsert struct {
	kind  string
	queue string
}

func (r *recordingEnqueuer) record(args river.JobArgs, opts *river.InsertOpts) {
	queue := ""
	if opts != nil {
		queue = opts.Queue
	}
	r.inserts = append(r.inserts, recordedInsert{kind: args.Kind(), queue: queue})
}

func (r *recordingEnqueuer) Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	r.record(args, opts)
	return &rivertype.JobInsertResult{}, nil
}

func (r *recordingEnqueuer) InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	r.record(args, opts)
	return &rivertype.JobInsertResult{}, nil
}

// TestManagerEnqueueKindsFollowCompatMode pins the two-phase blue/green
// rollout mechanism (second-review blocker: rollback was not mechanically
// safe). Phase 1 runs with LegacyJobCompat=true: every mutation is enqueued
// as a LEGACY kind on the default queue, which the still-deployed old binary
// can consume — so rolling phase 1 back strands no teardown work. Phase 2
// (a config-only flip to false) switches producers to the v2 lane; its
// rollback target is the phase-1 binary, which consumes v2. The critical
// invariant: teardown mutations are never enqueued on a lane the deploy's
// rollback target cannot consume.
func TestManagerEnqueueKindsFollowCompatMode(t *testing.T) {
	t.Run("default mode produces v2 kinds on the v2 queue", func(t *testing.T) {
		enq := &recordingEnqueuer{}
		m := NewManager(newFakeStore(), NewFakeProvider(), nil, Config{})
		m.SetEnqueuer(enq)

		if err := m.EnqueueProvision(context.Background(), "acme.com"); err != nil {
			t.Fatalf("EnqueueProvision: %v", err)
		}
		if err := m.EnqueueDeprovisionTx(context.Background(), nil, "acme.com"); err != nil {
			t.Fatalf("EnqueueDeprovisionTx: %v", err)
		}
		want := []recordedInsert{
			{kind: SyncArgs{}.Kind(), queue: jobs.QueueSenderIdentityV2},
			{kind: SyncArgs{}.Kind(), queue: jobs.QueueSenderIdentityV2},
		}
		if len(enq.inserts) != 2 || enq.inserts[0] != want[0] || enq.inserts[1] != want[1] {
			t.Fatalf("inserts = %+v, want %+v", enq.inserts, want)
		}
	})

	t.Run("legacy compat mode produces legacy kinds on the default queue", func(t *testing.T) {
		enq := &recordingEnqueuer{}
		m := NewManager(newFakeStore(), NewFakeProvider(), nil, Config{LegacyJobCompat: true})
		m.SetEnqueuer(enq)

		if err := m.EnqueueProvision(context.Background(), "acme.com"); err != nil {
			t.Fatalf("EnqueueProvision: %v", err)
		}
		if err := m.EnqueueProvisionTx(context.Background(), nil, "acme.com"); err != nil {
			t.Fatalf("EnqueueProvisionTx: %v", err)
		}
		if err := m.EnqueueDeprovisionTx(context.Background(), nil, "acme.com"); err != nil {
			t.Fatalf("EnqueueDeprovisionTx: %v", err)
		}
		want := []recordedInsert{
			{kind: ProvisionArgs{}.Kind(), queue: ""},
			{kind: ProvisionArgs{}.Kind(), queue: ""},
			{kind: DeprovisionArgs{}.Kind(), queue: ""},
		}
		if len(enq.inserts) != 3 {
			t.Fatalf("inserts = %+v, want 3", enq.inserts)
		}
		for i, w := range want {
			if enq.inserts[i] != w {
				t.Fatalf("insert[%d] = %+v, want %+v", i, enq.inserts[i], w)
			}
		}
	})
}

// TestReconcileInsertCompatKinds pins the poll lane the same way: in compat
// mode the fresh reconcile budget must be a legacy kind on the default queue
// (the old binary ignores the unknown incarnation field and polls with its
// old semantics — today's production behavior), so a phase-1 rollback cannot
// strand the poller either.
func TestReconcileInsertCompatKinds(t *testing.T) {
	args, opts := reconcileInsert("acme.com", "inc-1", 12, false)
	if args.Kind() != (ReconcileV2Args{}).Kind() || opts.Queue != jobs.QueueSenderIdentityV2 || opts.MaxAttempts != 12 {
		t.Fatalf("v2 mode: got kind=%s queue=%q maxAttempts=%d", args.Kind(), opts.Queue, opts.MaxAttempts)
	}
	legacyArgs, legacyOpts := reconcileInsert("acme.com", "inc-1", 12, true)
	if legacyArgs.Kind() != (ReconcileArgs{}).Kind() || legacyOpts.Queue != "" || legacyOpts.MaxAttempts != 12 {
		t.Fatalf("compat mode: got kind=%s queue=%q maxAttempts=%d", legacyArgs.Kind(), legacyOpts.Queue, legacyOpts.MaxAttempts)
	}
	if got := legacyArgs.(ReconcileArgs); got.Incarnation != "inc-1" {
		t.Fatalf("compat reconcile must carry the incarnation for the new binary, got %+v", got)
	}
}
