package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/logredact"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// approveRequest is the JSON body accepted by the approve endpoint. Every
// field is optional; any field present overrides the stored value before
// the message is sent. Using pointer types distinguishes "field not
// provided" (nil) from "explicitly empty" (non-nil pointer to zero value).
// The body field names match send/reply: the wire names are `text` and
// `html` (Go fields stay BodyText/BodyHTML).
type approveRequest struct {
	Subject     *string                `json:"subject,omitempty" maxLength:"2000"`
	BodyText    *string                `json:"text,omitempty" maxLength:"1048576"`
	BodyHTML    *string                `json:"html,omitempty" maxLength:"1048576"`
	To          *[]string              `json:"to,omitempty" nullable:"false" maxLength:"320" doc:"Override primary recipients. The message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters."`
	CC          *[]string              `json:"cc,omitempty" nullable:"false" maxLength:"320" doc:"Override Cc recipients. The message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters."`
	BCC         *[]string              `json:"bcc,omitempty" nullable:"false" maxLength:"320" doc:"Override Bcc recipients. The message is limited to 50 recipients across to, cc, and bcc combined. Each recipient string (display name + address combined) is limited to 320 characters."`
	Attachments *[]outbound.Attachment `json:"attachments,omitempty"`
}

func (req approveRequest) toEdit() (identity.PendingApprovalEdit, error) {
	e := identity.PendingApprovalEdit{
		Subject:  req.Subject,
		BodyText: req.BodyText,
		BodyHTML: req.BodyHTML,
	}
	if req.To != nil {
		e.To = *req.To
	}
	if req.CC != nil {
		e.CC = *req.CC
	}
	if req.BCC != nil {
		e.BCC = *req.BCC
	}
	if req.Attachments != nil {
		attJSON, err := json.Marshal(*req.Attachments)
		if err != nil {
			return identity.PendingApprovalEdit{}, err
		}
		e.AttachmentsJSON = attJSON
		e.AttachmentsSet = true
	}
	return e, nil
}

// ApproveOverrides are the optional reviewer edits applied on approve
// (exported alias of the internal body type so the v1 httpapi layer can build
// them).
type ApproveOverrides = approveRequest

