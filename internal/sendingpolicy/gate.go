package sendingpolicy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Gate is the provider-authorization surface: the only way anything in this
// codebase is permitted to hand a message to SES.
//
// The shape is deliberate. A caller cannot ask "am I allowed?" and then act on
// the answer later, because an allow is not a boolean — it is a single-use
// ProviderAuthorization bound to one durable attempt, which the SMTP adapter
// must redeem immediately before it opens a socket. That removes the entire
// class of bug where a decision goes stale between the check and the call: a
// pause, a plan downgrade, a policy change, a midnight rollover, or a
// competing worker all invalidate the token rather than being raced.
type Gate interface {
	PrepareExternalTx(ctx context.Context, tx pgx.Tx, messageID string) (AcceptanceDecision, OperationRef, error)
	PrepareNotificationTx(context.Context, pgx.Tx, NotificationRef) (OperationRef, error)
	PrepareProtectionNoticeTx(context.Context, pgx.Tx, ProtectionNoticeRef) (OperationRef, error)
	PreparePublicFeedback(context.Context, PublicFeedbackRef) (OperationRef, error)
	Reserve(context.Context, OperationRef) (Decision, AttemptRef, error)
	ConsumeAttempt(context.Context, AttemptRef) (Decision, *ProviderAuthorization, error)
	RedeemProviderCall(context.Context, ProviderAuthorization) error
	DeferAttempt(context.Context, AttemptRef) error
	CancelAttempt(context.Context, AttemptRef) error
	SettleProvider(context.Context, ProviderSettlement) error
}

var _ Gate = (*Module)(nil)

// NewGate binds the module to the deployment's policy authority and returns it
// as the narrow provider-authorization role.
//
// The source is a constructor argument rather than a runtime lookup because it
// decides where authority lives, and that must not be able to change under a
// decision in flight. A hosted deployment reads the audited database singleton;
// a self-host reads the config file it already validated at startup.
func NewGate(pool *pgxpool.Pool, secrets Secrets, source PolicySource, configPolicy RuntimePolicy) Gate {
	m := NewModule(pool, secrets)
	m.source = source
	m.configPolicy = configPolicy
	return m
}

// Sentinel errors for the authorization surface.
var (
	// ErrAttemptStale means the reference names an attempt that is no longer
	// the operation's current one, or one whose state forbids the requested
	// transition. It is always zero writes to the ledger and zero provider
	// calls.
	ErrAttemptStale = errors.New("sendingpolicy: attempt is stale")
	// ErrProviderCallStarted means the attempt already opened a socket, so its
	// capacity cannot be given back. Retrying needs a new ordinal, not this one.
	ErrProviderCallStarted = errors.New("sendingpolicy: provider call already started")
	// ErrAuthorizationInvalid means a redemption failed its final recheck. The
	// attempt is invalidated and a strictly greater ordinal must be allocated
	// before any provider call.
	ErrAuthorizationInvalid = errors.New("sendingpolicy: provider authorization is no longer valid")
	// ErrEnvelopeUnavailable means the operation's authorized envelope could
	// not be resolved from durable state or the caller's reference.
	ErrEnvelopeUnavailable = errors.New("sendingpolicy: authorized envelope is unavailable")
)

// effectivePolicy reads the policy that governs this transaction.
//
// Database source takes a share lock on the singleton, which is the first key
// in the normative lock order: a concurrent activation therefore either
// completes before this decision reads it or waits until after the decision
// commits. There is no window in which half a decision uses the old generation
// and half the new one. Config source has no row and takes no lock — a
// self-host's policy changes by redeploy, which is already a restart.
func (m *Module) effectivePolicy(ctx context.Context, tx pgx.Tx) (RuntimePolicy, error) {
	if m.source == PolicySourceDatabase {
		return m.currentPolicyForShare(ctx, tx)
	}
	return m.configPolicy, nil
}

// reservationRow is one row of sending_budget_reservations.
type reservationRow struct {
	OperationID      string
	Attempt          int
	SourceAccountRef *string
	PolicySubjectRef string
	Purpose          Purpose
	Day              time.Time
	Units            int
	Probation        bool
	State            string
	CallState        string
	Nonce            *string
	NoticeVersion    *int
	NoticeCommitment []byte
	Exists           bool
}

// scopeKeys returns the counters this stored reservation charged, using only
// values persisted on the row itself.
//
// It deliberately does not consult the current policy. The units were taken
// under whatever generation was in force at the time, and giving them back has
// to target exactly those rows — otherwise a domain that graduated out of
// probation, or a control that was disarmed, would leak reserved capacity until
// midnight.
func (r reservationRow) scopeKeys(shared bool) ([]counterKey, error) {
	account := ""
	if r.SourceAccountRef != nil {
		account = *r.SourceAccountRef
	}
	return scopeKeys(r.Purpose, account, shared, r.Probation)
}

// lockReservation reads and locks one attempt row.
func lockReservation(ctx context.Context, tx pgx.Tx, operationID string, attempt int) (reservationRow, error) {
	var r reservationRow
	err := tx.QueryRow(ctx, `
		SELECT operation_id, submission_attempt, source_account_ref, policy_subject_ref,
		       purpose, day, units, probation, state, call_state, authorization_nonce,
		       notice_recipient_version, notice_recipient_commitment
		  FROM sending_budget_reservations
		 WHERE operation_id = $1 AND submission_attempt = $2
		   FOR UPDATE`, operationID, attempt,
	).Scan(&r.OperationID, &r.Attempt, &r.SourceAccountRef, &r.PolicySubjectRef,
		&r.Purpose, &r.Day, &r.Units, &r.Probation, &r.State, &r.CallState, &r.Nonce,
		&r.NoticeVersion, &r.NoticeCommitment)
	if errors.Is(err, pgx.ErrNoRows) {
		return reservationRow{}, nil
	}
	if err != nil {
		return reservationRow{}, fmt.Errorf("sendingpolicy: lock reservation: %w", err)
	}
	r.Day = r.Day.UTC()
	r.Exists = true
	return r, nil
}

// messageEnvelope reads a customer message's provider-bound recipient set.
//
// To, Cc, and Bcc all become envelope recipients at the SMTP layer, so all
// three are charged. Bcc especially: it is invisible in the message body and is
// exactly how a naive accounting would undercount a fan-out by an order of
// magnitude.
func messageEnvelope(ctx context.Context, tx pgx.Tx, messageID string) ([]string, error) {
	envelope, _, err := messageEnvelopeAndClass(ctx, tx, messageID)
	return envelope, err
}

