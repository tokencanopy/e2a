package senderidentity

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
	"github.com/riverqueue/river/rivertype"
)

type blockingProvisionProvider struct {
	*FakeProvider
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type blockingStatusProvider struct {
	*FakeProvider
	statusStarted    chan struct{}
	statusRelease    chan struct{}
	provisionStarted chan struct{}
	statusOnce       sync.Once
	provisionOnce    sync.Once
}

func (p *blockingStatusProvider) Status(ctx context.Context, domain string) (Result, error) {
	p.statusOnce.Do(func() { close(p.statusStarted) })
	select {
	case <-p.statusRelease:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return Result{Status: StatusVerified}, nil
}

func (p *blockingStatusProvider) Provision(ctx context.Context, domain, selector string, key []byte) (Result, error) {
	p.provisionOnce.Do(func() { close(p.provisionStarted) })
	return p.FakeProvider.Provision(ctx, domain, selector, key)
}

func (p *blockingProvisionProvider) Provision(ctx context.Context, domain, selector string, key []byte) (Result, error) {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return p.FakeProvider.Provision(ctx, domain, selector, key)
}

// workCtxWithClient returns a context carrying a real (but DB-less) River
// client, shaped like the one River hands a live Work() call. The
// ProvisionWorker pending branch needs a client in context to enqueue the
// reconcile job. The returned cleanup closes the pool.
//
// The production pending branch uses river.ClientFromContextSafely (NOT
// ClientFromContext, which PANICS when no client is present): a missing client
// falls back to a clean no-enqueue pass — exercised by the
// "client-less ctx falls back cleanly" subtest below, which passes a bare
// context.Background().
func workCtxWithClient(t *testing.T) context.Context {
	t.Helper()
	// An unreachable DSN: we never connect, we only need a constructed client
	// so ClientFromContext returns non-nil and the pending branch proceeds.
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	client, err := river.NewClient[pgx.Tx](riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	return rivertest.WorkContext[pgx.Tx](context.Background(), client)
}

func reconcileJob(domain string, attempt, maxAttempts int) *river.Job[ReconcileArgs] {
	return &river.Job[ReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: maxAttempts, Kind: ReconcileArgs{}.Kind()},
		Args:   ReconcileArgs{Domain: domain, Incarnation: domain + "-incarnation"},
	}
}

func TestReconcileWorker_Work(t *testing.T) {
	const domain = "example.com"
	const owner = "user_1"

	t.Run("already verified is a no-op", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusVerified)
		store.setOwner(domain, owner)
		prov := NewFakeProvider()
		firer := &recordingFirer{}
		w := &ReconcileWorker{store: store, provider: prov, fire: firer.fire()}

		if err := w.Work(context.Background(), reconcileJob(domain, 1, 12)); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.StatusCalls) != 0 {
			t.Fatalf("provider.Status should not be called, got %d calls", len(prov.StatusCalls))
		}
		if firer.count() != 0 {
			t.Fatalf("firer should not fire, got %d", firer.count())
		}
	})

	t.Run("provider verified sets verified and fires", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusPending)
		store.setOwner(domain, owner)
		prov := NewFakeProvider()
		prov.SetStatus(domain, Result{Status: StatusVerified})
		firer := &recordingFirer{}
		baseFire := firer.fire()
		firedWhileLocked := false
		w := &ReconcileWorker{store: store, provider: prov, fire: func(ctx context.Context, domain, userID string, status Status, errMsg string) {
			firedWhileLocked = store.mutationHeld()
			baseFire(ctx, domain, userID, status, errMsg)
		}}

		if err := w.Work(context.Background(), reconcileJob(domain, 1, 12)); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		got, ok := store.lastSetStatus()
		if !ok || got.Status != StatusVerified {
			t.Fatalf("expected status verified, got %+v ok=%v", got, ok)
		}
		ev, ok := firer.last()
		if !ok || ev.Status != StatusVerified || ev.UserID != owner {
			t.Fatalf("expected fired verified for owner, got %+v ok=%v", ev, ok)
		}
		if firedWhileLocked {
			t.Fatal("event fired while the sender-identity mutation lock was held")
		}
	})

	t.Run("provider failed sets failed and fires", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusPending)
		store.setOwner(domain, owner)
		prov := NewFakeProvider()
		// Mixed axes: the rollup is failed but DKIM is fine and only the custom
		// MAIL FROM broke. The worker must persist the per-axis breakdown so the
		// API can surface exactly which record to fix.
		prov.SetStatus(domain, Result{Status: StatusFailed, DkimStatus: StatusVerified, MailFromStatus: StatusFailed, Error: "MAIL FROM rejected"})
		firer := &recordingFirer{}
		w := &ReconcileWorker{store: store, provider: prov, fire: firer.fire()}

		if err := w.Work(context.Background(), reconcileJob(domain, 1, 12)); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		got, _ := store.lastSetStatus()
		if got.Status != StatusFailed || got.ErrMsg != "MAIL FROM rejected" {
			t.Fatalf("expected failed with reason, got %+v", got)
		}
		if got.DkimStatus != StatusVerified || got.MailFromStatus != StatusFailed {
			t.Fatalf("expected per-axis dkim=verified mailFrom=failed persisted, got dkim=%q mailFrom=%q", got.DkimStatus, got.MailFromStatus)
		}
		ev, _ := firer.last()
		if ev.Status != StatusFailed || ev.ErrMsg != "MAIL FROM rejected" {
			t.Fatalf("expected fired failed, got %+v", ev)
		}
	})

	t.Run("pending with attempts left returns retry signal", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusPending)
		store.setOwner(domain, owner)
		prov := NewFakeProvider()
		prov.SetStatus(domain, Result{Status: StatusPending})
		firer := &recordingFirer{}
		w := &ReconcileWorker{store: store, provider: prov, fire: firer.fire()}

		err := w.Work(context.Background(), reconcileJob(domain, 1, 12))
		if err == nil {
			t.Fatalf("expected retry error, got nil")
		}
		if !errors.Is(err, errStillPending) {
			t.Fatalf("expected errStillPending, got %v", err)
		}
		if len(store.TouchCalls) != 1 {
			t.Fatalf("expected TouchSendingChecked called once, got %d", len(store.TouchCalls))
		}
		if st, _ := store.GetSendingStatus(context.Background(), domain); st != StatusPending {
			t.Fatalf("status should stay pending, got %v", st)
		}
		if len(store.SetStatusCalls) != 0 {
			t.Fatalf("SetSendingStatus should not be called on a still-pending poll, got %d", len(store.SetStatusCalls))
		}
	})

	t.Run("pending with attempts exhausted times out (TTL path)", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusPending)
		store.setOwner(domain, owner)
		prov := NewFakeProvider()
		prov.SetStatus(domain, Result{Status: StatusPending})
		firer := &recordingFirer{}
		w := &ReconcileWorker{store: store, provider: prov, fire: firer.fire()}

		// Attempt >= MaxAttempts → no more retries; mark failed.
		if err := w.Work(context.Background(), reconcileJob(domain, 12, 12)); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(store.TouchCalls) != 1 {
			t.Fatalf("expected TouchSendingChecked called once before TTL check, got %d", len(store.TouchCalls))
		}
		got, _ := store.lastSetStatus()
		if got.Status != StatusFailed || got.ErrMsg != "verification timed out" {
			t.Fatalf("expected failed/timeout, got %+v", got)
		}
		ev, _ := firer.last()
		if ev.Status != StatusFailed {
			t.Fatalf("expected fired failed, got %+v", ev)
		}
	})

	t.Run("identity not found repairs desired state", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusPending)
		store.setOwner(domain, owner)
		store.setProvisionInputs("current-selector", []byte("current-key"), true)
		prov := NewFakeProvider()
		prov.SetStatusNotFound(domain)
		firer := &recordingFirer{}
		w := &ReconcileWorker{store: store, provider: prov, fire: firer.fire()}

		if err := w.Work(context.Background(), reconcileJob(domain, 1, 12)); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		got, _ := store.lastSetStatus()
		if got.Status != StatusPending {
			t.Fatalf("expected repaired identity to restart pending verification, got %+v", got)
		}
		if len(prov.ProvisionCalls) != 1 {
			t.Fatalf("missing provider identity was not recreated: %v", prov.ProvisionCalls)
		}
		if firer.count() != 0 {
			t.Fatalf("repair must not fire a terminal event, got %d", firer.count())
		}
	})

	t.Run("domain gone (ErrNoRows) is a no-op", func(t *testing.T) {
		store := newFakeStore() // domain absent ⇒ GetSendingStatus returns pgx.ErrNoRows
		prov := NewFakeProvider()
		firer := &recordingFirer{}
		w := &ReconcileWorker{store: store, provider: prov, fire: firer.fire()}

		if err := w.Work(context.Background(), reconcileJob(domain, 1, 12)); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(store.SetStatusCalls) != 0 {
			t.Fatalf("nothing should be set when domain is gone, got %d", len(store.SetStatusCalls))
		}
		if len(prov.StatusCalls) != 0 {
			t.Fatalf("provider.Status should not be called when domain is gone, got %d", len(prov.StatusCalls))
		}
		if firer.count() != 0 {
			t.Fatalf("firer should not fire, got %d", firer.count())
		}
	})
}

