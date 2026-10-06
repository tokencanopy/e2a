package sendingpolicy

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// RecipientRegistration is a permanent, non-secret binding. Retiring a secret
// payload must never erase this evidence or permit reuse of its logical version.
type RecipientRegistration struct {
	Version         int    `json:"version"`
	CommitmentKeyID string `json:"commitment_key_id"`
	Commitment      string `json:"commitment"`
}

// KeyInventory is a read-only, consistent deployment snapshot. It contains no
// account/message identifiers, addresses, key material, or audit actors.
// Version sets are sorted and non-nil. SchemaVersion versions this CLI contract.
type KeyInventory struct {
	SchemaVersion             int                     `json:"schema_version"`
	PolicyGeneration          int64                   `json:"policy_generation"`
	PolicySHA256              string                  `json:"policy_sha256"`
	SelectedRecipientVersion  int                     `json:"selected_recipient_version"`
	RequiredHMACVersions      []int                   `json:"required_hmac_versions"`
	RequiredRecipientVersions []int                   `json:"required_recipient_versions"`
	RecipientRegistry         []RecipientRegistration `json:"recipient_registry"`
}

const inventoryLimit = 4096

// KeyInventory reads policy, permanent registry and unexpired feedback/notice
// references in one repeatable-read snapshot. Errors return no partial result.
// The snapshot does not fence concurrent writers: deploy gates must additionally
// retain keys and selectors that any still-running slot can use for new work.
// Normal operator-command startup migrations precede this read-only operation.
func (m *Module) KeyInventory(ctx context.Context) (KeyInventory, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return KeyInventory{}, errors.New("sendingpolicy: key inventory unavailable")
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		_ = tx.Rollback(cleanup)
	}()
	out, err := readKeyInventory(ctx, tx)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return KeyInventory{}, errors.New("sendingpolicy: key inventory unavailable")
	}
	return out, nil
}

func readKeyInventory(ctx context.Context, tx pgx.Tx) (KeyInventory, error) {
	snapshot, err := scanPolicy(tx.QueryRow(ctx, policySelect))
	if err != nil {
		return KeyInventory{}, err
	}
	out := KeyInventory{SchemaVersion: 1, PolicyGeneration: snapshot.Generation, PolicySHA256: snapshot.PolicySHA256, SelectedRecipientVersion: snapshot.Policy.OperatorNoticeRecipientVersion, RequiredHMACVersions: []int{}, RequiredRecipientVersions: []int{}, RecipientRegistry: []RecipientRegistration{}}
	rows, err := tx.Query(ctx, `SELECT logical_version,commitment_key_id,recipient_commitment FROM sending_operator_recipient_versions ORDER BY logical_version LIMIT 4097`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r RecipientRegistration
		if err := rows.Scan(&r.Version, &r.CommitmentKeyID, &r.Commitment); err != nil {
			rows.Close()
			return out, err
		}
		out.RecipientRegistry = append(out.RecipientRegistry, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.RecipientRegistry) > inventoryLimit {
		return out, errors.New("registry exceeds inventory bound")
	}
	// The same notice version column holds two different namespaces. Audience,
	// not purpose, determines which one. Keep even started attempts until expiry.
	// Current authorizations remain redeemable beyond nominal expiry (the same
	// conservative boundary as ledger retention). An orphaned notice reference is ambiguous and must block rather than vanish.
	rows, err = tx.Query(ctx, `
 SELECT DISTINCT 'hmac',r.hmac_key_version
 FROM sending_feedback_recipients r JOIN sending_feedback_correlations c USING(correlation_id)
 WHERE c.expires_at IS NULL OR c.expires_at>now()
 UNION
 SELECT DISTINCT CASE d.audience WHEN 'owner' THEN 'hmac' WHEN 'operator' THEN 'recipient' ELSE 'unknown' END,r.notice_recipient_version
 FROM sending_budget_reservations r
 LEFT JOIN sending_protection_notice_deliveries d ON d.current_operation_id=r.operation_id
 LEFT JOIN sending_provider_operations o ON o.operation_id=r.operation_id
 WHERE r.notice_recipient_version IS NOT NULL AND r.call_state IN ('authorized','started')
 AND (r.expires_at>now() OR (r.call_state='authorized' AND r.submission_attempt>=o.current_attempt))
 ORDER BY 1,2 LIMIT 4097`)
	if err != nil {
		return out, err
	}
	count := 0
	for rows.Next() {
		var kind string
		var version int
		if err := rows.Scan(&kind, &version); err != nil {
			rows.Close()
			return out, err
		}
		count++
		switch kind {
		case "hmac":
			out.RequiredHMACVersions = append(out.RequiredHMACVersions, version)
		case "recipient":
			out.RequiredRecipientVersions = append(out.RequiredRecipientVersions, version)
		default:
			rows.Close()
			return out, errors.New("unresolved notice key reference")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if count > inventoryLimit {
		return out, errors.New("references exceed inventory bound")
	}
	return out, nil
}
