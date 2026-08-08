package identity_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

type recordingOutboundJobCanceller struct {
	jobIDs []int64
	err    error
}

func (c *recordingOutboundJobCanceller) CancelTx(_ context.Context, _ pgx.Tx, jobID int64) error {
	c.jobIDs = append(c.jobIDs, jobID)
	return c.err
}

func linkTrashTestSendJob(t *testing.T, pool *pgxpool.Pool, messageID string, jobID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE messages SET send_job_id = $2, delivery_status = 'accepted' WHERE id = $1`,
		messageID, jobID); err != nil {
		t.Fatalf("link send job: %v", err)
	}
}

func wireTrashScheduledFinalizer(store *identity.Store, pool *pgxpool.Pool) {
	store.SetScheduledSendFinalizer(agent.NewOutboundSendStore(
		store,
		webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)),
		usage.NewNoopUsageTracker(),
	))
}

// trashTestSetup creates a user + verified domain + agent for trash tests.
func trashTestSetup(t *testing.T, store *identity.Store, slug string) (userID, agentID string) {
	t.Helper()
	ctx := context.Background()
	domain := slug + ".example.com"
	user, err := store.CreateOrGetUser(ctx, slug+"-owner@example.com", "Owner", "google-"+slug)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	a, err := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return user.ID, a.ID
}

func trashInbound(t *testing.T, store *identity.Store, agentID, recipient, subject string) *identity.Message {
	t.Helper()
	m, err := store.CreateInboundMessage(context.Background(), "", agentID,
		"alice@gmail.com", recipient, fmt.Sprintf("<%s@gmail.com>", identity.NewMessageID()),
		subject, "", "", []byte("raw"), nil, nil, false, "", nil, nil, nil,
		identity.InboundScreening{})
	if err != nil {
		t.Fatalf("CreateInboundMessage: %v", err)
	}
	return m
}

func trashOutbound(t *testing.T, store *identity.Store, agentID, subject string) *identity.Message {
	t.Helper()
	m, err := store.CreateOutboundMessage(context.Background(), agentID,
		[]string{"recipient@example.com"}, nil, nil, subject, "send", "smtp", "", "", []byte("raw"))
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}
	return m
}

// listIDs runs GetMessagesByAgent with the given trash flag and returns ids.
func listIDs(t *testing.T, store *identity.Store, agentID string, deleted bool) map[string]*identity.Message {
	t.Helper()
	msgs, err := store.GetMessagesByAgent(context.Background(), identity.MessageListFilter{
		AgentID: agentID, Direction: "all", Status: "all",
		Descending: true, Limit: 100, Deleted: deleted,
	})
	if err != nil {
		t.Fatalf("GetMessagesByAgent(deleted=%v): %v", deleted, err)
	}
	out := map[string]*identity.Message{}
	for i := range msgs {
		out[msgs[i].ID] = &msgs[i]
	}
	return out
}

func TestMessageTrashLifecycle(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	_, agentID := trashTestSetup(t, store, "msg-trash")

	kept := trashInbound(t, store, agentID, "bot@msg-trash.example.com", "kept")
	doomed := trashInbound(t, store, agentID, "bot@msg-trash.example.com", "doomed")

	// Trash `doomed`.
	if err := store.SoftDeleteMessage(ctx, doomed.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	// Idempotent on an already-trashed message.
	if err := store.SoftDeleteMessage(ctx, doomed.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage (repeat): %v", err)
	}

	// Live list excludes it; trash list carries it with DeletedAt set.
	live := listIDs(t, store, agentID, false)
	if _, ok := live[doomed.ID]; ok {
		t.Error("trashed message still in live list")
	}
	if _, ok := live[kept.ID]; !ok {
		t.Error("live message missing from live list")
	}
	trash := listIDs(t, store, agentID, true)
	tm, ok := trash[doomed.ID]
	if !ok {
		t.Fatal("trashed message missing from trash list")
	}
	if tm.DeletedAt == nil {
		t.Error("trash list row has nil DeletedAt")
	}
	if _, ok := trash[kept.ID]; ok {
		t.Error("live message leaked into trash list")
	}

	// Single-message get still opens it (trash detail view), annotated.
	got, err := store.GetMessageWithContent(ctx, doomed.ID, agentID)
	if err != nil {
		t.Fatalf("GetMessageWithContent(trashed): %v", err)
	}
	if got.DeletedAt == nil {
		t.Error("GetMessageWithContent(trashed): DeletedAt is nil")
	}

	// Reply/threading anchors treat it as gone.
	if _, err := store.GetInboundMessage(ctx, doomed.ID); err == nil {
		t.Error("GetInboundMessage returned a trashed message")
	}
	if _, err := store.GetRepliableMessage(ctx, doomed.ID); err == nil {
		t.Error("GetRepliableMessage returned a trashed message")
	}

	// Restore: back in the live list, gone from trash, DeletedAt cleared.
	if _, err := store.RestoreMessage(ctx, doomed.ID, agentID); err != nil {
		t.Fatalf("RestoreMessage: %v", err)
	}
	if _, ok := listIDs(t, store, agentID, false)[doomed.ID]; !ok {
		t.Error("restored message missing from live list")
	}
	if _, ok := listIDs(t, store, agentID, true)[doomed.ID]; ok {
		t.Error("restored message still in trash list")
	}

	// Restore/purge on a live message → ErrNotInTrash.
	if _, err := store.RestoreMessage(ctx, doomed.ID, agentID); !errors.Is(err, identity.ErrNotInTrash) {
		t.Errorf("RestoreMessage(live) = %v, want ErrNotInTrash", err)
	}
	if err := store.PurgeMessage(ctx, doomed.ID, agentID); !errors.Is(err, identity.ErrNotInTrash) {
		t.Errorf("PurgeMessage(live) = %v, want ErrNotInTrash", err)
	}
	// Missing message → ErrMessageNotFound.
	if _, err := store.RestoreMessage(ctx, "msg_nope", agentID); !errors.Is(err, identity.ErrMessageNotFound) {
		t.Errorf("RestoreMessage(missing) = %v, want ErrMessageNotFound", err)
	}
	if err := store.SoftDeleteMessage(ctx, "msg_nope", agentID); !errors.Is(err, identity.ErrMessageNotFound) {
		t.Errorf("SoftDeleteMessage(missing) = %v, want ErrMessageNotFound", err)
	}

	// Delete forever: trash then purge; the row is gone for good.
	if err := store.SoftDeleteMessage(ctx, doomed.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage (again): %v", err)
	}
	if err := store.PurgeMessage(ctx, doomed.ID, agentID); err != nil {
		t.Fatalf("PurgeMessage: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE id = $1`, doomed.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("purged message still present (n=%d, err=%v)", n, err)
	}
}

