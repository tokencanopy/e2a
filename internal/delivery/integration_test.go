//go:build integration

package delivery_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/eventpayload/goldenassert"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

func transactionalDeliveryFirer(outbox webhookpub.Outbox) delivery.Firer {
	return func(ctx context.Context, tx pgx.Tx, event delivery.FiredEvent) error {
		return outbox.PublishTx(ctx, tx, webhookpub.Event{
			ID:             webhookpub.DeterministicEventID(event.DedupKey),
			Type:           event.Type,
			CreatedAt:      event.OccurredAt,
			UserID:         event.UserID,
			AgentID:        event.AgentID,
			ConversationID: event.ConversationID,
			MessageID:      event.MessageID,
			Data:           event.Data,
		})
	}
}

func TestConsumerLifecycleMappings(t *testing.T) {
	tests := []struct {
		name       string
		kind       delivery.EventKind
		status     delivery.Status
		bounceType string
		suppress   bool
		reason     messagelifecycle.ReasonCode
		retryable  bool
	}{
		{"delivered", delivery.KindDelivery, delivery.StatusDelivered, "", false, messagelifecycle.ReasonDeliveryRecipientServerAccepted, false},
		{"delay", delivery.KindDeliveryDelay, delivery.StatusDeferred, "", false, messagelifecycle.ReasonDeliveryTemporaryDelay, true},
		{"permanent bounce", delivery.KindBounce, delivery.StatusBounced, "permanent", true, messagelifecycle.ReasonDeliveryPermanentBounce, false},
		{"transient bounce", delivery.KindBounce, delivery.StatusBounced, "transient", false, messagelifecycle.ReasonDeliveryTransientBounce, true},
		{"undetermined bounce", delivery.KindBounce, delivery.StatusBounced, "undetermined", false, messagelifecycle.ReasonDeliveryUndeterminedBounce, false},
		{"complaint", delivery.KindComplaint, delivery.StatusComplained, "", true, messagelifecycle.ReasonComplaintRecipientReported, false},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.TestDB(t)
			store := identity.NewStore(pool)
			_, messageID, _ := seedOutbound(t, store, "lifecycle-"+string(rune('a'+i)), "ses-lifecycle-"+string(rune('a'+i)), []string{"Person@Example.COM"})
			ev := &delivery.Event{Kind: tc.kind, SESMessageID: "ses-lifecycle-" + string(rune('a'+i)), ProviderEventID: "sns-lifecycle-" + string(rune('a'+i)), OccurredAt: time.Date(2026, 7, 21, 11, i, 0, 0, time.UTC), E2AMessageID: messageID, BounceType: tc.bounceType, BounceSubType: "General", Recipients: []delivery.RecipientOutcome{{Address: "person@example.com", Status: tc.status, Detail: "250 safe diagnostic", Suppress: tc.suppress}}}
			consumer := delivery.NewConsumer(store, nil)
			if err := consumer.Process(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
			if err := consumer.Process(context.Background(), ev); err != nil {
				t.Fatalf("duplicate feedback: %v", err)
			}
			var recipient, reason string
			var retryable bool
			var evidenceRaw, correlationsRaw []byte
			if err := pool.QueryRow(context.Background(), `SELECT recipient,reason_code,retryable,evidence,correlation_ids FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code=$2`, messageID, tc.reason).Scan(&recipient, &reason, &retryable, &evidenceRaw, &correlationsRaw); err != nil {
				t.Fatal(err)
			}
			if recipient != "person@example.com" || reason != string(tc.reason) || retryable != tc.retryable {
				t.Fatalf("transition recipient=%q reason=%q retryable=%v", recipient, reason, retryable)
			}
			var evidence map[string]any
			var correlations map[string]string
			if err := json.Unmarshal(evidenceRaw, &evidence); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(correlationsRaw, &correlations); err != nil {
				t.Fatal(err)
			}
			if evidence["smtp_detail"] != "250 safe diagnostic" || correlations["provider_message_id"] != ev.SESMessageID || correlations["provider_event_id"] != ev.ProviderEventID || correlations["email_message_id"] != messageID {
				t.Fatalf("evidence=%v correlations=%v", evidence, correlations)
			}
			var count int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code=$2`, messageID, tc.reason).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("logical transitions=%d want 1", count)
			}
		})
	}
}

func TestConsumerCausalSuppressionLifecycleAndEventParity(t *testing.T) {
	tests := []struct {
		name              string
		kind              delivery.EventKind
		status            delivery.Status
		bounceType        string
		detail            string
		feedbackReason    messagelifecycle.ReasonCode
		suppressionReason messagelifecycle.ReasonCode
		suppressionSource string
		eventType         string
	}{
		{"hard bounce", delivery.KindBounce, delivery.StatusBounced, "permanent", "550 5.1.1 no such user", messagelifecycle.ReasonDeliveryPermanentBounce, messagelifecycle.ReasonSuppressionHardBounceApplied, "bounce", delivery.EventEmailBounced},
		{"complaint", delivery.KindComplaint, delivery.StatusComplained, "", "abuse", messagelifecycle.ReasonComplaintRecipientReported, messagelifecycle.ReasonSuppressionComplaintApplied, "complaint", delivery.EventEmailComplained},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.TestDB(t)
			ctx := context.Background()
			store := identity.NewStore(pool)
			providerID := "ses-causal-" + string(rune('a'+i))
			providerEventID := "sns-causal-" + string(rune('a'+i))
			recipient := "person@example.com"
			if tc.name == "hard bounce" {
				providerID = "ses-golden"
				providerEventID = "sns-test"
				recipient = "bob@customer.example.com"
			}
			userID, messageID, agentEmail := seedOutbound(t, store, "causal-"+string(rune('a'+i)), providerID, []string{recipient})
			outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
			ev := &delivery.Event{
				Kind: tc.kind, SESMessageID: providerID, ProviderEventID: providerEventID,
				OccurredAt: time.Date(2026, 7, 21, 12, i, 0, 0, time.UTC), BounceType: tc.bounceType, BounceSubType: "General",
				Recipients: []delivery.RecipientOutcome{{Address: " " + recipient + " ", Status: tc.status, Detail: tc.detail, Suppress: true}},
			}
			consumer := delivery.NewConsumer(store, transactionalDeliveryFirer(outbox))
			if err := consumer.Process(ctx, ev); err != nil {
				t.Fatal(err)
			}
			if err := consumer.Process(ctx, ev); err != nil {
				t.Fatalf("duplicate feedback: %v", err)
			}

			var suppressions int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM suppressions WHERE user_id=$1 AND address=$2 AND source_message_id=$3`, userID, recipient, messageID).Scan(&suppressions); err != nil {
				t.Fatal(err)
			}
			if suppressions != 1 {
				t.Fatalf("suppressions=%d want 1", suppressions)
			}
			// The stored reason is the PROVIDER DIAGNOSTIC, not a derived
			// label: it is a public field of GET /v1/account/suppressions,
			// and an accounting layer that quietly rewrote it would change
			// what every existing integration reads.
			var storedReason, storedSource string
			if err := pool.QueryRow(ctx, `SELECT reason, source FROM suppressions WHERE user_id=$1 AND address=$2`, userID, recipient).Scan(&storedReason, &storedSource); err != nil {
				t.Fatal(err)
			}
			if storedReason != tc.detail {
				t.Fatalf("suppression reason = %q, want the provider diagnostic %q", storedReason, tc.detail)
			}
			if storedSource != tc.suppressionSource {
				t.Fatalf("suppression source = %q, want %q", storedSource, tc.suppressionSource)
			}
			var lifecycleCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code=ANY($2)`, messageID, []string{string(tc.feedbackReason), string(tc.suppressionReason)}).Scan(&lifecycleCount); err != nil {
				t.Fatal(err)
			}
			if lifecycleCount != 2 {
				t.Fatalf("causal lifecycle transitions=%d want 2", lifecycleCount)
			}
			if err := jobs.Migrate(ctx, pool); err != nil {
				t.Fatalf("migrate River lifecycle read dependency: %v", err)
			}
			publicLifecycle, err := messagelifecycle.NewStore(pool).ListForMessage(ctx, messageID, agentEmail)
			if err != nil {
				t.Fatalf("list public lifecycle: %v", err)
			}
			for _, reason := range []messagelifecycle.ReasonCode{tc.feedbackReason, tc.suppressionReason} {
				count := 0
				for _, transition := range publicLifecycle {
					if transition.ReasonCode == reason {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("public lifecycle reason %s count=%d want 1: %+v", reason, count, publicLifecycle)
				}
			}

			var eventEnvelope, suppressionEnvelope []byte
			if err := pool.QueryRow(ctx, `SELECT envelope FROM webhook_events WHERE user_id=$1 AND message_id=$2 AND type=$3`, userID, messageID, tc.eventType).Scan(&eventEnvelope); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT envelope FROM webhook_events WHERE user_id=$1 AND type=$2`, userID, delivery.EventSuppressionAdded).Scan(&suppressionEnvelope); err != nil {
				t.Fatal(err)
			}
			for label, envelope := range map[string][]byte{"feedback": eventEnvelope, "suppression": suppressionEnvelope} {
				var decoded struct {
					Data struct {
						LifecycleTransitions []messagelifecycle.MessageLifecycleTransition `json:"lifecycle_transitions"`
					} `json:"data"`
				}
				if err := json.Unmarshal(envelope, &decoded); err != nil {
					t.Fatalf("decode %s event: %v", label, err)
				}
				if len(decoded.Data.LifecycleTransitions) == 0 {
					t.Fatalf("%s event omitted canonical lifecycle: %s", label, envelope)
				}
				wantReason := tc.feedbackReason
				if label == "suppression" {
					wantReason = tc.suppressionReason
				}
				if len(decoded.Data.LifecycleTransitions) != 1 || decoded.Data.LifecycleTransitions[0].ReasonCode != wantReason {
					t.Fatalf("%s event lifecycle=%+v want only %s", label, decoded.Data.LifecycleTransitions, wantReason)
				}
				if tc.name == "hard bounce" {
					fixture := "../eventpayload/testdata/email.bounced.json"
					if label == "suppression" {
						fixture = "../eventpayload/testdata/domain.suppression_added.json"
					}
					goldenassert.Lifecycle(t, fixture, decoded.Data.LifecycleTransitions)
				}
				for _, transition := range decoded.Data.LifecycleTransitions {
					var persisted int
					if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions WHERE id=$1 AND message_id=$2`, transition.ID, messageID).Scan(&persisted); err != nil {
						t.Fatal(err)
					}
					if persisted != 1 {
						t.Fatalf("%s event transition %q was not persisted", label, transition.ID)
					}
				}
			}
			var suppressionEvidenceRaw []byte
			if err := pool.QueryRow(ctx, `SELECT evidence FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code=$2`, messageID, tc.suppressionReason).Scan(&suppressionEvidenceRaw); err != nil {
				t.Fatal(err)
			}
			var suppressionEvidence map[string]any
			if err := json.Unmarshal(suppressionEvidenceRaw, &suppressionEvidence); err != nil {
				t.Fatal(err)
			}
			wantSource := "bounce"
			if tc.kind == delivery.KindComplaint {
				wantSource = "complaint"
			}
			if suppressionEvidence["suppression_scope"] != "account" || suppressionEvidence["suppression_source"] != wantSource {
				t.Fatalf("suppression evidence=%v", suppressionEvidence)
			}
		})
	}
}

func TestFeedbackIgnoresRecipientsOutsideMessageEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       delivery.EventKind
		status     delivery.Status
		bounceType string
	}{
		{"delivery", delivery.KindDelivery, delivery.StatusDelivered, ""},
		{"delay", delivery.KindDeliveryDelay, delivery.StatusDeferred, ""},
		{"bounce", delivery.KindBounce, delivery.StatusBounced, "permanent"},
		{"complaint", delivery.KindComplaint, delivery.StatusComplained, ""},
		{"reject", delivery.KindReject, delivery.StatusFailed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.TestDB(t)
			ctx := context.Background()
			store := identity.NewStore(pool)
			userID, messageID, agentEmail := seedOutbound(t, store, "unrelated-"+tc.name, "ses-unrelated-"+tc.name, []string{"actual@example.com"})
			outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
			event := &delivery.Event{Kind: tc.kind, SESMessageID: "ses-unrelated-" + tc.name, ProviderEventID: "sns-unrelated-" + tc.name, OccurredAt: time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC), BounceType: tc.bounceType, Recipients: []delivery.RecipientOutcome{{Address: "unrelated@example.com", Status: tc.status, Detail: "diagnostic", Suppress: true}}}
			if err := delivery.NewConsumer(store, transactionalDeliveryFirer(outbox)).Process(ctx, event); err != nil {
				t.Fatal(err)
			}
			if got := deliveryStatus(t, store, messageID, agentEmail); got != "sent" {
				t.Fatalf("message status=%q want sent", got)
			}
			for label, query := range map[string]string{
				"recipient":   `SELECT count(*) FROM message_recipients WHERE message_id=$1 AND address='unrelated@example.com'`,
				"lifecycle":   `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND recipient='unrelated@example.com'`,
				"suppression": `SELECT count(*) FROM suppressions WHERE user_id=$1`,
				"event":       `SELECT count(*) FROM webhook_events WHERE user_id=$1`,
			} {
				arg := any(messageID)
				if label == "suppression" || label == "event" {
					arg = userID
				}
				var count int
				if err := pool.QueryRow(ctx, query, arg).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("%s rows=%d want 0", label, count)
				}
			}
		})
	}
}

func TestForeignOnlyFeedbackCannotFinalizeLocallyFailedMessage(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, err := store.CreateOrGetUser(ctx, "owner-foreign-failed@example.com", "Owner", "g-foreign-failed")
	if err != nil {
		t.Fatal(err)
	}
	domain := "foreign-failed.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatal(err)
	}
	agentEmail := "bot@" + domain
	if _, err := store.CreateAgent(ctx, agentEmail, domain, "", "", "", user.ID); err != nil {
		t.Fatal(err)
	}
	msg, err := store.CreateOutboundMessage(ctx, agentEmail, []string{"actual@example.com"}, nil, nil, "Subj", "send", "smtp", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	fallbackAt := time.Date(2026, 7, 21, 12, 58, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE messages SET delivery_status='failed',send_job_id=321,delivery_detail='local terminal fallback',delivery_failure_source='local',delivery_failure_reason_code='submission.local_retries_exhausted',delivery_failure_occurred_at=$2,delivery_failure_attempt=4,delivery_failure_blocked_recipients=ARRAY['blocked@example.com'] WHERE id=$1`, msg.ID, fallbackAt); err != nil {
		t.Fatal(err)
	}
	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	finalizer := agent.NewOutboundSendStore(store, outbox, usage.NewUsageTracker(usage.NewStore(pool)))
	consumer := delivery.NewConsumer(store, transactionalDeliveryFirer(outbox), finalizer.FinalizeProviderAcceptedTx)
	queries := map[string]string{
		"foreign recipient": `SELECT count(*) FROM message_recipients WHERE message_id=$1 AND address='foreign@example.com'`,
		"lifecycle":         `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1`,
		"events":            `SELECT count(*) FROM webhook_events WHERE message_id=$1`,
		"suppressions":      `SELECT count(*) FROM suppressions WHERE user_id=$1`,
		"usage":             `SELECT count(*) FROM usage_events WHERE user_id=$1`,
	}
	baselines := make(map[string]int, len(queries))
	for label, query := range queries {
		arg := any(msg.ID)
		if label == "suppressions" || label == "usage" {
			arg = user.ID
		}
		var count int
		if err := pool.QueryRow(ctx, query, arg).Scan(&count); err != nil {
			t.Fatal(err)
		}
		baselines[label] = count
	}
	event := &delivery.Event{Kind: delivery.KindDelivery, SESMessageID: "ses-foreign-failed", E2AMessageID: msg.ID, ProviderEventID: "sns-foreign-failed", OccurredAt: time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC), Recipients: []delivery.RecipientOutcome{{Address: "FOREIGN@example.com", Status: delivery.StatusDelivered}}}
	if err := consumer.Process(ctx, event); err != nil {
		t.Fatal(err)
	}

	var status, source, reason, detail, providerID string
	var acceptedAt *time.Time
	var occurredAt *time.Time
	var attempt *int
	var blocked []string
	if err := pool.QueryRow(ctx, `SELECT delivery_status,COALESCE(delivery_failure_source,''),COALESCE(delivery_failure_reason_code,''),COALESCE(delivery_detail,''),COALESCE(provider_message_id,''),provider_accepted_at,delivery_failure_occurred_at,delivery_failure_attempt,COALESCE(delivery_failure_blocked_recipients,'{}') FROM messages WHERE id=$1`, msg.ID).Scan(&status, &source, &reason, &detail, &providerID, &acceptedAt, &occurredAt, &attempt, &blocked); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || source != "local" || reason != "submission.local_retries_exhausted" || detail != "local terminal fallback" || providerID != "" || acceptedAt != nil || occurredAt == nil || !occurredAt.Equal(fallbackAt) || attempt == nil || *attempt != 4 || len(blocked) != 1 || blocked[0] != "blocked@example.com" {
		t.Fatalf("foreign feedback mutated failed row: status=%q source=%q reason=%q detail=%q provider=%q accepted=%v occurred=%v attempt=%v blocked=%v", status, source, reason, detail, providerID, acceptedAt, occurredAt, attempt, blocked)
	}
	for label, query := range queries {
		arg := any(msg.ID)
		if label == "suppressions" || label == "usage" {
			arg = user.ID
		}
		var count int
		if err := pool.QueryRow(ctx, query, arg).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != baselines[label] {
			t.Fatalf("%s rows changed from %d to %d", label, baselines[label], count)
		}
	}
}

