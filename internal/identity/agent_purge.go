package identity

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// InlinePurgeMaxMessages is the inbox size at or below which a permanent agent
// delete runs as ONE transaction — cascade, agent row gone, address freed,
// fully atomic — exactly as it always has. Above it the same work happens in
// the same request, but split into bounded transactions that each commit
// (see purgeAgentChunked).
//
// The threshold therefore selects ATOMIC vs CHUNKED, not inline vs deferred:
// the delete never leaves the request path. An earlier revision of this change
// handed oversized inboxes to a River job; that was dropped because the job's
// only target was the agent id, which IS the email address, and addresses are
// reusable — a retried or stale job could destroy a different, later inbox at
// the same address (docs/design/2026-08-09-async-agent-purge.md, "Revision").
// Chunking is the part that carried the weight anyway.
//
// It is a compile-time constant on purpose, not a config key: a knob nobody
// sets is a knob everybody has to reason about. Add one when a second
// environment genuinely needs a different value.
//
// Measured on local Postgres 16, one cascade child populated
// (message_lifecycle_transitions, 3 rows per message). Single-statement
// DELETE FROM messages WHERE agent_id = $1:
//
//	 1,000 →     79ms
//	 5,000 →    303ms
//	20,000 →  2,935ms
//	50,000 → 15,048ms
//
// The cost is SUPERLINEAR — 50x the rows costs 190x the time — which is the
// whole argument for a threshold: the cheap case is very cheap and the
// expensive case degrades faster than volume. The same 50,000-message inbox
// chunked at 5,000 with one transaction per chunk takes ~4.7s in total with a
// longest single transaction of 599ms, against 15,048ms single-shot: ~3x
// faster overall and ~25x shorter on the metric that actually matters (how
// long one pooled connection and one set of row locks are held).
//
// Local hardware is faster than the deployed database tier and
// only one of the seven cascade tables carried data, so treat every number as
// a lower bound. 5,000 lands at ~300ms atomic, the same order as the existing
// expiredDeleteBatch and comfortably inside a request; the next step up
// (20,000) is already ~3s locally. If the threshold moves it should fall.
const InlinePurgeMaxMessages = 5000

// agentPurgeChunkRows bounds one message-drain transaction. Same 5,000 as the
// webhook retention sweep, for the same reason: keep each statement's lock
// footprint and WAL batch small. Deliberately equal to InlinePurgeMaxMessages —
// a purge that just misses the atomic threshold should cost about one atomic
// delete per chunk.
const agentPurgeChunkRows = 5000

// agentPurgeCancelChunkRows bounds the transaction that drains messages with a
// live durable send job attached. It is an order of magnitude smaller than
// agentPurgeChunkRows because those rows cost far more than a delete: each one
// also costs a River JobCancelTx round trip. At 5,000 a single outreach agent
// with a full scheduled-send queue would reintroduce exactly the multi-second
// transaction this change exists to remove; at 500 the cancel work per
// transaction is bounded to roughly the same budget as one 5,000-row delete.
const agentPurgeCancelChunkRows = 500

