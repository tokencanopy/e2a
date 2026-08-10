package identity

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// These bounds cap every transaction in the permanent-agent purge. The
// classifier reads at most limit+1 rows under the agent lock, so its atomic
// path cannot hide unbounded cancellation or engagement work.
const (
	InlinePurgeMaxMessages    = 5000
	agentPurgeChunkRows       = 5000
	agentPurgeCancelChunkRows = 500
)

// agentPurgeDecisionTx locks the exact incarnation the request resolved and
// either leaves it on the bounded atomic path or durably claims it for a
// resumable chunked purge. An existing claim is always adopted.
func (s *Store) agentPurgeDecisionTx(
	ctx context.Context,
	tx pgx.Tx,
	agentID, userID string,
	createdAt time.Time,
) (token string, chunked bool, err error) {
	var purgeToken *string
	err = tx.QueryRow(ctx,
		`SELECT purge_token
		   FROM agent_identities
		  WHERE id = $1 AND user_id = $2 AND created_at = $3
		  FOR UPDATE`,
		agentID, userID, createdAt).Scan(&purgeToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrAgentNotFound
	}
	if err != nil {
		return "", false, err
	}
	if purgeToken != nil {
		return *purgeToken, true, nil
	}
	if err := ensureNoAgentSendInProgressTx(ctx, tx, agentID); err != nil {
		return "", false, err
	}

	tooManyMessages, err := rowsOverLimitTx(ctx, tx,
		`SELECT 1 FROM messages WHERE agent_id = $1 LIMIT $2`,
		agentID, InlinePurgeMaxMessages)
	if err != nil {
		return "", false, err
	}
	tooManyJobs := false
	tooManyEngagements := false
	if !tooManyMessages {
		// The parent lock blocks new FK inserts, but existing-message state
		// transitions do not all take a parent lock. Lock the bounded child set
		// before probing jobs so an approval cannot turn 501 held messages into
		// job-bearing sends after the classifier and reintroduce an unbounded
		// cancellation transaction.
		if err := lockAgentMessagesTx(ctx, tx, agentID); err != nil {
			return "", false, err
		}
		tooManyJobs, err = rowsOverLimitTx(ctx, tx,
			`SELECT 1 FROM messages
			  WHERE agent_id = $1
			    AND direction = 'outbound'
			    AND delivery_status IN ('accepted', 'sending')
			    AND send_job_id IS NOT NULL
			  LIMIT $2`, agentID, agentPurgeCancelChunkRows)
		if err != nil {
			return "", false, err
		}
		tooManyEngagements, err = rowsOverLimitTx(ctx, tx,
			`SELECT 1 FROM contact_engagements
			  WHERE user_id = $1 AND agent_id = $2
			  LIMIT $3`, userID, agentID, agentPurgeChunkRows)
		if err != nil {
			return "", false, err
		}
	}
	if !tooManyMessages && !tooManyJobs && !tooManyEngagements {
		return "", false, nil
	}

	token = "pur_" + generateID()
	err = tx.QueryRow(ctx,
		`UPDATE agent_identities
		    SET deleted_at = now(), purge_token = $3
		  WHERE id = $1 AND user_id = $2
		  RETURNING purge_token`, agentID, userID, token).Scan(&token)
	return token, true, err
}

