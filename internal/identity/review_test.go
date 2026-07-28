package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func seedReviewAgent(t *testing.T, store *identity.Store, ctx context.Context, domain string) (userID, agentID string) {
	t.Helper()
	user, err := store.CreateOrGetUser(ctx, "o@"+domain, "O", "g-"+domain)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	ag, err := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return user.ID, ag.ID
}

func createInbound(t *testing.T, store *identity.Store, ctx context.Context, agentID, sender, subj string, sc identity.InboundScreening) string {
	t.Helper()
	m, err := store.CreateInboundMessage(ctx, "", agentID, sender, agentID, "", subj, "", "unread",
		[]byte("Subject: "+subj+"\r\n\r\nx"), nil, nil, false, "", []string{agentID}, nil, nil, sc)
	if err != nil {
		t.Fatalf("CreateInboundMessage(%s): %v", subj, err)
	}
	return m.ID
}

func inboxIDs(t *testing.T, store *identity.Store, ctx context.Context, agentID string) map[string]bool {
	t.Helper()
	msgs, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{AgentID: agentID, Direction: "inbound", Status: "all", Limit: 100})
	if err != nil {
		t.Fatalf("GetMessagesByAgent: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range msgs {
		ids[m.ID] = true
	}
	return ids
}

// TestInboundReview_HeldExcludedFromInbox is the delivery-boundary contract: held
// (pending_review) and dropped (review_rejected) inbound messages do NOT appear in
// the agent inbox; approving a held one makes it visible.
func TestInboundReview_HeldExcludedFromInbox(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := seedReviewAgent(t, store, ctx, "held.example.com")

	exp := time.Now().Add(time.Hour)
	normalID := createInbound(t, store, ctx, agentID, "ok@x.com", "normal", identity.InboundScreening{})
	heldID := createInbound(t, store, ctx, agentID, "evil@x.com", "held", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ScanAction: "review",
		ReviewReason: identity.ReviewReasonInboundScan, ApprovalExpiresAt: &exp,
	})
	blockedID := createInbound(t, store, ctx, agentID, "evil2@x.com", "blocked", identity.InboundScreening{
		Status: identity.MessageStatusReviewRejected, ScanAction: "block", ReviewReason: identity.ReviewReasonInboundScan,
	})

	ids := inboxIDs(t, store, ctx, agentID)
	if !ids[normalID] {
		t.Errorf("normal message should be in the inbox")
	}
	if ids[heldID] {
		t.Errorf("pending_review message must be excluded from the inbox")
	}
	if ids[blockedID] {
		t.Errorf("blocked (review_rejected) message must be excluded from the inbox")
	}

	// A reviewer from a DIFFERENT tenant cannot release this agent's held message
	// (C2 tenant isolation — scoped by agent_id).
	if err := store.ApproveInboundReview(ctx, heldID, "bot@someone-else.example.com", userID); err != identity.ErrNotPendingReview {
		t.Errorf("cross-tenant approve = %v, want ErrNotPendingReview", err)
	}

	// Approve the held message (owning agent) → it becomes visible.
	if err := store.ApproveInboundReview(ctx, heldID, agentID, userID); err != nil {
		t.Fatalf("ApproveInboundReview: %v", err)
	}
	if !inboxIDs(t, store, ctx, agentID)[heldID] {
		t.Errorf("approved message should now appear in the inbox")
	}
}