// messageEnvelopeAndClass also reports the message's CURRENT reputation class,
// so final authorization can notice that it stopped matching the immutable one
// the operation was derived from.
func messageEnvelopeAndClass(ctx context.Context, tx pgx.Tx, messageID string) ([]string, bool, error) {
	var to, cc, bcc []string
	var sentAs *string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(to_recipients, '{}'), COALESCE(cc, '{}'), COALESCE(bcc, '{}'), sent_as
		  FROM messages
		 WHERE id = $1 AND direction = 'outbound'`, messageID,
	).Scan(&to, &cc, &bcc, &sentAs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrSourceUnavailable
	}
	if err != nil {
		return nil, false, fmt.Errorf("sendingpolicy: read message envelope: %w", err)
	}
	all := make([]string, 0, len(to)+len(cc)+len(bcc))
	all = append(all, to...)
	all = append(all, cc...)
	all = append(all, bcc...)
	envelope, err := normalizeEnvelope(all)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrEnvelopeUnavailable, err)
	}
	return envelope, sharedFromSentAs(sentAs), nil
}

// plannedUnits is how much capacity an attempt intends to take.
//
// Reserve needs a size before anything is resolved, and the size must not be
// larger than what final authorization will actually charge — an over-estimate
// would hold capacity that is never used until midnight. Notice and
// notification mail is exactly one recipient by construction; customer mail is
// its own deduplicated envelope; public feedback is its fixed configured set.
func plannedUnits(ctx context.Context, tx pgx.Tx, op operationRow, carried []string) (int, error) {
	switch op.Purpose {
	case PurposeCustomerMessage:
		envelope, err := messageEnvelope(ctx, tx, op.OperationID)
		if err != nil {
			return 0, err
		}
		return len(envelope), nil
	case PurposeCustomerNotification, PurposeCriticalOperational, PurposeViolationOperational:
		return 1, nil
	case PurposePublicFeedback, PurposeTrustedSystem:
		envelope, err := normalizeEnvelope(carried)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrEnvelopeUnavailable, err)
		}
		return len(envelope), nil
	}
	return 0, fmt.Errorf("sendingpolicy: unsupported purpose %q", op.Purpose)
}

// Reserve allocates this operation's current durable submission attempt and,
// as an optimization, tries to take its capacity early.
//
// It is explicitly not authority to submit. Its value is that a worker learns
// about an exhausted budget before it does the expensive work of composing and
// signing a message, and that the durable ordinal is allocated exactly once per
// provider opportunity. That ordinal allocation is the load-bearing half: after
// a crash, timeout, ambiguous SMTP result, or ordinary River retry, the next
// execution sees a confirmed row and allocates N+1 rather than reusing an
// ordinal that may already have reached the network.
func (m *Module) Reserve(ctx context.Context, ref OperationRef) (Decision, AttemptRef, error) {
	if ref.IsZero() {
		return Decision{}, AttemptRef{}, ErrSourceUnavailable
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return Decision{}, AttemptRef{}, fmt.Errorf("sendingpolicy: begin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	policy, err := m.effectivePolicy(ctx, tx)
	if err != nil {
		return Decision{}, AttemptRef{}, err
	}

	// Reserve judges the same caps ConsumeAttempt will, using the same
	// authoritative inputs, and that is not optional.
	//
	// An earlier version guessed instead: it skipped these reads and widened
	// the account cap so it could never under-admit a paid account. That trade
	// was wrong in both directions. Judging the ACCOUNT scope optimistically
	// while still charging the SHARED global pools at their real limits let one
	// Free account hold the entire platform pool in reservations it could never
	// confirm — starving every other tenant and paging the operator with a
	// guardrail incident. And judging it pessimistically deferred a paying
	// customer's mail to the next midnight, because the worker treats an early
	// hold as "snooze". The only correct answer is to read the class and the
	// plan, which costs one FOR SHARE apiece.
	var probeAccount string
	var probePurpose Purpose
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(source_account_ref, ''), purpose
		  FROM sending_provider_operations
		 WHERE operation_id = $1`, ref.id,
	).Scan(&probeAccount, &probePurpose); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Decision{}, AttemptRef{}, ErrSourceUnavailable
		}
		return Decision{}, AttemptRef{}, fmt.Errorf("sendingpolicy: read operation: %w", err)
	}

	var accountClass, planCode string
	if probePurpose.isCustomer() && probeAccount != "" {
		if err := tx.QueryRow(ctx,
			`SELECT account_class FROM users WHERE id = $1 FOR SHARE`, probeAccount,
		).Scan(&accountClass); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Decision{}, AttemptRef{}, fmt.Errorf("sendingpolicy: lock user: %w", err)
		}
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(plan_code, '') FROM account_limits WHERE user_id = $1 FOR SHARE`, probeAccount,
		).Scan(&planCode); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Decision{}, AttemptRef{}, fmt.Errorf("sendingpolicy: lock account limits: %w", err)
		}
	}

	op, err := lockOperation(ctx, tx, ref.id)
	if err != nil {
		return Decision{}, AttemptRef{}, err
	}

	attempt, current, err := allocateAttempt(ctx, tx, op)
	if err != nil {
		return Decision{}, AttemptRef{}, err
	}
	out := AttemptRef{operationID: op.OperationID, attempt: attempt, recipients: ref.recipients}

	// An attempt that already holds its capacity is not re-charged. Reserve is
	// idempotent per ordinal precisely so a worker that is retried before it
	// ever reached ConsumeAttempt does not accumulate reservations.
	if current.Exists && current.Attempt == attempt && current.State == "reserved" {
		return allowDecision(), out, m.commit(ctx, tx, "reserve")
	}

	units, err := plannedUnits(ctx, tx, op, ref.recipients)
	if err != nil {
		return Decision{}, AttemptRef{}, err
	}

	day, err := ledgerDay(ctx, tx)
	if err != nil {
		return Decision{}, AttemptRef{}, err
	}

	// Probation classification. Shared-domain traffic is probationary at every
	// plan level and never graduates by age or payment — higher-volume
	// initiated sending requires a customer-controlled domain. Custom-domain
	// probation is the custom-domain ramp's answer, composed into this
	// decision by Task 4; until then a dedicated domain is not probationary,
	// which is correct while the ramp itself is pass-through.
	probation := op.Shared

	// Trusted first-party accounts charge nothing, and that has to be true
	// HERE and not only at final authorization. The exemption exists so
	// probers keep working during the abuse wave that exhausted the customer
	// pools — but an early hold makes the worker snooze without ever reaching
	// ConsumeAttempt, so an exemption that lives only there is dead on the one
	// path it was written for.
	exempt := accountClassExempt(accountClass) && op.Purpose.isCustomer()
	charged := false
	deniedScope := Scope("")
	if policy.BudgetMode == ModeEnforce && !exempt {
		keys, err := scopeKeys(op.Purpose, op.accountRef(), op.Shared, probation)
		if err != nil {
			return Decision{}, AttemptRef{}, err
		}
		create := make(map[ledgerRef]int, len(keys))
		for _, key := range keys {
			create[ledgerRef{counterKey: key, Day: day}] = limitFor(key, policy, planCode)
		}
		plan, err := lockLedger(ctx, tx, create, nil)
		if err != nil {
			return Decision{}, AttemptRef{}, err
		}
		for _, key := range keys {
			if !plan.acquire(ledgerRef{counterKey: key, Day: day}, units) {
				deniedScope = key.Scope
				break
			}
		}
		if deniedScope == "" {
			if err := plan.flush(ctx, tx); err != nil {
				return Decision{}, AttemptRef{}, err
			}
			charged = true
		}
		// On a denial the plan's deltas are simply never flushed, so the
		// ledger is left exactly as it was found.
	}

	// The row records that capacity is HELD only when it actually is. Writing
	// `reserved` for an attempt that charged nothing — because the budget was
	// disabled, or the account is exempt, or the acquisition was denied — would
	// make a later ConsumeAttempt release units this attempt never took, and
	// on the day enforcement is first armed that phantom release is capacity
	// silently handed back to whoever else is in flight.
	state := "released"
	if charged {
		state = "reserved"
	}
	if err := upsertReservation(ctx, tx, op, attempt, day, units, probation, state); err != nil {
		return Decision{}, AttemptRef{}, err
	}

	if deniedScope != "" {
		// The violation notice is owed HERE, not only at final authorization.
		// Every scope except account_daily is judged identically by both
		// calls, so a denial that stops at Reserve is a denial the customer and
		// the operator would otherwise never hear about — the worker snoozes
		// and ConsumeAttempt, where the notice used to be written, is never
		// reached.
		if err := m.enqueueDenialNotice(ctx, tx, policy, op, day, deniedScope); err != nil {
			return Decision{}, AttemptRef{}, err
		}
	}

	if err := m.commit(ctx, tx, "reserve"); err != nil {
		return Decision{}, AttemptRef{}, err
	}
	if deniedScope != "" {
		return holdDecision(holdReasonForScope(deniedScope), nextUTCMidnight(day)), out, nil
	}
	return allowDecision(), out, nil
}

// allocateAttempt returns the ordinal this operation's next provider
// opportunity must use, advancing the durable counter when the current attempt
// has already been authorized.
//
// The rule is one-way: an ordinal is reusable only while it is provably before
// any network I/O. A confirmed reservation means capacity was irrevocably spent
// and a socket may have been opened, so the only safe continuation is a greater
// ordinal with fresh capacity. That is what bounds physical SES exposure to the
// charged amount even across crashes.
func allocateAttempt(ctx context.Context, tx pgx.Tx, op operationRow) (int, reservationRow, error) {
	current, err := lockReservation(ctx, tx, op.OperationID, op.CurrentAttempt)
	if err != nil {
		return 0, reservationRow{}, err
	}
	if !current.Exists || current.State == "reserved" || (current.State == "released" && current.CallState == "none") {
		return op.CurrentAttempt, current, nil
	}

	next := op.CurrentAttempt + 1
	if _, err := tx.Exec(ctx, `
		UPDATE sending_provider_operations
		   SET current_attempt = $2, updated_at = now()
		 WHERE operation_id = $1`, op.OperationID, next,
	); err != nil {
		return 0, reservationRow{}, fmt.Errorf("sendingpolicy: advance attempt: %w", err)
	}
	advanced, err := lockReservation(ctx, tx, op.OperationID, next)
	if err != nil {
		return 0, reservationRow{}, err
	}
	return next, advanced, nil
}

// upsertReservation writes the attempt row in a pre-provider state.
func upsertReservation(ctx context.Context, tx pgx.Tx, op operationRow, attempt int, day time.Time, units int, probation bool, state string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO sending_budget_reservations
		    (operation_id, submission_attempt, source_account_ref, policy_subject_ref,
		     purpose, day, units, probation, state, call_state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'none')
		ON CONFLICT (operation_id, submission_attempt) DO UPDATE
		   SET day = EXCLUDED.day,
		       units = EXCLUDED.units,
		       probation = EXCLUDED.probation,
		       state = EXCLUDED.state,
		       call_state = 'none',
		       authorization_nonce = NULL,
		       provider_call_started_at = NULL,
		       updated_at = now()`,
		op.OperationID, attempt, op.SourceAccountRef, op.PolicySubjectRef,
		op.Purpose, day, units, probation, state,
	)
	if err != nil {
		return fmt.Errorf("sendingpolicy: write reservation: %w", err)
	}
	return nil
}