func lockAgentMessagesTx(ctx context.Context, tx pgx.Tx, agentID string) error {
	rows, err := tx.Query(ctx,
		`SELECT id FROM messages WHERE agent_id = $1 FOR UPDATE`, agentID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func rowsOverLimitTx(ctx context.Context, tx pgx.Tx, query string, args ...any) (bool, error) {
	limit := args[len(args)-1].(int)
	args[len(args)-1] = limit + 1
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return n > limit, nil
}

func ensureNoAgentSendInProgressTx(ctx context.Context, tx pgx.Tx, agentID string) error {
	sending, err := agentSendInProgressTx(ctx, tx, agentID)
	if err != nil {
		return err
	}
	if sending {
		return ErrSendInProgress
	}
	return nil
}

func (s *Store) deleteAgentAtomicTx(ctx context.Context, tx pgx.Tx, agentID, userID string) (int64, error) {
	jobRows, err := tx.Query(ctx,
		`SELECT send_job_id
		   FROM messages
		  WHERE agent_id = $1
		    AND direction = 'outbound'
		    AND delivery_status IN ('accepted', 'sending')
		    AND send_job_id IS NOT NULL
		  FOR UPDATE`, agentID)
	if err != nil {
		return 0, err
	}
	jobIDs, err := scanOutboundJobIDs(jobRows)
	if err != nil {
		return 0, err
	}
	if err := s.cancelOutboundJobIDsTx(ctx, tx, jobIDs); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM messages WHERE agent_id = $1`, agentID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM contact_engagements WHERE user_id = $1 AND agent_id = $2`,
		userID, agentID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM agent_identities WHERE id = $1 AND user_id = $2`,
		agentID, userID); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) purgeAgentChunked(ctx context.Context, agentID, userID, token string) (messagesDeleted int64, err error) {
	defer func() {
		if err != nil {
			log.Printf("[agent-purge] claimed purge %s stopped after %d message(s): %v", token, messagesDeleted, err)
		}
	}()
	return s.drainAgentChunks(ctx, agentID, userID, token)
}

// drainAgentChunks deletes bounded committed prefixes and finishes with a
// sealing transaction. Every transaction matches the immutable purge token.
func (s *Store) drainAgentChunks(ctx context.Context, agentID, userID, token string) (int64, error) {
	deleted, _, err := s.drainAgentChunksResult(ctx, agentID, userID, token)
	return deleted, err
}

func (s *Store) drainAgentChunksResult(ctx context.Context, agentID, userID, token string) (int64, bool, error) {
	var messagesDeleted int64
	if token == "" {
		return 0, false, errPurgeTargetGone
	}

	targetGone, err := s.purgeChunkLoop(ctx, agentID, userID, token, &messagesDeleted,
		func(ctx context.Context, tx pgx.Tx) (int64, error) {
			return s.deleteMessageChunkTx(ctx, tx, agentID, agentPurgeCancelChunkRows, `
			 AND direction = 'outbound'
			 AND delivery_status IN ('accepted', 'sending')
			 AND send_job_id IS NOT NULL`)
		})
	if err != nil || targetGone {
		return messagesDeleted, false, err
	}

	targetGone, err = s.purgeChunkLoop(ctx, agentID, userID, token, &messagesDeleted,
		func(ctx context.Context, tx pgx.Tx) (int64, error) {
			return s.deleteMessageChunkTx(ctx, tx, agentID, agentPurgeChunkRows, `
			 AND NOT (direction = 'outbound'
			          AND delivery_status IN ('accepted', 'sending')
			          AND send_job_id IS NOT NULL)`)
		})
	if err != nil || targetGone {
		return messagesDeleted, false, err
	}

	var engagementsDeleted int64
	targetGone, err = s.purgeChunkLoop(ctx, agentID, userID, token, &engagementsDeleted,
		func(ctx context.Context, tx pgx.Tx) (int64, error) {
			tag, err := tx.Exec(ctx,
				`DELETE FROM contact_engagements
				  WHERE ctid IN (
				        SELECT ctid FROM contact_engagements
				         WHERE user_id = $1 AND agent_id = $2 LIMIT $3)`,
				userID, agentID, agentPurgeChunkRows)
			if err != nil {
				return 0, err
			}
			return tag.RowsAffected(), nil
		})
	if err != nil || targetGone {
		return messagesDeleted, false, err
	}

	for {
		sealed, gone, deleted, err := s.sealAgentPurge(ctx, agentID, userID, token)
		messagesDeleted += deleted
		if err != nil || gone {
			return messagesDeleted, false, err
		}
		if sealed {
			return messagesDeleted, true, nil
		}
		if err := ctx.Err(); err != nil {
			return messagesDeleted, false, err
		}
	}
}

// sealAgentPurge holds the agent lock from the final bounded probes through
// parent deletion. A late FK writer either commits before this lock and is
// drained here, or waits until the parent is gone and fails its FK check.
func (s *Store) sealAgentPurge(ctx context.Context, agentID, userID, token string) (sealed, targetGone bool, messagesDeleted int64, err error) {
	err = s.purgeAgentTx(ctx, agentID, userID, token, func(tx pgx.Tx) error {
		// Classify only after locking a bounded residual set. Existing-message
		// transitions can create a River job without locking the parent; this
		// lock makes their final state visible before we decide which jobs to
		// cancel and prevents the parent cascade from bypassing cancellation.
		messageIDs, err := lockResidualMessageChunkTx(ctx, tx, agentID, agentPurgeCancelChunkRows)
		if err != nil {
			return err
		}
		if len(messageIDs) > 0 {
			n, err := s.deleteLockedMessageChunkTx(ctx, tx, messageIDs)
			if err != nil {
				return err
			}
			messagesDeleted += n
			return nil
		}
		engagementIDs, err := lockResidualEngagementChunkTx(
			ctx, tx, userID, agentID, agentPurgeChunkRows)
		if err != nil {
			return err
		}
		if len(engagementIDs) > 0 {
			if _, err := tx.Exec(ctx,
				`DELETE FROM contact_engagements
				  WHERE user_id = $1 AND agent_id = $2 AND id = ANY($3)`,
				userID, agentID, engagementIDs); err != nil {
				return err
			}
			return nil
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM agent_identities
			  WHERE id = $1 AND user_id = $2 AND purge_token = $3`,
			agentID, userID, token); err != nil {
			return err
		}
		sealed = true
		return nil
	})
	if errors.Is(err, errPurgeTargetGone) {
		return false, true, 0, nil
	}
	if err != nil {
		// All deletes in this sealing attempt rolled back with the transaction;
		// do not add their provisional row count to the API receipt.
		return false, false, 0, err
	}
	return sealed, false, messagesDeleted, nil
}