func TestPurgeMessageCancelsLinkedSendJobTransactionally(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	canceller := &recordingOutboundJobCanceller{}
	store.SetOutboundJobCanceller(canceller)
	ctx := context.Background()
	_, agentID := trashTestSetup(t, store, "purge-job")

	message := trashOutbound(t, store, agentID, "scheduled")
	linkTrashTestSendJob(t, pool, message.ID, 101)

	if err := store.SoftDeleteMessage(ctx, message.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	if len(canceller.jobIDs) != 0 {
		t.Fatalf("soft delete cancelled jobs %v; scheduled jobs must survive for restore", canceller.jobIDs)
	}

	canceller.err = errors.New("queue unavailable")
	if err := store.PurgeMessage(ctx, message.ID, agentID); !errors.Is(err, canceller.err) {
		t.Fatalf("PurgeMessage cancellation failure = %v, want %v", err, canceller.err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`, message.ID).Scan(&exists); err != nil {
		t.Fatalf("check rollback: %v", err)
	}
	if !exists {
		t.Fatal("message was deleted even though linked job cancellation failed")
	}

	canceller.err = nil
	if err := store.PurgeMessage(ctx, message.ID, agentID); err != nil {
		t.Fatalf("PurgeMessage: %v", err)
	}
	if got := canceller.jobIDs; len(got) != 2 || got[0] != 101 || got[1] != 101 {
		t.Fatalf("cancelled jobs = %v, want [101 101] (rolled-back attempt plus committed retry)", got)
	}
}

func TestPermanentAgentDeletionCancelsLinkedSendJobs(t *testing.T) {
	t.Run("explicit delete", func(t *testing.T) {
		pool := testutil.TestDB(t)
		store := identity.NewStore(pool)
		canceller := &recordingOutboundJobCanceller{}
		store.SetOutboundJobCanceller(canceller)
		ctx := context.Background()
		userID, agentID := trashTestSetup(t, store, "delete-agent-job")
		message := trashOutbound(t, store, agentID, "scheduled")
		historical := trashOutbound(t, store, agentID, "already sent")
		linkTrashTestSendJob(t, pool, message.ID, 201)
		linkTrashTestSendJob(t, pool, historical.ID, 299)
		if _, err := pool.Exec(ctx,
			`UPDATE messages SET delivery_status = 'sent' WHERE id = $1`,
			historical.ID); err != nil {
			t.Fatalf("mark historical message terminal: %v", err)
		}

		if _, err := store.DeleteAgent(ctx, agentID, userID); err != nil {
			t.Fatalf("DeleteAgent: %v", err)
		}
		if got := canceller.jobIDs; len(got) != 1 || got[0] != 201 {
			t.Fatalf("cancelled jobs = %v, want [201]", got)
		}
	})

	t.Run("trash janitor", func(t *testing.T) {
		pool := testutil.TestDB(t)
		store := identity.NewStore(pool)
		canceller := &recordingOutboundJobCanceller{}
		store.SetOutboundJobCanceller(canceller)
		ctx := context.Background()
		userID, agentID := trashTestSetup(t, store, "purge-agent-job")
		message := trashOutbound(t, store, agentID, "scheduled")
		linkTrashTestSendJob(t, pool, message.ID, 202)

		if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
			t.Fatalf("SoftDeleteAgent: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE agent_identities SET deleted_at = now() - interval '31 days' WHERE id = $1`,
			agentID); err != nil {
			t.Fatalf("backdate agent: %v", err)
		}
		if _, err := store.PurgeDeletedAgents(ctx); err != nil {
			t.Fatalf("PurgeDeletedAgents: %v", err)
		}
		if got := canceller.jobIDs; len(got) != 1 || got[0] != 202 {
			t.Fatalf("cancelled jobs = %v, want [202]", got)
		}
	})
}

