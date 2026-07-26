package delivery

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

// CorrelatedMessage is CorrelateBySESMessageID's result: the outbound message
// a SES provider id maps to, plus the message fields the canonical event
// payloads need — all columns of the same row/join the correlation SELECT
// already reads, so widening it costs no extra query.
type CorrelatedMessage struct {
	MessageID string
	UserID    string
	// AgentID is the agent's own address (an agent identity's id IS its
	// email), so it doubles as agent_email on event payloads.
	AgentID        string
	Subject        string
	ConversationID string
	Method         string
	MessageType    string
	From           string
	To             []string
	CC             []string
	BCC            []string
}

// Store is the narrow persistence surface the consumer needs. *identity.Store
// satisfies it.
type Store interface {
	// CorrelateBySESMessageID finds the outbound message + owning user + agent
	// (plus the message fields the event payloads need — same single SELECT,
	// no extra query) by the SES-assigned provider_message_id captured at send
	// time. found=false when the id is unknown (message deleted, or an event
	// for another deployment).
	CorrelateBySESMessageID(ctx context.Context, sesMessageID string) (m *CorrelatedMessage, found bool, err error)
	// CorrelateByE2AMessageID finds the outbound message by the e2a message id
	// SES echoed back from the MessageIDHeader stamped at submit time — the
	// correlation fallback for the SMTP-accept↔mark-sent crash window, where
	// the provider id from the 250 response was never captured. Same return
	// shape as CorrelateBySESMessageID; found=false for unknown/non-outbound.
	CorrelateByE2AMessageID(ctx context.Context, e2aMessageID string) (m *CorrelatedMessage, found bool, err error)
	// RecordProviderAcceptEvidence durably notes that SES reported having
	// accepted this message's submission (any correlated post-acceptance
	// notification kind) and repairs a provider_message_id lost to the
	// crash window. Idempotent. The worker/terminal-reconciler guards read
	// this evidence before declaring a terminal failure.
	WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error
	// HasApplicableRecipientTx locks and checks the persisted immutable
	// envelope. Recipient-bearing feedback must pass this preflight before it
	// can establish provider acceptance or trigger canonical sent finalization.
	HasApplicableRecipientTx(ctx context.Context, tx pgx.Tx, messageID string, addresses []string) (bool, error)
	RecordProviderAcceptEvidenceTx(ctx context.Context, tx pgx.Tx, messageID, sesMessageID string, occurredAt time.Time) error
	ProviderAcceptancePendingTx(ctx context.Context, tx pgx.Tx, messageID string) (bool, error)
	RecordProviderRejectTx(ctx context.Context, tx pgx.Tx, messageID, detail string, occurredAt time.Time) error
	// RecordDeliveryOutcome upserts the per-recipient status monotonically and
	// recomputes the message rollup (worst status by precedence). Idempotent:
	// a duplicate/older event is a no-op.
	RecordDeliveryOutcomeTx(ctx context.Context, tx pgx.Tx, messageID, address string, status Status, detail string) (applied bool, err error)
	// AddSuppression idempotently inserts a (user, address) suppression.
	// added=false when it already existed, so an upsert no-op fires no event.
	// This is per-INSERT, not once-per-address-forever: if the caller deletes a
	// suppression and the address is suppressed again later, that is a genuine
	// new insert and does fire.
	AddSuppressionTx(ctx context.Context, tx pgx.Tx, userID, address, reason, source, sourceMessageID string) (suppressionID string, added bool, err error)
	AppendLifecycleTx(ctx context.Context, tx pgx.Tx, input messagelifecycle.AppendInput) (messagelifecycle.MessageLifecycleTransition, error)
}

// FiredEvent is the set of fields delivery hands its Firer for one webhook
// event. Data is the canonical typed payload (eventpayload.EmailDeliveredData /
// EmailBouncedData / EmailComplainedData / EmailFailedData /
// DomainSuppressionAddedData).
//
// AgentID, ConversationID, and MessageID are the ENVELOPE ROUTING KEYS: they
// let subscribers' agent_ids/labels filters match AND they populate the
// webhook_events row's agent_id/conversation_id/message_id columns, which is
// what makes the persisted event findable via GET /v1/events?message_id= /
// ?conversation_id= — the reconciliation query an integrator uses to learn a
// specific message's fate. A message-backed delivery-feedback event must set
// all three (they mirror the send-worker path's envelope, giving full
// payload+envelope parity); account-scoped events (suppression) leave them
// empty. DedupKey makes redeliveries idempotent (the publisher derives a stable
// event id from it, independent of the routing keys).
type FiredEvent struct {
	UserID         string
	AgentID        string
	ConversationID string
	MessageID      string
	Type           string
	Data           any
	DedupKey       string
	OccurredAt     time.Time
}

