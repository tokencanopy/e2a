// Package janitor runs the low-urgency hourly cleanup sweep as a River
// periodic on QueueMaintenance. It replaces the hand-rolled time.Ticker(1h)
// goroutine that used to live in cmd/e2a/main.go, mirroring the webhook
// auto-disable janitor (internal/webhookdelivery.MaintenanceJobs) and the
// inbound retention sweep (internal/inboundprocess.RetentionWorker).
//
// The sweep runs every prune SEQUENTIALLY (not concurrently — a janitor must
// be gentle on Postgres and there is no latency benefit to parallelism) and
// CONTINUES PAST any individual prune's error, so one failing DELETE never
// skips the rest. Sweep accumulates and returns the errors; the worker
// log-and-swallows them (returning nil) so a transient DB blip doesn't spin
// River's retry machinery — the next interval picks the work back up.
package janitor

import (
	"context"
	"errors"
	"github.com/tokencanopy/e2a/internal/identity"
	"log"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/oauth"
)

// janitorInterval is the cleanup cadence — matches the prior time.Ticker(1h).
const janitorInterval = 1 * time.Hour

// The prune dependencies are narrow interfaces (one per store's prune
// method(s)) so the sweep is unit-testable with a single fake and never
// depends on a concrete store. Signatures match the real store methods.

// MessagePruner purges messages past trash retention, expired user sessions, and trashed
// agents past their retention window (all live on *identity.Store today).
// DeleteExpiredMessages covers both message arms: live rows past their
// trashed rows past TrashRetention.
type MessagePruner interface {
	DeleteExpiredMessages(ctx context.Context) (int64, error)
	DeleteExpiredUserSessions(ctx context.Context) (int64, error)
	PurgeDeletedAgents(ctx context.Context) (int64, error)
}

// DeliveryPruner prunes expired webhook delivery records (*webhook.DeliveryStore).
type DeliveryPruner interface {
	DeleteExpiredDeliveries(ctx context.Context) (int64, error)
}

// SubscriberPruner prunes expired webhook subscriber deliveries
// (*webhook.SubscriberStore): deleted counts terminal expired rows removed,
// marked counts expired still-pending rows transitioned to 'failed'
// ("expired before delivery") instead of being silently deleted.
type SubscriberPruner interface {
	DeleteExpiredSubscriberDeliveries(ctx context.Context) (deleted, marked int, err error)
}

// WebhookEventPruner prunes expired webhook_events rows (webhookpub outbox).
type WebhookEventPruner interface {
	DeleteExpiredWebhookEvents(ctx context.Context) (int, error)
}

// IdempotencyPruner sweeps idempotency keys past their TTL (*idempotency.Store).
type IdempotencyPruner interface {
	Sweep(ctx context.Context) (int64, error)
}

// OAuthPruner cleans up expired OAuth rows (*oauth.Storage). Optional: when the
// OAuth provider isn't configured the dependency is nil and the pass is skipped
// (preserving the old `if oauthStorage != nil` guard). Pass a nil interface —
// NOT a typed nil *oauth.Storage — to skip it.
type OAuthPruner interface {
	CleanupExpired(ctx context.Context, now time.Time) (oauth.RetentionResult, error)
}

// Metrics is the narrow slice of telemetry.Metrics the janitor emits. Injectable
// so tests don't need a real backend; satisfied by *telemetry.Log / telemetry.NoOp.
type Metrics interface {
	JanitorRowsDeleted(table string, count int)
	// WebhookExpiredPending counts delivery rows the subscriber prune marked
	// failed at their TTL instead of deleting while pending.
	WebhookExpiredPending(count int)
}

// Janitor holds the prune dependencies and runs the cleanup sweep. All fields
// are required except oauth, which is nil when the OAuth provider is disabled.
// engagementReconcileBatch bounds one reconciliation pass so the consistency
// check cannot dominate a maintenance run on a large account.
const engagementReconcileBatch = 500

// dueEngagementBatch bounds one wake-up sweep. Anything not claimed this pass
// is picked up on the next, so a large campaign coming due at once spreads
// across runs instead of flooding a subscriber in a single burst.
const dueEngagementBatch = 200

