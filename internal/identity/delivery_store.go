package identity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

// OutboundSendClaimStaleWindow exceeds River's one-minute worker timeout and
// bounds how long a crashed worker can prevent permanent trash deletion.
const OutboundSendClaimStaleWindow = 10 * time.Minute

// --- Delivery feedback (decision 9 / Slice 4b) ---
//
// These back internal/delivery's Consumer.Store and the send path + the
// /v1/account/suppressions endpoints. delivery is a stdlib-only leaf package,
// so identity importing it (for delivery.Status / delivery.Merge) adds no heavy
// deps — unlike senderidentity, no adapter is needed.

// CorrelateBySESMessageID finds the outbound message + owning user by the
// SES-assigned provider_message_id captured at send time. found=false when the
// id is unknown (deleted message, or an event for a different deployment).
//
// The message fields (subject, and the envelope/threading fields the
// message-level email.failed payload needs on an SES Reject) ride along —
// they're columns on the very row this query already reads, so including them
// costs no extra query (contract freeze PR-2: `subject` on delivery events).
//
// The SNS notification carries the BARE SES id (e.g. 010f0193…-000000), but the
// send path stores it angle-bracketed and sometimes with an @region.amazonses.com
// suffix (parseMessageIDFromResponse) — same discrepancy LookupConversationID
// works around. Match all three stored shapes against the bare id: exact,
// <id>, and <id@…>. SES ids are [A-Za-z0-9-] so they carry no LIKE metacharacters.
func (s *Store) CorrelateBySESMessageID(ctx context.Context, sesMessageID string) (*delivery.CorrelatedMessage, bool, error) {
	if sesMessageID == "" {
		return nil, false, nil
	}
	m := &delivery.CorrelatedMessage{}
	err := s.pool.QueryRow(ctx,
		`SELECT m.id, a.user_id, m.agent_id, COALESCE(m.subject, ''),
		        COALESCE(m.conversation_id, ''), COALESCE(m.method, ''),
		        COALESCE(m.message_type, ''), COALESCE(m.sender, ''),
		        m.to_recipients, m.cc, m.bcc
		   FROM messages m
		   JOIN agent_identities a ON a.id = m.agent_id
		  WHERE m.direction = 'outbound'
		    AND ( m.provider_message_id = $1
		       OR m.provider_message_id = '<' || $1 || '>'
		       OR m.provider_message_id LIKE '<' || $1 || '@%' )
		  LIMIT 1`,
		sesMessageID,
	).Scan(&m.MessageID, &m.UserID, &m.AgentID, &m.Subject,
		&m.ConversationID, &m.Method, &m.MessageType, &m.From,
		&m.To, &m.CC, &m.BCC)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// CorrelateByE2AMessageID finds the outbound message + owning user/agent by
// the e2a message id SES echoed back from the X-E2A-Message-ID wire header
// (delivery.MessageIDHeader) — the §3.1 correlation fallback for the
// SMTP-accept↔mark-sent crash window, where the provider id from the SMTP 250
// was never captured so CorrelateBySESMessageID cannot match. Same return
// shape as CorrelateBySESMessageID. The id is shape-validated by the caller
// (internal/delivery) before it reaches this lookup.
func (s *Store) CorrelateByE2AMessageID(ctx context.Context, e2aMessageID string) (*delivery.CorrelatedMessage, bool, error) {
	if e2aMessageID == "" {
		return nil, false, nil
	}
	m := &delivery.CorrelatedMessage{}
	err := s.pool.QueryRow(ctx,
		`SELECT m.id, a.user_id, m.agent_id, COALESCE(m.subject, ''),
		        COALESCE(m.conversation_id, ''), COALESCE(m.method, ''),
		        COALESCE(m.message_type, ''), COALESCE(m.sender, ''),
		        m.to_recipients, m.cc, m.bcc
		   FROM messages m
		   JOIN agent_identities a ON a.id = m.agent_id
		  WHERE m.id = $1 AND m.direction = 'outbound'`,
		e2aMessageID,
	).Scan(&m.MessageID, &m.UserID, &m.AgentID, &m.Subject,
		&m.ConversationID, &m.Method, &m.MessageType, &m.From,
		&m.To, &m.CC, &m.BCC)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// RecordProviderAcceptEvidence durably notes that SES reported having accepted
// this message's submission (the SNS consumer calls it for every correlated
// post-acceptance notification) and repairs a provider_message_id lost to the
// SMTP-accept↔mark-sent crash window. Idempotent: the first-seen timestamp and
// a previously captured provider id are never overwritten. The terminal-failure
// guards (send worker, terminal reconciler, MarkOutboundFailedTx's CAS) read
// this evidence before declaring an accepted/sending row failed.
func (s *Store) RecordProviderAcceptEvidence(ctx context.Context, messageID, sesMessageID string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		return s.RecordProviderAcceptEvidenceTx(ctx, tx, messageID, sesMessageID, time.Now().UTC())
	})
}

func (s *Store) RecordProviderAcceptEvidenceTx(ctx context.Context, tx pgx.Tx, messageID, sesMessageID string, occurredAt time.Time) error {
	_, err := tx.Exec(ctx,
		`UPDATE messages
		    SET provider_accepted_at = COALESCE(provider_accepted_at, $3),
		        provider_message_id = CASE WHEN COALESCE(provider_message_id, '') = ''
		                                   THEN $2 ELSE provider_message_id END
		  WHERE id = $1 AND direction = 'outbound'`,
		messageID, sesMessageID, occurredAt.UTC())
	return err
}

func (s *Store) AppendLifecycleTx(ctx context.Context, tx pgx.Tx, input messagelifecycle.AppendInput) (messagelifecycle.MessageLifecycleTransition, error) {
	return messagelifecycle.AppendTx(ctx, tx, input)
}

