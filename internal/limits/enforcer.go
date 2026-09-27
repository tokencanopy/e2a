package limits

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/usage"
)

// Counter is the subset of *usage.Store the enforcer needs. Declared
// here as an interface so tests can supply a fake without standing up
// the full usage store.
type Counter interface {
	CountAgentsByUser(ctx context.Context, userID string) (int, error)
	CountDomainsByUser(ctx context.Context, userID string) (int, error)
	MessagesThisMonth(ctx context.Context, userID string) (int, error)
	// MessagesToday returns the current UTC day's outbound
	// recipient-delivery count, for the optional per-day cap.
	MessagesToday(ctx context.Context, userID string) (int, error)
	GetStorageBytes(ctx context.Context, userID string) (int64, error)
}

// limitsReader is the subset of *Store the enforcer needs. Declared as
// an interface so tests can supply a fake without a real Postgres pool.
type limitsReader interface {
	Get(ctx context.Context, userID string) (Limits, bool, error)
}

// counterTxReader is the optional transaction-scoped counter surface used by
// ReserveMessageSendTx. Declared separately from Counter (and asserted, not
// required) so the existing test fakes stay unchanged.
type counterTxReader interface {
	MessagesThisMonthTx(ctx context.Context, tx pgx.Tx, userID string) (int, error)
	MessagesTodayTx(ctx context.Context, tx pgx.Tx, userID string) (int, error)
}

// limitsTxReader is the optional transaction-scoped limits surface used by
// ReserveMessageSendTx; same rationale as counterTxReader.
type limitsTxReader interface {
	GetTx(ctx context.Context, tx pgx.Tx, userID string) (Limits, bool, error)
}

// DBEnforcer is the production Enforcer: reads account_limits + falls
// back to operator Defaults, counts current resources via the usage
// store, and caches the resolved Limits in-process for cacheTTL to keep
// hot paths off the DB on every send.
//
// The cache only stores the *Limits* (caps), not the *counts*. Counts
// must always reflect the live database — caching them would mean a
// just-created agent or just-sent message wouldn't show up until TTL
// expiry, which would either let users exceed caps or let the dashboard
// lie about current usage. Reading the count is one bounded query per
// check; the win from caching limits is avoiding the join into
// account_limits, which is the costlier read.
type DBEnforcer struct {
	store    limitsReader
	counter  Counter
	defaults Defaults
	cacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]cachedLimits

	// gen is the process-wide invalidation epoch. Get samples it (via
	// cacheGet, under mu) strictly BEFORE its DB read and hands it back
	// to cachePut, which stores the fill only if the epoch has not
	// advanced in between. Without that check this interleaving silently
	// defeats an explicit invalidation:
	//
	//	T1 Get        -> cache miss, reads the OLD account_limits row
	//	T2 sidecar    -> commits NEW limits, POSTs /api/internal/limits/invalidate
	//	T2 Invalidate -> evicts the entry
	//	T1 cachePut   -> stores the PRE-write limits with a FRESH TTL
	//
	// A user who had just upgraded would then keep hitting 402
	// limit_exceeded for up to cacheTTL (60s in prod) even though
	// billing did everything right — defeating the one mechanism billing
	// has to make an upgrade take effect immediately. A counter rather
	// than a timestamp because two events inside one clock tick must
	// still compare as different.
	//
	// The epoch is process-wide, not per-user — see Invalidate for why
	// (memory bound) and for the tradeoff that buys.
	gen uint64
}

// cachedLimits is one user's cache slot: the resolved Limits and their
// expiry. Invalidation state does not live here — the invalidation
// epoch is the process-wide DBEnforcer.gen, which is what lets
// Invalidate delete slots outright instead of retaining per-user
// tombstones.
type cachedLimits struct {
	limits  Limits
	expires time.Time
}

// NewEnforcer constructs the production enforcer. cacheTTL of 0 disables
// the cache (every Get hits the DB) — useful for tests that mutate
// account_limits and want immediate visibility.
func NewEnforcer(store *Store, counter Counter, defaults Defaults, cacheTTL time.Duration) *DBEnforcer {
	return &DBEnforcer{
		store:    store,
		counter:  counter,
		defaults: defaults,
		cacheTTL: cacheTTL,
		cache:    make(map[string]cachedLimits),
	}
}