func TestDeleteExpiredMessagesCancelsLinkedSendJobs(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	canceller := &recordingOutboundJobCanceller{}
	store.SetOutboundJobCanceller(canceller)
	ctx := context.Background()
	_, agentID := trashTestSetup(t, store, "expired-job")
	message := trashOutbound(t, store, agentID, "scheduled")
	linkTrashTestSendJob(t, pool, message.ID, 301)
	if err := store.SoftDeleteMessage(ctx, message.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET deleted_at = now() - interval '31 days' WHERE id = $1`,
		message.ID); err != nil {
		t.Fatalf("backdate message: %v", err)
	}

	if _, err := store.DeleteExpiredMessages(ctx); err != nil {
		t.Fatalf("DeleteExpiredMessages: %v", err)
	}
	if got := canceller.jobIDs; len(got) != 1 || got[0] != 301 {
		t.Fatalf("cancelled jobs = %v, want [301]", got)
	}
}

func TestRestoreMessageCancelsPastDueScheduledSend(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	canceller := &recordingOutboundJobCanceller{}
	store.SetOutboundJobCanceller(canceller)
	wireTrashScheduledFinalizer(store, pool)
	ctx := context.Background()
	_, agentID := trashTestSetup(t, store, "restore-schedule")
	message := trashOutbound(t, store, agentID, "scheduled")
	linkTrashTestSendJob(t, pool, message.ID, 501)
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET scheduled_at = now() + interval '1 hour' WHERE id = $1`,
		message.ID); err != nil {
		t.Fatalf("set future schedule: %v", err)
	}

	if err := store.SoftDeleteMessage(ctx, message.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage(future): %v", err)
	}
	if _, err := store.RestoreMessage(ctx, message.ID, agentID); err != nil {
		t.Fatalf("RestoreMessage(future): %v", err)
	}
	if len(canceller.jobIDs) != 0 {
		t.Fatalf("future restore cancelled jobs %v", canceller.jobIDs)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE messages SET scheduled_at = now() - interval '1 second' WHERE id = $1`,
		message.ID); err != nil {
		t.Fatalf("set past schedule: %v", err)
	}
	if err := store.SoftDeleteMessage(ctx, message.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage(past): %v", err)
	}
	if _, err := store.RestoreMessage(ctx, message.ID, agentID); err != nil {
		t.Fatalf("RestoreMessage(past): %v", err)
	}
	if got := canceller.jobIDs; len(got) != 1 || got[0] != 501 {
		t.Fatalf("cancelled jobs = %v, want [501]", got)
	}
	var deletedAt *time.Time
	var deliveryStatus, reason string
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at, delivery_status, COALESCE(delivery_failure_reason_code, '')
		   FROM messages WHERE id = $1`,
		message.ID).Scan(&deletedAt, &deliveryStatus, &reason); err != nil {
		t.Fatalf("read restored message: %v", err)
	}
	if deletedAt != nil || deliveryStatus != "failed" || reason != "submission.cancelled" {
		t.Fatalf("restored message = deleted_at %v status %q reason %q, want live/failed/submission.cancelled",
			deletedAt, deliveryStatus, reason)
	}
	var failedEvents int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_events WHERE message_id = $1 AND type = 'email.failed'`,
		message.ID).Scan(&failedEvents); err != nil {
		t.Fatalf("count email.failed events: %v", err)
	}
	if failedEvents != 1 {
		t.Fatalf("email.failed events = %d, want 1", failedEvents)
	}
}

func TestRestoreMessageUsesCutoffAfterWaitingForRowLock(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	canceller := &recordingOutboundJobCanceller{}
	store.SetOutboundJobCanceller(canceller)
	wireTrashScheduledFinalizer(store, pool)
	ctx := context.Background()
	_, agentID := trashTestSetup(t, store, "restore-cutoff-lock")
	message := trashOutbound(t, store, agentID, "scheduled across lock")
	linkTrashTestSendJob(t, pool, message.ID, 504)
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET scheduled_at = clock_timestamp() + interval '1 second' WHERE id = $1`,
		message.ID); err != nil {
		t.Fatalf("set schedule: %v", err)
	}
	if err := store.SoftDeleteMessage(ctx, message.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck
	if _, err := blocker.Exec(ctx, `SELECT id FROM messages WHERE id = $1 FOR UPDATE`, message.ID); err != nil {
		t.Fatalf("lock message: %v", err)
	}

	restoreDone := make(chan error, 1)
	go func() {
		_, err := store.RestoreMessage(ctx, message.ID, agentID)
		restoreDone <- err
	}()
	time.Sleep(1100 * time.Millisecond)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release message lock: %v", err)
	}
	if err := <-restoreDone; err != nil {
		t.Fatalf("RestoreMessage: %v", err)
	}
	if got := canceller.jobIDs; len(got) != 1 || got[0] != 504 {
		t.Fatalf("cancelled jobs = %v, want [504]", got)
	}
}

