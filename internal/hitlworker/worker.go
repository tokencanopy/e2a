// Package hitlworker runs the periodic sweep that finalizes pending_review
// holds whose TTL has elapsed. Outbound holds become sent (auto-approved) or
// review_expired_rejected; inbound holds become review_expired_approved
// (released to the agent) or review_expired_rejected — per the owning agent's
// hitl_expiration_action column. Message content remains retained in both cases.
package hitlworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/inboundpolicy"
	"github.com/tokencanopy/e2a/internal/inboundscreen"
	"github.com/tokencanopy/e2a/internal/logredact"
	"github.com/tokencanopy/e2a/internal/loopback"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/piguard"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// OutboundEnqueuer inserts an outbound_send job (QueueOutbound) in the caller's
// transaction. Satisfied by *outboundsend.Jobs. The sweep hands an approved
// outbound send to the queue-first pipeline — transitioning
// the hold to review_expired_approved + delivery_status='accepted' and enqueuing —
// instead of blocking on Sender.Send. Self-sends never use it (they loopback).
type OutboundEnqueuer interface {
	EnqueueSendTx(ctx context.Context, tx pgx.Tx, messageID string) (int64, error)
	// EnqueueScheduledSendTx enqueues the send to run no earlier than `at`, used
	// when an auto-approved hold carried a still-future send_at (#815). Same
	// outbox tx as EnqueueSendTx; only the job's first-run time differs.
	EnqueueScheduledSendTx(ctx context.Context, tx pgx.Tx, messageID string, at time.Time) (int64, error)
}

type ManagedUnsubscribeIssuer interface {
	Issue(ctx context.Context, userID, agentID, recipient string) (string, error)
}

type WebSocketHub interface {
	IsConnected(agentID string) bool
	Send(agentID string, msg []byte) bool
}

// DefaultBatchSize caps how many rows one sweep will try to finalize. The
// partial index on (approval_expires_at) WHERE status='pending_review'
// keeps the list query cheap regardless of total table size.
const DefaultBatchSize = 100

// Worker runs the TTL sweep. Construct with New; its RunOnce is driven on a
// schedule by the River maintenance periodic (see maintenance.go).
type Worker struct {
	store      *identity.Store
	sender     *outbound.Sender
	usage      usage.UsageTracker
	fromDomain string
	batchSize  int
	// publisher fires the review-resolution webhook when the sweep auto-resolves
	// a hold, so a TTL-resolved hold notifies subscribers exactly like a
	// human-resolved one (the user-driven path emits review_approved/rejected
	// from internal/agent). Optional — nil leaves the sweep silent (legacy
	// behavior). Wired via SetPublisher.
	publisher webhookpub.Publisher
	// outbox writes the terminal loopback email.sent/email.received pair in the
	// same transaction as the expired hold's Sent/Inbox persistence.
	outbox webhookpub.Outbox
	// outboundEnq routes an approved external send onto QueueOutbound. Main always
	// wires it; a nil value fails closed and leaves the hold pending. Self-sends use
	// the local loopback path.
	outboundEnq       OutboundEnqueuer
	unsubscribeIssuer ManagedUnsubscribeIssuer
	wsHub             WebSocketHub
	// footerResolver decides whether a TTL-auto-approved external send carries
	// the operator-configured outbound footer (SendRequest.AppendOutboundFooter).
	// Held rows do not persist the flag — every approval funnel resolves it at
	// compose time — so the sweep needs the same per-account decision the human
	// approve paths use (main wires agent.API.OutboundFooterForAccount).
	// Optional and fail-closed: nil (self-host default, feature off) means the
	// sweep never appends a footer.
	footerResolver func(ctx context.Context, userID string) bool
	// inboundScreen runs the agent's inbound protection over the loopback
	// inbound leg of a TTL-auto-approved self-send — the same engine
	// construction (heuristics + optional Gemini) the relay uses for SMTP
	// inbound, so the released inbox copy is judged identically to a wire
	// roundtrip of the same message.
	inboundScreen *piguard.Engine
}

