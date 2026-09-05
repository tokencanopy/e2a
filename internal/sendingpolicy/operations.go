package sendingpolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file turns a durable source row into a provider operation: the record
// that says who is about to be charged, on whose authority, and whether the
// traffic borrows platform reputation.
//
// Everything a later decision depends on is derived here, under a lock on the
// source, and then persisted as immutable. That is deliberate. Purpose,
// attribution, and the shared-reputation class are exactly the fields an
// attacker would want to change between acceptance and submission — relabel
// customer mail as operational, point attribution at another account, or claim
// a dedicated domain to escape the 50/day shared cap. Deriving them once from a
// locked row, and never reading them from a caller argument, River payload, or
// MIME header again, is what makes those attacks structurally unavailable
// rather than merely unimplemented.

// operationTTL is how long a provider operation and its attempts stay
// referencable. It matches the 30-day window migration 113 used when it
// adopted legacy jobs; the janitor in Task 12 reaps past it.
const operationTTL = 30 * 24 * time.Hour

// Sentinel errors for the preparation surface.
var (
	// ErrSourceUnavailable means the referenced durable source row is absent,
	// deleted, or not of the shape its constructor promised. It is always
	// fail-closed: no operation is created, so no provider call can follow.
	ErrSourceUnavailable = errors.New("sendingpolicy: notification source is unavailable")
	// ErrAudienceNotAllowed means a notice event was asked for an audience its
	// kind forbids — the global guardrail has no owner to blame.
	ErrAudienceNotAllowed = errors.New("sendingpolicy: audience is not allowed for this notice")
	// ErrNoticeSettled means the notice delivery already reached a terminal
	// state, so there is nothing left to send.
	ErrNoticeSettled = errors.New("sendingpolicy: notice delivery is already settled")
)

// operationRow is one row of sending_provider_operations.
type operationRow struct {
	OperationID      string
	SourceAccountRef *string
	PolicySubjectRef string
	Purpose          Purpose
	Shared           bool
	CurrentAttempt   int
}

// ref rebuilds the caller-facing reference from stored, authoritative values.
func (o operationRow) ref() OperationRef {
	ref := OperationRef{
		id:            o.OperationID,
		purpose:       o.Purpose,
		policySubject: o.PolicySubjectRef,
		shared:        o.Shared,
	}
	if o.SourceAccountRef != nil {
		ref.sourceAccount = *o.SourceAccountRef
	}
	return ref
}

// accountRef returns the attributed account, or "" when there is none.
func (o operationRow) accountRef() string {
	if o.SourceAccountRef == nil {
		return ""
	}
	return *o.SourceAccountRef
}

// lockOperation reads and locks one provider operation.
func lockOperation(ctx context.Context, tx pgx.Tx, id string) (operationRow, error) {
	var row operationRow
	err := tx.QueryRow(ctx, `
		SELECT operation_id, source_account_ref, policy_subject_ref, purpose,
		       shared_reputation, current_attempt
		  FROM sending_provider_operations
		 WHERE operation_id = $1
		   FOR UPDATE`, id,
	).Scan(&row.OperationID, &row.SourceAccountRef, &row.PolicySubjectRef,
		&row.Purpose, &row.Shared, &row.CurrentAttempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return operationRow{}, ErrSourceUnavailable
	}
	if err != nil {
		return operationRow{}, fmt.Errorf("sendingpolicy: lock operation: %w", err)
	}
	// A purpose this binary does not implement can only have been written by a
	// newer one. Guessing which pools it should charge is the one mistake that
	// silently un-budgets traffic during a mixed-version rollout, so an
	// unknown purpose fails closed instead.
	if !row.Purpose.valid() {
		return operationRow{}, fmt.Errorf("sendingpolicy: operation %s has unsupported purpose %q",
			row.OperationID, row.Purpose)
	}
	return row, nil
}

