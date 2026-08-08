package identity_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func lifecycleReasons(t *testing.T, pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, messageID string) []messagelifecycle.ReasonCode {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT reason_code FROM message_lifecycle_transitions WHERE message_id=$1`, messageID)
	if err != nil {
		t.Fatalf("query lifecycle: %v", err)
	}
	defer rows.Close()
	var got []messagelifecycle.ReasonCode
	for rows.Next() {
		var reason messagelifecycle.ReasonCode
		if err := rows.Scan(&reason); err != nil {
			t.Fatalf("scan lifecycle: %v", err)
		}
		got = append(got, reason)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	return got
}

func installTask6ApprovalJobs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS task6_approval_jobs; CREATE TABLE task6_approval_jobs (id bigserial PRIMARY KEY, message_id text NOT NULL UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS task6_approval_jobs`) })
}

func enqueueTask6ApprovalJob(ctx context.Context, tx pgx.Tx, messageID string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO task6_approval_jobs (message_id) VALUES ($1) RETURNING id`, messageID).Scan(&id)
	return id, err
}

func TestApproveAndAcceptLifecycle(t *testing.T) {
	pool := testutil.TestDB(t)
	installTask6ApprovalJobs(t, pool)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, _ := store.CreateOrGetUser(ctx, "owner-lifecycle-aa@example.com", "Owner", "google-lifecycle-aa")
	domain := "lifecycle-aa.example.com"
	_, _ = store.ClaimOrCreateDomain(ctx, domain, user.ID)
	_ = store.VerifyDomain(ctx, domain, user.ID)
	ag, _ := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID)
	msg, err := store.CreatePendingOutboundMessage(ctx, ag.ID, []string{"a@example.net"}, nil, nil, "Subj", "body", "", nil, "send", "conv-aa-lifecycle", "", "", 3600)
	if err != nil {
		t.Fatalf("create hold: %v", err)
	}
	wantHeld := []messagelifecycle.ReasonCode{messagelifecycle.ReasonAcceptanceOutboundAPI, messagelifecycle.ReasonReviewHoldCreated}
	sort.Slice(wantHeld, func(i, j int) bool { return wantHeld[i] < wantHeld[j] })
	if got := lifecycleReasons(t, pool, msg.ID); !reflect.DeepEqual(got, wantHeld) {
		t.Fatalf("held lifecycle=%v want %v", got, wantHeld)
	}

	acc := identity.AcceptedSend{To: []string{"a@example.net"}, Subject: "Subj", Method: "smtp", EnvelopeFrom: ag.ID, SentAs: "own_address", Raw: []byte("raw")}
	var durableJobID int64
	_, err = store.ApproveAndAccept(ctx, msg.ID, user.ID, identity.MessageStatusReviewApproved, false, acc,
		func(ctx context.Context, tx pgx.Tx, messageID string) (int64, error) {
			id, err := enqueueTask6ApprovalJob(ctx, tx, messageID)
			durableJobID = id
			return id, err
		}, nil, nil)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	want := append(wantHeld, messagelifecycle.ReasonReviewApproved, messagelifecycle.ReasonQueueOutboundSubmission)
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if got := lifecycleReasons(t, pool, msg.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("approved lifecycle=%v want %v", got, want)
	}
	var jobID string
	if err := pool.QueryRow(ctx, `SELECT correlation_ids->>'job_id' FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='queue.outbound_submission'`, msg.ID).Scan(&jobID); err != nil {
		t.Fatalf("queue correlation: %v", err)
	}
	if jobID != fmt.Sprint(durableJobID) {
		t.Fatalf("queue job_id=%q want %d", jobID, durableJobID)
	}
	var durableMessageID string
	if err := pool.QueryRow(ctx, `SELECT message_id FROM task6_approval_jobs WHERE id=$1`, durableJobID).Scan(&durableMessageID); err != nil {
		t.Fatal(err)
	}
	if durableMessageID != msg.ID {
		t.Fatalf("durable approval job message=%s want %s", durableMessageID, msg.ID)
	}
	_, err = store.ApproveAndAccept(ctx, msg.ID, user.ID, identity.MessageStatusReviewApproved, false, acc,
		enqueueTask6ApprovalJob, nil, nil)
	if !errors.Is(err, identity.ErrNotPendingApproval) {
		t.Fatalf("duplicate approve=%v", err)
	}
	if got := lifecycleReasons(t, pool, msg.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("duplicate lifecycle=%v want %v", got, want)
	}
	var jobCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task6_approval_jobs`).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 1 {
		t.Fatalf("duplicate approval durable jobs=%d want 1", jobCount)
	}
}

