package sendingpolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file is the operator half of external sending access: inspect, approve
// and revoke the shared-identity grant, and decide customer requests. It is
// reachable only from local server commands (flags on the e2a binary). No
// HTTP handler, SDK or MCP tool calls these mutations — a public API
// credential can never grant approval.

var (
	// ErrStaleExternalAccessRevision means the grant moved since the operator
	// inspected it. Zero rows were written; inspect again before retrying.
	ErrStaleExternalAccessRevision = errors.New("sendingpolicy: stale external sending access revision")
	// ErrAccountNotFound means no users row has that id.
	ErrAccountNotFound = errors.New("sendingpolicy: account not found")
	// ErrAccessRequestNotFound means no request with that id exists for the
	// account (or at all).
	ErrAccessRequestNotFound = errors.New("sendingpolicy: sending access request not found")
	// ErrAccessRequestNotPending means the request was already decided.
	ErrAccessRequestNotPending = errors.New("sendingpolicy: sending access request is not pending")
	// ErrAccessRequestRateLimited means the account already submitted the
	// maximum number of requests in the rolling window.
	ErrAccessRequestRateLimited = errors.New("sendingpolicy: too many sending access requests")
	// ErrInvalidAccessRequest means a request field failed validation.
	ErrInvalidAccessRequest = errors.New("sendingpolicy: invalid sending access request")
)

// NewPolicyModule binds a module to a pool, the trust roots and the
// deployment's policy authority. NewGate returns the same object narrowed to
// the provider-authorization role; operator commands and the API use the
// other narrow roles (ExternalAccess, ExternalAccessAdmin) of this one owner.
func NewPolicyModule(pool *pgxpool.Pool, secrets Secrets, source PolicySource, configPolicy RuntimePolicy) *Module {
	m := NewModule(pool, secrets)
	m.source = source
	m.configPolicy = configPolicy
	return m
}

// ExternalAccessRecord is the operator readback for one account.
type ExternalAccessRecord struct {
	AccountID          string
	Approved           bool
	Revision           int64
	ChangedAt          *time.Time
	Paused             bool
	PaidEntitled       bool
	OwnerVerified      bool
	EnforcementApplies bool
	PendingRequestID   string
}

// InspectExternalAccess reads the grant, its revision and every other fact an
// operator needs to understand the effect of a change — notably the paid
// entitlement, which a revocation does NOT remove.
func (m *Module) InspectExternalAccess(ctx context.Context, accountID string) (ExternalAccessRecord, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return ExternalAccessRecord{}, ErrAccountNotFound
	}
	policy, err := m.policyForRead(ctx, m.pool)
	if err != nil {
		return ExternalAccessRecord{}, err
	}
	facts, err := loadAccountAccessFacts(ctx, m.pool, accountID)
	if errors.Is(err, errAccountMissing) {
		return ExternalAccessRecord{}, ErrAccountNotFound
	}
	if err != nil {
		return ExternalAccessRecord{}, err
	}
	rec := ExternalAccessRecord{
		AccountID:     accountID,
		Approved:      facts.approved,
		PaidEntitled:  facts.entitled,
		OwnerVerified: facts.ownerRecipientVerified(),
	}
	var state string
	err = m.pool.QueryRow(ctx, `
		SELECT state, external_sending_access_revision, external_sending_access_changed_at
		  FROM account_sending_controls WHERE user_id = $1`, accountID,
	).Scan(&state, &rec.Revision, &rec.ChangedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ExternalAccessRecord{}, fmt.Errorf("sendingpolicy: read account control: %w", err)
	}
	rec.Paused = state == "paused"
	applies, err := externalAccessApplies(policy, facts)
	if err != nil {
		return ExternalAccessRecord{}, err
	}
	rec.EnforcementApplies = applies && policy.ExternalSendingMode() == ModeEnforce
	err = m.pool.QueryRow(ctx, `
		SELECT id FROM external_sending_access_requests
		 WHERE user_id = $1 AND state = 'pending'`, accountID,
	).Scan(&rec.PendingRequestID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ExternalAccessRecord{}, fmt.Errorf("sendingpolicy: read pending request: %w", err)
	}
	return rec, nil
}

