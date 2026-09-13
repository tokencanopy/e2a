// Package agentsignupnotify delivers public-signup verification mail from the
// shared River outbox. Signup state and its job commit in one transaction;
// provider I/O happens only in the worker after commit.
package agentsignupnotify

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/jobs"
)

const MaxAttempts = 6

type Args struct {
	SignupID string `json:"signup_id"`
	Nonce    string `json:"nonce"`
}

func (Args) Kind() string { return "agent_signup_notify" }

type Deliverer interface {
	DeliverAgentSignupVerification(context.Context, string, string) error
}

type Jobs struct {
	enq       jobs.Enqueuer
	mu        sync.RWMutex
	deliverer Deliverer
}

func NewJobs() *Jobs { return &Jobs{} }

func (j *Jobs) SetEnqueuer(e jobs.Enqueuer) { j.enq = e }

func (j *Jobs) SetDeliverer(d Deliverer) {
	j.mu.Lock()
	j.deliverer = d
	j.mu.Unlock()
}

func (j *Jobs) EnqueueAgentSignupVerificationTx(ctx context.Context, tx pgx.Tx, signupID, nonce string) error {
	if j.enq == nil {
		return errors.New("agent signup notification enqueuer is not wired")
	}
	_, err := j.enq.InsertTx(ctx, tx, Args{SignupID: signupID, Nonce: nonce}, &river.InsertOpts{
		Queue: jobs.QueueNotify, MaxAttempts: MaxAttempts,
	})
	return err
}

func (j *Jobs) RegisterJobs(workers *river.Workers) []*river.PeriodicJob {
	river.AddWorker(workers, &worker{jobs: j})
	return nil
}

type worker struct {
	river.WorkerDefaults[Args]
	jobs *Jobs
}

func (w *worker) Work(ctx context.Context, job *river.Job[Args]) error {
	w.jobs.mu.RLock()
	d := w.jobs.deliverer
	w.jobs.mu.RUnlock()
	if d == nil {
		return errors.New("agent signup notification deliverer is not wired")
	}
	return d.DeliverAgentSignupVerification(ctx, job.Args.SignupID, job.Args.Nonce)
}
