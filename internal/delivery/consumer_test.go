package delivery

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/eventpayload/goldenassert"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

var testFeedbackOccurredAt = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

// fakeConsumerStore is an in-memory delivery.Store.
type fakeConsumerStore struct {
	// correlation: sesMessageID → correlated message
	corr map[string]*CorrelatedMessage
	// header correlation: e2aMessageID → correlated message
	corrByE2A map[string]*CorrelatedMessage
	// e2a-id lookups actually attempted (pins the shape-validation gate)
	e2aLookups []string
	// evidence: recorded provider-accept evidence {messageID, sesMessageID}
	evidence [][2]string
	// recorded outcomes + suppressions
	outcomes    [][3]string // {messageID, address, status}
	suppressed  map[string]bool
	suppressErr error
	addSuppErr  error
	alreadySupp map[string]bool // (user|address) already suppressed → added=false
	pending     bool
	applicable  bool
	preflights  int
}

func newFakeConsumerStore() *fakeConsumerStore {
	return &fakeConsumerStore{corr: map[string]*CorrelatedMessage{}, corrByE2A: map[string]*CorrelatedMessage{}, suppressed: map[string]bool{}, alreadySupp: map[string]bool{}, applicable: true}
}

func (f *fakeConsumerStore) CorrelateBySESMessageID(ctx context.Context, id string) (*CorrelatedMessage, bool, error) {
	m, ok := f.corr[id]
	return m, ok, nil
}

func (f *fakeConsumerStore) CorrelateByE2AMessageID(ctx context.Context, id string) (*CorrelatedMessage, bool, error) {
	f.e2aLookups = append(f.e2aLookups, id)
	m, ok := f.corrByE2A[id]
	return m, ok, nil
}

func (f *fakeConsumerStore) RecordProviderAcceptEvidence(ctx context.Context, messageID, sesMessageID string) error {
	f.evidence = append(f.evidence, [2]string{messageID, sesMessageID})
	return nil
}
func (f *fakeConsumerStore) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return fn(nil)
}
func (f *fakeConsumerStore) HasApplicableRecipientTx(context.Context, pgx.Tx, string, []string) (bool, error) {
	f.preflights++
	return f.applicable, nil
}
func (f *fakeConsumerStore) RecordProviderAcceptEvidenceTx(ctx context.Context, _ pgx.Tx, messageID, sesMessageID string, _ time.Time) error {
	return f.RecordProviderAcceptEvidence(ctx, messageID, sesMessageID)
}
func (f *fakeConsumerStore) ProviderAcceptancePendingTx(context.Context, pgx.Tx, string) (bool, error) {
	return f.pending, nil
}
func (f *fakeConsumerStore) RecordProviderRejectTx(context.Context, pgx.Tx, string, string, time.Time) error {
	return nil
}
func (f *fakeConsumerStore) RecordDeliveryOutcomeTx(ctx context.Context, _ pgx.Tx, messageID, address string, st Status, detail string) (bool, error) {
	err := f.RecordDeliveryOutcome(ctx, messageID, address, st, detail)
	return err == nil, err
}
func (f *fakeConsumerStore) AddSuppressionTx(ctx context.Context, _ pgx.Tx, userID, address, reason, source, srcMsg string) (string, bool, error) {
	added, err := f.AddSuppression(ctx, userID, address, reason, source, srcMsg)
	return "supp_fake", added, err
}
func (f *fakeConsumerStore) AppendLifecycleTx(_ context.Context, _ pgx.Tx, input messagelifecycle.AppendInput) (messagelifecycle.MessageLifecycleTransition, error) {
	transition, err := messagelifecycle.NewTransition(input)
	transition.ID = "mlt_fake"
	return transition, err
}
func (f *fakeConsumerStore) RecordDeliveryOutcome(ctx context.Context, messageID, address string, st Status, detail string) error {
	f.outcomes = append(f.outcomes, [3]string{messageID, address, string(st)})
	return nil
}
func (f *fakeConsumerStore) AddSuppression(ctx context.Context, userID, address, reason, source, srcMsg string) (bool, error) {
	if f.addSuppErr != nil {
		return false, f.addSuppErr
	}
	key := userID + "|" + address
	if f.alreadySupp[key] {
		return false, nil
	}
	f.suppressed[key] = true
	return true, nil
}

type firedEvent struct {
	userID, agentID, conversationID, messageID, eventType string
	data                                                  any
	dedupKey                                              string
}