// insertOperation creates one operation, or returns the existing row when the
// caller's ID is already taken. Idempotency is by operation ID, which each
// constructor derives from something stable about its source.
func insertOperation(ctx context.Context, tx pgx.Tx, row operationRow, notBefore time.Time) (operationRow, error) {
	expires := time.Now().UTC()
	if notBefore.After(expires) {
		expires = notBefore.UTC()
	}
	expires = expires.Add(operationTTL)

	tag, err := tx.Exec(ctx, `
		INSERT INTO sending_provider_operations
		    (operation_id, source_account_ref, policy_subject_ref, purpose,
		     shared_reputation, current_attempt, expires_at)
		VALUES ($1, $2, $3, $4, $5, 1, $6)
		ON CONFLICT (operation_id) DO NOTHING`,
		row.OperationID, row.SourceAccountRef, row.PolicySubjectRef,
		row.Purpose, row.Shared, expires,
	)
	if err != nil {
		return operationRow{}, fmt.Errorf("sendingpolicy: create operation: %w", err)
	}
	if tag.RowsAffected() == 1 {
		row.CurrentAttempt = 1
		return row, nil
	}
	// Already present: return the stored values, never the caller's. A repeat
	// preparation of the same source must not be able to re-derive a different
	// purpose or attribution and have it silently believed.
	return lockOperation(ctx, tx, row.OperationID)
}

// ensureAccountControl creates the account's sending-control row when absent
// and returns its current state, holding the row lock.
//
// Creating it here — rather than at signup — is what guarantees every account
// that has ever tried to send has budget and detector state, including accounts
// created before this system existed and accounts created by a code path that
// forgets. The row is the account's sending identity; a missing one must never
// read as "no restrictions".
func ensureAccountControl(ctx context.Context, tx pgx.Tx, userID string) (state string, tenantName string, tenantReady bool, err error) {
	err = tx.QueryRow(ctx, `
		INSERT INTO account_sending_controls (user_id)
		VALUES ($1)
		ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id
		RETURNING state, ses_tenant_name, ses_tenant_ready`, userID,
	).Scan(&state, &tenantName, &tenantReady)
	if err != nil {
		return "", "", false, fmt.Errorf("sendingpolicy: ensure account control: %w", err)
	}
	return state, tenantName, tenantReady, nil
}

