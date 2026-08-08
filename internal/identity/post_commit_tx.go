package identity

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// postCommitTx buffers in-memory side effects until the database transaction
// that makes them true has committed successfully. Nested pgx transactions
// are savepoints: releasing one transfers its callbacks to the parent, while
// rolling it back discards them. Only the outer commit runs callbacks.
//
// The wrapper is intentionally private to Store-owned transactions. A raw
// pgx.Tx passed to a Store *Tx method has no observable commit boundary, so
// transaction-dependent in-memory effects must not be registered on it.
type postCommitTx struct {
	pgx.Tx
	parent    *postCommitTx
	callbacks []func()
	closed    bool
}

func newPostCommitTx(tx pgx.Tx, parent *postCommitTx) *postCommitTx {
	return &postCommitTx{Tx: tx, parent: parent}
}

func (tx *postCommitTx) Begin(ctx context.Context) (pgx.Tx, error) {
	child, err := tx.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return newPostCommitTx(child, tx), nil
}

func (tx *postCommitTx) Commit(ctx context.Context) error {
	if err := tx.Tx.Commit(ctx); err != nil {
		tx.callbacks = nil
		tx.closed = true
		return err
	}
	callbacks := tx.callbacks
	tx.callbacks = nil
	tx.closed = true
	if tx.parent != nil {
		for _, callback := range callbacks {
			tx.parent.afterCommit(callback)
		}
		return nil
	}
	for _, callback := range callbacks {
		callback()
	}
	return nil
}

func (tx *postCommitTx) Rollback(ctx context.Context) error {
	tx.callbacks = nil
	tx.closed = true
	return tx.Tx.Rollback(ctx)
}

func (tx *postCommitTx) afterCommit(callback func()) {
	if callback == nil || tx.closed {
		return
	}
	tx.callbacks = append(tx.callbacks, callback)
}

type postCommitRegistrar interface {
	afterCommit(func())
}

func registerPostCommit(tx pgx.Tx, callback func()) bool {
	observed, ok := tx.(postCommitRegistrar)
	if !ok {
		return false
	}
	observed.afterCommit(callback)
	return true
}
