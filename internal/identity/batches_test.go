package identity_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// batchTestSetup provisions a user + verified domain + agent and returns
// (userID, agentID). Similar to convoTestSetup (see conversations_test.go)
// but also surfaces userID because the batches table needs both FKs.
func batchTestSetup(t *testing.T, store *identity.Store, prefix string) (userID, agentID string) {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "owner-"+prefix+"@example.com", "Owner", "google-"+prefix)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain := prefix + ".example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	agent, err := store.CreateAgent(ctx, "bot@"+domain, domain, "", "https://example.com/webhook", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return user.ID, agent.ID
}

func TestCreateBatchTx_InsertAndFetch(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := batchTestSetup(t, store, "batch-insert")

	batchID := identity.NewBatchID()
	dropped := []identity.BatchSuppressedItem{
		{ItemIndex: 3, Address: "hardbounce@example.com", Reason: "hard_bounce"},
	}
	droppedJSON, err := json.Marshal(dropped)
	if err != nil {
		t.Fatalf("marshal suppressed: %v", err)
	}
	in := &identity.Batch{
		BatchID:        batchID,
		UserID:         userID,
		AgentID:        agentID,
		Requested:      10,
		Accepted:       9,
		SuppressedJSON: droppedJSON,
		RequestID:      "req_test_insert",
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return store.CreateBatchTx(ctx, tx, in)
	}); err != nil {
		t.Fatalf("CreateBatchTx: %v", err)
	}

	got, err := store.GetBatch(ctx, batchID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if got == nil {
		t.Fatalf("GetBatch: nil after insert")
	}
	if got.BatchID != batchID {
		t.Errorf("BatchID: got %q, want %q", got.BatchID, batchID)
	}
	if got.UserID != userID {
		t.Errorf("UserID: got %q, want %q", got.UserID, userID)
	}
	if got.AgentID != agentID {
		t.Errorf("AgentID: got %q, want %q", got.AgentID, agentID)
	}
	if got.Requested != 10 || got.Accepted != 9 {
		t.Errorf("counts: requested=%d accepted=%d, want 10/9", got.Requested, got.Accepted)
	}
	if got.RequestID != "req_test_insert" {
		t.Errorf("RequestID: got %q", got.RequestID)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt is zero")
	}
	decoded, err := got.DecodeSuppressed()
	if err != nil {
		t.Fatalf("DecodeSuppressed: %v", err)
	}
	if len(decoded) != 1 || decoded[0].Address != "hardbounce@example.com" || decoded[0].ItemIndex != 3 || decoded[0].Reason != "hard_bounce" {
		t.Errorf("suppressed round-trip mismatch: %+v", decoded)
	}
}

func TestCreateBatchTx_EmptySuppressedDefaultsToArray(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := batchTestSetup(t, store, "batch-empty-supp")

	batchID := identity.NewBatchID()
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return store.CreateBatchTx(ctx, tx, &identity.Batch{
			BatchID: batchID, UserID: userID, AgentID: agentID, Requested: 5, Accepted: 5,
		})
	}); err != nil {
		t.Fatalf("CreateBatchTx: %v", err)
	}
	got, err := store.GetBatch(ctx, batchID)
	if err != nil || got == nil {
		t.Fatalf("GetBatch: got=%v err=%v", got, err)
	}
	decoded, err := got.DecodeSuppressed()
	if err != nil {
		t.Fatalf("DecodeSuppressed: %v", err)
	}
	if len(decoded) != 0 {
		t.Errorf("expected empty suppressed slice, got %d items", len(decoded))
	}
}

