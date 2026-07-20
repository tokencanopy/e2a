package agent

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/sendramp"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// --- Async outbound send adapters (async-message-pipeline.md, slice C) ---
//
// These bridge internal/outboundsend's Store/Deliverer interfaces onto the
// concrete identity.Store + webhookpub.Outbox + outbound.Sender. They live in the
// agent package (not outboundsend) because agent already owns the store, outbox,
// usage tracker, and sender — and agent may import outboundsend, never the reverse.

// AcceptIdemCompleter completes the idempotency key inside the delivery tx with
// the exact result the wire will render. External sends cache 202/accepted;
// terminal local loopback caches 200/sent. nil when the request carries no
// Idempotency-Key (or no idempotency store is wired).
type AcceptIdemCompleter func(ctx context.Context, tx pgx.Tx, result *OutboundResult) error

// ApproveIdemCompleter completes an approval's idempotency key inside the
// async approve-and-enqueue transaction. It receives the resolved message so
// the httpapi layer can cache the exact SendResultView (including edited,
// method, and sent_as) that the live request returns.
type ApproveIdemCompleter func(ctx context.Context, tx pgx.Tx, approved *identity.Message) error

// OutboundEnqueuer is the accept-tx's handle on the shared River client — it
// inserts the outbound_send job in the same transaction as the message row.
// *outboundsend.Jobs satisfies it. Main always injects it; a nil value fails
// closed before provider I/O.
type OutboundEnqueuer interface {
	EnqueueSendTx(ctx context.Context, tx pgx.Tx, messageID string) (int64, error)
	// EnqueueScheduledSendTx enqueues the send job to run no earlier than `at`
	// (scheduled send). Same outbox transaction as EnqueueSendTx; only the job's
	// first-run time differs. *outboundsend.Jobs satisfies it.
	EnqueueScheduledSendTx(ctx context.Context, tx pgx.Tx, messageID string, at time.Time) (int64, error)
	// EnqueueBatchTx enqueues N outbound_send jobs in one round-trip
	// within the caller's tx. Returns job ids positionally aligned with
	// messageIDs so the accept-tx can stamp messages.send_job_id per row.
	// Used by DeliverBatch; single-send stays on EnqueueSendTx.
	EnqueueBatchTx(ctx context.Context, tx pgx.Tx, messageIDs []string) ([]int64, error)
}

// outboundSendStore implements outboundsend.Store over identity.Store +
// webhookpub.Outbox + the usage tracker. ClaimSend persists the short-lived
// sending state before provider I/O; MarkSent/MarkFailed then record one monotonic
// terminal outcome, lifecycle row, usage meter, and event in one fresh
// transaction (the message only becomes billable once submitted).
type outboundSendStore struct {
	store  *identity.Store
	outbox webhookpub.Outbox
	usage  usage.UsageTracker
	// overQuota, when set, is consulted at FIRE time for SCHEDULED sends only:
	// it reports whether the owning account is over its monthly message cap in
	// the fire month. A send scheduled into a later month was never counted
	// against that month's cap (accept-time enforcement can't know the fire
	// month), so this closes the "schedule into a future month to bypass quota"
	// gap. nil disables the gate (tests / plans without limits). The wrapper
	// returns (false, err) on a transient lookup failure so a quota-check glitch
	// never drops a legitimate send (fail open).
	overQuota func(ctx context.Context, userID string) (bool, error)
}

// SetScheduledSendQuota installs the fire-time monthly-cap gate for scheduled
// sends (see outboundSendStore.overQuota). Wired in main from the limits
// enforcer; left nil in tests and plan-agnostic setups.
func (a *outboundSendStore) SetScheduledSendQuota(f func(ctx context.Context, userID string) (bool, error)) {
	a.overQuota = f
}

// NewOutboundSendStore builds the outboundsend.Store adapter for main.go.
func NewOutboundSendStore(store *identity.Store, outbox webhookpub.Outbox, usageTracker usage.UsageTracker) *outboundSendStore {
	return &outboundSendStore{store: store, outbox: outbox, usage: usageTracker}
}

type outboundRampGate struct {
	store    *sendramp.Store
	schedule sendramp.Schedule
	enabled  bool
	now      func() time.Time
}