func TestApproveAndAcceptRejectsUnsupportedTargetBeforeMutation(t *testing.T) {
	pool := testutil.TestDB(t)
	installTask6ApprovalJobs(t, pool)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, _ := store.CreateOrGetUser(ctx, "owner-invalid-target@example.com", "Owner", "google-invalid-target")
	domain := "invalid-target.example.com"
	_, _ = store.ClaimOrCreateDomain(ctx, domain, user.ID)
	_ = store.VerifyDomain(ctx, domain, user.ID)
	ag, _ := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID)
	msg, err := store.CreatePendingOutboundMessage(ctx, ag.ID, []string{"a@example.net"}, nil, nil, "invalid target", "body", "", nil, "send", "", "", "", 3600)
	if err != nil {
		t.Fatal(err)
	}
	acc := identity.AcceptedSend{To: []string{"a@example.net"}, Subject: "invalid target", Method: "smtp", SentAs: "relay", Raw: []byte("raw")}
	enqueued := 0
	_, err = store.ApproveAndAccept(ctx, msg.ID, user.ID, identity.MessageStatusReviewRejected, false, acc,
		func(ctx context.Context, tx pgx.Tx, messageID string) (int64, error) {
			enqueued++
			return enqueueTask6ApprovalJob(ctx, tx, messageID)
		}, nil, nil)
	if !errors.Is(err, identity.ErrInvalidApprovalTarget) || !strings.Contains(err.Error(), identity.MessageStatusReviewRejected) {
		t.Fatalf("ApproveAndAccept(invalid target) error = %v, want clear invalid-target error", err)
	}
	var status string
	var deliveryStatus *string
	var sendJobID *int64
	if err := pool.QueryRow(ctx, `SELECT status, delivery_status, send_job_id FROM messages WHERE id=$1`, msg.ID).Scan(&status, &deliveryStatus, &sendJobID); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusPendingReview || deliveryStatus != nil || sendJobID != nil || enqueued != 0 {
		t.Fatalf("invalid target mutated hold: status=%s delivery=%v send_job_id=%v enqueued=%d", status, deliveryStatus, sendJobID, enqueued)
	}
	want := []messagelifecycle.ReasonCode{messagelifecycle.ReasonAcceptanceOutboundAPI, messagelifecycle.ReasonReviewHoldCreated}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if got := lifecycleReasons(t, pool, msg.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("invalid target lifecycle=%v want %v", got, want)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task6_approval_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("invalid target durable jobs=%d want 0", jobs)
	}
}