func TestCreateBatchTx_ValidationErrors(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	cases := []struct {
		name  string
		batch *identity.Batch
	}{
		{"nil batch", nil},
		{"empty batch_id", &identity.Batch{UserID: "u", AgentID: "a"}},
		{"empty user_id", &identity.Batch{BatchID: "bat_x", AgentID: "a"}},
		{"empty agent_id", &identity.Batch{BatchID: "bat_x", UserID: "u"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.WithTx(ctx, func(tx pgx.Tx) error {
				return store.CreateBatchTx(ctx, tx, tc.batch)
			})
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestGetBatch_NotFound(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	got, err := store.GetBatch(ctx, "bat_nonexistent_00000000000000")
	if err != nil {
		t.Fatalf("GetBatch on missing id returned err: %v", err)
	}
	if got != nil {
		t.Fatalf("GetBatch on missing id returned non-nil: %+v", got)
	}
}

func TestBatchStatusRollupByID_EmptyBatch(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := batchTestSetup(t, store, "batch-rollup-empty")

	// Insert a batch with no child messages (all suppressed).
	batchID := identity.NewBatchID()
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return store.CreateBatchTx(ctx, tx, &identity.Batch{
			BatchID: batchID, UserID: userID, AgentID: agentID, Requested: 3, Accepted: 0,
		})
	}); err != nil {
		t.Fatalf("CreateBatchTx: %v", err)
	}

	rollup, err := store.BatchStatusRollupByID(ctx, batchID)
	if err != nil {
		t.Fatalf("BatchStatusRollupByID: %v", err)
	}
	// Every counter should be zero — all-suppressed batch is valid per §14 Q9.
	if rollup.Accepted+rollup.Sending+rollup.Sent+rollup.Delivered+
		rollup.Deferred+rollup.Bounced+rollup.Complained+rollup.Failed != 0 {
		t.Errorf("expected all zeros, got %+v", rollup)
	}
}

func TestCreateOutboundMessagesTx_BulkAndRollup(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := batchTestSetup(t, store, "batch-bulk")

	batchID := identity.NewBatchID()
	// Insert batch header + 3 messages in one tx (mirrors the real accept-tx
	// shape from docs/design/batch-send.md §9 step 10).
	var msgs []*identity.Message
	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := store.CreateBatchTx(ctx, tx, &identity.Batch{
			BatchID: batchID, UserID: userID, AgentID: agentID, Requested: 3, Accepted: 3,
		}); err != nil {
			return err
		}
		inputs := []identity.OutboundMessageInput{
			{AgentID: agentID, ToRecipients: []string{"a@example.com"}, Subject: "hi a", MsgType: "send", Method: "smtp", DeliveryStatus: "accepted", BatchID: batchID},
			{AgentID: agentID, ToRecipients: []string{"b@example.com"}, Subject: "hi b", MsgType: "send", Method: "smtp", DeliveryStatus: "sent", BatchID: batchID},
			{AgentID: agentID, ToRecipients: []string{"c@example.com"}, Subject: "hi c", MsgType: "send", Method: "smtp", DeliveryStatus: "delivered", BatchID: batchID},
		}
		var err error
		msgs, err = store.CreateOutboundMessagesTx(ctx, tx, inputs)
		return err
	})
	if err != nil {
		t.Fatalf("accept-tx: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	// Positional alignment — msgs[i] corresponds to inputs[i].
	for i, want := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if msgs[i].Recipient != want {
			t.Errorf("msgs[%d].Recipient: got %q, want %q", i, msgs[i].Recipient, want)
		}
		if msgs[i].ID == "" {
			t.Errorf("msgs[%d].ID is empty", i)
		}
	}

	// Verify each row is actually persisted and carries batch_id.
	for i, m := range msgs {
		var storedBatchID string
		if err := pool.QueryRow(ctx, `SELECT batch_id FROM messages WHERE id = $1`, m.ID).Scan(&storedBatchID); err != nil {
			t.Fatalf("select msgs[%d].batch_id: %v", i, err)
		}
		if storedBatchID != batchID {
			t.Errorf("msgs[%d].batch_id in DB: got %q, want %q", i, storedBatchID, batchID)
		}
	}

	// Rollup should reflect the 3 different delivery_status values.
	rollup, err := store.BatchStatusRollupByID(ctx, batchID)
	if err != nil {
		t.Fatalf("BatchStatusRollupByID: %v", err)
	}
	if rollup.Accepted != 1 {
		t.Errorf("Accepted: got %d, want 1", rollup.Accepted)
	}
	if rollup.Sent != 1 {
		t.Errorf("Sent: got %d, want 1", rollup.Sent)
	}
	if rollup.Delivered != 1 {
		t.Errorf("Delivered: got %d, want 1", rollup.Delivered)
	}
	if rollup.Sending+rollup.Deferred+rollup.Bounced+rollup.Complained+rollup.Failed != 0 {
		t.Errorf("unexpected non-zero counters: %+v", rollup)
	}
}

func TestCreateOutboundMessagesTx_EmptyInput(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		msgs, err := store.CreateOutboundMessagesTx(ctx, tx, nil)
		if err != nil {
			return err
		}
		if len(msgs) != 0 {
			t.Errorf("expected empty result, got %d", len(msgs))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("empty input tx: %v", err)
	}
}

func TestCreateOutboundMessagesTx_MissingAgentIDFails(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := store.CreateOutboundMessagesTx(ctx, tx, []identity.OutboundMessageInput{
			{AgentID: ""}, // invalid
		})
		return err
	})
	if err == nil {
		t.Fatalf("expected error for empty AgentID input")
	}
}

