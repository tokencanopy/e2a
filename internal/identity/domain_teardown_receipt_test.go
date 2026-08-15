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

func TestDomainTeardownReceiptSurvivesDeleteAndClearsOnReRegistration(t *testing.T) {
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

	var initial domainteardown.State
	if err := store.DeleteDomainTx(ctx, domain, user.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		initial, err = store.BeginDomainTeardownReceiptTx(ctx, tx, domain, user.ID, true)
		return err
	}); err != nil {
		t.Fatalf("DeleteDomainTx: %v", err)
	}
	if initial != domainteardown.Pending {
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
	if _, err := store.LookupDomainTeardownReceipt(ctx, domain, user.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale receipt survived a new domain incarnation: %v", err)
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
		var state domainteardown.State
		if err := store.DeleteDomainTx(ctx, domain, user.ID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			state, err = store.BeginDomainTeardownReceiptTx(ctx, tx, domain, user.ID, false)
			return err
		}); err != nil {
			t.Fatalf("DeleteDomainTx: %v", err)
		}
		return state
	}

	if got := deleteWithReceipt(t, "never-managed.example.test", false); got != domainteardown.Confirmed {
		t.Fatalf("never-managed state = %q, want confirmed", got)
	}
	if got := deleteWithReceipt(t, "managed-provider-off.example.test", true); got != domainteardown.Pending {
		t.Fatalf("managed provider-off state = %q, want pending", got)
	}
}