// ApprovePendingCore is the HTTP-free core of the HITL approve→send: it
// verifies the held message (ownership-scoped + pending + optional
// expected-agent-email match + domain-verified), then runs ApproveAndSend
// with the shared send callback (self-send loopback / SES), records usage,
// and publishes the approved event. Both the legacy handler and the v1 layer
// call it. expectedAgentEmail (when non-empty) must equal the message's
// agent's email — mirrors the legacy verifyURLAgentEmail URL guard.
// On a nil-error return the local loopback or async enqueue has committed;
// the idempotency key must be Completed (cached), never Released. For queued delivery
// idemCompleteTx is invoked inside the approve-and-enqueue transaction.
func (a *API) ApprovePendingCore(ctx context.Context, userID, messageID, expectedAgentEmail string, ovr ApproveOverrides, idemCompleteTx ApproveIdemCompleter) (*identity.Message, *OutboundError) {
	edits, err := ovr.toEdit()
	if err != nil {
		return nil, &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: "invalid attachments"}
	}
	preview, err := a.store.GetOutboundMessageForUser(ctx, messageID, userID)
	if err != nil {
		return nil, &OutboundError{Status: http.StatusNotFound, Code: "not_found", Msg: "message not found"}
	}
	if preview.Status != identity.MessageStatusPendingReview {
		return nil, &OutboundError{Status: http.StatusConflict, Code: "message_not_pending", Msg: "message is not pending approval"}
	}
	agent, err := a.store.GetAgentByID(ctx, preview.AgentID)
	if err != nil {
		log.Printf("[api] approve: get agent %s: %v", preview.AgentID, err)
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "agent lookup failed"}
	}
	if expectedAgentEmail != "" && agent.Email != expectedAgentEmail {
		return nil, &OutboundError{Status: http.StatusNotFound, Code: "not_found", Msg: "message not found"}
	}
	if !agent.DomainVerified {
		return nil, &OutboundError{Status: http.StatusForbidden, Code: "domain_not_verified", Msg: "agent domain must be verified before sending"}
	}

	// Composed-message hard cap. A reviewer's edits (new subject/body/attachments)
	// are merged onto the stored draft, so the true composed size must be checked
	// on the MERGED message — not just the override fields. The per-field maxLength
	// (schema layer) and validateAttachments (httpapi) already bound each field and
	// each attachment individually, but a reviewer can still push a previously-valid
	// draft over the SES stored-message ceiling by editing the body on top of the
	// original attachments (or replacing attachments while the original body stays).
	// Mirror the send/reply/forward composed-cap check (httpapi.deliver) here so the
	// approve-override path can't bypass it. Applied on a copy so preview is
	// untouched (both async and sync dispatch below re-derive from it).
	merged := *preview
	edits.Apply(&merged)
	mergedReq, err := buildSendRequestFromMessage(&merged)
	if err != nil {
		return nil, &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: "invalid attachments"}
	}
	if total := outbound.ComposedSize(mergedReq.Subject, mergedReq.Body, mergedReq.HTMLBody, mergedReq.Attachments); total > outbound.MaxComposedMessageBytes {
		return nil, &OutboundError{Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large", Msg: fmt.Sprintf("composed message too large — %d bytes (subject + text + html + decoded attachments), limit is %d (%d MB)",
			total, outbound.MaxComposedMessageBytes, outbound.MaxComposedMessageBytes/(1024*1024)), Details: map[string]any{
			"scope":        "composed_message",
			"actual_bytes": total,
			"max_bytes":    outbound.MaxComposedMessageBytes,
		}}
	}

	// Suppression enforcement on the approval path. The reviewer's overrides
	// are already merged into mergedReq, so this checks the FINAL To/CC/BCC
	// set — a reviewer-added recipient can't bypass the accept-time check, and
	// an address suppressed while the draft was held is refused here. Scoped
	// to the message's owning account (agent.UserID == the userID that proved
	// ownership above). On refusal NOTHING below has run: the hold stays
	// pending_review and the 422 is returned strictly before any side effect,
	// so runIdempotent releases (never caches) the caller's Idempotency-Key —
	// same semantics as send's accept-time 422 — and the identical approve
	// succeeds once the suppression is removed. Fails CLOSED on a store error
	// (retryable 500, hold untouched) — see checkSuppressionCore.
	if supErr := a.checkSuppressionStrict(ctx, agent.UserID, agent.ID, mergedReq); supErr != nil {
		return nil, supErr
	}
	if uerr := prepareManagedUnsubscribe(ctx, a.unsubscribeIssuer, a.fromDomain, agent.UserID, agent, &mergedReq, true); uerr != nil {
		return nil, uerr
	}
	// A held draft that carried a future send_at must not silently lose it to a
	// reviewer edit (#815): self-delivery is an immediate in-process loopback with
	// no scheduled arm, so approving a still-scheduled hold whose FINAL (edited)
	// recipients target the agent's own address would deliver now and drop the
	// schedule. Reject before dispatch — the hold stays pending_review and the
	// reviewer re-targets an external recipient (or waits for the schedule to
	// lapse, after which approval delivers immediately). Mirrors the accept-path
	// rejection in DeliverOutbound; a lapsed schedule is already satisfied, so it
	// falls through to the immediate loopback below.
	if preview.ScheduledAt != nil && preview.ScheduledAt.After(time.Now()) && isSelfSend(mergedReq, agent.EmailAddress()) {
		return nil, &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request",
			Msg: fmt.Sprintf("cannot approve: this held message is scheduled to send at %s and the edited recipients target the agent's own address, which is delivered as an immediate loopback — change the recipients to an external address to approve",
				preview.ScheduledAt.UTC().Format(time.RFC3339))}
	}
	// Transition the hold to review_approved + delivery_status='accepted'
	// and enqueue an outbound_send job; the SendWorker performs the SMTP submit +
	// email.sent/failed + metering. The reviewer gets "accepted" back (the send is
	// durably queued). Self-sends fall through to the local loopback path below.
	if a.outboundEnq == nil {
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "outbound delivery queue unavailable"}
	}
	sent, handled, aerr := a.approveOutboundAsyncWithRequest(ctx, agent, messageID, userID, preview, edits, mergedReq, idemCompleteTx)
	if aerr != nil {
		return nil, approveAsyncError(agent.ID, messageID, aerr)
	}
	if handled {
		slug, _, _ := strings.Cut(agent.EmailAddress(), "@")
		log.Printf("[mail:%s] dir=outbound type=%s status=%s from=%s to_count=%d to_domains=%v slug=%s subject_len=%d edited=%v approved=user:%s delivery=async",
			sent.ID, sent.Type, sent.Status, agent.EmailAddress(), len(sent.ToRecipients), logredact.AddressDomains(sent.ToRecipients), slug, utf8.RuneCountInString(sent.Subject), sent.Edited, userID)
		a.publishApproved(ctx, a.buildApprovedEvent(agent, sent, userID), sent)
		// No metering here — the SendWorker meters on MarkSent.
		return sent, nil
	}

	sent, err = a.approveSelfSend(ctx, agent, messageID, userID, edits, idemCompleteTx)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrMessageNotFound):
			return nil, &OutboundError{Status: http.StatusNotFound, Code: "not_found", Msg: "message not found"}
		case errors.Is(err, identity.ErrNotPendingApproval):
			return nil, &OutboundError{Status: http.StatusConflict, Code: "message_not_pending", Msg: "message is not pending approval"}
		default:
			var ve *outbound.ValidationError
			if errors.As(err, &ve) {
				return nil, &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: ve.Error()}
			}
			log.Printf("[api] approve-send failed: agent=%s msg=%s err=%v", agent.ID, messageID, err)
			return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "send failed"}
		}
	}

	a.recordLoopbackUsage(ctx, userID, agent)
	slug, _, _ := strings.Cut(agent.EmailAddress(), "@")
	log.Printf("[mail:%s] dir=outbound type=%s status=%s from=%s to_count=%d to_domains=%v slug=%s subject_len=%d edited=%v approved=user:%s",
		sent.ID, sent.Type, sent.Status, agent.EmailAddress(), len(sent.ToRecipients), logredact.AddressDomains(sent.ToRecipients), slug, utf8.RuneCountInString(sent.Subject), sent.Edited, userID)
	a.publishApproved(ctx, a.buildApprovedEvent(agent, sent, userID), sent)
	return sent, nil
}

