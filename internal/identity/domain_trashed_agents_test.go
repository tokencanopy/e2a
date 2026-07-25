package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// Deleting an agent SOFT-deletes it (migration 063: agent_identities.deleted_at).
// HasAgentsOnDomain counted those trashed rows, which produced a dead end for
// anyone tidying up: delete every agent on a domain, delete the domain, get
// 400 domain_has_agents — while list_agents shows nothing on that domain. The
// only way out was permanent deletion, which is irreversible.
//
// This is the documented teardown order ("delete agents before domains"), so
// the guidance itself did not work.
func TestHasAgentsOnDomain_IgnoresTrashedAgents(t *testing.T) {
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

	// Live agent: the domain is genuinely in use, so the delete must stay blocked.
	has, err := store.HasAgentsOnDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("HasAgentsOnDomain (live): %v", err)
	}
	if !has {
		t.Fatal("a LIVE agent on the domain must block the domain delete")
	}

	// Trash it — the ordinary DELETE /v1/agents/{email} path, not a purge.
	if err := store.SoftDeleteAgent(ctx, agent.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	has, err = store.HasAgentsOnDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("HasAgentsOnDomain (trashed): %v", err)
	}
	if has {
		t.Fatal("a TRASHED agent must not block the domain delete — the caller sees no agents on the domain, so blocking is an unescapable dead end short of permanent deletion")
	}
}

// The same soft-delete filter was missing from the agent_count reported on each
// DomainView, so a listed domain over-reported how many agents were on it.
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

	keep, err := store.CreateAgent(ctx, "keep@"+domain, domain, "Keep", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent keep: %v", err)
	}
	_ = keep
	trash, err := store.CreateAgent(ctx, "trash@"+domain, domain, "Trash", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent trash: %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, trash.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	countFor := func(t *testing.T) int {
		t.Helper()
		domains, err := store.ListDomainsByUser(ctx, user.ID, 50, time.Time{}, "")
		if err != nil {
			t.Fatalf("ListDomainsByUser: %v", err)
		}
		for _, d := range domains {
			if d.Domain == domain {
				return d.AgentCount
			}
		}
		t.Fatalf("domain %s not returned by ListDomainsByUser", domain)
		return -1
	}

	if got := countFor(t); got != 1 {
		t.Fatalf("agent_count = %d, want 1 (one live agent; the trashed one must not be counted)", got)
	}
}
