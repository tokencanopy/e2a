package sendingpolicy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the external-sending-access decision: which recipients an
// account inside the rollout cohort may reach through a shared sending
// identity. It is ONE implementation, called from every stage that can let a
// customer message reach the provider — API preflight, acceptance
// (PrepareExternalTx), final authorization (ConsumeAttempt) and redemption
// (RedeemProviderCall) — so the stages can never disagree about the rule.
//
// The ordered rule (design: external-sending-access):
//
//  1. Pause and source deletion deny first (the callers already do this).
//  2. Feature disabled, account outside the cohort, or a system/internal
//     account class: existing behavior.
//  3. The message's ACTUAL outbound identity is the account's own currently
//     verified custom domain: external recipients allowed.
//  4. A current operator grant, or the billing-issued paid-base entitlement
//     (account_limits.external_sending_entitled — never plan_code):
//     external shared-identity sending allowed.
//  5. Otherwise every envelope recipient (To, Cc AND Bcc) must be the
//     account's currently verified owner mailbox or a live agent of the same
//     account. One disallowed recipient refuses the whole operation.
//
// Every fact is read from durable state at decision time; nothing is cached.
// A read error is returned, never translated into "not approved" or "allowed".

// ReasonExternalSendingNotEnabled is the gate hold reason for a message the
// account may not send to its recipients. It is terminal: nothing a retry can
// do changes it, and mail must never sit queued until an approval arrives.
const ReasonExternalSendingNotEnabled = "external_sending_not_enabled"

// AcceptanceExternalSendingNotEnabled refuses acceptance of a message whose
// envelope the account may not reach.
const AcceptanceExternalSendingNotEnabled AcceptanceDecision = "external_sending_not_enabled"

// ExternalAccessRoute names which step of the rule decided a verdict. It is a
// closed, bounded vocabulary suitable for metrics and logs.
type ExternalAccessRoute string

const (
	// RouteNotApplicable: feature off, outside the cohort, or exempt class.
	RouteNotApplicable ExternalAccessRoute = "not_applicable"
	// RouteCustomIdentity: sent as the account's own verified domain.
	RouteCustomIdentity ExternalAccessRoute = "custom_identity"
	// RouteOperatorApproval: the operator grant.
	RouteOperatorApproval ExternalAccessRoute = "operator_approval"
	// RoutePaidEntitlement: the billing-issued paid-base entitlement.
	RoutePaidEntitlement ExternalAccessRoute = "paid_entitlement"
	// RouteRestrictedRecipients: every recipient is the verified owner
	// mailbox or a live agent of the same account.
	RouteRestrictedRecipients ExternalAccessRoute = "restricted_recipients"
	// RouteDenied: at least one recipient is outside the allowed set.
	RouteDenied ExternalAccessRoute = "denied"
)

// ExternalAccessVerdict is the outcome of one evaluation.
type ExternalAccessVerdict struct {
	// Mode is the policy mode the verdict was computed under.
	Mode Mode
	// Route is the rule step that decided.
	Route ExternalAccessRoute
	// Allowed is the effective answer. In shadow mode a denial is computed
	// (Route == RouteDenied) but Allowed stays true.
	Allowed bool
	// Paused is set by the preflight when the account's sending is paused.
	// Pause wins over every access answer: the caller must report the
	// pause, never a restriction a payment or approval could appear to fix.
	Paused bool
}

// ErrExternalAccessDisabled means the deployment's policy leaves external
// sending access disabled (absent or mode disabled). The status and request
// surfaces answer "not available" rather than inventing a state.
var ErrExternalAccessDisabled = errors.New("sendingpolicy: external sending access is disabled on this deployment")

// Denied reports an enforced refusal.
func (v ExternalAccessVerdict) Denied() bool { return !v.Allowed }

// dbQuerier is the read surface shared by a transaction and the pool.
type dbQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// accountAccessFacts are the account-level inputs to the decision.
type accountAccessFacts struct {
	createdAt  time.Time
	class      string
	ownerEmail string
	// proofAddress is the exact mailbox a trusted login verified, "" if the
	// account has no proof.
	proofAddress string
	approved     bool
	// paused is account_sending_controls.state = 'paused'.
	paused bool
	// entitled is account_limits.external_sending_entitled — the
	// billing-issued paid-base entitlement. False when the row is missing.
	entitled bool
}

