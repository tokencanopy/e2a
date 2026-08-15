package identity_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func TestSendingIdentityMutationLockSerializesSameDomain(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	// A separate pool + Store bypasses the process-local mutex and proves the
	// PostgreSQL advisory lock serializes another replica/process.
	pool2, err := pgxpool.New(ctx, testutil.TestDBURL())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(pool2.Close)
	store2 := identity.NewStore(pool2)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.WithSendingIdentityMutationLock(ctx, "lock.example.com", func(context.Context) error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondStarted := make(chan struct{})
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- store2.WithSendingIdentityMutationLock(ctx, "lock.example.com", func(context.Context) error {
			close(secondEntered)
			return nil
		})
	}()
	<-secondStarted

	select {
	case <-secondEntered:
		t.Fatal("second callback entered while the first held the same domain lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first lock: %v", err)
	}
	select {
	case <-secondEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second callback did not enter after the first released the lock")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second lock: %v", err)
	}
}

// TestLoadSendingIdentityStateReportsAppliedIncarnation covers the ledger
// join that gates the healthy-recheck no-op: the snapshot must report "" until
// the provider confirmed THIS incarnation applied, and the incarnation after.
func TestLoadSendingIdentityStateReportsAppliedIncarnation(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "applied-state@example.com", "Owner", "applied-state-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "applied-state.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	inc, _, _, _, _, _, applied, _, err := store.LoadSendingIdentityState(ctx, domain)
	if err != nil {
		t.Fatalf("LoadSendingIdentityState: %v", err)
	}
	if applied != "" {
		t.Fatalf("applied = %q before any provider confirmation, want empty", applied)
	}
	if err := store.MarkSendingIdentityManaged(ctx, domain, inc); err != nil {
		t.Fatalf("MarkSendingIdentityManaged: %v", err)
	}
	if err := store.MarkSendingIdentityApplied(ctx, domain, inc); err != nil {
		t.Fatalf("MarkSendingIdentityApplied: %v", err)
	}
	_, _, _, _, _, _, applied, _, err = store.LoadSendingIdentityState(ctx, domain)
	if err != nil {
		t.Fatalf("LoadSendingIdentityState after apply: %v", err)
	}
	if applied != inc {
		t.Fatalf("applied = %q, want the confirmed incarnation %q", applied, inc)
	}
}

