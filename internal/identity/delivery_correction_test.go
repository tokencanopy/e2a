package identity_test

// DB-backed tests for the async-send-contract §3.1 correction machinery:
// failure provenance, provider-accept evidence, header correlation, and the
// guarded terminal writes. These pin the crash/race matrix rows around the
// SMTP-accept↔mark-sent window (a falsely-declared terminal `failed` must be
// correctable; a genuine provider rejection must never be revived).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// seedOutboundRow creates an accepted outbound message and returns its id.
func seedOutboundRow(t *testing.T, store *identity.Store, agentID string, to, cc, bcc []string, label string) string {
	t.Helper()
	ctx := context.Background()
	var msgID string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		m, err := store.CreateOutboundMessageTx(ctx, tx, agentID,
			to, cc, bcc, "S-"+label, "send", "smtp", "", "conv-"+label,
			[]byte("raw"), "accepted", "agent@test.e2a.dev", "relay")
		if err != nil {
			return err
		}
		msgID = m.ID
		return store.StampSendJobIDTx(ctx, tx, msgID, 777)
	}); err != nil {
		t.Fatalf("seed %s: %v", label, err)
	}
	return msgID
}

// failLocally moves the row through sending → failed with 'local' provenance
// (the reconciler/final-attempt shape).
func failLocally(t *testing.T, store *identity.Store, msgID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, msgID); err != nil {
			return err
		}
		info, err := store.MarkOutboundFailedTx(ctx, tx, msgID, "inferred: send job discarded", delivery.FailureSourceLocal)
		if err != nil {
			return err
		}
		if info == nil {
			t.Fatalf("MarkOutboundFailedTx returned nil for a live sending row")
		}
		return nil
	}); err != nil {
		t.Fatalf("failLocally: %v", err)
	}
}