// commit wraps tx.Commit with a named error so a failure is attributable.
func (m *Module) commit(ctx context.Context, tx pgx.Tx, what string) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("sendingpolicy: commit %s: %w", what, err)
	}
	return nil
}

// authState is everything ConsumeAttempt read under lock, in one value, so the
// decision logic below reads as policy rather than as SQL.
type authState struct {
	policy    RuntimePolicy
	op        operationRow
	stored    reservationRow
	day       time.Time
	units     int
	probation bool
	planCode  string
	class     string

	tenantMode TenantMode
	tenantName string

	envelope []string
	notice   *noticeState
}

// noticeState is the protection-notice half of a final authorization.
type noticeState struct {
	eventID         string
	audience        Audience
	deliveryAttempt int
	state           string
	version         int
	commitment      []byte
}

// ConsumeAttempt is the final pre-I/O authorization: the one place where the
// account's live state, the current policy generation, today's UTC date, and
// every applicable pool are checked together under lock.
//
// It is a full re-evaluation, not a confirmation of what Reserve decided.
// Everything Reserve saw may have changed: the policy can have been activated,
// the plan downgraded, the account paused, the day rolled over, a control armed
// or disarmed. Re-deriving from scratch — including releasing units Reserve
// took on scopes that no longer apply — is what makes a policy change between
// the two calls impossible to slip past.
func (m *Module) ConsumeAttempt(ctx context.Context, ref AttemptRef) (Decision, *ProviderAuthorization, error) {
	if ref.IsZero() {
		return Decision{}, nil, ErrSourceUnavailable
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return Decision{}, nil, fmt.Errorf("sendingpolicy: begin consume: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	state, decision, err := m.readAuthState(ctx, tx, ref)
	if err != nil {
		return Decision{}, nil, err
	}
	if !decision.Allow {
		// A state-based hold (pause, deleted owner, tenant not ready) still
		// has to give back whatever the early reservation is holding, or the
		// account loses that capacity until midnight for a message it never
		// sent.
		if err := m.releaseStoredUnits(ctx, tx, state); err != nil {
			return Decision{}, nil, err
		}
		if err := m.commit(ctx, tx, "consume hold"); err != nil {
			return Decision{}, nil, err
		}
		return decision, nil, nil
	}

	decision, err = m.reauthorizeBudget(ctx, tx, state)
	if err != nil {
		return Decision{}, nil, err
	}
	if !decision.Allow {
		if err := m.commit(ctx, tx, "consume budget hold"); err != nil {
			return Decision{}, nil, err
		}
		return decision, nil, nil
	}

	auth, err := m.authorize(ctx, tx, state)
	if err != nil {
		return Decision{}, nil, err
	}
	if err := m.commit(ctx, tx, "consume authorize"); err != nil {
		return Decision{}, nil, err
	}
	return allowDecision(), auth, nil
}

// readAuthState takes every lock in the normative order and returns either the
// assembled state or a state-based hold.
//
// The order is runtime policy → users → account control → plan → notice
// delivery → provider operation → attempt, and it is the same order every
// mutating path in this package uses. It is not arbitrary: locking the policy
// first means an activation serializes against every decision; locking the user
// row before the control row means a class change and a pause cannot
// interleave; locking the plan under the same contract the billing writer uses
// means a plan commit and an authorization cannot both believe they were first.
//
// Every lock is taken before any verdict is formed. Returning a hold early
// would skip the attempt lock, and the caller could then not give back the
// units an earlier Reserve is holding — the account would lose that capacity
// until midnight for a message it never sent.
func (m *Module) readAuthState(ctx context.Context, tx pgx.Tx, ref AttemptRef) (authState, Decision, error) {
	var st authState

	policy, err := m.effectivePolicy(ctx, tx)
	if err != nil {
		return st, Decision{}, err
	}
	st.policy = policy
	// Every operation has a tenant mode; only a customer purpose can have it
	// raised. Defaulting here rather than per-branch keeps the provenance row's
	// closed enum satisfiable for the paths that never consider a tenant.
	st.tenantMode = TenantModeNone

	// Unlocked probe: the account and purpose decide WHICH keys this
	// transaction must lock, so they have to be known before the ordered
	// locking begins. Nothing here is used for a verdict — every value a
	// decision rests on is re-read from the locked rows below.
	var probeAccount string
	var probePurpose Purpose
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(source_account_ref, ''), purpose
		  FROM sending_provider_operations
		 WHERE operation_id = $1`, ref.operationID,
	).Scan(&probeAccount, &probePurpose); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return st, Decision{}, ErrSourceUnavailable
		}
		return st, Decision{}, fmt.Errorf("sendingpolicy: read operation: %w", err)
	}

	// The user row is locked for two different reasons, and only one of them
	// is about authority. Customer traffic needs the class and the account's
	// live state; an owner-audience notice needs only the current address, and
	// deliberately does NOT consult that account's control row — an account
	// that was just paused must still receive the email saying so.
	var (
		ownerEmail   string
		ownerExists  bool
		controlState = "active"
		tenantReady  bool
		tenantName   string
	)
	needsOwner := probeAccount != "" && (probePurpose.isCustomer() || probePurpose.isOperational())
	if needsOwner {
		// FOR SHARE, not FOR UPDATE: many concurrent authorizations for one
		// account must proceed together, while an ordinary `UPDATE users` that
		// changes account_class or the owner address takes the conflicting
		// lock and serializes at exactly this boundary.
		err := tx.QueryRow(ctx,
			`SELECT account_class, email FROM users WHERE id = $1 FOR SHARE`, probeAccount,
		).Scan(&st.class, &ownerEmail)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			ownerExists = false
		case err != nil:
			return st, Decision{}, fmt.Errorf("sendingpolicy: lock user: %w", err)
		default:
			ownerExists = true
		}
	}

	if probePurpose.isCustomer() && ownerExists {
		controlState, tenantName, tenantReady, err = ensureAccountControl(ctx, tx, probeAccount)
		if err != nil {
			return st, Decision{}, err
		}

		// The authoritative plan, read directly rather than through the limits
		// enforcer's 60-second cache. A cached plan is fine for a quota error
		// message and unacceptable here: a downgrade that committed 40 seconds
		// ago must bind this decision, and the billing writer takes the same
		// account-control lock so the two serialize.
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(plan_code, '') FROM account_limits WHERE user_id = $1 FOR SHARE`, probeAccount,
		).Scan(&st.planCode); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return st, Decision{}, fmt.Errorf("sendingpolicy: lock account limits: %w", err)
		}
		st.tenantMode, st.tenantName = tenantModeFor(policy, probePurpose, probeAccount, tenantName)
	}
	if !probePurpose.isCustomer() {
		st.tenantMode, st.tenantName = tenantModeFor(policy, probePurpose, "", "")
	}

	// Notice delivery, when this operation is one. Locked before the provider
	// operation so the drain worker and a concurrent notice mutation agree on
	// order.
	notice, err := m.lockNoticeDelivery(ctx, tx, ref.operationID)
	if err != nil {
		return st, Decision{}, err
	}
	st.notice = notice

	op, err := lockOperation(ctx, tx, ref.operationID)
	if err != nil {
		return st, Decision{}, err
	}
	st.op = op

	stored, err := lockReservation(ctx, tx, ref.operationID, ref.attempt)
	if err != nil {
		return st, Decision{}, err
	}
	st.stored = stored

	// A reference that is not the operation's current ordinal is stale by
	// definition, and a confirmed attempt has already been authorized once.
	// Two workers that both observed reserved ordinal N therefore cannot both
	// get a token: the second finds it confirmed and stops here.
	if ref.attempt != op.CurrentAttempt {
		return st, Decision{}, ErrAttemptStale
	}
	if stored.Exists && stored.State == "confirmed" {
		return st, Decision{}, ErrAttemptStale
	}

	st.day, err = ledgerDay(ctx, tx)
	if err != nil {
		return st, Decision{}, err
	}
	st.probation = op.Shared

	// Verdicts, now that every key is held.
	//
	// Pause is checked independently of the budget mode. A paused account is
	// explicit operator or detector state, not a computed limit, and an
	// account paused for abuse must stop sending even in a deployment that has
	// not armed budgets yet. This is where the pause guarantee linearizes: a
	// pause that commits first prevents authorization, and an authorization
	// that commits first may already be entering its one SES call.
	if op.Purpose.isCustomer() {
		if !ownerExists {
			return st, terminalHold(ReasonAccountDeleted), nil
		}
		if controlState == "paused" {
			return st, holdDecision(ReasonAccountPaused, time.Time{}), nil
		}
		if st.tenantMode == TenantModeRequired && !tenantReady {
			return st, holdDecision(ReasonTenantNotReady, time.Time{}), nil
		}
	}
	// A required header with no name to put in it is not a header. The adapter
	// would have to either omit it or send an empty value, and both defeat the
	// isolation the header exists for, so the send waits for a real tenant.
	if st.tenantMode == TenantModeRequired && strings.TrimSpace(st.tenantName) == "" {
		return st, holdDecision(ReasonTenantUnnamed, time.Time{}), nil
	}

	// A delivery that already reached a terminal state must not be re-armed.
	// The stable-operation design guarantees a retry is a greater ordinal on
	// one logical notice; without this check it also permits a SECOND logical
	// notice for a delivery already marked sent, which is the exact duplicate
	// this module claims to make impossible.
	if notice != nil && notice.state != "pending" {
		return st, terminalHold(ReasonNoticeSettled), nil
	}

	if notice != nil && notice.audience == AudienceOwner && !ownerExists {
		// The account this notice was about is gone. There is nobody to tell,
		// so the delivery is closed rather than retried forever.
		if err := m.markNoticeSkipped(ctx, tx, notice); err != nil {
			return st, Decision{}, err
		}
		return st, terminalHold(ReasonAccountDeleted), nil
	}

	// The reputation class is immutable on the operation, but `sent_as` is not
	// immutable on the message: the approval path rewrites it, and it depends
	// on the domain's live verification state. If the operation was prepared
	// while the customer's own domain was sending-verified and the message now
	// goes out over the shared relay, sending it would put shared traffic
	// through a dedicated budget — escaping both the 50/day shared cap and the
	// probation pool. (Tightening the other way needs no action: an operation
	// already classed as shared stays shared.)
	//
	// TERMINAL, not a snooze. shared_reputation is frozen at operation creation
	// and the operation is keyed by message id, so there is no future in which
	// this operation describes that message again; a retryable hold would
	// leave the message stuck until a human noticed. The caller fails it — the
	// lifecycle catalog's submission.sending_setup_expired is the fitting code
	// — and prepares a fresh operation if the send should still happen.
	//
	// One read of the message row serves both this check and the envelope,
	// rather than resolving the same row twice per authorization.
	envelope, envErr := m.resolveEnvelopeAndClass(ctx, tx, op, ref, notice, ownerEmail)
	if envErr != nil {
		// A source that has been deleted, or an envelope that no longer
		// resolves, is a HOLD and not an error. Returning an error here rolls
		// the transaction back with the attempt still marked reserved, and
		// since every later execution fails at the same point, those units are
		// stranded on the SHARED pools until midnight with nothing able to
		// release them. A caller could farm that: reserve, delete, repeat.
		if errors.Is(envErr, ErrSourceUnavailable) || errors.Is(envErr, ErrEnvelopeUnavailable) {
			return st, terminalHold(ReasonSourceUnavailable), nil
		}
		if errors.Is(envErr, errReputationClassChanged) {
			return st, terminalHold(ReasonClassChanged), nil
		}
		return st, Decision{}, envErr
	}
	st.envelope = envelope
	st.units = len(st.envelope)
	return st, allowDecision(), nil
}

