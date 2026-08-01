package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

type storedAgentThreadTopology struct {
	conversationID string
	threadID       string
	threadParentID string
	rfcMessageID   string
	status         string
	deliveryStatus string
}

func readAgentThreadTopology(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, messageID string) storedAgentThreadTopology {
	t.Helper()
	var got storedAgentThreadTopology
	if err := pool.QueryRow(
		context.Background(),
		`SELECT conversation_id,
		        COALESCE(thread_id, ''),
		        COALESCE(thread_parent_id, ''),
		        COALESCE(rfc_message_id_key, ''),
		        COALESCE(status, ''),
		        COALESCE(delivery_status, '')
		   FROM messages
		  WHERE id = $1`,
		messageID,
	).Scan(
		&got.conversationID,
		&got.threadID,
		&got.threadParentID,
		&got.rfcMessageID,
		&got.status,
		&got.deliveryStatus,
	); err != nil {
		t.Fatalf("read message topology %s: %v", messageID, err)
	}
	return got
}

func TestDeliverOutboundPersistsEmailTopologyIndependentlyOfConversationID(t *testing.T) {
	api, store, _, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "thread-api")

	parent, err := store.CreateInboundMessage(
		ctx, "", ag.ID, "alice@example.net", ag.ID,
		"<Inbound.Root@MAIL.Example.NET>", "Root", "conv_parent", "unread",
		[]byte("Message-ID: <Inbound.Root@MAIL.Example.NET>\r\nSubject: Root\r\n\r\nbody"),
		nil, nil, false, "", []string{ag.EmailAddress()}, nil, nil, identity.InboundScreening{},
	)
	if err != nil {
		t.Fatalf("create inbound parent: %v", err)
	}
	if !identity.IsValidThreadID(parent.ThreadID) {
		t.Fatalf("parent thread_id = %q, want valid server-owned id", parent.ThreadID)
	}

	send := func(subject, conversationID string) string {
		t.Helper()
		result, outboundErr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
			To:             []string{"recipient@example.net"},
			Subject:        subject,
			Body:           "body",
			ConversationID: conversationID,
		}, "send", "", nil, nil)
		if outboundErr != nil {
			t.Fatalf("DeliverOutbound(%s): %+v", subject, outboundErr)
		}
		if result.Status != "accepted" {
			t.Fatalf("DeliverOutbound(%s) status = %q, want accepted", subject, result.Status)
		}
		return result.MessageID
	}

	firstID := send("fresh one", "conv_shared")
	secondID := send("fresh two", "conv_shared")
	first := readAgentThreadTopology(t, pool, firstID)
	second := readAgentThreadTopology(t, pool, secondID)
	if first.conversationID != "conv_shared" || second.conversationID != "conv_shared" {
		t.Fatalf("fresh conversation IDs = (%q, %q), want conv_shared", first.conversationID, second.conversationID)
	}
	if !identity.IsValidThreadID(first.threadID) || !identity.IsValidThreadID(second.threadID) || first.threadID == second.threadID {
		t.Fatalf("fresh sends sharing conversation_id collapsed: first=%q second=%q", first.threadID, second.threadID)
	}
	if first.threadParentID != "" || second.threadParentID != "" {
		t.Fatalf("fresh sends have reply parents: first=%q second=%q", first.threadParentID, second.threadParentID)
	}

	replyResult, outboundErr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To:               []string{"alice@example.net"},
		Subject:          "Re: Root",
		Body:             "reply",
		ConversationID:   "conv_reply_override",
		ReplyToMessageID: parent.EmailMessageID,
	}, "reply", parent.EmailMessageID, parent, nil)
	if outboundErr != nil {
		t.Fatalf("DeliverOutbound(reply): %+v", outboundErr)
	}
	reply := readAgentThreadTopology(t, pool, replyResult.MessageID)
	if reply.conversationID != "conv_reply_override" {
		t.Errorf("reply conversation_id = %q, want explicit caller value", reply.conversationID)
	}
	if reply.threadID != parent.ThreadID || reply.threadParentID != parent.ID {
		t.Errorf(
			"reply topology = thread %q parent %q, want %q / %q",
			reply.threadID, reply.threadParentID, parent.ThreadID, parent.ID,
		)
	}

	forwardResult, outboundErr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To:             []string{"forward@example.net"},
		Subject:        "Fwd: Root",
		Body:           "forward",
		ConversationID: parent.ConversationID,
	}, "forward", parent.EmailMessageID, parent, nil)
	if outboundErr != nil {
		t.Fatalf("DeliverOutbound(forward): %+v", outboundErr)
	}
	forward := readAgentThreadTopology(t, pool, forwardResult.MessageID)
	if forward.conversationID != parent.ConversationID {
		t.Errorf("forward conversation_id = %q, want preserved caller value %q", forward.conversationID, parent.ConversationID)
	}
	if !identity.IsValidThreadID(forward.threadID) || forward.threadID == parent.ThreadID || forward.threadParentID != "" {
		t.Errorf(
			"forward topology = thread %q parent %q, source thread %q",
			forward.threadID, forward.threadParentID, parent.ThreadID,
		)
	}
}

