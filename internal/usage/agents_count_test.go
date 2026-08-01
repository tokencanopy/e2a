package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

// TestCountAgentsByUser_ExcludesOrphanedAgents is the regression guard for the
// usage.agents / GET /v1/agents disagreement: CountAgentsByUser (which feeds
// both usage.Agents in the account view and the max_agents cap) must count only
// ACTIVE agents — those whose domain row still exists — so the count always
// equals the length of identity.Store.ListAgentsByUser. An agent orphaned onto
// a missing domain must NOT be counted; before the JOIN fix it inflated the
// count above the list length and silently consumed a plan slot.
func TestCountAgentsByUser_ExcludesOrphanedAgents(t *testing.T) {
	pool := testutil.TestDB(t)
	usageStore := usage.NewStore(pool)
	idStore := identity.NewStore(pool)
	ctx := context.Background()

	user, err := idStore.CreateOrGetUser(ctx, "orphan-count@example.com", "Orphan Count", "google-orphan-count")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	// One agent on a live domain (active) and one on a domain we will delete
	// out from under it (orphaned).
	if _, err := idStore.ClaimOrCreateDomain(ctx, "keep.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain keep: %v", err)
	}
	if _, err := idStore.ClaimOrCreateDomain(ctx, "gone.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain gone: %v", err)
	}
	if _, err := idStore.CreateAgent(ctx, "bot@keep.example.com", "keep.example.com", "", "", "", user.ID); err != nil {
		t.Fatalf("CreateAgent keep: %v", err)
	}
	if _, err := idStore.CreateAgent(ctx, "bot@gone.example.com", "gone.example.com", "", "", "", user.ID); err != nil {
		t.Fatalf("CreateAgent gone: %v", err)
	}

	// Sanity: before orphaning, both agents are active and counted.
	if n, err := usageStore.CountAgentsByUser(ctx, user.ID); err != nil || n != 2 {
		t.Fatalf("pre-orphan CountAgentsByUser = %d, err=%v; want 2", n, err)
	}

	// Orphan the second agent by deleting its domain row. The
	// agent_identities.registered_domain FK is ON DELETE NO ACTION, so a normal delete is
	// blocked; disable replication-role triggers on a single pinned connection
	// to force the orphaned state this test exists to reproduce.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("pool.Acquire: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET session_replication_role = 'replica'`); err != nil {
		conn.Release()
		t.Fatalf("disable FK triggers: %v", err)
	}
	_, delErr := conn.Exec(ctx, `DELETE FROM domains WHERE domain = 'gone.example.com'`)
	_, resetErr := conn.Exec(ctx, `SET session_replication_role = 'origin'`)
	conn.Release()
	if delErr != nil {
		t.Fatalf("delete domain row: %v", delErr)
	}
	if resetErr != nil {
		t.Fatalf("reset replication role: %v", resetErr)
	}

	// The list is the source of truth for "an agent"; the count must match it.
	// limit<=0 returns every agent unpaginated (first page = zero keyset).
	agents, err := idStore.ListAgentsByUser(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAgentsByUser: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("ListAgentsByUser returned %d agents, want 1 (orphaned agent excluded)", len(agents))
	}

	count, err := usageStore.CountAgentsByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("CountAgentsByUser: %v", err)
	}
	if count != len(agents) {
		t.Errorf("CountAgentsByUser = %d, but /v1/agents list length = %d; usage.agents must match the list", count, len(agents))
	}
	if count != 1 {
		t.Errorf("CountAgentsByUser = %d, want 1 (orphaned agent must not consume a slot)", count)
	}
}