func TestReconcileWorker_GetStatusRealError(t *testing.T) {
	store := newFakeStore()
	boom := errors.New("db down")
	store.getStatusErr = boom
	w := &ReconcileWorker{store: store, provider: NewFakeProvider()}
	err := w.Work(context.Background(), reconcileJob("example.com", 1, 12))
	if !errors.Is(err, boom) {
		t.Fatalf("expected real DB error to propagate, got %v", err)
	}
}

func TestDeprovisionWorker_Work(t *testing.T) {
	const domain = "gone.example"
	deprovJob := func() *river.Job[DeprovisionArgs] {
		return &river.Job[DeprovisionArgs]{
			JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 3, Kind: DeprovisionArgs{}.Kind()},
			Args:   DeprovisionArgs{Domain: domain},
		}
	}

	t.Run("success calls provider Deprovision", func(t *testing.T) {
		store := newFakeStore() // absent domain => desired provider state is absent
		store.managed[domain] = "deleted-incarnation"
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		w := &DeprovisionWorker{store: store, provider: prov}
		if err := w.Work(context.Background(), deprovJob()); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.DeprovisionCalls) != 1 || prov.DeprovisionCalls[0] != domain {
			t.Fatalf("expected Deprovision(%q), got %v", domain, prov.DeprovisionCalls)
		}
		if store.managed[domain] == "" {
			t.Fatal("worker removed deletion tombstone before the post-drain sweep")
		}
	})

	t.Run("provider error propagates for retry", func(t *testing.T) {
		store := newFakeStore()
		prov := NewFakeProvider()
		boom := errors.New("ses unreachable")
		prov.SetDeprovisionErr(boom)
		w := &DeprovisionWorker{store: store, provider: prov}
		if err := w.Work(context.Background(), deprovJob()); !errors.Is(err, boom) {
			t.Fatalf("expected provider error to propagate, got %v", err)
		}
	})
}

// TestManagerTryDeprovisionNow covers the post-commit best-effort convergence
// the delete handler runs after the row delete + durable teardown job have
// committed. It replaces the synchronous in-tx Deprovision (review findings:
// an untagged/foreign identity made the domain permanently undeletable, and a
// transient SES failure blocked deletion entirely).
func TestManagerTryDeprovisionNow(t *testing.T) {
	const domain = "delete-boundary.example.test"

	t.Run("removes the owned provider identity for a deleted domain", func(t *testing.T) {
		store := newFakeStore() // absent row = deleted domain
		store.managed[domain] = "deleted-incarnation"
		provider := NewFakeProvider()
		provider.SeedIdentity(domain)
		manager := NewManager(store, provider, nil, Config{})

		if err := manager.TryDeprovisionNow(context.Background(), domain); err != nil {
			t.Fatalf("TryDeprovisionNow: %v", err)
		}
		identities, _ := provider.List(context.Background())
		if len(identities) != 0 {
			t.Fatalf("provider identity survived the immediate deprovision: %v", identities)
		}
		if store.managed[domain] == "" {
			t.Fatal("tombstone must be retained for the post-drain audit to finalize")
		}
	})

	t.Run("tolerates a foreign or untagged identity", func(t *testing.T) {
		store := newFakeStore()
		store.managed[domain] = "deleted-incarnation"
		provider := NewFakeProvider()
		provider.SetDeprovisionErr(ErrIdentityNotOwned)
		manager := NewManager(store, provider, nil, Config{})

		if err := manager.TryDeprovisionNow(context.Background(), domain); err != nil {
			t.Fatalf("a not-owned identity is not e2a's to delete and must be tolerated: %v", err)
		}
	})

	t.Run("surfaces transient provider errors for the caller to log", func(t *testing.T) {
		store := newFakeStore()
		store.managed[domain] = "deleted-incarnation"
		provider := NewFakeProvider()
		provider.SetDeprovisionErr(errors.New("ses unavailable"))
		manager := NewManager(store, provider, nil, Config{})

		if err := manager.TryDeprovisionNow(context.Background(), domain); err == nil {
			t.Fatal("transient provider error must surface (the caller logs it; the job converges)")
		}
	})
}