func TestRestoreMessagePreservesProviderAcceptEvidence(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	canceller := &recordingOutboundJobCanceller{}
	store.SetOutboundJobCanceller(canceller)
	wireTrashScheduledFinalizer(store, pool)
	ctx := context.Background()
	_, agentID := trashTestSetup(t, store, "restore-provider-evidence")
	message := trashOutbound(t, store, agentID, "provider accepted")
	linkTrashTestSendJob(t, pool, message.ID, 505)
	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET scheduled_at = clock_timestamp() - interval '1 second',
		        provider_accepted_at = clock_timestamp(),
		        provider_message_id = '<accepted@example.com>'
		  WHERE id = $1`,
		message.ID); err != nil {
		t.Fatalf("set provider evidence: %v", err)
	}
	if err := store.SoftDeleteMessage(ctx, message.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	if _, err := store.RestoreMessage(ctx, message.ID, agentID); err != nil {
		t.Fatalf("RestoreMessage: %v", err)
	}

	var status string
	var sentEvents, failedEvents int
	if err := pool.QueryRow(ctx,
		`SELECT delivery_status,
		        (SELECT count(*) FROM webhook_events WHERE message_id = $1 AND type = 'email.sent'),
		        (SELECT count(*) FROM webhook_events WHERE message_id = $1 AND type = 'email.failed')
		   FROM messages WHERE id = $1`,
		message.ID).Scan(&status, &sentEvents, &failedEvents); err != nil {
		t.Fatalf("read restored message: %v", err)
	}
	if status != "sent" || sentEvents != 1 || failedEvents != 0 {
		t.Fatalf("provider evidence settled as status=%q sent_events=%d failed_events=%d; want sent/1/0",
			status, sentEvents, failedEvents)
	}
}

func TestRestoreAgentCancelsOnlyPastDueScheduledSends(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	canceller := &recordingOutboundJobCanceller{}
	store.SetOutboundJobCanceller(canceller)
	wireTrashScheduledFinalizer(store, pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "restore-agent-schedule")
	past := trashOutbound(t, store, agentID, "past scheduled")
	future := trashOutbound(t, store, agentID, "future scheduled")
	linkTrashTestSendJob(t, pool, past.ID, 502)
	linkTrashTestSendJob(t, pool, future.ID, 503)
	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET scheduled_at = CASE id
		          WHEN $1 THEN now() - interval '1 second'
		          ELSE now() + interval '1 hour'
		        END
		  WHERE id = ANY($2)`,
		past.ID, []string{past.ID, future.ID}); err != nil {
		t.Fatalf("set schedules: %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}
	if _, err := store.RestoreAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("RestoreAgent: %v", err)
	}
	if got := canceller.jobIDs; len(got) != 1 || got[0] != 502 {
		t.Fatalf("cancelled jobs = %v, want [502]", got)
	}
	var pastStatus, futureStatus string
	if err := pool.QueryRow(ctx, `SELECT delivery_status FROM messages WHERE id = $1`, past.ID).Scan(&pastStatus); err != nil {
		t.Fatalf("read past status: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT delivery_status FROM messages WHERE id = $1`, future.ID).Scan(&futureStatus); err != nil {
		t.Fatalf("read future status: %v", err)
	}
	if pastStatus != "failed" || futureStatus != "accepted" {
		t.Fatalf("statuses = past %q future %q, want failed/accepted", pastStatus, futureStatus)
	}
}

// TestSoftDeleteMessageHeldGuard: a pending_review message (either direction)
// cannot be trashed — the review queue is its resolution surface.
func TestSoftDeleteMessageHeldGuard(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	_, agentID := trashTestSetup(t, store, "held-trash")

	held, err := store.CreatePendingOutboundMessage(ctx, agentID,
		[]string{"x@example.com"}, nil, nil, "held", "body", "", nil,
		"send", "", "", "", 3600)
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}
	if err := store.SoftDeleteMessage(ctx, held.ID, agentID); !errors.Is(err, identity.ErrMessageHeld) {
		t.Errorf("SoftDeleteMessage(held) = %v, want ErrMessageHeld", err)
	}
	_ = pool // pool used via store
}