func TestMixedFeedbackProcessesOnlyEnvelopeRecipients(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	userID, messageID, agentEmail := seedOutbound(t, store, "mixed-recipients", "ses-mixed-recipients", []string{"actual@example.com"})
	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	event := &delivery.Event{Kind: delivery.KindBounce, SESMessageID: "ses-mixed-recipients", ProviderEventID: "sns-mixed-recipients", OccurredAt: time.Date(2026, 7, 21, 13, 1, 0, 0, time.UTC), BounceType: "permanent", Recipients: []delivery.RecipientOutcome{{Address: "ACTUAL@example.com", Status: delivery.StatusBounced, Detail: "550 actual", Suppress: true}, {Address: "foreign@example.com", Status: delivery.StatusBounced, Detail: "550 foreign", Suppress: true}}}
	if err := delivery.NewConsumer(store, transactionalDeliveryFirer(outbox)).Process(ctx, event); err != nil {
		t.Fatal(err)
	}
	if got := deliveryStatus(t, store, messageID, agentEmail); got != "bounced" {
		t.Fatalf("message status=%q want bounced", got)
	}
	var actualStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM message_recipients WHERE message_id=$1 AND address='actual@example.com'`, messageID).Scan(&actualStatus); err != nil {
		t.Fatal(err)
	}
	if actualStatus != "bounced" {
		t.Fatalf("actual recipient status=%q want bounced", actualStatus)
	}
	for label, query := range map[string]string{
		"foreign recipient":   `SELECT count(*) FROM message_recipients WHERE message_id=$1 AND address='foreign@example.com'`,
		"foreign lifecycle":   `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND recipient='foreign@example.com'`,
		"foreign suppression": `SELECT count(*) FROM suppressions WHERE user_id=$1 AND address='foreign@example.com'`,
		"foreign event":       `SELECT count(*) FROM webhook_events WHERE message_id=$1 AND envelope->'data'->>'delivered_to'='foreign@example.com'`,
		"actual lifecycle":    `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND recipient='actual@example.com'`,
		"actual suppression":  `SELECT count(*) FROM suppressions WHERE user_id=$1 AND address='actual@example.com'`,
		"actual bounce event": `SELECT count(*) FROM webhook_events WHERE message_id=$1 AND type='email.bounced' AND envelope->'data'->>'delivered_to'='actual@example.com'`,
	} {
		arg := any(messageID)
		want := 0
		if label == "foreign suppression" || label == "actual suppression" {
			arg = userID
		}
		if label == "actual lifecycle" {
			want = 2
		}
		if label == "actual suppression" {
			want = 1
		}
		if label == "actual bounce event" {
			want = 1
		}
		var count int
		if err := pool.QueryRow(ctx, query, arg).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("%s rows=%d want %d", label, count, want)
		}
	}
}

func TestFeedbackRequiresProviderEventIdentityAndObservedTime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*delivery.Event)
	}{
		{"missing provider event id", func(event *delivery.Event) { event.ProviderEventID = "" }},
		{"missing observed time", func(event *delivery.Event) { event.OccurredAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.TestDB(t)
			ctx := context.Background()
			store := identity.NewStore(pool)
			_, messageID, agentEmail := seedOutbound(t, store, "invalid-identity-"+tc.name, "ses-invalid-identity-"+tc.name, []string{"a@example.com"})
			event := &delivery.Event{Kind: delivery.KindBounce, SESMessageID: "ses-invalid-identity-" + tc.name, ProviderEventID: "sns-valid", OccurredAt: time.Date(2026, 7, 21, 13, 1, 0, 0, time.UTC), BounceType: "permanent", Recipients: []delivery.RecipientOutcome{{Address: "a@example.com", Status: delivery.StatusBounced, Suppress: true}}}
			tc.mutate(event)
			if err := delivery.NewConsumer(store, nil).Process(ctx, event); err == nil {
				t.Fatal("Process succeeded, want fail-closed identity/time error")
			}
			if got := deliveryStatus(t, store, messageID, agentEmail); got != "sent" {
				t.Fatalf("message mutated to %q", got)
			}
			var lifecycleCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND stage IN ('delivery','suppression','complaint')`, messageID).Scan(&lifecycleCount); err != nil {
				t.Fatal(err)
			}
			if lifecycleCount != 0 {
				t.Fatalf("lifecycle rows=%d want 0", lifecycleCount)
			}
		})
	}
}