func TestPostDrainSweepRepairsLegacyMutationRaces(t *testing.T) {
	t.Run("late legacy create after delete is removed", func(t *testing.T) {
		const domain = "deleted-race.example.com"
		store := newFakeStore()
		store.managed[domain] = "deleted-incarnation"
		provider := NewFakeProvider()
		provider.SeedIdentity(domain)

		worker := &DeprovisionWorker{store: store, provider: provider}
		if err := worker.Work(context.Background(), &river.Job[DeprovisionArgs]{Args: DeprovisionArgs{Domain: domain}}); err != nil {
			t.Fatalf("initial deprovision: %v", err)
		}
		provider.SeedIdentity(domain)      // old slot finishes Provision after v2 delete
		provider.SetStatusNotFound(domain) // audit only needs presence to choose force; absent DB still deprovisions
		if err := (&PostDrainAuditWorker{store: store, provider: provider}).Work(context.Background(), &river.Job[PostDrainAuditArgs]{Args: PostDrainAuditArgs{Domain: domain}}); err != nil {
			t.Fatalf("post-drain sweep: %v", err)
		}
		identities, _ := provider.List(context.Background())
		if len(identities) != 0 || len(store.managed) != 0 {
			t.Fatalf("late legacy create survived sweep: provider=%v ledger=%v", identities, store.managed)
		}
	})

	t.Run("late legacy delete of replacement is recreated", func(t *testing.T) {
		const domain = "replacement-race.example.com"
		store := newFakeStore()
		store.setStatus(domain, StatusVerified)
		store.setProvisionInputs("current-selector", []byte("current-key"), true)
		provider := NewFakeProvider()
		worker := &SyncWorker{store: store, provider: provider}
		if err := worker.Work(context.Background(), &river.Job[SyncArgs]{Args: SyncArgs{Domain: domain}}); err != nil {
			t.Fatalf("v2 sync: %v", err)
		}
		if err := provider.Deprovision(context.Background(), domain); err != nil { // old slot finishes delete
			t.Fatalf("legacy delete: %v", err)
		}
		provider.SetStatusNotFound(domain)
		if err := (&PostDrainAuditWorker{store: store, provider: provider}).Work(context.Background(), &river.Job[PostDrainAuditArgs]{Args: PostDrainAuditArgs{Domain: domain}}); err != nil {
			t.Fatalf("post-drain sweep: %v", err)
		}
		identities, _ := provider.List(context.Background())
		if len(identities) != 1 || identities[0] != domain || len(provider.ProvisionCalls) != 2 {
			t.Fatalf("late legacy delete was not repaired: identities=%v provisions=%v", identities, provider.ProvisionCalls)
		}
	})
}

// TestPostDrainAuditWindowScopesUniqueness pins the review fix for the
// completed-job dedupe trap: River cannot drop `completed` from a unique
// state set, so a completed audit inside the ~24h retention window silently
// swallowed the next mutation's audit (a domain re-registered and re-deleted
// within retention waited for the hourly reaper instead of the promised
// 15-minute finalizer). Scoping uniqueness by the audit's schedule bucket
// keeps same-window mutations deduped while a completed audit — whose bucket
// is always in the past relative to a new mutation's — can never block one.
func TestPostDrainAuditWindowScopesUniqueness(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	first, opts := postDrainAuditInsert("d.example", now)
	if opts.ScheduledAt != now.Add(postDrainConvergenceDelay) {
		t.Fatalf("ScheduledAt = %v, want now+%v", opts.ScheduledAt, postDrainConvergenceDelay)
	}
	if !opts.UniqueOpts.ByArgs || !opts.UniqueOpts.ByQueue {
		t.Fatalf("audit insert must stay unique by args+queue, got %+v", opts.UniqueOpts)
	}

	sameWindow, _ := postDrainAuditInsert("d.example", now.Add(30*time.Second))
	if first.Window != sameWindow.Window {
		t.Fatalf("mutations 30s apart must share a window: %d vs %d", first.Window, sameWindow.Window)
	}
	later, _ := postDrainAuditInsert("d.example", now.Add(postDrainConvergenceDelay+time.Minute))
	if first.Window == later.Window {
		t.Fatalf("a mutation after the first audit's schedule must get a fresh window, both %d", first.Window)
	}
}

func TestLegacyReapRetainsDeletionTombstone(t *testing.T) {
	const domain = "legacy-reap.example.test"
	store := newFakeStore()
	store.managed[domain] = "deleted-incarnation"
	provider := NewFakeProvider()
	provider.SeedIdentity(domain)
	worker := &LegacyReapWorker{store: store, provider: provider}
	if err := worker.Work(context.Background(), &river.Job[ReapArgs]{Args: ReapArgs{}}); err != nil {
		t.Fatalf("legacy reap: %v", err)
	}
	if store.managed[domain] == "" {
		t.Fatal("legacy reap finalized a tombstone while the old slot may still mutate SES")
	}
}

func TestProviderMutationRace_DeletionWinsOverInFlightProvision(t *testing.T) {
	const domain = "race.example.com"
	store := newFakeStore()
	store.setStatus(domain, StatusNone)
	store.setProvisionInputs("sel", []byte("der"), true)
	provider := &blockingProvisionProvider{
		FakeProvider: NewFakeProvider(),
		started:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	provision := &ProvisionWorker{store: store, provider: provider}
	deprovision := &DeprovisionWorker{store: store, provider: provider}

	provisionDone := make(chan error, 1)
	go func() { provisionDone <- provision.Work(context.Background(), provisionJob(domain)) }()
	<-provider.started

	// The domain delete commits while the external Provision call is in flight.
	// Its durable teardown job must serialize behind Provision and converge the
	// provider to the now-absent domain state.
	store.deleteDomain(domain)
	deprovisionDone := make(chan error, 1)
	go func() {
		deprovisionDone <- deprovision.Work(context.Background(), &river.Job[DeprovisionArgs]{
			JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 3, Kind: DeprovisionArgs{}.Kind()},
			Args:   DeprovisionArgs{Domain: domain},
		})
	}()
	close(provider.release)

	if err := <-provisionDone; err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("provision Work: %v", err)
	}
	if err := <-deprovisionDone; err != nil {
		t.Fatalf("deprovision Work: %v", err)
	}
	identities, err := provider.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(identities) != 0 {
		t.Fatalf("provider identity survived delete/provision interleaving: %v", identities)
	}
}

