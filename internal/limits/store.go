package limits

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps account_limits row access. Writes are intended for
// external provisioners (the hosted billing sidecar, admin tooling);
// the OSS server itself only reads.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Get returns the user's row from account_limits. Returns (Limits{},
// pgx.ErrNoRows, false) when no row exists so callers can apply
// operator-configured defaults — distinguishing "no row" from "real DB
// error" is important: a transient DB hiccup must fail closed, while a
// genuinely-missing row is the normal case for a fresh user.
func (s *Store) Get(ctx context.Context, userID string) (Limits, bool, error) {
	l := Limits{}
	err := s.pool.QueryRow(ctx,
		`SELECT plan_code, max_agents, max_domains, max_messages_month, max_storage_bytes, upgrade_url, outbound_footer_enabled
		   FROM account_limits WHERE user_id = $1`, userID,
	).Scan(&l.PlanCode, &l.MaxAgents, &l.MaxDomains, &l.MaxMessagesMonth, &l.MaxStorageBytes, &l.UpgradeURL, &l.OutboundFooterEnabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Limits{}, false, nil
		}
		return Limits{}, false, err
	}
	return l, true, nil
}

// Upsert writes/overwrites the user's row. Only used by external
// provisioners; not invoked from any OSS request path. Exposed here so
// the future internal-limits-invalidate endpoint can also serve as a
// minimal "set limits" API for the sidecar without that code reaching
// into the table directly.
func (s *Store) Upsert(ctx context.Context, userID string, l Limits) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO account_limits
		    (user_id, plan_code, max_agents, max_domains, max_messages_month, max_storage_bytes, upgrade_url, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		 ON CONFLICT (user_id) DO UPDATE SET
		    plan_code          = EXCLUDED.plan_code,
		    max_agents         = EXCLUDED.max_agents,
		    max_domains        = EXCLUDED.max_domains,
		    max_messages_month = EXCLUDED.max_messages_month,
		    max_storage_bytes  = EXCLUDED.max_storage_bytes,
		    upgrade_url        = EXCLUDED.upgrade_url,
		    updated_at         = now()`,
		userID, l.PlanCode, l.MaxAgents, l.MaxDomains, l.MaxMessagesMonth, l.MaxStorageBytes, l.UpgradeURL,
	)
	return err
}