// tenantModeFor resolves the tenant-header mode for one account.
//
// Canary is an explicit account list rather than a percentage: a tenant header
// is either sent or not, and the rollout gate wants a named account it can
// verify end to end, not a sample it has to go looking for.
func tenantModeFor(policy RuntimePolicy, purpose Purpose, accountID, tenantName string) (TenantMode, string) {
	// Operational and public-feedback mail is not a customer's traffic and has
	// no customer tenant; it uses the fixed system tenant. Returning "no
	// tenant" for them under an enforcing policy would fail OPEN — the one
	// direction a header whose purpose is provider-side isolation must never
	// fail.
	if !purpose.isCustomer() {
		if policy.TenantHeaderMode == TenantHeaderEnforce {
			return TenantModeRequired, SystemPolicySubject
		}
		return TenantModeNone, ""
	}
	switch policy.TenantHeaderMode {
	case TenantHeaderEnforce:
		return TenantModeRequired, tenantName
	case TenantHeaderCanary:
		for _, id := range policy.TenantHeaderCanaryAccountIDs {
			if id == accountID {
				return TenantModeRequired, tenantName
			}
		}
	}
	return TenantModeNone, ""
}

// lockNoticeDelivery locks the delivery row this operation serves, if any.
func (m *Module) lockNoticeDelivery(ctx context.Context, tx pgx.Tx, operationID string) (*noticeState, error) {
	var st noticeState
	var audience, state string
	err := tx.QueryRow(ctx, `
		SELECT event_id, audience, delivery_attempt, state
		  FROM sending_protection_notice_deliveries
		 WHERE current_operation_id = $1
		   FOR UPDATE`, operationID,
	).Scan(&st.eventID, &audience, &st.deliveryAttempt, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: lock notice delivery: %w", err)
	}
	st.audience = Audience(audience)
	st.state = state
	return &st, nil
}

// markNoticeSkipped closes a delivery whose owner no longer exists.
func (m *Module) markNoticeSkipped(ctx context.Context, tx pgx.Tx, notice *noticeState) error {
	if _, err := tx.Exec(ctx, `
		UPDATE sending_protection_notice_deliveries
		   SET state = 'skipped_account_deleted', updated_at = now()
		 WHERE event_id = $1 AND audience = $2`,
		notice.eventID, string(notice.audience),
	); err != nil {
		return fmt.Errorf("sendingpolicy: close notice delivery: %w", err)
	}
	return nil
}

// errReputationClassChanged marks a customer message whose class no longer
// matches the operation derived from it.
var errReputationClassChanged = errors.New("sendingpolicy: reputation class changed after preparation")

// resolveEnvelopeAndClass resolves the envelope and, for a customer message,
// re-checks that the message still belongs to the reputation class its
// operation was frozen with.
//
// The two are one function because they are one read. Splitting them is what
// had this path querying the same message row twice per authorization.
func (m *Module) resolveEnvelopeAndClass(ctx context.Context, tx pgx.Tx, op operationRow, ref AttemptRef, notice *noticeState, ownerEmail string) ([]string, error) {
	if op.Purpose != PurposeCustomerMessage {
		return m.resolveEnvelope(ctx, tx, op, ref, notice, ownerEmail)
	}
	envelope, nowShared, err := messageEnvelopeAndClass(ctx, tx, op.OperationID)
	if err != nil {
		return nil, err
	}
	if nowShared && !op.Shared {
		return nil, errReputationClassChanged
	}
	return envelope, nil
}