// newEnforcerWithReader is the test seam: lets unit tests inject a fake
// limitsReader so they don't need a Postgres pool.
func newEnforcerWithReader(reader limitsReader, counter Counter, defaults Defaults, cacheTTL time.Duration) *DBEnforcer {
	return &DBEnforcer{
		store:    reader,
		counter:  counter,
		defaults: defaults,
		cacheTTL: cacheTTL,
		cache:    make(map[string]cachedLimits),
	}
}

func (e *DBEnforcer) Get(ctx context.Context, userID string) (Limits, error) {
	cached, gen, ok := e.cacheGet(userID)
	if ok {
		return cached, nil
	}
	// gen was sampled together with the miss, i.e. strictly BEFORE the
	// DB read below. cachePut re-checks it so a fill that raced an
	// Invalidate is dropped instead of re-caching pre-write limits.
	row, found, err := e.store.Get(ctx, userID)
	if err != nil {
		return Limits{}, err
	}
	resolved := e.resolveLimits(row, found)
	e.cachePut(userID, resolved, gen)
	return resolved, nil
}

// resolveLimits applies the operator Defaults when the user has no
// account_limits row.
func (e *DBEnforcer) resolveLimits(row Limits, found bool) Limits {
	if found {
		return row
	}
	return Limits{
		PlanCode:              e.defaults.PlanCode,
		MaxAgents:             e.defaults.MaxAgents,
		MaxDomains:            e.defaults.MaxDomains,
		MaxMessagesMonth:      e.defaults.MaxMessagesMonth,
		MaxMessagesDay:        e.defaults.MaxMessagesDay,
		MaxStorageBytes:       e.defaults.MaxStorageBytes,
		OutboundFooterEnabled: e.defaults.OutboundFooterEnabled,
	}
}

// Invalidate evicts the user's cached Limits and advances the
// process-wide invalidation epoch, so any fill already in flight (one
// that read account_limits before the caller's write committed) is
// discarded by cachePut rather than stored with a fresh TTL.
//
// The epoch is process-wide rather than per-user so that Invalidate can
// DELETE the slot instead of retaining a per-user generation tombstone.
// A per-user generation must survive the invalidate (an uncached user
// is precisely the racing case: miss → DB read → invalidate → cachePut),
// which means every distinct userID ever passed here keeps a map entry
// for the process lifetime — an unbounded-growth vector, since this is
// reachable via the HMAC-authenticated /api/internal/limits/invalidate
// endpoint and the map has no TTL sweep. The global epoch preserves the
// race guard — it drops a strict superset of the fills a per-user
// counter would drop — while making deletion the only map effect, so
// the map is again bounded by users actually cached by real fills.
//
// Tradeoff: an Invalidate for user A that lands inside user B's fill
// window also drops B's fill (a false drop: B's result is still
// returned to its caller, just not cached, costing one extra DB read on
// B's next request). Prod invalidates are Stripe-event-driven — a
// handful per day — against millisecond fill windows, so the collision
// rate is noise, and correctness is unaffected either way because a
// dropped fill is simply re-read.
func (e *DBEnforcer) Invalidate(userID string) {
	if e.cacheTTL <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gen++
	delete(e.cache, userID) // delete is safe even on a nil map
}

// ensureCacheLocked lazily allocates the cache map, because writing to a
// nil map panics. Only cachePut writes the map now (Invalidate is back
// to delete, which is nil-safe), and the constructors always allocate,
// so this only covers a zero-value DBEnforcer built inside the package
// (the fields are unexported, so no external package can construct one
// that way). Caller must hold e.mu.
func (e *DBEnforcer) ensureCacheLocked() {
	if e.cache == nil {
		e.cache = make(map[string]cachedLimits)
	}
}

func (e *DBEnforcer) CheckAgentCreate(ctx context.Context, userID string) error {
	lim, err := e.Get(ctx, userID)
	if err != nil {
		return err
	}
	count, err := e.counter.CountAgentsByUser(ctx, userID)
	if err != nil {
		return err
	}
	if count >= lim.MaxAgents {
		return &LimitExceededError{
			Resource: "agents",
			Limit:    lim.MaxAgents,
			Current:  count,
			Limits:   lim,
		}
	}
	return nil
}

