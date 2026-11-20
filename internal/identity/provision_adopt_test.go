package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

// An adoption replay resolves by the existing mapping, so it survives the
// adopted user's email changing in between (an e2a.dev sign-in rewrites it).
func TestProvisionAdoptionReplaySurvivesEmailChange(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	existing := newDirectSignup(t, store, "ada@example.test")
	if _, _, _, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET email = 'ada.new@example.test' WHERE id = $1`, existing.ID); err != nil {
		t.Fatal(err)
	}

	u, created, adopted, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub)
	if err != nil {
		t.Fatalf("replay after email change: %v", err)
	}
	if u.ID != existing.ID || created || !adopted {
		t.Fatalf("got (id=%s created=%v adopted=%v), want the adopted user replayed", u.ID, created, adopted)
	}
}

// The mapping replay still demands the evidence: a different subject on an
// already-adopted pair is not a replay.
func TestProvisionAdoptionReplayRequiresSameSubject(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	newDirectSignup(t, store, "ada@example.test")
	if _, _, _, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, "100000000000000000999")
	if !errors.Is(err, identity.ErrEmailConflict) {
		t.Fatalf("err = %v, want ErrEmailConflict", err)
	}
}

// An account paused after its adoption still replays: the read-only check
// guards only a NEW mapping, exactly as attach's replay.
func TestProvisionAdoptionReplaysAfterPause(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	existing := newDirectSignup(t, store, "ada@example.test")
	if _, _, _, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub); err != nil {
		t.Fatal(err)
	}
	abusePause(t, pool, existing.ID, "")

	u, _, adopted, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub)
	if err != nil || u.ID != existing.ID || !adopted {
		t.Fatalf("replay after pause = (%v, adopted=%v, %v), want the adopted user", u, adopted, err)
	}
}

// purgedMappedPrincipal leaves a live login-subject tombstone for
// (testIssuer, ref): a user mapped under it is trashed and purged.
func purgedMappedPrincipal(t *testing.T, store *identity.Store, pool *pgxpool.Pool, ref string) {
	t.Helper()
	ctx := context.Background()
	gone, _, err := store.ProvisionUser(ctx, ref, "gone@example.test", "", testIssuer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, gone.ID, nil); err != nil {
		t.Fatal(err)
	}
	ageTrash(t, pool, gone.ID)
	if purged, err := store.PurgeDeletedUsers(ctx, nil); err != nil || len(purged) != 1 {
		t.Fatalf("purge: %v %v", purged, err)
	}
}

func TestProvisionAdoptionRefusesTombstonedMappingSubject(t *testing.T) {
	store, pool, _ := tombstoneStore(t)
	ctx := context.Background()
	purgedMappedPrincipal(t, store, pool, "tcusr_gone")
	newDirectSignup(t, store, "ada@example.test")

	_, _, _, err := store.ProvisionUserAdopting(ctx,
		"tcusr_gone", "ada@example.test", "", testIssuer, adoptGoogleSub)
	if !errors.Is(err, identity.ErrRegistrationRefused) {
		t.Fatalf("err = %v, want ErrRegistrationRefused", err)
	}
	assertUnmapped(t, store, "tcusr_gone")
}

// A wrong subject must answer exactly what no subject answers, including when
// the evidence-less path refuses on a tombstone before it reaches the email.
func TestProvisionMismatchAnswersAsNoSubjectUnderTombstone(t *testing.T) {
	store, pool, _ := tombstoneStore(t)
	ctx := context.Background()
	purgedMappedPrincipal(t, store, pool, "tcusr_gone")
	newDirectSignup(t, store, "ada@example.test")

	_, _, noSubject := store.ProvisionUser(ctx, "tcusr_gone", "ada@example.test", "", testIssuer)
	_, _, _, mismatch := store.ProvisionUserAdopting(ctx,
		"tcusr_gone", "ada@example.test", "", testIssuer, "100000000000000000999")
	if !errors.Is(noSubject, identity.ErrRegistrationRefused) || !errors.Is(mismatch, identity.ErrRegistrationRefused) {
		t.Fatalf("no subject = %v, mismatch = %v; want both ErrRegistrationRefused", noSubject, mismatch)
	}
}

func TestProvisionAdoptionWithoutTombstoneKeyIsUnavailable(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	newDirectSignup(t, store, "ada@example.test")
	store.SetTombstonePolicy(identity.TombstonePolicy{Enabled: true})

	_, _, _, err := store.ProvisionUserAdopting(ctx,
		"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub)
	if !errors.Is(err, identity.ErrTombstoneKeyUnavailable) {
		t.Fatalf("err = %v, want ErrTombstoneKeyUnavailable", err)
	}
	assertUnmapped(t, store, "tcusr_ada")
}

// waitForLockWaiter blocks until some backend in this database is waiting on
// a row lock, so a concurrency test can order its two transactions.
func waitForLockWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no transaction ever waited on the lock")
}

// A trash that commits while an adoption waits on the holder's row wins: the
// adoption re-reads the trashed row and refuses, writing no mapping.
func TestProvisionAdoptionWaitsForConcurrentTrash(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	existing := newDirectSignup(t, store, "ada@example.test")

	trash, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer trash.Rollback(ctx)
	if _, err := trash.Exec(ctx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, existing.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := store.ProvisionUserAdopting(ctx,
			"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub)
		done <- err
	}()
	waitForLockWaiter(t, pool)
	if _, err := trash.Exec(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, existing.ID); err != nil {
		t.Fatal(err)
	}
	if err := trash.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, identity.ErrEmailConflict) {
			t.Fatalf("adoption racing a trash: err = %v, want ErrEmailConflict", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("adoption never finished")
	}
	assertUnmapped(t, store, "tcusr_ada")
}

// Two concurrent first adoptions of one pair both resolve to the same user,
// with no deadlock and no conflict.
func TestProvisionConcurrentAdoptionsOfOnePair(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	existing := newDirectSignup(t, store, "ada@example.test")

	const n = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			u, _, _, err := store.ProvisionUserAdopting(ctx,
				"tcusr_ada", "ada@example.test", "", testIssuer, adoptGoogleSub)
			errs[i] = err
			if u != nil {
				ids[i] = u.ID
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || ids[i] != existing.ID {
			t.Fatalf("adoption %d = (%s, %v), want %s", i, ids[i], errs[i], existing.ID)
		}
	}
}
