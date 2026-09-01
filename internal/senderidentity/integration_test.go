//go:build integration

package senderidentity_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/senderidentity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// freshRiver applies River's schema and clears the job table. testutil does
// not truncate River's tables between runs, so leftover jobs from a prior run
// could otherwise execute against this test's domains; truncating river_job
// per test keeps each run isolated.
func freshRiver(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE river_job"); err != nil {
		t.Fatalf("truncate river_job: %v", err)
	}
}

// startShared builds the shared jobs client with mgr as its registrar, injects
// it back, and starts it — the production wiring, so the tests exercise the real
// shared-client path. Cleanup stops the client.
func startShared(t *testing.T, pool *pgxpool.Pool, mgr *senderidentity.Manager) {
	t.Helper()
	client, err := jobs.New(pool, jobs.Config{}, mgr)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	mgr.SetEnqueuer(client)
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("jobs start: %v", err)
	}
	t.Cleanup(func() { client.Stop(context.Background()) })
}

// recordingFire captures fired sender-identity events.
type recordingFire struct {
	mu     sync.Mutex
	events []firedEvent
}
type firedEvent struct {
	domain string
	userID string
	status senderidentity.Status
}

func (r *recordingFire) firer() senderidentity.EventFirer {
	return func(ctx context.Context, domain, userID string, status senderidentity.Status, errMsg string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, firedEvent{domain, userID, status})
	}
}

func (r *recordingFire) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// setupVerifiedDomain creates a user + a verified domain (with DKIM key
// material, which ClaimOrCreateDomain generates) and returns the domain.
func setupVerifiedDomain(t *testing.T, store *identity.Store, prefix string) (userID, domain string) {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "owner-"+prefix+"@example.com", "Owner", "google-"+prefix)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain = prefix + ".example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	return user.ID, domain
}

func waitForStatus(t *testing.T, store *identity.Store, domain string, want string, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		got, err := store.GetSendingStatus(ctx, domain)
		if err != nil {
			t.Fatalf("GetSendingStatus: %v", err)
		}
		last = got
		if got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sending_status for %s = %q, want %q within %s", domain, last, want, timeout)
}

// TestProvisionToVerified drives the full River-backed flow against a real DB
// with a FakeProvider: EnqueueProvision → ProvisionWorker (pending) → enqueued
// ReconcileWorker → FakeProvider reports verified → domain.sending_status
// becomes "verified" and domain.sending_verified fires.
func TestProvisionToVerified(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	freshRiver(t, pool)
	_, domain := setupVerifiedDomain(t, store, "siprov")

	fake := senderidentity.NewFakeProvider()
	// First reconcile poll already reports verified → no backoff wait.
	fake.SetStatus(domain, senderidentity.Result{Status: senderidentity.StatusVerified})

	rec := &recordingFire{}
	mgr := senderidentity.NewManager(senderidentity.NewStoreAdapter(store, nil), fake, rec.firer(), senderidentity.Config{})
	startShared(t, pool, mgr)

	if err := mgr.EnqueueProvision(ctx, domain); err != nil {
		t.Fatalf("EnqueueProvision: %v", err)
	}

	waitForStatus(t, store, domain, "verified", 15*time.Second)

	if len(fake.ProvisionCalls) == 0 {
		t.Error("expected Provision to be called")
	}
	if rec.count() == 0 {
		t.Error("expected domain.sending_verified to fire")
	}
}

// TestProvisionFailsClosed drives a provider that reports the verification
// failed: status must end at "failed" (relay From, fail-closed) with the
// reason persisted.
func TestProvisionFailsClosed(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	freshRiver(t, pool)
	_, domain := setupVerifiedDomain(t, store, "sifail")

	fake := senderidentity.NewFakeProvider()
	fake.SetStatus(domain, senderidentity.Result{Status: senderidentity.StatusFailed, Error: "dkim mismatch"})

	mgr := senderidentity.NewManager(senderidentity.NewStoreAdapter(store, nil), fake, nil, senderidentity.Config{})
	startShared(t, pool, mgr)

	if err := mgr.EnqueueProvision(ctx, domain); err != nil {
		t.Fatalf("EnqueueProvision: %v", err)
	}
	waitForStatus(t, store, domain, "failed", 15*time.Second)
}

// TestDeprovisionTeardown enqueues a deprovision job (as DeleteDomainTx would)
// and asserts the FakeProvider's Deprovision is called for the domain.
func TestDeprovisionTeardown(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	freshRiver(t, pool)

	fake := senderidentity.NewFakeProvider()
	fake.SeedIdentity("teardown.example.com")
	mgr := senderidentity.NewManager(senderidentity.NewStoreAdapter(store, nil), fake, nil, senderidentity.Config{})
	startShared(t, pool, mgr)

	// EnqueueDeprovisionTx requires a tx; use the pool's own.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := mgr.EnqueueDeprovisionTx(ctx, tx, "teardown.example.com"); err != nil {
		t.Fatalf("EnqueueDeprovisionTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(fake.DeprovisionCalls) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Deprovision was not called within timeout")
}

// TestReprovisionAfterFailed pins the review fix: after a domain reaches a
// terminal `failed` (leaving COMPLETED provision/reconcile jobs in river_job),
// the user fixing DNS and re-hitting POST /verify must re-provision — the
// completed jobs must NOT unique-dedup the new enqueue (ByState excludes
// completed). The domain must graduate to verified. NOTE: no truncate between
// the two enqueues — that is the whole point.
func TestReprovisionAfterFailed(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	freshRiver(t, pool)
	_, domain := setupVerifiedDomain(t, store, "sireprov")

	fake := senderidentity.NewFakeProvider()
	fake.SetStatus(domain, senderidentity.Result{Status: senderidentity.StatusFailed, Error: "dns not ready"})
	mgr := senderidentity.NewManager(senderidentity.NewStoreAdapter(store, nil), fake, nil, senderidentity.Config{})
	startShared(t, pool, mgr)

	if err := mgr.EnqueueProvision(ctx, domain); err != nil {
		t.Fatalf("EnqueueProvision: %v", err)
	}
	waitForStatus(t, store, domain, "failed", 15*time.Second)

	// User fixes DNS, forces a re-check. The completed jobs must not block this.
	fake.SetStatus(domain, senderidentity.Result{Status: senderidentity.StatusVerified})
	if err := mgr.EnqueueProvision(ctx, domain); err != nil {
		t.Fatalf("re-EnqueueProvision: %v", err)
	}
	waitForStatus(t, store, domain, "verified", 15*time.Second)
}