// approveOutboundAsyncWithRequest composes the edited draft and, for a non-self-send,
// transitions the hold to status='sent' + delivery_status='accepted' and enqueues an
// outbound_send job in one tx (via store.ApproveAndAccept). Returns (sent, true, nil)
// when queued; (nil, false, nil) when the message is a self-send (the caller uses the
// sync loopback path); (nil, false, err) on failure. Shared by the dashboard-approve
// (ApprovePendingCore) and magic-link (magicApprove) paths. draft is the loaded
// pending_review row; edits is empty for the magic-link path.
//
// The hold status becomes 'sent' — the same terminal the SYNC human approve
// (ApproveAndSend) uses: outbound has no separate "approved" hold status; the human
// resolution is recorded via reviewed_by_user_id + the review_approved event, and
// delivery_status ('accepted' → 'sent'/'failed') tracks the async send. (The TTL
// sweep uses review_expired_approved instead — see hitlworker.autoApproveAsync.)
func (a *API) approveOutboundAsyncWithRequest(ctx context.Context, agent *identity.AgentIdentity, messageID, userID string, draft *identity.Message, edits identity.PendingApprovalEdit, sendReq outbound.SendRequest, idemCompleteTx ApproveIdemCompleter) (*identity.Message, bool, error) {
	editedByReviewer := edits.Apply(draft)
	return a.approveOutboundAsyncComposed(ctx, agent, messageID, userID, draft, editedByReviewer, sendReq, idemCompleteTx)
}