func TestFeedbackReconcilesCompletePreservedFallbackFirst(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	userID, messageID, agentEmail := seedOutbound(t, store, "feedback-fallback", "ses-feedback-fallback", []string{"a@example.com"})
	fallbackAt := time.Date(2026, 7, 21, 12, 55, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `DELETE FROM message_recipients WHERE message_id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET delivery_status='accepted',send_job_id=8123,provider_accepted_at=NULL,delivery_failure_source='local',delivery_failure_reason_code='submission.local_retries_exhausted',delivery_detail='preserved terminal failure',delivery_failure_occurred_at=$2,delivery_failure_attempt=4,delivery_failure_blocked_recipients=ARRAY['blocked@example.com'] WHERE id=$1`, messageID, fallbackAt); err != nil {
		t.Fatal(err)
	}
	event := &delivery.Event{Kind: delivery.KindDelivery, SESMessageID: "ses-feedback-fallback", ProviderEventID: "sns-feedback-fallback", OccurredAt: time.Date(2026, 7, 21, 13, 2, 0, 0, time.UTC), Recipients: []delivery.RecipientOutcome{{Address: "a@example.com", Status: delivery.StatusDelivered}}}
	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	finalizer := agent.NewOutboundSendStore(store, outbox, usage.NewUsageTracker(usage.NewStore(pool)))
	consumer := delivery.NewConsumer(store, transactionalDeliveryFirer(outbox), finalizer.FinalizeProviderAcceptedTx)
	if err := consumer.Process(ctx, event); err != nil {
		t.Fatal(err)
	}
	redelivery := *event
	redelivery.ProviderEventID = event.ProviderEventID + "-later"
	redelivery.OccurredAt = event.OccurredAt.Add(5 * time.Minute)
	if err := consumer.Process(ctx, &redelivery); err != nil {
		t.Fatalf("duplicate feedback: %v", err)
	}
	if got := deliveryStatus(t, store, messageID, agentEmail); got != "delivered" {
		t.Fatalf("status=%q want delivered", got)
	}
	var source, reason, detail string
	var occurredAt *time.Time
	var attempt *int
	var blocked []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(delivery_failure_source,''),COALESCE(delivery_failure_reason_code,''),COALESCE(delivery_detail,''),delivery_failure_occurred_at,delivery_failure_attempt,COALESCE(delivery_failure_blocked_recipients,'{}') FROM messages WHERE id=$1`, messageID).Scan(&source, &reason, &detail, &occurredAt, &attempt, &blocked); err != nil {
		t.Fatal(err)
	}
	if source != "" || reason != "" || detail != "" || occurredAt != nil || attempt != nil || len(blocked) != 0 {
		t.Fatalf("fallback provenance retained: source=%q reason=%q detail=%q occurred=%v attempt=%v blocked=%v", source, reason, detail, occurredAt, attempt, blocked)
	}
	var suppressionAt time.Time
	if err := pool.QueryRow(ctx, `SELECT occurred_at FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='suppression.recipient_blocked' AND recipient='blocked@example.com'`, messageID).Scan(&suppressionAt); err != nil {
		t.Fatal(err)
	}
	if !suppressionAt.Equal(fallbackAt) {
		t.Fatalf("preserved suppression occurred_at=%s want %s", suppressionAt, fallbackAt)
	}
	var acceptedAt, submissionAt time.Time
	if err := pool.QueryRow(ctx, `SELECT provider_accepted_at FROM messages WHERE id=$1`, messageID).Scan(&acceptedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT occurred_at FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.upstream_accepted'`, messageID).Scan(&submissionAt); err != nil {
		t.Fatal(err)
	}
	if !acceptedAt.Equal(event.OccurredAt) || !submissionAt.Equal(event.OccurredAt) {
		t.Fatalf("signed acceptance time message=%s lifecycle=%s want=%s", acceptedAt, submissionAt, event.OccurredAt)
	}
	for label, query := range map[string]string{
		"email.sent": `SELECT count(*) FROM webhook_events WHERE message_id=$1 AND type='email.sent'`,
		"usage":      `SELECT count(*) FROM usage_events WHERE user_id=$1 AND direction='outbound'`,
		"acceptance": `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.upstream_accepted'`,
	} {
		var count int
		arg := any(messageID)
		if label == "usage" {
			arg = userID
		}
		if err := pool.QueryRow(ctx, query, arg).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s rows=%d want 1", label, count)
		}
	}
}