func recordingFirer() (Firer, *[]firedEvent) {
	var events []firedEvent
	f := func(ctx context.Context, _ pgx.Tx, e FiredEvent) error {
		events = append(events, firedEvent{e.UserID, e.AgentID, e.ConversationID, e.MessageID, e.Type, e.Data, e.DedupKey})
		return nil
	}
	return f, &events
}

// payloadBatchID extracts batch_id from any of the outbound event payloads
// that carry it, so the propagation test can assert one field across the four
// feedback outcomes without a type switch at each call site.
func payloadBatchID(data any) (string, bool) {
	switch d := data.(type) {
	case eventpayload.EmailDeliveredData:
		return d.BatchID, true
	case eventpayload.EmailBouncedData:
		return d.BatchID, true
	case eventpayload.EmailComplainedData:
		return d.BatchID, true
	case eventpayload.EmailFailedData:
		return d.BatchID, true
	}
	return "", false
}

// TestConsumerPropagatesBatchID pins that batch correlation survives PAST
// provider acceptance: every delivery-feedback event for a batch-originated
// message carries the same batch_id as email.sent/email.failed, so a subscriber
// can group delivered/bounced/complained/failed outcomes back to the batch
// call. The empty-BatchID (single-send) case omits it — locked by the golden
// .min.json fixtures.
func TestConsumerPropagatesBatchID(t *testing.T) {
	const batchID = "bat_feedbackcorrelation0001"
	cases := []struct {
		name     string
		ev       *Event
		wantType string
	}{
		{"delivered", &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt, Kind: KindDelivery, SESMessageID: "ses-b",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusDelivered}}}, EventEmailDelivered},
		{"bounced", &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt, Kind: KindBounce, SESMessageID: "ses-b", BounceType: "permanent",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusBounced}}}, EventEmailBounced},
		{"complained", &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt, Kind: KindComplaint, SESMessageID: "ses-b",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusComplained}}}, EventEmailComplained},
		{"reject->failed", &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt, Kind: KindReject, SESMessageID: "ses-b",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusFailed, Detail: "Bad content"}}}, EventEmailFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeConsumerStore()
			store.corr["ses-b"] = &CorrelatedMessage{MessageID: "msg_b", UserID: "u_b", AgentID: "bot@x.com", BatchID: batchID}
			fire, events := recordingFirer()
			c := NewConsumer(store, fire)
			if err := c.Process(context.Background(), tc.ev); err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, e := range *events {
				if e.eventType != tc.wantType {
					continue
				}
				got, ok := payloadBatchID(e.data)
				if !ok {
					t.Fatalf("%s payload is not a batch_id-carrying type: %T", tc.wantType, e.data)
				}
				if got != batchID {
					t.Errorf("%s batch_id = %q, want %q propagated from the correlated message", tc.wantType, got, batchID)
				}
				found = true
			}
			if !found {
				t.Fatalf("no %s event fired", tc.wantType)
			}
		})
	}
}