func readRow(t *testing.T, store *identity.Store, msgID string) (status, detail, source, providerID string) {
	t.Helper()
	if err := store.WithTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT COALESCE(delivery_status,''), COALESCE(delivery_detail,''),
			        COALESCE(delivery_failure_source,''), COALESCE(provider_message_id,'')
			   FROM messages WHERE id=$1`, msgID,
		).Scan(&status, &detail, &source, &providerID)
	}); err != nil {
		t.Fatalf("read row %s: %v", msgID, err)
	}
	return
}

func TestThreadProviderKeyProvenance(t *testing.T) {
	t.Run("qualified normal provider result writes canonical key", func(t *testing.T) {
		pool := testutil.TestDB(t)
		store := identity.NewStore(pool)
		ctx := context.Background()
		agentID := convoTestSetup(t, store, "thread-provider-qualified")
		msgID := seedOutboundRow(t, store, agentID, []string{"alice@example.net"}, nil, nil, "thread-provider-qualified")

		if _, err := pool.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, msgID); err != nil {
			t.Fatalf("mark sending: %v", err)
		}
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			info, err := store.MarkOutboundSentTx(ctx, tx, msgID, "<CaseSensitive.Left@MAIL.Example.COM>")
			if err != nil {
				return err
			}
			if info == nil {
				t.Fatal("MarkOutboundSentTx returned nil for live sending row")
			}
			return nil
		}); err != nil {
			t.Fatalf("MarkOutboundSentTx: %v", err)
		}

		var key string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(rfc_message_id_key, '') FROM messages WHERE id=$1`,
			msgID,
		).Scan(&key); err != nil {
			t.Fatalf("read RFC key: %v", err)
		}
		if key != "<CaseSensitive.Left@mail.example.com>" {
			t.Fatalf("qualified provider key = %q, want canonical qualified Message-ID", key)
		}
	})

	t.Run("bare normal provider result cannot become RFC key", func(t *testing.T) {
		pool := testutil.TestDB(t)
		store := identity.NewStore(pool)
		ctx := context.Background()
		agentID := convoTestSetup(t, store, "thread-provider-bare-normal")
		msgID := seedOutboundRow(t, store, agentID, []string{"bob@example.net"}, nil, nil, "thread-provider-bare-normal")

		if _, err := pool.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, msgID); err != nil {
			t.Fatalf("mark sending: %v", err)
		}
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			_, err := store.MarkOutboundSentTx(ctx, tx, msgID, "ses-bare-normal-id")
			return err
		}); err != nil {
			t.Fatalf("MarkOutboundSentTx: %v", err)
		}

		var keyIsNull bool
		if err := pool.QueryRow(ctx,
			`SELECT rfc_message_id_key IS NULL FROM messages WHERE id=$1`,
			msgID,
		).Scan(&keyIsNull); err != nil {
			t.Fatalf("read RFC key nullness: %v", err)
		}
		if !keyIsNull {
			t.Fatal("bare provider result populated rfc_message_id_key")
		}
	})

	t.Run("bare feedback and state-only resolution preserve null key", func(t *testing.T) {
		pool := testutil.TestDB(t)
		store := identity.NewStore(pool)
		ctx := context.Background()
		agentID := convoTestSetup(t, store, "thread-provider-feedback")
		msgID := seedOutboundRow(t, store, agentID, []string{"carol@example.net"}, nil, nil, "thread-provider-feedback")

		if err := store.RecordProviderAcceptEvidence(ctx, msgID, "ses-bare-feedback-id"); err != nil {
			t.Fatalf("RecordProviderAcceptEvidence: %v", err)
		}
		var keyIsNull bool
		if err := pool.QueryRow(ctx,
			`SELECT rfc_message_id_key IS NULL FROM messages WHERE id=$1`,
			msgID,
		).Scan(&keyIsNull); err != nil {
			t.Fatalf("read key after feedback: %v", err)
		}
		if !keyIsNull {
			t.Fatal("bare provider feedback populated rfc_message_id_key")
		}

		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			info, providerID, err := store.ResolveOutboundProviderAcceptedTx(ctx, tx, msgID)
			if err != nil {
				return err
			}
			if info == nil || providerID != "ses-bare-feedback-id" {
				t.Fatalf("state-only resolution = (%+v, %q), want sent info with preserved provider id", info, providerID)
			}
			return nil
		}); err != nil {
			t.Fatalf("ResolveOutboundProviderAcceptedTx: %v", err)
		}

		var status string
		if err := pool.QueryRow(ctx,
			`SELECT delivery_status, rfc_message_id_key IS NULL FROM messages WHERE id=$1`,
			msgID,
		).Scan(&status, &keyIsNull); err != nil {
			t.Fatalf("read state-only resolution: %v", err)
		}
		if status != "sent" || !keyIsNull {
			t.Fatalf("state-only resolution status/key = %q/null:%v, want sent/null", status, keyIsNull)
		}
	})
}

