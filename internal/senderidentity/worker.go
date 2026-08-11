package senderidentity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
)

// DefaultMaxReconcileAttempts bounds how long a domain may sit in `pending`
// before the reconciler gives up and marks it `failed` (the design's "no
// infinite poll" TTL). The wall-clock TTL is the sum of River's retry
// backoffs across this many attempts.
const DefaultMaxReconcileAttempts = 12

// postDrainConvergenceDelay is longer than the deployment's five-minute
// old-slot drain bound. A v2 sweep after this delay repairs either direction
// of a mixed-version race: a late legacy delete of a live replacement, or a
// late legacy create after the domain was deleted.
const postDrainConvergenceDelay = 15 * time.Minute

// errStillPending is returned by the reconcile worker to make River retry
// with backoff while SES is still verifying. It is an expected control-flow
// signal, not a real failure.
var errStillPending = errors.New("sending identity still pending verification")

// Store is the narrow persistence surface the workers need. *identity.Store
// satisfies it. Kept minimal so the workers don't depend on the whole store.
type Store interface {
	// WithSendingIdentityMutationLock serializes provider create/delete calls
	// for one domain across processes and passes a context pinned to the lock.
	WithSendingIdentityMutationLock(ctx context.Context, domain string, fn func(context.Context) error) error
	// LoadSendingIdentityState returns the current domain incarnation and its
	// desired provider state. pgx.ErrNoRows means the desired state is absent.
	LoadSendingIdentityState(ctx context.Context, domain string) (SendingIdentityState, error)
	// SetSendingStatus writes a terminal/transition status (+ the per-axis
	// dkim/mailFrom breakdown + error + DNS records) and stamps
	// sending_last_checked_at. dkimStatus/mailFromStatus may be empty ("")
	// when the caller has no per-axis signal (e.g. provision, or a terminal
	// failure with no SES poll); persisting empty lets the read path fall back
	// to the all-or-nothing rollup.
	SetSendingStatus(ctx context.Context, domain, incarnation string, status, dkimStatus, mailFromStatus Status, errMsg string, records []DNSRecord) error
	// TouchSendingChecked stamps sending_last_checked_at without changing the
	// status — used on a still-pending poll.
	TouchSendingChecked(ctx context.Context, domain, incarnation string) error
	// The managed-domain ledger survives domain deletion so exhausted River
	// jobs remain repairable without scanning/deleting unrelated identities in
	// the provider account.
	MarkSendingIdentityManaged(ctx context.Context, domain, incarnation string) error
	MarkSendingIdentityApplied(ctx context.Context, domain, incarnation string) error
	ForgetSendingIdentityManaged(ctx context.Context, domain string) error
	ListManagedSendingIdentityDomains(ctx context.Context) ([]string, map[string]bool, error)
}

// SendingIdentityState is the incarnation-consistent desired-state snapshot
// that both provision and deprovision jobs converge at execution time.
type SendingIdentityState struct {
	Incarnation string
	Owner       string
	Verified    bool
	Status      Status
	Selector    string
	PrivateKey  []byte
}

// EventFirer publishes a domain.sending_verified / domain.sending_failed
// event. Injected as a closure so this package doesn't depend on webhookpub.
// userID is the domain owner; a nil firer (tests) is a no-op.
type EventFirer func(ctx context.Context, domain, userID string, status Status, errMsg string)

// --- job args ---

type ProvisionArgs struct {
	Domain string `json:"domain"`
}

func (ProvisionArgs) Kind() string { return "sender_identity_provision" }

type ReconcileArgs struct {
	Domain      string `json:"domain"`
	Incarnation string `json:"incarnation,omitempty"`
}

func (ReconcileArgs) Kind() string { return "sender_identity_reconcile" }

type DeprovisionArgs struct {
	Domain string `json:"domain"`
}

func (DeprovisionArgs) Kind() string { return "sender_identity_deprovision" }

// SyncArgs is the rollout-safe desired-state mutation kind. Old blue/green
// binaries do not register this kind, so they cannot claim newly enqueued
// create/delete work while the new binary is baking or the old slot drains.
type SyncArgs struct {
	Domain string `json:"domain"`
}

func (SyncArgs) Kind() string { return "sender_identity_sync_v2" }