func TestApproveAndAcceptHumanExpiryRaceHasOneAtomicWinner(t *testing.T) {
	pool := testutil.TestDB(t)
	installTask6ApprovalJobs(t, pool)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, _ := store.CreateOrGetUser(ctx, "owner-approval-race@example.com", "Owner", "google-approval-race")
	domain := "approval-race.example.com"
	_, _ = store.ClaimOrCreateDomain(ctx, domain, user.ID)
	_ = store.VerifyDomain(ctx, domain, user.ID)
	ag, _ := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID)
	msg, err := store.CreatePendingOutboundMessage(ctx, ag.ID, []string{"a@example.net"}, nil, nil, "approval race", "body", "", nil, "send", "", "", "", 3600)
	if err != nil {
		t.Fatal(err)
	}
	acc := identity.AcceptedSend{To: []string{"a@example.net"}, Subject: "approval race", Method: "smtp", SentAs: "relay", Raw: []byte("raw")}
	start := make(chan struct{})
	errs := make(chan error, 2)
	targets := []struct{ reviewer, status string }{
		{user.ID, identity.MessageStatusReviewApproved},
		{"", identity.MessageStatusReviewExpiredApproved},
	}
	var wg sync.WaitGroup
	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.ApproveAndAccept(ctx, msg.ID, target.reviewer, target.status, false, acc, enqueueTask6ApprovalJob, nil, nil)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	winners, losers := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, identity.ErrNotPendingApproval):
			losers++
		default:
			t.Fatalf("approval race error = %v", err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("approval race winners=%d losers=%d want 1/1", winners, losers)
	}

	var status string
	var sendJobID *int64
	if err := pool.QueryRow(ctx, `SELECT status, send_job_id FROM messages WHERE id=$1`, msg.ID).Scan(&status, &sendJobID); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusReviewApproved && status != identity.MessageStatusReviewExpiredApproved {
		t.Fatalf("winner status=%s", status)
	}
	if sendJobID == nil {
		t.Fatal("winner send_job_id is nil")
	}
	var jobs int
	var durableJobID int64
	if err := pool.QueryRow(ctx, `SELECT count(*), min(id) FROM task6_approval_jobs WHERE message_id=$1`, msg.ID).Scan(&jobs, &durableJobID); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || *sendJobID != durableJobID {
		t.Fatalf("durable jobs=%d id=%d send_job_id=%d, want one linked job", jobs, durableJobID, *sendJobID)
	}
	reasons := lifecycleReasons(t, pool, msg.ID)
	resolutionCount, queueCount := 0, 0
	var resolution messagelifecycle.ReasonCode
	for _, reason := range reasons {
		if reason == messagelifecycle.ReasonReviewApproved || reason == messagelifecycle.ReasonReviewExpiredApproved {
			resolutionCount++
			resolution = reason
		}
		if reason == messagelifecycle.ReasonQueueOutboundSubmission {
			queueCount++
		}
	}
	if resolutionCount != 1 || queueCount != 1 {
		t.Fatalf("race lifecycle=%v resolution_count=%d queue_count=%d", reasons, resolutionCount, queueCount)
	}
	if status == identity.MessageStatusReviewApproved && resolution != messagelifecycle.ReasonReviewApproved {
		t.Fatalf("status=%s resolution=%s", status, resolution)
	}
	if status == identity.MessageStatusReviewExpiredApproved && resolution != messagelifecycle.ReasonReviewExpiredApproved {
		t.Fatalf("status=%s resolution=%s", status, resolution)
	}
}