func TestConsumerProcess(t *testing.T) {
	t.Run("uncorrelated message is a no-op ack", func(t *testing.T) {
		store := newFakeConsumerStore()
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindDelivery, SESMessageID: "unknown",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusDelivered}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(store.outcomes) != 0 || len(*events) != 0 {
			t.Fatal("nothing should be recorded for an uncorrelated message")
		}
	})

	t.Run("delivery records outcome + fires email.delivered with agent id", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-1"] = &CorrelatedMessage{MessageID: "msg_1", UserID: "u_1", AgentID: "bot@x.com", Subject: "hi"}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindDelivery, SESMessageID: "ses-1",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusDelivered}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(store.outcomes) != 1 || store.outcomes[0] != [3]string{"msg_1", "a@x.com", "delivered"} {
			t.Fatalf("outcomes=%v", store.outcomes)
		}
		if len(*events) != 1 {
			t.Fatalf("events=%v", *events)
		}
		e := (*events)[0]
		if e.eventType != EventEmailDelivered || e.userID != "u_1" || e.agentID != "bot@x.com" {
			t.Fatalf("event=%+v", e)
		}
		// The delivery-feedback events share the firer; the message-level routing
		// key must be set so the persisted event is findable by message_id.
		if e.messageID != "msg_1" {
			t.Errorf("email.delivered envelope messageID=%q, want msg_1 (findability)", e.messageID)
		}
		data, ok := e.data.(eventpayload.EmailDeliveredData)
		if !ok {
			t.Fatalf("data is not the canonical typed payload: %T", e.data)
		}
		if data.Subject != "hi" {
			t.Errorf("subject = %q, want the correlated message subject", data.Subject)
		}
		if data.Direction != "outbound" || data.DeliveredTo != "a@x.com" {
			t.Errorf("data=%+v", data)
		}
	})

	t.Run("hard bounce records + fires bounced + suppresses + fires suppression", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-2"] = &CorrelatedMessage{MessageID: "msg_2", UserID: "u_2", AgentID: "bot@x.com"}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		_ = c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindBounce, SESMessageID: "ses-2",
			BounceType: "permanent", BounceSubType: "General",
			Recipients: []RecipientOutcome{{Address: "b@x.com", Status: StatusBounced, Detail: "550", Suppress: true}},
		})
		if !store.suppressed["u_2|b@x.com"] {
			t.Fatal("address should be suppressed")
		}
		var types []string
		for _, e := range *events {
			types = append(types, e.eventType)
			if e.eventType == EventEmailBounced {
				data, ok := e.data.(eventpayload.EmailBouncedData)
				if !ok {
					t.Fatalf("bounced data is not typed: %T", e.data)
				}
				if data.BounceType != "permanent" || data.BounceSubType != "General" {
					t.Errorf("bounce classification not wired through: %+v", data)
				}
			}
		}
		if !contains(types, EventEmailBounced) || !contains(types, EventSuppressionAdded) {
			t.Fatalf("expected bounced + suppression_added, got %v", types)
		}
	})

	t.Run("bounce without a classification defaults to undetermined", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-2b"] = &CorrelatedMessage{MessageID: "msg_2b", UserID: "u_2", AgentID: "bot@x.com"}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		_ = c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindBounce, SESMessageID: "ses-2b",
			Recipients: []RecipientOutcome{{Address: "b@x.com", Status: StatusBounced}},
		})
		data := (*events)[0].data.(eventpayload.EmailBouncedData)
		if data.BounceType != "undetermined" {
			t.Errorf("bounce_type = %q, want undetermined (required field)", data.BounceType)
		}
	})

	t.Run("complaint suppresses with no agent id on the suppression event", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-3"] = &CorrelatedMessage{MessageID: "msg_3", UserID: "u_3", AgentID: "bot@x.com"}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		_ = c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindComplaint, SESMessageID: "ses-3",
			Recipients: []RecipientOutcome{{Address: "c@x.com", Status: StatusComplained, Suppress: true}},
		})
		for _, e := range *events {
			if e.eventType == EventSuppressionAdded {
				// Account-scoped: no agent/message/conversation envelope routing keys
				// (so it is not filtered out of a message/thread-scoped events query
				// it was never about; the triggering message is in the payload).
				if e.agentID != "" || e.messageID != "" || e.conversationID != "" {
					t.Errorf("suppression event is account-scoped; agentID/messageID/conversationID should be empty, got %q/%q/%q",
						e.agentID, e.messageID, e.conversationID)
				}
			}
		}
	})

	t.Run("re-suppression fires the event at most once", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-4"] = &CorrelatedMessage{MessageID: "msg_4", UserID: "u_4", AgentID: "bot@x.com"}
		store.alreadySupp["u_4|d@x.com"] = true // already on the list
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		_ = c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindComplaint, SESMessageID: "ses-4",
			Recipients: []RecipientOutcome{{Address: "d@x.com", Status: StatusComplained, Suppress: true}},
		})
		for _, e := range *events {
			if e.eventType == EventSuppressionAdded {
				t.Error("suppression_added must not fire when the address was already suppressed")
			}
		}
	})

	t.Run("reject records failed per recipient + fires exactly ONE message-level email.failed", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-5"] = &CorrelatedMessage{
			MessageID: "msg_5", UserID: "u_5", AgentID: "bot@x.com", Subject: "hello",
			ConversationID: "conv_5", Method: "smtp", MessageType: "send",
			From: "bot@x.com", To: []string{"a@x.com", "b@x.com"}, CC: []string{"c@x.com"},
		}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindReject, SESMessageID: "ses-5",
			Recipients: []RecipientOutcome{
				{Address: "a@x.com", Status: StatusFailed, Detail: "Bad content"},
				{Address: "b@x.com", Status: StatusFailed, Detail: "Bad content"},
				{Address: "c@x.com", Status: StatusFailed, Detail: "Bad content"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(store.outcomes) != 3 {
			t.Fatalf("outcomes=%v, want one failed outcome per recipient", store.outcomes)
		}
		for _, o := range store.outcomes {
			if o[0] != "msg_5" || o[2] != "failed" {
				t.Fatalf("outcome=%v, want (msg_5, _, failed)", o)
			}
		}
		if len(*events) != 1 {
			t.Fatalf("got %d events %v, want exactly one message-level email.failed (never one per recipient)", len(*events), *events)
		}
		e := (*events)[0]
		if e.eventType != EventEmailFailed || e.userID != "u_5" || e.agentID != "bot@x.com" {
			t.Fatalf("event=%+v", e)
		}
		// Envelope routing keys must be set so the persisted event is findable
		// via GET /v1/events?message_id=/?conversation_id= (the reconciliation
		// query) and matches the send-worker path's envelope.
		if e.messageID != "msg_5" || e.conversationID != "conv_5" {
			t.Fatalf("envelope routing keys missing: messageID=%q conversationID=%q", e.messageID, e.conversationID)
		}
		if e.dedupKey != "msg_5|"+EventEmailFailed {
			t.Fatalf("dedupKey=%q, want the worker-path deterministic formula message_id|event_type", e.dedupKey)
		}
		data, ok := e.data.(eventpayload.EmailFailedData)
		if !ok {
			t.Fatalf("data is not the canonical typed payload: %T", e.data)
		}
		if data.MessageID != "msg_5" || data.AgentEmail != "bot@x.com" || data.Direction != "outbound" {
			t.Fatalf("data=%+v", data)
		}
		if data.ConversationID != "conv_5" || data.Method != "smtp" || data.MessageType != "send" ||
			data.From != "bot@x.com" || data.Subject != "hello" {
			t.Fatalf("correlated message fields not wired through: %+v", data)
		}
		if len(data.To) != 2 || data.To[0] != "a@x.com" || data.To[1] != "b@x.com" ||
			len(data.CC) != 1 || data.CC[0] != "c@x.com" {
			t.Fatalf("recipient lists must come from the correlated message: to=%v cc=%v", data.To, data.CC)
		}
		if data.Reason != "Bad content" {
			t.Fatalf("reason=%q, want the SES reject reason", data.Reason)
		}
		if data.ReasonCode != string(messagelifecycle.ReasonSubmissionProviderRejected) || data.Retryable == nil || *data.Retryable {
			t.Fatalf("reason_code/retryable must carry the canonical provider rejection: %+v", data)
		}
	})

	t.Run("reject never suppresses and fires no suppression event", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-6"] = &CorrelatedMessage{MessageID: "msg_6", UserID: "u_6", AgentID: "bot@x.com"}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		_ = c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindReject, SESMessageID: "ses-6",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusFailed, Detail: "Bad content"}},
		})
		if len(store.suppressed) != 0 {
			t.Fatalf("SES Reject must not add suppressions: %v", store.suppressed)
		}
		for _, e := range *events {
			if e.eventType == EventSuppressionAdded {
				t.Fatal("SES Reject must not fire domain.suppression_added")
			}
		}
	})

	t.Run("duplicate reject notifications fire with an identical dedup key", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-7"] = &CorrelatedMessage{MessageID: "msg_7", UserID: "u_7", AgentID: "bot@x.com"}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		ev := &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindReject, SESMessageID: "ses-7",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusFailed, Detail: "Bad content"}},
		}
		for i := 0; i < 2; i++ { // SNS is at-least-once — the same notification can arrive twice
			if err := c.Process(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		}
		if len(*events) != 2 {
			t.Fatalf("events=%v", *events)
		}
		if (*events)[0].dedupKey != (*events)[1].dedupKey {
			t.Fatalf("dedup keys differ across redelivery: %q vs %q — the outbox could not collapse them",
				(*events)[0].dedupKey, (*events)[1].dedupKey)
		}
	})

	t.Run("reject with no reason falls back to a stable non-empty reason", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-8"] = &CorrelatedMessage{MessageID: "msg_8", UserID: "u_8", AgentID: "bot@x.com", To: nil}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		_ = c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindReject, SESMessageID: "ses-8",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusFailed}},
		})
		data := (*events)[0].data.(eventpayload.EmailFailedData)
		if data.Reason == "" {
			t.Fatal("email.failed requires a non-empty reason; an SES Reject without reject.reason must use the fallback")
		}
		if data.To == nil {
			t.Fatal("to is nullable:false — a correlated row without a recipient list must still marshal []")
		}
	})

	t.Run("reject for an uncorrelated message is a no-op ack", func(t *testing.T) {
		store := newFakeConsumerStore()
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindReject, SESMessageID: "unknown-reject",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusFailed, Detail: "Bad content"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(store.outcomes) != 0 || len(*events) != 0 {
			t.Fatal("nothing should be recorded or fired for an uncorrelated reject")
		}
	})
}