// errAccountMissing means the account row is gone. Callers already handle a
// deleted owner before reaching this check; it is surfaced as a source loss.
var errAccountMissing = fmt.Errorf("%w: account is gone", ErrSourceUnavailable)

// loadAccountAccessFacts reads the account, its control row and its limits in
// one statement. It takes no locks of its own: inside ConsumeAttempt the rows
// are already locked by that transaction in the normative order, and every
// other caller is either a pre-acceptance preflight or a refuse-only recheck.
func loadAccountAccessFacts(ctx context.Context, q dbQuerier, userID string) (accountAccessFacts, error) {
	var f accountAccessFacts
	var proofAddress *string
	var proofAt *time.Time
	err := q.QueryRow(ctx, `
		SELECT u.created_at, u.account_class, u.email,
		       u.owner_email_verified_address, u.owner_email_verified_at,
		       COALESCE(c.external_sending_approved, false),
		       COALESCE(c.state, 'active') = 'paused',
		       COALESCE(l.external_sending_entitled, false)
		  FROM users AS u
		  LEFT JOIN account_sending_controls AS c ON c.user_id = u.id
		  LEFT JOIN account_limits AS l ON l.user_id = u.id
		 WHERE u.id = $1`, userID,
	).Scan(&f.createdAt, &f.class, &f.ownerEmail, &proofAddress, &proofAt, &f.approved, &f.paused, &f.entitled)
	if errors.Is(err, pgx.ErrNoRows) {
		return accountAccessFacts{}, errAccountMissing
	}
	if err != nil {
		return accountAccessFacts{}, fmt.Errorf("sendingpolicy: read external access facts: %w", err)
	}
	if proofAddress != nil && proofAt != nil {
		f.proofAddress = *proofAddress
	}
	return f, nil
}

// NormalizeOwnerMailbox is the one normalization applied to both the stored
// proof and the account's current email: trimmed and lower-cased, matching the
// envelope normalization. No provider-specific alias folding (dots, plus
// tags) — that would widen the exception beyond the mailbox actually proven.
func NormalizeOwnerMailbox(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ownerRecipientVerified reports whether the account holds valid proof for
// its CURRENT owner mailbox. An email change invalidates older proof.
func (f accountAccessFacts) ownerRecipientVerified() bool {
	if f.proofAddress == "" {
		return false
	}
	current := NormalizeOwnerMailbox(f.ownerEmail)
	return current != "" && current == f.proofAddress
}

// applies reports whether the rule binds this account under the policy: the
// mode is armed, the account is in the cohort, and it is not a trusted
// first-party class. The class exemption uses the server-owned
// account_class, never names or email suffixes.
func externalAccessApplies(policy RuntimePolicy, f accountAccessFacts) (bool, error) {
	esa := policy.ExternalSendingAccess
	if esa == nil || esa.Mode == ModeDisabled {
		return false, nil
	}
	cutoff, err := esa.Cutoff()
	if err != nil {
		return false, err
	}
	if f.createdAt.Before(cutoff) {
		return false, nil
	}
	return !accountClassExempt(f.class), nil
}

// ownAgentRecipients returns which of the (normalized) recipients are live,
// non-trashed agents of the account, in one bounded query. The lookup is by
// current identity rows, never by a shared-domain suffix: an agent that was
// trashed, purged or moved to another account no longer qualifies.
func ownAgentRecipients(ctx context.Context, q dbQuerier, userID string, recipients []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(recipients))
	if len(recipients) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT lower(id) FROM agent_identities
		 WHERE user_id = $1 AND deleted_at IS NULL AND lower(id) = ANY($2::text[])`,
		userID, recipients)
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: read same-account agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, fmt.Errorf("sendingpolicy: scan same-account agent: %w", err)
		}
		out[addr] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sendingpolicy: read same-account agents: %w", err)
	}
	return out, nil
}

// customIdentityVerified reports whether the sending agent is a live agent of
// the account whose registered domain is owned by that same account, ownership
// verified, and provider sending-verified RIGHT NOW. The shared domain has no
// owning account, so it never qualifies; a domain verified elsewhere on the
// account does not qualify a different (shared) agent.
func customIdentityVerified(ctx context.Context, q dbQuerier, userID, agentID string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		      FROM agent_identities AS a
		      JOIN domains AS d ON d.domain = a.registered_domain
		     WHERE a.id = $1 AND a.user_id = $2 AND a.deleted_at IS NULL
		       AND d.user_id = $2 AND d.verified AND d.sending_status = 'verified')`,
		agentID, userID,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("sendingpolicy: read sending identity: %w", err)
	}
	return ok, nil
}