// SetPublisher wires the webhook publisher used to emit review-resolution events
// on TTL auto-resolution. Without it the sweep transitions rows silently.
func (w *Worker) SetPublisher(p webhookpub.Publisher) { w.publisher = p }

// SetOutbox wires the transactional outcome-event writer for providerless
// local delivery. Production uses the same unconditional outbox as all other
// message triggers.
func (w *Worker) SetOutbox(o webhookpub.Outbox) { w.outbox = o }

// SetOutboundEnqueuer wires the mandatory outbound send enqueuer. Two-phase
// wiring: pass the *outboundsend.Jobs pointer; its shared River client is injected
// later via the jobs client's SetEnqueuer.
func (w *Worker) SetOutboundEnqueuer(e OutboundEnqueuer) { w.outboundEnq = e }

func (w *Worker) SetManagedUnsubscribeIssuer(i ManagedUnsubscribeIssuer) { w.unsubscribeIssuer = i }
func (w *Worker) SetWebSocketHub(h WebSocketHub)                         { w.wsHub = h }

// SetOutboundFooterResolver wires the per-account outbound-footer decision for
// TTL auto-approved sends. See the footerResolver field for semantics.
func (w *Worker) SetOutboundFooterResolver(r func(ctx context.Context, userID string) bool) {
	w.footerResolver = r
}

// New constructs a Worker. fromDomain is the deployment's outbound
// from-domain (cfg.OutboundSMTP.FromDomain) — used by the self-send
// loopback branch to stamp the synthetic Message-ID / Received headers
// the same way internal/agent does on the user-driven approve path.
// Pass "" if the deployment has no outbound relay configured; the
// loopback path falls back to "e2a.local" for the host portion.
func New(store *identity.Store, sender *outbound.Sender, usageTracker usage.UsageTracker, fromDomain string) *Worker {
	return &Worker{
		store:         store,
		sender:        sender,
		usage:         usageTracker,
		fromDomain:    fromDomain,
		batchSize:     DefaultBatchSize,
		inboundScreen: inboundscreen.BuildEngine(),
	}
}

// RunOnce performs a single sweep of both queues (outbound holds, then inbound
// review holds). This is the sweep body the River maintenance periodic drives on
// a schedule (see maintenance.go); it's also called directly by tests for
// deterministic behavior. Returns nil — both sweeps log and swallow their own
// per-row/query errors internally (a transient DB blip should not spin River's
// retry machinery); the error return satisfies the Sweeper interface.
func (w *Worker) RunOnce(ctx context.Context) error {
	w.sweep(ctx)
	w.sweepReviews(ctx)
	return nil
}

// sweepReviews auto-resolves expired INBOUND review holds. Both directions share
// the pending_review status (unified — design 2026-06-22); ListExpiredReviews is
// direction='inbound'-scoped, so this never touches an outbound hold (those are the
// `sweep` path, where approve = send). Inbound: approve = release the held message
// to the agent's inbox (it becomes visible), reject = drop it. The compare-and-set
// status guard in the store methods makes concurrent/duplicate sweeps safe.
func (w *Worker) sweepReviews(ctx context.Context) {
	candidates, err := w.store.ListExpiredReviews(ctx, w.batchSize)
	if err != nil {
		log.Printf("[hitl-worker] list expired reviews: %v", err)
		return
	}
	for _, c := range candidates {
		// Capture the dispatch view + owner BEFORE the transition: a reject
		// makes the row terminal/hidden, and the resolution event mirrors the
		// human path's payload (sender/subject) and routes on the owner. A
		// lookup failure means we still resolve the hold but skip the event
		// (better than stranding the row).
		meta, mErr := w.store.GetReviewMessage(ctx, c.MessageID, c.AgentID)
		ownerUserID := ""
		if ag, aErr := w.store.GetAgentByID(ctx, c.AgentID); aErr == nil && ag != nil {
			ownerUserID = ag.UserID
		}
		canEmit := mErr == nil && meta != nil && ownerUserID != ""

		if c.ExpirationAction == identity.HITLExpirationApprove {
			transition, err := w.store.ExpireApproveReviewWithTransition(ctx, c.MessageID)
			if err != nil {
				if err != identity.ErrNotPendingReview {
					log.Printf("[hitl-worker] expire-approve review %s: %v", c.MessageID, err)
				}
				continue // not transitioned by us → don't emit
			}
			if canEmit {
				w.emitInboundResolved(meta, ownerUserID, true, "", transition)
			}
		} else {
			transition, err := w.store.ExpireRejectReviewWithTransition(ctx, c.MessageID, "ttl_expired")
			if err != nil {
				if err != identity.ErrNotPendingReview {
					log.Printf("[hitl-worker] expire-reject review %s: %v", c.MessageID, err)
				}
				continue
			}
			if canEmit {
				w.emitInboundResolved(meta, ownerUserID, false, "ttl_expired", transition)
			}
		}
	}
}