func TestProviderMutationRace_RefreshWinsOverStaleVerifiedPoll(t *testing.T) {
	const domain = "refresh-race.example.com"
	store := newFakeStore()
	store.setStatus(domain, StatusPending)
	store.setProvisionInputs("replacement-selector", []byte("replacement-key"), true)
	provider := &blockingStatusProvider{
		FakeProvider:     NewFakeProvider(),
		statusStarted:    make(chan struct{}),
		statusRelease:    make(chan struct{}),
		provisionStarted: make(chan struct{}),
	}
	reconcile := &ReconcileV2Worker{store: store, provider: provider}
	syncWorker := &SyncWorker{store: store, provider: provider}

	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- reconcile.Work(context.Background(), &river.Job[ReconcileV2Args]{
			JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 12, Kind: ReconcileV2Args{}.Kind()},
			Args:   ReconcileV2Args{Domain: domain, Incarnation: domain + "-incarnation"},
		})
	}()
	<-provider.statusStarted

	syncDone := make(chan error, 1)
	go func() {
		syncDone <- syncWorker.Work(context.Background(), &river.Job[SyncArgs]{Args: SyncArgs{Domain: domain}})
	}()
	select {
	case <-provider.provisionStarted:
		t.Fatal("refresh provision crossed an in-flight provider status snapshot")
	case <-time.After(100 * time.Millisecond):
	}
	close(provider.statusRelease)
	if err := <-reconcileDone; err != nil {
		t.Fatalf("reconcile Work: %v", err)
	}
	if err := <-syncDone; err != nil {
		t.Fatalf("sync Work: %v", err)
	}
	if got, _ := store.GetSendingStatus(context.Background(), domain); got != StatusPending {
		t.Fatalf("status = %q, want pending after replacement key refresh", got)
	}
}

func TestDeprovisionWorker_ConvergesReRegisteredDomain(t *testing.T) {
	const domain = "reclaimed.example.com"

	t.Run("verified replacement is provisioned rather than deleted", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusNone)
		store.setProvisionInputs("new-selector", []byte("new-key"), true)
		provider := NewFakeProvider()
		provider.SeedIdentity(domain) // identity from the deleted incarnation
		worker := &DeprovisionWorker{store: store, provider: provider}

		if err := worker.Work(context.Background(), &river.Job[DeprovisionArgs]{Args: DeprovisionArgs{Domain: domain}}); err != nil {
			t.Fatalf("Work: %v", err)
		}
		if len(provider.DeprovisionCalls) != 0 || len(provider.ProvisionCalls) != 1 {
			t.Fatalf("replacement must converge via provision; provision=%v deprovision=%v", provider.ProvisionCalls, provider.DeprovisionCalls)
		}
	})

	t.Run("unverified replacement keeps provider identity absent", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusNone)
		store.setVerified(domain, false)
		store.setProvisionInputs("new-selector", []byte("new-key"), true)
		provider := NewFakeProvider()
		provider.SeedIdentity(domain)
		worker := &DeprovisionWorker{store: store, provider: provider}

		if err := worker.Work(context.Background(), &river.Job[DeprovisionArgs]{Args: DeprovisionArgs{Domain: domain}}); err != nil {
			t.Fatalf("Work: %v", err)
		}
		identities, _ := provider.List(context.Background())
		if len(identities) != 0 || len(provider.ProvisionCalls) != 0 || len(provider.DeprovisionCalls) != 1 {
			t.Fatalf("unverified replacement must converge to absent; identities=%v provision=%v deprovision=%v", identities, provider.ProvisionCalls, provider.DeprovisionCalls)
		}
	})
}

func reapJob() *river.Job[ReapV2Args] {
	return &river.Job[ReapV2Args]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 1, Kind: ReapV2Args{}.Kind()},
		Args:   ReapV2Args{},
	}
}