// PrepareExternalTx derives the provider operation for an accepted outbound
// customer message, inside the same transaction that durably inserts it.
//
// It returns an acceptance verdict rather than a budget verdict on purpose.
// Budgets are decided immediately before the provider call, so a customer who
// has used today's allowance still gets their message queued and sent after
// midnight; only a paused account is refused at the door, because queueing mail
// that can never leave is worse than saying no.
func (m *Module) PrepareExternalTx(ctx context.Context, tx pgx.Tx, messageID string) (AcceptanceDecision, OperationRef, error) {
	if strings.TrimSpace(messageID) == "" {
		return "", OperationRef{}, ErrSourceUnavailable
	}

	// Agent before message, matching migration 113 and the irreversible
	// deletion path: deletion locks an agent and then its messages, so taking
	// them in the other order here would deadlock against a concurrent purge.
	var agentID string
	err := tx.QueryRow(ctx,
		`SELECT agent_id FROM messages WHERE id = $1 AND direction = 'outbound'`, messageID,
	).Scan(&agentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", OperationRef{}, ErrSourceUnavailable
	}
	if err != nil {
		return "", OperationRef{}, fmt.Errorf("sendingpolicy: read message: %w", err)
	}

	var userID string
	err = tx.QueryRow(ctx,
		`SELECT user_id FROM agent_identities WHERE id = $1 FOR UPDATE`, agentID,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", OperationRef{}, ErrSourceUnavailable
	}
	if err != nil {
		return "", OperationRef{}, fmt.Errorf("sendingpolicy: lock agent: %w", err)
	}

	// Recheck identity and direction under the message lock. Between the
	// unlocked read above and this one the row could have been retargeted or
	// removed; attribution must come from the locked tuple.
	var sentAs, method *string
	var scheduledAt *time.Time
	var toCount, ccCount, bccCount int
	err = tx.QueryRow(ctx, `
		SELECT sent_as, method, scheduled_at,
		       COALESCE(cardinality(to_recipients), 0),
		       COALESCE(cardinality(cc), 0),
		       COALESCE(cardinality(bcc), 0)
		  FROM messages
		 WHERE id = $1 AND agent_id = $2 AND direction = 'outbound'
		   FOR UPDATE`, messageID, agentID,
	).Scan(&sentAs, &method, &scheduledAt, &toCount, &ccCount, &bccCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", OperationRef{}, ErrSourceUnavailable
	}
	if err != nil {
		return "", OperationRef{}, fmt.Errorf("sendingpolicy: lock message: %w", err)
	}

	state, _, _, err := ensureAccountControl(ctx, tx, userID)
	if err != nil {
		return "", OperationRef{}, err
	}
	if state == "paused" {
		return AcceptanceSendingPaused, OperationRef{}, nil
	}

	// An exact agent-to-itself local delivery never reaches SES, so it gets no
	// provider operation and consumes nothing. The zero reference is the
	// contract: a caller holding one has nothing to reserve, which is exactly
	// right for a message that will never open a socket.
	//
	// The exemption is granted on the message's SHAPE, not on its label. A
	// `method` column saying "loopback" is one write away from being the whole
	// bypass, so the row must also look like the only thing the spec exempts:
	// exactly one To and no Cc or Bcc. Anything else is treated as ordinary
	// provider-bound mail and budgeted.
	if method != nil && *method == "loopback" && toCount == 1 && ccCount == 0 && bccCount == 0 {
		return AcceptanceAccept, OperationRef{}, nil
	}

	notBefore := time.Time{}
	if scheduledAt != nil {
		notBefore = *scheduledAt
	}
	row, err := insertOperation(ctx, tx, operationRow{
		OperationID:      messageID,
		SourceAccountRef: &userID,
		PolicySubjectRef: userID,
		Purpose:          PurposeCustomerMessage,
		Shared:           sharedFromSentAs(sentAs),
	}, notBefore)
	if err != nil {
		return "", OperationRef{}, err
	}
	return AcceptanceAccept, row.ref(), nil
}

// sharedFromSentAs classifies a message's reputation surface from the
// server-owned sent_as column.
//
// Unknown reads as shared. `own_address` is the only value that proves the
// customer's own verified domain carried the mail; anything else — the shared
// relay, a legacy row written before the column existed, a future value this
// binary does not know — is treated as borrowing platform reputation, which
// only ever tightens the applicable cap. Guessing the other way would hand a
// 50/day exemption to whatever wrote an unexpected value.
func sharedFromSentAs(sentAs *string) bool {
	return sentAs == nil || *sentAs != "own_address"
}

