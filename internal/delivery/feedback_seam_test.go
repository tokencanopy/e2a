package delivery

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeProcessor struct {
	calls  []ProviderFeedback
	result FeedbackResult
	err    error
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

type orderingStore struct {
	*fakeConsumerStore
	order *[]string
}

func (s *orderingStore) CorrelateBySESMessageID(ctx context.Context, id string) (*CorrelatedMessage, bool, error) {
	*s.order = append(*s.order, "correlate")
	return s.fakeConsumerStore.CorrelateBySESMessageID(ctx, id)
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

// TestConsumerUsesProcessorSuppressionVerdict: when the seam already
// upserted the suppression, its Inserted verdict decides the event, so a
// second upsert cannot hide a genuine insert; without a verdict the store
// path decides as before.
func TestConsumerUsesProcessorSuppressionVerdict(t *testing.T) {
	for name, tc := range map[string]struct {
		result     FeedbackResult
		wantEvents int
		wantStore  bool
	}{
		"processor inserted":    {result: FeedbackResult{Correlated: true, Suppressions: []FeedbackSuppression{{Address: "bob@example.test", ID: "supp_p", Inserted: true}}}, wantEvents: 1},
		"processor refreshed":   {result: FeedbackResult{Correlated: true, Suppressions: []FeedbackSuppression{{Address: "bob@example.test", ID: "supp_p", Inserted: false}}}, wantEvents: 0},
		"no verdict falls back": {result: FeedbackResult{Correlated: true}, wantEvents: 1, wantStore: true},
	} {
		t.Run(name, func(t *testing.T) {
			st := newFakeConsumerStore()
			st.corr["ses-1"] = &CorrelatedMessage{MessageID: "msg_1", UserID: "usr_1", AgentID: "agt_1"}
			fire, events := recordingFirer()
			c := NewConsumer(st, fire).WithFeedbackProcessor(&fakeProcessor{result: tc.result})
			if err := c.Process(context.Background(), bounceEvent("evt-3", "ses-1")); err != nil {
				t.Fatalf("Process: %v", err)
			}
			got := 0
			for _, e := range *events {
				if e.eventType == EventSuppressionAdded {
					got++
					if !tc.wantStore && e.dedupKey == "" {
						t.Fatal("suppression event without dedup key")
					}
				}
			}
			if got != tc.wantEvents {
				t.Fatalf("suppression events = %d, want %d", got, tc.wantEvents)
			}
			if stored := st.suppressed["usr_1|bob@example.test"]; stored != tc.wantStore {
				t.Fatalf("store upsert = %v, want %v", stored, tc.wantStore)
			}
		})
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