func TestApproveAndAcceptLifecycleFailureRollsBack(t *testing.T) {
	pool := testutil.TestDB(t)
	installTask6ApprovalJobs(t, pool)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, _ := store.CreateOrGetUser(ctx, "owner-lifecycle-rb@example.com", "Owner", "google-lifecycle-rb")
	domain := "lifecycle-rb.example.com"
	_, _ = store.ClaimOrCreateDomain(ctx, domain, user.ID)
	_ = store.VerifyDomain(ctx, domain, user.ID)
	ag, _ := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID)
	msg, _ := store.CreatePendingOutboundMessage(ctx, ag.ID, []string{"a@example.net"}, nil, nil, "Subj", "body", "", nil, "send", "", "", "", 3600)
	expiredMsg, _ := store.CreatePendingOutboundMessage(ctx, ag.ID, []string{"a@example.net"}, nil, nil, "Expired subj", "body", "", nil, "send", "", "", "", 3600)
	_, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_fail_approved_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_approved_lifecycle(); CREATE FUNCTION test_fail_approved_lifecycle() RETURNS trigger AS $f$ BEGIN IF NEW.reason_code='queue.outbound_submission' THEN RAISE EXCEPTION 'forced lifecycle failure after durable job'; END IF; RETURN NEW; END; $f$ LANGUAGE plpgsql; CREATE TRIGGER test_fail_approved_lifecycle BEFORE INSERT ON message_lifecycle_transitions FOR EACH ROW EXECUTE FUNCTION test_fail_approved_lifecycle();`)
	if err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_fail_approved_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_approved_lifecycle();`)
	})
	acc := identity.AcceptedSend{To: []string{"a@example.net"}, Subject: "Subj", Method: "smtp", SentAs: "relay", Raw: []byte("raw")}
	_, err = store.ApproveAndAccept(ctx, msg.ID, user.ID, identity.MessageStatusReviewApproved, false, acc, enqueueTask6ApprovalJob, nil, nil)
	if err == nil {
		t.Fatal("approve succeeded despite lifecycle failure")
	}
	_, err = store.ApproveAndAccept(ctx, expiredMsg.ID, "", identity.MessageStatusReviewExpiredApproved, false, acc, enqueueTask6ApprovalJob, nil, nil)
	if err == nil {
		t.Fatal("expired approve succeeded despite lifecycle failure")
	}
	var status string
	var sendJobID *int64
	if err := pool.QueryRow(ctx, `SELECT status, send_job_id FROM messages WHERE id=$1`, msg.ID).Scan(&status, &sendJobID); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusPendingReview || sendJobID != nil {
		t.Fatalf("partial approval status=%s job=%v", status, sendJobID)
	}
	if err := pool.QueryRow(ctx, `SELECT status, send_job_id FROM messages WHERE id=$1`, expiredMsg.ID).Scan(&status, &sendJobID); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusPendingReview || sendJobID != nil {
		t.Fatalf("partial expired approval status=%s job=%v", status, sendJobID)
	}
	var jobCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task6_approval_jobs`).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 0 {
		t.Fatalf("approval jobs survived lifecycle rollback=%d", jobCount)
	}
	for _, id := range []string{msg.ID, expiredMsg.ID} {
		got := lifecycleReasons(t, pool, id)
		if len(got) != 2 || got[0] != messagelifecycle.ReasonAcceptanceOutboundAPI || got[1] != messagelifecycle.ReasonReviewHoldCreated {
			t.Fatalf("lifecycle survived approval rollback for %s: %v", id, got)
		}
	}
}

func TestCreatePendingOutboundLifecycleFailureRollsBack(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, _ := store.CreateOrGetUser(ctx, "owner-hold-rb@example.com", "Owner", "google-hold-rb")
	domain := "hold-rb.example.com"
	_, _ = store.ClaimOrCreateDomain(ctx, domain, user.ID)
	_ = store.VerifyDomain(ctx, domain, user.ID)
	ag, _ := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID)
	_, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_fail_hold_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_hold_lifecycle(); CREATE FUNCTION test_fail_hold_lifecycle() RETURNS trigger AS $f$ BEGIN IF NEW.reason_code='review.hold_created' THEN RAISE EXCEPTION 'forced hold lifecycle failure'; END IF; RETURN NEW; END; $f$ LANGUAGE plpgsql; CREATE TRIGGER test_fail_hold_lifecycle BEFORE INSERT ON message_lifecycle_transitions FOR EACH ROW EXECUTE FUNCTION test_fail_hold_lifecycle();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_fail_hold_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_hold_lifecycle();`)
	})
	var lifecycleBaseline int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions`).Scan(&lifecycleBaseline); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePendingOutboundMessage(ctx, ag.ID, []string{"a@example.net"}, nil, nil, "forced hold rollback", "body", "", nil, "send", "", "", "", 3600); err == nil {
		t.Fatal("hold succeeded despite lifecycle failure")
	}
	var messages, lifecycleAfter int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE agent_id=$1 AND subject='forced hold rollback'`, ag.ID).Scan(&messages)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions`).Scan(&lifecycleAfter)
	if messages != 0 || lifecycleAfter != lifecycleBaseline {
		t.Fatalf("partial hold messages=%d lifecycle before=%d after=%d", messages, lifecycleBaseline, lifecycleAfter)
	}
}

func TestReviewRejectionLifecycleFailureRollsBack(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, _ := store.CreateOrGetUser(ctx, "owner-reject-rb@example.com", "Owner", "google-reject-rb")
	domain := "reject-rb.example.com"
	_, _ = store.ClaimOrCreateDomain(ctx, domain, user.ID)
	_ = store.VerifyDomain(ctx, domain, user.ID)
	ag, _ := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID)
	_, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_fail_rejected_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_rejected_lifecycle(); CREATE FUNCTION test_fail_rejected_lifecycle() RETURNS trigger AS $f$ BEGIN IF NEW.reason_code IN ('review.rejected','review.expired_rejected') THEN RAISE EXCEPTION 'forced rejected lifecycle failure'; END IF; RETURN NEW; END; $f$ LANGUAGE plpgsql; CREATE TRIGGER test_fail_rejected_lifecycle BEFORE INSERT ON message_lifecycle_transitions FOR EACH ROW EXECUTE FUNCTION test_fail_rejected_lifecycle();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_fail_rejected_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_rejected_lifecycle();`)
	})
	newHold := func(subject string) *identity.Message {
		m, err := store.CreatePendingOutboundMessage(ctx, ag.ID, []string{"a@example.net"}, nil, nil, subject, "body", "", nil, "send", "", "", "", 3600)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	human, expired := newHold("human rejection rollback"), newHold("expiry rejection rollback")
	if _, err := store.RejectPending(ctx, human.ID, user.ID, "no"); err == nil {
		t.Fatal("human rejection succeeded despite lifecycle failure")
	}
	if _, err := store.ExpireReject(ctx, expired.ID, "ttl_expired"); err == nil {
		t.Fatal("expiry rejection succeeded despite lifecycle failure")
	}
	for _, id := range []string{human.ID, expired.ID} {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM messages WHERE id=$1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != identity.MessageStatusPendingReview {
			t.Fatalf("status after rollback=%s", status)
		}
	}
}