// TestCorrection_LocalFailedCorrectedByCorrelatedDelivered is the load-bearing
// §3.1 regression: a locally inferred failed followed by authoritatively
// correlated delivered evidence corrects the stored message AND the recipient
// rollup, clearing the failure provenance + stale failure detail.
func TestCorrection_LocalFailedCorrectedByCorrelatedDelivered(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "corr-local")

	msgID := seedOutboundRow(t, store, agentID, []string{"alice@gmail.com"}, nil, nil, "corr-local")
	failLocally(t, store, msgID)
	if _, err := pool.Exec(ctx, `UPDATE messages SET delivery_failure_occurred_at=now(),delivery_failure_attempt=3,delivery_failure_blocked_recipients=ARRAY['alice@gmail.com'] WHERE id=$1`, msgID); err != nil {
		t.Fatal(err)
	}

	// The SES notification path: evidence first, then the recipient outcome
	// (mirrors Consumer.Process ordering).
	if err := store.RecordProviderAcceptEvidence(ctx, msgID, "ses-repair-1"); err != nil {
		t.Fatalf("RecordProviderAcceptEvidence: %v", err)
	}
	if err := store.RecordDeliveryOutcome(ctx, msgID, "alice@gmail.com", delivery.StatusDelivered, "250 ok"); err != nil {
		t.Fatalf("RecordDeliveryOutcome: %v", err)
	}

	status, detail, source, providerID := readRow(t, store, msgID)
	if status != "delivered" {
		t.Errorf("delivery_status = %q, want delivered (corrected)", status)
	}
	if source != "" {
		t.Errorf("delivery_failure_source = %q, want cleared after correction", source)
	}
	if detail != "" {
		t.Errorf("delivery_detail = %q, want the stale failure detail cleared", detail)
	}
	if providerID != "ses-repair-1" {
		t.Errorf("provider_message_id = %q, want the evidence-repaired id", providerID)
	}
	var retainedFallback bool
	if err := pool.QueryRow(ctx, `SELECT delivery_failure_occurred_at IS NOT NULL OR delivery_failure_attempt IS NOT NULL OR delivery_failure_blocked_recipients IS NOT NULL FROM messages WHERE id=$1`, msgID).Scan(&retainedFallback); err != nil {
		t.Fatal(err)
	}
	if retainedFallback {
		t.Error("delivery correction retained stale fallback provenance")
	}
	var rcptStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM message_recipients WHERE message_id=$1 AND address='alice@gmail.com'`, msgID,
	).Scan(&rcptStatus); err != nil {
		t.Fatalf("read recipient: %v", err)
	}
	if rcptStatus != "delivered" {
		t.Errorf("recipient status = %q, want delivered", rcptStatus)
	}

	// Duplicate / out-of-order replay stays idempotent: a re-delivered event
	// and a late lower-rank sent do not change anything.
	if err := store.RecordDeliveryOutcome(ctx, msgID, "alice@gmail.com", delivery.StatusDelivered, "250 ok"); err != nil {
		t.Fatalf("duplicate RecordDeliveryOutcome: %v", err)
	}
	if err := store.RecordDeliveryOutcome(ctx, msgID, "alice@gmail.com", delivery.StatusSent, ""); err != nil {
		t.Fatalf("late RecordDeliveryOutcome: %v", err)
	}
	status, _, _, _ = readRow(t, store, msgID)
	if status != "delivered" {
		t.Errorf("after replays delivery_status = %q, want delivered", status)
	}
}

// TestCorrection_ProviderConfirmedFailureNeverRevived: an explicit provider
// rejection (provenance 'provider') holds against later delivered feedback.
func TestCorrection_ProviderConfirmedFailureNeverRevived(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "corr-provider")

	msgID := seedOutboundRow(t, store, agentID, []string{"bob@gmail.com"}, nil, nil, "corr-provider")
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, msgID); err != nil {
			return err
		}
		_, err := store.MarkOutboundFailedTx(ctx, tx, msgID, "550 permanent rejection", delivery.FailureSourceProvider)
		return err
	}); err != nil {
		t.Fatalf("fail with provider provenance: %v", err)
	}

	if err := store.RecordDeliveryOutcome(ctx, msgID, "bob@gmail.com", delivery.StatusDelivered, "250 ok"); err != nil {
		t.Fatalf("RecordDeliveryOutcome: %v", err)
	}
	status, detail, source, _ := readRow(t, store, msgID)
	if status != "failed" {
		t.Errorf("delivery_status = %q, want failed (provider-confirmed is never revived)", status)
	}
	if source != "provider" || detail != "550 permanent rejection" {
		t.Errorf("provenance/detail = %q/%q, want provider/550 permanent rejection preserved", source, detail)
	}
}

// TestCorrection_UncorrelatedAddressCannotCorrect: feedback for an address the
// message never targeted must not create the recipient row that would revive a
// failure through the rollup.
func TestCorrection_UncorrelatedAddressCannotCorrect(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "corr-stranger")

	msgID := seedOutboundRow(t, store, agentID, []string{"carol@gmail.com"}, nil, nil, "corr-stranger")
	failLocally(t, store, msgID)

	if err := store.RecordDeliveryOutcome(ctx, msgID, "stranger@evil.com", delivery.StatusDelivered, "250 ok"); err != nil {
		t.Fatalf("RecordDeliveryOutcome: %v", err)
	}
	status, _, source, _ := readRow(t, store, msgID)
	if status != "failed" || source != "local" {
		t.Errorf("status/source = %q/%q, want failed/local — unrelated feedback must not revive a failure", status, source)
	}
	var rcpts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM message_recipients WHERE message_id=$1`, msgID,
	).Scan(&rcpts); err != nil {
		t.Fatalf("count recipients: %v", err)
	}
	if rcpts != 0 {
		t.Errorf("recipient rows = %d, want 0 (non-envelope address skipped on a failed row)", rcpts)
	}
}