func TestFeedbackFallbackFinalizationRollsBackAtomically(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	userID, messageID, _ := seedOutbound(t, store, "feedback-finalize-atomic", "ses-feedback-finalize-atomic", []string{"a@example.com"})
	fallbackAt := time.Date(2026, 7, 21, 13, 7, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `DELETE FROM message_recipients WHERE message_id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET delivery_status='accepted',send_job_id=9123,provider_accepted_at=NULL,delivery_failure_source='local',delivery_failure_reason_code='submission.local_retries_exhausted',delivery_detail='preserved',delivery_failure_occurred_at=$2,delivery_failure_attempt=3 WHERE id=$1`, messageID, fallbackAt); err != nil {
		t.Fatal(err)
	}
	event := &delivery.Event{Kind: delivery.KindDelivery, SESMessageID: "ses-feedback-finalize-atomic", ProviderEventID: "sns-feedback-finalize-atomic", OccurredAt: time.Date(2026, 7, 21, 13, 8, 0, 0, time.UTC), Recipients: []delivery.RecipientOutcome{{Address: "a@example.com", Status: delivery.StatusDelivered}}}
	if err := delivery.NewConsumer(store, nil).Process(ctx, event); err == nil {
		t.Fatal("feedback consumed pending acceptance without the canonical finalizer")
	}
	var preflightAcceptedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT provider_accepted_at FROM messages WHERE id=$1`, messageID).Scan(&preflightAcceptedAt); err != nil {
		t.Fatal(err)
	}
	if preflightAcceptedAt != nil {
		t.Fatalf("missing-finalizer attempt committed provider acceptance %v", preflightAcceptedAt)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION test_fail_feedback_after_sent() RETURNS trigger AS $f$ BEGIN IF NEW.type='email.delivered' THEN RAISE EXCEPTION 'forced feedback event failure'; END IF; RETURN NEW; END; $f$ LANGUAGE plpgsql; CREATE TRIGGER test_fail_feedback_after_sent BEFORE INSERT ON webhook_events FOR EACH ROW EXECUTE FUNCTION test_fail_feedback_after_sent()`); err != nil {
		t.Fatal(err)
	}
	drop := func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_fail_feedback_after_sent ON webhook_events; DROP FUNCTION IF EXISTS test_fail_feedback_after_sent()`)
	}
	t.Cleanup(drop)
	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	finalizer := agent.NewOutboundSendStore(store, outbox, usage.NewUsageTracker(usage.NewStore(pool)))
	consumer := delivery.NewConsumer(store, transactionalDeliveryFirer(outbox), finalizer.FinalizeProviderAcceptedTx)
	if err := consumer.Process(ctx, event); err == nil {
		t.Fatal("Process succeeded, want forced rollback")
	}
	var status, source string
	var acceptedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT delivery_status,COALESCE(delivery_failure_source,''),provider_accepted_at FROM messages WHERE id=$1`, messageID).Scan(&status, &source, &acceptedAt); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" || source != "local" || acceptedAt != nil {
		t.Fatalf("partial finalization status=%q source=%q accepted=%v", status, source, acceptedAt)
	}
	for label, query := range map[string]string{
		"events":    `SELECT count(*) FROM webhook_events WHERE user_id=$1`,
		"usage":     `SELECT count(*) FROM usage_events WHERE user_id=$1`,
		"lifecycle": `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code IN ('submission.upstream_accepted','delivery.recipient_server_accepted')`,
	} {
		arg := any(messageID)
		if label == "events" || label == "usage" {
			arg = userID
		}
		var count int
		if err := pool.QueryRow(ctx, query, arg).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows=%d want 0", label, count)
		}
	}
	drop()
	if err := consumer.Process(ctx, event); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestProviderDispositionOrdering(t *testing.T) {
	tests := []struct {
		name       string
		firstKind  delivery.EventKind
		first      delivery.Status
		secondKind delivery.EventKind
		second     delivery.Status
		want       string
	}{
		{"reject then bounce", delivery.KindReject, delivery.StatusFailed, delivery.KindBounce, delivery.StatusBounced, "bounced"},
		{"bounce then reject", delivery.KindBounce, delivery.StatusBounced, delivery.KindReject, delivery.StatusFailed, "bounced"},
		{"reject then complaint", delivery.KindReject, delivery.StatusFailed, delivery.KindComplaint, delivery.StatusComplained, "complained"},
		{"complaint then reject", delivery.KindComplaint, delivery.StatusComplained, delivery.KindReject, delivery.StatusFailed, "complained"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.TestDB(t)
			ctx := context.Background()
			store := identity.NewStore(pool)
			providerID := fmt.Sprintf("ses-disposition-%d", i)
			_, messageID, agentEmail := seedOutbound(t, store, fmt.Sprintf("disposition-%d", i), providerID, []string{"a@example.com"})
			consumer := delivery.NewConsumer(store, nil)
			makeEvent := func(kind delivery.EventKind, status delivery.Status, suffix string, minute int) *delivery.Event {
				return &delivery.Event{Kind: kind, SESMessageID: providerID, ProviderEventID: "sns-" + suffix, OccurredAt: time.Date(2026, 7, 21, 17, minute, 0, 0, time.UTC), BounceType: "permanent", Recipients: []delivery.RecipientOutcome{{Address: "a@example.com", Status: status, Detail: "provider disposition"}}}
			}
			if err := consumer.Process(ctx, makeEvent(tc.firstKind, tc.first, fmt.Sprintf("%d-first", i), i*2)); err != nil {
				t.Fatal(err)
			}
			if err := consumer.Process(ctx, makeEvent(tc.secondKind, tc.second, fmt.Sprintf("%d-second", i), i*2+1)); err != nil {
				t.Fatal(err)
			}
			if got := deliveryStatus(t, store, messageID, agentEmail); got != tc.want {
				t.Fatalf("status=%q want %q", got, tc.want)
			}
			var rejectionCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.provider_rejected'`, messageID).Scan(&rejectionCount); err != nil {
				t.Fatal(err)
			}
			if rejectionCount != 1 {
				t.Fatalf("provider rejection rows=%d want 1", rejectionCount)
			}
		})
	}
}