// NewOutboundRampGate adapts the durable sendramp store to the worker-owned
// gate contract. The schedule is snapshotted by Store on the first eligible
// send; config changes therefore affect only domains that have not armed yet.
func NewOutboundRampGate(store *sendramp.Store, schedule sendramp.Schedule, enabled bool, clocks ...func() time.Time) outboundsend.RampGate {
	now := time.Now
	if len(clocks) > 0 && clocks[0] != nil {
		now = clocks[0]
	}
	return &outboundRampGate{store: store, schedule: schedule, enabled: enabled, now: now}
}

func (g *outboundRampGate) Reserve(ctx context.Context, req outboundsend.RampRequest) (outboundsend.RampDecision, error) {
	if !g.enabled {
		if err := g.store.Exempt(ctx, req.UserID, req.Domain); err != nil {
			return outboundsend.RampDecision{}, err
		}
		return outboundsend.RampDecision{Allowed: true}, nil
	}
	d, err := g.store.Reserve(ctx, sendramp.ReserveRequest{
		MessageID: req.MessageID,
		UserID:    req.UserID,
		Domain:    req.Domain,
		Units:     req.Units,
		Day:       g.now().UTC(),
		Schedule:  g.schedule,
	})
	return outboundsend.RampDecision{Allowed: d.Allowed, RetryAt: d.RetryAt}, err
}

func (g *outboundRampGate) Confirm(ctx context.Context, messageID string) error {
	return g.store.Confirm(ctx, messageID)
}

func (g *outboundRampGate) Release(ctx context.Context, messageID string) error {
	return g.store.Release(ctx, messageID)
}

func (g *outboundRampGate) Resolve(ctx context.Context, messageID string) error {
	return g.store.Resolve(ctx, messageID)
}

func (a *outboundSendStore) ClaimSend(ctx context.Context, messageID string, jobID int64) (*outboundsend.SendJob, error) {
	if a.usage == nil {
		return nil, fmt.Errorf("outbound usage tracker is required")
	}
	if _, ok := a.usage.(usage.TransactionalUsageTracker); !ok {
		return nil, fmt.Errorf("outbound usage tracker lacks transaction support")
	}
	p, err := a.store.ClaimOutboundForSend(ctx, messageID, jobID)
	if err != nil || p == nil {
		return nil, err
	}
	// Fire-time monthly-cap gate for scheduled sends: a send scheduled into a
	// future month was never counted against that month's cap, so enforce it now
	// and refuse terminally (email.failed via MarkFailed) rather than deliver an
	// uncapped send. Immediate sends are already gated at accept and skip this.
	if p.ScheduledAt != nil && a.overQuota != nil {
		over, qerr := a.overQuota(ctx, p.UserID)
		if qerr != nil {
			log.Printf("[outbound-send:%s] scheduled-send quota check failed, proceeding (fail open): %v", p.ID, qerr)
		} else if over {
			if _, _, failErr := a.MarkFailed(ctx, p.ID, jobID, 0, time.Now().UTC(),
				"scheduled send canceled: monthly message limit exceeded at send time",
				delivery.FailureSourceLocal, messagelifecycle.ReasonSubmissionCancelled, nil); failErr != nil {
				return nil, failErr
			}
			return nil, nil // refused at fire — the worker delivers nothing
		}
	}
	sj := &outboundsend.SendJob{
		MessageID:          p.ID,
		UserID:             p.UserID,
		AgentID:            p.AgentID,
		Domain:             p.Domain,
		MessageType:        p.MessageType,
		Status:             p.DeliveryStatus,
		EnvelopeFrom:       p.EnvelopeFrom,
		Recipients:         p.Recipients,
		RawMessage:         p.Raw,
		SentAs:             p.SentAs,
		AcceptedAt:         p.CreatedAt,
		ProviderAccepted:   p.ProviderAccepted,
		ProviderAcceptedAt: p.ProviderAcceptedAt,
		ProviderMessageID:  p.ProviderMessageID,
	}
	if p.ScheduledAt != nil {
		sj.ScheduledAt = *p.ScheduledAt
	}
	if p.ReviewedAt != nil {
		sj.ReviewedAt = *p.ReviewedAt
	}
	return sj, nil
}

// SuppressedRecipients backs the SendWorker's pre-provider suppression guard:
// the effective account-wide + exact-agent subset (the store normalizes both
// sides).
func (a *outboundSendStore) SuppressedRecipients(ctx context.Context, userID, agentID string, recipients []string) ([]string, error) {
	return a.store.EffectiveSuppressions(ctx, userID, agentID, recipients)
}