// ReconcileV2Args likewise keeps new incarnation-aware polls away from an old
// worker that would ignore the incarnation field during a blue/green overlap.
type ReconcileV2Args struct {
	Domain      string `json:"domain"`
	Incarnation string `json:"incarnation"`
}

func (ReconcileV2Args) Kind() string { return "sender_identity_reconcile_v2" }

// PostDrainAuditArgs is a domain-scoped, deduplicated finalizer scheduled by
// mutations that can overlap a legacy blue/green slot. It runs only on the v2
// queue and only after the old slot's maximum bake+drain window has elapsed.
type PostDrainAuditArgs struct {
	Domain string `json:"domain" river:"unique"`
}

func (PostDrainAuditArgs) Kind() string { return "sender_identity_post_drain_audit_v2" }

// --- workers ---

// ProvisionWorker registers the SES sending identity (BYODKIM) for a domain
// and, on success, enqueues a reconcile job to poll it to verified.
type ProvisionWorker struct {
	river.WorkerDefaults[ProvisionArgs]
	store               Store
	provider            Provider
	fire                EventFirer
	maxReconcileAttempt int
}

func (w *ProvisionWorker) Work(ctx context.Context, job *river.Job[ProvisionArgs]) error {
	return convergeWorkerIdentity(ctx, job.Args.Domain, w.store, w.provider, w.fire, w.maxReconcileAttempt, true)
}

// SyncWorker handles all newly enqueued provider mutations. Legacy workers
// remain registered only to drain jobs written by the prior release.
type SyncWorker struct {
	river.WorkerDefaults[SyncArgs]
	store               Store
	provider            Provider
	fire                EventFirer
	maxReconcileAttempt int
}

func (w *SyncWorker) Work(ctx context.Context, job *river.Job[SyncArgs]) error {
	// A durable mutation signal always forces the current live incarnation to
	// be installed, even if a stale legacy poll incorrectly marked it verified.
	return convergeWorkerIdentity(ctx, job.Args.Domain, w.store, w.provider, w.fire, w.maxReconcileAttempt, true)
}

// ReconcileWorker polls SES for a pending domain and transitions it to
// verified/failed. While still pending it returns errStillPending so River
// retries with backoff; once the attempt budget is exhausted it marks the
// domain failed (bounded TTL — no infinite poll).
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	store               Store
	provider            Provider
	fire                EventFirer
	maxReconcileAttempt int
}

func (w *ReconcileWorker) Work(ctx context.Context, job *river.Job[ReconcileArgs]) error {
	// Jobs written by the old release have no incarnation. Never spend their
	// inherited attempt count polling whatever row now occupies the domain;
	// converge current desired state and enqueue a fresh v2 poll budget instead.
	if job.Args.Incarnation == "" {
		return convergeWorkerIdentity(ctx, job.Args.Domain, w.store, w.provider, w.fire, w.maxReconcileAttempt, true)
	}
	return reconcileProviderIdentity(ctx, job.Args.Domain, job.Args.Incarnation, job.Attempt, job.MaxAttempts, w.store, w.provider, w.fire, w.maxReconcileAttempt)
}

type ReconcileV2Worker struct {
	river.WorkerDefaults[ReconcileV2Args]
	store               Store
	provider            Provider
	fire                EventFirer
	maxReconcileAttempt int
}

func (w *ReconcileV2Worker) Work(ctx context.Context, job *river.Job[ReconcileV2Args]) error {
	return reconcileProviderIdentity(ctx, job.Args.Domain, job.Args.Incarnation, job.Attempt, job.MaxAttempts, w.store, w.provider, w.fire, w.maxReconcileAttempt)
}