func TestConsumerDuplicateAndOutOfOrderFeedbackConverges(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	_, messageID, agentEmail := seedOutbound(t, store, "out-of-order", "ses-out-of-order", []string{"a@example.com"})
	consumer := delivery.NewConsumer(store, nil)
	bounce := &delivery.Event{Kind: delivery.KindBounce, SESMessageID: "ses-out-of-order", ProviderEventID: "sns-bounce", OccurredAt: time.Date(2026, 7, 21, 12, 2, 0, 0, time.UTC), BounceType: "permanent", Recipients: []delivery.RecipientOutcome{{Address: "a@example.com", Status: delivery.StatusBounced, Detail: "550", Suppress: true}}}
	lateDelivery := &delivery.Event{Kind: delivery.KindDelivery, SESMessageID: "ses-out-of-order", ProviderEventID: "sns-delivery", OccurredAt: time.Date(2026, 7, 21, 12, 1, 0, 0, time.UTC), Recipients: []delivery.RecipientOutcome{{Address: "a@example.com", Status: delivery.StatusDelivered, Detail: "250 accepted"}}}
	for _, event := range []*delivery.Event{bounce, bounce, lateDelivery, lateDelivery} {
		if err := consumer.Process(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if got := deliveryStatus(t, store, messageID, agentEmail); got != "bounced" {
		t.Fatalf("rollup=%q want bounced", got)
	}
	for _, reason := range []messagelifecycle.ReasonCode{messagelifecycle.ReasonDeliveryPermanentBounce, messagelifecycle.ReasonSuppressionHardBounceApplied, messagelifecycle.ReasonDeliveryRecipientServerAccepted} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code=$2`, messageID, reason).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("reason %s count=%d want 1", reason, count)
		}
	}
}

func TestFeedbackAtomicRollbackOnEventInsertFailure(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	userID, messageID, agentEmail := seedOutbound(t, store, "feedback-atomic", "ses-feedback-atomic", []string{"a@example.com"})
	if _, err := pool.Exec(ctx, `UPDATE messages SET provider_accepted_at=NULL WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `
		CREATE FUNCTION test_fail_feedback_event() RETURNS trigger AS $f$
		BEGIN
			IF NEW.type='email.bounced' THEN RAISE EXCEPTION 'forced feedback event failure'; END IF;
			RETURN NEW;
		END;
		$f$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_feedback_event BEFORE INSERT ON webhook_events
		FOR EACH ROW EXECUTE FUNCTION test_fail_feedback_event();`)
	if err != nil {
		t.Fatal(err)
	}
	dropFailure := func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_fail_feedback_event ON webhook_events; DROP FUNCTION IF EXISTS test_fail_feedback_event()`)
	}
	t.Cleanup(dropFailure)

	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	consumer := delivery.NewConsumer(store, transactionalDeliveryFirer(outbox))
	ev := &delivery.Event{Kind: delivery.KindBounce, SESMessageID: "ses-feedback-atomic", ProviderEventID: "sns-feedback-atomic", OccurredAt: time.Date(2026, 7, 21, 13, 6, 0, 0, time.UTC), BounceType: "permanent", Recipients: []delivery.RecipientOutcome{{Address: "a@example.com", Status: delivery.StatusBounced, Detail: "550", Suppress: true}}}
	if err := consumer.Process(ctx, ev); err == nil {
		t.Fatal("Process succeeded, want forced event insertion failure")
	}
	if got := deliveryStatus(t, store, messageID, agentEmail); got != "sent" {
		t.Fatalf("message status committed despite event failure: %q", got)
	}
	var recipientStatus string
	var acceptedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT r.status,m.provider_accepted_at FROM message_recipients r JOIN messages m ON m.id=r.message_id WHERE r.message_id=$1 AND r.address='a@example.com'`, messageID).Scan(&recipientStatus, &acceptedAt); err != nil {
		t.Fatal(err)
	}
	if recipientStatus != "sent" || acceptedAt != nil {
		t.Fatalf("recipient/provider evidence committed: status=%q accepted_at=%v", recipientStatus, acceptedAt)
	}
	for label, query := range map[string]string{
		"lifecycle":   `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code IN ('delivery.permanent_bounce','suppression.hard_bounce_applied')`,
		"suppression": `SELECT count(*) FROM suppressions WHERE user_id=$1`,
		"event":       `SELECT count(*) FROM webhook_events WHERE user_id=$1`,
	} {
		arg := any(userID)
		if label == "lifecycle" {
			arg = messageID
		}
		var count int
		if err := pool.QueryRow(ctx, query, arg).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows=%d want 0 after rollback", label, count)
		}
	}

	dropFailure()
	if err := consumer.Process(ctx, ev); err != nil {
		t.Fatalf("retry after removing failure: %v", err)
	}
	if got := deliveryStatus(t, store, messageID, agentEmail); got != "bounced" {
		t.Fatalf("retry status=%q want bounced", got)
	}
}

// seedOutbound creates a user + verified domain + agent + one outbound message
// with the given SES provider id, marked sent to `to`. Returns userID,
// messageID, agentEmail.
func seedOutbound(t *testing.T, store *identity.Store, prefix, providerID string, to []string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "owner-"+prefix+"@example.com", "Owner", "g-"+prefix)
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
	agentEmail := "bot@" + domain
	if _, err := store.CreateAgent(ctx, agentEmail, domain, "", "", "", user.ID); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	msg, err := store.CreateOutboundMessage(ctx, agentEmail, to, nil, nil, "Subj", "send", "smtp", providerID, "", nil)
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}
	if err := store.MarkMessageSent(ctx, msg.ID, "relay", to, nil, nil); err != nil {
		t.Fatalf("MarkMessageSent: %v", err)
	}
	return user.ID, msg.ID, agentEmail
}

func deliveryStatus(t *testing.T, store *identity.Store, messageID, agentEmail string) string {
	t.Helper()
	msg, err := store.GetMessageWithContent(context.Background(), messageID, agentEmail)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	return msg.DeliveryStatus
}

