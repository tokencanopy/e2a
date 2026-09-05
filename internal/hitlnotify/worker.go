// worker.go is Layer 3 of the durable HITL approval-notification pipeline
// (docs/design/hitl-notify-river.md): the River execution stage that takes a
// pending_review message, recomposes the reviewer's approve/reject email, and
// submits it ONCE off the request path. It mirrors internal/outboundsend — a
// River Worker on the shared `notify` queue, with River owning claim/retry/rescue.
//
// At-least-once from the 202 response: the hitl_notify job is enqueued in the same
// tx as the pending_review row (the hold accept-tx) before the API answers, so an
// accepted hold's notification is never lost. The worker's notified_at stamp
// (written only AFTER a successful send) makes a crash-after-send re-drive a no-op;
// loss is impossible, a rare duplicate "please review" email is benign.
package hitlnotify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// notifyRetryBackoffs is the per-attempt delay for a failed notification send.
// Shorter than the outbound sender's: a review email is worthless once the hold
// passes its TTL (the worker short-circuits on approval_expires_at), so there is
// no point in the multi-hour tail an at-least-once customer send needs.
var notifyRetryBackoffs = []time.Duration{
	15 * time.Second,
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
}

// MaxNotifyAttempts caps the retry tail before River discards the job.
const MaxNotifyAttempts = 6

// notifyOutageSnooze defers a job when the relay is unreachable, without burning
// an attempt (mirrors the outbound outage snooze).
const notifyOutageSnooze = 5 * time.Minute

// HITLNotifyArgs drives one approval-notification job. Args carry only the message
// id; the worker re-reads the message + agent (the source of truth) each attempt,
// so the job row stays tiny and always reflects the current hold state.
type HITLNotifyArgs struct {
	MessageID string `json:"message_id"`
	// OperationRef is the durable sending operation the hold's accept
	// transaction prepared. A job enqueued by a pre-floor slot carries none
	// and is resolved at fire time through the same Prepare path, then
	// stamped so the derivation happens once.
	OperationRef *sendingpolicy.OperationRef `json:"operation_ref,omitempty"`
}

func (HITLNotifyArgs) Kind() string { return "hitl_notify" }

// DeliverOutcome is the classified result of one notification send. Permanent and
// Outage split the retry decision exactly as the outbound worker's does, using the
// shared internal/outbound SMTP classifiers.
type DeliverOutcome struct {
	Err       error
	Permanent bool // 5xx / validation — no retry
	Outage    bool // relay unreachable — snooze without spending an attempt
}

// Deliverer is the two-phase send of one approval email. Compose does every
// fallible, provider-free step (owner lookup, token signing, MIME, DKIM) and
// returns the envelope; Submit hands that envelope and a freshly consumed
// authorization to the provider seam. The split exists so the worker can
// ConsumeAttempt immediately before the socket opens: a compose failure then
// costs nothing, instead of a charged ordinal that never reached the
// provider. Implemented by *Notifier.
type Deliverer interface {
	Compose(ctx context.Context, pn *identity.PendingNotify) (outbound.Envelope, DeliverOutcome)
	Submit(ctx context.Context, env outbound.Envelope, auth sendingpolicy.ProviderAuthorization) DeliverOutcome
}

// errOperationMismatch marks a job whose operation reference names another
// held message's operation. Authorizing it would charge that operation's
// account, so the job is cancelled, never retried.
var errOperationMismatch = errors.New("hitl notify: job operation reference does not name this message")

// maxNotifyAge bounds how long an approval request may wait behind a gate
// hold, independent of the hold's own TTL.
const maxNotifyAge = 7 * 24 * time.Hour

// OperationResolver recovers the durable operation for a job that carries no
// reference, through the same Prepare path an enqueue runs.
type OperationResolver func(ctx context.Context, messageID string) (sendingpolicy.OperationRef, error)

// ArgStamper persists a resolved reference into the job's args so a legacy
// job is resolved once, not once per execution.
type ArgStamper func(ctx context.Context, jobID int64, ref sendingpolicy.OperationRef) error

// Store is the message surface the worker + reconciler need. Implemented over
// internal/identity (*identity.Store).
type Store interface {
	// LoadPendingNotify returns the held message + owning agent, or (nil, nil) when
	// there is nothing to notify about (message or agent gone) — a no-op.
	LoadPendingNotify(ctx context.Context, messageID string) (*identity.PendingNotify, error)
	// MarkMessageNotified stamps notified_at after a successful send (the dedup marker).
	MarkMessageNotified(ctx context.Context, messageID string) error
	// StampNotifyJobIDTx records the job id on a reconciled row (accept-tx + reconciler).
	StampNotifyJobIDTx(ctx context.Context, tx pgx.Tx, messageID string, jobID int64) error
}