func TestSelfSendPersistsPhysicalTwinsInOneThreadWithoutReplyParent(t *testing.T) {
	api, store, pool := setupCoreAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "thread-self")

	result, outboundErr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To:      []string{ag.EmailAddress()},
		Subject: "thread twins",
		Body:    "body",
	}, "send", "", nil, nil)
	if outboundErr != nil {
		t.Fatalf("DeliverOutbound(self): %+v", outboundErr)
	}

	rows, err := pool.Query(ctx,
		`SELECT id, direction, thread_id, COALESCE(thread_parent_id, '')
		   FROM messages
		  WHERE agent_id = $1 AND subject = 'thread twins'
		  ORDER BY direction`,
		ag.ID,
	)
	if err != nil {
		t.Fatalf("query self-send twins: %v", err)
	}
	defer rows.Close()

	type twin struct {
		id, direction, threadID, parentID string
	}
	var twins []twin
	for rows.Next() {
		var row twin
		if err := rows.Scan(&row.id, &row.direction, &row.threadID, &row.parentID); err != nil {
			t.Fatalf("scan self-send twin: %v", err)
		}
		twins = append(twins, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate self-send twins: %v", err)
	}
	if len(twins) != 2 {
		t.Fatalf("self-send rows = %+v, want Sent and Inbox twins", twins)
	}
	if twins[0].direction != "inbound" || twins[1].direction != "outbound" || twins[1].id != result.MessageID {
		t.Fatalf("self-send twin identities = %+v, outbound result=%q", twins, result.MessageID)
	}
	if !identity.IsValidThreadID(twins[0].threadID) || twins[0].threadID != twins[1].threadID {
		t.Fatalf("self-send twin threads = (%q, %q), want one valid shared thread", twins[0].threadID, twins[1].threadID)
	}
	if twins[0].parentID != "" || twins[1].parentID != "" {
		t.Fatalf("physical twins must not be reply parents: %+v", twins)
	}
}

