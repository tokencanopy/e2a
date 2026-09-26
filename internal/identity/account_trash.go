package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Account soft deletion (docs/design/account-soft-deletion.md §4.3–§4.6).
//
// DELETE /v1/account moves the whole account to the trash: the user row gets
// a deleted_at stamp, every live agent is trashed with trashed_by_account,
// every credential is revoked and every owned domain is unverified. Nothing
// is written to account_sending_controls — the sending gate denies a trashed
// account through its own users.deleted_at predicate, so an existing pause
// and its class survive both the trash and a restore untouched. After the
// retention window the janitor purges the account (PurgeDeletedUsers):
// tombstones and the deleted-account summary first, then the agents in
// bounded chunks, then the user row.

// AccountTrashRetention is how long a trashed account stays restorable before
// the janitor purges it. cmd/e2a assigns it at startup from
// trash.account_retention_days (default: trash.retention_days). Zero opts the
// deployment out of account trash: DELETE /v1/account then erases at once.
var AccountTrashRetention = 30 * 24 * time.Hour

// AccountTrashEnabled reports whether DELETE /v1/account trashes (true) or
// erases immediately (false, trash.account_retention_days: 0).
func AccountTrashEnabled() bool { return AccountTrashRetention > 0 }

// Receipt modes.
const (
	AccountDeleteModeTrash     = "trash"
	AccountDeleteModePermanent = "permanent"
)

// userColumns is the column list every User loader scans, in scanUser order.
const userColumns = `id, email, name, google_subject, created_at, account_class,
	acquisition_answered_at, deleted_at, restored_at, purge_token IS NOT NULL`

func scanUser(row pgx.Row) (*User, error) {
	u := &User{}
	if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt, &u.AccountClass,
		&u.AcquisitionAnsweredAt, &u.DeletedAt, &u.RestoredAt, &u.PurgeClaimed); err != nil {
		return nil, err
	}
	return u, nil
}

// PurgeAfter is when a trashed account becomes eligible for the janitor
// purge; nil for a live account.
func (u *User) PurgeAfter() *time.Time {
	if u == nil || u.DeletedAt == nil {
		return nil
	}
	t := u.DeletedAt.Add(AccountTrashRetention)
	return &t
}