func (w *Worker) sweep(ctx context.Context) {
	candidates, err := w.store.ListExpiredPending(ctx, w.batchSize)
	if err != nil {
		log.Printf("[hitl-worker] list expired: %v", err)
		return
	}
	for _, c := range candidates {
		w.processOne(ctx, c)
	}
}

func (w *Worker) processOne(ctx context.Context, c identity.ExpirationCandidate) {
	if c.ExpirationAction == identity.HITLExpirationApprove {
		w.autoApprove(ctx, c)
		return
	}
	w.autoReject(ctx, c.MessageID, "ttl_expired")
}

func (w *Worker) autoApprove(ctx context.Context, c identity.ExpirationCandidate) {
	agent, err := w.store.GetAgentByID(ctx, c.AgentID)
	if err != nil {
		// Not-found means the agent was hard-deleted or moved to the trash
		// between the sweep's candidate list and this load (GetAgentByID
		// excludes trashed agents — migration 063). SKIP, don't terminally
		// reject: a trashed inbox's holds must come back intact on restore
		// (RestoreAgent shifts their approval TTLs), and a hard-deleted
		// agent's rows are gone anyway.
		if errors.Is(err, pgx.ErrNoRows) {
			log.Printf("[hitl-worker] auto-approve %s: agent %s gone or trashed — skipping", c.MessageID, c.AgentID)
			return
		}
		log.Printf("[hitl-worker] auto-approve %s: agent lookup failed: %v", c.MessageID, err)
		w.autoReject(ctx, c.MessageID, fmt.Sprintf("auto-approve failed: agent lookup: %v", err))
		return
	}
	if !agent.DomainVerified {
		log.Printf("[hitl-worker] auto-approve %s: agent %s not verified", c.MessageID, agent.ID)
		w.autoReject(ctx, c.MessageID, "auto-approve failed: agent domain not verified")
		return
	}

	if w.outboundEnq == nil {
		log.Printf("[hitl-worker] auto-approve %s: outbound delivery queue unavailable", c.MessageID)
		return
	}
	// Hand external delivery to QueueOutbound. false means this is a self-send,
	// which uses the local loopback path below.
	if w.autoApproveAsync(ctx, agent, c) {
		return
	}
	w.autoApproveLoopback(ctx, agent, c)
}