func TestReapWorker_Work(t *testing.T) {
	t.Run("deletes a managed orphan", func(t *testing.T) {
		store := newFakeStore()
		store.managed["b.example"] = "old-incarnation"
		prov := NewFakeProvider()
		prov.SeedIdentity("b.example")
		w := &ReapWorker{store: store, provider: prov}

		if err := w.Work(context.Background(), reapJob()); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		identities, _ := prov.List(context.Background())
		if len(identities) != 0 || len(store.managed) != 0 {
			t.Fatalf("managed orphan survived: provider=%v ledger=%v", identities, store.managed)
		}
	})

	t.Run("alerts on an unledgered orphan identity without touching it", func(t *testing.T) {
		// A pre-upgrade orphan: its domain row died under the old release, so
		// migration 101's live-rows backfill never ledgered it and the sweep
		// will never converge it. The old reaper's ALERT was the only operator
		// signal for this class — removing it made such identities permanently
		// invisible (review finding).
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity("preupgrade-orphan.example")
		var buf bytes.Buffer
		prevOut := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(prevOut)
		w := &ReapWorker{store: store, provider: prov}

		if err := w.Work(context.Background(), reapJob()); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		logged := buf.String()
		if !strings.Contains(logged, "ALERT orphan sending identity") || !strings.Contains(logged, "preupgrade-orphan.example") {
			t.Fatalf("expected an orphan ALERT for the unledgered identity, got: %q", logged)
		}
		identities, _ := prov.List(context.Background())
		if len(identities) != 1 {
			t.Fatalf("alert-only path must never mutate the identity: %v", identities)
		}
	})

	t.Run("does not alert for a live domain outside the ledger", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus("live-unledgered.example", StatusVerified)
		prov := NewFakeProvider()
		prov.SeedIdentity("live-unledgered.example")
		var buf bytes.Buffer
		prevOut := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(prevOut)
		w := &ReapWorker{store: store, provider: prov}

		if err := w.Work(context.Background(), reapJob()); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if strings.Contains(buf.String(), "ALERT") {
			t.Fatalf("live domain must not be flagged as an orphan: %q", buf.String())
		}
	})

	t.Run("an orphan-check error does not fail the convergence sweep", func(t *testing.T) {
		// The alert loop is a diagnostic; a DB blip while checking an identity
		// that is not even ledgered must not red a sweep in which every
		// ledgered domain converged (adversarial-review finding).
		store := newFakeStore()
		store.domainExistsErr = errors.New("pool exhausted")
		prov := NewFakeProvider()
		prov.SeedIdentity("someone-elses.example")
		w := &ReapWorker{store: store, provider: prov}

		if err := w.Work(context.Background(), reapJob()); err != nil {
			t.Fatalf("alert-only orphan check failed the sweep: %v", err)
		}
	})

	t.Run("does not touch unmanaged provider identities", func(t *testing.T) {
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity("shared.example")
		w := &ReapWorker{store: store, provider: prov}

		if err := w.Work(context.Background(), reapJob()); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		identities, _ := prov.List(context.Background())
		if len(identities) != 1 || identities[0] != "shared.example" {
			t.Fatalf("unmanaged identity was mutated: %v", identities)
		}
	})

	t.Run("recreates a missing managed live identity", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus("live.example", StatusVerified)
		store.setProvisionInputs("current-selector", []byte("current-key"), true)
		store.managed["live.example"] = "live.example-incarnation"
		prov := NewFakeProvider()
		w := &ReapWorker{store: store, provider: prov}

		if err := w.Work(context.Background(), reapJob()); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.ProvisionCalls) != 1 {
			t.Fatalf("missing live identity was not recreated: %v", prov.ProvisionCalls)
		}
	})

	t.Run("repairs an unapplied replacement even when an old identity exists", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus("replacement.example", StatusVerified)
		store.setProvisionInputs("new-selector", []byte("new-key"), true)
		store.managed["replacement.example"] = "replacement.example-incarnation"
		store.applied["replacement.example"] = "old-incarnation"
		prov := NewFakeProvider()
		prov.SeedIdentity("replacement.example")
		w := &ReapWorker{store: store, provider: prov}

		if err := w.Work(context.Background(), reapJob()); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.ProvisionCalls) != 1 {
			t.Fatalf("unapplied replacement was not refreshed: %v", prov.ProvisionCalls)
		}
	})

	t.Run("healthy applied identity is not reprovisioned every sweep", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus("healthy.example", StatusVerified)
		store.setProvisionInputs("selector", []byte("key"), true)
		store.managed["healthy.example"] = "healthy.example-incarnation"
		store.applied["healthy.example"] = "healthy.example-incarnation"
		prov := NewFakeProvider()
		prov.SeedIdentity("healthy.example")
		w := &ReapWorker{store: store, provider: prov}

		if err := w.Work(context.Background(), reapJob()); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.ProvisionCalls) != 0 || len(prov.DeprovisionCalls) != 0 {
			t.Fatalf("healthy identity was mutated: provision=%v deprovision=%v", prov.ProvisionCalls, prov.DeprovisionCalls)
		}
	})
}