func (e *DBEnforcer) CheckDomainCreate(ctx context.Context, userID string) error {
	lim, err := e.Get(ctx, userID)
	if err != nil {
		return err
	}
	count, err := e.counter.CountDomainsByUser(ctx, userID)
	if err != nil {
		return err
	}
	if count >= lim.MaxDomains {
		return &LimitExceededError{
			Resource: "domains",
			Limit:    lim.MaxDomains,
			Current:  count,
			Limits:   lim,
		}
	}
	return nil
}

// CheckMessageSend enforces the month-flow cap and the storage stock
// cap for an outbound send of `units` recipient-deliveries. Either
// being exceeded blocks the operation. The flow cap is checked first
// because it's the cheaper read; if both would fail, the user sees
// "messages_month" as the reason, which is the easier one to explain
// ("you sent N this month"). The flow comparison is
// `used + units > max` — the whole message is rejected when it would
// cross the cap (no partial sends), and for units == 1 this reduces to
// the historical `used >= max`.
func (e *DBEnforcer) CheckMessageSend(ctx context.Context, userID string, units int) error {
	if units < 1 {
		units = 1
	}
	lim, err := e.Get(ctx, userID)
	if err != nil {
		return err
	}
	msgCount, err := e.counter.MessagesThisMonth(ctx, userID)
	if err != nil {
		return err
	}
	if msgCount+units > lim.MaxMessagesMonth {
		return &LimitExceededError{
			Resource: "messages_month",
			Limit:    lim.MaxMessagesMonth,
			Current:  msgCount,
			Limits:   lim,
		}
	}
	// Optional per-day cap (usage-based pricing v1). nil = no daily policy —
	// the self-host default and every paid shape — so the extra count read
	// only happens for accounts that actually carry the cap. Resets at UTC
	// midnight with the usage_summaries bucket_date.
	if lim.MaxMessagesDay != nil {
		dayCount, err := e.counter.MessagesToday(ctx, userID)
		if err != nil {
			return err
		}
		if dayCount+units > *lim.MaxMessagesDay {
			return &LimitExceededError{
				Resource: "messages_day",
				Limit:    *lim.MaxMessagesDay,
				Current:  dayCount,
				Limits:   lim,
			}
		}
	}
	storage, err := e.counter.GetStorageBytes(ctx, userID)
	if err != nil {
		return err
	}
	if storage >= lim.MaxStorageBytes {
		// Storage values can exceed int32 (MaxStorageBytes is BIGINT in
		// the DB and may be 100 GiB+ on Scale). The LimitExceededError
		// fields are int — that's 32-bit on 32-bit platforms. Clamp via
		// safeInt64ToInt so we don't roll over on unusual targets; the
		// real value is also surfaced verbatim in the Limits field for
		// callers that need bytes-accurate access.
		return &LimitExceededError{
			Resource: "storage_bytes",
			Limit:    safeInt64ToInt(lim.MaxStorageBytes),
			Current:  safeInt64ToInt(storage),
			Limits:   lim,
		}
	}
	return nil
}

