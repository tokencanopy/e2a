// Package billingnotify delivers account-trash transitions (trash, restore)
// to the external billing service as durable River jobs.
//
// A trash asks billing to cancel the subscription at period end; a restore
// asks it to revert that. Both used to be best-effort HTTP calls after
// commit, so a single billing 5xx on restore left a restored customer's
// subscription scheduled to cancel with no retry. The job is enqueued in the
// same transaction as the trash or restore, retried with River's backoff, and
// is convergent and serialized per account: the worker takes a
// transaction-scoped advisory lock on the account, re-reads its state and
// posts while still holding the lock, and skips a notice the account has
// since moved past (a trash notice for an account that was restored, a
// restore notice for one trashed again, either for an account already purged
// — purge has its own cancel call). Because the read and the post happen
// under one per-account lock, and every transition enqueues its own notice in
// its own transaction, the last post for an account always reflects its
// latest committed state — even when River runs a trash and a restore notice
// concurrently or out of order.
//
// The job is deliberately NOT River-unique by account: River's uniqueness
// must include the running state, so a restore notice inserted while a trash
// notice is running would be dropped as a duplicate — the running trash post
// would land last and leave a live customer set to cancel. The advisory lock
// gives the one-at-a-time property without that loss.
package billingnotify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
)

// Modes.
const (
	ModeTrash   = "trash"
	ModeRestore = "restore"
)

// MaxAttempts bounds retries (River's exponential backoff spreads 20
// attempts over roughly two weeks — longer than any billing outage we
// should survive silently).
const MaxAttempts = 20

// Args is the job payload.
type Args struct {
	UserID string `json:"user_id"`
	Mode   string `json:"mode"`
}

// Kind implements river.JobArgs.
func (Args) Kind() string { return "billing_account_state" }

// ErrNotFound is what a Poster returns when the billing service does not know
// the endpoint (an older service answering 404). It is permanent: the job
// completes with a log line instead of retrying for two weeks.
var ErrNotFound = errors.New("billingnotify: billing service has no account-state endpoint (404)")

// Poster sends one notice. Returns ErrNotFound for a 404.
type Poster func(ctx context.Context, userID, mode string) error

// Jobs is the registrar + enqueuer. Poster is late-bound (the agent API that
// owns the billing URLs is built after the River client starts).
type Jobs struct {
	enq  jobs.Enqueuer
	pool *pgxpool.Pool

	mu     sync.RWMutex
	poster Poster
}

// New builds the registrar over the database the account state lives in.
func New(pool *pgxpool.Pool) *Jobs { return &Jobs{pool: pool} }

// SetEnqueuer injects the shared River client.
func (j *Jobs) SetEnqueuer(e jobs.Enqueuer) { j.enq = e }

// SetPoster late-binds the HTTP sender.
func (j *Jobs) SetPoster(p Poster) {
	j.mu.Lock()
	j.poster = p
	j.mu.Unlock()
}

func (j *Jobs) getPoster() Poster {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.poster
}

// RegisterJobs implements jobs.Registrar.
func (j *Jobs) RegisterJobs(w *river.Workers) []*river.PeriodicJob {
	river.AddWorker(w, &Worker{jobs: j})
	return nil
}

// EnqueueTx inserts the notice in the caller's transaction (the trash or
// restore transaction), so the notice exists exactly when the transition
// commits.
func (j *Jobs) EnqueueTx(ctx context.Context, tx pgx.Tx, userID, mode string) error {
	if j.enq == nil {
		return errors.New("billingnotify: enqueuer not configured")
	}
	if mode != ModeTrash && mode != ModeRestore {
		return fmt.Errorf("billingnotify: unknown mode %q", mode)
	}
	_, err := j.enq.InsertTx(ctx, tx, Args{UserID: userID, Mode: mode}, &river.InsertOpts{
		Queue:       jobs.QueueDefault,
		MaxAttempts: MaxAttempts,
	})
	return err
}

// Worker posts one notice.
type Worker struct {
	river.WorkerDefaults[Args]
	jobs *Jobs
}

// Timeout bounds one attempt.
func (w *Worker) Timeout(*river.Job[Args]) time.Duration { return 30 * time.Second }

// Work posts the notice unless the account has moved past it. The state
// read and the post happen under one per-account advisory lock (held by this
// transaction for at most the job timeout), so notices for one account are
// strictly serialized.
func (w *Worker) Work(ctx context.Context, job *river.Job[Args]) error {
	poster := w.jobs.getPoster()
	if poster == nil {
		return river.JobSnooze(time.Minute) // API not bound yet (startup)
	}
	tx, err := w.jobs.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, LockKey(job.Args.UserID)); err != nil {
		return err
	}
	var trashed bool
	err = tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM users WHERE id = $1`, job.Args.UserID).Scan(&trashed)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	stale := !exists ||
		(job.Args.Mode == ModeTrash && !trashed) ||
		(job.Args.Mode == ModeRestore && trashed)
	if stale {
		return tx.Commit(ctx)
	}
	err = poster(ctx, job.Args.UserID, job.Args.Mode)
	if errors.Is(err, ErrNotFound) {
		log.Printf("[billing-notify] %s notice for user=%s: billing service has no account-state endpoint; not retrying", job.Args.Mode, job.Args.UserID)
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LockKey is the per-account advisory lock key notices serialize on.
func LockKey(userID string) string { return "billing_account_state:" + userID }

// NewWorkerForTest returns the worker RegisterJobs registers.
func NewWorkerForTest(j *Jobs) *Worker { return &Worker{jobs: j} }
