package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	AgentSignupPending  = "pending"
	AgentSignupVerified = "verified"
	AgentSignupRejected = "rejected"

	AgentSignupSendLimit             = 5
	AgentSignupMaxAttempts           = 5
	AgentSignupVerificationMailLimit = 5
)

var (
	ErrAgentSignupNotFound            = errors.New("agent signup not found")
	ErrAgentSignupCodeInvalid         = errors.New("agent signup verification code is invalid")
	ErrAgentSignupCodeExpired         = errors.New("agent signup verification code has expired")
	ErrAgentSignupAttemptsExhausted   = errors.New("agent signup verification attempts exhausted")
	ErrAgentSignupFinal               = errors.New("agent signup is already resolved")
	ErrAgentSignupPendingVerification = errors.New("agent signup is pending human verification")
	ErrAgentSignupSendLimit           = errors.New("agent signup provisional send limit reached")
	ErrAgentSignupResumeRequired      = errors.New("current agent signup key is required")
	ErrAgentSignupMailLimit           = errors.New("agent signup verification mail limit reached")
	ErrAgentSignupRejected            = errors.New("agent signup was rejected")
)

type AgentSignup struct {
	ID                   string
	UserID               string
	AgentID              string
	HumanEmail           string
	DisplayName          string
	NoteToHuman          string
	Harness              string
	Status               string
	CodeNonce            string
	SharedDomain         string
	CodeExpiresAt        time.Time
	VerificationSentAt   time.Time
	VerificationAttempts int
	ReviewOutbound       bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
	VerifiedAt           *time.Time
	RejectedAt           *time.Time
}

type AgentSignupRegistration struct {
	HumanEmail    string
	DisplayName   string
	NoteToHuman   string
	Harness       string
	SharedDomain  string
	CodeNonce     string
	CodeHash      string
	CodeExpiresAt time.Time
	CurrentAPIKey string
}

type AgentSignupResult struct {
	Signup  *AgentSignup
	APIKey  *APIKey
	Created bool
}

// EnsureAgentSignupUser returns the account row identified by the verified
// human mailbox. It creates a deliberately claimable placeholder only when no
// account owns that email; existing accounts are never modified or merged.
func (s *Store) EnsureAgentSignupUser(ctx context.Context, humanEmail, displayName string) (*User, error) {
	email := NormalizeEmail(humanEmail)
	id := generateID()
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (id, email, name, google_subject)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		 RETURNING id, email, name, google_subject, created_at`,
		id, email, strings.TrimSpace(displayName), "agent-signup:"+id,
	).Scan(&u.ID, &u.Email, &u.Name, &u.GoogleSubject, &u.CreatedAt)
	return u, err
}

func normalizedSignupName(name string) (string, string) {
	display := strings.Join(strings.Fields(name), " ")
	return display, strings.ToLower(display)
}

var signupSlugInvalid = regexp.MustCompile(`[^a-z0-9]+`)

var reservedSignupSlugs = map[string]bool{
	"admin": true, "postmaster": true, "abuse": true, "noreply": true,
	"no-reply": true, "mailer-daemon": true, "info": true, "help": true,
	"demo": true, "test": true, "www": true, "mail": true, "agent": true,
	"api": true, "system": true, "root": true,
}

func signupSlug(displayName, fallback string) string {
	slug := strings.Trim(signupSlugInvalid.ReplaceAllString(strings.ToLower(displayName), "-"), "-")
	if len(slug) > 32 {
		slug = strings.TrimRight(slug[:32], "-")
	}
	if len(slug) < 2 {
		slug = "agent-" + fallback[:6]
	}
	// Shared-domain operational and platform mailboxes must never be claimable
	// through an anonymous display name. Prefixing keeps the requested name
	// recognizable while matching the normal create-agent reserved namespace.
	if reservedSignupSlugs[slug] {
		slug = "agent-" + slug
	}
	return slug
}

func scanAgentSignup(row pgx.Row) (*AgentSignup, error) {
	v := &AgentSignup{}
	err := row.Scan(&v.ID, &v.UserID, &v.AgentID, &v.HumanEmail, &v.DisplayName,
		&v.NoteToHuman, &v.Harness, &v.Status, &v.CodeNonce, &v.SharedDomain, &v.CodeExpiresAt,
		&v.VerificationSentAt, &v.VerificationAttempts, &v.ReviewOutbound,
		&v.CreatedAt, &v.UpdatedAt, &v.VerifiedAt, &v.RejectedAt)
	return v, err
}

const agentSignupColumns = `id, user_id, agent_id, human_email, display_name,
 note_to_human, harness, status, code_nonce, shared_domain, code_expires_at, verification_sent_at,
 verification_attempts, review_outbound, created_at, updated_at, verified_at, rejected_at`

func createScopedAPIKeyTx(ctx context.Context, tx pgx.Tx, userID, name, agentID string, expiresAt time.Time) (*APIKey, error) {
	id := "apk_" + generateID()
	plaintext := generateAPIKey(ScopeAgent)
	now := time.Now()
	ak := &APIKey{ID: id, UserID: userID, Name: name, KeyPrefix: plaintext[:16], PlaintextKey: plaintext, CreatedAt: now, Scope: ScopeAgent, AgentID: &agentID, ExpiresAt: &expiresAt}
	_, err := tx.Exec(ctx,
		`INSERT INTO api_keys (id, user_id, name, key_prefix, key_hash, scope, agent_id, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		ak.ID, userID, name, ak.KeyPrefix, hashAPIKey(plaintext), ScopeAgent, agentID, now, expiresAt)
	return ak, err
}

