// External test package: delivery itself must stay a light leaf (no webhookpub
// import — webhookpub pulls in identity, which imports delivery), but the
// cross-path event-id invariant below needs webhookpub.DeterministicEventID.
package delivery_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

type crossPathStore struct{ corr *delivery.CorrelatedMessage }

func (s *crossPathStore) CorrelateBySESMessageID(ctx context.Context, id string) (*delivery.CorrelatedMessage, bool, error) {
	return s.corr, s.corr != nil, nil
}
func (s *crossPathStore) CorrelateByE2AMessageID(ctx context.Context, id string) (*delivery.CorrelatedMessage, bool, error) {
	return nil, false, nil
}
func (s *crossPathStore) RecordProviderAcceptEvidence(ctx context.Context, messageID, sesMessageID string) error {
	return nil
}
func (s *crossPathStore) RecordDeliveryOutcome(ctx context.Context, messageID, address string, status delivery.Status, detail string) error {
	return nil
}
func (s *crossPathStore) AddSuppression(ctx context.Context, userID, address, reason, source, sourceMessageID string) (bool, error) {
	return false, nil
}
func (s *crossPathStore) WithTx(ctx context.Context, fn func(pgx.Tx) error) error { return fn(nil) }
func (s *crossPathStore) LockAgentTx(context.Context, pgx.Tx, string) (bool, error) {
	return true, nil
}
func (s *crossPathStore) HasApplicableRecipientTx(context.Context, pgx.Tx, string, []string) (bool, error) {
	return true, nil
}
func (s *crossPathStore) RecordProviderAcceptEvidenceTx(context.Context, pgx.Tx, string, string, time.Time) error {
	return nil
}
func (s *crossPathStore) ProviderAcceptancePendingTx(context.Context, pgx.Tx, string) (bool, error) {
	return false, nil
}
func (s *crossPathStore) RecordProviderRejectTx(context.Context, pgx.Tx, string, string, time.Time) error {
	return nil
}
func (s *crossPathStore) RecordDeliveryOutcomeTx(context.Context, pgx.Tx, string, string, delivery.Status, string) (bool, error) {
	return true, nil
}
func (s *crossPathStore) AddSuppressionTx(context.Context, pgx.Tx, string, string, string, string, string) (string, bool, error) {
	return "supp_cross", false, nil
}
func (s *crossPathStore) AppendLifecycleTx(_ context.Context, _ pgx.Tx, input messagelifecycle.AppendInput) (messagelifecycle.MessageLifecycleTransition, error) {
	transition, err := messagelifecycle.NewTransition(input)
	transition.ID = "mlt_cross"
	return transition, err
}

// TestRejectEmailFailedIDCollapsesAcrossPaths pins the dedup design: the async
// send worker publishes email.failed with
// webhookpub.DeterministicEventID(messageID, EventEmailFailed), and main.go's
// deliveryEventFirer derives the SNS-path event id as
// DeterministicEventID(dedupKey). The consumer's Reject dedup key must hash to
// the SAME id, so (a) duplicate SNS deliveries and (b) any cross-path double
// emission collapse to one message-level email.failed in the outbox
// (ON CONFLICT (id) DO NOTHING) — subscribers can never receive two
// conflicting terminal events for one message.
func TestRejectEmailFailedIDCollapsesAcrossPaths(t *testing.T) {
	if delivery.EventEmailFailed != webhookpub.EventEmailFailed {
		t.Fatalf("event type drift: delivery=%q webhookpub=%q", delivery.EventEmailFailed, webhookpub.EventEmailFailed)
	}

	const msgID = "msg_crosspath"
	store := &crossPathStore{corr: &delivery.CorrelatedMessage{
		MessageID: msgID, UserID: "u_1", AgentID: "bot@x.com", To: []string{"a@x.com"},
	}}
	var keys []string
	fire := func(_ context.Context, _ pgx.Tx, e delivery.FiredEvent) error {
		if e.Type == delivery.EventEmailFailed {
			keys = append(keys, e.DedupKey)
		}
		return nil
	}
	c := delivery.NewConsumer(store, fire)
	for i := 0; i < 2; i++ { // duplicate SNS delivery of the same Reject
		if err := c.Process(context.Background(), &delivery.Event{
			Kind: delivery.KindReject, SESMessageID: "ses-crosspath", ProviderEventID: "sns-crosspath", OccurredAt: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
			Recipients: []delivery.RecipientOutcome{{Address: "a@x.com", Status: delivery.StatusFailed, Detail: "Bad content"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("dedup keys = %v, want two identical keys", keys)
	}
	got := webhookpub.DeterministicEventID(keys[0])
	want := webhookpub.DeterministicEventID(msgID, webhookpub.EventEmailFailed)
	if got != want {
		t.Fatalf("SNS-path event id %s != worker-path event id %s — cross-path dedup broken", got, want)
	}
}
