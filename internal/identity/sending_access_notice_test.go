package identity_test

import (
	"context"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// TestSendingAccessNoticeCounts_NoRows: a brand-new account has no
// account_limits or account_sending_controls row and no resources yet —
// every count degrades to its documented row-less default instead of erroring.
func TestSendingAccessNoticeCounts_NoRows(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID := templateTestUser(t, store, "sanc-norows")

	got, err := store.SendingAccessNoticeCounts(ctx, userID)
	if err != nil {
		t.Fatalf("SendingAccessNoticeCounts: %v", err)
	}
	want := identity.SendingAccessNoticeCounts{PlanCode: "", SendingState: "active", PauseClass: "", LiveAgents: 0, VerifiedDomains: 0}
	if got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
}

// TestSendingAccessNoticeCounts_Populated: a plan row, a paused control row,
// a mix of live/trashed agents, and a mix of verified/unverified domains all
// resolve to the right numbers.
func TestSendingAccessNoticeCounts_Populated(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	limitsStore := limits.NewStore(pool)
	ctx := context.Background()
	userID := templateTestUser(t, store, "sanc-full")

	if err := limitsStore.Upsert(ctx, userID, limits.Limits{
		PlanCode: "pro", MaxAgents: 25, MaxDomains: 10, MaxMessagesMonth: 100000, MaxStorageBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("seed account_limits: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_sending_controls (user_id, state, pause_class, reason, actor)
		VALUES ($1, 'paused', 'abuse', 'test', 'test')`, userID); err != nil {
		t.Fatalf("seed account_sending_controls: %v", err)
	}

	verified := "sanc-verified.example.com"
	unverified := "sanc-unverified.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, verified, userID); err != nil {
		t.Fatalf("claim verified domain: %v", err)
	}
	if err := store.VerifyDomain(ctx, verified, userID); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, unverified, userID); err != nil {
		t.Fatalf("claim unverified domain: %v", err)
	}

	live1, err := store.CreateAgent(ctx, "live1@"+verified, verified, "", "", "local", userID)
	if err != nil {
		t.Fatalf("create live agent 1: %v", err)
	}
	if _, err := store.CreateAgent(ctx, "live2@"+verified, verified, "", "", "local", userID); err != nil {
		t.Fatalf("create live agent 2: %v", err)
	}
	trashed, err := store.CreateAgent(ctx, "trashed@"+verified, verified, "", "", "local", userID)
	if err != nil {
		t.Fatalf("create trashed agent: %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, trashed.ID, userID); err != nil {
		t.Fatalf("soft-delete agent: %v", err)
	}
	_ = live1

	got, err := store.SendingAccessNoticeCounts(ctx, userID)
	if err != nil {
		t.Fatalf("SendingAccessNoticeCounts: %v", err)
	}
	want := identity.SendingAccessNoticeCounts{
		PlanCode: "pro", SendingState: "paused", PauseClass: "abuse", LiveAgents: 2, VerifiedDomains: 1,
	}
	if got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
}
