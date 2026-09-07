package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type fakeProcessor struct {
	calls   []ProviderFeedback
	repairs [][]FeedbackRepair
	result  FeedbackResult
	err     error
	// order records "processor" / "correlate" so the test can pin that
	// accounting runs before any live-message lookup.
	order *[]string
}

func (p *fakeProcessor) ProcessProviderFeedback(_ context.Context, fb ProviderFeedback) (FeedbackResult, error) {
	p.calls = append(p.calls, fb)
	if p.order != nil {
		*p.order = append(*p.order, "processor")
	}
	return p.result, p.err
}

func (p *fakeProcessor) RepairSuppressions(_ context.Context, _ string, repairs []FeedbackRepair) error {
	p.repairs = append(p.repairs, repairs)
	return nil
}

type orderingStore struct {
	*fakeConsumerStore
	order *[]string
	// failTxOnce makes the first live transaction fail, the two-phase
	// window an SNS retry has to recover from.
	failTxOnce bool
	txCalls    int
	// rollback discards whatever the failed transaction's firer recorded.
	rollback func()
}

func (s *orderingStore) CorrelateBySESMessageID(ctx context.Context, id string) (*CorrelatedMessage, bool, error) {
	if s.order != nil {
		*s.order = append(*s.order, "correlate")
	}
	return s.fakeConsumerStore.CorrelateBySESMessageID(ctx, id)
}

func (s *orderingStore) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	s.txCalls++
	// Mirror a real rollback: the body runs, then every write it made —
	// suppression rows AND the outbox events it fired — is discarded. That
	// fidelity is the whole point: the retry must be able to redo them.
	before := make(map[string]bool, len(s.suppressed))
	for k, v := range s.suppressed {
		before[k] = v
	}
	err := fn(nil)
	if s.failTxOnce && s.txCalls == 1 {
		s.fakeConsumerStore.suppressed = before
		if s.rollback != nil {
			s.rollback()
		}
		return errors.New("live transaction failed")
	}
	return err
}

func bounceEvent(id, ses string) *Event {
	return &Event{
		Kind: KindBounce, SESMessageID: ses, ProviderEventID: id, OccurredAt: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
		BounceType: "permanent", BounceSubType: "General", AttemptCorrelationID: "cor_0123abcd",
		Recipients: []RecipientOutcome{{Address: "Bob@Example.test", Status: StatusBounced, Detail: "550", Suppress: true}},
	}
}

