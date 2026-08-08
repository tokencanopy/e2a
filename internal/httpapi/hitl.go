package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
)

// This file holds the shared approve/reject dispatch behind the account-scoped
// review queue (/v1/reviews/{id}/approve|reject, registered in reviews.go). The
// deprecated agent-path endpoints (/v1/agents/{email}/messages/{id}/approve|reject)
// were removed in the pre-GA vocabulary freeze — /v1/reviews/{id} is the only
// approve/reject path.

// approveOutput returns the unified SendResultView (MSG-9) — approve is a send,
// so it shares send/reply/forward's result shape (with edited set).
type approveOutput struct {
	Status int
	Body   SendResultView
}

// RejectResultView is the reject response. Reject is not a send, so it keeps
// its own shape (status + rejection_reason).
type RejectResultView struct {
	Status          string `json:"status"`
	MessageID       string `json:"message_id"`
	RejectionReason string `json:"rejection_reason,omitempty"`
}

// maxRejectReasonLen bounds the reviewer-supplied rejection reason. Enforced
// declaratively via the maxLength struct tag (Huma validates in Unicode code
// points); TestGABoundTagsMatchConsts guards tag/const drift. Aliased to the
// canonical identity.MaxRejectReasonLen so this and the magic-link reject
// form (internal/agent, which clamps) share one source of truth.
const maxRejectReasonLen = identity.MaxRejectReasonLen

// RejectRequest is the reject body (MSG-10, was the inline RejectInputBody).
type RejectRequest struct {
	Reason string `json:"reason,omitempty" maxLength:"2000" doc:"Optional reviewer note explaining the rejection — echoed back as the held message's rejection_reason. At most 2000 characters (Unicode code points)."`
}

type rejectOutput struct {
	Body RejectResultView
}

// resolveHeldDirection looks up a held message scoped to the resolved owned agent
// and reports whether it is inbound. It returns (meta, true, nil) for an inbound
// hold (route to the release path), (nil, false, nil) for an outbound hold (fall
// through to the send-approval path), or a 404 envelope when the message does not
// exist for this agent. When the GetReviewMessage dep is not wired the endpoints
// stay outbound-only (pre-slice-3 behavior).
func (s *Server) resolveHeldDirection(ctx context.Context, messageID, agentID string) (*identity.ReviewMessageMeta, bool, error) {
	if s.deps.GetReviewMessage == nil {
		return nil, false, nil
	}
	meta, err := s.deps.GetReviewMessage(ctx, messageID, agentID)
	if err != nil || meta == nil {
		return nil, false, NewError(http.StatusNotFound, "not_found", "message not found")
	}
	return meta, meta.Direction == "inbound", nil
}

func envelopeFromOutboundError(derr *agent.OutboundError) *ErrorEnvelope {
	env := NewError(derr.Status, derr.Code, derr.Msg)
	if derr.Details != nil {
		env.WithDetails(derr.Details)
	}
	return env
}