// countAgentMessages is the threshold read. It is an index-only scan on
// idx_messages_agent_created, milliseconds even at 10^5, and it runs OUTSIDE
// any transaction so a huge inbox costs one short pooled read before
// DeleteAgent decides which shape to take.
func (s *Store) countAgentMessages(ctx context.Context, agentID string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE agent_id = $1`, agentID).Scan(&n)
	return n, err
}

// purgeAgentChunked is DeleteAgent's above-threshold shape: the same removal,
// synchronously, in bounded transactions that each COMMIT. It returns the
// number of message rows removed, summed across chunks — the exact
// RowsAffected total, not the threshold pre-count, so the API's
// messages_deleted means precisely what it always meant.
//
// ATOMICITY IS DELIBERATELY GIVEN UP HERE, and only here. At or below the
// threshold DeleteAgent still runs one transaction and a failure leaves the
// inbox untouched. Above it a mid-flight failure leaves a TRASHED, PARTIALLY
// DRAINED inbox: the caller sees an error, the agent is in the trash, and some
// of its mail is already gone. Restoring such an inbox returns a gutted
// mailbox. You cannot have both atomicity and bounded transactions, and
// bounded transactions are what stop one tenant's delete from taking the
// service's readiness with it. There is deliberately no marker column for the
// half-drained state; instead the failure is logged loudly with the agent and
// the rows removed so far, and re-issuing the delete finishes the job (every
// step is "delete what remains", so a retry resumes from the committed
// prefix).
//
// Committing between chunks is the entire point — a loop inside one
// transaction holds the same connection, the same locks and the same snapshot
// for the same total time as the single statement, and provides none of the
// benefit.
func (s *Store) purgeAgentChunked(ctx context.Context, agentID, userID string) (messagesDeleted int64, err error) {
	// Claim first: authorize the delete, refuse a send in flight, and mark the
	// agent deleted — all in one short transaction. The mark is what every
	// later chunk re-checks, and it also stops the inbox resolving (no new
	// mail, no new sends) for the duration of the drain.
	if err := s.claimAgentForChunkedPurge(ctx, agentID, userID); err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			log.Printf("[agent-purge] PARTIAL: chunked permanent delete of %s aborted after %d message(s) — "+
				"the inbox is in the trash and partially drained; re-issue the delete to finish it: %v",
				agentID, messagesDeleted, err)
		}
	}()
	return s.drainAgentChunks(ctx, agentID, userID)
}

// claimAgentForChunkedPurge is the chunked path's prologue. It mirrors the
// atomic path's opening exactly — same ownership lock, same ErrAgentNotFound /
// ErrSendInProgress terms — and adds the deleted_at mark.
//
// The mark uses COALESCE(deleted_at, now()): permanently deleting an agent
// that is ALREADY in the trash must not rewrite its trash timestamp. Callers
// can see deleted_at, and the janitor's retention sweep keys on it — a
// permanent delete that silently reset the clock would push the backstop purge
// 30 days further out, which is the opposite of what was asked for.
func (s *Store) claimAgentForChunkedPurge(ctx context.Context, agentID, userID string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var lockedID string
		err := tx.QueryRow(ctx,
			`SELECT id FROM agent_identities WHERE id = $1 AND user_id = $2 FOR UPDATE`,
			agentID, userID).Scan(&lockedID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentNotFound
		}
		if err != nil {
			return err
		}
		sending, err := agentSendInProgressTx(ctx, tx, agentID)
		if err != nil {
			return err
		}
		if sending {
			return ErrSendInProgress
		}
		_, err = tx.Exec(ctx,
			`UPDATE agent_identities SET deleted_at = COALESCE(deleted_at, now()) WHERE id = $1`,
			agentID)
		return err
	})
}

// drainAgentChunks removes everything the atomic delete's single transaction
// would, in committed chunks, and finally the agent row itself. Split out from
// purgeAgentChunked so it can be driven directly by tests that need to
// interrupt it, race a restore against it, or resume it.
//
// Idempotent by construction: every step is "delete what remains", so an
// interrupted attempt simply continues where it stopped. An agent row that is
// already gone is success (a previous attempt finished, or the janitor got
// there first).
//
// Two guards run in EVERY chunk transaction rather than once up front:
//
//   - the agent is still this user's AND still marked deleted, re-read under
//     FOR UPDATE. This is the data-loss guard. RestoreAgent clears deleted_at,
//     and a drain that kept deleting a restored inbox because it only checked
//     at the start would destroy live mail. Re-checking per chunk costs one
//     indexed row read against a 5,000-row delete and bounds the damage of any
//     restore that lands mid-drain to zero further chunks. The user_id half
//     closes the same ABA hole that killed the deferred design, now narrowed
//     to a single request: if the janitor hard-deleted this row and another
//     account claimed the freed address, the next chunk finds no row it owns
//     and stops instead of eating the newcomer's mail.
//   - ErrSendInProgress. A send lease can be taken between chunks, so the
//     prologue's check is stale the moment it commits.
func (s *Store) drainAgentChunks(ctx context.Context, agentID, userID string) (int64, error) {
	var messagesDeleted int64

	// Queued and in-flight sends first, in small transactions. These rows are
	// the expensive ones — each carries a durable River job that has to be
	// cancelled in the same transaction that removes its message, or the job
	// would later fire against nothing. Draining them here means the bulk loop
	// below finds (essentially) none left, so its 5,000-row chunks stay pure
	// deletes.
	if err := s.purgeChunkLoop(ctx, agentID, userID, &messagesDeleted,
		func(ctx context.Context, tx pgx.Tx) (int64, error) {
			return s.deleteMessageChunkTx(ctx, tx, agentID, agentPurgeCancelChunkRows, `
			 AND direction = 'outbound'
			 AND delivery_status IN ('accepted', 'sending')
			 AND send_job_id IS NOT NULL`)
		}); err != nil {
		return messagesDeleted, err
	}

	// Then the rest of the history. The ctid subselect is the local chunking
	// idiom (webhook retention sweep): it picks an arbitrary physical batch
	// with no sort and no offset, so cost stays flat as the table drains
	// instead of degrading with each pass. The cancel handling stays wired up
	// as a safety net — by construction the loop above left nothing for it,
	// and if that construction is ever wrong a stray job still gets cancelled
	// rather than orphaned.
	if err := s.purgeChunkLoop(ctx, agentID, userID, &messagesDeleted,
		func(ctx context.Context, tx pgx.Tx) (int64, error) {
			return s.deleteMessageChunkTx(ctx, tx, agentID, agentPurgeChunkRows, "")
		}); err != nil {
		return messagesDeleted, err
	}

	// Outreach state dies with a permanent delete while suppressions survive
	// (consent outlives the inbox) — the same asymmetry the atomic path's
	// explicit deletes encode. contact_engagements has no FK to
	// agent_identities, so nothing cascades it; leaving rows behind would hand
	// a recreated agent at the same address last campaign's stage and a
	// past-due schedule.
	var engagementsDeleted int64
	if err := s.purgeChunkLoop(ctx, agentID, userID, &engagementsDeleted,
		func(ctx context.Context, tx pgx.Tx) (int64, error) {
			tag, err := tx.Exec(ctx,
				`DELETE FROM contact_engagements
				  WHERE ctid IN (SELECT ctid FROM contact_engagements WHERE agent_id = $1 LIMIT $2)`,
				agentID, agentPurgeChunkRows)
			if err != nil {
				return 0, err
			}
			return tag.RowsAffected(), nil
		}); err != nil {
		return messagesDeleted, err
	}

	// The agent row last. By now it cascades over nothing, so deleting it is a
	// single-row write that frees the address.
	if err := s.purgeAgentTx(ctx, agentID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM agent_identities WHERE id = $1`, agentID)
		return err
	}); err != nil {
		return messagesDeleted, err
	}
	return messagesDeleted, nil
}

