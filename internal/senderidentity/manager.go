package senderidentity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
)

// defaultReaperInterval is how often the orphan-identity backstop sweeps.
// Hourly is plenty — the primary teardown is the transactional deprovision
// job; this only catches lost-job edge cases.
const defaultReaperInterval = time.Hour

// Config tunes the Manager. Zero values get sane defaults.
type Config struct {
	// MaxReconcileAttempts bounds the pending→failed TTL. 0 → default.
	MaxReconcileAttempts int
	// ReaperInterval overrides the orphan-sweep cadence. 0 → default.
	ReaperInterval time.Duration
	// LegacyJobCompat is phase 1 of the two-phase blue/green job-lane rollout
	// (config sender_identity.legacy_job_compat).
	// When true, mutation and reconcile jobs are PRODUCED as the legacy kinds
	// on the default queue — consumable by the previous release — while this
	// binary still CONSUMES both lanes. Deploy this release with the flag on;
	// once it is the stable rollback target, flip the flag off (a config-only
	// deploy) to switch producers to the versioned v2 lane. A rollback of
	// that second deploy lands on a binary that consumes v2, so nothing
	// strands. This only makes the River lanes rollback-compatible: operators
	// must still freeze sender-identity mutations while a pre-ownership worker
	// overlaps or is a possible rollback target (see the deployment design).
	// Default false: single-instance/self-host deployments have no blue/green
	// overlap and want the v2 semantics immediately.
	LegacyJobCompat bool
}

// Manager owns the sender-identity job lifecycle on the SHARED River client
// (internal/jobs), instead of a private client. It is a jobs.Registrar — it
// contributes rollout-compatible legacy workers, v2 sync/reconcile workers, and
// the periodic managed-identity reaper — plus the app's enqueue entry point: EnqueueProvision on domain verify,
// EnqueueDeprovisionTx in the domain-delete tx. The shared client is injected via
// SetEnqueuer after jobs.New has built it (which needs this Manager as a
// Registrar first — the standard two-phase wiring).
type Manager struct {
	store    Store
	provider Provider
	fire     EventFirer
	cfg      Config
	enq      jobs.Enqueuer
}

// TryDeprovisionNow is the post-commit best-effort convergence attempt for a
// just-deleted domain: the delete transaction has already committed the row
// delete plus the durable teardown job, so the provider identity is usually
// confirmed absent before the HTTP response returns — but a failure here is
// for the caller to LOG, never to propagate to the client (the committed job
// and the hourly reaper are the guarantee). It runs the same desired-state
// convergence as the workers: the absent row converges to provider absence,
// ErrIdentityNotOwned is tolerated (a foreign identity is not e2a's to
// delete) but returned as confirmed=false so callers never claim provider
// absence or release DNS. The ledger tombstone is deliberately retained for the
// post-drain audit to finalize (mixed-version late-create repair). The caller
// bounds the wait via ctx; the mutation gate honors cancellation.
func (m *Manager) TryDeprovisionNow(ctx context.Context, domain string) (confirmed bool, err error) {
	confirmed, err = syncProviderIdentity(ctx, domain, m.store, m.provider, m.fire, m.cfg.MaxReconcileAttempts, false, false, m.cfg.LegacyJobCompat)
	if errors.Is(err, ErrIdentityNotOwned) {
		// The synchronous DELETE response exposes this as manual_review, while
		// durable workers must keep retrying/alerting the same condition.
		return false, nil
	}
	return confirmed, err
}

// NewManager builds the manager with its dependencies. It does NOT build a River
// client — call jobs.New with this Manager as a Registrar, then SetEnqueuer with
// the resulting client. fire may be nil (no events).
func NewManager(store Store, provider Provider, fire EventFirer, cfg Config) *Manager {
	return &Manager{store: store, provider: provider, fire: fire, cfg: cfg}
}

// SetEnqueuer injects the shared client so the Enqueue* methods can insert jobs.
// Must be called (once, at startup) before EnqueueProvision/EnqueueDeprovisionTx.
func (m *Manager) SetEnqueuer(e jobs.Enqueuer) { m.enq = e }