// resolveEnvelope produces the exact final recipient set, under the locks that
// were just taken.
//
// Resolution happens here and not at preparation because the answer can change:
// an account owner can edit their address between the pause being decided and
// the notice being sent, and the operator mailbox map can be rotated. Binding
// the address at the last locked moment is what guarantees no notice is mailed
// to a retired address.
func (m *Module) resolveEnvelope(ctx context.Context, tx pgx.Tx, op operationRow, ref AttemptRef, notice *noticeState, ownerEmail string) ([]string, error) {
	switch {
	case notice != nil && notice.audience == AudienceOperator:
		version, err := m.policyOperatorVersion(ctx, tx)
		if err != nil {
			return nil, err
		}
		if m.secrets.Recipients == nil {
			return nil, fmt.Errorf("%w: no operator recipient map is loaded", ErrEnvelopeUnavailable)
		}
		if err := m.requireSelectedOperatorRecipient(ctx, tx, version); err != nil {
			return nil, err
		}
		mailbox, ok := m.secrets.Recipients.Mailbox(version)
		if !ok {
			return nil, ErrOperatorRecipientUnavailable
		}
		commitment, _ := m.secrets.Recipients.Commitment(version)
		notice.version = version
		notice.commitment = []byte(commitment)
		return normalizeEnvelope([]string{mailbox})

	case notice != nil:
		return normalizeEnvelope([]string{ownerEmail})

	case op.Purpose == PurposeCustomerMessage:
		return messageEnvelope(ctx, tx, op.OperationID)

	case op.Purpose == PurposeCustomerNotification:
		if ownerEmail == "" {
			return nil, fmt.Errorf("%w: the notified account has no address", ErrEnvelopeUnavailable)
		}
		return normalizeEnvelope([]string{ownerEmail})

	case op.Purpose == PurposePublicFeedback || op.Purpose == PurposeTrustedSystem:
		envelope, err := normalizeEnvelope(ref.recipients)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrEnvelopeUnavailable, err)
		}
		return envelope, nil
	}
	return nil, fmt.Errorf("sendingpolicy: unsupported purpose %q", op.Purpose)
}

// policyOperatorVersion returns the recipient version the current policy
// selects. The database source is authoritative when it is in use.
// policyOperatorVersion returns the recipient version the current policy
// selects.
//
// A read failure is propagated rather than absorbed. Falling back to the config
// value would, on a hosted deployment whose database has rotated the operator
// mailbox, silently select the RETIRED version — and because the redemption
// recheck calls this same helper, the recheck would confirm the stale answer
// instead of catching it. That is the one path whose whole guarantee is "zero
// calls to the retired address".
func (m *Module) policyOperatorVersion(ctx context.Context, tx pgx.Tx) (int, error) {
	if m.source == PolicySourceDatabase {
		policy, err := m.currentPolicyForShare(ctx, tx)
		if err != nil {
			return 0, err
		}
		return policy.OperatorNoticeRecipientVersion, nil
	}
	return m.configPolicy.OperatorNoticeRecipientVersion, nil
}

// releaseStoredUnits gives back whatever the stored attempt is holding.
func (m *Module) releaseStoredUnits(ctx context.Context, tx pgx.Tx, st authState) error {
	stored := st.stored
	if !stored.Exists || stored.State != "reserved" {
		return nil
	}
	oldKeys, err := stored.scopeKeys(st.op.Shared)
	if err != nil {
		return err
	}
	refs := refsFor(oldKeys, stored.Day)
	plan, err := lockLedger(ctx, tx, nil, refs)
	if err != nil {
		return err
	}
	for _, r := range refs {
		plan.release(r, stored.Units)
	}
	if err := plan.flush(ctx, tx); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE sending_budget_reservations
		   SET state = 'released', call_state = 'none', authorization_nonce = NULL,
		       provider_call_started_at = NULL, updated_at = now()
		 WHERE operation_id = $1 AND submission_attempt = $2`,
		stored.OperationID, stored.Attempt)
	if err != nil {
		return fmt.Errorf("sendingpolicy: release reservation: %w", err)
	}
	return nil
}

// reauthorizeBudget releases the attempt's stale units and takes exactly what
// the current generation, plan, class, and UTC day require.
//
// Both halves happen against one ordered set of locked rows, so the release and
// the acquisition cannot deadlock against each other or against another worker
// doing the mirror image. A denial leaves the attempt released rather than
// half-charged: a later fire-time pass re-arms it from scratch.
func (m *Module) reauthorizeBudget(ctx context.Context, tx pgx.Tx, st authState) (Decision, error) {
	stored := st.stored

	// Trusted first-party accounts and the disabled budget mode both mean "no
	// pool applies". They are handled together because the downstream effect
	// is identical: nothing is charged, and anything an earlier Reserve took
	// under a different policy is given back.
	exempt := accountClassExempt(st.class) && st.op.Purpose.isCustomer()
	if st.policy.BudgetMode == ModeDisabled || exempt || st.op.Purpose == PurposeTrustedSystem {
		return allowDecision(), m.releaseStoredUnits(ctx, tx, st)
	}

	currentKeys, err := scopeKeys(st.op.Purpose, st.op.accountRef(), st.op.Shared, st.probation)
	if err != nil {
		return Decision{}, err
	}
	create := make(map[ledgerRef]int, len(currentKeys))
	for _, key := range currentKeys {
		create[ledgerRef{counterKey: key, Day: st.day}] = limitFor(key, st.policy, st.planCode)
	}

	var releaseRefs []ledgerRef
	if stored.Exists && stored.State == "reserved" {
		storedKeys, err := stored.scopeKeys(st.op.Shared)
		if err != nil {
			return Decision{}, err
		}
		releaseRefs = refsFor(storedKeys, stored.Day)
	}

	plan, err := lockLedger(ctx, tx, create, releaseRefs)
	if err != nil {
		return Decision{}, err
	}
	for _, r := range releaseRefs {
		plan.release(r, stored.Units)
	}

	deniedScope := Scope("")
	for _, key := range currentKeys {
		if !plan.acquire(ledgerRef{counterKey: key, Day: st.day}, st.units) {
			deniedScope = key.Scope
			if st.policy.BudgetMode == ModeEnforce {
				break
			}
			// Shadow deliberately overruns. Clamping the counter at the limit
			// would make the shadow window prove only that the limit exists;
			// what the rollout gate needs is the real aggregate demand, which
			// is only visible if the counter is allowed past the cap.
			plan.overrun(ledgerRef{counterKey: key, Day: st.day}, st.units)
		}
	}

	if deniedScope != "" && st.policy.BudgetMode == ModeEnforce {
		// The release is kept; the acquisitions are discarded by not
		// flushing them. Re-lock nothing: the same plan is reused with only
		// the release deltas applied.
		plan.discardAcquisitions()
		if err := plan.flush(ctx, tx); err != nil {
			return Decision{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE sending_budget_reservations
			   SET state = 'released', call_state = 'none', authorization_nonce = NULL,
			       provider_call_started_at = NULL, day = $3, units = $4,
			       probation = $5, updated_at = now()
			 WHERE operation_id = $1 AND submission_attempt = $2`,
			st.op.OperationID, stored.Attempt, st.day, st.units, st.probation,
		); err != nil {
			return Decision{}, fmt.Errorf("sendingpolicy: release denied reservation: %w", err)
		}
		if err := m.enqueueDenialNotice(ctx, tx, st.policy, st.op, st.day, deniedScope); err != nil {
			return Decision{}, err
		}
		return holdDecision(holdReasonForScope(deniedScope), nextUTCMidnight(st.day)), nil
	}

	if deniedScope != "" {
		log.Printf("[sending-protection] shadow denial: purpose=%s scope=%s operation=%s units=%d",
			st.op.Purpose, deniedScope, st.op.OperationID, st.units)
	}
	if err := plan.flush(ctx, tx); err != nil {
		return Decision{}, err
	}
	return allowDecision(), nil
}

