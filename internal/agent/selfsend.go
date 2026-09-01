package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/inboundpolicy"
	"github.com/tokencanopy/e2a/internal/inboundscreen"
	"github.com/tokencanopy/e2a/internal/loopback"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// isSelfSend / stripAgentSelfAliases delegate to internal/loopback so the
// hitlworker package can share the same predicate logic without an upward
// import. Kept as unexported aliases here to minimize the diff at every
// call site and to preserve the existing test export below.
func isSelfSend(req outbound.SendRequest, agentEmail string) bool {
	return loopback.IsSelfSend(req, agentEmail)
}

// StripAgentSelfAliases is the exported seam over stripAgentSelfAliases so the
// v1 httpapi reply/forward builders reuse the same self-alias stripping.
func StripAgentSelfAliases(addrs []string, agentEmail string) []string {
	return stripAgentSelfAliases(addrs, agentEmail)
}

func stripAgentSelfAliases(addrs []string, agentEmail string) []string {
	return loopback.StripAgentSelfAliases(addrs, agentEmail)
}

// performSelfSend writes the message as BOTH an outbound row (sender's
// sent history) and an inbound row (recipient's inbox). Mirrors the
// two-row shape the SMTP roundtrip would produce naturally, so
// list_messages, threading, and downstream tooling don't need any
// special-casing.
//
// HITL approval uses the same transactional local-delivery invariants through
// approveSelfSend while preserving its pre-existing outbound resource id.
//
// Returns the GET-able outbound message resource. The provider-style RFC
// Message-ID remains an internal threading key shared with the inbound twin.
// Method on the outbound row is "loopback" so operators can tell the
// difference from "smtp" in logs and audits.
// msgType is one of "send", "reply", or "forward" — recorded on the
// outbound row so audit queries can distinguish a self-note from a
// self-reply or self-forward. Without it, the loopback branch of
// handleReplyToMessage / handleForwardMessage would store "send" and
// fork the audit shape from the SMTP branch (which records the
// caller's actual intent).
func (a *API) performSelfSend(
	ctx context.Context,
	agent *identity.AgentIdentity,
	req outbound.SendRequest,
	msgType string,
	parentMessageID string,
	idemCompleteTx AcceptIdemCompleter,
) (*identity.Message, error) {
	email := agent.EmailAddress()

	// Allocate providerID up front so the outbound row and inbound row
	// reference the same Message-ID — matching the two-row shape an
	// SMTP roundtrip produces.
	providerID := loopback.ProviderID(a.fromDomain)

	rawMessage, err := loopback.ComposeMIME(agent, req, providerID, a.fromDomain)
	if err != nil {
		return nil, fmt.Errorf("self-send compose: %w", err)
	}

	// Inbound-leg protection (gate + content scan) over the composed MIME —
	// the same evaluation the relay runs for SMTP inbound. The inbound row id
	// is pre-allocated so the audit rows and deterministic event ids anchor to
	// it. A review/block verdict persists the row as a hidden hold and
	// suppresses email.received (published conditionally below).
	//
	// Known cost: with the content scan enabled, a self-send runs TWO
	// sequential detector passes over byte-identical content within one HTTP
	// request — screenOutbound already scanned it on the egress side
	// (DeliverOutbound), and this is the ingress pass. With the Gemini
	// detector wired, each pass is bounded by its 10s timeout, so worst-case
	// latency and detector spend roughly double per self-send. Deliberate for
	// now (the two passes run different directions/thresholds and the egress
	// engine is heuristics-only); reusing/deduping the detector work is a
	// follow-up candidate.
	inboundID := identity.NewMessageID()
	screenRes, gate := inboundscreen.EvaluateLoopback(ctx, a.inboundScreen, agent, inboundID, rawMessage)

	var outboundMsg *identity.Message
	var receivedEvent webhookpub.Event
	err = a.store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		outboundMsg, txErr = a.store.CreateOutboundMessageThreadedTx(
			ctx, tx, parentMessageID, agent.ID, []string{email}, nil, nil, req.Subject, msgType,
			"loopback", providerID, req.ConversationID, rawMessage,
			"sent", "", "own_address",
		)
		if txErr != nil {
			return fmt.Errorf("self-send outbound row: %w", txErr)
		}

		inboundMsg, txErr := a.store.CreateInboundMessageAuthenticatedTwinInTx(
			ctx, tx, outboundMsg.ID, inboundID, agent.ID, identity.InboundAuth{HeaderFrom: email, StoredSender: loopbackDisplayFrom(req, email)}, email, providerID, req.Subject,
			req.ConversationID, "unread", rawMessage, gate.Flagged, gate.Reason,
			[]string{email}, nil, replyToList(req.ReplyTo), screenRes.Denorm,
		)
		if txErr != nil {
			return fmt.Errorf("self-send inbound row: %w", txErr)
		}
		if _, txErr = tx.Exec(ctx, `UPDATE messages SET method='loopback' WHERE id=$1`, inboundMsg.ID); txErr != nil {
			return fmt.Errorf("self-send inbound method: %w", txErr)
		}
		inboundMsg.Method = "loopback"
		submittedOutbound, txErr := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
			MessageID: outboundMsg.ID, DedupeKey: "submission:local-loopback", Direction: "outbound",
			ReasonCode:     messagelifecycle.ReasonSubmissionLocalLoopbackAccepted,
			CorrelationIDs: messagelifecycle.SafeCorrelationIDs(map[string]string{"email_message_id": providerID}), OccurredAt: time.Now(),
		})
		if txErr != nil {
			return txErr
		}
		acceptedInbound, txErr := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
			MessageID: inboundMsg.ID, DedupeKey: "acceptance", Direction: "inbound",
			ReasonCode:     messagelifecycle.ReasonAcceptanceLocalLoopback,
			CorrelationIDs: messagelifecycle.SafeCorrelationIDs(map[string]string{"email_message_id": providerID}), OccurredAt: inboundMsg.CreatedAt,
		})
		if txErr != nil {
			return txErr
		}

		if a.outbox != nil {
			if receivedEvent, txErr = a.publishLoopbackEventsTx(ctx, tx, agent, outboundMsg, inboundMsg, req, msgType, rawMessage, []messagelifecycle.MessageLifecycleTransition{submittedOutbound}, []messagelifecycle.MessageLifecycleTransition{acceptedInbound}, gate, screenRes); txErr != nil {
				return txErr
			}
		}

		if idemCompleteTx != nil {
			if txErr = idemCompleteTx(ctx, tx, &OutboundResult{
				MessageID: outboundMsg.ID, SentAs: "own_address", Method: "loopback",
			}); txErr != nil {
				return txErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if a.outbox != nil {
		a.emit().OutboxEventsPublished(webhookpub.EventEmailSent)
		if !screenRes.Hold {
			a.emit().OutboxEventsPublished(webhookpub.EventEmailReceived)
		}
		a.emitLoopbackScreeningMetrics(gate, screenRes)
	}
	// Screening audit rows are appended best-effort ONCE after the commit: a
	// crash between commit and this write loses the audit rows (nothing
	// re-drives a committed local delivery; an idempotent client retry replays
	// the cached response without re-screening). Accepted: the verdict itself
	// is durable on the message row; only the drill-down audit is best-effort.
	// The deterministic ids dedupe the rare full re-execution.
	a.writeProtectionEvents(ctx, inboundID, screenRes.Events)
	// receivedEvent.MessageID is empty when delivery was suppressed (held), so
	// the WebSocket push no-ops for holds.
	a.pushLoopbackReceived(ctx, agent.ID, receivedEvent.MessageID)
	return outboundMsg, nil
}

// publishLoopbackEventsTx publishes the outcome events for a loopback local
// delivery inside the delivery transaction. email.sent always fires (the
// outbound leg is terminally delivered); email.received fires only when the
// inbound leg is actually delivered — a review/block hold suppresses it,
// publishing email.review_requested / email.blocked instead (plus
// email.flagged on the delivered gate-flag path), mirroring the relay's
// inbound event semantics. Returns the zero Event when email.received was
// suppressed.
func (a *API) publishLoopbackEventsTx(
	ctx context.Context,
	tx pgx.Tx,
	agent *identity.AgentIdentity,
	outboundMsg, inboundMsg *identity.Message,
	req outbound.SendRequest,
	msgType string,
	rawMessage []byte,
	outboundTransitions, inboundTransitions []messagelifecycle.MessageLifecycleTransition,
	gate inboundpolicy.Decision,
	screenRes inboundscreen.Result,
) (webhookpub.Event, error) {
	sentResult := &outbound.SendResult{
		Method: "loopback", To: []string{agent.EmailAddress()},
		SentAs: "own_address", Raw: rawMessage,
	}
	sentEvent := a.buildSentEvent(agent, outboundMsg, sentResult, req, msgType)
	sentData := sentEvent.Data.(eventpayload.EmailSentData)
	sentData.LifecycleTransitions = outboundTransitions
	sentEvent.Data = sentData
	sentEvent.ID = webhookpub.DeterministicEventID(outboundMsg.ID, webhookpub.EventEmailSent)
	if err := a.outbox.PublishTx(ctx, tx, sentEvent); err != nil {
		return webhookpub.Event{}, fmt.Errorf("self-send email.sent event: %w", err)
	}

	var receivedEvent webhookpub.Event
	if !screenRes.Hold {
		receivedEvent = buildLoopbackReceivedEvent(agent, inboundMsg, req, rawMessage, inboundTransitions)
		if err := a.outbox.PublishTx(ctx, tx, receivedEvent); err != nil {
			return webhookpub.Event{}, fmt.Errorf("self-send email.received event: %w", err)
		}
	}
	for _, ev := range loopback.ScreeningEvents(agent, inboundMsg, gate, screenRes) {
		if err := a.outbox.PublishTx(ctx, tx, ev); err != nil {
			return webhookpub.Event{}, fmt.Errorf("self-send %s event: %w", ev.Type, err)
		}
	}
	return receivedEvent, nil
}

func (a *API) approveSelfSend(
	ctx context.Context,
	agent *identity.AgentIdentity,
	messageID, userID string,
	edits identity.PendingApprovalEdit,
	idemCompleteTx ApproveIdemCompleter,
) (*identity.Message, error) {
	var req outbound.SendRequest
	var receivedEvent webhookpub.Event
	var gate inboundpolicy.Decision
	var screenRes inboundscreen.Result
	var inboundID string
	sent, err := a.store.ApproveAndDeliverLocal(ctx, messageID, userID, edits,
		func(locked *identity.Message) (identity.SendResult, identity.LocalInboundScreen, error) {
			var buildErr error
			req, buildErr = buildSendRequestFromMessage(locked)
			if buildErr != nil {
				return identity.SendResult{}, identity.LocalInboundScreen{}, buildErr
			}
			attachReferencesChain(ctx, a.store, agent.ID, &req)
			if !isSelfSend(req, agent.EmailAddress()) {
				return identity.SendResult{}, identity.LocalInboundScreen{}, errors.New("external outbound approval must be queued")
			}
			providerID := loopback.ProviderID(a.fromDomain)
			raw, composeErr := loopback.ComposeMIME(agent, req, providerID, a.fromDomain)
			if composeErr != nil {
				return identity.SendResult{}, identity.LocalInboundScreen{}, composeErr
			}
			// Inbound-leg protection over the composed MIME — the outbound
			// approval releases the Sent copy; the agent's INBOUND protection
			// then judges the inbox copy exactly as the relay would (a
			// review-configured agent can hold its own approved self-send a
			// second time — intended double-review semantics).
			inboundID = identity.NewMessageID()
			screenRes, gate = inboundscreen.EvaluateLoopback(ctx, a.inboundScreen, agent, inboundID, raw)
			return identity.SendResult{
					ProviderMessageID: providerID,
					Method:            "loopback",
					To:                []string{agent.EmailAddress()},
					Sender:            loopbackDisplayFrom(req, agent.EmailAddress()),
					Raw:               raw,
				}, identity.LocalInboundScreen{
					MessageID:  inboundID,
					Flagged:    gate.Flagged,
					FlagReason: gate.Reason,
					Screening:  screenRes.Denorm,
				}, nil
		},
		func(ctx context.Context, tx pgx.Tx, outboundMsg, inboundMsg *identity.Message, result identity.SendResult, outboundTransitions, inboundTransitions []messagelifecycle.MessageLifecycleTransition) error {
			if a.outbox != nil {
				var hookErr error
				receivedEvent, hookErr = a.publishLoopbackEventsTx(ctx, tx, agent, outboundMsg, inboundMsg, req, outboundMsg.Type, result.Raw, outboundTransitions, inboundTransitions, gate, screenRes)
				if hookErr != nil {
					return hookErr
				}
			}
			if idemCompleteTx != nil {
				return idemCompleteTx(ctx, tx, outboundMsg)
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	if a.outbox != nil {
		a.emit().OutboxEventsPublished(webhookpub.EventEmailSent)
		if !screenRes.Hold {
			a.emit().OutboxEventsPublished(webhookpub.EventEmailReceived)
		}
		a.emitLoopbackScreeningMetrics(gate, screenRes)
	}
	a.writeProtectionEvents(ctx, inboundID, screenRes.Events)
	a.pushLoopbackReceived(ctx, agent.ID, receivedEvent.MessageID)
	return sent, nil
}

// emitLoopbackScreeningMetrics counts the screening-outcome events published
// for a loopback inbound leg (mirrors loopback.ScreeningEvents' conditions),
// so email.flagged/email.blocked/email.review_requested are counted the same
// way as the sent/received pair.
func (a *API) emitLoopbackScreeningMetrics(gate inboundpolicy.Decision, res inboundscreen.Result) {
	if gate.Flagged && !res.Hold {
		a.emit().OutboxEventsPublished(webhookpub.EventEmailFlagged)
	}
	if res.Blocked() {
		a.emit().OutboxEventsPublished(webhookpub.EventEmailBlocked)
	}
	if res.Review() {
		a.emit().OutboxEventsPublished(webhookpub.EventEmailReviewRequested)
	}
}

func (a *API) recordLoopbackUsage(ctx context.Context, userID string, agent *identity.AgentIdentity) {
	// Loopback is guaranteed single-recipient (loopback.IsSelfSend rejects
	// CC/BCC and multi-To), so each leg is exactly one recipient-delivery unit.
	for _, direction := range []string{"outbound", "inbound"} {
		if _, err := a.usage.RecordAndCheck(ctx, userID, agent.ID, agent.Domain, direction, 1); err != nil {
			log.Printf("[api] self-send %s usage recording error: %v", direction, err)
		}
	}
}

func replyToList(replyTo string) []string {
	if replyTo == "" {
		return []string{}
	}
	return []string{replyTo}
}

func buildLoopbackReceivedEvent(agent *identity.AgentIdentity, msg *identity.Message, req outbound.SendRequest, raw []byte, transitions []messagelifecycle.MessageLifecycleTransition) webhookpub.Event {
	data := eventpayload.EmailReceivedData{
		MessageID:            msg.ID,
		AgentEmail:           agent.EmailAddress(),
		Direction:            "inbound",
		ConversationID:       req.ConversationID,
		HeaderFrom:           stringPointer(agent.EmailAddress()),
		EnvelopeFrom:         nil,
		VerifiedDomain:       nil,
		To:                   []string{agent.EmailAddress()},
		CC:                   []string{},
		ReplyTo:              replyToList(req.ReplyTo),
		Authentication:       nil,
		DeliveredTo:          agent.EmailAddress(),
		Subject:              req.Subject,
		ReceivedAt:           msg.CreatedAt.UTC(),
		Attachments:          eventpayload.AttachmentMetadata(raw),
		LifecycleTransitions: transitions,
	}
	e := webhookpub.NewEvent(webhookpub.EventEmailReceived, agent.UserID, data)
	e.ID = webhookpub.DeterministicEventID(msg.ID, webhookpub.EventEmailReceived)
	e.AgentID = agent.ID
	e.ConversationID = req.ConversationID
	e.MessageID = msg.ID
	return e
}

func stringPointer(value string) *string { return &value }

func loopbackDisplayFrom(req outbound.SendRequest, agentEmail string) string {
	if req.ReplyTo != "" {
		if address, err := mail.ParseAddress(req.ReplyTo); err == nil {
			return address.Address
		}
		return req.ReplyTo
	}
	return agentEmail
}

func (a *API) pushLoopbackReceived(ctx context.Context, agentID, messageID string) {
	if a.wsHub == nil || messageID == "" || !a.wsHub.IsConnected(agentID) {
		return
	}
	payload, err := a.store.GetEventEnvelope(ctx, messageID, webhookpub.EventEmailReceived)
	if err != nil || len(payload) == 0 {
		return
	}
	a.wsHub.Send(agentID, payload)
}
