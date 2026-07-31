package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

// LocalDeliveryTxHook runs after both sides of a providerless local delivery
// are visible in tx and before commit. Callers use it for durable outcome
// events and idempotency completion so none can diverge from the Sent/Inbox
// pair.
type LocalDeliveryTxHook func(ctx context.Context, tx pgx.Tx, outbound, inbound *Message, result SendResult, outboundTransitions, inboundTransitions []messagelifecycle.MessageLifecycleTransition) error

// LocalInboundScreen carries the caller-evaluated inbound-protection verdict
// for the recipient-side row of a providerless local delivery. The evaluation
// itself lives in internal/inboundscreen (which this package cannot import —
// it imports identity), so the compose callback returns the verdict alongside
// the composed message and finalizeLocalDeliveryTx persists it: the inbound
// row is created with the screening denorm (a review/block status hides it
// from the inbox exactly like a relay-held message) and the gate flag
// annotation. The zero value means "screened clean" — delivered normally.
type LocalInboundScreen struct {
	// MessageID pre-allocates the inbound row id so the caller's audit rows
	// (protection_events) and deterministic event ids anchor to the row that is
	// actually created. Empty → the store allocates one.
	MessageID  string
	Flagged    bool
	FlagReason string
	Screening  InboundScreening
}

// GetEventEnvelope returns the exact durable event envelope for a message.
// WebSocket reconnect drain uses this instead of rebuilding an event whose
// timestamp or attachment metadata could differ under the same event id.
func (s *Store) GetEventEnvelope(ctx context.Context, messageID, eventType string) ([]byte, error) {
	var envelope []byte
	err := s.pool.QueryRow(ctx,
		`SELECT envelope FROM webhook_events WHERE message_id=$1 AND type=$2`,
		messageID, eventType,
	).Scan(&envelope)
	return envelope, err
}

// ApproveAndDeliverLocal atomically resolves a pending outbound review hold
// whose only recipient is a mailbox owned by this service. Unlike
// ApproveAndSend, compose must be a local, side-effect-free operation: the
// outbound update, recipient-side insert, events, and idempotency completion
// all commit or roll back together, so the SES-oriented send_attempts journal
// is neither needed nor appropriate. compose also returns the inbound-leg
// screening verdict (evaluated over the composed MIME) so the recipient-side
// row carries the agent's inbound protection outcome instead of a zero-value
// screening.
//
// compose runs BEFORE the transaction, on a lock-free snapshot of the pending
// row: the inbound screening it performs may include a piguard detector with a
// network call (Gemini, 10s detector timeout), which must not sit inside an
// open transaction holding the row lock. The FOR-UPDATE reload + pending_review
// CAS inside the transaction still protects against the row being resolved
// between screen and commit, and held rows are mutation-guarded (content
// cannot drift while pending_review), so screening the snapshot is sound —
// the same TOCTOU stance as performSelfSend and the relay, which both screen
// before their persist transactions.
func (s *Store) ApproveAndDeliverLocal(
	ctx context.Context,
	messageID, userID string,
	edits PendingApprovalEdit,
	compose func(msg *Message) (SendResult, LocalInboundScreen, error),
	beforeCommit LocalDeliveryTxHook,
) (*Message, error) {
	// Phase 1 (no tx, no lock): snapshot the pending row, apply the reviewer's
	// edits, compose + screen.
	snapshot, ownerUserID, err := loadPendingOutboundForLocalDeliverySnapshot(ctx, s.pool, messageID)
	if err != nil {
		return nil, err
	}
	if ownerUserID != userID {
		return nil, ErrMessageNotFound
	}
	edits.Apply(snapshot)
	result, screen, err := compose(snapshot)
	if err != nil {
		return nil, err
	}
	if result.Method != "loopback" || len(result.To) != 1 || len(result.Raw) == 0 || result.ProviderMessageID == "" || result.Sender == "" {
		return nil, errors.New("identity: invalid local delivery result")
	}

	// Phase 2: lock + CAS (still pending_review, still owned) and finalize.
	txCtx, cancel := context.WithTimeout(ctx, approvalTxTimeout)
	defer cancel()

	tx, err := s.pool.Begin(txCtx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(txCtx)
		}
	}()

	m, ownerUserID, err := loadPendingOutboundForLocalDelivery(txCtx, tx, messageID)
	if err != nil {
		return nil, err
	}
	if ownerUserID != userID {
		return nil, ErrMessageNotFound
	}
	editedByReviewer := edits.Apply(m)

	reviewerID := userID
	inbound, err := s.finalizeLocalDeliveryTx(txCtx, tx, m, result, MessageStatusSent, editedByReviewer, &reviewerID, screen)
	if err != nil {
		return nil, err
	}

	outboundTransitions, inboundTransitions, err := appendLocalDeliveryLifecycle(txCtx, tx, m, inbound, result, messagelifecycle.ReasonReviewApproved, MessageStatusReviewApproved)
	if err != nil {
		return nil, err
	}
	if beforeCommit != nil {
		if err := beforeCommit(ctx, tx, m, inbound, result, outboundTransitions, inboundTransitions); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(txCtx); err != nil {
		return nil, err
	}
	committed = true
	return m, nil
}