func (a *outboundSendStore) ReleaseSend(ctx context.Context, messageID string, jobID int64) error {
	return a.store.ReleaseOutboundSendClaim(ctx, messageID, jobID)
}

func (a *outboundSendStore) MarkSent(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, providerMessageID, sentAs string) error {
	if err := a.store.WithTx(ctx, func(tx pgx.Tx) error {
		info, err := a.store.MarkOutboundSentTx(ctx, tx, messageID, providerMessageID)
		if err != nil {
			return err
		}
		if info == nil {
			return nil
		}
		return a.finalizeSentTx(ctx, tx, info, jobID, attempt, occurredAt, providerMessageID)
	}); err != nil {
		return err
	}
	return nil
}

// FinalizeProviderAcceptedTx is the delivery-feedback crash-window bridge. It
// reuses the canonical sent finalizer inside the caller's transaction so the
// signed provider observation, email.sent, usage meter, preserved suppression
// facts, and following feedback outcome commit or roll back together.
func (a *outboundSendStore) FinalizeProviderAcceptedTx(ctx context.Context, tx pgx.Tx, messageID string) error {
	jobID, err := a.store.OutboundSendJobIDTx(ctx, tx, messageID)
	if err != nil {
		return err
	}
	info, providerID, err := a.store.ResolveOutboundProviderAcceptedTx(ctx, tx, messageID)
	if err != nil || info == nil {
		return err
	}
	return a.finalizeSentTx(ctx, tx, info, jobID, 0, info.ProviderAcceptedAt, providerID)
}

func (a *outboundSendStore) finalizeSentTx(ctx context.Context, tx pgx.Tx, info *identity.OutboundSentInfo, jobID int64, attempt int, occurredAt time.Time, providerMessageID string) error {
	if err := a.meterSentTx(ctx, tx, info); err != nil {
		return err
	}
	// A terminally accepted send and the timestamps used by the safe follow-up
	// query are one invariant. Keep them in this transaction so a crash cannot
	// leave a recently contacted person eligible for another send.
	for _, rcpt := range info.Message.ToRecipients {
		if rcpt == "" {
			continue
		}
		if _, err := a.store.RecordOutboundActivityTx(ctx, tx, info.UserID,
			info.Message.AgentID, rcpt, info.Message.ConversationID, occurredAt); err != nil {
			return err
		}
	}
	transition, err := appendSubmissionTransition(ctx, tx, info.Message.ID, jobID, attempt, occurredAt,
		messagelifecycle.ReasonSubmissionUpstreamAccepted, "", providerMessageID)
	if err != nil {
		return err
	}
	e := buildEmailSentEventFromRow(info, providerMessageID, transition)
	e.ID = webhookpub.DeterministicEventID(info.Message.ID, webhookpub.EventEmailSent)
	return a.outbox.PublishTx(ctx, tx, e)
}

// meterSentTx makes async terminal metering part of the same commit as state,
// lifecycle, and event publication. The accept-time cap pre-check remains the
// quota gate; this write is accounting, but an accounting failure must roll the
// terminal transaction back so the reconciler can converge without undercount.
func (a *outboundSendStore) meterSentTx(ctx context.Context, tx pgx.Tx, info *identity.OutboundSentInfo) error {
	if a.usage == nil {
		return nil
	}
	tracker, ok := a.usage.(usage.TransactionalUsageTracker)
	if !ok {
		return fmt.Errorf("outbound usage tracker lacks transaction support")
	}
	_, err := tracker.RecordAndCheckTx(ctx, tx, info.UserID, info.Message.AgentID, info.Domain, "outbound")
	return err
}