// PrepareNotificationTx derives the provider operation for platform mail a
// customer's own action triggered.
//
// These are attributed to and budgeted against the triggering customer, not the
// platform, because a customer controls how much of this mail exists: every
// held message is an approval email and every failing webhook is a health
// warning. Their From identity is platform-owned, so they also carry the shared
// reputation class and the stricter shared-domain cap that comes with it.
func (m *Module) PrepareNotificationTx(ctx context.Context, tx pgx.Tx, ref NotificationRef) (OperationRef, error) {
	if strings.TrimSpace(ref.id) == "" {
		return OperationRef{}, ErrSourceUnavailable
	}

	var userID, operationID string
	var err error
	switch ref.source {
	case NotificationHITLMessage:
		userID, err = lockHITLSourceOwner(ctx, tx, ref.id)
		operationID = HITLNotificationOperationID(ref.id)
	case NotificationWebhookHealth:
		// The operation is keyed by the episode the sweep stamped in the
		// same transaction that enqueues the notice, so preparing the same
		// episode twice (an enqueue and a later legacy resolve, or two
		// resolvers racing) yields one operation, and a job whose reference
		// names another episode is detectably stale.
		var warnedAt, disabledAt *time.Time
		err = tx.QueryRow(ctx,
			`SELECT user_id, warn_notified_at, auto_disabled_at FROM webhooks WHERE id = $1 FOR UPDATE`, ref.id,
		).Scan(&userID, &warnedAt, &disabledAt)
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrSourceUnavailable
		}
		if err == nil {
			var episode *time.Time
			switch ref.kind {
			case WebhookHealthKindWarning:
				episode = warnedAt
			case WebhookHealthKindDisabled:
				episode = disabledAt
			}
			if episode == nil {
				// Unknown kind, or an episode the sweep never stamped:
				// there is no notice to send, so there is nothing to
				// authorize.
				return OperationRef{}, ErrSourceUnavailable
			}
			operationID = WebhookHealthOperationID(ref.id, ref.kind, *episode)
		}
	default:
		return OperationRef{}, ErrSourceUnavailable
	}
	if err != nil {
		if errors.Is(err, ErrSourceUnavailable) {
			return OperationRef{}, err
		}
		return OperationRef{}, fmt.Errorf("sendingpolicy: lock notification source: %w", err)
	}

	if _, _, _, err := ensureAccountControl(ctx, tx, userID); err != nil {
		return OperationRef{}, err
	}

	row, err := insertOperation(ctx, tx, operationRow{
		OperationID:      operationID,
		SourceAccountRef: &userID,
		PolicySubjectRef: userID,
		Purpose:          PurposeCustomerNotification,
		Shared:           true,
	}, time.Time{})
	if err != nil {
		return OperationRef{}, err
	}
	return row.ref(), nil
}

// lockHITLSourceOwner resolves and locks the owner of a pending message.
func lockHITLSourceOwner(ctx context.Context, tx pgx.Tx, messageID string) (string, error) {
	var agentID string
	err := tx.QueryRow(ctx,
		`SELECT agent_id FROM messages WHERE id = $1 AND direction = 'outbound'`, messageID,
	).Scan(&agentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSourceUnavailable
	}
	if err != nil {
		return "", err
	}

	var userID string
	err = tx.QueryRow(ctx,
		`SELECT user_id FROM agent_identities WHERE id = $1 FOR UPDATE`, agentID,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSourceUnavailable
	}
	if err != nil {
		return "", err
	}

	// Recheck identity, direction AND review state under the lock. A
	// notification is owed only for a message actually awaiting approval;
	// deriving one from a message in any other state would let a settled
	// message mint customer-attributed provider capacity.
	var exists bool
	err = tx.QueryRow(ctx, `
		SELECT true FROM messages
		 WHERE id = $1 AND agent_id = $2 AND direction = 'outbound'
		   AND status = 'pending_review'
		   FOR UPDATE`, messageID, agentID,
	).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSourceUnavailable
	}
	if err != nil {
		return "", err
	}
	return userID, nil
}

