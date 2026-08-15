package senderidentity

import (
	"context"
	"errors"
	"log"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
)

// ReapArgs is the legacy periodic kind. It stays registered so jobs inserted
// by the draining release are handled safely by the new worker.
type ReapArgs struct{}

func (ReapArgs) Kind() string { return "sender_identity_reap" }

// ReapV2Args prevents the old blue/green slot from claiming new convergence
// sweeps during rollout.
type ReapV2Args struct {
	SweepID     int64  `json:"sweep_id,omitempty" river:"unique"`
	AfterDomain string `json:"after_domain,omitempty" river:"unique"`
}

func (ReapV2Args) Kind() string { return "sender_identity_reap_v2" }

type LegacyReapWorker struct {
	river.WorkerDefaults[ReapArgs]
	store               Store
	provider            Provider
	fire                EventFirer
	maxReconcileAttempt int
	legacyJobs          bool
}

func (w *LegacyReapWorker) Work(ctx context.Context, _ *river.Job[ReapArgs]) error {
	// A legacy reap job may run while the old slot can still finish an unlocked
	// provider mutation. Converge what is visible, but retain deletion
	// tombstones for the post-drain v2 audit to finalize.
	return reapManagedIdentities(ctx, w.store, w.provider, w.fire, w.maxReconcileAttempt, false, w.legacyJobs)
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
	fire                EventFirer
	maxReconcileAttempt int
	legacyJobs          bool
	// enqueueNext is injectable only so the page handoff can be proved without
	// a live River client. Production workers leave it nil.
	enqueueNext func(context.Context, ReapV2Args) error
}

func (w *ReapWorker) Work(ctx context.Context, job *river.Job[ReapV2Args]) error {
	enqueueNext := w.enqueueNext
	if enqueueNext == nil {
		enqueueNext = enqueueReapPage
	}
	return reapManagedIdentityPage(ctx, w.store, w.provider, w.fire, w.maxReconcileAttempt, true, w.legacyJobs, job.Args.SweepID, job.Args.AfterDomain, 25, enqueueNext)
}

func enqueueReapPage(ctx context.Context, args ReapV2Args) error {
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return err
	}
	_, err = client.Insert(ctx, args, &river.InsertOpts{
		Queue: jobs.QueueSenderIdentityV2,
		// A failed page is retried by River after it has already handed off its
		// continuation. Keep that retry from multiplying the rest of the chain,
		// while allowing the next hourly sweep to build a fresh chain.
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByQueue: true},
	})
	return err
}

type PostDrainAuditWorker struct {
	river.WorkerDefaults[PostDrainAuditArgs]
	store               Store
	provider            Provider
	fire                EventFirer
	maxReconcileAttempt int
	legacyJobs          bool
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
		_, err = syncProviderIdentity(ctx, domain, w.store, w.provider, w.fire, w.maxReconcileAttempt, providerMissing || needsProvision[domain], true, w.legacyJobs)
		return err
	}
	return nil
}

func reapManagedIdentities(ctx context.Context, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, finalizeDeletion, legacyJobs bool) error {
	return reapManagedIdentityPage(ctx, store, provider, fire, maxReconcileAttempt, finalizeDeletion, legacyJobs, 0, "", 0, nil)
}

func reapManagedIdentityPage(ctx context.Context, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, finalizeDeletion, legacyJobs bool, jobSweepID int64, afterDomain string, pageSize int, enqueueNext func(context.Context, ReapV2Args) error) error {
	managed, needsProvision, err := store.ListManagedSendingIdentityDomains(ctx)
	if err != nil {
		return err
	}
	sort.Strings(managed)
	page := managed
	if pageSize > 0 {
		start := sort.SearchStrings(managed, afterDomain)
		for start < len(managed) && managed[start] <= afterDomain {
			start++
		}
		end := start + pageSize
		if end > len(managed) {
			end = len(managed)
		}
		page = managed[start:end]
		if end < len(managed) {
			// Hand off the continuation before making provider calls. A slow or
			// failing identity in this page therefore cannot starve later domains.
			if err := enqueueNext(ctx, ReapV2Args{SweepID: jobSweepID, AfterDomain: page[len(page)-1]}); err != nil {
				return err
			}
		}
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
	for _, domain := range page {
		_, providerPresent := present[domain]
		// A missing provider identity must bypass the verified-state no-op and
		// be recreated. Present identities take the normal desired-state path,
		// which deletes them when their domain row is absent/unverified.
		if _, err := syncProviderIdentity(ctx, domain, store, provider, fire, maxReconcileAttempt, !providerPresent || needsProvision[domain], finalizeDeletion, legacyJobs); err != nil {
			errs = append(errs, errors.New(domain+": "+err.Error()))
			continue
		}
	}
	if len(page) > 0 {
		log.Printf("[senderidentity:reaper] converged %d managed identity candidate(s), %d error(s)", len(page), len(errs))
	}

	// Provider identities outside the ledger can never be converged (the
	// ledger is the only deletion authority) and, when no live domain row
	// backs them either, they are invisible to every other path: a delete
	// whose teardown was lost under a pre-ledger release, for example — the
	// migration backfill reads only live rows and could not adopt them.
	// ALERT-only, exactly like the pre-ledger reaper: an unledgered identity
	// may belong to another application in a shared SES account and must
	// never be mutated.
	ledgered := make(map[string]struct{}, len(managed))
	for _, domain := range managed {
		ledgered[domain] = struct{}{}
	}
	orphans := 0
	for _, domain := range providerDomains {
		if _, ok := ledgered[domain]; ok {
			continue
		}
		exists, err := store.DomainExists(ctx, domain)
		if err != nil {
			// Diagnostic-only: a blip checking an unledgered identity must not
			// red a sweep in which every ledgered domain converged. The next
			// hourly sweep re-checks anyway.
			log.Printf("[senderidentity:reaper] orphan check for %s: %v", domain, err)
			continue
		}
		if !exists {
			orphans++
			log.Printf("[senderidentity:reaper] ALERT orphan sending identity with no live domain: %s "+
				"(provider identity exists but is neither ledgered nor backed by a domain row) — manual review required", domain)
		}
	}
	if orphans > 0 {
		log.Printf("[senderidentity:reaper] swept %d provider identities, %d orphan(s) flagged", len(providerDomains), orphans)
	}
	return errors.Join(errs...)
}