// TestSendingIdentityTombstoneFinalizeRespectsAge pins the drain-window guard
// at the SQL layer: FinalizeSendingIdentityTombstone must be a no-op while the
// ledger row's updated_at is inside the window, delete it once aged, and
// TouchSendingIdentityTombstoneTx (the delete tx's stamp) must reset the age.
func TestSendingIdentityTombstoneFinalizeRespectsAge(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "tombstone-age@example.com", "Owner", "tombstone-age-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "tombstone-age.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	inc, _, _, _, _, _, _, _, err := store.LoadSendingIdentityState(ctx, domain)
	if err != nil {
		t.Fatalf("LoadSendingIdentityState: %v", err)
	}
	if err := store.MarkSendingIdentityManaged(ctx, domain, inc); err != nil {
		t.Fatalf("MarkSendingIdentityManaged: %v", err)
	}
	ledgered := func() bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM sender_identity_managed_domains WHERE domain=$1`, domain).Scan(&n); err != nil {
			t.Fatalf("count ledger: %v", err)
		}
		return n == 1
	}

	// Fresh row (Mark stamped now): finalize must be a no-op.
	if err := store.FinalizeSendingIdentityTombstone(ctx, domain, 15*time.Minute); err != nil {
		t.Fatalf("Finalize (fresh): %v", err)
	}
	if !ledgered() {
		t.Fatal("finalize deleted a tombstone inside the drain window")
	}

	// Backdate past the window, then stamp it via the delete-tx touch: the
	// bump must reset the age and keep the row protected.
	if _, err := pool.Exec(ctx, `UPDATE sender_identity_managed_domains SET updated_at = now() - interval '1 hour' WHERE domain=$1`, domain); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.TouchSendingIdentityTombstoneTx(ctx, tx, domain); err != nil {
		t.Fatalf("TouchSendingIdentityTombstoneTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := store.FinalizeSendingIdentityTombstone(ctx, domain, 15*time.Minute); err != nil {
		t.Fatalf("Finalize (touched): %v", err)
	}
	if !ledgered() {
		t.Fatal("finalize ignored the delete-tx stamp and removed a just-mutated tombstone")
	}

	// Aged row: finalize removes it.
	if _, err := pool.Exec(ctx, `UPDATE sender_identity_managed_domains SET updated_at = now() - interval '1 hour' WHERE domain=$1`, domain); err != nil {
		t.Fatalf("backdate again: %v", err)
	}
	if err := store.FinalizeSendingIdentityTombstone(ctx, domain, 15*time.Minute); err != nil {
		t.Fatalf("Finalize (aged): %v", err)
	}
	if ledgered() {
		t.Fatal("finalize left an aged tombstone in place")
	}
}

// TestSendingIdentityMutationGateHonorsContextCancellation pins the review
// fix for the process-wide mutation gate: a waiter whose context is cancelled
// (an HTTP handler on a deadline, a worker shutting down) must unblock with
// ctx.Err() instead of parking forever behind a slow provider call. The two
// goroutines use DIFFERENT domains so the process gate — not the per-domain
// advisory lock — is provably the thing being waited on.
func TestSendingIdentityMutationGateHonorsContextCancellation(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.WithSendingIdentityMutationLock(context.Background(), "gate-holder.example.com", func(context.Context) error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- store.WithSendingIdentityMutationLock(ctx, "gate-waiter.example.com", func(context.Context) error {
			return nil
		})
	}()
	// Let the waiter queue behind the gate, then cancel it.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter stayed parked behind the process-wide mutation gate")
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("gate holder: %v", err)
	}
}

// TestDeleteDomainTxUsesPinnedMutationConnection guards senderIdentityBegin's
// pinned-connection contract: any store write invoked inside a
// WithSendingIdentityMutationLock callback must run on the lock-owning
// connection rather than acquiring a second one. No production caller
// currently runs DeleteDomainTx under the lock (the delete handler enqueues
// in-tx and deprovisions post-commit), but the capability is what keeps every
// lock-scoped store method single-connection — hence the invariant stays
// pinned here with DeleteDomainTx as its exercise.
func TestDeleteDomainTxUsesPinnedMutationConnection(t *testing.T) {
	// Initialize and clean the shared test database, then use a dedicated
	// one-connection pool to prove the advisory-lock callback does not try to
	// acquire a second connection for its transaction.
	_ = testutil.TestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(testutil.TestDBURL())
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("one-connection pool: %v", err)
	}
	t.Cleanup(pool.Close)
	store := identity.NewStore(pool)
	user, err := store.CreateOrGetUser(ctx, "sender-pinned@example.com", "Sender Pinned", "sender-pinned-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "pinned-delete.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	err = store.WithSendingIdentityMutationLock(ctx, domain, func(lockedCtx context.Context) error {
		return store.DeleteDomainTx(lockedCtx, domain, user.ID, nil)
	})
	if err != nil {
		t.Fatalf("DeleteDomainTx under one-connection mutation lock: %v", err)
	}
}

func TestDeleteDomainTxSerializesSameOwnerReRegistration(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	user, err := store.CreateOrGetUser(ctx, "sender-reclaim@example.com", "Sender Reclaim", "sender-reclaim-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "reclaim-during-delete.example.com"
	first, err := store.ClaimOrCreateDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("first ClaimOrCreateDomain: %v", err)
	}

	deleteEntered := make(chan struct{})
	releaseDelete := make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- store.DeleteDomainTx(ctx, domain, user.ID, func(context.Context, pgx.Tx) error {
			close(deleteEntered)
			<-releaseDelete
			return nil
		})
	}()
	<-deleteEntered

	type claimResult struct {
		domain *identity.Domain
		err    error
	}
	claimDone := make(chan claimResult, 1)
	go func() {
		d, err := store.ClaimOrCreateDomain(ctx, domain, user.ID)
		claimDone <- claimResult{domain: d, err: err}
	}()
	select {
	case got := <-claimDone:
		close(releaseDelete)
		<-deleteDone
		t.Fatalf("re-registration returned before delete committed: domain=%+v err=%v", got.domain, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseDelete)
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteDomainTx: %v", err)
	}
	got := <-claimDone
	if got.err != nil {
		t.Fatalf("replacement ClaimOrCreateDomain: %v", got.err)
	}
	if got.domain.VerificationToken == first.VerificationToken {
		t.Fatal("re-registration reused the incarnation deleted by the concurrent transaction")
	}
}

func TestSendingStatusWriteRejectsDeletedIncarnation(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "sender-lock@example.com", "Sender Lock", "sender-lock-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	first, err := store.ClaimOrCreateDomain(ctx, "incarnation.example.com", user.ID)
	if err != nil {
		t.Fatalf("first ClaimOrCreateDomain: %v", err)
	}
	if err := store.DeleteDomain(ctx, first.Domain, user.ID); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	second, err := store.ClaimOrCreateDomain(ctx, first.Domain, user.ID)
	if err != nil {
		t.Fatalf("second ClaimOrCreateDomain: %v", err)
	}
	if first.VerificationToken == second.VerificationToken {
		t.Fatal("delete/re-register unexpectedly reused the verification-token incarnation")
	}

	err = store.SetSendingStatusForIncarnation(ctx, second.Domain, first.VerificationToken, "pending", "", "", "", nil)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale incarnation write error = %v, want pgx.ErrNoRows", err)
	}
	status, err := store.GetSendingStatus(ctx, second.Domain)
	if err != nil {
		t.Fatalf("GetSendingStatus: %v", err)
	}
	if status != "none" {
		t.Fatalf("replacement status = %q, want none", status)
	}
}

func TestManagedSendingIdentityLedgerSurvivesDomainDelete(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "sender-ledger@example.com", "Sender Ledger", "sender-ledger-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain, err := store.ClaimOrCreateDomain(ctx, "ledger.example.com", user.ID)
	if err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.MarkSendingIdentityManaged(ctx, domain.Domain, domain.VerificationToken); err != nil {
		t.Fatalf("MarkSendingIdentityManaged: %v", err)
	}
	if err := store.DeleteDomain(ctx, domain.Domain, user.ID); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	managed, needsProvision, err := store.ListManagedSendingIdentityDomains(ctx)
	if err != nil {
		t.Fatalf("ListManagedSendingIdentityDomains: %v", err)
	}
	if len(managed) != 1 || managed[0] != domain.Domain {
		t.Fatalf("ledger after domain delete = %v, want [%s]", managed, domain.Domain)
	}
	if !needsProvision[domain.Domain] {
		t.Fatal("unapplied ownership mark must remain a provisioning candidate")
	}
	if err := store.MarkSendingIdentityApplied(ctx, domain.Domain, domain.VerificationToken); err != nil {
		t.Fatalf("MarkSendingIdentityApplied: %v", err)
	}
	_, needsProvision, err = store.ListManagedSendingIdentityDomains(ctx)
	if err != nil {
		t.Fatalf("ListManagedSendingIdentityDomains after applied: %v", err)
	}
	if needsProvision[domain.Domain] {
		t.Fatal("confirmed provider incarnation still marked as needing provision")
	}
	if err := store.ForgetSendingIdentityManaged(ctx, domain.Domain); err != nil {
		t.Fatalf("ForgetSendingIdentityManaged: %v", err)
	}
	managed, _, err = store.ListManagedSendingIdentityDomains(ctx)
	if err != nil {
		t.Fatalf("ListManagedSendingIdentityDomains after forget: %v", err)
	}
	if len(managed) != 0 {
		t.Fatalf("ledger after confirmed provider delete = %v, want empty", managed)
	}
}

func TestManagedSendingIdentityLedgerKeysetPage(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		domain := fmt.Sprintf("%02d-ledger.example.test", i)
		incarnation := fmt.Sprintf("inc-%02d", i)
		if err := store.MarkSendingIdentityManaged(ctx, domain, incarnation); err != nil {
			t.Fatalf("MarkSendingIdentityManaged(%s): %v", domain, err)
		}
		if i%2 == 0 {
			if err := store.MarkSendingIdentityApplied(ctx, domain, incarnation); err != nil {
				t.Fatalf("MarkSendingIdentityApplied(%s): %v", domain, err)
			}
		}
	}

	domains, needs, more, err := store.ListManagedSendingIdentityDomainsPage(ctx, "09-ledger.example.test", 10)
	if err != nil {
		t.Fatalf("ListManagedSendingIdentityDomainsPage: %v", err)
	}
	if len(domains) != 10 || domains[0] != "10-ledger.example.test" || domains[9] != "19-ledger.example.test" || !more {
		t.Fatalf("page = %v more=%v, want 10..19 with continuation", domains, more)
	}
	if needs["10-ledger.example.test"] || !needs["11-ledger.example.test"] {
		t.Fatalf("page lost applied-incarnation state: %v", needs)
	}

	need, found, err := store.LookupManagedSendingIdentityDomain(ctx, "11-ledger.example.test")
	if err != nil || !found || !need {
		t.Fatalf("exact managed lookup = need:%v found:%v err:%v, want true/true/nil", need, found, err)
	}
	if _, found, err := store.LookupManagedSendingIdentityDomain(ctx, "absent.example.test"); err != nil || found {
		t.Fatalf("absent managed lookup = found:%v err:%v", found, err)
	}
}

func TestLegacySendingStatusTransitionCreatesManagedIdentityLedger(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "legacy-transition@example.com", "Legacy Transition", "legacy-transition-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain, err := store.ClaimOrCreateDomain(ctx, "legacy-transition.example.com", user.ID)
	if err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, domain.Domain, user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	managed, _, err := store.ListManagedSendingIdentityDomains(ctx)
	if err != nil {
		t.Fatalf("ListManagedSendingIdentityDomains before sender intent: %v", err)
	}
	if len(managed) != 0 {
		t.Fatalf("verification alone must not claim provider ownership: %v", managed)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE domains SET sending_status = 'pending' WHERE domain = $1`, domain.Domain,
	); err != nil {
		t.Fatalf("legacy sending status update: %v", err)
	}

	managed, needsProvision, err := store.ListManagedSendingIdentityDomains(ctx)
	if err != nil {
		t.Fatalf("ListManagedSendingIdentityDomains: %v", err)
	}
	if len(managed) != 1 || managed[0] != domain.Domain || !needsProvision[domain.Domain] {
		t.Fatalf("legacy verified transition was not durably adopted: managed=%v needs=%v", managed, needsProvision)
	}
}

