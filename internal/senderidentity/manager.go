package senderidentity

import (
	"context"
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

// DeprovisionBeforeDelete makes the HTTP domain-delete success boundary mean
// that the provider identity is already confirmed absent. The database delete
// runs under the same per-domain mutation lock, so a current-version provision
// worker cannot recreate the identity between provider teardown and commit.
// If provider teardown fails (including an unowned identity), deleteFn is not
// called and the domain/DNS remain intact for a safe retry.
func (m *Manager) DeprovisionBeforeDelete(ctx context.Context, domain string, deleteFn func(context.Context) error) error {
	return m.store.WithSendingIdentityMutationLock(ctx, domain, func(lockedCtx context.Context) error {
		if err := m.provider.Deprovision(lockedCtx, domain); err != nil {
			return err
		}
		return deleteFn(lockedCtx)
	})
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
	river.AddWorker(w, &ProvisionWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts})
	river.AddWorker(w, &ReconcileWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts})
	river.AddWorker(w, &DeprovisionWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts})
	river.AddWorker(w, &SyncWorker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts})
	river.AddWorker(w, &ReconcileV2Worker{store: m.store, provider: m.provider, fire: m.fire, maxReconcileAttempt: m.cfg.MaxReconcileAttempts})
	river.AddWorker(w, &PostDrainAuditWorker{store: m.store, provider: m.provider, maxReconcileAttempt: m.cfg.MaxReconcileAttempts})
	river.AddWorker(w, &LegacyReapWorker{store: m.store, provider: m.provider, maxReconcileAttempt: m.cfg.MaxReconcileAttempts})
	river.AddWorker(w, &ReapWorker{store: m.store, provider: m.provider, maxReconcileAttempt: m.cfg.MaxReconcileAttempts})

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
				return ReapV2Args{}, &river.InsertOpts{Queue: jobs.QueueSenderIdentityV2}
			},
			&river.PeriodicJobOpts{RunOnStart: false},
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
// domain — for the ~24h completed-job retention window. Instead we always
// enqueue and rely on desired-state convergence: the current incarnation's
// BYODKIM + MAIL FROM replaces any prior provider state.
// Provider mutations are serialized per process and per domain across replicas,
// so concurrent duplicate enqueues are harmless.
func (m *Manager) EnqueueProvision(ctx context.Context, domain string) error {
	_, err := m.enq.Insert(ctx, SyncArgs{Domain: domain}, &river.InsertOpts{Queue: jobs.QueueSenderIdentityV2})
	return err
}

// EnqueueProvisionTx is the verify-time atomic-outbox variant.
func (m *Manager) EnqueueProvisionTx(ctx context.Context, tx pgx.Tx, domain string) error {
	_, err := m.enq.InsertTx(ctx, tx, SyncArgs{Domain: domain}, &river.InsertOpts{Queue: jobs.QueueSenderIdentityV2})
	return err
}

// EnqueueDeprovisionTx enqueues sending-identity teardown WITHIN the caller's
// delete transaction, so the job is committed atomically with the domain-row
// delete — it can never be lost if SES is unreachable at delete time.
func (m *Manager) EnqueueDeprovisionTx(ctx context.Context, tx pgx.Tx, domain string) error {
	_, err := m.enq.InsertTx(ctx, tx, SyncArgs{Domain: domain}, &river.InsertOpts{Queue: jobs.QueueSenderIdentityV2})
	return err
}