// TestCorrection_RejectRollupIsProviderConfirmed: an SES Reject (per-recipient
// failed) rolls up to a provider-confirmed message failure that later delivered
// feedback for the same recipient cannot revive.
func TestCorrection_RejectRollupIsProviderConfirmed(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "corr-reject")

	msgID := seedOutboundRow(t, store, agentID, []string{"dave@gmail.com"}, nil, nil, "corr-reject")
	if _, err := pool.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, msgID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Reject event → per-recipient failed → rollup failed with 'provider'.
	if err := store.RecordDeliveryOutcome(ctx, msgID, "dave@gmail.com", delivery.StatusFailed, "SES Reject"); err != nil {
		t.Fatalf("RecordDeliveryOutcome(reject): %v", err)
	}
	status, _, source, _ := readRow(t, store, msgID)
	if status != "failed" || source != "provider" {
		t.Fatalf("status/source = %q/%q, want failed/provider", status, source)
	}

	// A later delivered for the same recipient: per-recipient Merge keeps
	// failed (Reject is provider-confirmed at the recipient level too), so the
	// rollup cannot revive the failure.
	if err := store.RecordDeliveryOutcome(ctx, msgID, "dave@gmail.com", delivery.StatusDelivered, "250 ok"); err != nil {
		t.Fatalf("RecordDeliveryOutcome(delivered): %v", err)
	}
	status, _, source, _ = readRow(t, store, msgID)
	if status != "failed" || source != "provider" {
		t.Errorf("after late delivered: status/source = %q/%q, want failed/provider", status, source)
	}
}

