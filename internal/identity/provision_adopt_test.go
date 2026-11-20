package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// A direct e2a.dev signup: a user row carrying a real Google subject.
const adoptGoogleSub = "100000000000000000001"

func newDirectSignup(t *testing.T, store *identity.Store, email string) *identity.User {
	t.Helper()
	u, err := store.CreateOrGetUser(context.Background(), email, "Direct Signup", adoptGoogleSub)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func assertUnmapped(t *testing.T, store *identity.Store, ref string) {
	t.Helper()
	got, err := store.GetUserByExternalPrincipal(context.Background(), testIssuer, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("(issuer, %q) is mapped to %s, want no mapping", ref, got.ID)
	}
}

func TestProvisionAdoptsExistingUserOnExactGoogleSubject(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	existing := newDirectSignup(t, store, "ada@example.test")

	u, created, adopted, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "Another Name", testIssuer, adoptGoogleSub)
	if err != nil {
		t.Fatalf("adopting provision: %v", err)
	}
	if u.ID != existing.ID || created || !adopted {
		t.Fatalf("got (id=%s created=%v adopted=%v), want (id=%s false true)", u.ID, created, adopted, existing.ID)
	}

	mapped, err := store.GetUserByExternalPrincipal(ctx, testIssuer, "tcusr_ada")
	if err != nil || mapped == nil || mapped.ID != existing.ID {
		t.Fatalf("mapping = %v, %v; want the adopted user", mapped, err)
	}
	// Adoption never rewrites the adopted user's own identity fields.
	after, err := store.GetUserByID(ctx, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.GoogleSubject != adoptGoogleSub || after.Name != "Direct Signup" || after.Email != "ada@example.test" {
		t.Fatalf("adopted user changed: subject=%q name=%q email=%q", after.GoogleSubject, after.Name, after.Email)
	}
}

func TestProvisionAdoptionReplaysIdempotently(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	existing := newDirectSignup(t, store, "ada@example.test")

	for i := 0; i < 2; i++ {
		u, created, adopted, err := store.ProvisionUserAdopting(ctx,
			"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if u.ID != existing.ID || created || !adopted {
			t.Fatalf("attempt %d: got (id=%s created=%v adopted=%v)", i, u.ID, created, adopted)
		}
	}
}

func TestProvisionRefusesAdoptionOnSubjectMismatch(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	newDirectSignup(t, store, "ada@example.test")

	_, _, _, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, "100000000000000000999")
	if !errors.Is(err, identity.ErrEmailConflict) {
		t.Fatalf("err = %v, want ErrEmailConflict", err)
	}
	assertUnmapped(t, store, "tcusr_ada")
}

func TestProvisionWithoutSubjectKeepsEmailConflict(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	newDirectSignup(t, store, "ada@example.test")

	_, _, err := store.ProvisionUser(ctx, "tcusr_ada", "ada@example.test", "", testIssuer)
	if !errors.Is(err, identity.ErrEmailConflict) {
		t.Fatalf("err = %v, want ErrEmailConflict", err)
	}
	assertUnmapped(t, store, "tcusr_ada")
}

func TestProvisionWithSubjectAndFreeEmailCreatesAsBefore(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	u, created, adopted, err := store.ProvisionUserAdopting(ctx,
		"tcusr_new", "new@example.test", "", testIssuer, adoptGoogleSub)
	if err != nil {
		t.Fatal(err)
	}
	if !created || adopted || u.GoogleSubject != "bootstrap:tcusr_new" {
		t.Fatalf("got (created=%v adopted=%v subject=%q), want a fresh bootstrap user", created, adopted, u.GoogleSubject)
	}
}

func TestProvisionRefusesAdoptingTrashedUser(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	existing := newDirectSignup(t, store, "ada@example.test")
	if _, err := store.TrashAccount(ctx, existing.ID, nil); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub)
	if !errors.Is(err, identity.ErrEmailConflict) {
		t.Fatalf("err = %v, want ErrEmailConflict", err)
	}
	assertUnmapped(t, store, "tcusr_ada")
}

func TestProvisionRefusesAdoptingReadOnlyUser(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	existing := newDirectSignup(t, store, "ada@example.test")
	if _, err := pool.Exec(ctx,
		`INSERT INTO account_sending_controls (user_id, state, reason, actor, pause_class)
		 VALUES ($1, 'paused', 'synthetic abuse', 'op', 'abuse')`, existing.ID); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub)
	if !errors.Is(err, identity.ErrAccountReadOnly) {
		t.Fatalf("err = %v, want ErrAccountReadOnly", err)
	}
	assertUnmapped(t, store, "tcusr_ada")
}

func TestProvisionAdoptionConflictsWhenPairMappedElsewhere(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	newDirectSignup(t, store, "ada@example.test")
	other, err := store.BootstrapUser(ctx, "other@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachExternalPrincipal(ctx, testIssuer, "tcusr_ada", other.ID); err != nil {
		t.Fatal(err)
	}

	_, _, _, err = store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub)
	if !errors.Is(err, identity.ErrExternalPrincipalConflict) {
		t.Fatalf("err = %v, want ErrExternalPrincipalConflict", err)
	}
}

// A synthetic subject is never evidence: "bootstrap:<ref>" would otherwise
// match the Token Canopy-created user for that ref and hand it to a
// different principal.
func TestProvisionRejectsSyntheticSubjectAsEvidence(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	native, _, err := store.ProvisionUser(ctx, "tcusr_owner", "owner@example.test", "", testIssuer)
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, err = store.ProvisionUserAdopting(ctx,
		"tcusr_intruder", "owner@example.test", "", testIssuer, native.GoogleSubject)
	if !errors.Is(err, identity.ErrInvalidGoogleSubject) {
		t.Fatalf("err = %v, want ErrInvalidGoogleSubject", err)
	}
	assertUnmapped(t, store, "tcusr_intruder")
}

// Adoption creates a delegated mapping, so it is meaningless without an
// issuer; the legacy issuer-less path must never adopt.
func TestProvisionAdoptionRequiresIssuer(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	newDirectSignup(t, store, "ada@example.test")

	_, _, _, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", "", adoptGoogleSub)
	if !errors.Is(err, identity.ErrInvalidGoogleSubject) {
		t.Fatalf("err = %v, want ErrInvalidGoogleSubject", err)
	}
}