// TestConsumerCorrelationAndEvidence pins the async-send-contract §3.1
// correlation machinery: the X-E2A-Message-ID header echo is the fallback
// correlation channel, correlated post-acceptance events record provider-accept
// evidence, and Reject/uncorrelated events never do.
func TestConsumerCorrelationAndEvidence(t *testing.T) {
	t.Run("header echo correlates when the SES id is unknown (crash window)", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corrByE2A["msg_abc123"] = &CorrelatedMessage{MessageID: "msg_abc123", UserID: "u_1", AgentID: "bot@x.com", Subject: "hi"}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindDelivery, SESMessageID: "ses-never-captured", E2AMessageID: "msg_abc123",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusDelivered}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(store.outcomes) != 1 || store.outcomes[0] != [3]string{"msg_abc123", "a@x.com", "delivered"} {
			t.Fatalf("outcomes=%v, want the header-correlated delivery recorded", store.outcomes)
		}
		if len(store.evidence) != 1 || store.evidence[0] != [2]string{"msg_abc123", "ses-never-captured"} {
			t.Fatalf("evidence=%v, want provider-accept evidence with the SES id for repair", store.evidence)
		}
		if len(*events) != 1 || (*events)[0].eventType != EventEmailDelivered {
			t.Fatalf("events=%v, want one email.delivered", *events)
		}
	})

	t.Run("SES-id correlation still wins and records evidence", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-1"] = &CorrelatedMessage{MessageID: "msg_1", UserID: "u_1", AgentID: "bot@x.com"}
		c := NewConsumer(store, nil)
		if err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindDelivery, SESMessageID: "ses-1",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusDelivered}},
		}); err != nil {
			t.Fatal(err)
		}
		if len(store.e2aLookups) != 0 {
			t.Errorf("header lookup attempted despite SES-id hit: %v", store.e2aLookups)
		}
		if len(store.evidence) != 1 {
			t.Errorf("evidence=%v, want one provider-accept record", store.evidence)
		}
	})

	t.Run("Send event with no recipients still records evidence", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.pending = true
		store.corrByE2A["msg_send1"] = &CorrelatedMessage{MessageID: "msg_send1", UserID: "u_1", AgentID: "bot@x.com"}
		finalized := 0
		c := NewConsumer(store, nil, func(context.Context, pgx.Tx, string) error {
			finalized++
			return nil
		})
		if err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindSend, SESMessageID: "ses-s1", E2AMessageID: "msg_send1",
		}); err != nil {
			t.Fatal(err)
		}
		if len(store.evidence) != 1 || store.evidence[0][0] != "msg_send1" {
			t.Fatalf("evidence=%v, want the Send acceptance recorded", store.evidence)
		}
		if len(store.outcomes) != 0 {
			t.Errorf("a Send has no recipient outcomes, got %v", store.outcomes)
		}
		if finalized != 1 {
			t.Errorf("Send canonical acceptance finalizations=%d want 1", finalized)
		}
		if store.preflights != 0 {
			t.Errorf("Send recipient preflights=%d want 0", store.preflights)
		}
	})

	t.Run("Reject never records provider-accept evidence", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-rej"] = &CorrelatedMessage{MessageID: "msg_rej", UserID: "u_1", AgentID: "bot@x.com"}
		c := NewConsumer(store, nil)
		if err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindReject, SESMessageID: "ses-rej",
			Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusFailed}},
		}); err != nil {
			t.Fatal(err)
		}
		if len(store.evidence) != 0 {
			t.Fatalf("Reject recorded accept evidence: %v", store.evidence)
		}
		if len(store.outcomes) != 1 || store.outcomes[0][2] != "failed" {
			t.Fatalf("outcomes=%v, want the per-recipient failed recorded", store.outcomes)
		}
	})

	t.Run("uncorrelated event with a garbage header marker stays a no-op", func(t *testing.T) {
		store := newFakeConsumerStore()
		c := NewConsumer(store, nil)
		for _, bad := range []string{"", "msg_", "not_an_id", "msg_../../etc", "msg_a b"} {
			if err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
				Kind: KindDelivery, SESMessageID: "unknown", E2AMessageID: bad,
				Recipients: []RecipientOutcome{{Address: "a@x.com", Status: StatusDelivered}},
			}); err != nil {
				t.Fatal(err)
			}
		}
		if len(store.e2aLookups) != 0 {
			t.Errorf("invalid marker shapes must not reach the store: %v", store.e2aLookups)
		}
		if len(store.outcomes) != 0 || len(store.evidence) != 0 {
			t.Error("uncorrelated feedback must record nothing")
		}
	})
}

