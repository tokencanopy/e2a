package senderidentity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/domainteardown"
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

// pendingVerificationBackstopTTL is the ABSOLUTE deadline for a domain stuck
// `pending` with no live reconcile budget (the hourly sweep's read-only
// resolve path). River's per-job attempt budget exhausts in hours, so a
// pending older than this (anchored on the ledger row's last mutation) has
// provably lost its poller and times out instead of being re-checked hourly
// forever — preserving the design's bounded-verification promise.
const pendingVerificationBackstopTTL = 24 * time.Hour

// verifiedProviderPendingGrace preserves a verified sender through transient
// provider rechecks, but fails closed when the provider stays pending beyond
// a full day. The first observation is persisted in the ownership ledger, so
// restarts and hourly-job retries cannot reset the grace period.
const verifiedProviderPendingGrace = 24 * time.Hour

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
	// SendingIdentityLedgerExpired compares entirely on the database clock so
	// application/DB clock skew cannot shorten or extend the pending TTL.
	SendingIdentityLedgerExpired(ctx context.Context, domain, incarnation string, olderThan time.Duration) (bool, error)
	// ObserveSendingIdentityProviderPending persists the first provider-pending
	// observation for a DB-verified identity and reports whether its grace has
	// expired, also using the database clock. Clear removes that drift marker
	// once the provider agrees or a terminal transition is committed.
	ObserveSendingIdentityProviderPending(ctx context.Context, domain, incarnation string, olderThan time.Duration) (bool, error)
	ClearSendingIdentityProviderPending(ctx context.Context, domain, incarnation string) error
	ForgetSendingIdentityManaged(ctx context.Context, domain string) error
	// FinalizeSendingIdentityTombstone removes the ledger row ONLY when its
	// last mutation (updated_at) is older than olderThan. An audit or sweep
	// that runs inside a later mutation's drain window therefore cannot
	// finalize that mutation's tombstone out from under a still-draining
	// legacy slot; the reaper (or that mutation's own audit) finalizes once
	// the window has truly elapsed.
	FinalizeSendingIdentityTombstone(ctx context.Context, domain string, olderThan time.Duration) error
	SetDomainTeardownState(ctx context.Context, domain string, state domainteardown.State) error
	ListManagedSendingIdentityDomains(ctx context.Context) ([]string, map[string]bool, error)
	ListManagedSendingIdentityDomainsPage(ctx context.Context, afterDomain string, limit int) ([]string, map[string]bool, bool, error)
	LookupManagedSendingIdentityDomain(ctx context.Context, domain string) (needsProvision, found bool, err error)
	// DomainExists reports whether a live domain row exists. The reaper uses
	// it to ALERT on provider identities that are neither ledgered nor backed
	// by a row (pre-upgrade orphans the migration backfill could not see).
	DomainExists(ctx context.Context, domain string) (bool, error)
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
	// AppliedIncarnation is the ledger's provider-confirmed incarnation ("" if
	// none). Equal to Incarnation only when THIS registration's key was
	// confirmed installed — the gate for the healthy-recheck no-op.
	AppliedIncarnation string
	// LedgerUpdatedAt remains available for diagnostics and adapters. Timeout
	// decisions use Store.SendingIdentityLedgerExpired so clocks are not mixed.
	LedgerUpdatedAt time.Time
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
// queue, nominally postDrainConvergenceDelay after its mutation — but see
// Window: a mutation deduped into an audit scheduled earlier in the same
// bucket can be finalized up to one bucket early, i.e. possibly while the old
// slot still drains. The hourly reaper and the orphan ALERT remain the
// backstop for that residual window (which the pre-Window code had in an
// unbounded form).
type PostDrainAuditArgs struct {
	Domain string `json:"domain" river:"unique"`
	// Window is the audit's schedule bucket (scheduled-at unix seconds /
	// postDrainConvergenceDelay). It scopes ByArgs uniqueness in time: River's
	// unique state set cannot drop `completed`, so without it a completed
	// audit inside the ~24h retention blocked the next mutation's audit for
	// the same domain. A completed audit's bucket is strictly older than any
	// new mutation's (an audit for bucket B cannot complete before B's start,
	// and any later mutation schedules into a later bucket — modulo replica
	// clock skew, which shrinks that guarantee by the skew), so dedupe can
	// only ever match a still-scheduled job. Mutations straddling a bucket
	// edge produce two audits; the audit is idempotent, so that is noise,
	// not a bug.
	Window int64 `json:"window" river:"unique"`
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
	legacyJobs          bool
}