// autoApproveAsync transitions the hold to review_expired_approved +
// delivery_status='accepted' and enqueues an outbound_send job; the SendWorker does
// the actual submit + email.sent/failed + metering. Returns false (handled nothing)
// ONLY when the message is a self-send — the caller then uses the local loopback
// path. Any other outcome (queued, already-resolved, transient failure left for the
// next cycle, or a permanent-draft reject) returns true.
func (w *Worker) autoApproveAsync(ctx context.Context, agent *identity.AgentIdentity, c identity.ExpirationCandidate) bool {
	msg, err := w.store.LoadOutboundDraft(ctx, c.MessageID)
	if err != nil {
		if errors.Is(err, identity.ErrMessageNotFound) {
			return true // gone — no-op
		}
		log.Printf("[hitl-worker] auto-approve %s: load draft: %v", c.MessageID, err)
		return true // transient — leave pending_review for the next cycle
	}
	if msg.Status != identity.MessageStatusPendingReview {
		return true // resolved by a human/other worker
	}
	req, err := sendRequestFromStoredMessage(msg)
	if err != nil {
		w.autoReject(ctx, c.MessageID, fmt.Sprintf("auto-approve failed: rebuild request: %v", err))
		return true
	}
	// Suppression enforcement on TTL auto-approval: never submit to an address
	// in the owning account-wide or exact-agent suppression scope (the store
	// normalizes both sides, so case differences still match). The check runs on the final
	// stored To/CC/BCC set, before the self-send branch, mirroring the
	// accept-time and human-approve checks. A match resolves the expired hold
	// through the existing rejected/expired lifecycle (review_expired_rejected
	// + review-resolution event) with an explicit suppression reason; a store
	// error is treated as transient — leave the row pending_review for the
	// next sweep rather than sending unchecked or terminally rejecting on a
	// DB blip.
	recipients := make([]string, 0, len(req.To)+len(req.CC)+len(req.BCC))
	recipients = append(recipients, req.To...)
	recipients = append(recipients, req.CC...)
	recipients = append(recipients, req.BCC...)
	suppressed, err := w.store.EffectiveSuppressions(ctx, agent.UserID, agent.ID, recipients)
	if err != nil {
		log.Printf("[hitl-worker] auto-approve %s: suppression check: %v (leaving pending for next sweep)", c.MessageID, err)
		return true
	}
	if len(suppressed) > 0 {
		// Precise addresses on purpose: this reason string is CUSTOMER-FACING
		// output, not a log line — it is persisted to messages.rejection_reason,
		// returned by the HITL REST API, and carried in the
		// email.review_rejected webhook payload. The owner is entitled to know
		// exactly which of THEIR OWN recipients is suppressed (two suppressed
		// recipients can share a domain). Log-side redaction happens in
		// autoReject, which is where the log copy is produced.
		w.autoReject(ctx, c.MessageID, "auto-approve blocked: recipient(s) on the suppression list: "+strings.Join(suppressed, ", "))
		return true
	}
	w.attachReferencesChain(ctx, agent.ID, &req)
	// A held platform test (type="test") targets the agent's own address by
	// design, so the self-send predicate below would silently reroute its TTL
	// auto-approval to local loopback — dropping the real SMTP → inbound
	// round-trip the test exists to exercise. Keep it platform-originated
	// (noreply@<from_domain>) instead, mirroring the human-approve path in
	// internal/agent/hitl_api.go.
	isPlatformTest := msg.Type == "test"
	if !isPlatformTest && loopback.IsSelfSend(req, agent.EmailAddress()) {
		return false // self-send — fall through to the local loopback path
	}
	if req.Unsubscribe != nil {
		_, _, _, finalRecipients, nerr := outbound.NormalizeRecipients(agent, w.fromDomain, req)
		if nerr != nil || len(finalRecipients) != 1 {
			w.autoReject(ctx, c.MessageID, "auto-approve failed: managed unsubscribe requires exactly one recipient")
			return true
		}
		if w.unsubscribeIssuer == nil {
			log.Printf("[hitl-worker] auto-approve %s: managed unsubscribe unavailable (leaving pending)", c.MessageID)
			return true
		}
		link, ierr := w.unsubscribeIssuer.Issue(ctx, agent.UserID, agent.ID, finalRecipients[0])
		if ierr != nil {
			log.Printf("[hitl-worker] auto-approve %s: managed unsubscribe issue: %v (leaving pending)", c.MessageID, ierr)
			return true
		}
		req.Unsubscribe.URL = link
	}
	var comp *outbound.ComposeResult
	if isPlatformTest {
		comp, err = w.sender.ComposePlatformForAccept(req)
	} else {
		// Outbound footer: TTL auto-approval composes here, so the footer
		// decision is resolved here — the same approval-time semantics as the
		// human approve funnel (internal/agent approveOutboundAsyncComposed).
		// Without this, an expires-to-approve hold would ship unfootered while
		// the identical hold approved by a human a minute earlier would not.
		// Fail-closed: nil resolver (feature off / self-host) = no footer.
		if w.footerResolver != nil {
			req.AppendOutboundFooter = w.footerResolver(ctx, agent.UserID)
		}
		comp, err = w.sender.ComposeForAccept(agent, req)
	}
	if err != nil {
		// Compose failures are deterministic (bad addresses / no visible
		// recipients) — a retry can't fix them, so reject the draft.
		w.autoReject(ctx, c.MessageID, fmt.Sprintf("auto-approve failed: compose: %v", err))
		return true
	}
	acc := identity.AcceptedSend{
		To: comp.To, CC: comp.CC, BCC: comp.BCC, Subject: req.Subject,
		Method: comp.Method, EnvelopeFrom: comp.EnvelopeFrom, SentAs: comp.SentAs, Raw: comp.Raw,
	}
	sent, err := w.store.ApproveAndAccept(ctx, c.MessageID, "", identity.MessageStatusReviewExpiredApproved, false, acc, w.outboundEnq.EnqueueSendTx, w.outboundEnq.EnqueueScheduledSendTx, nil)
	if err != nil {
		if errors.Is(err, identity.ErrNotPendingApproval) {
			return true // resolved between load and transition
		}
		// Transient tx/enqueue failure: leave the row pending_review for the next
		// cycle. Do NOT autoReject — no send happened, so this is not a "stuck" send.
		log.Printf("[hitl-worker] auto-approve %s: accept+enqueue: %v", c.MessageID, err)
		return true
	}
	log.Printf("[mail:%s] dir=outbound type=%s status=%s agent=%s to_count=%d to_domains=%v auto_approved=true delivery=async",
		sent.ID, sent.Type, sent.Status, agent.ID, len(sent.ToRecipients), logredact.AddressDomains(sent.ToRecipients))
	// review_approved fires now (hold resolved to approved); the delivery outcome
	// arrives later via email.sent/email.failed from the SendWorker. No metering
	// here — the SendWorker meters on MarkSent.
	w.emitOutboundApproved(agent, sent)
	return true
}