// NotifyWorker sends the approval notification for one pending_review message.
// Mirrors outboundsend.SendWorker.
type NotifyWorker struct {
	river.WorkerDefaults[HITLNotifyArgs]
	store     Store
	deliverer Deliverer
	gate      sendingpolicy.Gate
	resolve   OperationResolver
	stamp     ArgStamper
	restamp   ArgStamper
}

// NewNotifyWorker builds a worker with no sending-protection gate. Without one
// the deliverer receives an empty authorization, which the production
// submitter refuses before it dials; the composition root installs the gate
// via WithGate.
func NewNotifyWorker(store Store, deliverer Deliverer) *NotifyWorker {
	return &NotifyWorker{store: store, deliverer: deliverer}
}

// WithGate injects the sending-protection gate every notification must pass.
func (w *NotifyWorker) WithGate(g sendingpolicy.Gate) *NotifyWorker {
	if g != nil {
		w.gate = g
	}
	return w
}

// WithOperationResolver injects the legacy-argument resolver.
func (w *NotifyWorker) WithOperationResolver(r OperationResolver) *NotifyWorker {
	if r != nil {
		w.resolve = r
	}
	return w
}

// WithArgStamper injects the job-args stamp used after a legacy resolution
// (adds the reference only when absent).
func (w *NotifyWorker) WithArgStamper(s ArgStamper) *NotifyWorker {
	if s != nil {
		w.stamp = s
	}
	return w
}

// WithArgRestamper injects the unconditional re-key used when a job carries
// a pre-derivation reference.
func (w *NotifyWorker) WithArgRestamper(s ArgStamper) *NotifyWorker {
	if s != nil {
		w.restamp = s
	}
	return w
}

// NextRetry overrides River's default backoff with the decided notify envelope.
func (w *NotifyWorker) NextRetry(job *river.Job[HITLNotifyArgs]) time.Time {
	i := job.Attempt
	if i < 0 || i >= len(notifyRetryBackoffs) {
		return time.Time{} // fall back to River's default at the tail
	}
	return time.Now().Add(notifyRetryBackoffs[i])
}

// Work intentionally has no Timeout() override — a single SMTP submit of the approval
// notification fits River's 60s default JobTimeout.
func (w *NotifyWorker) Work(ctx context.Context, job *river.Job[HITLNotifyArgs]) error {
	pn, err := w.store.LoadPendingNotify(ctx, job.Args.MessageID)
	if err != nil {
		return err // DB error — retryable
	}
	if pn == nil {
		return nil // message or owning agent gone — nothing to notify
	}
	msg := pn.Message

	// Pointlessness / idempotency guards — each a no-op (return nil):
	if msg.Status != identity.MessageStatusPendingReview {
		return nil // resolved (approved/rejected/expired) before we notified
	}
	if msg.ApprovalExpiresAt != nil && msg.ApprovalExpiresAt.Before(time.Now()) {
		return nil // hold already past TTL — a review email is now useless
	}
	if !job.CreatedAt.IsZero() && time.Since(job.CreatedAt) > maxNotifyAge {
		// A hold with no TTL on record behind a paused account would otherwise
		// snooze forever (River's snooze spends no attempt); a week-old
		// approval request is stale by any reading.
		log.Printf("[hitl-notify] dropping notice for %s: older than %s", msg.ID, maxNotifyAge)
		return nil
	}
	if pn.Notified {
		return nil // a prior attempt already sent it (crash-after-send re-drive)
	}
	if pn.Agent != nil && pn.Agent.SuppressNotifications {
		return nil // agent opted out of approval notifications
	}

	// Compose first: owner lookup, magic-link signing, MIME and DKIM are all
	// fallible and none of them touches the provider, so they run before any
	// attempt is charged. A failure here is classified exactly like a send
	// failure but costs no ordinal.
	env, out := w.deliverer.Compose(ctx, pn)
	if out.Err != nil {
		return w.verdict(job, msg.ID, "compose", out)
	}

	// Every provider call is authorized: Reserve the durable attempt, hold
	// without I/O when the gate says so, ConsumeAttempt as the LAST decision
	// before Submit, whose submitter redeems the token immediately before the
	// socket opens. Notifications carry no durable hold class of their own —
	// the approval TTL guard above already bounds how long one can wait, and
	// a hold past it becomes the no-op the guard returns.
	auth := sendingpolicy.ProviderAuthorization{}
	if w.gate != nil {
		ref, err := w.operationFor(ctx, job)
		if err != nil {
			if errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
				return nil // the hold is gone — nothing to notify
			}
			if errors.Is(err, errOperationMismatch) {
				return river.JobCancel(err)
			}
			return err
		}
		early, attempt, err := w.gate.Reserve(ctx, ref)
		if err != nil {
			if errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
				return nil
			}
			return river.JobSnooze(notifyOutageSnooze)
		}
		if !early.Allow {
			return holdVerdict(early)
		}
		decision, token, err := w.gate.ConsumeAttempt(ctx, attempt)
		if err != nil {
			if errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
				return nil
			}
			return river.JobSnooze(notifyOutageSnooze)
		}
		if !decision.Allow || token == nil {
			return holdVerdict(decision)
		}
		auth = *token
	}

	out = w.deliverer.Submit(ctx, env, auth)
	if out.Err == nil {
		log.Printf("[hitl-notify] sent approval email: msg=%s", msg.ID)
		if merr := w.store.MarkMessageNotified(ctx, msg.ID); merr != nil {
			// The email is already out; only the dedup marker failed to persist. Do
			// NOT return an error — a retry would re-send. Completing the job leaves
			// notify_job_id set, so the reconciler won't re-enqueue it either.
			log.Printf("[hitl-notify] sent %s but mark-notified failed: %v", msg.ID, merr)
		}
		return nil
	}
	return w.verdict(job, msg.ID, "send", out)
}