// ReserveMessageSendTx is the accept-time, race-proof form of the message-flow
// check: the caller runs it inside the accept transaction, before inserting
// the message, so the counter it reads includes every accepted-but-unmetered
// send already committed. It takes a per-user advisory lock (keyspace 3,
// distinct from claimOrCreateDomain's keyspaces 0/1 and
// CreateAgentWithLimit's keyspace 2) so two simultaneous immediate sends for
// one account cannot both read the same pre-insert total — the same shape as
// the max_agents (#942) and max_domains (#901) fixes.
//
// The month and day caps are read on tx, so the lock never has to wait on a
// second pool connection; the storage stock cap is not re-checked here because
// it is not a flow budget and the accept-time pre-check already enforces it.
func (e *DBEnforcer) ReserveMessageSendTx(ctx context.Context, tx pgx.Tx, userID string, units int) error {
	if units < 1 {
		units = 1
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 3))`, userID); err != nil {
		return err
	}
	lim, err := e.limitsForTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	msgCount, err := e.messagesThisMonthForTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if msgCount+units > lim.MaxMessagesMonth {
		return &LimitExceededError{
			Resource: "messages_month",
			Limit:    lim.MaxMessagesMonth,
			Current:  msgCount,
			Limits:   lim,
		}
	}
	if lim.MaxMessagesDay != nil {
		dayCount, err := e.messagesTodayForTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if dayCount+units > *lim.MaxMessagesDay {
			return &LimitExceededError{
				Resource: "messages_day",
				Limit:    *lim.MaxMessagesDay,
				Current:  dayCount,
				Limits:   lim,
			}
		}
	}
	return nil
}

// limitsForTx resolves the caps without a pool read while the reservation
// holds its advisory lock: the cache first (the accept pre-check has normally
// just filled it), then the transaction, and the pooled getter only for an
// enforcer whose store has no tx form.
func (e *DBEnforcer) limitsForTx(ctx context.Context, tx pgx.Tx, userID string) (Limits, error) {
	if cached, _, ok := e.cacheGet(userID); ok {
		return cached, nil
	}
	if r, ok := e.store.(limitsTxReader); ok {
		row, found, err := r.GetTx(ctx, tx, userID)
		if err != nil {
			return Limits{}, err
		}
		return e.resolveLimits(row, found), nil
	}
	return e.Get(ctx, userID)
}

func (e *DBEnforcer) messagesThisMonthForTx(ctx context.Context, tx pgx.Tx, userID string) (int, error) {
	if c, ok := e.counter.(counterTxReader); ok {
		return c.MessagesThisMonthTx(ctx, tx, userID)
	}
	return e.counter.MessagesThisMonth(ctx, userID)
}

func (e *DBEnforcer) messagesTodayForTx(ctx context.Context, tx pgx.Tx, userID string) (int, error) {
	if c, ok := e.counter.(counterTxReader); ok {
		return c.MessagesTodayTx(ctx, tx, userID)
	}
	return e.counter.MessagesToday(ctx, userID)
}

// CheckInboundMessage enforces ONLY the storage stock cap for an inbound
// delivery. Message-flow caps are outbound-only (usage-based pricing v1):
// a recipient's exhausted send allowance must never bounce a stranger's
// inbound mail. Storage remains checked because an inbound message
// physically consumes the account's storage budget.
func (e *DBEnforcer) CheckInboundMessage(ctx context.Context, userID string) error {
	lim, err := e.Get(ctx, userID)
	if err != nil {
		return err
	}
	storage, err := e.counter.GetStorageBytes(ctx, userID)
	if err != nil {
		return err
	}
	if storage >= lim.MaxStorageBytes {
		return &LimitExceededError{
			Resource: "storage_bytes",
			Limit:    safeInt64ToInt(lim.MaxStorageBytes),
			Current:  safeInt64ToInt(storage),
			Limits:   lim,
		}
	}
	return nil
}

// safeInt64ToInt clamps an int64 to int.Max{,Min} so a downstream JSON
// encode never silently produces a negative number from int overflow
// on 32-bit Go targets. Production runs 64-bit; this is purely
// defensive against a future 32-bit build (or an ARM32 host).
func safeInt64ToInt(v int64) int {
	const maxInt = int64(^uint(0) >> 1)
	if v > maxInt {
		return int(maxInt)
	}
	return int(v)
}

// cacheGet returns the cached limits when they are present and unexpired.
// It always returns the current process-wide invalidation epoch alongside
// the result — sampled under the same lock acquisition as the lookup —
// so a caller that observes a miss can pass that epoch to cachePut and
// have the store rejected if any Invalidate landed in between.
func (e *DBEnforcer) cacheGet(userID string) (Limits, uint64, bool) {
	if e.cacheTTL <= 0 {
		return Limits{}, 0, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.cache[userID]
	if !ok || time.Now().After(c.expires) {
		return Limits{}, e.gen, false
	}
	return c.limits, e.gen, true
}

// cachePut stores a fill only if the invalidation epoch is still the one
// the caller sampled before its DB read. A stale fill is dropped
// outright rather than stored with a shorter TTL: the next Get simply
// re-reads account_limits, which is the correct post-write value and
// costs one query. The epoch being process-wide means an unrelated
// user's invalidate can also drop this fill — intentionally conservative;
// see Invalidate for the tradeoff.
func (e *DBEnforcer) cachePut(userID string, l Limits, gen uint64) {
	if e.cacheTTL <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gen != gen {
		return
	}
	e.ensureCacheLocked()
	e.cache[userID] = cachedLimits{limits: l, expires: time.Now().Add(e.cacheTTL)}
}

// Compile-time check: DBEnforcer satisfies the Enforcer interface and
// the Counter dependency is satisfied by *usage.Store.
var (
	_ Enforcer = (*DBEnforcer)(nil)
	_ Counter  = (*usage.Store)(nil)
)