// TestCorrection_FailedDoesNotOverridePriorComplaint: a complaint recorded
// before a late local failure attempt keeps precedence end to end.
func TestCorrection_FailedDoesNotOverridePriorComplaint(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "corr-complaint")

	msgID := seedOutboundRow(t, store, agentID, []string{"eve@gmail.com"}, nil, nil, "corr-complaint")
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE messages SET delivery_status='sending' WHERE id=$1`, msgID); err != nil {
			return err
		}
		_, err := store.MarkOutboundSentTx(ctx, tx, msgID, "<ses-compl>")
		return err
	}); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if err := store.RecordDeliveryOutcome(ctx, msgID, "eve@gmail.com", delivery.StatusComplained, "abuse"); err != nil {
		t.Fatalf("RecordDeliveryOutcome(complained): %v", err)
	}

	// A late terminal-failure write (e.g. a stale reconciler candidate) is a
	// no-op: the CAS only covers accepted/sending.
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		info, err := store.MarkOutboundFailedTx(ctx, tx, msgID, "late failure", delivery.FailureSourceLocal)
		if info != nil {
			t.Errorf("MarkOutboundFailedTx over complained returned info %+v, want nil", info)
		}
		return err
	}); err != nil {
		t.Fatalf("MarkOutboundFailedTx: %v", err)
	}
	status, _, _, _ := readRow(t, store, msgID)
	if status != "complained" {
		t.Errorf("delivery_status = %q, want complained preserved", status)
	}

	// And a Reject-shaped failed outcome for the same recipient cannot
	// downgrade the complaint either (Merge precedence).
	if err := store.RecordDeliveryOutcome(ctx, msgID, "eve@gmail.com", delivery.StatusFailed, "late reject"); err != nil {
		t.Fatalf("RecordDeliveryOutcome(failed): %v", err)
	}
	status, _, _, _ = readRow(t, store, msgID)
	if status != "complained" {
		t.Errorf("after late failed: delivery_status = %q, want complained", status)
	}
}

// TestProviderAcceptEvidence_RecordAndGuard pins the evidence plumbing: the
// idempotent evidence write + provider-id repair, the claim payload exposure,
// the MarkOutboundFailedTx evidence guard, and
// ResolveOutboundProviderAcceptedTx's settle-as-sent.
func TestProviderAcceptEvidence_RecordAndGuard(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "corr-evidence")

	msgID := seedOutboundRow(t, store, agentID, []string{"frank@gmail.com"}, nil, nil, "corr-evidence")

	// Evidence is idempotent and repairs only an EMPTY provider id.
	if err := store.RecordProviderAcceptEvidence(ctx, msgID, "ses-first"); err != nil {
		t.Fatalf("RecordProviderAcceptEvidence: %v", err)
	}
	var firstAt time.Time
	if err := pool.QueryRow(ctx, `SELECT provider_accepted_at FROM messages WHERE id=$1`, msgID).Scan(&firstAt); err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	if err := store.RecordProviderAcceptEvidence(ctx, msgID, "ses-second"); err != nil {
		t.Fatalf("second RecordProviderAcceptEvidence: %v", err)
	}
	var secondAt time.Time
	var providerID string
	if err := pool.QueryRow(ctx,
		`SELECT provider_accepted_at, provider_message_id FROM messages WHERE id=$1`, msgID,
	).Scan(&secondAt, &providerID); err != nil {
		t.Fatalf("re-read evidence: %v", err)
	}
	if !secondAt.Equal(firstAt) {
		t.Errorf("provider_accepted_at moved on duplicate evidence: %v → %v", firstAt, secondAt)
	}
	if providerID != "ses-first" {
		t.Errorf("provider_message_id = %q, want the first-seen id preserved", providerID)
	}

	// The claim payload surfaces the evidence so the worker can settle
	// without re-submitting.
	p, err := store.ClaimOutboundForSend(ctx, msgID, 777)
	if err != nil || p == nil {
		t.Fatalf("ClaimOutboundForSend = (%v, %v), want payload", p, err)
	}
	if !p.ProviderAccepted || p.ProviderMessageID != "ses-first" {
		t.Errorf("claim payload evidence = (%v, %q), want (true, ses-first)", p.ProviderAccepted, p.ProviderMessageID)
	}

	// The failure CAS refuses a row with evidence…
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		info, err := store.MarkOutboundFailedTx(ctx, tx, msgID, "would be a false failure", delivery.FailureSourceLocal)
		if info != nil {
			t.Errorf("MarkOutboundFailedTx over evidence returned info %+v, want nil", info)
		}
		return err
	}); err != nil {
		t.Fatalf("MarkOutboundFailedTx: %v", err)
	}
	status, _, _, _ := readRow(t, store, msgID)
	if status != "sending" {
		t.Fatalf("status after refused failure = %q, want sending (unchanged)", status)
	}

	// …and the resolve settles it as sent with the evidence id + recipients.
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		info, resolvedID, err := store.ResolveOutboundProviderAcceptedTx(ctx, tx, msgID)
		if err != nil {
			return err
		}
		if info == nil || resolvedID != "ses-first" {
			t.Fatalf("ResolveOutboundProviderAcceptedTx = (%+v, %q), want info + ses-first", info, resolvedID)
		}
		return nil
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	status, _, source, providerID := readRow(t, store, msgID)
	if status != "sent" || providerID != "ses-first" || source != "" {
		t.Errorf("after resolve: status/provider/source = %q/%q/%q, want sent/ses-first/''", status, providerID, source)
	}
	var rcpts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM message_recipients WHERE message_id=$1 AND status='sent'`, msgID,
	).Scan(&rcpts); err != nil {
		t.Fatalf("count recipients: %v", err)
	}
	if rcpts != 1 {
		t.Errorf("sent recipient rows = %d, want 1", rcpts)
	}

	// A second resolve is a no-op (row already sent).
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		info, _, err := store.ResolveOutboundProviderAcceptedTx(ctx, tx, msgID)
		if info != nil {
			t.Errorf("second resolve returned info %+v, want nil", info)
		}
		return err
	}); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
}