// Firer publishes one delivery/suppression webhook event to the owning user's
// subscribers. Injected as a closure so this package does not depend on
// webhookpub.
type Firer func(ctx context.Context, tx pgx.Tx, e FiredEvent) error

// ProviderAcceptanceFinalizer completes the canonical sent side effects in
// the consumer's transaction when feedback closes the SMTP-accept crash gap.
type ProviderAcceptanceFinalizer func(ctx context.Context, tx pgx.Tx, messageID string) error

// Event push types for delivery outcomes (decision 9 vocabulary).
const (
	EventEmailDelivered  = "email.delivered"
	EventEmailBounced    = "email.bounced"
	EventEmailComplained = "email.complained"
	// EventEmailFailed is the canonical stable terminal-failure event
	// (webhookpub.EventEmailFailed — string-duplicated so this package stays a
	// light leaf). The SES Reject path emits it MESSAGE-level, not per
	// recipient; see Process.
	EventEmailFailed        = "email.failed"
	EventSuppressionAdded   = "domain.suppression_added" // account-scoped despite the prefix (design vocab)
	suppressionSourceBounce = "bounce"
	suppressionSourceCompl  = "complaint"
)

// rejectFallbackReason keeps email.failed's required `reason` non-empty when
// an SES Reject notification carries no reject.reason.
const rejectFallbackReason = "rejected by the email provider after acceptance"

// pushEventFor maps a recipient status to its PER-RECIPIENT webhook event
// type, or "" for statuses with no per-recipient push event (queued/sent/
// deferred — poll instead; failed is emitted message-level by the Reject path
// in Process, never per recipient).
func pushEventFor(s Status) string {
	switch s {
	case StatusDelivered:
		return EventEmailDelivered
	case StatusBounced:
		return EventEmailBounced
	case StatusComplained:
		return EventEmailComplained
	}
	return ""
}

// Consumer applies a parsed SES Event to the store: per-recipient transitions,
// delivery events, and auto-suppression. It is idempotent end-to-end (safe to
// process the same SNS notification twice — at-least-once delivery).
type Consumer struct {
	store              Store
	fire               Firer
	finalizeAcceptance ProviderAcceptanceFinalizer
}

// NewConsumer builds the consumer. fire may be nil (no events).
func NewConsumer(store Store, fire Firer, finalizers ...ProviderAcceptanceFinalizer) *Consumer {
	consumer := &Consumer{store: store, fire: fire}
	if len(finalizers) > 0 {
		consumer.finalizeAcceptance = finalizers[0]
	}
	return consumer
}