// externalAccessInput is one evaluation request.
type externalAccessInput struct {
	userID  string
	agentID string
	// claimsOwnIdentity is true when the message will leave (or left
	// composition) as the agent's own address. For a stored message it is
	// sent_as = 'own_address'; the preflight derives it from the same
	// domain-verification facts the composer uses. Either way it is only a
	// claim — step 3 additionally requires customIdentityVerified now.
	claimsOwnIdentity bool
	// envelope is the normalized, deduplicated To+Cc+Bcc set.
	envelope []string
	// stage labels logs; bounded vocabulary.
	stage string
}

// evaluateExternalAccess applies the ordered rule. It performs no query at all
// when the policy leaves the feature disabled, so a self-host pays nothing.
func evaluateExternalAccess(ctx context.Context, q dbQuerier, policy RuntimePolicy, in externalAccessInput) (ExternalAccessVerdict, error) {
	mode := policy.ExternalSendingMode()
	if mode == ModeDisabled {
		return ExternalAccessVerdict{Mode: mode, Route: RouteNotApplicable, Allowed: true}, nil
	}
	facts, err := loadAccountAccessFacts(ctx, q, in.userID)
	if err != nil {
		return ExternalAccessVerdict{}, err
	}
	return evaluateWithFacts(ctx, q, policy, facts, in)
}

// evaluateWithFacts is evaluateExternalAccess over already-loaded facts.
func evaluateWithFacts(ctx context.Context, q dbQuerier, policy RuntimePolicy, facts accountAccessFacts, in externalAccessInput) (ExternalAccessVerdict, error) {
	mode := policy.ExternalSendingMode()
	route, err := decideExternalRoute(ctx, q, policy, facts, in)
	if err != nil {
		return ExternalAccessVerdict{}, err
	}
	v := ExternalAccessVerdict{Mode: mode, Route: route, Allowed: route != RouteDenied}
	observeExternalAccess(in.stage, route, mode)
	if route == RouteDenied && mode == ModeShadow {
		// Shadow computes the same decision and never blocks. Bounded fields
		// only: no account id, address, domain or content.
		log.Printf("[sending-protection] external access shadow denial: stage=%s recipients=%d", in.stage, len(in.envelope))
		v.Allowed = true
	}
	if v.Denied() {
		log.Printf("[sending-protection] external access denied: stage=%s recipients=%d", in.stage, len(in.envelope))
	}
	return v, nil
}

// decideExternalRoute is steps 2–5 over already-loaded account facts.
func decideExternalRoute(ctx context.Context, q dbQuerier, policy RuntimePolicy, facts accountAccessFacts, in externalAccessInput) (ExternalAccessRoute, error) {
	applies, err := externalAccessApplies(policy, facts)
	if err != nil {
		return "", err
	}
	if !applies {
		return RouteNotApplicable, nil
	}
	if in.claimsOwnIdentity && in.agentID != "" {
		ok, err := customIdentityVerified(ctx, q, in.userID, in.agentID)
		if err != nil {
			return "", err
		}
		if ok {
			return RouteCustomIdentity, nil
		}
	}
	if facts.approved {
		return RouteOperatorApproval, nil
	}
	if facts.entitled {
		return RoutePaidEntitlement, nil
	}
	if len(in.envelope) == 0 {
		// Nothing to authorize is not an authorization.
		return RouteDenied, nil
	}
	agents, err := ownAgentRecipients(ctx, q, in.userID, in.envelope)
	if err != nil {
		return "", err
	}
	owner := ""
	if facts.ownerRecipientVerified() {
		owner = facts.proofAddress
	}
	for _, addr := range in.envelope {
		if owner != "" && addr == owner {
			continue
		}
		if _, ok := agents[addr]; ok {
			continue
		}
		return RouteDenied, nil
	}
	return RouteRestrictedRecipients, nil
}

// messageSender reads the stored sender identity of an outbound message.
func messageSender(ctx context.Context, q dbQuerier, messageID string) (agentID string, ownAddress bool, err error) {
	var sentAs *string
	err = q.QueryRow(ctx,
		`SELECT agent_id, sent_as FROM messages WHERE id = $1 AND direction = 'outbound'`, messageID,
	).Scan(&agentID, &sentAs)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrSourceUnavailable
	}
	if err != nil {
		return "", false, fmt.Errorf("sendingpolicy: read message sender: %w", err)
	}
	return agentID, sentAs != nil && *sentAs == "own_address", nil
}