// GetUserByIDAnyState loads a user whatever its trash state. It is for the
// callers that must see a trashed owner: the restore interstitial, the data
// export, operator commands and the notifiers delivering notices that were
// already queued. Authentication must use GetUserByID, which excludes trashed
// accounts.
func (s *Store) GetUserByIDAnyState(ctx context.Context, id string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// accountRowCounts fills the per-table counts a receipt reports.
func accountRowCounts(ctx context.Context, tx pgx.Tx, userID string, res *DeleteUserDataResult) error {
	queries := []struct {
		dst   *int64
		query string
	}{
		{&res.SessionsDeleted, `SELECT count(*) FROM user_sessions WHERE user_id = $1`},
		{&res.DomainsDeleted, `SELECT count(*) FROM domains WHERE user_id = $1`},
		{&res.AgentsDeleted, `SELECT count(*) FROM agent_identities WHERE user_id = $1`},
		{&res.APIKeysDeleted, `SELECT count(*) FROM api_keys WHERE user_id = $1`},
		{&res.MessagesDeleted, `SELECT count(*) FROM messages m JOIN agent_identities a ON a.id = m.agent_id WHERE a.user_id = $1`},
		{&res.UsageEventsDeleted, `SELECT count(*) FROM usage_events WHERE user_id = $1`},
		{&res.UsageSummariesDeleted, `SELECT count(*) FROM usage_summaries WHERE user_id = $1`},
		{&res.AgentSuppressionsDeleted, `SELECT count(*) FROM agent_suppressions WHERE user_id = $1`},
		{&res.AgentUnsubscribeTokensDeleted, `SELECT count(*) FROM agent_unsubscribe_tokens WHERE user_id = $1`},
	}
	for _, q := range queries {
		if err := tx.QueryRow(ctx, q.query, userID).Scan(q.dst); err != nil {
			return fmt.Errorf("delete: count: %w", err)
		}
	}
	return nil
}

// TrashAccount moves a live account to the trash in one transaction and
// returns the trash receipt (mode "trash"; messages_deleted is 0, the other
// counts describe rows trashed, revoked or unverified). perDomainInTx, when
// non-nil, runs for every owned domain inside the transaction — it is how the
// SES sender-identity teardown is enqueued; with the domain now unverified the
// deprovision worker deletes the provider identity.
//
// Refuses with ErrSendInProgress while an outbound provider call holds a
// fresh lease, ErrAccountTrashed when the account is already in the trash,
// and pgx.ErrNoRows when it does not exist.
func (s *Store) TrashAccount(ctx context.Context, userID string, perDomainInTx func(ctx context.Context, tx pgx.Tx, domain string) error) (*DeleteUserDataResult, error) {
	res := &DeleteUserDataResult{Mode: AccountDeleteModeTrash}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var deletedAt *time.Time
		// FOR NO KEY UPDATE on the parent: the restore and purge paths take the
		// same lock, and it coexists with the FK key-share locks concurrent
		// child inserts hold (AGENTS.md row-lock rule).
		if err := tx.QueryRow(ctx,
			`SELECT deleted_at FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID,
		).Scan(&deletedAt); err != nil {
			return err
		}
		if deletedAt != nil {
			return ErrAccountTrashed
		}
		if err := lockAccountAgentsTx(ctx, tx, userID); err != nil {
			return err
		}
		if err := ensureNoAccountSendInProgressTx(ctx, tx, userID); err != nil {
			return err
		}

		var now time.Time
		if err := tx.QueryRow(ctx,
			`UPDATE users SET deleted_at = now() WHERE id = $1 RETURNING deleted_at`, userID,
		).Scan(&now); err != nil {
			return fmt.Errorf("trash: user: %w", err)
		}
		purgeAfter := now.Add(AccountTrashRetention)
		res.PurgeAfter = &purgeAfter

		tag, err := tx.Exec(ctx,
			`UPDATE agent_identities SET deleted_at = $2, trashed_by_account = true
			  WHERE user_id = $1 AND deleted_at IS NULL`, userID, now)
		if err != nil {
			return fmt.Errorf("trash: agents: %w", err)
		}
		res.AgentsDeleted = tag.RowsAffected()
		// Kill switch for agent identity assertions and access tokens: bump
		// every agent's assertion_version so outstanding tokens are stale at
		// once, and stay stale after a restore.
		if _, err := tx.Exec(ctx,
			`UPDATE agent_identities SET assertion_version = COALESCE(assertion_version, 1) + 1 WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("trash: agent assertion versions: %w", err)
		}

		tag, err = tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id = $1`, userID)
		if err != nil {
			return fmt.Errorf("trash: sessions: %w", err)
		}
		res.SessionsDeleted = tag.RowsAffected()

		tag, err = tx.Exec(ctx,
			`UPDATE api_keys SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
		if err != nil {
			return fmt.Errorf("trash: api keys: %w", err)
		}
		res.APIKeysDeleted = tag.RowsAffected()

		// OAuth grants: access and refresh tokens are revoked, pending codes
		// deactivated. The bearer check also fails through GetUserByID's
		// trashed predicate; revoking makes the refusal survive a restore.
		tag, err = tx.Exec(ctx,
			`UPDATE oauth_access_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
		if err != nil {
			return fmt.Errorf("trash: oauth access tokens: %w", err)
		}
		res.OAuthAccessTokensDeleted = tag.RowsAffected()
		tag, err = tx.Exec(ctx,
			`UPDATE oauth_refresh_tokens SET revoked_at = now(), active = false WHERE user_id = $1 AND revoked_at IS NULL`, userID)
		if err != nil {
			return fmt.Errorf("trash: oauth refresh tokens: %w", err)
		}
		res.OAuthRefreshTokensDeleted = tag.RowsAffected()
		tag, err = tx.Exec(ctx,
			`UPDATE oauth_auth_codes SET active = false WHERE user_id = $1 AND active`, userID)
		if err != nil {
			return fmt.Errorf("trash: oauth codes: %w", err)
		}
		res.OAuthAuthCodesDeleted = tag.RowsAffected()

		// Domains stay (agents reference them, and the names stay held as
		// domain_taken for the window) but lose every verification: the
		// sending axis resets and the verification token rotates, so a TXT
		// record still published cannot silently re-verify a trashed account.
		// verified_at is kept as the "ever verified" fact the abuse tombstone
		// and the deleted-account summary read.
		domains, err := scanDomainsForUser(ctx, tx, userID)
		if err != nil {
			return fmt.Errorf("trash: load domains: %w", err)
		}
		for _, d := range domains {
			// A domain another account's agents live on (the shared domain an
			// operator's probe account adopted) is infrastructure, not this
			// account's identity: unverifying it would take every other
			// account's inboxes offline. Leave it exactly as it is.
			var shared bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM agent_identities WHERE registered_domain = $1 AND user_id <> $2)`,
				d.Domain, userID).Scan(&shared); err != nil {
				return fmt.Errorf("trash: check domain %s: %w", d.Domain, err)
			}
			if shared {
				log.Printf("[account-trash] left domain %s verified: other accounts' agents depend on it (user=%s)", d.Domain, userID)
				continue
			}
			if _, err := tx.Exec(ctx, `
				UPDATE domains
				   SET verified = false,
				       verification_token = $2,
				       last_checked_at = NULL,
				       sending_status = 'none',
				       sending_error = NULL,
				       sending_dns_records = NULL,
				       sending_last_checked_at = NULL,
				       sending_dkim_status = NULL,
				       sending_mail_from_status = NULL
				 WHERE domain = $1 AND user_id = $3`,
				d.Domain, "e2a-verify="+generateID(), userID); err != nil {
				return fmt.Errorf("trash: unverify domain %s: %w", d.Domain, err)
			}
			if perDomainInTx != nil {
				if err := perDomainInTx(ctx, tx, d.Domain); err != nil {
					return fmt.Errorf("trash: enqueue sender teardown for %s: %w", d.Domain, err)
				}
			}
		}
		res.DomainsDeleted = int64(len(domains))
		if s.accountStateHook != nil {
			if err := s.accountStateHook(ctx, tx, userID, AccountDeleteModeTrash); err != nil {
				return fmt.Errorf("trash: account-state notice: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// lockAccountAgentsTx locks every agent of the account in id order, matching
// the outbound worker's agent-then-message claim order. NO KEY UPDATE: the
// trash only updates non-key columns, and it must coexist with FK key shares.
func lockAccountAgentsTx(ctx context.Context, tx pgx.Tx, userID string) error {
	rows, err := tx.Query(ctx,
		`SELECT id FROM agent_identities WHERE user_id = $1 ORDER BY id FOR NO KEY UPDATE`, userID)
	if err != nil {
		return fmt.Errorf("lock agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("lock agents: %w", err)
		}
	}
	return rows.Err()
}

func ensureNoAccountSendInProgressTx(ctx context.Context, tx pgx.Tx, userID string) error {
	var sending bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1
			  FROM messages m
			  JOIN agent_identities a ON a.id = m.agent_id
			 WHERE a.user_id = $1
			   AND m.delivery_status = 'sending'
			   AND m.send_claimed_at > now() - make_interval(secs => $2)
		)`,
		userID, int64(OutboundSendClaimStaleWindow/time.Second),
	).Scan(&sending); err != nil {
		return fmt.Errorf("check active sends: %w", err)
	}
	if sending {
		return ErrSendInProgress
	}
	return nil
}

// RestoreAccount brings a trashed account back: it clears deleted_at, stamps
// restored_at and revives exactly the agents the trash took
// (trashed_by_account), giving their pending review holds back the time spent
// in the trash. It deliberately leaves everything else as the trash left it:
// API keys stay revoked, domains stay unverified, and account_sending_controls
// is untouched, so a pause (and its class) survives the restore.
//
// sessionToken, when non-empty, is the restricted session the restore was
// requested through; it is upgraded to an ordinary session in the same
// transaction so the dashboard continues without a second sign-in.
//
// Refuses with ErrNotInTrash (live account), ErrPurgeInProgress (a purge has
// committed its claim), ErrRegistrationRefused (an abuse hold closes one of
// the account's identifiers), and pgx.ErrNoRows (no such account).
func (s *Store) RestoreAccount(ctx context.Context, userID, sessionToken string) (*User, error) {
	var restored *User
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var (
			deletedAt  *time.Time
			purgeToken *string
			email      string
			subject    string
			tooSoon    bool
		)
		if err := tx.QueryRow(ctx,
			`SELECT deleted_at, purge_token, email, google_subject,
			        COALESCE(restored_at > now() - make_interval(secs => $2), false)
			   FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID, RestoreCooldown.Seconds(),
		).Scan(&deletedAt, &purgeToken, &email, &subject, &tooSoon); err != nil {
			return err
		}
		if deletedAt == nil {
			return ErrNotInTrash
		}
		if purgeToken != nil {
			return ErrPurgeInProgress
		}
		if tooSoon {
			return ErrRestoreRateLimited
		}
		// An identity closed by a live tombstone (an operator hold, or an
		// earlier abuse purge of the same identifiers) stays closed.
		ids, err := accountLoginIdentifiersTx(ctx, tx, userID, email, subject)
		if err != nil {
			return err
		}
		if err := s.checkTombstonesTx(ctx, tx, ids); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE users SET deleted_at = NULL, restored_at = now() WHERE id = $1`, userID); err != nil {
			return fmt.Errorf("restore: user: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE messages m
			    SET approval_expires_at = m.approval_expires_at + (now() - $2::timestamptz)
			   FROM agent_identities a
			  WHERE a.id = m.agent_id AND a.user_id = $1 AND a.trashed_by_account
			    AND m.deleted_at IS NULL AND m.status = 'pending_review'
			    AND m.approval_expires_at IS NOT NULL`, userID, *deletedAt); err != nil {
			return fmt.Errorf("restore: holds: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE agent_identities SET deleted_at = NULL, trashed_by_account = false
			  WHERE user_id = $1 AND trashed_by_account AND purge_token IS NULL`, userID); err != nil {
			return fmt.Errorf("restore: agents: %w", err)
		}
		if sessionToken != "" {
			if _, err := tx.Exec(ctx,
				`UPDATE user_sessions SET restricted = false, expires_at = now() + make_interval(secs => $3)
				  WHERE token = $1 AND user_id = $2`,
				sessionToken, userID, SessionTTL.Seconds()); err != nil {
				return fmt.Errorf("restore: session: %w", err)
			}
		}
		if s.accountStateHook != nil {
			if err := s.accountStateHook(ctx, tx, userID, "restore"); err != nil {
				return fmt.Errorf("restore: account-state notice: %w", err)
			}
		}
		u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, userID))
		if err != nil {
			return err
		}
		restored = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return restored, nil
}

// accountLoginIdentifiersTx lists the account's login identifiers: its login
// subject, every delegated external principal, and its email.
func accountLoginIdentifiersTx(ctx context.Context, tx pgx.Tx, userID, email, subject string) ([]TombstoneIdentifier, error) {
	ids := []TombstoneIdentifier{
		{Kind: TombstoneKindLoginSubject, Value: subject},
		{Kind: TombstoneKindEmail, Value: email},
	}
	rows, err := tx.Query(ctx,
		`SELECT issuer, subject FROM external_principal_mappings WHERE user_id = $1 ORDER BY issuer, subject`, userID)
	if err != nil {
		return nil, fmt.Errorf("load external principals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var issuer, sub string
		if err := rows.Scan(&issuer, &sub); err != nil {
			return nil, err
		}
		ids = append(ids, TombstoneIdentifier{Kind: TombstoneKindLoginSubject, Value: externalPrincipalSubject(issuer, sub)})
	}
	return ids, rows.Err()
}

// EraseAccount is DELETE /v1/account?permanent=true (and the restricted
// session's "erase now"): it trashes a live account first (with every trash
// precondition) and then runs the purge immediately — tombstones and the
// summary before any row is deleted, never skipping them. The receipt is mode
// "permanent" with the real counts. An already-trashed account is purged
// directly.
//
// With tombstones enabled and the key unavailable it refuses with
// ErrTombstoneKeyUnavailable and leaves the account trashed.
//
// A paused account (any pause class) cannot be erased on demand: it refuses
// with ErrEraseHeld so an operator can classify the pause before the content
// goes (the account can still be trashed; the janitor purges it after the
// window, writing abuse tombstones when the history says abuse).
func (s *Store) EraseAccount(ctx context.Context, userID string, perDomainInTx func(ctx context.Context, tx pgx.Tx, domain string) error) (*DeleteUserDataResult, error) {
	res := &DeleteUserDataResult{Mode: AccountDeleteModePermanent}
	// Fast path with no side effects; the authoritative check runs under the
	// user lock in the purge claim (a pause racing in after this read leaves
	// the account trashed, not erased).
	if held, err := s.AccountSendingPaused(ctx, userID); err != nil {
		return nil, err
	} else if held {
		return nil, ErrEraseHeld
	}
	if err := s.WithTx(ctx, func(tx pgx.Tx) error { return accountRowCounts(ctx, tx, userID, res) }); err != nil {
		return nil, err
	}
	u, err := s.GetUserByIDAnyState(ctx, userID)
	if err != nil {
		return nil, err
	}
	if s.tombstones.Enabled && s.tombstones.Keyring == nil {
		return nil, ErrTombstoneKeyUnavailable
	}
	if u.DeletedAt == nil {
		if _, err := s.TrashAccount(ctx, userID, perDomainInTx); err != nil && !errors.Is(err, ErrAccountTrashed) {
			return nil, err
		}
	}
	purged, err := s.purgeAccount(ctx, userID, true, perDomainInTx)
	if err != nil {
		return nil, err
	}
	res.UserDeleted = purged
	return res, nil
}

// ErrEraseHeld refuses an on-demand permanent erasure (of an account or one
// of its agents) while the account's sending is paused.
var ErrEraseHeld = errors.New("identity: permanent erasure is held while the account is paused")

// ErrRestoreRateLimited refuses a restore within RestoreCooldown of the
// previous one.
var ErrRestoreRateLimited = errors.New("identity: the account was restored too recently")

// RestoreCooldown bounds trash/restore churn: one restore per account per
// window.
var RestoreCooldown = 10 * time.Minute

// AccountSendingPaused reports whether the account's sending is paused (any
// pause class). A missing control row is not paused.
func (s *Store) AccountSendingPaused(ctx context.Context, userID string) (bool, error) {
	var paused bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM account_sending_controls WHERE user_id = $1 AND state = 'paused')`, userID,
	).Scan(&paused)
	return paused, err
}

// userPurgeBatch bounds one janitor pass of PurgeDeletedUsers.
var userPurgeBatch = 20

// PurgeDeletedUsers purges trashed accounts past AccountTrashRetention, and
// resumes any purge that already committed its claim. It returns the ids of
// the accounts fully purged in this pass. An account whose tombstones cannot
// be written (key unavailable) is skipped with a logged error: its content
// stays trashed, never purged without tombstones. It must run BEFORE
// PurgeDeletedAgents in a sweep.
func (s *Store) PurgeDeletedUsers(ctx context.Context, perDomainInTx func(ctx context.Context, tx pgx.Tx, domain string) error) ([]string, error) {
	var purged []string
	var errs []error
	attempted := []string{}
	for i := 0; i < userPurgeBatch; i++ {
		var userID string
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM users
			  WHERE deleted_at IS NOT NULL
			    AND (purge_token IS NOT NULL OR deleted_at <= now() - make_interval(secs => $1))
			    AND NOT (id = ANY($2::text[]))
			  ORDER BY deleted_at
			  LIMIT 1`,
			AccountTrashRetention.Seconds(), attempted).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			errs = append(errs, err)
			break
		}
		attempted = append(attempted, userID)
		ok, err := s.purgeAccount(ctx, userID, false, perDomainInTx)
		if errors.Is(err, ErrNotInTrash) {
			continue // restored (or re-dated) between selection and claim
		}
		if err != nil {
			if errors.Is(err, ErrTombstoneKeyUnavailable) {
				log.Printf("[janitor] account purge skipped: user=%s err=%v (content stays trashed; configure E2A_TOMBSTONE_KEY)", userID, err)
			}
			errs = append(errs, fmt.Errorf("purge user %s: %w", userID, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if ok {
			purged = append(purged, userID)
		}
	}
	return purged, errors.Join(errs...)
}

// purgeAccount runs the resumable account purge. force skips the retention
// check (permanent erasure). It returns true when the user row is gone.
func (s *Store) purgeAccount(ctx context.Context, userID string, force bool, perDomainInTx func(ctx context.Context, tx pgx.Tx, domain string) error) (bool, error) {
	type agentClaim struct{ id, token string }
	var (
		token  string
		agents []agentClaim
		gone   bool
	)
	// Claim: lock the user, re-check the trash, stamp the purge token, write
	// the tombstones and the summary, and claim every agent — all before any
	// row is deleted, in one transaction.
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var (
			deletedAt  *time.Time
			purgeToken *string
			expired    bool
		)
		// The retention check is evaluated by the database clock, the same
		// clock PurgeDeletedUsers selected with, so app/DB skew cannot make
		// a selected account refuse its own claim.
		err := tx.QueryRow(ctx,
			`SELECT deleted_at, purge_token,
			        COALESCE(deleted_at <= now() - make_interval(secs => $2), false)
			   FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID, AccountTrashRetention.Seconds(),
		).Scan(&deletedAt, &purgeToken, &expired)
		if errors.Is(err, pgx.ErrNoRows) {
			gone = true
			return nil
		}
		if err != nil {
			return err
		}
		if deletedAt == nil {
			return ErrNotInTrash // a restore won the race
		}
		if purgeToken == nil && !force && !expired {
			return ErrNotInTrash
		}
		// On-demand erasure (force) re-checks the pause under the user lock:
		// SetAccountPause takes users FOR SHARE, so a pause cannot commit
		// between this read and the claim. The janitor's purge after the
		// window is not held (it writes abuse tombstones when warranted).
		if force && purgeToken == nil {
			var paused bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM account_sending_controls WHERE user_id = $1 AND state = 'paused')`, userID,
			).Scan(&paused); err != nil {
				return err
			}
			if paused {
				return ErrEraseHeld
			}
		}
		if purgeToken != nil {
			token = *purgeToken
		} else {
			token = "pur_" + generateID()
			if _, err := tx.Exec(ctx, `UPDATE users SET purge_token = $2 WHERE id = $1`, userID, token); err != nil {
				return fmt.Errorf("purge: claim user: %w", err)
			}
		}
		if err := s.writePurgeRecordsTx(ctx, tx, userID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx,
			`SELECT id, purge_token FROM agent_identities WHERE user_id = $1 ORDER BY id FOR NO KEY UPDATE`, userID)
		if err != nil {
			return fmt.Errorf("purge: lock agents: %w", err)
		}
		var pending []agentClaim
		for rows.Next() {
			var id string
			var tok *string
			if err := rows.Scan(&id, &tok); err != nil {
				rows.Close()
				return err
			}
			c := agentClaim{id: id}
			if tok != nil {
				c.token = *tok
			}
			pending = append(pending, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range pending {
			if pending[i].token == "" {
				pending[i].token = "pur_" + generateID()
				if _, err := tx.Exec(ctx,
					`UPDATE agent_identities
					    SET deleted_at = COALESCE(deleted_at, now()), purge_token = $2
					  WHERE id = $1`, pending[i].id, pending[i].token); err != nil {
					return fmt.Errorf("purge: claim agent: %w", err)
				}
			}
		}
		agents = pending
		return nil
	})
	if err != nil || gone {
		return gone, err
	}

	// Drain each agent with the established bounded-chunk machinery.
	for _, a := range agents {
		if _, _, err := s.drainAgentChunksResult(ctx, a.id, userID, a.token); err != nil {
			return false, fmt.Errorf("purge: drain agent: %w", err)
		}
	}

	// Seal: the remaining account rows and the user itself.
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		var locked string
		err := tx.QueryRow(ctx,
			`SELECT id FROM users WHERE id = $1 AND purge_token = $2 FOR NO KEY UPDATE`, userID, token,
		).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			gone = true
			return nil
		}
		if err != nil {
			return err
		}
		var residual bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM agent_identities WHERE user_id = $1)`, userID,
		).Scan(&residual); err != nil {
			return err
		}
		if residual {
			return errors.New("purge: agents remain after drain; will resume")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM usage_events WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("purge: usage_events: %w", err)
		}
		// A domain other accounts' agents live on (the shared domain a probe
		// account adopted) cannot cascade away with this user — the agents'
		// FK is ON DELETE NO ACTION and the purge would wedge forever. Hand it
		// back to the platform (unowned, as EnsureSharedDomain seeds it) and
		// leave its provider identity alone.
		if _, err := tx.Exec(ctx, `
			UPDATE domains SET user_id = NULL
			 WHERE user_id = $1
			   AND EXISTS (SELECT 1 FROM agent_identities a
			                WHERE a.registered_domain = domains.domain AND a.user_id <> $1)`, userID); err != nil {
			return fmt.Errorf("purge: release shared domains: %w", err)
		}
		domains, err := scanDomainsForUser(ctx, tx, userID)
		if err != nil {
			return fmt.Errorf("purge: load domains: %w", err)
		}
		if perDomainInTx != nil {
			for _, d := range domains {
				if err := perDomainInTx(ctx, tx, d.Domain); err != nil {
					return fmt.Errorf("purge: enqueue sender teardown for %s: %w", d.Domain, err)
				}
			}
		}
		tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1 AND purge_token = $2`, userID, token)
		if err != nil {
			return fmt.Errorf("purge: users: %w", err)
		}
		if perDomainInTx != nil {
			for _, d := range domains {
				if err := s.TouchSendingIdentityTombstoneTx(ctx, tx, d.Domain); err != nil {
					return fmt.Errorf("purge: restamp sender teardown for %s: %w", d.Domain, err)
				}
			}
		}
		gone = tag.RowsAffected() == 1
		return nil
	})
	return gone, err
}

// writePurgeRecordsTx writes the identity tombstones and the deleted-account
// summary for an account that is being purged. With tombstones disabled it
// writes nothing. It runs in the claim transaction, before any deletion.
func (s *Store) writePurgeRecordsTx(ctx context.Context, tx pgx.Tx, userID string) error {
	p := s.tombstones
	if !p.Enabled {
		return nil
	}
	if p.Keyring == nil {
		return ErrTombstoneKeyUnavailable
	}
	var (
		email, subject, accountClass string
		createdAt, deletedAt         time.Time
		controlState, pauseClass     *string
		evidenceRef                  *string
		abuseHistory                 bool
	)
	// Abuse is derived from history, not only the current row: an account
	// that was ever paused as abuse (the control audit remembers it) keeps the
	// abuse hold even if it was later resumed or re-paused under another class.
	if err := tx.QueryRow(ctx, `
		SELECT u.email, u.google_subject, u.account_class, u.created_at, u.deleted_at,
		       c.state, c.pause_class, c.evidence_ref,
		       EXISTS (SELECT 1 FROM account_sending_control_events e
		                WHERE e.account_ref = u.id AND e.pause_class = 'abuse')
		  FROM users u
		  LEFT JOIN account_sending_controls c ON c.user_id = u.id
		 WHERE u.id = $1`, userID,
	).Scan(&email, &subject, &accountClass, &createdAt, &deletedAt,
		&controlState, &pauseClass, &evidenceRef, &abuseHistory); err != nil {
		return fmt.Errorf("purge: load account: %w", err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return err
	}
	abuse := abuseHistory || (pauseClass != nil && *pauseClass == "abuse")

	ids, err := accountLoginIdentifiersTx(ctx, tx, userID, email, subject)
	if err != nil {
		return err
	}
	recentUntil := now.Add(RecentDeletionHold(now))
	if _, err := writeTombstonesTx(ctx, tx, p.Keyring, ids, TombstoneClassRecentDeletion, userID, recentUntil); err != nil {
		return err
	}
	verified, err := everVerifiedDomainsTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	abuseIDs, err := abuseIdentifiersTx(ctx, tx, userID, email, ids, verified)
	if err != nil {
		return err
	}
	retentionClass, summaryUntil := TombstoneClassRecentDeletion, recentUntil
	if abuse {
		abuseUntil := now.Add(AbuseTombstoneHold)
		if _, err := writeTombstonesTx(ctx, tx, p.Keyring, abuseIDs, TombstoneClassAbuse, userID, abuseUntil); err != nil {
			return err
		}
		retentionClass, summaryUntil = TombstoneClassAbuse, abuseUntil
	}

	sum, err := buildAccountSummaryTx(ctx, tx, p.Keyring, userID, verified)
	if err != nil {
		return err
	}
	// Every identifier an abuse hold would close, recorded as keyed digests so
	// an operator can escalate after purge (EscalateDeletedAccountToAbuse).
	identityDigests := make([]IdentityDigest, 0, len(abuseIDs))
	seen := map[string]bool{}
	for _, id := range abuseIDs {
		if strings.TrimSpace(id.Value) == "" {
			continue
		}
		d := p.Keyring.HexDigest(id.Kind, NormalizeTombstoneValue(id.Kind, id.Value))
		if seen[id.Kind+d] {
			continue
		}
		seen[id.Kind+d] = true
		identityDigests = append(identityDigests, IdentityDigest{Kind: id.Kind, Digest: d, KeyVersion: p.Keyring.ActiveVersion()})
	}
	identityJSON, err := json.Marshal(identityDigests)
	if err != nil {
		return err
	}
	// The free-text pause reason is deliberately NOT copied: it is operator
	// prose that may quote the customer. evidence_ref is the durable pointer.
	if _, err := tx.Exec(ctx, `
		INSERT INTO deleted_account_summaries (
		    account_ref, account_created_at, deleted_at, purged_at, account_class,
		    sending_state, pause_class, evidence_ref,
		    agents_count, messages_count, outbound_sends_count, first_outbound_at, last_outbound_at,
		    recipient_domains, daily_sends, subject_digests, verified_domain_digests, identity_digests,
		    digest_key_version, retention_class, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (account_ref) DO NOTHING`,
		userID, createdAt, deletedAt, now, accountClass,
		controlState, pauseClass, evidenceRef,
		sum.agents, sum.messages, sum.outbound, sum.firstOutbound, sum.lastOutbound,
		sum.recipientDomains, sum.dailySends, sum.subjectDigests, sum.domainDigests, identityJSON,
		p.Keyring.ActiveVersion(), retentionClass, summaryUntil,
	); err != nil {
		return fmt.Errorf("purge: write summary: %w", err)
	}
	return nil
}

// abuseIdentifiersTx is everything an abuse hold closes: the login
// identifiers, every domain the account ever verified, every agent address it
// held on a domain it did not own (the shared platform domain), and the owner
// email's domain unless it is a public webmail provider.
func abuseIdentifiersTx(ctx context.Context, tx pgx.Tx, userID, email string, login []TombstoneIdentifier, verified []string) ([]TombstoneIdentifier, error) {
	ids := append([]TombstoneIdentifier{}, login...)
	for _, d := range verified {
		ids = append(ids, TombstoneIdentifier{Kind: TombstoneKindDomain, Value: d})
	}
	if d := emailDomainOf(email); d != "" && !IsPublicWebmailDomain(d) {
		ids = append(ids, TombstoneIdentifier{Kind: TombstoneKindEmailDomain, Value: d})
	}
	rows, err := tx.Query(ctx, `
		SELECT a.id FROM agent_identities a
		  LEFT JOIN domains d ON d.domain = a.registered_domain
		 WHERE a.user_id = $1 AND d.user_id IS DISTINCT FROM $1
		 ORDER BY a.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("purge: load shared-domain agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, err
		}
		ids = append(ids, TombstoneIdentifier{Kind: TombstoneKindAgentAddress, Value: addr})
	}
	return ids, rows.Err()
}

func everVerifiedDomainsTx(ctx context.Context, tx pgx.Tx, userID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT domain FROM domains
		  WHERE user_id = $1 AND (verified OR verified_at IS NOT NULL)
		    -- a shared domain other accounts live on is never this account's identity
		    AND NOT EXISTS (SELECT 1 FROM agent_identities a
		                     WHERE a.registered_domain = domains.domain AND a.user_id <> $1)
		  ORDER BY domain`, userID)
	if err != nil {
		return nil, fmt.Errorf("purge: load verified domains: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Summary bounds.
const (
	summaryTopRecipientDomains = 20
	summaryTopSubjects         = 20
	summaryDailyWindowDays     = 30
)

// sentOutboundStatuses are the outbound delivery states that mean the message
// left (or was handed to a provider); accepted/sending/failed never did.
const sentOutboundStatuses = `('queued','sent','delivered','bounced','complained','deferred')`

type accountSummary struct {
	agents                       int
	messages, outbound           int64
	firstOutbound, lastOutbound  *time.Time
	recipientDomains, dailySends []byte
	subjectDigests               []byte
	domainDigests                []byte
}

// DomainCount is one bucket of the recipient-domain histogram.
type DomainCount struct {
	Domain string `json:"domain"`
	Count  int64  `json:"count"`
}

// DayCount is one day of the per-day send series (UTC).
type DayCount struct {
	Day   string `json:"day"`
	Count int64  `json:"count"`
}

// DigestCount is one keyed subject digest and how many sends carried it.
type DigestCount struct {
	Digest string `json:"digest"`
	Count  int64  `json:"count"`
}

func buildAccountSummaryTx(ctx context.Context, tx pgx.Tx, kr *TombstoneKeyring, userID string, verifiedDomains []string) (*accountSummary, error) {
	sum := &accountSummary{}
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM agent_identities WHERE user_id = $1`, userID).Scan(&sum.agents); err != nil {
		return nil, fmt.Errorf("summary: agents: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE m.direction = 'outbound' AND m.delivery_status IN `+sentOutboundStatuses+`),
		       min(m.created_at) FILTER (WHERE m.direction = 'outbound' AND m.delivery_status IN `+sentOutboundStatuses+`),
		       max(m.created_at) FILTER (WHERE m.direction = 'outbound' AND m.delivery_status IN `+sentOutboundStatuses+`)
		  FROM messages m JOIN agent_identities a ON a.id = m.agent_id
		 WHERE a.user_id = $1`, userID,
	).Scan(&sum.messages, &sum.outbound, &sum.firstOutbound, &sum.lastOutbound); err != nil {
		return nil, fmt.Errorf("summary: messages: %w", err)
	}
	// usage_events survive until purge even when the owner permanently deleted
	// messages first (and exist whenever usage tracking is on), so they are
	// the durable send record: take the larger count and the wider span.
	var usageSends int64
	var usageFirst, usageLast *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT count(*), min(created_at), max(created_at)
		  FROM usage_events WHERE user_id = $1 AND direction = 'outbound'`, userID,
	).Scan(&usageSends, &usageFirst, &usageLast); err != nil {
		return nil, fmt.Errorf("summary: usage events: %w", err)
	}
	if usageSends > sum.outbound {
		sum.outbound = usageSends
	}
	if usageFirst != nil && (sum.firstOutbound == nil || usageFirst.Before(*sum.firstOutbound)) {
		sum.firstOutbound = usageFirst
	}
	if usageLast != nil && (sum.lastOutbound == nil || usageLast.After(*sum.lastOutbound)) {
		sum.lastOutbound = usageLast
	}

	// Recipient domains: the domain part only — the local part never leaves
	// this query.
	domains := []DomainCount{}
	rows, err := tx.Query(ctx, `
		SELECT d, count(*) AS n FROM (
		    SELECT lower(substring(r FROM '@([^@<>[:space:]]+)[>[:space:]]*$')) AS d
		      FROM messages m
		      JOIN agent_identities a ON a.id = m.agent_id,
		           unnest(COALESCE(m.to_recipients, '{}') || COALESCE(m.cc, '{}') || COALESCE(m.bcc, '{}')) AS r
		     WHERE a.user_id = $1 AND m.direction = 'outbound'
		       AND m.delivery_status IN `+sentOutboundStatuses+`
		) x
		 WHERE d IS NOT NULL AND d <> ''
		 GROUP BY d ORDER BY n DESC, d
		 LIMIT $2`, userID, summaryTopRecipientDomains)
	if err != nil {
		return nil, fmt.Errorf("summary: recipient domains: %w", err)
	}
	for rows.Next() {
		var dc DomainCount
		if err := rows.Scan(&dc.Domain, &dc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		domains = append(domains, dc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	days := []DayCount{}
	// Per-day counts: the larger of the two records for each day.
	rows, err = tx.Query(ctx, `
		SELECT day, max(n) FROM (
		    SELECT to_char((m.created_at AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD') AS day, count(*) AS n
		      FROM messages m JOIN agent_identities a ON a.id = m.agent_id
		     WHERE a.user_id = $1 AND m.direction = 'outbound'
		       AND m.delivery_status IN `+sentOutboundStatuses+`
		       AND m.created_at >= now() - make_interval(days => $2)
		     GROUP BY 1
		    UNION ALL
		    SELECT to_char((created_at AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD'), count(*)
		      FROM usage_events
		     WHERE user_id = $1 AND direction = 'outbound'
		       AND created_at >= now() - make_interval(days => $2)
		     GROUP BY 1
		) x GROUP BY day ORDER BY day`, userID, summaryDailyWindowDays)
	if err != nil {
		return nil, fmt.Errorf("summary: daily sends: %w", err)
	}
	for rows.Next() {
		var dc DayCount
		if err := rows.Scan(&dc.Day, &dc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		days = append(days, dc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Subjects are keyed digests of a normalized form (lower-case, collapsed
	// whitespace); the plaintext never leaves this function.
	subjectCounts := map[string]int64{}
	rows, err = tx.Query(ctx, `
		SELECT COALESCE(m.subject, ''), count(*) AS n
		  FROM messages m JOIN agent_identities a ON a.id = m.agent_id
		 WHERE a.user_id = $1 AND m.direction = 'outbound'
		   AND m.delivery_status IN `+sentOutboundStatuses+`
		 GROUP BY 1 ORDER BY n DESC LIMIT 200`, userID)
	if err != nil {
		return nil, fmt.Errorf("summary: subjects: %w", err)
	}
	for rows.Next() {
		var subj string
		var n int64
		if err := rows.Scan(&subj, &n); err != nil {
			rows.Close()
			return nil, err
		}
		norm := strings.Join(strings.Fields(strings.ToLower(subj)), " ")
		if norm == "" {
			continue
		}
		subjectCounts[kr.HexDigest("subject", norm)] += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	subjects := make([]DigestCount, 0, len(subjectCounts))
	for d, n := range subjectCounts {
		subjects = append(subjects, DigestCount{Digest: d, Count: n})
	}
	sort.Slice(subjects, func(i, j int) bool {
		if subjects[i].Count != subjects[j].Count {
			return subjects[i].Count > subjects[j].Count
		}
		return subjects[i].Digest < subjects[j].Digest
	})
	if len(subjects) > summaryTopSubjects {
		subjects = subjects[:summaryTopSubjects]
	}

	domainDigests := make([]string, 0, len(verifiedDomains))
	for _, d := range verifiedDomains {
		domainDigests = append(domainDigests, kr.HexDigest(TombstoneKindDomain, NormalizeTombstoneValue(TombstoneKindDomain, d)))
	}
	sort.Strings(domainDigests)

	if sum.recipientDomains, err = json.Marshal(domains); err != nil {
		return nil, err
	}
	if sum.dailySends, err = json.Marshal(days); err != nil {
		return nil, err
	}
	if sum.subjectDigests, err = json.Marshal(subjects); err != nil {
		return nil, err
	}
	if sum.domainDigests, err = json.Marshal(domainDigests); err != nil {
		return nil, err
	}
	return sum, nil
}

// DeletedAccountSummary is the operator view of a deleted_account_summaries
// row (-inspect-deleted-account). It is never exposed over HTTP or MCP.
type DeletedAccountSummary struct {
	AccountRef            string           `json:"account_ref"`
	AccountCreatedAt      time.Time        `json:"account_created_at"`
	DeletedAt             time.Time        `json:"deleted_at"`
	PurgedAt              time.Time        `json:"purged_at"`
	AccountClass          string           `json:"account_class"`
	SendingState          *string          `json:"sending_state"`
	PauseClass            *string          `json:"pause_class"`
	EvidenceRef           *string          `json:"evidence_ref"`
	AgentsCount           int              `json:"agents_count"`
	MessagesCount         int64            `json:"messages_count"`
	OutboundSendsCount    int64            `json:"outbound_sends_count"`
	FirstOutboundAt       *time.Time       `json:"first_outbound_at"`
	LastOutboundAt        *time.Time       `json:"last_outbound_at"`
	RecipientDomains      []DomainCount    `json:"recipient_domains"`
	DailySends            []DayCount       `json:"daily_sends"`
	SubjectDigests        []DigestCount    `json:"subject_digests"`
	VerifiedDomainDigests []string         `json:"verified_domain_digests"`
	IdentityDigests       []IdentityDigest `json:"identity_digests"`
	DigestKeyVersion      *int             `json:"digest_key_version"`
	RetentionClass        string           `json:"retention_class"`
	ExpiresAt             time.Time        `json:"expires_at"`
}

// GetDeletedAccountSummary loads the summary of a purged account, or
// pgx.ErrNoRows when none is retained.
func (s *Store) GetDeletedAccountSummary(ctx context.Context, accountRef string) (*DeletedAccountSummary, error) {
	d := &DeletedAccountSummary{}
	var domains, days, subjects, digests, ids []byte
	err := s.pool.QueryRow(ctx, `
		SELECT account_ref, account_created_at, deleted_at, purged_at, account_class,
		       sending_state, pause_class, evidence_ref,
		       agents_count, messages_count, outbound_sends_count, first_outbound_at, last_outbound_at,
		       recipient_domains, daily_sends, subject_digests, verified_domain_digests, identity_digests,
		       digest_key_version, retention_class, expires_at
		  FROM deleted_account_summaries WHERE account_ref = $1`, accountRef,
	).Scan(&d.AccountRef, &d.AccountCreatedAt, &d.DeletedAt, &d.PurgedAt, &d.AccountClass,
		&d.SendingState, &d.PauseClass, &d.EvidenceRef,
		&d.AgentsCount, &d.MessagesCount, &d.OutboundSendsCount, &d.FirstOutboundAt, &d.LastOutboundAt,
		&domains, &days, &subjects, &digests, &ids, &d.DigestKeyVersion, &d.RetentionClass, &d.ExpiresAt)
	if err != nil {
		return nil, err
	}
	for _, f := range []struct {
		raw []byte
		dst any
	}{{domains, &d.RecipientDomains}, {days, &d.DailySends}, {subjects, &d.SubjectDigests}, {digests, &d.VerifiedDomainDigests}, {ids, &d.IdentityDigests}} {
		if err := json.Unmarshal(f.raw, f.dst); err != nil {
			return nil, fmt.Errorf("decode summary: %w", err)
		}
	}
	return d, nil
}

// DeleteExpiredDeletedAccountSummaries removes summaries past their hold.
func (s *Store) DeleteExpiredDeletedAccountSummaries(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM deleted_account_summaries WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