// Process applies one normalized SES event. Unknown/uncorrelated messages are
// no-ops (returns nil) — an SNS notification e2a can't act on must still be
// ACKed so SES stops retrying. A correlated event with no recipient outcomes
// (e.g. an SES Send) still records provider-accept evidence before returning.
func (c *Consumer) Process(ctx context.Context, ev *Event) error {
	if ev == nil {
		return nil
	}
	if strings.TrimSpace(ev.ProviderEventID) == "" {
		return fmt.Errorf("provider event id is required")
	}
	if ev.OccurredAt.IsZero() {
		return fmt.Errorf("provider event timestamp is required")
	}
	m, found, err := c.store.CorrelateBySESMessageID(ctx, ev.SESMessageID)
	if err != nil {
		return err
	}
	if !found && validE2AMessageID(ev.E2AMessageID) {
		// Correlation fallback: the X-E2A-Message-ID header e2a stamped on the
		// wire, echoed back in the (signature-verified — handler.go verifies
		// before Process) notification. This is what makes feedback for the
		// SMTP-accept↔mark-sent crash window authoritatively correlatable:
		// that window never captured a provider id, so the SES-id lookup
		// above cannot match.
		m, found, err = c.store.CorrelateByE2AMessageID(ctx, ev.E2AMessageID)
		if err != nil {
			return err
		}
	}
	if !found {
		if len(ev.Recipients) == 0 {
			return nil
		}
		log.Printf("[delivery] SES %s for unknown message id=%s (expired/foreign); acking", ev.Kind, ev.SESMessageID)
		return nil
	}

	return c.store.WithTx(ctx, func(tx pgx.Tx) error {
		// Evidence, recipient rollup, canonical lifecycle, causal suppression,
		// and event outbox rows share this transaction. A notification retry can
		// therefore never expose a state/event contradiction.
		if ev.Kind.requiresApplicableRecipient() {
			addresses := make([]string, 0, len(ev.Recipients))
			for _, recipient := range ev.Recipients {
				if address := norm(recipient.Address); address != "" {
					addresses = append(addresses, address)
				}
			}
			applicable, err := c.store.HasApplicableRecipientTx(ctx, tx, m.MessageID, addresses)
			if err != nil {
				return err
			}
			if !applicable {
				return nil
			}
		}
		if ev.Kind.impliesProviderAcceptance() {
			if err := c.store.RecordProviderAcceptEvidenceTx(ctx, tx, m.MessageID, ev.SESMessageID, ev.OccurredAt); err != nil {
				return err
			}
			pending, err := c.store.ProviderAcceptancePendingTx(ctx, tx, m.MessageID)
			if err != nil {
				return err
			}
			if pending {
				if c.finalizeAcceptance == nil {
					return fmt.Errorf("provider acceptance finalizer is required")
				}
				if err := c.finalizeAcceptance(ctx, tx, m.MessageID); err != nil {
					return err
				}
			}
		}

		recorded := 0
		for _, r := range ev.Recipients {
			r.Address = norm(r.Address)
			if r.Address == "" || !r.Status.Valid() {
				continue
			}
			applied, err := c.store.RecordDeliveryOutcomeTx(ctx, tx, m.MessageID, r.Address, r.Status, r.Detail)
			if err != nil {
				return err
			}
			if !applied {
				continue
			}
			recorded++
			reason := feedbackReason(ev, r)
			var feedbackTransitions []messagelifecycle.MessageLifecycleTransition
			var suppressionEvent *FiredEvent
			if reason != "" {
				transition, err := c.store.AppendLifecycleTx(ctx, tx, messagelifecycle.AppendInput{MessageID: m.MessageID, DedupeKey: feedbackDedupeKey(ev, r.Address, reason), Direction: "outbound", Recipient: r.Address, ReasonCode: reason, Evidence: feedbackEvidence(ev, r), CorrelationIDs: feedbackCorrelations(ev), OccurredAt: ev.OccurredAt})
				if err != nil {
					return err
				}
				feedbackTransitions = append(feedbackTransitions, transition)
			}

			if r.Suppress {
				source := suppressionSourceBounce
				suppressionReason := messagelifecycle.ReasonSuppressionHardBounceApplied
				if r.Status == StatusComplained {
					source = suppressionSourceCompl
					suppressionReason = messagelifecycle.ReasonSuppressionComplaintApplied
				}
				suppressionID, added, err := c.store.AddSuppressionTx(ctx, tx, m.UserID, r.Address, r.Detail, source, m.MessageID)
				if err != nil {
					return err
				}
				if added {
					suppressionTransition, err := c.store.AppendLifecycleTx(ctx, tx, messagelifecycle.AppendInput{MessageID: m.MessageID, DedupeKey: fmt.Sprintf("suppression:feedback:%s:%s:%s", suppressionID, m.MessageID, r.Address), Direction: "outbound", Recipient: r.Address, ReasonCode: suppressionReason, Evidence: map[string]any{"suppression_scope": "account", "suppression_source": source}, CorrelationIDs: feedbackCorrelations(ev), OccurredAt: ev.OccurredAt})
					if err != nil {
						return err
					}
					if c.fire != nil {
						// Dedup on the PROVIDER NOTIFICATION, not on (user, address).
						//
						// The old key was EventSuppressionAdded|userID|address, which
						// fired the event at most once per address for the lifetime of
						// the account. That guarded against nothing: once an address is
						// suppressed, a send to it is refused with 422
						// recipient_suppressed BEFORE it reaches the provider (see
						// internal/agent/api.go), so no second bounce can occur and
						// there is no repeat-event stream to protect anyone from.
						//
						// The only route to a second suppression is the remediation the
						// docs prescribe — DELETE /v1/account/suppressions/{address},
						// then send again. That is a deliberate act on the belief that
						// the address is fixed, and if it bounces again that belief was
						// wrong. Staying silent there left the caller's webhook-driven
						// state saying "deliverable" while e2a said "suppressed", with
						// every later send failing for a reason they could not see.
						//
						// Keying on ProviderEventID matches the sibling delivery events
						// (feedbackDedupeKey below): a REDELIVERED SNS notification —
						// the duplicate that actually happens — still dedups, while a
						// genuinely new suppression notifies. AddSuppressionTx's `added`
						// flag remains the guard that this is a real insert, not an
						// upsert no-op.
						event := FiredEvent{UserID: m.UserID, Type: EventSuppressionAdded, Data: eventpayload.DomainSuppressionAddedData{Address: r.Address, Source: source, Reason: r.Detail, MessageID: m.MessageID, LifecycleTransitions: []messagelifecycle.MessageLifecycleTransition{suppressionTransition}}, DedupKey: "provider-feedback:" + ev.ProviderEventID + ":" + r.Address + ":" + EventSuppressionAdded, OccurredAt: ev.OccurredAt}
						suppressionEvent = &event
					}
				}
			}

			if evType := pushEventFor(r.Status); evType != "" && c.fire != nil {
				if err := c.fire(ctx, tx, FiredEvent{UserID: m.UserID, AgentID: m.AgentID, ConversationID: m.ConversationID, MessageID: m.MessageID, Type: evType, Data: deliveryEventData(evType, m, ev, r, feedbackTransitions), DedupKey: feedbackDedupeKey(ev, r.Address, reason), OccurredAt: ev.OccurredAt}); err != nil {
					return err
				}
			}
			if suppressionEvent != nil {
				if err := c.fire(ctx, tx, *suppressionEvent); err != nil {
					return err
				}
			}
		}

		if ev.Kind == KindReject && recorded > 0 {
			if err := c.store.RecordProviderRejectTx(ctx, tx, m.MessageID, rejectReason(ev), ev.OccurredAt); err != nil {
				return err
			}
			transition, err := c.store.AppendLifecycleTx(ctx, tx, messagelifecycle.AppendInput{MessageID: m.MessageID, DedupeKey: feedbackDedupeKey(ev, "", messagelifecycle.ReasonSubmissionProviderRejected), Direction: "outbound", ReasonCode: messagelifecycle.ReasonSubmissionProviderRejected, Evidence: map[string]any{"failure_reason": messagelifecycle.SafeDiagnostic(rejectReason(ev)), "failure_code": string(messagelifecycle.ReasonSubmissionProviderRejected)}, CorrelationIDs: feedbackCorrelations(ev), OccurredAt: ev.OccurredAt})
			if err != nil {
				return err
			}
			if c.fire != nil {
				if err := c.fire(ctx, tx, FiredEvent{UserID: m.UserID, AgentID: m.AgentID, ConversationID: m.ConversationID, MessageID: m.MessageID, Type: EventEmailFailed, Data: failedEventData(m, rejectReason(ev), transition), DedupKey: m.MessageID + "|" + EventEmailFailed, OccurredAt: ev.OccurredAt}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (k EventKind) requiresApplicableRecipient() bool {
	switch k {
	case KindDelivery, KindDeliveryDelay, KindBounce, KindComplaint, KindReject:
		return true
	}
	return false
}

func feedbackReason(ev *Event, r RecipientOutcome) messagelifecycle.ReasonCode {
	switch r.Status {
	case StatusDelivered:
		return messagelifecycle.ReasonDeliveryRecipientServerAccepted
	case StatusDeferred:
		return messagelifecycle.ReasonDeliveryTemporaryDelay
	case StatusBounced:
		return messagelifecycle.BounceReason(ev.BounceType)
	case StatusComplained:
		return messagelifecycle.ReasonComplaintRecipientReported
	default:
		return ""
	}
}

func feedbackDedupeKey(ev *Event, recipient string, reason messagelifecycle.ReasonCode) string {
	return "provider-feedback:" + ev.ProviderEventID + ":" + recipient + ":" + string(reason)
}

func feedbackCorrelations(ev *Event) map[string]string {
	return messagelifecycle.SafeCorrelationIDs(map[string]string{
		"provider_message_id": ev.SESMessageID,
		"provider_event_id":   ev.ProviderEventID,
		"email_message_id":    ev.E2AMessageID,
	})
}

func feedbackEvidence(ev *Event, r RecipientOutcome) map[string]any {
	evidence := map[string]any{}
	if detail := messagelifecycle.SafeDiagnostic(r.Detail); detail != "" {
		evidence["smtp_detail"] = detail
	}
	if r.Status == StatusBounced {
		bounceType := ev.BounceType
		if bounceType == "" {
			bounceType = "undetermined"
		}
		evidence["bounce_type"] = bounceType
		if subType := messagelifecycle.SafeDiagnostic(ev.BounceSubType); subType != "" {
			evidence["bounce_sub_type"] = subType
		}
	}
	return evidence
}

// deliveryEventData builds the canonical typed payload for one per-recipient
// delivery outcome. bounce_type/bounce_sub_type come from the SES bounce
// notification's classification, parsed in ParseSESNotification; a bounced
// outcome that somehow lacks one is "undetermined" (the field is required).
func deliveryEventData(evType string, m *CorrelatedMessage, ev *Event, r RecipientOutcome, transitions []messagelifecycle.MessageLifecycleTransition) any {
	switch evType {
	case EventEmailBounced:
		bounceType := ev.BounceType
		if bounceType == "" {
			bounceType = "undetermined"
		}
		return eventpayload.EmailBouncedData{
			MessageID:            m.MessageID,
			AgentEmail:           m.AgentID,
			Direction:            "outbound",
			DeliveredTo:          r.Address,
			Subject:              m.Subject,
			SMTPDetail:           r.Detail,
			BounceType:           bounceType,
			BounceSubType:        ev.BounceSubType,
			LifecycleTransitions: transitions,
		}
	case EventEmailComplained:
		return eventpayload.EmailComplainedData{
			MessageID:            m.MessageID,
			AgentEmail:           m.AgentID,
			Direction:            "outbound",
			DeliveredTo:          r.Address,
			Subject:              m.Subject,
			SMTPDetail:           r.Detail,
			LifecycleTransitions: transitions,
		}
	default: // EventEmailDelivered
		return eventpayload.EmailDeliveredData{
			MessageID:            m.MessageID,
			AgentEmail:           m.AgentID,
			Direction:            "outbound",
			DeliveredTo:          r.Address,
			Subject:              m.Subject,
			SMTPDetail:           r.Detail,
			LifecycleTransitions: transitions,
		}
	}
}

// failedEventData builds the canonical eventpayload.EmailFailedData for an SES
// Reject — byte-identical in shape to the async send worker's emission
// (golden-fixture-locked): every field the correlated message carries is
// populated; ReasonCode and Retryable carry the canonical provider-rejection
// classification so every consumer receives the same terminal diagnosis.

func failedEventData(m *CorrelatedMessage, reason string, transitions ...messagelifecycle.MessageLifecycleTransition) eventpayload.EmailFailedData {
	retryable := false
	return eventpayload.EmailFailedData{
		MessageID:            m.MessageID,
		AgentEmail:           m.AgentID,
		Direction:            "outbound",
		ConversationID:       m.ConversationID,
		Method:               m.Method,
		From:                 m.From,
		To:                   nonNil(m.To),
		CC:                   m.CC,
		BCC:                  m.BCC,
		Subject:              m.Subject,
		MessageType:          m.MessageType,
		Reason:               reason,
		ReasonCode:           string(messagelifecycle.ReasonSubmissionProviderRejected),
		Retryable:            &retryable,
		LifecycleTransitions: transitions,
	}
}

// rejectReason returns the SES reject.reason (the parser stamps the same
// reason on every recipient outcome), or a stable fallback so email.failed's
// required `reason` is never empty.
func rejectReason(ev *Event) string {
	for _, r := range ev.Recipients {
		if r.Detail != "" {
			return r.Detail
		}
	}
	return rejectFallbackReason
}

// nonNil keeps `to` (nullable:false in the published schema) marshaling as []
// when the correlated row carried no recipient list.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
