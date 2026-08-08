//go:build integration

package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func TestAuditThreadIdentityRepairsInvalidParentsWithoutChangingThreads(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "thread-audit")
	otherAgentID := convoTestSetup(t, store, "thread-audit-other")

	newMessage := func(agent, subject string) *identity.Message {
		t.Helper()
		m, err := store.CreateOutboundMessage(ctx, agent,
			[]string{"recipient@example.net"}, nil, nil,
			subject, "send", "smtp", "", "", nil)
		if err != nil {
			t.Fatalf("CreateOutboundMessage(%s): %v", subject, err)
		}
		return m
	}

	dangling := newMessage(agentID, "dangling")
	crossAgent := newMessage(agentID, "cross-agent")
	mismatched := newMessage(agentID, "mismatched")
	sameMailboxParent := newMessage(agentID, "same-mailbox-parent")
	otherMailboxParent := newMessage(otherAgentID, "other-mailbox-parent")

	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET thread_parent_id = CASE id
		      WHEN $1 THEN 'msg_missing_parent'
		      WHEN $2 THEN $4
		      WHEN $3 THEN $5
		    END
		  WHERE id = ANY($6)`,
		dangling.ID, crossAgent.ID, mismatched.ID,
		otherMailboxParent.ID, sameMailboxParent.ID,
		[]string{dangling.ID, crossAgent.ID, mismatched.ID},
	); err != nil {
		t.Fatalf("seed invalid parent pointers: %v", err)
	}

	before := map[string]string{}
	for _, m := range []*identity.Message{dangling, crossAgent, mismatched, sameMailboxParent, otherMailboxParent} {
		before[m.ID] = m.ThreadID
	}

	result, err := store.AuditThreadIdentityBatch(ctx, "", 100, 16)
	if err != nil {
		t.Fatalf("AuditThreadIdentityBatch: %v", err)
	}
	if result.Violations.DanglingParent != 1 {
		t.Errorf("dangling violations = %d, want 1", result.Violations.DanglingParent)
	}
	if result.Violations.CrossAgentParent != 1 {
		t.Errorf("cross-agent violations = %d, want 1", result.Violations.CrossAgentParent)
	}
	if result.Violations.ThreadMismatch != 1 {
		t.Errorf("thread-mismatch violations = %d, want 1", result.Violations.ThreadMismatch)
	}
	if result.RepairedParentPointers != 3 {
		t.Errorf("repaired pointers = %d, want 3", result.RepairedParentPointers)
	}

	for _, id := range []string{dangling.ID, crossAgent.ID, mismatched.ID} {
		var parent string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(thread_parent_id, '') FROM messages WHERE id=$1`,
			id,
		).Scan(&parent); err != nil {
			t.Fatalf("load repaired parent %s: %v", id, err)
		}
		if parent != "" {
			t.Errorf("message %s parent = %q, want cleared", id, parent)
		}
	}
	for id, wantThread := range before {
		var gotThread string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(thread_id, '') FROM messages WHERE id=$1`,
			id,
		).Scan(&gotThread); err != nil {
			t.Fatalf("load thread %s: %v", id, err)
		}
		if gotThread != wantThread {
			t.Errorf("message %s thread changed from %q to %q", id, wantThread, gotThread)
		}
	}
}

func TestAuditThreadIdentityBreaksCyclesWithoutChangingThreadMembership(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "thread-cycle")

	var messages []*identity.Message
	for _, subject := range []string{"cycle-a", "cycle-b", "cycle-c"} {
		m, err := store.CreateOutboundMessage(ctx, agentID,
			[]string{"recipient@example.net"}, nil, nil,
			subject, "send", "smtp", "", "", nil)
		if err != nil {
			t.Fatalf("CreateOutboundMessage(%s): %v", subject, err)
		}
		messages = append(messages, m)
	}
	threadID := messages[0].ThreadID
	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET thread_id = $4,
		        thread_parent_id = CASE id
		          WHEN $1 THEN $2
		          WHEN $2 THEN $3
		          WHEN $3 THEN $1
		        END
		  WHERE id = ANY($5)`,
		messages[0].ID, messages[1].ID, messages[2].ID, threadID,
		[]string{messages[0].ID, messages[1].ID, messages[2].ID},
	); err != nil {
		t.Fatalf("seed cycle: %v", err)
	}

	result, err := store.AuditThreadIdentityBatch(ctx, "", 100, 16)
	if err != nil {
		t.Fatalf("AuditThreadIdentityBatch: %v", err)
	}
	if result.Violations.Cycle == 0 {
		t.Fatal("cycle violations = 0, want at least one")
	}
	if result.RepairedParentPointers == 0 {
		t.Fatal("repaired pointers = 0, want at least one cycle edge cleared")
	}

	cleared := 0
	for _, m := range messages {
		var gotThread, parent string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(thread_id, ''), COALESCE(thread_parent_id, '')
			   FROM messages WHERE id=$1`,
			m.ID,
		).Scan(&gotThread, &parent); err != nil {
			t.Fatalf("load cycle member %s: %v", m.ID, err)
		}
		if gotThread != threadID {
			t.Errorf("cycle repair changed thread for %s from %q to %q", m.ID, threadID, gotThread)
		}
		if parent == "" {
			cleared++
		}
	}
	if cleared == 0 {
		t.Fatal("cycle still has every edge populated")
	}
}

func TestAuditThreadIdentityUsesBoundedRotatingSampleForMeasurements(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "thread-measurements")

	newMessage := func(subject, conversationID string) *identity.Message {
		t.Helper()
		m, err := store.CreateOutboundMessage(ctx, agentID,
			[]string{"recipient@example.net"}, nil, nil,
			subject, "send", "smtp", "", conversationID, nil)
		if err != nil {
			t.Fatalf("CreateOutboundMessage(%s): %v", subject, err)
		}
		return m
	}

	first := newMessage("first", "conv_one")
	second := newMessage("second", "conv_two")
	third := newMessage("third", "conv_one")
	recentNull := newMessage("recent-null", "")

	if _, err := pool.Exec(ctx,
		`UPDATE messages SET thread_id=$2 WHERE id=$1`,
		second.ID, first.ThreadID,
	); err != nil {
		t.Fatalf("join sampled thread: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET thread_id=NULL, created_at=$2 WHERE id=$1`,
		recentNull.ID, time.Now().Add(-30*time.Minute),
	); err != nil {
		t.Fatalf("seed recent null: %v", err)
	}

	bounded, err := store.AuditThreadIdentityBatch(ctx, "", 2, 16)
	if err != nil {
		t.Fatalf("bounded AuditThreadIdentityBatch: %v", err)
	}
	if bounded.Scanned != 2 {
		t.Fatalf("bounded scan examined %d rows, want exactly 2", bounded.Scanned)
	}
	if bounded.NextCursor == "" {
		t.Fatal("bounded scan cursor is empty with additional rows remaining")
	}

	result, err := store.AuditThreadIdentityBatch(ctx, "", 100, 16)
	if err != nil {
		t.Fatalf("measurement AuditThreadIdentityBatch: %v", err)
	}
	if result.NullThreadsByAge.LessThanOneHour != 1 {
		t.Errorf("recent null count = %d, want 1", result.NullThreadsByAge.LessThanOneHour)
	}
	if result.ThreadsSampled != 2 || result.MultiConversationThreads != 1 {
		t.Errorf("thread spread = %d/%d, want 1/2", result.MultiConversationThreads, result.ThreadsSampled)
	}
	if result.ConversationsSampled != 2 || result.MultiThreadConversations != 1 {
		t.Errorf("conversation spread = %d/%d, want 1/2", result.MultiThreadConversations, result.ConversationsSampled)
	}

	_ = third // third keeps conv_one spread across two sampled threads.
}
