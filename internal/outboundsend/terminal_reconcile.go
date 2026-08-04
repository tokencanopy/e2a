package outboundsend

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
)

const terminalReconcileInterval = time.Minute

// providerEvidenceGrace is how long after its send job reached a terminal
// state (river_job.finalized_at) an accepted/sending row without recorded
// provider-accept evidence is left alone before being declared failed. A
// final attempt that crashed AFTER the SMTP accept never captured a provider
// id, and its SES Send/Delivery notification (carrying the X-E2A-Message-ID
// echo) typically lands within seconds-to-minutes — the grace gives that
// evidence time to arrive so the reconciler settles the row as sent instead
// of firing a false, hard-to-correct email.failed. Rows WITH evidence are
// settled immediately; a pruned/missing job is by definition long past any
// grace (River retains terminal jobs for hours before pruning). The cost of
// the grace is a ≤ ~grace+interval delay on the email.failed for a genuinely
// abandoned job — a deliberate trade against false terminal failures
// (async-send-contract §3.1).
const providerEvidenceGrace = 15 * time.Minute

// TerminalReconcileArgs drives the periodic safety net for outbound messages
// whose stamped send job reached a terminal state before recording delivery.
type TerminalReconcileArgs struct{}

func (TerminalReconcileArgs) Kind() string { return "outbound_terminal_reconcile" }

// TerminalReconcileWorker settles accepted/sending outbound messages after
// their stamped River job is terminal or has already been pruned. SendWorker
// is still the primary owner; the compare-and-set store transitions make races
// safe. The store's guarded MarkFailed is the single terminal write: a row
// with provider-accept evidence is settled as sent (+ email.sent), a row
// without evidence — once past the providerEvidenceGrace window — is declared
// failed with provenance 'local' (correctable, §3.1) + exactly one
// email.failed.
type TerminalReconcileWorker struct {
	river.WorkerDefaults[TerminalReconcileArgs]
	pool    *pgxpool.Pool
	store   Store
	ramp    RampGate
	metrics Metrics
}

// NewTerminalReconcileWorker builds the periodic safety-net worker.
func NewTerminalReconcileWorker(pool *pgxpool.Pool, store Store, ramps ...RampGate) *TerminalReconcileWorker {
	w := &TerminalReconcileWorker{pool: pool, store: store, metrics: noopMetrics{}}
	if len(ramps) > 0 {
		w.ramp = ramps[0]
	}
	return w
}

// WithMetrics injects the SLI recorder. Chainable; nil keeps the no-op
// default so metrics stay optional wiring.
func (w *TerminalReconcileWorker) WithMetrics(m Metrics) *TerminalReconcileWorker {
	if m != nil {
		w.metrics = m
	}
	return w
}

type terminalCandidate struct {
	messageID                string
	jobID                    int64
	attempt                  int
	state                    string
	finalizedAt              *time.Time
	acceptedAt               time.Time  // messages.created_at
	scheduledAt              *time.Time // messages.scheduled_at (nil unless scheduled)
	reviewedAt               *time.Time // messages.reviewed_at (nil unless HITL-held)
	failureSource            delivery.FailureSource
	detail                   string
	failureReason            string
	failureOccurredAt        *time.Time
	failureAttempt           *int
	failureBlockedRecipients []string
}

// submissionAnchor is this candidate's acceptance→terminal SLI baseline — the
// same definition the send worker uses, so a message settled by the reconciler
// and one settled inline are measured identically.
func (c terminalCandidate) submissionAnchor() time.Time {
	var scheduledAt, reviewedAt time.Time
	if c.scheduledAt != nil {
		scheduledAt = *c.scheduledAt
	}
	if c.reviewedAt != nil {
		reviewedAt = *c.reviewedAt
	}
	return submissionAnchor(c.acceptedAt, scheduledAt, reviewedAt)
}