func (a *API) approveOutboundAsyncComposed(ctx context.Context, agent *identity.AgentIdentity, messageID, userID string, draft *identity.Message, editedByReviewer bool, sendReq outbound.SendRequest, idemCompleteTx ApproveIdemCompleter) (*identity.Message, bool, error) {
	var err error
	attachReferencesChain(ctx, a.store, agent.ID, &sendReq)
	// A held platform test (type="test") targets the agent's own address by
	// design, so the self-send predicate below would silently reroute its
	// approval to local loopback — dropping the real SMTP → inbound round-trip
	// the test exists to exercise. Keep it platform-originated instead: same
	// compose the accept path (acceptPlatformSend) uses, same queued delivery.
	isPlatformTest := draft.Type == "test"
	if !isPlatformTest && isSelfSend(sendReq, agent.EmailAddress()) {
		return nil, false, nil // self-send — caller uses the sync loopback path
	}
	var comp *outbound.ComposeResult
	if isPlatformTest {
		comp, err = a.sender.ComposePlatformForAccept(sendReq)
	} else {
		// Held mail composes here, at approval time, so the outbound-footer
		// decision is resolved here too (same freeze-with-the-bytes semantics
		// as DeliverOutbound's accept path). The flag is not persisted on the
		// held row; the owning account's entitlement at approval time decides.
		// Keyed to the agent's owner, not the reviewing userID param, though
		// the ownership check upstream makes them the same account.
		sendReq.AppendOutboundFooter = a.resolveOutboundFooterByUserID(ctx, agent.UserID)
		comp, err = a.sender.ComposeForAccept(agent, sendReq)
	}
	if err != nil {
		return nil, false, err
	}
	acc := identity.AcceptedSend{
		To: comp.To, CC: comp.CC, BCC: comp.BCC, Subject: sendReq.Subject,
		Method: comp.Method, EnvelopeFrom: comp.EnvelopeFrom, SentAs: comp.SentAs, Raw: comp.Raw,
	}
	sent, err := a.store.ApproveAndAccept(ctx, messageID, userID, identity.MessageStatusSent, editedByReviewer, acc, a.outboundEnq.EnqueueSendTx, a.outboundEnq.EnqueueScheduledSendTx, idemCompleteTx)
	if err != nil {
		return nil, false, err
	}
	return sent, true, nil
}

// approveAsyncError maps an approveOutboundAsync failure to an OutboundError,
// matching the sync approve path's status codes.
func approveAsyncError(agentID, messageID string, err error) *OutboundError {
	if sizeErr := composedSizeOutboundError(err); sizeErr != nil {
		return sizeErr
	}
	switch {
	case errors.Is(err, identity.ErrNotPendingApproval):
		return &OutboundError{Status: http.StatusConflict, Code: "message_not_pending", Msg: "message is not pending approval"}
	case errors.Is(err, identity.ErrMessageNotFound):
		return &OutboundError{Status: http.StatusNotFound, Code: "not_found", Msg: "message not found"}
	default:
		var ve *outbound.ValidationError
		if errors.As(err, &ve) {
			return &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: ve.Error()}
		}
		log.Printf("[api] approve-accept failed: agent=%s msg=%s err=%v", agentID, messageID, err)
		return &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "send failed"}
	}
}

func composedSizeOutboundError(err error) *OutboundError {
	var sizeErr *outbound.ComposedSizeError
	if !errors.As(err, &sizeErr) {
		return nil
	}
	return &OutboundError{
		Status: http.StatusRequestEntityTooLarge,
		Code:   "payload_too_large",
		Msg:    sizeErr.Error(),
		Details: map[string]any{
			"scope":        "composed_message",
			"actual_bytes": sizeErr.ActualBytes,
			"max_bytes":    sizeErr.MaxBytes,
		},
	}
}

