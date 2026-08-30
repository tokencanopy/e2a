package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

const testIssuer = "https://issuer.example.test/oidc"

func TestAttachExternalPrincipalLifecycle(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	alice, err := store.BootstrapUser(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.BootstrapUser(ctx, "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.AttachExternalPrincipal(ctx, testIssuer, "principal-1", alice.ID)
	if err != nil || !created {
		t.Fatalf("first attach = (%v, %v), want created", created, err)
	}
	// Same triple: idempotent replay.
	created, err = store.AttachExternalPrincipal(ctx, testIssuer, "principal-1", alice.ID)
	if err != nil || created {
		t.Fatalf("replay attach = (%v, %v), want existing", created, err)
	}
	// Same pair, different user: conflict, never merged.
	if _, err = store.AttachExternalPrincipal(ctx, testIssuer, "principal-1", bob.ID); !errors.Is(err, identity.ErrExternalPrincipalConflict) {
		t.Fatalf("conflicting attach err = %v, want ErrExternalPrincipalConflict", err)
	}
	// Unknown user: not found.
	if _, err = store.AttachExternalPrincipal(ctx, testIssuer, "principal-2", "00000000000000000000000000000000"); !errors.Is(err, identity.ErrExternalPrincipalUserNotFound) {
		t.Fatalf("unknown-user attach err = %v, want ErrExternalPrincipalUserNotFound", err)
	}
	// Multiple pairs may map to one user.
	if created, err = store.AttachExternalPrincipal(ctx, "https://issuer2.example.test", "principal-1", alice.ID); err != nil || !created {
		t.Fatalf("second-issuer attach = (%v, %v), want created", created, err)
	}
	// The attach path never touches the user row.
	reread, err := store.GetUserByID(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reread.Email != alice.Email || reread.GoogleSubject != alice.GoogleSubject || reread.Name != alice.Name {
		t.Fatalf("attach mutated the user row: %+v vs %+v", reread, alice)
	}
}

func TestGetUserByExternalPrincipalExactPairLookup(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	u, err := store.BootstrapUser(ctx, "carol@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachExternalPrincipal(ctx, testIssuer, "principal-9", u.ID); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetUserByExternalPrincipal(ctx, testIssuer, "principal-9")
	if err != nil || got == nil || got.ID != u.ID {
		t.Fatalf("lookup = (%+v, %v), want user %s", got, err, u.ID)
	}
	if got.AccountClass == "" {
		t.Fatal("lookup must return the complete user including account_class")
	}
	// Unknown pair is (nil, nil) — a 401 for the caller, distinct from a
	// store failure.
	got, err = store.GetUserByExternalPrincipal(ctx, testIssuer, "unmapped")
	if err != nil || got != nil {
		t.Fatalf("unmapped pair = (%+v, %v), want (nil, nil)", got, err)
	}
	// Subject alone (right subject, different issuer) must not resolve.
	got, err = store.GetUserByExternalPrincipal(ctx, "https://other.example.test", "principal-9")
	if err != nil || got != nil {
		t.Fatalf("wrong-issuer lookup = (%+v, %v), want (nil, nil)", got, err)
	}
}

func TestProvisionUserWithExternalIssuerCreatesMapping(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	u, created, err := store.ProvisionUser(ctx, "ref-1", "dave@example.com", "Dave", testIssuer)
	if err != nil || !created {
		t.Fatalf("provision = (%v, %v), want created", created, err)
	}
	if u.GoogleSubject != "bootstrap:ref-1" {
		t.Fatalf("google_subject = %q, want bootstrap:ref-1", u.GoogleSubject)
	}
	mapped, err := store.GetUserByExternalPrincipal(ctx, testIssuer, "ref-1")
	if err != nil || mapped == nil || mapped.ID != u.ID {
		t.Fatalf("mapping lookup = (%+v, %v), want user %s", mapped, err, u.ID)
	}

	// Replay with the issuer: same user, mapping intact.
	again, created, err := store.ProvisionUser(ctx, "ref-1", "dave@example.com", "Dave", testIssuer)
	if err != nil || created || again.ID != u.ID {
		t.Fatalf("replay = (%+v, %v, %v), want existing %s", again, created, err, u.ID)
	}
}

func TestProvisionUserWithoutIssuerPreservesLegacyBehavior(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	u, created, err := store.ProvisionUser(ctx, "ref-legacy", "erin@example.com", "", "")
	if err != nil || !created {
		t.Fatalf("provision = (%v, %v), want created", created, err)
	}
	mapped, err := store.GetUserByExternalPrincipal(ctx, "", "ref-legacy")
	if err != nil || mapped != nil {
		t.Fatalf("legacy provision must create no mapping, got (%+v, %v)", mapped, err)
	}
	if u.GoogleSubject != "bootstrap:ref-legacy" {
		t.Fatalf("google_subject = %q", u.GoogleSubject)
	}
}

func TestProvisionUserExternalPrincipalConflictAborts(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	other, err := store.BootstrapUser(ctx, "frank@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// The pair a future provision would claim is already attached to a
	// different user.
	if _, err := store.AttachExternalPrincipal(ctx, testIssuer, "ref-conflict", other.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.ProvisionUser(ctx, "ref-conflict", "grace@example.com", "", testIssuer)
	if !errors.Is(err, identity.ErrExternalPrincipalConflict) {
		t.Fatalf("conflicting provision err = %v, want ErrExternalPrincipalConflict", err)
	}
	// The transaction rolled back: no user row was created for the email.
	if _, _, err := store.ProvisionUser(ctx, "ref-fresh", "grace@example.com", "", ""); err != nil {
		t.Fatalf("email must remain free after rolled-back provision: %v", err)
	}
}