// evaluateMessageAccess evaluates a stored customer message: its durable
// sender, its durable To/Cc/Bcc envelope, and the account's current state.
func evaluateMessageAccess(ctx context.Context, q dbQuerier, policy RuntimePolicy, userID, messageID, stage string) (ExternalAccessVerdict, error) {
	if policy.ExternalSendingMode() == ModeDisabled {
		return ExternalAccessVerdict{Mode: ModeDisabled, Route: RouteNotApplicable, Allowed: true}, nil
	}
	agentID, own, err := messageSender(ctx, q, messageID)
	if err != nil {
		return ExternalAccessVerdict{}, err
	}
	envelope, err := messageEnvelopeQ(ctx, q, messageID)
	if err != nil {
		return ExternalAccessVerdict{}, err
	}
	return evaluateExternalAccess(ctx, q, policy, externalAccessInput{
		userID: userID, agentID: agentID, claimsOwnIdentity: own, envelope: envelope, stage: stage,
	})
}

// messageEnvelopeQ is messageEnvelope over any querier.
func messageEnvelopeQ(ctx context.Context, q dbQuerier, messageID string) ([]string, error) {
	var to, cc, bcc []string
	err := q.QueryRow(ctx, `
		SELECT COALESCE(to_recipients, '{}'), COALESCE(cc, '{}'), COALESCE(bcc, '{}')
		  FROM messages
		 WHERE id = $1 AND direction = 'outbound'`, messageID,
	).Scan(&to, &cc, &bcc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSourceUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: read message envelope: %w", err)
	}
	all := make([]string, 0, len(to)+len(cc)+len(bcc))
	all = append(all, to...)
	all = append(all, cc...)
	all = append(all, bcc...)
	envelope, err := normalizeEnvelope(all)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEnvelopeUnavailable, err)
	}
	return envelope, nil
}

// policyForRead reads the governing policy WITHOUT the singleton share lock.
//
// Acceptance and redemption use it. Acceptance runs inside a transaction that
// already holds message, agent and account-control locks, so requesting the
// singleton share lock there could queue behind a pending activation that is
// itself waiting on a ConsumeAttempt blocked on those same rows. A plain read
// is sound: an activation that commits later is still observed by the locked
// read in ConsumeAttempt and by redemption, and a stale read can only be the
// previous reviewed generation.
func (m *Module) policyForRead(ctx context.Context, q dbQuerier) (RuntimePolicy, error) {
	if m.source == PolicySourceDatabase {
		snapshot, err := scanPolicy(q.QueryRow(ctx, policySelect))
		if err != nil {
			return RuntimePolicy{}, err
		}
		return snapshot.Policy, nil
	}
	return m.configPolicy, nil
}

// ExternalAccessPreflight evaluates a send before anything is persisted: the
// composed sender (agent) and the full To/Cc/Bcc envelope. It is guidance for
// a fast, clean 403 — acceptance and final authorization repeat the check
// against durable state, so a race after this call cannot bypass it.
func (m *Module) ExternalAccessPreflight(ctx context.Context, userID, agentID string, recipients []string) (ExternalAccessVerdict, error) {
	policy, err := m.policyForRead(ctx, m.pool)
	if err != nil {
		return ExternalAccessVerdict{}, err
	}
	if policy.ExternalSendingMode() == ModeDisabled {
		return ExternalAccessVerdict{Mode: ModeDisabled, Route: RouteNotApplicable, Allowed: true}, nil
	}
	facts, err := loadAccountAccessFacts(ctx, m.pool, userID)
	if err != nil {
		return ExternalAccessVerdict{}, err
	}
	envelope, err := normalizeEnvelope(recipients)
	if err != nil {
		// Malformed or empty recipients are a validation problem that the
		// composer answers with 400, not a permission answer. Nothing can be
		// sent from here: acceptance re-judges the persisted, normalized
		// envelope and fails closed on anything it cannot resolve.
		return ExternalAccessVerdict{Mode: policy.ExternalSendingMode(), Route: RouteNotApplicable, Allowed: true}, nil
	}
	// The composer sends as the agent's own address exactly when its domain
	// is ownership- and sending-verified, which is the step-3 predicate
	// itself; the stored sent_as is re-checked at every later stage.
	v, err := evaluateWithFacts(ctx, m.pool, policy, facts, externalAccessInput{
		userID: userID, agentID: agentID, claimsOwnIdentity: true, envelope: envelope, stage: "preflight",
	})
	if err != nil {
		return ExternalAccessVerdict{}, err
	}
	// Pause wins only where this control would itself refuse: it replaces an
	// ENFORCED denial (so a paused, restricted account is told it is paused,
	// never pointed at approval or payment), but never an allow, a
	// not-applicable, or a shadow answer — those sends (a self-send
	// loopback, a review hold) behave exactly as with the control disabled,
	// and the existing pause checks further down decide them.
	if v.Denied() && facts.paused {
		v.Paused = true
	}
	return v, nil
}

