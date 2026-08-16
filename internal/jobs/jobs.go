// Package jobs is the single River composition root for e2a's background work.
// All durable background jobs — outbound sends, webhook delivery, HITL hold
// resolution, sender-identity provisioning, janitors — run on ONE shared
// river.Client against ONE river_job table, instead of a bespoke Postgres queue
// per subsystem. Each domain contributes its workers as a Registrar; queues are
// central (queues.go); business code enqueues through the Enqueuer interface so
// it stays testable and River lives at the edges.
//
// This replaces the hand-rolled claim/lease/retry/sweep machinery: River's
// SKIP LOCKED claim, per-job MaxAttempts + retry policy, and built-in reaper do
// the plumbing, so domains only write job args + a Work() handler.
package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
)

// Enqueuer is the narrow insert surface business code depends on, rather than the
// whole river.Client — keeps the store/agent layer testable with a fake and River
// swappable. Insert enqueues immediately; InsertTx enqueues WITHIN the caller's
// transaction (the outbox pattern: the job commits atomically with the business
// write and can never be lost). *river.Client[pgx.Tx] satisfies this directly.
type Enqueuer interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Registrar is what each domain implements to contribute its jobs to the shared
// client. RegisterJobs adds the domain's workers to the shared bundle (via
// river.AddWorker) and returns any periodic jobs it wants scheduled. Called once,
// at client construction — River requires all workers registered before Start.
type Registrar interface {
	RegisterJobs(w *river.Workers) []*river.PeriodicJob
}

// Config sizes the per-queue worker pools. Zero values get sane defaults.
type Config struct {
	OutboundWorkers       int // QueueOutbound concurrency (default 8)
	InboundWorkers        int // QueueInbound concurrency (default 8)
	WebhookWorkers        int // QueueWebhook concurrency (default 16)
	MaintenanceWorkers    int // QueueMaintenance concurrency (default 2)
	NotifyWorkers         int // QueueNotify concurrency (default 4)
	SenderIdentityWorkers int // QueueSenderIdentityV2 concurrency (default 1)
	DefaultWorkers        int // QueueDefault concurrency (default 5)
}

func (c Config) withDefaults() Config {
	if c.OutboundWorkers <= 0 {
		c.OutboundWorkers = 8
	}
	if c.InboundWorkers <= 0 {
		c.InboundWorkers = 8
	}
	if c.WebhookWorkers <= 0 {
		c.WebhookWorkers = 16
	}
	if c.MaintenanceWorkers <= 0 {
		c.MaintenanceWorkers = 2
	}
	if c.NotifyWorkers <= 0 {
		c.NotifyWorkers = 4
	}
	if c.SenderIdentityWorkers <= 0 {
		c.SenderIdentityWorkers = 1
	}
	if c.DefaultWorkers <= 0 {
		c.DefaultWorkers = 5
	}
	return c
}

// Client is the shared River client plus e2a's lifecycle helpers. It embeds
// *river.Client[pgx.Tx], so it IS an Enqueuer and exposes Start/Stop directly.
type Client struct {
	*river.Client[pgx.Tx]
}

// CancelTx cancels a durable job atomically with a caller-owned transaction.
// A missing job is already in the desired state (River may have pruned a
// finalized row), so deletion workflows treat it as success.
func (c *Client) CancelTx(ctx context.Context, tx pgx.Tx, jobID int64) error {
	_, err := c.JobCancelTx(ctx, tx, jobID)
	if errors.Is(err, rivertype.ErrNotFound) {
		return nil
	}
	return err
}

// Migrate applies River's own schema (river_job et al.) to the pool. River tracks
// its migrations in its own river_migration table, separate from e2a's
// schema_migrations, so this is idempotent and safe alongside identity migrations.
// Call once at startup, after the e2a migrations. (Generalized from the
// per-manager Migrate senderidentity used to own.)
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	return nil
}

// New builds the one shared client from every domain's Registrar. Each registrar
// adds its workers and periodic jobs; queues are the central set (queues.go). The
// DB must already have River's schema (call Migrate first).
func New(pool *pgxpool.Pool, cfg Config, registrars ...Registrar) (*Client, error) {
	cfg = cfg.withDefaults()
	workers := river.NewWorkers()
	var periodic []*river.PeriodicJob
	for _, r := range registrars {
		periodic = append(periodic, r.RegisterJobs(workers)...)
	}

	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:       defaultQueueConfig(cfg),
		Workers:      workers,
		PeriodicJobs: periodic,
	})
	if err != nil {
		return nil, fmt.Errorf("river client: %w", err)
	}
	return &Client{Client: rc}, nil
}