func (w *TerminalReconcileWorker) Work(ctx context.Context, _ *river.Job[TerminalReconcileArgs]) error {
	// Candidates: outbound rows still pre-terminal whose stamped job can never
	// run again. Processed immediately when provider-accept evidence exists
	// (settled as sent) or when the job has been terminal past the grace
	// window / was already pruned; a freshly-terminal row without evidence is
	// skipped this pass so in-flight SES notifications can still arrive.
	rows, err := w.pool.Query(ctx,
		`SELECT m.id,
		        m.send_job_id,
		        COALESCE(r.attempt, 0),
		        CASE WHEN r.id IS NULL THEN 'missing' ELSE r.state::text END,
		        r.finalized_at,
		        m.created_at, m.scheduled_at, m.reviewed_at,
		        COALESCE(m.delivery_failure_source,''),COALESCE(m.delivery_detail,''),COALESCE(m.delivery_failure_reason_code,''),
		        m.delivery_failure_occurred_at,m.delivery_failure_attempt,m.delivery_failure_blocked_recipients
		   FROM messages m
		   LEFT JOIN river_job r ON r.id = m.send_job_id
		  WHERE m.direction = 'outbound'
		    AND m.delivery_status IN ('accepted', 'sending')
		    AND m.send_job_id IS NOT NULL
		    AND (r.id IS NULL OR r.state IN ('cancelled', 'discarded', 'completed'))
		    AND ( m.provider_accepted_at IS NOT NULL
		       OR r.id IS NULL
		       OR COALESCE(r.finalized_at, to_timestamp(0)) <= now() - make_interval(secs => $2) )
		  ORDER BY m.created_at ASC, m.id ASC
		  LIMIT $1`,
		jobs.DefaultReconcileBatch, providerEvidenceGrace.Seconds(),
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	candidates := make([]terminalCandidate, 0)
	for rows.Next() {
		var candidate terminalCandidate
		if err := rows.Scan(&candidate.messageID, &candidate.jobID, &candidate.attempt, &candidate.state, &candidate.finalizedAt, &candidate.acceptedAt, &candidate.scheduledAt, &candidate.reviewedAt, &candidate.failureSource, &candidate.detail, &candidate.failureReason, &candidate.failureOccurredAt, &candidate.failureAttempt, &candidate.failureBlockedRecipients); err != nil {
			return err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	processed := 0
	for _, candidate := range candidates {
		detail := fmt.Sprintf("outbound send job %s before terminal delivery status was recorded", candidate.state)
		if candidate.detail != "" {
			detail = candidate.detail
		}
		reason := messagelifecycle.ReasonSubmissionLocalRetriesExhausted
		source := delivery.FailureSourceLocal
		storedReason := messagelifecycle.ReasonCode(candidate.failureReason)
		if messagelifecycle.IsTerminalSubmissionFailure(storedReason) {
			reason = storedReason
			if reason == messagelifecycle.ReasonSubmissionProviderRejected {
				source = delivery.FailureSourceProvider
			}
		} else if candidate.failureSource == delivery.FailureSourceProvider {
			reason = messagelifecycle.ReasonSubmissionProviderRejected
			source = delivery.FailureSourceProvider
		} else if candidate.state == "cancelled" {
			reason = messagelifecycle.ReasonSubmissionCancelled
		}
		occurredAt := time.Now().UTC()
		if candidate.finalizedAt != nil && !candidate.finalizedAt.IsZero() {
			occurredAt = candidate.finalizedAt.UTC()
		}
		if candidate.failureOccurredAt != nil && !candidate.failureOccurredAt.IsZero() {
			occurredAt = candidate.failureOccurredAt.UTC()
		}
		attempt := candidate.attempt
		if candidate.failureAttempt != nil {
			attempt = *candidate.failureAttempt
		}
		// MarkFailed is the guarded terminal write: it settles the row as sent
		// when provider-accept evidence exists (never a false failure), else
		// fails it with provenance 'local' so later authoritative evidence can
		// still correct it. The stored detail of a deferred final attempt is
		// preferred over this generic sweep detail.
		settled, settledAt, err := w.store.MarkFailed(ctx, candidate.messageID, candidate.jobID, attempt, occurredAt, detail, source, reason, candidate.failureBlockedRecipients)
		if err != nil {
			if processed > 0 {
				log.Printf("[outbound-terminal-reconcile] processed %d candidates", processed)
			}
			return err
		}
		// One terminal outcome per settled stranded row — labeled by what the
		// guarded write actually did. Evidence-settled rows (the reconciler's
		// priority population: submitted, crashed before MarkSent) count as
		// "sent", not as a false failure; a no-op (row already terminal)
		// counts nothing. emitTerminal uses the write's EFFECTIVE occurred_at
		// (settledAt — the provider-accept evidence time on an evidence
		// settle), so the latency measures acceptance→provider-accept, not
		// acceptance→sweep.
		switch settled {
		case delivery.StatusFailed:
			emitTerminal(w.metrics, terminalOutcome(source, reason, candidate.failureBlockedRecipients), candidate.submissionAnchor(), settledAt)
		case delivery.StatusSent:
			emitTerminal(w.metrics, terminalSent, candidate.submissionAnchor(), settledAt)
		}
		if w.ramp != nil {
			if err := w.ramp.Resolve(ctx, candidate.messageID); err != nil {
				return fmt.Errorf("resolve sending ramp for %s: %w", candidate.messageID, err)
			}
		}
		processed++
	}
	if processed > 0 {
		log.Printf("[outbound-terminal-reconcile] processed %d candidates", processed)
	}
	return w.resolveTerminalRampReservations(ctx)
}

// resolveTerminalRampReservations is the durable safety net for the narrow
// window where a worker commits a terminal message outcome, then cannot settle
// its sending-ramp reservation. That worker returns an error and normally fixes
// the reservation on its next (unclaimable-message) retry, but its last River
// attempt can be discarded before another retry. The sweep also revisits a
// released reservation when authoritative provider feedback later corrects a
// locally inferred failure. The reservation table's state/updated_at index
// makes this bounded sweep cheap; Resolve derives confirm versus release from
// the message's durable delivery status.
func (w *TerminalReconcileWorker) resolveTerminalRampReservations(ctx context.Context) error {
	if w.ramp == nil {
		return nil
	}
	rows, err := w.pool.Query(ctx,
		`SELECT r.message_id
		   FROM sending_ramp_reservations r
		   JOIN messages m ON m.id = r.message_id
		  WHERE (r.state = 'reserved'
		         AND m.delivery_status IN ('sent', 'failed', 'deferred', 'delivered', 'bounced', 'complained'))
		     OR (r.state = 'released'
		         AND m.delivery_status IN ('sent', 'deferred', 'delivered', 'bounced', 'complained'))
		  ORDER BY r.updated_at ASC, r.message_id ASC
		  LIMIT $1`,
		jobs.DefaultReconcileBatch,
	)
	if err != nil {
		return err
	}
	messageIDs := make([]string, 0)
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			rows.Close()
			return err
		}
		messageIDs = append(messageIDs, messageID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, messageID := range messageIDs {
		if err := w.ramp.Resolve(ctx, messageID); err != nil {
			return fmt.Errorf("resolve terminal sending ramp for %s: %w", messageID, err)
		}
	}
	return nil
}

func terminalReconcilePeriodicConstructor() (river.JobArgs, *river.InsertOpts) {
	return TerminalReconcileArgs{}, &river.InsertOpts{Queue: jobs.QueueMaintenance}
}
