package sendramp

import (
	"context"
	"time"
)

// sweepAccounts bounds historical daily buckets without forgetting earned
// trust. Unresolved reservations are never pruned. Ninety days exceeds the
// detector lookback; older clean-day evidence is folded into the account row.
// Each run is bounded to 100 accounts and 1,000 reservations/day buckets per account and skips accounts currently sending.
func (s *Store) sweepAccounts(ctx context.Context, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	cutoff := utcDay(now).AddDate(0, 0, -90)
	rows, err := tx.Query(ctx, `SELECT a.user_id FROM account_sending_trust a WHERE EXISTS (
 SELECT 1 FROM account_send_days d WHERE d.user_id=a.user_id AND d.day<$1
 AND NOT EXISTS(SELECT 1 FROM account_send_reservations r WHERE r.user_id=d.user_id AND r.day=d.day AND r.state='reserved'))
 ORDER BY a.user_id LIMIT 100 FOR UPDATE OF a SKIP LOCKED`, cutoff)
	if err != nil {
		return err
	}
	var users []string
	for rows.Next() {
		var user string
		if err = rows.Scan(&user); err != nil {
			rows.Close()
			return err
		}
		users = append(users, user)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, user := range users {
		if _, err = tx.Exec(ctx, `DELETE FROM account_send_reservations WHERE message_id IN (SELECT message_id FROM account_send_reservations WHERE user_id=$1 AND day<$2 AND state IN ('confirmed','released') ORDER BY day,message_id LIMIT 1000)`, user, cutoff); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `WITH pruned AS (
   DELETE FROM account_send_days WHERE user_id=$1 AND day IN (SELECT d.day FROM account_send_days d WHERE d.user_id=$1 AND d.day<$2
   AND NOT EXISTS(SELECT 1 FROM account_send_reservations r WHERE r.user_id=d.user_id AND r.day=d.day) ORDER BY d.day LIMIT 1000)
   RETURNING confirmed_count,breached)
   UPDATE account_sending_trust SET archived_clean_days=archived_clean_days+(SELECT count(*) FROM pruned WHERE confirmed_count>0 AND NOT breached) WHERE user_id=$1`, user, cutoff)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
