package senderidentity

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

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
	SweepID       int64  `json:"sweep_id,omitempty" river:"unique"`
	AfterDomain   string `json:"after_domain,omitempty" river:"unique"`
	Phase         string `json:"phase,omitempty" river:"unique"`
	ProviderToken string `json:"provider_token,omitempty" river:"unique"`
}

func (ReapV2Args) Kind() string { return "sender_identity_reap_v2" }

const reapPhaseOrphans = "orphan_audit"

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
	// reclaim is the orphan-reclaim policy (see ReclaimConfig). Its ZERO VALUE
	// — every unit test that builds a ReapWorker by struct literal, and every
	// deployment that does not configure the block — reclaims nothing.
	reclaim ReclaimConfig
	// enqueueNext is injected with the River continuation in production and a
	// recorder/no-op in unit tests.
	enqueueNext func(context.Context, ReapV2Args) error
}

func (w *ReapWorker) Work(ctx context.Context, job *river.Job[ReapV2Args]) error {
	enqueueNext := w.enqueueNext
	if enqueueNext == nil {
		enqueueNext = func(context.Context, ReapV2Args) error { return nil }
	}
	if job.Args.Phase == reapPhaseOrphans {
		return reapProviderOrphanPage(ctx, w.store, w.provider, w.reclaim, job.Args, 25, enqueueNext)
	}
	return reapManagedIdentityPage(ctx, w.store, w.provider, w.fire, w.maxReconcileAttempt, true, w.legacyJobs, job.Args, 25, enqueueNext)
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
	needsProvision, managed, err := w.store.LookupManagedSendingIdentityDomain(ctx, job.Args.Domain)
	if err != nil {
		return err
	}
	if !managed {
		return nil
	}
	_, err = syncProviderIdentityForReaper(ctx, job.Args.Domain, w.store, w.provider, w.fire, w.maxReconcileAttempt, needsProvision, true, w.legacyJobs)
	return err
}

func reapManagedIdentities(ctx context.Context, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, finalizeDeletion, legacyJobs bool) error {
	managed, needsProvision, err := store.ListManagedSendingIdentityDomains(ctx)
	if err != nil {
		return err
	}
	sort.Strings(managed)
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
		if _, err := syncProviderIdentity(ctx, domain, store, provider, fire, maxReconcileAttempt, !providerPresent || needsProvision[domain], finalizeDeletion, legacyJobs); err != nil {
			recordReaperError(domain, err, &errs)
		}
	}
	auditProviderOrphans(ctx, store, managed, providerDomains)
	return errors.Join(errs...)
}

func reapManagedIdentityPage(ctx context.Context, store Store, provider Provider, fire EventFirer, maxReconcileAttempt int, finalizeDeletion, legacyJobs bool, args ReapV2Args, pageSize int, enqueueNext func(context.Context, ReapV2Args) error) error {
	page, needsProvision, hasMore, err := store.ListManagedSendingIdentityDomainsPage(ctx, args.AfterDomain, pageSize)
	if err != nil {
		return err
	}
	// Hand off before provider calls. The terminal ledger page starts the
	// separately paged provider-only audit; neither phase can monopolize the
	// single sender-identity-v2 worker.
	next := ReapV2Args{SweepID: args.SweepID, Phase: reapPhaseOrphans}
	if hasMore {
		next = ReapV2Args{SweepID: args.SweepID, AfterDomain: page[len(page)-1]}
	}
	if err := enqueueNext(ctx, next); err != nil {
		return err
	}

	var errs []error
	for _, domain := range page {
		if _, err := syncProviderIdentityForReaper(ctx, domain, store, provider, fire, maxReconcileAttempt, needsProvision[domain], finalizeDeletion, legacyJobs); err != nil {
			recordReaperError(domain, err, &errs)
		}
	}
	if len(page) > 0 {
		log.Printf("[senderidentity:reaper] converged %d managed identity candidate(s), %d error(s)", len(page), len(errs))
	}
	return errors.Join(errs...)
}

func reapProviderOrphanPage(ctx context.Context, store Store, provider Provider, reclaim ReclaimConfig, args ReapV2Args, pageSize int, enqueueNext func(context.Context, ReapV2Args) error) error {
	providerDomains, nextToken, err := provider.ListPage(ctx, args.ProviderToken, pageSize)
	if err != nil {
		return err
	}
	if nextToken != "" {
		if err := enqueueNext(ctx, ReapV2Args{SweepID: args.SweepID, Phase: reapPhaseOrphans, ProviderToken: nextToken}); err != nil {
			return err
		}
	}
	orphans := 0
	// reclaimed counts deletions in THIS JOB INVOCATION only. The orphan phase
	// is paginated across River jobs (each page is its own job), so this is a
	// per-page budget, not a global one: a sweep over N pages can delete up to
	// N*MaxPerSweep. That is deliberate — a genuinely global cap would need
	// cross-job state, and the cap's purpose is to bound the blast radius of a
	// systematic mistake to something a human notices in the log, which a
	// per-page bound already achieves.
	reclaimed := 0
	for _, domain := range providerDomains {
		_, ledgered, err := store.LookupManagedSendingIdentityDomain(ctx, domain)
		if err != nil {
			log.Printf("[senderidentity:reaper] orphan ledger check for %s: %v", domain, err)
			continue
		}
		if ledgered {
			continue
		}
		exists, err := store.DomainExists(ctx, domain)
		if err != nil {
			log.Printf("[senderidentity:reaper] orphan check for %s: %v", domain, err)
			continue
		}
		if !exists {
			orphans++
			reclaimed += reclaimProviderOrphan(ctx, provider, reclaim, domain, reclaimed)
		}
	}
	if orphans > 0 {
		log.Printf("[senderidentity:reaper] audited %d provider identities in this page, %d orphan(s) flagged, %d reclaimed", len(providerDomains), orphans, reclaimed)
	}
	return nil
}