// TestRestoreMessageKeepsIndefiniteRetention: restoring a message clears the
// trash marker without introducing a live-message expiry.
func TestRestoreMessageKeepsIndefiniteRetention(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	_, agentID := trashTestSetup(t, store, "shift-trash")

	m := trashInbound(t, store, agentID, "bot@shift-trash.example.com", "shift")
	if err := store.SoftDeleteMessage(ctx, m.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	// Simulate 20 days in the trash, still within the default purge window.
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET deleted_at = deleted_at - interval '20 days',
		                     created_at = created_at - interval '20 days'
		  WHERE id = $1`, m.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := store.RestoreMessage(ctx, m.ID, agentID); err != nil {
		t.Fatalf("RestoreMessage: %v", err)
	}
	var expires *time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM messages WHERE id = $1`, m.ID).Scan(&expires); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if expires != nil {
		t.Errorf("post-restore expires_at = %v, want NULL", expires)
	}
	// And it is visible again.
	if _, ok := listIDs(t, store, agentID, false)[m.ID]; !ok {
		t.Error("restored message missing from live list")
	}
}

// TestDeleteExpiredMessagesTrashArms: the janitor deletes only trashed rows
// past TrashRetention. Legacy live expiry timestamps are ignored, and a
// trashed row inside its retention window remains available for restore.
func TestDeleteExpiredMessagesTrashArms(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	_, agentID := trashTestSetup(t, store, "janitor-trash")

	expired := trashInbound(t, store, agentID, "bot@janitor-trash.example.com", "expired-live")
	freshTrash := trashInbound(t, store, agentID, "bot@janitor-trash.example.com", "fresh-trash")
	staleTrash := trashInbound(t, store, agentID, "bot@janitor-trash.example.com", "stale-trash")
	keeper := trashInbound(t, store, agentID, "bot@janitor-trash.example.com", "keeper")

	// Live row with a legacy past expiry → retained indefinitely.
	if _, err := pool.Exec(ctx, `UPDATE messages SET expires_at = now() - interval '1 hour' WHERE id = $1`, expired.ID); err != nil {
		t.Fatalf("backdate expired: %v", err)
	}
	// Trashed yesterday with a legacy expires_at in the past → kept.
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET deleted_at = now() - interval '1 day', expires_at = now() - interval '5 days' WHERE id = $1`,
		freshTrash.ID); err != nil {
		t.Fatalf("backdate freshTrash: %v", err)
	}
	// Trashed 31 days ago → past TrashRetention, purged.
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET deleted_at = now() - interval '31 days' WHERE id = $1`, staleTrash.ID); err != nil {
		t.Fatalf("backdate staleTrash: %v", err)
	}

	if _, err := store.DeleteExpiredMessages(ctx); err != nil {
		t.Fatalf("DeleteExpiredMessages: %v", err)
	}

	var got []string
	rows, err := pool.Query(ctx, `SELECT id FROM messages WHERE agent_id = $1`, agentID)
	if err != nil {
		t.Fatalf("query survivors: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	want := map[string]bool{expired.ID: true, freshTrash.ID: true, keeper.ID: true}
	if len(got) != 3 {
		t.Fatalf("survivors = %v, want three rows", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected survivor %s; survivors = %v", id, got)
		}
	}
}

func TestAgentTrashLifecycle(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "agent-trash")
	msg := trashInbound(t, store, agentID, "bot@agent-trash.example.com", "inbox mail")

	if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}
	// Repeat soft delete: already trashed → error (not found among live).
	if err := store.SoftDeleteAgent(ctx, agentID, userID); err == nil {
		t.Error("SoftDeleteAgent(already trashed): expected error")
	}

	// Live lookups treat it as nonexistent (inbound relay + API resolution).
	if _, err := store.GetAgentByID(ctx, agentID); err == nil {
		t.Error("GetAgentByID returned a trashed agent")
	}
	if _, err := store.GetAgentByEmail(ctx, agentID); err == nil {
		t.Error("GetAgentByEmail returned a trashed agent")
	}
	// Any-state lookup finds it, annotated.
	anyState, err := store.GetAgentByIDAnyState(ctx, agentID)
	if err != nil {
		t.Fatalf("GetAgentByIDAnyState: %v", err)
	}
	if anyState.DeletedAt == nil {
		t.Error("GetAgentByIDAnyState: DeletedAt is nil for a trashed agent")
	}

	// Live list excludes; trash list includes.
	liveAgents, err := store.ListAgentsByUser(ctx, userID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAgentsByUser: %v", err)
	}
	for _, a := range liveAgents {
		if a.ID == agentID {
			t.Error("trashed agent still in live list")
		}
	}
	trashed, err := store.ListDeletedAgentsByUser(ctx, userID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListDeletedAgentsByUser: %v", err)
	}
	if len(trashed) != 1 || trashed[0].ID != agentID {
		t.Fatalf("trash list = %+v, want exactly the trashed agent", trashed)
	}
	if trashed[0].DeletedAt == nil {
		t.Error("trash list row has nil DeletedAt")
	}

	// Restore: back everywhere, messages intact.
	if _, err := store.RestoreAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("RestoreAgent: %v", err)
	}
	if _, err := store.GetAgentByID(ctx, agentID); err != nil {
		t.Errorf("GetAgentByID after restore: %v", err)
	}
	if _, ok := listIDs(t, store, agentID, false)[msg.ID]; !ok {
		t.Error("agent's message missing after restore")
	}
	// Restore on a live agent → ErrNotInTrash.
	if _, err := store.RestoreAgent(ctx, agentID, userID); !errors.Is(err, identity.ErrNotInTrash) {
		t.Errorf("RestoreAgent(live) = %v, want ErrNotInTrash", err)
	}

	// Wrong owner can neither trash nor restore.
	otherUser, err := store.CreateOrGetUser(ctx, "intruder@example.com", "X", "google-intruder-trash")
	if err != nil {
		t.Fatalf("CreateOrGetUser(intruder): %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, agentID, otherUser.ID); err == nil {
		t.Error("SoftDeleteAgent by non-owner succeeded")
	}
}