// ExternalAccessStatus is the account-eligibility readback behind the
// additive `sending_access` object on GET /v1/account. Booleans only; it
// describes eligibility, not a promise that a given send will pass pause,
// quota, content or domain checks.
type ExternalAccessStatus struct {
	// EnforcementApplies is true exactly when the mode is enforce, the
	// account is in the cutoff cohort, and it is not a system/internal
	// class. It stays true for approved accounts.
	EnforcementApplies bool
	// SharedExternalApproved reports the operator grant.
	SharedExternalApproved bool
	// PaidExternalSendingEntitled reports the billing-issued entitlement.
	PaidExternalSendingEntitled bool
	// OwnerRecipientVerified reports valid proof for the current mailbox.
	OwnerRecipientVerified bool
}

// ExternalAccessStatus reads the account's eligibility.
func (m *Module) ExternalAccessStatus(ctx context.Context, userID string) (ExternalAccessStatus, error) {
	policy, err := m.policyForRead(ctx, m.pool)
	if err != nil {
		return ExternalAccessStatus{}, err
	}
	if policy.ExternalSendingMode() == ModeDisabled {
		// Feature off: no account read at all, and no object on the wire.
		return ExternalAccessStatus{}, ErrExternalAccessDisabled
	}
	facts, err := loadAccountAccessFacts(ctx, m.pool, userID)
	if err != nil {
		return ExternalAccessStatus{}, err
	}
	applies, err := externalAccessApplies(policy, facts)
	if err != nil {
		return ExternalAccessStatus{}, err
	}
	return ExternalAccessStatus{
		EnforcementApplies:          applies && policy.ExternalSendingMode() == ModeEnforce,
		SharedExternalApproved:      facts.approved,
		PaidExternalSendingEntitled: facts.entitled,
		OwnerRecipientVerified:      facts.ownerRecipientVerified(),
	}, nil
}

// ExternalAccess is the narrow role the API surface uses: preflight and the
// status readback. It carries no mutation.
type ExternalAccess interface {
	ExternalAccessPreflight(ctx context.Context, userID, agentID string, recipients []string) (ExternalAccessVerdict, error)
	ExternalAccessStatus(ctx context.Context, userID string) (ExternalAccessStatus, error)
}

var _ ExternalAccess = (*Module)(nil)

// ExternalAccessObserver receives one bounded (stage, route, mode) sample per
// evaluation in shadow or enforce mode. Set once at startup by the
// composition root (telemetry.Metrics.ExternalAccessDecision); nil = no-op.
type ExternalAccessObserver func(stage, route, mode string)

var externalAccessObserver atomic.Value // ExternalAccessObserver

// SetExternalAccessObserver installs the process-wide decision observer.
func SetExternalAccessObserver(o ExternalAccessObserver) {
	externalAccessObserver.Store(o)
}

func observeExternalAccess(stage string, route ExternalAccessRoute, mode Mode) {
	if o, ok := externalAccessObserver.Load().(ExternalAccessObserver); ok && o != nil {
		o(stage, string(route), string(mode))
	}
}

// requireExternalAccessEnabled refuses the customer request surfaces when the
// control is disabled, so a deployment that never turns it on files no rows
// and sends no operator mail.
func (m *Module) requireExternalAccessEnabled(ctx context.Context) error {
	policy, err := m.policyForRead(ctx, m.pool)
	if err != nil {
		return err
	}
	if policy.ExternalSendingMode() == ModeDisabled {
		return ErrExternalAccessDisabled
	}
	return nil
}