func reconcileProviderIdentity(ctx context.Context, domain, incarnation string, attempt, maxAttempt int, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int) error {
	state, err := store.LoadSendingIdentityState(ctx, domain)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // domain deleted; nothing to reconcile
		}
		return err
	}
	if !state.Verified || incarnation != state.Incarnation {
		return nil // unverified or a stale job for an older registration
	}
	if state.Status != StatusPending {
		return nil // already resolved (forced re-check, dup job, etc.)
	}

	res, err := provider.Status(ctx, domain)
	if errors.Is(err, ErrIdentityNotFound) {
		// The old blue/green slot may have deleted the replacement after v2
		// installed it. Repair desired state immediately instead of turning a
		// rollout race into a terminal customer-visible failure.
		return convergeWorkerIdentity(ctx, domain, store, provider, fire, maxReconcileAttempt, true)
	}
	if errors.Is(err, ErrIdentityNotOwned) {
		const reason = "provider identity exists but is not managed by e2a"
		if err := setFailedFire(ctx, store, fire, domain, state, reason); err != nil {
			return err
		}
		return store.ForgetSendingIdentityManaged(ctx, domain)
	}
	if err != nil {
		// Transient SES/network error. Retry — UNLESS this was the last
		// attempt, in which case returning err would let River discard the
		// job and strand the domain in `pending` forever. Mark failed so the
		// TTL is absolute even when the final poll errors.
		if attempt >= maxAttempt {
			return setFailedFire(ctx, store, fire, domain, state, "verification timed out")
		}
		return err // retry (consumes an attempt)
	}

	switch res.Status {
	case StatusVerified:
		if err := store.SetSendingStatus(ctx, domain, state.Incarnation, StatusVerified, res.DkimStatus, res.MailFromStatus, "", res.DNSRecords); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		fireOwner(ctx, fire, domain, state.Owner, StatusVerified, "")
		return nil
	case StatusFailed:
		if err := store.SetSendingStatus(ctx, domain, state.Incarnation, StatusFailed, res.DkimStatus, res.MailFromStatus, res.Error, res.DNSRecords); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		fireOwner(ctx, fire, domain, state.Owner, StatusFailed, res.Error)
		return nil
	default: // still pending
		if err := store.TouchSendingChecked(ctx, domain, state.Incarnation); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if attempt >= maxAttempt {
			return setFailedFire(ctx, store, fire, domain, state, "verification timed out")
		}
		return errStillPending
	}
}

// DeprovisionWorker removes the SES sending identity on domain/account
// delete. Idempotent: the provider treats a missing identity as success.
type DeprovisionWorker struct {
	river.WorkerDefaults[DeprovisionArgs]
	store               Store
	provider            Provider
	fire                EventFirer
	maxReconcileAttempt int
}

func (w *DeprovisionWorker) Work(ctx context.Context, job *river.Job[DeprovisionArgs]) error {
	// A job is a durable signal, not stale create/delete intent. Re-read the
	// current incarnation under the mutation lock: absent/unverified converges
	// to provider absence; a re-registered verified domain converges to its new
	// key instead of being deleted by an old teardown job.
	return convergeWorkerIdentity(ctx, job.Args.Domain, w.store, w.provider, w.fire, w.maxReconcileAttempt, true)
}

// --- helpers ---

func maxAttempts(n int) int {
	if n <= 0 {
		return DefaultMaxReconcileAttempts
	}
	return n
}

type syncOutcome struct {
	changed     bool
	incarnation string
	owner       string
	status      Status
	errMsg      string
}

