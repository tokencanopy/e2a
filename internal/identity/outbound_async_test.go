package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func TestStampSendJobIDTxRejectsInboundAtomically(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "stamp-inbound")
	inbound, err := store.CreateInboundMessage(ctx, "", agentID, "sender@example.net", agentID,
		"<stamp-inbound@example.net>", "Inbound", "", "unread", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("CreateInboundMessage: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS task6_stamp_jobs; CREATE TABLE task6_stamp_jobs (id bigint PRIMARY KEY, message_id text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS task6_stamp_jobs`) })

	err = store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO task6_stamp_jobs (id, message_id) VALUES (9191, $1)`, inbound.ID); err != nil {
			return err
		}
		return store.StampSendJobIDTx(ctx, tx, inbound.ID, 9191)
	})
	if !errors.Is(err, identity.ErrMessageNotFound) {
		t.Fatalf("StampSendJobIDTx(inbound) error = %v, want ErrMessageNotFound", err)
	}

	var sendJobID *int64
	if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, inbound.ID).Scan(&sendJobID); err != nil {
		t.Fatal(err)
	}
	var jobs, queueTransitions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task6_stamp_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='queue.outbound_submission'`, inbound.ID).Scan(&queueTransitions); err != nil {
		t.Fatal(err)
	}
	if sendJobID != nil || jobs != 0 || queueTransitions != 0 {
		t.Fatalf("inbound stamp partially committed: send_job_id=%v jobs=%d queue_transitions=%d", sendJobID, jobs, queueTransitions)
	}
}

// TestCreateOutboundMessageTx_AcceptedRow pins the async accept-tx store shape
// (async-message-pipeline.md, slice C): the row lands with delivery_status='accepted',
// status='sent' (the two-column model: lifecycle vs send-progression), an EMPTY
// provider_message_id, and the envelope_from / sent_as decided at compose time;
// StampSendJobIDTx records the River job id in the same spirit.
func TestCreateOutboundMessageTx_AcceptedRow(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "async-accept")

	var msg *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := store.CreateOutboundMessageTx(ctx, tx, agentID,
			[]string{"alice@gmail.com"}, []string{"cc@gmail.com"}, []string{"bcc@gmail.com"},
			"Hi", "send", "smtp", "", "conv-async",
			[]byte("From: bot\r\n\r\nbody"), "accepted", "agent@test.e2a.dev", "relay")
		if err != nil {
			return err
		}
		msg = m
		return store.StampSendJobIDTx(ctx, tx, m.ID, 4242)
	}); err != nil {
		t.Fatalf("accept tx: %v", err)
	}

	var (
		deliveryStatus, status, providerID, sentAs, envFrom string
		sendJobID                                           *int64
	)
	if err := pool.QueryRow(ctx,
		`SELECT delivery_status, status, COALESCE(provider_message_id,''), COALESCE(sent_as,''), COALESCE(envelope_from,''), send_job_id
		   FROM messages WHERE id=$1`, msg.ID,
	).Scan(&deliveryStatus, &status, &providerID, &sentAs, &envFrom, &sendJobID); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if deliveryStatus != "accepted" {
		t.Errorf("delivery_status = %q, want accepted", deliveryStatus)
	}
	if status != identity.MessageStatusSent {
		t.Errorf("status = %q, want %q (lifecycle column)", status, identity.MessageStatusSent)
	}
	if providerID != "" {
		t.Errorf("provider_message_id = %q, want empty at accept", providerID)
	}
	if sentAs != "relay" {
		t.Errorf("sent_as = %q, want relay", sentAs)
	}
	if envFrom != "agent@test.e2a.dev" {
		t.Errorf("envelope_from = %q, want agent@test.e2a.dev", envFrom)
	}
	if sendJobID == nil || *sendJobID != 4242 {
		t.Errorf("send_job_id = %v, want 4242", sendJobID)
	}

	// LoadOutboundForSend returns the worker payload: envelope = to+cc+bcc, raw, etc.
	p, err := store.LoadOutboundForSend(ctx, msg.ID)
	if err != nil {
		t.Fatalf("LoadOutboundForSend: %v", err)
	}
	if p == nil {
		t.Fatal("LoadOutboundForSend returned nil for an accepted row")
	}
	if p.DeliveryStatus != "accepted" || p.EnvelopeFrom != "agent@test.e2a.dev" || p.SentAs != "relay" {
		t.Errorf("payload = %+v, want accepted/agent@.../relay", p)
	}
	if p.Domain != "async-accept.example.com" || p.MessageType != "send" {
		t.Errorf("payload ramp identity = domain %q type %q, want async-accept.example.com/send", p.Domain, p.MessageType)
	}
	if p.AgentID != agentID {
		t.Errorf("payload AgentID = %q, want %q", p.AgentID, agentID)
	}
	if len(p.Recipients) != 3 {
		t.Errorf("recipients = %v, want 3 (to+cc+bcc)", p.Recipients)
	}
	if string(p.Raw) != "From: bot\r\n\r\nbody" {
		t.Errorf("raw = %q, want the persisted MIME", p.Raw)
	}

	// A missing row loads as (nil, nil) — the worker's no-op signal.
	gone, err := store.LoadOutboundForSend(ctx, "msg_does_not_exist")
	if err != nil || gone != nil {
		t.Errorf("LoadOutboundForSend(missing) = (%v, %v), want (nil, nil)", gone, err)
	}
}

