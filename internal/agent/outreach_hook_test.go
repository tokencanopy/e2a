package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// TestMarkSentUpdatesOutreachFromToRecipients is the regression guard for a bug
// that made the outbound outreach hook a complete no-op in production.
//
// The hook originally keyed its engagement update off Message.Recipient. The
// terminal path never populates that scalar — MarkOutboundSentTx scans
// ToRecipients into an array and leaves Recipient empty — so the UPDATE matched
// on "" and silently affected zero rows. No error, no log, counters frozen.
//
// Every existing test passed, because they build an OutboundSentInfo by hand
// with Recipient set. Only driving a real send revealed it. This test therefore
// goes through the REAL MarkSent against a REAL row, which is the only shape
// that can catch a recurrence: it never mentions Recipient, so a future change
// that reintroduces the dependency fails here.
func TestMarkSentUpdatesOutreachFromToRecipients(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "outreach-hook@example.com", "Owner", "google-outreach-hook")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "hook.example.com", user.ID); err != nil {
		t.Fatalf("claim domain: %v", err)
	}
	const agentAddr = "raise@hook.example.com"
	if _, err := store.CreateAgent(ctx, agentAddr, "hook.example.com", "",
		"https://example.com/webhook", "", user.ID); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	const recipient = "partner@hook.vc"
	stage := "touch1"
	if _, _, err := store.UpsertEngagement(ctx, user.ID, agentAddr, recipient, &stage, nil, nil); err != nil {
		t.Fatalf("enrol: %v", err)
	}

	msg, err := store.CreateOutboundMessage(ctx, agentAddr, []string{recipient}, nil, nil,
		"Intro", "send", "smtp", "", "conv-hook-1", []byte("raw"))
	if err != nil {
		t.Fatalf("create outbound message: %v", err)
	}

	// MarkOutboundSentTx only settles a row already claimed for sending, so put
	// the message in the state the worker would have left it in.
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET delivery_status = 'sending' WHERE id = $1`, msg.ID); err != nil {
		t.Fatalf("claim message: %v", err)
	}

	sendStore := agent.NewOutboundSendStore(store, webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)),
		usage.NewNoopUsageTracker())

	sentAt := time.Now().UTC().Truncate(time.Second)
	if err := sendStore.MarkSent(ctx, msg.ID, 0, 0, sentAt, "provider-1", ""); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	e, err := store.GetEngagement(ctx, user.ID, agentAddr, recipient)
	if err != nil {
		t.Fatalf("get engagement: %v", err)
	}
	if e.OutboundCount != 1 {
		t.Errorf("outbound_count = %d, want 1 — the terminal send did not reach the engagement", e.OutboundCount)
	}
	if e.FirstOutboundAt == nil || e.LastOutboundAt == nil {
		t.Errorf("outbound timestamps not set: first=%v last=%v", e.FirstOutboundAt, e.LastOutboundAt)
	}
	if e.LastConversationID != "conv-hook-1" {
		t.Errorf("last_conversation_id = %q, want conv-hook-1", e.LastConversationID)
	}
}

// TestMarkSentDoesNotEnrolUnknownRecipients pins the update-only rule through
// the real send path: ordinary correspondence must not silently populate an
// outreach list with people nobody is campaigning against.
func TestMarkSentDoesNotEnrolUnknownRecipients(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "outreach-noenrol@example.com", "Owner", "google-outreach-noenrol")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "noenrol.example.com", user.ID); err != nil {
		t.Fatalf("claim domain: %v", err)
	}
	const agentAddr = "raise@noenrol.example.com"
	if _, err := store.CreateAgent(ctx, agentAddr, "noenrol.example.com", "",
		"https://example.com/webhook", "", user.ID); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	msg, err := store.CreateOutboundMessage(ctx, agentAddr, []string{"stranger@nowhere.vc"}, nil, nil,
		"one-off", "send", "smtp", "", "conv-noenrol", []byte("raw"))
	if err != nil {
		t.Fatalf("create outbound message: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET delivery_status = 'sending' WHERE id = $1`, msg.ID); err != nil {
		t.Fatalf("claim message: %v", err)
	}
	sendStore := agent.NewOutboundSendStore(store, webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)),
		usage.NewNoopUsageTracker())
	if err := sendStore.MarkSent(ctx, msg.ID, 0, 0, time.Now().UTC(), "provider-2", ""); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM contact_engagements WHERE user_id = $1`, user.ID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("a send to an un-enrolled address created %d engagement(s)", count)
	}
}