// RegisterAgentSignup creates the provisional identity or rotates the
// credential/code of the existing pending identity for the normalized pair.
func (s *Store) RegisterAgentSignup(ctx context.Context, in AgentSignupRegistration, beforeCommit func(context.Context, pgx.Tx, *AgentSignup) error) (*AgentSignupResult, error) {
	human := NormalizeEmail(in.HumanEmail)
	display, displayKey := normalizedSignupName(in.DisplayName)
	domain := normalizeDomain(in.SharedDomain)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 31))`, human+"\n"+displayKey); err != nil {
		return nil, err
	}

	existing, err := scanAgentSignup(tx.QueryRow(ctx, `SELECT `+agentSignupColumns+` FROM agent_signups WHERE human_email=$1 AND display_name_key=$2 FOR UPDATE`, human, displayKey))
	if err == nil {
		if existing.Status != AgentSignupPending {
			return nil, ErrAgentSignupFinal
		}
		var keyMatches bool
		if in.CurrentAPIKey != "" {
			err = tx.QueryRow(ctx, `SELECT EXISTS(
				SELECT 1 FROM api_keys
				 WHERE agent_id=$1 AND key_hash=$2 AND revoked_at IS NULL
			)`, existing.AgentID, hashAPIKey(in.CurrentAPIKey)).Scan(&keyMatches)
			if err != nil {
				return nil, err
			}
		}
		if !keyMatches {
			return nil, ErrAgentSignupResumeRequired
		}
		if err := reserveAgentSignupVerificationMailTx(ctx, tx, human, time.Now().UTC()); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE agent_id=$1 AND revoked_at IS NULL`, existing.AgentID); err != nil {
			return nil, err
		}
		if tag, err := tx.Exec(ctx, `UPDATE agent_identities SET assertion_version=assertion_version+1 WHERE id=$1 AND deleted_at IS NULL`, existing.AgentID); err != nil {
			return nil, err
		} else if tag.RowsAffected() != 1 {
			return nil, ErrAgentSignupNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_signups SET note_to_human=$2, harness=$3, code_nonce=$4, code_hash=$5, code_expires_at=$6, verification_attempts=0, verification_sent_at=now(), updated_at=now() WHERE id=$1`, existing.ID, strings.TrimSpace(in.NoteToHuman), strings.TrimSpace(in.Harness), in.CodeNonce, in.CodeHash, in.CodeExpiresAt); err != nil {
			return nil, err
		}
		key, err := createScopedAPIKeyTx(ctx, tx, existing.UserID, "Agent signup", existing.AgentID, in.CodeExpiresAt)
		if err != nil {
			return nil, err
		}
		updated, err := scanAgentSignup(tx.QueryRow(ctx, `SELECT `+agentSignupColumns+` FROM agent_signups WHERE id=$1`, existing.ID))
		if err != nil {
			return nil, err
		}
		if beforeCommit != nil {
			if err := beforeCommit(ctx, tx, updated); err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &AgentSignupResult{Signup: updated, APIKey: key}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var verified bool
	if err := tx.QueryRow(ctx, `SELECT verified FROM domains WHERE domain=$1`, domain).Scan(&verified); err != nil || !verified {
		if err == nil {
			err = fmt.Errorf("shared domain is not verified")
		}
		return nil, err
	}
	if err := reserveAgentSignupVerificationMailTx(ctx, tx, human, time.Now().UTC()); err != nil {
		return nil, err
	}
	signupID := "asu_" + generateID()
	provisionalUserID := generateID()
	if _, err := tx.Exec(ctx, `INSERT INTO users(id,email,name,google_subject)
		VALUES($1,$2,$3,$4)`, provisionalUserID, signupID+"@agents.localhost", display, "agent-signup:"+signupID); err != nil {
		return nil, err
	}
	slug := signupSlug(display, strings.TrimPrefix(signupID, "asu_"))
	agentID := slug + "@" + domain
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_identities WHERE id=$1)`, agentID).Scan(&taken); err != nil {
		return nil, err
	}
	if taken {
		suffix := strings.TrimPrefix(signupID, "asu_")[:6]
		base := slug
		if len(base) > 32 {
			base = base[:32]
		}
		agentID = strings.TrimRight(base, "-") + "-" + suffix + "@" + domain
	}
	if _, err := s.CreateAgentWithLimitTx(ctx, tx, agentID, domain, display, provisionalUserID, 0); err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO agent_signups
		(id,user_id,agent_id,human_email,display_name,display_name_key,note_to_human,harness,code_nonce,shared_domain,code_hash,code_expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, signupID, provisionalUserID, agentID, human, display, displayKey, strings.TrimSpace(in.NoteToHuman), strings.TrimSpace(in.Harness), in.CodeNonce, domain, in.CodeHash, in.CodeExpiresAt)
	if err != nil {
		return nil, err
	}
	key, err := createScopedAPIKeyTx(ctx, tx, provisionalUserID, "Agent signup", agentID, in.CodeExpiresAt)
	if err != nil {
		return nil, err
	}
	signup, err := scanAgentSignup(tx.QueryRow(ctx, `SELECT `+agentSignupColumns+` FROM agent_signups WHERE id=$1`, signupID))
	if err != nil {
		return nil, err
	}
	if beforeCommit != nil {
		if err := beforeCommit(ctx, tx, signup); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &AgentSignupResult{Signup: signup, APIKey: key, Created: true}, nil
}

