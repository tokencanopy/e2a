package identity_test

import (
	"context"
	"errors"
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
