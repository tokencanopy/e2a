//go:build integration

package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/domainteardown"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func TestDomainTeardownReceiptSurvivesDeleteAndReRegistration(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "receipt-owner@example.test", "Receipt Owner", "receipt-owner-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "receipt.example.test"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	var initial domainteardown.Receipt
	if err := store.DeleteDomainTx(ctx, domain, user.ID, func(ctx context.Context, tx pgx.Tx, incarnation string) error {
		var err error
		initial, err = store.BeginDomainTeardownReceiptTx(ctx, tx, domain, incarnation, user.ID, true)
		return err
	}, nil); err != nil {
		t.Fatalf("DeleteDomainTx: %v", err)
	}
	if initial.State != domainteardown.Pending || initial.Incarnation == "" {
		t.Fatalf("initial state = %q, want pending while a provider check is outstanding", initial)
	}
	got, err := store.LookupDomainTeardownReceipt(ctx, domain, user.ID)
	if err != nil || got != domainteardown.Pending {
		t.Fatalf("receipt after delete = %q, %v; want pending", got, err)
	}
	if _, err := store.LookupDomainTeardownReceipt(ctx, domain, "another-user"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-owner receipt lookup error = %v, want pgx.ErrNoRows", err)
	}

	if err := store.SetDomainTeardownState(ctx, domain, domainteardown.Confirmed); err != nil {
		t.Fatalf("SetDomainTeardownState: %v", err)
	}
	got, err = store.LookupDomainTeardownReceipt(ctx, domain, user.ID)
	if err != nil || got != domainteardown.Confirmed {
		t.Fatalf("receipt after convergence = %q, %v; want confirmed", got, err)
	}

	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("re-register domain: %v", err)
	}
	got, err = store.LookupDomainTeardownReceiptForIncarnation(ctx, domain, initial.Incarnation, user.ID)
	if err != nil || got != domainteardown.Confirmed {
		t.Fatalf("historical receipt after re-registration = %q, %v; want confirmed", got, err)
	}

	var replacement domainteardown.Receipt
	if err := store.DeleteDomainTx(ctx, domain, user.ID, func(ctx context.Context, tx pgx.Tx, incarnation string) error {
		var err error
		replacement, err = store.BeginDomainTeardownReceiptTx(ctx, tx, domain, incarnation, user.ID, true)
		return err
	}, nil); err != nil {
		t.Fatalf("delete replacement: %v", err)
	}
	if replacement.Incarnation == initial.Incarnation {
		t.Fatalf("replacement incarnation = initial incarnation %q", initial.Incarnation)
	}
	got, err = store.LookupDomainTeardownReceiptForIncarnation(ctx, domain, initial.Incarnation, user.ID)
	if err != nil || got != domainteardown.Pending {
		t.Fatalf("old receipt during replacement teardown = %q, %v; want pending", got, err)
	}
	if err := store.SetDomainTeardownState(ctx, domain, domainteardown.Confirmed); err != nil {
		t.Fatalf("confirm replacement teardown: %v", err)
	}
	for _, incarnation := range []string{initial.Incarnation, replacement.Incarnation} {
		got, err = store.LookupDomainTeardownReceiptForIncarnation(ctx, domain, incarnation, user.ID)
		if err != nil || got != domainteardown.Confirmed {
			t.Fatalf("receipt %q after replacement convergence = %q, %v; want confirmed", incarnation, got, err)
		}
	}
}

func TestDomainTeardownSnapshotIsConsistentDuringReplacementDelete(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "snapshot-owner@example.test", "Snapshot Owner", "snapshot-owner-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	replacementOwner, err := store.CreateOrGetUser(ctx, "snapshot-replacement@example.test", "Snapshot Replacement", "snapshot-replacement-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser replacement: %v", err)
	}
	const domain = "snapshot.example.test"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain initial: %v", err)
	}
	var initial domainteardown.Receipt
	if err := store.DeleteDomainTx(ctx, domain, user.ID, func(ctx context.Context, tx pgx.Tx, incarnation string) error {
		var err error
		initial, err = store.BeginDomainTeardownReceiptTx(ctx, tx, domain, incarnation, user.ID, true)
		return err
	}, nil); err != nil {
		t.Fatalf("delete initial: %v", err)
	}
	if err := store.SetDomainTeardownState(ctx, domain, domainteardown.Confirmed); err != nil {
		t.Fatalf("confirm initial: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, replacementOwner.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain replacement: %v", err)
	}

	resetDone := make(chan struct{})
	releaseCommit := make(chan struct{})
	defer func() {
		select {
		case <-releaseCommit:
		default:
			close(releaseCommit)
		}
	}()
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- store.DeleteDomainTx(ctx, domain, replacementOwner.ID, func(ctx context.Context, tx pgx.Tx, incarnation string) error {
			if _, err := store.BeginDomainTeardownReceiptTx(ctx, tx, domain, incarnation, replacementOwner.ID, true); err != nil {
				return err
			}
			close(resetDone)
			<-releaseCommit
			return nil
		}, nil)
	}()
	<-resetDone

	state, live, err := store.LookupDomainTeardownSnapshot(ctx, domain, initial.Incarnation, user.ID)
	if err != nil {
		t.Fatalf("snapshot while replacement delete is uncommitted: %v", err)
	}
	if state != domainteardown.Confirmed || !live {
		t.Fatalf("pre-commit snapshot = (%q, live=%v), want (confirmed, true)", state, live)
	}
	close(releaseCommit)
	if err := <-deleteDone; err != nil {
		t.Fatalf("delete replacement: %v", err)
	}

	state, live, err = store.LookupDomainTeardownSnapshot(ctx, domain, initial.Incarnation, user.ID)
	if err != nil {
		t.Fatalf("snapshot after replacement delete: %v", err)
	}
	if state != domainteardown.Pending || live {
		t.Fatalf("post-commit snapshot = (%q, live=%v), want (pending, false)", state, live)
	}
}

func TestDomainTeardownReceiptWithoutProviderDependsOnManagedLedger(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "provider-off@example.test", "Provider Off", "provider-off-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	deleteWithReceipt := func(t *testing.T, domain string, managed bool) domainteardown.State {
		t.Helper()
		d, err := store.ClaimOrCreateDomain(ctx, domain, user.ID)
		if err != nil {
			t.Fatalf("ClaimOrCreateDomain: %v", err)
		}
		if managed {
			if err := store.MarkSendingIdentityManaged(ctx, domain, d.VerificationToken); err != nil {
				t.Fatalf("MarkSendingIdentityManaged: %v", err)
			}
		}
		var receipt domainteardown.Receipt
		if err := store.DeleteDomainTx(ctx, domain, user.ID, func(ctx context.Context, tx pgx.Tx, incarnation string) error {
			var err error
			receipt, err = store.BeginDomainTeardownReceiptTx(ctx, tx, domain, incarnation, user.ID, false)
			return err
		}, nil); err != nil {
			t.Fatalf("DeleteDomainTx: %v", err)
		}
		return receipt.State
	}

	if got := deleteWithReceipt(t, "never-managed.example.test", false); got != domainteardown.Confirmed {
		t.Fatalf("never-managed state = %q, want confirmed", got)
	}
	if got := deleteWithReceipt(t, "managed-provider-off.example.test", true); got != domainteardown.Pending {
		t.Fatalf("managed provider-off state = %q, want pending", got)
	}
}