// GetPendingAgentSignupNotification resolves a queued verification delivery.
// A superseded nonce or resolved signup is a successful no-op for the worker.
func (s *Store) GetPendingAgentSignupNotification(ctx context.Context, signupID, nonce string) (*AgentSignup, error) {
	v, err := scanAgentSignup(s.pool.QueryRow(ctx, `SELECT `+agentSignupColumns+` FROM agent_signups WHERE id=$1 AND code_nonce=$2 AND status='pending'`, signupID, nonce))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

func reserveAgentSignupVerificationMailTx(ctx context.Context, tx pgx.Tx, human string, now time.Time) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 32))`, human); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_signup_verification_events WHERE human_email=$1 AND sent_at <= $2`, human, now.Add(-24*time.Hour)); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_signup_verification_events WHERE human_email=$1`, human).Scan(&count); err != nil {
		return err
	}
	if count >= AgentSignupVerificationMailLimit {
		return ErrAgentSignupMailLimit
	}
	_, err := tx.Exec(ctx, `INSERT INTO agent_signup_verification_events(human_email,sent_at) VALUES($1,$2)`, human, now)
	return err
}

func applySignupReviewPolicy(ctx context.Context, tx pgx.Tx, agentID, human string, review bool) error {
	if !review {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE agent_identities SET outbound_policy='allowlist', outbound_allowlist=$2, outbound_policy_action='review' WHERE id=$1`, agentID, []string{human})
	return err
}