// ProviderAcceptancePendingTx reports whether signed provider evidence must be
// finalized through the canonical outbound sent path before feedback can be
// applied. Pre-terminal rows and locally inferred failures are correctable;
// authoritative provider failures are not. The consumer fails closed when
// that finalizer is unavailable.
func (s *Store) ProviderAcceptancePendingTx(ctx context.Context, tx pgx.Tx, messageID string) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx, `SELECT provider_accepted_at IS NOT NULL AND (
		delivery_status IN ('accepted','sending') OR
		(delivery_status='failed' AND COALESCE(delivery_failure_source,'local')='local')
	) FROM messages WHERE id=$1 FOR UPDATE`, messageID).Scan(&pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return pending, err
}

func (s *Store) OutboundSendJobIDTx(ctx context.Context, tx pgx.Tx, messageID string) (int64, error) {
	var jobID int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(send_job_id,0) FROM messages WHERE id=$1 FOR UPDATE`, messageID).Scan(&jobID)
	return jobID, err
}

// RecordProviderRejectTx establishes the authoritative terminal provenance
// for an SES Reject, including when a local inferred failure was already
// present. A later acceptance/delivery observation therefore cannot revive
// this message through the local-failure correction path.
func (s *Store) RecordProviderRejectTx(ctx context.Context, tx pgx.Tx, messageID, detail string, occurredAt time.Time) error {
	_, err := tx.Exec(ctx,
		`UPDATE messages
		    SET delivery_status=CASE WHEN delivery_status IN ('bounced','complained') THEN delivery_status ELSE 'failed' END,
		        delivery_failure_source=CASE WHEN delivery_status IN ('bounced','complained') THEN delivery_failure_source ELSE 'provider' END,
		        delivery_failure_reason_code=CASE WHEN delivery_status IN ('bounced','complained') THEN delivery_failure_reason_code ELSE $2 END,
		        delivery_detail=CASE WHEN delivery_status IN ('bounced','complained') THEN delivery_detail ELSE $3 END,
		        delivery_failure_occurred_at=CASE WHEN delivery_status IN ('bounced','complained') THEN delivery_failure_occurred_at ELSE $4 END,
		        delivery_failure_attempt=CASE WHEN delivery_status IN ('bounced','complained') THEN delivery_failure_attempt ELSE NULL END,
		        delivery_failure_blocked_recipients=CASE WHEN delivery_status IN ('bounced','complained') THEN delivery_failure_blocked_recipients ELSE NULL END,
		        send_claimed_at=NULL
		  WHERE id=$1 AND direction='outbound'`,
		messageID, string(messagelifecycle.ReasonSubmissionProviderRejected), nullIfEmpty(messagelifecycle.SafeDiagnostic(detail)), occurredAt.UTC())
	return err
}

// RecordDeliveryOutcome upserts one recipient's status monotonically (by the
// delivery precedence) and recomputes the message's rollup delivery_status as
// the worst status across its recipients. Runs in a tx with FOR UPDATE so
// concurrent SNS events can't race the merge. Idempotent: a duplicate or older
// event is a no-op for the status (detail still refreshes on an equal/higher).
//
// Correction rule (async-send-contract §3.1): when the message-level status is
// a locally inferred `failed` (delivery_failure_source 'local' or legacy NULL),
// a recomputed rollup that proves provider acceptance CORRECTS the row — the
// falsely-declared terminal failure from a final-attempt crash. A
// provider-confirmed `failed` is never revived, and feedback for an address
// outside the message's own envelope is always ignored so a foreign recipient
// can never create state or lifecycle for this message.
func (s *Store) RecordDeliveryOutcome(ctx context.Context, messageID, address string, status delivery.Status, detail string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := s.RecordDeliveryOutcomeTx(ctx, tx, messageID, address, status, detail)
		return err
	})
}

// HasApplicableRecipientTx locks the message and checks candidate provider
// recipients against its persisted immutable envelope. This preflight keeps a
// foreign-only notification from establishing provider-accept evidence before
// RecordDeliveryOutcomeTx gets a chance to apply its per-recipient gate.
func (s *Store) HasApplicableRecipientTx(ctx context.Context, tx pgx.Tx, messageID string, addresses []string) (bool, error) {
	var envTo, envCC, envBCC []string
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(to_recipients, '{}'), COALESCE(cc, '{}'), COALESCE(bcc, '{}')
		   FROM messages WHERE id=$1 FOR UPDATE`,
		messageID,
	).Scan(&envTo, &envCC, &envBCC)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, address := range addresses {
		if addressInEnvelope(NormalizeEmail(address), envTo, envCC, envBCC) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) RecordDeliveryOutcomeTx(ctx context.Context, tx pgx.Tx, messageID, address string, status delivery.Status, detail string) (bool, error) {
	addr := NormalizeEmail(address)

	// Lock the message row to serialize ALL events for this message (every
	// recipient). SES fans out delivery + bounce/complaint for the same message
	// concurrently; without this, two events for different recipients (or two
	// first-events for an un-pre-populated recipient) race the rollup write and
	// the insert ON CONFLICT path, dropping a terminal status. The lock makes
	// the read-merge-write below strictly monotonic per message, and serializes
	// the §3.1 correction against the terminal writers (MarkOutboundFailedTx /
	// ResolveOutboundProviderAcceptedTx lock the same row).
	var (
		curStatus, curSource string
		envTo, envCC, envBCC []string
	)
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(delivery_status, ''), COALESCE(delivery_failure_source, ''),
		        COALESCE(to_recipients, '{}'), COALESCE(cc, '{}'), COALESCE(bcc, '{}')
		   FROM messages WHERE id = $1 FOR UPDATE`,
		messageID,
	).Scan(&curStatus, &curSource, &envTo, &envCC, &envBCC)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // message deleted between correlation and now
	}
	if err != nil {
		return false, err
	}
	cur := delivery.Status(curStatus)

	// Only feedback for an address in this message's immutable envelope is an
	// applicable observation. Provider payload corruption or a foreign address
	// must not create recipient state, lifecycle, suppression, or events.
	if !addressInEnvelope(addr, envTo, envCC, envBCC) {
		return false, nil
	}

	var curRecipient string
	err = tx.QueryRow(ctx,
		`SELECT status FROM message_recipients WHERE message_id = $1 AND address = $2`,
		messageID, addr,
	).Scan(&curRecipient)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Recipient row not pre-populated (e.g. SES reports an address the send
		// path didn't record). Serialized by the message-row lock, so no insert
		// race.
		if _, err := tx.Exec(ctx,
			`INSERT INTO message_recipients (id, message_id, address, status, detail)
			 VALUES ($1, $2, $3, $4, $5)`,
			"rcpt_"+generateID(), messageID, addr, string(status), nullIfEmpty(detail),
		); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		merged := delivery.Merge(delivery.Status(curRecipient), status)
		// Only write when the status actually advances — a duplicate/lower-rank
		// event must not regress the status NOR clobber the diagnostic detail
		// (a late `delivered` carrying a detail must not overwrite the bounce
		// reason).
		if merged != delivery.Status(curRecipient) {
			if _, err := tx.Exec(ctx,
				`UPDATE message_recipients SET status = $3, detail = COALESCE($4, detail), updated_at = now()
				  WHERE message_id = $1 AND address = $2`,
				messageID, addr, string(merged), nullIfEmpty(detail),
			); err != nil {
				return false, err
			}
		}
	}

	// Recompute the rollup = worst recipient status by precedence. Few
	// recipients per message, so reduce in Go to keep the rank logic in one
	// place (delivery.Merge).
	rows, err := tx.Query(ctx, `SELECT status FROM message_recipients WHERE message_id = $1`, messageID)
	if err != nil {
		return false, err
	}
	var rollup delivery.Status
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			rows.Close()
			return false, err
		}
		rollup = delivery.Merge(rollup, delivery.Status(st))
	}
	rows.Close()
	if rollup != "" {
		next := delivery.ResolveMessageRollup(cur, delivery.FailureSource(curSource), rollup)
		switch {
		case next == cur:
			// No transition (includes a provider-confirmed failed holding
			// against a delivered rollup) — idempotent no-op.
		case next == delivery.StatusFailed:
			// Reject-driven rollup: the failure came from correlated provider
			// feedback, so it is provider-confirmed — record the provenance so
			// the §3.1 correction can never revive it.
			if _, err := tx.Exec(ctx,
				`UPDATE messages
				    SET delivery_status = 'failed',
				        delivery_failure_source = COALESCE(delivery_failure_source, 'provider')
				  WHERE id = $1`, messageID,
			); err != nil {
				return false, err
			}
		case cur == delivery.StatusFailed:
			// §3.1 correction: authoritatively correlated provider evidence
			// overrides the locally inferred failure. Clear the failure
			// provenance + the stale failure detail (the per-recipient rows
			// carry the provider diagnostics).
			if _, err := tx.Exec(ctx,
				`UPDATE messages
				    SET delivery_status = $2, delivery_failure_source = NULL, delivery_failure_reason_code = NULL, delivery_detail = NULL,
				        delivery_failure_occurred_at = NULL, delivery_failure_attempt = NULL, delivery_failure_blocked_recipients = NULL
				  WHERE id = $1`, messageID, string(next),
			); err != nil {
				return false, err
			}
		default:
			if _, err := tx.Exec(ctx,
				`UPDATE messages SET delivery_status = $2 WHERE id = $1`, messageID, string(next),
			); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// addressInEnvelope reports whether addr (already normalized) is one of the
// message's own to/cc/bcc recipients.
func addressInEnvelope(addr string, lists ...[]string) bool {
	for _, list := range lists {
		for _, a := range list {
			if NormalizeEmail(a) == addr {
				return true
			}
		}
	}
	return false
}

// MarkMessageSent records that an outbound message was accepted by the relay:
// delivery_status='sent', the From identity actually used, and one
// message_recipients row per recipient (to/cc/bcc) at 'sent'. Called after a
// successful relay accept. Idempotent on the recipient rows.
func (s *Store) MarkMessageSent(ctx context.Context, messageID, sentAs string, to, cc, bcc []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE messages SET delivery_status = 'sent', sent_as = $2 WHERE id = $1`,
		messageID, nullIfEmpty(sentAs),
	); err != nil {
		return err
	}
	add := func(addrs []string, kind string) error {
		for _, a := range addrs {
			addr := NormalizeEmail(a)
			if addr == "" {
				continue
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO message_recipients (id, message_id, address, kind, status)
				 VALUES ($1, $2, $3, $4, 'sent')
				 ON CONFLICT (message_id, address) DO NOTHING`,
				"rcpt_"+generateID(), messageID, addr, kind,
			); err != nil {
				return err
			}
		}
		return nil
	}
	if err := add(to, "to"); err != nil {
		return err
	}
	if err := add(cc, "cc"); err != nil {
		return err
	}
	if err := add(bcc, "bcc"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- Async outbound send (async-message-pipeline.md, slice C) ---

// OutboundSendPayload is the async worker's view of an accepted outbound message
// (the LoadOutboundForSend result). Recipients is the SMTP envelope (to+cc+bcc);
// Raw is the persisted Sent-folder bytes the worker submits via Sender.SubmitOnce.
type OutboundSendPayload struct {
	ID string
	// UserID is the owning account (agent_identities.user_id) — the tenant
	// scope for the worker's pre-provider suppression guard.
	UserID         string
	AgentID        string
	Domain         string
	MessageType    string
	DeliveryStatus string
	EnvelopeFrom   string
	SentAs         string
	Recipients     []string
	Raw            []byte
	CreatedAt      time.Time // accept time — the outage-tail retry-horizon clock
	// ProviderAccepted is true when the SNS consumer recorded provider-accept
	// evidence for this message (provider_accepted_at): the provider already
	// has it, so the worker must settle it as sent instead of re-submitting
	// (the SMTP-accept↔mark-sent crash window's duplicate residual).
	ProviderAccepted   bool
	ProviderAcceptedAt *time.Time
	// ScheduledAt is messages.scheduled_at for a scheduled send (nil for an
	// immediate one). The retry-horizon clock starts at max(CreatedAt,
	// ScheduledAt) so a long-scheduled send keeps the full outage-tolerant tail
	// from its fire time instead of a horizon already blown at fire.
	ScheduledAt *time.Time
	// ProviderMessageID is the evidence-repaired provider id ('' when none).
	ProviderMessageID string
}

// OutboundSentInfo carries the fields the async worker's MarkSent/MarkFailed
// adapters need to build the email.sent / email.failed event and meter usage,
// resolved from the row + its owning agent in one transaction.
type OutboundSentInfo struct {
	Message            *Message
	UserID             string
	Domain             string
	ProviderAcceptedAt time.Time
}

// SendOutcome is the current terminal-ish state of an async send, for wait=sent
// polling. DeliveryStatus is "" when the row is gone.
type SendOutcome struct {
	DeliveryStatus    string
	ProviderMessageID string
	SentAs            string
	DeliveryDetail    string
}

// GetSendOutcome reads the current delivery_status + provider id + sent_as + detail
// for an outbound message — the wait=sent poll. Returns a zero-value outcome (empty
// DeliveryStatus) if the row is gone, never an error for a missing row.
func (s *Store) GetSendOutcome(ctx context.Context, messageID string) (SendOutcome, error) {
	var o SendOutcome
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(delivery_status,''), COALESCE(provider_message_id,''),
		        COALESCE(sent_as,''), COALESCE(delivery_detail,'')
		   FROM messages WHERE id = $1 AND direction = 'outbound'`,
		messageID,
	).Scan(&o.DeliveryStatus, &o.ProviderMessageID, &o.SentAs, &o.DeliveryDetail)
	if errors.Is(err, pgx.ErrNoRows) {
		return SendOutcome{}, nil
	}
	if err != nil {
		return SendOutcome{}, err
	}
	return o, nil
}

// LoadOutboundForSend returns the payload the async send worker submits, or nil if
// the row is gone (agent delete / TTL) — the worker treats that as a no-op.
// A message in the trash, or one whose AGENT is in the trash, counts as gone:
// deleting is the user's one lever to stop a queued-but-unsent message (e.g.
// snoozed behind a provider-outage retry horizon), and a trashed inbox must
// never emit mail. Reads the envelope (to+cc+bcc) and the persisted wire
// bytes; does not touch message_recipients (those are written at MarkSent).
func (s *Store) LoadOutboundForSend(ctx context.Context, messageID string) (*OutboundSendPayload, error) {
	var (
		deliveryStatus   string
		envelopeFrom     string
		sentAs           string
		userID           string
		agentID          string
		registeredDomain string
		messageType      string
		to, cc, bcc      []string
		raw              []byte
		createdAt        time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(m.delivery_status,''), COALESCE(m.envelope_from,''), COALESCE(m.sent_as,''), a.user_id, m.agent_id, a.registered_domain,
		        COALESCE(m.message_type,''),
		        m.to_recipients, m.cc, m.bcc, m.raw_message, m.created_at
		   FROM messages m
		   JOIN agent_identities a ON a.id = m.agent_id
		  WHERE m.id = $1 AND m.direction = 'outbound'
		    AND m.deleted_at IS NULL AND a.deleted_at IS NULL`,
		messageID,
	).Scan(&deliveryStatus, &envelopeFrom, &sentAs, &userID, &agentID, &registeredDomain, &messageType, &to, &cc, &bcc, &raw, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	recipients := make([]string, 0, len(to)+len(cc)+len(bcc))
	recipients = append(recipients, to...)
	recipients = append(recipients, cc...)
	recipients = append(recipients, bcc...)
	return &OutboundSendPayload{
		ID:             messageID,
		UserID:         userID,
		AgentID:        agentID,
		Domain:         registeredDomain,
		MessageType:    messageType,
		DeliveryStatus: deliveryStatus,
		EnvelopeFrom:   envelopeFrom,
		SentAs:         sentAs,
		Recipients:     recipients,
		Raw:            raw,
		CreatedAt:      createdAt,
	}, nil
}

// ClaimOutboundForSend atomically moves the stamped River job's live outbound
// message into delivery_status='sending' and returns its provider payload. A
// retry of the same job may reclaim a row already in sending (River does not run
// two attempts of one job concurrently); a duplicate job id cannot claim it.
//
// The short transaction is the send/trash linearization point. If trash commits
// first, the claim misses and the queued send is canceled. If this claim commits
// first, trash may proceed while provider I/O runs, and MarkOutboundSentTx or
// MarkOutboundFailedTx still records the terminal result on the trashed row.
func (s *Store) ClaimOutboundForSend(ctx context.Context, messageID string, jobID int64) (*OutboundSendPayload, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin outbound send claim: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var agentID string
	err = tx.QueryRow(ctx,
		`SELECT agent_id FROM messages WHERE id = $1 AND direction = 'outbound'`,
		messageID,
	).Scan(&agentID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var (
		deliveryStatus     string
		envelopeFrom       string
		sentAs             string
		to, cc, bcc        []string
		raw                []byte
		createdAt          time.Time
		deletedAt          *time.Time
		agentDeletedAt     *time.Time
		stampedJobID       *int64
		providerAcceptedAt *time.Time
		providerMessageID  string
		messageType        string
		failureSource      string
		failureReason      string
		failureOccurredAt  *time.Time
		failureAttempt     *int
		scheduledAt        *time.Time
	)
	var userID, registeredDomain string
	// Lock agent first to match permanent agent deletion's lock order, then
	// lock the message. Message-only trash operations never need the agent lock.
	err = tx.QueryRow(ctx,
		`SELECT deleted_at, user_id, registered_domain FROM agent_identities WHERE id = $1 FOR UPDATE`,
		agentID,
	).Scan(&agentDeletedAt, &userID, &registeredDomain)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(m.delivery_status,''), COALESCE(m.envelope_from,''), COALESCE(m.sent_as,''),
		        COALESCE(m.message_type,''),
		        m.to_recipients, m.cc, m.bcc, m.raw_message, m.created_at,
		        m.deleted_at, m.send_job_id, m.provider_accepted_at, COALESCE(m.provider_message_id,''),
		        COALESCE(m.delivery_failure_source,''),COALESCE(m.delivery_failure_reason_code,''),
		        m.delivery_failure_occurred_at,m.delivery_failure_attempt,m.scheduled_at
		   FROM messages m
		  WHERE m.id = $1 AND m.agent_id = $2 AND m.direction = 'outbound'
		  FOR UPDATE OF m`,
		messageID, agentID,
	).Scan(&deliveryStatus, &envelopeFrom, &sentAs, &messageType, &to, &cc, &bcc, &raw, &createdAt,
		&deletedAt, &stampedJobID, &providerAcceptedAt, &providerMessageID,
		&failureSource, &failureReason, &failureOccurredAt, &failureAttempt, &scheduledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if stampedJobID == nil || *stampedJobID != jobID ||
		(deliveryStatus != "accepted" && deliveryStatus != "sending") {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if isCompleteTerminalFallback(failureSource, failureReason, failureOccurredAt, failureAttempt) {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if deletedAt != nil || agentDeletedAt != nil {
		// The locked snapshot says trash won before the claim. Cancel the queued
		// send without a webhook event; no provider attempt occurred in this run.
		// Provenance 'local' (a deliberate local cancel): if provider evidence
		// later proves an earlier crashed attempt DID reach SES, the §3.1
		// correction may still record the truthful outcome on the hidden row.
		if _, err := tx.Exec(ctx,
			`UPDATE messages
			    SET delivery_status = 'failed',
			        delivery_detail = 'send canceled because the message or agent is in trash',
			        delivery_failure_source = 'local',
			        delivery_failure_reason_code = 'submission.cancelled',
			        send_claimed_at = NULL
			  WHERE id = $1`,
			messageID,
		); err != nil {
			return nil, err
		}
		if _, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{MessageID: messageID, DedupeKey: fmt.Sprintf("submission:job:%d:attempt:0:%s", jobID, messagelifecycle.ReasonSubmissionCancelled), Direction: "outbound", ReasonCode: messagelifecycle.ReasonSubmissionCancelled, CorrelationIDs: messagelifecycle.SafeCorrelationIDs(map[string]string{"job_id": fmt.Sprint(jobID)}), OccurredAt: time.Now().UTC()}); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE messages SET delivery_status = 'sending', send_claimed_at = now() WHERE id = $1`,
		messageID,
	); err != nil {
		return nil, err
	}
	deliveryStatus = "sending"
	recipients := make([]string, 0, len(to)+len(cc)+len(bcc))
	recipients = append(recipients, to...)
	recipients = append(recipients, cc...)
	recipients = append(recipients, bcc...)
	p := &OutboundSendPayload{
		ID:                 messageID,
		UserID:             userID,
		AgentID:            agentID,
		Domain:             registeredDomain,
		MessageType:        messageType,
		DeliveryStatus:     deliveryStatus,
		EnvelopeFrom:       envelopeFrom,
		SentAs:             sentAs,
		Recipients:         recipients,
		Raw:                raw,
		CreatedAt:          createdAt,
		ProviderAccepted:   providerAcceptedAt != nil,
		ProviderAcceptedAt: providerAcceptedAt,
		ProviderMessageID:  providerMessageID,
		ScheduledAt:        scheduledAt,
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func isCompleteTerminalFallback(source, reason string, occurredAt *time.Time, attempt *int) bool {
	if occurredAt == nil || occurredAt.IsZero() || attempt == nil || *attempt < 0 {
		return false
	}
	switch messagelifecycle.ReasonCode(reason) {
	case messagelifecycle.ReasonSubmissionProviderRejected:
		return delivery.FailureSource(source) == delivery.FailureSourceProvider
	case messagelifecycle.ReasonSubmissionLocalRetriesExhausted, messagelifecycle.ReasonSubmissionCancelled:
		return delivery.FailureSource(source) == delivery.FailureSourceLocal
	default:
		return false
	}
}

// ReleaseOutboundSendClaim moves a side-effect-free provider failure back to
// accepted so River backoff is not mistaken for an active provider call.
func (s *Store) ReleaseOutboundSendClaim(ctx context.Context, messageID string, jobID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE messages
		    SET delivery_status = 'accepted', send_claimed_at = NULL
		  WHERE id = $1 AND send_job_id = $2 AND delivery_status = 'sending'`,
		messageID, jobID,
	)
	return err
}

// DeferOutboundTerminalFailure records a final attempt's diagnostic and
// releases the I/O claim WITHOUT declaring the message failed. The worker
// calls it when its last attempt errored ambiguously (the provider may have
// accepted the submission before the failure — the SMTP-accept↔mark-sent
// crash window): the terminal reconciler then declares the outcome after the
// provider-evidence grace window, preferring this stored detail over its
// generic sweep message, or settles the row as sent if evidence arrives.
// Job-scoped so a stale worker can't scribble on a re-enqueued message.
func (s *Store) DeferOutboundTerminalFailure(ctx context.Context, messageID string, jobID int64, detail string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE messages
		    SET delivery_detail = $3, send_claimed_at = NULL
		  WHERE id = $1 AND direction = 'outbound' AND send_job_id = $2
		    AND delivery_status IN ('accepted', 'sending')`,
		messageID, jobID, nullIfEmpty(detail),
	)
	return err
}

// PreserveOutboundTerminalFailure durably records authoritative failure
// provenance after the richer terminal transaction failed. It deliberately
// leaves the row preterminal and emits no event so the terminal reconciler can
// retry the complete state+lifecycle+event transaction later.
func (s *Store) PreserveOutboundTerminalFailure(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, detail string, source delivery.FailureSource, reason messagelifecycle.ReasonCode, blockedRecipients []string) error {
	if !messagelifecycle.IsTerminalSubmissionFailure(reason) {
		return fmt.Errorf("invalid terminal submission reason %q", reason)
	}
	if occurredAt.IsZero() {
		return fmt.Errorf("terminal failure occurred_at is required")
	}
	if attempt < 0 {
		return fmt.Errorf("terminal failure attempt must be non-negative")
	}
	seen := make(map[string]struct{}, len(blockedRecipients))
	for _, raw := range blockedRecipients {
		if recipient := NormalizeEmail(raw); recipient != "" {
			seen[recipient] = struct{}{}
		}
	}
	blockedRecipients = blockedRecipients[:0]
	for recipient := range seen {
		blockedRecipients = append(blockedRecipients, recipient)
	}
	sort.Strings(blockedRecipients)
	var storedRecipients any
	if len(blockedRecipients) > 0 {
		storedRecipients = blockedRecipients
	}
	_, err := s.pool.Exec(ctx, `UPDATE messages SET delivery_status='accepted',delivery_detail=$3,delivery_failure_source=$4,delivery_failure_reason_code=$5,delivery_failure_occurred_at=$6,delivery_failure_attempt=$7,delivery_failure_blocked_recipients=$8,send_claimed_at=NULL WHERE id=$1 AND direction='outbound' AND send_job_id=$2 AND delivery_status IN ('accepted','sending')`, messageID, jobID, nullIfEmpty(detail), string(source), string(reason), occurredAt.UTC(), attempt, storedRecipients)
	return err
}

// MarkOutboundSentTx records, within the caller's transaction, that a claimed
// outbound message was submitted to the provider: delivery_status='sent',
// provider_message_id, and one message_recipients row per recipient at 'sent'
// (mirrors MarkMessageSent's recipient shape). sent_as is left as the accept-time
// value. Returns the row + owning user/domain for the caller to emit email.sent +
// meter usage, or nil if the row is gone or no longer in `sending`. Trashed rows
// remain eligible because trash may commit after the durable send claim.
func (s *Store) MarkOutboundSentTx(ctx context.Context, tx pgx.Tx, messageID, providerMessageID string) (*OutboundSentInfo, error) {
	m := &Message{ID: messageID, Direction: "outbound", DeliveryStatus: "sent", ProviderMessageID: providerMessageID}
	err := tx.QueryRow(ctx,
		`UPDATE messages m
		    SET delivery_status = 'sent', provider_message_id = $2, send_claimed_at = NULL,
		        delivery_failure_source=NULL,delivery_failure_reason_code=NULL,delivery_detail=NULL,
		        delivery_failure_occurred_at=NULL,delivery_failure_attempt=NULL,delivery_failure_blocked_recipients=NULL
		   FROM agent_identities a
		  WHERE m.id = $1 AND m.direction = 'outbound'
		    AND m.agent_id = a.id
		    AND m.delivery_status = 'sending'
		 RETURNING m.agent_id, m.subject, m.message_type, m.method, m.conversation_id, m.sender, m.to_recipients, m.cc, m.bcc`,
		messageID, providerMessageID,
	).Scan(&m.AgentID, &m.Subject, &m.Type, &m.Method, &m.ConversationID, &m.Sender, &m.ToRecipients, &m.CC, &m.BCC)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := addSentRecipientRowsTx(ctx, tx, messageID, m.ToRecipients, m.CC, m.BCC); err != nil {
		return nil, err
	}
	return outboundSentInfoTx(ctx, tx, m)
}

// ResolveOutboundProviderAcceptedTx settles, within the caller's transaction,
// a pre-terminal outbound row for which authoritative provider-accept evidence
// has been recorded (provider_accepted_at, written by the SNS consumer):
// delivery_status='sent' with the evidence-repaired provider id + the standard
// per-recipient 'sent' rows. This is the terminal-failure guard's positive
// branch (async-send-contract §3.1 / pipeline §7): a final attempt or the
// terminal reconciler about to declare `failed` first settles the row as sent
// when the provider demonstrably has the message. Returns (nil, "", nil) when
// the row is gone, already sent/terminal, or carries no evidence — the caller
// proceeds to the failure write, whose own CAS re-checks the evidence.
func (s *Store) ResolveOutboundProviderAcceptedTx(ctx context.Context, tx pgx.Tx, messageID string) (*OutboundSentInfo, string, error) {
	var providerMessageID string
	var providerAcceptedAt time.Time
	var (
		jobID             *int64
		failureSource     string
		failureReason     string
		failureOccurredAt *time.Time
		failureAttempt    *int
		blockedRecipients []string
	)
	err := tx.QueryRow(ctx,
		`SELECT send_job_id,COALESCE(delivery_failure_source,''),COALESCE(delivery_failure_reason_code,''),
		        delivery_failure_occurred_at,delivery_failure_attempt,delivery_failure_blocked_recipients
		   FROM messages
		  WHERE id=$1 AND direction='outbound'
		    AND (delivery_status IN ('accepted','sending') OR
		         (delivery_status='failed' AND COALESCE(delivery_failure_source,'local')='local'))
		    AND provider_accepted_at IS NOT NULL
		  FOR UPDATE`, messageID,
	).Scan(&jobID, &failureSource, &failureReason, &failureOccurredAt, &failureAttempt, &blockedRecipients)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if jobID != nil && isCompleteTerminalFallback(failureSource, failureReason, failureOccurredAt, failureAttempt) {
		seen := make(map[string]struct{}, len(blockedRecipients))
		for _, raw := range blockedRecipients {
			if recipient := NormalizeEmail(raw); recipient != "" {
				seen[recipient] = struct{}{}
			}
		}
		blockedRecipients = blockedRecipients[:0]
		for recipient := range seen {
			blockedRecipients = append(blockedRecipients, recipient)
		}
		sort.Strings(blockedRecipients)
		for _, recipient := range blockedRecipients {
			if _, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{MessageID: messageID, DedupeKey: messagelifecycle.SendSuppressionDedupeKey(*jobID, *failureAttempt, recipient), Direction: "outbound", Recipient: recipient, ReasonCode: messagelifecycle.ReasonSuppressionRecipientBlocked, CorrelationIDs: messagelifecycle.SafeCorrelationIDs(map[string]string{"job_id": strconv.FormatInt(*jobID, 10)}), OccurredAt: failureOccurredAt.UTC()}); err != nil {
				return nil, "", err
			}
		}
	}
	m := &Message{ID: messageID, Direction: "outbound", DeliveryStatus: "sent"}
	err = tx.QueryRow(ctx,
		`UPDATE messages m
		    SET delivery_status = 'sent', send_claimed_at = NULL, delivery_failure_source = NULL, delivery_failure_reason_code = NULL, delivery_detail = NULL,
		        delivery_failure_occurred_at=NULL, delivery_failure_attempt=NULL, delivery_failure_blocked_recipients=NULL
		   FROM agent_identities a
		  WHERE m.id = $1 AND m.direction = 'outbound'
		    AND m.agent_id = a.id
		    AND (m.delivery_status IN ('accepted','sending') OR
		         (m.delivery_status='failed' AND COALESCE(m.delivery_failure_source,'local')='local'))
		    AND m.provider_accepted_at IS NOT NULL
		 RETURNING m.agent_id, m.subject, m.message_type, m.method, m.conversation_id, m.sender,
		           m.to_recipients, m.cc, m.bcc, COALESCE(m.provider_message_id, ''),m.provider_accepted_at`,
		messageID,
	).Scan(&m.AgentID, &m.Subject, &m.Type, &m.Method, &m.ConversationID, &m.Sender,
		&m.ToRecipients, &m.CC, &m.BCC, &providerMessageID, &providerAcceptedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	m.ProviderMessageID = providerMessageID
	if err := addSentRecipientRowsTx(ctx, tx, messageID, m.ToRecipients, m.CC, m.BCC); err != nil {
		return nil, "", err
	}
	info, err := outboundSentInfoTx(ctx, tx, m)
	if info != nil {
		info.ProviderAcceptedAt = providerAcceptedAt
	}
	return info, providerMessageID, err
}

// addSentRecipientRowsTx inserts the per-recipient 'sent' rows for a message
// just settled as sent (idempotent on re-drive). Shared by MarkOutboundSentTx
// and ResolveOutboundProviderAcceptedTx.
func addSentRecipientRowsTx(ctx context.Context, tx pgx.Tx, messageID string, to, cc, bcc []string) error {
	add := func(addrs []string, kind string) error {
		for _, a := range addrs {
			addr := NormalizeEmail(a)
			if addr == "" {
				continue
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO message_recipients (id, message_id, address, kind, status)
				 VALUES ($1, $2, $3, $4, 'sent')
				 ON CONFLICT (message_id, address) DO NOTHING`,
				"rcpt_"+generateID(), messageID, addr, kind,
			); err != nil {
				return err
			}
		}
		return nil
	}
	if err := add(to, "to"); err != nil {
		return err
	}
	if err := add(cc, "cc"); err != nil {
		return err
	}
	return add(bcc, "bcc")
}