// TestResolveOutboundProviderAcceptedTx_NoEvidenceIsNoOp: without evidence the
// resolve declines and the caller proceeds to the failure write.
func TestResolveOutboundProviderAcceptedTx_NoEvidenceIsNoOp(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "corr-noevidence")

	msgID := seedOutboundRow(t, store, agentID, []string{"gina@gmail.com"}, nil, nil, "corr-noevidence")
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		info, _, err := store.ResolveOutboundProviderAcceptedTx(ctx, tx, msgID)
		if info != nil {
			t.Errorf("resolve without evidence returned info %+v, want nil", info)
		}
		return err
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	status, _, _, _ := readRow(t, store, msgID)
	if status != "accepted" {
		t.Errorf("status = %q, want accepted unchanged", status)
	}
}

// TestCorrelateByE2AMessageID pins the header-echo correlation lookup.
func TestCorrelateByE2AMessageID(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "corr-lookup")
	_ = pool

	msgID := seedOutboundRow(t, store, agentID, []string{"hank@gmail.com"}, nil, nil, "corr-lookup")

	m, found, err := store.CorrelateByE2AMessageID(ctx, msgID)
	if err != nil || !found {
		t.Fatalf("CorrelateByE2AMessageID = (found=%v, err=%v), want found", found, err)
	}
	if m.MessageID != msgID || m.AgentID != agentID || m.UserID == "" || m.Subject != "S-corr-lookup" {
		t.Errorf("correlation = (%q,%q,%q,%q), want the seeded row", m.MessageID, m.UserID, m.AgentID, m.Subject)
	}

	if _, found, err := store.CorrelateByE2AMessageID(ctx, "msg_does_not_exist"); err != nil || found {
		t.Errorf("unknown id = (found=%v, err=%v), want not found", found, err)
	}
	if _, found, err := store.CorrelateByE2AMessageID(ctx, ""); err != nil || found {
		t.Errorf("empty id = (found=%v, err=%v), want not found", found, err)
	}
}

// TestDeferOutboundTerminalFailure pins the deferred-final-attempt write: the
// diagnostic is stored, the I/O claim released, the status left pre-terminal,
// and a foreign job id cannot scribble.
func TestDeferOutboundTerminalFailure(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "corr-defer")

	msgID := seedOutboundRow(t, store, agentID, []string{"iris@gmail.com"}, nil, nil, "corr-defer")
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET delivery_status='sending', send_claimed_at=now() WHERE id=$1`, msgID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Foreign job id: no-op.
	if err := store.DeferOutboundTerminalFailure(ctx, msgID, 999, "not mine"); err != nil {
		t.Fatalf("foreign defer: %v", err)
	}
	status, detail, _, _ := readRow(t, store, msgID)
	if detail != "" || status != "sending" {
		t.Fatalf("foreign defer wrote (%q, %q), want no-op", status, detail)
	}

	if err := store.DeferOutboundTerminalFailure(ctx, msgID, 777, "451 timeout after DATA"); err != nil {
		t.Fatalf("defer: %v", err)
	}
	var claimedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT send_claimed_at FROM messages WHERE id=$1`, msgID).Scan(&claimedAt); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	status, detail, _, _ = readRow(t, store, msgID)
	if status != "sending" || detail != "451 timeout after DATA" || claimedAt != nil {
		t.Errorf("after defer: (%q, %q, claim=%v), want sending + diagnostic + released claim", status, detail, claimedAt)
	}
}