func lockResidualEngagementChunkTx(
	ctx context.Context,
	tx pgx.Tx,
	userID, agentID string,
	limit int,
) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id FROM contact_engagements
		  WHERE user_id = $1 AND agent_id = $2
		  LIMIT $3 FOR UPDATE`, userID, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func lockResidualMessageChunkTx(ctx context.Context, tx pgx.Tx, agentID string, limit int) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id FROM messages WHERE agent_id = $1 LIMIT $2 FOR UPDATE`,
		agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) deleteLockedMessageChunkTx(ctx context.Context, tx pgx.Tx, messageIDs []string) (int64, error) {
	rows, err := tx.Query(ctx,
		`DELETE FROM messages WHERE id = ANY($1)
		 RETURNING CASE
		             WHEN direction = 'outbound'
		              AND delivery_status IN ('accepted', 'sending')
		             THEN send_job_id
		           END`, messageIDs)
	if err != nil {
		return 0, err
	}
	var deleted int64
	var jobIDs []int64
	for rows.Next() {
		var jobID *int64
		if err := rows.Scan(&jobID); err != nil {
			rows.Close()
			return 0, err
		}
		deleted++
		if jobID != nil {
			jobIDs = append(jobIDs, *jobID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := s.cancelOutboundJobIDsTx(ctx, tx, jobIDs); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (s *Store) deleteMessageChunkTx(ctx context.Context, tx pgx.Tx, agentID string, limit int, extraPredicate string) (int64, error) {
	rows, err := tx.Query(ctx,
		`DELETE FROM messages
		  WHERE ctid IN (
		        SELECT ctid FROM messages
		         WHERE agent_id = $1`+extraPredicate+`
		         LIMIT $2)
		 RETURNING CASE
		             WHEN direction = 'outbound'
		              AND delivery_status IN ('accepted', 'sending')
		             THEN send_job_id
		           END`, agentID, limit)
	if err != nil {
		return 0, err
	}
	var deleted int64
	var jobIDs []int64
	for rows.Next() {
		var jobID *int64
		if err := rows.Scan(&jobID); err != nil {
			rows.Close()
			return 0, err
		}
		deleted++
		if jobID != nil {
			jobIDs = append(jobIDs, *jobID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := s.cancelOutboundJobIDsTx(ctx, tx, jobIDs); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (s *Store) purgeChunkLoop(
	ctx context.Context,
	agentID, userID, token string,
	total *int64,
	chunk func(context.Context, pgx.Tx) (int64, error),
) (targetGone bool, err error) {
	for {
		var affected int64
		err := s.purgeAgentTx(ctx, agentID, userID, token, func(tx pgx.Tx) error {
			n, err := chunk(ctx, tx)
			affected = n
			return err
		})
		if errors.Is(err, errPurgeTargetGone) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		*total += affected
		if affected == 0 {
			return false, nil
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
}

func (s *Store) purgeAgentTx(ctx context.Context, agentID, userID, token string, body func(pgx.Tx) error) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var lockedID string
		err := tx.QueryRow(ctx,
			`SELECT id FROM agent_identities
			  WHERE id = $1 AND user_id = $2 AND purge_token = $3
			  FOR UPDATE`, agentID, userID, token).Scan(&lockedID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errPurgeTargetGone
		}
		if err != nil {
			return err
		}
		if err := ensureNoAgentSendInProgressTx(ctx, tx, agentID); err != nil {
			return err
		}
		return body(tx)
	})
}

var errPurgeTargetGone = errors.New("identity: purge target no longer exists")

func agentSendInProgressTx(ctx context.Context, tx pgx.Tx, agentID string) (bool, error) {
	var sending bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM messages
			 WHERE agent_id = $1 AND delivery_status = 'sending'
			   AND send_claimed_at > now() - make_interval(secs => $2)
		)`, agentID, int64(OutboundSendClaimStaleWindow/time.Second)).Scan(&sending)
	return sending, err
}