// verdict turns a classified failure into River's answer: a permanent one
// cancels (the hold still finalizes on its TTL), an outage snoozes without
// spending a River attempt, everything else retries per NextRetry until
// MaxNotifyAttempts.
func (w *NotifyWorker) verdict(job *river.Job[HITLNotifyArgs], messageID, phase string, out DeliverOutcome) error {
	if out.Permanent {
		log.Printf("[hitl-notify] permanent %s failure for %s (no retry): %v", phase, messageID, out.Err)
		return river.JobCancel(out.Err)
	}
	if out.Outage {
		// Relay unreachable. If the hold has since passed its TTL, the next
		// attempt's expiry guard short-circuits to a no-op.
		return river.JobSnooze(notifyOutageSnooze)
	}
	return fmt.Errorf("hitl notify attempt %d %s failed: %w", job.Attempt, phase, out.Err)
}

// operationFor returns the job's durable operation, resolving and stamping a
// legacy job through the accept path.
func (w *NotifyWorker) operationFor(ctx context.Context, job *river.Job[HITLNotifyArgs]) (sendingpolicy.OperationRef, error) {
	// The approval request's operation IS derived from the message id, so a
	// reference naming any other operation would charge another account: the
	// same binding the message worker enforces, checked before Reserve.
	want := sendingpolicy.HITLNotificationOperationID(job.Args.MessageID)
	stamp := w.stamp
	if job.Args.OperationRef != nil && !job.Args.OperationRef.IsZero() {
		stored := job.Args.OperationRef.ID()
		if stored == want {
			return *job.Args.OperationRef, nil
		}
		if sendingpolicy.IsHITLNotificationOperationID(stored) {
			// A derived id for a different message: foreign, never authorize.
			// (Any other shape, a wrong-kind derivation included, is re-derived
			// from this job's own source below, so no stored id can redirect
			// attribution.)
			return sendingpolicy.OperationRef{}, errOperationMismatch
		}
		// A pre-derivation reference — migration 113 stamped adopted jobs
		// with op_<md5>, and the first build of this seam minted op_<rand>.
		// Its source is still this job's own message, so re-derive through
		// the same Prepare path and replace the reference, once.
		log.Printf("[hitl-notify] job %d carries a pre-derivation operation reference %s; re-keying", job.ID, stored)
		stamp = w.restamp
	}
	if w.resolve == nil {
		return sendingpolicy.OperationRef{}, fmt.Errorf("hitl notify: legacy job %d carries no operation and no resolver is wired", job.ID)
	}
	ref, err := w.resolve(ctx, job.Args.MessageID)
	if err != nil {
		return sendingpolicy.OperationRef{}, err
	}
	if ref.ID() != want {
		return sendingpolicy.OperationRef{}, errOperationMismatch
	}
	if stamp != nil {
		if err := stamp(ctx, job.ID, ref); err != nil {
			// Not fatal: the reference is valid for this execution; a retry
			// resolves again (idempotently) and stamps then.
			log.Printf("[hitl-notify] stamp operation on legacy job %d: %v", job.ID, err)
		}
	}
	return ref, nil
}

// holdVerdict turns a gate hold into River's answer: a terminal hold cancels
// the job, everything else waits for the gate's retry time or the outage pace.
func holdVerdict(d sendingpolicy.Decision) error {
	if d.Terminal {
		return river.JobCancel(fmt.Errorf("hitl notify: sending policy: %s", d.Reason))
	}
	delay := notifyOutageSnooze
	if !d.RetryAt.IsZero() {
		if until := time.Until(d.RetryAt); until > delay {
			delay = until
		}
	}
	return river.JobSnooze(delay)
}

// Gate exposes the wired gate (nil when gateless), for the composition
// root's wiring test.
func (w *NotifyWorker) Gate() sendingpolicy.Gate { return w.gate }
