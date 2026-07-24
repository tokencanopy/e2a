package identity_test

import (
	"context"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// TestCreateOrGetUserWithCreated_Discriminator: the upsert reports
// created=true exactly once — on the INSERT that makes the row — and
// created=false on every subsequent login with the same google_subject,
// even when the profile fields change (the ON CONFLICT UPDATE path).
// The xmax=0 trick is subtle enough to deserve its own regression test.
func TestCreateOrGetUserWithCreated_Discriminator(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	first, created, err := store.CreateOrGetUserWithCreated(ctx, "upsert@test.com", "First Name", "google-upsert-disc")
	if err != nil {
		t.Fatalf("CreateOrGetUserWithCreated (insert): %v", err)
	}
	if !created {
		t.Errorf("first upsert: created = false, want true")
	}

	// Same subject, updated profile → the DO UPDATE path: same user row,
	// refreshed fields, created must be false.
	second, created, err := store.CreateOrGetUserWithCreated(ctx, "upsert-renamed@test.com", "New Name", "google-upsert-disc")
	if err != nil {
		t.Fatalf("CreateOrGetUserWithCreated (update): %v", err)
	}
	if created {
		t.Errorf("second upsert: created = true, want false")
	}
	if second.ID != first.ID {
		t.Errorf("second upsert returned a different user: %s vs %s", second.ID, first.ID)
	}
	if second.Email != "upsert-renamed@test.com" || second.Name != "New Name" {
		t.Errorf("second upsert did not refresh profile fields: email=%q name=%q", second.Email, second.Name)
	}

	// A different subject is a fresh signup again.
	third, created, err := store.CreateOrGetUserWithCreated(ctx, "other@test.com", "Other", "google-upsert-other")
	if err != nil {
		t.Fatalf("CreateOrGetUserWithCreated (second insert): %v", err)
	}
	if !created {
		t.Errorf("distinct-subject upsert: created = false, want true")
	}
	if third.ID == first.ID {
		t.Errorf("distinct subjects share a user ID: %s", third.ID)
	}

	// The plain wrapper keeps its contract for the many callers that
	// don't care about the signal.
	fourth, err := store.CreateOrGetUser(ctx, "upsert-renamed@test.com", "New Name", "google-upsert-disc")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if fourth.ID != first.ID {
		t.Errorf("wrapper returned a different user: %s vs %s", fourth.ID, first.ID)
	}
}