func TestInboundReviewApprovalAtomicallyAdvancesAuthenticatedEngagement(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := seedReviewAgent(t, store, ctx, "held-contact.example.com")
	const contact = "partner@fund.vc"
	enroll(t, store, userID, agentID, contact, "touch1")
	if _, err := store.RecordOutboundActivity(ctx, userID, agentID, contact, "conv_intro", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("RecordOutboundActivity: %v", err)
	}

	exp := time.Now().Add(time.Hour)
	msg, err := store.CreateInboundMessageAuthenticated(
		ctx, "", agentID,
		identity.InboundAuth{
			HeaderFrom:   contact,
			EnvelopeFrom: contact,
			Authentication: &emailauth.Authentication{
				DMARC: emailauth.DMARCResult{Status: emailauth.StatusPass},
			},
		},
		agentID, "", "Re: intro", "conv_intro", "unread", []byte("Subject: Re: intro\r\n\r\nyes"),
		false, "", []string{agentID}, nil, nil,
		identity.InboundScreening{
			Status: identity.MessageStatusPendingReview, ScanAction: "review",
			ReviewReason: identity.ReviewReasonInboundScan, ApprovalExpiresAt: &exp,
		},
	)
	if err != nil {
		t.Fatalf("CreateInboundMessageAuthenticated: %v", err)
	}

	if err := store.ApproveInboundReview(ctx, msg.ID, agentID, userID); err != nil {
		t.Fatalf("ApproveInboundReview: %v", err)
	}
	got, err := store.GetEngagement(ctx, userID, agentID, contact)
	if err != nil {
		t.Fatalf("GetEngagement: %v", err)
	}
	if got.LastInboundAt == nil || !got.Replied() || got.InboundCount != 1 {
		t.Fatalf("approved authenticated reply did not advance engagement atomically: %+v", got)
	}
}

// TestInboundReview_HeldUnreachableByAllReadPaths is the C1 regression: a held
// message must be invisible to the agent via EVERY read path, not just the inbox
// list (it leaked via get-by-id / reply / conversation / activity before the fix).
func TestInboundReview_HeldUnreachableByAllReadPaths(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	_, agentID := seedReviewAgent(t, store, ctx, "readpath.example.com")

	exp := time.Now().Add(time.Hour)
	heldID := createInbound(t, store, ctx, agentID, "evil@x.com", "held", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ScanAction: "review",
		ReviewReason: identity.ReviewReasonInboundScan, ApprovalExpiresAt: &exp,
	})
	if _, err := pool.Exec(ctx, `UPDATE messages SET conversation_id='conv_held', email_message_id='<held@evil.test>' WHERE id=$1`, heldID); err != nil {
		t.Fatalf("set conversation_id/email_message_id: %v", err)
	}

	if _, err := store.GetMessageWithContent(ctx, heldID, agentID); err == nil {
		t.Errorf("GetMessageWithContent returned a held message (fail-open)")
	}
	if _, err := store.GetInboundMessage(ctx, heldID); err == nil {
		t.Errorf("GetInboundMessage returned a held message (fail-open)")
	}
	// The reply-by-RFC-Message-ID path (agent reply with ReplyToMessageID) must
	// also refuse a held message.
	if _, err := store.GetInboundByEmailMessageID(ctx, agentID, "<held@evil.test>"); err == nil {
		t.Errorf("GetInboundByEmailMessageID returned a held message (fail-open)")
	}
	if conv, err := store.GetConversationByID(ctx, agentID, "conv_held"); err == nil && conv != nil {
		for _, m := range conv.Messages {
			if m.ID == heldID {
				t.Errorf("GetConversationByID leaked a held message")
			}
		}
	}
	acts, err := store.ListActivityByAgent(ctx, agentID, 50)
	if err != nil {
		t.Fatalf("ListActivityByAgent: %v", err)
	}
	for _, m := range acts {
		if m.ID == heldID {
			t.Errorf("ListActivityByAgent leaked a held message")
		}
	}
}