func TestRecipientApplicabilityContract(t *testing.T) {
	for _, tc := range []struct {
		kind EventKind
		want bool
	}{
		{KindDelivery, true},
		{KindDeliveryDelay, true},
		{KindBounce, true},
		{KindComplaint, true},
		{KindReject, true},
		{KindSend, false},
		{KindOther, false},
	} {
		if got := tc.kind.requiresApplicableRecipient(); got != tc.want {
			t.Errorf("%s requiresApplicableRecipient=%v want %v", tc.kind, got, tc.want)
		}
	}
}

// TestConsumerGoldenPayloads is this package's side of the cross-channel
// drift lock for fields independent of persistence. Lifecycle rows are
// deliberately excluded here; DB-backed consumer tests compare each emitted
// transition id to the exact row AppendTx committed.
func TestConsumerGoldenPayloads(t *testing.T) {
	const (
		msgID   = "msg_01h2xcejqtf2nbrexx3vqjhp44"
		userID  = "user_7a6b5c4d"
		agent   = "support@agents.example.com"
		subject = "Re: Order #1234 delayed"
		batchID = "bat_9c8b7a6d5e4f3a2b1c0d9e8f"
		fixture = "../eventpayload/testdata/"
	)
	fireGolden := func(sesEvent *Event) *[]firedEvent {
		t.Helper()
		store := newFakeConsumerStore()
		store.corr["ses-golden"] = &CorrelatedMessage{MessageID: msgID, UserID: userID, AgentID: agent, Subject: subject, BatchID: batchID}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		if err := c.Process(context.Background(), sesEvent); err != nil {
			t.Fatal(err)
		}
		return events
	}

	t.Run("email.delivered", func(t *testing.T) {
		events := fireGolden(&Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindDelivery, SESMessageID: "ses-golden",
			Recipients: []RecipientOutcome{{Address: "alice@customer.example.com", Status: StatusDelivered}},
		})
		goldenassert.DataIgnoringLifecycle(t, fixture+"email.delivered.json", (*events)[0].data)
	})

	t.Run("email.bounced + domain.suppression_added", func(t *testing.T) {
		events := fireGolden(&Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindBounce, SESMessageID: "ses-golden",
			BounceType: "permanent", BounceSubType: "General",
			Recipients: []RecipientOutcome{{
				Address: "bob@customer.example.com", Status: StatusBounced,
				Detail: "550 5.1.1 no such user", Suppress: true,
			}},
		})
		if len(*events) != 2 {
			t.Fatalf("expected bounced + suppression, got %v", *events)
		}
		goldenassert.DataIgnoringLifecycle(t, fixture+"email.bounced.json", (*events)[0].data)
		goldenassert.DataIgnoringLifecycle(t, fixture+"domain.suppression_added.json", (*events)[1].data)
	})

	t.Run("email.complained", func(t *testing.T) {
		events := fireGolden(&Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindComplaint, SESMessageID: "ses-golden",
			Recipients: []RecipientOutcome{{Address: "carol@customer.example.com", Status: StatusComplained, Detail: "abuse"}},
		})
		goldenassert.DataIgnoringLifecycle(t, fixture+"email.complained.json", (*events)[0].data)
	})

	// Minimal (required-fields-only) variants: SES feedback with no subject
	// correlation, no diagnostic Detail, and (for bounces) no sub-type must
	// byte-match the .min.json fixtures — locking that the optional
	// subject/smtp_detail/bounce_sub_type fields are ABSENT from the wire when
	// unset, which the fully-populated fixtures above can't detect.
	fireMinimal := func(sesEvent *Event) *[]firedEvent {
		t.Helper()
		store := newFakeConsumerStore()
		store.corr["ses-golden-min"] = &CorrelatedMessage{MessageID: msgID, UserID: userID, AgentID: agent} // no subject
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		if err := c.Process(context.Background(), sesEvent); err != nil {
			t.Fatal(err)
		}
		return events
	}

	t.Run("email.delivered minimal", func(t *testing.T) {
		events := fireMinimal(&Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindDelivery, SESMessageID: "ses-golden-min",
			Recipients: []RecipientOutcome{{Address: "alice@customer.example.com", Status: StatusDelivered}},
		})
		data := (*events)[0].data.(eventpayload.EmailDeliveredData)
		data.LifecycleTransitions = nil // historical/minimal fixture proves optional absence
		goldenassert.Data(t, fixture+"email.delivered.min.json", data)
	})

	t.Run("email.bounced minimal", func(t *testing.T) {
		events := fireMinimal(&Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindBounce, SESMessageID: "ses-golden-min",
			BounceType: "permanent",
			Recipients: []RecipientOutcome{{Address: "bob@customer.example.com", Status: StatusBounced}},
		})
		data := (*events)[0].data.(eventpayload.EmailBouncedData)
		data.LifecycleTransitions = nil // historical/minimal fixture proves optional absence
		goldenassert.Data(t, fixture+"email.bounced.min.json", data)
	})

	t.Run("email.complained minimal", func(t *testing.T) {
		events := fireMinimal(&Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindComplaint, SESMessageID: "ses-golden-min",
			Recipients: []RecipientOutcome{{Address: "carol@customer.example.com", Status: StatusComplained}},
		})
		data := (*events)[0].data.(eventpayload.EmailComplainedData)
		data.LifecycleTransitions = nil // historical/minimal fixture proves optional absence
		goldenassert.Data(t, fixture+"email.complained.min.json", data)
	})

	// email.failed via SES Reject must byte-match the SAME committed fixtures
	// the async send worker's emission is locked to (internal/eventpayload's
	// golden_test builds them from eventpayload.EmailFailedData directly) —
	// there is exactly one canonical email.failed shape, whichever path emits it.
	t.Run("email.failed via SES Reject", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-golden-reject"] = &CorrelatedMessage{
			MessageID:      "msg_01h2xcejqtf2nbrexx3vqjhp43",
			UserID:         userID,
			AgentID:        agent,
			Subject:        subject,
			ConversationID: "conv_9f8e7d6c",
			Method:         "smtp",
			MessageType:    "send",
			From:           agent,
			To:             []string{"alice@customer.example.com"},
			CC:             []string{"ops@customer.example.com"},
			BCC:            []string{"audit@agents.example.com"},
		}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		if err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindReject, SESMessageID: "ses-golden-reject",
			Recipients: []RecipientOutcome{{
				Address: "alice@customer.example.com", Status: StatusFailed,
				Detail: "550 5.1.1 user unknown",
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if len(*events) != 1 {
			t.Fatalf("expected exactly one email.failed, got %v", *events)
		}
		goldenassert.DataIgnoringLifecycle(t, fixture+"email.failed.json", (*events)[0].data)
	})

	t.Run("email.failed via SES Reject minimal", func(t *testing.T) {
		store := newFakeConsumerStore()
		store.corr["ses-golden-reject-min"] = &CorrelatedMessage{
			MessageID:   "msg_01h2xcejqtf2nbrexx3vqjhp43",
			UserID:      userID,
			AgentID:     agent,
			Subject:     subject,
			Method:      "smtp",
			MessageType: "send",
			From:        agent,
			To:          []string{"alice@customer.example.com"},
		}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		if err := c.Process(context.Background(), &Event{ProviderEventID: "sns-test", OccurredAt: testFeedbackOccurredAt,
			Kind: KindReject, SESMessageID: "ses-golden-reject-min",
			Recipients: []RecipientOutcome{{
				Address: "alice@customer.example.com", Status: StatusFailed,
				Detail: "550 5.1.1 user unknown",
			}},
		}); err != nil {
			t.Fatal(err)
		}
		data := (*events)[0].data.(eventpayload.EmailFailedData)
		data.LifecycleTransitions = nil // historical/minimal fixture proves optional absence
		data.ReasonCode = ""
		data.Retryable = nil
		goldenassert.Data(t, fixture+"email.failed.min.json", data)
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