func TestMarkSendingIdentityManagedClearsSameIncarnationAppliedState(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "same-incarnation@example.com", "Same Incarnation", "same-incarnation-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain, err := store.ClaimOrCreateDomain(ctx, "same-incarnation.example.com", user.ID)
	if err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.MarkSendingIdentityManaged(ctx, domain.Domain, domain.VerificationToken); err != nil {
		t.Fatalf("first MarkSendingIdentityManaged: %v", err)
	}
	if err := store.MarkSendingIdentityApplied(ctx, domain.Domain, domain.VerificationToken); err != nil {
		t.Fatalf("MarkSendingIdentityApplied: %v", err)
	}
	if err := store.MarkSendingIdentityManaged(ctx, domain.Domain, domain.VerificationToken); err != nil {
		t.Fatalf("refresh MarkSendingIdentityManaged: %v", err)
	}
	_, needsProvision, err := store.ListManagedSendingIdentityDomains(ctx)
	if err != nil {
		t.Fatalf("ListManagedSendingIdentityDomains: %v", err)
	}
	if !needsProvision[domain.Domain] {
		t.Fatal("same-incarnation mutation intent retained stale applied state")
	}
}

func TestSendingIdentityPendingDeadlinesUseDatabaseClock(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "pending-clock@example.com", "Pending Clock", "pending-clock-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain, err := store.ClaimOrCreateDomain(ctx, "pending-clock.example.com", user.ID)
	if err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.MarkSendingIdentityManaged(ctx, domain.Domain, domain.VerificationToken); err != nil {
		t.Fatalf("MarkSendingIdentityManaged: %v", err)
	}

	expired, err := store.SendingIdentityLedgerExpired(ctx, domain.Domain, domain.VerificationToken, 24*time.Hour)
	if err != nil {
		t.Fatalf("SendingIdentityLedgerExpired fresh: %v", err)
	}
	if expired {
		t.Fatal("fresh ledger row reported expired")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sender_identity_managed_domains SET updated_at = clock_timestamp() - interval '25 hours' WHERE domain = $1`,
		domain.Domain,
	); err != nil {
		t.Fatalf("age ledger: %v", err)
	}
	expired, err = store.SendingIdentityLedgerExpired(ctx, domain.Domain, domain.VerificationToken, 24*time.Hour)
	if err != nil {
		t.Fatalf("SendingIdentityLedgerExpired aged: %v", err)
	}
	if !expired {
		t.Fatal("aged ledger row did not expire")
	}

	expired, err = store.ObserveSendingIdentityProviderPending(ctx, domain.Domain, domain.VerificationToken, 24*time.Hour)
	if err != nil {
		t.Fatalf("ObserveSendingIdentityProviderPending first: %v", err)
	}
	if expired {
		t.Fatal("first provider-pending observation immediately expired")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sender_identity_managed_domains SET provider_pending_since = clock_timestamp() - interval '25 hours' WHERE domain = $1`,
		domain.Domain,
	); err != nil {
		t.Fatalf("age provider-pending marker: %v", err)
	}
	expired, err = store.ObserveSendingIdentityProviderPending(ctx, domain.Domain, domain.VerificationToken, 24*time.Hour)
	if err != nil {
		t.Fatalf("ObserveSendingIdentityProviderPending aged: %v", err)
	}
	if !expired {
		t.Fatal("aged provider-pending observation did not expire")
	}
	if err := store.ClearSendingIdentityProviderPending(ctx, domain.Domain, domain.VerificationToken); err != nil {
		t.Fatalf("ClearSendingIdentityProviderPending: %v", err)
	}
	var markerCleared bool
	if err := pool.QueryRow(ctx,
		`SELECT provider_pending_since IS NULL FROM sender_identity_managed_domains WHERE domain = $1`,
		domain.Domain,
	).Scan(&markerCleared); err != nil {
		t.Fatalf("read provider-pending marker: %v", err)
	}
	if !markerCleared {
		t.Fatal("provider-pending marker was not cleared")
	}
}