func (w *Worker) autoApproveLoopback(ctx context.Context, agent *identity.AgentIdentity, c identity.ExpirationCandidate) {
	var req outbound.SendRequest
	var receivedEvent webhookpub.Event
	var gate inboundpolicy.Decision
	var screenRes inboundscreen.Result
	var inboundID string
	sent, err := w.store.ExpireAndDeliverLocal(ctx, c.MessageID,
		func(msg *identity.Message) (identity.SendResult, identity.LocalInboundScreen, error) {
			var err error
			req, err = sendRequestFromStoredMessage(msg)
			if err != nil {
				return identity.SendResult{}, identity.LocalInboundScreen{}, err
			}
			w.attachReferencesChain(ctx, agent.ID, &req)
			// Self-sends bypass the SMTP relay — outbound.Sender would
			// strip the agent's own address from the recipient list and
			// error "no valid recipients", which the worker would then
			// interpret as a send failure and auto-REJECT the row,
			// silently inverting the operator-configured
			// hitl_expiration_action="approve" policy. Loopback writes
			// the inbound row directly and reports method=loopback on
			// the now-sent outbound row, matching the user-driven
			// approve paths in internal/agent/hitl_api.go and
			// internal/agent/hitl_magic_api.go.
			if !loopback.IsSelfSend(req, agent.EmailAddress()) {
				return identity.SendResult{}, identity.LocalInboundScreen{}, errors.New("external outbound approval must be queued")
			}
			providerID := loopback.ProviderID(w.fromDomain)
			raw, err := loopback.ComposeMIME(agent, req, providerID, w.fromDomain)
			if err != nil {
				return identity.SendResult{}, identity.LocalInboundScreen{}, err
			}
			// Inbound-leg protection over the composed MIME — the TTL
			// approval releases the Sent copy; the agent's INBOUND protection
			// then judges the inbox copy exactly as the relay would (it may
			// hold it again as an inbound review — intended double-review
			// semantics, matching the user-driven approve path).
			inboundID = identity.NewMessageID()
			screenRes, gate = inboundscreen.EvaluateLoopback(ctx, w.inboundScreen, agent, inboundID, raw)
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
			if w.outbox == nil {
				return nil
			}
			var eventErr error
			receivedEvent, eventErr = w.publishLoopbackOutcomeEventsTx(ctx, tx, agent, outboundMsg, inboundMsg, req, result, outboundTransitions, inboundTransitions, gate, screenRes)
			return eventErr
		})
	if err != nil {
		// ErrNotPendingApproval means another worker (or a human) handled
		// the row between list-and-lock. Treat as a no-op.
		if err == identity.ErrNotPendingApproval {
			return
		}
		// ErrSendInProgress means another worker is mid-send for this
		// row (send_attempts is 'attempting' and not yet stale). Don't
		// auto-reject — that would invert the operator-configured
		// expiration_action="approve" policy by terminally rejecting a
		// message that may have actually been sent. Skip silently; the
		// next poll either sees status='sent' (the in-flight worker
		// committed) or the row goes stale (10min window) and another
		// worker takes over.
		if err == identity.ErrSendInProgress {
			return
		}
		log.Printf("[hitl-worker] auto-approve %s: send failed: %v", c.MessageID, err)
		w.autoReject(ctx, c.MessageID, fmt.Sprintf("auto-approve send failed: %v", err))
		return
	}
	// Screening audit rows are appended best-effort ONCE after the commit: a
	// crash between the commit and this loop loses the audit rows for good —
	// unlike the relay, nothing re-drives an already-delivered local sweep
	// (the deterministic ids only dedupe the rare case where the same hold is
	// re-processed end-to-end). Accepted: the verdict itself is durable on the
	// message row; only the drill-down audit is best-effort.
	for _, ev := range screenRes.Events {
		if perr := w.store.CreateProtectionEvent(ctx, ev); perr != nil {
			log.Printf("[mail:%s] screening_event write failed (%s/%s): %v", inboundID, ev.Source, ev.Reason, perr)
		}
	}
	// receivedEvent.MessageID is empty when delivery was suppressed (held), so
	// the WebSocket push no-ops for holds.
	w.pushLoopbackReceived(ctx, agent.ID, receivedEvent.MessageID)
	// External sends are metered by the outbound worker after provider success.
	// Loopback is terminal here and persisted both a Sent and an Inbox copy, so
	// account for both directions after the transaction commits.
	for _, direction := range []string{"outbound", "inbound"} {
		if _, err := w.usage.RecordAndCheck(ctx, agent.UserID, agent.ID, agent.Domain, direction); err != nil {
			log.Printf("[hitl-worker] %s usage recording error: %v", direction, err)
		}
	}

	log.Printf("[mail:%s] dir=outbound type=%s status=%s agent=%s to_count=%d to_domains=%v auto_sent=true",
		sent.ID, sent.Type, sent.Status, agent.ID, len(sent.ToRecipients), logredact.AddressDomains(sent.ToRecipients))
	// Mirror the user-driven approve: fire email.review_approved (the send
	// already happened; this is the post-side-effect notification).
	w.emitOutboundApproved(agent, sent)
}

