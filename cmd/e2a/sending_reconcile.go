package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/rivertype"

	"github.com/tokencanopy/e2a/internal/hitlnotify"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/webhooknotify"
)

// legacySendingJobKinds are the River job kinds that submit mail to the
// provider and therefore must carry a sending operation reference. A job of
// one of these kinds without an operation_ref was enqueued by a pre-floor
// slot: the worker resolves it at fire time, but an operator can also settle
// the backlog up front with -reconcile-legacy-sending-jobs so the cutover
// leaves no job whose attribution is decided later than its enqueue.
var legacySendingJobKinds = []string{
	outboundsend.OutboundSendArgs{}.Kind(),
	hitlnotify.HITLNotifyArgs{}.Kind(),
	webhooknotify.WebhookNotifyArgs{}.Kind(),
}

// legacyReconcileStates are the job states a reconcile touches: those River
// may still pick up. A running job is left to its worker, and a finalized job
// (completed, cancelled, discarded) has nothing left to authorize.
var legacyReconcileStates = []string{
	string(rivertype.JobStateAvailable),
	string(rivertype.JobStatePending),
	string(rivertype.JobStateRetryable),
	string(rivertype.JobStateScheduled),
}

// legacyReconcileCounts is the operator-facing summary of one reconcile pass.
type legacyReconcileCounts struct {
	Scanned   int
	Stamped   int
	Cancelled int
	Paused    int
	Skipped   int // moved on by a worker between the scan and the job's own transaction
	Failed    int
}

// remaining is the number of scanned jobs that still carry no operation
// reference after the pass: those the resolver could not decide. A job whose
// account is paused is deliberately left for the worker's hold path, so it is
// not counted as remaining.
func (c legacyReconcileCounts) remaining() int { return c.Failed }

// runReconcileLegacySendingJobs stamps an operation reference onto every
// pending provider-submitting job that has none, cancelling the ones whose
// source row no longer exists. Each job is handled in its own transaction,
// through exactly the Prepare path its enqueue would have used, so a stamped
// job and a natively enqueued job authorize identically. Exit status is
// nonzero unless every scanned job was decided.
func runReconcileLegacySendingJobs(ctx context.Context, pool *pgxpool.Pool, gate sendingpolicy.Gate, stdout io.Writer) error {
	client, err := jobs.New(pool, jobs.Config{})
	if err != nil {
		return fmt.Errorf("river client: %w", err)
	}
	rows, err := pool.Query(ctx, `
		SELECT id, kind, args
		  FROM river_job
		 WHERE kind = ANY($1)
		   AND state = ANY($2)
		   AND NOT (args ? 'operation_ref')
		 ORDER BY id`, legacySendingJobKinds, legacyReconcileStates)
	if err != nil {
		return fmt.Errorf("scan legacy sending jobs: %w", err)
	}
	type legacyJob struct {
		id   int64
		kind string
		args []byte
	}
	var pending []legacyJob
	for rows.Next() {
		var j legacyJob
		if err := rows.Scan(&j.id, &j.kind, &j.args); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy sending job: %w", err)
		}
		pending = append(pending, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan legacy sending jobs: %w", err)
	}

	var counts legacyReconcileCounts
	for _, j := range pending {
		counts.Scanned++
		outcome, err := reconcileLegacySendingJob(ctx, pool, client, gate, j.id, j.kind, j.args)
		if err != nil {
			counts.Failed++
			fmt.Fprintf(stdout, "job %d (%s): %v\n", j.id, j.kind, err)
			continue
		}
		switch outcome {
		case legacyOutcomeStamped:
			counts.Stamped++
		case legacyOutcomeCancelled:
			counts.Cancelled++
		case legacyOutcomePaused:
			counts.Paused++
		case legacyOutcomeSkipped:
			counts.Skipped++
		}
	}

	fmt.Fprintf(stdout, "scanned:   %d\n", counts.Scanned)
	fmt.Fprintf(stdout, "stamped:   %d\n", counts.Stamped)
	fmt.Fprintf(stdout, "cancelled: %d\n", counts.Cancelled)
	fmt.Fprintf(stdout, "paused:    %d (left unstamped for the worker's hold path; rerun after the account resumes)\n", counts.Paused)
	fmt.Fprintf(stdout, "skipped:   %d (picked up by a worker meanwhile; the worker resolves them)\n", counts.Skipped)
	fmt.Fprintf(stdout, "failed:    %d\n", counts.Failed)
	fmt.Fprintf(stdout, "remaining: %d (undecided; nonzero exit)\n", counts.remaining())
	if counts.remaining() != 0 {
		return fmt.Errorf("%d legacy sending job(s) could not be reconciled", counts.remaining())
	}
	return nil
}