// ExternalAccessChange is one audited operator change to the grant.
type ExternalAccessChange struct {
	AccountID        string
	Approved         bool
	ExpectedRevision int64
	Actor            string
	Reason           string
	// RequestID optionally names the pending customer request this change
	// decides. Approving marks it approved; a revocation never touches
	// request history.
	RequestID string
}

// ExternalAccessChangeResult reports what a change did.
type ExternalAccessChangeResult struct {
	Record ExternalAccessRecord
	// NoOp is true when the grant already had the requested state at the
	// expected revision: nothing was written.
	NoOp bool
}

const maxOperatorReasonLength = 1000

// SetExternalAccess approves or revokes the grant under a compare-and-swap on
// the revision the operator inspected.
//
// Lock order follows the gate's normative order — users, then the account
// control row — so a change serializes against an in-flight ConsumeAttempt
// for the same account instead of deadlocking with it. The users row is taken
// FOR SHARE (the gate's own mode) and the control row FOR UPDATE through the
// same upsert ensureAccountControl uses. Approval never clears a pause.
func (m *Module) SetExternalAccess(ctx context.Context, req ExternalAccessChange) (ExternalAccessChangeResult, error) {
	req.AccountID = strings.TrimSpace(req.AccountID)
	if req.AccountID == "" {
		return ExternalAccessChangeResult{}, ErrAccountNotFound
	}
	if strings.TrimSpace(req.Actor) == "" {
		return ExternalAccessChangeResult{}, errors.New("sendingpolicy: an external access change requires an actor")
	}
	if strings.TrimSpace(req.Reason) == "" || len([]rune(req.Reason)) > maxOperatorReasonLength {
		return ExternalAccessChangeResult{}, fmt.Errorf("sendingpolicy: an external access change requires a nonblank reason of at most %d characters", maxOperatorReasonLength)
	}
	if req.ExpectedRevision < 0 {
		return ExternalAccessChangeResult{}, errors.New("sendingpolicy: an external access change requires the expected revision")
	}
	if req.RequestID != "" && !req.Approved {
		return ExternalAccessChangeResult{}, errors.New("sendingpolicy: a revocation does not decide a request; use the decline command")
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return ExternalAccessChangeResult{}, fmt.Errorf("sendingpolicy: begin external access change: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	policy, err := m.effectivePolicy(ctx, tx)
	if err != nil {
		return ExternalAccessChangeResult{}, err
	}

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1 FOR SHARE`, req.AccountID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExternalAccessChangeResult{}, ErrAccountNotFound
		}
		return ExternalAccessChangeResult{}, fmt.Errorf("sendingpolicy: lock user: %w", err)
	}
	var approved bool
	var revision int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO account_sending_controls (user_id)
		VALUES ($1)
		ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id
		RETURNING external_sending_approved, external_sending_access_revision`, req.AccountID,
	).Scan(&approved, &revision); err != nil {
		return ExternalAccessChangeResult{}, fmt.Errorf("sendingpolicy: lock account control: %w", err)
	}
	if revision != req.ExpectedRevision {
		return ExternalAccessChangeResult{}, fmt.Errorf("%w: stored revision is %d, expected %d",
			ErrStaleExternalAccessRevision, revision, req.ExpectedRevision)
	}

	noOp := approved == req.Approved
	if !noOp {
		retention := time.Duration(policy.SendingControlAuditRetentionDays) * 24 * time.Hour
		if _, err := tx.Exec(ctx, `
			UPDATE account_sending_controls
			   SET external_sending_approved = $2,
			       external_sending_access_revision = external_sending_access_revision + 1,
			       external_sending_access_changed_at = now(),
			       updated_at = now()
			 WHERE user_id = $1`, req.AccountID, req.Approved,
		); err != nil {
			return ExternalAccessChangeResult{}, fmt.Errorf("sendingpolicy: write external access: %w", err)
		}
		var requestRef *string
		if req.RequestID != "" {
			requestRef = &req.RequestID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO external_sending_access_events
			    (id, account_ref, old_approved, new_approved, old_revision, new_revision,
			     actor, reason, request_ref, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now() + $10::interval)`,
			randomID("esae_"), req.AccountID, approved, req.Approved, revision, revision+1,
			req.Actor, req.Reason, requestRef, fmt.Sprintf("%d seconds", int64(retention.Seconds())),
		); err != nil {
			return ExternalAccessChangeResult{}, fmt.Errorf("sendingpolicy: record external access event: %w", err)
		}
	}
	if req.RequestID != "" {
		if err := decideRequestTx(ctx, tx, req.AccountID, req.RequestID, "approved", req.Actor); err != nil {
			return ExternalAccessChangeResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ExternalAccessChangeResult{}, fmt.Errorf("sendingpolicy: commit external access change: %w", err)
	}
	rec, err := m.InspectExternalAccess(ctx, req.AccountID)
	if err != nil {
		return ExternalAccessChangeResult{}, err
	}
	return ExternalAccessChangeResult{Record: rec, NoOp: noOp}, nil
}

// decideRequestTx moves one pending request of the account to a decided state.
func decideRequestTx(ctx context.Context, tx pgx.Tx, accountID, requestID, state, actor string) error {
	var current string
	err := tx.QueryRow(ctx, `
		SELECT state FROM external_sending_access_requests
		 WHERE id = $1 AND user_id = $2 FOR UPDATE`, requestID, accountID,
	).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccessRequestNotFound
	}
	if err != nil {
		return fmt.Errorf("sendingpolicy: lock sending access request: %w", err)
	}
	if current != "pending" {
		return ErrAccessRequestNotPending
	}
	if _, err := tx.Exec(ctx, `
		UPDATE external_sending_access_requests
		   SET state = $3, decided_at = now(), decided_by = $4
		 WHERE id = $1 AND user_id = $2`, requestID, accountID, state, actor,
	); err != nil {
		return fmt.Errorf("sendingpolicy: decide sending access request: %w", err)
	}
	return nil
}

// DeclineExternalAccessRequest records a support decision to decline a
// pending request without touching the grant.
func (m *Module) DeclineExternalAccessRequest(ctx context.Context, accountID, requestID, actor string) error {
	if strings.TrimSpace(actor) == "" {
		return errors.New("sendingpolicy: declining a request requires an actor")
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sendingpolicy: begin decline: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := decideRequestTx(ctx, tx, strings.TrimSpace(accountID), strings.TrimSpace(requestID), "declined", actor); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- Customer requests -----------------------------------------------------

// AccessRequest is one customer approval request.
type AccessRequest struct {
	ID                  string
	State               string
	UseCase             string
	Recipients          string
	ExpectedDailyVolume int
	CreatedAt           time.Time
	DecidedAt           *time.Time
}

// AccessRequestInput is the bounded customer-supplied part of a request. The
// account is never part of it: callers bind it from the authenticated
// principal.
type AccessRequestInput struct {
	UseCase             string
	Recipients          string
	ExpectedDailyVolume int
}

// Request bounds, mirrored by the table CHECKs and the API schema.
const (
	MaxAccessRequestUseCase    = 2000
	MaxAccessRequestRecipients = 1000
	MaxAccessRequestVolume     = 1000000
	// accessRequestWindow / accessRequestMax bound how many requests (the
	// original plus any appeals after a decline) one account may file.
	accessRequestWindow = 30 * 24 * time.Hour
	accessRequestMax    = 3
)

func (in AccessRequestInput) validate() error {
	useCase := strings.TrimSpace(in.UseCase)
	recipients := strings.TrimSpace(in.Recipients)
	switch {
	case useCase == "" || len([]rune(useCase)) > MaxAccessRequestUseCase:
		return fmt.Errorf("%w: use_case must be 1-%d characters", ErrInvalidAccessRequest, MaxAccessRequestUseCase)
	case recipients == "" || len([]rune(recipients)) > MaxAccessRequestRecipients:
		return fmt.Errorf("%w: recipients must be 1-%d characters", ErrInvalidAccessRequest, MaxAccessRequestRecipients)
	case in.ExpectedDailyVolume < 1 || in.ExpectedDailyVolume > MaxAccessRequestVolume:
		return fmt.Errorf("%w: expected_daily_volume must be between 1 and %d", ErrInvalidAccessRequest, MaxAccessRequestVolume)
	}
	return nil
}

const accessRequestColumns = `id, state, use_case, recipients, expected_daily_volume, created_at, decided_at`

func scanAccessRequest(row pgx.Row) (AccessRequest, error) {
	var r AccessRequest
	err := row.Scan(&r.ID, &r.State, &r.UseCase, &r.Recipients, &r.ExpectedDailyVolume, &r.CreatedAt, &r.DecidedAt)
	return r, err
}

// SubmitAccessRequest files a request for the account, or returns the
// account's existing pending request unchanged (created=false). Submission
// is therefore idempotent while a request is pending; after a decline a new
// request (an appeal) may be filed within the rolling-window cap.
func (m *Module) SubmitAccessRequest(ctx context.Context, userID string, in AccessRequestInput) (AccessRequest, bool, error) {
	if err := m.requireExternalAccessEnabled(ctx); err != nil {
		return AccessRequest{}, false, err
	}
	if err := in.validate(); err != nil {
		return AccessRequest{}, false, err
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return AccessRequest{}, false, fmt.Errorf("sendingpolicy: begin access request: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// No row lock on users: the gate reads that row FOR SHARE on every
	// authorization and a customer form must not contend with it. The partial
	// unique index (one pending request per account) is the serialization
	// point; a concurrent duplicate submit loses the insert and reads back the
	// winner's pending request.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1`, userID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AccessRequest{}, false, ErrAccountNotFound
		}
		return AccessRequest{}, false, fmt.Errorf("sendingpolicy: read user: %w", err)
	}
	existing, err := scanAccessRequest(tx.QueryRow(ctx,
		`SELECT `+accessRequestColumns+` FROM external_sending_access_requests WHERE user_id = $1 AND state = 'pending'`, userID))
	if err == nil {
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AccessRequest{}, false, fmt.Errorf("sendingpolicy: read pending request: %w", err)
	}
	var recent int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM external_sending_access_requests
		 WHERE user_id = $1 AND created_at > now() - $2::interval`,
		userID, fmt.Sprintf("%d seconds", int64(accessRequestWindow.Seconds())),
	).Scan(&recent); err != nil {
		return AccessRequest{}, false, fmt.Errorf("sendingpolicy: count requests: %w", err)
	}
	if recent >= accessRequestMax {
		return AccessRequest{}, false, ErrAccessRequestRateLimited
	}
	created, err := scanAccessRequest(tx.QueryRow(ctx, `
		INSERT INTO external_sending_access_requests (id, user_id, use_case, recipients, expected_daily_volume)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+accessRequestColumns,
		randomID("esar_"), userID, strings.TrimSpace(in.UseCase), strings.TrimSpace(in.Recipients), in.ExpectedDailyVolume))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_ = tx.Rollback(ctx)
			winner, rerr := scanAccessRequest(m.pool.QueryRow(ctx,
				`SELECT `+accessRequestColumns+` FROM external_sending_access_requests WHERE user_id = $1 AND state = 'pending'`, userID))
			if rerr != nil {
				return AccessRequest{}, false, fmt.Errorf("sendingpolicy: read concurrent pending request: %w", rerr)
			}
			return winner, false, nil
		}
		return AccessRequest{}, false, fmt.Errorf("sendingpolicy: insert access request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AccessRequest{}, false, fmt.Errorf("sendingpolicy: commit access request: %w", err)
	}
	return created, true, nil
}

// LatestAccessRequest returns the account's most recent request, or nil.
func (m *Module) LatestAccessRequest(ctx context.Context, userID string) (*AccessRequest, error) {
	if err := m.requireExternalAccessEnabled(ctx); err != nil {
		return nil, err
	}
	r, err := scanAccessRequest(m.pool.QueryRow(ctx,
		`SELECT `+accessRequestColumns+` FROM external_sending_access_requests
		  WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: read latest request: %w", err)
	}
	return &r, nil
}