func transferAgentSignupTx(ctx context.Context, tx pgx.Tx, signupID, agentID, human, sourceUserID, targetUserID string, maxAgents int, review bool, now time.Time) error {
	if sourceUserID == targetUserID {
		return fmt.Errorf("provisional and verified signup owners must differ")
	}
	var targetEmail string
	if err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, targetUserID).Scan(&targetEmail); err != nil {
		return err
	}
	if NormalizeEmail(targetEmail) != human {
		return ErrAgentSignupNotFound
	}
	// Validate against the operator-configured shared domain recorded at
	// creation. domains.user_id is not authoritative here because deployment
	// tooling may adopt that row for lifecycle management.
	var registeredDomain, sharedDomain string
	if err := tx.QueryRow(ctx, `SELECT a.registered_domain,s.shared_domain FROM agent_identities a JOIN agent_signups s ON s.agent_id=a.id WHERE a.id=$1 AND s.id=$2`, agentID, signupID).Scan(&registeredDomain, &sharedDomain); err != nil {
		return err
	}
	if sharedDomain == "" || registeredDomain != sharedDomain {
		return ErrAgentSignupNotFound
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 2))`, targetUserID); err != nil {
		return err
	}
	if maxAgents > 0 {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_identities a JOIN domains d ON a.registered_domain=d.domain WHERE a.user_id=$1 AND a.deleted_at IS NULL`, targetUserID).Scan(&count); err != nil {
			return err
		}
		if count >= maxAgents {
			return &AgentLimitExceededError{Limit: maxAgents, Current: count}
		}
	}
	result, err := tx.Exec(ctx, `UPDATE agent_identities SET user_id=$3 WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL`, agentID, sourceUserID, targetUserID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrAgentSignupNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE api_keys SET user_id=$2,expires_at=NULL WHERE agent_id=$1`, agentID, targetUserID); err != nil {
		return err
	}
	if review {
		if err := applySignupReviewPolicy(ctx, tx, agentID, human, true); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_signups SET user_id=$2,status='verified',review_outbound=$3,verified_at=$4,updated_at=$4 WHERE id=$1`, signupID, targetUserID, review, now); err != nil {
		return err
	}
	return nil
}