func (w *ProvisionWorker) Work(ctx context.Context, job *river.Job[ProvisionArgs]) error {
	return convergeWorkerIdentity(ctx, job.Args.Domain, w.store, w.provider, w.fire, w.maxReconcileAttempt, true, w.legacyJobs)
}

// SyncWorker handles all newly enqueued provider mutations. Legacy workers
// remain registered only to drain jobs written by the prior release.
type SyncWorker struct {
	river.WorkerDefaults[SyncArgs]
	store               Store
	provider            Provider
	fire                EventFirer
	maxReconcileAttempt int
	legacyJobs          bool
}

func (w *SyncWorker) Work(ctx context.Context, job *river.Job[SyncArgs]) error {
	// A durable mutation signal always forces the current live incarnation to
	// be installed, even if a stale legacy poll incorrectly marked it verified.
	return convergeWorkerIdentity(ctx, job.Args.Domain, w.store, w.provider, w.fire, w.maxReconcileAttempt, true, w.legacyJobs)
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
	legacyJobs          bool
}

func (w *ReconcileWorker) Work(ctx context.Context, job *river.Job[ReconcileArgs]) error {
	// Jobs written by the old release have no incarnation. Never spend their
	// inherited attempt count polling whatever row now occupies the domain;
	// converge current desired state and enqueue a fresh v2 poll budget instead.
	if job.Args.Incarnation == "" {
		return convergeWorkerIdentity(ctx, job.Args.Domain, w.store, w.provider, w.fire, w.maxReconcileAttempt, true, w.legacyJobs)
	}
	return reconcileProviderIdentity(ctx, job.Args.Domain, job.Args.Incarnation, job.Attempt, job.MaxAttempts, w.store, w.provider, w.fire, w.maxReconcileAttempt, w.legacyJobs)
}

type ReconcileV2Worker struct {
	river.WorkerDefaults[ReconcileV2Args]
	store               Store
	provider            Provider
	fire                EventFirer
	maxReconcileAttempt int
	legacyJobs          bool
}

func (w *ReconcileV2Worker) Work(ctx context.Context, job *river.Job[ReconcileV2Args]) error {
	return reconcileProviderIdentity(ctx, job.Args.Domain, job.Args.Incarnation, job.Attempt, job.MaxAttempts, w.store, w.provider, w.fire, w.maxReconcileAttempt, w.legacyJobs)
}