// outboundSentInfoTx resolves the owning user/domain for a terminal outbound
// bookkeeping write's event emission + metering.
func outboundSentInfoTx(ctx context.Context, tx pgx.Tx, m *Message) (*OutboundSentInfo, error) {
	info := &OutboundSentInfo{Message: m}
	if err := tx.QueryRow(ctx,
		`SELECT user_id, registered_domain FROM agent_identities WHERE id = $1`, m.AgentID,
	).Scan(&info.UserID, &info.Domain); err != nil {
		return nil, err
	}
	return info, nil
}

// MarkOutboundFailedTx records, within the caller's transaction, a terminal
// outbound send failure: delivery_status='failed' + delivery_detail + the
// failure provenance (delivery_failure_source — 'provider' for an explicit
// provider rejection, 'local' for an inferred failure; §3.1 correction only
// ever applies to 'local'). Returns the row + owning user for the caller to
// emit email.failed, or nil if the row is gone, already left accepted/sending,
// or carries provider-accept evidence — the CAS's `provider_accepted_at IS
// NULL` arm is the terminal-failure guard's last line: a row the provider
// demonstrably accepted can never be declared failed, no matter which caller
// races in (the caller settles it via ResolveOutboundProviderAcceptedTx
// instead). A stored delivery_detail (a deferred final attempt's diagnostic)
// is preferred over the caller's generic detail. A post-accept trash does not
// suppress terminal bookkeeping on the hidden row.
func (s *Store) MarkOutboundFailedTx(ctx context.Context, tx pgx.Tx, messageID, detail string, source delivery.FailureSource) (*OutboundSentInfo, error) {
	m := &Message{ID: messageID, Direction: "outbound", DeliveryStatus: "failed", DeliveryDetail: detail}
	err := tx.QueryRow(ctx,
		`UPDATE messages m
		    SET delivery_status = 'failed',
		        delivery_detail = COALESCE(NULLIF(m.delivery_detail, ''), $2),
		        delivery_failure_source = $3,
		        send_claimed_at = NULL
		   FROM agent_identities a
		  WHERE m.id = $1 AND m.direction = 'outbound'
		    AND m.agent_id = a.id
		    AND m.delivery_status IN ('accepted', 'sending')
		    AND m.provider_accepted_at IS NULL
		 RETURNING m.agent_id, m.subject, m.message_type, m.method, m.conversation_id, m.sender, m.to_recipients, m.cc, m.bcc,
		           COALESCE(m.delivery_detail, '')`,
		messageID, nullIfEmpty(detail), string(source),
	).Scan(&m.AgentID, &m.Subject, &m.Type, &m.Method, &m.ConversationID, &m.Sender, &m.ToRecipients, &m.CC, &m.BCC,
		&m.DeliveryDetail)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return outboundSentInfoTx(ctx, tx, m)
}