// TestInboundReview_TransitionsCompareAndSet covers approve/reject + the
// compare-and-set guard (a second transition on the same row is a no-op).
func TestInboundReview_TransitionsCompareAndSet(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := seedReviewAgent(t, store, ctx, "trans.example.com")
	exp := time.Now().Add(time.Hour)

	id := createInbound(t, store, ctx, agentID, "e@x.com", "a", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ApprovalExpiresAt: &exp,
	})
	if err := store.ApproveInboundReview(ctx, id, agentID, userID); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if err := store.ApproveInboundReview(ctx, id, agentID, userID); err != identity.ErrNotPendingReview {
		t.Errorf("second approve = %v, want ErrNotPendingReview", err)
	}

	id2 := createInbound(t, store, ctx, agentID, "e2@x.com", "b", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ApprovalExpiresAt: &exp,
	})
	if err := store.RejectInboundReview(ctx, id2, agentID, userID, "looks malicious"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	var st, reason string
	if err := pool.QueryRow(ctx, `SELECT status, COALESCE(rejection_reason,'') FROM messages WHERE id=$1`, id2).Scan(&st, &reason); err != nil {
		t.Fatalf("read: %v", err)
	}
	if st != identity.MessageStatusReviewRejected || reason != "looks malicious" {
		t.Errorf("rejected row = status %q reason %q", st, reason)
	}
}

// TestListExpiredReviews_AndExpire covers the worker's store surface: an overdue
// pending_review is listed and resolved per the agent's hitl_expiration_action
// (default 'reject' → review_expired_rejected).
func TestListExpiredReviews_AndExpire(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	_, agentID := seedReviewAgent(t, store, ctx, "expire.example.com")

	past := time.Now().Add(-time.Hour)
	id := createInbound(t, store, ctx, agentID, "e@x.com", "overdue", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ApprovalExpiresAt: &past,
	})

	cands, err := store.ListExpiredReviews(ctx, 10)
	if err != nil {
		t.Fatalf("ListExpiredReviews: %v", err)
	}
	var action string
	var found bool
	for _, c := range cands {
		if c.MessageID == id {
			found, action = true, c.ExpirationAction
		}
	}
	if !found {
		t.Fatalf("overdue review not listed")
	}

	if action == identity.HITLExpirationApprove {
		if err := store.ExpireApproveReview(ctx, id); err != nil {
			t.Fatalf("ExpireApproveReview: %v", err)
		}
	} else {
		if err := store.ExpireRejectReview(ctx, id, "ttl_expired"); err != nil {
			t.Fatalf("ExpireRejectReview: %v", err)
		}
	}
	var st string
	if err := pool.QueryRow(ctx, `SELECT status FROM messages WHERE id=$1`, id).Scan(&st); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Default hitl_expiration_action is 'reject'.
	if st != identity.MessageStatusReviewExpiredRejected {
		t.Errorf("expired status = %q, want review_expired_rejected", st)
	}
	// No longer overdue-listed once terminal.
	cands2, _ := store.ListExpiredReviews(ctx, 10)
	for _, c := range cands2 {
		if c.MessageID == id {
			t.Errorf("terminal review should not be re-listed")
		}
	}
}