// ExpireAndDeliverLocal is the TTL-worker counterpart of
// ApproveAndDeliverLocal. It requires an expired pending row, records the
// review_expired_approved lifecycle, and atomically creates the Inbox copy and
// outcome events without using the external-provider send journal.
//
// Like ApproveAndDeliverLocal, compose (incl. the possibly-networked inbound
// screening) runs BEFORE the transaction on a lock-free snapshot; the locked
// reload's pending_review + expiry CAS inside the transaction protects
// against the row being resolved in between, and held-row content is
// mutation-guarded — so a single-threaded TTL sweep never holds the row lock
// through a detector's network timeout.
func (s *Store) ExpireAndDeliverLocal(
	ctx context.Context,
	messageID string,
	compose func(msg *Message) (SendResult, LocalInboundScreen, error),
	beforeCommit LocalDeliveryTxHook,
) (*Message, error) {
	// Phase 1 (no tx, no lock): snapshot + compose + screen.
	//
	// Under multiple sweep replicas, two workers can both pass this snapshot
	// for the same candidate; the loser of the phase-2 SKIP LOCKED race
	// discards its compose + screening work — a duplicate detector call
	// (potentially a Gemini HTTP request) whose result is thrown away.
	// Acceptable while the TTL sweep is single-threaded (one River
	// maintenance periodic); revisit if the sweep is ever parallelized.
	snapshot, _, err := loadExpiredPendingOutboundForLocalDeliverySnapshot(ctx, s.pool, messageID)
	if err != nil {
		return nil, err
	}
	result, screen, err := compose(snapshot)
	if err != nil {
		return nil, err
	}
	if result.Method != "loopback" || len(result.To) != 1 || len(result.Raw) == 0 || result.ProviderMessageID == "" || result.Sender == "" {
		return nil, errors.New("identity: invalid local delivery result")
	}

	// Phase 2: lock (SKIP LOCKED) + CAS and finalize.
	txCtx, cancel := context.WithTimeout(ctx, approvalTxTimeout)
	defer cancel()

	tx, err := s.pool.Begin(txCtx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(txCtx)
		}
	}()

	m, _, err := loadExpiredPendingOutboundForLocalDelivery(txCtx, tx, messageID)
	if err != nil {
		return nil, err
	}

	inbound, err := s.finalizeLocalDeliveryTx(txCtx, tx, m, result, MessageStatusReviewExpiredApproved, false, nil, screen)
	if err != nil {
		return nil, err
	}
	outboundTransitions, inboundTransitions, err := appendLocalDeliveryLifecycle(txCtx, tx, m, inbound, result, messagelifecycle.ReasonReviewExpiredApproved, MessageStatusReviewExpiredApproved)
	if err != nil {
		return nil, err
	}
	if beforeCommit != nil {
		if err := beforeCommit(ctx, tx, m, inbound, result, outboundTransitions, inboundTransitions); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(txCtx); err != nil {
		return nil, err
	}
	committed = true
	return m, nil
}