// publishLoopbackOutcomeEventsTx publishes the terminal loopback events inside
// the delivery transaction. email.sent always fires; email.received only when
// the inbound leg was actually delivered — an inbound-screening hold
// (review/block) suppresses it, publishing email.review_requested /
// email.blocked (plus email.flagged on the delivered gate-flag path) instead,
// mirroring the relay's inbound event semantics. Returns the zero Event when
// email.received was suppressed.
func (w *Worker) publishLoopbackOutcomeEventsTx(
	ctx context.Context,
	tx pgx.Tx,
	agent *identity.AgentIdentity,
	outboundMsg, inboundMsg *identity.Message,
	req outbound.SendRequest,
	result identity.SendResult,
	outboundTransitions, inboundTransitions []messagelifecycle.MessageLifecycleTransition,
	gate inboundpolicy.Decision,
	screenRes inboundscreen.Result,
) (webhookpub.Event, error) {
	sentData := eventpayload.EmailSentData{
		MessageID:            outboundMsg.ID,
		AgentEmail:           agent.EmailAddress(),
		Direction:            "outbound",
		ConversationID:       outboundMsg.ConversationID,
		Method:               "loopback",
		From:                 agent.EmailAddress(),
		To:                   []string{agent.EmailAddress()},
		CC:                   []string{},
		BCC:                  []string{},
		Subject:              outboundMsg.Subject,
		MessageType:          outboundMsg.Type,
		LifecycleTransitions: outboundTransitions,
	}
	sentEvent := webhookpub.NewEvent(webhookpub.EventEmailSent, agent.UserID, sentData)
	sentEvent.ID = webhookpub.DeterministicEventID(outboundMsg.ID, webhookpub.EventEmailSent)
	sentEvent.AgentID = agent.ID
	sentEvent.ConversationID = outboundMsg.ConversationID
	sentEvent.MessageID = outboundMsg.ID
	if err := w.outbox.PublishTx(ctx, tx, sentEvent); err != nil {
		return webhookpub.Event{}, fmt.Errorf("self-send email.sent event: %w", err)
	}

	var receivedEvent webhookpub.Event
	if !screenRes.Hold {
		replyTo := []string{}
		if req.ReplyTo != "" {
			replyTo = []string{req.ReplyTo}
		}
		receivedData := eventpayload.EmailReceivedData{
			MessageID:            inboundMsg.ID,
			AgentEmail:           agent.EmailAddress(),
			Direction:            "inbound",
			ConversationID:       inboundMsg.ConversationID,
			HeaderFrom:           stringPointer(agent.EmailAddress()),
			VerifiedDomain:       nil,
			To:                   []string{agent.EmailAddress()},
			CC:                   []string{},
			ReplyTo:              replyTo,
			DeliveredTo:          agent.EmailAddress(),
			Subject:              inboundMsg.Subject,
			ReceivedAt:           inboundMsg.CreatedAt.UTC(),
			Attachments:          eventpayload.AttachmentMetadata(result.Raw),
			LifecycleTransitions: inboundTransitions,
		}
		receivedEvent = webhookpub.NewEvent(webhookpub.EventEmailReceived, agent.UserID, receivedData)
		receivedEvent.ID = webhookpub.DeterministicEventID(inboundMsg.ID, webhookpub.EventEmailReceived)
		receivedEvent.AgentID = agent.ID
		receivedEvent.ConversationID = inboundMsg.ConversationID
		receivedEvent.MessageID = inboundMsg.ID
		if err := w.outbox.PublishTx(ctx, tx, receivedEvent); err != nil {
			return webhookpub.Event{}, fmt.Errorf("self-send email.received event: %w", err)
		}
	}
	for _, ev := range loopback.ScreeningEvents(agent, inboundMsg, gate, screenRes) {
		if err := w.outbox.PublishTx(ctx, tx, ev); err != nil {
			return webhookpub.Event{}, fmt.Errorf("self-send %s event: %w", ev.Type, err)
		}
	}
	return receivedEvent, nil
}