func TestClaimOutboundForSend_JobOwnership(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "async-claim-owner")

	var msgID string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := store.CreateOutboundMessageTx(ctx, tx, agentID,
			[]string{"a@gmail.com"}, nil, nil, "S", "send", "smtp", "", "conv-claim",
			[]byte("raw"), "accepted", "agent@test.e2a.dev", "relay")
		if err != nil {
			return err
		}
		msgID = m.ID
		return store.StampSendJobIDTx(ctx, tx, msgID, 4242)
	}); err != nil {
		t.Fatalf("accept tx: %v", err)
	}

	for i := 0; i < 2; i++ {
		payload, err := store.ClaimOutboundForSend(ctx, msgID, 4242)
		if err != nil || payload == nil {
			t.Fatalf("same-job claim %d = (%v, %v), want payload", i+1, payload, err)
		}
		if payload.AgentID != agentID {
			t.Fatalf("same-job claim AgentID = %q, want %q", payload.AgentID, agentID)
		}
	}
	payload, err := store.ClaimOutboundForSend(ctx, msgID, 4343)
	if err != nil || payload != nil {
		t.Fatalf("foreign-job claim = (%v, %v), want (nil, nil)", payload, err)
	}
	if err := store.ReleaseOutboundSendClaim(ctx, msgID, 4242); err != nil {
		t.Fatalf("ReleaseOutboundSendClaim: %v", err)
	}
	var status string
	var claimedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT delivery_status, send_claimed_at FROM messages WHERE id=$1`, msgID,
	).Scan(&status, &claimedAt); err != nil {
		t.Fatalf("read released claim: %v", err)
	}
	if status != "accepted" || claimedAt != nil {
		t.Fatalf("released claim = status %q claimed_at %v, want accepted/nil", status, claimedAt)
	}
}

func TestClaimOutboundForSend_PartialFallbackProvenanceRemainsClaimable(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "async-partial-fallback")

	seed := func(t *testing.T, jobID int64) string {
		t.Helper()
		var messageID string
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			m, err := store.CreateOutboundMessageTx(ctx, tx, agentID, []string{"a@gmail.com"}, nil, nil, "S", "send", "smtp", "", "conv-partial", []byte("raw"), "accepted", "agent@test.e2a.dev", "relay")
			if err != nil {
				return err
			}
			messageID = m.ID
			return store.StampSendJobIDTx(ctx, tx, messageID, jobID)
		}); err != nil {
			t.Fatal(err)
		}
		return messageID
	}

	cases := []struct {
		name   string
		jobID  int64
		update string
	}{
		{"missing observation", 5001, `delivery_failure_source='local',delivery_failure_reason_code='submission.cancelled'`},
		{"source reason mismatch", 5002, `delivery_failure_source='local',delivery_failure_reason_code='submission.provider_rejected',delivery_failure_occurred_at=now(),delivery_failure_attempt=2`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			messageID := seed(t, tc.jobID)
			if _, err := pool.Exec(ctx, `UPDATE messages SET `+tc.update+` WHERE id=$1`, messageID); err != nil {
				t.Fatal(err)
			}
			payload, err := store.ClaimOutboundForSend(ctx, messageID, tc.jobID)
			if err != nil || payload == nil {
				t.Fatalf("ClaimOutboundForSend = (%v, %v), want claimable partial provenance", payload, err)
			}
		})
	}
}

func TestOutboundForSend_UsesRegisteredParentDomain(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "claim-parent-domain")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "parent.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, "parent.example.com", user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	agent, err := store.CreateAgent(ctx, "agent@child.parent.example.com", "parent.example.com", "", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	var messageID string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		message, err := store.CreateOutboundMessageTx(ctx, tx, agent.ID,
			[]string{"recipient@example.net"}, nil, nil, "Subject", "send", "smtp", "", "conv-parent-domain",
			[]byte("raw"), "accepted", agent.EmailAddress(), "relay")
		if err != nil {
			return err
		}
		messageID = message.ID
		return store.StampSendJobIDTx(ctx, tx, messageID, 5150)
	}); err != nil {
		t.Fatalf("accept tx: %v", err)
	}

	loaded, err := store.LoadOutboundForSend(ctx, messageID)
	if err != nil {
		t.Fatalf("LoadOutboundForSend: %v", err)
	}
	if loaded == nil || loaded.Domain != "parent.example.com" {
		t.Fatalf("loaded payload = %+v, want registered parent domain", loaded)
	}

	payload, err := store.ClaimOutboundForSend(ctx, messageID, 5150)
	if err != nil {
		t.Fatalf("ClaimOutboundForSend: %v", err)
	}
	if payload == nil {
		t.Fatal("ClaimOutboundForSend returned nil")
	}
	if payload.Domain != "parent.example.com" {
		t.Fatalf("payload domain = %q, want registered parent domain", payload.Domain)
	}
}

// TestMarkOutboundSentTx flips a claimed sending row to sent with the provider id +
// per-recipient rows, and returns the owning user/domain for the caller.
func TestMarkOutboundSentTx(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "async-sent")

	var msgID string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := store.CreateOutboundMessageTx(ctx, tx, agentID,
			[]string{"a@gmail.com"}, nil, nil, "S", "send", "smtp", "", "conv-s",
			[]byte("raw"), "accepted", "agent@test.e2a.dev", "relay")
		msgID = m.ID
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, msgID)
		return err
	}); err != nil {
		t.Fatalf("accept tx: %v", err)
	}

	var info *identity.OutboundSentInfo
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		i, err := store.MarkOutboundSentTx(ctx, tx, msgID, "<ses-123@amazonses.com>")
		info = i
		return err
	}); err != nil {
		t.Fatalf("MarkOutboundSentTx: %v", err)
	}
	if info == nil || info.UserID == "" || info.Domain == "" {
		t.Fatalf("info = %+v, want user+domain populated", info)
	}

	var deliveryStatus, providerID string
	if err := pool.QueryRow(ctx,
		`SELECT delivery_status, provider_message_id FROM messages WHERE id=$1`, msgID,
	).Scan(&deliveryStatus, &providerID); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if deliveryStatus != "sent" {
		t.Errorf("delivery_status = %q, want sent", deliveryStatus)
	}
	if providerID != "<ses-123@amazonses.com>" {
		t.Errorf("provider_message_id = %q, want the SES id", providerID)
	}
	var rcpt int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM message_recipients WHERE message_id=$1 AND status='sent'`, msgID,
	).Scan(&rcpt); err != nil {
		t.Fatalf("count recipients: %v", err)
	}
	if rcpt != 1 {
		t.Errorf("sent recipient rows = %d, want 1", rcpt)
	}

	// A gone row marks as (nil, nil) — no-op.
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		i, err := store.MarkOutboundSentTx(ctx, tx, "msg_gone", "x")
		if i != nil {
			t.Errorf("MarkOutboundSentTx(missing) info = %+v, want nil", i)
		}
		return err
	}); err != nil {
		t.Fatalf("MarkOutboundSentTx(missing): %v", err)
	}
}