func TestHITLReplyHoldCommitsOneIdempotentThreadDecision(t *testing.T) {
	api, store, _, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "thread-hold")

	parent, err := store.CreateInboundMessage(
		ctx, "", ag.ID, "alice@example.net", ag.ID,
		"<held-parent@example.net>", "Held parent", "conv_parent", "unread",
		[]byte("Message-ID: <held-parent@example.net>\r\nSubject: Held parent\r\n\r\nbody"),
		nil, nil, false, "", []string{ag.EmailAddress()}, nil, nil, identity.InboundScreening{},
	)
	if err != nil {
		t.Fatalf("create held reply parent: %v", err)
	}

	cfg := openProtection()
	cfg.OutboundGatePolicy = "allowlist"
	cfg.OutboundAllowlist = []string{"trusted@example.net"}
	cfg.OutboundGateAction = "review"
	if _, err := store.UpdateAgentProtection(ctx, ag.ID, user.ID, cfg); err != nil {
		t.Fatalf("enable outbound review: %v", err)
	}
	ag, err = store.GetAgentByEmail(ctx, ag.EmailAddress())
	if err != nil {
		t.Fatalf("reload protected agent: %v", err)
	}

	const (
		idemKey = "thread-hold-replay"
		route   = "/v1/agents/thread-hold/messages/parent/reply"
	)
	rawRequest := []byte(`{"to":["review-target@example.net"],"text":"held reply"}`)
	requestHash := idempotency.HashRequest(route, rawRequest)
	idemStore := idempotency.NewStore(pool)
	claim, err := idemStore.Claim(ctx, user.ID, idemKey, route, requestHash)
	if err != nil || claim.Outcome != idempotency.OutcomeAcquired {
		t.Fatalf("initial idempotency claim = (%+v, %v), want acquired", claim, err)
	}
	completeTx := func(ctx context.Context, tx pgx.Tx, result *agent.OutboundResult) error {
		if !result.Held || result.PendingMessageID == "" {
			t.Fatalf("hold completion result = %+v", result)
		}
		body, marshalErr := json.Marshal(map[string]any{
			"status":     "pending_review",
			"message_id": result.PendingMessageID,
		})
		if marshalErr != nil {
			return marshalErr
		}
		return idemStore.CompleteTx(ctx, tx, user.ID, idemKey, idempotency.CachedResponse{
			StatusCode:  202,
			ContentType: "application/json",
			Body:        body,
		})
	}

	result, outboundErr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To:               []string{"review-target@example.net"},
		Subject:          "Held threaded reply",
		Body:             "held reply",
		ConversationID:   "conv_hold_override",
		ReplyToMessageID: parent.EmailMessageID,
	}, "reply", parent.EmailMessageID, parent, completeTx)
	if outboundErr != nil {
		t.Fatalf("DeliverOutbound(held reply): %+v", outboundErr)
	}
	if !result.Held || result.PendingMessageID == "" {
		t.Fatalf("held reply result = %+v", result)
	}

	heldID := result.PendingMessageID
	beforeApproval := readAgentThreadTopology(t, pool, heldID)
	if beforeApproval.status != identity.MessageStatusPendingReview ||
		beforeApproval.conversationID != "conv_hold_override" ||
		beforeApproval.threadID != parent.ThreadID ||
		beforeApproval.threadParentID != parent.ID {
		t.Fatalf("held reply topology before approval = %+v, parent=%+v", beforeApproval, parent)
	}
	if beforeApproval.rfcMessageID != "" {
		t.Fatalf("queue-first held reply rfc_message_id_key = %q, want null until provider acceptance", beforeApproval.rfcMessageID)
	}

	replay, err := idemStore.Claim(ctx, user.ID, idemKey, route, requestHash)
	if err != nil || replay.Outcome != idempotency.OutcomeReplay {
		t.Fatalf("repeat idempotency claim = (%+v, %v), want replay", replay, err)
	}
	var replayBody map[string]any
	if err := json.Unmarshal(replay.Cached.Body, &replayBody); err != nil {
		t.Fatalf("decode cached hold result: %v", err)
	}
	if replayBody["message_id"] != heldID {
		t.Fatalf("replayed held message = %v, want %s", replayBody["message_id"], heldID)
	}
	var heldRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE agent_id=$1 AND subject='Held threaded reply'`,
		ag.ID,
	).Scan(&heldRows); err != nil {
		t.Fatalf("count held reply rows: %v", err)
	}
	if heldRows != 1 {
		t.Fatalf("idempotent hold rows = %d, want exactly one", heldRows)
	}

	approved, approveErr := api.ApprovePendingCore(
		ctx, user.ID, heldID, ag.EmailAddress(), agent.ApproveOverrides{}, nil,
	)
	if approveErr != nil {
		t.Fatalf("ApprovePendingCore: %+v", approveErr)
	}
	if approved == nil || approved.ID != heldID {
		t.Fatalf("approved reply = %+v, want %s", approved, heldID)
	}
	afterApproval := readAgentThreadTopology(t, pool, heldID)
	if afterApproval.threadID != beforeApproval.threadID ||
		afterApproval.threadParentID != beforeApproval.threadParentID ||
		afterApproval.conversationID != beforeApproval.conversationID {
		t.Fatalf("approval changed held thread decision: before=%+v after=%+v", beforeApproval, afterApproval)
	}
	if afterApproval.rfcMessageID != "" {
		t.Fatalf("queue-first approval invented rfc_message_id_key %q without provider result", afterApproval.rfcMessageID)
	}
}

func TestHumanApprovedSelfSendPreservesHeldThreadAcrossLocalTwins(t *testing.T) {
	api, store, _, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "thread-approved-self")

	cfg := openProtection()
	cfg.OutboundGatePolicy = "allowlist"
	cfg.OutboundAllowlist = []string{"trusted@example.net"}
	cfg.OutboundGateAction = "review"
	if _, err := store.UpdateAgentProtection(ctx, ag.ID, user.ID, cfg); err != nil {
		t.Fatalf("enable outbound review: %v", err)
	}
	ag, err := store.GetAgentByEmail(ctx, ag.EmailAddress())
	if err != nil {
		t.Fatalf("reload protected self-send agent: %v", err)
	}

	result, outboundErr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To:      []string{ag.EmailAddress()},
		Subject: "approved threaded self",
		Body:    "body",
	}, "send", "", nil, nil)
	if outboundErr != nil {
		t.Fatalf("DeliverOutbound(held self-send): %+v", outboundErr)
	}
	if !result.Held || result.PendingMessageID == "" {
		t.Fatalf("held self-send result = %+v", result)
	}

	held := readAgentThreadTopology(t, pool, result.PendingMessageID)
	if held.status != identity.MessageStatusPendingReview ||
		!identity.IsValidThreadID(held.threadID) ||
		held.threadParentID != "" {
		t.Fatalf("held self-send topology = %+v", held)
	}

	approved, approveErr := api.ApprovePendingCore(
		ctx, user.ID, result.PendingMessageID, ag.EmailAddress(), agent.ApproveOverrides{}, nil,
	)
	if approveErr != nil {
		t.Fatalf("ApprovePendingCore(self): %+v", approveErr)
	}
	if approved == nil || approved.ID != result.PendingMessageID {
		t.Fatalf("approved self-send = %+v, want %s", approved, result.PendingMessageID)
	}

	rows, err := pool.Query(ctx,
		`SELECT id, direction, thread_id, COALESCE(thread_parent_id, '')
		   FROM messages
		  WHERE agent_id=$1 AND subject='approved threaded self'
		  ORDER BY direction`,
		ag.ID,
	)
	if err != nil {
		t.Fatalf("query approved local twins: %v", err)
	}
	defer rows.Close()
	type approvedTwin struct {
		id, direction, threadID, parentID string
	}
	var twins []approvedTwin
	for rows.Next() {
		var row approvedTwin
		if err := rows.Scan(&row.id, &row.direction, &row.threadID, &row.parentID); err != nil {
			t.Fatalf("scan approved local twin: %v", err)
		}
		twins = append(twins, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate approved local twins: %v", err)
	}
	if len(twins) != 2 ||
		twins[0].direction != "inbound" ||
		twins[1].direction != "outbound" ||
		twins[1].id != result.PendingMessageID {
		t.Fatalf("approved local twins = %+v", twins)
	}
	if twins[0].threadID != held.threadID ||
		twins[1].threadID != held.threadID ||
		twins[0].parentID != "" ||
		twins[1].parentID != "" {
		t.Fatalf("approval changed held decision or made twins parents: held=%+v twins=%+v", held, twins)
	}
}
