package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// Both live AND trashed agents block deleting their domain, and that is
// deliberate: the FK agent_identities.registered_domain -> domains.domain is
// ON DELETE NO ACTION (migration 001), so Postgres refuses regardless of what
// the app layer thinks, and a trashed agent still owns its address for the
// 30-day restore window — dropping the domain under it would break restore.
//
// CountAgentsOnDomain exists so the API can say WHICH kind is blocking. A
// trashed agent does not appear in list_agents, so a generic "agents exist"
// sends the caller hunting for agents they cannot see.
func TestCountAgentsOnDomain_SplitsLiveFromTrashed(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "trash-dom@example.com", "Trash Dom", "google-trash-dom")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "trashdom.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	agent, err := store.CreateAgent(ctx, "bot@"+domain, domain, "Bot", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	live, trashed, err := store.CountAgentsOnDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("CountAgentsOnDomain (live): %v", err)
	}
	if live != 1 || trashed != 0 {
		t.Fatalf("with one live agent: live=%d trashed=%d, want 1/0", live, trashed)
	}

	// Trash it — the ordinary DELETE /v1/agents/{email} path, not a purge.
	if err := store.SoftDeleteAgent(ctx, agent.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	live, trashed, err = store.CountAgentsOnDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("CountAgentsOnDomain (trashed): %v", err)
	}
	if live != 0 || trashed != 1 {
		t.Fatalf("after trashing the only agent: live=%d trashed=%d, want 0/1 — the split is what lets the API say WHICH kind blocks", live, trashed)
	}

	// The delete really is still blocked, by the FK, whatever the app layer
	// believes. This assertion catches anyone "fixing" the block by filtering
	// deleted_at out of the pre-check: doing that does not unblock anything, it
	// just moves the rejection into an FK-violation string match one layer down.
	if err := store.DeleteDomain(ctx, domain, user.ID); err == nil {
		t.Fatal("deleting a domain whose only agent is TRASHED unexpectedly succeeded — the FK is ON DELETE NO ACTION, so if this ever passes the schema changed and the error message needs revisiting")
	}
}

// The agent_count reported on each DomainView counts only LIVE agents, so it
// agrees with what list_agents returns. Blocking is a separate question (above);
// a displayed count that disagrees with the list is simply wrong.
func TestListDomains_AgentCountExcludesTrashedAgents(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "count-dom@example.com", "Count Dom", "google-count-dom")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "countdom.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	if _, err := store.CreateAgent(ctx, "keep@"+domain, domain, "Keep", "", "", user.ID); err != nil {
		t.Fatalf("CreateAgent keep: %v", err)
	}
	trash, err := store.CreateAgent(ctx, "trash@"+domain, domain, "Trash", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent trash: %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, trash.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	domains, err := store.ListDomainsByUser(ctx, user.ID, 50, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListDomainsByUser: %v", err)
	}
	for _, d := range domains {
		if d.Domain == domain {
			if d.AgentCount != 1 {
				t.Fatalf("agent_count = %d, want 1 (one live agent; the trashed one must not be counted — list_agents does not show it either)", d.AgentCount)
			}
			return
		}
	}
	t.Fatalf("domain %s not returned by ListDomainsByUser", domain)
}