func loopbackDisplayFrom(req outbound.SendRequest, agentEmail string) string {
	if req.ReplyTo != "" {
		if address, err := mail.ParseAddress(req.ReplyTo); err == nil {
			return address.Address
		}
		return req.ReplyTo
	}
	return agentEmail
}

func stringPointer(value string) *string { return &value }

func (w *Worker) pushLoopbackReceived(ctx context.Context, agentID, messageID string) {
	if w.wsHub == nil || messageID == "" || !w.wsHub.IsConnected(agentID) {
		return
	}
	payload, err := w.store.GetEventEnvelope(ctx, messageID, webhookpub.EventEmailReceived)
	if err != nil || len(payload) == 0 {
		return
	}
	w.wsHub.Send(agentID, payload)
}

// autoReject resolves a stuck/blocked hold to rejected. `reason` is customer-
// facing and must stay precise: it is written to messages.rejection_reason,
// surfaced by the HITL REST API, and emitted in email.review_rejected. It is
// therefore NOT log-safe — callers build it from suppressed recipient lists and
// from %v of arbitrary compose/validation errors, which routinely embed a
// recipient address (see internal/outbound.platform "invalid To address: %v").
// The redaction belongs here, at the log sink, not at the source: the two
// log.Printf calls below carry reason_len only. Operators recover the exact
// reason from the message row keyed by the message id already on the line.
func (w *Worker) autoReject(ctx context.Context, messageID, reason string) {
	reasonLen := utf8.RuneCountInString(reason)
	rejected, err := w.store.ExpireReject(ctx, messageID, reason)
	if err != nil {
		if err == identity.ErrNotPendingApproval {
			return
		}
		// This is the worst-case path: auto-approve already failed (or
		// the policy was reject), and now the rejection write fails too.
		// The row is stuck in pending_review until an operator
		// intervenes. Tag the log line so monitors / alerting can match
		// on it specifically — distinct from routine "[hitl-worker]"
		// noise.
		log.Printf("[hitl-stuck] message=%s reason_len=%d reject_error=%v ACTION=needs_manual_intervention",
			messageID, reasonLen, err)
		return
	}
	log.Printf("[mail:%s] dir=outbound type=%s status=%s agent=%s reason_len=%d auto_rejected=true",
		rejected.ID, rejected.Type, rejected.Status, rejected.AgentID, reasonLen)
	w.emitOutboundRejected(ctx, rejected, reason)
}

