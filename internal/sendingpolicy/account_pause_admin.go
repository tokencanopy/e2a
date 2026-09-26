package sendingpolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pause classes (account_sending_controls.pause_class, migration 122). The
// class records WHY an account is paused; `abuse` is the one with a
// consequence beyond sending: purging an account paused for abuse writes
// long-lived abuse tombstones and keeps its deleted-account summary for the
// abuse hold (docs/design/account-soft-deletion.md §4.5).
const (
	PauseClassOperator = "operator"
	PauseClassAbuse    = "abuse"
	PauseClassBilling  = "billing"
	PauseClassSystem   = "system"
)

// ValidPauseClass reports whether c is a known pause class.
func ValidPauseClass(c string) bool {
	switch c {
	case PauseClassOperator, PauseClassAbuse, PauseClassBilling, PauseClassSystem:
		return true
	}
	return false
}

const maxEvidenceRefLength = 200

// AccountPauseChange is an operator pause or resume.
type AccountPauseChange struct {
	AccountID string
	Paused    bool
	// Class is required for a pause; ignored for a resume (which resets it to
	// operator).
	Class string
	// EvidenceRef is an optional private reference (e.g. an incident id)
	// carried into the deleted-account summary. Never customer-visible.
	EvidenceRef string
	Actor       string
	Reason      string
}

// AccountPauseRecord is the operator readback of an account's pause state.
type AccountPauseRecord struct {
	AccountID     string
	State         string
	PauseClass    string
	Reason        string
	EvidenceRef   string
	AccountStatus string // live | trashed
	UpdatedAt     time.Time
}

// SetAccountPause pauses or resumes an account's sending, recording the class,
// the reason and an audit event. It deliberately works on accounts in ANY
// trash state: pausing a trashed account for abuse is the lever that makes its
// eventual purge write abuse tombstones ("paused, then they deleted"). A
// pause never touches external-sending approval, and a resume starts a new
// detector epoch.
//
// Lock order follows the gate's: users (FOR SHARE), then the control row.
func (m *Module) SetAccountPause(ctx context.Context, req AccountPauseChange) (AccountPauseRecord, error) {
	req.AccountID = strings.TrimSpace(req.AccountID)
	if req.AccountID == "" {
		return AccountPauseRecord{}, ErrAccountNotFound
	}
	if strings.TrimSpace(req.Actor) == "" {
		return AccountPauseRecord{}, errors.New("sendingpolicy: a pause change requires an actor")
	}
	if strings.TrimSpace(req.Reason) == "" || len([]rune(req.Reason)) > maxOperatorReasonLength {
		return AccountPauseRecord{}, fmt.Errorf("sendingpolicy: a pause change requires a nonblank reason of at most %d characters", maxOperatorReasonLength)
	}
	req.EvidenceRef = strings.TrimSpace(req.EvidenceRef)
	if len([]rune(req.EvidenceRef)) > maxEvidenceRefLength {
		return AccountPauseRecord{}, fmt.Errorf("sendingpolicy: an evidence reference is at most %d characters", maxEvidenceRefLength)
	}
	class := PauseClassOperator
	if req.Paused {
		class = strings.TrimSpace(req.Class)
		if !ValidPauseClass(class) {
			return AccountPauseRecord{}, fmt.Errorf("sendingpolicy: pause class must be one of operator, abuse, billing, system (got %q)", req.Class)
		}
	} else if req.EvidenceRef != "" {
		return AccountPauseRecord{}, errors.New("sendingpolicy: a resume takes no evidence reference")
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return AccountPauseRecord{}, fmt.Errorf("sendingpolicy: begin pause change: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	policy, err := m.effectivePolicy(ctx, tx)
	if err != nil {
		return AccountPauseRecord{}, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1 FOR SHARE`, req.AccountID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AccountPauseRecord{}, ErrAccountNotFound
		}
		return AccountPauseRecord{}, fmt.Errorf("sendingpolicy: lock user: %w", err)
	}
	var oldState string
	if err := tx.QueryRow(ctx, `
		INSERT INTO account_sending_controls (user_id)
		VALUES ($1)
		ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id
		RETURNING state`, req.AccountID,
	).Scan(&oldState); err != nil {
		return AccountPauseRecord{}, fmt.Errorf("sendingpolicy: lock account control: %w", err)
	}
	newState := "active"
	if req.Paused {
		newState = "paused"
	}
	var evidence *string
	if req.EvidenceRef != "" {
		evidence = &req.EvidenceRef
	}
	if req.Paused {
		if _, err := tx.Exec(ctx, `
			UPDATE account_sending_controls
			   SET state = 'paused', pause_class = $2, reason = $3, actor = $4,
			       evidence_ref = COALESCE($5, evidence_ref), updated_at = now()
			 WHERE user_id = $1`, req.AccountID, class, req.Reason, req.Actor, evidence,
		); err != nil {
			return AccountPauseRecord{}, fmt.Errorf("sendingpolicy: write pause: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE account_sending_controls
			   SET state = 'active', pause_class = 'operator', reason = $2, actor = $3,
			       evidence_ref = NULL,
			       outcome_epoch = CASE WHEN state = 'paused' THEN outcome_epoch + 1 ELSE outcome_epoch END,
			       last_resumed_at = CASE WHEN state = 'paused' THEN now() ELSE last_resumed_at END,
			       updated_at = now()
			 WHERE user_id = $1`, req.AccountID, req.Reason, req.Actor,
		); err != nil {
			return AccountPauseRecord{}, fmt.Errorf("sendingpolicy: write resume: %w", err)
		}
	}
	retention := time.Duration(policy.SendingControlAuditRetentionDays) * 24 * time.Hour
	if retention <= 0 {
		retention = 365 * 24 * time.Hour
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_sending_control_events
		    (id, account_ref, old_state, new_state, reason, actor, pause_class, evidence_ref, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + $9::interval)`,
		randomID("asce_"), req.AccountID, oldState, newState, req.Reason, req.Actor, class, evidence,
		fmt.Sprintf("%d seconds", int64(retention.Seconds())),
	); err != nil {
		return AccountPauseRecord{}, fmt.Errorf("sendingpolicy: record pause event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountPauseRecord{}, fmt.Errorf("sendingpolicy: commit pause change: %w", err)
	}
	return m.InspectAccountPause(ctx, req.AccountID)
}

// InspectAccountPause reads an account's pause state (any trash state).
func (m *Module) InspectAccountPause(ctx context.Context, accountID string) (AccountPauseRecord, error) {
	rec := AccountPauseRecord{AccountID: accountID}
	var trashed bool
	var evidence *string
	var updated *time.Time
	err := m.pool.QueryRow(ctx, `
		SELECT u.deleted_at IS NOT NULL,
		       COALESCE(c.state, 'active'), COALESCE(c.pause_class, 'operator'),
		       COALESCE(c.reason, ''), c.evidence_ref, c.updated_at
		  FROM users u LEFT JOIN account_sending_controls c ON c.user_id = u.id
		 WHERE u.id = $1`, accountID,
	).Scan(&trashed, &rec.State, &rec.PauseClass, &rec.Reason, &evidence, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, ErrAccountNotFound
	}
	if err != nil {
		return rec, fmt.Errorf("sendingpolicy: inspect pause: %w", err)
	}
	rec.AccountStatus = "live"
	if trashed {
		rec.AccountStatus = "trashed"
	}
	if evidence != nil {
		rec.EvidenceRef = *evidence
	}
	if updated != nil {
		rec.UpdatedAt = *updated
	}
	return rec, nil
}
