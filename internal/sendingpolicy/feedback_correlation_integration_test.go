package sendingpolicy_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// authorizedSend drives one message through accept + authorization and
// returns the token, its attempt correlation id, and the SES id it settles
// under, so feedback can be fed back by either key.
func (f *fixture) authorizedSend(g sendingpolicy.Gate, messageID string, recipients []string) (auth sendingpolicy.ProviderAuthorization, correlationID, sesID string) {
	f.t.Helper()
	accept, ref := f.prepareMessage(g, messageID)
	if accept != sendingpolicy.AcceptanceAccept {
		f.t.Fatalf("accept = %s", accept)
	}
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		f.t.Fatal(err)
	}
	decision, token, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || !decision.Allow || token == nil {
		f.t.Fatalf("consume: allow=%v err=%v", decision.Allow, err)
	}
	headers, err := token.ValidateEnvelope(recipients)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := g.RedeemProviderCall(f.ctx, *token); err != nil {
		f.t.Fatal(err)
	}
	sesID = fmt.Sprintf("<%s-ses@us-east-2.amazonses.com>", messageID)
	if err := g.SettleProvider(f.ctx, sendingpolicy.ProviderSettlement{Attempt: token.Attempt(), Outcome: sendingpolicy.SettlementProviderAccepted, ProviderMessageID: sesID}); err != nil {
		f.t.Fatal(err)
	}
	return *token, headers.AttemptCorrelationID, sesID
}

// messageTo inserts an outbound message with explicit recipients.
func (f *fixture) messageTo(agentID, sentAs string, recipients []string) string {
	f.t.Helper()
	messageSeq++
	id := fmt.Sprintf("msg_fb_%d", messageSeq)
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO messages (id, agent_id, direction, to_recipients, sent_as, status)
		 VALUES ($1, $2, 'outbound', $3, $4, 'sent')`, id, agentID, recipients, sentAs); err != nil {
		f.t.Fatalf("insert message: %v", err)
	}
	return id
}

func (f *fixture) outcomes(userID string) map[string][4]int {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `
		SELECT outcome_epoch, day, shared_reputation, delivered_count, terminal_other_count, hard_bounce_count, complaint_count
		  FROM account_sending_outcomes_daily WHERE user_id = $1`, userID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][4]int{}
	for rows.Next() {
		var epoch int64
		var day time.Time
		var shared bool
		var c [4]int
		if err := rows.Scan(&epoch, &day, &shared, &c[0], &c[1], &c[2], &c[3]); err != nil {
			f.t.Fatal(err)
		}
		out[fmt.Sprintf("e%d/%s/shared=%v", epoch, day.Format("2006-01-02"), shared)] = c
	}
	return out
}

func (f *fixture) recipientRow(correlationID string) (bucket string, rank int, epoch *int64) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, `SELECT detector_bucket, evidence_rank, bucket_epoch FROM sending_feedback_recipients WHERE correlation_id = $1`, correlationID).Scan(&bucket, &rank, &epoch); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *fixture) suppressed(userID, address string) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM suppressions WHERE user_id = $1 AND address = $2`, userID, address).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n > 0
}

func feedback(id string, at time.Time, kind delivery.EventKind, sesID, attemptID string, recipients ...string) delivery.ProviderFeedback {
	return delivery.ProviderFeedback{ProviderEventID: id, OccurredAt: at, Kind: kind, ProviderMessageID: sesID, AttemptCorrelationID: attemptID, Recipients: recipients}
}

func todayKey(epoch int64, shared bool) string {
	d := time.Now().UTC()
	return fmt.Sprintf("e%d/%s/shared=%v", epoch, d.Format("2006-01-02"), shared)
}

