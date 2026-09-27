package identity_test

import (
	"context"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// AccountReadOnly is true for exactly one control state: paused with pause
// class abuse. Every other class keeps today's behaviour (only sending is
// refused), a resume lifts read-only at once even though the class is kept,
// and an account with no control row is writable.
func TestAccountReadOnlyOnlyForAnAbusePause(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "ro@example.test", "RO", "sub-ro")
	if err != nil {
		t.Fatal(err)
	}
	check := func(label string, want bool) {
		t.Helper()
		got, err := store.AccountReadOnly(ctx, user.ID)
		if err != nil {
			t.Fatalf("%s: AccountReadOnly: %v", label, err)
		}
		if got != want {
			t.Fatalf("%s: AccountReadOnly = %v, want %v", label, got, want)
		}
	}
	check("no control row", false)

	set := func(state, class string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO account_sending_controls (user_id, state, reason, actor, pause_class)
			VALUES ($1, $2, 'synthetic', 'test', $3)
			ON CONFLICT (user_id) DO UPDATE SET state = $2, pause_class = $3`,
			user.ID, state, class); err != nil {
			t.Fatalf("set control %s/%s: %v", state, class, err)
		}
	}
	for _, class := range []string{"operator", "billing", "system"} {
		set("paused", class)
		check("paused/"+class, false)
	}
	set("paused", "abuse")
	check("paused/abuse", true)
	// A resume keeps the abuse class as history; it must not keep the
	// account read-only.
	set("active", "abuse")
	check("resumed after abuse", false)

	other, err := store.CreateOrGetUser(ctx, "ro-other@example.test", "RO2", "sub-ro-other")
	if err != nil {
		t.Fatal(err)
	}
	set("paused", "abuse")
	if got, err := store.AccountReadOnly(ctx, other.ID); err != nil || got {
		t.Fatalf("another account's abuse pause leaked: got %v err %v", got, err)
	}
}

func TestAccountReadOnlyReportsAQueryFailure(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.AccountReadOnly(ctx, "usr_x"); err == nil {
		t.Fatal("AccountReadOnly with a dead context returned no error; the guard could not fail closed")
	}
}
