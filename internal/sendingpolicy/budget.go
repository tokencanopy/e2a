package sendingpolicy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file owns the budget ledger: which pools an operation draws on, what
// today's limit for each pool is, and how capacity is taken, confirmed, and
// given back. Nothing here decides whether to enforce — that is the caller's
// reading of the runtime policy's budget mode. This layer answers only "is
// there room", so shadow mode computes the exact same arithmetic without
// blocking anything.

// randomID mints an opaque identifier. crypto/rand failure means the OS RNG is
// broken; panicking surfaces that as a 500 rather than writing a predictable
// operation ID that an attacker could then reference.
func randomID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("sendingpolicy: crypto/rand failed: %v", err))
	}
	return prefix + hex.EncodeToString(b)
}

// randomNonce mints the single-use redemption secret stored on a confirmed
// reservation. It is longer than an ID because guessing it would let a caller
// redeem an authorization it was never handed.
func randomNonce() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("sendingpolicy: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// accountClassExempt reports whether an account's class removes it from every
// customer budget.
//
// The list is closed and positive on purpose. `demo`, an unknown class written
// by a future binary, and the empty string all fall through to "budgeted",
// because the failure mode of budgeting a trusted account is a held prober
// message, while the failure mode of exempting an untrusted one is exactly the
// abuse this system exists to prevent.
func accountClassExempt(class string) bool {
	return class == "system" || class == "internal"
}

// accountDailyLimit resolves the account-daily cap from the current policy and
// the account's authoritative plan code.
//
// A paid plan is not "unlimited": it is limited at the all-customer ceiling, so
// its usage is still recorded on a per-account counter and a single paid
// account still cannot quietly consume the whole platform's day without that
// being visible per account. An unknown or missing plan code deliberately falls
// to the Free default — a plan the catalog does not name is not evidence of
// payment.
func accountDailyLimit(policy RuntimePolicy, planCode string) int {
	for _, code := range policy.DailyUnlimitedPlanCodes {
		if code == planCode {
			return policy.AllCustomerGlobalDailyRecipients
		}
	}
	return policy.DefaultAccountDailyRecipients
}

// scopeKeys returns which counters an operation of this shape charges, with no
// reference to limits or the calendar.
//
// Keeping structure separate from magnitude is what lets the release path work:
// releasing an old attempt's units needs to know exactly which rows it touched,
// and it must be able to answer that without re-deriving the plan code and
// policy generation that were in force when the units were taken.
//
// The mapping is the whole containment argument, so it is one switch rather
// than logic spread across call sites:
//
//   - customer traffic charges the platform ceiling and its own account, plus
//     the probation pool while it is unproven and the shared-domain pool while
//     it borrows platform reputation;
//   - public feedback has no account to charge but still burns platform and
//     probation capacity, because it shares the same provider surface;
//   - the two operational purposes draw only on their own pools, which is what
//     lets a pause notice go out during the abuse wave that exhausted
//     everything else;
//   - trusted first-party traffic charges nothing.
func scopeKeys(purpose Purpose, accountID string, shared, probation bool) []counterKey {
	var keys []counterKey
	switch purpose {
	case PurposeCustomerMessage, PurposeCustomerNotification:
		keys = append(keys,
			counterKey{ScopeGlobalAll, scopeIDAllCustomers},
			counterKey{ScopeAccountDaily, accountID},
		)
		if probation {
			keys = append(keys, counterKey{ScopeGlobalProbation, scopeIDProbation})
		}
		if shared {
			keys = append(keys, counterKey{ScopeAccountSharedDaily, accountID})
		}
	case PurposePublicFeedback:
		keys = append(keys,
			counterKey{ScopeGlobalAll, scopeIDAllCustomers},
			counterKey{ScopeGlobalProbation, scopeIDProbation},
		)
	case PurposeCriticalOperational:
		keys = append(keys, counterKey{ScopeGlobalCritical, scopeIDCritical})
	case PurposeViolationOperational:
		keys = append(keys, counterKey{ScopeGlobalViolation, scopeIDViolation})
	case PurposeTrustedSystem:
		return nil
	}
	sortCounterKeys(keys)
	return keys
}

// limitFor resolves today's cap for one counter under the current policy and
// the account's authoritative plan.
func limitFor(key counterKey, policy RuntimePolicy, planCode string) int {
	switch key.Scope {
	case ScopeGlobalAll:
		return policy.AllCustomerGlobalDailyRecipients
	case ScopeGlobalProbation:
		return policy.ProbationGlobalDailyRecipients
	case ScopeAccountDaily:
		return accountDailyLimit(policy, planCode)
	case ScopeAccountSharedDaily:
		return policy.SharedDomainAccountDailyRecip
	case ScopeGlobalCritical:
		return policy.CriticalOperationalDailyRecip
	case ScopeGlobalViolation:
		return policy.ViolationOperationalDailyRecip
	}
	return 0
}

// optimisticLimitFor is the cap Reserve checks against: the most permissive
// value any plan could produce for this scope.
//
// Reserve does not take the account-control and plan locks that make a plan
// read authoritative, so it cannot know whether this account is paid. That
// leaves two ways to be wrong, and they are not symmetric. A false ALLOW here
// costs nothing — ConsumeAttempt re-evaluates every scope under the real locks
// and holds the message if it must. A false DENY is a customer-visible outage:
// the worker treats an early hold as "snooze until midnight", so guessing the
// Free cap for a paying customer would defer their mail for a day that
// ConsumeAttempt would have allowed. So Reserve deliberately denies only when
// no plan could have fit.
func optimisticLimitFor(key counterKey, policy RuntimePolicy) int {
	if key.Scope != ScopeAccountDaily {
		return limitFor(key, policy, "")
	}
	if policy.AllCustomerGlobalDailyRecipients > policy.DefaultAccountDailyRecipients {
		return policy.AllCustomerGlobalDailyRecipients
	}
	return policy.DefaultAccountDailyRecipients
}

// holdReasonForScope maps a denied counter to its machine-readable reason.
func holdReasonForScope(scope Scope) string {
	switch scope {
	case ScopeAccountDaily:
		return ReasonAccountDailyBudget
	case ScopeAccountSharedDaily:
		return ReasonAccountSharedBudget
	case ScopeGlobalAll:
		return ReasonGlobalAllBudget
	case ScopeGlobalProbation:
		return ReasonGlobalProbation
	case ScopeGlobalCritical:
		return ReasonGlobalCritical
	case ScopeGlobalViolation:
		return ReasonGlobalViolation
	}
	return "budget_exhausted"
}

// ledgerDay reads the UTC date this transaction's writes belong to.
//
// clock_timestamp(), not now(): now() is frozen at transaction start, so a
// transaction that began at 23:59:59 and then waited two seconds on a row lock
// would charge yesterday's counter while the rest of the fleet had already
// rolled over. Reading the wall clock AFTER the serializing locks are held is
// what makes the day boundary consistent with lock acquisition, and it is why
// the adjacent-day physical exposure bound is 2*cap rather than unbounded.
func ledgerDay(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var day time.Time
	if err := tx.QueryRow(ctx, `SELECT (clock_timestamp() AT TIME ZONE 'UTC')::date`).Scan(&day); err != nil {
		return time.Time{}, fmt.Errorf("sendingpolicy: read ledger day: %w", err)
	}
	return day.UTC(), nil
}

// nextUTCMidnight is the retry time for a day-bounded hold: the moment the
// denied counter resets. Derived from the ledger day the transaction actually
// used, so a hold decided just after rollover does not advise a 24-hour wait.
func nextUTCMidnight(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

// ledgerRef names one physical counter row: a scope key on a specific day.
type ledgerRef struct {
	counterKey
	Day time.Time
}

// ledgerRow is one locked counter row plus the net change this transaction
// intends to apply to it.
type ledgerRow struct {
	ref       ledgerRef
	reserved  int
	confirmed int
	limit     int

	// delta is the pending change to reserved_count. Accumulated in memory so
	// a row that is both released and re-acquired in the same transaction is
	// written once, and so an all-or-nothing decision can be made before any
	// row is modified.
	delta int
	// acquired is the part of delta that came from taking capacity, tracked
	// separately so a denial can drop every acquisition while keeping the
	// releases that must survive it.
	acquired int
	// exists is false for a row that was absent and is not being created.
	exists bool
}

// ledgerPlan is the locked working set for one budget transaction.
type ledgerPlan struct {
	rows  map[ledgerRef]*ledgerRow
	order []ledgerRef
}

// lockLedger takes every counter row this transaction may touch, once, in the
// normative order, before any of them is read for a decision.
//
// A single ordered locking pass is the deadlock argument. Two workers charging
// the same account will contend on global_all and account_daily; if one took
// them in the opposite order — or took a release row after an acquire row —
// Postgres would resolve the cycle by killing a transaction, and on the final
// authorization path a killed transaction is a message that errors instead of
// being held. Every caller therefore hands the complete set here first and does
// its arithmetic afterwards against rows it already holds.
//
// `create` names the rows that must exist because this transaction intends to
// take capacity on them; they are upserted, which both creates and locks in one
// statement so two concurrent creators cannot both believe the row was absent.
// Rows outside `create` are release-only: if they do not exist there is nothing
// to give back, so a plain locking read is correct and avoids resurrecting a
// row a janitor legitimately removed.
func lockLedger(ctx context.Context, tx pgx.Tx, create map[ledgerRef]int, releaseOnly []ledgerRef) (*ledgerPlan, error) {
	plan := &ledgerPlan{rows: make(map[ledgerRef]*ledgerRow)}

	refs := make([]ledgerRef, 0, len(create)+len(releaseOnly))
	for ref := range create {
		refs = append(refs, ref)
	}
	for _, ref := range releaseOnly {
		if _, dup := create[ref]; dup {
			continue
		}
		refs = append(refs, ref)
	}
	sortLedgerRefs(refs)

	for _, ref := range refs {
		limit, creating := create[ref]
		row := &ledgerRow{ref: ref, limit: limit}
		var err error
		if creating {
			if limit <= 0 {
				// A non-positive limit cannot be stored (the CHECK forbids it)
				// and would mean "hold everything forever", which no policy
				// meant to express. Validation rejects it at activation, so
				// reaching here is a bug; failing closed is the only safe read.
				return nil, fmt.Errorf("sendingpolicy: scope %s has a non-positive limit %d", ref.Scope, limit)
			}
			// The DO UPDATE branch both takes the row lock and performs the
			// limit synchronization. Synchronizing before any check is what
			// makes a limit change effective on its first armed day in both
			// directions: a reduction blocks immediately even when today's
			// usage already exceeds the new value, and an increase releases
			// exactly the new headroom rather than retroactively forgiving
			// anything already spent.
			err = tx.QueryRow(ctx, `
				INSERT INTO sending_budget_counters
				    (scope, scope_id, day, reserved_count, confirmed_count, daily_limit)
				VALUES ($1, $2, $3, 0, 0, $4)
				ON CONFLICT (scope, scope_id, day) DO UPDATE
				    SET daily_limit = EXCLUDED.daily_limit
				RETURNING reserved_count, confirmed_count, daily_limit`,
				ref.Scope, ref.ScopeID, ref.Day, limit,
			).Scan(&row.reserved, &row.confirmed, &row.limit)
			if err != nil {
				return nil, fmt.Errorf("sendingpolicy: lock counter %s/%s: %w", ref.Scope, ref.ScopeID, err)
			}
			row.exists = true
		} else {
			err = tx.QueryRow(ctx, `
				SELECT reserved_count, confirmed_count, daily_limit
				  FROM sending_budget_counters
				 WHERE scope = $1 AND scope_id = $2 AND day = $3
				   FOR UPDATE`,
				ref.Scope, ref.ScopeID, ref.Day,
			).Scan(&row.reserved, &row.confirmed, &row.limit)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				row.exists = false
			case err != nil:
				return nil, fmt.Errorf("sendingpolicy: lock counter %s/%s: %w", ref.Scope, ref.ScopeID, err)
			default:
				row.exists = true
			}
		}
		plan.rows[ref] = row
		plan.order = append(plan.order, ref)
	}
	return plan, nil
}

// sortLedgerRefs orders physical counter rows: scope rank first (the normative
// order), then scope ID, then day. Including the day makes the order total when
// a transaction spans a midnight rollover and must touch yesterday's and
// today's row for the same scope.
func sortLedgerRefs(refs []ledgerRef) {
	sort.Slice(refs, func(i, j int) bool {
		ri, rj := scopeLockRank[refs[i].Scope], scopeLockRank[refs[j].Scope]
		if ri != rj {
			return ri < rj
		}
		if refs[i].ScopeID != refs[j].ScopeID {
			return refs[i].ScopeID < refs[j].ScopeID
		}
		return refs[i].Day.Before(refs[j].Day)
	})
}

// release records giving back `units` of reserved-but-unconfirmed capacity.
//
// Silently skipping an absent row is correct: a counter that no longer exists
// holds no units of ours. Clamping at confirmed_count is belt-and-braces
// against the one mistake that would corrupt the ledger — releasing units that
// were already confirmed. The table's CHECK would catch it, but a constraint
// violation aborts the whole transaction, and releases run on paths (rate
// deferral, suppression cancel) where aborting strands a message.
func (p *ledgerPlan) release(ref ledgerRef, units int) {
	row, ok := p.rows[ref]
	if !ok || !row.exists {
		return
	}
	if row.reserved+row.delta-units < row.confirmed {
		return
	}
	row.delta -= units
}

// acquire records taking `units`, reporting whether there was room.
func (p *ledgerPlan) acquire(ref ledgerRef, units int) bool {
	row, ok := p.rows[ref]
	if !ok {
		return false
	}
	if row.reserved+row.delta+units > row.limit {
		return false
	}
	row.delta += units
	row.acquired += units
	return true
}

// overrun takes `units` past the limit. Only shadow mode uses it.
//
// Clamping the counter at the cap during a shadow window would make the window
// prove only that the cap exists. What the activation gate actually has to
// approve is whether the proposed number covers real aggregate demand plus
// headroom, and that is measurable only if the counter is allowed to record
// demand it would have refused.
func (p *ledgerPlan) overrun(ref ledgerRef, units int) {
	row, ok := p.rows[ref]
	if !ok {
		return
	}
	row.delta += units
	row.acquired += units
}

// discardAcquisitions drops every take while keeping every give-back, so an
// enforced denial leaves the attempt released rather than half-charged.
func (p *ledgerPlan) discardAcquisitions() {
	for _, row := range p.rows {
		row.delta -= row.acquired
		row.acquired = 0
	}
}

// flush writes every accumulated delta, in the same order the rows were
// locked. A zero delta writes nothing — the common steady-state case where an
// attempt releases and immediately re-acquires the same units on the same row
// costs one lock and no write.
func (p *ledgerPlan) flush(ctx context.Context, tx pgx.Tx) error {
	for _, ref := range p.order {
		row := p.rows[ref]
		if row.delta == 0 || !row.exists {
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE sending_budget_counters
			   SET reserved_count = reserved_count + $4
			 WHERE scope = $1 AND scope_id = $2 AND day = $3`,
			ref.Scope, ref.ScopeID, ref.Day, row.delta,
		); err != nil {
			return fmt.Errorf("sendingpolicy: write counter %s/%s: %w", ref.Scope, ref.ScopeID, err)
		}
	}
	return nil
}

// confirm moves `units` from reserved to confirmed on every named row.
//
// Confirmed means the capacity is irrevocably spent for a possible provider
// attempt — not that SES accepted anything. That distinction is why a crash
// between authorization and the socket costs the account a recipient:
// under-admitting is recoverable at the next midnight, while refunding an
// attempt that might already have reached SES would let a delete-and-resend
// loop mint free reputation exposure.
func confirmCounters(ctx context.Context, tx pgx.Tx, refs []ledgerRef, units int) error {
	sorted := append([]ledgerRef(nil), refs...)
	sortLedgerRefs(sorted)
	for _, ref := range sorted {
		tag, err := tx.Exec(ctx, `
			UPDATE sending_budget_counters
			   SET confirmed_count = confirmed_count + $4
			 WHERE scope = $1 AND scope_id = $2 AND day = $3`,
			ref.Scope, ref.ScopeID, ref.Day, units,
		)
		if err != nil {
			return fmt.Errorf("sendingpolicy: confirm counter %s/%s: %w", ref.Scope, ref.ScopeID, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("sendingpolicy: confirm counter %s/%s: row is missing", ref.Scope, ref.ScopeID)
		}
	}
	return nil
}

// refsFor pairs a scope key set with a day.
func refsFor(keys []counterKey, day time.Time) []ledgerRef {
	refs := make([]ledgerRef, len(keys))
	for i, key := range keys {
		refs[i] = ledgerRef{counterKey: key, Day: day}
	}
	return refs
}
