package delivery

import (
	"context"
	"strings"
	"testing"
)

// A suppression that is deleted and later re-created must fire
// domain.suppression_added again.
//
// The event used to dedup on (type, userID, address), so it fired at most once
// per address for the lifetime of an account. That guarded against nothing:
// once an address is suppressed, a send to it is refused with 422
// recipient_suppressed BEFORE it reaches the provider, so no second bounce can
// occur and there is no repeat-event stream to protect anyone from.
//
// The only route to a second suppression is the remediation the docs prescribe
// — DELETE /v1/account/suppressions/{address}, then send again. That is a
// deliberate act on the belief the address is fixed; if it bounces again, that
// belief was wrong, and staying silent left the caller's webhook-driven state
// saying "deliverable" while e2a said "suppressed".
//
// Found by the first production conformance run: the suppression rows were
// created with timestamps matching their email.bounced / email.complained
// siblings, but no domain.suppression_added accompanied them.
func TestSuppressionAddedRefiresAfterDeleteAndReBounce(t *testing.T) {
	ctx := context.Background()
	const user, addr = "u_1", "bounce@x.com"

	fireOnce := func(providerEventID string) []firedEvent {
		store := newFakeConsumerStore()
		store.corr["ses-1"] = &CorrelatedMessage{MessageID: "msg_1", UserID: user, AgentID: "bot@x.com"}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		err := c.Process(ctx, &Event{
			ProviderEventID: providerEventID, OccurredAt: testFeedbackOccurredAt,
			Kind: KindBounce, SESMessageID: "ses-1",
			Recipients: []RecipientOutcome{{Address: addr, Status: StatusBounced, Detail: "550 user unknown", Suppress: true}},
		})
		if err != nil {
			t.Fatalf("Process(%s): %v", providerEventID, err)
		}
		return *events
	}

	suppressionKeys := func(evs []firedEvent) []string {
		var keys []string
		for _, e := range evs {
			if e.eventType == EventSuppressionAdded {
				keys = append(keys, e.dedupKey)
			}
		}
		return keys
	}

	// First bounce — a fresh store means the suppression is genuinely inserted.
	first := suppressionKeys(fireOnce("sns-bounce-1"))
	if len(first) != 1 {
		t.Fatalf("first bounce fired %d %s events, want 1", len(first), EventSuppressionAdded)
	}

	// The caller deletes the suppression and sends again; the address bounces a
	// second time, arriving as a DIFFERENT provider notification. A fresh store
	// models the post-delete state — the row is gone, so this is a real insert.
	second := suppressionKeys(fireOnce("sns-bounce-2"))
	if len(second) != 1 {
		t.Fatalf("re-bounce after delete fired %d %s events, want 1 — a genuinely new suppression must notify, or the caller's webhook state silently desyncs", len(second), EventSuppressionAdded)
	}

	// The keys must differ, or the publisher dedups the second one away. This is
	// the assertion that actually catches the regression: both events are
	// constructed either way, but identical keys mean only one is ever delivered.
	if first[0] == second[0] {
		t.Fatalf("dedup key is identical across two distinct provider notifications (%q) — the second event would be deduped away, which is the bug", first[0])
	}

	// And the key must be derived from the provider notification, matching the
	// sibling delivery events, so that a REDELIVERED notification still dedups.
	for _, k := range []string{first[0], second[0]} {
		if !strings.HasPrefix(k, "provider-feedback:") {
			t.Fatalf("dedup key %q should be derived from the provider notification (provider-feedback:<ProviderEventID>:...), matching feedbackDedupeKey, so redeliveries still dedup", k)
		}
	}
}

// The same provider notification arriving twice — the duplicate that actually
// happens with SNS at-least-once delivery — must still dedup to one event.
func TestSuppressionAddedDedupsARedeliveredNotification(t *testing.T) {
	ctx := context.Background()

	run := func() string {
		store := newFakeConsumerStore()
		store.corr["ses-1"] = &CorrelatedMessage{MessageID: "msg_1", UserID: "u_1", AgentID: "bot@x.com"}
		fire, events := recordingFirer()
		c := NewConsumer(store, fire)
		if err := c.Process(ctx, &Event{
			ProviderEventID: "sns-redelivered", OccurredAt: testFeedbackOccurredAt,
			Kind: KindBounce, SESMessageID: "ses-1",
			Recipients: []RecipientOutcome{{Address: "bounce@x.com", Status: StatusBounced, Detail: "550 user unknown", Suppress: true}},
		}); err != nil {
			t.Fatalf("Process: %v", err)
		}
		for _, e := range *events {
			if e.eventType == EventSuppressionAdded {
				return e.dedupKey
			}
		}
		t.Fatal("no suppression event fired")
		return ""
	}

	if a, b := run(), run(); a != b {
		t.Fatalf("the SAME provider notification produced different dedup keys (%q vs %q) — a redelivered SNS message would fire twice", a, b)
	}
}