// FinalizeScheduledCancellationTx performs the canonical guarded terminal
// transition for a scheduled message restored after its cutoff. Authoritative
// provider-accept evidence wins and is settled as sent; otherwise the
// cancellation becomes failed with a deterministic email.failed event.
func (a *outboundSendStore) FinalizeScheduledCancellationTx(
	ctx context.Context,
	tx pgx.Tx,
	messageID string,
	jobID int64,
	occurredAt time.Time,
) error {
	info, providerID, err := a.store.ResolveOutboundProviderAcceptedTx(ctx, tx, messageID)
	if err != nil {
		return err
	}
	if info != nil {
		return a.finalizeSentTx(ctx, tx, info, jobID, 0, info.ProviderAcceptedAt, providerID)
	}

	const detail = "scheduled send canceled because it was restored after scheduled_at"
	finfo, err := a.store.MarkOutboundFailedTx(ctx, tx, messageID, detail, delivery.FailureSourceLocal)
	if err != nil {
		return err
	}
	if finfo == nil {
		// Provider evidence raced the guarded update, or another terminal
		// transition already won. The reconciler will settle evidence-bearing
		// rows; never overwrite them with a false failure.
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE messages SET delivery_failure_reason_code=$2 WHERE id=$1`,
		messageID, string(messagelifecycle.ReasonSubmissionCancelled)); err != nil {
		return err
	}
	transition, err := appendSubmissionTransition(
		ctx, tx, messageID, jobID, 0, occurredAt,
		messagelifecycle.ReasonSubmissionCancelled, finfo.Message.DeliveryDetail, "",
	)
	if err != nil {
		return err
	}
	e := buildEmailFailedEventFromRow(finfo, finfo.Message.DeliveryDetail, transition)
	e.ID = webhookpub.DeterministicEventID(messageID, webhookpub.EventEmailFailed)
	return a.outbox.PublishTx(ctx, tx, e)
}

// MarkFailed is the guarded terminal write (async-send-contract §3.1): inside
// one transaction it first checks for provider-accept evidence — an SNS
// notification that proved the provider accepted this message's submission —
// and, when present, settles the row as SENT (+ email.sent, + metering) instead
// of declaring a false terminal failure. Only an evidence-free row is marked
// failed (+ email.failed) with the caller's failure provenance. Both events use
// deterministic ids, so duplicate finalizations stay idempotent, and
// MarkOutboundFailedTx's own `provider_accepted_at IS NULL` CAS closes the race
// where evidence lands between the two statements (the row is then left for the
// reconciler's next pass to settle as sent).
// The returned delivery.Status tells the caller what actually happened so
// metrics/logging report truth, not intent: StatusSent (evidence settle),
// StatusFailed (failure recorded), or "" (no-op — row gone, already
// terminal, or evidence raced the CAS; nothing was written). The returned
// time is the occurred_at the write actually used: the provider-accept
// evidence time on an evidence settle, the caller's occurredAt on a failure,
// zero on a no-op.
func (a *outboundSendStore) MarkFailed(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, detail string, source delivery.FailureSource, reason messagelifecycle.ReasonCode, blockedRecipients []string) (delivery.Status, time.Time, error) {
	detail = messagelifecycle.SafeDiagnostic(detail)
	blockedRecipients = normalizeBlockedRecipients(blockedRecipients)
	var settled delivery.Status
	var settledAt time.Time
	var resolved *identity.OutboundSentInfo
	var resolvedProviderID string
	if err := a.store.WithTx(ctx, func(tx pgx.Tx) error {
		info, providerID, err := a.store.ResolveOutboundProviderAcceptedTx(ctx, tx, messageID)
		if err != nil {
			return err
		}
		if info != nil {
			resolved, resolvedProviderID = info, providerID
			attempt = 0
			occurredAt = info.ProviderAcceptedAt
			settled = delivery.StatusSent
			settledAt = occurredAt
			return a.finalizeSentTx(ctx, tx, info, jobID, attempt, occurredAt, providerID)
		}
		finfo, err := a.store.MarkOutboundFailedTx(ctx, tx, messageID, detail, source)
		if err != nil {
			return err
		}
		if finfo == nil {
			settled = "" // row gone, already terminal, or evidence raced in — nothing to record
			return nil
		}
		settled = delivery.StatusFailed
		settledAt = occurredAt
		if _, err := tx.Exec(ctx, `UPDATE messages SET delivery_failure_reason_code=$2 WHERE id=$1`, messageID, string(reason)); err != nil {
			return err
		}
		transition, err := appendSubmissionTransition(ctx, tx, messageID, jobID, attempt, occurredAt, reason, finfo.Message.DeliveryDetail, "")
		if err != nil {
			return err
		}
		for _, recipient := range blockedRecipients {
			if _, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{MessageID: messageID, DedupeKey: messagelifecycle.SendSuppressionDedupeKey(jobID, attempt, recipient), Direction: "outbound", Recipient: recipient, ReasonCode: messagelifecycle.ReasonSuppressionRecipientBlocked, CorrelationIDs: messagelifecycle.SafeCorrelationIDs(map[string]string{"job_id": strconv.FormatInt(jobID, 10)}), OccurredAt: occurredAt}); err != nil {
				return err
			}
		}
		// Emit with the detail actually stored on the row (a deferred final
		// attempt's diagnostic is preferred over a generic sweep detail).
		e := buildEmailFailedEventFromRow(finfo, finfo.Message.DeliveryDetail, transition)
		e.ID = webhookpub.DeterministicEventID(messageID, webhookpub.EventEmailFailed)
		return a.outbox.PublishTx(ctx, tx, e)
	}); err != nil {
		return "", time.Time{}, err
	}
	if resolved != nil {
		log.Printf("[outbound-send] %s: terminal-failure guard settled as sent on provider evidence (provider id %q)", messageID, resolvedProviderID)
	}
	return settled, settledAt, nil
}

func (a *outboundSendStore) PreserveTerminalFailure(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, detail string, source delivery.FailureSource, reason messagelifecycle.ReasonCode, blockedRecipients []string) error {
	return a.store.PreserveOutboundTerminalFailure(ctx, messageID, jobID, attempt, occurredAt, messagelifecycle.SafeDiagnostic(detail), source, reason, normalizeBlockedRecipients(blockedRecipients))
}

func normalizeBlockedRecipients(recipients []string) []string {
	seen := make(map[string]struct{}, len(recipients))
	for _, raw := range recipients {
		if recipient := identity.NormalizeEmail(raw); recipient != "" {
			seen[recipient] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for recipient := range seen {
		result = append(result, recipient)
	}
	sort.Strings(result)
	return result
}

func (a *outboundSendStore) DeferTerminalFailure(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, detail string) error {
	detail = messagelifecycle.SafeDiagnostic(detail)
	return a.store.WithTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE messages SET delivery_detail=$3, send_claimed_at=NULL WHERE id=$1 AND direction='outbound' AND send_job_id=$2 AND delivery_status IN ('accepted','sending')`, messageID, jobID, nullableLifecycleDetail(detail))
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = appendTemporaryFirstTx(ctx, tx, messageID, jobID, attempt, occurredAt, detail)
		return err
	})
}