// TestMarkOutboundFailedTx flips a claimed sending row to failed with the detail.
func TestMarkOutboundFailedTx(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "async-failed")

	var msgID string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := store.CreateOutboundMessageTx(ctx, tx, agentID,
			[]string{"a@gmail.com"}, nil, nil, "F", "send", "smtp", "", "conv-f",
			[]byte("raw"), "accepted", "agent@test.e2a.dev", "relay")
		msgID = m.ID
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, msgID)
		return err
	}); err != nil {
		t.Fatalf("accept tx: %v", err)
	}

	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := store.MarkOutboundFailedTx(ctx, tx, msgID, "550 mailbox unavailable", delivery.FailureSourceProvider)
		return err
	}); err != nil {
		t.Fatalf("MarkOutboundFailedTx: %v", err)
	}

	var deliveryStatus, detail, failureSource string
	if err := pool.QueryRow(ctx,
		`SELECT delivery_status, COALESCE(delivery_detail,''), COALESCE(delivery_failure_source,'') FROM messages WHERE id=$1`, msgID,
	).Scan(&deliveryStatus, &detail, &failureSource); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if deliveryStatus != "failed" {
		t.Errorf("delivery_status = %q, want failed", deliveryStatus)
	}
	if detail != "550 mailbox unavailable" {
		t.Errorf("delivery_detail = %q, want the failure detail", detail)
	}
	if failureSource != "provider" {
		t.Errorf("delivery_failure_source = %q, want the provenance recorded", failureSource)
	}

	var sentMsgID string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := store.CreateOutboundMessageTx(ctx, tx, agentID,
			[]string{"b@gmail.com"}, nil, nil, "Already sent", "send", "smtp", "", "conv-sent",
			[]byte("raw"), "accepted", "agent@test.e2a.dev", "relay")
		if err != nil {
			return err
		}
		sentMsgID = m.ID
		if _, err := tx.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, sentMsgID); err != nil {
			return err
		}
		_, err = store.MarkOutboundSentTx(ctx, tx, sentMsgID, "<provider-sent>")
		return err
	}); err != nil {
		t.Fatalf("create sent message: %v", err)
	}

	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		info, err := store.MarkOutboundFailedTx(ctx, tx, sentMsgID, "late terminal job", delivery.FailureSourceLocal)
		if info != nil {
			t.Errorf("MarkOutboundFailedTx(sent) info = %+v, want nil", info)
		}
		return err
	}); err != nil {
		t.Fatalf("MarkOutboundFailedTx(sent): %v", err)
	}

	var providerID string
	if err := pool.QueryRow(ctx,
		`SELECT delivery_status, provider_message_id, COALESCE(delivery_detail,'') FROM messages WHERE id=$1`, sentMsgID,
	).Scan(&deliveryStatus, &providerID, &detail); err != nil {
		t.Fatalf("read sent row: %v", err)
	}
	if deliveryStatus != "sent" {
		t.Errorf("sent row delivery_status = %q, want sent", deliveryStatus)
	}
	if providerID != "<provider-sent>" {
		t.Errorf("sent row provider_message_id = %q, want unchanged", providerID)
	}
	if detail != "" {
		t.Errorf("sent row delivery_detail = %q, want unchanged empty detail", detail)
	}
}