// TestGetMessagesByAgent_BatchIDFilter verifies the batch_id list filter
// (docs/design/batch-send.md §7.2): listing an agent's messages filtered by
// batch_id returns only that batch's children.
func TestGetMessagesByAgent_BatchIDFilter(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := batchTestSetup(t, store, "batch-listfilter")

	batchID := identity.NewBatchID()
	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := store.CreateBatchTx(ctx, tx, &identity.Batch{
			BatchID: batchID, UserID: userID, AgentID: agentID, Requested: 2, Accepted: 2,
		}); err != nil {
			return err
		}
		// 2 messages in the batch...
		if _, err := store.CreateOutboundMessagesTx(ctx, tx, []identity.OutboundMessageInput{
			{AgentID: agentID, ToRecipients: []string{"in1@x.com"}, Subject: "in batch 1", MsgType: "send", Method: "smtp", DeliveryStatus: "accepted", BatchID: batchID},
			{AgentID: agentID, ToRecipients: []string{"in2@x.com"}, Subject: "in batch 2", MsgType: "send", Method: "smtp", DeliveryStatus: "accepted", BatchID: batchID},
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("setup batch: %v", err)
	}
	// ...and 1 message NOT in the batch (single-send, batch_id empty).
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := store.CreateOutboundMessageTx(ctx, tx, agentID,
			[]string{"solo@x.com"}, nil, nil, "single send", "send", "smtp", "", "", nil, "accepted", "", "")
		return e
	}); err != nil {
		t.Fatalf("single send: %v", err)
	}

	// Filter by batch_id → only the 2 batch children.
	filtered, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{
		AgentID: agentID, Direction: "outbound", BatchID: batchID, Limit: 100,
	})
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("batch_id filter returned %d messages, want 2", len(filtered))
	}
	for _, m := range filtered {
		if m.Recipient == "solo@x.com" {
			t.Errorf("single-send leaked into batch_id filter")
		}
	}

	// No filter → all 3.
	all, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{
		AgentID: agentID, Direction: "outbound", Limit: 100,
	})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unfiltered returned %d, want 3", len(all))
	}
}

func TestNewBatchID_HasExpectedPrefix(t *testing.T) {
	id := identity.NewBatchID()
	if len(id) < 5 || id[:4] != "bat_" {
		t.Errorf("expected 'bat_' prefix, got %q", id)
	}
	id2 := identity.NewBatchID()
	if id == id2 {
		t.Errorf("consecutive NewBatchID calls collided: %q", id)
	}
}
