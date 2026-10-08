package identity_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/migrations"
)

func TestPendingCountIndexMigration(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	// TestDB applies the embedded migrations through the real runner. Reapply
	// the index statement directly to also exercise its IF NOT EXISTS retry.
	sql, err := migrations.FS.ReadFile("132_messages_agent_pending_idx.sql")
	if err != nil {
		t.Fatalf("read pending-count migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("reapply pending-count migration: %v", err)
	}

	var definition string
	var valid, ready bool
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_indexdef(indexrelid), indisvalid, indisready
		   FROM pg_index
		  WHERE indexrelid = 'idx_messages_agent_pending'::regclass
		    AND indrelid = 'messages'::regclass`,
	).Scan(&definition, &valid, &ready); err != nil {
		t.Fatalf("read pending-count index: %v", err)
	}
	if !valid || !ready {
		t.Fatalf("pending-count index valid=%t ready=%t, want both true", valid, ready)
	}
	for _, fragment := range []string{
		"USING btree (agent_id) WHERE",
		"status = 'pending_review'",
		"direction = 'outbound'",
	} {
		if !strings.Contains(definition, fragment) {
			t.Fatalf("pending-count index definition %q missing %q", definition, fragment)
		}
	}
}

func TestListAgentsByUserPendingCountScopesOutboundHolds(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, first := setupPendingAgent(t, store, "pending-count")
	second, err := store.CreateAgent(ctx, "second@pending-count.example.com", first.Domain, "", "", "", user.ID)
	if err != nil {
		t.Fatalf("create second agent: %v", err)
	}
	_, otherOwner := setupPendingAgent(t, store, "pending-count-other")

	// Counts are all-time, per-agent outbound holds, not all review items or
	// the seven-day activity totals returned by the neighboring subqueries.
	rows := []struct {
		id        string
		agentID   string
		direction string
		status    string
		ageDays   int
	}{
		{"msg_pending_count_recent", first.ID, "outbound", "pending_review", 0},
		{"msg_pending_count_old", first.ID, "outbound", "pending_review", 14},
		{"msg_pending_count_inbound", first.ID, "inbound", "pending_review", 0},
		{"msg_pending_count_sent", first.ID, "outbound", "sent", 0},
		{"msg_pending_count_approved", first.ID, "inbound", "review_approved", 0},
		{"msg_pending_count_rejected", first.ID, "outbound", "review_rejected", 0},
		{"msg_pending_count_expired_approved", first.ID, "outbound", "review_expired_approved", 0},
		{"msg_pending_count_expired_rejected", first.ID, "outbound", "review_expired_rejected", 0},
		{"msg_pending_count_second", second.ID, "outbound", "pending_review", 0},
		{"msg_pending_count_other_owner", otherOwner.ID, "outbound", "pending_review", 0},
	}
	for _, row := range rows {
		if _, err := pool.Exec(ctx,
			`INSERT INTO messages (id, agent_id, direction, status, created_at)
			 VALUES ($1, $2, $3, $4, now() - make_interval(days => $5))`,
			row.id, row.agentID, row.direction, row.status, row.ageDays,
		); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}

	assertCounts := func(wantFirst int) {
		t.Helper()
		agents, err := store.ListAgentsByUser(ctx, user.ID, 0, time.Time{}, "")
		if err != nil {
			t.Fatalf("ListAgentsByUser: %v", err)
		}
		if len(agents) != 2 {
			t.Fatalf("agent count = %d, want 2 for the requested owner", len(agents))
		}
		want := map[string]int{first.ID: wantFirst, second.ID: 1}
		for _, agent := range agents {
			count, ok := want[agent.ID]
			if !ok {
				t.Fatalf("unexpected agent %q in requested owner's list", agent.ID)
			}
			if agent.PendingCount != count {
				t.Errorf("agent %q pending count = %d, want %d", agent.ID, agent.PendingCount, count)
			}
			delete(want, agent.ID)
		}
		if len(want) != 0 {
			t.Fatalf("missing agents: %v", want)
		}
	}
	assertCounts(2)

	if _, err := pool.Exec(ctx,
		`UPDATE messages SET status = 'sent' WHERE id = 'msg_pending_count_recent'`,
	); err != nil {
		t.Fatalf("resolve pending hold: %v", err)
	}
	assertCounts(1)
}