// attachReferencesChain rebuilds the References chain on a HITL-approved
// SendRequest by looking up the parent inbound's raw message via
// email_message_id. Required because the pending-outbound row only
// persists the parent's Message-ID, not its raw message — without
// re-deriving here, HITL-protected replies fall back to single-id
// References and fork Gmail threads in multi-party conversations.
//
// No-op when ReplyToMessageID is empty (a fresh /send, not a reply) or
// when the parent inbound has expired / been deleted. In the expiry
// case we silently fall back to legacy single-id behavior — better
// than refusing the send. Callers must keep ReplyToMessageID populated
// regardless; only References is filled in here.
func attachReferencesChain(ctx context.Context, store hitlParentLookup, agentID string, req *outbound.SendRequest) {
	if req.ReplyToMessageID == "" {
		return
	}
	// Direction-agnostic: a held reply's parent may be an outbound the agent
	// sent (reply-to-own-message), not only a received inbound.
	parent, err := store.GetMessageByEmailMessageID(ctx, agentID, req.ReplyToMessageID)
	if err != nil || parent == nil {
		return
	}
	req.References = outbound.BuildReferencesChain(parent.RawMessage, req.ReplyToMessageID)
}

// hitlParentLookup is the narrow store contract attachReferencesChain
// needs. Defined as an interface so tests can stub it without spinning
// up the full identity store.
type hitlParentLookup interface {
	GetMessageByEmailMessageID(ctx context.Context, agentID, emailMessageID string) (*identity.Message, error)
}

// buildSendRequestFromMessage reconstructs a SendRequest from a stored
// pending-approval message (with any reviewer edits already applied).
//
// ReplyToMessageID is only copied through for type="reply". Forwards
// also persist email_message_id (so the review panel can render the
// "what's being forwarded" pane via InboundContext), but a forward must
// ship as a new thread — copying email_message_id into ReplyToMessageID
// would emit In-Reply-To/References on the outbound and stitch the
// forward into the original thread.
func buildSendRequestFromMessage(m *identity.Message) (outbound.SendRequest, error) {
	var attachments []outbound.Attachment
	if len(m.AttachmentsJSON) > 0 {
		if err := json.Unmarshal(m.AttachmentsJSON, &attachments); err != nil {
			return outbound.SendRequest{}, err
		}
	}
	replyToMessageID := ""
	if m.Type == "reply" {
		replyToMessageID = m.EmailMessageID
	}
	// A caller-supplied Reply-To override is persisted on the held row's reply_to
	// column (single element) so it survives the recompose at approval time —
	// without this the override would silently vanish on every reviewed send.
	var replyTo string
	if len(m.ReplyTo) > 0 {
		replyTo = m.ReplyTo[0]
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

// RejectPendingCore is the HTTP-free core of HITL reject: optional
// expected-agent-email match (mirrors the legacy URL guard), then
// RejectPending + publish. Shared by the legacy handler and the v1 layer.
func (a *API) RejectPendingCore(ctx context.Context, userID, messageID, expectedAgentEmail, reason string) (*identity.Message, *OutboundError) {
	if expectedAgentEmail != "" {
		preview, err := a.store.GetOutboundMessageForUser(ctx, messageID, userID)
		if err != nil {
			return nil, &OutboundError{Status: http.StatusNotFound, Code: "not_found", Msg: "message not found"}
		}
		agent, err := a.store.GetAgentByID(ctx, preview.AgentID)
		if err != nil || agent.Email != expectedAgentEmail {
			return nil, &OutboundError{Status: http.StatusNotFound, Code: "not_found", Msg: "message not found"}
		}
	}
	rejected, err := a.store.RejectPending(ctx, messageID, userID, reason)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrMessageNotFound):
			return nil, &OutboundError{Status: http.StatusNotFound, Code: "not_found", Msg: "message not found"}
		case errors.Is(err, identity.ErrNotPendingApproval):
			return nil, &OutboundError{Status: http.StatusConflict, Code: "message_not_pending", Msg: "message is not pending approval"}
		default:
			log.Printf("[api] reject: %v", err)
			return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "failed to reject message"}
		}
	}
	log.Printf("[mail:%s] dir=outbound type=%s status=%s agent=%s rejected_by=user:%s reason_len=%d",
		rejected.ID, rejected.Type, rejected.Status, rejected.AgentID, userID, utf8.RuneCountInString(reason))
	a.publishRejected(ctx, a.buildRejectedEvent(userID, rejected, reason), rejected.ID)
	return rejected, nil
}