// TestReapWorker_FiresStatusEvents pins the review fix: reaper-driven
// sending-status transitions must emit the same domain.sending_failed /
// sending_verified webhooks as worker-driven ones. Before the fix the reap and
// audit workers were built with fire=nil, so a customer whose domain lost
// sending capability via the hourly sweep got no notification at all.
func TestReapWorker_FiresStatusEvents(t *testing.T) {
	const domain = "reaped-foreign.example"
	store := newFakeStore()
	store.setStatus(domain, StatusVerified)
	store.setOwner(domain, "u1")
	store.setProvisionInputs("sel", []byte("der"), true)
	store.managed[domain] = domain + "-incarnation" // unapplied ⇒ needsProvision
	prov := NewFakeProvider()
	prov.SeedIdentity(domain)
	prov.SetProvisionErr(ErrIdentityNotOwned)
	firer := &recordingFirer{}
	w := &ReapWorker{store: store, provider: prov, fire: firer.fire()}

	if err := w.Work(context.Background(), reapJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got, _ := store.GetSendingStatus(context.Background(), domain); got != StatusFailed {
		t.Fatalf("status = %q, want failed for a not-owned identity", got)
	}
	ev, ok := firer.last()
	if !ok || ev.Status != StatusFailed || ev.UserID != "u1" {
		t.Fatalf("reaper transition fired %+v ok=%v, want domain.sending_failed for u1", ev, ok)
	}
}

// TestPostDrainAuditWorker_FiresStatusEvents covers the same review fix for
// the delayed post-drain finalizer.
func TestPostDrainAuditWorker_FiresStatusEvents(t *testing.T) {
	const domain = "audited-foreign.example"
	store := newFakeStore()
	store.setStatus(domain, StatusVerified)
	store.setOwner(domain, "u2")
	store.setProvisionInputs("sel", []byte("der"), true)
	store.managed[domain] = domain + "-incarnation"
	prov := NewFakeProvider()
	prov.SeedIdentity(domain)
	prov.SetProvisionErr(ErrIdentityNotOwned)
	firer := &recordingFirer{}
	w := &PostDrainAuditWorker{store: store, provider: prov, fire: firer.fire()}

	if err := w.Work(context.Background(), &river.Job[PostDrainAuditArgs]{Args: PostDrainAuditArgs{Domain: domain}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	ev, ok := firer.last()
	if !ok || ev.Status != StatusFailed || ev.UserID != "u2" {
		t.Fatalf("audit transition fired %+v ok=%v, want domain.sending_failed for u2", ev, ok)
	}
}

// TestReconcileWorker_NotOwnedForgetFailureStillFires pins the review fix for
// the lost-event window: the failed status is committed, then the ledger
// Forget hits a transient DB error. The job retries (error propagates), but
// the retry short-circuits on status != pending — so the event MUST fire on
// this attempt or it is lost for good.
func TestReconcileWorker_NotOwnedForgetFailureStillFires(t *testing.T) {
	const domain = "forget-blip.example"
	store := newFakeStore()
	store.setStatus(domain, StatusPending)
	store.setOwner(domain, "u3")
	store.forgetErr = errors.New("db blip")
	prov := NewFakeProvider()
	prov.SetStatusErr(domain, ErrIdentityNotOwned)
	firer := &recordingFirer{}
	w := &ReconcileWorker{store: store, provider: prov, fire: firer.fire()}

	err := w.Work(context.Background(), reconcileJob(domain, 1, 12))
	if err == nil {
		t.Fatal("Forget failure must propagate so River retries the ledger cleanup")
	}
	if got, _ := store.GetSendingStatus(context.Background(), domain); got != StatusFailed {
		t.Fatalf("status = %q, want failed committed before the Forget error", got)
	}
	ev, ok := firer.last()
	if !ok || ev.Status != StatusFailed {
		t.Fatalf("fired %+v ok=%v, want domain.sending_failed despite the Forget error", ev, ok)
	}
}

// TestSyncWorker_NotOwnedForgetFailureStillFires: same window in the sync
// (provision) path — status commit succeeded, ledger Forget failed. The
// adversarial review then proved the naive fix fires once per River attempt
// (the sync path re-executes the same terminal transition on retry, unlike
// reconcile's status!=pending guard), so this also pins exactly-one event
// across the retries: the first attempt's write CHANGES the status and fires;
// the retry's failed→failed rewrite must not.
func TestSyncWorker_NotOwnedForgetFailureStillFires(t *testing.T) {
	const domain = "sync-forget-blip.example"
	store := newFakeStore()
	store.setStatus(domain, StatusVerified)
	store.setOwner(domain, "u4")
	store.setProvisionInputs("sel", []byte("der"), true)
	store.forgetErr = errors.New("db blip")
	prov := NewFakeProvider()
	prov.SetProvisionErr(ErrIdentityNotOwned)
	firer := &recordingFirer{}
	w := &SyncWorker{store: store, provider: prov, fire: firer.fire()}

	err := w.Work(context.Background(), &river.Job[SyncArgs]{Args: SyncArgs{Domain: domain}})
	if err == nil {
		t.Fatal("Forget failure must propagate so River retries the ledger cleanup")
	}
	ev, ok := firer.last()
	if !ok || ev.Status != StatusFailed {
		t.Fatalf("fired %+v ok=%v, want domain.sending_failed despite the Forget error", ev, ok)
	}
	// River retries (Forget still failing): the rewrite is failed→failed — no
	// duplicate webhook.
	for attempt := 2; attempt <= 4; attempt++ {
		if err := w.Work(context.Background(), &river.Job[SyncArgs]{Args: SyncArgs{Domain: domain}}); err == nil {
			t.Fatalf("attempt %d: Forget still failing must still propagate", attempt)
		}
	}
	if firer.count() != 1 {
		t.Fatalf("fired %d events over 4 attempts, want exactly 1 (no duplicate per retry)", firer.count())
	}
}

// TestReapWorker_StuckFailedDomainDoesNotRefireHourly pins the independent
// review's deploy-burst finding: with fire wired into the reaper, a domain
// stuck in `failed` whose identity is missing/foreign would re-emit
// domain.sending_failed on EVERY hourly sweep (and on the RunOnStart sweep of
// every deploy). A failed→failed rewrite is not a transition and must not
// notify the customer again.
func TestReapWorker_StuckFailedDomainDoesNotRefireHourly(t *testing.T) {
	const domain = "stuck-failed.example"
	store := newFakeStore()
	store.setStatus(domain, StatusFailed)
	store.setOwner(domain, "u5")
	store.setProvisionInputs("sel", []byte("der"), true)
	store.managed[domain] = domain + "-incarnation" // unapplied ⇒ forced every sweep
	prov := NewFakeProvider()
	prov.SeedIdentity(domain)
	prov.SetProvisionErr(ErrIdentityNotOwned)
	firer := &recordingFirer{}
	w := &ReapWorker{store: store, provider: prov, fire: firer.fire()}

	for sweep := 1; sweep <= 3; sweep++ {
		if err := w.Work(context.Background(), reapJob()); err != nil {
			t.Fatalf("sweep %d: %v", sweep, err)
		}
	}
	if firer.count() != 0 {
		t.Fatalf("fired %d events over 3 sweeps for an already-failed domain, want 0", firer.count())
	}
	if got, _ := store.GetSendingStatus(context.Background(), domain); got != StatusFailed {
		t.Fatalf("status = %q, want failed maintained", got)
	}
}

// TestSyncWorker_TerminalWriteFiresEvenWhenMarkAppliedFails extends the
// fire-on-commit rule to the main terminal-write branch (the reviewers found
// it was applied to only two of the three branches): a committed
// verified/failed write must fire even when a subsequent ledger call errors.
func TestSyncWorker_TerminalWriteFiresEvenWhenMarkAppliedFails(t *testing.T) {
	const domain = "mark-applied-blip.example"
	store := newFakeStore()
	store.setStatus(domain, StatusPending)
	store.setOwner(domain, "u6")
	store.setProvisionInputs("sel", []byte("der"), true)
	store.markAppliedErr = errors.New("db blip")
	prov := NewFakeProvider()
	prov.SetProvisionResult(Result{Status: StatusVerified})
	firer := &recordingFirer{}
	w := &SyncWorker{store: store, provider: prov, fire: firer.fire()}

	if err := w.Work(context.Background(), &river.Job[SyncArgs]{Args: SyncArgs{Domain: domain}}); err == nil {
		t.Fatal("MarkApplied failure must propagate for retry")
	}
	ev, ok := firer.last()
	if !ok || ev.Status != StatusVerified {
		t.Fatalf("fired %+v ok=%v, want sending_verified despite the MarkApplied error", ev, ok)
	}
}

func provisionJob(domain string) *river.Job[ProvisionArgs] {
	return &river.Job[ProvisionArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 12, Kind: ProvisionArgs{}.Kind()},
		Args:   ProvisionArgs{Domain: domain},
	}
}

func TestProvisionWorker_Work(t *testing.T) {
	const domain = "example.com"
	const owner = "user_1"

	t.Run("provisions ok then sets pending and attempts reconcile enqueue", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusNone)
		store.setOwner(domain, owner)
		store.setProvisionInputs("sel1", []byte("der-bytes"), true)
		prov := NewFakeProvider() // default Provision → StatusPending
		firer := &recordingFirer{}
		w := &ProvisionWorker{store: store, provider: prov, fire: firer.fire()}

		// A live Work() always has a River client in ctx; the pending branch
		// then enqueues the reconcile job. With a DB-less client the enqueue
		// fails (connection refused), which is what Work returns — but the
		// status MUST already be pending and the event MUST NOT have fired.
		err := w.Work(workCtxWithClient(t), provisionJob(domain))
		if err == nil {
			t.Fatalf("expected reconcile enqueue to fail without a live DB")
		}
		if len(prov.ProvisionCalls) != 1 {
			t.Fatalf("expected Provision called once, got %d", len(prov.ProvisionCalls))
		}
		got, ok := store.lastSetStatus()
		if !ok || got.Status != StatusPending {
			t.Fatalf("expected status pending set before enqueue, got %+v ok=%v", got, ok)
		}
		// pending should not fire an event.
		if firer.count() != 0 {
			t.Fatalf("pending should not fire, got %d", firer.count())
		}
	})

	// The pending branch uses river.ClientFromContextSafely (NOT
	// ClientFromContext, which panics when no client is present). On a
	// client-less context it must fall back to a clean no-enqueue pass: the
	// domain is left pending (provision succeeded) and Work returns nil
	// without panicking. A forced re-check later re-enqueues.
	t.Run("client-less ctx falls back cleanly (no panic, leaves pending)", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusNone)
		store.setProvisionInputs("sel1", []byte("der"), true)
		prov := NewFakeProvider()
		w := &ProvisionWorker{store: store, provider: prov}

		// Background() carries no River client → must NOT panic.
		if err := w.Work(context.Background(), provisionJob(domain)); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if got, _ := store.GetSendingStatus(context.Background(), domain); got != StatusPending {
			t.Fatalf("status = %q, want pending (provisioned but enqueue skipped)", got)
		}
	})

	t.Run("no DKIM key material sets failed and fires", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusNone)
		store.setOwner(domain, owner)
		store.setProvisionInputs("", nil, false) // ok=false
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		firer := &recordingFirer{}
		w := &ProvisionWorker{store: store, provider: prov, fire: firer.fire()}

		if err := w.Work(context.Background(), provisionJob(domain)); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.ProvisionCalls) != 0 {
			t.Fatalf("Provision must not be called without key material, got %d", len(prov.ProvisionCalls))
		}
		identities, _ := prov.List(context.Background())
		if len(identities) != 0 || len(prov.DeprovisionCalls) != 1 {
			t.Fatalf("stale provider identity survived missing-key failure: identities=%v deprovision=%v", identities, prov.DeprovisionCalls)
		}
		got, _ := store.lastSetStatus()
		if got.Status != StatusFailed {
			t.Fatalf("expected failed, got %+v", got)
		}
		if got.ErrMsg == "" {
			t.Fatalf("expected a non-empty failure reason")
		}
		if ev, _ := firer.last(); ev.Status != StatusFailed {
			t.Fatalf("expected fired failed, got %+v", ev)
		}
	})

	t.Run("transient provision error returns error and leaves status", func(t *testing.T) {
		store := newFakeStore()
		store.setStatus(domain, StatusNone)
		store.setOwner(domain, owner)
		store.setProvisionInputs("sel1", []byte("der"), true)
		prov := NewFakeProvider()
		boom := errors.New("ses throttled")
		prov.SetProvisionErr(boom)
		firer := &recordingFirer{}
		w := &ProvisionWorker{store: store, provider: prov, fire: firer.fire()}

		if err := w.Work(context.Background(), provisionJob(domain)); !errors.Is(err, boom) {
			t.Fatalf("expected transient error to propagate, got %v", err)
		}
		if len(store.SetStatusCalls) != 0 {
			t.Fatalf("status must not change on transient error, got %d set calls", len(store.SetStatusCalls))
		}
		if st, _ := store.GetSendingStatus(context.Background(), domain); st != StatusNone {
			t.Fatalf("status should remain none, got %v", st)
		}
	})

	t.Run("stale provision job for a deleted domain converges provider to absent", func(t *testing.T) {
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		w := &ProvisionWorker{store: store, provider: prov}
		if err := w.Work(context.Background(), provisionJob(domain)); err != nil {
			t.Fatalf("expected nil while converging deleted domain, got %v", err)
		}
		if len(prov.ProvisionCalls) != 0 || len(prov.DeprovisionCalls) != 1 || len(store.SetStatusCalls) != 0 {
			t.Fatalf("expected only provider deprovision for deleted domain; provision=%v deprovision=%v", prov.ProvisionCalls, prov.DeprovisionCalls)
		}
	})
}