func TestDeliveryPipeline_DeliveredThenBounceSuppresses(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	userID, msgID, agentEmail := seedOutbound(t, store, "dlv", "ses-msg-1", []string{"a@x.com"})

	// Initial rollup is 'sent'.
	if got := deliveryStatus(t, store, msgID, agentEmail); got != "sent" {
		t.Fatalf("initial delivery_status=%q, want sent", got)
	}

	consumer := delivery.NewConsumer(store, nil)

	// Delivery → delivered.
	if err := consumer.Process(ctx, &delivery.Event{
		Kind: delivery.KindDelivery, SESMessageID: "ses-msg-1", ProviderEventID: "sns-msg-1-delivery", OccurredAt: time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC),
		Recipients: []delivery.RecipientOutcome{{Address: "a@x.com", Status: delivery.StatusDelivered}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := deliveryStatus(t, store, msgID, agentEmail); got != "delivered" {
		t.Fatalf("after delivery: delivery_status=%q, want delivered", got)
	}

	// A later, lower-rank deferred must NOT regress a delivered rollup (monotonic).
	_ = consumer.Process(ctx, &delivery.Event{
		Kind: delivery.KindDeliveryDelay, SESMessageID: "ses-msg-1", ProviderEventID: "sns-msg-1-delay", OccurredAt: time.Date(2026, 7, 21, 14, 1, 0, 0, time.UTC),
		Recipients: []delivery.RecipientOutcome{{Address: "a@x.com", Status: delivery.StatusDeferred}},
	})
	if got := deliveryStatus(t, store, msgID, agentEmail); got != "delivered" {
		t.Fatalf("monotonic violated: delivery_status=%q, want still delivered", got)
	}

	// A hard bounce wins over delivered AND suppresses the address.
	if err := consumer.Process(ctx, &delivery.Event{
		Kind: delivery.KindBounce, SESMessageID: "ses-msg-1", ProviderEventID: "sns-msg-1-bounce", OccurredAt: time.Date(2026, 7, 21, 14, 2, 0, 0, time.UTC), BounceType: "permanent",
		Recipients: []delivery.RecipientOutcome{{Address: "a@x.com", Status: delivery.StatusBounced, Detail: "550", Suppress: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := deliveryStatus(t, store, msgID, agentEmail); got != "bounced" {
		t.Fatalf("after bounce: delivery_status=%q, want bounced", got)
	}
	supp, err := store.SuppressedAddresses(ctx, userID, []string{"a@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(supp) != 1 || supp[0] != "a@x.com" {
		t.Fatalf("expected a@x.com suppressed, got %v", supp)
	}
}

func TestDeliveryPipeline_PerRecipientRollup(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	_, msgID, agentEmail := seedOutbound(t, store, "multi", "ses-msg-2", []string{"good@x.com", "bad@x.com"})
	consumer := delivery.NewConsumer(store, nil)

	// good delivers, bad bounces → rollup is the worst (bounced).
	_ = consumer.Process(ctx, &delivery.Event{Kind: delivery.KindDelivery, SESMessageID: "ses-msg-2", ProviderEventID: "sns-msg-2-delivery", OccurredAt: time.Date(2026, 7, 21, 14, 3, 0, 0, time.UTC),
		Recipients: []delivery.RecipientOutcome{{Address: "good@x.com", Status: delivery.StatusDelivered}}})
	_ = consumer.Process(ctx, &delivery.Event{Kind: delivery.KindBounce, SESMessageID: "ses-msg-2", ProviderEventID: "sns-msg-2-bounce", OccurredAt: time.Date(2026, 7, 21, 14, 4, 0, 0, time.UTC), BounceType: "permanent",
		Recipients: []delivery.RecipientOutcome{{Address: "bad@x.com", Status: delivery.StatusBounced, Suppress: true}}})

	if got := deliveryStatus(t, store, msgID, agentEmail); got != "bounced" {
		t.Fatalf("rollup=%q, want bounced (worst across recipients)", got)
	}
}

func TestSuppressionCRUD(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, _ := store.CreateOrGetUser(ctx, "supp@example.com", "S", "g-supp")

	added, err := store.AddSuppression(ctx, user.ID, "X@x.com", "manual block", "manual", "")
	if err != nil || !added {
		t.Fatalf("AddSuppression added=%v err=%v", added, err)
	}
	// Idempotent: second add returns added=false.
	added2, _ := store.AddSuppression(ctx, user.ID, "x@x.com", "again", "manual", "")
	if added2 {
		t.Fatal("re-adding the same (normalized) address should return added=false")
	}
	list, _ := store.ListSuppressions(ctx, user.ID, 50, time.Time{}, "")
	if len(list) != 1 || list[0].Address != "x@x.com" {
		t.Fatalf("list=%v", list)
	}
	found, _ := store.RemoveSuppression(ctx, user.ID, "x@x.com")
	if !found {
		t.Fatal("RemoveSuppression should report found")
	}
	if supp, _ := store.SuppressedAddresses(ctx, user.ID, []string{"x@x.com"}); len(supp) != 0 {
		t.Fatalf("address should be un-suppressed, got %v", supp)
	}
}

// TestCorrelationMatchesBracketedSESID pins the review BLOCKER: SES stores the
// provider id angle-bracketed (and sometimes @region.amazonses.com-suffixed),
// but the SNS notification carries the BARE id. Correlation must match across
// those shapes — an exact-equality match would silently drop all feedback.
func TestCorrelationMatchesBracketedSESID(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	// Store the realistic bracketed+suffixed shape; correlate with the bare id.
	userID, msgID, agentEmail := seedOutbound(t, store, "corr", "<010f0193abc-000000@us-east-2.amazonses.com>", []string{"a@x.com"})

	m, found, err := store.CorrelateBySESMessageID(ctx, "010f0193abc-000000")
	if err != nil {
		t.Fatal(err)
	}
	if !found || m.MessageID != msgID {
		t.Fatalf("bare-id correlation against bracketed stored id failed: found=%v m=%+v want id %q", found, m, msgID)
	}
	// The correlation result carries the message fields the canonical event
	// payloads need — locked here against the seeded row.
	if m.UserID != userID || m.AgentID != agentEmail || m.Subject != "Subj" ||
		m.From != agentEmail || m.Method != "smtp" || m.MessageType != "send" {
		t.Fatalf("correlated fields wrong: %+v", m)
	}
	if len(m.To) != 1 || m.To[0] != "a@x.com" || len(m.CC) != 0 || len(m.BCC) != 0 {
		t.Fatalf("correlated recipient lists wrong: %+v", m)
	}

	// And the full pipeline must transition delivery_status via the bare id.
	if err := delivery.NewConsumer(store, nil).Process(ctx, &delivery.Event{
		Kind: delivery.KindDelivery, SESMessageID: "010f0193abc-000000", ProviderEventID: "sns-bracketed", OccurredAt: time.Date(2026, 7, 21, 14, 5, 0, 0, time.UTC),
		Recipients: []delivery.RecipientOutcome{{Address: "a@x.com", Status: delivery.StatusDelivered}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := deliveryStatus(t, store, msgID, agentEmail); got != "delivered" {
		t.Fatalf("delivery_status=%q, want delivered (bare-id correlation)", got)
	}
}

// TestDeliveryPipeline_RejectFailsMessageOnceWithoutSuppression drives the SES
// Reject path end-to-end against the real store: a correlated Reject must (1)
// transition the sent rollup to failed, (2) fire exactly ONE message-level
// email.failed with the canonical correlated payload, (3) never suppress, and
// (4) stay idempotent under duplicate SNS delivery (same dedup key, monotonic
// no-op status writes).
func TestDeliveryPipeline_RejectFailsMessageOnceWithoutSuppression(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	userID, msgID, agentEmail := seedOutbound(t, store, "rej", "ses-reject-1", []string{"a@x.com", "b@x.com"})
	if got := deliveryStatus(t, store, msgID, agentEmail); got != "sent" {
		t.Fatalf("initial delivery_status=%q, want sent", got)
	}

	type fired struct {
		typ, dedup, messageID, conversationID string
		data                                  any
	}
	var events []fired
	consumer := delivery.NewConsumer(store, func(_ context.Context, _ pgx.Tx, e delivery.FiredEvent) error {
		events = append(events, fired{e.Type, e.DedupKey, e.MessageID, e.ConversationID, e.Data})
		return nil
	})

	rejectEv := &delivery.Event{
		Kind: delivery.KindReject, SESMessageID: "ses-reject-1", ProviderEventID: "sns-reject-1", OccurredAt: time.Date(2026, 7, 21, 13, 3, 0, 0, time.UTC),
		Recipients: []delivery.RecipientOutcome{
			{Address: "a@x.com", Status: delivery.StatusFailed, Detail: "Bad content"},
			{Address: "b@x.com", Status: delivery.StatusFailed, Detail: "Bad content"},
		},
	}
	if err := consumer.Process(ctx, rejectEv); err != nil {
		t.Fatal(err)
	}

	// sent → failed on the message rollup, and both recipient rows failed.
	if got := deliveryStatus(t, store, msgID, agentEmail); got != "failed" {
		t.Fatalf("delivery_status=%q, want failed after SES Reject", got)
	}
	var failedRows int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM message_recipients WHERE message_id=$1 AND status='failed'", msgID,
	).Scan(&failedRows); err != nil {
		t.Fatal(err)
	}
	if failedRows != 2 {
		t.Fatalf("failed recipient rows=%d, want 2", failedRows)
	}

	// Exactly ONE message-level email.failed — not one per recipient.
	if len(events) != 1 || events[0].typ != "email.failed" {
		t.Fatalf("events=%+v, want exactly one email.failed", events)
	}
	// Envelope carries the message routing key (findability via ?message_id=).
	if events[0].messageID != msgID {
		t.Fatalf("email.failed envelope messageID=%q, want %q", events[0].messageID, msgID)
	}
	data, ok := events[0].data.(eventpayload.EmailFailedData)
	if !ok {
		t.Fatalf("data is not the canonical typed payload: %T", events[0].data)
	}
	if data.MessageID != msgID || data.AgentEmail != agentEmail || data.Direction != "outbound" ||
		data.From != agentEmail || data.Subject != "Subj" || data.Method != "smtp" ||
		data.MessageType != "send" || data.Reason != "Bad content" {
		t.Fatalf("payload=%+v", data)
	}
	if len(data.To) != 2 || data.To[0] != "a@x.com" || data.To[1] != "b@x.com" {
		t.Fatalf("payload to=%v, want both recipients from the correlated row", data.To)
	}
	if data.ReasonCode != string(messagelifecycle.ReasonSubmissionProviderRejected) || data.Retryable == nil || *data.Retryable {
		t.Fatalf("payload reason_code=%q retryable=%v", data.ReasonCode, data.Retryable)
	}
	if len(data.LifecycleTransitions) != 1 || data.LifecycleTransitions[0].ReasonCode != messagelifecycle.ReasonSubmissionProviderRejected {
		t.Fatalf("email.failed lifecycle=%+v, want one provider rejection", data.LifecycleTransitions)
	}

	// A Reject must never suppress the recipient addresses.
	supp, err := store.SuppressedAddresses(ctx, userID, []string{"a@x.com", "b@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(supp) != 0 {
		t.Fatalf("SES Reject must not suppress, got %v", supp)
	}

	// Duplicate SNS delivery: status stays failed, refire carries the SAME
	// dedup key (the outbox collapses it via the deterministic event id).
	if err := consumer.Process(ctx, rejectEv); err != nil {
		t.Fatal(err)
	}
	if got := deliveryStatus(t, store, msgID, agentEmail); got != "failed" {
		t.Fatalf("delivery_status=%q after duplicate, want failed", got)
	}
	if len(events) != 2 || events[1].typ != "email.failed" || events[1].dedup != events[0].dedup {
		t.Fatalf("duplicate delivery must refire with an identical dedup key: %+v", events)
	}
	var rejectionTransitions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.provider_rejected'`, msgID).Scan(&rejectionTransitions); err != nil {
		t.Fatal(err)
	}
	if rejectionTransitions != 1 {
		t.Fatalf("provider rejection transitions=%d want 1", rejectionTransitions)
	}
	var source, reasonCode, detail string
	var rejectionAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT COALESCE(delivery_failure_source,''),COALESCE(delivery_failure_reason_code,''),COALESCE(delivery_detail,''),delivery_failure_occurred_at FROM messages WHERE id=$1`, msgID).Scan(&source, &reasonCode, &detail, &rejectionAt); err != nil {
		t.Fatal(err)
	}
	if source != "provider" || reasonCode != string(messagelifecycle.ReasonSubmissionProviderRejected) || detail != "Bad content" || rejectionAt == nil || !rejectionAt.Equal(rejectEv.OccurredAt) {
		t.Fatalf("provider provenance source=%q reason=%q detail=%q occurred=%v", source, reasonCode, detail, rejectionAt)
	}
}

func TestRejectOverridesLocalFailureAndLaterDeliveryCannotRevive(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	_, messageID, agentEmail := seedOutbound(t, store, "reject-local", "ses-reject-local", []string{"a@example.com"})
	if _, err := pool.Exec(ctx, `UPDATE messages SET delivery_status='failed',delivery_failure_source='local',delivery_failure_reason_code='submission.local_retries_exhausted',delivery_detail='locally inferred' WHERE id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE message_recipients SET status='failed',detail='locally inferred' WHERE message_id=$1`, messageID); err != nil {
		t.Fatal(err)
	}
	reject := &delivery.Event{Kind: delivery.KindReject, SESMessageID: "ses-reject-local", ProviderEventID: "sns-reject-local", OccurredAt: time.Date(2026, 7, 21, 13, 4, 0, 0, time.UTC), Recipients: []delivery.RecipientOutcome{{Address: "a@example.com", Status: delivery.StatusFailed, Detail: "Bad content"}}}
	consumer := delivery.NewConsumer(store, nil)
	if err := consumer.Process(ctx, reject); err != nil {
		t.Fatal(err)
	}
	var status, source, reason, detail string
	var rejectionAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT delivery_status,COALESCE(delivery_failure_source,''),COALESCE(delivery_failure_reason_code,''),COALESCE(delivery_detail,''),delivery_failure_occurred_at FROM messages WHERE id=$1`, messageID).Scan(&status, &source, &reason, &detail, &rejectionAt); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || source != "provider" || reason != string(messagelifecycle.ReasonSubmissionProviderRejected) || detail != "Bad content" || rejectionAt == nil || !rejectionAt.Equal(reject.OccurredAt) {
		t.Fatalf("after Reject status=%q source=%q reason=%q detail=%q occurred=%v", status, source, reason, detail, rejectionAt)
	}
	lateDelivery := &delivery.Event{Kind: delivery.KindDelivery, SESMessageID: "ses-reject-local", ProviderEventID: "sns-reject-local-late-delivery", OccurredAt: time.Date(2026, 7, 21, 13, 5, 0, 0, time.UTC), Recipients: []delivery.RecipientOutcome{{Address: "a@example.com", Status: delivery.StatusDelivered}}}
	if err := consumer.Process(ctx, lateDelivery); err != nil {
		t.Fatal(err)
	}
	if got := deliveryStatus(t, store, messageID, agentEmail); got != "failed" {
		t.Fatalf("late delivery revived provider rejection to %q", got)
	}
}

// TestConcurrentRollupMonotonic pins the review race fix: concurrent SES events
// for the same recipient (one delivered, one bounced) must converge to the
// worst status (bounced) — the message-row lock serializes the merge.
func TestConcurrentRollupMonotonic(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	consumer := delivery.NewConsumer(store, nil)

	// Seed the user/domain/agent once; create a distinct message per iteration.
	_, _, agentEmail := seedOutbound(t, store, "race", "ses-race-seed", []string{"a@x.com"})

	for i := 0; i < 25; i++ {
		providerID := "ses-race-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		msg, err := store.CreateOutboundMessage(ctx, agentEmail, []string{"a@x.com"}, nil, nil, "S", "send", "smtp", providerID, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.MarkMessageSent(ctx, msg.ID, "relay", []string{"a@x.com"}, nil, nil); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 2)
		go func() {
			done <- consumer.Process(ctx, &delivery.Event{Kind: delivery.KindDelivery, SESMessageID: providerID, ProviderEventID: providerID + "-delivery", OccurredAt: time.Date(2026, 7, 21, 15, i, 0, 0, time.UTC),
				Recipients: []delivery.RecipientOutcome{{Address: "a@x.com", Status: delivery.StatusDelivered}}})
		}()
		go func() {
			done <- consumer.Process(ctx, &delivery.Event{Kind: delivery.KindBounce, SESMessageID: providerID, ProviderEventID: providerID + "-bounce", OccurredAt: time.Date(2026, 7, 21, 15, i, 1, 0, time.UTC), BounceType: "permanent",
				Recipients: []delivery.RecipientOutcome{{Address: "a@x.com", Status: delivery.StatusBounced, Detail: "550"}}})
		}()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := deliveryStatus(t, store, msg.ID, agentEmail); got != "bounced" {
			t.Fatalf("iter %d: rollup=%q, want bounced (concurrent monotonic)", i, got)
		}
	}
}

// TestDetailNotClobberedByLaterEvent pins the review low fix: a later
// lower-rank event carrying a detail must not overwrite the terminal
// diagnostic.
func TestDetailNotClobberedByLaterEvent(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	_, msgID, _ := seedOutbound(t, store, "detail", "ses-detail", []string{"a@x.com"})
	consumer := delivery.NewConsumer(store, nil)

	_ = consumer.Process(ctx, &delivery.Event{Kind: delivery.KindBounce, SESMessageID: "ses-detail", ProviderEventID: "sns-detail-bounce", OccurredAt: time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC), BounceType: "permanent",
		Recipients: []delivery.RecipientOutcome{{Address: "a@x.com", Status: delivery.StatusBounced, Detail: "550 mailbox full"}}})
	// A late delivered (lower rank) with its own detail must not clobber.
	_ = consumer.Process(ctx, &delivery.Event{Kind: delivery.KindDelivery, SESMessageID: "ses-detail", ProviderEventID: "sns-detail-delivery", OccurredAt: time.Date(2026, 7, 21, 16, 1, 0, 0, time.UTC),
		Recipients: []delivery.RecipientOutcome{{Address: "a@x.com", Status: delivery.StatusDelivered, Detail: "ok"}}})

	var detail string
	if err := pool.QueryRow(ctx, "SELECT COALESCE(detail,'') FROM message_recipients WHERE message_id=$1 AND address='a@x.com'", msgID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != "550 mailbox full" {
		t.Fatalf("detail=%q, want the preserved bounce reason", detail)
	}
}

// TestCrashWindowCorrectionEndToEnd drives the full §3.1 correction chain on a
// real store: signed provider feedback corrects a locally failed outbound row
// by first running the canonical sent finalizer and then applying delivery, all
// in one transaction. Re-delivery must not duplicate any logical side effect.
func TestCrashWindowCorrectionEndToEnd(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	// Seed WITHOUT a provider id and WITHOUT MarkMessageSent — the
	// SMTP-accept↔mark-sent crash window shape.
	user, err := store.CreateOrGetUser(ctx, "owner-crashwin@example.com", "Owner", "g-crashwin")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain := "crashwin.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	agentEmail := "bot@" + domain
	if _, err := store.CreateAgent(ctx, agentEmail, domain, "", "", "", user.ID); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	msg, err := store.CreateOutboundMessage(ctx, agentEmail, []string{"a@x.com"}, nil, nil, "Subj", "send", "smtp", "", "", nil)
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}
	// Complete locally inferred terminal failure (reconciler/final-attempt
	// shape), including suppression evidence which the sent finalizer must
	// preserve before clearing the fallback provenance.
	fallbackAt := time.Date(2026, 7, 21, 16, 1, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET delivery_status='failed', delivery_detail='send job discarded',
		        send_job_id=12345, delivery_failure_source='local',
		        delivery_failure_reason_code='submission.local_retries_exhausted',
		        delivery_failure_occurred_at=$2, delivery_failure_attempt=5,
		        delivery_failure_blocked_recipients=ARRAY['blocked@example.com']
		  WHERE id=$1`, msg.ID, fallbackAt); err != nil {
		t.Fatalf("force local failed: %v", err)
	}

	// The raw SES notification with the header echo (what /webhooks/ses hands
	// to ParseSESNotification after SNS signature verification).
	ev, err := delivery.ParseSESNotification([]byte(`{
		"eventType": "Delivery",
		"mail": {
			"messageId": "ses-crashwin-1",
			"destination": ["a@x.com"],
			"headers": [
				{"name": "From", "value": "` + agentEmail + `"},
				{"name": "X-E2A-Message-ID", "value": "` + msg.ID + `"}
			]
		},
		"delivery": {"recipients": ["a@x.com"]}
	}`))
	if err != nil {
		t.Fatalf("ParseSESNotification: %v", err)
	}
	ev.ProviderEventID = "sns-crashwin-1"
	ev.OccurredAt = time.Date(2026, 7, 21, 16, 2, 0, 0, time.UTC)

	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	finalizer := agent.NewOutboundSendStore(store, outbox, usage.NewUsageTracker(usage.NewStore(pool)))
	consumer := delivery.NewConsumer(store, transactionalDeliveryFirer(outbox), finalizer.FinalizeProviderAcceptedTx)

	// A downstream feedback publication failure must roll back the provider
	// evidence and every preceding canonical-sent side effect.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION test_fail_crashwin_feedback() RETURNS trigger AS $f$ BEGIN IF NEW.type='email.delivered' THEN RAISE EXCEPTION 'forced crash-window feedback failure'; END IF; RETURN NEW; END; $f$ LANGUAGE plpgsql; CREATE TRIGGER test_fail_crashwin_feedback BEFORE INSERT ON webhook_events FOR EACH ROW EXECUTE FUNCTION test_fail_crashwin_feedback()`); err != nil {
		t.Fatal(err)
	}
	dropFailure := func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_fail_crashwin_feedback ON webhook_events; DROP FUNCTION IF EXISTS test_fail_crashwin_feedback()`)
	}
	t.Cleanup(dropFailure)
	if err := consumer.Process(ctx, ev); err == nil {
		t.Fatal("Process succeeded, want injected downstream failure")
	}
	var rollbackStatus, rollbackSource, rollbackReason, rollbackDetail string
	var rollbackAcceptedAt *time.Time
	var rollbackOccurredAt *time.Time
	var rollbackAttempt *int
	var rollbackBlocked []string
	if err := pool.QueryRow(ctx, `SELECT delivery_status,COALESCE(delivery_failure_source,''),COALESCE(delivery_failure_reason_code,''),COALESCE(delivery_detail,''),provider_accepted_at,delivery_failure_occurred_at,delivery_failure_attempt,COALESCE(delivery_failure_blocked_recipients,'{}') FROM messages WHERE id=$1`, msg.ID).Scan(&rollbackStatus, &rollbackSource, &rollbackReason, &rollbackDetail, &rollbackAcceptedAt, &rollbackOccurredAt, &rollbackAttempt, &rollbackBlocked); err != nil {
		t.Fatal(err)
	}
	if rollbackStatus != "failed" || rollbackSource != "local" || rollbackReason != "submission.local_retries_exhausted" || rollbackDetail != "send job discarded" || rollbackAcceptedAt != nil || rollbackOccurredAt == nil || !rollbackOccurredAt.Equal(fallbackAt) || rollbackAttempt == nil || *rollbackAttempt != 5 || len(rollbackBlocked) != 1 || rollbackBlocked[0] != "blocked@example.com" {
		t.Fatalf("partial rollback state: status=%q source=%q reason=%q detail=%q accepted=%v occurred=%v attempt=%v blocked=%v", rollbackStatus, rollbackSource, rollbackReason, rollbackDetail, rollbackAcceptedAt, rollbackOccurredAt, rollbackAttempt, rollbackBlocked)
	}
	for label, query := range map[string]string{
		"events":      `SELECT count(*) FROM webhook_events WHERE message_id=$1 AND type IN ('email.sent','email.delivered')`,
		"usage":       `SELECT count(*) FROM usage_events WHERE user_id=$1 AND direction='outbound'`,
		"acceptance":  `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.upstream_accepted'`,
		"suppression": `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='suppression.recipient_blocked'`,
	} {
		arg := any(msg.ID)
		if label == "usage" {
			arg = user.ID
		}
		var count int
		if err := pool.QueryRow(ctx, query, arg).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("rollback %s rows=%d want 0", label, count)
		}
	}

	dropFailure()
	if err := consumer.Process(ctx, ev); err != nil {
		t.Fatalf("Process retry: %v", err)
	}

	var status, detail, source, reason, providerID string
	var acceptedAt time.Time
	var occurredAt *time.Time
	var attempt *int
	var blocked []string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(delivery_status,''), COALESCE(delivery_detail,''),
		        COALESCE(delivery_failure_source,''), COALESCE(delivery_failure_reason_code,''),
		        COALESCE(provider_message_id,''), provider_accepted_at,
		        delivery_failure_occurred_at,delivery_failure_attempt,
		        COALESCE(delivery_failure_blocked_recipients,'{}')
		   FROM messages WHERE id=$1`, msg.ID,
	).Scan(&status, &detail, &source, &reason, &providerID, &acceptedAt, &occurredAt, &attempt, &blocked); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "delivered" {
		t.Errorf("delivery_status=%q, want delivered (§3.1 correction)", status)
	}
	if source != "" || reason != "" || detail != "" || occurredAt != nil || attempt != nil || len(blocked) != 0 {
		t.Errorf("fallback provenance retained: source=%q reason=%q detail=%q occurred=%v attempt=%v blocked=%v", source, reason, detail, occurredAt, attempt, blocked)
	}
	if providerID != "ses-crashwin-1" {
		t.Errorf("provider_message_id=%q, want repaired from the notification", providerID)
	}
	if !acceptedAt.Equal(ev.OccurredAt) {
		t.Errorf("provider_accepted_at=%s want signed observation %s", acceptedAt, ev.OccurredAt)
	}

	var acceptanceID string
	var acceptanceAt time.Time
	if err := pool.QueryRow(ctx, `SELECT id,occurred_at FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.upstream_accepted'`, msg.ID).Scan(&acceptanceID, &acceptanceAt); err != nil {
		t.Fatal(err)
	}
	if !acceptanceAt.Equal(ev.OccurredAt) {
		t.Fatalf("acceptance occurred_at=%s want %s", acceptanceAt, ev.OccurredAt)
	}
	var suppressionAt time.Time
	var suppressionJobID string
	if err := pool.QueryRow(ctx, `SELECT occurred_at,correlation_ids->>'job_id' FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='suppression.recipient_blocked' AND recipient='blocked@example.com'`, msg.ID).Scan(&suppressionAt, &suppressionJobID); err != nil {
		t.Fatal(err)
	}
	if !suppressionAt.Equal(fallbackAt) || suppressionJobID != "12345" {
		t.Fatalf("preserved suppression occurred=%s job=%q want %s/12345", suppressionAt, suppressionJobID, fallbackAt)
	}
	var sentEnvelope []byte
	if err := pool.QueryRow(ctx, `SELECT envelope FROM webhook_events WHERE message_id=$1 AND type='email.sent'`, msg.ID).Scan(&sentEnvelope); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Data struct {
			LifecycleTransitions []messagelifecycle.MessageLifecycleTransition `json:"lifecycle_transitions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(sentEnvelope, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Data.LifecycleTransitions) != 1 || sent.Data.LifecycleTransitions[0].ID != acceptanceID {
		t.Fatalf("email.sent lifecycle=%+v want only acceptance %s", sent.Data.LifecycleTransitions, acceptanceID)
	}

	// Replay of the same notification is idempotent.
	if err := consumer.Process(ctx, ev); err != nil {
		t.Fatalf("replay Process: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(delivery_status,'') FROM messages WHERE id=$1`, msg.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" {
		t.Errorf("after replay delivery_status=%q, want delivered", status)
	}
	for label, query := range map[string]string{
		"email.sent":  `SELECT count(*) FROM webhook_events WHERE message_id=$1 AND type='email.sent'`,
		"delivered":   `SELECT count(*) FROM webhook_events WHERE message_id=$1 AND type='email.delivered'`,
		"usage":       `SELECT count(*) FROM usage_events WHERE user_id=$1 AND direction='outbound'`,
		"acceptance":  `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='submission.upstream_accepted'`,
		"delivery":    `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='delivery.recipient_server_accepted'`,
		"suppression": `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id=$1 AND reason_code='suppression.recipient_blocked'`,
	} {
		arg := any(msg.ID)
		if label == "usage" {
			arg = user.ID
		}
		var count int
		if err := pool.QueryRow(ctx, query, arg).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("after replay %s rows=%d want 1", label, count)
		}
	}
}