// authorize confirms the capacity, mints the single-use nonce, records the
// provenance correlation, and returns the token.
func (m *Module) authorize(ctx context.Context, tx pgx.Tx, st authState) (*ProviderAuthorization, error) {
	nonce := randomNonce()
	attempt := st.stored.Attempt
	if !st.stored.Exists {
		attempt = st.op.CurrentAttempt
	}

	var noticeVersion *int
	var noticeCommitment []byte
	var binding *noticeBinding
	if st.notice != nil {
		version, commitment, err := m.noticeCommitmentFor(st)
		if err != nil {
			// Reserve may already hold this attempt's units, and returning an
			// error here rolls back with the row still reserved — every retry
			// then fails identically and the capacity is stranded. Surface it
			// as the terminal hold it is so the caller releases and stops.
			return nil, err
		}
		noticeVersion, noticeCommitment = &version, commitment
		binding = &noticeBinding{
			eventID:             st.notice.eventID,
			audience:            st.notice.audience,
			deliveryAttempt:     st.notice.deliveryAttempt,
			recipientVersion:    version,
			recipientCommitment: commitment,
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO sending_budget_reservations
		    (operation_id, submission_attempt, source_account_ref, policy_subject_ref,
		     purpose, day, units, probation, state, call_state, authorization_nonce,
		     notice_recipient_version, notice_recipient_commitment)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'confirmed', 'authorized', $9, $10, $11)
		ON CONFLICT (operation_id, submission_attempt) DO UPDATE
		   SET day = EXCLUDED.day,
		       units = EXCLUDED.units,
		       probation = EXCLUDED.probation,
		       state = 'confirmed',
		       call_state = 'authorized',
		       authorization_nonce = EXCLUDED.authorization_nonce,
		       notice_recipient_version = EXCLUDED.notice_recipient_version,
		       notice_recipient_commitment = EXCLUDED.notice_recipient_commitment,
		       provider_call_started_at = NULL,
		       updated_at = now()`,
		st.op.OperationID, attempt, st.op.SourceAccountRef, st.op.PolicySubjectRef,
		st.op.Purpose, st.day, st.units, st.probation, nonce,
		noticeVersion, noticeCommitment,
	); err != nil {
		return nil, fmt.Errorf("sendingpolicy: confirm reservation: %w", err)
	}

	// Confirm the counters only after the reservation says confirmed, so a
	// failure anywhere in this transaction leaves both consistent.
	if err := m.confirmIfCharged(ctx, tx, st, attempt); err != nil {
		return nil, err
	}

	correlationID, err := m.recordCorrelation(ctx, tx, st, attempt)
	if err != nil {
		return nil, err
	}
	if st.notice != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE sending_protection_notice_deliveries
			   SET delivery_attempt = $3, updated_at = now()
			 WHERE event_id = $1 AND audience = $2`,
			st.notice.eventID, string(st.notice.audience), attempt,
		); err != nil {
			return nil, fmt.Errorf("sendingpolicy: advance notice delivery: %w", err)
		}
	}

	set := make(map[string]struct{}, len(st.envelope))
	for _, addr := range st.envelope {
		set[addr] = struct{}{}
	}
	return &ProviderAuthorization{
		attempt:       AttemptRef{operationID: st.op.OperationID, attempt: attempt},
		correlationID: correlationID,
		purpose:       st.op.Purpose,
		nonce:         nonce,
		recipients:    append([]string(nil), st.envelope...),
		recipientSet:  set,
		tenantMode:    st.tenantMode,
		tenantName:    st.tenantName,
		notice:        binding,
	}, nil
}

// noticeCommitmentFor returns the (version, commitment, hmac key version)
// triple a notice attempt persists.
//
// The two audiences use the same column pair for different proofs, which is
// what lets one durable shape cover both. An operator delivery commits to the
// non-secret logical version and its keyed commitment, so a rotation of the
// mailbox map invalidates the attempt. An owner delivery commits to the HMAC of
// the address itself under a named key version, so an owner who changes their
// email invalidates it. Neither stores an address.
func (m *Module) noticeCommitmentFor(st authState) (int, []byte, error) {
	if st.notice.audience == AudienceOperator {
		return st.notice.version, st.notice.commitment, nil
	}
	if m.secrets.Keyring == nil {
		return 0, nil, fmt.Errorf("%w: no feedback keyring is loaded", ErrEnvelopeUnavailable)
	}
	if len(st.envelope) != 1 {
		return 0, nil, fmt.Errorf("%w: an owner notice must have exactly one recipient", ErrEnvelopeUnavailable)
	}
	version, mac := m.secrets.Keyring.Sign([]byte(st.envelope[0]))
	return version, mac, nil
}

// confirmIfCharged moves this attempt's units from reserved to confirmed on
// every counter it actually charged.
func (m *Module) confirmIfCharged(ctx context.Context, tx pgx.Tx, st authState, attempt int) error {
	exempt := accountClassExempt(st.class) && st.op.Purpose.isCustomer()
	if st.policy.BudgetMode == ModeDisabled || exempt || st.op.Purpose == PurposeTrustedSystem {
		return nil
	}
	keys, err := scopeKeys(st.op.Purpose, st.op.accountRef(), st.op.Shared, st.probation)
	if err != nil {
		return err
	}
	return confirmCounters(ctx, tx, refsFor(keys, st.day), st.units)
}