// TestSyncWorker_AlreadyVerifiedNoOp restores the pre-ledger regression guard
// (the deleted TestProvisionWorker_AlreadyVerifiedNoOp): a forced re-check of
// a HEALTHY domain — ledger-applied current incarnation, provider-confirmed
// verified — must be a read-only no-op. Before the fix, every POST /verify on
// a verified domain re-Put the BYODKIM key, demoted sending_status
// verified→pending (dropping own-address From until the next poll), and
// emitted a duplicate sending_verified event on each flap.
func TestSyncWorker_AlreadyVerifiedNoOp(t *testing.T) {
	const domain = "healthy-recheck.example"
	store := newFakeStore()
	store.setStatus(domain, StatusVerified)
	store.setOwner(domain, "u1")
	store.setProvisionInputs("sel", []byte("der"), true)
	store.managed[domain] = domain + "-incarnation"
	store.applied[domain] = domain + "-incarnation"
	prov := NewFakeProvider()
	prov.SeedIdentity(domain)
	prov.SetStatus(domain, Result{Status: StatusVerified})
	firer := &recordingFirer{}
	w := &SyncWorker{store: store, provider: prov, fire: firer.fire()}

	if err := w.Work(context.Background(), &river.Job[SyncArgs]{Args: SyncArgs{Domain: domain}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(prov.ProvisionCalls) != 0 || len(prov.DeprovisionCalls) != 0 {
		t.Fatalf("healthy re-check must not mutate the provider: provision=%v deprovision=%v", prov.ProvisionCalls, prov.DeprovisionCalls)
	}
	if len(store.SetStatusCalls) != 0 {
		t.Fatalf("healthy re-check must not write status, got %d writes", len(store.SetStatusCalls))
	}
	if got, _ := store.GetSendingStatus(context.Background(), domain); got != StatusVerified {
		t.Fatalf("status = %q, want verified untouched", got)
	}
	if firer.count() != 0 {
		t.Fatalf("healthy re-check fired %d event(s), want none", firer.count())
	}
}

// TestSyncWorker_AppliedButProviderDriftedReprovisions: the healthy
// short-circuit must trust the ledger only when the provider agrees. An
// identity that vanished out-of-band (applied incarnation notwithstanding)
// still takes the full force-provision path — that drift repair is exactly
// what the forced re-check exists for.
func TestSyncWorker_AppliedButProviderDriftedReprovisions(t *testing.T) {
	const domain = "drifted.example"
	store := newFakeStore()
	store.setStatus(domain, StatusVerified)
	store.setProvisionInputs("sel", []byte("der"), true)
	store.managed[domain] = domain + "-incarnation"
	store.applied[domain] = domain + "-incarnation"
	prov := NewFakeProvider()
	prov.SetStatusNotFound(domain)
	w := &SyncWorker{store: store, provider: prov}

	if err := w.Work(context.Background(), &river.Job[SyncArgs]{Args: SyncArgs{Domain: domain}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(prov.ProvisionCalls) != 1 {
		t.Fatalf("drifted identity was not re-provisioned: %v", prov.ProvisionCalls)
	}
	if got, _ := store.GetSendingStatus(context.Background(), domain); got != StatusPending {
		t.Fatalf("status = %q, want pending while the recreated identity verifies", got)
	}
}

// TestSyncWorker_HealthyRecheckTransientStatusErrorRetries: when the healthy
// short-circuit's provider GET fails transiently, the worker has NO signal —
// mutating a (by-ledger) healthy sender on no signal is exactly the flap the
// short-circuit exists to prevent. Retry instead (adversarial-review finding:
// the fall-through let a transient GET error demote verified→pending when the
// subsequent Provision happened to succeed).
func TestSyncWorker_HealthyRecheckTransientStatusErrorRetries(t *testing.T) {
	const domain = "healthy-blip.example"
	store := newFakeStore()
	store.setStatus(domain, StatusVerified)
	store.setProvisionInputs("sel", []byte("der"), true)
	store.managed[domain] = domain + "-incarnation"
	store.applied[domain] = domain + "-incarnation"
	prov := NewFakeProvider()
	prov.SetStatusErr(domain, errors.New("ses throttled"))
	w := &SyncWorker{store: store, provider: prov}

	if err := w.Work(context.Background(), &river.Job[SyncArgs]{Args: SyncArgs{Domain: domain}}); err == nil {
		t.Fatal("transient GET error on a healthy re-check must retry, not converge blind")
	}
	if len(prov.ProvisionCalls) != 0 || len(store.SetStatusCalls) != 0 {
		t.Fatalf("no-signal path mutated state: provision=%v statusWrites=%d", prov.ProvisionCalls, len(store.SetStatusCalls))
	}
	if got, _ := store.GetSendingStatus(context.Background(), domain); got != StatusVerified {
		t.Fatalf("status = %q, want verified untouched", got)
	}
}

// Legacy mutation jobs force current desired state during blue/green rollout.
// This closes the window where an old reconcile could mark a replacement row
// verified before its new BYODKIM key was installed. (The fake store reports
// no applied incarnation here, so the healthy no-op short-circuit must NOT
// engage — a stale-verified row still converges.)
func TestProvisionWorker_AlreadyVerifiedConvergesCurrentKey(t *testing.T) {
	store := newFakeStore()
	store.setStatus("acme.com", StatusVerified)
	store.setProvisionInputs("sel", []byte("der"), true)
	prov := NewFakeProvider()
	w := &ProvisionWorker{store: store, provider: prov}

	if err := w.Work(context.Background(), provisionJob("acme.com")); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(prov.ProvisionCalls) != 1 {
		t.Errorf("Provision must refresh an already-verified replacement, got %d calls", len(prov.ProvisionCalls))
	}
	if got, _ := store.GetSendingStatus(context.Background(), "acme.com"); got != StatusPending {
		t.Errorf("status = %q, want pending after key refresh", got)
	}
}

func TestReconcileWorker_LegacyFinalAttemptConvergesReplacement(t *testing.T) {
	const domain = "replacement.example.com"
	store := newFakeStore()
	store.setStatus(domain, StatusPending)
	store.setProvisionInputs("new-selector", []byte("new-key"), true)
	prov := NewFakeProvider()
	prov.SeedIdentity(domain)
	w := &ReconcileWorker{store: store, provider: prov}
	job := &river.Job[ReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: 25, MaxAttempts: 25, Kind: ReconcileArgs{}.Kind()},
		Args:   ReconcileArgs{Domain: domain}, // pre-upgrade payload: no incarnation
	}

	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(prov.StatusCalls) != 0 || len(prov.ProvisionCalls) != 1 {
		t.Fatalf("legacy poll must provision current state, not poll old identity: provision=%v status=%v", prov.ProvisionCalls, prov.StatusCalls)
	}
	if got, _ := store.GetSendingStatus(context.Background(), domain); got != StatusPending {
		t.Fatalf("replacement status = %q, want pending with a fresh v2 poll budget", got)
	}
}

func TestProvisionWorker_TerminalProviderFailureRemovesStaleIdentity(t *testing.T) {
	const domain = "malformed.example.com"
	store := newFakeStore()
	store.setStatus(domain, StatusNone)
	store.setProvisionInputs("selector", []byte("malformed"), true)
	prov := NewFakeProvider()
	prov.SeedIdentity(domain)
	prov.SetProvisionResult(Result{Status: StatusFailed, Error: "invalid private key"})
	w := &ProvisionWorker{store: store, provider: prov}

	if err := w.Work(context.Background(), provisionJob(domain)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	identities, _ := prov.List(context.Background())
	if len(identities) != 0 || len(prov.DeprovisionCalls) != 1 {
		t.Fatalf("stale provider identity survived terminal provision failure: identities=%v deprovision=%v", identities, prov.DeprovisionCalls)
	}
}

// TestReconcileWorker_LastAttemptTransientErrorFails pins the review fix: a
// transient provider error on the FINAL attempt must mark the domain failed
// (absolute TTL) rather than return an error that River discards, stranding
// the domain in pending forever. A non-last attempt still retries.
func TestReconcileWorker_LastAttemptTransientErrorFails(t *testing.T) {
	store := newFakeStore()
	store.setStatus("acme.com", StatusPending)
	store.setOwner("acme.com", "u1")
	prov := NewFakeProvider()
	prov.SetStatusErr("acme.com", errors.New("ses throttled"))
	firer := &recordingFirer{}
	w := &ReconcileWorker{store: store, provider: prov, fire: firer.fire()}

	// Final attempt: must NOT return an error and must set failed.
	if err := w.Work(context.Background(), reconcileJob("acme.com", 5, 5)); err != nil {
		t.Fatalf("Work on last attempt returned err (would strand pending): %v", err)
	}
	if got, _ := store.GetSendingStatus(context.Background(), "acme.com"); got != StatusFailed {
		t.Errorf("status = %q, want failed after last-attempt transient error", got)
	}
	if firer.count() == 0 {
		t.Error("expected domain.sending_failed to fire on TTL timeout")
	}

	// Non-final attempt: must return the error to trigger a retry.
	store.setStatus("acme.com", StatusPending)
	if err := w.Work(context.Background(), reconcileJob("acme.com", 1, 5)); err == nil {
		t.Error("expected retry error on a non-final transient error")
	}
}