func appendLocalDeliveryLifecycle(ctx context.Context, tx pgx.Tx, outbound, inbound *Message, result SendResult, reviewReason messagelifecycle.ReasonCode, resolution string) ([]messagelifecycle.MessageLifecycleTransition, []messagelifecycle.MessageLifecycleTransition, error) {
	review, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
		MessageID: outbound.ID, DedupeKey: "review:resolution", Direction: "outbound",
		ReasonCode: reviewReason, Evidence: map[string]any{"review_resolution": resolution}, OccurredAt: time.Now(),
	})
	if err != nil {
		return nil, nil, err
	}
	outbound.LifecycleTransitions = []messagelifecycle.MessageLifecycleTransition{review}
	submitted, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
		MessageID: outbound.ID, DedupeKey: "submission:local-loopback", Direction: "outbound",
		ReasonCode:     messagelifecycle.ReasonSubmissionLocalLoopbackAccepted,
		CorrelationIDs: messagelifecycle.SafeCorrelationIDs(map[string]string{"email_message_id": result.ProviderMessageID}), OccurredAt: time.Now(),
	})
	if err != nil {
		return nil, nil, err
	}
	accepted, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
		MessageID: inbound.ID, DedupeKey: "acceptance", Direction: "inbound",
		ReasonCode:     messagelifecycle.ReasonAcceptanceLocalLoopback,
		CorrelationIDs: messagelifecycle.SafeCorrelationIDs(map[string]string{"email_message_id": result.ProviderMessageID}), OccurredAt: inbound.CreatedAt,
	})
	if err != nil {
		return nil, nil, err
	}
	return []messagelifecycle.MessageLifecycleTransition{submitted}, []messagelifecycle.MessageLifecycleTransition{accepted}, nil
}

func (s *Store) finalizeLocalDeliveryTx(
	ctx context.Context,
	tx pgx.Tx,
	m *Message,
	result SendResult,
	targetStatus string,
	editedByReviewer bool,
	reviewedByUserID *string,
	screen LocalInboundScreen,
) (*Message, error) {
	sourceThreadID, err := s.EnsureThreadTx(ctx, tx, m.AgentID, m.ID)
	if err != nil {
		return nil, err
	}
	m.ThreadID = sourceThreadID

	_, err = tx.Exec(ctx,
		`UPDATE messages
		    SET status                = $2,
		        delivery_status       = 'sent',
		        provider_message_id   = $3,
		        method                = $4,
		        to_recipients         = $5,
		        cc                    = $6,
		        bcc                   = $7,
		        recipient             = $8,
		        subject               = $9,
		        edited                = $10,
		        reviewed_at           = now(),
		        reviewed_by_user_id   = $11,
		        raw_message           = $12::bytea,
		        rfc_message_id_key    = CASE
		          WHEN rfc_message_id_key IS NULL AND $13 <> '' THEN $13
		          ELSE rfc_message_id_key
		        END,
		        sent_as               = 'own_address'
		  WHERE id = $1`,
		m.ID,
		targetStatus,
		result.ProviderMessageID,
		result.Method,
		result.To,
		result.CC,
		result.BCC,
		firstOr(result.To, ""),
		m.Subject,
		editedByReviewer || m.Edited,
		reviewedByUserID,
		result.Raw,
		canonicalRFCMessageIDKey(result.ProviderMessageID),
	)
	if err != nil {
		return nil, err
	}

	// The inbound row carries the caller-evaluated screening verdict: a
	// review/block status makes it a hidden hold (pending_review /
	// review_rejected) exactly like a relay-held inbound message.
	// header_from is the agent's actual EMAIL ADDRESS (the validated single
	// loopback recipient), matching performSelfSend — held rows surface
	// header_from to reviewers via the review queue, which expects an address,
	// not an agent id.
	inbound, err := createInboundMessage(
		ctx, tx, messageThreadAssignment{
			threadID:         sourceThreadID,
			rfcMessageIDKey:  freshInboundMessageThread(result.ProviderMessageID).rfcMessageIDKey,
			resolutionSource: "self_twin",
		}, screen.MessageID, m.AgentID, result.Sender, m.AgentID,
		result.ProviderMessageID, m.Subject, m.ConversationID, "unread",
		result.Raw, nil, nil, screen.Flagged, screen.FlagReason, result.To, result.CC, m.ReplyTo,
		screen.Screening, &InboundAuth{HeaderFrom: firstOr(result.To, m.AgentID)},
	)
	if err != nil {
		return nil, fmt.Errorf("local delivery inbound row: %w", err)
	}
	s.recordThreadResolution("self_twin", 1)
	if _, err := tx.Exec(ctx, `UPDATE messages SET method='loopback' WHERE id=$1`, inbound.ID); err != nil {
		return nil, fmt.Errorf("local delivery inbound method: %w", err)
	}
	inbound.Method = "loopback"

	m.Status = targetStatus
	m.DeliveryStatus = "sent"
	m.ProviderMessageID = result.ProviderMessageID
	m.Method = result.Method
	m.ToRecipients = result.To
	m.CC = result.CC
	m.BCC = result.BCC
	m.Recipient = firstOr(result.To, "")
	m.Edited = editedByReviewer || m.Edited
	m.RawMessage = result.Raw
	m.SentAs = "own_address"
	m.BodyText = ""
	m.BodyHTML = ""
	m.AttachmentsJSON = nil
	now := time.Now()
	m.ReviewedAt = &now
	m.ReviewedByUserID = reviewedByUserID
	return inbound, nil
}