// TestAgentTrashPausesHoldClock: live message retention remains indefinite
// while an inbox is in trash. RestoreAgent shifts only a held draft's
// approval_expires_at so review time does not elapse in trash.
func TestAgentTrashPausesMessageClocks(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "pause-trash")

	msg := trashInbound(t, store, agentID, "bot@pause-trash.example.com", "keep me")
	held, err := store.CreatePendingOutboundMessage(ctx, agentID,
		[]string{"x@example.com"}, nil, nil, "held", "body", "", nil,
		"send", "", "", "", 3600)
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}

	if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}
	// Simulate 20 days in the trash and 20 days of elapsed hold time.
	if _, err := pool.Exec(ctx,
		`UPDATE agent_identities SET deleted_at = deleted_at - interval '20 days' WHERE id = $1`, agentID); err != nil {
		t.Fatalf("backdate agent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET created_at = created_at - interval '20 days',
		                     approval_expires_at = approval_expires_at - interval '20 days'
		  WHERE agent_id = $1`, agentID); err != nil {
		t.Fatalf("backdate messages: %v", err)
	}

	// The message-level trash janitor must not touch an agent's messages; the
	// agent purge owns that deletion after the agent trash window elapses.
	if _, err := store.DeleteExpiredMessages(ctx); err != nil {
		t.Fatalf("DeleteExpiredMessages: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE agent_id = $1`, agentID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("messages of trashed agent survived = %d (err=%v), want 2", n, err)
	}

	if _, err := store.RestoreAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("RestoreAgent: %v", err)
	}
	// The inbound message remains indefinitely retained.
	var expires *time.Time
	var approval time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM messages WHERE id = $1`, msg.ID).Scan(&expires); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if expires != nil {
		t.Errorf("restored message expires_at = %v, want NULL", expires)
	}
	// The held draft resumes with ~1h of review window left — NOT already
	// lapsed (which would let the TTL sweep auto-resolve it immediately).
	if err := pool.QueryRow(ctx, `SELECT approval_expires_at FROM messages WHERE id = $1`, held.ID).Scan(&approval); err != nil {
		t.Fatalf("read approval_expires_at: %v", err)
	}
	if left := time.Until(approval); left < 30*time.Minute || left > 90*time.Minute {
		t.Errorf("restored hold review window = %v, want ~1h", left)
	}
	// And the restored hold is back in the review surfaces.
	pending, err := store.ListPendingOutboundForUser(ctx, userID, 100)
	if err != nil {
		t.Fatalf("ListPendingOutboundForUser: %v", err)
	}
	found := false
	for _, p := range pending {
		if p.ID == held.ID {
			found = true
		}
	}
	if !found {
		t.Error("restored agent's hold missing from pending list")
	}
}

// TestTrashedAgentHoldsCannotBeResolved: the hold-resolution paths treat a
// trashed agent's held draft as nonexistent — no approve, no reject-scrub.
func TestTrashedAgentHoldsCannotBeResolved(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "resolve-trash")

	held, err := store.CreatePendingOutboundMessage(ctx, agentID,
		[]string{"x@example.com"}, nil, nil, "held", "body", "", nil,
		"send", "", "", "", 3600)
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	if got, err := store.GetOutboundMessageForUser(ctx, held.ID, userID); err == nil && got != nil {
		t.Error("GetOutboundMessageForUser resolved a trashed agent's hold")
	}
	if _, _, err := store.ResolveOutboundOwner(ctx, held.ID); err == nil {
		t.Error("ResolveOutboundOwner resolved a trashed agent's hold")
	}
	if _, err := store.RejectPending(ctx, held.ID, userID, "nope"); err == nil {
		t.Error("RejectPending scrubbed a trashed agent's hold")
	}
	// The draft body is intact for restore.
	var bodyText *string
	if err := pool.QueryRow(ctx, `SELECT body_text FROM messages WHERE id = $1`, held.ID).Scan(&bodyText); err != nil {
		t.Fatalf("read body_text: %v", err)
	}
	if bodyText == nil || *bodyText != "body" {
		t.Errorf("held draft body was scrubbed while agent trashed: %v", bodyText)
	}
}

// TestLoadOutboundForSendSkipsTrash: the async send worker's load treats a
// trashed message — or a message of a trashed agent — as gone (nil, nil), so
// deleting is an effective "stop this queued send" lever.
func TestLoadOutboundForSendSkipsTrash(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "send-trash")

	msg, err := store.CreateOutboundMessage(ctx, agentID,
		[]string{"x@example.com"}, nil, nil, "queued send", "send", "smtp", "", "", []byte("raw"))
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}
	// Live: the worker sees the payload.
	if p, err := store.LoadOutboundForSend(ctx, msg.ID); err != nil || p == nil {
		t.Fatalf("LoadOutboundForSend(live) = (%v, %v), want payload", p, err)
	}
	// Message in the trash → gone.
	if err := store.SoftDeleteMessage(ctx, msg.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	if p, err := store.LoadOutboundForSend(ctx, msg.ID); err != nil || p != nil {
		t.Fatalf("LoadOutboundForSend(trashed msg) = (%v, %v), want (nil, nil)", p, err)
	}
	// Restore the message, trash the whole AGENT → also gone.
	if _, err := store.RestoreMessage(ctx, msg.ID, agentID); err != nil {
		t.Fatalf("RestoreMessage: %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}
	if p, err := store.LoadOutboundForSend(ctx, msg.ID); err != nil || p != nil {
		t.Fatalf("LoadOutboundForSend(trashed agent) = (%v, %v), want (nil, nil)", p, err)
	}
}

// TestPurgeDeletedAgents: only trashed agents past TrashRetention are purged,
// and the purge cascades to their messages.
func TestPurgeDeletedAgents(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, staleAgent := trashTestSetup(t, store, "purge-stale")
	_, freshAgent := trashTestSetup(t, store, "purge-fresh")
	staleMsg := trashInbound(t, store, staleAgent, "bot@purge-stale.example.com", "doomed with agent")

	for _, id := range []string{staleAgent, freshAgent} {
		uid := userID
		if id == freshAgent {
			// freshAgent belongs to its own setup user; resolve it.
			a, err := store.GetAgentByID(ctx, id)
			if err != nil {
				t.Fatalf("GetAgentByID(%s): %v", id, err)
			}
			uid = a.UserID
		}
		if err := store.SoftDeleteAgent(ctx, id, uid); err != nil {
			t.Fatalf("SoftDeleteAgent(%s): %v", id, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`UPDATE agent_identities SET deleted_at = now() - interval '31 days' WHERE id = $1`, staleAgent); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := store.PurgeDeletedAgents(ctx)
	if err != nil {
		t.Fatalf("PurgeDeletedAgents: %v", err)
	}
	if n < 1 {
		t.Errorf("PurgeDeletedAgents purged %d rows, want >= 1", n)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_identities WHERE id = $1`, staleAgent).Scan(&count); err != nil || count != 0 {
		t.Errorf("stale agent survived purge (count=%d, err=%v)", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE id = $1`, staleMsg.ID).Scan(&count); err != nil || count != 0 {
		t.Errorf("stale agent's message survived purge (count=%d, err=%v)", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_identities WHERE id = $1`, freshAgent).Scan(&count); err != nil || count != 1 {
		t.Errorf("fresh trashed agent was purged early (count=%d, err=%v)", count, err)
	}
}