type Janitor struct {
	messages     MessagePruner
	deliveries   DeliveryPruner
	subscribers  SubscriberPruner
	webhookEvent WebhookEventPruner
	oauth        OAuthPruner // optional; nil when OAuth is not configured
	idempotency  IdempotencyPruner
	// engagements is optional (nil when contacts are not wired) — the sweep is
	// a consistency check, not a prune, so its absence must not break cleanup.
	engagements EngagementReconciler
	// contact.due wake-up sweep; both nil unless wired.
	dueClaimer   DueEngagementClaimer
	duePublisher DuePublisher
	metrics      Metrics
}

// EngagementReconciler recomputes the materialized contact-engagement counters
// from message history and corrects drift, returning what it fixed.
//
// This is the safety net for materializing those counters instead of computing
// them on read. A non-empty result means an activity hook was missed — it is a
// bug signal, not routine maintenance — so the sweep logs corrections loudly
// rather than fixing them silently.
type EngagementReconciler interface {
	ReconcileEngagementCounts(ctx context.Context, userID string, limit int) ([]identity.EngagementCountDrift, error)
}

// SetEngagementReconciler wires the contact-engagement consistency sweep.
func (j *Janitor) SetEngagementReconciler(r EngagementReconciler) { j.engagements = r }

// DueEngagementClaimer atomically claims engagements whose next_action_at has
// passed and marks them notified, so a retried sweep cannot wake an agent twice
// for one schedule.
type DueEngagementClaimer interface {
	ClaimDueEngagements(ctx context.Context, now time.Time, limit int) ([]identity.DueEngagement, error)
}

// DuePublisher emits the wake-up event for a claimed engagement.
type DuePublisher interface {
	PublishContactDue(ctx context.Context, d identity.DueEngagement) error
}

// SetDueEngagementPublisher wires the contact.due wake-up sweep. Both
// collaborators are required; either being nil disables the sweep entirely,
// because claiming without publishing would silently consume the schedule and
// the agent would never be woken.
func (j *Janitor) SetDueEngagementPublisher(c DueEngagementClaimer, p DuePublisher) {
	j.dueClaimer, j.duePublisher = c, p
}

// New builds the Janitor. oauth may be nil (interface, not a typed-nil pointer)
// to skip the OAuth cleanup pass.
func New(
	messages MessagePruner,
	deliveries DeliveryPruner,
	subscribers SubscriberPruner,
	webhookEvent WebhookEventPruner,
	oauth OAuthPruner,
	idempotency IdempotencyPruner,
	metrics Metrics,
) *Janitor {
	return &Janitor{
		messages:     messages,
		deliveries:   deliveries,
		subscribers:  subscribers,
		webhookEvent: webhookEvent,
		oauth:        oauth,
		idempotency:  idempotency,
		metrics:      metrics,
	}
}