func (a *outboundSendStore) RecordTemporaryFailure(ctx context.Context, messageID string, jobID int64, attempt int, occurredAt time.Time, detail string) error {
	detail = messagelifecycle.SafeDiagnostic(detail)
	return a.store.WithTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE messages SET delivery_status='accepted', send_claimed_at=NULL WHERE id=$1 AND send_job_id=$2 AND delivery_status='sending'`, messageID, jobID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = appendTemporaryFirstTx(ctx, tx, messageID, jobID, attempt, occurredAt, detail)
		return err
	})
}

func appendTemporaryFirstTx(ctx context.Context, tx pgx.Tx, messageID string, jobID int64, attempt int, occurredAt time.Time, detail string) (messagelifecycle.MessageLifecycleTransition, error) {
	key := outboundsend.SubmissionDedupeKey(jobID, attempt, messagelifecycle.ReasonSubmissionTemporaryFailure)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM message_lifecycle_transitions WHERE message_id=$1 AND dedupe_key=$2)`, messageID, key).Scan(&exists); err != nil {
		return messagelifecycle.MessageLifecycleTransition{}, err
	}
	if exists {
		return messagelifecycle.MessageLifecycleTransition{}, nil
	}
	return appendSubmissionTransition(ctx, tx, messageID, jobID, attempt, occurredAt, messagelifecycle.ReasonSubmissionTemporaryFailure, detail, "")
}

func appendSubmissionTransition(ctx context.Context, tx pgx.Tx, messageID string, jobID int64, attempt int, occurredAt time.Time, reason messagelifecycle.ReasonCode, detail, providerID string) (messagelifecycle.MessageLifecycleTransition, error) {
	evidence := map[string]any{}
	if safe := messagelifecycle.SafeDiagnostic(detail); safe != "" {
		evidence["failure_reason"] = safe
	}
	if reason != messagelifecycle.ReasonSubmissionUpstreamAccepted {
		evidence["failure_code"] = string(reason)
	}
	return messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{
		MessageID: messageID, DedupeKey: outboundsend.SubmissionDedupeKey(jobID, attempt, reason), Direction: "outbound", ReasonCode: reason,
		Evidence: evidence, CorrelationIDs: messagelifecycle.SafeCorrelationIDs(map[string]string{"job_id": strconv.FormatInt(jobID, 10), "provider_message_id": providerID}), OccurredAt: occurredAt,
	})
}