func TestMarkOutboundSentTxSkipsTrashRace(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "async-sent-race")

	msg, err := store.CreateOutboundMessage(ctx, agentID,
		[]string{"x@example.com"}, nil, nil, "queued", "send", "smtp", "", "", []byte("raw"))
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}
	if p, err := store.LoadOutboundForSend(ctx, msg.ID); err != nil || p == nil {
		t.Fatalf("LoadOutboundForSend(live) = (%v, %v), want payload", p, err)
	}
	var wantDeliveryStatus, wantProviderID string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(delivery_status,''), COALESCE(provider_message_id,'') FROM messages WHERE id=$1`, msg.ID,
	).Scan(&wantDeliveryStatus, &wantProviderID); err != nil {
		t.Fatalf("read baseline row: %v", err)
	}

	t.Run("trashed message", func(t *testing.T) {
		if err := store.SoftDeleteMessage(ctx, msg.ID, agentID); err != nil {
			t.Fatalf("SoftDeleteMessage: %v", err)
		}
		var info *identity.OutboundSentInfo
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			i, err := store.MarkOutboundSentTx(ctx, tx, msg.ID, "<ses-race@amazonses.com>")
			info = i
			return err
		}); err != nil {
			t.Fatalf("MarkOutboundSentTx: %v", err)
		}
		if info != nil {
			t.Fatalf("MarkOutboundSentTx info = %+v, want nil for trashed message", info)
		}
		var deliveryStatus, providerID string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(delivery_status,''), COALESCE(provider_message_id,'') FROM messages WHERE id=$1`, msg.ID,
		).Scan(&deliveryStatus, &providerID); err != nil {
			t.Fatalf("read row: %v", err)
		}
		if deliveryStatus != wantDeliveryStatus || providerID != wantProviderID {
			t.Fatalf("after trashed-message race: delivery_status=%q provider_message_id=%q, want %q/%q", deliveryStatus, providerID, wantDeliveryStatus, wantProviderID)
		}
	})

	if _, err := store.RestoreMessage(ctx, msg.ID, agentID); err != nil {
		t.Fatalf("RestoreMessage: %v", err)
	}
	t.Run("trashed agent", func(t *testing.T) {
		if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
			t.Fatalf("SoftDeleteAgent: %v", err)
		}
		var info *identity.OutboundSentInfo
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			i, err := store.MarkOutboundSentTx(ctx, tx, msg.ID, "<ses-agent-race@amazonses.com>")
			info = i
			return err
		}); err != nil {
			t.Fatalf("MarkOutboundSentTx: %v", err)
		}
		if info != nil {
			t.Fatalf("MarkOutboundSentTx info = %+v, want nil for trashed agent", info)
		}
		var deliveryStatus, providerID string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(delivery_status,''), COALESCE(provider_message_id,'') FROM messages WHERE id=$1`, msg.ID,
		).Scan(&deliveryStatus, &providerID); err != nil {
			t.Fatalf("read row: %v", err)
		}
		if deliveryStatus != wantDeliveryStatus || providerID != wantProviderID {
			t.Fatalf("after trashed-agent race: delivery_status=%q provider_message_id=%q, want %q/%q", deliveryStatus, providerID, wantDeliveryStatus, wantProviderID)
		}
	})
}

func TestMarkOutboundFailedTxSkipsTrashRace(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "async-failed-race")

	msg, err := store.CreateOutboundMessage(ctx, agentID,
		[]string{"x@example.com"}, nil, nil, "queued", "send", "smtp", "", "", []byte("raw"))
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}
	if p, err := store.LoadOutboundForSend(ctx, msg.ID); err != nil || p == nil {
		t.Fatalf("LoadOutboundForSend(live) = (%v, %v), want payload", p, err)
	}
	var wantDeliveryStatus, wantDetail string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(delivery_status,''), COALESCE(delivery_detail,'') FROM messages WHERE id=$1`, msg.ID,
	).Scan(&wantDeliveryStatus, &wantDetail); err != nil {
		t.Fatalf("read baseline row: %v", err)
	}

	t.Run("trashed message", func(t *testing.T) {
		if err := store.SoftDeleteMessage(ctx, msg.ID, agentID); err != nil {
			t.Fatalf("SoftDeleteMessage: %v", err)
		}
		var info *identity.OutboundSentInfo
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			i, err := store.MarkOutboundFailedTx(ctx, tx, msg.ID, "550 after trash", delivery.FailureSourceProvider)
			info = i
			return err
		}); err != nil {
			t.Fatalf("MarkOutboundFailedTx: %v", err)
		}
		if info != nil {
			t.Fatalf("MarkOutboundFailedTx info = %+v, want nil for trashed message", info)
		}
		var deliveryStatus, detail string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(delivery_status,''), COALESCE(delivery_detail,'') FROM messages WHERE id=$1`, msg.ID,
		).Scan(&deliveryStatus, &detail); err != nil {
			t.Fatalf("read row: %v", err)
		}
		if deliveryStatus != wantDeliveryStatus || detail != wantDetail {
			t.Fatalf("after trashed-message race: delivery_status=%q detail=%q, want %q/%q", deliveryStatus, detail, wantDeliveryStatus, wantDetail)
		}
	})

	if _, err := store.RestoreMessage(ctx, msg.ID, agentID); err != nil {
		t.Fatalf("RestoreMessage: %v", err)
	}
	t.Run("trashed agent", func(t *testing.T) {
		if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
			t.Fatalf("SoftDeleteAgent: %v", err)
		}
		var info *identity.OutboundSentInfo
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			i, err := store.MarkOutboundFailedTx(ctx, tx, msg.ID, "550 after agent trash", delivery.FailureSourceProvider)
			info = i
			return err
		}); err != nil {
			t.Fatalf("MarkOutboundFailedTx: %v", err)
		}
		if info != nil {
			t.Fatalf("MarkOutboundFailedTx info = %+v, want nil for trashed agent", info)
		}
		var deliveryStatus, detail string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(delivery_status,''), COALESCE(delivery_detail,'') FROM messages WHERE id=$1`, msg.ID,
		).Scan(&deliveryStatus, &detail); err != nil {
			t.Fatalf("read row: %v", err)
		}
		if deliveryStatus != wantDeliveryStatus || detail != wantDetail {
			t.Fatalf("after trashed-agent race: delivery_status=%q detail=%q, want %q/%q", deliveryStatus, detail, wantDeliveryStatus, wantDetail)
		}
	})
}