// deleteMessageChunkTx removes up to limit of the agent's messages inside the
// caller's transaction and cancels the durable send job of every row it took
// that still had one. extraPredicate narrows which messages are eligible (it
// is a fragment appended to the subselect's WHERE — callers pass constants,
// never user input).
//
// The cancel rides RETURNING rather than a separate SELECT ... FOR UPDATE pass
// so the two can never disagree: the rows whose jobs are cancelled are exactly
// the rows this statement deleted, in the same transaction, so there is no
// window in which a send job outlives its message or a message outlives its
// cancellation.
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
		           END`,
		agentID, limit)
	if err != nil {
		return 0, err
	}
	var (
		deleted int64
		jobIDs  []int64
	)
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
	// Only now that the result set is drained is the connection free for the
	// cancellation statements.
	if err := s.cancelOutboundJobIDsTx(ctx, tx, jobIDs); err != nil {
		return 0, err
	}
	return deleted, nil
}

// purgeChunkLoop runs one chunk body repeatedly, one transaction per chunk,
// until a pass removes nothing. Each pass re-applies the purge guards.
func (s *Store) purgeChunkLoop(
	ctx context.Context,
	agentID, userID string,
	total *int64,
	chunk func(ctx context.Context, tx pgx.Tx) (int64, error),
) error {
	for {
		var affected int64
		if err := s.purgeAgentTx(ctx, agentID, userID, func(tx pgx.Tx) error {
			n, err := chunk(ctx, tx)
			if err != nil {
				return err
			}
			affected = n
			return nil
		}); err != nil {
			return err
		}
		*total += affected
		if affected == 0 {
			return nil
		}
		// A context cancellation between chunks (client hang-up, shutdown)
		// stops here rather than starting a chunk that cannot finish. The
		// committed prefix stands and a re-issued delete resumes from it.
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// purgeAgentTx runs one guarded purge transaction: lock the caller's agent row,
// verify it is still marked deleted and has no send in flight, then run body. A
// missing (or no-longer-ours) agent row means there is nothing left this
// request may touch — reported as errPurgeComplete so callers stop cleanly with
// the rows they did remove.
func (s *Store) purgeAgentTx(ctx context.Context, agentID, userID string, body func(tx pgx.Tx) error) error {
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var deletedAt *time.Time
		err := tx.QueryRow(ctx,
			`SELECT deleted_at FROM agent_identities WHERE id = $1 AND user_id = $2 FOR UPDATE`,
			agentID, userID).Scan(&deletedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return errPurgeComplete
		}
		if err != nil {
			return err
		}
		if deletedAt == nil {
			// Restored between chunks. Abort: everything still here is live
			// mail again.
			return ErrNotInTrash
		}
		sending, err := agentSendInProgressTx(ctx, tx, agentID)
		if err != nil {
			return err
		}
		if sending {
			return ErrSendInProgress
		}
		return body(tx)
	})
	if errors.Is(err, errPurgeComplete) {
		return nil
	}
	return err
}

// errPurgeComplete is the internal "nothing left to do" signal from
// purgeAgentTx. Never escapes drainAgentChunks.
var errPurgeComplete = errors.New("identity: agent already purged")

// agentSendInProgressTx reports whether the agent holds a fresh outbound send
// lease. Shared by the atomic delete, the chunked delete's prologue and every
// chunk so the guard cannot drift between them.
func agentSendInProgressTx(ctx context.Context, tx pgx.Tx, agentID string) (bool, error) {
	var sending bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM messages
			 WHERE agent_id = $1 AND delivery_status = 'sending'
			   AND send_claimed_at > now() - make_interval(secs => $2)
		)`,
		agentID, int64(OutboundSendClaimStaleWindow/time.Second),
	).Scan(&sending)
	return sending, err
}