func TestRejectAndExpireLifecycle(t *testing.T) {
	pool := testutil.TestDB(t)
	installTask6ApprovalJobs(t, pool)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, _ := store.CreateOrGetUser(ctx, "owner-review-lifecycle@example.com", "Owner", "google-review-lifecycle")
	domain := "review-lifecycle.example.com"
	_, _ = store.ClaimOrCreateDomain(ctx, domain, user.ID)
	_ = store.VerifyDomain(ctx, domain, user.ID)
	ag, _ := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID)
	if _, err := pool.Exec(ctx, `INSERT INTO task6_approval_jobs (message_id) VALUES ('baseline')`); err != nil {
		t.Fatal(err)
	}
	newHold := func(subject string) *identity.Message {
		m, err := store.CreatePendingOutboundMessage(ctx, ag.ID, []string{"a@example.net"}, nil, nil, subject, "body", "", nil, "send", "", "", "", 3600)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	human := newHold("human reject")
	if _, err := store.RejectPending(ctx, human.ID, user.ID, "no"); err != nil {
		t.Fatal(err)
	}
	expiredReject := newHold("expired reject")
	if _, err := store.ExpireReject(ctx, expiredReject.ID, "ttl_expired"); err != nil {
		t.Fatal(err)
	}
	expiredApprove := newHold("expired approve")
	acc := identity.AcceptedSend{To: []string{"a@example.net"}, Subject: "expired approve", Method: "smtp", SentAs: "relay", Raw: []byte("raw")}
	if _, err := store.ApproveAndAccept(ctx, expiredApprove.ID, "", identity.MessageStatusReviewExpiredApproved, false, acc, func(context.Context, pgx.Tx, string) (int64, error) { return 77, nil }, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id     string
		reason messagelifecycle.ReasonCode
		queued bool
	}{{human.ID, messagelifecycle.ReasonReviewRejected, false}, {expiredReject.ID, messagelifecycle.ReasonReviewExpiredRejected, false}, {expiredApprove.ID, messagelifecycle.ReasonReviewExpiredApproved, true}} {
		got := lifecycleReasons(t, pool, tc.id)
		found, queued := false, false
		for _, reason := range got {
			found = found || reason == tc.reason
			queued = queued || reason == messagelifecycle.ReasonQueueOutboundSubmission
		}
		if !found || queued != tc.queued {
			t.Fatalf("lifecycle %s=%v want reason=%s queued=%v", tc.id, got, tc.reason, tc.queued)
		}
		if !tc.queued {
			var sendJobID *int64
			if err := pool.QueryRow(ctx, `SELECT send_job_id FROM messages WHERE id=$1`, tc.id).Scan(&sendJobID); err != nil {
				t.Fatal(err)
			}
			if sendJobID != nil {
				t.Fatalf("rejected message %s send_job_id=%v", tc.id, sendJobID)
			}
		}
	}
	var jobCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task6_approval_jobs`).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 1 {
		t.Fatalf("rejection paths changed enqueue/job baseline: %d", jobCount)
	}
}

// TestApproveAndAccept transitions a pending_review outbound hold to
// approved+accepted, enqueues via the callback, stamps the returned job id, and is
// a no-op (ErrNotPendingApproval) once the hold is already resolved.
func TestApproveAndAccept(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	user, err := store.CreateOrGetUser(ctx, "owner-aa@example.com", "Owner", "google-aa")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain := "aa.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	ag, err := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	msg, err := store.CreatePendingOutboundMessage(ctx, ag.ID,
		[]string{"a@gmail.com"}, nil, nil, "Subj", "body", "", nil,
		"send", "conv-aa", "", "", 3600)
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}

	var enqueued int
	enqueue := func(_ context.Context, _ pgx.Tx, _ string) (int64, error) {
		enqueued++
		return 4242, nil
	}
	acc := identity.AcceptedSend{
		To: []string{"a@gmail.com"}, Subject: "Subj", Method: "smtp",
		EnvelopeFrom: "bot@aa.example.com", SentAs: "relay", Raw: []byte("raw-mime"),
	}

	out, err := store.ApproveAndAccept(ctx, msg.ID, user.ID, identity.MessageStatusReviewApproved, false, acc, enqueue, nil, nil)
	if err != nil {
		t.Fatalf("ApproveAndAccept: %v", err)
	}
	if out.Status != identity.MessageStatusReviewApproved {
		t.Errorf("status = %q, want %q", out.Status, identity.MessageStatusReviewApproved)
	}
	if out.DeliveryStatus != "accepted" {
		t.Errorf("delivery_status = %q, want accepted", out.DeliveryStatus)
	}
	if enqueued != 1 {
		t.Errorf("enqueue called %d times, want 1", enqueued)
	}

	// DB reflects the transition, stamped job, and retained outbound content.
	var (
		status, deliveryStatus string
		sendJobID              *int64
		bodyText               *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT status, delivery_status, send_job_id, body_text FROM messages WHERE id=$1`, msg.ID,
	).Scan(&status, &deliveryStatus, &sendJobID, &bodyText); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != identity.MessageStatusReviewApproved || deliveryStatus != "accepted" {
		t.Errorf("db status/delivery = %q/%q", status, deliveryStatus)
	}
	if sendJobID == nil || *sendJobID != 4242 {
		t.Errorf("send_job_id = %v, want 4242", sendJobID)
	}
	if bodyText == nil || *bodyText != "body" {
		t.Errorf("body_text = %v, want retained body", bodyText)
	}

	// Idempotent: a second attempt (row no longer pending_review) is a no-op.
	if _, err := store.ApproveAndAccept(ctx, msg.ID, user.ID, identity.MessageStatusReviewApproved, false, acc, enqueue, nil, nil); err != identity.ErrNotPendingApproval {
		t.Errorf("second ApproveAndAccept err = %v, want ErrNotPendingApproval", err)
	}
	if enqueued != 1 {
		t.Errorf("enqueue called %d times after no-op attempt, want still 1", enqueued)
	}
}