// recordCorrelation writes the provenance row this attempt's delivery feedback
// will later be matched against, plus the keyed HMAC of every recipient.
//
// Recipients are stored only as HMACs. The detector needs to count outcomes per
// recipient, which requires a stable identifier, but it never needs to read an
// address — and a table of customer contact addresses that outlives the message
// is exactly the asset that must not exist.
func (m *Module) recordCorrelation(ctx context.Context, tx pgx.Tx, st authState, attempt int) (string, error) {
	// The DURABLE row decides the correlation ID, not the value this call
	// happened to mint. The ID travels to SES as a header and comes back on
	// every delivery event, so a token carrying an ID that no row holds would
	// produce feedback nothing could ever be matched to — the failure would be
	// invisible until the detector had been silently blind for days.
	// Customer correlations get no expiry here: they must outlive the message
	// for as long as the account exists, so that a controlled recipient cannot
	// wait out a fixed timer before complaining. The post-deletion janitor sets
	// their horizon. A non-customer operation has no account to outlive, so it
	// receives the configured horizon at creation.
	var expires *time.Time
	if !st.op.Purpose.isCustomer() {
		horizon := time.Now().UTC().Add(
			time.Duration(st.policy.SendingFeedbackPostAcctRetention) * 24 * time.Hour)
		expires = &horizon
	}

	var correlationID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO sending_feedback_correlations
		    (correlation_id, operation_id, submission_attempt, source_account_ref,
		     policy_subject_ref, purpose, shared_reputation, tenant_mode, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (operation_id, submission_attempt) DO UPDATE
		    SET operation_id = EXCLUDED.operation_id
		RETURNING correlation_id`,
		randomID("cor_"), st.op.OperationID, attempt, st.op.SourceAccountRef,
		st.op.PolicySubjectRef, st.op.Purpose, st.op.Shared, string(st.tenantMode), expires,
	).Scan(&correlationID); err != nil {
		return "", fmt.Errorf("sendingpolicy: record correlation: %w", err)
	}

	if m.secrets.Keyring == nil {
		// A self-host with every control disabled legitimately has no keyring;
		// there is nothing to correlate because no detector runs.
		return correlationID, nil
	}
	for _, addr := range st.envelope {
		version, mac := m.secrets.Keyring.Sign([]byte(addr))
		if _, err := tx.Exec(ctx, `
			INSERT INTO sending_feedback_recipients (correlation_id, recipient_hmac, hmac_key_version)
			VALUES ($1, $2, $3)
			ON CONFLICT (correlation_id, recipient_hmac) DO NOTHING`,
			correlationID, mac, version,
		); err != nil {
			return "", fmt.Errorf("sendingpolicy: record recipient provenance: %w", err)
		}
	}
	return correlationID, nil
}

// enqueueDenialNotice writes the notice an enforced denial owes, exactly once
// per account, scope, and UTC day.
//
// The three cases are deliberately asymmetric. An account-owned scope is the
// customer's own violation, so both the owner and the operator hear about it. A
// global pool is a platform incident that innocent accounts merely happened to
// collide with, so it produces one coalesced operator notice and blames nobody.
// An operational purpose produces nothing at all: a notice that cannot be sent
// because the notice pool is exhausted must not enqueue another notice.
//
// Lock order note: this runs while the caller holds budget-counter locks, which
// is later in the normative order than the notice keys. The inversion is
// deliberate — the plan requires the notice to be inserted ATOMICALLY with the
// denial — and it is currently acyclic, but NOT for the obvious reason.
//
// readAuthState does lock an existing delivery row and then reach for counters,
// so the two directions do exist. What closes the cycle is that the only
// transactions holding a delivery row are notice operations, whose purpose is
// operational, and this function returns nil for operational purposes before
// touching a notice key. That is the load-bearing fact: if an operational
// purpose ever becomes able to enqueue a notice, the cycle closes silently.
func (m *Module) enqueueDenialNotice(ctx context.Context, tx pgx.Tx, policy RuntimePolicy, op operationRow, day time.Time, scope Scope) error {
	if op.Purpose.isOperational() {
		return nil
	}

	retention := time.Duration(policy.SendingControlAuditRetentionDays) * 24 * time.Hour
	expires := time.Now().UTC().Add(retention)

	switch scope {
	case ScopeAccountDaily, ScopeAccountSharedDaily:
		account := op.accountRef()
		if account == "" {
			return nil
		}
		var eventID string
		err := tx.QueryRow(ctx, `
			INSERT INTO sending_protection_notice_events
			    (id, account_ref, kind, reason_code, budget_scope, ledger_day, expires_at)
			VALUES ($1, $2, 'budget_violation', 'budget_limit', $3, $4, $5)
			ON CONFLICT (account_ref, budget_scope, ledger_day)
			    WHERE kind = 'budget_violation'
			    DO NOTHING
			RETURNING id`,
			randomID("spn_"), account, string(scope), day, expires,
		).Scan(&eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Already enqueued for this account, scope, and day. Later holds
			// for the same tuple deliberately enqueue nothing: one violation
			// email per day per failed scope, not one per held message.
			return nil
		}
		if err != nil {
			return fmt.Errorf("sendingpolicy: enqueue violation notice: %w", err)
		}
		return insertNoticeDeliveries(ctx, tx, eventID, AudienceOwner, AudienceOperator)

	case ScopeGlobalAll, ScopeGlobalProbation:
		var eventID string
		err := tx.QueryRow(ctx, `
			INSERT INTO sending_protection_notice_events
			    (id, account_ref, kind, reason_code, budget_scope, ledger_day, expires_at)
			VALUES ($1, NULL, 'global_guardrail', 'global_budget_exhausted', $2, $3, $4)
			ON CONFLICT (budget_scope, ledger_day)
			    WHERE kind = 'global_guardrail'
			    DO NOTHING
			RETURNING id`,
			randomID("spn_"), string(scope), day, expires,
		).Scan(&eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sendingpolicy: enqueue guardrail notice: %w", err)
		}
		return insertNoticeDeliveries(ctx, tx, eventID, AudienceOperator)
	}
	return nil
}

// insertNoticeDeliveries creates the outbox rows for one event.
func insertNoticeDeliveries(ctx context.Context, tx pgx.Tx, eventID string, audiences ...Audience) error {
	for _, audience := range audiences {
		if _, err := tx.Exec(ctx, `
			INSERT INTO sending_protection_notice_deliveries (event_id, audience)
			VALUES ($1, $2)
			ON CONFLICT (event_id, audience) DO NOTHING`,
			eventID, string(audience),
		); err != nil {
			return fmt.Errorf("sendingpolicy: enqueue notice delivery: %w", err)
		}
	}
	return nil
}

// RedeemProviderCall consumes the single-use authorization immediately before
// the socket opens.
//
// It exists because ConsumeAttempt's transaction has committed by the time the
// adapter runs, and everything it checked can have changed in the meantime.
// Re-proving the whole chain here — policy, owner, delivery, operation,
// ordinal, nonce, recipient selector — costs one short transaction and closes
// the window in which an owner edit, a policy rotation, a supersession, or a
// mixed-slot secret rotation could mail a retired address.
func (m *Module) RedeemProviderCall(ctx context.Context, auth ProviderAuthorization) error {
	if auth.IsZero() || auth.nonce == "" {
		return ErrAuthorizationInvalid
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sendingpolicy: begin redeem: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := m.effectivePolicy(ctx, tx); err != nil {
		return err
	}

	var ownerEmail string
	if auth.notice != nil && auth.notice.audience == AudienceOwner {
		if err := tx.QueryRow(ctx, `
			SELECT users.email
			  FROM sending_protection_notice_events AS event
			  JOIN users ON users.id = event.account_ref
			 WHERE event.id = $1
			   FOR SHARE OF users`, auth.notice.eventID,
		).Scan(&ownerEmail); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return m.invalidate(ctx, tx, auth.attempt)
			}
			return fmt.Errorf("sendingpolicy: lock notice owner: %w", err)
		}
	}

	if auth.notice != nil {
		var boundOperation *string
		if err := tx.QueryRow(ctx, `
			SELECT current_operation_id
			  FROM sending_protection_notice_deliveries
			 WHERE event_id = $1 AND audience = $2
			   FOR UPDATE`, auth.notice.eventID, string(auth.notice.audience),
		).Scan(&boundOperation); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return m.invalidate(ctx, tx, auth.attempt)
			}
			return fmt.Errorf("sendingpolicy: lock notice delivery: %w", err)
		}
		if boundOperation == nil || *boundOperation != auth.attempt.operationID {
			return m.invalidate(ctx, tx, auth.attempt)
		}
	}

	op, err := lockOperation(ctx, tx, auth.attempt.operationID)
	if err != nil {
		return err
	}
	if op.CurrentAttempt != auth.attempt.attempt {
		return m.invalidate(ctx, tx, auth.attempt)
	}

	stored, err := lockReservation(ctx, tx, auth.attempt.operationID, auth.attempt.attempt)
	if err != nil {
		return err
	}
	if !stored.Exists || stored.CallState != "authorized" || stored.Nonce == nil || *stored.Nonce != auth.nonce {
		return m.invalidate(ctx, tx, auth.attempt)
	}

	if auth.notice != nil {
		ok, err := m.noticeSelectorStillCurrent(ctx, tx, auth, stored, ownerEmail)
		if err != nil {
			return err
		}
		if !ok {
			return m.invalidate(ctx, tx, auth.attempt)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE sending_budget_reservations
		   SET call_state = 'started', provider_call_started_at = now(), updated_at = now()
		 WHERE operation_id = $1 AND submission_attempt = $2 AND call_state = 'authorized'`,
		auth.attempt.operationID, auth.attempt.attempt,
	); err != nil {
		return fmt.Errorf("sendingpolicy: redeem authorization: %w", err)
	}
	return m.commit(ctx, tx, "redeem")
}

// noticeSelectorStillCurrent re-proves that the recipient this token was built
// for is still the recipient the system would choose right now.
func (m *Module) noticeSelectorStillCurrent(ctx context.Context, tx pgx.Tx, auth ProviderAuthorization, stored reservationRow, ownerEmail string) (bool, error) {
	if stored.NoticeVersion == nil || len(stored.NoticeCommitment) == 0 {
		return false, nil
	}
	if auth.notice.audience == AudienceOperator {
		if m.secrets.Recipients == nil {
			return false, nil
		}
		version, err := m.policyOperatorVersion(ctx, tx)
		if err != nil {
			return false, err
		}
		if version != *stored.NoticeVersion || version != auth.notice.recipientVersion {
			return false, nil
		}
		commitment, ok := m.secrets.Recipients.Commitment(version)
		if !ok || commitment != string(stored.NoticeCommitment) || commitment != string(auth.notice.recipientCommitment) {
			return false, nil
		}
		return true, nil
	}

	if m.secrets.Keyring == nil {
		return false, nil
	}
	normalized, err := normalizeEnvelope([]string{ownerEmail})
	if err != nil {
		return false, nil
	}
	if !m.secrets.Keyring.Verify(*stored.NoticeVersion, []byte(normalized[0]), stored.NoticeCommitment) {
		return false, nil
	}
	return true, nil
}