// attachReferencesChain rebuilds the References chain on a HITL-approved
// SendRequest by looking up the parent message's raw message via
// email_message_id. The lookup is direction-agnostic: a held reply's parent
// may be an outbound the agent sent (reply-to-own-message), not only a
// received inbound. Duplicates the equivalent helper in internal/agent for the
// same reason sendRequestFromStoredMessage does — keep this low-level package
// free of upward imports. See that helper's docstring for the full rationale.
func (w *Worker) attachReferencesChain(ctx context.Context, agentID string, req *outbound.SendRequest) {
	if req.ReplyToMessageID == "" {
		return
	}
	parent, err := w.store.GetMessageByEmailMessageID(ctx, agentID, req.ReplyToMessageID)
	if err != nil || parent == nil {
		return
	}
	req.References = outbound.BuildReferencesChain(parent.RawMessage, req.ReplyToMessageID)
}

// sendRequestFromStoredMessage reconstructs a SendRequest from a locked
// pending-approval row. Duplicates the equivalent helper in internal/agent
// to avoid an upward import from this low-level package.
func sendRequestFromStoredMessage(m *identity.Message) (outbound.SendRequest, error) {
	var attachments []outbound.Attachment
	if len(m.AttachmentsJSON) > 0 {
		if err := json.Unmarshal(m.AttachmentsJSON, &attachments); err != nil {
			return outbound.SendRequest{}, err
		}
	}
	// Carry a caller-supplied Reply-To override (persisted single-element on the
	// held row's reply_to column) through the TTL auto-approve recompose, so an
	// expired-but-approved send keeps the same Reply-To a human approval would.
	var replyTo string
	if len(m.ReplyTo) > 0 {
		replyTo = m.ReplyTo[0]
	}
	replyToMessageID := ""
	if m.Type == "reply" {
		replyToMessageID = m.EmailMessageID
	}
	return outbound.SendRequest{
		To:               m.ToRecipients,
		CC:               m.CC,
		BCC:              m.BCC,
		Subject:          m.Subject,
		Body:             m.BodyText,
		HTMLBody:         m.BodyHTML,
		ReplyTo:          replyTo,
		ReplyToMessageID: replyToMessageID,
		ConversationID:   m.ConversationID,
		Attachments:      attachments,
		Unsubscribe:      outbound.ManagedUnsubscribeIntent(m.ManagedUnsubscribe),
	}, nil
}