// --- Suppression list ---

// Suppression is one (user, address) entry on the per-tenant suppression list.
type Suppression struct {
	Address         string    `json:"address"`
	Reason          string    `json:"reason,omitempty"`
	Source          string    `json:"source"` // bounce | complaint | manual
	SourceMessageID string    `json:"source_message_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// AddSuppression idempotently inserts a (user, address) suppression. added is
// false when it already existed, so the caller fires domain.suppression_added
// at most once per address.
func (s *Store) AddSuppression(ctx context.Context, userID, address, reason, source, sourceMessageID string) (bool, error) {
	var added bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		_, added, err = s.AddSuppressionTx(ctx, tx, userID, address, reason, source, sourceMessageID)
		return err
	})
	return added, err
}

func (s *Store) AddSuppressionTx(ctx context.Context, tx pgx.Tx, userID, address, reason, source, sourceMessageID string) (string, bool, error) {
	id := "supp_" + generateID()
	tag, err := tx.Exec(ctx,
		`INSERT INTO suppressions (id, user_id, address, reason, source, source_message_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (user_id, address) DO NOTHING`,
		id, userID, NormalizeEmail(address), reason, source, nullIfEmpty(sourceMessageID),
	)
	if err != nil {
		return "", false, err
	}
	if tag.RowsAffected() == 0 {
		if err := tx.QueryRow(ctx, `SELECT id FROM suppressions WHERE user_id=$1 AND address=$2`, userID, NormalizeEmail(address)).Scan(&id); err != nil {
			return "", false, err
		}
		return id, false, nil
	}
	return id, true, nil
}

// SuppressedAddresses returns the subset of addrs that are suppressed for the
// user — the send-time enforcement read. Empty input → empty result.
func (s *Store) SuppressedAddresses(ctx context.Context, userID string, addrs []string) ([]string, error) {
	if len(addrs) == 0 {
		return nil, nil
	}
	norm := make([]string, 0, len(addrs))
	for _, a := range addrs {
		norm = append(norm, NormalizeEmail(a))
	}
	rows, err := s.pool.Query(ctx,
		`SELECT address FROM suppressions WHERE user_id = $1 AND address = ANY($2)`,
		userID, norm,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListSuppressions returns the user's suppression list, newest first.
// ListSuppressions returns one page of the user's suppressed addresses,
// newest first, keyset-paginated on (created_at, address). The caller passes
// limit (fetch limit+1 to detect a further page) and the after-key from the
// previous page's last row (zero afterCreatedAt = first page). (A-5: the
// suppression list auto-grows on every bounce/complaint, so it needs real
// pagination, not a single page.)
func (s *Store) ListSuppressions(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterAddress string) ([]Suppression, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT address, reason, source, COALESCE(source_message_id, ''), created_at
	        FROM suppressions WHERE user_id = $1`
	args := []interface{}{userID}
	if !afterCreatedAt.IsZero() {
		i := len(args) + 1
		q += fmt.Sprintf(` AND (created_at < $%d OR (created_at = $%d AND address < $%d))`, i, i, i+1)
		args = append(args, afterCreatedAt, afterAddress)
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC, address DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suppression
	for rows.Next() {
		var sp Suppression
		if err := rows.Scan(&sp.Address, &sp.Reason, &sp.Source, &sp.SourceMessageID, &sp.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// RemoveSuppression deletes a (user, address) suppression. found=false when no
// such entry existed.
func (s *Store) RemoveSuppression(ctx context.Context, userID, address string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM suppressions WHERE user_id = $1 AND address = $2`,
		userID, NormalizeEmail(address),
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