// invalidate retires an attempt whose final recheck failed, forcing a strictly
// greater ordinal before any provider call.
//
// The confirmed capacity is deliberately not refunded. The attempt was charged
// because a socket might open; discovering at the last moment that it must not
// is a reason to stop, not a reason to hand back reputation exposure that a
// retry will immediately re-consume.
func (m *Module) invalidate(ctx context.Context, tx pgx.Tx, ref AttemptRef) error {
	// Guarded by the ordinal, not GREATEST(). An unguarded bump lets a worker
	// arriving late with a SUPERSEDED token retire the live attempt another
	// worker is holding — that worker's confirmed capacity is spent and never
	// refunded, so a supply of stale tokens becomes a livelock that burns the
	// account's budget without a single SES call. A stale token must retire
	// only itself, and it is already stale, so the correct write is none.
	if _, err := tx.Exec(ctx, `
		UPDATE sending_provider_operations
		   SET current_attempt = current_attempt + 1, updated_at = now()
		 WHERE operation_id = $1 AND current_attempt = $2`, ref.operationID, ref.attempt,
	); err != nil {
		return fmt.Errorf("sendingpolicy: invalidate attempt: %w", err)
	}
	if err := m.commit(ctx, tx, "invalidate"); err != nil {
		return err
	}
	return ErrAuthorizationInvalid
}

// DeferAttempt gives back only the sending budget for a rate deferral.
//
// The ramp reservation is deliberately retained: a message deferred by the
// per-agent rate limiter has not been rejected by the provider and has not
// used a ramp day, so releasing its ramp claim would let the same message
// re-qualify a stage it already qualified.
func (m *Module) DeferAttempt(ctx context.Context, ref AttemptRef) error {
	return m.releaseAttempt(ctx, ref, "defer")
}

// CancelAttempt gives back both ledgers for a terminal local cancellation such
// as a suppression match.
func (m *Module) CancelAttempt(ctx context.Context, ref AttemptRef) error {
	// The ramp half arrives with Task 4's adapter; until the ramp is composed
	// into final authorization there is no ramp reservation for this module
	// to release, and the existing message-keyed ledger stays with its
	// current owner.
	return m.releaseAttempt(ctx, ref, "cancel")
}

// releaseAttempt is the shared release path. Both callers are valid only while
// provider I/O is provably not begun; once a socket has opened, the capacity is
// spent whatever the outcome.
func (m *Module) releaseAttempt(ctx context.Context, ref AttemptRef, what string) error {
	if ref.IsZero() {
		return ErrSourceUnavailable
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sendingpolicy: begin %s: %w", what, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := m.effectivePolicy(ctx, tx); err != nil {
		return err
	}
	op, err := lockOperation(ctx, tx, ref.operationID)
	if err != nil {
		return err
	}
	stored, err := lockReservation(ctx, tx, ref.operationID, ref.attempt)
	if err != nil {
		return err
	}
	if !stored.Exists {
		return ErrAttemptStale
	}
	if stored.CallState == "started" {
		return ErrProviderCallStarted
	}
	if stored.State == "confirmed" {
		// The attempt was already authorized, so its capacity is irrevocably
		// spent whether or not a socket followed. Marking it released here
		// would leave the row disagreeing with the counter — the counter's
		// confirmed floor correctly refuses the refund, so the row would claim
		// a give-back that never happened. The worker order puts both callers
		// BEFORE final authorization, so reaching this is a caller bug; the
		// honest answer is that this ordinal is finished and the next provider
		// opportunity needs a new one.
		return ErrAttemptStale
	}
	if stored.State == "released" {
		// Already given back. Releases run on retry-prone paths, so repeating
		// one must be a no-op rather than an error or a double refund.
		return m.commit(ctx, tx, what)
	}

	storedKeys, err := stored.scopeKeys(op.Shared)
	if err != nil {
		return err
	}
	refs := refsFor(storedKeys, stored.Day)
	plan, err := lockLedger(ctx, tx, nil, refs)
	if err != nil {
		return err
	}
	for _, r := range refs {
		plan.release(r, stored.Units)
	}
	if err := plan.flush(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sending_budget_reservations
		   SET state = 'released', call_state = 'none', authorization_nonce = NULL,
		       provider_call_started_at = NULL, updated_at = now()
		 WHERE operation_id = $1 AND submission_attempt = $2`,
		ref.operationID, ref.attempt,
	); err != nil {
		return fmt.Errorf("sendingpolicy: %s reservation: %w", what, err)
	}
	return m.commit(ctx, tx, what)
}

// SettleProvider records an authoritative provider outcome.
//
// It changes only the custom-domain ramp ledger, never the sending budget: the
// budget was spent when the attempt was authorized, and SES rejecting a message
// does not give back the reputation exposure of having asked. Idempotent by
// construction, because both the synchronous success branch and the delayed
// delivery-feedback finalizer call it for the same attempt.
func (m *Module) SettleProvider(ctx context.Context, settlement ProviderSettlement) error {
	if settlement.Attempt.IsZero() {
		return ErrSourceUnavailable
	}
	if !settlement.Outcome.valid() {
		return fmt.Errorf("sendingpolicy: unsupported settlement outcome %q", settlement.Outcome)
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sendingpolicy: begin settle: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockOperation(ctx, tx, settlement.Attempt.operationID); err != nil {
		return err
	}
	stored, err := lockReservation(ctx, tx, settlement.Attempt.operationID, settlement.Attempt.attempt)
	if err != nil {
		return err
	}
	if !stored.Exists {
		return ErrAttemptStale
	}
	// Settlement reports what the PROVIDER did, so it is only meaningful for an
	// attempt that was authorized to reach the provider. Accepting a reserved
	// or released attempt would let a caller advance ramp progress for a send
	// that never happened once Task 4 hangs the ramp mutation off this call.
	if stored.State != "confirmed" {
		return ErrAttemptStale
	}

	if err := bindProviderMessageID(ctx, tx, settlement); err != nil {
		return err
	}
	// The ramp ledger is Task 4's to move. Validating the attempt here — and
	// taking its locks in the normative order — is what makes that composition
	// a body change rather than a reshaping of the interface every caller
	// already uses.
	return m.commit(ctx, tx, "settle")
}

// bindProviderMessageID records the provider's id on the attempt's feedback
// correlation, exactly once.
//
// It runs after the operation and reservation locks, which is the only place
// the correlation row is ever written after its insert, so no additional key
// joins the normative order. Replaying the same id is a no-op — the
// synchronous success branch and the delayed feedback finalizer both settle
// the same attempt — while a different id is refused outright. A correlation
// that has already aged out of retention is left alone: there is nothing to
// attribute feedback to any more, and failing a late settlement over it would
// only make the caller retry forever.
func bindProviderMessageID(ctx context.Context, tx pgx.Tx, settlement ProviderSettlement) error {
	id := strings.TrimSpace(settlement.ProviderMessageID)
	if id == "" {
		return nil
	}
	if settlement.Outcome != SettlementProviderAccepted {
		return fmt.Errorf("sendingpolicy: a %q settlement cannot carry a provider message id", settlement.Outcome)
	}
	var bound *string
	err := tx.QueryRow(ctx, `
		SELECT provider_message_id
		  FROM sending_feedback_correlations
		 WHERE operation_id = $1 AND submission_attempt = $2
		   FOR UPDATE`,
		settlement.Attempt.operationID, settlement.Attempt.attempt,
	).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sendingpolicy: lock correlation: %w", err)
	}
	if bound != nil {
		if *bound == id {
			return nil
		}
		return ErrProviderMessageIDConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sending_feedback_correlations
		   SET provider_message_id = $3
		 WHERE operation_id = $1 AND submission_attempt = $2`,
		settlement.Attempt.operationID, settlement.Attempt.attempt, id,
	); err != nil {
		return fmt.Errorf("sendingpolicy: bind provider message id: %w", err)
	}
	return nil
}
