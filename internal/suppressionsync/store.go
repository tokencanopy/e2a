// Package suppressionsync owns the write side of the account suppression list
// that provider feedback drives.
//
// Slice B8 of the sending abuse prevention plan moves suppression upsert
// ownership here ahead of the provider-facing reconciliation (Task 11): every
// feedback-driven upsert advances the row's sync_generation and clears any
// pending removal, so a stale delete that raced the upsert loses. The race
// contract is fixed now, before a remote list exists, so Task 11 can add the
// SES-facing half without changing it.
//
// The package is a leaf over pgx: it never reads a message, an agent, or a
// user row, which is what lets the deletion-resistant feedback path call it
// after the message that caused the bounce is long gone.
package suppressionsync

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Source values match the suppressions.source CHECK constraint.
const (
	SourceBounce    = "bounce"
	SourceComplaint = "complaint"
	SourceManual    = "manual"
)

// Upsert is the outcome of one feedback-driven upsert.
type Upsert struct {
	ID         string
	Inserted   bool  // a new row; false when an existing row was refreshed
	Generation int64 // sync_generation after this write
}

// UpsertTx inserts the (user, address) suppression or, when it already
// exists, advances its sync_generation and clears removal_pending. The
// existing row's reason and source are kept: the first evidence wins, and a
// refresh must not rewrite a manual entry into a bounce.
//
// The address must already be normalized (lower-cased, trimmed); this
// package does not own address canonicalization.
func UpsertTx(ctx context.Context, tx pgx.Tx, id, userID, address, reason, source, sourceMessageID string) (Upsert, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(address) == "" {
		return Upsert{}, errors.New("suppressionsync: id, user and address are required")
	}
	var out Upsert
	var srcMsg *string
	if sourceMessageID != "" {
		srcMsg = &sourceMessageID
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO suppressions (id, user_id, address, reason, source, source_message_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, address) DO UPDATE
		    SET sync_generation = suppressions.sync_generation + 1,
		        removal_pending = false
		RETURNING id, (xmax = 0) AS inserted, sync_generation`,
		id, userID, address, reason, source, srcMsg,
	).Scan(&out.ID, &out.Inserted, &out.Generation); err != nil {
		return Upsert{}, fmt.Errorf("suppressionsync: upsert: %w", err)
	}
	return out, nil
}

// MarkRemovalPendingTx flags the row for removal and returns the generation
// the remover must present to CompleteRemovalTx. found=false when there is
// no such suppression.
func MarkRemovalPendingTx(ctx context.Context, tx pgx.Tx, userID, address string) (generation int64, found bool, err error) {
	err = tx.QueryRow(ctx, `
		UPDATE suppressions
		   SET removal_pending = true
		 WHERE user_id = $1 AND address = $2
		RETURNING sync_generation`, userID, address,
	).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("suppressionsync: mark removal pending: %w", err)
	}
	return generation, true, nil
}

// CompleteRemovalTx deletes the row only if it is still pending removal at
// the generation the remover observed. A feedback upsert in between advanced
// the generation and cleared the flag, so the stale delete removes nothing:
// the address stays suppressed, which is the only safe answer when the
// provider has just said it bounces.
func CompleteRemovalTx(ctx context.Context, tx pgx.Tx, userID, address string, generation int64) (removed bool, err error) {
	tag, err := tx.Exec(ctx, `
		DELETE FROM suppressions
		 WHERE user_id = $1 AND address = $2
		   AND removal_pending = true
		   AND sync_generation = $3`, userID, address, generation)
	if err != nil {
		return false, fmt.Errorf("suppressionsync: complete removal: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
