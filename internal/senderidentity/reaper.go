package senderidentity

import (
	"context"
	"errors"
	"log"

	"github.com/riverqueue/river"
)

// ReapArgs is the legacy periodic kind. It stays registered so jobs inserted
// by the draining release are handled safely by the new worker.
type ReapArgs struct{}

func (ReapArgs) Kind() string { return "sender_identity_reap" }

// ReapV2Args prevents the old blue/green slot from claiming new convergence
// sweeps during rollout.
type ReapV2Args struct{}

func (ReapV2Args) Kind() string { return "sender_identity_reap_v2" }

type LegacyReapWorker struct {
	river.WorkerDefaults[ReapArgs]
	store               Store
	provider            Provider
	maxReconcileAttempt int
}

func (w *LegacyReapWorker) Work(ctx context.Context, _ *river.Job[ReapArgs]) error {
	// A legacy reap job may run while the old slot can still finish an unlocked
	// provider mutation. Converge what is visible, but retain deletion
	// tombstones for the post-drain v2 audit to finalize.
	return reapManagedIdentities(ctx, w.store, w.provider, w.maxReconcileAttempt, false)
}

// ReapWorker is the durable teardown/provisioning backstop. Normal mutation
// jobs retry promptly, while this hourly sweep keeps retrying the bounded
// managed-domain ledger after River exhausts a job's finite attempt budget.
// It never deletes arbitrary identities returned by SES List: only domains in
// e2a's durable ownership ledger are eligible.
type ReapWorker struct {
	river.WorkerDefaults[ReapV2Args]
	store               Store
	provider            Provider
	maxReconcileAttempt int
}

func (w *ReapWorker) Work(ctx context.Context, _ *river.Job[ReapV2Args]) error {
	return reapManagedIdentities(ctx, w.store, w.provider, w.maxReconcileAttempt, true)
}

type PostDrainAuditWorker struct {
	river.WorkerDefaults[PostDrainAuditArgs]
	store               Store
	provider            Provider
	maxReconcileAttempt int
}

func (w *PostDrainAuditWorker) Work(ctx context.Context, job *river.Job[PostDrainAuditArgs]) error {
	managed, needsProvision, err := w.store.ListManagedSendingIdentityDomains(ctx)
	if err != nil {
		return err
	}
	for _, domain := range managed {
		if domain != job.Args.Domain {
			continue
		}
		_, statusErr := w.provider.Status(ctx, domain)
		providerMissing := errors.Is(statusErr, ErrIdentityNotFound)
		if statusErr != nil && !providerMissing && !errors.Is(statusErr, ErrIdentityNotOwned) {
			return statusErr
		}
		return syncProviderIdentity(ctx, domain, w.store, w.provider, nil, w.maxReconcileAttempt, providerMissing || needsProvision[domain], true)
	}
	return nil
}

func reapManagedIdentities(ctx context.Context, store Store, provider Provider, maxReconcileAttempt int, finalizeDeletion bool) error {
	managed, needsProvision, err := store.ListManagedSendingIdentityDomains(ctx)
	if err != nil {
		return err
	}
	providerDomains, err := provider.List(ctx)
	if err != nil {
		return err
	}
	present := make(map[string]struct{}, len(providerDomains))
	for _, domain := range providerDomains {
		present[domain] = struct{}{}
	}

	var errs []error
	for _, domain := range managed {
		_, providerPresent := present[domain]
		// A missing provider identity must bypass the verified-state no-op and
		// be recreated. Present identities take the normal desired-state path,
		// which deletes them when their domain row is absent/unverified.
		if err := syncProviderIdentity(ctx, domain, store, provider, nil, maxReconcileAttempt, !providerPresent || needsProvision[domain], finalizeDeletion); err != nil {
			errs = append(errs, errors.New(domain+": "+err.Error()))
			continue
		}
	}
	if len(managed) > 0 {
		log.Printf("[senderidentity:reaper] converged %d managed identity candidate(s), %d error(s)", len(managed), len(errs))
	}
	return errors.Join(errs...)
}