// TestConsumerRunsAccountingBeforeLiveLookupAndForUncorrelated: the
// deletion-resistant seam sees every notification first — including one
// whose message is gone — and receives the retained subtypes, the attempt
// marker, and the normalized recipients.
func TestConsumerRunsAccountingBeforeLiveLookupAndForUncorrelated(t *testing.T) {
	var order []string
	st := &orderingStore{fakeConsumerStore: newFakeConsumerStore(), order: &order}
	p := &fakeProcessor{order: &order, result: FeedbackResult{Correlated: true}}
	c := NewConsumer(st, nil).WithFeedbackProcessor(p)

	if err := c.Process(context.Background(), bounceEvent("evt-1", "ses-unknown")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(order) < 2 || order[0] != "processor" || order[1] != "correlate" {
		t.Fatalf("order = %v, want accounting before the live lookup", order)
	}
	if len(p.calls) != 1 {
		t.Fatalf("processor calls = %d, want 1 even though the message is unknown", len(p.calls))
	}
	fb := p.calls[0]
	if fb.ProviderEventID != "evt-1" || fb.Kind != KindBounce || fb.BounceType != "permanent" || fb.BounceSubType != "General" ||
		fb.ProviderMessageID != "ses-unknown" || fb.AttemptCorrelationID != "cor_0123abcd" ||
		len(fb.Recipients) != 1 || fb.Recipients[0] != "bob@example.test" {
		t.Fatalf("processor input = %+v", fb)
	}
}

// TestConsumerProcessorErrorMakesProviderRetry: accounting failure is the
// consumer's failure; the notification must not be acked.
func TestConsumerProcessorErrorMakesProviderRetry(t *testing.T) {
	st := newFakeConsumerStore()
	st.corr["ses-1"] = &CorrelatedMessage{MessageID: "msg_1", UserID: "usr_1", AgentID: "agt_1"}
	p := &fakeProcessor{err: errors.New("ledger unavailable")}
	c := NewConsumer(st, nil).WithFeedbackProcessor(p)
	if err := c.Process(context.Background(), bounceEvent("evt-2", "ses-1")); err == nil {
		t.Fatal("a failing processor must fail Process so the provider retries")
	}
	if len(st.outcomes) != 0 {
		t.Fatalf("lifecycle path ran despite accounting failure: %v", st.outcomes)
	}
}

// TestLiveMessageOwnsTheSuppression: when a message survives, the store
// writes the suppression inside the live transaction with the source
// message id and the provider diagnostic, and the seam's reported repair is
// NOT applied separately — one writer, one announcement.
func TestLiveMessageOwnsTheSuppression(t *testing.T) {
	st := newFakeConsumerStore()
	st.corr["ses-1"] = &CorrelatedMessage{MessageID: "msg_1", UserID: "usr_1", AgentID: "agt_1"}
	fire, events := recordingFirer()
	p := &fakeProcessor{result: FeedbackResult{Correlated: true, AccountRef: "usr_1",
		RepairNeeded: []FeedbackRepair{{Address: "bob@example.test", Source: "bounce", Reason: "bounce:General"}}}}
	c := NewConsumer(st, fire).WithFeedbackProcessor(p)
	if err := c.Process(context.Background(), bounceEvent("evt-3", "ses-1")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !st.suppressed["usr_1|bob@example.test"] {
		t.Fatal("the live path must write the suppression")
	}
	if len(p.repairs) != 0 {
		t.Fatalf("the seam must not repair while a message owns the row: %v", p.repairs)
	}
	got := 0
	for _, e := range *events {
		if e.eventType == EventSuppressionAdded {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("suppression events = %d, want 1", got)
	}
}

// TestSuppressionSurvivesLiveTransactionFailureAndRetry is the regression
// this design exists for: the accounting seam commits on its own, the live
// transaction then fails, and the SNS retry must still end with exactly one
// suppression and exactly one suppression_added event. An earlier shape,
// where the seam wrote the row and the consumer trusted its insert verdict,
// lost the event forever on that retry.
func TestSuppressionSurvivesLiveTransactionFailureAndRetry(t *testing.T) {
	base := newFakeConsumerStore()
	base.corr["ses-1"] = &CorrelatedMessage{MessageID: "msg_1", UserID: "usr_1", AgentID: "agt_1"}
	st := &orderingStore{fakeConsumerStore: base, failTxOnce: true}
	fire, events := recordingFirer()
	st.rollback = func() { *events = (*events)[:0] }
	// The seam is a duplicate on the retry, exactly as the real one is.
	p := &fakeProcessor{result: FeedbackResult{Correlated: true, AccountRef: "usr_1",
		RepairNeeded: []FeedbackRepair{{Address: "bob@example.test", Source: "bounce", Reason: "bounce:General"}}}}
	c := NewConsumer(st, fire).WithFeedbackProcessor(p)

	if err := c.Process(context.Background(), bounceEvent("evt-4", "ses-1")); err == nil {
		t.Fatal("a failing live transaction must fail Process so SNS retries")
	}
	p.result.Duplicate = true
	if err := c.Process(context.Background(), bounceEvent("evt-4", "ses-1")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got := 0
	for _, e := range *events {
		if e.eventType == EventSuppressionAdded {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("suppression events across the retry = %d, want exactly 1", got)
	}
	if !st.suppressed["usr_1|bob@example.test"] {
		t.Fatal("the retry must leave the suppression in place")
	}
}

// TestPurgedMessageRepairsThroughTheSeam: with no live message to own the
// row, the account-wide repair is applied through the seam instead, and
// announces nothing (there is no message for an event to reference).
func TestPurgedMessageRepairsThroughTheSeam(t *testing.T) {
	st := newFakeConsumerStore() // no correlation → message is gone
	fire, events := recordingFirer()
	p := &fakeProcessor{result: FeedbackResult{Correlated: true, AccountRef: "usr_1",
		RepairNeeded: []FeedbackRepair{{Address: "bob@example.test", Source: "bounce", Reason: "bounce:General"}}}}
	c := NewConsumer(st, fire).WithFeedbackProcessor(p)
	if err := c.Process(context.Background(), bounceEvent("evt-5", "ses-gone")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(p.repairs) != 1 || len(p.repairs[0]) != 1 || p.repairs[0][0].Address != "bob@example.test" {
		t.Fatalf("repairs = %v, want the one address", p.repairs)
	}
	if len(*events) != 0 {
		t.Fatalf("a repair with no message must announce nothing, got %v", *events)
	}
}

// TestParseRetainsSubtypesAndAttemptMarker: complaintSubType and the
// attempt header survive parsing; a malformed marker is dropped, not used.
func TestParseRetainsSubtypesAndAttemptMarker(t *testing.T) {
	body := `{"eventType":"Complaint","mail":{"messageId":"ses-9","headers":[
	  {"name":"x-e2a-provider-attempt","value":" cor_00ff11aa "},
	  {"name":"X-E2A-Message-ID","value":"msg_abc"}]},
	  "complaint":{"complainedRecipients":[{"emailAddress":"D@x.com"}],"complaintFeedbackType":"abuse","complaintSubType":"OnAccountSuppressionList"}}`
	ev, err := ParseSESNotification([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if ev.ComplaintSubType != "OnAccountSuppressionList" || ev.AttemptCorrelationID != "cor_00ff11aa" || ev.E2AMessageID != "msg_abc" {
		t.Fatalf("parsed = %+v", ev)
	}
	fb := ev.FeedbackFor()
	if fb.ComplaintSubType != "OnAccountSuppressionList" || len(fb.Recipients) != 1 || fb.Recipients[0] != "d@x.com" {
		t.Fatalf("FeedbackFor = %+v", fb)
	}

	bad := `{"eventType":"Delivery","mail":{"messageId":"ses-10","headers":[{"name":"X-E2A-Provider-Attempt","value":"cor_NOTHEX; rm"}]},"delivery":{"recipients":["a@x.com","A@x.com"]}}`
	ev, err = ParseSESNotification([]byte(bad))
	if err != nil {
		t.Fatal(err)
	}
	if ev.AttemptCorrelationID != "" {
		t.Fatalf("malformed marker must be dropped, got %q", ev.AttemptCorrelationID)
	}
	if fb := ev.FeedbackFor(); len(fb.Recipients) != 1 {
		t.Fatalf("recipients must be deduplicated after normalization: %v", fb.Recipients)
	}
}
