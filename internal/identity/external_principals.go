package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrAuthUnavailable classifies an authentication failure caused by a
// backend the credential could not be checked against (delegated
// verifier not ready, identity-store outage) rather than by the
// credential itself. Response layers must surface it as 503 with no
// WWW-Authenticate challenge — it says nothing about whether the
// credential was valid — while every other auth error stays a 401.
var ErrAuthUnavailable = errors.New("identity: authentication temporarily unavailable")

// ErrExternalPrincipalConflict is returned when an (issuer, subject)
// pair is already attached to a DIFFERENT user. Callers must surface it
// as a conflict — reconciliation is an operator decision, never an
// auto-merge.
var ErrExternalPrincipalConflict = errors.New("identity: external principal attached to another user")

// ErrExternalPrincipalUserNotFound is returned by attach when the named
// user does not exist.
var ErrExternalPrincipalUserNotFound = errors.New("identity: user not found for external principal")

// rowQuerier is the slice of pgxpool.Pool / pgx.Tx the mapping helpers
// need, so the same statements run standalone or inside the provisioning
// transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// GetUserByExternalPrincipal resolves a VERIFIED delegated (issuer,
// subject) pair to its local user with one exact read-only lookup.
// Returns (nil, nil) when no mapping exists — the caller's 401, kept
// distinct from a store failure (err != nil), the caller's 503. It never
// looks up by subject alone, matches by email, trusts token profile
// claims, or writes.
func (s *Store) GetUserByExternalPrincipal(ctx context.Context, issuer, subject string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT u.id, u.email, u.name, u.google_subject, u.created_at, u.account_class
		 FROM external_principal_mappings m
		 JOIN users u ON u.id = m.user_id
		 WHERE m.issuer = $1 AND m.subject = $2`, issuer, subject,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AccountClass)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// AttachExternalPrincipal idempotently maps (issuer, subject) to an
// EXISTING user. The same triple replays as created=false; a pair
// already attached to another user is ErrExternalPrincipalConflict; an
// unknown user is ErrExternalPrincipalUserNotFound. It never touches the
// user row itself (email, name, google_subject).
func (s *Store) AttachExternalPrincipal(ctx context.Context, issuer, subject, userID string) (bool, error) {
	return attachExternalPrincipal(ctx, s.pool, issuer, subject, userID)
}

func attachExternalPrincipal(ctx context.Context, q rowQuerier, issuer, subject, userID string) (bool, error) {
	var attachedTo string
	err := q.QueryRow(ctx,
		`INSERT INTO external_principal_mappings (issuer, subject, user_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (issuer, subject) DO NOTHING
		 RETURNING user_id`, issuer, subject, userID,
	).Scan(&attachedTo)
	if err == nil {
		return true, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23503" {
		// The only FK on this table is user_id -> users(id).
		return false, ErrExternalPrincipalUserNotFound
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	// ON CONFLICT DO NOTHING swallowed the insert: the pair exists. Same
	// user = idempotent replay; different user = conflict.
	if err := q.QueryRow(ctx,
		`SELECT user_id FROM external_principal_mappings WHERE issuer = $1 AND subject = $2`,
		issuer, subject,
	).Scan(&attachedTo); err != nil {
		return false, err
	}
	if attachedTo != userID {
		return false, ErrExternalPrincipalConflict
	}
	return false, nil
}

// provisionExternalPrincipalTx is the provisioning-path attach: inside
// the ProvisionUser transaction it inserts or replays the mapping for
// the user the provision resolved to. A pair held by a different user
// aborts the whole provision with ErrExternalPrincipalConflict — the
// control plane's issuer/subject view and the local mapping disagree,
// which is never auto-resolved.
func provisionExternalPrincipalTx(ctx context.Context, tx pgx.Tx, issuer, subject, userID string) error {
	if _, err := attachExternalPrincipal(ctx, tx, issuer, subject, userID); err != nil {
		return fmt.Errorf("attach external principal: %w", err)
	}
	return nil
}