// TestGetReviewMessage_DispatchView covers the review-queue single getter that
// the /approve+/reject endpoints use to branch on direction. Unlike every
// agent-facing read path it MUST see held inbound statuses (so an inbound hold is
// resolvable), it MUST report direction (so the handler can dispatch), and it MUST
// be scoped to agent_id (tenant isolation).
func TestGetReviewMessage_DispatchView(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := seedReviewAgent(t, store, ctx, "getreview.example.com")
	exp := time.Now().Add(time.Hour)

	// A held inbound message — invisible to the agent, but resolvable here.
	heldID := createInbound(t, store, ctx, agentID, "evil@x.com", "held", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ScanAction: "review",
		ReviewReason: identity.ReviewReasonInboundScan, ApprovalExpiresAt: &exp,
	})
	// Sanity: it is genuinely held (the agent read path refuses it).
	if _, err := store.GetInboundMessage(ctx, heldID); err == nil {
		t.Fatalf("precondition: held message must be unreadable via GetInboundMessage")
	}

	got, err := store.GetReviewMessage(ctx, heldID, agentID)
	if err != nil {
		t.Fatalf("GetReviewMessage(held inbound): %v", err)
	}
	if got.Direction != "inbound" || got.Status != identity.MessageStatusPendingReview {
		t.Errorf("held inbound view = dir %q status %q, want inbound/pending_review", got.Direction, got.Status)
	}
	if got.Sender != "evil@x.com" {
		t.Errorf("sender = %q, want evil@x.com (needed for the review_approved event)", got.Sender)
	}

	// Tenant isolation: another agent cannot resolve this agent's held message.
	if _, err := store.GetReviewMessage(ctx, heldID, "bot@someone-else.example.com"); err == nil {
		t.Errorf("cross-tenant GetReviewMessage must fail, got nil error")
	}

	// Outbound messages report direction=outbound so the handler falls through to
	// the send-approval path.
	out, err := store.CreateOutboundMessage(ctx, agentID, []string{"to@x.com"}, nil, nil, "subj", "send", "smtp", "", "", []byte("Subject: subj\r\n\r\nx"))
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}
	gotOut, err := store.GetReviewMessage(ctx, out.ID, agentID)
	if err != nil {
		t.Fatalf("GetReviewMessage(outbound): %v", err)
	}
	if gotOut.Direction != "outbound" {
		t.Errorf("outbound view direction = %q, want outbound", gotOut.Direction)
	}

	// A resolved (dropped) inbound message is STILL returnable — this is the
	// review-queue getter, not an agent read path; it must report the terminal
	// state so a late approve/reject surfaces a clean 409 rather than a 404.
	if err := store.RejectInboundReview(ctx, heldID, agentID, userID, "x"); err != nil {
		t.Fatalf("RejectInboundReview: %v", err)
	}
	resolved, err := store.GetReviewMessage(ctx, heldID, agentID)
	if err != nil {
		t.Fatalf("GetReviewMessage(rejected): %v", err)
	}
	if resolved.Status != identity.MessageStatusReviewRejected {
		t.Errorf("resolved status = %q, want review_rejected", resolved.Status)
	}

	// Unknown id → error (the handler maps this to 404).
	if _, err := store.GetReviewMessage(ctx, "msg_nope", agentID); err == nil {
		t.Errorf("unknown id must error")
	}
}

// TestInboundReview_RejectDeliveredMessageIsNoop is the safety property behind the
// compare-and-set guard: a normally-delivered inbound message (never held — status
// 'unread') must NOT be droppable into the hidden review_rejected state. Without
// the status guard, an account owner (or a confused-deputy bug) could silently
// disappear a legitimately delivered message via /reject.
func TestInboundReview_RejectDeliveredMessageIsNoop(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := seedReviewAgent(t, store, ctx, "delivered.example.com")

	// A clean delivery — no screening hold (default status path).
	delivered := createInbound(t, store, ctx, agentID, "ok@x.com", "hello", identity.InboundScreening{})

	if err := store.RejectInboundReview(ctx, delivered, agentID, userID, "nope"); err != identity.ErrNotPendingReview {
		t.Errorf("reject of a delivered message = %v, want ErrNotPendingReview (no-op)", err)
	}
	if err := store.ApproveInboundReview(ctx, delivered, agentID, userID); err != identity.ErrNotPendingReview {
		t.Errorf("approve of a delivered message = %v, want ErrNotPendingReview (no-op)", err)
	}
	// The row is untouched — still a normal, agent-visible delivery.
	var st string
	if err := pool.QueryRow(ctx, `SELECT status FROM messages WHERE id=$1`, delivered).Scan(&st); err != nil {
		t.Fatalf("read: %v", err)
	}
	if st == identity.MessageStatusReviewRejected {
		t.Error("a delivered message was dropped to review_rejected via /reject — must be impossible")
	}
}