// TestCountAgentsByUser_CountsSubdomainAgent proves the association decision
// for subdomain agents: the agent is stored under its VERIFIED PARENT domain
// (agent_identities.registered_domain = "team.mnexa.ai") while keeping its full subdomain
// address ("otto@acme.team.mnexa.ai"). Because the stored domain has a real
// domains row, the CountAgentsByUser JOIN resolves and the subdomain agent is
// counted against max_agents — it does not slip past the quota. This mirrors
// exactly what handleCreateAgent does after resolving a covering parent.
func TestCountAgentsByUser_CountsSubdomainAgent(t *testing.T) {
	pool := testutil.TestDB(t)
	usageStore := usage.NewStore(pool)
	idStore := identity.NewStore(pool)
	ctx := context.Background()

	user, err := idStore.CreateOrGetUser(ctx, "owner@mnexa.ai", "Owner", "google-sub-count")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	// Register + verify the PARENT only — no separate subdomain registration.
	if _, err := idStore.ClaimOrCreateDomain(ctx, "team.mnexa.ai", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := idStore.VerifyDomain(ctx, "team.mnexa.ai", user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}

	// The covering parent the handler resolves, confirmed from the store.
	parent, err := idStore.LookupCoveringDomain(ctx, "acme.team.mnexa.ai", user.ID)
	if err != nil {
		t.Fatalf("LookupCoveringDomain: %v", err)
	}
	// Create the subdomain agent authorized by the registered parent.
	sub, err := idStore.CreateAgent(ctx, "otto@acme.team.mnexa.ai", parent.Domain, "", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent subdomain: %v", err)
	}
	if sub.EmailAddress() != "otto@acme.team.mnexa.ai" {
		t.Fatalf("agent address = %q, want the full subdomain", sub.EmailAddress())
	}
	if sub.Domain != "acme.team.mnexa.ai" {
		t.Fatalf("agent exact domain = %q, want acme.team.mnexa.ai", sub.Domain)
	}
	if sub.RegisteredDomain != "team.mnexa.ai" {
		t.Fatalf("agent registered domain = %q, want parent team.mnexa.ai", sub.RegisteredDomain)
	}
	if !sub.DomainVerified {
		t.Fatalf("subdomain agent must inherit the parent's verified state")
	}

	// The quota JOIN resolves the parent domains row, so the agent is counted.
	count, err := usageStore.CountAgentsByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("CountAgentsByUser: %v", err)
	}
	if count != 1 {
		t.Errorf("CountAgentsByUser = %d, want 1 (subdomain agent must count against max_agents)", count)
	}
	// And it is visible in the list (same JOIN) — never invisible/orphaned.
	list, err := idStore.ListAgentsByUser(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAgentsByUser: %v", err)
	}
	if len(list) != 1 || list[0].EmailAddress() != "otto@acme.team.mnexa.ai" {
		t.Errorf("subdomain agent must appear in the agent list, got %+v", list)
	}
}

// TestCountAgentsByUser_ExcludesTrashedAgents mirrors migration 063's trash
// exclusion: a soft-deleted agent is invisible to ListAgentsByUser, so it
// must neither show up as usage nor consume a max_agents slot — the user can
// create a replacement while the old inbox sits in the trash.
func TestCountAgentsByUser_ExcludesTrashedAgents(t *testing.T) {
	pool := testutil.TestDB(t)
	usageStore := usage.NewStore(pool)
	idStore := identity.NewStore(pool)
	ctx := context.Background()

	user, err := idStore.CreateOrGetUser(ctx, "trash-count@example.com", "Trash Count", "google-trash-count")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := idStore.ClaimOrCreateDomain(ctx, "trashcount.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	for _, email := range []string{"a@trashcount.example.com", "b@trashcount.example.com"} {
		if _, err := idStore.CreateAgent(ctx, email, "trashcount.example.com", "", "", "", user.ID); err != nil {
			t.Fatalf("CreateAgent %s: %v", email, err)
		}
	}
	if n, err := usageStore.CountAgentsByUser(ctx, user.ID); err != nil || n != 2 {
		t.Fatalf("pre-trash CountAgentsByUser = %d, err=%v; want 2", n, err)
	}

	if err := idStore.SoftDeleteAgent(ctx, "b@trashcount.example.com", user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}
	// The count drops with the trash move — and stays equal to the live list.
	if n, err := usageStore.CountAgentsByUser(ctx, user.ID); err != nil || n != 1 {
		t.Fatalf("post-trash CountAgentsByUser = %d, err=%v; want 1", n, err)
	}
	list, err := idStore.ListAgentsByUser(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAgentsByUser: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("live list = %d rows, want 1 (mirror invariant)", len(list))
	}
	// Restore brings the slot usage back.
	if _, err := idStore.RestoreAgent(ctx, "b@trashcount.example.com", user.ID); err != nil {
		t.Fatalf("RestoreAgent: %v", err)
	}
	if n, err := usageStore.CountAgentsByUser(ctx, user.ID); err != nil || n != 2 {
		t.Fatalf("post-restore CountAgentsByUser = %d, err=%v; want 2", n, err)
	}
}