// approveHeld is the shared approve dispatch behind /reviews/{id}/approve. The
// caller MUST have proven account scope + ownership of agentEmail. Branches on
// direction: inbound holds release to the inbox; outbound holds send via SES
// (with send-limit + idempotency).
func (s *Server) approveHeld(ctx context.Context, userID, msgID, agentEmail string, body agent.ApproveOverrides, idemKey string, rawBody []byte) (int, SendResultView, error) {
	meta, inbound, err := s.resolveHeldDirection(ctx, msgID, agentEmail)
	if err != nil {
		return 0, SendResultView{}, err
	}
	if inbound {
		if s.deps.ApproveInboundReview == nil {
			return 0, SendResultView{}, NewError(http.StatusInternalServerError, "internal_error", "approve unavailable")
		}
		if derr := s.deps.ApproveInboundReview(ctx, userID, meta); derr != nil {
			return 0, SendResultView{}, envelopeFromOutboundError(derr)
		}
		return http.StatusOK, SendResultView{Status: identity.MessageStatusReviewApproved, MessageID: meta.ID}, nil
	}
	// An approve that edits the held draft can replace its attachments; enforce the
	// same per-attachment / count / total limits here so this outbound path can't
	// bypass what send/reply/forward enforce at compose time.
	if body.Attachments != nil {
		if env := validateAttachments(*body.Attachments); env != nil {
			return 0, SendResultView{}, env
		}
	}
	if s.deps.ApprovePending == nil {
		return 0, SendResultView{}, NewError(http.StatusInternalServerError, "internal_error", "approve unavailable")
	}
	// Async approval resolves the hold and enqueues delivery in one transaction.
	// Complete a keyed request in that SAME transaction so a crash after commit
	// still replays the exact accepted response instead of re-running approval.
	var idemCompleteTx agent.ApproveIdemCompleter
	if idemKey != "" && s.deps.Idempotency != nil {
		nsKey := idemUserNS + idemKey
		uid := userID
		idemCompleteTx = func(ctx context.Context, tx pgx.Tx, sent *identity.Message) error {
			status, view := approveResult(sent)
			raw, marshalErr := json.Marshal(view)
			if marshalErr != nil {
				raw = []byte("{}")
			}
			return s.deps.Idempotency.CompleteTx(ctx, tx, uid, nsKey, idempotency.CachedResponse{
				StatusCode: status, ContentType: "application/json", Body: raw,
			})
		}
	}
	status, view, err := runIdempotent(s, ctx, userID, idemKey, "/v1/approve/"+msgID, rawBody, func() (int, SendResultView, error) {
		// Mutable rate-limit state is evaluated only after the idempotency claim,
		// so a completed keyed retry replays its cached response without consuming
		// another token or being replaced by a later 429.
		if env := s.checkSendLimit(agentEmail); env != nil {
			return 0, SendResultView{}, env
		}
		sent, derr := s.deps.ApprovePending(ctx, userID, msgID, agentEmail, body, idemCompleteTx)
		if derr != nil {
			return 0, SendResultView{}, envelopeFromOutboundError(derr)
		}
		status, view := approveResult(sent)
		return status, view, nil
	})
	if err != nil {
		return 0, SendResultView{}, err
	}
	return status, view, nil
}

// approveResult is the single source of the approval response's status + body,
// shared by the live response and the in-transaction idempotency cache.
func approveResult(sent *identity.Message) (int, SendResultView) {
	statusCode := http.StatusOK
	status := "sent"
	var scheduledAt *time.Time
	if sent.DeliveryStatus == "accepted" {
		statusCode = http.StatusAccepted
		status = "accepted"
		// A held draft that carried a still-future send_at is re-armed on approval
		// (#815): report status=scheduled + scheduled_at, matching the direct
		// scheduled-send response. A schedule that has already passed submits
		// immediately, so it stays "accepted" with no scheduled_at echoed.
		if sent.ScheduledAt != nil && sent.ScheduledAt.After(time.Now()) {
			status = "scheduled"
			scheduledAt = utcPtr(sent.ScheduledAt)
		}
	}
	providerMessageID := sent.ProviderMessageID
	if sent.Method == "loopback" {
		providerMessageID = ""
	}
	edited := sent.Edited
	return statusCode, SendResultView{
		Status: status, MessageID: sent.ID, ProviderMessageID: providerMessageID,
		SentAs: sent.SentAs, Method: sent.Method, Edited: &edited,
		ScheduledAt: scheduledAt,
	}
}

// rejectHeld is the shared reject dispatch behind /reviews/{id}/reject. Caller
// MUST have proven account scope + ownership of agentEmail. Inbound holds are
// dropped (hidden); outbound holds are discarded (never sent).
func (s *Server) rejectHeld(ctx context.Context, userID, msgID, agentEmail, reason string) (RejectResultView, error) {
	meta, inbound, err := s.resolveHeldDirection(ctx, msgID, agentEmail)
	if err != nil {
		return RejectResultView{}, err
	}
	if inbound {
		if s.deps.RejectInboundReview == nil {
			return RejectResultView{}, NewError(http.StatusInternalServerError, "internal_error", "reject unavailable")
		}
		if derr := s.deps.RejectInboundReview(ctx, userID, reason, meta); derr != nil {
			return RejectResultView{}, envelopeFromOutboundError(derr)
		}
		return RejectResultView{Status: identity.MessageStatusReviewRejected, MessageID: meta.ID, RejectionReason: reason}, nil
	}
	if s.deps.RejectPending == nil {
		return RejectResultView{}, NewError(http.StatusInternalServerError, "internal_error", "reject unavailable")
	}
	rejected, derr := s.deps.RejectPending(ctx, userID, msgID, agentEmail, reason)
	if derr != nil {
		return RejectResultView{}, envelopeFromOutboundError(derr)
	}
	return RejectResultView{Status: rejected.Status, MessageID: rejected.ID, RejectionReason: rejected.RejectionReason}, nil
}
