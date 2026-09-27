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
	// ErrSendingAccessNotRestricted means the account is not currently
	// restricted (enforcement does not bind it, it is already approved, or
	// an available unlock already lifts it): there is nothing to request,
	// so no row is written and no operator is notified.
	ErrSendingAccessNotRestricted = errors.New("sendingpolicy: external sending is not restricted for this account")
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
	// AvailableUnlocks is the effective unlock set of the governing policy
	// (nil when the control is disabled), so the operator sees whether the
	// paid entitlement or a verified domain would lift the restriction.
	AvailableUnlocks []ExternalUnlock
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
	if policy.ExternalSendingMode() != ModeDisabled {
		rec.AvailableUnlocks = policy.ExternalSendingAccess.AvailableUnlocks()
	}
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
	// FromExemptAccount is set by SubmitAccessRequest when the filing
	// account's server-owned account_class is exempt from the rule
	// (system/internal). Such a request needs no operator decision, so the
	// caller skips the operator notification. Not part of the customer view.
	FromExemptAccount bool
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

// normalizeRequestText converts CRLF line endings to LF — the one line-break
// form the operator email's quoting fence is built around — and trims.
func normalizeRequestText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

// hasForbiddenRequestRune reports a control character other than LF and TAB,
// a Unicode line/paragraph separator, or a bidi override/isolate control. Either could break out of the
// "> " fence that marks customer text as untrusted in the operator email.
func hasForbiddenRequestRune(s string) bool {
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			continue
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return true
		case r == '\u2028' || r == '\u2029':
			return true
		case (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069'):
			// Bidi embedding/override/isolate controls can visually reorder
			// the operator email so customer text reads as operator text.
			return true
		}
	}
	return false
}

func (in AccessRequestInput) validate() error {
	useCase := normalizeRequestText(in.UseCase)
	recipients := normalizeRequestText(in.Recipients)
	switch {
	case hasForbiddenRequestRune(useCase) || hasForbiddenRequestRune(recipients):
		return fmt.Errorf("%w: use_case and recipients must not contain control characters or Unicode line separators (line breaks and tabs are fine)", ErrInvalidAccessRequest)
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
	policy, err := m.policyForRead(ctx, m.pool)
	if err != nil {
		return AccessRequest{}, false, err
	}
	if policy.ExternalSendingMode() == ModeDisabled {
		return AccessRequest{}, false, ErrExternalAccessDisabled
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
	var class string
	if err := tx.QueryRow(ctx, `SELECT account_class FROM users WHERE id = $1`, userID).Scan(&class); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AccessRequest{}, false, ErrAccountNotFound
		}
		return AccessRequest{}, false, fmt.Errorf("sendingpolicy: read user: %w", err)
	}
	exempt := accountClassExempt(class)
	if !exempt {
		// Only an account the rule actually restricts right now may file:
		// enforce mode, inside the cohort, not approved, and no available
		// unlock already lifting it. Anything else would only mint operator
		// mail with nothing to decide. System/internal classes are exempt
		// from the rule; they may still file (first-party conformance) and
		// are never notified.
		facts, err := loadAccountAccessFacts(ctx, tx, userID)
		if errors.Is(err, errAccountMissing) {
			return AccessRequest{}, false, ErrAccountNotFound
		}
		if err != nil {
			return AccessRequest{}, false, err
		}
		applies, err := externalAccessApplies(policy, facts)
		if err != nil {
			return AccessRequest{}, false, err
		}
		esa := policy.ExternalSendingAccess
		if !applies || policy.ExternalSendingMode() != ModeEnforce || facts.approved ||
			(facts.entitled && esa.Allows(UnlockPaidEntitlement)) {
			return AccessRequest{}, false, ErrSendingAccessNotRestricted
		}
	}
	existing, err := scanAccessRequest(tx.QueryRow(ctx,
		`SELECT `+accessRequestColumns+` FROM external_sending_access_requests WHERE user_id = $1 AND state = 'pending'`, userID))
	if err == nil {
		existing.FromExemptAccount = exempt
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
		randomID("esar_"), userID, normalizeRequestText(in.UseCase), normalizeRequestText(in.Recipients), in.ExpectedDailyVolume))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_ = tx.Rollback(ctx)
			winner, rerr := scanAccessRequest(m.pool.QueryRow(ctx,
				`SELECT `+accessRequestColumns+` FROM external_sending_access_requests WHERE user_id = $1 AND state = 'pending'`, userID))
			if rerr != nil {
				return AccessRequest{}, false, fmt.Errorf("sendingpolicy: read concurrent pending request: %w", rerr)
			}
			winner.FromExemptAccount = exempt
			return winner, false, nil
		}
		return AccessRequest{}, false, fmt.Errorf("sendingpolicy: insert access request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AccessRequest{}, false, fmt.Errorf("sendingpolicy: commit access request: %w", err)
	}
	created.FromExemptAccount = exempt
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

// AccessRequestListing is one row of the operator's request queue. Account
// id, state and numbers only — no customer free text and no address.
type AccessRequestListing struct {
	ID                  string
	AccountID           string
	State               string
	CreatedAt           time.Time
	DecidedAt           *time.Time
	ExpectedDailyVolume int
	// Approved is the account's CURRENT shared-identity grant.
	Approved bool
}

// maxAccessRequestListing bounds one listing; the queue is a review
// worklist, not an export.
const maxAccessRequestListing = 500

// MaxAccessRequestListing exposes the listing bound for callers that report
// truncation.
const MaxAccessRequestListing = maxAccessRequestListing

// ListAccessRequests returns the operator's queue, each row with the
// account's current grant: pending requests oldest first (review order), or
// with all every request NEWEST first, so the bound drops the oldest history
// rather than the latest activity. truncated reports that more rows exist
// than the bound returned. The new-request email is a notification, not the
// system of record.
func (m *Module) ListAccessRequests(ctx context.Context, all bool) (reqs []AccessRequestListing, truncated bool, err error) {
	order := "r.created_at, r.id"
	if all {
		order = "r.created_at DESC, r.id DESC"
	}
	rows, err := m.pool.Query(ctx, `
		SELECT r.id, r.user_id, r.state, r.created_at, r.decided_at, r.expected_daily_volume,
		       COALESCE(c.external_sending_approved, false)
		  FROM external_sending_access_requests AS r
		  LEFT JOIN account_sending_controls AS c ON c.user_id = r.user_id
		 WHERE $1 OR r.state = 'pending'
		 ORDER BY `+order+`
		 LIMIT $2`, all, maxAccessRequestListing+1)
	if err != nil {
		return nil, false, fmt.Errorf("sendingpolicy: list sending access requests: %w", err)
	}
	defer rows.Close()
	var out []AccessRequestListing
	for rows.Next() {
		var l AccessRequestListing
		if err := rows.Scan(&l.ID, &l.AccountID, &l.State, &l.CreatedAt, &l.DecidedAt, &l.ExpectedDailyVolume, &l.Approved); err != nil {
			return nil, false, fmt.Errorf("sendingpolicy: scan sending access request: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("sendingpolicy: list sending access requests: %w", err)
	}
	if len(out) > maxAccessRequestListing {
		return out[:maxAccessRequestListing], true, nil
	}
	return out, false, nil
}