func (s *Store) VerifyAgentSignup(ctx context.Context, agentID, presentedHash string, review bool, now time.Time, bind func(*AgentSignup) (targetUserID string, maxAgents int, err error)) (*AgentSignup, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var signupID, human, sourceUserID, status, expected string
	var expires time.Time
	var attempts int
	if err := tx.QueryRow(ctx, `SELECT id,human_email,user_id,status,code_hash,code_expires_at,verification_attempts FROM agent_signups WHERE agent_id=$1 FOR UPDATE`, NormalizeEmail(agentID)).Scan(&signupID, &human, &sourceUserID, &status, &expected, &expires, &attempts); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAgentSignupNotFound
		}
		return nil, err
	}
	if status != AgentSignupPending {
		return nil, ErrAgentSignupFinal
	}
	if !now.Before(expires) {
		return nil, ErrAgentSignupCodeExpired
	}
	if attempts >= AgentSignupMaxAttempts {
		return nil, ErrAgentSignupAttemptsExhausted
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(presentedHash)) != 1 {
		_, err = tx.Exec(ctx, `UPDATE agent_signups SET verification_attempts=verification_attempts+1,updated_at=$2 WHERE id=$1`, signupID, now)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, ErrAgentSignupCodeInvalid
	}
	current, err := scanAgentSignup(tx.QueryRow(ctx, `SELECT `+agentSignupColumns+` FROM agent_signups WHERE id=$1`, signupID))
	if err != nil {
		return nil, err
	}
	if bind == nil {
		return nil, fmt.Errorf("agent signup owner resolver is required")
	}
	targetUserID, maxAgents, err := bind(current)
	if err != nil {
		return nil, err
	}
	if err := transferAgentSignupTx(ctx, tx, signupID, NormalizeEmail(agentID), human, sourceUserID, targetUserID, maxAgents, review, now); err != nil {
		return nil, err
	}
	v, err := scanAgentSignup(tx.QueryRow(ctx, `SELECT `+agentSignupColumns+` FROM agent_signups WHERE id=$1`, signupID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Store) ApproveAgentSignup(ctx context.Context, signupID, humanEmail, targetUserID string, maxAgents int, review bool, now time.Time) (*AgentSignup, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	human := NormalizeEmail(humanEmail)
	var agentID, sourceUserID, status string
	if err := tx.QueryRow(ctx, `SELECT agent_id,user_id,status FROM agent_signups WHERE id=$1 AND human_email=$2 FOR UPDATE`, signupID, human).Scan(&agentID, &sourceUserID, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAgentSignupNotFound
		}
		return nil, err
	}
	if status != AgentSignupPending {
		return nil, ErrAgentSignupFinal
	}
	if err := transferAgentSignupTx(ctx, tx, signupID, agentID, human, sourceUserID, targetUserID, maxAgents, review, now); err != nil {
		return nil, err
	}
	v, err := scanAgentSignup(tx.QueryRow(ctx, `SELECT `+agentSignupColumns+` FROM agent_signups WHERE id=$1`, signupID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// CheckAgentSignupSend validates the non-consuming part of the provisional
// egress policy. Call it before account quotas and suppression checks so a
// forbidden provisional request always receives the same 403 and cannot use
// those checks as an oracle. The returned status is empty for ordinary agents.
func (s *Store) CheckAgentSignupSend(ctx context.Context, agentID string, recipients []string, scheduled bool) (string, error) {
	var human, status string
	err := s.pool.QueryRow(ctx, `SELECT human_email,status FROM agent_signups WHERE agent_id=$1`, NormalizeEmail(agentID)).Scan(&human, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch status {
	case AgentSignupVerified:
		return status, nil
	case AgentSignupRejected:
		return status, ErrAgentSignupRejected
	case AgentSignupPending:
		if scheduled {
			return status, ErrAgentSignupPendingVerification
		}
		for _, recipient := range recipients {
			if NormalizeMailboxAddress(recipient) != human {
				return status, ErrAgentSignupPendingVerification
			}
		}
		return status, nil
	default:
		return status, ErrAgentSignupRejected
	}
}

func (s *Store) RejectAgentSignup(ctx context.Context, signupID, humanEmail string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var agentID, status string
	if err := tx.QueryRow(ctx, `SELECT agent_id,status FROM agent_signups WHERE id=$1 AND human_email=$2 FOR UPDATE`, signupID, NormalizeEmail(humanEmail)).Scan(&agentID, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgentSignupNotFound
		}
		return err
	}
	if status != AgentSignupPending {
		return ErrAgentSignupFinal
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_signups SET status='rejected',rejected_at=$2,updated_at=$2 WHERE id=$1`, signupID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at=$2 WHERE agent_id=$1 AND revoked_at IS NULL`, agentID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_identities SET deleted_at=$2 WHERE id=$1`, agentID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListPendingAgentSignups returns pending requests for the authenticated
// human, newest first, keyset-paginated on (created_at,id).
func (s *Store) ListPendingAgentSignups(ctx context.Context, humanEmail string, limit int, afterCreatedAt time.Time, afterID string) ([]AgentSignup, error) {
	query := `SELECT ` + agentSignupColumns + ` FROM agent_signups
		WHERE human_email=$1 AND status='pending'`
	args := []any{NormalizeEmail(humanEmail)}
	if !afterCreatedAt.IsZero() {
		query += ` AND (created_at,id) < ($2,$3)`
		args = append(args, afterCreatedAt, afterID)
	}
	query += ` ORDER BY created_at DESC,id DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AgentSignup, 0)
	for rows.Next() {
		v, err := scanAgentSignup(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *v)
	}
	return items, rows.Err()
}

// ConsumeAgentSignupSend is a no-op for ordinary and verified agents. Pending
// signups may address only their human and consume one of five rolling slots.
func (s *Store) ConsumeAgentSignupSend(ctx context.Context, agentID string, recipients []string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var signupID, human, status string
	err = tx.QueryRow(ctx, `SELECT id,human_email,status FROM agent_signups WHERE agent_id=$1 FOR UPDATE`, NormalizeEmail(agentID)).Scan(&signupID, &human, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch status {
	case AgentSignupVerified:
		return nil
	case AgentSignupRejected:
		return ErrAgentSignupRejected
	case AgentSignupPending:
		// Continue below while holding the signup row lock.
	default:
		return ErrAgentSignupRejected
	}
	for _, recipient := range recipients {
		if NormalizeMailboxAddress(recipient) != human {
			return ErrAgentSignupPendingVerification
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_signup_send_events WHERE signup_id=$1 AND sent_at <= $2`, signupID, now.Add(-24*time.Hour)); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_signup_send_events WHERE signup_id=$1`, signupID).Scan(&count); err != nil {
		return err
	}
	if count >= AgentSignupSendLimit {
		return ErrAgentSignupSendLimit
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_signup_send_events(signup_id,sent_at) VALUES($1,$2)`, signupID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
