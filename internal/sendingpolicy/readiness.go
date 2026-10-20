package sendingpolicy

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// CheckReadiness validates database-source policy and its selected permanent
// recipient commitment against this process's trust roots in one read-only
// snapshot. It never repairs or registers rows. Config-source deployments have
// no dependency on those rows. The caller supplies its dedicated connection so
// readiness does not compete for the application's shared connection pool.
func (m *Module) CheckReadiness(ctx context.Context, conn *pgx.Conn) error {
	if m.source == PolicySourceConfig {
		return nil
	}
	if m.source != PolicySourceDatabase {
		return errors.New("sendingpolicy: invalid readiness policy source")
	}
	if m.secrets.Keyring == nil || m.secrets.Recipients == nil {
		return errors.New("sendingpolicy: database policy requires both trust roots")
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	snapshot, err := scanPolicy(tx.QueryRow(ctx, policySelect))
	if err != nil {
		return err
	}
	if err = m.checkSelectedOperatorRecipient(ctx, tx, snapshot.Policy.OperatorNoticeRecipientVersion, ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