func reconcileProviderIdentity(ctx context.Context, domain, incarnation string, attempt, maxAttempt int, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, legacyJobs bool) error {
	var repairMissingIdentity bool
	var out syncOutcome
	err := store.WithSendingIdentityMutationLock(ctx, domain, func(ctx context.Context) error {
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

		res, err := provider.Status(ctx, domain, state.Selector, len(state.PrivateKey) > 0)
		if errors.Is(err, ErrIdentityNotFound) {
			// The old blue/green slot may have deleted the replacement after v2
			// installed it. Repair desired state immediately instead of turning a
			// rollout race into a terminal customer-visible failure.
			repairMissingIdentity = true
			return nil
		}
		if errors.Is(err, ErrIdentityNotOwned) {
			const reason = "provider identity exists but is not managed by e2a"
			changed, err := setFailedStatus(ctx, store, domain, state, reason)
			if err != nil {
				return err
			}
			if changed {
				// out is set BEFORE the ledger Forget: the failed status is already
				// committed, and the retry after a Forget error short-circuits on
				// status != pending — firing must not depend on Forget succeeding.
				out = syncOutcome{changed: true, owner: state.Owner, status: StatusFailed, errMsg: reason}
			}
			return store.ForgetSendingIdentityManaged(ctx, domain)
		}
		if err != nil {
			// Transient SES/network error. Retry — UNLESS this was the last
			// attempt, in which case returning err would let River discard the
			// job and strand the domain in `pending` forever. Mark failed so the
			// TTL is absolute even when the final poll errors.
			if attempt >= maxAttempt {
				const reason = "verification timed out"
				changed, err := setFailedStatus(ctx, store, domain, state, reason)
				if changed {
					out = syncOutcome{changed: true, owner: state.Owner, status: StatusFailed, errMsg: reason}
				}
				return err
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
			out = syncOutcome{changed: true, owner: state.Owner, status: StatusVerified}
			return nil
		case StatusFailed:
			if err := store.SetSendingStatus(ctx, domain, state.Incarnation, StatusFailed, res.DkimStatus, res.MailFromStatus, res.Error, res.DNSRecords); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			out = syncOutcome{changed: true, owner: state.Owner, status: StatusFailed, errMsg: res.Error}
			return nil
		default: // still pending
			if err := store.TouchSendingChecked(ctx, domain, state.Incarnation); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			if attempt >= maxAttempt {
				const reason = "verification timed out"
				changed, err := setFailedStatus(ctx, store, domain, state, reason)
				if changed {
					out = syncOutcome{changed: true, owner: state.Owner, status: StatusFailed, errMsg: reason}
				}
				return err
			}
			return errStillPending
		}
	})
	// Fire before the error check: a terminal status may have committed even
	// when a later store call in the same locked section failed (e.g. the
	// ledger Forget after a not-owned failure). The retry short-circuits on
	// status != pending, so this attempt is the event's only chance.
	if out.changed {
		fireOwner(ctx, fire, domain, out.owner, out.status, out.errMsg)
	}
	if err != nil {
		return err
	}
	if repairMissingIdentity {
		// Re-enter through the normal mutation helper only after releasing this
		// lock; convergeWorkerIdentity acquires the same non-reentrant lock.
		return convergeWorkerIdentity(ctx, domain, store, provider, fire, maxReconcileAttempt, true, legacyJobs)
	}
	return nil
}

// DeprovisionWorker removes the SES sending identity on domain/account
// delete. Idempotent: the provider treats a missing identity as success.
type DeprovisionWorker struct {
	river.WorkerDefaults[DeprovisionArgs]
	store               Store
	provider            Provider
	fire                EventFirer
	maxReconcileAttempt int
	legacyJobs          bool
}

func (w *DeprovisionWorker) Work(ctx context.Context, job *river.Job[DeprovisionArgs]) error {
	// A job is a durable signal, not stale create/delete intent. Re-read the
	// current incarnation under the mutation lock: absent/unverified converges
	// to provider absence; a re-registered verified domain converges to its new
	// key instead of being deleted by an old teardown job.
	return convergeWorkerIdentity(ctx, job.Args.Domain, w.store, w.provider, w.fire, w.maxReconcileAttempt, true, w.legacyJobs)
}

// --- helpers ---

func maxAttempts(n int) int {
	if n <= 0 {
		return DefaultMaxReconcileAttempts
	}
	return n
}

type syncOutcome struct {
	// changed means a status write committed this attempt (drives the
	// pending-path reconcile enqueue, which must run even for a
	// pending→pending re-provision).
	changed bool
	// statusChanged means the written status DIFFERS from the pre-write
	// state — the only thing that may fire a customer-visible webhook. The
	// sync path re-executes its terminal transition on every River retry and
	// on every forced hourly sweep, so firing on `changed` alone multiplies
	// domain.sending_* events by the retry/sweep count (review finding).
	statusChanged bool
	incarnation   string
	owner         string
	status        Status
	errMsg        string
}

func syncProviderIdentity(ctx context.Context, domain string, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, forceProvision, finalizeDeletion, legacyJobs bool) (teardownConfirmed bool, retErr error) {
	return syncProviderIdentityWithInspection(ctx, domain, store, provider, fire, maxReconcileAttempt, forceProvision, finalizeDeletion, legacyJobs, false)
}

// syncProviderIdentityForReaper inspects every live ledger candidate, including
// terminal failed rows, so a missing provider identity is repaired without a
// separate whole-account List. The observation is made and reused under the
// domain mutation lock, keeping the job to one provider GET per candidate.
func syncProviderIdentityForReaper(ctx context.Context, domain string, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, forceProvision, finalizeDeletion, legacyJobs bool) (teardownConfirmed bool, retErr error) {
	return syncProviderIdentityWithInspection(ctx, domain, store, provider, fire, maxReconcileAttempt, forceProvision, finalizeDeletion, legacyJobs, true)
}

func syncProviderIdentityWithInspection(ctx context.Context, domain string, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, forceProvision, finalizeDeletion, legacyJobs, inspectTerminal bool) (teardownConfirmed bool, retErr error) {
	var out syncOutcome
	err := store.WithSendingIdentityMutationLock(ctx, domain, func(lockedCtx context.Context) error {
		state, err := store.LoadSendingIdentityState(lockedCtx, domain)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !state.Verified) {
			if err := provider.Deprovision(lockedCtx, domain); errors.Is(err, ErrIdentityNotOwned) {
				// The provider identity still exists. It may be foreign, or it may
				// be an e2a identity whose ownership tag drifted. Either way we are
				// not authorized to delete it and must not claim absence. Keep the
				// durable job/reaper red so this cannot disappear as a successful
				// teardown after a lost HTTP response or account deletion.
				ownershipErr := fmt.Errorf("provider identity ownership is unconfirmed; manual review required: %w", ErrIdentityNotOwned)
				if stateErr := store.SetDomainTeardownState(lockedCtx, domain, domainteardown.ManualReview); stateErr != nil {
					return errors.Join(ownershipErr, fmt.Errorf("persist manual-review teardown receipt: %w", stateErr))
				}
				return ownershipErr
			} else if err != nil {
				return err
			}
			if err := store.SetDomainTeardownState(lockedCtx, domain, domainteardown.Confirmed); err != nil {
				return fmt.Errorf("persist confirmed teardown receipt: %w", err)
			}
			teardownConfirmed = true
			if finalizeDeletion {
				return store.FinalizeSendingIdentityTombstone(lockedCtx, domain, postDrainConvergenceDelay)
			}
			return nil
		}
		if err != nil {
			return err
		}
		var observedResult Result
		var observedErr error
		observed := false
		providerStatus := func() (Result, error) {
			if !observed {
				// state.PrivateKey backs the adoption judgement Status may make
				// internally (see canAdoptIdentity's doc): a non-empty selector
				// with no key material must never be treated as adoptable, or
				// Status would tag an identity e2a cannot sign for — and the
				// no-key branch below would then find it tagged and let
				// Deprovision actually delete it.
				observedResult, observedErr = provider.Status(lockedCtx, domain, state.Selector, len(state.PrivateKey) > 0)
				observed = true
			}
			return observedResult, observedErr
		}
		// The periodic sweep never MUTATES a healthy applied identity (that
		// would flap a verified sender back to pending), but it does inspect
		// the two states that can silently rot behind an applied, present
		// identity, both READ-ONLY:
		//
		//   - stuck `pending`: the reconcile budget is gone (enqueue failed or
		//     exhausted) and nothing else would ever poll it. Verified/failed
		//     resolves and fires; still-pending waits — bounded by an absolute
		//     backstop TTL anchored on the ledger's last mutation, so a
		//     never-resolving identity cannot be polled hourly forever.
		//   - `verified` drift: an ownership tag removed out-of-band or a
		//     provider-side hard failure (customer pulled the DNS) otherwise
		//     stays reported verified indefinitely. A definitive provider
		//     `failed` commits (with axes) and fires; a provider mid-recheck
		//     `pending` is NOT committed (no flap on a transient re-check).
		//
		// An absent/foreign answer in either state falls through to full
		// convergence; `failed` steady state costs no provider call.
		force := forceProvision
		if !force {
			if !inspectTerminal && state.Status != StatusPending && state.Status != StatusVerified {
				return nil
			}
			res, serr := providerStatus()
			switch {
			case errors.Is(serr, ErrIdentityNotFound), errors.Is(serr, ErrIdentityNotOwned):
				force = true
			case serr != nil:
				return serr
			case state.Status == StatusPending && (res.Status == StatusVerified || res.Status == StatusFailed),
				state.Status == StatusVerified && res.Status == StatusFailed:
				if err := store.SetSendingStatus(lockedCtx, domain, state.Incarnation, res.Status, res.DkimStatus, res.MailFromStatus, res.Error, res.DNSRecords); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return nil
					}
					return err
				}
				out = syncOutcome{changed: true, statusChanged: res.Status != state.Status, incarnation: state.Incarnation, owner: state.Owner, status: res.Status, errMsg: res.Error}
				return store.ClearSendingIdentityProviderPending(lockedCtx, domain, state.Incarnation)
			case state.Status == StatusPending:
				// Provider still verifying. Keep the deadline entirely on the DB
				// clock; comparing a DB timestamp with time.Now on this host makes
				// clock skew part of the customer-visible state machine.
				expired, err := store.SendingIdentityLedgerExpired(lockedCtx, domain, state.Incarnation, pendingVerificationBackstopTTL)
				if err != nil {
					return err
				}
				if expired {
					const reason = "verification timed out"
					changed, err := setFailedStatus(lockedCtx, store, domain, state, reason)
					if err != nil {
						return err
					}
					if changed {
						out = syncOutcome{changed: true, statusChanged: true, incarnation: state.Incarnation, owner: state.Owner, status: StatusFailed, errMsg: reason}
					}
				}
				return nil
			case state.Status == StatusVerified && res.Status == StatusPending:
				expired, err := store.ObserveSendingIdentityProviderPending(lockedCtx, domain, state.Incarnation, verifiedProviderPendingGrace)
				if err != nil {
					return err
				}
				if !expired {
					return nil
				}
				const reason = "provider verification remained pending"
				changed, err := setFailedStatus(lockedCtx, store, domain, state, reason)
				if err != nil {
					return err
				}
				if changed {
					out = syncOutcome{changed: true, statusChanged: true, incarnation: state.Incarnation, owner: state.Owner, status: StatusFailed, errMsg: reason}
				}
				return store.ClearSendingIdentityProviderPending(lockedCtx, domain, state.Incarnation)
			case state.Status == StatusVerified && res.Status == StatusVerified:
				return store.ClearSendingIdentityProviderPending(lockedCtx, domain, state.Incarnation)
			default:
				return nil
			}
		}
		// A durable mutation signal is usually a redundant re-check (POST
		// /verify on a healthy domain). When the ledger confirms THIS
		// incarnation applied and the provider agrees the identity is verified,
		// converging again would re-Put the BYODKIM key and demote
		// verified→pending until the next poll — the exact flap the pre-ledger
		// no-op guard prevented. One provider GET replaces the Create/Put; a
		// definitive non-healthy answer (missing, foreign, pending, failed)
		// falls through to full convergence, so a stale-verified row — whose
		// applied incarnation necessarily differs — still converges. A
		// transient GET error is NO signal: retry rather than mutate a
		// by-ledger-healthy sender blind.
		//
		// CAUTION: the GET cross-checks the provider's verification STATUS,
		// not the installed key material. `applied_incarnation == incarnation`
		// is what ties "verified" to "this registration's selector/key" — that
		// holds today because selector/key are immutable per incarnation. Any
		// future in-place DKIM key rotation or MAIL FROM convention change
		// must invalidate applied_incarnation (or revisit this gate), or the
		// forced re-check will silently stop re-installing.
		if state.Status == StatusVerified && state.AppliedIncarnation == state.Incarnation {
			res, serr := providerStatus()
			if serr == nil && res.Status == StatusVerified {
				return store.ClearSendingIdentityProviderPending(lockedCtx, domain, state.Incarnation)
			}
			if serr != nil && !errors.Is(serr, ErrIdentityNotFound) && !errors.Is(serr, ErrIdentityNotOwned) {
				return serr
			}
		}
		if state.Selector == "" || len(state.PrivateKey) == 0 {
			const reason = "no DKIM key material for domain; re-register the domain"
			if err := provider.Deprovision(lockedCtx, domain); err != nil && !errors.Is(err, ErrIdentityNotOwned) {
				return err
			}
			if err := store.SetSendingStatus(lockedCtx, domain, state.Incarnation, StatusFailed, "", "", reason, nil); err != nil {
				return err
			}
			// out before Forget: the failed status is committed, so the event
			// must fire even when the ledger cleanup errors and the job retries.
			out = syncOutcome{changed: true, statusChanged: state.Status != StatusFailed, incarnation: state.Incarnation, owner: state.Owner, status: StatusFailed, errMsg: reason}
			if finalizeDeletion {
				if err := store.FinalizeSendingIdentityTombstone(lockedCtx, domain, postDrainConvergenceDelay); err != nil {
					return err
				}
			}
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
			// out before Forget — see the no-key branch above.
			out = syncOutcome{changed: true, statusChanged: state.Status != StatusFailed, incarnation: state.Incarnation, owner: state.Owner, status: StatusFailed, errMsg: reason}
			return store.ForgetSendingIdentityManaged(lockedCtx, domain)
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
		// out before the ledger calls below, same rule as the two branches
		// above: the status is committed, so a terminal transition must fire
		// even when Forget/MarkApplied errors and the job retries.
		out = syncOutcome{changed: true, statusChanged: res.Status != state.Status, incarnation: state.Incarnation, owner: state.Owner, status: res.Status, errMsg: res.Error}
		if res.Status == StatusFailed {
			if finalizeDeletion {
				if err := store.FinalizeSendingIdentityTombstone(lockedCtx, domain, postDrainConvergenceDelay); err != nil {
					return err
				}
			}
		} else if err := store.MarkSendingIdentityApplied(lockedCtx, domain, state.Incarnation); err != nil {
			return err
		}
		return nil
	})
	// Fire terminal TRANSITIONS before the error check: the status write is
	// already committed, and after a later store error (e.g. the ledger
	// Forget) the retry's rewrite is same→same and will not fire — this
	// attempt is the event's only chance. Gating on statusChanged (not
	// changed) is what keeps that retry, and every forced hourly re-sweep of
	// a stuck domain, from duplicating the webhook.
	if out.statusChanged && (out.status == StatusVerified || out.status == StatusFailed) {
		fireOwner(ctx, fire, domain, out.owner, out.status, out.errMsg)
	}
	if err != nil {
		return teardownConfirmed, err
	}
	if !out.changed {
		return teardownConfirmed, nil
	}
	switch out.status {
	case StatusVerified, StatusFailed:
		return teardownConfirmed, nil
	default:
		client, cerr := river.ClientFromContextSafely[pgx.Tx](ctx)
		if cerr != nil || client == nil {
			return teardownConfirmed, nil
		}
		args, opts := reconcileInsert(domain, out.incarnation, maxAttempts(maxReconcileAttempt), legacyJobs)
		_, err := client.Insert(ctx, args, opts)
		return teardownConfirmed, err
	}
}