func syncProviderIdentity(ctx context.Context, domain string, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, forceProvision, finalizeDeletion bool) error {
	var out syncOutcome
	err := store.WithSendingIdentityMutationLock(ctx, domain, func(lockedCtx context.Context) error {
		state, err := store.LoadSendingIdentityState(lockedCtx, domain)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !state.Verified) {
			if err := provider.Deprovision(lockedCtx, domain); err != nil && !errors.Is(err, ErrIdentityNotOwned) {
				return err
			}
			if finalizeDeletion {
				return store.ForgetSendingIdentityManaged(lockedCtx, domain)
			}
			return nil
		}
		if err != nil {
			return err
		}
		// The periodic sweep only needs to act when the provider identity is
		// absent/unapplied. A healthy applied live identity is a no-op so hourly
		// convergence never flaps a verified sender back to pending.
		if !forceProvision {
			return nil
		}
		if state.Selector == "" || len(state.PrivateKey) == 0 {
			const reason = "no DKIM key material for domain; re-register the domain"
			if err := provider.Deprovision(lockedCtx, domain); err != nil && !errors.Is(err, ErrIdentityNotOwned) {
				return err
			}
			if err := store.SetSendingStatus(lockedCtx, domain, state.Incarnation, StatusFailed, "", "", reason, nil); err != nil {
				return err
			}
			if finalizeDeletion {
				if err := store.ForgetSendingIdentityManaged(lockedCtx, domain); err != nil {
					return err
				}
			}
			out = syncOutcome{changed: true, incarnation: state.Incarnation, owner: state.Owner, status: StatusFailed, errMsg: reason}
			return nil
		}
		if err := store.MarkSendingIdentityManaged(lockedCtx, domain, state.Incarnation); err != nil {
			return err
		}
		res, err := provider.Provision(lockedCtx, domain, state.Selector, state.PrivateKey)
		if errors.Is(err, ErrIdentityNotOwned) {
			const reason = "provider identity exists but is not managed by e2a"
			if err := store.SetSendingStatus(lockedCtx, domain, state.Incarnation, StatusFailed, "", "", reason, nil); err != nil {
				return err
			}
			if err := store.ForgetSendingIdentityManaged(lockedCtx, domain); err != nil {
				return err
			}
			out = syncOutcome{changed: true, incarnation: state.Incarnation, owner: state.Owner, status: StatusFailed, errMsg: reason}
			return nil
		}
		if err != nil {
			return err
		}
		if res.Status == StatusFailed {
			// A malformed key and other terminal pre-create failures must not
			// leave an older incarnation's provider identity alive.
			if err := provider.Deprovision(lockedCtx, domain); err != nil && !errors.Is(err, ErrIdentityNotOwned) {
				return err
			}
		}
		if err := store.SetSendingStatus(lockedCtx, domain, state.Incarnation, res.Status, res.DkimStatus, res.MailFromStatus, res.Error, res.DNSRecords); err != nil {
			return err
		}
		if res.Status == StatusFailed {
			if finalizeDeletion {
				if err := store.ForgetSendingIdentityManaged(lockedCtx, domain); err != nil {
					return err
				}
			}
		} else if err := store.MarkSendingIdentityApplied(lockedCtx, domain, state.Incarnation); err != nil {
			return err
		}
		out = syncOutcome{changed: true, incarnation: state.Incarnation, owner: state.Owner, status: res.Status, errMsg: res.Error}
		return nil
	})
	if err != nil {
		return err
	}
	if !out.changed {
		return nil
	}
	switch out.status {
	case StatusVerified, StatusFailed:
		fireOwner(ctx, fire, domain, out.owner, out.status, out.errMsg)
		return nil
	default:
		client, cerr := river.ClientFromContextSafely[pgx.Tx](ctx)
		if cerr != nil || client == nil {
			return nil
		}
		_, err := client.Insert(ctx, ReconcileV2Args{Domain: domain, Incarnation: out.incarnation}, &river.InsertOpts{
			MaxAttempts: maxAttempts(maxReconcileAttempt),
			Queue:       jobs.QueueSenderIdentityV2,
		})
		return err
	}
}

// convergeWorkerIdentity runs a mutation from a River worker but deliberately
// retains deletion tombstones until a post-drain v2 sweep. An old binary does
// not take the advisory lock and can still finish a provider create/delete
// after this call during blue/green overlap; the delayed sweep is the durable
// convergence handoff once that binary has stopped.
func convergeWorkerIdentity(ctx context.Context, domain string, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, forceProvision bool) error {
	if err := syncProviderIdentity(ctx, domain, store, provider, fire, maxReconcileAttempt, forceProvision, false); err != nil {
		return err
	}
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil || client == nil {
		return nil
	}
	_, err = client.Insert(ctx, PostDrainAuditArgs{Domain: domain}, &river.InsertOpts{
		Queue:       jobs.QueueSenderIdentityV2,
		ScheduledAt: time.Now().Add(postDrainConvergenceDelay),
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByQueue: true},
	})
	return err
}

// setFailedFire writes a failed status and fires domain.sending_failed.
func setFailedFire(ctx context.Context, store Store, fire EventFirer, domain string, state SendingIdentityState, reason string) error {
	// Terminal failures here (no key material, identity not found at provider,
	// verification timed out) carry no per-axis signal — persist empty axes so
	// the read path falls back to the rollup (all three records read failed).
	if err := store.SetSendingStatus(ctx, domain, state.Incarnation, StatusFailed, "", "", reason, nil); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	fireOwner(ctx, fire, domain, state.Owner, StatusFailed, reason)
	return nil
}

// fireOwner fires the event for the incarnation snapshot (best-effort).
func fireOwner(ctx context.Context, fire EventFirer, domain, owner string, st Status, errMsg string) {
	if fire == nil || owner == "" {
		return
	}
	fire(ctx, domain, owner, st, errMsg)
}
