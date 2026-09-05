package jobs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is the one method StampJobArg needs; both a pool and a transaction
// satisfy it.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// StampJobArg adds one key to a River job's args, only when that key is
// absent, leaving every existing field in place.
//
// It exists for the sending-protection compatibility resolvers: a job
// enqueued by a pre-floor slot carries no operation reference, the worker
// derives one through the same Prepare path an enqueue uses, and stamping it
// here makes that derivation happen once per job rather than once per
// execution. Existing fields stay so an older worker can still read the job.
func StampJobArg(ctx context.Context, db Execer, jobID int64, key string, value any) error {
	if db == nil {
		return fmt.Errorf("stamp job arg: no database")
	}
	patch, err := json.Marshal(map[string]any{key: value})
	if err != nil {
		return fmt.Errorf("stamp job arg: encode %s: %w", key, err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE river_job SET args = args || $2::jsonb WHERE id = $1 AND NOT (args ? $3)`,
		jobID, string(patch), key,
	); err != nil {
		return fmt.Errorf("stamp job arg %s on job %d: %w", key, jobID, err)
	}
	return nil
}