// RegisterJobs adds both legacy drain workers and rollout-safe v2 workers to the
// shared client, and returns the v2 periodic reaper. Implements jobs.Registrar. The workers run
// on the default queue (nil InsertOpts.Queue), preserving prior behavior.
func (m *Manager) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, &ProvisionWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts, legacyJobs: m.cfg.LegacyJobCompat})
	river.AddWorker(w, &ReconcileWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts, legacyJobs: m.cfg.LegacyJobCompat})
	river.AddWorker(w, &DeprovisionWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts, legacyJobs: m.cfg.LegacyJobCompat})
	river.AddWorker(w, &SyncWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts, legacyJobs: m.cfg.LegacyJobCompat})
	river.AddWorker(w, &ReconcileV2Worker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts, legacyJobs: m.cfg.LegacyJobCompat})
	river.AddWorker(w, &PostDrainAuditWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts, legacyJobs: m.cfg.LegacyJobCompat})
	river.AddWorker(w, &LegacyReapWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts, legacyJobs: m.cfg.LegacyJobCompat})
	river.AddWorker(w, &ReapWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts, legacyJobs: m.cfg.LegacyJobCompat})

	reaperInterval := m.cfg.ReaperInterval
	if reaperInterval <= 0 {
		reaperInterval = defaultReaperInterval
	}
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(reaperInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				// Route to the versioned sender-identity lane so the old blue/green
				// slot cannot claim an unknown v2 kind. No UniqueOpts: River's scheduler
				// already inserts at most one per interval, and a completed reap
				// must not dedup-block the next scheduled run (River can't drop
				// `completed` from a unique state set). The reaper is idempotent
				// anyway.
				return ReapV2Args{SweepID: time.Now().UnixNano()}, &river.InsertOpts{Queue: jobs.QueueSenderIdentityV2}
			},
			// RunOnStart: the reaper is the version-agnostic convergence path for
			// the ledger, so the first sweep after any (re)deploy — including a
			// re-upgrade after a rollback stranded v2 jobs on the unconsumed
			// queue — repairs the backlog within minutes instead of waiting up
			// to an hour. Idempotent; a startup sweep is cheap.
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}
}

// EnqueueProvision schedules sending-identity provisioning for a domain
// (called when a domain becomes verified, or on a forced re-check via
// POST /domains/{domain}/verify).
//
// Intentionally NOT unique: River's job uniqueness can't drop `completed` from
// its state set (only `retryable` is safely removable), so a completed job
// would block a legitimate re-provision — e.g. POST /verify retrying a `failed`
// domain — for the ~24h completed-job retention window. (Where dedupe IS
// wanted despite that constraint, scope uniqueness in time instead — see
// PostDrainAuditArgs.Window.) Instead we always
// enqueue and rely on desired-state convergence: the current incarnation's
// BYODKIM + MAIL FROM replaces any prior provider state.
// Provider mutations are serialized per process and per domain across replicas,
// so concurrent duplicate enqueues are harmless.
func (m *Manager) EnqueueProvision(ctx context.Context, domain string) error {
	args, opts := m.provisionInsert(domain)
	_, err := m.enq.Insert(ctx, args, opts)
	return err
}

// EnqueueProvisionTx is the verify-time atomic-outbox variant.
func (m *Manager) EnqueueProvisionTx(ctx context.Context, tx pgx.Tx, domain string) error {
	args, opts := m.provisionInsert(domain)
	_, err := m.enq.InsertTx(ctx, tx, args, opts)
	return err
}

// EnqueueDeprovisionTx enqueues sending-identity teardown WITHIN the caller's
// delete transaction, so the job is committed atomically with the domain-row
// delete — it can never be lost if SES is unreachable at delete time.
func (m *Manager) EnqueueDeprovisionTx(ctx context.Context, tx pgx.Tx, domain string) error {
	args, opts := m.deprovisionInsert(domain)
	_, err := m.enq.InsertTx(ctx, tx, args, opts)
	return err
}

// provisionInsert/deprovisionInsert choose the job lane per LegacyJobCompat
// (see Config): legacy kinds on the default queue keep the previous release
// able to consume this binary's mutations during phase 1 of the rollout.
func (m *Manager) provisionInsert(domain string) (river.JobArgs, *river.InsertOpts) {
	if m.cfg.LegacyJobCompat {
		return ProvisionArgs{Domain: domain}, &river.InsertOpts{}
	}
	return SyncArgs{Domain: domain}, &river.InsertOpts{Queue: jobs.QueueSenderIdentityV2}
}

func (m *Manager) deprovisionInsert(domain string) (river.JobArgs, *river.InsertOpts) {
	if m.cfg.LegacyJobCompat {
		return DeprovisionArgs{Domain: domain}, &river.InsertOpts{}
	}
	return SyncArgs{Domain: domain}, &river.InsertOpts{Queue: jobs.QueueSenderIdentityV2}
}