// reapOrphanAlert is the standing operator signal for an identity the reaper
// found at the provider with neither a ledger row nor a domain row. It always
// carries WHY the identity was not reclaimed, so "8 orphans accumulated" is
// diagnosable from the log alone instead of needing a manual AWS session.
const reapOrphanAlert = "[senderidentity:reaper] ALERT orphan sending identity with no live domain: %s " +
	"(provider identity exists but is neither ledgered nor backed by a domain row) — manual review required (not reclaimed: %s)"

// reclaimProviderOrphan runs the reclaim decision for one confirmed orphan and
// returns 1 if it deleted the identity. Every path is non-fatal: this is a
// cleanup pass layered on a diagnostic audit, and neither a provider read
// failure nor a refused deletion may red a sweep in which every LEDGERED
// domain converged.
//
// Deletion goes through provider.Deprovision — never a direct delete — so the
// provider independently re-reads the ownership tag and returns
// ErrIdentityNotOwned if it disagrees. That is the defense in depth that keeps
// a bug in the tag logic above from being sufficient on its own to destroy an
// identity.
func reclaimProviderOrphan(ctx context.Context, provider Provider, reclaim ReclaimConfig, domain string, alreadyReclaimed int) int {
	// Answered without a provider call, so an unconfigured deployment — every
	// self-host, and prod until an operator opts in — keeps paying exactly what
	// the alert-only audit paid before reclaim existed.
	if _, reason := reclaimPolicyUsable(reclaim); reason != "" {
		log.Printf(reapOrphanAlert, domain, reason)
		return 0
	}
	audit, err := provider.InspectIdentity(ctx, domain)
	if err != nil {
		// Includes ErrIdentityNotFound (the identity vanished between the list
		// and this read). No facts means no deletion, by construction.
		log.Printf(reapOrphanAlert, domain, fmt.Sprintf("provider inspect failed: %v", err))
		return 0
	}
	ok, reason := orphanReclaimable(audit, reclaim, time.Now())
	if !ok {
		log.Printf(reapOrphanAlert, domain, reason)
		return 0
	}
	if !reclaim.Enabled {
		// Observe-only mode: the full decision ran and said yes, but nothing is
		// mutated. The operator runs here for days and reads these lines before
		// arming sender_identity.reap_orphans. The per-sweep cap is deliberately
		// NOT applied on this path — the point of the dry run is to see EVERY
		// candidate, not a truncated preview.
		log.Printf("[senderidentity:reaper] WOULD DELETE orphan sending identity %s (%s) — sender_identity.reap_orphans is off, no provider call made", domain, reason)
		return 0
	}
	if alreadyReclaimed >= reclaim.MaxPerSweep {
		log.Printf(reapOrphanAlert, domain, fmt.Sprintf("per-sweep reclaim cap of %d already reached in this job; a later sweep will retry", reclaim.MaxPerSweep))
		return 0
	}
	if err := provider.Deprovision(ctx, domain); err != nil {
		// Deprovision's own ownership re-check refusing here means the tag
		// reasoning above and the provider disagree — exactly the case this
		// second gate exists for. Loud, and never fatal to the sweep.
		log.Printf(reapOrphanAlert, domain, fmt.Sprintf("delete failed: %v", err))
		return 0
	}
	log.Printf("[senderidentity:reaper] DELETED orphan sending identity %s (%s)", domain, reason)
	return 1
}

func recordReaperError(domain string, err error, errs *[]error) {
	if errors.Is(err, ErrIdentityNotOwned) {
		log.Printf("[senderidentity:reaper] ALERT teardown blocked for %s: %v", domain, err)
	}
	*errs = append(*errs, fmt.Errorf("%s: %w", domain, err))
}

func auditProviderOrphans(ctx context.Context, store Store, managed, providerDomains []string) {
	ledgered := make(map[string]struct{}, len(managed))
	for _, domain := range managed {
		ledgered[domain] = struct{}{}
	}
	for _, domain := range providerDomains {
		if _, ok := ledgered[domain]; ok {
			continue
		}
		exists, err := store.DomainExists(ctx, domain)
		if err != nil {
			log.Printf("[senderidentity:reaper] orphan check for %s: %v", domain, err)
			continue
		}
		if !exists {
			log.Printf("[senderidentity:reaper] ALERT orphan sending identity with no live domain: %s "+
				"(provider identity exists but is neither ledgered nor backed by a domain row) — manual review required", domain)
		}
	}
}