// convergeWorkerIdentity runs a mutation from a River worker but deliberately
// retains deletion tombstones until a post-drain v2 sweep. An old binary does
// not take the advisory lock and can still finish a provider create/delete
// after this call during blue/green overlap; the delayed sweep is the durable
// convergence handoff once that binary has stopped.
func convergeWorkerIdentity(ctx context.Context, domain string, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, forceProvision, legacyJobs bool) error {
	if _, err := syncProviderIdentity(ctx, domain, store, provider, fire, maxReconcileAttempt, forceProvision, false, legacyJobs); err != nil {
		return err
	}
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil || client == nil {
		return nil
	}
	args, opts := postDrainAuditInsert(domain, time.Now())
	_, err = client.Insert(ctx, args, opts)
	return err
}

// reconcileInsert builds the fresh verification-poll insert per compat mode
// (see Config.LegacyJobCompat): the legacy kind on the default queue keeps
// the previous release able to claim the poll during phase 1 (it ignores the
// unknown incarnation field and polls with its old semantics — today's
// production behavior), while this binary's own ReconcileWorker honors the
// incarnation.
func reconcileInsert(domain, incarnation string, maxAttempts int, legacy bool) (river.JobArgs, *river.InsertOpts) {
	if legacy {
		return ReconcileArgs{Domain: domain, Incarnation: incarnation}, &river.InsertOpts{MaxAttempts: maxAttempts}
	}
	return ReconcileV2Args{Domain: domain, Incarnation: incarnation}, &river.InsertOpts{MaxAttempts: maxAttempts, Queue: jobs.QueueSenderIdentityV2}
}