// TestTrashedAgentHoldsLeaveReviewSurfaces: a trashed agent's held messages
// disappear from the review queue, the pending list, the TTL sweep, and the
// dashboard pending count — nothing can be approved or auto-sent on behalf of
// a trashed inbox.
func TestTrashedAgentHoldsLeaveReviewSurfaces(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "held-agent-trash")

	held, err := store.CreatePendingOutboundMessage(ctx, agentID,
		[]string{"x@example.com"}, nil, nil, "held", "body", "", nil,
		"send", "", "", "", 60)
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}
	// Backdate the approval TTL so the hold qualifies for the expiry sweep.
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET approval_expires_at = now() - interval '10 minutes' WHERE id = $1`, held.ID); err != nil {
		t.Fatalf("backdate approval: %v", err)
	}

	if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	reviews, err := store.ListReviews(ctx, userID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	for _, r := range reviews {
		if r.ID == held.ID {
			t.Error("trashed agent's hold still in ListReviews")
		}
	}
	if _, err := store.GetReviewWithContent(ctx, userID, held.ID); err == nil {
		t.Error("GetReviewWithContent resolved a trashed agent's hold")
	}
	pending, err := store.ListPendingOutboundForUser(ctx, userID, 100)
	if err != nil {
		t.Fatalf("ListPendingOutboundForUser: %v", err)
	}
	for _, p := range pending {
		if p.ID == held.ID {
			t.Error("trashed agent's hold still in ListPendingOutboundForUser")
		}
	}
	expired, err := store.ListExpiredPending(ctx, 100)
	if err != nil {
		t.Fatalf("ListExpiredPending: %v", err)
	}
	for _, c := range expired {
		if c.MessageID == held.ID {
			t.Error("trashed agent's hold still in ListExpiredPending (TTL sweep would auto-send)")
		}
	}
}
