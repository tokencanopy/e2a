package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

// ReviewListItem is one row of the human review queue (GET /v1/reviews):
// non-secret summary of a held message of either direction. Bodies are
// fetched per-item via GetReviewWithContent.
type ReviewListItem struct {
	ID             string
	AgentID        string
	Direction      string // inbound | outbound
	Sender         string
	HeaderFrom     string
	EnvelopeFrom   string
	Authentication *emailauth.Authentication
	To             []string
	Subject        string
	ConversationID string
	Status         string // review lifecycle (pending_review)
	CreatedAt      time.Time
	Flagged        bool
	FlagReason     string
	// ReviewReason is the coded screening verdict that held this message
	// (sender_gate|recipient_gate|inbound_scan|outbound_scan|outbound_send).
	// Unlike FlagReason (inbound ingestion-gate only) it is populated for every
	// hold path — both directions, gate and scan — so it is the reason a reviewer
	// should see. See identity.Message.ReviewReason (migration 037/040).
	ReviewReason string
	// ScheduledAt is the future send_at an outbound hold carried (#815), or nil.
	// The schedule survives the hold, so a reviewer can see the message will send
	// at this instant (or immediately if it has passed) once approved.
	ScheduledAt *time.Time
}

// ListReviews returns one page of held (pending_review) messages — BOTH
// directions — across all of userID's agents, newest-first, keyset-paginated on
// (created_at, id). The caller passes limit (fetch limit+1 to detect a further
// page; limit<=0 returns every row unpaginated) and the after-key from the
// previous page's last row (zero afterCreatedAt = first page). The review queue
// grows unbounded with the pending-review backlog, so it needs real pagination
// rather than returning the whole set. This is the operator review queue: it
// intentionally includes held inbound (which every agent-facing read path
// excludes). SECURITY: account-scoped reviewer flow only; the user join is the
// tenant-isolation guard.
func (s *Store) ListReviews(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]ReviewListItem, error) {
	q := `SELECT m.id, m.agent_id, m.direction, COALESCE(m.method, ''), m.sender,
		        COALESCE(m.header_from, ''), COALESCE(m.envelope_from, ''),
		        m.authentication, m.auth_verdict, m.to_recipients,
		        COALESCE(m.subject, ''), COALESCE(m.conversation_id, ''),
	        COALESCE(m.status, ''), m.created_at,
	        COALESCE(m.flagged, false), COALESCE(m.flag_reason, ''),
	        COALESCE(m.review_reason, ''), m.scheduled_at
	   FROM messages m
	   JOIN agent_identities a ON a.id = m.agent_id
	  WHERE a.user_id = $1 AND a.deleted_at IS NULL
	    AND m.status = 'pending_review'`
	args := []interface{}{userID}
	if !afterCreatedAt.IsZero() {
		i := len(args) + 1
		q += fmt.Sprintf(` AND (m.created_at < $%d OR (m.created_at = $%d AND m.id < $%d))`, i, i, i+1)
		args = append(args, afterCreatedAt, afterID)
	}
	q += ` ORDER BY m.created_at DESC, m.id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT $%d`, len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewListItem
	for rows.Next() {
		var it ReviewListItem
		var method string
		var authentication, legacyVerdict []byte
		if err := rows.Scan(&it.ID, &it.AgentID, &it.Direction, &method, &it.Sender,
			&it.HeaderFrom, &it.EnvelopeFrom, &authentication, &legacyVerdict, &it.To,
			&it.Subject, &it.ConversationID, &it.Status, &it.CreatedAt,
			&it.Flagged, &it.FlagReason, &it.ReviewReason, &it.ScheduledAt); err != nil {
			return nil, err
		}
		message := Message{Direction: it.Direction, Method: method}
		if err := unmarshalAuthentication(authentication, legacyVerdict, &message); err != nil {
			return nil, err
		}
		it.Authentication = message.Authentication
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetReviewWithContent loads one held message (either direction) with its body,
// scoped to userID — the detail view behind GET /v1/reviews/{id}. Like
// GetReviewMessage it deliberately bypasses the held-inbound exclusion, so it is
// account-scoped reviewer flow ONLY (the user join is the tenant guard). Unlike
// GetMessageWithContent it does NOT mark the message read (reviewing isn't
// reading) and does NOT exclude held rows. Returns sql.ErrNoRows when no such
// message exists for the user.
func (s *Store) GetReviewWithContent(ctx context.Context, userID, messageID string) (*Message, error) {
	m := &Message{}
	var authHeadersJSON []byte
	var authentication, authVerdict []byte
	var outboundDeliveryStatus string
	err := s.pool.QueryRow(ctx,
		`SELECT m.id, m.agent_id, m.direction, COALESCE(m.method, ''), m.sender, COALESCE(m.header_from, ''), COALESCE(m.envelope_from, ''), m.authentication, m.recipient, m.to_recipients, m.cc, m.reply_to, m.subject, m.email_message_id, m.conversation_id, COALESCE(m.inbox_status, ''), m.raw_message, m.auth_headers, m.auth_verdict, COALESCE(m.flagged, false), COALESCE(m.flag_reason, ''), COALESCE(m.review_reason, ''), m.created_at, m.expires_at, m.labels, COALESCE(m.delivery_status, ''), COALESCE(m.delivery_detail, ''), COALESCE(m.sent_as, ''), COALESCE(m.body_text, ''), COALESCE(m.body_html, ''), COALESCE(m.status, ''), m.scheduled_at, COALESCE(wd.status, ''), COALESCE(wd.last_error, '')
		   FROM messages m
		   JOIN agent_identities a ON a.id = m.agent_id
		   LEFT JOIN webhook_deliveries wd ON wd.message_id = m.id
		  WHERE m.id = $1 AND a.user_id = $2 AND a.deleted_at IS NULL`,
		messageID, userID,
	).Scan(&m.ID, &m.AgentID, &m.Direction, &m.Method, &m.Sender, &m.HeaderFrom, &m.EnvelopeFrom, &authentication, &m.Recipient, &m.ToRecipients, &m.CC, &m.ReplyTo, &m.Subject, &m.EmailMessageID, &m.ConversationID, &m.InboxStatus, &m.RawMessage, &authHeadersJSON, &authVerdict, &m.Flagged, &m.FlagReason, &m.ReviewReason, &m.CreatedAt, &m.ExpiresAt, &m.Labels, &outboundDeliveryStatus, &m.DeliveryDetail, &m.SentAs, &m.BodyText, &m.BodyHTML, &m.Status, &m.ScheduledAt, &m.WebhookStatus, &m.WebhookError)
	if err != nil {
		return nil, err
	}
	m.SizeBytes = len(m.RawMessage)
	if m.Direction == "outbound" {
		m.DeliveryStatus = outboundDeliveryStatus
	} else {
		m.DeliveryStatus = m.InboxStatus
	}
	if authHeadersJSON != nil {
		if err := json.Unmarshal(authHeadersJSON, &m.AuthHeaders); err != nil {
			return nil, fmt.Errorf("unmarshal auth headers: %w", err)
		}
	}
	if err := unmarshalAuthVerdict(authVerdict, m); err != nil {
		return nil, err
	}
	if err := unmarshalAuthentication(authentication, authVerdict, m); err != nil {
		return nil, err
	}
	return m, nil
}

// ErrNotPendingReview is returned when an approve/reject/expire targets an inbound
// message that is not (or is no longer) in pending_review.
var ErrNotPendingReview = fmt.Errorf("message is not pending review")

// heldInboundStatuses is the SQL value-list of inbound review-hold statuses whose
// messages are NOT agent-visible. EVERY agent-facing read path MUST exclude these —
// a held message must be unreachable by the agent (push AND every poll/read path)
// until released. The human review queue (future endpoint) uses dedicated queries
// that intentionally include them. See docs/design/2026-06-20-agent-screening-hitl.md §4.4.
const heldInboundStatuses = `'pending_review', 'review_rejected', 'review_expired_rejected'`

// ReviewMessageMeta is the minimal dispatch view of a held message returned by
// GetReviewMessage: enough to branch the /approve+/reject endpoints on direction
// and to populate the resolution webhook (review_approved / review_rejected).
type ReviewMessageMeta struct {
	ID        string
	AgentID   string
	Direction string // inbound | outbound
	Status    string
	Sender    string
	Recipient string
	Subject   string
	Type      string
}

// GetReviewMessage is the review-queue single-item getter: it loads a message
// scoped to agentID REGARDLESS of held status (unlike every agent-facing read
// path, which excludes heldInboundStatuses). It exists so the account-scoped
// /approve+/reject endpoints can resolve a held message's direction and dispatch
// the correct release path.
//
// SECURITY: this deliberately bypasses the held-inbound read boundary, so it MUST
// only ever be reachable from an account-scoped reviewer flow that has already
// proven ownership of agentID (resolveOwnedAgent). It is NOT an agent read path —
// do not wire it onto any agent-scoped surface. The agentID filter is the
// tenant-isolation guard: a reviewer can only resolve a message belonging to an
// agent they own. Returns sql.ErrNoRows when no such message exists for the agent.
func (s *Store) GetReviewMessage(ctx context.Context, messageID, agentID string) (*ReviewMessageMeta, error) {
	m := &ReviewMessageMeta{}
	var msgType *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, agent_id, direction, status, sender, recipient, subject, message_type
		   FROM messages
		  WHERE id = $1 AND agent_id = $2`,
		messageID, agentID,
	).Scan(&m.ID, &m.AgentID, &m.Direction, &m.Status, &m.Sender, &m.Recipient, &m.Subject, &msgType)
	if err != nil {
		return nil, err
	}
	if msgType != nil {
		m.Type = *msgType
	}
	return m, nil
}

// ListExpiredReviews returns inbound pending_review messages whose
// approval_expires_at has passed, joined with their agent's hitl_expiration_action
// — the inbound analogue of ListExpiredPending. The expiry worker uses these to
// auto-resolve held messages per the agent's policy.
func (s *Store) ListExpiredReviews(ctx context.Context, limit int) ([]ExpirationCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.agent_id, a.hitl_expiration_action
		 FROM messages m
		 JOIN agent_identities a ON a.id = m.agent_id
		 WHERE m.status = 'pending_review' AND m.direction = 'inbound'
		   AND m.approval_expires_at < now()
		   AND a.deleted_at IS NULL
		 ORDER BY m.approval_expires_at ASC
		 LIMIT $1`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExpirationCandidate
	for rows.Next() {
		var c ExpirationCandidate
		if err := rows.Scan(&c.MessageID, &c.AgentID, &c.ExpirationAction); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// transitionReview flips a pending_review inbound message to a terminal review
// status. The status guard makes it compare-and-set: the first writer wins, a
// concurrent/duplicate transition sees RowsAffected=0 → ErrNotPendingReview.
//
// agentID, when non-empty, scopes the update to that agent — the tenant-isolation
// guard for human-driven transitions (a reviewer may only release a message
// belonging to an agent they own; the handler resolves the owned agent first).
// Worker-driven (TTL) transitions pass "" (system-scoped) and a nil reviewerID.
//
// The agent-not-trashed guard is part of the CAS: a hold whose agent was moved
// to the trash after ListExpiredReviews selected it must not auto-resolve —
// trashed holds stay pending_review with their clock paused until RestoreAgent
// shifts approval_expires_at (or the trash purge drops them). Same TOCTOU
// closure as the outbound arms (ExpireReject / ApproveAndAccept).
func (s *Store) transitionReview(ctx context.Context, messageID, agentID, newStatus string, reviewerID *string, rejectionReason string) (messagelifecycle.MessageLifecycleTransition, error) {
	var reason messagelifecycle.ReasonCode
	switch newStatus {
	case MessageStatusReviewApproved:
		reason = messagelifecycle.ReasonReviewApproved
	case MessageStatusReviewRejected:
		reason = messagelifecycle.ReasonReviewRejected
	case MessageStatusReviewExpiredApproved:
		reason = messagelifecycle.ReasonReviewExpiredApproved
	case MessageStatusReviewExpiredRejected:
		reason = messagelifecycle.ReasonReviewExpiredRejected
	default:
		return messagelifecycle.MessageLifecycleTransition{}, fmt.Errorf("invalid review transition status %q", newStatus)
	}
	args := []any{messageID, newStatus, reviewerID, rejectionReason}
	where := `id = $1 AND direction = 'inbound' AND status = 'pending_review'
	    AND NOT EXISTS (SELECT 1 FROM agent_identities ai
	                     WHERE ai.id = messages.agent_id AND ai.deleted_at IS NOT NULL)`
	if agentID != "" {
		args = append(args, agentID)
		where += ` AND agent_id = $5`
	}
	var transition messagelifecycle.MessageLifecycleTransition
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE messages
		    SET status = $2,
		        reviewed_at = now(),
		        reviewed_by_user_id = $3,
		        rejection_reason = NULLIF($4, '')
		  WHERE `+where,
			args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotPendingReview
		}
		if newStatus == MessageStatusReviewApproved || newStatus == MessageStatusReviewExpiredApproved {
			// Releasing an authenticated hold is the moment it reaches the agent,
			// so advance outreach state in the same transaction as the review
			// transition. Rejections remain invisible and cannot manufacture a
			// reply. The authentication predicate mirrors SMTP intake.
			if _, err = tx.Exec(ctx, `
				UPDATE contact_engagements ce
				   SET last_inbound_at = GREATEST(COALESCE(ce.last_inbound_at, m.created_at), m.created_at),
				       last_conversation_id = COALESCE(NULLIF(m.conversation_id, ''), ce.last_conversation_id),
				       updated_at = `+monotonicUpdatedAt("ce")+`
				  FROM messages m
				  JOIN agent_identities ai ON ai.id = m.agent_id
				 WHERE m.id = $1
				   AND m.authentication #>> '{dmarc,status}' = 'pass'
				   AND ce.user_id = ai.user_id
				   AND ce.agent_id = m.agent_id
				   AND ce.address = lower(COALESCE(m.header_from, m.sender))`,
				messageID); err != nil {
				return err
			}
		}
		transition, err = messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
			MessageID: messageID, DedupeKey: "review:resolution", Direction: "inbound",
			ReasonCode: reason, Evidence: map[string]any{"review_resolution": newStatus}, OccurredAt: time.Now(),
		})
		return err
	})
	return transition, err
}

// ApproveInboundReview releases a held inbound message to the agent (status
// review_approved → visible in the inbox). Scoped to agentID for tenant isolation;
// reviewerID identifies the human. Held content is retained (the message is now
// delivered).
func (s *Store) ApproveInboundReview(ctx context.Context, messageID, agentID, reviewerID string) error {
	_, err := s.ApproveInboundReviewWithTransition(ctx, messageID, agentID, reviewerID)
	return err
}

func (s *Store) ApproveInboundReviewWithTransition(ctx context.Context, messageID, agentID, reviewerID string) (messagelifecycle.MessageLifecycleTransition, error) {
	return s.transitionReview(ctx, messageID, agentID, MessageStatusReviewApproved, &reviewerID, "")
}

// RejectInboundReview drops a held inbound message (status review_rejected → stays
// hidden from the agent). Scoped to agentID for tenant isolation; reviewerID
// identifies the human; reason is operator-facing. The raw payload is retained
// (hidden) indefinitely for security forensics unless the message is deleted.
func (s *Store) RejectInboundReview(ctx context.Context, messageID, agentID, reviewerID, reason string) error {
	_, err := s.RejectInboundReviewWithTransition(ctx, messageID, agentID, reviewerID, reason)
	return err
}

func (s *Store) RejectInboundReviewWithTransition(ctx context.Context, messageID, agentID, reviewerID, rejectionReason string) (messagelifecycle.MessageLifecycleTransition, error) {
	return s.transitionReview(ctx, messageID, agentID, MessageStatusReviewRejected, &reviewerID, rejectionReason)
}

// ExpireApproveReview is the worker-side TTL auto-approve: releases the message
// (status review_expired_approved) with no human reviewer. System-scoped.
func (s *Store) ExpireApproveReview(ctx context.Context, messageID string) error {
	_, err := s.ExpireApproveReviewWithTransition(ctx, messageID)
	return err
}

func (s *Store) ExpireApproveReviewWithTransition(ctx context.Context, messageID string) (messagelifecycle.MessageLifecycleTransition, error) {
	return s.transitionReview(ctx, messageID, "", MessageStatusReviewExpiredApproved, nil, "")
}

// DeferReviewExpiry pushes a pending review's TTL forward without resolving
// it. The expiration sweep orders candidates by approval_expires_at, so a
// hold that cannot resolve yet — its account is paused for sending — would
// otherwise stay the oldest candidate and be re-picked first every cycle,
// starving every other expired review once enough of them accumulate.
// Deferring it yields the slot; when the account resumes, the next sweep
// after the deferred instant resolves it normally.
func (s *Store) DeferReviewExpiry(ctx context.Context, messageID string, until time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE messages SET approval_expires_at = $2 WHERE id = $1 AND status = 'pending_review'`,
		messageID, until.UTC())
	if err != nil {
		return fmt.Errorf("defer review expiry: %w", err)
	}
	return nil
}

// ExpireRejectReview is the worker-side TTL auto-reject: drops the message
// (status review_expired_rejected) with no human reviewer. System-scoped.
func (s *Store) ExpireRejectReview(ctx context.Context, messageID, reason string) error {
	_, err := s.ExpireRejectReviewWithTransition(ctx, messageID, reason)
	return err
}

func (s *Store) ExpireRejectReviewWithTransition(ctx context.Context, messageID, rejectionReason string) (messagelifecycle.MessageLifecycleTransition, error) {
	return s.transitionReview(ctx, messageID, "", MessageStatusReviewExpiredRejected, nil, rejectionReason)
}