// ApproveInboundReviewCore releases a held INBOUND message to its agent's inbox
// (status pending_review → review_approved, now readable) and fires
// email.review_approved — the inbound analogue of ApprovePendingCore. There is no
// SES send and no draft edit: an inbound hold is a screening decision, not a draft.
//
// msg is the dispatch view the account-scoped handler already resolved via
// GetReviewMessage (ownership + tenant isolation proven there); userID is the
// reviewing owner. The store transition is a compare-and-set on
// status='pending_review' AND agent_id, so a concurrent reviewer or the TTL sweep
// racing this call results in ErrNotPendingReview (409), never a double release.
func (a *API) ApproveInboundReviewCore(ctx context.Context, userID string, msg *identity.ReviewMessageMeta) *OutboundError {
	transition, err := a.store.ApproveInboundReviewWithTransition(ctx, msg.ID, msg.AgentID, userID)
	if err != nil {
		if errors.Is(err, identity.ErrNotPendingReview) {
			return &OutboundError{Status: http.StatusConflict, Code: "message_not_pending", Msg: "message is not pending review"}
		}
		log.Printf("[api] approve inbound review %s: %v", msg.ID, err)
		return &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "failed to approve message"}
	}
	log.Printf("[mail:%s] dir=inbound type=%s status=%s agent=%s approved_by=user:%s",
		msg.ID, msg.Type, identity.MessageStatusReviewApproved, msg.AgentID, userID)
	// Post-side-effect publish (the release row is already committed): reuse the
	// approved-event plumbing (deterministic id off the message id → MTA/retry
	// idempotent). A minimal *identity.Message carries the id publishApproved needs.
	a.publishApproved(ctx, a.buildInboundReleasedEvent(msg, a.reviewOwnerID(ctx, msg.AgentID, userID), userID, transition), &identity.Message{ID: msg.ID, AgentID: msg.AgentID})
	return nil
}

// reviewOwnerID returns the agent's owner user id — the webhook routing key for
// an inbound review event. It equals the reviewer today (the endpoint is
// account-scoped + ownership-checked), so on any lookup failure we fall back to
// the reviewer id rather than fail the already-committed release.
func (a *API) reviewOwnerID(ctx context.Context, agentID, fallbackUserID string) string {
	ag, err := a.store.GetAgentByID(ctx, agentID)
	if err != nil || ag == nil {
		log.Printf("[api] review owner lookup for agent %s: %v (routing on reviewer)", agentID, err)
		return fallbackUserID
	}
	return ag.UserID
}

// RejectInboundReviewCore drops a held INBOUND message (status pending_review →
// review_rejected; it stays hidden from the agent, raw payload retained for
// forensics) and fires email.review_rejected. Compare-and-set semantics as in
// ApproveInboundReviewCore.
func (a *API) RejectInboundReviewCore(ctx context.Context, userID, reason string, msg *identity.ReviewMessageMeta) *OutboundError {
	transition, err := a.store.RejectInboundReviewWithTransition(ctx, msg.ID, msg.AgentID, userID, reason)
	if err != nil {
		if errors.Is(err, identity.ErrNotPendingReview) {
			return &OutboundError{Status: http.StatusConflict, Code: "message_not_pending", Msg: "message is not pending review"}
		}
		log.Printf("[api] reject inbound review %s: %v", msg.ID, err)
		return &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "failed to reject message"}
	}
	log.Printf("[mail:%s] dir=inbound type=%s status=%s agent=%s rejected_by=user:%s reason_len=%d",
		msg.ID, msg.Type, identity.MessageStatusReviewRejected, msg.AgentID, userID, utf8.RuneCountInString(reason))
	a.publishRejected(ctx, a.buildInboundRejectedEvent(msg, a.reviewOwnerID(ctx, msg.AgentID, userID), userID, reason, transition), msg.ID)
	return nil
}