// PrepareProtectionNoticeTx allocates or resumes the one stable operation for a
// committed notice event and audience.
//
// Stability is the point. A pause notice may be retried for days; every
// physical retry must be a greater submission ordinal on the SAME operation, so
// that the ledger can prove at most one socket per ordinal and so a retry can
// never mint a second logical notice. Deliberately no recipient is resolved
// here: the owner address is whatever it is at final authorization, and binding
// it now would mail a retired address after a legitimate account edit.
func (m *Module) PrepareProtectionNoticeTx(ctx context.Context, tx pgx.Tx, ref ProtectionNoticeRef) (OperationRef, error) {
	if strings.TrimSpace(ref.eventID) == "" || !ref.audience.valid() {
		return OperationRef{}, ErrSourceUnavailable
	}

	var kind string
	var accountRef *string
	var existingOperation *string
	var state string
	err := tx.QueryRow(ctx, `
		SELECT event.kind, event.account_ref, delivery.current_operation_id, delivery.state
		  FROM sending_protection_notice_deliveries AS delivery
		  JOIN sending_protection_notice_events AS event ON event.id = delivery.event_id
		 WHERE delivery.event_id = $1 AND delivery.audience = $2
		   FOR UPDATE OF delivery`, ref.eventID, string(ref.audience),
	).Scan(&kind, &accountRef, &existingOperation, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationRef{}, ErrSourceUnavailable
	}
	if err != nil {
		return OperationRef{}, fmt.Errorf("sendingpolicy: lock notice delivery: %w", err)
	}

	// A global guardrail incident has no customer to blame, so it has no owner
	// to mail. The schema already forbids the row; refusing here as well means
	// the invariant holds even if a future migration relaxes the constraint.
	if kind == "global_guardrail" && ref.audience == AudienceOwner {
		return OperationRef{}, ErrAudienceNotAllowed
	}

	// A delivery that already reached a terminal state is finished. Re-preparing
	// it would resume its stable operation and let a fresh ordinal authorize a
	// SECOND physical send of a notice already sent — the one thing the stable
	// operation exists to prevent. Terminality has to live here rather than in
	// the drain worker's query, or it is only as good as the caller.
	if state != "pending" {
		return OperationRef{}, ErrNoticeSettled
	}

	if existingOperation != nil && *existingOperation != "" {
		row, err := lockOperation(ctx, tx, *existingOperation)
		if err != nil {
			return OperationRef{}, err
		}
		return row.ref(), nil
	}

	purpose := PurposeViolationOperational
	if kind == "pause" {
		purpose = PurposeCriticalOperational
	}

	// The affected customer is the SOURCE of the notice, never its authority.
	// A paused account must still receive the email telling it that it was
	// paused, so the policy subject is the fixed system account whose state no
	// customer action can change.
	row, err := insertOperation(ctx, tx, operationRow{
		OperationID:      randomID("opn_"),
		SourceAccountRef: accountRef,
		PolicySubjectRef: SystemPolicySubject,
		Purpose:          purpose,
		Shared:           false,
	}, time.Time{})
	if err != nil {
		return OperationRef{}, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE sending_protection_notice_deliveries
		   SET current_operation_id = $3, updated_at = now()
		 WHERE event_id = $1 AND audience = $2`,
		ref.eventID, string(ref.audience), row.OperationID,
	); err != nil {
		return OperationRef{}, fmt.Errorf("sendingpolicy: bind notice operation: %w", err)
	}
	return row.ref(), nil
}

// PreparePublicFeedback derives the operation for one /api/feedback fan-out.
//
// The unauthenticated endpoint has no account to charge, but its mail leaves
// through the same provider and damages the same reputation, so it consumes the
// platform and probation pools. Neither the recipient set nor the purpose comes
// from the request: the submission ID is server-minted and the envelope is
// configuration, which is what stops the form from becoming an open relay.
func (m *Module) PreparePublicFeedback(ctx context.Context, ref PublicFeedbackRef) (OperationRef, error) {
	if strings.TrimSpace(ref.submissionID) == "" {
		return OperationRef{}, ErrSourceUnavailable
	}
	recipients, err := normalizeEnvelope(ref.recipients)
	if err != nil {
		return OperationRef{}, err
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return OperationRef{}, fmt.Errorf("sendingpolicy: begin public feedback: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := insertOperation(ctx, tx, operationRow{
		// Keyed by the submission so a handler retry inside one request
		// reuses the operation rather than minting a second one that would
		// charge the pools twice for the same feedback.
		OperationID:      "opf_" + ref.submissionID,
		SourceAccountRef: nil,
		PolicySubjectRef: SystemPolicySubject,
		Purpose:          PurposePublicFeedback,
		Shared:           false,
	}, time.Time{})
	if err != nil {
		return OperationRef{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OperationRef{}, fmt.Errorf("sendingpolicy: commit public feedback: %w", err)
	}

	out := row.ref()
	// The configured envelope rides on the in-memory reference because there
	// is nowhere durable to re-derive it from and nothing that should:
	// public feedback runs inside one request's bounded retry loop and never
	// crosses a process boundary. A reference deserialized from anywhere else
	// arrives without recipients and is refused at final authorization.
	out.recipients = recipients
	return out, nil
}