// postDrainAuditInsert builds the delayed finalizer insert for a mutation
// happening at now. See PostDrainAuditArgs.Window for why uniqueness is
// scoped to the schedule bucket.
func postDrainAuditInsert(domain string, now time.Time) (PostDrainAuditArgs, *river.InsertOpts) {
	scheduledAt := now.Add(postDrainConvergenceDelay)
	return PostDrainAuditArgs{
			Domain: domain,
			Window: scheduledAt.Unix() / int64(postDrainConvergenceDelay/time.Second),
		}, &river.InsertOpts{
			Queue:       jobs.QueueSenderIdentityV2,
			ScheduledAt: scheduledAt,
			UniqueOpts:  river.UniqueOpts{ByArgs: true, ByQueue: true},
		}
}

// setFailedStatus writes a failed status and reports whether the incarnation
// still existed. Event publication must happen after the mutation lock is
// released because the production firer opens another database transaction.
func setFailedStatus(ctx context.Context, store Store, domain string, state SendingIdentityState, reason string) (bool, error) {
	// Terminal failures here (no key material, identity not found at provider,
	// verification timed out) carry no per-axis signal — persist empty axes so
	// the read path falls back to the rollup (all three records read failed).
	if err := store.SetSendingStatus(ctx, domain, state.Incarnation, StatusFailed, "", "", reason, nil); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// fireOwner fires the event for the incarnation snapshot (best-effort).
func fireOwner(ctx context.Context, fire EventFirer, domain, owner string, st Status, errMsg string) {
	if fire == nil || owner == "" {
		return
	}
	fire(ctx, domain, owner, st, errMsg)
}