func nullableLifecycleDetail(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// buildEmailSentEventFromRow reconstructs the email.sent event from a stored row
// (the async worker has no live SendRequest). Emits the SAME canonical
// eventpayload.EmailSentData struct as the synchronous buildSentEvent, so
// subscribers see an identical envelope on both paths (golden-fixture-locked).
func buildEmailSentEventFromRow(info *identity.OutboundSentInfo, providerMessageID string, transitions ...messagelifecycle.MessageLifecycleTransition) webhookpub.Event {
	m := info.Message
	data := eventpayload.EmailSentData{
		MessageID:            m.ID,
		AgentEmail:           m.Sender,
		Direction:            "outbound",
		ConversationID:       m.ConversationID,
		ProviderMessageID:    providerMessageID,
		Method:               m.Method,
		From:                 m.Sender,
		To:                   orEmpty(m.ToRecipients),
		CC:                   m.CC,
		BCC:                  m.BCC,
		Subject:              m.Subject,
		MessageType:          m.Type,
		LifecycleTransitions: transitions,
	}
	return webhookpub.Event{
		Type:           webhookpub.EventEmailSent,
		CreatedAt:      time.Now().UTC(),
		UserID:         info.UserID,
		AgentID:        m.AgentID,
		ConversationID: m.ConversationID,
		MessageID:      m.ID,
		Data:           data,
	}
}

// buildEmailFailedEventFromRow builds the email.failed event for a terminal
// outbound send failure.
func buildEmailFailedEventFromRow(info *identity.OutboundSentInfo, detail string, transitions ...messagelifecycle.MessageLifecycleTransition) webhookpub.Event {
	m := info.Message
	data := eventpayload.EmailFailedData{
		MessageID:            m.ID,
		AgentEmail:           m.Sender,
		Direction:            "outbound",
		ConversationID:       m.ConversationID,
		Method:               m.Method,
		From:                 m.Sender,
		To:                   orEmpty(m.ToRecipients),
		CC:                   m.CC,
		BCC:                  m.BCC,
		Subject:              m.Subject,
		MessageType:          m.Type,
		Reason:               detail,
		LifecycleTransitions: transitions,
	}
	if len(transitions) > 0 {
		data.ReasonCode = string(transitions[0].ReasonCode)
		retryable := transitions[0].Retryable
		data.Retryable = &retryable
	}
	return webhookpub.Event{
		Type:           webhookpub.EventEmailFailed,
		CreatedAt:      time.Now().UTC(),
		UserID:         info.UserID,
		AgentID:        m.AgentID,
		ConversationID: m.ConversationID,
		MessageID:      m.ID,
		Data:           data,
	}
}

// outboundDeliverer implements outboundsend.Deliverer over Sender.SubmitOnce — a
// single SMTP submit of the persisted Sent-folder bytes (River owns retries).
type outboundDeliverer struct {
	sender *outbound.Sender
}

// NewOutboundDeliverer builds the outboundsend.Deliverer adapter for main.go.
func NewOutboundDeliverer(sender *outbound.Sender) outboundsend.Deliverer {
	return &outboundDeliverer{sender: sender}
}

func (d *outboundDeliverer) Deliver(ctx context.Context, j *outboundsend.SendJob) outboundsend.DeliverOutcome {
	providerID, err := d.sender.SubmitOnceContext(ctx, j.MessageID, j.EnvelopeFrom, j.Recipients, j.RawMessage)
	if err != nil {
		// Classify (design §8): a definitely-permanent 5xx is terminal (JobCancel);
		// a provider-connection failure (relay unreachable/misconfigured) is an
		// outage → snooze without burning an attempt; everything else (4xx/unknown)
		// takes the bounded retry. Terminal-failing a send that could still succeed
		// would violate at-least-once.
		return outboundsend.DeliverOutcome{
			Err:       err,
			Permanent: outbound.IsPermanentSMTPError(err),
			Outage:    outbound.IsConnectionError(err),
		}
	}
	return outboundsend.DeliverOutcome{ProviderMessageID: providerID, SentAs: j.SentAs}
}