// TestFeedbackSurvivesMessageAndAgentPurge: authorize a send, purge the
// message and then the agent, and prove feedback by SES id and by the
// attempt header still lands on the right account with a valid HMAC match,
// while a recipient outside the authorized envelope is rejected.
func TestFeedbackSurvivesMessageAndAgentPurge(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	rcpts := []string{"alice@example.test", "bob@example.test"}
	msg := f.messageTo(agent, "relay", rcpts)
	_, corrID, sesID := f.authorizedSend(g, msg, rcpts)

	// Purge message then agent — the deletion order the store uses.
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM messages WHERE id = $1`, msg); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM agent_identities WHERE id = $1`, agent); err != nil {
		t.Fatal(err)
	}

	at := time.Now().UTC()
	// Hard bounce for alice by SES id.
	res, err := module.ProcessProviderFeedback(f.ctx, delivery.ProviderFeedback{
		ProviderEventID: "evt-hb", OccurredAt: at, Kind: delivery.KindBounce, BounceType: "permanent", BounceSubType: "General",
		ProviderMessageID: sesID, Recipients: []string{"Alice@Example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Correlated || res.Duplicate || res.AccountRef != user {
		t.Fatalf("result = %+v", res)
	}
	if s, ok := res.SuppressionFor("alice@example.test"); !ok || !s.Inserted || s.Source != "bounce" {
		t.Fatalf("suppression verdict = %+v ok=%v", s, ok)
	}
	if !f.suppressed(user, "alice@example.test") {
		t.Fatal("hard bounce must recreate the account suppression while the account exists")
	}
	// Delivered for bob by the attempt header only (no SES id).
	if _, err := module.ProcessProviderFeedback(f.ctx, feedback("evt-del", at, delivery.KindDelivery, "", corrID, "bob@example.test")); err != nil {
		t.Fatal(err)
	}
	// A recipient never in the envelope: rejected, no accounting, no suppression.
	res, err = module.ProcessProviderFeedback(f.ctx, delivery.ProviderFeedback{
		ProviderEventID: "evt-forged", OccurredAt: at, Kind: delivery.KindBounce, BounceType: "permanent", BounceSubType: "General",
		ProviderMessageID: sesID, Recipients: []string{"mallory@example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Correlated || len(res.Suppressions) != 0 || f.suppressed(user, "mallory@example.test") {
		t.Fatalf("forged recipient must be rejected: %+v", res)
	}

	got := f.outcomes(user)
	if c := got[todayKey(1, true)]; c != [4]int{1, 0, 1, 0} {
		t.Fatalf("shared aggregate = %v, want delivered=1 hard_bounce=1: %v", c, got)
	}
	if len(got) != 1 {
		t.Fatalf("unexpected aggregate rows: %v", got)
	}

	// Duplicate provider event: zero delta.
	res, err = module.ProcessProviderFeedback(f.ctx, delivery.ProviderFeedback{
		ProviderEventID: "evt-hb", OccurredAt: at, Kind: delivery.KindBounce, BounceType: "permanent", BounceSubType: "General",
		ProviderMessageID: sesID, Recipients: []string{"alice@example.test"},
	})
	if err != nil || !res.Duplicate {
		t.Fatalf("duplicate: res=%+v err=%v", res, err)
	}
	if c := f.outcomes(user)[todayKey(1, true)]; c != [4]int{1, 0, 1, 0} {
		t.Fatalf("duplicate changed the aggregate: %v", c)
	}
}

// TestFeedbackEvidenceIsMonotonic: delivered → complaint replaces (delivered
// subtracted, complaint added); a delayed delivery after a complaint is
// ignored; a suppression-list subtype after a hard bounce keeps the hard
// bounce but still repairs the list; equal-rank duplicates are zero delta.
func TestFeedbackEvidenceIsMonotonic(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	rcpt := []string{"carol@example.test"}
	msg := f.messageTo(agent, "own_address", rcpt) // dedicated path
	_, corrID, sesID := f.authorizedSend(g, msg, rcpt)
	t0 := time.Now().UTC().Add(-time.Hour)

	step := func(id string, at time.Time, kind delivery.EventKind, btype, bsub, csub string) {
		t.Helper()
		if _, err := module.ProcessProviderFeedback(f.ctx, delivery.ProviderFeedback{
			ProviderEventID: id, OccurredAt: at, Kind: kind, BounceType: btype, BounceSubType: bsub, ComplaintSubType: csub,
			ProviderMessageID: sesID, AttemptCorrelationID: corrID, Recipients: rcpt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	key := todayKey(1, false)

	step("e1", t0, delivery.KindDelivery, "", "", "")
	if c := f.outcomes(user)[key]; c != [4]int{1, 0, 0, 0} {
		t.Fatalf("after delivered: %v", c)
	}
	step("e2", t0.Add(time.Minute), delivery.KindComplaint, "", "", "")
	if c := f.outcomes(user)[key]; c != [4]int{0, 0, 0, 1} {
		t.Fatalf("delivered→complaint must move the unit: %v", c)
	}
	if b, r, _ := f.recipientRow(corrID); b != "complaint" || r != 4 {
		t.Fatalf("row = %s/%d", b, r)
	}
	// Delayed lower evidence never regresses.
	step("e3", t0.Add(2*time.Minute), delivery.KindDelivery, "", "", "")
	step("e4", t0.Add(3*time.Minute), delivery.KindBounce, "permanent", "OnAccountSuppressionList", "")
	if c := f.outcomes(user)[key]; c != [4]int{0, 0, 0, 1} {
		t.Fatalf("lower evidence regressed the complaint: %v", c)
	}
	// Equal rank duplicate (a second complaint event): zero delta.
	step("e5", t0.Add(4*time.Minute), delivery.KindComplaint, "", "", "")
	if c := f.outcomes(user)[key]; c != [4]int{0, 0, 0, 1} {
		t.Fatalf("equal-rank duplicate changed counts: %v", c)
	}
	if !f.suppressed(user, "carol@example.test") {
		t.Fatal("complaint must have repaired the suppression")
	}

	// A second message: hard bounce first, then a suppression-list bounce
	// (excluded, repairs) and a delayed transient bounce (lower rank).
	msg2 := f.messageTo(agent, "own_address", []string{"dave@example.test"})
	_, corr2, ses2 := f.authorizedSend(g, msg2, []string{"dave@example.test"})
	fb := func(id string, kind delivery.EventKind, btype, bsub string) delivery.ProviderFeedback {
		return delivery.ProviderFeedback{ProviderEventID: id, OccurredAt: t0, Kind: kind, BounceType: btype, BounceSubType: bsub, ProviderMessageID: ses2, AttemptCorrelationID: corr2, Recipients: []string{"dave@example.test"}}
	}
	for _, x := range []delivery.ProviderFeedback{fb("d1", delivery.KindBounce, "permanent", "General"), fb("d2", delivery.KindBounce, "permanent", "OnTenantSuppressionList"), fb("d3", delivery.KindBounce, "transient", "MailboxFull")} {
		if _, err := module.ProcessProviderFeedback(f.ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if c := f.outcomes(user)[key]; c != [4]int{0, 0, 1, 1} {
		t.Fatalf("second message: %v", c)
	}
	if b, _, _ := f.recipientRow(corr2); b != "hard_bounce" {
		t.Fatalf("row bucket = %s, want hard_bounce kept", b)
	}
	// terminal_other is a denominator unit on its own.
	msg3 := f.messageTo(agent, "own_address", []string{"erin@example.test"})
	_, _, ses3 := f.authorizedSend(g, msg3, []string{"erin@example.test"})
	if _, err := module.ProcessProviderFeedback(f.ctx, delivery.ProviderFeedback{ProviderEventID: "t1", OccurredAt: t0, Kind: delivery.KindBounce, BounceType: "undetermined", ProviderMessageID: ses3, Recipients: []string{"erin@example.test"}}); err != nil {
		t.Fatal(err)
	}
	if c := f.outcomes(user)[key]; c != [4]int{0, 1, 1, 1} {
		t.Fatalf("terminal_other: %v", c)
	}
	if f.suppressed(user, "erin@example.test") {
		t.Fatal("a soft bounce must not suppress")
	}
}

// TestFeedbackSharedAndDedicatedAggregatesStaySeparate: a hosted shared-
// mailbox message and a custom-domain message from one account land in
// different aggregate rows; a HITL notification is shared like the
// hosted message.
func TestFeedbackSharedAndDedicatedAggregatesStaySeparate(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	at := time.Now().UTC()

	shared := f.messageTo(agent, "relay", []string{"s@example.test"})
	_, _, sesShared := f.authorizedSend(g, shared, []string{"s@example.test"})
	dedicated := f.messageTo(agent, "own_address", []string{"d@example.test"})
	_, _, sesDed := f.authorizedSend(g, dedicated, []string{"d@example.test"})
	for _, x := range []delivery.ProviderFeedback{
		feedback("s1", at, delivery.KindDelivery, sesShared, "", "s@example.test"),
		feedback("d1", at, delivery.KindDelivery, sesDed, "", "d@example.test"),
	} {
		if _, err := module.ProcessProviderFeedback(f.ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	// HITL notification for a held message: authorized under the owner's
	// address, counted on the shared path.
	held := f.pendingMessage(agent, "own_address")
	var ref sendingpolicy.OperationRef
	f.inTx(func(tx pgx.Tx) error {
		var err error
		ref, err = g.PrepareNotificationTx(f.ctx, tx, sendingpolicy.NewHITLNotificationRef(held))
		return err
	})
	_, attempt, err := g.Reserve(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := g.ConsumeAttempt(f.ctx, attempt)
	if err != nil || token == nil {
		t.Fatalf("consume notification: %v", err)
	}
	owner := token.AuthorizedRecipients()
	headers, err := token.ValidateEnvelope(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := module.ProcessProviderFeedback(f.ctx, feedback("n1", at, delivery.KindDelivery, "", headers.AttemptCorrelationID, owner...)); err != nil {
		t.Fatal(err)
	}

	got := f.outcomes(user)
	if got[todayKey(1, true)] != [4]int{2, 0, 0, 0} || got[todayKey(1, false)] != [4]int{1, 0, 0, 0} {
		t.Fatalf("aggregates = %v", got)
	}
}

// TestFeedbackAfterAccountDeletionUpdatesProvenanceOnly: once the account is
// gone, feedback still advances the retained bucket but recreates no
// suppression and no aggregate.
func TestFeedbackAfterAccountDeletionUpdatesProvenanceOnly(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	msg := f.messageTo(agent, "relay", []string{"gone@example.test"})
	_, corrID, sesID := f.authorizedSend(g, msg, []string{"gone@example.test"})

	// Delete the whole account (cascades agents, messages, controls, aggregates).
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	res, err := module.ProcessProviderFeedback(f.ctx, delivery.ProviderFeedback{
		ProviderEventID: "post-del", OccurredAt: time.Now().UTC(), Kind: delivery.KindComplaint,
		ProviderMessageID: sesID, Recipients: []string{"gone@example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Correlated || res.AccountRef != "" || len(res.Suppressions) != 0 {
		t.Fatalf("result after deletion = %+v", res)
	}
	b, r, epoch := f.recipientRow(corrID)
	if b != "complaint" || r != 4 || epoch != nil {
		t.Fatalf("provenance = %s/%d epoch=%v, want complaint with no epoch", b, r, epoch)
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM suppressions WHERE address = 'gone@example.test'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a deleted account must not get customer state recreated")
	}
}

// TestKeyringRotationAndCoverage: rows signed under version 1 still match
// after the active key moves to 2 while 1 is retained; a keyring without 1
// fails coverage until those rows expire; a process holding only 2 cannot
// match a version-1 row.
func TestKeyringRotationAndCoverage(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy()) // signs under version 1 (fxHMAC active)
	user := f.user("standard")
	agent := f.agent(user)
	msg := f.messageTo(agent, "relay", []string{"rot@example.test"})
	_, _, sesID := f.authorizedSend(g, msg, []string{"rot@example.test"})

	superset, err := sendingpolicy.LoadKeyring(`{"active":2,"keys":{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","2":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}}`)
	if err != nil {
		t.Fatal(err)
	}
	onlyNew, err := sendingpolicy.LoadKeyring(`{"active":2,"keys":{"2":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}}`)
	if err != nil {
		t.Fatal(err)
	}
	rotated := sendingpolicy.NewModule(f.pool, sendingpolicy.Secrets{Keyring: superset})
	if err := rotated.VerifyKeyringCoverage(f.ctx); err != nil {
		t.Fatalf("superset keyring must cover retained rows: %v", err)
	}
	res, err := rotated.ProcessProviderFeedback(f.ctx, feedback("r1", time.Now().UTC(), delivery.KindDelivery, sesID, "", "rot@example.test"))
	if err != nil || !res.Correlated {
		t.Fatalf("rotated: %+v %v", res, err)
	}
	if c := f.outcomes(user)[todayKey(1, true)]; c != [4]int{1, 0, 0, 0} {
		t.Fatalf("version-1 row must still match under the superset keyring: %v", c)
	}

	narrow := sendingpolicy.NewModule(f.pool, sendingpolicy.Secrets{Keyring: onlyNew})
	if err := narrow.VerifyKeyringCoverage(f.ctx); err == nil {
		t.Fatal("removing version 1 while its rows are retained must fail coverage")
	}
	res, err = narrow.ProcessProviderFeedback(f.ctx, feedback("r2", time.Now().UTC(), delivery.KindComplaint, sesID, "", "rot@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if c := f.outcomes(user)[todayKey(1, true)]; c != [4]int{1, 0, 0, 0} {
		t.Fatalf("a keyring without version 1 must not match its rows: %v", c)
	}

	// Expire the row's correlation: coverage no longer needs version 1.
	if _, err := f.pool.Exec(f.ctx, `UPDATE sending_feedback_correlations SET expires_at = now() - interval '1 minute' WHERE provider_message_id = $1`, sendingpolicy.NormalizeProviderMessageID(sesID)); err != nil {
		t.Fatal(err)
	}
	if err := narrow.VerifyKeyringCoverage(f.ctx); err != nil {
		t.Fatalf("expired rows must not pin a key version: %v", err)
	}
}

// TestFeedbackGC: expired provenance is removed with its recipients and
// events; unexpired rows and recent aggregates stay; old aggregates go.
func TestFeedbackGC(t *testing.T) {
	f := newFixture(t)
	g := f.gate(sendingpolicy.DisabledPolicy())
	module := sendingpolicy.NewModule(f.pool, f.secrets())
	user := f.user("standard")
	agent := f.agent(user)
	live := f.messageTo(agent, "relay", []string{"live@example.test"})
	_, liveCorr, liveSES := f.authorizedSend(g, live, []string{"live@example.test"})
	old := f.messageTo(agent, "relay", []string{"old@example.test"})
	_, oldCorr, oldSES := f.authorizedSend(g, old, []string{"old@example.test"})
	at := time.Now().UTC()
	for _, x := range []delivery.ProviderFeedback{
		feedback("g-live", at, delivery.KindDelivery, liveSES, "", "live@example.test"),
		feedback("g-old", at, delivery.KindDelivery, oldSES, "", "old@example.test"),
	} {
		if _, err := module.ProcessProviderFeedback(f.ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sending_feedback_correlations SET expires_at = now() - interval '1 hour' WHERE correlation_id = $1`, oldCorr); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sending_feedback_events SET expires_at = now() - interval '1 hour' WHERE correlation_id = $1`, oldCorr); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO account_sending_outcomes_daily (user_id, outcome_epoch, day, shared_reputation, delivered_count) VALUES ($1, 1, current_date - 30, true, 5)`, user); err != nil {
		t.Fatal(err)
	}

	st, err := module.GCFeedback(f.ctx, time.Now(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if st.Correlations != 1 || st.Recipients != 1 || st.Events != 1 || st.Outcomes != 1 {
		t.Fatalf("gc stats = %+v", st)
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sending_feedback_correlations WHERE correlation_id IN ($1, $2)`, liveCorr, oldCorr).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("correlations left = %d, want the live one only", n)
	}
	if got := f.outcomes(user); len(got) != 1 || got[todayKey(1, true)] != [4]int{2, 0, 0, 0} {
		t.Fatalf("aggregates after gc = %v", got)
	}
}