// TestListReviews_SurfacesHoldReason is the review-queue reason contract: the
// coded review_reason (populated for EVERY hold path) comes back on both the
// list and detail. scan_score remains internal screening persistence; public
// confidence comes from validated protection events on the detail endpoint.
func TestListReviews_SurfacesHoldReason(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := seedReviewAgent(t, store, ctx, "reason.example.com")

	exp := time.Now().Add(time.Hour)
	// A scan-driven hold.
	scanID := createInbound(t, store, ctx, agentID, "evil@x.com", "scan-held", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ScanAction: "review",
		ReviewReason: identity.ReviewReasonInboundScan, ApprovalExpiresAt: &exp,
	})
	// A gate-driven hold: review_reason=sender_gate, no scan ran (nil score) —
	// the case flag_reason would cover but scan holds would not.
	gateID := createInbound(t, store, ctx, agentID, "blocked@x.com", "gate-held", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ScanAction: "review",
		ReviewReason: identity.ReviewReasonSenderGate, ApprovalExpiresAt: &exp,
	})

	items, err := store.ListReviews(ctx, userID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	byID := map[string]identity.ReviewListItem{}
	for _, it := range items {
		byID[it.ID] = it
	}

	scan, ok := byID[scanID]
	if !ok {
		t.Fatalf("scan hold %s missing from review queue", scanID)
	}
	if scan.ReviewReason != identity.ReviewReasonInboundScan {
		t.Errorf("scan hold review_reason = %q, want %q", scan.ReviewReason, identity.ReviewReasonInboundScan)
	}

	gate, ok := byID[gateID]
	if !ok {
		t.Fatalf("gate hold %s missing from review queue", gateID)
	}
	if gate.ReviewReason != identity.ReviewReasonSenderGate {
		t.Errorf("gate hold review_reason = %q, want %q", gate.ReviewReason, identity.ReviewReasonSenderGate)
	}
	// The detail surface (GET /v1/reviews/{id}) carries the same reason.
	detail, err := store.GetReviewWithContent(ctx, userID, scanID)
	if err != nil {
		t.Fatalf("GetReviewWithContent: %v", err)
	}
	if detail.ReviewReason != identity.ReviewReasonInboundScan {
		t.Errorf("detail review_reason = %q, want %q", detail.ReviewReason, identity.ReviewReasonInboundScan)
	}
}

func TestReviewReadsPreserveNullAuthenticationForLoopbackMessages(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := seedReviewAgent(t, store, ctx, "loopback-review.example.com")

	exp := time.Now().Add(time.Hour)
	heldID := createInbound(t, store, ctx, agentID, agentID, "loopback-held", identity.InboundScreening{
		Status: identity.MessageStatusPendingReview, ScanAction: "review",
		ReviewReason: identity.ReviewReasonInboundScan, ApprovalExpiresAt: &exp,
	})
	deliveredID := createInbound(t, store, ctx, agentID, agentID, "loopback-delivered", identity.InboundScreening{})
	if _, err := pool.Exec(ctx, `UPDATE messages SET method='loopback', authentication=NULL, auth_verdict=NULL WHERE id = ANY($1)`, []string{heldID, deliveredID}); err != nil {
		t.Fatalf("mark loopback messages: %v", err)
	}

	items, err := store.ListReviews(ctx, userID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	foundHeld := false
	for _, item := range items {
		if item.ID == heldID && item.Authentication != nil {
			t.Fatalf("held loopback authentication = %+v, want nil", item.Authentication)
		}
		foundHeld = foundHeld || item.ID == heldID
	}
	if !foundHeld {
		t.Fatalf("held loopback %s missing from review list", heldID)
	}

	detail, err := store.GetReviewWithContent(ctx, userID, deliveredID)
	if err != nil {
		t.Fatalf("GetReviewWithContent: %v", err)
	}
	if detail.Authentication != nil {
		t.Fatalf("delivered loopback authentication = %+v, want nil", detail.Authentication)
	}
}