func loadExpiredPendingOutboundForLocalDelivery(ctx context.Context, tx pgx.Tx, messageID string) (*Message, string, error) {
	return scanPendingOutboundForLocalDelivery(tx.QueryRow(ctx, localDeliverySelect+
		` AND m.approval_expires_at < now()
		  FOR NO KEY UPDATE OF m SKIP LOCKED`, messageID), ErrNotPendingApproval)
}

func loadPendingOutboundForLocalDelivery(ctx context.Context, tx pgx.Tx, messageID string) (*Message, string, error) {
	return scanPendingOutboundForLocalDelivery(tx.QueryRow(ctx, localDeliverySelect+
		` FOR NO KEY UPDATE OF m`, messageID), ErrMessageNotFound)
}

// localDeliveryQuerier is the QueryRow slice of *pgxpool.Pool / pgx.Tx the
// lock-free snapshot loaders need.
type localDeliveryQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// loadPendingOutboundForLocalDeliverySnapshot is the lock-free (pre-transaction)
// twin of loadPendingOutboundForLocalDelivery: same select and pending_review
// check, no row lock — used to compose + screen before the delivery
// transaction opens.
func loadPendingOutboundForLocalDeliverySnapshot(ctx context.Context, q localDeliveryQuerier, messageID string) (*Message, string, error) {
	return scanPendingOutboundForLocalDelivery(q.QueryRow(ctx, localDeliverySelect, messageID), ErrMessageNotFound)
}

// loadExpiredPendingOutboundForLocalDeliverySnapshot is the lock-free twin of
// loadExpiredPendingOutboundForLocalDelivery (no FOR UPDATE / SKIP LOCKED).
func loadExpiredPendingOutboundForLocalDeliverySnapshot(ctx context.Context, q localDeliveryQuerier, messageID string) (*Message, string, error) {
	return scanPendingOutboundForLocalDelivery(q.QueryRow(ctx, localDeliverySelect+
		` AND m.approval_expires_at < now()`, messageID), ErrNotPendingApproval)
}

const localDeliverySelect = `SELECT m.id, m.agent_id, m.direction, m.sender, m.recipient, m.subject,
		m.email_message_id, m.method, m.message_type,
		m.conversation_id, m.created_at, m.expires_at,
		m.to_recipients, m.cc, m.bcc, m.reply_to,
		m.status, m.approval_expires_at, m.edited,
		m.body_text, m.body_html, m.attachments_json,
		a.user_id
	 FROM messages m
	 JOIN agent_identities a ON a.id = m.agent_id
	WHERE m.id = $1 AND m.direction = 'outbound'
	  AND a.deleted_at IS NULL`

func scanPendingOutboundForLocalDelivery(row pgx.Row, noRowError error) (*Message, string, error) {
	var (
		m                  Message
		ownerUserID        string
		bodyText, bodyHTML *string
		attachments        []byte
		method, msgType    *string
		approvalExpires    *time.Time
	)
	err := row.Scan(
		&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.Recipient, &m.Subject,
		&m.EmailMessageID, &method, &msgType,
		&m.ConversationID, &m.CreatedAt, &m.ExpiresAt,
		&m.ToRecipients, &m.CC, &m.BCC, &m.ReplyTo,
		&m.Status, &approvalExpires, &m.Edited,
		&bodyText, &bodyHTML, &attachments,
		&ownerUserID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", noRowError
		}
		return nil, "", err
	}
	if m.Status != MessageStatusPendingReview {
		return nil, "", ErrNotPendingApproval
	}
	if method != nil {
		m.Method = *method
	}
	if msgType != nil {
		m.Type = *msgType
	}
	if approvalExpires != nil {
		m.ApprovalExpiresAt = approvalExpires
	}
	if bodyText != nil {
		m.BodyText = *bodyText
	}
	if bodyHTML != nil {
		m.BodyHTML = *bodyHTML
	}
	if len(attachments) > 0 {
		m.AttachmentsJSON = json.RawMessage(attachments)
	}
	return &m, ownerUserID, nil
}