type legacyOutcome int

const (
	legacyOutcomeStamped legacyOutcome = iota + 1
	legacyOutcomeCancelled
	legacyOutcomePaused
	legacyOutcomeSkipped
)

// reconcileLegacySendingJob decides one job inside one transaction: the
// source row is locked by the Prepare call, the reference is stamped (or the
// orphan cancelled) in the same transaction, and a failure rolls both back so
// a rerun sees the job untouched.
func reconcileLegacySendingJob(ctx context.Context, pool *pgxpool.Pool, client *jobs.Client, gate sendingpolicy.Gate, jobID int64, kind string, rawArgs []byte) (legacyOutcome, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Re-read the job under its row lock: the scan ran outside this
	// transaction, and a worker may have claimed the job (or resolved and
	// stamped it itself) since. Deciding a job a worker now owns would
	// prepare beside it and could cancel it mid-flight, so anything that
	// left the reconcilable states is skipped and left to that worker. The
	// lock also serializes against the worker's own stamp.
	var state string
	var stamped bool
	err = tx.QueryRow(ctx,
		`SELECT state, (args ? 'operation_ref') FROM river_job WHERE id = $1 FOR UPDATE`, jobID,
	).Scan(&state, &stamped)
	if errors.Is(err, pgx.ErrNoRows) {
		return legacyOutcomeSkipped, nil
	}
	if err != nil {
		return 0, fmt.Errorf("lock job: %w", err)
	}
	if stamped || !slices.Contains(legacyReconcileStates, state) {
		return legacyOutcomeSkipped, nil
	}

	var ref sendingpolicy.OperationRef
	var cancelReason string
	switch kind {
	case outboundsend.OutboundSendArgs{}.Kind():
		var args outboundsend.OutboundSendArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return 0, fmt.Errorf("decode args: %w", err)
		}
		decision, prepared, err := gate.PrepareExternalTx(ctx, tx, args.MessageID)
		switch {
		case errors.Is(err, sendingpolicy.ErrSourceUnavailable):
			cancelReason = "legacy source unavailable"
		case err != nil:
			return 0, err
		case decision == sendingpolicy.AcceptanceSendingPaused:
			// The worker's hold path owns a paused account: it records the
			// hold on the message and waits for the operator. Nothing to
			// stamp yet; the rerun after the resume picks it up.
			return legacyOutcomePaused, nil
		case prepared.IsZero():
			// The only accepted shape with no operation is an exact
			// self-send, which never enqueues; a queued job that resolves to
			// nothing cannot be authorized by any worker.
			cancelReason = "message has no provider operation"
		default:
			ref = prepared
		}
	case hitlnotify.HITLNotifyArgs{}.Kind():
		var args hitlnotify.HITLNotifyArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return 0, fmt.Errorf("decode args: %w", err)
		}
		ref, cancelReason, err = prepareLegacyNotification(ctx, tx, gate, sendingpolicy.NewHITLNotificationRef(args.MessageID))
		if err != nil {
			return 0, err
		}
	case webhooknotify.WebhookNotifyArgs{}.Kind():
		var args webhooknotify.WebhookNotifyArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return 0, fmt.Errorf("decode args: %w", err)
		}
		ref, cancelReason, err = prepareLegacyNotification(ctx, tx, gate, sendingpolicy.NewWebhookHealthNotificationRef(args.WebhookID, args.NotifyKind))
		if err != nil {
			return 0, err
		}
	default:
		return 0, fmt.Errorf("unexpected job kind %q", kind)
	}

	outcome := legacyOutcomeStamped
	if cancelReason != "" {
		if err := client.CancelTx(ctx, tx, jobID); err != nil {
			return 0, fmt.Errorf("cancel (%s): %w", cancelReason, err)
		}
		outcome = legacyOutcomeCancelled
	} else if err := jobs.StampJobArg(ctx, tx, jobID, "operation_ref", ref); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return outcome, nil
}

func prepareLegacyNotification(ctx context.Context, tx pgx.Tx, gate sendingpolicy.Gate, nref sendingpolicy.NotificationRef) (sendingpolicy.OperationRef, string, error) {
	ref, err := gate.PrepareNotificationTx(ctx, tx, nref)
	if errors.Is(err, sendingpolicy.ErrSourceUnavailable) {
		return sendingpolicy.OperationRef{}, "legacy source unavailable", nil
	}
	if err != nil {
		return sendingpolicy.OperationRef{}, "", err
	}
	return ref, "", nil
}