// Sweep runs every prune once, sequentially, continuing past any individual
// error. It preserves the exact per-prune logging and metrics emission of the
// old hand-rolled ticker. Errors are accumulated and returned joined; the
// caller (the worker) logs-and-swallows so one failing prune never aborts the
// run or spins River's retry.
func (j *Janitor) Sweep(ctx context.Context) error {
	var errs []error

	// contact.due: wake agents whose outreach is due. Claim-and-mark is atomic
	// in the store, so a failed publish costs at most one missed wake-up rather
	// than a duplicate one — the safer direction, since a duplicate event
	// invites a duplicate email.
	if j.dueClaimer != nil && j.duePublisher != nil {
		if due, err := j.dueClaimer.ClaimDueEngagements(ctx, time.Now().UTC(), dueEngagementBatch); err != nil {
			log.Printf("Failed to claim due contact engagements: %v", err)
			errs = append(errs, err)
		} else {
			for _, d := range due {
				if perr := j.duePublisher.PublishContactDue(ctx, d); perr != nil {
					log.Printf("Failed to publish contact.due for agent=%s address=%s: %v",
						d.AgentEmail, d.Address, perr)
					errs = append(errs, perr)
				}
			}
			if len(due) > 0 {
				log.Printf("Published %d contact.due event(s)", len(due))
			}
		}
	}

	// Contact-engagement counters: a consistency check rather than a prune.
	// Bounded per run so one pass cannot dominate the maintenance job; drift
	// left over is picked up on the next sweep.
	if j.engagements != nil {
		if drift, err := j.engagements.ReconcileEngagementCounts(ctx, "", engagementReconcileBatch); err != nil {
			log.Printf("Failed to reconcile contact engagement counters: %v", err)
			errs = append(errs, err)
		} else if len(drift) > 0 {
			// Loud on purpose: every correction means an activity hook did not
			// fire, which is a defect to chase rather than expected churn.
			for _, d := range drift {
				log.Printf("Corrected contact engagement drift: agent=%s address=%s %s stored=%d actual=%d",
					d.AgentID, d.Address, d.Field, d.Stored, d.Actual)
			}
			j.metrics.JanitorRowsDeleted("contact_engagements_reconciled", len(drift))
		}
	}

	if deleted, err := j.messages.DeleteExpiredMessages(ctx); err != nil {
		log.Printf("Failed to purge messages past trash retention: %v", err)
		errs = append(errs, err)
	} else if deleted > 0 {
		log.Printf("Purged %d message(s) past trash retention", deleted)
		j.metrics.JanitorRowsDeleted("messages", int(deleted))
	}

	// Trashed agents past TrashRetention: hard delete, messages included
	// (docs/design/trash-soft-delete.md). Order relative to the other prunes
	// is arbitrary — each pass is independent and idempotent.
	if deleted, err := j.messages.PurgeDeletedAgents(ctx); err != nil {
		log.Printf("Failed to purge trashed agents: %v", err)
		errs = append(errs, err)
	} else if deleted > 0 {
		log.Printf("Purged %d trashed agent(s) past retention", deleted)
		j.metrics.JanitorRowsDeleted("agent_identities", int(deleted))
	}

	if deleted, err := j.messages.DeleteExpiredUserSessions(ctx); err != nil {
		log.Printf("Failed to clean up expired user sessions: %v", err)
		errs = append(errs, err)
	} else if deleted > 0 {
		log.Printf("Cleaned up %d expired user session(s)", deleted)
	}

	if deleted, err := j.deliveries.DeleteExpiredDeliveries(ctx); err != nil {
		log.Printf("Failed to clean up expired webhook deliveries: %v", err)
		errs = append(errs, err)
	} else if deleted > 0 {
		log.Printf("Cleaned up %d expired webhook delivery record(s)", deleted)
	}

	if deleted, marked, err := j.subscribers.DeleteExpiredSubscriberDeliveries(ctx); err != nil {
		log.Printf("Failed to clean up expired webhook subscriber deliveries: %v", err)
		errs = append(errs, err)
	} else {
		if deleted > 0 {
			log.Printf("Cleaned up %d expired webhook subscriber delivery record(s)", deleted)
			j.metrics.JanitorRowsDeleted("webhook_subscriber_deliveries", deleted)
		}
		if marked > 0 {
			log.Printf("Marked %d expired pending webhook subscriber delivery record(s) failed (expired before delivery)", marked)
			j.metrics.WebhookExpiredPending(marked)
		}
	}

	// webhook_events rows also carry a 30-day TTL (migration 026); without
	// this the table grows monotonically once the outbox path writes events.
	if deleted, err := j.webhookEvent.DeleteExpiredWebhookEvents(ctx); err != nil {
		log.Printf("Failed to clean up expired webhook events: %v", err)
		errs = append(errs, err)
	} else if deleted > 0 {
		log.Printf("Cleaned up %d expired webhook event(s)", deleted)
		j.metrics.JanitorRowsDeleted("webhook_events", deleted)
	}

	// OAuth cleanup is skipped when the provider isn't configured (nil dep),
	// preserving the old `if oauthStorage != nil` guard.
	if j.oauth != nil {
		if res, err := j.oauth.CleanupExpired(ctx, time.Now()); err != nil {
			log.Printf("Failed to clean up expired OAuth rows: %v", err)
			errs = append(errs, err)
		} else if res.Total() > 0 {
			log.Printf("Cleaned up OAuth rows: codes=%d pkce=%d access=%d refresh=%d clients=%d",
				res.AuthCodesDeleted, res.PKCERequestsDeleted,
				res.AccessTokensDeleted, res.RefreshTokensDeleted,
				res.ClientsDeleted)
		}
	}

	if deleted, err := j.idempotency.Sweep(ctx); err != nil {
		log.Printf("Failed to sweep idempotency keys: %v", err)
		errs = append(errs, err)
	} else if deleted > 0 {
		log.Printf("Swept %d idempotency key(s) past TTL", deleted)
	}

	return errors.Join(errs...)
}

// JanitorArgs drives the periodic cleanup sweep. No fields — each run prunes
// the whole expired set.
type JanitorArgs struct{}

func (JanitorArgs) Kind() string { return "janitor_sweep" }

// MaintenanceWorker runs the cleanup sweep once per scheduled job. Sweep's
// errors are logged (with a [janitor] prefix) and swallowed — Work returns nil
// so a transient DB blip never spins River's retry for a best-effort idempotent
// sweep; the next interval picks it up.
type MaintenanceWorker struct {
	river.WorkerDefaults[JanitorArgs]
	janitor *Janitor
}

// NewMaintenanceWorker builds the worker around a Janitor. Exported so tests can
// drive Work directly (RegisterJobs builds an identical one for the client).
func NewMaintenanceWorker(j *Janitor) *MaintenanceWorker {
	return &MaintenanceWorker{janitor: j}
}

func (w *MaintenanceWorker) Work(ctx context.Context, _ *river.Job[JanitorArgs]) error {
	if err := w.janitor.Sweep(ctx); err != nil {
		log.Printf("[janitor] sweep completed with error(s): %v", err)
	}
	return nil
}

// Timeout gives the sweep a bounded 5-minute budget instead of River's 60s
// default JobTimeout. Every prune is now a ctx-aware batched delete (each
// 5000-row batch autocommits), so a cut is always safe — partial progress
// persists and the next hourly tick resumes — but 60s is tight for a
// first-deploy backlog where `messages` (pruned first) can eat the whole budget
// and starve the later tables. Five minutes lets a realistic backlog drain in
// far fewer ticks while still capping how long one sweep holds a
// QueueMaintenance slot. Deliberately NOT -1 (unbounded) like the HITL
// send-sweep, which must never be cut mid-SMTP; the janitor's deletes are safe
// to interrupt.
func (w *MaintenanceWorker) Timeout(*river.Job[JanitorArgs]) time.Duration {
	return 5 * time.Minute
}

// MaintenanceJobs is the jobs.Registrar for the cleanup janitor: it contributes
// the MaintenanceWorker and a periodic that fires it on QueueMaintenance. No
// enqueuer needed — the schedule is the only trigger.
type MaintenanceJobs struct{ janitor *Janitor }

// NewMaintenanceJobs builds the registrar around a Janitor.
func NewMaintenanceJobs(j *Janitor) *MaintenanceJobs { return &MaintenanceJobs{janitor: j} }

// RegisterJobs adds the maintenance worker + its periodic schedule. Mirrors the
// webhook janitor and senderidentity reaper: routed to QueueMaintenance, no
// UniqueOpts (River's periodic scheduler already inserts at most one per
// interval and a completed run must not dedup-block the next), RunOnStart:false
// (first sweep after one interval, matching the old ticker's first-tick-after-1h).
//
// Overlap tradeoff (inherited from the pre-River janitor, not introduced by this
// migration): with no UniqueOpts and QueueMaintenance's small worker pool, a sweep
// that runs longer than the 1h interval can overlap the next tick. That is safe —
// every prune is an idempotent batched DELETE on a distinct table and a partial
// sweep is simply finished by the next interval; overlapping sweeps only race to
// delete the same already-doomed rows. It only bites against a large, long-unpruned
// table (e.g. the first deploy); steady-state sweeps are near-empty. The prunes'
// unbounded-DELETE hazard is resolved — each is now a ctx-aware ctid-LIMIT drain
// loop (see the DeleteExpired* store methods) bounded by MaintenanceWorker.Timeout.
func (m *MaintenanceJobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, NewMaintenanceWorker(m.janitor))
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(janitorInterval),
			janitorPeriodicConstructor,
			&river.PeriodicJobOpts{RunOnStart: false},
		),
	}
}

// janitorPeriodicConstructor is the periodic's per-fire constructor: it routes
// each scheduled sweep to QueueMaintenance. Extracted (rather than inlined) so a
// white-box test can assert the queue routing — river.PeriodicJob keeps its
// constructor unexported, so this is the only way to verify it directly.
func janitorPeriodicConstructor() (river.JobArgs, *river.InsertOpts) {
	return JanitorArgs{}, &river.InsertOpts{Queue: jobs.QueueMaintenance}
}
